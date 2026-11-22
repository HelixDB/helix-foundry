package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	slackAppSettings    = `{"history_days":30,"credential":"helix_app","team_id":"T1"}`
	slackStagedSettings = `{"history_days":30,"credential":"helix_app","team_id":"T1","staged":true}`
)

func TestSlackAppRead(t *testing.T) {
	for _, c := range []struct {
		kind, settings string
		want           bool
	}{
		{"slack", slackAppSettings, true},
		{"slack", slackStagedSettings, false},
		{"slack", `{"history_days":30}`, false},
		{"slack", `not json`, false},
		{"stripe", slackAppSettings, false},
	} {
		if got := SlackAppRead(c.kind, json.RawMessage(c.settings)); got != c.want {
			t.Errorf("SlackAppRead(%s, %s) = %v", c.kind, c.settings, got)
		}
	}
}

func TestSlackAppStateOf(t *testing.T) {
	feed := &model.SlackFeed{InstallID: "inst-1", TeamID: "T1"}
	required := []string{"chat:write", "channels:history"}
	live := func(id, team string, scopes ...string) *model.SlackInstall {
		return &model.SlackInstall{ID: id, TeamID: team, Scopes: scopes}
	}
	for name, c := range map[string]struct {
		live    *model.SlackInstall
		want    SlackAppState
		missing []string
	}{
		"live":                 {live("inst-1", "T1", "chat:write", "channels:history", "extra"), SlackAppLive, nil},
		"needs reinstall":      {live("inst-1", "T1", "chat:write"), SlackAppNeedsReinstall, []string{"channels:history"}},
		"no install":           {nil, SlackAppInstallGone, nil},
		"another team":         {live("inst-2", "T2"), SlackAppTeamChanged, nil},
		"same team, not moved": {live("inst-2", "T1"), SlackAppInstallGone, nil},
	} {
		state, missing := SlackAppStateOf(feed, c.live, required)
		if state != c.want || fmt.Sprint(missing) != fmt.Sprint(c.missing) {
			t.Errorf("%s: SlackAppStateOf = %v, %v; want %v, %v", name, state, missing, c.want, c.missing)
		}
	}
	for state, name := range map[SlackAppState]string{SlackAppLive: "live", SlackAppNeedsReinstall: "needs_reinstall",
		SlackAppInstallGone: "install_gone", SlackAppTeamChanged: "team_changed", 0: "SlackAppState(0)"} {
		if state.String() != name {
			t.Errorf("%d.String() = %q, want %q", state, state.String(), name)
		}
	}
}

func ExampleSlackAppStateOf() {
	feed := &model.SlackFeed{InstallID: "inst-1", TeamID: "T1"}
	live := &model.SlackInstall{ID: "inst-1", TeamID: "T1", Scopes: []string{"chat:write"}}
	fmt.Println(SlackAppStateOf(feed, live, []string{"chat:write", "channels:history"}))
	fmt.Println(SlackAppStateOf(feed, &model.SlackInstall{ID: "inst-2", TeamID: "T2"}, nil))
	fmt.Println(SlackAppStateOf(feed, nil, nil))
	// Output:
	// needs_reinstall [channels:history]
	// team_changed []
	// install_gone []
}

func ExampleSlackAppGoneMessage() {
	removed := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	fmt.Println(SlackAppGoneMessage(&model.SlackInstall{TeamName: "Acme", RevokedAt: &removed}, 7*24*time.Hour))
	fmt.Println(SlackAppGoneMessage(&model.SlackInstall{TeamName: "Acme"}, 7*24*time.Hour))
	// Output:
	// Helix Foundry was removed from Acme: add it again in Settings › Slack, or switch this connection to a bot token. Its data is frozen as of its last sync and is deleted on 28 November 2026.
	// Helix Foundry was removed from Acme: add it again in Settings › Slack, or switch this connection to a bot token. Its data is frozen as of its last sync.
}

// slackAppDB is the store HostedSlackApps reads: one feed, the installs,
// and their sealed tokens; err fails the read it names.
type slackAppDB struct {
	feed     *model.SlackFeed
	installs []*model.SlackInstall
	tokens   map[string]model.SlackSealedToken
	err      map[string]error
}

func (d *slackAppDB) GetFoundrySlackFeed(_ context.Context, connectorID string) (*model.SlackFeed, error) {
	if err := d.err["feed"]; err != nil {
		return nil, err
	}
	if d.feed == nil || d.feed.ConnectorID != connectorID {
		return nil, foundrystore.ErrNotFound
	}
	return d.feed, nil
}

func (d *slackAppDB) GetFoundrySlackInstall(_ context.Context, workspaceID string) (*model.SlackInstall, error) {
	if err := d.err["live"]; err != nil {
		return nil, err
	}
	for _, i := range d.installs {
		if i.WorkspaceID == workspaceID && i.RevokedAt == nil {
			return i, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (d *slackAppDB) GetFoundrySlackInstallByID(_ context.Context, id string) (*model.SlackInstall, error) {
	if err := d.err["byID"]; err != nil {
		return nil, err
	}
	for _, i := range d.installs {
		if i.ID == id {
			return i, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (d *slackAppDB) GetFoundrySlackInstallToken(_ context.Context, installID string) (model.SlackSealedToken, error) {
	t, ok := d.tokens[installID]
	if !ok {
		return model.SlackSealedToken{}, foundrystore.ErrNotFound
	}
	return t, nil
}

func TestHostedSlackApps(t *testing.T) {
	ctx := context.Background()
	keys, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{3}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	removed := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	setup := func() (*slackAppDB, HostedSlackApps) {
		sealed, keyID, err := keys.Seal([]byte("xoxb-live"), "inst-1")
		if err != nil {
			t.Fatal(err)
		}
		db := &slackAppDB{
			feed: &model.SlackFeed{ID: "feed-1", ConnectorID: "conn-1", WorkspaceID: "ws-1", TeamID: "T1", InstallID: "inst-1"},
			installs: []*model.SlackInstall{{ID: "inst-1", WorkspaceID: "ws-1", TeamID: "T1", TeamName: "Acme", BotUserID: "U0BOT", BotID: "B0BOT",
				Scopes: []string{"chat:write", "channels:history"}}},
			tokens: map[string]model.SlackSealedToken{"inst-1": {Sealed: sealed, KeyID: keyID}}, err: map[string]error{},
		}
		return db, HostedSlackApps{DB: db, Keys: keys, AppID: "A0FOUNDRY", Required: []string{"chat:write", "channels:history"},
			OrphanRetention: 7 * 24 * time.Hour, BaseURL: "https://slack.test/api"}
	}
	permanent := func(t *testing.T, err error, words string) {
		t.Helper()
		var f *Failure
		if !errors.As(err, &f) || !f.Permanent || !strings.Contains(f.Message, words) {
			t.Fatalf("error = %v, want a permanent failure saying %q", err, words)
		}
	}

	t.Run("live", func(t *testing.T) {
		_, apps := setup()
		app, err := apps.SlackAppSource(ctx, "ws-1", "conn-1")
		if err != nil {
			t.Fatal(err)
		}
		caller, _ := app.Caller.(SlackBotCaller)
		if app.InstallID != "inst-1" || app.TeamID != "T1" || app.BotUserID != "U0BOT" || app.BotID != "B0BOT" || app.AppID != "A0FOUNDRY" ||
			caller.Token != "xoxb-live" || caller.BaseURL != "https://slack.test/api" {
			t.Fatalf("app = %+v", app)
		}
	})
	t.Run("permanent", func(t *testing.T) {
		db, apps := setup()
		db.feed.ConnectorID = "conn-2"
		_, err := apps.SlackAppSource(ctx, "ws-1", "conn-1")
		permanent(t, err, "no longer reads a Slack workspace")

		db, apps = setup()
		db.installs[0].Scopes = []string{"chat:write"}
		_, err = apps.SlackAppSource(ctx, "ws-1", "conn-1")
		permanent(t, err, "new Slack permissions to sync channels (channels:history)")

		db, apps = setup()
		db.installs[0].RevokedAt = &removed
		db.installs = append(db.installs, &model.SlackInstall{ID: "inst-2", WorkspaceID: "ws-1", TeamID: "T2", TeamName: "Globex"})
		_, err = apps.SlackAppSource(ctx, "ws-1", "conn-1")
		permanent(t, err, "this workspace's Slack is now Globex")

		db, apps = setup()
		db.installs[0].RevokedAt = &removed
		_, err = apps.SlackAppSource(ctx, "ws-1", "conn-1")
		permanent(t, err, "removed from Acme")
		permanent(t, err, "deleted on 27 November 2026")
	})
	t.Run("retried", func(t *testing.T) {
		broken := errors.New("store down")
		for _, read := range []string{"feed", "live", "byID"} {
			db, apps := setup()
			db.err[read] = broken
			if read == "byID" {
				db.installs[0].RevokedAt = &removed
			}
			_, err := apps.SlackAppSource(ctx, "ws-1", "conn-1")
			var f *Failure
			if !errors.Is(err, broken) || errors.As(err, &f) {
				t.Errorf("%s failing: error = %v, want the store's", read, err)
			}
		}
		db, apps := setup()
		delete(db.tokens, "inst-1")
		if _, err := apps.SlackAppSource(ctx, "ws-1", "conn-1"); !errors.Is(err, foundrystore.ErrNotFound) {
			t.Errorf("no token: error = %v", err)
		}
		db, apps = setup()
		db.tokens["inst-1"] = model.SlackSealedToken{Sealed: []byte("not sealed"), KeyID: "k1"}
		if _, err := apps.SlackAppSource(ctx, "ws-1", "conn-1"); err == nil || !strings.Contains(err.Error(), "open Slack install inst-1 token") {
			t.Errorf("a token that doesn't open: error = %v", err)
		}
	})
}

func TestSlackBotCaller(t *testing.T) {
	ctx := context.Background()
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.String()+" "+r.Header.Get("Authorization"))
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
	}))
	defer server.Close()
	caller := SlackBotCaller{BaseURL: server.URL + "/api/", Token: "xoxb-1"}
	code, header, body, err := caller.Call(ctx, "conversations.history", url.Values{"channel": {"C1"}})
	if err != nil || code != http.StatusTooManyRequests || header.Get("Retry-After") != "7" || string(body) != `{"ok":false,"error":"ratelimited"}` {
		t.Fatalf("Call = %d, %v, %s, %v", code, header, body, err)
	}
	if _, _, _, err := caller.Call(ctx, "auth.test", nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /api/conversations.history?channel=C1 Bearer xoxb-1", "GET /api/auth.test Bearer xoxb-1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("requests = %q", got)
	}
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte(`{"ok":`))
	}))
	defer short.Close()
	if _, _, _, err := (SlackBotCaller{BaseURL: short.URL}).Call(ctx, "auth.test", nil); err == nil {
		t.Error("an answer cut short: no error")
	}
	if _, _, _, err := (SlackBotCaller{BaseURL: "http://a b"}).Call(ctx, "auth.test", nil); err == nil {
		t.Error("a base URL that isn't one: no error")
	}
	server.Close()
	if _, _, _, err := caller.Call(ctx, "auth.test", nil); err == nil {
		t.Error("a Slack that doesn't answer: no error")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, _, err := (SlackBotCaller{Token: "xoxb-1"}).Call(cancelled, "auth.test", nil); err == nil {
		t.Error("slack.com, cancelled: no error")
	}
}

// slackApps answers SlackAppSource with app or err, recording who asked.
type slackApps struct {
	app   *connector.SlackApp
	err   error
	asked []string
}

func (s *slackApps) SlackAppSource(_ context.Context, workspaceID, connectorID string) (*connector.SlackApp, error) {
	s.asked = append(s.asked, workspaceID+"/"+connectorID)
	return s.app, s.err
}

// slackVariant is a Slack kind whose settings choose between a token kind
// and the app's, as the connector's does.
type slackVariant struct {
	token, app *fakeKind
}

func (v slackVariant) Name() string        { return "slack" }
func (v slackVariant) DisplayName() string { return "Slack" }
func (v slackVariant) Prepare(src connector.Source) (json.RawMessage, error) {
	return src.Settings, nil
}
func (v slackVariant) Hint(string) string { return "" }
func (v slackVariant) Discover(ctx context.Context, src connector.Source) (connector.Discovery, error) {
	return v.token.Discover(ctx, src)
}
func (v slackVariant) Sync(ctx context.Context, src connector.Source, table connector.Table, w connector.RowWriter) (connector.Table, error) {
	return v.token.Sync(ctx, src, table, w)
}

func (v slackVariant) For(settings json.RawMessage) (connector.Kind, error) {
	var s connector.SlackSettings
	if err := json.Unmarshal(settings, &s); err != nil {
		return nil, err
	}
	if SlackAppRead("slack", settings) {
		return slackNamed{v.app}, nil
	}
	return slackNamed{v.token}, nil
}

// slackNamed is a fakeKind named slack.
type slackNamed struct{ *fakeKind }

func (slackNamed) Name() string { return "slack" }

func TestSyncerSlackApp(t *testing.T) {
	ctx := context.Background()
	newSlackFixture := func(t *testing.T, settings string, apps SlackApps) (fixture, slackVariant) {
		t.Helper()
		kinds := slackVariant{token: signedInKind(), app: signedInKind()}
		f := newFixture(t, "http://unused", "slack", settings, nil)
		f.syncer.Catalog = connector.NewCatalogOf(kinds)
		f.syncer.PlanCatalog = catalog(t)
		f.syncer.SlackApps = apps
		return f, kinds
	}
	t.Run("reads through the install", func(t *testing.T) {
		apps := &slackApps{app: &connector.SlackApp{InstallID: "inst-1", TeamID: "T1"}}
		f, kinds := newSlackFixture(t, slackAppSettings, apps)
		f.store.connectors["conn-1"].SecretCiphertext, f.store.connectors["conn-1"].SecretKeyID = []byte{}, ""
		if _, err := f.syncer.Run(ctx, f.sync); err != nil {
			t.Fatal(err)
		}
		src := kinds.app.sources[0]
		if src.SlackApp == nil || src.SlackApp.InstallID != "inst-1" || src.Secret != "" || len(kinds.token.sources) != 0 ||
			fmt.Sprint(apps.asked) != "[ws-1/conn-1]" {
			t.Fatalf("app source = %+v, token reads %d, asked %v", src, len(kinds.token.sources), apps.asked)
		}
	})
	t.Run("a staged switch reads by its token", func(t *testing.T) {
		apps := &slackApps{}
		f, kinds := newSlackFixture(t, slackStagedSettings, apps)
		if _, err := f.syncer.Run(ctx, f.sync); err != nil {
			t.Fatal(err)
		}
		if src := kinds.token.sources[0]; src.Secret != "sk_test_abc" || src.SlackApp != nil || len(apps.asked) != 0 || len(kinds.app.sources) != 0 {
			t.Fatalf("token source = %+v, asked %v", src, apps.asked)
		}
	})
	t.Run("keeps a permanent failure of its install", func(t *testing.T) {
		gone := &Failure{Message: "Helix Foundry was removed from Acme", Permanent: true}
		f, _ := newSlackFixture(t, slackAppSettings, &slackApps{err: gone})
		_, err := f.syncer.Run(ctx, f.sync)
		if got := failure(t, err); got != gone {
			t.Fatalf("failure = %+v, want the install's", got)
		}
	})
	t.Run("retries a store failure", func(t *testing.T) {
		f, _ := newSlackFixture(t, slackAppSettings, &slackApps{err: errors.New("store down")})
		_, err := f.syncer.Run(ctx, f.sync)
		if got := failure(t, err); got.Permanent || !strings.Contains(got.Error(), "store down") {
			t.Fatalf("failure = %+v", got)
		}
	})
	t.Run("fails for good where the app isn't read", func(t *testing.T) {
		f, _ := newSlackFixture(t, slackAppSettings, nil)
		_, err := f.syncer.Run(ctx, f.sync)
		if got := failure(t, err); !got.Permanent || !strings.Contains(got.Message, "isn't available here") {
			t.Fatalf("failure = %+v", got)
		}
	})
	t.Run("settings no kind reads", func(t *testing.T) {
		f, _ := newSlackFixture(t, `not json`, nil)
		_, err := f.syncer.Run(ctx, f.sync)
		if got := failure(t, err); !got.Permanent || got.Message != "this connector's settings can't be read" {
			t.Fatalf("failure = %+v", got)
		}
		f.store.connectors["conn-1"].Kind = "fax"
		_, err = f.syncer.Run(ctx, f.sync)
		if got := failure(t, err); !got.Permanent || got.Message != "this connector kind is no longer supported" {
			t.Fatalf("failure = %+v", got)
		}
		if err := f.syncer.Release(ctx, f.store.connectors["conn-1"]); !errors.Is(err, connector.ErrUnknownKind) {
			t.Fatalf("Release of an unknown kind = %v", err)
		}
	})
}
