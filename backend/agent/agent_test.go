package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strconv"
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
	// ontologies are the workspace's; Sales graph (ont1) unless a test says.
	ontologies []*model.FoundryOntology
	connectors []*model.Connector
	sets       []*model.FoundryDocumentSet
	documents  []*model.FoundryDocument
	// dashboards are the workspace's; none unless a test says.
	dashboards []*model.FoundryDashboard
	// decided is what the person did with an action by the time the turn
	// prepares it again; nothing when empty.
	decided model.FoundryAgentActionStatus
	lost    bool
	// versionReads counts the versions and loads read.
	versionReads int
	// deltas are every text_delta event appended, though a step's final
	// text dropped it since.
	deltas []json.RawMessage
	// deltaFailures fails that many text_delta writes, as a store that
	// errs for a moment.
	deltaFailures int
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
	if s.deltaFailures > 0 && strings.Contains(string(events[0]), `"text_delta"`) {
		s.deltaFailures--
		return errors.New("lock wait timeout")
	}
	s.events = append(s.events, events...)
	for _, e := range events {
		var head struct{ Type string }
		_ = json.Unmarshal(e, &head)
		if head.Type == "text_delta" {
			s.deltas = append(s.deltas, e)
		}
	}
	return nil
}

func (s *memStore) FinishFoundryAgentText(_ context.Context, _, _ string, step int, events []json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lost {
		return foundrystore.ErrNotFound
	}
	s.events = slices.DeleteFunc(s.events, func(e json.RawMessage) bool {
		var head struct {
			Type string
			Step int
		}
		_ = json.Unmarshal(e, &head)
		return head.Type == "text_delta" && head.Step == step
	})
	s.events = append(s.events, events...)
	return nil
}

func (s *memStore) RecordFoundryAgentAction(_ context.Context, _ string, a *model.FoundryAgentAction, _ int,
	card func(*model.FoundryAgentAction) (json.RawMessage, error)) (*model.FoundryAgentAction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.actions {
		if existing.TurnID == a.TurnID && existing.PayloadHash == a.PayloadHash {
			if s.decided != "" {
				existing.Status = s.decided
			}
			return existing, nil
		}
	}
	s.actions = append(s.actions, a)
	event, err := card(a)
	s.events = append(s.events, event)
	return a, err
}

func (s *memStore) GetFoundryAgentActionByPayload(_ context.Context, turnID, payloadHash string) (*model.FoundryAgentAction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.actions {
		if existing.TurnID == turnID && existing.PayloadHash == payloadHash {
			if s.decided != "" {
				existing.Status = s.decided
			}
			return existing, nil
		}
	}
	return nil, foundrystore.ErrNotFound
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
	for _, o := range s.ontologies {
		if o.WorkspaceID == workspaceID && o.ID == id {
			return o, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *memStore) ListFoundryOntologies(_ context.Context, workspaceID, _ string, _ int) ([]*model.FoundryOntology, string, error) {
	var out []*model.FoundryOntology
	for _, o := range s.ontologies {
		if o.WorkspaceID == workspaceID {
			out = append(out, o)
		}
	}
	return out, "", nil
}

// GetOntologyVersion numbers ver<n> n, and any other version 3.
func (s *memStore) GetOntologyVersion(_ context.Context, _, id string) (*model.OntologyVersion, error) {
	s.versionReads++
	if id == "gone" {
		return nil, foundrystore.ErrNotFound
	}
	n, err := strconv.Atoi(strings.TrimPrefix(id, "ver"))
	if err != nil {
		n = 3
	}
	return &model.OntologyVersion{ID: id, Version: n}, nil
}

// loadedAt is when every load of the store started.
var loadedAt = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

func (s *memStore) GetFoundryLoad(_ context.Context, _, id string) (*model.FoundryLoad, error) {
	s.versionReads++
	return &model.FoundryLoad{ID: id, Status: model.FoundryLoadReady, VersionID: "ver2", Version: 2, CreatedAt: loadedAt}, nil
}

func (s *memStore) GetConnector(_ context.Context, workspaceID, id string) (*model.Connector, error) {
	for _, c := range s.connectors {
		if c.WorkspaceID == workspaceID && c.ID == id {
			return c, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *memStore) ListConnectors(_ context.Context, workspaceID string) ([]*model.Connector, error) {
	var out []*model.Connector
	for _, c := range s.connectors {
		if c.WorkspaceID == workspaceID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *memStore) GetFoundryDocumentSet(_ context.Context, workspaceID, id string) (*model.FoundryDocumentSet, error) {
	for _, set := range s.sets {
		if set.WorkspaceID == workspaceID && set.ID == id {
			return set, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *memStore) GetFoundryDocument(_ context.Context, workspaceID, id string) (*model.FoundryDocument, error) {
	for _, d := range s.documents {
		if d.WorkspaceID == workspaceID && d.ID == id {
			return d, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *memStore) ListFoundryDashboards(_ context.Context, workspaceID string, _ bool) ([]*model.FoundryDashboard, error) {
	var out []*model.FoundryDashboard
	for _, d := range s.dashboards {
		if d.WorkspaceID == workspaceID {
			out = append(out, d)
		}
	}
	return out, nil
}

func (s *memStore) GetFoundryDashboard(_ context.Context, workspaceID, id string) (*model.FoundryDashboard, error) {
	for _, d := range s.dashboards {
		if d.WorkspaceID == workspaceID && d.ID == id {
			return d, nil
		}
	}
	return nil, foundrystore.ErrNotFound
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
	// tick runs before each chunk; nil runs nothing.
	tick func()
	// failAfter fails a step after its chunks, as a stream that broke.
	failAfter error
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
			if m.tick != nil {
				m.tick()
			}
			if !yield(c, nil) {
				return
			}
		}
		if m.failAfter != nil {
			yield(nil, m.failAfter)
		}
	}
}

func say(text string) []llm.Chunk {
	return []llm.Chunk{llm.Text{Text: text}, llm.Usage{In: 100, Out: 10}, llm.Stop{Reason: llm.StopEnd}}
}

func call(id, name, input string) []llm.Chunk {
	return []llm.Chunk{llm.Reasoning{Writer: "w", Signature: "sig", Text: "think"},
		llm.ToolUse{ID: id, Name: name, Input: json.RawMessage(input)}, llm.Usage{In: 100, Out: 10}, llm.Stop{Reason: llm.StopToolUse}}
}

// backend is Foundry's API as the tools call it.
type backend struct {
	foundryv1.FoundryServiceClient
	asked  []*foundryv1.AskFoundryQuestionRequest
	loads  int
	listed int
	// onCall runs on every read the tools make.
	onCall func()
	// reviews are the current load's, by key; reviewErr refuses reading them.
	reviews   []*foundryv1.FoundryReview
	reviewErr error
	// onReviews runs after each read of the reviews.
	onReviews func()
}

func (b *backend) ListConnectorKinds(context.Context, *foundryv1.ListConnectorKindsRequest, ...grpc.CallOption) (*foundryv1.ListConnectorKindsResponse, error) {
	return &foundryv1.ListConnectorKindsResponse{Kinds: []*foundryv1.ConnectorKind{
		{Kind: "stripe", DisplayName: "Stripe", Available: true, SignIn: true},
		{Kind: model.ConnectorKindEvents, DisplayName: "API source", Available: true},
	}}, nil
}

func (b *backend) ListFoundryReviews(_ context.Context, r *foundryv1.ListFoundryReviewsRequest, _ ...grpc.CallOption) (*foundryv1.ListFoundryReviewsResponse, error) {
	if b.reviewErr != nil {
		return nil, b.reviewErr
	}
	out := &foundryv1.ListFoundryReviewsResponse{LoadId: "load1"}
	for _, review := range b.reviews {
		if slices.Contains(r.GetReviewKeys(), review.GetReviewKey()) {
			out.Reviews = append(out.Reviews, review)
		}
	}
	if b.onReviews != nil {
		b.onReviews()
	}
	return out, nil
}

// directory is the host's, holding every database in ws1.
type directory struct{}

func (directory) AuthorizeCreate(context.Context, string, string) error { return nil }
func (directory) TenantWorkspace(context.Context, string) (string, error) {
	return ws, nil
}
func (directory) CreateProject(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (directory) CreateDatabase(context.Context, string, string, string) (string, error) {
	return "", nil
}

// databases are the host's: t1 (sales) in ws1, t2 in another workspace.
type databases struct{ ports.Databases }

func (databases) Get(_ context.Context, tenantID string) (ports.Database, error) {
	switch tenantID {
	case "t1":
		return ports.Database{TenantID: "t1", WorkspaceID: ws, Name: "sales"}, nil
	case "t2":
		return ports.Database{TenantID: "t2", WorkspaceID: "ws2", Name: "theirs"}, nil
	}
	return ports.Database{}, foundrystore.ErrNotFound
}

func (b *backend) ListFoundryOntologies(context.Context, *foundryv1.ListFoundryOntologiesRequest, ...grpc.CallOption) (*foundryv1.ListFoundryOntologiesResponse, error) {
	b.listed++
	if b.onCall != nil {
		b.onCall()
	}
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

// sessions opens every session on one service, as an owner who may do
// anything unless may says otherwise.
type sessions struct {
	service *integration.Service
	actors  []ports.Actor
	err     error
	may     func(integration.Permission) bool
}

func (h *sessions) Session(ctx context.Context, actor ports.Actor, confirm integration.ConfirmMode) (integration.Session, error) {
	h.actors = append(h.actors, actor)
	if h.err != nil {
		return integration.Session{}, h.err
	}
	may := h.may
	if may == nil {
		may = func(integration.Permission) bool { return true }
	}
	return integration.Session{Ctx: ctx, Service: h.service, Identity: &integration.Identity{
		User: true, HasPermission: may,
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

// fixedAI resolves every workspace to Claude, its probe having found check.
type fixedAI struct {
	err   error
	check ai.AssistantCheck
}

func (f fixedAI) Resolve(context.Context, string) (ai.Resolved, error) {
	return ai.Resolved{Settings: ai.Settings{Provider: ai.Anthropic{}, Models: ai.Roles{Ask: ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}}}},
		Report: ai.ProbeReport{Assistant: f.check}}, f.err
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
	h := &harness{db: &memStore{ontologies: []*model.FoundryOntology{{ID: ontology, WorkspaceID: ws, Name: "Sales graph"}}},
		model: &scripted{steps: steps}, backend: b, keys: keys}
	service := (&integration.Service{Foundry: b, Actions: integration.FoundryClients{FoundryServiceClient: b}, Confirmations: &confirmations{},
		Workflows: b, FoundryURL: "http://localhost:3000", Wait: time.Second, Directory: directory{},
		SampleDatabase: &integration.SampleDatabase{Host: "acme-db", DisableTLS: true}, AgentHosts: true, PublicDashboards: true}).ForAgent()
	h.host = &sessions{service: service}
	catalog, err := ai.VendoredCatalog()
	require.NoError(t, err)
	cfg := config.Defaults().Foundry.Agent
	h.runner = &Runner{DB: h.db, Sessions: h.host, AI: fixedAI{}, Catalog: catalog, Keys: keys, Cfg: cfg, WorkerID: "w1", Now: time.Now, Databases: databases{},
		Models: func(ai.Resolved, *ai.Catalog) (llm.Binding, error) {
			return llm.Binding{Model: h.model, Pricer: llm.Pricer{Input: 2000, Output: 10000, CacheRead: 200, CacheWrite: 2500}}, nil
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
		call("toolu_2", toolPrepare, `{"operation":"upload_document","target":"ontology:ws1/ont1","payload":{}}`),
		say("Click Run to load v3."))
	turn := message("Load the Sales graph")
	h.db.turns = []*model.FoundryAgentTurn{turn}
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	require.Len(t, h.db.actions, 1)
	a := h.db.actions[0]
	assert.Equal(t, "create_load", a.Operation)
	assert.JSONEq(t, `{"ontology":{"id":"ont1","name":"Sales graph"},"version":{"id":"ver3","number":3}}`, string(a.Display))
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

// Dashboards are prepared as every action is: a card names the dashboard as
// Foundry has it, and the page the person is on names what they look at.
func TestTheAssistantPreparesDashboards(t *testing.T) {
	h := newHarness(t,
		call("toolu_1", toolPrepare, `{"operation":"update_dashboard","target":"dashboard:ws1/dash1","payload":{"expected_revision":2,"name":"Revenue by month"}}`),
		call("toolu_2", toolPrepare, `{"operation":"create_dashboard","target":"workspace:ws1","payload":{"name":"Pipeline"}}`),
		say("Click Run to rename it."))
	h.db.dashboards = []*model.FoundryDashboard{{ID: "dash1", WorkspaceID: ws, Name: "Revenue", Share: &model.FoundryDashboardShare{}}}
	turn := message("Rename this dashboard")
	turn.PageContext = json.RawMessage(`{"dashboard_id":"dash1"}`)
	h.db.turns = []*model.FoundryAgentTurn{turn}
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	text := h.model.requests[0].Messages[0].Content[0].(llm.Text).Text
	assert.Contains(t, text, `<page_context>{"dashboard_id":"dash1","dashboard_name":"Revenue"}</page_context>`)
	require.Len(t, h.db.actions, 2)
	renamed := h.db.actions[0]
	assert.Equal(t, "update_dashboard", renamed.Operation)
	assert.JSONEq(t, `{"dashboard":{"id":"dash1","name":"Revenue","published":true},"name":"Revenue by month"}`, string(renamed.Display))
	assert.True(t, renamed.TargetNamedByUser, "the page the person was on named it")
	assert.Equal(t, "create_dashboard", h.db.actions[1].Operation)
	assert.JSONEq(t, `{"name":"Pipeline","spec":{"tiles":[]}}`, string(h.db.actions[1].Display))
	for _, op := range []string{"create_dashboard", "update_dashboard", "delete_dashboard", "pin_dashboard", "publish_dashboard", "unpublish_dashboard"} {
		assert.True(t, Operations[op], op)
	}
}

// A page context naming nothing of the workspace tells the model nothing.
func TestAPageOfNothingIsNoPage(t *testing.T) {
	h := newHarness(t, say("Hello."))
	turn := message("Hi")
	turn.PageContext = json.RawMessage(`{"ontology_id":"gone","dashboard_id":"gone"}`)
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	assert.NotContains(t, h.model.requests[0].Messages[0].Content[0].(llm.Text).Text, "page_context")
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
	h.runner.Models = func(ai.Resolved, *ai.Catalog) (llm.Binding, error) {
		return llm.Binding{}, errors.New("llm: anthropic claude-unknown has no price in the catalog")
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
		{turnID: "a", msg: llm.Message{Role: llm.Assistant, Content: []llm.Block{llm.Reasoning{Writer: "w", Text: "r"}, use}}},
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

func TestAPrepareActsInItsTurnsWorkspaceOnly(t *testing.T) {
	h := newHarness(t, call("toolu_1", toolPrepare, `{"operation":"create_load","target":"ontology:ws2/ont1","payload":{"version_id":"ver3"}}`), say("ok"))
	turn := message("Load ont1")
	h.db.turns = []*model.FoundryAgentTurn{turn}
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	assert.Empty(t, h.db.actions, "no card for another workspace's ontology")
	refused := h.model.requests[1].Messages
	result := refused[len(refused)-1].Content[0].(llm.ToolResult)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content, "another workspace")
}

// A replayed attempt that prepares an action its person already decided is
// told so, and shows no card again.
func TestAReplayedPrepareOfADecidedActionIsRefused(t *testing.T) {
	for _, decided := range []model.FoundryAgentActionStatus{model.FoundryAgentActionDismissed, model.FoundryAgentActionExpired} {
		h := newHarness(t,
			call("toolu_1", toolPrepare, `{"operation":"create_load","target":"ontology:ws1/ont1","payload":{"version_id":"ver3"}}`),
			call("toolu_2", toolPrepare, `{"operation":"create_load","target":"ontology:ws1/ont1","payload":{"version_id":"ver3"}}`),
			say("ok"))
		turn := message("Load the Sales graph")
		h.db.turns = []*model.FoundryAgentTurn{turn}
		h.db.decided = decided
		_, err := h.runner.Run(context.Background(), turn)
		require.NoError(t, err)
		require.Len(t, h.db.actions, 1)
		replayed := h.model.requests[2].Messages
		result := replayed[len(replayed)-1].Content[0].(llm.ToolResult)
		assert.True(t, result.IsError, string(decided))
		assert.Contains(t, result.Content, "already decided")
		assert.Equal(t, 1, strings.Count(strings.Join(h.db.eventTypes(), " "), "action_card"), "one card")
	}
}

func TestATargetIsNamedByItsWholeName(t *testing.T) {
	for name, c := range map[string]struct {
		ontologies []string
		said       string
		named      bool
	}{
		"its name":                   {[]string{"Sales graph"}, "load the sales graph please", true},
		"part of a word":             {[]string{"Sales"}, "load the Salesforce data", false},
		"inside a longer ontology's": {[]string{"Sales", "Sales graph"}, "load the Sales graph", false},
		"beside a longer one":        {[]string{"Sales", "Sales graph"}, "load Sales", true},
		"too short to count":         {[]string{"a"}, "load a version", false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, call("toolu_1", toolPrepare, `{"operation":"create_load","target":"ontology:ws1/ont1","payload":{"version_id":"ver3"}}`), say("ok"))
			h.db.ontologies = nil
			for i, n := range c.ontologies {
				id := ontology
				if i > 0 {
					id = fmt.Sprintf("ont%d", i+1)
				}
				h.db.ontologies = append(h.db.ontologies, &model.FoundryOntology{ID: id, WorkspaceID: ws, Name: n})
			}
			turn := message(c.said)
			h.db.turns = []*model.FoundryAgentTurn{turn}
			_, err := h.runner.Run(context.Background(), turn)
			require.NoError(t, err)
			require.Len(t, h.db.actions, 1)
			assert.Equal(t, c.named, h.db.actions[0].TargetNamedByUser)
		})
	}
}

// A turn whose attempt died is resumed: the reservation the month paid for
// the dead attempt isn't spend against the turn's cap.
func TestAReclaimedTurnCallsTheModel(t *testing.T) {
	h := newHarness(t, say("Here."))
	turn := message("Go")
	turn.Attempts = 2
	turn.Spend.USDMicros, turn.LostMicros = turn.MaxUSDMicros, turn.MaxUSDMicros
	a, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	assert.Equal(t, StopAnswered, a.Stop)
	assert.Len(t, h.model.requests, 1)
}

func TestToolCallsAreBoundedPerReplyAndAsksPerTurn(t *testing.T) {
	var uses []llm.Chunk
	for i := range maxCallsPerReply + 2 {
		uses = append(uses, llm.ToolUse{ID: fmt.Sprintf("toolu_%d", i), Name: "helix_foundry_list_ontologies", Input: json.RawMessage(`{"workspace_id":"ws1"}`)})
	}
	for i := range maxAsksPerTurn + 1 {
		uses[i] = llm.ToolUse{ID: fmt.Sprintf("toolu_%d", i), Name: toolAsk,
			Input: json.RawMessage(fmt.Sprintf(`{"workspace_id":"ws1","ontology_id":"ont1","question":"Q%d?"}`, i))}
	}
	h := newHarness(t, append(uses, llm.Usage{In: 100, Out: 10}, llm.Stop{Reason: llm.StopToolUse}), say("Done."))
	_, err := h.runner.Run(context.Background(), message("Go"))
	require.NoError(t, err)
	assert.Len(t, h.backend.asked, maxAsksPerTurn, "asks past the turn's are left")
	results := h.model.requests[1].Messages
	answered := results[len(results)-1].Content
	require.Len(t, answered, maxCallsPerReply+2, "every call is answered")
	var unrun []string
	for _, b := range answered {
		if r := b.(llm.ToolResult); strings.HasPrefix(r.Content, "Not run") {
			unrun = append(unrun, r.ToolUseID)
		}
	}
	assert.Equal(t, []string{"toolu_3", "toolu_8", "toolu_9"}, unrun)
	// The person sees each call the limits cut, and why, by its code: the
	// model's words are for the model.
	assert.Equal(t, []string{"toolu_3:asks", "toolu_8:calls", "toolu_9:calls"}, h.db.notRun())
}

// notRun are the calls whose events say why they never ran, in order, as
// <tool use id>:<reason>, each with no words of the model's.
func (s *memStore) notRun() []string {
	var out []string
	for _, e := range s.events {
		var r struct {
			Type      string       `json:"type"`
			ToolUseID string       `json:"tool_use_id"`
			NotRun    NotRunReason `json:"not_run"`
			IsError   bool         `json:"is_error"`
			Error     *string      `json:"error"`
		}
		_ = json.Unmarshal(e, &r)
		if r.Type == "tool_result" && r.NotRun != "" && r.IsError && r.Error == nil {
			out = append(out, r.ToolUseID+":"+string(r.NotRun))
		}
	}
	return out
}

func TestToolsStopAtTheWallTime(t *testing.T) {
	uses := []llm.Chunk{
		llm.ToolUse{ID: "toolu_1", Name: "helix_foundry_list_ontologies", Input: json.RawMessage(`{"workspace_id":"ws1"}`)},
		llm.ToolUse{ID: "toolu_2", Name: "helix_foundry_list_ontologies", Input: json.RawMessage(`{"workspace_id":"ws1"}`)},
		llm.Usage{In: 100, Out: 10}, llm.Stop{Reason: llm.StopToolUse},
	}
	h := newHarness(t, uses)
	start := time.Now()
	h.runner.Now = func() time.Time { return start }
	h.backend.onCall = func() { h.runner.Now = func() time.Time { return start.Add(time.Hour) } }
	a, err := h.runner.Run(context.Background(), message("Go"))
	require.NoError(t, err)
	assert.Equal(t, StopWallTime, a.Stop)
	assert.Equal(t, 1, h.backend.listed, "the call after the time ran out isn't made")
	var results []llm.ToolResult
	blocks, err := llm.UnmarshalBlocks(h.db.messages[len(h.db.messages)-1].Content)
	require.NoError(t, err)
	for _, b := range blocks {
		results = append(results, b.(llm.ToolResult))
	}
	require.Len(t, results, 2)
	assert.Contains(t, results[1].Content, "ran out of time")
	assert.Equal(t, []string{"toolu_2:time"}, h.db.notRun())
}

// A reply cut off at its output's limit, or a call that isn't JSON, runs no
// tool: the cut call's input may be half written.
func TestACutOffCallNeverRuns(t *testing.T) {
	t.Run("max tokens", func(t *testing.T) {
		h := newHarness(t, []llm.Chunk{llm.Text{Text: "Loading"},
			llm.ToolUse{ID: "toolu_1", Name: toolPrepare, Input: json.RawMessage(`{}`)}, llm.Usage{In: 100, Out: 10}, llm.Stop{Reason: llm.StopMaxTokens}})
		a, err := h.runner.Run(context.Background(), message("Load it"))
		require.NoError(t, err)
		assert.Equal(t, StopMaxTokens, a.Stop)
		blocks, err := llm.UnmarshalBlocks(h.db.messages[len(h.db.messages)-1].Content)
		require.NoError(t, err)
		assert.Equal(t, []llm.Block{llm.Text{Text: "Loading"}}, blocks, "the text is kept, the call isn't")
		assert.Empty(t, h.db.actions)
		assert.Equal(t, []string{"started", "text", "tool_call", "tool_result", "finished"}, h.db.eventTypes())
		assert.Equal(t, []string{"toolu_1:length"}, h.db.notRun(), "the person sees the call cut off")
	})
	t.Run("not JSON", func(t *testing.T) {
		h := newHarness(t, []llm.Chunk{llm.ToolUse{ID: "toolu_1", Name: toolPrepare, Input: json.RawMessage(`{"operation":"create_lo`)},
			llm.Usage{In: 100, Out: 10}, llm.Stop{Reason: llm.StopToolUse}})
		a, err := h.runner.Run(context.Background(), message("Load it"))
		require.NoError(t, err)
		assert.Equal(t, StopAnswered, a.Stop, "nothing to run: the reply ends the turn")
		assert.Empty(t, h.db.actions)
		assert.Equal(t, []string{"toolu_1:input"}, h.db.notRun())
		var called struct{ Input string }
		require.NoError(t, json.Unmarshal(h.db.events[1], &called))
		assert.Equal(t, `{"operation":"create_lo`, called.Input, "its input shows as the text it was")
	})
}

// The check before a call counts the tools' definitions it sends.
func TestTheEstimateCountsTheTools(t *testing.T) {
	h := newHarness(t, say("ok"))
	turn := message("Go")
	pricer := llm.Pricer{Input: 2000, Output: 10000, CacheRead: 200, CacheWrite: 2500}
	// Enough for the system prompt and the message, not for the tools too.
	turn.MaxUSDMicros = pricer.Micros(llm.Usage{In: int64(len(system(ws))/4 + 200), Out: maxOutput})
	a, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	assert.Equal(t, StopBudget, a.Stop)
	assert.Empty(t, h.model.requests)
}

func TestTextShowsAsItStreams(t *testing.T) {
	h := newHarness(t, []llm.Chunk{
		llm.TextDelta{Text: "Hel"}, llm.TextDelta{Text: "lo "}, llm.TextDelta{Text: "the"}, llm.TextDelta{Text: "re."},
		llm.Text{Text: "Hello there."}, llm.Usage{In: 100, Out: 10}, llm.Stop{Reason: llm.StopEnd},
	})
	clock := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	h.runner.Now = func() time.Time { return clock }
	h.model.tick = func() { clock = clock.Add(100 * time.Millisecond) }
	turn := message("Hi")
	turn.Steps = 2 // an earlier attempt called the model twice
	a, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	assert.Equal(t, StopAnswered, a.Stop)
	// Shown every 250 ms of the stream: the first three deltas at 300 ms;
	// the last waits for the next flush, which the call's end makes moot.
	require.Len(t, h.db.deltas, 1)
	assert.JSONEq(t, `{"type":"text_delta","step":3,"text":"Hello the"}`, string(h.db.deltas[0]))
	// The step's text supersedes its deltas, which the store drops.
	assert.Equal(t, []string{"started", "text", "finished"}, h.db.eventTypes())
	for _, e := range h.db.events {
		var said struct {
			Type, Text string
			Step       int
		}
		require.NoError(t, json.Unmarshal(e, &said))
		if said.Type == "text" {
			assert.Equal(t, "Hello there.", said.Text)
			assert.Equal(t, 3, said.Step, "numbered across the turn's attempts")
		}
	}
	// The transcript holds the answer, never the deltas.
	require.Len(t, h.db.messages, 2)
	blocks, err := llm.UnmarshalBlocks(h.db.messages[1].Content)
	require.NoError(t, err)
	assert.Equal(t, []llm.Block{llm.Text{Text: "Hello there."}}, blocks)
}

// A stream that breaks keeps the deltas it showed: the next attempt's
// started event, or the turn's finished one, tells the site to drop them.
func TestABrokenStreamKeepsItsDeltas(t *testing.T) {
	h := newHarness(t, []llm.Chunk{llm.TextDelta{Text: "Half"}, llm.TextDelta{Text: " a"}, llm.TextDelta{Text: " word"}})
	clock := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	h.runner.Now = func() time.Time { return clock }
	h.model.tick = func() { clock = clock.Add(200 * time.Millisecond) }
	h.model.failAfter = &llm.Error{Kind: llm.ErrUnavailable, Err: errors.New("cut off")}
	_, err := h.runner.Run(context.Background(), message("Hi"))
	var f *Failure
	require.ErrorAs(t, err, &f)
	assert.False(t, f.Permanent)
	assert.Equal(t, []string{"started", "text_delta"}, h.db.eventTypes())
	assert.Len(t, h.db.messages, 1, "nothing of the broken answer is committed")
}

// Live text is for show: a write of it that fails keeps its text for the
// next, and the call streams on to its usage, which the turn counts.
func TestALiveTextWriteThatFailsLetsTheCallFinish(t *testing.T) {
	h := newHarness(t, []llm.Chunk{
		llm.TextDelta{Text: "Hel"}, llm.TextDelta{Text: "lo "}, llm.TextDelta{Text: "the"}, llm.TextDelta{Text: "re."},
		llm.Text{Text: "Hello there."}, llm.Usage{In: 100, Out: 10}, llm.Stop{Reason: llm.StopEnd},
	})
	clock := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	h.runner.Now = func() time.Time { return clock }
	h.model.tick = func() { clock = clock.Add(200 * time.Millisecond) }
	h.db.deltaFailures = 1
	a, err := h.runner.Run(context.Background(), message("Hi"))
	require.NoError(t, err)
	assert.Equal(t, StopAnswered, a.Stop)
	assert.Positive(t, a.Spend.USDMicros, "the call's usage is counted")
	require.Len(t, h.db.deltas, 1, "the failed write's text goes with the next")
	assert.JSONEq(t, `{"type":"text_delta","step":1,"text":"Hello there."}`, string(h.db.deltas[0]))
	assert.Equal(t, []string{"started", "text", "finished"}, h.db.eventTypes())
}

// A turn whose own work outgrows the model's window ends before a call the
// window would cut: earlier turns go first, but the running one never does.
func TestATurnPastTheWindowStopsBeforeTheCall(t *testing.T) {
	h := newHarness(t, say("never"))
	h.runner.Models = func(ai.Resolved, *ai.Catalog) (llm.Binding, error) {
		return llm.Binding{Model: h.model, Window: 65_536}, nil
	}
	a, err := h.runner.Run(context.Background(), message(strings.Repeat("long ", 48_000)))
	require.NoError(t, err)
	assert.Equal(t, StopContext, a.Stop)
	assert.Empty(t, h.model.requests, "no call is made")
	assert.Equal(t, []string{"started", "finished"}, h.db.eventTypes())
	// The Assistant's own cap bounds a turn the same way.
	h = newHarness(t, say("never"))
	h.runner.Cfg.ContextTokens = 8_000
	a, err = h.runner.Run(context.Background(), message(strings.Repeat("long ", 10_000)))
	require.NoError(t, err)
	assert.Equal(t, StopContext, a.Stop)
	assert.Empty(t, h.model.requests)
}

// A step that streamed no text writes nothing more: no text, no drop.
func TestAStepWithoutTextShowsNothing(t *testing.T) {
	h := newHarness(t, say(" "))
	_, err := h.runner.Run(context.Background(), message("Hi"))
	require.NoError(t, err)
	assert.Equal(t, []string{"started", "finished"}, h.db.eventTypes())
}

func TestAFailedToolCheckRefusesTheTurn(t *testing.T) {
	for check, words := range map[ai.AssistantCheck]string{
		ai.AssistantNoToolCall: "claude-sonnet-5-5, doesn't call tools",
		ai.AssistantAuth:       "Claude refused the workspace's key",
		ai.AssistantRefused:    "Claude refused the Assistant's calls to claude-sonnet-5-5",
	} {
		h := newHarness(t, say("never"))
		h.runner.AI = fixedAI{check: check}
		_, err := h.runner.Run(context.Background(), message("Hi"))
		var f *Failure
		require.ErrorAs(t, err, &f, check)
		assert.True(t, f.Permanent, check)
		assert.Contains(t, f.Message, words)
		assert.Contains(t, f.Message, "Settings › AI models")
		assert.Empty(t, h.model.requests, "no call is made")
	}
	for _, check := range []ai.AssistantCheck{ai.AssistantUnchecked, ai.AssistantPassed} {
		h := newHarness(t, say("ok"))
		h.runner.AI = fixedAI{check: check}
		_, err := h.runner.Run(context.Background(), message("Hi"))
		assert.NoError(t, err, check)
	}
}

func TestAModelsWindowBoundsTheTranscript(t *testing.T) {
	stored := func(h *harness) {
		block := func(text string) json.RawMessage {
			raw, _ := llm.MarshalBlocks([]llm.Block{llm.Text{Text: text}})
			return raw
		}
		// The thread's first message, then an earlier turn of about 60,000 tokens.
		h.db.messages = []model.FoundryAgentMessage{
			{Seq: 1, TurnID: "turn0", Role: model.FoundryAgentRoleUser, Content: block("First")},
			{Seq: 2, TurnID: "turn0", Role: model.FoundryAgentRoleAssistant, Content: block("Hi")},
			{Seq: 3, TurnID: "turnA", Role: model.FoundryAgentRoleUser, Content: block(strings.Repeat("long ", 48_000))},
			{Seq: 4, TurnID: "turnA", Role: model.FoundryAgentRoleAssistant, Content: block("Noted")},
		}
	}
	bind := func(h *harness, window int) {
		h.runner.Models = func(ai.Resolved, *ai.Catalog) (llm.Binding, error) {
			return llm.Binding{Model: h.model, Window: window}, nil
		}
	}
	// No window: the Assistant's own cap, 100,000 tokens, holds the earlier turn.
	h := newHarness(t, say("ok"))
	stored(h)
	_, err := h.runner.Run(context.Background(), message("Now"))
	require.NoError(t, err)
	assert.Len(t, h.model.requests[0].Messages, 5)
	// A 65,536-token window leaves room for less: the earlier turns go, the first message stays.
	h = newHarness(t, say("ok"))
	stored(h)
	bind(h, 65_536)
	_, err = h.runner.Run(context.Background(), message("Now"))
	require.NoError(t, err)
	msgs := h.model.requests[0].Messages
	require.Len(t, msgs, 2)
	assert.Equal(t, "First", msgs[0].Content[0].(llm.Text).Text)
	assert.Equal(t, "Now", msgs[1].Content[0].(llm.Text).Text)
	// A window too small for the tools and an answer refuses the turn.
	h = newHarness(t, say("ok"))
	bind(h, 16_384)
	_, err = h.runner.Run(context.Background(), message("Now"))
	var f *Failure
	require.ErrorAs(t, err, &f)
	assert.True(t, f.Permanent)
	assert.Contains(t, f.Message, "16384 tokens, is too small")
	assert.Empty(t, h.model.requests)
}
