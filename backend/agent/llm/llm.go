// Package llm is the Assistant's model calls: a provider-neutral request,
// transcript and stream of chunks, one implementation per provider kind, and
// the price of what a call used. The agent calls models here directly, never
// through the ontology engine, but on the wire the engine uses for each kind,
// so a setting its probe passed answers here too:
//
//   - ai.Anthropic: the Messages API (anthropic-sdk-go), Helix's Claude or
//     the workspace's key (anthropic.go);
//   - ai.OpenAI and ai.AzureOpenAI: the Responses API (openai-go), Azure on
//     its resource's v1 API with the api-key header (responses.go);
//   - ai.Compatible: Chat Completions (chat.go) or Responses, as its owner
//     chose, at most the setting's MaxCallsInFlight calls at once;
//   - ai.Cohere: Chat Completions (chat.go) at Cohere's Compatibility API,
//     never with the reasoning knob, as the engine calls it;
//   - ai.Ollama: the native /api/chat (ollama.go), whose options set the
//     context window: Ollama's OpenAI-compatible /v1 has no way to, and
//     would load the model in the server's default window and cut the
//     prompt past it without saying so;
//   - ai.Bedrock: the Converse API with the Bedrock API key as a bearer
//     token, no SigV4 (converse.go), for any model Bedrock serves.
//
// Every call dials through the deployment's guard (ai.Rules): hosted, only
// public addresses, whatever an owner's host resolves to when it is called.
// No client reads the process's environment (ANTHROPIC_*, OPENAI_*, AWS_*):
// the key, endpoint and headers are the workspace's alone.
package llm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"sync"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
)

// Model streams one call's answer.
type Model interface {
	// Stream yields the answer's chunks in order: TextDeltas as the model
	// writes, then its blocks (Text, ToolUse, Reasoning) whole, then its
	// Usage, then its Stop. The deltas are for show: the blocks are the
	// answer, and a Text block holds every delta before it. An error ends
	// it, a *Error for a provider's refusal.
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

// Chunk is one part of a streamed answer: TextDeltas as they come, a Block
// the model wrote (Text, ToolUse, Reasoning), then Usage and Stop.
type Chunk interface{ chunk() }

// Text is plain text.
type Text struct{ Text string }

// TextDelta is text the model has just written, before its block ends.
type TextDelta struct{ Text string }

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
// unchanged to the model that wrote it (Writer, writerOf) and dropped for
// any other, another endpoint or model of the same kind too: Anthropic's
// thinking (Signature, Text) or redacted thinking (Redacted), or a
// Responses API's encrypted reasoning item (Signature its id, Redacted).
type Reasoning struct {
	Writer    string
	Signature string
	Text      string
	Redacted  string
}

// writerOf names the model of kind at endpoint, as Reasoning.Writer: a
// digest, so a transcript keeps no endpoint.
func writerOf(kind ai.Kind, endpoint, model string) string {
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + endpoint + "\x00" + model))
	return string(kind) + ":" + hex.EncodeToString(sum[:8])
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
func (TextDelta) chunk()  {}
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

// Binding is the model a turn calls: how to call it, its price, and its
// context window in tokens (0 where the provider says nothing of one, so
// the Assistant's own cap applies).
type Binding struct {
	Model  Model
	Pricer Pricer
	Window int
}

// New is r's ask model, the one the Assistant runs on, bound to r's
// provider.
func New(r ai.Resolved, catalog *ai.Catalog) (Binding, error) { return NewFor(r, catalog, ai.RoleAsk) }

// NewFor is r's model for role, bound to r's provider: an exhaustive switch
// over the kinds, so a kind added to ai fails to build here until it is
// called. The ask model is sent its reasoning knob unless the setting's
// probe found the model refuses it; any other role's runs with reasoning
// off, as titles do: short structured work. Ollama loads the model in the
// role's window.
func NewFor(r ai.Resolved, catalog *ai.Catalog, role ai.Role) (Binding, error) {
	var spec ontologyagent.ModelSpec
	switch role {
	case ai.RoleAsk:
		spec = r.Ask().Ask
	case ai.RoleDraft:
		spec = r.Turn().Draft
	case ai.RoleExtract:
		spec = r.Extract().Extract
	case ai.RoleVerify:
		spec = r.Extract().Verify
	default:
		return Binding{}, fmt.Errorf("llm: unknown role %q", role)
	}
	var m ai.Model
	for _, rm := range r.Settings.Models.Each() {
		if rm.Role == role {
			m = rm.Model
		}
	}
	pricer, err := NewPricer(catalog, r.Settings.Provider.Kind(), m)
	if err != nil {
		return Binding{}, err
	}
	key, client := r.Key.Reveal(), guarded(r.Rules)
	reasoning := role == ai.RoleAsk && r.Report.Reasoning(ai.RoleAsk)
	out := Binding{Pricer: pricer}
	switch p := r.Settings.Provider.(type) {
	case ai.Anthropic:
		out.Model = newAnthropic(key, m.Name, r.CallTimeout, client)
	case ai.OpenAI:
		out.Model = newResponses(responsesOpenAI, openAIBaseURL, key, m.Name, reasoning, r.CallTimeout, client, nil)
	case ai.AzureOpenAI:
		endpoint, err := ai.AzureEndpoint(p.Endpoint)
		if err != nil {
			return Binding{}, err
		}
		out.Model = newResponses(responsesAzure, endpoint, key, m.Name, reasoning, r.CallTimeout, client, nil)
	case ai.Compatible:
		calls := gateOf(r.ConfigID, p.BaseURL, int(p.MaxCallsInFlight))
		switch p.API {
		case ai.CompatChatCompletions:
			out.Model = newChat(p.BaseURL, key, m.Name, reasoning, r.CallTimeout, client, calls)
		case ai.CompatResponses:
			out.Model = newResponses(responsesCompatible, p.BaseURL, key, m.Name, reasoning, r.CallTimeout, client, calls)
		default:
			return Binding{}, fmt.Errorf("llm: unknown API %q of an OpenAI-compatible server", p.API)
		}
	case ai.Cohere:
		// No reasoning knob: Cohere takes reasoning_effort only as none or
		// high, and the engine never sends it there (ai.Settings.spec).
		out.Model = newChat(ai.CohereBaseURL, key, m.Name, false, r.CallTimeout, client, nil)
	case ai.Ollama:
		// The window the engine loads the model with: the owner's, at most
		// what the probe read of the model.
		out.Window = int(spec.ContextTokens)
		out.Model = newOllama(p.BaseURL, m.Name, out.Window, r.CallTimeout, client, gateOf(r.ConfigID, p.BaseURL, 1))
	case ai.Bedrock:
		out.Model = newConverse(converseURL(p.Region, p.FIPS), key, m.Name, r.CallTimeout, client)
	default:
		return Binding{}, fmt.Errorf("llm: unknown provider %T", r.Settings.Provider)
	}
	return out, nil
}

// guarded is an HTTP client that dials only the addresses rules allow and
// follows no redirect, so neither a host's later DNS answer nor a provider's
// answer can turn a call inward. It sets no timeout: a call's own bounds
// its stream.
func guarded(rules ai.Rules) *http.Client {
	return &http.Client{
		Transport:     connector.Guard{AllowPrivate: rules.AllowPrivate}.Transport(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// gate bounds the calls in flight to one endpoint of one setting from this
// process, as its owner set (an OpenAI-compatible server's
// MaxCallsInFlight; Ollama's one): a call holds a slot for its whole
// stream. The bound is the setting's, so another workspace naming the same
// server has slots of its own. nil bounds nothing.
type gate struct {
	key string
	n   int
}

// gateOf is the gate of endpoint under the setting configID, n calls at once.
func gateOf(configID, endpoint string, n int) *gate {
	return &gate{key: fmt.Sprintf("%s\x00%s\x00%d", configID, endpoint, n), n: max(n, 1)}
}

// slots are the gates' slots by key, each kept while a call holds or waits
// for one of them: as many as the calls in flight, whatever servers owners
// name.
var (
	slotsMu sync.Mutex
	slots   = map[string]*gateSlots{}
)

type gateSlots struct {
	held  chan struct{}
	users int
}

// enter waits for a slot: the release, or ctx's end as an unavailable call.
func (g *gate) enter(ctx context.Context) (func(), error) {
	if g == nil {
		return func() {}, nil
	}
	slotsMu.Lock()
	s := slots[g.key]
	if s == nil {
		s = &gateSlots{held: make(chan struct{}, g.n)}
		slots[g.key] = s
	}
	s.users++
	slotsMu.Unlock()
	leave := func() {
		slotsMu.Lock()
		defer slotsMu.Unlock()
		if s.users--; s.users == 0 {
			delete(slots, g.key)
		}
	}
	select {
	case s.held <- struct{}{}:
		return func() { <-s.held; leave() }, nil
	case <-ctx.Done():
		leave()
		return nil, &Error{Kind: ErrUnavailable, Err: fmt.Errorf("waiting for a free call to the server: %w", ctx.Err())}
	}
}

// statusError is a provider's HTTP refusal as an *Error: whether a retry may
// pass by its status. Under 300 is an error a stream carried after it was
// accepted (overloaded, say), which may; a redirect, which no call follows,
// is the endpoint's to fix.
func statusError(code int, err error) *Error {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return &Error{Kind: ErrAuth, Err: err}
	case code == http.StatusTooManyRequests:
		return &Error{Kind: ErrRateLimited, Err: err}
	case code >= 500 || code == http.StatusRequestTimeout || code < http.StatusMultipleChoices:
		return &Error{Kind: ErrUnavailable, Err: err}
	default:
		return &Error{Kind: ErrInvalid, Err: err}
	}
}

// transportError is a call that got no answer: an endpoint the guard
// refuses is the setting's to fix, anything else may pass on a retry.
func transportError(err error) *Error {
	if errors.Is(err, connector.ErrPrivateAddress) {
		return &Error{Kind: ErrInvalid, Err: err}
	}
	return &Error{Kind: ErrUnavailable, Err: err}
}

// callID is a tool call's id as the model gave it, or a new one where it gave
// none (Ollama's older servers): ids key a turn's events and its asks'
// client tokens, so each must be unique in its thread.
func callID(id string) string {
	if id != "" {
		return id
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b) // never fails
	return "call_" + hex.EncodeToString(b)
}

// schemaOf is a tool's input schema as an object, for the wires that take
// one; an empty one is an object with no properties.
func schemaOf(t Tool) (map[string]any, error) {
	schema := map[string]any{}
	if len(t.InputSchema) > 0 {
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
			return nil, fmt.Errorf("llm: tool %s's schema: %w", t.Name, err)
		}
	}
	if schema == nil {
		schema = map[string]any{}
	}
	delete(schema, "$schema")
	delete(schema, "$id")
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	return schema, nil
}

// toolNames are the names of the calls in messages, by id: a wire that
// answers a call by its tool's name reads them here.
func toolNames(messages []Message) map[string]string {
	out := map[string]string{}
	for _, m := range messages {
		for _, b := range m.Content {
			if u, ok := b.(ToolUse); ok {
				out[u.ID] = u.Name
			}
		}
	}
	return out
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
	Writer    string          `json:"writer,omitempty"`
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
			out = append(out, blockJSON{Type: "reasoning", Writer: b.Writer, Signature: b.Signature, Text: b.Text, Redacted: b.Redacted})
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
			out = append(out, Reasoning{Writer: b.Writer, Signature: b.Signature, Text: b.Text, Redacted: b.Redacted})
		default:
			return nil, fmt.Errorf("llm: unknown block type %q", b.Type)
		}
	}
	return out, nil
}
