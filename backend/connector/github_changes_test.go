package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// cursorChanges is the ChangeWriter a time-cursor read fills, rows decoded as
// a snapshot holds them. GitHub and Linear never report a delete, so one fails the read.
type cursorChanges struct {
	noDocuments
	upserts map[string][]map[string]any
	resets  []string
}

func (c *cursorChanges) Upsert(table string, row map[string]any) error {
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return c.UpsertJSON(table, b)
}

func (c *cursorChanges) UpsertJSON(table string, row []byte) error {
	var m map[string]any
	if err := json.Unmarshal(row, &m); err != nil {
		return err
	}
	if c.upserts == nil {
		c.upserts = map[string][]map[string]any{}
	}
	c.upserts[table] = append(c.upserts[table], m)
	return nil
}

func (c *cursorChanges) Delete(table string, _ map[string]any) error {
	return fmt.Errorf("unexpected delete from %s", table)
}

func (c *cursorChanges) DeleteWhere(table, _ string, _ any) error {
	return fmt.Errorf("unexpected cascade on %s", table)
}

func (c *cursorChanges) Reset(table, _ string) { c.resets = append(c.resets, table) }

// assertTimeCursor checks a cursor time is its read's start, somewhere between before and after, less the overlap.
func assertTimeCursor(t *testing.T, got, before, after time.Time) {
	t.Helper()
	low, high := before.Add(-ChangeOverlap).Truncate(time.Second), after.Add(-ChangeOverlap)
	if got.Before(low) || got.After(high) {
		t.Fatalf("cursor = %s, want the read's start less %s: between %s and %s", got, ChangeOverlap, low, high)
	}
}

func TestGitHubChanges(t *testing.T) {
	ctx := context.Background()
	reader := func(t *testing.T, url string) ChangeReader {
		t.Helper()
		return kindOf(t, catalogFor(url), "github").(ChangeReader)
	}
	repo := func(*http.Request) (int, string) { return http.StatusOK, `{"id":1,"full_name":"acme/api"}` }

	// Labels, milestones, assignees and the repository itself have no "updated since" filter, so a change sync reads them in full.
	t.Run("follows issues, pull requests, comments and reviews only", func(t *testing.T) {
		want := map[string]Capture{
			"repositories": CaptureFull, "users": CaptureFull, "labels": CaptureFull, "milestones": CaptureFull,
			"issues": CaptureCursor, "pull_requests": CaptureCursor, "issue_comments": CaptureCursor, "pull_request_reviews": CaptureCursor,
		}
		r := reader(t, "http://unused")
		for table, capture := range want {
			if got := r.Capture(Source{}, table); got != capture {
				t.Errorf("Capture(%s) = %s, want %s", table, got, capture)
			}
		}
	})
	// The cursor is taken before the full read, so a change made during that read is read again by the next change sync.
	t.Run("starts each followed table of each repository before the full read, less the overlap", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/repos/acme/api": repo,
			"/repos/acme/web": func(*http.Request) (int, string) { return http.StatusOK, `{"id":2,"full_name":"acme/web"}` },
		})
		before := time.Now()
		start, err := reader(t, url).Start(ctx, githubSource(t, `{"repos":["acme/api","acme/web"]}`),
			[]Table{{Name: "issues"}, {Name: "pull_requests"}})
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		var cursor githubCursor
		if err := json.Unmarshal(start.Cursor, &cursor); err != nil {
			t.Fatal(err)
		}
		if len(cursor.Repositories) != 2 || len(cursor.Repositories["1"]) != 2 || len(cursor.Repositories["2"]) != 2 {
			t.Fatalf("cursor = %s", start.Cursor)
		}
		for _, times := range cursor.Repositories {
			for _, at := range times {
				assertTimeCursor(t, at, before, after)
			}
		}
	})
	// "since" filters by update time, so only what changed after the cursor is read; the next read starts where this one began.
	t.Run("reads issues and comments updated since the cursor", func(t *testing.T) {
		since := "2026-10-20T08:00:00Z"
		updatedSince := func(r *http.Request) {
			if q := r.URL.Query(); q.Get("since") != since || q.Get("sort") != "updated" || q.Get("direction") != "asc" {
				t.Errorf("query = %s", r.URL.RawQuery)
			}
		}
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/repos/acme/api": repo,
			"/repos/acme/api/issues": func(r *http.Request) (int, string) {
				updatedSince(r)
				if r.URL.Query().Get("state") != "all" {
					t.Errorf("query = %s", r.URL.RawQuery)
				}
				return http.StatusOK, `[{"id":2000,"number":101,"title":"Fix login"}]`
			},
			"/repos/acme/api/issues/comments": func(r *http.Request) (int, string) {
				updatedSince(r)
				return http.StatusOK, `[{"id":5,"issue_url":"https://api.github.com/repos/acme/api/issues/42","body":"+1"}]`
			},
		})
		changes := &cursorChanges{}
		before := time.Now()
		next, err := reader(t, url).Changes(ctx, githubSource(t, `{"repos":["acme/api"]}`), []Table{{Name: "issues"}, {Name: "issue_comments"}},
			json.RawMessage(`{"repositories":{"1":{"issues":"`+since+`","issue_comments":"`+since+`"}}}`), changes)
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		issues, comments := changes.upserts["issues"], changes.upserts["issue_comments"]
		if len(issues) != 1 || issues[0]["issue_key"] != "1#101" || len(comments) != 1 || comments[0]["issue_key"] != "1#42" {
			t.Fatalf("upserts = %v", changes.upserts)
		}
		var cursor githubCursor
		if err := json.Unmarshal(next, &cursor); err != nil {
			t.Fatal(err)
		}
		assertTimeCursor(t, cursor.Repositories["1"]["issues"], before, after)
		assertTimeCursor(t, cursor.Repositories["1"]["issue_comments"], before, after)
	})
	// An upsert replaces the stored row, so a changed row must be exactly what a full read would have stored.
	t.Run("writes rows exactly as a full read writes them", func(t *testing.T) {
		recent := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/repos/acme/api": repo,
			"/repos/acme/api/issues": func(*http.Request) (int, string) {
				return http.StatusOK, `[{"id":2000,"number":8,"title":"Fix login","comments_url":"x","user":{"id":7,"login":"ada"},
					"labels":[{"id":3,"name":"bug"}],"milestone":{"id":9},"assignees":[{"login":"bob"}],"pull_request":{"url":"x"}}]`
			},
			"/repos/acme/api/pulls": func(*http.Request) (int, string) {
				return http.StatusOK, fmt.Sprintf(`[{"id":300,"number":8,"updated_at":%q,"head":{"ref":"fix","sha":"a1"},
					"base":{"ref":"main","sha":"b2"},"requested_reviewers":[{"login":"cy"}],"merged_by":null}]`, recent)
			},
			"/repos/acme/api/issues/comments": func(*http.Request) (int, string) {
				return http.StatusOK, `[{"id":5,"issue_url":"https://api.github.com/repos/acme/api/issues/8","user":{"id":7,"login":"ada"}}]`
			},
			"/repos/acme/api/pulls/8/reviews": func(*http.Request) (int, string) {
				return http.StatusOK, `[{"id":11,"state":"APPROVED","user":{"id":7,"login":"ada"},"pull_request_url":"x"}]`
			},
		})
		src := githubSource(t, `{"repos":["acme/api"]}`)
		tables := []Table{{Name: "issues"}, {Name: "pull_requests"}, {Name: "issue_comments"}, {Name: "pull_request_reviews"}}
		since := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
		cursor := fmt.Sprintf(`{"repositories":{"1":{"issues":%[1]q,"pull_requests":%[1]q,"issue_comments":%[1]q,"pull_request_reviews":%[1]q}}}`, since)
		changes := &cursorChanges{}
		if _, err := reader(t, url).Changes(ctx, src, tables, json.RawMessage(cursor), changes); err != nil {
			t.Fatal(err)
		}
		for _, table := range tables {
			sink := &rowSink{}
			if _, err := kindOf(t, catalogFor(url), "github").Sync(ctx, src, table, sink); err != nil {
				t.Fatal(err)
			}
			if len(sink.rows) != 1 || fmt.Sprint(changes.upserts[table.Name]) != fmt.Sprint(sink.rows) {
				t.Errorf("%s: change rows = %v, full rows = %v", table.Name, changes.upserts[table.Name], sink.rows)
			}
		}
	})
	// Pull requests have no since filter; listed newest update first, they must stop at the cursor rather than page through every one.
	t.Run("stops listing pull requests at the first one older than the cursor", func(t *testing.T) {
		since := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/repos/acme/api": repo,
			"/repos/acme/api/pulls": func(r *http.Request) (int, string) {
				if q := r.URL.Query(); q.Get("sort") != "updated" || q.Get("direction") != "desc" || q.Get("page") != "1" {
					t.Errorf("query = %s", r.URL.RawQuery)
				}
				var rows []string
				for number := 100; number > 0; number-- {
					// Numbers above 50 were updated after the cursor, the rest before it.
					updated := since.Add(time.Duration(number-50)*time.Minute - time.Second)
					rows = append(rows, fmt.Sprintf(`{"id":%d,"number":%d,"updated_at":%q}`, number, number, updated.Format(time.RFC3339)))
				}
				return http.StatusOK, "[" + strings.Join(rows, ",") + "]"
			},
		})
		changes := &cursorChanges{}
		cursor := fmt.Sprintf(`{"repositories":{"1":{"pull_requests":%q}}}`, since.Format(time.RFC3339))
		if _, err := reader(t, url).Changes(ctx, githubSource(t, `{"repos":["acme/api"]}`), []Table{{Name: "pull_requests"}},
			json.RawMessage(cursor), changes); err != nil {
			t.Fatal(err)
		}
		pulls := changes.upserts["pull_requests"]
		if len(pulls) != 50 || pulls[49]["issue_key"] != "1#51" {
			t.Fatalf("upserted %d pull requests: %v", len(pulls), pulls)
		}
	})
	// Reviews are listed one pull request at a time, so only pull requests updated since the cursor have theirs read.
	t.Run("reads reviews only of pull requests updated since the cursor", func(t *testing.T) {
		since := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/repos/acme/api": repo,
			"/repos/acme/api/pulls": func(*http.Request) (int, string) {
				return http.StatusOK, fmt.Sprintf(`[{"number":8,"updated_at":%q},{"number":3,"updated_at":%q}]`,
					since.Add(24*time.Hour).Format(time.RFC3339), since.Add(-24*time.Hour).Format(time.RFC3339))
			},
			"/repos/acme/api/pulls/8/reviews": func(*http.Request) (int, string) {
				return http.StatusOK, `[{"id":11,"state":"APPROVED","user":{"id":7,"login":"ada"}}]`
			},
		})
		changes := &cursorChanges{}
		cursor := fmt.Sprintf(`{"repositories":{"1":{"pull_request_reviews":%q}}}`, since.Format(time.RFC3339))
		if _, err := reader(t, url).Changes(ctx, githubSource(t, `{"repos":["acme/api"]}`), []Table{{Name: "pull_request_reviews"}},
			json.RawMessage(cursor), changes); err != nil {
			t.Fatal(err)
		}
		want := []map[string]any{{"review_id": 11.0, "issue_key": "1#8", "repository_id": 1.0, "state": "APPROVED", "user_id": 7.0, "user_login": "ada"}}
		if fmt.Sprint(changes.upserts["pull_request_reviews"]) != fmt.Sprint(want) || changes.upserts["pull_requests"] != nil {
			t.Fatalf("upserts = %v", changes.upserts)
		}
	})
	// The reviews table holds pull requests updated within review_days only; a cursor older than that must not reach past it.
	t.Run("keeps reviews within review days when the cursor is older", func(t *testing.T) {
		now := time.Now()
		since := now.AddDate(0, 0, -40).UTC().Truncate(time.Second)
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/repos/acme/api": repo,
			"/repos/acme/api/pulls": func(*http.Request) (int, string) {
				return http.StatusOK, fmt.Sprintf(`[{"number":8,"updated_at":%q},{"number":5,"updated_at":%q}]`,
					now.AddDate(0, 0, -1).Format(time.RFC3339), now.AddDate(0, 0, -35).Format(time.RFC3339))
			},
			"/repos/acme/api/pulls/8/reviews": func(*http.Request) (int, string) { return http.StatusOK, `[{"id":11}]` },
		})
		changes := &cursorChanges{}
		cursor := fmt.Sprintf(`{"repositories":{"1":{"pull_requests":%[1]q,"pull_request_reviews":%[1]q}}}`, since.Format(time.RFC3339))
		if _, err := reader(t, url).Changes(ctx, githubSource(t, `{"repos":["acme/api"],"review_days":30}`),
			[]Table{{Name: "pull_requests"}, {Name: "pull_request_reviews"}}, json.RawMessage(cursor), changes); err != nil {
			t.Fatal(err)
		}
		if len(changes.upserts["pull_requests"]) != 2 || len(changes.upserts["pull_request_reviews"]) != 1 {
			t.Fatalf("upserts = %v", changes.upserts)
		}
	})
	// A repository with no cursor time, such as one just added to the settings, has nowhere to read changes from.
	t.Run("reads every followed table in full when a repository is new to the cursor", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/repos/acme/api": repo,
			"/repos/acme/web": func(*http.Request) (int, string) { return http.StatusOK, `{"id":2,"full_name":"acme/web"}` },
		})
		changes := &cursorChanges{}
		before := time.Now()
		next, err := reader(t, url).Changes(ctx, githubSource(t, `{"repos":["acme/api","acme/web"]}`), []Table{{Name: "issues"}},
			json.RawMessage(`{"repositories":{"1":{"issues":"2026-10-20T08:00:00Z"}}}`), changes)
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(changes.resets, []string{"issues"}) || len(changes.upserts) != 0 {
			t.Fatalf("resets = %v, upserts = %v", changes.resets, changes.upserts)
		}
		var cursor githubCursor
		if err := json.Unmarshal(next, &cursor); err != nil {
			t.Fatal(err)
		}
		assertTimeCursor(t, cursor.Repositories["1"]["issues"], before, after)
		assertTimeCursor(t, cursor.Repositories["2"]["issues"], before, after)
	})
	// A cursor the reader can't decode leaves no point to resume from, so the syncer reads every table in full.
	t.Run("asks for a full read when the cursor is unreadable", func(t *testing.T) {
		_, err := reader(t, "http://unused").Changes(ctx, githubSource(t, `{"repos":["acme/api"]}`), []Table{{Name: "issues"}},
			json.RawMessage(`"2026-10-20"`), &cursorChanges{})
		if !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("Changes error = %v, want ErrCursorExpired", err)
		}
	})
}
