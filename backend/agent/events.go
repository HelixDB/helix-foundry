package agent

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/helixdb/helix-foundry/backend/agentquestion"
)

// Event is one thing a turn shows its person, stored as JSON with its type:
// a closed set, which the site reads.
type Event interface{ eventType() string }

// Started opens an attempt: the model it runs on.
type Started struct {
	Model   string `json:"model"`
	Attempt int    `json:"attempt"`
}

// Said is text the model wrote, as it is, once its step's call ended: the
// site renders it as plain text, in place of the step's TextDeltas.
type Said struct {
	Step int    `json:"step"`
	Text string `json:"text"`
}

// TextDelta is text the model is writing in step (a turn's model calls
// count from 1, across its attempts), shown as it comes: the site appends a
// step's deltas in order. The step's Said events supersede them once its
// call ends (the store then drops them), and the next Started or Finished
// event, when it never does: the transcript, not the deltas, is what was said.
type TextDelta struct {
	Step int    `json:"step"`
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
// data, unless it failed (error) or was too large to show (truncated). A
// call that never ran says why as not_run, which the site words for the
// person (the model is told in words of its own), with no error.
type ToolReturned struct {
	ToolUseID string       `json:"tool_use_id"`
	Name      string       `json:"name"`
	IsError   bool         `json:"is_error"`
	Error     string       `json:"error,omitempty"`
	Output    any          `json:"output,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
	NotRun    NotRunReason `json:"not_run,omitempty"`
}

// NotRunReason is why a call the model wrote never ran: a closed set.
type NotRunReason string

const (
	// NotRunCalls is a reply's call past its first maxCallsPerReply.
	NotRunCalls NotRunReason = "calls"
	// NotRunAsks is an ask past a turn's maxAsksPerTurn.
	NotRunAsks NotRunReason = "asks"
	// NotRunTime is a call after the turn's wall time ran out.
	NotRunTime NotRunReason = "time"
	// NotRunLength is a call the reply was writing when it reached its length limit.
	NotRunLength NotRunReason = "length"
	// NotRunInput is a call whose input isn't JSON.
	NotRunInput NotRunReason = "input"
	// NotRunAsked is a call after the turn asked its person: it ends there.
	NotRunAsked NotRunReason = "asked"
)

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

// Asked shows the person the questions the turn asks them
// (toolAskPerson), which it ends waiting on (StopAsked): the Assistant's
// own, or a generation's, as its agent wrote them (Generation set). They
// are the agents' words, which may quote the data: plain text.
type Asked struct {
	Questions  []agentquestion.Question `json:"questions"`
	Generation *AskedGeneration         `json:"generation,omitempty"`
}

// AskedGeneration is the generation whose questions a turn relays, by its
// names as the workspace has them, and the round of its questions.
type AskedGeneration struct {
	OntologyID      string `json:"ontology_id"`
	OntologyName    string `json:"ontology_name"`
	GenerationID    string `json:"generation_id"`
	GenerationTitle string `json:"generation_title,omitempty"`
	Round           int    `json:"round"`
}

// Answered opens an answers turn: the person's answers to the questions
// turn TurnID asked, as checked (the labels of the options chosen, and their
// own words); a question left out was left to the agent.
type Answered struct {
	TurnID  string                 `json:"turn_id"`
	Answers []agentquestion.Answer `json:"answers"`
}

// Reported is an automation's step's report (toolReport): what it found and
// did, for its owner, and the answer or count a condition after it checks.
type Reported struct {
	Summary string `json:"summary"`
	Answer  *bool  `json:"answer,omitempty"`
	Count   *int64 `json:"count,omitempty"`
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
	// StopAsked: the turn asked its person questions (Asked) and waits for
	// their answers, which come as the thread's next turn.
	StopAsked StopReason = "asked"
)

func (Started) eventType() string      { return "started" }
func (Said) eventType() string         { return "text" }
func (TextDelta) eventType() string    { return "text_delta" }
func (ToolCalled) eventType() string   { return "tool_call" }
func (ToolReturned) eventType() string { return "tool_result" }
func (ActionCard) eventType() string   { return "action_card" }
func (ActionResult) eventType() string { return "action_result" }
func (Finished) eventType() string     { return "finished" }
func (Reported) eventType() string     { return "automation_report" }
func (Asked) eventType() string        { return "questions" }
func (Answered) eventType() string     { return "answers" }

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
