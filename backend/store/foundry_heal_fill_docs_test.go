package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moved records the fixture's load moved for finding tag (from and to its
// version, as a move it made), with the finding the heal worker stored: a
// fill due to collect.
func (f resendFixture) moved(t *testing.T, tag string) *model.FoundryHealFill {
	t.Helper()
	at, _ := f.loadVersion(t)
	f.moveRow(t, tag, at, at, model.FoundryFillMoved)
	_, err := testDB.Exec(`UPDATE foundry_heal_fills SET links_added='["mentors"]' WHERE id=?`, id26("HF-"+tag))
	require.NoError(t, err)
	_, err = testDB.Exec(`INSERT INTO foundry_heal_findings (ontology_id, fingerprint, kind, status, base_version_id, load_id, finding, support, sources)
VALUES (?, ?, 'unmatched', 'open', ?, ?, '{"signal":{"kind":"unmatched","canonical":"mentors","spellings":["mentors"],"set":"set_1"}}', 3, 3)`,
		f.ontology.ID, fingerprint(tag), at, f.load)
	require.NoError(t, err)
	fills, err := f.db.ListFoundryHealFills(context.Background(), f.load)
	require.NoError(t, err)
	return fills[fingerprint(tag)]
}

// fill reads the fixture's load's fill of finding tag.
func (f resendFixture) fill(t *testing.T, tag string) *model.FoundryHealFill {
	t.Helper()
	fills, err := f.db.ListFoundryHealFills(context.Background(), f.load)
	require.NoError(t, err)
	return fills[fingerprint(tag)]
}

// linksOwed counts the fixture's load's owed 'link' rows.
func (f resendFixture) linksOwed(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_resends WHERE load_id=? AND reason='link' AND status='owed'`, f.load).Scan(&n))
	return n
}

// extraction reads a document's facts and reviews in the fixture's load.
func (f resendFixture) extraction(t *testing.T, doc string) (facts, reviews int) {
	t.Helper()
	require.NoError(t, testDB.QueryRow(`SELECT facts, reviews FROM foundry_load_extractions WHERE load_id=? AND document_id=?`, f.load, doc).
		Scan(&facts, &reviews))
	return facts, reviews
}

// A moved load's fill: due to collect, its documents owed but counted only
// once it applies, then started with a resend queued; each document's word
// ends its request and is counted once, a retry's included, and the last
// ends the fill filled, queueing nothing more.
func TestFoundryFillCollectsSendsAndEnds(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILL")
	fill := f.moved(t, "FILL")
	due, err := f.db.DueFoundryHealFills(ctx, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.False(t, due[0].Resume)
	assert.Equal(t, fill.ID, due[0].Fill.ID)
	assert.Equal(t, f.load, due[0].Due.Load.ID)
	assert.Contains(t, string(due[0].Finding), `"spellings":["mentors"]`)
	ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a, f.b})
	require.NoError(t, err)
	assert.True(t, ok)
	// Owed, but not yet counted: the fill doesn't apply yet.
	assert.Equal(t, 2, f.linksOwed(t))
	listed, err := f.db.ListFoundryLinkResends(ctx, f.load, 10)
	require.NoError(t, err)
	assert.Empty(t, listed)
	assert.Nil(t, f.liveJob(t))
	assert.Empty(t, f.owed(t), "a fill's documents are no approval's")
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, 2, 1, &model.FoundryGraphJob{ID: id26("RS-FILL")}))
	got := f.fill(t, "FILL")
	assert.Equal(t, model.FoundryFillApplying, got.State)
	assert.Equal(t, model.FoundryFillCounts{Owed: 2, Left: 1}, got.Counts)
	assert.Equal(t, model.FoundryFillCapUSD, got.CapUSD)
	assert.ErrorIs(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, 2, 1, &model.FoundryGraphJob{ID: id26("RS-FILL2")}), store.ErrConflict)
	// No longer due to collect; and a job is live, so not to resume.
	due, err = f.db.DueFoundryHealFills(ctx, 10)
	require.NoError(t, err)
	assert.Empty(t, due)
	job := f.claim(t)
	assert.Equal(t, model.FoundryGraphJobResend, job.Kind)
	listed, err = f.db.ListFoundryLinkResends(ctx, f.load, 10)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	for _, r := range listed {
		require.NotNil(t, r.Document, r.DocumentID)
		assert.Equal(t, model.FoundryResendLink, r.Reason)
	}
	factsA, reviewsA := f.extraction(t, f.a)
	written := model.FoundryFillDocument{DocumentID: f.a, Requests: 1, Outcome: "written", Facts: 2, Held: 1, ReviewsTaken: 3, USDMicros: 1500}
	require.NoError(t, f.db.RecordFoundryFillDocument(ctx, job.ID, f.worker, written))
	// Again, as a retry after a lost event says it: counted once, but what
	// its calls spent is spent again.
	require.NoError(t, f.db.RecordFoundryFillDocument(ctx, job.ID, f.worker, written))
	facts, reviews := f.extraction(t, f.a)
	assert.Equal(t, factsA+2, facts)
	assert.Equal(t, max(reviewsA+1-3, 0), reviews)
	got = f.fill(t, "FILL")
	assert.Equal(t, model.FoundryFillApplying, got.State)
	assert.Equal(t, model.FoundryFillCounts{Owed: 2, Left: 1, Filled: 1, ReviewsTaken: 3, FactsAdded: 2}, got.Counts)
	assert.InDelta(t, 0.003, got.SpentUSD, 1e-9)
	var jobSpent int64
	require.NoError(t, testDB.QueryRow(`SELECT usd_micros FROM foundry_load_jobs WHERE id=?`, job.ID).Scan(&jobSpent))
	assert.Equal(t, int64(3000), jobSpent, "the job's spend, which the day's cap reads")
	var owedCards int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_review_owed WHERE load_id=? AND doc_id=?`, f.load, f.a).Scan(&owedCards))
	assert.Equal(t, 1, owedCards, "its review cards are rebuilt")
	// A word on a request the job didn't read ends nothing.
	require.NoError(t, f.db.RecordFoundryFillDocument(ctx, job.ID, f.worker, model.FoundryFillDocument{DocumentID: f.b, Requests: 9, Outcome: "nothing"}))
	assert.Equal(t, 1, f.linksOwed(t))
	// A native document's commit counted what it states: the fill adds none.
	require.NoError(t, f.db.RecordFoundryFillDocument(ctx, job.ID, f.worker,
		model.FoundryFillDocument{DocumentID: f.b, Requests: 1, Outcome: "native", Facts: 5, ReviewsTaken: 5}))
	got = f.fill(t, "FILL")
	assert.Equal(t, model.FoundryFillFilled, got.State)
	assert.Equal(t, model.FoundryFillCounts{Owed: 2, Left: 1, Filled: 1, Native: 1, ReviewsTaken: 3, FactsAdded: 2}, got.Counts)
	assert.Equal(t, int64(2), got.Counts.Done())
	assert.Zero(t, f.linksOwed(t))
	// The job's end queues nothing: the fill owes none.
	f.end(t, job, "filled", model.FoundryGraphJobSucceeded)
	assert.Nil(t, f.liveJob(t))
	// A lost lease writes nothing.
	assert.ErrorIs(t, f.db.RecordFoundryFillDocument(ctx, job.ID, f.worker, written), store.ErrNotFound)
	spent, err := f.db.SumFoundryFillSpend(ctx, f.ontology.ID, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(3000), spent)
	spent, err = f.db.SumFoundryFillSpend(ctx, f.ontology.ID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Zero(t, spent, "only jobs since")
}

// A fill that owes nothing, its reviews gone, is filled at once.
func TestFoundryFillWithNothingOwedIsFilled(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILL0")
	fill := f.moved(t, "FILL0")
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, 0, 0, &model.FoundryGraphJob{ID: id26("RS-FILL0")}))
	assert.Equal(t, model.FoundryFillFilled, f.fill(t, "FILL0").State)
	assert.Nil(t, f.liveJob(t))
}

// A failed job pauses the fill, its documents queueing nothing meanwhile;
// the heal worker resumes it once the pause ends; past the backoff the
// fill stops, its documents no longer owed.
func TestFoundryFillBacksOffThenStops(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLF")
	fill := f.moved(t, "FILLF")
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a})
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLF")}))
	for i := range len(model.FoundryFillBackoff) {
		job := f.claim(t)
		f.end(t, job, "failed-"+string(rune('a'+i)), model.FoundryGraphJobFailed)
		got := f.fill(t, "FILLF")
		require.Equal(t, model.FoundryFillApplying, got.State, "failure %d", i+1)
		require.NotNil(t, got.PausedUntil)
		assert.WithinDuration(t, time.Now().Add(model.FoundryFillBackoff[i]), *got.PausedUntil, time.Minute)
		assert.Nil(t, f.liveJob(t), "a paused fill queues no job")
		listed, err := f.db.ListFoundryLinkResends(ctx, f.load, 10)
		require.NoError(t, err)
		assert.Empty(t, listed, "a paused fill's documents aren't sent")
		due, err := f.db.DueFoundryHealFills(ctx, 10)
		require.NoError(t, err)
		assert.Empty(t, due, "not due while paused")
		_, err = testDB.Exec(`UPDATE foundry_heal_fills SET paused_until=NOW(6) - INTERVAL 1 SECOND WHERE load_id=?`, f.load)
		require.NoError(t, err)
		due, err = f.db.DueFoundryHealFills(ctx, 10)
		require.NoError(t, err)
		require.Len(t, due, 1)
		require.True(t, due[0].Resume)
		queued, err := f.db.ResumeFoundryFill(ctx, due[0].Fill, f.ontology.WorkspaceID, &model.FoundryGraphJob{ID: id26("RS-FILLF" + string(rune('a'+i)))})
		require.NoError(t, err)
		require.True(t, queued)
	}
	job := f.claim(t)
	f.end(t, job, "failed-last", model.FoundryGraphJobFailed)
	got := f.fill(t, "FILLF")
	assert.Equal(t, model.FoundryFillStopped, got.State)
	assert.Equal(t, model.FoundryFillModelUnavailable, got.StopReason)
	assert.Zero(t, f.linksOwed(t))
	assert.Nil(t, f.liveJob(t))
}

// A cap reached stops the fill; a day's cap pauses it; a dismissal stops a
// fill under way and one waiting to start.
func TestFoundryFillStoppedPausedOrDismissed(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLS")
	fill := f.moved(t, "FILLS")
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a, f.b})
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, 2, 0, &model.FoundryGraphJob{ID: id26("RS-FILLS")}))
	job := f.claim(t)
	until := time.Now().Add(24 * time.Hour)
	require.NoError(t, f.db.PauseFoundryFill(ctx, job.ID, f.worker, f.load, until))
	got := f.fill(t, "FILLS")
	require.NotNil(t, got.PausedUntil)
	assert.WithinDuration(t, until, *got.PausedUntil, time.Second)
	assert.Equal(t, 2, f.linksOwed(t), "paused, still owed")
	require.NoError(t, f.db.StopFoundryFill(ctx, job.ID, f.worker, f.load, model.FoundryFillCapReached))
	got = f.fill(t, "FILLS")
	assert.Equal(t, model.FoundryFillStopped, got.State)
	assert.Equal(t, model.FoundryFillCapReached, got.StopReason)
	assert.Nil(t, got.PausedUntil)
	assert.Zero(t, f.linksOwed(t))
	assert.Error(t, f.db.StopFoundryFill(ctx, job.ID, f.worker, id26("OTHER"), model.FoundryFillCapReached), "of another load")

	g := newResendFixture(t, "FILLD")
	gFill := g.moved(t, "FILLD")
	_, err = g.db.OweFoundryFillDocuments(ctx, gFill, g.ontology.WorkspaceID, []string{g.a})
	require.NoError(t, err)
	require.NoError(t, g.db.StartFoundryFill(ctx, gFill, g.ontology.WorkspaceID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLD")}))
	require.NoError(t, g.db.DismissFoundryHealFinding(ctx, g.ontology.ID, fingerprint("FILLD"), id26("user")))
	got = g.fill(t, "FILLD")
	assert.Equal(t, model.FoundryFillStopped, got.State)
	assert.Equal(t, model.FoundryFillDismissed, got.StopReason)
	assert.Zero(t, g.linksOwed(t))

	h := newResendFixture(t, "FILLW")
	h.moved(t, "FILLW")
	require.NoError(t, h.db.DismissFoundryHealFinding(ctx, h.ontology.ID, fingerprint("FILLW"), id26("user")))
	assert.Equal(t, model.FoundryFillStopped, h.fill(t, "FILLW").State)
	due, err := h.db.DueFoundryHealFills(ctx, 10)
	require.NoError(t, err)
	for _, d := range due {
		assert.NotEqual(t, h.load, d.Fill.LoadID, "a dismissed fill is never collected")
	}
	// The moved fill still records what the load gained: its documents ask
	// the link apart.
	asked, err := h.db.FoundryAskedApart(ctx, h.load)
	require.NoError(t, err)
	assert.Equal(t, []string{"mentors"}, asked)
}

// A document the load doesn't hold, or holds unextracted, can't be sent:
// it ends refused, counted once.
func TestFoundryFillDocumentUnsendable(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLU")
	fill := f.moved(t, "FILLU")
	gone := id26("FD-FILLU-gone")
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a, gone})
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, 2, 0, &model.FoundryGraphJob{ID: id26("RS-FILLU")}))
	job := f.claim(t)
	listed, err := f.db.ListFoundryLinkResends(ctx, f.load, 10)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	byDoc := map[string]*model.FoundryLoadResend{}
	for _, r := range listed {
		byDoc[r.DocumentID] = r
	}
	require.NotNil(t, byDoc[f.a].Document)
	require.Nil(t, byDoc[gone].Document)
	assert.Equal(t, model.FoundryReleaseDocumentGone, byDoc[gone].Unsendable)
	require.NoError(t, f.db.EndFoundryFillDocument(ctx, job.ID, f.worker, gone, byDoc[gone].Requests, byDoc[gone].Unsendable))
	got := f.fill(t, "FILLU")
	assert.Equal(t, int64(1), got.Counts.Refused)
	// A load moved again elsewhere, or no longer current: no document is owed.
	_, err = testDB.Exec(`UPDATE foundry_heal_fills SET state='moved', active_key=NULL WHERE id=?`, fill.ID)
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_ontologies SET current_load_id=NULL WHERE id=?`, f.ontology.ID)
	require.NoError(t, err)
	ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.b})
	require.NoError(t, err)
	assert.False(t, ok)
	assert.ErrorIs(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLU2")}), store.ErrConflict)
}
