package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
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

// pageAt is the cursor a collect of fill saves once it owes docs, at least
// one, a page past what it saved: the page's last document, the version the
// fill moved to, and what it owed so far.
func pageAt(fill *model.FoundryHealFill, docs []string) model.FoundryFillCursor {
	at := model.FoundryFillCursor{VersionID: fill.ToVersionID}
	if fill.Collect != nil {
		at = *fill.Collect
	}
	at.After = slices.Max(docs)
	at.Owed += int64(len(docs))
	return at
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
	ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a, f.b}, pageAt(fill, []string{f.a, f.b}))
	require.NoError(t, err)
	assert.True(t, ok)
	// Owed, but not yet counted: the fill doesn't apply yet.
	assert.Equal(t, 2, f.linksOwed(t))
	listed, err := f.db.ListFoundryLinkResends(ctx, f.load, 10)
	require.NoError(t, err)
	assert.Empty(t, listed)
	assert.Nil(t, f.liveJob(t))
	assert.Empty(t, f.owed(t), "a fill's documents are no approval's")
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 2, 1, &model.FoundryGraphJob{ID: id26("RS-FILL")}))
	got := f.fill(t, "FILL")
	assert.Equal(t, model.FoundryFillApplying, got.State)
	assert.Equal(t, model.FoundryFillCounts{Owed: 2, Left: 1}, got.Counts)
	assert.Equal(t, model.FoundryFillCapUSD, got.CapUSD)
	assert.ErrorIs(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 2, 1, &model.FoundryGraphJob{ID: id26("RS-FILL2")}), store.ErrConflict)
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
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, pageAt(fill, []string{f.a}))
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLC")}))
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

// A second finding of the same link (joining the first's move) sends a
// document the first filled again, and the engine finds the first's
// transaction landed: counted filled, but none of what that record counted
// is counted twice, on the fill or the document. A landed document of
// another link than any other fill of the load filled counts as ever.
func TestFoundryFillLandedOfAnotherFillOfItsLinkCountsNothingTwice(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLJ")
	first := f.collecting(t, f.moved(t, "FILLJ"))
	_, err := f.db.OweFoundryFillDocuments(ctx, first, f.ontology.WorkspaceID, []string{f.a}, pageAt(first, []string{f.a}))
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, first, f.ontology.WorkspaceID, first.ToVersionID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLJ")}))
	job := f.claim(t)
	written := model.FoundryFillDocument{FillID: first.ID, DocumentID: f.a, Requests: 1, Outcome: "written", Facts: 2, Held: 1, ReviewsTaken: 3}
	require.NoError(t, f.db.RecordFoundryFillDocument(ctx, job.ID, f.worker, written))
	require.Equal(t, model.FoundryFillFilled, f.fill(t, "FILLJ").State)
	f.end(t, job, "first", model.FoundryGraphJobSucceeded)
	factsA, reviewsA := f.extraction(t, f.a)
	factsB, reviewsB := f.extraction(t, f.b)
	second := f.collecting(t, f.moved(t, "FILLJ2"))
	_, err = f.db.OweFoundryFillDocuments(ctx, second, f.ontology.WorkspaceID, []string{f.a, f.b}, pageAt(second, []string{f.a, f.b}))
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, second, f.ontology.WorkspaceID, second.ToVersionID, 2, 0, &model.FoundryGraphJob{ID: id26("RS-FILLJ2")}))
	job = f.claim(t)
	landed := model.FoundryFillDocument{FillID: second.ID, DocumentID: f.a, Requests: 2, Outcome: "landed", Facts: 2, Held: 1, ReviewsTaken: 3}
	require.NoError(t, f.db.RecordFoundryFillDocument(ctx, job.ID, f.worker, landed))
	got := f.fill(t, "FILLJ2")
	assert.Equal(t, model.FoundryFillCounts{Owed: 2, Filled: 1}, got.Counts)
	facts, reviews := f.extraction(t, f.a)
	assert.Equal(t, [2]int{factsA, reviewsA}, [2]int{facts, reviews}, "counted by the first's record")
	// The first's finding of another link: the landed transaction can only
	// be this fill's own, its record lost.
	_, err = testDB.Exec(`UPDATE foundry_heal_findings SET finding=JSON_SET(finding, '$.signal.canonical', 'advises') WHERE fingerprint=?`,
		fingerprint("FILLJ"))
	require.NoError(t, err)
	landed.DocumentID, landed.Requests = f.b, 1
	require.NoError(t, f.db.RecordFoundryFillDocument(ctx, job.ID, f.worker, landed))
	got = f.fill(t, "FILLJ2")
	assert.Equal(t, model.FoundryFillFilled, got.State)
	assert.Equal(t, model.FoundryFillCounts{Owed: 2, Filled: 2, ReviewsTaken: 3, FactsAdded: 2}, got.Counts)
	facts, reviews = f.extraction(t, f.b)
	assert.Equal(t, [2]int{factsB + 2, max(reviewsB+1-3, 0)}, [2]int{facts, reviews})
}

// While another moved fill of the load holds a cursor at the version the
// load records, no other fill's collect is claimed (it would start the
// collect over, ending what the cursor's pages owed): only the cursor's.
// A cursor of a version the load moved on from holds nothing back.
func TestFoundryFillCollectWaitsForAnotherFillsCursor(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLWC")
	paged := f.collecting(t, f.moved(t, "FILLWC"))
	other := f.moved(t, "FILLWC2")
	ok, err := f.db.OweFoundryFillDocuments(ctx, paged, f.ontology.WorkspaceID, []string{f.a}, pageAt(paged, []string{f.a}))
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, f.db.ReleaseFoundryFillCollect(ctx, paged))
	claimed, err := f.db.ClaimFoundryFillCollect(ctx, other, time.Hour)
	require.NoError(t, err)
	assert.False(t, claimed, "another's cursor waits")
	assert.Nil(t, f.fill(t, "FILLWC2").PausedUntil)
	assert.Equal(t, 1, f.linksOwed(t))
	_, err = testDB.Exec(`UPDATE foundry_heal_fills SET collect_version_id=? WHERE id=?`, id26("V-OLD"), paged.ID)
	require.NoError(t, err)
	claimed, err = f.db.ClaimFoundryFillCollect(ctx, other, time.Hour)
	require.NoError(t, err)
	assert.True(t, claimed, "a cursor of an older version is started over")
}

// A fill that owes nothing, its reviews gone, is filled at once.
func TestFoundryFillWithNothingOwedIsFilled(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILL0")
	fill := f.collecting(t, f.moved(t, "FILL0"))
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 0, 0, &model.FoundryGraphJob{ID: id26("RS-FILL0")}))
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
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, pageAt(fill, []string{f.a}))
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLF")}))
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
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a, f.b}, pageAt(fill, []string{f.a, f.b}))
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 2, 0, &model.FoundryGraphJob{ID: id26("RS-FILLS")}))
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
	_, err = g.db.OweFoundryFillDocuments(ctx, gFill, g.ontology.WorkspaceID, []string{g.a}, pageAt(gFill, []string{g.a}))
	require.NoError(t, err)
	require.NoError(t, g.db.StartFoundryFill(ctx, gFill, g.ontology.WorkspaceID, gFill.ToVersionID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLD")}))
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
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a, gone}, pageAt(fill, []string{f.a, gone}))
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 2, 0, &model.FoundryGraphJob{ID: id26("RS-FILLU")}))
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
	ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.b}, pageAt(fill, []string{f.b}))
	require.NoError(t, err)
	assert.False(t, ok)
	assert.ErrorIs(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLU2")}), store.ErrConflict)
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
	ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a, f.b}, pageAt(fill, []string{f.a, f.b}))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 2, f.linksOwed(t))
	ok, err = f.db.RestartFoundryFillCollect(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Zero(t, f.linksOwed(t))
	_, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, pageAt(fill, []string{f.a}))
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
		require.NoError(t, f.db.FailFoundryFillCollect(ctx, fill, "agent down", false))
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
	require.NoError(t, f.db.FailFoundryFillCollect(ctx, fill, "agent down", false))
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
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a, f.b}, pageAt(fill, []string{f.a, f.b}))
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 2, 0, &model.FoundryGraphJob{ID: id26("RS-FILLSP")}))
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
	ok, err := f.db.OweFoundryFillDocuments(ctx, &stale, f.ontology.WorkspaceID, []string{f.a}, pageAt(&stale, []string{f.a}))
	require.NoError(t, err)
	assert.False(t, ok, "no lease, nothing owed")
	assert.Zero(t, f.linksOwed(t))
	ok, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a, f.b}, pageAt(fill, []string{f.a, f.b}))
	require.NoError(t, err)
	require.True(t, ok)
	// Its lease runs out, and another worker claims it: the first writes
	// nothing more, and starts nothing.
	_, err = testDB.Exec(`UPDATE foundry_heal_fills SET paused_until=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, fill.ID)
	require.NoError(t, err)
	f.collecting(t, &stale)
	ok, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, pageAt(fill, []string{f.a}))
	require.NoError(t, err)
	assert.False(t, ok)
	assert.ErrorIs(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 2, 0, &model.FoundryGraphJob{ID: id26("RS-FILLL0")}), store.ErrConflict)
	require.NoError(t, f.db.FailFoundryFillCollect(ctx, fill, "late", false))
	assert.Zero(t, f.fill(t, "FILLL").Failures, "a lease lost counts no failure")
	// The new holder's fresh collect ends what the first owed; owing one, it
	// can't start owing two, nor two after owing one.
	ok, err = f.db.RestartFoundryFillCollect(ctx, &stale, f.ontology.WorkspaceID, stale.ToVersionID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Zero(t, f.linksOwed(t))
	ok, err = f.db.OweFoundryFillDocuments(ctx, &stale, f.ontology.WorkspaceID, []string{f.b}, pageAt(&stale, []string{f.b}))
	require.NoError(t, err)
	require.True(t, ok)
	assert.ErrorIs(t, f.db.StartFoundryFill(ctx, &stale, f.ontology.WorkspaceID, stale.ToVersionID, 2, 0, &model.FoundryGraphJob{ID: id26("RS-FILLL1")}), store.ErrConflict)
	require.NoError(t, f.db.StartFoundryFill(ctx, &stale, f.ontology.WorkspaceID, stale.ToVersionID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLL2")}))
	got := f.fill(t, "FILLL")
	assert.Equal(t, model.FoundryFillApplying, got.State)
	assert.Nil(t, got.PausedUntil, "a fill started holds no lease")
}

// A collect given up on ends what it owed and its cursor, and the link its
// move added stays the load's: its Extracts still ask it apart, and an earlier move's
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
	ok, err := f.db.OweFoundryFillDocuments(ctx, given, f.ontology.WorkspaceID, []string{f.a}, pageAt(given, []string{f.a}))
	require.NoError(t, err)
	require.True(t, ok)
	// A page saved starts its failures from none: this one failed as often
	// since.
	_, err = testDB.Exec(`UPDATE foundry_heal_fills SET failures=? WHERE id=?`, len(model.FoundryFillBackoff), id26("HF-FILLKB"))
	require.NoError(t, err)
	require.NoError(t, f.db.FailFoundryFillCollect(ctx, given, "agent down", false))
	got := f.fill(t, "FILLKB")
	require.Equal(t, model.FoundryFillNeedsLoad, got.State)
	assert.Equal(t, []string{"advises"}, got.LinksAdded)
	assert.Zero(t, f.linksOwed(t), "what it owed ends with it")
	assert.Nil(t, got.Collect, "and its cursor")
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
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, pageAt(fill, []string{f.a}))
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLLR")}))
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
	_, err := f.db.OweFoundryFillDocuments(ctx, first, f.ontology.WorkspaceID, []string{f.a}, pageAt(first, []string{f.a}))
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, first, f.ontology.WorkspaceID, first.ToVersionID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLK")}))
	job := f.claim(t)
	require.NoError(t, f.db.DismissFoundryHealFinding(ctx, f.ontology.ID, fingerprint("FILLK"), id26("user")))
	second := f.collecting(t, f.moved(t, "FILLK2"))
	_, err = f.db.OweFoundryFillDocuments(ctx, second, f.ontology.WorkspaceID, []string{f.a}, pageAt(second, []string{f.a}))
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, second, f.ontology.WorkspaceID, second.ToVersionID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLK2")}))
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
	_, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, pageAt(fill, []string{f.a}))
	require.NoError(t, err)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLNO")}))
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

// collectAt reads the cursor fill tag's row holds, nil for none.
func (f resendFixture) collectAt(t *testing.T, tag string) *model.FoundryFillCursor {
	t.Helper()
	return f.fill(t, tag).Collect
}

// Each page a collect owes saves how far it got, with that page's documents,
// in one transaction, and starts its failures again from none; the due list
// reads it back; a cursor that counts no less than nothing and names its
// page is all it takes; and the fill's start clears it.
func TestFoundryFillCollectCursorSavedWithItsPage(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLCU")
	fill := f.moved(t, "FILLCU")
	_, err := testDB.Exec(`UPDATE foundry_heal_fills SET failures=2, last_error='agent down' WHERE id=?`, fill.ID)
	require.NoError(t, err)
	f.collecting(t, fill)
	_, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, model.FoundryFillCursor{VersionID: fill.ToVersionID})
	require.Error(t, err, "a cursor names its page's last document")
	_, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, model.FoundryFillCursor{After: f.a, VersionID: fill.ToVersionID, Owed: -1})
	require.Error(t, err)
	assert.Zero(t, f.linksOwed(t))
	first := model.FoundryFillCursor{After: f.a, VersionID: fill.ToVersionID, Owed: 1, Left: 4}
	ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, first)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, &first, f.collectAt(t, "FILLCU"))
	got := f.fill(t, "FILLCU")
	assert.Zero(t, got.Failures, "a page saved is progress")
	// A page that owes nothing still saves how far it got.
	empty := model.FoundryFillCursor{After: f.b, VersionID: fill.ToVersionID, Owed: 1, Left: 4}
	ok, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, nil, empty)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, &empty, f.collectAt(t, "FILLCU"))
	assert.Equal(t, 1, f.linksOwed(t))
	// Read back with the fill the heal worker resumes.
	require.NoError(t, f.db.ReleaseFoundryFillCollect(ctx, fill))
	due, err := f.db.DueFoundryHealFills(ctx, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, &empty, due[0].Fill.Collect)
	f.collecting(t, due[0].Fill)
	require.NoError(t, f.db.StartFoundryFill(ctx, due[0].Fill, f.ontology.WorkspaceID, due[0].Fill.ToVersionID, 1, 4, &model.FoundryGraphJob{ID: id26("RS-FILLCU")}))
	got = f.fill(t, "FILLCU")
	assert.Equal(t, model.FoundryFillApplying, got.State)
	assert.Equal(t, model.FoundryFillCounts{Owed: 1, Left: 4}, got.Counts)
	assert.Nil(t, got.Collect, "cleared on success")
}

// A collect that fails keeps what its pages owed and its cursor, backing
// off; one whose start the load refuses (it owes other than the cursor
// counts) starts over, as does one given up on: what the load owed ends
// and the cursor goes. A collect that starts over clears every cursor of
// its load, whose rows it ends.
func TestFoundryFillCollectFailureKeepsItsCursor(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLKC")
	fill := f.collecting(t, f.moved(t, "FILLKC"))
	ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, pageAt(fill, []string{f.a}))
	require.NoError(t, err)
	require.True(t, ok)
	saved := f.collectAt(t, "FILLKC")
	require.NotNil(t, saved)
	require.NoError(t, f.db.FailFoundryFillCollect(ctx, fill, "agent down", false))
	got := f.fill(t, "FILLKC")
	assert.Equal(t, 1, got.Failures)
	assert.Equal(t, saved, got.Collect, "kept for the next attempt")
	assert.Equal(t, 1, f.linksOwed(t), "what its page owed stays owed")
	// Another document owed behind its back (a row the cursor doesn't
	// count): the start refuses, and the collect starts over.
	_, err = testDB.Exec(`UPDATE foundry_heal_fills SET paused_until=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, fill.ID)
	require.NoError(t, err)
	f.collecting(t, fill)
	f.owe(t, f.b, string(model.FoundryResendLink))
	assert.ErrorIs(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, saved.Owed, saved.Left, &model.FoundryGraphJob{ID: id26("RS-FILLKC")}),
		store.ErrConflict)
	require.NoError(t, f.db.FailFoundryFillCollect(ctx, fill, "the load owes 2", true))
	got = f.fill(t, "FILLKC")
	assert.Equal(t, 2, got.Failures)
	assert.Nil(t, got.Collect)
	assert.Zero(t, f.linksOwed(t))
	// A fresh collect of the load clears another fill's cursor with the rows:
	// one of a version the load moved on from (one at the load's version
	// keeps every other fill's collect from being claimed).
	_, err = testDB.Exec(`UPDATE foundry_heal_fills SET collect_after=?, collect_version_id=?, collect_owed=1, paused_until=NULL WHERE id=?`,
		f.a, id26("V-OLD"), fill.ID)
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_load_resends SET status='owed', ended_at=NULL WHERE load_id=? AND document_id=? AND reason='link'`, f.load, f.a)
	require.NoError(t, err)
	other := f.collecting(t, f.moved(t, "FILLKC2"))
	ok, err = f.db.RestartFoundryFillCollect(ctx, other, f.ontology.WorkspaceID, other.ToVersionID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Nil(t, f.collectAt(t, "FILLKC"))
	assert.Zero(t, f.linksOwed(t))
}

// Of a load's moved fills, the one whose collect saved a cursor is due
// first, whatever its support, so no other's fresh collect ends what it
// owed.
func TestFoundryFillWithACursorCollectsFirst(t *testing.T) {
	f := newResendFixture(t, "FILLFI")
	low := f.moved(t, "FILLFI")
	f.moved(t, "FILLFI2")
	_, err := testDB.Exec(`UPDATE foundry_heal_findings SET support=9 WHERE fingerprint=?`, fingerprint("FILLFI"))
	require.NoError(t, err)
	due, err := f.db.DueFoundryHealFills(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, fingerprint("FILLFI2"), due[0].Fill.Fingerprint, "the least support first")
	_, err = testDB.Exec(`UPDATE foundry_heal_fills SET collect_after=?, collect_version_id=?, collect_owed=1 WHERE id=?`, f.a, low.ToVersionID, low.ID)
	require.NoError(t, err)
	due, err = f.db.DueFoundryHealFills(context.Background(), 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, low.ID, due[0].Fill.ID, "the one with a cursor")
}

// A finding that closes takes its fill's cursor with it: dismissed, the
// fill stops with none; superseded by a later audit, the cursor goes, and a
// collect still running saves nothing more; opened again, it starts over.
func TestFoundryFillCursorOfAFindingThatClosed(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLCL")
	fill := f.collecting(t, f.moved(t, "FILLCL"))
	ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, pageAt(fill, []string{f.a}))
	require.NoError(t, err)
	require.True(t, ok)
	at, _ := f.loadVersion(t)
	f.healRun(t, "FILLCL", f.load, at, false, unmatchedSeen("other", 3, 3))
	assert.Equal(t, model.FoundryHealSuperseded, f.healRows(t)[fingerprint("FILLCL")].Status)
	assert.Nil(t, f.collectAt(t, "FILLCL"), "superseded")
	ok, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.b}, pageAt(fill, []string{f.b}))
	require.NoError(t, err)
	assert.False(t, ok, "a collect still running saves nothing for a closed finding")
	assert.Nil(t, f.collectAt(t, "FILLCL"))
	assert.ErrorIs(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLCL")}), store.ErrConflict)
	_, listed := f.dueLoads(t)[f.load]
	assert.False(t, listed)
	// Dismissed with a cursor: stopped, with none.
	g := newResendFixture(t, "FILLCD")
	gFill := g.collecting(t, g.moved(t, "FILLCD"))
	ok, err = g.db.OweFoundryFillDocuments(ctx, gFill, g.ontology.WorkspaceID, []string{g.a}, pageAt(gFill, []string{g.a}))
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, g.db.DismissFoundryHealFinding(ctx, g.ontology.ID, fingerprint("FILLCD"), id26("user")))
	got := g.fill(t, "FILLCD")
	assert.Equal(t, model.FoundryFillStopped, got.State)
	assert.Nil(t, got.Collect)
}

// Another load made current ends what a collect of the one it replaces owed,
// and its cursor: made current again, the collect starts over.
func TestFoundryFillCursorEndsWhenAnotherLoadBecomesCurrent(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLCR")
	fill := f.collecting(t, f.moved(t, "FILLCR"))
	ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, pageAt(fill, []string{f.a}))
	require.NoError(t, err)
	require.True(t, ok)
	second := id26("FJ-FILLCR-2")
	var cols string
	require.NoError(t, testDB.QueryRow(`SELECT GROUP_CONCAT(COLUMN_NAME ORDER BY ORDINAL_POSITION) FROM INFORMATION_SCHEMA.COLUMNS
WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='foundry_loads' AND COLUMN_NAME<>'id'`).Scan(&cols))
	_, err = testDB.Exec(`INSERT INTO foundry_loads (id, `+cols+`) SELECT ?, `+cols+` FROM foundry_loads WHERE id=?`, second, f.load)
	require.NoError(t, err)
	_, err = f.db.SetCurrentFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, second, &model.FoundryGraphJob{ID: id26("FE-FILLCR")},
		&model.FoundryGraphJob{ID: id26("FR-FILLCR")}, noBudget)
	require.NoError(t, err)
	got := f.fill(t, "FILLCR")
	assert.Equal(t, model.FoundryFillMoved, got.State)
	assert.Nil(t, got.Collect)
	assert.Zero(t, f.linksOwed(t))
	ok, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.b}, pageAt(fill, []string{f.b}))
	require.NoError(t, err)
	assert.False(t, ok, "not the current load")
}

// waitsOnLock waits until a transaction waits for a row lock of table in
// the test schema, as InnoDB reports it: failing if finished says the
// transaction that should wait ended first, or none waits in 10 s.
func waitsOnLock(t *testing.T, table string, finished func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		require.False(t, finished(), "it ended without waiting for a lock of %s", table)
		var waits int
		require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM performance_schema.data_lock_waits w
JOIN performance_schema.data_locks l ON l.ENGINE_LOCK_ID=w.REQUESTING_ENGINE_LOCK_ID
WHERE l.OBJECT_SCHEMA=DATABASE() AND l.OBJECT_NAME=?`, table).Scan(&waits))
		if waits > 0 {
			return
		}
	}
	t.Fatalf("nothing waited for a lock of %s", table)
}

// A collect's page saved while a later audit supersedes its finding: the
// page's transaction holds the ontology's lock, so the publish waits for
// it, then clears the cursor it saved. The cursor that once outlived its
// finding (the publish took no lock) left the fill never due and every
// other fill of the load refused its claim.
func TestFoundryFillSupersedeWaitsForAPageSaved(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLSW")
	fill := f.collecting(t, f.moved(t, "FILLSW"))
	other := f.moved(t, "FILLSW2")
	at, _ := f.loadVersion(t)
	run := &model.FoundryHealRun{ID: id26("HR-FILLSW"), OntologyID: f.ontology.ID, LoadID: f.load}
	require.NoError(t, f.db.StartFoundryHealRun(ctx, run, time.Minute))
	// A page's transaction, as OweFoundryFillDocuments takes it: the
	// ontology's lock, the finding read open, and nothing written yet.
	page, err := testDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = page.Rollback() }()
	var locked, status string
	require.NoError(t, page.QueryRowContext(ctx, `SELECT id FROM foundry_ontologies WHERE id=? FOR UPDATE`, f.ontology.ID).Scan(&locked))
	require.NoError(t, page.QueryRowContext(ctx, `SELECT status FROM foundry_heal_findings WHERE ontology_id=? AND fingerprint=?`,
		f.ontology.ID, fingerprint("FILLSW")).Scan(&status))
	require.Equal(t, "open", status)
	published := make(chan error, 1)
	go func() {
		published <- f.db.PublishFoundryHealRun(ctx, model.FoundryHealPublish{RunID: run.ID, OntologyID: f.ontology.ID, LoadID: f.load,
			VersionID: at, Pages: 1, Findings: []model.FoundryHealSeen{unmatchedSeen("other", 3, 3)}})
	}()
	waitsOnLock(t, "foundry_ontologies", func() bool { return len(published) > 0 })
	_, err = page.ExecContext(ctx, `UPDATE foundry_heal_fills SET collect_after=?, collect_version_id=?, collect_owed=1 WHERE id=?`,
		f.a, fill.ToVersionID, fill.ID)
	require.NoError(t, err)
	require.NoError(t, page.Commit())
	require.NoError(t, <-published)
	assert.Equal(t, model.FoundryHealSuperseded, f.healRows(t)[fingerprint("FILLSW")].Status)
	assert.Nil(t, f.collectAt(t, "FILLSW"), "the page's cursor goes with its finding")
	require.NoError(t, f.db.ReleaseFoundryFillCollect(ctx, fill))
	claimed, err := f.db.ClaimFoundryFillCollect(ctx, other, time.Hour)
	require.NoError(t, err)
	assert.True(t, claimed, "another fill of the load collects")
}

// A page whose finding closes while it is written, by a writer that takes
// no ontology lock: the page waits for it, then owes and saves nothing.
func TestFoundryFillPageWaitsForItsFindingClosing(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLPW")
	fill := f.collecting(t, f.moved(t, "FILLPW"))
	closing, err := testDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = closing.Rollback() }()
	_, err = closing.ExecContext(ctx, `UPDATE foundry_heal_findings SET status='superseded' WHERE ontology_id=? AND fingerprint=?`,
		f.ontology.ID, fingerprint("FILLPW"))
	require.NoError(t, err)
	type result struct {
		ok  bool
		err error
	}
	saved := make(chan result, 1)
	go func() {
		ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, pageAt(fill, []string{f.a}))
		saved <- result{ok, err}
	}()
	waitsOnLock(t, "foundry_heal_findings", func() bool { return len(saved) > 0 })
	require.NoError(t, closing.Commit())
	r := <-saved
	require.NoError(t, r.err)
	assert.False(t, r.ok)
	assert.Zero(t, f.linksOwed(t))
	assert.Nil(t, f.collectAt(t, "FILLPW"))
}

// A cursor at the load's version whose finding isn't open (the fill is
// never due, so it never resumes) holds no other fill of the load back:
// another's collect is claimed, and starts over, ending what it owed.
func TestFoundryFillCursorOfAClosedFindingHoldsNothingBack(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLCH2")
	paged := f.collecting(t, f.moved(t, "FILLCH2"))
	other := f.moved(t, "FILLCH3")
	ok, err := f.db.OweFoundryFillDocuments(ctx, paged, f.ontology.WorkspaceID, []string{f.a}, pageAt(paged, []string{f.a}))
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, f.db.ReleaseFoundryFillCollect(ctx, paged))
	_, err = testDB.Exec(`UPDATE foundry_heal_findings SET status='superseded' WHERE ontology_id=? AND fingerprint=?`, f.ontology.ID, fingerprint("FILLCH2"))
	require.NoError(t, err)
	claimed, err := f.db.ClaimFoundryFillCollect(ctx, other, time.Hour)
	require.NoError(t, err)
	require.True(t, claimed)
	ok, err = f.db.RestartFoundryFillCollect(ctx, other, f.ontology.WorkspaceID, other.ToVersionID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Nil(t, f.collectAt(t, "FILLCH2"))
	assert.Zero(t, f.linksOwed(t))
}

// A collect whose cursor was cleared under it (started over: a document
// became reviewed behind it) saves no page past it, and starts no fill:
// the start says the collect is stale, nothing changed.
func TestFoundryFillCollectClearedUnderIt(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLCU2")
	fill := f.collecting(t, f.moved(t, "FILLCU2"))
	ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, pageAt(fill, []string{f.a}))
	require.NoError(t, err)
	require.True(t, ok)
	_, err = testDB.Exec(`UPDATE foundry_heal_fills SET collect_after=NULL, collect_version_id=NULL, collect_owed=0 WHERE id=?`, fill.ID)
	require.NoError(t, err)
	assert.ErrorIs(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLCU2")}),
		store.ErrFillCollectMoved)
	ok, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.b}, pageAt(fill, []string{f.b}))
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, 1, f.linksOwed(t), "only the first page's")
	got := f.fill(t, "FILLCU2")
	assert.Equal(t, model.FoundryFillMoved, got.State)
	assert.Nil(t, got.Collect)
	assert.Zero(t, got.Failures, "a collect started over counts no failure")
	// Started over, it owes from the first page again.
	ok, err = f.db.RestartFoundryFillCollect(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Zero(t, f.linksOwed(t))
}

// A move in place that lands while a collect runs: no page listed at the
// version it moved from is saved, no fill starts on them, and only a
// collect started over at the new version writes.
func TestFoundryFillCollectWhileTheLoadMoves(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLMV")
	fill := f.collecting(t, f.moved(t, "FILLMV"))
	ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, pageAt(fill, []string{f.a}))
	require.NoError(t, err)
	require.True(t, ok)
	later := f.draftVersion(t, "FILLMV-2", customerTOML)
	_, err = testDB.Exec(`UPDATE foundry_loads SET version_id=? WHERE id=?`, later.ID, f.load)
	require.NoError(t, err)
	ok, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.b}, pageAt(fill, []string{f.b}))
	require.NoError(t, err)
	assert.False(t, ok, "a page listed at the old version")
	assert.Equal(t, 1, f.linksOwed(t))
	assert.ErrorIs(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLMV")}),
		store.ErrFillCollectMoved)
	assert.Equal(t, model.FoundryFillMoved, f.fill(t, "FILLMV").State)
	ok, err = f.db.RestartFoundryFillCollect(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID)
	require.NoError(t, err)
	assert.False(t, ok, "nor restarted at it")
	ok, err = f.db.RestartFoundryFillCollect(ctx, fill, f.ontology.WorkspaceID, later.ID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Zero(t, f.linksOwed(t))
	page := model.FoundryFillCursor{After: f.b, VersionID: later.ID, Owed: 1}
	ok, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.b}, page)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, later.ID, 1, 0, &model.FoundryGraphJob{ID: id26("RS-FILLMV")}))
	assert.Equal(t, model.FoundryFillApplying, f.fill(t, "FILLMV").State)
}

// A document that becomes one of the load's reviewed documents behind a
// collect's cursor (a partial load's document extracted) rewinds the
// cursor to the document before it, so the collect lists it next rather
// than never (pages go forward from a cursor) and doesn't start over; a
// page that owes a document again counts it once. One ahead of the
// cursor, or reviewed already, moves nothing.
func TestFoundryFillDocumentReviewedBehindTheCursor(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLBH")
	fill := f.collecting(t, f.moved(t, "FILLBH"))
	ahead, behind := f.uploaded(t, "FILLBH-c"), f.uploaded(t, "FILLBH-d")
	job := f.claim(t)
	require.Equal(t, model.FoundryGraphJobExtract, job.Kind)
	reviewed := func(doc string) *model.FoundryDocumentExtraction {
		e := extracted(doc)
		e.Counts.Reviews = 2
		return e
	}
	first := model.FoundryFillCursor{After: strings.Repeat("0", 26), VersionID: fill.ToVersionID, Owed: 1}
	ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, first)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, reviewed(ahead), false))
	assert.Equal(t, &first, f.collectAt(t, "FILLBH"), "a document ahead of the cursor is listed when it gets there")
	last := model.FoundryFillCursor{After: strings.Repeat("Z", 26), VersionID: fill.ToVersionID, Owed: 2, Left: 3}
	ok, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.b}, last)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, extracted(f.a), true))
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, reviewed(ahead), true))
	assert.Equal(t, &last, f.collectAt(t, "FILLBH"), "a document reviewed already, or with none, moves nothing")
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, reviewed(behind), false))
	var before string
	err = testDB.QueryRow(`SELECT document_id FROM foundry_load_extractions WHERE load_id=? AND document_id<? ORDER BY document_id DESC LIMIT 1`,
		f.load, behind).Scan(&before)
	if !errors.Is(err, sql.ErrNoRows) {
		require.NoError(t, err)
	}
	rewound := &model.FoundryFillCursor{After: before, VersionID: fill.ToVersionID, Owed: 2}
	assert.Equal(t, rewound, f.collectAt(t, "FILLBH"), "rewound to the document before it, left from none")
	assert.Equal(t, 2, f.linksOwed(t), "what the pages owed stays owed")
	// The collect running saves nothing past it, and starts nothing.
	assert.ErrorIs(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 2, 3, &model.FoundryGraphJob{ID: id26("RS-FILLBH")}),
		store.ErrFillCollectMoved)
	ok, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, pageAt(fill, []string{f.a}))
	require.NoError(t, err)
	assert.False(t, ok)
	// The next resumes from it, owing the document and, again, those after
	// it: each counts once.
	fill.Collect = rewound
	again := []string{behind, f.a, f.b}
	ok, err = f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, again,
		model.FoundryFillCursor{After: slices.Max(again), VersionID: fill.ToVersionID, Owed: 5})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 3, f.linksOwed(t))
	assert.Equal(t, int64(3), fill.Collect.Owed, "the cursor counts the rows owed")
	assert.Equal(t, int64(3), f.collectAt(t, "FILLBH").Owed)
	require.NoError(t, f.db.StartFoundryFill(ctx, fill, f.ontology.WorkspaceID, fill.ToVersionID, 3, 0, &model.FoundryGraphJob{ID: id26("RS-FILLBH")}))
	assert.Equal(t, model.FoundryFillApplying, f.fill(t, "FILLBH").State)
}

// A collect that saved a page, then fails again and again with none saved,
// keeps its cursor and what it owed while it backs off, and once it has
// failed too often needs a load: what it owed ends, and its cursor.
func TestFoundryFillPagedCollectFailsThenNeedsALoad(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "FILLPF")
	fill := f.collecting(t, f.moved(t, "FILLPF"))
	ok, err := f.db.OweFoundryFillDocuments(ctx, fill, f.ontology.WorkspaceID, []string{f.a}, pageAt(fill, []string{f.a}))
	require.NoError(t, err)
	require.True(t, ok)
	saved := f.collectAt(t, "FILLPF")
	require.NotNil(t, saved)
	require.NoError(t, f.db.ReleaseFoundryFillCollect(ctx, fill))
	for range len(model.FoundryFillBackoff) {
		f.collecting(t, fill)
		require.NoError(t, f.db.FailFoundryFillCollect(ctx, fill, "agent down", false))
		got := f.fill(t, "FILLPF")
		require.Equal(t, model.FoundryFillMoved, got.State)
		assert.Equal(t, saved, got.Collect)
		assert.Equal(t, 1, f.linksOwed(t))
		_, err := testDB.Exec(`UPDATE foundry_heal_fills SET paused_until=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, fill.ID)
		require.NoError(t, err)
	}
	f.collecting(t, fill)
	require.NoError(t, f.db.FailFoundryFillCollect(ctx, fill, "agent down", false))
	got := f.fill(t, "FILLPF")
	assert.Equal(t, model.FoundryFillNeedsLoad, got.State)
	assert.Nil(t, got.Collect)
	assert.Zero(t, f.linksOwed(t))
}
