package llm

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// chatModel is a model of an OpenAI-compatible server through Chat
// Completions, streamed, as the engine calls one: usage asked for at the
// stream's end, the reasoning knob as reasoning_effort, and a server's own
// reasoning (reasoning_content), which no server takes back, dropped.
type chatModel struct {
	service   openai.ChatCompletionService
	model     string
	reasoning bool
	calls     gate
}

// newChat is model at baseURL, under key where there is one, through
// client: built from the Chat Completions service alone, never
// openai.NewClient, which would read OPENAI_* from the process's
// environment. opts follow, for tests.
func newChat(baseURL, key, model string, reasoning bool, timeout time.Duration, client *http.Client, calls gate, opts ...option.RequestOption) *chatModel {
	base := []option.RequestOption{option.WithBaseURL(baseURL), option.WithHTTPClient(client), option.WithMaxRetries(2)}
	if key != "" {
		base = append(base, option.WithAPIKey(key))
	}
	if timeout > 0 {
		base = append(base, option.WithRequestTimeout(timeout))
	}
	return &chatModel{service: openai.NewChatCompletionService(append(base, opts...)...), model: model, reasoning: reasoning, calls: calls}
}

// chatChunk is what the Assistant reads of a streamed chunk.
type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			Refusal   string `json:"refusal"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// chatCall is a tool call as its deltas build it.
type chatCall struct {
	index          int
	id, name, args string
}

func (m *chatModel) Stream(ctx context.Context, r Request) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		params, err := m.params(r)
		if err != nil {
			yield(nil, err)
			return
		}
		release, err := m.calls.enter(ctx)
		if err != nil {
			yield(nil, err)
			return
		}
		defer release()
		stream := m.service.NewStreaming(ctx, params)
		defer func() { _ = stream.Close() }()
		var text strings.Builder
		var calls []*chatCall
		var usage *Usage
		finish := ""
		for stream.Next() {
			var c chatChunk
			if err := json.Unmarshal([]byte(stream.Current().RawJSON()), &c); err != nil {
				yield(nil, &Error{Kind: ErrUnavailable, Err: err})
				return
			}
			if u := c.Usage; u != nil {
				cached := int64(0)
				if u.PromptTokensDetails != nil {
					cached = min(u.PromptTokensDetails.CachedTokens, u.PromptTokens)
				}
				usage = &Usage{In: u.PromptTokens - cached, Out: u.CompletionTokens, CacheRead: cached}
			}
			for _, choice := range c.Choices {
				if choice.FinishReason != "" {
					finish = choice.FinishReason
				}
				if choice.Delta.Refusal != "" {
					finish = "content_filter"
				}
				if d := choice.Delta.Content; d != "" {
					text.WriteString(d)
					if !yield(TextDelta{Text: d}, nil) {
						return
					}
				}
				for _, tc := range choice.Delta.ToolCalls {
					// A call goes on in its index's last call, unless it
					// names an id of its own: some servers number every call 0.
					var call *chatCall
					for _, c := range calls {
						if c.index == tc.Index {
							call = c
						}
					}
					if call == nil || (tc.ID != "" && call.id != "" && tc.ID != call.id) {
						call = &chatCall{index: tc.Index}
						calls = append(calls, call)
					}
					call.id += tc.ID
					call.name += tc.Function.Name
					call.args += tc.Function.Arguments
				}
			}
		}
		if err := stream.Err(); err != nil {
			yield(nil, openAIError(err))
			return
		}
		if finish == "" {
			yield(nil, &Error{Kind: ErrUnavailable, Err: errors.New("the stream ended before its answer did")})
			return
		}
		if text.Len() > 0 && !yield(Text{Text: text.String()}, nil) {
			return
		}
		for _, c := range calls {
			input := json.RawMessage(c.args)
			if strings.TrimSpace(c.args) == "" {
				input = json.RawMessage(`{}`)
			}
			if !yield(ToolUse{ID: callID(c.id), Name: c.name, Input: input}, nil) {
				return
			}
		}
		if usage == nil {
			// A server that counts nothing is charged the whole call: about
			// what it was sent, and all it could write.
			sent, _ := json.Marshal(params.Messages)
			usage = &Usage{In: int64(len(sent)/4 + 1), Out: int64(r.MaxOutput)}
		}
		if !yield(*usage, nil) {
			return
		}
		stop := StopEnd
		switch {
		case len(calls) > 0:
			stop = StopToolUse
		case finish == "length":
			stop = StopMaxTokens
		case finish == "content_filter":
			stop = StopRefusal
		}
		yield(Stop{Reason: stop}, nil)
	}
}

// params is r as a Chat Completions request: the system prompt first, a
// person's message as a user message after the results it carries, and the
// model's text and calls as one assistant message.
func (m *chatModel) params(r Request) (openai.ChatCompletionNewParams, error) {
	params := openai.ChatCompletionNewParams{
		Model: m.model, MaxTokens: openai.Int(int64(r.MaxOutput)),
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	}
	if m.reasoning {
		params.ReasoningEffort = shared.ReasoningEffortLow
	}
	for _, t := range r.Tools {
		schema, err := schemaOf(t)
		if err != nil {
			return params, err
		}
		params.Tools = append(params.Tools, openai.ChatCompletionToolUnionParam{OfFunction: &openai.ChatCompletionFunctionToolParam{
			Function: shared.FunctionDefinitionParam{Name: t.Name, Description: openai.String(t.Description), Parameters: schema},
		}})
	}
	messages := []openai.ChatCompletionMessageParamUnion{{OfSystem: &openai.ChatCompletionSystemMessageParam{
		Content: openai.ChatCompletionSystemMessageParamContentUnion{OfString: openai.String(r.System)},
	}}}
	for _, msg := range r.Messages {
		var texts []string
		var calls []openai.ChatCompletionMessageToolCallUnionParam
		for _, b := range msg.Content {
			switch b := b.(type) {
			case Text:
				texts = append(texts, b.Text)
			case ToolUse:
				calls = append(calls, openai.ChatCompletionMessageToolCallUnionParam{OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
					ID: b.ID, Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{Name: b.Name, Arguments: string(b.Input)},
				}})
			case ToolResult:
				content := b.Content
				if b.IsError {
					// The wire has no error flag: the result says so.
					content = "Error: " + content
				}
				messages = append(messages, openai.ChatCompletionMessageParamUnion{OfTool: &openai.ChatCompletionToolMessageParam{
					ToolCallID: b.ToolUseID, Content: openai.ChatCompletionToolMessageParamContentUnion{OfString: openai.String(content)},
				}})
			case Reasoning:
			default:
				return params, errors.New("llm: unknown block")
			}
		}
		switch {
		case msg.Role == Assistant && (len(texts) > 0 || len(calls) > 0):
			out := &openai.ChatCompletionAssistantMessageParam{ToolCalls: calls}
			if len(texts) > 0 {
				out.Content = openai.ChatCompletionAssistantMessageParamContentUnion{OfString: openai.String(strings.Join(texts, "\n\n"))}
			}
			messages = append(messages, openai.ChatCompletionMessageParamUnion{OfAssistant: out})
		case msg.Role == User && len(texts) > 0:
			messages = append(messages, openai.ChatCompletionMessageParamUnion{OfUser: &openai.ChatCompletionUserMessageParam{
				Content: openai.ChatCompletionUserMessageParamContentUnion{OfString: openai.String(strings.Join(texts, "\n\n"))},
			}})
		}
	}
	params.Messages = messages
	return params, nil
}
