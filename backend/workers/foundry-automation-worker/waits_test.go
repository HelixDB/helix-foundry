package foundryautomationworker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// refresh syncs Stripe, loads the Sales graph's current version, then
// publishes its dashboard again.
var refresh = spec(`[{"from":"start","to":"sync"},{"from":"sync","to":"load"},{"from":"load","to":"pub"}]`,
	node("start", "trigger", `{"kind":"manual"}`),
	node("sync", "action", `{"operation":"sync_connector","connector_id":"stripe"}`),
	node("load", "action", `{"operation":"create_load","ontology_id":"ont1"}`),
	node("pub", "action", `{"operation":"publish_dashboard","dashboard_id":"dash1"}`))

// publishing is a fixture whose owner may publish dashboard dash1, of the
// Sales graph (ont1), whose current version is ver7.
func publishing(t *testing.T) fixture {
	f := newFixture(t)
	f.host.service = (&integration.Service{Foundry: dashboardReads{}, Actions: f.actions, Confirmations: &confirmations{}, PublicDashboards: true}).ForAgent()
	current := "ver7"
	f.db.ontologies["ont1"] = &model.FoundryOntology{ID: "ont1", WorkspaceID: ws, Name: "Sales", CurrentVersionID: &current}
	f.db.dashboards["dash1"] = &model.FoundryDashboard{ID: "dash1", WorkspaceID: ws, Name: "Pipeline", Revision: 4,
		Share: &model.FoundryDashboardShare{SpecRevision: 4}}
	return f
}

// A sync, then a load, then a publish: each step waits for the work the one
// before started, so the dashboard is published from the load that's ready.
func TestASyncThenALoadThenAPublishWaitsForEach(t *testing.T) {
	f := publishing(t)
	run := f.queue(refresh)
	ctx := context.Background()
	f.task.advance(ctx, run)
	assert.Equal(t, 1, f.actions.syncs)
	assert.Equal(t, "Waiting for the sync…", f.output(t, run.ID, "sync").Summary)
	assert.Zero(t, f.actions.loads, "the load waits for the sync")

	// Still running, it waits on, and nothing after it starts.
	f.db.syncs["sync1"].Status = model.ConnectorSyncRunning
	f.task.advance(ctx, run)
	assert.Equal(t, model.FoundryAutomationNodeRunning, f.status(run.ID, "sync"))
	assert.Zero(t, f.actions.loads)

	f.endSync("sync1", model.ConnectorSyncSucceeded, 42)
	f.task.advance(ctx, run)
	assert.Equal(t, "Synced its connector: it wrote 42 rows.", f.output(t, run.ID, "sync").Summary)
	assert.Equal(t, 1, f.actions.loads, "the load starts once the sync succeeded")
	assert.Equal(t, model.FoundryAutomationNodeRunning, f.status(run.ID, "load"))
	assert.Equal(t, "Waiting for the load…", f.output(t, run.ID, "load").Summary)
	assert.Equal(t, ptr("load1"), f.db.nodes[run.ID]["load"].JobID)
	assert.Empty(t, f.actions.published, "nothing is published from a load still writing")

	f.task.advance(ctx, run)
	assert.Empty(t, f.actions.published)
	assert.NotContains(t, f.db.finished, run.ID)

	f.endLoad("load1", model.FoundryLoadReady)
	f.task.advance(ctx, run)
	assert.Equal(t, "Loaded its ontology's current version: the load is ready.", f.output(t, run.ID, "load").Summary)
	assert.Equal(t, []int64{4}, f.actions.published, "published once the load is ready")
	assert.Equal(t, model.FoundryAutomationRunSucceeded, f.db.finished[run.ID].Status)
	assert.Equal(t, 1, f.actions.syncs, "nothing ran twice")
	assert.Equal(t, 1, f.actions.loads)

	// The work is marked as the run's.
	assert.Equal(t, &model.FoundryAutomationCause{RunID: run.ID, AutomationID: "auto1", Depth: 1}, f.db.causes["sync1"])
	assert.Equal(t, &model.FoundryAutomationCause{RunID: run.ID, AutomationID: "auto1", Depth: 1}, f.db.causes["load1"])
}

// The step's outcome is its work's: a load or a sync that failed fails it,
// saying why, and the run with it.
func TestFailedWorkFailsItsStepAndTheRun(t *testing.T) {
	f := publishing(t)
	run := f.queue(refresh)
	ctx := context.Background()
	f.task.advance(ctx, run)
	f.endSync("sync1", model.ConnectorSyncSucceeded, 0)
	f.task.advance(ctx, run)
	why := "the workspace has spent this month's $20.00 of loads"
	f.db.loadJobs["load1"] = model.FoundryLoadJobs{Newest: []*model.FoundryGraphJob{{ID: "load1", Error: &why}}, Count: 1}
	f.endLoad("load1", model.FoundryLoadFailed)
	f.task.advance(ctx, run)
	assert.Equal(t, "The load failed: The workspace has spent this month's $20.00 of loads.", *f.db.nodes[run.ID]["load"].Error)
	assert.Equal(t, model.FoundryAutomationNodeBlocked, f.status(run.ID, "pub"))
	assert.Empty(t, f.actions.published)
	end := f.db.finished[run.ID]
	assert.Equal(t, model.FoundryAutomationRunFailed, end.Status)
	assert.Equal(t, `The action step "load" failed: The load failed: The workspace has spent this month's $20.00 of loads.`, end.Error)

	// A load whose jobs say nothing, a sync, and work gone or deleted.
	for name, c := range map[string]struct {
		end  func(f fixture)
		node string
		want string
	}{
		"a load that says nothing":      {func(f fixture) { f.endLoad("load1", model.FoundryLoadFailed) }, "load", "The load failed."},
		"a load deleted":                {func(f fixture) { f.endLoad("load1", model.FoundryLoadDeleted) }, "load", "Its load was deleted before the step saw it end."},
		"a load gone":                   {func(f fixture) { delete(f.db.loads, "load1") }, "load", "Its load is gone."},
		"a sync that failed":            {nil, "sync", "The sync failed: The connector's password was refused."},
		"a sync that failed, no reason": {nil, "sync", "The sync failed."},
		"a sync gone":                   {nil, "sync", "Its sync is gone."},
	} {
		f := publishing(t)
		run := f.queue(refresh)
		f.task.advance(ctx, run)
		switch {
		case c.node == "load":
			f.endSync("sync1", model.ConnectorSyncSucceeded, 0)
			f.task.advance(ctx, run)
			c.end(f)
		case name == "a sync gone":
			delete(f.db.syncs, "sync1")
		default:
			f.db.syncs["sync1"].Status = model.ConnectorSyncFailed
			if name == "a sync that failed" {
				f.db.syncs["sync1"].Error = ptr("the connector's password was refused")
			}
		}
		f.task.advance(ctx, run)
		assert.Equal(t, model.FoundryAutomationNodeFailed, f.status(run.ID, c.node), name)
		assert.Equal(t, c.want, *f.db.nodes[run.ID][c.node].Error, name)
		assert.Equal(t, model.FoundryAutomationRunFailed, f.db.finished[run.ID].Status, name)
		assert.Empty(t, f.actions.published, name)
	}
}

// A worker that stopped while a step waited leaves it waiting on its work:
// the run picked up again, by another worker, waits on, never starting a
// second load.
func TestAWorkerRestartResumesTheWait(t *testing.T) {
	f := publishing(t)
	run := f.queue(spec(`[{"from":"start","to":"load"},{"from":"load","to":"pub"}]`, node("start", "trigger", `{"kind":"manual"}`),
		node("load", "action", `{"operation":"create_load","ontology_id":"ont1"}`),
		node("pub", "action", `{"operation":"publish_dashboard","dashboard_id":"dash1"}`)))
	ctx := context.Background()
	f.task.advance(ctx, run)
	require.Equal(t, 1, f.actions.loads)

	// Another worker, with nothing of the first's in memory.
	restarted := &Task{cfg: f.task.cfg, db: f.db, sessions: f.host, assistant: true, email: true, workerID: "w2", now: f.task.now}
	run.Attempts++
	restarted.advance(ctx, run)
	assert.Equal(t, 1, f.actions.loads, "it waits on the load it started")
	assert.Equal(t, model.FoundryAutomationNodeRunning, f.status(run.ID, "load"))
	assert.Contains(t, f.db.yielded, run.ID)

	f.endLoad("load1", model.FoundryLoadReady)
	restarted.advance(ctx, run)
	assert.Equal(t, 1, f.actions.loads, "never a second load")
	assert.Equal(t, []int64{4}, f.actions.published)
	assert.Equal(t, model.FoundryAutomationRunSucceeded, f.db.finished[run.ID].Status)
	assert.Equal(t, "Loaded its ontology's current version: the load is ready.", f.output(t, run.ID, "load").Summary)
	assert.Contains(t, string(f.output(t, run.ID, "load").Result), `"id":"load1"`, "what the load's call returned, kept across the restart")
}

// A step waits for its work for so long, reading it less often as it takes
// longer, then fails saying so; the work goes on.
func TestAWaitTimesOut(t *testing.T) {
	f := publishing(t)
	run := f.queue(spec(`[{"from":"start","to":"load"},{"from":"load","to":"pub"}]`, node("start", "trigger", `{"kind":"manual"}`),
		node("load", "action", `{"operation":"create_load","ontology_id":"ont1"}`),
		node("pub", "action", `{"operation":"publish_dashboard","dashboard_id":"dash1"}`)))
	ctx := context.Background()
	started := f.task.now()
	f.task.advance(ctx, run)
	assert.Equal(t, started.Add(f.task.cfg.StepWait), f.db.yielded[run.ID], "read again soon after it starts")

	for took, wait := range map[time.Duration]time.Duration{
		10 * time.Second:           f.task.cfg.StepWait,
		5 * time.Minute:            30 * time.Second,
		3 * time.Hour:              jobPollMax,
		loadWait - time.Nanosecond: jobPollMax,
	} {
		now := started.Add(took)
		f.task.now = func() time.Time { return now }
		f.task.advance(ctx, run)
		assert.Equal(t, now.Add(wait), f.db.yielded[run.ID], "after %s", took)
		assert.Equal(t, model.FoundryAutomationNodeRunning, f.status(run.ID, "load"))
	}

	f.task.now = func() time.Time { return started.Add(loadWait) }
	f.task.advance(ctx, run)
	assert.Equal(t, "Its load didn't end within 6 hours, so the step stopped waiting; the load may still finish.", *f.db.nodes[run.ID]["load"].Error)
	assert.Equal(t, model.FoundryAutomationRunFailed, f.db.finished[run.ID].Status)
	assert.Empty(t, f.actions.published)
	assert.Equal(t, 1, f.actions.loads)

	// A sync waits an hour.
	f = publishing(t)
	run = f.queue(refresh)
	f.task.advance(ctx, run)
	f.task.now = func() time.Time { return started.Add(syncWait - time.Second) }
	f.task.advance(ctx, run)
	assert.Equal(t, model.FoundryAutomationNodeRunning, f.status(run.ID, "sync"))
	f.task.now = func() time.Time { return started.Add(syncWait) }
	f.task.advance(ctx, run)
	assert.Equal(t, "Its sync didn't end within an hour, so the step stopped waiting; the sync may still finish.", *f.db.nodes[run.ID]["sync"].Error)
	assert.Zero(t, f.actions.loads)
}

// An action whose answer names no work fails its step: it can't be waited
// for. One whose step was ended meanwhile is no longer this attempt's.
func TestAnActionWhoseWorkCantBeToldFails(t *testing.T) {
	f := publishing(t)
	f.actions.noID = true
	run := f.queue(refresh)
	f.task.advance(context.Background(), run)
	assert.Equal(t, "It started its sync, and Foundry can't tell which one to wait for.", *f.db.nodes[run.ID]["sync"].Error)
	assert.Equal(t, model.FoundryAutomationRunFailed, f.db.finished[run.ID].Status)

	f = publishing(t)
	run = f.queue(refresh)
	f.db.lost = false
	f.actions.onSync = func() { f.db.lost = true }
	f.task.advance(context.Background(), run)
	assert.NotContains(t, f.db.finished, run.ID, "the lease lost, nothing more is recorded")
	assert.NotContains(t, f.db.retried, run.ID)
	assert.Equal(t, model.FoundryAutomationNodeExecuting, f.status(run.ID, "sync"), "found so, it ends unknown")
}
