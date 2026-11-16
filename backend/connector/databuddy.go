package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// databuddyKeyRE is a Databuddy API key: dbdy_ and its secret.
var databuddyKeyRE = regexp.MustCompile(`^dbdy_[A-Za-z0-9_-]+$`)

const (
	defaultDatabuddyHistoryDays = 90
	// maxDatabuddyPages is the query API's page cap: past it, a table would
	// be published partly read.
	maxDatabuddyPages = 1000
)

// databuddyTable is one table: the query type it reads, its key, the rows a
// page asks for (a session or profile row is larger than an aggregate's),
// and the column the type's name field lands in, when it has one.
type databuddyTable struct {
	saasTable
	query    string
	pageRows int
	nameAs   string
}

// databuddyTables are the tables in display order. websites is the key's
// list of sites; every other table is one query type over each chosen
// site's history window, its rows tagged with website_id.
var databuddyTables = []databuddyTable{
	{saasTable: saasTable{"websites", "website_id"}},
	{saasTable{"daily_metrics", "date"}, "events_by_date", 1000, ""},
	{saasTable{"pages", "path"}, "top_pages", 10000, "path"},
	{saasTable{"referrers", "referrer"}, "top_referrers", 10000, "referrer"},
	{saasTable{"custom_events", "event_name"}, "custom_events", 10000, "event_name"},
	{saasTable{"sessions", "session_id"}, "session_list", 1000, ""},
	{saasTable{"profiles", "visitor_id"}, "profile_list", 1000, ""},
}

// databuddySiteKeyed are the tables keyed by (website_id, key): their key is
// only unique within a site. Sessions and visitors carry random ids.
var databuddySiteKeyed = map[string]bool{"daily_metrics": true, "pages": true, "referrers": true, "custom_events": true}

var databuddyForeignKeys = func() map[string][]ForeignKey {
	out := map[string][]ForeignKey{"sessions": {foreignKey("visitor_id", "profiles", "visitor_id")}}
	for _, t := range databuddyTables[1:] {
		out[t.name] = append(out[t.name], foreignKey("website_id", "websites", "website_id"))
	}
	return out
}()

func databuddyTableOf(name string) (databuddyTable, bool) {
	i := slices.IndexFunc(databuddyTables, func(t databuddyTable) bool { return t.name == name })
	if i < 0 {
		return databuddyTable{}, false
	}
	return databuddyTables[i], true
}

// DatabuddySettings are a Databuddy connector's settings: the sites it reads
// (none is every site the key reads) and how many days of history it keeps.
type DatabuddySettings struct {
	WebsiteIDs  []string `json:"website_ids,omitempty"`
	HistoryDays int      `json:"history_days"`
}

type databuddyKind struct {
	base string
	http *http.Client
}

func (k *databuddyKind) Name() string        { return "databuddy" }
func (k *databuddyKind) DisplayName() string { return "Databuddy" }

func (k *databuddyKind) Prepare(src Source) (json.RawMessage, error) {
	if !databuddyKeyRE.MatchString(strings.TrimSpace(src.Secret)) {
		return nil, invalid("enter a Databuddy API key (dbdy_…)")
	}
	var s DatabuddySettings
	decoder := json.NewDecoder(bytes.NewReader(orEmpty(src.Settings)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil {
		return nil, invalid("this connector takes only its key, its websites and how many days of history to keep")
	}
	switch {
	case s.HistoryDays == 0:
		s.HistoryDays = defaultDatabuddyHistoryDays
	case s.HistoryDays < 1 || s.HistoryDays > 365:
		return nil, invalid("keep 1 to 365 days of history")
	}
	ids := make([]string, 0, len(s.WebsiteIDs))
	for _, id := range s.WebsiteIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, invalid("a website id is empty")
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	s.WebsiteIDs = ids
	return json.Marshal(s)
}

func (k *databuddyKind) Hint(secret string) string { return keyHint(strings.TrimSpace(secret), 1) }

// Discover proves the key by listing its websites, checks the chosen ones
// are among them, then asks every query type for one row of today on the
// first site, in one request: a type the key may not read (a website-scoped
// key without custom events' permission, a plan without the feature) is
// unavailable, with Databuddy's reason. A key with no site reads only
// websites.
func (k *databuddyKind) Discover(ctx context.Context, src Source) (Discovery, error) {
	s, err := databuddySettingsOf(src.Settings)
	if err != nil {
		return Discovery{}, err
	}
	sites, err := k.sites(ctx, src.Secret, s)
	if err != nil {
		return Discovery{}, err
	}
	reasons := map[string]string{}
	readable := func(name string) bool { _, blocked := reasons[name]; return !blocked }
	if len(sites) == 0 {
		for _, t := range databuddyTables[1:] {
			reasons[t.name] = "the key reads no website yet"
		}
	} else {
		today := time.Now().UTC().Format(time.DateOnly)
		var queries []string
		for _, t := range databuddyTables[1:] {
			queries = append(queries, t.query)
		}
		results, err := k.query(ctx, src.Secret, databuddyText(sites[0]["id"]), databuddyRequest{
			Parameters: queries, StartDate: today, EndDate: today, Limit: 1, Page: 1, TimeZone: "UTC"})
		if err != nil {
			return Discovery{}, err
		}
		for _, t := range databuddyTables[1:] {
			r, ok := results[t.query]
			switch {
			case !ok:
				reasons[t.name] = "Databuddy did not answer its query"
			case !r.Success:
				reasons[t.name] = strings.TrimSpace(r.Error)
			}
		}
	}
	objects := make([]saasTable, len(databuddyTables))
	for i, t := range databuddyTables {
		objects[i] = t.saasTable
	}
	d := saasDiscovery(objects, readable, databuddyForeignKeys)
	for i := range d.Unavailable {
		d.Unavailable[i].Reason = reasons[d.Unavailable[i].Name]
	}
	for i, t := range d.Tables {
		if databuddySiteKeyed[t.Name] {
			d.Tables[i].PrimaryKey = []string{"website_id", t.PrimaryKey[0]}
		}
		d.Tables[i].Windowed = WindowedTable("databuddy", t.Name)
	}
	return d, nil
}

func (k *databuddyKind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	s, err := databuddySettingsOf(src.Settings)
	if err != nil {
		return table, err
	}
	t, ok := databuddyTableOf(table.Name)
	if !ok {
		return table, invalid("Databuddy has no table %q", table.Name)
	}
	sites, err := k.sites(ctx, src.Secret, s)
	if err != nil {
		return table, err
	}
	if t.name == "websites" {
		for _, site := range sites {
			if err := w.Write(databuddyRow(t, "", site)); err != nil {
				return table, err
			}
		}
		return table, nil
	}
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -s.HistoryDays)
	for _, site := range sites {
		if err := k.rows(ctx, src.Secret, t, databuddyText(site["id"]), from, now, w.Write); err != nil {
			return table, err
		}
	}
	return table, nil
}

// rows writes every row of t's query for one site from from's day to to's,
// page by page until a short page. A page past the cap fails the read rather
// than publishing part of the table.
func (k *databuddyKind) rows(ctx context.Context, key string, t databuddyTable, site string, from, to time.Time, write func(map[string]any) error) error {
	// profile_list once answered a row per visitor and session; each visitor is written once.
	seen := map[any]bool{}
	for page := 1; ; page++ {
		if page > maxDatabuddyPages {
			return fmt.Errorf("databuddy %s: more than %d pages of %d rows", t.query, maxDatabuddyPages, t.pageRows)
		}
		results, err := k.query(ctx, key, site, databuddyRequest{Parameters: []string{t.query},
			StartDate: from.Format(time.DateOnly), EndDate: to.Format(time.DateOnly), Limit: t.pageRows, Page: page, TimeZone: "UTC"})
		if err != nil {
			return err
		}
		r, ok := results[t.query]
		switch {
		case !ok:
			return fmt.Errorf("databuddy %s: the response has no result for it", t.query)
		case !r.Success:
			return refused(nil, "Databuddy refused the %s query: %s", t.query, strings.TrimSpace(r.Error))
		}
		for _, raw := range r.Data {
			row := databuddyRow(t, site, raw)
			if t.name == "profiles" {
				if seen[row["visitor_id"]] {
					continue
				}
				seen[row["visitor_id"]] = true
			}
			if err := write(row); err != nil {
				return err
			}
		}
		if len(r.Data) < t.pageRows {
			return nil
		}
	}
}

// databuddyRow is a query row as table t stores it: id becomes website_id
// (a website) and name its column, nested values (a session's events) and a
// visitor's per-session fields are dropped as other tables hold them, field
// names are snake_case, and every row but a website's carries its site.
func databuddyRow(t databuddyTable, site string, raw map[string]any) map[string]any {
	row := make(map[string]any, len(raw)+1)
	for field, value := range raw {
		switch value.(type) {
		case map[string]any, []any:
			continue
		}
		name := snakeCase(field)
		switch {
		case t.name == "websites" && name == "id":
			name = "website_id"
		case t.nameAs != "" && name == "name":
			name = t.nameAs
		case t.name == "profiles" && strings.HasPrefix(name, "session_") && name != "session_count":
			continue
		}
		row[name] = value
	}
	if t.name != "websites" {
		row["website_id"] = site
	}
	return row
}

// databuddySettingsOf decodes prepared settings.
func databuddySettingsOf(raw json.RawMessage) (DatabuddySettings, error) {
	var s DatabuddySettings
	if err := json.Unmarshal(orEmpty(raw), &s); err != nil {
		return s, invalid("the Databuddy settings are not valid")
	}
	if s.HistoryDays == 0 {
		s.HistoryDays = defaultDatabuddyHistoryDays
	}
	return s, nil
}

// sites lists the websites the key reads, only the chosen ones when s names
// any. A chosen site the key no longer reads fails: reading the others alone
// would drop its rows as if it had none.
func (k *databuddyKind) sites(ctx context.Context, key string, s DatabuddySettings) ([]map[string]any, error) {
	status, body, err := k.do(ctx, key, http.MethodGet, "/v1/query/websites", nil, nil)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, databuddyRefusal(status, body, "/v1/query/websites")
	}
	var resp struct {
		Success  bool             `json:"success"`
		Websites []map[string]any `json:"websites"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode databuddy websites: %w", err)
	}
	if !resp.Success || resp.Websites == nil {
		return nil, fmt.Errorf("databuddy websites: the response has no websites")
	}
	if len(s.WebsiteIDs) == 0 {
		return resp.Websites, nil
	}
	byID := map[string]map[string]any{}
	for _, site := range resp.Websites {
		byID[databuddyText(site["id"])] = site
	}
	out := make([]map[string]any, 0, len(s.WebsiteIDs))
	for _, id := range s.WebsiteIDs {
		site, ok := byID[id]
		if !ok {
			return nil, refused(nil, "this Databuddy key cannot read the website %s; choose another or give the key access to it", id)
		}
		out = append(out, site)
	}
	return out, nil
}

// databuddyRequest is one query: the types it runs over one date range.
// Both timezone fields: the docs name the body's timeZone and the URL's
// timezone.
type databuddyRequest struct {
	Parameters []string `json:"parameters"`
	StartDate  string   `json:"startDate"`
	EndDate    string   `json:"endDate"`
	Limit      int      `json:"limit"`
	Page       int      `json:"page"`
	TimeZone   string   `json:"timeZone"`
}

// databuddyResult is one query type's answer within a query.
type databuddyResult struct {
	Parameter string           `json:"parameter"`
	Success   bool             `json:"success"`
	Data      []map[string]any `json:"data"`
	Error     string           `json:"error"`
}

// query runs req on site and returns each query type's result by its name.
func (k *databuddyKind) query(ctx context.Context, key, site string, req databuddyRequest) (map[string]databuddyResult, error) {
	status, body, err := k.do(ctx, key, http.MethodPost, "/v1/query", url.Values{"website_id": {site}, "timezone": {"UTC"}}, req)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		return nil, databuddyRefusal(status, body, "/v1/query")
	}
	var resp struct {
		Success bool              `json:"success"`
		Error   string            `json:"error"`
		Data    []databuddyResult `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decode databuddy query: %w", err)
	}
	if !resp.Success && len(resp.Data) == 0 {
		return nil, refused(nil, "Databuddy refused the query: %s", strings.TrimSpace(resp.Error))
	}
	out := make(map[string]databuddyResult, len(resp.Data))
	for _, r := range resp.Data {
		out[r.Parameter] = r
	}
	return out, nil
}

// do sends one request with the key, retrying rate limits and server errors.
// A rejected key is refused here.
func (k *databuddyKind) do(ctx context.Context, key, method, path string, query url.Values, body any) (int, []byte, error) {
	target := k.base + path
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
	status, out, err := fetch(ctx, k.http, func() (*http.Request, error) {
		req, err := http.NewRequest(method, target, bytes.NewReader(payload))
		if err == nil {
			req.Header = http.Header{"X-Api-Key": {strings.TrimSpace(key)}}
			if body != nil {
				req.Header.Set("Content-Type", "application/json")
			}
		}
		return req, err
	})
	if err != nil {
		return 0, nil, refused(err, "could not reach Databuddy; try again")
	}
	if status == http.StatusUnauthorized {
		return 0, nil, rejectedCredential("Databuddy", nil, "Databuddy rejected this key; check it and enter it again")
	}
	return status, out, nil
}

// databuddyRefusal words a refused request with Databuddy's own error, which
// says what the key lacks; the codes are the API's.
func databuddyRefusal(status int, body []byte, path string) error {
	var e struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	_ = json.Unmarshal(body, &e)
	why := strings.TrimSpace(e.Error + " " + e.Message)
	switch {
	case status == http.StatusForbidden:
		return refused(nil, "this Databuddy key may not read %s; give it the read:data scope (%s)", path, why)
	case status == http.StatusTooManyRequests:
		return refused(nil, "this Databuddy key has used up its rate limit; Helix will retry")
	case why == "":
		return refused(nil, "Databuddy returned HTTP %d for %s; try again", status, path)
	}
	return refused(nil, "Databuddy returned HTTP %d for %s: %s", status, path, why)
}

// databuddyText is a JSON value as text: a string as it is, a number in its shortest form.
func databuddyText(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}
