// Package usage counts runner minutes per provider for the current
// calendar month (UTC) from the GitHub Actions API, with a local cache
// so most calls don't hit the API at all.
package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/isromero/switchyard/internal/config"
	"github.com/isromero/switchyard/internal/gh"
)

// API is the subset of the GitHub client used here (fakeable in tests).
type API interface {
	ListAccountRepos(ctx context.Context, account string) ([]gh.Repo, error)
	GetRepo(ctx context.Context, fullName string) (gh.Repo, error)
	ListRuns(ctx context.Context, fullName string, from, to time.Time) ([]gh.Run, error)
	ListJobs(ctx context.Context, fullName string, runID int64) ([]gh.Job, error)
}

// idleRecheck is how often a repository with no runs this month (and
// no push since the last check) is listed again. It only matters for
// scheduled or manually started workflows in otherwise quiet repos.
const idleRecheck = time.Hour

// rerunWindow is how far before the month start runs are listed.
const rerunWindow = 31 * 24 * time.Hour

// cacheVersion is bumped when the cache format changes.
const cacheVersion = 3

// Cache is the on-disk state. It holds raw job records, not totals, so
// a config change (labels, multipliers) applies without refetching.
type Cache struct {
	Version   int                   `json:"version"`
	Month     string                `json:"month"` // "2026-11"
	Scope     string                `json:"scope"` // what was scanned; see Scope
	Complete  bool                  `json:"complete"`
	UpdatedAt time.Time             `json:"updated_at"`
	Repos     map[string]*RepoState `json:"repos"`
	// Billing is GitHub's own report for the provider with billing_api,
	// in weighted minutes (gross amount / price per minute).
	Billing *Billing `json:"billing,omitempty"`
}

// Billing is what GitHub's billing usage report said this month.
type Billing struct {
	Weighted  float64   `json:"weighted"`
	FetchedAt time.Time `json:"fetched_at"`
}

// RepoState is what is cached for one repository.
type RepoState struct {
	Private   bool                `json:"private"`
	CheckedAt time.Time           `json:"checked_at"`
	Runs      map[int64]*RunState `json:"runs"`
}

// RunState is what is cached for one workflow run.
type RunState struct {
	Attempt   int       `json:"attempt"`
	Completed bool      `json:"completed"`
	Jobs      []JobInfo `json:"jobs"`
	// Waits are jobs that are waiting for a runner, or ended without one.
	Waits []Wait `json:"waits,omitempty"`
}

// Wait is a job that never got a runner (so far).
type Wait struct {
	Labels []string  `json:"labels"`
	Since  time.Time `json:"since"`          // when it was queued
	Until  time.Time `json:"until,omitzero"` // when it ended; zero while queued
}

// JobInfo is one job, reduced to what counting needs.
type JobInfo struct {
	Labels  []string  `json:"labels"`
	Started time.Time `json:"started"`
	Minutes int       `json:"minutes"` // rounded up per job, like GitHub bills
}

// MonthStart returns the first instant of t's month in UTC.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// NewCache returns an empty cache for the month containing now.
func NewCache(now time.Time) *Cache {
	return &Cache{Version: cacheVersion, Month: MonthStart(now).Format("2006-01"), Repos: map[string]*RepoState{}}
}

// Scope identifies what a config scans (accounts or explicit repos), so
// a cache built for one scope is never used for another.
func Scope(cfg *config.Config) string {
	var parts []string
	if len(cfg.Usage.Repos) > 0 {
		for _, r := range cfg.Usage.Repos {
			parts = append(parts, "repo:"+strings.ToLower(r))
		}
	} else {
		accounts := cfg.Usage.Accounts
		if len(accounts) == 0 {
			accounts = []string{os.Getenv("GITHUB_REPOSITORY_OWNER")}
		}
		for _, a := range accounts {
			parts = append(parts, "account:"+strings.ToLower(a))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// DefaultCacheFile is used when the config doesn't set one. Each scope
// gets its own file, so repositories of different owners sharing a
// runner don't overwrite each other's cache.
func DefaultCacheFile(scope string) string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		if h, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(h, ".cache")
		} else {
			dir = os.TempDir()
		}
	}
	sum := sha256.Sum256([]byte(scope))
	return filepath.Join(dir, "switchyard", "usage-"+hex.EncodeToString(sum[:6])+".json")
}

// LoadCache reads the cache file. A missing, unreadable, outdated,
// last-month or other-scope file yields an empty cache for this month.
func LoadCache(file, scope string, now time.Time) *Cache {
	fresh := NewCache(now)
	fresh.Scope = scope
	b, err := os.ReadFile(file)
	if err != nil {
		return fresh
	}
	var c Cache
	if json.Unmarshal(b, &c) != nil || c.Version != cacheVersion || c.Month != fresh.Month || c.Scope != scope || c.Repos == nil {
		return fresh
	}
	return &c
}

// Save writes the cache atomically.
func (c *Cache) Save(file string) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".usage-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), file)
}

// Fresh reports whether the cache can be used without refreshing.
func (c *Cache) Fresh(now time.Time, ttl time.Duration) bool {
	return c.Complete && now.Sub(c.UpdatedAt) < ttl
}

// Refresh brings the cache up to date. Completed runs already in the
// cache are not fetched again. On errors (or when ctx expires) every
// run fetched so far is kept, runs that failed keep their old state,
// the cache is marked incomplete and the error lists the repositories.
func Refresh(ctx context.Context, api API, cfg *config.Config, c *Cache, now time.Time) error {
	repos, err := listRepos(ctx, api, cfg)
	if err != nil {
		return fmt.Errorf("list repositories: %w", err)
	}
	// Runs can be re-run for up to 30 days after they were created, so a
	// run from last month can still use minutes this month. Jobs are
	// counted by the month they started in.
	from := MonthStart(now).Add(-rerunWindow)
	to := now.Add(time.Minute)

	var (
		mu      sync.Mutex
		errs    []error
		wg      sync.WaitGroup
		repoSem = make(chan struct{}, 12)
		jobSem  = make(chan struct{}, 16)
	)
	for _, r := range repos {
		wg.Add(1)
		go func(r gh.Repo) {
			defer wg.Done()
			repoSem <- struct{}{}
			defer func() { <-repoSem }()

			mu.Lock()
			old := c.Repos[r.FullName]
			mu.Unlock()
			if old != nil && len(old.Runs) == 0 && now.Sub(old.CheckedAt) < idleRecheck && !r.PushedAt.After(old.CheckedAt) {
				return
			}
			st, err := refreshRepo(ctx, api, r, old, from, to, now, jobSem)
			mu.Lock()
			defer mu.Unlock()
			if st != nil {
				if err != nil {
					// Partial: keep the progress, but don't let the idle
					// shortcut skip this repository next time.
					st.CheckedAt = time.Time{}
				}
				c.Repos[r.FullName] = st
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", r.FullName, err))
			}
		}(r)
	}
	wg.Wait()
	if len(errs) > 0 {
		c.Complete = false
		return errors.Join(errs...)
	}
	c.Complete = true
	c.UpdatedAt = now
	return nil
}

func listRepos(ctx context.Context, api API, cfg *config.Config) ([]gh.Repo, error) {
	if len(cfg.Usage.Repos) > 0 {
		var out []gh.Repo
		for _, name := range cfg.Usage.Repos {
			r, err := api.GetRepo(ctx, name)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
		return out, nil
	}
	accounts := cfg.Usage.Accounts
	if len(accounts) == 0 {
		if o := os.Getenv("GITHUB_REPOSITORY_OWNER"); o != "" {
			accounts = []string{o}
		}
	}
	if len(accounts) == 0 {
		return nil, errors.New("no usage.accounts configured and GITHUB_REPOSITORY_OWNER is not set")
	}
	seen := map[string]bool{}
	var out []gh.Repo
	for _, a := range accounts {
		rs, err := api.ListAccountRepos(ctx, a)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			if !seen[r.FullName] {
				seen[r.FullName] = true
				out = append(out, r)
			}
		}
	}
	return out, nil
}

func refreshRepo(ctx context.Context, api API, r gh.Repo, old *RepoState, from, to, now time.Time, jobSem chan struct{}) (*RepoState, error) {
	runs, err := api.ListRuns(ctx, r.FullName, from, to)
	if err != nil {
		return nil, err
	}
	st := &RepoState{Private: r.Private, CheckedAt: now, Runs: map[int64]*RunState{}}

	type result struct {
		id  int64
		rs  *RunState
		err error
	}
	results := make(chan result)
	pending := 0
	for _, run := range runs {
		if _, dup := st.Runs[run.ID]; dup {
			continue
		}
		if old != nil {
			if prev, ok := old.Runs[run.ID]; ok && prev.Completed && prev.Attempt == run.RunAttempt {
				st.Runs[run.ID] = prev
				continue
			}
		}
		st.Runs[run.ID] = nil // placeholder, filled below
		pending++
		go func(run gh.Run) {
			jobSem <- struct{}{}
			defer func() { <-jobSem }()
			jobs, err := api.ListJobs(ctx, r.FullName, run.ID)
			if err != nil {
				results <- result{id: run.ID, err: err}
				return
			}
			rs := &RunState{Attempt: run.RunAttempt, Completed: run.Status == "completed"}
			for _, j := range jobs {
				if ji, ok := jobInfo(j, now); ok {
					rs.Jobs = append(rs.Jobs, ji)
				} else if w, ok := waitInfo(j); ok {
					rs.Waits = append(rs.Waits, w)
				}
				if j.Status != "completed" {
					rs.Completed = false
				}
			}
			results <- result{id: run.ID, rs: rs}
		}(run)
	}
	var firstErr error
	for ; pending > 0; pending-- {
		res := <-results
		if res.err != nil {
			if firstErr == nil {
				firstErr = res.err
			}
			// Keep what we had for this run, if anything.
			if old != nil && old.Runs[res.id] != nil {
				st.Runs[res.id] = old.Runs[res.id]
			} else {
				delete(st.Runs, res.id)
			}
			continue
		}
		st.Runs[res.id] = res.rs
	}
	// On error the state is partial but still worth keeping: the runs
	// fetched so far won't be fetched again.
	return st, firstErr
}

// jobInfo converts an API job. Jobs that never started are dropped;
// running jobs are counted up to now.
func jobInfo(j gh.Job, now time.Time) (JobInfo, bool) {
	if j.StartedAt == nil || j.StartedAt.IsZero() {
		return JobInfo{}, false
	}
	// GitHub sets started_at on jobs still waiting in the queue, and on
	// jobs that ended without ever getting a runner (cancelled, or not
	// started because of billing). Only jobs that had a runner count.
	if j.RunnerName == "" || (j.Status != "in_progress" && j.Status != "completed") {
		return JobInfo{}, false
	}
	end := now
	if j.CompletedAt != nil && !j.CompletedAt.IsZero() {
		end = *j.CompletedAt
	}
	d := end.Sub(*j.StartedAt)
	mins := 0
	if d > 0 {
		mins = int(math.Ceil(d.Minutes()))
	}
	return JobInfo{Labels: j.Labels, Started: j.StartedAt.UTC(), Minutes: mins}, true
}

// waitInfo returns the job as a Wait if it is queued, or ended (e.g.
// cancelled) without ever getting a runner.
func waitInfo(j gh.Job) (Wait, bool) {
	if j.RunnerName != "" || j.StartedAt == nil || j.StartedAt.IsZero() {
		return Wait{}, false
	}
	w := Wait{Labels: j.Labels, Since: j.StartedAt.UTC()}
	switch j.Status {
	case "queued":
	case "completed":
		if j.CompletedAt == nil || j.CompletedAt.IsZero() {
			return Wait{}, false
		}
		w.Until = j.CompletedAt.UTC()
	default: // waiting for an approval or another job: not the provider's fault
		return Wait{}, false
	}
	return w, true
}

// StuckCooldown is how long a provider stays skipped after its stuck job
// ended, if no other job ran on it since.
const StuckCooldown = time.Hour

// Stuck returns, per hosted provider, why it looks stuck: a job asked for
// one of its labels, waited at least after without getting a runner, and
// no job has run on the provider since that one was queued. That happens
// when the provider's app isn't set up, the account is out of credit, or
// the provider is down. Waits that ended more than StuckCooldown ago
// don't count. after <= 0 turns the check off.
func Stuck(cfg *config.Config, c *Cache, now time.Time, after time.Duration) map[string]string {
	if after <= 0 || c == nil {
		return nil
	}
	type stuck struct {
		w      Wait
		waited time.Duration
	}
	worst := map[string]stuck{}
	for _, rs := range c.Repos {
		for _, run := range rs.Runs {
			if run == nil {
				continue
			}
			for _, w := range run.Waits {
				name, _ := attribute(cfg, w.Labels)
				p := cfg.Provider(name)
				if p == nil || p.Kind != config.KindHosted {
					continue
				}
				end := w.Until
				if end.IsZero() {
					end = now
				} else if now.Sub(end) > StuckCooldown {
					continue
				}
				if waited := end.Sub(w.Since); waited >= after {
					if prev, ok := worst[name]; !ok || w.Since.After(prev.w.Since) {
						worst[name] = stuck{w, waited}
					}
				}
			}
		}
	}
	out := map[string]string{}
	for name, s := range worst {
		if ranSince(cfg, c, name, s.w.Since) {
			continue
		}
		labels := strings.Join(s.w.Labels, ",")
		mins := int(s.waited.Minutes())
		if s.w.Until.IsZero() {
			out[name] = fmt.Sprintf("looks stuck: a job for %s has waited %d min for a runner", labels, mins)
		} else {
			out[name] = fmt.Sprintf("looks stuck: a job for %s waited %d min and ended without a runner; skipped until %s",
				labels, mins, s.w.Until.Add(StuckCooldown).Format("15:04 MST"))
		}
	}
	return out
}

// ranSince reports whether a job on the provider got a runner after t.
func ranSince(cfg *config.Config, c *Cache, name string, t time.Time) bool {
	for _, rs := range c.Repos {
		for _, run := range rs.Runs {
			if run == nil {
				continue
			}
			for _, j := range run.Jobs {
				if j.Started.After(t) {
					if n, _ := attribute(cfg, j.Labels); n == name {
						return true
					}
				}
			}
		}
	}
	return false
}

// Total is the usage of one provider.
type Total struct {
	Jobs     int
	Minutes  int     // raw minutes (rounded up per job)
	Weighted float64 // minutes × size multiplier; from the billing report if read
	// Estimated is the weighted minutes counted from jobs, and Billed
	// what GitHub's billing report says (HasBilled), for billing_api.
	Estimated float64
	Billed    float64
	HasBilled bool
}

// BillingLag is how far back jobs are counted from the estimate on top
// of GitHub's billing report, in case the report doesn't have them yet.
// The report usually trails by minutes.
const BillingLag = 2 * time.Hour

// Unattributed is the key for jobs no provider claims.
const Unattributed = "(other)"

// Totals attributes the cached jobs of the cache's month to providers.
// Each job goes to the first provider (in config order) that matches
// one of its labels.
func Totals(cfg *config.Config, c *Cache) map[string]Total {
	start, err := time.Parse("2006-01", c.Month)
	if err != nil {
		return nil
	}
	end := start.AddDate(0, 1, 0)
	out := map[string]Total{}
	// Jobs that started shortly before the billing report was read, or
	// after, may not be in it yet; they are added to the billed figure.
	var recentFrom time.Time
	if c.Billing != nil {
		recentFrom = c.Billing.FetchedAt.Add(-BillingLag)
	}
	recent := map[string]float64{}
	for _, repo := range sortedKeys(c.Repos) {
		rs := c.Repos[repo]
		for _, run := range rs.Runs {
			if run == nil {
				continue
			}
			for _, j := range run.Jobs {
				if j.Started.Before(start) || !j.Started.Before(end) {
					continue
				}
				name, mult := attribute(cfg, j.Labels)
				if name != Unattributed {
					if p := cfg.Provider(name); p.IgnorePublicRepos && !rs.Private {
						continue
					}
				}
				t := out[name]
				t.Jobs++
				t.Minutes += j.Minutes
				t.Weighted += float64(j.Minutes) * mult
				out[name] = t
				if c.Billing != nil && !j.Started.Before(recentFrom) {
					recent[name] += float64(j.Minutes) * mult
				}
			}
		}
	}
	for k, t := range out {
		t.Estimated = t.Weighted
		out[k] = t
	}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if !p.BillingAPI || c.Billing == nil {
			continue
		}
		t := out[p.Name]
		t.HasBilled = true
		t.Billed = c.Billing.Weighted
		// GitHub's report is what is actually billed to these accounts.
		// The estimate can be far off: a repository moved from another
		// account keeps its runs, but they were billed to the old owner.
		// The report trails by minutes, so recent jobs come from the
		// estimate (counted twice if already billed: the safe side).
		t.Weighted = t.Billed + recent[p.Name]
		out[p.Name] = t
	}
	return out
}

// BillingAPI is the part of the GitHub client used for billing reports.
type BillingAPI interface {
	IsOrg(ctx context.Context, account string) (bool, error)
	GetRepo(ctx context.Context, fullName string) (gh.Repo, error)
	BillingUsage(ctx context.Context, account string, org bool, year int, month time.Month) ([]gh.BillingItem, error)
}

// RefreshBilling reads GitHub's billing usage report for the scanned
// accounts, if a provider has billing_api set. On error the previous
// figure (if any) is kept.
func RefreshBilling(ctx context.Context, api BillingAPI, cfg *config.Config, c *Cache, now time.Time) error {
	var p *config.Provider
	for i := range cfg.Providers {
		if cfg.Providers[i].BillingAPI && cfg.Providers[i].IsEnabled() {
			p = &cfg.Providers[i]
		}
	}
	if p == nil {
		return nil
	}
	accounts := billingAccounts(cfg)
	if len(accounts) == 0 {
		return errors.New("billing: no accounts to read")
	}
	month := MonthStart(now)
	var gross float64
	for _, a := range accounts {
		org, err := api.IsOrg(ctx, a)
		if err != nil {
			return fmt.Errorf("billing %s: %w", a, err)
		}
		items, err := api.BillingUsage(ctx, a, org, month.Year(), month.Month())
		if err != nil {
			var ae *gh.APIError
			if errors.As(err, &ae) && (ae.Status == 403 || ae.Status == 404) {
				need := "Plan: read (classic: user scope)"
				if org {
					need = "organization Administration: read (classic: admin:org)"
				}
				return fmt.Errorf("billing %s: %w (the token needs %s)", a, err, need)
			}
			return fmt.Errorf("billing %s: %w", a, err)
		}
		var public map[string]bool
		if p.IgnorePublicRepos {
			public = publicRepos(ctx, api, c, a, items)
		}
		gross += ActionsGross(items, public)
	}
	c.Billing = &Billing{Weighted: gross / p.PricePerMinute, FetchedAt: now}
	return nil
}

// ActionsGross sums the gross amount (before the free allowance) of
// Actions runner minutes in a billing report. Storage and other
// products are left out, and so are repositories in skip (lowercased
// names, with or without the owner).
func ActionsGross(items []gh.BillingItem, skip map[string]bool) float64 {
	var sum float64
	for _, it := range items {
		if !strings.EqualFold(it.Product, "actions") || !strings.EqualFold(it.UnitType, "minutes") {
			continue
		}
		if skip[strings.ToLower(it.RepositoryName)] {
			continue
		}
		sum += it.GrossAmount
	}
	return sum
}

// publicRepos returns which repositories named in the billing items are
// public, keyed like the items name them (lowercased). Visibility comes
// from the cache, or from the API for repositories outside the scan
// (e.g. with usage.repos). A repository whose visibility can't be read
// counts as private, the safe side.
func publicRepos(ctx context.Context, api BillingAPI, c *Cache, account string, items []gh.BillingItem) map[string]bool {
	out := map[string]bool{}
	checked := map[string]bool{}
	for _, it := range items {
		key := strings.ToLower(it.RepositoryName)
		if key == "" || checked[key] {
			continue
		}
		checked[key] = true
		full := key
		if !strings.Contains(full, "/") {
			full = strings.ToLower(account) + "/" + full
		}
		if rs, ok := cachedRepo(c, full); ok {
			out[key] = !rs.Private
			continue
		}
		if r, err := api.GetRepo(ctx, full); err == nil {
			out[key] = !r.Private
		}
	}
	return out
}

func cachedRepo(c *Cache, full string) (*RepoState, bool) {
	for name, rs := range c.Repos {
		if strings.EqualFold(name, full) {
			return rs, true
		}
	}
	return nil, false
}

func billingAccounts(cfg *config.Config) []string {
	seen := map[string]bool{}
	var out []string
	add := func(a string) {
		if k := strings.ToLower(a); a != "" && !seen[k] {
			seen[k] = true
			out = append(out, a)
		}
	}
	for _, r := range cfg.Usage.Repos {
		owner, _, _ := strings.Cut(r, "/")
		add(owner)
	}
	if len(cfg.Usage.Repos) == 0 {
		for _, a := range cfg.Usage.Accounts {
			add(a)
		}
		if len(out) == 0 {
			add(os.Getenv("GITHUB_REPOSITORY_OWNER"))
		}
	}
	return out
}

func attribute(cfg *config.Config, labels []string) (string, float64) {
	for i := range cfg.Providers {
		if m, ok := cfg.Providers[i].Match(labels); ok {
			return cfg.Providers[i].Name, m
		}
	}
	return Unattributed, 1
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// Weighted returns provider name -> weighted minutes, the form the
// picker uses.
func Weighted(t map[string]Total) map[string]float64 {
	out := make(map[string]float64, len(t))
	for k, v := range t {
		if !strings.HasPrefix(k, "(") {
			out[k] = v.Weighted
		}
	}
	return out
}
