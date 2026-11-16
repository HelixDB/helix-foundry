package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/automation"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

func mustSpec(t *testing.T, raw json.RawMessage) automation.Spec {
	t.Helper()
	spec, err := automation.ParseSpec(raw)
	require.NoError(t, err)
	return spec
}

// An automation's Assistant step records its reads and questions of an
// ontology under the workflow run of its run's use of it, started at the
// first: Foundry names the run, whatever the model wrote.
func TestAnAutomationsCallsAreRecordedUnderItsWorkflowRun(t *testing.T) {
	h := newHarness(t,
		call("toolu_1", toolAsk, `{"workspace_id":"ws1","ontology_id":"ont1","question":"Who renews?","client_token":"c","workflow_run_id":"forged"}`),
		call("toolu_2", toolAsk, `{"workspace_id":"ws1","ontology_id":"ont1","question":"And when?","client_token":"d"}`),
		call("toolu_3", toolAsk, `{"workspace_id":"ws1","ontology_id":"ont9","question":"Theirs?","client_token":"e","workflow_run_id":"forged"}`),
		call("toolu_4", toolPrepare, `{"operation":"create_load","target":"ontology:ws1/ont1","payload":{"version_id":"ver3"}}`),
		say("Done."))
	turn := automationTurn(h, loadOnt1)
	h.db.nodes = map[string][]*model.FoundryAutomationRunNode{"run1": {{RunID: "run1", NodeID: "start", Output: json.RawMessage(`{"summary":"Run by hand."}`)}}}
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)

	require.Len(t, h.backend.asked, 3)
	for _, asked := range h.backend.asked[:2] {
		assert.Equal(t, "wfrun-ont1_key-ont1", asked.GetWorkflowRunId(), "recorded under its run's use of the ontology, with its key")
	}
	assert.Empty(t, h.backend.asked[2].GetWorkflowRunId(), "an ontology the workspace hasn't goes untracked, and nothing the model wrote is sent")
	require.Len(t, h.db.uses.starts, 1, "a run uses an ontology once; another workspace's goes untracked")
	start := h.db.uses.starts[0]
	assert.Equal(t, model.FoundryAutomationWorkflowStart{WorkspaceID: ws, AutomationRunID: "run1", OntologyID: ontology, Input: start.Input}, start)
	assert.Contains(t, start.Input, `Run by hand. The automation "Nightly load" ran these steps, in order:`)
	assert.Contains(t, start.Input, `1. Founder step "ask": Load the Sales graph (It may load the current version of the ontology "Sales graph" unasked.)`)
	assert.Contains(t, start.Input, `2. Notify step "mail": email its owner, with what the Founder steps before it said.`)
	assert.Equal(t, 1, h.backend.loads, "the approved load ran")

	for _, tool := range h.model.requests[0].Tools {
		assert.NotContains(t, string(tool.InputSchema), workflowRunField, "%s: Foundry names it, never the model", tool.Name)
	}
}

func TestAPersonsTurnRecordsUnderNoWorkflowRun(t *testing.T) {
	h := newHarness(t,
		call("toolu_1", toolAsk, `{"workspace_id":"ws1","ontology_id":"ont1","question":"Who?","client_token":"c","workflow_run_id":"someones"}`),
		say("Ada."))
	_, err := h.runner.Run(context.Background(), message("Who?"))
	require.NoError(t, err)
	require.Len(t, h.backend.asked, 1)
	assert.Empty(t, h.backend.asked[0].GetWorkflowRunId(), "a model never names one")
	assert.Empty(t, h.db.uses.starts)
}

// A use that can't be recorded still runs: tracking never stops a step.
func TestAnUntrackedUseStillRuns(t *testing.T) {
	h := newHarness(t,
		call("toolu_1", toolAsk, `{"workspace_id":"ws1","ontology_id":"ont1","question":"Who?","client_token":"c"}`),
		call("toolu_2", toolAsk, `{"workspace_id":"ws1","ontology_id":"ont1","question":"Who else?","client_token":"d"}`),
		say("Ada."))
	turn := automationTurn(h, loadOnt1)
	h.db.uses.full = map[string]bool{ontology: true}
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	require.Len(t, h.backend.asked, 2)
	assert.Empty(t, h.backend.asked[0].GetWorkflowRunId())
	assert.Empty(t, h.db.uses.starts)
}

func TestUsesOfAnEndedRunAreUntracked(t *testing.T) {
	db := &memStore{}
	runID := "run1"
	finished := &model.FoundryWorkflowRunEnd{Output: "Done."}
	db.uses.runs = []*model.FoundryWorkflowRun{{ID: "wfrun-ont1", OntologyID: ontology, AutomationRunID: &runID, Finished: finished}}
	uses := NewAutomationUses(db, &model.FoundryAutomationRun{ID: runID, WorkspaceID: ws}, "Nightly", mustSpec(t, stepSpec(loadOnt1)))
	got, err := uses.Use(context.Background(), ontology)
	require.NoError(t, err)
	assert.Empty(t, got)
	args := map[string]any{"ontology_id": ontology, workflowRunField: "x"}
	require.NoError(t, uses.Called(context.Background(), toolAsk, args))
	assert.NotContains(t, args, workflowRunField)
	assert.Empty(t, uses.prepared(context.Background(), map[string]any{"target": "ontology:ws2/ont1"}), "another workspace's")
	assert.Empty(t, uses.prepared(context.Background(), map[string]any{"target": "connector:ws1/c1"}))
	assert.Empty(t, uses.DashboardOntologies(context.Background(), "none"))
}

func TestWithoutWorkflowRun(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"ontology_id":{"type":"string"},"workflow_run_id":{"type":"string"}},"required":["ontology_id","workflow_run_id"]}`)
	assert.JSONEq(t, `{"type":"object","properties":{"ontology_id":{"type":"string"}},"required":["ontology_id"]}`, string(withoutWorkflowRun(schema)))
	plain := json.RawMessage(`{"type":"object","properties":{"ontology_id":{"type":"string"}}}`)
	assert.Equal(t, plain, withoutWorkflowRun(plain))
	assert.Equal(t, json.RawMessage(`[`), withoutWorkflowRun(json.RawMessage(`[`)))
}

func TestCutText(t *testing.T) {
	assert.Equal(t, "short", CutText("short", 10))
	assert.Equal(t, "h…", CutText("héllo", 4))
	assert.Equal(t, "hé…", CutText("héllo!", 6))
}
