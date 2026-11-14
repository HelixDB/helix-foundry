package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// automationFixture is a workspace where Ada owns automations and Alan is
// another member.
type automationFixture struct {
	db          store.Store
	workspaceID string
	tag         string
}

const (
	automationOwner = "user-ada"
	automationOther = "user-alan"
	// automationSpec stands for a spec: the store keeps it as JSON it doesn't read.
	automationSpec = `{"v": 1, "nodes": [], "edges": []}`
)

var automationLimits = model.FoundryAutomationLimits{RunsPerDay: 3}

func newAutomationFixture(t *testing.T, tag string) automationFixture {
	t.Helper()
	f := automationFixture{db: newTestStore(t), workspaceID: id26("UWS-" + tag), tag: tag}
	createWorkspace(t, f.workspaceID)
	return f
}

func (f automationFixture) id(n int) string    { return id26(fmt.Sprintf("U-%s-%03d-", f.tag, n)) }
func (f automationFixture) runID(n int) string { return id26(fmt.Sprintf("UR-%s-%03d-", f.tag, n)) }

// create stores automation n of trigger kind (and target), waiting for approval.
func (f automationFixture) create(t *testing.T, n int, kind string, target *string) *model.FoundryAutomation {
	t.Helper()
	token := fmt.Sprintf("create-%d", n)
	require.NoError(t, f.db.CreateFoundryAutomation(context.Background(), &model.FoundryAutomation{
		ID: f.id(n), WorkspaceID: f.workspaceID, OwnerUserID: automationOwner, Name: fmt.Sprintf("Automation %d", n), Description: "",
		Spec: json.RawMessage(automationSpec), TriggerKind: kind, TriggerTarget: target, ClientToken: &token,
	}, 10))
	a, err := f.db.GetFoundryAutomation(context.Background(), f.workspaceID, f.id(n))
	require.NoError(t, err)
	return a
}

// active creates automation n and approves it.
func (f automationFixture) active(t *testing.T, n int, kind string, target *string) *model.FoundryAutomation {
	t.Helper()
	a := f.create(t, n, kind, target)
	var next *time.Time
	if kind == "schedule" {
		next = ptr(time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond))
	}
	a, err := f.db.ApproveFoundryAutomation(context.Background(), f.workspaceID, a.ID, automationOwner, a.Revision, next)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationActive, a.Status)
	return a
}

func (f automationFixture) run(n, automation int, key string) *model.FoundryAutomationRun {
	return &model.FoundryAutomationRun{ID: f.runID(n), AutomationID: f.id(automation), WorkspaceID: f.workspaceID, OwnerUserID: automationOwner,
		TriggerKey: key, TriggerEvent: json.RawMessage(`{"kind": "manual"}`)}
}

func automationRefused(t *testing.T, err error, reason model.FoundryAutomationRefusalReason) {
	t.Helper()
	var r model.FoundryAutomationRefusal
	require.ErrorAs(t, err, &r)
	require.Equal(t, reason, r.Reason, r.Detail)
}

func TestFoundryAutomationCreateAndList(t *testing.T) {
	f := newAutomationFixture(t, "create")
	ctx := context.Background()
	a := f.create(t, 1, "manual", nil)
	require.Equal(t, model.FoundryAutomationNeedsApproval, a.Status)
	require.Nil(t, a.ApprovedSpecRevision)
	require.Equal(t, int64(1), a.Revision)
	require.JSONEq(t, automationSpec, string(a.Spec))
	require.Nil(t, a.LastRun)

	again := &model.FoundryAutomation{ID: f.id(2), WorkspaceID: f.workspaceID, OwnerUserID: automationOwner, Name: "Again", Spec: json.RawMessage(automationSpec),
		TriggerKind: "manual", ClientToken: a.ClientToken}
	require.ErrorIs(t, f.db.CreateFoundryAutomation(ctx, again, 10), store.ErrConflict, "the owner's token makes one automation")
	byToken, err := f.db.GetFoundryAutomationByClientToken(ctx, f.workspaceID, automationOwner, *a.ClientToken)
	require.NoError(t, err)
	require.Equal(t, a.ID, byToken.ID)

	again.ClientToken = nil
	automationRefused(t, f.db.CreateFoundryAutomation(ctx, again, 1), model.FoundryAutomationLimit)
	require.NoError(t, f.db.CreateFoundryAutomation(ctx, again, 2))
	list, err := f.db.ListFoundryAutomations(ctx, f.workspaceID)
	require.NoError(t, err)
	require.Len(t, list, 2)
	other, err := f.db.ListFoundryAutomations(ctx, id26("UWS-other"))
	require.NoError(t, err)
	require.Empty(t, other)
	_, err = f.db.GetFoundryAutomation(ctx, id26("UWS-other"), a.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "another workspace's automation is not found")
}

func TestFoundryAutomationUpdateAndApprove(t *testing.T) {
	f := newAutomationFixture(t, "update")
	ctx := context.Background()
	a := f.create(t, 1, "manual", nil)
	_, err := f.db.ApproveFoundryAutomation(ctx, f.workspaceID, a.ID, automationOther, a.Revision, nil)
	automationRefused(t, err, model.FoundryAutomationNotOwner)
	_, err = f.db.ApproveFoundryAutomation(ctx, f.workspaceID, a.ID, automationOwner, a.Revision+1, nil)
	automationRefused(t, err, model.FoundryAutomationChanged)
	_, err = f.db.ApproveFoundryAutomation(ctx, f.workspaceID, a.ID, automationOwner, a.Revision, ptr(time.Now()))
	require.ErrorContains(t, err, "only a schedule", "a manual trigger has no first time")

	a, err = f.db.ApproveFoundryAutomation(ctx, f.workspaceID, a.ID, automationOwner, a.Revision, nil)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationActive, a.Status)
	require.Equal(t, ptr(int64(1)), a.ApprovedSpecRevision)
	require.NotNil(t, a.ApprovedAt)
	require.Nil(t, a.NextFireAt)
	require.Nil(t, a.Cursor, "a manual trigger watches nothing")
	approvedAt := *a.ApprovedAt
	again, err := f.db.ApproveFoundryAutomation(ctx, f.workspaceID, a.ID, automationOwner, a.Revision, nil)
	require.NoError(t, err)
	require.Equal(t, approvedAt, *again.ApprovedAt, "approving the approved spec again changes nothing")

	// A name alone keeps it approved and running.
	name := "Renamed"
	a, err = f.db.UpdateFoundryAutomation(ctx, f.workspaceID, a.ID, automationOwner, a.Revision, model.FoundryAutomationUpdate{Name: &name})
	require.NoError(t, err)
	require.Equal(t, "Renamed", a.Name)
	require.Equal(t, int64(2), a.Revision)
	require.Equal(t, int64(1), a.SpecRevision)
	require.Equal(t, model.FoundryAutomationActive, a.Status)

	// The same spec, written another way, is no change.
	same := &model.FoundryAutomationSpec{JSON: json.RawMessage(`{"edges":[],"nodes":[],"v":1}`), TriggerKind: "manual"}
	a, err = f.db.UpdateFoundryAutomation(ctx, f.workspaceID, a.ID, automationOwner, a.Revision, model.FoundryAutomationUpdate{Spec: same})
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationActive, a.Status)
	require.Equal(t, int64(1), a.SpecRevision)

	// Another spec waits for its owner again, on its new trigger.
	set := id26("SET")
	changed := &model.FoundryAutomationSpec{JSON: json.RawMessage(`{"v": 1, "nodes": [1], "edges": []}`), TriggerKind: "document_arrived", TriggerTarget: &set}
	_, err = f.db.UpdateFoundryAutomation(ctx, f.workspaceID, a.ID, automationOther, a.Revision, model.FoundryAutomationUpdate{Spec: changed})
	automationRefused(t, err, model.FoundryAutomationNotOwner)
	_, err = f.db.UpdateFoundryAutomation(ctx, f.workspaceID, a.ID, automationOwner, a.Revision-1, model.FoundryAutomationUpdate{Spec: changed})
	automationRefused(t, err, model.FoundryAutomationChanged)
	a, err = f.db.UpdateFoundryAutomation(ctx, f.workspaceID, a.ID, automationOwner, a.Revision, model.FoundryAutomationUpdate{Spec: changed})
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationNeedsApproval, a.Status)
	require.Equal(t, int64(2), a.SpecRevision)
	require.Equal(t, ptr(int64(1)), a.ApprovedSpecRevision, "what was approved stays on record")
	require.Equal(t, "document_arrived", a.TriggerKind)

	a, err = f.db.ApproveFoundryAutomation(ctx, f.workspaceID, a.ID, automationOwner, a.Revision, nil)
	require.NoError(t, err)
	require.NotNil(t, a.Cursor, "an event trigger starts watching from now")
	require.WithinDuration(t, time.Now(), a.Cursor.At, time.Minute)
	_, err = f.db.UpdateFoundryAutomation(ctx, f.workspaceID, a.ID, automationOwner, a.Revision, model.FoundryAutomationUpdate{})
	require.Error(t, err, "an update changes something")
	_, err = f.db.UpdateFoundryAutomation(ctx, f.workspaceID, f.id(9), automationOwner, 1, model.FoundryAutomationUpdate{Name: &name})
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestFoundryAutomationPauseResumeDelete(t *testing.T) {
	f := newAutomationFixture(t, "pause")
	ctx := context.Background()
	waiting := f.create(t, 1, "manual", nil)
	_, err := f.db.PauseFoundryAutomation(ctx, f.workspaceID, waiting.ID, model.FoundryAutomationActor{UserID: automationOwner})
	automationRefused(t, err, model.FoundryAutomationNotActive)
	_, err = f.db.ResumeFoundryAutomation(ctx, f.workspaceID, waiting.ID, automationOwner, nil)
	automationRefused(t, err, model.FoundryAutomationUnapproved)

	a := f.active(t, 2, "schedule", nil)
	_, err = f.db.PauseFoundryAutomation(ctx, f.workspaceID, a.ID, model.FoundryAutomationActor{UserID: automationOther})
	automationRefused(t, err, model.FoundryAutomationNotAllowed)
	a, err = f.db.PauseFoundryAutomation(ctx, f.workspaceID, a.ID, model.FoundryAutomationActor{UserID: automationOther, Manager: true})
	require.NoError(t, err, "an owner or admin pauses anyone's")
	require.Equal(t, model.FoundryAutomationPaused, a.Status)
	require.Equal(t, ptr(model.FoundryAutomationPausedByPerson), a.PausedReason)
	require.Equal(t, ptr(automationOther), a.PausedByUserID)
	require.Nil(t, a.NextFireAt)
	again, err := f.db.PauseFoundryAutomation(ctx, f.workspaceID, a.ID, model.FoundryAutomationActor{UserID: automationOwner})
	require.NoError(t, err)
	require.Equal(t, a.PausedAt, again.PausedAt, "pausing it again changes nothing")

	_, err = f.db.ResumeFoundryAutomation(ctx, f.workspaceID, a.ID, automationOther, ptr(time.Now()))
	automationRefused(t, err, model.FoundryAutomationNotOwner)
	_, err = f.db.ResumeFoundryAutomation(ctx, f.workspaceID, a.ID, automationOwner, nil)
	require.ErrorContains(t, err, "only a schedule")
	next := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	a, err = f.db.ResumeFoundryAutomation(ctx, f.workspaceID, a.ID, automationOwner, &next)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationActive, a.Status)
	require.Nil(t, a.PausedReason)
	require.Equal(t, next, a.NextFireAt.UTC())
	_, err = f.db.ResumeFoundryAutomation(ctx, f.workspaceID, a.ID, automationOwner, &next)
	require.NoError(t, err, "resuming it again changes nothing")

	require.NoError(t, f.db.PauseFoundryAutomationsOf(ctx, f.workspaceID, automationOwner))
	a, err = f.db.GetFoundryAutomation(ctx, f.workspaceID, a.ID)
	require.NoError(t, err)
	require.Equal(t, ptr(model.FoundryAutomationPausedOwnerLostAccess), a.PausedReason)

	automationRefused(t, f.db.DeleteFoundryAutomation(ctx, f.workspaceID, a.ID, model.FoundryAutomationActor{UserID: automationOther}), model.FoundryAutomationNotAllowed)
	require.NoError(t, f.db.DeleteFoundryAutomation(ctx, f.workspaceID, a.ID, model.FoundryAutomationActor{UserID: automationOwner}))
	_, err = f.db.GetFoundryAutomation(ctx, f.workspaceID, a.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	require.ErrorIs(t, f.db.DeleteFoundryAutomation(ctx, f.workspaceID, a.ID, model.FoundryAutomationActor{UserID: automationOwner}), store.ErrNotFound)
}

func TestFoundryAutomationRunNow(t *testing.T) {
	f := newAutomationFixture(t, "now")
	ctx := context.Background()
	waiting := f.create(t, 1, "manual", nil)
	_, err := f.db.RunFoundryAutomationNow(ctx, f.run(1, 1, "manual:a"), "start", nil, automationLimits)
	automationRefused(t, err, model.FoundryAutomationUnapproved)
	_, err = f.db.ApproveFoundryAutomation(ctx, f.workspaceID, waiting.ID, automationOwner, waiting.Revision, nil)
	require.NoError(t, err)

	other := f.run(1, 1, "manual:a")
	other.OwnerUserID = automationOther
	_, err = f.db.RunFoundryAutomationNow(ctx, other, "start", nil, automationLimits)
	automationRefused(t, err, model.FoundryAutomationNotOwner)

	run, err := f.db.RunFoundryAutomationNow(ctx, f.run(1, 1, "manual:a"), "start", json.RawMessage(`{"summary": "Run by hand"}`), automationLimits)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationRunQueued, run.Status)
	same, err := f.db.RunFoundryAutomationNow(ctx, f.run(2, 1, "manual:a"), "start", nil, automationLimits)
	require.NoError(t, err)
	require.Equal(t, run.ID, same.ID, "the same click queues one run")
	_, err = f.db.RunFoundryAutomationNow(ctx, f.run(3, 1, "manual:b"), "start", nil, automationLimits)
	automationRefused(t, err, model.FoundryAutomationBusy)

	full, err := f.db.GetFoundryAutomationRun(ctx, f.workspaceID, run.ID)
	require.NoError(t, err)
	require.JSONEq(t, automationSpec, string(full.Spec), "a run keeps its automation's spec")
	nodes, err := f.db.ListFoundryAutomationRunNodes(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, nodes, 1)
	require.Equal(t, "start", nodes[0].NodeID)
	require.Equal(t, model.FoundryAutomationNodeSucceeded, nodes[0].Status)
	require.JSONEq(t, `{"summary": "Run by hand"}`, string(nodes[0].Output))

	a, err := f.db.GetFoundryAutomation(ctx, f.workspaceID, waiting.ID)
	require.NoError(t, err)
	require.Equal(t, run.ID, a.LastRun.ID)
	require.Nil(t, a.LastRun.Spec, "a list's run has no spec")

	// A paused automation runs by hand; past the day's runs, none does.
	_, err = f.db.CancelFoundryAutomationRun(ctx, f.workspaceID, run.ID, model.FoundryAutomationActor{UserID: automationOwner})
	require.NoError(t, err)
	_, err = f.db.PauseFoundryAutomation(ctx, f.workspaceID, waiting.ID, model.FoundryAutomationActor{UserID: automationOwner})
	require.NoError(t, err)
	for i, key := range []string{"manual:c", "manual:d"} {
		r, err := f.db.RunFoundryAutomationNow(ctx, f.run(10+i, 1, key), "start", nil, automationLimits)
		require.NoError(t, err)
		_, err = f.db.CancelFoundryAutomationRun(ctx, f.workspaceID, r.ID, model.FoundryAutomationActor{UserID: automationOwner})
		require.NoError(t, err)
	}
	_, err = f.db.RunFoundryAutomationNow(ctx, f.run(20, 1, "manual:e"), "start", nil, automationLimits)
	automationRefused(t, err, model.FoundryAutomationRunsPerDay)

	runs, next, err := f.db.ListFoundryAutomationRuns(ctx, waiting.ID, "", 2)
	require.NoError(t, err)
	require.Len(t, runs, 2)
	require.NotEmpty(t, next)
	rest, next, err := f.db.ListFoundryAutomationRuns(ctx, waiting.ID, next, 2)
	require.NoError(t, err)
	require.Len(t, rest, 1)
	require.Empty(t, next)
	require.Equal(t, run.ID, rest[0].ID, "newest first")
}

func TestFoundryAutomationFireSchedule(t *testing.T) {
	f := newAutomationFixture(t, "sched")
	ctx := context.Background()
	a := f.active(t, 1, "schedule", nil)
	due, err := f.db.DueFoundryAutomationSchedules(ctx, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	at := *due[0].NextFireAt
	next := at.Add(time.Hour)
	fire := func(n int, dueAt, nextAt time.Time) model.FoundryAutomationFired {
		r := f.run(n, 1, "schedule:"+dueAt.UTC().Format(time.RFC3339Nano))
		fired, err := f.db.FireFoundryAutomation(ctx, model.FoundryAutomationFire{AutomationID: a.ID, WorkspaceID: f.workspaceID,
			Advance: model.FoundryScheduleAdvance{Due: dueAt, Next: nextAt}, Run: r, TriggerNodeID: "start"}, automationLimits)
		require.NoError(t, err)
		return fired
	}
	require.Equal(t, model.FoundryAutomationFiredLost, fire(1, at.Add(time.Second), next), "another fire moved it first")
	require.Equal(t, model.FoundryAutomationFiredQueued, fire(1, at, next))
	require.Equal(t, model.FoundryAutomationFiredLost, fire(2, at, next), "a fire is delivered once")
	a, err = f.db.GetFoundryAutomation(ctx, f.workspaceID, a.ID)
	require.NoError(t, err)
	require.Equal(t, next.UTC(), a.NextFireAt.UTC())
	due, err = f.db.DueFoundryAutomationSchedules(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, due, "its next time is ahead")

	// A schedule that comes due while its run is live records a skipped run.
	require.Equal(t, model.FoundryAutomationFiredSkipped, fire(3, next, next.Add(time.Hour)))
	skipped, err := f.db.GetFoundryAutomationRun(ctx, f.workspaceID, f.runID(3))
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationRunSkipped, skipped.Status)
	require.Equal(t, "Skipped: the run before it was still going.", *skipped.Error)
	require.NotNil(t, skipped.FinishedAt)

	// A paused schedule fires nothing.
	_, err = f.db.PauseFoundryAutomation(ctx, f.workspaceID, a.ID, model.FoundryAutomationActor{UserID: automationOwner})
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationFiredLost, fire(4, next.Add(time.Hour), next.Add(2*time.Hour)))
}

func TestFoundryAutomationFireEvents(t *testing.T) {
	f := newAutomationFixture(t, "event")
	ctx := context.Background()
	connector := id26("UCON")
	a := f.active(t, 1, "sync_finished", &connector)
	watched, err := f.db.WatchedFoundryAutomations(ctx, time.Minute, 10)
	require.NoError(t, err)
	require.Len(t, watched, 1)
	from := *watched[0].Cursor
	to := model.FoundryAutomationCursor{At: from.At.Add(time.Second), ID: id26("SYNC1")}
	fire := func(n int, from, to model.FoundryAutomationCursor, run bool) model.FoundryAutomationFired {
		var r *model.FoundryAutomationRun
		if run {
			r = f.run(n, 1, "sync_finished:"+to.ID)
		}
		fired, err := f.db.FireFoundryAutomation(ctx, model.FoundryAutomationFire{AutomationID: a.ID, WorkspaceID: f.workspaceID,
			Advance: model.FoundryCursorAdvance{From: from, To: to}, Run: r, TriggerNodeID: "start", TriggerOutput: json.RawMessage(`{"count": 2}`)}, automationLimits)
		require.NoError(t, err)
		return fired
	}
	require.Equal(t, model.FoundryAutomationFiredLost, fire(1, to, to, true), "the cursor it read moved")
	require.Equal(t, model.FoundryAutomationFiredQueued, fire(1, from, to, true))
	watched, err = f.db.WatchedFoundryAutomations(ctx, time.Minute, 10)
	require.NoError(t, err)
	require.Empty(t, watched, "it looked just now")
	a, err = f.db.GetFoundryAutomation(ctx, f.workspaceID, a.ID)
	require.NoError(t, err)
	require.Equal(t, to.ID, a.Cursor.ID)

	// Work found while its run is live waits for it.
	later := model.FoundryAutomationCursor{At: to.At.Add(time.Second), ID: id26("SYNC2")}
	require.Equal(t, model.FoundryAutomationFiredWaiting, fire(2, *a.Cursor, later, true))
	a, err = f.db.GetFoundryAutomation(ctx, f.workspaceID, a.ID)
	require.NoError(t, err)
	require.Equal(t, to.ID, a.Cursor.ID, "the cursor stays, so the work is found again")

	// Work that fires nothing moves the cursor alone.
	require.Equal(t, model.FoundryAutomationFiredMoved, fire(3, *a.Cursor, later, false))
	// Work delivered before moves it with no second run.
	_, err = f.db.CancelFoundryAutomationRun(ctx, f.workspaceID, f.runID(1), model.FoundryAutomationActor{UserID: automationOwner})
	require.NoError(t, err)
	a, err = f.db.GetFoundryAutomation(ctx, f.workspaceID, a.ID)
	require.NoError(t, err)
	last := model.FoundryAutomationCursor{At: later.At.Add(time.Second), ID: id26("SYNC1")}
	require.Equal(t, model.FoundryAutomationFiredMoved, fire(4, *a.Cursor, last, true), "sync_finished:SYNC1 ran already")

	// Past the day's runs, found work records a skipped run.
	tight := model.FoundryAutomationLimits{RunsPerDay: 1}
	a, err = f.db.GetFoundryAutomation(ctx, f.workspaceID, a.ID)
	require.NoError(t, err)
	beyond := model.FoundryAutomationCursor{At: last.At.Add(time.Second), ID: id26("SYNC3")}
	_, err = f.db.FireFoundryAutomation(ctx, model.FoundryAutomationFire{AutomationID: a.ID, WorkspaceID: f.workspaceID,
		Advance: model.FoundryCursorAdvance{From: *a.Cursor, To: beyond}, Run: f.run(5, 1, "sync_finished:"+beyond.ID), TriggerNodeID: "start"}, tight)
	require.NoError(t, err)
	skipped, err := f.db.GetFoundryAutomationRun(ctx, f.workspaceID, f.runID(5))
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationRunSkipped, skipped.Status)
	require.Contains(t, *skipped.Error, "24 hours")

	_, err = f.db.FireFoundryAutomation(ctx, model.FoundryAutomationFire{AutomationID: f.id(9), WorkspaceID: f.workspaceID,
		Advance: model.FoundryCursorAdvance{From: from, To: to}}, automationLimits)
	require.NoError(t, err, "an automation deleted meanwhile is lost, not an error")
	_, err = f.db.FireFoundryAutomation(ctx, model.FoundryAutomationFire{AutomationID: a.ID, WorkspaceID: f.workspaceID,
		Advance: model.FoundryCursorAdvance{From: from, To: to}, Run: &model.FoundryAutomationRun{ID: "x"}}, automationLimits)
	require.Error(t, err, "a run is complete")
}

func TestFoundryAutomationFindEnded(t *testing.T) {
	f := newAutomationFixture(t, "find")
	ctx := context.Background()
	connector, ontology, set := id26("FCON"), id26("FONT"), id26("FSET")
	// rewind starts a's cursor an hour back, so the work below ended between it and now.
	rewind := func(a *model.FoundryAutomation) *model.FoundryAutomation {
		t.Helper()
		_, err := testDB.Exec(`UPDATE foundry_automations SET cursor_at = cursor_at - INTERVAL 1 HOUR WHERE id=?`, a.ID)
		require.NoError(t, err)
		a, err = f.db.GetFoundryAutomation(ctx, f.workspaceID, a.ID)
		require.NoError(t, err)
		return a
	}
	sync := rewind(f.active(t, 1, "sync_finished", &connector))
	start := sync.Cursor.At
	at := func(seconds int) time.Time { return start.Add(time.Duration(seconds) * time.Second) }
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := testDB.Exec(q, args...)
		require.NoError(t, err)
	}
	for i, row := range []struct {
		status  string
		finished time.Time
	}{{"succeeded", at(-10)}, {"succeeded", at(5)}, {"failed", at(5)}, {"running", at(6)}, {"succeeded", time.Now().UTC().Add(time.Hour)}} {
		var finished any = row.finished
		if row.status == "running" {
			finished = nil
		}
		exec(`INSERT INTO connector_syncs (id, connector_id, workspace_id, status, rows_synced, created_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id26(fmt.Sprintf("SY%d", i)), connector, f.workspaceID, row.status, 10*i, start.Add(-time.Hour), finished)
	}
	exec(`INSERT INTO connector_syncs (id, connector_id, workspace_id, status, created_at, finished_at) VALUES (?, ?, ?, 'succeeded', ?, ?)`,
		id26("SYOTHER"), id26("OTHERCON"), f.workspaceID, start, at(5))
	exec(`INSERT INTO connector_syncs (id, connector_id, workspace_id, status, created_at, finished_at) VALUES (?, ?, ?, 'succeeded', ?, ?)`,
		id26("SYOLD"), connector, f.workspaceID, start.Add(-4*24*time.Hour), at(7))
	ended, err := f.db.FindFoundryAutomationEnded(ctx, sync, 0, 10)
	require.NoError(t, err)
	require.Len(t, ended, 2, "after the cursor, ended, of its connector, settled, created within the look back")
	require.Equal(t, id26("SY1"), ended[0].Cursor.ID)
	require.Equal(t, id26("SY2"), ended[1].Cursor.ID, "the same end time orders by id")
	require.Equal(t, "failed", ended[1].Outcome)
	require.Equal(t, ptr(int64(20)), ended[1].Rows)
	limited, err := f.db.FindFoundryAutomationEnded(ctx, sync, 0, 1)
	require.NoError(t, err)
	require.Len(t, limited, 1)
	unsettled, err := f.db.FindFoundryAutomationEnded(ctx, sync, time.Since(start), 10)
	require.NoError(t, err)
	require.Empty(t, unsettled, "work that ended within settle waits")

	load := rewind(f.active(t, 2, "load_ready", &ontology))
	exec(`INSERT INTO foundry_loads (id, workspace_id, ontology_id, version_id, status, created_at, finished_at) VALUES (?, ?, ?, 'V', 'ready', ?, ?)`,
		id26("LOAD1"), f.workspaceID, ontology, load.Cursor.At, load.Cursor.At.Add(time.Second))
	exec(`INSERT INTO foundry_loads (id, workspace_id, ontology_id, version_id, status, created_at) VALUES (?, ?, ?, 'V', 'writing', ?)`,
		id26("LOAD2"), f.workspaceID, ontology, load.Cursor.At)
	ended, err = f.db.FindFoundryAutomationEnded(ctx, load, 0, 10)
	require.NoError(t, err)
	require.Len(t, ended, 1)
	require.Equal(t, "ready", ended[0].Outcome)

	generation := rewind(f.active(t, 3, "generation_finished", &ontology))
	exec(`INSERT INTO foundry_ontology_generations (id, workspace_id, ontology_id, status, input, created_at, finished_at) VALUES (?, ?, ?, 'succeeded', '{}', ?, ?)`,
		id26("GEN1"), f.workspaceID, ontology, generation.Cursor.At, generation.Cursor.At.Add(time.Second))
	exec(`INSERT INTO foundry_ontology_generations (id, workspace_id, ontology_id, status, input, created_at, finished_at) VALUES (?, ?, ?, 'succeeded', '{"revise": {}}', ?, ?)`,
		id26("GEN2"), f.workspaceID, ontology, generation.Cursor.At, generation.Cursor.At.Add(time.Second))
	ended, err = f.db.FindFoundryAutomationEnded(ctx, generation, 0, 10)
	require.NoError(t, err)
	require.Len(t, ended, 1, "a revision is no draft")
	require.Equal(t, id26("GEN1"), ended[0].Cursor.ID)

	docs := rewind(f.active(t, 4, "document_arrived", &set))
	for i, status := range []string{"uploaded", "pending_upload", "uploaded"} {
		var uploaded any = docs.Cursor.At.Add(time.Duration(i+1) * time.Second)
		if status == "pending_upload" {
			uploaded = nil
		}
		exec(`INSERT INTO foundry_documents (id, workspace_id, set_id, name, media_type, size_bytes, sha256, client_token, status, s3_key, created_at, uploaded_at)
VALUES (?, ?, ?, 'a.md', 'text/markdown', 1, ?, ?, ?, 'k', ?, ?)`, id26(fmt.Sprintf("DOC%d", i)), f.workspaceID, set, fmt.Sprintf("%064d", i),
			fmt.Sprintf("t%d", i), status, docs.Cursor.At, uploaded)
	}
	ended, err = f.db.FindFoundryAutomationEnded(ctx, docs, 0, 10)
	require.NoError(t, err)
	require.Len(t, ended, 2, "only documents that arrived")

	manual := f.active(t, 5, "manual", nil)
	_, err = f.db.FindFoundryAutomationEnded(ctx, manual, 0, 10)
	require.Error(t, err, "a manual trigger watches nothing")
}

func TestFoundryAutomationRunSteps(t *testing.T) {
	f := newAutomationFixture(t, "steps")
	ctx := context.Background()
	a := f.active(t, 1, "manual", nil)
	_, err := f.db.RunFoundryAutomationNow(ctx, f.run(1, 1, "manual:a"), "start", nil, automationLimits)
	require.NoError(t, err)
	run, err := f.db.ClaimFoundryAutomationRun(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.Equal(t, f.runID(1), run.ID)
	require.Equal(t, model.FoundryAutomationRunRunning, run.Status)
	require.Equal(t, 1, run.Attempts)
	require.JSONEq(t, automationSpec, string(run.Spec))
	none, err := f.db.ClaimFoundryAutomationRun(ctx, "w2", time.Minute)
	require.NoError(t, err)
	require.Nil(t, none, "a leased run is held")

	require.ErrorIs(t, f.db.StartFoundryAutomationNode(ctx, run.ID, "w2", "sync", model.FoundryAutomationNodeExecuting), store.ErrNotFound,
		"only its lease holder writes it")
	require.Error(t, f.db.StartFoundryAutomationNode(ctx, run.ID, "w1", "sync", model.FoundryAutomationNodeSucceeded))
	require.NoError(t, f.db.StartFoundryAutomationNode(ctx, run.ID, "w1", "sync", model.FoundryAutomationNodeExecuting))
	require.ErrorIs(t, f.db.StartFoundryAutomationNode(ctx, run.ID, "w1", "sync", model.FoundryAutomationNodeExecuting), store.ErrConflict)
	require.NoError(t, f.db.EndFoundryAutomationNode(ctx, run.ID, "w1", "sync", model.FoundryAutomationNodeEnd{
		Status: model.FoundryAutomationNodeSucceeded, Output: json.RawMessage(`{"result": {"ok": true}}`)}))
	require.ErrorIs(t, f.db.EndFoundryAutomationNode(ctx, run.ID, "w1", "sync", model.FoundryAutomationNodeEnd{Status: model.FoundryAutomationNodeSucceeded}),
		store.ErrConflict, "a step ends once")
	require.Error(t, f.db.EndFoundryAutomationNode(ctx, run.ID, "w1", "x", model.FoundryAutomationNodeEnd{Status: model.FoundryAutomationNodeFailed}),
		"a failed step says why")

	// A notify step ends with its email, at once.
	notice := &model.FoundryNotification{ID: id26("NOTE1"), Kind: model.FoundryNotifyAutomationRun, SubjectID: id26("NOTESUBJ1"), WorkspaceID: f.workspaceID,
		UserID: automationOwner, Status: model.FoundryNotificationPending}
	require.NoError(t, f.db.EndFoundryAutomationNode(ctx, run.ID, "w1", "mail", model.FoundryAutomationNodeEnd{
		Status: model.FoundryAutomationNodeSucceeded, Output: json.RawMessage(`{}`), Notification: notice, USDMicros: 0}))
	told, err := f.db.GetFoundryAutomationNotice(ctx, notice)
	require.NoError(t, err)
	require.Equal(t, "mail", told.Node.NodeID)
	require.Equal(t, a.ID, told.Automation.ID)
	require.Len(t, told.Earlier, 2, "the trigger and the sync")

	// An Assistant step queues its turn in the run's own thread.
	thread := &model.FoundryAgentThread{ID: id26("UTHREAD"), WorkspaceID: f.workspaceID, UserID: automationOwner, Kind: model.FoundryAgentThreadAutomation,
		Title: a.Name, Runtime: model.FoundryAgentRuntimeAPI}
	turn := &model.FoundryAgentTurn{ID: id26("UTURN"), WorkspaceID: f.workspaceID, ThreadID: thread.ID, UserID: automationOwner,
		Kind: model.FoundryAgentTurnAutomation, Input: "Count the invoices", MaxUSDMicros: 500_000, AutomationRunID: &run.ID, AutomationNodeID: ptr("ask")}
	require.Error(t, f.db.StartFoundryAutomationAssistant(ctx, run.ID, "w1", "other", thread, turn), "the turn is the step's")
	require.NoError(t, f.db.StartFoundryAutomationAssistant(ctx, run.ID, "w1", "ask", thread, turn))
	queued, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, turn.ID)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAgentTurnAutomation, queued.Kind)
	require.Equal(t, &run.ID, queued.AutomationRunID)
	require.Equal(t, ptr("ask"), queued.AutomationNodeID)
	threads, _, err := f.db.ListFoundryAgentThreads(ctx, f.workspaceID, automationOwner, "", 10)
	require.NoError(t, err)
	require.Empty(t, threads, "a run's thread is no chat")
	got, err := f.db.GetFoundryAgentThread(ctx, f.workspaceID, thread.ID)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAgentThreadAutomation, got.Kind)

	require.NoError(t, f.db.YieldFoundryAutomationRun(ctx, run.ID, "w1", time.Now().Add(-time.Second)))
	run, err = f.db.ClaimFoundryAutomationRun(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.Equal(t, 1, run.Attempts, "waiting on a step counts no attempt")
	require.Equal(t, &thread.ID, run.ThreadID)
	require.NoError(t, f.db.RenewFoundryAutomationRunLease(ctx, run.ID, "w1", time.Minute))
	require.NoError(t, f.db.EndFoundryAutomationNode(ctx, run.ID, "w1", "ask", model.FoundryAutomationNodeEnd{
		Status: model.FoundryAutomationNodeSucceeded, Output: json.RawMessage(`{"summary": "3 overdue"}`), USDMicros: 1200}))
	require.NoError(t, f.db.RetryFoundryAutomationRun(ctx, run.ID, "w1", time.Now().Add(-time.Second), "the store was busy"))
	run, err = f.db.ClaimFoundryAutomationRun(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.Equal(t, 2, run.Attempts, "a retry counts its attempt")
	require.Equal(t, int64(1200), run.USDMicros)

	require.NoError(t, f.db.FinishFoundryAutomationRun(ctx, run.ID, "w1", model.FoundryAutomationRunEnd{Status: model.FoundryAutomationRunSucceeded, PauseAfter: 3}))
	require.ErrorIs(t, f.db.FinishFoundryAutomationRun(ctx, run.ID, "w1", model.FoundryAutomationRunEnd{Status: model.FoundryAutomationRunSucceeded, PauseAfter: 3}),
		store.ErrNotFound, "a run ends once")
	nodes, err := f.db.ListFoundryAutomationRunNodes(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, nodes, 4)
}

func TestFoundryAutomationFailuresPause(t *testing.T) {
	f := newAutomationFixture(t, "fail")
	ctx := context.Background()
	a := f.active(t, 1, "manual", nil)
	failOnce := func(n int, lost bool) {
		t.Helper()
		_, err := f.db.RunFoundryAutomationNow(ctx, f.run(n, 1, fmt.Sprintf("manual:%d", n)), "start", nil, model.FoundryAutomationLimits{RunsPerDay: 100})
		require.NoError(t, err)
		run, err := f.db.ClaimFoundryAutomationRun(ctx, "w1", time.Minute)
		require.NoError(t, err)
		notice := &model.FoundryNotification{ID: id26(fmt.Sprintf("PN%d", n)), Kind: model.FoundryNotifyAutomationPaused, SubjectID: id26(fmt.Sprintf("PS%d", n)),
			WorkspaceID: f.workspaceID, UserID: automationOwner, Status: model.FoundryNotificationPending}
		require.NoError(t, f.db.FinishFoundryAutomationRun(ctx, run.ID, "w1", model.FoundryAutomationRunEnd{
			Status: model.FoundryAutomationRunFailed, Error: "Step sync failed.", PauseAfter: 2, Backoff: time.Minute, Notice: notice, LostAccess: lost}))
	}
	failOnce(1, false)
	a, err := f.db.GetFoundryAutomation(ctx, f.workspaceID, a.ID)
	require.NoError(t, err)
	require.Equal(t, 1, a.Failures)
	require.Equal(t, model.FoundryAutomationActive, a.Status)
	require.NotNil(t, a.BackoffUntil)
	require.WithinDuration(t, time.Now().Add(time.Minute), *a.BackoffUntil, 30*time.Second)
	require.Equal(t, model.FoundryAutomationRunFailed, a.LastRun.Status)

	failOnce(2, false)
	a, err = f.db.GetFoundryAutomation(ctx, f.workspaceID, a.ID)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationPaused, a.Status)
	require.Equal(t, ptr(model.FoundryAutomationPausedFailures), a.PausedReason)
	require.Equal(t, ptr(id26("PS2")), a.NoticeID)
	told, err := f.db.GetFoundryAutomationNotice(ctx, &model.FoundryNotification{Kind: model.FoundryNotifyAutomationPaused, SubjectID: id26("PS2"),
		WorkspaceID: f.workspaceID})
	require.NoError(t, err)
	require.Equal(t, a.ID, told.Automation.ID)

	// Resumed, a success clears its failures; an owner who lost access pauses it at once.
	a, err = f.db.ResumeFoundryAutomation(ctx, f.workspaceID, a.ID, automationOwner, nil)
	require.NoError(t, err)
	require.Zero(t, a.Failures)
	failOnce(3, true)
	a, err = f.db.GetFoundryAutomation(ctx, f.workspaceID, a.ID)
	require.NoError(t, err)
	require.Equal(t, ptr(model.FoundryAutomationPausedOwnerLostAccess), a.PausedReason)
	require.Equal(t, ptr(id26("PS2")), a.NoticeID, "losing access emails no one")

	require.Error(t, f.db.FinishFoundryAutomationRun(ctx, f.runID(3), "w1", model.FoundryAutomationRunEnd{Status: model.FoundryAutomationRunFailed, PauseAfter: 2}),
		"a failed run says why")
	_, err = f.db.GetFoundryAutomationNotice(ctx, &model.FoundryNotification{Kind: model.FoundryNotifyOntologyReady})
	require.Error(t, err)
}

func TestFoundryAutomationCancelAndDeleteEndTurns(t *testing.T) {
	f := newAutomationFixture(t, "cancel")
	ctx := context.Background()
	a := f.active(t, 1, "manual", nil)
	_, err := f.db.RunFoundryAutomationNow(ctx, f.run(1, 1, "manual:a"), "start", nil, automationLimits)
	require.NoError(t, err)
	run, err := f.db.ClaimFoundryAutomationRun(ctx, "w1", time.Minute)
	require.NoError(t, err)
	thread := &model.FoundryAgentThread{ID: id26("CTHREAD"), WorkspaceID: f.workspaceID, UserID: automationOwner, Kind: model.FoundryAgentThreadAutomation,
		Title: a.Name, Runtime: model.FoundryAgentRuntimeAPI}
	turn := &model.FoundryAgentTurn{ID: id26("CTURN"), WorkspaceID: f.workspaceID, ThreadID: thread.ID, UserID: automationOwner,
		Kind: model.FoundryAgentTurnAutomation, Input: "Look", MaxUSDMicros: 500_000, AutomationRunID: &run.ID, AutomationNodeID: ptr("ask")}
	require.NoError(t, f.db.StartFoundryAutomationAssistant(ctx, run.ID, "w1", "ask", thread, turn))

	_, err = f.db.CancelFoundryAutomationRun(ctx, f.workspaceID, run.ID, model.FoundryAutomationActor{UserID: automationOther})
	automationRefused(t, err, model.FoundryAutomationNotAllowed)
	cancelled, err := f.db.CancelFoundryAutomationRun(ctx, f.workspaceID, run.ID, model.FoundryAutomationActor{UserID: automationOther, Manager: true})
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationRunCancelled, cancelled.Status)
	again, err := f.db.CancelFoundryAutomationRun(ctx, f.workspaceID, run.ID, model.FoundryAutomationActor{UserID: automationOwner})
	require.NoError(t, err)
	require.Equal(t, cancelled.FinishedAt, again.FinishedAt)
	ended, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, turn.ID)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAgentTurnCancelled, ended.Status)
	nodes, err := f.db.ListFoundryAutomationRunNodes(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationNodeFailed, nodes[1].Status)
	require.ErrorIs(t, f.db.EndFoundryAutomationNode(ctx, run.ID, "w1", "x", model.FoundryAutomationNodeEnd{Status: model.FoundryAutomationNodeSkipped}),
		store.ErrNotFound, "a cancelled run's worker lost it")
	_, err = f.db.CancelFoundryAutomationRun(ctx, f.workspaceID, f.runID(9), model.FoundryAutomationActor{UserID: automationOwner})
	require.ErrorIs(t, err, store.ErrNotFound)

	// A run that ended otherwise can't be cancelled.
	_, err = f.db.RunFoundryAutomationNow(ctx, f.run(2, 1, "manual:b"), "start", nil, automationLimits)
	require.NoError(t, err)
	run, err = f.db.ClaimFoundryAutomationRun(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.NoError(t, f.db.FinishFoundryAutomationRun(ctx, run.ID, "w1", model.FoundryAutomationRunEnd{Status: model.FoundryAutomationRunSucceeded, PauseAfter: 3}))
	_, err = f.db.CancelFoundryAutomationRun(ctx, f.workspaceID, run.ID, model.FoundryAutomationActor{UserID: automationOwner})
	automationRefused(t, err, model.FoundryAutomationRunEnded)

	// Deleting it ends the turn a live run waits on, and its runs go with it.
	_, err = f.db.RunFoundryAutomationNow(ctx, f.run(3, 1, "manual:c"), "start", nil, automationLimits)
	require.NoError(t, err)
	run, err = f.db.ClaimFoundryAutomationRun(ctx, "w1", time.Minute)
	require.NoError(t, err)
	turn2 := *turn
	turn2.ID, turn2.AutomationRunID = id26("CTURN2"), &run.ID
	require.NoError(t, f.db.StartFoundryAutomationAssistant(ctx, run.ID, "w1", "ask", nil, &turn2))
	require.NoError(t, f.db.DeleteFoundryAutomation(ctx, f.workspaceID, a.ID, model.FoundryAutomationActor{UserID: automationOther, Manager: true}))
	ended, err = f.db.GetFoundryAgentTurn(ctx, f.workspaceID, turn2.ID)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAgentTurnCancelled, ended.Status)
	_, err = f.db.GetFoundryAutomationRun(ctx, f.workspaceID, run.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestFoundryAutomationTurnsHaveTheirOwnBudget(t *testing.T) {
	f := newAgentFixture(t, "pool")
	ctx := context.Background()
	runID := id26("POOLRUN")
	thread := &model.FoundryAgentThread{ID: id26("POOLTHREAD"), WorkspaceID: f.workspaceID, UserID: agentUser, Kind: model.FoundryAgentThreadAutomation,
		Title: "Weekly", Runtime: model.FoundryAgentRuntimeAPI}
	// The run row the turn's step belongs to.
	_, err := testDB.Exec(`INSERT INTO foundry_automation_runs (id, automation_id, workspace_id, owner_user_id, spec, spec_revision, trigger_key, trigger_event,
  status, lease_owner, lease_until) VALUES (?, ?, ?, ?, '{}', 1, 'manual:x', '{}', 'running', 'w1', NOW(6) + INTERVAL 1 HOUR)`,
		runID, id26("POOLAUTO"), f.workspaceID, agentUser)
	require.NoError(t, err)
	turn := &model.FoundryAgentTurn{ID: id26("POOLTURN"), WorkspaceID: f.workspaceID, ThreadID: thread.ID, UserID: agentUser,
		Kind: model.FoundryAgentTurnAutomation, Input: "Look", MaxUSDMicros: 500_000, AutomationRunID: &runID, AutomationNodeID: ptr("ask")}
	require.NoError(t, f.db.StartFoundryAutomationAssistant(ctx, runID, "w1", "ask", thread, turn))

	// People spent their whole month; automations' own budget still runs the turn.
	_, err = testDB.Exec(`INSERT INTO foundry_agent_turns (id, workspace_id, thread_id, user_id, runtime, kind, input, status, max_usd_micros, usd_micros,
  charged_at, finished_at) VALUES (?, ?, ?, ?, 'api', 'message', 'x', 'done', 500000, 1000000, NOW(6), NOW(6))`,
		id26("SPENT"), f.workspaceID, f.threadID(99), agentUser)
	require.NoError(t, err)
	claim := f.claim()
	claim.Automations = model.FoundryAgentLimits{Since: claim.Limits.Since, CapMicros: 600_000, Running: 1}
	claimed, err := f.db.ClaimFoundryAgentTurn(ctx, "agent-1", claim)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, turn.ID, claimed.ID)

	// And a person's message isn't held back by what automations spend.
	require.NoError(t, f.db.SettleFoundryAgentTurn(ctx, turn.ID, "agent-1", model.FoundryAgentTurnOutcome{Status: model.FoundryAgentTurnDone,
		Spend: model.FoundryGraphJobSpend{USDMicros: 600_000}}))
	_, err = testDB.Exec(`DELETE FROM foundry_agent_turns WHERE id=?`, id26("SPENT"))
	require.NoError(t, err)
	require.NoError(t, f.send(1, 0))

	// A second automation turn finds the month's automations budget spent.
	turn2 := *turn
	turn2.ID, turn2.AutomationNodeID = id26("POOLTURN2"), ptr("ask2")
	require.NoError(t, f.db.StartFoundryAutomationAssistant(ctx, runID, "w1", "ask2", nil, &turn2))
	claim.Limits.Running = 1
	for range 2 {
		got, err := f.db.ClaimFoundryAgentTurn(ctx, "agent-1", claim)
		require.NoError(t, err)
		if got != nil && got.ID != turn2.ID {
			continue
		}
		require.Nil(t, got)
	}
	failed, err := f.db.GetFoundryAgentTurn(ctx, f.workspaceID, turn2.ID)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAgentTurnFailed, failed.Status)
	require.Contains(t, *failed.Error, "of automations")
}

func TestFoundryAutomationWorkspaceDeletion(t *testing.T) {
	f := newAutomationFixture(t, "gone")
	ctx := context.Background()
	a := f.active(t, 1, "manual", nil)
	_, err := f.db.RunFoundryAutomationNow(ctx, f.run(1, 1, "manual:a"), "start", nil, automationLimits)
	require.NoError(t, err)
	require.NoError(t, f.db.WithTx(ctx, func(tx store.Store) error { return tx.CancelWorkspaceFoundryWork(ctx, f.workspaceID) }))
	a, err = f.db.GetFoundryAutomation(ctx, f.workspaceID, a.ID)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAutomationPaused, a.Status)
	require.Equal(t, model.FoundryAutomationRunCancelled, a.LastRun.Status)
}
