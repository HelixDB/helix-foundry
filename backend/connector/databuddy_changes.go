package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

var _ ChangeReader = (*databuddyKind)(nil)

// databuddyFollowed are the tables a change read follows: a day's metrics
// or event counts, a day's occurrences and a session are whole once read
// again from before they began. Every other table is a total over the whole
// window (a page's views, a visitor's sessions), which a read of the last
// days alone would replace with that part, so every sync reads them in full.
var databuddyFollowed = map[string]bool{"daily_metrics": true, "custom_events_daily": true, databuddyOccurrences: true, "sessions": true}

// databuddyCursor is when each followed table's next change read begins, and
// the window and sites it was taken for: a change read of other settings, or
// of a site the key reads since, would leave out the days before its cursor.
type databuddyCursor struct {
	Tables      map[string]time.Time `json:"tables"`
	HistoryDays int                  `json:"history_days"`
	Sites       []string             `json:"sites"`
}

func databuddyCursorAt(tables []Table, since time.Time, s DatabuddySettings, sites []string) (json.RawMessage, error) {
	c := databuddyCursor{Tables: make(map[string]time.Time, len(tables)), HistoryDays: s.HistoryDays, Sites: sites}
	for _, t := range tables {
		c.Tables[t.Name] = since
	}
	return json.Marshal(c)
}

// databuddySiteIDs are the sites' ids, sorted.
func databuddySiteIDs(sites []map[string]any) []string {
	ids := make([]string, len(sites))
	for i, site := range sites {
		ids[i] = databuddyText(site["id"])
	}
	slices.Sort(ids)
	return ids
}

func (k *databuddyKind) Capture(_ Source, table string) Capture {
	if databuddyFollowed[table] {
		return CaptureCursor
	}
	return CaptureFull
}

// Start takes the cursor before the full read lists anything, with the sites
// the key reads now.
func (k *databuddyKind) Start(ctx context.Context, src Source, tables []Table) (Start, error) {
	since := timeCursor(time.Now())
	s, err := databuddySettingsOf(src.Settings)
	if err != nil {
		return Start{}, err
	}
	sites, err := k.sites(ctx, src.Secret, s)
	if err != nil {
		return Start{}, err
	}
	cursor, err := databuddyCursorAt(tables, since, s, databuddySiteIDs(sites))
	return Start{Cursor: cursor}, err
}

// Changes reads each followed table from the UTC day before its cursor's (no
// earlier than the history window) to today, for every chosen site, and
// upserts each row: a day read again replaces the row it was, and an
// occurrence read again is the row it was (its key is its content). The
// occurrences a change read adds are not held to maxDatabuddyOccurrences:
// they are the newest days, and the next full read keeps what fits. A session that
// began before that day would come back holding only its later events, so
// sessions are read from a day earlier still and only those that began on
// or after it are upserted. Days and sessions that leave the window stay
// until the next full read, as the tables are windowed. A table the cursor
// has no time for is reset and read in full; a cursor of other days of
// history or other sites is expired.
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
	sites, err := k.sites(ctx, src.Secret, s)
	if err != nil {
		return nil, err
	}
	ids := databuddySiteIDs(sites)
	if prev.HistoryDays != s.HistoryDays || !slices.Equal(prev.Sites, ids) {
		return nil, fmt.Errorf("databuddy change cursor of other settings or sites: %w", ErrCursorExpired)
	}
	window := now.AddDate(0, 0, -s.HistoryDays)
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
		t, _ := databuddyTableOf(table.Name)
		since := at.UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
		if since.Before(window) {
			since = window
		}
		from, upsert := since, func(row map[string]any) error { return w.Upsert(table.Name, row) }
		if table.Name == databuddyOccurrences {
			if err := k.occurrences(ctx, src.Secret, sites, from, now, upsert); err != nil {
				return nil, err
			}
			continue
		}
		if table.Name == "sessions" {
			from = since.AddDate(0, 0, -1)
			upsert = func(row map[string]any) error {
				// A start Databuddy writes in a way not read here is kept: it can't be told apart.
				if began, ok := databuddyTime(row["first_visit"]); ok && began.Before(since) {
					return nil
				}
				return w.Upsert(table.Name, row)
			}
		}
		for _, site := range sites {
			if err := k.rows(ctx, src.Secret, t, databuddyText(site["id"]), from, now, upsert); err != nil {
				return nil, err
			}
		}
	}
	return databuddyCursorAt(tables, next, s, ids)
}

// databuddyTime reads a time Databuddy writes: RFC 3339, or ClickHouse's
// "2006-01-02 15:04:05" in UTC.
func databuddyTime(v any) (time.Time, bool) {
	text, _ := v.(string)
	for _, layout := range []string{time.RFC3339Nano, time.DateTime, "2006-01-02 15:04:05.000"} {
		if t, err := time.Parse(layout, text); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
