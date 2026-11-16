package foundryautomationworker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// watching is automation id, owned by the owner, of spec, its event
// trigger's cursor at cursor.
func (f fixture) watching(id string, raw json.RawMessage, cursor model.FoundryAutomationCursor) *model.FoundryAutomation {
	a := &model.FoundryAutomation{ID: id, WorkspaceID: ws, OwnerUserID: owner, Name: id, Spec: raw, Cursor: &cursor}
	f.db.automations[id] = a
	return a
}

// started stores the run fire queued, as the store does: its spec the
// automation's, its trigger done.
func (f fixture) started(fire model.FoundryAutomationFire) *model.FoundryAutomationRun {
	run := *fire.Run
	run.Spec, run.Attempts, run.Status = f.db.automations[fire.AutomationID].Spec, 1, model.FoundryAutomationRunRunning
	f.db.runs[run.ID] = &run
	f.db.nodes[run.ID] = map[string]*model.FoundryAutomationRunNode{
		fire.TriggerNodeID: {RunID: run.ID, NodeID: fire.TriggerNodeID, Status: model.FoundryAutomationNodeSucceeded, Output: fire.TriggerOutput}}
	return &run
}

// lastFire is the fire the latest watch made.
func (f fixture) lastFire(t *testing.T) model.FoundryAutomationFire {
	t.Helper()
	require.NotEmpty(t, f.db.fires)
	return f.db.fires[len(f.db.fires)-1]
}

// reloads loads the Sales graph whenever one of its loads is ready.
var reloads = spec(`[{"from":"start","to":"load"}]`, node("start", "trigger", `{"kind":"load_ready","ontology_id":"ont1"}`),
	node("load", "action", `{"operation":"create_load","ontology_id":"ont1"}`))

// An automation that loads the ontology whose loads fire it: a person's load
// fires it once, and the load its own run started is skipped, saying why,
// instead of firing it again after every run.
func TestAnAutomationNeverFiresOnItsOwnWork(t *testing.T) {
	f := newFixture(t)
	current := "ver7"
	f.db.ontologies["ont1"] = &model.FoundryOntology{ID: "ont1", WorkspaceID: ws, CurrentVersionID: &current}
	cursor := model.FoundryAutomationCursor{At: f.task.now().Add(-time.Hour)}
	a := f.watching("auto1", reloads, cursor)
	f.db.watched = []*model.FoundryAutomation{a}
	ctx := context.Background()

	// A person's load fires it.
	f.db.ended = []model.FoundryAutomationEnded{f.db.endedWork("person-load", "ready", cursor.At.Add(time.Minute))}
	f.task.watch(ctx)
	fire := f.lastFire(t)
	require.NotNil(t, fire.Run)
	assert.Empty(t, fire.Skip, "a person's load fires it")
	assert.Equal(t, "load_ready:person-load", fire.Run.TriggerKey)
	run := f.started(fire)
	f.task.advance(ctx, run)
	f.endLoad("load1", model.FoundryLoadReady)
	f.task.advance(ctx, run)
	require.Equal(t, model.FoundryAutomationRunSucceeded, f.db.finished[run.ID].Status)
	require.Equal(t, 1, f.actions.loads)

	// The load its run started ends: skipped, never run.
	cursor = fire.Advance.(model.FoundryCursorAdvance).To
	a.Cursor = &cursor
	f.db.ended = []model.FoundryAutomationEnded{f.db.endedWork("load1", "ready", cursor.At.Add(time.Minute))}
	f.task.watch(ctx)
	fire = f.lastFire(t)
	require.NotNil(t, fire.Run, "recorded, so its owner sees why it didn't run")
	assert.Equal(t, "Skipped: the load was started by this automation itself; running on it would loop.", fire.Skip)
	assert.Equal(t, "load_ready:load1", fire.Run.TriggerKey)
	assert.Equal(t, "load1", fire.Advance.(model.FoundryCursorAdvance).To.ID, "the cursor passes it")

	// A person's load beside its own fires it, named by the person's.
	f.db.ended = []model.FoundryAutomationEnded{f.db.endedWork("load1", "ready", cursor.At.Add(time.Minute)),
		f.db.endedWork("person-load-2", "ready", cursor.At.Add(2*time.Minute))}
	f.task.watch(ctx)
	fire = f.lastFire(t)
	assert.Empty(t, fire.Skip)
	assert.Equal(t, "load_ready:person-load-2", fire.Run.TriggerKey)
	var event model.FoundryAutomationEvent
	require.NoError(t, json.Unmarshal(fire.Run.TriggerEvent, &event))
	assert.Equal(t, int64(1), event.Count, "its own load isn't counted")
	assert.Empty(t, event.CausedBy, "a person set it off")
	assert.Zero(t, event.ChainDepth)
}

// A sync_finished automation that syncs its own connector skips the sync its
// run started, as a load_ready one does.
func TestASyncThatSyncsItsOwnConnectorIsSkipped(t *testing.T) {
	f := newFixture(t)
	cursor := model.FoundryAutomationCursor{At: f.task.now().Add(-time.Hour)}
	a := f.watching("auto1", spec(`[{"from":"start","to":"sync"}]`,
		node("start", "trigger", `{"kind":"sync_finished","connector_id":"stripe","outcome":"any"}`),
		node("sync", "action", `{"operation":"sync_connector","connector_id":"stripe"}`)), cursor)
	f.db.watched = []*model.FoundryAutomation{a}
	f.db.causes["sync9"] = &model.FoundryAutomationCause{RunID: "run0", AutomationID: "auto1", Depth: 1}
	f.db.ended = []model.FoundryAutomationEnded{f.db.endedWork("sync9", "failed", cursor.At.Add(time.Minute))}
	f.task.watch(context.Background())
	assert.Equal(t, "Skipped: the sync was started by this automation itself; running on it would loop.", f.lastFire(t).Skip)
}

// A's sync fires B, whose load would fire A again: the work B's run started
// is skipped by A, which set B off. The chain stops with no daily cap.
func TestAChainBackToItsStartIsSkipped(t *testing.T) {
	f := newFixture(t)
	current := "ver7"
	f.db.ontologies["ont1"] = &model.FoundryOntology{ID: "ont1", WorkspaceID: ws, CurrentVersionID: &current}
	cursor := model.FoundryAutomationCursor{At: f.task.now().Add(-time.Hour)}
	// A syncs Stripe once the Sales graph loads; B loads the Sales graph once Stripe syncs.
	a := f.watching("autoA", spec(`[{"from":"start","to":"sync"}]`, node("start", "trigger", `{"kind":"load_ready","ontology_id":"ont1"}`),
		node("sync", "action", `{"operation":"sync_connector","connector_id":"stripe"}`)), cursor)
	b := f.watching("autoB", spec(`[{"from":"start","to":"load"}]`,
		node("start", "trigger", `{"kind":"sync_finished","connector_id":"stripe","outcome":"succeeded"}`),
		node("load", "action", `{"operation":"create_load","ontology_id":"ont1"}`)), cursor)
	ctx := context.Background()
	at := cursor.At

	// A person's load fires A.
	f.db.watched = []*model.FoundryAutomation{a}
	at = at.Add(time.Minute)
	f.db.ended = []model.FoundryAutomationEnded{f.db.endedWork("person-load", "ready", at)}
	f.task.watch(ctx)
	runA := f.started(f.lastFire(t))
	f.task.advance(ctx, runA)
	f.endSync("sync1", model.ConnectorSyncSucceeded, 3)
	f.task.advance(ctx, runA)
	require.Equal(t, model.FoundryAutomationRunSucceeded, f.db.finished[runA.ID].Status)
	assert.Equal(t, &model.FoundryAutomationCause{RunID: runA.ID, AutomationID: "autoA", Depth: 1}, f.db.causes["sync1"])

	// A's sync fires B: not B's own, and only one deep.
	f.db.watched = []*model.FoundryAutomation{b}
	at = at.Add(time.Minute)
	f.db.ended = []model.FoundryAutomationEnded{f.db.endedWork("sync1", "succeeded", at)}
	f.task.watch(ctx)
	fire := f.lastFire(t)
	require.Empty(t, fire.Skip, "another automation's sync fires it")
	var event model.FoundryAutomationEvent
	require.NoError(t, json.Unmarshal(fire.Run.TriggerEvent, &event))
	assert.Equal(t, []string{"autoA"}, event.CausedBy)
	assert.Equal(t, 1, event.ChainDepth)
	runB := f.started(fire)
	f.task.advance(ctx, runB)
	f.endLoad("load1", model.FoundryLoadReady)
	f.task.advance(ctx, runB)
	require.Equal(t, model.FoundryAutomationRunSucceeded, f.db.finished[runB.ID].Status)
	assert.Equal(t, &model.FoundryAutomationCause{RunID: runB.ID, AutomationID: "autoB", Chain: []string{"autoA"}, Depth: 2}, f.db.causes["load1"])

	// B's load would fire A again: A set it off.
	f.db.watched = []*model.FoundryAutomation{a}
	at = at.Add(time.Minute)
	f.db.ended = []model.FoundryAutomationEnded{f.db.endedWork("load1", "ready", at)}
	f.task.watch(ctx)
	assert.Equal(t, "Skipped: the load was started by an automation this one set off; running on it would loop.", f.lastFire(t).Skip)
	assert.Equal(t, 1, f.actions.syncs, "A ran once")
	assert.Equal(t, 1, f.actions.loads, "B ran once")
}

// However many automations chain, each set off by work the one before
// started, the chain stops past maxChainDepth runs; within it, a run carries
// the chain on.
func TestAChainStopsAtItsDepth(t *testing.T) {
	f := newFixture(t)
	cursor := model.FoundryAutomationCursor{At: f.task.now().Add(-time.Hour)}
	a := f.watching("autoE", spec(`[{"from":"start","to":"mail"}]`, node("start", "trigger", `{"kind":"load_ready","ontology_id":"ont1"}`),
		node("mail", "notify", `{"message":""}`)), cursor)
	f.db.watched = []*model.FoundryAutomation{a}

	f.db.causes["deep"] = &model.FoundryAutomationCause{RunID: "runD", AutomationID: "autoD", Chain: []string{"autoA", "autoB", "autoC"}, Depth: 4}
	f.db.ended = []model.FoundryAutomationEnded{f.db.endedWork("deep", "partial", cursor.At.Add(time.Minute))}
	f.task.watch(context.Background())
	assert.Equal(t, "Skipped: the load came from a chain of 4 automations setting each other off; a chain stops after 3.", f.lastFire(t).Skip)

	f.db.causes["within"] = &model.FoundryAutomationCause{RunID: "runC", AutomationID: "autoC", Chain: []string{"autoB", "autoA"}, Depth: 3}
	f.db.ended = []model.FoundryAutomationEnded{f.db.endedWork("within", "ready", cursor.At.Add(time.Minute))}
	f.task.watch(context.Background())
	fire := f.lastFire(t)
	assert.Empty(t, fire.Skip, "3 deep, it fires")
	var event model.FoundryAutomationEvent
	require.NoError(t, json.Unmarshal(fire.Run.TriggerEvent, &event))
	assert.Equal(t, []string{"autoA", "autoB", "autoC"}, event.CausedBy, "every automation behind it, once")
	assert.Equal(t, 3, event.ChainDepth, "so the work its run starts is 4 deep")

	// A run whose automation is gone leaves only its depth.
	f.db.causes["orphan"] = &model.FoundryAutomationCause{RunID: "runX", Depth: 2}
	f.db.ended = []model.FoundryAutomationEnded{f.db.endedWork("orphan", "ready", cursor.At.Add(time.Minute))}
	f.task.watch(context.Background())
	fire = f.lastFire(t)
	assert.Empty(t, fire.Skip)
	require.NoError(t, json.Unmarshal(fire.Run.TriggerEvent, &event))
	assert.Empty(t, event.CausedBy)
	assert.Equal(t, 2, event.ChainDepth)
}

// Documents and drafts have no cause: a guard never holds them back.
func TestOtherTriggersAreNeverSkipped(t *testing.T) {
	f := newFixture(t)
	cursor := model.FoundryAutomationCursor{At: f.task.now().Add(-time.Hour)}
	a := f.watching("auto1", spec(`[{"from":"start","to":"mail"}]`, node("start", "trigger", `{"kind":"document_arrived","set_id":"set1"}`),
		node("mail", "notify", `{"message":""}`)), cursor)
	f.db.watched = []*model.FoundryAutomation{a}
	f.db.ended = []model.FoundryAutomationEnded{f.db.endedWork("doc1", "uploaded", cursor.At.Add(time.Minute))}
	f.task.watch(context.Background())
	assert.Empty(t, f.lastFire(t).Skip)
	assert.NotNil(t, f.lastFire(t).Run)
}
