package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// foundrySlackRelay adds the relay's tables, relayed installs, local access
// and read-only turns.
const foundrySlackRelay = 20261216000000

// Rows from before keep their meaning: an install serves its own workspace
// and stays its one live install, a link is full, a turn may act. A
// workspace holds one live install per destination; the migration goes down
// and up cleanly.
func TestFoundrySlackRelayMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", foundrySlackRelay-1); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) error {
		_, err := db.Exec(query, args...)
		return err
	}
	install := func(id, workspace, team string) error {
		return exec(`INSERT INTO foundry_slack_installs (id, workspace_id, team_id, team_name, app_id, bot_user_id, scopes, installed_by_user_id,
  installed_by_slack_user_id) VALUES (?, ?, ?, 'Acme', 'A1', 'UBOT', 'chat:write', 'user-ada', 'UADA')`, id, workspace, team)
	}
	for _, stmt := range []string{
		`INSERT INTO foundry_agent_threads (id, workspace_id, user_id, title) VALUES ('th-before', 'ws1', 'user-ada', 'Hi')`,
		`INSERT INTO foundry_agent_turns (id, workspace_id, thread_id, user_id, runtime, kind, input, status, max_usd_micros)
VALUES ('turn-before', 'ws1', 'th-before', 'user-ada', 'api', 'message', 'hi', 'queued', 1)`,
		`INSERT INTO foundry_slack_links (install_id, slack_user_id, user_id) VALUES ('i1', 'UADA', 'user-ada')`,
	} {
		if err := exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := install("i1", "ws1", "T1"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", foundrySlackRelay); err != nil {
		t.Fatal(err)
	}
	var ok bool
	if err := db.QueryRow(`SELECT
  (SELECT relay_link_id IS NULL AND relay_workspace_id IS NULL AND NOT feed_on AND relay_feed_rows=0 AND live_destination='ws1/'
     FROM foundry_slack_installs WHERE id='i1')
  AND (SELECT access='full' FROM foundry_slack_links WHERE install_id='i1')
  AND (SELECT NOT read_only FROM foundry_agent_turns WHERE id='turn-before')`).Scan(&ok); err != nil || !ok {
		t.Fatalf("rows from before keep their meaning: %v, %v", ok, err)
	}

	// One live install per destination: the workspace's own, and one per
	// local workspace of its links.
	if err := install("i2", "ws1", "T2"); err == nil {
		t.Fatal("two live installs of a workspace's own")
	}
	relayed := func(id, local, team string) error {
		return exec(`INSERT INTO foundry_slack_installs (id, workspace_id, relay_link_id, relay_workspace_id, team_id, team_name, app_id, bot_user_id,
  scopes, installed_by_user_id, installed_by_slack_user_id) VALUES (?, 'ws1', 'l1', ?, ?, 'Acme', 'A1', 'UBOT', 'chat:write', 'user-ada', 'UADA')`,
			id, local, team)
	}
	if err := relayed("i2", "local1", "T2"); err != nil {
		t.Fatalf("a relayed install beside the workspace's own: %v", err)
	}
	if err := relayed("i3", "local1", "T3"); err == nil {
		t.Fatal("two live installs of a local workspace")
	}
	if err := relayed("i3", "local2", "T1"); err == nil {
		t.Fatal("a team installed twice")
	}
	if err := exec(`UPDATE foundry_slack_installs SET revoked_at=NOW(6) WHERE id='i2'`); err != nil {
		t.Fatal(err)
	}
	if err := relayed("i3", "local1", "T3"); err != nil {
		t.Fatalf("a revoked install frees its destination: %v", err)
	}
	for _, stmt := range []string{
		`INSERT INTO foundry_relay_links (id, workspace_id, created_by_user_id, name, token_hash, redirect_origin)
VALUES ('l1', 'ws1', 'user-ada', 'Mac', REPEAT('a', 64), 'http://localhost:3000')`,
		`INSERT INTO foundry_relay_codes (code_hash, workspace_id, user_id, code_challenge, redirect_uri, name, expires_at)
VALUES (REPEAT('c', 64), 'ws1', 'user-ada', REPEAT('x', 43), 'http://localhost:3000/settings/slack', 'Mac', NOW(6))`,
		`INSERT INTO foundry_relay_deliveries (id, link_id, install_id, relay_workspace_id, dedupe_key, kind, payload, expires_at)
VALUES ('d1', 'l1', 'i3', 'local1', 'e:Ev1', 'event', '{}', NOW(6))`,
		`INSERT INTO foundry_relay_feed (id, link_id, install_id, relay_workspace_id, dedupe_key, channel_id, day, kind, payload, bytes, expires_at)
VALUES ('f1', 'l1', 'i3', 'local1', 'e:Ev2', 'C1', '2026-11-20', 'message', '{}', 2, NOW(6))`,
		`INSERT INTO foundry_relay_channels (install_id, channel_id, last_at) VALUES ('i3', 'C1', NOW(6))`,
		`INSERT INTO foundry_relay_users (install_id, slack_user_id, last_at) VALUES ('i3', 'UADA', NOW(6))`,
		`INSERT INTO foundry_relay_usage (link_id, day, deliveries) VALUES ('l1', '2026-11-20', 1)`,
		`INSERT INTO foundry_slack_access_requests (install_id, slack_user_id, slack_name) VALUES ('i1', 'UGRACE', 'Grace')`,
		`UPDATE foundry_slack_install_states SET relay_link_id='l1', relay_workspace_id='local1', return_to='http://localhost:3000/settings/slack'`,
		`UPDATE foundry_agent_turns SET read_only=TRUE`,
	} {
		if err := exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	for _, dup := range []string{
		`INSERT INTO foundry_relay_links (id, workspace_id, created_by_user_id, name, token_hash, redirect_origin)
VALUES ('l2', 'ws1', 'user-ada', 'Mac', REPEAT('a', 64), 'http://localhost:3000')`,
		`INSERT INTO foundry_relay_deliveries (id, link_id, install_id, relay_workspace_id, dedupe_key, kind, expires_at)
VALUES ('d2', 'l1', 'i3', 'local1', 'e:Ev1', 'event', NOW(6))`,
		`INSERT INTO foundry_relay_feed (id, link_id, install_id, relay_workspace_id, dedupe_key, channel_id, day, kind, payload, bytes, expires_at)
VALUES ('f2', 'l1', 'i3', 'local1', 'e:Ev2', 'C1', '2026-11-20', 'message', '{}', 2, NOW(6))`,
	} {
		if err := exec(dup); err == nil {
			t.Fatalf("kept twice: %s", dup)
		}
	}

	// Down needs one live install per workspace again.
	if err := exec(`UPDATE foundry_slack_installs SET revoked_at=NOW(6) WHERE relay_link_id IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownTo(db, ".", foundrySlackRelay-1); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"foundry_relay_links", "foundry_relay_codes", "foundry_relay_deliveries", "foundry_relay_feed",
		"foundry_relay_channels", "foundry_relay_users", "foundry_relay_usage", "foundry_slack_access_requests"} {
		if err := exec(`SELECT 1 FROM ` + table); err == nil {
			t.Fatalf("%s is still there after down", table)
		}
	}
	for _, column := range []string{"foundry_slack_installs.relay_link_id", "foundry_slack_installs.relay_workspace_id", "foundry_slack_installs.feed_on",
		"foundry_slack_installs.relay_feed_rows", "foundry_slack_installs.live_destination", "foundry_slack_install_states.relay_link_id",
		"foundry_slack_install_states.relay_workspace_id", "foundry_slack_install_states.return_to", "foundry_slack_links.access",
		"foundry_agent_turns.read_only"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND CONCAT(table_name, '.', column_name)=?`,
			column).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s is still there after down (%d, %v)", column, n, err)
		}
	}
	if err := install("i4", "ws1", "T4"); err == nil {
		t.Fatal("down: two live installs of a workspace")
	}
	if err := goose.UpTo(db, ".", foundrySlackRelay); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
