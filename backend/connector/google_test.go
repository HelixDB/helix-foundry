package connector

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every DocumentKind is listed in documentKinds (TestStreamCatalog);
// Google's is listed here, beside its tests.
func init() {
	documentKinds["google"] = struct {
		settings json.RawMessage
		keys     map[string][]string
	}{settings: googleSettings, keys: map[string][]string{
		"users": {"user_id"}, "files": {"file_id"}, "calendars": {"calendar_id"},
		"calendar_events": {"event_id"}, "event_attendees": {"event_id", "email"}}}
}

const googleSubject = "ada@acme.com"

var googleSettings = json.RawMessage(`{"subject":"ada@acme.com","event_days":90}`)

// googleTestKey is a service account's JSON key, made once for the tests,
// and its private key, whose public half checks what the kind signs.
var googleTestKey = sync.OnceValues(func() (string, *rsa.PrivateKey) {
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		panic(err)
	}
	key, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "acme-foundry", "private_key_id": "5c1e0d9f8a7b6c5d4e3f1a2b",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "foundry@acme.iam.gserviceaccount.com", "client_id": "104593021840982370921",
		"token_uri": "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		panic(err)
	}
	return string(key), private
})

func googleSource() Source {
	key, _ := googleTestKey()
	return Source{Secret: key, Settings: googleSettings}
}

// googleWorkspace plays Google for a test. Its token endpoint checks each
// assertion's signature and claims, then answers tokens[scope] when set,
// else a token for the scope. Every API request must carry the token of
// its API's scope; routes answer them by path.
type googleWorkspace struct {
	tokens map[string]func() (int, string)
	routes map[string]func(*http.Request) (int, string)

	mu        sync.Mutex
	exchanged []string // the scopes of the token requests
	asked     []string // the API requests, as path?query
}

// googleAt serves w and returns the google kind reading from it, unpaced.
func googleAt(t *testing.T, w *googleWorkspace) *googleKind {
	t.Helper()
	saved := googleInterval
	googleInterval = 0
	t.Cleanup(func() { googleInterval = saved })
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		status, body := w.serve(t, r)
		rw.WriteHeader(status)
		_, _ = rw.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return kindOf(t, NewCatalog(Endpoints{Google: server.URL, GoogleToken: server.URL + "/token"}, Guard{}, S3Options{}, EventsOptions{}),
		"google").(*googleKind)
}

func (w *googleWorkspace) serve(t *testing.T, r *http.Request) (int, string) {
	if r.URL.Path == "/token" {
		return w.exchange(t, r, "http://"+r.Host+"/token")
	}
	scope := map[string]string{"admin": googleDirectory.scope, "drive": googleDrive.scope, "calendar": googleCalendar.scope}[strings.Split(r.URL.Path, "/")[1]]
	if got := r.Header.Get("Authorization"); got != "Bearer token for "+scope {
		t.Errorf("%s: Authorization = %q", r.URL.Path, got)
	}
	w.mu.Lock()
	w.asked = append(w.asked, r.URL.Path+"?"+r.URL.RawQuery)
	handler, ok := w.routes[r.URL.Path]
	w.mu.Unlock()
	if !ok {
		t.Errorf("unexpected request %s", r.URL)
		return http.StatusNotFound, `{}`
	}
	return handler(r)
}

// exchange checks a token request as Google does: a JWT bearer grant whose
// assertion the key signed, from the key's service account acting as the
// settings' user, for this endpoint.
func (w *googleWorkspace) exchange(t *testing.T, r *http.Request, tokenURL string) (int, string) {
	_, private := googleTestKey()
	if err := r.ParseForm(); err != nil || r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		t.Errorf("token request = %v, %v", r.PostForm, err)
	}
	parts := strings.Split(r.PostForm.Get("assertion"), ".")
	if len(parts) != 3 {
		t.Errorf("assertion = %q", r.PostForm.Get("assertion"))
		return http.StatusBadRequest, `{"error":"invalid_request"}`
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if err := rsa.VerifyPKCS1v15(&private.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Errorf("assertion signature: %v", err)
	}
	type header struct{ Alg, Typ, Kid string }
	var (
		h      header
		claims struct {
			Iss, Sub, Scope, Aud string
			Iat, Exp             int64
		}
	)
	for i, into := range []any{&h, &claims} {
		raw, _ := base64.RawURLEncoding.DecodeString(parts[i])
		if err := json.Unmarshal(raw, into); err != nil {
			t.Errorf("assertion part %d: %v", i, err)
		}
	}
	if h != (header{"RS256", "JWT", "5c1e0d9f8a7b6c5d4e3f1a2b"}) || claims.Iss != "foundry@acme.iam.gserviceaccount.com" ||
		claims.Sub != googleSubject || claims.Aud != tokenURL || claims.Exp-claims.Iat != 3600 {
		t.Errorf("assertion header = %+v, claims = %+v", h, claims)
	}
	w.mu.Lock()
	w.exchanged = append(w.exchanged, claims.Scope)
	answer := w.tokens[claims.Scope]
	w.mu.Unlock()
	if answer != nil {
		return answer()
	}
	return http.StatusOK, fmt.Sprintf(`{"access_token":%q,"token_type":"Bearer","expires_in":3599}`, "token for "+claims.Scope)
}

// requested are the API requests whose path?query starts with prefix.
func (w *googleWorkspace) requested(prefix string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(w.asked), func(asked string) bool { return !strings.HasPrefix(asked, prefix) })
}

// googleProbed are routes answering each API's probe with an empty page.
func googleProbed(t *testing.T) map[string]func(*http.Request) (int, string) {
	probe := func(api googleAPI, page string) func(*http.Request) (int, string) {
		return func(r *http.Request) (int, string) {
			if got := r.URL.Path + "?" + r.URL.RawQuery; got != api.probe {
				t.Errorf("probe %s, want %s", got, api.probe)
			}
			return http.StatusOK, page
		}
	}
	return map[string]func(*http.Request) (int, string){
		"/admin/directory/v1/users":          probe(googleDirectory, `{"kind":"admin#directory#users"}`),
		"/drive/v3/files":                    probe(googleDrive, `{"files":[]}`),
		"/calendar/v3/users/me/calendarList": probe(googleCalendar, `{"kind":"calendar#calendarList","items":[]}`),
	}
}

// googleFailure answers with a Google API's error of status, its reason in errors[].
func googleFailure(status int, message, reason string) func(*http.Request) (int, string) {
	return func(*http.Request) (int, string) {
		return status, fmt.Sprintf(`{"error":{"code":%d,"message":%q,"errors":[{"message":%q,"domain":"global","reason":%q}]}}`,
			status, message, message, reason)
	}
}

// refusalOf is err's message for the customer when it is a refusal.
func refusalOf(err error) string {
	var refusedErr *RefusedError
	if !errors.As(err, &refusedErr) {
		return ""
	}
	return refusedErr.Message
}

func TestGoogleConnector(t *testing.T) {
	ctx := context.Background()
	key, private := googleTestKey()
	kind := kindOf(t, catalogFor("http://unused"), "google")
	t.Run("rejects what is not a service account's JSON key", func(t *testing.T) {
		with := func(field, value string) string {
			var fields map[string]string
			_ = json.Unmarshal([]byte(key), &fields)
			fields[field] = value
			out, _ := json.Marshal(fields)
			return string(out)
		}
		ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		ecDER, _ := x509.MarshalPKCS8PrivateKey(ec)
		for name, secret := range map[string]string{
			"nothing": "", "text": "not a key", "a list": "[]", "null": "null",
			"another type of key":     with("type", "authorized_user"),
			"no service account":      with("client_email", ""),
			"no key id":               with("private_key_id", ""),
			"a private key not PEM":   with("private_key", "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC"),
			"a private key not RSA":   with("private_key", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}))),
			"a private key not a key": with("private_key", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")}))),
		} {
			_, err := kind.Prepare(Source{Secret: secret, Settings: googleSettings})
			var invalidErr *InvalidError
			if !errors.As(err, &invalidErr) || invalidErr.Message != "paste the service account's JSON key" {
				t.Errorf("%s: Prepare error = %v, want InvalidError", name, err)
			}
			if hint := kind.Hint(secret); hint != "…" {
				t.Errorf("%s: Hint = %q", name, hint)
			}
		}
		// A key Google never makes, but whose PKCS #1 RSA key the token exchange signs with.
		pkcs1 := with("private_key", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(private)})))
		if _, err := kind.Prepare(Source{Secret: pkcs1, Settings: googleSettings}); err != nil {
			t.Errorf("Prepare(PKCS #1) = %v", err)
		}
	})
	t.Run("stores the user in lower case, and 90 days of events unless chosen", func(t *testing.T) {
		for raw, want := range map[string]string{
			`{"subject":" Ada@Acme.COM "}`:                `{"subject":"ada@acme.com","event_days":90}`,
			`{"subject":"ada@acme.com","event_days":0}`:   `{"subject":"ada@acme.com","event_days":90}`,
			`{"subject":"ada@acme.com","event_days":1}`:   `{"subject":"ada@acme.com","event_days":1}`,
			`{"subject":"ada@acme.com","event_days":365}`: `{"subject":"ada@acme.com","event_days":365}`,
		} {
			settings, err := kind.Prepare(Source{Secret: " " + key + "\n", Settings: json.RawMessage(raw)})
			if err != nil || string(settings) != want {
				t.Errorf("Prepare(%s) = (%s, %v), want %s", raw, settings, err, want)
			}
		}
	})
	t.Run("rejects a user that is not one email", func(t *testing.T) {
		for _, subject := range []string{"", "ada", "ada@", "@acme.com", "Ada <ada@acme.com>", "ada@acme.com, grace@acme.com",
			"ada@acme.com@acme.com", `"ada@home"@acme.com`} {
			raw, _ := json.Marshal(GoogleSettings{Subject: subject})
			_, err := kind.Prepare(Source{Secret: key, Settings: raw})
			var invalidErr *InvalidError
			if !errors.As(err, &invalidErr) || invalidErr.Message != "enter the email of the Workspace user Foundry acts as" {
				t.Errorf("Prepare(%s) error = %v, want InvalidError", raw, err)
			}
		}
		if _, err := kind.Prepare(Source{Secret: key}); err == nil {
			t.Error("Prepare took no settings, so no user")
		}
	})
	t.Run("keeps 1 to 365 days of past events", func(t *testing.T) {
		for _, days := range []int{-1, 366} {
			raw, _ := json.Marshal(GoogleSettings{Subject: googleSubject, EventDays: days})
			_, err := kind.Prepare(Source{Secret: key, Settings: raw})
			var invalidErr *InvalidError
			if !errors.As(err, &invalidErr) || invalidErr.Message != "keep 1 to 365 days of past events" {
				t.Errorf("Prepare(%s) error = %v, want InvalidError", raw, err)
			}
		}
	})
	t.Run("takes only its key, the user and the days of events", func(t *testing.T) {
		for _, raw := range []string{`{"subject":"ada@acme.com","tables":["users"]}`, `{"subject":"ada@acme.com","event_days":"30"}`, `[]`} {
			_, err := kind.Prepare(Source{Secret: key, Settings: json.RawMessage(raw)})
			var invalidErr *InvalidError
			if !errors.As(err, &invalidErr) ||
				invalidErr.Message != "this connector takes only its key, the user it acts as and how many days of past events to keep" {
				t.Errorf("Prepare(%s) error = %v, want InvalidError", raw, err)
			}
		}
	})
	t.Run("hints at a key by its service account and key id", func(t *testing.T) {
		if hint := kind.Hint(key); hint != "foundry@acme.iam.gserviceaccount.com…1a2b" || kind.DisplayName() != "Google Workspace" {
			t.Fatalf("Hint = %q, DisplayName = %q", hint, kind.DisplayName())
		}
	})
	t.Run("keeps every Doc in the docs set of the files table", func(t *testing.T) {
		streams := kind.(DocumentKind).Streams(Source{Settings: googleSettings})
		if len(streams) != 1 || streams[0].Name != "docs" || streams[0].Table != "files" || streams[0].Title != "Docs" ||
			streams[0].Format != Markdown || streams[0].Keep != (Keep{}) || streams[0].Problem() != "" {
			t.Fatalf("streams = %+v", streams)
		}
	})

	t.Run("lists every object, with keys, foreign keys and windows", func(t *testing.T) {
		w := &googleWorkspace{routes: googleProbed(t)}
		d, err := googleAt(t, w).Discover(ctx, Source{Secret: key, Settings: googleSettings, ForSync: true})
		if err != nil {
			t.Fatal(err)
		}
		if names := tableNames(d); !slices.Equal(names, []string{"users", "files", "calendars", "calendar_events", "event_attendees"}) || len(d.Unavailable) != 0 {
			t.Fatalf("tables = %v, unavailable = %v", names, d.Unavailable)
		}
		want := map[string]Table{
			"users":     {Name: "users", PrimaryKey: []string{"user_id"}},
			"files":     {Name: "files", PrimaryKey: []string{"file_id"}},
			"calendars": {Name: "calendars", PrimaryKey: []string{"calendar_id"}},
			"calendar_events": {Name: "calendar_events", PrimaryKey: []string{"event_id"}, Windowed: true,
				ForeignKeys: []ForeignKey{{Column: "calendar_id", ReferencedTable: "calendars", ReferencedColumn: "calendar_id"}}},
			"event_attendees": {Name: "event_attendees", PrimaryKey: []string{"event_id", "email"}, Windowed: true,
				ForeignKeys: []ForeignKey{{Column: "event_id", ReferencedTable: "calendar_events", ReferencedColumn: "event_id"}}},
		}
		for name, table := range want {
			if got := findTable(t, d, name); fmt.Sprint(got) != fmt.Sprint(table) {
				t.Errorf("%s = %+v, want %+v", name, got, table)
			}
		}
		// One token per scope, each exchanged once.
		scopes := []string{googleDirectory.scope, googleCalendar.scope, googleDrive.scope}
		if got := slices.Sorted(slices.Values(w.exchanged)); !slices.Equal(got, scopes) {
			t.Fatalf("exchanged = %v", got)
		}
	})
	// Google answers a scope the client isn't delegated with unauthorized_client.
	for _, tc := range []struct {
		api    googleAPI
		tables []string
	}{
		{googleDirectory, []string{"users"}},
		{googleDrive, []string{"files"}},
		{googleCalendar, []string{"calendars", "calendar_events", "event_attendees"}},
	} {
		t.Run("makes the tables of a scope the admin didn't delegate unavailable: "+tc.api.name, func(t *testing.T) {
			w := &googleWorkspace{routes: googleProbed(t), tokens: map[string]func() (int, string){tc.api.scope: func() (int, string) {
				return http.StatusUnauthorized, `{"error":"unauthorized_client","error_description":"Client is unauthorized to retrieve access tokens ` +
					`using this method, or client not authorized for any of the scopes requested."}`
			}}}
			d, err := googleAt(t, w).Discover(ctx, googleSource())
			if err != nil {
				t.Fatal(err)
			}
			reason := "allow the scope " + tc.api.scope + " for the service account's client ID in the Admin console's domain-wide delegation"
			var want []Unavailable
			for _, name := range tc.tables {
				want = append(want, Unavailable{Name: name, Reason: reason})
			}
			if !slices.Equal(d.Unavailable, want) || len(d.Tables) != 5-len(tc.tables) {
				t.Fatalf("tables = %v, unavailable = %v", tableNames(d), d.Unavailable)
			}
			if probes := w.requested(strings.Split(tc.api.probe, "?")[0]); len(probes) != 0 {
				t.Fatalf("probed %v without a token", probes)
			}
		})
	}
	// Older APIs give the reason in errors[], newer ones in details[].
	t.Run("makes an API the project hasn't enabled unavailable", func(t *testing.T) {
		routes := googleProbed(t)
		routes["/drive/v3/files"] = googleFailure(http.StatusForbidden,
			"Google Drive API has not been used in project 1234 before or it is disabled.", "accessNotConfigured")
		routes["/calendar/v3/users/me/calendarList"] = func(*http.Request) (int, string) {
			return http.StatusForbidden, `{"error":{"code":403,"message":"Google Calendar API has not been used in project 1234 before or it is disabled.",
				"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SERVICE_DISABLED",
				"domain":"googleapis.com"}]}}`
		}
		d, err := googleAt(t, &googleWorkspace{routes: routes}).Discover(ctx, googleSource())
		if err != nil {
			t.Fatal(err)
		}
		calendar := "enable the Google Calendar API in the service account's Google Cloud project"
		want := []Unavailable{{"files", "enable the Drive API in the service account's Google Cloud project"},
			{"calendars", calendar}, {"calendar_events", calendar}, {"event_attendees", calendar}}
		if !slices.Equal(d.Unavailable, want) || !slices.Equal(tableNames(d), []string{"users"}) {
			t.Fatalf("tables = %v, unavailable = %v", tableNames(d), d.Unavailable)
		}
	})
	t.Run("says a user who is not an admin can't read the Directory's users", func(t *testing.T) {
		routes := googleProbed(t)
		routes["/admin/directory/v1/users"] = googleFailure(http.StatusForbidden, "Not Authorized to access this resource/api", "forbidden")
		d, err := googleAt(t, &googleWorkspace{routes: routes}).Discover(ctx, googleSource())
		if err != nil {
			t.Fatal(err)
		}
		if want := []Unavailable{{"users", "ada@acme.com is not an admin, so the Directory's users can't be read"}}; !slices.Equal(d.Unavailable, want) {
			t.Fatalf("unavailable = %v", d.Unavailable)
		}
	})
	// Only the Directory's words mean the user is no admin; a 403 or 404 otherwise is no reason Google gives.
	t.Run("reports what the user can't read without a reason", func(t *testing.T) {
		routes := googleProbed(t)
		routes["/admin/directory/v1/users"] = googleFailure(http.StatusForbidden, "Forbidden", "forbidden")
		routes["/drive/v3/files"] = googleFailure(http.StatusForbidden, "Not Authorized to access this resource/api", "forbidden")
		// Google leaves an empty list out.
		routes["/calendar/v3/users/me/calendarList"] = func(*http.Request) (int, string) { return http.StatusOK, `{}` }
		d, err := googleAt(t, &googleWorkspace{routes: routes}).Discover(ctx, googleSource())
		if err != nil {
			t.Fatal(err)
		}
		if want := []Unavailable{{Name: "users"}, {Name: "files"}}; !slices.Equal(d.Unavailable, want) {
			t.Fatalf("unavailable = %v", d.Unavailable)
		}
		routes["/calendar/v3/users/me/calendarList"] = googleFailure(http.StatusNotFound, "Not Found", "notFound")
		_, err = googleAt(t, &googleWorkspace{routes: routes}).Discover(ctx, googleSource())
		if want := "the service account cannot read Drive, Calendar or the Directory as ada@acme.com; " +
			"check its domain-wide delegation and test again"; refusalOf(err) != want {
			t.Fatalf("Discover error = %v, want a refusal", err)
		}
	})
	t.Run("refuses when no scope is delegated", func(t *testing.T) {
		notDelegated := func() (int, string) { return http.StatusUnauthorized, `{"error":"unauthorized_client"}` }
		w := &googleWorkspace{routes: googleProbed(t), tokens: map[string]func() (int, string){
			googleDirectory.scope: notDelegated, googleDrive.scope: notDelegated, googleCalendar.scope: notDelegated,
		}}
		_, err := googleAt(t, w).Discover(ctx, googleSource())
		if !strings.HasPrefix(refusalOf(err), "the service account cannot read Drive, Calendar or the Directory as ada@acme.com") ||
			len(w.requested("/")) != 0 {
			t.Fatalf("Discover error = %v after %v, want a refusal", err, w.requested("/"))
		}
	})
	// What the token endpoint says of the user or the key is the same for every scope.
	for name, tc := range map[string]struct {
		status        int
		answer, words string
	}{
		"an unknown user": {http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Invalid email or User ID"}`,
			"Google has no user ada@acme.com, or the service account may not act as them; check the email"},
		"a user the grant names": {http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Not a valid user: Ada@Acme.com"}`,
			"Google has no user ada@acme.com, or the service account may not act as them; check the email"},
		"a deleted key": {http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Invalid JWT Signature."}`,
			"Google rejected this service account key; check it and paste it again"},
		"a deleted service account": {http.StatusUnauthorized, `{"error":"invalid_client","error_description":"The OAuth client was not found."}`,
			"Google rejected this service account key; check it and paste it again"},
		"another refusal": {http.StatusBadRequest, `{"error":"access_denied","error_description":"Requested client not authorized."}`,
			"Google refused the service account a token (access_denied); check its key and domain-wide delegation"},
		"a refusal that is not JSON": {http.StatusBadRequest, `<html>Bad request</html>`,
			"Google refused the service account a token (400 Bad Request); check its key and domain-wide delegation"},
		"an outage":    {http.StatusServiceUnavailable, `{}`, "could not reach Google; try again"},
		"a rate limit": {http.StatusTooManyRequests, `{"error":"rate_limit_exceeded"}`, "could not reach Google; try again"},
	} {
		t.Run("refuses "+name+" at the token endpoint", func(t *testing.T) {
			answer := func() (int, string) { return tc.status, tc.answer }
			w := &googleWorkspace{routes: googleProbed(t), tokens: map[string]func() (int, string){
				googleDirectory.scope: answer, googleDrive.scope: answer, googleCalendar.scope: answer,
			}}
			if _, err := googleAt(t, w).Discover(ctx, googleSource()); refusalOf(err) != tc.words {
				t.Fatalf("Discover error = %v, want %q", err, tc.words)
			}
		})
	}
	t.Run("refuses when Google's token endpoint can't be reached", func(t *testing.T) {
		closed := httptest.NewServer(http.NotFoundHandler())
		closed.Close()
		kind := googleAt(t, &googleWorkspace{routes: googleProbed(t)})
		kind.tokenURL = closed.URL
		if _, err := kind.Discover(ctx, googleSource()); refusalOf(err) != "could not reach Google; try again" {
			t.Fatalf("Discover error = %v, want a refusal", err)
		}
	})
	// A rate limit or an outage says nothing about the API, so it must not
	// make its tables unavailable: a sync would then fail them for good.
	for name, tc := range map[string]struct {
		answer func(*http.Request) (int, string)
		words  string
	}{
		"a rejected key": {func(*http.Request) (int, string) { return http.StatusUnauthorized, `{}` },
			"Google rejected this service account key; check it and paste it again"},
		"429 after retries": {func(*http.Request) (int, string) { return http.StatusTooManyRequests, `{}` },
			"this service account has used up its Google rate limit; try again in a few minutes"},
		"a rate limit Drive answers 403": {googleFailure(http.StatusForbidden, "User Rate Limit Exceeded", "userRateLimitExceeded"),
			"this service account has used up its Google rate limit; try again in a few minutes"},
		"502 after retries":  {func(*http.Request) (int, string) { return http.StatusBadGateway, `` }, "could not reach Google; try again"},
		"another error code": {googleFailure(http.StatusBadRequest, "Invalid Value", "invalid"), "Google returned HTTP 400 for /drive/v3/files"},
	} {
		t.Run("refuses a probe answered with "+name, func(t *testing.T) {
			noBackoff(t)
			routes := googleProbed(t)
			routes["/drive/v3/files"] = tc.answer
			if _, err := googleAt(t, &googleWorkspace{routes: routes}).Discover(ctx, googleSource()); refusalOf(err) != tc.words {
				t.Fatalf("Discover error = %v, want %q", err, tc.words)
			}
		})
	}
	t.Run("refuses settings and keys Prepare never stores", func(t *testing.T) {
		kind := googleAt(t, &googleWorkspace{})
		var invalidErr *InvalidError
		if _, err := kind.Discover(ctx, Source{Secret: "{}", Settings: googleSettings}); !errors.As(err, &invalidErr) ||
			invalidErr.Message != "paste the service account's JSON key" {
			t.Fatalf("Discover error = %v, want InvalidError", err)
		}
		if _, err := kind.Discover(ctx, Source{Secret: key, Settings: json.RawMessage(`[]`)}); !errors.As(err, &invalidErr) ||
			invalidErr.Message != "Google Workspace settings are malformed" {
			t.Fatalf("Discover error = %v, want InvalidError", err)
		}
	})
}

// googleUsers are two pages of the Directory's users: Ada's on both, as a
// list that changes while it is paged may show, and Alan's.
var googleUsers = map[string]string{
	"": `{"kind":"admin#directory#users","users":[{"kind":"admin#directory#user","id":"1001","primaryEmail":"Ada@Acme.com",
		"name":{"givenName":"Ada","familyName":"Lovelace","fullName":"Ada Lovelace"},"isAdmin":true,"suspended":false,"archived":false,
		"orgUnitPath":"/Engineering","organizations":[{"title":"CTO","department":"Engineering","primary":true},{"title":"Advisor"}],
		"relations":[{"value":"board@acme.com","type":"partner"},{"value":"Grace@acme.com","type":"manager"},{"value":"other@acme.com","type":"manager"}],
		"creationTime":"2020-01-02T03:04:05.000Z","lastLoginTime":"2026-10-28T09:00:00.000Z"}],"nextPageToken":"u1"}`,
	"u1": `{"users":[{"id":"1001","primaryEmail":"ada@acme.com"},{"id":"1002","primaryEmail":"alan@acme.com","name":{"fullName":"Alan Turing"},
		"suspended":true,"archived":true,"creationTime":"2021-05-06T07:08:09+02:00","lastLoginTime":"1970-01-01T00:00:00.000Z"}]}`,
}

// googleFiles are a page of Drive's files: a Doc, a folder, a shared
// drive's PDF and a Sheet.
const googleFiles = `{"files":[
	{"id":"doc1","name":"Q3  plan","mimeType":"application/vnd.google-apps.document","parents":["fold1"],"owners":[{"emailAddress":"Ada@acme.com"}],
		"lastModifyingUser":{"emailAddress":"grace@acme.com"},"createdTime":"2026-09-01T10:00:00.000Z","modifiedTime":"2026-10-20T15:30:00.250Z",
		"webViewLink":"https://docs.google.com/document/d/doc1/edit","shared":true,"starred":false,"version":"42"},
	{"id":"fold1","name":"Plans","mimeType":"application/vnd.google-apps.folder","owners":[{"emailAddress":"ada@acme.com"}],
		"createdTime":"2026-09-01T09:00:00Z","modifiedTime":"2026-09-01T09:00:00Z","webViewLink":"https://drive.google.com/drive/folders/fold1",
		"starred":true,"version":"1"},
	{"id":"pdf1","name":"contract.pdf","mimeType":"application/pdf","driveId":"drv1","parents":["drv1"],
		"lastModifyingUser":{"emailAddress":"alan@acme.com"},"createdTime":"2026-08-01T10:00:00+01:00","modifiedTime":"2026-08-02T10:00:00Z",
		"size":"52341","webViewLink":"https://drive.google.com/file/d/pdf1/view","shared":false,"version":"3"},
	{"id":"sheet1","name":"Budget","mimeType":"application/vnd.google-apps.spreadsheet","owners":[{"emailAddress":"ada@acme.com"}],
		"createdTime":"2026-09-02T10:00:00Z","modifiedTime":"2026-10-27T10:00:00Z","webViewLink":"https://docs.google.com/spreadsheets/d/sheet1/edit",
		"version":"9"}],"incompleteSearch":false}`

// googleCalendars is the calendar list: Ada's own, a holiday calendar whose
// id needs escaping, Grace's, of which she sees only the free/busy, and
// one she unsubscribed from since.
const googleCalendars = `{"kind":"calendar#calendarList","items":[
	{"id":"ada@acme.com","summary":"Ada Lovelace","timeZone":"Europe/London","accessRole":"owner","primary":true},
	{"id":"en.uk#holiday@group.v.calendar.google.com","summary":"Holidays in the UK","description":"Public holidays",
		"timeZone":"Europe/London","accessRole":"reader"},
	{"id":"grace@acme.com","summary":"Grace Hopper","accessRole":"freeBusyReader"},
	{"id":"gone@acme.com","summary":"Gone","accessRole":"reader"}]}`

// googleEvents are each calendar's events by page token: Ada's in two pages.
var googleEvents = map[string]map[string]string{
	"ada@acme.com": {
		"": `{"items":[{"id":"ev1","status":"confirmed","htmlLink":"https://www.google.com/calendar/event?eid=ev1","created":"2026-10-01T09:00:00.000Z",
			"updated":"2026-10-02T09:00:00.000Z","summary":"Roadmap","description":"Q4 plans","location":"Room 1","iCalUID":"ev1@google.com",
			"creator":{"email":"Ada@acme.com"},"organizer":{"email":"ada@acme.com","self":true},
			"start":{"dateTime":"2026-10-30T10:00:00+01:00","timeZone":"Europe/London"},"end":{"dateTime":"2026-10-30T11:00:00+01:00"},
			"attendees":[{"email":"ada@acme.com","displayName":"Ada Lovelace","organizer":true,"self":true,"responseStatus":"accepted"},
				{"email":"Grace@Acme.com","optional":true,"responseStatus":"needsAction"},
				{"email":"room-1@resource.calendar.google.com","displayName":"Room 1","resource":true,"responseStatus":"accepted"},
				{"email":"ADA@acme.com","responseStatus":"declined"},{"displayName":"No email"}]}],"nextPageToken":"e1"}`,
		"e1": `{"items":[{"id":"ev2","status":"cancelled"},{"id":"ev3_20261101T090000Z","status":"tentative","recurringEventId":"ev3",
			"iCalUID":"ev3@google.com","organizer":{"email":"grace@acme.com"},"start":{"dateTime":"2026-11-01T09:00:00Z"},
			"end":{"dateTime":"2026-11-01T09:30:00Z"}}]}`,
	},
	"en.uk#holiday@group.v.calendar.google.com": {
		"": `{"items":[{"id":"hol1","status":"confirmed","summary":"Christmas Day","start":{"date":"2026-12-25"},"end":{"date":"2026-12-26"},
			"organizer":{"email":"en.uk#holiday@group.v.calendar.google.com","displayName":"Holidays in the UK"}}]}`,
	},
}

// googleRoutes serves googleUsers, googleFiles, googleCalendars and
// googleEvents, checking each list's query; events must ask for days of
// the past. A calendar id's # unescaped would cut its events' path short.
func googleRoutes(t *testing.T, days int) map[string]func(*http.Request) (int, string) {
	routes := map[string]func(*http.Request) (int, string){
		"/admin/directory/v1/users": func(r *http.Request) (int, string) {
			if q := r.URL.Query(); q.Get("customer") != "my_customer" || q.Get("maxResults") != "500" {
				t.Errorf("users query = %s", r.URL.RawQuery)
			}
			return http.StatusOK, googleUsers[r.URL.Query().Get("pageToken")]
		},
		"/drive/v3/files": func(r *http.Request) (int, string) {
			if q := r.URL.Query(); q.Get("q") != "trashed=false" || q.Get("corpora") != "allDrives" || q.Get("includeItemsFromAllDrives") != "true" ||
				q.Get("supportsAllDrives") != "true" || q.Get("pageSize") != "1000" || q.Get("fields") != googleFileFields {
				t.Errorf("files query = %s", r.URL.RawQuery)
			}
			return http.StatusOK, googleFiles
		},
		"/calendar/v3/users/me/calendarList": func(r *http.Request) (int, string) {
			if r.URL.Query().Get("maxResults") != "250" {
				t.Errorf("calendar list query = %s", r.URL.RawQuery)
			}
			return http.StatusOK, googleCalendars
		},
		"/calendar/v3/calendars/grace@acme.com/events": googleFailure(http.StatusForbidden, "Forbidden", "forbidden"),
		"/calendar/v3/calendars/gone@acme.com/events":  googleFailure(http.StatusNotFound, "Not Found", "notFound"),
	}
	for calendar, pages := range googleEvents {
		routes["/calendar/v3/calendars/"+calendar+"/events"] = func(r *http.Request) (int, string) {
			q := r.URL.Query()
			from, errFrom := time.Parse(time.RFC3339, q.Get("timeMin"))
			to, errTo := time.Parse(time.RFC3339, q.Get("timeMax"))
			now := time.Now().UTC()
			if errFrom != nil || errTo != nil || now.AddDate(0, 0, -days).Sub(from).Abs() > time.Minute || now.AddDate(0, 0, 30).Sub(to).Abs() > time.Minute ||
				q.Get("singleEvents") != "true" || q.Get("showDeleted") != "false" || q.Get("maxResults") != "2500" {
				t.Errorf("events query = %s", r.URL.RawQuery)
			}
			return http.StatusOK, pages[q.Get("pageToken")]
		}
	}
	return routes
}

func TestGoogleSync(t *testing.T) {
	ctx := context.Background()
	read := func(t *testing.T, kind *googleKind, src Source, table string) ([]map[string]any, error) {
		t.Helper()
		sink := &rowSink{}
		_, err := kind.Sync(ctx, src, Table{Name: table}, sink)
		return sink.rows, err
	}
	// rowsOf reads table from googleRoutes, failing the test on an error.
	rowsOf := func(t *testing.T, table string) []map[string]any {
		t.Helper()
		rows, err := read(t, googleAt(t, &googleWorkspace{routes: googleRoutes(t, 90)}), googleSource(), table)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}

	// Pages follow pageToken, and a user listed twice is written once.
	t.Run("pages the Directory's users and writes each once", func(t *testing.T) {
		want := []map[string]any{
			{"user_id": "1001", "primary_email": "ada@acme.com", "full_name": "Ada Lovelace", "given_name": "Ada", "family_name": "Lovelace",
				"is_admin": true, "suspended": false, "archived": false, "org_unit_path": "/Engineering", "title": "CTO", "department": "Engineering",
				"manager_email": "grace@acme.com", "created_at": "2020-01-02T03:04:05Z", "last_login_at": "2026-10-28T09:00:00Z"},
			// Google's never-signed-in time is no time.
			{"user_id": "1002", "primary_email": "alan@acme.com", "full_name": "Alan Turing", "given_name": nil, "family_name": nil,
				"is_admin": false, "suspended": true, "archived": true, "org_unit_path": nil, "title": nil, "department": nil,
				"manager_email": nil, "created_at": "2021-05-06T05:08:09Z", "last_login_at": nil},
		}
		if rows := rowsOf(t, "users"); fmt.Sprint(rows) != fmt.Sprint(want) {
			t.Fatalf("rows = %v", rows)
		}
	})
	t.Run("lists every file, folders and shared drives' included", func(t *testing.T) {
		want := []map[string]any{
			{"file_id": "doc1", "name": "Q3  plan", "mime_type": "application/vnd.google-apps.document", "drive_id": nil, "parent_id": "fold1",
				"owner_email": "ada@acme.com", "last_modifying_user_email": "grace@acme.com", "created_at": "2026-09-01T10:00:00Z",
				"modified_at": "2026-10-20T15:30:00Z", "size": nil, "web_view_link": "https://docs.google.com/document/d/doc1/edit", "shared": true, "starred": false},
			{"file_id": "fold1", "name": "Plans", "mime_type": "application/vnd.google-apps.folder", "drive_id": nil, "parent_id": nil,
				"owner_email": "ada@acme.com", "last_modifying_user_email": nil, "created_at": "2026-09-01T09:00:00Z", "modified_at": "2026-09-01T09:00:00Z",
				"size": nil, "web_view_link": "https://drive.google.com/drive/folders/fold1", "shared": false, "starred": true},
			// A shared drive's file has no owner.
			{"file_id": "pdf1", "name": "contract.pdf", "mime_type": "application/pdf", "drive_id": "drv1", "parent_id": "drv1",
				"owner_email": nil, "last_modifying_user_email": "alan@acme.com", "created_at": "2026-08-01T09:00:00Z", "modified_at": "2026-08-02T10:00:00Z",
				"size": "52341", "web_view_link": "https://drive.google.com/file/d/pdf1/view", "shared": false, "starred": false},
			{"file_id": "sheet1", "name": "Budget", "mime_type": "application/vnd.google-apps.spreadsheet", "drive_id": nil, "parent_id": nil,
				"owner_email": "ada@acme.com", "last_modifying_user_email": nil, "created_at": "2026-09-02T10:00:00Z", "modified_at": "2026-10-27T10:00:00Z",
				"size": nil, "web_view_link": "https://docs.google.com/spreadsheets/d/sheet1/edit", "shared": false, "starred": false},
		}
		if rows := rowsOf(t, "files"); fmt.Sprint(rows) != fmt.Sprint(want) {
			t.Fatalf("rows = %v", rows)
		}
	})
	t.Run("lists the user's calendars", func(t *testing.T) {
		want := []map[string]any{
			{"calendar_id": "ada@acme.com", "summary": "Ada Lovelace", "description": nil, "time_zone": "Europe/London", "access_role": "owner", "primary": true},
			{"calendar_id": "en.uk#holiday@group.v.calendar.google.com", "summary": "Holidays in the UK", "description": "Public holidays",
				"time_zone": "Europe/London", "access_role": "reader", "primary": false},
			{"calendar_id": "grace@acme.com", "summary": "Grace Hopper", "description": nil, "time_zone": nil, "access_role": "freeBusyReader", "primary": false},
			{"calendar_id": "gone@acme.com", "summary": "Gone", "description": nil, "time_zone": nil, "access_role": "reader", "primary": false},
		}
		w := &googleWorkspace{routes: googleRoutes(t, 90)}
		rows, err := read(t, googleAt(t, w), googleSource(), "calendars")
		if err != nil || fmt.Sprint(rows) != fmt.Sprint(want) {
			t.Fatalf("Sync = (%v, %v)", rows, err)
		}
		if events := w.requested("/calendar/v3/calendars/"); len(events) != 0 {
			t.Fatalf("listed events %v for the calendars table", events)
		}
	})
	// Cancelled events are left out; a calendar that won't list its events is skipped.
	t.Run("lists each calendar's events in the window", func(t *testing.T) {
		want := []map[string]any{
			{"event_id": "ada@acme.com:ev1", "calendar_id": "ada@acme.com", "ical_uid": "ev1@google.com", "summary": "Roadmap",
				"description": "Q4 plans", "location": "Room 1", "status": "confirmed", "start_at": "2026-10-30T09:00:00Z", "end_at": "2026-10-30T10:00:00Z",
				"all_day": false, "organizer_email": "ada@acme.com", "creator_email": "ada@acme.com", "recurring_event_id": nil,
				"html_link": "https://www.google.com/calendar/event?eid=ev1", "created_at": "2026-10-01T09:00:00Z", "updated_at": "2026-10-02T09:00:00Z",
				"attendee_count": 2},
			{"event_id": "ada@acme.com:ev3_20261101T090000Z", "calendar_id": "ada@acme.com", "ical_uid": "ev3@google.com", "summary": nil,
				"description": nil, "location": nil, "status": "tentative", "start_at": "2026-11-01T09:00:00Z", "end_at": "2026-11-01T09:30:00Z",
				"all_day": false, "organizer_email": "grace@acme.com", "creator_email": nil, "recurring_event_id": "ev3",
				"html_link": nil, "created_at": nil, "updated_at": nil, "attendee_count": 0},
			// An all-day event keeps its dates.
			{"event_id": "en.uk#holiday@group.v.calendar.google.com:hol1", "calendar_id": "en.uk#holiday@group.v.calendar.google.com", "ical_uid": nil,
				"summary": "Christmas Day", "description": nil, "location": nil, "status": "confirmed", "start_at": "2026-12-25", "end_at": "2026-12-26",
				"all_day": true, "organizer_email": "en.uk#holiday@group.v.calendar.google.com", "creator_email": nil, "recurring_event_id": nil,
				"html_link": nil, "created_at": nil, "updated_at": nil, "attendee_count": 0},
		}
		w := &googleWorkspace{routes: googleRoutes(t, 90)}
		rows, err := read(t, googleAt(t, w), googleSource(), "calendar_events")
		if err != nil {
			t.Fatal(err)
		}
		slices.SortFunc(rows, func(a, b map[string]any) int { return strings.Compare(a["event_id"].(string), b["event_id"].(string)) })
		if fmt.Sprint(rows) != fmt.Sprint(want) {
			t.Fatalf("rows = %v", rows)
		}
		if listed := w.requested("/calendar/v3/calendars/"); len(listed) != 5 {
			t.Fatalf("event lists = %v; want each calendar's, Ada's in two pages", listed)
		}
	})
	t.Run("follows the settings' days of past events", func(t *testing.T) {
		kind := googleAt(t, &googleWorkspace{routes: googleRoutes(t, 7)})
		src := googleSource()
		src.Settings = json.RawMessage(`{"subject":"ada@acme.com","event_days":7}`)
		if _, err := read(t, kind, src, "calendar_events"); err != nil {
			t.Fatal(err)
		}
	})
	// Rooms and other resources are no attendees, and a person invited twice is one row.
	t.Run("writes each person an event invites once by email", func(t *testing.T) {
		want := []map[string]any{
			{"event_id": "ada@acme.com:ev1", "email": "ada@acme.com", "display_name": "Ada Lovelace", "response_status": "accepted",
				"optional": false, "organizer": true, "is_self": true},
			{"event_id": "ada@acme.com:ev1", "email": "grace@acme.com", "display_name": nil, "response_status": "needsAction",
				"optional": true, "organizer": false, "is_self": false},
		}
		if rows := rowsOf(t, "event_attendees"); fmt.Sprint(rows) != fmt.Sprint(want) {
			t.Fatalf("rows = %v", rows)
		}
	})
	// Ending early on an incomplete page would replace the snapshot with part of the table.
	for name, page := range map[string]string{
		"is not JSON":                        `<html>Bad gateway</html>`,
		"is null":                            `null`,
		"has a list that is not one":         `{"users":{"id":"1001"}}`,
		"lists a user that is not an object": `{"users":["1001"]}`,
		"lists a user without an id":         `{"users":[{"primaryEmail":"ada@acme.com"}]}`,
		"lists null":                         `{"users":[null]}`,
		"has a token that is not a string":   `{"users":[],"nextPageToken":7}`,
		"gives back the token it was asked":  `{"users":[],"nextPageToken":"u1"}`,
		"has an incompleteSearch not a flag": `{"users":[],"incompleteSearch":"yes"}`,
	} {
		t.Run("fails a page that "+name, func(t *testing.T) {
			kind := googleAt(t, &googleWorkspace{routes: map[string]func(*http.Request) (int, string){
				"/admin/directory/v1/users": func(*http.Request) (int, string) { return http.StatusOK, page },
			}})
			if rows, err := read(t, kind, googleSource(), "users"); err == nil || len(rows) != 0 {
				t.Fatalf("Sync = (%v, %v); want a failure before any row", rows, err)
			}
		})
	}
	// Drive may search only some of the shared drives a user is in.
	t.Run("refuses a listing Drive says is incomplete", func(t *testing.T) {
		kind := googleAt(t, &googleWorkspace{routes: map[string]func(*http.Request) (int, string){
			"/drive/v3/files": func(*http.Request) (int, string) { return http.StatusOK, `{"files":[],"incompleteSearch":true}` },
		}})
		_, err := read(t, kind, googleSource(), "files")
		if refusalOf(err) != "Drive searched only part of what ada@acme.com can read; try again later" {
			t.Fatalf("Sync error = %v, want a refusal", err)
		}
	})
	t.Run("stops a list that never ends", func(t *testing.T) {
		var mu sync.Mutex
		pages := 0
		kind := googleAt(t, &googleWorkspace{routes: map[string]func(*http.Request) (int, string){
			"/admin/directory/v1/users": func(*http.Request) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				pages++
				return http.StatusOK, fmt.Sprintf(`{"users":[],"nextPageToken":"p%d"}`, pages)
			},
		}})
		_, err := read(t, kind, googleSource(), "users")
		mu.Lock()
		defer mu.Unlock()
		if err == nil || !strings.Contains(err.Error(), "more than 10000 pages") || pages != maxGooglePages {
			t.Fatalf("Sync error = %v after %d pages", err, pages)
		}
	})
	t.Run("refuses a list the user can no longer read", func(t *testing.T) {
		kind := googleAt(t, &googleWorkspace{routes: map[string]func(*http.Request) (int, string){
			"/admin/directory/v1/users": googleFailure(http.StatusForbidden, "Forbidden", "forbidden"),
		}})
		_, err := read(t, kind, googleSource(), "users")
		if want := "this service account cannot read /admin/directory/v1/users as ada@acme.com; check its access and test again"; refusalOf(err) != want {
			t.Fatalf("Sync error = %v, want %q", err, want)
		}
	})
	// Only a calendar's own refusal skips it: a disabled API, a revoked key
	// or an outage fails the read rather than leaving events out.
	for name, tc := range map[string]struct {
		calendars, events func(*http.Request) (int, string)
		words             string
	}{
		"the calendar list failing": {calendars: func(*http.Request) (int, string) { return http.StatusInternalServerError, `{}` },
			words: "could not reach Google; try again"},
		"events of a disabled API": {events: googleFailure(http.StatusForbidden, "Calendar API is disabled", "accessNotConfigured"),
			words: "enable the Google Calendar API in the service account's Google Cloud project"},
		"events refused the key": {events: func(*http.Request) (int, string) { return http.StatusUnauthorized, `{}` },
			words: "Google rejected this service account key; check it and paste it again"},
	} {
		t.Run("fails the event tables on "+name, func(t *testing.T) {
			noBackoff(t)
			routes := googleRoutes(t, 90)
			if tc.calendars != nil {
				routes["/calendar/v3/users/me/calendarList"] = tc.calendars
			}
			if tc.events != nil {
				routes["/calendar/v3/calendars/ada@acme.com/events"] = tc.events
			}
			for _, table := range []string{"calendars", "calendar_events", "event_attendees"} {
				if tc.calendars == nil && table == "calendars" {
					continue
				}
				if _, err := read(t, googleAt(t, &googleWorkspace{routes: routes}), googleSource(), table); refusalOf(err) != tc.words {
					t.Fatalf("%s: Sync error = %v, want %q", table, err, tc.words)
				}
			}
		})
	}
	t.Run("retries a rate limit", func(t *testing.T) {
		noBackoff(t)
		var mu sync.Mutex
		calls := 0
		kind := googleAt(t, &googleWorkspace{routes: map[string]func(*http.Request) (int, string){
			"/calendar/v3/users/me/calendarList": func(*http.Request) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				if calls == 1 {
					return http.StatusTooManyRequests, `{}`
				}
				return http.StatusOK, `{"items":[{"id":"ada@acme.com"}]}`
			},
		}})
		if rows, err := read(t, kind, googleSource(), "calendars"); err != nil || len(rows) != 1 || calls != 2 {
			t.Fatalf("Sync = (%v, %v) after %d calls", rows, err, calls)
		}
	})
	// A limit that outlasts the retries fails the attempt with a message the syncer retries on.
	t.Run("refuses a rate limit that outlasts the retries, saying when to try again", func(t *testing.T) {
		noBackoff(t)
		kind := googleAt(t, &googleWorkspace{routes: map[string]func(*http.Request) (int, string){}})
		limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		t.Cleanup(limited.Close)
		kind.base = limited.URL
		before := time.Now()
		_, err := read(t, kind, googleSource(), "users")
		after := time.Now()
		const prefix = "this service account has used up its Google rate limit; try again after "
		if !strings.HasPrefix(refusalOf(err), prefix) {
			t.Fatalf("Sync error = %v, want a rate limit with its time", err)
		}
		at := strings.TrimPrefix(refusalOf(err), prefix)
		if early, late := before.Add(2*time.Minute).UTC().Format("15:04 UTC"), after.Add(2*time.Minute).UTC().Format("15:04 UTC"); at != early && at != late {
			t.Fatalf("retry at %q, want %s", at, early)
		}
	})
	t.Run("paces requests to every API together", func(t *testing.T) {
		if googleInterval < 100*time.Millisecond {
			t.Fatalf("googleInterval = %s, more than 10 requests a second", googleInterval)
		}
		var mu sync.Mutex
		var at []time.Time
		kind := googleAt(t, &googleWorkspace{routes: map[string]func(*http.Request) (int, string){
			"/admin/directory/v1/users": func(r *http.Request) (int, string) {
				mu.Lock()
				defer mu.Unlock()
				at = append(at, time.Now())
				if len(at) < 3 {
					return http.StatusOK, fmt.Sprintf(`{"users":[],"nextPageToken":"p%d"}`, len(at))
				}
				return http.StatusOK, `{"users":[]}`
			},
		}})
		googleInterval = 50 * time.Millisecond
		if _, err := read(t, kind, googleSource(), "users"); err != nil {
			t.Fatal(err)
		}
		for i := 1; i < len(at); i++ {
			if gap := at[i].Sub(at[i-1]); gap < 40*time.Millisecond {
				t.Fatalf("request %d came %s after the one before", i, gap)
			}
		}
	})
	t.Run("refuses a read Google could not be reached for", func(t *testing.T) {
		closed := httptest.NewServer(http.NotFoundHandler())
		closed.Close()
		kind := googleAt(t, &googleWorkspace{})
		kind.base = closed.URL
		if _, err := read(t, kind, googleSource(), "files"); refusalOf(err) != "could not reach Google; try again" {
			t.Fatalf("Sync error = %v, want a refusal", err)
		}
	})
	t.Run("stops when the read is cancelled", func(t *testing.T) {
		kind := googleAt(t, &googleWorkspace{routes: googleRoutes(t, 90)})
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := kind.Sync(cancelled, googleSource(), Table{Name: "users"}, &rowSink{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Sync error = %v, want context.Canceled", err)
		}
	})
	// A snapshot over its row limit stops the read with the writer's error, which the syncer reports.
	t.Run("stops at the writer's error", func(t *testing.T) {
		full := errors.New("row limit")
		kind := googleAt(t, &googleWorkspace{routes: googleRoutes(t, 90)})
		for _, table := range []string{"users", "files", "calendars", "calendar_events", "event_attendees"} {
			if _, err := kind.Sync(ctx, googleSource(), Table{Name: table}, refusingSink{full}); !errors.Is(err, full) {
				t.Fatalf("%s: Sync error = %v, want the writer's", table, err)
			}
		}
	})
	t.Run("has no object it does not list", func(t *testing.T) {
		_, err := read(t, googleAt(t, &googleWorkspace{}), googleSource(), "groups")
		var invalidErr *InvalidError
		if !errors.As(err, &invalidErr) {
			t.Fatalf("Sync error = %v, want InvalidError", err)
		}
		if _, err := read(t, googleAt(t, &googleWorkspace{}), Source{Secret: "{}", Settings: googleSettings}, "users"); !errors.As(err, &invalidErr) {
			t.Fatalf("Sync error = %v, want InvalidError", err)
		}
	})
}

// googleDocFiles are two pages of Drive's files with three Docs: doc1 with
// its owner, a shared drive's untitled doc2, the newest, and doc3, listed
// in another time zone, on the second page; and a Sheet, which is rows only.
var googleDocFiles = map[string]string{
	"": `{"files":[{"id":"doc1","name":"Q3  plan","mimeType":"application/vnd.google-apps.document","owners":[{"emailAddress":"Ada@acme.com"}],
		"modifiedTime":"2026-10-20T15:30:00.250Z","webViewLink":"https://docs.google.com/document/d/doc1/edit","version":"42"},
		{"id":"sheet1","name":"Budget","mimeType":"application/vnd.google-apps.spreadsheet","modifiedTime":"2026-10-28T10:00:00Z","version":"9"},
		{"id":"doc2","name":"","mimeType":"application/vnd.google-apps.document","driveId":"drv1","modifiedTime":"2026-10-25T08:00:00Z",
		"webViewLink":"https://docs.google.com/document/d/doc2/edit","version":"7"}],"nextPageToken":"f1"}`,
	"f1": `{"files":[{"id":"doc3","name":"Notes","mimeType":"application/vnd.google-apps.document","owners":[{"emailAddress":"grace@acme.com"}],
		"modifiedTime":"2026-10-22T09:00:00+01:00","webViewLink":"http://docs.google.com/document/d/doc3/edit"}]}`,
}

// googleExports are the Docs' Markdown exports: doc2 is empty, and doc3 is not UTF-8.
var googleExports = map[string]string{"doc1": "## Goals\n\nShip it.\n\n", "doc2": "", "doc3": "caf\xe9"}

func TestGoogleDocuments(t *testing.T) {
	ctx := context.Background()
	allTables := []Table{{Name: "users"}, {Name: "files"}, {Name: "calendars"}, {Name: "calendar_events"}, {Name: "event_attendees"}}
	// docRoutes serve googleDocFiles and the exports, or what exports has for a Doc.
	docRoutes := func(t *testing.T, exports map[string]func(*http.Request) (int, string)) map[string]func(*http.Request) (int, string) {
		routes := map[string]func(*http.Request) (int, string){
			"/drive/v3/files": func(r *http.Request) (int, string) {
				return http.StatusOK, googleDocFiles[r.URL.Query().Get("pageToken")]
			},
		}
		for id, text := range googleExports {
			routes["/drive/v3/files/"+id+"/export"] = func(r *http.Request) (int, string) {
				if r.URL.RawQuery != "mimeType=text%2Fmarkdown" {
					t.Errorf("export query = %s", r.URL.RawQuery)
				}
				if answer := exports[id]; answer != nil {
					return answer(r)
				}
				return http.StatusOK, text
			}
		}
		return routes
	}
	// pass reads tables in one SyncPass, into a sink per table.
	pass := func(t *testing.T, kind *googleKind, tables []Table, docs DocumentWriter) (map[string]*rowSink, error) {
		t.Helper()
		sinks, rows := map[string]*rowSink{}, map[string]RowWriter{}
		for _, table := range tables {
			sinks[table.Name] = &rowSink{}
			rows[table.Name] = sinks[table.Name]
		}
		written, err := kind.SyncPass(ctx, googleSource(), tables, rows, docs)
		if fmt.Sprint(written) != fmt.Sprint(tables) {
			t.Errorf("SyncPass wrote %v, want %v", written, tables)
		}
		return sinks, err
	}
	offers := []string{"docs doc2 1|2026-10-25T08:00:00Z|7", "docs doc3 1|2026-10-22T09:00:00+01:00|", "docs doc1 1|2026-10-20T15:30:00.250Z|42"}

	// The syncer keeps the first documents offered up to its cap, so the newest come first.
	t.Run("offers each Doc once the listing is whole, newest first, and writes what it wants", func(t *testing.T) {
		w := &googleWorkspace{routes: docRoutes(t, nil)}
		docs := &granolaDocs{want: map[string]bool{"docs": true}}
		sinks, err := pass(t, googleAt(t, w), []Table{{Name: "files"}}, docs)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(docs.offers, offers) || len(sinks["files"].rows) != 4 {
			t.Fatalf("offers = %q, rows = %d", docs.offers, len(sinks["files"].rows))
		}
		want := []Document{
			{ExternalID: "doc2", Version: "1|2026-10-25T08:00:00Z|7", At: time.Date(2026, 10, 25, 8, 0, 0, 0, time.UTC),
				URL: "https://docs.google.com/document/d/doc2/edit", Text: []byte("# Untitled document\n\nLast modified: 2026-10-25 08:00 UTC\nFile id: doc2\n")},
			{ExternalID: "doc3", Version: "1|2026-10-22T09:00:00+01:00|", At: time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC), Title: "Notes",
				URL:  "http://docs.google.com/document/d/doc3/edit",
				Text: []byte("# Notes\n\nOwner: grace@acme.com\nLast modified: 2026-10-22 08:00 UTC\nFile id: doc3\n\ncaf�\n")},
			{ExternalID: "doc1", Version: "1|2026-10-20T15:30:00.250Z|42", At: time.Date(2026, 10, 20, 15, 30, 0, 25e7, time.UTC), Title: "Q3 plan",
				URL:  "https://docs.google.com/document/d/doc1/edit",
				Text: []byte("# Q3 plan\n\nOwner: ada@acme.com\nLast modified: 2026-10-20 15:30 UTC\nFile id: doc1\n\n## Goals\n\nShip it.\n")},
		}
		if len(docs.puts) != len(want) {
			t.Fatalf("puts = %v", docs.putNames())
		}
		for i, p := range docs.puts {
			if p.stream != "docs" || !p.doc.At.Equal(want[i].At) || string(p.doc.Text) != string(want[i].Text) ||
				fmt.Sprint(p.doc.ExternalID, p.doc.Version, p.doc.Title, p.doc.URL) != fmt.Sprint(want[i].ExternalID, want[i].Version, want[i].Title, want[i].URL) {
				t.Errorf("put %d = %s %+v\n%s\nwant %+v\n%s", i, p.stream, p.doc, p.doc.Text, want[i], want[i].Text)
			}
		}
	})
	// A Doc the stream holds at this version is not exported again.
	t.Run("exports only the Docs it wants", func(t *testing.T) {
		w := &googleWorkspace{routes: docRoutes(t, nil)}
		docs := &granolaDocs{}
		if _, err := pass(t, googleAt(t, w), []Table{{Name: "files"}}, docs); err != nil {
			t.Fatal(err)
		}
		if exports := w.requested("/drive/v3/files/"); !slices.Equal(docs.offers, offers) || len(docs.puts) != 0 || len(exports) != 0 {
			t.Fatalf("offers = %q, puts = %v, exports = %v", docs.offers, docs.putNames(), exports)
		}
	})
	// Drive exports at most 10 MB, and an owner may keep viewers from downloading.
	t.Run("skips a Doc Drive won't export", func(t *testing.T) {
		w := &googleWorkspace{routes: docRoutes(t, map[string]func(*http.Request) (int, string){
			"doc1": googleFailure(http.StatusForbidden, "This file is too large to be exported.", "exportSizeLimitExceeded"),
			"doc2": googleFailure(http.StatusNotFound, "File not found: doc2.", "notFound"),
			"doc3": googleFailure(http.StatusForbidden, "This file cannot be exported by the user.", "cannotExportFile"),
		})}
		docs := &granolaDocs{want: map[string]bool{"docs": true}}
		sinks, err := pass(t, googleAt(t, w), []Table{{Name: "files"}}, docs)
		if err != nil || len(docs.offers) != 3 || len(docs.puts) != 0 || len(sinks["files"].rows) != 4 {
			t.Fatalf("SyncPass = %v, offers = %q, puts = %v", err, docs.offers, docs.putNames())
		}
	})
	for name, tc := range map[string]struct {
		answer func(*http.Request) (int, string)
		words  string
	}{
		"an outage":      {func(*http.Request) (int, string) { return http.StatusBadGateway, `` }, "could not reach Google; try again"},
		"a rejected key": {func(*http.Request) (int, string) { return http.StatusUnauthorized, `{}` }, "Google rejected this service account key; check it and paste it again"},
		"a rate limit": {googleFailure(http.StatusForbidden, "Rate Limit Exceeded", "rateLimitExceeded"),
			"this service account has used up its Google rate limit; try again in a few minutes"},
	} {
		t.Run("fails an export answered with "+name, func(t *testing.T) {
			noBackoff(t)
			w := &googleWorkspace{routes: docRoutes(t, map[string]func(*http.Request) (int, string){"doc2": tc.answer})}
			docs := &granolaDocs{want: map[string]bool{"docs": true}}
			if _, err := pass(t, googleAt(t, w), []Table{{Name: "files"}}, docs); refusalOf(err) != tc.words || len(docs.puts) != 0 {
				t.Fatalf("SyncPass error = %v after %v, want %q", err, docs.putNames(), tc.words)
			}
		})
	}
	t.Run("fails a Doc listed without its modified time", func(t *testing.T) {
		kind := googleAt(t, &googleWorkspace{routes: map[string]func(*http.Request) (int, string){
			"/drive/v3/files": func(*http.Request) (int, string) {
				return http.StatusOK, `{"files":[{"id":"doc1","name":"Q3 plan","mimeType":"application/vnd.google-apps.document"}]}`
			},
		}})
		_, err := pass(t, kind, []Table{{Name: "files"}}, &granolaDocs{})
		if err == nil || !strings.Contains(err.Error(), "Doc doc1 is listed without its modifiedTime") {
			t.Fatalf("SyncPass error = %v, want the listing refused", err)
		}
	})
	t.Run("stops at Put's error", func(t *testing.T) {
		failed := errors.New("store down")
		docs := &granolaDocs{want: map[string]bool{"docs": true}, fail: failed}
		if _, err := pass(t, googleAt(t, &googleWorkspace{routes: docRoutes(t, nil)}), []Table{{Name: "files"}}, docs); !errors.Is(err, failed) ||
			len(docs.puts) != 1 {
			t.Fatalf("SyncPass error = %v after %d puts, want Put's at once", err, len(docs.puts))
		}
	})
	// One pass lists each API once for all its tables.
	t.Run("reads every table in one pass", func(t *testing.T) {
		routes := googleRoutes(t, 90)
		for path, route := range docRoutes(t, nil) {
			routes[path] = route
		}
		w := &googleWorkspace{routes: routes}
		docs := &granolaDocs{want: map[string]bool{"docs": true}}
		sinks, err := pass(t, googleAt(t, w), allTables, docs)
		if err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for name, sink := range sinks {
			counts[name] = len(sink.rows)
		}
		want := map[string]int{"users": 2, "files": 4, "calendars": 4, "calendar_events": 3, "event_attendees": 2}
		if fmt.Sprint(counts) != fmt.Sprint(want) || len(docs.puts) != 3 {
			t.Fatalf("rows = %v, puts = %v", counts, docs.putNames())
		}
		if lists := len(w.requested("/admin/")) + len(w.requested("/drive/v3/files?")) + len(w.requested("/calendar/v3/users/")); lists != 5 {
			t.Fatalf("listed %v", w.requested("/"))
		}
	})
	// Sync has nowhere to Put documents, so it wants none and exports none.
	t.Run("reads the files through Sync without documents", func(t *testing.T) {
		w := &googleWorkspace{routes: docRoutes(t, nil)}
		kind := googleAt(t, w)
		sink := &rowSink{}
		if _, err := kind.Sync(ctx, googleSource(), Table{Name: "files"}, sink); err != nil || len(sink.rows) != 4 || len(w.requested("/drive/v3/files/")) != 0 {
			t.Fatalf("Sync = %v, rows = %v, exports = %v", err, sink.rows, w.requested("/drive/v3/files/"))
		}
		docs := &granolaDocs{want: map[string]bool{"docs": true}}
		if _, err := kind.SyncDocuments(ctx, googleSource(), Table{Name: "files"}, &rowSink{}, docs); err != nil || len(docs.puts) != 3 {
			t.Fatalf("SyncDocuments = %v, puts = %v", err, docs.putNames())
		}
	})
}
