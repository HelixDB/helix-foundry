package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/agent/llm"
	"github.com/helixdb/helix-foundry/backend/integration"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// eventsOf are the stored events of type typ, decoded.
func (s *memStore) eventsOf(t *testing.T, typ string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range s.events {
		var event map[string]any
		require.NoError(t, json.Unmarshal(e, &event))
		if event["type"] == typ {
			out = append(out, event)
		}
	}
	return out
}

// returned are the stored tool_result events, decoded.
func (s *memStore) returned(t *testing.T) []ToolReturned {
	t.Helper()
	var out []ToolReturned
	for _, e := range s.events {
		var head struct{ Type string }
		require.NoError(t, json.Unmarshal(e, &head))
		if head.Type != "tool_result" {
			continue
		}
		var r ToolReturned
		require.NoError(t, json.Unmarshal(e, &r))
		out = append(out, r)
	}
	return out
}

const makeDashboard = `{"operation":"create_dashboard","target":"workspace:ws1","payload":{"name":"Invoices by account"}}`

// A new dashboard the person's message asked for runs at once, as them,
// without a card to click: its card and result say so, the model is told to
// give its link, and a replayed attempt finds how it ran.
func TestFounderMakesTheDashboardItWasAskedForAtOnce(t *testing.T) {
	h := newHarness(t, call("toolu_1", toolPrepare, makeDashboard), say("I made it: /dashboards/dash1."))
	turn := message("Make me a dashboard of invoices by account")
	h.db.turns = []*model.FoundryAgentTurn{turn}
	a, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	assert.Equal(t, StopAnswered, a.Stop)

	require.Len(t, h.backend.created, 1, "made once, at once")
	assert.Equal(t, "Invoices by account", h.backend.created[0].GetName())
	assert.Equal(t, ws, h.backend.created[0].GetWorkspaceId())
	require.Len(t, h.db.actions, 1)
	action := h.db.actions[0]
	assert.Equal(t, model.FoundryAgentActionSucceeded, action.Status)
	assert.Equal(t, new(turn.Principal.Access().ID), action.DecidedBy, "decided by the person who asked")
	assert.Contains(t, string(action.Result), `"id":"dash1"`, "the dashboard Undo deletes")
	assert.Equal(t, []string{"started", "tool_call", "action_card", "action_result", "tool_result", "text", "finished"}, h.db.eventTypes(),
		"the card, then how it ran")
	assert.Len(t, h.host.actors, 2, "a session for the turn, and one as the person for the action")

	cards := h.db.eventsOf(t, "action_card")
	require.Len(t, cards, 1)
	assert.Equal(t, true, cards[0]["auto"])
	results := h.db.eventsOf(t, "action_result")
	require.Len(t, results, 1)
	assert.Equal(t, true, results[0]["auto"])
	assert.Equal(t, "succeeded", results[0]["status"])
	assert.Equal(t, "create_dashboard", results[0]["operation"])

	told := lastResult(t, h)
	assert.False(t, told.IsError)
	assert.Contains(t, told.Content, `"ran"`)
	assert.Contains(t, told.Content, "Foundry made this new dashboard at once")
	assert.NotContains(t, told.Content, "hlxc_", "the model never sees a token")

	// A replayed attempt prepares it again: it is found, not made again.
	h.db.messages = nil
	h.model.steps = [][]llm.Chunk{call("toolu_2", toolPrepare, makeDashboard), say("Made.")}
	_, err = h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	assert.Len(t, h.backend.created, 1, "a replay never makes it again")
	assert.Len(t, h.db.eventsOf(t, "action_card"), 1, "one card")
	again := lastResult(t, h)
	assert.Contains(t, again.Content, `"succeeded"`)
	assert.Contains(t, again.Content, "Foundry made this new dashboard at once")
}

// Everything else stays a card the person clicks: a dashboard their message
// didn't ask for, and every other change to a dashboard, however the
// message words it.
func TestFounderLeavesEverythingElseACard(t *testing.T) {
	for name, tc := range map[string]struct {
		said, prepare string
	}{
		"a dashboard no message asked for": {"Make me something to watch overdue invoices", makeDashboard},
		"a word that only holds dashboard": {"Make me a dashboarding view", makeDashboard},
		"an edit":                          {"Rename the Revenue dashboard", `{"operation":"update_dashboard","target":"dashboard:ws1/dash1","payload":{"expected_revision":2,"name":"Income"}}`},
		"a pin":                            {"Pin the Revenue dashboard", `{"operation":"pin_dashboard","target":"dashboard:ws1/dash1","payload":{"expected_revision":2,"pinned":true}}`},
		"a publish":                        {"Publish the Revenue dashboard", `{"operation":"publish_dashboard","target":"dashboard:ws1/dash1","payload":{"expected_revision":2}}`},
		"a delete":                         {"Delete the Revenue dashboard", `{"operation":"delete_dashboard","target":"dashboard:ws1/dash1","payload":{}}`},
		"a load":                           {"Load the Sales graph and make a dashboard", `{"operation":"create_load","target":"ontology:ws1/ont1","payload":{"version_id":"ver3"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, call("toolu_1", toolPrepare, tc.prepare), say("Click Run."))
			h.db.dashboards = []*model.FoundryDashboard{{ID: "dash1", WorkspaceID: ws, Name: "Revenue", Revision: 2}}
			turn := message(tc.said)
			h.db.turns = []*model.FoundryAgentTurn{turn}
			_, err := h.runner.Run(context.Background(), turn)
			require.NoError(t, err)
			require.Len(t, h.db.actions, 1)
			assert.Equal(t, model.FoundryAgentActionPrepared, h.db.actions[0].Status, "it waits for the person")
			assert.Nil(t, h.db.actions[0].DecidedBy)
			assert.Empty(t, h.backend.created)
			assert.Empty(t, h.backend.deleted)
			assert.Empty(t, h.backend.published)
			assert.Zero(t, h.backend.loads)
			assert.Empty(t, h.db.eventsOf(t, "action_result"))
			cards := h.db.eventsOf(t, "action_card")
			require.Len(t, cards, 1)
			assert.NotContains(t, cards[0], "auto")
			assert.Contains(t, lastResult(t, h).Content, "awaiting_person")
		})
	}
}

// An automation's turn makes no dashboard unasked, whatever its instruction
// says: what its owner didn't approve waits for them as a card.
func TestAnAutomationMakesNoDashboardUnasked(t *testing.T) {
	h := newHarness(t, call("toolu_1", toolPrepare, makeDashboard), say("It waits for you."))
	turn := automationTurn(h, loadOnt1)
	turn.Input = "Make a dashboard of the overdue invoices\n\n<workflow_context>{}</workflow_context>"
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	assert.Empty(t, h.backend.created)
	require.Len(t, h.db.actions, 1)
	assert.Equal(t, model.FoundryAgentActionPrepared, h.db.actions[0].Status)
}

func TestAutoRunIsANarrowList(t *testing.T) {
	assert.Equal(t, []string{"create_dashboard"}, func() []string {
		var ops []string
		for op := range autoRun {
			ops = append(ops, op)
		}
		return ops
	}(), "only a new dashboard runs without a card")
	turn := func(kind model.FoundryAgentTurnKind, input string) *model.FoundryAgentTurn {
		m := message(input)
		m.Kind = kind
		return m
	}
	for name, tc := range map[string]struct {
		turn              *model.FoundryAgentTurn
		operation, target string
		runs              bool
	}{
		"a message asking for a dashboard":    {message("Make me a dashboard of invoices"), "create_dashboard", "workspace:ws1", true},
		"dashboards, in capitals":             {message("DASHBOARDS: one for sales"), "create_dashboard", "workspace:ws1", true},
		"dashboard at the start":              {message("dashboard of overdue invoices please"), "create_dashboard", "workspace:ws1", true},
		"a message about something else":      {message("Which invoices are overdue?"), "create_dashboard", "workspace:ws1", false},
		"dashboard inside another word":       {message("a dashboarding tool"), "create_dashboard", "workspace:ws1", false},
		"another workspace":                   {message("Make me a dashboard"), "create_dashboard", "workspace:ws2", false},
		"an automation's turn":                {turn(model.FoundryAgentTurnAutomation, "Make a dashboard"), "create_dashboard", "workspace:ws1", false},
		"an action's result":                  {turn(model.FoundryAgentTurnActionResult, "dashboard"), "create_dashboard", "workspace:ws1", false},
		"answers to questions":                {turn(model.FoundryAgentTurnAnswers, "a dashboard"), "create_dashboard", "workspace:ws1", false},
		"an edit, though dashboard is said":   {message("Rename my dashboard"), "update_dashboard", "dashboard:ws1/dash1", false},
		"a pin, though dashboard is said":     {message("Pin my dashboard"), "pin_dashboard", "dashboard:ws1/dash1", false},
		"a publish, though dashboard is said": {message("Publish my dashboard"), "publish_dashboard", "dashboard:ws1/dash1", false},
		"a delete, though dashboard is said":  {message("Delete my dashboard"), "delete_dashboard", "dashboard:ws1/dash1", false},
	} {
		sink := &actionSink{turn: tc.turn, reach: ReachWorkspace}
		assert.Equal(t, tc.runs, sink.autoRuns(integration.PreparedAction{Operation: tc.operation, Target: tc.target}), name)
	}
}

// An ask and a get_question name their Analyst question on their result,
// whatever its size, so the person sees the Analyst's answer in place; a
// failed call and any other tool name none.
func TestAnAnalystQuestionIsNamedOnItsResult(t *testing.T) {
	want := &QuestionRef{OntologyID: ontology, ID: "q1", LoadID: "load1", ThreadID: "q1", Status: "answered"}
	for name, question := range map[string]string{"small": "Who are our customers?", "past what an event shows": strings.Repeat("x", maxResultBytes)} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t,
				call("toolu_1", toolAsk, `{"workspace_id":"ws1","ontology_id":"ont1","question":"Who are our customers?"}`),
				call("toolu_2", toolGetQuestion, `{"workspace_id":"ws1","ontology_id":"ont1","question_id":"q1"}`),
				call("toolu_3", toolAsk, `{"workspace_id":"ws1","ontology_id":"ont1","question":" "}`),
				call("toolu_4", "helix_foundry_list_ontologies", `{"workspace_id":"ws1"}`),
				say("That's the answer."))
			h.backend.question = &foundryv1.FoundryQuestion{Id: "q1", LoadId: "load1", ThreadId: "q1", Status: "answered", Question: question}
			_, err := h.runner.Run(context.Background(), message("Who are our customers?"))
			require.NoError(t, err)
			results := h.db.returned(t)
			require.Len(t, results, 4)
			truncated := len(question) > 100
			for _, r := range results[:2] {
				assert.False(t, r.IsError, r.Name)
				assert.Equal(t, want, r.Question, r.Name)
				assert.Equal(t, truncated, r.Truncated, r.Name)
			}
			assert.True(t, results[2].IsError, "a question with no words is refused")
			assert.Nil(t, results[2].Question, "a failed ask names none")
			assert.Nil(t, results[3].Question, "another tool names none")
		})
	}
	var event map[string]any
	shown, err := MarshalEvent(ToolReturned{ToolUseID: "toolu_1", Name: toolAsk, Output: map[string]any{}, Question: want})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(shown, &event))
	assert.Equal(t, map[string]any{"ontology_id": "ont1", "id": "q1", "load_id": "load1", "thread_id": "q1", "status": "answered"}, event["question"])
}

func TestQuestionOfReadsOnlyAQuestion(t *testing.T) {
	in := json.RawMessage(`{"ontology_id":"ont1","question":"Who?"}`)
	out := map[string]any{"id": "q1", "status": "queued", "done": false}
	assert.Equal(t, &QuestionRef{OntologyID: "ont1", ID: "q1", Status: "queued"}, questionOf(toolAsk, in, out))
	assert.Nil(t, questionOf("helix_foundry_read_graph", in, out), "another tool")
	assert.Nil(t, questionOf(toolAsk, json.RawMessage(`{"question":"Who?"}`), out), "no ontology")
	assert.Nil(t, questionOf(toolAsk, in, map[string]any{"status": "queued"}), "no question")
	assert.Nil(t, questionOf(toolAsk, in, nil), "no output")
	assert.Nil(t, questionOf(toolAsk, in, "opaque"), "an output that isn't an object")
	assert.Nil(t, questionOf(toolAsk, json.RawMessage(`not json`), out), "an input that isn't JSON")
}

// A helper's turn names its questions and makes the dashboard asked for at
// once, as the worker's own turns do.
func TestAHelpersTurnRunsAsTheWorkersDo(t *testing.T) {
	hosts, db, h := newHosts(t)
	ctx := context.Background()
	turn := message("Build a dashboard of invoices by account")
	db.turns = []*model.FoundryAgentTurn{turn}
	h.backend.question = &foundryv1.FoundryQuestion{Id: "q1", LoadId: "load1", ThreadId: "q1", Status: "answered", Question: strings.Repeat("x", maxResultBytes)}
	client := connectHost(t, hosts, turn, func(context.Context, bool) error { return nil })

	_, err := client.CallTool(ctx, &mcp.CallToolParams{Name: toolAsk, Arguments: map[string]any{
		"workspace_id": ws, "ontology_id": ontology, "question": "Which invoices are unpaid?"}})
	require.NoError(t, err)
	results := db.returned(t)
	require.Len(t, results, 1)
	assert.True(t, results[0].Truncated)
	assert.Equal(t, &QuestionRef{OntologyID: ontology, ID: "q1", LoadID: "load1", ThreadID: "q1", Status: "answered"}, results[0].Question)

	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: toolPrepare, Arguments: map[string]any{
		"operation": "create_dashboard", "target": "workspace:ws1", "payload": map[string]any{"name": "Invoices by account"}}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	raw, _ := json.Marshal(result)
	assert.Contains(t, string(raw), "Foundry made this new dashboard at once")
	assert.NotContains(t, string(raw), "hlxc_")
	require.Len(t, h.backend.created, 1, "the helper's turn made it, as the person")
	require.Len(t, db.actions, 1)
	assert.Equal(t, model.FoundryAgentActionSucceeded, db.actions[0].Status)
	cards := db.eventsOf(t, "action_card")
	require.Len(t, cards, 1)
	assert.Equal(t, true, cards[0]["auto"])
	ran := db.eventsOf(t, "action_result")
	require.Len(t, ran, 1)
	assert.Equal(t, true, ran[0]["auto"])
}

func TestFounderIsToldWhatItRunsAtOnce(t *testing.T) {
	assert.Equal(t, "assistant-19", PromptVersion)
	assert.True(t, strings.HasPrefix(systemPrompt, "You are Founder in Helix Foundry"))
	assert.NotContains(t, systemPrompt, "Assistant", "it names itself Founder")
	assert.Contains(t, systemPrompt, "You change nothing yourself, with one exception: a new dashboard the person asked for")
	assert.Contains(t, systemPrompt, "A new dashboard the person asked for runs at once, without a card")
	assert.Contains(t, systemPrompt, "Updating, pinning, publishing or deleting a dashboard stays a card")
	assert.Contains(t, systemPrompt, "don't repeat or paraphrase it")
	assert.Contains(t, systemPrompt, "it is the limit on questions about the data, not your own budget")
	assert.Contains(t, systemPrompt, "never call it the Analyst's", "people never read the name Analyst")
}
