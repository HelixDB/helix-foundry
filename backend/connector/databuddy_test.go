package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

const databuddyTestKey = "dbdy_0123456789abcdef0123456789abcdef"

const databuddySites = `{"success":true,"websites":[
	{"id":"web_a","name":"Acme","domain":"acme.com","isPublic":false,"createdAt":"2024-01-01T00:00:00.000Z"},
	{"id":"web_b","name":"Docs","domain":"docs.acme.com","isPublic":true,"createdAt":"2024-02-01T00:00:00.000Z"}],"total":2}`

// databuddyQuery is one POST /v1/query as the server saw it.
type databuddyQuery struct {
	site string
	body databuddyRequest
}

// databuddyServer answers the websites list with sites and each query with
// answer, after checking the key and the timezone every query names.
func databuddyServer(t *testing.T, sites string, answer func(q databuddyQuery) (int, string)) (Kind, *[]databuddyQuery) {
	t.Helper()
	noBackoff(t)
	var queries []databuddyQuery
	url := routeServer(t, map[string]func(*http.Request) (int, string){
		"/v1/query/websites": func(r *http.Request) (int, string) {
			if r.Method != http.MethodGet || r.Header.Get("X-Api-Key") != databuddyTestKey {
				t.Errorf("websites: %s with key %q", r.Method, r.Header.Get("X-Api-Key"))
			}
			return http.StatusOK, sites
		},
		"/v1/query": func(r *http.Request) (int, string) {
			var q databuddyQuery
			if err := json.NewDecoder(r.Body).Decode(&q.body); err != nil {
				t.Fatalf("decode query: %v", err)
			}
			q.site = r.URL.Query().Get("website_id")
			if r.Method != http.MethodPost || r.Header.Get("X-Api-Key") != databuddyTestKey ||
				r.Header.Get("Content-Type") != "application/json" || r.URL.Query().Get("timezone") != "UTC" || q.body.TimeZone != "UTC" || q.body.Granularity != "daily" {
				t.Errorf("query: %s %s key %q type %q body %+v", r.Method, r.URL, r.Header.Get("X-Api-Key"), r.Header.Get("Content-Type"), q.body)
			}
			queries = append(queries, q)
			return answer(q)
		},
	})
	return kindOf(t, NewCatalog(Endpoints{Databuddy: url}, Guard{}, S3Options{}, EventsOptions{}), "databuddy"), &queries
}

// databuddyAnswer is a query's answer: each parameter's result, rows from rows.
func databuddyAnswer(q databuddyQuery, rows func(parameter string) []map[string]any) string {
	results := make([]map[string]any, 0, len(q.body.Parameters))
	for _, p := range q.body.Parameters {
		results = append(results, map[string]any{"parameter": p, "success": true, "data": rows(p)})
	}
	b, _ := json.Marshal(map[string]any{"success": true, "data": results})
	return string(b)
}

func databuddySource(settings string) Source {
	return Source{Settings: json.RawMessage(settings), Secret: databuddyTestKey}
}

func TestDatabuddyPrepare(t *testing.T) {
	k := kindOf(t, catalogFor(""), "databuddy")
	for _, c := range []struct{ name, secret, settings, want, err string }{
		{"defaults", databuddyTestKey, ``, `{"history_days":90}`, ""},
		{"null settings", " " + databuddyTestKey + " ", `null`, `{"history_days":90}`, ""},
		{"sites trimmed and deduplicated", databuddyTestKey, `{"website_ids":[" web_a","web_b","web_a "],"history_days":30}`,
			`{"website_ids":["web_a","web_b"],"history_days":30}`, ""},
		{"a year", databuddyTestKey, `{"history_days":365}`, `{"history_days":365}`, ""},
		{"no key", "", `{}`, "", "enter a Databuddy API key"},
		{"another product's key", "sk_live_abc", `{}`, "", "enter a Databuddy API key"},
		{"too long a history", databuddyTestKey, `{"history_days":366}`, "", "keep 1 to 365 days"},
		{"a negative history", databuddyTestKey, `{"history_days":-1}`, "", "keep 1 to 365 days"},
		{"an empty site", databuddyTestKey, `{"website_ids":[" "]}`, "", "a website id is empty"},
		{"an unknown setting", databuddyTestKey, `{"host":"evil.example"}`, "", "takes only its key"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := k.Prepare(Source{Secret: c.secret, Settings: json.RawMessage(c.settings)})
			if c.err != "" {
				var invalidErr *InvalidError
				if !errors.As(err, &invalidErr) || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("err = %v, want InvalidError containing %q", err, c.err)
				}
				return
			}
			if err != nil || string(got) != c.want {
				t.Fatalf("Prepare = %s, %v; want %s", got, err, c.want)
			}
		})
	}
	if hint := k.Hint(" " + databuddyTestKey); hint != "dbdy…cdef" {
		t.Fatalf("hint = %q", hint)
	}
	if k.DisplayName() != "Databuddy" {
		t.Fatalf("display name = %q", k.DisplayName())
	}
}

func TestDatabuddyDiscover(t *testing.T) {
	ctx := context.Background()
	t.Run("lists every table, keyed by site and windowed, from one probe of each site", func(t *testing.T) {
		k, queries := databuddyServer(t, databuddySites, func(q databuddyQuery) (int, string) {
			return http.StatusOK, databuddyAnswer(q, func(string) []map[string]any { return nil })
		})
		d, err := k.Discover(ctx, databuddySource(`{"history_days":90}`))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"websites", "daily_metrics", "pages", "referrers", "custom_events", "custom_events_daily", "custom_event_occurrences", "sessions", "profiles"}
		if !slices.Equal(tableNames(d), want) || len(d.Unavailable) != 0 {
			t.Fatalf("tables = %v, unavailable = %v", tableNames(d), d.Unavailable)
		}
		keys := map[string][]string{"websites": {"website_id"}, "daily_metrics": {"website_id", "date"}, "pages": {"website_id", "path"},
			"referrers": {"website_id", "referrer"}, "custom_events": {"website_id", "event_name"}, "custom_events_daily": {"website_id", "event_name", "date"},
			"custom_event_occurrences": {"website_id", "event_id"}, "sessions": {"website_id", "session_id"}, "profiles": {"website_id", "visitor_id"}}
		for name, key := range keys {
			table := findTable(t, d, name)
			if !slices.Equal(table.PrimaryKey, key) {
				t.Errorf("%s key = %v, want %v", name, table.PrimaryKey, key)
			}
			if windowed := name != "websites" && name != "pages" && name != "referrers" && name != "custom_events"; table.Windowed != windowed {
				t.Errorf("%s windowed = %v", name, table.Windowed)
			}
			if want := []ForeignKey{foreignKey("website_id", "websites", "website_id")}; name != "websites" && !slices.Equal(table.ForeignKeys, want) {
				t.Errorf("%s foreign keys = %v", name, table.ForeignKeys)
			}
		}
		today := time.Now().UTC().Format(time.DateOnly)
		if len(*queries) != 2 {
			t.Fatalf("queries = %+v, want a probe a site", *queries)
		}
		for i, site := range []string{"web_a", "web_b"} {
			q := (*queries)[i]
			if q.site != site || len(q.body.Parameters) != 8 || q.body.Limit != 1 || q.body.StartDate != today || q.body.EndDate != today {
				t.Fatalf("probe = %+v", q)
			}
		}
	})

	// failing answers each probe with every type but the failed ones, by site.
	failing := func(failed map[string]map[string]string) func(q databuddyQuery) (int, string) {
		return func(q databuddyQuery) (int, string) {
			results := []map[string]any{}
			for _, p := range q.body.Parameters {
				why, ok := failed[q.site][p]
				switch {
				case !ok:
					results = append(results, map[string]any{"parameter": p, "success": true, "data": []any{}})
				case why != "":
					results = append(results, map[string]any{"parameter": p, "success": false, "error": why})
				}
			}
			b, _ := json.Marshal(map[string]any{"success": true, "data": results})
			return http.StatusOK, string(b)
		}
	}

	t.Run("a query the key or plan may not run on a site is unavailable with Databuddy's reason", func(t *testing.T) {
		k, _ := databuddyServer(t, databuddySites, failing(map[string]map[string]string{
			"web_a": {"custom_events": " Website permission required "},
			"web_b": {"custom_events": "Website permission required", "profile_list": "Feature unavailable on your plan"},
		}))
		d, err := k.Discover(ctx, databuddySource(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		got := fmt.Sprint(d.Unavailable)
		if got != "[{custom_events Website permission required} {profiles Feature unavailable on your plan}]" {
			t.Fatalf("unavailable = %s", got)
		}
		if slices.Contains(tableNames(d), "custom_events") || slices.Contains(tableNames(d), "profiles") {
			t.Fatalf("tables = %v", tableNames(d))
		}
	})

	t.Run("occurrences are unavailable when the daily counts that size them are", func(t *testing.T) {
		k, _ := databuddyServer(t, databuddySites, failing(map[string]map[string]string{
			"web_b": {"custom_events_trends_by_event": "Website permission required"},
		}))
		d, err := k.Discover(ctx, databuddySource(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(d.Unavailable); got != "[{custom_events_daily Website permission required} {custom_event_occurrences Website permission required}]" {
			t.Fatalf("unavailable = %s", got)
		}
	})

	// A table that was synced and is unavailable pauses its connector, so a
	// failure that may pass is refused, and the sync retried.
	for name, failed := range map[string]map[string]map[string]string{
		"could not run the session_list query (Query timeout); try again": {"web_b": {"session_list": "Query timeout"}},
		"did not answer the profile_list query; try again":                {"web_a": {"profile_list": ""}},
	} {
		t.Run("a query that fails for another reason is refused: "+name, func(t *testing.T) {
			k, _ := databuddyServer(t, databuddySites, failing(failed))
			_, err := k.Discover(ctx, databuddySource(`{}`))
			var refusedErr *RefusedError
			if !errors.As(err, &refusedErr) || !strings.Contains(err.Error(), name) {
				t.Fatalf("err = %v", err)
			}
		})
	}

	t.Run("a key with no website reads only websites", func(t *testing.T) {
		k, queries := databuddyServer(t, `{"success":true,"websites":[],"total":0}`, func(databuddyQuery) (int, string) {
			return http.StatusOK, `{}`
		})
		d, err := k.Discover(ctx, databuddySource(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(tableNames(d), []string{"websites"}) || len(d.Unavailable) != 8 || d.Unavailable[0].Reason != "the key reads no website yet" {
			t.Fatalf("tables = %v, unavailable = %v", tableNames(d), d.Unavailable)
		}
		if len(*queries) != 0 {
			t.Fatalf("queries = %+v", *queries)
		}
	})

	t.Run("a chosen website the key can't read is refused", func(t *testing.T) {
		k, _ := databuddyServer(t, databuddySites, func(databuddyQuery) (int, string) { return http.StatusOK, `{}` })
		_, err := k.Discover(ctx, databuddySource(`{"website_ids":["web_b","web_gone"]}`))
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || !strings.Contains(err.Error(), "cannot read the website web_gone") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("the chosen websites are probed in their order", func(t *testing.T) {
		k, queries := databuddyServer(t, databuddySites, func(q databuddyQuery) (int, string) {
			return http.StatusOK, databuddyAnswer(q, func(string) []map[string]any { return nil })
		})
		if _, err := k.Discover(ctx, databuddySource(`{"website_ids":["web_b"]}`)); err != nil {
			t.Fatal(err)
		}
		if (*queries)[0].site != "web_b" {
			t.Fatalf("probed %q", (*queries)[0].site)
		}
	})

	t.Run("bad settings are invalid", func(t *testing.T) {
		k := kindOf(t, catalogFor(""), "databuddy")
		var invalidErr *InvalidError
		if _, err := k.Discover(ctx, databuddySource(`[`)); !errors.As(err, &invalidErr) {
			t.Fatalf("err = %v", err)
		}
	})

	for _, c := range []struct {
		name   string
		status int
		body   string
		want   string
		auth   bool
	}{
		{"a rejected key", http.StatusUnauthorized, `{"success":false,"error":"Authentication required","code":"AUTH_REQUIRED"}`,
			"Databuddy rejected this key", true},
		{"a key without the scope", http.StatusForbidden, `{"success":false,"error":"Insufficient scope","code":"ACCESS_DENIED"}`,
			"give it the read:data scope (Insufficient scope)", false},
		{"the other envelope", http.StatusPaymentRequired, `{"defined":true,"code":"PLAN_LIMIT_EXCEEDED","status":402,"message":"Upgrade"}`,
			"HTTP 402 for /v1/query/websites: Upgrade", false},
		{"an outage without a body", http.StatusBadGateway, ``, "HTTP 502 for /v1/query/websites; try again", false},
		{"a spent rate limit", http.StatusTooManyRequests, `{"success":false,"code":"RATE_LIMITED"}`, "used up its rate limit", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			noBackoff(t)
			url := routeServer(t, map[string]func(*http.Request) (int, string){
				"/v1/query/websites": func(*http.Request) (int, string) { return c.status, c.body },
			})
			k := kindOf(t, NewCatalog(Endpoints{Databuddy: url}, Guard{}, S3Options{}, EventsOptions{}), "databuddy")
			_, err := k.Discover(ctx, databuddySource(`{}`))
			var refusedErr *RefusedError
			var authErr *AuthError
			if !errors.As(err, &refusedErr) || !strings.Contains(err.Error(), c.want) || errors.As(err, &authErr) != c.auth {
				t.Fatalf("err = %v", err)
			}
		})
	}

	t.Run("a malformed websites answer fails", func(t *testing.T) {
		for _, body := range []string{`not json`, `{"success":false}`, `{"success":true}`} {
			k, _ := databuddyServer(t, body, func(databuddyQuery) (int, string) { return http.StatusOK, `{}` })
			if _, err := k.Discover(ctx, databuddySource(`{}`)); err == nil {
				t.Fatalf("%s: no error", body)
			}
		}
	})

	t.Run("a refused probe fails the test", func(t *testing.T) {
		k, _ := databuddyServer(t, databuddySites, func(databuddyQuery) (int, string) {
			return http.StatusOK, `{"success":false,"error":"Missing website"}`
		})
		_, err := k.Discover(ctx, databuddySource(`{}`))
		if err == nil || !strings.Contains(err.Error(), "Databuddy refused the query: Missing website") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("an unreachable Databuddy is refused", func(t *testing.T) {
		k := kindOf(t, NewCatalog(Endpoints{Databuddy: "http://127.0.0.1:1"}, Guard{}, S3Options{}, EventsOptions{}), "databuddy")
		_, err := k.Discover(ctx, databuddySource(`{}`))
		if err == nil || !strings.Contains(err.Error(), "could not reach Databuddy") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestDatabuddySync(t *testing.T) {
	ctx := context.Background()
	t.Run("writes websites by their id", func(t *testing.T) {
		k, _ := databuddyServer(t, databuddySites, nil)
		var sink rowSink
		if _, err := k.Sync(ctx, databuddySource(`{"website_ids":["web_b"]}`), Table{Name: "websites"}, &sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.rows) != 1 || fmt.Sprint(sink.rows[0]) != "map[created_at:2024-02-01T00:00:00.000Z domain:docs.acme.com is_public:true name:Docs website_id:web_b]" {
			t.Fatalf("rows = %v", sink.rows)
		}
	})

	t.Run("pages each site's query over the window and tags each row", func(t *testing.T) {
		k, queries := databuddyServer(t, databuddySites, func(q databuddyQuery) (int, string) {
			// web_a has a full first page of pages and one more; web_b has one.
			n := 1
			if q.site == "web_a" && q.body.Page == 1 {
				n = q.body.Limit
			}
			return http.StatusOK, databuddyAnswer(q, func(string) []map[string]any {
				rows := make([]map[string]any, n)
				for i := range rows {
					rows[i] = map[string]any{"name": fmt.Sprintf("/%s/%d/%d", q.site, q.body.Page, i), "pageviews": 3, "visitors": 2, "percentage": 1.5}
				}
				return rows
			})
		})
		var sink rowSink
		if _, err := k.Sync(ctx, databuddySource(`{"history_days":30}`), Table{Name: "pages"}, &sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.rows) != 10002 {
			t.Fatalf("rows = %d", len(sink.rows))
		}
		if got := fmt.Sprint(sink.rows[10001]); got != "map[pageviews:3 path:/web_b/1/0 percentage:1.5 visitors:2 website_id:web_b]" {
			t.Fatalf("last row = %s", got)
		}
		now := time.Now().UTC()
		var pages []string
		for _, q := range *queries {
			pages = append(pages, fmt.Sprintf("%s:%d", q.site, q.body.Page))
			if !slices.Equal(q.body.Parameters, []string{"top_pages"}) || q.body.Limit != 10000 ||
				q.body.StartDate != now.AddDate(0, 0, -30).Format(time.DateOnly) || q.body.EndDate != now.Format(time.DateOnly) {
				t.Errorf("query = %+v", q.body)
			}
		}
		if !slices.Equal(pages, []string{"web_a:1", "web_a:2", "web_b:1"}) {
			t.Fatalf("pages = %v", pages)
		}
	})

	t.Run("names each table's name column and drops nested values", func(t *testing.T) {
		k, _ := databuddyServer(t, databuddySites, func(q databuddyQuery) (int, string) {
			return http.StatusOK, databuddyAnswer(q, func(p string) []map[string]any {
				return []map[string]any{{"name": "x", "totalEvents": 4, "events": []any{1}, "meta": map[string]any{"a": 1}}}
			})
		})
		for table, want := range map[string]string{
			"referrers":     "map[referrer:x total_events:4 website_id:web_a]",
			"custom_events": "map[event_name:x total_events:4 website_id:web_a]",
			"daily_metrics": "map[name:x total_events:4 website_id:web_a]",
		} {
			var sink rowSink
			if _, err := k.Sync(ctx, databuddySource(`{"website_ids":["web_a"]}`), Table{Name: table}, &sink); err != nil {
				t.Fatal(err)
			}
			if len(sink.rows) != 1 || fmt.Sprint(sink.rows[0]) != want {
				t.Errorf("%s rows = %v", table, sink.rows)
			}
		}
	})

	t.Run("writes each visitor once, without its sessions' fields", func(t *testing.T) {
		k, _ := databuddyServer(t, databuddySites, func(q databuddyQuery) (int, string) {
			if q.body.Limit != 1000 || q.body.Parameters[0] != "profile_list" {
				t.Errorf("query = %+v", q.body)
			}
			return http.StatusOK, databuddyAnswer(q, func(string) []map[string]any {
				return []map[string]any{
					{"visitor_id": "v1", "session_count": 2, "session_id": "s2", "session_country": "GB", "events": []any{}},
					{"visitor_id": "v1", "session_count": 2, "session_id": "s1"},
					{"visitor_id": "v2", "session_count": 1, "profile_id": "user_7", "ltv": 120},
				}
			})
		})
		var sink rowSink
		if _, err := k.Sync(ctx, databuddySource(`{"website_ids":["web_a"]}`), Table{Name: "profiles"}, &sink); err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(sink.rows); got != "[map[session_count:2 visitor_id:v1 website_id:web_a] map[ltv:120 profile_id:user_7 session_count:1 visitor_id:v2 website_id:web_a]]" {
			t.Fatalf("rows = %s", got)
		}
	})

	t.Run("a refused or missing result fails the read", func(t *testing.T) {
		for body, want := range map[string]string{
			`{"success":true,"data":[{"parameter":"session_list","success":false,"error":"Feature unavailable"}]}`: "Databuddy refused the session_list query: Feature unavailable",
			`{"success":true,"data":[{"parameter":"other","success":true,"data":[]}]}`:                             "the response has no result for it",
			`garbage`: "decode databuddy query",
		} {
			k, _ := databuddyServer(t, databuddySites, func(databuddyQuery) (int, string) { return http.StatusOK, body })
			_, err := k.Sync(ctx, databuddySource(`{}`), Table{Name: "sessions"}, &rowSink{})
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: err = %v", body, err)
			}
		}
	})

	t.Run("a query refused by HTTP status is refused with Databuddy's words", func(t *testing.T) {
		k, _ := databuddyServer(t, databuddySites, func(databuddyQuery) (int, string) {
			return http.StatusForbidden, `{"success":false,"error":"Website access denied","code":"ACCESS_DENIED"}`
		})
		_, err := k.Sync(ctx, databuddySource(`{}`), Table{Name: "pages"}, &rowSink{})
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || !strings.Contains(err.Error(), "may not read /v1/query; give it the read:data scope (Website access denied)") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("fails past the page cap rather than publishing part of a table", func(t *testing.T) {
		k, queries := databuddyServer(t, databuddySites, func(q databuddyQuery) (int, string) {
			return http.StatusOK, databuddyAnswer(q, func(string) []map[string]any {
				return slices.Repeat([]map[string]any{{"date": "2026-11-01"}}, q.body.Limit)
			})
		})
		// A writer with no limit that keeps no rows.
		_, err := k.Sync(ctx, databuddySource(`{"website_ids":["web_a"]}`), Table{Name: "daily_metrics"}, &calcomFullWriter{room: -1})
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) || !strings.Contains(err.Error(), "more daily metrics than Databuddy pages through (1000 pages of 1000); keep fewer days") ||
			len(*queries) != 1000 {
			t.Fatalf("err = %v after %d queries", err, len(*queries))
		}
	})

	t.Run("a writer's error stops the read", func(t *testing.T) {
		k, _ := databuddyServer(t, databuddySites, func(q databuddyQuery) (int, string) {
			return http.StatusOK, databuddyAnswer(q, func(string) []map[string]any { return []map[string]any{{"session_id": "s"}} })
		})
		if _, err := k.Sync(ctx, databuddySource(`{}`), Table{Name: "sessions"}, &calcomFullWriter{}); !errors.Is(err, errCalcomFull) {
			t.Fatalf("err = %v", err)
		}
		if _, err := k.Sync(ctx, databuddySource(`{}`), Table{Name: "websites"}, &calcomFullWriter{}); !errors.Is(err, errCalcomFull) {
			t.Fatalf("websites err = %v", err)
		}
	})

	t.Run("an unknown table, bad settings or a lost site fail", func(t *testing.T) {
		k, _ := databuddyServer(t, databuddySites, nil)
		var invalidErr *InvalidError
		if _, err := k.Sync(ctx, databuddySource(`{}`), Table{Name: "funnels"}, &rowSink{}); !errors.As(err, &invalidErr) {
			t.Errorf("unknown table: %v", err)
		}
		if _, err := k.Sync(ctx, databuddySource(`"x"`), Table{Name: "pages"}, &rowSink{}); !errors.As(err, &invalidErr) {
			t.Errorf("bad settings: %v", err)
		}
		if _, err := k.Sync(ctx, databuddySource(`{"website_ids":["web_gone"]}`), Table{Name: "pages"}, &rowSink{}); err == nil {
			t.Errorf("lost site: no error")
		}
	})

	t.Run("reads Databuddy's times", func(t *testing.T) {
		for v, want := range map[any]string{"2026-11-15 23:50:00": "2026-11-15T23:50:00Z", "2026-11-15 23:50:00.250": "2026-11-15T23:50:00.25Z",
			"2026-11-15T23:50:00.5Z": "2026-11-15T23:50:00.5Z", "2026-11-15T23:50:00+01:00": "2026-11-15T23:50:00+01:00"} {
			if got, ok := databuddyTime(v); !ok || got.Format(time.RFC3339Nano) != want {
				t.Errorf("databuddyTime(%v) = %v, %v", v, got, ok)
			}
		}
		for _, v := range []any{"yesterday", 12.0, nil} {
			if _, ok := databuddyTime(v); ok {
				t.Errorf("databuddyTime(%v) read a time", v)
			}
		}
	})

	t.Run("a number id is written as text", func(t *testing.T) {
		for v, want := range map[any]string{"a": "a", 12.0: "12", 1.5: "1.5", nil: "", true: "true"} {
			if got := databuddyText(v); got != want {
				t.Errorf("databuddyText(%v) = %q", v, got)
			}
		}
	})
}

func TestDatabuddyChanges(t *testing.T) {
	ctx := context.Background()
	tables := []Table{{Name: "websites"}, {Name: "daily_metrics"}, {Name: "sessions"}, {Name: "pages"}}
	// cursorOf is a cursor of days of history and sites; cursorAt one of the defaults and both test sites.
	cursorOf := func(at time.Time, days int, sites []string, names ...string) json.RawMessage {
		times := map[string]time.Time{}
		for _, n := range names {
			times[n] = at
		}
		b, _ := json.Marshal(databuddyCursor{Tables: times, HistoryDays: days, Sites: sites})
		return b
	}
	cursorAt := func(at time.Time, names ...string) json.RawMessage {
		return cursorOf(at, 90, []string{"web_a", "web_b"}, names...)
	}

	t.Run("follows daily metrics and sessions; reads the rest in full", func(t *testing.T) {
		k := kindOf(t, catalogFor(""), "databuddy").(ChangeReader)
		for _, table := range tables {
			want := CaptureFull
			if table.Name == "daily_metrics" || table.Name == "sessions" {
				want = CaptureCursor
			}
			if got := k.Capture(Source{}, table.Name); got != want {
				t.Errorf("%s capture = %s", table.Name, got)
			}
		}
		for _, table := range []string{"custom_events_daily", "custom_event_occurrences"} {
			if got := k.Capture(Source{}, table); got != CaptureCursor {
				t.Errorf("%s capture = %s", table, got)
			}
		}
		for _, table := range []string{"daily_metrics", "custom_events_daily", "custom_event_occurrences", "sessions", "profiles"} {
			if !WindowedTable("databuddy", table) {
				t.Errorf("%s is not windowed", table)
			}
		}
		if WindowedTable("databuddy", "pages") {
			t.Error("pages is windowed")
		}
	})

	t.Run("starts every table at now less the overlap, with its days of history and sites", func(t *testing.T) {
		k, _ := databuddyServer(t, databuddySites, nil)
		reader := k.(ChangeReader)
		before := time.Now()
		start, err := reader.Start(ctx, databuddySource(`{"website_ids":["web_b","web_a"],"history_days":30}`), tables)
		after := time.Now()
		if err != nil || start.Source != nil || start.Close != nil {
			t.Fatalf("start = %+v, %v", start, err)
		}
		var c databuddyCursor
		if err := json.Unmarshal(start.Cursor, &c); err != nil || len(c.Tables) != len(tables) || c.HistoryDays != 30 ||
			!slices.Equal(c.Sites, []string{"web_a", "web_b"}) {
			t.Fatalf("cursor = %s, %v", start.Cursor, err)
		}
		assertTimeCursor(t, c.Tables["sessions"], before, after)
		if _, err := reader.Start(ctx, databuddySource(`[`), tables); err == nil {
			t.Error("bad settings: no error")
		}
		if _, err := reader.Start(ctx, databuddySource(`{"website_ids":["web_gone"]}`), tables); err == nil {
			t.Error("lost site: no error")
		}
	})

	t.Run("a cursor of other days of history or other sites asks for a full read", func(t *testing.T) {
		k, queries := databuddyServer(t, databuddySites, nil)
		for settings, cursor := range map[string]json.RawMessage{
			`{"history_days":30}`:       cursorAt(time.Now(), "sessions"),
			`{"website_ids":["web_a"]}`: cursorAt(time.Now(), "sessions"),
			`{}`:                        cursorOf(time.Now(), 90, []string{"web_a"}, "sessions"),
		} {
			if _, err := k.(ChangeReader).Changes(ctx, databuddySource(settings), tables, cursor, &cursorChanges{}); !errors.Is(err, ErrCursorExpired) {
				t.Errorf("%s: err = %v", settings, err)
			}
		}
		if len(*queries) != 0 {
			t.Fatalf("queries = %v", *queries)
		}
	})

	t.Run("upserts each followed table's rows from the day before its cursor and resets the others", func(t *testing.T) {
		k, queries := databuddyServer(t, databuddySites, func(q databuddyQuery) (int, string) {
			return http.StatusOK, databuddyAnswer(q, func(p string) []map[string]any {
				if p == "session_list" {
					// One began before the read's first day, so came back cut short; one's start is unreadable, so is kept.
					return []map[string]any{{"session_id": "s-" + q.site, "page_views": 2, "first_visit": time.Now().UTC().Format(time.DateTime)},
						{"session_id": "early", "first_visit": "2000-01-01T00:00:00Z"}, {"session_id": "odd-" + q.site, "first_visit": "yesterday"}}
				}
				return []map[string]any{{"date": "2026-11-15", "pageviews": 9}}
			})
		})
		now := time.Now().UTC()
		at := now.Add(-3 * time.Hour)
		cursor := cursorAt(at, "websites", "daily_metrics", "sessions", "pages")
		w := &cursorChanges{}
		before := time.Now()
		next, err := k.(ChangeReader).Changes(ctx, databuddySource(`{"history_days":90}`), tables, cursor, w)
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(w.resets, []string{"websites", "pages"}) {
			t.Fatalf("resets = %v", w.resets)
		}
		var sessions []string
		for _, row := range w.upserts["sessions"] {
			sessions = append(sessions, fmt.Sprint(row["website_id"], "/", row["session_id"]))
		}
		if !slices.Equal(sessions, []string{"web_a/s-web_a", "web_a/odd-web_a", "web_b/s-web_b", "web_b/odd-web_b"}) {
			t.Fatalf("sessions = %v", sessions)
		}
		if len(w.upserts["daily_metrics"]) != 2 {
			t.Fatalf("daily metrics = %v", w.upserts["daily_metrics"])
		}
		since := at.Truncate(24*time.Hour).AddDate(0, 0, -1)
		for _, q := range *queries {
			from := since
			if q.body.Parameters[0] == "session_list" {
				from = since.AddDate(0, 0, -1)
			}
			if q.body.StartDate != from.Format(time.DateOnly) || q.body.EndDate != now.Format(time.DateOnly) {
				t.Errorf("query = %+v, want from %s", q.body, from)
			}
		}
		if len(*queries) != 4 {
			t.Fatalf("queries = %d", len(*queries))
		}
		var c databuddyCursor
		if err := json.Unmarshal(next, &c); err != nil {
			t.Fatal(err)
		}
		assertTimeCursor(t, c.Tables["daily_metrics"], before, after)
	})

	t.Run("reads no further back than the window", func(t *testing.T) {
		k, queries := databuddyServer(t, databuddySites, func(q databuddyQuery) (int, string) {
			return http.StatusOK, databuddyAnswer(q, func(string) []map[string]any { return nil })
		})
		cursor := cursorOf(time.Now().AddDate(0, 0, -40), 7, []string{"web_a"}, "daily_metrics")
		if _, err := k.(ChangeReader).Changes(ctx, databuddySource(`{"website_ids":["web_a"],"history_days":7}`), []Table{{Name: "daily_metrics"}}, cursor, &cursorChanges{}); err != nil {
			t.Fatal(err)
		}
		if want := time.Now().UTC().AddDate(0, 0, -7).Format(time.DateOnly); len(*queries) != 1 || (*queries)[0].body.StartDate != want {
			t.Fatalf("queries = %+v, want from %s", *queries, want)
		}
	})

	t.Run("a table without a cursor time is reset, and nothing is asked when none is followed", func(t *testing.T) {
		k, queries := databuddyServer(t, databuddySites, nil)
		w := &cursorChanges{}
		next, err := k.(ChangeReader).Changes(ctx, databuddySource(`{}`), []Table{{Name: "sessions"}, {Name: "pages"}}, cursorAt(time.Now(), "pages"), w)
		if err != nil || !slices.Equal(w.resets, []string{"sessions", "pages"}) || len(*queries) != 0 {
			t.Fatalf("err = %v, resets = %v, queries = %v", err, w.resets, *queries)
		}
		var c databuddyCursor
		if err := json.Unmarshal(next, &c); err != nil || len(c.Tables) != 2 {
			t.Fatalf("next = %s", next)
		}
	})

	t.Run("an unreadable cursor asks for a full read", func(t *testing.T) {
		k := kindOf(t, catalogFor(""), "databuddy").(ChangeReader)
		if _, err := k.Changes(ctx, databuddySource(`{}`), tables, json.RawMessage(`[]`), &cursorChanges{}); !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("bad settings, a lost site and a failed query fail the read", func(t *testing.T) {
		cursor := cursorAt(time.Now(), "sessions")
		k, _ := databuddyServer(t, databuddySites, func(databuddyQuery) (int, string) { return http.StatusOK, `{"success":false,"error":"down"}` })
		reader := k.(ChangeReader)
		sessions := []Table{{Name: "sessions"}}
		if _, err := reader.Changes(ctx, databuddySource(`7`), sessions, cursor, &cursorChanges{}); err == nil {
			t.Error("bad settings: no error")
		}
		if _, err := reader.Changes(ctx, databuddySource(`{"website_ids":["web_gone"]}`), sessions, cursor, &cursorChanges{}); err == nil {
			t.Error("lost site: no error")
		}
		if _, err := reader.Changes(ctx, databuddySource(`{}`), sessions, cursor, &cursorChanges{}); err == nil || !strings.Contains(err.Error(), "down") {
			t.Errorf("failed query: %v", err)
		}
	})
}

// databuddyEvents is a site's custom events as the two query types that
// read them answer: occurrences by UTC day for custom_events_recent, and
// each day's count, which counts may set apart from the occurrences, for
// custom_events_trends_by_event.
type databuddyEvents struct {
	byDay  map[string][]map[string]any
	counts map[string]float64
}

func (e databuddyEvents) answer(q databuddyQuery) (int, string) {
	return http.StatusOK, databuddyAnswer(q, func(p string) []map[string]any {
		switch p {
		case "custom_events_trends_by_event":
			var rows []map[string]any
			for day, n := range e.counts {
				if day >= q.body.StartDate && day <= q.body.EndDate {
					rows = append(rows, map[string]any{"date": day, "bucket_start": day + "T00:00:00Z", "event_name": "signup_completed", "total_events": n})
				}
			}
			return rows
		case "custom_events_recent":
			if q.body.StartDate != q.body.EndDate {
				panic("occurrences read over more than a day: " + q.body.StartDate + " to " + q.body.EndDate)
			}
			rows := e.byDay[q.body.StartDate]
			start := (q.body.Page - 1) * q.body.Limit
			return rows[min(start, len(rows)):min(start+q.body.Limit, len(rows))]
		}
		return nil
	})
}

// occurrence is a custom_events_recent row at at.
func occurrence(at time.Time, name string, properties any) map[string]any {
	return map[string]any{"event_name": name, "namespace": nil, "path": "/signup", "source": "browser", "properties": properties,
		"anonymous_id": "anon_1", "session_id": "sess_1", "timestamp": at.UTC().Format("2006-01-02 15:04:05.000")}
}

func TestDatabuddyCustomEvents(t *testing.T) {
	ctx := context.Background()
	today := time.Now().UTC().Truncate(24 * time.Hour)
	day := func(ago int) string { return today.AddDate(0, 0, -ago).Format(time.DateOnly) }

	t.Run("writes each event's count of each day", func(t *testing.T) {
		k, queries := databuddyServer(t, databuddySites, databuddyEvents{counts: map[string]float64{day(1): 89, day(8): 31}}.answer)
		var sink rowSink
		if _, err := k.Sync(ctx, databuddySource(`{"website_ids":["web_a"],"history_days":30}`), Table{Name: "custom_events_daily"}, &sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.rows) != 2 || len(*queries) != 1 || (*queries)[0].body.Limit != 10000 || (*queries)[0].body.StartDate != day(30) {
			t.Fatalf("rows = %v, queries = %+v", sink.rows, *queries)
		}
		for _, row := range sink.rows {
			if len(row) != 5 || row["website_id"] != "web_a" || row["event_name"] != "signup_completed" || row["bucket_start"] != fmt.Sprint(row["date"], "T00:00:00Z") {
				t.Errorf("row = %v", row)
			}
		}
	})

	t.Run("reads occurrences a day at a time, paging each day, keyed by their content", func(t *testing.T) {
		at := today.Add(-26 * time.Hour)
		busy := make([]map[string]any, 10001)
		for i := range busy {
			busy[i] = occurrence(at.Add(time.Duration(i)*time.Millisecond), "page_click", `{"n":1}`)
		}
		events := databuddyEvents{
			counts: map[string]float64{day(2): 10001, day(0): 3},
			byDay: map[string][]map[string]any{
				day(2): busy,
				day(0): {
					occurrence(today.Add(time.Hour), "signup_completed", `{"plan":"pro","seats":3}`),
					// The same occurrence read twice, as a page that shifted would: it is written once.
					occurrence(today.Add(time.Hour), "signup_completed", `{"plan":"pro","seats":3}`),
					occurrence(today.Add(2*time.Hour), "signup_completed", "not json"),
					occurrence(today.Add(3*time.Hour), "signup_completed", nil),
				},
			},
		}
		k, queries := databuddyServer(t, databuddySites, events.answer)
		var sink rowSink
		if _, err := k.Sync(ctx, databuddySource(`{"website_ids":["web_a"],"history_days":3}`), Table{Name: databuddyOccurrences}, &sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.rows) != 10004 {
			t.Fatalf("rows = %d", len(sink.rows))
		}
		var asked []string
		for _, q := range *queries {
			asked = append(asked, fmt.Sprintf("%s:%s:%d", q.body.Parameters[0], q.body.StartDate, q.body.Page))
		}
		want := []string{"custom_events_trends_by_event:" + day(3) + ":1",
			"custom_events_recent:" + day(3) + ":1", "custom_events_recent:" + day(2) + ":1", "custom_events_recent:" + day(2) + ":2",
			"custom_events_recent:" + day(1) + ":1", "custom_events_recent:" + day(0) + ":1"}
		if !slices.Equal(asked, want) {
			t.Fatalf("asked %v, want %v", asked, want)
		}
		first := sink.rows[10001]
		if first["timestamp"] != today.Add(time.Hour).Format("2006-01-02T15:04:05.000Z") || first["date"] != day(0) ||
			fmt.Sprint(first["properties"]) != "map[plan:pro seats:3]" || first["website_id"] != "web_a" || first["path"] != "/signup" ||
			first["namespace"] != nil || len(databuddyText(first["event_id"])) != 32 {
			t.Fatalf("row = %v", first)
		}
		if p := sink.rows[10002]["properties"]; p != nil {
			t.Errorf("unreadable properties = %v", p)
		}
		if p := sink.rows[10003]["properties"]; p != nil {
			t.Errorf("missing properties = %v", p)
		}
		ids := map[any]bool{}
		for _, row := range sink.rows {
			ids[row["event_id"]] = true
		}
		if len(ids) != len(sink.rows) {
			t.Fatalf("%d ids for %d rows", len(ids), len(sink.rows))
		}

		// A second read keys every occurrence the same.
		var again rowSink
		if _, err := k.Sync(ctx, databuddySource(`{"website_ids":["web_a"],"history_days":3}`), Table{Name: databuddyOccurrences}, &again); err != nil {
			t.Fatal(err)
		}
		for i, row := range again.rows {
			if row["event_id"] != sink.rows[i]["event_id"] {
				t.Fatalf("row %d keyed %v then %v", i, sink.rows[i]["event_id"], row["event_id"])
			}
		}
	})

	t.Run("keeps the newest whole days that fit, by the daily counts of every site", func(t *testing.T) {
		// Over both sites: today 200k, yesterday 250k (450k), two days ago 100k (550k, past the cap).
		counts := map[string]map[string]float64{
			"web_a": {day(0): 100_000, day(1): 200_000, day(2): 50_000, day(5): 1},
			"web_b": {day(0): 100_000, day(1): 50_000, day(2): 50_000},
		}
		k, queries := databuddyServer(t, databuddySites, func(q databuddyQuery) (int, string) {
			return databuddyEvents{counts: counts[q.site]}.answer(q)
		})
		if _, err := k.Sync(ctx, databuddySource(`{"history_days":30}`), Table{Name: databuddyOccurrences}, &rowSink{}); err != nil {
			t.Fatal(err)
		}
		var read []string
		for _, q := range *queries {
			if q.body.Parameters[0] == "custom_events_recent" {
				read = append(read, q.site+":"+q.body.StartDate)
			}
		}
		if want := []string{"web_a:" + day(1), "web_a:" + day(0), "web_b:" + day(1), "web_b:" + day(0)}; !slices.Equal(read, want) {
			t.Fatalf("read %v, want %v", read, want)
		}
	})

	t.Run("reads the whole window when it fits", func(t *testing.T) {
		k, queries := databuddyServer(t, databuddySites, databuddyEvents{counts: map[string]float64{day(0): maxDatabuddyOccurrences}}.answer)
		if _, err := k.Sync(ctx, databuddySource(`{"website_ids":["web_a"],"history_days":2}`), Table{Name: databuddyOccurrences}, &rowSink{}); err != nil {
			t.Fatal(err)
		}
		if len(*queries) != 4 || (*queries)[1].body.StartDate != day(2) {
			t.Fatalf("queries = %+v", *queries)
		}
	})

	t.Run("a day too large to keep alone is refused", func(t *testing.T) {
		k, queries := databuddyServer(t, databuddySites, databuddyEvents{counts: map[string]float64{day(0): maxDatabuddyOccurrences + 1}}.answer)
		_, err := k.Sync(ctx, databuddySource(`{"website_ids":["web_a"]}`), Table{Name: databuddyOccurrences}, &rowSink{})
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) || !strings.Contains(err.Error(), "more custom events today than Foundry keeps of them (500000)") || len(*queries) != 1 {
			t.Fatalf("err = %v after %d queries", err, len(*queries))
		}
	})

	t.Run("a count without a day or number, or an occurrence without a time, fails the read", func(t *testing.T) {
		for name, answer := range map[string]func(q databuddyQuery) (int, string){
			"a row has no day or count": func(q databuddyQuery) (int, string) {
				return http.StatusOK, databuddyAnswer(q, func(string) []map[string]any { return []map[string]any{{"date": "soon", "total_events": 3}} })
			},
			"no readable timestamp": func(q databuddyQuery) (int, string) {
				return http.StatusOK, databuddyAnswer(q, func(p string) []map[string]any {
					if p == "custom_events_recent" {
						return []map[string]any{{"event_name": "x", "timestamp": "later"}}
					}
					return nil
				})
			},
			"refused the custom_events_trends_by_event query": func(q databuddyQuery) (int, string) {
				return http.StatusOK, `{"success":true,"data":[{"parameter":"custom_events_trends_by_event","success":false,"error":"down"}]}`
			},
		} {
			k, _ := databuddyServer(t, databuddySites, answer)
			if _, err := k.Sync(ctx, databuddySource(`{"website_ids":["web_a"],"history_days":1}`), Table{Name: databuddyOccurrences}, &rowSink{}); err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("%s: err = %v", name, err)
			}
		}
	})

	t.Run("a writer's error stops the read", func(t *testing.T) {
		events := databuddyEvents{byDay: map[string][]map[string]any{day(0): {occurrence(today, "x", "{}")}}}
		k, _ := databuddyServer(t, databuddySites, events.answer)
		if _, err := k.Sync(ctx, databuddySource(`{"website_ids":["web_a"],"history_days":1}`), Table{Name: databuddyOccurrences}, &calcomFullWriter{}); !errors.Is(err, errCalcomFull) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("an event id differs by every field it hashes, a null from an empty string", func(t *testing.T) {
		at := "2026-11-24T20:00:16.840Z"
		base := occurrence(time.Date(2026, 11, 24, 20, 0, 16, 840e6, time.UTC), "signup_completed", `{"plan":"pro"}`)
		id := databuddyEventID("web_a", at, base)
		if databuddyEventID("web_a", at, maps.Clone(base)) != id {
			t.Fatal("the same occurrence keyed twice differs")
		}
		if databuddyEventID("web_b", at, base) == id || databuddyEventID("web_a", "2026-11-24T20:00:16.841Z", base) == id {
			t.Error("another site or time shares the id")
		}
		for _, field := range []string{"event_name", "namespace", "path", "source", "anonymous_id", "session_id", "properties"} {
			for _, v := range []any{"other", "", nil} {
				changed := maps.Clone(base)
				changed[field] = v
				if base[field] != v && databuddyEventID("web_a", at, changed) == id {
					t.Errorf("%s = %#v shares the id", field, v)
				}
			}
		}
	})

	t.Run("reads Databuddy's days", func(t *testing.T) {
		for v, want := range map[any]string{"2026-11-15": "2026-11-15", "2026-11-15T05:00:00Z": "2026-11-15", "2026-11-15 23:59:59": "2026-11-15"} {
			if got, ok := databuddyDay(v); !ok || got.Format(time.DateOnly) != want {
				t.Errorf("databuddyDay(%v) = %v, %v", v, got, ok)
			}
		}
		for _, v := range []any{"", 3.0, nil, "15/11/2026"} {
			if _, ok := databuddyDay(v); ok {
				t.Errorf("databuddyDay(%v) read a day", v)
			}
		}
	})

	t.Run("a change read upserts occurrences and counts from the day before the cursor", func(t *testing.T) {
		events := databuddyEvents{
			counts: map[string]float64{day(1): 2, day(0): 1},
			byDay: map[string][]map[string]any{
				day(1): {occurrence(today.Add(-time.Hour), "signup_completed", "{}"), occurrence(today.Add(-2*time.Hour), "signup_completed", "{}")},
				day(0): {occurrence(today.Add(time.Minute), "signup_completed", "{}")},
			},
		}
		k, queries := databuddyServer(t, databuddySites, events.answer)
		times := map[string]time.Time{"custom_events_daily": today.Add(time.Hour), databuddyOccurrences: today.Add(time.Hour)}
		cursor, _ := json.Marshal(databuddyCursor{Tables: times, HistoryDays: 90, Sites: []string{"web_a"}})
		w := &cursorChanges{}
		tables := []Table{{Name: "custom_events_daily"}, {Name: databuddyOccurrences}}
		if _, err := k.(ChangeReader).Changes(ctx, databuddySource(`{"website_ids":["web_a"]}`), tables, cursor, w); err != nil {
			t.Fatal(err)
		}
		if len(w.upserts[databuddyOccurrences]) != 3 || len(w.upserts["custom_events_daily"]) != 2 || len(w.resets) != 0 {
			t.Fatalf("upserts = %v, resets = %v", w.upserts, w.resets)
		}
		var asked []string
		for _, q := range *queries {
			asked = append(asked, q.body.Parameters[0]+":"+q.body.StartDate+":"+q.body.EndDate)
		}
		// No count is read to size a change read: it keeps the newest days whatever their size.
		want := []string{"custom_events_trends_by_event:" + day(1) + ":" + day(0),
			"custom_events_recent:" + day(1) + ":" + day(1), "custom_events_recent:" + day(0) + ":" + day(0)}
		if !slices.Equal(asked, want) {
			t.Fatalf("asked %v, want %v", asked, want)
		}
	})
}
