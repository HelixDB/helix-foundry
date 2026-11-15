package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"

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
// load when it becomes current: it joins the list carried, with its name's
// key (an alias; "not in data" has none), and the load owes its document a
// resend for a name. The alias, whose document's request the resend sends,
// is read for the documents that ask about its name; the engine's word on
// its document lands on the carried copy as on a pending one, and then it is
// read no more. Carrying again owes nothing: a copy no word came of waits,
// and the next job's end queues no resend for it.
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
	assert.Equal(t, map[string]string{alias.ID: "carried", none.ID: "carried"}, f.applied(t, alias, none))
	var key, noKey sql.NullInt64
	require.NoError(t, testDB.QueryRow(`SELECT name_key FROM foundry_load_decisions WHERE load_id=? AND id=?`, second, alias.ID).Scan(&key))
	want, _ := namewords.Key("acme corp")
	assert.Equal(t, sql.NullInt64{Int64: want, Valid: true}, key, "found by the Extracts of the documents that write it")
	require.NoError(t, testDB.QueryRow(`SELECT name_key FROM foundry_load_decisions WHERE load_id=? AND id=?`, second, none.ID).Scan(&noKey))
	assert.False(t, noKey.Valid, "not in data carries nothing to other documents")
	assert.Equal(t, map[string]int{f.a + "/name": 1, f.b + "/name": 1}, f.owedFor(t), "their own documents, not those the first load's reviews asked")
	carried, err := f.db.ListFoundryCarriedAliases(ctx, second)
	require.NoError(t, err)
	require.Len(t, carried, 1, "an alias reaches the documents that ask; not in data doesn't")
	assert.Equal(t, alias.ID, carried[0].ID)
	resend := f.liveJob(t)
	require.NotNil(t, resend)
	assert.Equal(t, model.FoundryGraphJobResend, resend.Kind)
	assert.Equal(t, second, resend.LoadID)

	job = f.claim(t)
	f.nameWritten(t, job, alias)
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, &model.FoundryDocumentExtraction{
		DocumentID: f.b, Status: model.FoundryExtractionExtracted, SettlesNames: true}, true))
	for _, doc := range []string{f.a, f.b} {
		require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{DocumentID: doc, Reason: model.FoundryResendName, Requests: 1}))
	}
	assert.Equal(t, map[string]string{alias.ID: "written", none.ID: "carried"}, f.applied(t, alias, none))
	carried, err = f.db.ListFoundryCarriedAliases(ctx, second)
	require.NoError(t, err)
	assert.Empty(t, carried, "read once")
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "rvcarried", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t), "carried once: nothing is owed again")
	assert.Empty(t, f.owedFor(t))
	assert.Len(t, resolvesIn(t, second), 2, "one copy each")
	_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, second, extractJob("RVCARRY"), resendJob("RVCARRY"), noBudget)
	require.NoError(t, err)
	assert.Nil(t, f.liveJob(t), "made current again, it owes nothing")
}

// A decision the load holds by id, copied when it was created, is not
// carried again, and its document is not owed.
func TestFoundryResolveHeldByIDNotCarried(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RVHELD")
	acme := "acme"
	d := f.resolveOf("1", f.a, "acme corp", &acme)
	require.True(t, f.settleName(t, "1", d))
	job := f.claim(t)
	f.nameWritten(t, job, d)
	require.NoError(t, f.db.EndFoundryLoadResend(ctx, job.ID, f.worker, model.FoundryResendEnd{DocumentID: f.a, Reason: model.FoundryResendName, Requests: 1}))
	f.end(t, job, "rvheld", model.FoundryGraphJobSucceeded)

	second := f.newLoad(t, "RVHELD2")
	f.loaded(t, f.worker, f.a, f.b)
	require.Equal(t, second, *f.ontologyNow(t).CurrentLoadID)
	f.load = second
	assert.Equal(t, map[string]string{d.ID: ""}, resolvesIn(t, second), "the copy made with the load, as it was")
	assert.Empty(t, f.owedFor(t))
	assert.Nil(t, f.liveJob(t))
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
	f.load = second
	assert.Equal(t, map[string]string{changed.ID: "carried"}, resolvesIn(t, second))
	assert.Equal(t, map[string]int{f.a + "/name": 1}, f.owedFor(t))
	list, err := f.db.ListFoundryLoadDecisions(ctx, second, "", []string{f.a, f.b}, 100)
	require.NoError(t, err)
	require.Len(t, list, 1, "the document's Extract decides its name once")
	assert.Equal(t, "globex", *list[0].EntityKey)
}

// A resolve removed while a load that holds it waiting, or refused, is not
// current never applies there: when that load is current again, the job end
// that carries to it drops the copy, as removing it then would have.
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

	// The first is current again while the second's resend is live: that
	// job's end carries to it.
	_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, extractJob("RVBACK3"), resendJob("RVBACK3"), noBudget)
	require.NoError(t, err)
	job = f.claim(t)
	require.Equal(t, second, job.LoadID)
	f.end(t, job, "rvback-2", model.FoundryGraphJobSucceeded)
	assert.Empty(t, resolvesIn(t, first), "the removed choice is sent to no load")
	list, err := f.db.ListFoundryLoadDecisions(ctx, first, "", []string{f.a}, 100)
	require.NoError(t, err)
	assert.Empty(t, list)
}

// Another load became current between the person reading the review and
// settling it: the decision is saved, checked against the current load's
// version, and carried to that load, which owes its own document only; the
// documents read from the load the review came from are another load's, and
// the resend job reads the current load's. The load the review came from is
// left as it is. A current load whose version doesn't take the decision
// refuses it, and nothing is saved.
func TestFoundryResolveSettledAfterTheLoadChanged(t *testing.T) {
	ctx := context.Background()
	t.Run("carried to the current load", func(t *testing.T) {
		f := newResendFixture(t, "RVSWAP")
		first := f.load
		second := f.newLoad(t, "RVSWAP2")
		f.loaded(t, f.worker, f.a, f.b)
		require.Nil(t, f.liveJob(t))
		acme := "acme"
		d := f.resolveOf("1", f.a, "acme corp", &acme)
		queued, err := f.db.SettleFoundryResolve(ctx, f.ontology.WorkspaceID, first, d, []string{f.b},
			&model.FoundryGraphJob{ID: id26("RN-1"), RequestedByUserID: ptr(id26("user"))})
		require.NoError(t, err)
		assert.True(t, queued)
		require.NotNil(t, d.Applied)
		assert.Equal(t, model.FoundryReleaseCarried, d.Applied.Status)
		f.load = second
		assert.Equal(t, map[string]string{d.ID: "carried"}, resolvesIn(t, second))
		assert.Empty(t, resolvesIn(t, first))
		assert.Equal(t, map[string]int{f.a + "/name": 1}, f.owedFor(t))
		carried, err := f.db.ListFoundryCarriedAliases(ctx, second)
		require.NoError(t, err)
		require.Len(t, carried, 1)
		resend := f.liveJob(t)
		require.NotNil(t, resend)
		assert.Equal(t, id26("RN-1"), resend.ID)
		assert.Equal(t, second, resend.LoadID)
	})
	t.Run("a version that doesn't take it", func(t *testing.T) {
		f := newResendFixture(t, "RVSWAPV")
		first := f.load
		f.draftVersion(t, "RVSWAPV2", "[types.Vendor]\nprops = { name = \"string\" }")
		require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "RVSWAPV2")))
		f.loaded(t, f.worker, f.a, f.b)
		acme := "acme"
		_, err := f.db.SettleFoundryResolve(ctx, f.ontology.WorkspaceID, first, f.resolveOf("1", f.a, "acme corp", &acme), nil,
			&model.FoundryGraphJob{ID: id26("RN-1")})
		assert.ErrorIs(t, err, store.ErrConflict)
		assert.Empty(t, f.decisionIDs(t))
	})
}

// A settle and the end of the load job that makes another load current run
// at once, in either order under the ontology's lock: the new current load
// holds one copy of the decision, carried, and owes its document alone.
func TestFoundryResolveSettledAsTheLoadChanges(t *testing.T) {
	ctx := context.Background()
	acme := "acme"
	for i := range 4 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			tag := fmt.Sprintf("RVRACE%d", i)
			f := newResendFixture(t, tag)
			first := f.load
			second := f.newLoad(t, tag+"B")
			job := f.claim(t)
			require.Equal(t, second, job.ID)
			for _, doc := range []string{f.a, f.b} {
				require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(doc), false))
			}
			d := f.resolveOf("1", f.a, "acme corp", &acme)
			var wg sync.WaitGroup
			var settleErr, endErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, settleErr = f.db.SettleFoundryResolve(ctx, f.ontology.WorkspaceID, first, d, []string{f.b}, &model.FoundryGraphJob{ID: id26("RN-" + tag)})
			}()
			go func() {
				defer wg.Done()
				_, endErr = f.db.FinishFoundryGraphJob(ctx, job.ID, f.worker, model.FoundryGraphJobOutcome{
					Status: model.FoundryGraphJobSucceeded, IngestDone: true, Next: next(tag)})
			}()
			wg.Wait()
			require.NoError(t, settleErr)
			require.NoError(t, endErr)
			require.Equal(t, second, *f.ontologyNow(t).CurrentLoadID)
			f.load = second
			assert.Equal(t, map[string]string{d.ID: "carried"}, resolvesIn(t, second))
			assert.Equal(t, map[string]int{f.a + "/name": 1}, f.owedFor(t))
			resend := f.liveJob(t)
			require.NotNil(t, resend)
			assert.Equal(t, second, resend.LoadID)
		})
	}
}

// The resend job owes the documents of its load whose reviews ask about a
// carried alias's name while that load is current, and holds the job's
// lease; a load no longer current is owed none.
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

	owed, err := f.db.OweFoundryAskingResends(ctx, job, f.worker, []string{f.b})
	require.NoError(t, err)
	assert.True(t, owed)
	assert.Equal(t, map[string]int{f.a + "/name": 1, f.b + "/name": 1}, f.owedFor(t))
	_, err = f.db.OweFoundryAskingResends(ctx, job, "w-other", []string{f.b})
	assert.ErrorIs(t, err, store.ErrNotFound, "the lease is the job's worker's")

	_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, extractJob("RVASK"), resendJob("RVASK"), noBudget)
	require.NoError(t, err)
	owed, err = f.db.OweFoundryAskingResends(ctx, job, f.worker, []string{f.b})
	require.NoError(t, err)
	assert.False(t, owed)
	assert.Equal(t, map[string]int{f.a + "/name": 1, f.b + "/name": 1}, f.owedFor(t), "nothing more")
}
