package model

import "time"

// FoundryResendSettle is how long a resend job waits, queued, for the
// requests that come with the one that queued it: Approve all settles its
// claims a few at a time, and each one that lands before the job is claimed
// puts it off again by this much, so they go in one job.
const FoundryResendSettle = 2 * time.Second

// FoundryResendReason is why a load owes a document a resend
// (foundry_load_resends.reason). It decides what a resend may spend: an
// approval's spends nothing, as the document's saved extraction replays.
type FoundryResendReason string

// FoundryResendApproval is a person's approval of a claim the document's
// review holds back, which the load's decision list gained after the document
// was committed.
const FoundryResendApproval FoundryResendReason = "approval"

// FoundryLoadResend is a document a load owes a resend, for a reason.
type FoundryLoadResend struct {
	LoadID     string
	DocumentID string
	Reason     FoundryResendReason
	// Requests is how often the row was owed: EndFoundryLoadResend ends only
	// the request a job read, so one that came during the job's run stays owed.
	Requests int
	// Document is the document to send, with the set it joined the load with:
	// nil when it can't be sent, and Unsendable says why.
	Document *FoundryDocument
	// Unsendable is why a document can't be sent, empty when it can:
	// FoundryReleaseDocumentGone (it is no longer uploaded, or never joined
	// the load) or FoundryReleaseDocumentUnread (the load holds it, but no
	// extraction of it to replay).
	Unsendable string
}

// FoundryResendEnd ends a document's resend, as the job that sent it found.
type FoundryResendEnd struct {
	DocumentID string
	Reason     FoundryResendReason
	Requests   int
	// Refused, when set, is why the document's decisions that still wait were
	// not applied: one of the FoundryRelease… reasons the backend gives.
	Refused string
}

// FoundryReleaseStatus is what became of a release decision in a load
// (foundry_load_decisions.applied_status). The empty status is a decision the
// load copied when it was created and no job has reported on yet.
type FoundryReleaseStatus string

const (
	// FoundryReleasePending was settled against a review of this load, and
	// the engine has yet to say what became of it: a resend of its document
	// is owed, unless the job that sent it failed
	// (FoundryDecisionApplied.Unconfirmed).
	FoundryReleasePending FoundryReleaseStatus = "pending"
	// FoundryReleaseCarried was saved while another load was current, and
	// joined this load's list when it became current: no review of this load
	// was read for it, and the engine has yet to say what became of it, as
	// with a pending one.
	FoundryReleaseCarried FoundryReleaseStatus = "carried"
	// FoundryReleaseWritten is a claim the engine wrote as a fact.
	FoundryReleaseWritten FoundryReleaseStatus = "written"
	// FoundryReleaseRefused is a claim that stays held; its reason says why.
	FoundryReleaseRefused FoundryReleaseStatus = "refused"
	// FoundryReleaseMissed is a decision this load holds no review for: its
	// claim has another key or place here.
	FoundryReleaseMissed FoundryReleaseStatus = "missed"
)

// Waits reports a decision the engine has yet to say anything of.
func (s FoundryReleaseStatus) Waits() bool {
	return s == FoundryReleasePending || s == FoundryReleaseCarried
}

// The reasons the backend gives a refused release; the others are the
// engine's (ontologyagent.ReleaseOutcome.Reason), stored as they came.
const (
	// FoundryReleaseNotReplayed: the document's saved extraction didn't
	// replay, so applying the approval would have cost a model call.
	FoundryReleaseNotReplayed = "not_replayed"
	// FoundryReleaseDocumentGone: the document left the load or was deleted.
	FoundryReleaseDocumentGone = "document_gone"
	// FoundryReleaseDocumentUnread: the load holds the document, but its row
	// no longer says it is extracted, as when a failure was recorded over the
	// row its commit wrote: there is no saved extraction to replay it from.
	FoundryReleaseDocumentUnread = "document_unread"
	// FoundryReleaseDocumentFailed: the engine failed the document.
	FoundryReleaseDocumentFailed = "document_failed"
	// FoundryReleaseJobFailed: the resend job that sent the decision ended
	// failed before the engine's word on it was recorded. It refuses nothing:
	// the engine may have written the claim, so the decision keeps its
	// status, still waiting, with this as its reason
	// (FoundryDecisionApplied.Unconfirmed).
	FoundryReleaseJobFailed = "job_failed"
	// FoundryReleaseUnanswered: the engine ended the document a resend sent
	// and said nothing of this decision, which the contract has it do.
	FoundryReleaseUnanswered = "unanswered"
)

// FoundryReleaseOutcomeKind is what the engine did with a release decision of
// a document it was sent (the document event's releases[].outcome.kind).
type FoundryReleaseOutcomeKind string

const (
	FoundryReleaseOutcomeWritten FoundryReleaseOutcomeKind = "written"
	FoundryReleaseOutcomeRefused FoundryReleaseOutcomeKind = "refused"
	// FoundryReleaseOutcomeMissed: the run committed the document, and its
	// commit held no review at the decision's key and place.
	FoundryReleaseOutcomeMissed FoundryReleaseOutcomeKind = "missed"
	// FoundryReleaseOutcomeNotHeld: the load held the document already, with
	// no review at the decision's key and place: one a release wrote before,
	// or a claim this load never held there.
	FoundryReleaseOutcomeNotHeld FoundryReleaseOutcomeKind = "not_held"
)

// FoundryReleaseOutcome is the engine's word on one release decision a job
// sent with a document. StatementHash is the hash the job sent: an approval
// of what the claim says now, saved while the job ran, is another, and the
// outcome is not of it.
type FoundryReleaseOutcome struct {
	DecisionID    string
	StatementHash string
	Kind          FoundryReleaseOutcomeKind
	// Reason is the engine's reason for a refusal.
	Reason string
}

// FoundryDecisionApplied is what became of a decision in the ontology's
// current load.
type FoundryDecisionApplied struct {
	Status FoundryReleaseStatus
	// Reason is why a refused one stays held, or FoundryReleaseJobFailed on
	// one that still waits (Unconfirmed).
	Reason string
	// At is when a job reported it; nil while it waits.
	At *time.Time
}

// FoundryReleaseUnconfirmed is how a reader is told of an unconfirmed
// decision (FoundryDecisionApplied.Unconfirmed). No load stores it: the
// decision keeps the status it waits in.
const FoundryReleaseUnconfirmed = "unconfirmed"

// Unconfirmed reports a decision that still waits for the engine's word
// after the resend job that sent its document failed for good: whether its
// claim was written is not known. The engine may have written it before the
// job failed, and the event that said so was never recorded. No job is
// queued for it, so no reader waits; the next resend of its document, as
// approving it again queues, says what became of it
// (FoundryDecisionApplied.After), where it can: a carried one the engine
// holds no review for stays unconfirmed, as that is true both of one it
// wrote and of one this load never held.
func (a FoundryDecisionApplied) Unconfirmed() bool {
	return a.Status.Waits() && a.Reason == FoundryReleaseJobFailed
}

// After is the status and reason a decision at a has once the engine said o
// of it. A release sent again finds no review (not_held): for a decision
// settled against a review of this load (pending) the review has gone since,
// which only a release does in place, so it was written, as when the event
// that said so was lost; for a carried one no review of this load was ever
// read, so the load holds none (missed), unless the job that sent it failed
// (Unconfirmed): the engine may have written it before that, which leaves no
// review either, so it stays unconfirmed; any other status is what an
// earlier event said, and stays: ok is false, and nothing changes. An outcome
// the backend doesn't know wrote nothing it can vouch for: the decision is
// refused, with the outcome's name as its reason, so none waits for ever.
func (a FoundryDecisionApplied) After(o FoundryReleaseOutcome) (status FoundryReleaseStatus, reason string, ok bool) {
	switch o.Kind {
	case FoundryReleaseOutcomeWritten:
		return FoundryReleaseWritten, "", true
	case FoundryReleaseOutcomeRefused:
		return FoundryReleaseRefused, o.Reason, true
	case FoundryReleaseOutcomeMissed:
		return FoundryReleaseMissed, "", true
	case FoundryReleaseOutcomeNotHeld:
		switch {
		case a.Status == FoundryReleasePending:
			return FoundryReleaseWritten, "", true
		case a.Status == FoundryReleaseCarried && !a.Unconfirmed():
			return FoundryReleaseMissed, "", true
		}
		return a.Status, a.Reason, false
	}
	return FoundryReleaseRefused, string(o.Kind), true
}
