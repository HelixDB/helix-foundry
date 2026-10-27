package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

const githubTestToken = "github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz"

func githubSource(t *testing.T, settings string) Source {
	t.Helper()
	prepared, err := kindOf(t, catalogFor("http://unused"), "github").Prepare(Source{Secret: githubTestToken, Settings: json.RawMessage(settings)})
	if err != nil {
		t.Fatal(err)
	}
	return Source{Secret: githubTestToken, Settings: prepared}
}

func TestGitHubConnector(t *testing.T) {
	t.Run("rejects something that is not a github token", func(t *testing.T) {
		_, err := kindOf(t, catalogFor("http://unused"), "github").Prepare(Source{Secret: "sk_live_abc"})
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) {
			t.Fatalf("Prepare error = %v, want InvalidError", err)
		}
	})
	t.Run("keeps each repository once and defaults review days", func(t *testing.T) {
		settings := githubSource(t, `{"repos":[" acme/api ","Acme/API","acme/web"]}`).Settings
		if string(settings) != `{"repos":["acme/api","acme/web"],"review_days":90}` {
			t.Fatalf("settings = %s", settings)
		}
	})
	t.Run("refuses a repository not written as owner/name", func(t *testing.T) {
		_, err := kindOf(t, catalogFor("http://unused"), "github").Prepare(Source{Secret: githubTestToken, Settings: json.RawMessage(`{"repos":["api"]}`)})
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) {
			t.Fatalf("Prepare error = %v, want InvalidError", err)
		}
	})
	t.Run("refuses review days past a year", func(t *testing.T) {
		_, err := kindOf(t, catalogFor("http://unused"), "github").Prepare(Source{Secret: githubTestToken, Settings: json.RawMessage(`{"review_days":400}`)})
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) {
			t.Fatalf("Prepare error = %v, want InvalidError", err)
		}
	})
	// A fine-grained token reads pull requests under their own permission; without it they are reported, not fatal.
	t.Run("reports objects the token's permissions leave out", func(t *testing.T) {
		api := newAPIServer(t, map[string]int{"/repos/acme/api/pulls": http.StatusForbidden},
			map[string]string{"/repos/acme/api": `{"id":1,"full_name":"acme/api"}`})
		d, err := kindOf(t, catalogFor(api.url), "github").Discover(context.Background(), githubSource(t, `{"repos":["acme/api"]}`))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(d.Unavailable, []Unavailable{{Name: "pull_requests"}, {Name: "pull_request_reviews"}}) {
			t.Fatalf("unavailable = %v", d.Unavailable)
		}
		issues := findTable(t, d, "issues")
		if !slices.Equal(issues.PrimaryKey, []string{"issue_key"}) || len(issues.ForeignKeys) != 3 {
			t.Fatalf("issues = %+v", issues)
		}
		comments := findTable(t, d, "issue_comments")
		if comments.ForeignKeys[0] != (ForeignKey{Column: "issue_key", ReferencedTable: "issues", ReferencedColumn: "issue_key"}) {
			t.Fatalf("issue_comments = %+v", comments)
		}
	})
	t.Run("refuses a chosen repository the token cannot read", func(t *testing.T) {
		api := newAPIServer(t, map[string]int{"/repos/acme/secret": http.StatusNotFound}, nil)
		_, err := kindOf(t, catalogFor(api.url), "github").Discover(context.Background(), githubSource(t, `{"repos":["acme/secret"]}`))
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) || !strings.Contains(invalidErr.Message, "acme/secret") {
			t.Fatalf("Discover error = %v, want InvalidError naming the repository", err)
		}
	})
	// Without a list, every repository the token reads is synced, so a token that reads too many must name them.
	t.Run("asks for a list when the token reads too many repositories", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/user": func(*http.Request) (int, string) { return http.StatusOK, `{}` },
			"/user/repos": func(r *http.Request) (int, string) {
				var rows []string
				for i := 0; i < 100; i++ {
					rows = append(rows, fmt.Sprintf(`{"id":%s%d,"full_name":"acme/r%d"}`, r.URL.Query().Get("page"), i, i))
				}
				return http.StatusOK, "[" + strings.Join(rows, ",") + "]"
			},
		})
		_, err := kindOf(t, catalogFor(url), "github").Discover(context.Background(), githubSource(t, `{}`))
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) {
			t.Fatalf("Discover error = %v, want InvalidError", err)
		}
	})
	// A spent rate limit answers 403; it must read as a wait, not as a missing permission.
	t.Run("reports a spent rate limit with its reset time", func(t *testing.T) {
		reset := time.Date(2026, 10, 21, 14, 5, 0, 0, time.UTC)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset.Unix()))
			w.WriteHeader(http.StatusForbidden)
		}))
		t.Cleanup(server.Close)
		_, err := kindOf(t, catalogFor(server.URL), "github").Discover(context.Background(), githubSource(t, `{"repos":["acme/api"]}`))
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || !strings.Contains(refusedErr.Message, "resets at 14:05 UTC") {
			t.Fatalf("Discover error = %v, want the reset time", err)
		}
	})
	// A secondary limit that outlasts the retries is refused for a later retry; marking
	// the table unavailable would fail the sync for good.
	for name, limit := range map[string]func(http.ResponseWriter){
		"with Retry-After": func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusForbidden)
		},
		"by its message": func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit."}`))
		},
	} {
		t.Run("refuses a secondary rate limit announced "+name, func(t *testing.T) {
			noBackoff(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/acme/api":
					_, _ = w.Write([]byte(`{"id":1,"full_name":"acme/api"}`))
				case "/repos/acme/api/pulls":
					limit(w)
				default:
					_, _ = w.Write([]byte(`[]`))
				}
			}))
			t.Cleanup(server.Close)
			_, err := kindOf(t, catalogFor(server.URL), "github").Discover(context.Background(), githubSource(t, `{"repos":["acme/api"]}`))
			var refusedErr *RefusedError
			if !errors.As(err, &refusedErr) || !strings.Contains(refusedErr.Message, "limiting how fast") {
				t.Fatalf("Discover error = %v, want a secondary rate limit", err)
			}
		})
	}
}

func TestGitHubSync(t *testing.T) {
	repo := func(*http.Request) (int, string) { return http.StatusOK, `{"id":1,"full_name":"acme/api"}` }

	// Pages are numbered and the last is short; a pull request also lists as an issue.
	t.Run("pages issues and keys them by repository and number", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/repos/acme/api": repo,
			"/repos/acme/api/issues": func(r *http.Request) (int, string) {
				if r.URL.Query().Get("state") != "all" {
					t.Errorf("query = %s", r.URL.RawQuery)
				}
				if r.URL.Query().Get("page") == "1" {
					var rows []string
					for i := 1; i <= 100; i++ {
						rows = append(rows, fmt.Sprintf(`{"id":%d,"number":%d,"title":"t"}`, 1000+i, i))
					}
					return http.StatusOK, "[" + strings.Join(rows, ",") + "]"
				}
				return http.StatusOK, `[{"id":2000,"number":101,"title":"Fix login","comments_url":"x","html_url":"h",
					"user":{"id":7,"login":"ada"},"labels":[{"id":3,"name":"bug"}],"milestone":{"id":9},
					"assignees":[{"login":"bob"}],"pull_request":{"url":"x"}}]`
			},
		})
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "github").Sync(context.Background(), githubSource(t, `{"repos":["acme/api"]}`), Table{Name: "issues"}, sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.rows) != 101 {
			t.Fatalf("rows = %d", len(sink.rows))
		}
		last := sink.rows[100]
		want := map[string]any{
			"issue_key": "1#101", "issue_id": 2000.0, "repository_id": 1.0, "number": 101.0, "title": "Fix login",
			"html_url": "h", "user_id": 7.0, "user_login": "ada", "label_ids": []any{3.0}, "label_names": []any{"bug"},
			"milestone_id": 9.0, "assignee_logins": []any{"bob"}, "is_pull_request": true,
		}
		if fmt.Sprint(last) != fmt.Sprint(want) {
			t.Fatalf("row = %v", last)
		}
	})
	t.Run("ties a comment to its issue through the issue url", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/repos/acme/api": repo,
			"/repos/acme/api/issues/comments": func(*http.Request) (int, string) {
				return http.StatusOK, `[{"id":5,"issue_url":"https://api.github.com/repos/acme/api/issues/42","body":"+1","user":{"id":7,"login":"ada"}}]`
			},
		})
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "github").Sync(context.Background(), githubSource(t, `{"repos":["acme/api"]}`), Table{Name: "issue_comments"}, sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.rows) != 1 || sink.rows[0]["issue_key"] != "1#42" || sink.rows[0]["comment_id"] != 5.0 {
			t.Fatalf("rows = %v", sink.rows)
		}
	})
	// Reviews are listed per pull request, so only recently updated ones are read.
	t.Run("reads reviews of pull requests updated within review days", func(t *testing.T) {
		recent, old := time.Now().Add(-24*time.Hour).Format(time.RFC3339), time.Now().AddDate(0, 0, -40).Format(time.RFC3339)
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/repos/acme/api": repo,
			"/repos/acme/api/pulls": func(r *http.Request) (int, string) {
				if r.URL.Query().Get("sort") != "updated" || r.URL.Query().Get("direction") != "desc" {
					t.Errorf("query = %s", r.URL.RawQuery)
				}
				return http.StatusOK, fmt.Sprintf(`[{"number":8,"updated_at":%q},{"number":3,"updated_at":%q}]`, recent, old)
			},
			"/repos/acme/api/pulls/8/reviews": func(*http.Request) (int, string) {
				return http.StatusOK, `[{"id":11,"state":"APPROVED","user":{"id":7,"login":"ada"},"pull_request_url":"x"}]`
			},
		})
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "github").Sync(context.Background(), githubSource(t, `{"repos":["acme/api"],"review_days":30}`), Table{Name: "pull_request_reviews"}, sink); err != nil {
			t.Fatal(err)
		}
		want := []map[string]any{{"review_id": 11.0, "issue_key": "1#8", "repository_id": 1.0, "state": "APPROVED", "user_id": 7.0, "user_login": "ada"}}
		if fmt.Sprint(sink.rows) != fmt.Sprint(want) {
			t.Fatalf("rows = %v", sink.rows)
		}
	})
	t.Run("lists no issues for a repository that turned them off", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/repos/acme/api":        repo,
			"/repos/acme/api/issues": func(*http.Request) (int, string) { return http.StatusGone, `{"message":"Issues are disabled"}` },
		})
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "github").Sync(context.Background(), githubSource(t, `{"repos":["acme/api"]}`), Table{Name: "issues"}, sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.rows) != 0 {
			t.Fatalf("rows = %v", sink.rows)
		}
	})
	t.Run("writes a person assignable in two repositories once", func(t *testing.T) {
		assignees := func(*http.Request) (int, string) { return http.StatusOK, `[{"id":7,"login":"ada","avatar_url":"x"}]` }
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/repos/acme/api":           repo,
			"/repos/acme/web":           func(*http.Request) (int, string) { return http.StatusOK, `{"id":2,"full_name":"acme/web"}` },
			"/repos/acme/api/assignees": assignees,
			"/repos/acme/web/assignees": assignees,
		})
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "github").Sync(context.Background(), githubSource(t, `{"repos":["acme/api","acme/web"]}`), Table{Name: "users"}, sink); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(sink.rows) != fmt.Sprint([]map[string]any{{"user_id": 7.0, "login": "ada"}}) {
			t.Fatalf("rows = %v", sink.rows)
		}
	})
	// GitHub answers a renamed repository's old name with a redirect to its id.
	t.Run("follows a renamed repository to its new name", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/acme/old", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/repositories/1", http.StatusMovedPermanently)
		})
		mux.HandleFunc("/repositories/1", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+githubTestToken {
				t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"id":1,"full_name":"acme/new"}`))
		})
		mux.HandleFunc("/repos/acme/new/labels", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[{"id":3,"name":"bug"}]`))
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(server.URL), "github").Sync(context.Background(), githubSource(t, `{"repos":["acme/old"]}`), Table{Name: "labels"}, sink); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(sink.rows) != fmt.Sprint([]map[string]any{{"label_id": 3.0, "name": "bug", "repository_id": 1.0}}) {
			t.Fatalf("rows = %v", sink.rows)
		}
	})
	t.Run("never follows a redirect to another host", func(t *testing.T) {
		elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("the token followed a redirect to %s", r.URL)
		}))
		t.Cleanup(elsewhere.Close)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusMovedPermanently)
		}))
		t.Cleanup(server.Close)
		_, err := kindOf(t, catalogFor(server.URL), "github").Sync(context.Background(), githubSource(t, `{"repos":["acme/api"]}`), Table{Name: "labels"}, &rowSink{})
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) {
			t.Fatalf("Sync error = %v, want a refusal", err)
		}
	})
}
