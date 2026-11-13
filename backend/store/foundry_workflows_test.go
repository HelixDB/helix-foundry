package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// workflow creates workflow FW-<tag> of the fixture's ontology.
func (f foundryFixture) workflow(t *testing.T, tag string) *model.FoundryWorkflow {
	t.Helper()
	w := &model.FoundryWorkflow{
		ID: id26("FW-" + tag), WorkspaceID: f.ontology.WorkspaceID, OntologyID: f.ontology.ID, Name: "Workflow " + tag,
		Description: "does " + tag, ClientToken: "workflow-" + tag, CreatedByUserID: id26("U-ADA"),
	}
	require.NoError(t, f.db.CreateFoundryWorkflow(context.Background(), w))
	return w
}

// run starts run FR-<tag> of w, by Ada.
func (f foundryFixture) run(t *testing.T, w *model.FoundryWorkflow, tag string, input *string) *model.FoundryWorkflowRun {
	t.Helper()
	r := &model.FoundryWorkflowRun{
		ID: id26("FR-" + tag), WorkflowID: w.ID, WorkspaceID: w.WorkspaceID, OntologyID: w.OntologyID, ClientToken: "run-" + tag,
		StartedBy: access.Principal{Kind: access.PrincipalUser, ID: id26("U-ADA")}, Input: input,
	}
	require.NoError(t, f.db.StartFoundryWorkflowRun(context.Background(), r))
	got, err := f.db.GetFoundryWorkflowRun(context.Background(), w.OntologyID, r.ID)
	require.NoError(t, err)
	return got
}

func read(load string, result string) *model.FoundryWorkflowRead {
	r := &model.FoundryWorkflowRead{LoadID: load, Reads: json.RawMessage(`[{"entity":{"type":"Customer","id":"acme"}}]`)}
	if result != "" {
		r.Result = json.RawMessage(result)
	}
	return r
}

func countRows(t *testing.T, table, where string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+where, args...).Scan(&n))
	return n
}

func TestFoundryWorkflows(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "WF")
	w := f.workflow(t, "A")

	got, err := f.db.GetFoundryWorkflow(ctx, f.ontology.ID, w.ID)
	require.NoError(t, err)
	assert.Equal(t, "Workflow A", got.Name)
	assert.Equal(t, "does A", got.Description)
	assert.Zero(t, got.Runs)
	assert.Nil(t, got.LastRunAt)

	byToken, err := f.db.GetFoundryWorkflowByClientToken(ctx, f.ontology.ID, "workflow-A")
	require.NoError(t, err)
	assert.Equal(t, w.ID, byToken.ID)

	t.Run("the same client token is a conflict", func(t *testing.T) {
		again := *w
		again.ID = id26("FW-A2")
		assert.ErrorIs(t, f.db.CreateFoundryWorkflow(ctx, &again), store.ErrConflict)
	})
	t.Run("another ontology's id is not found", func(t *testing.T) {
		_, err := f.db.GetFoundryWorkflow(ctx, id26("FO-OTHER"), w.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
	t.Run("missing fields are refused", func(t *testing.T) {
		assert.Error(t, f.db.CreateFoundryWorkflow(ctx, &model.FoundryWorkflow{ID: id26("FW-X")}))
		blank := *w
		blank.ID, blank.ClientToken, blank.Name = id26("FW-BLANK"), "blank", "  "
		assert.Error(t, f.db.CreateFoundryWorkflow(ctx, &blank))
	})
	t.Run("an ontology holds at most FoundryWorkflowsMax", func(t *testing.T) {
		for i := 1; i < model.FoundryWorkflowsMax; i++ {
			f.workflow(t, fmt.Sprintf("CAP%03dX", i))
		}
		over := &model.FoundryWorkflow{
			ID: id26("FW-OVER"), WorkspaceID: f.ontology.WorkspaceID, OntologyID: f.ontology.ID, Name: "over",
			ClientToken: "over", CreatedByUserID: id26("U-ADA"),
		}
		assert.ErrorIs(t, f.db.CreateFoundryWorkflow(ctx, over), store.ErrLimitExceeded)

		// Pages of 30, newest first, then nothing.
		seen := map[string]bool{}
		token := ""
		for {
			page, next, err := f.db.ListFoundryWorkflows(ctx, f.ontology.ID, token, 30)
			require.NoError(t, err)
			for _, w := range page {
				assert.False(t, seen[w.ID], "listed twice: %s", w.ID)
				seen[w.ID] = true
			}
			if next == "" {
				break
			}
			token = next
		}
		assert.Len(t, seen, model.FoundryWorkflowsMax)
		_, _, err := f.db.ListFoundryWorkflows(ctx, f.ontology.ID, "not-a-token", 30)
		assert.ErrorIs(t, err, store.ErrInvalidPageToken)
	})
}

func TestFoundryWorkflowOfDeletedOntology(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "WFDEL")
	w := f.workflow(t, "A")
	r := f.run(t, w, "A", nil)
	_, err := f.db.RecordFoundryWorkflowAccess(ctx, r.ID, read(id26("L1"), `{"results":[]}`))
	require.NoError(t, err)

	require.NoError(t, f.db.DeleteFoundryOntology(ctx, f.ontology.WorkspaceID, f.ontology.ID, id26("FJ-WFDEL")))
	assert.Zero(t, countRows(t, "foundry_workflows", "ontology_id=?", f.ontology.ID))
	assert.Zero(t, countRows(t, "foundry_workflow_runs", "ontology_id=?", f.ontology.ID))
	assert.Zero(t, countRows(t, "foundry_workflow_run_accesses", "run_id=?", r.ID))

	again := &model.FoundryWorkflow{
		ID: id26("FW-LATE"), WorkspaceID: f.ontology.WorkspaceID, OntologyID: f.ontology.ID, Name: "late",
		ClientToken: "late", CreatedByUserID: id26("U-ADA"),
	}
	assert.ErrorIs(t, f.db.CreateFoundryWorkflow(ctx, again), store.ErrNotFound)
}

func TestFoundryWorkflowRuns(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "WFR")
	w := f.workflow(t, "A")
	input := "Which customers renew this quarter?"
	r := f.run(t, w, "A", &input)

	assert.Equal(t, model.FoundryWorkflowRunOpen, r.Status())
	require.NotNil(t, r.Input)
	assert.Equal(t, input, *r.Input)
	assert.Equal(t, access.Principal{Kind: access.PrincipalUser, ID: id26("U-ADA")}, r.StartedBy)

	t.Run("by a service credential", func(t *testing.T) {
		key := &model.FoundryWorkflowRun{
			ID: id26("FR-KEY"), WorkflowID: w.ID, WorkspaceID: w.WorkspaceID, OntologyID: w.OntologyID, ClientToken: "key",
			StartedBy: access.Principal{Kind: access.PrincipalServiceCredential, ID: id26("SC-1")},
		}
		require.NoError(t, f.db.StartFoundryWorkflowRun(ctx, key))
		got, err := f.db.GetFoundryWorkflowRunByClientToken(ctx, w.ID, "key")
		require.NoError(t, err)
		assert.Equal(t, key.StartedBy, got.StartedBy)
		assert.Nil(t, got.Input)
	})
	t.Run("a team or no one starts nothing", func(t *testing.T) {
		team := &model.FoundryWorkflowRun{
			ID: id26("FR-TEAM"), WorkflowID: w.ID, WorkspaceID: w.WorkspaceID, OntologyID: w.OntologyID, ClientToken: "team",
			StartedBy: access.Principal{Kind: access.PrincipalTeam, ID: id26("T-1")},
		}
		assert.Error(t, f.db.StartFoundryWorkflowRun(ctx, team))
		team.StartedBy = access.Principal{}
		assert.Error(t, f.db.StartFoundryWorkflowRun(ctx, team))
	})
	t.Run("the same client token is a conflict", func(t *testing.T) {
		again := *r
		again.ID = id26("FR-A2")
		assert.ErrorIs(t, f.db.StartFoundryWorkflowRun(ctx, &again), store.ErrConflict)
	})
	t.Run("a deleted workflow takes no runs", func(t *testing.T) {
		gone := &model.FoundryWorkflowRun{
			ID: id26("FR-GONE"), WorkflowID: id26("FW-NONE"), WorkspaceID: w.WorkspaceID, OntologyID: w.OntologyID, ClientToken: "gone",
			StartedBy: r.StartedBy,
		}
		assert.ErrorIs(t, f.db.StartFoundryWorkflowRun(ctx, gone), store.ErrNotFound)
	})
	t.Run("the workspace reads it, another doesn't", func(t *testing.T) {
		got, err := f.db.GetFoundryWorkflowRunInWorkspace(ctx, w.WorkspaceID, r.ID)
		require.NoError(t, err)
		assert.Equal(t, r.ID, got.ID)
		_, err = f.db.GetFoundryWorkflowRunInWorkspace(ctx, id26("WS-OTHER"), r.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("finish keeps the input it started with", func(t *testing.T) {
		other := "something else"
		require.NoError(t, f.db.FinishFoundryWorkflowRun(ctx, r.ID, &other, "Acme and Globex.", false))
		got, err := f.db.GetFoundryWorkflowRun(ctx, f.ontology.ID, r.ID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryWorkflowRunFinished, got.Status())
		assert.Equal(t, input, *got.Input)
		assert.Equal(t, "Acme and Globex.", got.Finished.Output)
		assert.False(t, got.Finished.At.Before(got.StartedAt))
		assert.ErrorIs(t, f.db.FinishFoundryWorkflowRun(ctx, r.ID, nil, "again", false), store.ErrNotFound)
	})
	t.Run("finish sets the input it started without", func(t *testing.T) {
		later := f.run(t, w, "LATER", nil)
		in := "later input"
		require.NoError(t, f.db.FinishFoundryWorkflowRun(ctx, later.ID, &in, "out", false))
		got, err := f.db.GetFoundryWorkflowRun(ctx, f.ontology.ID, later.ID)
		require.NoError(t, err)
		assert.Equal(t, in, *got.Input)
	})
	t.Run("the schema refuses a finished run without its input", func(t *testing.T) {
		bare := f.run(t, w, "BARE", nil)
		assert.Error(t, f.db.FinishFoundryWorkflowRun(ctx, bare.ID, nil, "out", false))
	})

	workflow, err := f.db.GetFoundryWorkflow(ctx, f.ontology.ID, w.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 4, workflow.Runs)
	assert.EqualValues(t, 2, workflow.FinishedRuns)
	require.NotNil(t, workflow.LastRunAt)

	t.Run("runs list newest first, a page at a time", func(t *testing.T) {
		first, next, err := f.db.ListFoundryWorkflowRuns(ctx, w.ID, nil, "", 3)
		require.NoError(t, err)
		require.Len(t, first, 3)
		assert.Equal(t, id26("FR-BARE"), first[0].ID)
		rest, next2, err := f.db.ListFoundryWorkflowRuns(ctx, w.ID, nil, next, 3)
		require.NoError(t, err)
		assert.Empty(t, next2)
		require.Len(t, rest, 1)
		assert.Equal(t, r.ID, rest[0].ID)
		_, _, err = f.db.ListFoundryWorkflowRuns(ctx, w.ID, nil, "%%%", 3)
		assert.ErrorIs(t, err, store.ErrInvalidPageToken)
	})
	ada := access.Principal{Kind: access.PrincipalUser, ID: id26("U-ADA")}
	key := access.Principal{Kind: access.PrincipalServiceCredential, ID: id26("SC-1")}
	t.Run("runs list those one principal started", func(t *testing.T) {
		mine, next, err := f.db.ListFoundryWorkflowRuns(ctx, w.ID, &ada, "", 2)
		require.NoError(t, err)
		require.Len(t, mine, 2)
		rest, _, err := f.db.ListFoundryWorkflowRuns(ctx, w.ID, &ada, next, 2)
		require.NoError(t, err)
		require.Len(t, rest, 1)
		for _, r := range append(mine, rest...) {
			assert.Equal(t, ada, r.StartedBy)
		}
		keys, _, err := f.db.ListFoundryWorkflowRuns(ctx, w.ID, &key, "", 10)
		require.NoError(t, err)
		require.Len(t, keys, 1)
		assert.Equal(t, id26("FR-KEY"), keys[0].ID)
		// A team starts none, and a user id under the key's kind matches none.
		for _, p := range []access.Principal{{Kind: access.PrincipalTeam, ID: ada.ID}, {Kind: access.PrincipalServiceCredential, ID: ada.ID}} {
			none, _, err := f.db.ListFoundryWorkflowRuns(ctx, w.ID, &p, "", 10)
			require.NoError(t, err)
			assert.Empty(t, none, p)
		}
	})
	t.Run("a tally counts one principal's runs and their verdicts", func(t *testing.T) {
		_, err := testDB.Exec(`INSERT INTO foundry_workflow_run_assessments (run_id, workspace_id, ontology_id, workflow_id, status, verdict, assessment, finished_at)
VALUES (?, ?, ?, ?, 'done', 'helpful', '{}', NOW(6)), (?, ?, ?, ?, 'queued', NULL, NULL, NULL)`,
			r.ID, w.WorkspaceID, w.OntologyID, w.ID, id26("FR-KEY"), w.WorkspaceID, w.OntologyID, w.ID)
		require.NoError(t, err)
		other := id26("WF-NONE")
		tallies, err := f.db.TallyFoundryWorkflowRuns(ctx, []string{w.ID, other}, ada)
		require.NoError(t, err)
		mine := tallies[w.ID]
		assert.EqualValues(t, 3, mine.Runs)
		assert.EqualValues(t, 2, mine.FinishedRuns)
		require.NotNil(t, mine.LastRunAt)
		assert.Equal(t, map[string]int64{"helpful": 1}, mine.Verdicts)
		assert.Equal(t, model.FoundryWorkflowRunTally{Verdicts: map[string]int64{}}, tallies[other])
		keys, err := f.db.TallyFoundryWorkflowRuns(ctx, []string{w.ID}, key)
		require.NoError(t, err)
		assert.EqualValues(t, 1, keys[w.ID].Runs)
		assert.Zero(t, keys[w.ID].FinishedRuns)
		assert.Empty(t, keys[w.ID].Verdicts, "a queued assessment counts no verdict")
		teams, err := f.db.TallyFoundryWorkflowRuns(ctx, []string{w.ID}, access.Principal{Kind: access.PrincipalTeam, ID: ada.ID})
		require.NoError(t, err)
		assert.Zero(t, teams[w.ID].Runs)
		empty, err := f.db.TallyFoundryWorkflowRuns(ctx, nil, ada)
		require.NoError(t, err)
		assert.Empty(t, empty)
	})
}

func TestFoundryWorkflowRunsKept(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "WFK")
	w := f.workflow(t, "A")
	in := "q"
	oldest := f.run(t, w, "OLDEST", &in)
	_, err := f.db.RecordFoundryWorkflowAccess(ctx, oldest.ID, &model.FoundryWorkflowUpload{DocumentID: "d", SetID: "s"})
	require.NoError(t, err)
	// Finished: an open run from the last day is an agent's at work, spared.
	require.NoError(t, f.db.FinishFoundryWorkflowRun(ctx, oldest.ID, nil, "a", true))
	// The rest of the cap, all after it.
	var values []string
	var args []any
	for i := 1; i < model.FoundryWorkflowRunsKept; i++ {
		values = append(values, "(?, ?, ?, ?, 'open', ?, ?, NOW(6) + INTERVAL ? MICROSECOND)")
		args = append(args, id26(fmt.Sprintf("FR-K%04dX", i)), w.ID, w.WorkspaceID, w.OntologyID, fmt.Sprintf("k%d", i), id26("U-ADA"), i)
	}
	_, err = testDB.Exec(`INSERT INTO foundry_workflow_runs
  (id, workflow_id, workspace_id, ontology_id, status, client_token, started_by_user_id, started_at) VALUES `+
		strings.Join(values, ", "), args...)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryWorkflowRunsKept, countRows(t, "foundry_workflow_runs", "workflow_id=?", w.ID))

	f.run(t, w, "NEWEST", nil)
	assert.Equal(t, model.FoundryWorkflowRunsKept, countRows(t, "foundry_workflow_runs", "workflow_id=?", w.ID))
	_, err = f.db.GetFoundryWorkflowRun(ctx, f.ontology.ID, oldest.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Zero(t, countRows(t, "foundry_workflow_run_accesses", "run_id=?", oldest.ID))
	assert.Zero(t, countRows(t, "foundry_workflow_run_assessments", "run_id=?", oldest.ID))
}

func TestFoundryWorkflowAccesses(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "WFA")
	w := f.workflow(t, "A")
	r := f.run(t, w, "A", nil)
	load := id26("FJ-WFA")

	small := `{"results":[{"ok":{"entity":{"type":"Customer","id":"acme"}}}]}`
	first, err := f.db.RecordFoundryWorkflowAccess(ctx, r.ID, read(load, small))
	require.NoError(t, err)
	assert.EqualValues(t, 1, first.Seq)
	got := first.Call.(*model.FoundryWorkflowRead)
	assert.Equal(t, load, got.LoadID)
	assert.JSONEq(t, small, string(got.Result))
	assert.JSONEq(t, `[{"entity":{"type":"Customer","id":"acme"}}]`, string(got.Reads))

	t.Run("a result over the cap is left out", func(t *testing.T) {
		big := `{"pad":"` + strings.Repeat("x", model.FoundryWorkflowResultMaxBytes) + `"}`
		a, err := f.db.RecordFoundryWorkflowAccess(ctx, r.ID, read(load, big))
		require.NoError(t, err)
		assert.EqualValues(t, 2, a.Seq)
		assert.Nil(t, a.Call.(*model.FoundryWorkflowRead).Result)
	})
	t.Run("a question, a send and an upload are recorded once each", func(t *testing.T) {
		calls := []model.FoundryWorkflowCall{
			&model.FoundryWorkflowQuestion{LoadID: load, QuestionID: id26("Q1")},
			&model.FoundryWorkflowSend{ConnectorID: id26("C1"), Seq: 7, PieceIDs: []string{"p1", "p2", "p3"}},
			&model.FoundryWorkflowUpload{DocumentID: id26("D1"), SetID: id26("S1")},
		}
		for i, call := range calls {
			a, err := f.db.RecordFoundryWorkflowAccess(ctx, r.ID, call)
			require.NoError(t, err)
			assert.EqualValues(t, 3+i, a.Seq)
			assert.Equal(t, call, a.Call)
			again, err := f.db.RecordFoundryWorkflowAccess(ctx, r.ID, call)
			require.NoError(t, err)
			assert.Equal(t, a.Seq, again.Seq)
		}
		// Another send of the same source is another access.
		a, err := f.db.RecordFoundryWorkflowAccess(ctx, r.ID, &model.FoundryWorkflowSend{ConnectorID: id26("C1"), Seq: 8, PieceIDs: []string{"p4"}})
		require.NoError(t, err)
		assert.EqualValues(t, 6, a.Seq)
	})

	run, err := f.db.GetFoundryWorkflowRun(ctx, f.ontology.ID, r.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryWorkflowRunCounts{
		Accesses: 6, Reads: 2, Questions: 1, Sends: 2, Uploads: 1, Pieces: 4, ResultsOmitted: 1, ResultBytes: int64(len(small)),
	}, run.Counts)

	t.Run("accesses list in order, a page at a time", func(t *testing.T) {
		page, more, err := f.db.ListFoundryWorkflowRunAccesses(ctx, r.ID, 0, 4)
		require.NoError(t, err)
		assert.True(t, more)
		require.Len(t, page, 4)
		for i, a := range page {
			assert.EqualValues(t, i+1, a.Seq)
		}
		rest, more, err := f.db.ListFoundryWorkflowRunAccesses(ctx, r.ID, 4, 4)
		require.NoError(t, err)
		assert.False(t, more)
		require.Len(t, rest, 2)
		assert.EqualValues(t, 5, rest[0].Seq)
	})
	t.Run("a finished run records nothing more", func(t *testing.T) {
		in := "q"
		require.NoError(t, f.db.FinishFoundryWorkflowRun(ctx, r.ID, &in, "a", false))
		_, err := f.db.RecordFoundryWorkflowAccess(ctx, r.ID, read(load, small))
		assert.ErrorIs(t, err, store.ErrWorkflowRunFinished)
		// Not even a retry of a call it recorded.
		_, err = f.db.RecordFoundryWorkflowAccess(ctx, r.ID, &model.FoundryWorkflowUpload{DocumentID: id26("D1"), SetID: id26("S1")})
		assert.ErrorIs(t, err, store.ErrWorkflowRunFinished)
	})
	t.Run("a run that is gone records nothing", func(t *testing.T) {
		_, err := f.db.RecordFoundryWorkflowAccess(ctx, id26("FR-NONE"), read(load, small))
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
}

func TestFoundryWorkflowRunResultBudget(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "WFB")
	r := f.run(t, f.workflow(t, "A"), "A", nil)
	// Results of 200 KiB: the run keeps as many as fit in its budget.
	result := `{"pad":"` + strings.Repeat("y", 200<<10) + `"}`
	fit := model.FoundryWorkflowRunResultMaxBytes / len(result)
	for i := range fit + 2 {
		a, err := f.db.RecordFoundryWorkflowAccess(ctx, r.ID, read(id26("FJ-WFB"), result))
		require.NoError(t, err)
		assert.Equal(t, i < fit, a.Call.(*model.FoundryWorkflowRead).Result != nil, "read %d", i+1)
	}
	got, err := f.db.GetFoundryWorkflowRun(ctx, f.ontology.ID, r.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 2, got.Counts.ResultsOmitted)
	assert.EqualValues(t, fit*len(result), got.Counts.ResultBytes)
}

func TestFoundryWorkflowRunAccessCap(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "WFC")
	r := f.run(t, f.workflow(t, "A"), "A", nil)
	question := &model.FoundryWorkflowQuestion{LoadID: id26("L"), QuestionID: id26("Q")}
	_, err := f.db.RecordFoundryWorkflowAccess(ctx, r.ID, question)
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_workflow_runs SET accesses=?, reads_count=? WHERE id=?`,
		model.FoundryWorkflowRunMaxAccesses, model.FoundryWorkflowRunMaxAccesses-1, r.ID)
	require.NoError(t, err)
	_, err = f.db.RecordFoundryWorkflowAccess(ctx, r.ID, read(id26("L"), `{}`))
	assert.ErrorIs(t, err, store.ErrLimitExceeded)
	// A retry of what it recorded is still answered.
	a, err := f.db.RecordFoundryWorkflowAccess(ctx, r.ID, question)
	require.NoError(t, err)
	assert.EqualValues(t, 1, a.Seq)
}

func TestFoundryWorkflowDelete(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "WFD")
	keep, gone := f.workflow(t, "KEEP"), f.workflow(t, "GONE")
	kept, deleted := f.run(t, keep, "KEEP", nil), f.run(t, gone, "GONE", nil)
	for _, r := range []*model.FoundryWorkflowRun{kept, deleted} {
		_, err := f.db.RecordFoundryWorkflowAccess(ctx, r.ID, read(id26("L"), `{}`))
		require.NoError(t, err)
	}
	assert.ErrorIs(t, f.db.DeleteFoundryWorkflow(ctx, id26("FO-OTHER"), gone.ID), store.ErrNotFound)
	require.NoError(t, f.db.DeleteFoundryWorkflow(ctx, f.ontology.ID, gone.ID))
	assert.ErrorIs(t, f.db.DeleteFoundryWorkflow(ctx, f.ontology.ID, gone.ID), store.ErrNotFound)

	_, err := f.db.GetFoundryWorkflow(ctx, f.ontology.ID, gone.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Zero(t, countRows(t, "foundry_workflow_runs", "workflow_id=?", gone.ID))
	assert.Zero(t, countRows(t, "foundry_workflow_run_accesses", "run_id=?", deleted.ID))
	assert.Equal(t, 1, countRows(t, "foundry_workflow_run_accesses", "run_id=?", kept.ID))
	_, err = f.db.RecordFoundryWorkflowAccess(ctx, deleted.ID, read(id26("L"), `{}`))
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestFoundryWorkflowConcurrency races the writers that lock workflows, runs
// and accesses: each record gets its own seq, no access follows a finish,
// and deletes never deadlock with starts or records.
func TestFoundryWorkflowConcurrency(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "WFX")
	w := f.workflow(t, "A")

	t.Run("records take one seq each", func(t *testing.T) {
		r := f.run(t, w, "SEQ", nil)
		var wg sync.WaitGroup
		seqs := make(chan int32, 40)
		for i := range 40 {
			wg.Go(func() {
				a, err := f.db.RecordFoundryWorkflowAccess(ctx, r.ID, &model.FoundryWorkflowUpload{DocumentID: fmt.Sprintf("d%d", i), SetID: "s"})
				assert.NoError(t, err)
				seqs <- a.Seq
			})
		}
		wg.Wait()
		close(seqs)
		seen := map[int32]bool{}
		for s := range seqs {
			seen[s] = true
		}
		assert.Len(t, seen, 40)
		got, err := f.db.GetFoundryWorkflowRun(ctx, f.ontology.ID, r.ID)
		require.NoError(t, err)
		assert.EqualValues(t, 40, got.Counts.Accesses)
	})

	t.Run("a finish and records agree on what the run holds", func(t *testing.T) {
		r := f.run(t, w, "FIN", nil)
		var wg sync.WaitGroup
		var mu sync.Mutex
		recorded := 0
		in := "q"
		wg.Go(func() { assert.NoError(t, f.db.FinishFoundryWorkflowRun(ctx, r.ID, &in, "a", false)) })
		for i := range 20 {
			wg.Go(func() {
				_, err := f.db.RecordFoundryWorkflowAccess(ctx, r.ID, read(id26("L"), fmt.Sprintf(`{"n":%d}`, i)))
				if err != nil {
					assert.ErrorIs(t, err, store.ErrWorkflowRunFinished)
					return
				}
				mu.Lock()
				recorded++
				mu.Unlock()
			})
		}
		wg.Wait()
		got, err := f.db.GetFoundryWorkflowRun(ctx, f.ontology.ID, r.ID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryWorkflowRunFinished, got.Status())
		assert.EqualValues(t, recorded, got.Counts.Accesses)
		assert.Equal(t, recorded, countRows(t, "foundry_workflow_run_accesses", "run_id=?", r.ID))
		_, err = f.db.RecordFoundryWorkflowAccess(ctx, r.ID, read(id26("L"), `{}`))
		assert.ErrorIs(t, err, store.ErrWorkflowRunFinished)
	})

	t.Run("deletes race starts and records without deadlocks", func(t *testing.T) {
		for round := range 5 {
			g := f.workflow(t, fmt.Sprintf("RACE%d", round))
			r := f.run(t, g, fmt.Sprintf("RACE%d", round), nil)
			var wg sync.WaitGroup
			for i := range 10 {
				wg.Go(func() {
					_, err := f.db.RecordFoundryWorkflowAccess(ctx, r.ID, read(id26("L"), `{}`))
					if err != nil {
						assert.ErrorIs(t, err, store.ErrNotFound)
					}
				})
				wg.Go(func() {
					err := f.db.StartFoundryWorkflowRun(ctx, &model.FoundryWorkflowRun{
						ID: id26(fmt.Sprintf("FR-R%dS%02dX", round, i)), WorkflowID: g.ID, WorkspaceID: g.WorkspaceID, OntologyID: g.OntologyID,
						ClientToken: fmt.Sprintf("race-%d-%d", round, i), StartedBy: r.StartedBy,
					})
					if err != nil {
						assert.ErrorIs(t, err, store.ErrNotFound)
					}
				})
			}
			wg.Go(func() { assert.NoError(t, f.db.DeleteFoundryWorkflow(ctx, f.ontology.ID, g.ID)) })
			wg.Wait()
			assert.Zero(t, countRows(t, "foundry_workflow_runs", "workflow_id=?", g.ID))
			assert.Zero(t, countRows(t, "foundry_workflow_run_accesses", "run_id=?", r.ID))
		}
	})
}

func TestFoundryWorkflowRunAssessments(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "WFAS")
	w := f.workflow(t, "A")
	in := "Who renews?"
	finished := func(tag string) *model.FoundryWorkflowRun {
		r := f.run(t, w, tag, &in)
		require.NoError(t, f.db.FinishFoundryWorkflowRun(ctx, r.ID, nil, "Acme.", true))
		return r
	}
	r := finished("A")
	queued, err := f.db.GetFoundryWorkflowRunAssessment(ctx, r.ID)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryWorkflowAssessmentQueued, queued.Status)
	assert.Equal(t, w.ID, queued.WorkflowID)

	t.Run("a run finished without one has none, and an open run can't be queued", func(t *testing.T) {
		plain := f.run(t, w, "PLAIN", &in)
		assert.ErrorIs(t, f.db.QueueFoundryWorkflowRunAssessment(ctx, plain.ID), store.ErrNotFound)
		require.NoError(t, f.db.FinishFoundryWorkflowRun(ctx, plain.ID, nil, "a", false))
		_, err := f.db.GetFoundryWorkflowRunAssessment(ctx, plain.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		require.NoError(t, f.db.QueueFoundryWorkflowRunAssessment(ctx, plain.ID))
		got, err := f.db.GetFoundryWorkflowRunAssessment(ctx, plain.ID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryWorkflowAssessmentQueued, got.Status)
		// Queued, it can't be queued again.
		assert.ErrorIs(t, f.db.QueueFoundryWorkflowRunAssessment(ctx, plain.ID), store.ErrConflict)
		_, err = testDB.Exec(`DELETE FROM foundry_workflow_run_assessments WHERE run_id=?`, plain.ID)
		require.NoError(t, err)
	})

	t.Run("a claim leases it, counts the attempt and finishes it once", func(t *testing.T) {
		_, err := f.db.ClaimFoundryWorkflowRunAssessment(ctx, "", time.Minute)
		assert.Error(t, err)
		claimed, err := f.db.ClaimFoundryWorkflowRunAssessment(ctx, "w1", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, r.ID, claimed.RunID)
		assert.Equal(t, model.FoundryWorkflowAssessmentRunning, claimed.Status)
		assert.Equal(t, 1, claimed.Attempts)
		_, err = f.db.ClaimFoundryWorkflowRunAssessment(ctx, "w2", time.Minute)
		assert.ErrorIs(t, err, store.ErrNotFound, "a leased one isn't claimed again")
		require.NoError(t, f.db.RenewFoundryWorkflowRunAssessment(ctx, r.ID, "w1", time.Minute))
		assert.ErrorIs(t, f.db.RenewFoundryWorkflowRunAssessment(ctx, r.ID, "w2", time.Minute), store.ErrNotFound)
		assert.ErrorIs(t, f.db.QueueFoundryWorkflowRunAssessment(ctx, r.ID), store.ErrConflict)

		// A retry waits its time, and keeps what it spent.
		require.NoError(t, f.db.FinishFoundryWorkflowRunAssessment(ctx, r.ID, "w1", model.FoundryWorkflowAssessmentOutcome{
			Status: model.FoundryWorkflowAssessmentQueued, Error: "unavailable", USDMicros: 100, RetryAt: time.Now().Add(time.Hour),
		}))
		_, err = f.db.ClaimFoundryWorkflowRunAssessment(ctx, "w1", time.Minute)
		assert.ErrorIs(t, err, store.ErrNotFound, "not before its retry")
		_, err = testDB.Exec(`UPDATE foundry_workflow_run_assessments SET next_attempt_at=NOW(6) WHERE run_id=?`, r.ID)
		require.NoError(t, err)
		claimed, err = f.db.ClaimFoundryWorkflowRunAssessment(ctx, "w1", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, 2, claimed.Attempts)

		done := model.FoundryWorkflowAssessmentOutcome{Status: model.FoundryWorkflowAssessmentDone, Verdict: "helpful",
			Assessment: json.RawMessage(`{"verdict":"helpful"}`), LeftOut: []int32{3}, USDMicros: 250}
		assert.ErrorIs(t, f.db.FinishFoundryWorkflowRunAssessment(ctx, r.ID, "w2", done), store.ErrNotFound)
		require.NoError(t, f.db.FinishFoundryWorkflowRunAssessment(ctx, r.ID, "w1", done))
		assert.ErrorIs(t, f.db.FinishFoundryWorkflowRunAssessment(ctx, r.ID, "w1", done), store.ErrNotFound)
		got, err := f.db.GetFoundryWorkflowRunAssessment(ctx, r.ID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryWorkflowAssessmentDone, got.Status)
		assert.Equal(t, "helpful", got.Verdict)
		assert.JSONEq(t, `{"verdict":"helpful"}`, string(got.Assessment))
		assert.Equal(t, []int32{3}, got.LeftOut)
		assert.EqualValues(t, 350, got.USDMicros)
		require.NotNil(t, got.FinishedAt)

		spent, err := f.db.SumFoundryWorkflowAssessmentSpend(ctx, f.ontology.WorkspaceID, time.Now().Add(-time.Hour))
		require.NoError(t, err)
		assert.EqualValues(t, 350, spent)
		spent, err = f.db.SumFoundryWorkflowAssessmentSpend(ctx, f.ontology.WorkspaceID, time.Now().AddDate(0, 1, 0))
		require.NoError(t, err)
		assert.Zero(t, spent, "next month has spent nothing")
	})

	t.Run("an attempt given back isn't counted", func(t *testing.T) {
		back := finished("BACK")
		claimed, err := f.db.ClaimFoundryWorkflowRunAssessment(ctx, "w1", time.Minute)
		require.NoError(t, err)
		require.Equal(t, back.ID, claimed.RunID)
		require.NoError(t, f.db.FinishFoundryWorkflowRunAssessment(ctx, back.ID, "w1", model.FoundryWorkflowAssessmentOutcome{
			Status: model.FoundryWorkflowAssessmentQueued, Error: "no Assess yet", RetryAt: time.Now(), Uncounted: true,
		}))
		got, err := f.db.GetFoundryWorkflowRunAssessment(ctx, back.ID)
		require.NoError(t, err)
		assert.Zero(t, got.Attempts)
		_, err = testDB.Exec(`DELETE FROM foundry_workflow_run_assessments WHERE run_id=?`, back.ID)
		require.NoError(t, err)
	})

	t.Run("a lapsed lease is claimed again", func(t *testing.T) {
		lapsed := finished("LAPSED")
		claimed, err := f.db.ClaimFoundryWorkflowRunAssessment(ctx, "w1", time.Millisecond)
		require.NoError(t, err)
		require.Equal(t, lapsed.ID, claimed.RunID)
		time.Sleep(5 * time.Millisecond)
		again, err := f.db.ClaimFoundryWorkflowRunAssessment(ctx, "w2", time.Minute)
		require.NoError(t, err)
		assert.Equal(t, lapsed.ID, again.RunID)
		assert.Equal(t, 2, again.Attempts)
		assert.ErrorIs(t, f.db.RenewFoundryWorkflowRunAssessment(ctx, lapsed.ID, "w1", time.Minute), store.ErrNotFound)
		require.NoError(t, f.db.FinishFoundryWorkflowRunAssessment(ctx, lapsed.ID, "w2", model.FoundryWorkflowAssessmentOutcome{
			Status: model.FoundryWorkflowAssessmentSkipped, Error: "the month's assessments are spent",
		}))
		got, err := f.db.GetFoundryWorkflowRunAssessment(ctx, lapsed.ID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryWorkflowAssessmentSkipped, got.Status)
		assert.Equal(t, "the month's assessments are spent", got.Error)
		// Skipped, it can be queued again, as new.
		require.NoError(t, f.db.QueueFoundryWorkflowRunAssessment(ctx, lapsed.ID))
		got, err = f.db.GetFoundryWorkflowRunAssessment(ctx, lapsed.ID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryWorkflowAssessmentQueued, got.Status)
		assert.Zero(t, got.Attempts)
		assert.Empty(t, got.Error)
		assert.Nil(t, got.FinishedAt)
	})

	t.Run("outcomes the table can't hold are refused", func(t *testing.T) {
		for _, o := range []model.FoundryWorkflowAssessmentOutcome{
			{Status: model.FoundryWorkflowAssessmentRunning},
			{Status: model.FoundryWorkflowAssessmentFailed, Error: "x", Uncounted: true},
			{Status: model.FoundryWorkflowAssessmentDone, Verdict: "great", Assessment: json.RawMessage(`{}`)},
			{Status: model.FoundryWorkflowAssessmentDone, Verdict: "helpful"},
			{Status: model.FoundryWorkflowAssessmentFailed},
			{Status: model.FoundryWorkflowAssessmentFailed, Error: "x", USDMicros: -1},
		} {
			assert.Error(t, f.db.FinishFoundryWorkflowRunAssessment(ctx, r.ID, "w1", o), "%+v", o)
		}
	})

	t.Run("verdicts count by workflow, and assessments go with their runs", func(t *testing.T) {
		counts, err := f.db.CountFoundryWorkflowVerdicts(ctx, []string{w.ID, id26("FW-NONE")})
		require.NoError(t, err)
		assert.Equal(t, map[string]map[string]int64{w.ID: {"helpful": 1}}, counts)
		empty, err := f.db.CountFoundryWorkflowVerdicts(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, empty)
		listed, err := f.db.ListFoundryWorkflowRunAssessments(ctx, []string{r.ID, id26("FR-NONE")})
		require.NoError(t, err)
		assert.Len(t, listed, 1)
		none, err := f.db.ListFoundryWorkflowRunAssessments(ctx, nil)
		require.NoError(t, err)
		assert.Empty(t, none)

		require.NoError(t, f.db.DeleteFoundryWorkflow(ctx, f.ontology.ID, w.ID))
		assert.Zero(t, countRows(t, "foundry_workflow_run_assessments", "workflow_id=?", w.ID))
		// What they spent stays the month's.
		spent, err := f.db.SumFoundryWorkflowAssessmentSpend(ctx, f.ontology.WorkspaceID, time.Now())
		require.NoError(t, err)
		assert.EqualValues(t, 350, spent)
	})
}

// TestFoundryWorkflowPrunesSpareAgentsAtWork keeps a run an agent started in
// the last day while it is open, past the cap, and prunes one left open
// longer, and finished ones.
func TestFoundryWorkflowPrunesSpareAgentsAtWork(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "WFP")
	w := f.workflow(t, "A")
	fresh := f.run(t, w, "FRESH", nil)
	stale := f.run(t, w, "STALE", nil)
	_, err := testDB.Exec(`UPDATE foundry_workflow_runs SET started_at=NOW(6) - INTERVAL 3 DAY WHERE id=?`, stale.ID)
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_workflow_runs SET started_at=NOW(6) - INTERVAL 1 HOUR WHERE id=?`, fresh.ID)
	require.NoError(t, err)
	var values []string
	var args []any
	for i := 1; i < model.FoundryWorkflowRunsKept; i++ {
		values = append(values, "(?, ?, ?, ?, 'finished', '', '', ?, ?, NOW(6) + INTERVAL ? MICROSECOND, NOW(6))")
		args = append(args, id26(fmt.Sprintf("FR-P%04dX", i)), w.ID, w.WorkspaceID, w.OntologyID, fmt.Sprintf("p%d", i), id26("U-ADA"), i)
	}
	_, err = testDB.Exec(`INSERT INTO foundry_workflow_runs
  (id, workflow_id, workspace_id, ontology_id, status, input, output, client_token, started_by_user_id, started_at, finished_at) VALUES `+
		strings.Join(values, ", "), args...)
	require.NoError(t, err)
	f.run(t, w, "NEWEST", nil)
	_, err = f.db.GetFoundryWorkflowRun(ctx, f.ontology.ID, fresh.ID)
	assert.NoError(t, err, "an agent at work keeps its run")
	_, err = f.db.GetFoundryWorkflowRun(ctx, f.ontology.ID, stale.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "a run left open for days goes")
}

// TestFoundryWorkflowPurgesInBatches deletes more accesses than one purge
// statement takes.
func TestFoundryWorkflowPurgesInBatches(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "WFG")
	w := f.workflow(t, "A")
	var runs []string
	for i := range 6 {
		r := f.run(t, w, fmt.Sprintf("G%dX", i), nil)
		runs = append(runs, r.ID)
		var values []string
		var args []any
		for seq := 1; seq <= 1000; seq++ {
			values = append(values, `(?, ?, 'read', '{"load_id":"l","reads":[]}')`)
			args = append(args, r.ID, seq)
		}
		_, err := testDB.Exec(`INSERT INTO foundry_workflow_run_accesses (run_id, seq, kind, detail) VALUES `+strings.Join(values, ", "), args...)
		require.NoError(t, err)
	}
	require.NoError(t, f.db.DeleteFoundryWorkflow(ctx, f.ontology.ID, w.ID))
	assert.Zero(t, countRows(t, "foundry_workflow_run_accesses", "run_id IN (?, ?, ?, ?, ?, ?)", toAny(runs)...))
	assert.Zero(t, countRows(t, "foundry_workflow_runs", "workflow_id=?", w.ID))
}

func toAny(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// TestFoundryWorkflowInsightRuns reads the runs that started in a window,
// newest first, of a workflow and a starter when asked, each with its
// assessment, if any.
func TestFoundryWorkflowInsightRuns(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "WFI")
	a, b := f.workflow(t, "IA"), f.workflow(t, "IB")
	in := "q"
	at := func(r *model.FoundryWorkflowRun, when time.Time) {
		t.Helper()
		_, err := testDB.Exec(`UPDATE foundry_workflow_runs SET started_at=? WHERE id=?`, when, r.ID)
		require.NoError(t, err)
	}
	day := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	before := f.run(t, a, "I-BEFORE", &in)
	at(before, day.Add(-time.Millisecond))
	first := f.run(t, a, "I-FIRST", &in)
	at(first, day)
	key := &model.FoundryWorkflowRun{
		ID: id26("FR-I-KEY"), WorkflowID: a.ID, WorkspaceID: a.WorkspaceID, OntologyID: a.OntologyID, ClientToken: "i-key",
		StartedBy: access.Principal{Kind: access.PrincipalServiceCredential, ID: id26("SC-I")},
	}
	require.NoError(t, f.db.StartFoundryWorkflowRun(ctx, key))
	at(key, day.Add(33*time.Hour))
	other := f.run(t, b, "I-OTHER", &in)
	at(other, day.Add(36*time.Hour))
	after := f.run(t, a, "I-AFTER", &in)
	at(after, day.AddDate(0, 0, 3))

	require.NoError(t, f.db.FinishFoundryWorkflowRun(ctx, first.ID, nil, "answer", false))
	_, err := testDB.Exec(`UPDATE foundry_workflow_runs SET accesses=2, reads_count=1, questions=1 WHERE id=?`, first.ID)
	require.NoError(t, err)
	_, err = testDB.Exec(`INSERT INTO foundry_workflow_run_assessments (run_id, workspace_id, ontology_id, workflow_id, status, verdict, assessment, usd_micros, finished_at)
VALUES (?, ?, ?, ?, 'done', 'helpful', '{"verdict":"helpful"}', 123, NOW(6)), (?, ?, ?, ?, 'queued', NULL, NULL, 0, NULL)`,
		first.ID, a.WorkspaceID, a.OntologyID, a.ID, key.ID, a.WorkspaceID, a.OntologyID, a.ID)
	require.NoError(t, err)

	ids := func(runs []model.FoundryWorkflowInsightRun) []string {
		var out []string
		for _, r := range runs {
			out = append(out, r.ID)
		}
		return out
	}
	window := func(workflowID string, by *access.Principal, limit int) []model.FoundryWorkflowInsightRun {
		t.Helper()
		runs, err := f.db.ListFoundryWorkflowInsightRuns(ctx, f.ontology.ID, workflowID, by, day, day.AddDate(0, 0, 3), limit)
		require.NoError(t, err)
		return runs
	}
	all := window("", nil, 10)
	assert.Equal(t, []string{other.ID, key.ID, first.ID}, ids(all), "newest first; the instant the window starts is in it, its end isn't")
	got := all[2]
	assert.Equal(t, model.FoundryWorkflowRunFinished, got.Status)
	assert.Equal(t, a.ID, got.WorkflowID)
	assert.Equal(t, access.Principal{Kind: access.PrincipalUser, ID: id26("U-ADA")}, got.StartedBy)
	assert.True(t, got.StartedAt.Equal(day))
	assert.Equal(t, model.FoundryWorkflowRunCounts{Accesses: 2, Reads: 1, Questions: 1}, got.Counts)
	require.NotNil(t, got.Assessment)
	assert.Equal(t, model.FoundryWorkflowAssessmentDone, got.Assessment.Status)
	assert.Equal(t, "helpful", got.Assessment.Verdict)
	assert.JSONEq(t, `{"verdict":"helpful"}`, string(got.Assessment.Assessment))
	assert.EqualValues(t, 123, got.Assessment.USDMicros)
	require.NotNil(t, all[1].Assessment)
	assert.Equal(t, model.FoundryWorkflowAssessmentQueued, all[1].Assessment.Status)
	assert.Empty(t, all[1].Assessment.Verdict)
	assert.Equal(t, model.FoundryWorkflowRunOpen, all[1].Status)
	assert.Equal(t, key.StartedBy, all[1].StartedBy)
	assert.Nil(t, all[0].Assessment)

	assert.Equal(t, []string{key.ID, first.ID}, ids(window(a.ID, nil, 10)), "one workflow's")
	assert.Equal(t, []string{key.ID}, ids(window("", &key.StartedBy, 10)), "one starter's")
	ada := access.Principal{Kind: access.PrincipalUser, ID: id26("U-ADA")}
	assert.Equal(t, []string{other.ID, first.ID}, ids(window("", &ada, 10)))
	assert.Equal(t, []string{other.ID}, ids(window("", nil, 1)), "the newest, up to limit")
	none, err := f.db.ListFoundryWorkflowInsightRuns(ctx, id26("ONT-NONE"), "", nil, day, day.AddDate(0, 0, 3), 10)
	require.NoError(t, err)
	assert.Empty(t, none, "another ontology's")
}
