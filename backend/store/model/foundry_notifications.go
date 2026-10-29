package model

import "time"

// FoundryNotificationKind is what an email Foundry sends a person tells of.
type FoundryNotificationKind string

const (
	// FoundryNotifyOntologyReady tells the requester of an ontology's first
	// draft (OntologyGenerationInput.LoadWhenReady) how it ended: loaded,
	// loaded up to the month's budget, drafted and current but its load
	// failed, not drafted, or drafted and waiting for review. Its subject is
	// the generation.
	FoundryNotifyOntologyReady FoundryNotificationKind = "ontology_ready"
	// FoundryNotifySampleFollowUp asks a person who watched the sample
	// draft an ontology, and a day later has no data of their own in its
	// workspace, to try Foundry on theirs. Its subject is the person, so
	// each gets one at most, ever.
	FoundryNotifySampleFollowUp FoundryNotificationKind = "sample_follow_up"
)

// FoundryNotificationStatus is where an email is in its sending.
type FoundryNotificationStatus string

const (
	// FoundryNotificationPending waits to be sent, from NextAttemptAt.
	FoundryNotificationPending FoundryNotificationStatus = "pending"
	// FoundryNotificationSent was handed to the host's mail, once; it is
	// never sent again.
	FoundryNotificationSent FoundryNotificationStatus = "sent"
	// FoundryNotificationSkipped is never sent: its person, ontology or
	// workspace is gone, or it had nothing to say. LastError says why.
	FoundryNotificationSkipped FoundryNotificationStatus = "skipped"
	// FoundryNotificationFailed is never sent: the worker's last attempt
	// failed, as LastError says.
	FoundryNotificationFailed FoundryNotificationStatus = "failed"
)

// FoundryNotification is one email Foundry sends a person, about one
// subject (FoundryNotificationKind).
type FoundryNotification struct {
	ID          string
	Kind        FoundryNotificationKind
	SubjectID   string
	WorkspaceID string
	UserID      string
	// OntologyID is set exactly for FoundryNotifyOntologyReady.
	OntologyID *string
	Status     FoundryNotificationStatus
	// Attempts counts the claims made of it, the one held included.
	Attempts      int
	NextAttemptAt time.Time
	// ClaimToken holds a pending email while a worker sends it, until
	// ClaimedUntil; both are nil otherwise.
	ClaimToken   *string
	ClaimedUntil *time.Time
	LastError    *string
	CreatedAt    time.Time
	// SentAt is set exactly once it is sent.
	SentAt *time.Time
}

// FoundryNotificationOutcome ends a claim: FoundryNotificationDelivered,
// FoundryNotificationSkip, FoundryNotificationRetry or
// FoundryNotificationGiveUp, nothing else, as the unexported method seals it.
type FoundryNotificationOutcome interface {
	notificationOutcome()
}

// FoundryNotificationDelivered records the email sent.
type FoundryNotificationDelivered struct{}

// FoundryNotificationSkip ends the email unsent, for good, for Reason.
type FoundryNotificationSkip struct {
	Reason string
}

// FoundryNotificationRetry gives the email back to the queue until At,
// after a failure to send it that Error says.
type FoundryNotificationRetry struct {
	At    time.Time
	Error string
}

// FoundryNotificationGiveUp ends the email unsent after its last attempt,
// which failed as Error says.
type FoundryNotificationGiveUp struct {
	Error string
}

func (FoundryNotificationDelivered) notificationOutcome() {}
func (FoundryNotificationSkip) notificationOutcome()      {}
func (FoundryNotificationRetry) notificationOutcome()     {}
func (FoundryNotificationGiveUp) notificationOutcome()    {}
