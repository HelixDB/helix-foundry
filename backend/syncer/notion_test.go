package syncer

import (
	"cmp"
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

// notionWorkspace is a Notion workspace a test edits between syncs: its
// data sources and the pages shared with the integration, by id. It filters
// a query by last edit and a search by the trash, and sorts a search by
// last edit, as Notion does, and counts every Markdown read by page and
// every query by data source and filter.
type notionWorkspace struct {
	mu      sync.Mutex
	sources map[string]notionTestSource
	pages   map[string]notionTestPage
	reads   map[string]int
	queries map[string]int
}

// notionTestSource is a shared data source: its title and its properties
// but its title property, Name.
type notionTestSource struct{ title, properties string }

// notionTestTasks is the Tasks data source: a title and a checkbox.
var notionTestTasks = notionTestSource{title: "Tasks", properties: `"Done":{"id":"dn","type":"checkbox","checkbox":{}}`}

// notionTestPage is a shared page: its last edit, title and body, the data
// source it is a row of ("" for none) and its Done, whether it is in the
// trash, and whether the search misses it, as Notion's may.
type notionTestPage struct {
	edited, title, body, source string
	done, trashed, unsearched   bool
}

func (w *notionWorkspace) serve(t *testing.T) string {
	t.Helper()
	list := func(items []string) string {
		return `{"object":"list","results":[` + strings.Join(items, ",") + `],"next_cursor":null,"has_more":false}`
	}
	page := func(id string, p notionTestPage) string {
		parent := `{"type":"workspace","workspace":true}`
		if p.source != "" {
			parent = fmt.Sprintf(`{"type":"data_source_id","data_source_id":%q,"database_id":"db_1"}`, p.source)
		}
		return fmt.Sprintf(`{"object":"page","id":%q,"created_time":"2026-10-01T09:00:00.000Z","last_edited_time":%q,
			"created_by":{"object":"user","id":"usr_ada"},"last_edited_by":{"object":"user","id":"usr_ada"},"parent":%s,
			"in_trash":%t,"url":"https://www.notion.so/%s","properties":{"Name":{"id":"title","type":"title","title":[{"plain_text":%q}]},
			"Done":{"id":"dn","type":"checkbox","checkbox":%t}}}`, id, p.edited, parent, p.trashed, id, p.title, p.done)
	}
	// listed are the pages keep takes, by last edit, the newest first when newest.
	listed := func(keep func(notionTestPage) bool, newest bool) []string {
		ids := slices.DeleteFunc(slices.Sorted(maps.Keys(w.pages)), func(id string) bool { return !keep(w.pages[id]) })
		slices.SortStableFunc(ids, func(a, b string) int {
			if newest {
				a, b = b, a
			}
			return strings.Compare(w.pages[a].edited, w.pages[b].edited)
		})
		items := make([]string, len(ids))
		for i, id := range ids {
			items[i] = page(id, w.pages[id])
		}
		return items
	}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer sk_test_abc" || r.Header.Get("Notion-Version") != "2026-03-11" {
			t.Errorf("%s %s: headers = %v", r.Method, r.URL.Path, r.Header)
		}
		var body struct {
			Filter struct {
				Value   string `json:"value"`
				InTrash bool   `json:"in_trash"`
				// A query's: its last edit's bound, alone or with its creation's.
				Timestamp      string `json:"timestamp"`
				LastEditedTime struct {
					OnOrAfter string `json:"on_or_after"`
				} `json:"last_edited_time"`
			} `json:"filter"`
			Sort     struct{ Direction string } `json:"sort"`
			PageSize int                        `json:"page_size"`
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
			for _, id := range slices.Sorted(maps.Keys(w.sources)) {
				s := w.sources[id]
				items = append(items, fmt.Sprintf(`{"object":"data_source","id":%q,"created_time":"2026-10-01T00:00:00.000Z",
					"title":[{"plain_text":%q}],"properties":{"Name":{"id":"title","type":"title","title":{}},%s}}`, id, s.title, s.properties))
			}
		case path == "/v1/search":
			items = listed(func(p notionTestPage) bool { return !p.unsearched && p.trashed == body.Filter.InTrash }, body.Sort.Direction == "descending")
		case strings.HasPrefix(path, "/v1/data_sources/") && strings.HasSuffix(path, "/query"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/data_sources/"), "/query")
			since, _ := time.Parse(time.RFC3339, body.Filter.LastEditedTime.OnOrAfter)
			read := "all"
			if body.Filter.Timestamp == "last_edited_time" {
				read = "since"
			}
			w.queries[fmt.Sprintf("%s %s %s", id, read, cmp.Or(r.URL.Query().Get("filter_properties"), "whole"))]++
			items = listed(func(p notionTestPage) bool {
				edited, _ := time.Parse(time.RFC3339, p.edited)
				return p.source == id && !p.trashed && !edited.Before(since)
			}, false)
		case strings.HasSuffix(path, "/markdown"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/pages/"), "/markdown")
			w.reads[id]++
			p, ok := w.pages[id]
			if !ok || p.trashed {
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

// edit changes the workspace under its lock.
func (w *notionWorkspace) edit(change func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	change()
}

// took are the Markdown reads and the queries since the last call.
func (w *notionWorkspace) took() (reads, queries map[string]int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	reads, queries = w.reads, w.queries
	w.reads, w.queries = map[string]int{}, map[string]int{}
	return reads, queries
}

// notionFixture is a Notion connector on plan code over workspace.
func notionFixture(t *testing.T, code string, workspace *notionWorkspace) fixture {
	t.Helper()
	workspace.reads, workspace.queries = map[string]int{}, map[string]int{}
	f := newFixture(t, "http://unused", "notion", `{}`, plan{code: code})
	endpoints := connector.DefaultEndpoints()
	endpoints.Notion = workspace.serve(t)
	f.syncer.Catalog = connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{}, connector.EventsOptions{})
	return f
}

func notionDocument(id, title, body string) string {
	return fmt.Sprintf("# %s\n\nPage id: %s\n\n%s\n", title, id, body)
}

// TestNotionSyncDocuments runs Notion through the syncer on Free, which
// reads in full every sync: the first sync fills the Pages set from one
// Markdown read per page, an unchanged sync reads only the page that has
// no document, an edit of a row's properties alone updates its document in
// place, a row the search misses keeps its own, an edit of a body replaces
// its page's document, a page no longer shared loses its rows and its
// document, and a body emptied loses its document.
func TestNotionSyncDocuments(t *testing.T) {
	t.Parallel() // Notion's pacing makes it slow; it shares nothing with another test
	workspace := &notionWorkspace{sources: map[string]notionTestSource{"ds_tasks": notionTestTasks}, pages: map[string]notionTestPage{
		"pg_1": {edited: "2026-10-01T10:00:00.000Z", title: "Roadmap", body: "## Q4\n\nShip the connector."},
		"pg_2": {edited: "2026-10-02T10:00:00.000Z", title: "Write the guide", body: "Draft, then review.", source: "ds_tasks", done: true},
		"pg_3": {edited: "2026-10-03T10:00:00.000Z", title: "Blank", body: "\n"},
	}}
	f := notionFixture(t, "free", workspace)
	run := func(t *testing.T, id string) {
		t.Helper()
		if res := f.run(t, id); res.Mode != model.ConnectorSyncFull {
			t.Fatalf("sync %s mode = %s, want full on Free", id, res.Mode)
		}
	}

	run(t, "sync-1")
	if tables := slices.Sorted(maps.Keys(f.store.tables)); !slices.Equal(tables, []string{"pages", "tasks", "users"}) {
		t.Fatalf("tables = %v", tables)
	}
	if reads, _ := workspace.took(); fmt.Sprint(reads) != "map[pg_1:1 pg_2:1 pg_3:1]" {
		t.Fatalf("Markdown reads = %v; want each page once", reads)
	}
	pages := f.held(t, "pages")
	if pages["pg_1"] != notionDocument("pg_1", "Roadmap", "## Q4\n\nShip the connector.") ||
		pages["pg_2"] != notionDocument("pg_2", "Write the guide", "Draft, then review.") || len(pages) != 2 {
		t.Fatalf("pages = %q", pages)
	}
	if set := f.store.setOf("pages"); set.Name != "Pages" || set.Owner.ConnectorID != "conn-1" {
		t.Fatalf("pages set = %+v", set)
	}
	if tasks := f.snapshotRows(t, "tasks"); len(tasks) != 1 || !strings.Contains(tasks[0], `"done":true`) || !strings.Contains(tasks[0], `"page_id":"pg_2"`) {
		t.Fatalf("tasks = %v", tasks)
	}
	if f.store.connectors["conn-1"].ChangeCursor != nil {
		t.Fatalf("cursor = %s; Free follows no table", f.store.connectors["conn-1"].ChangeCursor)
	}

	// Nothing changed: only the page without a document is read again.
	run(t, "sync-2")
	if reads, _ := workspace.took(); fmt.Sprint(reads) != "map[pg_3:1]" {
		t.Fatalf("Markdown reads = %v", reads)
	}

	// A row's properties edited, its body not: its last edit moves, so it
	// is read again, and its document, the same bytes, is updated in place
	// rather than replaced and extracted again.
	task := f.liveID("pages", "pg_2")
	workspace.edit(func() {
		workspace.pages["pg_2"] = notionTestPage{edited: "2026-10-03T12:00:00.000Z", title: "Write the guide", body: "Draft, then review.", source: "ds_tasks"}
	})
	run(t, "sync-2b")
	if reads, _ := workspace.took(); fmt.Sprint(reads) != "map[pg_2:1 pg_3:1]" || f.liveID("pages", "pg_2") != task {
		t.Fatalf("Markdown reads = %v; pg_2's document %s, was %s", reads, f.liveID("pages", "pg_2"), task)
	}

	// A search that misses a row of a data source leaves its row and
	// document as they are: its query lists it.
	workspace.edit(func() {
		p := workspace.pages["pg_2"]
		p.unsearched = true
		workspace.pages["pg_2"] = p
	})
	run(t, "sync-2c")
	if reads, _ := workspace.took(); fmt.Sprint(reads) != "map[pg_3:1]" || f.liveID("pages", "pg_2") != task || f.store.tables["pages"].RowCount != 3 {
		t.Fatalf("Markdown reads = %v; pg_2's document %s, was %s; %d pages", reads, f.liveID("pages", "pg_2"), task, f.store.tables["pages"].RowCount)
	}

	// An edit moves its page's last edit, so its document is read and
	// replaced; a page no longer shared leaves the search, its table's rows
	// and its set.
	workspace.edit(func() {
		workspace.pages["pg_1"] = notionTestPage{edited: "2026-10-04T08:30:00.000Z", title: "Roadmap", body: "## Q4\n\nShipped."}
		delete(workspace.pages, "pg_2")
	})
	run(t, "sync-3")
	if reads, _ := workspace.took(); fmt.Sprint(reads) != "map[pg_1:1 pg_3:1]" {
		t.Fatalf("Markdown reads = %v", reads)
	}
	if pages := f.held(t, "pages"); len(pages) != 1 || pages["pg_1"] != notionDocument("pg_1", "Roadmap", "## Q4\n\nShipped.") {
		t.Fatalf("pages = %q", pages)
	}
	if tasks, pages := f.store.tables["tasks"].RowCount, f.store.tables["pages"].RowCount; tasks != 0 || pages != 2 {
		t.Fatalf("rows: tasks = %d, pages = %d", tasks, pages)
	}

	// A body emptied takes its document back; the page keeps its row.
	workspace.edit(func() {
		workspace.pages["pg_1"] = notionTestPage{edited: "2026-10-05T08:30:00.000Z", title: "Roadmap", body: " \n"}
	})
	run(t, "sync-4")
	if pages := f.held(t, "pages"); len(pages) != 0 || f.store.tables["pages"].RowCount != 2 {
		t.Fatalf("pages = %q, rows = %d", pages, f.store.tables["pages"].RowCount)
	}
}

// TestNotionSyncChanges runs Notion through the syncer on Pro, which
// follows the pages table and each data source's from a first full read:
// an unchanged sync queries each data source once and writes nothing; an
// edit, a new page and an emptied body change their rows and documents; an
// edit of a row's properties alone updates its document in place; a page
// trashed loses its row and document; a data source whose schema changed
// is read in full within the change sync; and a cursor it can't read, a
// new data source and a reconcile due each read the whole sync in full.
func TestNotionSyncChanges(t *testing.T) {
	t.Parallel() // Notion's pacing makes it slow; it shares nothing with another test
	workspace := &notionWorkspace{sources: map[string]notionTestSource{"ds_tasks": notionTestTasks}, pages: map[string]notionTestPage{
		"pg_1": {edited: "2026-10-01T10:00:00.000Z", title: "Roadmap", body: "## Q4\n\nShip the connector."},
		"pg_2": {edited: "2026-10-02T10:00:00.000Z", title: "Write the guide", body: "Draft, then review.", source: "ds_tasks", done: true},
		"pg_3": {edited: "2026-10-03T10:00:00.000Z", title: "Blank", body: "\n"},
	}}
	f := notionFixture(t, "pro", workspace)
	// Edits are after the cursor, which is the wall clock's.
	minute := 0
	edited := func() string {
		minute++
		return time.Now().UTC().Truncate(time.Minute).Add(time.Duration(minute) * time.Minute).Format(time.RFC3339)
	}
	changes := func(t *testing.T, id string) Result {
		t.Helper()
		res := f.run(t, id)
		if res.Mode != model.ConnectorSyncChanges {
			t.Fatalf("sync %s mode = %s, want changes", id, res.Mode)
		}
		slices.Sort(res.Changed)
		return res
	}
	row := func(t *testing.T, table, id string) string {
		t.Helper()
		for _, r := range f.snapshotRows(t, table) {
			if strings.Contains(r, `"page_id":"`+id+`"`) {
				return r
			}
		}
		return ""
	}

	if res := f.run(t, "sync-1"); res.Mode != model.ConnectorSyncFull || len(f.held(t, "pages")) != 2 {
		t.Fatalf("mode = %s, pages = %q", res.Mode, f.held(t, "pages"))
	}
	if reads, queries := workspace.took(); fmt.Sprint(reads) != "map[pg_1:1 pg_2:1 pg_3:1]" ||
		fmt.Sprint(queries) != "map[ds_tasks all whole:1]" {
		t.Fatalf("Markdown reads = %v, queries = %v", reads, queries)
	}

	// Nothing changed: one query of the data source, and nothing read or written.
	publishes, batches := len(f.store.publishes), len(slices.Concat(slices.Collect(maps.Values(f.store.batches))...))
	if res := changes(t, "sync-2"); len(res.Changed) != 0 {
		t.Fatalf("changed = %v", res.Changed)
	}
	if reads, queries := workspace.took(); len(reads) != 0 || fmt.Sprint(queries) != "map[ds_tasks since whole:1]" ||
		len(f.store.publishes) != publishes || len(slices.Concat(slices.Collect(maps.Values(f.store.batches))...)) != batches {
		t.Fatalf("Markdown reads = %v, queries = %v, publishes = %d, batches = %d", reads, queries,
			len(f.store.publishes)-publishes, len(slices.Concat(slices.Collect(maps.Values(f.store.batches))...))-batches)
	}

	// A row retitled updates its row in both tables and replaces its
	// document; a new page adds its own; an emptied body takes its back.
	task := f.liveID("pages", "pg_2")
	workspace.edit(func() {
		workspace.pages["pg_2"] = notionTestPage{edited: edited(), title: "Write the docs", body: "Draft, then review.", source: "ds_tasks", done: true}
		workspace.pages["pg_4"] = notionTestPage{edited: edited(), title: "Launch", body: "Launch in May."}
		workspace.pages["pg_1"] = notionTestPage{edited: edited(), title: "Roadmap", body: " \n"}
	})
	if res := changes(t, "sync-3"); !slices.Equal(res.Changed, []string{"pages", "tasks"}) {
		t.Fatalf("changed = %v", res.Changed)
	}
	if reads, queries := workspace.took(); fmt.Sprint(reads) != "map[pg_1:1 pg_2:1 pg_4:1]" || fmt.Sprint(queries) != "map[ds_tasks since whole:1]" {
		t.Fatalf("Markdown reads = %v, queries = %v", reads, queries)
	}
	pages := f.held(t, "pages")
	if len(pages) != 2 || pages["pg_2"] != notionDocument("pg_2", "Write the docs", "Draft, then review.") ||
		pages["pg_4"] != notionDocument("pg_4", "Launch", "Launch in May.") || f.liveID("pages", "pg_2") == task {
		t.Fatalf("pages = %q", pages)
	}
	if !strings.Contains(row(t, "tasks", "pg_2"), `"name":"Write the docs"`) || !strings.Contains(row(t, "pages", "pg_2"), `"title":"Write the docs"`) ||
		row(t, "pages", "pg_4") == "" || f.store.tables["pages"].RowCount != 4 || f.store.tables["tasks"].RowCount != 1 {
		t.Fatalf("tasks = %v, pages = %v", f.snapshotRows(t, "tasks"), f.snapshotRows(t, "pages"))
	}

	// A row's properties edited, its body and title not: the same bytes, so
	// its document is updated in place, not replaced and extracted again. A
	// page trashed since loses its row and its document. pg_1 has no
	// document to hold its version, so it is read again while its edit is
	// within the cursor's reach.
	task = f.liveID("pages", "pg_2")
	at := edited()
	workspace.edit(func() {
		workspace.pages["pg_2"] = notionTestPage{edited: at, title: "Write the docs", body: "Draft, then review.", source: "ds_tasks"}
		p := workspace.pages["pg_4"]
		p.edited, p.trashed = edited(), true
		workspace.pages["pg_4"] = p
	})
	if res := changes(t, "sync-4"); !slices.Equal(res.Changed, []string{"pages", "tasks"}) {
		t.Fatalf("changed = %v", res.Changed)
	}
	if reads, queries := workspace.took(); fmt.Sprint(reads) != "map[pg_1:1 pg_2:1]" || fmt.Sprint(queries) != "map[ds_tasks since whole:1]" ||
		f.liveID("pages", "pg_2") != task || !strings.Contains(f.store.docs[task].Source.Version, at[:16]) ||
		!strings.Contains(row(t, "tasks", "pg_2"), `"done":false`) {
		t.Fatalf("Markdown reads = %v, queries = %v; pg_2's document %s, was %s, version %s", reads, queries,
			f.liveID("pages", "pg_2"), task, f.store.docs[task].Source.Version)
	}
	if pages := f.held(t, "pages"); len(pages) != 1 || pages["pg_2"] == "" || row(t, "pages", "pg_4") != "" || f.store.tables["pages"].RowCount != 3 {
		t.Fatalf("pages = %q, rows = %v", pages, f.snapshotRows(t, "pages"))
	}

	// A property added to Tasks: its stored rows lack it, so the table is
	// read in full within the change sync, its titles queried for pages,
	// and the cursor holds the new schema for the next.
	type notionCursor struct {
		Since   time.Time
		Tables  []string
		Sources map[string]struct{ Table, Schema string }
	}
	stored := func() (c notionCursor) {
		t.Helper()
		if err := json.Unmarshal(f.store.connectors["conn-1"].ChangeCursor, &c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	was := stored().Sources["ds_tasks"]
	workspace.edit(func() {
		workspace.sources["ds_tasks"] = notionTestSource{title: "Tasks", properties: notionTestTasks.properties + `,"Owner":{"id":"ow","type":"rich_text","rich_text":{}}`}
	})
	changes(t, "sync-5")
	if _, queries := workspace.took(); fmt.Sprint(queries) != "map[ds_tasks all whole:1 ds_tasks since title:1]" ||
		!strings.Contains(string(f.store.tables["tasks"].Columns), `"owner"`) || !strings.Contains(row(t, "tasks", "pg_2"), `"owner":null`) ||
		stored().Sources["ds_tasks"].Schema == was.Schema || stored().Sources["ds_tasks"].Table != "tasks" {
		t.Fatalf("queries = %v, columns = %s, tasks = %v, cursor = %s", queries, f.store.tables["tasks"].Columns, f.snapshotRows(t, "tasks"),
			f.store.connectors["conn-1"].ChangeCursor)
	}

	// A cursor it can't read is read in full, and replaced.
	f.store.connectors["conn-1"].ChangeCursor = json.RawMessage(`"2026-10-01"`)
	if res := f.run(t, "sync-6"); res.Mode != model.ConnectorSyncFull {
		t.Fatalf("mode = %s", res.Mode)
	}
	if c := stored(); c.Since.IsZero() || !slices.Equal(c.Tables, []string{"pages", "tasks"}) {
		t.Fatalf("cursor = %s", f.store.connectors["conn-1"].ChangeCursor)
	}

	// A data source newly shared is a new table, which the sync reads in full.
	workspace.edit(func() {
		workspace.sources["ds_notes"] = notionTestSource{title: "Notes", properties: `"Done":{"id":"dn","type":"checkbox","checkbox":{}}`}
		workspace.pages["pg_5"] = notionTestPage{edited: "2026-10-02T10:00:00.000Z", title: "Standup", body: "Notes.", source: "ds_notes"}
	})
	if res := f.run(t, "sync-7"); res.Mode != model.ConnectorSyncFull || f.store.tables["notes"] == nil || row(t, "pages", "pg_5") == "" ||
		f.held(t, "pages")["pg_5"] == "" {
		t.Fatalf("mode = %s, tables = %v", res.Mode, slices.Sorted(maps.Keys(f.store.tables)))
	}
	workspace.took()

	// The daily reconcile reads in full what a change read can't see.
	old := time.Now().Add(-25 * time.Hour)
	f.store.tables["notes"].ReconciledAt = &old
	if res := f.run(t, "sync-8"); res.Mode != model.ConnectorSyncFull {
		t.Fatalf("mode = %s with a reconcile due", res.Mode)
	}
	if _, queries := workspace.took(); fmt.Sprint(queries) != "map[ds_notes all whole:1 ds_tasks all whole:1]" {
		t.Fatalf("queries = %v", queries)
	}
}
