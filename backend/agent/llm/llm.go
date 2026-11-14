// Package llm is the Assistant's model calls: a provider-neutral request,
// transcript and stream of chunks, one implementation per provider kind, and
// the price of what a call used. The agent calls models here directly, never
// through the ontology engine.
//
// Layer 1 calls Claude (ai.Anthropic: the workspace's own key, or Helix's
// default). Every other kind is ErrUnsupported until its implementation
// lands; New's switch names each kind, so adding one is one case.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"

	"github.com/helixdb/helix-foundry/backend/ai"
)

// Model streams one call's answer.
type Model interface {
	// Stream yields the answer's chunks in order: its blocks (Text, ToolUse,
	// Reasoning), then its Usage, then its Stop. An error ends it, a *Error
	// for a provider's refusal.
	Stream(ctx context.Context, r Request) iter.Seq2[Chunk, error]
}

// Request is one call: the system prompt, the transcript, the tools the model
// may call and the most it may write.
type Request struct {
	System    string
	Messages  []Message
	Tools     []Tool
	MaxOutput int
}

// Tool is a tool the model may call, its input a JSON Schema object.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Role is whose a message is.
type Role string

const (
	User      Role = "user"
	Assistant Role = "assistant"
)

// Message is one message of a transcript.
type Message struct {
	Role    Role
	Content []Block
}

// Block is one part of a message: Text, ToolUse, ToolResult or Reasoning.
type Block interface{ block() }

// Chunk is one part of a streamed answer: a Block the model wrote (Text,
// ToolUse, Reasoning), then Usage and Stop.
type Chunk interface{ chunk() }

// Text is plain text.
type Text struct{ Text string }

// ToolUse is the model's call of a tool.
type ToolUse struct {
	ID, Name string
	Input    json.RawMessage
}

// ToolResult answers a ToolUse, in the next user message.
type ToolResult struct {
	ToolUseID string
	Content   string
	IsError   bool
}

// Reasoning is a provider's opaque record of the model's thinking, sent back
// unchanged to the provider that wrote it and dropped for any other:
// Anthropic's thinking (Signature, Text) or redacted thinking (Redacted).
type Reasoning struct {
	Provider  ai.Kind
	Signature string
	Text      string
	Redacted  string
}

// Usage is what a call used: input tokens read in full, from the prompt
// cache and written to it, and output tokens.
type Usage struct{ In, Out, CacheRead, CacheWrite int64 }

// StopReason is why the model stopped: a closed set.
type StopReason string

const (
	StopEnd       StopReason = "end"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
	// StopContext is a context window the transcript filled.
	StopContext StopReason = "context"
)

// Stop ends a stream.
type Stop struct{ Reason StopReason }

func (Text) block()       {}
func (ToolUse) block()    {}
func (ToolResult) block() {}
func (Reasoning) block()  {}
func (Text) chunk()       {}
func (ToolUse) chunk()    {}
func (Reasoning) chunk()  {}
func (Usage) chunk()      {}
func (Stop) chunk()       {}

// ErrorKind is how a provider refused a call: a closed set.
type ErrorKind string

const (
	// ErrAuth: the key is wrong, or may not use the model.
	ErrAuth ErrorKind = "auth"
	// ErrInvalid: the provider refused the request itself (a model it
	// doesn't have, a transcript too long).
	ErrInvalid ErrorKind = "invalid"
	// ErrRateLimited and ErrUnavailable may pass on a retry.
	ErrRateLimited ErrorKind = "rate_limited"
	ErrUnavailable ErrorKind = "unavailable"
)

// Error is a provider's refusal of a call.
type Error struct {
	Kind ErrorKind
	Err  error
}

func (e *Error) Error() string { return fmt.Sprintf("model call %s: %v", e.Kind, e.Err) }
func (e *Error) Unwrap() error { return e.Err }

// Retryable reports a refusal a later attempt may pass.
func (e *Error) Retryable() bool { return e.Kind == ErrRateLimited || e.Kind == ErrUnavailable }

// ErrUnsupported is a provider kind the Assistant doesn't call yet.
var ErrUnsupported = errors.New("llm: the Assistant runs on Claude for now; this provider isn't supported yet")

// New is the model m of r's provider, and its price.
func New(r ai.Resolved, m ai.Model, catalog *ai.Catalog) (Model, Pricer, error) {
	pricer, err := NewPricer(catalog, r.Settings.Provider.Kind(), m)
	if err != nil {
		return nil, Pricer{}, err
	}
	switch r.Settings.Provider.(type) {
	case ai.Anthropic:
		return newAnthropic(r.Key.Reveal(), m.Name, r.CallTimeout), pricer, nil
	case ai.OpenAI, ai.AzureOpenAI, ai.Compatible, ai.Ollama, ai.Bedrock:
		return nil, Pricer{}, fmt.Errorf("%w: %s", ErrUnsupported, r.Settings.Provider.Kind())
	default:
		return nil, Pricer{}, fmt.Errorf("llm: unknown provider %T", r.Settings.Provider)
	}
}

// blockJSON is a block as a transcript stores it.
type blockJSON struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Provider  ai.Kind         `json:"provider,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Redacted  string          `json:"redacted,omitempty"`
}

// MarshalBlocks is blocks as a transcript stores them.
func MarshalBlocks(blocks []Block) (json.RawMessage, error) {
	out := make([]blockJSON, 0, len(blocks))
	for _, b := range blocks {
		switch b := b.(type) {
		case Text:
			out = append(out, blockJSON{Type: "text", Text: b.Text})
		case ToolUse:
			out = append(out, blockJSON{Type: "tool_use", ID: b.ID, Name: b.Name, Input: b.Input})
		case ToolResult:
			out = append(out, blockJSON{Type: "tool_result", ToolUseID: b.ToolUseID, Content: b.Content, IsError: b.IsError})
		case Reasoning:
			out = append(out, blockJSON{Type: "reasoning", Provider: b.Provider, Signature: b.Signature, Text: b.Text, Redacted: b.Redacted})
		default:
			return nil, fmt.Errorf("llm: unknown block %T", b)
		}
	}
	return json.Marshal(out)
}

// UnmarshalBlocks reads blocks MarshalBlocks wrote.
func UnmarshalBlocks(raw json.RawMessage) ([]Block, error) {
	var in []blockJSON
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("llm: read blocks: %w", err)
	}
	out := make([]Block, 0, len(in))
	for _, b := range in {
		switch b.Type {
		case "text":
			out = append(out, Text{Text: b.Text})
		case "tool_use":
			out = append(out, ToolUse{ID: b.ID, Name: b.Name, Input: b.Input})
		case "tool_result":
			out = append(out, ToolResult{ToolUseID: b.ToolUseID, Content: b.Content, IsError: b.IsError})
		case "reasoning":
			out = append(out, Reasoning{Provider: b.Provider, Signature: b.Signature, Text: b.Text, Redacted: b.Redacted})
		default:
			return nil, fmt.Errorf("llm: unknown block type %q", b.Type)
		}
	}
	return out, nil
}
