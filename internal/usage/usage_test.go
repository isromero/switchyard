package usage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/isromero/switchyard/internal/config"
	"github.com/isromero/switchyard/internal/gh"
)

const testConfig = `
usage:
  accounts: [octo-user]
providers:
  - name: pi
    kind: self-hosted
    runners: [{label: my-pi, arch: arm64}]
  - name: jog
    free_minutes: 5000
    runners:
      - {label: jog}
      - {label: jog-4vcpu, size: medium, multiplier: 2}
  - name: blacksmith
    free_minutes: 3000
    runners: [{label: blacksmith-4vcpu-ubuntu-2404, size: medium, multiplier: 2}]
  - name: github
    free_minutes: 2000
    ignore_public_repos: true
    runners: [{label: ubuntu-latest}]
    count: [{pattern: "ubuntu-*"}]
`

// fixtureServer serves testdata files by API path and counts requests.
type fixtureServer struct {
	t    *testing.T
	mu   sync.Mutex
	hits map[string]int
	fail map[string]bool
}

var routes = map[string]string{
	"/users/octo-user":                            "users_octo-user.json",
	"/user":                                       "user.json",
	"/user/repos":                                 "user_repos.json",
	"/repos/octo-user/app/actions/runs":           "runs_app.json",
	"/repos/octo-user/site/actions/runs":          "runs_site.json",
	"/repos/octo-user/app/actions/runs/101/jobs":  "jobs_101.json",
	"/repos/octo-user/app/actions/runs/102/jobs":  "jobs_102.json",
	"/repos/octo-user/app/actions/runs/103/jobs":  "jobs_103.json",
	"/repos/octo-user/site/actions/runs/201/jobs": "jobs_201.json",
}

func (f *fixtureServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
		f.t.Errorf("%s: Authorization header %q", r.URL.Path, got)
	}
	f.mu.Lock()
	f.hits[r.URL.Path]++
	fail := f.fail[r.URL.Path]
	f.mu.Unlock()
	if fail {
		http.Error(w, `{"message":"Server Error"}`, http.StatusInternalServerError)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/actions/runs") && !strings.Contains(r.URL.Query().Get("created"), "..") {
		f.t.Errorf("runs listed without created range: %s", r.URL.RawQuery)
	}
	file, ok := routes[r.URL.Path]
	if !ok {
		f.t.Errorf("unexpected request %s", r.URL.Path)
		http.NotFound(w, r)
		return
	}
	b, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		f.t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func setup(t *testing.T) (*config.Config, *gh.Client, *fixtureServer) {
	t.Helper()
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	fs := &fixtureServer{t: t, hits: map[string]int{}, fail: map[string]bool{}}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	return cfg, gh.New(srv.URL, "test-token"), fs
}

var now = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

func TestRefreshAndTotals(t *testing.T) {
	cfg, client, _ := setup(t)
	c := NewCache(now)
	if err := Refresh(context.Background(), client, cfg, c, now); err != nil {
		t.Fatal(err)
	}
	got := Totals(cfg, c)
	want := map[string]Total{
		// 20s pick job rounds up to 1 minute.
		"pi": {Jobs: 1, Minutes: 1, Weighted: 1, Estimated: 1},
		// app: 10m30s on jog-4vcpu -> 11 min x2, skipped job 0 min;
		// site (public, but jog still bills): 2 min.
		// The skipped job never had a runner and is not counted.
		"jog": {Jobs: 2, Minutes: 13, Weighted: 24, Estimated: 24},
		// in-progress job counted up to now (5 min x2); queued job dropped.
		"blacksmith": {Jobs: 1, Minutes: 5, Weighted: 10, Estimated: 10},
		// attempt 1 ran last month and is excluded; attempt 2: 3 min
		// ubuntu-latest + 1m01s ubuntu-22.04 (count rule) -> 2 min.
		// The public site repo's 4 minutes are ignored.
		"github": {Jobs: 2, Minutes: 5, Weighted: 5, Estimated: 5},
	}
	if len(got) != len(want) {
		t.Errorf("got providers %v", got)
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: got %+v, want %+v", k, got[k], w)
		}
	}
	if !c.Complete || !c.UpdatedAt.Equal(now) {
		t.Errorf("cache not marked complete: %+v", c)
	}
	if _, ok := c.Repos["octo-org/x"]; ok {
		t.Error("repo owned by another account was scanned")
	}
	if w := Weighted(got); w["jog"] != 24 || len(w) != 4 {
		t.Errorf("Weighted: %v", w)
	}
}

func TestRefreshIsIncremental(t *testing.T) {
	cfg, client, fs := setup(t)
	file := filepath.Join(t.TempDir(), "sub", "usage.json")

	c := LoadCache(file, "", now)
	if err := Refresh(context.Background(), client, cfg, c, now); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(file); err != nil {
		t.Fatal(err)
	}

	later := now.Add(3 * time.Minute)
	c2 := LoadCache(file, "", later)
	if !c2.Complete || len(c2.Repos) != 2 {
		t.Fatalf("cache not reloaded: %+v", c2)
	}
	if !c2.Fresh(later, 10*time.Minute) || c2.Fresh(later, time.Minute) {
		t.Error("Fresh() wrong")
	}
	if err := Refresh(context.Background(), client, cfg, c2, later); err != nil {
		t.Fatal(err)
	}
	// Completed runs are not fetched again; the in-progress one is.
	for path, want := range map[string]int{
		"/repos/octo-user/app/actions/runs/101/jobs":  1,
		"/repos/octo-user/app/actions/runs/102/jobs":  2,
		"/repos/octo-user/app/actions/runs/103/jobs":  1,
		"/repos/octo-user/site/actions/runs/201/jobs": 1,
		"/repos/octo-user/app/actions/runs":           2,
	} {
		if fs.hits[path] != want {
			t.Errorf("%s fetched %d times, want %d", path, fs.hits[path], want)
		}
	}
	if got := Totals(cfg, c2)["blacksmith"]; got.Minutes != 8 {
		t.Errorf("running job should now be 8 min, got %+v", got)
	}
}

func TestRefreshPartialFailureKeepsOldState(t *testing.T) {
	cfg, client, fs := setup(t)
	c := NewCache(now)
	if err := Refresh(context.Background(), client, cfg, c, now); err != nil {
		t.Fatal(err)
	}
	fs.fail["/repos/octo-user/site/actions/runs"] = true
	later := now.Add(time.Minute)
	err := Refresh(context.Background(), client, cfg, c, later)
	if err == nil || !strings.Contains(err.Error(), "octo-user/site") {
		t.Fatalf("want error naming the failed repo, got %v", err)
	}
	if c.Complete || !c.UpdatedAt.Equal(now) {
		t.Errorf("failed refresh must not look complete: %+v", c)
	}
	if got := Totals(cfg, c)["jog"]; got.Weighted != 24 {
		t.Errorf("old state for failed repo lost: %+v", got)
	}
}

func TestLoadCacheDiscardsOtherMonth(t *testing.T) {
	file := filepath.Join(t.TempDir(), "usage.json")
	old := NewCache(now.AddDate(0, -1, 0))
	old.Complete = true
	old.Repos["a/b"] = &RepoState{}
	if err := old.Save(file); err != nil {
		t.Fatal(err)
	}
	c := LoadCache(file, "", now)
	if c.Month != "2026-11" || len(c.Repos) != 0 || c.Complete {
		t.Errorf("stale month kept: %+v", c)
	}
	os.WriteFile(file, []byte("not json"), 0o644)
	if c := LoadCache(file, "", now); c.Month != "2026-11" {
		t.Error("corrupt cache not replaced")
	}
}

func TestJobInfoRounding(t *testing.T) {
	ts := func(s string) *time.Time { v, _ := time.Parse(time.RFC3339, s); return &v }
	tests := []struct {
		name  string
		job   gh.Job
		mins  int
		valid bool
	}{
		{"never started", gh.Job{}, 0, false},
		{"one second", gh.Job{Status: "completed", RunnerName: "r", StartedAt: ts("2026-11-01T00:00:00Z"), CompletedAt: ts("2026-11-01T00:00:01Z")}, 1, true},
		{"exact minute", gh.Job{Status: "completed", RunnerName: "r", StartedAt: ts("2026-11-01T00:00:00Z"), CompletedAt: ts("2026-11-01T00:02:00Z")}, 2, true},
		{"zero", gh.Job{Status: "completed", RunnerName: "r", StartedAt: ts("2026-11-01T00:00:00Z"), CompletedAt: ts("2026-11-01T00:00:00Z")}, 0, true},
		{"running", gh.Job{Status: "in_progress", RunnerName: "r", StartedAt: ts("2026-11-12T11:58:30Z")}, 2, true},
		// GitHub fills started_at on queued jobs and on jobs that ended
		// without a runner (e.g. blocked by billing).
		{"queued with started_at", gh.Job{Status: "queued", StartedAt: ts("2026-11-12T10:00:00Z")}, 0, false},
		{"completed without a runner", gh.Job{Status: "completed", StartedAt: ts("2026-11-12T10:00:00Z"), CompletedAt: ts("2026-11-12T11:00:00Z")}, 0, false},
	}
	for _, tt := range tests {
		ji, ok := jobInfo(tt.job, now)
		if ok != tt.valid || ji.Minutes != tt.mins {
			t.Errorf("%s: got %d,%v want %d,%v", tt.name, ji.Minutes, ok, tt.mins, tt.valid)
		}
	}
}

func TestExplicitRepos(t *testing.T) {
	cfg, err := config.Parse([]byte(`
usage: {repos: [octo-user/app]}
providers: [{name: jog, free_minutes: 1, runners: [{label: jog-4vcpu, multiplier: 2}]}]
`))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/octo-user/app":
			w.Write([]byte(`{"full_name":"octo-user/app","private":true}`))
		case "/repos/octo-user/app/actions/runs":
			w.Write([]byte(`{"total_count":1,"workflow_runs":[{"id":101,"run_attempt":1,"status":"completed"}]}`))
		case "/repos/octo-user/app/actions/runs/101/jobs":
			b, _ := os.ReadFile("testdata/jobs_101.json")
			w.Write(b)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewCache(now)
	if err := Refresh(context.Background(), gh.New(srv.URL, ""), cfg, c, now); err != nil {
		t.Fatal(err)
	}
	if got := Totals(cfg, c); got["jog"].Weighted != 22 || got[Unattributed].Jobs != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestRefreshKeepsPartialProgress(t *testing.T) {
	cfg, client, fs := setup(t)
	c := NewCache(now)
	fs.fail["/repos/octo-user/app/actions/runs/102/jobs"] = true
	if err := Refresh(context.Background(), client, cfg, c, now); err == nil {
		t.Fatal("want error")
	}
	if c.Complete || !c.UpdatedAt.IsZero() {
		t.Fatalf("partial refresh looks complete: %+v", c)
	}
	if n := len(c.Repos["octo-user/app"].Runs); n != 2 {
		t.Fatalf("want the 2 fetched runs kept, got %d", n)
	}
	delete(fs.fail, "/repos/octo-user/app/actions/runs/102/jobs")
	if err := Refresh(context.Background(), client, cfg, c, now); err != nil {
		t.Fatal(err)
	}
	if fs.hits["/repos/octo-user/app/actions/runs/101/jobs"] != 1 {
		t.Error("completed run fetched twice")
	}
	if !c.Complete || Totals(cfg, c)["blacksmith"].Weighted != 10 {
		t.Errorf("second refresh incomplete: %+v", Totals(cfg, c))
	}
}

func TestIdleReposAreNotListedEveryTime(t *testing.T) {
	cfg, client, fs := setup(t)
	c := NewCache(now)
	if err := Refresh(context.Background(), client, cfg, c, now); err != nil {
		t.Fatal(err)
	}
	// Pretend site had no runs at the last check.
	c.Repos["octo-user/site"].Runs = map[int64]*RunState{}
	for i, tt := range []struct {
		at   time.Time
		want int // total listings of site runs so far
	}{
		{now.Add(30 * time.Minute), 1}, // idle, recently checked, no push since: skipped
		{now.Add(61 * time.Minute), 2}, // recheck interval passed
	} {
		if err := Refresh(context.Background(), client, cfg, c, tt.at); err != nil {
			t.Fatal(err)
		}
		if got := fs.hits["/repos/octo-user/site/actions/runs"]; got != tt.want {
			t.Errorf("step %d: site runs listed %d times, want %d", i, got, tt.want)
		}
	}
}

type fakeBilling struct {
	repos map[string]gh.Repo
	org   map[string]bool
	items map[string][]gh.BillingItem
	err   error
	asked []string
}

func (f *fakeBilling) IsOrg(_ context.Context, a string) (bool, error) { return f.org[a], f.err }
func (f *fakeBilling) GetRepo(_ context.Context, full string) (gh.Repo, error) {
	r, ok := f.repos[full]
	if !ok {
		return gh.Repo{}, errors.New("not found")
	}
	return r, nil
}
func (f *fakeBilling) BillingUsage(_ context.Context, a string, org bool, y int, m time.Month) ([]gh.BillingItem, error) {
	f.asked = append(f.asked, fmt.Sprintf("%s org=%v %d-%02d", a, org, y, m))
	return f.items[a], f.err
}

func TestBilling(t *testing.T) {
	cfg, err := config.Parse([]byte(`
usage: {accounts: [octo-user, octo-org]}
providers:
  - name: github
    free_minutes: 2000
    price_per_minute: 0.006
    ignore_public_repos: true
    billing_api: true
    runners: [{label: ubuntu-latest}]
`))
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeBilling{
		org: map[string]bool{"octo-org": true},
		items: map[string][]gh.BillingItem{
			"octo-user": {
				{Product: "actions", SKU: "actions_linux", UnitType: "Minutes", GrossAmount: 1.2, RepositoryName: "octo-user/app"},
				{Product: "actions", SKU: "actions_linux", UnitType: "Minutes", GrossAmount: 9, RepositoryName: "octo-user/site"}, // public
				{Product: "actions", SKU: "actions_storage", UnitType: "GigabyteHours", GrossAmount: 50},
				{Product: "copilot", UnitType: "Minutes", GrossAmount: 50},
			},
			"octo-org": {{Product: "Actions", UnitType: "minutes", GrossAmount: 0.6, RepositoryName: "x"}},
		},
	}
	c := NewCache(now)
	c.Repos["octo-user/app"] = &RepoState{Private: true, Runs: map[int64]*RunState{
		1: {Completed: true, Jobs: []JobInfo{{Labels: []string{"ubuntu-latest"}, Started: now.Add(-time.Hour), Minutes: 500}}},
	}}
	c.Repos["octo-user/site"] = &RepoState{Private: false, Runs: map[int64]*RunState{}}

	if err := RefreshBilling(context.Background(), api, cfg, c, now); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(api.asked, ", "); got != fmt.Sprintf("octo-user org=false %d-%02d, octo-org org=true %d-%02d", now.Year(), now.Month(), now.Year(), now.Month()) {
		t.Errorf("asked %s", got)
	}
	// (1.2 + 0.6) / 0.006 = 300 billed. The 500 estimated minutes started
	// an hour ago, within BillingLag, so they are added: 800.
	tot := Totals(cfg, c)["github"]
	if !tot.HasBilled || math.Abs(tot.Billed-300) > 1e-9 || math.Abs(tot.Weighted-800) > 1e-9 || tot.Estimated != 500 {
		t.Fatalf("got %+v", tot)
	}
	// Older jobs are taken from the report alone, even when the estimate
	// is higher (e.g. runs billed to the account a repository came from).
	c.Repos["octo-user/app"].Runs[1].Jobs[0].Started = now.Add(-BillingLag - time.Minute)
	if tot := Totals(cfg, c)["github"]; math.Abs(tot.Weighted-300) > 1e-9 || tot.Estimated != 500 {
		t.Fatalf("got %+v", tot)
	}
	c.Repos["octo-user/app"].Runs[1].Jobs[0].Started = now.Add(-time.Hour)
	c.Billing.Weighted = 900
	if tot := Totals(cfg, c)["github"]; math.Abs(tot.Weighted-1400) > 1e-9 {
		t.Fatalf("got %+v", tot)
	}

	// An error keeps the previous figure.
	api.err = errors.New("403 needs Plan: read")
	if err := RefreshBilling(context.Background(), api, cfg, c, now); err == nil || c.Billing.Weighted != 900 {
		t.Fatalf("err %v billing %+v", err, c.Billing)
	}

	// No provider with billing_api: nothing is asked.
	cfg.Providers[0].BillingAPI = false
	api.asked = nil
	if err := RefreshBilling(context.Background(), api, cfg, c, now); err != nil || len(api.asked) != 0 {
		t.Fatalf("err %v asked %v", err, api.asked)
	}
}

func TestLoadCacheChecksScope(t *testing.T) {
	file := filepath.Join(t.TempDir(), "c.json")
	c := NewCache(now)
	c.Scope = "account:a"
	c.Complete, c.UpdatedAt = true, now
	if err := c.Save(file); err != nil {
		t.Fatal(err)
	}
	if got := LoadCache(file, "account:a", now); !got.Complete {
		t.Error("same scope: cache not reused")
	}
	if got := LoadCache(file, "account:b", now); got.Complete || got.Scope != "account:b" {
		t.Errorf("other scope reused: %+v", got)
	}
	if DefaultCacheFile("account:a") == DefaultCacheFile("account:b") {
		t.Error("scopes share a default cache file")
	}
}

func TestFailedJobFetchIsRetried(t *testing.T) {
	cfg, client, fs := setup(t)
	c := NewCache(now)
	// Runs are listed, but every job fetch of the site repo fails.
	fs.fail["/repos/octo-user/site/actions/runs/201/jobs"] = true
	if err := Refresh(context.Background(), client, cfg, c, now); err == nil {
		t.Fatal("expected an error")
	}
	// Recovered a minute later: the repo must be fetched again, not
	// skipped as idle, and its jobs counted.
	fs.fail = map[string]bool{}
	if err := Refresh(context.Background(), client, cfg, c, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if fs.hits["/repos/octo-user/site/actions/runs/201/jobs"] != 2 {
		t.Errorf("jobs of run 201 fetched %d times", fs.hits["/repos/octo-user/site/actions/runs/201/jobs"])
	}
	if got := Totals(cfg, c)["jog"]; got.Jobs != 2 {
		t.Errorf("jog after recovery: %+v", got)
	}
}

func TestBillingPublicRepoOutsideScan(t *testing.T) {
	cfg, err := config.Parse([]byte(`
usage: {repos: [octo-user/app]}
providers:
  - {name: github, free_minutes: 2000, price_per_minute: 0.006, ignore_public_repos: true, billing_api: true, runners: [{label: ubuntu-latest}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeBilling{
		repos: map[string]gh.Repo{"octo-user/site": {FullName: "octo-user/site", Private: false}},
		items: map[string][]gh.BillingItem{"octo-user": {
			{Product: "actions", UnitType: "Minutes", GrossAmount: 18, RepositoryName: "site"},      // public, not scanned
			{Product: "actions", UnitType: "Minutes", GrossAmount: 0.6, RepositoryName: "app"},      // private, scanned
			{Product: "actions", UnitType: "Minutes", GrossAmount: 0.6, RepositoryName: "vanished"}, // unknown: counted
		}},
	}
	c := NewCache(now)
	c.Repos["octo-user/app"] = &RepoState{Private: true, Runs: map[int64]*RunState{}}
	if err := RefreshBilling(context.Background(), api, cfg, c, now); err != nil {
		t.Fatal(err)
	}
	if math.Abs(c.Billing.Weighted-200) > 1e-9 {
		t.Errorf("billed %v, want 200", c.Billing.Weighted)
	}
}

func TestStuck(t *testing.T) {
	cfg, err := config.Parse([]byte(`
providers:
  - {name: vm, kind: self-hosted, runners: [{label: my-vm}]}
  - {name: blacksmith, free_minutes: 3000, runners: [{label: blacksmith-2vcpu}]}
  - {name: github, free_minutes: 2000, runners: [{label: ubuntu-latest}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	ts := func(d time.Duration) time.Time { return now.Add(-d) }
	c := NewCache(now)
	c.Repos["o/app"] = &RepoState{Private: true, Runs: map[int64]*RunState{
		1: {Waits: []Wait{
			{Labels: []string{"blacksmith-2vcpu"}, Since: ts(20 * time.Minute)}, // still queued
			{Labels: []string{"my-vm"}, Since: ts(3 * time.Hour)},               // self-hosted: ignored
			{Labels: []string{"ubuntu-latest"}, Since: ts(4 * time.Minute)},     // too short
		}},
	}}
	got := Stuck(cfg, c, now, 10*time.Minute)
	if len(got) != 1 || !strings.Contains(got["blacksmith"], "has waited 20 min") {
		t.Fatalf("got %v", got)
	}
	if Stuck(cfg, c, now, 0) != nil {
		t.Error("0 should turn the check off")
	}

	// A job that ran on the provider after the wait began: not stuck
	// (e.g. it was only busy).
	c.Repos["o/app"].Runs[2] = &RunState{Jobs: []JobInfo{{Labels: []string{"blacksmith-2vcpu"}, Started: ts(5 * time.Minute), Minutes: 1}}}
	if got := Stuck(cfg, c, now, 10*time.Minute); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	delete(c.Repos["o/app"].Runs, 2)

	// A wait that ended (cancelled) counts for StuckCooldown, then expires.
	c.Repos["o/app"].Runs[1].Waits[0].Until = ts(10 * time.Minute)
	if got := Stuck(cfg, c, now, 10*time.Minute); !strings.Contains(got["blacksmith"], "ended without a runner") {
		t.Fatalf("got %v", got)
	}
	c.Repos["o/app"].Runs[1].Waits[0].Until = ts(StuckCooldown + time.Minute)
	if got := Stuck(cfg, c, now, 5*time.Minute); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestWaitInfo(t *testing.T) {
	q, done := ts(now, 30*time.Minute), ts(now, 10*time.Minute)
	cases := []struct {
		j    gh.Job
		ok   bool
		open bool
	}{
		{gh.Job{Status: "queued", StartedAt: &q}, true, true},
		{gh.Job{Status: "completed", StartedAt: &q, CompletedAt: &done}, true, false},
		{gh.Job{Status: "completed", StartedAt: &q, CompletedAt: &done, RunnerName: "r1"}, false, false},
		{gh.Job{Status: "waiting", StartedAt: &q}, false, false}, // environment approval
		{gh.Job{Status: "queued"}, false, false},
	}
	for i, c := range cases {
		w, ok := waitInfo(c.j)
		if ok != c.ok || (ok && w.Until.IsZero() != c.open) {
			t.Errorf("case %d: got %+v %v", i, w, ok)
		}
	}
}

func ts(t time.Time, d time.Duration) time.Time { return t.Add(-d) }
