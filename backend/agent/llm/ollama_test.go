package llm

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fastRetries makes the raw wires' retries quick for a test.
func fastRetries(t *testing.T) {
	t.Helper()
	was := retryBackoff
	retryBackoff = time.Millisecond
	t.Cleanup(func() { retryBackoff = was })
}

func writeLines(w http.ResponseWriter, lines ...string) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	for _, l := range lines {
		_, _ = fmt.Fprintln(w, l)
	}
}

const ollamaDone = `{"model":"qwen3","message":{"role":"assistant","content":""},"done":true,"done_reason":"stop","prompt_eval_count":900,"eval_count":30}`

func TestOllamaStreamsInItsWindow(t *testing.T) {
	var got recorded
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(t, &got, r)
		writeLines(w,
			`{"model":"qwen3","message":{"role":"assistant","content":"Let me "},"done":false}`,
			`{"model":"qwen3","message":{"role":"assistant","content":"check."},"done":false}`,
			`{"model":"qwen3","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"helix_foundry_list_ontologies","arguments":{"workspace_id":"ws1"}}}]},"done":false}`,
			ollamaDone)
	}))
	defer server.Close()
	m := newOllama(server.URL+"/", "qwen3", 65536, 0, local, nil)
	chunks, err := collect(t, m, Request{System: "You are the Assistant.", MaxOutput: 8192, Tools: []Tool{listTool}, Messages: transcript("ollama:x")})
	require.NoError(t, err)
	require.Len(t, chunks, 6)
	assert.Equal(t, []Chunk{TextDelta{Text: "Let me "}, TextDelta{Text: "check."}, Text{Text: "Let me check."}}, chunks[:3])
	use := chunks[3].(ToolUse)
	assert.Equal(t, "helix_foundry_list_ontologies", use.Name)
	assert.JSONEq(t, `{"workspace_id":"ws1"}`, string(use.Input))
	assert.True(t, strings.HasPrefix(use.ID, "call_"), "a call Ollama names no id for gets one")
	assert.Equal(t, Usage{In: 900, Out: 30}, chunks[4])
	assert.Equal(t, Stop{Reason: StopToolUse}, chunks[5])

	assert.Equal(t, "/api/chat", got.path, "the native API, which sets the window")
	assert.Empty(t, got.header.Get("Authorization"), "Ollama takes no key")
	b := got.body
	assert.Equal(t, map[string]any{"num_ctx": float64(65536), "num_predict": float64(8192)}, b["options"])
	assert.Equal(t, false, b["think"])
	assert.Equal(t, true, b["stream"])
	assert.Equal(t, "10m", b["keep_alive"])
	params := b["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	assert.NotContains(t, params, "$schema")
	messages := b["messages"].([]any)
	require.Len(t, messages, 4)
	assert.Equal(t, map[string]any{"role": "system", "content": "You are the Assistant."}, messages[0])
	assistant := messages[2].(map[string]any)
	call := assistant["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	assert.Equal(t, map[string]any{"workspace_id": "ws1"}, call["arguments"], "arguments go as an object")
	assert.Equal(t, map[string]any{"role": "tool", "content": "Error: unavailable", "tool_name": "helix_foundry_list_ontologies"}, messages[3],
		"a result names its tool: the native API has no call ids")
}

func TestOllamaStopsAndFailures(t *testing.T) {
	fastRetries(t)
	var calls atomic.Int32
	for name, c := range map[string]struct {
		status int
		body   string
		lines  []string
		stop   StopReason
		fail   ErrorKind
		words  string
	}{
		"length":   {lines: []string{`{"message":{"content":"Hal"},"done":false}`, `{"message":{"content":""},"done":true,"done_reason":"length","prompt_eval_count":1,"eval_count":10}`}, stop: StopMaxTokens},
		"no model": {status: 404, body: `{"error":"model \"qwen9\" not found, try pulling it first"}`, fail: ErrInvalid, words: "not found"},
		"no tools": {status: 400, body: `{"error":"registry.ollama.ai/library/gemma:2b does not support tools"}`, fail: ErrInvalid, words: "does not support tools"},
		"broke":    {lines: []string{`{"message":{"content":"Hal"},"done":false}`, `{"error":"model runner has unexpectedly stopped"}`}, fail: ErrUnavailable, words: "unexpectedly stopped"},
		"cut off":  {lines: []string{`{"message":{"content":"Hal"},"done":false}`}, fail: ErrUnavailable},
		"not json": {lines: []string{`<html>`}, fail: ErrUnavailable},
		"busy":     {status: 503, body: `{"error":"server busy"}`, fail: ErrUnavailable, words: "server busy"},
	} {
		t.Run(name, func(t *testing.T) {
			calls.Store(0)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				if c.status != 0 {
					w.WriteHeader(c.status)
					_, _ = w.Write([]byte(c.body))
					return
				}
				writeLines(w, c.lines...)
			}))
			defer server.Close()
			chunks, err := collect(t, newOllama(server.URL, "qwen3", 65536, 0, local, nil), Request{System: "s", MaxOutput: 10})
			if c.fail == "" {
				require.NoError(t, err)
				assert.Equal(t, Stop{Reason: c.stop}, chunks[len(chunks)-1])
				return
			}
			var e *Error
			require.ErrorAs(t, err, &e)
			assert.Equal(t, c.fail, e.Kind)
			assert.Contains(t, err.Error(), c.words)
			if c.status == 503 {
				assert.EqualValues(t, 1+retries, calls.Load(), "a busy server is tried again")
			}
		})
	}
}

// A server that fails a call it may pass later is tried again, and the
// answer that comes is the call's.
func TestOllamaRetriesABusyServer(t *testing.T) {
	fastRetries(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeLines(w, `{"message":{"content":"ok"},"done":false}`, ollamaDone)
	}))
	defer server.Close()
	chunks, err := collect(t, newOllama(server.URL, "qwen3", 65536, 0, local, nil), Request{System: "s", MaxOutput: 10})
	require.NoError(t, err)
	assert.Contains(t, chunks, Chunk(Text{Text: "ok"}))
	assert.EqualValues(t, 2, calls.Load())
}

// Ollama runs one call at a time: a second waits for the first.
func TestOllamaRunsOneCallAtATime(t *testing.T) {
	var inFlight, most atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		now := inFlight.Add(1)
		defer inFlight.Add(-1)
		if now > most.Load() {
			most.Store(now)
		}
		time.Sleep(20 * time.Millisecond)
		writeLines(w, ollamaDone)
	}))
	defer server.Close()
	calls := gateOf("cfg", server.URL, 1)
	done := make(chan error, 3)
	for range 3 {
		go func() {
			_, err := collect(t, newOllama(server.URL, "qwen3", 65536, 0, local, calls), Request{System: "s", MaxOutput: 10})
			done <- err
		}()
	}
	for range 3 {
		require.NoError(t, <-done)
	}
	assert.EqualValues(t, 1, most.Load())
	raw, err := newOllama("http://x", "m", 65536, 0, local, nil).body(Request{System: "s", MaxOutput: 10,
		Messages: []Message{{Role: Assistant, Content: []Block{Reasoning{Writer: "ollama:x", Text: "t"}}}}})
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	assert.Len(t, body["messages"], 1, "a message of reasoning alone sends nothing")
}
