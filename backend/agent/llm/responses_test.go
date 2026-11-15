package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/ai"
)

// writeOpenAISSE writes events as OpenAI streams them: each its type as the
// event's name and its JSON as data.
func writeOpenAISSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, e := range events {
		var head struct{ Type string }
		_ = json.Unmarshal([]byte(e), &head)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", head.Type, e)
	}
}

// recorded is the last request a fake provider was sent.
type recorded struct {
	path   string
	header http.Header
	body   map[string]any
}

func record(t *testing.T, into *recorded, r *http.Request) {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	into.path, into.header = r.URL.Path, r.Header.Clone()
	into.body = map[string]any{}
	require.NoError(t, json.Unmarshal(raw, &into.body))
}

// transcript is a turn's: a question, a call with its reasoning (one of
// writer's, one of another model's), its failed result, and the model's last word.
func transcript(writer string) []Message {
	return []Message{
		{Role: User, Content: []Block{Text{Text: "What ontologies?"}}},
		{Role: Assistant, Content: []Block{
			Reasoning{Writer: writer, Signature: "rs_1", Redacted: "gAAA-sealed"},
			Reasoning{Writer: writerOf(ai.KindAnthropic, "", "claude-opus-5-5"), Signature: "sig", Text: "dropped"},
			Text{Text: "Let me look."},
			ToolUse{ID: "call_0", Name: "helix_foundry_list_ontologies", Input: json.RawMessage(`{"workspace_id":"ws1"}`)},
		}},
		{Role: User, Content: []Block{ToolResult{ToolUseID: "call_0", Content: "unavailable", IsError: true}}},
	}
}

var listTool = Tool{Name: "helix_foundry_list_ontologies", Description: "List them.",
	InputSchema: json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"workspace_id":{"type":"string"}},"required":["workspace_id"]}`)}

var responsesStream = []string{
	`{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`,
	`{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_2","encrypted_content":"gAAA-next","summary":[]}}`,
	`{"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"Let me "}`,
	`{"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"content_index":0,"delta":"check."}`,
	`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Let me check.","annotations":[]}]}}`,
	`{"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":2,"delta":"{\"workspace_id\":"}`,
	`{"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"helix_foundry_list_ontologies","arguments":"{\"workspace_id\":\"ws1\"}","status":"completed"}}`,
	`{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":600},"output_tokens":50,"output_tokens_details":{"reasoning_tokens":20},"total_tokens":1050}}}`,
}

func TestResponsesStreamsTextCallsReasoningAndUsage(t *testing.T) {
	var got recorded
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(t, &got, r)
		writeOpenAISSE(w, responsesStream...)
	}))
	defer server.Close()
	m := newResponses(responsesOpenAI, server.URL+"/v1", "sk-oa", "gpt-6.1-sol", true, 0, local, nil, option.WithMaxRetries(0))
	chunks, err := collect(t, m, Request{System: "You are the Assistant.", MaxOutput: 8192, Tools: []Tool{listTool}, Messages: transcript(m.writer)})
	require.NoError(t, err)
	assert.Equal(t, []Chunk{
		TextDelta{Text: "Let me "}, TextDelta{Text: "check."},
		Reasoning{Writer: m.writer, Signature: "rs_2", Redacted: "gAAA-next"},
		Text{Text: "Let me check."},
		ToolUse{ID: "call_1", Name: "helix_foundry_list_ontologies", Input: json.RawMessage(`{"workspace_id":"ws1"}`)},
		Usage{In: 400, Out: 50, CacheRead: 600},
		Stop{Reason: StopToolUse},
	}, chunks)

	assert.Equal(t, "/v1/responses", got.path)
	assert.Equal(t, "Bearer sk-oa", got.header.Get("Authorization"))
	assert.Empty(t, got.header.Get("api-key"))
	b := got.body
	assert.Equal(t, "gpt-6.1-sol", b["model"])
	assert.Equal(t, "You are the Assistant.", b["instructions"])
	assert.Equal(t, false, b["store"], "nothing is stored at the provider")
	assert.Equal(t, true, b["stream"])
	assert.EqualValues(t, 8192, b["max_output_tokens"])
	assert.Len(t, b["prompt_cache_key"], 32)
	assert.Equal(t, map[string]any{"effort": "low"}, b["reasoning"])
	assert.Equal(t, []any{"reasoning.encrypted_content"}, b["include"])
	tool := b["tools"].([]any)[0].(map[string]any)
	assert.Equal(t, "function", tool["type"])
	assert.Equal(t, false, tool["strict"])
	assert.NotContains(t, tool["parameters"], "$schema")
	input := b["input"].([]any)
	require.Len(t, input, 5, "the other provider's reasoning is dropped")
	assert.Equal(t, map[string]any{"role": "user", "content": "What ontologies?"}, input[0])
	assert.Equal(t, "reasoning", input[1].(map[string]any)["type"])
	assert.Equal(t, "gAAA-sealed", input[1].(map[string]any)["encrypted_content"])
	assert.Equal(t, map[string]any{"role": "assistant", "content": "Let me look."}, input[2])
	call := input[3].(map[string]any)
	assert.Equal(t, "function_call", call["type"])
	assert.Equal(t, "call_0", call["call_id"])
	result := input[4].(map[string]any)
	assert.Equal(t, "function_call_output", result["type"])
	assert.Equal(t, "Error: unavailable", result["output"], "the wire has no error flag")
}

// The same prefix keys the same cache, and another prefix another.
func TestResponsesCacheKeyFollowsThePrefix(t *testing.T) {
	m := newResponses(responsesOpenAI, "https://api.openai.com/v1", "k", "m", false, 0, local, nil)
	key := func(system string) string {
		p, err := m.params(Request{System: system, Tools: []Tool{listTool}, MaxOutput: 1})
		require.NoError(t, err)
		return p.PromptCacheKey.Value
	}
	assert.Equal(t, key("a"), key("a"))
	assert.NotEqual(t, key("a"), key("b"))
	p, err := m.params(Request{System: "s", MaxOutput: 1})
	require.NoError(t, err)
	assert.Empty(t, p.Reasoning.Effort, "a model without the knob is sent none")
	assert.Empty(t, p.Include)
}

func TestResponsesAzureAndCompatibleAuthorize(t *testing.T) {
	var got recorded
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(t, &got, r)
		writeOpenAISSE(w, responsesStream...)
	}))
	defer server.Close()
	azure := newResponses(responsesAzure, server.URL+"/openai/v1", "az-key", "sol-deployment", true, 0, local, nil, option.WithMaxRetries(0))
	request := Request{System: "s", MaxOutput: 10, Messages: transcript(azure.writer)}

	chunks, err := collect(t, azure, request)
	require.NoError(t, err)
	assert.Equal(t, "/openai/v1/responses", got.path)
	assert.Equal(t, "az-key", got.header.Get("api-key"))
	assert.Empty(t, got.header.Get("Authorization"), "Azure takes its key in api-key alone")
	assert.Equal(t, "sol-deployment", got.body["model"], "a model is a deployment")
	assert.Contains(t, chunks, Chunk(Reasoning{Writer: azure.writer, Signature: "rs_2", Redacted: "gAAA-next"}))
	assert.Len(t, got.body["input"], 5, "Azure's own reasoning goes back to it")

	keyless := newResponses(responsesCompatible, server.URL+"/v1", "", "local-model", false, 0, local, nil, option.WithMaxRetries(0))
	_, err = collect(t, keyless, request)
	require.NoError(t, err)
	assert.Empty(t, got.header.Get("Authorization"), "a server without a key is sent none")
	assert.Len(t, got.body["input"], 4, "another host's reasoning never goes back")
	keyed := newResponses(responsesCompatible, server.URL+"/v1", "local-key", "local-model", false, 0, local, nil, option.WithMaxRetries(0))
	_, err = collect(t, keyed, request)
	require.NoError(t, err)
	assert.Equal(t, "Bearer local-key", got.header.Get("Authorization"))
}

// Reasoning goes back to the model that wrote it, at its endpoint, while
// it reasons: never to another resource or server of its kind, another
// model, or a setting without the knob, which would refuse it.
func TestResponsesReasoningGoesBackToItsWriterOnly(t *testing.T) {
	wrote := newResponses(responsesAzure, "https://a.openai.azure.com/openai/v1", "k", "sol", true, 0, local, nil)
	sent := func(m *responsesModel) int {
		p, err := m.params(Request{System: "s", MaxOutput: 10, Messages: transcript(wrote.writer)})
		require.NoError(t, err)
		return len(p.Input.OfInputItemList)
	}
	assert.Equal(t, 5, sent(wrote))
	assert.Equal(t, 4, sent(newResponses(responsesAzure, "https://b.openai.azure.com/openai/v1", "k", "sol", true, 0, local, nil)), "another resource")
	assert.Equal(t, 4, sent(newResponses(responsesAzure, "https://a.openai.azure.com/openai/v1", "k", "luna", true, 0, local, nil)), "another model")
	assert.Equal(t, 4, sent(newResponses(responsesAzure, "https://a.openai.azure.com/openai/v1", "k", "sol", false, 0, local, nil)), "no reasoning knob")
	assert.Equal(t, 4, sent(newResponses(responsesCompatible, "https://a.openai.azure.com/openai/v1", "k", "sol", true, 0, local, nil)), "another kind")
}

func TestResponsesStops(t *testing.T) {
	for name, c := range map[string]struct {
		events []string
		want   StopReason
	}{
		"answered": {[]string{
			`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"Done."}]}}`,
			`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":2}}}`,
		}, StopEnd},
		"cut at its length": {[]string{
			`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"Hal"}]}}`,
			`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":10,"output_tokens":2}}}`,
		}, StopMaxTokens},
		"filtered": {[]string{
			`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"content_filter"}}}`,
		}, StopRefusal},
		"refused": {[]string{
			`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"refusal","refusal":"No."}]}}`,
			`{"type":"response.completed","response":{"status":"completed"}}`,
		}, StopRefusal},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeOpenAISSE(w, c.events...) }))
			defer server.Close()
			m := newResponses(responsesOpenAI, server.URL, "k", "m", false, 0, local, nil, option.WithMaxRetries(0))
			chunks, err := collect(t, m, Request{System: "s", MaxOutput: 10})
			require.NoError(t, err)
			assert.Equal(t, Stop{Reason: c.want}, chunks[len(chunks)-1])
		})
	}
}

func TestResponsesFailures(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		events []string
		want   ErrorKind
	}{
		"key refused":         {status: 401, want: ErrAuth},
		"no such deployment":  {status: 404, want: ErrInvalid},
		"rate limited":        {status: 429, want: ErrRateLimited},
		"down":                {status: 503, want: ErrUnavailable},
		"failed rate limited": {events: []string{`{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"slow down"}}}`}, want: ErrRateLimited},
		"failed prompt":       {events: []string{`{"type":"response.failed","response":{"status":"failed","error":{"code":"invalid_prompt","message":"no"}}}`}, want: ErrInvalid},
		"error event":         {events: []string{`{"type":"error","code":"server_error","message":"oops"}`}, want: ErrUnavailable},
		"cut off":             {events: []string{`{"type":"response.output_text.delta","delta":"Hal"}`}, want: ErrUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if c.status != 0 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(c.status)
					_, _ = w.Write([]byte(`{"error":{"message":"no","type":"x","code":"x"}}`))
					return
				}
				writeOpenAISSE(w, c.events...)
			}))
			defer server.Close()
			m := newResponses(responsesOpenAI, server.URL, "sk-SECRET-KEY", "m", false, 0, local, nil, option.WithMaxRetries(0))
			_, err := collect(t, m, Request{System: "s", MaxOutput: 10})
			var e *Error
			require.True(t, errors.As(err, &e), "%v", err)
			assert.Equal(t, c.want, e.Kind)
			assert.NotContains(t, err.Error(), "SECRET", "the key never reaches an error")
		})
	}
}

// A call a retry may pass is sent again, as openai-go retries.
func TestResponsesRetriesAnUnavailableCall(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeOpenAISSE(w, responsesStream...)
	}))
	defer server.Close()
	m := newResponses(responsesOpenAI, server.URL, "k", "m", false, 0, local, nil)
	_, err := collect(t, m, Request{System: "s", MaxOutput: 10})
	require.NoError(t, err)
	assert.EqualValues(t, 2, calls.Load())
}
