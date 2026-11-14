package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"time"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"

	"github.com/helixdb/helix-foundry/backend/ai"
)

// anthropicModel is Claude through the Messages API, streamed. The system
// prompt and tools are cached as one prefix, and the transcript after them,
// so each step of a turn reads what the step before wrote.
type anthropicModel struct {
	client anthropic.Client
	model  string
}

// newAnthropic is model under key. The client reads nothing of the process's
// environment: the key is the workspace's, or Helix's default, never a stray
// ANTHROPIC_API_KEY. opts follow, for tests.
func newAnthropic(key, model string, timeout time.Duration, opts ...option.RequestOption) *anthropicModel {
	base := []option.RequestOption{option.WithoutEnvironmentDefaults(), option.WithAPIKey(key), option.WithMaxRetries(2)}
	if timeout > 0 {
		base = append(base, option.WithRequestTimeout(timeout))
	}
	return &anthropicModel{client: anthropic.NewClient(append(base, opts...)...), model: model}
}

func (m *anthropicModel) Stream(ctx context.Context, r Request) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		params, err := m.params(r)
		if err != nil {
			yield(nil, err)
			return
		}
		stream := m.client.Messages.NewStreaming(ctx, params)
		defer func() { _ = stream.Close() }()
		var message anthropic.Message
		for stream.Next() {
			if err := message.Accumulate(stream.Current()); err != nil {
				yield(nil, &Error{Kind: ErrUnavailable, Err: err})
				return
			}
		}
		if err := stream.Err(); err != nil {
			yield(nil, anthropicError(err))
			return
		}
		for _, block := range message.Content {
			var chunk Chunk
			switch b := block.AsAny().(type) {
			case anthropic.TextBlock:
				chunk = Text{Text: b.Text}
			case anthropic.ToolUseBlock:
				input := b.Input
				if len(input) == 0 {
					input = json.RawMessage(`{}`)
				}
				chunk = ToolUse{ID: b.ID, Name: b.Name, Input: input}
			case anthropic.ThinkingBlock:
				chunk = Reasoning{Provider: ai.KindAnthropic, Signature: b.Signature, Text: b.Thinking}
			case anthropic.RedactedThinkingBlock:
				chunk = Reasoning{Provider: ai.KindAnthropic, Redacted: b.Data}
			default:
				continue // server tools, which the Assistant doesn't declare
			}
			if !yield(chunk, nil) {
				return
			}
		}
		u := message.Usage
		if !yield(Usage{In: u.InputTokens, Out: u.OutputTokens, CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheCreationInputTokens}, nil) {
			return
		}
		yield(Stop{Reason: anthropicStop(message.StopReason)}, nil)
	}
}

func (m *anthropicModel) params(r Request) (anthropic.MessageNewParams, error) {
	params := anthropic.MessageNewParams{
		Model: anthropic.Model(m.model), MaxTokens: int64(r.MaxOutput),
		System:       []anthropic.TextBlockParam{{Text: r.System, CacheControl: anthropic.NewCacheControlEphemeralParam()}},
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
	}
	for _, t := range r.Tools {
		var schema map[string]any
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
			return params, fmt.Errorf("llm: tool %s's schema: %w", t.Name, err)
		}
		in := anthropic.ToolInputSchemaParam{Properties: schema["properties"], ExtraFields: map[string]any{}}
		if required, ok := schema["required"].([]any); ok {
			for _, name := range required {
				if s, ok := name.(string); ok {
					in.Required = append(in.Required, s)
				}
			}
		}
		for key, value := range schema {
			switch key {
			case "type", "properties", "required", "$schema", "$id":
			default:
				in.ExtraFields[key] = value
			}
		}
		tool := anthropic.ToolParam{Name: t.Name, Description: anthropic.String(t.Description), InputSchema: in}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &tool})
	}
	for _, msg := range r.Messages {
		var blocks []anthropic.ContentBlockParamUnion
		for _, b := range msg.Content {
			switch b := b.(type) {
			case Text:
				blocks = append(blocks, anthropic.NewTextBlock(b.Text))
			case ToolUse:
				blocks = append(blocks, anthropic.NewToolUseBlock(b.ID, b.Input, b.Name))
			case ToolResult:
				blocks = append(blocks, anthropic.NewToolResultBlock(b.ToolUseID, b.Content, b.IsError))
			case Reasoning:
				switch {
				case b.Provider != ai.KindAnthropic:
				case b.Redacted != "":
					blocks = append(blocks, anthropic.NewRedactedThinkingBlock(b.Redacted))
				default:
					blocks = append(blocks, anthropic.NewThinkingBlock(b.Signature, b.Text))
				}
			default:
				return params, fmt.Errorf("llm: unknown block %T", b)
			}
		}
		switch msg.Role {
		case User:
			params.Messages = append(params.Messages, anthropic.NewUserMessage(blocks...))
		case Assistant:
			params.Messages = append(params.Messages, anthropic.NewAssistantMessage(blocks...))
		default:
			return params, fmt.Errorf("llm: unknown role %q", msg.Role)
		}
	}
	return params, nil
}

func anthropicStop(reason anthropic.StopReason) StopReason {
	switch reason {
	case anthropic.StopReasonToolUse:
		return StopToolUse
	case anthropic.StopReasonMaxTokens:
		return StopMaxTokens
	case anthropic.StopReasonRefusal:
		return StopRefusal
	case anthropic.StopReasonModelContextWindowExceeded:
		return StopContext
	default:
		return StopEnd
	}
}

// anthropicError is a failed call as an *Error: the API's status says
// whether a retry may pass; no answer at all may.
func anthropicError(err error) error {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return &Error{Kind: ErrUnavailable, Err: err}
	}
	switch code := apiErr.StatusCode; {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return &Error{Kind: ErrAuth, Err: err}
	case code == http.StatusTooManyRequests:
		return &Error{Kind: ErrRateLimited, Err: err}
	case code >= 500:
		return &Error{Kind: ErrUnavailable, Err: err}
	default:
		return &Error{Kind: ErrInvalid, Err: err}
	}
}
