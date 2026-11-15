// Package pick chooses a runner. It is pure: all outside state comes in
// through the State interface, so it is easy to test.
package pick

import (
	"fmt"
	"strings"

	"github.com/isromero/switchyard/internal/config"
	"github.com/isromero/switchyard/internal/gh"
)

// State is what the picker needs to know about the world. Both methods
// are called lazily (only if a provider needs them), so implementations
// should memoize.
type State interface {
	// Usage returns weighted minutes used this month per provider.
	Usage() (map[string]float64, error)
	// Runners returns the self-hosted runners visible to this repo.
	Runners() ([]gh.Runner, error)
	// Queued returns the labels of jobs that are waiting for a runner
	// right now. A self-hosted runner is only picked if there are more
	// idle runners with its label than queued jobs asking for it.
	Queued() ([][]string, error)
	// Stuck returns hosted providers that look stuck (they left a job
	// waiting without a runner), with the reason. They are skipped.
	Stuck() map[string]string
}

// Request is what the caller asks for.
type Request struct {
	Size          string          // small | medium | large
	Arch          string          // x64 | arm64 | "" (any)
	RunID         string          // $GITHUB_RUN_ID, for {{run_id}} labels
	CurrentRunner string          // $RUNNER_NAME of the pick job itself; counts as idle
	Disabled      map[string]bool // providers disabled for this call
	// PublicRepo: the calling repository is public. Providers with
	// ignore_public_repos (GitHub-hosted) are then free without limit.
	PublicRepo bool
	// ForkPR: the run was started by a pull request from a fork, so it runs
	// code from outside the repository. Self-hosted runners are never used
	// for it: that code would run on your machine.
	ForkPR bool
	// Claimed counts, per lowercased label, self-hosted runners already
	// given to earlier jobs of the same pick (see Assign).
	Claimed map[string]int
}

// Decision is the outcome.
type Decision struct {
	Label    string
	Provider string // empty for the fallback
	Paid     bool
	Fallback bool
	// Stop is set when on_exhausted is "fail", every provider is known to
	// be used up and the fallback is not known to be free, or when the
	// fallback is a self-hosted label and the run comes from a fork. The
	// caller should fail instead of using Label. StopReason says which.
	Stop       bool
	StopReason string
	// Notes explain why providers were skipped, for logs.
	Notes []string
}

// Choose picks a runner label. It never fails: when nothing fits, it
// returns the config fallback.
func Choose(cfg *config.Config, req Request, st State) Decision {
	var d Decision
	note := func(format string, a ...any) { d.Notes = append(d.Notes, fmt.Sprintf(format, a...)) }
	// unknown is set when some provider was skipped because of an error
	// rather than a known limit; on_exhausted: fail then does not apply.
	unknown := false

	// Pass 1: self-hosted runners that are idle, and free minutes.
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		r, ok := usable(p, req, note)
		if !ok {
			continue
		}
		if p.Kind == config.KindSelfHosted {
			if req.ForkPR {
				note("%s: not used for pull requests from forks", p.Name)
				continue
			}
			idle, why, err := hasIdleRunner(st, r.Label, req.CurrentRunner, claimedFor(req, r.Label))
			if idle {
				return d.choose(p, r, req, false)
			}
			if err {
				unknown = true
			}
			note("%s: %s", p.Name, why)
			continue
		}
		if why, ok := st.Stuck()[p.Name]; ok {
			unknown = true
			note("%s: %s", p.Name, why)
			continue
		}
		if p.IgnorePublicRepos && req.PublicRepo {
			return d.choose(p, r, req, false) // free for public repositories
		}
		if p.FreeMinutes <= 0 {
			continue // only considered in the paid pass
		}
		used, err := usedMinutes(st, p.Name)
		if err != nil {
			unknown = true
			note("%s: usage unknown (%v)", p.Name, err)
			continue
		}
		if p.CapMinutes > 0 && used+p.Margin(cfg) >= p.CapMinutes {
			note("%s: at minute cap (%.0f of %.0f)", p.Name, used, p.CapMinutes)
			continue
		}
		left := p.FreeMinutes - used
		if left > p.Margin(cfg) {
			return d.choose(p, r, req, false)
		}
		note("%s: free minutes used (%.0f of %.0f, margin %.0f)", p.Name, used, p.FreeMinutes, p.Margin(cfg))
	}

	// Pass 2: providers allowed to go paid, within their caps.
	for _, p := range paidOrder(cfg) {
		r, ok := usable(p, req, nil)
		if !ok {
			continue
		}
		if _, ok := st.Stuck()[p.Name]; ok {
			continue // noted in pass 1
		}
		used, err := usedMinutes(st, p.Name)
		if err != nil {
			unknown = true
			note("%s: usage unknown (%v)", p.Name, err)
			continue
		}
		margin := p.Margin(cfg)
		if p.CapMinutes > 0 && used+margin >= p.CapMinutes {
			note("%s: at minute cap (%.0f of %.0f)", p.Name, used, p.CapMinutes)
			continue
		}
		if p.CapUSD > 0 {
			spent := Cost(p, used)
			if spent+margin*p.PricePerMinute >= p.CapUSD {
				note("%s: at spend cap ($%.2f of $%.2f)", p.Name, spent, p.CapUSD)
				continue
			}
		}
		// Paid once the free allowance is actually used up; inside the
		// margin the next job may still be (partly) free.
		return d.choose(p, r, req, used >= p.FreeMinutes)
	}

	d.Label = cfg.Fallback
	d.Fallback = true
	cost := fallbackCost(cfg, req, st)
	d.Paid = cost == costPaid
	if cost == costUnsafe {
		d.Stop = true
		d.StopReason = "the fallback is a self-hosted runner and this run comes from a fork"
		return d
	}
	stoppable := cost == costPaid || cost == costUnmatched
	if cfg.OnExhausted == config.OnExhaustedFail && !unknown && stoppable {
		d.Stop = true
		d.StopReason = "every provider is used up and the fallback would be billed (on_exhausted: fail)"
	}
	return d
}

type fallbackCostKind int

const (
	costFree      fallbackCostKind = iota // a configured provider that is free right now
	costPaid                              // a configured provider whose free minutes are used up
	costUnmatched                         // the label belongs to no configured provider
	costUnknown                           // its usage could not be read
	costUnsafe                            // self-hosted, but the run comes from a fork
)

// fallbackCost classifies the fallback label by what using it costs.
func fallbackCost(cfg *config.Config, req Request, st State) fallbackCostKind {
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if _, ok := p.Match([]string{cfg.Fallback}); !ok {
			continue
		}
		if p.Kind == config.KindSelfHosted {
			if req.ForkPR {
				return costUnsafe
			}
			return costFree // may have to wait for a runner, but costs nothing
		}
		if p.IgnorePublicRepos && req.PublicRepo {
			return costFree
		}
		used, err := usedMinutes(st, p.Name)
		if err != nil {
			return costUnknown
		}
		if used < p.FreeMinutes {
			return costFree
		}
		return costPaid
	}
	return costUnmatched
}

func (d Decision) choose(p *config.Provider, r config.Runner, req Request, paid bool) Decision {
	d.Label = r.Render(req.RunID)
	d.Provider = p.Name
	d.Paid = paid
	return d
}

// Cost is the estimated spend in USD for weighted minutes used.
func Cost(p *config.Provider, used float64) float64 {
	over := used - p.FreeMinutes
	if over <= 0 {
		return 0
	}
	return over * p.PricePerMinute
}

// usable returns the provider's runner for the request, if the provider
// is enabled and has one.
func usable(p *config.Provider, req Request, note func(string, ...any)) (config.Runner, bool) {
	if !p.IsEnabled() || req.Disabled[p.Name] {
		if note != nil {
			note("%s: disabled", p.Name)
		}
		return config.Runner{}, false
	}
	for _, r := range p.Runners {
		if r.Size == req.Size && (req.Arch == "" || r.Arch == req.Arch) {
			return r, true
		}
	}
	return config.Runner{}, false
}

func paidOrder(cfg *config.Config) []*config.Provider {
	var out []*config.Provider
	add := func(p *config.Provider) {
		if p != nil && p.Kind == config.KindHosted && p.AllowPaid {
			out = append(out, p)
		}
	}
	if len(cfg.PaidOrder) > 0 {
		for _, n := range cfg.PaidOrder {
			add(cfg.Provider(n))
		}
		return out
	}
	for i := range cfg.Providers {
		add(&cfg.Providers[i])
	}
	return out
}

func usedMinutes(st State, name string) (float64, error) {
	u, err := st.Usage()
	if err != nil {
		return 0, err
	}
	return u[name], nil
}

// hasIdleRunner reports whether a runner with the label is free for one
// more job: there must be more idle runners than jobs already queued for
// the label. failed is true when the answer comes from an API error.
// claimed is how many of those runners this pick already gave to other jobs.
func hasIdleRunner(st State, label, current string, claimed int) (ok bool, why string, failed bool) {
	runners, err := st.Runners()
	if err != nil {
		return false, fmt.Sprintf("cannot list runners (%v)", err), true
	}
	found, idle := false, 0
	for _, r := range runners {
		if !r.HasLabel(label) {
			continue
		}
		found = true
		if r.Status != "online" {
			continue
		}
		// The pick job itself occupies its runner, but only for a few
		// seconds, so that runner counts as idle.
		if !r.Busy || (current != "" && strings.EqualFold(r.Name, current)) {
			idle++
		}
	}
	if !found {
		return false, fmt.Sprintf("no runner with label %q", label), false
	}
	if idle == 0 {
		return false, fmt.Sprintf("no idle online runner with label %q", label), false
	}
	queued := 0
	if jobs, err := st.Queued(); err == nil {
		for _, labels := range jobs {
			for _, l := range labels {
				if strings.EqualFold(l, label) {
					queued++
					break
				}
			}
		}
	} // on error, trust the runner list alone
	if idle <= queued+claimed {
		if claimed > 0 {
			return false, fmt.Sprintf("%d idle runner(s) with label %q, %d job(s) queued and %d given to earlier jobs of this run", idle, label, queued, claimed), false
		}
		return false, fmt.Sprintf("%d idle runner(s) with label %q but %d job(s) already queued for it", idle, label, queued), false
	}
	return true, "", false
}
