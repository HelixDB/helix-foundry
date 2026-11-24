package store_test

import (
	"context"
	"testing"

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

// Rows of a reason this binary doesn't know ('move', 'link', as a later
// binary owes them) are never listed, sent, ended, failed or counted owed:
// rolling back leaves them as they are, and queues no job for them.
func TestFoundryResendsOfAReasonUnknownStayAsTheyAre(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "RUNKNOWN")
	f.owe(t, "", "move")
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
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_resends WHERE load_id=? AND status='owed' AND reason IN ('move', 'link')`, f.load).Scan(&unknown))
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
