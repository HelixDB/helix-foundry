package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"
)

var _ ChangeReader = (*googleKind)(nil)

// googleChangeFields are what a page of a Drive change feed asks for: each
// change's file with the fields a listing asks for, and whether it is in the
// trash, which a listing leaves out by its query.
const googleChangeFields = "nextPageToken,newStartPageToken,changes(changeType,removed,fileId,file(" + googleFileFieldList + ",trashed))"

// googleCursor is where each followed API's next change read begins. Files
// is null, and Events null, when the cursor follows none of their tables.
type googleCursor struct {
	Files *googleFilesCursor `json:"files"`
	// Events is each calendar's sync token, by calendar id: Calendar's
	// nextSyncToken of its events, listed as a full read lists them. A
	// calendar the user can't read the events of has none.
	Events map[string]string `json:"events"`
}

// googleFilesCursor is the page token of each Drive change feed a full
// read's listings match: the user's (My Drive and what is shared with them,
// corpora=user) and each shared drive's (corpora=drive), by drive id.
type googleFilesCursor struct {
	User   string            `json:"user"`
	Drives map[string]string `json:"drives"`
}

// Capture follows files through Drive's change feeds and the event tables
// through each calendar's sync token. Users and calendars have no change
// feed but push notifications, so every sync reads them in full.
func (k *googleKind) Capture(_ Source, table string) Capture {
	switch table {
	case "files", "calendar_events", "event_attendees":
		return CaptureCursor
	}
	return CaptureFull
}

// Start takes the tokens of each followed API's feeds before the full read
// lists anything: Drive's start page token of the user's changes and of
// each shared drive's, and each calendar's sync token. Calendar gives a sync
// token only on the last page of a list, so each calendar's window is listed
// once more, as Google's own full sync takes it, and its events discarded.
func (k *googleKind) Start(ctx context.Context, src Source, tables []Table) (Start, error) {
	c, err := k.client(src)
	if err != nil {
		return Start{}, err
	}
	follow := map[string]bool{}
	for _, t := range tables {
		follow[t.Name] = true
	}
	var cursor googleCursor
	if follow["files"] {
		drives, err := c.drives(ctx)
		if err != nil {
			return Start{}, err
		}
		if cursor.Files, err = c.filesStart(ctx, drives); err != nil {
			return Start{}, err
		}
	}
	if follow["calendar_events"] || follow["event_attendees"] {
		calendars, err := c.calendarList(ctx, func(string, map[string]any) error { return nil })
		if err != nil {
			return Start{}, err
		}
		from, to := googleWindow(time.Now(), c.settings.EventDays)
		cursor.Events = map[string]string{}
		for _, calendar := range calendars {
			token, err := c.eventToken(ctx, calendar, from, to, func(string, map[string]any) error { return nil })
			switch {
			case errors.Is(err, errNoAccess):
				continue
			case err != nil:
				return Start{}, err
			}
			cursor.Events[calendar] = token
		}
	}
	b, err := json.Marshal(cursor)
	return Start{Cursor: b}, err
}

// Changes reads what each followed API's feeds hold after the cursor; a
// table whose feeds can't be followed this time is reset, and the syncer
// reads it in full in the same sync, from the tokens Changes returns, which
// it takes before that read.
func (k *googleKind) Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	var prev googleCursor
	if err := json.Unmarshal(cursor, &prev); err != nil {
		return nil, fmt.Errorf("decode google change cursor: %w", ErrCursorExpired)
	}
	c, err := k.client(src)
	if err != nil {
		return nil, err
	}
	follow := map[string]bool{}
	for _, t := range tables {
		if k.Capture(src, t.Name) != CaptureCursor {
			return nil, invalid("Google Workspace can't follow the changes of %q", t.Name)
		}
		follow[t.Name] = true
	}
	var next googleCursor
	if follow["files"] {
		if next.Files, err = c.fileChanges(ctx, prev.Files, w); err != nil {
			return nil, err
		}
	}
	if follow["calendar_events"] || follow["event_attendees"] {
		if next.Events, err = c.eventChanges(ctx, prev.Events, follow, w); err != nil {
			return nil, err
		}
	}
	return json.Marshal(next)
}

// filesStart is the start page token of the user's feed and of each of
// drives' feeds. A drive whose token is refused, one the user left since the
// drives were listed, has none; the next change read finds it gone.
func (c *googleClient) filesStart(ctx context.Context, drives []string) (*googleFilesCursor, error) {
	user, err := c.startPageToken(ctx, "")
	if err != nil {
		return nil, err
	}
	cursor := &googleFilesCursor{User: user, Drives: map[string]string{}}
	for _, drive := range drives {
		token, err := c.startPageToken(ctx, drive)
		switch {
		case errors.Is(err, errNoAccess):
			continue
		case err != nil:
			return nil, err
		}
		cursor.Drives[drive] = token
	}
	return cursor, nil
}

// fileChanges reads the user's feed and each shared drive's from the
// cursor, one page each when nothing changed. A file a feed shows as it is
// now gets its row, as a full read writes it, and a Doc is offered again
// (googleExporter): a changed one is exported again. A file every feed that
// shows it shows gone (removed: deleted, or no longer the user's to read; or
// in the trash) loses its row, and the syncer its document; a file one feed
// shows gone and another as it is, moved between drives, keeps it. A Doc its
// owner no longer lets the user download loses the document the stream
// holds, if Drive's version of it changed; a full read leaves it out.
//
// The files table is reset, and read in full, when the shared drives the
// user is in changed (a drive joined has no token; one left takes its files
// out of the full read's listings), when Drive no longer takes a feed's page
// token, and when there is no cursor. Each feed then keeps the token it had,
// or takes a new one, before that full read.
func (c *googleClient) fileChanges(ctx context.Context, prev *googleFilesCursor, w ChangeWriter) (*googleFilesCursor, error) {
	drives, err := c.drives(ctx)
	if err != nil {
		return nil, err
	}
	if prev == nil {
		w.Reset("files", "the table has no change cursor yet")
		return c.filesStart(ctx, drives)
	}
	reset := ""
	for _, drive := range drives {
		if _, ok := prev.Drives[drive]; !ok {
			reset = "the user joined shared drive " + drive
		}
	}
	for drive := range prev.Drives {
		if !slices.Contains(drives, drive) {
			reset = "the user left shared drive " + drive
		}
	}
	present, gone := map[string]bool{}, map[string]bool{}
	exporter := googleExporter{c: c, docs: w}
	each := func(id string, f map[string]any) error {
		if f == nil {
			gone[id] = true
			return nil
		}
		present[id] = true
		row, doc, err := googleFile(id, f)
		if err != nil {
			return err
		}
		if err := w.Upsert("files", row); err != nil || doc == nil {
			return err
		}
		if !doc.withheld {
			return exporter.offer(ctx, *doc)
		}
		if _, held := w.Held(googleDocsStream.Name, id); held && w.Want(googleDocsStream.Name, id, doc.version) {
			return w.Drop(googleDocsStream.Name, id)
		}
		return nil
	}
	next := &googleFilesCursor{Drives: map[string]string{}}
	for _, drive := range slices.Concat([]string{""}, drives) {
		token := prev.User
		if drive != "" {
			token = prev.Drives[drive]
		}
		if token != "" && reset == "" {
			token, err = c.driveFeed(ctx, drive, token, each)
			switch {
			case drive != "" && errors.Is(err, errNoAccess):
				reset = "the user left shared drive " + drive
				continue
			case errors.Is(err, errTokenGone):
				reset = "Drive no longer holds the changes after the cursor"
				token = ""
			case err != nil:
				return nil, err
			}
		}
		if token == "" {
			token, err = c.startPageToken(ctx, drive)
			switch {
			case drive != "" && errors.Is(err, errNoAccess):
				reset = "the user left shared drive " + drive
				continue
			case err != nil:
				return nil, err
			}
		}
		if drive == "" {
			next.User = token
		} else {
			next.Drives[drive] = token
		}
	}
	if reset != "" {
		w.Reset("files", reset)
		return next, nil
	}
	for _, id := range slices.DeleteFunc(slices.Sorted(maps.Keys(gone)), func(id string) bool { return present[id] }) {
		if err := w.Delete("files", map[string]any{"file_id": id}); err != nil {
			return nil, err
		}
	}
	return next, nil
}

// startPageToken is the token Drive's feed of drive's changes starts from
// now: the user's when drive is "". A shared drive the user is not in is
// errNoAccess.
func (c *googleClient) startPageToken(ctx context.Context, drive string) (string, error) {
	query := url.Values{"supportsAllDrives": {"true"}}
	if drive != "" {
		query.Set("driveId", drive)
	}
	target := "/drive/v3/changes/startPageToken?" + query.Encode()
	status, body, err := c.get(ctx, c.k.http, googleDrive, target)
	if err == nil {
		err = c.status(status, body, googleDrive, target)
	}
	if err != nil {
		return "", err
	}
	var answer struct{ StartPageToken string }
	if json.Unmarshal(body, &answer) != nil || answer.StartPageToken == "" {
		return "", errors.New("google /drive/v3/changes/startPageToken: the answer has no startPageToken")
	}
	return answer.StartPageToken, nil
}

// driveFeed reads one of Drive's change feeds from token to its end: the
// user's, or shared drive's when drive is set. It passes each file a change
// names to each, as the file is now; nil when it is gone from the feed or in
// the trash. A shared drive's own changes are not files'. It returns the
// feed's next page token. A token Drive no longer takes is errTokenGone; a
// shared drive the user is no longer in is errNoAccess. As pages does, a
// later page refused or a page that is not a complete one fails the read.
func (c *googleClient) driveFeed(ctx context.Context, drive, token string, each func(id string, file map[string]any) error) (string, error) {
	query := url.Values{"pageToken": {token}, "pageSize": {"1000"}, "includeItemsFromAllDrives": {"true"}, "supportsAllDrives": {"true"},
		"includeRemoved": {"true"}, "fields": {googleChangeFields}}
	if drive != "" {
		query.Set("driveId", drive)
	}
	const path = "/drive/v3/changes"
	for page := 0; ; page++ {
		if page == maxGooglePages {
			return "", fmt.Errorf("google %s: more than %d pages", path, maxGooglePages)
		}
		target := path + "?" + query.Encode()
		status, body, err := c.get(ctx, c.k.http, googleDrive, target)
		if err != nil {
			return "", err
		}
		if googlePageTokenRefused(status, body) {
			return "", refused(errTokenGone, "Drive no longer holds the changes of %s; try again", path)
		}
		if err := c.status(status, body, googleDrive, target); err != nil {
			if page > 0 && errors.Is(err, errNoAccess) {
				return "", refused(nil, "Google stopped listing %s as %s part way through; try again", path, c.settings.Subject)
			}
			return "", err
		}
		var p struct {
			Changes []*struct {
				ChangeType string
				Removed    bool
				FileID     string `json:"fileId"`
				File       map[string]any
			}
			NextPageToken, NewStartPageToken string
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return "", fmt.Errorf("google %s: the answer is not a complete page of changes", path)
		}
		for _, change := range p.Changes {
			switch {
			case change != nil && change.ChangeType == "drive":
				continue
			case change == nil || change.FileID == "" || !change.Removed && change.File == nil:
				return "", fmt.Errorf("google %s: a change names no file", path)
			}
			file := change.File
			if change.Removed || file["trashed"] == true {
				file = nil
			}
			if err := each(change.FileID, file); err != nil {
				return "", err
			}
		}
		switch {
		case p.NextPageToken == "" && p.NewStartPageToken == "":
			return "", fmt.Errorf("google %s: the last page has no newStartPageToken", path)
		case p.NextPageToken == "":
			return p.NewStartPageToken, nil
		case p.NextPageToken == query.Get("pageToken"):
			return "", fmt.Errorf("google %s: a page gives back the token it was asked with", path)
		}
		query.Set("pageToken", p.NextPageToken)
	}
}

// googlePageTokenRefused is Drive refusing a change feed's page token, which
// it answers with a 400 naming the pageToken parameter.
func googlePageTokenRefused(status int, body []byte) bool {
	var answer struct {
		Error struct{ Errors []struct{ Location string } }
	}
	_ = json.Unmarshal(body, &answer) // an answer without errors names no parameter
	return status == http.StatusBadRequest &&
		slices.ContainsFunc(answer.Error.Errors, func(e struct{ Location string }) bool { return e.Location == "pageToken" })
}

// googleEventQuery is what every list of a calendar's events that takes or
// follows a sync token asks, the same on each, as Calendar requires:
// instances of recurring events, and cancelled ones, which a sync token's
// changes always include; with both, a deleted series is its instances.
func googleEventQuery() url.Values {
	return url.Values{"singleEvents": {"true"}, "showDeleted": {"true"}, "maxResults": {"2500"}}
}

// eventToken lists calendar's events in the window from to to, as a full
// read lists them, passing each to each, and returns the list's sync token.
func (c *googleClient) eventToken(ctx context.Context, calendar string, from, to time.Time,
	each func(id string, e map[string]any) error) (string, error) {
	query := googleEventQuery()
	query.Set("timeMin", from.Format(time.RFC3339))
	query.Set("timeMax", to.Format(time.RFC3339))
	return c.eventPages(ctx, calendar, query, each)
}

// eventPages lists calendar's events under query, passing each to each, and
// returns the list's sync token, which Calendar gives every list of events
// without a filter that refuses one.
func (c *googleClient) eventPages(ctx context.Context, calendar string, query url.Values,
	each func(id string, e map[string]any) error) (string, error) {
	token, err := c.pages(ctx, googleCalendar, googleEventsPath(calendar), query, "items", each)
	if err == nil && token == "" {
		return "", fmt.Errorf("google %s: the last page has no nextSyncToken", googleEventsPath(calendar))
	}
	return token, err
}

// eventChanges reads each listed calendar's events changed after its sync
// token, one page each when nothing changed. A cancelled event loses its
// rows; a changed one in the window gets its row and its attendees' rows in
// place of the ones it had, as a full read writes them. A changed event
// outside the window is left as it is, as a full read leaves it: the tables
// are windowed, so its row, if any, leaves when the next full read drops it,
// and is no delete. A calendar without a token, newly listed or refused
// before, is listed in the window, as a full read lists it; one whose events
// are refused is skipped.
//
// The event tables are reset, and read in full, when there is no cursor,
// when a calendar with a token left the list or its events are refused (a
// full read leaves out its events, and their attendees have no calendar
// column to remove them by), and when Calendar no longer takes a token
// (410 Gone), whose calendar then takes a new one from its window.
func (c *googleClient) eventChanges(ctx context.Context, prev map[string]string, follow map[string]bool, w ChangeWriter) (map[string]string, error) {
	calendars, err := c.calendarList(ctx, func(string, map[string]any) error { return nil })
	if err != nil {
		return nil, err
	}
	reset := ""
	if prev == nil {
		reset = "the table has no change cursor yet"
	}
	for calendar := range prev {
		if !slices.Contains(calendars, calendar) {
			reset = "calendar " + calendar + " left the user's calendar list"
		}
	}
	from, to := googleWindow(time.Now(), c.settings.EventDays)
	next := map[string]string{}
	for _, calendar := range calendars {
		each := func(id string, e map[string]any) error {
			if e["status"] == "cancelled" {
				return googleEventGone(w, follow, calendar+":"+id)
			}
			if !googleInWindow(e, from, to) {
				return nil
			}
			row, people := googleEvent(calendar, id, e)
			return googleEventChanged(w, follow, row, people)
		}
		if token, ok := prev[calendar]; ok {
			query := googleEventQuery()
			query.Set("syncToken", token)
			token, err := c.eventPages(ctx, calendar, query, each)
			switch {
			case err == nil:
				next[calendar] = token
				continue
			case errors.Is(err, errNoAccess):
				reset = "the user can no longer read the events of calendar " + calendar
				continue
			case !errors.Is(err, errTokenGone):
				return nil, err
			}
			reset = "Google no longer holds the changes of calendar " + calendar
		}
		token, err := c.eventToken(ctx, calendar, from, to, each)
		switch {
		case errors.Is(err, errNoAccess):
			continue
		case err != nil:
			return nil, err
		}
		next[calendar] = token
	}
	if reset != "" {
		for _, table := range []string{"calendar_events", "event_attendees"} {
			if follow[table] {
				w.Reset(table, reset)
			}
		}
	}
	return next, nil
}

// googleEventGone removes a cancelled event's rows from the followed tables.
func googleEventGone(w ChangeWriter, follow map[string]bool, eventID string) error {
	if follow["calendar_events"] {
		if err := w.Delete("calendar_events", map[string]any{"event_id": eventID}); err != nil {
			return err
		}
	}
	if follow["event_attendees"] {
		return w.DeleteWhere("event_attendees", "event_id", eventID)
	}
	return nil
}

// googleEventChanged writes a changed event's rows to the followed tables,
// its attendees' in place of the ones it had.
func googleEventChanged(w ChangeWriter, follow map[string]bool, row map[string]any, people []map[string]any) error {
	if follow["calendar_events"] {
		if err := w.Upsert("calendar_events", row); err != nil {
			return err
		}
	}
	if !follow["event_attendees"] {
		return nil
	}
	if err := w.DeleteWhere("event_attendees", "event_id", row["event_id"]); err != nil {
		return err
	}
	for _, person := range people {
		if err := w.Upsert("event_attendees", person); err != nil {
			return err
		}
	}
	return nil
}

// googleInWindow is whether a full read's list of the window from to to
// holds event e, as Calendar filters it: it ends after from and starts
// before to. An all-day event's dates are taken as UTC days, where Calendar
// takes them in the calendar's time zone, so an event at an edge of the
// window may be in it here and not there for a day; the next full read
// keeps Calendar's. An event without its times is in no window.
func googleInWindow(e map[string]any, from, to time.Time) bool {
	at := func(field string) (time.Time, bool) {
		when, _ := e[field].(map[string]any)
		if date, ok := when["date"].(string); ok {
			t, err := time.Parse(time.DateOnly, date)
			return t, err == nil
		}
		s, _ := when["dateTime"].(string)
		t, err := time.Parse(time.RFC3339, s)
		return t, err == nil
	}
	start, okStart := at("start")
	end, okEnd := at("end")
	return okStart && okEnd && end.After(from) && start.Before(to)
}
