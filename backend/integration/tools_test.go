package integration

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// everyTool is a user's identity on a host that grants everything, so every
// tool registers.
func everyTool() *Identity {
	return &Identity{User: true, HasPermission: func(Permission) bool { return true }}
}

// connect is a client of a server holding s's tools for identity.
func connect(t *testing.T, s *Service, identity *Identity) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	s.AddTools(server, identity)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	_, err := server.Connect(ctx, serverTransport, nil)
	require.NoError(t, err)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// What an agent reads of the tools names API sources only where it can use them.
func TestToolsNameAPISourcesOnlyOnAHostWithThem(t *testing.T) {
	listed := func(s *Service) string {
		tools, err := connect(t, s, everyTool()).ListTools(context.Background(), nil)
		require.NoError(t, err)
		raw, err := json.Marshal(tools.Tools)
		require.NoError(t, err)
		return string(raw)
	}
	words := []string{"API source", "create_event_source", "event_source", "send_pieces", "send token", "kind events", "or events"}
	local := listed(&Service{FoundryURL: "http://localhost:3000"})
	for _, word := range words {
		assert.NotContains(t, local, word)
	}
	assert.Contains(t, local, "One of 12 operations: create_ontology, sync_connector,")
	assert.Contains(t, local, "generate_ontology: {goal, connector_ids, document_set_ids, no_connectors}")
	assert.Contains(t, local, "or s3.")
	hosted := listed(&Service{FoundryURL: "https://foundry.example", Events: true})
	for _, word := range words {
		assert.Contains(t, hosted, word)
	}
	assert.Contains(t, hosted, "One of 13 operations: create_ontology, create_event_source, sync_connector,")
}

// The default runner answers clients that read only content, not just those
// that read structuredContent.
func TestRunPutsTheOutputInContent(t *testing.T) {
	s := &Service{}
	want := FoundryReadGuideOutput{Notes: "from the tool", MaxReads: 3}
	result, out, err := s.Run(context.Background(), "tool", func(context.Context, *Call) (any, int, error) { return want, 1, nil })
	require.NoError(t, err)
	assert.Equal(t, want, out)
	require.Len(t, result.Content, 2)
	assert.Equal(t, untrustedContentNotice, result.Content[0].(*mcp.TextContent).Text)
	var got FoundryReadGuideOutput
	require.NoError(t, json.Unmarshal([]byte(result.Content[1].(*mcp.TextContent).Text), &got))
	assert.Equal(t, want.Notes, got.Notes)
	assert.Equal(t, want.MaxReads, got.MaxReads)
}

func TestRunGivesAFailureItsStableCode(t *testing.T) {
	s := &Service{}
	_, _, err := s.Run(context.Background(), "tool", func(context.Context, *Call) (any, int, error) { return nil, 0, errors.New("private") })
	require.EqualError(t, err, "internal_error")
	_, _, err = s.Run(context.Background(), "tool", func(context.Context, *Call) (any, int, error) { return make(chan int), 1, nil })
	require.EqualError(t, err, "internal_error", "an output that can't be encoded")
}

// connectorsBackend lists one connector of each sort, and fails the test's
// call if the API source's batches are read.
type connectorsBackend struct {
	foundryv1.FoundryServiceClient
	batches int
}

func (b *connectorsBackend) ListConnectors(context.Context, *foundryv1.ListConnectorsRequest, ...grpc.CallOption) (*foundryv1.ListConnectorsResponse, error) {
	return &foundryv1.ListConnectorsResponse{Connectors: []*foundryv1.Connector{
		{Id: "stripe-1", Kind: "stripe", Name: "Stripe"},
		{Id: "source-1", Kind: "events", Name: "Source", Config: &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Events{Events: &foundryv1.EventsConnectorConfig{}}}},
	}}, nil
}

func (b *connectorsBackend) ListFoundryEventBatches(context.Context, *foundryv1.ListFoundryEventBatchesRequest, ...grpc.CallOption) (*foundryv1.ListFoundryEventBatchesResponse, error) {
	b.batches++
	return &foundryv1.ListFoundryEventBatchesResponse{}, nil
}

// A host without API sources doesn't serve their batches, so listing connectors
// neither asks to call that method nor calls it, whatever the connectors are.
func TestListConnectorsReadsBatchesOnlyOnAHostWithAPISources(t *testing.T) {
	for _, events := range []bool{false, true} {
		b := &connectorsBackend{}
		var authorized []string
		identity := &Identity{Authorize: func(_ context.Context, _ any, _ Grant, methods ...string) error {
			authorized = methods
			return nil
		}}
		out, count, err := (&Service{Foundry: b, Events: events}).ListFoundryConnectors(context.Background(), identity, &Call{}, FoundryWorkspaceInput{WorkspaceID: "workspace-1"})
		require.NoError(t, err)
		assert.Equal(t, 2, count)
		if events {
			assert.Contains(t, authorized, "ListFoundryEventBatches")
			assert.Equal(t, 1, b.batches)
			assert.NotNil(t, out.Connectors[1].EventSource)
			continue
		}
		assert.NotContains(t, authorized, "ListFoundryEventBatches")
		assert.Zero(t, b.batches)
		assert.Nil(t, out.Connectors[1].EventSource)
	}
}
