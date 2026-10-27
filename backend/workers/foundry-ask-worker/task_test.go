package foundryaskworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/boost"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/graphproxy"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// fakeAgent serves Ask over gRPC: ask runs each call; requests are what it
// got, and keys each call's model key metadata. GetInfo answers after
// infoDelay, as a sidecar still starting does.
type fakeAgent struct {
	agentv1.UnimplementedAgentServer
	mu        sync.Mutex
	info      *agentv1.GetInfoResponse
	infoDelay time.Duration
	ask       func(req string, stream agentv1.Agent_AskServer) error
	requests  []string
	keys      [][]string
	// probe answers each Probe; probes and probeKeys are what each got.
	probe     func(req string) (string, error)
	probes    []string
	probeKeys [][]string
}

func (f *fakeAgent) Probe(ctx context.Context, req *agentv1.ProbeRequest) (*agentv1.ProbeResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.mu.Lock()
	f.probes = append(f.probes, req.GetRequestJson())
	f.probeKeys = append(f.probeKeys, md.Get(ontologyagent.ModelKeyHeader))
	probe := f.probe
	f.mu.Unlock()
	result, err := probe(req.GetRequestJson())
	if err != nil {
		return nil, err
	}
	return &agentv1.ProbeResponse{ResultJson: result}, nil
}

func (f *fakeAgent) GetInfo(context.Context, *agentv1.GetInfoRequest) (*agentv1.GetInfoResponse, error) {
	f.mu.Lock()
	info, delay := f.info, f.infoDelay
	f.mu.Unlock()
	time.Sleep(delay)
	return info, nil
}

func (f *fakeAgent) Ask(req *agentv1.AskRequest, stream agentv1.Agent_AskServer) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	f.mu.Lock()
	f.requests = append(f.requests, req.GetRequestJson())
	f.keys = append(f.keys, md.Get(ontologyagent.ModelKeyHeader))
	ask := f.ask
	f.mu.Unlock()
	return ask(req.GetRequestJson(), stream)
}

func (f *fakeAgent) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func startAgent(t *testing.T, agent *fakeAgent) *ontologyagent.Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	agentv1.RegisterAgentServer(server, agent)
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(server.Stop)
	client, err := ontologyagent.Dial(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// memStore is the ontology, its tenant on a shared cluster, a ready load of
// version 3 drafted by a generation over Stripe's customers, and one answered
// question earlier in the thread. It records the attempt's events, boosts
// and outcome, in order in log.
// onPro stands in for Autumn: every workspace is on Foundry Pro, so its own
// provider runs.
type onPro struct{}

func (onPro) Status(context.Context, string) (foundryplan.Status, error) {
	return foundryplan.Status{Plan: &foundryplan.Plan{Code: "pro", AutumnPlanID: "foundry-pro", Rank: 1}}, nil
}

type memStore struct {
	mu         sync.Mutex
	ontology   *model.FoundryOntology
	tenant     *ports.Database
	load       *model.FoundryLoad
	version    *model.OntologyVersion
	generation *model.OntologyGeneration
	history    []*model.FoundryQuestion
	// cancelAfter cancels the question once that many appends stored events
	// (0: never): every write of its attempt then finds no lease.
	cancelAfter int
	cancelled   bool
	appends     int
	// appendErr fails the next append, once.
	appendErr error
	// acquireErr fails every boost.
	acquireErr error
	events     []json.RawMessage
	settled    []model.FoundryQuestionOutcome
	acquired   []model.FoundryLoadBoost
	released   []model.FoundryLoadBoost
	// log is the order of append, boost, read, unboost and settle.
	log    []string
	claims []*model.FoundryQuestion
	renews int
	// aiConfig is the workspace's AI setting; nil is Helix's Claude.
	aiConfig *model.FoundryAIConfig
}

func (s *memStore) GetActiveFoundryAIConfig(context.Context, string) (*model.FoundryAIConfig, error) {
	if s.aiConfig == nil {
		return nil, foundrystore.ErrNotFound
	}
	return s.aiConfig, nil
}

func (s *memStore) record(event string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, event)
}

func (s *memStore) GetFoundryOntology(_ context.Context, workspaceID, id string) (*model.FoundryOntology, error) {
	if s.ontology == nil || s.ontology.ID != id || s.ontology.WorkspaceID != workspaceID {
		return nil, foundrystore.ErrNotFound
	}
	return s.ontology, nil
}

func (s *memStore) Get(_ context.Context, id string) (ports.Database, error) {
	if s.tenant.TenantID != id {
		return ports.Database{}, foundrystore.ErrNotFound
	}
	return *s.tenant, nil
}

func (s *memStore) Live(ctx context.Context, id string) (ports.Database, error) {
	return s.Get(ctx, id)
}

func (s *memStore) Endpoint(_ context.Context, _, path string) (string, error) {
	return "https://abc.cluster.example.com" + path, nil
}

func (s *memStore) GetFoundryLoad(_ context.Context, ontologyID, id string) (*model.FoundryLoad, error) {
	if s.load == nil || s.load.ID != id || s.load.OntologyID != ontologyID {
		return nil, foundrystore.ErrNotFound
	}
	return s.load, nil
}

func (s *memStore) GetOntologyVersion(_ context.Context, ontologyID, id string) (*model.OntologyVersion, error) {
	if s.version.ID != id || s.version.OntologyID != ontologyID {
		return nil, foundrystore.ErrNotFound
	}
	return s.version, nil
}

func (s *memStore) GetOntologyGeneration(_ context.Context, _, id string) (*model.OntologyGeneration, error) {
	if s.generation == nil || s.generation.ID != id {
		return nil, foundrystore.ErrNotFound
	}
	return s.generation, nil
}

func (s *memStore) ListConnectors(context.Context, string) ([]*model.Connector, error) {
	return []*model.Connector{{ID: "conn-1", WorkspaceID: "ws-1", Kind: "stripe", Name: "Stripe"}}, nil
}

// ListFoundryDocumentSets knows set-1, Contracts, and set-notes, a Granola
// connection's Notes.
func (s *memStore) ListFoundryDocumentSets(_ context.Context, workspaceID string) ([]*model.FoundryDocumentSet, error) {
	return []*model.FoundryDocumentSet{{ID: "set-1", WorkspaceID: workspaceID, Name: "Contracts"}, {ID: "set-notes", WorkspaceID: workspaceID,
		Name: "Notes", Owner: &model.FoundryDocumentSetOwner{ConnectorID: "conn-1", ConnectorName: "Team Granola", Stream: "notes"}}}, nil
}

// ListFoundryDocumentsByID knows one table file of set-1, orders.csv.
func (s *memStore) ListFoundryDocumentsByID(_ context.Context, workspaceID string, ids []string) (map[string]*model.FoundryDocument, error) {
	out := map[string]*model.FoundryDocument{}
	if slices.Contains(ids, "doc-csv") {
		out["doc-csv"] = &model.FoundryDocument{ID: "doc-csv", WorkspaceID: workspaceID, SetID: "set-1", Name: "orders.csv", MediaType: model.FoundryMediaCSV}
	}
	return out, nil
}

// GetOntologyVersionScope is the version's stored scope; a legacy one reads none here.
func (s *memStore) GetOntologyVersionScope(_ context.Context, ontologyID, id string) ([]string, error) {
	if s.version.ID != id || s.version.OntologyID != ontologyID {
		return nil, foundrystore.ErrNotFound
	}
	if s.version.DocumentSetIDs == nil {
		return []string{}, nil
	}
	return *s.version.DocumentSetIDs, nil
}

func (s *memStore) ListFoundryQuestionHistory(context.Context, *model.FoundryQuestion, int) ([]*model.FoundryQuestion, error) {
	return s.history, nil
}

func (s *memStore) AppendFoundryQuestionEvents(_ context.Context, _, workerID string, events []json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled || workerID != "w1" {
		return foundrystore.ErrNotFound
	}
	if s.appendErr != nil {
		err := s.appendErr
		s.appendErr = nil
		return err
	}
	s.events = append(s.events, events...)
	s.log = append(s.log, "append")
	s.appends++
	if s.appends == s.cancelAfter {
		s.cancelled = true
	}
	return nil
}

func (s *memStore) AcquireLoadBoost(_ context.Context, _ string, b model.FoundryLoadBoost) (ports.BoostChange, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acquireErr != nil {
		return ports.BoostChange{}, s.acquireErr
	}
	s.acquired = append(s.acquired, b)
	s.log = append(s.log, "boost")
	return ports.BoostChange{ClusterID: "shared", Revision: 7}, nil
}

func (s *memStore) ReleaseLoadBoost(_ context.Context, b model.FoundryLoadBoost) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, b)
	s.log = append(s.log, "unboost")
	return nil
}

func (s *memStore) SyncedRevision(context.Context, string) (uint64, error) {
	return 0, errors.New("a question does not wait for the sync")
}

func (s *memStore) ClaimFoundryQuestion(_ context.Context, workerID string, claim model.FoundryQuestionClaim) (*model.FoundryQuestion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if workerID != "w1" || claim.Limits.CapMicros != 30_000_000 || claim.Limits.Running != 2 || claim.Defer <= 0 {
		return nil, fmt.Errorf("unexpected claim %+v", claim)
	}
	if len(s.claims) == 0 {
		return nil, nil
	}
	q := s.claims[0]
	s.claims = s.claims[1:]
	return q, nil
}

func (s *memStore) RenewFoundryQuestionLease(context.Context, string, string, time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renews++
	if s.cancelled {
		return foundrystore.ErrNotFound
	}
	return nil
}

func (s *memStore) SettleFoundryQuestion(_ context.Context, _, _ string, outcome model.FoundryQuestionOutcome) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled {
		return foundrystore.ErrNotFound
	}
	s.settled = append(s.settled, outcome)
	s.log = append(s.log, "settle")
	return nil
}

func (s *memStore) SweepLoadBoosts(context.Context, int) (int, error) { return 0, nil }

// gateway answers every query and records each body.
type gateway struct {
	store  *memStore
	mu     sync.Mutex
	bodies []string
}

func (g *gateway) Query(_ context.Context, _, tenantID string, body []byte, _ http.Header) (helixgateway.Response, error) {
	g.store.record("read")
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bodies = append(g.bodies, tenantID+" "+string(body))
	return helixgateway.Response{StatusCode: 200, ContentType: "application/json", Body: []byte(`{"rows":[]}`)}, nil
}

type harness struct {
	task    *Task
	store   *memStore
	agent   *fakeAgent
	gateway *gateway
	ai      *ai.Resolver
}

// houseKey is Helix's Claude's key in the tests.
const houseKey = "sk-ant-api03-house-sentinel-0123456789"

func newHarness(t *testing.T) *harness {
	t.Helper()
	cfg := config.Defaults()
	cfg.Foundry.Graph.LoadRateLimit = config.GraphLoadRateLimit{RequestsPerSecond: 50, Burst: 100}
	cfg.Foundry.AI.Default = config.AIDefault{Provider: "anthropic", APIKey: houseKey, Models: config.AIModels{
		Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5",
	}}
	// Raised since the questions were asked, with $0.50 each: a question keeps its own cap.
	cfg.Foundry.Ask.MaxUSD = 1
	gen, goal := "gen-1", "Track who signs our contracts"
	def, _ := json.Marshal(ontologyagent.Definition{
		Format: "agent_bundle", Status: ontologyagent.StatusDone, OntologyTOML: "version = \"draft\"\n", MappingsTOML: "ontology = \"draft\"\n",
		Draft: json.RawMessage(`{"items":{"type.Customer":{}}}`), Questions: json.RawMessage(`[{"id":"q1"}]`),
	})
	store := &memStore{
		ontology: &model.FoundryOntology{ID: "ont-1", WorkspaceID: "ws-1", TenantID: "tenant-1", ProjectID: "proj-1"},
		tenant:   &ports.Database{TenantID: "tenant-1", WorkspaceID: "ws-1", ProjectID: "proj-1"},
		load:     &model.FoundryLoad{ID: "load-1", OntologyID: "ont-1", VersionID: "ver-3", Version: 3, Status: model.FoundryLoadReady},
		// The catalog names the sets of the load's version's scope, which
		// may be more than its generation picked (a revision's routes).
		version: &model.OntologyVersion{
			ID: "ver-3", OntologyID: "ont-1", Version: 3, Format: model.OntologyFormatAgentBundle, GenerationID: &gen, Definition: def,
			DocumentSetIDs: &[]string{"set-1", "set-notes", "set-gone"},
		},
		generation: &model.OntologyGeneration{ID: gen, OntologyID: "ont-1", Goal: &goal, Input: model.OntologyGenerationInput{
			Tables:         []model.OntologyGenerationTable{{ConnectorID: "conn-1", Table: "customers"}, {ConnectorID: "conn-gone", Table: "deals"}},
			DocumentSetIDs: []string{"set-1"},
			Documents:      []model.OntologyGenerationDocument{{ID: "doc-csv", SetID: "set-1"}, {ID: "doc-gone", SetID: "set-1"}},
		}},
		history: []*model.FoundryQuestion{{Question: "Who are our customers?", Result: json.RawMessage(answeredResult)}},
	}
	agent := &fakeAgent{info: &agentv1.GetInfoResponse{ContractVersion: ontologyagent.ContractVersion, Providers: []string{"anthropic"}},
		ask: answer(answeredResult)}
	resolver, err := ai.NewResolver(&cfg, store, onPro{})
	if err != nil {
		t.Fatal(err)
	}
	g := &gateway{store: store}
	proxy, err := graphproxy.ServeReadOnly(g, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	task := &Task{
		cfg: cfg.Foundry.Ask, queue: store, boosts: store, workerID: "w1", closeFn: func() error { return nil }, slots: make(chan struct{}, cfg.Foundry.Ask.Concurrency),
		now:    func() time.Time { return time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC) },
		runner: &runner{db: store, dbs: store, boosts: store, agent: startAgent(t, agent), ai: resolver, cfg: &cfg, workerID: "w1", proxy: proxy, flushEvery: 5 * time.Millisecond},
	}
	return &harness{task: task, store: store, agent: agent, gateway: g, ai: resolver}
}

// question is attempt n of a question of load-1, asked at noon.
func question(attempt int) *model.FoundryQuestion {
	return &model.FoundryQuestion{
		ID: "q-2", WorkspaceID: "ws-1", OntologyID: "ont-1", LoadID: "load-1", VersionID: "ver-3", ThreadID: "q-1",
		Question: "Which customers signed this year?", Focus: json.RawMessage(`{"type":"Customer","id":"scus_1"}`), Status: model.FoundryQuestionRunning,
		Attempts: attempt, MaxUSDMicros: 500_000, CreatedAt: time.Date(2026, 10, 19, 11, 59, 0, 0, time.UTC),
	}
}

const (
	answeredResult = `{"status":"answered","answer":{"blocks":[{"para":"Acme signed [c1]."},{"list":["Globex [c2]"]}]},` +
		`"citations":[{"marker":"c1","evidence_id":"e1","target":{"entity":{"type":"Customer","id":"acme"}}},` +
		`{"marker":"c2","evidence_id":"e1","target":{"result":{"evidence_id":"e1"}}}],"queries":[],` +
		`"usage":{"input":1000,"cache_read":200,"cache_write":300,"output":400,"usd":0.031}}`
	cannotResult = `{"status":"cannot_answer","reason":"No link says who signed.","gap":"A signed_by link.","queries":[],` +
		`"usage":{"input":10,"cache_read":0,"cache_write":0,"output":5,"usd":0.002}}`
	budgetResult = `{"status":"budget_exhausted","queries":[],"usage":{"input":10,"cache_read":0,"cache_write":0,"output":5,"usd":0.5}}`
)

// failedResult is a question that failed for reason.
func failedResult(reason ontologyagent.AskFailure) string {
	return `{"status":"failed","reason":"` + string(reason) + `","queries":[],"usage":{"input":10,"cache_read":0,"cache_write":0,"output":5,"usd":0.004}}`
}

// askEvents are a question's events: a read recorded as evidence, a call the
// agent refused, and the answer that ended the question, which has neither.
var askEvents = []string{
	`{"event":"started","model":"claude-sonnet-5-5","provider":"anthropic"}`,
	`{"event":"tool_call","call_id":"c1","tool":"query","input":{"start":{"type":"Customer"}}}`,
	`{"event":"tool_result","call_id":"c1","evidence_id":"e1","summary":"2 rows"}`,
	`{"event":"tool_call","call_id":"c2","tool":"find_entities","input":{"type":"Planet","by":"name","value":"Mars"}}`,
	"{\"event\":\"tool_result\",\"call_id\":\"c2\",\"summary\":\"find_entities\",\"error\":\"no type `Planet`\"}",
	`{"event":"tool_call","call_id":"c3","tool":"answer","input":{"blocks":[{"para":"Acme signed [c1]."}]}}`,
	`{"event":"tool_result","call_id":"c3","summary":"answer: ended the question"}`,
	`{"event":"finished"}`,
}

// decodeAsk reads a request the agent got, and its models block as sent: the
// worker writes models, never reads them.
func decodeAsk(t *testing.T, raw string) (ontologyagent.AskRequest, json.RawMessage) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatal(err)
	}
	models := fields["models"]
	delete(fields, "models")
	rest, _ := json.Marshal(fields)
	var req ontologyagent.AskRequest
	if err := json.Unmarshal(rest, &req); err != nil {
		t.Fatal(err)
	}
	return req, models
}

// helixTarget is the proxy an AskRequest's agent reads through.
func helixTarget(request string) (ontologyagent.HelixTarget, error) {
	var req struct {
		Helix ontologyagent.HelixTarget `json:"helix"`
	}
	err := json.Unmarshal([]byte(request), &req)
	return req.Helix, err
}

// postQuery sends body to target's proxy with token and headers, as the
// agent does, and returns the status.
func postQuery(target ontologyagent.HelixTarget, token, body string, headers map[string]string) (int, error) {
	httpReq, err := http.NewRequest(http.MethodPost, target.URL+graphproxy.QueryPath, strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// readThroughProxy reads once through the request's proxy, then tries a
// write, which the proxy must refuse.
func readThroughProxy(request string) error {
	target, err := helixTarget(request)
	if err != nil {
		return err
	}
	if code, err := postQuery(target, target.Token, `{"request_type":"read","query":{}}`, nil); err != nil || code != http.StatusOK {
		return fmt.Errorf("read: %d %v", code, err)
	}
	if code, err := postQuery(target, target.Token, `{"request_type":"write","query":{}}`, nil); err != nil || code != http.StatusForbidden {
		return fmt.Errorf("write: %d %v", code, err)
	}
	return nil
}

// answer reads through the proxy, sends askEvents one at a time, then result.
func answer(result string) func(string, agentv1.Agent_AskServer) error {
	return func(req string, stream agentv1.Agent_AskServer) error {
		if err := readThroughProxy(req); err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		for _, e := range askEvents {
			if err := stream.Send(&agentv1.AskEvent{Kind: &agentv1.AskEvent_EventJson{EventJson: e}}); err != nil {
				return err
			}
			time.Sleep(10 * time.Millisecond)
		}
		return stream.Send(&agentv1.AskEvent{Kind: &agentv1.AskEvent_ResultJson{ResultJson: result}})
	}
}

func (h *harness) run(t *testing.T, q *model.FoundryQuestion) []model.FoundryQuestionOutcome {
	t.Helper()
	h.task.process(context.Background(), q)
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	return append([]model.FoundryQuestionOutcome(nil), h.store.settled...)
}

func TestAnswer(t *testing.T) {
	// The agent gets the version's definition, the generation's goal and
	// tables, the thread's earlier answers and the question's caps, and reads
	// through a read-only proxy to the ontology's tenant, at the boosted
	// limit; every event is stored, in order, before the result settles the
	// question with what it spent.
	t.Run("answers through a read-only proxy and settles the result", func(t *testing.T) {
		h := newHarness(t)
		settled := h.run(t, question(1))
		if len(settled) != 1 {
			t.Fatalf("settled %+v", settled)
		}
		got := settled[0]
		if got.Status != model.FoundryQuestionAnswered || got.RetryAt != nil || string(got.Result) != answeredResult || got.Error != "" {
			t.Fatalf("outcome = %+v", got)
		}
		if got.Spend != (model.FoundryGraphJobSpend{USDMicros: 31_000, InputTokens: 1500, OutputTokens: 400}) {
			t.Fatalf("spend = %+v", got.Spend)
		}
		if len(h.store.events) != len(askEvents) {
			t.Fatalf("stored %d events", len(h.store.events))
		}
		for i, e := range h.store.events {
			if string(e) != askEvents[i] {
				t.Fatalf("event %d = %s", i, e)
			}
		}
		log := strings.Join(h.store.log, ",")
		if !strings.HasPrefix(log, "boost,read,") || !strings.HasSuffix(log, "unboost,settle") || strings.Contains(log, "settle,append") {
			t.Fatalf("log = %s", log)
		}
		want := model.FoundryLoadBoost{TenantID: "tenant-1", Holder: model.FoundryBoostQuestion, JobID: "q-2", Attempt: 1,
			RequestsPerSecond: 50, Burst: 100, TTL: h.task.cfg.AttemptTimeout() + boost.Margin}
		if len(h.store.acquired) != 1 || h.store.acquired[0] != want || len(h.store.released) != 1 || h.store.released[0] != want {
			t.Fatalf("acquired %+v, released %+v", h.store.acquired, h.store.released)
		}
		if len(h.gateway.bodies) != 1 || h.gateway.bodies[0] != `tenant-1 {"query":{},"request_type":"read"}` {
			t.Fatalf("the gateway saw %v", h.gateway.bodies)
		}

		if err := contracttest.Validate(t, contracttest.Schema(t, "ask_request.json"), []byte(h.agent.calls()[0])); err != nil {
			t.Fatalf("the request does not match ask_request.json: %v", err)
		}
		req, models := decodeAsk(t, h.agent.calls()[0])
		// It runs on Helix's Claude, whose key goes in the call's metadata alone.
		const house = `{"endpoint":{"provider":"anthropic","call_timeout_secs":600},"ask":{"name":"claude-sonnet-5-5","price":"table","reasoning":true}}`
		if string(models) != house || len(h.agent.keys) != 1 || fmt.Sprint(h.agent.keys[0]) != "["+houseKey+"]" ||
			strings.Contains(h.agent.calls()[0], houseKey) {
			t.Fatalf("models %s, keys %v", models, h.agent.keys)
		}
		catalog := []ontologyagent.CatalogEntry{
			{ID: ontologyagent.TableSourceID("conn-1", "customers"), Kind: ontologyagent.CatalogTable, Name: "Stripe / customers"},
			{ID: ontologyagent.TableSourceID("conn-gone", "deals"), Kind: ontologyagent.CatalogTable, Name: "conn-gone / deals"},
			{ID: "doc-csv", Kind: ontologyagent.CatalogTable, Name: "Contracts / orders.csv"},
			{ID: "doc-gone", Kind: ontologyagent.CatalogTable, Name: "doc-gone"},
			{ID: "set-1", Kind: ontologyagent.CatalogDocumentSet, Name: "Contracts"},
			// A connector's set is named as its connection's tables are.
			{ID: "set-notes", Kind: ontologyagent.CatalogDocumentSet, Name: "Team Granola / Notes"},
			{ID: "set-gone", Kind: ontologyagent.CatalogDocumentSet, Name: "set-gone"},
		}
		switch {
		case req.RequestID != "q-2" || req.Database != "tenant-1" || req.Load != "load-1" || req.OntologyVersion != "foundry-v3":
			t.Fatalf("ids = %+v", req)
		case req.OntologyTOML != "version = \"draft\"\n" || req.MappingsTOML != "ontology = \"draft\"\n" ||
			req.DraftJSON != `{"items":{"type.Customer":{}}}` || req.QuestionsJSON != `[{"id":"q1"}]`:
			t.Fatalf("definition = %+v", req)
		case req.Goal != "Track who signs our contracts" || fmt.Sprint(req.Catalog) != fmt.Sprint(catalog):
			t.Fatalf("goal %q, catalog %+v", req.Goal, req.Catalog)
		case req.Question != "Which customers signed this year?" || req.Focus == nil ||
			*req.Focus != (ontologyagent.EntityRef{Type: "Customer", ID: "scus_1"}) || req.AsOf != "2026-10-19T11:59:00Z":
			t.Fatalf("question = %+v", req)
		case len(req.History) != 1 || req.History[0] != (ontologyagent.AskTurn{Question: "Who are our customers?", Answer: "Acme signed [c1].\n\n- Globex [c2]"}):
			t.Fatalf("history = %+v", req.History)
		case req.Limits != (ontologyagent.Limits{MaxUSD: 0.5, MaxSteps: 20, MaxWallSecs: 240}):
			t.Fatalf("limits = %+v", req.Limits)
		case !strings.HasPrefix(req.Helix.URL, "http://127.0.0.1:") || req.Helix.Token == "" || req.Helix.TimeoutSecs != 30:
			t.Fatalf("helix = %+v", req.Helix)
		}
	})

	// The agent stops itself at its wall-time cap; the attempt's own bound,
	// which the call carries, also allows for the wait on a sidecar still
	// starting, so the call has AskAttemptMargin past the cap however long
	// the sidecar took, and an agent at its cap is never cut off first.
	t.Run("gives the agent its wall-time cap and the margin", func(t *testing.T) {
		for _, delay := range []time.Duration{0, 500 * time.Millisecond} {
			h := newHarness(t)
			h.agent.infoDelay = delay
			left := make(chan time.Duration, 1)
			h.agent.ask = func(req string, stream agentv1.Agent_AskServer) error {
				deadline, ok := stream.Context().Deadline()
				if !ok {
					return status.Error(codes.Internal, "the call has no deadline")
				}
				left <- time.Until(deadline)
				return answer(answeredResult)(req, stream)
			}
			if settled := h.run(t, question(1)); len(settled) != 1 || settled[0].Status != model.FoundryQuestionAnswered {
				t.Fatalf("info after %s: settled %+v", delay, settled)
			}
			want, bound := time.Duration(h.task.cfg.MaxWallSecs)*time.Second+config.AskAttemptMargin, h.task.cfg.AttemptTimeout()
			if got := <-left; got < want || got > bound {
				t.Fatalf("info after %s: the agent's call had %s left, want at least %s and at most %s", delay, got, want, bound)
			}
		}
	})

	// The pod's one read-only proxy serves every question: a question's token
	// reaches its database only while its attempt runs, so nothing the agent
	// sends once the attempt is over (or the boost released) is read.
	t.Run("ends the question's token with its attempt", func(t *testing.T) {
		h := newHarness(t)
		if settled := h.run(t, question(1)); len(settled) != 1 || settled[0].Status != model.FoundryQuestionAnswered {
			t.Fatalf("settled %+v", settled)
		}
		target, err := helixTarget(h.agent.calls()[0])
		if err != nil {
			t.Fatal(err)
		}
		code, err := postQuery(target, target.Token, `{"request_type":"read","query":{}}`, nil)
		if err != nil || code != http.StatusUnauthorized || len(h.gateway.bodies) != 1 {
			t.Fatalf("a read after the attempt: status %d (%v), the gateway saw %v", code, err, h.gateway.bodies)
		}
	})

	// The fake agent's events and results are ones the agent sends.
	t.Run("speaks the vendored contract", func(t *testing.T) {
		events, results := contracttest.Schema(t, "ask_event.json"), contracttest.Schema(t, "ask_result.json")
		for _, e := range askEvents {
			if err := contracttest.Validate(t, events, []byte(e)); err != nil {
				t.Errorf("event %s: %v", e, err)
			}
		}
		for _, r := range []string{answeredResult, cannotResult, budgetResult, failedResult(ontologyagent.AskNoAnswer)} {
			if err := contracttest.Validate(t, results, []byte(r)); err != nil {
				t.Errorf("result %s: %v", r, err)
			}
		}
	})

	// A question that failed says why, in the user's words, for each reason
	// the agent gives; it keeps the result and is charged what it spent.
	t.Run("words each failure's reason", func(t *testing.T) {
		for reason, want := range map[ontologyagent.AskFailure]string{
			ontologyagent.AskModelFailed: "could not be reached", ontologyagent.AskModelRefused: "declined",
			ontologyagent.AskNoAnswer: "without an answer", ontologyagent.AskLoadUnknown: "no longer in the database",
			ontologyagent.AskLoadMismatch: "another version", ontologyagent.AskStoreFailed: "graph could not be read",
		} {
			h := newHarness(t)
			h.agent.ask = answer(failedResult(reason))
			settled := h.run(t, question(1))
			if len(settled) != 1 || settled[0].Status != model.FoundryQuestionFailed || settled[0].RetryAt != nil ||
				!strings.Contains(settled[0].Error, want) || string(settled[0].Result) != failedResult(reason) || settled[0].Spend.USDMicros != 4_000 {
				t.Fatalf("%s: settled %+v", reason, settled)
			}
		}
	})

	// A question that can't be answered keeps the agent's reason and gap
	// for the website; one that ran out of its caps fails, keeping what it found.
	t.Run("settles cannot_answer and a budget stop", func(t *testing.T) {
		for result, want := range map[string]model.FoundryQuestionStatus{
			cannotResult: model.FoundryQuestionCannotAnswer, budgetResult: model.FoundryQuestionFailed,
		} {
			h := newHarness(t)
			h.agent.ask = answer(result)
			settled := h.run(t, question(1))
			if len(settled) != 1 || settled[0].Status != want || string(settled[0].Result) != result {
				t.Fatalf("%s: settled %+v", result, settled)
			}
			if want == model.FoundryQuestionFailed && !strings.Contains(settled[0].Error, "$0.50, 20 steps or 240 seconds") {
				t.Fatalf("error = %q", settled[0].Error)
			}
		}
	})

	// A question with no thread history and a version with no generation
	// and no set still asks, with empty arrays where the schema needs them.
	t.Run("asks without history or a generation", func(t *testing.T) {
		h := newHarness(t)
		h.store.history, h.store.version.GenerationID, h.store.version.DocumentSetIDs = nil, nil, &[]string{}
		if settled := h.run(t, question(1)); len(settled) != 1 || settled[0].Status != model.FoundryQuestionAnswered {
			t.Fatalf("settled %+v", settled)
		}
		raw := h.agent.calls()[0]
		if !strings.Contains(raw, `"history":[]`) || !strings.Contains(raw, `"catalog":[]`) || !strings.Contains(raw, `"goal":""`) {
			t.Fatalf("request = %s", raw)
		}
	})
}

func TestSharedReadProxy(t *testing.T) {
	// The pod's questions share its one read-only proxy, each with a token of
	// its own: a question's reads reach only its own ontology's database
	// while another question runs beside it, and the other question ending
	// takes only its own token away.
	t.Run("routes each question's reads to its own database", func(t *testing.T) {
		a, b := newHarness(t), newHarness(t)
		// b asks load-2 of ont-2, bound to tenant-2, through a's proxy.
		b.task.runner.proxy = a.task.runner.proxy
		b.store.ontology = &model.FoundryOntology{ID: "ont-2", WorkspaceID: "ws-1", TenantID: "tenant-2", ProjectID: "proj-1"}
		b.store.tenant = &ports.Database{TenantID: "tenant-2", WorkspaceID: "ws-1", ProjectID: "proj-1"}
		b.store.load = &model.FoundryLoad{ID: "load-2", OntologyID: "ont-2", VersionID: "ver-3", Version: 3, Status: model.FoundryLoadReady}
		b.store.version.OntologyID = "ont-2"
		qb := question(1)
		qb.ID, qb.OntologyID, qb.LoadID = "q-b", "ont-2", "load-2"

		var mu sync.Mutex
		var failures []string
		fail := func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			failures = append(failures, fmt.Sprintf(format, args...))
		}
		read := func(target ontologyagent.HelixTarget, token, from string, want int) {
			code, err := postQuery(target, token, `{"request_type":"read","query":{"from":"`+from+`"}}`, nil)
			if err != nil || code != want {
				fail("%s: status %d (%v), want %d", from, code, err, want)
			}
		}
		result := &agentv1.AskEvent{Kind: &agentv1.AskEvent_ResultJson{ResultJson: answeredResult}}
		aRead, bDone, bToken := make(chan struct{}), make(chan struct{}), make(chan string, 1)
		a.agent.ask = func(req string, stream agentv1.Agent_AskServer) error {
			target, err := helixTarget(req)
			if err != nil {
				close(aRead)
				return status.Error(codes.Internal, err.Error())
			}
			read(target, target.Token, "a", http.StatusOK)
			close(aRead)
			select {
			case <-bDone:
			case <-time.After(5 * time.Second):
				fail("b's attempt never ended")
			}
			read(target, target.Token, "a after b", http.StatusOK)
			select {
			case token := <-bToken:
				read(target, token, "b's token after b", http.StatusUnauthorized)
			default:
				fail("b never read")
			}
			return stream.Send(result)
		}
		b.agent.ask = func(req string, stream agentv1.Agent_AskServer) error {
			select {
			case <-aRead:
			case <-time.After(5 * time.Second):
				fail("a never read")
			}
			target, err := helixTarget(req)
			if err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			bToken <- target.Token
			read(target, target.Token, "b", http.StatusOK)
			return stream.Send(result)
		}

		aSettled := make(chan []model.FoundryQuestionOutcome, 1)
		go func() { aSettled <- a.run(t, question(1)) }()
		bOutcomes := b.run(t, qb)
		close(bDone)
		aOutcomes := <-aSettled

		if len(failures) != 0 {
			t.Fatalf("reads: %v", failures)
		}
		for name, settled := range map[string][]model.FoundryQuestionOutcome{"a": aOutcomes, "b": bOutcomes} {
			if len(settled) != 1 || settled[0].Status != model.FoundryQuestionAnswered {
				t.Fatalf("%s settled %+v", name, settled)
			}
		}
		want := []string{
			`tenant-1 {"query":{"from":"a"},"request_type":"read"}`,
			`tenant-2 {"query":{"from":"b"},"request_type":"read"}`,
			`tenant-1 {"query":{"from":"a after b"},"request_type":"read"}`,
		}
		if fmt.Sprint(a.gateway.bodies) != fmt.Sprint(want) || len(b.gateway.bodies) != 0 {
			t.Fatalf("the gateway saw %v, b's own proxy %v", a.gateway.bodies, b.gateway.bodies)
		}
		reqA, _ := decodeAsk(t, a.agent.calls()[0])
		reqB, _ := decodeAsk(t, b.agent.calls()[0])
		if reqA.Database != "tenant-1" || reqA.Load != "load-1" || reqB.Database != "tenant-2" || reqB.Load != "load-2" ||
			reqA.Helix.URL != reqB.Helix.URL || reqA.Helix.Token == reqB.Helix.Token {
			t.Fatalf("a asked %s/%s at %+v, b %s/%s at %+v", reqA.Database, reqA.Load, reqA.Helix, reqB.Database, reqB.Load, reqB.Helix)
		}
	})

	// A question's token is good for reads alone: a write however it is
	// spelled, a durable write's header and another database's are refused,
	// and none reaches the gateway, while a read beside them goes through.
	t.Run("refuses writes through a question's token", func(t *testing.T) {
		h := newHarness(t)
		var mu sync.Mutex
		got := map[string]int{}
		h.agent.ask = func(req string, stream agentv1.Agent_AskServer) error {
			target, err := helixTarget(req)
			if err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			for name, tc := range map[string]struct {
				body    string
				headers map[string]string
			}{
				"a read":                {`{"request_type":"read","query":{}}`, nil},
				"a write":               {`{"request_type":"write","query":{}}`, nil},
				"a write after a read":  {`{"request_type":"read","request_type":"write","query":{}}`, nil},
				"the camel-case alias":  {`{"request_type":"read","requestType":"write","query":{}}`, nil},
				"an await-durable read": {`{"request_type":"read","query":{}}`, map[string]string{"X-Helix-Await-Durable": "true"}},
				"another database":      {`{"request_type":"read","query":{}}`, map[string]string{"X-Helix-Tenant-ID": "tenant-2"}},
			} {
				code, err := postQuery(target, target.Token, tc.body, tc.headers)
				if err != nil {
					return status.Error(codes.Internal, err.Error())
				}
				mu.Lock()
				got[name] = code
				mu.Unlock()
			}
			return stream.Send(&agentv1.AskEvent{Kind: &agentv1.AskEvent_ResultJson{ResultJson: answeredResult}})
		}
		settled := h.run(t, question(1))
		if len(settled) != 1 || settled[0].Status != model.FoundryQuestionAnswered {
			t.Fatalf("settled %+v", settled)
		}
		mu.Lock()
		defer mu.Unlock()
		want := map[string]int{
			"a read": http.StatusOK, "a write": http.StatusForbidden, "a write after a read": http.StatusForbidden,
			"the camel-case alias": http.StatusForbidden, "an await-durable read": http.StatusBadRequest, "another database": http.StatusBadRequest,
		}
		if !maps.Equal(got, want) {
			t.Fatalf("statuses = %v, want %v", got, want)
		}
		if len(h.gateway.bodies) != 1 || h.gateway.bodies[0] != `tenant-1 {"query":{},"request_type":"read"}` {
			t.Fatalf("the gateway saw %v", h.gateway.bodies)
		}
	})
}

func TestCancel(t *testing.T) {
	// A cancel takes the lease: the next event flush finds it gone, the call
	// is cancelled, nothing more is stored or settled, and the boost is
	// released.
	t.Run("stops the run at the next flush and settles nothing", func(t *testing.T) {
		h := newHarness(t)
		h.store.cancelAfter = 1
		stopped := make(chan struct{})
		h.agent.ask = func(req string, stream agentv1.Agent_AskServer) error {
			defer close(stopped)
			if err := stream.Send(&agentv1.AskEvent{Kind: &agentv1.AskEvent_EventJson{EventJson: askEvents[0]}}); err != nil {
				return err
			}
			for {
				select {
				case <-stream.Context().Done():
					return stream.Context().Err()
				case <-time.After(10 * time.Millisecond):
					if err := stream.Send(&agentv1.AskEvent{Kind: &agentv1.AskEvent_EventJson{EventJson: askEvents[1]}}); err != nil {
						return err
					}
				}
			}
		}
		start := time.Now()
		if settled := h.run(t, question(1)); len(settled) != 0 {
			t.Fatalf("a cancelled question was settled: %+v", settled)
		}
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("the agent's call was not cancelled")
		}
		if time.Since(start) > 5*time.Second || len(h.store.released) != 1 {
			t.Fatalf("took %s, released %+v", time.Since(start), h.store.released)
		}
	})

	// A renewal that finds the lease gone stops the attempt the same way.
	t.Run("stops on a lost renewal", func(t *testing.T) {
		h := newHarness(t)
		h.task.cfg.Lease = 33 * time.Second // renews every second
		h.store.cancelled = true
		h.agent.ask = func(_ string, stream agentv1.Agent_AskServer) error {
			<-stream.Context().Done()
			return stream.Context().Err()
		}
		if settled := h.run(t, question(1)); len(settled) != 0 || h.store.renews == 0 {
			t.Fatalf("settled %+v after %d renewals", settled, h.store.renews)
		}
	})

	// The boost locks the question under its lease first: one that finds the
	// lease gone (a cancel came before the attempt got going) stops before
	// the agent is asked or the database read, and settles nothing.
	t.Run("stops before asking when its boost finds the lease gone", func(t *testing.T) {
		h := newHarness(t)
		h.store.acquireErr = foundrystore.ErrNotFound
		if settled := h.run(t, question(1)); len(settled) != 0 {
			t.Fatalf("a cancelled question was settled: %+v", settled)
		}
		if len(h.agent.calls()) != 0 || len(h.gateway.bodies) != 0 || len(h.store.released) != 0 {
			t.Fatalf("asked %d times, read %v, released %+v", len(h.agent.calls()), h.gateway.bodies, h.store.released)
		}
	})
}

func TestRetries(t *testing.T) {
	failing := func(code codes.Code, events int) func(string, agentv1.Agent_AskServer) error {
		return func(_ string, stream agentv1.Agent_AskServer) error {
			for _, e := range askEvents[:events] {
				if err := stream.Send(&agentv1.AskEvent{Kind: &agentv1.AskEvent_EventJson{EventJson: e}}); err != nil {
					return err
				}
			}
			return status.Error(code, "no")
		}
	}

	// What an attempt that started spent is unknown without its result: it
	// is charged its cap, and its events stay.
	t.Run("retries an agent that stopped, charging a started attempt its cap", func(t *testing.T) {
		h := newHarness(t)
		h.agent.ask = failing(codes.Unavailable, 2)
		settled := h.run(t, question(1))
		if len(settled) != 1 || settled[0].RetryAt == nil || settled[0].Uncounted || settled[0].Spend.USDMicros != 500_000 {
			t.Fatalf("settled %+v", settled)
		}
		if !settled[0].RetryAt.Equal(h.task.now().Add(h.task.cfg.RetryBackoff)) || len(h.store.events) != 2 || len(h.store.released) != 1 {
			t.Fatalf("retry at %s, %d events, released %+v", settled[0].RetryAt, len(h.store.events), h.store.released)
		}
	})

	t.Run("gives up after its last attempt", func(t *testing.T) {
		h := newHarness(t)
		h.agent.ask = failing(codes.Unavailable, 0)
		settled := h.run(t, question(2))
		if len(settled) != 1 || settled[0].Status != model.FoundryQuestionFailed || !strings.Contains(settled[0].Error, "gave up after 2 attempts") ||
			settled[0].Spend.USDMicros != 0 {
			t.Fatalf("settled %+v", settled)
		}
	})

	t.Run("gives a busy agent's attempt back", func(t *testing.T) {
		h := newHarness(t)
		h.agent.ask = failing(codes.ResourceExhausted, 0)
		settled := h.run(t, question(2))
		if len(settled) != 1 || settled[0].RetryAt == nil || !settled[0].Uncounted {
			t.Fatalf("settled %+v", settled)
		}
	})

	// A failed flush keeps its events for the next one, so none is lost or reordered.
	t.Run("keeps events a flush failed to store", func(t *testing.T) {
		h := newHarness(t)
		h.store.appendErr = errors.New("connection reset")
		settled := h.run(t, question(1))
		if len(settled) != 1 || settled[0].Status != model.FoundryQuestionAnswered || len(h.store.events) != len(askEvents) {
			t.Fatalf("settled %+v with %d events", settled, len(h.store.events))
		}
		for i, e := range h.store.events {
			if string(e) != askEvents[i] {
				t.Fatalf("event %d = %s", i, e)
			}
		}
	})

	t.Run("retries an unreadable answer", func(t *testing.T) {
		h := newHarness(t)
		h.agent.ask = answer(`{"status":"answered","queries":[],"usage":{"input":1,"cache_read":0,"cache_write":0,"output":1,"usd":0}}`)
		if settled := h.run(t, question(1)); len(settled) != 1 || settled[0].RetryAt == nil {
			t.Fatalf("settled %+v", settled)
		}
	})

	// A worker that stops gives the question back, its attempt uncounted.
	t.Run("gives a stopping worker's attempt back", func(t *testing.T) {
		h := newHarness(t)
		ctx, cancel := context.WithCancel(context.Background())
		h.agent.ask = func(_ string, stream agentv1.Agent_AskServer) error {
			cancel()
			<-stream.Context().Done()
			return stream.Context().Err()
		}
		h.task.process(ctx, question(1))
		if len(h.store.settled) != 1 || h.store.settled[0].RetryAt == nil || !h.store.settled[0].Uncounted {
			t.Fatalf("settled %+v", h.store.settled)
		}
	})
}

func TestFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(h *harness)
		want  string
	}{
		"another contract":     {func(h *harness) { h.agent.info = &agentv1.GetInfoResponse{ContractVersion: "0.0.1"} }, "speaks contract 0.0.1"},
		"a deleted load":       {func(h *harness) { h.store.load = nil }, "the load was deleted"},
		"a load being deleted": {func(h *harness) { h.store.load.Status = model.FoundryLoadDeleting }, "being deleted"},
		"a deleted ontology":   {func(h *harness) { h.store.ontology = nil }, "deleted"},
		"an agent without Ask": {func(h *harness) {
			h.agent.ask = func(string, agentv1.Agent_AskServer) error { return status.Error(codes.Unimplemented, "no Ask") }
		}, "can't answer this question"},
		"an event with no kind": {func(h *harness) {
			h.agent.ask = func(_ string, stream agentv1.Agent_AskServer) error {
				return stream.Send(&agentv1.AskEvent{Kind: &agentv1.AskEvent_EventJson{EventJson: `{}`}})
			}
		}, "stopped before an answer"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			tc.setup(h)
			settled := h.run(t, question(2))
			if len(settled) != 1 || settled[0].Status != model.FoundryQuestionFailed || !strings.Contains(settled[0].Error, tc.want) {
				t.Fatalf("settled %+v", settled)
			}
			if len(h.store.acquired) != len(h.store.released) {
				t.Fatalf("acquired %d boosts, released %d", len(h.store.acquired), len(h.store.released))
			}
		})
	}

	// A focus the agent would refuse fails the question before it is asked.
	t.Run("fails a focus that isn't an entity without asking", func(t *testing.T) {
		h := newHarness(t)
		q := question(1)
		q.Focus = json.RawMessage(`{"type":"Customer","key":"c1"}`)
		settled := h.run(t, q)
		if len(settled) != 1 || settled[0].Status != model.FoundryQuestionFailed || !strings.Contains(settled[0].Error, "focus is not an entity") ||
			len(h.agent.calls()) != 0 {
			t.Fatalf("settled %+v after %d calls", settled, len(h.agent.calls()))
		}
	})

	// A question that keeps killing its worker stops without another call.
	t.Run("fails a question past its attempts without asking", func(t *testing.T) {
		h := newHarness(t)
		settled := h.run(t, question(3))
		if len(settled) != 1 || settled[0].Status != model.FoundryQuestionFailed || len(h.agent.calls()) != 0 {
			t.Fatalf("settled %+v after %d calls", settled, len(h.agent.calls()))
		}
	})
}

func TestDrain(t *testing.T) {
	// The worker runs up to foundry.ask.concurrency questions at once, and
	// claims no more while its slots are full.
	h := newHarness(t)
	h.task.slots = make(chan struct{}, 2)
	release := make(chan struct{})
	var mu sync.Mutex
	running := 0
	h.agent.ask = func(_ string, stream agentv1.Agent_AskServer) error {
		mu.Lock()
		running++
		mu.Unlock()
		<-release
		return stream.Send(&agentv1.AskEvent{Kind: &agentv1.AskEvent_ResultJson{ResultJson: cannotResult}})
	}
	for i := range 3 {
		q := question(1)
		q.ID = fmt.Sprintf("q-%d", i)
		h.store.claims = append(h.store.claims, q)
	}
	h.task.drain(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := running
		mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d questions running, want 2", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.store.mu.Lock()
	left := len(h.store.claims)
	h.store.mu.Unlock()
	if left != 1 {
		t.Fatalf("%d questions left unclaimed, want 1", left)
	}
	close(release)
	h.task.running.Wait()
	h.task.drain(context.Background())
	h.task.running.Wait()
	if len(h.store.settled) != 3 {
		t.Fatalf("settled %d questions, want 3", len(h.store.settled))
	}
}

// ownKey is a workspace's own Anthropic key in the tests.
const ownKey = "sk-ant-api03-own-sentinel-9876543210"

// withOwnKey makes the workspace's setting its owner's Claude, sealed under
// a test keyring, as the probe left it: reasoning on.
func withOwnKey(t *testing.T, h *harness) {
	t.Helper()
	keys, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	h.ai.Keyring = keys
	settings, err := json.Marshal(ai.Settings{Provider: ai.Anthropic{}, Models: ai.Roles{
		Draft: ai.Model{Name: "claude-opus-5-5", Price: ai.PriceTable{}}, Ask: ai.Model{Name: "claude-opus-5", Price: ai.PriceTable{}},
		Extract: ai.Model{Name: "claude-haiku-5-5", Price: ai.PriceTable{}}, Verify: ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	c := &model.FoundryAIConfig{ID: "cfg-1", WorkspaceID: "ws-1", Kind: "anthropic", Settings: settings}
	if c.Secret, err = ai.SealKey(keys, c.ID, ontologyagent.NewModelKey(ownKey)); err != nil {
		t.Fatal(err)
	}
	h.store.aiConfig = c
}

// runLogged runs an attempt of q with every log line of it captured.
func (h *harness) runLogged(t *testing.T, q *model.FoundryQuestion) ([]model.FoundryQuestionOutcome, string) {
	t.Helper()
	var logs bytes.Buffer
	h.task.process(log.Into(context.Background(), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))), q)
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	return append([]model.FoundryQuestionOutcome(nil), h.store.settled...), logs.String()
}

// stored is everything the attempt left in the question's rows: its events
// and every outcome settled.
func (h *harness) stored(t *testing.T) string {
	t.Helper()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	raw, err := json.Marshal(struct {
		Events  []json.RawMessage
		Settled []model.FoundryQuestionOutcome
	}{h.store.events, h.store.settled})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestModelKeys(t *testing.T) {
	// A workspace that brought its own key asks on its own model, the key
	// sealed in its setting going to the agent in the call's metadata alone.
	t.Run("asks on the workspace's own key", func(t *testing.T) {
		h := newHarness(t)
		withOwnKey(t, h)
		settled, logs := h.runLogged(t, question(1))
		if len(settled) != 1 || settled[0].Status != model.FoundryQuestionAnswered {
			t.Fatalf("settled %+v", settled)
		}
		_, models := decodeAsk(t, h.agent.calls()[0])
		if !strings.Contains(string(models), `"ask":{"name":"claude-opus-5","price":"table","reasoning":true}`) ||
			fmt.Sprint(h.agent.keys) != "[["+ownKey+"]]" {
			t.Fatalf("models %s, keys %v", models, h.agent.keys)
		}
		// The model asked is logged, from the agent's started event; no key is.
		if !strings.Contains(logs, `"model":"claude-sonnet-5-5"`) || !strings.Contains(logs, `"provider":"anthropic"`) {
			t.Fatalf("log = %s", logs)
		}
		for _, secret := range []string{ownKey, houseKey} {
			if strings.Contains(logs, secret) || strings.Contains(h.stored(t), secret) || strings.Contains(strings.Join(h.agent.calls(), ""), secret) {
				t.Fatalf("a key reached a log, a row or a request")
			}
		}
	})

	// A failed attempt words the provider's failure, and leaves no key in
	// the logs or the question's rows: the house's raised with on-call as
	// Helix's own trouble, the owner's sent to the settings.
	t.Run("words a model's failure and leaks no key", func(t *testing.T) {
		for name, tc := range map[string]struct {
			own  bool
			want string
			ops  bool
		}{
			"Helix's key refused":     {false, "the model could not be reached; ask again", true},
			"the owner's key refused": {true, "Claude refused the API key. An owner can enter a new one in Settings › AI models.", false},
		} {
			h := newHarness(t)
			if tc.own {
				withOwnKey(t, h)
			}
			h.agent.ask = answer(failedResult(ontologyagent.AskModelAuth))
			settled, logs := h.runLogged(t, question(1))
			if len(settled) != 1 || settled[0].Status != model.FoundryQuestionFailed || settled[0].Error != tc.want {
				t.Fatalf("%s: settled %+v", name, settled)
			}
			if raised := strings.Contains(logs, `"level":"ERROR"`); raised != tc.ops {
				t.Fatalf("%s: raised %v, log %s", name, raised, logs)
			}
			for _, secret := range []string{ownKey, houseKey} {
				if strings.Contains(logs, secret) || strings.Contains(h.stored(t), secret) {
					t.Fatalf("%s: a key reached a log or a row: %s", name, logs)
				}
			}
		}
	})

	// A setting that can't run fails before the agent is asked: a key that
	// doesn't open is the owner's to enter again; a kind the agent doesn't
	// call yet, Helix's to roll out, so the question waits for it.
	t.Run("stops at a setting that can't run", func(t *testing.T) {
		h := newHarness(t)
		withOwnKey(t, h)
		h.store.aiConfig.Secret.Ciphertext[0] ^= 0xff
		settled, logs := h.runLogged(t, question(1))
		if len(settled) != 1 || settled[0].Status != model.FoundryQuestionFailed || !strings.HasPrefix(settled[0].Error, "The stored API key can't be opened") ||
			len(h.agent.calls()) != 0 || strings.Contains(logs, ownKey) {
			t.Fatalf("settled %+v, %d calls", settled, len(h.agent.calls()))
		}
		h = newHarness(t)
		h.agent.info.Providers = []string{"openai"}
		settled = h.run(t, question(1))
		if len(settled) != 1 || settled[0].RetryAt == nil || !strings.Contains(settled[0].Error, "can't call this AI provider") || len(h.agent.calls()) != 0 {
			t.Fatalf("settled %+v, %d calls", settled, len(h.agent.calls()))
		}
	})
}
