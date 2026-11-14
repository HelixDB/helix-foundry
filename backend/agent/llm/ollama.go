package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"iter"
	"net/http"
	"strings"
	"time"
)

// ollamaKeepAlive is how long Ollama keeps the model loaded after a call, as
// the engine asks: a turn's next step finds it there.
const ollamaKeepAlive = "10m"

// ollamaLineBytes is the longest line of a stream read: one chunk, which
// holds at most a few tokens or one tool call.
const ollamaLineBytes = 4 << 20

// ollamaModel is a model of an Ollama server through its native /api/chat,
// streamed, as the engine calls it. The native API is the one that sets the
// window: options.num_ctx loads the model with the setting's window, so the
// transcript the Assistant fits to it is read whole. (Ollama's OpenAI
// /v1 takes no num_ctx: the model would load in the server's default, often
// 4096 or 8192 tokens, and the prompt past it be cut silently.) The window
// never changes between calls, so the server keeps the model loaded and
// reuses its cache of the prompt's prefix. It never thinks (think false):
// thinking would cost a local model's whole budget, and is never sent back.
// The server takes no key, and one call at a time.
type ollamaModel struct {
	url     string
	model   string
	window  int
	timeout time.Duration
	client  *http.Client
	calls   gate
}

func newOllama(baseURL, model string, window int, timeout time.Duration, client *http.Client, calls gate) *ollamaModel {
	return &ollamaModel{url: strings.TrimSuffix(baseURL, "/") + "/api/chat", model: model, window: window, timeout: timeout, client: client, calls: calls}
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	// ToolName names the tool a result answers: the native API has no call ids.
	ToolName string `json:"tool_name,omitempty"`
}

type ollamaToolCall struct {
	ID       string `json:"id,omitempty"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type ollamaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type ollamaRequest struct {
	Model     string          `json:"model"`
	Messages  []ollamaMessage `json:"messages"`
	Tools     []ollamaTool    `json:"tools,omitempty"`
	Stream    bool            `json:"stream"`
	Think     bool            `json:"think"`
	KeepAlive string          `json:"keep_alive"`
	Options   struct {
		NumCtx     int `json:"num_ctx"`
		NumPredict int `json:"num_predict"`
	} `json:"options"`
}

// ollamaChunk is one line of a stream: some of the message, or, done, why
// it stopped and what it used; or the error that ended it.
type ollamaChunk struct {
	Message struct {
		Content   string           `json:"content"`
		ToolCalls []ollamaToolCall `json:"tool_calls"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int64  `json:"prompt_eval_count"`
	EvalCount       int64  `json:"eval_count"`
	Error           string `json:"error"`
}

func (m *ollamaModel) Stream(ctx context.Context, r Request) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		body, err := m.body(r)
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
		if m.timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, m.timeout)
			defer cancel()
		}
		resp, err := post(ctx, m.client, m.url, http.Header{}, body, func(raw []byte) string {
			var e struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(raw, &e)
			return e.Error
		})
		if err != nil {
			yield(nil, err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		lines := bufio.NewScanner(resp.Body)
		lines.Buffer(make([]byte, 0, 64<<10), ollamaLineBytes)
		var text strings.Builder
		var calls []ToolUse
		var last *ollamaChunk
		for lines.Scan() {
			if len(strings.TrimSpace(lines.Text())) == 0 {
				continue
			}
			var c ollamaChunk
			if err := json.Unmarshal(lines.Bytes(), &c); err != nil {
				yield(nil, &Error{Kind: ErrUnavailable, Err: err})
				return
			}
			if c.Error != "" {
				yield(nil, &Error{Kind: ErrUnavailable, Err: errors.New(c.Error)})
				return
			}
			if d := c.Message.Content; d != "" {
				text.WriteString(d)
				if !yield(TextDelta{Text: d}, nil) {
					return
				}
			}
			for _, tc := range c.Message.ToolCalls {
				input := tc.Function.Arguments
				if len(input) == 0 || string(input) == "null" {
					input = json.RawMessage(`{}`)
				}
				calls = append(calls, ToolUse{ID: callID(tc.ID), Name: tc.Function.Name, Input: input})
			}
			if c.Done {
				last = &c
				break
			}
		}
		if err := lines.Err(); err != nil {
			yield(nil, transportError(err))
			return
		}
		if last == nil {
			yield(nil, &Error{Kind: ErrUnavailable, Err: errBroken})
			return
		}
		if text.Len() > 0 && !yield(Text{Text: text.String()}, nil) {
			return
		}
		for _, c := range calls {
			if !yield(c, nil) {
				return
			}
		}
		if !yield(Usage{In: last.PromptEvalCount, Out: last.EvalCount}, nil) {
			return
		}
		stop := StopEnd
		switch {
		case len(calls) > 0:
			stop = StopToolUse
		case last.DoneReason == "length":
			stop = StopMaxTokens
		}
		yield(Stop{Reason: stop}, nil)
	}
}

// body is r as a native chat request in the setting's window.
func (m *ollamaModel) body(r Request) ([]byte, error) {
	req := ollamaRequest{Model: m.model, Stream: true, KeepAlive: ollamaKeepAlive}
	req.Options.NumCtx, req.Options.NumPredict = m.window, r.MaxOutput
	for _, t := range r.Tools {
		schema, err := schemaOf(t)
		if err != nil {
			return nil, err
		}
		tool := ollamaTool{Type: "function"}
		tool.Function.Name, tool.Function.Description, tool.Function.Parameters = t.Name, t.Description, schema
		req.Tools = append(req.Tools, tool)
	}
	names := toolNames(r.Messages)
	req.Messages = append(req.Messages, ollamaMessage{Role: "system", Content: r.System})
	for _, msg := range r.Messages {
		var texts []string
		var calls []ollamaToolCall
		for _, b := range msg.Content {
			switch b := b.(type) {
			case Text:
				texts = append(texts, b.Text)
			case ToolUse:
				var call ollamaToolCall
				call.ID, call.Function.Name, call.Function.Arguments = b.ID, b.Name, b.Input
				calls = append(calls, call)
			case ToolResult:
				content := b.Content
				if b.IsError {
					content = "Error: " + content
				}
				req.Messages = append(req.Messages, ollamaMessage{Role: "tool", Content: content, ToolName: names[b.ToolUseID]})
			case Reasoning:
			default:
				return nil, errors.New("llm: unknown block")
			}
		}
		switch {
		case msg.Role == Assistant && (len(texts) > 0 || len(calls) > 0):
			req.Messages = append(req.Messages, ollamaMessage{Role: "assistant", Content: strings.Join(texts, "\n\n"), ToolCalls: calls})
		case msg.Role == User && len(texts) > 0:
			req.Messages = append(req.Messages, ollamaMessage{Role: "user", Content: strings.Join(texts, "\n\n")})
		}
	}
	return json.Marshal(req)
}
