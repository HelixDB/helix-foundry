package automation

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Check is how a condition picks its branch from the step before it: a
// closed set.
type Check interface {
	// Evaluate is the branch out, true or false, from the step's output; an
	// error when the output holds nothing to check.
	Evaluate(out Output) (bool, error)
	// checks reports whether a step of config can give what it checks.
	checks(config Config) bool
	// needs says what it checks, for an error.
	needs() string
}

// AnswerIs is true when the Assistant step before it answered Yes (or no,
// with Yes false).
type AnswerIs struct{ Yes bool }

// CountIs is true when the count of the step before it compares to Value as
// Op says: the documents that arrived, the rows a sync wrote, or the count an
// Assistant step reported.
type CountIs struct {
	Op    CountOp
	Value int64
}

// CountOp is how a count compares: a closed set.
type CountOp string

const (
	AtLeast CountOp = "at_least"
	AtMost  CountOp = "at_most"
	Equals  CountOp = "equals"
)

func (c AnswerIs) Evaluate(out Output) (bool, error) {
	if out.Answer == nil {
		return false, errors.New("Founder gave no yes-or-no answer")
	}
	return *out.Answer == c.Yes, nil
}

func (c CountIs) Evaluate(out Output) (bool, error) {
	if out.Count == nil {
		return false, errors.New("the step before it gave no count")
	}
	switch c.Op {
	case AtLeast:
		return *out.Count >= c.Value, nil
	case AtMost:
		return *out.Count <= c.Value, nil
	default:
		return *out.Count == c.Value, nil
	}
}

func (AnswerIs) checks(config Config) bool {
	_, ok := config.(AssistantNode)
	return ok
}

func (CountIs) checks(config Config) bool {
	switch c := config.(type) {
	case AssistantNode:
		return true
	case TriggerNode:
		switch c.Trigger.(type) {
		case DocumentArrived, SyncFinished:
			return true
		}
	}
	return false
}

func (AnswerIs) needs() string { return "an answer is a Founder step's" }
func (CountIs) needs() string {
	return "a count is a Founder step's, or a document_arrived or sync_finished trigger's"
}

type wireCheck struct {
	Check  string  `json:"check"`
	Equals string  `json:"equals,omitempty"`
	Op     CountOp `json:"op,omitempty"`
	Value  *int64  `json:"value,omitempty"`
}

func parseCheck(raw json.RawMessage) (Check, error) {
	wrong := errors.New(`a condition is {check: "answer", equals: "yes" | "no"} or {check: "count", op: "at_least" | "at_most" | "equals", value: 0 or more}`)
	var w wireCheck
	if err := decodeStrict(raw, &w); err != nil {
		return nil, fmt.Errorf("%w: %w", wrong, err)
	}
	switch {
	case w.Check == "answer" && w.Op == "" && w.Value == nil && (w.Equals == "yes" || w.Equals == "no"):
		return AnswerIs{Yes: w.Equals == "yes"}, nil
	case w.Check == "count" && w.Equals == "" && w.Value != nil && *w.Value >= 0 && (w.Op == AtLeast || w.Op == AtMost || w.Op == Equals):
		return CountIs{Op: w.Op, Value: *w.Value}, nil
	}
	return nil, wrong
}

func wireCheckOf(c Check) wireCheck {
	switch c := c.(type) {
	case AnswerIs:
		equals := "no"
		if c.Yes {
			equals = "yes"
		}
		return wireCheck{Check: "answer", Equals: equals}
	case CountIs:
		value := c.Value
		return wireCheck{Check: "count", Op: c.Op, Value: &value}
	}
	panic(fmt.Sprintf("automation: no wire form of %T", c))
}

// Output is what a step of a run gave, as the run keeps it: what a condition
// checks, what a notify step tells, and what the run's page shows.
type Output struct {
	// Summary is what happened, in a few sentences: an Assistant step's
	// report, or what a trigger found.
	Summary string `json:"summary,omitempty"`
	// Answer is an Assistant step's yes or no, when it gave one.
	Answer *bool `json:"answer,omitempty"`
	// Count is the documents that arrived, the rows a sync wrote, or the count
	// an Assistant step reported.
	Count *int64 `json:"count,omitempty"`
	// Branch is the branch a condition took.
	Branch *bool `json:"branch,omitempty"`
	// Result is an action's result, as Foundry's tools return it: untrusted.
	Result json.RawMessage `json:"result,omitempty"`
	// Pending counts the actions an Assistant step prepared that wait for the
	// owner, unapproved.
	Pending int `json:"pending,omitempty"`
}

// NodeStatus is where a step of a run is: a closed set. A step with no
// status hasn't started.
type NodeStatus string

const (
	NodeRunning   NodeStatus = "running"
	NodeSucceeded NodeStatus = "succeeded"
	NodeFailed    NodeStatus = "failed"
	// NodeSkipped wasn't run: no edge into it was taken.
	NodeSkipped NodeStatus = "skipped"
	// NodeBlocked wasn't run: a step before it failed, or was blocked.
	NodeBlocked NodeStatus = "blocked"
)

// Ended reports a step that won't change again.
func (s NodeStatus) Ended() bool {
	return s == NodeSucceeded || s == NodeFailed || s == NodeSkipped || s == NodeBlocked
}

// NodeState is a step's status and output in a run.
type NodeState struct {
	Status NodeStatus
	Output Output
}

// Step is what a run does next: a closed set.
type Step interface{ step() }

// RunNode starts the node.
type RunNode struct{ ID string }

// SkipNode records the node as blocked (NodeBlocked) when AfterFailure, a
// step before it having failed, else as skipped (NodeSkipped): no edge into
// it was taken.
type SkipNode struct {
	ID           string
	AfterFailure bool
}

// WaitNode waits for the running node to end.
type WaitNode struct{ ID string }

// Done ends the run: failed when any step failed.
type Done struct{ Failed bool }

func (RunNode) step()  {}
func (SkipNode) step() {}
func (WaitNode) step() {}
func (Done) step()     {}

// Next is what a run of s does next, given each step's state, one step at a
// time in s.Order(): a node runs once every node with an edge into it ended
// and one of those edges was taken (from a step that succeeded, a
// condition's along the branch it took); it is skipped when none was, and
// after a failure of a node before it, whose downstream steps never run. A
// run ends once every node did, failed if any failed.
//
//	for step := spec.Next(states); ; step = spec.Next(states) { … }
func (s Spec) Next(states map[string]NodeState) Step {
	failed := false
	for _, id := range s.order {
		state := states[id]
		switch state.Status {
		case NodeRunning:
			return WaitNode{ID: id}
		case NodeFailed:
			failed = true
			continue
		case NodeSucceeded, NodeSkipped, NodeBlocked:
			continue
		}
		in := s.In(id)
		if slices.ContainsFunc(in, func(e Edge) bool { return states[e.From].Status == NodeFailed || states[e.From].Status == NodeBlocked }) {
			return SkipNode{ID: id, AfterFailure: true}
		}
		if slices.ContainsFunc(in, func(e Edge) bool { return taken(e, states[e.From]) }) || len(in) == 0 {
			return RunNode{ID: id}
		}
		return SkipNode{ID: id}
	}
	return Done{Failed: failed}
}

// taken reports whether e was taken out of a step in state from.
func taken(e Edge, from NodeState) bool {
	if from.Status != NodeSucceeded {
		return false
	}
	if e.Branch == BranchNone {
		return true
	}
	return from.Output.Branch != nil && *from.Output.Branch == (e.Branch == BranchTrue)
}
