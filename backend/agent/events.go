package agent

import (
	"bytes"
	"encoding/json"
	"time"
)

// Event is one thing a turn shows its person, stored as JSON with its type:
// a closed set, which the site reads.
type Event interface{ eventType() string }

// Started opens an attempt: the model it runs on.
type Started struct {
	Model   string `json:"model"`
	Attempt int    `json:"attempt"`
}

// Said is text the model wrote, as it is: the site renders it as plain text.
type Said struct {
	Text string `json:"text"`
}

// ToolCalled is a tool the model called, with its input as the model wrote it.
type ToolCalled struct {
	ToolUseID string          `json:"tool_use_id"`
	Name      string          `json:"name"`
	Title     string          `json:"title"`
	Input     json.RawMessage `json:"input"`
}

// ToolReturned is what the tool answered: its structured output, untrusted
// data, unless it failed (error) or was too large to show (truncated).
type ToolReturned struct {
	ToolUseID string `json:"tool_use_id"`
	Name      string `json:"name"`
	IsError   bool   `json:"is_error"`
	Error     string `json:"error,omitempty"`
	Output    any    `json:"output,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ActionCard is an action the agent prepared, for its person to run or
// dismiss: the action's own fields are read from the thread.
type ActionCard struct {
	ActionID          string          `json:"action_id"`
	Operation         string          `json:"operation"`
	Target            string          `json:"target"`
	Payload           json.RawMessage `json:"payload"`
	PayloadHash       string          `json:"payload_hash"`
	Display           json.RawMessage `json:"display"`
	TargetNamedByUser bool            `json:"target_named_by_user"`
	ExpiresAt         time.Time       `json:"expires_at"`
}

// ActionResult opens the turn the agent goes on from an action in: how the
// person's click ended.
type ActionResult struct {
	ActionID  string          `json:"action_id"`
	Operation string          `json:"operation"`
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// Finished closes an attempt that ended the turn: done, or stopped at one of
// its caps, or failed (error).
type Finished struct {
	Status string     `json:"status"`
	Reason StopReason `json:"reason"`
	Error  string     `json:"error,omitempty"`
}

// StopReason is why a turn ended: a closed set.
type StopReason string

const (
	// StopAnswered: the model finished.
	StopAnswered StopReason = "answered"
	// StopSteps, StopWallTime and StopBudget: the turn reached a cap.
	StopSteps    StopReason = "max_steps"
	StopWallTime StopReason = "wall_time"
	StopBudget   StopReason = "budget"
	// StopMaxTokens: the model's answer reached its length cap.
	StopMaxTokens StopReason = "max_tokens"
	// StopRefused: the model declined.
	StopRefused StopReason = "refusal"
	// StopContext: the transcript filled the model's window.
	StopContext StopReason = "context"
	// StopFailed: the turn failed; Error says why.
	StopFailed StopReason = "failed"
)

func (Started) eventType() string      { return "started" }
func (Said) eventType() string         { return "text" }
func (ToolCalled) eventType() string   { return "tool_call" }
func (ToolReturned) eventType() string { return "tool_result" }
func (ActionCard) eventType() string   { return "action_card" }
func (ActionResult) eventType() string { return "action_result" }
func (Finished) eventType() string     { return "finished" }

// MarshalEvent is e as a turn's event: its fields and its type.
func MarshalEvent(e Event) (json.RawMessage, error) {
	fields, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	// Numbers are kept as written: an id or a count past 2^53 stays exact.
	decoder := json.NewDecoder(bytes.NewReader(fields))
	decoder.UseNumber()
	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		return nil, err
	}
	out["type"] = e.eventType()
	return json.Marshal(out)
}
