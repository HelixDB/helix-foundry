package model

import (
	"encoding/json"
	"fmt"
	"time"
)

// FoundryQuestionStatus is where a question is in its life.
type FoundryQuestionStatus string

const (
	// FoundryQuestionQueued waits for the foundry-ask-worker: new, retried,
	// or held back by its workspace's limits.
	FoundryQuestionQueued FoundryQuestionStatus = "queued"
	// FoundryQuestionRunning is an attempt holding the question's lease and
	// its spend reservation.
	FoundryQuestionRunning FoundryQuestionStatus = "running"
	// FoundryQuestionAnswered has an answer with citations.
	FoundryQuestionAnswered FoundryQuestionStatus = "answered"
	// FoundryQuestionCannotAnswer has the agent's reason and the gap in the
	// ontology that kept it from answering.
	FoundryQuestionCannotAnswer FoundryQuestionStatus = "cannot_answer"
	// FoundryQuestionFailed ended without an answer: its error says why.
	FoundryQuestionFailed FoundryQuestionStatus = "failed"
	// FoundryQuestionCancelled was cancelled by a user before it ended.
	FoundryQuestionCancelled FoundryQuestionStatus = "cancelled"
)

// Live reports a question not yet ended.
func (s FoundryQuestionStatus) Live() bool {
	return s == FoundryQuestionQueued || s == FoundryQuestionRunning
}

// FoundryQuestion is one question asked of one load of an ontology.
type FoundryQuestion struct {
	ID          string
	WorkspaceID string
	OntologyID  string
	LoadID      string
	// VersionID is the load's version, whose definition the agent is given.
	VersionID string
	// ThreadID is the first question of its thread; a new thread's is its own ID.
	ThreadID string
	Question string
	// Focus is what the user asked about, passed to the agent as it came;
	// nil when the question has none.
	Focus  json.RawMessage
	Status FoundryQuestionStatus
	// Result is the agent's AskResult, set once the question is answered or
	// cannot be, and kept for a failed one whose agent sent one.
	Result        json.RawMessage
	Error         *string
	ClientToken   *string
	LeaseOwner    *string
	LeaseUntil    *time.Time
	Attempts      int
	NextAttemptAt time.Time
	// MaxUSDMicros is the question's own cap, reserved while it runs.
	MaxUSDMicros int64
	// ReservedMicros is the reservation of the running attempt; 0 otherwise.
	ReservedMicros int64
	// Spend is what its attempts spent.
	Spend FoundryGraphJobSpend
	// LastSeq is the seq of its newest event; 0 before any.
	LastSeq       int
	AskedByUserID *string
	CreatedAt     time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
}

// FoundryQuestionEvent is one event of a question's run, in its order.
type FoundryQuestionEvent struct {
	Seq       int
	Event     json.RawMessage
	CreatedAt time.Time
}

// FoundryQuestionLimits are a workspace's limits on questions, checked when
// one is asked and when one is claimed.
type FoundryQuestionLimits struct {
	// Since is the start of the month the budget covers: questions claimed
	// since then, whenever they were asked, count against CapMicros.
	Since     time.Time
	CapMicros int64
	// PerHour is how many questions the workspace may ask in an hour.
	PerHour int
	// Running is how many of its questions may run at once.
	Running int
}

// FoundryQuestionSpend is what a workspace's questions charged to one month spent,
// and what its running ones hold back.
type FoundryQuestionSpend struct {
	SpentMicros    int64
	ReservedMicros int64
}

// FoundryQuestionRefusal is a question the workspace's limits refuse now.
type FoundryQuestionRefusal struct {
	// Budget is a refusal for the month's budget; otherwise the hour's count.
	Budget bool
	Reason string
}

func (r FoundryQuestionRefusal) Error() string { return r.Reason }

// FoundryQuestionClaim is how a claim leases a question.
type FoundryQuestionClaim struct {
	Lease  time.Duration
	Limits FoundryQuestionLimits
	// Defer is how long a question held back by its workspace's running
	// questions or their reservations waits before it is tried again.
	Defer time.Duration
}

// FoundryQuestionOutcome ends an attempt. A non-nil RetryAt requeues the
// question (Uncounted gives its attempt back); otherwise Status is answered
// or cannot_answer, with Result, or failed, with Error. Spend adds to what
// earlier attempts spent.
type FoundryQuestionOutcome struct {
	Status    FoundryQuestionStatus
	RetryAt   *time.Time
	Uncounted bool
	Result    json.RawMessage
	Error     string
	Spend     FoundryGraphJobSpend
}

// Validate refuses an outcome no question can end with.
func (o FoundryQuestionOutcome) Validate() error {
	switch {
	case o.RetryAt != nil && o.Status != FoundryQuestionQueued:
		return fmt.Errorf("a retried question is queued, not %s", o.Status)
	case o.RetryAt == nil && o.Uncounted:
		return fmt.Errorf("only a retry gives its attempt back")
	case o.RetryAt != nil:
	case o.Status == FoundryQuestionAnswered || o.Status == FoundryQuestionCannotAnswer:
		if len(o.Result) == 0 {
			return fmt.Errorf("a question that ends %s has a result", o.Status)
		}
	case o.Status == FoundryQuestionFailed:
		if o.Error == "" {
			return fmt.Errorf("a failed question says why")
		}
	default:
		return fmt.Errorf("a question can't end %q", o.Status)
	}
	if o.Spend.USDMicros < 0 || o.Spend.InputTokens < 0 || o.Spend.OutputTokens < 0 {
		return fmt.Errorf("a question's spend is not negative")
	}
	return nil
}
