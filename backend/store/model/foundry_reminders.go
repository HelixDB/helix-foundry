package model

import "time"

// FoundryReminderStatus is where an address's reminders are.
type FoundryReminderStatus string

const (
	// FoundryReminderScheduled has a reminder due at NextSendAt.
	FoundryReminderScheduled FoundryReminderStatus = "scheduled"
	// FoundryReminderDone sent every reminder of its schedule.
	FoundryReminderDone FoundryReminderStatus = "done"
	// FoundryReminderStopped was stopped before its schedule ended, as
	// StopReason says.
	FoundryReminderStopped FoundryReminderStatus = "stopped"
	// FoundryReminderFailed gave up on a reminder after its last attempt
	// failed, as LastError says, and sends no more.
	FoundryReminderFailed FoundryReminderStatus = "failed"
)

// FoundryReminderStoppedSignedIn stops an address's reminders once its
// person signed in to Foundry: they have tried it.
const FoundryReminderStoppedSignedIn = "signed_in"

// FoundryReminder is one address's reminders to try Foundry on a computer,
// which a person asked for on the landing page, from a phone
// (foundry_reminders): a schedule of reminders, of which Sent went.
type FoundryReminder struct {
	ID string
	// Email is the address, trimmed and lowercased (reminder.Normalize).
	Email string
	// TimeZone is the IANA zone the schedule keeps to; UTC for one the
	// person's browser named that Foundry doesn't know.
	TimeZone string
	// Consent is what the person agreed to (reminder.Consent).
	Consent string
	Status  FoundryReminderStatus
	// Sent counts the reminders of the schedule that went.
	Sent int
	// NextSendAt is set exactly while it is scheduled.
	NextSendAt *time.Time
	// Attempts counts the claims of the reminder due, the one held
	// included: each one sent starts the next from 0.
	Attempts int
	// ClaimToken holds a scheduled reminder while a worker sends it, until
	// ClaimedUntil; both are nil otherwise. A schedule stopped while one is
	// sent keeps the claim, so the worker still records the send.
	ClaimToken   *string
	ClaimedUntil *time.Time
	LastError    *string
	// RequestedAt is when its current schedule was asked for: CreatedAt,
	// or when an ended one was asked for again.
	RequestedAt time.Time
	CreatedAt   time.Time
	// LastSentAt is when its last reminder went, of this schedule or one
	// before it; nil until one has.
	LastSentAt *time.Time
	// StoppedAt and StopReason are set exactly once it is stopped.
	StoppedAt  *time.Time
	StopReason *string
}

// FoundryReminderRequest is a person's request for reminders at an
// address.
type FoundryReminderRequest struct {
	// ID is the row's if the address has none yet.
	ID string
	// Email is reminder.Normalize's; TimeZone and Consent are kept as given.
	Email, TimeZone, Consent string
	// Client is the site's key for the visitor who asked, empty for none;
	// MaxNewPerClientPerHour (foundry.reminders.max_new_per_client_per_hour)
	// bounds the schedules one client starts an hour.
	Client                 string
	MaxNewPerClientPerHour int
	// MaxNewPerHour is foundry.reminders.max_new_per_hour.
	MaxNewPerHour int
	// Schedule is when a new schedule's first reminder goes, given when the
	// address's last reminder went (nil for none): the next weekday 09:00
	// in its zone, but no sooner than foundry.reminders.min_gap_days after
	// the last.
	Schedule func(lastSentAt *time.Time) time.Time
}

// FoundryReminderAnswer is what a request for reminders came to.
type FoundryReminderAnswer struct {
	NextSendAt time.Time
	// Again is true when the address already had a schedule running, which
	// the request left as it was.
	Again bool
}

// FoundryReminderOutcome ends a claim of an address's reminder:
// FoundryReminderSent, FoundryReminderRetry or FoundryReminderGiveUp,
// nothing else, as the unexported method seals it.
type FoundryReminderOutcome interface {
	reminderOutcome()
}

// FoundryReminderSent records the reminder sent. Next is when the next one
// goes; nil ends the schedule, done.
type FoundryReminderSent struct {
	Next *time.Time
}

// FoundryReminderRetry gives the reminder back to the schedule until At,
// after a failure to send it that Error says.
type FoundryReminderRetry struct {
	At    time.Time
	Error string
}

// FoundryReminderGiveUp ends the address's reminders unsent after the last
// attempt at one, which failed as Error says.
type FoundryReminderGiveUp struct {
	Error string
}

func (FoundryReminderSent) reminderOutcome()   {}
func (FoundryReminderRetry) reminderOutcome()  {}
func (FoundryReminderGiveUp) reminderOutcome() {}
