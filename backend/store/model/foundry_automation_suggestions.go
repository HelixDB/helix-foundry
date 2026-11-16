package model

import (
	"encoding/json"
	"errors"
	"time"
)

// FoundryAutomationSuggestionsStatus is where a version's suggestions for a
// new automation stand.
type FoundryAutomationSuggestionsStatus string

const (
	// FoundryAutomationSuggestionsQueued waits for the Assistant's worker.
	FoundryAutomationSuggestionsQueued FoundryAutomationSuggestionsStatus = "queued"
	// FoundryAutomationSuggestionsRunning is being written under a lease.
	FoundryAutomationSuggestionsRunning FoundryAutomationSuggestionsStatus = "running"
	// FoundryAutomationSuggestionsReady holds the suggestions written.
	FoundryAutomationSuggestionsReady FoundryAutomationSuggestionsStatus = "ready"
	// FoundryAutomationSuggestionsFailed couldn't be written; a read an
	// hour later queues it again.
	FoundryAutomationSuggestionsFailed FoundryAutomationSuggestionsStatus = "failed"
)

// FoundryAutomationSuggestions is one ontology version's suggestions for a
// new automation (migrations/20261204000000_foundry_automation_suggestions.sql).
type FoundryAutomationSuggestions struct {
	OntologyID  string
	VersionID   string
	WorkspaceID string
	Status      FoundryAutomationSuggestionsStatus
	// Suggestions is the JSON list package suggest kept; set only when ready.
	Suggestions json.RawMessage
	// Failure is why a failed write failed, briefly.
	Failure *string
	// USDMicros is what every attempt spent; ChargedAt when it was last
	// charged, which the Assistant's monthly budget reads.
	USDMicros  int64
	Attempts   int
	LeaseOwner *string
	LeaseUntil *time.Time
	// RequestedAt is when a read last queued it.
	RequestedAt time.Time
	UpdatedAt   time.Time
	ChargedAt   *time.Time
}

// FoundryAutomationSuggestionsEnd is how a write ended: ready with
// Suggestions, or failed with Failure, never both, and what it spent.
type FoundryAutomationSuggestionsEnd struct {
	Suggestions json.RawMessage
	Failure     string
	USDMicros   int64
}

// Validate refuses an end that is neither ready nor failed, or both, a
// failure past 255 characters or a negative spend.
func (e FoundryAutomationSuggestionsEnd) Validate() error {
	switch {
	case (len(e.Suggestions) > 0) == (e.Failure != ""):
		return errors.New("automation suggestions end ready or failed, not both or neither")
	case len([]rune(e.Failure)) > 255:
		return errors.New("an automation suggestions failure is at most 255 characters")
	case e.USDMicros < 0:
		return errors.New("automation suggestions spend nothing below zero")
	}
	return nil
}

// Status is the status e ends a write with.
func (e FoundryAutomationSuggestionsEnd) Status() FoundryAutomationSuggestionsStatus {
	if len(e.Suggestions) > 0 {
		return FoundryAutomationSuggestionsReady
	}
	return FoundryAutomationSuggestionsFailed
}
