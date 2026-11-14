package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/ai"
)

func TestCheckToolsSaysWhatARetryWouldFindAgain(t *testing.T) {
	catalog, err := ai.VendoredCatalog()
	require.NoError(t, err)
	free := ai.Model{Name: "qwen3", Price: ai.PriceFree{}}
	for name, c := range map[string]struct {
		status int
		chunks []string
		want   ai.AssistantCheck
		err    bool
	}{
		"called": {chunks: []string{chunk(`{"tool_calls":[{"index":0,"id":"c","function":{"name":"record_word","arguments":"{\"word\":\"ready\"}"}}]}`, "tool_calls"), chatUsage},
			want: ai.AssistantPassed},
		"answered in words": {chunks: []string{chunk(`{"content":"ready"}`, "stop"), chatUsage}, want: ai.AssistantNoToolCall},
		"called another":    {chunks: []string{chunk(`{"tool_calls":[{"index":0,"id":"c","function":{"name":"other","arguments":"{}"}}]}`, "tool_calls")}, want: ai.AssistantNoToolCall},
		"called with a list": {chunks: []string{chunk(`{"tool_calls":[{"index":0,"id":"c","function":{"name":"record_word","arguments":"[1]"}}]}`, "tool_calls")},
			want: ai.AssistantNoToolCall},
		"key refused":     {status: 401, want: ai.AssistantAuth},
		"request refused": {status: 400, want: ai.AssistantRefused},
		"rate limited":    {status: 429, want: ai.AssistantUnchecked, err: true},
		"cut off":         {chunks: []string{chunk(`{"content":"rea"}`, "")}, want: ai.AssistantUnchecked, err: true},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if c.status != 0 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(c.status)
					_, _ = w.Write([]byte(`{"error":{"message":"no"}}`))
					return
				}
				writeChunks(w, c.chunks...)
			}))
			defer server.Close()
			r := ai.Resolved{
				Settings: ai.Settings{Provider: ai.Compatible{BaseURL: server.URL + "/v1", API: ai.CompatChatCompletions, MaxCallsInFlight: 1},
					Models: ai.Roles{Draft: free, Ask: free, Extract: free, Verify: free}},
				Rules: ai.Rules{AllowPrivate: true},
			}
			check, err := CheckTools(context.Background(), r, catalog)
			assert.Equal(t, c.want, check)
			assert.Equal(t, c.err, err != nil, "%v", err)
		})
	}
	// A model the catalog can't price is no check at all.
	check, err := CheckTools(context.Background(), ai.Resolved{Settings: ai.Settings{Provider: ai.Anthropic{},
		Models: ai.Roles{Ask: ai.Model{Name: "claude-unknown", Price: ai.PriceTable{}}}}}, catalog)
	assert.Error(t, err)
	assert.Equal(t, ai.AssistantUnchecked, check)
}

func TestAssistantCheckRuns(t *testing.T) {
	for check, runs := range map[ai.AssistantCheck]bool{ai.AssistantUnchecked: true, ai.AssistantPassed: true,
		ai.AssistantAuth: false, ai.AssistantRefused: false, ai.AssistantNoToolCall: false} {
		assert.Equal(t, runs, check.Runs(), check)
	}
}
