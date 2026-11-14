package model

import (
	"encoding/json"
	"fmt"
	"time"
)

// FoundryAutomationStatus is whether an automation runs: a closed set.
type FoundryAutomationStatus string

const (
	// FoundryAutomationNeedsApproval waits for its owner to approve its spec
	// with a click in Foundry: a new one, or one whose spec changed.
	FoundryAutomationNeedsApproval FoundryAutomationStatus = "needs_approval"
	// FoundryAutomationActive runs when its trigger fires.
	FoundryAutomationActive FoundryAutomationStatus = "active"
	// FoundryAutomationPaused runs only when its owner runs it by hand.
	FoundryAutomationPaused FoundryAutomationStatus = "paused"
)

// FoundryAutomationPause is why an automation paused: a closed set.
type FoundryAutomationPause string

const (
	// FoundryAutomationPausedByPerson: its owner, or an owner or admin of the
	// workspace, paused it.
	FoundryAutomationPausedByPerson FoundryAutomationPause = "person"
	// FoundryAutomationPausedFailures: its runs failed too many times in a row.
	FoundryAutomationPausedFailures FoundryAutomationPause = "failures"
	// FoundryAutomationPausedOwnerLostAccess: its owner can no longer act in
	// the workspace.
	FoundryAutomationPausedOwnerLostAccess FoundryAutomationPause = "owner_lost_access"
)

// FoundryAutomation is an automation of a workspace, run as its owner.
type FoundryAutomation struct {
	ID          string
	WorkspaceID string
	OwnerUserID string
	Name        string
	Description string
	// Spec is the automation package's canonical JSON.
	Spec json.RawMessage
	// Revision counts its edits; SpecRevision its spec's.
	Revision     int64
	SpecRevision int64
	// ApprovedSpecRevision is the spec revision its owner last approved, at
	// ApprovedAt; nil until they first did.
	ApprovedSpecRevision *int64
	ApprovedAt           *time.Time
	// TriggerKind and TriggerTarget repeat the spec's trigger: its kind, and
	// the id an event trigger listens to.
	TriggerKind   string
	TriggerTarget *string
	Status        FoundryAutomationStatus
	// PausedReason, PausedByUserID (a person's pause) and PausedAt are set
	// exactly while it is paused.
	PausedReason   *FoundryAutomationPause
	PausedByUserID *string
	PausedAt       *time.Time
	// Failures counts the runs that failed in a row; BackoffUntil is when it
	// may start another after them.
	Failures     int
	BackoffUntil *time.Time
	// NextFireAt is an active schedule's next time.
	NextFireAt *time.Time
	// Cursor is an active event trigger's: the newest work it has seen.
	Cursor    *FoundryAutomationCursor
	CheckedAt *time.Time
	// NoticeID is the subject of the email that told its owner it paused
	// itself.
	NoticeID    *string
	ClientToken *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// LastRun is its newest run, without its spec; nil before its first. Set
	// by the reads that list and get automations.
	LastRun *FoundryAutomationRun
}

// FoundryAutomationCursor is the newest ended work an event trigger has seen:
// its end time and its row's id, in that order.
type FoundryAutomationCursor struct {
	At time.Time
	ID string
}

// After reports whether c comes after o.
func (c FoundryAutomationCursor) After(o FoundryAutomationCursor) bool {
	return c.At.After(o.At) || c.At.Equal(o.At) && c.ID > o.ID
}

// FoundryAutomationUpdate is an edit: each field set changes, at least one.
// A spec that changes needs its owner's approval again.
type FoundryAutomationUpdate struct {
	Name        *string
	Description *string
	Spec        *FoundryAutomationSpec
}

// FoundryAutomationSpec is a spec as the store keeps it: its canonical JSON
// and its trigger.
type FoundryAutomationSpec struct {
	JSON          json.RawMessage
	TriggerKind   string
	TriggerTarget *string
}

// FoundryAutomationRunStatus is where a run is: a closed set.
type FoundryAutomationRunStatus string

const (
	FoundryAutomationRunQueued    FoundryAutomationRunStatus = "queued"
	FoundryAutomationRunRunning   FoundryAutomationRunStatus = "running"
	FoundryAutomationRunSucceeded FoundryAutomationRunStatus = "succeeded"
	FoundryAutomationRunFailed    FoundryAutomationRunStatus = "failed"
	FoundryAutomationRunCancelled FoundryAutomationRunStatus = "cancelled"
	// FoundryAutomationRunSkipped never started: Error says why.
	FoundryAutomationRunSkipped FoundryAutomationRunStatus = "skipped"
)

// Live reports a run not yet ended.
func (s FoundryAutomationRunStatus) Live() bool {
	return s == FoundryAutomationRunQueued || s == FoundryAutomationRunRunning
}

// FoundryAutomationRun is one run of an automation.
type FoundryAutomationRun struct {
	ID           string
	AutomationID string
	WorkspaceID  string
	OwnerUserID  string
	// Spec is the spec it runs, as its automation had it when it was queued;
	// nil in a list.
	Spec         json.RawMessage
	SpecRevision int64
	TriggerKey   string
	// TriggerEvent is what fired it (FoundryAutomationEvent's JSON).
	TriggerEvent  json.RawMessage
	Status        FoundryAutomationRunStatus
	Error         *string
	ThreadID      *string
	LeaseOwner    *string
	LeaseUntil    *time.Time
	Attempts      int
	NextAttemptAt time.Time
	USDMicros     int64
	CreatedAt     time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
}

// FoundryAutomationNodeStatus is where a step of a run is: the automation
// package's statuses, and executing, an action whose call is in flight.
type FoundryAutomationNodeStatus string

const (
	FoundryAutomationNodeRunning   FoundryAutomationNodeStatus = "running"
	FoundryAutomationNodeExecuting FoundryAutomationNodeStatus = "executing"
	FoundryAutomationNodeSucceeded FoundryAutomationNodeStatus = "succeeded"
	FoundryAutomationNodeFailed    FoundryAutomationNodeStatus = "failed"
	FoundryAutomationNodeSkipped   FoundryAutomationNodeStatus = "skipped"
	FoundryAutomationNodeBlocked   FoundryAutomationNodeStatus = "blocked"
)

// Ended reports a step that won't change again.
func (s FoundryAutomationNodeStatus) Ended() bool {
	return s != FoundryAutomationNodeRunning && s != FoundryAutomationNodeExecuting
}

// FoundryAutomationRunNode is one step of a run.
type FoundryAutomationRunNode struct {
	RunID  string
	NodeID string
	Status FoundryAutomationNodeStatus
	// Output is the automation package's Output, as JSON; nil while it runs.
	Output         json.RawMessage
	Error          *string
	TurnID         *string
	NotificationID *string
	USDMicros      int64
	StartedAt      time.Time
	FinishedAt     *time.Time
}

// FoundryAutomationNodeEnd ends a step: succeeded with its output, skipped
// or blocked with why (Output's summary), failed with why (Error). Spend is
// what it cost; Notification, for a notify step, the email it queues.
type FoundryAutomationNodeEnd struct {
	Status       FoundryAutomationNodeStatus
	Output       json.RawMessage
	Error        string
	USDMicros    int64
	Notification *FoundryNotification
}

// Validate refuses an end no step can have.
func (e FoundryAutomationNodeEnd) Validate() error {
	switch {
	case !e.Status.Ended():
		return fmt.Errorf("a step can't end %q", e.Status)
	case e.Status == FoundryAutomationNodeFailed && e.Error == "":
		return fmt.Errorf("a failed step says why")
	case e.Status != FoundryAutomationNodeFailed && e.Error != "":
		return fmt.Errorf("only a failed step has an error")
	case e.USDMicros < 0:
		return fmt.Errorf("a step's spend is not negative")
	case e.Notification != nil && (e.Status != FoundryAutomationNodeSucceeded || e.Notification.Kind != FoundryNotifyAutomationRun):
		return fmt.Errorf("only a notify step that succeeded queues its email")
	}
	return nil
}

// FoundryAutomationEvent is what fired a run, as its trigger_event keeps it:
// Kind is the trigger's kind, or manual for a run by hand; the ids name the
// work it found (the newest, when several ended at once), Count how many
// ended or arrived, Outcome how a sync or generation ended.
type FoundryAutomationEvent struct {
	Kind         string    `json:"kind"`
	At           time.Time `json:"at"`
	SyncID       string    `json:"sync_id,omitempty"`
	LoadID       string    `json:"load_id,omitempty"`
	GenerationID string    `json:"generation_id,omitempty"`
	DocumentIDs  []string  `json:"document_ids,omitempty"`
	Outcome      string    `json:"outcome,omitempty"`
	Count        int64     `json:"count,omitempty"`
	// Rows is what a sync wrote.
	Rows *int64 `json:"rows,omitempty"`
	// ByUserID is who ran it by hand.
	ByUserID string `json:"by_user_id,omitempty"`
}

// FoundryAutomationEnded is one row of work an event trigger found ended: a
// sync, a load, a generation or a document, with its end time and id (its
// cursor) and how it ended.
type FoundryAutomationEnded struct {
	Cursor  FoundryAutomationCursor
	Outcome string
	// Rows is what a sync wrote.
	Rows *int64
}

// FoundryAutomationAdvance is how a fire moves its automation on: a closed
// set. A schedule's next time replaces the one that came due; an event
// trigger's cursor moves past the work it found.
type FoundryAutomationAdvance interface{ advance() }

// FoundryScheduleAdvance moves an active schedule from Due, the next_fire_at
// that came due, to Next.
type FoundryScheduleAdvance struct{ Due, Next time.Time }

// FoundryCursorAdvance moves an active event trigger's cursor from From, as
// it was read, to To.
type FoundryCursorAdvance struct{ From, To FoundryAutomationCursor }

func (FoundryScheduleAdvance) advance() {}
func (FoundryCursorAdvance) advance()   {}

// FoundryAutomationFire moves an automation on (Advance) and queues Run, with
// its trigger's output (TriggerNodeID's, TriggerOutput), in one write; a nil
// Run only moves it on (the work found fires nothing). Run's spec is the
// automation's as it is.
type FoundryAutomationFire struct {
	AutomationID  string
	WorkspaceID   string
	Advance       FoundryAutomationAdvance
	Run           *FoundryAutomationRun
	TriggerNodeID string
	TriggerOutput json.RawMessage
}

// FoundryAutomationFired is what a fire did: a closed set.
type FoundryAutomationFired string

const (
	// FoundryAutomationFiredQueued queued its run.
	FoundryAutomationFiredQueued FoundryAutomationFired = "queued"
	// FoundryAutomationFiredMoved only moved it on.
	FoundryAutomationFiredMoved FoundryAutomationFired = "moved"
	// FoundryAutomationFiredSkipped recorded its run as skipped: a run of it
	// was live (a schedule's), or the workspace ran its runs of the day.
	FoundryAutomationFiredSkipped FoundryAutomationFired = "skipped"
	// FoundryAutomationFiredWaiting changed nothing: a run of it is live, and
	// the work found waits for it to end (an event trigger's).
	FoundryAutomationFiredWaiting FoundryAutomationFired = "waiting"
	// FoundryAutomationFiredLost changed nothing: it is no longer active, or
	// another fire moved it on first.
	FoundryAutomationFiredLost FoundryAutomationFired = "lost"
)

// FoundryAutomationLimits are a workspace's limits on automations' runs.
type FoundryAutomationLimits struct {
	// RunsPerDay is how many runs its automations start in any 24 hours.
	RunsPerDay int
}

// FoundryAutomationRunEnd ends a claimed run: succeeded, or failed with why
// (a step's failure, or the run's own). PauseAfter pauses the automation once
// its runs have failed that many times in a row, queueing Notice, its owner's
// email; Backoff is how long a run that failed holds the next back, doubling
// with each failure in a row. LostAccess pauses it at once: its owner can no
// longer act in the workspace.
type FoundryAutomationRunEnd struct {
	Status     FoundryAutomationRunStatus
	Error      string
	PauseAfter int
	Backoff    time.Duration
	Notice     *FoundryNotification
	LostAccess bool
}

// Validate refuses an end no run can have.
func (e FoundryAutomationRunEnd) Validate() error {
	switch e.Status {
	case FoundryAutomationRunSucceeded:
		if e.Error != "" || e.LostAccess {
			return fmt.Errorf("a run that succeeded has no error")
		}
	case FoundryAutomationRunFailed:
		if e.Error == "" {
			return fmt.Errorf("a failed run says why")
		}
	default:
		return fmt.Errorf("a run can't end %q", e.Status)
	}
	if e.Notice != nil && e.Notice.Kind != FoundryNotifyAutomationPaused {
		return fmt.Errorf("a run's end queues only its automation's paused email")
	}
	if e.PauseAfter < 1 || e.Backoff < 0 {
		return fmt.Errorf("a run's end pauses after at least one failure")
	}
	return nil
}

// FoundryAutomationRefusalReason is why a write was refused: a closed set the
// API words for the person.
type FoundryAutomationRefusalReason string

const (
	// FoundryAutomationNotOwner: only its owner edits, approves, resumes or
	// runs an automation.
	FoundryAutomationNotOwner FoundryAutomationRefusalReason = "not_owner"
	// FoundryAutomationNotAllowed: pausing, deleting or cancelling another's
	// automation is an owner's or admin's of the workspace.
	FoundryAutomationNotAllowed FoundryAutomationRefusalReason = "not_allowed"
	// FoundryAutomationChanged: the write named a revision it no longer has.
	FoundryAutomationChanged FoundryAutomationRefusalReason = "changed"
	// FoundryAutomationLimit: the workspace has as many automations as it may.
	FoundryAutomationLimit FoundryAutomationRefusalReason = "limit"
	// FoundryAutomationUnapproved: its spec waits for its owner's approval.
	FoundryAutomationUnapproved FoundryAutomationRefusalReason = "unapproved"
	// FoundryAutomationBusy: a run of it is live.
	FoundryAutomationBusy FoundryAutomationRefusalReason = "busy"
	// FoundryAutomationRunsPerDay: the workspace's automations ran their runs
	// of the day.
	FoundryAutomationRunsPerDay FoundryAutomationRefusalReason = "runs_per_day"
	// FoundryAutomationNotActive: it isn't running, so there is nothing to pause.
	FoundryAutomationNotActive FoundryAutomationRefusalReason = "not_active"
	// FoundryAutomationRunEnded: the run already ended.
	FoundryAutomationRunEnded FoundryAutomationRefusalReason = "run_ended"
)

// FoundryAutomationRefusal is a write automations' rules refuse now.
type FoundryAutomationRefusal struct {
	Reason FoundryAutomationRefusalReason
	Detail string
}

func (r FoundryAutomationRefusal) Error() string { return string(r.Reason) + ": " + r.Detail }

// FoundryAutomationActor is who acts on an automation: a person, and whether
// they are an owner or admin of its workspace.
type FoundryAutomationActor struct {
	UserID  string
	Manager bool
}

// FoundryAutomationNotice is what an automation's email tells: the run's
// notify step and what its Assistant steps before it said, or the
// automation that paused itself.
type FoundryAutomationNotice struct {
	Automation *FoundryAutomation
	// Run, Node and Earlier are a notify step's: the run, the step, and the
	// run's steps that ended before it.
	Run     *FoundryAutomationRun
	Node    *FoundryAutomationRunNode
	Earlier []*FoundryAutomationRunNode
}
