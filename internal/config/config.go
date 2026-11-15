// Package config loads and validates the switchyard configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Provider kinds.
const (
	KindHosted     = "hosted"      // billed by the minute (third-party or GitHub-hosted)
	KindSelfHosted = "self-hosted" // your own machines; free, picked only when idle
)

// Sizes a job can ask for.
var Sizes = []string{"small", "medium", "large"}

// Architectures a runner can have.
var Archs = []string{"x64", "arm64"}

// Defaults.
const (
	DefaultFallback      = "ubuntu-latest"
	DefaultMarginMinutes = 60
	DefaultCacheTTL      = 10 * time.Minute
	DefaultStuckAfter    = 10 * time.Minute
	DefaultTimeout       = 20 * time.Second
)

// What to do when no provider can take the job (see Config.OnExhausted).
const (
	OnExhaustedFallback = "fallback"
	OnExhaustedFail     = "fail"
)

// Config is the whole switchyard.yml file.
type Config struct {
	// Fallback is the runs-on label printed when nothing else can be used
	// or when anything goes wrong.
	Fallback string `yaml:"fallback"`
	// MarginMinutes: stop using a provider's free minutes this many
	// (weighted) minutes before they run out. Per-provider override exists.
	MarginMinutes float64 `yaml:"margin_minutes"`
	// PaidOrder optionally sets a different priority for the paid pass.
	// Defaults to the order of Providers.
	PaidOrder []string `yaml:"paid_order"`
	// OnExhausted decides what happens when every provider is known to be
	// used up: "fallback" (default) prints the fallback label anyway;
	// "fail" makes pick fail instead, unless the fallback is known to be
	// free. Errors (usage unknown, API down) always use the fallback.
	OnExhausted string `yaml:"on_exhausted"`
	// StuckAfter: a hosted provider that left a job waiting this long
	// without giving it a runner is skipped until it runs jobs again (or
	// for StuckCooldown after that job ended). Default 10m; 0 turns it off.
	StuckAfter *Duration `yaml:"stuck_after"`

	Usage      Usage      `yaml:"usage"`
	SelfHosted SelfHosted `yaml:"self_hosted"`
	Providers  []Provider `yaml:"providers"`
}

// Usage controls how minutes are counted.
type Usage struct {
	// Accounts whose repositories are scanned (users or orgs).
	// Defaults to $GITHUB_REPOSITORY_OWNER.
	Accounts []string `yaml:"accounts"`
	// Repos, if set, replaces account scanning with an explicit
	// list of "owner/name" repositories.
	Repos []string `yaml:"repos"`
	// CacheFile is where the usage state is kept between runs.
	// Defaults to $XDG_CACHE_HOME/switchyard (or ~/.cache/switchyard).
	CacheFile string `yaml:"cache_file"`
	// CacheTTL: how old the cached usage may be before it is refreshed.
	CacheTTL Duration `yaml:"cache_ttl"`
	// Timeout bounds a usage refresh. Work done before the timeout is kept.
	Timeout Duration `yaml:"timeout"`
}

// SelfHosted controls where self-hosted runners are looked up.
type SelfHosted struct {
	// Orgs whose org-level runners are also checked (besides the
	// current repository's runners).
	Orgs []string `yaml:"orgs"`
}

// Provider is one place jobs can run.
type Provider struct {
	Name    string `yaml:"name"`
	Kind    string `yaml:"kind"`
	Enabled *bool  `yaml:"enabled"`

	// FreeMinutes included per calendar month, in weighted minutes
	// (i.e. minutes on a multiplier-1 runner).
	FreeMinutes float64 `yaml:"free_minutes"`
	// AllowPaid lets switchyard keep routing here after the free
	// minutes are used, up to the caps.
	AllowPaid bool `yaml:"allow_paid"`
	// CapMinutes is a hard monthly limit on weighted minutes (0 = none).
	CapMinutes float64 `yaml:"cap_minutes"`
	// CapUSD is a hard monthly limit on estimated spend (0 = none).
	CapUSD float64 `yaml:"cap_usd"`
	// PricePerMinute in USD for a multiplier-1 runner, used for CapUSD
	// and the usage report.
	PricePerMinute float64 `yaml:"price_per_minute"`
	// MarginMinutes overrides the global margin for this provider.
	MarginMinutes *float64 `yaml:"margin_minutes"`
	// IgnorePublicRepos: jobs in public repositories don't count
	// (true for GitHub-hosted runners, which are free for public repos).
	IgnorePublicRepos bool `yaml:"ignore_public_repos"`
	// BillingAPI: read GitHub's billing usage report for the scanned
	// accounts and count from it, plus the estimate for the last couple of
	// hours. Only for the provider that represents GitHub-hosted runners.
	BillingAPI bool `yaml:"billing_api"`

	Runners []Runner    `yaml:"runners"`
	Count   []CountRule `yaml:"count"`
}

// Runner is one runs-on label a provider offers.
type Runner struct {
	Label      string  `yaml:"label"`
	Size       string  `yaml:"size"`
	Arch       string  `yaml:"arch"`
	Multiplier float64 `yaml:"multiplier"`
}

// CountRule attributes past jobs to a provider by label pattern
// (path.Match syntax), for labels you don't route to but still use.
type CountRule struct {
	Pattern    string  `yaml:"pattern"`
	Multiplier float64 `yaml:"multiplier"`
}

// Duration is a time.Duration written as "10m", "30s", ...
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

// IsEnabled reports whether the provider is enabled (default true).
func (p *Provider) IsEnabled() bool { return p.Enabled == nil || *p.Enabled }

// Margin returns the provider's margin in weighted minutes.
func (p *Provider) Margin(c *Config) float64 {
	if p.MarginMinutes != nil {
		return *p.MarginMinutes
	}
	return c.MarginMinutes
}

// Provider returns the provider with the given name, or nil.
func (c *Config) Provider(name string) *Provider {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i]
		}
	}
	return nil
}

// Load reads, defaults and validates a config file.
func Load(file string) (*Config, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes, defaults and validates config bytes.
func Parse(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if errs, _ := c.Validate(); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Fallback == "" {
		c.Fallback = DefaultFallback
	}
	if c.OnExhausted == "" {
		c.OnExhausted = OnExhaustedFallback
	}
	if c.MarginMinutes == 0 {
		c.MarginMinutes = DefaultMarginMinutes
	}
	if c.Usage.CacheTTL == 0 {
		c.Usage.CacheTTL = Duration(DefaultCacheTTL)
	}
	if c.Usage.Timeout == 0 {
		c.Usage.Timeout = Duration(DefaultTimeout)
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Kind == "" {
			p.Kind = KindHosted
		}
		for j := range p.Runners {
			r := &p.Runners[j]
			if r.Size == "" {
				r.Size = "small"
			}
			if r.Arch == "" {
				r.Arch = "x64"
			}
			if r.Multiplier == 0 {
				r.Multiplier = 1
			}
		}
		for j := range p.Count {
			if p.Count[j].Multiplier == 0 {
				p.Count[j].Multiplier = 1
			}
		}
	}
}

// StuckWait returns how long a job may wait for a hosted provider's runner
// before the provider counts as stuck; 0 means never.
func (c *Config) StuckWait() time.Duration {
	if c.StuckAfter == nil {
		return DefaultStuckAfter
	}
	return time.Duration(*c.StuckAfter)
}

// Validate returns hard errors and soft warnings.
func (c *Config) Validate() (errs []error, warnings []string) {
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if len(c.Providers) == 0 {
		bad("no providers configured")
	}
	if c.MarginMinutes < 0 {
		bad("margin_minutes must be >= 0")
	}
	if c.StuckAfter != nil && *c.StuckAfter < 0 {
		bad("stuck_after must be >= 0")
	}
	if c.OnExhausted != OnExhaustedFallback && c.OnExhausted != OnExhaustedFail {
		bad("on_exhausted must be %q or %q, got %q", OnExhaustedFallback, OnExhaustedFail, c.OnExhausted)
	}
	for _, r := range c.Usage.Repos {
		if strings.Count(r, "/") != 1 || strings.HasPrefix(r, "/") || strings.HasSuffix(r, "/") {
			bad("usage.repos: %q is not owner/name", r)
		}
	}

	billing := 0
	names := map[string]bool{}
	labels := map[string]string{} // lowercased label -> provider
	for i := range c.Providers {
		p := &c.Providers[i]
		where := fmt.Sprintf("providers[%d]", i)
		if p.Name == "" {
			bad("%s: name is required", where)
		} else {
			where = fmt.Sprintf("provider %q", p.Name)
			if names[p.Name] {
				bad("%s: duplicate name", where)
			}
			names[p.Name] = true
		}
		if p.Kind != KindHosted && p.Kind != KindSelfHosted {
			bad("%s: kind must be %q or %q, got %q", where, KindHosted, KindSelfHosted, p.Kind)
		}
		if len(p.Runners) == 0 {
			bad("%s: at least one runner is required", where)
		}
		for _, v := range []struct {
			name string
			val  float64
		}{{"free_minutes", p.FreeMinutes}, {"cap_minutes", p.CapMinutes}, {"cap_usd", p.CapUSD}, {"price_per_minute", p.PricePerMinute}} {
			if v.val < 0 {
				bad("%s: %s must be >= 0", where, v.name)
			}
		}
		if p.MarginMinutes != nil && *p.MarginMinutes < 0 {
			bad("%s: margin_minutes must be >= 0", where)
		}
		seenSlot := map[string]bool{}
		for j, r := range p.Runners {
			rw := fmt.Sprintf("%s runners[%d]", where, j)
			if r.Label == "" {
				bad("%s: label is required", rw)
				continue
			}
			if !contains(Sizes, r.Size) {
				bad("%s: size must be one of %v, got %q", rw, Sizes, r.Size)
			}
			if !contains(Archs, r.Arch) {
				bad("%s: arch must be one of %v, got %q", rw, Archs, r.Arch)
			}
			if r.Multiplier <= 0 {
				bad("%s: multiplier must be > 0", rw)
			}
			slot := r.Size + "/" + r.Arch
			if seenSlot[slot] {
				warnings = append(warnings, fmt.Sprintf("%s: more than one runner for %s; only the first is picked", where, slot))
			}
			seenSlot[slot] = true
			l := strings.ToLower(r.Label)
			if other, ok := labels[l]; ok && other != p.Name {
				bad("%s: label %q is also used by provider %q", rw, r.Label, other)
			}
			labels[l] = p.Name
		}
		for j, cr := range p.Count {
			if _, err := path.Match(cr.Pattern, ""); err != nil || cr.Pattern == "" {
				bad("%s count[%d]: invalid pattern %q", where, j, cr.Pattern)
			}
			if cr.Multiplier <= 0 {
				bad("%s count[%d]: multiplier must be > 0", where, j)
			}
		}

		if p.BillingAPI {
			billing++
			if p.Kind != KindHosted || p.PricePerMinute <= 0 {
				bad("%s: billing_api needs a hosted provider with price_per_minute", where)
			}
		}

		switch p.Kind {
		case KindSelfHosted:
			if p.FreeMinutes > 0 || p.CapMinutes > 0 || p.CapUSD > 0 || p.AllowPaid {
				warnings = append(warnings, fmt.Sprintf("%s: minutes, caps and allow_paid are ignored for self-hosted providers", where))
			}
		case KindHosted:
			if p.FreeMinutes == 0 && !p.AllowPaid {
				warnings = append(warnings, fmt.Sprintf("%s: no free minutes and allow_paid is false, so it is never picked", where))
			}
			if p.AllowPaid && p.CapMinutes == 0 && p.CapUSD == 0 {
				warnings = append(warnings, fmt.Sprintf("%s: allow_paid without cap_minutes or cap_usd means unlimited spend", where))
			}
			if p.CapUSD > 0 && p.PricePerMinute == 0 {
				bad("%s: cap_usd needs price_per_minute", where)
			}
		}
	}

	if billing > 1 {
		bad("billing_api can be set on one provider only (the one for GitHub-hosted runners)")
	}

	seen := map[string]bool{}
	for _, n := range c.PaidOrder {
		p := c.Provider(n)
		switch {
		case p == nil:
			bad("paid_order: unknown provider %q", n)
		case seen[n]:
			bad("paid_order: %q listed twice", n)
		case p.Kind == KindSelfHosted || !p.AllowPaid:
			warnings = append(warnings, fmt.Sprintf("paid_order: %q does not allow paid use, entry has no effect", n))
		}
		seen[n] = true
	}
	return errs, warnings
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// RunIDPlaceholder in a runner label is replaced with $GITHUB_RUN_ID
// when picking (RunsOn needs the run id in its label).
const RunIDPlaceholder = "{{run_id}}"

// Render returns the runs-on label for this runner.
func (r Runner) Render(runID string) string {
	return strings.ReplaceAll(r.Label, RunIDPlaceholder, runID)
}

// Matches reports whether a job label was produced by this runner.
func (r Runner) Matches(jobLabel string) bool {
	if !strings.Contains(r.Label, RunIDPlaceholder) {
		return strings.EqualFold(r.Label, jobLabel)
	}
	pattern := strings.ReplaceAll(escapeGlob(strings.ToLower(r.Label)), RunIDPlaceholder, "*")
	ok, _ := path.Match(pattern, strings.ToLower(jobLabel))
	return ok
}

// Matches reports whether a job label matches the rule's pattern.
func (cr CountRule) Matches(jobLabel string) bool {
	ok, _ := path.Match(strings.ToLower(cr.Pattern), strings.ToLower(jobLabel))
	return ok
}

func escapeGlob(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`)
	return r.Replace(s)
}

// Match finds which runner or count rule of this provider produced a
// job with the given labels, and returns its multiplier.
func (p *Provider) Match(jobLabels []string) (multiplier float64, ok bool) {
	for _, r := range p.Runners {
		for _, l := range jobLabels {
			if r.Matches(l) {
				return r.Multiplier, true
			}
		}
	}
	for _, cr := range p.Count {
		for _, l := range jobLabels {
			if cr.Matches(l) {
				return cr.Multiplier, true
			}
		}
	}
	return 0, false
}
