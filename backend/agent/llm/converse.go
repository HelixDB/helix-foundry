package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

// converseModel is a model on Amazon Bedrock through the Converse API,
// streamed (converse-stream), as the engine calls it: Foundry stores a
// Bedrock API key (ABSK…), which bedrock-runtime takes as a bearer token in
// Authorization, so nothing is signed with SigV4 and no AWS credentials are
// read. Converse runs any model Bedrock serves, which settings may name, not
// only Claude. anthropic-sdk-go's bedrock package was not used: it calls
// Claude alone (InvokeModel), and its bearer token is replaced by the
// process's AWS_BEARER_TOKEN_BEDROCK whenever that is set, which would run a
// workspace's calls on another account's key; the AWS SDK's bedrockruntime
// client reads the same variable, and AWS_ENDPOINT_URL besides.
type converseModel struct {
	base    string
	key     string
	model   string
	timeout time.Duration
	client  *http.Client
}

func newConverse(base, key, model string, timeout time.Duration, client *http.Client) *converseModel {
	return &converseModel{base: base, key: key, model: model, timeout: timeout, client: client}
}

// converseURL is Bedrock's runtime in region: its FIPS endpoint when fips is
// set, and China's partition its own domain. The host is built from the
// region, never typed.
//
//	converseURL("us-east-1", false) // https://bedrock-runtime.us-east-1.amazonaws.com
func converseURL(region string, fips bool) string {
	host := "bedrock-runtime"
	if fips {
		host += "-fips"
	}
	domain := "amazonaws.com"
	if strings.HasPrefix(region, "cn-") {
		domain = "amazonaws.com.cn"
	}
	return fmt.Sprintf("https://%s.%s.%s", host, region, domain)
}

// family is the provider whose model id runs: the id's first part, past an
// inference profile's prefix and an ARN's path.
func family(model string) string {
	id := model[strings.LastIndex(model, "/")+1:]
	parts := strings.SplitN(id, ".", 3)
	switch {
	case len(parts) == 3 && strings.Contains(" global us us-gov eu apac au jp ca in ", " "+parts[0]+" "):
		return parts[1]
	default:
		return parts[0]
	}
}

func (m *converseModel) Stream(ctx context.Context, r Request) iter.Seq2[Chunk, error] {
	return func(yield func(Chunk, error) bool) {
		body, err := m.body(r)
		if err != nil {
			yield(nil, err)
			return
		}
		if m.timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, m.timeout)
			defer cancel()
		}
		auth := http.Header{"Authorization": {"Bearer " + m.key}, "Accept": {"application/vnd.amazon.eventstream"}}
		resp, err := post(ctx, m.client, m.base+"/model/"+segment(m.model)+"/converse-stream", auth, body, func(raw []byte) string {
			var e struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(raw, &e)
			return e.Message
		})
		if err != nil {
			yield(nil, err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		var text strings.Builder
		// The calls by content block, as their input's deltas build them.
		type call struct {
			id, name string
			input    strings.Builder
		}
		var calls []*call
		open := map[int]*call{}
		var usage *Usage
		stop := ""
		decoder := eventstream.NewDecoder()
		for {
			msg, err := decoder.Decode(resp.Body, nil)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				yield(nil, transportError(err))
				return
			}
			kind := header(msg, ":message-type")
			if kind == "exception" || kind == "error" {
				yield(nil, converseException(header(msg, ":exception-type")+header(msg, ":error-code"), msg.Payload))
				return
			}
			var e struct {
				ContentBlockIndex int `json:"contentBlockIndex"`
				Start             struct {
					ToolUse *struct {
						ToolUseID string `json:"toolUseId"`
						Name      string `json:"name"`
					} `json:"toolUse"`
				} `json:"start"`
				Delta struct {
					Text    string `json:"text"`
					ToolUse *struct {
						Input string `json:"input"`
					} `json:"toolUse"`
				} `json:"delta"`
				StopReason string `json:"stopReason"`
				Usage      *struct {
					InputTokens           int64 `json:"inputTokens"`
					OutputTokens          int64 `json:"outputTokens"`
					CacheReadInputTokens  int64 `json:"cacheReadInputTokens"`
					CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(msg.Payload, &e); err != nil {
				yield(nil, &Error{Kind: ErrUnavailable, Err: err})
				return
			}
			switch header(msg, ":event-type") {
			case "contentBlockStart":
				if t := e.Start.ToolUse; t != nil {
					c := &call{id: t.ToolUseID, name: t.Name}
					calls = append(calls, c)
					open[e.ContentBlockIndex] = c
				}
			case "contentBlockDelta":
				switch {
				case e.Delta.ToolUse != nil:
					if c := open[e.ContentBlockIndex]; c != nil {
						c.input.WriteString(e.Delta.ToolUse.Input)
					}
				case e.Delta.Text != "":
					text.WriteString(e.Delta.Text)
					if !yield(TextDelta{Text: e.Delta.Text}, nil) {
						return
					}
				}
			case "messageStop":
				stop = e.StopReason
			case "metadata":
				if u := e.Usage; u != nil {
					usage = &Usage{In: u.InputTokens, Out: u.OutputTokens, CacheRead: u.CacheReadInputTokens, CacheWrite: u.CacheWriteInputTokens}
				}
			}
		}
		if stop == "" || usage == nil {
			if usage != nil && !yield(*usage, nil) {
				return
			}
			yield(nil, &Error{Kind: ErrUnavailable, Err: errBroken})
			return
		}
		if text.Len() > 0 && !yield(Text{Text: text.String()}, nil) {
			return
		}
		for _, c := range calls {
			input := json.RawMessage(c.input.String())
			if strings.TrimSpace(c.input.String()) == "" {
				input = json.RawMessage(`{}`)
			}
			if !yield(ToolUse{ID: callID(c.id), Name: c.name, Input: input}, nil) {
				return
			}
		}
		if !yield(*usage, nil) {
			return
		}
		reason := StopEnd
		switch {
		case len(calls) > 0 || stop == "tool_use":
			reason = StopToolUse
		case stop == "max_tokens":
			reason = StopMaxTokens
		case stop == "content_filtered" || stop == "guardrail_intervened":
			reason = StopRefusal
		case stop == "model_context_window_exceeded":
			reason = StopContext
		}
		yield(Stop{Reason: reason}, nil)
	}
}

// segment is a model's id as one path segment: everything but letters,
// digits, -, ., _ and ~ escaped, so an inference profile's ARN stays one.
func segment(model string) string {
	return strings.ReplaceAll(url.QueryEscape(model), "+", "%20")
}

func header(msg eventstream.Message, name string) string {
	if v := msg.Headers.Get(name); v != nil {
		return v.String()
	}
	return ""
}

// converseException is an exception a stream carried, as an *Error.
func converseException(kind string, payload []byte) *Error {
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(payload, &e)
	err := fmt.Errorf("%s: %s", kind, e.Message)
	switch kind {
	case "throttlingException":
		return &Error{Kind: ErrRateLimited, Err: err}
	case "validationException":
		return &Error{Kind: ErrInvalid, Err: err}
	default:
		// internalServerException, modelStreamErrorException,
		// serviceUnavailableException: may pass on a retry.
		return &Error{Kind: ErrUnavailable, Err: err}
	}
}

// converseBlock is one content block of a Converse message.
type converseBlock map[string]any

// body is r as a Converse request. A Claude model gets cache points, as the
// engine sends them: after the system prompt, after the tools and at the
// transcript's end, so each step reads what the one before it wrote; no
// other model is known to take them. A tool's failed result carries
// status only to Claude and Amazon Nova, which take it; any other's says so
// in its text. Empty text blocks, which Converse refuses, are left out.
func (m *converseModel) body(r Request) ([]byte, error) {
	caches := family(m.model) == "anthropic"
	status := caches || family(m.model) == "amazon"
	point := converseBlock{"cachePoint": map[string]string{"type": "default"}}
	system := []converseBlock{{"text": r.System}}
	if caches {
		system = append(system, point)
	}
	body := map[string]any{"system": system, "inferenceConfig": map[string]int{"maxTokens": r.MaxOutput}}
	if len(r.Tools) > 0 {
		var tools []converseBlock
		for _, t := range r.Tools {
			schema, err := schemaOf(t)
			if err != nil {
				return nil, err
			}
			tools = append(tools, converseBlock{"toolSpec": map[string]any{"name": t.Name, "description": t.Description, "inputSchema": map[string]any{"json": schema}}})
		}
		if caches {
			tools = append(tools, point)
		}
		body["toolConfig"] = map[string]any{"tools": tools}
	}
	var messages []map[string]any
	for _, msg := range r.Messages {
		var content []converseBlock
		for _, b := range msg.Content {
			switch b := b.(type) {
			case Text:
				if strings.TrimSpace(b.Text) != "" {
					content = append(content, converseBlock{"text": b.Text})
				}
			case ToolUse:
				input := map[string]any{}
				if err := json.Unmarshal(b.Input, &input); err != nil || input == nil {
					input = map[string]any{}
				}
				content = append(content, converseBlock{"toolUse": map[string]any{"toolUseId": b.ID, "name": b.Name, "input": input}})
			case ToolResult:
				result := map[string]any{"toolUseId": b.ToolUseID, "content": []converseBlock{{"text": nonEmpty(b.Content)}}}
				switch {
				case b.IsError && status:
					result["status"] = "error"
				case b.IsError:
					result["content"] = []converseBlock{{"text": "Error: " + b.Content}}
				}
				content = append(content, converseBlock{"toolResult": result})
			case Reasoning:
			default:
				return nil, errors.New("llm: unknown block")
			}
		}
		if len(content) == 0 {
			continue
		}
		role := "user"
		if msg.Role == Assistant {
			role = "assistant"
		}
		messages = append(messages, map[string]any{"role": role, "content": content})
	}
	if caches && len(messages) > 0 {
		last := messages[len(messages)-1]
		last["content"] = append(last["content"].([]converseBlock), point)
	}
	body["messages"] = messages
	return json.Marshal(body)
}

// nonEmpty is a result's text, never empty, which Converse refuses.
func nonEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(empty)"
	}
	return s
}
