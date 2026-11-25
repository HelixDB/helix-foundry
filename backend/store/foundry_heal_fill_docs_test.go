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

// collecting claims fill's collect, as a heal worker does before it owes
// anything (ClaimFoundryFillCollect): fill then holds the lease.
func (f resendFixture) collecting(t *testing.T, fill *model.FoundryHealFill) *model.FoundryHealFill {
	t.Helper()
	claimed, err := f.db.ClaimFoundryFillCollect(context.Background(), fill, time.Hour)
	require.NoError(t, err)
	require.True(t, claimed)
	return fill
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
	var finding struct {
		Signal struct {
			Spellings []string `json:"spellings"`
		} `json:"signal"`
	}
	require.NoError(t, json.Unmarshal(due[0].Finding, &finding))
	assert.Equal(t, []string{"mentors"}, finding.Signal.Spellings)
	f.collecting(t, fill)
	ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a, f.b}, false)
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
	written := model.FoundryFillDocument{FillID: fill.ID, DocumentID: f.a, Requests: 1, Outcome: "written", Facts: 2, Held: 1, ReviewsTaken: 3, USDMicros: 1500}
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
	require.NoError(t, f.db.RecordFoundryFillDocument(ctx, job.ID, f.worker, model.FoundryFillDocument{FillID: fill.ID, DocumentID: f.b, Requests: 9, Outcome: "nothing"}))
	assert.Equal(t, 1, f.linksOwed(t))
	// A native document's commit counted what it states: the fill adds none.
	require.NoError(t, f.db.RecordFoundryFillDocument(ctx, job.ID, f.worker,
		model.FoundryFillDocument{FillID: fill.ID, DocumentID: f.b, Requests: 1, Outcome: "native", Facts: 5, ReviewsTaken: 5}))
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

// A job that crashed between the engine's write of a document's fill and
// its record of it: the document stays owed, the fill backs off, and the job
// that sends it again hears "landed" (contract 4.35.0) with what the landed
// transaction took. That counts as filled, once, with its reviews, so the
// suggestion reads fixed, not needing a load as a fill that took none would.
func TestFoundryFillLandedAfterACrashCountsAsFilled(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLC")
	fill := f.collecting(t, f.moved(t, "FILLC"))
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, false)
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLC")}))
	factsA, reviewsA := f.extraction(t, f.a)
	// The engine wrote it, then the job died: no word recorded.
	crashed := f.claim(t)
	f.end(t, crashed, "the worker died", model.FoundryGraphJobFailed)
	assert.Equal(t, 1, f.linksOwed(t), "still owed")
	got := f.fill(t, "FILLC")
	require.Equal(t, model.FoundryFillApplying, got.State)
	assert.Zero(t, got.Counts.Done())
	_, err = testDB.Exec(`UPDATE foundry_heal_fills SET paused_until=NOW(6) - INTERVAL 1 SECOND WHERE load_id=?`, f.load)
	require.NoError(t, err)
	due, err := f.db.DueFoundryHealFills(ctx, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	queued, err := f.db.ResumeFoundryFill(ctx, due[0].Fill, f.ontology.WorkspaceID, &model.FoundryGraphJob{ID: id26("RS-FILLC2")})
	require.NoError(t, err)
	require.True(t, queued)
	job := f.claim(t)
	landed := model.FoundryFillDocument{FillID: fill.ID, DocumentID: f.a, Requests: 1, Outcome: "landed", Facts: 2, Held: 1, ReviewsTaken: 3}
	require.NoError(t, f.db.RecordFoundryFillDocument(ctx, job.ID, f.worker, landed))
	require.NoError(t, f.db.RecordFoundryFillDocument(ctx, job.ID, f.worker, landed))
	got = f.fill(t, "FILLC")
	assert.Equal(t, model.FoundryFillFilled, got.State)
	assert.Equal(t, model.FoundryFillCounts{Owed: 1, Filled: 1, ReviewsTaken: 3, FactsAdded: 2}, got.Counts)
	facts, reviews := f.extraction(t, f.a)
	assert.Equal(t, factsA+2, facts)
	assert.Equal(t, max(reviewsA+1-3, 0), reviews)
	assert.IsType(t, model.FoundryFindingFixed{}, model.FoundryFindingStateOf(got, nil))
}

// A fill that owes nothing, its reviews gone, is filled at once.
func TestFoundryFillWithNothingOwedIsFilled(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILL0")
	fill := f.collecting(t, f.moved(t, "FILL0"))
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
	fill := f.collecting(t, f.moved(t, "FILLF"))
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, false)
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
	fill := f.collecting(t, f.moved(t, "FILLS"))
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a, f.b}, false)
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, 2, 0, &model.FoundryGraphJob{ID: id26("RS-FILLS")}))
	job := f.claim(t)
	until := time.Now().Add(24 * time.Hour)
	require.NoError(t, f.db.PauseFoundryFill(ctx, job.ID, f.worker, f.load, fill.ID, until))
	got := f.fill(t, "FILLS")
	require.NotNil(t, got.PausedUntil)
	assert.WithinDuration(t, until, *got.PausedUntil, time.Second)
	assert.Equal(t, 2, f.linksOwed(t), "paused, still owed")
	require.NoError(t, f.db.StopFoundryFill(ctx, job.ID, f.worker, f.load, id26("OTHER-FILL"), model.FoundryFillCapReached))
	assert.Equal(t, model.FoundryFillApplying, f.fill(t, "FILLS").State, "another fill's stop stops none")
	require.NoError(t, f.db.StopFoundryFill(ctx, job.ID, f.worker, f.load, fill.ID, model.FoundryFillCapReached))
	got = f.fill(t, "FILLS")
	assert.Equal(t, model.FoundryFillStopped, got.State)
	assert.Equal(t, model.FoundryFillCapReached, got.StopReason)
	assert.Nil(t, got.PausedUntil)
	assert.Zero(t, f.linksOwed(t))
	assert.Error(t, f.db.StopFoundryFill(ctx, job.ID, f.worker, id26("OTHER"), fill.ID, model.FoundryFillCapReached), "of another load")

	g := newResendFixture(t, "FILLD")
	gFill := g.collecting(t, g.moved(t, "FILLD"))
	_, err = g.db.OweFoundryFillDocuments(ctx, gFill, g.ontology.WorkspaceID, []string{g.a}, false)
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
	fill := f.collecting(t, f.moved(t, "FILLU"))
	gone := id26("FD-FILLU-gone")
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a, gone}, false)
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
	require.NoError(t, f.db.EndFoundryFillDocument(ctx, job.ID, f.worker, fill.ID, gone, byDoc[gone].Requests, byDoc[gone].Unsendable))
	got := f.fill(t, "FILLU")
	assert.Equal(t, int64(1), got.Counts.Refused)
	// A load moved again elsewhere, or no longer current: no document is owed.
	_, err = testDB.Exec(`UPDATE foundry_heal_fills SET state='moved', active_key=NULL WHERE id=?`, fill.ID)
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_ontologies SET current_load_id=NULL WHERE id=?`, f.ontology.ID)
	require.NoError(t, err)
	ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.b}, false)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.ErrorIs(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLU2")}), store.ErrConflict)
}

// finding stores the heal worker's finding of tag for the fixture's load,
// open, naming mentors.
func (f resendFixture) finding(t *testing.T, tag string) {
	t.Helper()
	at, _ := f.loadVersion(t)
	_, err := testDB.Exec(`INSERT INTO foundry_heal_findings (ontology_id, fingerprint, kind, status, base_version_id, load_id, finding, support, sources)
VALUES (?, ?, 'unmatched', 'open', ?, ?, '{"signal":{"kind":"unmatched","canonical":"mentors","spellings":["mentors"],"set":"set_1"}}', 3, 3)`,
		f.ontology.ID, fingerprint(tag), at, f.load)
	require.NoError(t, err)
}

// dueLoads is the loads DueFoundryHealFills lists now, with whether each
// is to resume.
func (f resendFixture) dueLoads(t *testing.T) map[string]bool {
	t.Helper()
	due, err := f.db.DueFoundryHealFills(context.Background(), 100)
	require.NoError(t, err)
	out := map[string]bool{}
	for _, d := range due {
		out[d.Fill.LoadID] = d.Resume
	}
	return out
}

// A finding dismissed while its move is under way: the move may finish, but
// its documents are never collected.
func TestFoundryFillDismissedWhileMovingNeverCollects(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLDM")
	at, _ := f.loadVersion(t)
	f.moveRow(t, "FILLDM", at, at, model.FoundryFillMoving)
	f.finding(t, "FILLDM")
	require.NoError(t, f.db.DismissFoundryHealFinding(ctx, f.ontology.ID, fingerprint("FILLDM"), id26("user")))
	assert.Equal(t, model.FoundryFillMoving, f.fill(t, "FILLDM").State, "the move, at no cost, goes on")
	_, err := testDB.Exec(`UPDATE foundry_heal_fills SET state='moved', links_added='["mentors"]' WHERE id=?`, id26("HF-FILLDM"))
	require.NoError(t, err)
	_, listed := f.dueLoads(t)[f.load]
	assert.False(t, listed, "a dismissed finding's fill is never collected")
	// Opened again (an audit found it with more support): collected.
	_, err = testDB.Exec(`UPDATE foundry_heal_findings SET status='open' WHERE fingerprint=?`, fingerprint("FILLDM"))
	require.NoError(t, err)
	resume, listed := f.dueLoads(t)[f.load]
	assert.True(t, listed)
	assert.False(t, resume)
}

// A fill moved to a version the load moved on from, by a later move in
// place of another suggestion, is still collected: each move only adds.
func TestFoundryFillCollectedAfterALaterMove(t *testing.T) {
	f := newResendFixture(t, "FILLCH")
	f.moved(t, "FILLCH")
	later := f.draftVersion(t, "FILLCH-2", customerTOML)
	_, err := testDB.Exec(`UPDATE foundry_loads SET version_id=? WHERE id=?`, later.ID, f.load)
	require.NoError(t, err)
	_, listed := f.dueLoads(t)[f.load]
	assert.False(t, listed, "a version no move recorded: not collected")
	at, _ := f.loadVersion(t)
	f.moveRow(t, "FILLCH-B", at, later.ID, model.FoundryFillFilled)
	_, err = testDB.Exec(`UPDATE foundry_heal_fills SET from_version_id=(SELECT to_version_id FROM (SELECT to_version_id FROM foundry_heal_fills WHERE id=?) x),
  links_added='["advises"]' WHERE id=?`, id26("HF-FILLCH"), id26("HF-FILLCH-B"))
	require.NoError(t, err)
	due, err := f.db.DueFoundryHealFills(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, fingerprint("FILLCH"), due[0].Fill.Fingerprint)
}

// A collect that stopped partway leaves rows owed; the next collect's first
// page ends them, so a fill sends only what its own collect listed.
func TestFoundryFillFreshCollectEndsWhatAnEarlierLeft(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLST")
	fill := f.collecting(t, f.moved(t, "FILLST"))
	ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a, f.b}, false)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, f.linksOwed(t))
	ok, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, nil, true)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Zero(t, f.linksOwed(t))
	_, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, false)
	require.NoError(t, err)
	assert.Equal(t, 1, f.linksOwed(t))
}

// A collect that keeps failing backs off, then needs a load, never
// running at every tick.
func TestFoundryFillCollectFailsThenNeedsALoad(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLCF")
	fill := f.moved(t, "FILLCF")
	for i := range len(model.FoundryFillBackoff) {
		f.collecting(t, fill)
		require.NoError(t, f.db.FailFoundryFillCollect(ctx, fill, "agent down"))
		got := f.fill(t, "FILLCF")
		require.Equal(t, model.FoundryFillMoved, got.State)
		require.NotNil(t, got.PausedUntil)
		assert.WithinDuration(t, time.Now().Add(model.FoundryFillBackoff[i]), *got.PausedUntil, time.Minute)
		_, listed := f.dueLoads(t)[f.load]
		assert.False(t, listed, "backing off")
		_, err := testDB.Exec(`UPDATE foundry_heal_fills SET paused_until=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, fill.ID)
		require.NoError(t, err)
		_, listed = f.dueLoads(t)[f.load]
		assert.True(t, listed, "due once its backoff ends")
	}
	f.collecting(t, fill)
	require.NoError(t, f.db.FailFoundryFillCollect(ctx, fill, "agent down"))
	got := f.fill(t, "FILLCF")
	assert.Equal(t, model.FoundryFillNeedsLoad, got.State)
	assert.Equal(t, []model.FoundryFoldWhy{{Kind: model.FoundryWhyCollectFailed, Item: "agent down"}}, got.Why)
	_, listed := f.dueLoads(t)[f.load]
	assert.False(t, listed)
}

// What a job spends after its fill stopped is still the fill's; a document
// whose model failed spends without ending or counting.
func TestFoundryFillSpendAfterAStopAndOnAFailure(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLSP")
	fill := f.collecting(t, f.moved(t, "FILLSP"))
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a, f.b}, false)
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, 2, 0, &model.FoundryGraphJob{ID: id26("RS-FILLSP")}))
	job := f.claim(t)
	failed := model.FoundryFillDocument{FillID: fill.ID, DocumentID: f.a, Requests: 1, USDMicros: 2000}
	require.NoError(t, f.db.SpendFoundryFillDocument(ctx, job.ID, f.worker, failed))
	got := f.fill(t, "FILLSP")
	assert.InDelta(t, 0.002, got.SpentUSD, 1e-9)
	assert.Zero(t, got.Counts.Done())
	assert.Equal(t, 2, f.linksOwed(t), "still owed")
	require.NoError(t, f.db.StopFoundryFill(ctx, job.ID, f.worker, f.load, fill.ID, model.FoundryFillCapReached))
	require.NoError(t, f.db.SpendFoundryFillDocument(ctx, job.ID, f.worker, failed))
	got = f.fill(t, "FILLSP")
	assert.Equal(t, model.FoundryFillStopped, got.State)
	assert.InDelta(t, 0.004, got.SpentUSD, 1e-9, "spent after the stop is the fill's")
	var jobSpent int64
	require.NoError(t, testDB.QueryRow(`SELECT usd_micros FROM foundry_load_jobs WHERE id=?`, job.ID).Scan(&jobSpent))
	assert.Equal(t, int64(4000), jobSpent)
	assert.ErrorIs(t, f.db.SpendFoundryFillDocument(ctx, id26("NOJOB"), f.worker, failed), store.ErrNotFound)
	// A job queued long before the day's window, still spending in it, counts.
	_, err = testDB.Exec(`UPDATE foundry_load_jobs SET created_at=NOW(6) - INTERVAL 2 DAY, finished_at=NULL WHERE id=?`, job.ID)
	require.NoError(t, err)
	spent, err := f.db.SumFoundryFillSpend(ctx, f.ontology.ID, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, int64(4000), spent)
	_, err = testDB.Exec(`UPDATE foundry_load_jobs SET finished_at=NOW(6) - INTERVAL 30 HOUR WHERE id=?`, job.ID)
	require.NoError(t, err)
	spent, err = f.db.SumFoundryFillSpend(ctx, f.ontology.ID, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Zero(t, spent, "finished before the window")
}

// A collect is claimed: while it runs, no other worker claims it, or
// another fill of its load, and neither is due; a write under a lease it no
// longer holds (one that ran out, and another worker claimed) writes
// nothing; a fill starts only owing exactly what its collect owed.
func TestFoundryFillCollectLeased(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLL")
	fill := f.moved(t, "FILLL")
	other := f.moved(t, "FILLL2")
	stale := *fill
	f.collecting(t, fill)
	claimed, err := f.db.ClaimFoundryFillCollect(ctx, &stale, time.Hour)
	require.NoError(t, err)
	assert.False(t, claimed, "claimed already")
	claimed, err = f.db.ClaimFoundryFillCollect(ctx, other, time.Hour)
	require.NoError(t, err)
	assert.False(t, claimed, "a load collects one fill at a time")
	_, listed := f.dueLoads(t)[f.load]
	assert.False(t, listed, "nothing of a load collected is due")
	ok, err := f.db.OweFoundryFillDocuments(ctx, &stale, f.ontology.WorkspaceID, []string{f.a}, false)
	require.NoError(t, err)
	assert.False(t, ok, "no lease, nothing owed")
	assert.Zero(t, f.linksOwed(t))
	ok, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a, f.b}, false)
	require.NoError(t, err)
	require.True(t, ok)
	// Its lease runs out, and another worker claims it: the first writes
	// nothing more, and starts nothing.
	_, err = testDB.Exec(`UPDATE foundry_heal_fills SET paused_until=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, fill.ID)
	require.NoError(t, err)
	f.collecting(t, &stale)
	ok, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, false)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.ErrorIs(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, 2, 0, &model.FoundryGraphJob{ID: id26("RS-FILLL0")}), store.ErrConflict)
	require.NoError(t, f.db.FailFoundryFillCollect(ctx, fill, "late"))
	assert.Zero(t, f.fill(t, "FILLL").Failures, "a lease lost counts no failure")
	// The new holder's fresh collect ends what the first owed; owing one, it
	// can't start owing two, nor two after owing one.
	ok, err = f.db.OweFoundryFillDocuments(ctx, &stale, f.ontology.WorkspaceID, nil, true)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Zero(t, f.linksOwed(t))
	ok, err = f.db.OweFoundryFillDocuments(ctx, &stale, f.ontology.WorkspaceID, []string{f.b}, false)
	require.NoError(t, err)
	require.True(t, ok)
	assert.ErrorIs(t, f.db.StartFoundryFill(ctx, &stale, f.ontology.WorkspaceID, 2, 0, &model.FoundryGraphJob{ID: id26("RS-FILLL1")}), store.ErrConflict)
	require.NoError(t, f.db.StartFoundryFill(ctx, &stale, f.ontology.WorkspaceID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLL2")}))
	got := f.fill(t, "FILLL")
	assert.Equal(t, model.FoundryFillApplying, got.State)
	assert.Nil(t, got.PausedUntil, "a fill started holds no lease")
}

// A collect given up on ends what it owed, and the link its move added
// stays the load's: its Extracts still ask it apart, and an earlier move's
// fill, whose version the given-up move moved on from, is still due.
func TestFoundryFillCollectGivenUpKeepsWhatItsMoveAdded(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLKA")
	first := f.moved(t, "FILLKA")
	at, _ := f.loadVersion(t)
	later := f.draftVersion(t, "FILLKA-2", customerTOML)
	f.moveRow(t, "FILLKB", at, later.ID, model.FoundryFillMoved)
	f.finding(t, "FILLKB")
	_, err := testDB.Exec(`UPDATE foundry_heal_fills SET links_added='["advises"]', failures=?, paused_until=NOW(6) - INTERVAL 1 SECOND WHERE id=?`,
		len(model.FoundryFillBackoff), id26("HF-FILLKB"))
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_loads SET version_id=? WHERE id=?`, later.ID, f.load)
	require.NoError(t, err)
	// The later fill's collect gives up, owing what it listed so far.
	fills, err := f.db.ListFoundryHealFills(ctx, f.load)
	require.NoError(t, err)
	given := f.collecting(t, fills[fingerprint("FILLKB")])
	ok, err := f.db.OweFoundryFillDocuments(ctx, given, f.ontology.WorkspaceID, []string{f.a}, false)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, f.db.FailFoundryFillCollect(ctx, given, "agent down"))
	got := f.fill(t, "FILLKB")
	require.Equal(t, model.FoundryFillNeedsLoad, got.State)
	assert.Equal(t, []string{"advises"}, got.LinksAdded)
	assert.Zero(t, f.linksOwed(t), "what it owed ends with it")
	asked, err := f.db.FoundryAskedApart(ctx, f.load)
	require.NoError(t, err)
	assert.Equal(t, []string{"advises", "mentors"}, asked, "the load still asks the link it gained apart")
	due, err := f.db.DueFoundryHealFills(ctx, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, first.ID, due[0].Fill.ID, "the earlier move's fill, along the chain")
}

// A fill under way stops once another load becomes current, ending what it
// owes: made current again, the load doesn't resume it.
func TestFoundryFillStopsWhenAnotherLoadBecomesCurrent(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLLR")
	fill := f.collecting(t, f.moved(t, "FILLLR"))
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, false)
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLLR")}))
	// Another ready load of the ontology, made current.
	second := id26("FJ-FILLLR-2")
	var cols string
	require.NoError(t, testDB.QueryRow(`SELECT GROUP_CONCAT(COLUMN_NAME ORDER BY ORDINAL_POSITION) FROM INFORMATION_SCHEMA.COLUMNS
WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='foundry_loads' AND COLUMN_NAME<>'id'`).Scan(&cols))
	_, err = testDB.Exec(`INSERT INTO foundry_loads (id, `+cols+`) SELECT ?, `+cols+` FROM foundry_loads WHERE id=?`, second, f.load)
	require.NoError(t, err)
	_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, second, &model.FoundryGraphJob{ID: id26("FE-FILLLR")},
		&model.FoundryGraphJob{ID: id26("FR-FILLLR")}, noBudget)
	require.NoError(t, err)
	got := f.fill(t, "FILLLR")
	assert.Equal(t, model.FoundryFillStopped, got.State)
	assert.Equal(t, model.FoundryFillLoadReplaced, got.StopReason)
	assert.Zero(t, f.linksOwed(t))
	_, listed := f.dueLoads(t)[f.load]
	assert.False(t, listed)
}

// A fill dismissed while a job sent its documents, another fill of the load
// applying since: what the job's documents spent and said is the first
// fill's, never the second's.
func TestFoundryFillSpendAndCountsKeyedToTheirFill(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLK")
	first := f.collecting(t, f.moved(t, "FILLK"))
	_, err := f.db.OweFoundryFillDocuments(ctx, first, f.ontology.WorkspaceID, []string{f.a}, false)
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, first, f.ontology.WorkspaceID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLK")}))
	job := f.claim(t)
	require.NoError(t, f.db.DismissFoundryHealFinding(ctx, f.ontology.ID, fingerprint("FILLK"), id26("user")))
	second := f.collecting(t, f.moved(t, "FILLK2"))
	_, err = f.db.OweFoundryFillDocuments(ctx, second, f.ontology.WorkspaceID, []string{f.a}, false)
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, second, f.ontology.WorkspaceID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLK2")}))
	late := model.FoundryFillDocument{FillID: first.ID, DocumentID: f.a, Requests: 1, Outcome: "written", Facts: 1, ReviewsTaken: 1, USDMicros: 3000}
	require.NoError(t, f.db.RecordFoundryFillDocument(ctx, job.ID, f.worker, late))
	gotFirst, gotSecond := f.fill(t, "FILLK"), f.fill(t, "FILLK2")
	assert.InDelta(t, 0.003, gotFirst.SpentUSD, 1e-9)
	assert.Zero(t, gotSecond.SpentUSD)
	assert.Zero(t, gotSecond.Counts.Done(), "the second's request is a newer one")
	assert.Equal(t, 1, f.linksOwed(t))
}

// A finding no longer open (superseded by a later audit, or dismissed)
// names no link to fill: a job reading its fill stops it.
func TestFoundryFillOfAFindingNoLongerOpen(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLNO")
	fill := f.collecting(t, f.moved(t, "FILLNO"))
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, false)
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLNO")}))
	got, err := f.db.FoundryFillApplying(ctx, f.load)
	require.NoError(t, err)
	assert.Equal(t, "mentors", got.Link)
	_, err = testDB.Exec(`UPDATE foundry_heal_findings SET status='superseded' WHERE fingerprint=?`, fingerprint("FILLNO"))
	require.NoError(t, err)
	got, err = f.db.FoundryFillApplying(ctx, f.load)
	require.NoError(t, err)
	assert.Empty(t, got.Link)
	assert.Empty(t, got.Spellings)
}

// A collect released (its worker stopping) is due again at once, counting
// no failure; a release without the lease releases nothing.
func TestFoundryFillCollectReleased(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLRL")
	fill := f.collecting(t, f.moved(t, "FILLRL"))
	stale, other := *fill, time.Now().UTC()
	stale.PausedUntil = &other
	require.NoError(t, f.db.ReleaseFoundryFillCollect(ctx, &stale))
	_, listed := f.dueLoads(t)[f.load]
	assert.False(t, listed, "still held")
	require.NoError(t, f.db.ReleaseFoundryFillCollect(ctx, fill))
	_, listed = f.dueLoads(t)[f.load]
	assert.True(t, listed)
	assert.Zero(t, f.fill(t, "FILLRL").Failures)
}
