package connector

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/helixdb/helix-foundry/backend/events"
)

// Capture is how a table is followed between full reads. The values are the
// plan catalog's capture methods.
type Capture string

const (
	// CaptureFull reads every row; the table can't be followed.
	CaptureFull Capture = "full"
	// CaptureCursor reads rows the source says changed after a point.
	CaptureCursor Capture = "cursor"
	// CaptureEvents reads the source's change events.
	CaptureEvents Capture = "events"
	// CaptureLog reads a database's replication log.
	CaptureLog Capture = "log"
)

// ErrCursorExpired means the source no longer holds the changes after the
// cursor; the syncer reads every table in full and starts again.
var ErrCursorExpired = errors.New("connector: change cursor expired")

// PositionLostError is ErrCursorExpired when the source dropped the position
// the cursor held, such as a purged binlog; Reason becomes the last reset.
type PositionLostError struct{ Reason string }

func (e *PositionLostError) Error() string { return ErrCursorExpired.Error() + ": " + e.Reason }
func (e *PositionLostError) Unwrap() error { return ErrCursorExpired }

// KeepLastReset gives next, the cursor a full read began from, the last reset
// its CDC status shows: the one Start recorded, else the lost position that
// expired the change read, else prev's. A cursor without a "cdc" object is
// returned as it is.
func KeepLastReset(next, prev json.RawMessage, expired error) json.RawMessage {
	var cursor, status map[string]json.RawMessage
	if json.Unmarshal(next, &cursor) != nil || json.Unmarshal(cursor["cdc"], &status) != nil || status == nil || status["last_reset"] != nil {
		return next
	}
	var lost *PositionLostError
	if errors.As(expired, &lost) {
		status["last_reset_at"], status["last_reset"] = encodeJSON(time.Now().UTC()), encodeJSON(lost.Reason)
	} else {
		var old struct {
			CDC map[string]json.RawMessage `json:"cdc"`
		}
		if json.Unmarshal(prev, &old) != nil || old.CDC["last_reset"] == nil {
			return next
		}
		status["last_reset_at"], status["last_reset"] = old.CDC["last_reset_at"], old.CDC["last_reset"]
	}
	cursor["cdc"] = encodeJSON(status)
	return encodeJSON(cursor)
}

// encodeJSON encodes values that cannot fail to: times, strings, and maps of decoded JSON.
func encodeJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ChangeWriter takes one change read's changes, per table, in the order the
// source reports them; the last change to a key wins. A DocumentKind Puts
// the documents of the rows it upserts, as its full read does; a change read
// has no cap, and a row that loses its text keeps its document until the
// next full read.
type ChangeWriter interface {
	DocumentWriter
	Upsert(table string, row map[string]any) error
	// UpsertJSON takes a row the source already encoded, as its full read does.
	UpsertJSON(table string, row []byte) error
	Delete(table string, key map[string]any) error
	// DeleteWhere removes every row whose column holds value: a cascade the
	// source doesn't report row by row.
	DeleteWhere(table, column string, value any) error
	// Reset says the change read can't follow table this time; the syncer
	// reads it in full in the same sync.
	Reset(table, reason string)
}

// VersionedWriter is a ChangeWriter that deletes a row of a table with a
// Version column only when the delete is not older than the stored row.
type VersionedWriter interface {
	DeleteVersion(table string, key map[string]any, version any) error
}

// ColumnWriter is a ChangeWriter that a kind can tell the columns its rows
// carry. A Dense table's stored rows are padded to them, and they replace
// the table's stored columns, types included. Other tables ignore them.
type ColumnWriter interface {
	Columns(table string, columns []Column)
}

// deleteVersion deletes key as of version where w can compare versions.
func deleteVersion(w ChangeWriter, table string, key map[string]any, version any) error {
	if vw, ok := w.(VersionedWriter); ok {
		return vw.DeleteVersion(table, key, version)
	}
	return w.Delete(table, key)
}

// Start is where change reads begin after a full read.
type Start struct {
	Cursor json.RawMessage
	// Source, when set, is the source the full read must use: one bound to a
	// database snapshot that matches Cursor.
	Source *Source
	// Close releases what Start holds open until the full read ends; nil when nothing is.
	Close func()
}

// ChangeReader is a kind that can read only what changed. A table it can't
// follow under the given settings is read in full on every sync.
type ChangeReader interface {
	// Capture is how table is followed with src's settings; CaptureFull when it can't be.
	Capture(src Source, table string) Capture
	// Start returns the cursor a full read of tables begins from: every change
	// after it is the next change read's.
	Start(ctx context.Context, src Source, tables []Table) (Start, error)
	// Changes writes every change to tables after cursor and returns where the
	// next read starts. ErrCursorExpired asks for a full read.
	Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error)
}

// Releaser is a kind that holds something on the source between change
// reads, such as a replication slot, and must give it up when the connector
// stops following changes.
type Releaser interface {
	Release(ctx context.Context, src Source, cursor json.RawMessage) error
}

// Backlog is what a Pushed kind holds beyond its cursor. It is the events
// package's, so the provider's batch reads answer the events kind directly.
type Backlog = events.Backlog

const (
	BacklogNone = events.BacklogNone // every row is in the tables
	BacklogSome = events.BacklogSome // sync again after the spacing
	BacklogMore = events.BacklogMore // more than one read takes: sync again at once
)

// Pushed is a kind whose rows Helix itself holds: nothing changes at a
// source, so it is followed on every plan, never reconciled, and synced when
// rows arrive.
type Pushed interface {
	ChangeReader
	// Backlog says what waits beyond cursor, the one the sync just stored.
	Backlog(ctx context.Context, src Source, cursor json.RawMessage) (Backlog, error)
}

// IntervalFloor is a kind that must not sync more often than a floor under
// some settings, whatever the plan allows: PostHog's query API, for one.
type IntervalFloor interface {
	MinInterval(src Source) time.Duration
}

// ChangeOverlap is how far a time cursor reaches back before the read that
// set it: "updated after" filters and clocks are not exact, and a row read
// twice changes nothing.
var ChangeOverlap = 2 * time.Minute
