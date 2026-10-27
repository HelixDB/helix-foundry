package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"time"
)

var _ ChangeReader = (*granolaKind)(nil)

// granolaFollowed are the tables read through notes, which Granola lists by
// update time. Folders and spaces have no such filter and are small, so
// every sync reads them in full.
var granolaFollowed = map[string]bool{"notes": true, "note_attendees": true, "note_folders": true}

// granolaCursor is when each followed table's next change read begins.
type granolaCursor struct {
	Tables map[string]time.Time `json:"tables"`
}

func granolaCursorAt(tables []Table, since time.Time) (json.RawMessage, error) {
	c := granolaCursor{Tables: make(map[string]time.Time, len(tables))}
	for _, t := range tables {
		c.Tables[t.Name] = since
	}
	return json.Marshal(c)
}

func (k *granolaKind) Capture(_ Source, table string) Capture {
	if granolaFollowed[table] {
		return CaptureCursor
	}
	return CaptureFull
}

func (k *granolaKind) Start(_ context.Context, _ Source, tables []Table) (Start, error) {
	cursor, err := granolaCursorAt(tables, timeCursor(time.Now()))
	return Start{Cursor: cursor}, err
}

// Changes lists the notes updated after the cursor and gets each once, for
// every followed table: its notes row, and its attendee and folder rows in
// place of the ones it had. With notes followed, it Puts the documents of
// the notes it upserts, as a full read does (granolaNotes). A table the
// cursor has no time for is reset and read in full. A note the list shows
// deleted, or Get Note no longer finds, loses its rows, as a full read
// would leave them out, and the syncer removes its documents with them.
// Other deletions, and notes the key can no longer read, are invisible here;
// the daily reconcile finds them. Granola doesn't say whether filing a note
// in another folder moves its updated_at; a move that doesn't waits for
// reconcile too.
func (k *granolaKind) Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	next := timeCursor(time.Now())
	var prev granolaCursor
	if err := json.Unmarshal(cursor, &prev); err != nil {
		return nil, fmt.Errorf("decode granola change cursor: %w", ErrCursorExpired)
	}
	var (
		follow []string
		since  time.Time
	)
	for _, t := range tables {
		at, ok := prev.Tables[t.Name]
		if !ok {
			w.Reset(t.Name, "the table has no change cursor yet")
			continue
		}
		follow = append(follow, t.Name)
		// The tables share one list, read from the oldest time: a note read again changes nothing.
		if len(follow) == 1 || at.Before(since) {
			since = at
		}
	}
	if len(follow) == 0 {
		return granolaCursorAt(tables, next)
	}
	query := url.Values{"updated_after": {since.UTC().Format(time.RFC3339)}}
	c := k.client(src.Secret)
	summaries, err := c.list(ctx, query)
	if err != nil {
		return nil, err
	}
	notes := granolaNotes{c: c, docs: w}
	if slices.Contains(follow, "notes") {
		notes.streams = k.Streams(src)
	}
	err = notes.read(ctx, summaries, func(note map[string]any) error {
		for _, table := range follow {
			if table != "notes" {
				if err := w.DeleteWhere(table, "note_id", note["id"]); err != nil {
					return err
				}
			}
			for _, row := range granolaRows(table, note) {
				if err := w.Upsert(table, row); err != nil {
					return err
				}
			}
		}
		return nil
	}, func(id string, _ bool) error {
		for _, table := range follow {
			var err error
			if table == "notes" {
				err = w.Delete(table, map[string]any{"note_id": id})
			} else {
				err = w.DeleteWhere(table, "note_id", id)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return granolaCursorAt(tables, next)
}
