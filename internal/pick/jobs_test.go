package pick

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/isromero/switchyard/internal/config"
	"github.com/isromero/switchyard/internal/gh"
)

func TestParseJobs(t *testing.T) {
	tests := []struct {
		spec    string
		want    []Slot
		wantErr string
	}{
		{spec: "", want: nil},
		{spec: "check", want: []Slot{{"check", "small"}}},
		{spec: "check, test:medium*3\nlint:large", want: []Slot{
			{"check", "small"}, {"test-1", "medium"}, {"test-2", "medium"}, {"test-3", "medium"}, {"lint", "large"}}},
		{spec: "a b,,c", want: []Slot{{"a", "small"}, {"b", "small"}, {"c", "small"}}},
		{spec: "test*0", wantErr: "positive"},
		{spec: "test*x", wantErr: "positive"},
		{spec: "test:huge", wantErr: "unknown size"},
		{spec: "a/b", wantErr: "job name"},
		{spec: "test, test", wantErr: "twice"},
		{spec: "test-1, test*2", wantErr: "twice"},
		{spec: "t*65", wantErr: "more than"},
	}
	for _, tt := range tests {
		got, err := ParseJobs(tt.spec, "small")
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("%q: err %v, want %q", tt.spec, err, tt.wantErr)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %v, %v; want %v", tt.spec, got, err, tt.want)
		}
	}
}

func TestAssignSpreadsJobs(t *testing.T) {
	cfg, err := config.Parse([]byte(`
providers:
  - {name: vm, kind: self-hosted, runners: [{label: my-vm}, {label: my-vm, size: medium}]}
  - {name: cloud, free_minutes: 3000, runners: [{label: cloud-2}, {label: cloud-4, size: medium}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	two := []gh.Runner{runner("vm-1", "online", false, "my-vm"), runner("vm-2", "online", false, "my-vm")}
	slots, _ := ParseJobs("check, test:medium*3", "small")

	labels := func(ds []Decision) []string {
		var out []string
		for _, d := range ds {
			out = append(out, d.Label)
		}
		return out
	}
	// Two idle runners: the first two jobs get them, the rest go to the cloud.
	got := labels(Assign(cfg, Request{}, &fakeState{runners: two, usage: map[string]float64{}}, slots))
	if want := []string{"my-vm", "my-vm", "cloud-4", "cloud-4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("two idle: got %v, want %v", got, want)
	}
	// One of them already has a queued job waiting: only one is free.
	got = labels(Assign(cfg, Request{}, &fakeState{runners: two, queued: [][]string{{"my-vm"}}, usage: map[string]float64{}}, slots))
	if want := []string{"my-vm", "cloud-4", "cloud-4", "cloud-4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("one queued: got %v, want %v", got, want)
	}
	// Runner list unavailable: everything goes to the cloud.
	got = labels(Assign(cfg, Request{}, &fakeState{runnersErr: errors.New("403"), usage: map[string]float64{}}, slots))
	if want := []string{"cloud-2", "cloud-4", "cloud-4", "cloud-4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("no runner list: got %v, want %v", got, want)
	}
}
