package llm

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/helixdb/helix-foundry/backend/ai"
)

// checkTool is the tool CheckTools asks the model to call.
const checkTool = "record_word"

// MaxOutput is the most one of the Assistant's calls may write: its tool
// check asks for as much, so a model that can't write that much fails the
// check, not every turn.
const MaxOutput = 8192

// CheckTools calls r's ask model once, on the Assistant's own wire, with one
// tool it is asked to call: whether the Assistant can run on the setting.
// The engine's probe checks the same model on the engine's wire; this one
// checks what only the Assistant sends (Bedrock's Converse with its bearer
// key, Ollama's native chat in its window, the tool schemas as the Assistant
// writes them).
//
// It reports only what a retry would find again: the key refused
// (ai.AssistantAuth), the request refused (ai.AssistantRefused: a model the
// endpoint lacks, tools it doesn't take), or an answer without the call
// (ai.AssistantNoToolCall). A call the provider couldn't answer (rate
// limited, unavailable, cut short) is an error, and the setting stays
// unchecked.
func CheckTools(ctx context.Context, r ai.Resolved, catalog *ai.Catalog) (ai.AssistantCheck, error) {
	b, err := New(r, catalog)
	if err != nil {
		return ai.AssistantUnchecked, err
	}
	req := Request{
		System: "You check that tool calls work. Answer only by calling the tool.", MaxOutput: MaxOutput,
		Messages: []Message{{Role: User, Content: []Block{Text{Text: "Call " + checkTool + " with the word \"ready\"."}}}},
		Tools: []Tool{{Name: checkTool, Description: "Records one word.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"word":{"type":"string","description":"The word to record."}},"required":["word"]}`)}},
	}
	called := false
	for chunk, err := range b.Model.Stream(ctx, req) {
		if err != nil {
			var refused *Error
			switch {
			case !errors.As(err, &refused):
				return ai.AssistantUnchecked, err
			case refused.Kind == ErrAuth:
				return ai.AssistantAuth, nil
			case refused.Kind == ErrInvalid:
				return ai.AssistantRefused, nil
			default:
				return ai.AssistantUnchecked, err
			}
		}
		if use, ok := chunk.(ToolUse); ok && use.Name == checkTool {
			var input map[string]any
			called = called || (json.Unmarshal(use.Input, &input) == nil && input != nil)
		}
	}
	if !called {
		return ai.AssistantNoToolCall, nil
	}
	return ai.AssistantPassed, nil
}
