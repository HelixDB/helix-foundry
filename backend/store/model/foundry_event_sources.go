package model

import (
	"context"
	"time"

	"github.com/helixdb/helix-foundry/backend/events"
)

// foundry_event_sources.go is Foundry's event sources (connector kind
// events; people call them API sources): the batches people and their
// agents send them (foundry_event_batches) and their send-only tokens
// (foundry_event_source_tokens). The rules of a send are package events'.

// ConnectorKindEvents is the kind of an event source's connectors row.
const ConnectorKindEvents = "events"

// FoundryEventBatch is one send to an event source: a foundry_event_batches
// row and the object at S3Key it names, written before the row.
type FoundryEventBatch struct {
	ID          string
	WorkspaceID string
	ConnectorID string
	// Seq counts the source's sends from 1 in commit order.
	Seq int64
	// ClientToken makes a retry of the send return it; nil when the sender
	// gave none.
	ClientToken *string
	S3Key       string
	// Pieces is the row's events: how many pieces the send held. Bytes is
	// its object's uncompressed size.
	Pieces int
	Bytes  int64
	// FirstAt and LastAt are the earliest and latest piece time, its
	// occurred_at or else its received_at.
	FirstAt, LastAt time.Time
	// CreatedAt is the send's received_at, written on every line.
	CreatedAt time.Time
}

// FoundryEventSourceToken is a send-only credential of one event source. It
// stores the SHA-256 of the token, never the token.
type FoundryEventSourceToken struct {
	ID          string
	WorkspaceID string
	ConnectorID string
	// Hash is the lowercase hex SHA-256 of the whole token.
	Hash string
	// Prefix is the token's first 12 characters, to tell tokens apart.
	Prefix    string
	CreatedBy string
	CreatedAt time.Time
	// RevokedAt is when it stopped working; nil while it is live.
	RevokedAt *time.Time
	// LastUsedAt is when it last authenticated, to the minute; nil if never.
	LastUsedAt *time.Time
}

// EventSourceUpdate is an owner's edit of an event source. A nil field stays
// as it is, and at least one must be set. Nothing else of a source is edited:
// its learned schema, change cursor and next sync are its sends' and syncs',
// but for the lock a new text field puts on the schema.
type EventSourceUpdate struct {
	Name        *string
	Description *string
	Text        *EventSourceText
}

// EventSourceText makes Field an event source's text field
// (config.text_field): the top-level field its pieces' documents take their
// body from. "" clears it, and the source keeps no documents. Any other
// field is locked as a string in the source's schema
// (events.Schema.WithText), within MaxFields fields (foundry.events
// max_fields). The change queues a full sync of origin events, for
// RequestedByUserID (nil for none), which renders the source's newest pieces
// by the new field. A field the source already has changes nothing.
type EventSourceText struct {
	Field             string
	MaxFields         int
	RequestedByUserID *string
}

// FoundryEventSourceState is what one consistent read sees of a live event
// source: what a send checks before and under its lock, and what a list
// shows of how full the source is.
type FoundryEventSourceState struct {
	Connector *Connector
	// Schema is config.schema: the fields its pieces have carried.
	Schema events.Schema
	// Cursor is change_cursor.seq: the newest batch merged into its table,
	// 0 before the first sync.
	Cursor int64
	// TableKey, TableRows and TableFields are its events table's snapshot,
	// row count and column names in connector_tables; "", 0 and nil before
	// its first sync.
	TableKey    string
	TableRows   int64
	TableFields []string
	// PendingPieces and PendingBytes are the sums of events and bytes of
	// its batches above Cursor, which no sync has merged yet.
	PendingPieces, PendingBytes int64
}

// Holding is what the source holds, as the caps count it, given tableBytes:
// the uncompressed size of the snapshot at TableKey (its ISIZE), 0 for none.
func (s FoundryEventSourceState) Holding(tableBytes int64) events.Holding {
	return events.Holding{
		TableRows: s.TableRows, TableBytes: tableBytes, TableFields: s.TableFields,
		PendingPieces: s.PendingPieces, PendingBytes: s.PendingBytes,
	}
}

// FoundryEventBatchList is an event source's newest batches and its state,
// read together. A batch is synced, in its table, when its Seq is at most
// State.Cursor.
type FoundryEventBatchList struct {
	State   FoundryEventSourceState
	Batches []*FoundryEventBatch
}

// EventSyncSpacing is how long an event source waits between syncs
// (foundry.events sync_after and sync_after_per_million_rows): every merge
// rewrites the whole table, so a larger table syncs less often.
type EventSyncSpacing struct {
	After          time.Duration
	PerMillionRows time.Duration
}

// For is the spacing of a source whose events table holds rows rows (0
// before its first sync).
//
//	EventSyncSpacing{After: time.Minute, PerMillionRows: 10 * time.Minute}.For(2_000_000) // 21m
func (s EventSyncSpacing) For(rows int64) time.Duration {
	return s.After + time.Duration(float64(s.PerMillionRows)*float64(rows)/1e6)
}

// FoundryEventSend is one send, ready to commit: its object is already
// stored at Key, and the commit only adds its row.
type FoundryEventSend struct {
	// ID is the batch's id, which names its object.
	ID          string
	WorkspaceID string
	ConnectorID string
	ClientToken *string
	Key         string
	// Send is the validated pieces, and Object their encoding: its times
	// are the row's created_at, first_at and last_at.
	Send   events.Send
	Object events.Object
	// MaxFields is foundry.events max_fields, Caps the caps of every table,
	// and Spacing schedules the source's next sync when none is scheduled.
	MaxFields int
	Caps      events.Caps
	Spacing   EventSyncSpacing
	// TableBytes returns the uncompressed size of the events table's
	// snapshot at key (its ISIZE). The commit calls it under the source's
	// lock whenever the source has a snapshot, so it should answer from a
	// cache by key and read storage only for a key it has not seen: keys
	// are never reused.
	TableBytes func(ctx context.Context, key string) (int64, error)
}

// FoundryEventsSent is a committed send.
type FoundryEventsSent struct {
	Batch FoundryEventBatch
	// New are the fields this send added to the source.
	New []events.Field
	// Capacity is how full the source is with this send.
	Capacity events.Capacity
}

// FoundryEventTotals are a workspace's sends since a time, as its daily caps
// count them: the pieces, and the bytes of their batch objects uncompressed.
type FoundryEventTotals struct {
	Pieces, Bytes int64
}

// FoundryEventSourcePurge is a deleted event source as the purge goes
// through them, in the order they were deleted: (DeletedAt, ConnectorID)
// orders them and is the purge's cursor.
type FoundryEventSourcePurge struct {
	WorkspaceID string
	ConnectorID string
	DeletedAt   time.Time
	// Batches is whether batches of it are left, whose objects and rows the
	// purge deletes before its folder.
	Batches bool
}
