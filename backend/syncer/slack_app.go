package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// SlackApps opens a workspace's live install of Helix's Slack app for a
// Slack connector read through it (connector.SlackCredentialApp, not
// staged); nil where foundry.slack.feed is off. An error that is a *Failure
// is the sync's as it is: an install removed, of another team, or lacking
// permissions fails for good, so the schedule pauses.
type SlackApps interface {
	SlackAppSource(ctx context.Context, workspaceID, connectorID string) (*connector.SlackApp, error)
}

// SlackAppRead says whether a Slack connector's settings read through
// Helix's Slack app now: they name the app and aren't staged (a staged
// switch syncs by its token until the app has read the history). Settings
// that aren't a Slack connector's read with their own credential.
func SlackAppRead(kind string, settings json.RawMessage) bool {
	var s connector.SlackSettings
	return kind == model.ConnectorKindSlack && json.Unmarshal(settings, &s) == nil && s.Credential == connector.SlackCredentialApp && !s.Staged
}

// SlackAppState is how a Slack connector through Helix's Slack app stands
// with its workspace's install: a closed set.
type SlackAppState int

const (
	// SlackAppLive reads: its feed's install is the workspace's live one,
	// holding every permission it needs.
	SlackAppLive SlackAppState = iota + 1
	// SlackAppNeedsReinstall: the install lacks a permission the feed needs.
	SlackAppNeedsReinstall
	// SlackAppInstallGone: the app was removed from the team, and the
	// workspace has no install of another team.
	SlackAppInstallGone
	// SlackAppTeamChanged: the workspace's live install is of another team.
	SlackAppTeamChanged
)

// String names the state, for logs and metrics.
func (s SlackAppState) String() string {
	switch s {
	case SlackAppLive:
		return "live"
	case SlackAppNeedsReinstall:
		return "needs_reinstall"
	case SlackAppInstallGone:
		return "install_gone"
	case SlackAppTeamChanged:
		return "team_changed"
	}
	return fmt.Sprintf("SlackAppState(%d)", int(s))
}

// SlackAppStateOf is how feed stands with live, its workspace's live
// install (nil when it has none), when the feed needs required scopes, and
// the scopes the install lacks (only for SlackAppNeedsReinstall):
//   - live is the feed's install: SlackAppLive, or SlackAppNeedsReinstall
//     while it lacks one of required;
//   - live is of another team: SlackAppTeamChanged;
//   - else (no live install, or one of the same team the feed wasn't
//     pointed at yet): SlackAppInstallGone.
func SlackAppStateOf(feed *model.SlackFeed, live *model.SlackInstall, required []string) (SlackAppState, []string) {
	switch {
	case live == nil:
		return SlackAppInstallGone, nil
	case live.ID == feed.InstallID:
		var missing []string
		for _, scope := range required {
			if !slices.Contains(live.Scopes, scope) {
				missing = append(missing, scope)
			}
		}
		if len(missing) > 0 {
			return SlackAppNeedsReinstall, missing
		}
		return SlackAppLive, nil
	case live.TeamID != feed.TeamID:
		return SlackAppTeamChanged, nil
	}
	return SlackAppInstallGone, nil
}

// slackAppStore is what HostedSlackApps reads of the store.
type slackAppStore interface {
	GetFoundrySlackFeed(ctx context.Context, connectorID string) (*model.SlackFeed, error)
	GetFoundrySlackInstall(ctx context.Context, workspaceID string) (*model.SlackInstall, error)
	GetFoundrySlackInstallByID(ctx context.Context, id string) (*model.SlackInstall, error)
	GetFoundrySlackInstallToken(ctx context.Context, installID string) (model.SlackSealedToken, error)
}

// HostedSlackApps opens a connector's install of Helix's Slack app where
// Helix holds the installs' bot tokens (hosted): the connector's feed,
// the workspace's live install, then the install's token, sealed by Keys
// under the install's id. AppID is Helix's app (foundry.slack.app_id),
// Required the scopes a feed needs (slack.Required), and OrphanRetention
// how long a feed whose install is gone keeps its data
// (foundry.slack.feed.orphan_retention), which its failure says. Calls go
// to Slack's Web API at BaseURL (https://slack.com/api when empty) through
// HTTP (one with a 30 s timeout when nil).
type HostedSlackApps struct {
	DB              slackAppStore
	Keys            *secretbox.Keyring
	AppID           string
	Required        []string
	OrphanRetention time.Duration
	BaseURL         string
	HTTP            *http.Client
}

var _ SlackApps = HostedSlackApps{}

// SlackAppSource is the connector's install, as SlackAppStateOf finds it:
// live, its bot token opened; else a permanent *Failure saying what to do.
// A store or keyring error is returned as it is, which a sync retries.
func (h HostedSlackApps) SlackAppSource(ctx context.Context, workspaceID, connectorID string) (*connector.SlackApp, error) {
	feed, err := h.DB.GetFoundrySlackFeed(ctx, connectorID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, &Failure{Message: "this connection no longer reads a Slack workspace through Helix Foundry: edit it to choose one", Permanent: true,
			Err: err}
	case err != nil:
		return nil, fmt.Errorf("read connector %s Slack feed: %w", connectorID, err)
	}
	live, err := h.DB.GetFoundrySlackInstall(ctx, workspaceID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		live = nil
	case err != nil:
		return nil, fmt.Errorf("read workspace %s Slack install: %w", workspaceID, err)
	}
	state, missing := SlackAppStateOf(feed, live, h.Required)
	switch state {
	case SlackAppNeedsReinstall:
		return nil, &Failure{Message: "Helix Foundry needs new Slack permissions to sync channels (" + strings.Join(missing, ", ") +
			"): an owner or admin adds it to Slack again from Settings › Slack", Permanent: true}
	case SlackAppTeamChanged:
		return nil, &Failure{Message: "this workspace's Slack is now " + live.TeamName + ", not the Slack workspace this connection reads: " +
			"edit the connection to read " + live.TeamName + ", or delete it", Permanent: true}
	case SlackAppInstallGone:
		gone, err := h.DB.GetFoundrySlackInstallByID(ctx, feed.InstallID)
		if err != nil {
			return nil, fmt.Errorf("read Slack install %s: %w", feed.InstallID, err)
		}
		return nil, &Failure{Message: SlackAppGoneMessage(gone, h.OrphanRetention), Permanent: true}
	}
	sealed, err := h.DB.GetFoundrySlackInstallToken(ctx, live.ID)
	if err != nil {
		return nil, fmt.Errorf("read Slack install %s token: %w", live.ID, err)
	}
	token, err := h.Keys.Open(sealed.Sealed, sealed.KeyID, live.ID)
	if err != nil {
		return nil, fmt.Errorf("open Slack install %s token: %w", live.ID, err)
	}
	return &connector.SlackApp{InstallID: live.ID, TeamID: live.TeamID, BotUserID: live.BotUserID, BotID: live.BotID, AppID: h.AppID,
		Caller: SlackBotCaller{BaseURL: h.BaseURL, HTTP: h.HTTP, Token: string(token)}}, nil
}

// SlackAppGoneMessage is what a connector through Helix's Slack app says
// once the app was removed from its team (install, revoked), whose data
// is deleted retention after:
//
//	Helix Foundry was removed from Acme: add it again in Settings › Slack,
//	or switch this connection to a bot token. Its data is frozen as of its
//	last sync and is deleted on 28 November 2026.
func SlackAppGoneMessage(install *model.SlackInstall, retention time.Duration) string {
	msg := "Helix Foundry was removed from " + install.TeamName + ": add it again in Settings › Slack, or switch this connection to a bot token. " +
		"Its data is frozen as of its last sync"
	if install.RevokedAt == nil {
		return msg + "."
	}
	return msg + " and is deleted on " + install.RevokedAt.Add(retention).UTC().Format("2 January 2006") + "."
}

// slackAnswerBytes bounds one Slack Web API answer read through a
// SlackBotCaller.
const slackAnswerBytes = 64 << 20

// SlackBotCaller sends an install's Web API calls with its bot token, as a
// GET of BaseURL/method (https://slack.com/api when empty) through HTTP
// (one with a 30 s timeout when nil): connector.SlackCaller.
type SlackBotCaller struct {
	BaseURL string
	HTTP    *http.Client
	Token   string
}

var _ connector.SlackCaller = SlackBotCaller{}

// Call sends one GET and returns Slack's answer as it came, at most 64 MiB
// of its body; an error is a call that got no answer.
func (c SlackBotCaller) Call(ctx context.Context, method string, query url.Values) (int, http.Header, []byte, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "https://slack.com/api"
	}
	target := base + "/" + method
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, slackAnswerBytes))
	if err != nil {
		return 0, nil, nil, err
	}
	return res.StatusCode, res.Header, body, nil
}
