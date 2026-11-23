package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/access"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/slack"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Helix Foundry for Slack (foundry.slack): the requests Slack sends Helix's
// app, which the door forwards (ReceiveSlackRequest), and Settings › Slack:
// adding the app by Slack's OAuth, disconnecting it, and linking people's
// Slack accounts to their Foundry ones. The foundry-slack-worker turns what
// ReceiveSlackRequest records into Founder's turns, and posts what they say.

const (
	// slackIntakeBudget bounds ReceiveSlackRequest's own work: the door waits
	// 2.5 s for it, within the 3 s Slack waits for an answer.
	slackIntakeBudget = 2 * time.Second
	// slackStateTTL is how long an install's OAuth state lives: Slack's
	// round trip, once.
	slackStateTTL = 10 * time.Minute
	// slackLinkTokenPrefix starts the tokens the bot's link prompts carry.
	slackLinkTokenPrefix = "hx_sl_"
	// maxSlackArg is the longest code, state or link token taken.
	maxSlackArg = 256
	// slackAPIShare is the API's share of Slack's rate limits: it calls
	// Slack only for a person's click in Settings › Slack, beside the
	// workers, which take theirs (foundry.slack.api_share).
	slackAPIShare = 0.25
	// slackCallTimeout bounds the Slack calls one Settings › Slack request
	// makes.
	slackCallTimeout = 10 * time.Second
	// defaultSlackURL is Slack's web origin: its OAuth page, and the Web API
	// under /api.
	defaultSlackURL = "https://slack.com"
)

var slackReceived = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_slack_received_total",
	Help: "Requests from Slack the API took, by route (events or interactivity) and outcome: queued, duplicate, ignored, " +
		"unknown_team, uninstalled, stale_uninstall, verification, unverified, malformed, disabled, feed (a channel event: " +
		"helix_foundry_slack_feed_events_total), busy or failed.",
}, []string{"route", "outcome"})

// slackRoutes names the door's routes, for the metric.
var slackRoutes = map[foundryv1.SlackRoute]string{
	foundryv1.SlackRoute_SLACK_ROUTE_EVENTS:        "events",
	foundryv1.SlackRoute_SLACK_ROUTE_INTERACTIVITY: "interactivity",
}

// slackOK is Slack's answer to a request taken, or one the bot does nothing
// with.
func slackOK() *foundryv1.ReceiveSlackRequestResponse {
	return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: http.StatusOK}
}

// ReceiveSlackRequest takes a request Slack sent the app, which only
// Foundry's door forwards: it verifies Slack's signature with the app's
// signing secret, then, with one read and one write and no call to Slack,
// records once what the bot acts on (intake, the worker's) or ends an
// uninstalled install. A channel message or channel event goes to the
// Slack data connector's feed and the bot's unmentioned follow-ups
// (receiveSlackFeed), refused UNAVAILABLE at once while the replica's feed
// slots are taken. Wherever foundry.slack is off every request is
// answered 404. A store failure is UNAVAILABLE, which the door answers 500:
// Slack retries an event, and the dedupe key keeps a retry from counting
// twice; a click isn't retried, and its person clicks again. No log holds
// what Slack sent.
func (s *Service) ReceiveSlackRequest(ctx context.Context, req *foundryv1.ReceiveSlackRequestRequest) (*foundryv1.ReceiveSlackRequestResponse, error) {
	a := actor(ctx)
	if a.Source != ports.SourceEventSource || a.EventSourceTokenID != "" || a.WorkspaceID != "" || a.UserID != "" || a.ServiceCredentialID != "" {
		return nil, status.Error(codes.PermissionDenied, "only Foundry's door takes Slack's requests")
	}
	route, ok := slackRoutes[req.GetRoute()]
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "route is Slack's events or interactivity")
	}
	cfg := s.deps.Cfg.Foundry.Slack
	if !cfg.Enabled {
		slackReceived.WithLabelValues(route, "disabled").Inc()
		return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: http.StatusNotFound}, nil
	}
	if err := slack.Verify(cfg.SigningSecret, req.GetTimestamp(), req.GetBody(), req.GetSignature(), time.Now()); err != nil {
		slackReceived.WithLabelValues(route, "unverified").Inc()
		return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: http.StatusUnauthorized}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, slackIntakeBudget)
	defer cancel()
	var (
		resp    *foundryv1.ReceiveSlackRequestResponse
		outcome string
		err     error
	)
	if req.GetRoute() == foundryv1.SlackRoute_SLACK_ROUTE_EVENTS {
		resp, outcome, err = s.receiveSlackEvent(ctx, req.GetBody())
	} else {
		resp, outcome, err = s.receiveSlackClick(ctx, req.GetBody())
	}
	if errors.Is(err, errSlackFeedBusy) {
		slackReceived.WithLabelValues(route, "busy").Inc()
		return nil, status.Error(codes.Unavailable, "Foundry takes no more of Slack's channel messages just now")
	}
	if err != nil {
		slackReceived.WithLabelValues(route, "failed").Inc()
		log.From(ctx).Error("take a Slack request failed", "route", route, "retry", req.GetRetryNum(), "err", err)
		return nil, status.Error(codes.Unavailable, "Foundry couldn't take Slack's request just now")
	}
	slackReceived.WithLabelValues(route, outcome).Inc()
	return resp, nil
}

// receiveSlackEvent takes a verified request to the events URL, answering
// it and naming its outcome; an error is the store's.
func (s *Service) receiveSlackEvent(ctx context.Context, body []byte) (*foundryv1.ReceiveSlackRequestResponse, string, error) {
	cb, err := slack.ParseEvent(body)
	if err != nil {
		return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: http.StatusBadRequest}, "malformed", nil
	}
	if cb.AppID != "" && cb.AppID != s.deps.Cfg.Foundry.Slack.AppID {
		return slackOK(), "ignored", nil
	}
	key := model.SlackEventDedupe(cb.EventID)
	// at is the thread a message was said in: its conversation, which the
	// worker takes one message at a time. A stop has none: it mustn't wait
	// behind the message whose turn it stops.
	var (
		team, user string
		at         *slack.Conversation
	)
	switch e := cb.Event.(type) {
	case slack.URLVerification:
		challenge, _ := json.Marshal(map[string]string{"challenge": e.Challenge}) // a string always marshals
		return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: http.StatusOK, ContentType: "application/json", Body: challenge}, "verification", nil
	case slack.Uninstalled:
		return s.uninstallSlackTeam(ctx, e.TeamID, key, e.EventTime)
	case slack.TokensRevoked:
		if !e.Bot {
			return slackOK(), "ignored", nil // a user's token, which the app never holds
		}
		return s.uninstallSlackTeam(ctx, e.TeamID, key, e.EventTime)
	case slack.Mention:
		team, user, at = e.At.TeamID, e.UserID, &e.At
	case slack.DirectMessage:
		team, user, at = e.At.TeamID, e.UserID, &e.At
	case slack.SessionStopped:
		team, user = e.At.TeamID, e.UserID
	case slack.FeedEvent:
		return s.receiveSlackFeed(ctx, cb.EventID, e)
	default:
		return slackOK(), "ignored", nil
	}
	if !model.ValidSlackDedupe(key) {
		return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: http.StatusBadRequest}, "malformed", nil
	}
	install, err := s.deps.DB.GetFoundrySlackInstallByTeam(ctx, team)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return slackOK(), "unknown_team", nil
	}
	if err != nil {
		return nil, "", err
	}
	payload, err := slack.EventPayload(cb.Event)
	if err != nil {
		return nil, "", err
	}
	conversation := ""
	if at != nil {
		// One Founder thread per Slack thread and person, or per Slack
		// thread for an install's own bot.
		person := user
		if _, bot := install.Mode.(model.SlackSharedBot); bot {
			person = ""
		}
		conversation = model.SlackConversation(install.ID, at.ChannelID, at.ThreadTS, person)
	}
	in := &model.SlackIntake{
		ID: id.New(), InstallID: install.ID, DedupeKey: key, Kind: model.SlackIntakeEvent, Conversation: conversation, SlackUserID: user, Payload: payload,
	}
	// A relayed install's requests wait for its local install, but one from a
	// channel shared with another organisation, which Helix's worker refuses.
	if _, relayed := install.Destination.(model.SlackServesRelay); relayed {
		switch e := cb.Event.(type) {
		case slack.Mention:
			if !e.At.ExtShared {
				return s.queueRelayDelivery(ctx, in, relaySeen(e.At, e.UserID, e.Text))
			}
		case slack.DirectMessage:
			if !e.At.ExtShared {
				return s.queueRelayDelivery(ctx, in, relaySeen(e.At, e.UserID, e.Text))
			}
		case slack.SessionStopped:
			if !e.At.ExtShared {
				return s.queueRelayDelivery(ctx, in, relaySeen(e.At, e.UserID, ""))
			}
		}
	}
	return s.queueSlackIntake(ctx, in)
}

// uninstallSlackTeam takes Slack's word that the app left team at
// eventTime: the store ends its live install, unless the event is older than
// its last install.
func (s *Service) uninstallSlackTeam(ctx context.Context, team, key string, eventTime time.Time) (*foundryv1.ReceiveSlackRequestResponse, string, error) {
	if !model.ValidSlackDedupe(key) {
		return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: http.StatusBadRequest}, "malformed", nil
	}
	outcome, err := s.deps.DB.UninstallFoundrySlackTeam(ctx, team, id.New(), key, eventTime)
	if err != nil {
		return nil, "", err
	}
	named := map[model.SlackUninstallOutcome]string{
		model.SlackUninstallDone: "uninstalled", model.SlackUninstallStale: "stale_uninstall",
		model.SlackUninstallUnknownTeam: "unknown_team", model.SlackUninstallDuplicate: "duplicate",
	}[outcome]
	if named == "" {
		return nil, "", errors.New("the store answered an uninstall it doesn't name")
	}
	if outcome == model.SlackUninstallDone {
		log.From(ctx).Info("foundry slack uninstalled by Slack", "team", team)
	}
	return slackOK(), named, nil
}

// receiveSlackClick takes a verified request to the interactivity URL.
func (s *Service) receiveSlackClick(ctx context.Context, body []byte) (*foundryv1.ReceiveSlackRequestResponse, string, error) {
	in, err := slack.ParseInteraction(body)
	if err != nil {
		return &foundryv1.ReceiveSlackRequestResponse{HttpStatus: http.StatusBadRequest}, "malformed", nil
	}
	var c slack.Clicked
	switch in := in.(type) {
	case slack.Approve:
		c = in.Clicked
	case slack.Dismiss:
		c = in.Clicked
	case slack.Refresh:
		c = in.Clicked
	case slack.Review:
		c = in.Clicked
	case slack.Run:
		c = in.Clicked
	case slack.Stop:
		c = in.Clicked
	case slack.Unlink:
		c = in.Clicked
	default:
		return slackOK(), "ignored", nil
	}
	install, err := s.deps.DB.GetFoundrySlackInstallByTeam(ctx, c.TeamID)
	if errors.Is(err, foundrystore.ErrNotFound) && c.EnterpriseID != "" {
		// In a Grid org a click names the workspace the clicker's session is
		// in, which in a channel several of the org's workspaces share may
		// not be the install's (a click carries no authorizations, as an
		// event does): the org's one live install takes it, and the worker
		// acts only for a clicker linked to that install. With several, the
		// click can't be told apart, and is left.
		install, err = s.deps.DB.GetFoundrySlackInstallByEnterprise(ctx, c.EnterpriseID)
	}
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || errors.Is(err, foundrystore.ErrConflict):
		return slackOK(), "unknown_team", nil
	case err != nil:
		return nil, "", err
	}
	payload, err := slack.InteractionPayload(in)
	if err != nil {
		return nil, "", err
	}
	intake := &model.SlackIntake{
		ID: id.New(), InstallID: install.ID, DedupeKey: slack.InteractionKey(in), Kind: model.SlackIntakeInteraction, SlackUserID: c.UserID, Payload: payload,
	}
	// A click on a relayed install's message is its local install's: the
	// message's channel is one it was sent something from already.
	if _, relayed := install.Destination.(model.SlackServesRelay); relayed {
		return s.queueRelayDelivery(ctx, intake, model.RelaySeen{UserIDs: []string{c.UserID}})
	}
	return s.queueSlackIntake(ctx, intake)
}

// queueSlackIntake records in, once: a retry of it is a duplicate.
func (s *Service) queueSlackIntake(ctx context.Context, in *model.SlackIntake) (*foundryv1.ReceiveSlackRequestResponse, string, error) {
	switch err := s.deps.DB.QueueFoundrySlackIntake(ctx, in); {
	case errors.Is(err, foundrystore.ErrConflict):
		return slackOK(), "duplicate", nil
	case err != nil:
		return nil, "", err
	}
	return slackOK(), "queued", nil
}

// slackPerson is the person a Settings › Slack call is for: someone signed
// in to Foundry's site, never a key or an agent. Wherever foundry.slack is
// off it is FAILED_PRECONDITION, reason FOUNDRY_SLACK_DISABLED.
func (s *Service) slackPerson(ctx context.Context, workspaceID string) (string, error) {
	if !s.deps.Cfg.Foundry.Slack.Enabled {
		return "", foundryPrecondition(workspaceID, "FOUNDRY_SLACK_DISABLED", "Helix Foundry for Slack isn't available here")
	}
	a := actor(ctx)
	if a.Source != ports.SourceUser || a.UserID == "" || a.ServiceCredentialID != "" {
		return "", status.Error(codes.PermissionDenied, "Slack is set up by people signed in to Foundry")
	}
	return a.UserID, nil
}

// slackRole is userID's role in workspaceID; RoleNone where the host has no
// one to ask.
func (s *Service) slackRole(ctx context.Context, workspaceID, userID string) (ports.Role, error) {
	if s.deps.People == nil {
		return ports.RoleNone, nil
	}
	m, err := s.deps.People.Member(ctx, workspaceID, userID)
	if err != nil {
		return ports.RoleNone, status.Errorf(codes.Unavailable, "read the workspace's members: %v", err)
	}
	return m.Role, nil
}

// requireSlackManager refuses anyone but an owner or admin of workspaceID,
// asked now: the front end's check may be of a role since lost.
func (s *Service) requireSlackManager(ctx context.Context, workspaceID, userID string) error {
	role, err := s.slackRole(ctx, workspaceID, userID)
	if err != nil {
		return err
	}
	if role != ports.RoleOwner && role != ports.RoleAdmin {
		return status.Error(codes.PermissionDenied, "only the workspace's owners and admins manage its Slack")
	}
	return nil
}

// GetFoundrySlack answers the workspace's install, the caller's link in it,
// and whether the workspace may add the app.
func (s *Service) GetFoundrySlack(ctx context.Context, req *foundryv1.GetFoundrySlackRequest) (*foundryv1.FoundrySlackResponse, error) {
	ws := req.GetWorkspaceId()
	if ws == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	userID, err := s.slackPerson(ctx, ws)
	if err != nil {
		return nil, err
	}
	return s.slackResponse(ctx, ws, userID)
}

// slackResponse is the workspace's Slack as userID sees it.
func (s *Service) slackResponse(ctx context.Context, workspaceID, userID string) (*foundryv1.FoundrySlackResponse, error) {
	role, err := s.slackRole(ctx, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	out := &foundryv1.FoundrySlack{
		WorkspaceId: workspaceID, InstallAllowed: s.deps.Cfg.Foundry.Slack.InstallAllowed(workspaceID),
		CanManage: role == ports.RoleOwner || role == ports.RoleAdmin,
	}
	install, err := s.deps.DB.GetFoundrySlackInstall(ctx, workspaceID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return &foundryv1.FoundrySlackResponse{Slack: out}, nil
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the Slack install: %v", err)
	}
	out.Install = s.slackInstallToProto(ctx, install)
	// The bot's key as the host holds it now, which sessions as it need
	// (bot.broken); no session is opened to tell.
	if bot, on := install.Mode.(model.SlackSharedBot); on && s.deps.BotCredentials != nil {
		cred, err := s.deps.BotCredentials.Get(ctx, workspaceID, bot.CredentialID)
		switch {
		case errors.Is(err, foundrystore.ErrNotFound):
			out.Install.Bot.Broken = true
		case err != nil:
			log.From(ctx).Warn("read the Slack bot's key failed", "workspace_id", workspaceID, "credential_id", bot.CredentialID, "err", err)
		default:
			out.Install.Bot.CredentialName, out.Install.Bot.Broken = cred.Name, !cred.Usable
		}
	}
	links, err := s.deps.DB.ListFoundrySlackLinks(ctx, install.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the Slack links: %v", err)
	}
	// A standalone install keeps those its owner allows as the owner too,
	// read-only: none of them is the owner's own Slack account.
	if i := slices.IndexFunc(links, func(l *model.SlackLink) bool { return l.UserID == userID && l.Access != model.SlackReadAccess{} }); i >= 0 {
		out.MyLink = s.slackLinkToProto(ctx, links[i])
	}
	return &foundryv1.FoundrySlackResponse{Slack: out}, nil
}

// slackUserName is the name Foundry shows for userID; "" when it can't say.
func (s *Service) slackUserName(ctx context.Context, userID string) string {
	if s.deps.People == nil {
		return ""
	}
	name, err := s.deps.People.DisplayName(ctx, userID)
	if err != nil {
		return ""
	}
	return name
}

func (s *Service) slackLinkToProto(ctx context.Context, l *model.SlackLink) *foundryv1.FoundrySlackLink {
	access := foundryv1.FoundrySlackLinkAccess_FOUNDRY_SLACK_LINK_ACCESS_FULL
	if _, read := l.Access.(model.SlackReadAccess); read {
		access = foundryv1.FoundrySlackLinkAccess_FOUNDRY_SLACK_LINK_ACCESS_READ
	}
	return &foundryv1.FoundrySlackLink{SlackUserId: l.SlackUserID, UserId: l.UserID, UserName: s.slackUserName(ctx, l.UserID),
		CreatedAt: timestamppb.New(l.CreatedAt), Access: access}
}

// StartFoundrySlackInstall starts an owner's or admin's install of the app:
// a state bound to them, the workspace and the site path to return to for
// slackStateTTL, once, and Slack's page asking to add the app with the
// scopes foundry.slack's flags need (slack.Required), which returns to the
// site's /slack/installed. A workspace whose install lacks none of them is
// refused, unless the Slack connector is on (foundry.slack.feed) and the
// install lacks what a connection through the app needs of it besides: a
// Slack admin or owner's install, or its bot's id. Such an install, or one
// lacking a scope, is installed again.
func (s *Service) StartFoundrySlackInstall(ctx context.Context, req *foundryv1.StartFoundrySlackInstallRequest) (
	*foundryv1.StartFoundrySlackInstallResponse, error) {
	ws := req.GetWorkspaceId()
	if ws == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	userID, err := s.slackPerson(ctx, ws)
	if err != nil {
		return nil, err
	}
	returnPath, ok := slackReturnPath(req.GetReturnPath())
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "return_path is /settings/slack, /data?add=slack or a Slack connection's page")
	}
	if err := s.requireSlackInstall(ctx, ws, userID); err != nil {
		return nil, err
	}
	required := s.slackRequired()
	switch install, err := s.deps.DB.GetFoundrySlackInstall(ctx, ws); {
	case err == nil && !slices.ContainsFunc(required, func(scope string) bool { return !slices.Contains(install.Scopes, scope) }) &&
		(!s.slackConfig().Feed.Enabled || install.InstallerAdmin && install.BotID != ""):
		return nil, foundryPrecondition(ws, "FOUNDRY_SLACK_INSTALLED", "this workspace is connected to Slack already; disconnect it first")
	case err != nil && !errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Errorf(codes.Internal, "read the Slack install: %v", err)
	}
	// A standalone install adds the app through Helix, which holds its
	// install and returns the owner to its Settings › Slack.
	if s.deps.Relay != nil {
		authorize, err := s.deps.Relay.StartSlackInstall(ctx, ws)
		switch {
		case errors.Is(err, foundrystore.ErrNotFound):
			return nil, foundryPrecondition(ws, "FOUNDRY_RELAY_NOT_LINKED", "link this Foundry to Helix first: Slack reaches it through Helix")
		case err != nil:
			return nil, status.Errorf(codes.Unavailable, "start adding Helix Foundry to Slack through Helix: %v", err)
		}
		return &foundryv1.StartFoundrySlackInstallResponse{AuthorizeUrl: authorize}, nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, status.Errorf(codes.Internal, "make the install's state: %v", err)
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	if err := s.deps.DB.CreateFoundrySlackInstallState(ctx, ws, userID, hashToken(state), returnPath, slackStateTTL); err != nil {
		return nil, status.Errorf(codes.Internal, "record the install's state: %v", err)
	}
	cfg := s.deps.Cfg.Foundry.Slack
	query := url.Values{"client_id": {cfg.ClientID}, "scope": {strings.Join(required, ",")},
		"redirect_uri": {slackRedirectURI(cfg.SiteURL)}, "state": {state}}
	return &foundryv1.StartFoundrySlackInstallResponse{AuthorizeUrl: s.slackURL() + "/oauth/v2/authorize?" + query.Encode()}, nil
}

// requireSlackInstall refuses an install the workspace may not make now: by
// anyone but an owner or admin, off the install list, or with Founder off.
func (s *Service) requireSlackInstall(ctx context.Context, workspaceID, userID string) error {
	if err := s.requireSlackManager(ctx, workspaceID, userID); err != nil {
		return err
	}
	switch {
	case !s.deps.Cfg.Foundry.Slack.InstallAllowed(workspaceID):
		return foundryPrecondition(workspaceID, "FOUNDRY_SLACK_NOT_OFFERED", "Helix Foundry for Slack isn't offered to this workspace yet")
	case !s.deps.Cfg.Foundry.Agent.Enabled:
		return foundryPrecondition(workspaceID, "FOUNDRY_AGENT_DISABLED", "Founder isn't available here, so it can't answer in Slack")
	}
	return nil
}

// slackRedirectURI is where Slack returns an install: the site's
// /slack/installed.
func slackRedirectURI(siteURL string) string {
	return strings.TrimRight(siteURL, "/") + "/slack/installed"
}

// slackURL is Slack's web origin.
func (s *Service) slackURL() string {
	if s.deps.SlackURL == "" {
		return defaultSlackURL
	}
	return s.deps.SlackURL
}

// FinishFoundrySlackInstall finishes an install with what Slack returned to
// the site: it spends the state, for the member who started it, asks again
// that they are an owner or admin of its workspace, exchanges the code for
// the bot's token (oauth.v2.access) and records the install with its token
// sealed under the install's id. An org-wide install, another app's, or one
// without every scope foundry.slack's flags need (slack.Required) is
// refused; the store refuses a Slack workspace connected elsewhere, or
// still being disconnected. The same Slack workspace again is a reinstall.
// Then it reads the bot's id (auth.test) and whether the installer is a
// Slack admin or owner (users.info), which a Slack connection through the
// app needs; either failing keeps what the install had, and is logged. It
// answers the path the install was started from. Slack's error in place of
// a code (a cancel) spends the state and refuses FOUNDRY_SLACK_CANCELLED.
// A refusal once the state is spent says, for an install a linked
// standalone install started, where its Settings › Slack is
// (slackReturnTo), so the site sends the browser back there, not to
// Helix's own.
func (s *Service) FinishFoundrySlackInstall(ctx context.Context, req *foundryv1.FinishFoundrySlackInstallRequest) (*foundryv1.FoundrySlackResponse, error) {
	userID, err := s.slackPerson(ctx, "")
	if err != nil {
		return nil, err
	}
	state := req.GetState()
	var code, cancelled string
	switch answer := req.GetAnswer().(type) {
	case *foundryv1.FinishFoundrySlackInstallRequest_Code:
		code = answer.Code
	case *foundryv1.FinishFoundrySlackInstallRequest_Error:
		cancelled = answer.Error
	}
	switch {
	case code == "" && !slackErrorPattern.MatchString(cancelled) || len(code) > maxSlackArg || state == "" || len(state) > maxSlackArg:
		return nil, status.Errorf(codes.InvalidArgument,
			"state and a code, or Slack's error code, are what Slack returned, at most %d characters", maxSlackArg)
	case code != "" && s.deps.ConnectorKeys == nil:
		return nil, status.Error(codes.Unavailable, "Foundry can't keep a Slack token here: it has no connector keyring")
	}
	st, err := s.deps.DB.SpendFoundrySlackInstallState(ctx, hashToken(state), userID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, foundryPrecondition("", "FOUNDRY_SLACK_INSTALL_EXPIRED",
			"this install expired or was finished already: add Helix Foundry to Slack again from Settings › Slack")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the install's state: %v", err)
	case cancelled != "":
		log.From(ctx).Info("foundry slack install cancelled", "workspace_id", st.WorkspaceID, "error", cancelled)
		return nil, slackReturnTo(foundryPrecondition(st.WorkspaceID, "FOUNDRY_SLACK_CANCELLED",
			"Helix Foundry wasn't added: adding it to Slack was cancelled"), st.ReturnTo)
	}
	resp, err := s.finishSlackInstall(ctx, st, userID, code)
	if err != nil {
		return nil, slackReturnTo(err, st.ReturnTo)
	}
	return resp, nil
}

// slackErrorPattern is an OAuth error code Slack returns in place of a code
// (access_denied, invalid_scope, ...).
var slackErrorPattern = regexp.MustCompile(`^[a-z_]{1,64}$`)

// slackReturnTo is err with returnTo (an install's own Settings › Slack, a
// linked standalone install's) as its ErrorInfo's metadata return_to: one
// added when it had none (FOUNDRY_SLACK_INSTALL_FAILED). err as it is for
// none.
func slackReturnTo(err error, returnTo string) error {
	if returnTo == "" {
		return err
	}
	st := status.Convert(err)
	info := &errdetails.ErrorInfo{Reason: "FOUNDRY_SLACK_INSTALL_FAILED", Domain: foundryDomain}
	var others []protoadapt.MessageV1
	for _, d := range st.Details() {
		if held, ok := d.(*errdetails.ErrorInfo); ok {
			info = held
		} else if m, ok := d.(protoadapt.MessageV1); ok {
			others = append(others, m)
		}
	}
	info.Metadata = maps.Clone(info.Metadata)
	if info.Metadata == nil {
		info.Metadata = map[string]string{}
	}
	info.Metadata["return_to"] = returnTo
	out, detailErr := status.New(st.Code(), st.Message()).WithDetails(append(others, info)...)
	if detailErr != nil {
		return err
	}
	return out.Err()
}

// finishSlackInstall finishes the install whose state st the caller spent
// with Slack's code (FinishFoundrySlackInstall).
func (s *Service) finishSlackInstall(ctx context.Context, st *model.SlackInstallState, userID, code string) (*foundryv1.FoundrySlackResponse, error) {
	ws := st.WorkspaceID
	_, relayed := st.Destination.(model.SlackServesRelay)
	switch {
	case relayed:
		// A linked install runs its own Founder: its member must still
		// manage the workspace, which must still be offered the app.
		if err := s.relayOn(ws); err != nil {
			return nil, err
		}
		if err := s.requireSlackManager(ctx, ws, userID); err != nil {
			return nil, err
		}
		if !s.deps.Cfg.Foundry.Slack.InstallAllowed(ws) {
			return nil, foundryPrecondition(ws, "FOUNDRY_SLACK_NOT_OFFERED", "Helix Foundry for Slack isn't offered to this workspace yet")
		}
	default:
		if err := s.requireSlackInstall(ctx, ws, userID); err != nil {
			return nil, err
		}
	}
	cfg := s.deps.Cfg.Foundry.Slack
	callCtx, cancel := context.WithTimeout(ctx, slackCallTimeout)
	defer cancel()
	grant, err := s.deps.Slack.OAuthAccess(callCtx, cfg.ClientID, cfg.ClientSecret, code, slackRedirectURI(cfg.SiteURL))
	var refused *slack.APIError
	switch {
	case errors.As(err, &refused) && refused.Code != "":
		return nil, foundryPrecondition(ws, "FOUNDRY_SLACK_REFUSED", "Slack refused the install ("+refused.Code+"): add Helix Foundry to Slack again")
	case err != nil:
		log.From(ctx).Warn("slack oauth.v2.access failed", "err", err)
		return nil, status.Error(codes.Unavailable, "Slack couldn't be reached to finish the install; add Helix Foundry to Slack again")
	case grant.EnterpriseInstall:
		return nil, foundryPrecondition(ws, "FOUNDRY_SLACK_ENTERPRISE_INSTALL",
			"Helix Foundry is added to one Slack workspace, not a whole Enterprise Grid organisation: add it to one of its workspaces")
	case grant.AppID != cfg.AppID:
		return nil, status.Errorf(codes.Internal, "Slack installed app %s, not foundry.slack.app_id", grant.AppID)
	}
	var missing []string
	for _, scope := range s.slackRequired() {
		if !slices.Contains(grant.Scopes, scope) {
			missing = append(missing, scope)
		}
	}
	if len(missing) > 0 {
		return nil, foundryPrecondition(ws, "FOUNDRY_SLACK_SCOPES",
			"Slack didn't grant Helix Foundry every permission it needs ("+strings.Join(missing, ", ")+"): add it again and allow them")
	}
	install := &model.SlackInstall{
		ID: id.New(), WorkspaceID: ws, TeamID: grant.TeamID, EnterpriseID: grant.EnterpriseID, TeamName: grant.TeamName, AppID: grant.AppID,
		BotUserID: grant.BotUserID, Scopes: grant.Scopes, InstalledByUserID: userID, InstalledBySlackUserID: grant.InstallerID,
		Destination: st.Destination,
	}
	stored, reinstalled, err := s.deps.DB.InstallFoundrySlack(ctx, install, func(installID string) (model.SlackSealedToken, error) {
		sealed, keyID, err := s.deps.ConnectorKeys.Seal([]byte(grant.BotToken), installID)
		return model.SlackSealedToken{Sealed: sealed, KeyID: keyID}, err
	})
	var refusal model.SlackRefusal
	switch {
	case errors.As(err, &refusal):
		said, ok := map[model.SlackRefusalReason][2]string{
			model.SlackDisconnecting: {"FOUNDRY_SLACK_DISCONNECTING", "Helix is still disconnecting this Slack workspace; try again in a minute"},
			model.SlackTeamTaken: {"FOUNDRY_SLACK_TEAM_TAKEN", "this Slack workspace is connected to another Foundry workspace: disconnect it there, " +
				"or a Slack admin can remove Helix Foundry in Slack's app settings"},
			model.SlackInstalled: {"FOUNDRY_SLACK_INSTALLED", "this workspace is connected to another Slack workspace; disconnect it first"},
		}[refusal.Reason]
		if !ok {
			return nil, status.Errorf(codes.Internal, "record the install: %v", err)
		}
		return nil, foundryPrecondition(ws, said[0], said[1])
	case errors.Is(err, foundrystore.ErrNotFound) && relayed:
		return nil, foundryPrecondition(ws, "FOUNDRY_RELAY_LINK_REVOKED", "the Foundry that started this install was unlinked from Helix meanwhile")
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "workspace not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "record the install: %v", err)
	}
	log.From(ctx).Info("foundry slack installed", "workspace_id", ws, "install_id", stored.ID, "team", stored.TeamID, "reinstalled", reinstalled)
	s.learnSlackBot(ctx, stored.ID, slack.Bot{InstallID: stored.ID, Token: grant.BotToken}, grant.InstallerID)
	resp, err := s.slackResponse(ctx, ws, userID)
	if err != nil {
		return nil, err
	}
	resp.ReturnPath, resp.ReturnTo = st.ReturnPath, st.ReturnTo
	return resp, nil
}

// learnSlackBot records what Slack says of an install's bot (auth.test's
// bot_id) and its installer (users.info: a Slack admin or owner). Best
// effort: either call failing keeps what the install had.
func (s *Service) learnSlackBot(ctx context.Context, installID string, bot slack.Bot, installer string) {
	callCtx, cancel := context.WithTimeout(ctx, slackCallTimeout)
	defer cancel()
	var learned model.SlackInstallBotUpdate
	if botID, err := s.deps.Slack.BotID(callCtx, bot); err == nil {
		learned.BotID = &botID
	} else {
		log.From(ctx).Warn("slack auth.test failed; the install keeps its bot id", "install_id", installID, "err", err)
	}
	if user, err := s.deps.Slack.UserInfo(callCtx, bot, installer); err == nil {
		learned.InstallerAdmin = &user.Admin
	} else {
		log.From(ctx).Warn("slack users.info of the installer failed; the install keeps whether a Slack admin installed it", "install_id", installID,
			"err", err)
	}
	if learned.BotID == nil && learned.InstallerAdmin == nil {
		return
	}
	if err := s.deps.DB.UpdateFoundrySlackInstallBot(ctx, installID, learned); err != nil {
		log.From(ctx).Warn("record the Slack install's bot failed", "install_id", installID, "err", err)
	}
}

// DisconnectFoundrySlack disconnects the workspace's install, for an owner
// or admin: it asks Slack to remove the app (apps.uninstall), then revokes
// the install, which deletes its links, link tokens, open intake and
// replies, and leaves its threads' Slack text to be purged. When Slack
// can't be told, the install stays revoked but not uninstalled, keeping its
// token for the worker's sweep to tell Slack, and the Slack workspace can't
// be installed again until then.
func (s *Service) DisconnectFoundrySlack(ctx context.Context, req *foundryv1.DisconnectFoundrySlackRequest) (*foundryv1.FoundrySlackResponse, error) {
	ws := req.GetWorkspaceId()
	if ws == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	userID, err := s.slackPerson(ctx, ws)
	if err != nil {
		return nil, err
	}
	if err := s.requireSlackManager(ctx, ws, userID); err != nil {
		return nil, err
	}
	install, err := s.deps.DB.GetFoundrySlackInstall(ctx, ws)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "this workspace isn't connected to Slack")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the Slack install: %v", err)
	}
	uninstalled := s.uninstallSlackApp(ctx, install)
	switch err := s.deps.DB.RevokeFoundrySlackInstall(ctx, ws, install.ID, uninstalled); {
	case errors.Is(err, foundrystore.ErrNotFound):
		// Slack's own uninstall, or another disconnect, came first.
	case err != nil:
		return nil, status.Errorf(codes.Internal, "disconnect the Slack install: %v", err)
	}
	log.From(ctx).Info("foundry slack disconnected", "workspace_id", ws, "install_id", install.ID, "uninstalled", uninstalled)
	// The bot's key goes with it; one the host keeps meanwhile acts as no
	// one's bot (IsFoundrySlackBot needs a live install), and stays locked.
	if bot, on := install.Mode.(model.SlackSharedBot); on {
		if err := s.revokeSlackBot(ctx, ws, bot.CredentialID); err != nil {
			log.From(ctx).Warn("revoke the disconnected Slack bot's key failed", "workspace_id", ws, "credential_id", bot.CredentialID, "err", err)
		}
	}
	return s.slackResponse(ctx, ws, userID)
}

// UpdateFoundrySlack sets whom the workspace's install acts as, for an owner
// or admin. BOT makes the bot a workspace service credential no one holds
// the token of (ports.BotCredentials, hosted only:
// FOUNDRY_SLACK_BOT_HOSTED_ONLY elsewhere), whose sources Settings › Access
// chooses. PER_PERSON revokes it first, so a failure leaves bot mode on to
// try again, then the store clears it and retires the bot's threads. The
// mode the install has already changes nothing.
func (s *Service) UpdateFoundrySlack(ctx context.Context, req *foundryv1.UpdateFoundrySlackRequest) (*foundryv1.FoundrySlackResponse, error) {
	ws := req.GetWorkspaceId()
	if ws == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	userID, err := s.slackPerson(ctx, ws)
	if err != nil {
		return nil, err
	}
	mode := req.GetIdentityMode()
	if mode != foundryv1.FoundrySlackIdentityMode_FOUNDRY_SLACK_IDENTITY_MODE_PER_PERSON &&
		mode != foundryv1.FoundrySlackIdentityMode_FOUNDRY_SLACK_IDENTITY_MODE_BOT {
		return nil, status.Error(codes.InvalidArgument, "identity_mode is PER_PERSON or BOT")
	}
	if err := s.requireSlackManager(ctx, ws, userID); err != nil {
		return nil, err
	}
	install, err := s.deps.DB.GetFoundrySlackInstall(ctx, ws)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "this workspace isn't connected to Slack")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the Slack install: %v", err)
	}
	bot, isBot := install.Mode.(model.SlackSharedBot)
	switch {
	case isBot == (mode == foundryv1.FoundrySlackIdentityMode_FOUNDRY_SLACK_IDENTITY_MODE_BOT):
		// The mode it has.
	case !isBot && s.deps.BotCredentials == nil:
		return nil, foundryPrecondition(ws, "FOUNDRY_SLACK_BOT_HOSTED_ONLY", "a shared Slack bot is offered on Helix's hosted Foundry only")
	case !isBot:
		// service_credentials.name holds 64 characters.
		name := []rune("Slack bot (" + install.TeamName + ")")
		if len(name) > 64 {
			name = append(name[:62], []rune("…)")...)
		}
		credentialID, err := s.deps.BotCredentials.Create(ctx, ws, string(name), userID)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "make the bot's key: %v", err)
		}
		_, err = s.deps.DB.SetFoundrySlackMode(ctx, ws, install.ID, model.SlackSharedBot{CredentialID: credentialID})
		if err != nil {
			// Unused: another click made the bot first (ErrConflict), the
			// install went, or the write failed.
			if revokeErr := s.revokeSlackBot(ctx, ws, credentialID); revokeErr != nil {
				log.From(ctx).Error("revoke an unused Slack bot key failed", "workspace_id", ws, "credential_id", credentialID, "err", revokeErr)
			}
		}
		switch {
		case errors.Is(err, foundrystore.ErrNotFound):
			return nil, status.Error(codes.NotFound, "this workspace isn't connected to Slack")
		case err != nil && !errors.Is(err, foundrystore.ErrConflict):
			return nil, status.Errorf(codes.Internal, "turn bot mode on: %v", err)
		}
		log.From(ctx).Info("foundry slack bot mode on", "workspace_id", ws, "install_id", install.ID, "credential_id", credentialID)
	default:
		if err := s.revokeSlackBot(ctx, ws, bot.CredentialID); err != nil {
			return nil, status.Errorf(codes.Unavailable, "revoke the bot's key: %v; try again", err)
		}
		was, err := s.deps.DB.SetFoundrySlackMode(ctx, ws, install.ID, model.SlackPerPerson{})
		switch {
		case errors.Is(err, foundrystore.ErrNotFound):
			return nil, status.Error(codes.NotFound, "this workspace isn't connected to Slack")
		case err != nil:
			return nil, status.Errorf(codes.Internal, "turn bot mode off: %v", err)
		}
		// Another click turned it off and on again meanwhile: its key goes too.
		if other, ok := was.(model.SlackSharedBot); ok && other != bot {
			if err := s.revokeSlackBot(ctx, ws, other.CredentialID); err != nil {
				log.From(ctx).Error("revoke a retired Slack bot's key failed", "workspace_id", ws, "credential_id", other.CredentialID, "err", err)
			}
		}
		log.From(ctx).Info("foundry slack bot mode off", "workspace_id", ws, "install_id", install.ID, "credential_id", bot.CredentialID)
	}
	return s.slackResponse(ctx, ws, userID)
}

// revokeSlackBot revokes a Slack bot's key where the host makes them, and
// drops its source grants, which no one holds then.
func (s *Service) revokeSlackBot(ctx context.Context, workspaceID, credentialID string) error {
	if s.deps.BotCredentials != nil {
		if err := s.deps.BotCredentials.Revoke(ctx, workspaceID, credentialID); err != nil {
			return err
		}
	}
	return s.deps.DB.DeleteFoundryAccessPrincipal(ctx, workspaceID, access.Principal{Kind: access.PrincipalServiceCredential, ID: credentialID})
}

// uninstallSlackApp asks Slack to remove the app from install's workspace,
// and reports whether Slack knows it is gone: it said so, or the token no
// longer works there.
func (s *Service) uninstallSlackApp(ctx context.Context, install *model.SlackInstall) bool {
	bot, err := s.slackBot(ctx, install.ID)
	if err != nil {
		log.From(ctx).Warn("open the Slack install's token failed", "install_id", install.ID, "err", err)
		return false
	}
	cfg := s.deps.Cfg.Foundry.Slack
	callCtx, cancel := context.WithTimeout(ctx, slackCallTimeout)
	defer cancel()
	err = s.deps.Slack.Uninstall(callCtx, bot, cfg.ClientID, cfg.ClientSecret)
	if err == nil || slack.IsCode(err, "invalid_auth", "account_inactive", "token_revoked", "token_expired", "not_authed") {
		return true
	}
	log.From(ctx).Warn("slack apps.uninstall failed; the sweep tries again", "install_id", install.ID, "err", err)
	return false
}

// slackAs is the Web API client and install's bot that call Slack as it:
// through Helix's relay on a standalone install, else with its own token.
func (s *Service) slackAs(ctx context.Context, install *model.SlackInstall) (*slack.Client, slack.Bot, error) {
	if s.deps.Relay != nil {
		return s.deps.Relay.Slack(ctx, install)
	}
	bot, err := s.slackBot(ctx, install.ID)
	return s.deps.Slack, bot, err
}

// slackBot is install installID's bot token, opened.
func (s *Service) slackBot(ctx context.Context, installID string) (slack.Bot, error) {
	if s.deps.ConnectorKeys == nil {
		return slack.Bot{}, errors.New("no connector keyring")
	}
	sealed, err := s.deps.DB.GetFoundrySlackInstallToken(ctx, installID)
	if err != nil {
		return slack.Bot{}, err
	}
	token, err := s.deps.ConnectorKeys.Open(sealed.Sealed, sealed.KeyID, installID)
	if err != nil {
		return slack.Bot{}, err
	}
	return slack.Bot{InstallID: installID, Token: string(token)}, nil
}

// ListFoundrySlackLinks lists who is linked, for owners and admins, and on a
// standalone install who asked to use it: none while the workspace has no
// install.
func (s *Service) ListFoundrySlackLinks(ctx context.Context, req *foundryv1.ListFoundrySlackLinksRequest) (*foundryv1.ListFoundrySlackLinksResponse, error) {
	ws := req.GetWorkspaceId()
	if ws == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	userID, err := s.slackPerson(ctx, ws)
	if err != nil {
		return nil, err
	}
	if err := s.requireSlackManager(ctx, ws, userID); err != nil {
		return nil, err
	}
	install, err := s.deps.DB.GetFoundrySlackInstall(ctx, ws)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return &foundryv1.ListFoundrySlackLinksResponse{}, nil
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the Slack install: %v", err)
	}
	links, err := s.deps.DB.ListFoundrySlackLinks(ctx, install.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the Slack links: %v", err)
	}
	out := &foundryv1.ListFoundrySlackLinksResponse{Links: make([]*foundryv1.FoundrySlackLink, 0, len(links))}
	for _, l := range links {
		out.Links = append(out.Links, s.slackLinkToProto(ctx, l))
	}
	// Only a standalone install's owner allows people who ask.
	if !s.deps.Cfg.Host.Local {
		return out, nil
	}
	requests, err := s.deps.DB.ListFoundrySlackAccessRequests(ctx, install.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the Slack access requests: %v", err)
	}
	for _, r := range requests {
		out.Requests = append(out.Requests, &foundryv1.FoundrySlackAccessRequest{SlackUserId: r.SlackUserID, SlackName: r.SlackName,
			CreatedAt: timestamppb.New(r.CreatedAt)})
	}
	return out, nil
}

// slackLinkCheck is what a link token would link: the install, the token,
// the Slack user as Slack says now, and why they may not be linked to the
// caller (UNSPECIFIED when they may).
type slackLinkCheck struct {
	install *model.SlackInstall
	token   *model.SlackLinkToken
	user    slack.User
	refusal foundryv1.FoundrySlackLinkRefusal
}

// checkSlackLink reads what linking raw's Slack user to userID would do:
// FOUNDRY_SLACK_LINK_EXPIRED unless raw is a live token of the workspace's
// install. The Slack user must be a full member of the installed Slack
// workspace, with a confirmed email that is userID's (case aside), so no
// one links a Slack account to a Foundry account not theirs. A standalone
// install has one person, its owner, and Helix keeps Slack's emails: there
// only the Slack account that installed the app links (the bot offers no
// one else a link, the spec's §10.5), as the owner.
func (s *Service) checkSlackLink(ctx context.Context, workspaceID, userID, raw string) (slackLinkCheck, error) {
	if !strings.HasPrefix(raw, slackLinkTokenPrefix) || len(raw) > maxSlackArg {
		return slackLinkCheck{}, status.Error(codes.InvalidArgument, "token is the one Helix Foundry's link carries")
	}
	expired := foundryPrecondition(workspaceID, "FOUNDRY_SLACK_LINK_EXPIRED",
		"this link expired or was used: message Helix Foundry in Slack for a new one")
	install, err := s.deps.DB.GetFoundrySlackInstall(ctx, workspaceID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return slackLinkCheck{}, expired
	case err != nil:
		return slackLinkCheck{}, status.Errorf(codes.Internal, "read the Slack install: %v", err)
	}
	token, err := s.deps.DB.GetFoundrySlackLinkToken(ctx, hashToken(raw))
	local := s.deps.Relay != nil
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || (err == nil && token.InstallID != install.ID):
		return slackLinkCheck{}, expired
	case err != nil:
		return slackLinkCheck{}, status.Errorf(codes.Internal, "read the link: %v", err)
	case local && token.SlackUserID != install.InstalledBySlackUserID:
		return slackLinkCheck{}, expired
	}
	email := ""
	if !local {
		if s.deps.Emails == nil {
			return slackLinkCheck{}, status.Error(codes.Unavailable, "Foundry can't read your email here, so it can't check the Slack account is yours")
		}
		if email, err = s.deps.Emails.UserEmail(ctx, userID); err != nil && !errors.Is(err, foundrystore.ErrNotFound) {
			return slackLinkCheck{}, status.Errorf(codes.Unavailable, "read your email: %v", err)
		}
	}
	client, bot, err := s.slackAs(ctx, install)
	if err != nil {
		return slackLinkCheck{}, status.Errorf(codes.Unavailable, "open the Slack install's token: %v", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, slackCallTimeout)
	defer cancel()
	user, err := client.UserInfo(callCtx, bot, token.SlackUserID)
	switch {
	case slack.IsCode(err, "user_not_found"):
		user = slack.User{ID: token.SlackUserID}
	case err != nil:
		log.From(ctx).Warn("slack users.info failed", "install_id", install.ID, "err", err)
		return slackLinkCheck{}, status.Error(codes.Unavailable, "Slack couldn't be asked who this is just now; try again in a minute")
	}
	c := slackLinkCheck{install: install, token: token, user: user}
	switch {
	case !user.MemberOf(install.TeamID, install.EnterpriseID):
		c.refusal = foundryv1.FoundrySlackLinkRefusal_FOUNDRY_SLACK_LINK_REFUSAL_NOT_A_MEMBER
	case local:
		// The installer, as the owner: no email to compare.
	case email == "" || !strings.EqualFold(strings.TrimSpace(user.Email), strings.TrimSpace(email)):
		c.refusal = foundryv1.FoundrySlackLinkRefusal_FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_MISMATCH
	case !user.EmailConfirmed:
		c.refusal = foundryv1.FoundrySlackLinkRefusal_FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_UNCONFIRMED
	}
	return c, nil
}

// maskEmail hides all of an address but its first character and its
// domain: "a•••@acme.com"; "" for no address.
func maskEmail(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at <= 0 {
		return ""
	}
	first, _ := utf8.DecodeRuneInString(email)
	return string(first) + "•••" + email[at:]
}

// PreviewFoundrySlackLink says who a link token's Slack user is, and
// whether they may be linked to the caller, without spending it.
func (s *Service) PreviewFoundrySlackLink(ctx context.Context, req *foundryv1.PreviewFoundrySlackLinkRequest) (*foundryv1.PreviewFoundrySlackLinkResponse, error) {
	ws := req.GetWorkspaceId()
	if ws == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	userID, err := s.slackPerson(ctx, ws)
	if err != nil {
		return nil, err
	}
	c, err := s.checkSlackLink(ctx, ws, userID, req.GetToken())
	if err != nil {
		return nil, err
	}
	return &foundryv1.PreviewFoundrySlackLinkResponse{
		TeamName: c.install.TeamName, SlackUserId: c.user.ID, SlackName: c.user.Shown(), SlackHandle: c.user.Name,
		SlackEmailMasked: maskEmail(c.user.Email), MatchesYou: c.refusal == foundryv1.FoundrySlackLinkRefusal_FOUNDRY_SLACK_LINK_REFUSAL_UNSPECIFIED,
		Refusal: c.refusal,
	}, nil
}

// slackLinkRefusals word why a Slack user can't be linked to the caller.
var slackLinkRefusals = map[foundryv1.FoundrySlackLinkRefusal]string{
	foundryv1.FoundrySlackLinkRefusal_FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_MISMATCH: "this Slack account's email isn't your Foundry email: " +
		"sign in to Foundry as the person the Slack account is",
	foundryv1.FoundrySlackLinkRefusal_FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_UNCONFIRMED: "Slack hasn't confirmed this Slack account's email: " +
		"confirm it in Slack, then open the link again",
	foundryv1.FoundrySlackLinkRefusal_FOUNDRY_SLACK_LINK_REFUSAL_NOT_A_MEMBER: "this Slack account isn't a full member of the connected Slack workspace: " +
		"guests and people of other organisations can't use Helix Foundry",
}

// CreateFoundrySlackLink links a link token's Slack user to the caller, a
// member of the workspace, as PreviewFoundrySlackLink checked it, spending
// the token; it replaces the Slack user's earlier link, and the caller's.
// Then the bot tells the Slack user in a direct message, with Unlink, so a
// link they didn't make shows at once.
func (s *Service) CreateFoundrySlackLink(ctx context.Context, req *foundryv1.CreateFoundrySlackLinkRequest) (*foundryv1.FoundrySlackLinkResponse, error) {
	ws := req.GetWorkspaceId()
	if ws == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	userID, err := s.slackPerson(ctx, ws)
	if err != nil {
		return nil, err
	}
	switch role, err := s.slackRole(ctx, ws, userID); {
	case err != nil:
		return nil, err
	case role == ports.RoleNone:
		return nil, status.Error(codes.PermissionDenied, "only the workspace's members link their Slack accounts")
	}
	c, err := s.checkSlackLink(ctx, ws, userID, req.GetToken())
	if err != nil {
		return nil, err
	}
	if msg, refused := slackLinkRefusals[c.refusal]; refused {
		st, detailErr := status.New(codes.FailedPrecondition, msg).WithDetails(&errdetails.ErrorInfo{
			Reason: "FOUNDRY_SLACK_LINK_REFUSED", Domain: foundryDomain, Metadata: map[string]string{"workspace_id": ws, "refusal": c.refusal.String()},
		})
		if detailErr != nil {
			return nil, status.Error(codes.FailedPrecondition, msg)
		}
		return nil, st.Err()
	}
	link, err := s.deps.DB.LinkFoundrySlackUser(ctx, ws, c.token.TokenHash, userID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, foundryPrecondition(ws, "FOUNDRY_SLACK_LINK_EXPIRED", "this link expired or was used: message Helix Foundry in Slack for a new one")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "link the Slack account: %v", err)
	}
	log.From(ctx).Info("foundry slack linked", "workspace_id", ws, "install_id", c.install.ID)
	s.tellSlackLinked(ctx, c.install, c.user)
	return &foundryv1.FoundrySlackLinkResponse{Link: s.slackLinkToProto(ctx, link)}, nil
}

// tellSlackLinked tells a Slack user, in the bot's direct message, whose
// Foundry account they now act as, with Unlink. Best effort: the link
// stands whatever Slack says, and Settings › Slack shows it.
func (s *Service) tellSlackLinked(ctx context.Context, install *model.SlackInstall, user slack.User) {
	site, err := slack.NewSite(s.deps.Cfg.Foundry.Slack.SiteURL)
	if err != nil {
		log.From(ctx).Warn("foundry.slack.site_url isn't an origin", "err", err)
		return
	}
	client, bot, err := s.slackAs(ctx, install)
	if err != nil {
		log.From(ctx).Warn("open the Slack install's token failed", "install_id", install.ID, "err", err)
		return
	}
	// A standalone install's owner has no email Slack would show here.
	whom := user.Email + " in Foundry"
	if s.deps.Relay != nil {
		whom = "the owner of the Foundry on your computer"
	}
	text := site.Text("Your Slack account is linked to " + whom + ", and I answer you as them. Not you? Unlink it now.")
	msg := slack.Message{Text: text, Blocks: []slack.Block{
		slack.Section{Text: text},
		slack.Actions{Elements: []slack.Button{{ActionID: slack.ActionUnlink, Text: "Unlink", Style: slack.StyleDanger}}},
	}}
	callCtx, cancel := context.WithTimeout(ctx, slackCallTimeout)
	defer cancel()
	channel, err := client.OpenDM(callCtx, bot, user.ID)
	if err == nil {
		_, err = client.PostMessage(callCtx, bot, slack.Post{Channel: channel, Message: msg})
	}
	if err != nil {
		log.From(ctx).Warn("tell the Slack user they are linked failed", "install_id", install.ID, "err", err)
	}
}

// DeleteFoundrySlackLink unlinks a Slack user of the workspace's install:
// the caller's own, or, for an owner or admin, anyone's.
func (s *Service) DeleteFoundrySlackLink(ctx context.Context, req *foundryv1.DeleteFoundrySlackLinkRequest) (*emptypb.Empty, error) {
	ws := req.GetWorkspaceId()
	if ws == "" || !model.ValidSlackID(req.GetSlackUserId()) {
		return nil, status.Error(codes.InvalidArgument, "workspace_id and slack_user_id are required")
	}
	userID, err := s.slackPerson(ctx, ws)
	if err != nil {
		return nil, err
	}
	notFound := status.Error(codes.NotFound, "that Slack account isn't linked")
	install, err := s.deps.DB.GetFoundrySlackInstall(ctx, ws)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, notFound
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the Slack install: %v", err)
	}
	link, err := s.deps.DB.GetFoundrySlackLink(ctx, install.ID, req.GetSlackUserId())
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, notFound
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the Slack link: %v", err)
	}
	if link.UserID != userID {
		if err := s.requireSlackManager(ctx, ws, userID); err != nil {
			return nil, err
		}
	}
	switch err := s.deps.DB.DeleteFoundrySlackLink(ctx, install.ID, link.SlackUserID); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, notFound
	case err != nil:
		return nil, status.Errorf(codes.Internal, "unlink the Slack account: %v", err)
	}
	log.From(ctx).Info("foundry slack unlinked", "workspace_id", ws, "install_id", install.ID, "own", link.UserID == userID)
	return &emptypb.Empty{}, nil
}
