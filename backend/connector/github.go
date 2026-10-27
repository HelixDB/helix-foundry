package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	githubTokenRE = regexp.MustCompile(`^(gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})$`)
	githubRepoRE  = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
)

const (
	githubAPIVersion = "2022-11-28"
	// maxGitHubRepos bounds one connector; a token that reads more names the ones to sync.
	maxGitHubRepos          = 100
	maxGitHubPages          = 10_000
	defaultGitHubReviewDays = 90
)

var githubObjects = []saasTable{
	{"repositories", "repository_id"},
	{"users", "user_id"},
	{"labels", "label_id"},
	{"milestones", "milestone_id"},
	// A pull request is an issue on GitHub, so both are keyed by repository and number.
	{"issues", "issue_key"},
	{"pull_requests", "issue_key"},
	{"issue_comments", "comment_id"},
	{"pull_request_reviews", "review_id"},
}

// githubProbes is the repository list that proves an object readable; a
// fine-grained token reads issues and pull requests under separate permissions.
var githubProbes = map[string]string{
	"users":                "/assignees",
	"labels":               "/labels",
	"milestones":           "/milestones",
	"issues":               "/issues",
	"pull_requests":        "/pulls",
	"issue_comments":       "/issues/comments",
	"pull_request_reviews": "/pulls",
}

var githubForeignKeys = referencedBy(githubObjects, map[string][]string{
	"labels":               {"repository_id"},
	"milestones":           {"repository_id"},
	"issues":               {"repository_id", "milestone_id", "user_id"},
	"pull_requests":        {"issue_key", "repository_id", "milestone_id", "user_id"},
	"issue_comments":       {"issue_key", "repository_id", "user_id"},
	"pull_request_reviews": {"issue_key", "repository_id", "user_id"},
})

// GitHubSettings choose what a GitHub connector reads.
type GitHubSettings struct {
	// Repos are owner/name; empty reads every repository the token can.
	Repos []string `json:"repos,omitempty"`
	// ReviewDays limits reviews to pull requests updated in the last this many
	// days: reviews are listed one pull request at a time.
	ReviewDays int `json:"review_days"`
}

type githubKind struct {
	base string
	http *http.Client
}

type githubRepo struct {
	ID       int64
	FullName string
}

func (k *githubKind) Name() string        { return "github" }
func (k *githubKind) DisplayName() string { return "GitHub" }

func (k *githubKind) Prepare(src Source) (json.RawMessage, error) {
	if !githubTokenRE.MatchString(strings.TrimSpace(src.Secret)) {
		return nil, invalid("enter a GitHub personal access token (github_pat_… or ghp_…)")
	}
	var s GitHubSettings
	if err := json.Unmarshal(orEmpty(src.Settings), &s); err != nil {
		return nil, invalid("GitHub settings are malformed")
	}
	seen := map[string]bool{}
	var repos []string
	for _, repo := range s.Repos {
		repo = strings.TrimSpace(repo)
		if !githubRepoRE.MatchString(repo) {
			return nil, invalid("%q is not a repository; write it as owner/name", repo)
		}
		if !seen[strings.ToLower(repo)] {
			seen[strings.ToLower(repo)] = true
			repos = append(repos, repo)
		}
	}
	if len(repos) > maxGitHubRepos {
		return nil, invalid("choose at most %d repositories", maxGitHubRepos)
	}
	s.Repos = repos
	switch {
	case s.ReviewDays == 0:
		s.ReviewDays = defaultGitHubReviewDays
	case s.ReviewDays < 1 || s.ReviewDays > 365:
		return nil, invalid("review days must be between 1 and 365")
	}
	return json.Marshal(s)
}

// Discover checks the token, resolves the repositories and probes the first
// one for each object's permission.
func (k *githubKind) Discover(ctx context.Context, src Source) (Discovery, error) {
	var s GitHubSettings
	if err := json.Unmarshal(src.Settings, &s); err != nil {
		return Discovery{}, invalid("GitHub settings are malformed")
	}
	status, _, err := k.get(ctx, src.Secret, "/user", nil)
	if err != nil {
		return Discovery{}, err
	}
	if err := githubStatus(status, "/user"); err != nil {
		return Discovery{}, err
	}
	repos, err := k.repos(ctx, src.Secret, s.Repos)
	if err != nil {
		return Discovery{}, err
	}
	if len(repos) == 0 {
		return Discovery{}, refused(nil, "this token cannot read any repository; grant it repositories and test again")
	}
	readable, err := probeAll(ctx, uniqueValues(githubProbes), func(ctx context.Context, path string) (bool, error) {
		status, _, err := k.get(ctx, src.Secret, "/repos/"+repos[0].FullName+path, url.Values{"per_page": {"1"}})
		switch {
		case err != nil:
			return false, err
		case status == http.StatusForbidden || status == http.StatusNotFound || status == http.StatusGone:
			return false, nil
		}
		return true, githubStatus(status, path)
	})
	if err != nil {
		return Discovery{}, err
	}
	d := saasDiscovery(githubObjects, func(name string) bool {
		return name == "repositories" || readable[githubProbes[name]]
	}, githubForeignKeys)
	for i := range d.Tables {
		// Reviews are read for pull requests updated within review_days only.
		d.Tables[i].Windowed = d.Tables[i].Name == "pull_request_reviews"
	}
	return d, nil
}

func (k *githubKind) Hint(secret string) string {
	secret = strings.TrimSpace(secret)
	if strings.HasPrefix(secret, "github_pat_") {
		return keyHint(secret, 2)
	}
	return keyHint(secret, 1)
}

func (k *githubKind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	s, repos, err := k.resolve(ctx, src)
	if err != nil {
		return table, err
	}
	seenUsers := map[any]bool{}
	for _, repo := range repos {
		if err := k.syncRepo(ctx, src.Secret, s, repo, table.Name, seenUsers, w); err != nil {
			return table, err
		}
	}
	return table, nil
}

// resolve reads the prepared settings and the repositories they name.
func (k *githubKind) resolve(ctx context.Context, src Source) (GitHubSettings, []githubRepo, error) {
	var s GitHubSettings
	if err := json.Unmarshal(src.Settings, &s); err != nil {
		return s, nil, invalid("GitHub settings are malformed")
	}
	repos, err := k.repos(ctx, src.Secret, s.Repos)
	return s, repos, err
}

func (k *githubKind) syncRepo(ctx context.Context, token string, s GitHubSettings, repo githubRepo, table string,
	seenUsers map[any]bool, w RowWriter) error {
	path := "/repos/" + repo.FullName
	write := func(row map[string]any) error { return w.Write(normalizeGitHub(table, repo, row)) }
	created := url.Values{"state": {"all"}, "sort": {"created"}, "direction": {"asc"}}
	switch table {
	case "repositories":
		status, body, err := k.get(ctx, token, path, nil)
		if err != nil {
			return err
		}
		if err := githubStatus(status, path); err != nil {
			return err
		}
		var row map[string]any
		if err := json.Unmarshal(body, &row); err != nil {
			return fmt.Errorf("decode github %s: %w", path, err)
		}
		return write(row)
	case "users":
		return k.pages(ctx, token, path+"/assignees", nil, func(row map[string]any) error {
			if seenUsers[row["id"]] {
				return nil
			}
			seenUsers[row["id"]] = true
			return write(row)
		})
	case "labels":
		return k.pages(ctx, token, path+"/labels", nil, write)
	case "milestones":
		return k.pages(ctx, token, path+"/milestones", url.Values{"state": {"all"}}, write)
	case "issues":
		return k.pages(ctx, token, path+"/issues", created, write)
	case "pull_requests":
		return k.pages(ctx, token, path+"/pulls", created, write)
	case "issue_comments":
		return k.pages(ctx, token, path+"/issues/comments", url.Values{"sort": {"created"}, "direction": {"asc"}}, write)
	case "pull_request_reviews":
		return k.syncReviews(ctx, token, s, repo, write)
	}
	return invalid("GitHub has no object %q", table)
}

// errStopPaging ends a list early without failing it.
var errStopPaging = errors.New("stop paging")

// syncReviews lists the reviews of each pull request updated within ReviewDays.
func (k *githubKind) syncReviews(ctx context.Context, token string, s GitHubSettings, repo githubRepo, write func(map[string]any) error) error {
	path := "/repos/" + repo.FullName
	cutoff := time.Now().AddDate(0, 0, -s.ReviewDays)
	var numbers []int64
	recent := url.Values{"state": {"all"}, "sort": {"updated"}, "direction": {"desc"}}
	err := k.pages(ctx, token, path+"/pulls", recent, func(pr map[string]any) error {
		updated, _ := time.Parse(time.RFC3339, fmt.Sprint(pr["updated_at"]))
		if updated.Before(cutoff) {
			return errStopPaging
		}
		numbers = append(numbers, int64Of(pr["number"]))
		return nil
	})
	if err != nil {
		return err
	}
	return k.reviews(ctx, token, repo, numbers, write)
}

// reviews lists the reviews of each numbered pull request, tied to it by issue_key.
func (k *githubKind) reviews(ctx context.Context, token string, repo githubRepo, numbers []int64, write func(map[string]any) error) error {
	for _, number := range numbers {
		key := githubIssueKey(repo.ID, number)
		err := k.pages(ctx, token, fmt.Sprintf("/repos/%s/pulls/%d/reviews", repo.FullName, number), nil, func(row map[string]any) error {
			row["issue_key"] = key
			return write(row)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// repos resolves the chosen repositories, or lists every one the token reads.
func (k *githubKind) repos(ctx context.Context, token string, chosen []string) ([]githubRepo, error) {
	var out []githubRepo
	if len(chosen) == 0 {
		err := k.pages(ctx, token, "/user/repos", url.Values{"sort": {"full_name"}}, func(row map[string]any) error {
			if len(out) == maxGitHubRepos {
				return errTooManyRepos
			}
			out = append(out, githubRepo{ID: int64Of(row["id"]), FullName: fmt.Sprint(row["full_name"])})
			return nil
		})
		if errors.Is(err, errTooManyRepos) {
			return nil, invalid("this token reads more than %d repositories; list the ones to sync", maxGitHubRepos)
		}
		return out, err
	}
	for _, name := range chosen {
		status, body, err := k.get(ctx, token, "/repos/"+name, nil)
		if err != nil {
			return nil, err
		}
		if status == http.StatusForbidden || status == http.StatusNotFound {
			return nil, invalid("GitHub has no repository %s that this token can read", name)
		}
		if err := githubStatus(status, "/repos/"+name); err != nil {
			return nil, err
		}
		var repo struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		}
		if err := json.Unmarshal(body, &repo); err != nil {
			return nil, fmt.Errorf("decode github repository %s: %w", name, err)
		}
		out = append(out, githubRepo{ID: repo.ID, FullName: repo.FullName})
	}
	return out, nil
}

var errTooManyRepos = errors.New("too many repositories")

// pages lists a GitHub collection 100 rows at a time by page number. A 410 is
// a feature the repository turned off, such as issues, and lists nothing.
func (k *githubKind) pages(ctx context.Context, token, path string, query url.Values, each func(map[string]any) error) error {
	for page := 1; ; page++ {
		if page > maxGitHubPages {
			return fmt.Errorf("github %s: more than %d pages", path, maxGitHubPages)
		}
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		for key, values := range query {
			q[key] = values
		}
		status, body, err := k.get(ctx, token, path, q)
		if err != nil {
			return err
		}
		if status == http.StatusGone {
			return nil
		}
		if err := githubStatus(status, path); err != nil {
			return err
		}
		var rows []map[string]any
		if err := json.Unmarshal(body, &rows); err != nil {
			return fmt.Errorf("decode github %s page: %w", path, err)
		}
		for _, row := range rows {
			if err := each(row); errors.Is(err, errStopPaging) {
				return nil
			} else if err != nil {
				return err
			}
		}
		if len(rows) < 100 {
			return nil
		}
	}
}

// get sends one GitHub request. A token whose rate limit is spent is refused
// with the time it resets, so the sync retries rather than failing for good.
func (k *githubKind) get(ctx context.Context, token, path string, query url.Values) (int, []byte, error) {
	target := k.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	status, header, body, err := fetchWithHeader(ctx, k.http, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err == nil {
			req.Header = http.Header{
				"Authorization":        {"Bearer " + strings.TrimSpace(token)},
				"Accept":               {"application/vnd.github+json"},
				"X-Github-Api-Version": {githubAPIVersion},
			}
		}
		return req, err
	})
	if err != nil {
		return 0, nil, refused(err, "could not reach GitHub; try again")
	}
	limited := status == http.StatusForbidden || status == http.StatusTooManyRequests
	switch {
	case limited && header.Get("X-Ratelimit-Remaining") == "0":
		resets := "later"
		if epoch, err := strconv.ParseInt(header.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
			resets = "at " + time.Unix(epoch, 0).UTC().Format("15:04 UTC")
		}
		return 0, nil, refused(nil, "this token has used up its GitHub rate limit; it resets %s", resets)
	// A secondary limit that outlasts fetch's retries must not read as a missing permission.
	case status == http.StatusTooManyRequests || limited && (header.Get("Retry-After") != "" || bytes.Contains(body, []byte("secondary rate limit"))):
		return 0, nil, refused(nil, "GitHub is limiting how fast this token can make requests; try again in a few minutes")
	}
	return status, body, nil
}

// withinHost follows a redirect only on the host of the first request, such
// as GitHub's redirect from a renamed repository, so the token never leaves it.
func withinHost(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 || req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
		return http.ErrUseLastResponse
	}
	return nil
}

func githubStatus(status int, path string) error {
	switch {
	case status == http.StatusUnauthorized:
		return refused(nil, "GitHub rejected this token; check it and enter it again")
	case status == http.StatusForbidden || status == http.StatusNotFound:
		return refused(nil, "this token cannot read %s; check its repository access and permissions", path)
	case status >= 300:
		return refused(nil, "GitHub returned HTTP %d for %s", status, path)
	}
	return nil
}

func githubIssueKey(repositoryID, number int64) string {
	return fmt.Sprintf("%d#%d", repositoryID, number)
}

// githubDropped are fields that only describe the API or repeat another table.
var githubDropped = map[string]bool{
	"node_id": true, "_links": true, "permissions": true, "reactions": true, "repository": true,
	"performed_via_github_app": true, "assignee": true, "requested_teams": true, "organization": true,
	"parent": true, "source": true, "template_repository": true, "security_and_analysis": true,
	"auto_merge": true, "sub_issues_summary": true, "issue_dependencies_summary": true, "pull_request": true,
}

// normalizeGitHub renames id to the table's key, adds the repository, keeps
// the ids of nested users, labels and milestones, and drops API links.
func normalizeGitHub(table string, repo githubRepo, row map[string]any) map[string]any {
	out := make(map[string]any, len(row))
	for field, value := range row {
		switch {
		case githubDropped[field], strings.HasSuffix(field, "_url") && field != "html_url":
		case field == "id":
			out[githubIDColumn[table]] = value
		case field == "user", field == "creator", field == "owner", field == "merged_by", field == "closed_by":
			out[field+"_id"], out[field+"_login"] = nil, nil
			if u, ok := value.(map[string]any); ok {
				out[field+"_id"], out[field+"_login"] = u["id"], u["login"]
			}
		case field == "assignees", field == "requested_reviewers":
			out[strings.TrimSuffix(field, "s")+"_logins"] = githubField(value, "login")
		case field == "labels":
			out["label_ids"], out["label_names"] = githubField(value, "id"), githubField(value, "name")
		case field == "milestone":
			out["milestone_id"] = nil
			if m, ok := value.(map[string]any); ok {
				out["milestone_id"] = m["id"]
			}
		case field == "head", field == "base":
			if ref, ok := value.(map[string]any); ok {
				out[field+"_ref"], out[field+"_sha"] = ref["ref"], ref["sha"]
			}
		case field == "license":
			out["license"] = nil
			if l, ok := value.(map[string]any); ok {
				out["license"] = l["spdx_id"]
			}
		default:
			out[field] = value
		}
	}
	if table != "repositories" && table != "users" {
		out["repository_id"] = repo.ID
	}
	switch table {
	case "issues":
		out["is_pull_request"] = row["pull_request"] != nil
		out["issue_key"] = githubIssueKey(repo.ID, int64Of(row["number"]))
	case "pull_requests":
		out["issue_key"] = githubIssueKey(repo.ID, int64Of(row["number"]))
	case "issue_comments":
		number, _ := strconv.ParseInt(lastSegment(fmt.Sprint(row["issue_url"])), 10, 64)
		out["issue_key"] = githubIssueKey(repo.ID, number)
	}
	return out
}

// githubIDColumn is the column each table's GitHub id lands in; issues and
// pull requests keep theirs beside the issue_key they are keyed by.
var githubIDColumn = map[string]string{
	"repositories": "repository_id", "users": "user_id", "labels": "label_id", "milestones": "milestone_id",
	"issues": "issue_id", "pull_requests": "pull_request_id", "issue_comments": "comment_id",
	"pull_request_reviews": "review_id",
}

// githubField collects one field of each object in a list.
func githubField(list any, field string) []any {
	items, _ := list.([]any)
	out := make([]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m[field])
		}
	}
	return out
}

func lastSegment(s string) string { return s[strings.LastIndexByte(s, '/')+1:] }

// int64Of reads a JSON number as an integer.
func int64Of(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}
