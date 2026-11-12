package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

func (s *decisionStore) SaveFoundryReviewLabel(_ context.Context, ws string, l *foundrymodel.FoundryReviewLabel) error {
	if ws != s.ontology.WorkspaceID {
		return foundrystore.ErrNotFound
	}
	if s.labelErr != nil {
		return s.labelErr
	}
	if err := l.Validate(); err != nil {
		return err
	}
	l.DecidedAt = time.Now()
	s.labels = append(s.labels, l)
	return nil
}
func (s *decisionStore) FoundryReviewLabelsOf(_ context.Context, o, doc string, span foundrymodel.FoundryClaimSpan, claimKey, factKey *string) ([]*foundrymodel.FoundryReviewLabel, error) {
	same := func(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
	var out []*foundrymodel.FoundryReviewLabel
	for _, l := range s.labels {
		if o == s.ontology.ID && l.DocID == doc && l.Span == span && same(l.ClaimKey, claimKey) && same(l.FactKey, factKey) {
			out = append(out, l)
		}
	}
	return out, nil
}
func (s *decisionStore) ListFoundryReviewLabels(_ context.Context, o, after string, limit int) ([]*foundrymodel.FoundryReviewLabel, error) {
	if o != s.ontology.ID {
		panic("wrong ontology")
	}
	return s.labels, nil
}
func (s *decisionStore) GetFoundryReviewLabel(_ context.Context, o, id string) (*foundrymodel.FoundryReviewLabel, error) {
	for _, l := range s.labels {
		if o == s.ontology.ID && l.ID == id {
			return l, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}
func (s *decisionStore) GetFoundryReviewDecision(_ context.Context, o, id string) (*foundrymodel.FoundryReviewDecision, error) {
	for _, d := range s.rows {
		if o == s.ontology.ID && d.ID == id {
			return d, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

// heldScored is a held claim of the random slice: as heldReview, read with
// the verifier's score.
func heldScored(reason string, confidence string) string {
	return strings.Replace(heldReview(reason, testClaimKey), `"suggested_link"`, `"confidence":`+confidence+`,"suggested_link"`, 1)
}

func drawnRequest(choice *foundryv1.FoundryReviewChoice, drawn string) *foundryv1.SettleFoundryReviewRequest {
	r := settleRequest(choice)
	r.Drawn = drawn
	return r
}

// A decision made on a card of the review queue says how its card was drawn,
// and a held claim's decision of the random slice is kept as a label of the
// claim too, with the score its review was read at: an approval says it is
// right, a rejection wrong. A ranked one, and one made elsewhere, is not.
func TestSettleDrawnHeldClaim(t *testing.T) {
	ctx := ports.WithActor(context.Background(), reviewer)
	for _, tc := range []struct {
		choice  *foundryv1.FoundryReviewChoice
		verdict foundrymodel.FoundryVerdict
	}{{releaseChoice(testRead), foundrymodel.FoundryVerdictRight}, {rejectChoice(testRead), foundrymodel.FoundryVerdictWrong}} {
		s, db, _ := reviewService(t, heldScored("low_confidence", "0.83"))
		out, err := s.SettleFoundryReview(ctx, drawnRequest(tc.choice, "random"))
		if err != nil {
			t.Fatal(err)
		}
		if out.Decision.GetDrawn() != "random" || db.saved[0].Drawn == nil || *db.saved[0].Drawn != foundrymodel.FoundryDrawnRandom {
			t.Fatalf("drawn %q saved %+v", out.Decision.GetDrawn(), db.saved[0].Drawn)
		}
		if len(db.labels) != 1 {
			t.Fatalf("labels %+v", db.labels)
		}
		l := db.labels[0]
		if l.Drawn != foundrymodel.FoundryLabelRandom || l.Verdict != tc.verdict || l.DocID != "doc-a" || l.Span != (foundrymodel.FoundryClaimSpan{Section: 2, Start: 10, End: 25}) ||
			*l.ClaimKey != testClaimKey || *l.StatementHash != testRead || l.FactKey != nil || *l.Link != "promised_to" || *l.Confidence != 0.83 ||
			*l.DecisionID != db.saved[0].ID || l.LoadID != "load-1" || l.DecidedByUserID != "user-a" || l.Against != nil {
			t.Fatalf("label %+v", l)
		}
	}
	// Approved again where someone approved the statement first: their
	// decision stands, and the label is this person's word.
	s, db, _ := reviewService(t, heldScored("low_confidence", "0.83"))
	db.firstApprover = "user-z"
	out, err := s.SettleFoundryReview(ctx, drawnRequest(releaseChoice(testRead), "random"))
	if err != nil || out.Decision.GetDecidedByUserId() != "user-z" || len(db.labels) != 1 || db.labels[0].DecidedByUserID != "user-a" {
		t.Fatalf("%v %+v %+v", err, out, db.labels)
	}
	// Ranked: the decision says so, and no label is kept.
	s, db, _ = reviewService(t, heldScored("unverified", "null"))
	out, err = s.SettleFoundryReview(ctx, drawnRequest(releaseChoice(testRead), "ranked"))
	if err != nil || out.Decision.GetDrawn() != "ranked" || len(db.labels) != 0 {
		t.Fatalf("%v %+v %+v", err, out, db.labels)
	}
	// Elsewhere: nothing drawn.
	s, db, _ = reviewService(t, heldReview("low_confidence", testClaimKey))
	out, err = s.SettleFoundryReview(ctx, settleRequest(releaseChoice(testRead)))
	if err != nil || out.Decision.Drawn != "" || db.saved[0].Drawn != nil || len(db.labels) != 0 {
		t.Fatalf("%v %+v %+v", err, out, db.labels)
	}
	// A held claim read with no score is labelled with none.
	s, db, _ = reviewService(t, heldScored("unverified", "null"))
	if _, err := s.SettleFoundryReview(ctx, drawnRequest(rejectChoice(testRead), "random")); err != nil || len(db.labels) != 1 || db.labels[0].Confidence != nil {
		t.Fatalf("%v %+v", err, db.labels)
	}
	// A score out of range is none.
	s, db, _ = reviewService(t, heldScored("low_confidence", "1.5"))
	if _, err := s.SettleFoundryReview(ctx, drawnRequest(rejectChoice(testRead), "random")); err != nil || db.labels[0].Confidence != nil {
		t.Fatalf("%v %+v", err, db.labels)
	}
	// A name settled from the queue says how it was drawn and keeps no label.
	review := `{"items":[{"reason":"unresolved","type":"Customer","doc_id":"doc-a","norm_name":"variant","confidence":0.95}]}`
	s, db, _ = reviewService(t, review)
	out, err = s.SettleFoundryReview(ctx, drawnRequest(noneChoice(), "ranked"))
	if err != nil || out.Decision.GetDrawn() != "ranked" || len(db.labels) != 0 {
		t.Fatalf("%v %+v %+v", err, out, db.labels)
	}
	// Anything else drawn is refused before a decision is made.
	s, db, _ = reviewService(t, heldScored("low_confidence", "0.83"))
	if _, err := s.SettleFoundryReview(ctx, drawnRequest(releaseChoice(testRead), "lucky")); status.Code(err) != codes.InvalidArgument || len(db.saved) != 0 {
		t.Fatalf("%v saved %v", err, db.saved)
	}
	// The decision stands when its label can't be saved, and the answer says
	// to settle again, which saves the same decision and the label.
	s, db, _ = reviewService(t, heldScored("low_confidence", "0.83"))
	db.labelErr = errors.New("down")
	if _, err := s.SettleFoundryReview(ctx, drawnRequest(releaseChoice(testRead), "random")); status.Code(err) != codes.Internal ||
		!strings.Contains(err.Error(), "settle it again") || len(db.saved) != 1 {
		t.Fatalf("%v saved %v", err, db.saved)
	}
}

// factGraph answers foundryFactQuote's read with quotes, their section edges
// and the sections' ordinals, and records each read.
func factGraph(t *testing.T, body string, reads *[]string) graphFunc {
	return func(ctx context.Context, tenantID string, query []byte) (int, []byte, error) {
		*reads = append(*reads, string(query))
		if tenantID != "tenant-a" {
			t.Errorf("wrong tenant: %s", tenantID)
		}
		return 200, []byte(body), nil
	}
}

const factQuotes = `{"items":[{"$id":7,"start":10,"end":25,"link":"partner_of","confidence":0.93},{"$id":8,"start":40,"end":55,"link":"partner_of","confidence":0.91}],
"in_section":[{"$from":7,"$to":70},{"$from":8,"$to":71}],"sections":[{"$id":70,"ordinal":2},{"$id":71,"ordinal":3}]}`

func labelService(t *testing.T, body string) (*Service, *decisionStore, *[]string) {
	s, db, _ := reviewService(t, "")
	reads := []string{}
	s.deps.Graph = factGraph(t, body, &reads)
	return s, db, &reads
}

func factRequest(fact *foundryv1.FoundryLabelFact, verdict, drawn string) *foundryv1.SettleFoundryReviewLabelRequest {
	return &foundryv1.SettleFoundryReviewLabelRequest{WorkspaceId: "workspace-a", OntologyId: "o",
		Subject: &foundryv1.SettleFoundryReviewLabelRequest_Fact{Fact: fact}, Verdict: verdict, Drawn: drawn}
}

func fact(section, start, end uint32) *foundryv1.FoundryLabelFact {
	return &foundryv1.FoundryLabelFact{DocId: "doc-a", Section: section, Start: start, End: end, FactKey: "fk-1"}
}

// An accepted fact's quote drawn at random is labelled from the current load
// as it holds it: the quote at the place sent, in the section of that
// ordinal, with its link and the verifier's score from the graph.
func TestLabelAcceptedFact(t *testing.T) {
	ctx := ports.WithActor(context.Background(), reviewer)
	s, db, reads := labelService(t, factQuotes)
	out, err := s.SettleFoundryReviewLabel(ctx, factRequest(fact(2, 10, 25), "wrong", "random"))
	if err != nil {
		t.Fatal(err)
	}
	l := db.labels[0]
	if l.Drawn != foundrymodel.FoundryLabelRandom || l.Verdict != foundrymodel.FoundryVerdictWrong || l.DocID != "doc-a" || *l.FactKey != "fk-1" ||
		l.ClaimKey != nil || *l.Link != "partner_of" || *l.Confidence != 0.93 || l.LoadID != "load-1" || l.DecidedByUserID != "user-a" || l.Against != nil {
		t.Fatalf("label %+v", l)
	}
	if out.Label.GetFactKey() != "fk-1" || out.Label.GetConfidence() != 0.93 || out.Label.GetVerdict() != "wrong" || out.Label.Against != nil {
		t.Fatalf("answer %+v", out.Label)
	}
	// The read is the load's, by the fact's key and document.
	var read struct {
		Query struct {
			Read struct {
				Entries []json.RawMessage `json:"entries"`
			} `json:"read"`
		} `json:"query"`
	}
	if err := json.Unmarshal([]byte((*reads)[0]), &read); err != nil || len(read.Query.Read.Entries) != 4 ||
		!strings.Contains((*reads)[0], `"load-1"`) || !strings.Contains((*reads)[0], `"fk-1"`) || !strings.Contains((*reads)[0], `"doc-a"`) {
		t.Fatalf("read %s", (*reads)[0])
	}
	// No quote there: another section, another range, or none at all.
	for _, f := range []*foundryv1.FoundryLabelFact{fact(3, 10, 25), fact(2, 11, 25), fact(2, 40, 55)} {
		s, db, _ := labelService(t, factQuotes)
		if _, err := s.SettleFoundryReviewLabel(ctx, factRequest(f, "right", "random")); status.Code(err) != codes.NotFound || len(db.labels) != 0 {
			t.Fatalf("%v: %v %+v", f, err, db.labels)
		}
	}
	// A request that names no claim, says nothing a label takes, or reads
	// no load is refused before anything is read.
	for _, tc := range []struct {
		req  *foundryv1.SettleFoundryReviewLabelRequest
		code codes.Code
	}{
		{factRequest(fact(2, 10, 25), "maybe", "random"), codes.InvalidArgument},
		{factRequest(fact(2, 10, 25), "right", "ranked"), codes.InvalidArgument},
		// A fact's word is drawn at random; a re-check names the label.
		{factRequest(fact(2, 10, 25), "right", "recheck"), codes.InvalidArgument},
		{&foundryv1.SettleFoundryReviewLabelRequest{WorkspaceId: "workspace-a", OntologyId: "o", Verdict: "right", Drawn: "recheck",
			Subject: &foundryv1.SettleFoundryReviewLabelRequest_ReviewId{ReviewId: "review-1"}}, codes.InvalidArgument},
		{&foundryv1.SettleFoundryReviewLabelRequest{WorkspaceId: "workspace-a", OntologyId: "o", Verdict: "right", Drawn: "random",
			Subject: &foundryv1.SettleFoundryReviewLabelRequest_LabelId{LabelId: "l-1"}}, codes.InvalidArgument},
		{factRequest(fact(2, 25, 25), "right", "random"), codes.InvalidArgument},
		{factRequest(&foundryv1.FoundryLabelFact{DocId: "doc-a", Section: 2, Start: 10, End: 25}, "right", "random"), codes.InvalidArgument},
		{factRequest(&foundryv1.FoundryLabelFact{Section: 2, Start: 10, End: 25, FactKey: "fk-1"}, "right", "random"), codes.InvalidArgument},
		{factRequest(&foundryv1.FoundryLabelFact{DocId: "doc-a", Section: 2, Start: 10, End: 25, FactKey: strings.Repeat("f", 129)}, "right", "random"), codes.InvalidArgument},
		{&foundryv1.SettleFoundryReviewLabelRequest{WorkspaceId: "workspace-a", OntologyId: "o", Verdict: "right", Drawn: "random"}, codes.InvalidArgument},
		{&foundryv1.SettleFoundryReviewLabelRequest{WorkspaceId: "workspace-a", OntologyId: "o", Verdict: "right", Drawn: "random",
			Subject: &foundryv1.SettleFoundryReviewLabelRequest_DecisionId{DecisionId: "d-1"}}, codes.InvalidArgument},
	} {
		s, db, reads := labelService(t, factQuotes)
		if _, err := s.SettleFoundryReviewLabel(ctx, tc.req); status.Code(err) != tc.code || len(db.labels) != 0 || len(*reads) != 0 {
			t.Fatalf("%v: %v %+v %v", tc.req, err, db.labels, *reads)
		}
	}
	// No current load, or one not ready: nothing to read a fact from.
	s, db, _ = labelService(t, factQuotes)
	db.ontology.CurrentLoadID = nil
	if _, err := s.SettleFoundryReviewLabel(ctx, factRequest(fact(2, 10, 25), "right", "random")); status.Code(err) != codes.FailedPrecondition {
		t.Fatal(err)
	}
	s, db, _ = labelService(t, factQuotes)
	db.load.Status = foundrymodel.FoundryLoadWriting
	if _, err := s.SettleFoundryReviewLabel(ctx, factRequest(fact(2, 10, 25), "right", "random")); status.Code(err) != codes.FailedPrecondition {
		t.Fatal(err)
	}
	// A signed-out caller labels nothing.
	s, _, _ = labelService(t, factQuotes)
	if _, err := s.SettleFoundryReviewLabel(context.Background(), factRequest(fact(2, 10, 25), "right", "random")); status.Code(err) != codes.Unauthenticated {
		t.Fatal(err)
	}
	// The graph's failure is the caller's to retry.
	s, _, _ = reviewService(t, "")
	s.deps.Graph = graphFunc(func(context.Context, string, []byte) (int, []byte, error) { return 503, nil, nil })
	if _, err := s.SettleFoundryReviewLabel(ctx, factRequest(fact(2, 10, 25), "right", "random")); status.Code(err) != codes.Unavailable {
		t.Fatal(err)
	}
}

// A re-check is a second person's word: on a decision of a held claim made
// by someone else, which it checks (an approval says right, a rejection
// wrong), or on another person's label of an accepted fact.
func TestRecheckLabel(t *testing.T) {
	ctx := ports.WithActor(context.Background(), reviewer)
	doc, span, hash := "doc-a", foundrymodel.FoundryClaimSpan{Section: 2, Start: 10, End: 25}, testRead
	decision := func(id, kind, by string) *foundrymodel.FoundryReviewDecision {
		return &foundrymodel.FoundryReviewDecision{ID: id, OntologyID: "o", Kind: foundrymodel.FoundryReviewDecisionKind(kind), DocID: &doc,
			ClaimKey: &testClaimKey, ClaimSpan: &span, StatementHash: &hash, DecidedByUserID: by}
	}
	recheck := func(id, verdict string) *foundryv1.SettleFoundryReviewLabelRequest {
		return &foundryv1.SettleFoundryReviewLabelRequest{WorkspaceId: "workspace-a", OntologyId: "o", Verdict: verdict, Drawn: "recheck",
			Subject: &foundryv1.SettleFoundryReviewLabelRequest_DecisionId{DecisionId: id}}
	}
	s, db, reads := labelService(t, factQuotes)
	db.rows = []*foundrymodel.FoundryReviewDecision{decision("d-1", "release", "user-b"), decision("d-2", "reject", "user-b"), decision("d-3", "release", "user-a"),
		{ID: "d-4", OntologyID: "o", Kind: foundrymodel.FoundryReviewResolve, Type: "Customer", DocID: &doc, DecidedByUserID: "user-b"}}
	// d-1 came from the random slice, so its label has the claim's score.
	link, confidence, right := "promised_to", 0.84, foundrymodel.FoundryVerdictRight
	db.labels = []*foundrymodel.FoundryReviewLabel{{ID: "l-0", OntologyID: "o", LoadID: "load-1", Drawn: foundrymodel.FoundryLabelRandom, DocID: doc, Span: span,
		ClaimKey: &testClaimKey, StatementHash: &hash, Link: &link, Confidence: &confidence, Verdict: right, DecisionID: ptr("d-1"), DecidedByUserID: "user-b"}}
	out, err := s.SettleFoundryReviewLabel(ctx, recheck("d-1", "wrong"))
	if err != nil {
		t.Fatal(err)
	}
	l := db.labels[1]
	if l.Drawn != foundrymodel.FoundryLabelRecheck || l.Verdict != foundrymodel.FoundryVerdictWrong || *l.Against != foundrymodel.FoundryVerdictRight ||
		*l.ClaimKey != testClaimKey || *l.StatementHash != hash || l.Span != span || *l.DecisionID != "d-1" || *l.Link != link || *l.Confidence != confidence ||
		out.Label.GetAgainst() != "right" || len(*reads) != 0 {
		t.Fatalf("label %+v answer %+v reads %v", l, out.Label, *reads)
	}
	// A rejection re-checked: against wrong; a ranked decision has no score.
	if _, err := s.SettleFoundryReviewLabel(ctx, recheck("d-2", "wrong")); err != nil || *db.labels[2].Against != foundrymodel.FoundryVerdictWrong ||
		db.labels[2].Confidence != nil {
		t.Fatalf("%v %+v", err, db.labels[2])
	}
	for id, code := range map[string]codes.Code{"d-3": codes.FailedPrecondition, "d-4": codes.FailedPrecondition, "missing": codes.NotFound,
		strings.Repeat("x", 27): codes.InvalidArgument} {
		if _, err := s.SettleFoundryReviewLabel(ctx, recheck(id, "right")); status.Code(err) != code {
			t.Fatalf("%s: %v", id, err)
		}
	}
	// One who labelled the claim at random re-checks nobody's word on it.
	db.labels = append(db.labels, &foundrymodel.FoundryReviewLabel{ID: "l-9", OntologyID: "o", LoadID: "load-1", Drawn: foundrymodel.FoundryLabelRandom,
		DocID: doc, Span: span, ClaimKey: &testClaimKey, StatementHash: &hash, Verdict: right, DecidedByUserID: "user-a"})
	if _, err := s.SettleFoundryReviewLabel(ctx, recheck("d-1", "right")); status.Code(err) != codes.FailedPrecondition {
		t.Fatal(err)
	}

	// Another person's label drawn at random: re-checked by its id, against
	// its verdict, with its claim, link and score.
	s, db, _ = labelService(t, factQuotes)
	key, wrong := "fk-1", foundrymodel.FoundryVerdictWrong
	db.labels = []*foundrymodel.FoundryReviewLabel{
		{ID: "l-1", OntologyID: "o", LoadID: "load-1", Drawn: foundrymodel.FoundryLabelRandom, DocID: doc, Span: span,
			FactKey: &key, Link: &link, Confidence: &confidence, Verdict: wrong, DecidedByUserID: "user-b"},
		{ID: "l-2", OntologyID: "o", LoadID: "load-1", Drawn: foundrymodel.FoundryLabelRandom, DocID: doc, Span: span,
			FactKey: &key, Verdict: right, DecidedByUserID: "user-c"},
		{ID: "l-3", OntologyID: "o", LoadID: "load-1", Drawn: foundrymodel.FoundryLabelRecheck, DocID: doc, Span: span,
			FactKey: &key, Verdict: right, Against: &wrong, DecidedByUserID: "user-c"},
	}
	byLabel := func(id string) *foundryv1.SettleFoundryReviewLabelRequest {
		return &foundryv1.SettleFoundryReviewLabelRequest{WorkspaceId: "workspace-a", OntologyId: "o", Verdict: "right", Drawn: "recheck",
			Subject: &foundryv1.SettleFoundryReviewLabelRequest_LabelId{LabelId: id}}
	}
	// The label shown, l-1, whatever else was said of the fact since.
	if _, err := s.SettleFoundryReviewLabel(ctx, byLabel("l-1")); err != nil {
		t.Fatal(err)
	}
	got := db.labels[3]
	if got.Drawn != foundrymodel.FoundryLabelRecheck || *got.Against != wrong || *got.FactKey != key || *got.Link != link ||
		*got.Confidence != confidence || got.Span != span || got.DecisionID != nil {
		t.Fatalf("%+v", got)
	}
	for id, code := range map[string]codes.Code{"l-3": codes.FailedPrecondition, "missing": codes.NotFound, strings.Repeat("x", 27): codes.InvalidArgument} {
		if _, err := s.SettleFoundryReviewLabel(ctx, byLabel(id)); status.Code(err) != code {
			t.Fatalf("%s: %v", id, err)
		}
	}
	// Not one's own label, nor by one who labelled the claim at random.
	db.labels[0].DecidedByUserID = "user-a"
	if _, err := s.SettleFoundryReviewLabel(ctx, byLabel("l-1")); status.Code(err) != codes.FailedPrecondition {
		t.Fatal(err)
	}
	db.labels[0].DecidedByUserID = "user-b"
	db.labels = append(db.labels, &foundrymodel.FoundryReviewLabel{ID: "l-9", OntologyID: "o", LoadID: "load-1", Drawn: foundrymodel.FoundryLabelRandom,
		DocID: doc, Span: span, FactKey: &key, Verdict: right, DecidedByUserID: "user-a"})
	if _, err := s.SettleFoundryReviewLabel(ctx, byLabel("l-1")); status.Code(err) != codes.FailedPrecondition {
		t.Fatal(err)
	}
}

// A held claim someone decided, drawn at random, is labelled from its review
// in the current load: its key and place, the hash of what it states, its
// link and score. Its decision is left as it is.
func TestLabelDecidedHeldClaim(t *testing.T) {
	ctx := ports.WithActor(context.Background(), reviewer)
	request := func(verdict string) *foundryv1.SettleFoundryReviewLabelRequest {
		return &foundryv1.SettleFoundryReviewLabelRequest{WorkspaceId: "workspace-a", OntologyId: "o", Verdict: verdict, Drawn: "random",
			Subject: &foundryv1.SettleFoundryReviewLabelRequest_ReviewId{ReviewId: "review-1"}}
	}
	s, db, queries := reviewService(t, heldScored("conflict", "0.91"))
	if _, err := s.SettleFoundryReviewLabel(ctx, request("wrong")); err != nil {
		t.Fatal(err)
	}
	l := db.labels[0]
	if l.Drawn != foundrymodel.FoundryLabelRandom || l.Verdict != foundrymodel.FoundryVerdictWrong || *l.ClaimKey != testClaimKey ||
		*l.StatementHash != testRead || l.Span != (foundrymodel.FoundryClaimSpan{Section: 2, Start: 10, End: 25}) || *l.Link != "promised_to" ||
		*l.Confidence != 0.91 || l.DecisionID != nil || len(db.saved) != 0 {
		t.Fatalf("%+v saved %v", l, db.saved)
	}
	if !strings.Contains((*queries)[0], `"review-1"`) || !strings.Contains((*queries)[0], `"load-1"`) {
		t.Fatalf("read %s", (*queries)[0])
	}
	// Not a review of a held claim, none at all, or one with no statement.
	for name, review := range map[string]string{
		"name":      `{"items":[{"reason":"unresolved","type":"Customer","doc_id":"doc-a","norm_name":"variant"}]}`,
		"none":      `{"items":[]}`,
		"statement": `{"items":[{"reason":"low_confidence","doc_id":"doc-a","claim_key":"` + testClaimKey + `","section":2,"start":10,"end":25,"suggested_link":"promised_to"}]}`,
	} {
		s, db, _ := reviewService(t, review)
		if _, err := s.SettleFoundryReviewLabel(ctx, request("right")); err == nil || len(db.labels) != 0 {
			t.Fatalf("%s: %v %+v", name, err, db.labels)
		}
	}
}

// Labels list a hundred a page, by id.
func TestListFoundryReviewLabels(t *testing.T) {
	s, db, _ := labelService(t, factQuotes)
	key := "fk-1"
	for i := 0; i < 101; i++ {
		db.labels = append(db.labels, &foundrymodel.FoundryReviewLabel{ID: strings.Repeat("0", 24) + string(rune('A'+i/26)) + string(rune('A'+i%26)), OntologyID: "o",
			LoadID: "load-1", Drawn: foundrymodel.FoundryLabelRandom, DocID: "doc-a", Span: foundrymodel.FoundryClaimSpan{Start: uint32(i), End: uint32(i + 1)},
			FactKey: &key, Verdict: foundrymodel.FoundryVerdictRight, DecidedByUserID: "user-b", DecidedAt: time.Unix(1, 0)})
	}
	out, err := s.ListFoundryReviewLabels(ports.WithActor(context.Background(), reviewer),
		&foundryv1.ListFoundryReviewLabelsRequest{WorkspaceId: "workspace-a", OntologyId: "o"})
	if err != nil || len(out.Labels) != 100 || out.NextPageToken != db.labels[99].ID || out.Labels[0].GetFactKey() != "fk-1" {
		t.Fatalf("%v %d %q", err, len(out.Labels), out.NextPageToken)
	}
	if _, err := s.ListFoundryReviewLabels(context.Background(), &foundryv1.ListFoundryReviewLabelsRequest{WorkspaceId: "workspace-a", OntologyId: "o",
		PageToken: strings.Repeat("x", 27)}); status.Code(err) != codes.InvalidArgument {
		t.Fatal(err)
	}
	if _, err := s.ListFoundryReviewLabels(context.Background(), &foundryv1.ListFoundryReviewLabelsRequest{WorkspaceId: "workspace-b", OntologyId: "o"}); status.Code(err) != codes.NotFound {
		t.Fatal(err)
	}
}
