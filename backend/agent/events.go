package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
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
// person (the model is told in words of its own), with no error. An ask or
// get_question that returned names its Analyst question (question), whatever
// the output's size, so the site shows the Analyst's answer in place.
type ToolReturned struct {
	ToolUseID string       `json:"tool_use_id"`
	Name      string       `json:"name"`
	IsError   bool         `json:"is_error"`
	Error     string       `json:"error,omitempty"`
	Output    any          `json:"output,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
	NotRun    NotRunReason `json:"not_run,omitempty"`
	Question  *QuestionRef `json:"question,omitempty"`
}

// QuestionRef names the Analyst question an ask or get_question call returned, kept whatever the output's size.
type QuestionRef struct {
	OntologyID string `json:"ontology_id"`
	ID         string `json:"id"`
	LoadID     string `json:"load_id,omitempty"`
	ThreadID   string `json:"thread_id,omitempty"`
	Status     string `json:"status,omitempty"`
}

// questionOf is the Analyst question a call of name that returned output
// names: its input's ontology and its output's question, for an ask or a
// get_question; nil for any other call, or one whose input or output names
// none.
func questionOf(name string, input json.RawMessage, output any) *QuestionRef {
	if name != toolAsk && name != toolGetQuestion {
		return nil
	}
	var in struct {
		OntologyID string `json:"ontology_id"`
	}
	var out struct {
		ID       string `json:"id"`
		LoadID   string `json:"load_id"`
		ThreadID string `json:"thread_id"`
		Status   string `json:"status"`
	}
	raw, err := json.Marshal(output)
	if err != nil || json.Unmarshal(input, &in) != nil || json.Unmarshal(raw, &out) != nil || in.OntologyID == "" || out.ID == "" {
		return nil
	}
	return &QuestionRef{OntologyID: in.OntologyID, ID: out.ID, LoadID: out.LoadID, ThreadID: out.ThreadID, Status: out.Status}
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
// dismiss: the action's own fields are read from the thread. Auto is one
// that ran at once under autoRun, because the person's message asked for
// it; never an automation's approved action or a click.
type ActionCard struct {
	ActionID          string          `json:"action_id"`
	Operation         string          `json:"operation"`
	Target            string          `json:"target"`
	Payload           json.RawMessage `json:"payload"`
	PayloadHash       string          `json:"payload_hash"`
	Display           json.RawMessage `json:"display"`
	TargetNamedByUser bool            `json:"target_named_by_user"`
	ExpiresAt         time.Time       `json:"expires_at"`
	Auto              bool            `json:"auto,omitempty"`
}

// ActionResult opens the turn the agent goes on from an action in: how the
// person's click ended. In a turn that ran an action at once, it is how that
// ended: Auto as on its ActionCard.
type ActionResult struct {
	ActionID  string          `json:"action_id"`
	Operation string          `json:"operation"`
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	Auto      bool            `json:"auto,omitempty"`
}

// Asked shows the person the questions the turn asks them
// (toolAskPerson), which it ends waiting on (StopAsked): Founder's own, or
// a generation's, as its agent wrote them (Generation set). They are the
// agents' words, which may quote the data: plain text.
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

// decoders reads each event type of the closed set, keyed by the type
// MarshalEvent writes.
var decoders = map[string]func(json.RawMessage) (Event, error){
	Started{}.eventType():      decodeAs[Started],
	Said{}.eventType():         decodeAs[Said],
	TextDelta{}.eventType():    decodeAs[TextDelta],
	ToolCalled{}.eventType():   decodeAs[ToolCalled],
	ToolReturned{}.eventType(): decodeAs[ToolReturned],
	ActionCard{}.eventType():   decodeAs[ActionCard],
	ActionResult{}.eventType(): decodeAs[ActionResult],
	Finished{}.eventType():     decodeAs[Finished],
	Reported{}.eventType():     decodeAs[Reported],
	Asked{}.eventType():        decodeAs[Asked],
	Answered{}.eventType():     decodeAs[Answered],
}

// DecodeEvent reads a turn's stored event (MarshalEvent's) back as the closed
// set, by value: a Said, a ToolReturned, a Finished, and so on. An event
// without a type, of a type outside the set, or whose fields don't fit its
// type is an error. Fields its type doesn't name are ignored, so a newer
// writer's additions don't stop an older reader; enum fields (a Finished's
// Reason, a ToolReturned's NotRun) are kept as written, for the reader to
// word with a default. Numbers inside a tool's output stay exact
// (json.Number), as MarshalEvent keeps them.
func DecodeEvent(raw json.RawMessage) (Event, error) {
	var head struct {
		Type *string `json:"type"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("agent: event: %w", err)
	}
	if head.Type == nil {
		return nil, fmt.Errorf("agent: event has no type")
	}
	decode, ok := decoders[*head.Type]
	if !ok {
		return nil, fmt.Errorf("agent: unknown event type %q", *head.Type)
	}
	return decode(raw)
}

// decodeAs reads raw as an E.
func decodeAs[E Event](raw json.RawMessage) (Event, error) {
	var e E
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&e); err != nil {
		return nil, fmt.Errorf("agent: %s event: %w", e.eventType(), err)
	}
	return e, nil
}
