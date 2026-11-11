package integration

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// workflowBackend answers the workflow RPCs and keeps what it was asked.
type workflowBackend struct {
	FoundryWorkflowBackendAPI
	created  *foundryv1.CreateFoundryWorkflowRequest
	started  *foundryv1.StartFoundryWorkflowRunRequest
	finished *foundryv1.FinishFoundryWorkflowRunRequest
	listed   *foundryv1.ListFoundryWorkflowRunAccessesRequest
	more     bool
}

func (b *workflowBackend) CreateFoundryWorkflow(_ context.Context, r *foundryv1.CreateFoundryWorkflowRequest, _ ...grpc.CallOption) (*foundryv1.FoundryWorkflowResponse, error) {
	b.created = r
	return &foundryv1.FoundryWorkflowResponse{Workflow: &foundryv1.FoundryWorkflow{Id: "wf-1", Name: r.GetName(), CreatedAt: timestamppb.Now()}}, nil
}

func (b *workflowBackend) ListFoundryWorkflows(context.Context, *foundryv1.ListFoundryWorkflowsRequest, ...grpc.CallOption) (*foundryv1.ListFoundryWorkflowsResponse, error) {
	return &foundryv1.ListFoundryWorkflowsResponse{Workflows: []*foundryv1.FoundryWorkflow{{Id: "wf-1", Name: "Triage", Runs: 3}}, NextPageToken: "p2"}, nil
}

func (b *workflowBackend) StartFoundryWorkflowRun(_ context.Context, r *foundryv1.StartFoundryWorkflowRunRequest, _ ...grpc.CallOption) (*foundryv1.FoundryWorkflowRunResponse, error) {
	b.started = r
	return &foundryv1.FoundryWorkflowRunResponse{Run: &foundryv1.FoundryWorkflowRun{Id: "run-1", WorkflowId: r.GetWorkflowId(), Status: "open", Input: r.GetInput()}}, nil
}

func (b *workflowBackend) FinishFoundryWorkflowRun(_ context.Context, r *foundryv1.FinishFoundryWorkflowRunRequest, _ ...grpc.CallOption) (*foundryv1.FoundryWorkflowRunResponse, error) {
	b.finished = r
	return &foundryv1.FoundryWorkflowRunResponse{Run: &foundryv1.FoundryWorkflowRun{
		Id: r.GetId(), Status: "finished", Input: r.GetInput(), Output: r.GetOutput(), FinishedAt: timestamppb.Now(),
		Counts: &foundryv1.FoundryWorkflowRunCounts{Accesses: 2, Reads: 1, Questions: 1},
	}}, nil
}

func (b *workflowBackend) GetFoundryWorkflowRun(_ context.Context, r *foundryv1.GetFoundryWorkflowRunRequest, _ ...grpc.CallOption) (*foundryv1.FoundryWorkflowRunResponse, error) {
	return &foundryv1.FoundryWorkflowRunResponse{Run: &foundryv1.FoundryWorkflowRun{Id: r.GetId(), Status: "open"}}, nil
}

func (b *workflowBackend) ListFoundryWorkflowRunAccesses(_ context.Context, r *foundryv1.ListFoundryWorkflowRunAccessesRequest, _ ...grpc.CallOption) (*foundryv1.ListFoundryWorkflowRunAccessesResponse, error) {
	b.listed = r
	return &foundryv1.ListFoundryWorkflowRunAccessesResponse{More: b.more, Accesses: []*foundryv1.FoundryWorkflowAccess{
		{Seq: 1, Access: &foundryv1.FoundryWorkflowAccess_Read{Read: &foundryv1.FoundryWorkflowRead{LoadId: "load-1", ReadsJson: `[{"entity":{}}]`, ResultJson: `{"n":9007199254740993}`}}},
		{Seq: 2, Access: &foundryv1.FoundryWorkflowAccess_Read{Read: &foundryv1.FoundryWorkflowRead{LoadId: "load-1", ReadsJson: `[]`, ResultOmitted: true}}},
		{Seq: 3, Access: &foundryv1.FoundryWorkflowAccess_Question{Question: &foundryv1.FoundryWorkflowQuestion{LoadId: "load-1", QuestionId: "q-1"}}},
		{Seq: 4, Access: &foundryv1.FoundryWorkflowAccess_Send{Send: &foundryv1.FoundryWorkflowSend{ConnectorId: "c-1", Seq: 12, PieceIds: []string{"p-1"}}}},
		{Seq: 5, Access: &foundryv1.FoundryWorkflowAccess_Upload{Upload: &foundryv1.FoundryWorkflowUpload{DocumentId: "d-1", SetId: "s-1"}}},
	}}, nil
}

var workflowTools = []string{toolFoundryCreateWorkflow, toolFoundryListWorkflows, toolFoundryStartWorkflowRun, toolFoundryFinishWorkflowRun, toolFoundryGetWorkflowRun}

// A user who manages ontologies gets every workflow tool; an agent's key reading databases runs
// workflows but creates none; restricted callers and hosts without the backend's workflows get none.
func TestWorkflowToolsByIdentity(t *testing.T) {
	with := &Service{Workflows: &workflowBackend{}}
	assert.Subset(t, toolNames(t, with, everyTool()), workflowTools)
	for _, name := range workflowTools {
		assert.NotContains(t, toolNames(t, &Service{}, everyTool()), name)
	}

	// Only a host with API sources names the send tool.
	describe := func(s *Service) string {
		listed, err := connect(t, s, everyTool()).ListTools(context.Background(), nil)
		require.NoError(t, err)
		for _, tool := range listed.Tools {
			if tool.Name == toolFoundryStartWorkflowRun {
				return tool.Description
			}
		}
		return ""
	}
	assert.NotContains(t, describe(with), toolFoundrySendPieces)
	assert.Contains(t, describe(&Service{Workflows: &workflowBackend{}, Events: true}), toolFoundrySendPieces)

	key := &Identity{ServiceKey: true, HasPermission: func(p Permission) bool { return p == PermissionDatabaseQueryRead }}
	names := toolNames(t, with, key)
	assert.Subset(t, names, workflowTools[1:])
	assert.NotContains(t, names, toolFoundryCreateWorkflow)

	granted := grantKey()
	granted.FoundryGrants = true
	names = toolNames(t, with, granted)
	assert.Contains(t, names, toolFoundryReadGraph)
	for _, name := range workflowTools {
		assert.NotContains(t, names, name)
	}

	viewer := &Identity{User: true, HasPermission: func(p Permission) bool { return p == PermissionWorkspaceRead }}
	for _, name := range workflowTools {
		assert.NotContains(t, toolNames(t, with, viewer), name)
	}
}

func callTool(t *testing.T, session *mcp.ClientSession, name, args string) map[string]any {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: json.RawMessage(args)})
	require.NoError(t, err)
	require.False(t, result.IsError, "%s: %v", name, result.Content)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(result.Content[len(result.Content)-1].(*mcp.TextContent).Text), &out))
	return out
}

const workflowIDs = `"workspace_id":"workspace-1","ontology_id":"ontology-1"`

func TestWorkflowRunTools(t *testing.T) {
	b := &workflowBackend{more: true}
	identity := everyTool()
	var methods []string
	identity.Authorize = func(_ context.Context, _ any, _ Grant, m ...string) error {
		methods = append(methods, m...)
		return nil
	}
	session := connect(t, &Service{Workflows: b}, identity)

	created := callTool(t, session, toolFoundryCreateWorkflow, `{`+workflowIDs+`,"name":"Triage","description":"Tickets","client_token":"c-1"}`)
	assert.Equal(t, "wf-1", created["id"])
	assert.Equal(t, contentTrustUntrusted, created["content_trust"])
	assert.Equal(t, "Tickets", b.created.GetDescription())
	assert.Equal(t, "c-1", b.created.GetClientToken())

	listed := callTool(t, session, toolFoundryListWorkflows, `{`+workflowIDs+`}`)
	assert.Equal(t, "p2", listed["next_page_token"])
	assert.Len(t, listed["workflows"], 1)

	started := callTool(t, session, toolFoundryStartWorkflowRun, `{`+workflowIDs+`,"workflow_id":"wf-1","input":"Who renews?","client_token":"r-1"}`)
	assert.Equal(t, "run-1", started["id"])
	assert.Contains(t, started["next"], "workflow_run_id run-1")
	assert.Equal(t, "Who renews?", b.started.GetInput())

	finished := callTool(t, session, toolFoundryFinishWorkflowRun, `{`+workflowIDs+`,"workflow_id":"wf-1","run_id":"run-1","output":"Acme."}`)
	assert.Equal(t, "finished", finished["status"])
	assert.Equal(t, "Acme.", b.finished.GetOutput())
	assert.Empty(t, b.finished.GetInput())

	got := callTool(t, session, toolFoundryGetWorkflowRun, `{`+workflowIDs+`,"workflow_id":"wf-1","run_id":"run-1","after_seq":0}`)
	assert.EqualValues(t, 5, got["next_after_seq"])
	accesses := got["accesses"].([]any)
	require.Len(t, accesses, 5)
	kinds := make([]string, len(accesses))
	for i, a := range accesses {
		kinds[i] = a.(map[string]any)["kind"].(string)
	}
	assert.Equal(t, []string{"read", "read", "question", "send", "upload"}, kinds)
	read := accesses[0].(map[string]any)
	assert.Equal(t, "9007199254740993", read["result"].(map[string]any)["n"], "a result keeps large integers exact")
	omitted := accesses[1].(map[string]any)
	assert.Nil(t, omitted["result"])
	assert.Equal(t, true, omitted["result_omitted"])
	assert.Equal(t, "12", accesses[3].(map[string]any)["send_seq"])

	for _, m := range []string{"CreateFoundryWorkflow", "ListFoundryWorkflows", "StartFoundryWorkflowRun", "FinishFoundryWorkflowRun",
		"GetFoundryWorkflowRun", "ListFoundryWorkflowRunAccesses"} {
		assert.True(t, slices.Contains(methods, m), "%s was not authorized", m)
	}

	// A run id that isn't one is refused before the backend is called.
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: toolFoundryFinishWorkflowRun,
		Arguments: json.RawMessage(`{` + workflowIDs + `,"workflow_id":"wf-1","run_id":"../x","output":"a"}`)})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}

// Each tracked tool hands its workflow_run_id to the backend, and refuses one that isn't an id.
func TestTrackedToolsPassTheRun(t *testing.T) {
	b := &graphBackend{}
	var req *foundryv1.ReadFoundryGraphRequest
	identity := everyTool()
	identity.Authorize = func(_ context.Context, r any, _ Grant, _ ...string) error {
		if read, ok := r.(*foundryv1.ReadFoundryGraphRequest); ok {
			req = read
		}
		return nil
	}
	session := connect(t, &Service{Foundry: b}, identity)
	callTool(t, session, toolFoundryReadGraph,
		`{`+workflowIDs+`,"load_id":"load-1","reads":[{"entity":{"type":"C","id":"1"}}],"workflow_run_id":"run-1"}`)
	require.NotNil(t, req)
	assert.Equal(t, "run-1", req.GetWorkflowRunId())

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: toolFoundryReadGraph, Arguments: json.RawMessage(
		`{` + workflowIDs + `,"load_id":"load-1","reads":[{"entity":{"type":"C","id":"1"}}],"workflow_run_id":"not an id"}`)})
	require.NoError(t, err)
	assert.True(t, result.IsError)
}
