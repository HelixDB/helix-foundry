package connector

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// notionEdits is a workspace a change read reads: its data sources by id,
// those its search leaves out (unsearched), its pages by id, each page's
// Markdown, and the requests it answered. It filters a query by last edit
// and a search by the trash, and sorts a search by last edit, as Notion
// does; a data source read by id is one it has, else 404.
type notionEdits struct {
	t          *testing.T
	sources    map[string]string
	unsearched map[string]bool
	pages      map[string]notionEdit
	bodies     map[string]string
	mu         sync.Mutex
	asked      []string
}

// notionEdit is a page: its last edit, the data source it is a row of ("" for
// a page that isn't), its properties, and whether it is in the trash.
type notionEdit struct {
	edited, source, properties string
	trashed                    bool
	// created is when the page was made; before every cursor when empty.
	created string
}

func (e notionEdit) item(id string) string {
	parent := notionWorkspace
	if e.source != "" {
		parent = notionUnder("data_source_id", e.source)
	}
	created := cmp.Or(e.created, "2026-01-05T09:00:00.000Z")
	return fmt.Sprintf(`{"object":"page","id":%q,"created_time":%q,"last_edited_time":%q,
		"created_by":{"object":"user","id":"usr_ada"},"last_edited_by":{"object":"user","id":"usr_ada"},"in_trash":%t,
		"parent":%s,"url":"https://www.notion.so/%s","properties":%s}`, id, created, e.edited, e.trashed, parent, id, e.properties)
}

// ask records a request.
func (e *notionEdits) ask(request string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.asked = append(e.asked, request)
}

// took are the requests since the last call.
func (e *notionEdits) took() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	asked := e.asked
	e.asked = nil
	return asked
}

// listed are the pages keep takes, by last edit, the newest first when newest.
func (e *notionEdits) listed(keep func(notionEdit) bool, newest bool) []string {
	var ids []string
	for _, id := range slices.Sorted(maps.Keys(e.pages)) {
		if keep(e.pages[id]) {
			ids = append(ids, id)
		}
	}
	slices.SortStableFunc(ids, func(a, b string) int {
		order := strings.Compare(e.pages[a].edited, e.pages[b].edited)
		if newest {
			return -order
		}
		return order
	})
	items := make([]string, len(ids))
	for i, id := range ids {
		items[i] = e.pages[id].item(id)
	}
	return items
}

// kind serves the workspace, with extra routes added or replacing its own.
func (e *notionEdits) kind(extra notionRoutes) *notionKind {
	t := e.t
	routes := notionRoutes{
		"/v1/users/me": func(*http.Request) (int, string) { return http.StatusOK, notionBotJSON },
		"/v1/users":    func(*http.Request) (int, string) { return http.StatusOK, notionResults(notionAdaJSON) },
		"/v1/search": func(r *http.Request) (int, string) {
			body := notionBody(t, r)
			filter, _ := body["filter"].(map[string]any)
			order, _ := body["sort"].(map[string]any)
			if filter["value"] == "data_source" {
				e.ask("search data sources")
				return http.StatusOK, notionResults(slices.Collect(func(yield func(string) bool) {
					for _, id := range slices.Sorted(maps.Keys(e.sources)) {
						if !e.unsearched[id] && !yield(e.sources[id]) {
							return
						}
					}
				})...)
			}
			if fields := r.URL.Query()["filter_properties"]; !slices.Equal(fields, []string{"title"}) {
				t.Errorf("search filter_properties = %q", fields)
			}
			trash := filter["in_trash"] == true
			e.ask(fmt.Sprintf("search %s %v", map[bool]string{false: "pages", true: "trash"}[trash], order["direction"]))
			return http.StatusOK, notionResults(e.listed(func(p notionEdit) bool { return p.trashed == trash }, order["direction"] == "descending")...)
		},
	}
	// The data sources the tests' cursors name, shared or not.
	for _, id := range []string{"ds_1", "ds_2", "ds_gone"} {
		routes["/v1/data_sources/"+id] = func(*http.Request) (int, string) {
			e.ask("data source " + id)
			if source, ok := e.sources[id]; ok {
				return http.StatusOK, source
			}
			return http.StatusNotFound, `{"object":"error","status":404,"code":"object_not_found"}`
		}
	}
	for id := range e.sources {
		routes["/v1/data_sources/"+id+"/query"] = func(r *http.Request) (int, string) {
			body := notionBody(t, r)
			filter, _ := body["filter"].(map[string]any)
			bounds := []any{filter}
			if and, ok := filter["and"].([]any); ok {
				bounds = and
			}
			since := ""
			for _, b := range bounds {
				if b, _ := b.(map[string]any); b["timestamp"] == "last_edited_time" {
					since, _ = b["last_edited_time"].(map[string]any)["on_or_after"].(string)
				}
			}
			e.ask(fmt.Sprintf("query %s since %s %s", id, cmp.Or(since, "ever"), cmp.Or(r.URL.Query().Get("filter_properties"), "whole")))
			from, _ := time.Parse(time.RFC3339, since)
			return http.StatusOK, notionResults(e.listed(func(p notionEdit) bool {
				edited, _ := time.Parse(time.RFC3339, p.edited)
				return p.source == id && !p.trashed && !edited.Before(from)
			}, false)...)
		}
	}
	for id, body := range e.bodies {
		routes["/v1/pages/"+id+"/markdown"] = func(*http.Request) (int, string) {
			e.ask("markdown " + id)
			if body == "" {
				return http.StatusNotFound, `{"object":"error","status":404,"code":"object_not_found"}`
			}
			return http.StatusOK, notionMarkdownJSON(id, body, false)
		}
	}
	maps.Copy(routes, extra)
	return notionAt(t, routes)
}

// notionChanges is a ChangeWriter that takes documents as granolaDocs does,
// holds the documents of held (those a full read of the test workspace
// wrote, row_1's and pg_1's, unless set), and logs every change in the
// order it was written. A set fail refuses every change.
type notionChanges struct {
	*granolaDocs
	cursorChanges
	held map[string]bool
	log  []string
	fail error
}

func newNotionChanges() *notionChanges {
	return &notionChanges{granolaDocs: &granolaDocs{want: map[string]bool{"pages": true}}, held: map[string]bool{"row_1": true, "pg_1": true}}
}

func (c *notionChanges) Held(_, id string) (string, bool) { return "2|held", c.held[id] }

func (c *notionChanges) Upsert(table string, row map[string]any) error {
	c.log = append(c.log, fmt.Sprintf("upsert %s %v", table, row["page_id"]))
	if c.fail != nil {
		return c.fail
	}
	return c.cursorChanges.Upsert(table, row)
}

func (c *notionChanges) Delete(table string, key map[string]any) error {
	c.log = append(c.log, fmt.Sprintf("delete %s %v", table, key["page_id"]))
	return c.fail
}

func (c *notionChanges) DeleteWhere(table, column string, value any) error {
	c.log = append(c.log, fmt.Sprintf("cascade %s %s=%v", table, column, value))
	return c.fail
}

func TestNotionChanges(t *testing.T) {
	ctx := context.Background()
	src := Source{Secret: notionTestSecret}
	tables := []Table{{Name: "pages"}, {Name: "customer_accounts"}, {Name: "deals"}}
	// before is an edit older than the searches reach back to (notionSearchLag).
	since := time.Date(2026, 1, 8, 0, 0, 0, 0, time.UTC)
	before, after := "2026-01-07T23:00:00.000Z", "2026-01-08T10:00:00.000Z"
	deals := `{"Name":{"id":"title","name":"Name","type":"title","title":{}},
		"Projects":{"id":"pr","name":"Projects","type":"relation","relation":{}}}`
	title := func(name string) string {
		return fmt.Sprintf(`{"title":{"id":"title","type":"title","title":[{"plain_text":%q}]}}`, name)
	}
	// workspace is two shared data sources, one row each, and a page, all
	// last edited before the cursor but those of edits.
	workspace := func(t *testing.T, edits map[string]notionEdit) *notionEdits {
		e := &notionEdits{t: t, sources: map[string]string{
			"ds_1": notionTestSource("ds_1", "Customer Accounts", "2026-01-01T00:00:00.000Z", notionAccountsSchema),
			"ds_2": notionTestSource("ds_2", "Deals", "2026-01-02T00:00:00.000Z", deals),
		}, pages: map[string]notionEdit{
			"row_1": {edited: before, source: "ds_1", properties: notionAccountValues},
			"row_2": {edited: before, source: "ds_2", properties: `{"Name":{"id":"title","type":"title","title":[{"plain_text":"Big deal"}]}}`},
			"pg_1":  {edited: before, properties: title("Roadmap")},
		}, bodies: map[string]string{"row_1": "Renews in March.", "row_2": "", "pg_1": "## Q1\n\nShip it."}}
		maps.Copy(e.pages, edits)
		return e
	}
	// cursorOf is Start's cursor at since, edited by edit.
	cursorOf := func(t *testing.T, e *notionEdits, edit func(*notionCursor)) json.RawMessage {
		t.Helper()
		start, err := e.kind(nil).Start(ctx, src, tables)
		if err != nil {
			t.Fatal(err)
		}
		var c notionCursor
		if err := json.Unmarshal(start.Cursor, &c); err != nil {
			t.Fatal(err)
		}
		c.Since = since
		if edit != nil {
			edit(&c)
		}
		b, _ := json.Marshal(c)
		e.took()
		return b
	}
	changes := func(t *testing.T, e *notionEdits, cursor json.RawMessage, w ChangeWriter) json.RawMessage {
		t.Helper()
		next, err := e.kind(nil).Changes(ctx, src, tables, cursor, w)
		if err != nil {
			t.Fatal(err)
		}
		return next
	}
	queries := func(fields string) []string {
		return []string{"query ds_1 since 2026-01-08T00:00:00Z " + fields, "query ds_2 since 2026-01-08T00:00:00Z " + fields}
	}
	// The trash first, then each data source's rows, then the other pages.
	unchanged := append(append([]string{"search data sources", "search trash descending"}, queries("whole")...), "search pages descending")

	t.Run("follows pages and every data source's table, not users", func(t *testing.T) {
		kind := notionKindAt(t, "http://unused")
		for table, want := range map[string]Capture{"users": CaptureFull, "pages": CaptureCursor, "customer_accounts": CaptureCursor, "deals": CaptureCursor} {
			if got := kind.Capture(src, table); got != want {
				t.Errorf("Capture(%s) = %s, want %s", table, got, want)
			}
		}
	})
	// The cursor is taken before the full read, to the minute Notion keeps an
	// edit to, so a change made during that read is read again.
	t.Run("starts before the full read, less the overlap, to the minute, with each data source's table and schema", func(t *testing.T) {
		e := workspace(t, nil)
		before := time.Now()
		start, err := e.kind(nil).Start(ctx, src, tables)
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		var c notionCursor
		if err := json.Unmarshal(start.Cursor, &c); err != nil {
			t.Fatal(err)
		}
		low, high := before.Add(-ChangeOverlap).Truncate(time.Minute), after.Add(-ChangeOverlap)
		if c.Since.Before(low) || c.Since.After(high) || !c.Since.Equal(c.Since.Truncate(time.Minute)) {
			t.Fatalf("since = %s, want the read's start less %s to the minute", c.Since, ChangeOverlap)
		}
		if fmt.Sprint(c.Tables) != "[pages customer_accounts deals]" || len(c.Sources) != 2 || c.Sources["ds_1"].Table != "customer_accounts" ||
			c.Sources["ds_2"].Table != "deals" || len(c.Sources["ds_1"].Schema) != 16 || c.Sources["ds_1"].Schema == c.Sources["ds_2"].Schema {
			t.Fatalf("cursor = %s", start.Cursor)
		}
		if asked := e.took(); !slices.Equal(asked, []string{"search data sources"}) {
			t.Fatalf("requests = %q", asked)
		}
	})
	t.Run("refuses a start Notion refuses", func(t *testing.T) {
		e := workspace(t, nil)
		_, err := e.kind(notionRoutes{"/v1/search": func(*http.Request) (int, string) { return http.StatusUnauthorized, `{}` }}).Start(ctx, src, tables)
		notionRefusal(t, err, "Notion rejected this secret; check it and enter it again")
	})
	// An hourly sync with nothing new costs one query a data source, a page
	// of the trash and a page of the search.
	t.Run("queries each data source once and writes nothing when nothing changed", func(t *testing.T) {
		e := workspace(t, nil)
		w := newNotionChanges()
		before := time.Now()
		next := changes(t, e, cursorOf(t, e, nil), w)
		after := time.Now()
		if asked := e.took(); !slices.Equal(asked, unchanged) {
			t.Fatalf("requests = %q\nwant %q", asked, unchanged)
		}
		if len(w.log) != 0 || len(w.resets) != 0 || len(w.offers) != 0 {
			t.Fatalf("changes = %q, resets = %v, offers = %q", w.log, w.resets, w.offers)
		}
		var c notionCursor
		if err := json.Unmarshal(next, &c); err != nil {
			t.Fatal(err)
		}
		if c.Since.Before(before.Add(-ChangeOverlap).Truncate(time.Minute)) || c.Since.After(after.Add(-ChangeOverlap)) ||
			fmt.Sprint(c.Tables) != "[pages customer_accounts deals]" || c.Sources["ds_1"].Table != "customer_accounts" {
			t.Fatalf("next cursor = %s", next)
		}
	})
	// An upsert replaces the stored row, so a change must be exactly what a full read stores.
	t.Run("upserts an edited row into its table and the pages table as a full read writes them, with its document", func(t *testing.T) {
		e := workspace(t, map[string]notionEdit{"row_1": {edited: after, source: "ds_1", properties: notionAccountValues}})
		w := newNotionChanges()
		changes(t, e, cursorOf(t, e, nil), w)
		if want := []string{"upsert customer_accounts row_1", "upsert pages row_1"}; !slices.Equal(w.log, want) {
			t.Fatalf("changes = %q", w.log)
		}
		if !slices.Equal(w.offers, []string{"pages row_1 2|2026-01-08T10:00:00Z|2923"}) || !slices.Equal(w.putNames(), []string{"pages row_1"}) ||
			string(w.puts[0].doc.Text) != "# Acme\n\nPage id: row_1\n\nRenews in March.\n" {
			t.Fatalf("offers = %q, puts = %v", w.offers, w.putNames())
		}
		kind := e.kind(nil)
		for _, table := range []string{"customer_accounts", "pages"} {
			sink := &rowSink{}
			if _, err := kind.Sync(ctx, src, Table{Name: table}, sink); err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(sink.rows, func(row map[string]any) bool { return row["page_id"] == "row_1" })
			if i < 0 || fmt.Sprint(w.upserts[table]) != fmt.Sprint(sink.rows[i:i+1]) {
				t.Errorf("%s: change rows = %v, full rows = %v", table, w.upserts[table], sink.rows)
			}
		}
	})
	// A row's document is its title and body: an edit of its other
	// properties puts the same bytes at a new version, which the syncer
	// updates in place.
	t.Run("puts the same document for an edit of a row's other properties", func(t *testing.T) {
		e := workspace(t, nil)
		full := newNotionChanges()
		if _, err := e.kind(nil).SyncPass(ctx, src, []Table{{Name: "pages"}}, map[string]RowWriter{"pages": &rowSink{}}, full); err != nil {
			t.Fatal(err)
		}
		e.pages["row_1"] = notionEdit{edited: after, source: "ds_1", properties: strings.Replace(notionAccountValues, "Gold", "Silver", 1)}
		w := newNotionChanges()
		changes(t, e, cursorOf(t, e, nil), w)
		i := slices.IndexFunc(full.puts, func(p granolaPut) bool { return p.doc.ExternalID == "row_1" })
		if len(w.puts) != 1 || i < 0 || string(w.puts[0].doc.Text) != string(full.puts[i].doc.Text) || w.puts[0].doc.Version == full.puts[i].doc.Version ||
			w.upserts["customer_accounts"][0]["plan"] != "Silver" {
			t.Fatalf("puts = %v, upserts = %v", w.putNames(), w.upserts)
		}
	})
	t.Run("adds a page edited since, with its document", func(t *testing.T) {
		e := workspace(t, map[string]notionEdit{"pg_2": {edited: after, created: after, properties: title("Launch plan")}})
		e.bodies["pg_2"] = "Launch in May."
		w := newNotionChanges()
		changes(t, e, cursorOf(t, e, nil), w)
		if !slices.Equal(w.log, []string{"upsert pages pg_2"}) || !slices.Equal(w.putNames(), []string{"pages pg_2"}) ||
			w.upserts["pages"][0]["title"] != "Launch plan" {
			t.Fatalf("changes = %q, puts = %v", w.log, w.putNames())
		}
	})
	// A page made before the cursor that the stream holds no document for
	// may be one the full read left past the stream's cap: its document
	// would go again at the reconcile, so only its row is written.
	t.Run("offers no document for an older page the stream holds none for", func(t *testing.T) {
		e := workspace(t, map[string]notionEdit{"row_2": {edited: after, source: "ds_2", properties: title("Bigger deal")}})
		e.bodies["row_2"] = "Signed in June."
		w := newNotionChanges()
		changes(t, e, cursorOf(t, e, nil), w)
		if asked := e.took(); !slices.Equal(w.log, []string{"upsert deals row_2", "upsert pages row_2"}) || len(w.offers) != 0 ||
			slices.Contains(asked, "markdown row_2") {
			t.Fatalf("changes = %q, offers = %q, asked = %q", w.log, w.offers, asked)
		}
	})
	// The search lists rows too, newest first: it stops at the first older
	// than notionSearchLag before the cursor, and a row a query read is that query's.
	t.Run("searches back to its lag before the cursor only, leaving out the rows the queries read", func(t *testing.T) {
		e := workspace(t, map[string]notionEdit{"row_1": {edited: after, source: "ds_1", properties: notionAccountValues}})
		var cursors []string
		kind := e.kind(notionRoutes{"/v1/search": func(r *http.Request) (int, string) {
			body := notionBody(t, r)
			if filter, _ := body["filter"].(map[string]any); filter["value"] == "data_source" {
				return http.StatusOK, notionResults(e.sources["ds_1"], e.sources["ds_2"])
			}
			if filter, _ := body["filter"].(map[string]any); filter["in_trash"] == true {
				return http.StatusOK, notionResults()
			}
			cursor, _ := body["start_cursor"].(string)
			cursors = append(cursors, cursor)
			return http.StatusOK, notionMore("more", e.pages["row_1"].item("row_1"),
				notionEdit{edited: after, properties: title("New")}.item("pg_new"), e.pages["pg_1"].item("pg_1"))
		}, "/v1/pages/pg_new/markdown": func(*http.Request) (int, string) { return http.StatusOK, notionMarkdownJSON("pg_new", "New.", false) }})
		w := newNotionChanges()
		if _, err := kind.Changes(ctx, src, tables, cursorOf(t, e, nil), w); err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(cursors) != "[]" || !slices.Contains(w.log, "upsert pages pg_new") || slices.Contains(w.log, "upsert pages pg_1") ||
			slices.Contains(w.log, "delete pages row_1") {
			t.Fatalf("search cursors = %q, changes = %q", cursors, w.log)
		}
	})
	// A full read of the table this sync writes what its rows are now.
	t.Run("resets a data source the cursor doesn't have, reading its titles for the pages table", func(t *testing.T) {
		e := workspace(t, map[string]notionEdit{"row_1": {edited: after, source: "ds_1", properties: notionAccountValues}})
		w := newNotionChanges()
		changes(t, e, cursorOf(t, e, func(c *notionCursor) { delete(c.Sources, "ds_1") }), w)
		asked := e.took()
		if !slices.Equal(w.resets, []string{"customer_accounts"}) || !slices.Equal(w.log, []string{"upsert pages row_1"}) ||
			!slices.Contains(asked, "query ds_1 since 2026-01-08T00:00:00Z title") || !slices.Contains(asked, "query ds_2 since 2026-01-08T00:00:00Z whole") {
			t.Fatalf("resets = %v, changes = %q, requests = %q", w.resets, w.log, asked)
		}
	})
	t.Run("resets a data source whose schema changed", func(t *testing.T) {
		e := workspace(t, nil)
		w := newNotionChanges()
		changes(t, e, cursorOf(t, e, func(c *notionCursor) {
			c.Sources["ds_2"] = notionSourceCursor{Table: "deals", Schema: "0123456789abcdef"}
		}), w)
		if !slices.Equal(w.resets, []string{"deals"}) || len(w.log) != 0 {
			t.Fatalf("resets = %v, changes = %q", w.resets, w.log)
		}
	})
	// A table whose name another data source held, gone since, holds that one's rows.
	t.Run("resets a table its data source's rows were not read into", func(t *testing.T) {
		e := workspace(t, nil)
		w := newNotionChanges()
		changes(t, e, cursorOf(t, e, func(c *notionCursor) {
			c.Sources["ds_1"] = notionSourceCursor{Table: "customer_accounts__1a2b3c4d", Schema: c.Sources["ds_1"].Schema}
		}), w)
		if !slices.Equal(w.resets, []string{"customer_accounts"}) {
			t.Fatalf("resets = %v", w.resets)
		}
	})
	// A table only Notion's search has gone from since discovery is refused by the full read.
	t.Run("resets a table Notion no longer lists the data source of", func(t *testing.T) {
		e := workspace(t, nil)
		cursor := cursorOf(t, e, nil)
		delete(e.sources, "ds_2")
		w := newNotionChanges()
		changes(t, e, cursor, w)
		if !slices.Equal(w.resets, []string{"deals"}) || !slices.Equal(w.log, []string{"cascade pages data_source_id=ds_2"}) {
			t.Fatalf("resets = %v, changes = %q", w.resets, w.log)
		}
	})
	// Without the pages table, nothing is searched but the trash, for the
	// rows of the tables followed, and a data source reset isn't queried.
	t.Run("reads only the followed data sources when the pages table is reset", func(t *testing.T) {
		e := workspace(t, map[string]notionEdit{"row_1": {edited: after, source: "ds_1", properties: notionAccountValues},
			"row_3": {edited: after, source: "ds_1", properties: notionAccountValues, trashed: true},
			"pg_9":  {edited: after, properties: title("Gone"), trashed: true},
			"pg_2":  {edited: after, properties: title("New")}})
		w := newNotionChanges()
		changes(t, e, cursorOf(t, e, func(c *notionCursor) {
			c.Tables = []string{"customer_accounts", "deals"}
			c.Sources["ds_2"] = notionSourceCursor{Table: "deals"}
		}), w)
		asked := e.took()
		if want := []string{"search data sources", "search trash descending", "query ds_1 since 2026-01-08T00:00:00Z whole"}; !slices.Equal(asked, want) {
			t.Fatalf("requests = %q\nwant %q", asked, want)
		}
		if !slices.Equal(w.resets, []string{"pages", "deals"}) || !slices.Equal(w.log, []string{"delete customer_accounts row_3", "upsert customer_accounts row_1"}) ||
			len(w.offers) != 0 {
			t.Fatalf("resets = %v, changes = %q, offers = %q", w.resets, w.log, w.offers)
		}
	})
	t.Run("reads nothing when every table is reset", func(t *testing.T) {
		e := workspace(t, nil)
		w := newNotionChanges()
		next := changes(t, e, json.RawMessage(`{"since":"2026-01-08T00:00:00Z","tables":[],"sources":{}}`), w)
		if !slices.Equal(w.resets, []string{"pages", "customer_accounts", "deals"}) || !slices.Equal(e.took(), []string{"search data sources"}) ||
			!strings.Contains(string(next), `"tables":["pages","customer_accounts","deals"]`) {
			t.Fatalf("resets = %v, next = %s", w.resets, next)
		}
	})
	// Emptied, a page's body has no document; the one held goes, the row stays.
	t.Run("drops the held document of a page whose body is now empty", func(t *testing.T) {
		e := workspace(t, map[string]notionEdit{"pg_1": {edited: after, properties: title("Roadmap")}})
		e.bodies["pg_1"] = " \n"
		w := newNotionChanges()
		w.held = map[string]bool{"pg_1": true}
		changes(t, e, cursorOf(t, e, nil), w)
		if !slices.Equal(w.drops, []string{"pages pg_1"}) || len(w.puts) != 0 || !slices.Equal(w.log, []string{"upsert pages pg_1"}) {
			t.Fatalf("drops = %v, puts = %v, changes = %q", w.drops, w.putNames(), w.log)
		}
	})
	// A full read would leave it out; the syncer removes its document with the row.
	t.Run("deletes the pages row of a page Notion no longer finds", func(t *testing.T) {
		e := workspace(t, map[string]notionEdit{"pg_1": {edited: after, properties: title("Roadmap")}})
		e.bodies["pg_1"] = ""
		w := newNotionChanges()
		changes(t, e, cursorOf(t, e, nil), w)
		if !slices.Equal(w.log, []string{"delete pages pg_1"}) {
			t.Fatalf("changes = %q", w.log)
		}
	})
	// Newest first, back to the cursor: a page trashed before it was read so by an earlier sync.
	t.Run("deletes the rows of a page trashed since, before reading what changed", func(t *testing.T) {
		e := workspace(t, map[string]notionEdit{
			"row_1": {edited: after, source: "ds_1", properties: notionAccountValues, trashed: true},
			"pg_1":  {edited: after, properties: title("Roadmap"), trashed: true},
			"pg_9":  {edited: before, properties: title("Long gone"), trashed: true},
			"pg_2":  {edited: after, properties: title("New")},
		})
		e.bodies["pg_2"] = "New."
		w := newNotionChanges()
		changes(t, e, cursorOf(t, e, nil), w)
		want := []string{"delete pages pg_1", "delete customer_accounts row_1", "delete pages row_1", "upsert pages pg_2"}
		if !slices.Equal(w.log, want) {
			t.Fatalf("changes = %q\nwant %q", w.log, want)
		}
	})
	t.Run("deletes the pages rows of a data source no longer shared", func(t *testing.T) {
		e := workspace(t, nil)
		w := newNotionChanges()
		changes(t, e, cursorOf(t, e, func(c *notionCursor) { c.Sources["ds_gone"] = notionSourceCursor{Table: "old"} }), w)
		if !slices.Equal(w.log, []string{"cascade pages data_source_id=ds_gone"}) || len(w.resets) != 0 {
			t.Fatalf("changes = %q, resets = %v", w.log, w.resets)
		}
	})
	// Notion's search is eventually consistent: one that leaves out a data
	// source the cursor has must not cost its rows and their documents.
	t.Run("reads a data source the search leaves out by id, and follows it as listed", func(t *testing.T) {
		e := workspace(t, map[string]notionEdit{"row_2": {edited: after, source: "ds_2", properties: title("Bigger deal")}})
		cursor := cursorOf(t, e, nil)
		e.unsearched = map[string]bool{"ds_2": true}
		w := newNotionChanges()
		next := changes(t, e, cursor, w)
		asked := e.took()
		if !slices.Contains(asked, "data source ds_2") || !slices.Contains(asked, "query ds_2 since 2026-01-08T00:00:00Z whole") ||
			len(w.resets) != 0 || !slices.Equal(w.log, []string{"upsert deals row_2", "upsert pages row_2"}) ||
			!strings.Contains(string(next), `"ds_2":{"table":"deals"`) {
			t.Fatalf("requests = %q, resets = %v, changes = %q, next = %s", asked, w.resets, w.log, next)
		}
	})
	for name, answer := range map[string]func(*http.Request) (int, string){
		"answers 403 for": func(*http.Request) (int, string) {
			return http.StatusForbidden, `{"object":"error","status":403,"code":"restricted_resource"}`
		},
		"has in the trash": func(*http.Request) (int, string) {
			return http.StatusOK, strings.Replace(notionTestSource("ds_2", "Deals", "2026-01-02T00:00:00.000Z", deals), `"in_trash":false`, `"in_trash":true`, 1)
		},
	} {
		t.Run("deletes the pages rows of a data source Notion "+name, func(t *testing.T) {
			e := workspace(t, nil)
			cursor := cursorOf(t, e, nil)
			e.unsearched = map[string]bool{"ds_2": true}
			w := newNotionChanges()
			next, err := e.kind(notionRoutes{"/v1/data_sources/ds_2": answer}).Changes(ctx, src, tables, cursor, w)
			if err != nil || !slices.Equal(w.log, []string{"cascade pages data_source_id=ds_2"}) || !slices.Equal(w.resets, []string{"deals"}) ||
				strings.Contains(string(next), `"ds_2"`) {
				t.Fatalf("Changes = %v, changes = %q, resets = %v, next = %s", err, w.log, w.resets, next)
			}
		})
	}
	t.Run("fails the read when Notion won't say whether it still shares a data source", func(t *testing.T) {
		e := workspace(t, nil)
		cursor := cursorOf(t, e, nil)
		e.unsearched = map[string]bool{"ds_2": true}
		w := newNotionChanges()
		_, err := e.kind(notionRoutes{"/v1/data_sources/ds_2": func(*http.Request) (int, string) {
			return http.StatusBadRequest, `{"object":"error","status":400,"code":"validation_error"}`
		}}).Changes(ctx, src, tables, cursor, w)
		notionRefusal(t, err, "Notion returned HTTP 400 for /v1/data_sources/ds_2")
		if len(w.log) != 0 {
			t.Fatalf("changes = %q", w.log)
		}
	})
	// A page joins the search late, out of its place by last edit: the
	// searches reach back notionSearchLag before the cursor and stop only
	// past it. A page listed again unchanged writes nothing new.
	t.Run("reads an edit a lagging search lists late or out of order", func(t *testing.T) {
		lagged := "2026-01-07T23:45:00.000Z"
		e := workspace(t, map[string]notionEdit{
			"pg_1":  {edited: lagged, properties: title("Roadmap")},
			"row_1": {edited: lagged, source: "ds_1", properties: notionAccountValues},
			"pg_9":  {edited: lagged, properties: title("Gone"), trashed: true},
			"pg_10": {edited: after, created: after, properties: title("Late")},
		})
		e.bodies["pg_1"], e.bodies["pg_10"] = "## Q1\n\nShipped.", "Late."
		order := []string{"pg_1", "pg_10"} // pg_10, newer, is listed after pg_1
		kind := e.kind(notionRoutes{"/v1/search": func(r *http.Request) (int, string) {
			body := notionBody(t, r)
			filter, _ := body["filter"].(map[string]any)
			switch {
			case filter["value"] == "data_source":
				return http.StatusOK, notionResults(e.sources["ds_1"], e.sources["ds_2"])
			case filter["in_trash"] == true:
				return http.StatusOK, notionResults(e.pages["pg_9"].item("pg_9"), e.pages["row_1"].item("row_1"))
			}
			var items []string
			for _, id := range append(order, "row_2") {
				items = append(items, e.pages[id].item(id))
			}
			return http.StatusOK, notionResults(items...)
		}})
		w := newNotionChanges()
		if _, err := kind.Changes(ctx, src, tables, cursorOf(t, e, nil), w); err != nil {
			t.Fatal(err)
		}
		// row_1, listed by the trash but not in it, keeps its row.
		want := []string{"delete pages pg_9", "upsert pages pg_1", "upsert pages pg_10"}
		if !slices.Equal(w.log, want) || !slices.Equal(w.putNames(), []string{"pages pg_1", "pages pg_10"}) {
			t.Fatalf("changes = %q, puts = %v\nwant %q", w.log, w.putNames(), want)
		}
	})
	// A page just trashed may still be listed by the search, and one just
	// restored by the trash's: each is left to the search it is in.
	t.Run("leaves out a listed page whose trash isn't the search's", func(t *testing.T) {
		e := workspace(t, map[string]notionEdit{"pg_2": {edited: after, created: after, properties: title("Trashed"), trashed: true},
			"pg_3": {edited: after, properties: title("Restored")}})
		kind := e.kind(notionRoutes{"/v1/search": func(r *http.Request) (int, string) {
			body := notionBody(t, r)
			if filter, _ := body["filter"].(map[string]any); filter["value"] == "data_source" {
				return http.StatusOK, notionResults(e.sources["ds_1"], e.sources["ds_2"])
			}
			// Both searches list both pages, as a lagging index does.
			return http.StatusOK, notionResults(e.pages["pg_2"].item("pg_2"), e.pages["pg_3"].item("pg_3"))
		}})
		w := newNotionChanges()
		if _, err := kind.Changes(ctx, src, tables, cursorOf(t, e, nil), w); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(w.log, []string{"delete pages pg_2", "upsert pages pg_3"}) {
			t.Fatalf("changes = %q", w.log)
		}
	})
	// A page shows 25 ids of a relation; the rest are read from the property.
	t.Run("reads a relation the row cut at 25 whole", func(t *testing.T) {
		shown := make([]string, notionShownReferences)
		for i := range shown {
			shown[i] = fmt.Sprintf(`{"id":"pg_%d"}`, i)
		}
		e := workspace(t, map[string]notionEdit{"row_2": {edited: after, source: "ds_2",
			properties: `{"Projects":{"id":"pr","type":"relation","relation":[` + strings.Join(shown, ",") + `],"has_more":true}}`}})
		e.bodies["row_2"] = "Body."
		kind := e.kind(notionRoutes{"/v1/pages/row_2/properties/pr": func(*http.Request) (int, string) {
			e.ask("references row_2")
			var refs []string
			for i := range 30 {
				refs = append(refs, fmt.Sprintf(`{"object":"property_item","type":"relation","relation":{"id":"pg_%d"}}`, i))
			}
			return http.StatusOK, notionResults(refs...)
		}})
		w := newNotionChanges()
		if _, err := kind.Changes(ctx, src, tables, cursorOf(t, e, nil), w); err != nil {
			t.Fatal(err)
		}
		if projects, _ := w.upserts["deals"][0]["projects"].([]any); len(projects) != 30 || !slices.Contains(e.took(), "references row_2") {
			t.Fatalf("upserts = %v", w.upserts["deals"])
		}
	})
	// A cursor the reader can't decode, or with no time, leaves no point to resume from.
	for _, cursor := range []string{`"2026-01-08"`, `{}`, `{"since":"soon"}`, `{"tables":["pages"]}`, `[]`} {
		t.Run("asks for a full read when the cursor is "+cursor, func(t *testing.T) {
			e := workspace(t, nil)
			if _, err := e.kind(nil).Changes(ctx, src, tables, json.RawMessage(cursor), newNotionChanges()); !errors.Is(err, ErrCursorExpired) {
				t.Fatalf("Changes error = %v, want ErrCursorExpired", err)
			}
			if asked := e.took(); len(asked) != 0 {
				t.Fatalf("requests = %q", asked)
			}
		})
	}
	// A spool that fails must fail the read at the first change it refuses.
	t.Run("stops at the writer's error", func(t *testing.T) {
		full := errors.New("disk full")
		for name, edits := range map[string]map[string]notionEdit{
			"an upsert": {"row_1": {edited: after, source: "ds_1", properties: notionAccountValues}},
			"a page":    {"pg_1": {edited: after, properties: title("Roadmap")}},
			"a delete":  {"row_1": {edited: after, source: "ds_1", properties: notionAccountValues, trashed: true}},
		} {
			e := workspace(t, edits)
			w := newNotionChanges()
			w.fail = full
			if _, err := e.kind(nil).Changes(ctx, src, tables, cursorOf(t, e, nil), w); !errors.Is(err, full) || len(w.log) != 1 {
				t.Fatalf("%s: Changes error = %v after %q, want the writer's at once", name, err, w.log)
			}
		}
		e := workspace(t, nil)
		w := newNotionChanges()
		w.fail = full
		if _, err := e.kind(nil).Changes(ctx, src, tables, cursorOf(t, e, func(c *notionCursor) { c.Sources["ds_gone"] = notionSourceCursor{} }), w); !errors.Is(err, full) {
			t.Fatalf("cascade: Changes error = %v, want the writer's", err)
		}
		e = workspace(t, map[string]notionEdit{"pg_1": {edited: after, properties: title("Roadmap")}})
		docs := newNotionChanges()
		docs.granolaDocs.fail = full
		if _, err := e.kind(nil).Changes(ctx, src, tables, cursorOf(t, e, nil), docs); !errors.Is(err, full) {
			t.Fatalf("Put: Changes error = %v, want Put's", err)
		}
	})
	// A refusal mid-read must fail the read, not move the cursor past changes it never wrote.
	for name, path := range map[string]string{"its search": "/v1/search", "a query": "/v1/data_sources/ds_1/query", "a document": "/v1/pages/row_1/markdown"} {
		t.Run("fails the read when Notion refuses "+name, func(t *testing.T) {
			e := workspace(t, map[string]notionEdit{"row_1": {edited: after, source: "ds_1", properties: notionAccountValues}})
			cursor := cursorOf(t, e, nil)
			unauthorized := func(*http.Request) (int, string) { return http.StatusUnauthorized, `{"code":"unauthorized"}` }
			next, err := e.kind(notionRoutes{path: unauthorized}).Changes(ctx, src, tables, cursor, newNotionChanges())
			if next != nil {
				t.Fatalf("next = %s", next)
			}
			notionRefusal(t, err, "Notion rejected this secret; check it and enter it again")
		})
	}
	for name, edit := range map[string]notionEdit{
		"a trashed page":  {edited: after, properties: title("Gone"), trashed: true},
		"a page":          {edited: after, properties: title("New")},
		"a data source's": {edited: after, source: "ds_1", properties: notionAccountValues},
	} {
		t.Run("fails "+name+" listed without its times", func(t *testing.T) {
			e := workspace(t, map[string]notionEdit{"pg_x": edit})
			e.bodies["pg_x"] = "x"
			cursor := cursorOf(t, e, nil)
			e.pages["pg_x"] = notionEdit{edited: "soon", source: edit.source, properties: edit.properties, trashed: edit.trashed}
			if _, err := e.kind(nil).Changes(ctx, src, tables, cursor, newNotionChanges()); err == nil ||
				!strings.Contains(err.Error(), "page pg_x is listed without its created_time and last_edited_time") {
				t.Fatalf("Changes error = %v", err)
			}
		})
	}
}

// TestNotionSchema checks what a data source's schema fingerprint holds:
// what its table's columns and their values derive from, and nothing else.
func TestNotionSchema(t *testing.T) {
	schemaOf := func(t *testing.T, properties string) string {
		t.Helper()
		kind := notionAt(t, notionRoutes{"/v1/search": notionSearchOf(t, nil, []string{notionTestSource("ds_1", "Accounts", "2026-01-01T00:00:00.000Z", properties)})})
		listed, err := kind.client(notionTestSecret).listSources(context.Background())
		if err != nil || len(listed) != 1 {
			t.Fatalf("sources = %v, %v", listed, err)
		}
		return notionSources(listed)[0].schema
	}
	base := `{"Name":{"id":"title","type":"title","title":{}},"Plan":{"id":"pl","type":"select","select":{"options":[{"name":"Gold"}]}},
		"Score":{"id":"sc","type":"formula","formula":{"expression":"1"}},"Ticket":{"id":"tk","type":"unique_id","unique_id":{"prefix":"T"}}}`
	was := schemaOf(t, base)
	for name, properties := range map[string]string{
		"an option added":                strings.Replace(base, `[{"name":"Gold"}]`, `[{"name":"Gold"},{"name":"Silver"}]`, 1),
		"a property renamed to its slug": strings.Replace(base, `"Plan":`, `"PLAN":`, 1),
		"a property Helix has no column": strings.Replace(base, `"Name":`, `"Send":{"id":"bt","type":"button","button":{}},"Name":`, 1),
	} {
		if got := schemaOf(t, properties); got != was {
			t.Errorf("%s: schema moved from %s to %s", name, was, got)
		}
	}
	for name, properties := range map[string]string{
		"a property added":            strings.Replace(base, `"Name":`, `"Notes":{"id":"nt","type":"rich_text","rich_text":{}},"Name":`, 1),
		"a property removed":          strings.Replace(base, `"Plan":{"id":"pl","type":"select","select":{"options":[{"name":"Gold"}]}},`, ``, 1),
		"a column renamed":            strings.Replace(base, `"Plan":`, `"Tier":`, 1),
		"a type changed":              strings.Replace(base, `"type":"select","select"`, `"type":"status","status"`, 1),
		"a property id changed":       strings.Replace(base, `"id":"pl"`, `"id":"p2"`, 1),
		"a formula changed":           strings.Replace(base, `"expression":"1"`, `"expression":"2"`, 1),
		"a unique id's prefix change": strings.Replace(base, `"prefix":"T"`, `"prefix":"TASK"`, 1),
	} {
		if got := schemaOf(t, properties); got == was {
			t.Errorf("%s: schema stayed %s", name, got)
		}
	}
}
