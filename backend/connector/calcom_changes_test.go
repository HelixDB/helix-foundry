package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"
)

// calcomChanges is cursorChanges that also records, in order, each upsert
// and each cascade a changed booking's attendees and hosts take.
type calcomChanges struct {
	cursorChanges
	calls []string
}

func (c *calcomChanges) Upsert(table string, row map[string]any) error {
	c.calls = append(c.calls, fmt.Sprintf("upsert %s %v", table, row["booking_uid"]))
	return c.cursorChanges.Upsert(table, row)
}

func (c *calcomChanges) DeleteWhere(table, column string, value any) error {
	c.calls = append(c.calls, fmt.Sprintf("where %s %s=%v", table, column, value))
	return nil
}

func TestCalcomChanges(t *testing.T) {
	ctx := context.Background()
	reader := func(t *testing.T, url string) ChangeReader {
		t.Helper()
		return kindOf(t, catalogFor(url), "calcom").(ChangeReader)
	}
	bookingTables := []Table{{Name: "bookings"}, {Name: "booking_attendees"}, {Name: "booking_hosts"}}
	since := "2026-10-20T08:00:00Z"
	cursorAt := func(at string, tables ...string) json.RawMessage {
		times := map[string]string{}
		for _, table := range tables {
			times[table] = at
		}
		b, _ := json.Marshal(map[string]any{"tables": times})
		return b
	}
	// bookingsServer answers the updated list with b1 and the created list with b1 and b2, as a booking created after the cursor shows in both.
	bookingsServer := func(t *testing.T, since string) (string, *[]string) {
		var filters []string
		url := calcomServer(t, map[string]func(*http.Request) (int, string){
			"/v2/bookings": func(r *http.Request) (int, string) {
				q := r.URL.Query()
				if q.Get("limit") != "100" || q.Has("status") {
					t.Errorf("bookings query = %s", r.URL.RawQuery)
				}
				switch {
				case q.Get("afterUpdatedAt") == since && !q.Has("afterCreatedAt"):
					filters = append(filters, "updated")
					return http.StatusOK, `{"status":"success","data":[` + calcomBooking + `],"pagination":{"nextCursor":null,"hasMore":false}}`
				case q.Get("afterCreatedAt") == since && !q.Has("afterUpdatedAt"):
					filters = append(filters, "created")
					return http.StatusOK, `{"status":"success","data":[` + calcomBooking + `,` + calcomOldBooking + `],"pagination":{"hasMore":false}}`
				}
				t.Errorf("bookings query = %s, want one filter from %s", r.URL.RawQuery, since)
				return http.StatusBadRequest, `{"status":"error"}`
			},
		})
		return url, &filters
	}

	// The other tables have no update filter and are few, so a change sync reads them in full.
	t.Run("follows bookings and their attendees and hosts only", func(t *testing.T) {
		r := reader(t, "http://unused")
		for _, o := range calcomObjects {
			want := CaptureFull
			if o.name == "bookings" || o.name == "booking_attendees" || o.name == "booking_hosts" {
				want = CaptureCursor
			}
			if got := r.Capture(Source{}, o.name); got != want {
				t.Errorf("Capture(%s) = %s, want %s", o.name, got, want)
			}
		}
	})
	// The cursor is taken before the full read, so a change made during that read is read again by the next change sync.
	t.Run("starts each table before the full read, less the overlap", func(t *testing.T) {
		before := time.Now()
		start, err := reader(t, "http://unused").Start(ctx, Source{Secret: calcomTestKey}, bookingTables)
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		var cursor calcomCursor
		if err := json.Unmarshal(start.Cursor, &cursor); err != nil {
			t.Fatal(err)
		}
		if len(cursor.Tables) != 3 {
			t.Fatalf("cursor = %s", start.Cursor)
		}
		for _, table := range bookingTables {
			assertTimeCursor(t, cursor.Tables[table.Name], before, after)
		}
	})
	// A booking never edited has no update time, so only the created list finds it; each booking is written once.
	t.Run("reads bookings updated or created after the cursor, every status", func(t *testing.T) {
		url, filters := bookingsServer(t, since)
		changes := &calcomChanges{}
		before := time.Now()
		next, err := reader(t, url).Changes(ctx, Source{Secret: calcomTestKey}, []Table{{Name: "bookings"}}, cursorAt(since, "bookings"), changes)
		after := time.Now()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(*filters, []string{"updated", "created"}) || !slices.Equal(changes.calls, []string{"upsert bookings b1", "upsert bookings b2"}) {
			t.Fatalf("filters = %v, changes = %v", *filters, changes.calls)
		}
		var cursor calcomCursor
		if err := json.Unmarshal(next, &cursor); err != nil {
			t.Fatal(err)
		}
		assertTimeCursor(t, cursor.Tables["bookings"], before, after)
	})
	// An attendee or host the booking no longer lists must leave the table, and a change read sees no deletes.
	t.Run("replaces a changed booking's attendees and hosts", func(t *testing.T) {
		url, _ := bookingsServer(t, since)
		changes := &calcomChanges{}
		_, err := reader(t, url).Changes(ctx, Source{Secret: calcomTestKey}, bookingTables, cursorAt(since, "bookings", "booking_attendees", "booking_hosts"), changes)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{
			"upsert bookings b1",
			"where booking_attendees booking_uid=b1", "upsert booking_attendees b1", "upsert booking_attendees b1",
			"where booking_hosts booking_uid=b1", "upsert booking_hosts b1",
			"upsert bookings b2", "where booking_attendees booking_uid=b2", "where booking_hosts booking_uid=b2",
		}
		if !slices.Equal(changes.calls, want) {
			t.Fatalf("changes = %q, want %q", changes.calls, want)
		}
	})
	// An upsert replaces the stored row, so a changed row must be exactly what a full read would have stored.
	t.Run("writes rows exactly as a full read writes them", func(t *testing.T) {
		url, _ := bookingsServer(t, since)
		changes := &calcomChanges{}
		_, err := reader(t, url).Changes(ctx, Source{Secret: calcomTestKey}, bookingTables, cursorAt(since, "bookings", "booking_attendees", "booking_hosts"), changes)
		if err != nil {
			t.Fatal(err)
		}
		full := calcomServer(t, map[string]func(*http.Request) (int, string){
			"/v2/bookings": calcomOK(`{"status":"success","data":[` + calcomBooking + `,` + calcomOldBooking + `],"pagination":{"hasMore":false}}`),
		})
		for _, table := range bookingTables {
			rows, err := calcomSync(t, full, table.Name)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 0 || fmt.Sprint(changes.upserts[table.Name]) != fmt.Sprint(rows) {
				t.Fatalf("%s change rows = %v, full rows = %v", table.Name, changes.upserts[table.Name], rows)
			}
		}
	})
	// One list serves all three tables, so it starts at the earliest time any of them holds; a later table reads some rows twice, which changes nothing.
	t.Run("reads from the earliest time the cursor holds", func(t *testing.T) {
		earlier := "2026-10-19T08:00:00Z"
		url, filters := bookingsServer(t, earlier)
		cursor := json.RawMessage(`{"tables":{"bookings":"` + since + `","booking_hosts":"` + earlier + `"}}`)
		_, err := reader(t, url).Changes(ctx, Source{Secret: calcomTestKey}, []Table{{Name: "bookings"}, {Name: "booking_hosts"}}, cursor, &calcomChanges{})
		if err != nil || len(*filters) != 2 {
			t.Fatalf("Changes error = %v, filters = %v", err, *filters)
		}
	})
	// A table with no cursor time has nowhere to read changes from, so it is read in full this sync and followed from the next.
	t.Run("reads a table the cursor has no time for in full", func(t *testing.T) {
		url, _ := bookingsServer(t, since)
		changes := &calcomChanges{}
		next, err := reader(t, url).Changes(ctx, Source{Secret: calcomTestKey}, []Table{{Name: "bookings"}, {Name: "booking_hosts"}},
			cursorAt(since, "bookings"), changes)
		if err != nil {
			t.Fatal(err)
		}
		var cursor calcomCursor
		if err := json.Unmarshal(next, &cursor); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(changes.resets, []string{"booking_hosts"}) || len(cursor.Tables) != 2 ||
			slices.Contains(changes.calls, "where booking_hosts booking_uid=b1") {
			t.Fatalf("resets = %v, calls = %v, cursor = %s", changes.resets, changes.calls, next)
		}
	})
	// A table Cal.com can't list by update time has no changes to read; it is read in full without a bookings request.
	t.Run("reads a table it cannot follow in full", func(t *testing.T) {
		changes := &calcomChanges{}
		_, err := reader(t, "http://unused").Changes(ctx, Source{Secret: calcomTestKey}, []Table{{Name: "event_types"}},
			cursorAt(since, "event_types"), changes)
		if err != nil || !slices.Equal(changes.resets, []string{"event_types"}) || len(changes.calls) != 0 {
			t.Fatalf("Changes error = %v, resets = %v, calls = %v", err, changes.resets, changes.calls)
		}
	})
	// A cursor the reader can't decode leaves no point to resume from, so the syncer reads every table in full.
	t.Run("asks for a full read when the cursor is unreadable", func(t *testing.T) {
		_, err := reader(t, "http://unused").Changes(ctx, Source{Secret: calcomTestKey}, bookingTables, json.RawMessage(`"2026-10-20"`), &calcomChanges{})
		if !errors.Is(err, ErrCursorExpired) {
			t.Fatalf("Changes error = %v, want ErrCursorExpired", err)
		}
	})
	// A rate limit spent part way fails the change read; the cursor stays where it was for the retry.
	t.Run("fails on a spent rate limit", func(t *testing.T) {
		noBackoff(t)
		url := calcomServer(t, map[string]func(*http.Request) (int, string){"/v2/bookings": calcomAnswer(http.StatusTooManyRequests)})
		next, err := reader(t, url).Changes(ctx, Source{Secret: calcomTestKey}, bookingTables, cursorAt(since, "bookings", "booking_attendees", "booking_hosts"), &calcomChanges{})
		var refusedErr *RefusedError
		if !errors.As(err, &refusedErr) || next != nil {
			t.Fatalf("Changes = (%s, %v), want a rate limit", next, err)
		}
	})
}
