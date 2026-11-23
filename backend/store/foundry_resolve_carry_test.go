package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/pkg/namewords"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resolvesIn lists load's resolve copies, by id, with their status ("" for
// none).
func resolvesIn(t *testing.T, load string) map[string]string {
	t.Helper()
	rows, err := testDB.Query(`SELECT id, COALESCE(applied_status, '') FROM foundry_load_decisions WHERE load_id=? AND kind='resolve'`, load)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, status string
		require.NoError(t, rows.Scan(&id, &status))
		out[id] = status
	}
	require.NoError(t, rows.Err())
	return out
}

// owedRaw lists load's owed requests by document and reason as stored,
// those a resend holds back included (ListFoundryLoadResends).
func owedRaw(t *testing.T, load string) map[string]int {
	t.Helper()
	rows, err := testDB.Query(`SELECT document_id, reason, requests FROM foundry_load_resends WHERE load_id=? AND status='owed'`, load)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var doc, reason string
		var requests int
		require.NoError(t, rows.Scan(&doc, &reason, &requests))
		out[doc+"/"+reason] = requests
	}
	require.NoError(t, rows.Err())
	return out
}

// entityIn is the entity load's copy of decision id chose ("" for none).
func entityIn(t *testing.T, load, id string) string {
	t.Helper()
	var entity sql.NullString
	require.NoError(t, testDB.QueryRow(`SELECT entity_key FROM foundry_load_decisions WHERE load_id=? AND id=?`, load, id).Scan(&entity))
	return entity.String
}

// settleBound settles d as a resolve of a name the fixture's load links
// already, as the API does (SettleFoundryBoundResolve): for the next load
// while that load is current.
func (f resendFixture) settleBound(t *testing.T, d *model.FoundryReviewDecision) {
	t.Helper()
	_, err := settleBound(f.db, context.Background(), f.ontology.WorkspaceID, f.load, d, &model.FoundryGraphJob{ID: id26("RB-" + d.ID)})
	require.NoError(t, err)
}

// newLoad starts a load of a version of customerTOML, FJ-<tag>, as its
// queued load job.
func (f resendFixture) newLoad(t *testing.T, tag string) string {
	t.Helper()
	f.draftVersion(t, tag, customerTOML)
	require.NoError(t, f.db.CreateFoundryLoad(context.Background(), loadJob(f.ontology, tag)))
	return id26("FJ-" + tag)
}

// nameWritten records the engine's word that d's own document settled its name,
// as a resend's run read with its saved work.
func (f resendFixture) nameWritten(t *testing.T, job *model.FoundryGraphJob, d *model.FoundryReviewDecision) {
	t.Helper()
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(context.Background(), job.ID, f.worker, &model.FoundryDocumentExtraction{
		DocumentID: *d.DocID, Status: model.FoundryExtractionExtracted, SettlesNames: true,
		Names: []model.FoundryDocumentName{{Decision: model.FoundryNameResolve, Type: d.Type, NormName: *d.NormName, DecisionID: &d.ID,
			Outcome: model.FoundryNameWritten}},
	}, true))
}

// A resolve settled while a load created before it is written reaches that
// load when it becomes current, by the load job's end: an alias joins the
// list carried, with its name's key, and "not in data" pending, with none;
// the load owes each one's document a resend for a name, and the alias's is
// held back until the resend job has owed the documents of this load whose
// reviews ask about its name, which moves the copy to pending. The engine's
// word on its document lands on the copy as on any pending one. A job's end
// carries nothing again, and the load made current again owes nothing.
func TestFoundryResolveCarriedToALoadMadeBeforeIt(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RVCARRY")
	first := f.load
	acme := "acme"
	second := f.newLoad(t, "RVCARRY2")
	alias, none := f.resolveOf("1", f.a, "acme corp", &acme), f.resolveOf("2", f.b, "helio", nil)
	require.False(t, f.settleName(t, "1", alias, f.b), "the load job goes on")
	require.False(t, f.settleName(t, "2", none), "the load job goes on")
	assert.Equal(t, map[string]string{alias.ID: "pending", none.ID: "pending"}, resolvesIn(t, first))
	assert.Empty(t, resolvesIn(t, second), "created before them")

	job := f.claim(t)
	require.Equal(t, second, job.ID)
	for _, doc := range []string{f.a, f.b} {
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(doc), false))
	}
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "rvcarry", model.FoundryGraphJobSucceeded))
	require.Equal(t, second, *f.ontologyNow(t).CurrentLoadID)
	f.load = second
	assert.Equal(t, map[string]string{alias.ID: "carried", none.ID: "pending"}, f.applied(t, alias, none))
	var key, noKey sql.NullInt64
	require.NoError(t, testDB.QueryRow(`SELECT name_key FROM foundry_load_decisions WHERE load_id=? AND id=?`, second, alias.ID).Scan(&key))
	want, _ := namewords.Key("acme corp")
	assert.Equal(t, sql.NullInt64{Int64: want, Valid: true}, key, "found by the Extracts of the documents that write it")
	require.NoError(t, testDB.QueryRow(`SELECT name_key FROM foundry_load_decisions WHERE load_id=? AND id=?`, second, none.ID).Scan(&noKey))
	assert.False(t, noKey.Valid, "not in data carries nothing to other documents")
	assert.Equal(t, map[string]int{f.a + "/name": 1, f.b + "/name": 1}, owedRaw(t, second), "their own documents, not those the first load's reviews asked")
	assert.Equal(t, map[string]int{f.b + "/name": 1}, f.owedFor(t), "the alias's document waits for its asking documents")
	carried, err := f.db.ListFoundryCarriedAliases(ctx, second, 10)
	require.NoError(t, err)
	require.Len(t, carried, 1, "an alias reaches the documents that ask; not in data doesn't")
	assert.Equal(t, alias.ID, carried[0].ID)
	resend := f.liveJob(t)
	require.NotNil(t, resend)
	assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
	assert.Equal(t, second, resend.LoadID)

	job = f.claim(t)
	owed, err := f.db.OweFoundryAskingResends(ctx, job, f.worker, []string{alias.ID}, []string{f.b})
	require.NoError(t, err)
	require.True(t, owed)
	assert.Equal(t, map[string]string{alias.ID: "pending", none.ID: "pending"}, f.applied(t, alias, none))
	assert.Equal(t, map[string]int{f.a + "/name": 1, f.b + "/name": 2}, f.owedFor(t))
	carried, err = f.db.ListFoundryCarriedAliases(ctx, second, 10)
	require.NoError(t, err)
	assert.Empty(t, carried, "read once")
	f.nameWritten(t, job, alias)
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, &model.FoundryDocumentExtraction{
		DocumentID: f.b, Status: model.FoundryExtractionExtracted, SettlesNames: true}, true))
	require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{DocumentID: f.a, Reason: model.FoundryResendName, Requests: 1}))
	require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{DocumentID: f.b, Reason: model.FoundryResendName, Requests: 2}))
	assert.Equal(t, map[string]string{alias.ID: "written", none.ID: "pending"}, f.applied(t, alias, none))
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "rvcarried", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t), "carried once: nothing is owed again")
	assert.Empty(t, owedRaw(t, second))
	assert.Len(t, resolvesIn(t, second), 2, "one copy each")
	_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, second, extractJob("RVCARRY"), resendJob("RVCARRY"), noBudget)
	require.NoError(t, err)
	assert.Nil(t, f.liveJob(t), "made current again, it owes nothing")
}

// A decision the load holds by id, copied when it was created, is left as it
// is when the load becomes current, while one saved during its writing is
// carried: only that one's document is owed. Made current again, the load
// carries nothing anew and owes nothing more.
func TestFoundryResolveHeldByIDNotCarried(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RVHELD")
	first := f.load
	acme := "acme"
	d := f.resolveOf("1", f.a, "acme corp", &acme)
	require.True(t, f.settleName(t, "1", d))
	job := f.claim(t)
	f.nameWritten(t, job, d)
	require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{DocumentID: f.a, Reason: model.FoundryResendName, Requests: 1}))
	f.end(t, job, "rvheld", model.FoundryGraphJobSucceeded)

	second := f.newLoad(t, "RVHELD2")
	later := f.resolveOf("2", f.b, "helio", &acme)
	require.False(t, f.settleName(t, "2", later))
	f.loaded(t, f.worker, f.a, f.b)
	require.Equal(t, second, *f.ontologyNow(t).CurrentLoadID)
	assert.Equal(t, map[string]string{d.ID: "", later.ID: "carried"}, resolvesIn(t, second), "the copy made with the load, as it was")
	assert.Equal(t, map[string]int{f.b + "/name": 1}, owedRaw(t, second))

	for i, load := range []string{first, second} {
		_, err := f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, load, extractJob(fmt.Sprint("RVHELD", i)),
			resendJob(fmt.Sprint("RVHELD", i)), noBudget)
		require.NoError(t, err)
	}
	assert.Equal(t, map[string]string{d.ID: "", later.ID: "carried"}, resolvesIn(t, second))
	assert.Equal(t, map[string]int{f.b + "/name": 1}, owedRaw(t, second), "owed once")
	assert.Equal(t, map[string]string{d.ID: "written", later.ID: "pending"}, resolvesIn(t, first))
}

// What a person did while the load was written reaches it as they left it:
// a changed choice is the one copy of its name (a written one of the earlier
// choice, copied with the load, gives way), and a resolve removed is not
// carried, nor its document owed.
func TestFoundryResolveChangedOrRemovedWhileTheLoadIsWritten(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RVCHG")
	acme, globex := "acme", "globex"
	earlier := f.resolveOf("1", f.a, "acme corp", &acme)
	require.True(t, f.settleName(t, "1", earlier))
	job := f.claim(t)
	f.nameWritten(t, job, earlier)
	require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{DocumentID: f.a, Reason: model.FoundryResendName, Requests: 1}))
	f.end(t, job, "rvchg", model.FoundryGraphJobSucceeded)

	second := f.newLoad(t, "RVCHG2")
	assert.Equal(t, map[string]string{earlier.ID: ""}, resolvesIn(t, second))
	changed := f.resolveOf("2", f.a, "acme corp", &globex)
	require.False(t, f.settleName(t, "2", changed))
	require.NotEqual(t, earlier.ID, changed.ID)
	gone := f.resolveOf("3", f.b, "helio", &acme)
	require.False(t, f.settleName(t, "3", gone))
	require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, gone.ID, &model.FoundryGraphJob{ID: id26("RD-3")}))

	f.loaded(t, f.worker, f.a, f.b)
	require.Equal(t, second, *f.ontologyNow(t).CurrentLoadID)
	assert.Equal(t, map[string]string{changed.ID: "carried"}, resolvesIn(t, second))
	assert.Equal(t, map[string]int{f.a + "/name": 1}, owedRaw(t, second))
	list, err := f.db.ListFoundryLoadDecisions(ctx, second, "", []string{f.a, f.b}, 100)
	require.NoError(t, err)
	require.Len(t, list, 1, "the document's Extract decides its name once")
	assert.Equal(t, "globex", *list[0].EntityKey)
}

// A resolve of a name the load links already is saved for the next load
// (SettleFoundryReviewDecision): the current load's later resends and job
// ends leave it out. Chosen again under its id while a load created before
// is written, it reaches that load as last chosen: the copy the load was
// created with gives way, as a changed choice under another id does.
func TestFoundryResolveBoundIsTheNextLoads(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RVBOUND")
	first := f.load
	acme, globex := "acme", "globex"
	bound := f.resolveOf("1", f.a, "acme corp", &acme)
	f.settleBound(t, bound)
	asks := f.resolveOf("2", f.b, "helio", &acme)
	require.True(t, f.settleName(t, "2", asks))
	job := f.claim(t)
	require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{DocumentID: f.b, Reason: model.FoundryResendName, Requests: 1}))
	f.end(t, job, "rvbound", model.FoundryGraphJobSucceeded)
	assert.Equal(t, map[string]string{asks.ID: "pending"}, resolvesIn(t, first), "the bound name is not this load's")
	assert.Empty(t, owedRaw(t, first))
	assert.Nil(t, f.liveJob(t))

	second := f.newLoad(t, "RVBOUND2")
	assert.Equal(t, "acme", entityIn(t, second, bound.ID), "copied as it was chosen then")
	again := f.resolveOf("1", f.a, "acme corp", &globex)
	f.settleBound(t, again)
	require.Equal(t, bound.ID, again.ID, "a bound name chosen again keeps its decision")
	assert.NotContains(t, resolvesIn(t, first), bound.ID)

	f.loaded(t, f.worker, f.a, f.b)
	require.Equal(t, second, *f.ontologyNow(t).CurrentLoadID)
	assert.Equal(t, map[string]string{bound.ID: "carried", asks.ID: ""}, resolvesIn(t, second))
	assert.Equal(t, "globex", entityIn(t, second, bound.ID), "the choice that stands")
	assert.Equal(t, map[string]int{f.a + "/name": 1}, owedRaw(t, second))
}

// An alias carried to a load that holds no extraction of its document still
// reaches that load's other documents: its document is owed, so the resend
// job owes the documents that ask about its name, and the copy then waits on
// nothing, as one the load was created with: the document's Extract carries
// it, and its request, which the load can't send, refuses nothing. "Not in
// data" joins the same way, with nothing owed.
func TestFoundryResolveCarriedWhereItsDocumentIsUnread(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RVUNREAD")
	first := f.load
	second := f.newLoad(t, "RVUNREAD2")
	f.loaded(t, f.worker, f.a, f.b)
	_, err := f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, extractJob("RVUNREAD"), resendJob("RVUNREAD"), noBudget)
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_load_extractions SET status='failed', error_code='unsupported', error_retryable=FALSE
WHERE load_id=? AND document_id=?`, second, f.a)
	require.NoError(t, err)
	acme := "acme"
	alias, none := f.resolveOf("1", f.a, "acme corp", &acme), f.resolveOf("2", f.a, "helio", nil)
	f.settleName(t, "1", alias, f.b)
	f.settleName(t, "2", none)
	job := f.claim(t)
	owedFirst, err := f.db.ListFoundryLoadResends(ctx, first)
	require.NoError(t, err)
	for _, r := range owedFirst {
		require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{DocumentID: r.DocumentID, Reason: r.Reason, Requests: r.Requests}))
	}
	f.end(t, job, "rvunread", model.FoundryGraphJobSucceeded)
	require.Nil(t, f.liveJob(t))

	_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, second, extractJob("RVUNREAD2"), resendJob("RVUNREAD2"), noBudget)
	require.NoError(t, err)
	f.load = second
	assert.Equal(t, map[string]string{alias.ID: "carried", none.ID: ""}, resolvesIn(t, second))
	assert.Equal(t, map[string]int{f.a + "/name": 1}, owedRaw(t, second), "the alias's document, to reach the documents that ask")
	assert.Empty(t, f.owedFor(t))

	job = f.claim(t)
	require.Equal(t, second, job.LoadID)
	owed, err := f.db.OweFoundryAskingResends(ctx, job, f.worker, []string{alias.ID}, []string{f.b})
	require.NoError(t, err)
	require.True(t, owed)
	assert.Equal(t, map[string]string{alias.ID: "", none.ID: ""}, resolvesIn(t, second))
	listed, err := f.db.ListFoundryLoadResends(ctx, second)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	assert.Equal(t, f.b, listed[0].DocumentID)
	assert.Equal(t, f.a, listed[1].DocumentID)
	assert.Equal(t, model.FoundryReleaseDocumentUnread, listed[1].Unsendable)
	require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{DocumentID: f.a, Reason: model.FoundryResendName,
		Requests: 1, Refused: listed[1].Unsendable}))
	assert.Equal(t, map[string]string{alias.ID: "", none.ID: ""}, resolvesIn(t, second), "nothing refused")
	list, err := f.db.ListFoundryLoadDecisions(ctx, second, "", []string{f.a}, 100)
	require.NoError(t, err)
	assert.Len(t, list, 2, "its document's Extract carries both")
	key, _ := namewords.Key("acme corp")
	aliases, err := f.db.ListFoundryLoadAliasesNamed(ctx, second, []int64{key})
	require.NoError(t, err)
	require.Len(t, aliases, 1, "and every Extract of a document that writes the name")
	assert.Equal(t, alias.ID, aliases[0].ID)
}

// A resolve removed while a load that holds it waiting, or refused, is not
// current never applies there: when that load is current again, the carry
// drops the copy at once, as removing it then would have, though another
// load's resend is live.
func TestFoundryResolveRemovedWhileItsLoadWasNotCurrent(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RVBACK")
	first := f.load
	second := f.newLoad(t, "RVBACK2")
	f.loaded(t, f.worker, f.a, f.b)
	_, err := f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, extractJob("RVBACK"), resendJob("RVBACK"), noBudget)
	require.NoError(t, err)
	require.Nil(t, f.liveJob(t))

	acme := "acme"
	d := f.resolveOf("1", f.a, "acme corp", &acme)
	require.True(t, f.settleName(t, "1", d))
	job := f.claim(t)
	require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{DocumentID: f.a, Reason: model.FoundryResendName,
		Requests: 1, Refused: string(model.FoundryNameTwoRows)}))
	f.end(t, job, "rvback", model.FoundryGraphJobSucceeded)
	assert.Equal(t, map[string]string{d.ID: "refused"}, resolvesIn(t, first))

	// The second load is made current: it gets the resolve, and loses it
	// again as the person removes it there.
	queued, err := f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, second, extractJob("RVBACK2"), resendJob("RVBACK2"), noBudget)
	require.NoError(t, err)
	assert.False(t, queued)
	assert.Equal(t, map[string]string{d.ID: "carried"}, resolvesIn(t, second))
	require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.WorkspaceID, f.ontology.ID, d.ID, &model.FoundryGraphJob{ID: id26("RD-1")}))
	assert.Empty(t, resolvesIn(t, second))
	assert.Equal(t, map[string]string{d.ID: "refused"}, resolvesIn(t, first), "not current: left until it is")

	// The first is current again while the second's resend is live.
	require.NotNil(t, f.liveJob(t))
	_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, extractJob("RVBACK3"), resendJob("RVBACK3"), noBudget)
	require.NoError(t, err)
	assert.Empty(t, resolvesIn(t, first), "the removed choice is sent to no load")
	job = f.claim(t)
	require.Equal(t, second, job.LoadID)
	f.end(t, job, "rvback-2", model.FoundryGraphJobSucceeded)
	assert.Empty(t, resolvesIn(t, first))
	list, err := f.db.ListFoundryLoadDecisions(ctx, first, "", []string{f.a}, 100)
	require.NoError(t, err)
	assert.Empty(t, list)
}

// Another load became current between the person reading the review and
// settling it: the decision is saved, checked against the current load's
// version, and joins that load, which owes its own document only; the
// documents read from the load the review came from are another load's, and
// the resend job reads the current load's. The load the review came from is
// left as it is. "Not in data" waits as pending where the current load holds
// its document extracted, and with no status, nothing owed, where it
// doesn't. A current load whose version doesn't take the decision refuses
// it, and nothing is saved.
func TestFoundryResolveSettledAfterTheLoadChanged(t *testing.T) {
	ctx := context.Background()
	swapped := func(t *testing.T, tag string) (f resendFixture, first, second string) {
		f = newResendFixture(t, tag)
		first = f.load
		second = f.newLoad(t, tag+"2")
		f.loaded(t, f.worker, f.a, f.b)
		require.Nil(t, f.liveJob(t))
		return f, first, second
	}
	t.Run("an alias carried to the current load", func(t *testing.T) {
		f, first, second := swapped(t, "RVSWAP")
		acme := "acme"
		d := f.resolveOf("1", f.a, "acme corp", &acme)
		queued, err := settleResolve(f.db, ctx, f.ontology.WorkspaceID, first, d, []string{f.b},
			&model.FoundryGraphJob{ID: id26("RN-1"), RequestedByUserID: ptr(id26("user"))})
		require.NoError(t, err)
		assert.True(t, queued)
		require.NotNil(t, d.Applied)
		assert.Equal(t, model.FoundryReleaseCarried, d.Applied.Status)
		assert.Equal(t, map[string]string{d.ID: "carried"}, resolvesIn(t, second))
		assert.Empty(t, resolvesIn(t, first))
		assert.Equal(t, map[string]int{f.a + "/name": 1}, owedRaw(t, second))
		carried, err := f.db.ListFoundryCarriedAliases(ctx, second, 10)
		require.NoError(t, err)
		require.Len(t, carried, 1)
		resend := f.liveJob(t)
		require.NotNil(t, resend)
		assert.Equal(t, id26("RN-1"), resend.ID)
		assert.Equal(t, second, resend.LoadID)
	})
	t.Run("not in data", func(t *testing.T) {
		f, first, second := swapped(t, "RVSWAPN")
		d := f.resolveOf("1", f.a, "helio", nil)
		_, err := settleResolve(f.db, ctx, f.ontology.WorkspaceID, first, d, nil, &model.FoundryGraphJob{ID: id26("RN-1")})
		require.NoError(t, err)
		require.NotNil(t, d.Applied)
		assert.Equal(t, model.FoundryReleasePending, d.Applied.Status)
		assert.Equal(t, map[string]int{f.a + "/name": 1}, owedRaw(t, second))
		_, err = testDB.Exec(`UPDATE foundry_load_extractions SET status='failed', error_code='unsupported', error_retryable=FALSE
WHERE load_id=? AND document_id=?`, second, f.b)
		require.NoError(t, err)
		unread := f.resolveOf("2", f.b, "helio", nil)
		_, err = settleResolve(f.db, ctx, f.ontology.WorkspaceID, first, unread, nil, &model.FoundryGraphJob{ID: id26("RN-2")})
		require.NoError(t, err)
		assert.Nil(t, unread.Applied)
		assert.Equal(t, map[string]string{d.ID: "pending", unread.ID: ""}, resolvesIn(t, second))
		assert.Equal(t, map[string]int{f.a + "/name": 1}, owedRaw(t, second), "nothing for the unread document")
	})
	t.Run("a version that doesn't take it", func(t *testing.T) {
		f := newResendFixture(t, "RVSWAPV")
		first := f.load
		f.draftVersion(t, "RVSWAPV2", "[types.Vendor]\nprops = { name = \"string\" }")
		require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "RVSWAPV2")))
		f.loaded(t, f.worker, f.a, f.b)
		acme := "acme"
		_, err := settleResolve(f.db, ctx, f.ontology.WorkspaceID, first, f.resolveOf("1", f.a, "acme corp", &acme), nil,
			&model.FoundryGraphJob{ID: id26("RN-1")})
		assert.ErrorIs(t, err, store.ErrConflict)
		assert.Empty(t, f.decisionIDs(t))
	})
}

// A settle and the end of the load job that makes another load current, in
// either order under the ontology's lock, leave the new current load the
// same: one copy of the decision, carried, its document alone owed, and a
// resend of it queued. A resolve that asks, settled first, waits in the load
// its review came from as well, with the documents read there owed; a bound
// one is the next load's there. Settled after, it leaves that load as it is.
func TestFoundryResolveSettledAsTheLoadChanges(t *testing.T) {
	ctx := context.Background()
	acme := "acme"
	for _, bound := range []bool{false, true} {
		for _, settleFirst := range []bool{true, false} {
			t.Run(fmt.Sprintf("bound %t, settled first %t", bound, settleFirst), func(t *testing.T) {
				tag := "RVORD" + map[bool]string{false: "A", true: "B"}[bound] + map[bool]string{false: "S", true: "F"}[settleFirst]
				f := newResendFixture(t, tag)
				first := f.load
				second := f.newLoad(t, tag+"B")
				job := f.claim(t)
				require.Equal(t, second, job.ID)
				for _, doc := range []string{f.a, f.b} {
					require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(doc), false))
				}
				d := f.resolveOf("1", f.a, "acme corp", &acme)
				settle := func() {
					var err error
					if bound {
						_, err = settleBound(f.db, ctx, f.ontology.WorkspaceID, first, d, &model.FoundryGraphJob{ID: id26("RN-" + tag)})
					} else {
						_, err = settleResolve(f.db, ctx, f.ontology.WorkspaceID, first, d, []string{f.b}, &model.FoundryGraphJob{ID: id26("RN-" + tag)})
					}
					require.NoError(t, err)
				}
				if settleFirst {
					settle()
				}
				f.end(t, job, tag, model.FoundryGraphJobSucceeded)
				require.Equal(t, second, *f.ontologyNow(t).CurrentLoadID)
				if !settleFirst {
					settle()
				}
				assert.Equal(t, map[string]string{d.ID: "carried"}, resolvesIn(t, second))
				assert.Equal(t, map[string]int{f.a + "/name": 1}, owedRaw(t, second))
				resend := f.liveJob(t)
				require.NotNil(t, resend)
				assert.Equal(t, second, resend.LoadID)
				if settleFirst && !bound {
					assert.Equal(t, map[string]string{d.ID: "pending"}, resolvesIn(t, first))
					assert.Equal(t, map[string]int{f.a + "/name": 1, f.b + "/name": 1}, owedRaw(t, first))
					return
				}
				assert.Empty(t, resolvesIn(t, first))
				assert.Empty(t, owedRaw(t, first))
			})
		}
	}
}

// A load made current takes the live job's row before it writes any
// decision's, as a worker recording what became of a decision holds its
// job's row first: the two wait on each other in that order, and never
// deadlock.
func TestFoundryLoadMadeCurrentLocksTheJobFirst(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RVLOCK")
	first := f.load
	second := f.newLoad(t, "RVLOCK2")
	f.loaded(t, f.worker, f.a, f.b)
	_, err := f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, extractJob("RVLOCK"), resendJob("RVLOCK"), noBudget)
	require.NoError(t, err)
	acme := "acme"
	d := f.resolveOf("1", f.a, "acme corp", &acme)
	require.True(t, f.settleName(t, "1", d))
	live := f.claim(t)

	rootDSN := os.Getenv("HELIX_TEST_ROOT_DSN")
	if rootDSN == "" {
		rootDSN = "root:helix@tcp(127.0.0.1:3307)/"
	}
	root, err := sql.Open("mysql", rootDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	require.NoError(t, root.PingContext(ctx), "reading lock waits needs HELIX_TEST_ROOT_DSN")
	// The worker: its job's row, then a decision's.
	worker, err := testDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = worker.Rollback() })
	_, err = worker.ExecContext(ctx, `UPDATE foundry_load_jobs SET lease_until=lease_until WHERE id=?`, live.ID)
	require.NoError(t, err)
	var conn, trx uint64
	require.NoError(t, worker.QueryRowContext(ctx, `SELECT CONNECTION_ID()`).Scan(&conn))
	require.NoError(t, root.QueryRowContext(ctx, `SELECT trx_id FROM information_schema.innodb_trx WHERE trx_mysql_thread_id=?`, conn).Scan(&trx))

	made := make(chan error, 1)
	go func() {
		_, err := f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, second, extractJob("RVLOCK2"), resendJob("RVLOCK2"), noBudget)
		made <- err
	}()
	require.EventuallyWithT(t, func(collect *assert.CollectT) {
		var waiting int
		require.NoError(collect, root.QueryRowContext(ctx, `SELECT COUNT(*) FROM performance_schema.data_lock_waits
WHERE BLOCKING_ENGINE_TRANSACTION_ID=?`, trx).Scan(&waiting))
		assert.Positive(collect, waiting)
	}, 10*time.Second, 10*time.Millisecond, "making the load current waits on the job's row")
	_, err = worker.ExecContext(ctx, `UPDATE foundry_load_decisions SET applied_at=applied_at WHERE load_id=? AND id=?`, second, d.ID)
	require.NoError(t, err, "the worker records what became of the decision")
	require.NoError(t, worker.Commit())
	require.NoError(t, <-made)
	assert.Equal(t, map[string]string{d.ID: "carried"}, resolvesIn(t, second))
}

// The resend job owes the documents of its load whose reviews ask about a
// carried alias's name while that load is current, and holds the job's
// lease, and the copy then waits as pending. With the load no longer current
// none is owed, and the copy stays carried, its document's name held back,
// until the load is current again and a resend of it owes them.
func TestFoundryAskingDocumentsOwed(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RVASK")
	first := f.load
	second := f.newLoad(t, "RVASK2")
	acme := "acme"
	d := f.resolveOf("1", f.a, "acme corp", &acme)
	require.False(t, f.settleName(t, "1", d, f.b))
	f.loaded(t, f.worker, f.a, f.b)
	f.load = second
	job := f.claim(t)
	require.Equal(t, model.FoundryGraphJobResend, job.Kind)

	_, err := f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, extractJob("RVASK"), resendJob("RVASK"), noBudget)
	require.NoError(t, err)
	owed, err := f.db.OweFoundryAskingResends(ctx, job, f.worker, []string{d.ID}, []string{f.b})
	require.NoError(t, err)
	assert.False(t, owed)
	assert.Equal(t, map[string]string{d.ID: "carried"}, resolvesIn(t, second))
	assert.Equal(t, map[string]int{f.a + "/name": 1}, owedRaw(t, second))
	assert.Empty(t, f.owedFor(t), "its document waits with it")

	_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, second, extractJob("RVASK2"), resendJob("RVASK2"), noBudget)
	require.NoError(t, err)
	_, err = f.db.OweFoundryAskingResends(ctx, job, "w-other", []string{d.ID}, []string{f.b})
	assert.ErrorIs(t, err, store.ErrNotFound, "the lease is the job's worker's")
	owed, err = f.db.OweFoundryAskingResends(ctx, job, f.worker, []string{d.ID}, []string{f.b})
	require.NoError(t, err)
	assert.True(t, owed)
	assert.Equal(t, map[string]string{d.ID: "pending"}, resolvesIn(t, second))
	assert.Equal(t, map[string]int{f.a + "/name": 1, f.b + "/name": 1}, f.owedFor(t))
}

// A resend job that fails leaves an alias it read unconfirmed, as a pending
// one, and that document's request ends. One it never read (its asking
// documents yet to be owed: past carriedAliasesPerJob, or with the load no
// longer current) stays carried, its document owed and held back, and no job
// follows the failure for it alone: the next resend of the load reads it.
func TestFoundryCarriedAliasJobFailed(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RVFAIL")
	second := f.newLoad(t, "RVFAIL2")
	acme := "acme"
	read, unread := f.resolveOf("1", f.a, "acme corp", &acme), f.resolveOf("2", f.b, "helio", &acme)
	f.settleName(t, "1", read)
	f.settleName(t, "2", unread)
	f.loaded(t, f.worker, f.a, f.b)
	f.load = second
	job := f.claim(t)
	require.Equal(t, second, job.LoadID)
	owed, err := f.db.OweFoundryAskingResends(ctx, job, f.worker, []string{read.ID}, nil)
	require.NoError(t, err)
	require.True(t, owed)
	f.end(t, job, "rvfail", model.FoundryGraphJobFailed)
	assert.Equal(t, map[string]string{read.ID: "pending/" + model.FoundryReleaseJobFailed, unread.ID: "carried"}, f.applied(t, read, unread))
	assert.Equal(t, map[string]int{f.b + "/name": 1}, owedRaw(t, second), "the unread alias's document stays owed")
	assert.Empty(t, f.owedFor(t), "held back")
	assert.Nil(t, f.liveJob(t), "no job follows the failure for it alone")
	carried, err := f.db.ListFoundryCarriedAliases(ctx, second, 10)
	require.NoError(t, err)
	require.Len(t, carried, 1)
	assert.Equal(t, unread.ID, carried[0].ID)

	_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, second, extractJob("RVFAIL"), resendJob("RVFAIL"), noBudget)
	require.NoError(t, err)
	resend := f.liveJob(t)
	require.NotNil(t, resend, "the next resend of the load reads it")
	assert.Equal(t, second, resend.LoadID)
}
