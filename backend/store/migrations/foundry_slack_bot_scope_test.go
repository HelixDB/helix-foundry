package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// foundrySlackBotScope gives a Slack install's shared bot its scope.
const foundrySlackBotScope = 20261227000000

// A bot from before, and an install per person, read as the workspace's
// scope; a scope is written as the store writes it; and the migration goes
// down and up cleanly.
func TestFoundrySlackBotScopeMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	hostWorkspaces(t, db)
	if err := goose.UpTo(db, ".", foundrySlackBotScope-1); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) error {
		_, err := db.Exec(query, args...)
		return err
	}
	for _, stmt := range []string{
		`INSERT INTO foundry_slack_installs (id, workspace_id, team_id, team_name, app_id, bot_user_id, scopes, installed_by_user_id,
  installed_by_slack_user_id, bot_credential_id) VALUES ('bot', 'ws1', 'T1', 'Acme', 'A1', 'UBOT', 'chat:write', 'user-ada', 'UADA',
  '01CREDENTIAL0000000000000A')`,
		`INSERT INTO foundry_slack_installs (id, workspace_id, team_id, team_name, app_id, bot_user_id, scopes, installed_by_user_id,
  installed_by_slack_user_id) VALUES ('person', 'ws2', 'T2', 'Globex', 'A1', 'UBOT', 'chat:write', 'user-ada', 'UADA')`,
	} {
		if err := exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := goose.UpTo(db, ".", foundrySlackBotScope); err != nil {
		t.Fatal(err)
	}
	var ok bool
	if err := db.QueryRow(`SELECT
  (SELECT bot_scope='workspace' FROM foundry_slack_installs WHERE id='bot')
  AND (SELECT bot_scope='workspace' FROM foundry_slack_installs WHERE id='person')`).Scan(&ok); err != nil || !ok {
		t.Fatalf("installs from before read as the workspace's scope: %v, %v", ok, err)
	}
	if err := exec(`UPDATE foundry_slack_installs SET bot_scope='sources' WHERE id='bot'`); err != nil {
		t.Fatal(err)
	}

	if err := goose.DownTo(db, ".", foundrySlackBotScope-1); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns
WHERE table_schema=DATABASE() AND table_name='foundry_slack_installs' AND column_name='bot_scope'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("bot_scope is still there after down (%d, %v)", n, err)
	}
	if err := goose.UpTo(db, ".", foundrySlackBotScope); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := db.QueryRow(`SELECT bot_scope='workspace' FROM foundry_slack_installs WHERE id='bot'`).Scan(&ok); err != nil || !ok {
		t.Fatalf("up again reads the workspace's scope: %v, %v", ok, err)
	}
}
