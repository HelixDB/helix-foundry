package connector

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const attioTestToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// attioAllScopes are every scope the kind reads with; records are held as
// read-write, which reads.
const attioAllScopes = "user_management:read object_configuration:read record_permission:read-write list_configuration:read " +
	"list_entry:read note:read task:read webhook:read"

type attioRoutes = map[string]func(*http.Request) (int, string)

// attioHits counts the requests a test's Attio answered, as "METHOD path".
type attioHits struct {
	mu   sync.Mutex
	hits map[string]int
}

// took is the requests since the last call.
func (h *attioHits) took() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	took := h.hits
	h.hits = map[string]int{}
	return took
}

// attioAt serves routes, checking every request carries the token and, on
// a POST, its JSON type, and returns the attio kind reading from them,
// unpaced and without backoff, and the requests it answers.
func attioAt(t *testing.T, routes attioRoutes) (*attioKind, *attioHits) {
	t.Helper()
	noBackoff(t)
	saved := attioNotesInterval
	attioNotesInterval = 0
	t.Cleanup(func() { attioNotesInterval = saved })
	hits := &attioHits{hits: map[string]int{}}
	checked := attioRoutes{}
	for path, handler := range routes {
		checked[path] = func(r *http.Request) (int, string) {
			if r.Header.Get("Authorization") != "Bearer "+attioTestToken {
				t.Errorf("%s %s: Authorization = %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
			}
			if isJSON := r.Header.Get("Content-Type") == "application/json"; isJSON != (r.Method == http.MethodPost) {
				t.Errorf("%s %s: Content-Type = %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
			}
			hits.mu.Lock()
			hits.hits[r.Method+" "+r.URL.Path]++
			hits.mu.Unlock()
			return handler(r)
		}
	}
	url := routeServer(t, checked)
	return kindOf(t, NewCatalog(Endpoints{Attio: url}, Guard{}, S3Options{}, EventsOptions{}), "attio").(*attioKind), hits
}

// attioData is a list's answer.
func attioData(items ...string) string { return `{"data":[` + strings.Join(items, ",") + `]}` }

// attioPaged answers a paged list from items: a query's limit and offset
// from its JSON body, a GET's from its URL.
func attioPaged(t *testing.T, items []string) func(*http.Request) (int, string) {
	return func(r *http.Request) (int, string) {
		var limit, offset int
		if r.Method == http.MethodPost {
			var body map[string]int
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("%s body: %v", r.URL.Path, err)
			}
			limit, offset = body["limit"], body["offset"]
		} else {
			limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
			offset, _ = strconv.Atoi(r.URL.Query().Get("offset"))
		}
		if limit < 1 {
			t.Errorf("%s %s: no limit", r.Method, r.URL)
		}
		return http.StatusOK, attioData(items[min(offset, len(items)):min(offset+limit, len(items))]...)
	}
}

// attioNullable is s as JSON, null when empty.
func attioNullable(s string) string {
	if s == "" {
		return "null"
	}
	return strconv.Quote(s)
}

func attioSelfJSON(scope string) string {
	return fmt.Sprintf(`{"active":true,"scope":%q,"client_id":"tok","token_type":"Bearer","exp":null,"iat":1700000000,
		"iss":"attio.com","token_level":"workspace","workspace_id":"ws_1","workspace_name":"Acme","workspace_slug":"acme"}`, scope)
}

func attioObjectJSON(id, slug string) string {
	return fmt.Sprintf(`{"id":{"workspace_id":"ws_1","object_id":%q},"api_slug":%s,"singular_noun":"Thing","plural_noun":"Things",
		"created_at":"2024-01-01T00:00:00.000000000Z"}`, id, attioNullable(slug))
}

func attioAttributeJSON(slug, kind string, multi, archived bool) string {
	return fmt.Sprintf(`{"id":{"workspace_id":"ws_1","object_id":"o","attribute_id":"a_%s"},"title":%q,"description":null,
		"api_slug":%q,"type":%q,"is_system_attribute":false,"is_writable":true,"is_required":false,"is_unique":false,
		"is_multiselect":%t,"is_default_value_enabled":false,"is_archived":%t,"default_value":null,"relationship":null,
		"created_at":"2024-01-01T00:00:00.000000000Z","config":{}}`, slug, slug, slug, kind, multi, archived)
}

func attioListJSON(id, slug string, parents ...string) string {
	quoted := make([]string, len(parents))
	for i, p := range parents {
		quoted[i] = strconv.Quote(p)
	}
	return fmt.Sprintf(`{"id":{"workspace_id":"ws_1","list_id":%q},"api_slug":%s,"name":"Sales pipeline","parent_object":[%s],
		"workspace_access":"full-access","workspace_member_access":[],"created_by_actor":{"type":"workspace-member","id":"wm_ada"},
		"created_at":"2024-01-02T00:00:00.000000000Z"}`, id, attioNullable(slug), strings.Join(quoted, ","))
}

func attioRecordJSON(id, values string) string {
	return fmt.Sprintf(`{"id":{"workspace_id":"ws_1","object_id":"o","record_id":%q},"created_at":"2024-02-01T09:00:00.000000000Z",
		"web_url":"https://app.attio.com/acme/person/%s","values":%s}`, id, id, values)
}

func attioNoteJSON(id, title, content, created, parentObject, parentRecord string) string {
	return fmt.Sprintf(`{"id":{"workspace_id":"ws_1","note_id":%q},"parent_object":%s,"parent_record_id":%s,"title":%q,
		"meeting_id":null,"content_plaintext":"","content_markdown":%q,"tags":[],
		"created_by_actor":{"type":"workspace-member","id":"wm_ada"},"created_at":%q}`,
		id, attioNullable(parentObject), attioNullable(parentRecord), title, content, created)
}

// attioValueJSON is one active value of an attribute of type kind with fields.
func attioValueJSON(kind, fields string) string {
	return fmt.Sprintf(`{"active_from":"2024-02-01T09:00:00.000000000Z","active_until":null,
		"created_by_actor":{"type":"system","id":null},"attribute_type":%q,%s}`, kind, fields)
}

var (
	attioPeopleAttributes = []string{
		attioAttributeJSON("name", "personal-name", false, false),
		attioAttributeJSON("email_addresses", "email-address", true, false),
		attioAttributeJSON("company", "record-reference", false, false),
		attioAttributeJSON("old_notes", "text", false, true),
		attioAttributeJSON("created_at", "timestamp", false, false),
	}
	attioAda = attioRecordJSON("rec_ada", `{
		"name":[`+attioValueJSON("personal-name", `"first_name":"Ada","last_name":"Lovelace","full_name":"Ada Lovelace"`)+`],
		"email_addresses":[`+attioValueJSON("email-address", `"email_address":"ada@acme.com","email_domain":"acme.com"`)+`,`+
		attioValueJSON("email-address", `"email_address":"ada@example.com"`)+`],
		"company":[`+attioValueJSON("record-reference", `"target_object":"companies","target_record_id":"rec_acme"`)+`],
		"created_at":[`+attioValueJSON("timestamp", `"value":"2024-02-01T09:00:00.000000000Z"`)+`]}`)
	attioGrace = attioRecordJSON("rec_grace", `{"name":[],"email_addresses":[]}`)
	attioAcme  = attioRecordJSON("rec_acme", `{"name":[`+attioValueJSON("text", `"value":"Acme"`)+`],
		"domains":[`+attioValueJSON("domain", `"domain":"acme.com","root_domain":"acme.com"`)+`],
		"stage":[`+attioValueJSON("status", `"status":{"id":{"status_id":"s1"},"title":"Customer","is_archived":false}`)+`]}`)
	attioEntry = `{"id":{"workspace_id":"ws_1","list_id":"lst_sales","entry_id":"ent_1"},"parent_record_id":"rec_acme",
		"parent_object":"companies","created_at":"2024-03-01T00:00:00.000000000Z","entry_values":{
		"stage":[` + attioValueJSON("status", `"status":{"title":"Won"}`) + `],
		"value":[` + attioValueJSON("currency", `"currency_value":1200.5,"currency_code":"GBP"`) + `]}}`
	attioNotes = []string{
		attioNoteJSON("not_old", "Kickoff", "Agenda", "2024-03-01T10:00:00.000000000Z", "people", "rec_ada"),
		attioNoteJSON("not_new", "  Renewal   call ", "## Terms\n\nTwo years.", "2024-05-01T10:00:00.123456789Z", "companies", "rec_acme"),
		attioNoteJSON("not_tie_b", "Tie B", "b", "2024-04-15T10:00:00.000000000Z", "people", "rec_ada"),
		attioNoteJSON("not_empty", "Blank", " \n ", "2024-04-01T10:00:00.000000000Z", "people", "rec_ada"),
		attioNoteJSON("not_tie_a", "Tie A", "a", "2024-04-15T10:00:00.000000000Z", "people", "rec_ada"),
		attioNoteJSON("not_untitled", "", "Just text", "2024-02-01T10:00:00.000000000Z", "", ""),
	}
	attioTasks = []string{
		`{"id":{"workspace_id":"ws_1","task_id":"tsk_1"},"content_plaintext":"Call Ada","format":"plaintext","deadline_at":null,
			"is_completed":false,"completed_at":null,
			"linked_records":[{"target_object_id":"obj_people","target_record_id":"rec_ada"},
				{"target_object_id":"obj_people","target_record_id":"rec_ada"},{"target_object_id":"obj_gone","target_record_id":"rec_x"}],
			"assignees":[{"referenced_actor_type":"workspace-member","referenced_actor_id":"wm_ada"},
				{"referenced_actor_type":"workspace-member","referenced_actor_id":"wm_ada"},
				{"referenced_actor_type":"workspace-member","referenced_actor_id":null}],
			"created_by_actor":{"type":"workspace-member","id":"wm_alan"},"created_at":"2024-03-02T00:00:00.000000000Z"}`,
		`{"id":{"workspace_id":"ws_1","task_id":"tsk_2"},"content_plaintext":"Send terms","deadline_at":"2024-06-01T00:00:00.000000000Z",
			"is_completed":true,"completed_at":"2024-05-20T00:00:00.000000000Z","linked_records":[],"assignees":[],
			"created_by_actor":{"type":"api-token","id":"tok"},"created_at":"2024-03-03T00:00:00.000000000Z"}`,
	}
)

// attioWorkspace is a workspace whose token holds scope: People (with an
// archived attribute and one whose column a fixed one holds), Companies and
// an object without a slug; a Sales pipeline list of companies; two
// members; notes; and tasks. extra adds routes or replaces them.
func attioWorkspace(t *testing.T, scope string, extra attioRoutes) attioRoutes {
	routes := attioRoutes{
		"/v2/self": func(*http.Request) (int, string) { return http.StatusOK, attioSelfJSON(scope) },
		"/v2/objects": func(*http.Request) (int, string) {
			return http.StatusOK, attioData(attioObjectJSON("obj_people", "people"), attioObjectJSON("obj_companies", "companies"),
				attioObjectJSON("obj_noslug", ""))
		},
		"/v2/objects/obj_people/attributes": attioPaged(t, attioPeopleAttributes),
		"/v2/objects/obj_companies/attributes": attioPaged(t, []string{attioAttributeJSON("name", "text", false, false),
			attioAttributeJSON("domains", "domain", true, false), attioAttributeJSON("stage", "status", false, false)}),
		"/v2/objects/obj_people/records/query":    attioPaged(t, []string{attioAda, attioGrace}),
		"/v2/objects/obj_companies/records/query": attioPaged(t, []string{attioAcme}),
		"/v2/lists": func(*http.Request) (int, string) {
			return http.StatusOK, attioData(attioListJSON("lst_sales", "sales_pipeline", "companies"), attioListJSON("lst_both", "", "people", "companies"))
		},
		"/v2/lists/lst_sales/attributes": attioPaged(t, []string{attioAttributeJSON("stage", "status", false, false),
			attioAttributeJSON("value", "currency", false, false)}),
		"/v2/lists/lst_sales/entries/query": attioPaged(t, []string{attioEntry}),
		"/v2/workspace_members": func(*http.Request) (int, string) {
			return http.StatusOK, attioData(
				`{"id":{"workspace_id":"ws_1","workspace_member_id":"wm_ada"},"first_name":"Ada","last_name":"Lovelace","avatar_url":null,
					"email_address":"ada@acme.com","created_at":"2024-01-01T00:00:00.000000000Z","access_level":"admin"}`,
				`{"id":{"workspace_id":"ws_1","workspace_member_id":"wm_alan"},"first_name":"Alan","last_name":null,
					"email_address":"alan@acme.com","created_at":"2024-01-03T00:00:00.000000000Z","access_level":"member"}`)
		},
		"/v2/notes": attioPaged(t, attioNotes),
		"/v2/tasks": attioPaged(t, attioTasks),
	}
	maps.Copy(routes, extra)
	return routes
}

func attioTestSource(forSync bool) Source {
	return Source{Settings: json.RawMessage(`{}`), Secret: attioTestToken, ForSync: forSync}
}

// attioRows are rows as JSON, keys sorted.
func attioRows(rows []map[string]any) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		b, _ := json.Marshal(row)
		out[i] = string(b)
	}
	return out
}

func columnNames(t Table) []string {
	var names []string
	for _, c := range t.Columns {
		names = append(names, c.Name)
	}
	return names
}

func TestAttioPrepare(t *testing.T) {
	k := kindOf(t, catalogFor(""), "attio")
	const message = "enter an Attio access token (64 characters)"
	for _, c := range []struct{ name, secret, settings, err string }{
		{"a token", attioTestToken, ``, ""},
		{"spaces around it", " \n" + attioTestToken + "\t ", `null`, ""},
		{"any characters", strings.Repeat("aZ-_.~9", 9) + "x", `{}`, ""},
		{"no token", "", `{}`, message},
		{"one short", attioTestToken[1:], `{}`, message},
		{"one long", attioTestToken + "0", `{}`, message},
		{"a space inside", attioTestToken[:30] + " " + attioTestToken[31:], `{}`, message},
		{"a tab inside", attioTestToken[:30] + "\t" + attioTestToken[31:], `{}`, message},
		{"a control character", attioTestToken[:63] + "\x00", `{}`, message},
		{"a setting", attioTestToken, `{"objects":["people"]}`, "this connector takes no settings besides its key"},
		{"settings not an object", attioTestToken, `[]`, "this connector takes no settings besides its key"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := k.Prepare(Source{Secret: c.secret, Settings: json.RawMessage(c.settings)})
			if c.err != "" {
				notionInvalid(t, err, c.err)
				return
			}
			if err != nil || string(got) != `{}` {
				t.Fatalf("Prepare = %s, %v", got, err)
			}
		})
	}
	if hint := k.Hint(" " + attioTestToken + "\n"); hint != "…cdef" {
		t.Fatalf("hint = %q", hint)
	}
	if hint := k.Hint("short"); hint != "…" {
		t.Fatalf("hint of a short token = %q", hint)
	}
	if k.Name() != "attio" || k.DisplayName() != "Attio" {
		t.Fatalf("name = %q, display name = %q", k.Name(), k.DisplayName())
	}
	if streams := k.(DocumentKind).Streams(Source{}); len(streams) != 1 || streams[0].Name != "notes" || streams[0].Table != "notes" ||
		streams[0].Title != "Notes" || streams[0].Format != Markdown || streams[0].Keep.Newest() != 0 || streams[0].Problem() != "" {
		t.Fatalf("streams = %+v", streams)
	}
}

func TestAttioDiscover(t *testing.T) {
	ctx := context.Background()
	everyTable := []string{"workspace_members", "objects", "people", "companies", "lists", "list_sales_pipeline",
		"notes", "tasks", "task_assignees", "task_records"}

	t.Run("lists every table the token's scopes read, with a preview of each object and list", func(t *testing.T) {
		k, hits := attioAt(t, attioWorkspace(t, attioAllScopes, nil))
		d, err := k.Discover(ctx, attioTestSource(false))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(tableNames(d), everyTable) || len(d.Unavailable) != 0 {
			t.Fatalf("tables = %v, unavailable = %v", tableNames(d), d.Unavailable)
		}
		keys := map[string][]string{"workspace_members": {"workspace_member_id"}, "objects": {"object_id"}, "people": {"record_id"},
			"companies": {"record_id"}, "lists": {"list_id"}, "list_sales_pipeline": {"entry_id"}, "notes": {"note_id"},
			"tasks": {"task_id"}, "task_assignees": {"task_id", "assignee_type", "assignee_id"},
			"task_records": {"task_id", "target_object", "target_record_id"}}
		for name, key := range keys {
			table := findTable(t, d, name)
			if !slices.Equal(table.PrimaryKey, key) || !table.Dense || table.Windowed || len(table.Columns) == 0 {
				t.Errorf("%s: key = %v, dense = %v, windowed = %v, columns = %v", name, table.PrimaryKey, table.Dense, table.Windowed, table.Columns)
			}
			wantFKs := 0
			if name == "task_assignees" || name == "task_records" {
				wantFKs = 1
			}
			if len(table.ForeignKeys) != wantFKs || wantFKs == 1 && table.ForeignKeys[0] != foreignKey("task_id", "tasks", "task_id") {
				t.Errorf("%s foreign keys = %v", name, table.ForeignKeys)
			}
			if preview := name == "people" || name == "companies" || name == "list_sales_pipeline"; preview != (len(table.Preview) > 0) {
				t.Errorf("%s preview = %s", name, table.Preview)
			}
		}
		// The archived attribute has no column, a single value's fields
		// have siblings, and created_at, which a fixed column holds, is numbered.
		people := findTable(t, d, "people")
		want := []string{"record_id", "created_at", "web_url", "name", "name_first_name", "name_last_name", "email_addresses",
			"company", "company_object", "created_at_2"}
		if !slices.Equal(columnNames(people), want) {
			t.Fatalf("people columns = %v", columnNames(people))
		}
		types := map[string]Column{}
		for _, c := range people.Columns {
			types[c.Name] = c
		}
		if types["email_addresses"] != (Column{Name: "email_addresses", Type: "array"}) ||
			types["name"] != (Column{Name: "name", Type: "string", Nullable: true}) ||
			types["company_object"] != (Column{Name: "company_object", Type: "string", Nullable: true}) ||
			types["record_id"] != (Column{Name: "record_id", Type: "string"}) {
			t.Fatalf("people column types = %v", people.Columns)
		}
		if list := findTable(t, d, "list_sales_pipeline"); !slices.Equal(columnNames(list),
			[]string{"entry_id", "parent_object", "parent_record_id", "created_at", "stage", "value", "value_currency"}) {
			t.Fatalf("list columns = %v", columnNames(list))
		}
		if len(people.Preview) != 2 || !strings.Contains(string(people.Preview[0]), `"name":"Ada Lovelace"`) {
			t.Fatalf("people preview = %s", people.Preview)
		}
		// One request a list, attributes and preview; no list of the
		// object without a slug, or of the list without one.
		took := hits.took()
		for path, n := range took {
			if n != 1 {
				t.Errorf("%s asked %d times", path, n)
			}
		}
		for _, path := range []string{"GET /v2/self", "GET /v2/objects", "GET /v2/objects/obj_people/attributes",
			"POST /v2/objects/obj_people/records/query", "GET /v2/lists/lst_sales/attributes", "POST /v2/lists/lst_sales/entries/query"} {
			if took[path] != 1 {
				t.Errorf("%s asked %d times", path, took[path])
			}
		}
		if len(took) != 9 {
			t.Fatalf("requests = %v", took)
		}
	})

	t.Run("a sync lists what it reads without previews", func(t *testing.T) {
		k, hits := attioAt(t, attioWorkspace(t, attioAllScopes, nil))
		d, err := k.Discover(ctx, attioTestSource(true))
		if err != nil || !slices.Equal(tableNames(d), everyTable) {
			t.Fatalf("Discover = %v, %v", tableNames(d), err)
		}
		for path := range hits.took() {
			if strings.HasSuffix(path, "/query") {
				t.Fatalf("a sync's discovery asked %s", path)
			}
		}
	})

	for _, c := range []struct {
		name, scope string
		unavailable []string
		asked       func(map[string]int) bool
	}{
		{"without notes and tasks", strings.ReplaceAll(strings.ReplaceAll(attioAllScopes, "note:read", ""), "task:read", ""), []string{
			"notes: the token lacks the note:read scope",
			"tasks: the token lacks the task:read scope",
			"task_assignees: the token lacks the task:read scope",
			"task_records: the token lacks the task:read scope",
		}, nil},
		{"without records, whose attributes are not read", strings.ReplaceAll(attioAllScopes, "record_permission:read-write", ""), []string{
			"people: the token lacks the record_permission:read scope",
			"companies: the token lacks the record_permission:read scope",
			"notes: the token lacks the record_permission:read scope",
			"tasks: the token lacks the record_permission:read scope",
			"task_assignees: the token lacks the record_permission:read scope",
			"task_records: the token lacks the record_permission:read scope",
		}, func(took map[string]int) bool {
			return took["GET /v2/objects/obj_people/attributes"] == 0 && took["GET /v2/objects"] == 1
		}},
		{"without object configuration, whose objects are not listed", strings.ReplaceAll(attioAllScopes, "object_configuration:read", ""), []string{
			"objects: the token lacks the object_configuration:read scope, so no object's records can be read either",
			"notes: the token lacks the object_configuration:read scope",
			"tasks: the token lacks the object_configuration:read scope",
			"task_assignees: the token lacks the object_configuration:read scope",
			"task_records: the token lacks the object_configuration:read scope",
		}, func(took map[string]int) bool { return took["GET /v2/objects"] == 0 }},
		{"without lists", strings.ReplaceAll(attioAllScopes, "list_configuration:read", ""), []string{
			"lists: the token lacks the list_configuration:read scope, so no list's entries can be read either",
		}, func(took map[string]int) bool { return took["GET /v2/lists"] == 0 }},
		{"without list entries", strings.ReplaceAll(attioAllScopes, "list_entry:read", ""), []string{
			"list_sales_pipeline: the token lacks the list_entry:read scope",
		}, func(took map[string]int) bool {
			return took["GET /v2/lists"] == 1 && took["GET /v2/lists/lst_sales/attributes"] == 0
		}},
		{"without members", strings.ReplaceAll(attioAllScopes, "user_management:read", ""), []string{
			"workspace_members: the token lacks the user_management:read scope",
			"tasks: the token lacks the user_management:read scope",
			"task_assignees: the token lacks the user_management:read scope",
			"task_records: the token lacks the user_management:read scope",
		}, nil},
		{"with members and notes alone, naming every scope a table lacks", "user_management:read-write note:read", []string{
			"objects: the token lacks the object_configuration:read scope, so no object's records can be read either",
			"lists: the token lacks the list_configuration:read scope, so no list's entries can be read either",
			"notes: the token lacks the object_configuration:read and record_permission:read scopes",
			"tasks: the token lacks the task:read, object_configuration:read and record_permission:read scopes",
			"task_assignees: the token lacks the task:read, object_configuration:read and record_permission:read scopes",
			"task_records: the token lacks the task:read, object_configuration:read and record_permission:read scopes",
		}, nil},
	} {
		t.Run("names the scopes a table lacks: "+c.name, func(t *testing.T) {
			k, hits := attioAt(t, attioWorkspace(t, c.scope, nil))
			d, err := k.Discover(ctx, attioTestSource(true))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, u := range d.Unavailable {
				got = append(got, u.Name+": "+u.Reason)
			}
			if !slices.Equal(got, c.unavailable) {
				t.Fatalf("unavailable = %q", got)
			}
			if took := hits.took(); c.asked != nil && !c.asked(took) {
				t.Fatalf("requests = %v", took)
			}
		})
	}

	t.Run("reads every table when Attio names no scopes", func(t *testing.T) {
		k, _ := attioAt(t, attioWorkspace(t, "", attioRoutes{
			"/v2/self": func(*http.Request) (int, string) { return http.StatusOK, `{"active":true,"workspace_slug":"acme"}` },
		}))
		d, err := k.Discover(ctx, attioTestSource(true))
		if err != nil || !slices.Equal(tableNames(d), everyTable) || len(d.Unavailable) != 0 {
			t.Fatalf("Discover = %v, %v, %v", tableNames(d), d.Unavailable, err)
		}
	})

	t.Run("refuses a token with no scope it reads with", func(t *testing.T) {
		k, _ := attioAt(t, attioWorkspace(t, "webhook:read", nil))
		_, err := k.Discover(ctx, attioTestSource(false))
		notionRefusal(t, err, "this Attio token has none of the read scopes Helix reads with; give it read access to records, "+
			"lists, notes, tasks or members in Attio, then test again")
	})

	for _, c := range []struct {
		name   string
		status int
		body   string
	}{
		{"an inactive token", http.StatusOK, `{"active":false}`},
		{"a 401", http.StatusUnauthorized, `{"status_code":401,"type":"auth_error","code":"unauthorized","message":"Bad token"}`},
	} {
		t.Run("rejects "+c.name, func(t *testing.T) {
			k, _ := attioAt(t, attioRoutes{"/v2/self": func(*http.Request) (int, string) { return c.status, c.body }})
			_, err := k.Discover(ctx, attioTestSource(false))
			notionRefusal(t, err, "Attio rejected this token; check it and enter it again")
			var authErr *AuthError
			if !errors.As(err, &authErr) || authErr.Provider != "Attio" || strings.Contains(err.Error(), attioTestToken) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	t.Run("makes a table Attio refuses the token unavailable with Attio's reason", func(t *testing.T) {
		k, _ := attioAt(t, attioWorkspace(t, attioAllScopes, attioRoutes{
			"/v2/objects/obj_people/records/query": func(*http.Request) (int, string) {
				return http.StatusForbidden, `{"status_code":403,"type":"auth_error","code":"unauthorized","message":"Missing permission to read People."}`
			},
			"/v2/lists/lst_sales/entries/query": func(*http.Request) (int, string) {
				return http.StatusForbidden, `{"status_code":403,"type":"invalid_request_error","code":"billing_error","message":"Upgrade to read lists."}`
			},
		}))
		d, err := k.Discover(ctx, attioTestSource(false))
		if err != nil {
			t.Fatal(err)
		}
		want := []Unavailable{{Name: "people", Reason: "Missing permission to read People."}, {Name: "list_sales_pipeline", Reason: "Upgrade to read lists."}}
		if !slices.Equal(d.Unavailable, want) || slices.Contains(tableNames(d), "people") || !slices.Contains(tableNames(d), "companies") {
			t.Fatalf("tables = %v, unavailable = %v", tableNames(d), d.Unavailable)
		}
	})

	t.Run("refuses any other failure of a preview, so a sync retries it", func(t *testing.T) {
		k, _ := attioAt(t, attioWorkspace(t, attioAllScopes, attioRoutes{
			"/v2/lists/lst_sales/entries/query": func(*http.Request) (int, string) {
				return http.StatusNotFound, `{"status_code":404,"type":"invalid_request_error","code":"not_found","message":"List not found."}`
			},
			"/v2/objects/obj_companies/records/query": func(*http.Request) (int, string) {
				return http.StatusForbidden, `{"status_code":403,"type":"invalid_request_error","code":"validation_type","message":"No."}`
			},
		}))
		_, err := k.Discover(ctx, attioTestSource(false))
		notionRefusal(t, err, "Attio returned HTTP 403 for /v2/objects/obj_companies/records/query: No.")
	})

	t.Run("retries a rate limit until the date Attio names", func(t *testing.T) {
		calls := 0
		k, _ := attioAt(t, attioWorkspace(t, attioAllScopes, attioRoutes{
			"/v2/self": func(*http.Request) (int, string) {
				if calls++; calls == 1 {
					return http.StatusTooManyRequests, `{"status_code":429,"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Slow down"}`
				}
				return http.StatusOK, attioSelfJSON(attioAllScopes)
			},
		}))
		if d, err := k.Discover(ctx, attioTestSource(true)); err != nil || calls != 2 || !slices.Equal(tableNames(d), everyTable) {
			t.Fatalf("Discover = %v, %v after %d calls", tableNames(d), err, calls)
		}
	})

	t.Run("a rate limit that outlasts the retries is refused", func(t *testing.T) {
		k, hits := attioAt(t, attioRoutes{"/v2/self": func(*http.Request) (int, string) {
			return http.StatusTooManyRequests, `{"status_code":429,"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Slow down"}`
		}})
		_, err := k.Discover(ctx, attioTestSource(true))
		notionRefusal(t, err, "this Attio token has used up its rate limit; Helix will retry")
		if took := hits.took(); took["GET /v2/self"] != maxRetries+1 {
			t.Fatalf("requests = %v", took)
		}
	})

	t.Run("refuses an outage after retrying it, with Attio's message", func(t *testing.T) {
		k, hits := attioAt(t, attioWorkspace(t, attioAllScopes, attioRoutes{
			"/v2/objects": func(*http.Request) (int, string) {
				return http.StatusBadGateway, `{"status_code":502,"type":"api_error","code":"bad_gateway","message":"Try later."}`
			},
		}))
		_, err := k.Discover(ctx, attioTestSource(true))
		notionRefusal(t, err, "Attio returned HTTP 502 for /v2/objects: Try later.")
		if took := hits.took(); took["GET /v2/objects"] != maxRetries+1 {
			t.Fatalf("requests = %v", took)
		}
	})

	t.Run("refuses an answer without a body's message", func(t *testing.T) {
		k, _ := attioAt(t, attioRoutes{"/v2/self": func(*http.Request) (int, string) { return http.StatusBadRequest, `oops` }})
		_, err := k.Discover(ctx, attioTestSource(true))
		notionRefusal(t, err, "Attio returned HTTP 400 for /v2/self; try again")
	})

	t.Run("fails on a list without its data", func(t *testing.T) {
		k, _ := attioAt(t, attioWorkspace(t, attioAllScopes, attioRoutes{
			"/v2/objects": func(*http.Request) (int, string) { return http.StatusOK, `{}` },
		}))
		if _, err := k.Discover(ctx, attioTestSource(true)); err == nil || !strings.Contains(err.Error(), "the response has no data") {
			t.Fatalf("Discover = %v", err)
		}
	})
}

// Objects and lists name their tables from their slugs, in Attio's order:
// one without a slug has none, and a name a fixed table, an earlier object
// or a list holds takes the first free number. Attributes name columns
// likewise.
func TestAttioNaming(t *testing.T) {
	k, _ := attioAt(t, attioRoutes{
		"/v2/self": func(*http.Request) (int, string) { return http.StatusOK, attioSelfJSON(attioAllScopes) },
		"/v2/objects": func(*http.Request) (int, string) {
			return http.StatusOK, attioData(attioObjectJSON("o1", "deals"), attioObjectJSON("o2", "Deals"), attioObjectJSON("o3", "notes"),
				attioObjectJSON("o4", ""), attioObjectJSON("o5", "Big Deals!"), attioObjectJSON("o6", "list_pipeline"), attioObjectJSON("o7", "---"),
				attioObjectJSON("o8", "dealStage"))
		},
		"/v2/objects/o1/attributes": attioPaged(t, []string{attioAttributeJSON("name", "personal-name", false, false),
			attioAttributeJSON("name_first_name", "text", false, false), attioAttributeJSON("Name", "text", false, false),
			attioAttributeJSON("value", "currency", true, false), attioAttributeJSON("vibe", "sentiment", false, false),
			attioAttributeJSON("vibes", "sentiment", true, false), attioAttributeJSON("touch", "interaction", false, false),
			attioAttributeJSON("where", "location", false, false), attioAttributeJSON("", "text", false, false)}),
		"/v2/objects/o2/attributes": attioPaged(t, nil),
		"/v2/objects/o3/attributes": attioPaged(t, nil),
		"/v2/objects/o5/attributes": attioPaged(t, nil),
		"/v2/objects/o6/attributes": attioPaged(t, nil),
		"/v2/objects/o7/attributes": attioPaged(t, nil),
		"/v2/objects/o8/attributes": attioPaged(t, nil),
		"/v2/lists": func(*http.Request) (int, string) {
			return http.StatusOK, attioData(attioListJSON("l1", "pipeline", "deals"), attioListJSON("l2", "Pipeline", "deals"))
		},
		"/v2/lists/l1/attributes": attioPaged(t, nil),
		"/v2/lists/l2/attributes": attioPaged(t, nil),
	})
	d, err := k.Discover(context.Background(), attioTestSource(true))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"workspace_members", "objects", "deals", "deals_2", "notes_2", "big_deals", "list_pipeline", "object", "deal_stage",
		"lists", "list_pipeline_2", "list_pipeline_3", "notes", "tasks", "task_assignees", "task_records"}
	if !slices.Equal(tableNames(d), want) {
		t.Fatalf("tables = %v", tableNames(d))
	}
	deals := findTable(t, d, "deals")
	columns := []Column{
		{Name: "record_id", Type: "string"}, {Name: "created_at", Type: "string", Nullable: true}, {Name: "web_url", Type: "string", Nullable: true},
		{Name: "name", Type: "string", Nullable: true}, {Name: "name_first_name", Type: "string", Nullable: true},
		{Name: "name_last_name", Type: "string", Nullable: true}, {Name: "name_first_name_2", Type: "string", Nullable: true},
		{Name: "name_2", Type: "string", Nullable: true}, {Name: "value", Type: "array"},
		{Name: "vibe", Nullable: true}, {Name: "vibes", Type: "array"},
		{Name: "touch", Type: "string", Nullable: true}, {Name: "touch_type", Type: "string", Nullable: true},
		{Name: "where", Type: "object", Nullable: true},
	}
	if !slices.Equal(deals.Columns, columns) {
		t.Fatalf("deals columns = %+v", deals.Columns)
	}
}

// attioColumnOf is the column of an attribute of type kind, named v, as
// the listing names its siblings.
func attioColumnOf(kind string, multi bool) attioColumn {
	col := attioColumn{slug: "v", kind: kind, name: "v", multi: multi}
	if _, known := attioTypes[kind]; known && !multi {
		for _, sib := range attioSiblings[kind] {
			col.siblings = append(col.siblings, struct{ name, field string }{"v" + sib.suffix, sib.field})
		}
	}
	return col
}

// TestAttioValues fills the columns of every attribute type from its values.
func TestAttioValues(t *testing.T) {
	value := func(fields string) map[string]any {
		var v map[string]any
		if err := json.Unmarshal([]byte(`{"active_from":"2024-01-01T00:00:00Z","active_until":null,"attribute_type":"x",`+fields+`}`), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		name, kind string
		multi      bool
		values     []map[string]any
		want       string
	}{
		{"text", "text", false, []map[string]any{value(`"value":"Hello"`)}, `{"v":"Hello"}`},
		{"number", "number", false, []map[string]any{value(`"value":42.5`)}, `{"v":42.5}`},
		{"checkbox", "checkbox", false, []map[string]any{value(`"value":true`)}, `{"v":true}`},
		{"rating", "rating", false, []map[string]any{value(`"value":4`)}, `{"v":4}`},
		{"date", "date", false, []map[string]any{value(`"value":"2024-03-01"`)}, `{"v":"2024-03-01"}`},
		{"timestamp", "timestamp", false, []map[string]any{value(`"value":"2024-03-01T10:00:00.000000000Z"`)}, `{"v":"2024-03-01T10:00:00.000000000Z"}`},
		{"currency", "currency", false, []map[string]any{value(`"currency_value":99.99,"currency_code":"EUR"`)}, `{"v":99.99,"v_currency":"EUR"}`},
		{"currency without a code", "currency", false, []map[string]any{value(`"currency_value":5,"currency_code":null`)}, `{"v":5,"v_currency":null}`},
		{"select", "select", false, []map[string]any{value(`"option":{"id":{"option_id":"o1"},"title":"Enterprise","is_archived":false}`)}, `{"v":"Enterprise"}`},
		{"status", "status", false, []map[string]any{value(`"status":{"id":{"status_id":"s1"},"title":"Won","is_archived":false}`)}, `{"v":"Won"}`},
		{"record reference", "record-reference", false, []map[string]any{value(`"target_object":"companies","target_record_id":"rec_1"`)},
			`{"v":"rec_1","v_object":"companies"}`},
		{"actor reference", "actor-reference", false, []map[string]any{value(`"referenced_actor_type":"workspace-member","referenced_actor_id":"wm_1"`)}, `{"v":"wm_1"}`},
		{"domain", "domain", false, []map[string]any{value(`"domain":"app.acme.com","root_domain":"acme.com"`)}, `{"v":"app.acme.com"}`},
		{"email address", "email-address", false, []map[string]any{value(`"original_email_address":"Ada@Acme.com","email_address":"ada@acme.com"`)}, `{"v":"ada@acme.com"}`},
		{"phone number", "phone-number", false, []map[string]any{value(`"original_phone_number":"07700 900000","country_code":"GB","phone_number":"+447700900000"`)}, `{"v":"+447700900000"}`},
		{"personal name", "personal-name", false, []map[string]any{value(`"first_name":"Ada","last_name":"Lovelace","full_name":"Ada Lovelace"`)},
			`{"v":"Ada Lovelace","v_first_name":"Ada","v_last_name":"Lovelace"}`},
		{"location", "location", false, []map[string]any{value(`"line_1":"1 Main St","line_2":null,"line_3":null,"line_4":null,"locality":"London",
			"region":null,"postcode":"N1 1AA","country_code":"GB","latitude":"51.5","longitude":null`)},
			`{"v":{"country_code":"GB","latitude":"51.5","line_1":"1 Main St","locality":"London","postcode":"N1 1AA"}}`},
		{"an empty location", "location", false, []map[string]any{value(`"line_1":null,"locality":null`)}, `{"v":null}`},
		{"interaction", "interaction", false, []map[string]any{value(`"interaction_type":"email","interacted_at":"2024-03-01T10:00:00Z","owner_actor":{}`)},
			`{"v":"2024-03-01T10:00:00Z","v_type":"email"}`},
		{"a type Attio adds later", "sentiment", false, []map[string]any{value(`"mood":"sunny"`)},
			`{"v":{"active_from":"2024-01-01T00:00:00Z","active_until":null,"attribute_type":"x","mood":"sunny"}}`},
		{"a multiselect", "select", true, []map[string]any{value(`"option":{"title":"Red"}`), value(`"option":{"title":"Blue"}`)}, `{"v":["Red","Blue"]}`},
		{"a multiselect reference", "record-reference", true, []map[string]any{value(`"target_object":"people","target_record_id":"rec_1"`),
			value(`"target_object":"people","target_record_id":"rec_2"`)}, `{"v":["rec_1","rec_2"]}`},
		{"a multiselect of nothing", "email-address", true, nil, `{"v":[]}`},
		{"a multiselect value without its field", "actor-reference", true, []map[string]any{value(`"referenced_actor_type":"system","referenced_actor_id":null`),
			value(`"referenced_actor_id":"wm_2"`)}, `{"v":["wm_2"]}`},
		{"no value", "personal-name", false, nil, `{"v":null,"v_first_name":null,"v_last_name":null}`},
		{"no currency", "currency", false, []map[string]any{}, `{"v":null,"v_currency":null}`},
		{"an inactive value", "text", false, []map[string]any{value(`"value":"old","active_until":"2024-02-01T00:00:00Z"`), value(`"value":"new"`)}, `{"v":"new"}`},
		{"the first of several", "text", false, []map[string]any{value(`"value":"one"`), value(`"value":"two"`)}, `{"v":"one"}`},
		{"a missing field", "number", false, []map[string]any{value(`"other":1`)}, `{"v":null}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			row := map[string]any{}
			attioFill(row, attioColumnOf(c.kind, c.multi), c.values)
			if got, _ := json.Marshal(row); string(got) != c.want {
				t.Fatalf("row = %s, want %s", got, c.want)
			}
		})
	}
}

func TestAttioSync(t *testing.T) {
	ctx := context.Background()

	t.Run("writes each object's records and each list's entries with every column", func(t *testing.T) {
		k, _ := attioAt(t, attioWorkspace(t, attioAllScopes, nil))
		people := &rowSink{}
		written, err := k.Sync(ctx, attioTestSource(false), Table{Name: "people"}, people)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(columnNames(written), []string{"record_id", "created_at", "web_url", "name", "name_first_name", "name_last_name",
			"email_addresses", "company", "company_object", "created_at_2"}) || !written.Dense || !slices.Equal(written.PrimaryKey, []string{"record_id"}) {
			t.Fatalf("written = %+v", written)
		}
		want := []string{
			`{"company":"rec_acme","company_object":"companies","created_at":"2024-02-01T09:00:00.000000000Z","created_at_2":"2024-02-01T09:00:00.000000000Z",` +
				`"email_addresses":["ada@acme.com","ada@example.com"],"name":"Ada Lovelace","name_first_name":"Ada","name_last_name":"Lovelace",` +
				`"record_id":"rec_ada","web_url":"https://app.attio.com/acme/person/rec_ada"}`,
			`{"company":null,"company_object":null,"created_at":"2024-02-01T09:00:00.000000000Z","created_at_2":null,"email_addresses":[],` +
				`"name":null,"name_first_name":null,"name_last_name":null,"record_id":"rec_grace","web_url":"https://app.attio.com/acme/person/rec_grace"}`,
		}
		if got := attioRows(people.rows); !slices.Equal(got, want) {
			t.Fatalf("people = %s", got)
		}
		entries := &rowSink{}
		if _, err := k.Sync(ctx, attioTestSource(false), Table{Name: "list_sales_pipeline"}, entries); err != nil {
			t.Fatal(err)
		}
		if got := attioRows(entries.rows); !slices.Equal(got, []string{`{"created_at":"2024-03-01T00:00:00.000000000Z","entry_id":"ent_1",` +
			`"parent_object":"companies","parent_record_id":"rec_acme","stage":"Won","value":1200.5,"value_currency":"GBP"}`}) {
			t.Fatalf("entries = %s", got)
		}
	})

	t.Run("writes members, objects and lists", func(t *testing.T) {
		k, _ := attioAt(t, attioWorkspace(t, attioAllScopes, nil))
		want := map[string][]string{
			"workspace_members": {
				`{"access_level":"admin","created_at":"2024-01-01T00:00:00.000000000Z","email_address":"ada@acme.com","first_name":"Ada","last_name":"Lovelace","workspace_member_id":"wm_ada"}`,
				`{"access_level":"member","created_at":"2024-01-03T00:00:00.000000000Z","email_address":"alan@acme.com","first_name":"Alan","last_name":null,"workspace_member_id":"wm_alan"}`,
			},
			"objects": {
				`{"api_slug":"people","created_at":"2024-01-01T00:00:00.000000000Z","object_id":"obj_people","plural_noun":"Things","singular_noun":"Thing"}`,
				`{"api_slug":"companies","created_at":"2024-01-01T00:00:00.000000000Z","object_id":"obj_companies","plural_noun":"Things","singular_noun":"Thing"}`,
				`{"api_slug":null,"created_at":"2024-01-01T00:00:00.000000000Z","object_id":"obj_noslug","plural_noun":"Things","singular_noun":"Thing"}`,
			},
			"lists": {
				`{"api_slug":"sales_pipeline","created_at":"2024-01-02T00:00:00.000000000Z","list_id":"lst_sales","name":"Sales pipeline","parent_object":"companies"}`,
				`{"api_slug":null,"created_at":"2024-01-02T00:00:00.000000000Z","list_id":"lst_both","name":"Sales pipeline","parent_object":["people","companies"]}`,
			},
		}
		for name, rows := range want {
			sink := &rowSink{}
			if _, err := k.Sync(ctx, attioTestSource(false), Table{Name: name}, sink); err != nil {
				t.Fatal(err)
			}
			if got := attioRows(sink.rows); !slices.Equal(got, rows) {
				t.Fatalf("%s = %s", name, got)
			}
		}
	})

	t.Run("reads the three task tables from one list of the tasks", func(t *testing.T) {
		k, hits := attioAt(t, attioWorkspace(t, attioAllScopes, attioRoutes{
			"/v2/tasks": func(r *http.Request) (int, string) {
				if q := r.URL.Query(); q.Get("sort") != "created_at:asc" || q.Get("limit") != "500" || q.Get("offset") != "0" {
					t.Errorf("tasks query = %s", r.URL.RawQuery)
				}
				return http.StatusOK, attioData(attioTasks...)
			},
		}))
		sinks := map[string]*rowSink{"tasks": {}, "task_assignees": {}, "task_records": {}}
		tables := []Table{{Name: "tasks"}, {Name: "task_assignees"}, {Name: "task_records"}}
		rows := map[string]RowWriter{}
		for name, sink := range sinks {
			rows[name] = sink
		}
		if _, err := k.SyncPass(ctx, attioTestSource(false), tables, rows, noDocuments{}); err != nil {
			t.Fatal(err)
		}
		if took := hits.took(); took["GET /v2/tasks"] != 1 {
			t.Fatalf("requests = %v", took)
		}
		want := map[string][]string{
			"tasks": {
				`{"completed_at":null,"content":"Call Ada","created_at":"2024-03-02T00:00:00.000000000Z","created_by_id":"wm_alan",` +
					`"created_by_type":"workspace-member","deadline_at":null,"is_completed":false,"task_id":"tsk_1"}`,
				`{"completed_at":"2024-05-20T00:00:00.000000000Z","content":"Send terms","created_at":"2024-03-03T00:00:00.000000000Z",` +
					`"created_by_id":"tok","created_by_type":"api-token","deadline_at":"2024-06-01T00:00:00.000000000Z","is_completed":true,"task_id":"tsk_2"}`,
			},
			// An assignee without an id has no row, and one listed twice one.
			"task_assignees": {`{"assignee_id":"wm_ada","assignee_type":"workspace-member","task_id":"tsk_1"}`},
			// An object the listing doesn't name keeps its id.
			"task_records": {
				`{"target_object":"people","target_record_id":"rec_ada","task_id":"tsk_1"}`,
				`{"target_object":"obj_gone","target_record_id":"rec_x","task_id":"tsk_1"}`,
			},
		}
		for name, rows := range want {
			if got := attioRows(sinks[name].rows); !slices.Equal(got, rows) {
				t.Fatalf("%s = %s", name, got)
			}
		}
	})

	t.Run("pages through every record by offset, writing each once", func(t *testing.T) {
		var records []string
		for i := range 1100 {
			records = append(records, attioRecordJSON(fmt.Sprintf("rec_%04d", i), `{}`))
		}
		// A record that moved between pages is listed again on the next.
		records[500] = records[499]
		var offsets []int
		k, _ := attioAt(t, attioWorkspace(t, attioAllScopes, attioRoutes{
			"/v2/objects/obj_companies/records/query": func(r *http.Request) (int, string) {
				var body map[string]int
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 2 || body["limit"] != 500 {
					t.Errorf("query = %v, %v", body, err)
				}
				offsets = append(offsets, body["offset"])
				return http.StatusOK, attioData(records[min(body["offset"], len(records)):min(body["offset"]+500, len(records))]...)
			},
		}))
		sink := &rowSink{}
		if _, err := k.Sync(ctx, attioTestSource(false), Table{Name: "companies"}, sink); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(offsets, []int{0, 500, 1000}) || len(sink.rows) != 1099 {
			t.Fatalf("offsets = %v, rows = %d", offsets, len(sink.rows))
		}
		if sink.rows[0]["name"] != nil || sink.rows[0]["record_id"] != "rec_0000" || len(sink.rows[0]) != 6 {
			t.Fatalf("first row = %v", sink.rows[0])
		}
	})

	t.Run("fails a list past the page cap rather than publish part of it", func(t *testing.T) {
		saved := maxAttioPages
		maxAttioPages = 2
		t.Cleanup(func() { maxAttioPages = saved })
		var notes []string
		for i := range 150 {
			notes = append(notes, attioNoteJSON(fmt.Sprintf("not_%03d", i), "N", "body", "2024-03-01T10:00:00Z", "people", "rec_ada"))
		}
		k, _ := attioAt(t, attioWorkspace(t, attioAllScopes, attioRoutes{"/v2/notes": attioPaged(t, notes)}))
		_, err := k.Sync(ctx, attioTestSource(false), Table{Name: "notes"}, &rowSink{})
		notionInvalid(t, err, "Attio holds more notes than Helix reads in one sync (2 pages of 50)")
	})

	t.Run("refuses a table whose object or list Attio no longer has", func(t *testing.T) {
		k, _ := attioAt(t, attioWorkspace(t, attioAllScopes, nil))
		for _, name := range []string{"deals", "list_renewals"} {
			_, err := k.Sync(ctx, attioTestSource(false), Table{Name: name}, &rowSink{})
			notionRefusal(t, err, "the Attio object or list of table "+name+" no longer exists in Attio, or was renamed; test the connection again")
		}
	})

	t.Run("refuses a record query Attio fails", func(t *testing.T) {
		k, _ := attioAt(t, attioWorkspace(t, attioAllScopes, attioRoutes{
			"/v2/objects/obj_people/records/query": func(*http.Request) (int, string) {
				return http.StatusForbidden, `{"status_code":403,"type":"auth_error","code":"unauthorized","message":"No access."}`
			},
		}))
		_, err := k.Sync(ctx, attioTestSource(false), Table{Name: "people"}, &rowSink{})
		notionRefusal(t, err, "this Attio token may not read /v2/objects/obj_people/records/query: No access.")
	})

	t.Run("a sync reads the listing its discovery made", func(t *testing.T) {
		k, hits := attioAt(t, attioWorkspace(t, attioAllScopes, nil))
		src := attioTestSource(true)
		src.ConnectorID = "conn-1"
		d, err := k.Discover(ctx, src)
		if err != nil {
			t.Fatal(err)
		}
		hits.took()
		rows := map[string]RowWriter{}
		for _, table := range d.Tables {
			rows[table.Name] = &rowSink{}
		}
		written, err := k.SyncPass(ctx, src, d.Tables, rows, noDocuments{})
		if err != nil || len(written) != len(d.Tables) {
			t.Fatalf("SyncPass = %v, %v", written, err)
		}
		took := hits.took()
		for _, path := range []string{"GET /v2/self", "GET /v2/objects", "GET /v2/lists", "GET /v2/objects/obj_people/attributes"} {
			if took[path] != 0 {
				t.Fatalf("the sync listed again: %v", took)
			}
		}
		if took["GET /v2/workspace_members"] != 1 || took["GET /v2/tasks"] != 1 || took["GET /v2/notes"] != 1 ||
			took["POST /v2/objects/obj_people/records/query"] != 1 || took["POST /v2/lists/lst_sales/entries/query"] != 1 {
			t.Fatalf("requests = %v", took)
		}
		// Another connector's sync lists for itself.
		other := src
		other.ConnectorID = "conn-2"
		if _, err := k.listed(ctx, k.client(other.Secret), other); err != nil || hits.took()["GET /v2/self"] != 1 {
			t.Fatalf("listed = %v", err)
		}
	})
}

// attioVersion is a note's document's version, as the kind writes it.
func attioVersion(title, content string) string {
	return fmt.Sprintf("1|%x", sha256.Sum256([]byte(title+"\x00"+content)))[:34]
}

func TestAttioDocuments(t *testing.T) {
	ctx := context.Background()

	t.Run("offers each note's document newest first, and writes every note's row", func(t *testing.T) {
		k, _ := attioAt(t, attioWorkspace(t, attioAllScopes, nil))
		docs := &granolaDocs{want: map[string]bool{"notes": true}}
		sink := &rowSink{}
		if _, err := k.SyncDocuments(ctx, attioTestSource(false), Table{Name: "notes"}, sink, docs); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, row := range sink.rows {
			ids = append(ids, row["note_id"].(string))
		}
		if !slices.Equal(ids, []string{"not_new", "not_tie_a", "not_tie_b", "not_empty", "not_old", "not_untitled"}) {
			t.Fatalf("rows = %v", ids)
		}
		// A blank note the stream doesn't hold isn't offered.
		if !slices.Equal(docs.putNames(), []string{"notes not_new", "notes not_tie_a", "notes not_tie_b", "notes not_old", "notes not_untitled"}) ||
			len(docs.offers) != 5 || len(docs.drops) != 0 {
			t.Fatalf("offers = %q, puts = %v, drops = %v", docs.offers, docs.putNames(), docs.drops)
		}
		renewal := docs.puts[0].doc
		want := Document{ExternalID: "not_new", Version: attioVersion("  Renewal   call ", "## Terms\n\nTwo years."),
			At: time.Date(2024, 5, 1, 10, 0, 0, 123456789, time.UTC), Title: "Renewal call", URL: "https://app.attio.com/acme/companies/rec_acme",
			Text: []byte("# Renewal call\n\n## Terms\n\nTwo years.")}
		if renewal.ExternalID != want.ExternalID || renewal.Version != want.Version || !renewal.At.Equal(want.At) ||
			renewal.Title != want.Title || renewal.URL != want.URL || string(renewal.Text) != string(want.Text) {
			t.Fatalf("document = %+v, text %q", renewal, renewal.Text)
		}
		if docs.offers[0] != "notes not_new "+want.Version {
			t.Fatalf("offer = %q", docs.offers[0])
		}
		untitled := docs.puts[4].doc
		if untitled.Title != "Untitled note" || string(untitled.Text) != "# Untitled note\n\nJust text" || untitled.URL != "" {
			t.Fatalf("untitled = %+v, text %q", untitled, untitled.Text)
		}
		got := attioRows(sink.rows)
		if got[0] != `{"created_at":"2024-05-01T10:00:00.123456789Z","created_by_id":"wm_ada","created_by_type":"workspace-member",`+
			`"meeting_id":null,"note_id":"not_new","parent_object":"companies","parent_record_id":"rec_acme","title":"  Renewal   call "}` ||
			!strings.Contains(got[5], `"title":null`) || !strings.Contains(got[5], `"parent_object":null`) {
			t.Fatalf("rows = %s", got)
		}
	})

	t.Run("takes back the document of a note whose body was emptied", func(t *testing.T) {
		k, _ := attioAt(t, attioWorkspace(t, attioAllScopes, nil))
		docs := &notionHeld{granolaDocs: &granolaDocs{want: map[string]bool{"notes": true}}, held: map[string]bool{"not_empty": true}}
		if _, err := k.SyncDocuments(ctx, attioTestSource(false), Table{Name: "notes"}, &rowSink{}, docs); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(docs.drops, []string{"notes not_empty"}) || slices.Contains(docs.putNames(), "notes not_empty") || len(docs.offers) != 6 {
			t.Fatalf("offers = %q, drops = %v", docs.offers, docs.drops)
		}
	})

	t.Run("puts nothing a writer doesn't want, and nothing in a plain sync", func(t *testing.T) {
		k, _ := attioAt(t, attioWorkspace(t, attioAllScopes, nil))
		only := &onlyDocs{id: "not_old"}
		if _, err := k.SyncDocuments(ctx, attioTestSource(false), Table{Name: "notes"}, &rowSink{}, only); err != nil {
			t.Fatal(err)
		}
		sink := &rowSink{}
		if _, err := k.Sync(ctx, attioTestSource(false), Table{Name: "notes"}, sink); err != nil || len(sink.rows) != 6 {
			t.Fatalf("Sync = %d rows, %v", len(sink.rows), err)
		}
	})

	t.Run("pages through every note, paced", func(t *testing.T) {
		var notes []string
		for i := range 120 {
			notes = append(notes, attioNoteJSON(fmt.Sprintf("not_%03d", i), "N", "body", fmt.Sprintf("2024-03-01T10:%02d:00Z", i%60), "people", "rec_ada"))
		}
		var asked []string
		paged := attioPaged(t, notes)
		k, _ := attioAt(t, attioWorkspace(t, attioAllScopes, attioRoutes{"/v2/notes": func(r *http.Request) (int, string) {
			asked = append(asked, r.URL.RawQuery)
			return paged(r)
		}}))
		docs := &granolaDocs{want: map[string]bool{"notes": true}}
		sink := &rowSink{}
		if _, err := k.SyncDocuments(ctx, attioTestSource(false), Table{Name: "notes"}, sink, docs); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(asked, []string{"limit=50&offset=0", "limit=50&offset=50", "limit=50&offset=100"}) || len(sink.rows) != 120 || len(docs.puts) != 120 {
			t.Fatalf("asked %v, rows = %d, puts = %d", asked, len(sink.rows), len(docs.puts))
		}
	})

	t.Run("a note's version is its title's and body's", func(t *testing.T) {
		versionOf := func(title, content, created, parent string) string {
			n, err := attioNoteOf(json.RawMessage(attioNoteJSON("not_1", title, content, created, parent, "rec_1")), "acme")
			if err != nil {
				t.Fatal(err)
			}
			return n.version
		}
		v := versionOf("Call", "Body", "2024-03-01T10:00:00Z", "people")
		if v != attioVersion("Call", "Body") || len(v) != 34 || !strings.HasPrefix(v, "1|") {
			t.Fatalf("version = %q", v)
		}
		if versionOf("Call", "Body", "2024-03-02T10:00:00Z", "companies") != v {
			t.Fatal("the version moved with what is not its text")
		}
		for _, other := range []string{versionOf("Call", "Body.", "2024-03-01T10:00:00Z", "people"), versionOf("Calls", "Body", "2024-03-01T10:00:00Z", "people"),
			versionOf("CallB", "ody", "2024-03-01T10:00:00Z", "people")} {
			if other == v {
				t.Fatal("a changed note kept its version")
			}
		}
	})

	t.Run("a note without its id or time fails the read", func(t *testing.T) {
		for _, raw := range []string{
			attioNoteJSON("", "T", "b", "2024-03-01T10:00:00Z", "people", "rec_1"),
			attioNoteJSON("not_1", "T", "b", "yesterday", "people", "rec_1"),
		} {
			if _, err := attioNoteOf(json.RawMessage(raw), "acme"); err == nil {
				t.Fatalf("read %s", raw)
			}
		}
	})

	t.Run("a note has no link without the workspace's slug", func(t *testing.T) {
		n, err := attioNoteOf(json.RawMessage(attioNoteJSON("not_1", "T", "b", "2024-03-01T10:00:00Z", "people", "rec_1")), "")
		if err != nil || n.document().URL != "" {
			t.Fatalf("note = %+v, %v", n, err)
		}
	})
}
