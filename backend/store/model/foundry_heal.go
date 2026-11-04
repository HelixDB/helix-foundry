package model

import (
	"encoding/json"
	"slices"
	"time"
)

// FoundryHealStatus is where a self-healing finding stands.
type FoundryHealStatus string

const (
	// FoundryHealOpen is a finding the latest complete audit found, shown
	// while it is among the ontology's top open ones (FoundryHealShown).
	FoundryHealOpen FoundryHealStatus = "open"
	// FoundryHealDismissed is hidden by a person until an audit finds it
	// with twice the support it had then, or under a spelling it didn't
	// hold (FoundryHealFinding.Next).
	FoundryHealDismissed FoundryHealStatus = "dismissed"
	// FoundryHealSuperseded is an open finding a complete audit no longer
	// found: its target went, so a fix or a new version took it away. Found
	// again, it is open again.
	FoundryHealSuperseded FoundryHealStatus = "superseded"
)

// FoundryHealShownLimit is how many open findings an ontology shows, by
// support: no flood.
const FoundryHealShownLimit = 20

// FoundryHealMinSources is how many documents a finding backed by reviews
// needs before it is stored and shown: one document's reviews are no
// pattern. A row finding has no documents (Sources 0) and needs none.
const FoundryHealMinSources = 2

// FoundryHealFinding is one row of foundry_heal_findings.
type FoundryHealFinding struct {
	OntologyID  string
	Fingerprint string
	// Kind is the signal's kind as the engine names it, which a later
	// contract may add to.
	Kind   string
	Status FoundryHealStatus
	// LoadID is the load Support was counted over; BaseVersionID its
	// version.
	LoadID        string
	BaseVersionID string
	// Finding is the engine's Finding with its backing summed over the
	// load's pages. A dismissed finding keeps it as it was dismissed.
	Finding json.RawMessage
	// Support is the reviews behind it (a row finding's rows); Sources the
	// documents, 0 exactly for a row finding.
	Support int64
	Sources int64
	// Dismissal is who dismissed it and its support then, set while it is
	// dismissed.
	Dismissal *FoundryHealDismissal
}

// FoundryHealDismissal is a person's dismissal of a finding.
type FoundryHealDismissal struct {
	ByUserID string
	// Support is the finding's support when it was dismissed: it shows
	// again at twice that.
	Support int64
}

// FoundryHealSeen is a finding a complete audit of a load found, summed
// over its pages.
type FoundryHealSeen struct {
	Fingerprint string
	Kind        string
	Finding     json.RawMessage
	Support     int64
	Sources     int64
}

// Shown reports whether a finding is backed enough to store and show: by
// FoundryHealMinSources documents, or by rows.
func (s FoundryHealSeen) Shown() bool {
	return s.Sources == 0 || s.Sources >= FoundryHealMinSources
}

// Next is what stored becomes once a complete audit of loadID (a load of
// versionID) found seen: the row to write, or nil to write none. stored is
// nil for a finding no row holds yet.
//
//   - A new finding is stored open when it is Shown; a thinner one isn't
//     stored.
//   - An open or superseded one is open, with seen's finding and backing.
//   - A dismissed one stays dismissed, keeping the finding it was dismissed
//     with, until seen has twice the support it had then, or a spelling it
//     didn't hold (FoundryHealSpellings): then it is open again, its
//     dismissal gone.
//   - Any other status is a fix's, which a later change drafts and checks:
//     it takes seen's backing and keeps the rest.
func (stored *FoundryHealFinding) Next(seen FoundryHealSeen, loadID, versionID string) *FoundryHealFinding {
	open := &FoundryHealFinding{
		Fingerprint: seen.Fingerprint, Kind: seen.Kind, Status: FoundryHealOpen, LoadID: loadID, BaseVersionID: versionID,
		Finding: seen.Finding, Support: seen.Support, Sources: seen.Sources,
	}
	if stored == nil {
		if !seen.Shown() {
			return nil
		}
		return open
	}
	open.OntologyID = stored.OntologyID
	switch stored.Status {
	case FoundryHealOpen, FoundryHealSuperseded:
		return open
	case FoundryHealDismissed:
		dismissedWith := FoundryHealSpellings(stored.Finding)
		respelled := slices.ContainsFunc(FoundryHealSpellings(seen.Finding), func(s string) bool { return !slices.Contains(dismissedWith, s) })
		if stored.Dismissal == nil || seen.Support >= 2*stored.Dismissal.Support || respelled {
			return open
		}
	}
	kept := *stored
	kept.LoadID, kept.BaseVersionID, kept.Support, kept.Sources = loadID, versionID, seen.Support, seen.Sources
	return &kept
}

// FoundryHealSpellings are the forms a finding's reports took, canonical,
// as its Finding JSON says: a gathered relationship's `spellings` (contract
// 4.8.0), or else the one canonical name it is named by. None for a signal
// that names no relationship, or JSON that isn't a finding.
func FoundryHealSpellings(finding json.RawMessage) []string {
	var f struct {
		Signal struct {
			Canonical string   `json:"canonical"`
			Spellings []string `json:"spellings"`
		} `json:"signal"`
	}
	if json.Unmarshal(finding, &f) != nil {
		return nil
	}
	if f.Signal.Spellings != nil {
		return f.Signal.Spellings
	}
	if f.Signal.Canonical != "" {
		return []string{f.Signal.Canonical}
	}
	return nil
}

// FoundryHealRunStatus is where one audit of a load stands.
type FoundryHealRunStatus string

const (
	// FoundryHealRunning is held by a worker until its lease ends.
	FoundryHealRunning FoundryHealRunStatus = "running"
	// FoundryHealRunComplete read every page whole and stored its findings.
	FoundryHealRunComplete FoundryHealRunStatus = "complete"
	// FoundryHealRunIncomplete stored nothing: even a page of one document
	// was cut short by the agent's wall, the run ran out of time, or a
	// graph job changed the load under it. A later run audits it again.
	FoundryHealRunIncomplete FoundryHealRunStatus = "incomplete"
	// FoundryHealRunFailed stopped otherwise, its worker's lease ran out,
	// or storing its findings failed, which may have stored some of them.
	FoundryHealRunFailed FoundryHealRunStatus = "failed"
)

// FoundryHealRunsKept is how many of an ontology's newest runs are kept:
// an older one is deleted as a run starts.
const FoundryHealRunsKept = 50

// FoundryHealRun is one row of foundry_heal_runs.
type FoundryHealRun struct {
	ID         string
	OntologyID string
	LoadID     string
	Status     FoundryHealRunStatus
	// Pages are the Audit calls it made; Cuts those the wall cut short.
	Pages, Cuts int
	// RowsMeasured is whether it audited the load's rows, as a complete run
	// does when its load's report holds them; false for any other run.
	RowsMeasured bool
	CreatedAt    time.Time
	// FinishedAt is set exactly once it is no longer running.
	FinishedAt *time.Time
}

// FoundryHealDue is an ontology whose current load is due an audit.
type FoundryHealDue struct {
	WorkspaceID string
	OntologyID  string
	// Load is the current load: ready or partial, with no graph job live on
	// the ontology.
	Load *FoundryLoad
}

// FoundryHealPublish is what a run that read every page whole stores: its
// findings, then the run complete. Findings of a kind the run measured
// that it didn't find are superseded: every kind backed by reviews, and
// the row kinds (Sources 0) when RowsMeasured. Unmeasured, a row finding
// of an earlier load is superseded too: nothing says it still holds.
type FoundryHealPublish struct {
	RunID      string
	OntologyID string
	LoadID     string
	VersionID  string
	// Pages are the run's Audit calls; Cuts those cut short, each audited
	// again in smaller pages.
	Pages, Cuts  int
	Findings     []FoundryHealSeen
	RowsMeasured bool
}
