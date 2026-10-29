package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const notionTestSecret = "ntn_0123456789abcdefABCDEF"

// notionTestNow is the time reads version their documents at.
var notionTestNow = time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)

type notionRoutes = map[string]func(*http.Request) (int, string)

// notionAt serves routes, checking every request carries the secret, the
// API version and, on a POST, its JSON type, and returns the notion kind
// reading from them, unpaced.
func notionAt(t *testing.T, routes notionRoutes) *notionKind {
	t.Helper()
	savedInterval, savedNow := notionInterval, notionNow
	notionInterval, notionNow = 0, func() time.Time { return notionTestNow }
	t.Cleanup(func() { notionInterval, notionNow = savedInterval, savedNow })
	checked := notionRoutes{}
	for path, handler := range routes {
		checked[path] = func(r *http.Request) (int, string) {
			if r.Header.Get("Authorization") != "Bearer "+notionTestSecret || r.Header.Get("Notion-Version") != "2026-03-11" {
				t.Errorf("%s %s: Authorization = %q, Notion-Version = %q", r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Notion-Version"))
			}
			if isJSON := r.Header.Get("Content-Type") == "application/json"; isJSON != (r.Method == http.MethodPost) {
				t.Errorf("%s %s: Content-Type = %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
			}
			return handler(r)
		}
	}
	return notionKindAt(t, routeServer(t, checked))
}

func notionKindAt(t *testing.T, url string) *notionKind {
	t.Helper()
	return kindOf(t, NewCatalog(Endpoints{Notion: url}, Guard{AllowPrivate: true}, S3Options{}, EventsOptions{}), "notion").(*notionKind)
}

// notionBody is a POST's JSON body.
func notionBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("%s body: %v", r.URL.Path, err)
	}
	return body
}

// notionResults is a last page of a list of items.
func notionResults(items ...string) string {
	return `{"object":"list","results":[` + strings.Join(items, ",") +
		`],"next_cursor":null,"has_more":false,"type":"page_or_data_source","page_or_data_source":{}}`
}

// notionMore is a page of items after which more follow, from cursor.
func notionMore(cursor string, items ...string) string {
	return strings.Replace(notionResults(items...), `"next_cursor":null,"has_more":false`, `"next_cursor":"`+cursor+`","has_more":true`, 1)
}

// notionCut is a last page of items that Notion cut at its result limit.
func notionCut(items ...string) string {
	return strings.Replace(notionResults(items...), `"has_more":false`, `"has_more":false,"request_status":{"type":"incomplete","incomplete_reason":"query_result_limit_reached"}`, 1)
}

const (
	notionWorkspace = `{"type":"workspace","workspace":true}`
	notionBotJSON   = `{"object":"user","id":"usr_bot","type":"bot","name":"Helix","bot":{"workspace_name":"Acme"}}`
	notionAdaJSON   = `{"object":"user","id":"usr_ada","type":"person","name":"Ada Lovelace","avatar_url":null,"person":{"email":"ada@example.com"}}`
	notionGraceJSON = `{"object":"user","id":"usr_grace","type":"person","name":"Grace Hopper","person":{}}`
)

func notionUnder(kind, id string) string {
	if kind == "data_source_id" {
		return fmt.Sprintf(`{"type":"data_source_id","data_source_id":%q,"database_id":"db_1"}`, id)
	}
	return fmt.Sprintf(`{"type":%q,%q:%q}`, kind, kind, id)
}

// notionTestPage is a page as a search lists it, edited at edited.
func notionTestPage(id, title, edited, parent string) string {
	return fmt.Sprintf(`{"object":"page","id":%q,"created_time":"2026-01-05T09:00:00.000Z","last_edited_time":%q,
		"created_by":{"object":"user","id":"usr_ada"},"last_edited_by":{"object":"user","id":"usr_bot"},"in_trash":false,
		"parent":%s,"url":"https://www.notion.so/%s","public_url":null,
		"properties":{"title":{"id":"title","type":"title","title":[{"type":"text","text":{"content":%q},"plain_text":%q}]}}}`,
		id, edited, parent, id, title, title)
}

// notionTestSource is a data source as a search lists it.
func notionTestSource(id, title, created, properties string) string {
	return fmt.Sprintf(`{"object":"data_source","id":%q,"created_time":%q,"last_edited_time":"2026-01-10T00:00:00.000Z",
		"title":[{"type":"text","text":{"content":%q},"plain_text":%q}],"description":[],"properties":%s,
		"parent":{"type":"database_id","database_id":"db_1"},"in_trash":false}`, id, created, title, title, properties)
}

// notionTestRow is a row of data source ds_1 created at created.
func notionTestRow(id, created, properties string) string {
	return fmt.Sprintf(`{"object":"page","id":%q,"created_time":%q,"last_edited_time":"2026-01-06T10:00:00.000Z",
		"created_by":{"object":"user","id":"usr_ada"},"last_edited_by":{"object":"user","id":"usr_ada"},
		"parent":{"type":"data_source_id","data_source_id":"ds_1","database_id":"db_1"},"url":"https://www.notion.so/%s","properties":%s}`,
		id, created, id, properties)
}

const (
	notionAccountsSchema = `{"Name":{"id":"title","name":"Name","type":"title","title":{}},
		"Plan":{"id":"pl","name":"Plan","type":"select","select":{"options":[]}},"Renewal":{"id":"rn","name":"Renewal","type":"date","date":{}}}`
	notionAccountValues = `{"Name":{"id":"title","type":"title","title":[{"plain_text":"Acme"}]},
		"Plan":{"id":"pl","type":"select","select":{"name":"Gold"}},"Renewal":{"id":"rn","type":"date","date":{"start":"2026-03-01","end":null}}}`
)

// notionSearchOf answers a search for pages from pages and for data
// sources from sources, one result for a probe.
func notionSearchOf(t *testing.T, pages, sources []string) func(*http.Request) (int, string) {
	return func(r *http.Request) (int, string) {
		body := notionBody(t, r)
		filter, _ := body["filter"].(map[string]any)
		items := pages
		if filter["value"] == "data_source" {
			items = sources
		}
		if body["page_size"] == float64(1) {
			items = items[:min(1, len(items))]
		}
		return http.StatusOK, notionResults(items...)
	}
}

// notionShared is a workspace that shares pages and sources with the
// integration, and has two people and the bot; extra adds routes or
// replaces them.
func notionShared(t *testing.T, pages, sources []string, extra notionRoutes) notionRoutes {
	routes := notionRoutes{
		"/v1/users/me": func(*http.Request) (int, string) { return http.StatusOK, notionBotJSON },
		"/v1/users": func(*http.Request) (int, string) {
			return http.StatusOK, notionResults(notionAdaJSON, notionGraceJSON, notionBotJSON)
		},
		"/v1/search": notionSearchOf(t, pages, sources),
	}
	maps.Copy(routes, extra)
	return routes
}

func notionRefusal(t *testing.T, err error, message string) {
	t.Helper()
	var refusedErr *RefusedError
	if !errors.As(err, &refusedErr) || refusedErr.Message != message {
		t.Fatalf("error = %v, want the refusal %q", err, message)
	}
}

func notionInvalid(t *testing.T, err error, message string) {
	t.Helper()
	var invalidErr *InvalidError
	if !errors.As(err, &invalidErr) || invalidErr.Message != message {
		t.Fatalf("error = %v, want InvalidError %q", err, message)
	}
}

func TestNotionConnector(t *testing.T) {
	ctx := context.Background()
	for _, secret := range []string{"", "sk_live_abc", "ntn_", "secret_", "ntn_abc-def", "ntn_abc_def", "secret_abc def", "Bearer ntn_abc", "NTN_abc"} {
		t.Run(fmt.Sprintf("rejects %q as a secret", secret), func(t *testing.T) {
			_, err := notionKindAt(t, "http://unused").Prepare(Source{Secret: secret})
			notionInvalid(t, err, "enter a Notion integration secret (ntn_…)")
		})
	}
	t.Run("takes an ntn_ or a secret_ secret and no settings", func(t *testing.T) {
		kind := notionKindAt(t, "http://unused")
		for _, secret := range []string{" " + notionTestSecret + "\n", "secret_0123456789abcdefABCDEF"} {
			for _, raw := range []string{"", "null", "{}"} {
				settings, err := kind.Prepare(Source{Secret: secret, Settings: json.RawMessage(raw)})
				if err != nil || string(settings) != `{}` {
					t.Errorf("Prepare(%q, %s) = (%s, %v)", secret, raw, settings, err)
				}
			}
		}
		for _, raw := range []string{`{"token":"ntn_abc"}`, `[]`} {
			_, err := kind.Prepare(Source{Secret: notionTestSecret, Settings: json.RawMessage(raw)})
			notionInvalid(t, err, "this connector takes no settings besides its key")
		}
	})
	t.Run("hints at a secret by its prefix and last four characters", func(t *testing.T) {
		kind := notionKindAt(t, "http://unused")
		if hint := kind.Hint(" " + notionTestSecret); hint != "ntn…CDEF" || kind.DisplayName() != "Notion" || kind.Name() != "notion" {
			t.Fatalf("Hint = %q, DisplayName = %q", hint, kind.DisplayName())
		}
		if hint := kind.Hint("secret_0123456789abcdef4f2a"); hint != "secret…4f2a" {
			t.Fatalf("Hint = %q", hint)
		}
	})
	t.Run("keeps every page's Markdown on the pages table", func(t *testing.T) {
		streams := notionKindAt(t, "http://unused").Streams(Source{})
		if len(streams) != 1 || streams[0].Name != "pages" || streams[0].Table != "pages" || streams[0].Title != "Pages" ||
			streams[0].Format != Markdown || streams[0].Keep.Newest() != 0 || streams[0].Problem() != "" {
			t.Fatalf("streams = %+v", streams)
		}
	})

	pages := []string{notionTestPage("pg_1", "Roadmap", "2026-01-06T10:00:00.000Z", notionWorkspace)}
	accounts := notionTestSource("ds_1", "Customer Accounts", "2026-01-01T00:00:00.000Z", notionAccountsSchema)
	accountRows := func(n int) func(r *http.Request) (int, string) {
		return func(r *http.Request) (int, string) {
			if body := notionBody(t, r); body["page_size"] != float64(notionPreviewRows) || body["result_type"] != "page" {
				t.Errorf("preview query = %v", body)
			}
			var rows []string
			for i := range n {
				rows = append(rows, notionTestRow(fmt.Sprintf("row_%d", i+1), "2026-01-05T09:00:00.000Z", notionAccountValues))
			}
			return http.StatusOK, notionResults(rows...)
		}
	}
	t.Run("lists users, pages and each shared data source, with columns and a preview", func(t *testing.T) {
		var probes []string
		var mu sync.Mutex
		routes := notionShared(t, pages, []string{accounts}, notionRoutes{"/v1/data_sources/ds_1/query": accountRows(2)})
		listUsers := routes["/v1/users"]
		routes["/v1/users"] = func(r *http.Request) (int, string) {
			mu.Lock()
			probes = append(probes, "users "+r.URL.RawQuery)
			mu.Unlock()
			return listUsers(r)
		}
		d, err := notionAt(t, routes).Discover(ctx, Source{Secret: notionTestSecret})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(tableNames(d), []string{"users", "pages", "customer_accounts"}) || len(d.Unavailable) != 0 || !slices.Equal(probes, []string{"users page_size=1"}) {
			t.Fatalf("tables = %v, unavailable = %v, probes = %v", tableNames(d), d.Unavailable, probes)
		}
		users := findTable(t, d, "users")
		if !slices.Equal(users.PrimaryKey, []string{"user_id"}) || !users.Dense || len(users.ForeignKeys) != 0 ||
			fmt.Sprint(users.Columns) != "[{user_id string false} {type string false} {name string true} {email string true}]" {
			t.Fatalf("users = %+v", users)
		}
		pagesTable := findTable(t, d, "pages")
		want := []ForeignKey{{Column: "created_by_id", ReferencedTable: "users", ReferencedColumn: "user_id"},
			{Column: "last_edited_by_id", ReferencedTable: "users", ReferencedColumn: "user_id"}}
		if !slices.Equal(pagesTable.ForeignKeys, want) || !slices.Equal(pagesTable.PrimaryKey, []string{"page_id"}) || !pagesTable.Dense ||
			len(pagesTable.Columns) != 10 || len(pagesTable.Preview) != 0 {
			t.Fatalf("pages = %+v", pagesTable)
		}
		table := findTable(t, d, "customer_accounts")
		columns := "[{page_id string false} {url string true} {created_at string false} {last_edited_at string false} " +
			"{name string true} {plan string true} {renewal string true} {renewal_end string true}]"
		if fmt.Sprint(table.Columns) != columns || !slices.Equal(table.PrimaryKey, []string{"page_id"}) || !table.Dense || len(table.ForeignKeys) != 0 {
			t.Fatalf("customer_accounts = %+v", table)
		}
		preview := `{"created_at":"2026-01-05T09:00:00Z","last_edited_at":"2026-01-06T10:00:00Z","name":"Acme","page_id":"row_1",` +
			`"plan":"Gold","renewal":"2026-03-01","renewal_end":null,"url":"https://www.notion.so/row_1"}`
		if len(table.Preview) != 2 || string(table.Preview[0]) != preview {
			t.Fatalf("preview = %s", table.Preview)
		}
	})
	t.Run("shows at most five rows of a data source", func(t *testing.T) {
		d, err := notionAt(t, notionShared(t, pages, []string{accounts}, notionRoutes{"/v1/data_sources/ds_1/query": accountRows(7)})).
			Discover(ctx, Source{Secret: notionTestSecret})
		if err != nil || len(findTable(t, d, "customer_accounts").Preview) != 5 {
			t.Fatalf("Discover = %v", err)
		}
	})
	// A sync reads every row anyway, so it asks for no preview.
	t.Run("reads no rows for a sync", func(t *testing.T) {
		d, err := notionAt(t, notionShared(t, pages, []string{accounts}, nil)).Discover(ctx, Source{Secret: notionTestSecret, ForSync: true})
		if err != nil || !slices.Equal(tableNames(d), []string{"users", "pages", "customer_accounts"}) || d.Tables[2].Preview != nil {
			t.Fatalf("Discover = (%v, %v)", tableNames(d), err)
		}
	})
	t.Run("reports users unavailable to an integration that may not read them", func(t *testing.T) {
		d, err := notionAt(t, notionShared(t, pages, nil, notionRoutes{
			"/v1/users": func(*http.Request) (int, string) {
				return http.StatusForbidden, `{"object":"error","status":403,"code":"restricted_resource","message":"Insufficient permissions"}`
			},
		})).Discover(ctx, Source{Secret: notionTestSecret})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(tableNames(d), []string{"pages"}) || len(d.Tables[0].ForeignKeys) != 0 ||
			!slices.Equal(d.Unavailable, []Unavailable{{Name: "users", Reason: "turn on Read user information in the integration's capabilities"}}) {
			t.Fatalf("tables = %v, unavailable = %v", d.Tables, d.Unavailable)
		}
	})
	// Only the probes search when no data source is shared.
	t.Run("lists no data source when none is shared", func(t *testing.T) {
		var mu sync.Mutex
		searches := 0
		search := notionSearchOf(t, pages, nil)
		d, err := notionAt(t, notionShared(t, nil, nil, notionRoutes{"/v1/search": func(r *http.Request) (int, string) {
			mu.Lock()
			searches++
			mu.Unlock()
			return search(r)
		}})).Discover(ctx, Source{Secret: notionTestSecret})
		if err != nil || !slices.Equal(tableNames(d), []string{"users", "pages"}) || searches != 2 {
			t.Fatalf("Discover = (%v, %v) after %d searches", tableNames(d), err, searches)
		}
	})
	t.Run("lists a workspace that shares only databases", func(t *testing.T) {
		d, err := notionAt(t, notionShared(t, nil, []string{accounts}, notionRoutes{"/v1/data_sources/ds_1/query": accountRows(0)})).
			Discover(ctx, Source{Secret: notionTestSecret})
		if err != nil || !slices.Equal(tableNames(d), []string{"users", "pages", "customer_accounts"}) || len(d.Tables[2].Preview) != 0 {
			t.Fatalf("Discover = (%v, %v)", tableNames(d), err)
		}
	})
	t.Run("refuses an integration nothing is shared with", func(t *testing.T) {
		_, err := notionAt(t, notionShared(t, nil, nil, nil)).Discover(ctx, Source{Secret: notionTestSecret})
		notionRefusal(t, err, "nothing is shared with this integration yet: in Notion, add it to a page or database "+
			"from its ••• menu › Connections, then test again")
	})
	unauthorized := func(*http.Request) (int, string) {
		return http.StatusUnauthorized, `{"object":"error","status":401,"code":"unauthorized","message":"API token is invalid."}`
	}
	t.Run("reports a rejected secret", func(t *testing.T) {
		_, err := notionAt(t, notionRoutes{"/v1/users/me": unauthorized}).Discover(ctx, Source{Secret: notionTestSecret})
		notionRefusal(t, err, "Notion rejected this secret; check it and enter it again")
		_, err = notionAt(t, notionShared(t, pages, nil, notionRoutes{"/v1/users": unauthorized})).Discover(ctx, Source{Secret: notionTestSecret})
		notionRefusal(t, err, "Notion rejected this secret; check it and enter it again")
	})
	// A rate limit or an outage says nothing about what the integration may
	// read, so it fails the test rather than making a table unavailable.
	for name, tc := range map[string]struct {
		routes  notionRoutes
		message string
	}{
		"a users probe Notion lost":   {notionRoutes{"/v1/users": func(*http.Request) (int, string) { return http.StatusNotFound, `{}` }}, "this integration cannot read /v1/users; check its capabilities in Notion and test again"},
		"a search Notion is down for": {notionRoutes{"/v1/search": func(*http.Request) (int, string) { return http.StatusServiceUnavailable, `{}` }}, "could not reach Notion; try again"},
		"a search Notion refuses":     {notionRoutes{"/v1/search": func(*http.Request) (int, string) { return http.StatusBadRequest, `{"code":"validation_error"}` }}, "Notion returned HTTP 400 for /v1/search"},
		"a rate limit on the users":   {notionRoutes{"/v1/users": func(*http.Request) (int, string) { return http.StatusTooManyRequests, `{"code":"rate_limited"}` }}, "this integration has used up its Notion rate limit; try again in a few minutes"},
		"a preview it may not read":   {notionRoutes{"/v1/data_sources/ds_1/query": func(*http.Request) (int, string) { return http.StatusNotFound, `{}` }}, "this integration cannot read /v1/data_sources/ds_1/query; check its capabilities in Notion and test again"},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			noBackoff(t)
			_, err := notionAt(t, notionShared(t, pages, []string{accounts}, tc.routes)).Discover(ctx, Source{Secret: notionTestSecret})
			notionRefusal(t, err, tc.message)
		})
	}
	t.Run("fails a preview row without its times", func(t *testing.T) {
		_, err := notionAt(t, notionShared(t, pages, []string{accounts}, notionRoutes{
			"/v1/data_sources/ds_1/query": func(*http.Request) (int, string) { return http.StatusOK, notionResults(`{"id":"row_1"}`) },
		})).Discover(ctx, Source{Secret: notionTestSecret})
		if err == nil || !strings.Contains(err.Error(), "page row_1 is listed without its created_time and last_edited_time") {
			t.Fatalf("Discover error = %v", err)
		}
	})
	t.Run("fails a data source list Notion cut short", func(t *testing.T) {
		_, err := notionAt(t, notionShared(t, pages, []string{accounts}, notionRoutes{
			"/v1/search": func(r *http.Request) (int, string) {
				if body := notionBody(t, r); body["page_size"] == float64(1) {
					return http.StatusOK, notionResults(accounts)
				}
				return http.StatusOK, notionCut(accounts)
			},
		})).Discover(ctx, Source{Secret: notionTestSecret})
		notionInvalid(t, err, "Notion stopped listing the data sources shared with this integration at its result limit; share fewer with it")
	})
	t.Run("refuses a test Notion could not be reached for", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		server.Close()
		kind := notionAt(t, nil)
		kind.base = server.URL
		_, err := kind.Discover(ctx, Source{Secret: notionTestSecret})
		notionRefusal(t, err, "could not reach Notion; try again")
	})
}

func TestNotionNaming(t *testing.T) {
	for title, want := range map[string]string{
		"Customer Accounts":            "customer_accounts",
		"  Q3 — Roadmap!!  ":           "q3_roadmap",
		"Café Menü":                    "cafe_menu",
		"Straße":                       "stra_e",
		"日本語":                          "",
		"📅 Due date":                   "due_date",
		"CustomerAccounts":             "customeraccounts",
		"2024 Plans":                   "2024_plans",
		"page_id":                      "page_id",
		strings.Repeat("a", 70):        strings.Repeat("a", 63),
		strings.Repeat("a", 62) + " b": strings.Repeat("a", 62),
		"":                             "",
	} {
		if got := notionSlug(title); got != want {
			t.Errorf("notionSlug(%q) = %q, want %q", title, got, want)
		}
	}
	// A suffix is of the id alone, longer only should another id's take it.
	taken := map[string]bool{"tasks__5837f7a6": true}
	if got := notionSuffixed(taken, "tasks", "ds_1"); got != "tasks__5837f7a66d" || !taken[got] {
		t.Errorf("notionSuffixed = %q", got)
	}
	if got := notionSuffixed(taken, "tasks", "ds_b"); got != "tasks__f248edc1" {
		t.Errorf("notionSuffixed = %q", got)
	}

	discover := func(t *testing.T, sources ...string) Discovery {
		t.Helper()
		d, err := notionAt(t, notionShared(t, nil, sources, nil)).Discover(context.Background(), Source{Secret: notionTestSecret, ForSync: true})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	// The oldest data source holds a name, ties by id; the others take it
	// with their id's suffix, and none takes users or pages.
	t.Run("names data sources uniquely, the oldest holding a name", func(t *testing.T) {
		d := discover(t,
			notionTestSource("ds_1", "Tasks", "2026-01-03T00:00:00.000Z", `{}`),
			notionTestSource("ds_2", "Tasks", "2026-01-01T00:00:00.000Z", `{}`),
			notionTestSource("ds_3", "tasks", "2026-01-03T00:00:00.000Z", `{}`),
			notionTestSource("ds_4", "Users", "2026-01-01T00:00:00.000Z", `{}`),
			notionTestSource("ds_5", "Pages", "2026-01-01T00:00:00.000Z", `{}`),
			notionTestSource("ds_6", "", "2026-01-01T00:00:00.000Z", `{}`),
			notionTestSource("ds_7", "🚀🚀", "2026-01-01T00:00:00.000Z", `{}`),
			notionTestSource("ds_8", "Tasks 2", "2026-01-02T00:00:00.000Z", `{}`),
			`{"object":"data_source","id":"ds_9","title":[{"plain_text":"Zeta"}],"properties":{}}`,
		)
		want := []string{"users", "pages", "zeta", "tasks", "users__e3972d4b", "pages__65bf4c9f", "data_source", "data_source__945a3f58",
			"tasks_2", "tasks__5837f7a6", "tasks__c6fb397c"}
		if got := tableNames(d); !slices.Equal(got, want) {
			t.Fatalf("tables = %v, want %v", got, want)
		}
	})
	// A newer data source of the same title, shared later, moves no name;
	// one unshared frees no name for another.
	t.Run("keeps every table's name as newer data sources of its title come and go", func(t *testing.T) {
		older := notionTestSource("ds_a", "Tasks", "2026-01-01T00:00:00.000Z", `{}`)
		b := notionTestSource("ds_b", "Tasks", "2026-02-01T00:00:00.000Z", `{}`)
		c := notionTestSource("ds_c", "Tasks", "2026-01-15T00:00:00.000Z", `{}`)
		for _, tc := range []struct {
			sources []string
			want    string
		}{
			{[]string{older}, "[users pages tasks]"},
			{[]string{older, b}, "[users pages tasks tasks__f248edc1]"},
			{[]string{older, b, c}, "[users pages tasks tasks__ceef22ea tasks__f248edc1]"},
			{[]string{b, c}, "[users pages tasks tasks__f248edc1]"},
		} {
			if got := fmt.Sprint(tableNames(discover(t, tc.sources...))); got != tc.want {
				t.Errorf("tables = %s, want %s", got, tc.want)
			}
		}
	})
	// Columns go in property name order. A name two properties would take,
	// or a fixed column's, each takes with its property id's suffix, and a
	// date's end column is its name and _end.
	t.Run("names a data source's columns uniquely by property", func(t *testing.T) {
		schema := `{"Page ID":{"id":"a","type":"rich_text"},"URL":{"id":"b","type":"url"},"Due":{"id":"c","type":"date"},
			"Due End":{"id":"d","type":"rich_text"},"Ünïcode Näme":{"id":"e","type":"people"},"🔥":{"id":"f","type":"checkbox"},
			"✨":{"id":"g","type":"number"},"Button":{"id":"h","type":"button"},"Verify":{"id":"i","type":"verification"},
			"Place":{"id":"j","type":"place"},"Weird":{"id":"k","type":"something_new"},"Odd":"not a property"}`
		d := discover(t, notionTestSource("ds_1", "Projects", "2026-01-01T00:00:00.000Z", schema))
		want := "[{page_id string false} {url string true} {created_at string false} {last_edited_at string false} " +
			"{due string true} {due_end__2e7d2c03 string true} {due_end__18ac3e73 string true} {page_id__ca978112 string true} " +
			"{url__3e23e816 string true} {unicode_name array true} {property__cd0aa985 number true} {property__252f10c8 boolean true}]"
		if got := fmt.Sprint(findTable(t, d, "projects").Columns); got != want {
			t.Fatalf("columns = %s\nwant %s", got, want)
		}
	})
	// Adding "Due Date" beside "Due date" leaves due_date to neither.
	t.Run("gives a column no other property's name as properties come", func(t *testing.T) {
		one := `{"Due date":{"id":"c","type":"date"}}`
		two := `{"Due date":{"id":"c","type":"date"},"Due Date":{"id":"d","type":"rich_text"}}`
		for schema, want := range map[string]string{
			one: "[due_date due_date_end]",
			two: "[due_date__18ac3e73 due_date__2e7d2c03 due_date_end]",
		} {
			var names []string
			for _, c := range findTable(t, discover(t, notionTestSource("ds_1", "Projects", "2026-01-01T00:00:00.000Z", schema)), "projects").Columns[4:] {
				names = append(names, c.Name)
			}
			if got := fmt.Sprint(names); got != want {
				t.Errorf("columns = %s, want %s", got, want)
			}
		}
	})
}

// notionEverySchema has a property of every type Notion has.
const notionEverySchema = `{
	"Name":{"id":"title","name":"Name","type":"title","title":{}},
	"Notes":{"id":"nt","type":"rich_text","rich_text":{}},
	"Amount":{"id":"am","type":"number","number":{"format":"dollar"}},
	"Active":{"id":"ac","type":"checkbox","checkbox":{}},
	"Tier":{"id":"ti","type":"select","select":{"options":[]}},
	"Stage":{"id":"st","type":"status","status":{}},
	"Tags":{"id":"tg","type":"multi_select","multi_select":{}},
	"Renewal":{"id":"rn","type":"date","date":{}},
	"Owners":{"id":"ow","type":"people","people":{}},
	"Projects":{"id":"pr","type":"relation","relation":{}},
	"Site":{"id":"si","type":"url","url":{}},
	"Email":{"id":"em","type":"email","email":{}},
	"Phone":{"id":"ph","type":"phone_number","phone_number":{}},
	"Score":{"id":"sc","type":"formula","formula":{"expression":"1"}},
	"Label":{"id":"lb","type":"formula","formula":{}},
	"Flag":{"id":"fl","type":"formula","formula":{}},
	"When":{"id":"wh","type":"formula","formula":{}},
	"Broken":{"id":"br","type":"formula","formula":{}},
	"Total":{"id":"to","type":"rollup","rollup":{"function":"sum"}},
	"First":{"id":"fi","type":"rollup","rollup":{"function":"earliest_date"}},
	"Names":{"id":"na","type":"rollup","rollup":{"function":"show_original"}},
	"Pending":{"id":"pe","type":"rollup","rollup":{}},
	"Creator":{"id":"cb","type":"created_by","created_by":{}},
	"Editor":{"id":"eb","type":"last_edited_by","last_edited_by":{}},
	"Created":{"id":"ct","type":"created_time","created_time":{}},
	"Edited":{"id":"et","type":"last_edited_time","last_edited_time":{}},
	"Ticket":{"id":"tk","type":"unique_id","unique_id":{"prefix":"TASK"}},
	"Seq":{"id":"sq","type":"unique_id","unique_id":{"prefix":null}},
	"Docs":{"id":"dc","type":"files","files":{}},
	"Send":{"id":"bt","type":"button","button":{}},
	"Check":{"id":"vf","type":"verification","verification":{}},
	"Where":{"id":"pl","type":"place","place":{}}}`

func TestNotionValues(t *testing.T) {
	full := `{
		"Name":{"id":"title","type":"title","title":[{"plain_text":"Acme "},{"plain_text":"Corp"}]},
		"Notes":{"id":"nt","type":"rich_text","rich_text":[{"plain_text":"Renews in March"}]},
		"Amount":{"id":"am","type":"number","number":1200.5},
		"Active":{"id":"ac","type":"checkbox","checkbox":true},
		"Tier":{"id":"ti","type":"select","select":{"id":"x","name":"Gold","color":"yellow"}},
		"Stage":{"id":"st","type":"status","status":{"name":"In progress"}},
		"Tags":{"id":"tg","type":"multi_select","multi_select":[{"name":"vip"},{"name":"emea"}]},
		"Renewal":{"id":"rn","type":"date","date":{"start":"2026-03-01","end":"2026-03-31","time_zone":null}},
		"Owners":{"id":"ow","type":"people","people":[{"object":"user","id":"usr_ada"},{"object":"user","id":"usr_grace"}]},
		"Projects":{"id":"pr","type":"relation","relation":[{"id":"pg_1"}],"has_more":false},
		"Site":{"id":"si","type":"url","url":"https://acme.example"},
		"Email":{"id":"em","type":"email","email":"ops@acme.example"},
		"Phone":{"id":"ph","type":"phone_number","phone_number":"+1 415 555 0123"},
		"Score":{"id":"sc","type":"formula","formula":{"type":"number","number":16}},
		"Label":{"id":"lb","type":"formula","formula":{"type":"string","string":"Ready"}},
		"Flag":{"id":"fl","type":"formula","formula":{"type":"boolean","boolean":false}},
		"When":{"id":"wh","type":"formula","formula":{"type":"date","date":{"start":"2026-09-15T09:00:00.000+02:00","end":null}}},
		"Broken":{"id":"br","type":"formula","formula":{"type":"unsupported","unsupported":{}}},
		"Total":{"id":"to","type":"rollup","rollup":{"type":"number","number":8,"function":"sum"}},
		"First":{"id":"fi","type":"rollup","rollup":{"type":"date","date":{"start":"2026-01-02","end":null},"function":"earliest_date"}},
		"Names":{"id":"na","type":"rollup","rollup":{"type":"array","function":"show_original","array":[
			{"type":"title","title":[{"plain_text":"Alpha"}]},{"type":"multi_select","multi_select":[{"name":"x"},{"name":"y"}]},
			{"type":"number","number":null},{"type":"rich_text","rich_text":[]},"not a value"]}},
		"Pending":{"id":"pe","type":"rollup","rollup":{"type":"incomplete","incomplete":{}}},
		"Creator":{"id":"cb","type":"created_by","created_by":{"object":"user","id":"usr_ada"}},
		"Editor":{"id":"eb","type":"last_edited_by","last_edited_by":{"object":"user","id":"usr_bot"}},
		"Created":{"id":"ct","type":"created_time","created_time":"2026-01-05T09:00:00.000Z"},
		"Edited":{"id":"et","type":"last_edited_time","last_edited_time":"2026-01-06T10:30:00.000+01:00"},
		"Ticket":{"id":"tk","type":"unique_id","unique_id":{"prefix":"TASK","number":42}},
		"Seq":{"id":"sq","type":"unique_id","unique_id":{"prefix":null,"number":7}},
		"Docs":{"id":"dc","type":"files","files":[{"name":"Brief.pdf","type":"external","external":{"url":"https://x"}},{"name":"","type":"file"}]},
		"Send":{"id":"bt","type":"button","button":{}},
		"Check":{"id":"vf","type":"verification","verification":{"state":"verified"}},
		"Where":{"id":"pl","type":"place","place":{"lat":1,"lon":2}}}`
	empty := `{
		"Name":{"id":"title","type":"title","title":[]},
		"Amount":{"id":"am","type":"number","number":null},
		"Active":{"id":"ac","type":"checkbox","checkbox":false},
		"Tier":{"id":"ti","type":"select","select":null},
		"Tags":{"id":"tg","type":"multi_select","multi_select":[]},
		"Renewal":{"id":"rn","type":"date","date":null},
		"Site":{"id":"si","type":"url","url":null},
		"Label":{"id":"lb","type":"formula","formula":{"type":"string","string":""}},
		"Names":{"id":"na","type":"rollup","rollup":{"type":"array","array":[],"function":"show_original"}},
		"Created":{"id":"ct","type":"created_time","created_time":"soon"},
		"Ticket":{"id":"tk","type":"unique_id","unique_id":{"prefix":"TASK","number":null}},
		"Docs":{"id":"dc","type":"files","files":[]},
		"Unknown":{"id":"zz","type":"rich_text","rich_text":[{"plain_text":"not in the schema"}]},
		"Odd":"not a value"}`
	source := notionTestSource("ds_1", "Accounts", "2026-01-01T00:00:00.000Z", notionEverySchema)
	kind := notionAt(t, notionShared(t, nil, []string{source}, notionRoutes{
		"/v1/data_sources/ds_1/query": func(*http.Request) (int, string) {
			return http.StatusOK, notionResults(notionTestRow("row_1", "2026-01-05T09:00:00.000Z", full),
				notionTestRow("row_2", "2026-01-05T09:01:00.000Z", empty), notionTestRow("row_3", "2026-01-05T09:02:00.000Z", `null`))
		},
	}))
	sink := &rowSink{}
	written, err := kind.Sync(context.Background(), Source{Secret: notionTestSecret}, Table{Name: "accounts"}, sink)
	if err != nil {
		t.Fatal(err)
	}
	columns := "[{page_id string false} {url string true} {created_at string false} {last_edited_at string false} " +
		"{active boolean true} {amount number true} {broken  true} {created string true} {creator string true} {docs array true} " +
		"{edited string true} {editor string true} {email string true} {first  true} {flag  true} {label  true} {name string true} " +
		"{names  true} {notes string true} {owners array true} {pending  true} {phone string true} {projects array true} " +
		"{renewal string true} {renewal_end string true} {score  true} {seq  true} {site string true} {stage string true} " +
		"{tags array true} {ticket  true} {tier string true} {total  true} {when  true}]"
	if fmt.Sprint(written.Columns) != columns || !written.Dense || !slices.Equal(written.PrimaryKey, []string{"page_id"}) {
		t.Fatalf("table = %+v\nwant columns %s", written, columns)
	}
	if len(sink.rows) != 3 {
		t.Fatalf("rows = %v", sink.rows)
	}
	want := map[string]any{
		"page_id": "row_1", "url": "https://www.notion.so/row_1", "created_at": "2026-01-05T09:00:00Z", "last_edited_at": "2026-01-06T10:00:00Z",
		"name": "Acme Corp", "notes": "Renews in March", "amount": 1200.5, "active": true, "tier": "Gold", "stage": "In progress",
		"tags": []any{"vip", "emea"}, "renewal": "2026-03-01", "renewal_end": "2026-03-31", "owners": []any{"usr_ada", "usr_grace"},
		"projects": []any{"pg_1"}, "site": "https://acme.example", "email": "ops@acme.example", "phone": "+1 415 555 0123",
		"score": 16, "label": "Ready", "flag": false, "when": "2026-09-15T09:00:00.000+02:00", "broken": nil, "total": 8,
		"first": "2026-01-02", "names": []any{"Alpha", "x", "y"}, "pending": nil, "creator": "usr_ada", "editor": "usr_bot",
		"created": "2026-01-05T09:00:00Z", "edited": "2026-01-06T09:30:00Z", "ticket": "TASK-42", "seq": 7, "docs": []any{"Brief.pdf"},
	}
	if fmt.Sprint(sink.rows[0]) != fmt.Sprint(want) {
		t.Errorf("full row = %v\nwant %v", sink.rows[0], want)
	}
	// Every column is on every row, null where the page has no value.
	for _, row := range sink.rows[1:] {
		if len(row) != 34 {
			t.Errorf("row %v has %d columns", row["page_id"], len(row))
		}
		for column, value := range row {
			switch column {
			case "page_id", "url", "created_at", "last_edited_at":
			case "active":
				if row["page_id"] == "row_2" && value != false {
					t.Errorf("active = %v", value)
				}
			default:
				if value != nil {
					t.Errorf("row %v: %s = %v, want null", row["page_id"], column, value)
				}
			}
		}
	}
}

// notionWaits makes do's retries wait no time, and records the Retry-After
// each was asked to wait.
func notionWaits(t *testing.T) *[]string {
	t.Helper()
	saved := notionWait
	var mu sync.Mutex
	var asked []string
	notionWait = func(_ int, retryAfter string) time.Duration {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, retryAfter)
		return 0
	}
	t.Cleanup(func() { notionWait = saved })
	return &asked
}

// notionUsers serves the users list in two pages.
func notionUsers(t *testing.T) func(*http.Request) (int, string) {
	return func(r *http.Request) (int, string) {
		q := r.URL.Query()
		if q.Get("page_size") != "100" {
			t.Errorf("users query = %s", r.URL.RawQuery)
		}
		if q.Get("start_cursor") == "" {
			return http.StatusOK, notionMore("u1", notionAdaJSON, notionGraceJSON)
		}
		return http.StatusOK, notionResults(notionBotJSON)
	}
}

func TestNotionSync(t *testing.T) {
	ctx := context.Background()
	src := Source{Secret: notionTestSecret}
	read := func(t *testing.T, kind *notionKind, table string) ([]map[string]any, error) {
		t.Helper()
		sink := &rowSink{}
		_, err := kind.Sync(ctx, src, Table{Name: table}, sink)
		return sink.rows, err
	}

	t.Run("pages users and writes each with its email when Notion shares it", func(t *testing.T) {
		rows, err := read(t, notionAt(t, notionRoutes{"/v1/users": notionUsers(t)}), "users")
		if err != nil {
			t.Fatal(err)
		}
		want := []map[string]any{
			{"user_id": "usr_ada", "type": "person", "name": "Ada Lovelace", "email": "ada@example.com"},
			{"user_id": "usr_grace", "type": "person", "name": "Grace Hopper", "email": nil},
			{"user_id": "usr_bot", "type": "bot", "name": "Helix", "email": nil},
		}
		if fmt.Sprint(rows) != fmt.Sprint(want) {
			t.Fatalf("users = %v", rows)
		}
	})
	t.Run("pages the search and writes each page's place", func(t *testing.T) {
		var cursors []string
		kind := notionAt(t, notionRoutes{"/v1/search": func(r *http.Request) (int, string) {
			body := notionBody(t, r)
			want := map[string]any{"filter": map[string]any{"property": "object", "value": "page"}, "page_size": 100.0,
				"sort": map[string]any{"direction": "ascending", "timestamp": "last_edited_time"}}
			cursor, _ := body["start_cursor"].(string)
			delete(body, "start_cursor")
			if fmt.Sprint(body) != fmt.Sprint(want) {
				t.Errorf("search body = %v", body)
			}
			cursors = append(cursors, cursor)
			if cursor == "" {
				return http.StatusOK, notionMore("p1", notionTestPage("pg_1", "Roadmap", "2026-01-06T10:00:00.000Z", notionWorkspace),
					notionTestPage("pg_2", "Q1", "2026-01-07T10:00:00.000Z", notionUnder("page_id", "pg_1")))
			}
			return http.StatusOK, notionResults(notionTestPage("pg_3", "", "2026-01-08T10:00:00.000+01:00", notionUnder("block_id", "blk_1")),
				notionTestRow("row_1", "2026-01-05T09:00:00.000Z", notionAccountValues))
		}})
		rows, err := read(t, kind, "pages")
		if err != nil {
			t.Fatal(err)
		}
		row := func(id, title, parentType, parentID, dataSource, edited, editor string) map[string]any {
			m := map[string]any{"page_id": id, "title": orNull(title), "url": "https://www.notion.so/" + id, "parent_type": parentType,
				"parent_id": orNull(parentID), "data_source_id": orNull(dataSource), "created_at": "2026-01-05T09:00:00Z",
				"last_edited_at": edited, "created_by_id": "usr_ada", "last_edited_by_id": editor}
			return m
		}
		// Newest edit first, as the documents are offered.
		want := []map[string]any{
			row("pg_3", "", "block", "blk_1", "", "2026-01-08T09:00:00Z", "usr_bot"),
			row("pg_2", "Q1", "page", "pg_1", "", "2026-01-07T10:00:00Z", "usr_bot"),
			row("pg_1", "Roadmap", "workspace", "", "", "2026-01-06T10:00:00Z", "usr_bot"),
			row("row_1", "Acme", "data_source", "ds_1", "ds_1", "2026-01-06T10:00:00Z", "usr_ada"),
		}
		if fmt.Sprint(cursors) != "[ p1]" || fmt.Sprint(rows) != fmt.Sprint(want) {
			t.Fatalf("cursors = %q, rows = %v\nwant %v", cursors, rows, want)
		}
	})
	t.Run("pages a data source's rows and returns its table as read", func(t *testing.T) {
		var cursors []string
		kind := notionAt(t, notionShared(t, nil, []string{notionTestSource("ds_1", "Customer Accounts", "2026-01-01T00:00:00.000Z", notionAccountsSchema)},
			notionRoutes{"/v1/data_sources/ds_1/query": func(r *http.Request) (int, string) {
				body := notionBody(t, r)
				cursor, _ := body["start_cursor"].(string)
				if body["page_size"] != 100.0 || body["result_type"] != "page" || fmt.Sprint(body["sorts"]) != "[map[direction:ascending timestamp:created_time]]" ||
					body["filter"] != nil {
					t.Errorf("query body = %v", body)
				}
				cursors = append(cursors, cursor)
				if cursor == "" {
					return http.StatusOK, notionMore("r1", notionTestRow("row_1", "2026-01-05T09:00:00.000Z", notionAccountValues))
				}
				return http.StatusOK, notionResults(notionTestRow("row_2", "2026-01-05T09:01:00.000Z", `{}`))
			}}))
		sink := &rowSink{}
		written, err := kind.Sync(ctx, src, Table{Name: "customer_accounts"}, sink)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(cursors) != "[ r1]" || len(sink.rows) != 2 || sink.rows[0]["plan"] != "Gold" || sink.rows[1]["renewal_end"] != nil ||
			written.Name != "customer_accounts" || len(written.Columns) != 8 || !written.Dense {
			t.Fatalf("cursors = %q, rows = %v, table = %+v", cursors, sink.rows, written)
		}
	})
	// Notion answers at most 10,000 rows of one query; the read goes on from the last row's minute.
	t.Run("reads a data source past one query's limit", func(t *testing.T) {
		var filters []string
		kind := notionAt(t, notionShared(t, nil, []string{notionTestSource("ds_1", "Big", "2026-01-01T00:00:00.000Z", notionAccountsSchema)},
			notionRoutes{"/v1/data_sources/ds_1/query": func(r *http.Request) (int, string) {
				body := notionBody(t, r)
				filters = append(filters, fmt.Sprint(body["filter"]))
				if body["filter"] == nil {
					return http.StatusOK, notionCut(notionTestRow("row_1", "2026-01-05T09:00:00.000Z", `{}`),
						notionTestRow("row_2", "2026-01-05T09:01:00.000Z", `{}`), notionTestRow("row_3", "2026-01-05T09:01:00.000Z", `{}`))
				}
				return http.StatusOK, notionResults(notionTestRow("row_2", "2026-01-05T09:01:00.000Z", `{}`),
					notionTestRow("row_3", "2026-01-05T09:01:00.000Z", `{}`), notionTestRow("row_4", "2026-01-05T09:02:00.000Z", `{}`))
			}}))
		rows, err := read(t, kind, "big")
		if err != nil {
			t.Fatal(err)
		}
		var ids []any
		for _, row := range rows {
			ids = append(ids, row["page_id"])
		}
		if fmt.Sprint(ids) != "[row_1 row_2 row_3 row_4]" ||
			fmt.Sprint(filters) != "[<nil> map[created_time:map[on_or_after:2026-01-05T09:01:00Z] timestamp:created_time]]" {
			t.Fatalf("rows = %v, filters = %v", ids, filters)
		}
	})
	t.Run("fails a data source with more rows made in one minute than one query answers", func(t *testing.T) {
		kind := notionAt(t, notionShared(t, nil, []string{notionTestSource("ds_1", "Imported", "2026-01-01T00:00:00.000Z", `{}`)},
			notionRoutes{"/v1/data_sources/ds_1/query": func(*http.Request) (int, string) {
				return http.StatusOK, notionCut(notionTestRow("row_1", "2026-01-05T09:00:00.000Z", `{}`), notionTestRow("row_2", "2026-01-05T09:00:00.000Z", `{}`))
			}}))
		rows, err := read(t, kind, "imported")
		notionInvalid(t, err, "Notion answers at most 10,000 rows of a database at a time, and more rows of imported were created in one minute; "+
			"Helix can't read them all")
		if len(rows) != 2 {
			t.Fatalf("rows = %v", rows)
		}
	})
	// A page shows 25 ids of a relation or people value; the rest are read
	// from the property, 100 a request.
	t.Run("reads a relation or people value the row cut at 25 whole", func(t *testing.T) {
		refs := func(kind, prefix string, from, n int) []string {
			var items []string
			for i := from; i < from+n; i++ {
				ref := fmt.Sprintf(`{"id":"%s%d"}`, prefix, i)
				if kind == "people" {
					ref = fmt.Sprintf(`{"object":"user","id":"%s%d"}`, prefix, i)
				}
				items = append(items, fmt.Sprintf(`{"object":"property_item","id":"x","type":%q,%q:%s}`, kind, kind, ref))
			}
			return items
		}
		values := func(relation, people int, more bool) string {
			var projects, users []string
			for i := range relation {
				projects = append(projects, fmt.Sprintf(`{"id":"pg_%d"}`, i))
			}
			for i := range people {
				users = append(users, fmt.Sprintf(`{"object":"user","id":"usr_%d"}`, i))
			}
			return fmt.Sprintf(`{"Projects":{"id":"pr","type":"relation","relation":[%s],"has_more":%t},"Owners":{"id":"ow","type":"people","people":[%s]}}`,
				strings.Join(projects, ","), more, strings.Join(users, ","))
		}
		var asked []string
		kind := notionAt(t, notionShared(t, nil, []string{notionTestSource("ds_1", "Deals", "2026-01-01T00:00:00.000Z",
			`{"Projects":{"id":"pr","type":"relation","relation":{}},"Owners":{"id":"ow","type":"people","people":{}}}`)}, notionRoutes{
			"/v1/data_sources/ds_1/query": func(*http.Request) (int, string) {
				return http.StatusOK, notionResults(notionTestRow("row_1", "2026-01-05T09:00:00.000Z", values(25, 25, true)),
					notionTestRow("row_2", "2026-01-05T09:01:00.000Z", values(3, 24, false)))
			},
			"/v1/pages/row_1/properties/pr": func(r *http.Request) (int, string) {
				q := r.URL.Query()
				asked = append(asked, "pr "+q.Get("page_size")+" "+q.Get("start_cursor"))
				if q.Get("start_cursor") == "" {
					return http.StatusOK, notionMore("c1", refs("relation", "pg_", 0, 100)...)
				}
				return http.StatusOK, notionResults(refs("relation", "pg_", 100, 30)...)
			},
			"/v1/pages/row_1/properties/ow": func(r *http.Request) (int, string) {
				asked = append(asked, "ow "+r.URL.Query().Get("page_size"))
				return http.StatusOK, notionResults(refs("people", "usr_", 0, 30)...)
			},
		}))
		rows, err := read(t, kind, "deals")
		if err != nil {
			t.Fatal(err)
		}
		projects, _ := rows[0]["projects"].([]any)
		owners, _ := rows[0]["owners"].([]any)
		if len(projects) != 130 || projects[129] != "pg_129" || len(owners) != 30 || owners[29] != "usr_29" ||
			len(rows[1]["projects"].([]any)) != 3 || len(rows[1]["owners"].([]any)) != 24 || fmt.Sprint(asked) != "[ow 100 pr 100  pr 100 c1]" {
			t.Fatalf("row_1 = %d projects, %d owners; row_2 = %v; asked = %q", len(projects), len(owners), rows[1], asked)
		}
	})
	t.Run("reads at most 1,000 ids of one value", func(t *testing.T) {
		calls := 0
		kind := notionAt(t, notionShared(t, nil, []string{notionTestSource("ds_1", "Deals", "2026-01-01T00:00:00.000Z",
			`{"Projects":{"id":"pr","type":"relation","relation":{}}}`)}, notionRoutes{
			"/v1/data_sources/ds_1/query": func(*http.Request) (int, string) {
				return http.StatusOK, notionResults(notionTestRow("row_1", "2026-01-05T09:00:00.000Z",
					`{"Projects":{"id":"pr","type":"relation","relation":[{"id":"pg_0"}],"has_more":true}}`))
			},
			"/v1/pages/row_1/properties/pr": func(*http.Request) (int, string) {
				calls++
				var items []string
				for i := range 100 {
					items = append(items, fmt.Sprintf(`{"type":"relation","relation":{"id":"pg_%d_%d"}}`, calls, i))
				}
				return http.StatusOK, notionMore(fmt.Sprintf("c%d", calls), items...)
			},
		}))
		rows, err := read(t, kind, "deals")
		if projects, _ := rows[0]["projects"].([]any); err != nil || len(projects) != 1000 || calls != 10 {
			t.Fatalf("Sync = %v, %d projects after %d calls", err, len(projects), calls)
		}
	})
	// The page went since its query, say: the row keeps what it showed.
	for status, want := range map[int]string{http.StatusNotFound: "", http.StatusBadRequest: "",
		http.StatusUnauthorized: "Notion rejected this secret; check it and enter it again"} {
		t.Run(fmt.Sprintf("keeps the ids a row showed when Notion answers %d for the rest, unless the secret is refused", status), func(t *testing.T) {
			kind := notionAt(t, notionShared(t, nil, []string{notionTestSource("ds_1", "Deals", "2026-01-01T00:00:00.000Z",
				`{"Projects":{"id":"pr","type":"relation","relation":{}}}`)}, notionRoutes{
				"/v1/data_sources/ds_1/query": func(*http.Request) (int, string) {
					return http.StatusOK, notionResults(notionTestRow("row_1", "2026-01-05T09:00:00.000Z",
						`{"Projects":{"id":"pr","type":"relation","relation":[{"id":"pg_0"}],"has_more":true}}`))
				},
				"/v1/pages/row_1/properties/pr": func(*http.Request) (int, string) { return status, `{"object":"error"}` },
			}))
			rows, err := read(t, kind, "deals")
			if want != "" {
				notionRefusal(t, err, want)
				return
			}
			if err != nil || fmt.Sprint(rows[0]["projects"]) != "[pg_0]" {
				t.Fatalf("Sync = (%v, %v)", rows, err)
			}
		})
	}
	t.Run("fails a search Notion cut short", func(t *testing.T) {
		kind := notionAt(t, notionRoutes{"/v1/search": func(*http.Request) (int, string) {
			return http.StatusOK, notionCut(notionTestPage("pg_1", "Roadmap", "2026-01-06T10:00:00.000Z", notionWorkspace))
		}})
		rows, err := read(t, kind, "pages")
		notionInvalid(t, err, "Notion stopped listing the pages shared with this integration at its result limit; share fewer with it")
		if len(rows) != 0 {
			t.Fatalf("rows = %v", rows)
		}
	})
	// The pass searches data sources again to find a table's.
	t.Run("refuses a table whose data source is no longer shared", func(t *testing.T) {
		_, err := read(t, notionAt(t, notionShared(t, nil, nil, nil)), "customer_accounts")
		notionRefusal(t, err, "Notion no longer shares the database of table customer_accounts with this integration; try again")
	})
	// Ending early on an incomplete page would replace the snapshot with part of the table.
	for name, page := range map[string]string{
		"is not JSON":                            `<html>Bad gateway</html>`,
		"lacks its results":                      `{"object":"list","has_more":false,"next_cursor":null}`,
		"has null for its results":               `{"results":null,"has_more":false,"next_cursor":null}`,
		"lacks has_more":                         `{"results":[],"next_cursor":null}`,
		"says more follow but gives no cursor":   `{"results":[],"has_more":true,"next_cursor":null}`,
		"says more follow with an empty cursor":  `{"results":[],"has_more":true,"next_cursor":""}`,
		"lists a result that is not an object":   `{"results":["pg_1"],"has_more":false,"next_cursor":null}`,
		"lists a page without an id":             `{"results":[{"object":"page"}],"has_more":false,"next_cursor":null}`,
		"lists a page without its times":         `{"results":[{"id":"pg_1","created_time":"2026-01-05T09:00:00Z"}],"has_more":false,"next_cursor":null}`,
		"gives back the cursor it was asked for": "",
	} {
		t.Run("fails a search page that "+name, func(t *testing.T) {
			kind := notionAt(t, notionRoutes{"/v1/search": func(r *http.Request) (int, string) {
				if page != "" {
					return http.StatusOK, page
				}
				return http.StatusOK, notionMore("c1")
			}})
			if rows, err := read(t, kind, "pages"); err == nil || len(rows) != 0 {
				t.Fatalf("Sync = (%v, %v); want a failure before any row", rows, err)
			}
		})
	}
	for name, results := range map[string]string{
		"a user without an id":           `{"object":"user","type":"person"}`,
		"a data source without an id":    `{"object":"data_source","title":[]}`,
		"a row without an id":            `{"object":"page"}`,
		"a row without its created time": `{"object":"page","id":"row_1","last_edited_time":"2026-01-06T10:00:00.000Z"}`,
	} {
		t.Run("fails "+name, func(t *testing.T) {
			routes := notionShared(t, nil, []string{notionTestSource("ds_1", "Accounts", "2026-01-01T00:00:00.000Z", `{}`)}, notionRoutes{
				"/v1/users":                   func(*http.Request) (int, string) { return http.StatusOK, notionResults(results) },
				"/v1/data_sources/ds_1/query": func(*http.Request) (int, string) { return http.StatusOK, notionResults(results) },
			})
			table := "accounts"
			switch {
			case strings.Contains(name, "user"):
				table = "users"
			case strings.Contains(name, "data source"):
				routes["/v1/search"] = notionSearchOf(t, nil, []string{results})
			}
			if rows, err := read(t, notionAt(t, routes), table); err == nil || len(rows) != 0 {
				t.Fatalf("Sync = (%v, %v); want a failure", rows, err)
			}
		})
	}
	t.Run("stops a list that never ends", func(t *testing.T) {
		saved := maxNotionPages
		maxNotionPages = 3
		t.Cleanup(func() { maxNotionPages = saved })
		calls := 0
		kind := notionAt(t, notionRoutes{"/v1/users": func(*http.Request) (int, string) {
			calls++
			return http.StatusOK, notionMore(fmt.Sprintf("u%d", calls), notionAdaJSON)
		}})
		_, err := read(t, kind, "users")
		if err == nil || !strings.Contains(err.Error(), "more than 3 pages") || calls != 3 {
			t.Fatalf("Sync error = %v after %d calls", err, calls)
		}
	})
	t.Run("refuses a secret revoked during the read", func(t *testing.T) {
		kind := notionAt(t, notionRoutes{"/v1/users": func(*http.Request) (int, string) { return http.StatusUnauthorized, `{"code":"unauthorized"}` }})
		_, err := read(t, kind, "users")
		notionRefusal(t, err, "Notion rejected this secret; check it and enter it again")
	})
	t.Run("retries a rate limit and an outage", func(t *testing.T) {
		noBackoff(t)
		var mu sync.Mutex
		calls := 0
		kind := notionAt(t, notionRoutes{"/v1/users": func(r *http.Request) (int, string) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			switch calls {
			case 1:
				return http.StatusTooManyRequests, `{"code":"rate_limited"}`
			case 2:
				return http.StatusBadGateway, ``
			}
			return http.StatusOK, notionResults(notionAdaJSON)
		}})
		if rows, err := read(t, kind, "users"); err != nil || len(rows) != 1 || calls != 3 {
			t.Fatalf("Sync = (%v, %v) after %d calls", rows, err, calls)
		}
	})
	// A limit that outlasts the retries fails the attempt with a message the syncer retries on.
	t.Run("refuses a rate limit that outlasts the retries, saying when to try again", func(t *testing.T) {
		waits := notionWaits(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		t.Cleanup(server.Close)
		kind := notionAt(t, nil)
		kind.base = server.URL
		before := time.Now()
		_, err := read(t, kind, "users")
		after := time.Now()
		var refusedErr *RefusedError
		const prefix = "this integration has used up its Notion rate limit; try again after "
		if !errors.As(err, &refusedErr) || !strings.HasPrefix(refusedErr.Message, prefix) {
			t.Fatalf("Sync error = %v, want a rate limit with its time", err)
		}
		at := strings.TrimPrefix(refusedErr.Message, prefix)
		if early, late := before.Add(2*time.Minute).UTC().Format("15:04 UTC"), after.Add(2*time.Minute).UTC().Format("15:04 UTC"); at != early && at != late {
			t.Fatalf("retry at %q, want %s", at, early)
		}
		if fmt.Sprint(*waits) != "[120 120 120 120 120 120]" {
			t.Fatalf("waits = %v, want each of the 6 retries to wait the Retry-After", *waits)
		}
	})
	// Notion's own SDK waits as long as Retry-After asks, up to a minute.
	t.Run("waits the Retry-After Notion asks for, up to a minute", func(t *testing.T) {
		noBackoff(t)
		for retryAfter, want := range map[string]time.Duration{"45": 45 * time.Second, "60": time.Minute, "120": time.Minute, "": 0, "soon": 0, "-5": 0} {
			if got := notionWait(2, retryAfter); got != want {
				t.Errorf("notionWait(%q) = %s, want %s", retryAfter, got, want)
			}
		}
	})
	// Notion's remedy for a query it can't answer in time is a smaller page_size.
	t.Run("asks for half the results of a page Notion could not build in time, for the rest of the list", func(t *testing.T) {
		var asked []string
		kind := notionAt(t, notionRoutes{"/v1/users": func(r *http.Request) (int, string) {
			q := r.URL.Query()
			asked = append(asked, q.Get("page_size")+" "+q.Get("start_cursor"))
			switch len(asked) {
			case 1:
				return http.StatusServiceUnavailable, `{"code":"service_unavailable"}`
			case 2:
				return http.StatusGatewayTimeout, `{"code":"gateway_timeout"}`
			case 3:
				return http.StatusOK, notionMore("u1", notionAdaJSON)
			}
			return http.StatusOK, notionResults(notionGraceJSON)
		}})
		rows, err := read(t, kind, "users")
		if err != nil || len(rows) != 2 || fmt.Sprint(asked) != "[100  50  25  25 u1]" {
			t.Fatalf("Sync = (%v, %v), asked = %q", rows, err, asked)
		}
	})
	t.Run("retries a page that times out at the fewest results, then refuses", func(t *testing.T) {
		noBackoff(t)
		var asked []string
		kind := notionAt(t, notionRoutes{"/v1/users": func(r *http.Request) (int, string) {
			asked = append(asked, r.URL.Query().Get("page_size"))
			return http.StatusGatewayTimeout, `{"code":"gateway_timeout"}`
		}})
		_, err := read(t, kind, "users")
		notionRefusal(t, err, "could not reach Notion; try again")
		if want := "[100 50 25 12 10 10 10 10 10 10 10]"; fmt.Sprint(asked) != want || !strings.Contains(err.Error(), "HTTP 504 for /v1/users") {
			t.Fatalf("asked = %v, want %s; error = %v", asked, want, err)
		}
	})
	t.Run("refuses an outage that outlasts the retries", func(t *testing.T) {
		noBackoff(t)
		kind := notionAt(t, notionRoutes{"/v1/users": func(*http.Request) (int, string) { return http.StatusServiceUnavailable, `{}` }})
		_, err := read(t, kind, "users")
		notionRefusal(t, err, "could not reach Notion; try again")
		if !strings.Contains(err.Error(), "HTTP 503 for /v1/users") {
			t.Fatalf("Sync error = %v, want the status kept for the log", err)
		}
	})
	// Notion allows an average of 3 requests a second; reads stay under it.
	t.Run("paces requests under Notion's rate limit", func(t *testing.T) {
		if notionInterval < 333*time.Millisecond {
			t.Fatalf("notionInterval = %s, more than 3 requests a second", notionInterval)
		}
		var mu sync.Mutex
		var at []time.Time
		kind := notionAt(t, notionRoutes{"/v1/users": func(*http.Request) (int, string) {
			mu.Lock()
			defer mu.Unlock()
			at = append(at, time.Now())
			if len(at) < 3 {
				return http.StatusOK, notionMore(fmt.Sprintf("u%d", len(at)))
			}
			return http.StatusOK, notionResults()
		}})
		notionInterval = 50 * time.Millisecond
		if _, err := read(t, kind, "users"); err != nil {
			t.Fatal(err)
		}
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap < 40*time.Millisecond {
				t.Fatalf("request %d came %s after the one before", i, gap)
			}
		}
	})
	t.Run("refuses a read Notion dropped the connection for", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}))
		t.Cleanup(server.Close)
		kind := notionAt(t, nil)
		kind.base = server.URL
		_, err := read(t, kind, "pages")
		notionRefusal(t, err, "could not reach Notion; try again")
	})
	t.Run("stops when the read is cancelled", func(t *testing.T) {
		kind := notionAt(t, notionRoutes{"/v1/users": notionUsers(t)})
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := kind.Sync(cancelled, src, Table{Name: "users"}, &rowSink{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Sync error = %v, want context.Canceled", err)
		}
	})
	// A snapshot over its row limit stops the read with the writer's error, which the syncer reports.
	t.Run("stops at the writer's error", func(t *testing.T) {
		full := errors.New("row limit")
		kind := notionAt(t, notionShared(t, []string{notionTestPage("pg_1", "Roadmap", "2026-01-06T10:00:00.000Z", notionWorkspace)},
			[]string{notionTestSource("ds_1", "Accounts", "2026-01-01T00:00:00.000Z", `{}`)}, notionRoutes{
				"/v1/data_sources/ds_1/query": func(*http.Request) (int, string) {
					return http.StatusOK, notionResults(notionTestRow("row_1", "2026-01-05T09:00:00.000Z", `{}`))
				},
			}))
		for _, table := range []string{"users", "pages", "accounts"} {
			if _, err := kind.Sync(ctx, src, Table{Name: table}, refusingSink{full}); !errors.Is(err, full) {
				t.Fatalf("%s: Sync error = %v, want the writer's", table, err)
			}
		}
	})
	// One pass searches the data sources once for all their tables.
	t.Run("reads every table in one pass", func(t *testing.T) {
		var mu sync.Mutex
		searches := map[string]int{}
		sources := []string{notionTestSource("ds_1", "Accounts", "2026-01-01T00:00:00.000Z", `{}`), notionTestSource("ds_2", "Deals", "2026-01-02T00:00:00.000Z", `{}`)}
		rows := func(id string) func(*http.Request) (int, string) {
			return func(*http.Request) (int, string) {
				return http.StatusOK, notionResults(notionTestRow(id, "2026-01-05T09:00:00.000Z", `{}`))
			}
		}
		kind := notionAt(t, notionShared(t, nil, nil, notionRoutes{
			"/v1/search": func(r *http.Request) (int, string) {
				filter, _ := notionBody(t, r)["filter"].(map[string]any)
				object, _ := filter["value"].(string)
				mu.Lock()
				searches[object]++
				mu.Unlock()
				if object == "page" {
					return http.StatusOK, notionResults(notionTestPage("pg_1", "Roadmap", "2026-01-06T10:00:00.000Z", notionWorkspace))
				}
				return http.StatusOK, notionResults(sources...)
			},
			"/v1/data_sources/ds_1/query": rows("row_1"), "/v1/data_sources/ds_2/query": rows("row_2"),
		}))
		tables := []Table{{Name: "users"}, {Name: "pages"}, {Name: "accounts"}, {Name: "deals"}}
		sinks, writers := map[string]*rowSink{}, map[string]RowWriter{}
		for _, table := range tables {
			sinks[table.Name] = &rowSink{}
			writers[table.Name] = sinks[table.Name]
		}
		written, err := kind.SyncPass(ctx, src, tables, writers, noDocuments{})
		if err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for name, sink := range sinks {
			counts[name] = len(sink.rows)
		}
		if fmt.Sprint(counts) != "map[accounts:1 deals:1 pages:1 users:3]" || fmt.Sprint(searches) != "map[data_source:1 page:1]" ||
			!slices.Equal(tableNames(Discovery{Tables: written}), []string{"users", "pages", "accounts", "deals"}) || len(written[3].Columns) != 4 {
			t.Fatalf("rows = %v, searches = %v, written = %v", counts, searches, written)
		}
	})
}

// notionMarkdownOf answers each page's Markdown from bodies, 404 for a page
// it has none of, and counts the reads.
func notionMarkdownOf(bodies map[string]string, reads *sync.Map) notionRoutes {
	routes := notionRoutes{}
	for id, body := range bodies {
		routes["/v1/pages/"+id+"/markdown"] = func(r *http.Request) (int, string) {
			n, _ := reads.LoadOrStore(id, new(int))
			*n.(*int)++
			if body == "" {
				return http.StatusNotFound, `{"object":"error","status":404,"code":"object_not_found","message":"Could not find page"}`
			}
			return http.StatusOK, body
		}
	}
	return routes
}

func notionMarkdownJSON(id, markdown string, truncated bool) string {
	return fmt.Sprintf(`{"object":"page_markdown","id":%q,"markdown":%q,"truncated":%t,"unknown_block_ids":[]}`, id, markdown, truncated)
}

func readCount(reads *sync.Map) map[string]int {
	out := map[string]int{}
	reads.Range(func(k, v any) bool {
		out[k.(string)] = *v.(*int)
		return true
	})
	return out
}

func TestNotionDocuments(t *testing.T) {
	ctx := context.Background()
	src := Source{Secret: notionTestSecret}
	// pass reads the pages table in one SyncPass.
	pass := func(t *testing.T, kind *notionKind, docs DocumentWriter) (*rowSink, error) {
		t.Helper()
		sink := &rowSink{}
		_, err := kind.SyncPass(ctx, src, []Table{{Name: "pages"}}, map[string]RowWriter{"pages": sink}, docs)
		return sink, err
	}
	searchOf := func(pages ...string) notionRoutes {
		return notionRoutes{"/v1/search": func(*http.Request) (int, string) { return http.StatusOK, notionResults(pages...) }}
	}
	roadmap := notionTestPage("pg_1", "  Product\nroadmap ", "2026-01-06T10:45:30.000Z", notionWorkspace)

	t.Run("writes each page's Markdown under its title and id", func(t *testing.T) {
		reads := &sync.Map{}
		routes := searchOf(roadmap, notionTestPage("pg_2", "", "2026-01-05T10:00:00.000Z", notionWorkspace))
		maps.Copy(routes, notionMarkdownOf(map[string]string{
			"pg_1": notionMarkdownJSON("pg_1", "\n## Q1\n\n- Ship it\n\n", false),
			"pg_2": notionMarkdownJSON("pg_2", "Long page", true),
		}, reads))
		docs := &granolaDocs{want: map[string]bool{"pages": true}}
		sink, err := pass(t, notionAt(t, routes), docs)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(docs.offers, []string{"pages pg_1 2|2026-01-06T10:45:30Z|2923", "pages pg_2 2|2026-01-05T10:00:00Z|2924"}) || len(sink.rows) != 2 {
			t.Fatalf("offers = %q, rows = %v", docs.offers, sink.rows)
		}
		want := []Document{
			{ExternalID: "pg_1", Version: "2|2026-01-06T10:45:30Z|2923", At: time.Date(2026, 1, 6, 10, 45, 30, 0, time.UTC), Title: "Product roadmap",
				URL: "https://www.notion.so/pg_1", Text: []byte("# Product roadmap\n\nPage id: pg_1\n\n## Q1\n\n- Ship it\n")},
			{ExternalID: "pg_2", Version: "2|2026-01-05T10:00:00Z|2924", At: time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC), Title: "",
				URL: "https://www.notion.so/pg_2", Text: []byte("# Untitled\n\nPage id: pg_2\n\nLong page\n" +
					"\nNotion cut this page short at its block limit; the rest of it is not here.\n")},
		}
		if len(docs.puts) != len(want) {
			t.Fatalf("puts = %v", docs.putNames())
		}
		for i, p := range docs.puts {
			if p.stream != "pages" || !p.doc.At.Equal(want[i].At) || string(p.doc.Text) != string(want[i].Text) ||
				fmt.Sprint(p.doc.ExternalID, p.doc.Version, p.doc.Title, p.doc.URL) != fmt.Sprint(want[i].ExternalID, want[i].Version, want[i].Title, want[i].URL) {
				t.Errorf("put %d = %+v\n%s\nwant %+v\n%s", i, p.doc, p.doc.Text, want[i], want[i].Text)
			}
		}
	})
	// A page is read again once a week at a time of its own, and at each edit.
	t.Run("versions a page's document by its last edit and the week of its re-read", func(t *testing.T) {
		edited := time.Date(2026, 1, 6, 10, 45, 30, 0, time.UTC)
		if v := (notionPage{id: "pg_1", edited: edited}).version(notionTestNow); v != "2|2026-01-06T10:45:30Z|2923" {
			t.Fatalf("version = %q", v)
		}
		// The first minute each version has moved by, over two weeks.
		changes := map[string][]time.Time{}
		for _, id := range []string{"pg_1", "pg_2", "pg_b"} {
			p := notionPage{id: id, edited: edited}
			last := p.version(notionTestNow)
			for at := notionTestNow; at.Before(notionTestNow.Add(14 * 24 * time.Hour)); at = at.Add(time.Minute) {
				if v := p.version(at); v != last {
					changes[id], last = append(changes[id], at), v
				}
			}
		}
		want := map[string][]time.Time{
			"pg_1": {time.Date(2026, 1, 14, 0, 45, 0, 0, time.UTC), time.Date(2026, 1, 21, 0, 45, 0, 0, time.UTC)},
			"pg_2": {time.Date(2026, 1, 16, 17, 13, 0, 0, time.UTC), time.Date(2026, 1, 23, 17, 13, 0, 0, time.UTC)},
			"pg_b": {time.Date(2026, 1, 10, 19, 38, 0, 0, time.UTC), time.Date(2026, 1, 17, 19, 38, 0, 0, time.UTC)},
		}
		if fmt.Sprint(changes) != fmt.Sprint(want) {
			t.Fatalf("version changes = %v\nwant %v", changes, want)
		}
		if v := (notionPage{id: "pg_1", edited: edited.Add(time.Minute)}).version(notionTestNow); v != "2|2026-01-06T10:46:30Z|2923" {
			t.Fatalf("version after an edit = %q", v)
		}
	})
	t.Run("puts no document for a page with an empty body", func(t *testing.T) {
		reads := &sync.Map{}
		routes := searchOf(roadmap)
		maps.Copy(routes, notionMarkdownOf(map[string]string{"pg_1": notionMarkdownJSON("pg_1", " \n\n ", false)}, reads))
		docs := &granolaDocs{want: map[string]bool{"pages": true}}
		sink, err := pass(t, notionAt(t, routes), docs)
		if err != nil || len(sink.rows) != 1 || len(docs.puts) != 0 || readCount(reads)["pg_1"] != 1 {
			t.Fatalf("SyncPass = %v, rows = %v, puts = %v", err, sink.rows, docs.putNames())
		}
	})
	// A page the stream holds at this version costs no Markdown read.
	t.Run("reads Markdown only for a document that is wanted", func(t *testing.T) {
		reads := &sync.Map{}
		routes := searchOf(roadmap)
		maps.Copy(routes, notionMarkdownOf(map[string]string{"pg_1": notionMarkdownJSON("pg_1", "Body", false)}, reads))
		docs := &granolaDocs{}
		sink, err := pass(t, notionAt(t, routes), docs)
		if err != nil || len(sink.rows) != 1 || len(docs.offers) != 1 || len(docs.puts) != 0 || len(readCount(reads)) != 0 {
			t.Fatalf("SyncPass = %v, rows = %v, offers = %q, reads = %v", err, sink.rows, docs.offers, readCount(reads))
		}
	})
	// The row goes and the full read's removal finds its document by key.
	t.Run("leaves out a page Notion no longer finds", func(t *testing.T) {
		reads := &sync.Map{}
		routes := searchOf(roadmap, notionTestPage("pg_2", "Gone", "2026-01-05T10:00:00.000Z", notionWorkspace))
		maps.Copy(routes, notionMarkdownOf(map[string]string{"pg_1": notionMarkdownJSON("pg_1", "Body", false), "pg_2": ""}, reads))
		docs := &granolaDocs{want: map[string]bool{"pages": true}}
		sink, err := pass(t, notionAt(t, routes), docs)
		if err != nil || len(sink.rows) != 1 || sink.rows[0]["page_id"] != "pg_1" || !slices.Equal(docs.putNames(), []string{"pages pg_1"}) {
			t.Fatalf("SyncPass = %v, rows = %v, puts = %v", err, sink.rows, docs.putNames())
		}
	})
	// Every listed page missing is Notion failing; an empty table would drop every stored page.
	t.Run("fails when Notion returns none of the pages it listed", func(t *testing.T) {
		routes := searchOf(roadmap, notionTestPage("pg_2", "Gone", "2026-01-05T10:00:00.000Z", notionWorkspace))
		maps.Copy(routes, notionMarkdownOf(map[string]string{"pg_1": "", "pg_2": ""}, &sync.Map{}))
		sink, err := pass(t, notionAt(t, routes), &granolaDocs{want: map[string]bool{"pages": true}})
		notionRefusal(t, err, "Notion listed 2 pages but returned none of them; try again")
		if len(sink.rows) != 0 {
			t.Fatalf("rows = %v", sink.rows)
		}
	})
	// A page gone while the others are held unchanged is that page unshared, not Notion failing.
	t.Run("keeps the pages it did not read when the one it read is gone", func(t *testing.T) {
		routes := searchOf(roadmap, notionTestPage("pg_2", "Gone", "2026-01-05T10:00:00.000Z", notionWorkspace))
		maps.Copy(routes, notionMarkdownOf(map[string]string{"pg_2": ""}, &sync.Map{}))
		docs := &onlyDocs{id: "pg_2"}
		sink, err := pass(t, notionAt(t, routes), docs)
		if err != nil || len(sink.rows) != 1 || sink.rows[0]["page_id"] != "pg_1" {
			t.Fatalf("SyncPass = %v, rows = %v", err, sink.rows)
		}
	})
	t.Run("reads a page listed twice once, as last listed", func(t *testing.T) {
		reads := &sync.Map{}
		routes := notionRoutes{"/v1/search": func(r *http.Request) (int, string) {
			if cursor, _ := notionBody(t, r)["start_cursor"].(string); cursor == "" {
				return http.StatusOK, notionMore("p1", roadmap)
			}
			return http.StatusOK, notionResults(strings.Replace(roadmap, "2026-01-06T10:45:30.000Z", "2026-01-07T08:00:00.000Z", 1))
		}}
		maps.Copy(routes, notionMarkdownOf(map[string]string{"pg_1": notionMarkdownJSON("pg_1", "Body", false)}, reads))
		docs := &granolaDocs{want: map[string]bool{"pages": true}}
		sink, err := pass(t, notionAt(t, routes), docs)
		if err != nil || len(sink.rows) != 1 || !slices.Equal(docs.offers, []string{"pages pg_1 2|2026-01-07T08:00:00Z|2923"}) || readCount(reads)["pg_1"] != 1 {
			t.Fatalf("SyncPass = %v, rows = %v, offers = %q, reads = %v", err, sink.rows, docs.offers, readCount(reads))
		}
	})
	// The syncer keeps the first documents offered up to its cap, so the newest edit comes first.
	t.Run("offers the newest edits first", func(t *testing.T) {
		docs := &granolaDocs{}
		_, err := pass(t, notionAt(t, searchOf(notionTestPage("pg_b", "B", "2026-01-05T10:00:00.000Z", notionWorkspace),
			notionTestPage("pg_c", "C", "2026-01-07T10:00:00.000Z", notionWorkspace),
			notionTestPage("pg_a", "A", "2026-01-05T10:00:00.000Z", notionWorkspace))), docs)
		want := []string{"pages pg_c 2|2026-01-07T10:00:00Z|2924", "pages pg_a 2|2026-01-05T10:00:00Z|2924", "pages pg_b 2|2026-01-05T10:00:00Z|2923"}
		if err != nil || !slices.Equal(docs.offers, want) {
			t.Fatalf("SyncPass = %v, offers = %q", err, docs.offers)
		}
	})
	for name, answer := range map[string]string{
		"is not JSON":     `{"object":"page_markdown",`,
		"has no markdown": `{"object":"page_markdown","id":"pg_1","truncated":false}`,
		"is null":         `null`,
	} {
		t.Run("fails a Markdown answer that "+name, func(t *testing.T) {
			routes := searchOf(roadmap)
			routes["/v1/pages/pg_1/markdown"] = func(*http.Request) (int, string) { return http.StatusOK, answer }
			if sink, err := pass(t, notionAt(t, routes), &granolaDocs{want: map[string]bool{"pages": true}}); err == nil || len(sink.rows) != 0 {
				t.Fatalf("SyncPass = (%v, %v); want a failure", sink.rows, err)
			}
		})
	}
	t.Run("refuses a page the integration may not read", func(t *testing.T) {
		routes := searchOf(roadmap)
		routes["/v1/pages/pg_1/markdown"] = func(*http.Request) (int, string) { return http.StatusForbidden, `{"code":"restricted_resource"}` }
		_, err := pass(t, notionAt(t, routes), &granolaDocs{want: map[string]bool{"pages": true}})
		notionRefusal(t, err, "this integration cannot read /v1/pages/pg_1/markdown; check its capabilities in Notion and test again")
	})
	t.Run("stops at Put's error", func(t *testing.T) {
		routes := searchOf(roadmap, notionTestPage("pg_2", "Two", "2026-01-05T10:00:00.000Z", notionWorkspace))
		maps.Copy(routes, notionMarkdownOf(map[string]string{"pg_1": notionMarkdownJSON("pg_1", "One", false), "pg_2": notionMarkdownJSON("pg_2", "Two", false)}, &sync.Map{}))
		failed := errors.New("store down")
		docs := &granolaDocs{want: map[string]bool{"pages": true}, fail: failed}
		if _, err := pass(t, notionAt(t, routes), docs); !errors.Is(err, failed) || len(docs.puts) != 1 {
			t.Fatalf("SyncPass error = %v after %d puts, want Put's at once", err, len(docs.puts))
		}
	})
	t.Run("stops at a Markdown read that fails", func(t *testing.T) {
		noBackoff(t)
		routes := searchOf(roadmap)
		routes["/v1/pages/pg_1/markdown"] = func(*http.Request) (int, string) { return http.StatusInternalServerError, `{}` }
		_, err := pass(t, notionAt(t, routes), &granolaDocs{want: map[string]bool{"pages": true}})
		notionRefusal(t, err, "could not reach Notion; try again")
	})
	// Sync has nowhere to Put documents, so it wants none and reads no Markdown.
	t.Run("reads one table through Sync without documents", func(t *testing.T) {
		reads := &sync.Map{}
		routes := searchOf(roadmap)
		maps.Copy(routes, notionMarkdownOf(map[string]string{"pg_1": notionMarkdownJSON("pg_1", "Body", false)}, reads))
		kind := notionAt(t, routes)
		sink := &rowSink{}
		if _, err := kind.Sync(ctx, src, Table{Name: "pages"}, sink); err != nil || len(sink.rows) != 1 || len(readCount(reads)) != 0 {
			t.Fatalf("Sync = %v, rows = %v, reads = %v", err, sink.rows, readCount(reads))
		}
		docs := &granolaDocs{want: map[string]bool{"pages": true}}
		if _, err := kind.SyncDocuments(ctx, src, Table{Name: "pages"}, &rowSink{}, docs); err != nil || len(docs.puts) != 1 {
			t.Fatalf("SyncDocuments = %v, puts = %v", err, docs.putNames())
		}
	})
	// Documents belong to the pages table; a read of the others offers none.
	t.Run("offers no document unless the pages table is read", func(t *testing.T) {
		docs := &granolaDocs{want: map[string]bool{"pages": true}}
		kind := notionAt(t, notionRoutes{"/v1/users": notionUsers(t)})
		if _, err := kind.SyncDocuments(ctx, src, Table{Name: "users"}, &rowSink{}, docs); err != nil || len(docs.offers) != 0 {
			t.Fatalf("SyncDocuments = %v, offers = %q", err, docs.offers)
		}
	})
}

// onlyDocs is a DocumentWriter that wants only the document of id, as one
// that holds every other at its version.
type onlyDocs struct{ id string }

func (d *onlyDocs) Want(_, id, _ string) bool                   { return id == d.id }
func (d *onlyDocs) Put(context.Context, string, Document) error { return nil }
func (d *onlyDocs) Drop(string, string) error                   { return nil }
func (d *onlyDocs) Held(string, string) (string, bool)          { return "", false }
func (d *onlyDocs) Paused(string) bool                          { return false }
