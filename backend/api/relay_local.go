package api

import (
	"context"
	"errors"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/slack"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// A standalone install's side of Helix's Slack relay: its owner links it to
// a Helix workspace (Deps.Relay, package local/relay, does the work), and
// chooses which Slack users who asked may use it, read-only, as the owner.
// Hosted Helix serves none of it.

// RelayLinker is a standalone install's link to Helix's relay, which the
// owner's Settings › Slack drives. Its methods act on the install's one
// link, whichever workspace the call came from.
type RelayLinker interface {
	// Relay is the link as it stands.
	Relay(ctx context.Context) (RelayState, error)
	// StartLink starts linking: a state and PKCE verifier kept for the
	// return, and Helix's page asking an owner or admin to link the install,
	// by its name, to one of their workspaces.
	StartLink(ctx context.Context) (authorizeURL string, err error)
	// CompleteLink redeems the code Helix handed back for state with its
	// verifier, and keeps the token: store.ErrNotFound for a state unknown,
	// spent or expired.
	CompleteLink(ctx context.Context, code, state string) error
	// SetSlackApprovals turns approving changes from Slack on or off:
	// store.ErrNotFound while unlinked.
	SetSlackApprovals(ctx context.Context, on bool) error
	// Unlink revokes the link at Helix, best effort, and forgets it.
	Unlink(ctx context.Context) error
	// StartSlackInstall starts adding Helix's Slack app for the local
	// workspace workspaceID, through Helix: Slack's page to send the owner
	// to, which Helix finishes and sends back to Settings › Slack;
	// store.ErrNotFound while unlinked.
	StartSlackInstall(ctx context.Context, workspaceID string) (authorizeURL string, err error)
	// Slack is a Web API client that calls Slack through Helix's relay, and
	// install's bot it calls as: store.ErrNotFound once the install is gone.
	Slack(ctx context.Context, install *model.SlackInstall) (*slack.Client, slack.Bot, error)
}

// RelayState is a standalone install's link to Helix: Linked false and the
// rest zero while unlinked.
type RelayState struct {
	Linked         bool
	HostedOrigin   string
	AccountEmail   string
	LinkedAt       time.Time
	LastOKAt       *time.Time
	SlackApprovals bool
}

func relayToProto(r RelayState) *foundryv1.FoundryRelayResponse {
	out := &foundryv1.FoundryRelay{Linked: r.Linked}
	if !r.Linked {
		return &foundryv1.FoundryRelayResponse{Relay: out}
	}
	out.HostedOrigin, out.AccountEmail, out.SlackApprovals = r.HostedOrigin, r.AccountEmail, r.SlackApprovals
	out.LinkedAt = timestamppb.New(r.LinkedAt)
	if r.LastOKAt != nil {
		out.LastOkAt = timestamppb.New(*r.LastOKAt)
	}
	return &foundryv1.FoundryRelayResponse{Relay: out}
}

// localRelay is the install's link for its owner: FAILED_PRECONDITION
// FOUNDRY_RELAY_LOCAL_ONLY where the host has none (hosted Helix).
func (s *Service) localRelay(ctx context.Context, workspaceID string) (RelayLinker, error) {
	if workspaceID == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	userID, err := s.slackPerson(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if s.deps.Relay == nil {
		return nil, foundryPrecondition(workspaceID, "FOUNDRY_RELAY_LOCAL_ONLY", "a Foundry install on your computer links to Helix's relay; Helix has no relay of its own")
	}
	if err := s.requireSlackManager(ctx, workspaceID, userID); err != nil {
		return nil, err
	}
	return s.deps.Relay, nil
}

// relayAnswer is the link as it stands once a call changed it.
func (s *Service) relayAnswer(ctx context.Context, relay RelayLinker) (*foundryv1.FoundryRelayResponse, error) {
	state, err := relay.Relay(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "read the link to Helix: %v", err)
	}
	return relayToProto(state), nil
}

func (s *Service) GetFoundryRelay(ctx context.Context, req *foundryv1.GetFoundryRelayRequest) (*foundryv1.FoundryRelayResponse, error) {
	relay, err := s.localRelay(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	return s.relayAnswer(ctx, relay)
}

// StartFoundryRelayLink starts linking the install to Helix.
func (s *Service) StartFoundryRelayLink(ctx context.Context, req *foundryv1.StartFoundryRelayLinkRequest) (*foundryv1.StartFoundryRelayLinkResponse, error) {
	relay, err := s.localRelay(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	authorize, err := relay.StartLink(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "start linking to Helix: %v", err)
	}
	return &foundryv1.StartFoundryRelayLinkResponse{AuthorizeUrl: authorize}, nil
}

// CompleteFoundryRelayLink finishes linking with what Helix handed back.
func (s *Service) CompleteFoundryRelayLink(ctx context.Context, req *foundryv1.CompleteFoundryRelayLinkRequest) (*foundryv1.FoundryRelayResponse, error) {
	relay, err := s.localRelay(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	if req.GetCode() == "" || len(req.GetCode()) > maxSlackArg || !relayStatePattern.MatchString(req.GetState()) {
		return nil, status.Error(codes.InvalidArgument, "code and state are what Helix handed back")
	}
	switch err := relay.CompleteLink(ctx, req.GetCode(), req.GetState()); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, foundryPrecondition(req.GetWorkspaceId(), "FOUNDRY_RELAY_LINK_EXPIRED", "this link expired or was finished already: link to Helix again")
	case err != nil:
		return nil, status.Errorf(codes.Unavailable, "finish linking to Helix: %v", err)
	}
	log.From(ctx).Info("foundry relay linked to Helix")
	return s.relayAnswer(ctx, relay)
}

// UpdateFoundryRelay turns approving changes from Slack on or off.
func (s *Service) UpdateFoundryRelay(ctx context.Context, req *foundryv1.UpdateFoundryRelayRequest) (*foundryv1.FoundryRelayResponse, error) {
	relay, err := s.localRelay(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	switch err := relay.SetSlackApprovals(ctx, req.GetSlackApprovals()); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "this Foundry isn't linked to Helix")
	case err != nil:
		return nil, status.Errorf(codes.Unavailable, "change approving from Slack: %v", err)
	}
	return s.relayAnswer(ctx, relay)
}

// UnlinkFoundryRelay unlinks the install from Helix.
func (s *Service) UnlinkFoundryRelay(ctx context.Context, req *foundryv1.UnlinkFoundryRelayRequest) (*foundryv1.FoundryRelayResponse, error) {
	relay, err := s.localRelay(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	if err := relay.Unlink(ctx); err != nil {
		return nil, status.Errorf(codes.Unavailable, "unlink from Helix: %v", err)
	}
	log.From(ctx).Info("foundry relay unlinked from Helix")
	return s.relayAnswer(ctx, relay)
}

// SetFoundrySlackAccess lets a Slack user who asked use a standalone
// install read-only, as its owner (READ), or removes them, or their request
// (NONE), for the owner. The owner's own link is never changed here.
func (s *Service) SetFoundrySlackAccess(ctx context.Context, req *foundryv1.SetFoundrySlackAccessRequest) (*foundryv1.FoundrySlackAccessResponse, error) {
	ws, slackUser, access := req.GetWorkspaceId(), req.GetSlackUserId(), req.GetAccess()
	switch {
	case ws == "" || !model.ValidSlackID(slackUser):
		return nil, status.Error(codes.InvalidArgument, "workspace_id and slack_user_id are required")
	case access != foundryv1.FoundrySlackLinkAccess_FOUNDRY_SLACK_LINK_ACCESS_READ && access != foundryv1.FoundrySlackLinkAccess_FOUNDRY_SLACK_LINK_ACCESS_NONE:
		return nil, status.Error(codes.InvalidArgument, "access is READ or NONE")
	}
	userID, err := s.slackPerson(ctx, ws)
	if err != nil {
		return nil, err
	}
	if !s.deps.Cfg.Host.Local {
		return nil, foundryPrecondition(ws, "FOUNDRY_SLACK_LOCAL_ONLY", "on Helix each person links their own Slack account")
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
	link, err := s.deps.DB.GetFoundrySlackLink(ctx, install.ID, slackUser)
	switch {
	case err == nil && link.Access == model.SlackFullAccess{}:
		return nil, foundryPrecondition(ws, "FOUNDRY_SLACK_OWNER_LINK", "that is the owner's own Slack account, which unlinks from Settings › Slack")
	case err != nil && !errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Errorf(codes.Internal, "read the Slack link: %v", err)
	}
	requests, err := s.deps.DB.ListFoundrySlackAccessRequests(ctx, install.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the Slack access requests: %v", err)
	}
	asked := slices.ContainsFunc(requests, func(r *model.SlackAccessRequest) bool { return r.SlackUserID == slackUser })
	if link == nil && !asked {
		return nil, status.Error(codes.NotFound, "that Slack user hasn't asked to use this Foundry")
	}
	if access == foundryv1.FoundrySlackLinkAccess_FOUNDRY_SLACK_LINK_ACCESS_NONE {
		if link != nil {
			if err := s.deps.DB.DeleteFoundrySlackLink(ctx, install.ID, slackUser); err != nil && !errors.Is(err, foundrystore.ErrNotFound) {
				return nil, status.Errorf(codes.Internal, "remove the Slack user: %v", err)
			}
		}
		if err := s.deps.DB.DeleteFoundrySlackAccessRequest(ctx, install.ID, slackUser); err != nil && !errors.Is(err, foundrystore.ErrNotFound) {
			return nil, status.Errorf(codes.Internal, "drop the access request: %v", err)
		}
		log.From(ctx).Info("foundry slack access removed", "workspace_id", ws, "install_id", install.ID)
		return &foundryv1.FoundrySlackAccessResponse{}, nil
	}
	allowed, err := s.deps.DB.AllowFoundrySlackUser(ctx, install.ID, slackUser, userID)
	switch {
	case errors.Is(err, foundrystore.ErrConflict):
		return nil, foundryPrecondition(ws, "FOUNDRY_SLACK_OWNER_LINK", "that is the owner's own Slack account, which unlinks from Settings › Slack")
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "this workspace isn't connected to Slack")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "allow the Slack user: %v", err)
	}
	log.From(ctx).Info("foundry slack access allowed", "workspace_id", ws, "install_id", install.ID)
	return &foundryv1.FoundrySlackAccessResponse{Link: s.slackLinkToProto(ctx, allowed)}, nil
}
