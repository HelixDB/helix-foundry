package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
)

// postHogAPI is the query API export mode still calls: the project list,
// and groups. It records every HogQL query it is sent.
type postHogAPI struct {
	url     string
	mu      sync.Mutex
	queries []string
}

func newPostHogAPI(t *testing.T) *postHogAPI {
	t.Helper()
	api := &postHogAPI{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/projects/":
			fmt.Fprint(w, `{"results":[{"id":42,"name":"Web"}]}`)
		case "/api/projects/42/groups_types/":
			fmt.Fprint(w, `[{"group_type_index":0,"group_type":"company"}]`)
		case "/api/projects/42/query/":
			var body struct {
				Query struct {
					Query string `json:"query"`
				} `json:"query"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			api.mu.Lock()
			api.queries = append(api.queries, body.Query.Query)
			api.mu.Unlock()
			if strings.Contains(body.Query.Query, "FROM groups") {
				fmt.Fprint(w, `{"results":[["2024-01-01","{\"name\":\"Acme\"}",0,"acme"]]}`)
				return
			}
			w.WriteHeader(http.StatusBadRequest)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	api.url = server.URL
	return api
}

func (a *postHogAPI) seen() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.queries)
}

// postHogExports is a PostHog connector in export mode over a fake bucket,
// STS and query API.
func postHogExports(t *testing.T) (*posthogKind, *s3Harness, *postHogAPI, Source) {
	t.Helper()
	h := newS3Harness(t, nil)
	api := newPostHogAPI(t)
	kind := &posthogKind{us: api.url, eu: api.url, http: Guard{AllowPrivate: true}.HTTPClient(), files: h.kind}
	raw, err := kind.Prepare(Source{Secret: "phx_abc", Settings: json.RawMessage(
		`{"project_id":"42","mode":"export","export_bucket":{"bucket":"acme-exports","prefix":"posthog","role":{"arn":"` + customerRole + `"}}}`)})
	if err != nil {
		t.Fatal(err)
	}
	return kind, h, api, Source{Settings: raw, Secret: "phx_abc", WorkspaceID: "ws-1"}
}

// eventsParquet is an events export file as PostHog writes Parquet:
// properties a JSON string, timestamps in microseconds UTC.
func eventsParquet(t *testing.T, rows string) string {
	t.Helper()
	ts := &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}
	sc := arrow.NewSchema([]arrow.Field{
		{Name: "uuid", Type: arrow.BinaryTypes.String}, {Name: "event", Type: arrow.BinaryTypes.String},
		{Name: "distinct_id", Type: arrow.BinaryTypes.String}, {Name: "person_id", Type: arrow.BinaryTypes.String},
		{Name: "properties", Type: arrow.BinaryTypes.String}, {Name: "timestamp", Type: ts}, {Name: "created_at", Type: ts},
	}, nil)
	rec, _, err := array.RecordFromJSON(memory.DefaultAllocator, sc, strings.NewReader(rows))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := pqarrow.NewFileWriter(sc, &buf, parquet.NewWriterProperties(), pqarrow.DefaultWriterProps())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// day is the UTC date n days ago, inside a 30-day window unless n is 30 or more.
func day(n int) string { return time.Now().UTC().AddDate(0, 0, -n).Format(time.DateOnly) }

// putPostHogExports fills the bucket with one export of each model: events
// in Parquet and JSON Lines, one event repeated and one outside the window,
// and persons with an older version written after a newer one and a deleted person.
func putPostHogExports(t *testing.T, f *fakeS3) {
	t.Helper()
	f.put("posthog/events/2026-10-20/a.parquet.zst", eventsParquet(t, `[
		{"uuid":"e1","event":"$pageview","distinct_id":"d1","person_id":"p1","properties":"{\"$current_url\":\"/a\",\"$session_id\":\"s1\",\"n\":1.50}",
		 "timestamp":"`+day(1)+`T10:00:00.5Z","created_at":"`+day(1)+`T10:00:01Z"},
		{"uuid":"e2","event":"signup","distinct_id":"d3","person_id":"p2","properties":"{\"plan\":\"pro\"}","timestamp":"`+day(1)+`T11:00:00Z","created_at":"`+day(1)+`T11:00:00Z"}]`))
	f.put("posthog/events/2026-10-21/b.jsonl",
		`{"uuid":"e1","event":"$pageview","distinct_id":"d1","person_id":"p1","properties":{"$current_url":"/a","$session_id":"s1","n":1.50},"timestamp":"`+day(1)+`T10:00:00.500000+00:00","created_at":"`+day(1)+`T10:00:01+00:00","_inserted_at":"x"}`+"\n"+
			`{"uuid":"e3","event":"$pageview","distinct_id":"d1","person_id":"p1","properties":{},"timestamp":"`+day(40)+`T10:00:00+00:00","created_at":"`+day(40)+`T10:00:00+00:00"}`+"\n"+
			`{"uuid":"e4","event":"$pageview","distinct_id":"d2","person_id":"p1","properties":{"$current_url":"/b"},"timestamp":"`+day(0)+`T00:00:00+00:00","created_at":"`+day(0)+`T00:00:00+00:00"}`+"\n")
	f.put("posthog/events/2026-10-21/manifest.json", `{"files":["b.jsonl"]}`)
	f.put("posthog/persons/2026-10-21/a.jsonl",
		`{"team_id":42,"distinct_id":"d1","person_id":"p1","properties":{"email":"ada@x.io","name":"Ada"},"person_distinct_id_version":1,"person_version":1,"created_at":"2026-01-01T00:00:00+00:00","is_deleted":false}`+"\n"+
			`{"team_id":42,"distinct_id":"d2","person_id":"p1","properties":{"email":"ada@x.io","name":"Ada"},"person_distinct_id_version":1,"person_version":1,"created_at":"2026-01-01T00:00:00+00:00","is_deleted":false}`+"\n"+
			`{"team_id":42,"distinct_id":"d3","person_id":"p2","properties":{"email":"bo@x.io"},"person_distinct_id_version":1,"person_version":3,"created_at":"2026-02-01T00:00:00+00:00","is_deleted":false}`+"\n"+
			`{"team_id":42,"distinct_id":"d3","person_id":"p2","properties":{"email":"old@x.io"},"person_distinct_id_version":0,"person_version":2,"created_at":"2026-02-01T00:00:00+00:00","is_deleted":false}`+"\n"+
			`{"team_id":42,"distinct_id":"d4","person_id":"p3","properties":{},"person_distinct_id_version":2,"person_version":5,"created_at":"2026-03-01T00:00:00+00:00","is_deleted":true}`+"\n")
}

func TestPostHogExportSettings(t *testing.T) {
	// The bucket is normalised as an s3 connector's is, and the API key stays:
	// groups are still read through the query API.
	t.Run("stores the export bucket", func(t *testing.T) {
		kind, _, _, src := postHogExports(t)
		want := `{"region":"us","project_id":"42","event_days":30,"mode":"export","export_bucket":{"bucket":"acme-exports","prefix":"posthog/","role":{"arn":"` + customerRole + `"}}}`
		if string(src.Settings) != want {
			t.Fatalf("settings = %s", src.Settings)
		}
		if principal, externalID, ok := kind.Setup("ws-9"); !ok || principal != testConnectorRole || externalID != "helix-foundry-ws-9" {
			t.Fatalf("setup = %s %s %v", principal, externalID, ok)
		}
	})
	role := `"role":{"arn":"` + customerRole + `"}`
	// Helix reads exports only through the customer's role, never with a key
	// it would have to hold, and a bucket outside export mode is a mistake.
	for name, tc := range map[string]struct{ settings, want string }{
		"no bucket":       {`{"mode":"export"}`, "enter the bucket your PostHog batch exports write to"},
		"an access key":   {`{"mode":"export","export_bucket":{"bucket":"acme-exports","keys":{"access_key_id":"AKIAEXAMPLE"}}}`, "Helix reads PostHog exports through an IAM role; remove the access key"},
		"no role":         {`{"mode":"export","export_bucket":{"bucket":"acme-exports"}}`, msgRoleARN},
		"a bad bucket":    {`{"mode":"export","export_bucket":{"bucket":"Acme",` + role + `}}`, msgBucket},
		"a Helix bucket":  {`{"mode":"export","export_bucket":{"bucket":"example-foundry-data",` + role + `}}`, "example-foundry-data is a Helix bucket"},
		"an unknown mode": {`{"mode":"stream"}`, "PostHog mode must be export, or empty to use the query API"},
		"a query-mode bucket": {`{"export_bucket":{"bucket":"acme-exports",` + role + `}}`,
			"an export bucket is only read in export mode"},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			kind, _, _, _ := postHogExports(t)
			_, err := kind.Prepare(Source{Secret: "phx_abc", Settings: json.RawMessage(tc.settings)})
			if got := invalidMessage(t, err); got != tc.want {
				t.Fatalf("message = %q, want %q", got, tc.want)
			}
		})
	}
	// Where role access is off there is no way to read an export.
	t.Run("refuses export mode without role access", func(t *testing.T) {
		kind := &posthogKind{}
		_, err := kind.Prepare(Source{Secret: "phx_abc", Settings: json.RawMessage(`{"mode":"export","export_bucket":{"bucket":"acme-exports",` + role + `}}`)})
		if got := invalidMessage(t, err); got != msgNoRole {
			t.Fatalf("message = %q", got)
		}
		if _, _, ok := kind.Setup("ws-1"); ok {
			t.Fatal("setup offered without role access")
		}
	})
}

func TestPostHogExports(t *testing.T) {
	ctx := context.Background()

	// Ontologies built on a query-mode connector must keep working, so export
	// mode reports the same tables with the same keys, from the bucket.
	t.Run("reports query mode's tables and keys", func(t *testing.T) {
		kind, h, api, src := postHogExports(t)
		putPostHogExports(t, h.s3)
		d, err := kind.Discover(ctx, src)
		if err != nil {
			t.Fatal(err)
		}
		if got := tableNamesOf(d); !slices.Equal(got, []string{"persons", "person_distinct_ids", "groups", "events", "daily_event_counts"}) {
			t.Fatalf("tables = %v, unavailable = %v", got, d.Unavailable)
		}
		keys := map[string][]string{}
		for _, table := range d.Tables {
			keys[table.Name] = table.PrimaryKey
			if table.Windowed != (table.Name == "events" || table.Name == "daily_event_counts") {
				t.Fatalf("%s windowed = %v", table.Name, table.Windowed)
			}
		}
		want := map[string][]string{"persons": {"person_id"}, "person_distinct_ids": {"distinct_id"}, "groups": {"group_type", "group_key"},
			"events": {"event_id"}, "daily_event_counts": {"day", "event", "person_id"}}
		if fmt.Sprint(keys) != fmt.Sprint(want) {
			t.Fatalf("keys = %v", keys)
		}
		if events := d.Tables[3]; len(events.Files) != 2 || events.Files[0].Format.String() != "parquet" {
			t.Fatalf("events files = %+v", events.Files)
		}
		if !strings.Contains(string(d.Settings), `"region":"eu-west-2"`) {
			t.Fatalf("settings = %s", d.Settings)
		}
		if got := api.seen(); !slices.Equal(got, []string{"SELECT 1 FROM groups LIMIT 1"}) {
			t.Fatalf("queries = %q", got)
		}
	})

	// An export that hasn't landed yet leaves its tables out, saying why,
	// rather than syncing them empty.
	t.Run("says which model has no export yet", func(t *testing.T) {
		kind, h, _, src := postHogExports(t)
		h.s3.put("posthog/events/2026-10-21/b.jsonl", `{"uuid":"e1","event":"x","timestamp":"`+day(0)+`T00:00:00Z"}`+"\n")
		d, err := kind.Discover(ctx, src)
		if err != nil {
			t.Fatal(err)
		}
		want := []Unavailable{{"persons", "no PostHog persons export under s3://acme-exports/posthog/persons/ yet"},
			{"person_distinct_ids", "no PostHog persons export under s3://acme-exports/posthog/persons/ yet"}}
		if !slices.Equal(d.Unavailable, want) {
			t.Fatalf("unavailable = %v", d.Unavailable)
		}
	})

	// A role any Helix workspace could assume would expose the bucket across
	// workspaces; export mode refuses it as the s3 kind does.
	t.Run("refuses a role whose trust lacks the external id", func(t *testing.T) {
		kind, h, _, src := postHogExports(t)
		putPostHogExports(t, h.s3)
		h.sts.roles[customerRole] = trustOpen
		_, err := kind.Discover(ctx, src)
		if got := invalidMessage(t, err); !strings.Contains(got, "must require external ID helix-foundry-ws-1") {
			t.Fatalf("message = %q", got)
		}
	})

	// The full read gives each table the rows query mode gives: one row per
	// event in the window, its last copy winning, each person and distinct id
	// at its newest version, deleted ones dropped. Parquet and JSON Lines
	// give the same row for the same event.
	t.Run("builds query mode's rows from the files", func(t *testing.T) {
		kind, h, _, src := postHogExports(t)
		putPostHogExports(t, h.s3)
		_, _, rows := fileFullRead(t, kind, src)
		e2 := `{"created_at":"` + day(1) + `T11:00:00Z","current_url":null,"distinct_id":"d3","event":"signup","event_id":"e2","person_id":"p2",` +
			`"properties":{"plan":"pro"},"session_id":null,"timestamp":"` + day(1) + `T11:00:00Z"}`
		e1 := `{"created_at":"` + day(1) + `T10:00:01Z","current_url":"/a","distinct_id":"d1","event":"$pageview","event_id":"e1","person_id":"p1",` +
			`"properties":{"$current_url":"/a","$session_id":"s1","n":1.50},"session_id":"s1","timestamp":"` + day(1) + `T10:00:00.5Z"}`
		if len(rows["events"]) != 3 || rows["events"][0] != e2 || rows["events"][1] != e1 || !strings.Contains(rows["events"][2], `"event_id":"e4"`) {
			t.Fatalf("events = %q", rows["events"])
		}
		wantCounts := []string{
			`{"day":"` + day(1) + `","event":"$pageview","events":1,"person_id":"p1"}`,
			`{"day":"` + day(1) + `","event":"signup","events":1,"person_id":"p2"}`,
			`{"day":"` + day(0) + `","event":"$pageview","events":1,"person_id":"p1"}`,
		}
		if !slices.Equal(rows["daily_event_counts"], wantCounts) {
			t.Fatalf("daily_event_counts = %q", rows["daily_event_counts"])
		}
		wantPersons := []string{
			`{"created_at":"2026-01-01T00:00:00Z","email":"ada@x.io","name":"Ada","person_id":"p1","person_version":1,"properties":{"email":"ada@x.io","name":"Ada"}}`,
			`{"created_at":"2026-02-01T00:00:00Z","email":"bo@x.io","name":null,"person_id":"p2","person_version":3,"properties":{"email":"bo@x.io"}}`,
		}
		if !slices.Equal(rows["persons"], wantPersons) {
			t.Fatalf("persons = %q", rows["persons"])
		}
		wantIDs := []string{`{"distinct_id":"d1","person_distinct_id_version":1,"person_id":"p1"}`,
			`{"distinct_id":"d2","person_distinct_id_version":1,"person_id":"p1"}`, `{"distinct_id":"d3","person_distinct_id_version":1,"person_id":"p2"}`}
		if !slices.Equal(rows["person_distinct_ids"], wantIDs) {
			t.Fatalf("person_distinct_ids = %q", rows["person_distinct_ids"])
		}
		if !strings.Contains(rows["groups"][0], `"group_type":"company"`) {
			t.Fatalf("groups = %q", rows["groups"])
		}
	})

	// Exports are append-only, so a sync reads only the files that landed
	// since the last: events are upserted by uuid, persons and distinct ids
	// take their newest versions, and a deletion deletes. Every upsert is the
	// row a full read would write, or the merge would drift from it.
	t.Run("reads only new export files", func(t *testing.T) {
		kind, h, _, src := postHogExports(t)
		putPostHogExports(t, h.s3)
		cursor, stored, _ := fileFullRead(t, kind, src)
		h.s3.put("posthog/events/2026-10-22/c.jsonl",
			`{"uuid":"e1","event":"$pageview","distinct_id":"d1","person_id":"p1","properties":{"$current_url":"/a","$session_id":"s1","n":1.50},"timestamp":"`+day(1)+`T10:00:00.5Z","created_at":"`+day(1)+`T10:00:01Z","_inserted_at":"y"}`+"\n"+
				`{"uuid":"e5","event":"signup","distinct_id":"d1","person_id":"p1","properties":{},"timestamp":"`+day(0)+`T01:00:00Z","created_at":"`+day(0)+`T01:00:00Z"}`+"\n")
		h.s3.put("posthog/persons/2026-10-22/b.jsonl",
			`{"distinct_id":"d2","person_id":"p2","properties":{"email":"bo@x.io"},"person_distinct_id_version":2,"person_version":3,"created_at":"2026-02-01T00:00:00Z","is_deleted":false}`+"\n"+
				`{"distinct_id":"d1","person_id":"p1","properties":{"email":"ada@x.io","name":"Ada L"},"person_distinct_id_version":1,"person_version":2,"created_at":"2026-01-01T00:00:00Z","is_deleted":false}`+"\n"+
				`{"distinct_id":"d3","person_id":"p2","properties":{},"person_distinct_id_version":2,"person_version":4,"created_at":"2026-02-01T00:00:00Z","is_deleted":true}`+"\n")
		before := len(h.s3.seen())
		log, next, err := fileChangeRead(t, kind, src, cursor, stored)
		if err != nil {
			t.Fatal(err)
		}
		if gets := h.s3.objectGets(before); !slices.Equal(gets, []string{"posthog/events/2026-10-22/c.jsonl", "posthog/persons/2026-10-22/b.jsonl"}) {
			t.Fatalf("downloaded %v", gets)
		}
		if len(log.resets) != 0 || !slices.Equal(log.deletes["persons"], []string{`{"person_id":"p2"}`}) ||
			!slices.Equal(log.deletes["person_distinct_ids"], []string{`{"distinct_id":"d3"}`}) {
			t.Fatalf("resets = %v, deletes = %v", log.resets, log.deletes)
		}
		// A delete carries its version, so a replayed old one can't remove a newer row.
		if fmt.Sprint(log.versions["persons"]) != "[4]" || fmt.Sprint(log.versions["person_distinct_ids"]) != "[2]" {
			t.Fatalf("delete versions = %v", log.versions)
		}
		_, _, after := fileFullRead(t, kind, src)
		for table, n := range map[string]int{"events": 2, "persons": 1, "person_distinct_ids": 2} {
			if len(log.upserts[table]) != n {
				t.Fatalf("%s upserts = %q", table, log.upserts[table])
			}
			for _, row := range log.upserts[table] {
				if !slices.Contains(after[table], row) {
					t.Fatalf("%s upsert %s is not a full read's row: %q", table, row, after[table])
				}
			}
		}
		if !slices.Equal(after["person_distinct_ids"], []string{`{"distinct_id":"d2","person_distinct_id_version":2,"person_id":"p2"}`,
			`{"distinct_id":"d1","person_distinct_id_version":1,"person_id":"p1"}`}) {
			t.Fatalf("person_distinct_ids after = %q", after["person_distinct_ids"])
		}
		if log, _, err := fileChangeRead(t, kind, src, next, stored); err != nil || !log.empty() {
			t.Fatalf("next read = %+v, %v", log, err)
		}
	})

	// A rewritten export file may have dropped rows a merge would keep, so
	// its model's tables are read in full.
	t.Run("reads a model in full when an export file is rewritten", func(t *testing.T) {
		kind, h, _, src := postHogExports(t)
		putPostHogExports(t, h.s3)
		cursor, stored, _ := fileFullRead(t, kind, src)
		h.s3.put("posthog/persons/2026-10-21/a.jsonl", `{"distinct_id":"d1","person_id":"p1","person_version":9}`+"\n")
		log, _, err := fileChangeRead(t, kind, src, cursor, stored)
		if err != nil {
			t.Fatal(err)
		}
		if log.resets["persons"] == "" || log.resets["person_distinct_ids"] == "" || log.resets["events"] != "" || len(log.upserts) != 0 {
			t.Fatalf("log = %+v", log)
		}
	})

	// A backfill exports old versions after newer ones; the merge compares
	// these columns so a change read never goes back.
	t.Run("orders persons and distinct ids by their export versions", func(t *testing.T) {
		kind, h, _, src := postHogExports(t)
		putPostHogExports(t, h.s3)
		src.ForSync = true
		d, err := kind.Discover(context.Background(), src)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, table := range d.Tables {
			got[table.Name] = table.Version
		}
		if got["persons"] != "person_version" || got["person_distinct_ids"] != "person_distinct_id_version" || got["events"] != "" {
			t.Fatalf("versions = %v", got)
		}
	})
	// Counts can't be followed from new files, and groups only through the
	// query API, which PostHog forbids for recurring reads: groups are
	// followed so only the daily reconcile reads them. Query mode follows nothing.
	t.Run("follows only the tables new files can update", func(t *testing.T) {
		kind, _, _, src := postHogExports(t)
		for table, want := range map[string]Capture{"events": CaptureCursor, "persons": CaptureCursor, "person_distinct_ids": CaptureCursor,
			"daily_event_counts": CaptureFull, "groups": CaptureCursor} {
			if got := kind.Capture(src, table); got != want {
				t.Fatalf("Capture(%s) = %s", table, got)
			}
		}
		if got := kind.Capture(Source{Settings: json.RawMessage(`{"region":"us","event_days":30}`)}, "events"); got != CaptureFull {
			t.Fatalf("query mode Capture = %s", got)
		}
	})
}
