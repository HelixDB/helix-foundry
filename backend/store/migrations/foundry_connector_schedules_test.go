package migrations

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

func TestScheduleFoundryConnectors(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", 20261101000000); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	connector := func(id, extra string) {
		t.Helper()
		mustExec(`INSERT INTO connectors (id, workspace_id, kind, name, config, secret_ciphertext, secret_key_id, secret_hint)
VALUES (?, 'w', 'stripe', ?, CAST('{}' AS JSON), 'x', 'k1', 'h')`, id, id)
		if extra != "" {
			mustExec(`UPDATE connectors SET `+extra+` WHERE id=?`, id)
		}
	}
	// next is how many seconds from now the connector's first sync is, measured
	// by the database: the test connection reads DATETIMEs as bytes.
	next := func(id string) sql.NullInt64 {
		t.Helper()
		var in sql.NullInt64
		if err := db.QueryRow(`SELECT TIMESTAMPDIFF(SECOND, NOW(6), next_sync_at) FROM connectors WHERE id=?`, id).Scan(&in); err != nil {
			t.Fatal(err)
		}
		return in
	}
	connector("plain", "")
	connector("manual", "sync_every_seconds=0")
	connector("paused", "schedule_paused='Stripe rejected this key'")
	connector("deleted", "deleted_at=NOW(6)")
	connector("busy", "")
	mustExec(`INSERT INTO connector_syncs (id, connector_id, workspace_id, status, active_key) VALUES ('s1', 'busy', 'w', 'queued', 1)`)

	// A dry run says what it would do and changes nothing.
	var dry bytes.Buffer
	if err := ScheduleFoundryConnectors(ctx, db, false, &dry); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dry.String(), "would schedule 1 connectors") || next("plain").Valid {
		t.Fatalf("dry run = %q", dry.String())
	}
	// Only a connector that would otherwise never be scheduled gets a first sync, within the hour.
	var applied bytes.Buffer
	if err := ScheduleFoundryConnectors(ctx, db, true, &applied); err != nil {
		t.Fatal(err)
	}
	if in := next("plain"); !in.Valid || in.Int64 < -60 || in.Int64 > 3660 {
		t.Fatalf("plain next sync in %+v seconds, want within the hour", in)
	}
	for _, id := range []string{"manual", "paused", "deleted", "busy"} {
		if next(id).Valid {
			t.Fatalf("%s was scheduled", id)
		}
	}
	var again bytes.Buffer
	if err := ScheduleFoundryConnectors(ctx, db, true, &again); err != nil || !strings.Contains(again.String(), "scheduled 0 connectors") {
		t.Fatalf("re-run = (%q, %v)", again.String(), err)
	}
}
