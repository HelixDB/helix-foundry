package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryAgentPrincipals gives Founder's rows a principal, a bot's actions
// no confirmation, installs a bot credential, and bot-mode threads their
// people.
const foundryAgentPrincipals = 20261215000000

// Rows from before are a person's; a bot's action holds no confirmation;
// a thread keeps each person once; and the migration goes down and up
// cleanly.
func TestFoundryAgentPrincipalsMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", foundryAgentPrincipals-1); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) error {
		_, err := db.Exec(query, args...)
		return err
	}
	for _, stmt := range []string{
		`INSERT INTO foundry_agent_threads (id, workspace_id, user_id, title) VALUES ('th-before', 'ws1', 'user-ada', 'Hi')`,
		`INSERT INTO foundry_agent_turns (id, workspace_id, thread_id, user_id, runtime, kind, input, status, max_usd_micros)
VALUES ('turn-before', 'ws1', 'th-before', 'user-ada', 'api', 'message', 'hi', 'queued', 1)`,
		`INSERT INTO foundry_agent_actions (id, turn_id, thread_id, workspace_id, user_id, operation, target, payload, payload_hash, display,
  target_named_by_user, confirmation_id, confirmation_token_sealed, confirmation_key_id, status, expires_at)
VALUES ('act-before', 'turn-before', 'th-before', 'ws1', 'user-ada', 'create_load', 't', '{}', REPEAT('a', 64), '{}', FALSE, 'c', x'01', 'k',
  'prepared', NOW(6))`,
		`INSERT INTO foundry_slack_installs (id, workspace_id, team_id, team_name, app_id, bot_user_id, scopes, installed_by_user_id,
  installed_by_slack_user_id) VALUES ('i1', 'ws1', 'T1', 'Acme', 'A1', 'UBOT', 'chat:write', 'user-ada', 'UADA')`,
	} {
		if err := exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := exec(`INSERT INTO foundry_agent_actions (id, turn_id, thread_id, workspace_id, user_id, operation, target, payload, payload_hash,
  display, target_named_by_user, status, expires_at) VALUES ('act-early', 'turn-before', 'th-before', 'ws1', 'u', 'o', 't', '{}', REPEAT('b', 64),
  '{}', FALSE, 'prepared', NOW(6))`); err == nil {
		t.Fatal("an action without a confirmation before the migration")
	}
	if err := goose.UpTo(db, ".", foundryAgentPrincipals); err != nil {
		t.Fatal(err)
	}
	var ok bool
	if err := db.QueryRow(`SELECT
  (SELECT principal_kind='user' FROM foundry_agent_threads WHERE id='th-before')
  AND (SELECT principal_kind='user' FROM foundry_agent_turns WHERE id='turn-before')
  AND (SELECT principal_kind='user' AND confirmation_id='c' FROM foundry_agent_actions WHERE id='act-before')
  AND (SELECT bot_credential_id IS NULL FROM foundry_slack_installs WHERE id='i1')`).Scan(&ok); err != nil || !ok {
		t.Fatalf("rows from before read as a person's, per person: %v, %v", ok, err)
	}
	for _, stmt := range []string{
		`INSERT INTO foundry_agent_actions (id, turn_id, thread_id, workspace_id, principal_kind, user_id, operation, target, payload, payload_hash,
  display, target_named_by_user, status, expires_at) VALUES ('act-bot', 'turn-before', 'th-before', 'ws1', 'service_credential', 'cred1', 'o', 't',
  '{}', REPEAT('b', 64), '{}', FALSE, 'prepared', NOW(6))`,
		`UPDATE foundry_slack_installs SET bot_credential_id='01CREDENTIAL0000000000000A' WHERE id='i1'`,
		`INSERT INTO foundry_slack_thread_people (thread_id, slack_user_id) VALUES ('m1', 'UADA'), ('m1', 'UGRACE')`,
	} {
		if err := exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := exec(`INSERT INTO foundry_slack_thread_people (thread_id, slack_user_id) VALUES ('m1', 'UADA')`); err == nil {
		t.Fatal("a person kept twice in a thread")
	}

	if err := goose.DownTo(db, ".", foundryAgentPrincipals-1); err != nil {
		t.Fatal(err)
	}
	if err := exec(`SELECT 1 FROM foundry_slack_thread_people`); err == nil {
		t.Fatal("foundry_slack_thread_people is still there after down")
	}
	for _, column := range []string{"foundry_agent_threads.principal_kind", "foundry_agent_turns.principal_kind", "foundry_agent_actions.principal_kind",
		"foundry_slack_installs.bot_credential_id"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND CONCAT(table_name, '.', column_name)=?`,
			column).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s is still there after down (%d, %v)", column, n, err)
		}
	}
	if err := goose.UpTo(db, ".", foundryAgentPrincipals); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
