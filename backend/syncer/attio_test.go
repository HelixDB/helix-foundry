package syncer

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// attioWorkspace is an Attio workspace a test edits between syncs: its
// people by record id, and its notes by id. It counts every request by
// method and path. A workspace with deals has a Deals object whose records
// Attio refuses the token.
type attioWorkspace struct {
	mu     sync.Mutex
	people map[string]string
	notes  map[string]attioFakeNote
	asked  map[string]int
	deals  bool
}

type attioFakeNote struct{ title, content, created string }

func (a *attioWorkspace) serve(t *testing.T) string {
	t.Helper()
	data := func(items []string) string { return `{"data":[` + strings.Join(items, ",") + `]}` }
	// paged answers one page of items: a query's limit and offset from its
	// JSON body, a GET's from its URL.
	paged := func(r *http.Request, items []string) string {
		var limit, offset int
		if r.Method == http.MethodPost {
			var body struct{ Limit, Offset int }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("%s body: %v", r.URL.Path, err)
			}
			limit, offset = body.Limit, body.Offset
		} else {
			limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
			offset, _ = strconv.Atoi(r.URL.Query().Get("offset"))
		}
		return data(items[min(offset, len(items)):min(offset+limit, len(items))])
	}
	attribute := func(slug, kind string) string {
		return fmt.Sprintf(`{"api_slug":%q,"type":%q,"is_multiselect":false,"is_archived":false}`, slug, kind)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.asked[r.Method+" "+r.URL.Path]++
		if r.Header.Get("Authorization") != "Bearer sk_test_abc" {
			t.Errorf("%s: Authorization = %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body string
		switch r.URL.Path {
		case "/v2/self":
			body = `{"active":true,"scope":"user_management:read object_configuration:read record_permission:read list_configuration:read ` +
				`list_entry:read note:read task:read","workspace_slug":"acme"}`
		case "/v2/objects":
			objects := []string{`{"id":{"object_id":"obj_people"},"api_slug":"people","singular_noun":"Person","plural_noun":"People",` +
				`"created_at":"2024-01-01T00:00:00.000000000Z"}`}
			if a.deals {
				objects = append(objects, `{"id":{"object_id":"obj_deals"},"api_slug":"deals","created_at":"2024-01-01T00:00:00.000000000Z"}`)
			}
			body = data(objects)
		case "/v2/objects/obj_deals/attributes":
			body = paged(r, []string{attribute("name", "text")})
		case "/v2/objects/obj_deals/records/query":
			w.WriteHeader(http.StatusForbidden)
			body = `{"status_code":403,"type":"auth_error","code":"unauthorized","message":"Missing permission to read Deals."}`
		case "/v2/objects/obj_people/attributes":
			body = paged(r, []string{attribute("name", "personal-name")})
		case "/v2/objects/obj_people/records/query", "/v2/objects/people/records/query":
			var records []string
			for _, id := range slices.Sorted(maps.Keys(a.people)) {
				first, last, _ := strings.Cut(a.people[id], " ")
				records = append(records, fmt.Sprintf(`{"id":{"record_id":%q},"created_at":"2024-02-01T00:00:00.000000000Z",
					"web_url":"https://app.attio.com/acme/person/%s","values":{"name":[{"active_until":null,"first_name":%q,
					"last_name":%q,"full_name":%q}]}}`, id, id, first, last, a.people[id]))
			}
			body = paged(r, records)
		case "/v2/lists":
			body = data([]string{`{"id":{"list_id":"lst_sales"},"api_slug":"sales_pipeline","name":"Sales pipeline",` +
				`"parent_object":["people"],"created_at":"2024-01-02T00:00:00.000000000Z"}`})
		case "/v2/lists/lst_sales/attributes":
			body = paged(r, []string{attribute("stage", "status")})
		case "/v2/lists/lst_sales/entries/query":
			body = paged(r, []string{`{"id":{"entry_id":"ent_1"},"parent_record_id":"rec_ada","parent_object":"people",` +
				`"created_at":"2024-03-01T00:00:00.000000000Z","entry_values":{"stage":[{"active_until":null,"status":{"title":"Won"}}]}}`})
		case "/v2/workspace_members":
			body = data([]string{`{"id":{"workspace_member_id":"wm_ada"},"first_name":"Ada","last_name":"Lovelace",` +
				`"email_address":"ada@acme.com","access_level":"admin","created_at":"2024-01-01T00:00:00.000000000Z"}`})
		case "/v2/notes":
			var notes []string
			for _, id := range slices.Sorted(maps.Keys(a.notes)) {
				n := a.notes[id]
				notes = append(notes, fmt.Sprintf(`{"id":{"note_id":%q},"parent_object":"people","parent_record_id":"rec_ada",
					"title":%q,"meeting_id":null,"content_markdown":%q,"created_by_actor":{"type":"workspace-member","id":"wm_ada"},
					"created_at":%q}`, id, n.title, n.content, n.created))
			}
			body = paged(r, notes)
		case "/v2/tasks":
			body = paged(r, []string{`{"id":{"task_id":"tsk_1"},"content_plaintext":"Call Ada","deadline_at":null,"is_completed":false,
				"completed_at":null,"linked_records":[{"target_object_id":"obj_people","target_record_id":"rec_ada"}],
				"assignees":[{"referenced_actor_type":"workspace-member","referenced_actor_id":"wm_ada"}],
				"created_by_actor":{"type":"workspace-member","id":"wm_ada"},"created_at":"2024-03-02T00:00:00.000000000Z"}`})
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// took are the requests since the last call, by method and path.
func (a *attioWorkspace) took() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	asked := a.asked
	a.asked = map[string]int{}
	return asked
}

var attioTableNames = []string{"list_sales_pipeline", "lists", "notes", "objects", "people", "task_assignees", "task_records", "tasks",
	"workspace_members"}

// TestAttioSyncs runs Attio through the syncer on Free, which includes the
// kind and reads every table in full on every sync: the first sync writes
// every table and a document per note in the Notes set; after a note is
// edited, another deleted and a person added, the next writes the edit,
// removes the deleted note's document and keeps the unchanged one's. Each
// sync lists the workspace once, in its discovery, and its read reuses it.
func TestAttioSyncs(t *testing.T) {
	ws := &attioWorkspace{asked: map[string]int{}, people: map[string]string{"rec_ada": "Ada Lovelace"}, notes: map[string]attioFakeNote{
		"not_1": {"Kickoff", "Agenda", "2026-10-01T09:00:00.000000000Z"},
		"not_2": {"Renewal", "## Terms\n\nTwo years.", "2026-10-02T09:00:00.000000000Z"},
		"not_3": {"Pricing", "Gold plan.", "2026-10-03T09:00:00.000000000Z"},
	}}
	f := newFixture(t, "http://unused", "attio", `{}`, plan{code: "free"})
	endpoints := connector.DefaultEndpoints()
	endpoints.Attio = ws.serve(t)
	f.syncer.Catalog = connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{}, connector.EventsOptions{})

	res := f.run(t, "sync-1")
	if res.Mode != model.ConnectorSyncFull || !slices.Equal(slices.Sorted(maps.Keys(f.store.tables)), attioTableNames) {
		t.Fatalf("mode = %s, tables = %v", res.Mode, slices.Sorted(maps.Keys(f.store.tables)))
	}
	// Discovery asks People for a row, and the read reads it; the notes'
	// links read one person.
	if took := ws.took(); took["GET /v2/self"] != 1 || took["GET /v2/notes"] != 1 || took["GET /v2/tasks"] != 1 ||
		took["POST /v2/objects/obj_people/records/query"] != 2 || took["POST /v2/objects/people/records/query"] != 1 {
		t.Fatalf("requests = %v", took)
	}
	notes := f.held(t, "notes")
	if len(notes) != 3 || notes["not_1"] != "# Kickoff\n\nAgenda" || notes["not_2"] != "# Renewal\n\n## Terms\n\nTwo years." {
		t.Fatalf("notes = %q", notes)
	}
	for _, d := range f.store.live(f.store.setOf("notes").ID) {
		if d.Source.URL == nil || *d.Source.URL != "https://app.attio.com/acme/person/rec_ada" {
			t.Fatalf("%s links %v", d.Source.ExternalID, d.Source.URL)
		}
	}
	if set := f.store.setOf("notes"); set == nil || set.Name != "Notes" || set.Owner.ConnectorID != "conn-1" {
		t.Fatalf("notes set = %+v", set)
	}
	for table, want := range map[string][]string{
		"people":              {`"name":"Ada Lovelace"`, `"name_first_name":"Ada"`, `"record_id":"rec_ada"`},
		"list_sales_pipeline": {`"entry_id":"ent_1"`, `"stage":"Won"`, `"parent_record_id":"rec_ada"`},
		"notes":               {`"note_id":"not_3"`, `"title":"Pricing"`, `"parent_object":"people"`},
		"task_records":        {`"target_object":"people"`, `"target_record_id":"rec_ada"`},
		"task_assignees":      {`"assignee_id":"wm_ada"`},
		"workspace_members":   {`"workspace_member_id":"wm_ada"`},
		"objects":             {`"api_slug":"people"`},
		"lists":               {`"parent_object":"people"`},
	} {
		rows := strings.Join(f.snapshotRows(t, table), "\n")
		for _, w := range want {
			if !strings.Contains(rows, w) {
				t.Fatalf("%s = %s; want %s", table, rows, w)
			}
		}
	}
	if rows := f.snapshotRows(t, "notes"); len(rows) != 3 || strings.Contains(strings.Join(rows, ""), "Two years") {
		t.Fatalf("notes rows = %v; want three, without their bodies", rows)
	}
	kept := f.liveID("notes", "not_1")

	// Edit one note, delete another and add a person.
	ws.mu.Lock()
	ws.notes["not_2"] = attioFakeNote{"Renewal", "## Terms\n\nThree years.", "2026-10-02T09:00:00.000000000Z"}
	delete(ws.notes, "not_3")
	ws.people["rec_grace"] = "Grace Hopper"
	ws.mu.Unlock()
	res = f.run(t, "sync-2")
	if res.Mode != model.ConnectorSyncFull || !slices.Contains(res.Changed, "notes") || !slices.Contains(res.Changed, "people") ||
		slices.Contains(res.Changed, "tasks") {
		t.Fatalf("mode = %s, changed = %v", res.Mode, res.Changed)
	}
	if took := ws.took(); took["GET /v2/self"] != 1 {
		t.Fatalf("requests = %v", took)
	}
	notes = f.held(t, "notes")
	if len(notes) != 2 || notes["not_2"] != "# Renewal\n\n## Terms\n\nThree years." || notes["not_3"] != "" {
		t.Fatalf("notes = %q", notes)
	}
	if f.liveID("notes", "not_1") != kept {
		t.Fatalf("the unchanged note's document was replaced: %s, was %s", f.liveID("notes", "not_1"), kept)
	}
	if rows := f.snapshotRows(t, "people"); len(rows) != 2 || !strings.Contains(rows[0]+rows[1], `"name":"Grace Hopper"`) {
		t.Fatalf("people = %v", rows)
	}
	if rows := f.snapshotRows(t, "notes"); len(rows) != 2 {
		t.Fatalf("notes rows = %v", rows)
	}
}

// TestAttioSyncsAroundADeniedObject syncs a workspace whose token Attio
// refuses one object's records: its discovery leaves that table out, as the
// connection's test does, and every other table publishes.
func TestAttioSyncsAroundADeniedObject(t *testing.T) {
	ws := &attioWorkspace{asked: map[string]int{}, deals: true, people: map[string]string{"rec_ada": "Ada Lovelace"},
		notes: map[string]attioFakeNote{"not_1": {"Kickoff", "Agenda", "2026-10-01T09:00:00.000000000Z"}}}
	f := newFixture(t, "http://unused", "attio", `{}`, plan{code: "free"})
	endpoints := connector.DefaultEndpoints()
	endpoints.Attio = ws.serve(t)
	f.syncer.Catalog = connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{}, connector.EventsOptions{})

	res := f.run(t, "sync-1")
	if res.Mode != model.ConnectorSyncFull || !slices.Equal(slices.Sorted(maps.Keys(f.store.tables)), attioTableNames) {
		t.Fatalf("mode = %s, tables = %v", res.Mode, slices.Sorted(maps.Keys(f.store.tables)))
	}
	if rows := f.snapshotRows(t, "people"); len(rows) != 1 || len(f.held(t, "notes")) != 1 {
		t.Fatalf("people = %v, notes = %v", rows, f.held(t, "notes"))
	}
	if took := ws.took(); took["POST /v2/objects/obj_deals/records/query"] != 1 {
		t.Fatalf("requests = %v", took)
	}
}
