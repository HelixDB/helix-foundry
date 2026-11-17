package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/agent/llm"
	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// hostMem is the store a helper's turns use, in memory: memStore, a queue
// of due turns, and how each claim ended.
type hostMem struct {
	*memStore
	due      []*model.FoundryAgentTurn
	claims   []model.FoundryAgentClaim
	settled  []model.FoundryAgentTurnOutcome
	session  string
	renewals int
	// unclaimed is what EndUnclaimedFoundryAgentTurns was asked to end.
	unclaimed []unclaimedCall
}

type unclaimedCall struct {
	runtime model.FoundryAgentRuntime
	before  time.Time
	why     string
}

func (s *hostMem) EndUnclaimedFoundryAgentTurns(_ context.Context, runtime model.FoundryAgentRuntime, before time.Time, why string) (int64, error) {
	s.unclaimed = append(s.unclaimed, unclaimedCall{runtime, before, why})
	return 1, nil
}

// GetFoundryAgentThread is a helper's thread, holding what the store's says
// of its grants (memStore.thread).
func (s *hostMem) GetFoundryAgentThread(ctx context.Context, workspaceID, id string) (*model.FoundryAgentThread, error) {
	t, err := s.memStore.GetFoundryAgentThread(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	t.Runtime, t.RuntimeSessionID = model.FoundryAgentRuntimeClaudeCode, s.session
	return t, nil
}

func (s *hostMem) ListFoundryAgentEvents(context.Context, string, int, int) ([]model.FoundryAgentEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]model.FoundryAgentEvent, 0, len(s.events))
	for i, e := range s.events {
		out = append(out, model.FoundryAgentEvent{Seq: i + 1, Event: e})
	}
	return out, nil
}

func (s *hostMem) ClaimFoundryAgentTurn(_ context.Context, _ string, claim model.FoundryAgentClaim) (*model.FoundryAgentTurn, error) {
	s.claims = append(s.claims, claim)
	if len(s.due) == 0 {
		return nil, nil
	}
	turn := s.due[0]
	s.due = s.due[1:]
	turn.Attempts++
	if turn.StartedAt == nil {
		now := time.Now()
		turn.StartedAt = &now
	}
	return turn, nil
}

func (s *hostMem) RenewFoundryAgentTurnLease(context.Context, string, string, time.Duration) error {
	if s.lost {
		return foundrystore.ErrNotFound
	}
	s.renewals++
	return nil
}

func (s *hostMem) SettleFoundryAgentTurn(_ context.Context, _, _ string, o model.FoundryAgentTurnOutcome) error {
	if s.lost {
		return foundrystore.ErrNotFound
	}
	s.settled = append(s.settled, o)
	return nil
}

func (s *hostMem) SetFoundryAgentThreadSession(_ context.Context, _, _, session string) error {
	if s.lost {
		return foundrystore.ErrNotFound
	}
	s.session = session
	return nil
}

// lastEvent is the newest event, decoded.
func (s *hostMem) lastEvent(t *testing.T) map[string]any {
	t.Helper()
	require.NotEmpty(t, s.events)
	var out map[string]any
	require.NoError(t, json.Unmarshal(s.events[len(s.events)-1], &out))
	return out
}

func newHosts(t *testing.T) (*Hosts, *hostMem, *harness) {
	t.Helper()
	h := newHarness(t)
	db := &hostMem{memStore: h.db}
	return &Hosts{DB: db, Sessions: h.host, Keys: h.keys, Databases: databases{}, Cfg: config.Defaults().Foundry.Agent, Now: time.Now}, db, h
}

func TestAHelperClaimOpensItsTurnOnce(t *testing.T) {
	hosts, db, _ := newHosts(t)
	_, err := hosts.Claim(context.Background(), model.FoundryAgentRuntimeAPI, "host:1")
	assert.Error(t, err, "a helper never claims the worker's turns")

	none, err := hosts.Claim(context.Background(), model.FoundryAgentRuntimeClaudeCode, "host:1")
	require.NoError(t, err)
	assert.Nil(t, none)

	turn := message("Load the Sales graph")
	turn.Runtime = model.FoundryAgentRuntimeClaudeCode
	turn.Attempts = 0
	turn.PageContext = json.RawMessage(`{"ontology_id":"ont1"}`)
	db.due = []*model.FoundryAgentTurn{turn}
	db.session = "session-7"
	before := time.Now()
	claimed, err := hosts.Claim(context.Background(), model.FoundryAgentRuntimeClaudeCode, "host:1")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, model.FoundryAgentRuntimeClaudeCode, db.claims[len(db.claims)-1].Runtime)
	assert.Equal(t, hosts.Cfg.PerHour, db.claims[0].Limits.PerHour, "a helper's turns count toward the hour's")
	assert.Contains(t, claimed.System, "workspace's id is ws1")
	assert.Contains(t, claimed.System, "MCP server named foundry")
	assert.Contains(t, claimed.Prompt, `<page_context>{"ontology_id":"ont1","ontology_name":"Sales graph"}</page_context>`)
	assert.Contains(t, claimed.Prompt, "Load the Sales graph")
	assert.Equal(t, "session-7", claimed.Resume)
	assert.Empty(t, claimed.History, "a first turn has none")
	assert.Equal(t, hosts.Cfg.MaxSteps, claimed.MaxSteps)
	assert.WithinDuration(t, before.Add(hosts.Cfg.Lease), claimed.Lease, 5*time.Second)
	assert.WithinDuration(t, before.Add(time.Duration(hosts.Cfg.MaxWallSecs)*time.Second), claimed.Deadline, 5*time.Second)
	assert.Equal(t, []string{"started"}, db.eventTypes())
	assert.Equal(t, "Claude Code", db.lastEvent(t)["model"])
	require.Len(t, db.messages, 1)

	// Claimed again (its helper stopped before it showed anything), it runs
	// again, and its message isn't added twice.
	db.due = []*model.FoundryAgentTurn{turn}
	again, err := hosts.Claim(context.Background(), model.FoundryAgentRuntimeClaudeCode, "host:2")
	require.NoError(t, err)
	require.NotNil(t, again)
	assert.Len(t, db.messages, 1)
	assert.Equal(t, []string{"started", "started"}, db.eventTypes())
}

func TestAHelperClaimEndsTheTurnsItCantRun(t *testing.T) {
	hosts, db, _ := newHosts(t)
	spent := message("one")
	spent.ID, spent.Runtime, spent.Attempts = "turn-spent", model.FoundryAgentRuntimeCodex, hosts.Cfg.MaxAttempts
	started := message("two")
	started.ID, started.Runtime, started.Attempts = "turn-started", model.FoundryAgentRuntimeCodex, 1
	fresh := message("three")
	fresh.ID, fresh.Runtime, fresh.Attempts = "turn-fresh", model.FoundryAgentRuntimeCodex, 0
	db.due = []*model.FoundryAgentTurn{spent}
	claimed, err := hosts.Claim(context.Background(), model.FoundryAgentRuntimeCodex, "host:1")
	require.NoError(t, err)
	assert.Nil(t, claimed)
	require.Len(t, db.settled, 1)
	assert.Equal(t, model.FoundryAgentTurnFailed, db.settled[0].Status)
	assert.Contains(t, db.settled[0].Error, "stopped without finishing")

	// A turn an earlier claim showed a tool call of ends interrupted, and the
	// claim takes the next.
	raw, err := MarshalEvent(ToolCalled{ToolUseID: "mcp_1", Name: "helix_foundry_list_ontologies", Input: json.RawMessage(`{}`)})
	require.NoError(t, err)
	db.events = []json.RawMessage{raw}
	db.due = []*model.FoundryAgentTurn{started, fresh}
	claimed, err = hosts.Claim(context.Background(), model.FoundryAgentRuntimeCodex, "host:1")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(t, "turn-fresh", claimed.Turn.ID)
	assert.Contains(t, claimed.System, "MCP server named "+CodexMCPServer, "Codex's model is told which server is Foundry's")
	assert.Contains(t, claimed.System, "never use them", "and to leave the person's own tools alone")
	assert.Equal(t, "Codex", db.lastEvent(t)["model"])
	require.Len(t, db.settled, 2)
	assert.Contains(t, db.settled[1].Error, "stopped before it finished")
	assert.Zero(t, db.settled[1].Spend.USDMicros, "a helper's turn spends nothing")
}

func TestAHelperIsToldTheThreadInWords(t *testing.T) {
	text := func(s string) json.RawMessage {
		raw, _ := llm.MarshalBlocks([]llm.Block{llm.Text{Text: s}})
		return raw
	}
	result, _ := llm.MarshalBlocks([]llm.Block{llm.ToolResult{ToolUseID: "t1", Content: "secret rows"}})
	all, err := entries([]model.FoundryAgentMessage{
		{TurnID: "a", Role: model.FoundryAgentRoleUser, Content: text("Which ontologies?")},
		{TurnID: "a", Role: model.FoundryAgentRoleUser, Content: result},
		{TurnID: "a", Role: model.FoundryAgentRoleAssistant, Content: text("You have Sales graph.")},
		{TurnID: "b", Role: model.FoundryAgentRoleUser, Content: text("Load it")},
	})
	require.NoError(t, err)
	told := history(all, "b")
	assert.Contains(t, told, "The person: Which ontologies?")
	assert.Contains(t, told, "You: You have Sales graph.")
	assert.NotContains(t, told, "secret rows", "tool results are read again, not retold")
	assert.NotContains(t, told, "Load it", "the turn's own message is its prompt")

	long := strings.Repeat("x", maxHistoryChars/2)
	all = append(all, entry{turnID: "c", msg: llm.Message{Role: llm.User, Content: []llm.Block{llm.Text{Text: long}}}},
		entry{turnID: "c", msg: llm.Message{Role: llm.Assistant, Content: []llm.Block{llm.Text{Text: long}}}})
	told = history(all, "d")
	assert.Contains(t, told, "[Earlier turns left out.]")
	assert.NotContains(t, told, "Which ontologies?", "the oldest go first")
	assert.LessOrEqual(t, len(told), maxHistoryChars+200)
}

func TestAHelpersTextIsShownThenKept(t *testing.T) {
	hosts, db, _ := newHosts(t)
	ctx := context.Background()
	require.NoError(t, hosts.Text(ctx, "turn1", "host:1", HostText{Step: 1, Text: "Load"}))
	require.NoError(t, hosts.Text(ctx, "turn1", "host:1", HostText{Step: 1, Text: "ing v3"}))
	assert.Equal(t, []string{"text_delta", "text_delta"}, db.eventTypes())
	require.NoError(t, hosts.Text(ctx, "turn1", "host:1", HostText{Step: 1, Text: "Loading v3.", Final: true}))
	assert.Equal(t, []string{"text"}, db.eventTypes(), "the final text replaces the step's deltas")
	require.Len(t, db.messages, 1)
	assert.Equal(t, model.FoundryAgentRoleAssistant, db.messages[0].Role)

	// A blank final text shows nothing and keeps nothing.
	require.NoError(t, hosts.Text(ctx, "turn1", "host:1", HostText{Step: 2, Text: "  ", Final: true}))
	assert.Len(t, db.messages, 1)

	for _, bad := range []HostText{{Step: 0, Text: "x"}, {Step: hosts.Cfg.MaxSteps + 1, Text: "x"}, {Step: 1, Text: string([]byte{0xff})},
		{Step: 1, Text: strings.Repeat("x", 64<<10+1)}} {
		assert.Equal(t, codes.InvalidArgument, status.Code(hosts.Text(ctx, "turn1", "host:1", bad)))
	}
	db.lost = true
	assert.ErrorIs(t, hosts.Text(ctx, "turn1", "host:1", HostText{Step: 1, Text: "x"}), ErrLost)
	assert.ErrorIs(t, hosts.Text(ctx, "turn1", "host:1", HostText{Step: 1, Text: "x", Final: true}), ErrLost)
}

func TestAHelperEndsItsTurnInFoundrysWords(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		runtime model.FoundryAgentRuntime
		outcome HostOutcome
		status  model.FoundryAgentTurnStatus
		reason  StopReason
		says    string
	}{
		{"answered", model.FoundryAgentRuntimeClaudeCode, HostOutcome{End: HostAnswered{}, Steps: 3, Session: "s-1"}, model.FoundryAgentTurnDone, StopAnswered, ""},
		{"steps", model.FoundryAgentRuntimeClaudeCode, HostOutcome{End: HostStopped{Reason: StopSteps}, Steps: 20}, model.FoundryAgentTurnDone, StopSteps, ""},
		{"signed out", model.FoundryAgentRuntimeClaudeCode, HostOutcome{End: HostFailed{Kind: HostSignedOut}}, model.FoundryAgentTurnFailed, StopFailed, "Run claude, then /login"},
		{"codex signed out", model.FoundryAgentRuntimeCodex, HostOutcome{End: HostFailed{Kind: HostSignedOut}}, model.FoundryAgentTurnFailed, StopFailed, "Run codex login"},
		{"not installed", model.FoundryAgentRuntimeCodex, HostOutcome{End: HostFailed{Kind: HostNotInstalled}}, model.FoundryAgentTurnFailed, StopFailed, "can't find Codex"},
		{"rate limited", model.FoundryAgentRuntimeClaudeCode, HostOutcome{End: HostFailed{Kind: HostRateLimited}}, model.FoundryAgentTurnFailed, StopFailed, "usage limit"},
		{"cli error", model.FoundryAgentRuntimeClaudeCode, HostOutcome{End: HostFailed{Kind: HostCLIError, Detail: "boom\n\nat line 2"}}, model.FoundryAgentTurnFailed, StopFailed,
			"Claude Code stopped with an error: boom at line 2"},
		{"cli error without words", model.FoundryAgentRuntimeCodex, HostOutcome{End: HostFailed{Kind: HostCLIError}}, model.FoundryAgentTurnFailed, StopFailed,
			"Codex stopped with an error. Send"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hosts, db, _ := newHosts(t)
			require.NoError(t, hosts.Finish(ctx, &model.FoundryAgentTurn{ID: "turn1", Runtime: tc.runtime}, "host:1", tc.outcome))
			last := db.lastEvent(t)
			assert.Equal(t, "finished", last["type"])
			assert.Equal(t, string(tc.status), last["status"])
			assert.Equal(t, string(tc.reason), last["reason"])
			require.Len(t, db.settled, 1)
			assert.Equal(t, tc.status, db.settled[0].Status)
			assert.Equal(t, tc.outcome.Steps, db.settled[0].Steps)
			assert.Zero(t, db.settled[0].Spend.USDMicros)
			assert.Contains(t, db.settled[0].Error, tc.says)
			assert.Equal(t, tc.outcome.Session, db.session)
		})
	}
	hosts, db, _ := newHosts(t)
	long := HostOutcome{End: HostFailed{Kind: HostCLIError, Detail: strings.Repeat("é", 2*maxHostDetail)}}
	require.NoError(t, hosts.Finish(ctx, &model.FoundryAgentTurn{ID: "turn1", Runtime: model.FoundryAgentRuntimeCodex}, "host:1", long))
	assert.LessOrEqual(t, len([]rune(db.settled[0].Error)), maxHostDetail+40)

	for _, bad := range []HostOutcome{
		{End: HostStopped{Reason: StopBudget}}, {End: HostStopped{Reason: StopFailed}}, {End: HostFailed{Kind: "crashed"}}, {End: nil},
		{End: HostAnswered{}, Steps: -1}, {End: HostAnswered{}, Steps: hosts.Cfg.MaxSteps + 1}, {End: HostAnswered{}, Session: strings.Repeat("s", 129)},
	} {
		assert.Equal(t, codes.InvalidArgument, status.Code(hosts.Finish(ctx, &model.FoundryAgentTurn{ID: "turn1", Runtime: model.FoundryAgentRuntimeCodex}, "host:1", bad)), "%+v", bad)
	}
	db.lost = true
	assert.ErrorIs(t, hosts.Finish(ctx, &model.FoundryAgentTurn{ID: "turn1", Runtime: model.FoundryAgentRuntimeCodex}, "host:1", HostOutcome{End: HostAnswered{}, Session: "s"}), ErrLost)
	assert.ErrorIs(t, hosts.Finish(ctx, &model.FoundryAgentTurn{ID: "turn1", Runtime: model.FoundryAgentRuntimeCodex}, "host:1", HostOutcome{End: HostAnswered{}}), ErrLost)
}

func TestAHelpersTurnEndsAtItsWallTime(t *testing.T) {
	ctx := context.Background()
	hosts, db, _ := newHosts(t)
	lease, err := hosts.Renew(ctx, "turn1", "host:1", time.Now().Add(time.Minute))
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(hosts.Cfg.Lease), lease, 5*time.Second)
	assert.Equal(t, 1, db.renewals)

	_, err = hosts.Renew(ctx, "turn1", "host:1", time.Now().Add(-time.Second))
	assert.ErrorIs(t, err, ErrWallTime)
	assert.Equal(t, 1, db.renewals, "a turn past its time isn't renewed")
	assert.Equal(t, string(StopWallTime), db.lastEvent(t)["reason"])
	require.Len(t, db.settled, 1)
	assert.Equal(t, model.FoundryAgentTurnDone, db.settled[0].Status)

	db.lost = true
	_, err = hosts.Renew(ctx, "turn1", "host:1", time.Now().Add(time.Minute))
	assert.ErrorIs(t, err, ErrLost)
}

// runtimeBackend answers the runtime's reads: Claude Code online on the Mac.
type runtimeBackend struct{ *backend }

func (runtimeBackend) GetFoundryAgentRuntime(context.Context, *foundryv1.GetFoundryAgentRuntimeRequest, ...grpc.CallOption) (*foundryv1.FoundryAgentRuntimeResponse, error) {
	return &foundryv1.FoundryAgentRuntimeResponse{Runtime: &foundryv1.FoundryAgentRuntime{Runtime: "api", Online: []string{"claude_code"},
		Hosts: []*foundryv1.FoundryAgentHost{{Id: "01HOSTAAAAAAAAAAAAAAAAAAAA", Name: "Ada's Mac", Runtimes: []string{"claude_code"}, Online: true}}}}, nil
}

// connectHost connects an MCP client to the server a helper's CLI calls.
func connectHost(t *testing.T, hosts *Hosts, turn *model.FoundryAgentTurn, spend HostSpend) *mcp.ClientSession {
	t.Helper()
	server, err := hosts.Server(context.Background(), turn, "host:1", spend)
	require.NoError(t, err)
	serverSide, clientSide := mcp.NewInMemoryTransports()
	_, err = server.Connect(context.Background(), serverSide, nil)
	require.NoError(t, err)
	client, err := mcp.NewClient(&mcp.Implementation{Name: "claude-code"}, nil).Connect(context.Background(), clientSide, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestAHelpersCLICallsTheAssistantsToolsAsThePerson(t *testing.T) {
	hosts, db, h := newHosts(t)
	ctx := context.Background()
	turn := message("Load the Sales graph")
	db.turns = []*model.FoundryAgentTurn{turn}
	calls, asks := 0, 0
	client := connectHost(t, hosts, turn, func(_ context.Context, ask bool) error {
		calls++
		if ask {
			asks++
		}
		return nil
	})
	listed, err := client.ListTools(ctx, nil)
	require.NoError(t, err)
	var names []string
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		assert.True(t, Tools[tool.Name], tool.Name)
	}
	assert.Contains(t, names, toolPrepare)
	for _, name := range []string{"helix_foundry_execute_operation", "helix_foundry_send_pieces", "helix_list_workspaces"} {
		assert.NotContains(t, names, name)
	}
	assert.Equal(t, []actorRef{{user, ws}}, actorsOf(h), "the tools act as the turn's person, in its workspace")

	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "helix_foundry_list_ontologies", Arguments: map[string]any{"workspace_id": ws}})
	require.NoError(t, err)
	assert.False(t, result.IsError)
	assert.Equal(t, []string{"tool_call", "tool_result"}, db.eventTypes(), "Foundry shows the call, not the helper")
	assert.Equal(t, "helix_foundry_list_ontologies", db.lastEvent(t)["name"])
	assert.NotNil(t, db.lastEvent(t)["output"])

	// A prepared action is a card; the CLI gets its id and no token.
	result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: toolPrepare, Arguments: map[string]any{
		"operation": "create_load", "target": "ontology:ws1/ont1", "payload": map[string]any{"version_id": "ver3"}}})
	require.NoError(t, err)
	require.False(t, result.IsError)
	raw, _ := json.Marshal(result)
	assert.NotContains(t, string(raw), "hlxc_")
	assert.Contains(t, string(raw), "awaiting_person")
	require.Len(t, db.actions, 1)
	assert.True(t, db.actions[0].TargetNamedByUser)
	assert.Contains(t, db.eventTypes(), "action_card")
	assert.Zero(t, h.backend.loads, "preparing runs nothing")

	// The same question in the turn is asked once, whatever token the CLI
	// sends, and under no workflow run it names.
	for _, token := range []string{"a", "b"} {
		_, err = client.CallTool(ctx, &mcp.CallToolParams{Name: toolAsk, Arguments: map[string]any{
			"workspace_id": ws, "ontology_id": ontology, "question": "Who?", "client_token": token, "use_case_run_id": "01RUN_key"}})
		require.NoError(t, err)
	}
	require.Len(t, h.backend.asked, 2)
	assert.Empty(t, h.backend.asked[0].GetWorkflowRunId(), "a person's turn records under no workflow run")
	assert.Equal(t, h.backend.asked[0].GetClientToken(), h.backend.asked[1].GetClientToken())
	assert.NotEqual(t, "a", h.backend.asked[0].GetClientToken())
	assert.Equal(t, 4, calls)
	assert.Equal(t, 2, asks)

	// An action it can't prepare is refused in words.
	result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: toolPrepare, Arguments: map[string]any{
		"operation": "pair_assistant_helper", "target": "workspace:ws1", "payload": map[string]any{}}})
	require.NoError(t, err)
	assert.True(t, result.IsError, "a helper's pairing code would pass through the model")
}

// actorRef is an actor's user and workspace.
type actorRef struct{ user, workspace string }

func actorsOf(h *harness) []actorRef {
	var out []actorRef
	for _, a := range h.host.actors {
		out = append(out, actorRef{a.UserID, a.WorkspaceID})
	}
	return out
}

func TestAHelpersCallsAreCounted(t *testing.T) {
	hosts, db, h := newHosts(t)
	ctx := context.Background()
	turn := message("Go")
	client := connectHost(t, hosts, turn, func(_ context.Context, ask bool) error {
		if ask {
			return ErrHostAsksSpent
		}
		return ErrHostCallsSpent
	})
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "helix_foundry_list_ontologies", Arguments: map[string]any{"workspace_id": ws}})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].(*mcp.TextContent).Text, "at most 25 tool calls")
	assert.Equal(t, "calls", db.lastEvent(t)["not_run"])
	assert.Zero(t, h.backend.listed, "a call past the turn's never runs")

	result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: toolAsk, Arguments: map[string]any{"workspace_id": ws, "ontology_id": ontology, "question": "Who?"}})
	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.Equal(t, "asks", db.lastEvent(t)["not_run"])
	assert.Empty(t, h.backend.asked)

	// A claim that lost its lease answers nothing.
	client = connectHost(t, hosts, turn, func(context.Context, bool) error { return ErrLost })
	_, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "helix_foundry_list_ontologies", Arguments: map[string]any{"workspace_id": ws}})
	assert.Error(t, err)
	db.lost = true
	client = connectHost(t, hosts, turn, func(context.Context, bool) error { return nil })
	_, err = client.CallTool(ctx, &mcp.CallToolParams{Name: "helix_foundry_list_ontologies", Arguments: map[string]any{"workspace_id": ws}})
	assert.Error(t, err, "nothing is shown or run once the lease is lost")
	assert.Zero(t, h.backend.listed)
}

func TestTheAssistantPreparesARuntimeChangeAsACard(t *testing.T) {
	hosts, db, h := newHosts(t)
	ctx := context.Background()
	service := *h.host.service
	service.Foundry = runtimeBackend{h.backend}
	h.host.service = &service
	turn := message("Run new threads on Claude Code")
	db.turns = []*model.FoundryAgentTurn{turn}
	client := connectHost(t, hosts, turn, func(context.Context, bool) error { return nil })
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: toolPrepare, Arguments: map[string]any{
		"operation": "set_assistant_runtime", "target": "workspace:ws1", "payload": map[string]any{"runtime": "claude_code"}}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	require.Len(t, db.actions, 1)
	assert.JSONEq(t, `{"runtime":"claude_code","current":"api","online":true}`, string(db.actions[0].Display))
	assert.True(t, db.actions[0].TargetNamedByUser, "the person named Claude Code")

	result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: toolPrepare, Arguments: map[string]any{
		"operation": "revoke_assistant_helper", "target": "workspace:ws1", "payload": map[string]any{"host_id": "01HOSTAAAAAAAAAAAAAAAAAAAA"}}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	require.Len(t, db.actions, 2)
	assert.JSONEq(t, `{"host":{"id":"01HOSTAAAAAAAAAAAAAAAAAAAA","name":"Ada's Mac"},"runtimes":["claude_code"],"online":true}`, string(db.actions[1].Display))
	assert.False(t, db.actions[1].TargetNamedByUser)

	result, err = client.CallTool(ctx, &mcp.CallToolParams{Name: toolPrepare, Arguments: map[string]any{
		"operation": "revoke_assistant_helper", "target": "workspace:ws1", "payload": map[string]any{"host_id": "01HOSTBBBBBBBBBBBBBBBBBBBB"}}})
	require.NoError(t, err)
	assert.True(t, result.IsError, "a helper that isn't paired is refused")
	assert.True(t, slices.ContainsFunc(result.Content, func(c mcp.Content) bool {
		text, ok := c.(*mcp.TextContent)
		return ok && strings.Contains(text.Text, "no helper")
	}))
}

func TestAHelpersTurnKeepsItsFirstStartsWallTime(t *testing.T) {
	hosts, db, _ := newHosts(t)
	wall := time.Duration(hosts.Cfg.MaxWallSecs) * time.Second
	// Its first claim's helper stopped: the next has what is left of its
	// time, not a fresh wall time.
	started := time.Now().Add(-wall / 2)
	resumed := message("go on")
	resumed.ID, resumed.Runtime, resumed.Attempts, resumed.StartedAt = "turn-resumed", model.FoundryAgentRuntimeClaudeCode, 1, &started
	db.due = []*model.FoundryAgentTurn{resumed}
	claimed, err := hosts.Claim(context.Background(), model.FoundryAgentRuntimeClaudeCode, "host:2")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.WithinDuration(t, started.Add(wall), claimed.Deadline, time.Second)

	// One past its wall time ends at it, as Renew would have ended it, and
	// the claim takes the next.
	db.events = nil
	late := time.Now().Add(-wall - time.Second)
	over := message("long answer")
	over.ID, over.Runtime, over.Attempts, over.StartedAt = "turn-over", model.FoundryAgentRuntimeClaudeCode, 1, &late
	db.due = []*model.FoundryAgentTurn{over}
	claimed, err = hosts.Claim(context.Background(), model.FoundryAgentRuntimeClaudeCode, "host:3")
	require.NoError(t, err)
	assert.Nil(t, claimed)
	require.Len(t, db.settled, 1)
	assert.Equal(t, model.FoundryAgentTurnDone, db.settled[0].Status)
	assert.Equal(t, string(StopWallTime), db.lastEvent(t)["reason"])
	assert.Equal(t, []string{"finished"}, db.eventTypes(), "it doesn't start again")
}

func TestAHelperGivesBackATurnItCantBeTold(t *testing.T) {
	hosts, db, _ := newHosts(t)
	require.NoError(t, hosts.Release(context.Background(), "turn1", "host:1"))
	require.Len(t, db.settled, 1)
	got := db.settled[0]
	assert.Equal(t, model.FoundryAgentTurnQueued, got.Status)
	require.NotNil(t, got.RetryAt)
	assert.WithinDuration(t, time.Now(), *got.RetryAt, 5*time.Second, "due at once, for another helper")
	assert.True(t, got.Uncounted, "the attempt isn't counted")
	db.lost = true
	assert.ErrorIs(t, hosts.Release(context.Background(), "turn1", "host:1"), ErrLost)
}

// onlineHosts is a registry whose helpers run the runtimes online says.
type onlineHosts struct {
	HostRegistry
	online map[model.FoundryAgentRuntime]bool
	err    error
}

func (h onlineHosts) AgentRuntimeOnline(_ context.Context, r model.FoundryAgentRuntime) (bool, error) {
	return h.online[r], h.err
}

func TestAHelpersTurnNoneTakesEnds(t *testing.T) {
	hosts, db, _ := newHosts(t)
	before := time.Now()
	require.NoError(t, hosts.EndUnclaimed(context.Background(), onlineHosts{online: map[model.FoundryAgentRuntime]bool{model.FoundryAgentRuntimeClaudeCode: true}}))
	require.Len(t, db.unclaimed, 1, "Claude Code's helper is online: its turns wait for it")
	got := db.unclaimed[0]
	assert.Equal(t, model.FoundryAgentRuntimeCodex, got.runtime)
	assert.WithinDuration(t, before.Add(-HostWaitFor), got.before, 5*time.Second)
	assert.Contains(t, got.why, "No helper running Codex picked this up within 3 minutes")
	assert.Contains(t, got.why, "Settings › AI models", "the dock links it")

	db.unclaimed = nil
	require.NoError(t, hosts.EndUnclaimed(context.Background(), onlineHosts{}))
	assert.Len(t, db.unclaimed, 2)
	assert.Error(t, hosts.EndUnclaimed(context.Background(), onlineHosts{err: errors.New("down")}))
}

func TestAHelpersSubscriptionIsPrintableASCII(t *testing.T) {
	claude := []model.FoundryAgentRuntime{model.FoundryAgentRuntimeClaudeCode}
	ok := func(sub string) error {
		return ValidHostAccounts([]HostAccount{{Runtime: model.FoundryAgentRuntimeClaudeCode, State: AccountSignedIn, Subscription: sub}}, claude)
	}
	require.NoError(t, ok("max"))
	require.NoError(t, ok("claude_max_20x"))
	for _, bad := range []string{"max\u202e", "max\u0085", "plüs", "pro\n", strings.Repeat("x", 33)} {
		assert.Error(t, ok(bad), "%q", bad)
	}
	assert.True(t, PrintableASCII("darwin-arm64 0.1.0 ~"))
	assert.False(t, PrintableASCII("darwin\u2066"))
}

func TestACodexOlderThanTheFloorIsntRun(t *testing.T) {
	codex, claude := model.FoundryAgentRuntimeCodex, model.FoundryAgentRuntimeClaudeCode
	at := func(v string) HostAccount { return HostAccount{Runtime: codex, State: AccountSignedIn, Version: v} }
	for v, outdated := range map[string]bool{
		MinCodexVersion: false, "0.170.0": false, "1.0.0": false, "0.170.0-alpha.5": false,
		"0.168.9": true, MinCodexVersion + "-alpha.2": true, "garbage": true, "0.169": true,
		// No version read: it didn't start, which updating won't fix.
		"": false,
	} {
		assert.Equal(t, outdated, at(v).Outdated(), "%q", v)
	}
	assert.False(t, HostAccount{Runtime: claude, State: AccountSignedIn}.Outdated(), "Claude Code has no floor")
	both := []model.FoundryAgentRuntime{claude, codex}
	assert.Equal(t, both, Runnable(both, []HostAccount{at("0.170.0")}))
	assert.Equal(t, []model.FoundryAgentRuntime{claude}, Runnable(both, []HostAccount{at("0.150.0")}))
	assert.Equal(t, []model.FoundryAgentRuntime{claude}, Runnable(both, nil), "a Codex it says nothing of")
	assert.Equal(t, []model.FoundryAgentRuntime{claude}, Runnable(both, []HostAccount{{Runtime: codex, State: AccountUnknown}}),
		"a Codex of no version, outdated or not")
	assert.Error(t, ValidHostAccounts([]HostAccount{at(strings.Repeat("1", 65))}, both))
	assert.Error(t, ValidHostAccounts([]HostAccount{at("0.169.0\u202e")}, both))
	assert.NoError(t, ValidHostAccounts([]HostAccount{at("0.170.0-alpha.5")}, both))
}

func TestAPairCommandNamesAMovedPort(t *testing.T) {
	assert.Equal(t, "npx -y "+HelperPackage+" pair hxp_x", PairCommand("hxp_x", ""))
	assert.Equal(t, "npx -y "+HelperPackage+" pair hxp_x", PairCommand("hxp_x", DefaultHelperFoundry))
	assert.Equal(t, "npx -y "+HelperPackage+" pair hxp_x --foundry http://127.0.0.1:8090", PairCommand("hxp_x", "http://127.0.0.1:8090"))
}
