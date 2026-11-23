package local

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/local/directory"
	directoryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/directory/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store"
)

// AgentSessions opens the Assistant's sessions on a standalone install
// (integration.SessionHost): the owner's tools, as the install's MCP gives
// them, and Founder's own API (integration.FounderAPI), both calling the
// install's own gRPC with the site token, inside this process only. Its
// confirmations bind the owner, so the worker that prepares an action and
// the API that runs it match.
type AgentSessions struct {
	auth      *Authenticator
	service   *integration.Service
	founder   integration.FounderAPI
	directory directoryv1.DirectoryServiceClient
	siteToken string
}

var _ integration.SessionHost = (*AgentSessions)(nil)

// NewAgentSessions opens sessions on service (LocalService's) as the owner
// auth authenticates, with founder (the install's gRPC client) as their
// Founder and siteToken on every call.
func NewAgentSessions(auth *Authenticator, service *integration.Service, founder integration.FounderAPI, directory directoryv1.DirectoryServiceClient,
	siteToken string) *AgentSessions {
	return &AgentSessions{auth: auth, service: service.ForAgent(), founder: founder, directory: directory, siteToken: siteToken}
}

// Session opens the owner's tools in actor's workspace, and that workspace
// alone: a call naming another is not found, though the owner has it, so a
// turn reads and prepares only in its own. Any other user, or a workspace the
// owner no longer has, is refused: an install has one person.
func (s *AgentSessions) Session(ctx context.Context, actor ports.Actor, confirm integration.ConfirmMode) (integration.Session, error) {
	owner, err := s.auth.directory.Owner(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return integration.Session{}, status.Error(codes.PermissionDenied, "the install has no owner")
	case err != nil:
		return integration.Session{}, err
	case actor.Source != ports.SourceUser || actor.UserID != owner.ID:
		return integration.Session{}, status.Error(codes.PermissionDenied, "Founder acts only for the install's owner")
	}
	w, err := s.auth.directory.Workspace(ctx, actor.WorkspaceID)
	switch {
	case errors.Is(err, store.ErrNotFound) || err == nil && (w.DeletedAt != nil || w.Status == directory.StatusDeleted):
		return integration.Session{}, status.Error(codes.NotFound, "workspace not found")
	case err != nil:
		return integration.Session{}, err
	}
	// The owner, as the front end's gate makes them: what the confirmations bind.
	ctx = ports.WithActor(ctx, ports.Actor{Source: ports.SourceUser, UserID: owner.ID, WorkspaceID: actor.WorkspaceID,
		QueryRead: ports.AllProjects(), QueryWrite: ports.AllProjects()})
	// Only the site token goes to gRPC, which checks it as it checks the site's.
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+s.siteToken))
	identity := ownerIdentity(s.auth, func(context.Context) string { return actor.WorkspaceID })
	identity.Confirm = confirm
	authorize := identity.Authorize
	identity.Authorize = func(ctx context.Context, req any, grant integration.Grant, methods ...string) error {
		if r, ok := req.(interface{ GetWorkspaceId() string }); ok && r.GetWorkspaceId() != actor.WorkspaceID {
			return status.Error(codes.NotFound, "resource not found")
		}
		return authorize(ctx, req, grant, methods...)
	}
	identity.Allows = func(workspaceID, _ string, _ integration.Grant) bool { return workspaceID == actor.WorkspaceID }
	return integration.Session{Ctx: ctx, Service: s.service, Identity: identity, Founder: s.founder, HostTools: func(server *mcp.Server) {
		addLocalDiscovery(s.service, server, identity, s.directory, actor.WorkspaceID)
	}}, nil
}
