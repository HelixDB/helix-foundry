package ontologyagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
)

// The blocks of Helix's Claude, as the backend's default resolves to them
// and as the agent's own hosted default (agent_server::cli::HOSTED) names
// them: Anthropic, 600 s calls, each role's model priced from the table,
// titles on the extraction model without reasoning.
var (
	houseEndpoint = Endpoint{Provider: Anthropic{}, CallTimeoutSecs: 600}
	houseDraft    = ModelSpec{Name: "claude-opus-5-5", Price: PriceTable{}, Reasoning: true}
	houseAsk      = ModelSpec{Name: "claude-sonnet-5-5", Price: PriceTable{}, Reasoning: true}
	houseExtract  = ModelSpec{Name: "claude-haiku-5-5", Price: PriceTable{}, Reasoning: true}
	houseVerify   = ModelSpec{Name: "claude-sonnet-5-5", Price: PriceTable{}, Reasoning: true}
	houseTitle    = ModelSpec{Name: "claude-haiku-5-5", Price: PriceTable{}}
)

func houseTurn() TurnModels {
	return TurnModels{Endpoint: houseEndpoint, Draft: houseDraft, Extract: houseExtract, Verify: houseVerify}
}
func houseAskModels() AskModels { return AskModels{Endpoint: houseEndpoint, Ask: houseAsk} }
func houseExtractModels() ExtractModels {
	return ExtractModels{Endpoint: houseEndpoint, Extract: houseExtract, Verify: houseVerify}
}

// everyKind is an endpoint of each kind the contract declares, with a model
// priced each way the contract prices one.
func everyKind() map[ProviderKind]struct {
	endpoint Endpoint
	model    ModelSpec
} {
	cached := uint64(250)
	return map[ProviderKind]struct {
		endpoint Endpoint
		model    ModelSpec
	}{
		KindAnthropic: {houseEndpoint, ModelSpec{Name: "claude-opus-5-5", Price: PriceTable{}, Reasoning: true, MaxOutputTokens: 64000}},
		KindOpenAI: {Endpoint{Provider: OpenAI{}, CallTimeoutSecs: 600},
			ModelSpec{Name: "gpt-6.1", Price: PriceRates{Input: 1250, Output: 10000, CacheRead: &cached}, Reasoning: true}},
		KindAzureOpenAI: {Endpoint{Provider: AzureOpenAI{Endpoint: "https://acme.openai.azure.com/openai/v1"}, CallTimeoutSecs: 600},
			ModelSpec{Name: "drafts", Price: PriceTableAs{Model: "gpt-6.1"}, Reasoning: true}},
		KindBedrock: {Endpoint{Provider: Bedrock{Region: "us-east-1", FIPS: true}, CallTimeoutSecs: 600},
			ModelSpec{Name: "us.anthropic.claude-sonnet-5-5", Price: PriceTableAs{Model: "anthropic.claude-sonnet-5-5"}, Reasoning: true}},
		KindOllama: {Endpoint{Provider: Ollama{BaseURL: "http://127.0.0.1:11434"}, CallTimeoutSecs: 1800, MaxCallsInFlight: 1},
			ModelSpec{Name: "qwen3:32b", Price: PriceFree{}, ContextTokens: 32768, Revision: "sha256:" + strings.Repeat("ab", 32)}},
		KindOpenAICompatible: {Endpoint{
			Provider: OpenAICompatible{BaseURL: "https://llm.example.com/v1", API: CompatChatCompletions}, CallTimeoutSecs: 1800, MaxCallsInFlight: 4,
		}, ModelSpec{Name: "llama-4", Price: PriceFree{}}},
	}
}

func TestModels(t *testing.T) {
	// Helix's Claude is sent exactly so: every request names its models.
	t.Run("writes Helix's Claude", func(t *testing.T) {
		raw, err := json.Marshal(houseTurn())
		if err != nil {
			t.Fatal(err)
		}
		const want = `{"endpoint":{"provider":"anthropic","call_timeout_secs":600},` +
			`"draft":{"name":"claude-opus-5-5","price":"table","reasoning":true},` +
			`"extract":{"name":"claude-haiku-5-5","price":"table","reasoning":true},` +
			`"verify":{"name":"claude-sonnet-5-5","price":"table","reasoning":true}}`
		if string(raw) != want {
			t.Fatalf("turn models = %s\nwant %s", raw, want)
		}
		title, _ := json.Marshal(TitleModels{Endpoint: houseEndpoint, Title: houseTitle})
		if !strings.Contains(string(title), `"title":{"name":"claude-haiku-5-5","price":"table","reasoning":false}`) {
			t.Fatalf("title models = %s", title)
		}
	})

	// Every kind, every price form and every optional field is a block the
	// agent accepts, in every request that names models.
	t.Run("writes every kind as the schemas take it", func(t *testing.T) {
		for kind, k := range everyKind() {
			turn := TurnRequest{SessionID: "s", RequestID: "r", Sources: []Source{}, Input: Input{Start: Start{Workflow: "w", Goals: []string{}}},
				Limits: Limits{MaxUSD: 1, MaxSteps: 1, MaxWallSecs: 1}, Models: TurnModels{Endpoint: k.endpoint, Draft: k.model, Extract: k.model, Verify: k.model}}
			ask := askRequest()
			ask.Models = AskModels{Endpoint: k.endpoint, Ask: k.model}
			extract := testExtractRequest()
			extract.Models = ExtractModels{Endpoint: k.endpoint, Extract: k.model, Verify: k.model}
			title := TitleRequest{RequestID: "gen-1", Goal: "track churn", Models: TitleModels{Endpoint: k.endpoint, Title: k.model}}
			probe := ProbeRequest{RequestID: "cfg-1", Endpoint: k.endpoint, Limits: ProbeLimits{MaxUSD: 0.5, MaxWallSecs: 120}}
			for _, role := range ProbeRoles {
				probe.Checks = append(probe.Checks, ProbeCheck{Role: role, Model: k.model})
			}
			for schema, request := range map[string]any{
				"turn_request.json": turn, "ask_request.json": ask, "extract_request.json": extract,
				"title_request.json": title, "probe_request.json": probe,
			} {
				raw, err := json.Marshal(request)
				if err != nil {
					t.Fatalf("%s %s: %v", kind, schema, err)
				}
				if err := contracttest.Validate(t, contracttest.Schema(t, schema), raw); err != nil {
					t.Errorf("%s: %s refused %s: %v", kind, schema, raw, err)
				}
			}
		}
		if len(everyKind()) != len(ProviderKinds) {
			t.Fatalf("%d kinds tested of %d", len(everyKind()), len(ProviderKinds))
		}
	})

	// The check has teeth: a request without its models, or with a model
	// the contract can't price, is refused.
	t.Run("the schemas refuse missing or unknown models", func(t *testing.T) {
		raw, _ := json.Marshal(TitleRequest{RequestID: "gen-1", Goal: "g", Models: TitleModels{Endpoint: houseEndpoint, Title: houseTitle}})
		schema := contracttest.Schema(t, "title_request.json")
		for name, bad := range map[string]string{
			"no models":      strings.Replace(string(raw), `,"models":`, `,"other":`, 1),
			"a key in JSON":  strings.Replace(string(raw), `"call_timeout_secs"`, `"api_key":"sk","call_timeout_secs"`, 1),
			"an odd price":   strings.Replace(string(raw), `"price":"table"`, `"price":"cheap"`, 1),
			"an odd kind":    strings.Replace(string(raw), `"provider":"anthropic"`, `"provider":"gemini"`, 1),
			"no timeout":     strings.Replace(string(raw), `,"call_timeout_secs":600`, ``, 1),
			"a zero timeout": strings.Replace(string(raw), `"call_timeout_secs":600`, `"call_timeout_secs":0`, 1),
		} {
			if bad == string(raw) {
				t.Fatalf("%s: the replacement missed", name)
			}
			if err := contracttest.Validate(t, schema, []byte(bad)); err == nil {
				t.Errorf("%s: title_request.json took %s", name, bad)
			}
		}
	})

	// A block with a piece missing is never sent half-written.
	t.Run("refuses to write a block with no provider or price", func(t *testing.T) {
		if raw, err := json.Marshal(Endpoint{CallTimeoutSecs: 1}); err == nil {
			t.Fatalf("an endpoint with no provider = %s", raw)
		}
		if raw, err := json.Marshal(ModelSpec{Name: "m"}); err == nil {
			t.Fatalf("a model with no price = %s", raw)
		}
	})

	// Every failure the contract names, and no other, is read.
	t.Run("reads the contract's model failures", func(t *testing.T) {
		raw, err := json.Marshal(ModelFailures)
		if err != nil {
			t.Fatal(err)
		}
		var back []ModelFailure
		if err := json.Unmarshal(raw, &back); err != nil || len(back) != 14 {
			t.Fatalf("round trip = %v, %v", back, err)
		}
		var f ModelFailure
		if err := json.Unmarshal([]byte(`"api"`), &f); err == nil {
			t.Fatal("3.x's api failure was read")
		}
	})
}

func TestTitleAndProbeResults(t *testing.T) {
	titles, probes := contracttest.Schema(t, "title_result.json"), contracttest.Schema(t, "probe_result.json")
	usage := `"usage":{"input":90,"cache_read":0,"cache_write":0,"output":4,"usd":0.0001}`

	t.Run("reads a title's text or failure", func(t *testing.T) {
		for raw, want := range map[string]TitleResult{
			`{"outcome":{"text":{"text":"Churn"}},` + usage + `}`:     {Text: "Churn"},
			`{"outcome":{"failed":{"failure":"auth"}},` + usage + `}`: {Failure: ModelAuth},
		} {
			if err := contracttest.Validate(t, titles, []byte(raw)); err != nil {
				t.Fatalf("%s: %v", raw, err)
			}
			got, err := DecodeTitleResult([]byte(raw))
			want.Usage = Usage{Input: 90, Output: 4, USD: 0.0001}
			if err != nil || got != want {
				t.Fatalf("DecodeTitleResult(%s) = %+v, %v", raw, got, err)
			}
		}
		for _, raw := range []string{
			`{"outcome":{"text":{"text":"x"}}}`, `{"outcome":{},` + usage + `}`, `{"outcome":{"failed":{"failure":"tired"}},` + usage + `}`,
			`{"outcome":{"failed":{}},` + usage + `}`, `{"outcome":{"text":{}},` + usage + `}`, `not json`,
		} {
			if got, err := DecodeTitleResult([]byte(raw)); err == nil {
				t.Errorf("DecodeTitleResult(%s) = %+v", raw, got)
			}
		}
	})

	t.Run("reads each check of a probe", func(t *testing.T) {
		raw := `{"checks":[` +
			`{"role":"draft","outcome":{"passed":{"seconds":2.5,"reasoning":true}}},` +
			`{"role":"extract","outcome":{"passed":{"seconds":1,"reasoning":false,"served_model":null}}},` +
			`{"role":"title","outcome":{"failed":{"failure":"unsupported","detail":"text_missing"}}}],` +
			`"ollama":{"digest":"sha256:ab","context_length":32768,"capabilities":["tools"]},` + usage + `}`
		if err := contracttest.Validate(t, probes, []byte(raw)); err != nil {
			t.Fatal(err)
		}
		asked := []ProbeRole{ProbeDraft, ProbeExtract, ProbeTitle}
		got, err := DecodeProbeResult([]byte(raw), asked)
		if err != nil {
			t.Fatal(err)
		}
		want := []ProbeCheckResult{
			{Role: ProbeDraft, Passed: true, Seconds: 2.5, Reasoning: true},
			{Role: ProbeExtract, Passed: true, Seconds: 1},
			{Role: ProbeTitle, Failure: ModelUnsupported, Detail: ProbeTextMissing},
		}
		if fmt.Sprint(got.Checks) != fmt.Sprint(want) || got.Ollama == nil || got.Ollama.ContextLength != 32768 || got.Usage.Output != 4 {
			t.Fatalf("DecodeProbeResult = %+v", got)
		}
		if failed := got.Failed(); failed == nil || failed.Role != ProbeTitle {
			t.Fatalf("Failed() = %+v", failed)
		}
		passing, _ := DecodeProbeResult([]byte(`{"checks":[{"role":"draft","outcome":{"passed":{"seconds":1,"reasoning":true}}}],`+usage+`}`),
			asked[:1])
		if passing.Failed() != nil || passing.Ollama != nil {
			t.Fatalf("a passing probe = %+v", passing)
		}
		// Only the checks asked for, in their order, each whole.
		for name, bad := range map[string]string{
			"another role": `{"checks":[{"role":"ask","outcome":{"passed":{"seconds":1,"reasoning":true}}}],` + usage + `}`,
			"too few":      `{"checks":[],` + usage + `}`,
			"no usage":     `{"checks":[{"role":"draft","outcome":{"passed":{"seconds":1,"reasoning":true}}}]}`,
			"no seconds":   `{"checks":[{"role":"draft","outcome":{"passed":{"reasoning":true}}}],` + usage + `}`,
			"odd detail":   `{"checks":[{"role":"draft","outcome":{"failed":{"failure":"auth","detail":"sad"}}}],` + usage + `}`,
			"no failure":   `{"checks":[{"role":"draft","outcome":{"failed":{"detail":"none"}}}],` + usage + `}`,
			"both":         `{"checks":[{"role":"draft","outcome":{"passed":{"seconds":1,"reasoning":true},"failed":{"failure":"auth","detail":"none"}}}],` + usage + `}`,
		} {
			if got, err := DecodeProbeResult([]byte(bad), asked[:1]); err == nil {
				t.Errorf("%s: DecodeProbeResult = %+v", name, got)
			}
		}
	})
}

// keyAgent records what each model call's metadata carried as its key.
type keyAgent struct {
	agentv1.UnimplementedAgentServer
	mu       sync.Mutex
	keys     map[string][]string
	requests []string
}

func (a *keyAgent) record(ctx context.Context, rpc, request string) {
	md, _ := metadata.FromIncomingContext(ctx)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.keys[rpc] = md.Get(ModelKeyHeader)
	a.requests = append(a.requests, request)
}

func (a *keyAgent) Turn(req *agentv1.TurnRequest, stream agentv1.Agent_TurnServer) error {
	a.record(stream.Context(), "turn", req.GetRequestJson())
	return stream.Send(&agentv1.TurnEvent{Kind: &agentv1.TurnEvent_ResultJson{ResultJson: "{}"}})
}

func (a *keyAgent) Ask(req *agentv1.AskRequest, stream agentv1.Agent_AskServer) error {
	a.record(stream.Context(), "ask", req.GetRequestJson())
	return stream.Send(&agentv1.AskEvent{Kind: &agentv1.AskEvent_ResultJson{ResultJson: "{}"}})
}

func (a *keyAgent) Extract(req *agentv1.ExtractRequest, stream agentv1.Agent_ExtractServer) error {
	a.record(stream.Context(), "extract", req.GetRequestJson())
	return stream.Send(&agentv1.ExtractEvent{Kind: &agentv1.ExtractEvent_ResultJson{ResultJson: "{}"}})
}

func (a *keyAgent) Title(ctx context.Context, req *agentv1.TitleRequest) (*agentv1.TitleResponse, error) {
	a.record(ctx, "title", req.GetRequestJson())
	return &agentv1.TitleResponse{ResultJson: `{"title":1}`}, nil
}

func (a *keyAgent) Probe(ctx context.Context, req *agentv1.ProbeRequest) (*agentv1.ProbeResponse, error) {
	a.record(ctx, "probe", req.GetRequestJson())
	return &agentv1.ProbeResponse{ResultJson: `{"probe":1}`}, nil
}

// The key of each model call goes in its metadata, exactly as given, and
// nowhere else; no key, no header.
func TestClientModelKey(t *testing.T) {
	const secret = "sk-ant-api03-sentinel-0123456789"
	agent := &keyAgent{keys: map[string][]string{}}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	agentv1.RegisterAgentServer(srv, agent)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	client, err := Dial(lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	ctx := context.Background()
	call := func(key ModelKey) map[string]string {
		results := map[string]string{}
		ignore := func(json.RawMessage) error { return nil }
		raw, err := client.Turn(ctx, []byte(`{"turn":1}`), key, func(json.RawMessage) {})
		results["turn"] = string(raw) + fmt.Sprint(err)
		raw, err = client.Ask(ctx, []byte(`{"ask":1}`), key, ignore)
		results["ask"] = string(raw) + fmt.Sprint(err)
		raw, err = client.Extract(ctx, []byte(`{"extract":1}`), key, ignore)
		results["extract"] = string(raw) + fmt.Sprint(err)
		raw, err = client.Title(ctx, []byte(`{"title":1}`), key)
		results["title"] = string(raw) + fmt.Sprint(err)
		raw, err = client.Probe(ctx, []byte(`{"probe":1}`), key)
		results["probe"] = string(raw) + fmt.Sprint(err)
		return results
	}
	results := call(NewModelKey(secret))
	for _, rpc := range []string{"turn", "ask", "extract", "title", "probe"} {
		if got := agent.keys[rpc]; len(got) != 1 || got[0] != secret {
			t.Errorf("%s carried key %q", rpc, got)
		}
		if !strings.HasSuffix(results[rpc], "<nil>") {
			t.Errorf("%s = %s", rpc, results[rpc])
		}
	}
	if results["title"] != `{"title":1}<nil>` || results["probe"] != `{"probe":1}<nil>` {
		t.Errorf("unary results = %v", results)
	}
	if strings.Contains(strings.Join(agent.requests, ""), secret) {
		t.Fatal("a request body carried the key")
	}
	call(ModelKey{})
	for rpc, got := range agent.keys {
		if len(got) != 0 {
			t.Errorf("%s without a key carried %q", rpc, got)
		}
	}
}
