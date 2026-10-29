package connector

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/jwt"
	"golang.org/x/time/rate"
)

var _ PassKind = (*googleKind)(nil)

const (
	defaultGoogleEventDays = 90
	// googleFutureDays is how far ahead of now calendar events are read.
	googleFutureDays = 30
	maxGooglePages   = 10_000
	googleDocType    = "application/vnd.google-apps.document"
	// googleFileFieldList are the fields of a file a Drive listing or
	// change feed asks for: Drive returns only id, name and mimeType of each
	// file unless asked.
	googleFileFieldList = "id,name,mimeType,driveId,parents,owners(emailAddress),lastModifyingUser(emailAddress),createdTime," +
		"modifiedTime,size,webViewLink,shared,starred,version,capabilities(canDownload)"
	// googleFileFields are the fields a Drive listing asks for.
	googleFileFields = "nextPageToken,incompleteSearch,files(" + googleFileFieldList + ")"
	// googleTokenMargin is how much of a token's life must be left for a
	// request to use it: Google's tokens live an hour, and a request with
	// its retries takes well under this.
	googleTokenMargin = 5 * time.Minute
	// googleExportTimeout bounds one export, which Drive makes of up to
	// 10 MB as it is asked, slower than a page of a list.
	googleExportTimeout = 2 * time.Minute
	// googleExportFailures is how many Docs in a row Drive may fail to
	// export, each skipped, before the read fails: that many is Drive
	// failing, not the Docs.
	googleExportFailures = 3
)

var (
	// googleImageData is a reference definition Drive's Markdown export
	// writes for each image of a Doc, on a line of its own, with the image's
	// bytes as a data URI; googleImageUse is a use of one, ![alt][label].
	googleImageData = regexp.MustCompile(`(?m)^\[([^\]\n]+)\]:[ \t]*<?data:image/[^\n]*\n?`)
	googleImageUse  = regexp.MustCompile(`!\[([^\]\n]*)\]\[([^\]\n]+)\]`)
)

var googleDocsSkipped = promauto.NewCounter(prometheus.CounterOpts{
	Name: "foundry_connector_google_docs_skipped_total",
	Help: "Google Docs a sync left as their set holds them because Drive failed to export them; the next sync exports them again.",
})

// googleLimits are the reasons Google gives a 403 or 429 for a rate limit or
// a quota: the request is retried with backoff, then refused with when to try
// again.
var googleLimits = []string{"rateLimitExceeded", "userRateLimitExceeded", "RATE_LIMIT_EXCEEDED",
	"quotaExceeded", "dailyLimitExceeded", "usageLimits"}

// googleInterval spaces one read's requests, to all three APIs together:
// 10 a second stays well under each API's per-user quota and leaves room
// for the user's other clients. Tests set it to 0.
var googleInterval = 100 * time.Millisecond

// googleAPI is one of the APIs a connector reads. Each is read under a token
// of its own scope, so a scope the admin didn't delegate makes only its
// tables unavailable.
type googleAPI struct {
	name  string // as the Google Cloud console names it
	scope string
	probe string // a one-item list, path and query, that proves it readable
}

var (
	googleDirectory = googleAPI{"Admin SDK", "https://www.googleapis.com/auth/admin.directory.user.readonly",
		"/admin/directory/v1/users?customer=my_customer&maxResults=1"}
	googleDrive    = googleAPI{"Drive", "https://www.googleapis.com/auth/drive.readonly", "/drive/v3/files?pageSize=1"}
	googleCalendar = googleAPI{"Google Calendar", "https://www.googleapis.com/auth/calendar.readonly",
		"/calendar/v3/users/me/calendarList?maxResults=1"}
)

var googleObjects = []saasTable{
	{"users", "user_id"},
	{"files", "file_id"},
	{"calendars", "calendar_id"},
	// An event's id is its calendar's id and its own, since Google repeats
	// an event's id in each calendar it is on.
	{"calendar_events", "event_id"},
	// An event's attendees come with the event; Discover keys them by (event_id, email).
	{"event_attendees", "event_id"},
}

// googleAPIOf is the API each object is read from.
var googleAPIOf = map[string]googleAPI{
	"users": googleDirectory, "files": googleDrive,
	"calendars": googleCalendar, "calendar_events": googleCalendar, "event_attendees": googleCalendar,
}

var googleForeignKeys = map[string][]ForeignKey{
	"calendar_events": {foreignKey("calendar_id", "calendars", "calendar_id")},
	"event_attendees": {foreignKey("event_id", "calendar_events", "event_id")},
}

// googleDocsStream is every Google Doc in the files table.
var googleDocsStream = Stream{Name: "docs", Table: "files", Title: "Docs", Format: Markdown,
	Description: "Each Google Doc the connection's user can open and download in Drive, as Markdown: its title, owner, last " +
		"modified time and file id, then the Doc's text as Drive exports it, with its headings, lists, tables and links; its " +
		"images are left out, keeping their alt text, and a Doc whose export is over Drive's 10 MB limit is its plain text."}

var (
	// errUnreadable is an API the connection can't read; its refusal's
	// message is why, worded as the fix.
	errUnreadable = errors.New("google: the API is unreadable")
	// errNoAccess is a 403 or 404 for one object the user can't read, such
	// as a calendar they see only the free/busy of.
	errNoAccess = errors.New("google: no access")
	// errUnreachable is a request Google gave no answer to, or a server
	// error after the retries.
	errUnreachable = errors.New("google: no answer")
	// errTokenGone is a change feed's token Google no longer takes: Drive's
	// page token or Calendar's sync token. A change read reads the feed's
	// tables in full instead.
	errTokenGone = errors.New("google: the change token is no longer valid")
)

type googleKind struct {
	base     string
	tokenURL string
	http     *http.Client
}

func (k *googleKind) Name() string        { return "google" }
func (k *googleKind) DisplayName() string { return "Google Workspace" }

// GoogleSettings are a Google Workspace connector's settings: the user the
// service account acts as, and how many days of past calendar events it keeps.
type GoogleSettings struct {
	Subject   string `json:"subject"`
	EventDays int    `json:"event_days"`
}

// googleKey is what a connector uses of a service account's JSON key.
type googleKey struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKey   string `json:"private_key"`
	PrivateKeyID string `json:"private_key_id"`
}

// parseGoogleKey reads a service account's JSON key; ok is false unless it
// has every field the token exchange signs with, and a PEM RSA private key,
// as Google makes them.
func parseGoogleKey(secret string) (key googleKey, ok bool) {
	if json.Unmarshal([]byte(strings.TrimSpace(secret)), &key) != nil || key.Type != "service_account" ||
		key.ClientEmail == "" || key.PrivateKeyID == "" {
		return key, false
	}
	block, _ := pem.Decode([]byte(key.PrivateKey))
	if block == nil {
		return key, false
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		_, ok = parsed.(*rsa.PrivateKey)
		return key, ok
	}
	_, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	return key, err == nil
}

func (k *googleKind) Prepare(src Source) (json.RawMessage, error) {
	if _, ok := parseGoogleKey(src.Secret); !ok {
		return nil, invalid("paste the service account's JSON key")
	}
	var s GoogleSettings
	decoder := json.NewDecoder(bytes.NewReader(orEmpty(src.Settings)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&s); err != nil {
		return nil, invalid("this connector takes only its key, the user it acts as and how many days of past events to keep")
	}
	s.Subject = strings.ToLower(strings.TrimSpace(s.Subject))
	if address, err := mail.ParseAddress(s.Subject); err != nil || address.Address != s.Subject || strings.Count(s.Subject, "@") != 1 {
		return nil, invalid("enter the email of the Workspace user Foundry acts as")
	}
	switch {
	case s.EventDays == 0:
		s.EventDays = defaultGoogleEventDays
	case s.EventDays < 1 || s.EventDays > 365:
		return nil, invalid("keep 1 to 365 days of past events")
	}
	return json.Marshal(s)
}

// Streams are the Docs, always.
func (k *googleKind) Streams(Source) []Stream { return []Stream{googleDocsStream} }

// Discover exchanges the key for each API's token and lists one item of
// each, concurrently. A scope the admin didn't delegate, an API the
// service account's project hasn't enabled, or a Directory the user may
// not read makes only that API's tables unavailable, saying how to fix it.
// The six requests are cheap enough for every sync to make them too.
func (k *googleKind) Discover(ctx context.Context, src Source) (Discovery, error) {
	c, err := k.client(src)
	if err != nil {
		return Discovery{}, err
	}
	apis := map[string]googleAPI{}
	for _, api := range googleAPIOf {
		apis[api.name] = api
	}
	var mu sync.Mutex
	reasons := map[string]string{}
	readable, err := probeAll(ctx, slices.Collect(maps.Keys(apis)), func(ctx context.Context, name string) (bool, error) {
		api := apis[name]
		status, body, err := c.get(ctx, c.k.http, api, api.probe)
		if err == nil {
			err = c.status(status, body, api, api.probe)
		}
		var refusal *RefusedError
		switch {
		case errors.Is(err, errUnreadable) && errors.As(err, &refusal):
			mu.Lock()
			defer mu.Unlock()
			reasons[name] = refusal.Message
			return false, nil
		case errors.Is(err, errNoAccess):
			return false, nil
		}
		return err == nil, err
	})
	if err != nil {
		return Discovery{}, err
	}
	d := saasDiscovery(googleObjects, func(name string) bool { return readable[googleAPIOf[name].name] }, googleForeignKeys)
	if len(d.Tables) == 0 {
		return Discovery{}, refused(nil, "the service account cannot read Drive, Calendar or the Directory as %s; "+
			"check its domain-wide delegation and test again", c.settings.Subject)
	}
	for i, u := range d.Unavailable {
		d.Unavailable[i].Reason = reasons[googleAPIOf[u.Name].name]
	}
	for i, t := range d.Tables {
		if t.Name == "event_attendees" {
			d.Tables[i].PrimaryKey = []string{"event_id", "email"}
		}
		// Events are read from event_days ago to googleFutureDays ahead.
		d.Tables[i].Windowed = WindowedTable("google", t.Name)
	}
	return d, nil
}

// Hint is the key's service account and the last four characters of its
// key id, e.g. foundry@acme.iam.gserviceaccount.com…1a2b.
func (k *googleKind) Hint(secret string) string {
	key, ok := parseGoogleKey(secret)
	if !ok {
		return "…"
	}
	return key.ClientEmail + "…" + key.PrivateKeyID[max(0, len(key.PrivateKeyID)-4):]
}

// Sync and SyncDocuments read one table through SyncPass. The syncer
// reads a PassKind's tables in one pass and calls neither.
func (k *googleKind) Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error) {
	return k.SyncDocuments(ctx, src, table, w, noDocuments{})
}

func (k *googleKind) SyncDocuments(ctx context.Context, src Source, table Table, rows RowWriter, docs DocumentWriter) (Table, error) {
	_, err := k.SyncPass(ctx, src, []Table{table}, map[string]RowWriter{table.Name: rows}, docs)
	return table, err
}

// SyncPass reads each API the tables need once: the Directory's users;
// Drive's files, then the Docs among them as documents; and the calendar
// list, then each calendar's events, which fill both event tables.
func (k *googleKind) SyncPass(ctx context.Context, src Source, tables []Table, rows map[string]RowWriter, docs DocumentWriter) ([]Table, error) {
	for _, table := range tables {
		if _, ok := googleAPIOf[table.Name]; !ok {
			return tables, invalid("Google Workspace has no object %q", table.Name)
		}
	}
	c, err := k.client(src)
	if err != nil {
		return tables, err
	}
	if w := rows["users"]; w != nil {
		if err := c.users(ctx, w); err != nil {
			return tables, err
		}
	}
	if w := rows["files"]; w != nil {
		if err := c.files(ctx, w, docs); err != nil {
			return tables, err
		}
	}
	if rows["calendars"] != nil || rows["calendar_events"] != nil || rows["event_attendees"] != nil {
		if err := c.calendars(ctx, rows); err != nil {
			return tables, err
		}
	}
	return tables, nil
}

// googleClient is one read's requests as the settings' user, paced together
// under one limit, with a token per API's scope.
type googleClient struct {
	k        *googleKind
	settings GoogleSettings
	tokens   map[string]*googleToken // by scope
	pace     *rate.Limiter
	// exports is the kind's HTTP client with googleExportTimeout.
	exports *http.Client
}

// googleToken is one scope's access token: exchanged when first used, again
// once it has under googleTokenMargin left, and again when an API refuses it.
type googleToken struct {
	config *jwt.Config
	mu     sync.Mutex
	token  *oauth2.Token
}

// client reads the prepared settings and the key. A subject is required:
// without one the assertion names no user, and the service account would
// read as itself.
func (k *googleKind) client(src Source) (*googleClient, error) {
	key, ok := parseGoogleKey(src.Secret)
	if !ok {
		return nil, invalid("paste the service account's JSON key")
	}
	var s GoogleSettings
	if err := json.Unmarshal(src.Settings, &s); err != nil {
		return nil, invalid("Google Workspace settings are malformed")
	}
	if s.Subject == "" {
		return nil, invalid("enter the email of the Workspace user Foundry acts as")
	}
	exports := *k.http
	exports.Timeout = googleExportTimeout
	c := &googleClient{k: k, settings: s, tokens: map[string]*googleToken{}, pace: rate.NewLimiter(rate.Every(googleInterval), 1), exports: &exports}
	for _, api := range googleAPIOf {
		c.tokens[api.scope] = &googleToken{config: &jwt.Config{Email: key.ClientEmail, PrivateKey: []byte(key.PrivateKey),
			PrivateKeyID: key.PrivateKeyID, Scopes: []string{api.scope}, TokenURL: k.tokenURL, Subject: s.Subject}}
	}
	return c, nil
}

// token is api's access token, a new one in place of rejected, the token an
// API refused, if that is the one held. The exchange, through the kind's own
// HTTP client, is retried on no answer, a 429 or a 5xx, as fetchWithHeader
// retries. A scope the admin didn't delegate is unreadable; a user Google
// doesn't know, or a key it doesn't, is refused.
func (c *googleClient) token(ctx context.Context, api googleAPI, rejected string) (string, error) {
	t := c.tokens[api.scope]
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != nil && t.token.AccessToken != rejected && (t.token.Expiry.IsZero() || time.Until(t.token.Expiry) > googleTokenMargin) {
		return t.token.AccessToken, nil
	}
	exchange := context.WithValue(ctx, oauth2.HTTPClient, c.k.http)
	for attempt := 0; ; attempt++ {
		// A new token source holds no token, so it exchanges one.
		token, err := t.config.TokenSource(exchange).Token()
		if err == nil {
			t.token = token
			return token.AccessToken, nil
		}
		var answer *oauth2.RetrieveError
		answered := errors.As(err, &answer)
		if answered && answer.Response.StatusCode != http.StatusTooManyRequests && answer.Response.StatusCode < 500 {
			return "", c.tokenRefusal(api, answer)
		}
		if attempt == maxRetries {
			return "", refused(err, "could not reach Google; try again")
		}
		retryAfter := ""
		if answered {
			retryAfter = answer.Response.Header.Get("Retry-After")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(retryBackoff(attempt, retryAfter)):
		}
	}
}

// tokenRefusal says why the token endpoint refused api's token.
func (c *googleClient) tokenRefusal(api googleAPI, answer *oauth2.RetrieveError) error {
	var reply struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(answer.Body, &reply) // an answer without them is judged by its status
	subject := c.settings.Subject
	switch {
	case reply.Error == "unauthorized_client":
		return refused(errUnreadable, "allow the scope %s for the service account's client ID in the Admin console's domain-wide delegation", api.scope)
	case reply.Error == "invalid_grant" && (strings.Contains(reply.Description, "Invalid email or User ID") ||
		strings.Contains(strings.ToLower(reply.Description), subject)):
		return refused(nil, "Google has no user %s, or the service account may not act as them; check the email", subject)
	case reply.Error == "invalid_grant" || reply.Error == "invalid_client":
		return refused(nil, "Google rejected this service account key; check it and paste it again")
	}
	return refused(answer, "Google refused the service account a token (%s); check its key and domain-wide delegation",
		cmp.Or(reply.Error, answer.Response.Status))
}

// get sends one GET of target, a path and query, under api's token through
// hc, paced, with the token each attempt holds. It retries what
// fetchWithHeader retries, a 429 or a 5xx, and a 403 for a rate limit or a
// quota (googleLimits), which Google sends without Retry-After; a 401 is
// retried once, at once, under a new token. A rate limit that outlasts the
// retries is refused with when to try again, so the sync retries rather than
// failing for good; a request Google gave no answer to is refused as
// errUnreachable.
func (c *googleClient) get(ctx context.Context, hc *http.Client, api googleAPI, target string) (int, []byte, error) {
	rejected := ""
	for attempt := 0; ; attempt++ {
		if err := c.pace.Wait(ctx); err != nil {
			return 0, nil, err
		}
		token, err := c.token(ctx, api, rejected)
		if err != nil {
			return 0, nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.k.base+target, nil)
		if err != nil {
			return 0, nil, err
		}
		req.Header = http.Header{"Authorization": {"Bearer " + token}}
		status, header, body, err := func() (int, http.Header, []byte, error) {
			res, err := hc.Do(req)
			if err != nil {
				return 0, nil, nil, err
			}
			defer res.Body.Close()
			body, err := io.ReadAll(io.LimitReader(res.Body, maxPageBytes))
			return res.StatusCode, res.Header, body, err
		}()
		switch {
		case ctx.Err() != nil:
			return 0, nil, ctx.Err()
		case err != nil:
			return 0, nil, refused(fmt.Errorf("%w: %w", errUnreachable, err), "could not reach Google; try again")
		}
		reasons, _ := googleError(body)
		limited := status == http.StatusTooManyRequests ||
			status == http.StatusForbidden && slices.ContainsFunc(reasons, func(r string) bool { return slices.Contains(googleLimits, r) })
		switch {
		case status == http.StatusUnauthorized && rejected == "" && attempt < maxRetries:
			rejected = token
			continue
		case (limited || status >= 500) && attempt < maxRetries:
			select {
			case <-ctx.Done():
				return 0, nil, ctx.Err()
			case <-time.After(retryBackoff(attempt, header.Get("Retry-After"))):
			}
			continue
		case limited:
			when := "in a few minutes"
			if seconds, err := strconv.Atoi(header.Get("Retry-After")); err == nil && seconds > 0 {
				when = "after " + time.Now().Add(time.Duration(seconds)*time.Second).UTC().Format("15:04 UTC")
			}
			return 0, nil, refused(nil, "this service account has used up its Google rate limit; try again %s", when)
		}
		return status, body, nil
	}
}

// status says what an answer other than a success means for api's target:
// an API the connection can't read is errUnreadable, worded as the fix;
// any other 403 or 404 is errNoAccess, one object the user can't read.
func (c *googleClient) status(status int, body []byte, api googleAPI, target string) error {
	if status < 300 {
		return nil
	}
	path, _, _ := strings.Cut(target, "?")
	reasons, message := googleError(body)
	switch {
	// The token endpoint took the key, and get tried a new token, so the
	// key is not the fault.
	case status == http.StatusUnauthorized:
		return refused(nil, "Google refused the %s API's access token for %s; try again", api.name, c.settings.Subject)
	case status == http.StatusForbidden && (slices.Contains(reasons, "accessNotConfigured") || slices.Contains(reasons, "SERVICE_DISABLED")):
		return refused(errUnreadable, "enable the %s API in the service account's Google Cloud project", api.name)
	case status == http.StatusForbidden && api == googleDirectory && strings.Contains(message, "Not Authorized to access this resource/api"):
		return refused(errUnreadable, "%s is not an admin, so the Directory's users can't be read", c.settings.Subject)
	case status == http.StatusForbidden || status == http.StatusNotFound:
		return refused(errNoAccess, "this service account cannot read %s as %s; check its access and test again", path, c.settings.Subject)
	// Calendar's answer to a sync token it no longer takes.
	case status == http.StatusGone:
		return refused(errTokenGone, "Google no longer holds the changes of %s; try again", path)
	case status >= 500:
		return refused(errUnreachable, "could not reach Google; try again")
	}
	return refused(nil, "Google returned HTTP %d for %s", status, path)
}

// googleError is the reasons and message of a Google API's error answer:
// errors[].reason, as the older APIs give them, then details[].reason, as
// the newer do. An answer that isn't one has none.
func googleError(body []byte) (reasons []string, message string) {
	var answer struct {
		Error struct {
			Message         string
			Errors, Details []struct{ Reason string }
		}
	}
	_ = json.Unmarshal(body, &answer)
	for _, e := range slices.Concat(answer.Error.Errors, answer.Error.Details) {
		reasons = append(reasons, e.Reason)
	}
	return reasons, answer.Error.Message
}

// pages lists a Google collection a page at a time, following
// nextPageToken, and passes each item of field to each, in the order
// listed: a list that changes while it is paged may show an item twice, the
// later copy as it is later. A full read takes the first (googleOnce); a
// change read every copy, the last winning. Google leaves an empty list
// out. It returns the last page's nextSyncToken, which
// Calendar gives a list of events, and "" for a list without one. Only the
// first page refused (errNoAccess) says the user can't read the list. A
// later page refused, a page that isn't an object, whose list isn't one of
// objects, that lists an item without an id, that gives back the token it
// was asked with, or that Drive says searched only part of what the user
// can read fails the read: ending early would publish a partial table.
func (c *googleClient) pages(ctx context.Context, api googleAPI, path string, query url.Values, field string,
	each func(id string, item map[string]any) error) (string, error) {
	for page := 0; ; page++ {
		if page == maxGooglePages {
			return "", fmt.Errorf("google %s: more than %d pages", path, maxGooglePages)
		}
		target := path + "?" + query.Encode()
		status, body, err := c.get(ctx, c.k.http, api, target)
		if err != nil {
			return "", err
		}
		if err := c.status(status, body, api, target); err != nil {
			if page > 0 && errors.Is(err, errNoAccess) {
				return "", refused(nil, "Google stopped listing %s as %s part way through; try again", path, c.settings.Subject)
			}
			return "", err
		}
		var (
			fields          map[string]json.RawMessage
			items           []map[string]any
			next, syncToken string
			incomplete      bool
		)
		err = json.Unmarshal(body, &fields)
		for name, into := range map[string]any{field: &items, "nextPageToken": &next, "nextSyncToken": &syncToken, "incompleteSearch": &incomplete} {
			if raw, ok := fields[name]; ok && err == nil {
				err = json.Unmarshal(raw, into)
			}
		}
		switch {
		case err != nil || fields == nil:
			return "", fmt.Errorf("google %s: the answer is not a complete page of %s", path, field)
		case incomplete:
			return "", refused(nil, "Drive searched only part of what %s can read; try again later", c.settings.Subject)
		case next != "" && next == query.Get("pageToken"):
			return "", fmt.Errorf("google %s: a page gives back the token it was asked with", path)
		}
		for _, item := range items {
			id, _ := item["id"].(string)
			if id == "" {
				return "", fmt.Errorf("google %s: a listed item has no id", path)
			}
			if err := each(id, item); err != nil {
				return "", err
			}
		}
		if next == "" {
			return syncToken, nil
		}
		query.Set("pageToken", next)
	}
}

// googleOnce passes each id's first item to each, as a full read writes
// each row once.
func googleOnce(each func(id string, item map[string]any) error) func(id string, item map[string]any) error {
	seen := map[string]bool{}
	return func(id string, item map[string]any) error {
		if seen[id] {
			return nil
		}
		seen[id] = true
		return each(id, item)
	}
}

// users lists the Directory's users of the user's own customer.
func (c *googleClient) users(ctx context.Context, w RowWriter) error {
	query := url.Values{"customer": {"my_customer"}, "maxResults": {"500"}}
	_, err := c.pages(ctx, googleDirectory, "/admin/directory/v1/users", query, "users", googleOnce(func(id string, u map[string]any) error {
		name, _ := u["name"].(map[string]any)
		org, _ := firstOf(u["organizations"]).(map[string]any)
		var manager any
		relations, _ := u["relations"].([]any)
		for _, r := range relations {
			if relation, _ := r.(map[string]any); relation["type"] == "manager" {
				manager = googleEmail(relation["value"])
				break
			}
		}
		lastLogin := googleTime(u["lastLoginTime"])
		if lastLogin == "1970-01-01T00:00:00Z" { // Google's never
			lastLogin = nil
		}
		return w.Write(map[string]any{
			"user_id": id, "primary_email": googleEmail(u["primaryEmail"]),
			"full_name": name["fullName"], "given_name": name["givenName"], "family_name": name["familyName"],
			"is_admin": u["isAdmin"] == true, "suspended": u["suspended"] == true, "archived": u["archived"] == true,
			"org_unit_path": u["orgUnitPath"], "title": org["title"], "department": org["department"], "manager_email": manager,
			"created_at": googleTime(u["creationTime"]), "last_login_at": lastLogin,
		})
	}))
	return err
}

// googleDoc is a listed Google Doc: what its document is made of besides
// its export, the time that orders it, and its version. A withheld Doc is
// one its owner keeps viewers from downloading, and so exporting: it can't
// be a document, and has no time.
type googleDoc struct {
	id, name, owner, link, version string
	modified                       time.Time
	withheld                       bool
}

// googleFile is a listed file's row and, for a Doc, its document's parts;
// nil for any other file. A Doc the user may download must be listed with
// its modifiedTime, which orders its document.
func googleFile(id string, f map[string]any) (map[string]any, *googleDoc, error) {
	owner, _ := firstOf(f["owners"]).(map[string]any)
	modifier, _ := f["lastModifyingUser"].(map[string]any)
	var size any // an int64 as a string; Google's own files have none
	if s, ok := f["size"].(string); ok {
		size = s
	}
	row := map[string]any{
		"file_id": id, "name": f["name"], "mime_type": f["mimeType"], "drive_id": f["driveId"], "parent_id": firstOf(f["parents"]),
		"owner_email": googleEmail(owner["emailAddress"]), "last_modifying_user_email": googleEmail(modifier["emailAddress"]),
		"created_at": googleTime(f["createdTime"]), "modified_at": googleTime(f["modifiedTime"]), "size": size,
		"web_view_link": f["webViewLink"], "shared": f["shared"] == true, "starred": f["starred"] == true,
	}
	if f["mimeType"] != googleDocType {
		return row, nil, nil
	}
	raw, _ := f["modifiedTime"].(string)
	doc := &googleDoc{id: id}
	doc.name, _ = f["name"].(string)
	doc.owner, _ = row["owner_email"].(string)
	doc.link, _ = f["webViewLink"].(string)
	// The version is a render revision, the Doc's modified time and Drive's
	// version of it, then a digest of what the document shows beside the
	// export, which may change without either: an owner's email renamed.
	version, _ := f["version"].(string)
	shown := sha256.Sum256([]byte(doc.name + "\x00" + doc.owner + "\x00" + doc.link))
	doc.version = "1|" + raw + "|" + version + "|" + hex.EncodeToString(shown[:8])
	if capabilities, _ := f["capabilities"].(map[string]any); capabilities["canDownload"] == false {
		doc.withheld = true
		return row, doc, nil
	}
	modified, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return row, nil, fmt.Errorf("google /drive/v3/files: Doc %s is listed without its modifiedTime", id)
	}
	doc.modified = modified
	return row, doc, nil
}

// files lists every file the user can read that isn't in the trash,
// folders included: My Drive and what is shared with them (corpora=user),
// then each shared drive they are a member of (corpora=drive), each file
// once by its id. Drive may search only part of corpora=allDrives for a
// user in many shared drives, on every attempt, so each is listed alone. A
// shared drive whose first page is refused, one the user left since the
// drives were listed, is skipped. Once the listing is whole, it offers each
// Doc's document, newest first (googleExporter). A withheld Doc is not
// offered, so the read, always in full, removes what the stream holds of it.
func (c *googleClient) files(ctx context.Context, w RowWriter, docs DocumentWriter) error {
	var listed []googleDoc
	seen := map[string]bool{}
	list := func(corpus url.Values) error {
		query := url.Values{"q": {"trashed=false"}, "includeItemsFromAllDrives": {"true"}, "supportsAllDrives": {"true"},
			"pageSize": {"1000"}, "fields": {googleFileFields}}
		maps.Copy(query, corpus)
		_, err := c.pages(ctx, googleDrive, "/drive/v3/files", query, "files", func(id string, f map[string]any) error {
			if seen[id] {
				return nil
			}
			seen[id] = true
			row, doc, err := googleFile(id, f)
			if err := w.Write(row); err != nil {
				return err
			}
			if err == nil && doc != nil && !doc.withheld {
				listed = append(listed, *doc)
			}
			return err
		})
		return err
	}
	if err := list(url.Values{"corpora": {"user"}}); err != nil {
		return err
	}
	drives, err := c.drives(ctx)
	if err != nil {
		return err
	}
	for _, drive := range drives {
		if err := list(url.Values{"corpora": {"drive"}, "driveId": {drive}}); err != nil && !errors.Is(err, errNoAccess) {
			return err
		}
	}
	// The syncer keeps the first documents offered up to its cap.
	slices.SortStableFunc(listed, func(a, b googleDoc) int { return b.modified.Compare(a.modified) })
	exporter := googleExporter{c: c, docs: docs}
	for _, doc := range listed {
		if err := exporter.offer(ctx, doc); err != nil {
			return err
		}
	}
	return nil
}

// drives are the ids of the shared drives the user is a member of.
func (c *googleClient) drives(ctx context.Context) ([]string, error) {
	var drives []string
	_, err := c.pages(ctx, googleDrive, "/drive/v3/drives", url.Values{"pageSize": {"100"}, "fields": {"nextPageToken,drives(id)"}}, "drives",
		googleOnce(func(id string, _ map[string]any) error {
			drives = append(drives, id)
			return nil
		}))
	return drives, err
}

// googleExporter offers a read's Docs to its documents and exports the ones
// they want (export): an unchanged Doc costs nothing. One Drive refuses to
// export now is dropped. One Drive fails to export is skipped and counted,
// keeping what the stream holds of it until a later sync exports it, unless
// googleExportFailures fail in a row.
type googleExporter struct {
	c      *googleClient
	docs   DocumentWriter
	failed int // in a row
}

// offer offers doc, which is not withheld.
func (e *googleExporter) offer(ctx context.Context, doc googleDoc) error {
	if !e.docs.Want(googleDocsStream.Name, doc.id, doc.version) {
		return nil
	}
	export, err := e.c.export(ctx, doc.id, "text/markdown")
	if errors.Is(err, errUnreachable) {
		if e.failed++; e.failed == googleExportFailures {
			return err
		}
		googleDocsSkipped.Inc()
		return nil
	}
	e.failed = 0
	switch {
	case errors.Is(err, errNoAccess):
		return e.docs.Drop(googleDocsStream.Name, doc.id)
	case err != nil:
		return err
	}
	name := strings.Join(strings.Fields(doc.name), " ")
	var text strings.Builder
	fmt.Fprintf(&text, "# %s\n\n", cmp.Or(name, "Untitled document"))
	if doc.owner != "" {
		fmt.Fprintf(&text, "Owner: %s\n", doc.owner)
	}
	fmt.Fprintf(&text, "Last modified: %s\nFile id: %s\n", doc.modified.UTC().Format("2006-01-02 15:04 UTC"), doc.id)
	if export = strings.TrimSpace(export); export != "" {
		text.WriteString("\n" + export + "\n")
	}
	return e.docs.Put(ctx, googleDocsStream.Name, Document{ExternalID: doc.id, Version: doc.version, At: doc.modified,
		Title: name, URL: doc.link, Text: []byte(text.String())})
}

// export is Doc id's text as Drive exports it as mediaType, under
// googleExportTimeout: Markdown without its images, whose bytes Drive
// writes into it as data URIs, keeping their alt text; or plain text, which
// has none, when the Markdown with its images is over Drive's 10 MB limit.
// err wraps errNoAccess when Drive won't export the Doc to the user (its
// owner keeps viewers from downloading it, it is over the limit as plain
// text too, or it is gone or unshared since the listing), and
// errUnreachable when Drive gave no answer, or a server error after the
// retries; any other fails the read: a refused token, a disabled API, a
// rate limit that outlasted the retries.
func (c *googleClient) export(ctx context.Context, id, mediaType string) (string, error) {
	target := "/drive/v3/files/" + url.PathEscape(id) + "/export?" + url.Values{"mimeType": {mediaType}}.Encode()
	status, body, err := c.get(ctx, c.exports, googleDrive, target)
	if err != nil {
		return "", err
	}
	if reasons, _ := googleError(body); status == http.StatusForbidden && mediaType == "text/markdown" &&
		slices.Contains(reasons, "exportSizeLimitExceeded") {
		return c.export(ctx, id, "text/plain")
	}
	if err := c.status(status, body, googleDrive, target); err != nil {
		return "", err
	}
	// A plain text export starts with a byte order mark, and ends its lines with CRLF.
	text := strings.ReplaceAll(strings.TrimPrefix(strings.ToValidUTF8(string(body), "\uFFFD"), "\uFEFF"), "\r\n", "\n")
	if mediaType != "text/markdown" {
		return text, nil
	}
	images := map[string]bool{}
	for _, m := range googleImageData.FindAllStringSubmatch(text, -1) {
		images[m[1]] = true
	}
	return googleImageUse.ReplaceAllStringFunc(googleImageData.ReplaceAllString(text, ""), func(use string) string {
		if m := googleImageUse.FindStringSubmatch(use); images[m[2]] {
			return m[1]
		}
		return use
	}), nil
}

// calendars lists the user's calendar list into the calendars table and,
// when an event table is read, each calendar's events in the window
// (googleWindow), a recurring event as its instances and a cancelled one
// left out. A calendar whose first page of events is refused (errNoAccess),
// as one the user sees only the free/busy of or one deleted since the list,
// is skipped.
func (c *googleClient) calendars(ctx context.Context, rows map[string]RowWriter) error {
	calendars, err := c.calendarList(ctx, func(id string, cal map[string]any) error {
		w := rows["calendars"]
		if w == nil {
			return nil
		}
		return w.Write(map[string]any{
			"calendar_id": id, "summary": cal["summary"], "description": cal["description"], "time_zone": cal["timeZone"],
			"access_role": cal["accessRole"], "primary": cal["primary"] == true,
		})
	})
	events, attendees := rows["calendar_events"], rows["event_attendees"]
	if err != nil || events == nil && attendees == nil {
		return err
	}
	from, to := googleWindow(time.Now(), c.settings.EventDays)
	window := url.Values{"singleEvents": {"true"}, "showDeleted": {"false"}, "maxResults": {"2500"},
		"timeMin": {from.Format(time.RFC3339)}, "timeMax": {to.Format(time.RFC3339)}}
	for _, calendar := range calendars {
		_, err := c.pages(ctx, googleCalendar, googleEventsPath(calendar), maps.Clone(window), "items", googleOnce(func(id string, e map[string]any) error {
			if e["status"] == "cancelled" {
				return nil
			}
			row, people := googleEvent(calendar, id, e)
			if events != nil {
				if err := events.Write(row); err != nil {
					return err
				}
			}
			if attendees == nil {
				return nil
			}
			for _, person := range people {
				if err := attendees.Write(person); err != nil {
					return err
				}
			}
			return nil
		}))
		if err != nil && !errors.Is(err, errNoAccess) {
			return err
		}
	}
	return nil
}

// calendarList lists the user's calendar list, passing each calendar to
// each, and returns their ids.
func (c *googleClient) calendarList(ctx context.Context, each func(id string, cal map[string]any) error) ([]string, error) {
	var calendars []string
	_, err := c.pages(ctx, googleCalendar, "/calendar/v3/users/me/calendarList", url.Values{"maxResults": {"250"}}, "items",
		googleOnce(func(id string, cal map[string]any) error {
			calendars = append(calendars, id)
			return each(id, cal)
		}))
	return calendars, err
}

// googleWindow is the window of events a read keeps, as of now: from days
// ago to googleFutureDays ahead.
func googleWindow(now time.Time, days int) (from, to time.Time) {
	now = now.UTC().Truncate(time.Second)
	return now.AddDate(0, 0, -days), now.AddDate(0, 0, googleFutureDays)
}

// googleEventsPath is the path of a calendar's events.
func googleEventsPath(calendar string) string {
	return "/calendar/v3/calendars/" + url.PathEscape(calendar) + "/events"
}

// googleEvent is a listed event's calendar_events row and its
// event_attendees rows: each person it invites once by email, a room or
// other resource being no person. An event whose attendees Google left out
// says so (attendees_omitted).
func googleEvent(calendar, id string, e map[string]any) (map[string]any, []map[string]any) {
	eventID := calendar + ":" + id
	var people []map[string]any
	seen := map[string]bool{}
	list, _ := e["attendees"].([]any)
	for _, a := range list {
		person, _ := a.(map[string]any)
		email, _ := googleEmail(person["email"]).(string)
		if email == "" || person["resource"] == true || seen[email] {
			continue
		}
		seen[email] = true
		people = append(people, map[string]any{
			"event_id": eventID, "email": email, "display_name": person["displayName"], "response_status": person["responseStatus"],
			"optional": person["optional"] == true, "organizer": person["organizer"] == true, "is_self": person["self"] == true,
		})
	}
	start, _ := e["start"].(map[string]any)
	end, _ := e["end"].(map[string]any)
	// An all-day event has dates, kept as they are; any other has times.
	at := func(when map[string]any) any {
		if date, ok := when["date"].(string); ok {
			return date
		}
		return googleTime(when["dateTime"])
	}
	_, allDay := start["date"].(string)
	organizer, _ := e["organizer"].(map[string]any)
	creator, _ := e["creator"].(map[string]any)
	return map[string]any{
		"event_id": eventID, "calendar_id": calendar, "ical_uid": e["iCalUID"], "summary": e["summary"],
		"description": e["description"], "location": e["location"], "status": e["status"],
		"start_at": at(start), "end_at": at(end), "all_day": allDay,
		"organizer_email": googleEmail(organizer["email"]), "creator_email": googleEmail(creator["email"]),
		"recurring_event_id": e["recurringEventId"], "html_link": e["htmlLink"],
		"created_at": googleTime(e["created"]), "updated_at": googleTime(e["updated"]), "attendee_count": len(people),
		"attendees_omitted": e["attendeesOmitted"] == true,
	}, people
}

// firstOf is the first item of a JSON list; nil for none.
func firstOf(v any) any {
	list, _ := v.([]any)
	if len(list) == 0 {
		return nil
	}
	return list[0]
}

// googleEmail is an email as rows hold it, in lower case, so tables join on
// it; nil for none.
func googleEmail(v any) any {
	email, _ := v.(string)
	if email = strings.ToLower(strings.TrimSpace(email)); email == "" {
		return nil
	}
	return email
}

// googleTime is a time Google gives in RFC 3339, in UTC to the second; nil
// for none.
func googleTime(v any) any {
	s, _ := v.(string)
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}
