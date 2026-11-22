package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/slack"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/helixdb/helix-foundry/backend/syncer"
)

// The Slack data connector on Helix's Slack app (foundry.slack.feed): a
// Slack connection that reads through the workspace's install of the app,
// bound to it by an owner or admin who accepts the storage notice, and a
// token connection's staged switch to it.

// slackFeedNotice is the storage notice an owner or admin accepts to bind
// a Slack connection to Helix's Slack app: what Helix keeps of the
// channels, and for how long. A new wording takes the next number, and
// binds refuse the old one.
const slackFeedNotice = 1

// slackNoticeMissing refuses a binding without the current storage notice.
const slackNoticeMissing = "Read and accept Helix Foundry's current storage notice to sync Slack through its app"

// slackEventsLimitedShown is how long after Slack last dropped a team's
// events the site says so.
const slackEventsLimitedShown = 24 * time.Hour

// slackDirectoryPages bounds the users.conversations pages a bind reads:
// 200 channels a page.
const slackDirectoryPages = 100

// slackConfig is foundry.slack; off without a configuration.
func (s *Service) slackConfig() config.SlackConfig {
	if s.deps.Cfg == nil {
		return config.SlackConfig{}
	}
	return s.deps.Cfg.Foundry.Slack
}

// slackRequired is the scopes an install must hold under foundry.slack's
// flags (slack.Required).
func (s *Service) slackRequired() []string {
	cfg := s.slackConfig()
	scopes, _ := slack.Required(cfg.Feed.Enabled, cfg.Follow.Enabled)
	return scopes
}

// slackInstallToProto is an install as Settings › Slack and the Slack
// connection's form show it, with the scopes it lacks under the stamp's
// flags.
func (s *Service) slackInstallToProto(ctx context.Context, install *model.SlackInstall) *foundryv1.FoundrySlackInstall {
	out := &foundryv1.FoundrySlackInstall{
		TeamId: install.TeamID, TeamName: install.TeamName, EnterpriseId: install.EnterpriseID, BotUserId: install.BotUserID,
		InstalledByUserId: install.InstalledByUserID, InstalledByName: s.slackUserName(ctx, install.InstalledByUserID),
		CreatedAt: timestamppb.New(install.CreatedAt), UpdatedAt: timestamppb.New(install.UpdatedAt), InstallerAdmin: install.InstallerAdmin,
	}
	for _, scope := range s.slackRequired() {
		if !slices.Contains(install.Scopes, scope) {
			out.MissingScopes = append(out.MissingScopes, scope)
		}
	}
	if at := install.EventsLimitedAt; at != nil && time.Since(*at) < slackEventsLimitedShown {
		out.EventsLimitedAt = timestamppb.New(*at)
	}
	return out
}

// slackFeedRefused words a refused binding for the person: FAILED_PRECONDITION,
// reason FOUNDRY_SLACK_APP_<REASON>, with the connection in the way
// (connector_id) when it is this workspace's.
func (s *Service) slackFeedRefused(ctx context.Context, workspaceID string, r model.SlackFeedRefusal) error {
	ours := ""
	if r.ConnectorID != "" && (r.WorkspaceID == "" || r.WorkspaceID == workspaceID) {
		if c, err := s.deps.DB.GetConnector(ctx, workspaceID, r.ConnectorID); err == nil {
			ours = c.Name
		}
	}
	var reason, msg string
	switch r.Reason {
	case model.SlackFeedNotInstalled:
		reason, msg = "FOUNDRY_SLACK_APP_NOT_INSTALLED", "Add Helix Foundry to Slack from Settings › Slack first"
	case model.SlackFeedScopes:
		reason, msg = "FOUNDRY_SLACK_APP_SCOPES", "Helix Foundry needs new Slack permissions to sync channels: add it to Slack again from Settings › Slack"
	case model.SlackFeedAdmin:
		reason, msg = "FOUNDRY_SLACK_APP_ADMIN", "A Slack workspace admin or owner must add Helix Foundry to Slack before it can sync channels"
	case model.SlackFeedBotUnknown:
		reason, msg = "FOUNDRY_SLACK_APP_BOT_UNKNOWN",
			"Add Helix Foundry to Slack again from Settings › Slack, so it can tell its own messages from the channels'"
	case model.SlackFeedInUse:
		reason, msg = "FOUNDRY_SLACK_APP_IN_USE", "Another connection already reads this Slack workspace"
		if ours != "" {
			msg = ours + " already reads this Slack workspace"
		}
	case model.SlackFeedEnterprise:
		reason, msg = "FOUNDRY_SLACK_APP_ENTERPRISE", "Another Foundry workspace already reads a workspace of this Slack organisation"
		if ours != "" {
			msg = ours + " already reads a workspace of this Slack organisation"
		}
	default:
		return status.Errorf(codes.Internal, "bind the Slack connection: %v", r)
	}
	metadata := map[string]string{"workspace_id": workspaceID}
	if ours != "" {
		metadata["connector_id"] = r.ConnectorID
	}
	st, err := status.New(codes.FailedPrecondition, msg).WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: foundryDomain, Metadata: metadata})
	if err != nil {
		return status.Error(codes.FailedPrecondition, msg)
	}
	return st.Err()
}

// slackAppInstall is the workspace's live install a Slack connection binds
// to, with the source it reads through, refused as the store would refuse
// the binding while nothing is written yet: the connector off here
// (FOUNDRY_SLACK_FEED_DISABLED), no install, scopes missing, no Slack admin
// or owner's install, or a bot id unknown.
func (s *Service) slackAppInstall(ctx context.Context, workspaceID string) (*model.SlackInstall, *connector.SlackApp, error) {
	if cfg := s.slackConfig(); !cfg.Enabled || !cfg.Feed.Enabled {
		return nil, nil, foundryPrecondition(workspaceID, "FOUNDRY_SLACK_FEED_DISABLED",
			"Syncing Slack through Helix Foundry's app isn't available here: use your own Slack app's token")
	}
	install, err := s.deps.DB.GetFoundrySlackInstall(ctx, workspaceID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, nil, s.slackFeedRefused(ctx, workspaceID, model.SlackFeedRefusal{Reason: model.SlackFeedNotInstalled})
	case err != nil:
		return nil, nil, status.Errorf(codes.Internal, "read the Slack install: %v", err)
	}
	refusal := model.SlackFeedRefusalReason("")
	switch {
	case slices.ContainsFunc(s.slackRequired(), func(scope string) bool { return !slices.Contains(install.Scopes, scope) }):
		refusal = model.SlackFeedScopes
	case !install.InstallerAdmin:
		refusal = model.SlackFeedAdmin
	case install.BotID == "":
		refusal = model.SlackFeedBotUnknown
	}
	if refusal != "" {
		return nil, nil, s.slackFeedRefused(ctx, workspaceID, model.SlackFeedRefusal{Reason: refusal})
	}
	bot, err := s.slackBot(ctx, install.ID)
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "open the Slack install's token: %v", err)
	}
	return install, &connector.SlackApp{InstallID: install.ID, TeamID: install.TeamID, BotUserID: install.BotUserID, BotID: install.BotID,
		AppID: s.slackConfig().AppID, Caller: syncer.SlackBotCaller{BaseURL: s.slackURL() + "/api", Token: bot.Token}}, nil
}

// slackReadable lists the channels app's bot is in whose history its
// install may read (users.conversations): neither archived nor shared with
// another organisation, of a kind whose history scope it holds. It is the
// readable set a binding's backfill starts from; a sync covers the rest.
func slackReadable(ctx context.Context, app *connector.SlackApp, scopes []string) ([]string, error) {
	types := "public_channel"
	if slices.Contains(scopes, "groups:read") {
		types += ",private_channel"
	}
	var out []string
	cursor := ""
	for range slackDirectoryPages {
		query := url.Values{"types": {types}, "exclude_archived": {"true"}, "limit": {"200"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		code, _, body, err := app.Caller.Call(ctx, "users.conversations", query)
		if err != nil {
			return nil, fmt.Errorf("users.conversations: %w", err)
		}
		var page struct {
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
			Channels []struct {
				ID                 string `json:"id"`
				IsPrivate          bool   `json:"is_private"`
				IsArchived         bool   `json:"is_archived"`
				IsExtShared        bool   `json:"is_ext_shared"`
				IsPendingExtShared bool   `json:"is_pending_ext_shared"`
			} `json:"channels"`
			Metadata struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		switch {
		case code != http.StatusOK:
			return nil, fmt.Errorf("users.conversations answered %d", code)
		case json.Unmarshal(body, &page) != nil:
			return nil, errors.New("users.conversations answered what isn't JSON")
		case !page.OK:
			return nil, fmt.Errorf("users.conversations: %s", page.Error)
		}
		for _, ch := range page.Channels {
			history := "channels:history"
			if ch.IsPrivate {
				history = "groups:history"
			}
			if model.ValidSlackID(ch.ID) && !ch.IsArchived && !ch.IsExtShared && !ch.IsPendingExtShared && slices.Contains(scopes, history) &&
				!slices.Contains(out, ch.ID) {
				out = append(out, ch.ID)
			}
		}
		if cursor = page.Metadata.NextCursor; cursor == "" {
			return out, nil
		}
	}
	return out, nil
}

// slackBinding is the binding of a Slack connection to install: its
// readable channels, read through app now, in the order its backfill reads
// them.
func (s *Service) slackBinding(ctx context.Context, install *model.SlackInstall, app *connector.SlackApp, notice, historyDays int) (
	model.SlackFeedBind, error) {
	callCtx, cancel := context.WithTimeout(ctx, slackCallTimeout)
	defer cancel()
	channels, err := slackReadable(callCtx, app, install.Scopes)
	if err != nil {
		return model.SlackFeedBind{}, status.Errorf(codes.Unavailable, "Slack couldn't list the channels Helix Foundry is in just now (%v); try again", err)
	}
	return model.SlackFeedBind{InstallID: install.ID, Scopes: s.slackRequired(), NoticeVersion: notice, BoundBy: actor(ctx).UserID,
		HistoryDays: historyDays, Channels: channels}, nil
}

// slackSettingsOf reads a Slack connector's stored settings.
func slackSettingsOf(raw json.RawMessage) (connector.SlackSettings, error) {
	var s connector.SlackSettings
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, status.Errorf(codes.Internal, "read the Slack connection's settings: %v", err)
	}
	return s, nil
}

// slackNotice refuses a binding by anyone but a person signed in, or with
// a storage notice that isn't the current one (FOUNDRY_SLACK_APP_NOTICE).
func slackNotice(ctx context.Context, workspaceID string, notice int) error {
	if a := actor(ctx); a.Source != ports.SourceUser || a.UserID == "" {
		return status.Error(codes.PermissionDenied, "a Slack connection through Helix Foundry's app is made by a person signed in to Foundry")
	}
	if notice != slackFeedNotice {
		return foundryPrecondition(workspaceID, "FOUNDRY_SLACK_APP_NOTICE", slackNoticeMissing)
	}
	return nil
}

// createSlackAppConnector creates a Slack connection through Helix's Slack
// app (slackAppInstall's refusals, and slackNotice's): tested through the
// install, bound with its readable channels and pushed, in one
// transaction the store refuses as bindFoundrySlackFeed says.
func (s *Service) createSlackAppConnector(ctx context.Context, catalog *connector.Catalog, workspaceID, name string, historyDays int,
	notice int) (*foundryv1.ConnectorResponse, error) {
	if err := slackNotice(ctx, workspaceID, notice); err != nil {
		return nil, err
	}
	install, app, err := s.slackAppInstall(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(connector.SlackSettings{HistoryDays: historyDays, Credential: connector.SlackCredentialApp, TeamID: install.TeamID}) // plain fields
	kind, err := catalog.KindFor(model.ConnectorKindSlack, raw)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "connector kind slack is not available: %v", err)
	}
	settings, discovery, err := s.testSource(ctx, workspaceID, kind, connector.Source{Settings: raw, SlackApp: app})
	if err != nil {
		return nil, err
	}
	prepared, err := slackSettingsOf(settings)
	if err != nil {
		return nil, err
	}
	bind, err := s.slackBinding(ctx, install, app, notice, prepared.HistoryDays)
	if err != nil {
		return nil, err
	}
	userID := actor(ctx).UserID
	c := &model.Connector{ID: id.New(), WorkspaceID: workspaceID, Kind: model.ConnectorKindSlack, Name: name, Config: settings, CreatedByUserID: &userID}
	var refusal model.SlackFeedRefusal
	switch err := s.deps.DB.CreateSlackAppConnector(ctx, c, bind); {
	case errors.As(err, &refusal):
		return nil, s.slackFeedRefused(ctx, workspaceID, refusal)
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "workspace not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "create connector: %v", err)
	}
	stored, err := s.deps.DB.GetConnector(ctx, workspaceID, c.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read created connector: %v", err)
	}
	return s.connectorResponse(ctx, stored, &discovery)
}

// updateSlackConnector changes a Slack connection that reads, or is to
// read, through Helix's Slack app:
//   - a bot token: it reads with the token again; its feed is unbound and
//     it syncs in full;
//   - from a token (no helix_app.now): a staged switch. It keeps syncing by
//     its token, untouched, while the app's backfill reads its history;
//     the worker flips it once that is done;
//   - staged, with helix_app.now: the flip, at once;
//   - else it reads through the app now, syncing in full when its settings
//     changed. A binding to another install than its feed's (the
//     workspace's Slack changed) binds again, which needs the current
//     storage notice.
//
// Unset, or an empty bot_token, keeps the credential it has.
func (s *Service) updateSlackConnector(ctx context.Context, catalog *connector.Catalog, keyring *secretbox.Keyring, current *model.Connector,
	req *foundryv1.UpdateConnectorRequest) (*foundryv1.ConnectorResponse, error) {
	sl := req.GetConfig().GetSlack()
	if sl == nil {
		return nil, status.Error(codes.InvalidArgument, "a connector's kind cannot change; create a new connector")
	}
	was, err := slackSettingsOf(current.Config)
	if err != nil {
		return nil, err
	}
	ws := current.WorkspaceID
	var update model.ConnectorUpdate
	if req.Name != nil {
		name, err := connectorName(req.GetName())
		if err != nil {
			return nil, err
		}
		update.Name = &name
	}
	if req.SyncEverySeconds != nil {
		return nil, status.Error(codes.InvalidArgument, "a Slack connection through Helix Foundry's app syncs when Slack sends messages: it has no interval")
	}
	idle := func() error {
		switch _, err := s.deps.DB.GetActiveConnectorSync(ctx, ws, current.ID); {
		case err == nil:
			return status.Error(codes.FailedPrecondition, "a sync of this connector is queued or running; change its settings once the sync ends")
		case !errors.Is(err, foundrystore.ErrNotFound):
			return status.Errorf(codes.Internal, "read active sync: %v", err)
		}
		return nil
	}
	app := sl.GetHelixApp()
	if token := strings.TrimSpace(sl.GetBotToken()); app == nil && token != "" {
		raw, _ := json.Marshal(connector.SlackSettings{HistoryDays: int(sl.GetHistoryDays())}) // plain fields
		kind, err := catalog.KindFor(model.ConnectorKindSlack, raw)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "connector kind slack is not available: %v", err)
		}
		settings, discovery, err := s.testSource(ctx, ws, kind, connector.Source{Settings: raw, Secret: token})
		if err != nil {
			return nil, err
		}
		if err := idle(); err != nil {
			return nil, err
		}
		update.Config, update.SecretHint = settings, kind.Hint(token)
		if update.SecretCiphertext, update.SecretKeyID, err = keyring.Seal([]byte(token), connector.SecretContext(current.ID)); err != nil {
			return nil, status.Errorf(codes.Internal, "seal connector credential: %v", err)
		}
		update.Schedule = &model.ConnectorScheduleUpdate{SyncEverySeconds: current.SyncEverySeconds}
		return s.switchSlackConnector(ctx, current, update, model.SlackAppUnbind{}, &discovery, true)
	}
	if app != nil {
		if err := slackNotice(ctx, ws, int(app.GetNoticeVersion())); err != nil {
			return nil, err
		}
	}
	install, appSource, err := s.slackAppInstall(ctx, ws)
	if err != nil {
		return nil, err
	}
	feed, err := s.deps.DB.GetFoundrySlackFeed(ctx, current.ID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		feed = nil
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the Slack connection's feed: %v", err)
	}
	rebind := feed == nil || feed.InstallID != install.ID
	if rebind && app == nil {
		// Kept, but bound again: the storage notice is the binder's to accept.
		return nil, foundryPrecondition(ws, "FOUNDRY_SLACK_APP_NOTICE", slackNoticeMissing)
	}
	staged := (was.Credential == "" || was.Staged) && !app.GetNow()
	next := connector.SlackSettings{HistoryDays: int(sl.GetHistoryDays()), Credential: connector.SlackCredentialApp, TeamID: install.TeamID, Staged: staged}
	src := connector.Source{SlackApp: appSource}
	if staged {
		secret, err := keyring.Open(current.SecretCiphertext, current.SecretKeyID, connector.SecretContext(current.ID))
		if err != nil {
			return nil, status.Errorf(codes.Internal, "open connector %s credential: %v", current.ID, err)
		}
		src = connector.Source{Secret: string(secret)}
	}
	src.Settings, _ = json.Marshal(next) // plain fields
	kind, err := catalog.KindFor(model.ConnectorKindSlack, src.Settings)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "connector kind slack is not available: %v", err)
	}
	settings, discovery, err := s.testSource(ctx, ws, kind, src)
	if err != nil {
		return nil, err
	}
	if sameJSON(current.Config, settings) && !rebind {
		if update.Name != nil {
			if err := s.deps.DB.UpdateConnector(ctx, ws, current.ID, update); err != nil {
				return nil, status.Errorf(codes.Internal, "update connector: %v", err)
			}
		}
		stored, err := s.loadConnector(ctx, ws, current.ID)
		if err != nil {
			return nil, err
		}
		return s.connectorResponse(ctx, stored, &discovery)
	}
	if err := idle(); err != nil {
		return nil, err
	}
	prepared, err := slackSettingsOf(settings)
	if err != nil {
		return nil, err
	}
	var bind model.SlackFeedBind
	if rebind {
		if bind, err = s.slackBinding(ctx, install, appSource, int(app.GetNoticeVersion()), prepared.HistoryDays); err != nil {
			return nil, err
		}
	} else {
		bind = model.SlackFeedBind{InstallID: install.ID, Scopes: s.slackRequired(), NoticeVersion: feed.NoticeVersion, BoundBy: feed.BoundBy,
			HistoryDays: prepared.HistoryDays}
	}
	secret := ""
	update.Config, update.Schedule = settings, &model.ConnectorScheduleUpdate{SyncEverySeconds: current.SyncEverySeconds}
	var sw model.SlackAppSwitch = model.SlackAppBindNow{Bind: bind}
	if staged {
		// It syncs by its token, on its schedule, until the flip.
		secret, update.SecretHint, sw = src.Secret, current.SecretHint, model.SlackAppBindStaged{Bind: bind}
		now := time.Now()
		update.Schedule.NextSyncAt = cmp.Or(current.NextSyncAt, &now)
	}
	if update.SecretCiphertext, update.SecretKeyID, err = keyring.Seal([]byte(secret), connector.SecretContext(current.ID)); err != nil {
		return nil, status.Errorf(codes.Internal, "seal connector credential: %v", err)
	}
	return s.switchSlackConnector(ctx, current, update, sw, &discovery, !staged)
}

// switchSlackConnector stores update and the switch sw, then, when full,
// queues a full read (a sync queued meanwhile reads the new settings when
// it starts).
func (s *Service) switchSlackConnector(ctx context.Context, current *model.Connector, update model.ConnectorUpdate, sw model.SlackAppSwitch,
	discovery *connector.Discovery, full bool) (*foundryv1.ConnectorResponse, error) {
	var refusal model.SlackFeedRefusal
	switch err := s.deps.DB.UpdateSlackAppConnector(ctx, current.WorkspaceID, current.ID, update, sw); {
	case errors.As(err, &refusal):
		return nil, s.slackFeedRefused(ctx, current.WorkspaceID, refusal)
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "connector not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "update connector: %v", err)
	}
	stored, err := s.loadConnector(ctx, current.WorkspaceID, current.ID)
	if err != nil {
		return nil, err
	}
	if full {
		if _, err := s.queueConnectorSync(ctx, stored, model.ConnectorSyncSettings, model.ConnectorSyncFull); err != nil && status.Code(err) != codes.AlreadyExists {
			return nil, err
		}
	}
	return s.connectorResponse(ctx, stored, discovery)
}

// slackStatus is how a Slack connection through Helix's Slack app, or
// switching to it (settings), reads its Slack workspace; cfg carries the
// notice its feed was bound under.
func (s *Service) slackStatus(ctx context.Context, c *model.Connector, settings connector.SlackSettings, cfg *foundryv1.SlackConnectorConfig) (
	*foundryv1.ConnectorSlackStatus, error) {
	out := &foundryv1.ConnectorSlackStatus{TeamId: settings.TeamID, Switching: settings.Staged}
	feed, err := s.deps.DB.GetFoundrySlackFeed(ctx, c.ID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		out.State = foundryv1.SlackFeedState_SLACK_FEED_STATE_INSTALL_GONE
		return out, nil
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read connector %s Slack feed: %v", c.ID, err)
	}
	if app := cfg.GetHelixApp(); app != nil {
		app.NoticeVersion = int32(feed.NoticeVersion)
	}
	out.TeamId = feed.TeamID
	live, err := s.deps.DB.GetFoundrySlackInstall(ctx, c.WorkspaceID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		live = nil
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the Slack install: %v", err)
	}
	install, err := s.deps.DB.GetFoundrySlackInstallByID(ctx, feed.InstallID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the Slack install of connector %s: %v", c.ID, err)
	}
	out.TeamName = install.TeamName
	if at := install.EventsLimitedAt; at != nil && time.Since(*at) < slackEventsLimitedShown {
		out.EventsLimitedAt = timestamppb.New(*at)
	}
	state, missing := syncer.SlackAppStateOf(feed, live, s.slackRequired())
	out.State, out.MissingScopes = map[syncer.SlackAppState]foundryv1.SlackFeedState{
		syncer.SlackAppLive:           foundryv1.SlackFeedState_SLACK_FEED_STATE_LIVE,
		syncer.SlackAppNeedsReinstall: foundryv1.SlackFeedState_SLACK_FEED_STATE_NEEDS_REINSTALL,
		syncer.SlackAppInstallGone:    foundryv1.SlackFeedState_SLACK_FEED_STATE_INSTALL_GONE,
		syncer.SlackAppTeamChanged:    foundryv1.SlackFeedState_SLACK_FEED_STATE_TEAM_CHANGED,
	}[state], missing
	if state == syncer.SlackAppInstallGone && install.RevokedAt != nil {
		out.DeletedAt = timestamppb.New(install.RevokedAt.Add(s.slackConfig().Feed.OrphanRetention))
	}
	progress, err := s.deps.DB.FoundrySlackBackfillProgress(ctx, feed.ID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return out, nil
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read connector %s Slack backfill: %v", c.ID, err)
	}
	out.LossyDays, out.Backfill = int32(progress.LossyDays), slackBackfillToProto(progress, s.slackConfig().Backfill, time.Now())
	return out, nil
}

// slackBackfillToProto is what a feed's backfill has left (p), as Slack's
// pace under b allows, at now; nil when nothing is. Each live window's
// messages left are its density so far (messages over the seconds read)
// over the span left, the mean density for a window not read yet, else a
// page; history calls are those over b.PageLimit, and reply calls the
// queued threads and the threads the messages left within b.RepliesDays
// will bring at the rate read so far. The ETA is the slower method's
// calls at its calls a minute. A slot that waits past its next call (a
// Retry-After) is waiting_until.
func slackBackfillToProto(p model.SlackBackfillProgress, b config.SlackBackfillConfig, now time.Time) *foundryv1.SlackBackfill {
	out := &foundryv1.SlackBackfill{}
	var messages, threads int
	var covered int64
	var live []model.SlackBackfillWindow
	for _, w := range p.Windows {
		out.Channels++
		switch w.Status {
		case model.SlackBackfillDone, model.SlackBackfillCancelled:
			out.ChannelsDone++
		case model.SlackBackfillQueued, model.SlackBackfillRunning:
			live = append(live, w)
		}
		messages, threads, covered = messages+w.Messages, threads+w.Threads, covered+w.CoveredS
	}
	if len(live) == 0 && p.QueuedThreads == 0 {
		return nil
	}
	pageLimit := float64(max(b.PageLimit, 1))
	mean := -1.0
	if covered > 0 {
		mean = float64(messages) / float64(covered)
	}
	perThread := 0.0
	if messages > 0 {
		perThread = float64(threads) / float64(messages)
	}
	repliesFrom := now.Add(-time.Duration(b.RepliesDays) * 24 * time.Hour)
	historyCalls, threadCalls := 0.0, float64(p.QueuedThreads)
	for _, w := range live {
		span := max(w.Latest.Sub(w.Oldest).Seconds(), 0)
		left := pageLimit
		switch {
		case w.CoveredS > 0:
			left = float64(w.Messages) / float64(w.CoveredS) * span
		case mean >= 0:
			left = mean * span
		}
		historyCalls += max(math.Ceil(left/pageLimit), 1)
		from := w.Oldest
		if repliesFrom.After(from) {
			from = repliesFrom
		}
		if recent := w.Latest.Sub(from).Seconds(); span > 0 && recent > 0 {
			threadCalls += left * recent / span * perThread
		}
	}
	minutes := max(historyCalls/float64(max(b.HistoryPerMinute, 1)), threadCalls/float64(max(b.RepliesPerMinute, 1)))
	out.EtaSeconds = int64(math.Ceil(minutes * 60))
	var waiting time.Time
	for _, slot := range []struct {
		at        time.Time
		perMinute int
	}{{p.HistoryAt, b.HistoryPerMinute}, {p.RepliesAt, b.RepliesPerMinute}} {
		if slot.at.After(now.Add(time.Minute/time.Duration(max(slot.perMinute, 1)))) && slot.at.After(waiting) {
			waiting = slot.at
		}
	}
	if !waiting.IsZero() {
		out.WaitingUntil = timestamppb.New(waiting)
	}
	return out
}

// slackSetup is what the Slack kind's form needs to read through Helix's
// Slack app, where foundry.slack.feed is on: the workspace's install,
// whether the caller may add it (again), the connection reading it
// already, and the storage notice; nil where the feed is off.
func (s *Service) slackSetup(ctx context.Context, workspaceID string) (*foundryv1.SlackConnectorSetup, error) {
	cfg := s.slackConfig()
	if !cfg.Enabled || !cfg.Feed.Enabled {
		return nil, nil
	}
	out := &foundryv1.SlackConnectorSetup{NoticeVersion: slackFeedNotice}
	install, err := s.deps.DB.GetFoundrySlackInstall(ctx, workspaceID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the Slack install: %v", err)
	default:
		out.Install, out.InstallerAdmin = s.slackInstallToProto(ctx, install), install.InstallerAdmin
	}
	if a := actor(ctx); a.Source == ports.SourceUser && a.UserID != "" {
		out.CanInstall = s.requireSlackInstall(ctx, workspaceID, a.UserID) == nil
	}
	connectors, err := s.deps.DB.ListConnectors(ctx, workspaceID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list connectors: %v", err)
	}
	for _, c := range connectors {
		var settings connector.SlackSettings
		if c.Kind == model.ConnectorKindSlack && json.Unmarshal(c.Config, &settings) == nil && settings.Credential == connector.SlackCredentialApp {
			out.InUseByConnectorId = c.ID
			break
		}
	}
	return out, nil
}

// slackReturnPaths are the site paths an install may return to, besides
// Settings › Slack (""): the Data area's Slack form, and a connection's
// page (slackConnectionPath).
const (
	slackReturnSettings   = "/settings/slack"
	slackReturnData       = "/data?add=slack"
	slackReturnConnection = "/data/connections/"
)

// slackReturnPath reads a return path an install may carry back to the
// site; false for any other, which could send the person elsewhere.
func slackReturnPath(p string) (string, bool) {
	switch p {
	case "", slackReturnSettings:
		return "", true
	case slackReturnData:
		return p, true
	}
	connectorID, ok := strings.CutPrefix(p, slackReturnConnection)
	if !ok || connectorID == "" || len(connectorID) > 64 || strings.Trim(connectorID, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz") != "" {
		return "", false
	}
	return p, true
}
