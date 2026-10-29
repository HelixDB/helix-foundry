package syncer

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// notionWorkspace is a Notion workspace a test edits between syncs: the
// pages shared with the integration, by id. It counts every Markdown read
// by page.
type notionWorkspace struct {
	mu    sync.Mutex
	pages map[string]notionTestPage
	reads map[string]int
}

// notionTestPage is a shared page: its last edit, title and body, whether
// it is a row of the Tasks data source, and whether the search misses it,
// as Notion's may.
type notionTestPage struct {
	edited, title, body string
	task, unsearched    bool
}

func (w *notionWorkspace) serve(t *testing.T) string {
	t.Helper()
	list := func(items []string) string {
		return `{"object":"list","results":[` + strings.Join(items, ",") + `],"next_cursor":null,"has_more":false}`
	}
	page := func(id string, p notionTestPage) string {
		parent := `{"type":"workspace","workspace":true}`
		if p.task {
			parent = `{"type":"data_source_id","data_source_id":"ds_tasks","database_id":"db_tasks"}`
		}
		return fmt.Sprintf(`{"object":"page","id":%q,"created_time":"2026-10-01T09:00:00.000Z","last_edited_time":%q,
			"created_by":{"object":"user","id":"usr_ada"},"last_edited_by":{"object":"user","id":"usr_ada"},"parent":%s,
			"url":"https://www.notion.so/%s","properties":{"Name":{"id":"title","type":"title","title":[{"plain_text":%q}]},
			"Done":{"id":"dn","type":"checkbox","checkbox":true}}}`, id, p.edited, parent, id, p.title)
	}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer sk_test_abc" || r.Header.Get("Notion-Version") != "2026-03-11" {
			t.Errorf("%s %s: headers = %v", r.Method, r.URL.Path, r.Header)
		}
		var body struct {
			Filter   struct{ Value string } `json:"filter"`
			PageSize int                    `json:"page_size"`
		}
		if r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		var items []string
		switch path := r.URL.Path; {
		case path == "/v1/users/me":
			_, _ = rw.Write([]byte(`{"object":"user","id":"usr_bot","type":"bot","name":"Helix","bot":{}}`))
			return
		case path == "/v1/users":
			items = []string{`{"object":"user","id":"usr_ada","type":"person","name":"Ada Lovelace","person":{"email":"ada@example.com"}}`}
		case path == "/v1/search" && body.Filter.Value == "data_source":
			items = []string{`{"object":"data_source","id":"ds_tasks","created_time":"2026-10-01T00:00:00.000Z","title":[{"plain_text":"Tasks"}],
				"properties":{"Name":{"id":"title","type":"title","title":{}},"Done":{"id":"dn","type":"checkbox","checkbox":{}}}}`}
		case path == "/v1/search" || path == "/v1/data_sources/ds_tasks/query":
			for _, id := range slices.Sorted(maps.Keys(w.pages)) {
				if p := w.pages[id]; path == "/v1/search" && !p.unsearched || path != "/v1/search" && p.task {
					items = append(items, page(id, p))
				}
			}
		case strings.HasSuffix(path, "/markdown"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/pages/"), "/markdown")
			w.reads[id]++
			p, ok := w.pages[id]
			if !ok {
				rw.WriteHeader(http.StatusNotFound)
				_, _ = rw.Write([]byte(`{"object":"error","status":404,"code":"object_not_found"}`))
				return
			}
			_, _ = fmt.Fprintf(rw, `{"object":"page_markdown","id":%q,"markdown":%q,"truncated":false,"unknown_block_ids":[]}`, id, p.body)
			return
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		if body.PageSize == 1 || r.URL.Query().Get("page_size") == "1" {
			items = items[:min(1, len(items))]
		}
		_, _ = rw.Write([]byte(list(items)))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// took are the Markdown reads since the last call, by page.
func (w *notionWorkspace) took() map[string]int {
	w.mu.Lock()
	defer w.mu.Unlock()
	reads := w.reads
	w.reads = map[string]int{}
	return reads
}

// TestNotionSyncDocuments runs Notion through the syncer: the first sync
// fills the Pages set from one Markdown read per page, an unchanged sync
// reads only the page that has no document, an edit of a row's properties
// alone updates its document in place, a row the search misses keeps its
// own, an edit of a body replaces its page's document, a page no longer
// shared loses its rows and its document, and a body emptied loses its
// document.
func TestNotionSyncDocuments(t *testing.T) {
	workspace := &notionWorkspace{reads: map[string]int{}, pages: map[string]notionTestPage{
		"pg_1": {edited: "2026-10-01T10:00:00.000Z", title: "Roadmap", body: "## Q4\n\nShip the connector."},
		"pg_2": {edited: "2026-10-02T10:00:00.000Z", title: "Write the guide", body: "Draft, then review.", task: true},
		"pg_3": {edited: "2026-10-03T10:00:00.000Z", title: "Blank", body: "\n"},
	}}
	f := newFixture(t, "http://unused", "notion", `{}`, plan{code: "pro"})
	endpoints := connector.DefaultEndpoints()
	endpoints.Notion = workspace.serve(t)
	f.syncer.Catalog = connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{}, connector.EventsOptions{})
	document := func(id, title, body string) string {
		return fmt.Sprintf("# %s\n\nPage id: %s\n\n%s\n", title, id, body)
	}

	res := f.run(t, "sync-1")
	if tables := slices.Sorted(maps.Keys(f.store.tables)); res.Mode != model.ConnectorSyncFull || !slices.Equal(tables, []string{"pages", "tasks", "users"}) {
		t.Fatalf("mode = %s, tables = %v", res.Mode, tables)
	}
	if reads := workspace.took(); fmt.Sprint(reads) != "map[pg_1:1 pg_2:1 pg_3:1]" {
		t.Fatalf("Markdown reads = %v; want each page once", reads)
	}
	pages := f.held(t, "pages")
	if pages["pg_1"] != document("pg_1", "Roadmap", "## Q4\n\nShip the connector.") ||
		pages["pg_2"] != document("pg_2", "Write the guide", "Draft, then review.") || len(pages) != 2 {
		t.Fatalf("pages = %q", pages)
	}
	if set := f.store.setOf("pages"); set.Name != "Pages" || set.Owner.ConnectorID != "conn-1" {
		t.Fatalf("pages set = %+v", set)
	}
	if tasks := f.snapshotRows(t, "tasks"); len(tasks) != 1 || !strings.Contains(tasks[0], `"done":true`) || !strings.Contains(tasks[0], `"page_id":"pg_2"`) {
		t.Fatalf("tasks = %v", tasks)
	}

	// Nothing changed: only the page without a document is read again.
	f.run(t, "sync-2")
	if reads := workspace.took(); fmt.Sprint(reads) != "map[pg_3:1]" {
		t.Fatalf("Markdown reads = %v", reads)
	}

	// A row's properties edited, its body not: its last edit moves, so it
	// is read again, and its document, the same bytes, is updated in place
	// rather than replaced and extracted again.
	task := f.liveID("pages", "pg_2")
	workspace.mu.Lock()
	workspace.pages["pg_2"] = notionTestPage{edited: "2026-10-03T12:00:00.000Z", title: "Write the guide", body: "Draft, then review.", task: true}
	workspace.mu.Unlock()
	f.run(t, "sync-2b")
	if reads := workspace.took(); fmt.Sprint(reads) != "map[pg_2:1 pg_3:1]" || f.liveID("pages", "pg_2") != task {
		t.Fatalf("Markdown reads = %v; pg_2's document %s, was %s", reads, f.liveID("pages", "pg_2"), task)
	}

	// A search that misses a row of a data source leaves its row and
	// document as they are: its query lists it.
	workspace.mu.Lock()
	p := workspace.pages["pg_2"]
	p.unsearched = true
	workspace.pages["pg_2"] = p
	workspace.mu.Unlock()
	f.run(t, "sync-2c")
	if reads := workspace.took(); fmt.Sprint(reads) != "map[pg_3:1]" || f.liveID("pages", "pg_2") != task || f.store.tables["pages"].RowCount != 3 {
		t.Fatalf("Markdown reads = %v; pg_2's document %s, was %s; %d pages", reads, f.liveID("pages", "pg_2"), task, f.store.tables["pages"].RowCount)
	}

	// An edit moves its page's last edit, so its document is read and
	// replaced; a page no longer shared leaves the search, its table's rows
	// and its set.
	workspace.mu.Lock()
	workspace.pages["pg_1"] = notionTestPage{edited: "2026-10-04T08:30:00.000Z", title: "Roadmap", body: "## Q4\n\nShipped."}
	delete(workspace.pages, "pg_2")
	workspace.mu.Unlock()
	f.run(t, "sync-3")
	if reads := workspace.took(); fmt.Sprint(reads) != "map[pg_1:1 pg_3:1]" {
		t.Fatalf("Markdown reads = %v", reads)
	}
	if pages := f.held(t, "pages"); len(pages) != 1 || pages["pg_1"] != document("pg_1", "Roadmap", "## Q4\n\nShipped.") {
		t.Fatalf("pages = %q", pages)
	}
	if tasks, pages := f.store.tables["tasks"].RowCount, f.store.tables["pages"].RowCount; tasks != 0 || pages != 2 {
		t.Fatalf("rows: tasks = %d, pages = %d", tasks, pages)
	}

	// A body emptied takes its document back; the page keeps its row.
	workspace.mu.Lock()
	workspace.pages["pg_1"] = notionTestPage{edited: "2026-10-05T08:30:00.000Z", title: "Roadmap", body: " \n"}
	workspace.mu.Unlock()
	f.run(t, "sync-4")
	if pages := f.held(t, "pages"); len(pages) != 0 || f.store.tables["pages"].RowCount != 2 {
		t.Fatalf("pages = %q, rows = %d", pages, f.store.tables["pages"].RowCount)
	}
}
