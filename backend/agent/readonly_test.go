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

	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// readOnly is turn, read-only: a standalone install's Slack user the owner
// allowed, or the owner's own while approving from Slack is off.
func readOnly(turn *model.FoundryAgentTurn) *model.FoundryAgentTurn {
	turn.ReadOnly = true
	return turn
}

// A read-only turn only narrows its reach: a person with the whole view
// reads it all, read-only; a restricted member keeps their grants; a bot
// reads its grants and proposes nothing.
func TestAReadOnlyTurnsReach(t *testing.T) {
	h := newHarness(t)
	session, err := h.host.Session(context.Background(), ports.Actor{}, integration.ConfirmByCaller{})
	require.NoError(t, err)
	for name, tc := range map[string]struct {
		turn    *model.FoundryAgentTurn
		grantee bool
		want    Reach
	}{
		"a person's":               {turn: readOnly(slackMessage("Hi")), want: ReachReadOnly},
		"a restricted member's":    {turn: readOnly(slackMessage("Hi")), grantee: true, want: ReachGrants},
		"a bot's":                  {turn: readOnly(botMessage("Hi")), want: ReachGrants},
		"a person's, not narrowed": {turn: slackMessage("Hi"), want: ReachWorkspace},
	} {
		h.backend.grantee = tc.grantee
		reach, err := turnReach(session, tc.turn)
		require.NoError(t, err, name)
		assert.Equal(t, tc.want, reach, name)
	}
	h.backend.grantee = false
	h.backend.accessErr = status.Error(codes.Unavailable, "down")
	_, err = turnReach(session, readOnly(message("Hi")))
	var f *Failure
	require.ErrorAs(t, err, &f, "a read that fails fails the attempt, read-only or not")
	assert.False(t, f.Permanent)
}

// A read-only turn reads the whole view with a restricted member's tools,
// and its thread records the whole view, not grants.
func TestReadOnlyReadsTheWholeViewWithGranteeTools(t *testing.T) {
	assert.Equal(t, GranteeTools, ReachReadOnly.tools())
	for reach, whole := range map[Reach]bool{ReachWorkspace: true, ReachReadOnly: true, ReachGrants: false, ReachBot: false} {
		assert.Equal(t, whole, reach.wholeView(), "%v", reach)
	}
}

// A read-only turn offers GranteeTools alone, is told so, and prepares,
// runs and makes nothing, though its person may.
func TestAReadOnlyTurnOnlyReadsAndAsks(t *testing.T) {
	h := newHarness(t,
		call("toolu_1", toolPrepare, `{"operation":"create_dashboard","target":"workspace:ws1","payload":{"name":"Invoices","spec":{}}}`),
		call("toolu_2", "helix_foundry_list_ontologies", `{"workspace_id":"ws1"}`),
		say("The install's owner makes dashboards in Foundry."))
	_, err := h.runner.Run(context.Background(), readOnly(slackMessage("Make me a dashboard of invoices")))
	require.NoError(t, err)

	var offered []string
	for _, tool := range h.model.requests[0].Tools {
		offered = append(offered, tool.Name)
	}
	assert.ElementsMatch(t, slices.Collect(maps.Keys(GranteeTools)), offered)
	assert.Contains(t, toolResultOf(t, h, 1).Content, "No tool named")
	assert.False(t, toolResultOf(t, h, 2).IsError, "it reads")
	assert.Empty(t, h.backend.created)
	assert.Empty(t, h.db.actions)
	system := h.model.requests[0].System
	assert.Contains(t, system, readOnlyPrompt)
	assert.NotContains(t, system, granteePrompt)
	assert.Less(t, strings.Index(system, slackPrompt), strings.Index(system, readOnlyPrompt))
	assert.Nil(t, h.db.thread("thread1").Granted, "the whole view's thread")
	assert.Contains(t, readOnlyPrompt, "the install's owner makes changes in Foundry")
}

// A read-only turn's sink allows, records, runs and proposes nothing.
func TestAReadOnlySinkDoesNothing(t *testing.T) {
	h := newHarness(t)
	sink := &actionSink{db: h.db, keys: h.keys, turn: readOnly(message("Make me a dashboard")), maxPending: 3, reach: ReachReadOnly}
	for operation := range Operations {
		assert.False(t, sink.Allows(operation), operation)
	}
	dashboard := integration.PreparedAction{Operation: "create_dashboard", Target: "workspace:" + ws, PayloadHash: strings.Repeat("a", 64)}
	assert.False(t, sink.autoRuns(dashboard))
	_, err := sink.Record(context.Background(), dashboard)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = sink.Propose(context.Background(), integration.ProposedAction{Operation: "create_dashboard", Target: "workspace:" + ws})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Empty(t, h.db.actions)
}

// A helper's read-only turn is told so before the helper's own prompt, and
// its CLI is offered a restricted member's tools.
func TestAHelpersReadOnlyTurn(t *testing.T) {
	hosts, db, _ := newHosts(t)
	turn := readOnly(slackMessage("Who renewed?"))
	turn.Runtime, turn.Attempts = model.FoundryAgentRuntimeClaudeCode, 0
	db.due = []*model.FoundryAgentTurn{turn}
	claimed, err := hosts.Claim(context.Background(), model.FoundryAgentRuntimeClaudeCode, "host:1")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, system(ws)+surfacePrompt(turn)+"\n\n"+readOnlyPrompt+hostSystem(model.FoundryAgentRuntimeClaudeCode), claimed.System)

	hosts, db, _ = newHosts(t)
	turn = readOnly(message("Load the Sales graph"))
	db.turns = []*model.FoundryAgentTurn{turn}
	client := connectHost(t, hosts, turn, func(context.Context, bool) error { return nil })
	listed, err := client.ListTools(context.Background(), nil)
	require.NoError(t, err)
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	assert.ElementsMatch(t, slices.Collect(maps.Keys(GranteeTools)), names)
	assert.Nil(t, db.thread("thread1").Granted, "the whole view's thread")
}
