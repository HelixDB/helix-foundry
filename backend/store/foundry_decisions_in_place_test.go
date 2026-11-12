package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// copies is what a load's list holds of every decision, sent or not, by id:
// its status, with its reason after a slash.
func (f resendFixture) copies(t *testing.T, loadID string) map[string]string {
	t.Helper()
	rows, err := testDB.Query(`SELECT id, COALESCE(applied_status, ''), COALESCE(applied_reason, '') FROM foundry_load_decisions WHERE load_id=?`, loadID)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, status, reason string
		require.NoError(t, rows.Scan(&id, &status, &reason))
		out[id] = status
		if reason != "" {
			out[id] += "/" + reason
		}
	}
	require.NoError(t, rows.Err())
	return out
}

// withheld reports whether a servable load's value decisions keep it from a
// reader who doesn't cover the documents they were decided from: the sets
// those documents are in, as the access rules read them from the load's list.
func (f resendFixture) withheld(t *testing.T, loadID string) []access.SetRef {
	t.Helper()
	in, err := f.db.ListFoundryLoadInputs(context.Background(), f.ontology.WorkspaceID, []string{loadID})
	require.NoError(t, err)
	require.Contains(t, in, loadID)
	require.False(t, in[loadID].Inputs.Uncoverable, "every decision here records its documents")
	return in[loadID].Inputs.DecisionSets
}

// decisionDocuments are the documents a decision was decided from.
func decisionDocuments(t *testing.T, decisionID string) []string {
	t.Helper()
	rows, err := testDB.Query(`SELECT document_id FROM foundry_review_decision_documents WHERE decision_id=? ORDER BY document_id`, decisionID)
	require.NoError(t, err)
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

// The engine keeps a decision once, by its id: sent an id it holds with
// another value, it sets nothing. So a property decided again with another
// value is a new decision, the earlier one goes from the ontology and from
// the load's list, and the new one keeps the documents the earlier one was
// decided from. The same value chosen again is the decision that stands,
// with its id, its time and who decided it, and is only sent again. Two
// people who decide a property one after the other leave one decision, the
// second's.
func TestFoundryValueDecidedAgain(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "VAGAIN")
	decided := func(id string) (at time.Time, by string) {
		require.NoError(t, testDB.QueryRow(`SELECT decided_at, decided_by_user_id FROM foundry_review_decisions WHERE id=?`, id).Scan(&at, &by))
		return at, by
	}
	apply := func(tag string) {
		t.Helper()
		job := f.claim(t)
		require.Equal(t, model.FoundryGraphJobResend, job.Kind)
		f.carry(t, job, f.requests(t, f.load))
		require.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, tag, model.FoundryGraphJobSucceeded))
		require.Nil(t, f.liveJob(t))
	}

	d := f.value("1", "acme", "gold")
	d.DocumentIDs = []string{f.a}
	require.True(t, f.decide(t, "1", d))
	apply("first")
	require.Equal(t, map[string]string{d.ID: "applied"}, f.applied(t, d))

	// Another value: a new id, and the old one is in neither list.
	again := f.value("2", "acme", "platinum")
	again.DocumentIDs = []string{f.b}
	require.True(t, f.decide(t, "2", again))
	assert.Equal(t, id26("VD-2"), again.ID)
	assert.Equal(t, []string{again.ID}, f.decisionIDs(t), "the ontology holds the new decision alone")
	assert.Equal(t, map[string]string{again.ID: "pending"}, f.copies(t, f.load), "and so does the load's list")
	assert.Equal(t, map[string]string{"acme": "platinum"}, f.values(t, f.load))
	assert.Equal(t, map[string]string{again.ID: "pending"}, f.applied(t, d, again))
	assert.Equal(t, slices.Sorted(slices.Values([]string{f.a, f.b})), decisionDocuments(t, again.ID),
		"it keeps what the earlier value was decided from")
	apply("changed")
	assert.Equal(t, map[string]string{again.ID: "applied"}, f.applied(t, again))

	// The same value again, by someone else: nothing about the decision moves.
	at, by := decided(again.ID)
	same := f.value("3", "acme", "platinum")
	same.DecidedByUserID = id26("someone-else")
	require.True(t, f.decide(t, "3", same))
	assert.Equal(t, again.ID, same.ID)
	nowAt, nowBy := decided(again.ID)
	assert.True(t, at.Equal(nowAt), "decided at %v, now %v", at, nowAt)
	assert.Equal(t, by, nowBy)
	assert.Equal(t, map[string]string{again.ID: "pending"}, f.applied(t, again), "it is sent again")
	assert.Equal(t, []string{again.ID}, f.decisionIDs(t))
	apply("same")
	assert.Equal(t, map[string]string{again.ID: "applied"}, f.applied(t, again))

	// Two people, one after the other, before the job runs: one job, and
	// the second's decision is the one the load is sent.
	one := f.value("4", "acme", "silver")
	require.True(t, f.decide(t, "4", one))
	two := f.value("5", "acme", "bronze")
	two.DecidedByUserID = id26("someone-else")
	require.False(t, f.decide(t, "5", two), "the queued job takes it")
	assert.NotEqual(t, one.ID, two.ID)
	assert.Equal(t, []string{two.ID}, f.decisionIDs(t))
	assert.Equal(t, map[string]string{two.ID: "pending"}, f.copies(t, f.load))
	assert.Equal(t, map[string]string{"acme": "bronze"}, f.values(t, f.load))
	apply("two")
	assert.Equal(t, map[string]string{two.ID: "applied"}, f.applied(t, one, two))

	// A load that read another value under this id, as before a changed
	// value had a new id: the engine holds that value under the id, so the
	// load says nothing of the decision, the carry sends nothing in its
	// name, and choosing the value there is a new decision.
	_, err := testDB.Exec(`UPDATE foundry_load_decisions SET value=CAST('"gold"' AS JSON), decided_at=decided_at - INTERVAL 1 DAY
WHERE load_id=? AND id=?`, f.load, two.ID)
	require.NoError(t, err)
	assert.Empty(t, f.applied(t, two), "the load's copy is from another time")
	c := f.uploaded(t, "VAGAIN-c")
	job := f.claim(t)
	require.Equal(t, model.FoundryGraphJobExtract, job.Kind)
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(c), false))
	require.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "legacy", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t), "the job's end carries nothing under an id the engine holds")
	assert.Equal(t, map[string]string{"acme": "gold"}, f.values(t, f.load))
	here := f.value("6", "acme", "bronze")
	require.True(t, f.decide(t, "6", here))
	assert.Equal(t, id26("VD-6"), here.ID, "the same value, and still a new decision for this load")
	assert.Equal(t, []string{here.ID}, f.decisionIDs(t))
	assert.Equal(t, map[string]string{"acme": "bronze"}, f.values(t, f.load))
	apply("legacy-again")
	assert.Equal(t, map[string]string{here.ID: "applied"}, f.applied(t, here))
}

// A value decision taken back stays in the load's list, marked removed,
// until an Extract without it has ended. No Extract sends it, so the engine
// withdraws the value; until that is known the load still holds it, and the
// property stays withheld from a reader who doesn't cover the documents it
// was decided from. A resend that fails leaves the mark: the failed job is
// followed by none, and the end of the next job sends the list again, so
// the withdrawal is never dropped.
func TestFoundryValueRemovedIsWithheldUntilTheLoadIsTold(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "VHELD")
	d := f.value("1", "acme", "gold")
	d.DocumentIDs = []string{f.a}
	require.True(t, f.decide(t, "1", d))
	job := f.claim(t)
	f.carry(t, job, 1)
	f.end(t, job, "applied", model.FoundryGraphJobSucceeded)
	require.Len(t, f.withheld(t, f.load), 1, "the set of the document it was decided from")

	f.remove(t, "1", d)
	assert.Empty(t, f.decisionIDs(t), "the decision is gone")
	assert.Empty(t, f.values(t, f.load), "and no Extract sends it")
	assert.Equal(t, map[string]string{d.ID: "removed"}, f.copies(t, f.load))
	assert.Len(t, f.withheld(t, f.load), 1, "the load still holds the value")
	assert.Empty(t, f.applied(t, d))

	// The job that was to tell the load fails for good.
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, f.claim(t), "failed", model.FoundryGraphJobFailed))
	assert.Equal(t, map[string]string{d.ID: "removed/job_failed"}, f.copies(t, f.load))
	assert.Len(t, f.withheld(t, f.load), 1)
	assert.Zero(t, f.requests(t, f.load))
	assert.Nil(t, f.liveJob(t), "the failed job is followed by no other")

	// The next job's end owes the list again; that resend fails too, and is
	// followed by none either.
	c := f.uploaded(t, "VHELD-c")
	job = f.claim(t)
	require.Equal(t, model.FoundryGraphJobExtract, job.Kind)
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(c), false))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "up", model.FoundryGraphJobSucceeded))
	resend := f.liveJob(t)
	require.NotNil(t, resend, "the extract's end sends the list again")
	require.Equal(t, model.FoundryGraphJobResend, resend.Kind)
	assert.Equal(t, map[string]string{d.ID: "removed"}, f.copies(t, f.load))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, f.claim(t), "failed2", model.FoundryGraphJobFailed))
	assert.Equal(t, map[string]string{d.ID: "removed/job_failed"}, f.copies(t, f.load))
	assert.Nil(t, f.liveJob(t))

	assert.Len(t, f.withheld(t, f.load), 1)

	// The job after that owes it once more, and this resend's Extract ends:
	// it carried a list without the removed one.
	e := f.uploaded(t, "VHELD-e")
	job = f.claim(t)
	require.Equal(t, model.FoundryGraphJobExtract, job.Kind)
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(e), false))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "up2", model.FoundryGraphJobSucceeded))
	job = f.claim(t)
	require.Equal(t, model.FoundryGraphJobResend, job.Kind)
	f.carry(t, job, f.requests(t, f.load))
	assert.Empty(t, f.copies(t, f.load), "the load was told: the copy goes")
	assert.Empty(t, f.withheld(t, f.load), "and the property is withheld no longer")
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "told", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))
}

// A removal reaches every copy of the property's decision: a load that is
// not current may hold it under the id it had before its value changed, and
// a load being written copied it when it was created. Each is marked, owes
// its list, and is told when it is the current load. A property decided
// anew meanwhile takes the mark's place, with the documents it stood for.
func TestFoundryValueRemovedReachesEveryCopyOfItsProperty(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "VEVERY")
	first := f.load
	x := f.value("1", "acme", "gold")
	x.DocumentIDs = []string{f.a}
	require.True(t, f.decide(t, "1", x))
	job := f.claim(t)
	f.carry(t, job, 1)
	f.end(t, job, "first", model.FoundryGraphJobSucceeded)

	// A second load copies it and becomes current; there the value changes.
	f.draftVersion(t, "VEVERY2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "VEVERY2")))
	f.loaded(t, f.worker, f.a, f.b)
	second := id26("FJ-VEVERY2")
	f.load = second
	y := f.value("2", "acme", "platinum")
	require.True(t, f.decide(t, "2", y))
	job = f.claim(t)
	f.carry(t, job, f.requests(t, second))
	f.end(t, job, "second", model.FoundryGraphJobSucceeded)
	require.Equal(t, map[string]string{x.ID: "applied"}, f.copies(t, first), "the first load holds it under its earlier id")
	require.Equal(t, map[string]string{y.ID: "applied"}, f.copies(t, second))

	// A third load is being written when the decision is removed.
	f.draftVersion(t, "VEVERY3", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "VEVERY3")))
	third := id26("FJ-VEVERY3")
	require.Equal(t, map[string]string{y.ID: ""}, f.copies(t, third))
	f.remove(t, "1", y)
	assert.Equal(t, map[string]string{x.ID: "removed"}, f.copies(t, first))
	assert.Equal(t, map[string]string{y.ID: "removed"}, f.copies(t, second))
	assert.Equal(t, map[string]string{y.ID: "removed"}, f.copies(t, third))
	for _, load := range []string{first, second, third} {
		assert.Empty(t, f.values(t, load), "no load sends it")
		assert.NotZero(t, f.requests(t, load), "and each owes its list")
	}
	for _, load := range []string{first, second} {
		assert.Len(t, f.withheld(t, load), 1, "while each may still hold the value")
	}

	// The third load becomes current and is told.
	f.loaded(t, f.worker, f.a, f.b)
	require.Equal(t, third, *f.ontologyNow(t).CurrentLoadID)
	f.load = third
	job = f.claim(t)
	require.Equal(t, model.FoundryGraphJobResend, job.Kind)
	f.carry(t, job, f.requests(t, third))
	assert.Empty(t, f.copies(t, third))
	assert.Empty(t, f.withheld(t, third))
	f.end(t, job, "third", model.FoundryGraphJobSucceeded)
	require.Nil(t, f.liveJob(t))

	// The property is decided anew on the third load; the first, made
	// current, gives up its mark for the new decision, which keeps the
	// documents the removed one was decided from.
	z := f.value("3", "acme", "silver")
	require.True(t, f.decide(t, "3", z))
	job = f.claim(t)
	f.carry(t, job, f.requests(t, third))
	f.end(t, job, "anew", model.FoundryGraphJobSucceeded)
	_, err := f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, extractJob("VEVERY"), resendJob("VEVERY"), noBudget)
	require.NoError(t, err)
	f.load = first
	assert.Equal(t, map[string]string{z.ID: "carried"}, f.copies(t, first))
	assert.Equal(t, []string{f.a}, decisionDocuments(t, z.ID))
	assert.Len(t, f.withheld(t, first), 1)
	job = f.claim(t)
	require.Equal(t, id26("FR-VEVERY"), job.ID)
	f.carry(t, job, f.requests(t, first))
	assert.Equal(t, map[string]string{z.ID: "applied"}, f.copies(t, first))
}

// The carry that takes a withdrawn approval's copy from a load made current
// again also brings that load the value decisions it lacks, and neither
// touches the other's rows: the approval's copy goes, the value the load
// applied stays applied, the one that waited there still waits, and the one
// decided on the other load is carried. One job sends the document the
// withdrawn approval owed, with nothing to release, and the list.
func TestFoundryWithdrawnApprovalGoesAsValuesAreCarried(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "VSWAP")
	first := f.load
	kept := f.value("1", "acme", "gold")
	require.True(t, f.decide(t, "1", kept))
	job := f.claim(t)
	f.carry(t, job, 1)
	f.end(t, job, "kept", model.FoundryGraphJobSucceeded)

	// While a second load is written, a claim is approved and a value chosen
	// on the first: both wait there.
	f.draftVersion(t, "VSWAP2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "VSWAP2")))
	x := f.held(f.a, 0, readA)
	require.False(t, f.approve(t, "1", x), "the load job goes on")
	late := f.value("2", "globex", "silver")
	require.False(t, f.decide(t, "2", late))
	f.loaded(t, f.worker, f.a, f.b)
	second := id26("FJ-VSWAP2")
	f.load = second
	require.Equal(t, map[string]string{kept.ID: "", x.ID: "carried", late.ID: "carried"}, f.copies(t, second))
	job = f.claim(t)
	require.Equal(t, model.FoundryGraphJobResend, job.Kind)
	owed, err := f.db.ListFoundryLoadResends(ctx, second)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	f.sent(t, job, owed[0], 0, refused(x, "conflict"))
	f.carry(t, job, f.requests(t, second))
	f.end(t, job, "second", model.FoundryGraphJobSucceeded)
	require.Equal(t, map[string]string{kept.ID: "", x.ID: "refused/conflict", late.ID: "applied"}, f.copies(t, second))

	// On the second load the approval is taken back and another value chosen.
	require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, x.ID, nil))
	another := f.value("3", "initech", "bronze")
	require.True(t, f.decide(t, "3", another))
	job = f.claim(t)
	f.carry(t, job, f.requests(t, second))
	f.end(t, job, "another", model.FoundryGraphJobSucceeded)
	require.Equal(t, map[string]string{kept.ID: "applied", x.ID: "pending", late.ID: "pending"}, f.copies(t, first),
		"removal reached only the load that was current")

	_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, extractJob("VSWAP"), resendJob("VSWAP"), noBudget)
	require.NoError(t, err)
	f.load = first
	assert.Equal(t, map[string]string{kept.ID: "applied", late.ID: "pending", another.ID: "carried"}, f.copies(t, first))
	assert.Equal(t, map[string]string{"acme": "gold", "globex": "silver", "initech": "bronze"}, f.values(t, first))
	job = f.claim(t)
	require.Equal(t, id26("FR-VSWAP"), job.ID)
	owed, err = f.db.ListFoundryLoadResends(ctx, first)
	require.NoError(t, err)
	require.Len(t, owed, 1, "the document the withdrawn approval owed")
	assert.Equal(t, f.a, owed[0].DocumentID)
	f.sent(t, job, owed[0], 0)
	f.carry(t, job, f.requests(t, first))
	assert.Equal(t, map[string]string{kept.ID: "applied", late.ID: "applied", another.ID: "applied"}, f.copies(t, first))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "back", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))
}

// A load made current by a person never read the value decisions saved
// since it copied the ontology's: they are carried to it there and then, and
// one resend, of no document, sends it its list.
func TestFoundryValueCarriedWhenALoadIsMadeCurrent(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "VCUR")
	first := f.load
	f.draftVersion(t, "VCUR2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "VCUR2")))
	f.loaded(t, f.worker, f.a, f.b)
	second := id26("FJ-VCUR2")
	f.load = second
	v := f.value("1", "acme", "gold")
	require.True(t, f.decide(t, "1", v))
	job := f.claim(t)
	f.carry(t, job, 1)
	f.end(t, job, "second", model.FoundryGraphJobSucceeded)
	require.Nil(t, f.liveJob(t))

	queued, err := f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, extractJob("VCUR"),
		&model.FoundryGraphJob{ID: id26("FR-VCUR"), RequestedByUserID: ptr(id26("user"))}, noBudget)
	require.NoError(t, err)
	assert.False(t, queued, "no document waits for an extract")
	f.load = first
	assert.Equal(t, map[string]string{v.ID: "carried"}, f.applied(t, v))
	assert.Equal(t, 1, f.requests(t, first))
	assert.Empty(t, f.owed(t), "no document is owed")
	resend := f.liveJob(t)
	require.NotNil(t, resend, "the change of current load queues the resend")
	assert.Equal(t, id26("FR-VCUR"), resend.ID)
	assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
	assert.Equal(t, first, resend.LoadID)
	job = f.claim(t)
	f.carry(t, job, 1)
	assert.Equal(t, map[string]string{v.ID: "applied"}, f.applied(t, v))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "first", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))

	// A load with nothing to carry queues nothing.
	_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, second, extractJob("VCURb"), resendJob("VCURb"), noBudget)
	require.NoError(t, err)
	assert.Nil(t, f.liveJob(t))
}

// A purge of enriched profiles makes its clean load current. That load
// copied its decisions when it was created: a value chosen since is carried
// to it there and then, and its resend, which names no document, goes
// before the delete of the load that applied the profiles.
func TestFoundryValueCarriedWhenAPurgeMakesItsLoadCurrent(t *testing.T) {
	ctx := context.Background()
	base := newFoundryFixture(t, "VPURGE")
	a, b := base.uploaded(t, "VPURGE-a"), base.uploaded(t, "VPURGE-b")
	base.draftVersion(t, "VPURGE", customerTOML)
	require.NoError(t, base.db.CreateFoundryLoad(ctx, loadJob(base.ontology, "VPURGE")))
	f := resendFixture{foundryFixture: base, load: id26("FJ-VPURGE"), a: a, b: b, worker: "w-VPURGE"}
	// loads claims the queued load job, has it apply profiles (or none),
	// records both documents and ends it with status.
	loads := func(prepared model.FoundryEnrichmentPrepared, status model.FoundryGraphJobStatus) *model.FoundryGraphJob {
		job := f.claim(t)
		require.Equal(t, model.FoundryGraphJobLoad, job.Kind)
		_, err := f.db.StartFoundryLoadEnrichment(ctx, f.worker, job)
		require.NoError(t, err)
		require.NoError(t, f.db.EndPreparedFoundryLoadEnrichment(ctx, job.LoadID, f.worker, prepared))
		for _, doc := range []string{a, b} {
			require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(doc), false))
		}
		outcome := model.FoundryGraphJobOutcome{Status: status, IngestDone: true, Next: next("VPURGE-" + string(status))}
		if status == model.FoundryGraphJobBudgetExhausted {
			outcome.Error = "budget"
		}
		_, err = f.db.FinishFoundryGraphJob(ctx, job.ID, f.worker, outcome)
		require.NoError(t, err)
		return job
	}
	dirty := loads(model.FoundryEnrichmentPrepared{
		End: model.EndFoundryEnrichment(model.FoundryEnrichmentDone), CarriedKey: "c", ResultsKey: "r",
		Applied: model.FoundryEnrichmentApplied{Key: "c", SHA256: "cc"},
	}, model.FoundryGraphJobSucceeded)
	require.Equal(t, f.load, *f.ontologyNow(t).CurrentLoadID)

	// Consent is withdrawn, and the purge queues its clean load.
	require.NoError(t, f.db.SetFoundryEnrichmentConsent(ctx, consent(f.ontology.WorkspaceID, "off", false, 0)))
	step, err := f.db.AdvanceFoundryEnrichmentPurge(ctx, f.ontology.ID, &model.FoundryGraphJob{ID: id26("FJ-VPURGE-clean")}, id26("FN-VPURGE-1"),
		24*time.Hour, 3)
	require.NoError(t, err)
	require.Equal(t, model.FoundryEnrichmentPurgeQueued, step)

	// While it is written a value is chosen on the load still current. The
	// clean load runs out of budget, so it ends partial and is not made
	// current by its own end; the value is applied to the first load.
	v := f.value("1", "acme", "gold")
	require.False(t, f.decide(t, "1", v), "the load job goes on")
	clean := loads(model.FoundryEnrichmentPrepared{End: model.EndFoundryEnrichment(model.FoundryEnrichmentOff), Applied: model.FoundryEnrichmentNone},
		model.FoundryGraphJobBudgetExhausted)
	require.Equal(t, model.FoundryLoadPartial, f.loadStatus(t, clean.LoadID))
	require.Equal(t, dirty.LoadID, *f.ontologyNow(t).CurrentLoadID)
	job := f.claim(t)
	require.Equal(t, model.FoundryGraphJobResend, job.Kind)
	f.carry(t, job, f.requests(t, f.load))
	f.end(t, job, "dirty", model.FoundryGraphJobSucceeded)
	require.Equal(t, map[string]string{v.ID: "applied"}, f.applied(t, v))
	require.Nil(t, f.liveJob(t))

	// The purge makes the clean load current. It never read the value.
	step, err = f.db.AdvanceFoundryEnrichmentPurge(ctx, f.ontology.ID, &model.FoundryGraphJob{ID: id26("FJ-VPURGE-again")}, id26("FN-VPURGE-2"),
		24*time.Hour, 3)
	require.NoError(t, err)
	require.Equal(t, model.FoundryEnrichmentPurgeDone, step)
	require.Equal(t, clean.LoadID, *f.ontologyNow(t).CurrentLoadID)
	f.load = clean.LoadID
	assert.Equal(t, map[string]string{v.ID: "carried"}, f.applied(t, v), "carried as the load is made current")
	assert.Equal(t, 1, f.requests(t, f.load))
	assert.Empty(t, f.owed(t), "no document is owed")
	resend := f.liveJob(t)
	require.NotNil(t, resend)
	assert.Equal(t, id26("FN-VPURGE-2"), resend.ID, "the purge's next job is the resend, though it names no document")
	assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
	assert.Equal(t, clean.LoadID, resend.LoadID)
	assert.Equal(t, model.FoundryLoadDeleting, f.loadStatus(t, dirty.LoadID), "the load that applied purged profiles goes")

	job = f.claim(t)
	f.carry(t, job, 1)
	assert.Equal(t, map[string]string{v.ID: "applied"}, f.applied(t, v))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "clean", model.FoundryGraphJobSucceeded))
	deleting := f.liveJob(t)
	require.NotNil(t, deleting, "the resend's end queues the delete")
	assert.Equal(t, model.FoundryGraphJobDelete, deleting.Kind)
	assert.Equal(t, dirty.LoadID, deleting.LoadID)
}

// A carried value whose resend job failed is unconfirmed, as a carried
// approval is, and the failed job is followed by none. A later resend that
// sends a document and refuses or writes its approvals says nothing of the
// value; the end of that job owes the load its list again, and the resend
// that ends that request marks the value applied.
func TestFoundryCarriedValueWhoseJobFailed(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "VCFAIL")
	f.draftVersion(t, "VCFAIL2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "VCFAIL2")))
	v := f.value("1", "acme", "gold")
	require.False(t, f.decide(t, "1", v), "the load job goes on")
	f.loaded(t, f.worker, f.a, f.b)
	f.load = id26("FJ-VCFAIL2")
	require.Equal(t, map[string]string{v.ID: "carried"}, f.applied(t, v))

	job := f.claim(t)
	require.Equal(t, model.FoundryGraphJobResend, job.Kind)
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "cfail", model.FoundryGraphJobFailed))
	assert.Equal(t, map[string]string{v.ID: "carried/job_failed"}, f.applied(t, v))
	got, err := f.db.ListFoundryDecisionsApplied(ctx, f.ontology.ID, []string{v.ID})
	require.NoError(t, err)
	assert.True(t, got[v.ID].Unconfirmed())
	assert.Zero(t, f.requests(t, f.load))
	assert.Nil(t, f.liveJob(t), "the failed job's end owes it no other")

	// An approval's resend: the document's refusal is not the value's.
	x := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", x))
	assert.Zero(t, f.requests(t, f.load), "an approval's carry owes nothing for a value whose job failed")
	job = f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{
		DocumentID: owed[0].DocumentID, Reason: owed[0].Reason, Requests: owed[0].Requests, Refused: model.FoundryReleaseNotReplayed,
	}))
	assert.Equal(t, map[string]string{v.ID: "carried/job_failed", x.ID: "refused/not_replayed"}, f.applied(t, v, x))

	// That job's end owes the list again.
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "approval", model.FoundryGraphJobSucceeded))
	resend := f.liveJob(t)
	require.NotNil(t, resend)
	assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
	assert.Equal(t, map[string]string{v.ID: "carried"}, f.applied(t, v))
	job = f.claim(t)
	f.carry(t, job, f.requests(t, f.load))
	assert.Equal(t, map[string]string{v.ID: "applied"}, f.applied(t, v))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "sent", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))
}

// A resend job the binary before this one ran, as after a rollback: it reads
// the load's own request as a document that is gone, ends it unsent, and
// ends the job succeeded. The value goes on waiting with nothing owed. Back
// on this binary the next job's end owes the list again.
func TestFoundryValueRequestEndedByAnOlderBinary(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "VOLD")
	v := f.value("1", "acme", "gold")
	require.True(t, f.decide(t, "1", v))
	job := f.liveJob(t)
	require.NotNil(t, job)
	_, err := testDB.Exec(`UPDATE foundry_load_resends SET status='done', ended_at=NOW(6), job_id=? WHERE load_id=? AND document_id=''`, job.ID, f.load)
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_load_jobs SET status='succeeded', active_key=NULL, lease_owner=NULL, lease_until=NULL,
  finished_at=NOW(6) WHERE id=?`, job.ID)
	require.NoError(t, err)
	require.Nil(t, f.liveJob(t))
	require.Equal(t, map[string]string{v.ID: "pending"}, f.applied(t, v))
	require.Zero(t, f.requests(t, f.load))

	c := f.uploaded(t, "VOLD-c")
	job = f.claim(t)
	require.Equal(t, model.FoundryGraphJobExtract, job.Kind)
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(c), false))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "vold", model.FoundryGraphJobSucceeded))
	resend := f.liveJob(t)
	require.NotNil(t, resend, "the value that waited with nothing owed is owed again")
	assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
	assert.NotZero(t, f.requests(t, f.load))
	job = f.claim(t)
	f.carry(t, job, f.requests(t, f.load))
	assert.Equal(t, map[string]string{v.ID: "applied"}, f.applied(t, v))
}
