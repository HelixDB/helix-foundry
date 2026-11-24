package connector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// databuddyTable is one table: its primary key, the query type it reads,
// the rows a page asks for (a session or profile row is larger than an
// aggregate's), and the column the type's name field lands in, when it has
// one.
type databuddyTable struct {
	name     string
	key      []string
	query    string
	pageRows int
	nameAs   string
}

// databuddyOccurrences is the one table read a UTC day at a time and keyed
// by a hash of each row (databuddyEventID): Databuddy gives an occurrence no
// id. maxDatabuddyOccurrences bounds what a full read of it keeps across the
// chosen sites: the newest whole days that fit, sized from custom_events_daily's
// counts before anything is read (occurrenceStart).
const (
	databuddyOccurrences    = "custom_event_occurrences"
	maxDatabuddyOccurrences = 500_000
)

// databuddyTables are the tables in display order. websites is the key's
// list of sites; every other table is one query type over each chosen
// site's history window, its rows tagged with website_id and keyed by it
// first: a day, page or name repeats across sites, and so does a visitor id
// where two sites share an origin.
var databuddyTables = []databuddyTable{
	{name: "websites", key: []string{"website_id"}},
	{"daily_metrics", []string{"website_id", "date"}, "events_by_date", 1000, ""},
	{"pages", []string{"website_id", "path"}, "top_pages", 10000, "path"},
	{"referrers", []string{"website_id", "referrer"}, "top_referrers", 10000, "referrer"},
	{"custom_events", []string{"website_id", "event_name"}, "custom_events", 10000, "event_name"},
	{"custom_events_daily", []string{"website_id", "event_name", "date"}, "custom_events_trends_by_event", 10000, ""},
	{databuddyOccurrences, []string{"website_id", "event_id"}, "custom_events_recent", 10000, ""},
	{"sessions", []string{"website_id", "session_id"}, "session_list", 1000, ""},
	{"profiles", []string{"website_id", "visitor_id"}, "profile_list", 1000, ""},
}

// databuddyForeignKeys tie every table but websites to its site. A
// session's visitor is no single-column key of profiles (it repeats across
// sites), so it has no foreign key, and nor does an occurrence's session.
var databuddyForeignKeys = func() map[string][]ForeignKey {
	out := map[string][]ForeignKey{}
	for _, t := range databuddyTables[1:] {
		out[t.name] = []ForeignKey{foreignKey("website_id", "websites", "website_id")}
	}
	return out
}()

// databuddyDeniedRE tells a query type the key or plan may not run (website
// permission, a scope, a feature the plan lacks) from a failure that passes,
// such as a timeout: only the first makes its table unavailable, which pauses
// a connector that synced it, so anything else is retried. Databuddy words
// each result's error but gives it no code.
var databuddyDeniedRE = regexp.MustCompile(`(?i)permission|access|scope|forbidden|unauthori|feature|plan|upgrade`)

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
// are among them, then asks every query type for one row of today on each
// site, in one request a site: a type the key may not run on one of them (a
// website-scoped key without custom events' permission, a plan without the
// feature) is unavailable, with Databuddy's reason. Any other failure of a
// type is refused, so the sync is retried rather than paused. A key with no
// site reads only websites.
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
	}
	today := time.Now().UTC().Format(time.DateOnly)
	var queries []string
	for _, t := range databuddyTables[1:] {
		queries = append(queries, t.query)
	}
	for _, site := range sites {
		results, err := k.query(ctx, src.Secret, databuddyText(site["id"]), databuddyRequest{
			Parameters: queries, StartDate: today, EndDate: today, Limit: 1, Page: 1, TimeZone: "UTC", Granularity: "daily"})
		if err != nil {
			return Discovery{}, err
		}
		for _, t := range databuddyTables[1:] {
			r, ok := results[t.query]
			why := strings.TrimSpace(r.Error)
			switch {
			case !ok:
				return Discovery{}, refused(nil, "Databuddy did not answer the %s query; try again", t.query)
			case r.Success || reasons[t.name] != "":
			case databuddyDeniedRE.MatchString(why):
				reasons[t.name] = why
			default:
				return Discovery{}, refused(nil, "Databuddy could not run the %s query (%s); try again", t.query, why)
			}
		}
	}
	// A full read of occurrences is sized from the daily counts.
	if why := reasons["custom_events_daily"]; why != "" && reasons[databuddyOccurrences] == "" {
		reasons[databuddyOccurrences] = why
	}
	objects := make([]saasTable, len(databuddyTables))
	for i, t := range databuddyTables {
		objects[i] = saasTable{t.name, t.key[0]}
	}
	d := saasDiscovery(objects, readable, databuddyForeignKeys)
	for i := range d.Unavailable {
		d.Unavailable[i].Reason = reasons[d.Unavailable[i].Name]
	}
	for i, t := range d.Tables {
		table, _ := databuddyTableOf(t.Name)
		d.Tables[i].PrimaryKey = slices.Clone(table.key)
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
	if t.name == databuddyOccurrences {
		if from, err = k.occurrenceStart(ctx, src.Secret, sites, from, now); err != nil {
			return table, err
		}
		return table, k.occurrences(ctx, src.Secret, sites, from, now, w.Write)
	}
	for _, site := range sites {
		if err := k.rows(ctx, src.Secret, t, databuddyText(site["id"]), from, now, w.Write); err != nil {
			return table, err
		}
	}
	return table, nil
}

// occurrenceStart is the first day a full read of occurrences keeps: the
// oldest day from from's on whose occurrences, with every later day's to
// to's, fit maxDatabuddyOccurrences across the sites, by Databuddy's daily
// counts. A day too large to keep alone is refused rather than kept in part:
// past it the table would hold no whole day.
func (k *databuddyKind) occurrenceStart(ctx context.Context, key string, sites []map[string]any, from, to time.Time) (time.Time, error) {
	daily, _ := databuddyTableOf("custom_events_daily")
	counts := map[string]int64{}
	for _, site := range sites {
		err := k.rows(ctx, key, daily, databuddyText(site["id"]), from, to, func(row map[string]any) error {
			n, ok := row["total_events"].(float64)
			day, dated := databuddyDay(row["date"])
			if !ok || !dated || n < 0 {
				return fmt.Errorf("databuddy %s: a row has no day or count: %v", daily.query, row)
			}
			counts[day.Format(time.DateOnly)] += int64(n)
			return nil
		})
		if err != nil {
			return time.Time{}, err
		}
	}
	var kept int64
	first := databuddyDayOf(from)
	for day := databuddyDayOf(to); !day.Before(first); day = day.AddDate(0, 0, -1) {
		kept += counts[day.Format(time.DateOnly)]
		switch {
		case kept <= maxDatabuddyOccurrences:
		case day.Equal(databuddyDayOf(to)):
			return time.Time{}, invalid("the websites sent more custom events today than Foundry keeps of them (%d); choose fewer websites or turn off %s",
				maxDatabuddyOccurrences, databuddyOccurrences)
		default:
			return day.AddDate(0, 0, 1), nil
		}
	}
	return first, nil
}

// occurrences writes every site's custom event occurrences of each UTC day
// from from's to to's, a day at a time: each day pages on its own, so the
// page cap bounds a day, not the window. A row read twice (the pages are
// offsets into a list that grows while it is read) is written once, and so
// are two occurrences Databuddy holds identically (databuddyEventID).
func (k *databuddyKind) occurrences(ctx context.Context, key string, sites []map[string]any, from, to time.Time, write func(map[string]any) error) error {
	t, _ := databuddyTableOf(databuddyOccurrences)
	for _, site := range sites {
		id := databuddyText(site["id"])
		for day := databuddyDayOf(from); !day.After(to); day = day.AddDate(0, 0, 1) {
			seen := map[string]bool{}
			err := k.rows(ctx, key, t, id, day, day, func(row map[string]any) error {
				if seen[databuddyText(row["event_id"])] {
					return nil
				}
				seen[databuddyText(row["event_id"])] = true
				return write(row)
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// rows writes every row of t's query for one site from from's day to to's,
// page by page until a short page. A page past the cap fails the read rather
// than publishing part of the table, saying to keep fewer days. The pages
// are offsets into a list that may change while it is read; Databuddy
// documents no stable order to page by, so a row that moves between pages
// may be missed until the next full read.
func (k *databuddyKind) rows(ctx context.Context, key string, t databuddyTable, site string, from, to time.Time, write func(map[string]any) error) error {
	// profile_list once answered a row per visitor and session; each visitor is written once.
	seen := map[any]bool{}
	for page := 1; ; page++ {
		if page > maxDatabuddyPages {
			return invalid("a website holds more %s than Databuddy pages through (%d pages of %d); keep fewer days of history",
				strings.ReplaceAll(t.name, "_", " "), maxDatabuddyPages, t.pageRows)
		}
		results, err := k.query(ctx, key, site, databuddyRequest{Parameters: []string{t.query},
			StartDate: from.Format(time.DateOnly), EndDate: to.Format(time.DateOnly), Limit: t.pageRows, Page: page, TimeZone: "UTC",
			Granularity: "daily"})
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
			if t.name == databuddyOccurrences {
				if row, err = databuddyOccurrence(site, raw, row); err != nil {
					return err
				}
			}
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

// databuddyOccurrence is an occurrence row as custom_event_occurrences
// stores it: its time in RFC 3339 UTC to the millisecond, its UTC day, its
// properties as an object (none when Databuddy's text holds no JSON object)
// and its event_id. An occurrence without a readable time can't be dated or
// keyed, so it fails the read.
func databuddyOccurrence(site string, raw, row map[string]any) (map[string]any, error) {
	at, ok := databuddyTime(raw["timestamp"])
	if !ok {
		return nil, fmt.Errorf("databuddy custom_events_recent: an occurrence has no readable timestamp (%v)", raw["timestamp"])
	}
	at = at.UTC()
	row["timestamp"] = at.Format("2006-01-02T15:04:05.000Z")
	row["date"] = at.Format(time.DateOnly)
	var properties map[string]any
	if text, ok := raw["properties"].(string); !ok || json.Unmarshal([]byte(text), &properties) != nil {
		properties = nil
	}
	row["properties"] = properties
	row["event_id"] = databuddyEventID(site, row["timestamp"], raw)
	return row, nil
}

// databuddyEventID keys an occurrence: the first 128 bits, in hex, of the
// SHA-256 of a JSON array of its site, its time to the millisecond, and the
// fields custom_events_recent writes (event name, namespace, path, source,
// anonymous and session ids and the properties' text), each as Databuddy
// sent it, so a null and an empty string differ. It is the same at every
// read; two occurrences alike in all of them share it and are kept once.
func databuddyEventID(site string, at any, raw map[string]any) string {
	fields := []any{site, at}
	for _, name := range []string{"event_name", "namespace", "path", "source", "anonymous_id", "session_id", "properties"} {
		fields = append(fields, raw[name])
	}
	text, _ := json.Marshal(fields)
	sum := sha256.Sum256(text)
	return hex.EncodeToString(sum[:16])
}

// databuddyDayOf is t's UTC day.
func databuddyDayOf(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// databuddyDay reads a day Databuddy writes, YYYY-MM-DD or a time on it.
func databuddyDay(v any) (time.Time, bool) {
	if text, ok := v.(string); ok {
		if day, err := time.Parse(time.DateOnly, text); err == nil {
			return day, true
		}
	}
	t, ok := databuddyTime(v)
	return databuddyDayOf(t), ok
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

// databuddyRequest is one query: the types it runs over one date range,
// by UTC day (events_by_date also breaks a range down by hour). Both
// timezone fields: the docs name the body's timeZone and the URL's timezone.
type databuddyRequest struct {
	Parameters  []string `json:"parameters"`
	StartDate   string   `json:"startDate"`
	EndDate     string   `json:"endDate"`
	Limit       int      `json:"limit"`
	Page        int      `json:"page"`
	TimeZone    string   `json:"timeZone"`
	Granularity string   `json:"granularity"`
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
