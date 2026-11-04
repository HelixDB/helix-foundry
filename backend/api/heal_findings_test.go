package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// healStore holds one ontology's findings and runs.
type healStore struct {
	foundrystore.Unimplemented
	ontology *foundrymodel.FoundryOntology
	findings map[string]*foundrymodel.FoundryHealFinding
	// run and complete are its newest run and its newest complete one.
	run, complete *foundrymodel.FoundryHealRun
	// raced, when set, is the status another writer gave the finding just
	// before the dismissal's guarded write, which then finds it not open.
	raced foundrymodel.FoundryHealStatus
}

func (s *healStore) GetFoundryOntology(_ context.Context, ws, id string) (*foundrymodel.FoundryOntology, error) {
	if s.ontology.WorkspaceID != ws || s.ontology.ID != id {
		return nil, foundrystore.ErrNotFound
	}
	return s.ontology, nil
}
func (s *healStore) ListFoundryHealFindings(_ context.Context, o string) ([]*foundrymodel.FoundryHealFinding, error) {
	if o != s.ontology.ID {
		panic("wrong ontology")
	}
	var out []*foundrymodel.FoundryHealFinding
	for _, f := range s.findings {
		if f.Status == foundrymodel.FoundryHealOpen {
			out = append(out, f)
		}
	}
	return out, nil
}
func (s *healStore) GetFoundryHealFinding(_ context.Context, o, fp string) (*foundrymodel.FoundryHealFinding, error) {
	f, ok := s.findings[fp]
	if o != s.ontology.ID || !ok {
		return nil, foundrystore.ErrNotFound
	}
	copied := *f
	return &copied, nil
}
func (s *healStore) DismissFoundryHealFinding(_ context.Context, o, fp, user string) error {
	f := s.findings[fp]
	if s.raced != "" {
		f.Status = s.raced
		if s.raced == foundrymodel.FoundryHealDismissed {
			f.Dismissal = &foundrymodel.FoundryHealDismissal{ByUserID: "someone-else", Support: f.Support}
		}
		return foundrystore.ErrNotFound
	}
	if f.Status != foundrymodel.FoundryHealOpen {
		return foundrystore.ErrNotFound
	}
	f.Status, f.Dismissal = foundrymodel.FoundryHealDismissed, &foundrymodel.FoundryHealDismissal{ByUserID: user, Support: f.Support}
	return nil
}
func (s *healStore) GetLatestFoundryHealRun(context.Context, string) (*foundrymodel.FoundryHealRun, error) {
	if s.run == nil {
		return nil, foundrystore.ErrNotFound
	}
	return s.run, nil
}
func (s *healStore) GetLatestCompleteFoundryHealRun(context.Context, string) (*foundrymodel.FoundryHealRun, error) {
	if s.complete == nil {
		return nil, foundrystore.ErrNotFound
	}
	return s.complete, nil
}

func newHealService() (*Service, *healStore) {
	finding := json.RawMessage(`{"fingerprint":"fp1","signal":{"kind":"unmatched","canonical":"supplier_of","set":"set_1","backing":{"count":4,"sources":2,"examples":[]}},"proposal":null}`)
	db := &healStore{
		ontology: &foundrymodel.FoundryOntology{ID: "o1", WorkspaceID: "ws-1"},
		findings: map[string]*foundrymodel.FoundryHealFinding{
			"fp1": {OntologyID: "o1", Fingerprint: "fp1", Kind: "unmatched", Status: foundrymodel.FoundryHealOpen, LoadID: "l1", BaseVersionID: "v1",
				Finding: finding, Support: 4, Sources: 2},
			"fp2": {OntologyID: "o1", Fingerprint: "fp2", Kind: "unmatched", Status: foundrymodel.FoundryHealSuperseded, Finding: finding},
		},
	}
	return &Service{deps: &runtimeDeps{DB: db, Cfg: &config.Config{}}}, db
}

func TestListFoundryHealFindings(t *testing.T) {
	s, db := newHealService()
	ctx := ports.WithActor(context.Background(), reviewer)
	_, err := s.ListFoundryHealFindings(ctx, &foundryv1.ListFoundryHealFindingsRequest{WorkspaceId: "ws-2", OntologyId: "o1"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("another workspace's ontology: %v", err)
	}
	out, err := s.ListFoundryHealFindings(ctx, &foundryv1.ListFoundryHealFindingsRequest{WorkspaceId: "ws-1", OntologyId: "o1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.GetFindings()) != 1 || out.GetFindings()[0].GetFingerprint() != "fp1" || out.LastRun != nil || out.LastCompleteRun != nil || out.GetEnabled() {
		t.Fatalf("list = %v", out)
	}
	// It says whether this deployment audits loads at all.
	s.deps.Cfg.Foundry.Heal.Enabled = true
	if out, _ = s.ListFoundryHealFindings(ctx, &foundryv1.ListFoundryHealFindingsRequest{WorkspaceId: "ws-1", OntologyId: "o1"}); !out.GetEnabled() {
		t.Fatal("heal is on, the list says off")
	}
	got := out.GetFindings()[0]
	if got.GetKind() != "unmatched" || got.GetStatus() != "open" || got.GetSupport() != 4 || got.GetSources() != 2 || got.GetLoadId() != "l1" ||
		got.GetBaseVersionId() != "v1" || got.GetFindingJson() != string(db.findings["fp1"].Finding) || got.DismissedByUserId != nil {
		t.Fatalf("finding = %v", got)
	}

	finished := time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC)
	db.run = &foundrymodel.FoundryHealRun{ID: "r1", LoadID: "l1", Status: foundrymodel.FoundryHealRunIncomplete, Pages: 4, Cuts: 1,
		CreatedAt: finished.Add(-time.Minute), FinishedAt: &finished}
	out, err = s.ListFoundryHealFindings(ctx, &foundryv1.ListFoundryHealFindingsRequest{WorkspaceId: "ws-1", OntologyId: "o1"})
	if err != nil {
		t.Fatal(err)
	}
	run := out.GetLastRun()
	if run.GetId() != "r1" || run.GetStatus() != "incomplete" || run.GetPages() != 4 || run.GetCuts() != 1 ||
		!run.GetFinishedAt().AsTime().Equal(finished) || !run.GetStartedAt().AsTime().Equal(finished.Add(-time.Minute)) {
		t.Fatalf("last run = %v", run)
	}
	if run.GetRowsMeasured() {
		t.Fatal("an incomplete run measured the rows")
	}
	if out.LastCompleteRun != nil {
		t.Fatalf("no run is complete, the list says %v", out.LastCompleteRun)
	}
	// The newest complete run comes too, of whichever load.
	db.complete = &foundrymodel.FoundryHealRun{ID: "r0", LoadID: "l0", Status: foundrymodel.FoundryHealRunComplete, Pages: 2,
		RowsMeasured: true, CreatedAt: finished.Add(-time.Hour), FinishedAt: &finished}
	out, _ = s.ListFoundryHealFindings(ctx, &foundryv1.ListFoundryHealFindingsRequest{WorkspaceId: "ws-1", OntologyId: "o1"})
	if c := out.GetLastCompleteRun(); c.GetId() != "r0" || c.GetLoadId() != "l0" || c.GetStatus() != "complete" || !c.GetRowsMeasured() ||
		out.GetLastRun().GetId() != "r1" {
		t.Fatalf("last complete run = %v, last run = %v", c, out.GetLastRun())
	}
	db.run.FinishedAt = nil
	out, _ = s.ListFoundryHealFindings(ctx, &foundryv1.ListFoundryHealFindingsRequest{WorkspaceId: "ws-1", OntologyId: "o1"})
	if out.GetLastRun().FinishedAt != nil {
		t.Fatal("a running run has an end")
	}
	// A complete run says whether it measured the rows.
	db.run.Status, db.run.FinishedAt, db.run.RowsMeasured = foundrymodel.FoundryHealRunComplete, &finished, true
	if out, _ = s.ListFoundryHealFindings(ctx, &foundryv1.ListFoundryHealFindingsRequest{WorkspaceId: "ws-1", OntologyId: "o1"}); !out.GetLastRun().GetRowsMeasured() {
		t.Fatalf("a run that measured the rows = %v", out.GetLastRun())
	}
}

func TestDismissFoundryHealFinding(t *testing.T) {
	s, db := newHealService()
	ctx := ports.WithActor(context.Background(), reviewer)
	req := func(fp string) *foundryv1.DismissFoundryHealFindingRequest {
		return &foundryv1.DismissFoundryHealFindingRequest{WorkspaceId: "ws-1", OntologyId: "o1", Fingerprint: fp}
	}
	for _, c := range []struct {
		name string
		ctx  context.Context
		req  *foundryv1.DismissFoundryHealFindingRequest
		code codes.Code
	}{
		{"no fingerprint", ctx, req(""), codes.InvalidArgument},
		{"no user", ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceUser}), req("fp1"), codes.Unauthenticated},
		{"another workspace", ctx, &foundryv1.DismissFoundryHealFindingRequest{WorkspaceId: "ws-2", OntologyId: "o1", Fingerprint: "fp1"}, codes.NotFound},
		{"no such finding", ctx, req("fp9"), codes.NotFound},
		{"not open", ctx, req("fp2"), codes.FailedPrecondition},
	} {
		if _, err := s.DismissFoundryHealFinding(c.ctx, c.req); status.Code(err) != c.code {
			t.Fatalf("%s: %v, want %s", c.name, err, c.code)
		}
	}
	// Superseded by a run between the read and the write: not open.
	db.raced = foundrymodel.FoundryHealSuperseded
	if _, err := s.DismissFoundryHealFinding(ctx, req("fp1")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("a finding superseded meanwhile: %v", err)
	}
	// Dismissed by someone else meanwhile: theirs stands, and it's no error.
	db.findings["fp1"].Status, db.raced = foundrymodel.FoundryHealOpen, foundrymodel.FoundryHealDismissed
	if out, err := s.DismissFoundryHealFinding(ctx, req("fp1")); err != nil || out.GetFinding().GetDismissedByUserId() != "someone-else" {
		t.Fatalf("a finding dismissed meanwhile = %v, %v", out, err)
	}
	db.findings["fp1"].Status, db.findings["fp1"].Dismissal, db.raced = foundrymodel.FoundryHealOpen, nil, ""
	out, err := s.DismissFoundryHealFinding(ctx, req("fp1"))
	if err != nil {
		t.Fatal(err)
	}
	got := out.GetFinding()
	if got.GetStatus() != "dismissed" || got.GetDismissedByUserId() != reviewer.UserID || got.GetSupportAtDismissal() != 4 {
		t.Fatalf("dismissed = %v", got)
	}
	// Dismissing it again is idempotent: the same dismissal, no error.
	again, err := s.DismissFoundryHealFinding(ctx, req("fp1"))
	if err != nil || again.GetFinding().GetDismissedByUserId() != reviewer.UserID || again.GetFinding().GetSupportAtDismissal() != 4 {
		t.Fatalf("dismissed again = %v, %v", again, err)
	}
}
