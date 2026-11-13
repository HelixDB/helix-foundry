package model

import "time"

// FoundryResendSettle is how long a resend job waits, queued, for the
// requests that come with the one that queued it: Approve all settles its
// claims a few at a time, and each one that lands before the job is claimed
// puts it off again by this much, so they go in one job.
const FoundryResendSettle = 2 * time.Second

// FoundryResendReason is why a load owes the engine a resend
// (foundry_load_resends.reason). It decides what a resend may spend: an
// approval's spends nothing, as the document's saved extraction replays, and
// the load's decisions travel with an Extract that sends no document.
type FoundryResendReason string

// FoundryResendApproval is a person's approval of a claim the document's
// review holds back, which the load's decision list gained after the document
// was committed.
const FoundryResendApproval FoundryResendReason = "approval"

// FoundryResendDecisions is a change to the load's own list of value
// decisions since the engine was last sent it: a person chose a value, or
// took a choice back. It names no document (FoundryLoadResend.DocumentID is
// empty): every Extract of a load carries all of its value decisions, and
// the engine sets each one's value and withdraws those it is no longer sent,
// so any Extract that ends answers the request, one of no documents included.
const FoundryResendDecisions FoundryResendReason = "decisions"

// FoundryExtractDecisionsMax is the most decisions one Extract takes (the
// engine's Decisions). Every value decision of a load goes with every one of
// its Extracts, so an ontology takes no more value decisions than this
// (store.FoundryValueDecisionsFull): one more and no Extract of a load that
// holds them could be sent. At the limit itself an Extract still goes, but
// only with documents that have no decision of their own: a name settled or
// a claim approved in one makes it one too many.
const FoundryExtractDecisionsMax = 10000

// FoundryLoadResend is a document a load owes a resend, for a reason, or the
// load's own decisions (FoundryResendDecisions), which name no document.
type FoundryLoadResend struct {
	LoadID string
	// DocumentID is empty for FoundryResendDecisions.
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
	// Unsaid is a document an Extract of the job carried to a run that
	// reached its documents, which said nothing of its withdraws (contract
	// 4.16.0: it needed a model call, so got no event; or its event failed
	// and left one out). The engine decides each before it reads any
	// document, and may have refused one (differs, too_large), so nothing
	// here says it was taken back: the withdraws that wait stay waiting,
	// unconfirmed with Refused as their reason
	// (FoundryDecisionApplied.Unconfirmed). Its releases are refused as
	// Refused says, which Unsaid needs.
	Unsaid bool
}

// FoundryReleaseStatus is what became of a release or a value decision in a
// load (foundry_load_decisions.applied_status). The empty status is a decision
// the load copied when it was created and no job has reported on yet. A
// release ends written, refused or missed, as the engine says of each; a
// value decision ends applied, which is all a load can say of one, and one
// taken back is removed until the load is told. A withdraw waits as pending
// and ends withdrawn, not_written or refused.
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
	// FoundryReleaseWritten is a claim the engine wrote as a fact. From
	// contract 4.14.0, on a link that holds one value, a statement dated
	// after the approval takes effect (its quote's date, else its
	// document's, else the day it was decided), or a later approval, may
	// hold the link instead, the claim's quote kept as its evidence.
	FoundryReleaseWritten FoundryReleaseStatus = "written"
	// FoundryReleaseRefused is a claim that stays held; its reason says why.
	FoundryReleaseRefused FoundryReleaseStatus = "refused"
	// FoundryReleaseMissed is a decision this load holds no review for: its
	// claim has another key or place here.
	FoundryReleaseMissed FoundryReleaseStatus = "missed"
	// FoundryValueApplied is a value decision an Extract of the load carried
	// to its end. The engine reports nothing of a value decision, so this is
	// what is known: the load was sent it and the run succeeded. The engine
	// then holds it as a statement of the property from when it was decided,
	// which a document dated later still beats, and writes nothing for an
	// entity the load doesn't hold.
	FoundryValueApplied FoundryReleaseStatus = "applied"
	// FoundryValueRemoved is a value decision a person took back, of which
	// the load has yet to be told: the decision is gone, and its copy stays
	// in the load's list under this status until an Extract without it has
	// ended (the load's own request, FoundryResendDecisions). No Extract
	// sends it, so the engine withdraws the value. Until then the load still
	// holds that value, so a reader with partial access is withheld the
	// property as while the decision stood (the access rules read the
	// list). A resend job that failed leaves it with FoundryReleaseJobFailed,
	// and the end of the next job of the ontology sends the list again.
	FoundryValueRemoved FoundryReleaseStatus = "removed"
	// FoundryWithdrawn is a withdraw (FoundryReviewWithdraw) the engine
	// carried out: what the approval wrote no longer acts. Its claim is held
	// again with its review or, where what held it is gone (a conflict with a
	// document since removed), stays written as its document's own statement
	// with no approval to rank it.
	FoundryWithdrawn FoundryReleaseStatus = "withdrawn"
	// FoundryNotWritten is a withdraw of an approval of which the load holds
	// nothing: it was never written, was taken back already, or another
	// decision wrote the claim since.
	FoundryNotWritten FoundryReleaseStatus = "not_written"
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
	// FoundryReleaseTooLarge: the document, with the decisions that go with
	// it, is more than one Extract takes, so it was not sent. The engine
	// gives the same reason to a release it can't match among a document's
	// held claims: either way the document holds too much for the approval
	// to be applied.
	FoundryReleaseTooLarge = "too_large"
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
	// FoundryReleaseOutcomeWithdrawn and FoundryReleaseOutcomeNotWritten are
	// of a withdraw alone (contract 4.16.0), as are their statuses.
	FoundryReleaseOutcomeWithdrawn  FoundryReleaseOutcomeKind = "withdrawn"
	FoundryReleaseOutcomeNotWritten FoundryReleaseOutcomeKind = "not_written"
)

// FoundryReleaseOutcome is the engine's word on one release decision a job
// sent with a document. StatementHash is the hash the job sent: an approval
// of what the claim says now, saved while the job ran, is another, and the
// outcome is not of it.
type FoundryReleaseOutcome struct {
	DecisionID    string
	StatementHash string
	// Withdraw is a withdraw the job sent, not a release: the load's copy of
	// the decision is read under that kind, and a copy the person took back
	// while the job ran (a release then, a withdraw now) is not what the
	// engine speaks of.
	Withdraw bool
	Kind     FoundryReleaseOutcomeKind
	// Reason is the engine's reason for a refusal.
	Reason string
}

// FoundryDecisionApplied is what became of a decision in the ontology's
// current load.
type FoundryDecisionApplied struct {
	Status FoundryReleaseStatus
	// Reason is why a refused one stays held, or, on one that still waits,
	// why the engine's word on it is not known (Unconfirmed).
	Reason string
	// At is when a job reported it; nil while it waits.
	At *time.Time
	// WasWritten is of a withdraw: its load had recorded the release it takes
	// back written (foundry_load_decisions.was_written).
	WasWritten bool
	// TakesBack is of a release: the load may have written it
	// (store.foundryMayHaveWritten), so removing it takes it back.
	TakesBack bool
}

// MovesBack reports whether a withdraw that ends at status takes back what
// its load recorded written, which moves its document's counts back: a fact
// fewer and a review more, as they moved forward when the release was
// written. Only once, from waiting.
func (a FoundryDecisionApplied) MovesBack(status FoundryReleaseStatus) bool {
	return status == FoundryWithdrawn && a.WasWritten && a.Status.Waits()
}

// MovesForward reports whether a release that ends at status after o was
// written by a run whose word was lost: not_held of one settled here
// (After). The worker counts a fact only for a written word, so that run's
// document counts never moved; they move now, a fact more and a review
// fewer, so the take-back (MovesBack) moves back only what moved forward.
// Only where the document's counts add up what each run wrote: a fresh
// commit's counts are its own, so the store asks only of a run that kept
// the document's row (store.recordFoundryReleaseOutcomes).
func (a FoundryDecisionApplied) MovesForward(o FoundryReleaseOutcome, status FoundryReleaseStatus) bool {
	return !o.Withdraw && o.Kind == FoundryReleaseOutcomeNotHeld && status == FoundryReleaseWritten && a.Status.Waits()
}

// FoundryReleaseUnconfirmed is how a reader is told of an unconfirmed
// decision (FoundryDecisionApplied.Unconfirmed). No load stores it: the
// decision keeps the status it waits in.
const FoundryReleaseUnconfirmed = "unconfirmed"

// Unconfirmed reports a decision that still waits for the engine's word
// after the resend job that sent its document failed for good
// (FoundryReleaseJobFailed): whether its claim was written is not known. The
// engine may have written it before the job failed, and the event that said
// so was never recorded. No job is queued for it, so no reader waits; the
// next resend of its document, as approving it again queues, says what
// became of it (FoundryDecisionApplied.After), where it can: a carried one
// the engine holds no review for stays unconfirmed, as that is true both of
// one it wrote and of one this load never held. A withdraw is unconfirmed
// too, with the run's reason, where the run that carried it said nothing of
// it (FoundryResendEnd.Unsaid): whether the engine took it back or refused
// it is not known. Only job_failed ones are sent again at the next job's
// end. One whose document failed (FoundryReleaseDocumentFailed) goes with
// the document's next resend, which may read it. One whose document's saved
// reading doesn't replay (FoundryReleaseNotReplayed) gets no word while
// this load is current: every resend costs nothing, and that document needs
// a model call. Sent again it would get none either, so nothing queues one
// for it; the next load holds no copy of the approval, and won't write it.
func (a FoundryDecisionApplied) Unconfirmed() bool {
	return a.Status.Waits() && a.Reason != ""
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
//
// A withdraw ends as the engine says: withdrawn, not_written or refused;
// another word, which only a release gets, is refused with its name as the
// reason, as an unknown one is. not_written of an approval its load had
// recorded written (WasWritten) is the take-back done already, by a run that
// stopped, or whose document needed a model call, before its event said so
// (the engine takes withdraws back before it reads any document): withdrawn.
func (a FoundryDecisionApplied) After(o FoundryReleaseOutcome) (status FoundryReleaseStatus, reason string, ok bool) {
	if o.Withdraw {
		switch o.Kind {
		case FoundryReleaseOutcomeWithdrawn:
			return FoundryWithdrawn, "", true
		case FoundryReleaseOutcomeNotWritten:
			if a.WasWritten {
				return FoundryWithdrawn, "", true
			}
			return FoundryNotWritten, "", true
		case FoundryReleaseOutcomeRefused:
			return FoundryReleaseRefused, o.Reason, true
		}
		return FoundryReleaseRefused, string(o.Kind), true
	}
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
