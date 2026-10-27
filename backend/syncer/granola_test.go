package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// granolaAccount is a Granola account a test edits between syncs: each
// note's update time and summary, by id. It counts every Get Note by its
// path and query.
type granolaAccount struct {
	mu    sync.Mutex
	notes map[string]granolaTestNote
	gets  map[string]int
}

type granolaTestNote struct{ created, updated, summary string }

func (a *granolaAccount) serve(t *testing.T) string {
	t.Helper()
	page := func(field string, items []string) string {
		return fmt.Sprintf(`{%q:[%s],"hasMore":false,"cursor":null}`, field, strings.Join(items, ","))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch path := r.URL.Path; {
		case path == "/v1/folders" || path == "/v1/spaces":
			_, _ = w.Write([]byte(page(strings.TrimPrefix(path, "/v1/"), nil)))
		case path == "/v1/notes":
			var listed []string
			after := r.URL.Query().Get("updated_after")
			for _, id := range slices.Sorted(maps.Keys(a.notes)) {
				if n := a.notes[id]; after == "" || n.updated > after {
					listed = append(listed, fmt.Sprintf(`{"id":%q,"created_at":%q,"updated_at":%q,"deleted_at":null}`, id, n.created, n.updated))
				}
			}
			_, _ = w.Write([]byte(page("notes", listed)))
		case strings.HasPrefix(path, "/v1/notes/"):
			id := strings.TrimPrefix(path, "/v1/notes/")
			a.gets[id+"?"+r.URL.RawQuery]++
			n, ok := a.notes[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			transcript := "null"
			if r.URL.Query().Get("include") == "transcript" {
				transcript = `[{"speaker":{"source":"microphone","attribution":"me"},"text":"Hello."},{"speaker":{"source":"speaker"},"text":"Hi."}]`
			}
			_, _ = fmt.Fprintf(w, `{"id":%q,"title":"Meeting %s","created_at":%q,"updated_at":%q,"web_url":"https://notes.granola.ai/d/%s",
				"attendees":[{"name":"Ada","email":"ada@example.com"}],"calendar_event":null,"folder_membership":[],
				"summary_markdown":%q,"transcript":%s}`, id, id, n.created, n.updated, id, n.summary, transcript)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// took are the Get Notes since the last call, by path and query.
func (a *granolaAccount) took() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	gets := a.gets
	a.gets = map[string]int{}
	return gets
}

// TestGranolaSyncDocuments runs Granola through the syncer: one Get Note
// per note covers every note table and both documents, an unchanged sync
// fetches and writes nothing, an edit replaces a note's summary in a change
// read, and a full read removes a deleted note's documents.
func TestGranolaSyncDocuments(t *testing.T) {
	account := &granolaAccount{gets: map[string]int{}, notes: map[string]granolaTestNote{
		"not_1": {"2026-10-01T09:00:00Z", "2026-10-01T10:00:00Z", "## One\n\nFirst."},
		"not_2": {"2026-10-02T09:00:00Z", "2026-10-02T10:00:00Z", "## Two\n\nSecond."},
	}}
	f := newFixture(t, "http://unused", "granola", `{"transcripts":true}`, plan{code: "pro"})
	endpoints := connector.DefaultEndpoints()
	endpoints.Granola = account.serve(t)
	f.syncer.Catalog = connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{}, connector.EventsOptions{})
	f.syncer.Now = func() time.Time { return time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC) }
	header := func(id, created string) string {
		return fmt.Sprintf("# Meeting %s\n\nDate: %s\nAttendees: Ada <ada@example.com>\nNote id: %s\n", id, created, id)
	}

	res := f.run(t, "sync-1")
	if res.Mode != model.ConnectorSyncFull || len(f.store.tables) != 5 {
		t.Fatalf("mode = %s, tables = %v", res.Mode, slices.Sorted(maps.Keys(f.store.tables)))
	}
	if gets := account.took(); fmt.Sprint(gets) != "map[not_1?include=transcript:1 not_2?include=transcript:1]" {
		t.Fatalf("Get Notes = %v; want each note once, with its transcript", gets)
	}
	notes, transcripts := f.held(t, "notes"), f.held(t, "transcripts")
	if notes["not_1"] != header("not_1", "2026-10-01 09:00 UTC")+"\n## One\n\nFirst.\n" || len(notes) != 2 {
		t.Fatalf("notes = %q", notes)
	}
	if transcripts["not_2"] != header("not_2", "2026-10-02 09:00 UTC")+"\nMe: Hello.\nThem: Hi.\n" || len(transcripts) != 2 {
		t.Fatalf("transcripts = %q", transcripts)
	}
	if set := f.store.setOf("transcripts"); set.Name != "Transcripts" || set.Owner.ConnectorID != "conn-1" {
		t.Fatalf("transcripts set = %+v", set)
	}

	// Nothing changed: the change read lists nothing and writes nothing.
	publishes := len(f.store.publishes)
	if res := f.run(t, "sync-2"); res.Mode != model.ConnectorSyncChanges || len(res.Changed) != 0 {
		t.Fatalf("mode = %s, changed = %v", res.Mode, res.Changed)
	}
	if gets := account.took(); len(gets) != 0 || len(f.store.publishes) != publishes {
		t.Fatalf("Get Notes = %v, publishes = %d", gets, len(f.store.publishes)-publishes)
	}

	// An edited summary replaces the notes document; the transcript's bytes
	// are the same, so its document is updated in place. Granola's change
	// cursor is the wall clock's, so the edit is after it.
	transcript := f.liveID("transcripts", "not_1")
	edited := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	account.mu.Lock()
	account.notes["not_1"] = granolaTestNote{"2026-10-01T09:00:00Z", edited.Format(time.RFC3339), "## One\n\nFirst, edited."}
	account.mu.Unlock()
	if res := f.run(t, "sync-3"); res.Mode != model.ConnectorSyncChanges {
		t.Fatalf("mode = %s", res.Mode)
	}
	if gets := account.took(); fmt.Sprint(gets) != "map[not_1?include=transcript:1]" {
		t.Fatalf("Get Notes = %v", gets)
	}
	if got := f.held(t, "notes")["not_1"]; !strings.HasSuffix(got, "First, edited.\n") {
		t.Fatalf("notes not_1 = %q", got)
	}
	if f.liveID("transcripts", "not_1") != transcript || f.store.docs[transcript].Source.Version != "1|"+edited.Format(time.RFC3339) {
		t.Fatalf("transcript = %s, was %s", f.liveID("transcripts", "not_1"), transcript)
	}

	// A full read gets each note without its transcript, which is held, and
	// removes the documents of a note Granola no longer lists.
	account.mu.Lock()
	delete(account.notes, "not_2")
	account.mu.Unlock()
	full := newSync("sync-4")
	full.Mode = model.ConnectorSyncFull
	if _, err := f.syncer.Run(context.Background(), full); err != nil {
		t.Fatal(err)
	}
	if gets := account.took(); fmt.Sprint(gets) != "map[not_1?:1]" {
		t.Fatalf("Get Notes = %v", gets)
	}
	if notes, transcripts := f.held(t, "notes"), f.held(t, "transcripts"); len(notes) != 1 || len(transcripts) != 1 || notes["not_1"] == "" {
		t.Fatalf("notes = %q, transcripts = %q", notes, transcripts)
	}

	// Turning transcripts off empties their set at the next sync and keeps it.
	f.store.connectors["conn-1"].Config = json.RawMessage(`{}`)
	f.store.connectors["conn-1"].ChangeCursor = nil
	f.run(t, "sync-5")
	if gets := account.took(); fmt.Sprint(gets) != "map[not_1?:1]" {
		t.Fatalf("Get Notes = %v", gets)
	}
	if transcripts := f.held(t, "transcripts"); len(transcripts) != 0 || f.store.setOf("transcripts") == nil || len(f.held(t, "notes")) != 1 {
		t.Fatalf("transcripts = %q", transcripts)
	}
}
