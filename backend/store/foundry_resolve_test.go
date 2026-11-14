package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/pkg/namewords"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resolveOf is a resolve of name in doc, to entity or "not in data".
func (f resendFixture) resolveOf(tag, doc, name string, entity *string) *model.FoundryReviewDecision {
	return &model.FoundryReviewDecision{ID: id26("RV-" + tag), OntologyID: f.ontology.ID, Kind: model.FoundryReviewResolve, Type: "Customer",
		DocID: &doc, NormName: &name, EntityKey: entity, DecidedByUserID: id26("user")}
}

// settleName settles d against the current load as resend job RN-<tag>,
// owing asking besides its own document.
func (f resendFixture) settleName(t *testing.T, tag string, d *model.FoundryReviewDecision, asking ...string) bool {
	t.Helper()
	queued, err := f.db.SettleFoundryResolve(context.Background(), f.ontology.WorkspaceID, f.load, d, asking,
		&model.FoundryGraphJob{ID: id26("RN-" + tag), RequestedByUserID: ptr(id26("user"))})
	require.NoError(t, err)
	return queued
}

// owedFor lists the load's owed requests by document and reason.
func (f resendFixture) owedFor(t *testing.T) map[string]int {
	t.Helper()
	rows, err := f.db.ListFoundryLoadResends(context.Background(), f.load)
	require.NoError(t, err)
	out := map[string]int{}
	for _, r := range rows {
		out[r.DocumentID+"/"+string(r.Reason)] = r.Requests
	}
	return out
}

// A person's resolve joins the current load's list at once, pending, with
// its name's key, and queues a resend of its document and of the documents
// whose reviews ask about the name, owed for a name. The engine's word on
// the decision's own document is recorded on the copy; an alias's word on
// another document is the document's alone. Chosen again, the copy is the
// new choice and waits again; another load's list is untouched.
func TestSettleFoundryResolveAppliesAtOnce(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RSV")
	acme, globex := "acme", "globex"
	d := f.resolveOf("1", f.a, "acme corp", &acme)
	require.True(t, f.settleName(t, "1", d, f.b, f.a), "the first settle queues the job")
	assert.Equal(t, model.FoundryReleasePending, d.Applied.Status, "the answer says it waits")
	job := f.liveJob(t)
	require.NotNil(t, job)
	assert.Equal(t, model.FoundryGraphJobResend, job.Kind)
	assert.Equal(t, map[string]int{f.a + "/name": 1, f.b + "/name": 1}, f.owedFor(t))
	assert.Equal(t, map[string]string{d.ID: "pending"}, f.applied(t, d))
	var key sql.NullInt64
	require.NoError(t, testDB.QueryRow(`SELECT name_key FROM foundry_load_decisions WHERE load_id=? AND id=?`, f.load, d.ID).Scan(&key))
	want, _ := namewords.Key("acme corp")
	assert.Equal(t, sql.NullInt64{Int64: want, Valid: true}, key, "found by the Extracts of the documents that write it")
	list, err := f.db.ListFoundryLoadDecisions(ctx, f.load, "", []string{f.a}, 100)
	require.NoError(t, err)
	require.Len(t, list, 1, "the document's Extract sends it")

	// Chosen again before the job: one copy, the new entity, waiting.
	again := f.resolveOf("2", f.a, "acme corp", &globex)
	require.False(t, f.settleName(t, "2", again), "it joins the queued job")
	assert.Equal(t, d.ID, again.ID, "one decision a name of a document")
	var entity string
	var copies int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*), MAX(entity_key) FROM foundry_load_decisions WHERE load_id=? AND kind='resolve' AND doc_id=?`,
		f.load, f.a).Scan(&copies, &entity))
	assert.Equal(t, 1, copies)
	assert.Equal(t, "globex", entity)
	assert.Equal(t, map[string]int{f.a + "/name": 2, f.b + "/name": 1}, f.owedFor(t))

	claimed := f.claim(t)
	require.Equal(t, job.ID, claimed.ID)
	id := d.ID
	// The decision's own document: written. The other: what an alias did
	// there is the document's, and the copy is left as it is.
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, claimed.ID, f.worker, &model.FoundryDocumentExtraction{
		DocumentID: f.b, Status: model.FoundryExtractionExtracted,
		Names: []model.FoundryDocumentName{{Decision: model.FoundryNameAlias, Type: "Customer", NormName: "acme corp", DecisionID: &id, Outcome: model.FoundryNameWritten}},
	}, true))
	assert.Equal(t, map[string]string{d.ID: "pending"}, f.applied(t, d))
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, claimed.ID, f.worker, &model.FoundryDocumentExtraction{
		DocumentID: f.a, Status: model.FoundryExtractionExtracted,
		Names: []model.FoundryDocumentName{{Decision: model.FoundryNameResolve, Type: "Customer", NormName: "acme corp", DecisionID: &id, Outcome: model.FoundryNameWritten}},
	}, true))
	assert.Equal(t, map[string]string{d.ID: "written"}, f.applied(t, d))
	for _, doc := range []string{f.a, f.b} {
		require.NoError(t, f.db.EndFoundryLoadResend(ctx, claimed.ID, f.worker, model.FoundryResendEnd{
			DocumentID: doc, Reason: model.FoundryResendName, Requests: map[string]int{f.a: 2, f.b: 1}[doc]}))
	}
	assert.Empty(t, f.owedFor(t))
	// A later word on it, as a resend's not_replayed, changes nothing.
	require.NoError(t, f.db.RecordFoundryReleaseOutcomes(ctx, claimed.ID, f.worker, f.a, nil,
		[]model.FoundryDocumentName{{Decision: model.FoundryNameResolve, Type: "Customer", NormName: "acme corp", DecisionID: &id,
			Outcome: model.FoundryNameRefused, Reason: model.FoundryNameNotReplayed}}))
	assert.Equal(t, map[string]string{d.ID: "written"}, f.applied(t, d))

	_, err = f.db.SettleFoundryResolve(ctx, f.ontology.WorkspaceID, id26("FJ-other"), f.resolveOf("3", f.b, "helio", nil), nil,
		&model.FoundryGraphJob{ID: id26("RN-3")})
	assert.ErrorIs(t, err, store.ErrConflict, "only the current load")
	_, err = f.db.SettleFoundryResolve(ctx, f.ontology.WorkspaceID, f.load, f.held(f.a, 0, readA), nil, &model.FoundryGraphJob{ID: id26("RN-4")})
	assert.Error(t, err, "a release is not a resolve")
}

// What the engine says of a resolve on its own document, as the copy keeps
// it: refused with its refusal, not_replayed among them (a load read before
// prompt 9), not_asked and not_named as their own reasons. A document the
// engine sent no event for refuses the document's waiting resolves, and not
// its approvals; a job that fails leaves them unconfirmed.
func TestFoundryResolveOutcomes(t *testing.T) {
	ctx := context.Background()
	acme := "acme"
	for i, c := range []struct {
		name string
		said model.FoundryDocumentName
		want string
	}{
		{"not replayed", model.FoundryDocumentName{Outcome: model.FoundryNameRefused, Reason: model.FoundryNameNotReplayed}, "refused/not_replayed"},
		{"two rows", model.FoundryDocumentName{Outcome: model.FoundryNameRefused, Reason: model.FoundryNameTwoRows}, "refused/two_rows"},
		{"not asked", model.FoundryDocumentName{Outcome: model.FoundryNameNotAsked}, "refused/not_asked"},
		{"not named", model.FoundryDocumentName{Outcome: model.FoundryNameNotNamed}, "refused/not_named"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newResendFixture(t, fmt.Sprintf("RO%d", i))
			d := f.resolveOf("1", f.a, "acme corp", &acme)
			f.settleName(t, "1", d)
			job := f.claim(t)
			said := c.said
			said.Decision, said.Type, said.NormName, said.DecisionID = model.FoundryNameResolve, "Customer", "acme corp", &d.ID
			require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, &model.FoundryDocumentExtraction{
				DocumentID: f.a, Status: model.FoundryExtractionExtracted, Names: []model.FoundryDocumentName{said},
			}, true))
			assert.Equal(t, map[string]string{d.ID: c.want}, f.applied(t, d))
		})
	}

	t.Run("no event", func(t *testing.T) {
		f := newResendFixture(t, "RONONE")
		d := f.resolveOf("1", f.a, "acme corp", &acme)
		approval := f.held(f.a, 0, readA)
		f.approve(t, "1", approval)
		f.settleName(t, "1", d)
		job := f.claim(t)
		require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{
			DocumentID: f.a, Reason: model.FoundryResendName, Requests: 1, Refused: model.FoundryReleaseNotReplayed}))
		assert.Equal(t, map[string]string{d.ID: "refused/not_replayed", approval.ID: "pending"}, f.applied(t, d, approval),
			"a name's request refuses its resolves alone")
	})

	t.Run("a failed job", func(t *testing.T) {
		f := newResendFixture(t, "ROFAIL")
		d := f.resolveOf("1", f.a, "acme corp", &acme)
		f.settleName(t, "1", d)
		job := f.claim(t)
		f.end(t, job, "fail", model.FoundryGraphJobFailed)
		got, err := f.db.ListFoundryDecisionsApplied(ctx, f.ontology.ID, []string{d.ID})
		require.NoError(t, err)
		assert.True(t, got[d.ID].Unconfirmed(), "%+v", got[d.ID])
		assert.Empty(t, f.owedFor(t))
	})
}

// A resolve taken back while its copy waits is not sent; one the engine has
// its word on stays in the load, as what it wrote stays. A resolve the load
// was created with is listed with no status, and one chosen again since is
// not listed as that decision.
func TestFoundryResolveRemovedAndListed(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RSDEL")
	acme := "acme"
	waiting := f.resolveOf("1", f.a, "acme corp", &acme)
	f.settleName(t, "1", waiting)
	require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, waiting.ID, &model.FoundryGraphJob{ID: id26("RD-1")}))
	assert.Empty(t, f.applied(t, waiting), "not sent")

	done := f.resolveOf("2", f.b, "helio", &acme)
	f.settleName(t, "2", done)
	job := f.claim(t)
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, &model.FoundryDocumentExtraction{
		DocumentID: f.b, Status: model.FoundryExtractionExtracted,
		Names: []model.FoundryDocumentName{{Decision: model.FoundryNameResolve, Type: "Customer", NormName: "helio", DecisionID: &done.ID, Outcome: model.FoundryNameWritten}},
	}, true))
	require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, done.ID, &model.FoundryGraphJob{ID: id26("RD-2")}))
	var copies int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_decisions WHERE load_id=? AND id=?`, f.load, done.ID).Scan(&copies))
	assert.Equal(t, 1, copies, "what it wrote stays")

	// A copy the load was created with, and one from another time.
	created := f.resolveOf("3", f.a, "initech", &acme)
	addLoadResolve(t, f, f.load, "RV-3", "resolve", f.a, "initech", "acme", time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), true)
	_, err := testDB.Exec(`INSERT INTO foundry_review_decisions (id, ontology_id, kind, type, doc_id, norm_name, entity_key, decided_by_user_id, decided_at)
SELECT id, ontology_id, kind, type, doc_id, norm_name, entity_key, decided_by_user_id, decided_at FROM foundry_load_decisions WHERE load_id=? AND id=?`,
		f.load, created.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{created.ID: ""}, f.applied(t, created))
	_, err = testDB.Exec(`UPDATE foundry_review_decisions SET decided_at=NOW(6) + INTERVAL 1 SECOND WHERE ontology_id=? AND id=?`, f.ontology.ID, created.ID)
	require.NoError(t, err)
	assert.Empty(t, f.applied(t, created), "chosen again since: not this load's copy")
}
