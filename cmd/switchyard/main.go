// Command switchyard picks a GitHub Actions runner per run, spending
// free minutes first. See README.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/isromero/switchyard/internal/config"
	"github.com/isromero/switchyard/internal/gh"
	"github.com/isromero/switchyard/internal/pick"
	"github.com/isromero/switchyard/internal/usage"
)

var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usageText(stderr)
		return 2
	}
	log := &logger{w: stderr, actions: os.Getenv("GITHUB_ACTIONS") == "true"}
	switch args[0] {
	case "pick":
		return cmdPick(args[1:], stdout, log)
	case "usage":
		return cmdUsage(args[1:], stdout, log)
	case "check":
		return cmdCheck(args[1:], stdout, log)
	case "version", "--version":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "-h", "--help":
		usageText(stdout)
		return 0
	}
	fmt.Fprintf(stderr, "unknown command %q\n", args[0])
	usageText(stderr)
	return 2
}

func usageText(w io.Writer) {
	fmt.Fprint(w, `usage: switchyard <command> [flags]

commands:
  pick     print the runs-on label to use (never fails; falls back)
  usage    show minutes used this month per provider
  check    validate the config file
  version  print the version

Run "switchyard <command> -h" for flags.
`)
}

// ---- logging ----

type logger struct {
	w       io.Writer
	actions bool
}

func (l *logger) Infof(format string, a ...any) { fmt.Fprintf(l.w, format+"\n", a...) }

func (l *logger) Errorf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if l.actions {
		fmt.Fprintf(l.w, "::error title=switchyard::%s\n", strings.ReplaceAll(msg, "\n", "; "))
		return
	}
	fmt.Fprintf(l.w, "error: %s\n", msg)
}

func (l *logger) Warnf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if l.actions {
		// Workflow commands are one line.
		fmt.Fprintf(l.w, "::warning title=switchyard::%s\n", strings.ReplaceAll(msg, "\n", "; "))
		return
	}
	fmt.Fprintf(l.w, "warning: %s\n", msg)
}

// ---- shared setup ----

func configPath(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv("SWITCHYARD_CONFIG"); v != "" {
		return v
	}
	for _, p := range []string{"switchyard.yml", ".github/switchyard.yml"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "switchyard.yml"
}

func token() string {
	for _, k := range []string{"SWITCHYARD_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// runnersToken is used only to list self-hosted runners, which needs
// admin rights. It defaults to the main token.
func runnersToken() string {
	return firstNonEmpty(os.Getenv("SWITCHYARD_RUNNERS_TOKEN"), token())
}

func newClient() *gh.Client {
	return gh.New(os.Getenv("GITHUB_API_URL"), token())
}

func newRunnersClient() *gh.Client {
	return gh.New(os.Getenv("GITHUB_API_URL"), runnersToken())
}

// exitStop is the exit code of pick when on_exhausted: fail applies.
const exitStop = 3

// runEvent is what pick needs from the workflow event payload.
type runEvent struct {
	Public bool // the calling repository is public
	ForkPR bool // started by a pull request whose head is in another repository
}

// readEvent reads the event payload. Unknown visibility means private (the
// safe answer for counting). A pull request whose head repository is
// missing (a deleted fork) counts as a fork.
func readEvent() runEvent {
	b, err := os.ReadFile(os.Getenv("GITHUB_EVENT_PATH"))
	if err != nil {
		return runEvent{}
	}
	type repo struct {
		FullName   string `json:"full_name"`
		Private    *bool  `json:"private"`
		Visibility string `json:"visibility"`
	}
	var ev struct {
		Repository  *repo `json:"repository"`
		PullRequest *struct {
			Head struct {
				Repo *repo `json:"repo"`
			} `json:"head"`
		} `json:"pull_request"`
	}
	if json.Unmarshal(b, &ev) != nil {
		return runEvent{}
	}
	var out runEvent
	if r := ev.Repository; r != nil {
		if r.Visibility != "" {
			out.Public = r.Visibility == "public"
		} else {
			out.Public = r.Private != nil && !*r.Private
		}
	}
	if pr := ev.PullRequest; pr != nil {
		head := pr.Head.Repo
		out.ForkPR = head == nil || ev.Repository == nil || !strings.EqualFold(head.FullName, ev.Repository.FullName)
	}
	return out
}

// liveState implements pick.State against the GitHub API, memoized.
type liveState struct {
	cfg     *config.Config
	client  *gh.Client
	runCl   *gh.Client // for the runner list (may use another token)
	runID   string     // $GITHUB_RUN_ID, left out of the queue count
	log     *logger
	repo    string
	refresh bool          // force a usage refresh
	timeout time.Duration // overrides usage.timeout when > 0
	now     time.Time

	usageOnce sync.Once
	usage     map[string]usage.Total
	usageErr  error
	cache     *usage.Cache // what usage was read from

	runnersOnce sync.Once
	runners     []gh.Runner
	runnersErr  error

	queuedOnce sync.Once
	queued     [][]string
	queuedErr  error
}

func (s *liveState) Totals() (map[string]usage.Total, error) {
	s.usageOnce.Do(func() { s.usage, s.usageErr = s.loadUsage() })
	return s.usage, s.usageErr
}

func (s *liveState) Usage() (map[string]float64, error) {
	t, err := s.Totals()
	if err != nil {
		return nil, err
	}
	return usage.Weighted(t), nil
}

// Stuck reports hosted providers that look stuck, from the usage scan.
// Without usage data nothing is reported.
func (s *liveState) Stuck() map[string]string {
	if _, err := s.Totals(); err != nil {
		return nil
	}
	return usage.Stuck(s.cfg, s.cache, s.now, s.cfg.StuckWait())
}

func (s *liveState) loadUsage() (map[string]usage.Total, error) {
	scope := usage.Scope(s.cfg)
	file := firstNonEmpty(os.Getenv("SWITCHYARD_CACHE_FILE"), s.cfg.Usage.CacheFile, usage.DefaultCacheFile(scope))
	c := usage.LoadCache(file, scope, s.now)
	s.cache = c
	if !s.refresh && c.Fresh(s.now, time.Duration(s.cfg.Usage.CacheTTL)) {
		return usage.Totals(s.cfg, c), nil
	}
	if token() == "" {
		return nil, errors.New("no token (set SWITCHYARD_TOKEN)")
	}
	timeout := time.Duration(s.cfg.Usage.Timeout)
	if s.timeout > 0 {
		timeout = s.timeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := usage.Refresh(ctx, s.client, s.cfg, c, s.now)
	if berr := usage.RefreshBilling(ctx, s.client, s.cfg, c, s.now); berr != nil {
		s.log.Warnf("GitHub billing report not read, using the estimate: %v", berr)
	}
	if serr := c.Save(file); serr != nil {
		s.log.Warnf("cannot save usage cache: %v", serr)
	}
	if err != nil {
		if c.UpdatedAt.IsZero() {
			// Never had a full picture this month: unknown.
			return nil, fmt.Errorf("usage refresh failed: %w", err)
		}
		s.log.Warnf("usage refresh incomplete, using data from %s for the rest: %v",
			c.UpdatedAt.Format(time.RFC3339), err)
	}
	return usage.Totals(s.cfg, c), nil
}

func (s *liveState) Runners() ([]gh.Runner, error) {
	s.runnersOnce.Do(func() {
		if s.repo == "" {
			s.runnersErr = errors.New("GITHUB_REPOSITORY is not set (use --repo)")
			return
		}
		if runnersToken() == "" {
			s.runnersErr = errors.New("no token (set SWITCHYARD_TOKEN or SWITCHYARD_RUNNERS_TOKEN)")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cl := s.runCl
		if cl == nil {
			cl = s.client
		}
		rs, err := cl.ListRepoRunners(ctx, s.repo)
		if err != nil {
			s.runnersErr = err
			return
		}
		_, repoName, _ := strings.Cut(s.repo, "/")
		for _, org := range s.cfg.SelfHosted.Orgs {
			// Only runners in groups this repository may use.
			more, err := cl.ListOrgRunnersFor(ctx, org, repoName)
			if err != nil {
				s.log.Warnf("cannot list runners of org %s: %v", org, err)
				continue
			}
			rs = append(rs, more...)
		}
		s.runners = rs
	})
	return s.runners, s.runnersErr
}

// Queued returns the labels of jobs waiting for a runner in this
// repository and, for self_hosted.orgs, in that org's repositories pushed
// to in the last day (at most queuedRepoLimit of them).
func (s *liveState) Queued() ([][]string, error) {
	s.queuedOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		repos, err := s.queueRepos(ctx)
		if err != nil {
			s.queuedErr = err
			s.log.Warnf("cannot check queued jobs: %v", err)
			return
		}
		var (
			mu   sync.Mutex
			wg   sync.WaitGroup
			sem  = make(chan struct{}, 8)
			errs []error
		)
		add := func(labels []string, err error) {
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			s.queued = append(s.queued, labels)
		}
		for _, repo := range repos {
			// A run can move from queued to in_progress between the two
			// listings; look at each run once.
			seen := map[int64]bool{}
			for _, status := range []string{"queued", "in_progress"} {
				runs, err := s.client.ListActiveRuns(ctx, repo, status)
				if err != nil {
					add(nil, err)
					continue
				}
				for _, run := range runs {
					if seen[run.ID] || fmt.Sprint(run.ID) == s.runID {
						continue // our own later jobs are not waiting yet
					}
					seen[run.ID] = true
					wg.Add(1)
					go func(repo string, id int64) {
						defer wg.Done()
						sem <- struct{}{}
						defer func() { <-sem }()
						jobs, err := s.client.ListLatestJobs(ctx, repo, id)
						if err != nil {
							add(nil, err)
							return
						}
						for _, j := range jobs {
							switch j.Status {
							case "queued", "pending", "requested":
								add(j.Labels, nil)
							}
						}
					}(repo, run.ID)
				}
			}
		}
		wg.Wait()
		if len(errs) > 0 {
			// Partial answers are still useful; report the first error.
			s.log.Warnf("queued jobs may be undercounted: %v", errs[0])
		}
	})
	return s.queued, s.queuedErr
}

const queuedRepoLimit = 50

// recentlyActive returns the repositories (lowercased) that had a job
// start within d, according to the usage cache, if there is one.
func (s *liveState) recentlyActive(d time.Duration) map[string]bool {
	out := map[string]bool{}
	if len(s.cfg.SelfHosted.Orgs) == 0 {
		return out
	}
	scope := usage.Scope(s.cfg)
	file := firstNonEmpty(os.Getenv("SWITCHYARD_CACHE_FILE"), s.cfg.Usage.CacheFile, usage.DefaultCacheFile(scope))
	c := usage.LoadCache(file, scope, s.now)
	for name, rs := range c.Repos {
		for _, run := range rs.Runs {
			if run == nil {
				continue
			}
			for _, j := range run.Jobs {
				if s.now.Sub(j.Started) < d {
					out[strings.ToLower(name)] = true
				}
			}
		}
	}
	return out
}

func (s *liveState) queueRepos(ctx context.Context) ([]string, error) {
	if s.repo == "" {
		return nil, errors.New("GITHUB_REPOSITORY is not set (use --repo)")
	}
	out := []string{s.repo}
	var recent []gh.Repo
	active := s.recentlyActive(48 * time.Hour)
	for _, org := range s.cfg.SelfHosted.Orgs {
		rs, err := s.client.ListAccountRepos(ctx, org)
		if err != nil {
			s.log.Warnf("cannot list repositories of %s for the queue check: %v", org, err)
			continue
		}
		for _, r := range rs {
			// Repositories with recent pushes or recent jobs (scheduled
			// and manual runs don't push) are the ones that can have
			// queued jobs.
			if (s.now.Sub(r.PushedAt) < 24*time.Hour || active[strings.ToLower(r.FullName)]) && !strings.EqualFold(r.FullName, s.repo) {
				recent = append(recent, r)
			}
		}
	}
	sort.Slice(recent, func(i, j int) bool { return recent[i].PushedAt.After(recent[j].PushedAt) })
	for _, r := range recent {
		if len(out) >= queuedRepoLimit {
			s.log.Warnf("queue check limited to %d repositories; queued jobs elsewhere are not seen", queuedRepoLimit)
			break
		}
		out = append(out, r.FullName)
	}
	return out, nil
}

// ---- pick ----

func cmdPick(args []string, stdout io.Writer, log *logger) (code int) {
	fs := flag.NewFlagSet("pick", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "config file (default: $SWITCHYARD_CONFIG, ./switchyard.yml, ./.github/switchyard.yml)")
	size := fs.String("size", envOr("SWITCHYARD_SIZE", "small"), "runner size: small, medium or large")
	arch := fs.String("arch", envOr("SWITCHYARD_ARCH", "any"), "runner arch: x64, arm64 or any")
	fallback := fs.String("fallback", os.Getenv("SWITCHYARD_FALLBACK"), "label to use when nothing fits (default: config fallback, then ubuntu-latest)")
	disable := fs.String("disable", os.Getenv("SWITCHYARD_DISABLE"), "comma-separated providers to skip this time")
	repo := fs.String("repo", os.Getenv("GITHUB_REPOSITORY"), "owner/name whose self-hosted runners are checked")
	refresh := fs.Bool("refresh", false, "refresh usage even if the cache is fresh")
	jobs := fs.String("jobs", os.Getenv("SWITCHYARD_JOBS"), "jobs that each get their own runner, e.g. \"check, test:medium*5\" (output: runners)")
	if err := fs.Parse(args); err != nil {
		// Even bad flags must not break CI.
		out := firstNonEmpty(*fallback, config.DefaultFallback)
		log.Warnf("bad arguments, using fallback %s", out)
		emit(stdout, log, pick.Decision{Label: out, Fallback: true}, nil, sameRunner(*jobs, out))
		return 0
	}

	var cfg *config.Config
	defer func() {
		if r := recover(); r != nil {
			out := config.DefaultFallback
			if cfg != nil {
				out = cfg.Fallback
			}
			out = firstNonEmpty(*fallback, out)
			log.Warnf("internal error (%v), using fallback %s", r, out)
			emit(stdout, log, pick.Decision{Label: out, Fallback: true}, nil, sameRunner(*jobs, out))
			code = 0
		}
	}()

	cfg, err := config.Load(configPath(*cfgFlag))
	if err != nil {
		out := firstNonEmpty(*fallback, config.DefaultFallback)
		log.Warnf("config: %v; using fallback %s", err, out)
		emit(stdout, log, pick.Decision{Label: out, Fallback: true}, nil, sameRunner(*jobs, out))
		return 0
	}
	if *fallback != "" {
		cfg.Fallback = *fallback
	}
	if !validSize(*size) {
		log.Warnf("unknown size %q, using small", *size)
		*size = "small"
	}
	a := *arch
	switch a {
	case "any", "":
		a = ""
	case "x64", "arm64":
	default:
		log.Warnf("unknown arch %q, using any", a)
		a = ""
	}

	st := &liveState{cfg: cfg, client: newClient(), runCl: newRunnersClient(), runID: os.Getenv("GITHUB_RUN_ID"),
		log: log, repo: *repo, refresh: *refresh, now: time.Now()}
	ev := readEvent()
	req := pick.Request{
		PublicRepo:    ev.Public,
		ForkPR:        ev.ForkPR,
		Size:          *size,
		Arch:          a,
		RunID:         os.Getenv("GITHUB_RUN_ID"),
		CurrentRunner: os.Getenv("RUNNER_NAME"),
		Disabled:      map[string]bool{},
	}
	for _, n := range strings.Split(*disable, ",") {
		if n = strings.TrimSpace(n); n != "" {
			req.Disabled[n] = true
		}
	}

	slots, err := pick.ParseJobs(*jobs, req.Size)
	if err != nil {
		// Jobs reading the runners output get an empty runs-on and fail,
		// which shows the mistake; the other outputs still work.
		log.Errorf("jobs: %v; the runners output is empty", err)
		slots = nil
	}

	d := pick.Choose(cfg, req, st)
	for _, n := range d.Notes {
		log.Infof("skip %s", n)
	}
	if d.Stop && len(slots) == 0 {
		log.Errorf("stopping for size=%s arch=%s: %s (fallback %s)", *size, *arch, d.StopReason, d.Label)
		emit(stdout, log, d, nil, "{}")
		return exitStop
	}
	switch {
	case d.Stop:
		log.Warnf("runner is empty for size=%s arch=%s: %s", *size, *arch, d.StopReason)
		d.Label = ""
	case d.Fallback:
		extra := ""
		if d.Paid {
			extra = " (its free minutes are used up, so this run is billed)"
		}
		log.Warnf("no provider available for size=%s arch=%s, using fallback %s%s", *size, *arch, d.Label, extra)
	default:
		kind := "free"
		if d.Paid {
			kind = "paid"
		}
		log.Infof("picked %s (%s, %s)", d.Label, d.Provider, kind)
	}

	// Labels for every size, so one pick job can serve jobs of different sizes.
	perSize := map[string]string{}
	for _, s := range config.Sizes {
		if s == req.Size {
			perSize[s] = d.Label
			continue
		}
		r := req
		r.Size = s
		other := pick.Choose(cfg, r, st)
		if other.Stop {
			// Empty on purpose: a job using it fails instead of running
			// on a billed fallback, or on a self-hosted runner for a fork.
			log.Warnf("runner_%s is empty: %s", s, other.StopReason)
			perSize[s] = ""
			continue
		}
		perSize[s] = other.Label
	}

	// One runner per listed job.
	runners := map[string]string{}
	stopped := 0
	for i, sd := range pick.Assign(cfg, req, st, slots) {
		name := slots[i].Name
		switch {
		case sd.Stop:
			stopped++
			runners[name] = ""
			log.Warnf("job %s: no runner (%s)", name, sd.StopReason)
		case sd.Fallback:
			runners[name] = sd.Label
			log.Warnf("job %s: fallback %s", name, sd.Label)
		default:
			runners[name] = sd.Label
			log.Infof("job %s: %s (%s)", name, sd.Label, sd.Provider)
		}
	}
	emit(stdout, log, d, perSize, toJSON(runners))
	if len(slots) > 0 && stopped == len(slots) {
		log.Errorf("stopping: no job of this run has a runner")
		return exitStop
	}
	return 0
}

// sameRunner is the runners output when every job gets the same label
// (the fallback paths). A bad job list gives an empty object.
func sameRunner(spec, label string) string {
	slots, err := pick.ParseJobs(spec, "small")
	if err != nil {
		return "{}"
	}
	m := map[string]string{}
	for _, s := range slots {
		m[s.Name] = label
	}
	return toJSON(m)
}

func toJSON(m map[string]string) string {
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func emit(stdout io.Writer, log *logger, d pick.Decision, perSize map[string]string, runners string) {
	fmt.Fprintln(stdout, d.Label)
	file := os.Getenv("GITHUB_OUTPUT")
	if file == "" {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "runner=%s\n", d.Label)
	fmt.Fprintf(&b, "provider=%s\n", d.Provider)
	fmt.Fprintf(&b, "paid=%t\n", d.Paid)
	fmt.Fprintf(&b, "fallback=%t\n", d.Fallback)
	for _, s := range config.Sizes {
		l, ok := perSize[s]
		if !ok {
			l = d.Label
		}
		fmt.Fprintf(&b, "runner_%s=%s\n", s, l)
	}
	fmt.Fprintf(&b, "runners=%s\n", runners)
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		log.Warnf("cannot write GITHUB_OUTPUT: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		log.Warnf("cannot write GITHUB_OUTPUT: %v", err)
	}
}

// ---- usage ----

func cmdUsage(args []string, stdout io.Writer, log *logger) int {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "config file")
	refresh := fs.Bool("refresh", false, "refresh even if the cache is fresh")
	timeout := fs.Duration("timeout", 5*time.Minute, "time limit for a refresh (pick uses usage.timeout)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(configPath(*cfgFlag))
	if err != nil {
		log.Warnf("config: %v", err)
		return 1
	}
	now := time.Now()
	st := &liveState{cfg: cfg, client: newClient(), log: log, refresh: *refresh, timeout: *timeout, now: now}
	totals, err := st.Totals()
	if err != nil {
		log.Warnf("%v", err)
		return 1
	}
	fmt.Fprintf(stdout, "Usage for %s (UTC calendar month, weighted minutes)\n\n", usage.MonthStart(now).Format("January 2006"))
	stuck := st.Stuck()
	writeUsageTable(stdout, cfg, totals, stuck)
	for i := range cfg.Providers {
		if why, ok := stuck[cfg.Providers[i].Name]; ok {
			fmt.Fprintf(stdout, "\n%s %s\n", cfg.Providers[i].Name, why)
		}
	}
	return 0
}

func writeUsageTable(w io.Writer, cfg *config.Config, totals map[string]usage.Total, stuck map[string]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "PROVIDER\tJOBS\tMINUTES\tWEIGHTED\tBILLED\tFREE\tLEFT\tCAP\tEST. COST\tSTATUS\t")
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		t := totals[p.Name]
		free, left, capS, cost, billed := "-", "-", "-", "-", "-"
		if t.HasBilled {
			billed = fmt.Sprintf("%.0f", t.Billed)
		}
		status := "ok"
		if p.Kind == config.KindSelfHosted {
			status = "self-hosted"
		} else {
			free = fmt.Sprintf("%.0f", p.FreeMinutes)
			left = fmt.Sprintf("%.0f", max(0, p.FreeMinutes-t.Weighted))
			var caps []string
			if p.CapMinutes > 0 {
				caps = append(caps, fmt.Sprintf("%.0f min", p.CapMinutes))
			}
			if p.CapUSD > 0 {
				caps = append(caps, fmt.Sprintf("$%.2f", p.CapUSD))
			}
			if len(caps) > 0 {
				capS = strings.Join(caps, " / ")
			}
			if p.PricePerMinute > 0 {
				cost = fmt.Sprintf("$%.2f", pick.Cost(p, t.Weighted))
			}
			margin := p.Margin(cfg)
			switch {
			case p.FreeMinutes-t.Weighted > margin:
				status = "free"
			case !p.AllowPaid:
				status = "exhausted"
			case p.CapMinutes > 0 && t.Weighted+margin >= p.CapMinutes,
				p.CapUSD > 0 && pick.Cost(p, t.Weighted)+margin*p.PricePerMinute >= p.CapUSD:
				status = "at cap"
			default:
				status = "paid"
			}
		}
		if _, ok := stuck[p.Name]; ok {
			status = "stuck"
		}
		if !p.IsEnabled() {
			status = "disabled"
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%.0f\t%s\t%s\t%s\t%s\t%s\t%s\t\n",
			p.Name, t.Jobs, t.Minutes, t.Estimated, billed, free, left, capS, cost, status)
	}
	if t, ok := totals[usage.Unattributed]; ok {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%.0f\t-\t-\t-\t-\t-\tnot counted\t\n", usage.Unattributed, t.Jobs, t.Minutes, t.Weighted)
	}
	tw.Flush()
}

// ---- check ----

func cmdCheck(args []string, stdout io.Writer, log *logger) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	cfgFlag := fs.String("config", "", "config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	path := configPath(*cfgFlag)
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(stdout, "%s: invalid\n%v\n", path, err)
		return 1
	}
	_, warnings := cfg.Validate()
	for _, w := range warnings {
		fmt.Fprintf(stdout, "warning: %s\n", w)
	}
	fmt.Fprintf(stdout, "%s: ok (%d providers, fallback %s, on_exhausted %s)\n", path, len(cfg.Providers), cfg.Fallback, cfg.OnExhausted)
	fmt.Fprintln(stdout, "\nToken permissions this config needs (fine-grained token):")
	for _, l := range permissions(cfg) {
		fmt.Fprintf(stdout, "  %s\n", l)
	}
	return 0
}

// permissions lists what the tokens need for this config.
func permissions(cfg *config.Config) []string {
	out := []string{
		"token: Actions: read and Metadata: read on every scanned repository (Contents: read too if the action is private)",
	}
	selfHosted, billing := false, false
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.IsEnabled() {
			continue
		}
		selfHosted = selfHosted || p.Kind == config.KindSelfHosted
		billing = billing || p.BillingAPI
	}
	if billing {
		out = append(out, "token: Plan: read (user accounts) or Administration: read (organizations), for billing_api")
	}
	if selfHosted {
		s := "runners-token: Administration: read on the repositories that run switchyard"
		if len(cfg.SelfHosted.Orgs) > 0 {
			s += ", and organization Self-hosted runners: read"
		}
		out = append(out, s+" (only used to list runners)")
	}
	return out
}

// ---- helpers ----

func validSize(s string) bool {
	for _, v := range config.Sizes {
		if v == s {
			return true
		}
	}
	return false
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
