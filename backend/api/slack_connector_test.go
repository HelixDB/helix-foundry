package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/slack"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/helixdb/helix-foundry/backend/syncer"
)

// slackFeedFake is the store's side of Slack connections through Helix's
// Slack app and their feeds, in memory: what the API binds, switches,
// appends and reads.
type slackFeedFake struct {
	feeds       map[string]*model.SlackFeed // by connector
	progress    model.SlackBackfillProgress
	progressErr error
	binds       []model.SlackFeedBind
	switches    []model.SlackAppSwitch
	refusal     error
	// appended are the batches appended, by install; outcome answers an
	// install's append (appended when unset), appendErr fails them.
	appended  map[string][]model.SlackFeedBatch
	outcome   map[string]model.SlackAppendOutcome
	appendErr error
	limited   []string
	mappings  []*model.SlackThread
	mapErr    error
	others    []string
	bots      map[string]model.SlackInstallBotUpdate
}

func newSlackFeedFake() *slackFeedFake {
	return &slackFeedFake{feeds: map[string]*model.SlackFeed{}, appended: map[string][]model.SlackFeedBatch{},
		outcome: map[string]model.SlackAppendOutcome{}, bots: map[string]model.SlackInstallBotUpdate{}}
}

func (s *slackStore) GetConnector(ctx context.Context, ws, id string) (*model.Connector, error) {
	return s.conns.GetConnector(ctx, ws, id)
}
func (s *slackStore) CreateConnector(ctx context.Context, c *model.Connector) error {
	return s.conns.CreateConnector(ctx, c)
}
func (s *slackStore) UpdateConnector(ctx context.Context, ws, id string, u model.ConnectorUpdate) error {
	return s.conns.UpdateConnector(ctx, ws, id, u)
}
func (s *slackStore) ListConnectorTables(ctx context.Context, ws, id string) ([]*model.ConnectorTable, error) {
	return s.conns.ListConnectorTables(ctx, ws, id)
}
func (s *slackStore) ListConnectorSyncs(ctx context.Context, ws, id string, limit int) ([]*model.ConnectorSync, error) {
	return s.conns.ListConnectorSyncs(ctx, ws, id, limit)
}
func (s *slackStore) GetActiveConnectorSync(ctx context.Context, ws, id string) (*model.ConnectorSync, error) {
	return s.conns.GetActiveConnectorSync(ctx, ws, id)
}
func (s *slackStore) EnqueueConnectorSync(ctx context.Context, sync *model.ConnectorSync) error {
	return s.conns.EnqueueConnectorSync(ctx, sync)
}
func (s *slackStore) GetConnectorSync(ctx context.Context, ws, id string) (*model.ConnectorSync, error) {
	return s.conns.GetConnectorSync(ctx, ws, id)
}
func (s *slackStore) ListConnectorChangeBatches(ctx context.Context, ws, id string) ([]*model.ConnectorChangeBatch, error) {
	return s.conns.ListConnectorChangeBatches(ctx, ws, id)
}

func (s *slackStore) ListConnectors(_ context.Context, ws string) ([]*model.Connector, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	var out []*model.Connector
	for _, c := range s.conns.rows {
		if c.WorkspaceID == ws {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b *model.Connector) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (s *slackStore) GetFoundrySlackInstallByID(_ context.Context, id string) (*model.SlackInstall, error) {
	for _, i := range s.installs {
		if i.ID == id {
			c := *i
			return &c, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *slackStore) UpdateFoundrySlackInstallBot(_ context.Context, installID string, u model.SlackInstallBotUpdate) error {
	s.feed.bots[installID] = u
	for _, i := range s.installs {
		if i.ID == installID && i.RevokedAt == nil {
			if u.BotID != nil {
				i.BotID = *u.BotID
			}
			if u.InstallerAdmin != nil {
				i.InstallerAdmin = *u.InstallerAdmin
			}
			return nil
		}
	}
	return foundrystore.ErrNotFound
}

func (s *slackStore) GetFoundrySlackFeed(_ context.Context, connectorID string) (*model.SlackFeed, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	f, ok := s.feed.feeds[connectorID]
	if !ok {
		return nil, foundrystore.ErrNotFound
	}
	c := *f
	return &c, nil
}

func (s *slackStore) FoundrySlackBackfillProgress(_ context.Context, feedID string) (model.SlackBackfillProgress, error) {
	return s.feed.progress, s.feed.progressErr
}

// bindFeed binds connectorID's feed as the store would: to bind's install.
func (s *slackStore) bindFeed(ws, connectorID string, bind model.SlackFeedBind) {
	team := ""
	for _, i := range s.installs {
		if i.ID == bind.InstallID {
			team = i.TeamID
		}
	}
	s.feed.feeds[connectorID] = &model.SlackFeed{ID: "feed-" + connectorID, ConnectorID: connectorID, WorkspaceID: ws, TeamID: team,
		InstallID: bind.InstallID, NoticeVersion: bind.NoticeVersion, BoundBy: bind.BoundBy}
}

func (s *slackStore) CreateSlackAppConnector(ctx context.Context, c *model.Connector, bind model.SlackFeedBind) error {
	if err := bind.Validate(); err != nil {
		return err
	}
	if s.feed.refusal != nil {
		return s.feed.refusal
	}
	s.feed.binds = append(s.feed.binds, bind)
	c.Pushed = true
	s.bindFeed(c.WorkspaceID, c.ID, bind)
	return s.conns.CreateConnector(ctx, c)
}

func (s *slackStore) UpdateSlackAppConnector(ctx context.Context, ws, id string, update model.ConnectorUpdate, sw model.SlackAppSwitch) error {
	if s.feed.refusal != nil {
		return s.feed.refusal
	}
	if err := s.conns.UpdateConnector(ctx, ws, id, update); err != nil {
		return err
	}
	s.feed.switches = append(s.feed.switches, sw)
	row := s.conns.rows[id]
	var bind *model.SlackFeedBind
	switch sw := sw.(type) {
	case model.SlackAppBindNow:
		bind, row.Pushed = &sw.Bind, true
	case model.SlackAppBindStaged:
		bind, row.Pushed = &sw.Bind, false
	case model.SlackAppUnbind:
		row.Pushed = false
		delete(s.feed.feeds, id)
	}
	if bind != nil {
		if err := bind.Validate(); err != nil {
			return err
		}
		if f, ok := s.feed.feeds[id]; !ok || f.InstallID != bind.InstallID {
			s.feed.binds = append(s.feed.binds, *bind)
			s.bindFeed(ws, id, *bind)
		}
	}
	return nil
}

// slackStubKind stands in for the connector's Slack kind: the connector's
// own Prepare and Hint, and a Discover that records its sources. Settings
// naming Helix's app (not staged) read through its pushed variant.
type slackStubKind struct {
	poll    connector.Kind
	sources *[]connector.Source
	err     *error
}

func (k slackStubKind) Name() string        { return "slack" }
func (k slackStubKind) DisplayName() string { return "Slack" }
func (k slackStubKind) Prepare(src connector.Source) (json.RawMessage, error) {
	return k.poll.Prepare(src)
}
func (k slackStubKind) Hint(secret string) string { return k.poll.Hint(secret) }
func (k slackStubKind) Discover(_ context.Context, src connector.Source) (connector.Discovery, error) {
	*k.sources = append(*k.sources, src)
	if *k.err != nil {
		return connector.Discovery{}, *k.err
	}
	return connector.Discovery{Tables: []connector.Table{{Name: "messages", PrimaryKey: []string{"channel_id", "ts"}}}}, nil
}
func (k slackStubKind) Sync(context.Context, connector.Source, connector.Table, connector.RowWriter) (connector.Table, error) {
	return connector.Table{}, nil
}
func (k slackStubKind) For(settings json.RawMessage) (connector.Kind, error) {
	var s connector.SlackSettings
	if err := json.Unmarshal(settings, &s); err != nil {
		return nil, err
	}
	if syncer.SlackAppRead("slack", settings) {
		return slackStubApp{k}, nil
	}
	return k, nil
}

// slackStubApp is the app's variant: pushed.
type slackStubApp struct{ slackStubKind }

func (slackStubApp) Capture(connector.Source, string) connector.Capture {
	return connector.CaptureCursor
}
func (slackStubApp) Start(context.Context, connector.Source, []connector.Table) (connector.Start, error) {
	return connector.Start{}, nil
}
func (slackStubApp) Changes(context.Context, connector.Source, []connector.Table, json.RawMessage, connector.ChangeWriter) (json.RawMessage, error) {
	return nil, nil
}
func (slackStubApp) Backlog(context.Context, connector.Source, json.RawMessage) (connector.Backlog, error) {
	return connector.BacklogNone, nil
}

// slackAppFixture is a Slack fixture with the data connector on: its
// connectors' catalog, and an install that can bind (every scope, its bot
// id, a Slack admin's) unless a test changes it.
type slackAppFixture struct {
	slackFixture
	discovered  *[]connector.Source
	discoverErr *error
}

const slackToken = "xoxb-1234-5678-abcdef4f2a"

func newSlackAppFixture(t *testing.T) slackAppFixture {
	t.Helper()
	f := newSlackFixture(t)
	f.cfg.Foundry.Slack.Feed.Enabled = true
	poll, err := connector.NewCatalog(connector.DefaultEndpoints(), connector.Guard{}, connector.S3Options{}, connector.EventsOptions{}).Kind("slack")
	require.NoError(t, err)
	var sources []connector.Source
	var discoverErr error
	f.s.deps.ConnectorCatalog = connector.NewCatalogOf(slackStubKind{poll: poll, sources: &sources, err: &discoverErr})
	return slackAppFixture{slackFixture: f, discovered: &sources, discoverErr: &discoverErr}
}

// install adds the live install a feed binds to.
func (f slackAppFixture) install(t *testing.T) *model.SlackInstall {
	t.Helper()
	i := f.db.install(t, f.keys, model.SlackPerPerson{})
	i.Scopes = append(slices.Clone(slack.Scopes), slack.HistoryScopes...)
	i.BotID, i.InstallerAdmin = "B0BOT0001", true
	return i
}

// channels answers users.conversations with the bot's channels.
func (f slackAppFixture) channels() {
	f.slack.answer("users.conversations", http.StatusOK, `{"ok":true,"channels":[
		{"id":"C0GEN0001"},{"id":"C0PRIV0001","is_private":true},{"id":"C0OLD0001","is_archived":true},
		{"id":"C0EXT0001","is_ext_shared":true},{"id":"C0PEND0001","is_pending_ext_shared":true},{"id":"bad id"},{"id":"C0GEN0001"}],
		"response_metadata":{"next_cursor":""}}`)
}

func slackAppConfig(notice int32, days int32) *foundryv1.ConnectorConfig {
	return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Slack{Slack: &foundryv1.SlackConnectorConfig{HistoryDays: days,
		Credential: &foundryv1.SlackConnectorConfig_HelixApp{HelixApp: &foundryv1.SlackAppCredential{NoticeVersion: notice}}}}}
}

func slackTokenConfig(token string, days int32) *foundryv1.ConnectorConfig {
	return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Slack{Slack: &foundryv1.SlackConnectorConfig{HistoryDays: days,
		Credential: &foundryv1.SlackConnectorConfig_BotToken{BotToken: token}}}}
}

func (f slackAppFixture) create(ctx context.Context, cfg *foundryv1.ConnectorConfig) (*foundryv1.ConnectorResponse, error) {
	return f.s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: slackWS, Name: "Acme Slack", Config: cfg})
}

func TestCreateSlackAppConnector(t *testing.T) {
	t.Run("binds the install's readable channels, pushed", func(t *testing.T) {
		f := newSlackAppFixture(t)
		f.install(t)
		f.channels()
		resp, err := f.create(as("user-1"), slackAppConfig(slackFeedNotice, 30))
		require.NoError(t, err)
		c := resp.GetConnector()
		row := f.db.conns.rows[c.GetId()]
		assert.True(t, row.Pushed)
		assert.Nil(t, row.NextSyncAt, "it syncs once Slack sends")
		assert.Empty(t, row.SecretCiphertext)
		assert.JSONEq(t, `{"history_days":30,"credential":"helix_app","team_id":"T0AAA1111"}`, string(row.Config))
		assert.Equal(t, "user-1", *row.CreatedByUserID)
		require.Len(t, f.db.feed.binds, 1)
		assert.Equal(t, model.SlackFeedBind{InstallID: "inst-1", Scopes: f.s.slackRequired(), NoticeVersion: slackFeedNotice, BoundBy: "user-1",
			HistoryDays: 30, Channels: []string{"C0GEN0001", "C0PRIV0001"}}, f.db.feed.binds[0])
		call := f.slack.call("users.conversations")
		assert.Equal(t, "Bearer xoxb-live", call.auth, "through the install's token")
		src := (*f.discovered)[0]
		require.NotNil(t, src.SlackApp)
		assert.Equal(t, connector.SlackApp{InstallID: "inst-1", TeamID: slackTeam, BotUserID: "U0BOT0001", BotID: "B0BOT0001", AppID: "A0FOUNDRY",
			Caller: syncer.SlackBotCaller{BaseURL: f.slack.URL + "/api", Token: "xoxb-live"}}, *src.SlackApp)
		assert.Equal(t, int32(slackFeedNotice), c.GetConfig().GetSlack().GetHelixApp().GetNoticeVersion())
		assert.Equal(t, foundryv1.SlackFeedState_SLACK_FEED_STATE_LIVE, c.GetSlackStatus().GetState())
		assert.Zero(t, c.GetSchedule().GetEffectiveEverySeconds(), "pushed: no interval")
		assert.Len(t, resp.GetDiscovery().GetTables(), 1)
	})
	t.Run("refused before anything is written", func(t *testing.T) {
		for name, c := range map[string]struct {
			ctx    context.Context
			notice int32
			setup  func(f slackAppFixture)
			code   codes.Code
			reason string
		}{
			"off here": {as("user-1"), 1, func(f slackAppFixture) { f.cfg.Foundry.Slack.Feed.Enabled = false }, codes.FailedPrecondition,
				"FOUNDRY_SLACK_FEED_DISABLED"},
			"an agent":      {ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceMCP, UserID: "user-1"}), 1, nil, codes.PermissionDenied, ""},
			"no notice":     {as("user-1"), 0, nil, codes.FailedPrecondition, "FOUNDRY_SLACK_APP_NOTICE"},
			"an old notice": {as("user-1"), slackFeedNotice + 1, nil, codes.FailedPrecondition, "FOUNDRY_SLACK_APP_NOTICE"},
			"no install":    {as("user-1"), 1, func(f slackAppFixture) { f.db.installs = nil }, codes.FailedPrecondition, "FOUNDRY_SLACK_APP_NOT_INSTALLED"},
			"scopes": {as("user-1"), 1, func(f slackAppFixture) { f.db.installs[0].Scopes = slack.Scopes }, codes.FailedPrecondition,
				"FOUNDRY_SLACK_APP_SCOPES"},
			"a member's install": {as("user-1"), 1, func(f slackAppFixture) { f.db.installs[0].InstallerAdmin = false }, codes.FailedPrecondition,
				"FOUNDRY_SLACK_APP_ADMIN"},
			"no bot id": {as("user-1"), 1, func(f slackAppFixture) { f.db.installs[0].BotID = "" }, codes.FailedPrecondition,
				"FOUNDRY_SLACK_APP_BOT_UNKNOWN"},
			"no token":           {as("user-1"), 1, func(f slackAppFixture) { delete(f.db.tokens, "inst-1") }, codes.Unavailable, ""},
			"install unreadable": {as("user-1"), 1, func(f slackAppFixture) { f.db.fail = errors.New("down") }, codes.Internal, ""},
			"channels unlisted": {as("user-1"), 1, func(f slackAppFixture) {
				f.slack.answer("users.conversations", http.StatusOK, `{"ok":false,"error":"missing_scope"}`)
			}, codes.Unavailable, ""},
			"the test fails": {as("user-1"), 1, func(f slackAppFixture) { *f.discoverErr = &connector.InvalidError{Message: "no"} },
				codes.InvalidArgument, ""},
			"the store refuses": {as("user-1"), 1, func(f slackAppFixture) { f.db.feed.refusal = errors.New("down") }, codes.Internal, ""},
			"the workspace is gone": {as("user-1"), 1, func(f slackAppFixture) { f.db.feed.refusal = foundrystore.ErrNotFound },
				codes.NotFound, ""},
		} {
			f := newSlackAppFixture(t)
			f.install(t)
			f.channels()
			if c.setup != nil {
				c.setup(f)
			}
			_, err := f.create(c.ctx, slackAppConfig(c.notice, 30))
			assert.Equal(t, c.code, status.Code(err), name)
			reason, _ := slackReason(t, err)
			assert.Equal(t, c.reason, reason, name)
			assert.Empty(t, f.db.conns.rows, name)
		}
	})
	t.Run("the store's refusals, worded", func(t *testing.T) {
		for name, c := range map[string]struct {
			refusal model.SlackFeedRefusal
			reason  string
			msg     string
			ours    bool
		}{
			"in use": {model.SlackFeedRefusal{Reason: model.SlackFeedInUse, ConnectorID: "conn-old", WorkspaceID: slackWS},
				"FOUNDRY_SLACK_APP_IN_USE", "Old Slack already reads this Slack workspace", true},
			"in use, gone meanwhile": {model.SlackFeedRefusal{Reason: model.SlackFeedInUse, ConnectorID: "conn-gone"},
				"FOUNDRY_SLACK_APP_IN_USE", "Another connection already reads this Slack workspace", false},
			"the org, here": {model.SlackFeedRefusal{Reason: model.SlackFeedEnterprise, ConnectorID: "conn-old", WorkspaceID: slackWS},
				"FOUNDRY_SLACK_APP_ENTERPRISE", "Old Slack already reads a workspace of this Slack organisation", true},
			"the org, elsewhere": {model.SlackFeedRefusal{Reason: model.SlackFeedEnterprise, ConnectorID: "conn-old", WorkspaceID: "ws-other"},
				"FOUNDRY_SLACK_APP_ENTERPRISE", "Another Foundry workspace already reads a workspace of this Slack organisation", false},
			"raced off":   {model.SlackFeedRefusal{Reason: model.SlackFeedNotInstalled}, "FOUNDRY_SLACK_APP_NOT_INSTALLED", "", false},
			"raced scope": {model.SlackFeedRefusal{Reason: model.SlackFeedScopes}, "FOUNDRY_SLACK_APP_SCOPES", "", false},
			"raced admin": {model.SlackFeedRefusal{Reason: model.SlackFeedAdmin}, "FOUNDRY_SLACK_APP_ADMIN", "", false},
			"raced bot":   {model.SlackFeedRefusal{Reason: model.SlackFeedBotUnknown}, "FOUNDRY_SLACK_APP_BOT_UNKNOWN", "", false},
		} {
			f := newSlackAppFixture(t)
			f.install(t)
			f.channels()
			f.db.conns.rows["conn-old"] = &model.Connector{ID: "conn-old", WorkspaceID: slackWS, Kind: "slack", Name: "Old Slack"}
			f.db.feed.refusal = c.refusal
			_, err := f.create(as("user-1"), slackAppConfig(slackFeedNotice, 30))
			reason, meta := slackReason(t, err)
			assert.Equal(t, c.reason, reason, name)
			if c.msg != "" {
				assert.Equal(t, c.msg, status.Convert(err).Message(), name)
			}
			assert.Equal(t, c.ours, meta["connector_id"] == "conn-old", name)
		}
		f := newSlackAppFixture(t)
		err := f.s.slackFeedRefused(context.Background(), slackWS, model.SlackFeedRefusal{Reason: "unheard of"})
		assert.Equal(t, codes.Internal, status.Code(err))
	})
}

func TestTestSlackAppConnector(t *testing.T) {
	f := newSlackAppFixture(t)
	f.install(t)
	resp, err := f.s.TestConnector(as("user-1"), &foundryv1.TestConnectorRequest{WorkspaceId: slackWS, Config: slackAppConfig(0, 45)})
	require.NoError(t, err)
	assert.NotNil(t, resp.GetConfig().GetSlack().GetHelixApp())
	require.Len(t, *f.discovered, 1)
	assert.Equal(t, "inst-1", (*f.discovered)[0].SlackApp.InstallID)
	assert.JSONEq(t, `{"history_days":45,"credential":"helix_app","team_id":"T0AAA1111"}`, string((*f.discovered)[0].Settings))
	_, err = f.s.TestConnector(as("user-1"), &foundryv1.TestConnectorRequest{WorkspaceId: slackWS, Config: slackAppConfig(0, 91)})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "at most 90 days through the app")
	f.cfg.Foundry.Slack.Feed.Enabled = false
	_, err = f.s.TestConnector(as("user-1"), &foundryv1.TestConnectorRequest{WorkspaceId: slackWS, Config: slackAppConfig(0, 45)})
	reason, _ := slackReason(t, err)
	assert.Equal(t, "FOUNDRY_SLACK_FEED_DISABLED", reason)
}

// tokenConnector stores a Slack connection reading with a token.
func (f slackAppFixture) tokenConnector(t *testing.T, settings string) *model.Connector {
	t.Helper()
	sealed, keyID, err := f.keys.Seal([]byte(slackToken), connector.SecretContext("conn-1"))
	require.NoError(t, err)
	next := time.Now().Add(time.Hour)
	c := &model.Connector{ID: "conn-1", WorkspaceID: slackWS, Kind: "slack", Name: "Slack", Config: json.RawMessage(settings),
		SecretCiphertext: sealed, SecretKeyID: keyID, SecretHint: "xoxb…4f2a", NextSyncAt: &next}
	f.db.conns.rows[c.ID] = c
	return c
}

// appConnector stores a Slack connection through the app, bound to inst-1.
func (f slackAppFixture) appConnector(t *testing.T) *model.Connector {
	t.Helper()
	c := &model.Connector{ID: "conn-1", WorkspaceID: slackWS, Kind: "slack", Name: "Slack", Pushed: true,
		Config: json.RawMessage(`{"history_days":30,"credential":"helix_app","team_id":"T0AAA1111"}`)}
	f.db.conns.rows[c.ID] = c
	f.db.bindFeed(slackWS, c.ID, model.SlackFeedBind{InstallID: "inst-1", NoticeVersion: slackFeedNotice, BoundBy: "user-3"})
	return c
}

func (f slackAppFixture) update(ctx context.Context, cfg *foundryv1.ConnectorConfig, name *string) (*foundryv1.ConnectorResponse, error) {
	return f.s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: slackWS, Id: "conn-1", Config: cfg, Name: name})
}

func keptSlackConfig(days int32) *foundryv1.ConnectorConfig {
	return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Slack{Slack: &foundryv1.SlackConnectorConfig{HistoryDays: days}}}
}

func TestUpdateSlackConnector(t *testing.T) {
	t.Run("a token's switch is staged: it keeps syncing by its token", func(t *testing.T) {
		f := newSlackAppFixture(t)
		f.install(t)
		f.channels()
		c := f.tokenConnector(t, `{"history_days":90}`)
		resp, err := f.update(as("user-1"), slackAppConfig(slackFeedNotice, 60), nil)
		require.NoError(t, err)
		require.Len(t, f.db.feed.switches, 1)
		staged, ok := f.db.feed.switches[0].(model.SlackAppBindStaged)
		require.True(t, ok, "%T", f.db.feed.switches[0])
		assert.Equal(t, []string{"C0GEN0001", "C0PRIV0001"}, staged.Bind.Channels)
		row := f.db.conns.rows["conn-1"]
		assert.JSONEq(t, `{"history_days":60,"credential":"helix_app","team_id":"T0AAA1111","staged":true}`, string(row.Config))
		token, err := f.keys.Open(row.SecretCiphertext, row.SecretKeyID, connector.SecretContext("conn-1"))
		require.NoError(t, err)
		assert.Equal(t, slackToken, string(token), "the token stays")
		assert.Equal(t, "xoxb…4f2a", row.SecretHint)
		assert.Equal(t, c.NextSyncAt, row.NextSyncAt, "its schedule goes on")
		assert.Empty(t, f.db.conns.syncs, "no full read")
		assert.Equal(t, slackToken, (*f.discovered)[0].Secret, "tested by its token")
		assert.True(t, resp.GetConnector().GetSlackStatus().GetSwitching())
	})
	t.Run("switch now: the flip", func(t *testing.T) {
		f := newSlackAppFixture(t)
		f.install(t)
		f.tokenConnector(t, `{"history_days":60,"credential":"helix_app","team_id":"T0AAA1111","staged":true}`)
		f.db.bindFeed(slackWS, "conn-1", model.SlackFeedBind{InstallID: "inst-1", NoticeVersion: slackFeedNotice, BoundBy: "user-3"})
		cfg := slackAppConfig(slackFeedNotice, 60)
		cfg.GetSlack().GetHelixApp().Now = true
		_, err := f.update(as("user-1"), cfg, nil)
		require.NoError(t, err)
		now, ok := f.db.feed.switches[0].(model.SlackAppBindNow)
		require.True(t, ok)
		assert.Equal(t, "user-3", now.Bind.BoundBy, "the same feed: no new binding")
		assert.Empty(t, f.db.feed.binds)
		assert.Empty(t, f.slack.called(), "no channels listed again")
		row := f.db.conns.rows["conn-1"]
		assert.JSONEq(t, `{"history_days":60,"credential":"helix_app","team_id":"T0AAA1111"}`, string(row.Config))
		token, err := f.keys.Open(row.SecretCiphertext, row.SecretKeyID, connector.SecretContext("conn-1"))
		require.NoError(t, err)
		assert.Empty(t, token, "the token is gone")
		require.Len(t, f.db.conns.syncs, 1)
		assert.Equal(t, model.ConnectorSyncFull, f.db.conns.syncs[0].Mode)
		assert.NotNil(t, (*f.discovered)[0].SlackApp)
	})
	t.Run("back to a token", func(t *testing.T) {
		f := newSlackAppFixture(t)
		f.install(t)
		f.appConnector(t)
		_, err := f.update(as("user-1"), slackTokenConfig(" "+slackToken+" ", 200), nil)
		require.NoError(t, err)
		assert.Equal(t, []model.SlackAppSwitch{model.SlackAppUnbind{}}, f.db.feed.switches)
		row := f.db.conns.rows["conn-1"]
		assert.JSONEq(t, `{"history_days":200}`, string(row.Config))
		assert.Equal(t, "xoxb…4f2a", row.SecretHint)
		assert.Len(t, f.db.conns.syncs, 1, "read in full by its token")
		f.cfg.Foundry.Slack.Feed.Enabled = false
		_, err = f.update(as("user-1"), slackTokenConfig("xoxp-not-a-bot", 0), nil)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "tested as any token")
	})
	t.Run("kept: a rename alone changes nothing else", func(t *testing.T) {
		f := newSlackAppFixture(t)
		f.install(t)
		f.appConnector(t)
		name := "Our Slack"
		resp, err := f.update(as("user-1"), keptSlackConfig(30), &name)
		require.NoError(t, err)
		assert.Equal(t, "Our Slack", resp.GetConnector().GetName())
		assert.Empty(t, f.db.feed.switches)
		assert.Empty(t, f.db.conns.syncs)
		_, err = f.update(as("user-1"), slackTokenConfig("", 30), nil)
		require.NoError(t, err, "an empty token keeps the app too")
		assert.Empty(t, f.db.feed.switches)
	})
	t.Run("kept: more history reads in full from the same feed", func(t *testing.T) {
		f := newSlackAppFixture(t)
		f.install(t)
		f.appConnector(t)
		_, err := f.update(as("user-1"), keptSlackConfig(90), nil)
		require.NoError(t, err)
		now, ok := f.db.feed.switches[0].(model.SlackAppBindNow)
		require.True(t, ok)
		assert.Equal(t, 90, now.Bind.HistoryDays)
		assert.Len(t, f.db.conns.syncs, 1)
		_, err = f.update(as("user-1"), keptSlackConfig(91), nil)
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})
	t.Run("another Slack workspace binds again, with the notice", func(t *testing.T) {
		f := newSlackAppFixture(t)
		old := f.install(t)
		f.appConnector(t)
		now := time.Now()
		old.RevokedAt = &now
		i := f.install(t)
		i.ID, i.TeamID = "inst-2", "T0BBB2222"
		sealed, keyID, err := f.keys.Seal([]byte("xoxb-two"), "inst-2")
		require.NoError(t, err)
		f.db.tokens["inst-2"] = model.SlackSealedToken{Sealed: sealed, KeyID: keyID}
		f.channels()
		_, err = f.update(as("user-1"), keptSlackConfig(30), nil)
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_APP_NOTICE", reason, "kept, but bound again")
		_, err = f.update(as("user-1"), slackAppConfig(slackFeedNotice, 30), nil)
		require.NoError(t, err)
		now2, ok := f.db.feed.switches[0].(model.SlackAppBindNow)
		require.True(t, ok)
		assert.Equal(t, "inst-2", now2.Bind.InstallID)
		assert.Equal(t, "user-1", now2.Bind.BoundBy)
		assert.JSONEq(t, `{"history_days":30,"credential":"helix_app","team_id":"T0BBB2222"}`, string(f.db.conns.rows["conn-1"].Config))
	})
	t.Run("refused", func(t *testing.T) {
		f := newSlackAppFixture(t)
		f.install(t)
		f.appConnector(t)
		every := int64(3600)
		_, err := f.s.UpdateConnector(as("user-1"), &foundryv1.UpdateConnectorRequest{WorkspaceId: slackWS, Id: "conn-1", Config: keptSlackConfig(30),
			SyncEverySeconds: &every})
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "no interval")
		_, err = f.update(as("user-1"), stripeConfig("sk_test_1"), nil)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "another kind")
		blank := ""
		_, err = f.update(as("user-1"), keptSlackConfig(30), &blank)
		assert.Equal(t, codes.InvalidArgument, status.Code(err), "a blank name")
		_, err = f.update(as("user-1"), slackAppConfig(2, 30), nil)
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_APP_NOTICE", reason)
		f.db.conns.syncs = append(f.db.conns.syncs, &model.ConnectorSync{ID: "s1", ConnectorID: "conn-1", WorkspaceID: slackWS,
			Status: model.ConnectorSyncRunning})
		_, err = f.update(as("user-1"), keptSlackConfig(60), nil)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err), "a sync is live")
		_, err = f.update(as("user-1"), slackTokenConfig(slackToken, 60), nil)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err), "a sync is live")
		f.db.conns.syncs = nil
		f.db.feed.refusal = model.SlackFeedRefusal{Reason: model.SlackFeedScopes}
		_, err = f.update(as("user-1"), keptSlackConfig(60), nil)
		reason, _ = slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_APP_SCOPES", reason)
		f.db.feed.refusal = foundrystore.ErrNotFound
		_, err = f.update(as("user-1"), keptSlackConfig(60), nil)
		assert.Equal(t, codes.NotFound, status.Code(err))
		f.db.feed.refusal = errors.New("down")
		_, err = f.update(as("user-1"), keptSlackConfig(60), nil)
		assert.Equal(t, codes.Internal, status.Code(err))
		f.db.conns.rows["conn-1"].Config = json.RawMessage(`[]`)
		_, err = f.update(as("user-1"), keptSlackConfig(60), nil)
		assert.Equal(t, codes.Internal, status.Code(err), "settings unreadable")
	})
}

func TestSlackConnectorStatus(t *testing.T) {
	get := func(t *testing.T, f slackAppFixture) *foundryv1.Connector {
		t.Helper()
		resp, err := f.s.GetConnector(as("user-1"), &foundryv1.GetConnectorRequest{WorkspaceId: slackWS, Id: "conn-1"})
		require.NoError(t, err)
		return resp.GetConnector()
	}
	t.Run("live, its backfill under way", func(t *testing.T) {
		f := newSlackAppFixture(t)
		i := f.install(t)
		limited := time.Now().Add(-time.Hour)
		i.EventsLimitedAt = &limited
		f.appConnector(t)
		now := time.Now()
		f.db.feed.progress = model.SlackBackfillProgress{LossyDays: 2, Windows: []model.SlackBackfillWindow{
			{Status: model.SlackBackfillDone}, {Status: model.SlackBackfillQueued, Oldest: now.Add(-24 * time.Hour), Latest: now}}}
		c := get(t, f)
		st := c.GetSlackStatus()
		assert.Equal(t, foundryv1.SlackFeedState_SLACK_FEED_STATE_LIVE, st.GetState())
		assert.Equal(t, slackTeam, st.GetTeamId())
		assert.Equal(t, "Acme", st.GetTeamName())
		assert.Equal(t, int32(2), st.GetLossyDays())
		assert.Equal(t, int32(2), st.GetBackfill().GetChannels())
		assert.Equal(t, int32(1), st.GetBackfill().GetChannelsDone())
		assert.NotNil(t, st.GetEventsLimitedAt())
		assert.False(t, st.GetSwitching())
		assert.Equal(t, int32(slackFeedNotice), c.GetConfig().GetSlack().GetHelixApp().GetNoticeVersion())
		i.EventsLimitedAt = nil
		f.db.feed.progress = model.SlackBackfillProgress{}
		st = get(t, f).GetSlackStatus()
		assert.Nil(t, st.GetBackfill())
		assert.Nil(t, st.GetEventsLimitedAt())
	})
	t.Run("what it needs", func(t *testing.T) {
		f := newSlackAppFixture(t)
		i := f.install(t)
		f.appConnector(t)
		i.Scopes = slack.Scopes
		st := get(t, f).GetSlackStatus()
		assert.Equal(t, foundryv1.SlackFeedState_SLACK_FEED_STATE_NEEDS_REINSTALL, st.GetState())
		assert.Equal(t, slack.HistoryScopes, st.GetMissingScopes())

		removed := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
		i.RevokedAt = &removed
		f.cfg.Foundry.Slack.Feed.OrphanRetention = 7 * 24 * time.Hour
		st = get(t, f).GetSlackStatus()
		assert.Equal(t, foundryv1.SlackFeedState_SLACK_FEED_STATE_INSTALL_GONE, st.GetState())
		assert.Equal(t, removed.Add(7*24*time.Hour), st.GetDeletedAt().AsTime())

		other := f.install(t)
		other.ID, other.TeamID = "inst-2", "T0BBB2222"
		st = get(t, f).GetSlackStatus()
		assert.Equal(t, foundryv1.SlackFeedState_SLACK_FEED_STATE_TEAM_CHANGED, st.GetState())

		delete(f.db.feed.feeds, "conn-1")
		st = get(t, f).GetSlackStatus()
		assert.Equal(t, foundryv1.SlackFeedState_SLACK_FEED_STATE_INSTALL_GONE, st.GetState(), "its feed gone")
		assert.Equal(t, slackTeam, st.GetTeamId(), "from its settings")
	})
	t.Run("a token's has none; reads that fail", func(t *testing.T) {
		f := newSlackAppFixture(t)
		f.install(t)
		f.tokenConnector(t, `{"history_days":90}`)
		c := get(t, f)
		assert.Nil(t, c.GetSlackStatus())
		assert.Equal(t, "", c.GetConfig().GetSlack().GetBotToken())
		assert.Nil(t, c.GetConfig().GetSlack().GetHelixApp())

		f.appConnector(t)
		f.db.feed.progressErr = errors.New("down")
		_, err := f.s.GetConnector(as("user-1"), &foundryv1.GetConnectorRequest{WorkspaceId: slackWS, Id: "conn-1"})
		assert.Equal(t, codes.Internal, status.Code(err))
		f.db.feed.progressErr = foundrystore.ErrNotFound
		assert.Nil(t, get(t, f).GetSlackStatus().GetBackfill(), "a feed deleting meanwhile")
		f.db.installs[0].ID = "inst-gone"
		_, err = f.s.GetConnector(as("user-1"), &foundryv1.GetConnectorRequest{WorkspaceId: slackWS, Id: "conn-1"})
		assert.Equal(t, codes.Internal, status.Code(err), "its install unreadable")
		f.db.fail = errors.New("down")
		_, err = f.s.GetConnector(as("user-1"), &foundryv1.GetConnectorRequest{WorkspaceId: slackWS, Id: "conn-1"})
		assert.Equal(t, codes.Internal, status.Code(err))
	})
}

func TestSlackBackfillToProto(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	b := config.SlackBackfillConfig{HistoryPerMinute: 1, RepliesPerMinute: 1, PageLimit: 15, RepliesDays: 30}
	assert.Nil(t, slackBackfillToProto(model.SlackBackfillProgress{}, b, now), "nothing left")
	assert.Nil(t, slackBackfillToProto(model.SlackBackfillProgress{Windows: []model.SlackBackfillWindow{{Status: model.SlackBackfillDone},
		{Status: model.SlackBackfillFailed}}}, b, now), "nothing live")

	// 40 messages and 10 threads a day read over 10 days; 80 days left.
	read := model.SlackBackfillWindow{Status: model.SlackBackfillRunning, Oldest: now.Add(-90 * day), Latest: now.Add(-10 * day),
		Messages: 400, Threads: 100, CoveredS: int64((10 * day).Seconds())}
	got := slackBackfillToProto(model.SlackBackfillProgress{Windows: []model.SlackBackfillWindow{read}}, b, now)
	// 3,200 messages left: 214 pages; 20 days of them recent: 800 × ¼ = 200 threads.
	assert.Equal(t, int64(214*60), got.GetEtaSeconds())

	// A window not read yet takes the mean density; another feed's none.
	fresh := model.SlackBackfillWindow{Status: model.SlackBackfillQueued, Oldest: now.Add(-10 * day), Latest: now}
	got = slackBackfillToProto(model.SlackBackfillProgress{Windows: []model.SlackBackfillWindow{read, fresh}, QueuedThreads: 5}, b, now)
	// 214 + 27 pages; threads 200 + 400 × ¼ + 5 = 305.
	assert.Equal(t, int64(305*60), got.GetEtaSeconds())
	got = slackBackfillToProto(model.SlackBackfillProgress{Windows: []model.SlackBackfillWindow{fresh}}, b, now)
	assert.Equal(t, int64(60), got.GetEtaSeconds(), "a page")

	// Slack asked to wait past the next call.
	got = slackBackfillToProto(model.SlackBackfillProgress{Windows: []model.SlackBackfillWindow{fresh}, HistoryAt: now.Add(30 * time.Second),
		RepliesAt: now.Add(10 * time.Minute)}, b, now)
	assert.Equal(t, now.Add(10*time.Minute), got.GetWaitingUntil().AsTime())
	got = slackBackfillToProto(model.SlackBackfillProgress{Windows: []model.SlackBackfillWindow{fresh}, HistoryAt: now.Add(30 * time.Second)}, b, now)
	assert.Nil(t, got.GetWaitingUntil(), "a minute's pace isn't waiting")
}

func Example_slackBackfill() {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	// 12 channels of 90 days, 40 messages and 10 threads a day, at Tier 1.
	var windows []model.SlackBackfillWindow
	for range 12 {
		windows = append(windows, model.SlackBackfillWindow{Status: model.SlackBackfillRunning, Oldest: now.Add(-90 * day), Latest: now.Add(-day),
			Messages: 40, Threads: 10, CoveredS: int64(day.Seconds())})
	}
	got := slackBackfillToProto(model.SlackBackfillProgress{Windows: windows}, config.SlackBackfillConfig{HistoryPerMinute: 1, RepliesPerMinute: 1,
		PageLimit: 15, RepliesDays: 30}, now)
	fmt.Printf("%.1f days\n", time.Duration(got.GetEtaSeconds()*int64(time.Second)).Hours()/24)
	// Output: 2.4 days
}

func TestSlackReturnPath(t *testing.T) {
	for p, want := range map[string]string{"": "", "/settings/slack": "", "/data?add=slack": "/data?add=slack",
		"/data/connections/01JABCDEF": "/data/connections/01JABCDEF"} {
		got, ok := slackReturnPath(p)
		assert.True(t, ok, p)
		assert.Equal(t, want, got, p)
	}
	for _, p := range []string{"https://evil.example", "//evil.example", "/data/connections/", "/data/connections/a/b", "/data/connections/a?x=1",
		"/data/connections/" + strings.Repeat("a", 65), "/settings/billing", "/data?add=github"} {
		_, ok := slackReturnPath(p)
		assert.False(t, ok, p)
	}
}

func TestListConnectorKindsSlackSetup(t *testing.T) {
	setup := func(t *testing.T, f slackAppFixture, ctx context.Context) *foundryv1.SlackConnectorSetup {
		t.Helper()
		resp, err := f.s.ListConnectorKinds(ctx, &foundryv1.ListConnectorKindsRequest{WorkspaceId: slackWS})
		require.NoError(t, err)
		require.Len(t, resp.GetKinds(), 1)
		return resp.GetKinds()[0].GetSlack()
	}
	f := newSlackAppFixture(t)
	got := setup(t, f, as("user-1"))
	assert.Equal(t, int32(slackFeedNotice), got.GetNoticeVersion())
	assert.True(t, got.GetCanInstall())
	assert.Nil(t, got.GetInstall())
	assert.False(t, setup(t, f, as("user-2")).GetCanInstall(), "a member")
	assert.False(t, setup(t, f, ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceMCP, UserID: "user-1"})).GetCanInstall(), "an agent")

	i := f.install(t)
	i.InstallerAdmin, i.Scopes = false, slack.Scopes
	got = setup(t, f, as("user-1"))
	assert.False(t, got.GetInstallerAdmin())
	assert.Equal(t, slack.HistoryScopes, got.GetInstall().GetMissingScopes())
	assert.Empty(t, got.GetInUseByConnectorId())
	f.tokenConnector(t, `{"history_days":90}`)
	assert.Empty(t, setup(t, f, as("user-1")).GetInUseByConnectorId(), "a token's reads nothing through the app")
	f.appConnector(t)
	assert.Equal(t, "conn-1", setup(t, f, as("user-1")).GetInUseByConnectorId())

	f.db.fail = errors.New("down")
	_, err := f.s.ListConnectorKinds(as("user-1"), &foundryv1.ListConnectorKindsRequest{WorkspaceId: slackWS})
	assert.Equal(t, codes.Internal, status.Code(err))
	f.db.fail = nil
	f.cfg.Foundry.Slack.Feed.Enabled = false
	assert.Nil(t, setup(t, f, as("user-1")), "off here")
}

func TestStartFoundrySlackReinstall(t *testing.T) {
	f := newSlackAppFixture(t)
	start := func(returnPath string) (*foundryv1.StartFoundrySlackInstallResponse, error) {
		return f.s.StartFoundrySlackInstall(as("user-1"), &foundryv1.StartFoundrySlackInstallRequest{WorkspaceId: slackWS, ReturnPath: returnPath})
	}
	_, err := start("https://evil.example")
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	i := f.db.install(t, f.keys, model.SlackPerPerson{})
	resp, err := start("/data/connections/conn1")
	require.NoError(t, err, "an install lacking the history scopes installs again")
	u, err := url.Parse(resp.GetAuthorizeUrl())
	require.NoError(t, err)
	assert.Equal(t, strings.Join(f.s.slackRequired(), ","), u.Query().Get("scope"))
	assert.Equal(t, "/data/connections/conn1", f.db.states[hashToken(u.Query().Get("state"))].ReturnPath)
	i.Scopes = f.s.slackRequired()
	_, err = start("")
	reason, _ := slackReason(t, err)
	assert.Equal(t, "FOUNDRY_SLACK_INSTALLED", reason)
}

func TestFinishFoundrySlackLearnsTheBot(t *testing.T) {
	f := newSlackAppFixture(t)
	scopes := strings.Join(f.s.slackRequired(), ",")
	f.slack.answer("oauth.v2.access", http.StatusOK, grantJSON(func(g map[string]any) { g["scope"] = scopes }))
	f.slack.answer("auth.test", http.StatusOK, `{"ok":true,"bot_id":"B0BOT0001"}`)
	f.slack.answer("users.info", http.StatusOK, slackUserJSON(slackAda, "ada@acme.com", func(u map[string]any) { u["is_owner"] = true }))
	start, err := f.s.StartFoundrySlackInstall(as("user-1"), &foundryv1.StartFoundrySlackInstallRequest{WorkspaceId: slackWS,
		ReturnPath: "/data?add=slack"})
	require.NoError(t, err)
	u, _ := url.Parse(start.GetAuthorizeUrl())
	resp, err := f.s.FinishFoundrySlackInstall(as("user-1"), &foundryv1.FinishFoundrySlackInstallRequest{Code: "c", State: u.Query().Get("state")})
	require.NoError(t, err)
	assert.Equal(t, "/data?add=slack", resp.GetReturnPath())
	assert.True(t, resp.GetSlack().GetInstall().GetInstallerAdmin())
	assert.Empty(t, resp.GetSlack().GetInstall().GetMissingScopes())
	in := f.db.installs[0]
	assert.Equal(t, "B0BOT0001", in.BotID)
	assert.Equal(t, "Bearer xoxb-new", f.slack.call("auth.test").auth)
	user, _ := url.ParseQuery(string(f.slack.call("users.info").body))
	assert.Equal(t, slackAda, user.Get("user"))

	// Slack not saying keeps what the install had.
	f.slack.answer("auth.test", http.StatusOK, `{"ok":false,"error":"invalid_auth"}`)
	f.slack.answer("users.info", http.StatusOK, `{"ok":false,"error":"user_not_found"}`)
	_, err = f.s.StartFoundrySlackInstall(as("user-1"), &foundryv1.StartFoundrySlackInstallRequest{WorkspaceId: slackWS})
	require.Error(t, err, "nothing is missing now")
	in.Scopes = slack.Scopes
	start, err = f.s.StartFoundrySlackInstall(as("user-1"), &foundryv1.StartFoundrySlackInstallRequest{WorkspaceId: slackWS})
	require.NoError(t, err)
	u, _ = url.Parse(start.GetAuthorizeUrl())
	delete(f.db.feed.bots, in.ID)
	resp, err = f.s.FinishFoundrySlackInstall(as("user-1"), &foundryv1.FinishFoundrySlackInstallRequest{Code: "c", State: u.Query().Get("state")})
	require.NoError(t, err)
	assert.Empty(t, resp.GetReturnPath())
	assert.NotContains(t, f.db.feed.bots, in.ID, "nothing to record")
}

// slackCaller answers each users.conversations call with the next page.
type slackCaller struct {
	pages []string
	code  int
	err   error
	asked []url.Values
}

func (c *slackCaller) Call(_ context.Context, method string, query url.Values) (int, http.Header, []byte, error) {
	c.asked = append(c.asked, query)
	if c.err != nil {
		return 0, nil, nil, c.err
	}
	page := c.pages[0]
	c.pages = c.pages[1:]
	return max(c.code, http.StatusOK), nil, []byte(page), nil
}

func TestSlackReadable(t *testing.T) {
	ctx := context.Background()
	public := []string{"channels:read", "channels:history"}
	caller := &slackCaller{pages: []string{
		`{"ok":true,"channels":[{"id":"C1"},{"id":"G1","is_private":true}],"response_metadata":{"next_cursor":"p2"}}`,
		`{"ok":true,"channels":[{"id":"C2"}],"response_metadata":{"next_cursor":""}}`}}
	got, err := slackReadable(ctx, &connector.SlackApp{Caller: caller}, public)
	require.NoError(t, err)
	assert.Equal(t, []string{"C1", "C2"}, got, "a private channel's history needs groups:history")
	assert.Equal(t, "public_channel", caller.asked[0].Get("types"))
	assert.Equal(t, "p2", caller.asked[1].Get("cursor"))
	caller = &slackCaller{pages: []string{`{"ok":true,"channels":[{"id":"G1","is_private":true}]}`}}
	got, err = slackReadable(ctx, &connector.SlackApp{Caller: caller}, append(public, "groups:read", "groups:history"))
	require.NoError(t, err)
	assert.Equal(t, []string{"G1"}, got)
	assert.Equal(t, "public_channel,private_channel", caller.asked[0].Get("types"))
	for name, c := range map[string]*slackCaller{
		"unreachable": {err: errors.New("down")},
		"429":         {pages: []string{`{}`}, code: http.StatusTooManyRequests},
		"not JSON":    {pages: []string{`<html>`}},
		"refused":     {pages: []string{`{"ok":false,"error":"missing_scope"}`}},
	} {
		_, err := slackReadable(ctx, &connector.SlackApp{Caller: c}, public)
		assert.Error(t, err, name)
	}
	endless := &slackCaller{}
	for range slackDirectoryPages {
		endless.pages = append(endless.pages, `{"ok":true,"channels":[{"id":"C1"}],"response_metadata":{"next_cursor":"more"}}`)
	}
	got, err = slackReadable(ctx, &connector.SlackApp{Caller: endless}, public)
	require.NoError(t, err)
	assert.Equal(t, []string{"C1"}, got, "pages stop at the bound")
}

func TestAgentThreadSurface(t *testing.T) {
	for surface, want := range map[model.FoundryAgentSurface]foundryv1.FoundryAgentSurface{
		model.FoundryAgentSurfaceSite:  foundryv1.FoundryAgentSurface_FOUNDRY_AGENT_SURFACE_UNSPECIFIED,
		model.FoundryAgentSurfaceSlack: foundryv1.FoundryAgentSurface_FOUNDRY_AGENT_SURFACE_SLACK,
	} {
		got := agentThreadToProto(&model.FoundryAgentThread{ID: "th-1", Title: "Hi", Surface: surface})
		assert.Equal(t, want, got.GetSurface(), surface.String())
		assert.Equal(t, "th-1", got.GetId())
	}
}

func TestSlackInstallEventsLimited(t *testing.T) {
	f := newSlackAppFixture(t)
	i := f.install(t)
	recent, old := time.Now().Add(-time.Hour), time.Now().Add(-25*time.Hour)
	i.EventsLimitedAt = &recent
	assert.NotNil(t, f.s.slackInstallToProto(context.Background(), i).GetEventsLimitedAt())
	i.EventsLimitedAt = &old
	assert.Nil(t, f.s.slackInstallToProto(context.Background(), i).GetEventsLimitedAt(), "past a day")
}
