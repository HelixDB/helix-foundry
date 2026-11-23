package migrations

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryAutomationReviewsWaiting widens foundry_automations' trigger and
// cursor checks to the reviews_waiting kind, after review advice.
const foundryAutomationReviewsWaiting = 20261220000000

// automationKindChecks returns foundry_automations' checks of the prefix.
func automationKindChecks(t *testing.T, db *sql.DB, prefix string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT constraint_name FROM information_schema.table_constraints
WHERE constraint_schema = DATABASE() AND table_name = 'foundry_automations'
  AND constraint_type = 'CHECK' AND constraint_name LIKE ?
ORDER BY constraint_name`, prefix+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// The migration takes an active reviews_waiting automation, with its target
// and cursor, and still refuses one without its cursor and an unknown kind,
// on a host that names checks as MySQL does and on one that names them as
// Vitess does (the first 38 characters and a hash), leaving exactly one
// check of each prefix.
func TestFoundryAutomationReviewsWaitingMigration(t *testing.T) {
	for _, vitess := range []bool{true, false} {
		t.Run(map[bool]string{true: "vitess names", false: "mysql names"}[vitess], func(t *testing.T) {
			db := isolatedTestDB(t)
			if err := setup(db); err != nil {
				t.Fatal(err)
			}
			if err := goose.UpTo(db, ".", foundryReviewAdvice); err != nil {
				t.Fatal(err)
			}
			exec := func(query string, args ...any) error {
				_, err := db.Exec(query, args...)
				return err
			}
			if vitess {
				if err := exec(`ALTER TABLE foundry_automations
  DROP CHECK foundry_automations_trigger_chk,
  DROP CHECK foundry_automations_cursor_chk,
  ADD CONSTRAINT foundry_automations_trigger_chk_7f1k2m3n4p5q6r7s8t9u0v1w2 CHECK (
    trigger_kind IN ('schedule', 'sync_finished', 'load_ready', 'document_arrived', 'generation_finished', 'manual') AND
    (trigger_target IS NULL) = (trigger_kind IN ('schedule', 'manual'))),
  ADD CONSTRAINT foundry_automations_cursor_chk_3a4b5c6d7e8f9g0h1i2j3k4l5 CHECK (
    (cursor_at IS NOT NULL) = (status = 'active' AND trigger_kind IN ('sync_finished', 'load_ready', 'document_arrived', 'generation_finished')))`); err != nil {
					t.Fatal(err)
				}
			}
			insert := func(id, kind, target string, cursor bool) error {
				cursorAt := "NULL"
				if cursor {
					cursorAt = "NOW(6)"
				}
				return exec(`INSERT INTO foundry_automations (id, workspace_id, owner_user_id, name, description, spec, revision, spec_revision,
  approved_spec_revision, approved_at, trigger_kind, trigger_target, status, cursor_at, client_token)
VALUES (?, 'WS1', 'U1', 'Reviews', '', '{}', 1, 1, 1, NOW(6), ?, ?, 'active', `+cursorAt+`, ?)`, id, kind, target, id)
			}
			if err := insert("A0", "reviews_waiting", "ONT1", true); err == nil {
				t.Fatalf("vitess=%v: a reviews_waiting automation was stored before the migration", vitess)
			}
			if err := goose.UpTo(db, ".", foundryAutomationReviewsWaiting); err != nil {
				t.Fatal(err)
			}
			if got := automationKindChecks(t, db, "foundry_automations_trigger_ch"); len(got) != 1 || got[0] != "foundry_automations_trigger_chk" {
				t.Fatalf("vitess=%v: trigger checks %v, want only the wide one", vitess, got)
			}
			if got := automationKindChecks(t, db, "foundry_automations_cursor_ch"); len(got) != 1 || got[0] != "foundry_automations_cursor_chk" {
				t.Fatalf("vitess=%v: cursor checks %v, want only the wide one", vitess, got)
			}
			if err := insert("A1", "reviews_waiting", "ONT1", true); err != nil {
				t.Fatalf("vitess=%v: an active reviews_waiting automation: %v", vitess, err)
			}
			if err := insert("A2", "reviews_waiting", "ONT1", false); err == nil {
				t.Fatalf("vitess=%v: an active reviews_waiting automation was stored without its cursor", vitess)
			}
			if err := insert("A3", "reviews_piling", "ONT1", true); err == nil {
				t.Fatalf("vitess=%v: an unknown kind was stored", vitess)
			}
			if err := insert("A4", "load_ready", "ONT1", true); err != nil {
				t.Fatalf("vitess=%v: an earlier kind: %v", vitess, err)
			}
		})
	}
}
