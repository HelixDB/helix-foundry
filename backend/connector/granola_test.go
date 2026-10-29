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
	"sync"
	"testing"
	"time"
)

const granolaTestKey = "grn_0123456789abcdefABCDEF"

// granolaAt serves routes, checking every request carries the key, and
// returns the granola kind reading from them, unpaced.
func granolaAt(t *testing.T, routes map[string]func(*http.Request) (int, string)) *granolaKind {
	t.Helper()
	saved := granolaInterval
	granolaInterval = 0
	t.Cleanup(func() { granolaInterval = saved })
	checked := map[string]func(*http.Request) (int, string){}
	for path, handler := range routes {
		checked[path] = func(r *http.Request) (int, string) {
			if r.Header.Get("Authorization") != "Bearer "+granolaTestKey {
				t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
			}
			return handler(r)
		}
	}
	return kindOf(t, catalogFor(routeServer(t, checked)), "granola").(*granolaKind)
}

// granolaNote is Get Note's answer for id, with every field Granola documents.
func granolaNote(id string) string {
	return fmt.Sprintf(`{"id":%q,"object":"note","title":"Budget review","owner":{"name":"Oat Benson","email":"oat@granola.ai"},
		"created_at":"2026-01-27T15:30:00Z","updated_at":"2026-01-27T16:45:00Z","deleted_at":null,"web_url":"https://notes.granola.ai/d/f3e4",
		"calendar_event":{"event_title":"Quarterly review","invitees":[{"email":"Raisin@granola.ai"},{"email":"kiwi@granola.ai"}],
			"organiser":"oat@granola.ai","calendar_event_id":"2su9_20260127T153000Z",
			"scheduled_start_time":"2026-01-27T15:30:00Z","scheduled_end_time":"2026-01-27T16:30:00Z"},
		"attachments":[{"id":"att_4y6LduVdwSKC27","object":"attachment","type":"photo","url":"https://files.granola.ai/a","width":640,"height":480}],
		"attendees":[{"name":"Oat Benson","email":"oat@granola.ai"},{"name":"Raisin Patel","email":"raisin@granola.ai"}],
		"folder_membership":[
			{"id":"fol_4y6LduVdwSKC27","object":"folder","name":"Recipes","parent_folder_id":"fol_a74g2hvl98iUHG","space_id":"spa_8kTq2mWxLp4Rz7"},
			{"id":"fol_a74g2hvl98iUHG","object":"folder","name":"Kitchen","parent_folder_id":null,"space_id":"spa_8kTq2mWxLp4Rz7"},
			{"id":"fol_4y6LduVdwSKC27","object":"folder","name":"Recipes","parent_folder_id":"fol_a74g2hvl98iUHG"}],
		"space_membership":[{"id":"spa_8kTq2mWxLp4Rz7","object":"space","name":"Product team"}],
		"summary_text":"It went well.","summary_markdown":"## Review\n\nIt went well.",
		"private_notes_text":"Push back","private_notes_markdown":"- Push back","transcript":null}`, id)
}

// granolaList is a last page of note summaries for ids.
func granolaList(ids ...string) string {
	var notes []string
	for _, id := range ids {
		notes = append(notes, fmt.Sprintf(`{"id":%q,"object":"note","title":"Budget review","owner":{"name":null,"email":"oat@granola.ai"},`+
			`"created_at":"2026-01-27T15:30:00Z","updated_at":"2026-01-27T16:45:00Z","deleted_at":null}`, id))
	}
	return `{"notes":[` + strings.Join(notes, ",") + `],"hasMore":false,"cursor":null}`
}

func TestGranolaConnector(t *testing.T) {
	ctx := context.Background()
	for _, secret := range []string{"sk_live_abc", "grn_", "grn_abc def", "Bearer grn_abc"} {
		t.Run(fmt.Sprintf("rejects %q as a key", secret), func(t *testing.T) {
			_, err := kindOf(t, catalogFor("http://unused"), "granola").Prepare(Source{Secret: secret})
			var invalidErr *InvalidError
			if !errors.As(err, &invalidErr) || !strings.Contains(invalidErr.Message, "grn_") {
				t.Fatalf("Prepare error = %v, want InvalidError", err)
			}
		})
	}
	// Transcripts are off unless asked for, stored as {} like a key-only kind's settings.
	t.Run("takes only whether to keep transcripts besides the key", func(t *testing.T) {
		kind := kindOf(t, catalogFor("http://unused"), "granola")
		for raw, want := range map[string]string{"": `{}`, `{}`: `{}`, `{"transcripts":false}`: `{}`, `{"transcripts":true}`: `{"transcripts":true}`} {
			settings, err := kind.Prepare(Source{Secret: " " + granolaTestKey + " ", Settings: json.RawMessage(raw)})
			if err != nil || string(settings) != want {
				t.Errorf("Prepare(%s) = (%s, %v), want %s", raw, settings, err, want)
			}
		}
		for _, raw := range []string{`{"tables":["notes"]}`, `{"transcripts":"yes"}`, `[]`} {
			_, err := kind.Prepare(Source{Secret: granolaTestKey, Settings: json.RawMessage(raw)})
			var invalidErr *InvalidError
			if !errors.As(err, &invalidErr) || invalidErr.Message != "this connector takes only its key and whether to keep transcripts" {
				t.Errorf("Prepare(%s) error = %v, want InvalidError", raw, err)
			}
		}
	})
	t.Run("keeps notes, and transcripts when the settings turn them on", func(t *testing.T) {
		kind := kindOf(t, catalogFor("http://unused"), "granola").(DocumentKind)
		names := func(streams []Stream) (out []string) {
			for _, s := range streams {
				out = append(out, s.Name+" of "+s.Table)
			}
			return out
		}
		if got := names(kind.Streams(Source{Settings: json.RawMessage(`{}`)})); !slices.Equal(got, []string{"notes of notes"}) {
			t.Fatalf("streams = %v", got)
		}
		got := names(kind.Streams(Source{Settings: json.RawMessage(`{"transcripts":true}`)}))
		if !slices.Equal(got, []string{"notes of notes", "transcripts of notes"}) {
			t.Fatalf("streams = %v", got)
		}
	})
	t.Run("hints at a key by its prefix and last four characters", func(t *testing.T) {
		kind := kindOf(t, catalogFor("http://unused"), "granola")
		if hint := kind.Hint(" " + granolaTestKey); hint != "grn…CDEF" || kind.DisplayName() != "Granola" {
			t.Fatalf("Hint = %q, DisplayName = %q", hint, kind.DisplayName())
		}
	})
	onePage := func(field string) func(*http.Request) (int, string) {
		return func(r *http.Request) (int, string) {
			if r.URL.Query().Get("page_size") != "1" {
				t.Errorf("probe %s", r.URL)
			}
			return http.StatusOK, `{"` + field + `":[],"hasMore":false,"cursor":null}`
		}
	}
	t.Run("lists every object the key reads, with keys and foreign keys", func(t *testing.T) {
		d, err := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": onePage("notes"), "/v1/folders": onePage("folders"), "/v1/spaces": onePage("spaces"),
		}).Discover(ctx, Source{Secret: granolaTestKey, ForSync: true})
		if err != nil {
			t.Fatal(err)
		}
		if names := tableNames(d); !slices.Equal(names, []string{"notes", "note_attendees", "note_folders", "folders", "spaces"}) || len(d.Unavailable) != 0 {
			t.Fatalf("tables = %v, unavailable = %v", names, d.Unavailable)
		}
		want := map[string]Table{
			"notes":          {Name: "notes", PrimaryKey: []string{"note_id"}},
			"note_attendees": {Name: "note_attendees", PrimaryKey: []string{"note_id", "email"}, ForeignKeys: granolaForeignKeys["note_attendees"]},
			"note_folders":   {Name: "note_folders", PrimaryKey: []string{"note_id", "folder_id"}, ForeignKeys: granolaForeignKeys["note_folders"]},
			"folders":        {Name: "folders", PrimaryKey: []string{"folder_id"}, ForeignKeys: granolaForeignKeys["folders"]},
			"spaces":         {Name: "spaces", PrimaryKey: []string{"space_id"}},
		}
		for name, table := range want {
			if got := findTable(t, d, name); fmt.Sprint(got) != fmt.Sprint(table) {
				t.Errorf("%s = %+v, want %+v", name, got, table)
			}
		}
		folders := findTable(t, d, "folders")
		if folders.ForeignKeys[0] != (ForeignKey{Column: "parent_folder_id", ReferencedTable: "folders", ReferencedColumn: "folder_id"}) {
			t.Fatalf("folders = %+v", folders)
		}
	})
	// A list the key may not read is reported, not fatal, and foreign keys into it are dropped.
	t.Run("reports objects the key cannot read", func(t *testing.T) {
		d, err := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": onePage("notes"), "/v1/folders": onePage("folders"),
			"/v1/spaces": func(*http.Request) (int, string) { return http.StatusForbidden, `{"message":"Forbidden"}` },
		}).Discover(ctx, Source{Secret: granolaTestKey})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(d.Unavailable, []Unavailable{{Name: "spaces"}}) {
			t.Fatalf("unavailable = %v", d.Unavailable)
		}
		if folders := findTable(t, d, "folders"); len(folders.ForeignKeys) != 1 || folders.ForeignKeys[0].ReferencedTable != "folders" {
			t.Fatalf("folders = %+v", folders)
		}
	})
	t.Run("drops a note's tables when the key cannot list notes", func(t *testing.T) {
		d, err := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":   func(*http.Request) (int, string) { return http.StatusNotFound, `{}` },
			"/v1/folders": onePage("folders"), "/v1/spaces": onePage("spaces"),
		}).Discover(ctx, Source{Secret: granolaTestKey})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(unavailableNames(d), []string{"notes", "note_attendees", "note_folders"}) || !slices.Equal(tableNames(d), []string{"folders", "spaces"}) {
			t.Fatalf("tables = %v, unavailable = %v", tableNames(d), d.Unavailable)
		}
	})
	t.Run("refuses a key that reads nothing", func(t *testing.T) {
		forbidden := func(*http.Request) (int, string) { return http.StatusForbidden, `{}` }
		_, err := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": forbidden, "/v1/folders": forbidden, "/v1/spaces": forbidden,
		}).Discover(ctx, Source{Secret: granolaTestKey})
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || !strings.Contains(refusedErr.Message, "cannot read Granola notes, folders or spaces") {
			t.Fatalf("Discover error = %v, want a refusal", err)
		}
	})
	// Granola answers an invalid or revoked key with 401.
	t.Run("reports a rejected key", func(t *testing.T) {
		unauthorized := func(*http.Request) (int, string) { return http.StatusUnauthorized, `{"message":"Unauthorized"}` }
		_, err := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": unauthorized, "/v1/folders": unauthorized, "/v1/spaces": unauthorized,
		}).Discover(ctx, Source{Secret: granolaTestKey})
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || refusedErr.Message != "Granola rejected this key; check it and enter it again" {
			t.Fatalf("Discover error = %v, want a rejected key", err)
		}
	})
	// A rate limit or an outage says nothing about the object, so it must not
	// make the object unavailable: a sync would then fail it for good.
	for status, message := range map[int]string{
		http.StatusTooManyRequests: "this key has used up its Granola rate limit; try again in a few minutes",
		http.StatusBadGateway:      "Granola returned HTTP 502 for /v1/spaces",
	} {
		t.Run(fmt.Sprintf("refuses a probe answered %d after retries", status), func(t *testing.T) {
			noBackoff(t)
			_, err := granolaAt(t, map[string]func(*http.Request) (int, string){
				"/v1/notes": onePage("notes"), "/v1/folders": onePage("folders"),
				"/v1/spaces": func(*http.Request) (int, string) { return status, `{}` },
			}).Discover(ctx, Source{Secret: granolaTestKey})
			var refusedErr *RefusedError
			if !errors.As(err, &refusedErr) || refusedErr.Message != message {
				t.Fatalf("Discover error = %v, want %q", err, message)
			}
		})
	}
}

func TestGranolaSync(t *testing.T) {
	ctx := context.Background()
	src := Source{Secret: granolaTestKey}
	read := func(t *testing.T, kind *granolaKind, table string) ([]map[string]any, error) {
		t.Helper()
		sink := &rowSink{}
		_, err := kind.Sync(ctx, src, Table{Name: table}, sink)
		return sink.rows, err
	}
	getNote := func(id string) func(*http.Request) (int, string) {
		return func(r *http.Request) (int, string) {
			if r.URL.RawQuery != "" {
				t.Errorf("Get Note query = %s; the transcript is not read", r.URL.RawQuery)
			}
			return http.StatusOK, granolaNote(id)
		}
	}

	// Pages follow the cursor 30 notes at a time, and each listed note is read in full.
	t.Run("pages notes and gets each in full", func(t *testing.T) {
		var cursors []string
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": func(r *http.Request) (int, string) {
				q := r.URL.Query()
				if q.Get("page_size") != "30" {
					t.Errorf("query = %s", r.URL.RawQuery)
				}
				cursors = append(cursors, q.Get("cursor"))
				if q.Get("cursor") == "" {
					page := strings.Replace(granolaList("not_1", "not_2"), `"hasMore":false,"cursor":null`, `"hasMore":true,"cursor":"c1"`, 1)
					return http.StatusOK, page
				}
				return http.StatusOK, granolaList("not_3")
			},
			"/v1/notes/not_1": getNote("not_1"), "/v1/notes/not_2": getNote("not_2"), "/v1/notes/not_3": getNote("not_3"),
		})
		rows, err := read(t, kind, "notes")
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(cursors) != "[ c1]" || len(rows) != 3 || rows[2]["note_id"] != "not_3" {
			t.Fatalf("cursors = %q, rows = %v", cursors, rows)
		}
	})
	t.Run("flattens a note's owner and calendar event and leaves out its lists and private notes", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
			"/v1/notes/not_1": getNote("not_1"),
		})
		rows, err := read(t, kind, "notes")
		if err != nil {
			t.Fatal(err)
		}
		want := []map[string]any{{
			"note_id": "not_1", "title": "Budget review", "owner_name": "Oat Benson", "owner_email": "oat@granola.ai",
			"created_at": "2026-01-27T15:30:00Z", "updated_at": "2026-01-27T16:45:00Z", "web_url": "https://notes.granola.ai/d/f3e4",
			"calendar_event_id": "2su9_20260127T153000Z", "calendar_event_title": "Quarterly review", "organiser_email": "oat@granola.ai",
			"scheduled_start_time": "2026-01-27T15:30:00Z", "scheduled_end_time": "2026-01-27T16:30:00Z",
			"summary_text": "It went well.", "summary_markdown": "## Review\n\nIt went well.",
		}}
		if fmt.Sprint(rows) != fmt.Sprint(want) {
			t.Fatalf("rows = %v", rows)
		}
	})
	t.Run("keeps a note without a calendar event's columns empty", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
			"/v1/notes/not_1": func(*http.Request) (int, string) {
				return http.StatusOK, `{"id":"not_1","title":null,"owner":{"name":null,"email":"oat@granola.ai"},"calendar_event":null,
					"attendees":[{"name":"Nobody","email":" "},{"name":"Nobody"}]}`
			},
		})
		rows, err := read(t, kind, "notes")
		if err != nil {
			t.Fatal(err)
		}
		want := []map[string]any{{
			"note_id": "not_1", "title": nil, "owner_name": nil, "owner_email": "oat@granola.ai", "calendar_event_id": nil,
			"calendar_event_title": nil, "organiser_email": nil, "scheduled_start_time": nil, "scheduled_end_time": nil,
		}}
		if fmt.Sprint(rows) != fmt.Sprint(want) {
			t.Fatalf("rows = %v", rows)
		}
		attendees, err := read(t, kind, "note_attendees")
		if err != nil || len(attendees) != 0 {
			t.Fatalf("attendees = %v, %v", attendees, err)
		}
	})
	// An attendee the event also invited is one row; emails differ in case between the two lists.
	t.Run("writes each attendee and invitee once by email", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
			"/v1/notes/not_1": getNote("not_1"),
		})
		rows, err := read(t, kind, "note_attendees")
		if err != nil {
			t.Fatal(err)
		}
		want := []map[string]any{
			{"note_id": "not_1", "email": "oat@granola.ai", "name": "Oat Benson", "attended": true, "invited": false},
			{"note_id": "not_1", "email": "raisin@granola.ai", "name": "Raisin Patel", "attended": true, "invited": true},
			{"note_id": "not_1", "email": "kiwi@granola.ai", "name": nil, "attended": false, "invited": true},
		}
		if fmt.Sprint(rows) != fmt.Sprint(want) {
			t.Fatalf("rows = %v", rows)
		}
	})
	t.Run("writes each folder a note is in once", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
			"/v1/notes/not_1": getNote("not_1"),
		})
		rows, err := read(t, kind, "note_folders")
		if err != nil {
			t.Fatal(err)
		}
		want := []map[string]any{{"note_id": "not_1", "folder_id": "fol_4y6LduVdwSKC27"}, {"note_id": "not_1", "folder_id": "fol_a74g2hvl98iUHG"}}
		if fmt.Sprint(rows) != fmt.Sprint(want) {
			t.Fatalf("rows = %v", rows)
		}
	})
	t.Run("pages folders and spaces and keys them by id", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/folders": func(r *http.Request) (int, string) {
				if r.URL.Query().Get("cursor") == "" {
					return http.StatusOK, `{"folders":[{"id":"fol_a74g2hvl98iUHG","object":"folder","name":"Kitchen","parent_folder_id":null}],"hasMore":true,"cursor":"f1"}`
				}
				return http.StatusOK, `{"folders":[{"id":"fol_4y6LduVdwSKC27","object":"folder","name":"Recipes",
					"parent_folder_id":"fol_a74g2hvl98iUHG","space_id":"spa_8kTq2mWxLp4Rz7"}],"hasMore":false,"cursor":null}`
			},
			"/v1/spaces": func(*http.Request) (int, string) {
				return http.StatusOK, `{"spaces":[{"id":"spa_8kTq2mWxLp4Rz7","object":"space","name":"Product team"}],"hasMore":false,"cursor":null}`
			},
		})
		folders, err := read(t, kind, "folders")
		if err != nil {
			t.Fatal(err)
		}
		want := []map[string]any{
			{"folder_id": "fol_a74g2hvl98iUHG", "name": "Kitchen", "parent_folder_id": nil, "space_id": nil},
			{"folder_id": "fol_4y6LduVdwSKC27", "name": "Recipes", "parent_folder_id": "fol_a74g2hvl98iUHG", "space_id": "spa_8kTq2mWxLp4Rz7"},
		}
		if fmt.Sprint(folders) != fmt.Sprint(want) {
			t.Fatalf("folders = %v", folders)
		}
		spaces, err := read(t, kind, "spaces")
		if err != nil || fmt.Sprint(spaces) != fmt.Sprint([]map[string]any{{"space_id": "spa_8kTq2mWxLp4Rz7", "name": "Product team"}}) {
			t.Fatalf("spaces = %v, %v", spaces, err)
		}
	})
	// Granola lists only processed notes, but one can go between the list and the get;
	// a key that sees every workspace note also lists deleted ones.
	t.Run("skips a note Get Note no longer finds and one the list shows deleted", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": func(*http.Request) (int, string) {
				return http.StatusOK, `{"notes":[{"id":"not_1","created_at":"2026-01-27T15:30:00Z","updated_at":"2026-01-27T16:45:00Z","deleted_at":null},
					{"id":"not_2","created_at":"2026-01-26T15:30:00Z","updated_at":"2026-01-26T16:45:00Z","deleted_at":null},
					{"id":"not_3","deleted_at":"2026-01-28T09:00:00Z"}],"hasMore":false,"cursor":null}`
			},
			"/v1/notes/not_1": getNote("not_1"),
			"/v1/notes/not_2": func(*http.Request) (int, string) { return http.StatusNotFound, `{"message":"Not found"}` },
		})
		rows, err := read(t, kind, "notes")
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0]["note_id"] != "not_1" {
			t.Fatalf("rows = %v", rows)
		}
	})
	// Every listed note missing is Granola failing; an empty table would drop every stored note.
	t.Run("fails when Get Note finds none of the listed notes", func(t *testing.T) {
		missing := func(*http.Request) (int, string) { return http.StatusNotFound, `{}` }
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1", "not_2") },
			"/v1/notes/not_1": missing, "/v1/notes/not_2": missing,
		})
		rows, err := read(t, kind, "note_attendees")
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || refusedErr.Message != "Granola listed 2 notes but returned none of them; try again" || len(rows) != 0 {
			t.Fatalf("Sync = (%v, %v), want a refusal", rows, err)
		}
	})
	t.Run("reads an empty account as empty tables", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": func(*http.Request) (int, string) { return http.StatusOK, granolaList() },
		})
		if rows, err := read(t, kind, "notes"); err != nil || len(rows) != 0 {
			t.Fatalf("Sync = (%v, %v)", rows, err)
		}
	})
	// Ending early on an incomplete page would replace the snapshot with part of the table.
	for name, page := range map[string]string{
		"is not JSON":                            `<html>Bad gateway</html>`,
		"lacks its notes":                        `{"hasMore":false,"cursor":null}`,
		"has null for its notes":                 `{"notes":null,"hasMore":false,"cursor":null}`,
		"lacks hasMore":                          `{"notes":[],"cursor":null}`,
		"says more follow but gives no cursor":   `{"notes":[],"hasMore":true,"cursor":null}`,
		"gives back the cursor it was asked for": `{"notes":[],"hasMore":true,"cursor":"c1"}`,
		"lists a note without an id":             `{"notes":[{"title":"t"}],"hasMore":false,"cursor":null}`,
		"lists a note that is not an object":     `{"notes":["not_1"],"hasMore":false,"cursor":null}`,
	} {
		t.Run("fails a page that "+name, func(t *testing.T) {
			kind := granolaAt(t, map[string]func(*http.Request) (int, string){
				"/v1/notes": func(*http.Request) (int, string) { return http.StatusOK, page },
			})
			if rows, err := read(t, kind, "notes"); err == nil || len(rows) != 0 {
				t.Fatalf("Sync = (%v, %v); want a failure before any row", rows, err)
			}
		})
	}
	for name, answer := range map[string]string{
		"is not JSON":       `{"id":"not_1",`,
		"is another note":   granolaNote("not_9"),
		"has no id":         `{"title":"t"}`,
		"is null":           `null`,
		"is not an object":  `["not_1"]`,
		"is an empty value": ``,
	} {
		t.Run("fails a Get Note answer that "+name, func(t *testing.T) {
			kind := granolaAt(t, map[string]func(*http.Request) (int, string){
				"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
				"/v1/notes/not_1": func(*http.Request) (int, string) { return http.StatusOK, answer },
			})
			if rows, err := read(t, kind, "notes"); err == nil || len(rows) != 0 {
				t.Fatalf("Sync = (%v, %v); want a failure", rows, err)
			}
		})
	}
	t.Run("refuses a key revoked during the read", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
			"/v1/notes/not_1": func(*http.Request) (int, string) { return http.StatusUnauthorized, `{}` },
		})
		_, err := read(t, kind, "notes")
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || !strings.Contains(refusedErr.Message, "rejected this key") {
			t.Fatalf("Sync error = %v, want a rejected key", err)
		}
	})
	t.Run("retries a rate limit", func(t *testing.T) {
		noBackoff(t)
		var mu sync.Mutex
		calls := 0
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/spaces": func(*http.Request) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if calls == 1 {
					return http.StatusTooManyRequests, `{}`
				}
				return http.StatusOK, `{"spaces":[{"id":"spa_1","name":"Team"}],"hasMore":false,"cursor":null}`
			},
		})
		if rows, err := read(t, kind, "spaces"); err != nil || len(rows) != 1 || calls != 2 {
			t.Fatalf("Sync = (%v, %v) after %d calls", rows, err, calls)
		}
	})
	// A limit that outlasts the retries fails the attempt with a message the syncer retries on.
	t.Run("refuses a rate limit that outlasts the retries, saying when to try again", func(t *testing.T) {
		noBackoff(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		t.Cleanup(server.Close)
		kind := granolaAt(t, nil)
		kind.base = server.URL
		before := time.Now()
		_, err := read(t, kind, "spaces")
		after := time.Now()
		var refusedErr *RefusedError
		const prefix = "this key has used up its Granola rate limit; try again after "
		if !errors.As(err, &refusedErr) || !strings.HasPrefix(refusedErr.Message, prefix) {
			t.Fatalf("Sync error = %v, want a rate limit with its time", err)
		}
		at := strings.TrimPrefix(refusedErr.Message, prefix)
		if early, late := before.Add(2*time.Minute).UTC().Format("15:04 UTC"), after.Add(2*time.Minute).UTC().Format("15:04 UTC"); at != early && at != late {
			t.Fatalf("retry at %q, want %s", at, early)
		}
	})
	// Granola allows 5 requests a second sustained; reads stay under it.
	t.Run("paces requests under Granola's rate limit", func(t *testing.T) {
		if granolaInterval < 200*time.Millisecond {
			t.Fatalf("granolaInterval = %s, more than 5 requests a second", granolaInterval)
		}
		var mu sync.Mutex
		var at []time.Time
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/spaces": func(r *http.Request) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				at = append(at, time.Now())
				if len(at) < 3 {
					return http.StatusOK, fmt.Sprintf(`{"spaces":[],"hasMore":true,"cursor":"s%d"}`, len(at))
				}
				return http.StatusOK, `{"spaces":[],"hasMore":false,"cursor":null}`
			},
		})
		granolaInterval = 50 * time.Millisecond
		if _, err := read(t, kind, "spaces"); err != nil {
			t.Fatal(err)
		}
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap < 40*time.Millisecond {
				t.Fatalf("request %d came %s after the one before", i, gap)
			}
		}
	})
	t.Run("refuses a list the key can no longer read", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/folders": func(*http.Request) (int, string) { return http.StatusForbidden, `{}` },
		})
		_, err := read(t, kind, "folders")
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || refusedErr.Message != "this key cannot read /v1/folders; check its access in Granola and test again" {
			t.Fatalf("Sync error = %v, want a refusal", err)
		}
	})
	t.Run("refuses a read Granola could not be reached for", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		server.Close()
		kind := granolaAt(t, nil)
		kind.base = server.URL
		_, err := read(t, kind, "spaces")
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || refusedErr.Message != "could not reach Granola; try again" {
			t.Fatalf("Sync error = %v, want a refusal", err)
		}
	})
	t.Run("refuses a note Granola dropped the connection for", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/v1/notes", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(granolaList("not_1"))) })
		mux.HandleFunc("/v1/notes/not_1", func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		kind := granolaAt(t, nil)
		kind.base = server.URL
		_, err := read(t, kind, "notes")
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || refusedErr.Message != "could not reach Granola; try again" {
			t.Fatalf("Sync error = %v, want a refusal", err)
		}
	})
	t.Run("stops when the read is cancelled", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
		})
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := kind.Sync(cancelled, src, Table{Name: "notes"}, &rowSink{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Sync error = %v, want context.Canceled", err)
		}
	})
	// A snapshot over its row limit stops the read with the writer's error, which the syncer reports.
	t.Run("stops at the writer's error", func(t *testing.T) {
		full := errors.New("row limit")
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1", "not_2") },
			"/v1/notes/not_1": getNote("not_1"),
		})
		for _, table := range []string{"notes", "note_attendees", "note_folders"} {
			if _, err := kind.Sync(ctx, src, Table{Name: table}, refusingSink{full}); !errors.Is(err, full) {
				t.Fatalf("%s: Sync error = %v, want the writer's", table, err)
			}
		}
	})
	t.Run("has no object it does not list", func(t *testing.T) {
		_, err := read(t, granolaAt(t, nil), "transcripts")
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) {
			t.Fatalf("Sync error = %v, want InvalidError", err)
		}
	})
}

// refusingSink fails every write with err.
type refusingSink struct{ err error }

func (s refusingSink) Write(map[string]any) error { return s.err }
func (s refusingSink) WriteJSON([]byte) error     { return s.err }

// granolaDocs is a DocumentWriter that says yes to the streams want names
// and records each offer, Put and Drop; a set fail fails every Put.
type granolaDocs struct {
	want   map[string]bool
	offers []string
	puts   []granolaPut
	drops  []string
	fail   error
}

type granolaPut struct {
	stream string
	doc    Document
}

func (d *granolaDocs) Want(stream, id, version string) bool {
	d.offers = append(d.offers, stream+" "+id+" "+version)
	return d.want[stream]
}

func (d *granolaDocs) Put(_ context.Context, stream string, doc Document) error {
	d.puts = append(d.puts, granolaPut{stream, doc})
	return d.fail
}

func (d *granolaDocs) Drop(stream, id string) error {
	d.drops = append(d.drops, stream+" "+id)
	return nil
}

func (d *granolaDocs) Held(string, string) (string, bool) { return "", false }

func (d *granolaDocs) Paused(string) bool { return false }

// putNames are the Puts, as "stream id".
func (d *granolaDocs) putNames() []string {
	var names []string
	for _, p := range d.puts {
		names = append(names, p.stream+" "+p.doc.ExternalID)
	}
	return names
}

// granolaTranscribed is Get Note's answer for id with its transcript.
func granolaTranscribed(id string) string {
	return strings.Replace(granolaNote(id), `"transcript":null`, `"transcript":[
		{"speaker":{"source":"microphone","attribution":"me","name":"Oat Benson"},"text":"Shall we start?","start_time":"2026-01-27T15:30:01Z","end_time":"2026-01-27T15:30:02Z"},
		{"speaker":{"source":"speaker","diarization_label":"Speaker B"},"text":"  Yes,\n let's. ","start_time":"2026-01-27T15:30:03Z","end_time":"2026-01-27T15:30:04Z"},
		{"speaker":{"source":"speaker","attribution":"them"},"text":"Budget first.","start_time":"2026-01-27T15:30:05Z","end_time":"2026-01-27T15:30:06Z"},
		{"speaker":{"source":"microphone","attribution":"me"},"text":"Agreed.","start_time":"2026-01-27T15:30:07Z","end_time":"2026-01-27T15:30:08Z"},
		{"speaker":{"source":"microphone"},"text":"Mic only.","start_time":"2026-01-27T15:30:09Z","end_time":"2026-01-27T15:30:10Z"},
		{"speaker":{"source":"speaker"},"text":"System audio.","start_time":"2026-01-27T15:30:11Z","end_time":"2026-01-27T15:30:12Z"},
		{"speaker":{"source":"speaker"},"text":"   ","start_time":"2026-01-27T15:30:13Z","end_time":"2026-01-27T15:30:14Z"}]`, 1)
}

// granolaHeader is the opening both of granolaNote's documents share.
const granolaHeader = "# Budget review\n\nDate: 2026-01-27 15:30 UTC\n" +
	"Attendees: Oat Benson <oat@granola.ai>, Raisin Patel <raisin@granola.ai>\nNote id: not_1\n"

func TestGranolaDocuments(t *testing.T) {
	ctx := context.Background()
	transcripts := Source{Secret: granolaTestKey, Settings: json.RawMessage(`{"transcripts":true}`)}
	allTables := []Table{{Name: "notes"}, {Name: "note_attendees"}, {Name: "note_folders"}, {Name: "folders"}, {Name: "spaces"}}
	// pass reads tables in one SyncPass, into a sink per table.
	pass := func(t *testing.T, kind *granolaKind, src Source, tables []Table, docs DocumentWriter) (map[string]*rowSink, error) {
		t.Helper()
		sinks, rows := map[string]*rowSink{}, map[string]RowWriter{}
		for _, table := range tables {
			sinks[table.Name] = &rowSink{}
			rows[table.Name] = sinks[table.Name]
		}
		written, err := kind.SyncPass(ctx, src, tables, rows, docs)
		if fmt.Sprint(written) != fmt.Sprint(tables) {
			t.Errorf("SyncPass wrote %v, want %v", written, tables)
		}
		return sinks, err
	}
	// gets counts each Get Note, by path and query.
	type gets struct {
		mu sync.Mutex
		n  map[string]int
	}
	counted := func(g *gets, answer func(r *http.Request) (int, string)) func(*http.Request) (int, string) {
		return func(r *http.Request) (int, string) {
			g.mu.Lock()
			g.n[r.URL.Path+"?"+r.URL.RawQuery]++
			g.mu.Unlock()
			return answer(r)
		}
	}
	noteOrTranscript := func(r *http.Request) (int, string) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/notes/")
		if r.URL.Query().Get("include") == "transcript" {
			return http.StatusOK, granolaTranscribed(id)
		}
		return http.StatusOK, granolaNote(id)
	}

	// One pass reads every note once for all the note tables, not once per table.
	t.Run("reads every table in one pass, with one Get Note per note", func(t *testing.T) {
		g := &gets{n: map[string]int{}}
		lists := 0
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": func(*http.Request) (int, string) {
				lists++
				return http.StatusOK, granolaList("not_1", "not_2")
			},
			"/v1/notes/not_1": counted(g, noteOrTranscript), "/v1/notes/not_2": counted(g, noteOrTranscript),
			"/v1/folders": func(*http.Request) (int, string) {
				return http.StatusOK, `{"folders":[{"id":"fol_1","name":"Kitchen"}],"hasMore":false,"cursor":null}`
			},
			"/v1/spaces": func(*http.Request) (int, string) {
				return http.StatusOK, `{"spaces":[{"id":"spa_1","name":"Team"}],"hasMore":false,"cursor":null}`
			},
		})
		docs := &granolaDocs{want: map[string]bool{"notes": true}}
		sinks, err := pass(t, kind, Source{Secret: granolaTestKey}, allTables, docs)
		if err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for name, sink := range sinks {
			counts[name] = len(sink.rows)
		}
		want := map[string]int{"notes": 2, "note_attendees": 6, "note_folders": 4, "folders": 1, "spaces": 1}
		if fmt.Sprint(counts) != fmt.Sprint(want) || lists != 1 {
			t.Fatalf("rows = %v after %d lists", counts, lists)
		}
		if fmt.Sprint(g.n) != "map[/v1/notes/not_1?:1 /v1/notes/not_2?:1]" {
			t.Fatalf("Get Notes = %v", g.n)
		}
		if !slices.Equal(docs.putNames(), []string{"notes not_1", "notes not_2"}) {
			t.Fatalf("puts = %v", docs.putNames())
		}
	})
	t.Run("writes the notes and transcripts documents", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
			"/v1/notes/not_1": noteOrTranscript,
		})
		docs := &granolaDocs{want: map[string]bool{"notes": true, "transcripts": true}}
		if _, err := pass(t, kind, transcripts, allTables[:1], docs); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(docs.offers, []string{"notes not_1 1|2026-01-27T16:45:00Z", "transcripts not_1 1|2026-01-27T16:45:00Z"}) {
			t.Fatalf("offers = %q", docs.offers)
		}
		at := time.Date(2026, 1, 27, 15, 30, 0, 0, time.UTC)
		want := []granolaPut{
			{"notes", Document{ExternalID: "not_1", Version: "1|2026-01-27T16:45:00Z", At: at, Title: "Budget review",
				URL: "https://notes.granola.ai/d/f3e4", Text: []byte(granolaHeader + "\n## Review\n\nIt went well.\n")}},
			{"transcripts", Document{ExternalID: "not_1", Version: "1|2026-01-27T16:45:00Z", At: at, Title: "Budget review",
				URL: "https://notes.granola.ai/d/f3e4", Text: []byte(granolaHeader + "\nOat Benson: Shall we start?\nSpeaker B: Yes, let's.\n" +
					"Them: Budget first.\nMe: Agreed.\nMe: Mic only.\nThem: System audio.\n")}},
		}
		if len(docs.puts) != len(want) {
			t.Fatalf("puts = %v", docs.putNames())
		}
		for i, p := range docs.puts {
			if p.stream != want[i].stream || !p.doc.At.Equal(want[i].doc.At) || string(p.doc.Text) != string(want[i].doc.Text) ||
				fmt.Sprint(p.doc.ExternalID, p.doc.Version, p.doc.Title, p.doc.URL) != fmt.Sprint(want[i].doc.ExternalID, want[i].doc.Version, want[i].doc.Title, want[i].doc.URL) {
				t.Errorf("put %d = %s %+v\n%s\nwant %+v\n%s", i, p.stream, p.doc, p.doc.Text, want[i].doc, want[i].doc.Text)
			}
		}
	})
	// A note without a title, summary or attendees still has its header; the
	// summary falls back to plain text; a transcript with nothing said is not Put.
	t.Run("writes what a sparse note has", func(t *testing.T) {
		sparse := map[string]string{
			"not_1": `{"id":"not_1","title":null,"web_url":"http://notes.granola.ai/d/1","attendees":[],"summary_markdown":null,
				"summary_text":"Plain summary.","transcript":[{"speaker":{"source":"speaker"},"text":" "}]}`,
			"not_2": `{"id":"not_2","title":"  Stand\nup ","summary_markdown":"  ","summary_text":null,"transcript":null}`,
		}
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1", "not_2") },
			"/v1/notes/not_1": func(*http.Request) (int, string) { return http.StatusOK, sparse["not_1"] },
			"/v1/notes/not_2": func(*http.Request) (int, string) { return http.StatusOK, sparse["not_2"] },
		})
		docs := &granolaDocs{want: map[string]bool{"notes": true, "transcripts": true}}
		if _, err := pass(t, kind, transcripts, allTables[:1], docs); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(docs.putNames(), []string{"notes not_1", "notes not_2"}) {
			t.Fatalf("puts = %v", docs.putNames())
		}
		first, second := docs.puts[0].doc, docs.puts[1].doc
		if string(first.Text) != "# Untitled note\n\nDate: 2026-01-27 15:30 UTC\nNote id: not_1\n\nPlain summary.\n" || first.Title != "" ||
			first.URL != "http://notes.granola.ai/d/1" {
			t.Fatalf("first = %+v\n%s", first, first.Text)
		}
		if string(second.Text) != "# Stand up\n\nDate: 2026-01-27 15:30 UTC\nNote id: not_2\n" || second.Title != "Stand up" {
			t.Fatalf("second = %+v\n%s", second, second.Text)
		}
	})
	// The syncer keeps the first documents offered up to its cap, so the newest must come first.
	t.Run("offers the newest notes first", func(t *testing.T) {
		listed := func(id, created string) string {
			return fmt.Sprintf(`{"id":%q,"created_at":%q,"updated_at":"2026-02-01T00:00:00Z","deleted_at":null}`, id, created)
		}
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": func(r *http.Request) (int, string) {
				if r.URL.Query().Get("cursor") == "" {
					return http.StatusOK, `{"notes":[` + listed("not_1", "2026-01-10T09:00:00Z") + "," + listed("not_2", "2026-01-12T09:00:00Z") +
						`],"hasMore":true,"cursor":"c1"}`
				}
				return http.StatusOK, `{"notes":[` + listed("not_3", "2026-01-11T09:00:00.5+01:00") + `],"hasMore":false,"cursor":null}`
			},
			"/v1/notes/not_1": noteOrTranscript, "/v1/notes/not_2": noteOrTranscript, "/v1/notes/not_3": noteOrTranscript,
		})
		docs := &granolaDocs{want: map[string]bool{"notes": true}}
		if _, err := pass(t, kind, Source{Secret: granolaTestKey}, allTables[:1], docs); err != nil {
			t.Fatal(err)
		}
		want := []string{"notes not_2 1|2026-02-01T00:00:00Z", "notes not_3 1|2026-02-01T00:00:00Z", "notes not_1 1|2026-02-01T00:00:00Z"}
		if !slices.Equal(docs.offers, want) {
			t.Fatalf("offers = %q", docs.offers)
		}
		if at := docs.puts[1].doc.At; !at.Equal(time.Date(2026, 1, 11, 8, 0, 0, 5e8, time.UTC)) {
			t.Fatalf("At = %s", at)
		}
	})
	// A list paged while it changes can show a note twice; it is got, written and offered once.
	t.Run("reads a note listed twice once, as last listed", func(t *testing.T) {
		g := &gets{n: map[string]int{}}
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": func(r *http.Request) (int, string) {
				if r.URL.Query().Get("cursor") == "" {
					page := strings.Replace(granolaList("not_1"), `"hasMore":false,"cursor":null`, `"hasMore":true,"cursor":"c1"`, 1)
					return http.StatusOK, page
				}
				return http.StatusOK, strings.Replace(granolaList("not_1"), "16:45:00Z", "17:00:00Z", 1)
			},
			"/v1/notes/not_1": counted(g, noteOrTranscript),
		})
		docs := &granolaDocs{want: map[string]bool{"notes": true}}
		sinks, err := pass(t, kind, Source{Secret: granolaTestKey}, allTables[:1], docs)
		if err != nil {
			t.Fatal(err)
		}
		if len(sinks["notes"].rows) != 1 || g.n["/v1/notes/not_1?"] != 1 || !slices.Equal(docs.offers, []string{"notes not_1 1|2026-01-27T17:00:00Z"}) ||
			len(docs.puts) != 1 {
			t.Fatalf("rows = %d, gets = %v, offers = %q, puts = %v", len(sinks["notes"].rows), g.n, docs.offers, docs.putNames())
		}
	})
	// A transcript the stream holds at this version is not fetched again.
	t.Run("asks for a transcript only when it is wanted", func(t *testing.T) {
		for _, want := range []map[string]bool{{"notes": true}, {}, {"transcripts": true}} {
			g := &gets{n: map[string]int{}}
			kind := granolaAt(t, map[string]func(*http.Request) (int, string){
				"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
				"/v1/notes/not_1": counted(g, noteOrTranscript),
			})
			docs := &granolaDocs{want: want}
			if _, err := pass(t, kind, transcripts, allTables[:1], docs); err != nil {
				t.Fatal(err)
			}
			got := "map[/v1/notes/not_1?:1]"
			if want["transcripts"] {
				got = "map[/v1/notes/not_1?include=transcript:1]"
			}
			if fmt.Sprint(g.n) != got || len(docs.offers) != 2 || len(docs.puts) != len(want) {
				t.Fatalf("want %v: gets = %v, offers = %q, puts = %v", want, g.n, docs.offers, docs.putNames())
			}
		}
	})
	// Granola answers 413 for a transcript too large to return inline, to be read in pages.
	t.Run("reads a transcript too large to return inline in pages", func(t *testing.T) {
		var asked []string
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
			"/v1/notes/not_1": func(r *http.Request) (int, string) {
				asked = append(asked, r.URL.RawQuery)
				if r.URL.Query().Get("include") == "transcript" {
					return http.StatusRequestEntityTooLarge, `{"message":"TRANSCRIPT_TOO_LARGE"}`
				}
				return http.StatusOK, granolaNote("not_1")
			},
			"/v1/notes/not_1/transcript": func(r *http.Request) (int, string) {
				q := r.URL.Query()
				asked = append(asked, r.URL.RawQuery)
				if q.Get("page_size") != "100" {
					t.Errorf("transcript query = %s", r.URL.RawQuery)
				}
				if q.Get("cursor") == "" {
					return http.StatusOK, `{"transcript":[{"speaker":{"source":"microphone"},"text":"One."}],"hasMore":true,"cursor":"t1"}`
				}
				return http.StatusOK, `{"transcript":[{"speaker":{"source":"speaker","name":"Raisin Patel"},"text":"Two."}],"hasMore":false,"cursor":null}`
			},
		})
		docs := &granolaDocs{want: map[string]bool{"transcripts": true}}
		sinks, err := pass(t, kind, transcripts, allTables[:1], docs)
		if err != nil {
			t.Fatal(err)
		}
		if len(asked) != 4 || asked[0] != "include=transcript" || asked[1] != "" || len(sinks["notes"].rows) != 1 {
			t.Fatalf("asked = %q, rows = %v", asked, sinks["notes"].rows)
		}
		if len(docs.puts) != 1 || string(docs.puts[0].doc.Text) != granolaHeader+"\nMe: One.\nRaisin Patel: Two.\n" {
			t.Fatalf("puts = %v", docs.puts)
		}
	})
	t.Run("fails a transcript read that fails part way", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes": func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
			"/v1/notes/not_1": func(r *http.Request) (int, string) {
				if r.URL.Query().Get("include") == "transcript" {
					return http.StatusRequestEntityTooLarge, `{}`
				}
				return http.StatusOK, granolaNote("not_1")
			},
			"/v1/notes/not_1/transcript": func(*http.Request) (int, string) { return http.StatusOK, `{"transcript":[],"hasMore":true}` },
		})
		docs := &granolaDocs{want: map[string]bool{"transcripts": true}}
		sinks, err := pass(t, kind, transcripts, allTables[:1], docs)
		if err == nil || len(sinks["notes"].rows) != 0 || len(docs.puts) != 0 {
			t.Fatalf("SyncPass = (%v, %v), puts = %v; want a failure before any row", sinks["notes"].rows, err, docs.putNames())
		}
	})
	// A 413 is an answer about the transcript only; without one asked for, it is Granola failing.
	t.Run("refuses a 413 for a note asked for without its transcript", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
			"/v1/notes/not_1": func(*http.Request) (int, string) { return http.StatusRequestEntityTooLarge, `{}` },
		})
		_, err := pass(t, kind, Source{Secret: granolaTestKey}, allTables[:1], &granolaDocs{})
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || refusedErr.Message != "Granola returned HTTP 413 for /v1/notes/not_1" {
			t.Fatalf("SyncPass error = %v, want a refusal", err)
		}
	})
	// Documents belong to the notes table; a read of the others offers none.
	t.Run("offers no document unless the notes table is read", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
			"/v1/notes/not_1": noteOrTranscript,
		})
		docs := &granolaDocs{want: map[string]bool{"notes": true, "transcripts": true}}
		sinks, err := pass(t, kind, transcripts, allTables[1:3], docs)
		if err != nil || len(sinks["note_attendees"].rows) != 3 || len(docs.offers) != 0 {
			t.Fatalf("SyncPass = %v, offers = %q", err, docs.offers)
		}
	})
	// The row goes and the full read's removal finds its document by key.
	t.Run("puts nothing for a note Get Note no longer finds", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1", "not_2") },
			"/v1/notes/not_1": noteOrTranscript,
			"/v1/notes/not_2": func(*http.Request) (int, string) { return http.StatusNotFound, `{}` },
		})
		docs := &granolaDocs{want: map[string]bool{"notes": true}}
		sinks, err := pass(t, kind, Source{Secret: granolaTestKey}, allTables[:1], docs)
		if err != nil || len(sinks["notes"].rows) != 1 || len(docs.offers) != 2 || !slices.Equal(docs.putNames(), []string{"notes not_1"}) {
			t.Fatalf("SyncPass = %v, offers = %q, puts = %v", err, docs.offers, docs.putNames())
		}
	})
	// The external id is the row's key, so the two must agree to the byte.
	t.Run("keys a note by the id it was listed under", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
			"/v1/notes/not_1": func(*http.Request) (int, string) { return http.StatusOK, granolaNote("NOT_1") },
		})
		docs := &granolaDocs{want: map[string]bool{"notes": true}}
		sinks, err := pass(t, kind, Source{Secret: granolaTestKey}, allTables[:2], docs)
		if err != nil || sinks["notes"].rows[0]["note_id"] != "not_1" || sinks["note_attendees"].rows[0]["note_id"] != "not_1" ||
			docs.puts[0].doc.ExternalID != "not_1" {
			t.Fatalf("SyncPass = %v, rows = %v, puts = %v", err, sinks["notes"].rows, docs.putNames())
		}
	})
	t.Run("fails a live note listed without its times", func(t *testing.T) {
		for _, listing := range []string{
			`{"id":"not_1","updated_at":"2026-01-27T16:45:00Z","deleted_at":null}`,
			`{"id":"not_1","created_at":"2026-01-27T15:30:00Z","updated_at":"yesterday","deleted_at":null}`,
		} {
			kind := granolaAt(t, map[string]func(*http.Request) (int, string){
				"/v1/notes": func(*http.Request) (int, string) {
					return http.StatusOK, `{"notes":[` + listing + `],"hasMore":false,"cursor":null}`
				},
			})
			_, err := pass(t, kind, Source{Secret: granolaTestKey}, allTables[:1], &granolaDocs{})
			if err == nil || !strings.Contains(err.Error(), "note not_1 is listed without its created_at and updated_at times") {
				t.Fatalf("SyncPass error = %v, want the listing refused", err)
			}
		}
	})
	t.Run("stops at Put's error", func(t *testing.T) {
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1", "not_2") },
			"/v1/notes/not_1": noteOrTranscript, "/v1/notes/not_2": noteOrTranscript,
		})
		failed := errors.New("store down")
		docs := &granolaDocs{want: map[string]bool{"notes": true}, fail: failed}
		if _, err := pass(t, kind, Source{Secret: granolaTestKey}, allTables[:1], docs); !errors.Is(err, failed) || len(docs.puts) != 1 {
			t.Fatalf("SyncPass error = %v after %d puts, want Put's at once", err, len(docs.puts))
		}
	})
	// Sync has nowhere to Put documents, so it wants none and never asks for a transcript.
	t.Run("reads one table through Sync without documents", func(t *testing.T) {
		g := &gets{n: map[string]int{}}
		kind := granolaAt(t, map[string]func(*http.Request) (int, string){
			"/v1/notes":       func(*http.Request) (int, string) { return http.StatusOK, granolaList("not_1") },
			"/v1/notes/not_1": counted(g, noteOrTranscript),
		})
		sink := &rowSink{}
		if _, err := kind.Sync(ctx, transcripts, Table{Name: "notes"}, sink); err != nil || len(sink.rows) != 1 ||
			fmt.Sprint(g.n) != "map[/v1/notes/not_1?:1]" {
			t.Fatalf("Sync = %v, rows = %v, gets = %v", err, sink.rows, g.n)
		}
		docs := &granolaDocs{want: map[string]bool{"notes": true}}
		if _, err := kind.SyncDocuments(ctx, transcripts, Table{Name: "notes"}, &rowSink{}, docs); err != nil || len(docs.puts) != 1 {
			t.Fatalf("SyncDocuments = %v, puts = %v", err, docs.putNames())
		}
	})
}
