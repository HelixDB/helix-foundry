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

// automationWorkflowFixture is a workspace with two ontologies where Ada's
// automation 1 runs.
type automationWorkflowFixture struct {
	automationFixture
	sales, contracts *model.FoundryOntology
	automation       *model.FoundryAutomation
}

func newAutomationWorkflowFixture(t *testing.T, tag string) automationWorkflowFixture {
	t.Helper()
	f := automationWorkflowFixture{automationFixture: newAutomationFixture(t, tag)}
	f.sales = sampleOntology(t, f.db, f.workspaceID, tag+"S")
	f.contracts = sampleOntology(t, f.db, f.workspaceID, tag+"C")
	f.automation = f.active(t, 1, "manual", nil)
	return f
}

// claim runs automation 1 by hand (run n) and claims it for w1.
func (f automationWorkflowFixture) claim(t *testing.T, n int) *model.FoundryAutomationRun {
	t.Helper()
	ctx := context.Background()
	_, err := f.db.RunFoundryAutomationNow(ctx, f.run(n, 1, fmt.Sprintf("manual:%d", n)), "start", nil, model.FoundryAutomationLimits{RunsPerDay: 100})
	require.NoError(t, err)
	run, err := f.db.ClaimFoundryAutomationRun(ctx, "w1", time.Minute)
	require.NoError(t, err)
	require.Equal(t, f.runID(n), run.ID)
	return run
}

// use starts the workflow run recording run's use of ontology.
func (f automationWorkflowFixture) use(run *model.FoundryAutomationRun, ontology *model.FoundryOntology) (*model.FoundryWorkflowRun, error) {
	return f.db.StartFoundryAutomationWorkflowRun(context.Background(), model.FoundryAutomationWorkflowStart{WorkspaceID: f.workspaceID,
		AutomationRunID: run.ID, OntologyID: ontology.ID, Input: "Run by hand. The workflow \"Automation 1\" ran these steps, in order:"})
}

func TestFoundryAutomationWorkflowRuns(t *testing.T) {
	ctx := context.Background()
	f := newAutomationWorkflowFixture(t, "AWR")
	first := f.claim(t, 1)
	// A person's workflow whatever its client token: it never stands in the
	// automation's way.
	require.NoError(t, f.db.CreateFoundryWorkflow(ctx, &model.FoundryWorkflow{ID: id26("FW-SQUAT"), WorkspaceID: f.workspaceID, OntologyID: f.sales.ID,
		Name: "Squat", ClientToken: "automation:" + f.automation.ID, CreatedByUserID: automationOther}))

	_, err := f.db.GetFoundryAutomationWorkflowRun(ctx, first.ID, f.sales.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "no use yet")
	r, err := f.use(first, f.sales)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryWorkflowRunOpen, r.Status())
	assert.Regexp(t, `^[0-9a-f]{32}$`, r.AutomationKey, "the key its calls name it with")
	assert.Equal(t, r.ID+"_"+r.AutomationKey, r.Recording())
	assert.Equal(t, access.Principal{Kind: access.PrincipalUser, ID: automationOwner}, r.StartedBy, "the owner's")
	assert.Equal(t, &first.ID, r.AutomationRunID)
	assert.Equal(t, first.ID, r.ClientToken)
	require.NotNil(t, r.Input)
	assert.Contains(t, *r.Input, "Run by hand.")

	w, err := f.db.GetFoundryWorkflow(ctx, f.sales.ID, r.WorkflowID)
	require.NoError(t, err)
	assert.Equal(t, "Automation 1", w.Name, "named after the automation")
	assert.Equal(t, model.FoundryAutomationWorkflowDescription, w.Description)
	assert.Equal(t, automationOwner, w.CreatedByUserID)
	assert.Equal(t, &model.FoundryWorkflowAutomation{ID: f.automation.ID, Name: "Automation 1"}, w.Automation)

	again, err := f.use(first, f.sales)
	require.NoError(t, err)
	assert.Equal(t, r.ID, again.ID, "a run uses an ontology once")
	assert.Equal(t, r.AutomationKey, again.AutomationKey)
	got, err := f.db.GetFoundryAutomationWorkflowRun(ctx, first.ID, f.sales.ID)
	require.NoError(t, err)
	assert.Equal(t, r.ID, got.ID)
	other, err := f.use(first, f.contracts)
	require.NoError(t, err)
	assert.NotEqual(t, r.WorkflowID, other.WorkflowID, "a workflow per ontology")

	// Its accesses record under it, as any run's.
	_, err = f.db.RecordFoundryWorkflowAccess(ctx, r.ID, &model.FoundryWorkflowQuestion{LoadID: id26("L1"), QuestionID: id26("Q1")})
	require.NoError(t, err)

	// The run's end finishes both with what it found, assessing the one
	// that made a call: the other has nothing to assess.
	require.NoError(t, f.db.FinishFoundryAutomationRun(ctx, first.ID, "w1", model.FoundryAutomationRunEnd{Status: model.FoundryAutomationRunSucceeded,
		PauseAfter: 3, Answer: "The run succeeded.\nWhat its steps found: 3 overdue invoices.", Assess: true}))
	for _, o := range []*model.FoundryOntology{f.sales, f.contracts} {
		ended, err := f.db.GetFoundryAutomationWorkflowRun(ctx, first.ID, o.ID)
		require.NoError(t, err)
		require.NotNil(t, ended.Finished)
		assert.Contains(t, ended.Finished.Output, "3 overdue invoices")
		a, err := f.db.GetFoundryWorkflowRunAssessment(ctx, ended.ID)
		if o == f.contracts {
			assert.ErrorIs(t, err, store.ErrNotFound, "no call, no assessment")
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, model.FoundryWorkflowAssessmentQueued, a.Status)
	}
	_, err = f.use(first, f.sales)
	require.ErrorIs(t, err, store.ErrNotFound, "an ended run uses nothing more")

	// The next run reuses the workflow with a run of its own.
	second := f.claim(t, 2)
	next, err := f.use(second, f.sales)
	require.NoError(t, err)
	assert.Equal(t, r.WorkflowID, next.WorkflowID)
	assert.NotEqual(t, r.ID, next.ID)
	w, err = f.db.GetFoundryWorkflow(ctx, f.sales.ID, r.WorkflowID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), w.Runs)

	used, err := f.db.ListFoundryAutomationRunOntologies(ctx, first.ID)
	require.NoError(t, err)
	require.Len(t, used, 2)
	assert.Equal(t, "Ontology AWRC", used[0].OntologyName, "by name")
	assert.Equal(t, f.sales.ID, used[1].OntologyID)
	assert.Equal(t, r.ID, used[1].Run.ID)
	require.NotNil(t, used[1].Assessment)
	assert.Equal(t, model.FoundryWorkflowAssessmentQueued, used[1].Assessment.Status)

	t.Run("a run that isn't live, or an ontology that isn't the workspace's, starts none", func(t *testing.T) {
		_, err := f.db.StartFoundryAutomationWorkflowRun(ctx, model.FoundryAutomationWorkflowStart{WorkspaceID: f.workspaceID,
			AutomationRunID: f.runID(99), OntologyID: f.sales.ID, Input: "x"})
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = f.db.StartFoundryAutomationWorkflowRun(ctx, model.FoundryAutomationWorkflowStart{WorkspaceID: id26("UWS-OTHER"),
			AutomationRunID: second.ID, OntologyID: f.sales.ID, Input: "x"})
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = f.db.StartFoundryAutomationWorkflowRun(ctx, model.FoundryAutomationWorkflowStart{WorkspaceID: f.workspaceID,
			AutomationRunID: second.ID, OntologyID: id26("FO-NONE"), Input: "x"})
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = f.db.StartFoundryAutomationWorkflowRun(ctx, model.FoundryAutomationWorkflowStart{WorkspaceID: f.workspaceID,
			AutomationRunID: second.ID, OntologyID: f.sales.ID, Input: " "})
		assert.Error(t, err, "a run's request isn't blank")
		_, err = f.db.StartFoundryAutomationWorkflowRun(ctx, model.FoundryAutomationWorkflowStart{WorkspaceID: f.workspaceID,
			AutomationRunID: second.ID, OntologyID: f.sales.ID, Input: strings.Repeat("x", model.FoundryWorkflowInputMaxBytes+1)})
		assert.Error(t, err, "nor too long")
	})

	t.Run("cancelling a run finishes its workflow runs unassessed", func(t *testing.T) {
		_, err := f.db.CancelFoundryAutomationRun(ctx, f.workspaceID, second.ID, model.FoundryAutomationActor{UserID: automationOwner})
		require.NoError(t, err)
		ended, err := f.db.GetFoundryAutomationWorkflowRun(ctx, second.ID, f.sales.ID)
		require.NoError(t, err)
		require.NotNil(t, ended.Finished)
		assert.Equal(t, "A person cancelled the workflow's run before it finished.", ended.Finished.Output)
		_, err = f.db.GetFoundryWorkflowRunAssessment(ctx, ended.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("its workflows are named after it as it is renamed", func(t *testing.T) {
		long := strings.Repeat("é", 120)
		a, err := f.db.UpdateFoundryAutomation(ctx, f.workspaceID, f.automation.ID, automationOwner, f.automation.Revision,
			model.FoundryAutomationUpdate{Name: &long})
		require.NoError(t, err)
		f.automation = a
		w, err := f.db.GetFoundryWorkflow(ctx, f.sales.ID, r.WorkflowID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryAutomationWorkflowName(long), w.Name)
		assert.Len(t, []rune(w.Name), model.FoundryWorkflowNameMax)
		assert.True(t, strings.HasSuffix(w.Name, "…"))
	})

	t.Run("its workflow is deleted only once it is, and stays as history", func(t *testing.T) {
		third := f.claim(t, 3)
		live, err := f.use(third, f.sales)
		require.NoError(t, err)
		require.ErrorIs(t, f.db.DeleteFoundryWorkflow(ctx, f.sales.ID, r.WorkflowID), store.ErrAutomationWorkflow)
		require.NoError(t, f.db.DeleteFoundryAutomation(ctx, f.workspaceID, f.automation.ID, model.FoundryAutomationActor{UserID: automationOwner}))
		ended, err := f.db.GetFoundryWorkflowRun(ctx, f.sales.ID, live.ID)
		require.NoError(t, err)
		require.NotNil(t, ended.Finished, "its live run's workflow run finished with it")
		assert.Equal(t, "The workflow was deleted before its run finished.", ended.Finished.Output)
		w, err := f.db.GetFoundryWorkflow(ctx, f.sales.ID, r.WorkflowID)
		require.NoError(t, err)
		require.NotNil(t, w.Automation)
		assert.True(t, w.Automation.Deleted)
		assert.Equal(t, int64(3), w.Runs, "its runs stay")
		require.NoError(t, f.db.DeleteFoundryWorkflow(ctx, f.sales.ID, r.WorkflowID))
		_, err = f.db.GetFoundryWorkflow(ctx, f.sales.ID, r.WorkflowID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		assert.ErrorIs(t, f.db.DeleteFoundryWorkflow(ctx, f.sales.ID, r.WorkflowID), store.ErrNotFound)
	})
}

func TestFoundryAutomationWorkflowsFull(t *testing.T) {
	ctx := context.Background()
	f := newAutomationWorkflowFixture(t, "AWF")
	for i := range model.FoundryWorkflowsMax {
		require.NoError(t, f.db.CreateFoundryWorkflow(ctx, &model.FoundryWorkflow{ID: id26(fmt.Sprintf("FWFULL%03d", i)), WorkspaceID: f.workspaceID,
			OntologyID: f.sales.ID, Name: "w", ClientToken: fmt.Sprintf("full-%d", i), CreatedByUserID: automationOther}))
	}
	run := f.claim(t, 1)
	_, err := f.use(run, f.sales)
	require.ErrorIs(t, err, store.ErrLimitExceeded)
	_, err = f.db.EnsureFoundryAutomationWorkflow(ctx, f.workspaceID, f.automation.ID, f.sales.ID)
	require.ErrorIs(t, err, store.ErrLimitExceeded)
	tracked, err := f.use(run, f.contracts)
	require.NoError(t, err)

	all, err := f.db.ListFoundryAutomationOntologies(ctx, f.workspaceID, []string{f.automation.ID, f.id(9)})
	require.NoError(t, err)
	require.Len(t, all[f.automation.ID], 2)
	assert.Empty(t, all[f.id(9)])
	contracts, sales := all[f.automation.ID][0], all[f.automation.ID][1]
	assert.Equal(t, f.contracts.ID, contracts.OntologyID)
	got, ok := contracts.Tracking.(model.FoundryAutomationTracked)
	require.True(t, ok)
	assert.Equal(t, tracked.WorkflowID, got.Workflow.ID)
	assert.Equal(t, int64(1), got.Workflow.Runs)
	assert.Empty(t, got.Verdicts)
	untracked, ok := sales.Tracking.(model.FoundryAutomationUntracked)
	require.True(t, ok, "the full ontology says why")
	assert.Equal(t, model.FoundryAutomationWorkflowsFull, untracked.Reason)

	// Room again, the next use makes it and clears why.
	require.NoError(t, f.db.DeleteFoundryWorkflow(ctx, f.sales.ID, id26("FWFULL000")))
	w, err := f.db.EnsureFoundryAutomationWorkflow(ctx, f.workspaceID, f.automation.ID, f.sales.ID)
	require.NoError(t, err)
	same, err := f.db.EnsureFoundryAutomationWorkflow(ctx, f.workspaceID, f.automation.ID, f.sales.ID)
	require.NoError(t, err)
	assert.Equal(t, w.ID, same.ID, "one workflow an ontology")
	all, err = f.db.ListFoundryAutomationOntologies(ctx, f.workspaceID, []string{f.automation.ID})
	require.NoError(t, err)
	for _, o := range all[f.automation.ID] {
		_, ok := o.Tracking.(model.FoundryAutomationTracked)
		assert.True(t, ok, o.OntologyName)
	}

	_, err = f.db.EnsureFoundryAutomationWorkflow(ctx, f.workspaceID, f.id(9), f.sales.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "no such automation")
	require.NoError(t, f.db.DeleteFoundryAutomation(ctx, f.workspaceID, f.automation.ID, model.FoundryAutomationActor{UserID: automationOwner}))
	assert.Zero(t, countRows(t, "foundry_automation_untracked", "automation_id=?", f.automation.ID))
}

func TestFoundryAutomationWorkflowVerdictsAndInsights(t *testing.T) {
	ctx := context.Background()
	f := newAutomationWorkflowFixture(t, "AWV")
	var runs []*model.FoundryWorkflowRun
	for n, verdict := range []string{"helpful", "unused"} {
		run := f.claim(t, n+1)
		r, err := f.use(run, f.sales)
		require.NoError(t, err)
		_, err = f.db.RecordFoundryWorkflowAccess(ctx, r.ID, &model.FoundryWorkflowQuestion{LoadID: id26("L1"), QuestionID: id26("Q1")})
		require.NoError(t, err)
		require.NoError(t, f.db.FinishFoundryAutomationRun(ctx, run.ID, "w1", model.FoundryAutomationRunEnd{Status: model.FoundryAutomationRunSucceeded,
			PauseAfter: 3, Answer: "Done.", Assess: true}))
		claimed, err := f.db.ClaimFoundryWorkflowRunAssessment(ctx, "a1", time.Minute)
		require.NoError(t, err)
		require.Equal(t, r.ID, claimed.RunID)
		require.NoError(t, f.db.FinishFoundryWorkflowRunAssessment(ctx, r.ID, "a1", model.FoundryWorkflowAssessmentOutcome{
			Status: model.FoundryWorkflowAssessmentDone, Verdict: verdict,
			Assessment: json.RawMessage(fmt.Sprintf(`{"verdict":%q,"gaps":[{"kind":"missing_data","what":"gap %d"}]}`, verdict, n))}))
		runs = append(runs, r)
	}
	// A third that made no call: finishing queues none, and one assessed
	// before such runs went unqueued, newest though it is, counts nowhere.
	silentRun := f.claim(t, 3)
	silent, err := f.use(silentRun, f.sales)
	require.NoError(t, err)
	require.NoError(t, f.db.FinishFoundryAutomationRun(ctx, silentRun.ID, "w1", model.FoundryAutomationRunEnd{Status: model.FoundryAutomationRunSucceeded,
		PauseAfter: 3, Answer: "Done.", Assess: true}))
	_, err = f.db.GetFoundryWorkflowRunAssessment(ctx, silent.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = testDB.Exec(`INSERT INTO foundry_workflow_run_assessments (run_id, workspace_id, ontology_id, workflow_id, status, verdict, assessment, finished_at)
VALUES (?, ?, ?, ?, 'done', 'unused', '{"verdict":"unused","gaps":[{"kind":"other","what":"never called"}]}', NOW(6) + INTERVAL 1 SECOND)`,
		silent.ID, f.workspaceID, f.sales.ID, silent.WorkflowID)
	require.NoError(t, err)
	all, err := f.db.ListFoundryAutomationOntologies(ctx, f.workspaceID, []string{f.automation.ID})
	require.NoError(t, err)
	require.Len(t, all[f.automation.ID], 1)
	tracked := all[f.automation.ID][0].Tracking.(model.FoundryAutomationTracked)
	assert.Equal(t, map[string]int64{"helpful": 1, "unused": 1}, tracked.Verdicts)
	latest, err := f.db.LatestFoundryWorkflowAssessments(ctx, []string{tracked.Workflow.ID, id26("FW-NONE")})
	require.NoError(t, err)
	require.Len(t, latest, 1)
	assert.Contains(t, string(latest[tracked.Workflow.ID].Assessment), "gap 1", "the newest")

	// A person's own run of another workflow, beside the automation's.
	mine := &model.FoundryWorkflow{ID: id26("FW-MINE"), WorkspaceID: f.workspaceID, OntologyID: f.sales.ID, Name: "Mine", ClientToken: "mine",
		CreatedByUserID: automationOwner}
	require.NoError(t, f.db.CreateFoundryWorkflow(ctx, mine))
	require.NoError(t, f.db.StartFoundryWorkflowRun(ctx, &model.FoundryWorkflowRun{ID: id26("FR-MINE"), WorkflowID: mine.ID, WorkspaceID: f.workspaceID,
		OntologyID: f.sales.ID, ClientToken: "mine", StartedBy: access.Principal{Kind: access.PrincipalUser, ID: automationOwner}}))
	ada := &access.Principal{Kind: access.PrincipalUser, ID: automationOwner}
	day := time.Now().UTC().AddDate(0, 0, -1)
	every, err := f.db.ListFoundryWorkflowInsightRuns(ctx, f.sales.ID, "", ada, true, day, day.AddDate(0, 0, 3), 10)
	require.NoError(t, err)
	assert.Len(t, every, 4, "a grantee's own reads all it started")
	byHand, err := f.db.ListFoundryWorkflowInsightRuns(ctx, f.sales.ID, "", ada, false, day, day.AddDate(0, 0, 3), 10)
	require.NoError(t, err)
	require.Len(t, byHand, 1, "a principal asked for counts what it started itself")
	assert.Nil(t, byHand[0].Automation)
	persons, err := f.db.ListFoundryWorkflowInsightRuns(ctx, f.sales.ID, "", nil, false, day, day.AddDate(0, 0, 3), 10)
	require.NoError(t, err)
	require.Len(t, persons, 1, "without automations', as a grantee reads them")
	assert.Nil(t, persons[0].Automation)
	all3, err := f.db.ListFoundryWorkflowInsightRuns(ctx, f.sales.ID, "", nil, true, day, day.AddDate(0, 0, 3), 10)
	require.NoError(t, err)
	require.Len(t, all3, 4)
	automated := 0
	for _, r := range all3 {
		if r.Automation != nil {
			automated++
			assert.Equal(t, &model.FoundryWorkflowAutomation{ID: f.automation.ID, Name: "Automation 1"}, r.Automation)
		}
	}
	assert.Equal(t, 3, automated)
	assert.Len(t, runs, 2)

	listed, _, err := f.db.ListFoundryWorkflows(ctx, f.sales.ID, true, "", 10)
	require.NoError(t, err)
	assert.Len(t, listed, 2)
	listed, _, err = f.db.ListFoundryWorkflows(ctx, f.sales.ID, false, "", 10)
	require.NoError(t, err)
	require.Len(t, listed, 1, "without automations', as a grantee lists them")
	assert.Equal(t, mine.ID, listed[0].ID)
}

// Deleting the workspace ends its automations' live runs, and the workflow
// runs they started with them, unassessed.
func TestFoundryAutomationWorkflowRunsEndWithTheWorkspace(t *testing.T) {
	ctx := context.Background()
	f := newAutomationWorkflowFixture(t, "AWD")
	run := f.claim(t, 1)
	r, err := f.use(run, f.sales)
	require.NoError(t, err)
	require.NoError(t, f.db.WithTx(ctx, func(tx store.Store) error { return tx.CancelWorkspaceFoundryWork(ctx, f.workspaceID) }))
	ended, err := f.db.GetFoundryWorkflowRun(ctx, f.sales.ID, r.ID)
	require.NoError(t, err)
	require.NotNil(t, ended.Finished)
	assert.Equal(t, "The workspace was deleted before the run finished.", ended.Finished.Output)
	_, err = f.db.GetFoundryWorkflowRunAssessment(ctx, r.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestFoundryAutomationWorkflowRunStartsOnceUnderRaces(t *testing.T) {
	f := newAutomationWorkflowFixture(t, "AWX")
	run := f.claim(t, 1)
	var wg sync.WaitGroup
	ids := make([]string, 8)
	errs := make([]error, 8)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := f.use(run, f.sales)
			errs[i] = err
			if err == nil {
				ids[i] = r.ID
			}
		}()
	}
	wg.Wait()
	for i := range ids {
		require.NoError(t, errs[i])
		assert.Equal(t, ids[0], ids[i])
	}
	assert.Equal(t, 1, countRows(t, "foundry_workflows", "automation_id=?", f.automation.ID))
	assert.Equal(t, 1, countRows(t, "foundry_workflow_runs", "automation_run_id=?", run.ID))
}
