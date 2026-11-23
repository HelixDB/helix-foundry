package store_test

import (
	"context"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/reviewcards"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// settleOne settles one item as the API's single settle does: a batch of
// one that doesn't expect its card open, whose refusal is its error.
func settleOne(ctx context.Context, db store.Store, ws, load string, it *model.FoundrySettle, resend *model.FoundryGraphJob) (bool, error) {
	refusals, queued, err := db.SettleFoundryReviews(ctx, ws, it.D.OntologyID, load, []*model.FoundrySettle{it}, false, resend)
	if err != nil {
		return false, err
	}
	if r := refusals[0]; r != nil {
		return false, r.Err
	}
	return queued, nil
}

// settleResolve settles a resolve of a name the load asks about, owing
// asking besides its own document.
func settleResolve(db store.Store, ctx context.Context, ws, load string, d *model.FoundryReviewDecision, asking []string,
	resend *model.FoundryGraphJob) (bool, error) {
	return settleOne(ctx, db, ws, load, &model.FoundrySettle{D: d, Name: model.FoundryNameAsking{Documents: asking}}, resend)
}

// settleBound settles a resolve of a name the load links already.
func settleBound(db store.Store, ctx context.Context, ws, load string, d *model.FoundryReviewDecision, resend *model.FoundryGraphJob) (bool, error) {
	return settleOne(ctx, db, ws, load, &model.FoundrySettle{D: d, Name: model.FoundryNameLinked{}}, resend)
}

func settleValue(db store.Store, ctx context.Context, ws, load string, d *model.FoundryReviewDecision, resend *model.FoundryGraphJob) (bool, error) {
	return settleOne(ctx, db, ws, load, &model.FoundrySettle{D: d}, resend)
}

func settleReject(db store.Store, ctx context.Context, ws, load string, d *model.FoundryReviewDecision) error {
	_, err := settleOne(ctx, db, ws, load, &model.FoundrySettle{D: d}, nil)
	return err
}

// settleReleases settles ds in one batch, as Approve all does: the first
// refusal is its error.
func settleReleases(db store.Store, ctx context.Context, ws, load string, ds []*model.FoundryReviewDecision,
	resend *model.FoundryGraphJob) (bool, error) {
	if len(ds) == 0 {
		return false, nil
	}
	items := make([]*model.FoundrySettle, len(ds))
	for i, d := range ds {
		items[i] = &model.FoundrySettle{D: d}
	}
	refusals, queued, err := db.SettleFoundryReviews(ctx, ws, ds[0].OntologyID, load, items, false, resend)
	if err != nil {
		return false, err
	}
	for _, r := range refusals {
		if r != nil {
			return queued, r.Err
		}
	}
	return queued, nil
}

// batch settles items as job RB-<tag>, expecting their cards open or not.
func (f resendFixture) batch(t *testing.T, tag, load string, expectOpen bool, items ...*model.FoundrySettle) ([]*model.FoundrySettleRefusal, bool) {
	t.Helper()
	refusals, queued, err := f.db.SettleFoundryReviews(context.Background(), f.ontology.WorkspaceID, f.ontology.ID, load, items, expectOpen,
		&model.FoundryGraphJob{ID: id26("RB-" + tag), RequestedByUserID: ptr(id26("user"))})
	require.NoError(t, err)
	require.Len(t, refusals, len(items))
	return refusals, queued
}

func item(d *model.FoundryReviewDecision) *model.FoundrySettle { return &model.FoundrySettle{D: d} }
func asking(d *model.FoundryReviewDecision, docs ...string) *model.FoundrySettle {
	return &model.FoundrySettle{D: d, Name: model.FoundryNameAsking{Documents: docs}}
}

// by is d decided by user (an id26 tag), under id tag.
func by(d *model.FoundryReviewDecision, tag, user string) *model.FoundryReviewDecision {
	d.ID, d.DecidedByUserID = id26(tag), id26(user)
	return d
}

func (f resendFixture) resends(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_jobs WHERE ontology_id=? AND kind='resend'`, f.ontology.ID).Scan(&n))
	return n
}

// The shape of an item: a resolve, and only a resolve, says how its load
// holds the name, and a withdraw is never settled. A malformed item refuses
// the batch, saving nothing.
func TestFoundrySettleValidate(t *testing.T) {
	f := newResendFixture(t, "SVAL")
	resolve := f.resolveOf("1", f.a, "acme", nil)
	withdraw := f.held(f.a, 0, readA)
	withdraw.Kind = model.FoundryReviewWithdraw
	for name, it := range map[string]*model.FoundrySettle{
		"no decision":         {},
		"resolve, no name":    item(resolve),
		"release with a name": {D: f.held(f.a, 0, readA), Name: model.FoundryNameLinked{}},
		"withdraw":            item(withdraw),
	} {
		assert.Error(t, it.Validate(), name)
		_, _, err := f.db.SettleFoundryReviews(context.Background(), f.ontology.WorkspaceID, f.ontology.ID, f.load,
			[]*model.FoundrySettle{item(f.held(f.b, 1, readA)), it}, false, nil)
		assert.Error(t, err, name)
	}
	assert.NoError(t, asking(resolve).Validate())
	assert.NoError(t, (&model.FoundrySettle{D: resolve, Name: model.FoundryNameLinked{}}).Validate())
	assert.Empty(t, f.decisionIDs(t))
	// Another ontology's decision is no item of this one.
	other := f.held(f.a, 0, readA)
	other.OntologyID = id26("another-ontology")
	_, _, err := f.db.SettleFoundryReviews(context.Background(), f.ontology.WorkspaceID, f.ontology.ID, f.load, []*model.FoundrySettle{item(other)}, false, nil)
	assert.Error(t, err)
	refusals, queued, err := f.db.SettleFoundryReviews(context.Background(), f.ontology.WorkspaceID, f.ontology.ID, f.load, nil, false, nil)
	assert.NoError(t, err)
	assert.Empty(t, refusals)
	assert.False(t, queued)
}

// One batch of all four kinds: each applied as its kind is, one resend job
// for all of them, and the open counts marked dirty.
func TestFoundrySettleBatchOfEveryKind(t *testing.T) {
	f := newResendFixture(t, "SMIX")
	_, err := testDB.Exec(`INSERT INTO foundry_review_loads (load_id, ontology_id, dirty_at) VALUES (?, ?, '2000-01-01')
ON DUPLICATE KEY UPDATE dirty_at='2000-01-01'`, f.load, f.ontology.ID)
	require.NoError(t, err)
	resolve := f.resolveOf("1", f.a, "acme corp", ptr("acme"))
	value := f.value("1", "initech", "gold")
	release := f.held(f.a, 0, readA)
	reject := f.rejected(f.b, 1, readA)
	refusals, queued := f.batch(t, "1", f.load, false, asking(resolve, f.b), item(value), item(release), item(reject))
	assert.Equal(t, []*model.FoundrySettleRefusal{nil, nil, nil, nil}, refusals)
	assert.True(t, queued)
	assert.Equal(t, 1, f.resends(t), "one resend for the batch")
	for d, want := range map[*model.FoundryReviewDecision]model.FoundryReleaseStatus{resolve: model.FoundryReleasePending,
		value: model.FoundryReleasePending, release: model.FoundryReleasePending} {
		require.NotNil(t, d.Applied, d.ID)
		assert.Equal(t, want, d.Applied.Status, d.ID)
	}
	assert.Nil(t, reject.Applied, "a reject goes to no load")
	assert.ElementsMatch(t, []string{resolve.ID, value.ID, release.ID, reject.ID}, f.decisionIDs(t))
	assert.Equal(t, map[string]int{f.a + "/" + string(model.FoundryResendName): 1, f.b + "/" + string(model.FoundryResendName): 1,
		f.a + "/" + string(model.FoundryResendApproval): 1}, f.owedFor(t))
	var dirty time.Time
	require.NoError(t, testDB.QueryRow(`SELECT dirty_at FROM foundry_review_loads WHERE load_id=?`, f.load).Scan(&dirty))
	assert.True(t, dirty.After(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)), "marked dirty")
	// A batch of rejects alone owes nothing and queues nothing.
	refusals, queued = f.batch(t, "2", f.load, false, item(f.rejected(f.b, 2, readA)))
	assert.Nil(t, refusals[0])
	assert.False(t, queued)
}

// A refused item rolls back to its savepoint: the items around it are
// saved, and nothing of it is. A batch of refusals alone marks nothing.
func TestFoundrySettleRefusedItem(t *testing.T) {
	f := newResendFixture(t, "SREF")
	first, last := f.held(f.a, 0, readA), f.held(f.b, 0, readA)
	stale := f.value("stale", "initech", "gold")
	stale.Type = "Nope" // the load's version has no such type
	refusals, queued := f.batch(t, "1", f.load, false, item(first), item(stale), item(last))
	assert.Nil(t, refusals[0])
	require.NotNil(t, refusals[1])
	assert.Equal(t, model.FoundrySettleStale, refusals[1].Reason)
	assert.ErrorIs(t, refusals[1].Err, store.ErrConflict)
	assert.Nil(t, refusals[2])
	assert.True(t, queued)
	assert.ElementsMatch(t, []string{first.ID, last.ID}, f.decisionIDs(t))
	assert.Equal(t, id26("VD-stale"), stale.ID, "a refused item comes back as it was sent")
	assert.Nil(t, stale.Applied)

}

// The review was read from a load that is no longer current: a resolve
// carries to the one that is, every other kind is stale, and the one resend
// the resolve owes is queued.
func TestFoundrySettleLoadMovedBeforeTheLock(t *testing.T) {
	f := newResendFixture(t, "SMOVE")
	resolve := f.resolveOf("1", f.a, "acme corp", ptr("acme"))
	refusals, queued := f.batch(t, "1", id26("another-load"), true, asking(resolve, f.b), item(f.value("1", "initech", "gold")),
		item(f.held(f.a, 0, readA)), item(f.rejected(f.b, 1, readA)))
	assert.Nil(t, refusals[0])
	for _, r := range refusals[1:] {
		require.NotNil(t, r)
		assert.Equal(t, model.FoundrySettleStale, r.Reason)
	}
	assert.True(t, queued)
	assert.Equal(t, 1, f.resends(t))
	require.NotNil(t, resolve.Applied)
	assert.Equal(t, model.FoundryReleaseCarried, resolve.Applied.Status, "carried to the current load")
	assert.Equal(t, []string{resolve.ID}, f.decisionIDs(t))
}

// With expect open, another person's decision of the card refuses the item
// as decided, with their decision, whatever it chose; one's own earlier
// choice is settled again; a card closed only through another document's
// link of the name, or by an approval of what the claim said before, is
// open.
func TestFoundrySettleExpectOpen(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "SOPEN")
	acme, globex := "acme", "globex"
	// Ada links "acme corp" in a to Acme, approves claim a0, and decides
	// Initech's tier.
	adaName := by(f.resolveOf("ada", f.a, "acme corp", &acme), "ada-name", "ada")
	adaClaim := by(f.held(f.a, 0, readA), "ada-claim", "ada")
	adaValue := by(f.value("ada", "initech", "gold"), "ada-value", "ada")
	refusals, _ := f.batch(t, "ada", f.load, true, asking(adaName), item(adaClaim), item(adaValue))
	require.Equal(t, []*model.FoundrySettleRefusal{nil, nil, nil}, refusals)

	decided := func(r *model.FoundrySettleRefusal, want *model.FoundryReviewDecision, what string) {
		t.Helper()
		require.NotNil(t, r, what)
		assert.Equal(t, model.FoundrySettleDecided, r.Reason, what)
		assert.ErrorIs(t, r.Err, store.FoundryReviewDecidedElsewhere, what)
		require.NotNil(t, r.Decided, what)
		assert.Equal(t, want.ID, r.Decided.ID, what)
		assert.Equal(t, id26("ada"), r.Decided.DecidedByUserID, what)
	}
	// Alan, on the same cards: a different choice and the same one alike.
	refusals, queued := f.batch(t, "alan", f.load, true,
		asking(by(f.resolveOf("alan-1", f.a, "acme corp", &globex), "alan-1", "alan")),
		asking(by(f.resolveOf("alan-2", f.a, "acme corp", &acme), "alan-2", "alan")),
		item(by(f.held(f.a, 0, readA), "alan-3", "alan")),
		item(by(f.rejected(f.a, 0, readA), "alan-4", "alan")),
		item(by(f.value("alan", "initech", "silver"), "alan-5", "alan")),
		item(by(f.value("alan", "initech", "gold"), "alan-6", "alan")))
	decided(refusals[0], adaName, "another entity")
	decided(refusals[1], adaName, "the same entity")
	decided(refusals[2], adaClaim, "the same approval")
	decided(refusals[3], adaClaim, "a rejection")
	decided(refusals[4], adaValue, "another value")
	decided(refusals[5], adaValue, "the same value")
	assert.False(t, queued)
	stored := f.stored(t, adaName.ID)
	assert.Equal(t, acme, *stored.EntityKey, "Ada's link stands")
	assert.Equal(t, id26("ada"), stored.DecidedByUserID)

	// Ada again, on her own cards: settled as today.
	refusals, _ = f.batch(t, "ada-2", f.load, true, asking(by(f.resolveOf("ada-3", f.a, "acme corp", &globex), "ada-3", "ada")))
	assert.Nil(t, refusals[0])

	// Alan links "acme corp" in b, which Ada's link in a reaches as an
	// alias, and approves claim a0 as it reads now, after a rebuild changed
	// what it says: both cards are open.
	refusals, _ = f.batch(t, "alan-2", f.load, true, asking(by(f.resolveOf("alan-b", f.b, "acme corp", &acme), "alan-b", "alan")),
		item(by(f.held(f.a, 0, readB), "alan-moved", "alan")))
	assert.Equal(t, []*model.FoundrySettleRefusal{nil, nil}, refusals)

	// Without expect open, the single settle saves as it always has.
	_, err := settleValue(f.db, ctx, f.ontology.WorkspaceID, f.load, by(f.value("alan-7", "initech", "silver"), "alan-7", "alan"),
		resendJob("SOPEN-7"))
	assert.NoError(t, err)
}

// Two people's batches over overlapping cards, at once on two connections:
// each card is settled by exactly one, the other is told it was decided,
// and no decision is lost.
func TestFoundrySettleConcurrentBatches(t *testing.T) {
	f := newResendFixture(t, "SCONC")
	users := []string{"ada", "alan"}
	// Ada: claims 0 and 1; Alan: claims 1 and 2, of document a.
	spans := [][]int{{0, 1}, {1, 2}}
	refusals := make([][]*model.FoundrySettleRefusal, 2)
	var wg sync.WaitGroup
	for i, user := range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items := make([]*model.FoundrySettle, 0, 2)
			for _, n := range spans[i] {
				items = append(items, item(by(f.held(f.a, n, readA), user+"-"+string(rune('0'+n)), user)))
			}
			out, _, err := f.db.SettleFoundryReviews(context.Background(), f.ontology.WorkspaceID, f.ontology.ID, f.load, items, true,
				&model.FoundryGraphJob{ID: id26("RB-" + user), RequestedByUserID: ptr(id26(user))})
			assert.NoError(t, err)
			refusals[i] = out
		}()
	}
	wg.Wait()
	require.Len(t, refusals[0], 2)
	require.Len(t, refusals[1], 2)
	assert.Nil(t, refusals[0][0])
	assert.Nil(t, refusals[1][1])
	// Claim 1: exactly one of them.
	won := refusals[0][1] == nil
	assert.NotEqual(t, won, refusals[1][0] == nil, "exactly one settles the shared card")
	loser := refusals[1][0]
	if !won {
		loser = refusals[0][1]
	}
	require.NotNil(t, loser)
	assert.Equal(t, model.FoundrySettleDecided, loser.Reason)
	assert.Len(t, f.decisionIDs(t), 3, "one decision a card, none lost")
}

// A failure that isn't a refusal (here a lock wait that times out on item
// two's row) aborts the batch: item one, already written, is not saved.
func TestFoundrySettleAbortsOnADriverError(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "SABORT")
	prior := f.value("prior", "initech", "gold")
	_, err := settleValue(f.db, ctx, f.ontology.WorkspaceID, f.load, prior, resendJob("SABORT-prior"))
	require.NoError(t, err)
	before := f.decisionIDs(t)

	impatient, err := openWithRetry(testDSN+"&innodb_lock_wait_timeout=1", 1, 0)
	require.NoError(t, err)
	defer impatient.Close()
	holder, err := testDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = holder.Rollback() }()
	_, err = holder.ExecContext(ctx, `SELECT id FROM foundry_review_decisions WHERE id=? FOR UPDATE`, prior.ID)
	require.NoError(t, err)

	db := store.NewMySQL(impatient, hostWorkspaces{})
	_, _, err = db.SettleFoundryReviews(ctx, f.ontology.WorkspaceID, f.ontology.ID, f.load,
		[]*model.FoundrySettle{item(f.held(f.a, 0, readA)), item(f.value("next", "initech", "silver"))}, false,
		&model.FoundryGraphJob{ID: id26("RB-abort")})
	require.Error(t, err)
	require.NoError(t, holder.Rollback())
	assert.Equal(t, before, f.decisionIDs(t), "nothing of the batch is saved")
	assert.Equal(t, 1, f.resends(t), "the prior settle's resend alone")
	var copies int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_decisions WHERE load_id=? AND kind='release'`, f.load).Scan(&copies))
	assert.Zero(t, copies, "nor its copy in the load")
}

// One card read by its id, with whether it is open.
func TestGetFoundryReviewCard(t *testing.T) {
	ctx := context.Background()
	f := newCardsFixture(t, "rget")
	f.pass(t)
	var value *model.FoundryReviewListedCard
	for _, c := range f.list(t, model.FoundryReviewCardFilter{}) {
		if c.Kind == reviewcards.KindValue && c.DocID == f.a {
			value = c
		}
	}
	require.NotNil(t, value)
	card, open, err := f.db.GetFoundryReviewCard(ctx, f.load, value.ID)
	require.NoError(t, err)
	assert.True(t, open)
	assert.Equal(t, value.ReviewKey, card.ReviewKey)
	assert.Equal(t, hex.EncodeToString(value.ID[:]), hex.EncodeToString(card.ID[:]))

	f.decide(t, "rget-v", "prop", "Customer", nil, nil, "scus_1", "currency", `"EUR"`, nil, nil, nil)
	_, open, err = f.db.GetFoundryReviewCard(ctx, f.load, value.ID)
	require.NoError(t, err)
	assert.False(t, open, "decided")

	_, _, err = f.db.GetFoundryReviewCard(ctx, id26("another-load"), value.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, _, err = f.db.GetFoundryReviewCard(ctx, f.load, reviewcards.ID{1})
	assert.ErrorIs(t, err, store.ErrNotFound)
}
