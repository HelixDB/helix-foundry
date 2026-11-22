package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// foundrySlackFeed creates the Slack data connector's tables and columns.
const foundrySlackFeed = 20261214000000

// The feed's tables go up and down cleanly: event sources become pushed
// and nothing else does; a connector, an install and a Grid org hold one
// live feed each, which a deleting one frees; an install from before
// reads no bot id and no admin installer.
func TestFoundrySlackFeedMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", foundrySlackFeed-1); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) error {
		_, err := db.Exec(query, args...)
		return err
	}
	for _, c := range [][2]string{{"c-events", "events"}, {"c-slack", "slack"}} {
		if err := exec(`INSERT INTO connectors (id, workspace_id, kind, name, config, secret_ciphertext, secret_key_id, secret_hint)
VALUES (?, 'ws1', ?, ?, '{}', x'00', 'k1', '')`, c[0], c[1], c[0]); err != nil {
			t.Fatal(err)
		}
	}
	if err := exec(`INSERT INTO foundry_slack_installs (id, workspace_id, team_id, team_name, app_id, bot_user_id, scopes, installed_by_user_id,
  installed_by_slack_user_id) VALUES ('i1', 'ws1', 'T1', 'Acme', 'A1', 'UBOT', 'chat:write', 'user-ada', 'UADA')`); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", foundrySlackFeed); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"c-events": true, "c-slack": false} {
		var pushed bool
		if err := db.QueryRow(`SELECT pushed FROM connectors WHERE id=?`, id).Scan(&pushed); err != nil || pushed != want {
			t.Fatalf("%s pushed %v, %v", id, pushed, err)
		}
	}
	var botID string
	var admin bool
	if err := db.QueryRow(`SELECT bot_id, installer_admin FROM foundry_slack_installs WHERE id='i1'`).Scan(&botID, &admin); err != nil ||
		botID != "" || admin {
		t.Fatalf("an install from before reads bot %q, admin %v: %v", botID, admin, err)
	}

	feed := func(id, connector, install, enterprise string) error {
		return exec(`INSERT INTO foundry_slack_feeds (id, connector_id, workspace_id, team_id, enterprise_id, install_id, notice_version, bound_by)
VALUES (?, ?, 'ws1', 'T1', ?, ?, 1, 'user-ada')`, id, connector, enterprise, install)
	}
	if err := feed("f1", "c-slack", "i1", "E1"); err != nil {
		t.Fatal(err)
	}
	for _, dup := range [][3]string{{"c-slack", "i2", ""}, {"c-other", "i1", ""}, {"c-other", "i2", "E1"}} {
		if err := feed("f2", dup[0], dup[1], dup[2]); err == nil {
			t.Fatalf("two live feeds of %v", dup)
		}
	}
	if err := feed("f2", "c-other", "i2", ""); err != nil {
		t.Fatalf("feeds outside an org: %v", err)
	}
	if err := exec(`UPDATE foundry_slack_feeds SET deleting_at=NOW(6) WHERE id='f1'`); err != nil {
		t.Fatal(err)
	}
	if err := feed("f3", "c-slack", "i1", "E1"); err != nil {
		t.Fatalf("a deleting feed frees its connector, install and org: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO foundry_slack_slots (install_id, method) VALUES ('i1', 'history')`,
		`INSERT INTO foundry_slack_feed (feed_id, seq, dedupe_key, channel_id, day, kind, source, payload, bytes)
VALUES ('f3', 1, 'e:Ev1', 'C1', '2026-11-20', 'message', 'event', '{}', 2)`,
		`INSERT INTO foundry_slack_feed_channels (feed_id, channel_id, covered_from) VALUES ('f3', 'C1', NOW(6))`,
		`INSERT INTO foundry_slack_feed_days (feed_id, channel_id, day, object_key, digest, messages, bytes, state, updated_seq)
VALUES ('f3', 'C1', '2026-11-20', 'k', REPEAT('0', 64), 1, 2, 'partial', 1)`,
		`INSERT INTO foundry_slack_backfill (feed_id, channel_id, oldest, reason, latest) VALUES ('f3', 'C1', NOW(6) - INTERVAL 1 DAY, 'bound', NOW(6))`,
		`INSERT INTO foundry_slack_backfill_threads (feed_id, channel_id, thread_ts, day) VALUES ('f3', 'C1', '1.1', '2026-11-20')`,
		`INSERT INTO foundry_slack_gap_budget (feed_id, day) VALUES ('f3', '2026-11-20')`,
		`UPDATE foundry_slack_threads SET bot_at=NOW(6), others_at=NOW(6)`,
		`UPDATE foundry_slack_install_states SET return_path='/data?add=slack'`,
	} {
		if err := exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := exec(`INSERT INTO foundry_slack_feed (feed_id, seq, dedupe_key, channel_id, day, kind, source, payload, bytes)
VALUES ('f3', 2, 'e:Ev1', 'C1', '2026-11-20', 'message', 'event', '{}', 2)`); err == nil {
		t.Fatal("an event was kept twice")
	}

	if err := goose.DownTo(db, ".", foundrySlackFeed-1); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"foundry_slack_slots", "foundry_slack_feeds", "foundry_slack_feed", "foundry_slack_feed_channels",
		"foundry_slack_feed_days", "foundry_slack_backfill", "foundry_slack_backfill_threads", "foundry_slack_gap_budget"} {
		if err := exec(`SELECT 1 FROM ` + table); err == nil {
			t.Fatalf("%s is still there after down", table)
		}
	}
	for _, column := range []string{"connectors.pushed", "foundry_slack_installs.bot_id", "foundry_slack_installs.installer_admin",
		"foundry_slack_installs.events_limited_at", "foundry_slack_install_states.return_path", "foundry_slack_threads.bot_at",
		"foundry_slack_threads.others_at"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND CONCAT(table_name, '.', column_name)=?`,
			column).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s is still there after down (%d, %v)", column, n, err)
		}
	}
	if err := goose.UpTo(db, ".", foundrySlackFeed); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
