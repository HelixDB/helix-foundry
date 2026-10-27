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

// granolaChanges is cursorChanges that also takes deletes and cascades, and
// logs every change in the order it was written. A set fail refuses every change.
type granolaChanges struct {
	cursorChanges
	log  []string
	fail error
}

func (c *granolaChanges) Upsert(table string, row map[string]any) error {
	c.log = append(c.log, fmt.Sprintf("upsert %s %v", table, row["note_id"]))
	if c.fail != nil {
		return c.fail
	}
	return c.cursorChanges.Upsert(table, row)
}

func (c *granolaChanges) Delete(table string, key map[string]any) error {
	c.log = append(c.log, fmt.Sprintf("delete %s %v", table, key))
	return c.fail
}

func (c *granolaChanges) DeleteWhere(table, column string, value any) error {
	c.log = append(c.log, fmt.Sprintf("cascade %s %s=%v", table, column, value))
	return c.fail
}

// granolaDocChanges is granolaChanges that takes documents as granolaDocs does.
type granolaDocChanges struct {
	*granolaDocs
	granolaChanges
}

func TestGranolaChanges(t *testing.T) {
	ctx := context.Background()
	src := Source{Secret: granolaTestKey}
	noteTables := []Table{{Name: "notes"}, {Name: "note_attendees"}, {Name: "note_folders"}}
	since := "2026-10-20T08:00:00Z"
	cursorFor := func(tables ...string) json.RawMessage {
		c := granolaCursor{Tables: map[string]time.Time{}}
		for _, table := range tables {
			c.Tables[table], _ = time.Parse(time.RFC3339, since)
		}
		b, _ := json.Marshal(c)
		return b
	}
	getNote := func(id string) func(*http.Request) (int, string) {
		return func(*http.Request) (int, string) { return http.StatusOK, granolaNote(id) }
	}

	// Only notes can be listed by update time; folders and spaces are small and read in full.
	t.Run("follows notes and their attendees and folders only", func(t *testing.T) {
		want := map[string]Capture{
			"notes": CaptureCursor, "note_attendees": CaptureCursor, "note_folders": CaptureCursor,
			"folders": CaptureFull, "spaces": CaptureFull,
		}
		kind := granolaAt(t, nil)
		for table, capture := range want {
			if got := kind.Capture(src, table); got != capture {
				t.Errorf("Capture(%s) = %s, want %s", table, got, capture)
			}
		}
	})
	// The cursor is taken before the full read, so a change made during that read is read again by the next change sync.
	t.Run("starts each table before the full read, less the overlap", func(t *testing.T) {
		before := time.Now()
		start, err := granolaAt(t, nil).Start(ctx, src, noteTables)
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		var cursor granolaCursor
		if err := json.Unmarshal(start.Cursor, &cursor); err != nil {
			t.Fatal(err)
		}
		if len(cursor.Tables) != 3 {
			t.Fatalf("cursor = %s", start.Cursor)
		}
		for _, at := range cursor.Tables {
			assertTimeCursor(t, at, before, after)
		}
	})
	// One list serves every note table, each changed note is read once, and its
	// attendee and folder rows replace the ones it had.
	t.Run("reads notes updated after the cursor and replaces their attendees and folders", func(t *testing.T) {
		older := "2026-10-19T08:00:00Z"
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": func(r *http.Request) (int, string) {
				if q := r.URL.Query(); q.Get("updated_after") != older || q.Get("page_size") != "30" {
					t.Errorf("query = %s", r.URL.RawQuery)
				}
				return http.StatusOK, granolaList("not_1")
			},
			"/v1/notes/not_1": getNote("not_1"),
		})
		cursor := json.RawMessage(fmt.Sprintf(`{"tables":{"notes":%q,"note_attendees":%q,"note_folders":%q}}`, since, since, older))
		changes := &granolaChanges{}
		before := time.Now()
		next, err := kind.Changes(ctx, src, noteTables, cursor, changes)
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"upsert notes not_1",
			"cascade note_attendees note_id=not_1",
			"upsert note_attendees not_1", "upsert note_attendees not_1", "upsert note_attendees not_1",
			"cascade note_folders note_id=not_1",
			"upsert note_folders not_1", "upsert note_folders not_1",
		}
		if !slices.Equal(changes.log, want) || len(changes.resets) != 0 {
			t.Fatalf("changes = %q, resets = %v", changes.log, changes.resets)
		}
		var c granolaCursor
		if err := json.Unmarshal(next, &c); err != nil {
			t.Fatal(err)
		}
		if len(c.Tables) != 3 {
			t.Fatalf("cursor = %s", next)
		}
		for _, at := range c.Tables {
			assertTimeCursor(t, at, before, after)
		}
	})
	// An upsert replaces the stored row, so a changed note must be exactly what a full read would have stored.
	t.Run("writes rows exactly as a full read writes them", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1", "not_2") },
			"/v1/notes/not_1": getNote("not_1"), "/v1/notes/not_2": getNote("not_2"),
		})
		changes := &granolaChanges{}
		if _, err := kind.Changes(ctx, src, noteTables, cursorFor("notes", "note_attendees", "note_folders"), changes); err != nil {
			t.Fatal(err)
		}
		for _, table := range noteTables {
			sink := &rowSink{}
			if _, err := kind.Sync(ctx, src, table, sink); err != nil {
				t.Fatal(err)
			}
			if len(sink.rows) == 0 || fmt.Sprint(changes.upserts[table.Name]) != fmt.Sprint(sink.rows) {
				t.Errorf("%s: change rows = %v, full rows = %v", table.Name, changes.upserts[table.Name], sink.rows)
			}
		}
	})
	// A change read Puts what a full read would for the notes it upserts; the
	// syncer removes a deleted note's documents with its row.
	t.Run("puts the documents of the notes it upserts", func(t *testing.T) {
		var queries []string
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": func(*http.Request) (int, string) {
				return http.StatusOK, strings.Replace(granolaList("not_1", "not_2"), `]`,
					`,{"id":"not_3","deleted_at":"2026-10-20T09:00:00Z"}]`, 1)
			},
			"/v1/notes/not_1": func(r *http.Request) (int, string) {
				queries = append(queries, r.URL.RawQuery)
				return http.StatusOK, granolaTranscribed("not_1")
			},
			"/v1/notes/not_2": func(*http.Request) (int, string) { return http.StatusNotFound, `{}` },
		})
		withTranscripts := Source{Secret: granolaTestKey, Settings: json.RawMessage(`{"transcripts":true}`)}
		changes := &granolaDocChanges{granolaDocs: &granolaDocs{want: map[string]bool{"notes": true, "transcripts": true}}}
		if _, err := kind.Changes(ctx, withTranscripts, noteTables, cursorFor("notes", "note_attendees", "note_folders"), changes); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(changes.putNames(), []string{"notes not_1", "transcripts not_1"}) || !slices.Equal(queries, []string{"include=transcript"}) ||
			len(changes.offers) != 4 || changes.log[0] != "upsert notes not_1" {
			t.Fatalf("puts = %v, queries = %q, offers = %q, changes = %q", changes.putNames(), queries, changes.offers, changes.log)
		}
		if text := string(changes.puts[0].doc.Text); text != granolaHeader+"\n## Review\n\nIt went well.\n" {
			t.Fatalf("notes document = %s", text)
		}
	})
	// Documents belong to the notes table: without it followed, they wait for its full read.
	t.Run("offers no document while notes is not followed", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
			"/v1/notes/not_1": getNote("not_1"),
		})
		changes := &granolaDocChanges{granolaDocs: &granolaDocs{want: map[string]bool{"notes": true}}}
		if _, err := kind.Changes(ctx, src, noteTables, cursorFor("note_attendees", "note_folders"), changes); err != nil {
			t.Fatal(err)
		}
		if len(changes.offers) != 0 || !slices.Equal(changes.resets, []string{"notes"}) {
			t.Fatalf("offers = %q, resets = %v", changes.offers, changes.resets)
		}
	})
	// A full read leaves such notes out, so a change read removes them and their rows.
	t.Run("removes a note the list shows deleted or Get Note no longer finds", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": func(*http.Request) (int, string) {
				return http.StatusOK, `{"notes":[{"id":"not_2","created_at":"2026-10-20T08:30:00Z","updated_at":"2026-10-20T08:45:00Z","deleted_at":null},
					{"id":"not_3","deleted_at":"2026-10-20T09:00:00Z"}],"hasMore":false,"cursor":null}`
			},
			"/v1/notes/not_2": func(*http.Request) (int, string) { return http.StatusNotFound, `{}` },
		})
		changes := &granolaChanges{}
		if _, err := kind.Changes(ctx, src, noteTables, cursorFor("notes", "note_attendees", "note_folders"), changes); err != nil {
			t.Fatal(err)
		}
		want := []string{
			"delete notes map[note_id:not_2]", "cascade note_attendees note_id=not_2", "cascade note_folders note_id=not_2",
			"delete notes map[note_id:not_3]", "cascade note_attendees note_id=not_3", "cascade note_folders note_id=not_3",
		}
		if !slices.Equal(changes.log, want) {
			t.Fatalf("changes = %q", changes.log)
		}
	})
	// A table with no cursor time has nowhere to read changes from, so it is read in full this sync and followed from the next.
	t.Run("reads a table the cursor has no time for in full", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
			"/v1/notes/not_1": getNote("not_1"),
		})
		changes := &granolaChanges{}
		next, err := kind.Changes(ctx, src, noteTables, cursorFor("notes"), changes)
		if err != nil {
			t.Fatal(err)
		}
		var c granolaCursor
		if err := json.Unmarshal(next, &c); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(changes.resets, []string{"note_attendees", "note_folders"}) || !slices.Equal(changes.log, []string{"upsert notes not_1"}) || len(c.Tables) != 3 {
			t.Fatalf("resets = %v, changes = %q, cursor = %s", changes.resets, changes.log, next)
		}
	})
	t.Run("lists nothing when every table is reset", func(t *testing.T) {
		changes := &granolaChanges{}
		next, err := granolaAt(t, nil).Changes(ctx, src, noteTables[:1], json.RawMessage(`{"tables":{}}`), changes)
		if err != nil || !slices.Equal(changes.resets, []string{"notes"}) || len(next) == 0 {
			t.Fatalf("Changes = (%s, %v), resets = %v", next, err, changes.resets)
		}
	})
	// A spool that fails must fail the read at the first change it refuses.
	t.Run("stops at the writer's error", func(t *testing.T) {
		full := errors.New("disk full")
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
			"/v1/notes/not_1": getNote("not_1"),
		})
		// The first change is a notes upsert, or with notes not followed, an attendee cascade.
		for _, tables := range [][]Table{noteTables, noteTables[1:]} {
			changes := &granolaChanges{fail: full}
			if _, err := kind.Changes(ctx, src, tables, cursorFor("notes", "note_attendees", "note_folders"), changes); !errors.Is(err, full) || len(changes.log) != 1 {
				t.Fatalf("%v: Changes error = %v after %q, want the writer's at once", tables, err, changes.log)
			}
		}
		changes := &granolaChanges{fail: full}
		gone := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": func(*http.Request) (int, string) {
				return http.StatusOK, `{"notes":[{"id":"not_3","deleted_at":"2026-10-20T09:00:00Z"}],"hasMore":false,"cursor":null}`
			},
		})
		if _, err := gone.Changes(ctx, src, noteTables, cursorFor("notes"), changes); !errors.Is(err, full) || len(changes.log) != 1 {
			t.Fatalf("Changes error = %v after %q, want the writer's at once", err, changes.log)
		}
	})
	// A refusal mid-read must fail the read, not move the cursor past changes it never wrote.
	t.Run("fails the read when Granola refuses it", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": func(*http.Request) (int, string) { return http.StatusUnauthorized, `{}` },
		})
		next, err := kind.Changes(ctx, src, noteTables, cursorFor("notes", "note_attendees", "note_folders"), &granolaChanges{})
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || next != nil {
			t.Fatalf("Changes = (%s, %v), want a refusal", next, err)
		}
	})
	// A cursor the reader can't decode leaves no point to resume from, so the syncer reads every table in full.
	t.Run("asks for a full read when the cursor is unreadable", func(t *testing.T) {
		_, err := granolaAt(t, nil).Changes(ctx, src, noteTables, json.RawMessage(`"2026-10-20"`), &granolaChanges{})
		if !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("Changes error = %v, want ErrCursorExpired", err)
		}
	})
}
