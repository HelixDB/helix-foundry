package syncer

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// googleTestWorkspace is a Workspace a test edits between syncs: one user,
// no shared drives, Drive's Docs by id, each with its modified time,
// Markdown and owner, whether it is in the trash, and whether its owner
// keeps the user from exporting it; and Ada's one calendar's events by id.
// Drive's change feed and the calendar's sync tokens follow every edit
// (edit, editEvent): a token is the count of edits it was taken after, and a
// feed shows each file or event edited after it as it is now. A page token
// below expired is refused, as is a sync token below gone. It counts the
// requests a test asserts on by what they are (took).
type googleTestWorkspace struct {
	mu            sync.Mutex
	docs          map[string]googleTestDoc
	events        map[string]googleTestEvent
	edits         int
	edited        map[string]int // by "file:<id>" or "event:<id>", the edit that last changed it
	expired, gone int
	asked         map[string]int
}

type googleTestDoc struct {
	modified, text, owner string
	trashed, withheld     bool
}

// googleTestEvent is an hour from start, inviting attendees, or cancelled.
type googleTestEvent struct {
	start     time.Time
	summary   string
	attendees []string
	cancelled bool
}

func newGoogleTestWorkspace(docs map[string]googleTestDoc, events map[string]googleTestEvent) *googleTestWorkspace {
	return &googleTestWorkspace{docs: docs, events: events, edited: map[string]int{}, asked: map[string]int{}}
}

// edit sets a Doc, as Drive's feed then shows it; a zero doc deletes it.
func (w *googleTestWorkspace) edit(id string, doc googleTestDoc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if doc == (googleTestDoc{}) {
		delete(w.docs, id)
	} else {
		w.docs[id] = doc
	}
	w.edits++
	w.edited["file:"+id] = w.edits
}

// editEvent sets an event, as the calendar's sync tokens then show it.
func (w *googleTestWorkspace) editEvent(id string, e googleTestEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.events[id] = e
	w.edits++
	w.edited["event:"+id] = w.edits
}

// took are the requests since the last call: "files" listings, Drive's
// "start" tokens and "changes" pages, the calendar's "window" lists and
// "sync" token lists, and each Doc's "export".
func (w *googleTestWorkspace) took() map[string]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	asked := w.asked
	w.asked = map[string]int{}
	return asked
}

func (w *googleTestWorkspace) file(id string, doc googleTestDoc) string {
	return fmt.Sprintf(`{"id":%q,"name":"Doc %s","mimeType":"application/vnd.google-apps.document","owners":[{"emailAddress":%q}],`+
		`"modifiedTime":%q,"webViewLink":"https://docs.google.com/document/d/%s/edit","version":"1","trashed":%t}`,
		id, id, doc.owner, doc.modified, id, doc.trashed)
}

func (w *googleTestWorkspace) event(id string, e googleTestEvent) string {
	if e.cancelled {
		return fmt.Sprintf(`{"id":%q,"status":"cancelled"}`, id)
	}
	var attendees []string
	for _, email := range e.attendees {
		attendees = append(attendees, fmt.Sprintf(`{"email":%q,"responseStatus":"accepted"}`, email))
	}
	return fmt.Sprintf(`{"id":%q,"status":"confirmed","summary":%q,"start":{"dateTime":%q},"end":{"dateTime":%q},"attendees":[%s]}`,
		id, e.summary, e.start.UTC().Format(time.RFC3339), e.start.Add(time.Hour).UTC().Format(time.RFC3339), strings.Join(attendees, ","))
}

// changed are the ids of kind ("file" or "event") edited after token, in order.
func (w *googleTestWorkspace) changed(kind string, token int) []string {
	var ids []string
	for key, at := range w.edited {
		if id, ok := strings.CutPrefix(key, kind+":"); ok && at > token {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

func (w *googleTestWorkspace) serve(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		q := r.URL.Query()
		write := func(format string, args ...any) { _, _ = fmt.Fprintf(rw, format, args...) }
		switch path := r.URL.Path; {
		case path == "/token":
			write(`{"access_token":"token","token_type":"Bearer","expires_in":3600}`)
		case path == "/admin/directory/v1/users":
			write(`{"users":[{"id":"1001","primaryEmail":"ada@acme.com","name":{"fullName":"Ada Lovelace"}}]}`)
		case path == "/calendar/v3/users/me/calendarList":
			write(`{"items":[{"id":"ada@acme.com","primary":true}]}`)
		case path == "/drive/v3/drives":
			write(`{"drives":[]}`)
		case path == "/drive/v3/changes/startPageToken":
			w.asked["start"]++
			write(`{"startPageToken":"%d"}`, w.edits)
		case path == "/drive/v3/changes":
			w.asked["changes"]++
			if token, err := strconv.Atoi(q.Get("pageToken")); err != nil || token < w.expired {
				rw.WriteHeader(http.StatusBadRequest)
				write(`{"error":{"code":400,"message":"Invalid Value","errors":[{"reason":"invalid","location":"pageToken"}]}}`)
				return
			}
			token, _ := strconv.Atoi(q.Get("pageToken"))
			var changes []string
			for _, id := range w.changed("file", token) {
				if doc, ok := w.docs[id]; ok {
					changes = append(changes, fmt.Sprintf(`{"changeType":"file","fileId":%q,"file":%s}`, id, w.file(id, doc)))
				} else {
					changes = append(changes, fmt.Sprintf(`{"changeType":"file","fileId":%q,"removed":true}`, id))
				}
			}
			write(`{"changes":[%s],"newStartPageToken":"%d"}`, strings.Join(changes, ","), w.edits)
		case path == "/drive/v3/files":
			// Discover's probe is no listing.
			if q.Has("corpora") {
				w.asked["files"]++
			}
			// A Sheet is a row and never a document.
			files := []string{`{"id":"sheet1","name":"Budget","mimeType":"application/vnd.google-apps.spreadsheet","modifiedTime":"2026-10-01T09:00:00Z"}`}
			for _, id := range slices.Sorted(maps.Keys(w.docs)) {
				if doc := w.docs[id]; !doc.trashed || q.Get("q") != "trashed=false" {
					files = append(files, w.file(id, doc))
				}
			}
			write(`{"files":[%s]}`, strings.Join(files, ","))
		case strings.HasPrefix(path, "/drive/v3/files/") && strings.HasSuffix(path, "/export"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/drive/v3/files/"), "/export")
			w.asked["export "+id]++
			doc, ok := w.docs[id]
			switch {
			case !ok:
				rw.WriteHeader(http.StatusNotFound)
				return
			case doc.withheld:
				rw.WriteHeader(http.StatusForbidden)
				write(`{"error":{"code":403,"message":"This file cannot be exported by the user.","errors":[{"reason":"cannotExportFile"}]}}`)
				return
			}
			write("%s", doc.text)
		case path == "/calendar/v3/calendars/ada@acme.com/events":
			var items []string
			if q.Has("syncToken") {
				w.asked["sync"]++
				token, err := strconv.Atoi(q.Get("syncToken"))
				if err != nil || token < w.gone || q.Has("timeMin") || q.Has("timeMax") || q.Get("singleEvents") != "true" {
					rw.WriteHeader(http.StatusGone)
					write(`{"error":{"code":410,"message":"Sync token is no longer valid, a full sync is required.","errors":[{"reason":"fullSyncRequired"}]}}`)
					return
				}
				for _, id := range w.changed("event", token) {
					items = append(items, w.event(id, w.events[id]))
				}
			} else {
				w.asked["window"]++
				from, _ := time.Parse(time.RFC3339, q.Get("timeMin"))
				to, _ := time.Parse(time.RFC3339, q.Get("timeMax"))
				for _, id := range slices.Sorted(maps.Keys(w.events)) {
					e := w.events[id]
					if e.start.Add(time.Hour).After(from) && e.start.Before(to) && (!e.cancelled || q.Get("showDeleted") == "true") {
						items = append(items, w.event(id, e))
					}
				}
			}
			write(`{"items":[%s],"nextSyncToken":"%d"}`, strings.Join(items, ","), w.edits)
		default:
			t.Errorf("unexpected request %s", r.URL)
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// googleFixture is a Google Workspace connector on plan code, reading w.
func googleFixture(t *testing.T, code string, w *googleTestWorkspace) fixture {
	t.Helper()
	f := newFixture(t, "http://unused", "google", `{"subject":"ada@acme.com","event_days":90}`, plan{code: code})
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "foundry@acme.iam.gserviceaccount.com", "private_key_id": "5c1e0d9f1a2b",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	})
	conn := f.store.connectors["conn-1"]
	if conn.SecretCiphertext, conn.SecretKeyID, err = f.syncer.Keys.Seal(key, connector.SecretContext("conn-1")); err != nil {
		t.Fatal(err)
	}
	endpoints := connector.DefaultEndpoints()
	endpoints.Google = w.serve(t)
	endpoints.GoogleToken = endpoints.Google + "/token"
	f.syncer.Catalog = connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{}, connector.EventsOptions{})
	return f
}

// TestGoogleSyncDocuments runs Google Workspace through the syncer on Pro:
// a sync reads every table in one pass and fills the Docs set; then Drive's
// change feed is followed: an unchanged Doc is not exported again, an
// edited one is, a Doc moved to the trash leaves the files table and its
// document goes with its row, and one its owner no longer lets the user
// export keeps its row and loses its document. An owner's email renamed
// changes no file, so only a full read exports its Doc again.
func TestGoogleSyncDocuments(t *testing.T) {
	drive := newGoogleTestWorkspace(map[string]googleTestDoc{
		"doc1": {modified: "2026-10-01T10:00:00Z", text: "## One\n\nFirst.", owner: "ada@acme.com"},
		"doc2": {modified: "2026-10-02T10:00:00.500Z", text: "## Two\n\nSecond.", owner: "ada@acme.com"},
	}, map[string]googleTestEvent{})
	f := googleFixture(t, "pro", drive)
	text := func(id, owner, modified, body string) string {
		return fmt.Sprintf("# Doc %s\n\nOwner: %s\nLast modified: %s\nFile id: %s\n\n%s\n", id, owner, modified, id, body)
	}
	exports := func() map[string]int {
		out := map[string]int{}
		for what, n := range drive.took() {
			if id, ok := strings.CutPrefix(what, "export "); ok {
				out[id] = n
			}
		}
		return out
	}

	res := f.run(t, "sync-1")
	if res.Mode != model.ConnectorSyncFull || len(f.store.tables) != 5 {
		t.Fatalf("mode = %s, tables = %v", res.Mode, slices.Sorted(maps.Keys(f.store.tables)))
	}
	if exports := exports(); fmt.Sprint(exports) != "map[doc1:1 doc2:1]" {
		t.Fatalf("exports = %v; want each Doc once", exports)
	}
	docs := f.held(t, "docs")
	if len(docs) != 2 || docs["doc1"] != text("doc1", "ada@acme.com", "2026-10-01 10:00 UTC", "## One\n\nFirst.") ||
		docs["doc2"] != text("doc2", "ada@acme.com", "2026-10-02 10:00 UTC", "## Two\n\nSecond.") {
		t.Fatalf("docs = %q", docs)
	}
	if set := f.store.setOf("docs"); set.Name != "Docs" || set.Owner.ConnectorID != "conn-1" || !strings.Contains(set.Description, "Google Doc") {
		t.Fatalf("docs set = %+v", set)
	}

	// Nothing changed: Drive's feed shows nothing, so no Doc is offered or exported.
	doc1, publishes := f.liveID("docs", "doc1"), len(f.store.publishes)
	if res := f.run(t, "sync-2"); res.Mode != model.ConnectorSyncChanges || len(res.Changed) != 0 {
		t.Fatalf("mode = %s, changed = %v", res.Mode, res.Changed)
	}
	if exports := exports(); len(exports) != 0 || f.liveID("docs", "doc1") != doc1 || len(f.store.publishes) != publishes {
		t.Fatalf("exports = %v, doc1 = %s, was %s, publishes = %d", exports, f.liveID("docs", "doc1"), doc1, len(f.store.publishes)-publishes)
	}

	// An edited Doc is exported again and replaces its document; one moved
	// to the trash loses its row, and its document goes with it.
	drive.edit("doc1", googleTestDoc{modified: "2026-10-01T10:00:00Z", text: "## One\n\nFirst.", owner: "ada@acme.com", trashed: true})
	drive.edit("doc2", googleTestDoc{modified: "2026-10-28T08:00:00Z", text: "## Two\n\nSecond, edited.", owner: "ada@acme.com"})
	if res := f.run(t, "sync-3"); res.Mode != model.ConnectorSyncChanges || !slices.Equal(res.Changed, []string{"files"}) {
		t.Fatalf("mode = %s, changed = %v", res.Mode, res.Changed)
	}
	if exports := exports(); fmt.Sprint(exports) != "map[doc2:1]" {
		t.Fatalf("exports = %v", exports)
	}
	if docs := f.held(t, "docs"); len(docs) != 1 || docs["doc2"] != text("doc2", "ada@acme.com", "2026-10-28 08:00 UTC", "## Two\n\nSecond, edited.") {
		t.Fatalf("docs = %q", docs)
	}
	if f.store.docs[f.liveID("docs", "doc2")].Source.Version != "1|2026-10-28T08:00:00Z|1|e8d1b1d0bebed26b" {
		t.Fatalf("doc2 = %+v", f.store.docs[f.liveID("docs", "doc2")].Source)
	}
	if rows := f.snapshotRows(t, "files"); len(rows) != 2 || !strings.Contains(rows[0], `"file_id":"doc2"`) || !strings.Contains(rows[1], `"file_id":"sheet1"`) {
		t.Fatalf("files = %v", rows)
	}

	// The owner's email renamed changes neither the Doc nor Drive's feed:
	// a change read can't see it, and the next full read exports it again.
	drive.mu.Lock()
	drive.docs["doc2"] = googleTestDoc{modified: "2026-10-28T08:00:00Z", text: "## Two\n\nSecond, edited.", owner: "ada.lovelace@acme.com"}
	drive.mu.Unlock()
	f.run(t, "sync-4")
	if exports := exports(); len(exports) != 0 {
		t.Fatalf("exports = %v", exports)
	}
	full := newSync("sync-5")
	full.Mode = model.ConnectorSyncFull
	if res, err := f.syncer.Run(context.Background(), full); err != nil || res.Mode != model.ConnectorSyncFull {
		t.Fatalf("Run = (%+v, %v)", res, err)
	}
	if exports := exports(); fmt.Sprint(exports) != "map[doc2:1]" {
		t.Fatalf("exports = %v", exports)
	}
	if docs := f.held(t, "docs"); len(docs) != 1 || docs["doc2"] != text("doc2", "ada.lovelace@acme.com", "2026-10-28 08:00 UTC", "## Two\n\nSecond, edited.") {
		t.Fatalf("docs = %q", docs)
	}

	// Its owner keeps the user from exporting it, edited since: its row stays
	// and its document goes.
	drive.edit("doc2", googleTestDoc{modified: "2026-10-29T08:00:00Z", owner: "ada.lovelace@acme.com", withheld: true})
	if res := f.run(t, "sync-6"); res.Mode != model.ConnectorSyncChanges {
		t.Fatalf("mode = %s", res.Mode)
	}
	if exports := exports(); fmt.Sprint(exports) != "map[doc2:1]" {
		t.Fatalf("exports = %v", exports)
	}
	if docs := f.held(t, "docs"); len(docs) != 0 {
		t.Fatalf("docs = %q", docs)
	}
	if rows := f.snapshotRows(t, "files"); len(rows) != 2 || !strings.Contains(rows[0], `"file_id":"doc2"`) {
		t.Fatalf("files = %v", rows)
	}
}

// TestGoogleSyncChanges follows Google Workspace's files and events on
// Pro: a new file appears, a trashed one loses its row; a changed event
// replaces its row and attendees, a cancelled one loses them, and one moved
// out of the window keeps its row; users and calendars are read in full in
// the same sync. A page token or sync token Google no longer takes reads
// that API's tables in full in the same sync, and the daily reconcile reads
// every table in full. Free reads in full every time.
func TestGoogleSyncChanges(t *testing.T) {
	soon := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	workspace := func() *googleTestWorkspace {
		return newGoogleTestWorkspace(map[string]googleTestDoc{
			"doc1": {modified: "2026-10-01T10:00:00Z", text: "One.", owner: "ada@acme.com"},
		}, map[string]googleTestEvent{
			"ev1": {start: soon, summary: "Roadmap", attendees: []string{"ada@acme.com", "grace@acme.com"}},
			"ev2": {start: soon.Add(time.Hour), summary: "Standup"},
			"ev3": {start: soon.Add(2 * time.Hour), summary: "Review", attendees: []string{"ada@acme.com"}},
		})
	}
	// ids are the snapshot's values of column, in order.
	ids := func(t *testing.T, f fixture, table, column string) []string {
		t.Helper()
		var out []string
		for _, line := range f.snapshotRows(t, table) {
			var row map[string]any
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprint(row[column]))
		}
		slices.Sort(out)
		return out
	}
	batch := func(f fixture, table string) *model.ConnectorChangeBatch {
		batches := f.store.batches[table]
		return batches[len(batches)-1]
	}

	google := workspace()
	f := googleFixture(t, "pro", google)
	// Start takes Drive's token and lists the window once for the calendar's
	// sync token, before the full read lists both.
	if res := f.run(t, "sync-1"); res.Mode != model.ConnectorSyncFull {
		t.Fatalf("mode = %s", res.Mode)
	}
	if asked := google.took(); fmt.Sprint(asked) != "map[export doc1:1 files:1 start:1 window:2]" {
		t.Fatalf("asked %v", asked)
	}
	if got := ids(t, f, "calendar_events", "event_id"); fmt.Sprint(got) != "[ada@acme.com:ev1 ada@acme.com:ev2 ada@acme.com:ev3]" {
		t.Fatalf("events = %v", got)
	}

	// Nothing changed: one page of Drive's feed and one of the calendar's.
	tables := maps.Clone(f.store.tables)
	if res := f.run(t, "sync-2"); res.Mode != model.ConnectorSyncChanges || len(res.Changed) != 0 {
		t.Fatalf("mode = %s, changed = %v", res.Mode, res.Changed)
	}
	if asked := google.took(); fmt.Sprint(asked) != "map[changes:1 sync:1]" {
		t.Fatalf("asked %v", asked)
	}
	for name, table := range f.store.tables {
		if table.S3Key != tables[name].S3Key {
			t.Fatalf("%s was published again", name)
		}
	}

	// A new Doc appears and a trashed one goes; ev1 changes, ev2 is
	// cancelled and ev3 moves past the window, so its row stays as it was.
	google.edit("doc4", googleTestDoc{modified: "2026-10-29T09:00:00Z", text: "Four.", owner: "ada@acme.com"})
	google.edit("doc1", googleTestDoc{modified: "2026-10-01T10:00:00Z", text: "One.", owner: "ada@acme.com", trashed: true})
	google.editEvent("ev1", googleTestEvent{start: soon, summary: "Roadmap, moved", attendees: []string{"ada@acme.com", "alan@acme.com"}})
	google.editEvent("ev2", googleTestEvent{cancelled: true})
	google.editEvent("ev3", googleTestEvent{start: soon.AddDate(0, 0, 60), summary: "Review, later", attendees: []string{"ada@acme.com"}})
	res := f.run(t, "sync-3")
	if res.Mode != model.ConnectorSyncChanges || fmt.Sprint(slices.Sorted(slices.Values(res.Changed))) != "[calendar_events event_attendees files]" {
		t.Fatalf("mode = %s, changed = %v", res.Mode, res.Changed)
	}
	if asked := google.took(); fmt.Sprint(asked) != "map[changes:1 export doc4:1 sync:1]" {
		t.Fatalf("asked %v", asked)
	}
	if got := ids(t, f, "files", "file_id"); fmt.Sprint(got) != "[doc4 sheet1]" {
		t.Fatalf("files = %v", got)
	}
	if docs := f.held(t, "docs"); len(docs) != 1 || !strings.HasSuffix(docs["doc4"], "\n\nFour.\n") {
		t.Fatalf("docs = %q", docs)
	}
	if b := batch(f, "files"); b.Kind != model.ConnectorChangesBatch || b.Inserts != 1 || b.Deletes != 1 || b.Updates != 0 {
		t.Fatalf("files batch = %+v", b)
	}
	if got := ids(t, f, "calendar_events", "summary"); fmt.Sprint(got) != "[Review Roadmap, moved]" {
		t.Fatalf("events = %v", got)
	}
	if b := batch(f, "calendar_events"); b.Updates != 1 || b.Deletes != 1 || b.Inserts != 0 {
		t.Fatalf("calendar_events batch = %+v", b)
	}
	if got := ids(t, f, "event_attendees", "email"); fmt.Sprint(got) != "[ada@acme.com ada@acme.com alan@acme.com]" {
		t.Fatalf("attendees = %v", got)
	}
	if b := batch(f, "event_attendees"); b.Inserts != 1 || b.Deletes != 1 {
		t.Fatalf("event_attendees batch = %+v", b)
	}

	// Drive no longer takes the page token: files is read in full, from a
	// new token, in the same sync; the calendar's changes are still followed.
	google.mu.Lock()
	google.expired = google.edits + 1
	google.mu.Unlock()
	if res := f.run(t, "sync-4"); res.Mode != model.ConnectorSyncChanges || len(res.Changed) != 0 {
		t.Fatalf("mode = %s, changed = %v", res.Mode, res.Changed)
	}
	if asked := google.took(); fmt.Sprint(asked) != "map[changes:1 files:1 start:1 sync:1]" {
		t.Fatalf("asked %v", asked)
	}
	google.mu.Lock()
	google.expired = 0
	google.mu.Unlock()
	f.run(t, "sync-5")
	if asked := google.took(); fmt.Sprint(asked) != "map[changes:1 sync:1]" {
		t.Fatalf("asked %v after the new token", asked)
	}

	// Calendar no longer takes the sync token (410 Gone): the event tables
	// are read in full, from a new token, in the same sync, which drops ev3,
	// out of the window, from the snapshot without deleting it.
	google.mu.Lock()
	google.gone = google.edits + 1
	google.mu.Unlock()
	deletes := len(f.store.batches["calendar_events"])
	if res := f.run(t, "sync-6"); res.Mode != model.ConnectorSyncChanges {
		t.Fatalf("mode = %s", res.Mode)
	}
	if asked := google.took(); fmt.Sprint(asked) != "map[changes:1 sync:1 window:2]" {
		t.Fatalf("asked %v", asked)
	}
	if got := ids(t, f, "calendar_events", "event_id"); fmt.Sprint(got) != "[ada@acme.com:ev1]" ||
		len(f.store.batches["calendar_events"]) != deletes {
		t.Fatalf("events = %v, batches = %v", got, f.store.batches["calendar_events"][deletes:])
	}
	google.mu.Lock()
	google.gone = 0
	google.mu.Unlock()
	f.run(t, "sync-7")
	if asked := google.took(); fmt.Sprint(asked) != "map[changes:1 sync:1]" {
		t.Fatalf("asked %v after the new token", asked)
	}

	// The daily reconcile reads every table in full, from new tokens.
	old := time.Now().Add(-25 * time.Hour)
	f.store.tables["files"].ReconciledAt = &old
	if res := f.run(t, "sync-8"); res.Mode != model.ConnectorSyncFull {
		t.Fatalf("mode = %s", res.Mode)
	}
	if asked := google.took(); fmt.Sprint(asked) != "map[files:1 start:1 window:2]" {
		t.Fatalf("asked %v", asked)
	}

	t.Run("reads in full on Free", func(t *testing.T) {
		google := workspace()
		f := googleFixture(t, "free", google)
		for _, id := range []string{"sync-1", "sync-2"} {
			if res := f.run(t, id); res.Mode != model.ConnectorSyncFull || f.store.connectors["conn-1"].ChangeCursor != nil {
				t.Fatalf("%s: mode = %s, cursor = %s", id, res.Mode, f.store.connectors["conn-1"].ChangeCursor)
			}
			google.took()
		}
		google.editEvent("ev2", googleTestEvent{cancelled: true})
		f.run(t, "sync-3")
		if asked := google.took(); fmt.Sprint(asked) != "map[files:1 window:1]" {
			t.Fatalf("asked %v", asked)
		}
		if got := ids(t, f, "calendar_events", "event_id"); fmt.Sprint(got) != "[ada@acme.com:ev1 ada@acme.com:ev3]" {
			t.Fatalf("events = %v", got)
		}
	})
}
