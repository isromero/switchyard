package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setEnv(t *testing.T, kv map[string]string) {
	for _, k := range []string{"SWITCHYARD_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "SWITCHYARD_CONFIG", "SWITCHYARD_FALLBACK",
		"SWITCHYARD_DISABLE", "SWITCHYARD_SIZE", "SWITCHYARD_ARCH", "GITHUB_REPOSITORY", "GITHUB_OUTPUT", "GITHUB_ACTIONS",
		"GITHUB_API_URL", "RUNNER_NAME", "GITHUB_RUN_ID", "GITHUB_REPOSITORY_OWNER", "SWITCHYARD_RUNNERS_TOKEN", "SWITCHYARD_JOBS",
		"GITHUB_EVENT_PATH", "SWITCHYARD_CACHE_FILE"} {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestPickFailsOpenWithoutConfig(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	setEnv(t, map[string]string{"GITHUB_OUTPUT": out, "GITHUB_ACTIONS": "true"})
	var stdout, stderr bytes.Buffer
	code := run([]string{"pick", "--config", filepath.Join(dir, "missing.yml"), "--fallback", "my-fallback"}, &stdout, &stderr)
	if code != 0 || strings.TrimSpace(stdout.String()) != "my-fallback" {
		t.Fatalf("code=%d stdout=%q", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "::warning") {
		t.Errorf("no warning annotation: %s", stderr.String())
	}
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), "runner=my-fallback\n") || !strings.Contains(string(b), "fallback=true\n") {
		t.Errorf("GITHUB_OUTPUT: %q", b)
	}
}

func TestPickFailsOpenOnBadFlags(t *testing.T) {
	setEnv(t, nil)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"pick", "--nope"}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != "ubuntu-latest" {
		t.Fatalf("code=%d stdout=%q", code, stdout.String())
	}
}

func TestPickEndToEnd(t *testing.T) {
	// API: the pi runner is busy, the vm runner idle; usage is not needed.
	// The runner list uses the runners token, the queue check the main one.
	token, runnersToken := "ghp_do_not_print", "ghp_admin_do_not_print"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/octo-user/app/actions/runs" {
			if r.Header.Get("Authorization") != "Bearer "+token {
				t.Errorf("queue check: wrong token")
			}
			w.Write([]byte(`{"total_count":0,"workflow_runs":[]}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+runnersToken {
			t.Errorf("runner list: wrong token")
		}
		if r.URL.Path != "/repos/octo-user/app/actions/runners" {
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"total_count":2,"runners":[
		  {"id":1,"name":"pi","os":"Linux","status":"online","busy":true,"labels":[{"name":"self-hosted"},{"name":"my-pi"}]},
		  {"id":2,"name":"vm","os":"Linux","status":"online","busy":false,"labels":[{"name":"self-hosted"},{"name":"my-arm-vm"}]}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := filepath.Join(dir, "switchyard.yml")
	os.WriteFile(cfg, []byte(`
usage: {cache_file: `+filepath.Join(dir, "cache.json")+`}
providers:
  - {name: pi, kind: self-hosted, runners: [{label: my-pi}]}
  - {name: vm, kind: self-hosted, runners: [{label: my-arm-vm}, {label: my-arm-vm, size: medium}]}
`), 0o644)
	out := filepath.Join(dir, "out")
	setEnv(t, map[string]string{
		"SWITCHYARD_TOKEN": token, "SWITCHYARD_RUNNERS_TOKEN": runnersToken,
		"GITHUB_API_URL": srv.URL, "GITHUB_REPOSITORY": "octo-user/app", "GITHUB_OUTPUT": out,
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"pick", "--config", cfg}, &stdout, &stderr); code != 0 {
		t.Fatalf("code %d", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != "my-arm-vm" {
		t.Fatalf("picked %q; stderr:\n%s", got, stderr.String())
	}
	b, _ := os.ReadFile(out)
	for _, want := range []string{"runner=my-arm-vm\n", "provider=vm\n", "runner_medium=my-arm-vm\n", "runner_large=ubuntu-latest\n"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("GITHUB_OUTPUT missing %q:\n%s", want, b)
		}
	}
	if all := stderr.String() + stdout.String() + string(b); strings.Contains(all, token) || strings.Contains(all, runnersToken) {
		t.Error("token printed")
	}
}

func TestPickStopsWhenExhausted(t *testing.T) {
	dir := t.TempDir()
	cache := filepath.Join(dir, "cache.json")
	// A fresh cache where the only provider has used its minutes.
	month := time.Now().UTC().Format("2006-01")
	started := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	os.WriteFile(cache, []byte(`{"version":3,"scope":"account:octo-user","month":"`+month+`","complete":true,"updated_at":"`+time.Now().UTC().Format(time.RFC3339)+`",
	  "repos":{"octo-user/app":{"private":true,"checked_at":"`+started+`","runs":{"1":{"attempt":1,"completed":true,
	  "jobs":[{"labels":["ubuntu-latest"],"started":"`+started+`","minutes":2000}]}}}}}`), 0o644)
	cfg := filepath.Join(dir, "switchyard.yml")
	os.WriteFile(cfg, []byte(`
on_exhausted: fail
usage: {cache_file: `+cache+`, accounts: [octo-user]}
providers:
  - {name: github, free_minutes: 2000, price_per_minute: 0.006, ignore_public_repos: true, runners: [{label: ubuntu-latest}]}
`), 0o644)
	setEnv(t, map[string]string{"GITHUB_OUTPUT": filepath.Join(dir, "out"), "GITHUB_REPOSITORY": "octo-user/app"})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"pick", "--config", cfg}, &stdout, &stderr); code != exitStop {
		t.Fatalf("code %d, want %d; stderr:\n%s", code, exitStop, stderr.String())
	}
	if !strings.Contains(stderr.String(), "on_exhausted: fail") {
		t.Errorf("stderr: %s", stderr.String())
	}

	// The requested size can be served, another can't: that size's
	// output is empty instead of a billed fallback.
	os.WriteFile(cfg, []byte(`
on_exhausted: fail
usage: {cache_file: `+cache+`, accounts: [octo-user]}
providers:
  - {name: jog, free_minutes: 5000, runners: [{label: jog}]}
  - {name: github, free_minutes: 2000, price_per_minute: 0.006, ignore_public_repos: true, runners: [{label: ubuntu-latest}]}
`), 0o644)
	os.Remove(filepath.Join(dir, "out"))
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"pick", "--config", cfg}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != "jog" {
		t.Fatalf("code %d out %q; stderr:\n%s", code, stdout.String(), stderr.String())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "out")); !strings.Contains(string(b), "runner_small=jog\nrunner_medium=\nrunner_large=\n") {
		t.Errorf("outputs:\n%s", b)
	}

	// The same repository made public: GitHub-hosted is free, no stop.
	os.WriteFile(cfg, []byte(`
on_exhausted: fail
usage: {cache_file: `+cache+`, accounts: [octo-user]}
providers:
  - {name: github, free_minutes: 2000, price_per_minute: 0.006, ignore_public_repos: true, runners: [{label: ubuntu-latest}]}
`), 0o644)
	ev := filepath.Join(dir, "event.json")
	os.WriteFile(ev, []byte(`{"repository":{"private":false,"visibility":"public"}}`), 0o644)
	t.Setenv("GITHUB_EVENT_PATH", ev)
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"pick", "--config", cfg}, &stdout, &stderr); code != 0 || strings.TrimSpace(stdout.String()) != "ubuntu-latest" {
		t.Fatalf("public repo: code %d out %q; stderr:\n%s", code, stdout.String(), stderr.String())
	}
}

func TestCheck(t *testing.T) {
	setEnv(t, nil)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"check", "--config", "../../switchyard.yml"}, &stdout, &stderr); code != 0 {
		t.Fatalf("check failed: %s", stdout.String())
	}
	bad := filepath.Join(t.TempDir(), "bad.yml")
	os.WriteFile(bad, []byte("providers: [{name: a}]"), 0o644)
	stdout.Reset()
	if code := run([]string{"check", "--config", bad}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "invalid") {
		t.Fatalf("code=%d %s", code, stdout.String())
	}
}

func TestReadEvent(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name, payload string
		want          runEvent
	}{
		{"push to a public repo", `{"repository":{"full_name":"o/r","visibility":"public"}}`, runEvent{Public: true}},
		{"private by flag", `{"repository":{"full_name":"o/r","private":true}}`, runEvent{}},
		{"pull request from a branch", `{"repository":{"full_name":"o/r","visibility":"public"},"pull_request":{"head":{"repo":{"full_name":"O/R"}}}}`, runEvent{Public: true}},
		{"pull request from a fork", `{"repository":{"full_name":"o/r","visibility":"public"},"pull_request":{"head":{"repo":{"full_name":"someone/r"}}}}`, runEvent{Public: true, ForkPR: true}},
		{"pull request from a deleted fork", `{"repository":{"full_name":"o/r","visibility":"public"},"pull_request":{"head":{"repo":null}}}`, runEvent{Public: true, ForkPR: true}},
		{"not json", `nope`, runEvent{}},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := filepath.Join(dir, fmt.Sprintf("ev%d.json", i))
			os.WriteFile(f, []byte(tt.payload), 0o644)
			t.Setenv("GITHUB_EVENT_PATH", f)
			if got := readEvent(); got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestPickJobsOutput(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "switchyard.yml")
	os.WriteFile(cfg, []byte(`
fallback: ubuntu-latest
providers:
  - {name: cloud, runners: [{label: cloud-2}]}
`), 0o644)
	out := filepath.Join(dir, "out")
	setEnv(t, map[string]string{"GITHUB_OUTPUT": out, "GITHUB_REPOSITORY": "octo-user/app"})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"pick", "--config", cfg, "--jobs", "check, test*2"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code %d; stderr:\n%s", code, stderr.String())
	}
	// No free minutes and no paid pass: every job gets the fallback.
	b, _ := os.ReadFile(out)
	if !strings.Contains(string(b), `runners={"check":"ubuntu-latest","test-1":"ubuntu-latest","test-2":"ubuntu-latest"}`) {
		t.Errorf("outputs:\n%s", b)
	}

	// A config that can't be read still fills every job with the fallback.
	os.Remove(out)
	if code := run([]string{"pick", "--config", filepath.Join(dir, "missing.yml"), "--jobs", "a*2"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code %d", code)
	}
	if b, _ := os.ReadFile(out); !strings.Contains(string(b), `runners={"a-1":"ubuntu-latest","a-2":"ubuntu-latest"}`) {
		t.Errorf("outputs on config error:\n%s", b)
	}

	// A bad job list leaves runners empty but the run goes on.
	os.Remove(out)
	stderr.Reset()
	if code := run([]string{"pick", "--config", cfg, "--jobs", "a*0"}, &stdout, &stderr); code != 0 {
		t.Fatalf("code %d", code)
	}
	if b, _ := os.ReadFile(out); !strings.Contains(string(b), "runners={}\n") || !strings.Contains(stderr.String(), "jobs:") {
		t.Errorf("outputs:\n%s\nstderr:\n%s", b, stderr.String())
	}
}
