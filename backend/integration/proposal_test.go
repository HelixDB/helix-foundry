package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// memProposals keeps what a bot proposes, allowing what allow names.
type memProposals struct {
	allow    map[string]bool
	proposed []ProposedAction
	err      error
}

func (s *memProposals) Allows(operation string) bool { return s.allow[operation] }

func (s *memProposals) Propose(_ context.Context, a ProposedAction) (AwaitingAdmin, error) {
	if s.err != nil {
		return AwaitingAdmin{}, s.err
	}
	s.proposed = append(s.proposed, a)
	return AwaitingAdmin{ActionID: "act-1"}, nil
}

// botReads is what a bot's key reads: the ontology ont1, the dashboard d1
// and no workflow, unless a test says.
type botReads struct {
	FoundryBackendAPI
	workflows bool
}

func (botReads) GetFoundryOntology(_ context.Context, r *foundryv1.GetFoundryOntologyRequest, _ ...grpc.CallOption) (*foundryv1.FoundryOntologyResponse, error) {
	if r.GetOntologyId() != "ont1" {
		return nil, status.Error(codes.NotFound, "ontology not found")
	}
	return &foundryv1.FoundryOntologyResponse{Ontology: &foundryv1.FoundryOntology{Id: "ont1"}}, nil
}

func (botReads) GetCurrentOntologyVersion(context.Context, *foundryv1.GetCurrentOntologyVersionRequest, ...grpc.CallOption) (*foundryv1.OntologyVersionResponse, error) {
	return &foundryv1.OntologyVersionResponse{}, nil
}

func (botReads) ListOntologyVersions(context.Context, *foundryv1.ListOntologyVersionsRequest, ...grpc.CallOption) (*foundryv1.ListOntologyVersionsResponse, error) {
	return &foundryv1.ListOntologyVersionsResponse{}, nil
}

func (botReads) ListFoundryLoads(context.Context, *foundryv1.ListFoundryLoadsRequest, ...grpc.CallOption) (*foundryv1.ListFoundryLoadsResponse, error) {
	return &foundryv1.ListFoundryLoadsResponse{}, nil
}

func (botReads) ListOntologyGenerations(context.Context, *foundryv1.ListOntologyGenerationsRequest, ...grpc.CallOption) (*foundryv1.ListOntologyGenerationsResponse, error) {
	return &foundryv1.ListOntologyGenerationsResponse{}, nil
}

func (botReads) ListFoundryDocumentSets(context.Context, *foundryv1.ListFoundryDocumentSetsRequest, ...grpc.CallOption) (*foundryv1.ListFoundryDocumentSetsResponse, error) {
	return &foundryv1.ListFoundryDocumentSetsResponse{}, nil
}

func (botReads) GetFoundryDashboard(_ context.Context, r *foundryv1.GetFoundryDashboardRequest, _ ...grpc.CallOption) (*foundryv1.FoundryDashboardResponse, error) {
	if r.GetId() != "d1" {
		return nil, status.Error(codes.NotFound, "dashboard not found")
	}
	return &foundryv1.FoundryDashboardResponse{Dashboard: &foundryv1.FoundryDashboard{Id: "d1"}}, nil
}

func (b botReads) GetFoundryAutomation(_ context.Context, r *foundryv1.GetFoundryAutomationRequest, _ ...grpc.CallOption) (*foundryv1.FoundryAutomationResponse, error) {
	if !b.workflows {
		return nil, status.Error(codes.PermissionDenied, "workflows are people's: sign in to Foundry")
	}
	return &foundryv1.FoundryAutomationResponse{Automation: &foundryv1.FoundryAutomation{Id: r.GetId()}}, nil
}

// botIdentity is a Slack install's bot's key: no grant of its own, so
// nothing but ConfirmByAdmin gives it prepare.
func botIdentity(sink ProposalSink) *Identity {
	identity := reader()
	identity.User, identity.ServiceKey = false, true
	identity.HasPermission = func(Permission) bool { return false }
	identity.Confirm = ConfirmByAdmin{Sink: sink}
	return identity
}

func TestABotProposesForAnOwnerOrAdmin(t *testing.T) {
	confirmations := &memConfirmations{}
	sink := &memProposals{allow: map[string]bool{"create_dashboard": true, "create_load": true}}
	s := (&Service{Foundry: botReads{}, Confirmations: confirmations, Actions: &loadActions{}}).ForAgent()
	session := connect(t, s, botIdentity(sink))
	tools, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	var prepare *mcp.Tool
	for _, tool := range tools.Tools {
		assert.NotEqual(t, toolExecuteFoundry, tool.Name)
		assert.NotEqual(t, toolFoundryCompleteUpload, tool.Name)
		if tool.Name == toolPrepareFoundry {
			prepare = tool
		}
	}
	require.NotNil(t, prepare, "a key with no grant proposes all the same")
	schema, err := json.Marshal(prepare.InputSchema)
	require.NoError(t, err)
	assert.Contains(t, string(schema), `"create_dashboard"`)
	assert.NotContains(t, string(schema), `"sync_connector"`, "only what the sink allows")

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: toolPrepareFoundry, Arguments: map[string]any{
		"operation": "create_dashboard", "target": "workspace:ws1", "payload": map[string]any{"spec": map[string]any{}, "name": "Invoices"},
	}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err)
	assert.JSONEq(t, `{"action_id":"act-1","status":"awaiting_admin","expires_at":"0001-01-01T00:00:00Z","next":"`+awaitingAdminNext+`"}`, string(raw))
	require.Len(t, sink.proposed, 1)
	proposed := sink.proposed[0]
	assert.Equal(t, `{"name":"Invoices","spec":{}}`, string(proposed.Payload), "the model's payload, keys sorted")
	assert.Equal(t, confirmationPayloadHash("foundry:create_dashboard", "workspace:ws1", proposed.Payload), proposed.PayloadHash)
	assert.Zero(t, confirmations.prepared, "nothing is confirmed as the bot")

	refused, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: toolPrepareFoundry, Arguments: map[string]any{
		"operation": "create_load", "target": "connector:ws1/c1", "payload": map[string]any{},
	}})
	require.NoError(t, err)
	assert.True(t, refused.IsError, "a target the bot can't read")
	assert.Len(t, sink.proposed, 1)

	// Execute refuses a bot outright, whatever it holds.
	_, _, err = s.ExecuteFoundryOperation(context.Background(), botIdentity(sink), &Call{}, ExecuteFoundryInput{
		Operation: loadInput.Operation, Target: loadInput.Target, Payload: loadInput.Payload, ConfirmationID: "c", ConfirmationToken: "t",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only an owner or admin runs a proposal")
}

// A bot proposes only on what it reads: the workspace, or an ontology, a
// dashboard or a workflow its key's reads find.
func TestABotProposesOnlyOnWhatItReads(t *testing.T) {
	payload := map[string]any{"version_id": "ver1"}
	for _, c := range []struct {
		name, operation, target string
		payload                 map[string]any
		workflows               bool
		refused                 string
	}{
		{name: "the workspace", operation: "create_dashboard", target: "workspace:ws1", payload: map[string]any{}},
		{name: "an ontology it reads", operation: "create_load", target: "ontology:ws1/ont1", payload: payload},
		{name: "a dashboard it reads", operation: "create_load", target: "dashboard:ws1/d1", payload: payload},
		{name: "a workflow it reads", operation: "create_load", target: "workflow:ws1/w1", payload: payload, workflows: true},
		{name: "an ontology it doesn't read", operation: "create_load", target: "ontology:ws1/ont9", payload: payload, refused: "can't see ontology:ws1/ont9"},
		{name: "a dashboard it doesn't read", operation: "create_load", target: "dashboard:ws1/d9", payload: payload, refused: "can't see"},
		{name: "a workflow, which a key reads none of", operation: "create_load", target: "workflow:ws1/w1", payload: payload, refused: "can't see"},
		{name: "a connector", operation: "create_load", target: "connector:ws1/c1", payload: payload, refused: "can't see"},
		{name: "a database", operation: "create_load", target: "tenant:t1", payload: payload, refused: "can't see"},
		{name: "a malformed workspace", operation: "create_load", target: "workspace:", payload: payload, refused: "can't see"},
		{name: "an ontology with no id", operation: "create_load", target: "ontology:ws1", payload: payload, refused: "can't see"},
		{name: "an operation the sink refuses", operation: "sync_connector", target: "workspace:ws1", payload: payload, refused: "can't propose sync_connector"},
		{name: "no payload", operation: "create_load", target: "workspace:ws1", refused: "a payload object are required"},
		{name: "a payload too large", operation: "create_load", target: "workspace:ws1", payload: map[string]any{"name": strings.Repeat("a", maxProposalBytes)},
			refused: "over 32768 bytes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sink := &memProposals{allow: map[string]bool{"create_dashboard": true, "create_load": true}}
			s := &Service{Foundry: botReads{workflows: c.workflows}}
			call := &Call{}
			_, err := s.proposeForAdmin(context.Background(), botIdentity(sink), call, FoundryOperationInput{Operation: c.operation, Target: c.target,
				Payload: c.payload}, sink)
			assert.Equal(t, c.target, call.ResourceID, "audited by its target")
			if c.refused == "" {
				require.NoError(t, err)
				assert.Len(t, sink.proposed, 1)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.refused)
			assert.Empty(t, sink.proposed)
		})
	}
	sink := &memProposals{allow: map[string]bool{"create_dashboard": true}, err: errors.New("the store went away")}
	_, err := (&Service{}).proposeForAdmin(context.Background(), botIdentity(sink), &Call{}, FoundryOperationInput{Operation: "create_dashboard",
		Target: "workspace:ws1", Payload: map[string]any{}}, sink)
	assert.EqualError(t, err, "the store went away", "the sink's refusal is the tool's")
}

// An owner's or admin's run of a proposal runs as them, once, under its own
// name in the audit.
func TestRunSlackApprovedPreparesAndRunsOnceAsTheApprover(t *testing.T) {
	actions := &loadActions{}
	var tools []string
	s := (&Service{Confirmations: &memConfirmations{}, Actions: actions, RunTool: func(ctx context.Context, name string, invoke Invocation) (
		*mcp.CallToolResult, any, error) {
		tools = append(tools, name)
		out, _, err := invoke(ctx, &Call{})
		return nil, out, err
	}}).ForAgent()
	ok, isOK := s.RunSlackApproved(context.Background(), reader(), loadInput).(ExecutedOK)
	require.True(t, isOK)
	assert.Equal(t, "create_load", ok.Result.Operation)
	assert.Equal(t, 1, actions.calls)
	assert.Equal(t, []string{toolRunSlackApproved}, tools)

	assert.IsType(t, ExecutedFailed{}, s.RunSlackApproved(context.Background(), botIdentity(&memProposals{}), loadInput),
		"the bot's own identity never runs it")
	assert.Equal(t, 1, actions.calls)
}
