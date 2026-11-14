package llm

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeChunks writes a Chat Completions stream: each chunk as data, then [DONE].
func writeChunks(w http.ResponseWriter, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, c := range chunks {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", c)
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func chunk(delta, finish string) string {
	f := "null"
	if finish != "" {
		f = `"` + finish + `"`
	}
	return `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + f + `}]}`
}

const chatUsage = `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":40,"total_tokens":1040,"prompt_tokens_details":{"cached_tokens":800}}}`

func TestChatStreamsTextCallsAndUsage(t *testing.T) {
	var got recorded
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(t, &got, r)
		writeChunks(w,
			chunk(`{"role":"assistant","reasoning_content":"Hmm."}`, ""),
			chunk(`{"content":"Let me "}`, ""),
			chunk(`{"content":"check."}`, ""),
			chunk(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"helix_foundry_list_ontologies","arguments":"{\"workspace_"}}]}`, ""),
			chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"id\":\"ws1\"}"}}]}`, ""),
			chunk(`{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"helix_foundry_get_ontology","arguments":""}}]}`, ""),
			chunk(`{}`, "tool_calls"),
			chatUsage)
	}))
	defer server.Close()
	m := newChat(server.URL+"/v1", "local-key", "qwen3", true, 0, local, nil, option.WithMaxRetries(0))
	chunks, err := collect(t, m, Request{System: "You are the Assistant.", MaxOutput: 8192, Tools: []Tool{listTool}, Messages: transcript("openai_compatible:x")})
	require.NoError(t, err)
	assert.Equal(t, []Chunk{
		TextDelta{Text: "Let me "}, TextDelta{Text: "check."},
		Text{Text: "Let me check."},
		ToolUse{ID: "call_1", Name: "helix_foundry_list_ontologies", Input: json.RawMessage(`{"workspace_id":"ws1"}`)},
		ToolUse{ID: "call_2", Name: "helix_foundry_get_ontology", Input: json.RawMessage(`{}`)},
		Usage{In: 200, Out: 40, CacheRead: 800},
		Stop{Reason: StopToolUse},
	}, chunks, "the server's reasoning is never kept")

	assert.Equal(t, "/v1/chat/completions", got.path)
	assert.Equal(t, "Bearer local-key", got.header.Get("Authorization"))
	b := got.body
	assert.Equal(t, "qwen3", b["model"])
	assert.EqualValues(t, 8192, b["max_tokens"])
	assert.Equal(t, true, b["stream"])
	assert.Equal(t, map[string]any{"include_usage": true}, b["stream_options"])
	assert.Equal(t, "low", b["reasoning_effort"])
	fn := b["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	assert.Equal(t, "helix_foundry_list_ontologies", fn["name"])
	assert.NotContains(t, fn["parameters"], "$schema")
	messages := b["messages"].([]any)
	require.Len(t, messages, 4)
	assert.Equal(t, map[string]any{"role": "system", "content": "You are the Assistant."}, messages[0])
	assert.Equal(t, map[string]any{"role": "user", "content": "What ontologies?"}, messages[1])
	assistant := messages[2].(map[string]any)
	assert.Equal(t, "Let me look.", assistant["content"])
	assert.Equal(t, "call_0", assistant["tool_calls"].([]any)[0].(map[string]any)["id"])
	assert.Equal(t, map[string]any{"role": "tool", "tool_call_id": "call_0", "content": "Error: unavailable"}, messages[3])
}

// Some servers number every call 0, each with an id of its own; a server
// that counts nothing is charged the whole call.
func TestChatCallsByIDAndUncountedUsage(t *testing.T) {
	var got recorded
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		record(t, &got, r)
		writeChunks(w,
			chunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"one","arguments":"{}"}}]}`, ""),
			chunk(`{"tool_calls":[{"index":0,"id":"b","function":{"name":"two","arguments":"{}"}}]}`, "tool_calls"))
	}))
	defer server.Close()
	m := newChat(server.URL, "", "m", false, 0, local, nil, option.WithMaxRetries(0))
	chunks, err := collect(t, m, Request{System: "s", MaxOutput: 500, Messages: []Message{{Role: User, Content: []Block{Text{Text: "hi"}}}}})
	require.NoError(t, err)
	require.Len(t, chunks, 4)
	assert.Equal(t, "one", chunks[0].(ToolUse).Name)
	assert.Equal(t, "two", chunks[1].(ToolUse).Name)
	usage := chunks[2].(Usage)
	assert.EqualValues(t, 500, usage.Out, "all it could write")
	assert.Positive(t, usage.In)
	assert.Empty(t, got.header.Get("Authorization"), "a server without a key is sent none")
	assert.NotContains(t, got.body, "reasoning_effort", "a model without the knob is sent none")
}

// Some servers and proxies repeat a call's id and name on every chunk of
// it: still one call, its arguments whole.
func TestChatCallsWithRepeatedIDsAndNames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeChunks(w,
			chunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"record_word","arguments":""}}]}`, ""),
			chunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"record_word","arguments":"{\"word\""}}]}`, ""),
			chunk(`{"tool_calls":[{"index":0,"id":"c1","function":{"name":"record_word","arguments":":\"ready\"}"}}]}`, ""),
			// A name a server streams in pieces still adds up.
			chunk(`{"tool_calls":[{"index":1,"id":"c2","function":{"name":"record_","arguments":"{}"}}]}`, ""),
			chunk(`{"tool_calls":[{"index":1,"function":{"name":"word"}}]}`, "tool_calls"),
			chatUsage)
	}))
	defer server.Close()
	m := newChat(server.URL, "", "m", false, 0, local, nil, option.WithMaxRetries(0))
	chunks, err := collect(t, m, Request{System: "s", MaxOutput: 10})
	require.NoError(t, err)
	assert.Equal(t, []Chunk{
		ToolUse{ID: "c1", Name: "record_word", Input: json.RawMessage(`{"word":"ready"}`)},
		ToolUse{ID: "c2", Name: "record_word", Input: json.RawMessage(`{}`)},
		Usage{In: 200, Out: 40, CacheRead: 800},
		Stop{Reason: StopToolUse},
	}, chunks)
}

func TestChatStopsAndFailures(t *testing.T) {
	for name, c := range map[string]struct {
		chunks []string
		stop   StopReason
		fail   ErrorKind
	}{
		"answered": {chunks: []string{chunk(`{"content":"Done."}`, "stop"), chatUsage}, stop: StopEnd},
		"length":   {chunks: []string{chunk(`{"content":"Hal"}`, "length"), chatUsage}, stop: StopMaxTokens},
		"filtered": {chunks: []string{chunk(`{}`, "content_filter")}, stop: StopRefusal},
		"refusal":  {chunks: []string{chunk(`{"refusal":"No."}`, "")}, stop: StopRefusal},
		"cut off":  {chunks: []string{chunk(`{"content":"Hal"}`, "")}, fail: ErrUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeChunks(w, c.chunks...) }))
			defer server.Close()
			m := newChat(server.URL, "", "m", false, 0, local, nil, option.WithMaxRetries(0))
			chunks, err := collect(t, m, Request{System: "s", MaxOutput: 10})
			if c.fail != "" {
				var e *Error
				require.ErrorAs(t, err, &e)
				assert.Equal(t, c.fail, e.Kind)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, Stop{Reason: c.stop}, chunks[len(chunks)-1])
		})
	}
	for status, kind := range map[int]ErrorKind{401: ErrAuth, 404: ErrInvalid, 429: ErrRateLimited, 500: ErrUnavailable} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"no"}}`))
		}))
		m := newChat(server.URL, "sk-SECRET-KEY", "m", false, 0, local, nil, option.WithMaxRetries(0))
		_, err := collect(t, m, Request{System: "s", MaxOutput: 10})
		var e *Error
		require.True(t, errors.As(err, &e), "%d: %v", status, err)
		assert.Equal(t, kind, e.Kind, "%d", status)
		assert.NotContains(t, err.Error(), "SECRET")
		server.Close()
	}
}

// An OpenAI-compatible server is sent at most its MaxCallsInFlight at once,
// however many turns call it.
func TestChatHonoursCallsInFlight(t *testing.T) {
	var inFlight, most atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		now := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			seen := most.Load()
			if now <= seen || most.CompareAndSwap(seen, now) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		writeChunks(w, chunk(`{"content":"ok"}`, "stop"), chatUsage)
	}))
	defer server.Close()
	calls := gateOf("cfg", server.URL, 2)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := newChat(server.URL, "", "m", false, 0, local, calls, option.WithMaxRetries(0))
			_, err := collect(t, m, Request{System: "s", MaxOutput: 10})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	assert.EqualValues(t, 2, most.Load())
}
