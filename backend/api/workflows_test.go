package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

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
	// assessments are by run; queueErr fails the next queue.
	assessments map[string]*foundrymodel.FoundryWorkflowAssessment
	queueErr    error
}

func newWorkflowStore(inner foundrystore.Store) *workflowStore {
	return &workflowStore{Store: inner, accesses: map[string][]*foundrymodel.FoundryWorkflowAccess{},
		assessments: map[string]*foundrymodel.FoundryWorkflowAssessment{}}
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

func (s *workflowStore) ListFoundryWorkflowRuns(_ context.Context, workflowID string, startedBy *access.Principal, pageToken string, _ int) ([]*foundrymodel.FoundryWorkflowRun, string, error) {
	if pageToken == "bad" {
		return nil, "", foundrystore.ErrInvalidPageToken
	}
	var out []*foundrymodel.FoundryWorkflowRun
	for _, r := range s.runs {
		if r.WorkflowID == workflowID && (startedBy == nil || r.StartedBy == *startedBy) {
			out = append(out, r)
		}
	}
	return out, "", nil
}

func (s *workflowStore) ListFoundryWorkflowInsightRuns(_ context.Context, ontologyID, workflowID string, startedBy *access.Principal,
	from, to time.Time, limit int) ([]foundrymodel.FoundryWorkflowInsightRun, error) {
	var out []foundrymodel.FoundryWorkflowInsightRun
	for _, r := range s.runs {
		if r.OntologyID != ontologyID || workflowID != "" && r.WorkflowID != workflowID || startedBy != nil && r.StartedBy != *startedBy ||
			r.StartedAt.Before(from) || !r.StartedAt.Before(to) {
			continue
		}
		run := foundrymodel.FoundryWorkflowInsightRun{ID: r.ID, WorkflowID: r.WorkflowID, StartedBy: r.StartedBy, StartedAt: r.StartedAt,
			Status: r.Status(), Counts: r.Counts}
		if a := s.assessments[r.ID]; a != nil {
			run.Assessment = &foundrymodel.FoundryWorkflowInsightAssessment{Status: a.Status, Verdict: a.Verdict, Assessment: a.Assessment,
				USDMicros: a.USDMicros}
		}
		out = append(out, run)
	}
	slices.SortFunc(out, func(a, b foundrymodel.FoundryWorkflowInsightRun) int { return b.StartedAt.Compare(a.StartedAt) })
	return out[:min(len(out), limit)], nil
}

func (s *workflowStore) TallyFoundryWorkflowRuns(_ context.Context, workflowIDs []string, by access.Principal) (map[string]foundrymodel.FoundryWorkflowRunTally, error) {
	out := map[string]foundrymodel.FoundryWorkflowRunTally{}
	for _, w := range workflowIDs {
		t := foundrymodel.FoundryWorkflowRunTally{Verdicts: map[string]int64{}}
		for _, r := range s.runs {
			if r.WorkflowID != w || r.StartedBy != by {
				continue
			}
			t.Runs++
			if r.Finished != nil {
				t.FinishedRuns++
			}
			if a := s.assessments[r.ID]; a != nil && a.Status == foundrymodel.FoundryWorkflowAssessmentDone {
				t.Verdicts[a.Verdict]++
			}
		}
		out[w] = t
	}
	return out, nil
}

func (s *workflowStore) FinishFoundryWorkflowRun(_ context.Context, id string, input *string, output string, assess bool) error {
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
	if assess {
		s.assessments[id] = &foundrymodel.FoundryWorkflowAssessment{RunID: id, Status: foundrymodel.FoundryWorkflowAssessmentQueued, QueuedAt: time.Now()}
	}
	return nil
}

func (s *workflowStore) GetFoundryWorkflowRunAssessment(_ context.Context, runID string) (*foundrymodel.FoundryWorkflowAssessment, error) {
	if a, ok := s.assessments[runID]; ok {
		return a, nil
	}
	return nil, foundrystore.ErrNotFound
}

func (s *workflowStore) ListFoundryWorkflowRunAssessments(_ context.Context, runIDs []string) (map[string]*foundrymodel.FoundryWorkflowAssessment, error) {
	out := map[string]*foundrymodel.FoundryWorkflowAssessment{}
	for _, id := range runIDs {
		if a, ok := s.assessments[id]; ok {
			out[id] = a
		}
	}
	return out, nil
}

func (s *workflowStore) CountFoundryWorkflowVerdicts(_ context.Context, workflowIDs []string) (map[string]map[string]int64, error) {
	out := map[string]map[string]int64{}
	for _, r := range s.runs {
		a := s.assessments[r.ID]
		if a == nil || a.Status != foundrymodel.FoundryWorkflowAssessmentDone || !slices.Contains(workflowIDs, r.WorkflowID) {
			continue
		}
		if out[r.WorkflowID] == nil {
			out[r.WorkflowID] = map[string]int64{}
		}
		out[r.WorkflowID][a.Verdict]++
	}
	return out, nil
}

func (s *workflowStore) QueueFoundryWorkflowRunAssessment(_ context.Context, runID string) error {
	if err := s.queueErr; err != nil {
		s.queueErr = nil
		return err
	}
	s.assessments[runID] = &foundrymodel.FoundryWorkflowAssessment{RunID: runID, Status: foundrymodel.FoundryWorkflowAssessmentQueued}
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

	// A call recorded under a run that changed meanwhile, or by a store that
	// failed, is done all the same: nothing is the call's error.
	r := &foundrymodel.FoundryWorkflowRun{ID: run.GetId()}
	for _, recordErr := range []error{foundrystore.ErrNotFound, foundrystore.ErrWorkflowRunFinished, foundrystore.ErrLimitExceeded, errors.New("outage")} {
		store.recordErr = recordErr
		s.track(ctx, r, &foundrymodel.FoundryWorkflowUpload{DocumentID: "d"})
	}
	s.track(ctx, nil, &foundrymodel.FoundryWorkflowUpload{DocumentID: "d"})
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
		// A read the run can't take after the fact is made all the same.
		store.recordErr = foundrystore.ErrWorkflowRunFinished
		if err := readWith(run.GetId()); err != nil {
			t.Fatal(err)
		}
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

func TestFoundryWorkflowAssessments(t *testing.T) {
	finish := func(s *Service, run *foundryv1.FoundryWorkflowRun) *foundryv1.FoundryWorkflowRun {
		t.Helper()
		done, err := s.FinishFoundryWorkflowRun(asPerson(), &foundryv1.FinishFoundryWorkflowRunRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: run.GetWorkflowId(), Id: run.GetId(), Output: "Acme.",
		})
		if err != nil {
			t.Fatal(err)
		}
		return done.GetRun()
	}
	assess := func(s *Service, run *foundryv1.FoundryWorkflowRun) (*foundryv1.FoundryWorkflowRunResponse, error) {
		return s.AssessFoundryWorkflowRun(asPerson(), &foundryv1.AssessFoundryWorkflowRunRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: run.GetWorkflowId(), Id: run.GetId(),
		})
	}

	t.Run("finishing a run queues its assessment, where they are on", func(t *testing.T) {
		s, store := workflowService(t)
		run := finish(s, startRun(t, s, "q"))
		if run.GetAssessment().GetStatus() != "queued" {
			t.Fatalf("assessment = %+v", run.GetAssessment())
		}
		// Done, it reads back with its verdict, and the workflow counts it.
		now := time.Now()
		store.assessments[run.GetId()] = &foundrymodel.FoundryWorkflowAssessment{
			RunID: run.GetId(), Status: foundrymodel.FoundryWorkflowAssessmentDone, Verdict: "helpful",
			Assessment: []byte(`{"verdict":"helpful"}`), LeftOut: []int32{2}, USDMicros: 4200, QueuedAt: now, FinishedAt: &now,
		}
		got, err := s.GetFoundryWorkflowRun(asPerson(), &foundryv1.GetFoundryWorkflowRunRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: run.GetWorkflowId(), Id: run.GetId(),
		})
		if a := got.GetRun().GetAssessment(); err != nil || a.GetVerdict() != "helpful" || a.GetAssessmentJson() != `{"verdict":"helpful"}` ||
			!slices.Equal(a.GetLeftOut(), []int32{2}) || a.GetUsdMicros() != 4200 || a.GetFinishedAt() == nil {
			t.Fatalf("run = %+v, %v", got, err)
		}
		runs, _ := s.ListFoundryWorkflowRuns(asPerson(), &foundryv1.ListFoundryWorkflowRunsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: run.GetWorkflowId()})
		if runs.GetRuns()[0].GetAssessment().GetVerdict() != "helpful" {
			t.Fatalf("runs = %+v", runs)
		}
		for _, w := range []*foundryv1.FoundryWorkflow{
			must(s.GetFoundryWorkflow(asPerson(), &foundryv1.GetFoundryWorkflowRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: run.GetWorkflowId()})).GetWorkflow(),
			must(s.ListFoundryWorkflows(asPerson(), &foundryv1.ListFoundryWorkflowsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})).GetWorkflows()[0],
		} {
			if len(w.GetVerdicts()) != 1 || w.GetVerdicts()[0].GetVerdict() != "helpful" || w.GetVerdicts()[0].GetRuns() != 1 {
				t.Fatalf("verdicts = %+v", w.GetVerdicts())
			}
		}
		// It can be asked for again.
		again, err := assess(s, run)
		if err != nil || again.GetRun().GetAssessment().GetStatus() != "queued" {
			t.Fatalf("again = %v, %v", again, err)
		}
		for queueErr, want := range map[error]codes.Code{
			foundrystore.ErrNotFound: codes.FailedPrecondition, foundrystore.ErrConflict: codes.FailedPrecondition, errors.New("outage"): codes.Internal,
		} {
			store.queueErr = queueErr
			_, err := assess(s, run)
			wantCode(t, err, want)
		}
		_, err = assess(s, &foundryv1.FoundryWorkflowRun{Id: run.GetId(), WorkflowId: "other"})
		wantCode(t, err, codes.NotFound)
	})

	t.Run("where they are off, none is queued or asked for", func(t *testing.T) {
		s, store := workflowService(t)
		s.deps.Cfg.Foundry.Workflows.Assess.Enabled = false
		run := finish(s, startRun(t, s, "q"))
		if run.GetAssessment() != nil || len(store.assessments) != 0 {
			t.Fatalf("assessment = %+v", run.GetAssessment())
		}
		_, err := assess(s, run)
		wantCode(t, err, codes.FailedPrecondition)
	})
}

// must is v, or the test's end.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestFoundryWorkflowListsStayInsideAResponse(t *testing.T) {
	if got := previewText(strings.Repeat("é", 500)); got != strings.Repeat("é", 500) {
		t.Fatalf("a 500-character text is cut: %d", utf8.RuneCountInString(got))
	}
	if got := previewText(strings.Repeat("é", 501)); utf8.RuneCountInString(got) != 500 || !strings.HasSuffix(got, "…") {
		t.Fatalf("preview = %d characters", utf8.RuneCountInString(got))
	}
	s, store := workflowService(t)
	run := startRun(t, s, strings.Repeat("q", 2000))
	runs := must(s.ListFoundryWorkflowRuns(asPerson(), &foundryv1.ListFoundryWorkflowRunsRequest{
		WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: run.GetWorkflowId(),
	}))
	if got := runs.GetRuns()[0].GetInput(); utf8.RuneCountInString(got) != 500 {
		t.Fatalf("a list's input is %d characters", utf8.RuneCountInString(got))
	}
	whole := must(s.GetFoundryWorkflowRun(asPerson(), &foundryv1.GetFoundryWorkflowRunRequest{
		WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: run.GetWorkflowId(), Id: run.GetId(),
	}))
	if len(whole.GetRun().GetInput()) != 2000 {
		t.Fatalf("one run's input is %d bytes", len(whole.GetRun().GetInput()))
	}

	// Results of 1.5 MiB: a page ends once it passes 2 MiB, keeping at least one.
	big := json.RawMessage(`"` + strings.Repeat("x", 3<<19) + `"`)
	for range 3 {
		if _, err := store.RecordFoundryWorkflowAccess(context.Background(), run.GetId(), &foundrymodel.FoundryWorkflowRead{LoadID: "l", Reads: json.RawMessage(`[]`), Result: big}); err != nil {
			t.Fatal(err)
		}
	}
	page := func(after int32) *foundryv1.ListFoundryWorkflowRunAccessesResponse {
		return must(s.ListFoundryWorkflowRunAccesses(asPerson(), &foundryv1.ListFoundryWorkflowRunAccessesRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: run.GetWorkflowId(), Id: run.GetId(), AfterSeq: after,
		}))
	}
	for after, want := range map[int32]struct {
		seqs []int32
		more bool
	}{0: {[]int32{1}, true}, 1: {[]int32{2}, true}, 2: {[]int32{3}, false}} {
		got := page(after)
		var seqs []int32
		for _, a := range got.GetAccesses() {
			seqs = append(seqs, a.GetSeq())
		}
		if !slices.Equal(seqs, want.seqs) || got.GetMore() != want.more {
			t.Fatalf("after %d: %v, more %v", after, seqs, got.GetMore())
		}
	}
}

// TestGranteeWorkflows: with the Workflows permission, a grantee lists an
// ontology's workflows and starts, records under and finishes its own runs.
// It reads every run of an ontology it holds all of (the agent's key holds
// all of Billing), and only its own of one it holds part of (user-9),
// counts and verdicts included. Off, for it or a team of its, every
// workflow call is refused, as is recording under a run; the host's callers
// never read it.
func TestGranteeWorkflows(t *testing.T) {
	s, acc := accessService(t)
	store := newWorkflowStore(acc)
	s.deps.DB = store
	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)
	owners := startRun(t, s, "the owner's")
	wf := owners.GetWorkflowId()
	start := func(who ports.Actor, input string) *foundryv1.FoundryWorkflowRun {
		t.Helper()
		r, err := client.StartFoundryWorkflowRun(as(who), &foundryv1.StartFoundryWorkflowRunRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: wf, Input: input,
		})
		if err != nil {
			t.Fatal(err)
		}
		return r.GetRun()
	}
	agents, members := start(agent, "the agent's"), start(member, "the member's")
	if agents.GetStartedByServiceCredentialId() != "cred-1" || members.GetStartedByUserId() != "user-9" {
		t.Fatalf("started by %v and %v", agents, members)
	}
	for id, verdict := range map[string]string{owners.GetId(): "helpful", members.GetId(): "unused"} {
		store.assessments[id] = &foundrymodel.FoundryWorkflowAssessment{RunID: id, WorkflowID: wf, Status: foundrymodel.FoundryWorkflowAssessmentDone, Verdict: verdict}
	}
	ids := func(who ports.Actor) []string {
		t.Helper()
		runs, err := client.ListFoundryWorkflowRuns(as(who), &foundryv1.ListFoundryWorkflowRunsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: wf})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range runs.GetRuns() {
			out = append(out, r.GetId())
		}
		return slices.Sorted(slices.Values(out))
	}
	if got, want := ids(agent), slices.Sorted(slices.Values([]string{owners.GetId(), agents.GetId(), members.GetId()})); !slices.Equal(got, want) {
		t.Fatalf("the agent's key lists %v, want every run %v", got, want)
	}
	if got := ids(member); !slices.Equal(got, []string{members.GetId()}) {
		t.Fatalf("user-9 lists %v, want its own run", got)
	}
	verdicts := func(w *foundryv1.FoundryWorkflow) map[string]int64 {
		out := map[string]int64{}
		for _, v := range w.GetVerdicts() {
			out[v.GetVerdict()] = v.GetRuns()
		}
		return out
	}
	whole, err := client.GetFoundryWorkflow(as(agent), &foundryv1.GetFoundryWorkflowRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: wf})
	if err != nil || whole.GetWorkflow().GetRuns() != 3 || len(verdicts(whole.GetWorkflow())) != 2 {
		t.Fatalf("the agent's workflow = %v, %v", whole, err)
	}
	part, err := client.GetFoundryWorkflow(as(member), &foundryv1.GetFoundryWorkflowRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: wf})
	if err != nil || part.GetWorkflow().GetRuns() != 1 || part.GetWorkflow().GetFinishedRuns() != 0 ||
		!maps.Equal(verdicts(part.GetWorkflow()), map[string]int64{"unused": 1}) {
		t.Fatalf("user-9's workflow = %v, %v", part, err)
	}
	listed, err := client.ListFoundryWorkflows(as(member), &foundryv1.ListFoundryWorkflowsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
	if err != nil || len(listed.GetWorkflows()) != 1 || listed.GetWorkflows()[0].GetRuns() != 1 {
		t.Fatalf("user-9's workflows = %v, %v", listed, err)
	}

	// Another's run is not found for user-9, whatever it asks of it.
	_, err = client.GetFoundryWorkflowRun(as(member), &foundryv1.GetFoundryWorkflowRunRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: wf, Id: owners.GetId()})
	wantCode(t, err, codes.NotFound)
	_, err = client.ListFoundryWorkflowRunAccesses(as(member), &foundryv1.ListFoundryWorkflowRunAccessesRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: wf, Id: agents.GetId()})
	wantCode(t, err, codes.NotFound)
	_, err = client.FinishFoundryWorkflowRun(as(member), &foundryv1.FinishFoundryWorkflowRunRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: wf, Id: owners.GetId(), Output: "x"})
	wantCode(t, err, codes.NotFound)
	if _, err := client.GetFoundryWorkflowRun(as(agent), &foundryv1.GetFoundryWorkflowRunRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: wf, Id: members.GetId()}); err != nil {
		t.Fatalf("the agent's key reads user-9's run: %v", err)
	}
	// Making, deleting and assessing them stay with owners and admins.
	_, err = client.CreateFoundryWorkflow(as(agent), &foundryv1.CreateFoundryWorkflowRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Name: "Mine"})
	wantCode(t, err, codes.PermissionDenied)

	// Recording: user-9 only under its own run, the agent's key under any.
	user9 := access.Principal{Kind: access.PrincipalUser, ID: "user-9"}
	partly := context.WithValue(access.WithCaller(asPerson(), access.Grantee{WorkspaceID: "ws-1", Principal: user9}), granteeLoadKey{},
		foundrymodel.FoundryLoadInputs{LoadID: "load-1", Partial: true})
	wholly := context.WithValue(access.WithCaller(asPerson(), access.Grantee{WorkspaceID: "ws-1", Principal: agentKey}), granteeLoadKey{},
		foundrymodel.FoundryLoadInputs{LoadID: "load-1"})
	for _, run := range []string{owners.GetId(), agents.GetId()} {
		_, err := s.trackedRun(partly, "ws-1", "ont-1", run)
		wantCode(t, err, codes.NotFound)
	}
	if r, err := s.trackedRun(partly, "ws-1", "ont-1", members.GetId()); err != nil || r.ID != members.GetId() {
		t.Fatalf("user-9's own run = %v, %v", r, err)
	}
	if r, err := s.trackedRun(wholly, "ws-1", "ont-1", owners.GetId()); err != nil || r.ID != owners.GetId() {
		t.Fatalf("the owner's run for the agent's key = %v, %v", r, err)
	}
	finished, err := client.FinishFoundryWorkflowRun(as(member), &foundryv1.FinishFoundryWorkflowRunRequest{
		WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: wf, Id: members.GetId(), Output: "done",
	})
	if err != nil || finished.GetRun().GetStatus() != string(foundrymodel.FoundryWorkflowRunFinished) {
		t.Fatalf("user-9 finishing its run = %v, %v", finished, err)
	}

	// Off for the agent's key: refused, saying where to turn it on, and the
	// workspace's list names it. Its grants stay as they were.
	held := []*foundryv1.FoundryAccessTarget{
		{Kind: foundryv1.FoundryAccessTarget_KIND_CONNECTOR, ConnectorId: "conn-1"},
		{Kind: foundryv1.FoundryAccessTarget_KIND_DOCUMENT_SET, DocumentSetId: "set-general"},
	}
	set, err := s.SetFoundryAccessGrants(asPerson(), &foundryv1.SetFoundryAccessGrantsRequest{
		WorkspaceId: "ws-1", Principal: principalToProto(agentKey), Targets: held, Workflows: proto.Bool(false),
	})
	if err != nil || set.GetWorkflows() || len(set.GetGrants()) != 2 {
		t.Fatalf("turning workflows off = %v, %v", set, err)
	}
	_, err = client.ListFoundryWorkflows(as(agent), &foundryv1.ListFoundryWorkflowsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
	wantCode(t, err, codes.PermissionDenied)
	if !strings.Contains(status.Convert(err).Message(), "Settings › Access") {
		t.Fatalf("refusal %q doesn't say where to turn it on", status.Convert(err).Message())
	}
	_, err = s.trackedRun(wholly, "ws-1", "ont-1", owners.GetId())
	wantCode(t, err, codes.PermissionDenied)
	mine, err := client.GetMyFoundryAccess(as(agent), &foundryv1.GetMyFoundryAccessRequest{WorkspaceId: "ws-1"})
	if err != nil || mine.GetWorkflows() {
		t.Fatalf("the agent's key's access = %v, %v", mine, err)
	}
	all, err := s.ListFoundryAccess(asPerson(), &foundryv1.ListFoundryAccessRequest{WorkspaceId: "ws-1"})
	if err != nil || len(all.GetWorkflowsOff()) != 1 || all.GetWorkflowsOff()[0].GetId() != "cred-1" {
		t.Fatalf("listed access = %v, %v", all.GetWorkflowsOff(), err)
	}
	// Left out of a save, it stays off; turned on again, it is.
	if set, err = s.SetFoundryAccessGrants(asPerson(), &foundryv1.SetFoundryAccessGrantsRequest{
		WorkspaceId: "ws-1", Principal: principalToProto(agentKey), Targets: held,
	}); err != nil || set.GetWorkflows() {
		t.Fatalf("a save leaving it out = %v, %v", set, err)
	}
	if set, err = s.SetFoundryAccessGrants(asPerson(), &foundryv1.SetFoundryAccessGrantsRequest{
		WorkspaceId: "ws-1", Principal: principalToProto(agentKey), Targets: held, Workflows: proto.Bool(true),
	}); err != nil || !set.GetWorkflows() {
		t.Fatalf("turning workflows on = %v, %v", set, err)
	}
	if _, err := client.ListFoundryWorkflows(as(agent), &foundryv1.ListFoundryWorkflowsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}); err != nil {
		t.Fatalf("on again: %v", err)
	}

	// Off for a team of user-9's turns it off for them too; the host's caller,
	// for whom it was turned off as well, is served as before.
	acc.teams = []foundrymodel.FoundryTeam{{ID: "team-1", WorkspaceID: "ws-1", Name: "Support", MemberUserIDs: []string{"user-9"}}}
	acc.workflowsOff = []access.Principal{{Kind: access.PrincipalTeam, ID: "team-1"}, {Kind: access.PrincipalUser, ID: person.UserID}}
	_, err = client.ListFoundryWorkflowRuns(as(member), &foundryv1.ListFoundryWorkflowRunsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: wf})
	wantCode(t, err, codes.PermissionDenied)
	if mine, err := client.GetMyFoundryAccess(as(member), &foundryv1.GetMyFoundryAccessRequest{WorkspaceId: "ws-1"}); err != nil || mine.GetWorkflows() {
		t.Fatalf("user-9's access = %v, %v", mine, err)
	}
	if runs, err := s.ListFoundryWorkflowRuns(asPerson(), &foundryv1.ListFoundryWorkflowRunsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: wf}); err != nil || len(runs.GetRuns()) != 3 {
		t.Fatalf("the host's caller lists %v, %v", runs, err)
	}
	if mine, err := s.GetMyFoundryAccess(asPerson(), &foundryv1.GetMyFoundryAccessRequest{WorkspaceId: "ws-1"}); err != nil || !mine.GetWorkflows() {
		t.Fatalf("the host's caller's access = %v, %v", mine, err)
	}
}
