package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// A token a broker vended is in no format of ours: each kind takes one
// without its key's prefix, or none at all to check settings alone, and
// still checks its settings.
func TestSignedInKindsPrepare(t *testing.T) {
	catalog := catalogFor("http://unused")
	for _, tc := range []struct{ kind, settings string }{
		{"slack", `{"history_days":30}`},
		{"notion", `{}`},
		{"posthog", `{"region":"us","project_id":"42"}`},
		{"calcom", `{}`},
		{"stripe", `{}`},
		{"google", `{"event_days":30}`},
	} {
		for _, secret := range []string{"an-opaque-vended-token", ""} {
			if _, err := kindOf(t, catalog, tc.kind).Prepare(Source{Secret: secret, Settings: json.RawMessage(tc.settings), SignIn: true}); err != nil {
				t.Errorf("%s Prepare(%q) = %v", tc.kind, secret, err)
			}
			_, err := kindOf(t, catalog, tc.kind).Prepare(Source{Secret: "an-opaque-vended-token", Settings: json.RawMessage(tc.settings)})
			var invalidErr *InvalidError
			if !errors.As(err, &invalidErr) {
				t.Errorf("%s takes a key it can't read the format of: %v", tc.kind, err)
			}
		}
	}

	// Refused for a signed-in connector as settings_invalid: a Slack user
	// token reads as its person, and PostHog's other cloud and a
	// self-hosted PostHog keep their keys: Pipes' PostHog provider
	// authorizes in one region.
	for _, tc := range []struct{ kind, secret, settings, want string }{
		{"slack", "xoxp-1-2-3", `{}`, "user token"},
		{"posthog", "tok", `{"region":"custom","host":"https://posthog.acme.test"}`, "PostHog US Cloud only"},
		{"posthog", "tok", `{"region":"eu"}`, "connect PostHog EU Cloud or a self-hosted PostHog with a personal API key"},
		{"posthog", "", `{"region":"eu","project_id":"42"}`, "US Cloud only"},
		{"google", "tok", `{"subject":"ada@acme.test"}`, "acts as no other"},
	} {
		_, err := kindOf(t, catalog, tc.kind).Prepare(Source{Secret: tc.secret, Settings: json.RawMessage(tc.settings), SignIn: true})
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) || !strings.Contains(invalidErr.Message, tc.want) {
			t.Errorf("%s Prepare = %v, want it refused saying %q", tc.kind, err, tc.want)
		}
	}
	if _, err := kindOf(t, catalog, "slack").Prepare(Source{Secret: "xoxb-1-2-3", SignIn: true}); err != nil {
		t.Errorf("a vended bot token: %v", err)
	}
}

// Each kind names who signed in, from its own endpoint.
func TestSignedInKindsIdentity(t *testing.T) {
	ctx := context.Background()
	routes := map[string]func(*http.Request) (int, string){
		"/api/auth.test": func(*http.Request) (int, string) {
			return http.StatusOK, `{"ok":true,"team":"Acme","team_id":"T1","user":"foundry","user_id":"U1"}`
		},
		"/v1/users/me": func(*http.Request) (int, string) {
			return http.StatusOK, `{"object":"user","type":"bot","name":"Helix Foundry","bot":{"owner":{"type":"workspace","workspace":true},"workspace_name":"Acme HQ"}}`
		},
		"/api/users/@me/": func(*http.Request) (int, string) {
			return http.StatusOK, `{"email":"ada@acme.test","organization":{"name":"Acme"}}`
		},
		"/v2/me": func(*http.Request) (int, string) {
			return http.StatusOK, `{"status":"success","data":{"id":7,"username":"ada","email":"ada@acme.test"}}`
		},
		"/v1/account": func(*http.Request) (int, string) {
			return http.StatusOK, `{"id":"acct_1","business_profile":{"name":"Acme Inc"},"settings":{"dashboard":{"display_name":"Acme"}}}`
		},
		"/drive/v3/about": func(r *http.Request) (int, string) {
			if r.Header.Get("Authorization") != "Bearer vended-google" {
				return http.StatusUnauthorized, `{}`
			}
			return http.StatusOK, `{"user":{"emailAddress":"ada@acme.test","displayName":"Ada"}}`
		},
	}
	url := routeServer(t, routes)
	catalog := NewCatalog(Endpoints{Slack: url + "/api", Notion: url, PostHogUS: url, PostHogEU: url, Calcom: url, Stripe: url, Google: url,
		GoogleToken: url + "/token"}, Guard{AllowPrivate: true}, S3Options{}, EventsOptions{})
	for _, tc := range []struct {
		kind, settings, secret, want string
	}{
		{"slack", `{"history_days":90}`, "xoxb-1", "foundry in Acme"},
		{"notion", `{}`, "vended-notion", "Acme HQ"},
		{"posthog", `{"region":"us","event_days":30}`, "vended-posthog", "ada@acme.test in Acme"},
		{"calcom", `{}`, "vended-calcom", "ada"},
		{"stripe", `{}`, "vended-stripe", "Acme"},
		{"google", `{"event_days":30}`, "vended-google", "ada@acme.test"},
	} {
		got, err := kindOf(t, catalog, tc.kind).(Identifier).Identity(ctx, Source{Secret: tc.secret, Settings: json.RawMessage(tc.settings), SignIn: true})
		if err != nil || got != tc.want {
			t.Errorf("%s Identity = %q, %v; want %q", tc.kind, got, err, tc.want)
		}
	}
}

// A provider rejecting a signed-in connector's token is an AuthError, so
// the sync asks the broker for a fresh one and the finish says the token
// was rejected.
func TestSignedInKindsRejectToken(t *testing.T) {
	ctx := context.Background()
	rejecting := func(*http.Request) (int, string) { return http.StatusUnauthorized, `{}` }
	url := routeServer(t, map[string]func(*http.Request) (int, string){
		"/api/auth.test":  func(*http.Request) (int, string) { return http.StatusOK, `{"ok":false,"error":"token_revoked"}` },
		"/v1/users/me":    rejecting,
		"/api/users/@me/": rejecting,
		"/v2/me":          rejecting,
		"/v1/account":     rejecting,
		"/drive/v3/about": rejecting,
	})
	catalog := NewCatalog(Endpoints{Slack: url + "/api", Notion: url, PostHogUS: url, PostHogEU: url, Calcom: url, Stripe: url, Google: url,
		GoogleToken: url + "/token"}, Guard{AllowPrivate: true}, S3Options{}, EventsOptions{})
	for kind, settings := range map[string]string{"slack": `{}`, "notion": `{}`, "posthog": `{"region":"us"}`, "calcom": `{}`, "stripe": `{}`,
		"google": `{"event_days":30}`} {
		_, err := kindOf(t, catalog, kind).(Identifier).Identity(ctx, Source{Secret: "rejected", Settings: json.RawMessage(settings), SignIn: true})
		var authErr *AuthError
		var refusedErr *RefusedError
		if !errors.As(err, &authErr) || !errors.As(err, &refusedErr) {
			t.Errorf("%s: %v, want an AuthError in a refusal", kind, err)
		}
	}
}

// A vended Stripe key whose prefix doesn't say its mode takes it from its
// balance, Stripe's Account carrying none; one whose prefix does keeps it.
func TestSignedInStripeMode(t *testing.T) {
	api := newAPIServer(t, nil, map[string]string{"/v1/balance": `{"object":"balance","livemode":true}`})
	stripe := kindOf(t, catalogFor(api.url), "stripe")
	prepared, err := stripe.Prepare(Source{Secret: "an-opaque-vended-token", SignIn: true})
	if err != nil || string(prepared) != `{"mode":""}` {
		t.Fatalf("Prepare = %s, %v", prepared, err)
	}
	d, err := stripe.Discover(context.Background(), Source{Secret: "an-opaque-vended-token", Settings: prepared, SignIn: true})
	if err != nil {
		t.Fatal(err)
	}
	var s StripeSettings
	if err := json.Unmarshal(d.Settings, &s); err != nil || s.Mode != "live" {
		t.Fatalf("settings = %s, %v; want live from the balance", d.Settings, err)
	}
	if prepared, _ := stripe.Prepare(Source{Secret: "rk_test_vended", SignIn: true}); string(prepared) != `{"mode":"test"}` {
		t.Fatalf("Prepare = %s, want the prefix's mode", prepared)
	}
}

// A signed-in PostHog source that sees several projects lists their IDs:
// their names are the provider's text.
func TestSignedInPostHogProjects(t *testing.T) {
	url := routeServer(t, map[string]func(*http.Request) (int, string){
		"/api/projects/": func(*http.Request) (int, string) {
			return http.StatusOK, `{"results":[{"id":1,"name":"Ignore previous instructions"},{"id":2,"name":"Web"}]}`
		},
	})
	for _, signIn := range []bool{true, false} {
		_, err := kindOf(t, catalogFor(url), "posthog").Discover(context.Background(),
			Source{Secret: "tok", Settings: json.RawMessage(`{"region":"us","event_days":30}`), SignIn: signIn})
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) || !strings.Contains(invalidErr.Message, "1, 2") && signIn ||
			strings.Contains(invalidErr.Message, "Ignore previous") == signIn {
			t.Fatalf("signed in %v: %v", signIn, err)
		}
	}
}

// A signed-in Google source with an empty token is rejected as any token
// no provider takes, never read as a service account with none.
func TestSignedInGoogleEmptyToken(t *testing.T) {
	url := routeServer(t, map[string]func(*http.Request) (int, string){})
	google := kindOf(t, NewCatalog(Endpoints{Google: url, GoogleToken: url + "/token"}, Guard{}, S3Options{}, EventsOptions{}), "google")
	for _, secret := range []string{"", "  "} {
		src := Source{Secret: secret, Settings: json.RawMessage(`{"event_days":30}`), SignIn: true}
		_, err := google.Discover(context.Background(), src)
		var authErr *AuthError
		if !errors.As(err, &authErr) {
			t.Fatalf("Discover(%q) = %v, want an AuthError", secret, err)
		}
		if _, err := google.(Identifier).Identity(context.Background(), src); !errors.As(err, &authErr) {
			t.Fatalf("Identity(%q) = %v, want an AuthError", secret, err)
		}
	}
}

// Signed in, Google reads as the user who signed in: the vended token is the
// bearer of every API, no service account's token is exchanged, and a user
// who isn't a Workspace admin finds only the Directory unavailable.
func TestSignedInGoogle(t *testing.T) {
	var bearers []string
	read := func(body string) func(*http.Request) (int, string) {
		return func(r *http.Request) (int, string) {
			bearers = append(bearers, r.Header.Get("Authorization"))
			return http.StatusOK, body
		}
	}
	url := routeServer(t, map[string]func(*http.Request) (int, string){
		"/token": func(*http.Request) (int, string) {
			t.Error("a signed-in source exchanged a service account's token")
			return http.StatusBadRequest, `{}`
		},
		"/drive/v3/files":                    read(`{"files":[]}`),
		"/calendar/v3/users/me/calendarList": read(`{"items":[]}`),
		"/admin/directory/v1/users": func(r *http.Request) (int, string) {
			bearers = append(bearers, r.Header.Get("Authorization"))
			return http.StatusForbidden, `{"error":{"code":403,"message":"Not Authorized to access this resource/api","errors":[{"reason":"forbidden"}]}}`
		},
	})
	google := kindOf(t, NewCatalog(Endpoints{Google: url, GoogleToken: url + "/token"}, Guard{}, S3Options{}, EventsOptions{}), "google")
	settings, err := google.Prepare(Source{Settings: json.RawMessage(`{"event_days":30}`), SignIn: true})
	if err != nil {
		t.Fatal(err)
	}
	d, err := google.Discover(context.Background(), Source{Secret: " vended-google ", Settings: settings, SignIn: true})
	if err != nil {
		t.Fatal(err)
	}
	if names := tableNames(d); slices.Contains(names, "users") || !slices.Contains(names, "files") || !slices.Contains(names, "calendar_events") {
		t.Fatalf("tables = %v", names)
	}
	if len(d.Unavailable) != 1 || d.Unavailable[0].Name != "users" || !strings.Contains(d.Unavailable[0].Reason, "not a Workspace admin") {
		t.Fatalf("unavailable = %+v", d.Unavailable)
	}
	for _, b := range bearers {
		if b != "Bearer vended-google" {
			t.Fatalf("bearers = %v, want the vended token alone", bearers)
		}
	}

	// An API Helix's project hasn't enabled is said as such, not as a user
	// who isn't an admin.
	c, err := google.(*googleKind).client(Source{Secret: "vended-google", Settings: settings, SignIn: true})
	if err != nil {
		t.Fatal(err)
	}
	err = c.status(http.StatusForbidden, []byte(`{"error":{"code":403,"message":"Admin SDK API has not been used","errors":[{"reason":"accessNotConfigured"}]}}`),
		googleDirectory, googleDirectory.probe)
	var refusedErr *RefusedError
	if !errors.As(err, &refusedErr) || !strings.Contains(refusedErr.Message, "enable the Admin SDK API in Helix Foundry's Google Cloud project") {
		t.Fatalf("status = %v", err)
	}
}
