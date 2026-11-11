package integration

import (
	"context"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// Workflows: an agent's task over one ontology. A run of one ties the agent's request, every
// read, question, send and upload it makes with the run's id (workflow_run_id), and its answer
// together, so the run shows what the ontology gave the agent.
const (
	toolFoundryCreateWorkflow    = "helix_foundry_create_workflow"
	toolFoundryListWorkflows     = "helix_foundry_list_workflows"
	toolFoundryStartWorkflowRun  = "helix_foundry_start_workflow_run"
	toolFoundryFinishWorkflowRun = "helix_foundry_finish_workflow_run"
	toolFoundryGetWorkflowRun    = "helix_foundry_get_workflow_run"
)

// FoundryWorkflowBackendAPI is the backend's workflow RPCs.
type FoundryWorkflowBackendAPI interface {
	CreateFoundryWorkflow(context.Context, *foundryv1.CreateFoundryWorkflowRequest, ...grpc.CallOption) (*foundryv1.FoundryWorkflowResponse, error)
	ListFoundryWorkflows(context.Context, *foundryv1.ListFoundryWorkflowsRequest, ...grpc.CallOption) (*foundryv1.ListFoundryWorkflowsResponse, error)
	StartFoundryWorkflowRun(context.Context, *foundryv1.StartFoundryWorkflowRunRequest, ...grpc.CallOption) (*foundryv1.FoundryWorkflowRunResponse, error)
	FinishFoundryWorkflowRun(context.Context, *foundryv1.FinishFoundryWorkflowRunRequest, ...grpc.CallOption) (*foundryv1.FoundryWorkflowRunResponse, error)
	GetFoundryWorkflowRun(context.Context, *foundryv1.GetFoundryWorkflowRunRequest, ...grpc.CallOption) (*foundryv1.FoundryWorkflowRunResponse, error)
	ListFoundryWorkflowRunAccesses(context.Context, *foundryv1.ListFoundryWorkflowRunAccessesRequest, ...grpc.CallOption) (*foundryv1.ListFoundryWorkflowRunAccessesResponse, error)
}

type CreateFoundryWorkflowInput struct {
	OntologyInput
	Name        string `json:"name" jsonschema:"1 to 100 characters naming what the agent does, such as Support triage."`
	Description string `json:"description,omitempty" jsonschema:"What the agent does, at most 2,000 characters."`
	ClientToken string `json:"client_token,omitempty" jsonschema:"At most 64 characters; the same token returns the workflow it made instead of making another."`
}

type ListFoundryWorkflowsInput struct {
	OntologyInput
	PageToken string `json:"page_token,omitempty" jsonschema:"Opaque continuation token from the previous call."`
}

type StartFoundryWorkflowRunInput struct {
	OntologyInput
	WorkflowID  string `json:"workflow_id" jsonschema:"The workflow, as helix_foundry_create_workflow or helix_foundry_list_workflows returned it."`
	Input       string `json:"input,omitempty" jsonschema:"The run's initial request, as the agent was given it, at most 64 KiB. Optional here: send it with helix_foundry_finish_workflow_run instead."`
	ClientToken string `json:"client_token,omitempty" jsonschema:"At most 64 characters; the same token returns the run it started instead of starting another."`
}

type FinishFoundryWorkflowRunInput struct {
	OntologyInput
	WorkflowID string `json:"workflow_id" jsonschema:"The run's workflow."`
	RunID      string `json:"run_id" jsonschema:"The run, as helix_foundry_start_workflow_run returned it."`
	Input      string `json:"input,omitempty" jsonschema:"The run's initial request, at most 64 KiB: required unless the run was started with it."`
	Output     string `json:"output" jsonschema:"The agent's final answer, at most 256 KiB."`
}

type GetFoundryWorkflowRunInput struct {
	OntologyInput
	WorkflowID string `json:"workflow_id" jsonschema:"The run's workflow."`
	RunID      string `json:"run_id" jsonschema:"The run."`
	AfterSeq   int32  `json:"after_seq,omitempty" jsonschema:"Return the calls it recorded after this seq; 0 from the first."`
}

type FoundryWorkflowOutput struct {
	ContentTrust string `json:"content_trust,omitempty"`
	ID           string `json:"id"`
	OntologyID   string `json:"ontology_id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Runs         int64  `json:"runs"`
	FinishedRuns int64  `json:"finished_runs"`
	CreatedAt    string `json:"created_at"`
	LastRunAt    string `json:"last_run_at,omitempty"`
}

type ListFoundryWorkflowsOutput struct {
	ContentTrust  string                  `json:"content_trust"`
	Workflows     []FoundryWorkflowOutput `json:"workflows"`
	NextPageToken string                  `json:"next_page_token,omitempty"`
}

type FoundryWorkflowRunOutput struct {
	ContentTrust string `json:"content_trust"`
	ID           string `json:"id"`
	WorkflowID   string `json:"workflow_id"`
	// Status is open (it records calls made with its id) or finished.
	Status     string                              `json:"status"`
	Input      string                              `json:"input,omitempty"`
	Output     string                              `json:"output,omitempty"`
	StartedAt  string                              `json:"started_at"`
	FinishedAt string                              `json:"finished_at,omitempty"`
	Counts     *foundryv1.FoundryWorkflowRunCounts `json:"counts"`
	Next       string                              `json:"next,omitempty"`
}

type FoundryWorkflowAccessOutput struct {
	Seq int32  `json:"seq"`
	At  string `json:"at"`
	// Kind is read, question, send or upload.
	Kind          string   `json:"kind"`
	LoadID        string   `json:"load_id,omitempty"`
	Reads         any      `json:"reads,omitempty" jsonschema:"A read's reads, as sent."`
	Result        any      `json:"result,omitempty" jsonschema:"Untrusted: a read's result as it was returned, absent when the run left it out."`
	ResultOmitted bool     `json:"result_omitted,omitempty"`
	QuestionID    string   `json:"question_id,omitempty"`
	ConnectorID   string   `json:"connector_id,omitempty"`
	SendSeq       string   `json:"send_seq,omitempty"`
	PieceIDs      []string `json:"piece_ids,omitempty"`
	DocumentID    string   `json:"document_id,omitempty"`
	SetID         string   `json:"set_id,omitempty"`
}

type GetFoundryWorkflowRunOutput struct {
	Run      FoundryWorkflowRunOutput      `json:"run"`
	Accesses []FoundryWorkflowAccessOutput `json:"accesses"`
	// NextAfterSeq is after_seq for the next page; absent when none follows.
	NextAfterSeq int32 `json:"next_after_seq,omitempty"`
}

// addFoundryWorkflowTools registers the workflow tools: creating one for a user who manages
// ontologies, and the rest for whoever reads ontologies' databases, an agent's key included.
func (s *Service) addFoundryWorkflowTools(server *mcp.Server, identity *Identity, reads bool) {
	no := false
	if !identity.ServiceKey && identity.HasPermission(PermissionWorkspaceOntologyManage) {
		addFoundryTool(s, server, identity, &mcp.Tool{Name: toolFoundryCreateWorkflow,
			Description: "Create a workflow of a Foundry ontology: an agent's task (such as answering support tickets) whose runs record what the agent read and wrote between its request and its answer. " +
				"Start a run with " + toolFoundryStartWorkflowRun + " each time the agent works on a request.",
			Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: &no, OpenWorldHint: &no},
		}, s.CreateFoundryWorkflow)
	}
	if !reads {
		return
	}
	addFoundryTool(s, server, identity, tool(toolFoundryListWorkflows,
		"List a Foundry ontology's workflows, newest first, with how many runs each kept."), s.ListFoundryWorkflows)
	// A host without API sources names no send tool.
	tracked := toolFoundryReadGraph + ", " + toolFoundryAsk + " and " + toolFoundryCompleteUpload
	if s.Events {
		tracked = toolFoundryReadGraph + ", " + toolFoundryAsk + ", " + toolFoundrySendPieces + " and " + toolFoundryCompleteUpload
	}
	addFoundryTool(s, server, identity, &mcp.Tool{Name: toolFoundryStartWorkflowRun,
		Description: "Start a run of a Foundry workflow when the agent takes on a request. Pass the run's id as workflow_run_id to " + tracked +
			" while working on it: each records what it read or wrote under the run. " +
			"Finish it with " + toolFoundryFinishWorkflowRun + " and the agent's answer.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: &no, OpenWorldHint: &no},
	}, s.StartFoundryWorkflowRun)
	addFoundryTool(s, server, identity, &mcp.Tool{Name: toolFoundryFinishWorkflowRun,
		Description: "Finish a run of a Foundry workflow with the agent's final answer, and its initial request if the run was started without it. A finished run records nothing more. Finishing again with the same answer is no error.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: &no, OpenWorldHint: &no},
	}, s.FinishFoundryWorkflowRun)
	addFoundryTool(s, server, identity, tool(toolFoundryGetWorkflowRun,
		"Get a run of a Foundry workflow with the calls it recorded, 100 at a time: each read with its result, question, send with its pieces' ids, and upload."), s.GetFoundryWorkflowRun)
}

func (s *Service) CreateFoundryWorkflow(
	ctx context.Context, identity *Identity, call *Call, input CreateFoundryWorkflowInput,
) (FoundryWorkflowOutput, int, error) {
	call.ResourceKind, call.ResourceID = "ontology", input.OntologyID
	if !ValidIDs([]string{input.WorkspaceID, input.OntologyID}) || input.Name == "" {
		return FoundryWorkflowOutput{}, 0, errCodeInvalidArguments
	}
	req := &foundryv1.CreateFoundryWorkflowRequest{
		WorkspaceId: input.WorkspaceID, OntologyId: input.OntologyID, Name: input.Name, Description: input.Description, ClientToken: input.ClientToken,
	}
	if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "CreateFoundryWorkflow"); err != nil {
		return FoundryWorkflowOutput{}, 0, err
	}
	response, err := s.Workflows.CreateFoundryWorkflow(ctx, req)
	if err != nil {
		return FoundryWorkflowOutput{}, 0, FoundryError(err)
	}
	out := FoundryWorkflow(response.GetWorkflow())
	out.ContentTrust = contentTrustUntrusted
	return out, 1, nil
}

func (s *Service) ListFoundryWorkflows(
	ctx context.Context, identity *Identity, call *Call, input ListFoundryWorkflowsInput,
) (ListFoundryWorkflowsOutput, int, error) {
	call.ResourceKind, call.ResourceID = "ontology", input.OntologyID
	if !ValidIDs([]string{input.WorkspaceID, input.OntologyID}) {
		return ListFoundryWorkflowsOutput{}, 0, errCodeInvalidArguments
	}
	req := &foundryv1.ListFoundryWorkflowsRequest{WorkspaceId: input.WorkspaceID, OntologyId: input.OntologyID, PageToken: input.PageToken}
	if err := s.AuthorizeFoundry(ctx, identity, req, QueryRead, "ListFoundryWorkflows"); err != nil {
		return ListFoundryWorkflowsOutput{}, 0, err
	}
	response, err := s.Workflows.ListFoundryWorkflows(ctx, req)
	if err != nil {
		return ListFoundryWorkflowsOutput{}, 0, FoundryError(err)
	}
	out := ListFoundryWorkflowsOutput{ContentTrust: contentTrustUntrusted, Workflows: []FoundryWorkflowOutput{}, NextPageToken: response.GetNextPageToken()}
	for _, w := range response.GetWorkflows() {
		out.Workflows = append(out.Workflows, FoundryWorkflow(w))
	}
	return out, len(out.Workflows), nil
}

func (s *Service) StartFoundryWorkflowRun(
	ctx context.Context, identity *Identity, call *Call, input StartFoundryWorkflowRunInput,
) (FoundryWorkflowRunOutput, int, error) {
	call.ResourceKind, call.ResourceID = "ontology", input.OntologyID
	if !ValidIDs([]string{input.WorkspaceID, input.OntologyID, input.WorkflowID}) {
		return FoundryWorkflowRunOutput{}, 0, errCodeInvalidArguments
	}
	req := &foundryv1.StartFoundryWorkflowRunRequest{
		WorkspaceId: input.WorkspaceID, OntologyId: input.OntologyID, WorkflowId: input.WorkflowID, Input: input.Input, ClientToken: input.ClientToken,
	}
	if err := s.AuthorizeFoundry(ctx, identity, req, QueryRead, "StartFoundryWorkflowRun"); err != nil {
		return FoundryWorkflowRunOutput{}, 0, err
	}
	response, err := s.Workflows.StartFoundryWorkflowRun(ctx, req)
	if err != nil {
		return FoundryWorkflowRunOutput{}, 0, FoundryError(err)
	}
	out := FoundryWorkflowRun(response.GetRun())
	out.Next = "Pass workflow_run_id " + out.ID + " to every Foundry call made for this request, then call " +
		toolFoundryFinishWorkflowRun + " with the answer."
	return out, 1, nil
}

func (s *Service) FinishFoundryWorkflowRun(
	ctx context.Context, identity *Identity, call *Call, input FinishFoundryWorkflowRunInput,
) (FoundryWorkflowRunOutput, int, error) {
	call.ResourceKind, call.ResourceID = "ontology", input.OntologyID
	if !ValidIDs([]string{input.WorkspaceID, input.OntologyID, input.WorkflowID, input.RunID}) || input.Output == "" {
		return FoundryWorkflowRunOutput{}, 0, errCodeInvalidArguments
	}
	req := &foundryv1.FinishFoundryWorkflowRunRequest{
		WorkspaceId: input.WorkspaceID, OntologyId: input.OntologyID, WorkflowId: input.WorkflowID, Id: input.RunID,
		Input: input.Input, Output: input.Output,
	}
	if err := s.AuthorizeFoundry(ctx, identity, req, QueryRead, "FinishFoundryWorkflowRun"); err != nil {
		return FoundryWorkflowRunOutput{}, 0, err
	}
	response, err := s.Workflows.FinishFoundryWorkflowRun(ctx, req)
	if err != nil {
		return FoundryWorkflowRunOutput{}, 0, FoundryError(err)
	}
	return FoundryWorkflowRun(response.GetRun()), 1, nil
}

func (s *Service) GetFoundryWorkflowRun(
	ctx context.Context, identity *Identity, call *Call, input GetFoundryWorkflowRunInput,
) (GetFoundryWorkflowRunOutput, int, error) {
	call.ResourceKind, call.ResourceID = "ontology", input.OntologyID
	if !ValidIDs([]string{input.WorkspaceID, input.OntologyID, input.WorkflowID, input.RunID}) || input.AfterSeq < 0 {
		return GetFoundryWorkflowRunOutput{}, 0, errCodeInvalidArguments
	}
	get := &foundryv1.GetFoundryWorkflowRunRequest{WorkspaceId: input.WorkspaceID, OntologyId: input.OntologyID, WorkflowId: input.WorkflowID, Id: input.RunID}
	list := &foundryv1.ListFoundryWorkflowRunAccessesRequest{
		WorkspaceId: input.WorkspaceID, OntologyId: input.OntologyID, WorkflowId: input.WorkflowID, Id: input.RunID, AfterSeq: input.AfterSeq,
	}
	if err := s.AuthorizeFoundry(ctx, identity, get, QueryRead, "GetFoundryWorkflowRun", "ListFoundryWorkflowRunAccesses"); err != nil {
		return GetFoundryWorkflowRunOutput{}, 0, err
	}
	run, err := s.Workflows.GetFoundryWorkflowRun(ctx, get)
	if err != nil {
		return GetFoundryWorkflowRunOutput{}, 0, FoundryError(err)
	}
	page, err := s.Workflows.ListFoundryWorkflowRunAccesses(ctx, list)
	if err != nil {
		return GetFoundryWorkflowRunOutput{}, 0, FoundryError(err)
	}
	out := GetFoundryWorkflowRunOutput{Run: FoundryWorkflowRun(run.GetRun()), Accesses: []FoundryWorkflowAccessOutput{}}
	for _, a := range page.GetAccesses() {
		out.Accesses = append(out.Accesses, FoundryWorkflowAccess(a))
	}
	if page.GetMore() && len(out.Accesses) > 0 {
		out.NextAfterSeq = out.Accesses[len(out.Accesses)-1].Seq
	}
	return out, len(out.Accesses), nil
}

// FoundryWorkflow is a workflow as the tools return it, without its content trust, which a list
// says once.
func FoundryWorkflow(w *foundryv1.FoundryWorkflow) FoundryWorkflowOutput {
	return FoundryWorkflowOutput{
		ID: w.GetId(), OntologyID: w.GetOntologyId(), Name: w.GetName(), Description: w.GetDescription(),
		Runs: w.GetRuns(), FinishedRuns: w.GetFinishedRuns(), CreatedAt: protoTimestamp(w.GetCreatedAt()), LastRunAt: protoTimestamp(w.GetLastRunAt()),
	}
}

// FoundryWorkflowRun is a run as the tools return it: its input and output are whoever sent
// them, so it is untrusted.
func FoundryWorkflowRun(r *foundryv1.FoundryWorkflowRun) FoundryWorkflowRunOutput {
	counts := r.GetCounts()
	if counts == nil {
		counts = &foundryv1.FoundryWorkflowRunCounts{}
	}
	return FoundryWorkflowRunOutput{
		ContentTrust: contentTrustUntrusted, ID: r.GetId(), WorkflowID: r.GetWorkflowId(), Status: r.GetStatus(),
		Input: r.GetInput(), Output: r.GetOutput(), StartedAt: protoTimestamp(r.GetStartedAt()), FinishedAt: protoTimestamp(r.GetFinishedAt()),
		Counts: counts,
	}
}

// FoundryWorkflowAccess is one call a run recorded, named by its kind.
func FoundryWorkflowAccess(a *foundryv1.FoundryWorkflowAccess) FoundryWorkflowAccessOutput {
	out := FoundryWorkflowAccessOutput{Seq: a.GetSeq(), At: protoTimestamp(a.GetAt())}
	switch x := a.GetAccess().(type) {
	case *foundryv1.FoundryWorkflowAccess_Read:
		out.Kind, out.LoadID, out.ResultOmitted = "read", x.Read.GetLoadId(), x.Read.GetResultOmitted()
		out.Reads = UntrustedJSON(x.Read.GetReadsJson())
		if !out.ResultOmitted {
			out.Result = UntrustedJSON(x.Read.GetResultJson())
		}
	case *foundryv1.FoundryWorkflowAccess_Question:
		out.Kind, out.LoadID, out.QuestionID = "question", x.Question.GetLoadId(), x.Question.GetQuestionId()
	case *foundryv1.FoundryWorkflowAccess_Send:
		out.Kind, out.ConnectorID, out.PieceIDs = "send", x.Send.GetConnectorId(), x.Send.GetPieceIds()
		out.SendSeq = strconv.FormatInt(x.Send.GetSeq(), 10)
	case *foundryv1.FoundryWorkflowAccess_Upload:
		out.Kind, out.DocumentID, out.SetID = "upload", x.Upload.GetDocumentId(), x.Upload.GetSetId()
	}
	return out
}
