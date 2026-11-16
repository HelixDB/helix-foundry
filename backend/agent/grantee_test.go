package agent

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/agent/llm"
	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// toolResultOf is what the model was told of the call before request n.
func toolResultOf(t *testing.T, h *harness, n int) llm.ToolResult {
	t.Helper()
	require.Greater(t, len(h.model.requests), n)
	told := h.model.requests[n].Messages
	return told[len(told)-1].Content[0].(llm.ToolResult)
}

// A restricted member's turn offers GranteeTools alone: it reads and asks,
// and a prepare it calls anyway is no tool, so nothing is made.
func TestAGranteesTurnOnlyReadsAndAsks(t *testing.T) {
	h := newHarness(t,
		call("toolu_1", toolPrepare, `{"operation":"create_dashboard","target":"workspace:ws1","payload":{"name":"Invoices","spec":{}}}`),
		say("An owner or admin makes dashboards."))
	h.backend.grantee = true
	_, err := h.runner.Run(context.Background(), message("Make me a dashboard of invoices"))
	require.NoError(t, err)

	var offered []string
	for _, tool := range h.model.requests[0].Tools {
		offered = append(offered, tool.Name)
	}
	assert.ElementsMatch(t, slices.Collect(maps.Keys(GranteeTools)), offered)
	for _, name := range []string{toolPrepare, toolReport, "helix_foundry_connect", "helix_foundry_preview_dashboard_tile", "helix_foundry_list_documents",
		"helix_foundry_get_generation", "helix_foundry_list_reviews", "helix_list_projects", "helix_list_databases"} {
		assert.NotContains(t, offered, name)
	}
	assert.Contains(t, toolResultOf(t, h, 1).Content, "No tool named")
	assert.Empty(t, h.backend.created, "no dashboard, though the message asked for one")
	assert.Empty(t, h.db.actions)
	assert.NotContains(t, h.db.eventTypes(), "action_card")
	assert.Contains(t, h.model.requests[0].System, granteePrompt)
}

// GranteeTools are a part of Tools, without prepare or the report.
func TestGranteeToolsAreWithinTools(t *testing.T) {
	for name := range GranteeTools {
		assert.True(t, Tools[name], name)
	}
	assert.False(t, GranteeTools[toolPrepare])
	assert.False(t, GranteeTools[toolReport])
	assert.True(t, GranteeTools[toolAskPerson])
	assert.True(t, GranteeTools[toolAsk])
}

// A sink that never learned its turn's reach, or learned ReachGrants,
// allows, records and runs nothing, even a dashboard the message asked for.
func TestAGranteesSinkAllowsAndRunsNothing(t *testing.T) {
	h := newHarness(t)
	sink := &actionSink{db: h.db, keys: h.keys, turn: message("Make me a dashboard"), maxPending: 3}
	for operation := range Operations {
		assert.False(t, sink.Allows(operation), operation)
	}
	dashboard := integration.PreparedAction{Operation: "create_dashboard", Target: "workspace:" + ws, PayloadHash: strings.Repeat("a", 64)}
	assert.False(t, sink.autoRuns(dashboard))
	_, err := sink.Record(context.Background(), dashboard)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Empty(t, h.db.actions)

	sink.reach = ReachWorkspace
	assert.True(t, sink.Allows("create_dashboard"))
	assert.True(t, sink.autoRuns(dashboard))
}

// A restricted member answers Founder's own questions, never a draft's.
func TestAGranteeIsAskedItsOwnQuestionsAlone(t *testing.T) {
	t.Run("a draft's questions aren't relayed", func(t *testing.T) {
		h := newHarness(t,
			call("toolu_1", toolAskPerson, `{"generation":{"ontology_id":"ont1","generation_id":"gen1"}}`),
			say("An owner or admin answers the draft's questions."))
		h.backend.grantee = true
		turn := message("What does the draft need?")
		h.db.turns = []*model.FoundryAgentTurn{turn}
		_, err := h.runner.Run(context.Background(), turn)
		require.NoError(t, err)
		result := toolResultOf(t, h, 1)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Content, "can't answer a draft's questions")
		assert.Nil(t, h.db.asked[turn.ID])
		assert.Empty(t, h.backend.answered)
	})
	t.Run("its own are", func(t *testing.T) {
		h := newHarness(t, call("toolu_1", toolAskPerson, threeQuestions))
		h.backend.grantee = true
		turn := message("Who are our biggest customers?")
		h.db.turns = []*model.FoundryAgentTurn{turn}
		a, err := h.runner.Run(context.Background(), turn)
		require.NoError(t, err)
		assert.Equal(t, StopAsked, a.Stop)
		require.NotNil(t, h.db.asked[turn.ID])
		assert.Len(t, h.db.asked[turn.ID].Questions, 3)
	})
}

// Only a restricted member's turn is told it reads and asks only.
func TestOnlyAGranteeIsToldItReadsOnly(t *testing.T) {
	for _, grantee := range []bool{false, true} {
		h := newHarness(t, say("Hello."))
		h.backend.grantee = grantee
		_, err := h.runner.Run(context.Background(), message("Hi"))
		require.NoError(t, err)
		assert.Equal(t, grantee, strings.Contains(h.model.requests[0].System, granteePrompt), "grantee %v", grantee)
	}
	assert.Contains(t, granteePrompt, "leave load_id empty")
	assert.Contains(t, granteePrompt, "an owner or admin does it in Foundry")
	assert.Contains(t, granteePrompt, "the limit on their questions about the data, not your own budget")
	assert.NotContains(t, granteePrompt, "Analyst")
	assert.Equal(t, "assistant-14", PromptVersion)
}

// Access that can't be read fails the attempt closed, to retry: no model
// call, so no tool is offered.
func TestAnUnreadableAccessFailsTheAttemptToRetry(t *testing.T) {
	h := newHarness(t, say("Never said."))
	h.backend.accessErr = status.Error(codes.Unavailable, "the API is down")
	_, err := h.runner.Run(context.Background(), message("Hi"))
	var f *Failure
	require.ErrorAs(t, err, &f)
	assert.False(t, f.Permanent)
	assert.Equal(t, "Your access to this workspace couldn't be read; Helix will retry", f.Message)
	assert.Empty(t, h.model.requests)
}

// An automation whose owner is now restricted fails for good: a grantee
// runs no automation.
func TestAGranteesAutomationFailsForGood(t *testing.T) {
	h := newHarness(t, say("Never said."))
	h.backend.grantee = true
	_, err := h.runner.Run(context.Background(), automationTurn(h, loadOnt1))
	var f *Failure
	require.ErrorAs(t, err, &f)
	assert.True(t, f.Permanent)
	assert.Contains(t, f.Message, "an owner or admin can take it over")
	assert.Empty(t, h.model.requests)
	assert.Zero(t, h.backend.loads)
}

// A helper's CLI offers a restricted member GranteeTools alone, and none
// while their access can't be read.
func TestAHelpersCLIOffersAGranteeGranteeToolsAlone(t *testing.T) {
	hosts, db, h := newHosts(t)
	h.backend.grantee = true
	turn := message("Who pays?")
	db.turns = []*model.FoundryAgentTurn{turn}
	client := connectHost(t, hosts, turn, func(context.Context, bool) error { return nil })
	listed, err := client.ListTools(context.Background(), nil)
	require.NoError(t, err)
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	assert.ElementsMatch(t, slices.Collect(maps.Keys(GranteeTools)), names)

	h.backend.accessErr = status.Error(codes.Unavailable, "the API is down")
	_, err = hosts.Server(context.Background(), turn, "host:1", func(context.Context, bool) error { return nil })
	assert.Error(t, err)
}

// A turn records its reach on its thread before anything runs: the whole
// view's makes it no restricted member's, for good; a restricted member's
// adds what they hold now, through themselves and their teams.
func TestATurnRecordsItsReachOnItsThread(t *testing.T) {
	stripe, general := access.ConnectorTarget("conn-stripe"), access.DocumentSetTarget("set-general")
	t.Run("the whole view's", func(t *testing.T) {
		h := newHarness(t, say("Hello."))
		_, err := h.runner.Run(context.Background(), message("Hi"))
		require.NoError(t, err)
		assert.Nil(t, h.db.thread("thread1").Granted)
	})
	t.Run("a restricted member's", func(t *testing.T) {
		h := newHarness(t, say("Hello."))
		h.backend.grantee = true
		h.db.teams = map[string][]string{user: {"finance"}}
		h.db.grants = []model.FoundryAccessGrant{
			{Principal: access.Principal{Kind: access.PrincipalUser, ID: user}, Target: stripe},
			{Principal: access.Principal{Kind: access.PrincipalTeam, ID: "finance"}, Target: general},
			{Principal: access.Principal{Kind: access.PrincipalUser, ID: "user-eve"}, Target: access.EnrichmentTarget()},
		}
		_, err := h.runner.Run(context.Background(), message("Hi"))
		require.NoError(t, err)
		require.NotNil(t, h.db.thread("thread1").Granted)
		assert.Equal(t, []access.Target{stripe, general}, h.db.thread("thread1").Granted.Targets)
	})
}

// A restricted member's turn runs only in a thread whose every grant they
// still hold: one begun with the whole view (before their restriction, or
// while they were an owner or admin), or one whose grants they lost, fails
// for good before the model reads its transcript.
func TestAGranteesTurnNeedsItsThreadsGrants(t *testing.T) {
	stripe := access.ConnectorTarget("conn-stripe")
	for name, granted := range map[string]*model.FoundryAgentThreadGrants{
		"begun with the whole view": nil,
		"its grants lost":           {Targets: []access.Target{stripe}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, say("Never said."))
			h.backend.grantee = true
			h.db.thread("thread1").Granted = granted
			_, err := h.runner.Run(context.Background(), message("What did you read before?"))
			var f *Failure
			require.ErrorAs(t, err, &f)
			assert.True(t, f.Permanent)
			assert.Contains(t, f.Message, "start a new one")
			assert.Empty(t, h.model.requests, "no model call")
			assert.Equal(t, granted, h.db.thread("thread1").Granted, "nothing recorded")
		})
	}
	t.Run("its grants still held", func(t *testing.T) {
		h := newHarness(t, say("Hello."))
		h.backend.grantee = true
		h.db.thread("thread1").Granted = &model.FoundryAgentThreadGrants{Targets: []access.Target{access.TableTarget("conn-stripe", "invoices")}}
		h.db.grants = []model.FoundryAccessGrant{{Principal: access.Principal{Kind: access.PrincipalUser, ID: user}, Target: stripe}}
		_, err := h.runner.Run(context.Background(), message("Hi"))
		require.NoError(t, err)
		assert.Equal(t, []access.Target{access.TableTarget("conn-stripe", "invoices"), stripe}, h.db.thread("thread1").Granted.Targets)
	})
}
