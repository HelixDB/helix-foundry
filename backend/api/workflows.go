package api

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/access"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// workflowsPage and workflowRunsPage are how many a list returns at
	// once, workflowAccessesPage how many accesses.
	workflowsPage        = 50
	workflowRunsPage     = 50
	workflowAccessesPage = 100
	// workflowAccessesPageBytes ends a page of accesses early, well inside a
	// response's 4 MiB: a run keeps up to 4 MiB of results.
	workflowAccessesPageBytes = 2 << 20
	// listPreviewRunes is how much of a run's request and answer a list
	// returns; GetFoundryWorkflowRun returns them whole.
	listPreviewRunes = 500
)

// CreateFoundryWorkflow makes a workflow of a live ontology for the calling
// user. The same client token returns the workflow it made; an ontology
// holds up to foundrymodel.FoundryWorkflowsMax.
func (s *Service) CreateFoundryWorkflow(ctx context.Context, req *foundryv1.CreateFoundryWorkflowRequest) (*foundryv1.FoundryWorkflowResponse, error) {
	userID := actor(ctx).UserID
	if userID == "" {
		return nil, status.Error(codes.PermissionDenied, "a person creates workflows")
	}
	name := strings.TrimSpace(req.GetName())
	description := strings.TrimSpace(req.GetDescription())
	token := req.GetClientToken()
	switch {
	case name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > foundrymodel.FoundryWorkflowNameMax:
		return nil, status.Errorf(codes.InvalidArgument, "name must be 1 to %d characters", foundrymodel.FoundryWorkflowNameMax)
	case !utf8.ValidString(description) || utf8.RuneCountInString(description) > foundrymodel.FoundryWorkflowDescriptionMax:
		return nil, status.Errorf(codes.InvalidArgument, "description must be at most %d characters", foundrymodel.FoundryWorkflowDescriptionMax)
	case len(token) > maxClientTokenLength:
		return nil, status.Errorf(codes.InvalidArgument, "client_token is at most %d characters", maxClientTokenLength)
	}
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if token == "" {
		token = id.New()
	}
	w := &foundrymodel.FoundryWorkflow{
		ID: id.New(), WorkspaceID: o.WorkspaceID, OntologyID: o.ID, Name: name, Description: description,
		ClientToken: token, CreatedByUserID: userID,
	}
	switch err := s.deps.DB.CreateFoundryWorkflow(ctx, w); {
	case errors.Is(err, foundrystore.ErrConflict):
		existing, err := s.deps.DB.GetFoundryWorkflowByClientToken(ctx, o.ID, token)
		switch {
		case err != nil:
			return nil, status.Errorf(codes.Internal, "get workflow: %v", err)
		case existing.Name != name || existing.Description != description:
			return nil, status.Error(codes.InvalidArgument, "client_token was already used for another workflow")
		}
		w = existing
	case errors.Is(err, foundrystore.ErrLimitExceeded):
		return nil, status.Errorf(codes.FailedPrecondition, "an ontology holds up to %d workflows; delete one first", foundrymodel.FoundryWorkflowsMax)
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "ontology not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "create workflow: %v", err)
	}
	return s.foundryWorkflowResponse(ctx, o.ID, w.ID)
}

// ListFoundryWorkflows lists a live ontology's workflows, newest first.
func (s *Service) ListFoundryWorkflows(ctx context.Context, req *foundryv1.ListFoundryWorkflowsRequest) (*foundryv1.ListFoundryWorkflowsResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	workflows, next, err := s.deps.DB.ListFoundryWorkflows(ctx, o.ID, req.GetPageToken(), workflowsPage)
	switch {
	case errors.Is(err, foundrystore.ErrInvalidPageToken):
		return nil, status.Error(codes.InvalidArgument, "invalid page_token")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "list workflows: %v", err)
	}
	verdicts, err := s.workflowVerdicts(ctx, workflows)
	if err != nil {
		return nil, err
	}
	out := &foundryv1.ListFoundryWorkflowsResponse{NextPageToken: next}
	for _, w := range workflows {
		out.Workflows = append(out.Workflows, foundryWorkflowToProto(w, verdicts[w.ID]))
	}
	return out, nil
}

// GetFoundryWorkflow reads one workflow of a live ontology.
func (s *Service) GetFoundryWorkflow(ctx context.Context, req *foundryv1.GetFoundryWorkflowRequest) (*foundryv1.FoundryWorkflowResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	return s.foundryWorkflowResponse(ctx, o.ID, req.GetId())
}

// DeleteFoundryWorkflow deletes a workflow, its runs and what they recorded.
func (s *Service) DeleteFoundryWorkflow(ctx context.Context, req *foundryv1.DeleteFoundryWorkflowRequest) (*emptypb.Empty, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	switch err := s.deps.DB.DeleteFoundryWorkflow(ctx, o.ID, req.GetId()); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "workflow not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "delete workflow: %v", err)
	}
	return &emptypb.Empty{}, nil
}

// StartFoundryWorkflowRun opens a run of a workflow for the calling user or
// agent's service credential, with its input when given. The same client
// token returns the run it opened, refused when the input differs.
func (s *Service) StartFoundryWorkflowRun(ctx context.Context, req *foundryv1.StartFoundryWorkflowRunRequest) (*foundryv1.FoundryWorkflowRunResponse, error) {
	var by access.Principal
	switch a := actor(ctx); {
	case a.UserID != "":
		by = access.Principal{Kind: access.PrincipalUser, ID: a.UserID}
	case a.ServiceCredentialID != "":
		by = access.Principal{Kind: access.PrincipalServiceCredential, ID: a.ServiceCredentialID}
	default:
		return nil, status.Error(codes.PermissionDenied, "a person or an agent's key starts a workflow's runs")
	}
	input, err := workflowRunInput(req.GetInput())
	if err != nil {
		return nil, err
	}
	token := req.GetClientToken()
	if len(token) > maxClientTokenLength {
		return nil, status.Errorf(codes.InvalidArgument, "client_token is at most %d characters", maxClientTokenLength)
	}
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if token == "" {
		token = id.New()
	}
	r := &foundrymodel.FoundryWorkflowRun{
		ID: id.New(), WorkflowID: req.GetWorkflowId(), WorkspaceID: o.WorkspaceID, OntologyID: o.ID, ClientToken: token,
		StartedBy: by, Input: input,
	}
	switch err := s.deps.DB.StartFoundryWorkflowRun(ctx, r); {
	case errors.Is(err, foundrystore.ErrConflict):
		existing, err := s.deps.DB.GetFoundryWorkflowRunByClientToken(ctx, r.WorkflowID, token)
		switch {
		case err != nil:
			return nil, status.Errorf(codes.Internal, "get workflow run: %v", err)
		case existing.OntologyID != o.ID || !sameText(existing.Input, input):
			return nil, status.Error(codes.InvalidArgument, "client_token was already used for another run")
		}
		r = existing
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "workflow not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "start workflow run: %v", err)
	}
	return s.foundryWorkflowRunResponse(ctx, o.ID, r.WorkflowID, r.ID)
}

// ListFoundryWorkflowRuns lists a workflow's runs, newest first: a grantee
// holding only some of the ontology's current load lists those it started.
func (s *Service) ListFoundryWorkflowRuns(ctx context.Context, req *foundryv1.ListFoundryWorkflowRunsRequest) (*foundryv1.ListFoundryWorkflowRunsResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if _, err := s.foundryWorkflow(ctx, o.ID, req.GetWorkflowId()); err != nil {
		return nil, err
	}
	var startedBy *access.Principal
	if by, own := ownRunsOnly(ctx); own {
		startedBy = &by
	}
	runs, next, err := s.deps.DB.ListFoundryWorkflowRuns(ctx, req.GetWorkflowId(), startedBy, req.GetPageToken(), workflowRunsPage)
	switch {
	case errors.Is(err, foundrystore.ErrInvalidPageToken):
		return nil, status.Error(codes.InvalidArgument, "invalid page_token")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "list workflow runs: %v", err)
	}
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.ID
	}
	assessments, err := s.deps.DB.ListFoundryWorkflowRunAssessments(ctx, ids)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list assessments: %v", err)
	}
	out := &foundryv1.ListFoundryWorkflowRunsResponse{NextPageToken: next}
	for _, r := range runs {
		run := foundryWorkflowRunToProto(r, assessments[r.ID])
		// A page of whole requests and answers could pass a response's limit.
		run.Input, run.Output = previewText(run.Input), previewText(run.Output)
		out.Runs = append(out.Runs, run)
	}
	return out, nil
}

// GetFoundryWorkflowRun reads one run of a workflow.
func (s *Service) GetFoundryWorkflowRun(ctx context.Context, req *foundryv1.GetFoundryWorkflowRunRequest) (*foundryv1.FoundryWorkflowRunResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	return s.foundryWorkflowRunResponse(ctx, o.ID, req.GetWorkflowId(), req.GetId())
}

// FinishFoundryWorkflowRun closes an open run with its output, and its input
// when it was started without one. Finishing it again with the same input
// and output returns it.
func (s *Service) FinishFoundryWorkflowRun(ctx context.Context, req *foundryv1.FinishFoundryWorkflowRunRequest) (*foundryv1.FoundryWorkflowRunResponse, error) {
	input, err := workflowRunInput(req.GetInput())
	if err != nil {
		return nil, err
	}
	output := req.GetOutput()
	if strings.TrimSpace(output) == "" || !utf8.ValidString(output) || len(output) > foundrymodel.FoundryWorkflowOutputMaxBytes {
		return nil, status.Errorf(codes.InvalidArgument, "output is required, at most %d bytes of UTF-8", foundrymodel.FoundryWorkflowOutputMaxBytes)
	}
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	r, err := s.foundryWorkflowRun(ctx, o.ID, req.GetWorkflowId(), req.GetId())
	if err != nil {
		return nil, err
	}
	switch {
	case r.Finished != nil && r.Finished.Output == output && (input == nil || sameText(r.Input, input)):
		return s.foundryWorkflowRunResponse(ctx, o.ID, r.WorkflowID, r.ID)
	case r.Finished != nil:
		return nil, status.Error(codes.FailedPrecondition, "the run already finished with another answer")
	case r.Input == nil && input == nil:
		return nil, status.Error(codes.InvalidArgument, "input is required: the run was started without it")
	case r.Input != nil && input != nil && *r.Input != *input:
		return nil, status.Error(codes.InvalidArgument, "input differs from the one the run was started with")
	}
	switch err := s.deps.DB.FinishFoundryWorkflowRun(ctx, r.ID, input, output, s.deps.Cfg.Foundry.Workflows.Assess.Enabled); {
	case errors.Is(err, foundrystore.ErrNotFound):
		// Finished, or deleted, meanwhile: answer as a retry would be. The run
		// is no longer open, so the retry ends before it gets here.
		return s.FinishFoundryWorkflowRun(ctx, req)
	case err != nil:
		return nil, status.Errorf(codes.Internal, "finish workflow run: %v", err)
	}
	return s.foundryWorkflowRunResponse(ctx, o.ID, r.WorkflowID, r.ID)
}

// ListFoundryWorkflowRunAccesses returns a page of what a run recorded, in
// order.
func (s *Service) ListFoundryWorkflowRunAccesses(ctx context.Context, req *foundryv1.ListFoundryWorkflowRunAccessesRequest) (*foundryv1.ListFoundryWorkflowRunAccessesResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	r, err := s.foundryWorkflowRun(ctx, o.ID, req.GetWorkflowId(), req.GetId())
	if err != nil {
		return nil, err
	}
	accesses, more, err := s.deps.DB.ListFoundryWorkflowRunAccesses(ctx, r.ID, max(req.GetAfterSeq(), 0), workflowAccessesPage)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list workflow run accesses: %v", err)
	}
	out := &foundryv1.ListFoundryWorkflowRunAccessesResponse{More: more}
	size := 0
	for i, a := range accesses {
		p := foundryWorkflowAccessToProto(a)
		size += proto.Size(p)
		if i > 0 && size > workflowAccessesPageBytes {
			// The rest are the next page's.
			out.More = true
			break
		}
		out.Accesses = append(out.Accesses, p)
	}
	return out, nil
}

// trackedRun is the open run a call names in workflow_run_id to record what
// it accessed under, checked before the call runs so a bad id refuses it
// before it reads or writes anything: nil for none. ontologyID is the
// ontology the call reads (a read, a question, an upload's ontology route),
// or empty for a call of the workspace (a send), whose run may be of any
// of its ontologies. A grantee records only with its Workflows permission,
// and, holding only some of the ontology's current load, only under a run it
// started.
func (s *Service) trackedRun(ctx context.Context, workspaceID, ontologyID, runID string) (*foundrymodel.FoundryWorkflowRun, error) {
	if runID == "" {
		return nil, nil
	}
	if g, ok := access.GranteeFrom(ctx); ok {
		switch on, err := s.workflowsOn(ctx, g); {
		case err != nil:
			return nil, err
		case !on:
			return nil, errWorkflowsOff
		}
	}
	var r *foundrymodel.FoundryWorkflowRun
	var err error
	if ontologyID == "" {
		r, err = s.deps.DB.GetFoundryWorkflowRunInWorkspace(ctx, workspaceID, runID)
	} else {
		r, err = s.deps.DB.GetFoundryWorkflowRun(ctx, ontologyID, runID)
	}
	by, own := ownRunsOnly(ctx)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || err == nil && (r.WorkspaceID != workspaceID || own && r.StartedBy != by):
		return nil, status.Error(codes.NotFound, "workflow run not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "get workflow run: %v", err)
	case r.Finished != nil:
		return nil, status.Error(codes.FailedPrecondition, "the workflow run finished: start another to record more")
	case r.Counts.Accesses >= foundrymodel.FoundryWorkflowRunMaxAccesses:
		return nil, status.Errorf(codes.ResourceExhausted, "a workflow run records up to %d calls", foundrymodel.FoundryWorkflowRunMaxAccesses)
	}
	return r, nil
}

// track records call under run, if any, once the call succeeded. The call
// is done whatever the run does: a run that finished, filled or went since
// trackedRun checked it, or a store that failed, leaves it unrecorded,
// which is logged, never the call's error, so a retry neither fails nor
// repeats what it did.
func (s *Service) track(ctx context.Context, run *foundrymodel.FoundryWorkflowRun, call foundrymodel.FoundryWorkflowCall) {
	if run == nil {
		return
	}
	if _, err := s.deps.DB.RecordFoundryWorkflowAccess(ctx, run.ID, call); err != nil {
		log.From(ctx).Warn("foundry workflow: a call was made but its run recorded nothing", "run", run.ID,
			"kind", call.Kind(), "err", err)
	}
}

func (s *Service) foundryWorkflow(ctx context.Context, ontologyID, workflowID string) (*foundrymodel.FoundryWorkflow, error) {
	w, err := s.deps.DB.GetFoundryWorkflow(ctx, ontologyID, workflowID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "workflow not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "get workflow: %v", err)
	}
	return w, nil
}

func (s *Service) foundryWorkflowResponse(ctx context.Context, ontologyID, workflowID string) (*foundryv1.FoundryWorkflowResponse, error) {
	w, err := s.foundryWorkflow(ctx, ontologyID, workflowID)
	if err != nil {
		return nil, err
	}
	verdicts, err := s.workflowVerdicts(ctx, []*foundrymodel.FoundryWorkflow{w})
	if err != nil {
		return nil, err
	}
	return &foundryv1.FoundryWorkflowResponse{Workflow: foundryWorkflowToProto(w, verdicts[w.ID])}, nil
}

// ownRunsOnly is the principal whose runs alone the call sees: a grantee's,
// unless it holds all of the ontology's current load. The host's callers,
// and a grantee holding all of it, see every run.
func ownRunsOnly(ctx context.Context) (access.Principal, bool) {
	g, ok := access.GranteeFrom(ctx)
	if !ok {
		return access.Principal{}, false
	}
	l, ok := granteeLoad(ctx)
	return g.Principal, !ok || l.Partial
}

// workflowVerdicts counts the done assessments of workflows' runs by verdict.
// For a caller who sees only its own runs, it counts those alone, and
// narrows each workflow's run counts to them too.
func (s *Service) workflowVerdicts(ctx context.Context, workflows []*foundrymodel.FoundryWorkflow) (map[string]map[string]int64, error) {
	ids := make([]string, len(workflows))
	for i, w := range workflows {
		ids[i] = w.ID
	}
	by, own := ownRunsOnly(ctx)
	if !own {
		verdicts, err := s.deps.DB.CountFoundryWorkflowVerdicts(ctx, ids)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "count verdicts: %v", err)
		}
		return verdicts, nil
	}
	tallies, err := s.deps.DB.TallyFoundryWorkflowRuns(ctx, ids, by)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "count runs: %v", err)
	}
	verdicts := map[string]map[string]int64{}
	for _, w := range workflows {
		t := tallies[w.ID]
		w.Runs, w.FinishedRuns, w.LastRunAt = t.Runs, t.FinishedRuns, t.LastRunAt
		verdicts[w.ID] = t.Verdicts
	}
	return verdicts, nil
}

// foundryWorkflowRun reads a run of the ontology's workflow workflowID: a
// run of another workflow is not found, nor one someone else started for a
// caller who sees only its own (ownRunsOnly).
func (s *Service) foundryWorkflowRun(ctx context.Context, ontologyID, workflowID, runID string) (*foundrymodel.FoundryWorkflowRun, error) {
	r, err := s.deps.DB.GetFoundryWorkflowRun(ctx, ontologyID, runID)
	by, own := ownRunsOnly(ctx)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || err == nil && (r.WorkflowID != workflowID || own && r.StartedBy != by):
		return nil, status.Error(codes.NotFound, "workflow run not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "get workflow run: %v", err)
	}
	return r, nil
}

func (s *Service) foundryWorkflowRunResponse(ctx context.Context, ontologyID, workflowID, runID string) (*foundryv1.FoundryWorkflowRunResponse, error) {
	r, err := s.foundryWorkflowRun(ctx, ontologyID, workflowID, runID)
	if err != nil {
		return nil, err
	}
	a, err := s.deps.DB.GetFoundryWorkflowRunAssessment(ctx, r.ID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		a = nil
	case err != nil:
		return nil, status.Errorf(codes.Internal, "get assessment: %v", err)
	}
	return &foundryv1.FoundryWorkflowRunResponse{Run: foundryWorkflowRunToProto(r, a)}, nil
}

// AssessFoundryWorkflowRun queues a finished run's assessment again, where
// assessments are on.
func (s *Service) AssessFoundryWorkflowRun(ctx context.Context, req *foundryv1.AssessFoundryWorkflowRunRequest) (*foundryv1.FoundryWorkflowRunResponse, error) {
	if !s.deps.Cfg.Foundry.Workflows.Assess.Enabled {
		return nil, status.Error(codes.FailedPrecondition, "run assessments are off here")
	}
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	r, err := s.foundryWorkflowRun(ctx, o.ID, req.GetWorkflowId(), req.GetId())
	if err != nil {
		return nil, err
	}
	switch err := s.deps.DB.QueueFoundryWorkflowRunAssessment(ctx, r.ID); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.FailedPrecondition, "the run is still open: it is assessed once it finishes")
	case errors.Is(err, foundrystore.ErrConflict):
		return nil, status.Error(codes.FailedPrecondition, "the run is being assessed already")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "queue assessment: %v", err)
	}
	return s.foundryWorkflowRunResponse(ctx, o.ID, r.WorkflowID, r.ID)
}

// workflowRunInput checks a run's input as sent: nil when empty.
func workflowRunInput(input string) (*string, error) {
	switch {
	case input == "":
		return nil, nil
	case strings.TrimSpace(input) == "" || !utf8.ValidString(input) || len(input) > foundrymodel.FoundryWorkflowInputMaxBytes:
		return nil, status.Errorf(codes.InvalidArgument, "input is at most %d bytes of UTF-8, not only spaces", foundrymodel.FoundryWorkflowInputMaxBytes)
	}
	return &input, nil
}

// sameText reports whether two optional texts are both absent or equal.
func sameText(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func foundryWorkflowToProto(w *foundrymodel.FoundryWorkflow, verdicts map[string]int64) *foundryv1.FoundryWorkflow {
	out := &foundryv1.FoundryWorkflow{
		Id: w.ID, OntologyId: w.OntologyID, Name: w.Name, Description: w.Description, CreatedByUserId: w.CreatedByUserID,
		CreatedAt: timestamppb.New(w.CreatedAt), Runs: w.Runs, FinishedRuns: w.FinishedRuns,
	}
	if w.LastRunAt != nil {
		out.LastRunAt = timestamppb.New(*w.LastRunAt)
	}
	for _, v := range foundrymodel.FoundryWorkflowVerdicts {
		if n := verdicts[v]; n > 0 {
			out.Verdicts = append(out.Verdicts, &foundryv1.FoundryWorkflowVerdictCount{Verdict: v, Runs: n})
		}
	}
	return out
}

func foundryWorkflowRunToProto(r *foundrymodel.FoundryWorkflowRun, a *foundrymodel.FoundryWorkflowAssessment) *foundryv1.FoundryWorkflowRun {
	c := r.Counts
	out := &foundryv1.FoundryWorkflowRun{
		Id: r.ID, WorkflowId: r.WorkflowID, Status: string(r.Status()), StartedAt: timestamppb.New(r.StartedAt),
		Counts: &foundryv1.FoundryWorkflowRunCounts{
			Accesses: c.Accesses, Reads: c.Reads, Questions: c.Questions, Sends: c.Sends, Uploads: c.Uploads,
			Pieces: c.Pieces, ResultsOmitted: c.ResultsOmitted,
		},
	}
	if r.Input != nil {
		out.Input = *r.Input
	}
	if r.Finished != nil {
		out.Output, out.FinishedAt = r.Finished.Output, timestamppb.New(r.Finished.At)
	}
	switch r.StartedBy.Kind {
	case access.PrincipalUser:
		out.StartedByUserId = r.StartedBy.ID
	case access.PrincipalServiceCredential:
		out.StartedByServiceCredentialId = r.StartedBy.ID
	}
	if a != nil {
		out.Assessment = &foundryv1.FoundryWorkflowAssessment{
			Status: string(a.Status), Verdict: a.Verdict, AssessmentJson: string(a.Assessment), LeftOut: a.LeftOut,
			Error: a.Error, UsdMicros: a.USDMicros, QueuedAt: timestamppb.New(a.QueuedAt),
		}
		if a.FinishedAt != nil {
			out.Assessment.FinishedAt = timestamppb.New(*a.FinishedAt)
		}
	}
	return out
}

func foundryWorkflowAccessToProto(a *foundrymodel.FoundryWorkflowAccess) *foundryv1.FoundryWorkflowAccess {
	out := &foundryv1.FoundryWorkflowAccess{Seq: a.Seq, At: timestamppb.New(a.At)}
	switch c := a.Call.(type) {
	case *foundrymodel.FoundryWorkflowRead:
		out.Access = &foundryv1.FoundryWorkflowAccess_Read{Read: &foundryv1.FoundryWorkflowRead{
			LoadId: c.LoadID, ReadsJson: string(c.Reads), ResultJson: string(c.Result), ResultOmitted: c.Result == nil,
		}}
	case *foundrymodel.FoundryWorkflowQuestion:
		out.Access = &foundryv1.FoundryWorkflowAccess_Question{Question: &foundryv1.FoundryWorkflowQuestion{
			LoadId: c.LoadID, QuestionId: c.QuestionID,
		}}
	case *foundrymodel.FoundryWorkflowSend:
		out.Access = &foundryv1.FoundryWorkflowAccess_Send{Send: &foundryv1.FoundryWorkflowSend{
			ConnectorId: c.ConnectorID, Seq: c.Seq, PieceIds: c.PieceIDs,
		}}
	case *foundrymodel.FoundryWorkflowUpload:
		out.Access = &foundryv1.FoundryWorkflowAccess_Upload{Upload: &foundryv1.FoundryWorkflowUpload{
			DocumentId: c.DocumentID, SetId: c.SetID,
		}}
	}
	return out
}

// previewText is text cut to listPreviewRunes characters, with an ellipsis
// when it was cut.
func previewText(text string) string {
	if utf8.RuneCountInString(text) <= listPreviewRunes {
		return text
	}
	return string([]rune(text)[:listPreviewRunes-1]) + "…"
}
