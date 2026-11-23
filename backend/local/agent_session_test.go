package local

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/integration"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
)

func agentSessions(t *testing.T) (*AgentSessions, *Authenticator) {
	t.Helper()
	auth, err := NewAuthenticator(strings.Repeat("t", MinSiteToken), testDirectory(), fakeOntologies{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	service := &integration.Service{ResourceURI: mcpAudience, SignIn: true}
	return NewAgentSessions(auth, service, &founderCalls{}, nil, strings.Repeat("t", MinSiteToken)), auth
}

// founderCalls is the install's gRPC client as Founder's API, keeping each
// call's context.
type founderCalls struct {
	foundryv1.FoundryServiceClient
	ctxs []context.Context
}

func (f *founderCalls) SendFoundryAgentMessage(ctx context.Context, _ *foundryv1.SendFoundryAgentMessageRequest, _ ...grpc.CallOption) (
	*foundryv1.FoundryAgentTurnResponse, error) {
	f.ctxs = append(f.ctxs, ctx)
	return &foundryv1.FoundryAgentTurnResponse{}, nil
}

// A session's Founder is the install's own API, and its calls, made with the
// session's context, pass the install's gate as the owner: Slack's turns and
// clicks act as the owner and only in the session's workspace.
func TestAgentSessionsFounderActsAsTheOwner(t *testing.T) {
	sessions, auth := agentSessions(t)
	ctx := context.Background()
	session, err := sessions.Session(ctx, ports.Actor{Source: ports.SourceUser, UserID: "owner", WorkspaceID: "ws"}, integration.ConfirmByCaller{})
	require.NoError(t, err)
	require.Same(t, sessions.founder, session.Founder)
	_, err = session.Founder.SendFoundryAgentMessage(session.Ctx, &foundryv1.SendFoundryAgentMessageRequest{WorkspaceId: "ws",
		Input: &foundryv1.SendFoundryAgentMessageRequest_Text{Text: "Hi"}})
	require.NoError(t, err)
	calls := sessions.founder.(*founderCalls).ctxs
	require.Len(t, calls, 1)
	out, _ := metadata.FromOutgoingContext(calls[0])
	incoming := metadata.NewIncomingContext(ctx, out)
	for method, req := range map[string]any{
		foundryv1.FoundryService_SendFoundryAgentMessage_FullMethodName:    &foundryv1.SendFoundryAgentMessageRequest{WorkspaceId: "ws"},
		foundryv1.FoundryService_ListFoundryAgentTurnEvents_FullMethodName: &foundryv1.ListFoundryAgentTurnEventsRequest{WorkspaceId: "ws"},
		foundryv1.FoundryService_ExecuteFoundryAgentAction_FullMethodName:  &foundryv1.ExecuteFoundryAgentActionRequest{WorkspaceId: "ws"},
		foundryv1.FoundryService_DismissFoundryAgentAction_FullMethodName:  &foundryv1.DismissFoundryAgentActionRequest{WorkspaceId: "ws"},
		foundryv1.FoundryService_RefreshFoundryAgentAction_FullMethodName:  &foundryv1.RefreshFoundryAgentActionRequest{WorkspaceId: "ws"},
		foundryv1.FoundryService_CancelFoundryAgentTurn_FullMethodName:     &foundryv1.CancelFoundryAgentTurnRequest{WorkspaceId: "ws"},
	} {
		gated, err := auth.gate(incoming, method, req)
		require.NoError(t, err, method)
		actor, _ := ports.ActorFrom(gated)
		assert.Equal(t, "owner", actor.UserID, method)
	}
	// Without the session's context, the same client is refused at the gate.
	_, err = auth.gate(metadata.NewIncomingContext(ctx, metadata.MD{}), foundryv1.FoundryService_SendFoundryAgentMessage_FullMethodName,
		&foundryv1.SendFoundryAgentMessageRequest{WorkspaceId: "ws"})
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestAgentSessionsAreTheOwnersAlone(t *testing.T) {
	sessions, auth := agentSessions(t)
	ctx := context.Background()
	owner := ports.Actor{Source: ports.SourceUser, UserID: "owner", WorkspaceID: "ws"}
	for name, actor := range map[string]ports.Actor{
		"another user":       {Source: ports.SourceUser, UserID: "nameless", WorkspaceID: "ws"},
		"an agent's key":     {Source: ports.SourceMCP, ServiceCredentialID: "key", WorkspaceID: "ws"},
		"a deleted space":    {Source: ports.SourceUser, UserID: "owner", WorkspaceID: "gone"},
		"no such workspace":  {Source: ports.SourceUser, UserID: "owner", WorkspaceID: "nowhere"},
		"the owner as agent": {Source: ports.SourceGrantee, UserID: "owner", WorkspaceID: "ws"},
	} {
		_, err := sessions.Session(ctx, actor, integration.ConfirmByCaller{})
		assert.Contains(t, []codes.Code{codes.PermissionDenied, codes.NotFound}, status.Code(err), name)
	}

	sink := integration.ConfirmByPerson{}
	session, err := sessions.Session(ctx, owner, sink)
	require.NoError(t, err)
	assert.Equal(t, integration.AgentAudience, session.Service.ResourceURI, "its confirmations are the Assistant's")
	assert.False(t, session.Service.SignIn)
	assert.Equal(t, sink, session.Identity.Confirm)
	assert.True(t, session.Identity.User)
	assert.Equal(t, "ws", session.Identity.OnlyWorkspace(ctx), "a turn is of one workspace")

	// Its confirmations bind the owner, and its calls pass the install's gate as the owner.
	a, ok := ports.ActorFrom(session.Ctx)
	require.True(t, ok)
	assert.Equal(t, "owner", a.UserID)
	out, _ := metadata.FromOutgoingContext(session.Ctx)
	gated, err := auth.gate(metadata.NewIncomingContext(ctx, out), foundryv1.FoundryService_ListFoundryOntologies_FullMethodName,
		&foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws"})
	require.NoError(t, err)
	gatedActor, _ := ports.ActorFrom(gated)
	assert.Equal(t, "owner", gatedActor.UserID)

	// The host's tools are the install's discovery.
	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "1"}, nil)
	session.HostTools(server)
	client := connectTo(t, server)
	tools, err := client.ListTools(ctx, nil)
	require.NoError(t, err)
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	assert.Subset(t, names, []string{"helix_list_projects", "helix_list_databases"})

	// A turn is of its workspace alone, though the owner has others.
	err = session.Identity.Authorize(ctx, &foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "other"}, integration.ProjectRead,
		"ListFoundryOntologies")
	assert.Equal(t, codes.NotFound, status.Code(err), "a read of another workspace")
	err = session.Identity.Authorize(ctx, &foundryv1.CreateFoundryLoadRequest{WorkspaceId: "other", OntologyId: "o"}, integration.QueryWrite,
		"CreateFoundryLoad")
	assert.Equal(t, codes.NotFound, status.Code(err), "an action on another workspace")
	assert.NoError(t, session.Identity.Authorize(ctx, &foundryv1.ListFoundryOntologiesRequest{WorkspaceId: "ws"}, integration.ProjectRead,
		"ListFoundryOntologies"))
	assert.False(t, session.Identity.Allows("other", "p", integration.ProjectRead))
	assert.True(t, session.Identity.Allows("ws", "p", integration.ProjectRead))
	listed, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "helix_list_projects", Arguments: map[string]any{"workspace_id": "other"}})
	require.NoError(t, err)
	assert.True(t, listed.IsError, "the host's tools name its workspace alone")
}

func connectTo(t *testing.T, server *mcp.Server) *mcp.ClientSession {
	t.Helper()
	serverSide, clientSide := mcp.NewInMemoryTransports()
	_, err := server.Connect(context.Background(), serverSide, nil)
	require.NoError(t, err)
	client, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).Connect(context.Background(), clientSide, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}
