package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/filesource"
	"github.com/jackc/pgx/v5"
)

// Source is one connector's settings and its decrypted credential. Settings
// are the kind's JSON config and never hold the credential.
type Source struct {
	Settings json.RawMessage
	Secret   string
	// WorkspaceID owns the connector. Prepare ignores it; the s3 kind derives
	// the external ID its customer's role must require from it.
	WorkspaceID string
	// ForSync is set by the syncer only: Discover lists what a sync reads and
	// skips the previews a test shows.
	ForSync bool
	// ConnectorID is the stored connector's id; empty for a test of new settings.
	ConnectorID string
	// Captures are the capture methods the workspace's plan allows. A kind
	// with more than one way to follow a table takes the best one allowed.
	Captures []Capture
	// held is the transaction a PostgreSQL SourceSnapshot holds open: every
	// table read with this source is read on it, at its one snapshot.
	held pgx.Tx
	// SignIn is set when Secret is a token the host's broker vended for a
	// signed-in connector's account, not a key a person entered: Prepare
	// skips the key's format checks (and takes no Secret at all, to check
	// settings alone), and the kind sends it as the OAuth token it is.
	SignIn bool
}

func (s Source) Allows(c Capture) bool { return slices.Contains(s.Captures, c) }

// Kind is one type of data source.
type Kind interface {
	Name() string
	DisplayName() string
	// Prepare checks the settings and credential without touching the network
	// and returns the settings to store, with defaults applied.
	Prepare(src Source) (json.RawMessage, error)
	// Discover proves the credential works and lists what it can read. It is
	// given prepared settings.
	Discover(ctx context.Context, src Source) (Discovery, error)
	// Hint identifies the credential to a person without revealing it.
	Hint(secret string) string
	// Sync writes every row of table, as Discover returned it, to w. It is
	// given prepared settings. It returns the table as written: a kind that
	// learns a table's columns and key while reading it returns them; the
	// others return the table they were given.
	Sync(ctx context.Context, src Source, table Table, w RowWriter) (Table, error)
}

// Targeted is a kind whose settings choose the server its credential is sent
// to. An edit keeps the stored credential only while the target is the same,
// so new settings cannot send it to a server it was never entered for.
type Targeted interface {
	// Target names where prepared settings send the credential; two settings
	// with equal targets send it to the same server.
	Target(settings json.RawMessage) (string, error)
}

// SnapshotKind is a kind that can read every table of a full read at one
// snapshot of its source, so the tables agree with each other: a change
// committed while they are read shows in all of them or in none.
type SnapshotKind interface {
	Kind
	// SourceSnapshot holds one snapshot of the source open for the tables
	// read with the returned Source, one at a time, until its Close. It
	// takes no more of the source's connections than reading a table at a
	// time does. A source Start already bound to a snapshot keeps it. An
	// error holds nothing open: each table is then read at a moment of its
	// own, as by any other kind.
	SourceSnapshot(ctx context.Context, src Source) (SourceSnapshot, error)
}

// SourceSnapshot is the one moment of a source a full read's tables are read at.
type SourceSnapshot struct {
	// Source is what every table's Sync reads with, one table at a time.
	Source Source
	// Close releases the snapshot once the tables are read, on every path.
	// Never nil.
	Close func()
}

// Identifier is a kind that says who its credential reads as: for a person
// to recognise the account a signed-in connector reads with, and to tell
// whether signing in again changed it.
type Identifier interface {
	// Identity is the provider's name for the credential's identity, its
	// label (a GitHub login; a Linear user and their workspace), and a
	// stable id that tells one identity from another where labels repeat
	// (a GitHub user's number). An empty id is an identity unknown. Its
	// errors are the kind's, an *AuthError among them.
	Identity(ctx context.Context, src Source) (label, id string, err error)
}

// SecretContext binds a sealed credential to its connector row, so a
// ciphertext copied to another row does not open.
func SecretContext(connectorID string) string { return "connector:" + connectorID }

// RowWriter receives one table's rows.
type RowWriter interface {
	// Write appends a row; its keys become the table's columns.
	Write(row map[string]any) error
	// WriteJSON appends a row the source already encoded as one JSON object.
	WriteJSON(row []byte) error
}

// Discovery is what a source exposes to the credential.
type Discovery struct {
	Tables []Table
	// Unavailable are objects the source has but the connector cannot read.
	Unavailable []Unavailable
	// Settings replaces the stored settings when discovery decided a value,
	// such as the PostHog project; nil keeps them.
	Settings json.RawMessage
	// MaxBytes is a file source's limit on the stored bytes of the tables
	// one sync reads; 0 for other sources.
	MaxBytes int64
	// Warnings are settings that work but cost the customer, worded with the
	// fix. Only a test, not a sync, looks for them.
	Warnings []string
}

// Unavailable is an object the connector cannot read, and why when the source
// says; an empty Reason is a credential that may not read it.
type Unavailable struct {
	Name, Reason string
}

type Table struct {
	Name        string
	Columns     []Column
	PrimaryKey  []string
	ForeignKeys []ForeignKey
	// Files are a file source's objects; Sync reads exactly these. Like
	// Preview and Problem, they are never stored.
	Files []Object
	// Preview is up to five sample rows, each one JSON object, for a test.
	Preview []json.RawMessage
	// Problem is why a sample could not be read; the sync fails until it is fixed.
	Problem string
	// Windowed is a table that holds only a recent window of the source's
	// rows: a row leaving the window is not a delete.
	Windowed bool
	// Version names the column whose larger value marks the newer row: a
	// change read never replaces or deletes a stored row with an older one.
	Version string
	// Dense is a table whose every row carries every column, null when
	// absent: a change read's merge pads the stored rows to the columns the
	// read declares (ColumnWriter), and stores those columns.
	Dense bool
}

// WindowedTable reports whether a kind's table holds only a recent window
// of its source's rows (Table.Windowed): its change batches never delete a
// row that left the window.
func WindowedTable(kind, table string) bool {
	switch kind {
	case "posthog":
		return table == "events" || table == "daily_event_counts"
	case "github":
		return table == "pull_request_reviews"
	case "slack":
		return table == "messages" || table == "channel_days"
	case "google":
		return table == "calendar_events" || table == "event_attendees"
	}
	return false
}

// Object is one file of a file source's table.
type Object struct {
	Key    string
	Size   int64
	ETag   string
	Format filesource.Format
}

// Column and ForeignKey are also stored as JSON in connector_tables.
type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type,omitempty"`
	Nullable bool   `json:"nullable,omitempty"`
}

type ForeignKey struct {
	Column           string `json:"column"`
	ReferencedTable  string `json:"referenced_table"`
	ReferencedColumn string `json:"referenced_column"`
}

// InvalidError is a setting the customer must fix; nothing was dialed.
type InvalidError struct{ Message string }

func (e *InvalidError) Error() string { return e.Message }

func invalid(format string, args ...any) error {
	return &InvalidError{Message: fmt.Sprintf(format, args...)}
}

// RefusedError is the source refusing or failing the connection. Message is
// written for the customer and never carries the credential.
type RefusedError struct {
	Message string
	Err     error
}

func (e *RefusedError) Error() string {
	if e.Err == nil {
		return e.Message
	}
	return e.Message + ": " + e.Err.Error()
}

func (e *RefusedError) Unwrap() error { return e.Err }

func refused(err error, format string, args ...any) error {
	return &RefusedError{Message: fmt.Sprintf(format, args...), Err: err}
}

// AuthError is the provider rejecting the credential itself, not one
// request: GitHub's HTTP 401, Linear's AUTHENTICATION_ERROR. It rides in the
// RefusedError that words it for a key, so a caller that doesn't look for it
// reads a refusal; one that does (errors.As) asks a signed-in connector's
// broker for a fresh token, or says to sign it in again.
type AuthError struct {
	// Provider is the kind's display name.
	Provider string
}

func (e *AuthError) Error() string { return e.Provider + " rejected the credential" }

// rejectedCredential is the refusal of a credential provider rejected
// outright, worded for a key: an *AuthError, beside cause when the kind
// keeps one, so a signed-in connector's callers ask its broker for a fresh
// token or say to sign it in again.
func rejectedCredential(provider string, cause error, format string, args ...any) error {
	var err error = &AuthError{Provider: provider}
	if cause != nil {
		err = errors.Join(cause, err)
	}
	return refused(err, format, args...)
}

// Endpoints are the SaaS API roots; tests point them at local servers.
type Endpoints struct {
	Stripe string
	WorkOS string
	// PostHogUS and PostHogEU are the cloud regions; a custom host comes from settings.
	PostHogUS string
	PostHogEU string
	GitHub    string
	Linear    string
	Calcom    string
	Granola   string
	Slack     string
	Notion    string
	// Google serves Drive, Calendar and the Directory; GoogleToken is the
	// OAuth token endpoint a service account's assertion is exchanged at.
	Google      string
	GoogleToken string
	// S3 and STS replace AWS's own endpoints in tests; empty uses AWS's.
	S3  string
	STS string
	// GitHubAppInstall is where a signed-in GitHub connector's messages send
	// people to install Helix Foundry's GitHub App
	// (foundry.connectors.github_app_install_url); empty names no link.
	GitHubAppInstall string
}

func DefaultEndpoints() Endpoints {
	return Endpoints{
		Stripe:      "https://api.stripe.com",
		WorkOS:      "https://api.workos.com",
		PostHogUS:   "https://us.posthog.com",
		PostHogEU:   "https://eu.posthog.com",
		GitHub:      "https://api.github.com",
		Linear:      "https://api.linear.app",
		Calcom:      "https://api.cal.com",
		Granola:     "https://public-api.granola.ai",
		Slack:       "https://slack.com/api",
		Notion:      "https://api.notion.com",
		Google:      "https://www.googleapis.com",
		GoogleToken: "https://oauth2.googleapis.com/token",
	}
}

// Catalog holds every kind Foundry can connect, in display order.
type Catalog struct {
	kinds []Kind
}

// NewCatalog wires the kinds to the network: fixed SaaS hosts through http,
// customer-chosen hosts through guard, and event sources to what Helix holds
// of them through pushed. The events kind comes last, so the display order
// of every other kind stays as it was.
func NewCatalog(endpoints Endpoints, guard Guard, s3 S3Options, pushed EventsOptions) *Catalog {
	saas := &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirects}
	// PostHog's export mode reads the customer's bucket the way the s3 kind does.
	files := newS3Kind(endpoints, guard, s3)
	return &Catalog{kinds: []Kind{
		&stripeKind{base: endpoints.Stripe, http: saas},
		&workosKind{base: endpoints.WorkOS, http: saas},
		&posthogKind{us: endpoints.PostHogUS, eu: endpoints.PostHogEU, http: guard.HTTPClient(), files: files},
		&githubKind{base: endpoints.GitHub, http: &http.Client{Timeout: 30 * time.Second, CheckRedirect: withinHost}, install: endpoints.GitHubAppInstall},
		&linearKind{base: endpoints.Linear, http: saas},
		&calcomKind{base: endpoints.Calcom, http: saas},
		&granolaKind{base: endpoints.Granola, http: saas},
		&slackKind{base: endpoints.Slack, http: saas},
		&notionKind{base: endpoints.Notion, http: saas},
		&googleKind{base: endpoints.Google, tokenURL: endpoints.GoogleToken, http: saas},
		&postgresKind{guard: guard},
		&mysqlKind{guard: guard},
		files,
		&eventsKind{pushed},
	}}
}

// KindNames lists every kind's name in display order; the plan catalog checks its kinds against it.
func KindNames() []string {
	var names []string
	for _, k := range NewCatalog(Endpoints{}, Guard{}, S3Options{}, EventsOptions{}).Kinds() {
		names = append(names, k.Name())
	}
	return names
}

// NewCatalogOf holds exactly kinds, so tests can stand in for one.
func NewCatalogOf(kinds ...Kind) *Catalog { return &Catalog{kinds: kinds} }

func (c *Catalog) Kinds() []Kind { return slices.Clone(c.kinds) }

// ErrUnknownKind is a kind the catalog does not hold.
var ErrUnknownKind = errors.New("connector: unknown kind")

func (c *Catalog) Kind(name string) (Kind, error) {
	for _, k := range c.kinds {
		if k.Name() == name {
			return k, nil
		}
	}
	return nil, fmt.Errorf("%w %q", ErrUnknownKind, name)
}

// saasTable is a fixed API object exposed as a table keyed by key.
type saasTable struct {
	name string
	key  string
}

// saasDiscovery lists the readable objects with their keys and the foreign
// keys whose both ends are readable.
func saasDiscovery(objects []saasTable, readable func(name string) bool, foreignKeys map[string][]ForeignKey) Discovery {
	var d Discovery
	for _, o := range objects {
		if !readable(o.name) {
			d.Unavailable = append(d.Unavailable, Unavailable{Name: o.name})
			continue
		}
		t := Table{Name: o.name, PrimaryKey: []string{o.key}}
		for _, fk := range foreignKeys[o.name] {
			if readable(fk.ReferencedTable) {
				t.ForeignKeys = append(t.ForeignKeys, fk)
			}
		}
		d.Tables = append(d.Tables, t)
	}
	return d
}

// keyHint keeps a key's type prefix and last four characters, e.g. sk_live…4f2a.
func keyHint(key string, prefixParts int) string {
	if len(key) < 12 {
		return "…"
	}
	prefix, rest := "", key
	for i := 0; i < prefixParts; i++ {
		j := strings.IndexByte(rest, '_')
		if j < 0 {
			break
		}
		prefix += rest[:j+1]
		rest = rest[j+1:]
	}
	if len(prefix) > 0 {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + "…" + key[len(key)-4:]
}

// FormatBytes writes a size for a person: binary units named KB, MB, GB and
// TB as people expect, with at most one decimal.
func FormatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d bytes", n)
	}
	value, unit := float64(n), ""
	for _, u := range []string{"KB", "MB", "GB", "TB"} {
		if value < 1024 && unit != "" {
			break
		}
		value, unit = value/1024, u
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", value), ".0") + " " + unit
}
