package connector

import (
	"bytes"
	"cmp"
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/text/unicode/norm"
	"golang.org/x/time/rate"

	"github.com/helixdb/helix-foundry/backend/config"
)

var _ PassKind = (*notionKind)(nil)

// notionSecretRE takes an internal integration's secret: ntn_ since
// September 2024, secret_ before.
var notionSecretRE = regexp.MustCompile(`^(ntn|secret)_[A-Za-z0-9]+$`)

const (
	// notionVersion is the API version every request asks for. Since
	// 2025-09-03 a database holds data sources, whose rows are pages.
	notionVersion = "2026-03-11"
	// notionPageSize is the most any Notion list returns per page, and
	// notionPreviewRows the rows a test shows of each of the first
	// notionPreviews data sources.
	notionPageSize    = 100
	notionPreviewRows = 5
	notionPreviews    = 20
	// notionMinPageSize is the fewest results list asks for of a page Notion
	// could not build in time.
	notionMinPageSize = 10
	// notionMinWindow is the shortest span of creation time rows splits a
	// data source's query into.
	notionMinWindow = time.Second
	// maxNotionName is the longest name, in bytes, a table or column takes
	// from a Notion title, before any suffix that makes it unique.
	maxNotionName = 63
)

// notionInterval spaces one read's requests. Notion allows an average of 3
// requests a second per integration, and data source queries 3 a second
// of their own; one read keeps to 3 in all. Tests set it to 0.
var notionInterval = time.Second / 3

// maxNotionPages bounds one list, at 100 results a page. Tests lower it.
var maxNotionPages = 20_000

// notionWait is how long do waits before a retry: the Retry-After Notion
// asks for, up to a minute, as Notion's own SDK waits (an integration's
// limit asks for at most a minute; a workspace's may ask for more, which
// the retries wait out a minute at a time), else retryBackoff's doubling.
// Tests replace it.
var notionWait = func(attempt int, retryAfter string) time.Duration {
	if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds > 0 {
		return min(time.Duration(seconds)*time.Second, time.Minute)
	}
	return retryBackoff(attempt, "")
}

// notionReread is how often a page's document is read again though the
// page shows no edit, and notionNow the clock of a read's versions, which
// tests replace.
const notionReread = 7 * 24 * time.Hour

var notionNow = time.Now

// errNotionSlow is a page of results Notion could not build in time, and
// errNotionEnough a list read as far as it needs.
var (
	errNotionSlow   = errors.New("notion could not answer in time")
	errNotionEnough = errors.New("notion list read far enough")
)

// notionShownReferences is the most ids of a relation or people value a
// page shows, and maxNotionReferences the most references reads of one.
const (
	notionShownReferences = 25
	maxNotionReferences   = 1000
)

// The fixed tables' columns, and those every data source's table starts
// with. Columns are typed by the JSON they hold, as an API source's are;
// every time is an RFC 3339 string in UTC.
var (
	notionUserColumns = []Column{{Name: "user_id", Type: "string"}, {Name: "type", Type: "string"},
		{Name: "name", Type: "string", Nullable: true}, {Name: "email", Type: "string", Nullable: true}}
	notionPageColumns = []Column{{Name: "page_id", Type: "string"}, {Name: "title", Type: "string", Nullable: true},
		{Name: "url", Type: "string", Nullable: true}, {Name: "parent_type", Type: "string", Nullable: true},
		{Name: "parent_id", Type: "string", Nullable: true}, {Name: "data_source_id", Type: "string", Nullable: true},
		{Name: "created_at", Type: "string"}, {Name: "last_edited_at", Type: "string"},
		{Name: "created_by_id", Type: "string", Nullable: true}, {Name: "last_edited_by_id", Type: "string", Nullable: true}}
	notionRowColumns = []Column{{Name: "page_id", Type: "string"}, {Name: "url", Type: "string", Nullable: true},
		{Name: "created_at", Type: "string"}, {Name: "last_edited_at", Type: "string"}}
)

// notionTypes are the property types a data source's table has a column
// for, and the JSON type of its values; a formula's and a rollup's follow
// their result, and a unique id's its prefix, so they have none.
// Verification, button, place and any type Notion adds later have no column.
var notionTypes = map[string]string{
	"title": "string", "rich_text": "string", "number": "number", "checkbox": "boolean",
	"select": "string", "status": "string", "multi_select": "array", "date": "string",
	"people": "array", "relation": "array", "files": "array",
	"url": "string", "email": "string", "phone_number": "string", "formula": "", "rollup": "",
	"created_by": "string", "last_edited_by": "string", "created_time": "string", "last_edited_time": "string",
	"unique_id": "",
}

// notionPagesStream is every shared page's Markdown, kept on the pages table.
var notionPagesStream = Stream{Name: "pages", Table: "pages", Title: "Pages", Format: Markdown,
	Description: "Each Notion page shared with the integration, database rows included, as Markdown: its title and " +
		"page id, then the page's content as Notion exports it."}

type notionKind struct {
	base string
	http *http.Client
}

func (k *notionKind) Name() string        { return "notion" }
func (k *notionKind) DisplayName() string { return "Notion" }

func (k *notionKind) Prepare(src Source) (json.RawMessage, error) {
	if !notionSecretRE.MatchString(strings.TrimSpace(src.Secret)) {
		return nil, invalid("enter a Notion integration secret (ntn_…)")
	}
	if err := emptySettings(src.Settings); err != nil {
		return nil, err
	}
	return json.RawMessage(`{}`), nil
}

// Streams are the pages, always.
func (k *notionKind) Streams(Source) []Stream { return []Stream{notionPagesStream} }

func (k *notionKind) Hint(secret string) string { return keyHint(strings.TrimSpace(secret), 1) }

// Discover proves the secret on the integration's own user, then probes the
// users list and a search each of pages and data sources, one result each.
// A secret that may not read users makes the users table unavailable; one
// shared with nothing is refused. Every shared data source is a table, its
// columns from its schema; a test shows a few rows of each of the oldest 20.
func (k *notionKind) Discover(ctx context.Context, src Source) (Discovery, error) {
	c := k.client(src.Secret)
	status, _, err := c.do(ctx, "/v1/users/me", nil, nil)
	if err != nil {
		return Discovery{}, err
	}
	if err := notionStatus(status, "/v1/users/me"); err != nil {
		return Discovery{}, err
	}
	found, err := probeAll(ctx, []string{"users", "page", "data_source"}, func(ctx context.Context, probe string) (bool, error) {
		if probe != "users" {
			l, err := c.page(ctx, "/v1/search", nil, map[string]any{"filter": map[string]any{"property": "object", "value": probe}, "page_size": 1})
			return len(l.Results) > 0, err
		}
		status, _, err := c.do(ctx, "/v1/users", url.Values{"page_size": {"1"}}, nil)
		switch {
		case err != nil:
			return false, err
		case status == http.StatusForbidden:
			return false, nil
		}
		return true, notionStatus(status, "/v1/users")
	})
	if err != nil {
		return Discovery{}, err
	}
	if !found["page"] && !found["data_source"] {
		return Discovery{}, refused(nil, "nothing is shared with this integration yet: in Notion, add it to a page or database "+
			"from its ••• menu › Connections, then test again")
	}
	var d Discovery
	pages := Table{Name: "pages", Columns: notionPageColumns, PrimaryKey: []string{"page_id"}, Dense: true}
	if found["users"] {
		d.Tables = append(d.Tables, Table{Name: "users", Columns: notionUserColumns, PrimaryKey: []string{"user_id"}, Dense: true})
		pages.ForeignKeys = []ForeignKey{foreignKey("created_by_id", "users", "user_id"), foreignKey("last_edited_by_id", "users", "user_id")}
	} else {
		d.Unavailable = append(d.Unavailable, Unavailable{Name: "users", Reason: "turn on Read user information in the integration's capabilities"})
	}
	d.Tables = append(d.Tables, pages)
	if !found["data_source"] {
		return d, nil
	}
	sources, err := c.sources(ctx)
	if err != nil {
		return Discovery{}, err
	}
	for i, s := range sources {
		t := s.table
		if !src.ForSync && i < notionPreviews {
			l, err := c.page(ctx, s.path(), nil, map[string]any{"page_size": notionPreviewRows, "result_type": "page"})
			if err != nil {
				return Discovery{}, err
			}
			for _, item := range l.Results[:min(len(l.Results), notionPreviewRows)] {
				p, err := notionPageOf(s.path(), item)
				if err != nil {
					return Discovery{}, err
				}
				values, _ := s.row(p, item)    // a preview shows the references a row shows
				row, _ := json.Marshal(values) // values decoded from JSON always encode
				t.Preview = append(t.Preview, row)
			}
		}
		d.Tables = append(d.Tables, t)
	}
	return d, nil
}

// Sync and SyncDocuments read one table through SyncPass. The syncer
// reads a PassKind's tables in one pass and calls neither.
func (k *notionKind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	return k.SyncDocuments(ctx, src, table, w, noDocuments{})
}

func (k *notionKind) SyncDocuments(ctx context.Context, src Source, table Table, rows RowWriter, docs DocumentWriter) (Table, error) {
	written, err := k.SyncPass(ctx, src, []Table{table}, map[string]RowWriter{table.Name: rows}, docs)
	return written[0], err
}

// SyncPass reads users from their list, and each data source's table from
// a query of its rows. The pages table is the rows of every shared data
// source, from those queries (of their titles alone where the table isn't
// read), and every other page from a search, which Notion doesn't promise
// is complete; one Markdown read per page whose document is wanted. A table
// names its data source only by the name Discover gave it, so the pass
// searches the data sources again and names them the same way; a data
// source renamed in Notion is a new table, and its old one goes. Each data
// source's table is returned with the columns of the schema this search
// read.
func (k *notionKind) SyncPass(ctx context.Context, src Source, tables []Table, rows map[string]RowWriter, docs DocumentWriter) ([]Table, error) {
	c := k.client(src.Secret)
	written := slices.Clone(tables)
	var pages *notionPages
	read := map[string]int{} // the data sources' tables read, by name, at their place in tables
	for i, table := range tables {
		switch table.Name {
		case "users":
			if err := c.users(ctx, rows[table.Name]); err != nil {
				return written, err
			}
		case "pages":
			pages = &notionPages{w: rows[table.Name]}
		default:
			read[table.Name] = i
		}
	}
	if pages == nil && len(read) == 0 {
		return written, nil
	}
	sources, err := c.sources(ctx)
	if err != nil {
		return written, err
	}
	for _, table := range tables {
		if _, ok := read[table.Name]; ok && !slices.ContainsFunc(sources, func(s notionSource) bool { return s.table.Name == table.Name }) {
			return written, refused(nil, "Notion no longer shares the database of table %s with this integration; try again", table.Name)
		}
	}
	queried := map[string]bool{}
	for _, s := range sources {
		i, ok := read[s.table.Name]
		if !ok && pages == nil {
			continue
		}
		queried[s.id] = true
		var w RowWriter
		query := url.Values{"filter_properties": {"title"}}
		if ok {
			written[i], w, query = s.table, rows[s.table.Name], nil
		}
		err := c.rows(ctx, s, query, time.Time{}, func(p notionPage, item map[string]any) error {
			if pages != nil {
				if err := pages.add(p); err != nil {
					return err
				}
			}
			if w == nil {
				return nil
			}
			row, err := c.tableRow(ctx, s, p, item)
			if err != nil {
				return err
			}
			return w.Write(row)
		})
		if err != nil {
			return written, err
		}
	}
	if pages == nil {
		return written, nil
	}
	return written, c.pages(ctx, pages, queried, docs)
}

// notionClient is one read's requests, paced together under Notion's rate
// limit, and the time its documents are versioned at.
type notionClient struct {
	k      *notionKind
	secret string
	pace   *rate.Limiter
	now    time.Time
}

func (k *notionKind) client(secret string) *notionClient {
	return &notionClient{k: k, secret: strings.TrimSpace(secret), pace: rate.NewLimiter(rate.Every(notionInterval), 1), now: notionNow()}
}

// do sends one Notion request, paced: a GET of path with query, or with
// body a POST of it as JSON. It retries a rate limit and an outage up to
// maxRetries times, waiting notionWait. A request for more than
// notionMinPageSize results that Notion could not answer in time (503,
// 504) is not retried here: Notion's remedy is a smaller page_size, which
// list asks for (errNotionSlow). A rate limit that outlasts the retries is
// refused with when to try again, so the sync retries rather than failing
// for good; Notion's Retry-After is in seconds. An answer Notion could not
// give (5xx) is an outage, like no answer.
func (c *notionClient) do(ctx context.Context, path string, query url.Values, body map[string]any) (int, []byte, error) {
	target := c.k.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	method, payload := http.MethodGet, []byte(nil)
	size, _ := strconv.Atoi(query.Get("page_size"))
	if body != nil {
		method = http.MethodPost
		payload, _ = json.Marshal(body) // strings, numbers and maps of them always encode
		size, _ = body["page_size"].(int)
	}
	for attempt := 0; ; attempt++ {
		if err := c.pace.Wait(ctx); err != nil {
			return 0, nil, refused(err, "could not reach Notion; try again")
		}
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(payload))
		if err != nil {
			return 0, nil, err
		}
		req.Header = http.Header{"Authorization": {"Bearer " + c.secret}, "Notion-Version": {notionVersion}}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		res, err := c.k.http.Do(req)
		if err != nil {
			return 0, nil, refused(err, "could not reach Notion; try again")
		}
		raw, err := io.ReadAll(io.LimitReader(res.Body, maxPageBytes))
		res.Body.Close()
		if err != nil {
			return 0, nil, refused(err, "could not reach Notion; try again")
		}
		status, retryAfter := res.StatusCode, res.Header.Get("Retry-After")
		switch {
		case (status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout) && size > notionMinPageSize:
			return 0, nil, refused(fmt.Errorf("HTTP %d for %s: %w", status, path, errNotionSlow), "could not reach Notion; try again")
		case (status == http.StatusTooManyRequests || status >= 500) && attempt < maxRetries:
			select {
			case <-ctx.Done():
				return 0, nil, refused(ctx.Err(), "could not reach Notion; try again")
			case <-time.After(notionWait(attempt, retryAfter)):
			}
			continue
		case status == http.StatusTooManyRequests:
			when := "in a few minutes"
			if seconds, err := strconv.Atoi(retryAfter); err == nil && seconds > 0 {
				when = "after " + time.Now().Add(time.Duration(seconds)*time.Second).UTC().Format("15:04 UTC")
			}
			return 0, nil, refused(nil, "this integration has used up its Notion rate limit; try again %s", when)
		case status >= 500:
			return 0, nil, refused(fmt.Errorf("HTTP %d for %s", status, path), "could not reach Notion; try again")
		}
		return status, raw, nil
	}
}

// notionStatus refuses an answer that is not a success, with its status.
func notionStatus(status int, path string) error {
	switch {
	case status == http.StatusUnauthorized:
		return refused(notionHTTPError(status), "Notion rejected this secret; check it and enter it again")
	case status == http.StatusForbidden || status == http.StatusNotFound:
		return refused(notionHTTPError(status), "this integration cannot read %s; check its capabilities in Notion and test again", path)
	case status >= 300:
		return refused(notionHTTPError(status), "Notion returned HTTP %d for %s", status, path)
	}
	return nil
}

// notionHTTPError is the status of an answer notionStatus refused.
type notionHTTPError int

func (e notionHTTPError) Error() string { return "HTTP " + strconv.Itoa(int(e)) }

// notionOneRefused says Notion refused the one object a request asked
// for, not the read: a 4xx but 401, the secret's, and 429, which do
// refuses first.
func notionOneRefused(status int) bool {
	return status >= 400 && status < 500 && status != http.StatusUnauthorized && status != http.StatusTooManyRequests
}

// notionList is one page of a Notion list. Notion says a list it stopped
// at its result limit is incomplete, on a last page.
type notionList struct {
	Results       []map[string]any `json:"results"`
	HasMore       *bool            `json:"has_more"`
	NextCursor    *string          `json:"next_cursor"`
	RequestStatus struct {
		Type string `json:"type"`
	} `json:"request_status"`
}

// page reads one page of a list: a GET of path with query, or a POST of body.
func (c *notionClient) page(ctx context.Context, path string, query url.Values, body map[string]any) (notionList, error) {
	status, raw, err := c.do(ctx, path, query, body)
	if err != nil {
		return notionList{}, err
	}
	if err := notionStatus(status, path); err != nil {
		return notionList{}, err
	}
	var l notionList
	if json.Unmarshal(raw, &l) != nil || l.Results == nil || l.HasMore == nil {
		return notionList{}, fmt.Errorf("notion %s: the response has no complete page of results", path)
	}
	return l, nil
}

// list pages through a Notion list, a GET with its cursor in query without
// body, else a POST with it in body, and passes each result to each. It
// says whether Notion cut the list at its result limit. An incomplete page
// fails the attempt: ending early would publish a partial table. A page
// Notion could not build in time is asked for again with half the results,
// down to notionMinPageSize, for the rest of the list.
func (c *notionClient) list(ctx context.Context, path string, query url.Values, body map[string]any, each func(map[string]any) error) (cut bool, err error) {
	query, body = maps.Clone(query), maps.Clone(body)
	if query == nil {
		query = url.Values{}
	}
	set := func(key string, value any) {
		if body == nil {
			query.Set(key, fmt.Sprint(value))
		} else {
			body[key] = value
		}
	}
	size, cursor := notionPageSize, ""
	set("page_size", size)
	for page := 0; ; page++ {
		if page == maxNotionPages {
			return false, fmt.Errorf("notion %s: more than %d pages", path, maxNotionPages)
		}
		l, err := c.page(ctx, path, query, body)
		if errors.Is(err, errNotionSlow) {
			size = max(size/2, notionMinPageSize)
			set("page_size", size)
			page--
			continue
		}
		if err != nil {
			return false, err
		}
		next := ""
		if l.NextCursor != nil {
			next = *l.NextCursor
		}
		if *l.HasMore && (next == "" || next == cursor) {
			return false, fmt.Errorf("notion %s: a page says more follow but gives no new cursor", path)
		}
		for _, item := range l.Results {
			if err := each(item); err != nil {
				return false, err
			}
		}
		if !*l.HasMore {
			return l.RequestStatus.Type == "incomplete", nil
		}
		cursor = next
		set("start_cursor", cursor)
	}
}

// search lists every object filter names, of a type (its value: "page" or
// "data_source"), by last edit in direction, "ascending" or "descending",
// and passes each to each: one edited while an ascending search is paged
// moves to its end rather than out of it, so it may come twice. A partial
// object, its id alone, is read whole first, with query
// (filter_properties), and left out if Notion refuses it
// (notionOneRefused). Notion lists no trashed object unless the filter
// asks for the trash (in_trash), and says when it cut a search at its
// result limit, which fails the read: what it left out would go.
func (c *notionClient) search(ctx context.Context, filter map[string]any, direction string, query url.Values, each func(item map[string]any) error) error {
	object, _ := filter["value"].(string)
	cut, err := c.list(ctx, "/v1/search", query, map[string]any{
		"filter": filter,
		"sort":   map[string]any{"timestamp": "last_edited_time", "direction": direction},
	}, func(item map[string]any) error {
		id, _ := item["id"].(string)
		if id == "" {
			return fmt.Errorf("notion /v1/search: a listed %s has no id", object)
		}
		if _, full := item["properties"]; full {
			return each(item)
		}
		path := "/v1/" + object + "s/" + url.PathEscape(id)
		status, raw, err := c.do(ctx, path, query, nil)
		switch {
		case err != nil:
			return err
		case notionOneRefused(status):
			return nil
		}
		if err := notionStatus(status, path); err != nil {
			return err
		}
		var whole map[string]any
		if json.Unmarshal(raw, &whole) != nil || whole["id"] != id {
			return fmt.Errorf("notion %s: the response is not the %s", path, strings.ReplaceAll(object, "_", " "))
		}
		return each(whole)
	})
	switch {
	case err != nil:
		return err
	case cut:
		return invalid("Notion stopped listing the %ss shared with this integration at its result limit; share fewer with it",
			strings.ReplaceAll(object, "_", " "))
	}
	return nil
}

// users writes the users table. Notion lists the workspace's people and
// bots, not its guests, and a person's email only when the integration may
// read emails.
func (c *notionClient) users(ctx context.Context, w RowWriter) error {
	_, err := c.list(ctx, "/v1/users", nil, nil, func(u map[string]any) error { // the users list has no result limit
		id, _ := u["id"].(string)
		if id == "" {
			return fmt.Errorf("notion /v1/users: a listed user has no id")
		}
		person, _ := u["person"].(map[string]any)
		return w.Write(map[string]any{"user_id": id, "type": u["type"], "name": u["name"], "email": person["email"]})
	})
	return err
}

// notionPage is a listed page: what its row and document need.
type notionPage struct {
	id, title, url                     string
	parentType, parentID, dataSourceID string
	createdBy, editedBy                string
	created, edited                    time.Time
}

// version is the version of the page's document as of now: a render
// revision, to bump whenever its text changes for the same page; the
// page's last edit; and the week of its re-read. Notion keeps an edit to
// the minute, so a second edit in the minute of a sync shows at the page's
// next edit or re-read. A page shows content from elsewhere (a synced
// block, a child page's title, a mention) that changes no edit of its own,
// so its version also moves once every notionReread, at an offset its id
// sets so a workspace's pages are not all read again in one sync; read
// again unchanged, its document is updated in place, not extracted again.
func (p notionPage) version(now time.Time) string {
	sum := sha256.Sum256([]byte(p.id))
	period := int64(notionReread / time.Second)
	offset := int64(binary.BigEndian.Uint64(sum[:8]) % uint64(period))
	return fmt.Sprintf("2|%s|%d", p.edited.UTC().Format(time.RFC3339Nano), (now.Unix()+offset)/period)
}

// notionPageOf reads a listed page, or a data source's row. One without
// both times fails the read: they order the read and version the page's document.
func notionPageOf(path string, item map[string]any) (notionPage, error) {
	p := notionPage{title: notionTitle(item["properties"])}
	p.id, _ = item["id"].(string)
	if p.id == "" {
		return p, fmt.Errorf("notion %s: a listed page has no id", path)
	}
	var errCreated, errEdited error
	p.created, errCreated = notionTime(item["created_time"])
	p.edited, errEdited = notionTime(item["last_edited_time"])
	if errCreated != nil || errEdited != nil {
		return p, fmt.Errorf("notion %s: page %s is listed without its created_time and last_edited_time", path, p.id)
	}
	p.url, _ = item["url"].(string)
	parent, _ := item["parent"].(map[string]any)
	kind, _ := parent["type"].(string)
	// workspace, page_id, data_source_id or block_id; a workspace's value is true.
	p.parentType = strings.TrimSuffix(kind, "_id")
	p.parentID, _ = parent[kind].(string)
	p.dataSourceID, _ = parent["data_source_id"].(string)
	createdBy, _ := item["created_by"].(map[string]any)
	editedBy, _ := item["last_edited_by"].(map[string]any)
	p.createdBy, _ = createdBy["id"].(string)
	p.editedBy, _ = editedBy["id"].(string)
	return p, nil
}

// row is the page's row of the pages table.
func (p notionPage) row() map[string]any {
	return map[string]any{
		"page_id": p.id, "title": orNull(p.title), "url": orNull(p.url),
		"parent_type": orNull(p.parentType), "parent_id": orNull(p.parentID), "data_source_id": orNull(p.dataSourceID),
		"created_at": p.created.UTC().Format(time.RFC3339), "last_edited_at": p.edited.UTC().Format(time.RFC3339),
		"created_by_id": orNull(p.createdBy), "last_edited_by_id": orNull(p.editedBy),
	}
}

// notionPages is the pages table of one read, its rows written as their
// pages are listed, but for the candidates for a document: the first
// config.MaxDocumentsPerStream in notionOrder, the most a full read keeps,
// held in a heap whose top is the last, and written once their Markdown is
// read (offer). So a read holds that many pages, not all of them.
type notionPages struct {
	w          RowWriter
	candidates notionHeap
	listed     int
}

func (ps *notionPages) add(p notionPage) error {
	ps.listed++
	switch {
	case len(ps.candidates) < config.MaxDocumentsPerStream:
		heap.Push(&ps.candidates, p)
		return nil
	case notionOrder(p, ps.candidates[0]) > 0:
		return ps.w.Write(p.row())
	}
	last := ps.candidates[0]
	ps.candidates[0] = p
	heap.Fix(&ps.candidates, 0)
	return ps.w.Write(last.row())
}

// notionOrder is the order a read offers its pages' documents in: pages
// that aren't database rows first, since a row more often has an empty
// body, which takes a place under the cap and is read on every sync, as it
// has no document to hold its version; then the newest edit, since a full
// read keeps the first documents it offers up to its cap; then the lower id.
func notionOrder(a, b notionPage) int {
	if aRow, bRow := a.parentType == "data_source", b.parentType == "data_source"; aRow != bRow {
		if aRow {
			return 1
		}
		return -1
	}
	return cmp.Or(b.edited.Compare(a.edited), strings.Compare(a.id, b.id))
}

// notionHeap is a heap of pages, the last in notionOrder on top.
type notionHeap []notionPage

func (h notionHeap) Len() int           { return len(h) }
func (h notionHeap) Less(i, j int) bool { return notionOrder(h[i], h[j]) > 0 }
func (h notionHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *notionHeap) Push(p any)        { *h = append(*h, p.(notionPage)) }
func (h *notionHeap) Pop() any {
	p := (*h)[len(*h)-1]
	*h = (*h)[:len(*h)-1]
	return p
}

// notionPagesRead counts the bodies of Notion pages syncs read.
var notionPagesRead = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "foundry_connector_notion_pages_total",
	Help: "Notion pages whose Markdown a sync read, by outcome: read, empty, gone (Notion no longer finds it) or refused (Notion refused that page's body).",
}, []string{"outcome"})

// pages adds every page a search lists to ps but the rows of the data
// sources the read queried, which their queries listed, then offers their
// documents. listed is the ids the search passed, so a page it lists twice
// is added once, at its first listing; its next sync reads the edit that
// moved it.
func (c *notionClient) pages(ctx context.Context, ps *notionPages, queried map[string]bool, docs DocumentWriter) error {
	listed := map[string]bool{}
	err := c.search(ctx, map[string]any{"property": "object", "value": "page"}, "ascending", url.Values{"filter_properties": {"title"}}, func(item map[string]any) error {
		p, err := notionPageOf("/v1/search", item)
		switch {
		case err != nil:
			return err
		case queried[p.dataSourceID] || listed[p.id]:
			return nil
		}
		listed[p.id] = true
		return ps.add(p)
	})
	if err != nil {
		return err
	}
	return c.offer(ctx, ps, docs)
}

// offer offers each candidate's document to docs in notionOrder (document)
// and writes its row. A page Notion no longer finds is left out, and so is
// its document. Notion finding none of the pages it listed is Notion
// failing, not every page going between the two: an empty table would drop
// them all.
func (c *notionClient) offer(ctx context.Context, ps *notionPages, docs DocumentWriter) error {
	missing := 0
	for _, p := range slices.SortedFunc(slices.Values(ps.candidates), notionOrder) {
		gone, err := c.document(ctx, p, docs)
		switch {
		case err != nil:
			return err
		case gone:
			missing++
			continue
		}
		if err := ps.w.Write(p.row()); err != nil {
			return err
		}
	}
	if missing > 0 && missing == ps.listed {
		return refused(nil, "Notion listed %d pages but returned none of them; try again", missing)
	}
	return nil
}

// document offers p's document to docs, reads its Markdown only when docs
// wants it, and Puts it. gone is a page Notion no longer finds, unshared or
// deleted since it was listed, which has no row. One whose body Notion
// refuses (another notionOneRefused: past a limit of Notion's, say) keeps
// its document as held, and the read goes on. A body empty has no
// document, and takes back the one held.
func (c *notionClient) document(ctx context.Context, p notionPage, docs DocumentWriter) (gone bool, _ error) {
	version := p.version(c.now)
	if !docs.Want(notionPagesStream.Name, p.id, version) {
		return false, nil
	}
	md, refusal, err := c.markdown(ctx, p.id)
	switch {
	case err != nil:
		return false, err
	case refusal == http.StatusNotFound:
		notionPagesRead.WithLabelValues("gone").Inc()
		return true, nil
	case refusal != 0:
		notionPagesRead.WithLabelValues("refused").Inc()
		return false, nil
	}
	doc, ok := notionDocument(p, *md, version)
	if !ok {
		notionPagesRead.WithLabelValues("empty").Inc()
		if _, held := docs.Held(notionPagesStream.Name, p.id); held {
			return false, docs.Drop(notionPagesStream.Name, p.id)
		}
		return false, nil
	}
	notionPagesRead.WithLabelValues("read").Inc()
	return false, docs.Put(ctx, notionPagesStream.Name, doc)
}

// notionMarkdown is a page's body as Notion exports it. Notion cuts a page
// of more than about 20,000 blocks short and says so.
type notionMarkdown struct {
	Markdown  *string `json:"markdown"`
	Truncated bool    `json:"truncated"`
}

// markdown reads a page's body, or the status of Notion's refusing it
// (notionOneRefused): 404 for a page it no longer finds.
func (c *notionClient) markdown(ctx context.Context, id string) (_ *notionMarkdown, refusal int, _ error) {
	path := "/v1/pages/" + url.PathEscape(id) + "/markdown"
	status, raw, err := c.do(ctx, path, nil, nil)
	switch {
	case err != nil:
		return nil, 0, err
	case notionOneRefused(status):
		return nil, status, nil
	}
	if err := notionStatus(status, path); err != nil {
		return nil, 0, err
	}
	var md notionMarkdown
	if json.Unmarshal(raw, &md) != nil || md.Markdown == nil {
		return nil, 0, fmt.Errorf("notion %s: the response has no markdown", path)
	}
	return &md, 0, nil
}

// notionDocument is a page's document at version: its title and id, then
// its body, and a line saying so when Notion cut the body short. Its last
// edit is its At and its row's, not its text, so an edit of the row's
// properties alone leaves the text as it was. ok is false for an empty
// body, which has no document.
func notionDocument(p notionPage, md notionMarkdown, version string) (_ Document, ok bool) {
	body := strings.TrimSpace(*md.Markdown)
	if body == "" {
		return Document{}, false
	}
	title := strings.Join(strings.Fields(p.title), " ")
	var text strings.Builder
	fmt.Fprintf(&text, "# %s\n\nPage id: %s\n\n%s\n", cmp.Or(title, "Untitled"), p.id, body)
	if md.Truncated {
		text.WriteString("\nNotion cut this page short at its block limit; the rest of it is not here.\n")
	}
	return Document{ExternalID: p.id, Version: version, At: p.edited, Title: title, URL: p.url, Text: []byte(text.String())}, true
}

// notionSource is a shared data source and its table. schema is a
// fingerprint of what its table's columns and their values derive from:
// each column's property id and type, the column's name (and its end's),
// and the settings a formula, rollup or unique id computes its value from.
// A change that keeps it keeps every stored row as a full read would write it.
type notionSource struct {
	id      string
	table   Table
	columns []notionColumn
	schema  string
}

// notionColumn is a property's column: rows give its value under the
// property's id, and a date's end goes to a column of its own.
type notionColumn struct {
	id, name, end string
}

func (s notionSource) path() string { return "/v1/data_sources/" + url.PathEscape(s.id) + "/query" }

// sources lists every shared data source with its table, named from its
// title (notionSlug), or data_source for a title with no letter or digit
// Helix keeps. A name is never users or pages, and is held by the oldest
// data source by creation, then id; any other that would take it takes it
// with its id's suffix (notionSuffixed) instead, so sharing a newer one
// moves no table.
func (c *notionClient) sources(ctx context.Context) ([]notionSource, error) {
	type listed struct {
		id, title  string
		created    time.Time
		properties map[string]any
	}
	// A data source listed twice keeps its last listing.
	var all []listed
	at := map[string]int{}
	err := c.search(ctx, map[string]any{"property": "object", "value": "data_source"}, "ascending", nil, func(item map[string]any) error {
		l := listed{title: notionText(item["title"])}
		l.id, _ = item["id"].(string)
		l.created, _ = notionTime(item["created_time"]) // none sorts first
		l.properties, _ = item["properties"].(map[string]any)
		if i, ok := at[l.id]; ok {
			all[i] = l
			return nil
		}
		at[l.id] = len(all)
		all = append(all, l)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(all, func(a, b listed) int { return cmp.Or(a.created.Compare(b.created), strings.Compare(a.id, b.id)) })
	tables := map[string]bool{"users": true, "pages": true}
	sources := make([]notionSource, len(all))
	for i, l := range all {
		name := cmp.Or(notionSlug(l.title), "data_source")
		if tables[name] {
			name = notionSuffixed(tables, name, l.id)
		}
		tables[name] = true
		s := notionSource{id: l.id, table: Table{Name: name, Columns: slices.Clone(notionRowColumns), PrimaryKey: []string{"page_id"}, Dense: true}}
		// A property's column is named from its name, and a date's end
		// column from its name and _end. Properties have no creation time
		// to settle a name two of them would take, so each of those takes it
		// with its id's suffix, as a property would that takes a fixed
		// column's name: no column ever takes another's name.
		type property struct {
			id, kind, name string
			computed       any // a formula's, rollup's or unique id's settings
		}
		var properties []property
		claims := map[string]int{}
		for _, name := range slices.Sorted(maps.Keys(l.properties)) {
			schema, _ := l.properties[name].(map[string]any)
			p := property{name: cmp.Or(notionSlug(name), "property")}
			p.kind, _ = schema["type"].(string)
			p.id, _ = schema["id"].(string)
			if _, ok := notionTypes[p.kind]; !ok {
				continue
			}
			if p.kind == "formula" || p.kind == "rollup" || p.kind == "unique_id" {
				p.computed = schema[p.kind]
			}
			properties = append(properties, p)
			claims[p.name]++
			if p.kind == "date" {
				claims[p.name+"_end"]++
			}
		}
		columns := map[string]bool{"page_id": true, "url": true, "created_at": true, "last_edited_at": true}
		column := func(name, id string) string {
			if claims[name] > 1 || columns[name] {
				return notionSuffixed(columns, name, id)
			}
			columns[name] = true
			return name
		}
		schema := sha256.New()
		for _, p := range properties {
			col := notionColumn{id: p.id, name: column(p.name, p.id)}
			s.table.Columns = append(s.table.Columns, Column{Name: col.name, Type: notionTypes[p.kind], Nullable: true})
			if p.kind == "date" {
				col.end = column(p.name+"_end", p.id)
				s.table.Columns = append(s.table.Columns, Column{Name: col.end, Type: "string", Nullable: true})
			}
			s.columns = append(s.columns, col)
			computed, _ := json.Marshal(p.computed) // decoded JSON always encodes
			fmt.Fprintf(schema, "%q %q %q %q %s\n", p.id, p.kind, col.name, col.end, computed)
		}
		s.schema = hex.EncodeToString(schema.Sum(nil)[:8])
		sources[i] = s
	}
	return sources, nil
}

// rows reads every row of a data source, oldest first by creation, and
// passes each to each. Notion answers at most 10,000 rows per query and
// says when it cut one short; the read then queries again from the last
// row's creation time, which Notion shows to the minute, skipping the rows
// of that minute it has. A minute of more rows than one query answers, as
// an import or a duplicated database makes, is read in windows of it,
// halved until each answers whole, since Notion compares a filter's time
// to the millisecond; a window of a second or less that one query can't
// answer fails the read. query is the queries' own (filter_properties),
// and a non-zero edited reads only the rows last edited at or after it. A
// wiki's query also lists the data sources under it, which are not rows.
func (c *notionClient) rows(ctx context.Context, s notionSource, query url.Values, edited time.Time,
	each func(p notionPage, item map[string]any) error) error {
	body := map[string]any{"result_type": "page", "sorts": []any{map[string]any{"timestamp": "created_time", "direction": "ascending"}}}
	var since time.Time
	at := map[string]bool{} // the rows created at since
	// windows are the spans of creation time [from, until) left to read,
	// the next one last; a zero time is no bound.
	windows := [][2]time.Time{{}}
	for len(windows) > 0 {
		from, until := windows[len(windows)-1][0], windows[len(windows)-1][1]
		windows = windows[:len(windows)-1]
		var filter []any
		bound := func(timestamp, op string, t time.Time) {
			if !t.IsZero() {
				filter = append(filter, map[string]any{"timestamp": timestamp, timestamp: map[string]any{op: t.UTC().Format(time.RFC3339Nano)}})
			}
		}
		bound("last_edited_time", "on_or_after", edited)
		bound("created_time", "on_or_after", from)
		bound("created_time", "before", until)
		switch len(filter) {
		case 0:
			delete(body, "filter")
		case 1:
			body["filter"] = filter[0]
		default:
			body["filter"] = map[string]any{"and": filter}
		}
		cut, err := c.list(ctx, s.path(), query, body, func(item map[string]any) error {
			p, err := notionPageOf(s.path(), item)
			switch {
			case err != nil:
				return err
			case p.created.Equal(since) && at[p.id]:
				return nil
			case !p.created.Equal(since):
				since, at = p.created, map[string]bool{}
			}
			at[p.id] = true
			return each(p, item)
		})
		switch {
		case err != nil:
			return err
		case !cut:
			continue
		case since.After(from):
			// Every row created before since's minute is read.
			windows = append(windows, [2]time.Time{since, until})
			continue
		}
		// One query can't answer the rows of the window's first minute, or
		// of the part of a minute the window is.
		end := until
		if minute := from.Add(time.Minute); until.IsZero() || until.After(minute) {
			windows, end = append(windows, [2]time.Time{minute, until}), minute
		}
		if end.Sub(from) <= notionMinWindow {
			return invalid("Notion answers at most 10,000 rows of a database at a time, and more rows of %s were created within one second; "+
				"Helix can't read them all", s.table.Name)
		}
		mid := from.Add(end.Sub(from) / 2)
		windows = append(windows, [2]time.Time{mid, end}, [2]time.Time{from, mid})
	}
	return nil
}

// row is a data source's row: every column, null where the page has no
// value. more are its relation and people columns whose value the page
// cut at notionShownReferences, for references to read whole: a relation
// says so with has_more, a people value doesn't.
func (s notionSource) row(p notionPage, item map[string]any) (row map[string]any, more []notionColumn) {
	row = map[string]any{"page_id": p.id, "url": orNull(p.url),
		"created_at": p.created.UTC().Format(time.RFC3339), "last_edited_at": p.edited.UTC().Format(time.RFC3339)}
	properties, _ := item["properties"].(map[string]any)
	byID := map[string]map[string]any{}
	for _, v := range properties {
		value, _ := v.(map[string]any)
		id, _ := value["id"].(string)
		byID[id] = value
	}
	for _, col := range s.columns {
		v := byID[col.id]
		value, end := notionValue(v)
		row[col.name] = value
		if col.end != "" {
			row[col.end] = end
		}
		if shown, _ := v["people"].([]any); v["has_more"] == true || v["type"] == "people" && len(shown) >= notionShownReferences {
			more = append(more, col)
		}
	}
	return row, more
}

// tableRow is a data source's row (row), its relation and people values
// that the page cut short read whole (references).
func (c *notionClient) tableRow(ctx context.Context, s notionSource, p notionPage, item map[string]any) (map[string]any, error) {
	row, more := s.row(p, item)
	for _, col := range more {
		switch ids, ok, err := c.references(ctx, p.id, col.id); {
		case err != nil:
			return nil, err
		case ok && len(ids) == 0:
			row[col.name] = nil
		case ok:
			row[col.name] = ids
		}
	}
	return row, nil
}

// references reads the ids of a relation's or people value of page whole,
// 100 a request, up to maxNotionReferences: one of more keeps its first.
// ok is false for a value Notion refused to read (notionOneRefused), such
// as one of a page deleted since its query; its row keeps the ids it showed.
func (c *notionClient) references(ctx context.Context, pageID, propertyID string) (ids []any, ok bool, err error) {
	_, err = c.list(ctx, "/v1/pages/"+url.PathEscape(pageID)+"/properties/"+url.PathEscape(propertyID), nil, nil, func(item map[string]any) error {
		kind, _ := item["type"].(string)
		ref, _ := item[kind].(map[string]any)
		if id, _ := ref["id"].(string); id != "" {
			ids = append(ids, id)
		}
		if len(ids) == maxNotionReferences {
			return errNotionEnough
		}
		return nil
	})
	var status notionHTTPError
	switch {
	case errors.Is(err, errNotionEnough):
	case errors.As(err, &status) && notionOneRefused(int(status)):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return ids, true, nil
}

// notionValue is a property value, or a formula's or rollup's result, as
// its column holds it: text plain, an option by its name, people and
// relations by id, files by name, a date by its start (and end), a unique
// id as PREFIX-n or n. Empty is null.
func notionValue(v map[string]any) (value, end any) {
	kind, _ := v["type"].(string)
	inner := v[kind]
	switch kind {
	case "title", "rich_text":
		return orNull(notionText(inner)), nil
	case "number", "checkbox", "boolean", "url", "email", "phone_number", "string":
		if s, ok := inner.(string); ok {
			return orNull(s), nil
		}
		return inner, nil
	case "select", "status":
		option, _ := inner.(map[string]any)
		return option["name"], nil
	case "multi_select", "files":
		return notionField(inner, "name"), nil
	case "people", "relation":
		return notionField(inner, "id"), nil
	case "date":
		date, _ := inner.(map[string]any)
		return date["start"], date["end"]
	case "created_by", "last_edited_by":
		user, _ := inner.(map[string]any)
		return user["id"], nil
	case "created_time", "last_edited_time":
		t, err := notionTime(inner)
		if err != nil {
			return nil, nil
		}
		return t.UTC().Format(time.RFC3339), nil
	case "unique_id":
		id, _ := inner.(map[string]any)
		n, ok := id["number"].(float64)
		if !ok {
			return nil, nil
		}
		if prefix, _ := id["prefix"].(string); prefix != "" {
			return prefix + "-" + strconv.FormatFloat(n, 'f', -1, 64), nil
		}
		return n, nil
	case "formula", "rollup":
		result, _ := inner.(map[string]any)
		if result["type"] != "array" {
			value, _ := notionValue(result)
			return value, nil
		}
		// A rollup that shows its pages' values: each one's plain value, lists flattened.
		items, _ := result["array"].([]any)
		var values []any
		for _, item := range items {
			m, _ := item.(map[string]any)
			switch value, _ := notionValue(m); value := value.(type) {
			case nil:
			case []any:
				values = append(values, value...)
			default:
				values = append(values, value)
			}
		}
		if len(values) == 0 {
			return nil, nil
		}
		return values, nil
	}
	return nil, nil
}

// notionField is each listed object's field, or null when none has it.
func notionField(list any, field string) any {
	items, _ := list.([]any)
	var values []any
	for _, item := range items {
		m, _ := item.(map[string]any)
		if s, _ := m[field].(string); s != "" {
			values = append(values, s)
		}
	}
	if len(values) == 0 {
		return nil
	}
	return values
}

// notionText is rich text as plain text.
func notionText(v any) string {
	items, _ := v.([]any)
	var text strings.Builder
	for _, item := range items {
		m, _ := item.(map[string]any)
		s, _ := m["plain_text"].(string)
		text.WriteString(s)
	}
	return text.String()
}

// notionTitle is a page's title: its one title property's text.
func notionTitle(properties any) string {
	props, _ := properties.(map[string]any)
	for _, v := range props {
		if p, _ := v.(map[string]any); p["type"] == "title" {
			return notionText(p["title"])
		}
	}
	return ""
}

func notionTime(v any) (time.Time, error) {
	s, _ := v.(string)
	return time.Parse(time.RFC3339, s)
}

// notionSlug names a table or column from a Notion title in snake case:
// ASCII letters and digits in lower case, accents dropped (é is e), every
// other run of characters one underscore, at most maxNotionName bytes;
// "Customer Accounts" is customer_accounts. A title with none of those is "".
func notionSlug(title string) string {
	var slug strings.Builder
	gap := false
	for _, r := range norm.NFD.String(title) {
		switch {
		case unicode.Is(unicode.Mn, r):
		case r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			if gap && slug.Len() > 0 {
				slug.WriteByte('_')
			}
			gap = false
			slug.WriteRune(unicode.ToLower(r))
		default:
			gap = true
		}
	}
	return strings.TrimRight(slug.String()[:min(slug.Len(), maxNotionName)], "_")
}

// notionSuffixed is name with a suffix of id that no name in taken holds,
// which it then holds: two underscores, which no slug has, and the first 4
// bytes of id's SHA-256 in hex, or more should another id's take that. It
// depends on id alone, so it never moves as other names come and go.
func notionSuffixed(taken map[string]bool, name, id string) string {
	sum := sha256.Sum256([]byte(id))
	for n := 4; ; n++ {
		if got := name + "__" + hex.EncodeToString(sum[:n]); !taken[got] {
			taken[got] = true
			return got
		}
	}
}
