package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// foundrySlack creates the Slack tables and foundry_agent_turns.surface.
const foundrySlack = 20261213000000

// The Slack tables go up and down cleanly: a team and a workspace hold one
// live install each, which a revoke frees; a turn queued before the
// migration reads the site; intake dedupes and a thread maps once.
func TestFoundrySlackMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", foundrySlack-1); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) error {
		_, err := db.Exec(query, args...)
		return err
	}
	if err := exec(`INSERT INTO foundry_agent_turns (id, workspace_id, thread_id, user_id, runtime, kind, input, status, max_usd_micros)
VALUES ('turn-before', 'ws1', 'thread1', 'user-ada', 'api', 'message', 'hi', 'queued', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", foundrySlack); err != nil {
		t.Fatal(err)
	}
	var surface string
	if err := db.QueryRow(`SELECT surface FROM foundry_agent_turns WHERE id='turn-before'`).Scan(&surface); err != nil || surface != "site" {
		t.Fatalf("a turn from before reads %q, %v", surface, err)
	}

	install := func(id, workspace, team string) error {
		return exec(`INSERT INTO foundry_slack_installs (id, workspace_id, team_id, team_name, app_id, bot_user_id, scopes, installed_by_user_id,
  installed_by_slack_user_id) VALUES (?, ?, ?, 'Acme', 'A1', 'UBOT', 'chat:write', 'user-ada', 'UADA')`, id, workspace, team)
	}
	if err := install("i1", "ws1", "T1"); err != nil {
		t.Fatal(err)
	}
	if err := install("i2", "ws2", "T1"); err == nil {
		t.Fatal("a team held two live installs")
	}
	if err := install("i3", "ws1", "T2"); err == nil {
		t.Fatal("a workspace held two live installs")
	}
	if err := exec(`UPDATE foundry_slack_installs SET revoked_at=NOW(6) WHERE id='i1'`); err != nil {
		t.Fatal(err)
	}
	if err := install("i2", "ws2", "T1"); err != nil {
		t.Fatalf("a revoked install frees its team: %v", err)
	}
	if err := install("i3", "ws1", "T2"); err != nil {
		t.Fatalf("a revoked install frees its workspace: %v", err)
	}
	if err := exec(`INSERT INTO foundry_slack_install_tokens (install_id, token_sealed, key_id) VALUES ('i2', x'00', 'k1')`); err != nil {
		t.Fatal(err)
	}

	intake := func(id, key string) error {
		return exec(`INSERT INTO foundry_slack_intake (id, install_id, dedupe_key, kind, payload) VALUES (?, 'i2', ?, 'event', '{}')`, id, key)
	}
	if err := intake("in1", "e:Ev1"); err != nil {
		t.Fatal(err)
	}
	if err := intake("in2", "e:Ev1"); err == nil {
		t.Fatal("an event was taken twice")
	}
	if err := exec(`INSERT INTO foundry_slack_intake (id, install_id, dedupe_key, kind, payload) VALUES ('in3', 'i2', ?, 'interaction', '{}')`,
		"i:"+"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("a click's key fits: %v", err)
	}
	thread := func(id, agent string) error {
		return exec(`INSERT INTO foundry_slack_threads (id, install_id, channel_id, thread_ts, slack_user_id, agent_thread_id)
VALUES (?, 'i2', 'C1', '1712345678.000100', 'UADA', ?)`, id, agent)
	}
	if err := thread("s1", "a1"); err != nil {
		t.Fatal(err)
	}
	if err := thread("s2", "a2"); err == nil {
		t.Fatal("a Slack thread continued two Founder threads for one person")
	}
	for _, stmt := range []string{
		`INSERT INTO foundry_slack_install_states (state_hash, workspace_id, user_id, expires_at) VALUES (REPEAT('a', 64), 'ws1', 'user-ada', NOW(6))`,
		`INSERT INTO foundry_slack_links (install_id, slack_user_id, user_id) VALUES ('i2', 'UADA', 'user-ada')`,
		`INSERT INTO foundry_slack_link_tokens (token_hash, install_id, slack_user_id, expires_at) VALUES (REPEAT('b', 64), 'i2', 'UADA', NOW(6))`,
		`INSERT INTO foundry_slack_replies (turn_id, install_id, channel_id, thread_ts, slack_user_id, next_poll_at)
VALUES ('turn-before', 'i2', 'C1', '1712345678.000100', 'UADA', NOW(6))`,
	} {
		if err := exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	if err := goose.DownTo(db, ".", foundrySlack-1); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"foundry_slack_installs", "foundry_slack_install_tokens", "foundry_slack_install_states", "foundry_slack_links",
		"foundry_slack_link_tokens", "foundry_slack_threads", "foundry_slack_intake", "foundry_slack_replies"} {
		if err := exec(`SELECT 1 FROM ` + table); err == nil {
			t.Fatalf("%s is still there after down", table)
		}
	}
	if err := exec(`SELECT surface FROM foundry_agent_turns`); err == nil {
		t.Fatal("foundry_agent_turns.surface is still there after down")
	}
	if err := goose.UpTo(db, ".", foundrySlack); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
