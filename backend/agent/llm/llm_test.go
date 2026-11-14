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

	"github.com/anthropics/anthropic-sdk-go/v2/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
)

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
	m := newAnthropic("sk-ws", "claude-sonnet-5-5", 0, option.WithBaseURL(server.URL), option.WithMaxRetries(0))
	chunks, err := collect(t, m, Request{
		System: "You are the Assistant.", MaxOutput: 1000,
		Tools: []Tool{{Name: "helix_foundry_list_ontologies", Description: "List them.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"workspace_id":{"type":"string"}},"required":["workspace_id"],"additionalProperties":false,"$schema":"x"}`)}},
		Messages: []Message{
			{Role: User, Content: []Block{Text{Text: "What ontologies?"}}},
			{Role: Assistant, Content: []Block{Reasoning{Provider: ai.KindAnthropic, Signature: "s0", Text: "t0"}, Reasoning{Provider: ai.KindOpenAI, Text: "dropped"},
				ToolUse{ID: "toolu_0", Name: "helix_foundry_get_context", Input: json.RawMessage(`{"x":1}`)}}},
			{Role: User, Content: []Block{ToolResult{ToolUseID: "toolu_0", Content: "nothing", IsError: true}}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "Bearer sk-ws", key)
	assert.Equal(t, []Chunk{
		Reasoning{Provider: ai.KindAnthropic, Signature: "sig-1", Text: "Look it up."},
		Text{Text: "Let me check."},
		ToolUse{ID: "toolu_1", Name: "helix_foundry_list_ontologies", Input: json.RawMessage(`{"workspace_id":"ws1"}`)},
		Usage{In: 100, Out: 40, CacheRead: 2000, CacheWrite: 300},
		Stop{Reason: StopToolUse},
	}, chunks)

	// The prefix is cached, the tool's schema sent whole but for $schema, and
	// another provider's reasoning dropped.
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
		m := newAnthropic("sk-SECRET-KEY", "claude-sonnet-5-5", 0, option.WithBaseURL(server.URL), option.WithMaxRetries(0))
		_, err := collect(t, m, Request{System: "s", MaxOutput: 10, Messages: []Message{{Role: User, Content: []Block{Text{Text: "hi"}}}}})
		var e *Error
		require.True(t, errors.As(err, &e), "%d: %v", status, err)
		assert.Equal(t, kind, e.Kind, "%d", status)
		assert.Equal(t, kind == ErrRateLimited || kind == ErrUnavailable, e.Retryable())
		assert.NotContains(t, err.Error(), "SECRET", "the key never reaches an error")
		server.Close()
	}
}

func TestBlocksRoundTrip(t *testing.T) {
	blocks := []Block{Text{Text: "a"}, ToolUse{ID: "t", Name: "n", Input: json.RawMessage(`{"a":1}`)},
		ToolResult{ToolUseID: "t", Content: "c", IsError: true}, Reasoning{Provider: ai.KindAnthropic, Signature: "s", Text: "x"},
		Reasoning{Provider: ai.KindAnthropic, Redacted: "r"}}
	raw, err := MarshalBlocks(blocks)
	require.NoError(t, err)
	back, err := UnmarshalBlocks(raw)
	require.NoError(t, err)
	assert.Equal(t, blocks, back)
	_, err = UnmarshalBlocks(json.RawMessage(`[{"type":"image"}]`))
	assert.Error(t, err)
}

func TestNewCallsClaudeAlone(t *testing.T) {
	catalog, err := ai.VendoredCatalog()
	require.NoError(t, err)
	ask := ai.Model{Name: "claude-sonnet-5-5", Price: ai.PriceTable{}}
	m, pricer, err := New(ai.Resolved{Settings: ai.Settings{Provider: ai.Anthropic{}}, Key: ontologyagent.NewModelKey("sk")}, ask, catalog)
	require.NoError(t, err)
	assert.NotNil(t, m)
	assert.NotZero(t, pricer.Output)
	for _, p := range []ai.Provider{ai.OpenAI{}, ai.AzureOpenAI{}, ai.Bedrock{}, ai.Ollama{}, ai.Compatible{}} {
		_, _, err := New(ai.Resolved{Settings: ai.Settings{Provider: p}}, ai.Model{Name: "x", Price: ai.PriceFree{}}, catalog)
		assert.ErrorIs(t, err, ErrUnsupported, "%T", p)
	}
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
	assert.True(t, strings.HasPrefix(ErrUnsupported.Error(), "llm:"))
}
