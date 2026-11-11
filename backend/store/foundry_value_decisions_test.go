package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// value is a decision that customer's tier is tier, of the fixture's ontology.
func (f resendFixture) value(tag, customer, tier string) *model.FoundryReviewDecision {
	raw, _ := json.Marshal(tier)
	return &model.FoundryReviewDecision{
		ID: id26("VD-" + tag), OntologyID: f.ontology.ID, Kind: model.FoundryReviewProp, Type: "Customer",
		EntityKey: &customer, Prop: ptr("tier"), Value: raw, DecidedByUserID: id26("user"),
	}
}

// decide settles a value against the fixture's load, as resend job RS-<tag>.
func (f resendFixture) decide(t *testing.T, tag string, d *model.FoundryReviewDecision) bool {
	t.Helper()
	queued, err := f.db.SettleFoundryValue(context.Background(), f.ontology.WorkspaceID, f.load, d,
		&model.FoundryGraphJob{ID: id26("RS-" + tag), RequestedByUserID: ptr(id26("user"))})
	require.NoError(t, err)
	return queued
}

// remove takes a decision back, as resend job RM-<tag>.
func (f resendFixture) remove(t *testing.T, tag string, d *model.FoundryReviewDecision) {
	t.Helper()
	require.NoError(t, f.db.DeleteFoundryReviewDecision(context.Background(), f.ontology.WorkspaceID, f.ontology.ID, d.ID,
		&model.FoundryGraphJob{ID: id26("RM-" + tag), RequestedByUserID: ptr(id26("user"))}))
}

// values are the tiers a load's list sends the engine, by customer.
func (f resendFixture) values(t *testing.T, loadID string) map[string]string {
	t.Helper()
	rows, err := f.db.ListFoundryLoadDecisions(context.Background(), loadID, "", []string{}, 1000)
	require.NoError(t, err)
	out := map[string]string{}
	for _, d := range rows {
		require.Equal(t, model.FoundryReviewProp, d.Kind)
		var tier string
		require.NoError(t, json.Unmarshal(d.Value, &tier))
		_, twice := out[*d.EntityKey]
		require.False(t, twice, "a load's list decides %s twice", *d.EntityKey)
		out[*d.EntityKey] = tier
	}
	return out
}

// requests is how often a load was owed its decisions, 0 when it owes none.
func (f resendFixture) requests(t *testing.T, loadID string) int {
	t.Helper()
	owed, err := f.db.FoundryLoadDecisionsOwed(context.Background(), loadID)
	require.NoError(t, err)
	if owed == nil {
		return 0
	}
	assert.Equal(t, model.FoundryResendDecisions, owed.Reason)
	assert.Empty(t, owed.DocumentID)
	return owed.Requests
}

// carry ends the request for a load's decisions that job read, as its Extract
// of the load's list ended.
func (f resendFixture) carry(t *testing.T, job *model.FoundryGraphJob, requests int) {
	t.Helper()
	require.NoError(t, f.db.EndFoundryLoadResend(context.Background(), job.ID, f.worker, model.FoundryResendEnd{
		Reason: model.FoundryResendDecisions, Requests: requests,
	}))
}

// A value a person chose joins the current load's list and queues one resend
// job, which sends the load's decisions and no document; once its Extract
// ends, the decision is applied. Choosing again for the same property is the
// same decision with the new value, and goes the same way.
func TestFoundryValueAppliesAtOnce(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "VNOW")
	d := f.value("1", "acme", "gold")
	require.True(t, f.decide(t, "1", d))
	assert.Equal(t, model.FoundryReleasePending, d.Applied.Status)
	assert.Equal(t, map[string]string{d.ID: "pending"}, f.applied(t, d))
	assert.Equal(t, map[string]string{"acme": "gold"}, f.values(t, f.load))
	assert.Equal(t, 1, f.requests(t, f.load))
	assert.Empty(t, f.owed(t), "no document is owed: a value decision names none")
	resend := f.liveJob(t)
	require.NotNil(t, resend)
	assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
	assert.Equal(t, f.load, resend.LoadID)
	assert.True(t, resend.NextAttemptAt.After(time.Now()), "it waits for the decisions that come with it")

	// Another customer's value lands before the job is claimed: one job.
	other := f.value("2", "globex", "silver")
	require.False(t, f.decide(t, "2", other), "the queued job takes it")
	assert.Equal(t, 2, f.requests(t, f.load))

	job := f.claim(t)
	require.Equal(t, resend.ID, job.ID)
	f.carry(t, job, 2)
	assert.Equal(t, map[string]string{d.ID: "applied", other.ID: "applied"}, f.applied(t, d, other))
	assert.Zero(t, f.requests(t, f.load))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "now", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t), "nothing is owed: no job follows")

	// Chosen again: the same decision, with the value chosen last.
	again := f.value("3", "acme", "platinum")
	require.True(t, f.decide(t, "3", again))
	assert.Equal(t, d.ID, again.ID)
	assert.Equal(t, map[string]string{d.ID: "pending", other.ID: "applied"}, f.applied(t, d, other))
	assert.Equal(t, map[string]string{"acme": "platinum", "globex": "silver"}, f.values(t, f.load))
	job = f.claim(t)
	f.carry(t, job, f.requests(t, f.load))
	assert.Equal(t, map[string]string{d.ID: "applied"}, f.applied(t, d))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "again", model.FoundryGraphJobSucceeded))

	// Only a value decision is settled this way, and only against the load
	// its review was read from.
	_, err := f.db.SettleFoundryValue(ctx, f.ontology.WorkspaceID, f.load, f.held(f.a, 0, readA), resendJob("VNOW-x"))
	assert.Error(t, err)
	_, err = f.db.SettleFoundryValue(ctx, f.ontology.WorkspaceID, id26("another-load"), f.value("4", "initech", "gold"), resendJob("VNOW-y"))
	assert.ErrorIs(t, err, store.ErrConflict)
	gone := f.value("5", "initech", "gold")
	gone.Prop = ptr("region")
	_, err = f.db.SettleFoundryValue(ctx, f.ontology.WorkspaceID, f.load, gone, resendJob("VNOW-z"))
	assert.ErrorIs(t, err, store.ErrConflict, "the load's version has no such property")
	assert.Nil(t, f.liveJob(t))
}

// One job at a time writes an ontology's database: a value chosen while
// another job is live waits in the load's list, and that job's end queues
// the resend.
func TestFoundryValueWhileAJobIsLive(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "VLIVE")
	c := f.uploaded(t, "VLIVE-c")
	extract := f.liveJob(t)
	require.Equal(t, model.FoundryGraphJobExtract, extract.Kind)
	d := f.value("1", "acme", "gold")
	require.False(t, f.decide(t, "1", d), "the extract goes on")
	assert.Equal(t, extract.ID, f.liveJob(t).ID)
	assert.Equal(t, map[string]string{d.ID: "pending"}, f.applied(t, d))

	job := f.claim(t)
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(c), false))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "vlive", model.FoundryGraphJobSucceeded))
	// The extract's own Extracts may have carried it, and no one knows: the
	// resend is what says so.
	assert.Equal(t, map[string]string{d.ID: "pending"}, f.applied(t, d))
	resend := f.liveJob(t)
	require.NotNil(t, resend, "the extract's end queues the resend")
	assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
	assert.Equal(t, id26("FN-R-vlive"), resend.ID)
	job = f.claim(t)
	f.carry(t, job, 1)
	assert.Equal(t, map[string]string{d.ID: "applied"}, f.applied(t, d))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "vlive2", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))
}

// A value chosen while the resend that carries the list runs is not that
// job's to answer: the job ends only the request it read, the decision goes
// on waiting, and the job's end queues another.
func TestFoundryValueLandsWhileAResendRuns(t *testing.T) {
	f := newResendFixture(t, "VRUN")
	d := f.value("1", "acme", "gold")
	require.True(t, f.decide(t, "1", d))
	job := f.claim(t)
	read := f.requests(t, f.load)
	late := f.value("2", "globex", "silver")
	require.False(t, f.decide(t, "2", late), "the running job goes on")
	f.carry(t, job, read)
	assert.Equal(t, map[string]string{d.ID: "pending", late.ID: "pending"}, f.applied(t, d, late),
		"nothing says the Extract carried the late one, so neither is marked")
	assert.Equal(t, 2, f.requests(t, f.load))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "vrun", model.FoundryGraphJobSucceeded))
	next := f.claim(t)
	require.Equal(t, model.FoundryGraphJobResend, next.Kind)
	f.carry(t, next, 2)
	assert.Equal(t, map[string]string{d.ID: "applied", late.ID: "applied"}, f.applied(t, d, late))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, next, "vrun2", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))
}

// A value decision made, changed or removed while a new load is written is
// what that load holds once it is current: the new one joins its list as
// carried, the changed one replaces the copy the load was created with, the
// removed one has left its list, and one resend sends the list. None waits
// for ever: a value decision needs no document.
func TestFoundryValueCarriedToTheNextLoad(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "VCARRY")
	first := f.load
	kept, changed, removed := f.value("1", "acme", "gold"), f.value("2", "globex", "silver"), f.value("3", "initech", "bronze")
	for i, d := range []*model.FoundryReviewDecision{kept, changed, removed} {
		f.decide(t, string(rune('a'+i)), d)
	}
	job := f.claim(t)
	f.carry(t, job, 3)
	f.end(t, job, "early", model.FoundryGraphJobSucceeded)

	// A new load starts and copies the three.
	f.draftVersion(t, "VCARRY2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "VCARRY2")))
	second := id26("FJ-VCARRY2")
	assert.Equal(t, map[string]string{"acme": "gold", "globex": "silver", "initech": "bronze"}, f.values(t, second))

	// While it is written: one more is chosen, one is chosen again, one is
	// taken back, all on the load that is current.
	late := f.value("4", "umbrella", "gold")
	require.False(t, f.decide(t, "late", late), "the load job goes on")
	again := f.value("5", "globex", "platinum")
	require.False(t, f.decide(t, "again", again))
	require.Equal(t, changed.ID, again.ID)
	f.remove(t, "gone", removed)
	assert.Equal(t, map[string]string{"acme": "gold", "globex": "platinum", "umbrella": "gold"}, f.values(t, first))
	assert.Equal(t, map[string]string{"acme": "gold", "globex": "silver"}, f.values(t, second),
		"the removed one left the load being written too")
	assert.Equal(t, 1, f.requests(t, second), "which owes the engine its list for it")

	f.loaded(t, f.worker, f.a, f.b)
	require.Equal(t, second, *f.ontologyNow(t).CurrentLoadID)
	f.load = second
	assert.Equal(t, map[string]string{"acme": "gold", "globex": "platinum", "umbrella": "gold"}, f.values(t, second))
	assert.Equal(t, map[string]string{kept.ID: "", again.ID: "carried", late.ID: "carried"}, f.applied(t, kept, again, late))
	resend := f.liveJob(t)
	require.NotNil(t, resend, "the load's end queues the resend")
	assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
	assert.Equal(t, second, resend.LoadID)

	job = f.claim(t)
	f.carry(t, job, f.requests(t, second))
	assert.Equal(t, map[string]string{kept.ID: "", again.ID: "applied", late.ID: "applied"}, f.applied(t, kept, again, late))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "carried", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t), "carried once: nothing more is owed")
}

// Removing a value decision takes it out of every load's list, and each of
// those owes the engine its decisions again, so the engine withdraws it: the
// current load at once, another when it is made current.
func TestFoundryValueRemoved(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "VBACK")
	first := f.load

	// Before it is applied: the queued job sends a list without it.
	early := f.value("1", "acme", "gold")
	require.True(t, f.decide(t, "1", early))
	queued := f.liveJob(t)
	f.remove(t, "1", early)
	assert.Empty(t, f.values(t, first))
	assert.Empty(t, f.applied(t, early))
	assert.Equal(t, queued.ID, f.liveJob(t).ID, "the job already queued sends it")
	assert.Equal(t, 2, f.requests(t, first))
	job := f.claim(t)
	f.carry(t, job, 2)
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "early", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))
	assert.ErrorIs(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, early.ID, resendJob("VBACK-x")), store.ErrNotFound)

	// After it is applied, with a second load that copied it.
	d := f.value("2", "acme", "gold")
	require.True(t, f.decide(t, "2", d))
	job = f.claim(t)
	f.carry(t, job, f.requests(t, first))
	f.end(t, job, "applied", model.FoundryGraphJobSucceeded)
	f.draftVersion(t, "VBACK2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "VBACK2")))
	f.loaded(t, f.worker, f.a, f.b)
	second := id26("FJ-VBACK2")
	require.Equal(t, second, *f.ontologyNow(t).CurrentLoadID)
	f.load = second
	require.Nil(t, f.liveJob(t))
	assert.Equal(t, map[string]string{"acme": "gold"}, f.values(t, second))

	f.remove(t, "2", d)
	assert.Empty(t, f.values(t, first))
	assert.Empty(t, f.values(t, second))
	assert.NotZero(t, f.requests(t, first), "each load that held it owes its list")
	assert.Equal(t, 1, f.requests(t, second))
	resend := f.liveJob(t)
	require.NotNil(t, resend, "the current load is sent its list at once")
	assert.Equal(t, id26("RM-2"), resend.ID)
	assert.Equal(t, second, resend.LoadID)
	job = f.claim(t)
	f.carry(t, job, 1)
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "withdrawn", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))

	// The first load still holds the value in its database: made current, it
	// is sent its list, and the engine withdraws it there too.
	_, err := f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, extractJob("VBACK"), resendJob("VBACK"), noBudget)
	require.NoError(t, err)
	resend = f.liveJob(t)
	require.NotNil(t, resend)
	assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
	assert.Equal(t, first, resend.LoadID)

	// A resolve or an approval removed queues nothing of the kind.
	f.load = first
	job = f.claim(t)
	f.carry(t, job, f.requests(t, first))
	f.end(t, job, "first", model.FoundryGraphJobSucceeded)
	resolve := &model.FoundryReviewDecision{ID: id26("VBACK-r"), OntologyID: f.ontology.ID, Kind: model.FoundryReviewResolve, Type: "Customer",
		DocID: &f.a, NormName: ptr("acme"), DecidedByUserID: id26("user")}
	require.NoError(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, resolve))
	f.remove(t, "3", resolve)
	assert.Nil(t, f.liveJob(t))
	assert.Zero(t, f.requests(t, first))
}

// A resend job that fails for good leaves the value decisions it was to
// carry unconfirmed: the load's list holds them, so some Extract may have
// carried them, and no job is coming to say so. Choosing a value again
// queues one, and its end marks every one that waited.
func TestFoundryValueWhoseJobFailed(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "VFAIL")
	d, other := f.value("1", "acme", "gold"), f.value("2", "globex", "silver")
	require.True(t, f.decide(t, "1", d))
	f.decide(t, "2", other)
	job := f.claim(t)
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "vfail", model.FoundryGraphJobFailed))
	assert.Equal(t, map[string]string{d.ID: "pending/job_failed", other.ID: "pending/job_failed"}, f.applied(t, d, other))
	got, err := f.db.ListFoundryDecisionsApplied(ctx, f.ontology.ID, []string{d.ID})
	require.NoError(t, err)
	assert.True(t, got[d.ID].Unconfirmed())
	assert.Zero(t, f.requests(t, f.load))
	assert.Nil(t, f.liveJob(t), "nothing is queued for a decision whose job failed")
	assert.Equal(t, map[string]string{"acme": "gold", "globex": "silver"}, f.values(t, f.load), "they stay in the list")

	// Nor does a later job's end queue one.
	c := f.uploaded(t, "VFAIL-c")
	job = f.claim(t)
	require.Equal(t, model.FoundryGraphJobExtract, job.Kind)
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(c), false))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "vfail-up", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))

	again := f.value("3", "acme", "gold")
	require.True(t, f.decide(t, "3", again))
	assert.Equal(t, map[string]string{d.ID: "pending", other.ID: "pending/job_failed"}, f.applied(t, d, other))
	job = f.claim(t)
	f.carry(t, job, f.requests(t, f.load))
	assert.Equal(t, map[string]string{d.ID: "applied", other.ID: "applied"}, f.applied(t, d, other),
		"the Extract that ended carried the whole list")
}

// An approval's document and the load's decisions go in one job: each
// request ends on its own.
func TestFoundryValueWithAnApproval(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "VBOTH")
	approval, v := f.held(f.a, 0, readA), f.value("1", "acme", "gold")
	require.True(t, f.approve(t, "1", approval))
	require.False(t, f.decide(t, "2", v), "the queued job takes it")
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1, "the load's own request is no document")
	assert.Equal(t, f.a, owed[0].DocumentID)
	f.sent(t, job, owed[0], 1, written(approval))
	f.carry(t, job, 1)
	assert.Equal(t, map[string]string{approval.ID: "written", v.ID: "applied"}, f.applied(t, approval, v))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "both", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t))
}

// A load that copied a decision of a property since decided anew under
// another id (its version dropped the property and took it back) holds one
// decision of it once it is current: the one chosen last.
func TestFoundryValueNeverDecidesAPropertyTwice(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "VTWICE")
	first := f.load
	d := f.value("1", "acme", "gold")
	require.True(t, f.decide(t, "1", d))
	job := f.claim(t)
	f.carry(t, job, 1)
	f.end(t, job, "one", model.FoundryGraphJobSucceeded)
	f.draftVersion(t, "VTWICE2", customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "VTWICE2")))
	f.loaded(t, f.worker, f.a, f.b)
	f.load = id26("FJ-VTWICE2")
	// The ontology's decision goes, as a version without the property prunes
	// it, and the loads keep their copies.
	_, err := testDB.Exec(`DELETE FROM foundry_review_decisions WHERE id=?`, d.ID)
	require.NoError(t, err)
	anew := f.value("2", "acme", "silver")
	require.True(t, f.decide(t, "2", anew))
	require.NotEqual(t, d.ID, anew.ID)
	assert.Equal(t, map[string]string{"acme": "silver"}, f.values(t, f.load), "the list it is settled against holds the new one alone")
	job = f.claim(t)
	f.carry(t, job, 1)
	f.end(t, job, "two", model.FoundryGraphJobSucceeded)

	_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, extractJob("VTWICE"), resendJob("VTWICE"), noBudget)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"acme": "silver"}, f.values(t, first))
	require.NotNil(t, f.liveJob(t), "and is sent it")
}

// Every value decision goes with every Extract, which takes
// model.FoundryExtractDecisionsMax decisions: the ontology takes no more
// value decisions than that, so no load is made unsendable by one. A property
// already decided can still be decided again.
func TestFoundryValueDecisionsFull(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "VFULL")
	d := f.value("1", "acme", "gold")
	require.True(t, f.decide(t, "1", d))
	// One short of the limit, beside the one settled.
	_, err := testDB.Exec(`INSERT INTO foundry_review_decisions (id, ontology_id, kind, type, entity_key, prop, value, decided_by_user_id)
SELECT CONCAT('VF', LPAD(a.n + 10*b.n + 100*c.n + 1000*e.n, 24, '0')), ?, 'prop', 'Customer', CONCAT('c', a.n + 10*b.n + 100*c.n + 1000*e.n), 'tier',
  CAST('"gold"' AS JSON), ?
FROM (SELECT 0 n UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) a,
  (SELECT 0 n UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) b,
  (SELECT 0 n UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) c,
  (SELECT 0 n UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) e
WHERE a.n + 10*b.n + 100*c.n + 1000*e.n < ?`, f.ontology.ID, id26("user"), model.FoundryExtractDecisionsMax-2)
	require.NoError(t, err)
	last := f.value("2", "globex", "silver")
	_, err = f.db.SettleFoundryValue(ctx, f.ontology.WorkspaceID, f.load, last, resendJob("VFULL-a"))
	require.NoError(t, err, "the last one the limit takes")
	over := f.value("3", "initech", "gold")
	_, err = f.db.SettleFoundryValue(ctx, f.ontology.WorkspaceID, f.load, over, resendJob("VFULL-b"))
	assert.ErrorIs(t, err, store.FoundryValueDecisionsFull)
	assert.Empty(t, f.applied(t, over))
	again := f.value("4", "acme", "platinum")
	_, err = f.db.SettleFoundryValue(ctx, f.ontology.WorkspaceID, f.load, again, resendJob("VFULL-c"))
	require.NoError(t, err, "a property already decided is decided again")
	assert.Equal(t, d.ID, again.ID)
	// Removing one makes room.
	f.remove(t, "1", last)
	_, err = f.db.SettleFoundryValue(ctx, f.ontology.WorkspaceID, f.load, over, resendJob("VFULL-d"))
	require.NoError(t, err)
}
