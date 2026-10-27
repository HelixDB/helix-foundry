package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

var _ ChangeReader = (*calcomKind)(nil)

// calcomFollowed are the tables a change read follows: bookings list by update
// and creation time, and their attendees and hosts come within them. Users,
// teams, memberships, event types and schedules have no such filter and are
// few, so every sync reads them in full.
var calcomFollowed = map[string]bool{"bookings": true, "booking_attendees": true, "booking_hosts": true}

// calcomCursor is when each followed table's next change read begins.
type calcomCursor struct {
	Tables map[string]time.Time `json:"tables"`
}

func calcomCursorAt(tables []Table, since time.Time) (json.RawMessage, error) {
	c := calcomCursor{Tables: make(map[string]time.Time, len(tables))}
	for _, t := range tables {
		c.Tables[t.Name] = since
	}
	return json.Marshal(c)
}

func (k *calcomKind) Capture(_ Source, table string) Capture {
	if calcomFollowed[table] {
		return CaptureCursor
	}
	return CaptureFull
}

func (k *calcomKind) Start(_ context.Context, _ Source, tables []Table) (Start, error) {
	cursor, err := calcomCursorAt(tables, timeCursor(time.Now()))
	return Start{Cursor: cursor}, err
}

// Changes lists the bookings of every status updated, then created, after the
// earliest time the cursor holds for a followed table, and writes each as a
// full read does; a booking's attendees and hosts replace the rows it held.
// Both filters run because a booking never edited has no update time. A
// table the cursor has no time for is reset and read in full. Deleted
// bookings, and attendees changed without their booking, such as one marked
// absent, are invisible here; the daily full reconcile finds them.
func (k *calcomKind) Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	next := timeCursor(time.Now())
	var prev calcomCursor
	if err := json.Unmarshal(cursor, &prev); err != nil {
		return nil, fmt.Errorf("decode calcom change cursor: %w", ErrCursorExpired)
	}
	var (
		follow []string
		since  time.Time
	)
	for _, t := range tables {
		at, ok := prev.Tables[t.Name]
		switch {
		case !calcomFollowed[t.Name]:
			w.Reset(t.Name, "Cal.com cannot list this table's changes")
		case !ok:
			w.Reset(t.Name, "the table has no change cursor yet")
		default:
			follow = append(follow, t.Name)
			if since.IsZero() || at.Before(since) {
				since = at
			}
		}
	}
	if len(follow) == 0 {
		return calcomCursorAt(tables, next)
	}
	// A booking created after the cursor shows in both lists; it is written once.
	seen := map[any]bool{}
	for _, filter := range []string{"afterUpdatedAt", "afterCreatedAt"} {
		err := k.bookings(ctx, src.Secret, url.Values{filter: {since.UTC().Format(time.RFC3339)}}, func(booking map[string]any) error {
			if seen[booking["uid"]] {
				return nil
			}
			seen[booking["uid"]] = true
			for _, table := range follow {
				// An attendee or host the booking no longer lists is gone.
				if table != "bookings" {
					if err := w.DeleteWhere(table, "booking_uid", booking["uid"]); err != nil {
						return err
					}
				}
				for _, row := range calcomBookingRows(table, booking) {
					if err := w.Upsert(table, row); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return calcomCursorAt(tables, next)
}
