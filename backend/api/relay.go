package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/keytoken"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/slack"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Helix's Slack relay (foundry.slack.relay), hosted's side: a standalone
// Foundry install, linked to a Helix workspace by an owner or admin
// (AuthorizeFoundryRelayLink, then RedeemFoundryRelayCode with the
// install's PKCE verifier), claims what Slack sends its installs
// (ClaimFoundryRelayDeliveries) and calls Slack as them
// (CallSlackForRelay, relay_proxy.go), through the foundry-events door with
// its hx_rl_ token. Its installs are hosted installs of Helix's app serving
// the link's local workspaces (model.SlackServesRelay): ReceiveSlackRequest
// queues their requests as deliveries, never as intake, and Helix runs no
// bot for them. The site's Settings › Slack lists and revokes the links.

const (
	// relayCodeTTL is how long a link's code waits to be redeemed: the
	// browser's way back to the install, once.
	relayCodeTTL = 10 * time.Minute
	// relayLease is how long a claim holds what it leased: a delivery not
	// acked by then is claimed again.
	relayLease = time.Minute
	// maxRelayDeliveries is the most deliveries one claim leases.
	maxRelayDeliveries = 10
	// maxRelayAcks is the most ids one claim acks.
	maxRelayAcks = 2048
	// maxRelayAddress is the most of an address a link keeps.
	maxRelayAddress = 64
)

var (
	relayClaims = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_relay_claims_total",
		Help: "Claims of linked standalone installs, by outcome: delivered (something leased), empty (nothing arrived in the wait), " +
			"unauthenticated (no live link), refused or failed.",
	}, []string{"outcome"})
	relayDeliveries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_relay_deliveries_total",
		Help: "Slack's requests for relayed installs, by outcome: queued, duplicate, gone (the install was revoked meanwhile) or " +
			"disabled (foundry.slack.relay off).",
	}, []string{"outcome"})
)

// relayStatePattern is a link's state as its install makes it: URL-safe,
// handed back as it is.
var relayStatePattern = regexp.MustCompile(`^[A-Za-z0-9._~-]{16,256}$`)

// localWorkspacePattern is a standalone install's workspace id: a ULID.
var localWorkspacePattern = regexp.MustCompile(`^[0-9A-Z]{26}$`)

// relayOn refuses a relay call wherever foundry.slack or its relay is off:
// FAILED_PRECONDITION, reason FOUNDRY_RELAY_DISABLED.
func (s *Service) relayOn(workspaceID string) error {
	if cfg := s.deps.Cfg.Foundry.Slack; !cfg.Enabled || !cfg.Relay.Enabled || s.deps.Cfg.Host.Local {
		return foundryPrecondition(workspaceID, "FOUNDRY_RELAY_DISABLED", "Helix's Slack relay isn't available here")
	}
	return nil
}

// relayDoor refuses any caller but the door, and a relay that is off.
func (s *Service) relayDoor(ctx context.Context) error {
	a := actor(ctx)
	if a.Source != ports.SourceEventSource || a.EventSourceTokenID != "" || a.WorkspaceID != "" || a.UserID != "" || a.ServiceCredentialID != "" {
		return status.Error(codes.PermissionDenied, "only Foundry's door calls the relay")
	}
	return s.relayOn("")
}

// relayLink is the live link a door call's token hash names:
// UNAUTHENTICATED for none.
func (s *Service) relayLink(ctx context.Context, tokenHash string) (*model.RelayLink, error) {
	if err := s.relayDoor(ctx); err != nil {
		return nil, err
	}
	if !model.ValidSHA256Hex(tokenHash) {
		return nil, status.Error(codes.InvalidArgument, "token_sha256 is a SHA-256 in lowercase hex")
	}
	link, err := s.deps.DB.GetFoundryRelayLinkByToken(ctx, tokenHash)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.Unauthenticated, "this relay token is revoked or unknown")
	case err != nil:
		return nil, status.Errorf(codes.Unavailable, "read the relay link: %v", err)
	}
	return link, nil
}

// ValidateFoundryRelayToken answers the live link a token's hash names, for
// the door's admission: valid false for none.
func (s *Service) ValidateFoundryRelayToken(ctx context.Context, req *foundryv1.ValidateFoundryRelayTokenRequest) (
	*foundryv1.ValidateFoundryRelayTokenResponse, error) {
	link, err := s.relayLink(ctx, req.GetTokenSha256())
	if status.Code(err) == codes.Unauthenticated {
		return &foundryv1.ValidateFoundryRelayTokenResponse{}, nil
	}
	if err != nil {
		return nil, err
	}
	return &foundryv1.ValidateFoundryRelayTokenResponse{Valid: true, LinkId: link.ID, WorkspaceId: link.WorkspaceID}, nil
}

// AuthorizeFoundryRelayLink authorizes an owner or admin's link of a
// standalone install to their workspace: a one-time code for relayCodeTTL,
// bound to the install's PKCE challenge, its loopback Settings › Slack and
// its name, handed back there with the install's own state.
func (s *Service) AuthorizeFoundryRelayLink(ctx context.Context, req *foundryv1.AuthorizeFoundryRelayLinkRequest) (
	*foundryv1.AuthorizeFoundryRelayLinkResponse, error) {
	ws := req.GetWorkspaceId()
	if ws == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	userID, err := s.slackPerson(ctx, ws)
	if err != nil {
		return nil, err
	}
	if err := s.relayOn(ws); err != nil {
		return nil, err
	}
	if !relayStatePattern.MatchString(req.GetState()) {
		return nil, status.Error(codes.InvalidArgument, "state is 16 to 256 URL-safe characters")
	}
	if err := s.requireSlackManager(ctx, ws, userID); err != nil {
		return nil, err
	}
	if !s.deps.Cfg.Foundry.Slack.InstallAllowed(ws) {
		return nil, foundryPrecondition(ws, "FOUNDRY_SLACK_NOT_OFFERED", "Helix Foundry for Slack isn't offered to this workspace yet")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, status.Errorf(codes.Internal, "make the link's code: %v", err)
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	c := &model.RelayCode{CodeHash: hashToken(code), WorkspaceID: ws, UserID: userID, CodeChallenge: req.GetCodeChallenge(),
		RedirectURI: req.GetRedirectUri(), Name: req.GetName()}
	if err := c.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.deps.DB.CreateFoundryRelayCode(ctx, c, relayCodeTTL); err != nil {
		return nil, status.Errorf(codes.Internal, "record the link's code: %v", err)
	}
	log.From(ctx).Info("foundry relay link authorized", "workspace_id", ws)
	return &foundryv1.AuthorizeFoundryRelayLinkResponse{
		RedirectUrl: c.RedirectURI + "?" + url.Values{"code": {code}, "state": {req.GetState()}}.Encode(),
	}, nil
}

// RedeemFoundryRelayCode links the install the code was authorized for,
// once, with its PKCE verifier: a new hx_rl_ token, kept as its SHA-256. A
// member who lost their role since is refused, and the link revoked at once.
func (s *Service) RedeemFoundryRelayCode(ctx context.Context, req *foundryv1.RedeemFoundryRelayCodeRequest) (
	*foundryv1.RedeemFoundryRelayCodeResponse, error) {
	if err := s.relayDoor(ctx); err != nil {
		return nil, err
	}
	code, verifier := req.GetCode(), req.GetCodeVerifier()
	if code == "" || len(code) > maxSlackArg || !model.ValidRelayVerifier(verifier) {
		return nil, status.Error(codes.InvalidArgument, "code is the one Helix handed back, and code_verifier a PKCE verifier")
	}
	token, err := keytoken.NewRelay()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "make the relay token: %v", err)
	}
	link, err := s.deps.DB.RedeemFoundryRelayCode(ctx, hashToken(code), verifier, id.New(), hashToken(token))
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "this link expired or was used already: link the install to Helix again")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "link the install: %v", err)
	}
	if err := s.requireSlackManager(ctx, link.WorkspaceID, link.CreatedByUserID); err != nil {
		if revokeErr := s.deps.DB.RevokeFoundryRelayLink(ctx, link.WorkspaceID, link.ID); revokeErr != nil {
			log.From(ctx).Error("revoke a relay link its member may no longer make failed", "link_id", link.ID, "err", revokeErr)
		}
		return nil, err
	}
	email := ""
	if s.deps.Emails != nil {
		if email, err = s.deps.Emails.UserEmail(ctx, link.CreatedByUserID); err != nil {
			log.From(ctx).Warn("read a relay link's member's email failed", "link_id", link.ID, "err", err)
			email = ""
		}
	}
	log.From(ctx).Info("foundry relay linked", "workspace_id", link.WorkspaceID, "link_id", link.ID)
	return &foundryv1.RedeemFoundryRelayCodeResponse{Token: token, LinkId: link.ID, WorkspaceId: link.WorkspaceID, AccountEmail: email}, nil
}

// RevokeFoundryRelayToken revokes the token's link, from its install's
// side; its installs are revoked with it, and the Slack worker's sweep
// removes them from Slack.
func (s *Service) RevokeFoundryRelayToken(ctx context.Context, req *foundryv1.RevokeFoundryRelayTokenRequest) (
	*foundryv1.RevokeFoundryRelayTokenResponse, error) {
	link, err := s.relayLink(ctx, req.GetTokenSha256())
	if err != nil {
		return nil, err
	}
	if err := s.deps.DB.RevokeFoundryRelayLink(ctx, link.WorkspaceID, link.ID); err != nil && !errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Errorf(codes.Unavailable, "revoke the relay link: %v", err)
	}
	log.From(ctx).Info("foundry relay unlinked by its install", "workspace_id", link.WorkspaceID, "link_id", link.ID)
	return &foundryv1.RevokeFoundryRelayTokenResponse{}, nil
}

// ClaimFoundryRelayDeliveries is a linked install's claim (design §10.3):
// it records the link's contact, acks what the install took, records which
// of its workspaces' data connectors read their installs (only while
// hosted's feed or follow-ups are on), then waits up to wait_ms, within
// foundry.slack.relay.claim_wait, looking every claim_poll with a plain
// read and leasing only once something waits: the bot's requests first,
// then channel messages within relay.feed_batch and feed_batch_bytes
// (none while hosted's feed and follow-ups are off). It answers the link's
// live installs whatever it leased.
func (s *Service) ClaimFoundryRelayDeliveries(ctx context.Context, req *foundryv1.ClaimFoundryRelayDeliveriesRequest) (
	*foundryv1.ClaimFoundryRelayDeliveriesResponse, error) {
	link, err := s.relayLink(ctx, req.GetTokenSha256())
	if err != nil {
		outcome := "refused"
		if status.Code(err) == codes.Unauthenticated {
			outcome = "unauthenticated"
		}
		relayClaims.WithLabelValues(outcome).Inc()
		return nil, err
	}
	cfg := s.deps.Cfg.Foundry.Slack
	wait, most := time.Duration(req.GetWaitMs())*time.Millisecond, int(req.GetMax())
	switch {
	case req.GetWaitMs() < 0 || most < 0 || most > maxRelayDeliveries:
		relayClaims.WithLabelValues("refused").Inc()
		return nil, status.Errorf(codes.InvalidArgument, "wait_ms is not negative, and max from 0 to %d", maxRelayDeliveries)
	case len(req.GetAcks()) > maxRelayAcks:
		relayClaims.WithLabelValues("refused").Inc()
		return nil, status.Errorf(codes.InvalidArgument, "a claim acks at most %d ids", maxRelayAcks)
	case most == 0:
		most = maxRelayDeliveries
	}
	feeds := make([]model.RelayFeedOn, 0, len(req.GetFeeds()))
	for _, f := range req.GetFeeds() {
		if !localWorkspacePattern.MatchString(f.GetLocalWorkspaceId()) {
			relayClaims.WithLabelValues("refused").Inc()
			return nil, status.Error(codes.InvalidArgument, "feeds name local workspaces by id")
		}
		feeds = append(feeds, model.RelayFeedOn{LocalWorkspaceID: f.GetLocalWorkspaceId(), On: f.GetOn() && cfg.ReadsHistory()})
	}
	if err := s.deps.DB.SeeFoundryRelayLink(ctx, link.ID, truncateUTF8(req.GetClientAddress(), maxRelayAddress)); err != nil {
		log.From(ctx).Warn("record a relay link's contact failed", "link_id", link.ID, "err", err)
	}
	fail := func(what string, err error) (*foundryv1.ClaimFoundryRelayDeliveriesResponse, error) {
		relayClaims.WithLabelValues("failed").Inc()
		log.From(ctx).Error("a relay claim failed", "link_id", link.ID, "step", what, "err", err)
		return nil, status.Errorf(codes.Unavailable, "%s: try the claim again", what)
	}
	acked, err := s.deps.DB.AckFoundryRelay(ctx, link.ID, req.GetAcks())
	if err != nil {
		return fail("ack what the install took", err)
	}
	if len(feeds) > 0 {
		if err := s.deps.DB.SetFoundryRelayFeeds(ctx, link.ID, feeds); err != nil {
			return fail("record the install's feeds", err)
		}
	}
	budget := model.RelayBudget{Deliveries: most}
	if cfg.ReadsHistory() {
		budget.FeedRows, budget.FeedBytes = cfg.Relay.FeedBatch, cfg.Relay.FeedBatchBytes
	}
	claim, err := s.leaseRelay(ctx, link.ID, min(wait, cfg.Relay.ClaimWait), cfg.Relay.ClaimPoll, budget)
	if err != nil {
		return fail("lease what waits", err)
	}
	installs, err := s.deps.DB.ListFoundryRelayInstalls(ctx, link.ID)
	if err != nil {
		return fail("read the link's installs", err)
	}
	out := &foundryv1.ClaimFoundryRelayDeliveriesResponse{Acked: int32(acked), HostedFeed: cfg.Feed.Enabled, HostedFollow: cfg.Follow.Enabled}
	for _, d := range claim.Deliveries {
		out.Deliveries = append(out.Deliveries, &foundryv1.FoundryRelayDelivery{
			Id: d.ID, InstallId: d.InstallID, LocalWorkspaceId: d.LocalWorkspaceID, Kind: string(d.Kind), DedupeKey: d.DedupeKey,
			Conversation: d.Conversation, SlackUserId: d.SlackUserID, PayloadJson: string(d.Payload),
		})
	}
	for _, item := range claim.Feed {
		switch it := item.(type) {
		case model.RelayFeedRow:
			out.Feed = append(out.Feed, &foundryv1.FoundryRelayFeedRow{
				Id: it.ID, InstallId: it.InstallID, LocalWorkspaceId: it.LocalWorkspaceID, DedupeKey: it.Row.DedupeKey, ChannelId: it.Row.ChannelID,
				Day: timestamppb.New(it.Row.Day), Kind: string(it.Row.Kind), PayloadJson: string(it.Row.Payload),
			})
		case model.RelayFeedGap:
			out.FeedGaps = append(out.FeedGaps, &foundryv1.FoundryRelayFeedGap{
				Id: it.ID, InstallId: it.InstallID, LocalWorkspaceId: it.LocalWorkspaceID, ChannelId: it.Gap.ChannelID,
				From: timestamppb.New(it.Gap.From), To: timestamppb.New(it.Gap.To),
			})
		}
	}
	for _, in := range installs {
		relayed, _ := in.Destination.(model.SlackServesRelay)
		out.Installs = append(out.Installs, &foundryv1.FoundryRelayInstall{
			InstallId: in.ID, LocalWorkspaceId: relayed.LocalWorkspaceID, TeamId: in.TeamID, EnterpriseId: in.EnterpriseID, TeamName: in.TeamName,
			AppId: in.AppID, BotUserId: in.BotUserID, BotId: in.BotID, InstallerAdmin: in.InstallerAdmin, Scopes: in.Scopes,
			InstalledBySlackUserId: in.InstalledBySlackUserID, CreatedAt: timestamppb.New(in.CreatedAt), FeedOn: in.FeedOn,
		})
	}
	outcome := "empty"
	if len(claim.Deliveries)+len(claim.Feed) > 0 {
		outcome = "delivered"
	}
	relayClaims.WithLabelValues(outcome).Inc()
	return out, nil
}

// leaseRelay waits up to wait for something to wait for the link, looking
// at once and then every poll without a lock, and leases it once it does.
// A lease that finds nothing (another claim took it) looks again. It
// returns nothing when wait passes, or the claim's caller goes, first.
func (s *Service) leaseRelay(ctx context.Context, linkID string, wait, poll time.Duration, budget model.RelayBudget) (model.RelayClaim, error) {
	deadline := time.Now().Add(wait)
	for {
		waiting, err := s.deps.DB.FoundryRelayWaiting(ctx, linkID)
		if err != nil {
			return model.RelayClaim{}, err
		}
		if waiting {
			claim, err := s.deps.DB.LeaseFoundryRelay(ctx, linkID, relayLease, budget)
			if err != nil || len(claim.Deliveries)+len(claim.Feed) > 0 {
				return claim, err
			}
		}
		left := time.Until(deadline)
		if left <= 0 {
			return model.RelayClaim{}, nil
		}
		timer := time.NewTimer(min(poll, left))
		select {
		case <-ctx.Done():
			timer.Stop()
			return model.RelayClaim{}, nil
		case <-timer.C:
		}
	}
}

// StartFoundryRelaySlackInstall starts an install of Helix's app for one of
// a linked install's workspaces, as the member who linked it, still an
// owner or admin: its OAuth state, bound to the link and the local
// workspace, returns to the link's own Settings › Slack once the site
// finishes it.
func (s *Service) StartFoundryRelaySlackInstall(ctx context.Context, req *foundryv1.StartFoundryRelaySlackInstallRequest) (
	*foundryv1.StartFoundryRelaySlackInstallResponse, error) {
	link, err := s.relayLink(ctx, req.GetTokenSha256())
	if err != nil {
		return nil, err
	}
	lws := req.GetLocalWorkspaceId()
	switch {
	case !localWorkspacePattern.MatchString(lws):
		return nil, status.Error(codes.InvalidArgument, "local_workspace_id is the install's workspace id")
	case req.GetReturnTo() != link.ReturnTo():
		return nil, status.Errorf(codes.InvalidArgument, "return_to is the linked install's Settings › Slack, %s", link.ReturnTo())
	}
	ws := link.WorkspaceID
	if err := s.requireSlackManager(ctx, ws, link.CreatedByUserID); err != nil {
		return nil, status.Error(codes.PermissionDenied, "the member who linked this install is no longer an owner or admin of its Helix workspace")
	}
	if !s.deps.Cfg.Foundry.Slack.InstallAllowed(ws) {
		return nil, foundryPrecondition(ws, "FOUNDRY_SLACK_NOT_OFFERED", "Helix Foundry for Slack isn't offered to this workspace yet")
	}
	required := s.slackRequired()
	switch install, err := s.deps.DB.GetFoundryRelayInstall(ctx, link.ID, lws); {
	case err == nil && !slices.ContainsFunc(required, func(scope string) bool { return !slices.Contains(install.Scopes, scope) }):
		return nil, foundryPrecondition(ws, "FOUNDRY_SLACK_INSTALLED", "this workspace is connected to Slack already; disconnect it first")
	case err != nil && !errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Errorf(codes.Unavailable, "read the relayed install: %v", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, status.Errorf(codes.Internal, "make the install's state: %v", err)
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	switch err := s.deps.DB.CreateFoundrySlackRelayInstallState(ctx, link.ID, lws, hashToken(state), slackStateTTL); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.Unauthenticated, "this relay token is revoked or unknown")
	case err != nil:
		return nil, status.Errorf(codes.Unavailable, "record the install's state: %v", err)
	}
	cfg := s.deps.Cfg.Foundry.Slack
	query := url.Values{"client_id": {cfg.ClientID}, "scope": {strings.Join(required, ",")},
		"redirect_uri": {slackRedirectURI(cfg.SiteURL)}, "state": {state}}
	return &foundryv1.StartFoundryRelaySlackInstallResponse{AuthorizeUrl: s.slackURL() + "/oauth/v2/authorize?" + query.Encode()}, nil
}

// queueRelayDelivery queues in, a request for the relayed install, as a
// delivery its local install claims within foundry.slack.relay.delivery_ttl,
// recording what it lets the install's calls name (seen). A retry is a
// duplicate; an install revoked meanwhile drops it.
func (s *Service) queueRelayDelivery(ctx context.Context, in *model.SlackIntake, seen model.RelaySeen) (
	*foundryv1.ReceiveSlackRequestResponse, string, error) {
	cfg := s.deps.Cfg.Foundry.Slack
	if !cfg.Relay.Enabled {
		relayDeliveries.WithLabelValues("disabled").Inc()
		return slackOK(), "ignored", nil
	}
	d := &model.RelayDelivery{ID: in.ID, InstallID: in.InstallID, DedupeKey: in.DedupeKey, Kind: in.Kind, Conversation: in.Conversation,
		SlackUserID: in.SlackUserID, Payload: in.Payload}
	switch err := s.deps.DB.QueueFoundryRelayDelivery(ctx, d, cfg.Relay.DeliveryTTL, seen); {
	case errors.Is(err, foundrystore.ErrConflict):
		relayDeliveries.WithLabelValues("duplicate").Inc()
		return slackOK(), "duplicate", nil
	case errors.Is(err, foundrystore.ErrNotFound):
		relayDeliveries.WithLabelValues("gone").Inc()
		return slackOK(), "unknown_team", nil
	case err != nil:
		return nil, "", err
	}
	relayDeliveries.WithLabelValues("queued").Inc()
	return slackOK(), "relayed", nil
}

// slackMentionPattern is a user mention in Slack's mrkdwn: <@U…> or
// <@U…|name>.
var slackMentionPattern = regexp.MustCompile(`<@([UW][A-Z0-9]{2,31})(?:\|[^>]*)?>`)

// relaySeen is what a message tells the relay its install's calls may
// name: its channel, unless shared with another organisation, and its
// sender and the users it mentions.
func relaySeen(at slack.Conversation, sender, text string) model.RelaySeen {
	seen := model.RelaySeen{UserIDs: []string{sender}}
	if !at.ExtShared {
		seen.ChannelID = at.ChannelID
	}
	for _, m := range slackMentionPattern.FindAllStringSubmatch(text, 64) {
		seen.UserIDs = append(seen.UserIDs, m[1])
	}
	return seen
}

// ListFoundryRelayLinks lists the workspace's linked installs, for owners
// and admins, with their Slack installs and today's counts.
func (s *Service) ListFoundryRelayLinks(ctx context.Context, req *foundryv1.ListFoundryRelayLinksRequest) (*foundryv1.ListFoundryRelayLinksResponse, error) {
	ws := req.GetWorkspaceId()
	if ws == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	if _, err := s.relayManager(ctx, ws); err != nil {
		return nil, err
	}
	links, err := s.deps.DB.ListFoundryRelayLinks(ctx, ws)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the relay links: %v", err)
	}
	out := &foundryv1.ListFoundryRelayLinksResponse{}
	today := time.Now().UTC()
	for _, l := range links {
		installs, err := s.deps.DB.ListFoundryRelayInstalls(ctx, l.ID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read a relay link's installs: %v", err)
		}
		usage, err := s.deps.DB.GetFoundryRelayUsage(ctx, l.ID, today)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read a relay link's counts: %v", err)
		}
		p := &foundryv1.FoundryRelayLink{
			Id: l.ID, Name: l.Name, CreatedByUserId: l.CreatedByUserID, CreatedByName: s.slackUserName(ctx, l.CreatedByUserID),
			CreatedAt: timestamppb.New(l.CreatedAt), DeliveriesToday: usage.Deliveries, CallsToday: usage.Calls, RefusedToday: usage.Refused,
			FeedRowsToday: usage.FeedRows,
		}
		if l.LastSeenAt != nil {
			p.LastSeenAt = timestamppb.New(*l.LastSeenAt)
		}
		for _, in := range installs {
			p.Installs = append(p.Installs, &foundryv1.FoundryRelayLinkInstall{TeamId: in.TeamID, TeamName: in.TeamName, CreatedAt: timestamppb.New(in.CreatedAt)})
		}
		out.Links = append(out.Links, p)
	}
	return out, nil
}

// RevokeFoundryRelayLink revokes a linked install, for an owner or admin:
// its token stops working, and its installs are revoked, which the Slack
// worker's sweep removes from Slack.
func (s *Service) RevokeFoundryRelayLink(ctx context.Context, req *foundryv1.RevokeFoundryRelayLinkRequest) (*emptypb.Empty, error) {
	ws := req.GetWorkspaceId()
	if ws == "" || req.GetLinkId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id and link_id are required")
	}
	if _, err := s.relayManager(ctx, ws); err != nil {
		return nil, err
	}
	switch err := s.deps.DB.RevokeFoundryRelayLink(ctx, ws, req.GetLinkId()); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "no such linked install")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "revoke the relay link: %v", err)
	}
	log.From(ctx).Info("foundry relay link revoked", "workspace_id", ws, "link_id", req.GetLinkId())
	return &emptypb.Empty{}, nil
}

// relayManager is an owner or admin of workspaceID calling hosted's relay.
func (s *Service) relayManager(ctx context.Context, workspaceID string) (string, error) {
	userID, err := s.slackPerson(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	if err := s.relayOn(workspaceID); err != nil {
		return "", err
	}
	return userID, s.requireSlackManager(ctx, workspaceID, userID)
}

// truncateUTF8 is s cut to at most n bytes, on a character's start.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
