package gh

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPagination(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/actions/runs/1/jobs?page=2>; rel="next", <%s/x?page=2>; rel="last"`, srvURL, srvURL))
			fmt.Fprint(w, `{"jobs":[{"id":1,"labels":["a"]}]}`)
			return
		}
		fmt.Fprint(w, `{"jobs":[{"id":2,"labels":["b"]}]}`)
	}))
	defer srv.Close()
	srvURL = srv.URL
	jobs, err := New(srv.URL, "t").ListJobs(context.Background(), "o/r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[1].ID != 2 {
		t.Fatalf("got %+v", jobs)
	}
}

func TestListRunsSplitsLargeRanges(t *testing.T) {
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		created := r.URL.Query().Get("created")
		parts := strings.Split(created, "..")
		a, _ := time.Parse(time.RFC3339, parts[0])
		// Pretend the first full range has too many runs; halves fit.
		if a.Equal(from) && strings.HasPrefix(parts[1], "2026-11-12T23:59:59") {
			fmt.Fprint(w, `{"total_count":1500,"workflow_runs":[{"id":999}]}`)
			return
		}
		fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[{"id":%d}]}`, a.Day())
	}))
	defer srv.Close()
	runs, err := New(srv.URL, "t").ListRuns(context.Background(), "o/r", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != 1 || runs[1].ID != 7 || calls.Load() != 3 {
		t.Fatalf("runs=%+v calls=%d", runs, calls.Load())
	}
}

func TestErrorsDoNotLeakToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"Resource not accessible by personal access token"}`)
	}))
	defer srv.Close()
	_, err := New(srv.URL, "ghp_secretvalue").ListRepoRunners(context.Background(), "o/r")
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "ghp_secretvalue") || !strings.Contains(err.Error(), "403") {
		t.Errorf("error: %v", err)
	}
}

func TestHasLabel(t *testing.T) {
	r := Runner{Labels: []Label{{Name: "self-hosted"}, {Name: "My-Pi"}}}
	if !r.HasLabel("my-pi") || r.HasLabel("my-arm-vm") {
		t.Error("HasLabel")
	}
}

func TestListRunsFetchesAllPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		fmt.Fprintf(w, `{"total_count":250,"workflow_runs":[{"id":%s}]}`, page)
	}))
	defer srv.Close()
	runs, err := New(srv.URL, "").ListRuns(context.Background(), "o/r", time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 || runs[0].ID != 1 || runs[2].ID != 3 {
		t.Fatalf("got %+v", runs)
	}
}

func TestBillingUsagePaths(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Path+"?"+r.URL.RawQuery)
		w.Write([]byte(`{"usageItems":[{"product":"actions","unitType":"Minutes","grossAmount":1.5,"repositoryName":"octo-user/app"}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "t")
	items, err := c.BillingUsage(context.Background(), "octo-user", false, 2026, time.November)
	if err != nil || len(items) != 1 || items[0].GrossAmount != 1.5 || items[0].RepositoryName != "octo-user/app" {
		t.Fatalf("items %+v err %v", items, err)
	}
	if _, err := c.BillingUsage(context.Background(), "octo-org", true, 2026, time.November); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/users/octo-user/settings/billing/usage?year=2026&month=11",
		"/organizations/octo-org/settings/billing/usage?year=2026&month=11",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("got %v", got)
	}
}

func TestListOrgRunnersForUsesVisibleGroups(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/orgs/octo-org/actions/runner-groups":
			if r.URL.Query().Get("visible_to_repository") != "app" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			w.Write([]byte(`{"total_count":2,"runner_groups":[{"id":1},{"id":2}]}`))
		case "/orgs/octo-org/actions/runner-groups/1/runners":
			w.Write([]byte(`{"runners":[{"id":10,"name":"a"},{"id":11,"name":"b"}]}`))
		case "/orgs/octo-org/actions/runner-groups/2/runners":
			w.Write([]byte(`{"runners":[{"id":11,"name":"b"}]}`))
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	rs, err := New(srv.URL, "t").ListOrgRunnersFor(context.Background(), "octo-org", "app")
	if err != nil || len(rs) != 2 {
		t.Fatalf("runners %+v err %v", rs, err)
	}
}
