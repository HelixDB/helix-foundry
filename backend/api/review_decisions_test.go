package api

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
)

type decisionStore struct {
	foundrystore.Unimplemented
	ontology *foundrymodel.FoundryOntology
	load     *foundrymodel.FoundryLoad
	version  *foundrymodel.OntologyVersion
	rows     []*foundrymodel.FoundryReviewDecision
	saved    []*foundrymodel.FoundryReviewDecision
	saveErr  error
	deleted  string
	// releasedTo is the load SettleFoundryReleases or SettleFoundryValue
	// applied its decisions to, and resends the resend jobs they and
	// DeleteFoundryReviewDecision were handed; rejectedOn is the load
	// SettleFoundryReject read its review from; applied is what
	// ListFoundryDecisionsApplied answers.
	releasedTo string
	rejectedOn string
	resends    []*foundrymodel.FoundryGraphJob
	applied    map[string]foundrymodel.FoundryDecisionApplied
}

func (s *decisionStore) GetFoundryOntology(_ context.Context, ws, id string) (*foundrymodel.FoundryOntology, error) {
	if s.ontology.WorkspaceID != ws || s.ontology.ID != id {
		return nil, foundrystore.ErrNotFound
	}
	return s.ontology, nil
}
func (s *decisionStore) GetFoundryLoad(_ context.Context, o, id string) (*foundrymodel.FoundryLoad, error) {
	if s.load == nil || o != s.ontology.ID || id != s.load.ID {
		return nil, foundrystore.ErrNotFound
	}
	return s.load, nil
}
func (s *decisionStore) GetOntologyVersion(_ context.Context, o, id string) (*foundrymodel.OntologyVersion, error) {
	if o != s.ontology.ID || id != s.version.ID {
		return nil, foundrystore.ErrNotFound
	}
	return s.version, nil
}
func (s *decisionStore) SettleFoundryReviewDecision(_ context.Context, ws string, d *foundrymodel.FoundryReviewDecision) error {
	if ws != s.ontology.WorkspaceID {
		return foundrystore.ErrNotFound
	}
	if s.saveErr != nil {
		return s.saveErr
	}
	d.DecidedAt = time.Now()
	s.saved = append(s.saved, d)
	return nil
}
func (s *decisionStore) SettleFoundryReleases(_ context.Context, ws, load string, ds []*foundrymodel.FoundryReviewDecision,
	resend *foundrymodel.FoundryGraphJob) (bool, error) {
	if ws != s.ontology.WorkspaceID {
		return false, foundrystore.ErrNotFound
	}
	if s.saveErr != nil {
		return false, s.saveErr
	}
	for _, d := range ds {
		d.DecidedAt = time.Now()
		d.Applied = &foundrymodel.FoundryDecisionApplied{Status: foundrymodel.FoundryReleasePending}
	}
	s.saved, s.releasedTo, s.resends = append(s.saved, ds...), load, append(s.resends, resend)
	return true, nil
}
func (s *decisionStore) SettleFoundryValue(_ context.Context, ws, load string, d *foundrymodel.FoundryReviewDecision,
	resend *foundrymodel.FoundryGraphJob) (bool, error) {
	if ws != s.ontology.WorkspaceID {
		return false, foundrystore.ErrNotFound
	}
	if s.saveErr != nil {
		return false, s.saveErr
	}
	d.DecidedAt = time.Now()
	d.Applied = &foundrymodel.FoundryDecisionApplied{Status: foundrymodel.FoundryReleasePending}
	s.saved, s.releasedTo, s.resends = append(s.saved, d), load, append(s.resends, resend)
	return true, nil
}
func (s *decisionStore) SettleFoundryReject(_ context.Context, ws, load string, d *foundrymodel.FoundryReviewDecision) error {
	if ws != s.ontology.WorkspaceID {
		return foundrystore.ErrNotFound
	}
	if s.saveErr != nil {
		return s.saveErr
	}
	d.DecidedAt = time.Now()
	s.saved, s.rejectedOn = append(s.saved, d), load
	return nil
}
func (s *decisionStore) ListFoundryDecisionsApplied(_ context.Context, o string, ids []string) (map[string]foundrymodel.FoundryDecisionApplied, error) {
	if o != s.ontology.ID {
		panic("wrong ontology")
	}
	return s.applied, nil
}
func (s *decisionStore) ListFoundryReviewDecisions(_ context.Context, o, after string, docs []string, limit int) ([]*foundrymodel.FoundryReviewDecision, error) {
	if o != s.ontology.ID {
		panic("wrong ontology")
	}
	return s.rows, nil
}
func (s *decisionStore) DeleteFoundryReviewDecision(_ context.Context, ws, o, id string, resend *foundrymodel.FoundryGraphJob) error {
	if ws != s.ontology.WorkspaceID || o != s.ontology.ID {
		panic("wrong ontology")
	}
	if id == "missing" {
		return foundrystore.ErrNotFound
	}
	s.deleted, s.resends = id, append(s.resends, resend)
	return nil
}

// graphFunc is a ports.GraphReader of one function.
type graphFunc func(ctx context.Context, tenantID string, query []byte) (int, []byte, error)

func (f graphFunc) ReadQuery(ctx context.Context, tenantID string, query []byte) (int, []byte, error) {
	return f(ctx, tenantID, query)
}

// reviewer is user-a, who settles the reviews of workspace-a.
var reviewer = ports.Actor{Source: ports.SourceUser, UserID: "user-a"}

// reviewService has workspace-a's ontology o, loaded into tenant-a, whose
// graph answers review to the first read and finds customer-1 after.
func reviewService(t *testing.T, review string) (*Service, *decisionStore, *[]string) {
	t.Helper()
	queries := []string{}
	current := "load-1"
	def, _ := json.Marshal(ontologyagent.Definition{OntologyTOML: "[types.Customer]\nkey = [\"id\"]\nprops = { name = \"string\", amount = \"float\" }"})
	db := &decisionStore{ontology: &foundrymodel.FoundryOntology{ID: "o", WorkspaceID: "workspace-a", TenantID: "tenant-a", CurrentLoadID: &current}, load: &foundrymodel.FoundryLoad{ID: current, VersionID: "v", Status: foundrymodel.FoundryLoadReady}, version: &foundrymodel.OntologyVersion{ID: "v", Definition: def}}
	graph := graphFunc(func(ctx context.Context, tenantID string, query []byte) (int, []byte, error) {
		queries = append(queries, string(query))
		if tenantID != "tenant-a" {
			t.Errorf("wrong tenant: %s", tenantID)
		}
		if a, _ := ports.ActorFrom(ctx); a.UserID != reviewer.UserID {
			t.Errorf("read as %+v, not the reviewer", a)
		}
		response := review
		if len(queries) > 1 {
			response = `{"items":[{"entity_key":"customer-1"}]}`
		}
		return 200, []byte(response), nil
	})
	return &Service{deps: &runtimeDeps{DB: db, Graph: graph}}, db, &queries
}

func resolveChoice(key string) *foundryv1.FoundryReviewChoice {
	return &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Resolve{Resolve: &foundryv1.FoundryResolveChoice{Target: &foundryv1.FoundryResolveChoice_EntityKey{EntityKey: key}}}}
}
func noneChoice() *foundryv1.FoundryReviewChoice {
	return &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Resolve{Resolve: &foundryv1.FoundryResolveChoice{Target: &foundryv1.FoundryResolveChoice_NotInData{NotInData: true}}}}
}
func propChoice(v *foundryv1.FoundryPropValue) *foundryv1.FoundryReviewChoice {
	return &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Prop{Prop: v}}
}
func propText(s string) *foundryv1.FoundryPropValue {
	return &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Text{Text: s}}
}
func propInteger(n int64) *foundryv1.FoundryPropValue {
	return &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Integer{Integer: n}}
}
func propNumber(f float64) *foundryv1.FoundryPropValue {
	return &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Number{Number: f}}
}
func propBoolean(b bool) *foundryv1.FoundryPropValue {
	return &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Boolean{Boolean: b}}
}
func propNull() *foundryv1.FoundryPropValue {
	return &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Null{}}
}
func rejectChoice(hash string) *foundryv1.FoundryReviewChoice {
	return &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Reject{Reject: &foundryv1.FoundryRejectChoice{StatementHash: hash}}}
}

func releaseChoice(hash string) *foundryv1.FoundryReviewChoice {
	return &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Release{Release: &foundryv1.FoundryReleaseChoice{StatementHash: hash}}}
}

const testStatement = `{"subject":{"type":"Commitment","name":"support"},"object":{"type":"Account","name":"Acme"},"props":{}}`

var (
	testClaimKey = strings.Repeat("ab", 32)
	testRead     = foundrymodel.FoundryStatementHash("promised_to", testStatement)
)

// heldReview is a held claim of reason, as the load stores it, at a place.
func heldReview(reason, claimKey string) string {
	return heldStatement(reason, claimKey, testStatement)
}

// heldStatement is a held claim stating statement.
func heldStatement(reason, claimKey, statement string) string {
	stored, _ := json.Marshal(statement)
	return `{"items":[{"reason":"` + reason + `","doc_id":"doc-a","claim_key":"` + claimKey + `","section":2,"start":10,"end":25,"suggested_link":"promised_to","statement":` + string(stored) + `}]}`
}

func settleRequest(choice *foundryv1.FoundryReviewChoice) *foundryv1.SettleFoundryReviewRequest {
	return &foundryv1.SettleFoundryReviewRequest{WorkspaceId: "workspace-a", OntologyId: "o", ReviewId: "review-1", Decision: choice}
}

func TestSettleFoundryReview(t *testing.T) {
	for _, reason := range []string{"unresolved", "ambiguous", "collision", "identifier_conflict", "adjudicated", "legal_form", "part_name", "own_name"} {
		for _, choice := range []*foundryv1.FoundryReviewChoice{resolveChoice("customer-1"), noneChoice()} {
			t.Run(reason+choice.String(), func(t *testing.T) {
				review := `{"items":[{"reason":"` + reason + `","type":"Customer","doc_id":"doc-a","norm_name":"variant","entity_key":"previous-link","confidence":0.98}]}`
				s, db, queries := reviewService(t, review)
				out, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(choice))
				if err != nil {
					t.Fatal(err)
				}
				if len(db.saved) != 1 || out.Decision.DocId == nil || *out.Decision.DocId != "doc-a" || out.Decision.GetType() != "Customer" || out.Decision.GetDecidedByUserId() != "user-a" {
					t.Fatalf("%+v saved %v", out, db.saved)
				}
				if (out.Decision.EntityKey == nil) != (choice.GetResolve().GetNotInData()) {
					t.Fatalf("bad choice: %+v", out.Decision)
				}
				if !choice.GetResolve().GetNotInData() && out.Decision.GetEntityKey() != choice.GetResolve().GetEntityKey() {
					t.Fatalf("kept the previous adjudicated target: %+v", out.Decision)
				}
				for _, q := range *queries {
					if !strings.Contains(q, `"request_type":"read"`) || !strings.Contains(q, `"foundry_load"`) || !strings.Contains(q, `"load-1"`) {
						t.Fatalf("unscoped read %s", q)
					}
				}
			})
		}
	}
	for _, value := range []*foundryv1.FoundryPropValue{propText("Chosen"), propNull()} {
		s, db, _ := reviewService(t, `{"items":[{"reason":"prop_conflict","type":"Customer","entity_key":"customer-1","prop":"name"}]}`)
		out, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(propChoice(value)))
		if err != nil {
			t.Fatal(err)
		}
		if len(db.saved) != 1 || out.Decision.Kind != "prop" || out.Decision.GetProp() != "name" || out.Decision.Value == nil {
			t.Fatalf("%+v", out)
		}
		if db.saved[0].DocumentIDs != nil {
			t.Fatalf("a review that quotes nothing records no documents: %v", db.saved[0].DocumentIDs)
		}
		// A value applies at once, as an approval does: to the load its
		// review was read from, by a resend job the person asked for, and
		// the answer says it waits.
		if db.releasedTo != "load-1" || len(db.resends) != 1 || db.resends[0].ID == "" || db.resends[0].RequestedByUserID == nil ||
			*db.resends[0].RequestedByUserID != reviewer.UserID {
			t.Fatalf("applied to %q by %+v", db.releasedTo, db.resends)
		}
		if a := out.Decision.GetApplied(); a.GetStatus() != "pending" || a.GetReason() != "" || a.GetAt() != nil {
			t.Fatalf("applied %+v", a)
		}
	}
	// The documents its quotes come from are recorded with it, once each:
	// a grantee covers the value only by covering them.
	quotes := `[{"value":"A","doc_id":"doc-a","quote":"A","span":{"section":0,"start":0,"end":1}},` +
		`{"value":"B","doc_id":"doc-b","quote":"B","span":{"section":1,"start":0,"end":1}},{"value":"A","doc_id":"doc-a"},{"value":"C"}]`
	values, _ := json.Marshal(quotes)
	s, db, _ := reviewService(t, `{"items":[{"reason":"prop_conflict","type":"Customer","entity_key":"customer-1","prop":"name","values":`+string(values)+`}]}`)
	if _, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(propChoice(propText("A")))); err != nil {
		t.Fatal(err)
	}
	if got := db.saved[0].DocumentIDs; !slices.Equal(got, []string{"doc-a", "doc-b"}) {
		t.Fatalf("recorded %v", got)
	}
}

// A person's approval of a held claim names its document and its key, no type,
// and reads nothing of the load's entities.
func TestSettleHeldClaim(t *testing.T) {
	for _, reason := range []string{"low_confidence", "unverified"} {
		s, db, queries := reviewService(t, heldReview(reason, testClaimKey))
		out, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(releaseChoice(testRead)))
		if err != nil {
			t.Fatal(err)
		}
		d := out.Decision
		if d.GetStatementHash() != testRead {
			t.Fatalf("not bound to what was read: %+v", d)
		}
		if len(db.saved) != 1 || d.Kind != "release" || d.GetType() != "" || d.GetDocId() != "doc-a" || d.GetClaimKey() != testClaimKey || d.EntityKey != nil {
			t.Fatalf("%+v saved %v", d, db.saved)
		}
		if d.ClaimSection == nil || d.GetClaimSection() != 2 || d.GetClaimStart() != 10 || d.GetClaimEnd() != 25 {
			t.Fatalf("not pinned to its place: %+v", d)
		}
		if len(*queries) != 1 {
			t.Fatalf("read %d times: %v", len(*queries), *queries)
		}
		// It applies at once: to the load its review was read from, by a
		// resend job the person asked for, and the answer says it waits.
		if db.releasedTo != "load-1" || len(db.resends) != 1 || db.resends[0].ID == "" || db.resends[0].RequestedByUserID == nil ||
			*db.resends[0].RequestedByUserID != reviewer.UserID {
			t.Fatalf("applied to %q by %+v", db.releasedTo, db.resends)
		}
		if a := d.GetApplied(); a.GetStatus() != "pending" || a.GetReason() != "" || a.GetAt() != nil {
			t.Fatalf("applied %+v", a)
		}
	}
}

// A rejection of a held claim names the review as an approval does, bound to
// what the person read, and is only saved: no load is told, no job is
// handed over, and the answer says nothing of a load.
func TestRejectHeldClaim(t *testing.T) {
	for _, reason := range []string{"low_confidence", "unverified"} {
		s, db, queries := reviewService(t, heldReview(reason, testClaimKey))
		out, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(rejectChoice(testRead)))
		if err != nil {
			t.Fatal(err)
		}
		d := out.Decision
		if len(db.saved) != 1 || d.Kind != "reject" || d.GetType() != "" || d.GetDocId() != "doc-a" || d.GetClaimKey() != testClaimKey ||
			d.GetStatementHash() != testRead || d.EntityKey != nil || d.GetDecidedByUserId() != "user-a" {
			t.Fatalf("%+v saved %v", d, db.saved)
		}
		if d.ClaimSection == nil || d.GetClaimSection() != 2 || d.GetClaimStart() != 10 || d.GetClaimEnd() != 25 {
			t.Fatalf("not pinned to its place: %+v", d)
		}
		if len(*queries) != 1 {
			t.Fatalf("read %d times: %v", len(*queries), *queries)
		}
		if db.rejectedOn != "load-1" || db.releasedTo != "" || len(db.resends) != 0 || d.Applied != nil {
			t.Fatalf("rejected on %q, released to %q, resends %v, applied %v", db.rejectedOn, db.releasedTo, db.resends, d.Applied)
		}
	}
	// A claim that reads otherwise now is not the one the person rejected,
	// and a load that changed, or wrote the claim since, holds no such review.
	s, db, _ := reviewService(t, heldReview("low_confidence", testClaimKey))
	_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(rejectChoice(strings.Repeat("0", 64))))
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "reads differently now") || len(db.saved) != 0 {
		t.Fatalf("%v", err)
	}
	s, db, _ = reviewService(t, heldReview("low_confidence", testClaimKey))
	db.saveErr = foundrystore.ErrConflict
	_, err = s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(rejectChoice(testRead)))
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "reload the review") {
		t.Fatalf("%v", err)
	}
	// Only a held claim is rejected: a name or a value takes its own decision.
	for _, review := range []string{
		`{"items":[{"reason":"unresolved","type":"Customer","doc_id":"doc-a","norm_name":"variant"}]}`,
		`{"items":[{"reason":"prop_conflict","type":"Customer","entity_key":"customer-1","prop":"name"}]}`,
		`{"items":[{"reason":"conflict","doc_id":"doc-a"}]}`,
	} {
		s, db, _ := reviewService(t, review)
		_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(rejectChoice(testRead)))
		if status.Code(err) != codes.InvalidArgument && status.Code(err) != codes.FailedPrecondition || len(db.saved) != 0 {
			t.Fatalf("%s: %v", review, err)
		}
	}
}

// The current load changed between the review's read and the settle: its
// reviews are no longer the ones to settle, and the card must be read again.
func TestSettleHeldClaimOnAnotherLoad(t *testing.T) {
	s, db, _ := reviewService(t, heldReview("low_confidence", testClaimKey))
	db.saveErr = foundrystore.ErrConflict
	_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(releaseChoice(testRead)))
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "reload the review") {
		t.Fatalf("%v", err)
	}
}

// A value the current load can't be settled with: its load changed since the
// review was read, or the ontology holds as many value decisions as a load
// can be sent, which the answer says with what to do about it.
func TestSettleValueRefused(t *testing.T) {
	review := `{"items":[{"reason":"prop_conflict","type":"Customer","entity_key":"customer-1","prop":"name"}]}`
	for saveErr, want := range map[error]string{
		foundrystore.ErrConflict:               "reload the review",
		foundrystore.FoundryValueDecisionsFull: "10000 value decisions, the most a load can be sent; remove one",
	} {
		s, db, _ := reviewService(t, review)
		db.saveErr = saveErr
		_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(propChoice(propText("Chosen"))))
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), want) || len(db.saved) != 0 {
			t.Fatalf("%v: %v", saveErr, err)
		}
	}
}

// A decision lists what became of it in the current load: nothing for one
// the load's list doesn't hold, pending for one a resend owes (carried over
// or settled there), the engine's reason for one it refused, and applied for
// a value the load was sent.
func TestListFoundryReviewDecisionsApplied(t *testing.T) {
	s, db, _ := reviewService(t, `{}`)
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		db.rows = append(db.rows, &foundrymodel.FoundryReviewDecision{ID: id, Kind: foundrymodel.FoundryReviewResolve, Type: "Customer"})
	}
	db.applied = map[string]foundrymodel.FoundryDecisionApplied{
		"b": {Status: foundrymodel.FoundryReleaseCarried},
		"c": {Status: foundrymodel.FoundryReleaseRefused, Reason: "unresolved", At: &at},
		"d": {Status: foundrymodel.FoundryReleaseWritten, At: &at},
		"e": {},
		"f": {Status: foundrymodel.FoundryReleasePending, Reason: foundrymodel.FoundryReleaseJobFailed},
		"g": {Status: foundrymodel.FoundryReleaseCarried, Reason: foundrymodel.FoundryReleaseJobFailed},
		"h": {Status: foundrymodel.FoundryValueApplied, At: &at},
	}
	out, err := s.ListFoundryReviewDecisions(context.Background(), &foundryv1.ListFoundryReviewDecisionsRequest{WorkspaceId: "workspace-a", OntologyId: "o"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, d := range out.GetDecisions() {
		if d.Applied == nil {
			got[d.GetId()] = "unset"
			continue
		}
		got[d.GetId()] = d.Applied.GetStatus() + "/" + d.Applied.GetReason()
		if (d.Applied.GetAt() != nil) != (d.GetId() == "c" || d.GetId() == "d" || d.GetId() == "h") {
			t.Fatalf("%s at %v", d.GetId(), d.Applied.GetAt())
		}
	}
	// One whose job failed is neither pending nor refused: nothing is coming,
	// and whether it was written isn't known.
	want := map[string]string{"a": "unset", "b": "pending/", "c": "refused/unresolved", "d": "written/", "e": "/",
		"f": "unconfirmed/job_failed", "g": "unconfirmed/job_failed", "h": "applied/"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("applied %v", got)
	}
}

func TestSettleHeldClaimValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		review string
		choice *foundryv1.FoundryReviewChoice
		code   codes.Code
	}{
		"a resolve of a held claim": {heldReview("low_confidence", testClaimKey), noneChoice(), codes.InvalidArgument},
		"a value of a held claim":   {heldReview("low_confidence", testClaimKey), propChoice(propText("x")), codes.InvalidArgument},
		"no release":                {heldReview("low_confidence", testClaimKey), &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Release{}}, codes.InvalidArgument},
		"no reject":                 {heldReview("low_confidence", testClaimKey), &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Reject{}}, codes.InvalidArgument},
		"a reject with no hash":     {heldReview("low_confidence", testClaimKey), rejectChoice(""), codes.FailedPrecondition},
		"no hash":                   {heldReview("low_confidence", testClaimKey), releaseChoice(""), codes.FailedPrecondition},
		"a card read before a load": {heldStatement("low_confidence", testClaimKey, `{"other":"reading"}`), releaseChoice(testRead), codes.FailedPrecondition},
		"another link":              {strings.Replace(heldReview("low_confidence", testClaimKey), "promised_to", "partner_of", 1), releaseChoice(testRead), codes.FailedPrecondition},
		"no statement":              {heldStatement("low_confidence", testClaimKey, ""), releaseChoice(foundrymodel.FoundryStatementHash("promised_to", "")), codes.FailedPrecondition},
		"no link":                   {strings.Replace(heldReview("low_confidence", testClaimKey), `"suggested_link":"promised_to",`, "", 1), releaseChoice(foundrymodel.FoundryStatementHash("", testStatement)), codes.FailedPrecondition},
		"a release of a name":       {`{"items":[{"reason":"unresolved","type":"Customer","doc_id":"a","norm_name":"a"}]}`, releaseChoice(testRead), codes.InvalidArgument},
		"a release of a conflict":   {`{"items":[{"reason":"prop_conflict","type":"Customer","entity_key":"customer-1","prop":"name"}]}`, releaseChoice(testRead), codes.InvalidArgument},
		"written before claim keys": {`{"items":[{"reason":"low_confidence","doc_id":"doc-a"}]}`, releaseChoice(testRead), codes.FailedPrecondition},
		"a key the engine refuses":  {heldReview("low_confidence", "ABC"), releaseChoice(testRead), codes.FailedPrecondition},
		"no place":                  {`{"items":[{"reason":"low_confidence","doc_id":"doc-a","claim_key":"` + testClaimKey + `"}]}`, releaseChoice(testRead), codes.FailedPrecondition},
		"a value's hold":            {heldReview("attribute_low_confidence", testClaimKey), releaseChoice(testRead), codes.FailedPrecondition},
		"a conflict":                {heldReview("conflict", testClaimKey), releaseChoice(testRead), codes.FailedPrecondition},
		"a constraint's":            {heldReview("violation", testClaimKey), releaseChoice(testRead), codes.FailedPrecondition},
		"an identity":               {heldReview("identity", testClaimKey), releaseChoice(testRead), codes.FailedPrecondition},
	} {
		t.Run(name, func(t *testing.T) {
			s, db, _ := reviewService(t, tc.review)
			_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(tc.choice))
			if status.Code(err) != tc.code || len(db.saved) != 0 {
				t.Fatalf("err %v, writes %d", err, len(db.saved))
			}
		})
	}
}

func TestSettleFoundryReviewValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		review string
		choice *foundryv1.FoundryReviewChoice
		code   codes.Code
	}{
		"wrong resolve kind":       {`{"items":[{"reason":"unresolved","type":"Customer","doc_id":"a","norm_name":"variant"}]}`, propChoice(propText("name")), codes.InvalidArgument},
		"wrong prop kind":          {`{"items":[{"reason":"prop_conflict","type":"Customer","entity_key":"customer-1","prop":"name"}]}`, noneChoice(), codes.InvalidArgument},
		"unsupported reason":       {`{"items":[{"reason":"unmatched","type":"Customer"}]}`, noneChoice(), codes.InvalidArgument},
		"a 4.9.0 identity review":  {`{"items":[{"reason":"identity","type":"Customer"}]}`, noneChoice(), codes.InvalidArgument},
		"missing review":           {`{"items":[]}`, noneChoice(), codes.NotFound},
		"missing type":             {`{"items":[{"reason":"unresolved"}]}`, noneChoice(), codes.FailedPrecondition},
		"missing name":             {`{"items":[{"reason":"unresolved","type":"Customer","doc_id":"a"}]}`, noneChoice(), codes.FailedPrecondition},
		"empty target":             {`{"items":[{"reason":"unresolved","type":"Customer","doc_id":"a","norm_name":"a"}]}`, resolveChoice(""), codes.InvalidArgument},
		"wrong value type":         {`{"items":[{"reason":"prop_conflict","type":"Customer","entity_key":"customer-1","prop":"name"}]}`, propChoice(propNumber(3)), codes.InvalidArgument},
		"removed property":         {`{"items":[{"reason":"prop_conflict","type":"Customer","entity_key":"customer-1","prop":"gone"}]}`, propChoice(propNull()), codes.FailedPrecondition},
		"other type":               {`{"items":[{"reason":"unresolved","type":"Other"}]}`, noneChoice(), codes.FailedPrecondition},
		"malformed graph response": {`{`, noneChoice(), codes.Internal},
	} {
		t.Run(name, func(t *testing.T) {
			s, db, _ := reviewService(t, tc.review)
			_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(tc.choice))
			if status.Code(err) != tc.code || len(db.saved) != 0 {
				t.Fatalf("err %v, writes %d", err, len(db.saved))
			}
		})
	}
	for name, change := range map[string]func(*Service, *decisionStore, *foundryv1.SettleFoundryReviewRequest){
		"another workspace": func(_ *Service, _ *decisionStore, r *foundryv1.SettleFoundryReviewRequest) { r.WorkspaceId = "other" },
		"no current load": func(_ *Service, d *decisionStore, _ *foundryv1.SettleFoundryReviewRequest) {
			d.ontology.CurrentLoadID = nil
		},
		"load deleted": func(_ *Service, d *decisionStore, _ *foundryv1.SettleFoundryReviewRequest) { d.load = nil },
		"load writing": func(_ *Service, d *decisionStore, _ *foundryv1.SettleFoundryReviewRequest) {
			d.load.Status = foundrymodel.FoundryLoadWriting
		},
		"missing choice": func(_ *Service, _ *decisionStore, r *foundryv1.SettleFoundryReviewRequest) { r.Decision = nil },
	} {
		t.Run(name, func(t *testing.T) {
			s, db, _ := reviewService(t, `{"items":[]}`)
			r := settleRequest(noneChoice())
			change(s, db, r)
			_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), r)
			if err == nil || len(db.saved) != 0 {
				t.Fatalf("err %v", err)
			}
		})
	}
	t.Run("missing actor", func(t *testing.T) {
		s, db, _ := reviewService(t, `{"items":[]}`)
		_, err := s.SettleFoundryReview(context.Background(), settleRequest(noneChoice()))
		if status.Code(err) != codes.Unauthenticated || len(db.saved) != 0 {
			t.Fatalf("err %v", err)
		}
	})
}

// A prop decision's value is checked as the engine parses it, and stored as
// the engine keeps it: one the engine refuses would fail every later load.
func TestSettleFoundryReviewPropValues(t *testing.T) {
	for _, tc := range []struct {
		prop  string
		value *foundryv1.FoundryPropValue
		saved string // the value stored; empty when refused
	}{
		{"name", propText("  Chosen "), `"Chosen"`},
		{"name", propText(""), ``},
		{"name", propText("   "), ``},
		{"name", propNull(), `null`},
		{"signed", propText("2026-03"), `"2026-03"`},
		{"signed", propText("2026"), `"2026"`},
		{"signed", propText("2026-02-30"), ``},
		{"at", propText("2026-03-14T09:30:00+01:00"), `"2026-03-14T08:30:00Z"`},
		{"at", propText("9999-12-31T23:30:00-01:00"), ``},
		{"count", propNumber(3), `3`},
		{"count", propNumber(3.5), ``},
		{"count", propText("3"), ``},
		// An integer keeps every digit: past 2^53 a double would round it,
		// and int64's extremes are in range.
		{"count", propInteger(9007199254740993), `9007199254740993`},
		{"count", propInteger(math.MaxInt64), `9223372036854775807`},
		{"count", propInteger(math.MinInt64), `-9223372036854775808`},
		{"count", &foundryv1.FoundryPropValue{}, ``},
		// A float keeps what the engine's f64 does.
		{"amount", propInteger(9007199254740993), `9007199254740992`},
		{"amount", propNumber(2.5), `2.5`},
		{"amount", propNumber(math.Inf(1)), ``},
		{"amount", propNumber(math.NaN()), ``},
		{"active", propBoolean(true), `true`},
		{"active", propInteger(1), ``},
	} {
		t.Run(tc.prop+" "+tc.value.String(), func(t *testing.T) {
			s, db, _ := reviewService(t, `{"items":[{"reason":"prop_conflict","type":"Customer","entity_key":"customer-1","prop":"`+tc.prop+`"}]}`)
			db.version.Definition, _ = json.Marshal(ontologyagent.Definition{OntologyTOML: "[types.Customer]\nkey = [\"id\"]\n" +
				`props = { id = "string", name = "string", signed = "date", at = "datetime", count = "integer", amount = "float", active = "boolean" }`})
			_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(propChoice(tc.value)))
			if tc.saved == "" {
				if status.Code(err) != codes.InvalidArgument || len(db.saved) != 0 {
					t.Fatalf("err %v, saved %v", err, db.saved)
				}
				return
			}
			if err != nil || len(db.saved) != 1 || string(db.saved[0].Value) != tc.saved {
				t.Fatalf("err %v, saved %v", err, db.saved)
			}
		})
	}
}

// A stored value is answered with every digit an integer has: listed as a
// double, one past 2^53 would come back as another number.
func TestFoundryReviewDecisionValueReadBack(t *testing.T) {
	for stored, want := range map[string]*foundryv1.FoundryPropValue{
		`9007199254740993`:     propInteger(9007199254740993),
		`9223372036854775807`:  propInteger(math.MaxInt64),
		`-9223372036854775808`: propInteger(math.MinInt64),
		`3`:                    propInteger(3),
		`2.5`:                  propNumber(2.5),
		`1e+20`:                propNumber(1e20),
		`"Chosen"`:             propText("Chosen"),
		`true`:                 propBoolean(true),
		`null`:                 propNull(),
	} {
		d := &foundrymodel.FoundryReviewDecision{ID: "d", Kind: foundrymodel.FoundryReviewProp, Type: "Customer", EntityKey: ptr("customer-1"), Prop: ptr("count"), Value: json.RawMessage(stored)}
		if got := foundryReviewDecisionProto(d).GetValue(); !proto.Equal(got, want) {
			t.Errorf("%s read back as %v, want %v", stored, got, want)
		}
	}
	resolve := &foundrymodel.FoundryReviewDecision{ID: "d", Kind: foundrymodel.FoundryReviewResolve, Type: "Customer", DocID: ptr("doc"), NormName: ptr("name")}
	if got := foundryReviewDecisionProto(resolve).GetValue(); got != nil {
		t.Errorf("a resolve has no value, got %v", got)
	}
}

func TestListDeleteFoundryReviewDecisions(t *testing.T) {
	s, db, _ := reviewService(t, `{}`)
	for i := 0; i < 101; i++ {
		db.rows = append(db.rows, &foundrymodel.FoundryReviewDecision{ID: "d", Kind: foundrymodel.FoundryReviewResolve, Type: "Customer"})
	}
	out, err := s.ListFoundryReviewDecisions(context.Background(), &foundryv1.ListFoundryReviewDecisionsRequest{WorkspaceId: "workspace-a", OntologyId: "o"})
	if err != nil || len(out.GetDecisions()) != 100 || out.GetNextPageToken() != "d" {
		t.Fatalf("%v %v", out, err)
	}
	for _, ws := range []string{"workspace-a", "other"} {
		_, err := s.DeleteFoundryReviewDecision(context.Background(), &foundryv1.DeleteFoundryReviewDecisionRequest{WorkspaceId: ws, OntologyId: "o", Id: "d"})
		if ws == "workspace-a" && (err != nil || db.deleted != "d") {
			t.Fatal(err)
		}
		if ws == "other" && status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
		_, err = s.ListFoundryReviewDecisions(context.Background(), &foundryv1.ListFoundryReviewDecisionsRequest{WorkspaceId: ws, OntologyId: "o"})
		if ws == "other" && status.Code(err) != codes.NotFound {
			t.Fatal(err)
		}
	}
	_, err = s.DeleteFoundryReviewDecision(context.Background(), &foundryv1.DeleteFoundryReviewDecisionRequest{WorkspaceId: "workspace-a", OntologyId: "o", Id: "missing"})
	if status.Code(err) != codes.NotFound {
		t.Fatal(err)
	}
	// A removal hands the store the resend job that withdraws a value
	// decision from the current load, asked for by who removed it.
	_, err = s.DeleteFoundryReviewDecision(ports.WithActor(context.Background(), reviewer),
		&foundryv1.DeleteFoundryReviewDecisionRequest{WorkspaceId: "workspace-a", OntologyId: "o", Id: "v"})
	if err != nil || db.deleted != "v" {
		t.Fatal(err)
	}
	if last := db.resends[len(db.resends)-1]; last.ID == "" || last.RequestedByUserID == nil || *last.RequestedByUserID != reviewer.UserID {
		t.Fatalf("resend %+v", last)
	}
}

func TestSettleFoundryReviewReadFailures(t *testing.T) {
	review := `{"items":[{"reason":"unresolved","type":"Customer","doc_id":"doc-a","norm_name":"variant"}]}`
	for _, code := range []int{429, 500} {
		s, db, _ := reviewService(t, review)
		s.deps.Graph = graphFunc(func(context.Context, string, []byte) (int, []byte, error) { return code, []byte(`{}`), nil })
		_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(noneChoice()))
		if status.Code(err) != codes.Unavailable || len(db.saved) != 0 {
			t.Fatalf("%v", err)
		}
	}
	s, db, _ := reviewService(t, review)
	calls := 0
	s.deps.Graph = graphFunc(func(context.Context, string, []byte) (int, []byte, error) {
		calls++
		body := review
		if calls > 1 {
			body = `{"items":[]}`
		}
		return 200, []byte(body), nil
	})
	_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(resolveChoice("other-load-entity")))
	if status.Code(err) != codes.InvalidArgument || len(db.saved) != 0 {
		t.Fatalf("%v", err)
	}
	for _, saveErr := range []error{foundrystore.ErrConflict, foundrystore.ErrNotFound, io.ErrUnexpectedEOF} {
		s, db, _ := reviewService(t, review)
		db.saveErr = saveErr
		_, err := s.SettleFoundryReview(ports.WithActor(context.Background(), reviewer), settleRequest(noneChoice()))
		want := codes.Internal
		if saveErr == foundrystore.ErrConflict {
			want = codes.FailedPrecondition
		}
		if saveErr == foundrystore.ErrNotFound {
			want = codes.NotFound
		}
		if status.Code(err) != want || len(db.saved) != 0 {
			t.Fatalf("%v", err)
		}
	}
}
