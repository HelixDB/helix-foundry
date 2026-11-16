package integration

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

const (
	autoWorkspace = "01JZZZZZZZZZZZZZZZZZZZZZZW"
	autoID        = "01JZZZZZZZZZZZZZZZZZZZZZZA"
	autoRunID     = "01JZZZZZZZZZZZZZZZZZZZZZZR"
	autoSpec      = `{"v":1,"nodes":[` +
		`{"id":"start","kind":"trigger","config":{"kind":"schedule","every":"week","weekday":"monday","at":"09:00","time_zone":"Europe/London"},"position":{"x":0,"y":0}},` +
		`{"id":"sync","kind":"action","config":{"operation":"sync_connector","connector_id":"con-1"},"position":{"x":200,"y":0}},` +
		`{"id":"mail","kind":"notify","config":{"message":"Synced"},"position":{"x":400,"y":0}}],` +
		`"edges":[{"from":"start","to":"sync"},{"from":"sync","to":"mail"}]}`
)

// autoBackend answers the workflow (automation) RPCs as Foundry does, recording what it was asked.
type autoBackend struct {
	FoundryBackendAPI
	FoundryActionAPI
	creates []*foundryv1.CreateFoundryAutomationRequest
	updates []*foundryv1.UpdateFoundryAutomationRequest
	pauses  int
	resumes int
	deletes int
	runs    []*foundryv1.RunFoundryAutomationRequest
	refuse  error
}

func automationProto() *foundryv1.FoundryAutomation {
	return &foundryv1.FoundryAutomation{Id: autoID, Name: "Weekly sync", Revision: 2, Status: "needs_approval", TriggerKind: "schedule", SpecJson: autoSpec,
		OwnerUserId: "user-ada", CanEdit: true, CanRun: false, CanDelete: true,
		LastRun: &foundryv1.FoundryAutomationRun{Id: autoRunID, AutomationId: autoID, Status: "failed", Error: "Step sync failed.", CreatedAt: timestamppb.Now()},
		Ontologies: []*foundryv1.FoundryAutomationOntology{
			{OntologyId: "ont-1", OntologyName: "Sales", WorkflowId: "wf-1", Runs: 3, FinishedRuns: 2, Gaps: []string{"no renewal dates"},
				Verdicts: []*foundryv1.FoundryWorkflowVerdictCount{{Verdict: "helpful", Runs: 2}}},
			{OntologyId: "ont-2", OntologyName: "Support", Untracked: "workflows_full"},
		}}
}

func runProto() *foundryv1.FoundryAutomationRun {
	return &foundryv1.FoundryAutomationRun{Id: autoRunID, AutomationId: autoID, Status: "succeeded", TriggerEventJson: `{"kind":"manual"}`, UsdMicros: 1200,
		Nodes: []*foundryv1.FoundryAutomationRunNode{{NodeId: "start", Status: "succeeded", OutputJson: `{"summary":"Run by hand."}`},
			{NodeId: "sync", Status: "failed", Error: "the connector is gone"}},
		Ontologies: []*foundryv1.FoundryAutomationRunOntology{{OntologyId: "ont-1", OntologyName: "Sales", WorkflowId: "wf-1", WorkflowRunId: "wfr-1",
			Status: "finished", AssessmentStatus: "done", Verdict: "helpful"}}}
}

func (b *autoBackend) ListFoundryAutomations(context.Context, *foundryv1.ListFoundryAutomationsRequest, ...grpc.CallOption) (*foundryv1.ListFoundryAutomationsResponse, error) {
	listed := automationProto()
	listed.SpecJson = ""
	return &foundryv1.ListFoundryAutomationsResponse{Automations: []*foundryv1.FoundryAutomation{listed}, CanCreate: true, Email: true}, nil
}

func (b *autoBackend) GetFoundryAutomation(context.Context, *foundryv1.GetFoundryAutomationRequest, ...grpc.CallOption) (*foundryv1.FoundryAutomationResponse, error) {
	return &foundryv1.FoundryAutomationResponse{Automation: automationProto()}, b.refuse
}

func (b *autoBackend) ListFoundryAutomationRuns(context.Context, *foundryv1.ListFoundryAutomationRunsRequest, ...grpc.CallOption) (
	*foundryv1.ListFoundryAutomationRunsResponse, error) {
	listed := runProto()
	listed.Nodes = nil
	return &foundryv1.ListFoundryAutomationRunsResponse{Runs: []*foundryv1.FoundryAutomationRun{listed}, NextPageToken: "next"}, nil
}

func (b *autoBackend) GetFoundryAutomationRun(context.Context, *foundryv1.GetFoundryAutomationRunRequest, ...grpc.CallOption) (
	*foundryv1.FoundryAutomationRunResponse, error) {
	return &foundryv1.FoundryAutomationRunResponse{Run: runProto()}, nil
}

func (b *autoBackend) CreateFoundryAutomation(_ context.Context, r *foundryv1.CreateFoundryAutomationRequest, _ ...grpc.CallOption) (
	*foundryv1.FoundryAutomationResponse, error) {
	b.creates = append(b.creates, r)
	return &foundryv1.FoundryAutomationResponse{Automation: automationProto()}, b.refuse
}

func (b *autoBackend) UpdateFoundryAutomation(_ context.Context, r *foundryv1.UpdateFoundryAutomationRequest, _ ...grpc.CallOption) (
	*foundryv1.FoundryAutomationResponse, error) {
	b.updates = append(b.updates, r)
	return &foundryv1.FoundryAutomationResponse{Automation: automationProto()}, nil
}

func (b *autoBackend) PauseFoundryAutomation(context.Context, *foundryv1.PauseFoundryAutomationRequest, ...grpc.CallOption) (*foundryv1.FoundryAutomationResponse, error) {
	b.pauses++
	return &foundryv1.FoundryAutomationResponse{Automation: automationProto()}, nil
}

func (b *autoBackend) ResumeFoundryAutomation(context.Context, *foundryv1.ResumeFoundryAutomationRequest, ...grpc.CallOption) (*foundryv1.FoundryAutomationResponse, error) {
	b.resumes++
	return &foundryv1.FoundryAutomationResponse{Automation: automationProto()}, nil
}

func (b *autoBackend) DeleteFoundryAutomation(context.Context, *foundryv1.DeleteFoundryAutomationRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	b.deletes++
	return &emptypb.Empty{}, nil
}

func (b *autoBackend) RunFoundryAutomation(_ context.Context, r *foundryv1.RunFoundryAutomationRequest, _ ...grpc.CallOption) (*foundryv1.FoundryAutomationRunResponse, error) {
	b.runs = append(b.runs, r)
	return &foundryv1.FoundryAutomationRunResponse{Run: runProto()}, nil
}

func autoService() (*Service, *autoBackend) {
	b := &autoBackend{}
	return &Service{Foundry: b, Actions: b, Confirmations: &memConfirmations{}, FoundryURL: "https://foundry.example"}, b
}

func TestAutomationReadTools(t *testing.T) {
	s, _ := autoService()
	list := called(t, s, toolFoundryListWorkflows, map[string]any{"workspace_id": autoWorkspace})
	assert.Equal(t, true, list["email"])
	listed := list["workflows"].([]any)[0].(map[string]any)
	assert.Equal(t, "Weekly sync", listed["name"])
	assert.Equal(t, "needs_approval", listed["status"])
	assert.NotContains(t, listed, "spec", "a list leaves specs out")
	assert.Equal(t, "https://foundry.example/workflows/"+autoID+"?workspace="+autoWorkspace, listed["url"], "where its owner approves it")
	assert.Equal(t, "failed", listed["last_run"].(map[string]any)["status"])

	got := called(t, s, toolFoundryGetWorkflow, map[string]any{"workspace_id": autoWorkspace, "workflow_id": autoID})
	workflow := got["workflow"].(map[string]any)
	assert.EqualValues(t, 2, workflow["revision"])
	assert.Len(t, workflow["spec"].(map[string]any)["nodes"], 3)

	runs := called(t, s, toolFoundryListWorkflowRuns, map[string]any{"workspace_id": autoWorkspace, "workflow_id": autoID})
	assert.Equal(t, "next", runs["next_page_token"])
	assert.NotContains(t, runs["runs"].([]any)[0], "steps")

	run := called(t, s, toolFoundryGetWorkflowRun, map[string]any{"workspace_id": autoWorkspace, "workflow_id": autoID, "run_id": autoRunID})["run"].(map[string]any)
	assert.Equal(t, autoID, run["workflow_id"])
	steps := run["steps"].([]any)
	require.Len(t, steps, 2)
	assert.Equal(t, "the connector is gone", steps[1].(map[string]any)["error"])
	assert.Equal(t, "https://foundry.example/workflows/"+autoID+"/runs/"+autoRunID+"?workspace="+autoWorkspace, run["url"])

	// The ontologies its runs use, and how much each helped.
	useCases := workflow["use_cases"].([]any)
	require.Len(t, useCases, 2)
	assert.Equal(t, map[string]any{"ontology_id": "ont-1", "ontology_name": "Sales", "use_case_id": "wf-1", "runs": float64(3), "finished_runs": float64(2),
		"verdicts": map[string]any{"helpful": float64(2)}, "gaps": []any{"no renewal dates"}}, useCases[0])
	assert.Equal(t, "workflows_full", useCases[1].(map[string]any)["untracked"])
	assert.NotEmpty(t, listed["use_cases"], "a list says them too")
	used := run["use_cases"].([]any)
	require.Len(t, used, 1)
	assert.Equal(t, map[string]any{"ontology_id": "ont-1", "ontology_name": "Sales", "use_case_id": "wf-1", "use_case_run_id": "wfr-1",
		"status": "finished", "assessment_status": "done", "verdict": "helpful"}, used[0])
}

func TestAutomationToolsAreOffered(t *testing.T) {
	s, _ := autoService()
	listed, err := connect(t, s, reader()).ListTools(context.Background(), nil)
	require.NoError(t, err)
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	for _, name := range []string{toolFoundryListWorkflows, toolFoundryGetWorkflow, toolFoundryListWorkflowRuns, toolFoundryGetWorkflowRun} {
		assert.Contains(t, names, name)
	}
	listed, err = connect(t, s, grantKey(PermissionDatabaseQueryRead)).ListTools(context.Background(), nil)
	require.NoError(t, err)
	for _, tool := range listed.Tools {
		assert.NotEqual(t, toolFoundryListWorkflows, tool.Name, "workflows are people's")
	}
}

func TestAutomationOperations(t *testing.T) {
	prepare := func(s *Service, operation, target string, payload map[string]any) (FoundryOperation, error) {
		return s.ValidateFoundryOperation(context.Background(), reader(), FoundryOperationInput{Operation: operation, Target: target, Payload: payload})
	}
	var spec map[string]any
	require.NoError(t, json.Unmarshal([]byte(autoSpec), &spec))
	target := "workflow:" + autoWorkspace + "/" + autoID

	t.Run("each runs its call with the payload prepared", func(t *testing.T) {
		s, b := autoService()
		op, err := prepare(s, "create_workflow", "workspace:"+autoWorkspace, map[string]any{"name": " Weekly sync ", "spec": spec})
		require.NoError(t, err)
		assert.Contains(t, string(op.CanonicalPayload()), `"name":"Weekly sync"`)
		result, err := op.Execute(context.Background(), s, reader(), "conf-1")
		require.NoError(t, err)
		require.Len(t, b.creates, 1)
		assert.Equal(t, "conf-1", b.creates[0].GetClientToken(), "the confirmation is the create's client token")
		assert.Contains(t, b.creates[0].GetSpecJson(), `"kind":"schedule"`)
		created := result.(FoundryAutomationOutput)
		assert.Equal(t, autoID, created.ID)
		assert.Equal(t, "needs_approval", created.Status, "nothing an agent makes runs before its owner approves it")

		op, err = prepare(s, "update_workflow", target, map[string]any{"expected_revision": json.Number("2"), "name": "Renamed", "spec": spec})
		require.NoError(t, err)
		_, err = op.Execute(context.Background(), s, reader(), "conf-2")
		require.NoError(t, err)
		assert.EqualValues(t, 2, b.updates[0].GetExpectedRevision())
		assert.Equal(t, "Renamed", b.updates[0].GetName())
		assert.NotNil(t, b.updates[0].SpecJson)
		assert.Nil(t, b.updates[0].Description, "what an update leaves out stays")

		for _, operation := range []string{"pause_workflow", "resume_workflow", "delete_workflow", "run_workflow"} {
			op, err := prepare(s, operation, target, map[string]any{})
			require.NoError(t, err, operation)
			assert.Equal(t, "{}", string(op.CanonicalPayload()))
			result, err := op.Execute(context.Background(), s, reader(), "conf-"+operation)
			require.NoError(t, err, operation)
			if operation == "delete_workflow" {
				assert.Equal(t, map[string]any{"deleted": true, "workflow_id": autoID}, result)
			}
		}
		assert.Equal(t, 1, b.pauses)
		assert.Equal(t, 1, b.resumes)
		assert.Equal(t, 1, b.deletes)
		require.Len(t, b.runs, 1)
		assert.Equal(t, "conf-run_workflow", b.runs[0].GetClientToken(), "a replayed run finds the run it queued")
	})

	t.Run("refuses what the backend would", func(t *testing.T) {
		s, _ := autoService()
		for _, c := range []struct {
			name, operation, target string
			payload                 map[string]any
		}{
			{"no name", "create_workflow", "workspace:" + autoWorkspace, map[string]any{"name": " ", "spec": spec}},
			{"no spec", "create_workflow", "workspace:" + autoWorkspace, map[string]any{"name": "x"}},
			{"a bad spec", "create_workflow", "workspace:" + autoWorkspace, map[string]any{"name": "x", "spec": map[string]any{"v": 1}}},
			{"an unknown field", "create_workflow", "workspace:" + autoWorkspace, map[string]any{"name": "x", "spec": spec, "approve": true}},
			{"another target kind", "create_workflow", "ontology:" + autoWorkspace + "/o", map[string]any{"name": "x", "spec": spec}},
			{"no revision", "update_workflow", target, map[string]any{"name": "x"}},
			{"no change", "update_workflow", target, map[string]any{"expected_revision": json.Number("2")}},
			{"a bad spec edit", "update_workflow", target, map[string]any{"expected_revision": json.Number("2"), "spec": map[string]any{}}},
			{"a payload to pause", "pause_workflow", target, map[string]any{"why": "x"}},
			{"a payload to run", "run_workflow", target, map[string]any{"approve": true}},
			{"a bad target", "run_workflow", "workflow:" + autoWorkspace, map[string]any{}},
			{"the old target kind", "run_workflow", "automation:" + autoWorkspace + "/" + autoID, map[string]any{}},
			{"the old operation name", "run_automation", target, map[string]any{}},
			{"approving", "approve_workflow", target, map[string]any{}},
		} {
			_, err := prepare(s, c.operation, c.target, c.payload)
			assert.Error(t, err, c.name)
		}
	})

	t.Run("a member prepares their own", func(t *testing.T) {
		s, _ := autoService()
		member := reader()
		member.HasPermission = func(p Permission) bool { return p == PermissionWorkspaceRead }
		_, err := s.ValidateFoundryOperation(context.Background(), member, FoundryOperationInput{Operation: "create_workflow",
			Target: "workspace:" + autoWorkspace, Payload: map[string]any{"name": "Mine", "spec": spec}})
		require.NoError(t, err)
	})

	t.Run("a backend refusal keeps its words", func(t *testing.T) {
		s, b := autoService()
		b.refuse = status.Error(codes.FailedPrecondition, "the workspace has 50 workflows, as many as it may")
		op, err := prepare(s, "create_workflow", "workspace:"+autoWorkspace, map[string]any{"name": "x", "spec": spec})
		require.NoError(t, err)
		_, err = op.Execute(context.Background(), s, reader(), "conf-1")
		require.Error(t, err)
		assert.Equal(t, "automation", TargetKind(target), "the audit keeps the kind automation")
		assert.Empty(t, TargetKind("automation:"+autoWorkspace+"/"+autoID), "the old target kind is no kind")
	})
}
