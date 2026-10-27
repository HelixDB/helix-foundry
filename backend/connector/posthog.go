package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	posthogKeyRE     = regexp.MustCompile(`^phx_[A-Za-z0-9_]+$`)
	posthogProjectRE = regexp.MustCompile(`^\d+$`)
)

var posthogObjects = []saasTable{
	{"persons", "person_id"},
	{"person_distinct_ids", "distinct_id"},
	{"groups", "group_key"},
	{"events", "event_id"},
	{"daily_event_counts", "day"},
}

// posthogSourceTables is the HogQL table each object reads; counts read events.
var posthogSourceTables = map[string]string{
	"persons":             "persons",
	"person_distinct_ids": "person_distinct_ids",
	"groups":              "groups",
	"events":              "events",
	"daily_event_counts":  "events",
}

var posthogPrimaryKeys = map[string][]string{
	"groups":             {"group_type", "group_key"},
	"daily_event_counts": {"day", "event", "person_id"},
}

var posthogForeignKeys = referencedBy(posthogObjects, map[string][]string{
	"person_distinct_ids": {"person_id"},
	"events":              {"person_id", "distinct_id"},
	"daily_event_counts":  {"person_id"},
})

type posthogKind struct {
	us, eu string
	http   *http.Client
	// files reads batch exports with the s3 kind's role access; nil turns
	// export mode off.
	files *s3Kind
}

type PostHogSettings struct {
	Region string `json:"region"`
	// Host is the origin of a self-hosted PostHog; set only for region custom.
	Host      string `json:"host,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	EventDays int    `json:"event_days"`
	// Mode export reads PostHog batch exports from ExportBucket; empty reads the query API.
	Mode string `json:"mode,omitempty"`
	// ExportBucket is where the customer's PostHog batch exports land, read
	// through a role the way the s3 kind reads.
	ExportBucket *S3Settings `json:"export_bucket,omitempty"`
}

// MinInterval keeps a query-API connector to one sync a day on every plan:
// PostHog rate-limits or rejects recurring exports through /query.
func (k *posthogKind) MinInterval(src Source) time.Duration {
	var s PostHogSettings
	if json.Unmarshal(src.Settings, &s) == nil && s.Mode == "export" {
		return 0
	}
	return 24 * time.Hour
}

func (k *posthogKind) Name() string        { return "posthog" }
func (k *posthogKind) DisplayName() string { return "PostHog" }

func (k *posthogKind) Prepare(src Source) (json.RawMessage, error) {
	if !posthogKeyRE.MatchString(strings.TrimSpace(src.Secret)) {
		return nil, invalid("enter a PostHog personal API key (phx_…)")
	}
	var s PostHogSettings
	if err := json.Unmarshal(orEmpty(src.Settings), &s); err != nil {
		return nil, invalid("PostHog settings are malformed")
	}
	if s.Region == "" {
		s.Region = "us"
	}
	switch s.Region {
	case "us", "eu":
		if s.Host != "" {
			return nil, invalid("a PostHog host is only used with the custom region")
		}
	case "custom":
		u, err := url.Parse(strings.TrimSpace(s.Host))
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return nil, invalid("enter the HTTPS address of your PostHog, without credentials")
		}
		s.Host = u.Scheme + "://" + u.Host
	default:
		return nil, invalid("PostHog region must be us, eu or custom")
	}
	s.ProjectID = strings.TrimSpace(s.ProjectID)
	if s.ProjectID != "" && !posthogProjectRE.MatchString(s.ProjectID) {
		return nil, invalid("the PostHog project ID is a number, found in project settings")
	}
	if s.EventDays == 0 {
		s.EventDays = 30
	}
	if s.EventDays < 1 || s.EventDays > 365 {
		return nil, invalid("PostHog event days must be between 1 and 365")
	}
	switch s.Mode {
	case "":
		if s.ExportBucket != nil {
			return nil, invalid("an export bucket is only read in export mode")
		}
	case posthogExportMode:
		if err := k.prepareExports(s.ExportBucket); err != nil {
			return nil, err
		}
	default:
		return nil, invalid("PostHog mode must be export, or empty to use the query API")
	}
	return json.Marshal(s)
}

func (k *posthogKind) Discover(ctx context.Context, src Source) (Discovery, error) {
	var s PostHogSettings
	if err := json.Unmarshal(src.Settings, &s); err != nil {
		return Discovery{}, invalid("PostHog settings are malformed")
	}
	host := k.host(s)
	header := http.Header{"Authorization": {"Bearer " + strings.TrimSpace(src.Secret)}}
	if s.ProjectID == "" {
		id, err := k.onlyProject(ctx, host, header)
		if err != nil {
			return Discovery{}, err
		}
		s.ProjectID = id
	}

	queryURL := host + "/api/projects/" + s.ProjectID + "/query/"
	tables := uniqueValues(posthogSourceTables)
	if s.Mode == "export" {
		tables = []string{"groups"}
	}
	readable, err := probeAll(ctx, tables, func(ctx context.Context, table string) (bool, error) {
		status, _, err := postJSON(ctx, k.http, queryURL, header, map[string]any{
			"query": map[string]string{"kind": "HogQLQuery", "query": "SELECT 1 FROM " + table + " LIMIT 1"},
			"name":  "helix foundry probe",
		})
		switch {
		case errors.Is(err, ErrPrivateAddress):
			return false, invalid("that PostHog host is not on the public internet")
		case err != nil:
			return false, refused(err, "could not reach PostHog; check the region or host")
		case status == http.StatusUnauthorized:
			return false, refused(nil, "PostHog rejected this key; check it and the region, then test again")
		case status == http.StatusTooManyRequests || status >= 500:
			// Not an answer about the table: a sync must retry, not call it unreadable.
			return false, refused(nil, "PostHog returned HTTP %d; try again", status)
		}
		// A 403 is a missing scope; a query error means the table is not there. Neither fails the test.
		return status < 300, nil
	})
	if err != nil {
		return Discovery{}, err
	}
	if s.Mode == "export" {
		return k.discoverExports(ctx, src, s, readable["groups"])
	}
	d := saasDiscovery(posthogObjects, func(name string) bool { return readable[posthogSourceTables[name]] }, posthogForeignKeys)
	if len(d.Tables) == 0 {
		return Discovery{}, refused(nil, "this key cannot query PostHog data; grant Query Read and test again")
	}
	shapePostHogTables(d.Tables)
	d.Settings, err = json.Marshal(s)
	return d, err
}

// shapePostHogTables gives each table its key and marks the tables that
// cover the last event_days only.
func shapePostHogTables(tables []Table) {
	for i := range tables {
		if pk, ok := posthogPrimaryKeys[tables[i].Name]; ok {
			tables[i].PrimaryKey = pk
		}
		tables[i].Windowed = tables[i].Name == "events" || tables[i].Name == "daily_event_counts"
	}
}

// onlyProject returns the key's project when it sees exactly one.
func (k *posthogKind) onlyProject(ctx context.Context, host string, header http.Header) (string, error) {
	status, body, err := get(ctx, k.http, host+"/api/projects/", header)
	switch {
	case errors.Is(err, ErrPrivateAddress):
		return "", invalid("that PostHog host is not on the public internet")
	case err != nil:
		return "", refused(err, "could not reach PostHog; check the region or host")
	case status == http.StatusUnauthorized:
		return "", refused(nil, "PostHog rejected this key; check it and the region, then test again")
	case status == http.StatusForbidden:
		return "", invalid("enter the project ID from PostHog project settings")
	case status >= 300:
		return "", refused(nil, "PostHog returned HTTP %d; try again", status)
	}
	var page struct {
		Results []struct {
			ID   json.Number `json:"id"`
			Name string      `json:"name"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		return "", refused(err, "PostHog returned an unexpected project list")
	}
	switch len(page.Results) {
	case 0:
		return "", refused(nil, "this key cannot see any PostHog projects")
	case 1:
		return page.Results[0].ID.String(), nil
	}
	names := make([]string, len(page.Results))
	for i, p := range page.Results {
		names[i] = fmt.Sprintf("%s (%s)", p.Name, p.ID)
	}
	return "", invalid("enter a PostHog project ID: %s", strings.Join(names, ", "))
}

func (k *posthogKind) Hint(secret string) string { return keyHint(strings.TrimSpace(secret), 1) }

const posthogMaxPages = 20_000

// posthogPage is the HogQL page size; tests shrink it to exercise paging.
var posthogPage = 10_000

// posthogQuery is how one object reads. HogQL pages by keyset, so the select
// list ends with the ordering columns and keys says how many of them there are.
type posthogQuery struct {
	sql   func(window, after string) string
	order []string
	keys  int
	daily bool
	row   func(r []any, groupTypes map[int]string) map[string]any
}

var posthogQueries = map[string]posthogQuery{
	"persons": {
		sql: func(window, after string) string {
			return "SELECT created_at, is_identified, properties, toString(id) FROM persons WHERE " + window + " AND (" + after + ")"
		},
		order: []string{"toString(id)"}, keys: 1,
		row: func(r []any, _ map[int]string) map[string]any {
			p := posthogProperties(r[2])
			return map[string]any{"person_id": r[3], "created_at": r[0], "is_identified": r[1],
				"email": p["email"], "name": p["name"], "properties": p}
		},
	},
	"person_distinct_ids": {
		sql: func(window, after string) string {
			return "SELECT toString(person_id), distinct_id FROM person_distinct_ids WHERE " + window + " AND (" + after + ")"
		},
		order: []string{"distinct_id"}, keys: 1,
		row: func(r []any, _ map[int]string) map[string]any {
			return map[string]any{"distinct_id": r[1], "person_id": r[0]}
		},
	},
	"groups": {
		sql: func(window, after string) string {
			return "SELECT created_at, properties, index, key FROM groups WHERE " + window + " AND (" + after + ")"
		},
		order: []string{"index", "key"}, keys: 2,
		row: func(r []any, groupTypes map[int]string) map[string]any {
			p := posthogProperties(r[1])
			index := posthogInt(r[2])
			groupType, ok := groupTypes[index]
			if !ok {
				groupType = fmt.Sprintf("group_%d", index)
			}
			return map[string]any{"group_type": groupType, "group_key": r[3], "name": p["name"], "created_at": r[0], "properties": p}
		},
	},
	"events": {
		sql: func(window, after string) string {
			return "SELECT event, timestamp, distinct_id, toString(person_id), properties, toString(uuid) FROM events WHERE " +
				window + " AND (" + after + ")"
		},
		order: []string{"toString(uuid)"}, keys: 1, daily: true,
		row: func(r []any, _ map[int]string) map[string]any {
			p := posthogProperties(r[4])
			return map[string]any{"event_id": r[5], "event": r[0], "timestamp": r[1], "distinct_id": r[2], "person_id": r[3],
				"session_id": p["$session_id"], "current_url": p["$current_url"], "properties": p}
		},
	},
	"daily_event_counts": {
		sql: func(window, after string) string {
			return "SELECT toString(toDate(timestamp)), count(), event, toString(person_id) FROM events WHERE " + window +
				" AND (" + after + ") GROUP BY toDate(timestamp), event, toString(person_id)"
		},
		order: []string{"event", "toString(person_id)"}, keys: 2, daily: true,
		row: func(r []any, _ map[int]string) map[string]any {
			return map[string]any{"day": r[0], "event": r[2], "person_id": r[3], "events": r[1]}
		},
	},
}

func (k *posthogKind) host(s PostHogSettings) string {
	return map[string]string{"us": k.us, "eu": k.eu, "custom": s.Host}[s.Region]
}

func (k *posthogKind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	var s PostHogSettings
	if err := json.Unmarshal(src.Settings, &s); err != nil {
		return table, invalid("PostHog settings are malformed")
	}
	// Groups are no export model; Capture keeps them to the daily reconcile.
	if s.Mode == posthogExportMode && table.Name != "groups" {
		return table, k.syncExport(ctx, src, s, table, w)
	}
	return table, k.copyRows(ctx, src, table.Name, w)
}

func (k *posthogKind) copyRows(ctx context.Context, src Source, table string, w RowWriter) error {
	q, ok := posthogQueries[table]
	if !ok {
		return invalid("PostHog has no object %q", table)
	}
	var s PostHogSettings
	if err := json.Unmarshal(src.Settings, &s); err != nil {
		return invalid("PostHog settings are malformed")
	}
	host := k.host(s)
	header := http.Header{"Authorization": {"Bearer " + strings.TrimSpace(src.Secret)}}
	if s.ProjectID == "" {
		id, err := k.onlyProject(ctx, host, header)
		if err != nil {
			return err
		}
		s.ProjectID = id
	}
	groupTypes := map[int]string{}
	if table == "groups" {
		groupTypes = k.groupTypes(ctx, host, s.ProjectID, header)
	}
	windows := []string{"1 = 1"}
	if q.daily {
		windows = posthogDays(time.Now().UTC(), s.EventDays)
	}
	queryURL := host + "/api/projects/" + s.ProjectID + "/query/"
	pages := 0
	for _, window := range windows {
		var last []any
		for {
			pages++
			if pages > posthogMaxPages {
				return fmt.Errorf("posthog %s: more than %d pages", table, posthogMaxPages)
			}
			sql := q.sql(window, posthogAfter(q.order, last)) + " ORDER BY " + strings.Join(q.order, ", ") +
				" LIMIT " + strconv.Itoa(posthogPage)
			results, err := k.query(ctx, queryURL, header, sql)
			if err != nil {
				return err
			}
			for _, r := range results {
				if err := w.Write(q.row(r, groupTypes)); err != nil {
					return err
				}
			}
			if len(results) < posthogPage {
				break
			}
			lastRow := results[len(results)-1]
			last = lastRow[len(lastRow)-q.keys:]
		}
	}
	return nil
}

func (k *posthogKind) query(ctx context.Context, queryURL string, header http.Header, sql string) ([][]any, error) {
	payload, err := json.Marshal(map[string]any{
		"query": map[string]string{"kind": "HogQLQuery", "query": sql},
		"name":  "helix foundry sync",
	})
	if err != nil {
		return nil, err
	}
	status, body, err := fetch(ctx, k.http, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, queryURL, bytes.NewReader(payload))
		if err == nil {
			req.Header = header.Clone()
			req.Header.Set("Content-Type", "application/json")
		}
		return req, err
	})
	switch {
	case errors.Is(err, ErrPrivateAddress):
		return nil, invalid("that PostHog host is not on the public internet")
	case err != nil:
		return nil, refused(err, "could not reach PostHog; the sync will retry")
	case status == http.StatusUnauthorized:
		return nil, refused(nil, "PostHog rejected this key; update the connector")
	case status == http.StatusForbidden:
		return nil, refused(nil, "this key is missing a permission; grant Query Read and Project Read")
	case status >= 300:
		return nil, refused(nil, "PostHog returned HTTP %d", status)
	}
	var out struct {
		Results [][]any `json:"results"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode posthog query result: %w", err)
	}
	return out.Results, nil
}

// groupTypes names each group type index; without them groups fall back to group_<index>.
func (k *posthogKind) groupTypes(ctx context.Context, host, projectID string, header http.Header) map[int]string {
	types := map[int]string{}
	status, body, err := get(ctx, k.http, host+"/api/projects/"+projectID+"/groups_types/", header)
	if err != nil || status >= 300 {
		return types
	}
	var list []struct {
		Index json.Number `json:"group_type_index"`
		Name  string      `json:"group_type"`
	}
	if json.Unmarshal(body, &list) != nil {
		var wrapped struct {
			Results json.RawMessage `json:"results"`
		}
		if json.Unmarshal(body, &wrapped) != nil || json.Unmarshal(wrapped.Results, &list) != nil {
			return types
		}
	}
	for _, t := range list {
		if i, err := t.Index.Int64(); err == nil {
			types[int(i)] = t.Name
		}
	}
	return types
}

// posthogDays splits the last days days into UTC-day windows, oldest first.
func posthogDays(now time.Time, days int) []string {
	today := now.Truncate(24 * time.Hour)
	windows := make([]string, 0, days)
	for i := days - 1; i >= 0; i-- {
		start := today.AddDate(0, 0, -i)
		windows = append(windows, fmt.Sprintf("timestamp >= toDateTime(%s, 'UTC') AND timestamp < toDateTime(%s, 'UTC')",
			posthogLiteral(start.Format(time.DateTime)), posthogLiteral(start.AddDate(0, 0, 1).Format(time.DateTime))))
	}
	return windows
}

// posthogAfter is the keyset condition for rows after last, which holds the
// final row's ordering values.
func posthogAfter(order []string, last []any) string {
	if last == nil {
		return "1 = 1"
	}
	clauses := make([]string, len(order))
	for i, column := range order {
		var parts []string
		for j := 0; j < i; j++ {
			parts = append(parts, order[j]+" = "+posthogLiteral(last[j]))
		}
		parts = append(parts, column+" > "+posthogLiteral(last[i]))
		clauses[i] = "(" + strings.Join(parts, " AND ") + ")"
	}
	return strings.Join(clauses, " OR ")
}

func posthogLiteral(v any) string {
	if n, ok := v.(float64); ok && !math.IsInf(n, 0) && !math.IsNaN(n) {
		return strconv.FormatFloat(n, 'f', -1, 64)
	}
	s := fmt.Sprint(v)
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}

// posthogProperties decodes a properties column, which HogQL returns as a JSON string.
func posthogProperties(v any) map[string]any {
	switch v := v.(type) {
	case map[string]any:
		return v
	case string:
		var p map[string]any
		if json.Unmarshal([]byte(v), &p) == nil && p != nil {
			return p
		}
	}
	return map[string]any{}
}

func posthogInt(v any) int {
	switch v := v.(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}
