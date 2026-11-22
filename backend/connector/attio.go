package connector

import (
	"bytes"
	"cmp"
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/time/rate"

	"github.com/helixdb/helix-foundry/backend/config"
)

// The attio kind reads an Attio workspace with an access token: its
// members, objects and each object's records, lists and each list's
// entries, notes (each also a Markdown document) and tasks, as far as the
// token's scopes allow.
//
// Attio offers no change reads: no record, entry, note or task carries an
// update time, and no list filters by one, so every table is read in full
// on every sync (the kind is no ChangeReader), and the plans' reconcile is
// the only schedule that matters.
var _ PassKind = (*attioKind)(nil)

const (
	// attioTokenLength is an access token's length: Attio refuses any other
	// with "API Keys should be 64 characters long". Its characters are not
	// documented, so they are not checked.
	attioTokenLength = 64
	// attioRecordPage is the records and entries a query asks for (Attio's
	// default), attioNotePage the most a notes page returns, attioTaskPage
	// the tasks a page asks for (Attio's default), and attioAttributePage
	// the attributes.
	attioRecordPage    = 500
	attioNotePage      = 50
	attioTaskPage      = 500
	attioAttributePage = 100
	// attioPreviewRows are the rows a test shows of each of the first
	// attioPreviews object and list tables.
	attioPreviewRows = 5
	attioPreviews    = 20
)

// maxAttioPages bounds one list: past it, a table would be published
// partly read. Tests lower it.
var maxAttioPages = 10_000

// attioNotesInterval spaces one read's notes pages: Attio limits listing
// notes to 10 requests a second. Tests set it to 0.
var attioNotesInterval = time.Second / 10

// The scopes Attio's endpoints require. A token holds each resource's
// scope as :read or :read-write; either reads.
const (
	attioMembersScope = "user_management:read"
	attioObjectsScope = "object_configuration:read"
	attioRecordsScope = "record_permission:read"
	attioListsScope   = "list_configuration:read"
	attioEntriesScope = "list_entry:read"
	attioNotesScope   = "note:read"
	attioTasksScope   = "task:read"
)

// The scopes each fixed table's reads require, as Attio's API documents them.
var (
	attioNoteScopes = []string{attioNotesScope, attioObjectsScope, attioRecordsScope}
	attioTaskScopes = []string{attioTasksScope, attioObjectsScope, attioRecordsScope, attioMembersScope}
)

// attioFixed are the tables every Attio connector may have, by name; an
// object's or a list's table never takes one of their names.
var attioFixed = []string{"workspace_members", "objects", "lists", "notes", "tasks", "task_assignees", "task_records"}

// The fixed tables' columns, and those every object's and list's table
// starts with. Columns are typed by the JSON they hold; times are as Attio
// gives them, ISO 8601 in UTC.
var (
	attioMemberColumns = []Column{{Name: "workspace_member_id", Type: "string"}, {Name: "first_name", Type: "string", Nullable: true},
		{Name: "last_name", Type: "string", Nullable: true}, {Name: "email_address", Type: "string", Nullable: true},
		{Name: "access_level", Type: "string", Nullable: true}, {Name: "created_at", Type: "string", Nullable: true}}
	attioObjectColumns = []Column{{Name: "object_id", Type: "string"}, {Name: "api_slug", Type: "string", Nullable: true},
		{Name: "singular_noun", Type: "string", Nullable: true}, {Name: "plural_noun", Type: "string", Nullable: true},
		{Name: "created_at", Type: "string", Nullable: true}}
	// A list's parent_object is its one object's slug, or an array of them
	// for a list of several objects.
	attioListColumns = []Column{{Name: "list_id", Type: "string"}, {Name: "api_slug", Type: "string", Nullable: true},
		{Name: "name", Type: "string", Nullable: true}, {Name: "parent_object", Nullable: true},
		{Name: "created_at", Type: "string", Nullable: true}}
	attioNoteColumns = []Column{{Name: "note_id", Type: "string"}, {Name: "parent_object", Type: "string", Nullable: true},
		{Name: "parent_record_id", Type: "string", Nullable: true}, {Name: "title", Type: "string", Nullable: true},
		{Name: "meeting_id", Type: "string", Nullable: true}, {Name: "created_by_type", Type: "string", Nullable: true},
		{Name: "created_by_id", Type: "string", Nullable: true}, {Name: "created_at", Type: "string"}}
	attioTaskColumns = []Column{{Name: "task_id", Type: "string"}, {Name: "content", Type: "string", Nullable: true},
		{Name: "deadline_at", Type: "string", Nullable: true}, {Name: "is_completed", Type: "boolean"},
		{Name: "completed_at", Type: "string", Nullable: true}, {Name: "created_by_type", Type: "string", Nullable: true},
		{Name: "created_by_id", Type: "string", Nullable: true}, {Name: "created_at", Type: "string", Nullable: true}}
	attioAssigneeColumns = []Column{{Name: "task_id", Type: "string"}, {Name: "assignee_type", Type: "string"},
		{Name: "assignee_id", Type: "string"}}
	attioTaskRecordColumns = []Column{{Name: "task_id", Type: "string"}, {Name: "target_object", Type: "string"},
		{Name: "target_record_id", Type: "string"}}
	attioRecordColumns = []Column{{Name: "record_id", Type: "string"}, {Name: "created_at", Type: "string", Nullable: true},
		{Name: "web_url", Type: "string", Nullable: true}}
	attioEntryColumns = []Column{{Name: "entry_id", Type: "string"}, {Name: "parent_object", Type: "string", Nullable: true},
		{Name: "parent_record_id", Type: "string", Nullable: true}, {Name: "created_at", Type: "string", Nullable: true}}
)

// attioTypes are the attribute types Attio documents and the JSON type a
// single value of each is stored as (attioValue); a type Attio adds later
// is stored as its raw value, of no one type.
var attioTypes = map[string]string{
	"text": "string", "number": "number", "checkbox": "boolean", "currency": "number", "date": "string",
	"timestamp": "string", "rating": "number", "status": "string", "select": "string",
	"record-reference": "string", "actor-reference": "string", "location": "object", "domain": "string",
	"email-address": "string", "phone-number": "string", "interaction": "string", "personal-name": "string",
}

// attioSiblings are the columns a single-valued attribute of a type has
// beside its own, named by the attribute's column and the suffix: each
// holds the value's field.
var attioSiblings = map[string][]struct{ suffix, field string }{
	"currency":         {{"_currency", "currency_code"}},
	"record-reference": {{"_object", "target_object"}},
	"personal-name":    {{"_first_name", "first_name"}, {"_last_name", "last_name"}},
	"interaction":      {{"_type", "interaction_type"}},
}

// attioLocationFields are a location's fields, the non-null ones of which
// its column holds as one object.
var attioLocationFields = []string{"line_1", "line_2", "line_3", "line_4", "locality", "region", "postcode",
	"country_code", "latitude", "longitude"}

// attioNotesStream is every note's Markdown, kept on the notes table.
var attioNotesStream = Stream{Name: "notes", Table: "notes", Title: "Notes", Format: Markdown,
	Description: "Notes written in Attio on its records, one document each: the note's title as a heading, then its " +
		"Markdown body. The notes table names the record each is on (parent_object, parent_record_id), its meeting " +
		"and who wrote it."}

type attioKind struct {
	base string
	http *http.Client
	// listings are by connector id (attioRemembered), under mu.
	mu       sync.Mutex
	listings map[string]attioRemembered
}

func (k *attioKind) Name() string        { return "attio" }
func (k *attioKind) DisplayName() string { return "Attio" }

var _ Identifier = (*attioKind)(nil)

// Prepare takes a token of 64 characters with no space or control
// character, and no settings: the connector reads everything the token's
// scopes allow. A token the broker vended (an OAuth app's) has no format of
// ours to check.
func (k *attioKind) Prepare(src Source) (json.RawMessage, error) {
	token := strings.TrimSpace(src.Secret)
	if !src.SignIn && (len(token) != attioTokenLength ||
		strings.ContainsFunc(token, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })) {
		return nil, invalid("enter an Attio access token (64 characters)")
	}
	if err := emptySettings(src.Settings); err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}

func (k *attioKind) Hint(secret string) string { return keyHint(strings.TrimSpace(secret), 0) }

// Identity is the name of the workspace the token reads, from Attio's
// introspection (/v2/self), and its id: an OAuth app's token is installed in
// one workspace. A token Attio answers as inactive is rejected.
func (k *attioKind) Identity(ctx context.Context, src Source) (string, string, error) {
	var self struct {
		Active        bool   `json:"active"`
		WorkspaceID   string `json:"workspace_id"`
		WorkspaceName string `json:"workspace_name"`
		WorkspaceSlug string `json:"workspace_slug"`
	}
	if err := k.client(src.Secret).get(ctx, "/v2/self", nil, &self); err != nil {
		return "", "", err
	}
	if !self.Active {
		return "", "", attioRejected()
	}
	if name := cmp.Or(strings.TrimSpace(self.WorkspaceName), self.WorkspaceSlug); name != "" {
		return name, self.WorkspaceID, nil
	}
	return "", "", errors.New("attio /v2/self names no workspace")
}

// Streams are the notes, always.
func (k *attioKind) Streams(Source) []Stream { return []Stream{attioNotesStream} }

// Discover proves the token with Attio's introspection, which also names
// its scopes, then lists the objects and lists those scopes may read, and
// each one's attributes. A table whose reads need a scope the token lacks
// is unavailable, naming the scopes. A test also reads a few records or
// entries of each of the first object and list tables: one Attio refuses
// (403) though the token holds its scopes is unavailable with Attio's
// reason. A sync's reads use what its Discover listed (remember).
func (k *attioKind) Discover(ctx context.Context, src Source) (Discovery, error) {
	c := k.client(src.Secret)
	l, err := c.listing(ctx)
	if err != nil {
		return Discovery{}, err
	}
	var d Discovery
	add := func(t Table, why string) {
		if why != "" {
			d.Unavailable = append(d.Unavailable, Unavailable{Name: t.Name, Reason: why})
			return
		}
		d.Tables = append(d.Tables, t)
	}
	fixed := func(name string, columns []Column, key []string) Table {
		return Table{Name: name, Columns: slices.Clone(columns), PrimaryKey: key, Dense: true}
	}
	add(fixed("workspace_members", attioMemberColumns, []string{"workspace_member_id"}), l.scopes.lacks(attioMembersScope))
	objectsWhy := l.scopes.lacks(attioObjectsScope)
	if objectsWhy != "" {
		objectsWhy += ", so no object's records can be read either"
	}
	add(fixed("objects", attioObjectColumns, []string{"object_id"}), objectsWhy)
	listsWhy := l.scopes.lacks(attioListsScope)
	if listsWhy != "" {
		listsWhy += ", so no list's entries can be read either"
	}
	var lists []attioSource
	for _, s := range l.sources {
		if s.list {
			lists = append(lists, s)
			continue
		}
		add(s.table, l.scopes.lacks(attioRecordsScope, attioObjectsScope))
	}
	add(fixed("lists", attioListColumns, []string{"list_id"}), listsWhy)
	for _, s := range lists {
		add(s.table, l.scopes.lacks(attioEntriesScope, attioListsScope))
	}
	add(fixed("notes", attioNoteColumns, []string{"note_id"}), l.scopes.lacks(attioNoteScopes...))
	tasksWhy := l.scopes.lacks(attioTaskScopes...)
	tasks := fixed("tasks", attioTaskColumns, []string{"task_id"})
	assignees := fixed("task_assignees", attioAssigneeColumns, []string{"task_id", "assignee_type", "assignee_id"})
	records := fixed("task_records", attioTaskRecordColumns, []string{"task_id", "target_object", "target_record_id"})
	assignees.ForeignKeys = []ForeignKey{foreignKey("task_id", "tasks", "task_id")}
	records.ForeignKeys = []ForeignKey{foreignKey("task_id", "tasks", "task_id")}
	for _, t := range []Table{tasks, assignees, records} {
		add(t, tasksWhy)
	}
	if len(d.Tables) == 0 {
		return Discovery{}, refused(nil, "this Attio token has none of the read scopes Helix reads with; give it read access "+
			"to records, lists, notes, tasks or members in Attio, then test again")
	}
	if !src.ForSync {
		if d, err = c.previews(ctx, l, d); err != nil {
			return Discovery{}, err
		}
	}
	if src.ForSync && src.ConnectorID != "" {
		k.remember(src.ConnectorID, c.token, l)
	}
	return d, nil
}

// previews reads a few rows of each of the first attioPreviews object and
// list tables d holds, one query each. A query Attio refuses for the
// token's access (attioDenied) makes the table unavailable with Attio's
// reason; any other failure fails the test.
func (c *attioClient) previews(ctx context.Context, l attioListing, d Discovery) (Discovery, error) {
	shown := 0
	out := d
	out.Tables = nil
	for _, t := range d.Tables {
		s, ok := l.source(t.Name)
		if !ok || shown == attioPreviews {
			out.Tables = append(out.Tables, t)
			continue
		}
		shown++
		items, err := c.page(ctx, s.path(), true, attioPreviewRows, 0, nil)
		var denied *attioDenied
		switch {
		case errors.As(err, &denied):
			out.Unavailable = append(out.Unavailable, Unavailable{Name: t.Name, Reason: denied.message})
			continue
		case err != nil:
			return Discovery{}, err
		}
		for _, raw := range items[:min(len(items), attioPreviewRows)] {
			values, _, err := s.row(raw)
			if err != nil {
				return Discovery{}, err
			}
			row, _ := json.Marshal(values) // values decoded from JSON always encode
			t.Preview = append(t.Preview, row)
		}
		out.Tables = append(out.Tables, t)
	}
	return out, nil
}

// Sync and SyncDocuments read one table through SyncPass. The syncer
// reads a PassKind's tables in one pass and calls neither.
func (k *attioKind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	return k.SyncDocuments(ctx, src, table, w, noDocuments{})
}

func (k *attioKind) SyncDocuments(ctx context.Context, src Source, table Table, rows RowWriter, docs DocumentWriter) (Table, error) {
	written, err := k.SyncPass(ctx, src, []Table{table}, map[string]RowWriter{table.Name: rows}, docs)
	return written[0], err
}

// SyncPass reads each table in full: the members from their list, objects
// and lists from the listing its sync's Discover made (or, past
// attioListingFresh, a new one), each object's records and each list's
// entries from their queries, the notes from their list, offering their
// documents, and the three task tables from one list of tasks. An object's
// or list's table is named only by the listing, so a table whose object or
// list is gone, or renamed in Attio, is refused, and the sync's next
// Discover names it anew. Each object's and list's table is returned with
// the columns of the attributes that listing read.
func (k *attioKind) SyncPass(ctx context.Context, src Source, tables []Table, rows map[string]RowWriter, docs DocumentWriter) ([]Table, error) {
	c := k.client(src.Secret)
	written := slices.Clone(tables)
	if len(tables) == 1 && tables[0].Name == "workspace_members" {
		return written, c.members(ctx, rows["workspace_members"])
	}
	l, err := k.listed(ctx, c, src)
	if err != nil {
		return written, err
	}
	for i, table := range tables {
		if slices.Contains(attioFixed, table.Name) {
			continue
		}
		s, ok := l.source(table.Name)
		if !ok {
			return written, refused(nil, "the Attio object or list of table %s no longer exists in Attio, or was renamed; test the connection again", table.Name)
		}
		written[i] = s.table
	}
	var taskWriters [3]RowWriter
	for _, table := range tables {
		w := rows[table.Name]
		switch table.Name {
		case "workspace_members":
			err = c.members(ctx, w)
		case "objects":
			err = writeAll(w, l.objects)
		case "lists":
			err = writeAll(w, l.lists)
		case "notes":
			err = c.notes(ctx, l, w, docs)
		case "tasks":
			taskWriters[0] = w
		case "task_assignees":
			taskWriters[1] = w
		case "task_records":
			taskWriters[2] = w
		default:
			s, _ := l.source(table.Name)
			err = c.records(ctx, s, w)
		}
		if err != nil {
			return written, err
		}
	}
	if slices.ContainsFunc(taskWriters[:], func(w RowWriter) bool { return w != nil }) {
		return written, c.tasks(ctx, l, taskWriters)
	}
	return written, nil
}

func writeAll(w RowWriter, rows []map[string]any) error {
	for _, row := range rows {
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return nil
}

// attioClient is one read's requests. Attio allows 100 reads a second per
// workspace, which one read stays under; its notes list allows 10 a
// second, which notePace keeps to.
type attioClient struct {
	k        *attioKind
	token    string
	notePace *rate.Limiter
}

func (k *attioKind) client(secret string) *attioClient {
	return &attioClient{k: k, token: strings.TrimSpace(secret), notePace: rate.NewLimiter(rate.Every(attioNotesInterval), 1)}
}

// do sends one request with the token: a GET of path with query, or with
// body a POST of it as JSON. fetch retries a rate limit and an outage;
// Attio's Retry-After is an HTTP date (retryBackoff). A rejected token is
// refused here.
func (c *attioClient) do(ctx context.Context, method, path string, query url.Values, body any) (int, []byte, error) {
	target := c.k.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return 0, nil, err
		}
	}
	paced := path == "/v2/notes"
	status, out, err := fetch(ctx, c.k.http, func() (*http.Request, error) {
		if paced {
			if err := c.notePace.Wait(ctx); err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequest(method, target, bytes.NewReader(payload))
		if err == nil {
			req.Header = http.Header{"Authorization": {"Bearer " + c.token}, "Accept": {"application/json"}}
			if body != nil {
				req.Header.Set("Content-Type", "application/json")
			}
		}
		return req, err
	})
	if err != nil {
		return 0, nil, refused(err, "could not reach Attio; try again")
	}
	if status == http.StatusUnauthorized {
		return 0, nil, attioRejected()
	}
	return status, out, nil
}

// attioRejected is the refusal of a token Attio doesn't know: deleted,
// revoked or mistyped.
func attioRejected() error {
	return rejectedCredential("Attio", nil, "Attio rejected this token; check it and enter it again")
}

// attioDenied is Attio refusing the token one read for its access (403
// auth_error, or billing_error for a feature the workspace's plan lacks),
// with Attio's reason.
type attioDenied struct{ message string }

func (e *attioDenied) Error() string { return "attio denied the read: " + e.message }

// attioRefusal words a refused request with Attio's own message, which
// says what is wrong; its error body is {status_code, type, code, message}.
func attioRefusal(status int, body []byte, path string) error {
	var e struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &e)
	why := strings.TrimSpace(e.Message)
	switch {
	case status == http.StatusForbidden && (e.Type == "auth_error" || e.Type == "billing_error" || e.Code == "billing_error"):
		why = cmp.Or(why, "Attio refused the token access")
		return refused(&attioDenied{message: why}, "this Attio token may not read %s: %s", path, why)
	case status == http.StatusTooManyRequests:
		return refused(nil, "this Attio token has used up its rate limit; Helix will retry")
	case why == "":
		return refused(nil, "Attio returned HTTP %d for %s; try again", status, path)
	}
	return refused(nil, "Attio returned HTTP %d for %s: %s", status, path, why)
}

// get reads one answer of path into out.
func (c *attioClient) get(ctx context.Context, path string, query url.Values, out any) error {
	status, raw, err := c.do(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return err
	}
	if status >= 300 {
		return attioRefusal(status, raw, path)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode attio %s: %w", path, err)
	}
	return nil
}

// data reads a list Attio returns whole, without paging.
func (c *attioClient) data(ctx context.Context, path string) ([]json.RawMessage, error) {
	var resp struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := c.get(ctx, path, nil, &resp); err != nil {
		return nil, err
	}
	if resp.Data == nil {
		return nil, fmt.Errorf("attio %s: the response has no data", path)
	}
	return resp.Data, nil
}

// page reads one page of a paged list: a POST of a query with its limit and
// offset (query), else a GET with them and extra in the URL.
func (c *attioClient) page(ctx context.Context, path string, query bool, limit, offset int, extra url.Values) ([]json.RawMessage, error) {
	var status int
	var raw []byte
	var err error
	if query {
		status, raw, err = c.do(ctx, http.MethodPost, path, nil, map[string]int{"limit": limit, "offset": offset})
	} else {
		q := url.Values{}
		maps.Copy(q, extra)
		q.Set("limit", fmt.Sprint(limit))
		q.Set("offset", fmt.Sprint(offset))
		status, raw, err = c.do(ctx, http.MethodGet, path, q, nil)
	}
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, attioRefusal(status, raw, path)
	}
	var resp struct {
		Data []json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &resp) != nil || resp.Data == nil {
		return nil, fmt.Errorf("attio %s: the response has no complete page of results", path)
	}
	return resp.Data, nil
}

// pages passes every item of a paged list to each, page by page by offset
// until a page shorter than limit: Attio's lists give no total or next
// marker. A list past maxAttioPages fails the read rather than publishing
// part of a table. The pages are offsets into a list that may change while
// it is read, so a row that moves between pages may be missed or read
// twice until the next sync; the readers write a row read twice once.
func (c *attioClient) pages(ctx context.Context, path string, query bool, limit int, extra url.Values, what string,
	each func(json.RawMessage) error) error {
	for n, offset := 0, 0; ; n++ {
		if n == maxAttioPages {
			return invalid("Attio holds more %s than Helix reads in one sync (%d pages of %d)", what, maxAttioPages, limit)
		}
		items, err := c.page(ctx, path, query, limit, offset, extra)
		if err != nil {
			return err
		}
		for _, item := range items {
			if err := each(item); err != nil {
				return err
			}
		}
		if len(items) < limit {
			return nil
		}
		offset += len(items)
	}
}

// attioScopes are the scopes a token holds; nil when Attio's introspection
// named none, which reads as every scope held: Attio refuses what it
// doesn't allow.
type attioScopes map[string]bool

// lacks says which of the scopes the token doesn't hold, as a table's
// unavailable reason, or "" when it holds them all. A :read-write scope
// holds its :read.
func (s attioScopes) lacks(scopes ...string) string {
	var missing []string
	for _, scope := range scopes {
		if s != nil && !s[scope] && !s[strings.TrimSuffix(scope, ":read")+":read-write"] {
			missing = append(missing, scope)
		}
	}
	switch len(missing) {
	case 0:
		return ""
	case 1:
		return "the token lacks the " + missing[0] + " scope"
	}
	return "the token lacks the " + strings.Join(missing[:len(missing)-1], ", ") + " and " + missing[len(missing)-1] + " scopes"
}

// attioListing is what one listing of a workspace found: its slug, the
// token's scopes, the objects and lists table rows, the tables of the
// objects and lists the scopes may list, and each object's slug by id.
type attioListing struct {
	workspace string
	scopes    attioScopes
	objects   []map[string]any
	lists     []map[string]any
	sources   []attioSource
	slugs     map[string]string
}

func (l attioListing) source(table string) (attioSource, bool) {
	i := slices.IndexFunc(l.sources, func(s attioSource) bool { return s.table.Name == table })
	if i < 0 {
		return attioSource{}, false
	}
	return l.sources[i], true
}

// attioSource is an object or a list and its table: one row per record or
// entry, with a column per attribute. Only a source whose rows the token's
// scopes may read has its attributes' columns.
type attioSource struct {
	id      string
	list    bool
	table   Table
	columns []attioColumn
}

func (s attioSource) path() string {
	if s.list {
		return "/v2/lists/" + url.PathEscape(s.id) + "/entries/query"
	}
	return "/v2/objects/" + url.PathEscape(s.id) + "/records/query"
}

// attioColumn is an attribute's column, its values under the attribute's
// slug, and, for a single value, its siblings: the columns of its other
// fields (attioSiblings).
type attioColumn struct {
	slug, kind, name string
	multi            bool
	siblings         []struct{ name, field string }
}

// listing reads the token's introspection, then the objects and lists its
// scopes may list, and the attributes of each whose rows they may read.
// A token Attio doesn't know is answered as inactive, not refused.
func (c *attioClient) listing(ctx context.Context) (attioListing, error) {
	var self struct {
		Active        bool    `json:"active"`
		Scope         *string `json:"scope"`
		WorkspaceSlug string  `json:"workspace_slug"`
	}
	if err := c.get(ctx, "/v2/self", nil, &self); err != nil {
		return attioListing{}, err
	}
	if !self.Active {
		return attioListing{}, attioRejected()
	}
	l := attioListing{workspace: self.WorkspaceSlug, slugs: map[string]string{}}
	if self.Scope != nil {
		l.scopes = attioScopes{}
		for _, scope := range strings.Fields(*self.Scope) {
			l.scopes[scope] = true
		}
	}
	taken := map[string]bool{}
	for _, name := range attioFixed {
		taken[name] = true
	}
	if l.scopes.lacks(attioObjectsScope) == "" {
		items, err := c.data(ctx, "/v2/objects")
		if err != nil {
			return attioListing{}, err
		}
		for _, raw := range items {
			var o struct {
				ID struct {
					ObjectID string `json:"object_id"`
				} `json:"id"`
				APISlug      any `json:"api_slug"`
				SingularNoun any `json:"singular_noun"`
				PluralNoun   any `json:"plural_noun"`
				CreatedAt    any `json:"created_at"`
			}
			if err := json.Unmarshal(raw, &o); err != nil || o.ID.ObjectID == "" {
				return attioListing{}, fmt.Errorf("attio /v2/objects: an object has no id")
			}
			l.objects = append(l.objects, map[string]any{"object_id": o.ID.ObjectID, "api_slug": o.APISlug,
				"singular_noun": o.SingularNoun, "plural_noun": o.PluralNoun, "created_at": o.CreatedAt})
			slug, _ := o.APISlug.(string)
			if slug == "" {
				continue // Attio lets an object have no slug; no table can be named for it
			}
			l.slugs[o.ID.ObjectID] = slug
			s := attioSource{id: o.ID.ObjectID}
			s.table.Name = attioNumbered(taken, cmp.Or(attioSnake(slug), "object"))
			if l.scopes.lacks(attioRecordsScope) == "" {
				if s, err = c.columns(ctx, s); err != nil {
					return attioListing{}, err
				}
			}
			l.sources = append(l.sources, s)
		}
	}
	if l.scopes.lacks(attioListsScope) == "" {
		items, err := c.data(ctx, "/v2/lists")
		if err != nil {
			return attioListing{}, err
		}
		for _, raw := range items {
			var li struct {
				ID struct {
					ListID string `json:"list_id"`
				} `json:"id"`
				APISlug      any      `json:"api_slug"`
				Name         any      `json:"name"`
				ParentObject []string `json:"parent_object"`
				CreatedAt    any      `json:"created_at"`
			}
			if err := json.Unmarshal(raw, &li); err != nil || li.ID.ListID == "" {
				return attioListing{}, fmt.Errorf("attio /v2/lists: a list has no id")
			}
			var parent any
			switch len(li.ParentObject) {
			case 0:
			case 1:
				parent = li.ParentObject[0]
			default:
				parent = li.ParentObject
			}
			l.lists = append(l.lists, map[string]any{"list_id": li.ID.ListID, "api_slug": li.APISlug, "name": li.Name,
				"parent_object": parent, "created_at": li.CreatedAt})
			slug, _ := li.APISlug.(string)
			if slug == "" {
				continue
			}
			s := attioSource{id: li.ID.ListID, list: true}
			s.table.Name = attioNumbered(taken, "list_"+cmp.Or(attioSnake(slug), "list"))
			if l.scopes.lacks(attioEntriesScope) == "" {
				if s, err = c.columns(ctx, s); err != nil {
					return attioListing{}, err
				}
			}
			l.sources = append(l.sources, s)
		}
	}
	return l, nil
}

// columns gives s its table's columns: its fixed ones, then one per
// attribute Attio lists for it that isn't archived, in Attio's order, each
// named from the attribute's slug (attioSnake) and its siblings from that
// name. A name a column already holds takes a number (attioNumbered).
func (c *attioClient) columns(ctx context.Context, s attioSource) (attioSource, error) {
	target, key, fixed := "objects", "record_id", attioRecordColumns
	if s.list {
		target, key, fixed = "lists", "entry_id", attioEntryColumns
	}
	s.table = Table{Name: s.table.Name, Columns: slices.Clone(fixed), PrimaryKey: []string{key}, Dense: true}
	taken := map[string]bool{}
	for _, col := range fixed {
		taken[col.Name] = true
	}
	path := "/v2/" + target + "/" + url.PathEscape(s.id) + "/attributes"
	err := c.pages(ctx, path, false, attioAttributePage, url.Values{"show_archived": {"false"}}, "attributes", func(raw json.RawMessage) error {
		var a struct {
			APISlug       string `json:"api_slug"`
			Type          string `json:"type"`
			IsMultiselect bool   `json:"is_multiselect"`
			IsArchived    bool   `json:"is_archived"`
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return fmt.Errorf("decode attio %s: %w", path, err)
		}
		if a.IsArchived || a.APISlug == "" {
			return nil
		}
		col := attioColumn{slug: a.APISlug, kind: a.Type, multi: a.IsMultiselect,
			name: attioNumbered(taken, cmp.Or(attioSnake(a.APISlug), "attribute"))}
		typ, known := attioTypes[a.Type]
		if col.multi {
			typ = "array"
		}
		s.table.Columns = append(s.table.Columns, Column{Name: col.name, Type: typ, Nullable: !col.multi})
		if !col.multi && known {
			for _, sib := range attioSiblings[a.Type] {
				name := attioNumbered(taken, col.name+sib.suffix)
				col.siblings = append(col.siblings, struct{ name, field string }{name, sib.field})
				s.table.Columns = append(s.table.Columns, Column{Name: name, Type: "string", Nullable: true})
			}
		}
		s.columns = append(s.columns, col)
		return nil
	})
	return s, err
}

// attioSnake names a table or column from an Attio slug: snake case
// (snakeCase), then only ASCII letters, digits and single underscores
// (notionSlug); "" for a slug with none of those.
func attioSnake(slug string) string { return notionSlug(snakeCase(slug)) }

// attioNumbered is name, or when taken holds it, name with the first
// number from 2 up that makes it free; taken then holds it. Objects, lists
// and attributes are numbered in Attio's order.
func attioNumbered(taken map[string]bool, name string) string {
	got := name
	for n := 2; taken[got]; n++ {
		got = fmt.Sprintf("%s_%d", name, n)
	}
	taken[got] = true
	return got
}

// attioRecord is a record or a list entry as a query returns it.
type attioRecord struct {
	ID             map[string]any              `json:"id"`
	CreatedAt      any                         `json:"created_at"`
	WebURL         any                         `json:"web_url"`
	Values         map[string][]map[string]any `json:"values"`
	ParentObject   any                         `json:"parent_object"`
	ParentRecordID any                         `json:"parent_record_id"`
	EntryValues    map[string][]map[string]any `json:"entry_values"`
}

// row is a record's or an entry's row of s's table, and its id: every
// column, null where it has no value.
func (s attioSource) row(raw json.RawMessage) (map[string]any, string, error) {
	var r attioRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, "", fmt.Errorf("decode attio %s: %w", s.path(), err)
	}
	key, values := "record_id", r.Values
	if s.list {
		key, values = "entry_id", r.EntryValues
	}
	id, _ := r.ID[key].(string)
	if id == "" {
		return nil, "", fmt.Errorf("attio %s: a row has no %s", s.path(), key)
	}
	row := map[string]any{key: id, "created_at": r.CreatedAt}
	if s.list {
		row["parent_object"], row["parent_record_id"] = r.ParentObject, r.ParentRecordID
	} else {
		row["web_url"] = r.WebURL
	}
	for _, col := range s.columns {
		attioFill(row, col, values[col.slug])
	}
	return row, id, nil
}

// attioFill sets col's columns of row from the attribute's values: a
// multiselect's column is an array of every value (attioValue), empty when
// there is none; any other's its first value or null, and its siblings
// that value's fields. A value Attio says is no longer active is left out.
func attioFill(row map[string]any, col attioColumn, values []map[string]any) {
	active := slices.DeleteFunc(slices.Clone(values), func(v map[string]any) bool { return v == nil || v["active_until"] != nil })
	if col.multi {
		out := []any{}
		for _, v := range active {
			if value := attioValue(col.kind, v); value != nil {
				out = append(out, value)
			}
		}
		row[col.name] = out
		return
	}
	var v map[string]any
	row[col.name] = nil
	if len(active) > 0 {
		v = active[0]
		row[col.name] = attioValue(col.kind, v)
	}
	for _, sib := range col.siblings {
		row[sib.name] = v[sib.field]
	}
}

// attioValue is one value of an attribute of type kind as its column holds
// it: a plain value's value; a currency's amount; a select's option and a
// status by title; a reference's record or actor id; a domain, email
// address and phone number as the address; a name in full; an
// interaction's time; a location as an object of its fields that aren't
// null (null when none is); and a value of a type Attio adds later as
// Attio gives it, so a new type never fails a sync.
func attioValue(kind string, v map[string]any) any {
	switch kind {
	case "text", "number", "checkbox", "rating", "date", "timestamp":
		return v["value"]
	case "currency":
		return v["currency_value"]
	case "select", "status":
		field := "option"
		if kind == "status" {
			field = "status"
		}
		titled, _ := v[field].(map[string]any)
		return titled["title"]
	case "record-reference":
		return v["target_record_id"]
	case "actor-reference":
		return v["referenced_actor_id"]
	case "domain":
		return v["domain"]
	case "email-address":
		return v["email_address"]
	case "phone-number":
		return v["phone_number"]
	case "personal-name":
		return v["full_name"]
	case "interaction":
		return v["interacted_at"]
	case "location":
		out := map[string]any{}
		for _, field := range attioLocationFields {
			if value := v[field]; value != nil {
				out[field] = value
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return v
}

// records writes every record or entry of s, each once.
func (c *attioClient) records(ctx context.Context, s attioSource, w RowWriter) error {
	seen := map[string]bool{}
	what := "records of " + s.table.Name
	if s.list {
		what = "entries of " + s.table.Name
	}
	return c.pages(ctx, s.path(), true, attioRecordPage, nil, what, func(raw json.RawMessage) error {
		row, id, err := s.row(raw)
		if err != nil || seen[id] {
			return err
		}
		seen[id] = true
		return w.Write(row)
	})
}

// members writes the workspace's members, whom Attio lists whole.
func (c *attioClient) members(ctx context.Context, w RowWriter) error {
	items, err := c.data(ctx, "/v2/workspace_members")
	if err != nil {
		return err
	}
	for _, raw := range items {
		var m struct {
			ID           map[string]any `json:"id"`
			FirstName    any            `json:"first_name"`
			LastName     any            `json:"last_name"`
			EmailAddress any            `json:"email_address"`
			AccessLevel  any            `json:"access_level"`
			CreatedAt    any            `json:"created_at"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("decode attio /v2/workspace_members: %w", err)
		}
		id, _ := m.ID["workspace_member_id"].(string)
		if id == "" {
			return fmt.Errorf("attio /v2/workspace_members: a member has no id")
		}
		if err := w.Write(map[string]any{"workspace_member_id": id, "first_name": m.FirstName, "last_name": m.LastName,
			"email_address": m.EmailAddress, "access_level": m.AccessLevel, "created_at": m.CreatedAt}); err != nil {
			return err
		}
	}
	return nil
}

// tasks writes every task to w[0], its assignees to w[1] and the records
// it is linked to to w[2], skipping a table whose writer is nil, from one
// list of the tasks, oldest first. A linked record's object is named by
// its slug, or by its id for an object the listing has no slug for. An
// assignee without an id has no row: its key would be null.
func (c *attioClient) tasks(ctx context.Context, l attioListing, w [3]RowWriter) error {
	seen := map[string]bool{}
	return c.pages(ctx, "/v2/tasks", false, attioTaskPage, url.Values{"sort": {"created_at:asc"}}, "tasks", func(raw json.RawMessage) error {
		var t struct {
			ID            map[string]any `json:"id"`
			Content       any            `json:"content_plaintext"`
			DeadlineAt    any            `json:"deadline_at"`
			IsCompleted   bool           `json:"is_completed"`
			CompletedAt   any            `json:"completed_at"`
			LinkedRecords []struct {
				TargetObjectID string `json:"target_object_id"`
				TargetRecordID string `json:"target_record_id"`
			} `json:"linked_records"`
			Assignees []struct {
				Type string `json:"referenced_actor_type"`
				ID   string `json:"referenced_actor_id"`
			} `json:"assignees"`
			CreatedBy struct {
				ID   any `json:"id"`
				Type any `json:"type"`
			} `json:"created_by_actor"`
			CreatedAt any `json:"created_at"`
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			return fmt.Errorf("decode attio /v2/tasks: %w", err)
		}
		id, _ := t.ID["task_id"].(string)
		switch {
		case id == "":
			return fmt.Errorf("attio /v2/tasks: a task has no id")
		case seen[id]:
			return nil
		}
		seen[id] = true
		if w[0] != nil {
			if err := w[0].Write(map[string]any{"task_id": id, "content": t.Content, "deadline_at": t.DeadlineAt,
				"is_completed": t.IsCompleted, "completed_at": t.CompletedAt, "created_by_type": t.CreatedBy.Type,
				"created_by_id": t.CreatedBy.ID, "created_at": t.CreatedAt}); err != nil {
				return err
			}
		}
		if w[1] != nil {
			written := map[[2]string]bool{}
			for _, a := range t.Assignees {
				if a.ID == "" || written[[2]string{a.Type, a.ID}] {
					continue
				}
				written[[2]string{a.Type, a.ID}] = true
				if err := w[1].Write(map[string]any{"task_id": id, "assignee_type": a.Type, "assignee_id": a.ID}); err != nil {
					return err
				}
			}
		}
		if w[2] != nil {
			written := map[[2]string]bool{}
			for _, r := range t.LinkedRecords {
				object := cmp.Or(l.slugs[r.TargetObjectID], r.TargetObjectID)
				if r.TargetRecordID == "" || written[[2]string{object, r.TargetRecordID}] {
					continue
				}
				written[[2]string{object, r.TargetRecordID}] = true
				if err := w[2].Write(map[string]any{"task_id": id, "target_object": object, "target_record_id": r.TargetRecordID}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// attioNote is a listed note: its row, what orders the read and versions
// its document, and, while it is a candidate for one, its body.
type attioNote struct {
	id, title, url, body string
	created              time.Time
	version              string
	row                  map[string]any
	// empty is a note whose body is blank, which has no document;
	// candidate one of the newest config.MaxDocumentsPerStream others, the
	// most a full read keeps, whose body is held until it is offered.
	empty, candidate bool
}

// attioNoteOf reads a listed note. One without its id or creation time
// fails the read: they key its row and order its document.
func attioNoteOf(raw json.RawMessage, workspace string) (*attioNote, error) {
	var item struct {
		ID              map[string]any `json:"id"`
		ParentObject    any            `json:"parent_object"`
		ParentRecordID  any            `json:"parent_record_id"`
		Title           string         `json:"title"`
		MeetingID       any            `json:"meeting_id"`
		ContentMarkdown string         `json:"content_markdown"`
		CreatedBy       struct {
			ID   any `json:"id"`
			Type any `json:"type"`
		} `json:"created_by_actor"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, fmt.Errorf("decode attio /v2/notes: %w", err)
	}
	n := &attioNote{title: item.Title, body: item.ContentMarkdown, empty: strings.TrimSpace(item.ContentMarkdown) == ""}
	n.id, _ = item.ID["note_id"].(string)
	if n.id == "" {
		return nil, fmt.Errorf("attio /v2/notes: a listed note has no id")
	}
	var err error
	if n.created, err = time.Parse(time.RFC3339Nano, item.CreatedAt); err != nil {
		return nil, fmt.Errorf("attio /v2/notes: note %s is listed without its created_at", n.id)
	}
	sum := sha256.Sum256([]byte(item.Title + "\x00" + item.ContentMarkdown))
	n.version = "1|" + hex.EncodeToString(sum[:])[:32]
	parentObject, _ := item.ParentObject.(string)
	parentRecord, _ := item.ParentRecordID.(string)
	if workspace != "" && parentObject != "" && parentRecord != "" {
		n.url = "https://app.attio.com/" + url.PathEscape(workspace) + "/" + url.PathEscape(parentObject) + "/" + url.PathEscape(parentRecord)
	}
	n.row = map[string]any{"note_id": n.id, "parent_object": item.ParentObject, "parent_record_id": item.ParentRecordID,
		"title": orNull(item.Title), "meeting_id": item.MeetingID, "created_by_type": item.CreatedBy.Type,
		"created_by_id": item.CreatedBy.ID, "created_at": item.CreatedAt}
	return n, nil
}

// attioNoteOrder is the order a read offers its notes' documents in: the
// newest first, since a full read keeps the first documents it offers up to
// its cap; then the lower id.
func attioNoteOrder(a, b *attioNote) int {
	return cmp.Or(b.created.Compare(a.created), strings.Compare(a.id, b.id))
}

// attioNoteHeap is a heap of notes, the last in attioNoteOrder on top.
type attioNoteHeap []*attioNote

func (h attioNoteHeap) Len() int           { return len(h) }
func (h attioNoteHeap) Less(i, j int) bool { return attioNoteOrder(h[i], h[j]) > 0 }
func (h attioNoteHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *attioNoteHeap) Push(n any)        { *h = append(*h, n.(*attioNote)) }
func (h *attioNoteHeap) Pop() any {
	n := (*h)[len(*h)-1]
	*h = (*h)[:len(*h)-1]
	return n
}

// notes lists every note, each once, then writes their rows and offers
// their documents newest first (attioNoteOrder). Attio returns each note's
// body with it and documents no order, so the read lists them all before
// offering any; it holds the bodies of the newest
// config.MaxDocumentsPerStream notes with one, the most a full read keeps,
// and no others.
func (c *attioClient) notes(ctx context.Context, l attioListing, w RowWriter, docs DocumentWriter) error {
	var all []*attioNote
	var candidates attioNoteHeap
	seen := map[string]bool{}
	err := c.pages(ctx, "/v2/notes", false, attioNotePage, nil, "notes", func(raw json.RawMessage) error {
		n, err := attioNoteOf(raw, l.workspace)
		if err != nil || seen[n.id] {
			return err
		}
		seen[n.id] = true
		all = append(all, n)
		if n.empty {
			n.body = ""
			return nil
		}
		n.candidate = true
		heap.Push(&candidates, n)
		if len(candidates) > config.MaxDocumentsPerStream {
			last := heap.Pop(&candidates).(*attioNote)
			last.candidate, last.body = false, ""
		}
		return nil
	})
	if err != nil {
		return err
	}
	slices.SortFunc(all, attioNoteOrder)
	for _, n := range all {
		if err := w.Write(n.row); err != nil {
			return err
		}
		if err := n.offer(ctx, docs); err != nil {
			return err
		}
	}
	return nil
}

// offer offers n's document to docs and Puts it when docs wants it. A note
// whose body is blank has none: it takes back the one the stream holds,
// and is otherwise not offered, so it takes no place under the cap.
func (n *attioNote) offer(ctx context.Context, docs DocumentWriter) error {
	stream := attioNotesStream.Name
	switch {
	case n.empty:
		if _, held := docs.Held(stream, n.id); held && docs.Want(stream, n.id, n.version) {
			return docs.Drop(stream, n.id)
		}
		return nil
	case !n.candidate || !docs.Want(stream, n.id, n.version):
		return nil
	}
	return docs.Put(ctx, stream, n.document())
}

// document is a note's document: its title as a heading, "Untitled note"
// for none, then its Markdown body as Attio gives it. Its time is its
// creation's: Attio keeps no edit time.
func (n *attioNote) document() Document {
	title := strings.Join(strings.Fields(n.title), " ")
	heading := cmp.Or(title, "Untitled note")
	return Document{ExternalID: n.id, Version: n.version, At: n.created, Title: heading, URL: n.url,
		Text: []byte("# " + heading + "\n\n" + n.body)}
}

// attioRemembered is the listing a sync's Discover made, which the same
// sync's SyncPass reads rather than listing again, for attioListingFresh:
// a sync names its tables as its discovery did.
type attioRemembered struct {
	// secret is the SHA-256 of the token it was listed with: the kind
	// serves every connector, so it keeps no token past a read.
	secret  [sha256.Size]byte
	at      time.Time
	listing attioListing
}

// attioListingFresh is how long a sync's discovered listing serves its
// reads: past it, a read lists again, so an object deleted meanwhile isn't
// queried.
const attioListingFresh = time.Minute

// remember keeps what a sync's Discover listed for the connector, and
// forgets the listings no read may use any longer.
func (k *attioKind) remember(connectorID, token string, l attioListing) {
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	maps.DeleteFunc(k.listings, func(_ string, e attioRemembered) bool { return now.Sub(e.at) >= attioListingFresh })
	if k.listings == nil {
		k.listings = map[string]attioRemembered{}
	}
	k.listings[connectorID] = attioRemembered{secret: sha256.Sum256([]byte(token)), at: now, listing: l}
}

// listed is the listing a sync of src reads: the one its Discover made
// (remember), while fresh and for the same token, else a new one.
func (k *attioKind) listed(ctx context.Context, c *attioClient, src Source) (attioListing, error) {
	if src.ForSync && src.ConnectorID != "" {
		k.mu.Lock()
		e, ok := k.listings[src.ConnectorID]
		k.mu.Unlock()
		if ok && e.secret == sha256.Sum256([]byte(c.token)) && time.Since(e.at) < attioListingFresh {
			return e.listing, nil
		}
	}
	return c.listing(ctx)
}
