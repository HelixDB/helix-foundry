package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		want := []string{"websites", "daily_metrics", "pages", "referrers", "custom_events", "sessions", "profiles"}
		if !slices.Equal(tableNames(d), want) || len(d.Unavailable) != 0 {
			t.Fatalf("tables = %v, unavailable = %v", tableNames(d), d.Unavailable)
		}
		keys := map[string][]string{"websites": {"website_id"}, "daily_metrics": {"website_id", "date"}, "pages": {"website_id", "path"},
			"referrers": {"website_id", "referrer"}, "custom_events": {"website_id", "event_name"}, "sessions": {"website_id", "session_id"}, "profiles": {"website_id", "visitor_id"}}
		for name, key := range keys {
			table := findTable(t, d, name)
			if !slices.Equal(table.PrimaryKey, key) {
				t.Errorf("%s key = %v, want %v", name, table.PrimaryKey, key)
			}
			if windowed := name == "daily_metrics" || name == "sessions" || name == "profiles"; table.Windowed != windowed {
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
			if q.site != site || len(q.body.Parameters) != 6 || q.body.Limit != 1 || q.body.StartDate != today || q.body.EndDate != today {
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
		if !slices.Equal(tableNames(d), []string{"websites"}) || len(d.Unavailable) != 6 || d.Unavailable[0].Reason != "the key reads no website yet" {
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
		for _, table := range []string{"daily_metrics", "sessions", "profiles"} {
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
