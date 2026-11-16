package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/agent/llm"
	"github.com/helixdb/helix-foundry/backend/automation"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// stepSpec is an automation whose Assistant step "ask" may run allow unasked.
func stepSpec(allow string) json.RawMessage {
	return json.RawMessage(`{"v":1,"nodes":[` +
		`{"id":"start","kind":"trigger","config":{"kind":"manual"},"position":{"x":0,"y":0}},` +
		`{"id":"ask","kind":"assistant","config":{"instruction":"Load the Sales graph","allow":[` + allow + `]},"position":{"x":1,"y":0}},` +
		`{"id":"mail","kind":"notify","config":{"message":""},"position":{"x":2,"y":0}}],` +
		`"edges":[{"from":"start","to":"ask"},{"from":"ask","to":"mail"}]}`)
}

const loadOnt1 = `{"operation":"create_load","ontology_id":"ont1"}`

// automationTurn is the step "ask" of a live run of the owner's "Nightly load".
func automationTurn(h *harness, allow string) *model.FoundryAgentTurn {
	h.db.automations = []*model.FoundryAutomation{{ID: "auto1", WorkspaceID: ws, OwnerUserID: user, Name: "Nightly load"}}
	h.db.runs = []*model.FoundryAutomationRun{{ID: "run1", AutomationID: "auto1", WorkspaceID: ws, Status: model.FoundryAutomationRunRunning, Spec: stepSpec(allow)}}
	runID, node := "run1", "ask"
	turn := &model.FoundryAgentTurn{ID: "turn1", WorkspaceID: ws, ThreadID: "thread1", UserID: user, Kind: model.FoundryAgentTurnAutomation,
		Input: "Load the Sales graph\n\n<automation_context>{}</automation_context>", AutomationRunID: &runID, AutomationNodeID: &node, MaxUSDMicros: 500_000, Attempts: 1}
	h.db.turns = []*model.FoundryAgentTurn{turn}
	return turn
}

func TestAnAutomationRunsWhatItsOwnerApproved(t *testing.T) {
	h := newHarness(t,
		call("toolu_1", toolPrepare, `{"operation":"create_load","target":"ontology:ws1/ont1","payload":{"version_id":"ver3"}}`),
		call("toolu_2", toolReport, `{"summary":"Loaded the Sales graph.","answer":"yes","count":3}`),
		say("Done."))
	turn := automationTurn(h, loadOnt1)
	a, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	assert.Equal(t, StopAnswered, a.Stop)
	assert.Equal(t, 1, h.backend.loads, "the approved action ran once, at once")
	require.Len(t, h.db.actions, 1)
	action := h.db.actions[0]
	assert.Equal(t, model.FoundryAgentActionSucceeded, action.Status)
	assert.Equal(t, &turn.UserID, action.DecidedBy, "decided by its owner's approval")
	assert.Equal(t, []string{"started", "tool_call", "action_card", "action_result", "tool_result", "tool_call", "automation_report", "tool_result", "text", "finished"},
		h.db.eventTypes(), "the card, then how it ran")
	assert.Equal(t, 2, len(h.host.actors), "a session for the turn, and one as its owner for the action")

	system := h.model.requests[0].System
	assert.Contains(t, system, `automation "Nightly load"`)
	assert.Contains(t, system, "unattended")
	assert.Contains(t, system, "create_load on ontology:<this workspace>/ont1")
	assert.Contains(t, h.model.requests[0].Messages[0].Content[0].(llm.Text).Text, "<automation_context>")
	offered := map[string]bool{}
	for _, tool := range h.model.requests[0].Tools {
		offered[tool.Name] = true
	}
	assert.True(t, offered[toolReport], "an automation's turn reports")

	afterPrepare := h.model.requests[1].Messages
	result := afterPrepare[len(afterPrepare)-1].Content[0].(llm.ToolResult)
	assert.Contains(t, result.Content, `"ran"`)
	assert.Contains(t, result.Content, `"succeeded"`)
	assert.NotContains(t, result.Content, "hlxc_", "the model never sees a token")

	var report Reported
	for _, e := range h.db.events {
		var head struct{ Type string }
		require.NoError(t, json.Unmarshal(e, &head))
		if head.Type == "automation_report" {
			require.NoError(t, json.Unmarshal(e, &report))
		}
	}
	assert.Equal(t, "Loaded the Sales graph.", report.Summary)
	require.NotNil(t, report.Answer)
	assert.True(t, *report.Answer)
	assert.Equal(t, int64(3), *report.Count)
}

func TestAnAutomationLeavesWhatItsOwnerDidntApproveAsACard(t *testing.T) {
	h := newHarness(t,
		call("toolu_1", toolPrepare, `{"operation":"create_load","target":"ontology:ws1/ont1","payload":{"version_id":"ver3"}}`),
		say("It waits for you."))
	turn := automationTurn(h, `{"operation":"create_load","ontology_id":"ont2"}`)
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	assert.Zero(t, h.backend.loads, "another ontology's load was approved, not this one's")
	require.Len(t, h.db.actions, 1)
	assert.Equal(t, model.FoundryAgentActionPrepared, h.db.actions[0].Status)
	afterPrepare := h.model.requests[1].Messages
	assert.Contains(t, afterPrepare[len(afterPrepare)-1].Content[0].(llm.ToolResult).Content, "awaiting_person")
}

// An approved publish refreshes only the revision already at the
// dashboard's link: an edit made since, by anyone, waits for the owner as a
// card, as does a dashboard that was never published.
func TestAnApprovedPublishRefreshesOnlyWhatWasPublished(t *testing.T) {
	for name, tc := range map[string]struct {
		share   *model.FoundryDashboardShare
		prepare int64
		ran     bool
	}{
		"the revision at its link": {&model.FoundryDashboardShare{SpecRevision: 4}, 4, true},
		"an edit made since":       {&model.FoundryDashboardShare{SpecRevision: 3}, 4, false},
		"never published":          {nil, 4, false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t,
				call("toolu_1", toolPrepare, fmt.Sprintf(`{"operation":"publish_dashboard","target":"dashboard:ws1/dash1","payload":{"expected_revision":%d}}`, tc.prepare)),
				say("Done."))
			h.host.service.PublicDashboards = true
			h.db.dashboards = []*model.FoundryDashboard{{ID: "dash1", WorkspaceID: ws, Name: "Revenue", Revision: 4, Share: tc.share}}
			turn := automationTurn(h, `{"operation":"publish_dashboard","dashboard_id":"dash1"}`)
			_, err := h.runner.Run(context.Background(), turn)
			require.NoError(t, err)
			require.Len(t, h.db.actions, 1)
			if tc.ran {
				assert.Equal(t, []int64{4}, h.backend.published)
				assert.Equal(t, model.FoundryAgentActionSucceeded, h.db.actions[0].Status)
				return
			}
			assert.Empty(t, h.backend.published, "nothing is published unasked")
			assert.Equal(t, model.FoundryAgentActionPrepared, h.db.actions[0].Status, "it waits for its owner")
		})
	}
}

func TestAnAutomationWithNothingApprovedSaysSo(t *testing.T) {
	prompt := automationPrompt(&automationStep{name: "Weekly"})
	assert.Contains(t, prompt, "approved none for this step")
	assert.NotContains(t, prompt, "run at once")
}

func TestAnApprovedActionRunsOnceAcrossReplays(t *testing.T) {
	prepare := `{"operation":"create_load","target":"ontology:ws1/ont1","payload":{"version_id":"ver3"}}`
	h := newHarness(t, call("toolu_1", toolPrepare, prepare), call("toolu_2", toolPrepare, prepare), say("Loaded."))
	turn := automationTurn(h, loadOnt1)
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	assert.Equal(t, 1, h.backend.loads, "prepared again, it is found, not run again")
	again := h.model.requests[2].Messages
	assert.Contains(t, again[len(again)-1].Content[0].(llm.ToolResult).Content, `"succeeded"`)

	// An attempt that died while it ran finds it running: it may or may not have.
	h.db.decided, h.db.messages = model.FoundryAgentActionExecuting, nil
	h.model.steps = [][]llm.Chunk{call("toolu_3", toolPrepare, prepare), say("Check it.")}
	_, err = h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	assert.Equal(t, 1, h.backend.loads)
	last := h.model.requests[len(h.model.requests)-1].Messages
	assert.Contains(t, last[len(last)-1].Content[0].(llm.ToolResult).Content, `"unknown"`)

	// One its person dismissed is theirs, and refused.
	h.db.decided, h.db.messages = model.FoundryAgentActionDismissed, nil
	h.model.steps = [][]llm.Chunk{call("toolu_4", toolPrepare, prepare), say("Not run.")}
	_, err = h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	last = h.model.requests[len(h.model.requests)-1].Messages
	refused := last[len(last)-1].Content[0].(llm.ToolResult)
	assert.True(t, refused.IsError)
	assert.Contains(t, refused.Content, "already decided")
}

// Every action an automation's owner may approve for a step is one the
// Assistant prepares.
func TestAnAutomationAllowsWhatItsOwnerMayApprove(t *testing.T) {
	sink := &actionSink{reach: ReachWorkspace}
	for operation := range automation.Operations {
		assert.True(t, sink.Allows(operation), operation)
	}
	assert.False(t, sink.Allows("upload_document"))
}

func TestReportsAreChecked(t *testing.T) {
	three, minus := int64(3), int64(-1)
	for _, bad := range []ReportInput{
		{Summary: " "}, {Summary: strings.Repeat("s", maxReportChars+1)}, {Summary: "ok", Answer: "maybe"}, {Summary: "ok", Count: &minus},
	} {
		_, err := reportOf(bad)
		assert.Error(t, err, "%+v", bad)
	}
	report, err := reportOf(ReportInput{Summary: " Three overdue. ", Answer: "no", Count: &three})
	require.NoError(t, err)
	assert.Equal(t, "Three overdue.", report.Summary)
	assert.False(t, *report.Answer)
}

func TestAnAutomationStepIsReadFromItsRun(t *testing.T) {
	h := newHarness(t)
	turn := automationTurn(h, loadOnt1)
	step, err := h.runner.automationStep(context.Background(), turn)
	require.NoError(t, err)
	assert.Equal(t, "Nightly load", step.name)
	require.Len(t, step.allow, 1)

	for name, change := range map[string]func(){
		"the run gone":        func() { h.db.runs = nil },
		"the run ended":       func() { h.db.runs[0].Status = model.FoundryAutomationRunCancelled },
		"the automation gone": func() { h.db.automations = nil },
		"no spec":             func() { h.db.runs[0].Spec = json.RawMessage(`{}`) },
		"another step":        func() { node := "mail"; turn.AutomationNodeID = &node },
		"no step":             func() { turn.AutomationNodeID = nil },
	} {
		automationTurn(h, loadOnt1)
		turn = h.db.turns[0]
		change()
		_, err := h.runner.automationStep(context.Background(), turn)
		var failure *Failure
		require.ErrorAs(t, err, &failure, name)
		assert.True(t, failure.Permanent, name)
	}
	// Run fails such a turn for good, before any model call.
	h = newHarness(t)
	turn = automationTurn(h, loadOnt1)
	h.db.runs = nil
	_, err = h.runner.Run(context.Background(), turn)
	var failure *Failure
	require.ErrorAs(t, err, &failure)
	assert.Empty(t, h.model.requests)
}
