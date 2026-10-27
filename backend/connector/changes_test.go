package connector

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestKeepLastReset(t *testing.T) {
	type status struct {
		Position    string     `json:"position"`
		LastResetAt *time.Time `json:"last_reset_at"`
		LastReset   string     `json:"last_reset"`
	}
	decode := func(t *testing.T, cursor json.RawMessage) status {
		t.Helper()
		var c struct {
			CDC status `json:"cdc"`
		}
		if err := json.Unmarshal(cursor, &c); err != nil {
			t.Fatalf("decode %s: %v", cursor, err)
		}
		return c.CDC
	}
	prev := json.RawMessage(`{"lsn":"0/1","cdc":{"position":"s at 0/1","last_reset_at":"2026-10-22T20:56:26Z","last_reset":"orders: truncate"}}`)

	// A reconcile or a manual full read is not a reset, so the status keeps
	// saying when the log last could not be followed.
	t.Run("carries the previous reset across a full read", func(t *testing.T) {
		next := KeepLastReset(json.RawMessage(`{"lsn":"0/9","cdc":{"position":"s at 0/9"}}`), prev, nil)
		got := decode(t, next)
		if got.Position != "s at 0/9" || got.LastReset != "orders: truncate" || got.LastResetAt == nil || !got.LastResetAt.Equal(time.Date(2026, 10, 22, 20, 56, 26, 0, time.UTC)) {
			t.Fatalf("status = %+v", got)
		}
		var c map[string]json.RawMessage
		if err := json.Unmarshal(next, &c); err != nil || string(c["lsn"]) != `"0/9"` {
			t.Fatalf("cursor = %s", next)
		}
	})
	// A purged binlog or a dropped slot is the reset the customer needs to see:
	// it is why every table was copied again.
	t.Run("records the lost position that forced the full read", func(t *testing.T) {
		before := time.Now().Add(-time.Second)
		got := decode(t, KeepLastReset(json.RawMessage(`{"cdc":{"position":"s at 0/9"}}`), prev, &PositionLostError{Reason: "missing"}))
		if got.LastReset != "missing" || got.LastResetAt == nil || got.LastResetAt.Before(before) {
			t.Fatalf("status = %+v", got)
		}
	})
	// Start sees what the change read cannot, such as a slot that lost WAL.
	t.Run("keeps a reset the full read recorded itself", func(t *testing.T) {
		next := json.RawMessage(`{"cdc":{"position":"s at 0/9","last_reset_at":"2026-10-23T00:00:00Z","last_reset":"lost"}}`)
		if got := KeepLastReset(next, prev, &PositionLostError{Reason: "missing"}); string(got) != string(next) {
			t.Fatalf("cursor = %s", got)
		}
	})
	// An expiry that is not a lost position, such as CDC turned on, records nothing new.
	t.Run("treats a plain expiry as no reset", func(t *testing.T) {
		got := decode(t, KeepLastReset(json.RawMessage(`{"cdc":{"position":"s at 0/9"}}`), prev, ErrCursorExpired))
		if got.LastReset != "orders: truncate" {
			t.Fatalf("status = %+v", got)
		}
	})
	// Kinds that follow no log, and connectors leaving CDC, have no status to fill.
	t.Run("leaves a cursor without a CDC status alone", func(t *testing.T) {
		for _, next := range []json.RawMessage{nil, json.RawMessage(`"c0"`), json.RawMessage(`{"columns":{"orders":null}}`)} {
			if got := KeepLastReset(next, prev, &PositionLostError{Reason: "missing"}); string(got) != string(next) {
				t.Fatalf("KeepLastReset(%s) = %s", next, got)
			}
		}
	})
	// The first full read of a log has no earlier reset to show.
	t.Run("adds nothing when there was no earlier reset", func(t *testing.T) {
		next := json.RawMessage(`{"cdc":{"position":"s at 0/9"}}`)
		if got := KeepLastReset(next, nil, nil); string(got) != string(next) {
			t.Fatalf("cursor = %s", got)
		}
	})
	// A connector's lost position must still be recognised as an expired cursor.
	t.Run("is an expired cursor", func(t *testing.T) {
		if !errors.Is(&PositionLostError{Reason: "missing"}, ErrCursorExpired) {
			t.Fatal("a lost position is not ErrCursorExpired")
		}
	})
}
