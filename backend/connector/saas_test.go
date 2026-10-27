package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// apiServer answers each path with a status (default 200) and records requests.
type apiServer struct {
	mu       sync.Mutex
	statuses map[string]int
	bodies   map[string]string
	requests []*http.Request
	url      string
}

func newAPIServer(t *testing.T, statuses map[string]int, bodies map[string]string) *apiServer {
	t.Helper()
	s := &apiServer{statuses: statuses, bodies: bodies}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests = append(s.requests, r)
		key := r.URL.Path
		if r.Method == http.MethodPost {
			var body struct {
				Query struct {
					Query string `json:"query"`
				} `json:"query"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			key = body.Query.Query
		}
		status := http.StatusOK
		if code, ok := s.statuses[key]; ok {
			status = code
		}
		w.WriteHeader(status)
		if b, ok := s.bodies[key]; ok {
			_, _ = w.Write([]byte(b))
			return
		}
		_, _ = w.Write([]byte(`{"data":[],"results":[]}`))
	}))
	t.Cleanup(server.Close)
	s.url = server.URL
	return s
}

func catalogFor(url string) *Catalog {
	return NewCatalog(Endpoints{Stripe: url, WorkOS: url, PostHogUS: url, PostHogEU: url, GitHub: url, Linear: url, Calcom: url, Granola: url}, Guard{AllowPrivate: true}, S3Options{}, EventsOptions{})
}

func kindOf(t *testing.T, c *Catalog, name string) Kind {
	t.Helper()
	k, err := c.Kind(name)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func tableNames(d Discovery) []string {
	var names []string
	for _, t := range d.Tables {
		names = append(names, t.Name)
	}
	return names
}

func findTable(t *testing.T, d Discovery, name string) Table {
	t.Helper()
	for _, tbl := range d.Tables {
		if tbl.Name == name {
			return tbl
		}
	}
	t.Fatalf("table %s not discovered: %v", name, tableNames(d))
	return Table{}
}

func TestStripeConnector(t *testing.T) {
	t.Run("rejects something that is not a stripe key", func(t *testing.T) {
		_, err := kindOf(t, catalogFor("http://unused"), "stripe").Prepare(Source{Secret: "pk_live_abc"})
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) {
			t.Fatalf("Prepare error = %v, want InvalidError", err)
		}
	})
	t.Run("records whether the key is live", func(t *testing.T) {
		settings, err := kindOf(t, catalogFor("http://unused"), "stripe").Prepare(Source{Secret: " rk_live_abc123 "})
		if err != nil || string(settings) != `{"mode":"live"}` {
			t.Fatalf("Prepare = (%s, %v)", settings, err)
		}
	})
	// A restricted key can lack some objects; those are reported, not fatal, and their keys are dropped.
	t.Run("lists readable objects and drops foreign keys into unreadable ones", func(t *testing.T) {
		api := newAPIServer(t, map[string]int{"/v1/balance_transactions": http.StatusForbidden}, nil)
		d, err := kindOf(t, catalogFor(api.url), "stripe").Discover(context.Background(), Source{Secret: "sk_test_abc"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(unavailableNames(d), []string{"balance_transactions"}) || len(d.Tables) != 14 {
			t.Fatalf("tables = %v, unavailable = %v", tableNames(d), d.Unavailable)
		}
		refunds := findTable(t, d, "refunds")
		if !slices.Equal(refunds.PrimaryKey, []string{"refund_id"}) || len(refunds.ForeignKeys) != 2 {
			t.Fatalf("refunds = %+v", refunds)
		}
		for _, r := range api.requests {
			if r.Header.Get("Stripe-Version") != stripeVersion || r.URL.Query().Get("limit") != "1" {
				t.Fatalf("probe %s sent version %q", r.URL, r.Header.Get("Stripe-Version"))
			}
			if r.URL.Path == "/v1/subscriptions" && r.URL.Query().Get("status") != "all" {
				t.Fatalf("subscriptions probed without status=all: %s", r.URL)
			}
		}
	})
	t.Run("a rejected key fails the test", func(t *testing.T) {
		api := newAPIServer(t, map[string]int{"/v1/customers": http.StatusUnauthorized}, nil)
		_, err := kindOf(t, catalogFor(api.url), "stripe").Discover(context.Background(), Source{Secret: "sk_test_abc"})
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || !strings.Contains(refusedErr.Message, "rejected") {
			t.Fatalf("Discover error = %v, want a rejected key", err)
		}
	})
	t.Run("a key that reads nothing fails the test", func(t *testing.T) {
		statuses := map[string]int{}
		for _, path := range stripeListPaths {
			statuses[strings.Split(path, "?")[0]] = http.StatusForbidden
		}
		api := newAPIServer(t, statuses, nil)
		_, err := kindOf(t, catalogFor(api.url), "stripe").Discover(context.Background(), Source{Secret: "sk_test_abc"})
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) {
			t.Fatalf("Discover error = %v, want RefusedError", err)
		}
	})
}

func TestWorkOSConnector(t *testing.T) {
	t.Run("takes no settings", func(t *testing.T) {
		_, err := kindOf(t, catalogFor("http://unused"), "workos").Prepare(Source{Secret: "sk_test_abc", Settings: json.RawMessage(`{"host":"x"}`)})
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) {
			t.Fatalf("Prepare error = %v, want InvalidError", err)
		}
	})
	// WorkOS answers 404 for a product the environment lacks; its child objects go with it.
	t.Run("drops objects listed under an unreadable parent", func(t *testing.T) {
		api := newAPIServer(t, map[string]int{"/directories": http.StatusNotFound}, nil)
		d, err := kindOf(t, catalogFor(api.url), "workos").Discover(context.Background(), Source{Secret: "sk_test_abc"})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(unavailableNames(d), []string{"directories", "directory_users", "directory_groups", "directory_group_members"}) {
			t.Fatalf("unavailable = %v", d.Unavailable)
		}
		memberships := findTable(t, d, "organization_memberships")
		if len(memberships.ForeignKeys) != 2 {
			t.Fatalf("organization_memberships = %+v", memberships)
		}
	})
	// WorkOS answers 422 for /directory_groups without a directory or user.
	t.Run("never lists directory groups without a directory", func(t *testing.T) {
		api := newAPIServer(t, map[string]int{"/directory_groups": http.StatusUnprocessableEntity}, nil)
		d, err := kindOf(t, catalogFor(api.url), "workos").Discover(context.Background(), Source{Secret: "sk_test_abc"})
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Unavailable) != 0 {
			t.Fatalf("unavailable = %v", d.Unavailable)
		}
		members := findTable(t, d, "directory_group_members")
		if !slices.Equal(members.PrimaryKey, []string{"directory_group_id", "directory_user_id"}) || len(members.ForeignKeys) != 2 {
			t.Fatalf("directory_group_members = %+v", members)
		}
	})
}

func TestPostHogConnector(t *testing.T) {
	t.Run("defaults to the us cloud and thirty days of events", func(t *testing.T) {
		settings, err := kindOf(t, catalogFor("http://unused"), "posthog").Prepare(Source{Secret: "phx_abc"})
		if err != nil || string(settings) != `{"region":"us","event_days":30}` {
			t.Fatalf("Prepare = (%s, %v)", settings, err)
		}
	})
	for name, settings := range map[string]string{
		"a plain-http custom host":    `{"region":"custom","host":"http://posthog.example.com"}`,
		"a host on a cloud region":    `{"region":"us","host":"https://posthog.example.com"}`,
		"a project id that is a name": `{"project_id":"web"}`,
		"a year and a day of events":  `{"event_days":366}`,
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := kindOf(t, catalogFor("http://unused"), "posthog").Prepare(Source{Secret: "phx_abc", Settings: json.RawMessage(settings)})
			var invalidErr *InvalidError
			if !errors.As(err, &invalidErr) {
				t.Fatalf("Prepare error = %v, want InvalidError", err)
			}
		})
	}
	// The only project the key sees is stored, so later syncs need no lookup.
	t.Run("resolves the only project and reports unreadable tables", func(t *testing.T) {
		api := newAPIServer(t, map[string]int{"SELECT 1 FROM groups LIMIT 1": http.StatusBadRequest},
			map[string]string{"/api/projects/": `{"results":[{"id":42,"name":"Web"}]}`})
		kind := kindOf(t, catalogFor(api.url), "posthog")
		settings, err := kind.Prepare(Source{Secret: "phx_abc"})
		if err != nil {
			t.Fatal(err)
		}
		d, err := kind.Discover(context.Background(), Source{Secret: "phx_abc", Settings: settings})
		if err != nil {
			t.Fatal(err)
		}
		if string(d.Settings) != `{"region":"us","project_id":"42","event_days":30}` {
			t.Fatalf("settings = %s", d.Settings)
		}
		if !slices.Equal(unavailableNames(d), []string{"groups"}) {
			t.Fatalf("unavailable = %v", d.Unavailable)
		}
		counts := findTable(t, d, "daily_event_counts")
		if !slices.Equal(counts.PrimaryKey, []string{"day", "event", "person_id"}) {
			t.Fatalf("daily_event_counts = %+v", counts)
		}
		for _, r := range api.requests {
			if r.Method == http.MethodPost && r.URL.Path != "/api/projects/42/query/" {
				t.Fatalf("queried %s", r.URL.Path)
			}
		}
	})
	// A rate limit or an outage says nothing about the table, so it must not
	// make the table unavailable: a sync would then fail it for good.
	t.Run("refuses a probe that was rate limited or failed", func(t *testing.T) {
		for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway} {
			api := newAPIServer(t, map[string]int{"SELECT 1 FROM groups LIMIT 1": status}, nil)
			kind := kindOf(t, catalogFor(api.url), "posthog")
			_, err := kind.Discover(context.Background(), Source{Secret: "phx_abc", Settings: json.RawMessage(`{"region":"us","project_id":"42","event_days":30}`)})
			var refusedErr *RefusedError
			if !errors.As(err, &refusedErr) || refusedErr.Message != fmt.Sprintf("PostHog returned HTTP %d; try again", status) {
				t.Fatalf("status %d: Discover error = %v, want a refusal", status, err)
			}
		}
	})
	t.Run("asks which project when the key sees several", func(t *testing.T) {
		api := newAPIServer(t, nil, map[string]string{"/api/projects/": `{"results":[{"id":1,"name":"Web"},{"id":2,"name":"App"}]}`})
		kind := kindOf(t, catalogFor(api.url), "posthog")
		settings, _ := kind.Prepare(Source{Secret: "phx_abc"})
		_, err := kind.Discover(context.Background(), Source{Secret: "phx_abc", Settings: settings})
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) || !strings.Contains(invalidErr.Message, "Web (1), App (2)") {
			t.Fatalf("Discover error = %v, want the project choice", err)
		}
	})
	// The guard applies to PostHog too: a custom host is customer-chosen.
	t.Run("refuses a custom host inside our network", func(t *testing.T) {
		kind := NewCatalog(DefaultEndpoints(), Guard{}, S3Options{}, EventsOptions{}).kinds[2]
		_, err := kind.Discover(context.Background(), Source{Secret: "phx_abc", Settings: json.RawMessage(`{"region":"custom","host":"https://127.0.0.1","event_days":30}`)})
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) {
			t.Fatalf("Discover error = %v, want InvalidError", err)
		}
	})
}

func unavailableNames(d Discovery) []string {
	var out []string
	for _, u := range d.Unavailable {
		out = append(out, u.Name)
	}
	return out
}
