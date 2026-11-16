package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// causeOf reads what a sync's or a load's row says started it.
func causeOf(t *testing.T, table, id string) (runID *string, depth *int) {
	t.Helper()
	require.NoError(t, testDB.QueryRow(`SELECT started_by_automation_run_id, automation_chain_depth FROM `+table+` WHERE id=?`, id).Scan(&runID, &depth))
	return runID, depth
}

// An action step that started a sync or a load waits for it: the step runs
// on with the work's id, and the work names the run and how deep a chain of
// automations led to it, one more than the run's own.
func TestFoundryAutomationActionAwaitsItsJob(t *testing.T) {
	f := newAutomationFixture(t, "await")
	ctx := context.Background()
	f.active(t, 1, "manual", nil)
	run := f.run(1, 1, "manual:a")
	run.TriggerEvent = json.RawMessage(`{"kind": "load_ready", "caused_by_automations": ["AUTO-B"], "chain_depth": 1}`)
	_, err := f.db.RunFoundryAutomationNow(ctx, run, "start", nil, automationLimits)
	require.NoError(t, err)
	claimed, err := f.db.ClaimFoundryAutomationRun(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.Equal(t, run.ID, claimed.ID)

	load, sync, foreign := id26("AWLOAD"), id26("AWSYNC"), id26("AWFOREIGN")
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO foundry_loads (id, workspace_id, ontology_id, version_id, status) VALUES (?, ?, ?, 'V', 'writing')`, []any{load, f.workspaceID, id26("AWONT")}},
		{`INSERT INTO connector_syncs (id, connector_id, workspace_id, status) VALUES (?, ?, ?, 'queued')`, []any{sync, id26("AWCON"), f.workspaceID}},
		{`INSERT INTO connector_syncs (id, connector_id, workspace_id, status) VALUES (?, ?, ?, 'queued')`, []any{foreign, id26("AWCON"), id26("AWOTHERWS")}},
	} {
		_, err := testDB.Exec(q.sql, q.args...)
		require.NoError(t, err)
	}
	job := model.FoundryAutomationJob{Kind: model.FoundryAutomationJobLoad, ID: load}
	waiting := json.RawMessage(`{"summary": "Waiting for the load…"}`)

	require.ErrorIs(t, f.db.AwaitFoundryAutomationJob(ctx, run.ID, "w1", "load", job, waiting), store.ErrConflict, "the step isn't executing")
	require.NoError(t, f.db.StartFoundryAutomationNode(ctx, run.ID, "w1", "load", model.FoundryAutomationNodeExecuting))
	require.ErrorIs(t, f.db.AwaitFoundryAutomationJob(ctx, run.ID, "w2", "load", job, waiting), store.ErrNotFound, "only its lease holder writes it")
	require.NoError(t, f.db.AwaitFoundryAutomationJob(ctx, run.ID, "w1", "load", job, waiting))
	require.ErrorIs(t, f.db.AwaitFoundryAutomationJob(ctx, run.ID, "w1", "load", job, waiting), store.ErrConflict, "it waits already")

	nodes, err := f.db.ListFoundryAutomationRunNodes(ctx, run.ID)
	require.NoError(t, err)
	var step *model.FoundryAutomationRunNode
	for _, n := range nodes {
		if n.NodeID == "load" {
			step = n
		}
	}
	require.NotNil(t, step)
	require.Equal(t, model.FoundryAutomationNodeRunning, step.Status)
	require.Equal(t, &load, step.JobID)
	require.JSONEq(t, string(waiting), string(step.Output))
	require.Nil(t, step.FinishedAt)
	runID, depth := causeOf(t, "foundry_loads", load)
	require.Equal(t, &run.ID, runID)
	require.Equal(t, ptr(2), depth, "one more than the run's chain")

	// An Assistant step's sync is marked the same way, once; another
	// workspace's work never is.
	require.NoError(t, f.db.MarkFoundryAutomationCause(ctx, run.ID, model.FoundryAutomationJob{Kind: model.FoundryAutomationJobSync, ID: sync}))
	require.NoError(t, f.db.MarkFoundryAutomationCause(ctx, run.ID, model.FoundryAutomationJob{Kind: model.FoundryAutomationJobSync, ID: foreign}))
	runID, depth = causeOf(t, "connector_syncs", sync)
	require.Equal(t, &run.ID, runID)
	require.Equal(t, ptr(2), depth)
	runID, depth = causeOf(t, "connector_syncs", foreign)
	require.Nil(t, runID)
	require.Nil(t, depth)

	// A run with no chain in its event starts work one deep, and work marked
	// already keeps its cause.
	f.active(t, 2, "manual", nil)
	_, err = f.db.RunFoundryAutomationNow(ctx, f.run(2, 2, "manual:b"), "start", nil, automationLimits)
	require.NoError(t, err)
	require.NoError(t, f.db.MarkFoundryAutomationCause(ctx, f.runID(2), model.FoundryAutomationJob{Kind: model.FoundryAutomationJobSync, ID: sync}))
	runID, _ = causeOf(t, "connector_syncs", sync)
	require.Equal(t, &run.ID, runID, "the first cause stays")
	fresh := id26("AWFRESH")
	_, err = testDB.Exec(`INSERT INTO connector_syncs (id, connector_id, workspace_id, status) VALUES (?, ?, ?, 'queued')`, fresh, id26("AWCON2"), f.workspaceID)
	require.NoError(t, err)
	require.NoError(t, f.db.MarkFoundryAutomationCause(ctx, f.runID(2), model.FoundryAutomationJob{Kind: model.FoundryAutomationJobSync, ID: fresh}))
	_, depth = causeOf(t, "connector_syncs", fresh)
	require.Equal(t, ptr(1), depth)

	require.Error(t, f.db.MarkFoundryAutomationCause(ctx, run.ID, model.FoundryAutomationJob{Kind: "generation", ID: load}))
}

// An event trigger reads who started each sync or load it finds: the run,
// its automation and the chain that set it off; a person's has none, and a
// run deleted since leaves the depth.
func TestFoundryAutomationFindEndedCauses(t *testing.T) {
	f := newAutomationFixture(t, "causes")
	ctx := context.Background()
	ontology, connector := id26("CAONT"), id26("CACON")
	f.active(t, 1, "manual", nil)
	run := f.run(1, 1, "manual:a")
	run.TriggerEvent = json.RawMessage(`{"kind": "sync_finished", "caused_by_automations": ["AUTO-B", "AUTO-C"], "chain_depth": 2}`)
	_, err := f.db.RunFoundryAutomationNow(ctx, run, "start", nil, automationLimits)
	require.NoError(t, err)

	watch := func(n int, kind, target string) *model.FoundryAutomation {
		a := f.active(t, n, kind, &target)
		_, err := testDB.Exec(`UPDATE foundry_automations SET cursor_at = cursor_at - INTERVAL 1 HOUR WHERE id=?`, a.ID)
		require.NoError(t, err)
		a, err = f.db.GetFoundryAutomation(ctx, f.workspaceID, a.ID)
		require.NoError(t, err)
		return a
	}
	loads := watch(2, "load_ready", ontology)
	at := loads.Cursor.At
	for i, row := range []struct {
		id    string
		run   any
		depth any
	}{{id26("CAPERSON"), nil, nil}, {id26("CAOWN"), run.ID, 3}, {id26("CAGONE"), id26("CAGONERUN"), 4}} {
		_, err := testDB.Exec(`INSERT INTO foundry_loads (id, workspace_id, ontology_id, version_id, status, created_at, finished_at, started_by_automation_run_id, automation_chain_depth)
VALUES (?, ?, ?, 'V', 'ready', ?, ?, ?, ?)`, row.id, f.workspaceID, ontology, at, at.Add(time.Duration(i+1)*time.Second), row.run, row.depth)
		require.NoError(t, err)
	}
	ended, err := f.db.FindFoundryAutomationEnded(ctx, loads, 0, 10)
	require.NoError(t, err)
	require.Len(t, ended, 3)
	require.Nil(t, ended[0].Cause, "a person's load")
	require.Equal(t, &model.FoundryAutomationCause{RunID: run.ID, AutomationID: f.id(1), Chain: []string{"AUTO-B", "AUTO-C"}, Depth: 3}, ended[1].Cause)
	require.Equal(t, &model.FoundryAutomationCause{RunID: id26("CAGONERUN"), Depth: 4}, ended[2].Cause, "its run is gone")

	syncs := watch(3, "sync_finished", connector)
	_, err = testDB.Exec(`INSERT INTO connector_syncs (id, connector_id, workspace_id, status, created_at, finished_at, started_by_automation_run_id, automation_chain_depth)
VALUES (?, ?, ?, 'succeeded', ?, ?, ?, 1)`, id26("CASYNC"), connector, f.workspaceID, syncs.Cursor.At, syncs.Cursor.At.Add(time.Second), run.ID)
	require.NoError(t, err)
	ended, err = f.db.FindFoundryAutomationEnded(ctx, syncs, 0, 10)
	require.NoError(t, err)
	require.Len(t, ended, 1)
	require.Equal(t, f.id(1), ended[0].Cause.AutomationID)
	require.Equal(t, 1, ended[0].Cause.Depth)
}

// Work that would loop records its run skipped, saying why, even while a run
// of the automation is live; delivered again, it moves the cursor alone.
func TestFoundryAutomationFireSkipsLoops(t *testing.T) {
	f := newAutomationFixture(t, "loops")
	ctx := context.Background()
	ontology := id26("LOONT")
	a := f.active(t, 1, "load_ready", &ontology)
	fire := func(n int, to model.FoundryAutomationCursor, skip string) (model.FoundryAutomationFired, error) {
		a, err := f.db.GetFoundryAutomation(ctx, f.workspaceID, a.ID)
		require.NoError(t, err)
		return f.db.FireFoundryAutomation(ctx, model.FoundryAutomationFire{AutomationID: a.ID, WorkspaceID: f.workspaceID,
			Advance: model.FoundryCursorAdvance{From: *a.Cursor, To: to}, Run: f.run(n, 1, "load_ready:"+to.ID), TriggerNodeID: "start", Skip: skip},
			automationLimits)
	}
	start := *a.Cursor
	first := model.FoundryAutomationCursor{At: start.At.Add(time.Second), ID: id26("LOAD1")}
	fired, err := fire(1, first, "")
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationFiredQueued, fired)

	own := model.FoundryAutomationCursor{At: first.At.Add(time.Second), ID: id26("LOAD2")}
	why := "Skipped: the load was started by this automation itself; running on it would loop."
	fired, err = fire(2, own, why)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationFiredSkipped, fired, "a live run doesn't hold it")
	skipped, err := f.db.GetFoundryAutomationRun(ctx, f.workspaceID, f.runID(2))
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationRunSkipped, skipped.Status)
	require.Equal(t, why, *skipped.Error)
	require.Nil(t, skipped.Spec, "a skipped run runs nothing")
	a, err = f.db.GetFoundryAutomation(ctx, f.workspaceID, a.ID)
	require.NoError(t, err)
	require.Equal(t, own, *a.Cursor, "the cursor passes it")

	// Skipped runs never use up the day's.
	_, err = f.db.CancelFoundryAutomationRun(ctx, f.workspaceID, f.runID(1), model.FoundryAutomationActor{UserID: automationOwner})
	require.NoError(t, err)
	for i := range 3 {
		next := model.FoundryAutomationCursor{At: own.At.Add(time.Duration(i+1) * time.Second), ID: id26("LOADSKIP" + string(rune('A'+i)))}
		fired, err = fire(10+i, next, why)
		require.NoError(t, err)
		require.Equal(t, model.FoundryAutomationFiredSkipped, fired)
	}
	room := model.FoundryAutomationCursor{At: own.At.Add(10 * time.Second), ID: id26("LOAD3")}
	fired, err = fire(3, room, "")
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationFiredQueued, fired)

	again := model.FoundryAutomationCursor{At: room.At.Add(time.Second), ID: id26("LOAD2")}
	fired, err = fire(4, again, why)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationFiredMoved, fired, "load_ready:LOAD2 was delivered before")

	_, err = f.db.FireFoundryAutomation(ctx, model.FoundryAutomationFire{AutomationID: a.ID, WorkspaceID: f.workspaceID,
		Advance: model.FoundryCursorAdvance{From: start, To: first}, Skip: why}, automationLimits)
	require.Error(t, err, "a skip records a run")
}
