package pick

import (
	"errors"
	"strings"
	"testing"

	"github.com/isromero/switchyard/internal/config"
	"github.com/isromero/switchyard/internal/gh"
)

type fakeState struct {
	stuck      map[string]string
	usage      map[string]float64
	usageErr   error
	runners    []gh.Runner
	runnersErr error
	queued     [][]string
	queuedErr  error
	usageCalls int
}

func (f *fakeState) Usage() (map[string]float64, error) { f.usageCalls++; return f.usage, f.usageErr }
func (f *fakeState) Runners() ([]gh.Runner, error)      { return f.runners, f.runnersErr }
func (f *fakeState) Queued() ([][]string, error)        { return f.queued, f.queuedErr }
func (f *fakeState) Stuck() map[string]string           { return f.stuck }

func runner(name, status string, busy bool, labels ...string) gh.Runner {
	r := gh.Runner{Name: name, Status: status, Busy: busy}
	for _, l := range labels {
		r.Labels = append(r.Labels, gh.Label{Name: l})
	}
	return r
}

const testConfig = `
fallback: ubuntu-latest
margin_minutes: 50
providers:
  - name: pi
    kind: self-hosted
    runners: [{label: my-pi, arch: arm64}]
  - name: jog
    free_minutes: 5000
    price_per_minute: 0.0012
    runners:
      - {label: jog, size: small}
      - {label: jog-4vcpu, size: medium, multiplier: 2}
  - name: blacksmith
    free_minutes: 3000
    margin_minutes: 0
    price_per_minute: 0.004
    runners:
      - {label: blacksmith-2vcpu-ubuntu-2404}
      - {label: blacksmith-2vcpu-ubuntu-2404-arm, arch: arm64}
  - name: depot
    allow_paid: true
    price_per_minute: 0.006
    cap_usd: 10
    runners: [{label: depot-ubuntu-24.04}]
  - name: runs-on
    allow_paid: true
    cap_minutes: 1000
    runners: [{label: "runs-on={{run_id}}/runner=2cpu-linux-x64"}]
  - name: github
    free_minutes: 2000
    allow_paid: true
    price_per_minute: 0.006
    cap_usd: 5
    runners: [{label: ubuntu-latest}]
`

func TestChoose(t *testing.T) {
	cfg, err := config.Parse([]byte(testConfig))
	if err != nil {
		t.Fatal(err)
	}
	idlePi := []gh.Runner{runner("pi-1", "online", false, "self-hosted", "my-pi")}
	busyPi := []gh.Runner{runner("pi-1", "online", true, "self-hosted", "my-pi")}
	exhausted := map[string]float64{"jog": 5000, "blacksmith": 3000}

	tests := []struct {
		name      string
		req       Request
		st        *fakeState
		paidOrder []string
		want      string
		paid      bool
		fallback  bool
		note      string
	}{
		{name: "idle self-hosted first", req: Request{Size: "small"}, st: &fakeState{runners: idlePi}, want: "my-pi"},
		{name: "self-hosted not needed for usage", req: Request{Size: "small"}, st: &fakeState{runners: idlePi, usageErr: errors.New("boom")}, want: "my-pi"},
		{name: "busy self-hosted skipped", req: Request{Size: "small"}, st: &fakeState{runners: busyPi, usage: map[string]float64{}}, want: "jog", note: "no idle online runner"},
		{name: "busy with the pick job itself counts as idle", req: Request{Size: "small", CurrentRunner: "PI-1"}, st: &fakeState{runners: busyPi}, want: "my-pi"},
		{name: "idle runner already claimed by a queued job", req: Request{Size: "small"},
			st: &fakeState{runners: idlePi, queued: [][]string{{"self-hosted", "MY-PI"}}, usage: map[string]float64{}}, want: "jog", note: "already queued"},
		{name: "two idle runners, one queued job", req: Request{Size: "small"},
			st: &fakeState{runners: append([]gh.Runner{runner("pi-2", "online", false, "my-pi")}, idlePi...), queued: [][]string{{"my-pi"}}}, want: "my-pi"},
		{name: "queued jobs for other labels don't count", req: Request{Size: "small"},
			st: &fakeState{runners: idlePi, queued: [][]string{{"ubuntu-latest"}}}, want: "my-pi"},
		{name: "queue check error trusts the runner list", req: Request{Size: "small"},
			st: &fakeState{runners: idlePi, queuedErr: errors.New("timeout")}, want: "my-pi"},
		{name: "offline self-hosted skipped", req: Request{Size: "small"}, st: &fakeState{runners: []gh.Runner{runner("pi-1", "offline", false, "my-pi")}, usage: map[string]float64{}}, want: "jog"},
		{name: "runner list error skips self-hosted", req: Request{Size: "small"}, st: &fakeState{runnersErr: errors.New("403"), usage: map[string]float64{}}, want: "jog", note: "cannot list runners"},
		{name: "arch filter", req: Request{Size: "small", Arch: "x64"}, st: &fakeState{runners: idlePi, usage: map[string]float64{}}, want: "jog"},
		{name: "arch arm64 skips x64-only providers", req: Request{Size: "small", Arch: "arm64"}, st: &fakeState{runners: busyPi, usage: map[string]float64{}}, want: "blacksmith-2vcpu-ubuntu-2404-arm"},
		{name: "size medium", req: Request{Size: "medium"}, st: &fakeState{usage: map[string]float64{}}, want: "jog-4vcpu"},
		{name: "margin stops jog early", req: Request{Size: "small"}, st: &fakeState{usage: map[string]float64{"jog": 4950}}, want: "blacksmith-2vcpu-ubuntu-2404", note: "free minutes used"},
		{name: "just inside margin", req: Request{Size: "small"}, st: &fakeState{usage: map[string]float64{"jog": 4949}}, want: "jog"},
		{name: "provider margin override of 0", req: Request{Size: "small"}, st: &fakeState{usage: map[string]float64{"jog": 5000, "blacksmith": 2999}}, want: "blacksmith-2vcpu-ubuntu-2404"},
		{name: "github free minutes before paid", req: Request{Size: "small"}, st: &fakeState{usage: exhausted}, want: "ubuntu-latest"},
		{name: "paid pass in config order", req: Request{Size: "small"}, st: &fakeState{usage: map[string]float64{"jog": 5000, "blacksmith": 3000, "github": 2000}}, want: "depot-ubuntu-24.04", paid: true},
		{name: "paid_order overrides", req: Request{Size: "small", RunID: "99"}, paidOrder: []string{"runs-on", "depot"},
			st: &fakeState{usage: map[string]float64{"jog": 5000, "blacksmith": 3000, "github": 2000}}, want: "runs-on=99/runner=2cpu-linux-x64", paid: true},
		{name: "usd cap reached", req: Request{Size: "small", RunID: "7"},
			st: &fakeState{usage: map[string]float64{"jog": 5000, "blacksmith": 3000, "github": 2000, "depot": 1617}}, want: "runs-on=7/runner=2cpu-linux-x64", paid: true, note: "at spend cap"},
		{name: "minute cap reached, github paid", req: Request{Size: "small"},
			st: &fakeState{usage: map[string]float64{"jog": 5000, "blacksmith": 3000, "github": 2000, "depot": 2000, "runs-on": 950}}, want: "ubuntu-latest", paid: true, note: "at minute cap"},
		{name: "everything at cap falls back", req: Request{Size: "small"},
			st: &fakeState{usage: map[string]float64{"jog": 5000, "blacksmith": 3000, "github": 3000, "depot": 2000, "runs-on": 1000}}, want: "ubuntu-latest", paid: true, fallback: true},
		{name: "usage unknown falls back", req: Request{Size: "small"}, st: &fakeState{usageErr: errors.New("api down")}, want: "ubuntu-latest", fallback: true, note: "usage unknown"},
		{name: "disabled provider skipped", req: Request{Size: "small", Disabled: map[string]bool{"jog": true}}, st: &fakeState{usage: map[string]float64{}}, want: "blacksmith-2vcpu-ubuntu-2404", note: "jog: disabled"},
		{name: "size nobody has falls back", req: Request{Size: "large"}, st: &fakeState{usage: map[string]float64{}}, want: "ubuntu-latest", fallback: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := *cfg
			c.PaidOrder = tt.paidOrder
			d := Choose(&c, tt.req, tt.st)
			if d.Label != tt.want || d.Paid != tt.paid || d.Fallback != tt.fallback {
				t.Fatalf("got %q paid=%v fallback=%v, want %q paid=%v fallback=%v\nnotes: %v",
					d.Label, d.Paid, d.Fallback, tt.want, tt.paid, tt.fallback, d.Notes)
			}
			if tt.note != "" && !strings.Contains(strings.Join(d.Notes, "\n"), tt.note) {
				t.Errorf("notes %v do not mention %q", d.Notes, tt.note)
			}
		})
	}
}

const exhaustConfig = `
fallback: ubuntu-latest
on_exhausted: fail
providers:
  - {name: jog, free_minutes: 100, runners: [{label: jog}]}
  - name: github
    free_minutes: 2000
    price_per_minute: 0.006
    ignore_public_repos: true
    runners: [{label: ubuntu-latest}]
`

func TestOnExhausted(t *testing.T) {
	cfg, err := config.Parse([]byte(exhaustConfig))
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]float64{"jog": 100, "github": 2000}
	tests := []struct {
		name            string
		cfgFallback     string
		onExhausted     string
		req             Request
		st              *fakeState
		want            string
		stop, paid, fbk bool
	}{
		{name: "known exhausted stops", req: Request{Size: "small"}, st: &fakeState{usage: used}, want: "ubuntu-latest", stop: true, paid: true, fbk: true},
		{name: "fallback mode keeps going but says paid", onExhausted: "fallback", req: Request{Size: "small"}, st: &fakeState{usage: used}, want: "ubuntu-latest", paid: true, fbk: true},
		{name: "usage unknown never stops", req: Request{Size: "small"}, st: &fakeState{usageErr: errors.New("down")}, want: "ubuntu-latest", fbk: true},
		{name: "public repo: github is free", req: Request{Size: "small", PublicRepo: true}, st: &fakeState{usage: used}, want: "ubuntu-latest"},
		{name: "public repo still prefers earlier free providers", req: Request{Size: "small", PublicRepo: true}, st: &fakeState{usage: map[string]float64{}}, want: "jog"},
		{name: "fallback outside the config: unknown cost, stops", cfgFallback: "my-label", req: Request{Size: "small"}, st: &fakeState{usage: used}, want: "my-label", stop: true, fbk: true},
		{name: "fallback provider with minutes left is free: no stop", req: Request{Size: "small", Disabled: map[string]bool{"github": true}}, st: &fakeState{usage: map[string]float64{"jog": 100}}, want: "ubuntu-latest", fbk: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := *cfg
			if tt.cfgFallback != "" {
				c.Fallback = tt.cfgFallback
			}
			if tt.onExhausted != "" {
				c.OnExhausted = tt.onExhausted
			}
			d := Choose(&c, tt.req, tt.st)
			if d.Label != tt.want || d.Stop != tt.stop || d.Paid != tt.paid || d.Fallback != tt.fbk {
				t.Fatalf("got %q stop=%v paid=%v fallback=%v, want %q stop=%v paid=%v fallback=%v\nnotes: %v",
					d.Label, d.Stop, d.Paid, d.Fallback, tt.want, tt.stop, tt.paid, tt.fbk, d.Notes)
			}
		})
	}
}

func TestFreePassAndPaidFlag(t *testing.T) {
	cfg, err := config.Parse([]byte(`
margin_minutes: 60
providers:
  - {name: capped, free_minutes: 5000, cap_minutes: 1000, runners: [{label: capped}]}
  - {name: gh, free_minutes: 2000, allow_paid: true, cap_minutes: 9000, runners: [{label: ubuntu-latest}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	// cap_minutes applies to free minutes too.
	d := Choose(cfg, Request{Size: "small"}, &fakeState{usage: map[string]float64{"capped": 1500, "gh": 1950}})
	if d.Label != "ubuntu-latest" || !strings.Contains(strings.Join(d.Notes, " "), "capped: at minute cap") {
		t.Fatalf("got %+v", d)
	}
	// 1950 of 2000 used: inside the margin, so the paid pass picks it,
	// but it is not billed yet.
	if d.Paid {
		t.Errorf("paid inside the margin: %+v", d)
	}
	d = Choose(cfg, Request{Size: "small"}, &fakeState{usage: map[string]float64{"capped": 1500, "gh": 2000}})
	if !d.Paid {
		t.Errorf("not paid after the allowance: %+v", d)
	}
}

func TestFallbackCost(t *testing.T) {
	base := `
on_exhausted: fail
fallback: %s
providers:
  - {name: pi, kind: self-hosted, runners: [{label: my-pi}]}
  - {name: gh, free_minutes: 2000, price_per_minute: 0.006, runners: [{label: ubuntu-latest}, {label: big, size: large}]}
`
	tests := []struct {
		name, fallback string
		req            Request
		st             *fakeState
		stop           bool
	}{
		{"self-hosted fallback is free", "my-pi", Request{Size: "small"}, &fakeState{runners: nil, usage: map[string]float64{"gh": 5000}}, false},
		{"usage error while classifying the fallback falls back", "ubuntu-latest", Request{Size: "medium"}, &fakeState{usageErr: errors.New("down")}, false},
		{"no provider for the size, fallback paid", "ubuntu-latest", Request{Size: "medium"}, &fakeState{usage: map[string]float64{"gh": 5000}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := config.Parse([]byte(strings.Replace(base, "%s", tt.fallback, 1)))
			if err != nil {
				t.Fatal(err)
			}
			if d := Choose(cfg, tt.req, tt.st); d.Stop != tt.stop || !d.Fallback {
				t.Fatalf("got %+v, want stop=%v", d, tt.stop)
			}
		})
	}
}

func TestChooseDoesNotFetchUsageWhenSelfHostedIdle(t *testing.T) {
	cfg, _ := config.Parse([]byte(testConfig))
	st := &fakeState{runners: []gh.Runner{runner("pi", "online", false, "my-pi")}}
	Choose(cfg, Request{Size: "small"}, st)
	if st.usageCalls != 0 {
		t.Errorf("usage fetched %d times", st.usageCalls)
	}
}

func TestDisabledInConfig(t *testing.T) {
	cfg, err := config.Parse([]byte(`
providers:
  - {name: a, enabled: false, free_minutes: 100, runners: [{label: a}]}
  - {name: b, free_minutes: 100, runners: [{label: b}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if d := Choose(cfg, Request{Size: "small"}, &fakeState{usage: map[string]float64{}}); d.Label != "b" {
		t.Errorf("got %s", d.Label)
	}
}

func TestCost(t *testing.T) {
	p := &config.Provider{FreeMinutes: 100, PricePerMinute: 0.01}
	for _, tt := range []struct{ used, want float64 }{{0, 0}, {100, 0}, {150, 0.5}} {
		if got := Cost(p, tt.used); got != tt.want {
			t.Errorf("Cost(%v) = %v, want %v", tt.used, got, tt.want)
		}
	}
}

func TestForkPRNeverUsesSelfHosted(t *testing.T) {
	cfg, err := config.Parse([]byte(`
fallback: my-pi
providers:
  - {name: pi, kind: self-hosted, runners: [{label: my-pi}]}
  - {name: gh, free_minutes: 2000, runners: [{label: ubuntu-latest}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	idle := []gh.Runner{runner("pi-1", "online", false, "my-pi")}

	d := Choose(cfg, Request{Size: "small", ForkPR: true}, &fakeState{runners: idle, usage: map[string]float64{}})
	if d.Label != "ubuntu-latest" || !strings.Contains(strings.Join(d.Notes, " "), "pi: not used for pull requests from forks") {
		t.Fatalf("idle self-hosted used for a fork: %+v", d)
	}
	// Hosted used up: the self-hosted fallback must not be used either,
	// whatever on_exhausted says.
	d = Choose(cfg, Request{Size: "small", ForkPR: true}, &fakeState{runners: idle, usage: map[string]float64{"gh": 2000}})
	if !d.Stop || !strings.Contains(d.StopReason, "fork") {
		t.Fatalf("self-hosted fallback for a fork: %+v", d)
	}
	// Same run from a branch of the repository: self-hosted is fine.
	if d := Choose(cfg, Request{Size: "small"}, &fakeState{runners: idle}); d.Label != "my-pi" {
		t.Fatalf("got %+v", d)
	}
}

func TestStuckProviderSkipped(t *testing.T) {
	cfg, err := config.Parse([]byte(`
providers:
  - {name: blacksmith, free_minutes: 3000, runners: [{label: blacksmith-2vcpu}]}
  - {name: github, free_minutes: 2000, runners: [{label: ubuntu-latest}]}
fallback: ubuntu-latest
on_exhausted: fail
`))
	if err != nil {
		t.Fatal(err)
	}
	st := &fakeState{usage: map[string]float64{}, stuck: map[string]string{"blacksmith": "looks stuck"}}
	d := Choose(cfg, Request{Size: "small"}, st)
	if d.Label != "ubuntu-latest" || d.Provider != "github" {
		t.Fatalf("got %+v", d)
	}
	// Everything stuck: the fallback, and not a stop (it's not exhaustion).
	st.stuck["github"] = "looks stuck"
	d = Choose(cfg, Request{Size: "small"}, st)
	if !d.Fallback || d.Stop {
		t.Fatalf("got %+v", d)
	}
}
