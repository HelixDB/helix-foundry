package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// workflowStore keeps workflows, runs and accesses in memory over the
// store a harness built, which answers everything else.
type workflowStore struct {
	foundrystore.Store
	workflows []*foundrymodel.FoundryWorkflow
	runs      []*foundrymodel.FoundryWorkflowRun
	accesses  map[string][]*foundrymodel.FoundryWorkflowAccess
	// createErr fails the next create; recordErr every record.
	createErr, recordErr error
	// raceFinish, when set, finishes the run with this output just before a
	// finish's guarded write, which then finds it finished.
	raceFinish string
}

func newWorkflowStore(inner foundrystore.Store) *workflowStore {
	return &workflowStore{Store: inner, accesses: map[string][]*foundrymodel.FoundryWorkflowAccess{}}
}

func (s *workflowStore) CreateFoundryWorkflow(_ context.Context, w *foundrymodel.FoundryWorkflow) error {
	if err := s.createErr; err != nil {
		s.createErr = nil
		return err
	}
	for _, x := range s.workflows {
		if x.OntologyID == w.OntologyID && x.ClientToken == w.ClientToken {
			return foundrystore.ErrConflict
		}
	}
	stored := *w
	stored.CreatedAt = time.Now()
	s.workflows = append(s.workflows, &stored)
	return nil
}

// counted is w with its runs counted, as the store reads it.
func (s *workflowStore) counted(w *foundrymodel.FoundryWorkflow) *foundrymodel.FoundryWorkflow {
	copied := *w
	for _, r := range s.runs {
		if r.WorkflowID == w.ID {
			copied.Runs++
		}
	}
	return &copied
}

func (s *workflowStore) find(match func(*foundrymodel.FoundryWorkflow) bool) (*foundrymodel.FoundryWorkflow, error) {
	for _, w := range s.workflows {
		if match(w) {
			return s.counted(w), nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *workflowStore) GetFoundryWorkflow(_ context.Context, ontologyID, id string) (*foundrymodel.FoundryWorkflow, error) {
	return s.find(func(w *foundrymodel.FoundryWorkflow) bool { return w.ID == id && w.OntologyID == ontologyID })
}

func (s *workflowStore) GetFoundryWorkflowByClientToken(_ context.Context, ontologyID, token string) (*foundrymodel.FoundryWorkflow, error) {
	return s.find(func(w *foundrymodel.FoundryWorkflow) bool {
		return w.ClientToken == token && w.OntologyID == ontologyID
	})
}

func (s *workflowStore) ListFoundryWorkflows(_ context.Context, ontologyID, pageToken string, _ int) ([]*foundrymodel.FoundryWorkflow, string, error) {
	if pageToken == "bad" {
		return nil, "", foundrystore.ErrInvalidPageToken
	}
	var out []*foundrymodel.FoundryWorkflow
	for _, w := range s.workflows {
		if w.OntologyID == ontologyID {
			out = append(out, s.counted(w))
		}
	}
	return out, "next", nil
}

func (s *workflowStore) DeleteFoundryWorkflow(_ context.Context, ontologyID, id string) error {
	n := len(s.workflows)
	s.workflows = slices.DeleteFunc(s.workflows, func(w *foundrymodel.FoundryWorkflow) bool { return w.ID == id && w.OntologyID == ontologyID })
	if len(s.workflows) == n {
		return foundrystore.ErrNotFound
	}
	s.runs = slices.DeleteFunc(s.runs, func(r *foundrymodel.FoundryWorkflowRun) bool { return r.WorkflowID == id })
	return nil
}

func (s *workflowStore) StartFoundryWorkflowRun(_ context.Context, r *foundrymodel.FoundryWorkflowRun) error {
	if _, err := s.GetFoundryWorkflow(context.Background(), r.OntologyID, r.WorkflowID); err != nil {
		return err
	}
	for _, x := range s.runs {
		if x.WorkflowID == r.WorkflowID && x.ClientToken == r.ClientToken {
			return foundrystore.ErrConflict
		}
	}
	stored := *r
	stored.StartedAt = time.Now()
	s.runs = append(s.runs, &stored)
	return nil
}

func (s *workflowStore) run(match func(*foundrymodel.FoundryWorkflowRun) bool) (*foundrymodel.FoundryWorkflowRun, error) {
	for _, r := range s.runs {
		if match(r) {
			copied := *r
			return &copied, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *workflowStore) GetFoundryWorkflowRun(_ context.Context, ontologyID, id string) (*foundrymodel.FoundryWorkflowRun, error) {
	return s.run(func(r *foundrymodel.FoundryWorkflowRun) bool { return r.ID == id && r.OntologyID == ontologyID })
}

func (s *workflowStore) GetFoundryWorkflowRunInWorkspace(_ context.Context, workspaceID, id string) (*foundrymodel.FoundryWorkflowRun, error) {
	return s.run(func(r *foundrymodel.FoundryWorkflowRun) bool { return r.ID == id && r.WorkspaceID == workspaceID })
}

func (s *workflowStore) GetFoundryWorkflowRunByClientToken(_ context.Context, workflowID, token string) (*foundrymodel.FoundryWorkflowRun, error) {
	return s.run(func(r *foundrymodel.FoundryWorkflowRun) bool {
		return r.ClientToken == token && r.WorkflowID == workflowID
	})
}

func (s *workflowStore) ListFoundryWorkflowRuns(_ context.Context, workflowID, pageToken string, _ int) ([]*foundrymodel.FoundryWorkflowRun, string, error) {
	if pageToken == "bad" {
		return nil, "", foundrystore.ErrInvalidPageToken
	}
	var out []*foundrymodel.FoundryWorkflowRun
	for _, r := range s.runs {
		if r.WorkflowID == workflowID {
			out = append(out, r)
		}
	}
	return out, "", nil
}

func (s *workflowStore) FinishFoundryWorkflowRun(_ context.Context, id string, input *string, output string) error {
	i := slices.IndexFunc(s.runs, func(r *foundrymodel.FoundryWorkflowRun) bool { return r.ID == id })
	if i < 0 || s.runs[i].Finished != nil {
		return foundrystore.ErrNotFound
	}
	r := s.runs[i]
	if r.Input == nil {
		r.Input = input
	}
	if s.raceFinish != "" {
		r.Finished = &foundrymodel.FoundryWorkflowRunEnd{At: time.Now(), Output: s.raceFinish}
		return foundrystore.ErrNotFound
	}
	r.Finished = &foundrymodel.FoundryWorkflowRunEnd{At: time.Now(), Output: output}
	return nil
}

func (s *workflowStore) RecordFoundryWorkflowAccess(_ context.Context, runID string, call foundrymodel.FoundryWorkflowCall) (*foundrymodel.FoundryWorkflowAccess, error) {
	if s.recordErr != nil {
		return nil, s.recordErr
	}
	for _, a := range s.accesses[runID] {
		if key := call.DedupeKey(); key != "" && a.Call.DedupeKey() == key {
			return a, nil
		}
	}
	a := &foundrymodel.FoundryWorkflowAccess{Seq: int32(len(s.accesses[runID]) + 1), At: time.Now(), Call: call}
	s.accesses[runID] = append(s.accesses[runID], a)
	i := slices.IndexFunc(s.runs, func(r *foundrymodel.FoundryWorkflowRun) bool { return r.ID == runID })
	s.runs[i].Counts.Accesses++
	return a, nil
}

func (s *workflowStore) ListFoundryWorkflowRunAccesses(_ context.Context, runID string, afterSeq int32, limit int) ([]*foundrymodel.FoundryWorkflowAccess, bool, error) {
	var out []*foundrymodel.FoundryWorkflowAccess
	for _, a := range s.accesses[runID] {
		if a.Seq > afterSeq {
			out = append(out, a)
		}
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// asPerson is ctx for the test user.
func asPerson() context.Context { return ports.WithActor(context.Background(), person) }

func workflowService(t *testing.T) (*Service, *workflowStore) {
	t.Helper()
	s, foundry, _ := foundryService(t)
	store := newWorkflowStore(foundry)
	s.deps.DB = store
	return s, store
}

// startRun makes workflow "Triage" of ont-1 and starts a run of it.
func startRun(t *testing.T, s *Service, input string) *foundryv1.FoundryWorkflowRun {
	t.Helper()
	w, err := s.CreateFoundryWorkflow(asPerson(), &foundryv1.CreateFoundryWorkflowRequest{
		WorkspaceId: "ws-1", OntologyId: "ont-1", Name: "Triage", ClientToken: "triage",
	})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.StartFoundryWorkflowRun(asPerson(), &foundryv1.StartFoundryWorkflowRunRequest{
		WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: w.GetWorkflow().GetId(), Input: input,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r.GetRun()
}

func wantCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("code = %v (%v), want %v", status.Code(err), err, code)
	}
}

func TestCreateFoundryWorkflow(t *testing.T) {
	create := func(s *Service, ctx context.Context, mutate func(*foundryv1.CreateFoundryWorkflowRequest)) (*foundryv1.FoundryWorkflowResponse, error) {
		req := &foundryv1.CreateFoundryWorkflowRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", Name: "  Support triage ", Description: " Answers tickets ", ClientToken: "c1",
		}
		if mutate != nil {
			mutate(req)
		}
		return s.CreateFoundryWorkflow(ctx, req)
	}

	t.Run("makes a workflow for the user, the same token the same one", func(t *testing.T) {
		s, store := workflowService(t)
		resp, err := create(s, asPerson(), nil)
		if err != nil {
			t.Fatal(err)
		}
		w := resp.GetWorkflow()
		if w.GetId() == "" || w.GetName() != "Support triage" || w.GetDescription() != "Answers tickets" ||
			w.GetOntologyId() != "ont-1" || w.GetCreatedByUserId() != "user-1" || w.GetLastRunAt() != nil {
			t.Fatalf("workflow = %+v", w)
		}
		again, err := create(s, asPerson(), nil)
		if err != nil || again.GetWorkflow().GetId() != w.GetId() || len(store.workflows) != 1 {
			t.Fatalf("again = %v, %v", again, err)
		}
		_, err = create(s, asPerson(), func(r *foundryv1.CreateFoundryWorkflowRequest) { r.Name = "Other" })
		wantCode(t, err, codes.InvalidArgument)
	})
	t.Run("without a token, each create is new", func(t *testing.T) {
		s, store := workflowService(t)
		for range 2 {
			if _, err := create(s, asPerson(), func(r *foundryv1.CreateFoundryWorkflowRequest) { r.ClientToken = "" }); err != nil {
				t.Fatal(err)
			}
		}
		if len(store.workflows) != 2 || store.workflows[0].ClientToken == store.workflows[1].ClientToken {
			t.Fatalf("workflows = %+v", store.workflows)
		}
	})
	t.Run("refuses what it can't make", func(t *testing.T) {
		for name, tc := range map[string]struct {
			ctx    context.Context
			mutate func(*foundryv1.CreateFoundryWorkflowRequest)
			want   codes.Code
		}{
			"no user":          {ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceMCP, ServiceCredentialID: "cred-1"}), nil, codes.PermissionDenied},
			"blank name":       {asPerson(), func(r *foundryv1.CreateFoundryWorkflowRequest) { r.Name = "  " }, codes.InvalidArgument},
			"long name":        {asPerson(), func(r *foundryv1.CreateFoundryWorkflowRequest) { r.Name = strings.Repeat("é", 101) }, codes.InvalidArgument},
			"invalid name":     {asPerson(), func(r *foundryv1.CreateFoundryWorkflowRequest) { r.Name = "a\xff" }, codes.InvalidArgument},
			"long description": {asPerson(), func(r *foundryv1.CreateFoundryWorkflowRequest) { r.Description = strings.Repeat("d", 2001) }, codes.InvalidArgument},
			"long token":       {asPerson(), func(r *foundryv1.CreateFoundryWorkflowRequest) { r.ClientToken = strings.Repeat("t", 65) }, codes.InvalidArgument},
			"other workspace":  {asPerson(), func(r *foundryv1.CreateFoundryWorkflowRequest) { r.OntologyId = "ont-2" }, codes.NotFound},
		} {
			t.Run(name, func(t *testing.T) {
				s, _ := workflowService(t)
				_, err := create(s, tc.ctx, tc.mutate)
				wantCode(t, err, tc.want)
			})
		}
	})
	t.Run("says why the store refused it", func(t *testing.T) {
		for err, want := range map[error]codes.Code{
			foundrystore.ErrLimitExceeded: codes.FailedPrecondition,
			foundrystore.ErrNotFound:      codes.NotFound,
			errors.New("outage"):          codes.Internal,
		} {
			s, store := workflowService(t)
			store.createErr = err
			_, got := create(s, asPerson(), nil)
			wantCode(t, got, want)
		}
	})
}

func TestFoundryWorkflowReads(t *testing.T) {
	s, store := workflowService(t)
	run := startRun(t, s, "")
	w := store.workflows[0]
	ctx := asPerson()

	list, err := s.ListFoundryWorkflows(ctx, &foundryv1.ListFoundryWorkflowsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
	if err != nil || len(list.GetWorkflows()) != 1 || list.GetWorkflows()[0].GetRuns() != 1 || list.GetNextPageToken() != "next" {
		t.Fatalf("list = %v, %v", list, err)
	}
	_, err = s.ListFoundryWorkflows(ctx, &foundryv1.ListFoundryWorkflowsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", PageToken: "bad"})
	wantCode(t, err, codes.InvalidArgument)
	_, err = s.ListFoundryWorkflows(ctx, &foundryv1.ListFoundryWorkflowsRequest{WorkspaceId: "ws-1", OntologyId: "ont-2"})
	wantCode(t, err, codes.NotFound)

	got, err := s.GetFoundryWorkflow(ctx, &foundryv1.GetFoundryWorkflowRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: w.ID})
	if err != nil || got.GetWorkflow().GetName() != "Triage" {
		t.Fatalf("get = %v, %v", got, err)
	}
	_, err = s.GetFoundryWorkflow(ctx, &foundryv1.GetFoundryWorkflowRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "nope"})
	wantCode(t, err, codes.NotFound)

	runs, err := s.ListFoundryWorkflowRuns(ctx, &foundryv1.ListFoundryWorkflowRunsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: w.ID})
	if err != nil || len(runs.GetRuns()) != 1 || runs.GetRuns()[0].GetId() != run.GetId() || runs.GetRuns()[0].GetStatus() != "open" {
		t.Fatalf("runs = %v, %v", runs, err)
	}
	_, err = s.ListFoundryWorkflowRuns(ctx, &foundryv1.ListFoundryWorkflowRunsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: w.ID, PageToken: "bad"})
	wantCode(t, err, codes.InvalidArgument)
	_, err = s.ListFoundryWorkflowRuns(ctx, &foundryv1.ListFoundryWorkflowRunsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: "nope"})
	wantCode(t, err, codes.NotFound)

	_, err = s.GetFoundryWorkflowRun(ctx, &foundryv1.GetFoundryWorkflowRunRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: "other", Id: run.GetId()})
	wantCode(t, err, codes.NotFound)

	// Every kind of access reads back as its own message.
	for _, call := range []foundrymodel.FoundryWorkflowCall{
		&foundrymodel.FoundryWorkflowRead{LoadID: "load-1", Reads: json.RawMessage(`[]`), Result: json.RawMessage(`{"results":[]}`)},
		&foundrymodel.FoundryWorkflowRead{LoadID: "load-1", Reads: json.RawMessage(`[]`)},
		&foundrymodel.FoundryWorkflowQuestion{LoadID: "load-1", QuestionID: "q-1"},
		&foundrymodel.FoundryWorkflowSend{ConnectorID: "c-1", Seq: 4, PieceIDs: []string{"p-1"}},
		&foundrymodel.FoundryWorkflowUpload{DocumentID: "d-1", SetID: "s-1"},
	} {
		if _, err := store.RecordFoundryWorkflowAccess(ctx, run.GetId(), call); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.ListFoundryWorkflowRunAccesses(ctx, &foundryv1.ListFoundryWorkflowRunAccessesRequest{
		WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: w.ID, Id: run.GetId(), AfterSeq: -3,
	})
	if err != nil || page.GetMore() || len(page.GetAccesses()) != 5 {
		t.Fatalf("accesses = %v, %v", page, err)
	}
	a := page.GetAccesses()
	if r := a[0].GetRead(); r.GetResultJson() != `{"results":[]}` || r.GetResultOmitted() || r.GetLoadId() != "load-1" {
		t.Fatalf("read = %+v", r)
	}
	if r := a[1].GetRead(); r.GetResultJson() != "" || !r.GetResultOmitted() {
		t.Fatalf("omitted read = %+v", r)
	}
	if q := a[2].GetQuestion(); q.GetQuestionId() != "q-1" || a[2].GetSeq() != 3 {
		t.Fatalf("question = %+v", a[2])
	}
	if sent := a[3].GetSend(); sent.GetConnectorId() != "c-1" || sent.GetSeq() != 4 || !slices.Equal(sent.GetPieceIds(), []string{"p-1"}) {
		t.Fatalf("send = %+v", sent)
	}
	if u := a[4].GetUpload(); u.GetDocumentId() != "d-1" || u.GetSetId() != "s-1" {
		t.Fatalf("upload = %+v", u)
	}

	if _, err := s.DeleteFoundryWorkflow(ctx, &foundryv1.DeleteFoundryWorkflowRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: w.ID}); err != nil {
		t.Fatal(err)
	}
	_, err = s.DeleteFoundryWorkflow(ctx, &foundryv1.DeleteFoundryWorkflowRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: w.ID})
	wantCode(t, err, codes.NotFound)
	_, err = s.ListFoundryWorkflowRunAccesses(ctx, &foundryv1.ListFoundryWorkflowRunAccessesRequest{
		WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: w.ID, Id: run.GetId(),
	})
	wantCode(t, err, codes.NotFound)
}

func TestStartFoundryWorkflowRun(t *testing.T) {
	start := func(s *Service, ctx context.Context, workflowID, input, token string) (*foundryv1.FoundryWorkflowRunResponse, error) {
		return s.StartFoundryWorkflowRun(ctx, &foundryv1.StartFoundryWorkflowRunRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: workflowID, Input: input, ClientToken: token,
		})
	}
	s, store := workflowService(t)
	first := startRun(t, s, "Who renews?")
	w := store.workflows[0].ID
	if first.GetInput() != "Who renews?" || first.GetStartedByUserId() != "user-1" || first.GetStatus() != "open" || first.GetFinishedAt() != nil {
		t.Fatalf("run = %+v", first)
	}

	t.Run("an agent's key starts one", func(t *testing.T) {
		key := ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceMCP, ServiceCredentialID: "cred-1"})
		r, err := start(s, key, w, "", "k1")
		if err != nil || r.GetRun().GetStartedByServiceCredentialId() != "cred-1" || r.GetRun().GetStartedByUserId() != "" || r.GetRun().GetInput() != "" {
			t.Fatalf("run = %v, %v", r, err)
		}
		again, err := start(s, key, w, "", "k1")
		if err != nil || again.GetRun().GetId() != r.GetRun().GetId() {
			t.Fatalf("again = %v, %v", again, err)
		}
		_, err = start(s, key, w, "another input", "k1")
		wantCode(t, err, codes.InvalidArgument)
	})
	t.Run("refuses what it can't start", func(t *testing.T) {
		sender := ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceEventSource, EventSourceTokenID: "tok-1"})
		for name, tc := range map[string]struct {
			ctx                    context.Context
			workflow, input, token string
			want                   codes.Code
		}{
			"no person or key": {sender, w, "", "", codes.PermissionDenied},
			"only spaces":      {asPerson(), w, "  \n", "", codes.InvalidArgument},
			"invalid input":    {asPerson(), w, "a\xff", "", codes.InvalidArgument},
			"long input":       {asPerson(), w, strings.Repeat("i", foundrymodel.FoundryWorkflowInputMaxBytes+1), "", codes.InvalidArgument},
			"long token":       {asPerson(), w, "", strings.Repeat("t", 65), codes.InvalidArgument},
			"no workflow":      {asPerson(), "nope", "", "", codes.NotFound},
		} {
			t.Run(name, func(t *testing.T) {
				_, err := start(s, tc.ctx, tc.workflow, tc.input, tc.token)
				wantCode(t, err, tc.want)
			})
		}
	})
}

func TestFinishFoundryWorkflowRun(t *testing.T) {
	finish := func(s *Service, run *foundryv1.FoundryWorkflowRun, input, output string) (*foundryv1.FoundryWorkflowRunResponse, error) {
		return s.FinishFoundryWorkflowRun(asPerson(), &foundryv1.FinishFoundryWorkflowRunRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: run.GetWorkflowId(), Id: run.GetId(), Input: input, Output: output,
		})
	}

	t.Run("takes the input it wasn't started with, once", func(t *testing.T) {
		s, _ := workflowService(t)
		run := startRun(t, s, "")
		_, err := finish(s, run, "", "Acme renews.")
		wantCode(t, err, codes.InvalidArgument)
		done, err := finish(s, run, "Who renews?", "Acme renews.")
		if err != nil || done.GetRun().GetStatus() != "finished" || done.GetRun().GetInput() != "Who renews?" ||
			done.GetRun().GetOutput() != "Acme renews." || done.GetRun().GetFinishedAt() == nil {
			t.Fatalf("finished = %v, %v", done, err)
		}
		// A retry is answered; another answer is refused.
		for input, want := range map[string]codes.Code{"": codes.OK, "Who renews?": codes.OK, "Other?": codes.FailedPrecondition} {
			_, err := finish(s, run, input, "Acme renews.")
			wantCode(t, err, want)
		}
		_, err = finish(s, run, "", "Globex renews.")
		wantCode(t, err, codes.FailedPrecondition)
	})
	t.Run("keeps the input it was started with", func(t *testing.T) {
		s, _ := workflowService(t)
		run := startRun(t, s, "Who renews?")
		_, err := finish(s, run, "Something else?", "Acme.")
		wantCode(t, err, codes.InvalidArgument)
		done, err := finish(s, run, "", "Acme.")
		if err != nil || done.GetRun().GetInput() != "Who renews?" {
			t.Fatalf("finished = %v, %v", done, err)
		}
	})
	t.Run("answers a finish that raced another", func(t *testing.T) {
		s, store := workflowService(t)
		run := startRun(t, s, "q")
		store.raceFinish = "Acme."
		if _, err := finish(s, run, "", "Acme."); err != nil {
			t.Fatal(err)
		}
		store.runs[0].Finished = nil
		store.raceFinish = "Globex."
		_, err := finish(s, run, "", "Acme.")
		wantCode(t, err, codes.FailedPrecondition)
	})
	t.Run("refuses a bad output or run", func(t *testing.T) {
		s, _ := workflowService(t)
		run := startRun(t, s, "q")
		for output, want := range map[string]codes.Code{
			"":      codes.InvalidArgument,
			" \t ":  codes.InvalidArgument,
			"a\xff": codes.InvalidArgument,
			strings.Repeat("o", foundrymodel.FoundryWorkflowOutputMaxBytes+1): codes.InvalidArgument,
		} {
			_, err := finish(s, run, "", output)
			wantCode(t, err, want)
		}
		_, err := finish(s, &foundryv1.FoundryWorkflowRun{Id: run.GetId(), WorkflowId: "other"}, "", "a")
		wantCode(t, err, codes.NotFound)
		_, err = finish(s, run, "\xff", "a")
		wantCode(t, err, codes.InvalidArgument)
	})
}

func TestTrackedRun(t *testing.T) {
	s, store := workflowService(t)
	run := startRun(t, s, "q")
	ctx := asPerson()

	if r, err := s.trackedRun(ctx, "ws-1", "ont-1", ""); r != nil || err != nil {
		t.Fatalf("no run = %v, %v", r, err)
	}
	if r, err := s.trackedRun(ctx, "ws-1", "ont-1", run.GetId()); err != nil || r.ID != run.GetId() {
		t.Fatalf("run = %v, %v", r, err)
	}
	if r, err := s.trackedRun(ctx, "ws-1", "", run.GetId()); err != nil || r.ID != run.GetId() {
		t.Fatalf("workspace run = %v, %v", r, err)
	}
	for name, tc := range map[string]struct {
		ctx                context.Context
		workspace, onto, r string
		want               codes.Code
	}{
		"another ontology":                   {ctx, "ws-1", "ont-9", run.GetId(), codes.NotFound},
		"another workspace":                  {ctx, "ws-2", "", run.GetId(), codes.NotFound},
		"another workspace's ontology route": {ctx, "ws-2", "ont-1", run.GetId(), codes.NotFound},
		"unknown":                            {ctx, "ws-1", "ont-1", "nope", codes.NotFound},
		"a grantee":                          {access.WithCaller(ctx, access.Grantee{WorkspaceID: "ws-1", Principal: agentKey}), "ws-1", "ont-1", run.GetId(), codes.PermissionDenied},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.trackedRun(tc.ctx, tc.workspace, tc.onto, tc.r)
			wantCode(t, err, tc.want)
		})
	}
	store.runs[0].Counts.Accesses = foundrymodel.FoundryWorkflowRunMaxAccesses
	_, err := s.trackedRun(ctx, "ws-1", "ont-1", run.GetId())
	wantCode(t, err, codes.ResourceExhausted)
	store.runs[0].Finished = &foundrymodel.FoundryWorkflowRunEnd{Output: "a"}
	_, err = s.trackedRun(ctx, "ws-1", "ont-1", run.GetId())
	wantCode(t, err, codes.FailedPrecondition)

	// A call recorded under a run that changed meanwhile says it was made.
	r := &foundrymodel.FoundryWorkflowRun{ID: run.GetId()}
	for recordErr, want := range map[error]codes.Code{
		foundrystore.ErrNotFound:            codes.NotFound,
		foundrystore.ErrWorkflowRunFinished: codes.FailedPrecondition,
		foundrystore.ErrLimitExceeded:       codes.ResourceExhausted,
		errors.New("outage"):                codes.Internal,
	} {
		store.recordErr = recordErr
		err := s.track(ctx, r, &foundrymodel.FoundryWorkflowUpload{DocumentID: "d"})
		wantCode(t, err, want)
		if !strings.Contains(status.Convert(err).Message(), "the call was made") {
			t.Fatalf("message = %q", status.Convert(err).Message())
		}
	}
	if err := s.track(ctx, nil, &foundrymodel.FoundryWorkflowUpload{DocumentID: "d"}); err != nil {
		t.Fatal(err)
	}
}

// TestFoundryWorkflowTracking records each kind of call under a run as its
// handler makes it, and refuses a bad run before the call is made.
func TestFoundryWorkflowTracking(t *testing.T) {
	t.Run("a read with its result", func(t *testing.T) {
		s, foundry, _ := foundryService(t)
		def, _ := json.Marshal(map[string]any{"format": "agent_bundle", "status": "done", "ontology_toml": "version = \"v3\"\n", "mappings_toml": ""})
		foundry.versions = append(foundry.versions, &foundrymodel.OntologyVersion{
			ID: "v-3", WorkspaceID: "ws-1", OntologyID: "ont-1", Version: 3, Format: foundrymodel.OntologyFormatAgentBundle, Definition: def,
		})
		foundry.loads = append(foundry.loads,
			&foundrymodel.FoundryLoad{ID: "load-1", WorkspaceID: "ws-1", OntologyID: "ont-1", VersionID: "v-3", Version: 3, Status: foundrymodel.FoundryLoadReady})
		reader := &fakeFoundryRead{result: `{"results":[{"ok":{}}]}`}
		s.deps.FoundryRead = reader
		store := newWorkflowStore(foundry)
		s.deps.DB = store
		run := startRun(t, s, "q")
		const reads = `[{"entity":{"type":"Customer","id":"c-1"}}]`
		readWith := func(runID string) error {
			_, err := s.ReadFoundryGraph(asPerson(), &foundryv1.ReadFoundryGraphRequest{
				WorkspaceId: "ws-1", OntologyId: "ont-1", LoadId: "load-1", ReadsJson: reads, WorkflowRunId: runID,
			})
			return err
		}
		if err := readWith(run.GetId()); err != nil {
			t.Fatal(err)
		}
		got := store.accesses[run.GetId()]
		if len(got) != 1 {
			t.Fatalf("accesses = %v", got)
		}
		read := got[0].Call.(*foundrymodel.FoundryWorkflowRead)
		if read.LoadID != "load-1" || string(read.Reads) != reads || string(read.Result) != reader.result {
			t.Fatalf("read = %+v", read)
		}
		wantCode(t, readWith("nope"), codes.NotFound)
		if len(reader.requests) != 1 {
			t.Fatalf("a bad run still read: %d reads", len(reader.requests))
		}
		// A read the run can't take after the fact fails, though made.
		store.recordErr = foundrystore.ErrWorkflowRunFinished
		wantCode(t, readWith(run.GetId()), codes.FailedPrecondition)
	})

	t.Run("a question, once per client token", func(t *testing.T) {
		s, questions := questionService(t)
		store := newWorkflowStore(questions)
		s.deps.DB = store
		run := startRun(t, s, "q")
		var asked string
		for range 2 {
			resp, err := ask(s, "c1", "Which customers signed?", func(r *foundryv1.AskFoundryQuestionRequest) { r.WorkflowRunId = run.GetId() })
			if err != nil {
				t.Fatal(err)
			}
			asked = resp.GetQuestion().GetId()
		}
		got := store.accesses[run.GetId()]
		if len(got) != 1 || *got[0].Call.(*foundrymodel.FoundryWorkflowQuestion) != (foundrymodel.FoundryWorkflowQuestion{LoadID: "load-1", QuestionID: asked}) {
			t.Fatalf("accesses = %+v", got)
		}
		_, err := ask(s, "c2", "Who?", func(r *foundryv1.AskFoundryQuestionRequest) { r.WorkflowRunId = "nope" })
		wantCode(t, err, codes.NotFound)
		if len(questions.questions) != 1 {
			t.Fatalf("a bad run still asked: %d questions", len(questions.questions))
		}
	})

	t.Run("an upload, once however often completed", func(t *testing.T) {
		s, foundry, _ := foundryService(t)
		store := newWorkflowStore(foundry)
		s.deps.DB = store
		run := startRun(t, s, "q")
		writes := ports.WithActor(context.Background(), ports.Actor{
			Source: ports.SourceUser, UserID: "user-1", WorkspaceID: "ws-1", QueryWrite: ports.SelectedProjects([]string{"proj-1"}),
		})
		created, err := s.CreateFoundryOntologyDocument(writes, &foundryv1.CreateFoundryOntologyDocumentRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", Name: "contract.md", MediaType: "text/markdown", SizeBytes: 120,
			Sha256: strings.Repeat("a", 64), ClientToken: "up-1", SetId: "set-general",
		})
		if err != nil {
			t.Fatal(err)
		}
		doc := created.GetDocument().GetId()
		complete := func(runID string) error {
			_, err := s.CompleteFoundryOntologyDocumentUpload(writes, &foundryv1.CompleteFoundryOntologyDocumentUploadRequest{
				WorkspaceId: "ws-1", OntologyId: "ont-1", Id: doc, WorkflowRunId: runID,
			})
			return err
		}
		wantCode(t, complete("nope"), codes.NotFound)
		for range 2 {
			if err := complete(run.GetId()); err != nil {
				t.Fatal(err)
			}
		}
		got := store.accesses[run.GetId()]
		if len(got) != 1 || *got[0].Call.(*foundrymodel.FoundryWorkflowUpload) != (foundrymodel.FoundryWorkflowUpload{DocumentID: doc, SetID: "set-general"}) {
			t.Fatalf("accesses = %+v", got)
		}
	})

	t.Run("a send with its pieces' ids", func(t *testing.T) {
		h := newEventHarness(t, nil)
		store := newWorkflowStore(h.store)
		h.s.deps.DB = store
		w := &foundrymodel.FoundryWorkflow{ID: "wf-1", WorkspaceID: "ws-1", OntologyID: "ont-1", ClientToken: "w"}
		store.workflows = append(store.workflows, w)
		store.runs = append(store.runs, &foundrymodel.FoundryWorkflowRun{ID: "run-1", WorkflowID: "wf-1", WorkspaceID: "ws-1", OntologyID: "ont-1"})
		send := func(runID, token string) (*foundryv1.SendFoundryEventsResponse, error) {
			return h.s.SendFoundryEvents(asPerson(), &foundryv1.SendFoundryEventsRequest{
				WorkspaceId: "ws-1", ConnectorId: h.source, ClientToken: token, WorkflowRunId: runID,
				Pieces: []*foundryv1.FoundryEventPiece{{Id: "o-1", DataJson: `{"amount":1}`}, {Id: "o-2", DataJson: `{"amount":2}`}},
			})
		}
		for range 2 {
			if _, err := send("run-1", "0123456789abcdef-1"); err != nil {
				t.Fatal(err)
			}
		}
		got := store.accesses["run-1"]
		if len(got) != 1 {
			t.Fatalf("accesses = %+v", got)
		}
		sent := got[0].Call.(*foundrymodel.FoundryWorkflowSend)
		if sent.ConnectorID != h.source || sent.Seq != 1 || !slices.Equal(sent.PieceIDs, []string{"o-1", "o-2"}) {
			t.Fatalf("send = %+v", sent)
		}
		_, err := send("nope", "0123456789abcdef-2")
		wantCode(t, err, codes.NotFound)
	})
}
