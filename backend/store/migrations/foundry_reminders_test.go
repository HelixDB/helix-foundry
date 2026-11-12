package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryReminders creates foundry_reminders.
const foundryReminders = 20261119000000

// The reminders' table goes up and down cleanly, keeps one row per address
// byte for byte, and its checks refuse what no layer writes: a schedule with
// no time, an end with one, a stop with no reason, a failure with no error,
// a claim of a schedule that ended (but one stopped while it sent), and
// reminders sent with no time.
func TestFoundryRemindersMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", foundryReminders); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) error {
		_, err := db.Exec(query, args...)
		return err
	}
	row := func(id, email, status string, next, stopped, reason, lastError any) error {
		return exec(`INSERT INTO foundry_reminders (id, email, time_zone, consent, status, next_send_at, stopped_at, stop_reason, last_error)
VALUES (?, ?, 'Europe/London', 'remind-v1', ?, ?, ?, ?, ?)`, id, email, status, next, stopped, reason, lastError)
	}
	const at = "2026-11-11 09:00:00"
	for _, refused := range []struct {
		name                             string
		status                           string
		next, stopped, reason, lastError any
	}{
		{"scheduled with no time", "scheduled", nil, nil, nil, nil},
		{"done with a time", "done", at, nil, nil, nil},
		{"an unknown status", "paused", nil, nil, nil, nil},
		{"stopped with no reason", "stopped", nil, at, nil, nil},
		{"stopped for an unknown reason", "stopped", nil, at, "bored", nil},
		{"a reason while scheduled", "scheduled", at, nil, "signed_in", nil},
		{"failed with no error", "failed", nil, nil, nil, nil},
	} {
		if err := row("r-refused", "ada@example.com", refused.status, refused.next, refused.stopped, refused.reason, refused.lastError); err == nil {
			t.Fatalf("%s was stored", refused.name)
		}
	}
	for _, kept := range []struct {
		status                           string
		next, stopped, reason, lastError any
	}{
		{"scheduled", at, nil, nil, nil},
		{"done", nil, nil, nil, nil},
		{"stopped", nil, at, "signed_in", nil},
		{"failed", nil, nil, nil, "throttled"},
	} {
		if err := row("r-1", "ada@example.com", kept.status, kept.next, kept.stopped, kept.reason, kept.lastError); err != nil {
			t.Fatalf("%s: %v", kept.status, err)
		}
		if err := exec(`DELETE FROM foundry_reminders`); err != nil {
			t.Fatal(err)
		}
	}
	if err := row("r-1", "ada@example.com", "scheduled", at, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := row("r-2", "ada@example.com", "scheduled", at, nil, nil, nil); err == nil {
		t.Fatal("an address held two rows")
	}
	if err := row("r-3", "Ada@example.com", "scheduled", at, nil, nil, nil); err != nil {
		t.Fatalf("addresses are compared byte for byte (the store lowercases them): %v", err)
	}
	if err := exec(`UPDATE foundry_reminders SET sent=1 WHERE id='r-1'`); err == nil {
		t.Fatal("a reminder counted sent with no time")
	}
	if err := exec(`UPDATE foundry_reminders SET claim_token='t' WHERE id='r-1'`); err == nil {
		t.Fatal("a claim with no lease")
	}
	if err := exec(`UPDATE foundry_reminders SET status='done', next_send_at=NULL, claim_token='t', claimed_until=NOW(6) WHERE id='r-1'`); err == nil {
		t.Fatal("a claim of a schedule that ended")
	}
	if err := exec(`UPDATE foundry_reminders SET status='stopped', next_send_at=NULL, stopped_at=NOW(6), stop_reason='signed_in',
  claim_token='t', claimed_until=NOW(6), client_key='q3ZyY1J8b0m2Xw' WHERE id='r-1'`); err != nil {
		t.Fatalf("a schedule stopped while its reminder is sent keeps the claim: %v", err)
	}
	if err := goose.DownTo(db, ".", foundryReminders-1); err != nil {
		t.Fatal(err)
	}
	if err := exec(`SELECT 1 FROM foundry_reminders`); err == nil {
		t.Fatal("foundry_reminders is still there after down")
	}
}
