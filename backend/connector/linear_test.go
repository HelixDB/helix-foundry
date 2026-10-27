package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

const linearTestKey = "lin_api_0123456789abcdefghij"

// linearServer answers GraphQL posts with answer, given the query and its variables.
func linearServer(t *testing.T, answer func(query string, variables map[string]any) (int, string)) string {
	t.Helper()
	return routeServer(t, map[string]func(*http.Request) (int, string){
		"/graphql": func(r *http.Request) (int, string) {
			if r.Header.Get("Authorization") != linearTestKey {
				t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			}
			var body struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			return answer(body.Query, body.Variables)
		},
	})
}

func TestLinearConnector(t *testing.T) {
	t.Run("rejects something that is not a linear key", func(t *testing.T) {
		_, err := kindOf(t, catalogFor("http://unused"), "linear").Prepare(Source{Secret: "lin_oauth_abc"})
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) {
			t.Fatalf("Prepare error = %v, want InvalidError", err)
		}
	})
	t.Run("lists every object once the key works", func(t *testing.T) {
		url := linearServer(t, func(string, map[string]any) (int, string) {
			return http.StatusOK, `{"data":{"viewer":{"id":"u1"}}}`
		})
		d, err := kindOf(t, catalogFor(url), "linear").Discover(context.Background(), Source{Secret: linearTestKey})
		if err != nil {
			t.Fatal(err)
		}
		if names := tableNames(d); len(names) != 8 || len(d.Unavailable) != 0 {
			t.Fatalf("tables = %v, unavailable = %v", names, d.Unavailable)
		}
		issues := findTable(t, d, "issues")
		if len(issues.ForeignKeys) != 7 || issues.ForeignKeys[2] != (ForeignKey{Column: "assignee_id", ReferencedTable: "users", ReferencedColumn: "user_id"}) {
			t.Fatalf("issues = %+v", issues)
		}
	})
	// Linear answers a bad key with a GraphQL error, not an HTTP 401.
	t.Run("reports a rejected key", func(t *testing.T) {
		url := linearServer(t, func(string, map[string]any) (int, string) {
			return http.StatusBadRequest, `{"errors":[{"message":"Authentication required","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`
		})
		_, err := kindOf(t, catalogFor(url), "linear").Discover(context.Background(), Source{Secret: linearTestKey})
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || !strings.Contains(refusedErr.Message, "rejected this key") {
			t.Fatalf("Discover error = %v, want a rejected key", err)
		}
	})
}

func TestLinearSync(t *testing.T) {
	t.Run("pages issues by cursor and keeps nested ids", func(t *testing.T) {
		var afters []any
		url := linearServer(t, func(query string, variables map[string]any) (int, string) {
			if !strings.Contains(query, "issues(first: 100, after: $after, includeArchived: true, orderBy: createdAt)") {
				t.Errorf("query = %s", query)
			}
			afters = append(afters, variables["after"])
			if variables["after"] == nil {
				return http.StatusOK, `{"data":{"issues":{"nodes":[{"id":"i1","identifier":"ENG-1","labelIds":["l1"],
					"state":{"id":"s1"},"assignee":null,"team":{"id":"t1"}}],"pageInfo":{"hasNextPage":true,"endCursor":"c1"}}}}`
			}
			return http.StatusOK, `{"data":{"issues":{"nodes":[{"id":"i2","identifier":"ENG-2"}],"pageInfo":{"hasNextPage":false,"endCursor":"c2"}}}}`
		})
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "linear").Sync(context.Background(), Source{Secret: linearTestKey}, Table{Name: "issues"}, sink); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(afters) != "[<nil> c1]" || len(sink.rows) != 2 {
			t.Fatalf("afters = %v, rows = %v", afters, sink.rows)
		}
		want := map[string]any{"issue_id": "i1", "identifier": "ENG-1", "label_ids": []any{"l1"},
			"workflow_state_id": "s1", "assignee_id": nil, "team_id": "t1"}
		if fmt.Sprint(sink.rows[0]) != fmt.Sprint(want) {
			t.Fatalf("row = %v", sink.rows[0])
		}
	})
	t.Run("keeps a project's status as its name and type", func(t *testing.T) {
		url := linearServer(t, func(string, map[string]any) (int, string) {
			return http.StatusOK, `{"data":{"projects":{"nodes":[{"id":"p1","status":{"name":"In Progress","type":"started"},"lead":{"id":"u1"}}],
				"pageInfo":{"hasNextPage":false}}}}`
		})
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "linear").Sync(context.Background(), Source{Secret: linearTestKey}, Table{Name: "projects"}, sink); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"project_id": "p1", "status_name": "In Progress", "status_type": "started", "lead_id": "u1"}
		if fmt.Sprint(sink.rows[0]) != fmt.Sprint(want) {
			t.Fatalf("row = %v", sink.rows[0])
		}
	})
	// Ending early on an incomplete page would replace the snapshot with part of the table.
	for name, response := range map[string]string{
		"lacks its connection":                 `{"data":{}}`,
		"lacks its page info":                  `{"data":{"teams":{"nodes":[{"id":"t1"}]}}}`,
		"says more follow but gives no cursor": `{"data":{"teams":{"nodes":[{"id":"t1"}],"pageInfo":{"hasNextPage":true}}}}`,
	} {
		t.Run("fails a page that "+name, func(t *testing.T) {
			url := linearServer(t, func(string, map[string]any) (int, string) { return http.StatusOK, response })
			sink := &rowSink{}
			_, err := kindOf(t, catalogFor(url), "linear").Sync(context.Background(), Source{Secret: linearTestKey}, Table{Name: "teams"}, sink)
			if err == nil || len(sink.rows) != 0 {
				t.Fatalf("Sync error = %v, rows = %v; want a failure before any row", err, sink.rows)
			}
		})
	}
	// A spent rate limit fails the attempt with a message the syncer retries on.
	t.Run("reports a spent rate limit", func(t *testing.T) {
		url := linearServer(t, func(string, map[string]any) (int, string) {
			return http.StatusBadRequest, `{"errors":[{"message":"Rate limit exceeded","extensions":{"code":"RATELIMITED"}}]}`
		})
		_, err := kindOf(t, catalogFor(url), "linear").Sync(context.Background(), Source{Secret: linearTestKey}, Table{Name: "teams"}, &rowSink{})
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || !strings.Contains(refusedErr.Message, "rate limit") {
			t.Fatalf("Sync error = %v, want a rate limit", err)
		}
	})
}
