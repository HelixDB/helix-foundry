package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

var _ ChangeReader = (*linearKind)(nil)

// linearCursor is when each table's next change read begins.
type linearCursor struct {
	Tables map[string]time.Time `json:"tables"`
}

func linearCursorAt(tables []Table, since time.Time) (json.RawMessage, error) {
	c := linearCursor{Tables: make(map[string]time.Time, len(tables))}
	for _, t := range tables {
		c.Tables[t.Name] = since
	}
	return json.Marshal(c)
}

// Capture follows every table: each root connection filters on updatedAt.
func (k *linearKind) Capture(_ Source, table string) Capture {
	if _, ok := linearQueries[table]; ok {
		return CaptureCursor
	}
	return CaptureFull
}

func (k *linearKind) Start(_ context.Context, _ Source, tables []Table) (Start, error) {
	cursor, err := linearCursorAt(tables, timeCursor(time.Now()))
	return Start{Cursor: cursor}, err
}

// Changes upserts each table's nodes updated after the cursor, archived ones
// included. A table the cursor has no time for is reset and read in full.
// Permanently deleted nodes are invisible here; reconcile finds them.
func (k *linearKind) Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	next := timeCursor(time.Now())
	var prev linearCursor
	if err := json.Unmarshal(cursor, &prev); err != nil {
		return nil, fmt.Errorf("decode linear change cursor: %w", ErrCursorExpired)
	}
	for _, t := range tables {
		since, ok := prev.Tables[t.Name]
		if !ok {
			w.Reset(t.Name, "the table has no change cursor yet")
			continue
		}
		err := k.read(ctx, src.Secret, t.Name, &since, func(row map[string]any) error { return w.Upsert(t.Name, row) })
		if err != nil {
			return nil, err
		}
	}
	return linearCursorAt(tables, next)
}
