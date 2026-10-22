package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func noBackoff(t *testing.T) {
	t.Helper()
	saved := retryBackoff
	retryBackoff = func(int, string) time.Duration { return 0 }
	t.Cleanup(func() { retryBackoff = saved })
}

// routeServer answers each request with the handler registered for its path.
func routeServer(t *testing.T, routes map[string]func(r *http.Request) (int, string)) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler, ok := routes[r.URL.Path]
		if !ok {
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		status, body := handler(r)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestStripeSync(t *testing.T) {
	// Pages follow starting_after, and a rate limit is retried rather than failing the sync.
	t.Run("pages through a list and retries a rate limit", func(t *testing.T) {
		noBackoff(t)
		var mu sync.Mutex
		calls := 0
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/v1/customers": func(r *http.Request) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if calls == 1 {
					return http.StatusTooManyRequests, `{}`
				}
				if r.URL.Query().Get("starting_after") == "" {
					return http.StatusOK, `{"data":[{"id":"cus_1","object":"customer","created":1700000000,"default_source":null}],"has_more":true}`
				}
				if r.URL.Query().Get("starting_after") != "cus_1" || r.URL.Query().Get("limit") != "100" {
					t.Errorf("second page query = %s", r.URL.RawQuery)
				}
				return http.StatusOK, `{"data":[{"id":"cus_2","object":"customer","created":1700000001}],"has_more":false}`
			},
		})
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "stripe").Sync(context.Background(), Source{Secret: "sk_test_abc"}, Table{Name: "customers"}, sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.rows) != 2 || sink.rows[0]["customer_id"] != "cus_1" || sink.rows[0]["created"] != "2023-11-14T22:13:20.000Z" {
			t.Fatalf("rows = %v", sink.rows)
		}
		if _, ok := sink.rows[0]["object"]; ok {
			t.Fatalf("object field kept: %v", sink.rows[0])
		}
	})
	// Expanded and plain references both become the referenced table's key.
	t.Run("collapses references to ids", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/v1/balance_transactions": func(*http.Request) (int, string) {
				return http.StatusOK, `{"data":[{"id":"txn_1","source":{"id":"ch_1","object":"charge"},"amount":0}],"has_more":false}`
			},
		})
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "stripe").Sync(context.Background(), Source{Secret: "sk_test_abc"}, Table{Name: "balance_transactions"}, sink); err != nil {
			t.Fatal(err)
		}
		row := sink.rows[0]
		if row["balance_transaction_id"] != "txn_1" || row["source_id"] != "ch_1" || row["amount"] != float64(0) {
			t.Fatalf("row = %v", row)
		}
	})
	// Child rows come from the parent's embedded list, or from their own endpoint when it is truncated.
	t.Run("lists child rows through their parents", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/v1/invoices": func(*http.Request) (int, string) {
				return http.StatusOK, `{"data":[
				  {"id":"in_1","lines":{"data":[{"id":"il_1","object":"line_item","pricing":{"price_details":{"price":"price_9"}}}],"has_more":false}},
				  {"id":"in_2","lines":{"data":[],"has_more":true}}],"has_more":false}`
			},
			"/v1/invoices/in_2/lines": func(*http.Request) (int, string) {
				return http.StatusOK, `{"data":[{"id":"il_2","price":{"id":"price_1"}}],"has_more":false}`
			},
		})
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "stripe").Sync(context.Background(), Source{Secret: "sk_test_abc"}, Table{Name: "invoice_line_items"}, sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.rows) != 2 {
			t.Fatalf("rows = %v", sink.rows)
		}
		first, second := sink.rows[0], sink.rows[1]
		if first["line_item_id"] != "il_1" || first["invoice_id"] != "in_1" || first["price_id"] != "price_9" ||
			second["invoice_id"] != "in_2" || second["price_id"] != "price_1" {
			t.Fatalf("rows = %v", sink.rows)
		}
	})
}

func TestWorkOSSync(t *testing.T) {
	t.Run("pages with the after cursor", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/organizations": func(r *http.Request) (int, string) {
				if r.URL.Query().Get("order") != "asc" {
					t.Errorf("query = %s", r.URL.RawQuery)
				}
				if r.URL.Query().Get("after") == "" {
					return http.StatusOK, `{"data":[{"id":"org_1","object":"organization"}],"list_metadata":{"after":"org_1"}}`
				}
				return http.StatusOK, `{"data":[{"id":"org_2","object":"organization"}],"list_metadata":{"after":null}}`
			},
		})
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "workos").Sync(context.Background(), Source{Secret: "sk_test_abc"}, Table{Name: "organizations"}, sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.rows) != 2 || sink.rows[1]["organization_id"] != "org_2" {
			t.Fatalf("rows = %v", sink.rows)
		}
	})
	// A change read fetches a membership by id whatever its status, so the full
	// read must list pending and inactive ones too, or the two reads disagree.
	t.Run("lists memberships of every status", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/organizations": func(*http.Request) (int, string) {
				return http.StatusOK, `{"data":[{"id":"org_1"}],"list_metadata":{}}`
			},
			"/user_management/organization_memberships": func(r *http.Request) (int, string) {
				q := r.URL.Query()
				if q.Get("organization_id") != "org_1" || q.Get("statuses") != "active,inactive,pending" {
					t.Errorf("memberships listed as %s", r.URL.RawQuery)
				}
				return http.StatusOK, `{"data":[{"id":"om_1","status":"pending"}],"list_metadata":{}}`
			},
		})
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "workos").Sync(context.Background(), Source{Secret: "sk_test_abc"}, Table{Name: "organization_memberships"}, sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.rows) != 1 || sink.rows[0]["status"] != "pending" || sink.rows[0]["organization_id"] != "org_1" {
			t.Fatalf("rows = %v", sink.rows)
		}
	})
	// Group membership rows are listed per group of each directory and reduced to the (group, user) pair.
	t.Run("lists members per group of each directory", func(t *testing.T) {
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/directories": func(*http.Request) (int, string) {
				return http.StatusOK, `{"data":[{"id":"dir_1"},{"id":"dir_2"}],"list_metadata":{}}`
			},
			"/directory_groups": func(r *http.Request) (int, string) {
				switch r.URL.Query().Get("directory") {
				case "":
					return http.StatusUnprocessableEntity, `{"message":"Validation failed"}`
				case "dir_1":
					return http.StatusOK, `{"data":[{"id":"grp_1"}],"list_metadata":{}}`
				}
				return http.StatusOK, `{"data":[{"id":"grp_2"}],"list_metadata":{}}`
			},
			"/directory_users": func(r *http.Request) (int, string) {
				group := r.URL.Query().Get("group")
				return http.StatusOK, fmt.Sprintf(`{"data":[{"id":"usr_%s","groups":[{"id":"x"}],"email":"a@b.c"}],"list_metadata":{}}`, group)
			},
		})
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "workos").Sync(context.Background(), Source{Secret: "sk_test_abc"}, Table{Name: "directory_group_members"}, sink); err != nil {
			t.Fatal(err)
		}
		want := []map[string]any{
			{"directory_group_id": "grp_1", "directory_user_id": "usr_grp_1"},
			{"directory_group_id": "grp_2", "directory_user_id": "usr_grp_2"},
		}
		if fmt.Sprint(sink.rows) != fmt.Sprint(want) {
			t.Fatalf("rows = %v", sink.rows)
		}
	})
}

func TestPostHogSync(t *testing.T) {
	shrinkPage := func(t *testing.T, n int) {
		saved := posthogPage
		posthogPage = n
		t.Cleanup(func() { posthogPage = saved })
	}
	queries := func(t *testing.T, answer func(sql string) string) (string, *[]string) {
		var mu sync.Mutex
		var seen []string
		url := routeServer(t, map[string]func(*http.Request) (int, string){
			"/api/projects/7/query/": func(r *http.Request) (int, string) {
				var body struct {
					Query struct {
						Query string `json:"query"`
					} `json:"query"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				seen = append(seen, body.Query.Query)
				mu.Unlock()
				return http.StatusOK, answer(body.Query.Query)
			},
			"/api/projects/7/groups_types/": func(*http.Request) (int, string) {
				return http.StatusOK, `[{"group_type_index":0,"group_type":"company"}]`
			},
		})
		return url, &seen
	}
	settings := func(days int) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"region":"us","project_id":"7","event_days":%d}`, days))
	}

	// Each page continues after the last row's ordering key.
	t.Run("pages persons by keyset", func(t *testing.T) {
		shrinkPage(t, 2)
		url, seen := queries(t, func(sql string) string {
			if strings.Contains(sql, "toString(id) > 'p2'") {
				return `{"results":[["2024-01-03",true,"{}","p3"]]}`
			}
			return `{"results":[["2024-01-01",false,"{\"email\":\"a@b.c\"}","p1"],["2024-01-02",true,null,"p2"]]}`
		})
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "posthog").Sync(context.Background(), Source{Secret: "phx_abc", Settings: settings(30)}, Table{Name: "persons"}, sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.rows) != 3 || sink.rows[0]["email"] != "a@b.c" || sink.rows[2]["person_id"] != "p3" || len(*seen) != 2 {
			t.Fatalf("rows = %v, queries = %q", sink.rows, *seen)
		}
		if !strings.HasSuffix((*seen)[0], "ORDER BY toString(id) LIMIT 2") {
			t.Fatalf("first query = %q", (*seen)[0])
		}
	})
	// Events are read one UTC day at a time over the configured window.
	t.Run("reads events one day at a time", func(t *testing.T) {
		url, seen := queries(t, func(string) string {
			return `{"results":[["$pageview","2024-01-01T00:00:00Z","d1","p1","{\"$current_url\":\"/x\"}","e1"]]}`
		})
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "posthog").Sync(context.Background(), Source{Secret: "phx_abc", Settings: settings(3)}, Table{Name: "events"}, sink); err != nil {
			t.Fatal(err)
		}
		if len(*seen) != 3 || len(sink.rows) != 3 || sink.rows[0]["current_url"] != "/x" || sink.rows[0]["event_id"] != "e1" {
			t.Fatalf("rows = %v, queries = %q", sink.rows, *seen)
		}
		today := time.Now().UTC().Format(time.DateOnly)
		if !strings.Contains((*seen)[2], "toDateTime('"+today+" 00:00:00', 'UTC')") {
			t.Fatalf("last window = %q", (*seen)[2])
		}
	})
	t.Run("names groups by their type", func(t *testing.T) {
		url, _ := queries(t, func(string) string {
			return `{"results":[["2024-01-01","{\"name\":\"Acme\"}",0,"acme"],["2024-01-01","{}",3,"x"]]}`
		})
		sink := &rowSink{}
		if _, err := kindOf(t, catalogFor(url), "posthog").Sync(context.Background(), Source{Secret: "phx_abc", Settings: settings(30)}, Table{Name: "groups"}, sink); err != nil {
			t.Fatal(err)
		}
		if sink.rows[0]["group_type"] != "company" || sink.rows[0]["name"] != "Acme" || sink.rows[1]["group_type"] != "group_3" {
			t.Fatalf("rows = %v", sink.rows)
		}
	})
}

func TestDatabaseSync(t *testing.T) {
	local := NewCatalog(DefaultEndpoints(), Guard{AllowPrivate: true}, S3Options{}, EventsOptions{})
	seed := "INSERT INTO customers VALUES (1, 'a@b.c'), (2, 'd@e.f'); INSERT INTO orders VALUES (10, 1, 12.50), (11, NULL, NULL);"

	t.Run("copies a mysql table row by row", func(t *testing.T) {
		database := mysqlFixture(t)
		seedMySQL(t, database, seed)
		settings, _ := json.Marshal(map[string]any{"host": "127.0.0.1", "port": 3307, "database": database, "user": "root", "disable_tls": true})
		sink := &rowSink{}
		if _, err := kindOf(t, local, "mysql").Sync(context.Background(), Source{Settings: settings, Secret: "helix"}, Table{Name: "orders"}, sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.rows) != 2 || sink.rows[0]["id"] != float64(10) || sink.rows[1]["customer_id"] != nil {
			t.Fatalf("rows = %v", sink.rows)
		}
	})
	t.Run("copies a postgres table row by row", func(t *testing.T) {
		schema := postgresFixture(t)
		seedPostgres(t, schema, seed)
		settings, _ := json.Marshal(map[string]any{
			"host": testPostgresHost, "port": testPostgresPort, "database": "helix_connector_test",
			"user": "helix", "schema": schema, "disable_tls": true,
		})
		sink := &rowSink{}
		if _, err := kindOf(t, local, "postgres").Sync(context.Background(), Source{Settings: settings, Secret: "helix"}, Table{Name: "customers"}, sink); err != nil {
			t.Fatal(err)
		}
		if len(sink.rows) != 2 || sink.rows[1]["email"] != "d@e.f" {
			t.Fatalf("rows = %v", sink.rows)
		}
	})
}
