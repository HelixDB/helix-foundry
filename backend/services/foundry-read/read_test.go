package foundryread

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/graphproxy"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
	foundryreadv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundryread/v1"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

var testNow = time.Date(2026, 10, 19, 23, 30, 0, 0, time.FixedZone("UTC-5", -5*3600))

// fakeAgent serves ontology_agent.v1.Agent's GetInfo and Read in process.
// Each Read sends a read and a write through the request's Helix target,
// then waits for hold (when set) and answers result, or err.
type fakeAgent struct {
	agentv1.UnimplementedAgentServer
	contract string
	hold     chan struct{}
	entered  chan struct{}
	// result answers the kinds of the request's reads, in order.
	result func(kinds []string) string
	err    error

	// title answers each Title; titles and titleKeys are what each got.
	title     func(req string) (string, error)
	titles    []string
	titleKeys [][]string
	providers []string

	mu       sync.Mutex
	requests []ontologyagent.ReadRequest
	// bodies are the requests as sent.
	bodies []string
	// statuses are what the proxy answered the read and the write of each call.
	statuses [][2]int
}

func (a *fakeAgent) GetInfo(context.Context, *agentv1.GetInfoRequest) (*agentv1.GetInfoResponse, error) {
	return &agentv1.GetInfoResponse{ContractVersion: a.contract, Providers: a.providers}, nil
}

func (a *fakeAgent) Title(ctx context.Context, req *agentv1.TitleRequest) (*agentv1.TitleResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	a.mu.Lock()
	a.titles = append(a.titles, req.GetRequestJson())
	a.titleKeys = append(a.titleKeys, md.Get(ontologyagent.ModelKeyHeader))
	a.mu.Unlock()
	result, err := a.title(req.GetRequestJson())
	if err != nil {
		return nil, err
	}
	return &agentv1.TitleResponse{ResultJson: result}, nil
}

func (a *fakeAgent) Read(ctx context.Context, req *agentv1.ReadRequest) (*agentv1.ReadResponse, error) {
	var got struct {
		ontologyagent.ReadRequest
		Reads []json.RawMessage `json:"reads"`
	}
	if err := json.Unmarshal([]byte(req.GetRequestJson()), &got); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	read := query(ctx, got.Helix, `{"request_type":"read","query":{}}`)
	write := query(ctx, got.Helix, `{"request_type":"write","query":{}}`)
	a.mu.Lock()
	a.requests = append(a.requests, got.ReadRequest)
	a.bodies = append(a.bodies, req.GetRequestJson())
	a.statuses = append(a.statuses, [2]int{read, write})
	a.mu.Unlock()
	if a.entered != nil {
		a.entered <- struct{}{}
	}
	if a.hold != nil {
		select {
		case <-a.hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if a.err != nil {
		return nil, a.err
	}
	kinds := make([]string, 0, len(got.Reads))
	for _, r := range got.Reads {
		var read map[string]json.RawMessage
		_ = json.Unmarshal(r, &read)
		for kind := range read {
			kinds = append(kinds, kind)
		}
	}
	return &agentv1.ReadResponse{ResultJson: a.result(kinds)}, nil
}

// query posts body to the Helix target as the agent does, returning the status.
func query(ctx context.Context, target ontologyagent.HelixTarget, body string) int {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, target.URL+graphproxy.QueryPath, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+target.Token)
	req.Header.Set("X-Helix-Require-Writer", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func (a *fakeAgent) calls() ([]ontologyagent.ReadRequest, [][2]int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]ontologyagent.ReadRequest(nil), a.requests...), append([][2]int(nil), a.statuses...)
}

// okResult answers each read with an empty answer of its kind.
func okResult(kinds []string) string {
	results := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		results = append(results, `{"ok":{"kind":"`+kind+`"}}`)
	}
	return `{"as_of":"2026-10-20","results":[` + strings.Join(results, ",") + `],"stats":{"instances":0,"wall_ms":1}}`
}

// gateway records the queries the proxy forwards and answers each with 200.
type gateway struct {
	mu      sync.Mutex
	tenants []string
	bodies  []string
}

func (g *gateway) Query(_ context.Context, _, tenantID string, body []byte, _ http.Header) (helixgateway.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tenants, g.bodies = append(g.tenants, tenantID), append(g.bodies, string(body))
	return helixgateway.Response{StatusCode: 200, ContentType: "application/json", Body: []byte(`{}`)}, nil
}

func (g *gateway) sent() ([]string, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.tenants...), append([]string(nil), g.bodies...)
}

// store holds two workspaces' ontologies, each bound to its own database,
// and their AI settings: none is Helix's Claude.
// onPro stands in for Autumn: every workspace is on Foundry Pro, so its own
// provider runs.
type onPro struct{}

func (onPro) Status(context.Context, string) (foundryplan.Status, error) {
	return foundryplan.Status{Plan: &foundryplan.Plan{Code: "pro", AutumnPlanID: "foundry-pro", Rank: 1}}, nil
}

type store struct {
	ontologies map[string]*model.FoundryOntology
	tenants    map[string]*ports.Database
	// paused are the databases whose queries are paused.
	paused map[string]bool
	ai     map[string]*model.FoundryAIConfig
}

func (s *store) GetActiveFoundryAIConfig(_ context.Context, workspaceID string) (*model.FoundryAIConfig, error) {
	if c, ok := s.ai[workspaceID]; ok {
		return c, nil
	}
	return nil, foundrystore.ErrNotFound
}

func (s *store) GetFoundryOntology(_ context.Context, workspaceID, id string) (*model.FoundryOntology, error) {
	o, ok := s.ontologies[id]
	if !ok || o.WorkspaceID != workspaceID {
		return nil, foundrystore.ErrNotFound
	}
	return o, nil
}

func (s *store) Get(_ context.Context, id string) (ports.Database, error) {
	if t, ok := s.tenants[id]; ok {
		return *t, nil
	}
	return ports.Database{}, foundrystore.ErrNotFound
}

func (s *store) Live(ctx context.Context, id string) (ports.Database, error) { return s.Get(ctx, id) }

func (s *store) Endpoint(_ context.Context, id, path string) (string, error) {
	if s.paused[id] {
		return "", status.Error(codes.FailedPrecondition, "the database's queries are paused")
	}
	return "https://abc.cluster.example.com" + path, nil
}

type harness struct {
	reader  *reader
	agent   *fakeAgent
	gateway *gateway
	store   *store
	ai      *ai.Resolver
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	agent := &fakeAgent{contract: ontologyagent.ContractVersion, result: okResult, providers: []string{"anthropic"},
		title: func(string) (string, error) { return titled("Customer churn"), nil }}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	agentv1.RegisterAgentServer(srv, agent)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	client, err := ontologyagent.Dial(lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	g := &gateway{}
	proxy, err := graphproxy.ServeReadOnly(g, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })

	defaults := config.Defaults()
	cfg := &defaults
	cfg.Foundry.AI.Default = config.AIDefault{Provider: "anthropic", APIKey: houseKey, Models: config.AIModels{
		Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5",
	}}
	s := &store{
		ontologies: map[string]*model.FoundryOntology{
			"ont-1": {ID: "ont-1", WorkspaceID: "ws-1", TenantID: "tenant-1"},
			"ont-2": {ID: "ont-2", WorkspaceID: "ws-2", TenantID: "tenant-2"},
		},
		tenants: map[string]*ports.Database{
			"tenant-1": {TenantID: "tenant-1", WorkspaceID: "ws-1"},
			"tenant-2": {TenantID: "tenant-2", WorkspaceID: "ws-2"},
		},
		paused: map[string]bool{},
	}
	resolver, err := ai.NewResolver(cfg, s, onPro{})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{
		reader: &reader{db: s, dbs: s, cfg: cfg, agent: client, ai: resolver, proxy: proxy, limit: newWorkspaceLimit(MaxReadsPerWorkspace),
			titles: newWorkspaceLimit(MaxTitlesPerWorkspace), now: func() time.Time { return testNow }},
		agent: agent, gateway: g, store: s, ai: resolver,
	}
}

func request(workspace, ontology string) *foundryreadv1.ReadRequest {
	return &foundryreadv1.ReadRequest{
		WorkspaceId: workspace, OntologyId: ontology, LoadId: "load-1", OntologyVersion: "foundry-v3", OntologyToml: "version = \"x\"\n",
		ReadsJson: `[{"entity":{"type":"Customer","id":"c-1"}},{"explain":{"pred":"derived:blocked","args":[{"type":"Customer","id":"c-1"}]}}]`,
	}
}

func TestRead(t *testing.T) {
	ctx := context.Background()

	// The sidecar reads the load through the proxy, which forwards its reads
	// to the ontology's database and refuses its writes, for the call only.
	t.Run("reads a load through the read-only proxy", func(t *testing.T) {
		h := newHarness(t)
		resp, err := h.reader.Read(ctx, request("ws-1", "ont-1"))
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetResultJson() != okResult([]string{"entity", "explain"}) {
			t.Fatalf("result = %s", resp.GetResultJson())
		}
		requests, statuses := h.agent.calls()
		if len(requests) != 1 {
			t.Fatalf("agent read %d times", len(requests))
		}
		if err := contracttest.Validate(t, contracttest.Schema(t, "read_request.json"), []byte(h.agent.bodies[0])); err != nil {
			t.Fatalf("the request does not match read_request.json: %v", err)
		}
		got := requests[0]
		if got.RequestID == "" || got.Database != "tenant-1" || got.Load != "load-1" || got.OntologyVersion != "foundry-v3" ||
			got.OntologyTOML != "version = \"x\"\n" || got.AsOf != "2026-10-20" || got.Budget != ontologyagent.DefaultReadBudget {
			t.Fatalf("request = %+v (as_of is the UTC date)", got)
		}
		if got.Helix.URL != h.reader.proxy.URL() || got.Helix.Token == "" || got.Helix.TimeoutSecs != helixTimeoutSecs ||
			got.Helix.MaxRequestBytes != graphproxy.MaxRequestBytes {
			t.Fatalf("helix target = %+v", got.Helix)
		}
		if statuses[0] != [2]int{http.StatusOK, http.StatusForbidden} {
			t.Fatalf("proxy answered read %d, write %d", statuses[0][0], statuses[0][1])
		}
		tenants, bodies := h.gateway.sent()
		if len(tenants) != 1 || tenants[0] != "tenant-1" || !strings.Contains(bodies[0], `"request_type":"read"`) {
			t.Fatalf("gateway saw %v %v", tenants, bodies)
		}
		// The call's token ends with it.
		if code := query(ctx, got.Helix, `{"request_type":"read","query":{}}`); code != http.StatusUnauthorized {
			t.Fatalf("token after the call: status %d", code)
		}
	})

	// A workspace runs at most MaxReadsPerWorkspace reads at once; another
	// workspace's reads don't wait for it, and a slot frees when a read ends.
	t.Run("limits each workspace's concurrent reads", func(t *testing.T) {
		h := newHarness(t)
		h.agent.hold, h.agent.entered = make(chan struct{}), make(chan struct{}, MaxReadsPerWorkspace+2)
		errs := make(chan error, MaxReadsPerWorkspace+1)
		for range MaxReadsPerWorkspace {
			go func() {
				_, err := h.reader.Read(ctx, request("ws-1", "ont-1"))
				errs <- err
			}()
		}
		for range MaxReadsPerWorkspace {
			<-h.agent.entered
		}
		if _, err := h.reader.Read(ctx, request("ws-1", "ont-1")); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("a fifth read: %v", err)
		}
		go func() {
			_, err := h.reader.Read(ctx, request("ws-2", "ont-2"))
			errs <- err
		}()
		<-h.agent.entered // ws-2's read runs beside ws-1's four
		close(h.agent.hold)
		for range MaxReadsPerWorkspace + 1 {
			if err := <-errs; err != nil {
				t.Fatalf("a held read: %v", err)
			}
		}
		if _, err := h.reader.Read(ctx, request("ws-1", "ont-1")); err != nil {
			t.Fatalf("a read once the slots freed: %v", err)
		}
		h.reader.limit.mu.Lock()
		defer h.reader.limit.mu.Unlock()
		if len(h.reader.limit.running) != 0 {
			t.Fatalf("slots still taken: %v", h.reader.limit.running)
		}
	})

	// What can't be read never reaches the sidecar.
	t.Run("refuses before calling the agent", func(t *testing.T) {
		now := time.Now()
		for name, tc := range map[string]struct {
			req    *foundryreadv1.ReadRequest
			mutate func(*harness)
			code   codes.Code
		}{
			"no load":                 {&foundryreadv1.ReadRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}, nil, codes.InvalidArgument},
			"bad reads":               {func() *foundryreadv1.ReadRequest { r := request("ws-1", "ont-1"); r.ReadsJson = `[]`; return r }(), nil, codes.InvalidArgument},
			"another workspace's":     {request("ws-1", "ont-2"), nil, codes.NotFound},
			"a deleted ontology":      {request("ws-1", "ont-1"), func(h *harness) { h.store.ontologies["ont-1"].DeletedAt = &now }, codes.NotFound},
			"a paused database":       {request("ws-2", "ont-2"), func(h *harness) { h.store.paused["tenant-2"] = true }, codes.FailedPrecondition},
			"an agent on another one": {request("ws-1", "ont-1"), func(h *harness) { h.agent.contract = "0.0.1" }, codes.FailedPrecondition},
		} {
			h := newHarness(t)
			if tc.mutate != nil {
				tc.mutate(h)
			}
			if _, err := h.reader.Read(ctx, tc.req); status.Code(err) != tc.code {
				t.Fatalf("%s: %v, want %s", name, err, tc.code)
			}
			if requests, _ := h.agent.calls(); len(requests) != 0 {
				t.Fatalf("%s: agent read %d times", name, len(requests))
			}
			if len(h.reader.limit.running) != 0 {
				t.Fatalf("%s: slots still taken: %v", name, h.reader.limit.running)
			}
		}
	})

	// The agent's own refusals reach the caller as it worded them; anything
	// else is the reader failing.
	t.Run("relays the agent's refusals", func(t *testing.T) {
		for name, tc := range map[string]struct {
			err  error
			code codes.Code
			msg  string
		}{
			"an unknown load":   {status.Error(codes.FailedPrecondition, "load_unknown"), codes.FailedPrecondition, "load_unknown"},
			"bad reads":         {status.Error(codes.InvalidArgument, "reads[0]: unknown pred"), codes.InvalidArgument, "reads[0]: unknown pred"},
			"a full sidecar":    {status.Error(codes.ResourceExhausted, "8 reads running"), codes.ResourceExhausted, "8 reads running"},
			"a sidecar failing": {status.Error(codes.Internal, "panic"), codes.Unavailable, "the graph reader failed; try again"},
			"a slow read":       {status.Error(codes.DeadlineExceeded, "late"), codes.DeadlineExceeded, "the reads took too long; ask for fewer at once"},
		} {
			h := newHarness(t)
			h.agent.err = tc.err
			_, err := h.reader.Read(ctx, request("ws-1", "ont-1"))
			if status.Code(err) != tc.code || status.Convert(err).Message() != tc.msg {
				t.Fatalf("%s: %v", name, err)
			}
		}
	})

	// A result is relayed only with an answer of each read's kind, in order,
	// and small enough for every hop back to the website.
	t.Run("refuses a result it can't relay", func(t *testing.T) {
		h := newHarness(t)
		h.agent.result = func([]string) string { return okResult([]string{"entity"}) }
		if _, err := h.reader.Read(ctx, request("ws-1", "ont-1")); status.Code(err) != codes.Internal {
			t.Fatalf("one result for two reads: %v", err)
		}
		h.agent.result = func([]string) string { return okResult([]string{"entity", "pattern"}) }
		if _, err := h.reader.Read(ctx, request("ws-1", "ont-1")); status.Code(err) != codes.Internal {
			t.Fatalf("a pattern's answer to an explain: %v", err)
		}
		h.agent.result = func([]string) string {
			return `{"as_of":"d","results":[{"ok":{"pad":"` + strings.Repeat("a", MaxResultBytes) + `"}},{"ok":{}}]}`
		}
		if _, err := h.reader.Read(ctx, request("ws-1", "ont-1")); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("a result past %d bytes: %v", MaxResultBytes, err)
		}
	})
}

func TestWorkspaceLimit(t *testing.T) {
	l := newWorkspaceLimit(1)
	release, ok := l.acquire("ws-1")
	if !ok {
		t.Fatal("first acquire refused")
	}
	if _, ok := l.acquire("ws-1"); ok {
		t.Fatal("second acquire admitted")
	}
	release()
	release() // a second release frees nothing more
	if _, ok := l.acquire("ws-1"); !ok {
		t.Fatal("acquire after release refused")
	}
	if _, ok := l.acquire("ws-1"); ok {
		t.Fatal("a double release freed two slots")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("a limit of 0 was made")
		}
	}()
	newWorkspaceLimit(0)
}
