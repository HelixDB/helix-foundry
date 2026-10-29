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

// googleDocs is a DocumentWriter that holds a version of some Docs and,
// as the syncer does, wants a Doc unless it holds that version or this
// read offered it already. It records each offer, Put and Drop.
type googleDocs struct {
	held   map[string]string
	offers []string
	puts   []Document
	drops  []string
	seen   map[string]bool
}

func (d *googleDocs) Want(stream, id, version string) bool {
	d.offers = append(d.offers, stream+" "+id+" "+version)
	if d.seen == nil {
		d.seen = map[string]bool{}
	}
	wanted := !d.seen[id] && d.held[id] != version
	d.seen[id] = true
	return wanted
}

func (d *googleDocs) Put(_ context.Context, _ string, doc Document) error {
	d.puts = append(d.puts, doc)
	return nil
}

func (d *googleDocs) Drop(_, id string) error {
	d.drops = append(d.drops, id)
	return nil
}

func (d *googleDocs) Held(_, id string) (string, bool) {
	version, ok := d.held[id]
	return version, ok
}

func (d *googleDocs) Paused(string) bool { return false }

// googleChanges is a ChangeWriter that logs each change by its key, in the
// order written, keeps the rows upserted, and takes documents as googleDocs.
type googleChanges struct {
	googleDocs
	log    []string
	rows   map[string][]map[string]any
	resets map[string]string
}

// googleKeyOf is a row's key as the log names it.
func googleKeyOf(table string, row map[string]any) string {
	switch table {
	case "files":
		return fmt.Sprint(row["file_id"])
	case "event_attendees":
		return fmt.Sprint(row["event_id"], " ", row["email"])
	}
	return fmt.Sprint(row["event_id"])
}

func (c *googleChanges) Upsert(table string, row map[string]any) error {
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return c.UpsertJSON(table, b)
}

func (c *googleChanges) UpsertJSON(table string, b []byte) error {
	var row map[string]any
	if err := json.Unmarshal(b, &row); err != nil {
		return err
	}
	if c.rows == nil {
		c.rows = map[string][]map[string]any{}
	}
	c.rows[table] = append(c.rows[table], row)
	c.log = append(c.log, "upsert "+table+" "+googleKeyOf(table, row))
	return nil
}

func (c *googleChanges) Delete(table string, key map[string]any) error {
	c.log = append(c.log, "delete "+table+" "+googleKeyOf(table, key))
	return nil
}

func (c *googleChanges) DeleteWhere(table, column string, value any) error {
	c.log = append(c.log, fmt.Sprintf("cascade %s %s=%v", table, column, value))
	return nil
}

func (c *googleChanges) Reset(table, reason string) {
	if c.resets == nil {
		c.resets = map[string]string{}
	}
	c.resets[table] = reason
}

// failingChanges is googleChanges that fails the first change of kind at
// (upsert, delete or cascade) to table, and every change after it.
type failingChanges struct {
	*googleChanges
	table, at string
	err       error
	failed    bool
}

func (c *failingChanges) check(table, at string) error {
	if c.failed || table == c.table && at == c.at {
		c.failed = true
		return c.err
	}
	return nil
}

func (c *failingChanges) Upsert(table string, row map[string]any) error {
	_ = c.googleChanges.Upsert(table, row)
	return c.check(table, "upsert")
}

func (c *failingChanges) Delete(table string, key map[string]any) error {
	_ = c.googleChanges.Delete(table, key)
	return c.check(table, "delete")
}

func (c *failingChanges) DeleteWhere(table, column string, value any) error {
	_ = c.googleChanges.DeleteWhere(table, column, value)
	return c.check(table, "cascade")
}

// googleStartRoute answers Drive's start page token of each feed in starts,
// by drive id ("" the user's); a shared drive it lacks is not found.
func googleStartRoute(t *testing.T, starts map[string]string) func(*http.Request) (int, string) {
	return func(r *http.Request) (int, string) {
		q := r.URL.Query()
		if q.Get("supportsAllDrives") != "true" || len(q) != 1+len(q["driveId"]) {
			t.Errorf("startPageToken query = %s", r.URL.RawQuery)
		}
		token, ok := starts[q.Get("driveId")]
		if !ok {
			return googleFailure(http.StatusNotFound, "Shared drive not found: "+q.Get("driveId"), "notFound")(r)
		}
		return http.StatusOK, `{"kind":"drive#startPageToken","startPageToken":"` + token + `"}`
	}
}

// googleFeedRoute serves Drive's change feeds: pages[drive][pageToken] is
// the page of drive's feed ("" the user's) from that token. It checks each
// request asks for every drive's items, removed ones and a listing's fields.
func googleFeedRoute(t *testing.T, pages map[string]map[string]string) func(*http.Request) (int, string) {
	return func(r *http.Request) (int, string) {
		q := r.URL.Query()
		fields := "nextPageToken,newStartPageToken,changes(changeType,removed,fileId,file(" + googleFileFieldList + ",trashed))"
		if q.Get("pageSize") != "1000" || q.Get("includeItemsFromAllDrives") != "true" || q.Get("supportsAllDrives") != "true" ||
			q.Get("includeRemoved") != "true" || q.Get("fields") != fields || !strings.Contains(googleFileFields, googleFileFieldList) {
			t.Errorf("changes query = %s", r.URL.RawQuery)
		}
		page, ok := pages[q.Get("driveId")][q.Get("pageToken")]
		if !ok {
			t.Errorf("changes of %q from %q", q.Get("driveId"), q.Get("pageToken"))
			return http.StatusNotFound, `{}`
		}
		return http.StatusOK, page
	}
}

// googleEventFeed serves one calendar's events: pages["window"] is its
// window's list, pages[token] its changes after a sync token, and
// "<either>/<pageToken>" each later page. A window list must ask for the
// days of events a full read keeps; every list asks for instances and
// cancelled events, and a sync token's no time.
func googleEventFeed(t *testing.T, days int, pages map[string]string) func(*http.Request) (int, string) {
	return func(r *http.Request) (int, string) {
		q := r.URL.Query()
		if q.Get("singleEvents") != "true" || q.Get("showDeleted") != "true" || q.Get("maxResults") != "2500" || q.Has("orderBy") {
			t.Errorf("events query = %s", r.URL.RawQuery)
		}
		key := q.Get("syncToken")
		if key == "" {
			key = "window"
			from, errFrom := time.Parse(time.RFC3339, q.Get("timeMin"))
			to, errTo := time.Parse(time.RFC3339, q.Get("timeMax"))
			now := time.Now().UTC()
			if errFrom != nil || errTo != nil || now.AddDate(0, 0, -days).Sub(from).Abs() > time.Minute || now.AddDate(0, 0, 30).Sub(to).Abs() > time.Minute {
				t.Errorf("window query = %s", r.URL.RawQuery)
			}
		} else if q.Has("timeMin") || q.Has("timeMax") {
			t.Errorf("sync token query = %s", r.URL.RawQuery)
		}
		if token := q.Get("pageToken"); token != "" {
			key += "/" + token
		}
		page, ok := pages[key]
		if !ok {
			t.Errorf("%s: no page %q", r.URL.Path, key)
			return http.StatusNotFound, `{}`
		}
		return http.StatusOK, page
	}
}

// googleTestEvent is an event starting at start for an hour, with fields
// added (a JSON object's members, or "").
func googleTestEvent(id string, start time.Time, fields string) string {
	if fields != "" {
		fields = "," + fields
	}
	return fmt.Sprintf(`{"id":%q,"status":"confirmed","summary":"Event %s","start":{"dateTime":%q},"end":{"dateTime":%q}%s}`,
		id, id, start.UTC().Format(time.RFC3339), start.Add(time.Hour).UTC().Format(time.RFC3339), fields)
}

// googleChangeFile is a file as a change shows it: doc1, the Q3 plan, at
// version, unless id names another file, in the trash when trashed.
func googleChangeFile(id, mimeType, modified, version, extra string) string {
	return fmt.Sprintf(`{"id":%q,"name":"File %s","mimeType":%q,"owners":[{"emailAddress":"ada@acme.com"}],"modifiedTime":%q,`+
		`"webViewLink":"https://docs.google.com/document/d/%s/edit","version":%q%s}`, id, id, mimeType, modified, id, version, extra)
}

func TestGoogleChanges(t *testing.T) {
	ctx := context.Background()
	src := googleSource()
	allFollowed := []Table{{Name: "files"}, {Name: "calendar_events"}, {Name: "event_attendees"}}
	now := time.Now().UTC()
	soon := now.Add(48 * time.Hour).Truncate(time.Second)
	// calendarList lists Ada's calendar, the holidays' and Grace's, of which she sees only the free/busy.
	calendarList := func(*http.Request) (int, string) {
		return http.StatusOK, `{"items":[{"id":"ada@acme.com"},{"id":"en.uk#holiday@group.v.calendar.google.com"},{"id":"grace@acme.com"}]}`
	}
	unchanged := func() map[string]func(*http.Request) (int, string) {
		return map[string]func(*http.Request) (int, string){
			"/drive/v3/drives":                   googleDrivesRoute(t, map[string]string{"": `{"drives":[{"id":"drv1"}]}`}),
			"/drive/v3/changes/startPageToken":   googleStartRoute(t, map[string]string{"": "500", "drv1": "600"}),
			"/drive/v3/changes":                  googleFeedRoute(t, map[string]map[string]string{"": {"100": `{"newStartPageToken":"101"}`}, "drv1": {"200": `{"newStartPageToken":"201"}`}}),
			"/calendar/v3/users/me/calendarList": calendarList,
			"/calendar/v3/calendars/ada@acme.com/events": googleEventFeed(t, 90, map[string]string{
				"s1": `{"items":[],"nextSyncToken":"s2"}`, "window": `{"items":[],"nextSyncToken":"w1"}`}),
			"/calendar/v3/calendars/en.uk#holiday@group.v.calendar.google.com/events": googleEventFeed(t, 90, map[string]string{
				"h1": `{"nextSyncToken":"h2"}`, "window": `{"nextSyncToken":"hw"}`}),
			"/calendar/v3/calendars/grace@acme.com/events": googleFailure(http.StatusForbidden, "Forbidden", "forbidden"),
		}
	}
	cursor := json.RawMessage(`{"files":{"user":"100","drives":{"drv1":"200"}},` +
		`"events":{"ada@acme.com":"s1","en.uk#holiday@group.v.calendar.google.com":"h1"}}`)
	// changes reads Changes of tables from cursor under routes, failing the test on an error.
	changes := func(t *testing.T, w *googleWorkspace, tables []Table, cursor json.RawMessage, held map[string]string) (*googleChanges, string) {
		t.Helper()
		got := &googleChanges{googleDocs: googleDocs{held: held}}
		next, err := googleAt(t, w).Changes(ctx, src, tables, cursor, got)
		if err != nil {
			t.Fatal(err)
		}
		return got, string(next)
	}

	t.Run("follows files and the event tables, and reads users and calendars in full", func(t *testing.T) {
		kind := googleAt(t, &googleWorkspace{})
		want := map[string]Capture{"users": CaptureFull, "files": CaptureCursor, "calendars": CaptureFull,
			"calendar_events": CaptureCursor, "event_attendees": CaptureCursor}
		for table, capture := range want {
			if got := kind.Capture(src, table); got != capture {
				t.Errorf("Capture(%s) = %s, want %s", table, got, capture)
			}
		}
	})
	// Calendar gives a sync token only on a list's last page.
	t.Run("starts from each Drive feed's token and each calendar's window", func(t *testing.T) {
		routes := unchanged()
		routes["/drive/v3/drives"] = googleDrivesRoute(t, map[string]string{"": `{"drives":[{"id":"drv1"},{"id":"drv2"}]}`})
		routes["/calendar/v3/calendars/ada@acme.com/events"] = googleEventFeed(t, 90, map[string]string{
			"window": `{"items":[` + googleTestEvent("ev1", soon, "") + `],"nextPageToken":"p2"}`, "window/p2": `{"items":[],"nextSyncToken":"w1"}`})
		w := &googleWorkspace{routes: routes}
		start, err := googleAt(t, w).Start(ctx, src, allFollowed)
		if err != nil {
			t.Fatal(err)
		}
		// drv2, left since the drives were listed, has no token; Grace's calendar, refused, none.
		if want := `{"files":{"user":"500","drives":{"drv1":"600"}},"events":{"ada@acme.com":"w1","en.uk#holiday@group.v.calendar.google.com":"hw"}}`; string(start.Cursor) != want {
			t.Fatalf("cursor = %s, want %s", start.Cursor, want)
		}
		if start.Source != nil || start.Close != nil {
			t.Fatalf("start = %+v", start)
		}
		if listed := w.requested("/drive/v3/files"); len(listed) != 0 {
			t.Fatalf("Start listed files: %v", listed)
		}
		if events := w.requested("/calendar/v3/calendars/"); len(events) != 4 {
			t.Fatalf("event lists = %v; want Ada's two pages, the holidays' and Grace's", events)
		}
	})
	t.Run("starts only the feeds of the tables followed", func(t *testing.T) {
		w := &googleWorkspace{routes: unchanged()}
		start, err := googleAt(t, w).Start(ctx, src, []Table{{Name: "files"}})
		if err != nil || string(start.Cursor) != `{"files":{"user":"500","drives":{"drv1":"600"}},"events":null}` || len(w.requested("/calendar/")) != 0 {
			t.Fatalf("Start = (%s, %v) after %v", start.Cursor, err, w.requested("/"))
		}
		w = &googleWorkspace{routes: unchanged()}
		start, err = googleAt(t, w).Start(ctx, src, []Table{{Name: "event_attendees"}})
		if err != nil || !strings.HasPrefix(string(start.Cursor), `{"files":null,"events":{"ada@acme.com":"w1",`) || len(w.requested("/drive/")) != 0 {
			t.Fatalf("Start = (%s, %v) after %v", start.Cursor, err, w.requested("/"))
		}
	})
	t.Run("fails a start Google refuses", func(t *testing.T) {
		for name, route := range map[string]string{"the user's token": "/drive/v3/changes/startPageToken", "the drives": "/drive/v3/drives",
			"the calendar list": "/calendar/v3/users/me/calendarList"} {
			routes := unchanged()
			routes[route] = googleFailure(http.StatusBadRequest, "Invalid Value", "invalid")
			if _, err := googleAt(t, &googleWorkspace{routes: routes}).Start(ctx, src, allFollowed); err == nil {
				t.Errorf("%s: Start took a refusal", name)
			}
		}
		routes := unchanged()
		routes["/drive/v3/changes/startPageToken"] = func(*http.Request) (int, string) { return http.StatusOK, `{}` }
		if _, err := googleAt(t, &googleWorkspace{routes: routes}).Start(ctx, src, allFollowed); err == nil {
			t.Error("Start took an answer without a token")
		}
	})
	// Google's own full sync takes its token from a list from a time; should
	// Calendar give a window's list none, the calendar is listed from its
	// window at every change read.
	t.Run("lists the window of a calendar Calendar gives no sync token", func(t *testing.T) {
		routes := unchanged()
		routes["/calendar/v3/calendars/ada@acme.com/events"] = googleEventFeed(t, 90, map[string]string{
			"window": `{"items":[` + googleTestEvent("ev1", soon, "") + `,{"id":"ev2","status":"cancelled"}]}`})
		routes["/calendar/v3/calendars/en.uk#holiday@group.v.calendar.google.com/events"] = googleEventFeed(t, 90, map[string]string{
			"window": `{"nextSyncToken":"hw"}`, "hw": `{"nextSyncToken":"h3"}`, "h1": `{"nextSyncToken":"h2"}`})
		start, err := googleAt(t, &googleWorkspace{routes: routes}).Start(ctx, src, allFollowed[1:])
		if err != nil || string(start.Cursor) != `{"files":null,"events":{"ada@acme.com":"","en.uk#holiday@group.v.calendar.google.com":"hw"}}` {
			t.Fatalf("Start = (%s, %v)", start.Cursor, err)
		}
		got, next := changes(t, &googleWorkspace{routes: routes}, allFollowed[1:], start.Cursor, nil)
		want := []string{"upsert calendar_events ada@acme.com:ev1", "cascade event_attendees event_id=ada@acme.com:ev1",
			"delete calendar_events ada@acme.com:ev2", "cascade event_attendees event_id=ada@acme.com:ev2"}
		if !slices.Equal(got.log, want) || len(got.resets) != 0 ||
			next != `{"files":null,"events":{"ada@acme.com":"","en.uk#holiday@group.v.calendar.google.com":"h3"}}` {
			t.Fatalf("log = %v, resets = %v, next = %s", got.log, got.resets, next)
		}
		// A sync token's list without a new one leaves the calendar to its window next time.
		routes["/calendar/v3/calendars/ada@acme.com/events"] = googleEventFeed(t, 90, map[string]string{"s1": `{"items":[]}`})
		if _, next := changes(t, &googleWorkspace{routes: routes}, allFollowed[1:], cursor, nil); next !=
			`{"files":null,"events":{"ada@acme.com":"","en.uk#holiday@group.v.calendar.google.com":"h2"}}` {
			t.Fatalf("next = %s", next)
		}
	})
	// An hourly read of nothing: one page of each feed, and the list and one page of each calendar.
	t.Run("writes nothing and exports nothing when nothing changed", func(t *testing.T) {
		w := &googleWorkspace{routes: unchanged()}
		got, next := changes(t, w, allFollowed, cursor, nil)
		if len(got.log) != 0 || len(got.resets) != 0 || len(got.offers) != 0 {
			t.Fatalf("log = %v, resets = %v, offers = %v", got.log, got.resets, got.offers)
		}
		if want := `{"files":{"user":"101","drives":{"drv1":"201"}},"events":{"ada@acme.com":"s2","en.uk#holiday@group.v.calendar.google.com":"h2"}}`; next != want {
			t.Fatalf("next = %s, want %s", next, want)
		}
		// Grace's calendar has no token, so it is tried from its window.
		if asked := len(w.requested("/")); asked != 7 {
			t.Fatalf("asked %d: %v", asked, w.requested("/"))
		}
	})
	t.Run("writes a changed file's row, exports a changed Doc and deletes a gone file", func(t *testing.T) {
		routes := unchanged()
		edited := googleChangeFile("doc1", googleDocType, "2026-10-29T08:00:00Z", "43", "")
		routes["/drive/v3/changes"] = googleFeedRoute(t, map[string]map[string]string{
			"": {
				"100": `{"changes":[{"changeType":"file","fileId":"doc1","file":` + edited + `},
					{"changeType":"file","fileId":"sheet1","file":` + googleChangeFile("sheet1", "application/vnd.google-apps.spreadsheet",
					"2026-10-29T08:00:00Z", "10", `,"trashed":true`) + `},
					{"changeType":"drive","driveId":"drv1","drive":{"id":"drv1","name":"Legal"}}],"nextPageToken":"100b"}`,
				"100b": `{"changes":[{"changeType":"file","fileId":"fold1","removed":true},
					{"changeType":"file","fileId":"doc4","file":` + googleChangeFile("doc4", googleDocType, "2026-10-29T09:00:00Z", "1", "") + `},
					{"changeType":"file","fileId":"pdf1","file":` + googleChangeFile("pdf1", "application/pdf", "2026-10-29T09:00:00Z", "4", `,"driveId":"drv2"`) + `}],
					"newStartPageToken":"102"}`,
			},
			// pdf1 moved from drv1 to another drive the user can read; deck1 was deleted.
			"drv1": {"200": `{"changes":[{"fileId":"pdf1","removed":true},{"fileId":"deck1","removed":true}],"newStartPageToken":"202"}`},
		})
		for _, id := range []string{"doc1", "doc4"} {
			routes["/drive/v3/files/"+id+"/export"] = func(*http.Request) (int, string) { return http.StatusOK, "## " + id + "\n\nEdited." }
		}
		w := &googleWorkspace{routes: routes}
		got, next := changes(t, w, []Table{{Name: "files"}}, cursor, map[string]string{"doc1": "1|2026-10-20T15:30:00.250Z|42|old"})
		want := []string{"upsert files doc1", "upsert files doc4", "upsert files pdf1", "delete files deck1", "delete files fold1", "delete files sheet1"}
		if !slices.Equal(got.log, want) || len(got.resets) != 0 {
			t.Fatalf("log = %v, resets = %v", got.log, got.resets)
		}
		if next != `{"files":{"user":"102","drives":{"drv1":"202"}},"events":null}` || len(w.requested("/calendar/")) != 0 {
			t.Fatalf("next = %s", next)
		}
		// Its row is the full read's.
		row := got.rows["files"][0]
		if fmt.Sprint(row) != fmt.Sprint(map[string]any{"file_id": "doc1", "name": "File doc1", "mime_type": googleDocType, "drive_id": nil,
			"parent_id": nil, "owner_email": "ada@acme.com", "last_modifying_user_email": nil, "created_at": nil, "modified_at": "2026-10-29T08:00:00Z",
			"size": nil, "web_view_link": "https://docs.google.com/document/d/doc1/edit", "shared": false, "starred": false}) {
			t.Fatalf("row = %v", row)
		}
		if len(got.puts) != 2 || got.puts[0].ExternalID != "doc1" || !strings.HasPrefix(got.puts[0].Version, "1|2026-10-29T08:00:00Z|43|") ||
			string(got.puts[0].Text) != "# File doc1\n\nOwner: ada@acme.com\nLast modified: 2026-10-29 08:00 UTC\nFile id: doc1\n\n## doc1\n\nEdited.\n" ||
			got.puts[1].ExternalID != "doc4" {
			t.Fatalf("puts = %+v", got.puts)
		}
	})
	// A list that changes while it is paged shows a file again, as it is later.
	t.Run("takes a file's last change in a feed", func(t *testing.T) {
		routes := unchanged()
		sheet := func(id string) string {
			return `{"fileId":"` + id + `","file":` + googleChangeFile(id, "application/vnd.google-apps.spreadsheet", "2026-10-29T08:00:00Z", "10", "") + `}`
		}
		routes["/drive/v3/changes"] = googleFeedRoute(t, map[string]map[string]string{
			"": {"100": `{"changes":[` + sheet("sheet1") + `,{"fileId":"sheet2","removed":true}],"nextPageToken":"100b"}`,
				"100b": `{"changes":[{"fileId":"sheet1","removed":true},` + sheet("sheet2") + `],"newStartPageToken":"101"}`},
			"drv1": {"200": `{"newStartPageToken":"201"}`}})
		got, _ := changes(t, &googleWorkspace{routes: routes}, []Table{{Name: "files"}}, cursor, nil)
		if fmt.Sprint(got.log) != "[upsert files sheet1 upsert files sheet2 delete files sheet1]" {
			t.Fatalf("log = %v", got.log)
		}
	})
	// A Doc offered by two feeds, as one in a shared drive shared with the user, is exported once.
	t.Run("exports a Doc two feeds show once", func(t *testing.T) {
		routes := unchanged()
		doc := `{"changes":[{"fileId":"doc1","file":` + googleChangeFile("doc1", googleDocType, "2026-10-29T08:00:00Z", "43", `,"driveId":"drv1"`) + `}],`
		routes["/drive/v3/changes"] = googleFeedRoute(t, map[string]map[string]string{
			"": {"100": doc + `"newStartPageToken":"101"}`}, "drv1": {"200": doc + `"newStartPageToken":"201"}`}})
		routes["/drive/v3/files/doc1/export"] = func(*http.Request) (int, string) { return http.StatusOK, "Text." }
		w := &googleWorkspace{routes: routes}
		got, _ := changes(t, w, []Table{{Name: "files"}}, cursor, nil)
		if fmt.Sprint(got.log) != "[upsert files doc1 upsert files doc1]" || len(got.puts) != 1 || len(w.requested("/drive/v3/files/doc1/export")) != 1 {
			t.Fatalf("log = %v, puts = %d, exports = %v", got.log, len(got.puts), w.requested("/drive/v3/files/"))
		}
	})
	// A full read leaves such a Doc unoffered, so the stream drops it; Drive's version changes with the setting.
	t.Run("drops the document of a Doc its owner no longer lets the user download", func(t *testing.T) {
		withheld := func(id string) string {
			return googleChangeFile(id, googleDocType, "2026-10-20T15:30:00Z", "43", `,"capabilities":{"canDownload":false}`)
		}
		var f map[string]any
		if err := json.Unmarshal([]byte(withheld("doc2")), &f); err != nil {
			t.Fatal(err)
		}
		_, doc2, err := googleFile("doc2", f)
		if err != nil || !doc2.withheld {
			t.Fatalf("googleFile = (%+v, %v)", doc2, err)
		}
		routes := unchanged()
		routes["/drive/v3/changes"] = googleFeedRoute(t, map[string]map[string]string{
			"": {"100": `{"changes":[{"fileId":"doc1","file":` + withheld("doc1") + `},{"fileId":"doc2","file":` + withheld("doc2") + `},` +
				`{"fileId":"doc3","file":` + withheld("doc3") + `}],"newStartPageToken":"101"}`},
			"drv1": {"200": `{"newStartPageToken":"201"}`}})
		w := &googleWorkspace{routes: routes}
		// doc1 is held at an older version, doc2 at the one listed, which a
		// full read leaves out, and doc3 not at all.
		got, _ := changes(t, w, []Table{{Name: "files"}}, cursor, map[string]string{"doc1": "1|old", "doc2": doc2.version})
		if fmt.Sprint(got.drops) != "[doc1]" || len(got.offers) != 2 || len(got.puts) != 0 || len(w.requested("/drive/v3/files/")) != 0 ||
			len(got.rows["files"]) != 3 {
			t.Fatalf("drops = %v, offers = %v, exports = %v", got.drops, got.offers, w.requested("/drive/v3/files/"))
		}
	})
	t.Run("skips a changed Doc Drive fails to export, as a full read does", func(t *testing.T) {
		noBackoff(t)
		routes := unchanged()
		routes["/drive/v3/changes"] = googleFeedRoute(t, map[string]map[string]string{
			"":     {"100": `{"changes":[{"fileId":"doc1","file":` + googleChangeFile("doc1", googleDocType, "2026-10-29T08:00:00Z", "43", "") + `}],"newStartPageToken":"101"}`},
			"drv1": {"200": `{"newStartPageToken":"201"}`}})
		routes["/drive/v3/files/doc1/export"] = func(*http.Request) (int, string) { return http.StatusBadGateway, `` }
		got, _ := changes(t, &googleWorkspace{routes: routes}, []Table{{Name: "files"}}, cursor, nil)
		if fmt.Sprint(got.log) != "[upsert files doc1]" || len(got.puts) != 0 || len(got.drops) != 0 {
			t.Fatalf("log = %v, puts = %v, drops = %v", got.log, got.puts, got.drops)
		}
	})
	// Drive refuses a token it no longer takes with a 400 naming the parameter.
	t.Run("reads files in full when Drive no longer takes a page token", func(t *testing.T) {
		routes := unchanged()
		routes["/drive/v3/changes"] = func(*http.Request) (int, string) {
			return http.StatusBadRequest, `{"error":{"code":400,"message":"Invalid Value","errors":[{"domain":"global","reason":"invalid",
				"message":"Invalid Value","locationType":"parameter","location":"pageToken"}]}}`
		}
		w := &googleWorkspace{routes: routes}
		got, next := changes(t, w, []Table{{Name: "files"}}, cursor, nil)
		// The user's feed takes a new token before the full read; drv1's keeps its own, unread.
		if len(got.log) != 0 || got.resets["files"] != "Drive no longer holds the changes after the cursor" ||
			next != `{"files":{"user":"500","drives":{"drv1":"200"}},"events":null}` || len(w.requested("/drive/v3/changes?")) != 1 {
			t.Fatalf("log = %v, resets = %v, next = %s, feeds = %v", got.log, got.resets, next, w.requested("/drive/v3/changes?"))
		}
		// Another 400 is no expired token.
		routes["/drive/v3/changes"] = googleFailure(http.StatusBadRequest, "Invalid Value", "invalid")
		if _, err := googleAt(t, &googleWorkspace{routes: routes}).Changes(ctx, src, []Table{{Name: "files"}}, cursor, &googleChanges{}); errors.Is(err, errTokenGone) ||
			refusalOf(err) != "Google returned HTTP 400 for /drive/v3/changes" {
			t.Fatalf("Changes error = %v", err)
		}
	})
	t.Run("reads files in full when the user joins or leaves a shared drive", func(t *testing.T) {
		for name, tc := range map[string]struct{ drives, reason, next string }{
			"joins": {`{"drives":[{"id":"drv1"},{"id":"drv3"}]}`, "the user joined shared drive drv3",
				`{"files":{"user":"100","drives":{"drv1":"200","drv3":"700"}},"events":null}`},
			"leaves": {`{"drives":[]}`, "the user left shared drive drv1", `{"files":{"user":"100","drives":{}},"events":null}`},
		} {
			routes := unchanged()
			routes["/drive/v3/drives"] = googleDrivesRoute(t, map[string]string{"": tc.drives})
			routes["/drive/v3/changes/startPageToken"] = googleStartRoute(t, map[string]string{"drv3": "700"})
			w := &googleWorkspace{routes: routes}
			got, next := changes(t, w, []Table{{Name: "files"}}, cursor, nil)
			if got.resets["files"] != tc.reason || next != tc.next || len(w.requested("/drive/v3/changes?")) != 0 {
				t.Errorf("%s: resets = %v, next = %s, feeds = %v", name, got.resets, next, w.requested("/drive/v3/changes?"))
			}
		}
	})
	t.Run("reads files in full when a shared drive's feed is refused", func(t *testing.T) {
		routes := unchanged()
		feeds := googleFeedRoute(t, map[string]map[string]string{"": {"100": `{"newStartPageToken":"101"}`}})
		routes["/drive/v3/changes"] = func(r *http.Request) (int, string) {
			if r.URL.Query().Get("driveId") == "drv1" {
				return googleFailure(http.StatusNotFound, "Shared drive not found: drv1", "notFound")(r)
			}
			return feeds(r)
		}
		got, next := changes(t, &googleWorkspace{routes: routes}, []Table{{Name: "files"}}, cursor, nil)
		if got.resets["files"] != "the user left shared drive drv1" || next != `{"files":{"user":"101","drives":{}},"events":null}` {
			t.Fatalf("resets = %v, next = %s", got.resets, next)
		}
	})
	t.Run("reads files in full, from new tokens, without a cursor for them", func(t *testing.T) {
		w := &googleWorkspace{routes: unchanged()}
		got, next := changes(t, w, []Table{{Name: "files"}}, json.RawMessage(`{"files":null,"events":null}`), nil)
		if got.resets["files"] != "the table has no change cursor yet" || next != `{"files":{"user":"500","drives":{"drv1":"600"}},"events":null}` {
			t.Fatalf("resets = %v, next = %s", got.resets, next)
		}
	})
	// Ending early would lose changes the next read starts after.
	for name, page := range map[string]string{
		"is not JSON":                         `<html>Bad gateway</html>`,
		"is null":                             `null`,
		"names no file":                       `{"changes":[{"changeType":"file","removed":true}],"newStartPageToken":"101"}`,
		"lists null":                          `{"changes":[null],"newStartPageToken":"101"}`,
		"shows a file without it":             `{"changes":[{"fileId":"doc1"}],"newStartPageToken":"101"}`,
		"ends without a start token":          `{"changes":[]}`,
		"gives back the token it was asked":   `{"changes":[],"nextPageToken":"100"}`,
		"has a list that is not one":          `{"changes":{"fileId":"doc1"},"newStartPageToken":"101"}`,
		"has a Doc without its modified time": `{"changes":[{"fileId":"doc1","file":{"id":"doc1","mimeType":"` + googleDocType + `"}}],"newStartPageToken":"101"}`,
	} {
		t.Run("fails a change page that "+name, func(t *testing.T) {
			routes := unchanged()
			routes["/drive/v3/changes"] = func(*http.Request) (int, string) { return http.StatusOK, page }
			got := &googleChanges{}
			if _, err := googleAt(t, &googleWorkspace{routes: routes}).Changes(ctx, src, []Table{{Name: "files"}}, cursor, got); err == nil || len(got.resets) != 0 {
				t.Fatalf("Changes = %v, resets = %v; want a failure", err, got.resets)
			}
		})
	}
	t.Run("fails a feed refused after its first page", func(t *testing.T) {
		routes := unchanged()
		routes["/drive/v3/changes"] = func(r *http.Request) (int, string) {
			if r.URL.Query().Get("pageToken") == "100" {
				return http.StatusOK, `{"changes":[],"nextPageToken":"100b"}`
			}
			return googleFailure(http.StatusNotFound, "Not Found", "notFound")(r)
		}
		_, err := googleAt(t, &googleWorkspace{routes: routes}).Changes(ctx, src, []Table{{Name: "files"}}, cursor, &googleChanges{})
		if want := "Google stopped listing /drive/v3/changes as ada@acme.com part way through; try again"; refusalOf(err) != want {
			t.Fatalf("Changes error = %v", err)
		}
		// The user's own feed refused is no shared drive left.
		routes["/drive/v3/changes"] = googleFailure(http.StatusNotFound, "Not Found", "notFound")
		if _, err := googleAt(t, &googleWorkspace{routes: routes}).Changes(ctx, src, []Table{{Name: "files"}}, cursor, &googleChanges{}); !errors.Is(err, errNoAccess) {
			t.Fatalf("Changes error = %v", err)
		}
	})

	// ev1 changed in the window; ev2 and an instance of ev3 were cancelled;
	// ev5 moved past the window and ev6 is long past: neither is in it.
	adaChanges := map[string]string{
		"s1": `{"items":[` + googleTestEvent("ev1", soon, `"attendees":[{"email":"Ada@acme.com","responseStatus":"accepted","self":true},
			{"email":"grace@acme.com","responseStatus":"needsAction"},{"email":"room@resource.calendar.google.com","resource":true}]`) + `],"nextPageToken":"p2"}`,
		"s1/p2": `{"items":[{"id":"ev2","status":"cancelled"},{"id":"ev3_20261101T090000Z","status":"cancelled","recurringEventId":"ev3"},
			` + googleTestEvent("ev5", now.AddDate(0, 0, 60), "") + `,` + googleTestEvent("ev6", now.AddDate(0, 0, -200), "") + `],"nextSyncToken":"s2"}`,
	}
	t.Run("replaces a changed event's rows, removes a cancelled one's, and leaves one outside the window", func(t *testing.T) {
		routes := unchanged()
		routes["/calendar/v3/calendars/ada@acme.com/events"] = googleEventFeed(t, 90, adaChanges)
		w := &googleWorkspace{routes: routes}
		got, next := changes(t, w, allFollowed[1:], cursor, nil)
		want := []string{"upsert calendar_events ada@acme.com:ev1", "cascade event_attendees event_id=ada@acme.com:ev1",
			"upsert event_attendees ada@acme.com:ev1 ada@acme.com", "upsert event_attendees ada@acme.com:ev1 grace@acme.com",
			"delete calendar_events ada@acme.com:ev2", "cascade event_attendees event_id=ada@acme.com:ev2",
			"delete calendar_events ada@acme.com:ev3_20261101T090000Z", "cascade event_attendees event_id=ada@acme.com:ev3_20261101T090000Z"}
		if !slices.Equal(got.log, want) || len(got.resets) != 0 {
			t.Fatalf("log = %v, resets = %v", got.log, got.resets)
		}
		if row := got.rows["calendar_events"][0]; row["start_at"] != soon.Format(time.RFC3339) || row["attendee_count"] != float64(2) || row["summary"] != "Event ev1" {
			t.Fatalf("row = %v", row)
		}
		if want := `{"files":null,"events":{"ada@acme.com":"s2","en.uk#holiday@group.v.calendar.google.com":"h2"}}`; next != want {
			t.Fatalf("next = %s", next)
		}
		if len(w.requested("/drive/")) != 0 {
			t.Fatalf("asked Drive %v", w.requested("/drive/"))
		}
	})
	t.Run("takes an event's last change in a list", func(t *testing.T) {
		routes := unchanged()
		routes["/calendar/v3/calendars/ada@acme.com/events"] = googleEventFeed(t, 90, map[string]string{
			"s1":    `{"items":[` + googleTestEvent("ev1", soon, "") + `],"nextPageToken":"p2"}`,
			"s1/p2": `{"items":[{"id":"ev1","status":"cancelled"}],"nextSyncToken":"s2"}`})
		got, _ := changes(t, &googleWorkspace{routes: routes}, []Table{{Name: "calendar_events"}}, cursor, nil)
		if fmt.Sprint(got.log) != "[upsert calendar_events ada@acme.com:ev1 delete calendar_events ada@acme.com:ev1]" {
			t.Fatalf("log = %v", got.log)
		}
	})
	// The table's earlier rows go by DeleteWhere, but not the ones an earlier
	// copy wrote in the same read: they go by key.
	t.Run("takes back the attendees an earlier copy of an event wrote", func(t *testing.T) {
		ev1 := googleTestEvent("ev1", soon, `"attendees":[{"email":"ada@acme.com"},{"email":"grace@acme.com"}]`)
		for name, tc := range map[string]struct{ later, log string }{
			"cancelled": {`{"id":"ev1","status":"cancelled"}`, "[delete calendar_events ada@acme.com:ev1 cascade event_attendees event_id=ada@acme.com:ev1 " +
				"delete event_attendees ada@acme.com:ev1 ada@acme.com delete event_attendees ada@acme.com:ev1 grace@acme.com]"},
			"changed": {googleTestEvent("ev1", soon, `"attendees":[{"email":"ada@acme.com"}]`), "[upsert calendar_events ada@acme.com:ev1 " +
				"cascade event_attendees event_id=ada@acme.com:ev1 delete event_attendees ada@acme.com:ev1 ada@acme.com " +
				"delete event_attendees ada@acme.com:ev1 grace@acme.com upsert event_attendees ada@acme.com:ev1 ada@acme.com]"},
		} {
			routes := unchanged()
			routes["/calendar/v3/calendars/ada@acme.com/events"] = googleEventFeed(t, 90, map[string]string{
				"s1": `{"items":[` + ev1 + `],"nextPageToken":"p2"}`, "s1/p2": `{"items":[` + tc.later + `],"nextSyncToken":"s2"}`})
			got, _ := changes(t, &googleWorkspace{routes: routes}, allFollowed[1:], cursor, nil)
			first := "[upsert calendar_events ada@acme.com:ev1 cascade event_attendees event_id=ada@acme.com:ev1 " +
				"upsert event_attendees ada@acme.com:ev1 ada@acme.com upsert event_attendees ada@acme.com:ev1 grace@acme.com "
			if fmt.Sprint(got.log) != first+strings.TrimPrefix(tc.log, "[") {
				t.Errorf("%s: log = %v", name, got.log)
			}
		}
	})
	t.Run("writes only the event table followed", func(t *testing.T) {
		routes := unchanged()
		routes["/calendar/v3/calendars/ada@acme.com/events"] = googleEventFeed(t, 90, adaChanges)
		got, _ := changes(t, &googleWorkspace{routes: routes}, []Table{{Name: "calendar_events"}}, cursor, nil)
		if fmt.Sprint(got.log) != "[upsert calendar_events ada@acme.com:ev1 delete calendar_events ada@acme.com:ev2 "+
			"delete calendar_events ada@acme.com:ev3_20261101T090000Z]" {
			t.Fatalf("log = %v", got.log)
		}
		got, _ = changes(t, &googleWorkspace{routes: routes}, []Table{{Name: "event_attendees"}}, cursor, nil)
		if len(got.log) != 5 || slices.ContainsFunc(got.log, func(l string) bool { return strings.Contains(l, "calendar_events") }) {
			t.Fatalf("log = %v", got.log)
		}
	})
	// A calendar the user just subscribed to has no token.
	t.Run("lists a calendar without a token from its window, keeping what is in it", func(t *testing.T) {
		routes := unchanged()
		routes["/calendar/v3/calendars/en.uk#holiday@group.v.calendar.google.com/events"] = googleEventFeed(t, 90, map[string]string{
			"window": `{"items":[{"id":"hol1","status":"confirmed","start":{"date":"` + soon.Format(time.DateOnly) + `"},"end":{"date":"` +
				soon.AddDate(0, 0, 1).Format(time.DateOnly) + `"}},{"id":"hol0","status":"cancelled"}],"nextSyncToken":"hw"}`})
		got, next := changes(t, &googleWorkspace{routes: routes}, allFollowed[1:], json.RawMessage(`{"files":null,"events":{"ada@acme.com":"s1"}}`), nil)
		want := []string{"upsert calendar_events en.uk#holiday@group.v.calendar.google.com:hol1",
			"cascade event_attendees event_id=en.uk#holiday@group.v.calendar.google.com:hol1",
			"delete calendar_events en.uk#holiday@group.v.calendar.google.com:hol0",
			"cascade event_attendees event_id=en.uk#holiday@group.v.calendar.google.com:hol0"}
		if !slices.Equal(got.log, want) || len(got.resets) != 0 || !strings.Contains(next, `"en.uk#holiday@group.v.calendar.google.com":"hw"`) {
			t.Fatalf("log = %v, resets = %v, next = %s", got.log, got.resets, next)
		}
	})
	// Calendar answers a sync token it no longer takes with 410 Gone.
	t.Run("reads the event tables in full when Calendar no longer takes a sync token", func(t *testing.T) {
		routes := unchanged()
		ada := googleEventFeed(t, 90, map[string]string{"window": `{"items":[],"nextSyncToken":"w1"}`})
		routes["/calendar/v3/calendars/ada@acme.com/events"] = func(r *http.Request) (int, string) {
			if r.URL.Query().Has("syncToken") {
				return googleFailure(http.StatusGone, "Sync token is no longer valid, a full sync is required.", "fullSyncRequired")(r)
			}
			return ada(r)
		}
		got, next := changes(t, &googleWorkspace{routes: routes}, allFollowed[1:], cursor, nil)
		reason := "Google no longer holds the changes of calendar ada@acme.com"
		if fmt.Sprint(got.resets) != fmt.Sprint(map[string]string{"calendar_events": reason, "event_attendees": reason}) ||
			next != `{"files":null,"events":{"ada@acme.com":"w1","en.uk#holiday@group.v.calendar.google.com":"h2"}}` {
			t.Fatalf("resets = %v, next = %s", got.resets, next)
		}
	})
	t.Run("reads the event tables in full when a calendar leaves the list or its events are refused", func(t *testing.T) {
		for name, tc := range map[string]struct{ cursor, reason, next string }{
			"left": {`{"files":null,"events":{"ada@acme.com":"s1","gone@acme.com":"g1"}}`, "calendar gone@acme.com left the user's calendar list",
				`{"files":null,"events":{"ada@acme.com":"s2","en.uk#holiday@group.v.calendar.google.com":"hw"}}`},
			"refused": {`{"files":null,"events":{"grace@acme.com":"r1"}}`, "the user can no longer read the events of calendar grace@acme.com",
				`{"files":null,"events":{"ada@acme.com":"w1","en.uk#holiday@group.v.calendar.google.com":"hw"}}`},
			"left, listed by its window": {`{"files":null,"events":{"ada@acme.com":"s1","gone@acme.com":""}}`,
				"calendar gone@acme.com left the user's calendar list",
				`{"files":null,"events":{"ada@acme.com":"s2","en.uk#holiday@group.v.calendar.google.com":"hw"}}`},
			"refused, listed by its window": {`{"files":null,"events":{"ada@acme.com":"s1","grace@acme.com":""}}`,
				"the user can no longer read the events of calendar grace@acme.com",
				`{"files":null,"events":{"ada@acme.com":"s2","en.uk#holiday@group.v.calendar.google.com":"hw"}}`},
			"none": {`{"files":null,"events":null}`, "the table has no change cursor yet",
				`{"files":null,"events":{"ada@acme.com":"w1","en.uk#holiday@group.v.calendar.google.com":"hw"}}`},
		} {
			got, next := changes(t, &googleWorkspace{routes: unchanged()}, allFollowed[1:], json.RawMessage(tc.cursor), nil)
			if got.resets["calendar_events"] != tc.reason || got.resets["event_attendees"] != tc.reason || next != tc.next {
				t.Errorf("%s: resets = %v, next = %s", name, got.resets, next)
			}
		}
	})
	t.Run("fails the event tables on a refusal that is no token's", func(t *testing.T) {
		noBackoff(t)
		routes := unchanged()
		routes["/calendar/v3/calendars/ada@acme.com/events"] = googleFailure(http.StatusForbidden, "Calendar usage limits exceeded.", "quotaExceeded")
		got := &googleChanges{}
		_, err := googleAt(t, &googleWorkspace{routes: routes}).Changes(ctx, src, allFollowed[1:], cursor, got)
		if refusalOf(err) != "this service account has used up its Google rate limit; try again in a few minutes" || len(got.resets) != 0 {
			t.Fatalf("Changes error = %v, resets = %v", err, got.resets)
		}
	})
	// A spool that fails takes no more changes.
	t.Run("stops at the writer's error", func(t *testing.T) {
		routes := unchanged()
		routes["/drive/v3/changes"] = googleFeedRoute(t, map[string]map[string]string{
			"": {"100": `{"changes":[{"fileId":"sheet1","file":` + googleChangeFile("sheet1", "application/vnd.google-apps.spreadsheet",
				"2026-10-29T08:00:00Z", "10", "") + `},{"fileId":"fold1","removed":true}],"newStartPageToken":"101"}`},
			"drv1": {"200": `{"changes":[{"fileId":"deck1","removed":true}],"newStartPageToken":"201"}`}})
		routes["/calendar/v3/calendars/ada@acme.com/events"] = googleEventFeed(t, 90, adaChanges)
		full := errors.New("spool full")
		for _, table := range []string{"files", "calendar_events", "event_attendees"} {
			for _, at := range []string{"upsert", "delete", "cascade"} {
				got := &googleChanges{}
				failing := &failingChanges{googleChanges: got, table: table, at: at, err: full}
				_, err := googleAt(t, &googleWorkspace{routes: routes}).Changes(ctx, src, allFollowed, cursor, failing)
				wrote := slices.ContainsFunc(got.log, func(l string) bool { return strings.HasPrefix(l, at+" "+table+" ") })
				if wrote != errors.Is(err, full) {
					t.Errorf("%s %s: Changes error = %v after %v", at, table, err, got.log)
				}
			}
		}
	})
	t.Run("starts over on a cursor it can't read", func(t *testing.T) {
		_, err := googleAt(t, &googleWorkspace{}).Changes(ctx, src, allFollowed, json.RawMessage(`[]`), &googleChanges{})
		if !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("Changes error = %v, want ErrCursorExpired", err)
		}
	})
	t.Run("refuses a table it doesn't follow and settings Prepare never stores", func(t *testing.T) {
		var invalidErr *InvalidError
		if _, err := googleAt(t, &googleWorkspace{}).Changes(ctx, src, []Table{{Name: "users"}}, cursor, &googleChanges{}); !errors.As(err, &invalidErr) {
			t.Fatalf("Changes error = %v, want InvalidError", err)
		}
		bad := Source{Secret: "{}", Settings: googleSettings}
		if _, err := googleAt(t, &googleWorkspace{}).Changes(ctx, bad, allFollowed, cursor, &googleChanges{}); !errors.As(err, &invalidErr) {
			t.Fatalf("Changes error = %v, want InvalidError", err)
		}
		if _, err := googleAt(t, &googleWorkspace{}).Start(ctx, bad, allFollowed); !errors.As(err, &invalidErr) {
			t.Fatalf("Start error = %v, want InvalidError", err)
		}
	})
	t.Run("keeps an event in the window as Calendar does", func(t *testing.T) {
		from, to := googleWindow(now, 90)
		for event, want := range map[string]bool{
			googleTestEvent("in", soon, ""):                                        true,
			googleTestEvent("ends at the start", from.Add(-time.Hour), ""):         false,
			googleTestEvent("ends after the start", from.Add(-30*time.Minute), ""): true,
			googleTestEvent("starts at the end", to, ""):                           false,
			`{"id":"day","start":{"date":"` + soon.Format(time.DateOnly) + `"},"end":{"date":"` + soon.AddDate(0, 0, 1).Format(time.DateOnly) + `"}}`: true,
			`{"id":"no times"}`: false,
			`{"id":"bad time","start":{"dateTime":"soon"},"end":{"dateTime":"later"}}`: false,
		} {
			var e map[string]any
			if err := json.Unmarshal([]byte(event), &e); err != nil {
				t.Fatal(err)
			}
			if got := googleInWindow(e, from, to); got != want {
				t.Errorf("googleInWindow(%s) = %v", event, got)
			}
		}
	})
}
