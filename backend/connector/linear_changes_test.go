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

func TestLinearChanges(t *testing.T) {
	ctx := context.Background()
	reader := func(t *testing.T, url string) ChangeReader {
		t.Helper()
		return kindOf(t, catalogFor(url), "linear").(ChangeReader)
	}

	// Every root connection filters on updatedAt, so no Linear table needs a full read between reconciles.
	t.Run("follows every table", func(t *testing.T) {
		r := reader(t, "http://unused")
		for _, o := range linearObjects {
			if got := r.Capture(Source{}, o.name); got != CaptureCursor {
				t.Errorf("Capture(%s) = %s, want %s", o.name, got, CaptureCursor)
			}
		}
	})
	// The cursor is taken before the full read, so a change made during that read is read again by the next change sync.
	t.Run("starts each table before the full read, less the overlap", func(t *testing.T) {
		before := time.Now()
		start, err := reader(t, "http://unused").Start(ctx, Source{Secret: linearTestKey}, []Table{{Name: "issues"}, {Name: "comments"}})
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		var cursor linearCursor
		if err := json.Unmarshal(start.Cursor, &cursor); err != nil {
			t.Fatal(err)
		}
		if len(cursor.Tables) != 2 {
			t.Fatalf("cursor = %s", start.Cursor)
		}
		assertTimeCursor(t, cursor.Tables["issues"], before, after)
		assertTimeCursor(t, cursor.Tables["comments"], before, after)
	})
	// Only nodes updated after the cursor are read, archived ones included so an archive is seen; the next read starts where this one began.
	t.Run("reads nodes updated after the cursor page by page", func(t *testing.T) {
		since := "2026-10-20T08:00:00Z"
		var afters []any
		url := linearServer(t, func(query string, variables map[string]any) (int, string) {
			if !strings.Contains(query, "query($after: String, $since: DateTimeOrDuration!)") ||
				!strings.Contains(query, "issues(first: 100, after: $after, includeArchived: true, orderBy: updatedAt, filter: {updatedAt: {gt: $since}})") ||
				variables["since"] != since {
				t.Errorf("query = %s, variables = %v", query, variables)
			}
			afters = append(afters, variables["after"])
			if variables["after"] == nil {
				return http.StatusOK, `{"data":{"issues":{"nodes":[{"id":"i1"}],"pageInfo":{"hasNextPage":true,"endCursor":"c1"}}}}`
			}
			return http.StatusOK, `{"data":{"issues":{"nodes":[{"id":"i2"}],"pageInfo":{"hasNextPage":false}}}}`
		})
		changes := &cursorChanges{}
		before := time.Now()
		next, err := reader(t, url).Changes(ctx, Source{Secret: linearTestKey}, []Table{{Name: "issues"}},
			json.RawMessage(`{"tables":{"issues":"`+since+`"}}`), changes)
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(afters) != "[<nil> c1]" || fmt.Sprint(changes.upserts["issues"]) != "[map[issue_id:i1] map[issue_id:i2]]" {
			t.Fatalf("afters = %v, upserts = %v", afters, changes.upserts)
		}
		var cursor linearCursor
		if err := json.Unmarshal(next, &cursor); err != nil {
			t.Fatal(err)
		}
		assertTimeCursor(t, cursor.Tables["issues"], before, after)
	})
	// An upsert replaces the stored row, so a changed node must be exactly what a full read would have stored.
	t.Run("writes rows exactly as a full read writes them", func(t *testing.T) {
		url := linearServer(t, func(string, map[string]any) (int, string) {
			return http.StatusOK, `{"data":{"issues":{"nodes":[{"id":"i1","identifier":"ENG-1","labelIds":["l1"],"state":{"id":"s1"},
				"assignee":null,"team":{"id":"t1"},"archivedAt":"2026-10-21T00:00:00.000Z"}],"pageInfo":{"hasNextPage":false}}}}`
		})
		changes := &cursorChanges{}
		if _, err := reader(t, url).Changes(ctx, Source{Secret: linearTestKey}, []Table{{Name: "issues"}},
			json.RawMessage(`{"tables":{"issues":"2026-10-20T08:00:00Z"}}`), changes); err != nil {
			t.Fatal(err)
		}
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "linear").Sync(ctx, Source{Secret: linearTestKey}, Table{Name: "issues"}, sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.rows) != 1 || fmt.Sprint(changes.upserts["issues"]) != fmt.Sprint(sink.rows) {
			t.Fatalf("change rows = %v, full rows = %v", changes.upserts["issues"], sink.rows)
		}
	})
	// A table with no cursor time has nowhere to read changes from, so it is read in full this sync and followed from the next.
	t.Run("reads a table the cursor has no time for in full", func(t *testing.T) {
		url := linearServer(t, func(query string, _ map[string]any) (int, string) {
			if !strings.Contains(query, "issues(") {
				t.Errorf("query = %s", query)
			}
			return http.StatusOK, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false}}}}`
		})
		changes := &cursorChanges{}
		next, err := reader(t, url).Changes(ctx, Source{Secret: linearTestKey}, []Table{{Name: "issues"}, {Name: "comments"}},
			json.RawMessage(`{"tables":{"issues":"2026-10-20T08:00:00Z"}}`), changes)
		if err != nil {
			t.Fatal(err)
		}
		var cursor linearCursor
		if err := json.Unmarshal(next, &cursor); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(changes.resets, []string{"comments"}) || len(cursor.Tables) != 2 {
			t.Fatalf("resets = %v, cursor = %s", changes.resets, next)
		}
	})
	// A cursor the reader can't decode leaves no point to resume from, so the syncer reads every table in full.
	t.Run("asks for a full read when the cursor is unreadable", func(t *testing.T) {
		_, err := reader(t, "http://unused").Changes(ctx, Source{Secret: linearTestKey}, []Table{{Name: "issues"}},
			json.RawMessage(`"2026-10-20"`), &cursorChanges{})
		if !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("Changes error = %v, want ErrCursorExpired", err)
		}
	})
}
