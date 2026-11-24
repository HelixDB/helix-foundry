package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moveRow records a move of the fixture's load from version from to version
// to, in state, as "Add this link" writes one.
func (f resendFixture) moveRow(t *testing.T, tag, from, to string, state model.FoundryHealFillState) {
	t.Helper()
	why := any(nil)
	if state == model.FoundryFillNeedsLoad {
		why = `[{"kind":"type_added","item":"types.Deal"}]`
	}
	_, err := testDB.Exec(`INSERT INTO foundry_heal_fills (id, ontology_id, load_id, fingerprint, from_version_id, to_version_id, state, why)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id26("HF-"+tag), f.ontology.ID, f.load, fingerprint(tag), from, to, string(state), why)
	require.NoError(t, err)
}

// owe owes the fixture's load a row of reason for document doc, as a later
// binary writes one.
func (f resendFixture) owe(t *testing.T, doc, reason string) {
	t.Helper()
	_, err := testDB.Exec(`INSERT INTO foundry_load_resends (load_id, document_id, reason) VALUES (?, ?, ?)`, f.load, doc, reason)
	require.NoError(t, err)
}

// loadVersion reads the version a load's row records, and its live jobs'.
func (f resendFixture) loadVersion(t *testing.T) (load string, jobs []string) {
	t.Helper()
	require.NoError(t, testDB.QueryRow(`SELECT version_id FROM foundry_loads WHERE id=?`, f.load).Scan(&load))
	rows, err := testDB.Query(`SELECT version_id FROM foundry_load_jobs WHERE load_id=? AND status IN ('queued', 'running') ORDER BY id`, f.load)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var v string
		require.NoError(t, rows.Scan(&v))
		jobs = append(jobs, v)
	}
	return load, jobs
}

// Rows of a reason this binary doesn't know (as a later binary owes them)
// are never listed, sent, ended, failed or counted owed: rolling back leaves
// them as they are, and queues no job for them. Nor are 'link' rows of a
// load whose fill doesn't apply.
func TestFoundryResendsOfAReasonUnknownStayAsTheyAre(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RUNKNOWN")
	f.owe(t, "", "later")
	f.owe(t, f.a, "link")
	assert.Empty(t, f.owed(t), "not listed")
	// An approval queues a resend, which sends only the approval's document.
	d := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", d))
	job := f.claim(t)
	owed, err := f.db.ListFoundryLoadResends(ctx, f.load)
	require.NoError(t, err)
	require.Len(t, owed, 1)
	assert.Equal(t, model.FoundryResendApproval, owed[0].Reason)
	// The job fails: what it knows ends, the rest stays owed.
	f.end(t, job, "failed", model.FoundryGraphJobFailed)
	var known, unknown int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_resends WHERE load_id=? AND status='owed' AND reason='approval'`, f.load).Scan(&known))
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_resends WHERE load_id=? AND status='owed' AND reason IN ('later', 'link')`, f.load).Scan(&unknown))
	assert.Equal(t, 0, known)
	assert.Equal(t, 2, unknown)
	assert.Nil(t, f.liveJob(t), "nothing queued for them: they aren't owed here")
}

// A load whose engine marker moved in place before Foundry recorded it is
// put right from the recorded move, in whatever state, with its live jobs;
// never to a version no move of it records, nor from a version it isn't at.
func TestFoundryLoadMarkerReconciledOnlyFromARecordedMove(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RECON")
	from, _ := f.loadVersion(t)
	to := f.draftVersion(t, "RECON-2", customerTOML)
	other := f.draftVersion(t, "RECON-3", customerTOML)
	label := func(v *model.OntologyVersion) string {
		got, err := f.db.GetOntologyVersion(ctx, f.ontology.ID, v.ID)
		require.NoError(t, err)
		return (&model.FoundryLoad{Version: got.Version}).OntologyVersion()
	}
	// No move recorded: nothing changes.
	reconciled, err := f.db.ReconcileFoundryLoadMarker(ctx, f.load, label(to))
	require.NoError(t, err)
	assert.False(t, reconciled)
	// A queued job of the load at its version moves with it.
	d := f.held(f.a, 0, readA)
	require.True(t, f.approve(t, "1", d))
	f.moveRow(t, "RECON", from, to.ID, model.FoundryFillMoving)
	reconciled, err = f.db.ReconcileFoundryLoadMarker(ctx, f.load, label(other))
	require.NoError(t, err)
	assert.False(t, reconciled, "a marker no move records is never adopted")
	reconciled, err = f.db.ReconcileFoundryLoadMarker(ctx, f.load, label(to))
	require.NoError(t, err)
	assert.True(t, reconciled)
	load, jobs := f.loadVersion(t)
	assert.Equal(t, to.ID, load)
	assert.Equal(t, []string{to.ID}, jobs)
	// Again: the load is at the version the move started from no more.
	reconciled, err = f.db.ReconcileFoundryLoadMarker(ctx, f.load, label(to))
	require.NoError(t, err)
	assert.False(t, reconciled)
	// A move recorded as needing a load whose marker is at its version after
	// all (a fold given up on) is put right too, and owed again.
	g := newResendFixture(t, "RECONNL")
	gFrom, _ := g.loadVersion(t)
	gTo := g.draftVersion(t, "RECONNL-2", customerTOML)
	g.moveRow(t, "RECONNL", gFrom, gTo.ID, model.FoundryFillNeedsLoad)
	got, err := g.db.GetOntologyVersion(ctx, g.ontology.ID, gTo.ID)
	require.NoError(t, err)
	reconciled, err = g.db.ReconcileFoundryLoadMarker(ctx, g.load, (&model.FoundryLoad{Version: got.Version}).OntologyVersion())
	require.NoError(t, err)
	assert.True(t, reconciled)
	fills, err := g.db.ListFoundryHealFills(ctx, g.load)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryFillMoving, fills[fingerprint("RECONNL")].State)
	assert.Equal(t, 1, g.moveOwed(t))
}

// The table's checks keep its states whole: a needs_load row says why, a
// stopped one why it stopped, and only an applying one holds its load's key.
func TestFoundryHealFillChecks(t *testing.T) {
	f := newResendFixture(t, "FCHK")
	from, _ := f.loadVersion(t)
	insert := func(tag, state string, why, stop, active any) error {
		_, err := testDB.Exec(`INSERT INTO foundry_heal_fills (id, ontology_id, load_id, fingerprint, from_version_id, to_version_id, state, why,
  stop_reason, active_key) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id26("HF-"+tag), f.ontology.ID, f.load, fingerprint(tag), from, from, state,
			why, stop, active)
		return err
	}
	assert.Error(t, insert("A", "needs_load", nil, nil, nil), "needs_load without why")
	assert.Error(t, insert("B", "stopped", nil, nil, nil), "stopped without a reason")
	assert.Error(t, insert("C", "moved", nil, "dismissed", nil), "a reason while not stopped")
	assert.Error(t, insert("D", "applying", nil, nil, nil), "applying without its key")
	assert.Error(t, insert("E", "moved", nil, nil, f.load), "a key while not applying")
	assert.Error(t, insert("F", "applying", nil, nil, f.a), "another load's key")
	assert.Error(t, insert("G", "other", nil, nil, nil), "an unknown state")
	require.NoError(t, insert("H", "applying", nil, nil, f.load))
	assert.Error(t, insert("I", "applying", nil, nil, f.load), "one fill applies per load")
	require.NoError(t, insert("J", "stopped", nil, "dismissed", nil))
	require.NoError(t, insert("K", "needs_load", `[]`, nil, nil))
}

// generated gives version versionID of f a generation that read tables.
func (f resendFixture) generated(t *testing.T, versionID string, tables ...string) {
	t.Helper()
	in := model.OntologyGenerationInput{}
	for _, table := range tables {
		in.Tables = append(in.Tables, model.OntologyGenerationTable{ConnectorID: id26("RC-" + table), Table: table})
	}
	input, err := json.Marshal(in)
	require.NoError(t, err)
	gen := id26("G" + strings.TrimRight(versionID[3:], "0"))
	_, err = testDB.Exec(`INSERT INTO foundry_ontology_generations (id, workspace_id, ontology_id, status, input) VALUES (?, ?, ?, 'succeeded', ?)`,
		gen, f.ontology.WorkspaceID, f.ontology.ID, string(input))
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_ontology_versions SET generation_id=? WHERE id=?`, gen, versionID)
	require.NoError(t, err)
}

// press is a finding's "Add this link" making version current, as resend
// job RS-<tag>: the move's fill row, if any.
func (f resendFixture) press(t *testing.T, tag, version string, expected *string, finding string) (*model.FoundryHealFill, error) {
	t.Helper()
	return f.db.SetCurrentOntologyVersionFilling(context.Background(), f.ontology.WorkspaceID, f.ontology.ID, version, expected, finding,
		&model.FoundryGraphJob{ID: id26("RS-" + tag)})
}

// moveFailed ends job failed, having sent its load's move
// (FoundryGraphJobOutcome.MoveSent), naming what it may queue after by tag.
func (f resendFixture) moveFailed(t *testing.T, job *model.FoundryGraphJob, tag string) {
	t.Helper()
	_, err := f.db.FinishFoundryGraphJob(context.Background(), job.ID, f.worker, model.FoundryGraphJobOutcome{
		Status: model.FoundryGraphJobFailed, Next: next(tag), MoveSent: true,
	})
	require.NoError(t, err)
}

// moveOwed counts the load's owed move requests.
func (f resendFixture) moveOwed(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_resends WHERE load_id=? AND reason='move' AND status='owed'`, f.load).Scan(&n))
	return n
}

// A finding's "Add this link" makes the version current and owes the load a
// move in place, which a resend job sends: recorded moved, the load, its
// live job and every later Extract of the version's chain ask the new link
// apart. Pressed again once moved, nothing more is owed.
func TestFoundryLoadMovesInPlaceForAFinding(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "MOVE")
	from, _ := f.loadVersion(t)
	f.generated(t, from, "customers")
	to := f.draftVersion(t, "MOVE-2", customerTOML)
	f.generated(t, to.ID, "customers")
	current := f.ontologyNow(t).CurrentVersionID
	fp := fingerprint("MOVE")
	// The version the caller saw current isn't: refused, nothing owed.
	other := id26("FV-OTHER")
	_, err := f.press(t, "0", to.ID, &other, fp)
	assert.ErrorIs(t, err, store.ErrConflict)
	assert.Zero(t, f.moveOwed(t))
	fill, err := f.press(t, "1", to.ID, current, fp)
	require.NoError(t, err)
	require.NotNil(t, fill)
	assert.Equal(t, model.FoundryFillMoving, fill.State)
	assert.Equal(t, from, fill.FromVersionID)
	assert.Equal(t, to.ID, *f.ontologyNow(t).CurrentVersionID)
	assert.Equal(t, 1, f.moveOwed(t))
	assert.Empty(t, f.owed(t), "a move names no document")
	owed, err := f.db.FoundryLoadMoveOwed(ctx, f.load)
	require.NoError(t, err)
	require.NotNil(t, owed)
	assert.Equal(t, fill.ID, owed.ID)
	job := f.claim(t)
	assert.Equal(t, model.FoundryGraphJobResend, job.Kind)
	assert.Equal(t, from, job.VersionID)
	require.NoError(t, f.db.RecordFoundryLoadMove(ctx, job.ID, f.worker, owed,
		model.FoundryFoldOutcome{State: model.FoundryFillMoved, LinksAdded: []string{"ceo_of"}}))
	load, jobs := f.loadVersion(t)
	assert.Equal(t, to.ID, load)
	assert.Equal(t, []string{to.ID}, jobs, "the running job runs at the new version")
	assert.Zero(t, f.moveOwed(t))
	fills, err := f.db.ListFoundryHealFills(ctx, f.load)
	require.NoError(t, err)
	require.Contains(t, fills, fp)
	assert.Equal(t, model.FoundryFillMoved, fills[fp].State)
	assert.Equal(t, []string{"ceo_of"}, fills[fp].LinksAdded)
	asked, err := f.db.FoundryAskedApart(ctx, f.load)
	require.NoError(t, err)
	assert.Equal(t, []string{"ceo_of"}, asked)
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "moved", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t), "nothing more is owed")
	// Pressed again: the load is at the version, so nothing moves.
	again, err := f.press(t, "2", to.ID, &to.ID, fp)
	require.NoError(t, err)
	assert.Nil(t, again)
	assert.Zero(t, f.moveOwed(t))
	// Another finding of the link the move added, pressed at the version the
	// load is at now: it joins the move, moved, for the heal worker to fill.
	finding := func(tag, canonical string) string {
		_, err := testDB.Exec(`INSERT INTO foundry_heal_findings (ontology_id, fingerprint, kind, status, base_version_id, load_id, finding, support, sources)
VALUES (?, ?, 'unmatched', 'open', ?, ?, ?, 3, 3)`, f.ontology.ID, fingerprint(tag), to.ID, f.load,
			`{"signal":{"kind":"unmatched","canonical":"`+canonical+`","spellings":["`+canonical+`"],"set":"set_1"}}`)
		require.NoError(t, err)
		return fingerprint(tag)
	}
	joined, err := f.press(t, "2b", to.ID, &to.ID, finding("MOVEJ", "ceo_of"))
	require.NoError(t, err)
	require.NotNil(t, joined)
	assert.Equal(t, model.FoundryFillMoved, joined.State)
	assert.Equal(t, []string{"ceo_of"}, joined.LinksAdded)
	assert.Equal(t, from, joined.FromVersionID)
	assert.Equal(t, to.ID, joined.ToVersionID)
	assert.Zero(t, f.moveOwed(t), "nothing moves")
	// One whose link no move of the load added: its documents were asked it.
	_, err = f.press(t, "2c", to.ID, &to.ID, finding("MOVEN", "owns"))
	var refused *store.FillRefusal
	require.ErrorAs(t, err, &refused)
	assert.Contains(t, refused.Why, "asked it")
	// A second move along the chain asks both links apart.
	next := f.draftVersion(t, "MOVE-3", customerTOML)
	f.generated(t, next.ID, "customers")
	fill, err = f.press(t, "3", next.ID, &to.ID, fingerprint("MOVE3"))
	require.NoError(t, err)
	job = f.claim(t)
	require.NoError(t, f.db.RecordFoundryLoadMove(ctx, job.ID, f.worker, fill,
		model.FoundryFoldOutcome{State: model.FoundryFillMoved, LinksAdded: []string{"cfo_of"}}))
	asked, err = f.db.FoundryAskedApart(ctx, f.load)
	require.NoError(t, err)
	assert.Equal(t, []string{"ceo_of", "cfo_of"}, asked)
	f.end(t, job, "moved-again", model.FoundryGraphJobSucceeded)
	// A load a person makes at that version asks no link apart: its
	// documents' saved work holds no call of them, so it asks them with the
	// rest. A refresh of the moved load replays what it asked.
	fresh := loadJob(f.ontology, "MOVE-FRESH")
	fresh.VersionID = next.ID
	require.NoError(t, f.db.CreateFoundryLoad(ctx, fresh))
	asked, err = f.db.FoundryAskedApart(ctx, fresh.LoadID)
	require.NoError(t, err)
	assert.Empty(t, asked)
	_, err = testDB.Exec(`UPDATE foundry_loads SET origin='refresh', refreshes_load_id=? WHERE id=?`, f.load, fresh.LoadID)
	require.NoError(t, err)
	asked, err = f.db.FoundryAskedApart(ctx, fresh.LoadID)
	require.NoError(t, err)
	assert.Equal(t, []string{"ceo_of", "cfo_of"}, asked)
}

// What Foundry tells without the engine needs a load at once: another
// version's sets or tables. What the engine says does too, and the move's
// request ends with the load where it was.
func TestFoundryLoadMoveRefused(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "MOVENL")
	from, _ := f.loadVersion(t)
	f.generated(t, from, "customers")
	tables := f.draftVersion(t, "MOVENL-2", customerTOML)
	f.generated(t, tables.ID, "customers", "invoices")
	current := f.ontologyNow(t).CurrentVersionID
	fill, err := f.press(t, "1", tables.ID, current, fingerprint("MOVENL"))
	require.NoError(t, err)
	require.NotNil(t, fill)
	assert.Equal(t, model.FoundryFillNeedsLoad, fill.State)
	assert.Equal(t, []model.FoundryFoldWhy{{Kind: "sources", Item: "tables"}}, fill.Why)
	assert.Zero(t, f.moveOwed(t))
	assert.Nil(t, f.liveJob(t), "nothing is queued")
	// A version without a generation can't be judged here: a load.
	bare := f.draftVersion(t, "MOVENL-3", customerTOML)
	fill, err = f.press(t, "2", bare.ID, &tables.ID, fingerprint("MOVENL"))
	require.NoError(t, err)
	assert.Equal(t, []model.FoundryFoldWhy{{Kind: "not_a_draft", Item: "version"}}, fill.Why)
	// The engine refuses one Foundry let through.
	same := f.draftVersion(t, "MOVENL-4", customerTOML)
	f.generated(t, same.ID, "customers")
	fill, err = f.press(t, "3", same.ID, &bare.ID, fingerprint("MOVENL"))
	require.NoError(t, err)
	assert.Equal(t, model.FoundryFillMoving, fill.State, "the same finding pressed again replaces its row")
	job := f.claim(t)
	why := []model.FoundryFoldWhy{{Kind: "type_added", Item: "types.Deal"}}
	require.NoError(t, f.db.RecordFoundryLoadMove(ctx, job.ID, f.worker, fill, model.FoundryFoldOutcome{State: model.FoundryFillNeedsLoad, Why: why}))
	load, _ := f.loadVersion(t)
	assert.Equal(t, from, load)
	assert.Zero(t, f.moveOwed(t))
	fills, err := f.db.ListFoundryHealFills(ctx, f.load)
	require.NoError(t, err)
	assert.Equal(t, why, fills[fingerprint("MOVENL")].Why)
	// A lost lease records nothing.
	assert.ErrorIs(t, f.db.RecordFoundryLoadMove(ctx, job.ID, "w-other", fill, model.FoundryFoldOutcome{State: model.FoundryFillMoved}),
		store.ErrNotFound)
}

// A resend job that fails leaves the move owed, unlike what it ends, and
// queues no job for it alone: the next job that ends sends it. Nothing is
// pressed meanwhile, whatever the finding: the fold may have moved the
// marker, and repair reads the versions it was sent with.
func TestFoundryLoadMoveOwedPastAFailedJob(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "MOVEF")
	from, _ := f.loadVersion(t)
	f.generated(t, from, "customers")
	to := f.draftVersion(t, "MOVEF-2", customerTOML)
	f.generated(t, to.ID, "customers")
	other := f.draftVersion(t, "MOVEF-3", customerTOML)
	f.generated(t, other.ID, "customers")
	_, err := f.press(t, "1", to.ID, f.ontologyNow(t).CurrentVersionID, fingerprint("MOVEF"))
	require.NoError(t, err)
	// A job that failed before it sent the move counts nothing against it.
	job := f.claim(t)
	f.end(t, job, "failed-first", model.FoundryGraphJobFailed)
	got := f.fill(t, "MOVEF")
	assert.Zero(t, got.Failures)
	assert.Nil(t, got.PausedUntil)
	queued, err := f.db.ResumeFoundryFill(ctx, got, f.ontology.WorkspaceID, &model.FoundryGraphJob{ID: id26("RS-MOVEF1")})
	require.NoError(t, err)
	require.True(t, queued)
	job = f.claim(t)
	f.moveFailed(t, job, "failed")
	assert.Equal(t, 1, f.moveOwed(t))
	assert.Nil(t, f.liveJob(t), "a failed move doesn't follow itself")
	// It backs off, then the heal worker sends it again: never stuck moving.
	// Meanwhile no job sends it.
	owed, err := f.db.FoundryLoadMoveOwed(ctx, f.load)
	require.NoError(t, err)
	assert.Nil(t, owed, "backing off: no job meets it")
	got = f.fill(t, "MOVEF")
	require.NotNil(t, got.PausedUntil)
	assert.WithinDuration(t, time.Now().Add(model.FoundryFillBackoff[0]), *got.PausedUntil, time.Minute)
	f.finding(t, "MOVEF")
	_, listed := f.dueLoads(t)[f.load]
	assert.False(t, listed, "backing off")
	_, err = testDB.Exec(`UPDATE foundry_heal_fills SET paused_until=NOW(6) - INTERVAL 1 SECOND WHERE load_id=?`, f.load)
	require.NoError(t, err)
	resume, listed := f.dueLoads(t)[f.load]
	require.True(t, listed)
	require.True(t, resume)
	owed, err = f.db.FoundryLoadMoveOwed(ctx, f.load)
	require.NoError(t, err)
	require.NotNil(t, owed)
	due, err := f.db.DueFoundryHealFills(ctx, 10)
	require.NoError(t, err)
	queued, err = f.db.ResumeFoundryFill(ctx, due[0].Fill, f.ontology.WorkspaceID, &model.FoundryGraphJob{ID: id26("RS-MOVEF2")})
	require.NoError(t, err)
	require.True(t, queued)
	retry := f.claim(t)
	f.moveFailed(t, retry, "failed-again")
	assert.Equal(t, 2, f.fill(t, "MOVEF").Failures, "each attempt that sent it counts toward the give-up")
	// The marker may be ahead now: no press rewrites the versions, of this
	// finding or another.
	for _, fp := range []string{fingerprint("MOVEF"), fingerprint("OTHER")} {
		_, err = f.press(t, "again", other.ID, &to.ID, fp)
		var refused *store.FillRefusal
		require.ErrorAs(t, err, &refused)
		assert.Contains(t, refused.Why, "moving to another version")
	}
	fills, err := f.db.ListFoundryHealFills(ctx, f.load)
	require.NoError(t, err)
	assert.Equal(t, to.ID, fills[fingerprint("MOVEF")].ToVersionID)
	assert.Equal(t, to.ID, *f.ontologyNow(t).CurrentVersionID, "a refused press changes nothing")
	// Moved at last: its failures and backoff were the move's, so its
	// collect starts with none.
	_, err = testDB.Exec(`UPDATE foundry_heal_fills SET paused_until=NOW(6) - INTERVAL 1 SECOND WHERE load_id=?`, f.load)
	require.NoError(t, err)
	queued, err = f.db.ResumeFoundryFill(ctx, f.fill(t, "MOVEF"), f.ontology.WorkspaceID, &model.FoundryGraphJob{ID: id26("RS-MOVEF3")})
	require.NoError(t, err)
	require.True(t, queued)
	last := f.claim(t)
	owed, err = f.db.FoundryLoadMoveOwed(ctx, f.load)
	require.NoError(t, err)
	require.NotNil(t, owed)
	require.NoError(t, f.db.RecordFoundryLoadMove(ctx, last.ID, f.worker, owed,
		model.FoundryFoldOutcome{State: model.FoundryFillMoved, LinksAdded: []string{"ceo_of"}}))
	got = f.fill(t, "MOVEF")
	assert.Equal(t, model.FoundryFillMoved, got.State)
	assert.Zero(t, got.Failures)
	assert.Nil(t, got.PausedUntil)
}

// Past three failed jobs a move is given up as needing a load, its error kept
// whole to a character's edge; pressed again it is refused. Should its fold
// have moved the marker after all, the next mismatch puts the load right from
// the row it gave up on, and the move is owed again, to record its links.
func TestFoundryLoadMoveGivenUpThenRepaired(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "MOVEG")
	from, _ := f.loadVersion(t)
	f.generated(t, from, "customers")
	to := f.draftVersion(t, "MOVEG-2", customerTOML)
	f.generated(t, to.ID, "customers")
	_, err := f.press(t, "1", to.ID, f.ontologyNow(t).CurrentVersionID, fingerprint("MOVEG"))
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_heal_fills SET failures=2 WHERE load_id=?`, f.load)
	require.NoError(t, err)
	job := f.claim(t)
	// Two bytes a character, cut at byte 200 lands between them unless cut on an edge.
	said := "x" + strings.Repeat("é", 150)
	_, err = f.db.FinishFoundryGraphJob(ctx, job.ID, f.worker, model.FoundryGraphJobOutcome{
		Status: model.FoundryGraphJobFailed, Error: said, Next: next("given-up"), MoveSent: true,
	})
	require.NoError(t, err)
	assert.Zero(t, f.moveOwed(t))
	fills, err := f.db.ListFoundryHealFills(ctx, f.load)
	require.NoError(t, err)
	fill := fills[fingerprint("MOVEG")]
	assert.Equal(t, model.FoundryFillNeedsLoad, fill.State)
	require.Len(t, fill.Why, 1)
	assert.Equal(t, "move_failed", fill.Why[0].Kind)
	assert.True(t, utf8.ValidString(fill.Why[0].Item) && len(fill.Why[0].Item) <= 200 && strings.HasPrefix(said, fill.Why[0].Item),
		"%q", fill.Why[0].Item)
	_, err = f.press(t, "2", to.ID, &to.ID, fingerprint("MOVEG"))
	var refused *store.FillRefusal
	require.ErrorAs(t, err, &refused)
	assert.Contains(t, refused.Why, "failed")
	// The marker had moved: repaired from the row, whatever its state.
	got, err := f.db.GetOntologyVersion(ctx, f.ontology.ID, to.ID)
	require.NoError(t, err)
	reconciled, err := f.db.ReconcileFoundryLoadMarker(ctx, f.load, (&model.FoundryLoad{Version: got.Version}).OntologyVersion())
	require.NoError(t, err)
	assert.True(t, reconciled)
	load, _ := f.loadVersion(t)
	assert.Equal(t, to.ID, load)
	fills, err = f.db.ListFoundryHealFills(ctx, f.load)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryFillMoving, fills[fingerprint("MOVEG")].State)
	assert.Equal(t, 1, f.moveOwed(t))
	owed, err := f.db.FoundryLoadMoveOwed(ctx, f.load)
	require.NoError(t, err)
	require.NotNil(t, owed)
	assert.Equal(t, to.ID, owed.ToVersionID, "the move to the version the load holds first")
}

// Two findings' moves from one version: the one recorded moved answers the
// other, moved where it was to the same version, else needing a load as the
// load moved elsewhere. A move recorded for a load at neither version is
// refused, for the job to try again.
func TestFoundryLoadMovesFromOneVersion(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "MOVE2")
	from, _ := f.loadVersion(t)
	f.generated(t, from, "customers")
	to := f.draftVersion(t, "MOVE2-2", customerTOML)
	f.generated(t, to.ID, "customers")
	third := f.draftVersion(t, "MOVE2-3", customerTOML)
	f.generated(t, third.ID, "customers")
	a, err := f.press(t, "1", to.ID, f.ontologyNow(t).CurrentVersionID, fingerprint("MOVEA"))
	require.NoError(t, err)
	// Rows a carry may leave: another to the same version, one to another.
	for tag, version := range map[string]string{"MOVEB": to.ID, "MOVEC": third.ID} {
		_, err = testDB.Exec(`INSERT INTO foundry_heal_fills (id, ontology_id, load_id, fingerprint, from_version_id, to_version_id, state)
VALUES (?, ?, ?, ?, ?, ?, 'moving')`, id26("HF-"+tag), f.ontology.ID, f.load, fingerprint(tag), from, version)
		require.NoError(t, err)
	}
	job := f.claim(t)
	// A load at neither version records nothing.
	_, err = testDB.Exec(`UPDATE foundry_loads SET version_id=? WHERE id=?`, third.ID, f.load)
	require.NoError(t, err)
	err = f.db.RecordFoundryLoadMove(ctx, job.ID, f.worker, a, model.FoundryFoldOutcome{State: model.FoundryFillMoved, LinksAdded: []string{"ceo_of"}})
	assert.ErrorIs(t, err, store.ErrConflict)
	_, err = testDB.Exec(`UPDATE foundry_loads SET version_id=? WHERE id=?`, from, f.load)
	require.NoError(t, err)
	require.NoError(t, f.db.RecordFoundryLoadMove(ctx, job.ID, f.worker, a,
		model.FoundryFoldOutcome{State: model.FoundryFillMoved, LinksAdded: []string{"ceo_of"}}))
	fills, err := f.db.ListFoundryHealFills(ctx, f.load)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryFillMoved, fills[fingerprint("MOVEA")].State)
	assert.Equal(t, model.FoundryFillMoved, fills[fingerprint("MOVEB")].State)
	assert.Equal(t, []string{"ceo_of"}, fills[fingerprint("MOVEB")].LinksAdded)
	c := fills[fingerprint("MOVEC")]
	assert.Equal(t, model.FoundryFillNeedsLoad, c.State)
	require.Len(t, c.Why, 1)
	assert.Equal(t, "load_moved", c.Why[0].Kind)
	assert.Equal(t, third.ID, c.ToVersionID, "its versions kept")
	assert.Zero(t, f.moveOwed(t), "nothing moves now")
}

// A load that replaces the current one as a job ends, of the version a move
// started from, is moved too: the move follows it, owed there.
func TestFoundryLoadMoveCarriedToALoadOfItsVersion(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "MOVEC")
	from, _ := f.loadVersion(t)
	f.generated(t, from, "customers")
	to := f.draftVersion(t, "MOVEC-2", customerTOML)
	f.generated(t, to.ID, "customers")
	fill, err := f.press(t, "1", to.ID, f.ontologyNow(t).CurrentVersionID, fingerprint("MOVEC"))
	require.NoError(t, err)
	job := f.claim(t)
	require.NoError(t, f.db.RecordFoundryLoadMove(ctx, job.ID, f.worker, fill,
		model.FoundryFoldOutcome{State: model.FoundryFillMoved, LinksAdded: []string{"ceo_of"}}))
	f.end(t, job, "moved", model.FoundryGraphJobSucceeded)
	// A load of the version the move started from, as a rebuild queued before it.
	rebuild := loadJob(f.ontology, "MOVEC-B")
	rebuild.VersionID = from
	require.NoError(t, f.db.CreateFoundryLoad(ctx, rebuild))
	f.loaded(t, "w-rebuild", f.a, f.b)
	require.Equal(t, rebuild.LoadID, *f.ontologyNow(t).CurrentLoadID)
	fills, err := f.db.ListFoundryHealFills(ctx, rebuild.LoadID)
	require.NoError(t, err)
	carried := fills[fingerprint("MOVEC")]
	require.NotNil(t, carried)
	assert.Equal(t, model.FoundryFillMoving, carried.State)
	assert.Equal(t, to.ID, carried.ToVersionID)
	var n int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_resends WHERE load_id=? AND reason='move' AND status='owed'`,
		rebuild.LoadID).Scan(&n))
	assert.Equal(t, 1, n)
	owed, err := f.db.FoundryLoadMoveOwed(ctx, rebuild.LoadID)
	require.NoError(t, err)
	require.NotNil(t, owed)
	assert.NotNil(t, f.liveJob(t), "a resend is queued to move it")
}
