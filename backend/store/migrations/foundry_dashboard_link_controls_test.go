package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryDashboardLinkControls adds a public link's expiry and the
// workspace's switch for public links.
const foundryDashboardLinkControls = 20261209000000

// The link controls migration keeps every link as it was (never expiring),
// leaves every workspace with public links on (no settings row), takes an
// older binary's publish, and goes down and up again cleanly.
func TestFoundryDashboardLinkControlsMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", foundryDashboardLinkControls-1); err != nil {
		t.Fatal(err)
	}
	publish := func(id string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO foundry_dashboard_shares
  (dashboard_id, workspace_id, token_sha256, token_sealed, token_key_id, snapshot, spec_revision, ontology_ids, published_by_user_id,
   published_at, first_published_at)
VALUES (?, 'WS1', SHA2(?, 256), 'sealed', 'k1', '{}', 1, '[]', 'u1', NOW(6), NOW(6))`, id, id); err != nil {
			t.Fatal(err)
		}
	}
	publish("D1")
	if err := goose.UpTo(db, ".", foundryDashboardLinkControls); err != nil {
		t.Fatal(err)
	}
	var nullable string
	if err := db.QueryRow(`SELECT is_nullable FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'foundry_dashboard_shares'
AND column_name = 'expires_at'`).Scan(&nullable); err != nil {
		t.Fatalf("foundry_dashboard_shares.expires_at: %v", err)
	}
	if nullable != "YES" {
		t.Fatal("foundry_dashboard_shares.expires_at is not nullable")
	}
	// An older binary's publish names no expiry.
	publish("D2")
	var never, settings int
	if err := db.QueryRow(`SELECT COUNT(*) FROM foundry_dashboard_shares WHERE expires_at IS NULL`).Scan(&never); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM foundry_dashboard_settings`).Scan(&settings); err != nil {
		t.Fatal(err)
	}
	if never != 2 || settings != 0 {
		t.Fatalf("%d of 2 links never expire; %d settings rows, want none", never, settings)
	}
	if _, err := db.Exec(`INSERT INTO foundry_dashboard_settings (workspace_id, public_links, updated_by_user_id) VALUES ('WS1', FALSE, 'u1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE foundry_dashboard_shares SET expires_at = NOW(6) WHERE dashboard_id = 'D2'`); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownTo(db, ".", foundryDashboardLinkControls-1); err != nil {
		t.Fatalf("down: %v", err)
	}
	var left, tables, rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'foundry_dashboard_shares'
AND column_name = 'expires_at'`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'foundry_dashboard_settings'`).
		Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM foundry_dashboard_shares`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if left != 0 || tables != 0 || rows != 2 {
		t.Fatalf("down left %d columns and %d tables, and kept %d of 2 links", left, tables, rows)
	}
	if err := goose.UpTo(db, ".", foundryDashboardLinkControls); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
