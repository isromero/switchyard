// Package gh is a minimal GitHub REST API client with only the calls
// switchyard needs.
package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Client talks to the GitHub REST API.
type Client struct {
	BaseURL string // e.g. https://api.github.com
	token   string
	HTTP    *http.Client
}

// New returns a client. The token is never logged.
func New(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
}

// Runner is a self-hosted runner.
type Runner struct {
	ID     int64   `json:"id"`
	Name   string  `json:"name"`
	Status string  `json:"status"` // "online" or "offline"
	Busy   bool    `json:"busy"`
	Labels []Label `json:"labels"`
}

// Label is a runner label.
type Label struct {
	Name string `json:"name"`
}

// HasLabel reports whether the runner has the label (case-insensitive).
func (r Runner) HasLabel(l string) bool {
	for _, x := range r.Labels {
		if strings.EqualFold(x.Name, l) {
			return true
		}
	}
	return false
}

// Repo is a repository.
type Repo struct {
	FullName string    `json:"full_name"`
	Private  bool      `json:"private"`
	PushedAt time.Time `json:"pushed_at"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// Run is a workflow run.
type Run struct {
	ID         int64     `json:"id"`
	RunAttempt int       `json:"run_attempt"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"created_at"`
}

// Job is a workflow job.
type Job struct {
	ID          int64      `json:"id"`
	RunAttempt  int        `json:"run_attempt"`
	Status      string     `json:"status"`
	Labels      []string   `json:"labels"`
	RunnerName  string     `json:"runner_name"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`
}

// APIError is a non-2xx response.
type APIError struct {
	Status int
	Path   string
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github api %s: %d %s", e.Path, e.Status, e.Msg)
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// get fetches one page into v and returns the next page URL, if any.
func (c *Client) get(ctx context.Context, rawURL string, v any) (string, error) {
	if !strings.HasPrefix(rawURL, "http") {
		rawURL = c.BaseURL + rawURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "switchyard")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Strip the URL query from transport errors; it never holds the
		// token, but keep messages short.
		return "", fmt.Errorf("github api %s: %w", req.URL.Path, unwrapURLError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var body struct {
			Message string `json:"message"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = json.Unmarshal(b, &body)
		return "", &APIError{Status: resp.StatusCode, Path: req.URL.Path, Msg: body.Message}
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return "", fmt.Errorf("github api %s: decode: %w", req.URL.Path, err)
	}
	next := ""
	if m := nextLink.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
		next = m[1]
	}
	return next, nil
}

func unwrapURLError(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}

// ListRepoRunners lists self-hosted runners of a repository.
// Needs admin (fine-grained: "Administration: read") on the repository.
func (c *Client) ListRepoRunners(ctx context.Context, fullName string) ([]Runner, error) {
	return c.listRunners(ctx, "/repos/"+fullName+"/actions/runners?per_page=100")
}

func (c *Client) listRunners(ctx context.Context, u string) ([]Runner, error) {
	var out []Runner
	for u != "" {
		var page struct {
			Runners []Runner `json:"runners"`
		}
		next, err := c.get(ctx, u, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Runners...)
		u = next
	}
	return out, nil
}

// ListAccountRepos lists all repositories of a user or organization
// the token can see, including private ones when the token belongs to
// that user or has access to that org.
func (c *Client) ListAccountRepos(ctx context.Context, account string) ([]Repo, error) {
	var acct struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	}
	if _, err := c.get(ctx, "/users/"+url.PathEscape(account), &acct); err != nil {
		return nil, err
	}
	var u string
	filterOwner := false
	if acct.Type == "Organization" {
		u = "/orgs/" + url.PathEscape(account) + "/repos?type=all&per_page=100"
	} else {
		var me struct {
			Login string `json:"login"`
		}
		if _, err := c.get(ctx, "/user", &me); err == nil && strings.EqualFold(me.Login, account) {
			u = "/user/repos?affiliation=owner&visibility=all&per_page=100"
			filterOwner = true
		} else {
			u = "/users/" + url.PathEscape(account) + "/repos?type=owner&per_page=100"
		}
	}
	var out []Repo
	for u != "" {
		var page []Repo
		next, err := c.get(ctx, u, &page)
		if err != nil {
			return nil, err
		}
		for _, r := range page {
			if filterOwner && !strings.EqualFold(r.Owner.Login, account) {
				continue
			}
			out = append(out, r)
		}
		u = next
	}
	return out, nil
}

// GetRepo returns one repository.
func (c *Client) GetRepo(ctx context.Context, fullName string) (Repo, error) {
	var r Repo
	_, err := c.get(ctx, "/repos/"+fullName, &r)
	return r, err
}

// runsSearchLimit is the most results the runs endpoint returns for a
// query using the "created" filter.
const runsSearchLimit = 1000

const runsPerPage = 100

// ListRuns lists workflow runs created in [from, to). Ranges with more
// runs than the API will return are split in half until they fit.
// Pages after the first are fetched in parallel.
func (c *Client) ListRuns(ctx context.Context, fullName string, from, to time.Time) ([]Run, error) {
	type runsPage struct {
		TotalCount   int   `json:"total_count"`
		WorkflowRuns []Run `json:"workflow_runs"`
	}
	base := fmt.Sprintf("/repos/%s/actions/runs?per_page=%d&created=%s", fullName, runsPerPage,
		url.QueryEscape(from.UTC().Format(time.RFC3339)+".."+to.Add(-time.Second).UTC().Format(time.RFC3339)))
	var first runsPage
	if _, err := c.get(ctx, base, &first); err != nil {
		return nil, err
	}
	if first.TotalCount > runsSearchLimit && to.Sub(from) > 2*time.Minute {
		mid := from.Add(to.Sub(from) / 2).Truncate(time.Second)
		a, err := c.ListRuns(ctx, fullName, from, mid)
		if err != nil {
			return nil, err
		}
		b, err := c.ListRuns(ctx, fullName, mid, to)
		if err != nil {
			return nil, err
		}
		return append(a, b...), nil
	}
	pages := (min(first.TotalCount, runsSearchLimit) + runsPerPage - 1) / runsPerPage
	if pages <= 1 {
		return first.WorkflowRuns, nil
	}
	results := make([][]Run, pages)
	results[0] = first.WorkflowRuns
	errs := make([]error, pages)
	var wg sync.WaitGroup
	for i := 1; i < pages; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var p runsPage
			_, errs[i] = c.get(ctx, fmt.Sprintf("%s&page=%d", base, i+1), &p)
			results[i] = p.WorkflowRuns
		}(i)
	}
	wg.Wait()
	var out []Run
	for i := range results {
		if errs[i] != nil {
			return nil, errs[i]
		}
		out = append(out, results[i]...)
	}
	return out, nil
}

// ListJobs lists the jobs of all attempts of a run.
func (c *Client) ListJobs(ctx context.Context, fullName string, runID int64) ([]Job, error) {
	var out []Job
	u := fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=all&per_page=100", fullName, runID)
	for u != "" {
		var page struct {
			Jobs []Job `json:"jobs"`
		}
		next, err := c.get(ctx, u, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Jobs...)
		u = next
	}
	return out, nil
}

// ListActiveRuns lists the runs of a repository with the given status
// ("queued" or "in_progress").
func (c *Client) ListActiveRuns(ctx context.Context, fullName, status string) ([]Run, error) {
	var out []Run
	u := fmt.Sprintf("/repos/%s/actions/runs?status=%s&per_page=100", fullName, url.QueryEscape(status))
	for u != "" {
		var page struct {
			WorkflowRuns []Run `json:"workflow_runs"`
		}
		next, err := c.get(ctx, u, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page.WorkflowRuns...)
		u = next
	}
	return out, nil
}

// ListLatestJobs lists the jobs of the latest attempt of a run.
func (c *Client) ListLatestJobs(ctx context.Context, fullName string, runID int64) ([]Job, error) {
	var out []Job
	u := fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", fullName, runID)
	for u != "" {
		var page struct {
			Jobs []Job `json:"jobs"`
		}
		next, err := c.get(ctx, u, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Jobs...)
		u = next
	}
	return out, nil
}

// IsOrg reports whether an account is an organization.
func (c *Client) IsOrg(ctx context.Context, account string) (bool, error) {
	var acct struct {
		Type string `json:"type"`
	}
	if _, err := c.get(ctx, "/users/"+url.PathEscape(account), &acct); err != nil {
		return false, err
	}
	return acct.Type == "Organization", nil
}

// BillingItem is one line of GitHub's billing usage report.
type BillingItem struct {
	Product        string  `json:"product"`
	SKU            string  `json:"sku"`
	Quantity       float64 `json:"quantity"`
	UnitType       string  `json:"unitType"`
	PricePerUnit   float64 `json:"pricePerUnit"`
	GrossAmount    float64 `json:"grossAmount"`
	DiscountAmount float64 `json:"discountAmount"`
	NetAmount      float64 `json:"netAmount"`
	RepositoryName string  `json:"repositoryName"`
}

// BillingUsage returns the billing usage report of a user or organization
// for one month. Needs "Plan: read" (user) or "Administration: read"
// (organization) on a fine-grained token, or the "user" / "admin:org"
// scope on a classic one.
func (c *Client) BillingUsage(ctx context.Context, account string, org bool, year int, month time.Month) ([]BillingItem, error) {
	base := "/users/"
	if org {
		base = "/organizations/"
	}
	var page struct {
		UsageItems []BillingItem `json:"usageItems"`
	}
	_, err := c.get(ctx, fmt.Sprintf("%s%s/settings/billing/usage?year=%d&month=%d", base, url.PathEscape(account), year, int(month)), &page)
	return page.UsageItems, err
}

// ListOrgRunnersFor lists an organization's self-hosted runners that the
// repository repoName may use: those in runner groups visible to it.
// Needs organization "Self-hosted runners: read".
func (c *Client) ListOrgRunnersFor(ctx context.Context, org, repoName string) ([]Runner, error) {
	var groups []struct {
		ID int64 `json:"id"`
	}
	u := "/orgs/" + url.PathEscape(org) + "/actions/runner-groups?per_page=100&visible_to_repository=" + url.QueryEscape(repoName)
	for u != "" {
		var page struct {
			RunnerGroups []struct {
				ID int64 `json:"id"`
			} `json:"runner_groups"`
		}
		next, err := c.get(ctx, u, &page)
		if err != nil {
			return nil, err
		}
		groups = append(groups, page.RunnerGroups...)
		u = next
	}
	seen := map[int64]bool{}
	var out []Runner
	for _, g := range groups {
		rs, err := c.listRunners(ctx, fmt.Sprintf("/orgs/%s/actions/runner-groups/%d/runners?per_page=100", url.PathEscape(org), g.ID))
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			if !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, r)
			}
		}
	}
	return out, nil
}
