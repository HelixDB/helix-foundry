package syncer

import (
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

// databuddyOrg is a Databuddy organization a test edits between syncs: one
// website, its sessions by id, and each query it was asked, as type@startDate.
type databuddyOrg struct {
	mu       sync.Mutex
	sessions map[string]int
	asked    []string
}

func (o *databuddyOrg) serve(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		switch r.URL.Path {
		case "/v1/query/websites":
			_, _ = w.Write([]byte(`{"success":true,"websites":[{"id":"web_a","name":"Acme","domain":"acme.com"}],"total":1}`))
		case "/v1/query":
			var body struct {
				Parameters []string `json:"parameters"`
				StartDate  string   `json:"startDate"`
				Limit      int      `json:"limit"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode query: %v", err)
			}
			var results []string
			for _, p := range body.Parameters {
				if body.Limit > 1 {
					o.asked = append(o.asked, p)
				}
				var rows []string
				switch p {
				case "events_by_date":
					rows = []string{`{"date":"2026-11-15","pageviews":10}`}
				case "top_pages":
					rows = []string{`{"name":"/","pageviews":10}`}
				case "top_referrers":
					rows = []string{`{"name":"google.com","pageviews":4}`}
				case "custom_events":
					rows = []string{`{"name":"signup","total_events":2}`}
				case "custom_events_trends_by_event":
					rows = []string{fmt.Sprintf(`{"date":%q,"event_name":"signup","total_events":1}`, time.Now().UTC().Format(time.DateOnly))}
				case "custom_events_recent":
					// One occurrence today; every other day has none.
					if body.StartDate == time.Now().UTC().Format(time.DateOnly) {
						rows = []string{fmt.Sprintf(`{"event_name":"signup","properties":"{\"plan\":\"pro\"}","session_id":"s1","timestamp":%q}`,
							time.Now().UTC().Format(time.DateTime))}
					}
				case "session_list":
					for _, id := range slices.Sorted(maps.Keys(o.sessions)) {
						rows = append(rows, fmt.Sprintf(`{"session_id":%q,"visitor_id":"v1","page_views":%d}`, id, o.sessions[id]))
					}
				case "profile_list":
					rows = []string{`{"visitor_id":"v1","session_count":1}`}
				}
				results = append(results, fmt.Sprintf(`{"parameter":%q,"success":true,"data":[%s]}`, p, strings.Join(rows, ",")))
			}
			_, _ = fmt.Fprintf(w, `{"success":true,"data":[%s]}`, strings.Join(results, ","))
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// took are the query types asked since the last call, sorted, each once:
// occurrences are asked a day at a time.
func (o *databuddyOrg) took() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	asked := o.asked
	o.asked = nil
	slices.Sort(asked)
	return slices.Compact(asked)
}

func databuddyFixture(t *testing.T, code string, org *databuddyOrg) fixture {
	t.Helper()
	f := newFixture(t, "http://unused", "databuddy", `{"history_days":90}`, plan{code: code})
	endpoints := connector.DefaultEndpoints()
	endpoints.Databuddy = org.serve(t)
	f.syncer.Catalog = connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{}, connector.EventsOptions{})
	return f
}

var databuddyTableNames = []string{"custom_event_occurrences", "custom_events", "custom_events_daily", "daily_metrics", "pages", "profiles", "referrers", "sessions", "websites"}

// TestDatabuddySyncs runs Databuddy through the syncer on each plan. Free
// includes the kind and reads every table in full on every sync. Pro reads
// every table in full first, then follows daily metrics and sessions with
// change reads: a new session joins and an old one stays, a changed one is
// replaced, and the totals over the window are read in full in the same sync.
func TestDatabuddySyncs(t *testing.T) {
	t.Run("free reads every table in full on every sync", func(t *testing.T) {
		org := &databuddyOrg{sessions: map[string]int{"s1": 1}}
		f := databuddyFixture(t, "free", org)
		for _, id := range []string{"sync-1", "sync-2"} {
			res := f.run(t, id)
			if res.Mode != model.ConnectorSyncFull || !slices.Equal(slices.Sorted(maps.Keys(f.store.tables)), databuddyTableNames) {
				t.Fatalf("%s: mode = %s, tables = %v", id, res.Mode, slices.Sorted(maps.Keys(f.store.tables)))
			}
			if asked := org.took(); len(asked) != 8 {
				t.Fatalf("%s: asked %v", id, asked)
			}
		}
		rows := f.snapshotRows(t, "custom_event_occurrences")
		if len(rows) != 1 || !strings.Contains(rows[0], `"properties":{"plan":"pro"}`) || !strings.Contains(rows[0], `"website_id":"web_a"`) {
			t.Fatalf("occurrences = %v", rows)
		}
		if rows := f.snapshotRows(t, "pages"); fmt.Sprint(rows) != `[{"pageviews":10,"path":"/","website_id":"web_a"}]` {
			t.Fatalf("pages = %v", rows)
		}
	})

	t.Run("pro follows daily metrics and sessions between full reads", func(t *testing.T) {
		org := &databuddyOrg{sessions: map[string]int{"s1": 1, "s2": 1}}
		f := databuddyFixture(t, "pro", org)
		if res := f.run(t, "sync-1"); res.Mode != model.ConnectorSyncFull || len(f.store.tables) != len(databuddyTableNames) {
			t.Fatalf("mode = %s, tables = %v", res.Mode, slices.Sorted(maps.Keys(f.store.tables)))
		}
		org.took()
		if f.store.connectors["conn-1"].ChangeCursor == nil {
			t.Fatal("the full read stored no change cursor")
		}

		// s1 grows and s3 starts; Databuddy's list no longer holds s2, as it
		// would for a session that left the window: the change read keeps it.
		org.mu.Lock()
		org.sessions = map[string]int{"s1": 4, "s3": 1}
		org.mu.Unlock()
		res := f.run(t, "sync-2")
		if res.Mode != model.ConnectorSyncChanges {
			t.Fatalf("mode = %s", res.Mode)
		}
		if asked := org.took(); len(asked) != 8 {
			t.Fatalf("asked %v; want the followed tables' changes and the others in full", asked)
		}
		want := []string{
			`{"page_views":1,"session_id":"s2","visitor_id":"v1","website_id":"web_a"}`,
			`{"page_views":1,"session_id":"s3","visitor_id":"v1","website_id":"web_a"}`,
			`{"page_views":4,"session_id":"s1","visitor_id":"v1","website_id":"web_a"}`,
		}
		if rows := f.snapshotRows(t, "sessions"); !slices.Equal(rows, want) {
			t.Fatalf("sessions = %v", rows)
		}
		if !slices.Contains(res.Changed, "sessions") {
			t.Fatalf("changed = %v", res.Changed)
		}
	})
}
