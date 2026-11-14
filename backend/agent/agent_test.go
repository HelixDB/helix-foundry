package agent

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/agent/llm"
	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/integration"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	ws       = "ws1"
	ontology = "ont1"
	user     = "user-ada"
)

// memStore is the agent's store in memory.
type memStore struct {
	mu       sync.Mutex
	messages []model.FoundryAgentMessage
	events   []json.RawMessage
	turns    []*model.FoundryAgentTurn
	actions  []*model.FoundryAgentAction
	lost     bool
}

func (s *memStore) ListFoundryAgentMessages(context.Context, string) ([]model.FoundryAgentMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.messages), nil
}

func (s *memStore) ListFoundryAgentTurns(context.Context, string, int) ([]*model.FoundryAgentTurn, error) {
	return s.turns, nil
}

func (s *memStore) AppendFoundryAgentMessages(_ context.Context, _, _ string, messages []model.FoundryAgentMessage, _ int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lost {
		return foundrystore.ErrNotFound
	}
	for _, m := range messages {
		m.Seq = len(s.messages) + 1
		s.messages = append(s.messages, m)
	}
	return nil
}

func (s *memStore) AppendFoundryAgentEvents(_ context.Context, _, _ string, events []json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lost {
		return foundrystore.ErrNotFound
	}
	s.events = append(s.events, events...)
	return nil
}

func (s *memStore) RecordFoundryAgentAction(_ context.Context, _ string, a *model.FoundryAgentAction, _ int,
	card func(*model.FoundryAgentAction) (json.RawMessage, error)) (*model.FoundryAgentAction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.actions {
		if existing.TurnID == a.TurnID && existing.PayloadHash == a.PayloadHash {
			return existing, nil
		}
	}
	s.actions = append(s.actions, a)
	event, err := card(a)
	s.events = append(s.events, event)
	return a, err
}

func (s *memStore) GetFoundryAgentAction(_ context.Context, _, id string) (*model.FoundryAgentAction, error) {
	for _, a := range s.actions {
		if a.ID == id {
			return a, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *memStore) GetFoundryOntology(_ context.Context, workspaceID, id string) (*model.FoundryOntology, error) {
	if workspaceID != ws || id != ontology {
		return nil, foundrystore.ErrNotFound
	}
	return &model.FoundryOntology{ID: ontology, WorkspaceID: ws, Name: "Sales graph"}, nil
}

func (s *memStore) GetOntologyVersion(_ context.Context, _, id string) (*model.OntologyVersion, error) {
	return &model.OntologyVersion{ID: id, Version: 3}, nil
}

func (s *memStore) GetFoundryLoad(_ context.Context, _, id string) (*model.FoundryLoad, error) {
	return &model.FoundryLoad{ID: id, Status: model.FoundryLoadReady}, nil
}

// eventTypes are the stored events' types, in order.
func (s *memStore) eventTypes() []string {
	var out []string
	for _, e := range s.events {
		var head struct{ Type string }
		_ = json.Unmarshal(e, &head)
		out = append(out, head.Type)
	}
	return out
}

// scripted answers each call with the next of its steps, and records what it was sent.
type scripted struct {
	steps    [][]llm.Chunk
	err      error
	requests []llm.Request
}

func (m *scripted) Stream(_ context.Context, r llm.Request) iter.Seq2[llm.Chunk, error] {
	m.requests = append(m.requests, r)
	return func(yield func(llm.Chunk, error) bool) {
		if m.err != nil {
			yield(nil, m.err)
			return
		}
		step := m.steps[0]
		m.steps = m.steps[1:]
		for _, c := range step {
			if !yield(c, nil) {
				return
			}
		}
	}
}

func say(text string) []llm.Chunk {
	return []llm.Chunk{llm.Text{Text: text}, llm.Usage{In: 100, Out: 10}, llm.Stop{Reason: llm.StopEnd}}
}

func call(id, name, input string) []llm.Chunk {
	return []llm.Chunk{llm.Reasoning{Provider: ai.KindAnthropic, Signature: "sig", Text: "think"},
		llm.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)}, llm.Usage{In: 100, Out: 10}, llm.Stop{Reason: llm.StopToolUse}}
}

// backend is Foundry's API as the tools call it.
type backend struct {
	foundryv1.FoundryServiceClient
	asked []*foundryv1.AskFoundryQuestionRequest
	loads int
}

func (b *backend) ListFoundryOntologies(context.Context, *foundryv1.ListFoundryOntologiesRequest, ...grpc.CallOption) (*foundryv1.ListFoundryOntologiesResponse, error) {
	return &foundryv1.ListFoundryOntologiesResponse{Ontologies: []*foundryv1.FoundryOntology{{Id: ontology, Name: "Sales graph"}}}, nil
}

func (b *backend) AskFoundryQuestion(_ context.Context, r *foundryv1.AskFoundryQuestionRequest, _ ...grpc.CallOption) (*foundryv1.FoundryQuestionResponse, error) {
	b.asked = append(b.asked, r)
	return &foundryv1.FoundryQuestionResponse{Question: &foundryv1.FoundryQuestion{Id: "q1", Status: "answered"}}, nil
}

func (b *backend) CreateFoundryLoad(context.Context, *foundryv1.CreateFoundryLoadRequest, ...grpc.CallOption) (*foundryv1.FoundryLoadResponse, error) {
	b.loads++
	return &foundryv1.FoundryLoadResponse{Load: &foundryv1.FoundryLoad{Id: "load1"}}, nil
}

// confirmations prepare and consume as a host's do.
type confirmations struct {
	mu   sync.Mutex
	kept map[string]integration.ConfirmationBinding
}

func (c *confirmations) Prepare(_ context.Context, b integration.ConfirmationBinding) (integration.Confirmation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kept == nil {
		c.kept = map[string]integration.ConfirmationBinding{}
	}
	b.ID, b.Token = "conf"+string(rune('a'+len(c.kept))), "hlxc_secret"+string(rune('a'+len(c.kept)))
	c.kept[b.ID] = b
	return integration.Confirmation{ID: b.ID, Token: b.Token, ExpiresAt: time.Now().Add(5 * time.Minute)}, nil
}

func (c *confirmations) Consume(_ context.Context, b integration.ConfirmationBinding) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kept[b.ID] != b {
		return errors.New("mismatch")
	}
	delete(c.kept, b.ID)
	return nil
}

// sessions opens every session on one service, as an owner who may do anything.
type sessions struct {
	service *integration.Service
	actors  []ports.Actor
	err     error
}

func (h *sessions) Session(ctx context.Context, actor ports.Actor, confirm integration.ConfirmMode) (integration.Session, error) {
	h.actors = append(h.actors, actor)
	if h.err != nil {
		return integration.Session{}, h.err
	}
	return integration.Session{Ctx: ctx, Service: h.service, Identity: &integration.Identity{
		User: true, HasPermission: func(integration.Permission) bool { return true },
		Authorize: func(context.Context, any, integration.Grant, ...string) error { return nil },
		Allows:    func(string, string, integration.Grant) bool { return true },
		WithScope: func(ctx context.Context, _ string, _ bool) context.Context { return ctx },
		Confirm:   confirm,
	}, HostTools: func(server *mcp.Server) {
		integration.AddReadTool(h.service, server, &integration.Identity{}, "helix_list_projects", "List projects", "List projects.",
			func(context.Context, *integration.Identity, *integration.Call, struct{}) (map[string]any, int, error) {
				return map[string]any{"projects": []any{}}, 0, nil
			})
		integration.AddReadTool(h.service, server, &integration.Identity{}, "helix_list_databases", "List databases", "List databases.",
			func(context.Context, *integration.Identity, *integration.Call, struct{}) (map[string]any, int, error) {
				return map[string]any{"databases": []any{}}, 0, nil
			})
		integration.AddReadTool(h.service, server, &integration.Identity{}, "helix_list_workspaces", "List workspaces", "List workspaces.",
			func(context.Context, *integration.Identity, *integration.Call, struct{}) (map[string]any, int, error) {
				return map[string]any{}, 0, nil
			})
	}}, nil
}

type fixedAI struct{ err error }

func (f fixedAI) Resolve(context.Context, string) (ai.Resolved, error) {
	return ai.Resolved{Settings: ai.Settings{Provider: ai.Anthropic{}, Models: ai.Roles{Ask: ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}}}}}, f.err
}

type harness struct {
	db      *memStore
	model   *scripted
	backend *backend
	host    *sessions
	runner  *Runner
	keys    *secretbox.Keyring
}

func newHarness(t *testing.T, steps ...[]llm.Chunk) *harness {
	t.Helper()
	keys, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": []byte(strings.Repeat("k", 32))})
	require.NoError(t, err)
	b := &backend{}
	h := &harness{db: &memStore{}, model: &scripted{steps: steps}, backend: b, keys: keys}
	service := (&integration.Service{Foundry: b, Actions: integration.FoundryClients{FoundryServiceClient: b}, Confirmations: &confirmations{},
		Workflows: b, FoundryURL: "http://localhost:3000", Wait: time.Second}).ForAgent()
	h.host = &sessions{service: service}
	catalog, err := ai.VendoredCatalog()
	require.NoError(t, err)
	cfg := config.Defaults().Foundry.Agent
	h.runner = &Runner{DB: h.db, Sessions: h.host, AI: fixedAI{}, Catalog: catalog, Keys: keys, Cfg: cfg, WorkerID: "w1", Now: time.Now,
		Models: func(ai.Resolved, ai.Model, *ai.Catalog) (llm.Model, llm.Pricer, error) {
			return h.model, llm.Pricer{Input: 2000, Output: 10000, CacheRead: 200, CacheWrite: 2500}, nil
		}}
	return h
}

func message(text string) *model.FoundryAgentTurn {
	return &model.FoundryAgentTurn{ID: "turn1", WorkspaceID: ws, ThreadID: "thread1", UserID: user, Kind: model.FoundryAgentTurnMessage,
		Input: text, MaxUSDMicros: 500_000, Attempts: 1}
}

func TestATurnReadsWithToolsAndAnswers(t *testing.T) {
	h := newHarness(t, call("toolu_1", "helix_foundry_list_ontologies", `{"workspace_id":"ws1"}`), say("You have Sales graph."))
	turn := message("What ontologies do I have?")
	turn.PageContext = json.RawMessage(`{"ontology_id":"ont1","load_id":"load9"}`)
	a, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	assert.Equal(t, StopAnswered, a.Stop)
	assert.Equal(t, 2, a.Steps)
	assert.EqualValues(t, 2*(100*2000+10*10000)/1000, a.Spend.USDMicros)
	assert.Equal(t, []string{"started", "tool_call", "tool_result", "text", "finished"}, h.db.eventTypes())
	assert.Equal(t, []ports.Actor{{Source: ports.SourceUser, UserID: user, WorkspaceID: ws}}, h.host.actors, "it acts as its person")
	require.Len(t, h.db.messages, 4)

	first := h.model.requests[0]
	assert.Contains(t, first.System, "ws1")
	text := first.Messages[0].Content[0].(llm.Text).Text
	assert.Contains(t, text, `<page_context>{"ontology_id":"ont1","ontology_name":"Sales graph","load_id":"load9"}</page_context>`)
	second := h.model.requests[1].Messages
	result := second[len(second)-1].Content[0].(llm.ToolResult)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content, "Sales graph")
	assert.Contains(t, result.Content, "untrusted", "results come marked as data")
	// The turn running sends its reasoning back.
	assert.IsType(t, llm.Reasoning{}, second[1].Content[0])
}

// The model sees Tools alone, whatever the session registers.
func TestToolsAreAClosedList(t *testing.T) {
	h := newHarness(t, call("toolu_1", "helix_foundry_execute_operation", `{}`), call("toolu_2", "helix_list_workspaces", `{}`), say("Done."))
	_, err := h.runner.Run(context.Background(), message("Load it"))
	require.NoError(t, err)
	var offered []string
	for _, tool := range h.model.requests[0].Tools {
		offered = append(offered, tool.Name)
		assert.True(t, Tools[tool.Name], tool.Name)
	}
	assert.True(t, slices.IsSorted(offered), "one order, for the cache")
	for _, name := range []string{"helix_foundry_execute_operation", "helix_foundry_complete_document_upload", "helix_foundry_send_pieces",
		"helix_foundry_cancel_question", "helix_foundry_create_workflow", "helix_list_workspaces"} {
		assert.NotContains(t, offered, name)
	}
	// Every name the list allows is a tool a full session offers, so none is misspelled.
	for name := range Tools {
		assert.Contains(t, offered, name)
	}
	refused := h.model.requests[1].Messages
	assert.Contains(t, refused[len(refused)-1].Content[0].(llm.ToolResult).Content, "No tool named")
	refused = h.model.requests[2].Messages
	assert.Contains(t, refused[len(refused)-1].Content[0].(llm.ToolResult).Content, "No tool named")
	assert.Zero(t, h.backend.loads)
}

func TestPreparedActionsGoToThePersonNotTheModel(t *testing.T) {
	h := newHarness(t,
		call("toolu_1", toolPrepare, `{"operation":"create_load","target":"ontology:ws1/ont1","payload":{"version_id":"ver3"}}`),
		call("toolu_2", toolPrepare, `{"operation":"sync_connector","target":"connector:ws1/c1","payload":{}}`),
		say("Click Run to load v3."))
	turn := message("Load the Sales graph")
	h.db.turns = []*model.FoundryAgentTurn{turn}
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	require.Len(t, h.db.actions, 1)
	a := h.db.actions[0]
	assert.Equal(t, "create_load", a.Operation)
	assert.JSONEq(t, `{"ontology_id":"ont1","ontology_name":"Sales graph","version_id":"ver3","version":3}`, string(a.Display))
	assert.True(t, a.TargetNamedByUser, "the person named the ontology")
	token, err := h.keys.Open(a.TokenSealed, a.KeyID, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "hlxc_secreta", string(token))
	assert.Zero(t, h.backend.loads, "preparing runs nothing")
	for _, r := range h.model.requests {
		for _, m := range r.Messages {
			raw, _ := llm.MarshalBlocks(m.Content)
			assert.NotContains(t, string(raw), "hlxc_", "the model never sees a token")
		}
	}
	afterPrepare := h.model.requests[1].Messages
	assert.Contains(t, afterPrepare[len(afterPrepare)-1].Content[0].(llm.ToolResult).Content, "awaiting_person")
	afterRefusal := h.model.requests[2].Messages
	refusal := afterRefusal[len(afterRefusal)-1].Content[0].(llm.ToolResult)
	assert.True(t, refusal.IsError, "an operation outside Operations is refused")
	assert.Contains(t, h.db.eventTypes(), "action_card")
}

func TestTheTargetOnlyTheModelChoseIsFlagged(t *testing.T) {
	h := newHarness(t, call("toolu_1", toolPrepare, `{"operation":"create_load","target":"ontology:ws1/ont1","payload":{"version_id":"ver3"}}`), say("ok"))
	turn := message("Load something")
	h.db.turns = []*model.FoundryAgentTurn{turn}
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	require.Len(t, h.db.actions, 1)
	assert.False(t, h.db.actions[0].TargetNamedByUser)
}

func TestAskSendsTheTurnsOwnClientToken(t *testing.T) {
	h := newHarness(t, call("toolu_9", toolAsk, `{"workspace_id":"ws1","ontology_id":"ont1","question":"Who?","client_token":"mine"}`), say("Ada."))
	_, err := h.runner.Run(context.Background(), message("Who?"))
	require.NoError(t, err)
	require.Len(t, h.backend.asked, 1)
	assert.Equal(t, askClientToken("turn1", "toolu_9"), h.backend.asked[0].GetClientToken())
}

func TestAnAttemptResumesWhatItsTurnCommitted(t *testing.T) {
	h := newHarness(t, say("Here they are."))
	turn := message("List them")
	user, _ := llm.MarshalBlocks([]llm.Block{llm.Text{Text: "List them"}})
	asked, _ := llm.MarshalBlocks([]llm.Block{llm.ToolUse{ID: "toolu_1", Name: "helix_foundry_list_ontologies", Input: json.RawMessage(`{"workspace_id":"ws1"}`)}})
	h.db.messages = []model.FoundryAgentMessage{
		{Seq: 1, TurnID: turn.ID, Role: model.FoundryAgentRoleUser, Content: user},
		{Seq: 2, TurnID: turn.ID, Role: model.FoundryAgentRoleAssistant, Content: asked},
	}
	turn.Steps = 1
	a, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	assert.Equal(t, 1, a.Steps, "the call left unanswered runs before the model is called again")
	require.Len(t, h.model.requests, 1)
	msgs := h.model.requests[0].Messages
	assert.Len(t, msgs, 3)
	assert.Equal(t, "List them", msgs[0].Content[0].(llm.Text).Text, "the turn's message isn't added twice")
}

func TestATurnStopsAtItsCaps(t *testing.T) {
	t.Run("steps", func(t *testing.T) {
		h := newHarness(t, call("toolu_1", "helix_foundry_list_ontologies", `{"workspace_id":"ws1"}`))
		h.runner.Cfg.MaxSteps = 1
		a, err := h.runner.Run(context.Background(), message("Go"))
		require.NoError(t, err)
		assert.Equal(t, StopSteps, a.Stop)
		assert.Len(t, h.model.requests, 1)
	})
	t.Run("budget", func(t *testing.T) {
		h := newHarness(t)
		turn := message("Go")
		turn.MaxUSDMicros = 10
		a, err := h.runner.Run(context.Background(), turn)
		require.NoError(t, err)
		assert.Equal(t, StopBudget, a.Stop)
		assert.Empty(t, h.model.requests, "a call that may pass the reservation isn't made")
	})
	t.Run("wall time", func(t *testing.T) {
		h := newHarness(t)
		start := time.Now()
		calls := 0
		h.runner.Now = func() time.Time { calls++; return start.Add(time.Duration(calls) * time.Hour) }
		a, err := h.runner.Run(context.Background(), message("Go"))
		require.NoError(t, err)
		assert.Equal(t, StopWallTime, a.Stop)
	})
	t.Run("refusal", func(t *testing.T) {
		h := newHarness(t, []llm.Chunk{llm.Usage{In: 10}, llm.Stop{Reason: llm.StopRefusal}})
		a, err := h.runner.Run(context.Background(), message("Go"))
		require.NoError(t, err)
		assert.Equal(t, StopRefused, a.Stop)
	})
}

func TestFailuresSayWhetherToRetry(t *testing.T) {
	failure := func(t *testing.T, h *harness) *Failure {
		t.Helper()
		_, err := h.runner.Run(context.Background(), message("Go"))
		var f *Failure
		require.ErrorAs(t, err, &f)
		return f
	}
	h := newHarness(t)
	h.model.err = &llm.Error{Kind: llm.ErrAuth, Err: errors.New("401")}
	assert.True(t, failure(t, h).Permanent)
	h = newHarness(t)
	h.model.err = &llm.Error{Kind: llm.ErrRateLimited, Err: errors.New("429")}
	assert.False(t, failure(t, h).Permanent)
	h = newHarness(t)
	h.runner.Models = func(ai.Resolved, ai.Model, *ai.Catalog) (llm.Model, llm.Pricer, error) {
		return nil, llm.Pricer{}, llm.ErrUnsupported
	}
	assert.True(t, failure(t, h).Permanent)
	h = newHarness(t)
	h.runner.AI = fixedAI{err: ai.ErrNotConfigured}
	assert.True(t, failure(t, h).Permanent)
	h = newHarness(t)
	h.host.err = errors.New("hydrate failed")
	assert.False(t, failure(t, h).Permanent, "a session that couldn't open is retried, never run with less")
	h = newHarness(t)
	h.host.err = status.Error(codes.PermissionDenied, "not a member")
	assert.True(t, failure(t, h).Permanent, "a person who lost the workspace isn't retried")

	h = newHarness(t, say("x"))
	h.db.lost = true
	_, err := h.runner.Run(context.Background(), message("Go"))
	assert.ErrorIs(t, err, ErrLost)
}

func TestAnActionResultTurnTellsTheModelHowTheClickEnded(t *testing.T) {
	h := newHarness(t, say("It's loading."))
	h.db.actions = []*model.FoundryAgentAction{{ID: "act1", Operation: "create_load", Status: model.FoundryAgentActionSucceeded,
		Result: json.RawMessage(`{"result":{"id":"load1"}}`)}}
	turn := message("act1")
	turn.Kind = model.FoundryAgentTurnActionResult
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	text := h.model.requests[0].Messages[0].Content[0].(llm.Text).Text
	assert.True(t, strings.HasPrefix(text, "[Foundry] The person ran the create_load action"))
	assert.Contains(t, text, `"load1"`)
}

func TestPromptKeepsWhatAModelNeeds(t *testing.T) {
	use := llm.ToolUse{ID: "t1", Name: "helix_foundry_get_load", Input: json.RawMessage(`{}`)}
	all := []entry{
		{turnID: "a", msg: llm.Message{Role: llm.User, Content: []llm.Block{llm.ToolResult{ToolUseID: "gone", Content: "orphan"}}}},
		{turnID: "a", msg: llm.Message{Role: llm.User, Content: []llm.Block{llm.Text{Text: "first"}}}},
		{turnID: "a", msg: llm.Message{Role: llm.Assistant, Content: []llm.Block{llm.Reasoning{Provider: ai.KindAnthropic, Text: "r"}, use}}},
		{turnID: "a", msg: llm.Message{Role: llm.User, Content: []llm.Block{llm.ToolResult{ToolUseID: "t1", Content: strings.Repeat("x", 4000)}}}},
		{turnID: "b", msg: llm.Message{Role: llm.User, Content: []llm.Block{llm.Text{Text: "second"}}}},
		{turnID: "b", msg: llm.Message{Role: llm.Assistant, Content: []llm.Block{use}}},
		{turnID: "c", msg: llm.Message{Role: llm.User, Content: []llm.Block{llm.Text{Text: "third"}}}},
	}
	got := prompt(all, "c", 100_000)
	require.Len(t, got, 7, "starts at a message, and answers b's call it left")
	assert.Equal(t, "first", got[0].Content[0].(llm.Text).Text)
	assert.Len(t, got[1].Content, 1, "an earlier turn's reasoning isn't sent")
	assert.Equal(t, earlierResult, got[2].Content[0].(llm.ToolResult).Content)
	assert.Equal(t, notRun, got[5].Content[0].(llm.ToolResult).Content)

	trimmed := prompt(all, "c", 20)
	assert.Equal(t, "first", trimmed[0].Content[0].(llm.Text).Text, "the first message stays")
	assert.Equal(t, "third", trimmed[len(trimmed)-1].Content[0].(llm.Text).Text)
	assert.Len(t, trimmed, 2, "the oldest turns go, whole")
}

func TestTitle(t *testing.T) {
	assert.Equal(t, "Load the sales graph", Title("  Load the sales graph\nplease"))
	long := Title(strings.Repeat("é", 200))
	assert.Equal(t, maxTitleChars, len([]rune(long)))
	assert.True(t, strings.HasSuffix(long, "…"))
}
