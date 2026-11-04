package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// fingerprint is a 64-hex fingerprint named by tag.
func fingerprint(tag string) string { return tag + strings.Repeat("0", 64-len(tag)) }

// unmatchedSeen is an unmatched finding a run found, backed by support
// reviews from sources documents, spelled as spellings (none: 4.4.0's,
// named by its canonical alone).
func unmatchedSeen(tag string, support, sources int64, spellings ...string) model.FoundryHealSeen {
	signal := map[string]any{"kind": "unmatched", "canonical": tag, "set": "set_1",
		"backing": map[string]any{"count": support, "sources": sources, "examples": []any{}}}
	if len(spellings) > 0 {
		signal["spellings"] = spellings
	}
	raw, _ := json.Marshal(map[string]any{"fingerprint": fingerprint(tag), "signal": signal, "proposal": nil})
	return model.FoundryHealSeen{Fingerprint: fingerprint(tag), Kind: "unmatched", Finding: raw, Support: support, Sources: sources}
}

// rowSeen is a row_errors finding of support rows.
func rowSeen(tag string, support int64) model.FoundryHealSeen {
	raw, _ := json.Marshal(map[string]any{"fingerprint": fingerprint(tag), "proposal": nil,
		"signal": map[string]any{"kind": "row_errors", "mapping": tag, "errors": map[string]any{"count": support, "examples": []string{}}}})
	return model.FoundryHealSeen{Fingerprint: fingerprint(tag), Kind: "row_errors", Finding: raw, Support: support}
}

// healRun starts a run of the ontology's load and publishes what it found,
// returning the run's id.
func (f foundryFixture) healRun(t *testing.T, tag, loadID, versionID string, rowsMeasured bool, seen ...model.FoundryHealSeen) string {
	t.Helper()
	ctx := context.Background()
	run := &model.FoundryHealRun{ID: id26("HR-" + tag), OntologyID: f.ontology.ID, LoadID: loadID}
	require.NoError(t, f.db.StartFoundryHealRun(ctx, run, time.Minute))
	require.NoError(t, f.db.PublishFoundryHealRun(ctx, model.FoundryHealPublish{
		RunID: run.ID, OntologyID: f.ontology.ID, LoadID: loadID, VersionID: versionID, Pages: 1, Findings: seen, RowsMeasured: rowsMeasured,
	}))
	return run.ID
}

// healRows is every row the ontology holds, by fingerprint.
func (f foundryFixture) healRows(t *testing.T) map[string]*model.FoundryHealFinding {
	t.Helper()
	rows, err := testDB.Query(`SELECT fingerprint FROM foundry_heal_findings WHERE ontology_id=?`, f.ontology.ID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	out := map[string]*model.FoundryHealFinding{}
	for rows.Next() {
		var fp string
		require.NoError(t, rows.Scan(&fp))
		got, err := f.db.GetFoundryHealFinding(context.Background(), f.ontology.ID, fp)
		require.NoError(t, err)
		out[fp] = got
	}
	require.NoError(t, rows.Err())
	return out
}

func TestFoundryHealPublish(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "HEAL")
	v := f.currentLoad(t, "HEAL-1", customerTOML)
	load := id26("FJ-HEAL-1")

	// Three runs of the same load find the same findings: one row each,
	// their support the run's, never summed across runs.
	for i := range 3 {
		f.healRun(t, fmt.Sprintf("p%d", i), load, v.ID, true, unmatchedSeen("aa", 5, 3), unmatchedSeen("bb", 4, 1), rowSeen("cc", 7))
	}
	rows := f.healRows(t)
	require.Len(t, rows, 2, "the one-document finding isn't stored")
	assert.Equal(t, int64(5), rows[fingerprint("aa")].Support)
	assert.Equal(t, int64(3), rows[fingerprint("aa")].Sources)
	assert.Equal(t, model.FoundryHealOpen, rows[fingerprint("aa")].Status)
	assert.Equal(t, load, rows[fingerprint("aa")].LoadID)
	assert.Equal(t, v.ID, rows[fingerprint("aa")].BaseVersionID)
	assert.Equal(t, int64(0), rows[fingerprint("cc")].Sources)

	shown, err := f.db.ListFoundryHealFindings(ctx, f.ontology.ID)
	require.NoError(t, err)
	require.Len(t, shown, 2)
	assert.Equal(t, fingerprint("cc"), shown[0].Fingerprint, "by support")

	// A finding thinner than two documents stays stored once it is, but
	// isn't shown.
	f.healRun(t, "thin", load, v.ID, true, unmatchedSeen("aa", 1, 1), rowSeen("cc", 7))
	shown, err = f.db.ListFoundryHealFindings(ctx, f.ontology.ID)
	require.NoError(t, err)
	require.Len(t, shown, 1)
	assert.Equal(t, fingerprint("cc"), shown[0].Fingerprint)

	run, err := f.db.GetLatestFoundryHealRun(ctx, f.ontology.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryHealRunComplete, run.Status)
	assert.Equal(t, 1, run.Pages)
	assert.NotNil(t, run.FinishedAt)
}

func TestFoundryHealShownLimit(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "HLIM")
	v := f.currentLoad(t, "HLIM-1", customerTOML)
	var seen []model.FoundryHealSeen
	for i := range model.FoundryHealShownLimit + 5 {
		seen = append(seen, unmatchedSeen(fmt.Sprintf("f%02d", i), int64(10+i), 2))
	}
	f.healRun(t, "lim", id26("FJ-HLIM-1"), v.ID, true, seen...)
	shown, err := f.db.ListFoundryHealFindings(ctx, f.ontology.ID)
	require.NoError(t, err)
	require.Len(t, shown, model.FoundryHealShownLimit)
	assert.Equal(t, int64(10+model.FoundryHealShownLimit+4), shown[0].Support)
	assert.Equal(t, int64(15), shown[len(shown)-1].Support)
}

func TestFoundryHealDismissal(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "HDIS")
	v1 := f.currentLoad(t, "HDIS-1", customerTOML)
	load1 := id26("FJ-HDIS-1")
	f.healRun(t, "d1", load1, v1.ID, true, unmatchedSeen("aa", 4, 2))
	assert.ErrorIs(t, f.db.DismissFoundryHealFinding(ctx, f.ontology.ID, fingerprint("zz"), id26("user")), store.ErrNotFound)
	require.NoError(t, f.db.DismissFoundryHealFinding(ctx, f.ontology.ID, fingerprint("aa"), id26("user")))
	assert.ErrorIs(t, f.db.DismissFoundryHealFinding(ctx, f.ontology.ID, fingerprint("aa"), id26("user")), store.ErrNotFound, "only an open finding")
	dismissed := f.healRows(t)[fingerprint("aa")]
	require.Equal(t, model.FoundryHealDismissed, dismissed.Status)
	require.Equal(t, &model.FoundryHealDismissal{ByUserID: id26("user"), Support: 4}, dismissed.Dismissal)
	shown, err := f.db.ListFoundryHealFindings(ctx, f.ontology.ID)
	require.NoError(t, err)
	assert.Empty(t, shown)

	// A new version's load: the dismissal holds below twice its support,
	// counting the new load, keeping the finding it was dismissed with.
	v2 := f.currentLoad(t, "HDIS-2", customerTOML)
	load2 := id26("FJ-HDIS-2")
	f.healRun(t, "d2", load2, v2.ID, true, unmatchedSeen("aa", 7, 5))
	held := f.healRows(t)[fingerprint("aa")]
	assert.Equal(t, model.FoundryHealDismissed, held.Status)
	assert.Equal(t, int64(7), held.Support)
	assert.Equal(t, load2, held.LoadID)
	assert.Equal(t, v2.ID, held.BaseVersionID)
	assert.JSONEq(t, string(dismissed.Finding), string(held.Finding))

	// A run that doesn't find it leaves it dismissed: it isn't superseded.
	f.healRun(t, "d3", load2, v2.ID, true)
	assert.Equal(t, model.FoundryHealDismissed, f.healRows(t)[fingerprint("aa")].Status)

	// At twice its support it is open again, its dismissal gone.
	f.healRun(t, "d4", load2, v2.ID, true, unmatchedSeen("aa", 8, 5))
	reopened := f.healRows(t)[fingerprint("aa")]
	assert.Equal(t, model.FoundryHealOpen, reopened.Status)
	assert.Nil(t, reopened.Dismissal)
	assert.Equal(t, int64(8), reopened.Support)
}

// A dismissal is kept with the spellings it was dismissed with: a gathered
// finding that later holds another spelling is shown again, whatever its
// support (the re-keying rule of contract 4.8.0's Audit).
func TestFoundryHealDismissalSpellings(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "HSPL")
	v := f.currentLoad(t, "HSPL-1", customerTOML)
	load := id26("FJ-HSPL-1")
	f.healRun(t, "s1", load, v.ID, true, unmatchedSeen("aa", 4, 2))
	require.NoError(t, f.db.DismissFoundryHealFinding(ctx, f.ontology.ID, fingerprint("aa"), id26("user")))
	// The 4.4.0 finding's one spelling, its canonical name, spelled out.
	f.healRun(t, "s2", load, v.ID, true, unmatchedSeen("aa", 4, 2, "aa"))
	assert.Equal(t, model.FoundryHealDismissed, f.healRows(t)[fingerprint("aa")].Status)
	f.healRun(t, "s3", load, v.ID, true, unmatchedSeen("aa", 4, 2, "aa", "aa_since"))
	assert.Equal(t, model.FoundryHealOpen, f.healRows(t)[fingerprint("aa")].Status)

	require.NoError(t, f.db.DismissFoundryHealFinding(ctx, f.ontology.ID, fingerprint("aa"), id26("user")))
	f.healRun(t, "s4", load, v.ID, true, unmatchedSeen("aa", 5, 2, "aa_since", "aa"))
	assert.Equal(t, model.FoundryHealDismissed, f.healRows(t)[fingerprint("aa")].Status, "the spellings it was dismissed with")
}

func TestFoundryHealSupersede(t *testing.T) {
	f := newFoundryFixture(t, "HSUP")
	v := f.currentLoad(t, "HSUP-1", customerTOML)
	load := id26("FJ-HSUP-1")
	f.healRun(t, "u1", load, v.ID, true, unmatchedSeen("aa", 4, 2), unmatchedSeen("bb", 4, 2), rowSeen("cc", 3))
	// Rows unmeasured: the row finding stays open; the missing review
	// finding goes.
	f.healRun(t, "u2", load, v.ID, false, unmatchedSeen("aa", 4, 2))
	rows := f.healRows(t)
	assert.Equal(t, model.FoundryHealOpen, rows[fingerprint("aa")].Status)
	assert.Equal(t, model.FoundryHealSuperseded, rows[fingerprint("bb")].Status)
	assert.Equal(t, model.FoundryHealOpen, rows[fingerprint("cc")].Status)
	// Rows measured and clean: it goes too.
	f.healRun(t, "u3", load, v.ID, true, unmatchedSeen("aa", 4, 2))
	assert.Equal(t, model.FoundryHealSuperseded, f.healRows(t)[fingerprint("cc")].Status)
	// Found again, a superseded finding is open again.
	f.healRun(t, "u4", load, v.ID, true, unmatchedSeen("aa", 4, 2), unmatchedSeen("bb", 6, 3))
	bb := f.healRows(t)[fingerprint("bb")]
	assert.Equal(t, model.FoundryHealOpen, bb.Status)
	assert.Equal(t, int64(6), bb.Support)
}

func TestFoundryHealRuns(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "HRUN")
	v := f.currentLoad(t, "HRUN-1", customerTOML)
	load := id26("FJ-HRUN-1")
	due := func() []model.FoundryHealDue {
		t.Helper()
		got, err := f.db.DueFoundryHealOntologies(ctx, 24*time.Hour, time.Hour, 10)
		require.NoError(t, err)
		return got
	}
	require.Len(t, due(), 1, "a ready current load with no run is due")
	assert.Equal(t, load, due()[0].Load.ID)
	assert.Equal(t, f.ontology.WorkspaceID, due()[0].WorkspaceID)

	// One running run per ontology: a second worker's start is refused.
	first := &model.FoundryHealRun{ID: id26("HR-1"), OntologyID: f.ontology.ID, LoadID: load}
	require.NoError(t, f.db.StartFoundryHealRun(ctx, first, time.Minute))
	assert.ErrorIs(t, f.db.StartFoundryHealRun(ctx, &model.FoundryHealRun{ID: id26("HR-2"), OntologyID: f.ontology.ID, LoadID: load}, time.Minute), store.ErrConflict)
	assert.Empty(t, due(), "a run is held")
	require.NoError(t, f.db.RenewFoundryHealRun(ctx, first.ID, time.Minute))
	require.NoError(t, f.db.FinishFoundryHealRun(ctx, first.ID, model.FoundryHealRunIncomplete, 3, 1))
	assert.ErrorIs(t, f.db.RenewFoundryHealRun(ctx, first.ID, time.Minute), store.ErrNotFound)
	assert.ErrorIs(t, f.db.FinishFoundryHealRun(ctx, first.ID, model.FoundryHealRunFailed, 3, 1), store.ErrNotFound)
	assert.Error(t, f.db.FinishFoundryHealRun(ctx, first.ID, model.FoundryHealRunComplete, 3, 0), "a complete run publishes")
	assert.Empty(t, due(), "an incomplete run waits retry_after")
	_, err := testDB.Exec(`UPDATE foundry_heal_runs SET created_at = created_at - INTERVAL 2 HOUR, finished_at = finished_at - INTERVAL 2 HOUR WHERE id=?`, first.ID)
	require.NoError(t, err)
	require.Len(t, due(), 1)

	// A run whose lease ran out holds nothing, but waits retry_after like a
	// failed one; the next start fails it, and its worker can neither
	// renew nor publish it.
	lapsed := &model.FoundryHealRun{ID: id26("HR-3"), OntologyID: f.ontology.ID, LoadID: load}
	require.NoError(t, f.db.StartFoundryHealRun(ctx, lapsed, time.Millisecond))
	time.Sleep(5 * time.Millisecond)
	assert.Empty(t, due(), "a lapsed run waits retry_after")
	_, err = testDB.Exec(`UPDATE foundry_heal_runs SET lease_until = lease_until - INTERVAL 2 HOUR WHERE id=?`, lapsed.ID)
	require.NoError(t, err)
	require.Len(t, due(), 1, "a lapsed run holds nothing")
	taker := &model.FoundryHealRun{ID: id26("HR-4"), OntologyID: f.ontology.ID, LoadID: load}
	require.NoError(t, f.db.StartFoundryHealRun(ctx, taker, time.Minute))
	assert.ErrorIs(t, f.db.RenewFoundryHealRun(ctx, lapsed.ID, time.Minute), store.ErrNotFound)
	assert.ErrorIs(t, f.db.PublishFoundryHealRun(ctx, model.FoundryHealPublish{RunID: lapsed.ID, OntologyID: f.ontology.ID, LoadID: load, VersionID: v.ID,
		Findings: []model.FoundryHealSeen{unmatchedSeen("aa", 4, 2)}}), store.ErrNotFound)
	assert.Empty(t, f.healRows(t), "a lapsed run stores nothing")
	require.NoError(t, f.db.PublishFoundryHealRun(ctx, model.FoundryHealPublish{RunID: taker.ID, OntologyID: f.ontology.ID, LoadID: load, VersionID: v.ID, Pages: 2}))
	assert.Empty(t, due(), "complete within every")
	run, err := f.db.GetLatestFoundryHealRun(ctx, f.ontology.ID)
	require.NoError(t, err)
	assert.Equal(t, taker.ID, run.ID)
	assert.Equal(t, model.FoundryHealRunComplete, run.Status)
	_, err = testDB.Exec(`UPDATE foundry_heal_runs SET created_at = created_at - INTERVAL 25 HOUR, finished_at = finished_at - INTERVAL 25 HOUR
WHERE ontology_id=?`, f.ontology.ID)
	require.NoError(t, err)
	require.Len(t, due(), 1, "nightly")

	// A live graph job on the ontology holds the audit off.
	f.load(t, "HRUN-2")
	assert.Empty(t, due())
}

// Two workers starting the same ontology's run at once: exactly one holds
// it.
func TestFoundryHealSingleFlight(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "HONE")
	f.currentLoad(t, "HONE-1", customerTOML)
	var wg sync.WaitGroup
	results := make([]error, 8)
	for i := range results {
		wg.Go(func() {
			results[i] = f.db.StartFoundryHealRun(ctx, &model.FoundryHealRun{ID: id26(fmt.Sprintf("HR-%d", i)), OntologyID: f.ontology.ID, LoadID: id26("FJ-HONE-1")}, time.Minute)
		})
	}
	wg.Wait()
	started := 0
	for _, err := range results {
		if err == nil {
			started++
			continue
		}
		assert.ErrorIs(t, err, store.ErrConflict)
	}
	assert.Equal(t, 1, started)
}

func TestFoundryHealDocuments(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "HDOC")
	f.currentLoad(t, "HDOC-1", customerTOML)
	load := id26("FJ-HDOC-1")
	for i, reviews := range []int{2, 0, 1, 3, 1} {
		status := "extracted"
		if i == 4 {
			status = "duplicate"
		}
		var dup any
		if status == "duplicate" {
			dup = id26("dup")
		}
		_, err := testDB.Exec(`INSERT INTO foundry_load_extractions (load_id, document_id, ontology_id, workspace_id, job_id, status, duplicate_of, reviews)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, load, id26(fmt.Sprintf("D%d", i)), f.ontology.ID, f.ontology.WorkspaceID, load, status, dup, reviews)
		require.NoError(t, err)
	}
	page, err := f.db.ListFoundryHealDocuments(ctx, load, "", 2)
	require.NoError(t, err)
	assert.Equal(t, []string{id26("D0"), id26("D2")}, page)
	page, err = f.db.ListFoundryHealDocuments(ctx, load, page[len(page)-1], 2)
	require.NoError(t, err)
	assert.Equal(t, []string{id26("D3")}, page)
	page, err = f.db.ListFoundryHealDocuments(ctx, load, page[0], 2)
	require.NoError(t, err)
	assert.Empty(t, page)
}

// Only a live ontology of a live workspace, whose current load is ready or
// partial and of an agent bundle, is due.
func TestFoundryHealDueSkips(t *testing.T) {
	ctx := context.Background()
	for name, mutate := range map[string]string{
		"a deleting workspace":              `UPDATE workspaces SET status='deleting' WHERE id=?`,
		"a deleted workspace":               `UPDATE workspaces SET deleted_at=NOW(6) WHERE id=?`,
		"a load being deleted":              `UPDATE foundry_loads SET status='deleting' WHERE workspace_id=?`,
		"a version of Foundry's own format": `UPDATE foundry_ontology_versions SET format='foundry_v1' WHERE workspace_id=?`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFoundryFixture(t, "HSKP")
			f.currentLoad(t, "HSKP-1", customerTOML)
			due, err := f.db.DueFoundryHealOntologies(ctx, 24*time.Hour, time.Hour, 10)
			require.NoError(t, err)
			require.Len(t, due, 1)
			_, err = testDB.Exec(mutate, f.ontology.WorkspaceID)
			require.NoError(t, err)
			due, err = f.db.DueFoundryHealOntologies(ctx, 24*time.Hour, time.Hour, 10)
			require.NoError(t, err)
			assert.Empty(t, due)
		})
	}
}

// The stalest load comes first: one no run has audited, then by its newest
// run's start, whatever the ontologies' ids.
func TestFoundryHealDueOrder(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	var loads []string
	for _, tag := range []string{"HORA", "HORB", "HORC"} {
		f := foundryFixture{db: db, ontology: sampleOntology(t, db, id26("FWS-"+tag), tag)}
		v := f.currentLoad(t, tag+"-1", customerTOML)
		loads = append(loads, id26("FJ-"+tag+"-1"))
		if tag != "HORB" {
			f.healRun(t, tag, loads[len(loads)-1], v.ID, true)
		}
	}
	// HORA's run is older than HORC's.
	_, err := testDB.Exec(`UPDATE foundry_heal_runs SET created_at = created_at - INTERVAL 30 HOUR, finished_at = finished_at - INTERVAL 30 HOUR
WHERE load_id=?`, loads[0])
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_heal_runs SET created_at = created_at - INTERVAL 25 HOUR, finished_at = finished_at - INTERVAL 25 HOUR
WHERE load_id=?`, loads[2])
	require.NoError(t, err)
	due, err := db.DueFoundryHealOntologies(ctx, 24*time.Hour, time.Hour, 10)
	require.NoError(t, err)
	var got []string
	for _, d := range due {
		if slices.Contains(loads, d.Load.ID) {
			got = append(got, d.Load.ID)
		}
	}
	assert.Equal(t, []string{loads[1], loads[0], loads[2]}, got)
}

// A run starts and publishes only while its load is the ontology's current
// one with no graph job live, or started since the run did.
func TestFoundryHealStale(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "HSTL")
	v := f.currentLoad(t, "HSTL-1", customerTOML)
	load := id26("FJ-HSTL-1")
	start := func(tag string) (*model.FoundryHealRun, error) {
		run := &model.FoundryHealRun{ID: id26("HR-" + tag), OntologyID: f.ontology.ID, LoadID: load}
		return run, f.db.StartFoundryHealRun(ctx, run, time.Minute)
	}
	publish := func(run *model.FoundryHealRun) error {
		return f.db.PublishFoundryHealRun(ctx, model.FoundryHealPublish{RunID: run.ID, OntologyID: f.ontology.ID, LoadID: load, VersionID: v.ID,
			Pages: 1, Findings: []model.FoundryHealSeen{unmatchedSeen("aa", 4, 2)}})
	}
	assert.ErrorIs(t, f.db.StartFoundryHealRun(ctx, &model.FoundryHealRun{ID: id26("HR-nc"), OntologyID: f.ontology.ID, LoadID: id26("FJ-elsewhere")}, time.Minute),
		store.ErrStale, "a load that isn't current")

	// A graph job queued after the run started: the run stores nothing,
	// and the worker, asking after each page, learns it at once.
	run, err := start("job")
	require.NoError(t, err)
	changed, err := f.db.FoundryHealLoadChanged(ctx, run.ID, f.ontology.ID, load)
	require.NoError(t, err)
	assert.False(t, changed)
	f.load(t, "HSTL-2")
	changed, err = f.db.FoundryHealLoadChanged(ctx, run.ID, f.ontology.ID, load)
	require.NoError(t, err)
	assert.True(t, changed, "a graph job since the run started")
	assert.ErrorIs(t, publish(run), store.ErrStale)
	assert.Empty(t, f.healRows(t))
	_, err = start("live")
	assert.ErrorIs(t, err, store.ErrStale, "a live graph job holds the start off")
}

// A load made current without a graph job (set current) is stale too.
func TestFoundryHealStaleCurrent(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "HSTC")
	v := f.currentLoad(t, "HSTC-1", customerTOML)
	load := id26("FJ-HSTC-1")
	run := &model.FoundryHealRun{ID: id26("HR-c1"), OntologyID: f.ontology.ID, LoadID: load}
	require.NoError(t, f.db.StartFoundryHealRun(ctx, run, time.Minute))
	_, err := testDB.Exec(`UPDATE foundry_ontologies SET current_load_id=NULL WHERE id=?`, f.ontology.ID)
	require.NoError(t, err)
	changed, err := f.db.FoundryHealLoadChanged(ctx, run.ID, f.ontology.ID, load)
	require.NoError(t, err)
	assert.True(t, changed, "a load no longer current")
	assert.ErrorIs(t, f.db.PublishFoundryHealRun(ctx, model.FoundryHealPublish{RunID: run.ID, OntologyID: f.ontology.ID, LoadID: load, VersionID: v.ID,
		Pages: 1}), store.ErrStale)
}

// Once a publish's first transaction passed, every page was read: a graph
// job queued then changes nothing the run stores, and the run completes.
// The load stops being current, though, and the next batch stops.
func TestFoundryHealPublishAfterFirstBatch(t *testing.T) {
	ctx := context.Background()
	var seen []model.FoundryHealSeen
	for i := range store.HealPublishBatch + 10 {
		seen = append(seen, unmatchedSeen(fmt.Sprintf("b%03d", i), 4, 2))
	}
	for name, c := range map[string]struct {
		after string
		want  error
	}{
		"a graph job queued":        {after: "job", want: nil},
		"the load made not current": {after: "current", want: store.ErrStale},
	} {
		t.Run(name, func(t *testing.T) {
			tag := "HPA" + strings.ToUpper(c.after[:1])
			f := newFoundryFixture(t, tag)
			v := f.currentLoad(t, tag+"-1", customerTOML)
			load := id26("FJ-" + tag + "-1")
			once := false
			store.SetAfterHealPublishBatch(t, func() {
				if once {
					return
				}
				once = true
				if c.after == "job" {
					f.load(t, tag+"-2")
					return
				}
				_, err := testDB.Exec(`UPDATE foundry_ontologies SET current_load_id=NULL WHERE id=?`, f.ontology.ID)
				require.NoError(t, err)
			})
			run := &model.FoundryHealRun{ID: id26("HR-" + tag), OntologyID: f.ontology.ID, LoadID: load}
			require.NoError(t, f.db.StartFoundryHealRun(ctx, run, time.Minute))
			err := f.db.PublishFoundryHealRun(ctx, model.FoundryHealPublish{RunID: run.ID, OntologyID: f.ontology.ID, LoadID: load, VersionID: v.ID,
				Pages: 2, Findings: seen})
			if c.want == nil {
				require.NoError(t, err)
				assert.Len(t, f.healRows(t), len(seen))
				latest, err := f.db.GetLatestFoundryHealRun(ctx, f.ontology.ID)
				require.NoError(t, err)
				assert.Equal(t, model.FoundryHealRunComplete, latest.Status)
				return
			}
			assert.ErrorIs(t, err, c.want)
			assert.Len(t, f.healRows(t), store.HealPublishBatch, "the first batch alone")
		})
	}
}

// Rows unmeasured, an earlier load's row findings are superseded: nothing
// says they still hold. A superseded finding keeps no examples.
func TestFoundryHealSupersedeEarlierRows(t *testing.T) {
	f := newFoundryFixture(t, "HSER")
	v1 := f.currentLoad(t, "HSER-1", customerTOML)
	withExamples := func(seen model.FoundryHealSeen, path ...string) model.FoundryHealSeen {
		var finding map[string]any
		require.NoError(t, json.Unmarshal(seen.Finding, &finding))
		at := finding["signal"].(map[string]any)[path[0]].(map[string]any)
		at["examples"] = []any{map[string]any{"doc_id": "d1", "quote": "a verbatim quote"}}
		seen.Finding, _ = json.Marshal(finding)
		return seen
	}
	f.healRun(t, "r1", id26("FJ-HSER-1"), v1.ID, true, withExamples(rowSeen("cc", 3), "errors"), withExamples(unmatchedSeen("aa", 4, 2), "backing"))
	v2 := f.currentLoad(t, "HSER-2", customerTOML)
	f.healRun(t, "r2", id26("FJ-HSER-2"), v2.ID, false)
	rows := f.healRows(t)
	for _, fp := range []string{fingerprint("cc"), fingerprint("aa")} {
		assert.Equal(t, model.FoundryHealSuperseded, rows[fp].Status)
		assert.NotContains(t, string(rows[fp].Finding), "a verbatim quote")
		assert.NotContains(t, string(rows[fp].Finding), "examples")
	}
	// The load's own row findings stand while a later run of it leaves the
	// rows unmeasured.
	f.healRun(t, "r3", id26("FJ-HSER-2"), v2.ID, true, rowSeen("dd", 2))
	f.healRun(t, "r4", id26("FJ-HSER-2"), v2.ID, false)
	assert.Equal(t, model.FoundryHealOpen, f.healRows(t)[fingerprint("dd")].Status)
}

// A complete run records whether it measured the load's rows; an
// incomplete one never says it did.
func TestFoundryHealRowsMeasured(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "HRMS")
	v := f.currentLoad(t, "HRMS-1", customerTOML)
	load := id26("FJ-HRMS-1")
	latest := func() *model.FoundryHealRun {
		t.Helper()
		run, err := f.db.GetLatestFoundryHealRun(ctx, f.ontology.ID)
		require.NoError(t, err)
		return run
	}
	f.healRun(t, "m1", load, v.ID, true, rowSeen("cc", 3))
	assert.True(t, latest().RowsMeasured, "measured")
	f.healRun(t, "m2", load, v.ID, false)
	assert.False(t, latest().RowsMeasured, "unmeasured")
	run := &model.FoundryHealRun{ID: id26("HR-m3"), OntologyID: f.ontology.ID, LoadID: load}
	require.NoError(t, f.db.StartFoundryHealRun(ctx, run, time.Minute))
	require.NoError(t, f.db.FinishFoundryHealRun(ctx, run.ID, model.FoundryHealRunIncomplete, 1, 1))
	assert.False(t, latest().RowsMeasured, "an incomplete run")
	// The newest complete run is m2's, behind the incomplete one.
	complete, err := f.db.GetLatestCompleteFoundryHealRun(ctx, f.ontology.ID)
	require.NoError(t, err)
	assert.Equal(t, id26("HR-m2"), complete.ID)
	assert.Equal(t, model.FoundryHealRunComplete, complete.Status)
	_, err = f.db.GetLatestCompleteFoundryHealRun(ctx, id26("no-ontology"))
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// An ontology keeps its FoundryHealRunsKept newest runs.
func TestFoundryHealRunsPruned(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "HPRN")
	v := f.currentLoad(t, "HPRN-1", customerTOML)
	load := id26("FJ-HPRN-1")
	for i := range model.FoundryHealRunsKept + 3 {
		f.healRun(t, fmt.Sprintf("n%02d", i), load, v.ID, true)
	}
	var n int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_heal_runs WHERE ontology_id=?`, f.ontology.ID).Scan(&n))
	assert.Equal(t, model.FoundryHealRunsKept, n)
	latest, err := f.db.GetLatestFoundryHealRun(ctx, f.ontology.ID)
	require.NoError(t, err)
	assert.Equal(t, id26(fmt.Sprintf("HR-n%02d", model.FoundryHealRunsKept+2)), latest.ID)
}

// Deleting an ontology deletes its findings, with their quotes, and its
// runs: a run in flight then loses its lease.
func TestFoundryHealOntologyDeleted(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "HDEL")
	v := f.currentLoad(t, "HDEL-1", customerTOML)
	load := id26("FJ-HDEL-1")
	f.healRun(t, "x1", load, v.ID, true, unmatchedSeen("aa", 4, 2))
	running := &model.FoundryHealRun{ID: id26("HR-x2"), OntologyID: f.ontology.ID, LoadID: load}
	require.NoError(t, f.db.StartFoundryHealRun(ctx, running, time.Minute))
	require.NoError(t, f.db.DeleteFoundryOntology(ctx, f.ontology.WorkspaceID, f.ontology.ID, id26("FJ-HDEL-del")))
	for _, table := range []string{"foundry_heal_findings", "foundry_heal_runs"} {
		var n int
		require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE ontology_id=?`, f.ontology.ID).Scan(&n))
		assert.Zero(t, n, table)
	}
	assert.ErrorIs(t, f.db.RenewFoundryHealRun(ctx, running.ID, time.Minute), store.ErrNotFound)
}

// A publish stopped between batches keeps the batches it committed and
// leaves the run running, nothing superseded, for its worker to fail; the
// failed run then waits retry_after.
func TestFoundryHealPublishStopped(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "HPST")
	v := f.currentLoad(t, "HPST-1", customerTOML)
	load := id26("FJ-HPST-1")
	f.healRun(t, "s0", load, v.ID, true, unmatchedSeen("zz", 4, 2))
	var seen []model.FoundryHealSeen
	for i := range store.HealPublishBatch + 10 {
		seen = append(seen, unmatchedSeen(fmt.Sprintf("b%03d", i), 4, 2))
	}
	publishCtx, cancel := context.WithCancel(ctx)
	store.SetAfterHealPublishBatch(t, cancel)
	run := &model.FoundryHealRun{ID: id26("HR-s1"), OntologyID: f.ontology.ID, LoadID: load}
	require.NoError(t, f.db.StartFoundryHealRun(ctx, run, time.Minute))
	err := f.db.PublishFoundryHealRun(publishCtx, model.FoundryHealPublish{RunID: run.ID, OntologyID: f.ontology.ID, LoadID: load, VersionID: v.ID,
		Pages: 2, Findings: seen})
	require.ErrorIs(t, err, context.Canceled)
	rows := f.healRows(t)
	assert.Len(t, rows, store.HealPublishBatch+1, "the first batch, and the earlier finding")
	assert.Equal(t, model.FoundryHealOpen, rows[fingerprint("zz")].Status, "nothing superseded")
	latest, err := f.db.GetLatestFoundryHealRun(ctx, f.ontology.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryHealRunning, latest.Status)
	require.NoError(t, f.db.FinishFoundryHealRun(ctx, run.ID, model.FoundryHealRunFailed, 2, 0))
	due, err := f.db.DueFoundryHealOntologies(ctx, 24*time.Hour, time.Hour, 10)
	require.NoError(t, err)
	for _, d := range due {
		assert.NotEqual(t, load, d.Load.ID, "a failed run waits retry_after")
	}
}
