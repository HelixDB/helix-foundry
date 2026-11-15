package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/helixdb/helix-foundry/backend/ai"
)

// openAIBaseURL is OpenAI's API.
const openAIBaseURL = "https://api.openai.com/v1"

// responsesHost is who serves a Responses API: a closed set, which says how
// a call is authorized and whose reasoning it sends back.
type responsesHost int

const (
	// responsesOpenAI is OpenAI's API, with the key as a bearer token.
	responsesOpenAI responsesHost = iota
	// responsesAzure is an Azure OpenAI resource's v1 API, with the key in
	// the api-key header; a model is one of its deployments.
	responsesAzure
	// responsesCompatible is an OpenAI-compatible server, its key (if any)
	// as a bearer token.
	responsesCompatible
)

func (h responsesHost) kind() ai.Kind {
	switch h {
	case responsesAzure:
		return ai.KindAzureOpenAI
	case responsesCompatible:
		return ai.KindCompatible
	default:
		return ai.KindOpenAI
	}
}

// responsesModel is a model behind a Responses API, streamed, as the engine
// calls it: nothing stored at the provider (store false), each call keyed
// for its prompt cache, and with the reasoning knob, the model's encrypted
// reasoning asked for and sent back as it came.
type responsesModel struct {
	service   responses.ResponseService
	host      responsesHost
	model     string
	reasoning bool
	calls     *gate
	// writer names the model at its endpoint: only its own reasoning goes back.
	writer string
}

// newResponses is model at baseURL under key, through client. It is built
// from the Responses service alone, never openai.NewClient, which would read
// OPENAI_* from the process's environment into every call. opts follow, for
// tests.
func newResponses(host responsesHost, baseURL, key, model string, reasoning bool, timeout time.Duration, client *http.Client, calls *gate,
	opts ...option.RequestOption) *responsesModel {
	base := []option.RequestOption{option.WithBaseURL(baseURL), option.WithHTTPClient(client), option.WithMaxRetries(2)}
	switch {
	case host == responsesAzure:
		base = append(base, option.WithHeader("api-key", key))
	case key != "":
		base = append(base, option.WithAPIKey(key))
	}
	if timeout > 0 {
		base = append(base, option.WithRequestTimeout(timeout))
	}
	return &responsesModel{service: responses.NewResponseService(append(base, opts...)...), host: host, model: model, reasoning: reasoning, calls: calls,
		writer: writerOf(host.kind(), baseURL, model)}
}

// responsesEvent is what the Assistant reads of a stream's event; the rest
// of it (annotations, logprobs, other tools' items) it never uses.
type responsesEvent struct {
	Type    string `json:"type"`
	Delta   string `json:"delta"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Item    struct {
		Type             string `json:"type"`
		ID               string `json:"id"`
		CallID           string `json:"call_id"`
		Name             string `json:"name"`
		Arguments        string `json:"arguments"`
		EncryptedContent string `json:"encrypted_content"`
		Content          []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"item"`
	Response struct {
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Usage *struct {
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	} `json:"response"`
}

func (m *responsesModel) Stream(ctx context.Context, r Request) iter.Seq2[Chunk, error] {
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
		var blocks []Chunk
		var usage Usage
		stop, ended := StopEnd, false
		for stream.Next() {
			var e responsesEvent
			if err := json.Unmarshal([]byte(stream.Current().RawJSON()), &e); err != nil {
				yield(nil, &Error{Kind: ErrUnavailable, Err: err})
				return
			}
			switch e.Type {
			case "response.output_text.delta":
				if e.Delta != "" && !yield(TextDelta{Text: e.Delta}, nil) {
					return
				}
			case "response.output_item.done":
				switch it := e.Item; it.Type {
				case "message":
					var text strings.Builder
					for _, part := range it.Content {
						switch part.Type {
						case "output_text":
							text.WriteString(part.Text)
						case "refusal":
							stop = StopRefusal
						}
					}
					if text.Len() > 0 {
						blocks = append(blocks, Text{Text: text.String()})
					}
				case "function_call":
					input := json.RawMessage(it.Arguments)
					if strings.TrimSpace(it.Arguments) == "" {
						input = json.RawMessage(`{}`)
					}
					blocks = append(blocks, ToolUse{ID: callID(it.CallID), Name: it.Name, Input: input})
				case "reasoning":
					// Without its encrypted content, a reasoning item can't be sent back.
					if it.EncryptedContent != "" {
						blocks = append(blocks, Reasoning{Writer: m.writer, Signature: it.ID, Redacted: it.EncryptedContent})
					}
				}
			case "response.completed", "response.incomplete":
				ended = true
				if u := e.Response.Usage; u != nil {
					cached := min(u.InputTokensDetails.CachedTokens, u.InputTokens)
					usage = Usage{In: u.InputTokens - cached, Out: u.OutputTokens, CacheRead: cached}
				}
				if d := e.Response.IncompleteDetails; d != nil {
					switch d.Reason {
					case "max_output_tokens":
						stop = StopMaxTokens
					case "content_filter":
						stop = StopRefusal
					}
				}
			case "response.failed":
				code, message := "", "the response failed"
				if f := e.Response.Error; f != nil {
					code, message = f.Code, f.Message
				}
				yield(nil, responsesFailure(code, message))
				return
			case "error":
				yield(nil, responsesFailure(e.Code, e.Message))
				return
			}
		}
		if err := stream.Err(); err != nil {
			yield(nil, openAIError(err))
			return
		}
		if !ended {
			yield(nil, &Error{Kind: ErrUnavailable, Err: errors.New("the stream ended before its response did")})
			return
		}
		for _, b := range blocks {
			if _, ok := b.(ToolUse); ok && stop == StopEnd {
				stop = StopToolUse
			}
			if !yield(b, nil) {
				return
			}
		}
		if !yield(usage, nil) {
			return
		}
		yield(Stop{Reason: stop}, nil)
	}
}

// params is r as a Responses request.
func (m *responsesModel) params(r Request) (responses.ResponseNewParams, error) {
	params := responses.ResponseNewParams{
		Model: m.model, Instructions: openai.String(r.System), Store: openai.Bool(false),
		MaxOutputTokens: openai.Int(int64(r.MaxOutput)),
	}
	// Calls that share a prefix share a key, so the provider routes them to
	// the same cache: the system prompt names the workspace, so a
	// workspace's turns share one.
	key := sha256.New()
	key.Write([]byte(r.System))
	for _, t := range r.Tools {
		schema, err := schemaOf(t)
		if err != nil {
			return params, err
		}
		params.Tools = append(params.Tools, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
			Name: t.Name, Description: openai.String(t.Description), Parameters: schema, Strict: openai.Bool(false),
		}})
		key.Write([]byte("\x00" + t.Name))
	}
	params.PromptCacheKey = openai.String(hex.EncodeToString(key.Sum(nil))[:32])
	if m.reasoning {
		params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffortLow}
		params.Include = []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent}
	}
	var items responses.ResponseInputParam
	text := func(role responses.EasyInputMessageRole, s string) responses.ResponseInputItemUnionParam {
		return responses.ResponseInputItemUnionParam{OfMessage: &responses.EasyInputMessageParam{
			Role: role, Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(s)},
		}}
	}
	for _, msg := range r.Messages {
		for _, b := range msg.Content {
			switch b := b.(type) {
			case Text:
				role := responses.EasyInputMessageRoleUser
				if msg.Role == Assistant {
					role = responses.EasyInputMessageRoleAssistant
				}
				items = append(items, text(role, b.Text))
			case ToolUse:
				items = append(items, responses.ResponseInputItemUnionParam{OfFunctionCall: &responses.ResponseFunctionToolCallParam{
					CallID: b.ID, Name: b.Name, Arguments: string(b.Input),
				}})
			case ToolResult:
				output := b.Content
				if b.IsError {
					// The wire has no error flag: the result says so.
					output = "Error: " + output
				}
				items = append(items, responses.ResponseInputItemUnionParam{OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: b.ToolUseID, Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfString: openai.String(output)},
				}})
			case Reasoning:
				// Only the model that wrote it, at the same endpoint, reads
				// it, and only with the reasoning knob it was written with.
				if m.reasoning && b.Writer == m.writer && b.Redacted != "" {
					items = append(items, responses.ResponseInputItemUnionParam{OfReasoning: &responses.ResponseReasoningItemParam{
						ID: b.Signature, EncryptedContent: openai.String(b.Redacted), Summary: []responses.ResponseReasoningItemSummaryParam{},
					}})
				}
			default:
				return params, errors.New("llm: unknown block")
			}
		}
	}
	params.Input = responses.ResponseNewParamsInputUnion{OfInputItemList: items}
	return params, nil
}

// responsesFailure is a stream's own failure event as an *Error, by its code.
func responsesFailure(code, message string) *Error {
	err := errors.New(code + ": " + message)
	switch code {
	case "rate_limit_exceeded", "insufficient_quota":
		return &Error{Kind: ErrRateLimited, Err: err}
	case "server_error", "", "server_is_overloaded", "slow_down":
		return &Error{Kind: ErrUnavailable, Err: err}
	default:
		return &Error{Kind: ErrInvalid, Err: err}
	}
}

// openAIError is a failed call of openai-go as an *Error, by its status.
func openAIError(err error) *Error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		return statusError(apiErr.StatusCode, err)
	}
	return transportError(err)
}
