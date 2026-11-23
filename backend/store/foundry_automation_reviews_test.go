package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/reviewcards"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// A reviews_waiting trigger reads its ontology's current load and its open
// cards, once the review index is whole and its count fresh, whatever its
// cursor names (the worker decides), with the cause of a load an automation
// started; nothing for an index never whole, a stale count, a load no longer
// current, a deleted ontology or another workspace's.
func TestFoundryAutomationFindEndedReviewsWaiting(t *testing.T) {
	f := newCardsFixture(t, "RWAIT")
	ctx := context.Background()
	autos := automationFixture{db: f.db, workspaceID: f.ontology.WorkspaceID, tag: "RWAIT"}
	a := autos.active(t, 1, "reviews_waiting", &f.ontology.ID)
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := testDB.Exec(q, args...)
		require.NoError(t, err)
	}
	// indexed sets the load's index row: its builder version (nil: never
	// whole), open count, and when it was dirtied and counted.
	indexed := func(version any, open int, dirty, counted time.Time) {
		t.Helper()
		exec(`INSERT INTO foundry_review_loads (load_id, ontology_id, version, open_cards, dirty_at, counted_at) VALUES (?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE version=VALUES(version), open_cards=VALUES(open_cards), dirty_at=VALUES(dirty_at), counted_at=VALUES(counted_at)`,
			f.load, f.ontology.ID, version, open, dirty, counted)
	}
	find := func(a *model.FoundryAutomation) []model.FoundryAutomationEnded {
		t.Helper()
		ended, err := f.db.FindFoundryAutomationEnded(ctx, a, time.Hour, 0)
		require.NoError(t, err)
		return ended
	}
	before, after := a.Cursor.At.Add(-time.Minute), a.Cursor.At.Add(time.Minute)

	indexed(nil, 60, after, after)
	require.Empty(t, find(a), "an index never whole")
	indexed(reviewcards.Version, 60, after.Add(time.Second), after)
	require.Empty(t, find(a), "a stale count")

	indexed(reviewcards.Version, 60, after, after)
	ended := find(a)
	require.Len(t, ended, 1, "settle and limit don't apply: it reads state")
	require.Equal(t, model.FoundryAutomationEnded{Cursor: model.FoundryAutomationCursor{At: after, ID: f.load}, Rows: ptr(int64(60))}, ended[0])
	indexed(reviewcards.Version, 60, before, before)
	require.Equal(t, a.Cursor.At, find(a)[0].Cursor.At, "never before the cursor")

	exec(`UPDATE foundry_automations SET cursor_id=? WHERE id=?`, f.load, a.ID)
	fired, err := f.db.GetFoundryAutomation(ctx, f.ontology.WorkspaceID, a.ID)
	require.NoError(t, err)
	require.Len(t, find(fired), 1, "the worker decides what a load it fired on does")

	// A load a run of the automation started carries that run.
	run := autos.runID(1)
	exec(`INSERT INTO foundry_automation_runs (id, automation_id, workspace_id, owner_user_id, spec_revision, spec, trigger_key, trigger_event, status, finished_at)
VALUES (?, ?, ?, ?, 1, '{}', 'manual:x', '{"kind": "manual", "caused_by_automations": ["`+autos.id(9)+`"]}', 'succeeded', NOW(6))`,
		run, a.ID, f.ontology.WorkspaceID, automationOwner)
	exec(`UPDATE foundry_loads SET started_by_automation_run_id=?, automation_chain_depth=2 WHERE id=?`, run, f.load)
	require.Equal(t, &model.FoundryAutomationCause{RunID: run, AutomationID: a.ID, Chain: []string{autos.id(9)}, Depth: 2}, find(a)[0].Cause)

	elsewhere := automationFixture{db: f.db, workspaceID: id26("UWS-RWOTHER"), tag: "RWOTHER"}
	createWorkspace(t, elsewhere.workspaceID)
	other := elsewhere.active(t, 1, "reviews_waiting", &f.ontology.ID)
	require.Empty(t, find(other), "another workspace's ontology")

	exec(`UPDATE foundry_ontologies SET current_load_id=NULL WHERE id=?`, f.ontology.ID)
	require.Empty(t, find(a), "a load no longer current")
	exec(`UPDATE foundry_ontologies SET deleted_at=NOW(6), live_key=NULL WHERE id=?`, f.ontology.ID)
	require.Empty(t, find(a), "a deleted ontology")
}
