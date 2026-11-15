package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestRepoConfigIsValid(t *testing.T) {
	b, err := os.ReadFile("../../switchyard.yml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(b)
	if err != nil {
		t.Fatalf("switchyard.yml: %v", err)
	}
	if c.Provider("jog") == nil || c.Provider("pi").Kind != KindSelfHosted {
		t.Fatal("expected jog and self-hosted pi providers")
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(`
providers:
  - name: gh
    free_minutes: 100
    runners: [{label: ubuntu-latest}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Fallback != DefaultFallback || c.MarginMinutes != DefaultMarginMinutes {
		t.Errorf("fallback/margin defaults: %q %v", c.Fallback, c.MarginMinutes)
	}
	if time.Duration(c.Usage.CacheTTL) != DefaultCacheTTL || time.Duration(c.Usage.Timeout) != DefaultTimeout {
		t.Errorf("duration defaults: %v %v", c.Usage.CacheTTL, c.Usage.Timeout)
	}
	p := c.Providers[0]
	r := p.Runners[0]
	if p.Kind != KindHosted || r.Size != "small" || r.Arch != "x64" || r.Multiplier != 1 || !p.IsEnabled() {
		t.Errorf("provider defaults: %+v %+v", p, r)
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string // substring of the error
	}{
		{"no providers", `fallback: x`, "no providers"},
		{"unknown field", "providers: []\nbogus: 1", "bogus"},
		{"missing name", `providers: [{runners: [{label: a}], free_minutes: 1}]`, "name is required"},
		{"duplicate name", `providers: [{name: a, free_minutes: 1, runners: [{label: a}]}, {name: a, free_minutes: 1, runners: [{label: b}]}]`, "duplicate name"},
		{"bad kind", `providers: [{name: a, kind: cloud, runners: [{label: a}]}]`, "kind must be"},
		{"no runners", `providers: [{name: a, free_minutes: 1}]`, "at least one runner"},
		{"bad size", `providers: [{name: a, free_minutes: 1, runners: [{label: a, size: huge}]}]`, "size must be"},
		{"bad arch", `providers: [{name: a, free_minutes: 1, runners: [{label: a, arch: riscv}]}]`, "arch must be"},
		{"negative multiplier", `providers: [{name: a, free_minutes: 1, runners: [{label: a, multiplier: -1}]}]`, "multiplier must be > 0"},
		{"label in two providers", `providers: [{name: a, free_minutes: 1, runners: [{label: x}]}, {name: b, free_minutes: 1, runners: [{label: X}]}]`, "also used by"},
		{"cap_usd without price", `providers: [{name: a, allow_paid: true, cap_usd: 5, runners: [{label: a}]}]`, "needs price_per_minute"},
		{"negative free", `providers: [{name: a, free_minutes: -1, runners: [{label: a}]}]`, "free_minutes must be >= 0"},
		{"bad pattern", `providers: [{name: a, free_minutes: 1, runners: [{label: a}], count: [{pattern: "["}]}]`, "invalid pattern"},
		{"unknown paid_order", "paid_order: [zzz]\nproviders: [{name: a, free_minutes: 1, runners: [{label: a}]}]", "unknown provider"},
		{"bad repo", "usage: {repos: [nope]}\nproviders: [{name: a, free_minutes: 1, runners: [{label: a}]}]", "not owner/name"},
		{"bad on_exhausted", "on_exhausted: panic\nproviders: [{name: a, free_minutes: 1, runners: [{label: a}]}]", "on_exhausted must be"},
		{"billing_api on self-hosted", `providers: [{name: a, kind: self-hosted, billing_api: true, runners: [{label: a}]}]`, "billing_api needs"},
		{"billing_api twice", `providers: [{name: a, free_minutes: 1, price_per_minute: 1, billing_api: true, runners: [{label: a}]}, {name: b, free_minutes: 1, price_per_minute: 1, billing_api: true, runners: [{label: b}]}]`, "one provider only"},
		{"bad duration", "usage: {cache_ttl: soon}\nproviders: [{name: a, free_minutes: 1, runners: [{label: a}]}]", "invalid duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestValidateWarnings(t *testing.T) {
	c, err := Parse([]byte(`
providers:
  - {name: never, runners: [{label: a}]}
  - {name: unlimited, allow_paid: true, runners: [{label: b}]}
  - {name: pi, kind: self-hosted, free_minutes: 10, runners: [{label: c}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	_, w := c.Validate()
	all := strings.Join(w, "\n")
	for _, want := range []string{"never picked", "unlimited spend", "ignored for self-hosted"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing warning %q in:\n%s", want, all)
		}
	}
}

func TestRunnerLabels(t *testing.T) {
	tests := []struct {
		label, jobLabel string
		match           bool
	}{
		{"jog", "jog", true},
		{"jog", "JOG", true},
		{"jog", "jog-4vcpu", false},
		{"runs-on={{run_id}}/runner=2cpu-linux-x64", "runs-on=123456/runner=2cpu-linux-x64", true},
		{"runs-on={{run_id}}/runner=2cpu-linux-x64", "runs-on=123456/runner=4cpu-linux-x64", false},
		{"runs-on={{run_id}}/runner=2cpu-linux-x64", "runs-on=1/2/runner=2cpu-linux-x64", false},
	}
	for _, tt := range tests {
		if got := (Runner{Label: tt.label}).Matches(tt.jobLabel); got != tt.match {
			t.Errorf("%q matches %q = %v, want %v", tt.label, tt.jobLabel, got, tt.match)
		}
	}
	if got := (Runner{Label: "runs-on={{run_id}}/runner=2cpu-linux-x64"}).Render("42"); got != "runs-on=42/runner=2cpu-linux-x64" {
		t.Errorf("render: %s", got)
	}
}

func TestProviderMatch(t *testing.T) {
	p := Provider{
		Runners: []Runner{{Label: "jog-4vcpu", Multiplier: 2}},
		Count:   []CountRule{{Pattern: "jog-*", Multiplier: 1.5}},
	}
	tests := []struct {
		labels []string
		mult   float64
		ok     bool
	}{
		{[]string{"jog-4vcpu"}, 2, true},
		{[]string{"jog-16vcpu"}, 1.5, true},
		{[]string{"self-hosted", "linux"}, 0, false},
	}
	for _, tt := range tests {
		m, ok := p.Match(tt.labels)
		if m != tt.mult || ok != tt.ok {
			t.Errorf("Match(%v) = %v,%v want %v,%v", tt.labels, m, ok, tt.mult, tt.ok)
		}
	}
}
