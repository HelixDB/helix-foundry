package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

var _ ChangeReader = (*databuddyKind)(nil)

// databuddyFollowed are the tables a change read follows: a day's metrics
// and a session are whole once read again from the day before the cursor's,
// as a session runs at most hours. Every other table is a total over the
// whole window (a page's views, a visitor's sessions), which a read of the
// last days alone would replace with that part, so every sync reads them in
// full.
var databuddyFollowed = map[string]bool{"daily_metrics": true, "sessions": true}

// databuddyCursor is when each followed table's next change read begins.
type databuddyCursor struct {
	Tables map[string]time.Time `json:"tables"`
}

func databuddyCursorAt(tables []Table, since time.Time) (json.RawMessage, error) {
	c := databuddyCursor{Tables: make(map[string]time.Time, len(tables))}
	for _, t := range tables {
		c.Tables[t.Name] = since
	}
	return json.Marshal(c)
}

func (k *databuddyKind) Capture(_ Source, table string) Capture {
	if databuddyFollowed[table] {
		return CaptureCursor
	}
	return CaptureFull
}

func (k *databuddyKind) Start(_ context.Context, _ Source, tables []Table) (Start, error) {
	cursor, err := databuddyCursorAt(tables, timeCursor(time.Now()))
	return Start{Cursor: cursor}, err
}

// Changes reads each followed table's query from the UTC day before its
// cursor's (no earlier than the history window) to today, for every chosen
// site, and upserts each row: a day or session read again replaces the row
// it was. Days and sessions that leave the window stay until the next full
// read, as the tables are windowed. A table the cursor has no time for is
// reset and read in full.
func (k *databuddyKind) Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	now := time.Now().UTC()
	next := timeCursor(now)
	var prev databuddyCursor
	if err := json.Unmarshal(cursor, &prev); err != nil {
		return nil, fmt.Errorf("decode databuddy change cursor: %w", ErrCursorExpired)
	}
	s, err := databuddySettingsOf(src.Settings)
	if err != nil {
		return nil, err
	}
	window := now.AddDate(0, 0, -s.HistoryDays)
	var sites []map[string]any
	for _, table := range tables {
		at, ok := prev.Tables[table.Name]
		switch {
		case !databuddyFollowed[table.Name]:
			w.Reset(table.Name, "Databuddy totals this table over the whole window")
			continue
		case !ok:
			w.Reset(table.Name, "the table has no change cursor yet")
			continue
		}
		if sites == nil {
			if sites, err = k.sites(ctx, src.Secret, s); err != nil {
				return nil, err
			}
		}
		t, _ := databuddyTableOf(table.Name)
		since := at.UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
		if since.Before(window) {
			since = window
		}
		for _, site := range sites {
			err := k.rows(ctx, src.Secret, t, databuddyText(site["id"]), since, now, func(row map[string]any) error {
				return w.Upsert(table.Name, row)
			})
			if err != nil {
				return nil, err
			}
		}
	}
	return databuddyCursorAt(tables, next)
}
