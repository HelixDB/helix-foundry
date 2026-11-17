package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
)

// local dials anything, as a local install's rules let it: the fake
// providers listen on 127.0.0.1.
var local = guarded(ai.Rules{AllowPrivate: true})

// withoutDeltas is chunks without their TextDeltas: the answer.
func withoutDeltas(chunks []Chunk) []Chunk {
	var out []Chunk
	for _, c := range chunks {
		if _, ok := c.(TextDelta); !ok {
			out = append(out, c)
		}
	}
	return out
}

// sse writes a Messages stream: thinking, text and a tool call, ending for the tool.
func sse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	events := []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":100,"output_tokens":1,"cache_read_input_tokens":2000,"cache_creation_input_tokens":300}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Look it up."}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Let me "}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"check."}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"helix_foundry_list_ontologies","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"workspace_id\":"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"ws1\"}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":40}}`,
		`{"type":"message_stop"}`,
	}
	for _, e := range events {
		var head struct{ Type string }
		_ = json.Unmarshal([]byte(e), &head)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, e)
	}
}

func collect(t *testing.T, m Model, r Request) ([]Chunk, error) {
	t.Helper()
	var out []Chunk
	for chunk, err := range m.Stream(context.Background(), r) {
		if err != nil {
			return out, err
		}
		out = append(out, chunk)
	}
	return out, nil
}

func TestAnthropicStreamsBlocksUsageAndStop(t *testing.T) {
	var body map[string]any
	var key string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(raw, &body))
		sse(w)
	}))
	defer server.Close()
	m := newAnthropic("sk-ws", "claude-sonnet-5-5", 0, local, option.WithBaseURL(server.URL), option.WithMaxRetries(0))
	chunks, err := collect(t, m, Request{
		System: "You are the Assistant.", MaxOutput: 1000,
		Tools: []Tool{{Name: "helix_foundry_list_ontologies", Description: "List them.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"workspace_id":{"type":"string"}},"required":["workspace_id"],"additionalProperties":false,"$schema":"x"}`)}},
		Messages: []Message{
			{Role: User, Content: []Block{Text{Text: "What ontologies?"}}},
			{Role: Assistant, Content: []Block{Reasoning{Writer: m.writer, Signature: "s0", Text: "t0"}, Reasoning{Writer: writerOf(ai.KindAnthropic, "", "claude-opus-5-5"), Text: "dropped"},
				ToolUse{ID: "toolu_0", Name: "helix_foundry_get_context", Input: json.RawMessage(`{"x":1}`)}}},
			{Role: User, Content: []Block{ToolResult{ToolUseID: "toolu_0", Content: "nothing", IsError: true}}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer sk-ws", key)
	assert.Equal(t, []Chunk{
		TextDelta{Text: "Let me "}, TextDelta{Text: "check."},
		Reasoning{Writer: m.writer, Signature: "sig-1", Text: "Look it up."},
		Text{Text: "Let me check."},
		ToolUse{ID: "toolu_1", Name: "helix_foundry_list_ontologies", Input: json.RawMessage(`{"workspace_id":"ws1"}`)},
		Usage{In: 100, Out: 40, CacheRead: 2000, CacheWrite: 300},
		Stop{Reason: StopToolUse},
	}, chunks)

	// The prefix is cached, the tool's schema sent whole but for $schema, and
	// another model's reasoning dropped.
	system := body["system"].([]any)[0].(map[string]any)
	assert.Equal(t, "ephemeral", system["cache_control"].(map[string]any)["type"])
	assert.NotNil(t, body["cache_control"])
	schema := body["tools"].([]any)[0].(map[string]any)["input_schema"].(map[string]any)
	assert.Equal(t, false, schema["additionalProperties"])
	assert.Equal(t, []any{"workspace_id"}, schema["required"])
	assert.NotContains(t, schema, "$schema")
	assistant := body["messages"].([]any)[1].(map[string]any)["content"].([]any)
	require.Len(t, assistant, 2)
	assert.Equal(t, "thinking", assistant[0].(map[string]any)["type"])
	result := body["messages"].([]any)[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	assert.Equal(t, true, result["is_error"])
}

func TestAnthropicErrorsSayWhetherToRetry(t *testing.T) {
	for status, kind := range map[int]ErrorKind{401: ErrAuth, 403: ErrAuth, 400: ErrInvalid, 404: ErrInvalid, 429: ErrRateLimited, 529: ErrUnavailable} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"x","message":"no"}}`))
		}))
		m := newAnthropic("sk-SECRET-KEY", "claude-sonnet-5-5", 0, local, option.WithBaseURL(server.URL), option.WithMaxRetries(0))
		_, err := collect(t, m, Request{System: "s", MaxOutput: 10, Messages: []Message{{Role: User, Content: []Block{Text{Text: "hi"}}}}})
		var e *Error
		require.True(t, errors.As(err, &e), "%d: %v", status, err)
		assert.Equal(t, kind, e.Kind, "%d", status)
		assert.Equal(t, kind == ErrRateLimited || kind == ErrUnavailable, e.Retryable())
		assert.NotContains(t, err.Error(), "SECRET", "the key never reaches an error")
		server.Close()
	}
}

// writeSSE writes events, each a JSON object whose type names it.
func writeSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		var head struct{ Type string }
		_ = json.Unmarshal([]byte(e), &head)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, e)
	}
}

const started = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[],"stop_reason":null,"usage":{"input_tokens":90000,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":5000}}}`

// A stream that breaks after it started reports what it used, which the
// provider bills, then its error, which a retry may pass.
func TestAnthropicBrokenStreamReportsItsUsage(t *testing.T) {
	for name, tail := range map[string][]string{
		"error event": {`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
		"cut off": {`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Half"}}`},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeSSE(w, append([]string{started}, tail...)...)
			}))
			defer server.Close()
			m := newAnthropic("sk", "claude-sonnet-5-5", 0, local, option.WithBaseURL(server.URL), option.WithMaxRetries(0))
			chunks, err := collect(t, m, Request{System: "s", MaxOutput: 10, Messages: []Message{{Role: User, Content: []Block{Text{Text: "hi"}}}}})
			assert.Equal(t, []Chunk{Usage{In: 90000, Out: 1, CacheWrite: 5000}}, withoutDeltas(chunks), "nothing but what it used")
			var e *Error
			require.ErrorAs(t, err, &e)
			assert.True(t, e.Retryable(), "%v", err)
		})
	}
}

// max_tokens can stop a reply in the middle of a tool call's input: the
// call comes back as JSON, never as bytes that don't parse.
func TestAnthropicToolCallCutAtMaxTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSE(w, started,
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"helix_foundry_prepare_operation","input":{}}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"operation\":\"create_lo"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":null},"usage":{"output_tokens":10}}`,
			`{"type":"message_stop"}`)
	}))
	defer server.Close()
	m := newAnthropic("sk", "claude-sonnet-5-5", 0, local, option.WithBaseURL(server.URL), option.WithMaxRetries(0))
	chunks, err := collect(t, m, Request{System: "s", MaxOutput: 10, Messages: []Message{{Role: User, Content: []Block{Text{Text: "hi"}}}}})
	require.NoError(t, err)
	require.Len(t, chunks, 3)
	use := chunks[0].(ToolUse)
	assert.True(t, json.Valid(use.Input), "%s", use.Input)
	assert.Equal(t, Stop{Reason: StopMaxTokens}, chunks[2])
}

func TestBlocksRoundTrip(t *testing.T) {
	blocks := []Block{Text{Text: "a"}, ToolUse{ID: "t", Name: "n", Input: json.RawMessage(`{"a":1}`)},
		ToolResult{ToolUseID: "t", Content: "c", IsError: true}, Reasoning{Writer: "anthropic:x", Signature: "s", Text: "x"},
		Reasoning{Writer: "anthropic:x", Redacted: "r"}}
	raw, err := MarshalBlocks(blocks)
	require.NoError(t, err)
	back, err := UnmarshalBlocks(raw)
	require.NoError(t, err)
	assert.Equal(t, blocks, back)
	_, err = UnmarshalBlocks(json.RawMessage(`[{"type":"image"}]`))
	assert.Error(t, err)
}

func TestNewBindsEveryKind(t *testing.T) {
	catalog, err := ai.VendoredCatalog()
	require.NoError(t, err)
	free := ai.Roles{Ask: ai.Model{Name: "qwen3", Price: ai.PriceFree{}}}
	for _, c := range []struct {
		provider ai.Provider
		models   ai.Roles
		want     Model
	}{
		{ai.Anthropic{}, ai.Roles{Ask: ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}}}, &anthropicModel{}},
		{ai.OpenAI{}, ai.Roles{Ask: ai.Model{Name: "gpt-6.1-sol", Price: ai.PriceTable{}}}, &responsesModel{}},
		{ai.AzureOpenAI{Endpoint: "https://acme.openai.azure.com"}, ai.Roles{Ask: ai.Model{Name: "sol", Price: ai.PriceTableAs{Model: "gpt-6.1-sol"}}}, &responsesModel{}},
		{ai.Compatible{BaseURL: "https://llm.example.com/v1", API: ai.CompatChatCompletions, MaxCallsInFlight: 4}, free, &chatModel{}},
		{ai.Compatible{BaseURL: "https://llm.example.com/v1", API: ai.CompatResponses, MaxCallsInFlight: 4}, free, &responsesModel{}},
		{ai.Ollama{BaseURL: "http://127.0.0.1:11434", ContextTokens: 65536}, free, &ollamaModel{}},
		{ai.Bedrock{Region: "us-east-1"}, ai.Roles{Ask: ai.Model{Name: "global.anthropic.claude-sonnet-5-5", Price: ai.PriceTable{}}}, &converseModel{}},
		{ai.Cohere{}, ai.Roles{Ask: ai.Model{Name: "command-a-03-2025", Price: ai.PriceRates{Input: 2500, Output: 10000}}}, &chatModel{}},
	} {
		b, err := New(ai.Resolved{Settings: ai.Settings{Provider: c.provider, Models: c.models}, Key: ontologyagent.NewModelKey("k")}, catalog)
		require.NoError(t, err, "%T", c.provider)
		assert.IsType(t, c.want, b.Model, "%T", c.provider)
	}
	_, err = New(ai.Resolved{Settings: ai.Settings{Provider: ai.Anthropic{}, Models: ai.Roles{Ask: ai.Model{Name: "claude-unknown", Price: ai.PriceTable{}}}}}, catalog)
	assert.Error(t, err, "an unpriced model is refused")
	_, err = New(ai.Resolved{Settings: ai.Settings{Provider: ai.Compatible{BaseURL: "https://x/v1", API: "grpc", MaxCallsInFlight: 1}, Models: free}}, catalog)
	assert.Error(t, err, "an API no wire speaks")
	_, err = New(ai.Resolved{Settings: ai.Settings{Provider: ai.AzureOpenAI{Endpoint: "https://example.com"}, Models: free}}, catalog)
	assert.Error(t, err, "an endpoint that isn't Azure's")
}

// Cohere's questions never send the reasoning knob, even where a probe's
// report would have them, as the engine's never do: its Compatibility API
// takes reasoning_effort only as none or high.
func TestNewSendsCohereNoReasoning(t *testing.T) {
	catalog, err := ai.VendoredCatalog()
	require.NoError(t, err)
	model := ai.Model{Name: "command-a-reasoning-08-2025", Price: ai.PriceRates{Input: 2500, Output: 10000}}
	b, err := New(ai.Resolved{Settings: ai.Settings{Provider: ai.Cohere{}, Models: ai.Roles{Draft: model, Ask: model, Extract: model, Verify: model}},
		Report: ai.ProbeReport{Checks: []ai.ProbeCheck{{Role: "ask", Passed: true, Reasoning: true}}}}, catalog)
	require.NoError(t, err)
	chat := b.Model.(*chatModel)
	assert.False(t, chat.reasoning)
	assert.Nil(t, chat.calls, "a cloud API, bounded by no gate")
	params, err := chat.params(Request{MaxOutput: 1000})
	require.NoError(t, err)
	assert.Empty(t, params.ReasoningEffort)
}

// Ollama runs in the owner's window, at most what the probe read of the
// model, and says so, so the Assistant fits the transcript to it.
func TestNewSetsOllamasWindow(t *testing.T) {
	catalog, err := ai.VendoredCatalog()
	require.NoError(t, err)
	model := ai.Model{Name: "qwen3", Price: ai.PriceFree{}}
	settings := ai.Settings{Provider: ai.Ollama{BaseURL: "http://127.0.0.1:11434", ContextTokens: 131072},
		Models: ai.Roles{Draft: model, Ask: model, Extract: model, Verify: model}}
	b, err := New(ai.Resolved{Settings: settings}, catalog)
	require.NoError(t, err)
	assert.Equal(t, 131072, b.Window)
	assert.Equal(t, 131072, b.Model.(*ollamaModel).window)
	b, err = New(ai.Resolved{Settings: settings, Report: ai.ProbeReport{Ollama: &ai.OllamaReport{ContextLength: 65536}}}, catalog)
	require.NoError(t, err)
	assert.Equal(t, 65536, b.Window, "the model's own window caps the owner's")
	b, err = New(ai.Resolved{Settings: ai.Settings{Provider: ai.Anthropic{}, Models: ai.Roles{Ask: ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}}}}}, catalog)
	require.NoError(t, err)
	assert.Zero(t, b.Window, "a provider that says nothing of one")
}

// NewFor binds a role's own model, priced as its own, its reasoning off
// except the ask model's, and Ollama in that role's window.
func TestNewForBindsTheRolesModel(t *testing.T) {
	catalog, err := ai.VendoredCatalog()
	require.NoError(t, err)
	roles := ai.Roles{Draft: ai.Model{Name: "gpt-6.1-sol", Price: ai.PriceTable{}}, Ask: ai.Model{Name: "gpt-6.1-sol", Price: ai.PriceTable{}},
		Extract: ai.Model{Name: "cheap", Price: ai.PriceRates{Input: 100, Output: 400}}, Verify: ai.Model{Name: "gpt-6.1-sol", Price: ai.PriceTable{}}}
	r := ai.Resolved{Settings: ai.Settings{Provider: ai.OpenAI{}, Models: roles}, Key: ontologyagent.NewModelKey("k")}
	b, err := NewFor(r, catalog, ai.RoleExtract)
	require.NoError(t, err)
	assert.Equal(t, "cheap", b.Model.(*responsesModel).model)
	assert.False(t, b.Model.(*responsesModel).reasoning, "short structured work runs without reasoning")
	assert.Equal(t, Pricer{Input: 100, Output: 400, CacheRead: 100, CacheWrite: 125}, b.Pricer)
	ask, err := NewFor(r, catalog, ai.RoleAsk)
	require.NoError(t, err)
	assert.Equal(t, "gpt-6.1-sol", ask.Model.(*responsesModel).model)
	assert.True(t, ask.Model.(*responsesModel).reasoning)
	for _, role := range []ai.Role{ai.RoleDraft, ai.RoleVerify} {
		_, err := NewFor(r, catalog, role)
		require.NoError(t, err, role)
	}
	_, err = NewFor(r, catalog, "title")
	assert.Error(t, err, "a role ai doesn't have")

	free := ai.Model{Name: "qwen3", Price: ai.PriceFree{}}
	ollama := ai.Resolved{Settings: ai.Settings{Provider: ai.Ollama{BaseURL: "http://127.0.0.1:11434", ContextTokens: 65536},
		Models: ai.Roles{Draft: free, Ask: free, Extract: free, Verify: free}}}
	b, err = NewFor(ollama, catalog, ai.RoleExtract)
	require.NoError(t, err)
	assert.Equal(t, 65536, b.Window)
}

// Hosted, a call dials only public addresses, whatever the host resolves
// to when it's made: a server on a private one is refused before a byte is
// sent, as the setting's to fix.
func TestHostedCallsRefusePrivateAddresses(t *testing.T) {
	hits := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer server.Close()
	m := newChat(server.URL, "", "m", false, 0, guarded(ai.Rules{}), nil)
	_, err := collect(t, m, Request{System: "s", MaxOutput: 10, Messages: []Message{{Role: User, Content: []Block{Text{Text: "hi"}}}}})
	var e *Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, ErrInvalid, e.Kind)
	assert.ErrorIs(t, err, connector.ErrPrivateAddress)
	assert.Zero(t, hits)
}

func TestGateBoundsCallsInFlight(t *testing.T) {
	g := gateOf("cfg_a", "https://one", 1)
	release, err := g.enter(context.Background())
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = gateOf("cfg_a", "https://one", 1).enter(ctx)
	var e *Error
	require.ErrorAs(t, err, &e, "a second call of the setting waits, until its context ends")
	assert.True(t, e.Retryable())
	// Another workspace's setting naming the same server has slots of its own.
	other, err := gateOf("cfg_b", "https://one", 1).enter(context.Background())
	require.NoError(t, err)
	other()
	release()
	again, err := g.enter(context.Background())
	require.NoError(t, err)
	again()
	slotsMu.Lock()
	assert.Empty(t, slots, "a gate no call holds or waits for is let go")
	slotsMu.Unlock()
	none, err := (*gate)(nil).enter(context.Background())
	require.NoError(t, err, "nil bounds nothing")
	none()
}

func TestStatusErrors(t *testing.T) {
	for code, kind := range map[int]ErrorKind{0: ErrUnavailable, 200: ErrUnavailable, 302: ErrInvalid, 400: ErrInvalid, 401: ErrAuth, 403: ErrAuth,
		404: ErrInvalid, 408: ErrUnavailable, 429: ErrRateLimited, 500: ErrUnavailable, 503: ErrUnavailable} {
		assert.Equal(t, kind, statusError(code, errors.New("x")).Kind, "%d", code)
	}
}

func TestCallIDs(t *testing.T) {
	assert.Equal(t, "call_7", callID("call_7"))
	a, b := callID(""), callID("")
	assert.True(t, strings.HasPrefix(a, "call_"))
	assert.NotEqual(t, a, b, "every call a server leaves unnamed gets an id of its own")
}

func TestSchemaOf(t *testing.T) {
	schema, err := schemaOf(Tool{Name: "t", InputSchema: json.RawMessage(`{"$schema":"x","$id":"y","properties":{}}`)})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"type": "object", "properties": map[string]any{}}, schema)
	schema, err = schemaOf(Tool{Name: "t"})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"type": "object"}, schema)
	_, err = schemaOf(Tool{Name: "t", InputSchema: json.RawMessage(`[`)})
	assert.Error(t, err)
}

func TestPricer(t *testing.T) {
	catalog, err := ai.VendoredCatalog()
	require.NoError(t, err)
	table, err := NewPricer(catalog, ai.KindAnthropic, ai.Model{Name: "claude-opus-5-5", Price: ai.PriceTable{}})
	require.NoError(t, err)
	assert.Equal(t, Pricer{Input: 4000, Output: 20000, CacheRead: 200, CacheWrite: 5000}, table)
	// 1,000 in, 100 out, 10,000 read, 1,000 written: 4 + 2 + 2 + 5 thousandths of a dollar.
	assert.EqualValues(t, 13_000, table.Micros(Usage{In: 1000, Out: 100, CacheRead: 10_000, CacheWrite: 1000}))
	assert.EqualValues(t, 1, table.Micros(Usage{CacheRead: 1}), "rounded up")

	as, err := NewPricer(catalog, ai.KindAnthropic, ai.Model{Name: "my-alias", Price: ai.PriceTableAs{Model: "claude-opus-5-5"}})
	require.NoError(t, err)
	assert.Equal(t, table, as)
	_, err = NewPricer(catalog, ai.KindAnthropic, ai.Model{Name: "claude-unknown", Price: ai.PriceTable{}})
	assert.Error(t, err, "an unpriced model is refused")

	read := uint64(1)
	rates, err := NewPricer(catalog, ai.KindAnthropic, ai.Model{Name: "x", Price: ai.PriceRates{Input: 10, Output: 20, CacheRead: &read}})
	require.NoError(t, err)
	assert.Equal(t, Pricer{Input: 10, Output: 20, CacheRead: 1, CacheWrite: 13}, rates)
	free, err := NewPricer(catalog, ai.KindAnthropic, ai.Model{Name: "x", Price: ai.PriceFree{}})
	require.NoError(t, err)
	assert.Zero(t, free.Micros(Usage{In: 1 << 20, Out: 1 << 20}))
}
