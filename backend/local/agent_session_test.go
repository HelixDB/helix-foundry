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
	return NewAgentSessions(auth, service, nil, strings.Repeat("t", MinSiteToken)), auth
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
