package agent

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/agent/llm"
	"github.com/helixdb/helix-foundry/backend/integration"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
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
	assert.Equal(t, "assistant-20", PromptVersion)
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

	// Nor in a thread begun with the whole view.
	db.thread(turn.ThreadID).Granted = nil
	_, err = hosts.Server(context.Background(), turn, "host:1", func(context.Context, bool) error { return nil })
	var f *Failure
	require.ErrorAs(t, err, &f)
	assert.Contains(t, f.Message, "start a new one")
	db.thread(turn.ThreadID).Granted = &model.FoundryAgentThreadGrants{}

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
// still hold, and stops before the model reads its transcript: one whose
// grants they lost fails for good; one begun with the whole view (before
// their restriction, or while they were an owner or admin) fails to retry, as
// a role that couldn't be read for a moment reads as restricted.
func TestAGranteesTurnNeedsItsThreadsGrants(t *testing.T) {
	stripe := access.ConnectorTarget("conn-stripe")
	for name, tc := range map[string]struct {
		granted   *model.FoundryAgentThreadGrants
		permanent bool
	}{
		"begun with the whole view": {nil, false},
		"its grants lost":           {&model.FoundryAgentThreadGrants{Targets: []access.Target{stripe}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, say("Never said."))
			h.backend.grantee = true
			h.db.thread("thread1").Granted = tc.granted
			_, err := h.runner.Run(context.Background(), message("What did you read before?"))
			var f *Failure
			require.ErrorAs(t, err, &f)
			assert.Equal(t, tc.permanent, f.Permanent)
			assert.Contains(t, f.Message, "start a new one")
			assert.Empty(t, h.model.requests, "no model call")
			assert.Equal(t, tc.granted, h.db.thread("thread1").Granted, "nothing recorded")
		})
	}
	// She was in #exec (C2) when the thread ran, and was removed from it since.
	t.Run("a channel it ran under no longer shared", func(t *testing.T) {
		h := newHarness(t, say("Never said."))
		h.backend.grantee = true
		h.backend.items = []*foundryv1.FoundrySourceItem{{ConnectorId: "conn-slack", ItemKey: "C1"}}
		h.db.grants = []model.FoundryAccessGrant{{Principal: access.Principal{Kind: access.PrincipalUser, ID: user}, Target: access.ConnectorTarget("conn-slack")}}
		granted := &model.FoundryAgentThreadGrants{Targets: []access.Target{access.ConnectorTarget("conn-slack")},
			Items: []access.ItemRef{{ConnectorID: "conn-slack", Item: "C1"}, {ConnectorID: "conn-slack", Item: "C2"}}}
		h.db.thread("thread1").Granted = granted
		_, err := h.runner.Run(context.Background(), message("What did #exec say?"))
		var f *Failure
		require.ErrorAs(t, err, &f)
		assert.True(t, f.Permanent)
		assert.Contains(t, f.Message, "no longer lets you read")
		assert.Empty(t, h.model.requests, "no model call")
	})
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

// Every call of a restricted member's turn is read between two reads of
// their reach and grants, each recorded on the thread: a grant given during
// the turn is recorded once a call may have read it, and a call during which
// their grants or role changed makes the thread the whole view's and fails
// the turn for good, its result reaching neither the model nor the person.
func TestAGranteesCallsRecordWhatTheyMayRead(t *testing.T) {
	eve := access.Principal{Kind: access.PrincipalUser, ID: user}
	general, stripe := access.DocumentSetTarget("set-general"), access.ConnectorTarget("conn-stripe")
	list := func() []llm.Chunk { return call("toolu_1", "helix_foundry_list_ontologies", `{"workspace_id":"ws1"}`) }
	t.Run("a grant given between calls is recorded at the next", func(t *testing.T) {
		h := newHarness(t, list(), call("toolu_2", "helix_foundry_list_ontologies", `{"workspace_id":"ws1"}`), say("Sales graph."))
		h.backend.grantee = true
		h.db.grants = []model.FoundryAccessGrant{{Principal: eve, Target: general}}
		h.model.tick = func() {
			if len(h.model.requests) == 2 && len(h.db.grants) == 1 {
				h.db.grants = append(h.db.grants, model.FoundryAccessGrant{Principal: eve, Target: stripe})
			}
		}
		_, err := h.runner.Run(context.Background(), message("What do I have?"))
		require.NoError(t, err)
		assert.Equal(t, []access.Target{general, stripe}, h.db.thread("thread1").Granted.Targets)
	})
	t.Run("the channels shared with them, of nothing it read, are not recorded", func(t *testing.T) {
		h := newHarness(t, list(), say("Sales graph."))
		h.backend.grantee = true
		h.backend.items = []*foundryv1.FoundrySourceItem{{ConnectorId: "conn-slack", ItemKey: "C2"}, {ConnectorId: "conn-slack", ItemKey: "C1"}}
		h.db.grants = []model.FoundryAccessGrant{{Principal: eve, Target: general}}
		_, err := h.runner.Run(context.Background(), message("What do I have?"))
		require.NoError(t, err)
		assert.Empty(t, h.db.thread("thread1").Granted.Items)
	})
	// A channel shared with them, or no longer, during a call that read
	// nothing of it changes nothing of the turn.
	for name, items := range map[string][]*foundryv1.FoundrySourceItem{
		"added to a channel during a call":     {{ConnectorId: "conn-slack", ItemKey: "C1"}, {ConnectorId: "conn-slack", ItemKey: "C2"}},
		"removed from a channel during a call": nil,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, list(), say("Sales graph."))
			h.backend.grantee = true
			h.backend.items = []*foundryv1.FoundrySourceItem{{ConnectorId: "conn-slack", ItemKey: "C1"}}
			h.db.grants = []model.FoundryAccessGrant{{Principal: eve, Target: general}}
			h.backend.onCall = func() { h.backend.items = items }
			_, err := h.runner.Run(context.Background(), message("What do I have?"))
			require.NoError(t, err)
			assert.Len(t, h.model.requests, 2, "the result reached the model")
			assert.Empty(t, h.db.thread("thread1").Granted.Items)
		})
	}
	for name, tc := range map[string]struct {
		change func(h *harness)
		// said is what the turn fails with; whole, whether the thread is the
		// whole view's after (a thread whose grants were taken is hidden
		// anyway).
		said  string
		whole bool
	}{
		"a grant given during a call": {func(h *harness) {
			h.db.grants = append(h.db.grants, model.FoundryAccessGrant{Principal: eve, Target: stripe})
		}, "Your access changed while Founder was answering", true},
		"made an admin during a call": {func(h *harness) { h.backend.grantee = false }, "Your access changed while Founder was answering", true},
		"a grant taken during a call": {func(h *harness) { h.db.grants = nil }, "no longer lets you read", false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, list(), say("Never said."))
			h.backend.grantee = true
			h.backend.items = []*foundryv1.FoundrySourceItem{{ConnectorId: "conn-slack", ItemKey: "C1"}}
			h.db.grants = []model.FoundryAccessGrant{{Principal: eve, Target: general}}
			h.backend.onCall = func() { tc.change(h) }
			_, err := h.runner.Run(context.Background(), message("What do I have?"))
			var f *Failure
			require.ErrorAs(t, err, &f)
			assert.True(t, f.Permanent)
			assert.Contains(t, f.Message, tc.said)
			assert.Equal(t, tc.whole, h.db.thread("thread1").Granted == nil)
			assert.Len(t, h.model.requests, 1, "the result never reached the model")
			assert.NotContains(t, h.db.eventTypes(), "tool_result")
		})
	}
}

// slackWorkspace is ws1 with a Slack connection (conn-slack) whose
// Conversations set (set-conv) Eve is granted: #general (C1) and #leadership
// (C2) are shared with her. load-general holds a day of #general,
// load-leadership a day of #leadership, and load-both a day of each, as one
// load of the whole set does; q-general and q-leadership are Analyst answers
// that read each.
func slackWorkspace(h *harness) {
	h.backend.grantee = true
	h.backend.items = []*foundryv1.FoundrySourceItem{{ConnectorId: "conn-slack", ItemKey: "C1"}, {ConnectorId: "conn-slack", ItemKey: "C2"}}
	h.db.grants = []model.FoundryAccessGrant{{Principal: access.Principal{Kind: access.PrincipalUser, ID: user}, Target: access.DocumentSetTarget("set-conv")}}
	h.db.connectors = []*model.Connector{{ID: "conn-slack", WorkspaceID: ws, Kind: model.ConnectorKindSlack, Name: "Slack"}}
	day := func(id, channel string) access.DocumentRef {
		return access.DocumentRef{ID: id, Set: access.SetRef{SetID: "set-conv", ConnectorID: "conn-slack"}, Stream: "conversations", ExternalID: channel + ":2026-11-20"}
	}
	h.db.holdings = map[string]access.Holdings{
		"load-general":    {Documents: []access.DocumentRef{day("doc-general", "C1")}},
		"load-leadership": {Documents: []access.DocumentRef{day("doc-leadership", "C2")}},
		"load-both":       {Documents: []access.DocumentRef{day("doc-general", "C1"), day("doc-leadership", "C2")}},
	}
	h.db.questions = map[string]*model.FoundryQuestion{
		"q-general":    {ID: "q-general", OntologyID: ontology, SeenItems: []access.ItemRef{{ConnectorID: "conn-slack", Item: "C1"}}},
		"q-leadership": {ID: "q-leadership", OntologyID: ontology, SeenItems: []access.ItemRef{{ConnectorID: "conn-slack", Item: "C2"}}},
	}
}

// readLoad is a call reading load.
func readLoad(load string) []llm.Chunk {
	return call("toolu_1", "helix_foundry_read_graph", `{"workspace_id":"ws1","ontology_id":"ont1","load_id":"`+load+`","reads":[{"query":{}}]}`)
}

// A restricted member's thread records only the channels of what its calls
// read: the documents of a load it read that she reads, an answer's. It is
// hers while each of those is still shared with her, whatever other channel
// she loses.
func TestAGranteesThreadHoldsTheChannelsItReadAlone(t *testing.T) {
	general, leadership := access.ItemRef{ConnectorID: "conn-slack", Item: "C1"}, access.ItemRef{ConnectorID: "conn-slack", Item: "C2"}
	// An ask's answer is the backend's question (setup).
	ask := call("toolu_1", "helix_foundry_ask", `{"workspace_id":"ws1","ontology_id":"ont1","question":"What changed?"}`)
	// A list's questions are the backend's (setup), their text with them.
	list := call("toolu_1", "helix_foundry_list_questions", `{"workspace_id":"ws1","ontology_id":"ont1"}`)
	for name, tc := range map[string]struct {
		read  []llm.Chunk
		setup func(h *harness)
		holds []access.ItemRef
	}{
		"a read of #general":       {read: readLoad("load-general"), holds: []access.ItemRef{general}},
		"a read of #leadership":    {read: readLoad("load-leadership"), holds: []access.ItemRef{leadership}},
		"a read of a load of both": {read: readLoad("load-both"), holds: []access.ItemRef{general, leadership}},
		"a list of #general's questions": {read: list,
			setup: func(h *harness) { h.backend.questions = []*foundryv1.FoundryQuestion{{Id: "q-general"}} }, holds: []access.ItemRef{general}},
		"a list of both channels' questions": {read: list, holds: []access.ItemRef{general, leadership},
			setup: func(h *harness) {
				h.backend.questions = []*foundryv1.FoundryQuestion{{Id: "q-leadership"}, {Id: "q-general"}}
			}},
		"an empty list of questions": {read: list},
		"nothing item-bound read":    {read: call("toolu_1", "helix_foundry_list_ontologies", `{"workspace_id":"ws1"}`)},
		"an answer from #general": {read: ask, holds: []access.ItemRef{general},
			setup: func(h *harness) { h.backend.question = &foundryv1.FoundryQuestion{Id: "q-general", Status: "answered"} }},
		"an answer from #leadership": {read: ask, holds: []access.ItemRef{leadership},
			setup: func(h *harness) {
				h.backend.question = &foundryv1.FoundryQuestion{Id: "q-leadership", Status: "answered"}
			}},
		"a read while no connection follows its source": {read: readLoad("load-leadership"),
			setup: func(h *harness) { h.db.connectors = nil }},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, tc.read, say("Here it is."), say("Still here."))
			slackWorkspace(h)
			if tc.setup != nil {
				tc.setup(h)
			}
			_, err := h.runner.Run(context.Background(), message("What did they say?"))
			require.NoError(t, err)
			assert.Equal(t, tc.holds, h.db.thread("thread1").Granted.Items)

			// She leaves #leadership: the thread is hers only if it read
			// nothing of it.
			h.backend.items = h.backend.items[:1]
			next := message("And then?")
			next.ID = "turn2"
			_, err = h.runner.Run(context.Background(), next)
			if slices.Contains(tc.holds, leadership) {
				var f *Failure
				require.ErrorAs(t, err, &f)
				assert.True(t, f.Permanent)
				assert.Contains(t, f.Message, "no longer lets you read")
				assert.Len(t, h.model.requests, 2, "no model call")
				return
			}
			require.NoError(t, err)
			assert.Len(t, h.model.requests, 3)
		})
	}
}

// A read of Slack's tables draws on the channels whose rows she reads and,
// while Slack has a non-guest account for her (the API tells her its
// directory, access.DirectoryItem), its users: the thread is hers only while
// both still are, so one that quoted the directory goes once she is a guest.
func TestAGranteesThreadHoldsTheSlackRowsItRead(t *testing.T) {
	directory := access.ItemRef{ConnectorID: "conn-slack", Item: access.DirectoryItem}
	general, leadership := access.ItemRef{ConnectorID: "conn-slack", Item: "C1"}, access.ItemRef{ConnectorID: "conn-slack", Item: "C2"}
	tables := func(h *harness, member bool) {
		slackWorkspace(h)
		h.db.grants = []model.FoundryAccessGrant{{Principal: access.Principal{Kind: access.PrincipalUser, ID: user}, Target: access.ConnectorTarget("conn-slack")}}
		h.db.holdings["load-tables"] = access.Holdings{Tables: []access.TableRef{{ConnectorID: "conn-slack", Table: "users"}, {ConnectorID: "conn-slack", Table: "messages"}}}
		if member {
			h.backend.items = append([]*foundryv1.FoundrySourceItem{{ConnectorId: "conn-slack", ItemKey: access.DirectoryItem}}, h.backend.items...)
		}
	}
	t.Run("a member who becomes a guest", func(t *testing.T) {
		h := newHarness(t, readLoad("load-tables"), say("Here it is."), say("Never said."))
		tables(h, true)
		_, err := h.runner.Run(context.Background(), message("Who posted?"))
		require.NoError(t, err)
		assert.Equal(t, []access.ItemRef{directory, general, leadership}, h.db.thread("thread1").Granted.Items)

		h.backend.items = h.backend.items[1:]
		next := message("And then?")
		next.ID = "turn2"
		_, err = h.runner.Run(context.Background(), next)
		var f *Failure
		require.ErrorAs(t, err, &f)
		assert.True(t, f.Permanent)
		assert.Contains(t, f.Message, "no longer lets you read")
		assert.Len(t, h.model.requests, 2, "no model call")
	})
	t.Run("a guest reads no directory", func(t *testing.T) {
		h := newHarness(t, readLoad("load-tables"), say("Here it is."))
		tables(h, false)
		_, err := h.runner.Run(context.Background(), message("Who posted?"))
		require.NoError(t, err)
		assert.Equal(t, []access.ItemRef{general, leadership}, h.db.thread("thread1").Granted.Items, "#general's and #leadership's messages")
	})
}

// A call that read a channel she was removed from during it fails the turn
// for good, its result reaching neither the model nor the person, and the
// thread, holding that channel, is no longer hers; one she was added to
// during it is recorded, and the turn goes on.
func TestAGranteesCallRecordsTheChannelsAroundIt(t *testing.T) {
	general, leadership := access.ItemRef{ConnectorID: "conn-slack", Item: "C1"}, access.ItemRef{ConnectorID: "conn-slack", Item: "C2"}
	t.Run("removed from the channel it read", func(t *testing.T) {
		h := newHarness(t, readLoad("load-leadership"), say("Never said."))
		slackWorkspace(h)
		h.backend.onCall = func() { h.backend.items = h.backend.items[:1] }
		_, err := h.runner.Run(context.Background(), message("What did #leadership say?"))
		var f *Failure
		require.ErrorAs(t, err, &f)
		assert.True(t, f.Permanent)
		assert.Contains(t, f.Message, "no longer lets you read")
		assert.Len(t, h.model.requests, 1, "the result never reached the model")
		assert.NotContains(t, h.db.eventTypes(), "tool_result")
		assert.Equal(t, []access.ItemRef{leadership}, h.db.thread("thread1").Granted.Items)
	})
	t.Run("removed from another channel", func(t *testing.T) {
		h := newHarness(t, readLoad("load-general"), say("Here it is."))
		slackWorkspace(h)
		h.backend.onCall = func() { h.backend.items = h.backend.items[:1] }
		_, err := h.runner.Run(context.Background(), message("What did #general say?"))
		require.NoError(t, err)
		assert.Equal(t, []access.ItemRef{general}, h.db.thread("thread1").Granted.Items)
	})
	t.Run("added to the channel it read", func(t *testing.T) {
		h := newHarness(t, readLoad("load-leadership"), say("Here it is."))
		slackWorkspace(h)
		h.backend.items = h.backend.items[:1]
		h.backend.onCall = func() {
			h.backend.items = append(h.backend.items, &foundryv1.FoundrySourceItem{ConnectorId: "conn-slack", ItemKey: "C2"})
		}
		_, err := h.runner.Run(context.Background(), message("What did #leadership say?"))
		require.NoError(t, err)
		assert.Equal(t, []access.ItemRef{leadership}, h.db.thread("thread1").Granted.Items)
	})
	// A sync trims the only day of #leadership out of the load while the
	// read reads it: what the load held before the read is recorded too.
	t.Run("a day trimmed out of the load during the read", func(t *testing.T) {
		h := newHarness(t, readLoad("load-leadership"), say("Here it is."))
		slackWorkspace(h)
		h.backend.onCall = func() { h.db.holdings["load-leadership"] = access.Holdings{} }
		_, err := h.runner.Run(context.Background(), message("What did #leadership say?"))
		require.NoError(t, err)
		assert.Equal(t, []access.ItemRef{leadership}, h.db.thread("thread1").Granted.Items)
	})
	// An ask that returns before its answer records what its question read
	// then; the get_question that reads the answer, what the ask worker
	// recorded since (the thread's earlier answers it was told).
	t.Run("an answer read after the ask returned", func(t *testing.T) {
		h := newHarness(t,
			call("toolu_1", "helix_foundry_ask", `{"workspace_id":"ws1","ontology_id":"ont1","question":"What changed?"}`),
			call("toolu_2", "helix_foundry_get_question", `{"workspace_id":"ws1","ontology_id":"ont1","question_id":"q-general"}`),
			say("Here it is."))
		slackWorkspace(h)
		h.backend.question = &foundryv1.FoundryQuestion{Id: "q-general", Status: "queued"}
		h.model.tick = func() {
			if len(h.model.requests) == 2 && h.backend.question.Status == "queued" {
				assert.Equal(t, []access.ItemRef{general}, h.db.thread("thread1").Granted.Items)
				h.db.questions["q-general"].SeenItems = []access.ItemRef{general, leadership}
				h.backend.question = &foundryv1.FoundryQuestion{Id: "q-general", Status: "answered"}
			}
		}
		_, err := h.runner.Run(context.Background(), message("What changed?"))
		require.NoError(t, err)
		assert.Equal(t, []access.ItemRef{general, leadership}, h.db.thread("thread1").Granted.Items)
	})
	t.Run("a load it can't tell fails the attempt to retry", func(t *testing.T) {
		h := newHarness(t, readLoad("load-gone"), say("Never said."))
		slackWorkspace(h)
		_, err := h.runner.Run(context.Background(), message("What did they say?"))
		var f *Failure
		require.ErrorAs(t, err, &f)
		assert.False(t, f.Permanent)
		assert.Len(t, h.model.requests, 1, "the result never reached the model")
	})
}

// A Slack install's bot's calls are read around as a restricted member's:
// its thread holds the channels of what it read under its key's grants, and
// a call of a channel no longer shared with it during the call fails the
// turn for good.
func TestABotsCallRecordsTheChannelsItRead(t *testing.T) {
	for name, tc := range map[string]struct {
		read   []llm.Chunk
		change func(h *harness)
		holds  []access.ItemRef
		failed bool
	}{
		"a read of #leadership": {read: readLoad("load-leadership"), holds: []access.ItemRef{{ConnectorID: "conn-slack", Item: "C2"}}},
		"a read of #general, #leadership no longer shared during it": {read: readLoad("load-general"),
			change: func(h *harness) { h.backend.items = h.backend.items[:1] }, holds: []access.ItemRef{{ConnectorID: "conn-slack", Item: "C1"}}},
		"a read of #leadership, no longer shared during it": {read: readLoad("load-leadership"),
			change: func(h *harness) { h.backend.items = h.backend.items[:1] }, holds: []access.ItemRef{{ConnectorID: "conn-slack", Item: "C2"}}, failed: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, tc.read, say("Here it is."))
			slackWorkspace(h)
			h.db.grants = []model.FoundryAccessGrant{{Principal: model.AgentBot{CredentialID: botKey}.Access(), Target: access.DocumentSetTarget("set-conv")}}
			if tc.change != nil {
				h.backend.onCall = func() { tc.change(h) }
			}
			turn := botMessage("What did they say?")
			h.db.turns = []*model.FoundryAgentTurn{turn}
			_, err := h.runner.Run(context.Background(), turn)
			assert.Equal(t, tc.holds, h.db.thread(turn.ThreadID).Granted.Items)
			if tc.failed {
				var f *Failure
				require.ErrorAs(t, err, &f)
				assert.True(t, f.Permanent)
				assert.Len(t, h.model.requests, 1, "the result never reached the model")
				return
			}
			require.NoError(t, err)
			assert.Len(t, h.model.requests, 2)
		})
	}
}

// A helper's call records the channels of what it read as the worker's do,
// and returns no result read from a channel she was removed from during it.
func TestAHelpersCallRecordsTheChannelsItRead(t *testing.T) {
	hosts, db, h := newHosts(t)
	slackWorkspace(h)
	turn := message("What did they say?")
	db.turns = []*model.FoundryAgentTurn{turn}
	client := connectHost(t, hosts, turn, func(context.Context, bool) error { return nil })
	read := func(load string) *mcp.CallToolResult {
		t.Helper()
		result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "helix_foundry_read_graph",
			Arguments: map[string]any{"workspace_id": ws, "ontology_id": ontology, "load_id": load, "reads": []any{map[string]any{"query": map[string]any{}}}}})
		require.NoError(t, err)
		return result
	}
	assert.False(t, read("load-general").IsError)
	assert.Equal(t, []access.ItemRef{{ConnectorID: "conn-slack", Item: "C1"}}, db.thread(turn.ThreadID).Granted.Items)

	h.backend.onCall = func() { h.backend.items = h.backend.items[:1] }
	result := read("load-leadership")
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(*mcp.TextContent).Text, "no longer lets you read")
	assert.Contains(t, db.thread(turn.ThreadID).Granted.Items, access.ItemRef{ConnectorID: "conn-slack", Item: "C2"}, "the thread is no longer hers")
	// The turn ends there, as the worker's would, after the call shows.
	require.Len(t, db.settled, 1)
	assert.Equal(t, model.FoundryAgentTurnFailed, db.settled[0].Status)
	assert.Contains(t, db.settled[0].Error, "no longer lets you read")
	events := db.eventTypes()
	assert.Equal(t, []string{"tool_result", "finished"}, events[len(events)-2:])

	// Its CLI's next request finds the thread no longer hers, and ends the
	// turn, as one between calls does.
	_, err := hosts.Server(context.Background(), turn, "host:1", func(context.Context, bool) error { return nil })
	var f *Failure
	require.ErrorAs(t, err, &f)
	assert.True(t, f.Permanent)
	assert.Len(t, db.settled, 2)
}

// A helper's call during which its reader's reach or grants changed makes the
// thread the whole view's and ends the turn, as the worker's does; what its
// load held before it ran is recorded too; and a read of the reader's access
// that fails after it returns its CLI a tool error to call it again, the
// turn going on.
func TestAHelpersCallIsReadAroundAsTheWorkersAre(t *testing.T) {
	setup := func(t *testing.T) (*mcp.ClientSession, *hostMem, *harness, *model.FoundryAgentTurn) {
		hosts, db, h := newHosts(t)
		slackWorkspace(h)
		turn := message("What did they say?")
		db.turns = []*model.FoundryAgentTurn{turn}
		return connectHost(t, hosts, turn, func(context.Context, bool) error { return nil }), db, h, turn
	}
	read := func(t *testing.T, client *mcp.ClientSession, load string) *mcp.CallToolResult {
		t.Helper()
		result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "helix_foundry_read_graph",
			Arguments: map[string]any{"workspace_id": ws, "ontology_id": ontology, "load_id": load, "reads": []any{map[string]any{"query": map[string]any{}}}}})
		require.NoError(t, err)
		return result
	}
	for name, change := range map[string]func(h *harness){
		"made an admin during a call": func(h *harness) { h.backend.grantee = false },
		"a grant given during a call": func(h *harness) {
			h.db.grants = append(h.db.grants, model.FoundryAccessGrant{Principal: access.Principal{Kind: access.PrincipalUser, ID: user},
				Target: access.ConnectorTarget("conn-stripe")})
		},
	} {
		t.Run(name, func(t *testing.T) {
			client, db, h, turn := setup(t)
			h.backend.onCall = func() { change(h) }
			result := read(t, client, "load-general")
			assert.True(t, result.IsError)
			assert.Contains(t, result.Content[0].(*mcp.TextContent).Text, "Your access changed while Founder was answering")
			assert.Nil(t, db.thread(turn.ThreadID).Granted, "the thread is the whole view's, for good")
			require.Len(t, db.settled, 1)
			assert.Equal(t, model.FoundryAgentTurnFailed, db.settled[0].Status)
		})
	}
	t.Run("a day trimmed out of the load during the read", func(t *testing.T) {
		client, db, h, turn := setup(t)
		h.backend.onCall = func() { h.db.holdings["load-leadership"] = access.Holdings{} }
		assert.False(t, read(t, client, "load-leadership").IsError)
		assert.Equal(t, []access.ItemRef{{ConnectorID: "conn-slack", Item: "C2"}}, db.thread(turn.ThreadID).Granted.Items)
	})
	t.Run("access that can't be read after the call", func(t *testing.T) {
		client, db, h, _ := setup(t)
		h.backend.onCall = func() { h.backend.accessErr = status.Error(codes.Unavailable, "the API is down") }
		result := read(t, client, "load-general")
		assert.True(t, result.IsError)
		assert.Contains(t, result.Content[0].(*mcp.TextContent).Text, "call it again")
		assert.NotContains(t, result.Content[0].(*mcp.TextContent).Text, "Helix will retry")
		assert.Empty(t, db.settled, "the turn goes on")
	})
}

// holdingBefore reads the load a graph read is about to read: the input's,
// else the ontology's current one; nothing for another call, or a load or
// ontology that isn't there to read.
func TestHoldingBeforeReadsTheLoadAboutToBeRead(t *testing.T) {
	h := newHarness(t)
	slackWorkspace(h)
	for name, tc := range map[string]struct {
		tool, input, current string
		want                 string
	}{
		"its input's load":                 {toolReadGraph, `{"ontology_id":"ont1","load_id":"load-general"}`, "load-leadership", "load-general"},
		"the ontology's current load":      {toolReadGraph, `{"ontology_id":"ont1"}`, "load-leadership", "load-leadership"},
		"an ontology with no current load": {toolReadGraph, `{"ontology_id":"ont1"}`, "", ""},
		"a load not there to read":         {toolReadGraph, `{"ontology_id":"ont1","load_id":"load-gone"}`, "", ""},
		"an ontology not there":            {toolReadGraph, `{"ontology_id":"ont-gone"}`, "load-leadership", ""},
		"an input that isn't one":          {toolReadGraph, `[]`, "load-leadership", ""},
		"another call":                     {toolAsk, `{"ontology_id":"ont1","load_id":"load-general"}`, "load-leadership", ""},
	} {
		t.Run(name, func(t *testing.T) {
			h.db.ontologies[0].CurrentLoadID = nil
			if tc.current != "" {
				h.db.ontologies[0].CurrentLoadID = &tc.current
			}
			got, err := holdingBefore(context.Background(), h.db, ws, tc.tool, json.RawMessage(tc.input))
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.id)
			assert.Equal(t, h.db.holdings[tc.want], got.holdings)
		})
	}
}
