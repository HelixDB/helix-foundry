package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/ai"
)

// frame is one message of a Converse stream: an event, or an exception.
type frame struct {
	kind, name, payload string
}

func event(name, payload string) frame { return frame{"event", name, payload} }

// writeFrames writes frames as Bedrock streams them: AWS's binary event stream.
func writeFrames(t *testing.T, w http.ResponseWriter, frames ...frame) {
	t.Helper()
	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
	encoder := eventstream.NewEncoder()
	for _, f := range frames {
		var headers eventstream.Headers
		headers.Set(":message-type", eventstream.StringValue(f.kind))
		if f.kind == "event" {
			headers.Set(":event-type", eventstream.StringValue(f.name))
		} else {
			headers.Set(":exception-type", eventstream.StringValue(f.name))
		}
		headers.Set(":content-type", eventstream.StringValue("application/json"))
		require.NoError(t, encoder.Encode(w, eventstream.Message{Headers: headers, Payload: []byte(f.payload)}))
	}
}

var converseStream = []frame{
	event("messageStart", `{"role":"assistant"}`),
	event("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"Let me "}}`),
	event("contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"check."}}`),
	event("contentBlockStop", `{"contentBlockIndex":0}`),
	event("contentBlockStart", `{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"tooluse_1","name":"helix_foundry_list_ontologies"}}}`),
	event("contentBlockDelta", `{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"workspace_id\":"}}}`),
	event("contentBlockDelta", `{"contentBlockIndex":1,"delta":{"toolUse":{"input":"\"ws1\"}"}}}`),
	event("contentBlockStop", `{"contentBlockIndex":1}`),
	event("messageStop", `{"stopReason":"tool_use"}`),
	event("metadata", `{"usage":{"inputTokens":120,"outputTokens":40,"totalTokens":2160,"cacheReadInputTokens":1500,"cacheWriteInputTokens":500},"metrics":{"latencyMs":800}}`),
}

func TestConverseStreamsWithTheBearerKey(t *testing.T) {
	// A key in the process's environment never replaces the workspace's.
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "ABSK-the-process-s")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIA-process")
	var got recorded
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(t, &got, r)
		got.path = r.URL.EscapedPath()
		writeFrames(t, w, converseStream...)
	}))
	defer server.Close()
	m := newConverse(server.URL, "ABSK-workspace", "global.anthropic.claude-sonnet-5-5", 0, local)
	chunks, err := collect(t, m, Request{System: "You are the Assistant.", MaxOutput: 8192, Tools: []Tool{listTool}, Messages: transcript(ai.KindBedrock)})
	require.NoError(t, err)
	assert.Equal(t, []Chunk{
		TextDelta{Text: "Let me "}, TextDelta{Text: "check."},
		Text{Text: "Let me check."},
		ToolUse{ID: "tooluse_1", Name: "helix_foundry_list_ontologies", Input: json.RawMessage(`{"workspace_id":"ws1"}`)},
		Usage{In: 120, Out: 40, CacheRead: 1500, CacheWrite: 500},
		Stop{Reason: StopToolUse},
	}, chunks)

	assert.Equal(t, "/model/global.anthropic.claude-sonnet-5-5/converse-stream", got.path)
	assert.Equal(t, "Bearer ABSK-workspace", got.header.Get("Authorization"), "the workspace's key, as a bearer token")
	assert.Empty(t, got.header.Get("X-Amz-Date"), "nothing is signed with SigV4")
	assert.Empty(t, got.header.Get("X-Amz-Security-Token"))
	b := got.body
	point := map[string]any{"cachePoint": map[string]any{"type": "default"}}
	assert.Equal(t, []any{map[string]any{"text": "You are the Assistant."}, point}, b["system"], "Claude's prefix is cached")
	tools := b["toolConfig"].(map[string]any)["tools"].([]any)
	require.Len(t, tools, 2)
	assert.Equal(t, point, tools[1])
	spec := tools[0].(map[string]any)["toolSpec"].(map[string]any)
	assert.NotContains(t, spec["inputSchema"].(map[string]any)["json"], "$schema")
	assert.Equal(t, map[string]any{"maxTokens": float64(8192)}, b["inferenceConfig"])
	messages := b["messages"].([]any)
	require.Len(t, messages, 3)
	assistant := messages[1].(map[string]any)["content"].([]any)
	require.Len(t, assistant, 2, "reasoning never goes back")
	assert.Equal(t, map[string]any{"toolUseId": "call_0", "name": "helix_foundry_list_ontologies", "input": map[string]any{"workspace_id": "ws1"}},
		assistant[1].(map[string]any)["toolUse"])
	last := messages[2].(map[string]any)["content"].([]any)
	require.Len(t, last, 2)
	assert.Equal(t, map[string]any{"toolUseId": "call_0", "content": []any{map[string]any{"text": "unavailable"}}, "status": "error"},
		last[0].(map[string]any)["toolResult"], "Claude takes a result's status")
	assert.Equal(t, point, last[1], "the transcript's end is cached")
}

// A model other than Claude gets no cache points, and one that takes no
// result status is told in the result's text; its id goes as one segment.
func TestConverseOtherModels(t *testing.T) {
	var got recorded
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(t, &got, r)
		got.path = r.URL.EscapedPath()
		writeFrames(t, w, converseStream...)
	}))
	defer server.Close()
	_, err := collect(t, newConverse(server.URL, "k", "mistral.mistral-large-3:0", 0, local), Request{System: "s", MaxOutput: 10, Tools: []Tool{listTool}, Messages: transcript(ai.KindBedrock)})
	require.NoError(t, err)
	assert.Equal(t, "/model/mistral.mistral-large-3%3A0/converse-stream", got.path)
	assert.Len(t, got.body["system"], 1)
	assert.Len(t, got.body["toolConfig"].(map[string]any)["tools"], 1)
	last := got.body["messages"].([]any)[2].(map[string]any)["content"].([]any)
	require.Len(t, last, 1)
	assert.Equal(t, map[string]any{"toolUseId": "call_0", "content": []any{map[string]any{"text": "Error: unavailable"}}}, last[0].(map[string]any)["toolResult"])

	_, err = collect(t, newConverse(server.URL, "k", "us.amazon.nova-pro-v1:0", 0, local), Request{System: "s", MaxOutput: 10, Messages: transcript(ai.KindBedrock)})
	require.NoError(t, err)
	assert.NotContains(t, got.body, "toolConfig", "no tools, no tool config")
	last = got.body["messages"].([]any)[2].(map[string]any)["content"].([]any)
	assert.Equal(t, "error", last[0].(map[string]any)["toolResult"].(map[string]any)["status"], "Nova takes a result's status")
}

func TestConverseStopsAndFailures(t *testing.T) {
	fastRetries(t)
	for name, c := range map[string]struct {
		status int
		frames []frame
		stop   StopReason
		fail   ErrorKind
		words  string
	}{
		"answered": {frames: []frame{event("contentBlockDelta", `{"delta":{"text":"Done."}}`), event("messageStop", `{"stopReason":"end_turn"}`),
			event("metadata", `{"usage":{"inputTokens":1,"outputTokens":1}}`)}, stop: StopEnd},
		"length": {frames: []frame{event("messageStop", `{"stopReason":"max_tokens"}`), event("metadata", `{"usage":{"inputTokens":1,"outputTokens":1}}`)}, stop: StopMaxTokens},
		"guarded": {frames: []frame{event("messageStop", `{"stopReason":"guardrail_intervened"}`), event("metadata", `{"usage":{"inputTokens":1,"outputTokens":1}}`)},
			stop: StopRefusal},
		"window": {frames: []frame{event("messageStop", `{"stopReason":"model_context_window_exceeded"}`), event("metadata", `{"usage":{"inputTokens":1,"outputTokens":1}}`)},
			stop: StopContext},
		"throttled":  {frames: []frame{event("messageStart", `{}`), {"exception", "throttlingException", `{"message":"Too many tokens"}`}}, fail: ErrRateLimited, words: "Too many tokens"},
		"stream err": {frames: []frame{{"exception", "modelStreamErrorException", `{"message":"boom"}`}}, fail: ErrUnavailable},
		"invalid":    {frames: []frame{{"exception", "validationException", `{"message":"bad"}`}}, fail: ErrInvalid},
		"cut off":    {frames: []frame{event("contentBlockDelta", `{"delta":{"text":"Hal"}}`)}, fail: ErrUnavailable},
		"key":        {status: 403, fail: ErrAuth, words: "invalid"},
		"no model":   {status: 400, fail: ErrInvalid, words: "invalid"},
		"down":       {status: 500, fail: ErrUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if c.status != 0 {
					w.Header().Set("X-Amzn-Errortype", "x")
					w.WriteHeader(c.status)
					_, _ = w.Write([]byte(`{"message":"The bearer token is invalid"}`))
					return
				}
				writeFrames(t, w, c.frames...)
			}))
			defer server.Close()
			chunks, err := collect(t, newConverse(server.URL, "ABSK-SECRET", "anthropic.claude-haiku-5-5", 0, local), Request{System: "s", MaxOutput: 10})
			if c.fail == "" {
				require.NoError(t, err)
				assert.Equal(t, Stop{Reason: c.stop}, chunks[len(chunks)-1])
				return
			}
			var e *Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, c.fail, e.Kind)
			assert.Contains(t, err.Error(), c.words)
			assert.NotContains(t, err.Error(), "SECRET")
		})
	}
}

func TestConverseRetriesThrottledCalls(t *testing.T) {
	fastRetries(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writeFrames(t, w, converseStream...)
	}))
	defer server.Close()
	_, err := collect(t, newConverse(server.URL, "k", "anthropic.claude-haiku-5-5", 0, local), Request{System: "s", MaxOutput: 10})
	require.NoError(t, err)
	assert.EqualValues(t, 3, calls.Load())
}

func TestConverseURLAndFamily(t *testing.T) {
	assert.Equal(t, "https://bedrock-runtime.us-east-1.amazonaws.com", converseURL("us-east-1", false))
	assert.Equal(t, "https://bedrock-runtime-fips.us-gov-west-1.amazonaws.com", converseURL("us-gov-west-1", true))
	assert.Equal(t, "https://bedrock-runtime.cn-north-1.amazonaws.com.cn", converseURL("cn-north-1", false))
	for model, want := range map[string]string{
		"anthropic.claude-opus-5-5": "anthropic", "global.anthropic.claude-sonnet-5-5": "anthropic", "us-gov.anthropic.claude-sonnet-5-5": "anthropic",
		"amazon.nova-pro-v1:0": "amazon", "arn:aws:bedrock:us-east-1:1:inference-profile/us.amazon.nova-pro-v1:0": "amazon",
		"mistral.mistral-large-3": "mistral", "meta.llama4": "meta",
	} {
		assert.Equal(t, want, family(model), model)
	}
	assert.Equal(t, "arn%3Aaws%3Abedrock%3Aus-east-1%3A1%3Ainference-profile%2Fus.amazon.nova-pro-v1%3A0",
		segment("arn:aws:bedrock:us-east-1:1:inference-profile/us.amazon.nova-pro-v1:0"))
}
