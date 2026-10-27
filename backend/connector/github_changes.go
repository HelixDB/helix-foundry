package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

var _ ChangeReader = (*githubKind)(nil)

// githubFollowed are the tables GitHub can list by update time. The rest have
// no such filter and are small, so every sync reads them in full.
var githubFollowed = map[string]bool{
	"issues": true, "pull_requests": true, "issue_comments": true, "pull_request_reviews": true,
}

// githubCursor is when each followed table's next change read begins, per
// repository id: a repository resolves anew on every read and may be renamed.
type githubCursor struct {
	Repositories map[string]map[string]time.Time `json:"repositories"`
}

func githubCursorAt(repos []githubRepo, tables []Table, since time.Time) githubCursor {
	c := githubCursor{Repositories: make(map[string]map[string]time.Time, len(repos))}
	for _, repo := range repos {
		times := make(map[string]time.Time, len(tables))
		for _, t := range tables {
			times[t.Name] = since
		}
		c.Repositories[strconv.FormatInt(repo.ID, 10)] = times
	}
	return c
}

// timeCursor is where the next change read begins when this one begins at
// start, in the whole seconds GitHub's since takes.
func timeCursor(start time.Time) time.Time {
	return start.Add(-ChangeOverlap).UTC().Truncate(time.Second)
}

func (k *githubKind) Capture(_ Source, table string) Capture {
	if githubFollowed[table] {
		return CaptureCursor
	}
	return CaptureFull
}

func (k *githubKind) Start(ctx context.Context, src Source, tables []Table) (Start, error) {
	since := timeCursor(time.Now())
	_, repos, err := k.resolve(ctx, src)
	if err != nil {
		return Start{}, err
	}
	cursor, err := json.Marshal(githubCursorAt(repos, tables, since))
	return Start{Cursor: cursor}, err
}

// Changes reads each repository's followed tables from the cursor. A
// repository the cursor has no time for, such as one just added to the
// settings, has nowhere to read from, so each followed table missing it is
// reset and read in full for every repository. Deletes are invisible here;
// reconcile finds them.
func (k *githubKind) Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	next := timeCursor(time.Now())
	var prev githubCursor
	if err := json.Unmarshal(cursor, &prev); err != nil {
		return nil, fmt.Errorf("decode github change cursor: %w", ErrCursorExpired)
	}
	s, repos, err := k.resolve(ctx, src)
	if err != nil {
		return nil, err
	}
	reset := map[string]bool{}
	for _, t := range tables {
		for _, repo := range repos {
			if _, ok := prev.Repositories[strconv.FormatInt(repo.ID, 10)][t.Name]; !ok && !reset[t.Name] {
				reset[t.Name] = true
				w.Reset(t.Name, "repository "+repo.FullName+" has no change cursor yet")
			}
		}
	}
	for _, repo := range repos {
		since := map[string]time.Time{}
		for _, t := range tables {
			if !reset[t.Name] {
				since[t.Name] = prev.Repositories[strconv.FormatInt(repo.ID, 10)][t.Name]
			}
		}
		if err := k.repoChanges(ctx, src.Secret, s, repo, since, w); err != nil {
			return nil, err
		}
	}
	return json.Marshal(githubCursorAt(repos, tables, next))
}

// repoChanges upserts one repository's rows changed after the time since
// holds for each table, normalized as a full read writes them.
func (k *githubKind) repoChanges(ctx context.Context, token string, s GitHubSettings, repo githubRepo,
	since map[string]time.Time, w ChangeWriter) error {
	path := "/repos/" + repo.FullName
	upsert := func(table string) func(map[string]any) error {
		return func(row map[string]any) error { return w.Upsert(table, normalizeGitHub(table, repo, row)) }
	}
	updatedSince := func(t time.Time) url.Values {
		return url.Values{"sort": {"updated"}, "direction": {"asc"}, "since": {t.UTC().Format(time.RFC3339)}}
	}
	if t, ok := since["issues"]; ok {
		query := updatedSince(t)
		query.Set("state", "all")
		if err := k.pages(ctx, token, path+"/issues", query, upsert("issues")); err != nil {
			return err
		}
	}
	if t, ok := since["issue_comments"]; ok {
		if err := k.pages(ctx, token, path+"/issues/comments", updatedSince(t), upsert("issue_comments")); err != nil {
			return err
		}
	}
	pullsSince, pulls := since["pull_requests"]
	reviewsSince, reviews := since["pull_request_reviews"]
	if !pulls && !reviews {
		return nil
	}
	// Reviews are kept only for pull requests updated within ReviewDays, as a full read keeps them.
	if cutoff := time.Now().AddDate(0, 0, -s.ReviewDays); reviewsSince.Before(cutoff) {
		reviewsSince = cutoff
	}
	oldest := reviewsSince
	if !reviews || pulls && pullsSince.Before(oldest) {
		oldest = pullsSince
	}
	// Pull requests have no since filter, so they are listed newest update first until one is older than both cursors.
	var numbers []int64
	recent := url.Values{"state": {"all"}, "sort": {"updated"}, "direction": {"desc"}}
	err := k.pages(ctx, token, path+"/pulls", recent, func(pr map[string]any) error {
		updated, _ := time.Parse(time.RFC3339, fmt.Sprint(pr["updated_at"]))
		if updated.Before(oldest) {
			return errStopPaging
		}
		if reviews && !updated.Before(reviewsSince) {
			numbers = append(numbers, int64Of(pr["number"]))
		}
		if pulls && !updated.Before(pullsSince) {
			return upsert("pull_requests")(pr)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return k.reviews(ctx, token, repo, numbers, upsert("pull_request_reviews"))
}
