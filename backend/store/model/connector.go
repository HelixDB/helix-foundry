package model

import (
	"encoding/json"
	"time"
)

// Connector is a customer data source Helix Foundry reads from. Config holds
// only non-secret settings; credentials are sealed in SecretCiphertext under
// SecretKeyID and never leave the backend.
type Connector struct {
	ID               string
	WorkspaceID      string
	Kind             string
	Name             string
	Config           json.RawMessage
	SecretCiphertext []byte
	SecretKeyID      string
	// SecretHint identifies the credential to a person without revealing it, e.g. sk_live…4f2a.
	SecretHint      string
	CreatedByUserID *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// SyncEverySeconds is the owner's interval: nil is the plan's, 0 manual only.
	SyncEverySeconds *int
	// NextSyncAt is when the scheduler queues the next sync; nil when none is scheduled.
	NextSyncAt *time.Time
	// SchedulePaused is why automatic syncs stopped; nil while they run.
	SchedulePaused *string
	// ChangeCursor is where the next change read starts, in the kind's own JSON.
	ChangeCursor json.RawMessage
}

// ConnectorUpdate changes a connector. A nil Config leaves config and
// credentials alone; a non-nil one replaces both, since they are tested together.
type ConnectorUpdate struct {
	Name             *string
	Config           json.RawMessage
	SecretCiphertext []byte
	SecretKeyID      string
	SecretHint       string
	// Schedule, when set, replaces the owner's interval, clears a pause and
	// sets the next sync.
	Schedule *ConnectorScheduleUpdate
}

// ConnectorScheduleUpdate is an owner's schedule choice. A nil
// SyncEverySeconds returns to the plan's interval; NextSyncAt nil schedules nothing.
type ConnectorScheduleUpdate struct {
	SyncEverySeconds *int
	NextSyncAt       *time.Time
}

// ConnectorSyncOrigin is what queued a sync.
type ConnectorSyncOrigin string

const (
	ConnectorSyncManual   ConnectorSyncOrigin = "manual"
	ConnectorSyncSchedule ConnectorSyncOrigin = "schedule"
	ConnectorSyncSettings ConnectorSyncOrigin = "settings"
	// ConnectorSyncEvents is every sync of an event source, whatever queued
	// it: a workspace runs one such sync at a time, beside its other syncs.
	ConnectorSyncEvents ConnectorSyncOrigin = "events"
)

// ConnectorSyncMode is how much a sync may read: changes reads only what
// changed where a table allows it; full reads every table in full.
type ConnectorSyncMode string

const (
	ConnectorSyncChanges ConnectorSyncMode = "changes"
	ConnectorSyncFull    ConnectorSyncMode = "full"
)

// ConnectorSyncStatus is where a sync is in its run.
type ConnectorSyncStatus string

const (
	ConnectorSyncQueued    ConnectorSyncStatus = "queued"
	ConnectorSyncRunning   ConnectorSyncStatus = "running"
	ConnectorSyncSucceeded ConnectorSyncStatus = "succeeded"
	ConnectorSyncFailed    ConnectorSyncStatus = "failed"
)

// ConnectorSync is one run copying a connector's tables to S3, and the
// leased job the sync worker claims. A connector has at most one live sync.
type ConnectorSync struct {
	ID                string
	ConnectorID       string
	WorkspaceID       string
	Status            ConnectorSyncStatus
	LeaseOwner        *string
	LeaseUntil        *time.Time
	Attempts          int
	NextAttemptAt     time.Time
	Error             *string
	RowsSynced        *int64
	RequestedByUserID *string
	CreatedAt         time.Time
	StartedAt         *time.Time
	FinishedAt        *time.Time
	Origin            ConnectorSyncOrigin
	Mode              ConnectorSyncMode
}

// ConnectorSyncOutcome ends a sync's attempt. A non-nil RetryAt requeues it;
// otherwise Status must be succeeded or failed and the sync stops being live.
type ConnectorSyncOutcome struct {
	Status     ConnectorSyncStatus
	Error      string
	RowsSynced *int64
	RetryAt    *time.Time
	// Schedule, on a terminal outcome, is what the connector's schedule becomes.
	Schedule *ConnectorScheduleOutcome
	// Mode, on a terminal outcome, is how the sync read; empty keeps the mode it was queued with.
	Mode ConnectorSyncMode
}

// ConnectorScheduleOutcome is the connector's schedule after a sync: the next
// sync, or none with Paused saying why.
type ConnectorScheduleOutcome struct {
	NextSyncAt *time.Time
	Paused     string
	// Every is the connector's sync_every_seconds the outcome was worked out
	// from; a schedule the owner changed during the sync stays as they set it.
	Every *int
	// Pushed, for a pushed kind's connector (an event source), is its next
	// sync in place of NextSyncAt and Every, which must then be nil: it has
	// no interval, and a send may have scheduled a sync while this one ran.
	Pushed PushedNext
}

// PushedNext is when a pushed kind's connector syncs next: PushedKeep,
// PushedNotBefore or PushedAt, nothing else, as the unexported method seals
// it. Each says what becomes of a sync a send scheduled meanwhile.
type PushedNext interface{ pushedNext() }

// PushedKeep leaves the next sync as it is: none, or one a send scheduled.
type PushedKeep struct{}

// PushedNotBefore syncs no sooner than At: a later sync a send scheduled
// stays, and none or an earlier one becomes At, so the spacing between two
// syncs is never cut short.
type PushedNotBefore struct{ At time.Time }

// PushedAt syncs at At, whatever was scheduled.
type PushedAt struct{ At time.Time }

func (PushedKeep) pushedNext()      {}
func (PushedNotBefore) pushedNext() {}
func (PushedAt) pushedNext()        {}

// ConnectorTable is the latest snapshot of one source table. Columns,
// PrimaryKey and ForeignKeys are JSON arrays in the connector package's shapes.
type ConnectorTable struct {
	ID          string
	ConnectorID string
	WorkspaceID string
	TableName   string
	Columns     json.RawMessage
	PrimaryKey  json.RawMessage
	ForeignKeys json.RawMessage
	S3Key       string
	RowCount    int64
	SyncID      string
	SyncedAt    time.Time
	// ContentDigest is the SHA-256 of the table's sorted key and row hashes; nil before the first digest.
	ContentDigest []byte
	// ChangeSeq is the table's newest change batch, 0 before the first.
	ChangeSeq int64
	// ReconciledAt is the table's last full read.
	ReconciledAt *time.Time
}

// ConnectorChangeKind is what a change batch says about its table.
type ConnectorChangeKind string

const (
	// ConnectorChangesBatch is a file of upserts and deletes by key.
	ConnectorChangesBatch ConnectorChangeKind = "changes"
	// ConnectorReplaceBatch is a table replaced whole: keyless, or a first snapshot.
	ConnectorReplaceBatch ConnectorChangeKind = "replace"
	// ConnectorDropBatch is a table no longer synced.
	ConnectorDropBatch ConnectorChangeKind = "drop"
)

// ConnectorChangeBatch is one sync's change to one table.
type ConnectorChangeBatch struct {
	ID          string
	WorkspaceID string
	ConnectorID string
	TableName   string
	Seq         int64
	SyncID      string
	Kind        ConnectorChangeKind
	S3Key       *string
	Inserts     int64
	Updates     int64
	Deletes     int64
	CreatedAt   time.Time
}

// ReplacedSnapshot is a snapshot a sync replaced, waiting for no load to pin it.
type ReplacedSnapshot struct {
	ID          string
	WorkspaceID string
	ConnectorID string
	S3Key       string
	ReplacedAt  time.Time
}

// FoundryLoadSource is one snapshot a load job reads, pinned on its first
// attempt. SyncID, SyncedAt and ChangeSeq come from the same ConnectorTable
// row as S3Key: the sync that last observed the snapshot, when, and the
// table's change batch the snapshot holds every change up to. A pin written
// before they were kept has no SyncID.
type FoundryLoadSource struct {
	ConnectorID string    `json:"connector_id"`
	Table       string    `json:"table"`
	S3Key       string    `json:"s3_key"`
	SyncID      string    `json:"sync_id"`
	SyncedAt    time.Time `json:"synced_at"`
	ChangeSeq   int64     `json:"change_seq"`
}

// ConnectorPartName is what the user calls a connector's table or document
// set: "<connection> / <name>".
//
//	ConnectorPartName("Granola", "Notes") // "Granola / Notes"
func ConnectorPartName(connectorName, name string) string { return connectorName + " / " + name }
