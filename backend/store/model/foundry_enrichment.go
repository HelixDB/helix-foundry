package model

import (
	"fmt"
	"time"
)

// FoundryEnrichmentCredits is what one returned profile costs, accepted or
// held, and what an unanswered call is booked at.
const FoundryEnrichmentCredits = 20

// FoundryEnrichmentConsent is one change of a workspace's People enrichment
// setting. Changes are kept as history: the newest is the setting.
type FoundryEnrichmentConsent struct {
	ID          string
	WorkspaceID string
	// Enabled is the owner's consent to look the workspace's people up.
	Enabled bool
	// MonthlyCreditCap bounds the credits the workspace's loads charge per
	// calendar month (UTC).
	MonthlyCreditCap int64
	// NoticeVersion names the notice the owner was shown.
	NoticeVersion string
	UserID        string
	CreatedAt     time.Time
}

// FoundryEnrichmentStatus is where a load's enrichment step is.
type FoundryEnrichmentStatus string

const (
	// FoundryEnrichmentPreparing is a step the graph worker is planning.
	FoundryEnrichmentPreparing FoundryEnrichmentStatus = "preparing"
	// FoundryEnrichmentQueued waits for the enrichment worker until claim_by.
	FoundryEnrichmentQueued FoundryEnrichmentStatus = "queued"
	// FoundryEnrichmentRunning is a step the enrichment worker holds.
	FoundryEnrichmentRunning FoundryEnrichmentStatus = "running"
	// FoundryEnrichmentDone looked up everyone the plan called.
	FoundryEnrichmentDone FoundryEnrichmentStatus = "done"
	// FoundryEnrichmentPartial stopped early (StoppedBy says why) and
	// published what it had.
	FoundryEnrichmentPartial FoundryEnrichmentStatus = "partial"
	// FoundryEnrichmentOff ran without the workspace's consent: no profiles.
	FoundryEnrichmentOff FoundryEnrichmentStatus = "off"
	// FoundryEnrichmentFailed could not plan or publish.
	FoundryEnrichmentFailed FoundryEnrichmentStatus = "failed"
	// FoundryEnrichmentCancelled ended because consent was turned off, or
	// the ontology or workspace was deleted: nothing is published.
	FoundryEnrichmentCancelled FoundryEnrichmentStatus = "cancelled"
)

// Ended reports a step that will change no more.
func (s FoundryEnrichmentStatus) Ended() bool {
	return s != FoundryEnrichmentPreparing && s != FoundryEnrichmentQueued && s != FoundryEnrichmentRunning
}

// FoundryEnrichmentStop is why a partial step stopped.
type FoundryEnrichmentStop string

const (
	// FoundryEnrichmentStopCap is the monthly credit cap, the workspace's
	// or Helix's.
	FoundryEnrichmentStopCap FoundryEnrichmentStop = "cap"
	// FoundryEnrichmentStopProviderCredits is the provider refusing the key
	// or the credits (402, 401, 403), which halts lookups for a while.
	FoundryEnrichmentStopProviderCredits FoundryEnrichmentStop = "provider_credits"
	// FoundryEnrichmentStopProvider is ten failed calls in a row.
	FoundryEnrichmentStopProvider FoundryEnrichmentStop = "provider"
	// FoundryEnrichmentStopTime is the step's deadline.
	FoundryEnrichmentStopTime FoundryEnrichmentStop = "time"
	// FoundryEnrichmentStopBusy is a step no worker claimed before claim_by.
	FoundryEnrichmentStopBusy FoundryEnrichmentStop = "busy"
	// FoundryEnrichmentStopStopped is a user's Stop.
	FoundryEnrichmentStopStopped FoundryEnrichmentStop = "stopped"
	// FoundryEnrichmentStopPro is a workspace off Foundry Pro.
	FoundryEnrichmentStopPro FoundryEnrichmentStop = "pro"
	// FoundryEnrichmentStopFlag is Helix's operators' switch, off.
	FoundryEnrichmentStopFlag FoundryEnrichmentStop = "flag"
)

// FoundryEnrichmentEnd is how a step ends. Construct it with
// EndFoundryEnrichment or PartialFoundryEnrichment, so a partial end always
// says why and no other end does.
type FoundryEnrichmentEnd struct {
	status    FoundryEnrichmentStatus
	stoppedBy *FoundryEnrichmentStop
}

// EndFoundryEnrichment is an end other than partial. It panics on a status
// that is not an end, or on partial: a programming error.
func EndFoundryEnrichment(status FoundryEnrichmentStatus) FoundryEnrichmentEnd {
	if !status.Ended() || status == FoundryEnrichmentPartial {
		panic(fmt.Sprintf("foundry enrichment end %q is not an end other than partial", status))
	}
	return FoundryEnrichmentEnd{status: status}
}

// PartialFoundryEnrichment is a partial end, stopped by why.
func PartialFoundryEnrichment(why FoundryEnrichmentStop) FoundryEnrichmentEnd {
	return FoundryEnrichmentEnd{status: FoundryEnrichmentPartial, stoppedBy: &why}
}

// Status is the end's status.
func (e FoundryEnrichmentEnd) Status() FoundryEnrichmentStatus { return e.status }

// StoppedBy is why a partial end stopped; nil for any other.
func (e FoundryEnrichmentEnd) StoppedBy() *FoundryEnrichmentStop { return e.stoppedBy }

// FoundryEnrichmentApplied is the file a load's Ingest reads as
// foundry_enrichment, chosen once per load. An empty Key is the zero-byte
// file, which reads as no profiles.
type FoundryEnrichmentApplied struct {
	Key string
	// SHA256 is the lowercase hex digest of the object's bytes.
	SHA256 string
}

// FoundryEnrichmentNone is the zero-byte file: no profiles.
var FoundryEnrichmentNone = FoundryEnrichmentApplied{Key: "", SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}

// FoundryEnrichmentCounts are a step's numbers, stored as JSON on its row.
type FoundryEnrichmentCounts struct {
	// People is how many entities of the type Clues read.
	People int64 `json:"people"`
	// NoName have no full name: none, or fewer than two letter tokens.
	NoName int64 `json:"noName"`
	// NoClue have no usable email or LinkedIn URL left.
	NoClue int64 `json:"noClue"`
	// Shared had a clue another entity also holds, dropped for both.
	Shared int64 `json:"shared"`
	// Skipped is everyone the plan left out (NoName and NoClue, as planned).
	Skipped int64 `json:"skipped"`
	// Carried is everyone whose earlier result was reused, at no cost.
	Carried int64 `json:"carried"`
	// ToLookUp is everyone the plan calls.
	ToLookUp int64 `json:"toLookUp"`
	// LookedUp is everyone called whose call ended.
	LookedUp int64 `json:"lookedUp"`
	// Found, Held, NotFound and Uncertain are what this step's calls found.
	Found     int64 `json:"found"`
	Held      int64 `json:"held"`
	NotFound  int64 `json:"notFound"`
	Uncertain int64 `json:"uncertain"`
	// Profiles is how many rows the published profiles hold: Found plus the
	// accepted rows carried.
	Profiles int64 `json:"profiles"`
	// Credits is what the step charged, uncertain calls included.
	Credits int64 `json:"credits"`
	// Applied and Unmatched are what the load's Ingest bound and could not.
	Applied   int64 `json:"applied"`
	Unmatched int64 `json:"unmatched"`
}

// FoundryLoadEnrichment is a load's enrichment step.
type FoundryLoadEnrichment struct {
	LoadID      string
	WorkspaceID string
	OntologyID  string
	// JobID is the load job, whose attempts prepare, park and wake on it.
	JobID  string
	Status FoundryEnrichmentStatus
	// StoppedBy is set exactly on a partial step.
	StoppedBy *FoundryEnrichmentStop
	// ClaimBy is when a queued step stops waiting for a worker.
	ClaimBy *time.Time
	// Deadline is when a claimed step stops calling.
	Deadline   *time.Time
	LeaseOwner *string
	LeaseUntil *time.Time
	// BatchCursor is the next batch to run; InflightCredits what the batch
	// in flight (BatchCursor) may charge, 0 when none is.
	BatchCursor     int
	ReservedCredits int64
	InflightCredits int64
	Counts          FoundryEnrichmentCounts
	PlanKey         *string
	CarriedKey      *string
	ProfilesKey     *string
	ResultsKey      *string
	// Applied is the file the load's Ingest reads, nil until chosen.
	Applied *FoundryEnrichmentApplied
	// StopRequested is the user who asked to stop.
	StopRequested   *string
	CancelRequested bool
	PurgedAt        *time.Time
	CreatedAt       time.Time
	ClaimedAt       *time.Time
	EndedAt         *time.Time
	PublishedAt     *time.Time
	CallsDeletedAt  *time.Time
}

// FoundryEnrichmentSignals are what a running step's renewal, its re-checks
// and each of its batches as it begins read: a user's Stop, a cancel
// (consent off, the ontology deleted), and the provider's halt.
type FoundryEnrichmentSignals struct {
	Stop   bool
	Cancel bool
	// Halted is the provider's halt in force by the database's clock,
	// recorded by any step or an operator: no lookup runs anywhere until it
	// ends.
	Halted bool
}

// FoundryEnrichmentBegun is what beginning a step's batch came to: N of its
// calls in flight, or none, because the step must end first (a Stop, a
// cancel, the provider halted) or the caps are reached.
type FoundryEnrichmentBegun struct {
	// N is how many of the batch's calls are in flight; 0 when none is.
	N int
	FoundryEnrichmentSignals
}

// FoundryEnrichmentChecks are a running step's periodic re-checks.
type FoundryEnrichmentChecks struct {
	FoundryEnrichmentSignals
	// Consent is the workspace's current setting; nil when never set.
	Consent *FoundryEnrichmentConsent
	// Gone means the ontology or the workspace is deleted or being deleted.
	Gone bool
}

// FoundryEnrichmentSettings are what a workspace's People enrichment
// settings show.
type FoundryEnrichmentSettings struct {
	// Consent is the newest change; nil when never set, which is off.
	Consent *FoundryEnrichmentConsent
	// Enabled is the change that turned it on, while it is on: the first
	// since it was last turned off. Nil while off.
	Enabled *FoundryEnrichmentConsent
	// Ledger is the month's credits so far.
	Ledger FoundryEnrichmentLedger
	// PurgesPending counts the workspace's ontologies whose enriched values
	// a clean load has still to remove.
	PurgesPending int
}

// FoundryEnrichmentCaps bound a month's credits: the workspace's cap and
// Helix's across workspaces, counted from Since, the month's start.
type FoundryEnrichmentCaps struct {
	Workspace int64
	Global    int64
	Since     time.Time
}

// FoundryEnrichmentLedger is a month's credits so far: charged, and reserved
// by live steps, for one workspace and across all.
type FoundryEnrichmentLedger struct {
	WorkspaceCharged  int64
	WorkspaceReserved int64
	GlobalCharged     int64
	GlobalReserved    int64
}

// Headroom is what the caps leave for a new reservation.
func (l FoundryEnrichmentLedger) Headroom(caps FoundryEnrichmentCaps) int64 {
	return max(min(caps.Workspace-l.WorkspaceCharged-l.WorkspaceReserved, caps.Global-l.GlobalCharged-l.GlobalReserved), 0)
}

// Unspent is what the caps leave once the month's charges are counted,
// whatever running steps reserved: a reservation is spent only as its
// batches are charged, and a step not yet claimed reserves from Headroom
// once it is, after the step its workspace runs has ended.
func (l FoundryEnrichmentLedger) Unspent(caps FoundryEnrichmentCaps) int64 {
	return max(min(caps.Workspace-l.WorkspaceCharged, caps.Global-l.GlobalCharged), 0)
}

// FoundryEnrichmentCharge is one batch's settlement.
type FoundryEnrichmentCharge struct {
	Batch int
	// Credits is for returned profiles; UncertainCredits for calls that got
	// no answer, which may have been charged.
	Credits          int64
	UncertainCredits int64
	// Remaining is the most the step's calls after this batch may cost: its
	// reservation is cut to it, so what the batch's free answers left
	// reserved goes back to the caps.
	Remaining int64
}

// FoundryEnrichmentPublished are the files a step published.
type FoundryEnrichmentPublished struct {
	ProfilesKey string
	ResultsKey  string
}

// FoundryEnrichmentProvider is a provider's halt.
type FoundryEnrichmentProvider struct {
	Provider    string
	HaltedUntil *time.Time
	HaltCode    *string
}

// Halted reports a halt still in force at now.
func (p *FoundryEnrichmentProvider) Halted(now time.Time) bool {
	return p != nil && p.HaltedUntil != nil && now.Before(*p.HaltedUntil)
}

// FoundryEnrichmentPurge is an ontology whose enriched values a clean load
// removes, after consent was turned off.
type FoundryEnrichmentPurge struct {
	OntologyID  string
	WorkspaceID string
	RequestedAt time.Time
	Attempts    int
	LastLoadID  *string
	DoneAt      *time.Time
}

// FoundryEnrichmentQueue is a prepared step handed to the enrichment worker.
type FoundryEnrichmentQueue struct {
	PlanKey    string
	CarriedKey string
	Counts     FoundryEnrichmentCounts
	// ClaimBy is when it stops waiting for a worker.
	ClaimBy time.Time
}

// FoundryEnrichmentPrepared is a step the graph worker ended while preparing
// it: no lookups, so it applies what it says at once. Keys it never wrote
// are empty.
type FoundryEnrichmentPrepared struct {
	End        FoundryEnrichmentEnd
	PlanKey    string
	CarriedKey string
	ResultsKey string
	Counts     FoundryEnrichmentCounts
	Applied    FoundryEnrichmentApplied
}

// FoundryEnrichmentSweep is what the sweep deletes of one ended step: each
// non-nil key, the batch files results/<b> with Plan, and the batch files
// calls/<b> with Calls, for every batch below Batches. With WholeLayout
// (the step is purged, or failed before any worker claimed it) it loses
// every file of its load's layout, named on its row or not: an attempt may
// have written one before the row named it.
type FoundryEnrichmentSweep struct {
	LoadID      string
	WorkspaceID string
	OntologyID  string
	Batches     int
	Plan        *string
	Carried     *string
	Profiles    *string
	Results     *string
	Calls       bool
	WholeLayout bool
}

// FoundryEnrichmentPurgeStep is what advancing a purge did.
type FoundryEnrichmentPurgeStep string

const (
	// FoundryEnrichmentPurgeDone found the ontology's current load clean,
	// or the ontology deleted; the loads that applied purged profiles are
	// being deleted.
	FoundryEnrichmentPurgeDone FoundryEnrichmentPurgeStep = "done"
	// FoundryEnrichmentPurgeWaiting waits for a live job, or for the day
	// between attempts.
	FoundryEnrichmentPurgeWaiting FoundryEnrichmentPurgeStep = "waiting"
	// FoundryEnrichmentPurgeQueued queued a clean load.
	FoundryEnrichmentPurgeQueued FoundryEnrichmentPurgeStep = "queued"
	// FoundryEnrichmentPurgeExhausted gave up after the last attempt: an
	// operator must look.
	FoundryEnrichmentPurgeExhausted FoundryEnrichmentPurgeStep = "exhausted"
)
