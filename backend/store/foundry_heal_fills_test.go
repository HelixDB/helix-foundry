package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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

// Rows of a reason this binary doesn't know ('link', as a later binary owes
// them) are never listed, sent, ended, failed or counted owed: rolling back
// leaves them as they are, and queues no job for them.
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
// put right from the recorded move, with its live jobs; never to a version
// no move of it records, nor from a version it isn't at.
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
	// A move that needs a load moved nothing, so puts nothing right.
	g := newResendFixture(t, "RECONNL")
	gFrom, _ := g.loadVersion(t)
	gTo := g.draftVersion(t, "RECONNL-2", customerTOML)
	g.moveRow(t, "RECONNL", gFrom, gTo.ID, model.FoundryFillNeedsLoad)
	got, err := g.db.GetOntologyVersion(ctx, g.ontology.ID, gTo.ID)
	require.NoError(t, err)
	reconciled, err = g.db.ReconcileFoundryLoadMarker(ctx, g.load, (&model.FoundryLoad{Version: got.Version}).OntologyVersion())
	require.NoError(t, err)
	assert.False(t, reconciled)
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
	assert.Equal(t, to.ID, f.ontologyNow(t).CurrentVersionID)
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
	asked, err := f.db.FoundryAskedApart(ctx, f.ontology.ID, to.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"ceo_of"}, asked)
	asked, err = f.db.FoundryAskedApart(ctx, f.ontology.ID, from)
	require.NoError(t, err)
	assert.Empty(t, asked)
	assert.Equal(t, model.FoundryGraphJobEnded, f.end(t, job, "moved", model.FoundryGraphJobSucceeded))
	assert.Nil(t, f.liveJob(t), "nothing more is owed")
	// Pressed again: the load is at the version, so nothing moves.
	again, err := f.press(t, "2", to.ID, &to.ID, fp)
	require.NoError(t, err)
	assert.Nil(t, again)
	assert.Zero(t, f.moveOwed(t))
	// A second move along the chain asks both links apart.
	next := f.draftVersion(t, "MOVE-3", customerTOML)
	f.generated(t, next.ID, "customers")
	fill, err = f.press(t, "3", next.ID, &to.ID, fingerprint("MOVE3"))
	require.NoError(t, err)
	job = f.claim(t)
	require.NoError(t, f.db.RecordFoundryLoadMove(ctx, job.ID, f.worker, fill,
		model.FoundryFoldOutcome{State: model.FoundryFillMoved, LinksAdded: []string{"cfo_of"}}))
	asked, err = f.db.FoundryAskedApart(ctx, f.ontology.ID, next.ID)
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
// queues no job for it alone: the next job that ends sends it. Past three
// failed jobs the move is given up as needing a load.
func TestFoundryLoadMoveOwedPastAFailedJob(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "MOVEF")
	from, _ := f.loadVersion(t)
	f.generated(t, from, "customers")
	to := f.draftVersion(t, "MOVEF-2", customerTOML)
	f.generated(t, to.ID, "customers")
	_, err := f.press(t, "1", to.ID, f.ontologyNow(t).CurrentVersionID, fingerprint("MOVEF"))
	require.NoError(t, err)
	job := f.claim(t)
	f.end(t, job, "failed", model.FoundryGraphJobFailed)
	assert.Equal(t, 1, f.moveOwed(t))
	assert.Nil(t, f.liveJob(t), "a failed move doesn't follow itself")
	owed, err := f.db.FoundryLoadMoveOwed(ctx, f.load)
	require.NoError(t, err)
	require.NotNil(t, owed)
	// An approval's resend sends it again; two more failures give it up.
	for i, d := range []*model.FoundryReviewDecision{f.held(f.a, 0, readA), f.held(f.b, 1, readB)} {
		require.True(t, f.approve(t, fmt.Sprint(i), d))
		f.end(t, f.claim(t), fmt.Sprint("failed", i), model.FoundryGraphJobFailed)
	}
	assert.Zero(t, f.moveOwed(t))
	fills, err := f.db.ListFoundryHealFills(ctx, f.load)
	require.NoError(t, err)
	fill := fills[fingerprint("MOVEF")]
	assert.Equal(t, model.FoundryFillNeedsLoad, fill.State)
	require.Len(t, fill.Why, 1)
	assert.Equal(t, "move_failed", fill.Why[0].Kind)
	assert.Nil(t, f.liveJob(t))
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
	f.loaded(t, "w-rebuild")
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
