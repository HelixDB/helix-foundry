package model

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/agentquestion"
)

// FoundryAgentRuntime is who runs a thread's turns. A thread keeps the
// runtime it was created with.
type FoundryAgentRuntime string

const (
	// FoundryAgentRuntimeAPI is Foundry's own foundry-agent-worker, on the
	// workspace's model provider.
	FoundryAgentRuntimeAPI FoundryAgentRuntime = "api"
	// FoundryAgentRuntimeClaudeCode and FoundryAgentRuntimeCodex are a helper
	// on a local install's machine driving a CLI its owner signed in to.
	FoundryAgentRuntimeClaudeCode FoundryAgentRuntime = "claude_code"
	FoundryAgentRuntimeCodex      FoundryAgentRuntime = "codex"
)

// Valid reports a runtime of the closed set.
func (r FoundryAgentRuntime) Valid() bool {
	switch r {
	case FoundryAgentRuntimeAPI, FoundryAgentRuntimeClaudeCode, FoundryAgentRuntimeCodex:
		return true
	}
	return false
}

// Subscription reports a helper's runtime: its turns run on a CLI its
// owner signed in to, and spend nothing of the workspace's budget.
func (r FoundryAgentRuntime) Subscription() bool {
	return r == FoundryAgentRuntimeClaudeCode || r == FoundryAgentRuntimeCodex
}

// FoundryAgentThreadKind is what a thread is: a closed set.
type FoundryAgentThreadKind string

const (
	// FoundryAgentThreadChat is a person's conversation, in their list.
	FoundryAgentThreadChat FoundryAgentThreadKind = "chat"
	// FoundryAgentThreadAutomation is the thread an automation's run runs its
	// Assistant steps in, as its owner: theirs to open from the run, and out
	// of their list.
	FoundryAgentThreadAutomation FoundryAgentThreadKind = "automation"
)

// FoundryAgentThread is one conversation with the Assistant, private to the
// user who started it.
type FoundryAgentThread struct {
	ID          string
	WorkspaceID string
	UserID      string
	Kind        FoundryAgentThreadKind
	Title       string
	Runtime     FoundryAgentRuntime
	// RuntimeSessionID is a helper's CLI session that the thread's next turn
	// resumes; empty for 'api', and until its first turn ends.
	RuntimeSessionID string
	// Granted is nil for a thread made, or run, with the workspace's whole
	// view, which a restricted member never reads; else the grant targets a
	// restricted member held when its turns ran, which they must all still
	// hold to read it, so narrowing their grants takes away what Founder read
	// under them, as it takes away an Analyst's answer.
	Granted *FoundryAgentThreadGrants
	// Surface is where it was started: its first turn's surface, read by
	// GetFoundryAgentThread and ListFoundryAgentThreads only.
	Surface   FoundryAgentSurface
	CreatedAt time.Time
	UpdatedAt time.Time
}

// FoundryAgentThreadGrants are the grant targets a restricted member's turns
// of a thread ran under, through themselves and their teams.
type FoundryAgentThreadGrants struct {
	Targets []access.Target
}

// FoundryAgentTurnKind is what started a turn.
type FoundryAgentTurnKind string

const (
	// FoundryAgentTurnMessage is a person's message; its Input is the text.
	FoundryAgentTurnMessage FoundryAgentTurnKind = "message"
	// FoundryAgentTurnActionResult is an action the person ran, which the
	// agent goes on from; its Input is the action's id.
	FoundryAgentTurnActionResult FoundryAgentTurnKind = "action_result"
	// FoundryAgentTurnAnswers is the person's answers to the questions the
	// thread's newest turn asked them (its Asked); its Input is that turn's
	// id, and Answers the answers, checked against its questions.
	FoundryAgentTurnAnswers FoundryAgentTurnKind = "answers"
	// FoundryAgentTurnAutomation is an automation's Assistant step, run
	// unattended as its owner; its Input is the step's instruction with what
	// fired the run, and AutomationRunID and AutomationNodeID name the step.
	FoundryAgentTurnAutomation FoundryAgentTurnKind = "automation"
)

// FoundryAgentSurface is where a turn's person reads it: a closed set. Its
// prompt and what may run on its own differ by surface. The zero value is
// the site, where every turn was read before Slack.
type FoundryAgentSurface uint8

const (
	// FoundryAgentSurfaceSite is Foundry's website (and an automation's run).
	FoundryAgentSurfaceSite FoundryAgentSurface = iota
	// FoundryAgentSurfaceSlack is a Slack thread, through Helix Foundry's bot.
	FoundryAgentSurfaceSlack
)

// String is the surface as its column keeps it.
func (s FoundryAgentSurface) String() string {
	switch s {
	case FoundryAgentSurfaceSite:
		return "site"
	case FoundryAgentSurfaceSlack:
		return "slack"
	}
	return fmt.Sprintf("FoundryAgentSurface(%d)", uint8(s))
}

// ParseFoundryAgentSurface reads a surface as its column keeps it, refusing
// any other.
func ParseFoundryAgentSurface(s string) (FoundryAgentSurface, error) {
	switch s {
	case "site":
		return FoundryAgentSurfaceSite, nil
	case "slack":
		return FoundryAgentSurfaceSlack, nil
	}
	return 0, fmt.Errorf("unknown foundry agent surface %q", s)
}

// FoundryAgentTurnStatus is where a turn is in its life.
type FoundryAgentTurnStatus string

const (
	FoundryAgentTurnQueued    FoundryAgentTurnStatus = "queued"
	FoundryAgentTurnRunning   FoundryAgentTurnStatus = "running"
	FoundryAgentTurnDone      FoundryAgentTurnStatus = "done"
	FoundryAgentTurnFailed    FoundryAgentTurnStatus = "failed"
	FoundryAgentTurnCancelled FoundryAgentTurnStatus = "cancelled"
)

// Live reports a turn not yet ended.
func (s FoundryAgentTurnStatus) Live() bool {
	return s == FoundryAgentTurnQueued || s == FoundryAgentTurnRunning
}

// FoundryAgentTurn is one turn of a thread.
type FoundryAgentTurn struct {
	ID          string
	WorkspaceID string
	ThreadID    string
	UserID      string
	// Runtime is its thread's, which the store copies when it queues the turn.
	Runtime FoundryAgentRuntime
	Kind    FoundryAgentTurnKind
	// Surface is where its person reads it. A continuation (an action's
	// result) takes its action's turn's, whatever its caller says.
	Surface FoundryAgentSurface
	Input   string
	// AutomationRunID and AutomationNodeID are set exactly on an automation's
	// turn: the run and step it is.
	AutomationRunID  *string
	AutomationNodeID *string
	// PageContext is what the person was looking at, ids only; nil for none.
	PageContext json.RawMessage
	// Asked is what the turn asked its person and ended waiting on
	// (helix_foundry_ask_person); nil for none, and always for an
	// automation's turn.
	Asked *FoundryAgentAsked
	// Answers are an answers turn's: the person's, one per question at
	// most, every blocking one answered (agentquestion.Check); nil for any
	// other kind, and empty when they left every question to the agent.
	Answers        []agentquestion.Answer
	Status         FoundryAgentTurnStatus
	Error          *string
	ClientToken    *string
	LeaseOwner     *string
	LeaseUntil     *time.Time
	Attempts       int
	NextAttemptAt  time.Time
	MaxUSDMicros   int64
	ReservedMicros int64
	// Spend is what the month was charged for the turn, LostMicros included.
	Spend FoundryGraphJobSpend
	// LostMicros is what Spend holds of reservations whose attempt died
	// unsettled: charged to the month, but no part of the turn's own spend,
	// which its cap bounds.
	LostMicros int64
	// Steps is how many model calls its attempts made.
	Steps      int
	LastSeq    int
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// MaxAgentQuestions is the most questions a turn asks its person at once.
const MaxAgentQuestions = 4

// FoundryAgentAsked is what a turn asked its person: 1 to MaxAgentQuestions
// questions of the Assistant's own, or a generation's questions, relayed as
// its agent wrote them (Generation set).
type FoundryAgentAsked struct {
	Questions  []agentquestion.Question     `json:"questions"`
	Generation *FoundryAgentAskedGeneration `json:"generation,omitempty"`
}

// FoundryAgentAskedGeneration is the generation, and the round of its
// questions, a turn relayed: the person's answers to them are sent to it
// (answer_generation_questions, run as them) before the answers turn.
type FoundryAgentAskedGeneration struct {
	OntologyID   string `json:"ontology_id"`
	GenerationID string `json:"generation_id"`
	Round        int    `json:"round"`
}

// Validate checks what a turn asked: questions with unique ids, the
// Assistant's own at most MaxAgentQuestions, and a relay naming its
// generation and a round.
func (a *FoundryAgentAsked) Validate() error {
	switch g := a.Generation; {
	case len(a.Questions) == 0:
		return fmt.Errorf("a turn asks at least one question")
	case g == nil && len(a.Questions) > MaxAgentQuestions:
		return fmt.Errorf("a turn asks at most %d questions of its own", MaxAgentQuestions)
	case g != nil && (g.OntologyID == "" || g.GenerationID == "" || g.Round < 1):
		return fmt.Errorf("a relayed generation's questions name it and their round")
	}
	for i, q := range a.Questions {
		if err := q.Validate(); err != nil {
			return err
		}
		if slices.ContainsFunc(a.Questions[:i], func(p agentquestion.Question) bool { return p.ID == q.ID }) {
			return fmt.Errorf("question %s is asked twice", q.ID)
		}
	}
	return nil
}

// FoundryAgentEvent is one event a turn shows, in its order.
type FoundryAgentEvent struct {
	Seq       int
	Event     json.RawMessage
	CreatedAt time.Time
}

// FoundryAgentRole is whose a transcript message is.
type FoundryAgentRole string

const (
	FoundryAgentRoleUser      FoundryAgentRole = "user"
	FoundryAgentRoleAssistant FoundryAgentRole = "assistant"
)

// FoundryAgentMessage is one message of a thread's transcript: its content is
// the agent's provider-neutral blocks, as JSON.
type FoundryAgentMessage struct {
	Seq       int
	TurnID    string
	Role      FoundryAgentRole
	Content   json.RawMessage
	EstTokens int
}

// FoundryAgentActionStatus is where an action is: prepared waits for its
// person; executing is their click running; the rest ended.
type FoundryAgentActionStatus string

const (
	FoundryAgentActionPrepared  FoundryAgentActionStatus = "prepared"
	FoundryAgentActionExecuting FoundryAgentActionStatus = "executing"
	FoundryAgentActionSucceeded FoundryAgentActionStatus = "succeeded"
	FoundryAgentActionFailed    FoundryAgentActionStatus = "failed"
	// FoundryAgentActionUnknown ran, then lost its answer: it may or may not
	// have taken effect, and is never run again.
	FoundryAgentActionUnknown   FoundryAgentActionStatus = "unknown"
	FoundryAgentActionExpired   FoundryAgentActionStatus = "expired"
	FoundryAgentActionDismissed FoundryAgentActionStatus = "dismissed"
)

// FoundryAgentExecutingFor is how long a click, or an automation's approved
// action, may run before the action reads as unknown: its process died
// without saying how it ended.
const FoundryAgentExecutingFor = 2 * time.Minute

// FoundryAgentAction is an action the agent prepared for its person to run.
type FoundryAgentAction struct {
	ID          string
	TurnID      string
	ThreadID    string
	WorkspaceID string
	UserID      string
	Operation   string
	Target      string
	// Payload is canonical, and PayloadHash the confirmation's hash of it.
	Payload     json.RawMessage
	PayloadHash string
	// Display is the names the server resolved for it.
	Display json.RawMessage
	// TargetNamedByUser says the target's ids came from the person's own
	// messages or page, not only from the agent.
	TargetNamedByUser bool
	ConfirmationID    string
	// TokenSealed is the confirmation's token, sealed under KeyID with the
	// action's id as its context.
	TokenSealed []byte
	KeyID       string
	Status      FoundryAgentActionStatus
	Result      json.RawMessage
	Error       *string
	ExpiresAt   time.Time
	DecidedBy   *string
	DecidedAt   *time.Time
	CreatedAt   time.Time
}

// Shown is the action's status as a person reads it at now: a prepared one
// past its confirmation has expired, and a click that ran too long lost its
// answer.
func (a *FoundryAgentAction) Shown(now time.Time) FoundryAgentActionStatus {
	switch {
	case a.Status == FoundryAgentActionPrepared && !now.Before(a.ExpiresAt):
		return FoundryAgentActionExpired
	case a.Status == FoundryAgentActionExecuting && a.DecidedAt != nil && now.Sub(*a.DecidedAt) > FoundryAgentExecutingFor:
		return FoundryAgentActionUnknown
	}
	return a.Status
}

// FoundryAgentLimits are a workspace's limits on the Assistant, checked when
// a person sends and when a turn is claimed.
type FoundryAgentLimits struct {
	// Since is the start of the budget's month.
	Since     time.Time
	CapMicros int64
	// PerHour is how many messages one user may send in an hour.
	PerHour int
	// Running is how many of the workspace's turns may run at once.
	Running int
}

// FoundryAgentClaim is how a claim leases a turn.
type FoundryAgentClaim struct {
	// Runtime is the turns this claim takes: those of its runtime only.
	Runtime FoundryAgentRuntime
	Lease   time.Duration
	Defer   time.Duration
	// Limits bound people's turns; Automations automations' turns, a budget
	// and a running cap of their own (PerHour unused): unattended work never
	// spends what people use. Only the api runtime runs automations' turns
	// (their threads are Foundry's worker's), so a helper's claim sets none.
	Limits      FoundryAgentLimits
	Automations FoundryAgentLimits
}

// FoundryAgentRefusalReason is why a write was refused: a closed set the API
// words for the person.
type FoundryAgentRefusalReason string

const (
	// FoundryAgentThreadBusy: the thread has a live turn, or an action of it
	// is running.
	FoundryAgentThreadBusy FoundryAgentRefusalReason = "thread_busy"
	FoundryAgentPerHour    FoundryAgentRefusalReason = "per_hour"
	FoundryAgentBudget     FoundryAgentRefusalReason = "budget"
	// FoundryAgentTooManyCards: the thread already waits on its person for
	// as many actions as it may.
	FoundryAgentTooManyCards FoundryAgentRefusalReason = "too_many_cards"
	// FoundryAgentNotPrepared: the action isn't waiting to be run.
	FoundryAgentNotPrepared FoundryAgentRefusalReason = "not_prepared"
	// FoundryAgentChanged: the click named another payload than the action's.
	FoundryAgentChanged FoundryAgentRefusalReason = "changed"
	// FoundryAgentExpired: the action's confirmation ran out; refresh it.
	FoundryAgentExpired FoundryAgentRefusalReason = "expired"
	// FoundryAgentQuestionsClosed: the answers are to questions no longer
	// open: answered already, or the thread went on since they were asked.
	FoundryAgentQuestionsClosed FoundryAgentRefusalReason = "questions_closed"
)

// FoundryAgentRefusal is a write the Assistant's rules refuse now.
type FoundryAgentRefusal struct {
	Reason FoundryAgentRefusalReason
	Detail string
}

func (r FoundryAgentRefusal) Error() string { return string(r.Reason) + ": " + r.Detail }

// FoundryAgentTurnOutcome ends an attempt. A non-nil RetryAt requeues the
// turn (Uncounted gives its attempt back); otherwise Status is done, or
// failed with Error. Spend and Steps add to earlier attempts'.
type FoundryAgentTurnOutcome struct {
	Status    FoundryAgentTurnStatus
	RetryAt   *time.Time
	Uncounted bool
	Error     string
	Spend     FoundryGraphJobSpend
	Steps     int
}

// Validate refuses an outcome no turn can end with.
func (o FoundryAgentTurnOutcome) Validate() error {
	switch {
	case o.RetryAt != nil && o.Status != FoundryAgentTurnQueued:
		return fmt.Errorf("a retried turn is queued, not %s", o.Status)
	case o.RetryAt == nil && o.Uncounted:
		return fmt.Errorf("only a retry gives its attempt back")
	case o.RetryAt != nil, o.Status == FoundryAgentTurnDone:
	case o.Status == FoundryAgentTurnFailed:
		if o.Error == "" {
			return fmt.Errorf("a failed turn says why")
		}
	default:
		return fmt.Errorf("a turn can't end %q", o.Status)
	}
	if o.Spend.USDMicros < 0 || o.Spend.InputTokens < 0 || o.Spend.OutputTokens < 0 || o.Steps < 0 {
		return fmt.Errorf("a turn's spend is not negative")
	}
	return nil
}

// FoundryAgentActionEnd is how a click ended: succeeded with its result,
// failed or unknown with why.
type FoundryAgentActionEnd struct {
	Status FoundryAgentActionStatus
	Result json.RawMessage
	Error  string
}

// Validate refuses an end no click can have.
func (e FoundryAgentActionEnd) Validate() error {
	switch e.Status {
	case FoundryAgentActionSucceeded:
		if len(e.Result) == 0 {
			return fmt.Errorf("a succeeded action has its result")
		}
	case FoundryAgentActionFailed, FoundryAgentActionUnknown:
		if e.Error == "" {
			return fmt.Errorf("a %s action says why", e.Status)
		}
	default:
		return fmt.Errorf("a click can't end %q", e.Status)
	}
	return nil
}
