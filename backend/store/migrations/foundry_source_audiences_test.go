package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// foundrySourceAudiences adds the tables of what connectors' sources share
// with whom, and of the items grantees' answers and threads read.
const foundrySourceAudiences = 20261217000000

// The source audiences migration adds six tables and changes nothing that
// exists, refuses a read with neither a time nor a problem, a problem
// without its time, or a read without its digest, matches emails and item
// keys byte for byte, and goes down and up again cleanly.
func TestFoundrySourceAudiencesMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", foundrySourceAudiences-1); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", foundrySourceAudiences); err != nil {
		t.Fatal(err)
	}
	tables := []string{"foundry_source_audiences", "foundry_source_people", "foundry_source_items", "foundry_source_item_members",
		"foundry_question_source_items", "foundry_agent_thread_source_items"}
	var after, added int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name IN (?, ?, ?, ?, ?, ?)`,
		tables[0], tables[1], tables[2], tables[3], tables[4], tables[5]).Scan(&added); err != nil {
		t.Fatal(err)
	}
	if added != 21 || after != before+added {
		t.Fatalf("the migration added %d columns, %d of them its own tables'; want 21, all of them", after-before, added)
	}
	for _, ok := range []string{
		`INSERT INTO foundry_source_audiences (connector_id, workspace_id, read_at, digest) VALUES ('C1', 'W1', NOW(6), UNHEX(SHA2('x', 256)))`,
		`INSERT INTO foundry_source_audiences (connector_id, workspace_id, problem, problem_at) VALUES ('C2', 'W1', 'no scope', NOW(6))`,
		`INSERT INTO foundry_source_people (connector_id, email, guest) VALUES ('C1', 'jose@example.com', FALSE), ('C1', 'josé@example.com', TRUE)`,
		`INSERT INTO foundry_source_items (connector_id, item_key, everyone) VALUES ('C1', 'c1', TRUE), ('C1', 'C1', FALSE)`,
		`INSERT INTO foundry_source_item_members (connector_id, item_key, email) VALUES ('C1', 'C1', 'jose@example.com')`,
		`INSERT INTO foundry_question_source_items (question_id, connector_id, item_key) VALUES ('Q1', 'C1', 'c1'), ('Q1', 'C1', 'C1')`,
		`INSERT INTO foundry_agent_thread_source_items (thread_id, connector_id, item_key) VALUES ('T1', 'C1', 'c1'), ('T1', 'C1', 'C1')`,
	} {
		if _, err := db.Exec(ok); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	for _, refused := range []string{
		`INSERT INTO foundry_source_audiences (connector_id, workspace_id) VALUES ('C3', 'W1')`,
		`INSERT INTO foundry_source_audiences (connector_id, workspace_id, read_at, problem) VALUES ('C3', 'W1', NOW(6), 'x')`,
		`INSERT INTO foundry_source_audiences (connector_id, workspace_id, read_at, problem_at) VALUES ('C3', 'W1', NOW(6), NOW(6))`,
		`INSERT INTO foundry_source_audiences (connector_id, workspace_id, read_at) VALUES ('C3', 'W1', NOW(6))`,
		`INSERT INTO foundry_source_audiences (connector_id, workspace_id, digest, problem, problem_at) VALUES ('C3', 'W1', UNHEX(SHA2('x', 256)), 'x', NOW(6))`,
		`INSERT INTO foundry_question_source_items (question_id, connector_id, item_key) VALUES ('Q1', 'C1', 'c1')`,
		`INSERT INTO foundry_source_people (connector_id, email, guest) VALUES ('C1', 'jose@example.com', TRUE)`,
	} {
		if _, err := db.Exec(refused); err == nil {
			t.Fatalf("%s was taken", refused)
		}
	}
	var matched int
	if err := db.QueryRow(`SELECT COUNT(*) FROM foundry_source_people WHERE connector_id = 'C1' AND email = 'JOSE@example.com'`).Scan(&matched); err != nil {
		t.Fatal(err)
	}
	if matched != 0 {
		t.Fatal("an email matched another case")
	}
	if err := goose.DownTo(db, ".", foundrySourceAudiences-1); err != nil {
		t.Fatalf("down: %v", err)
	}
	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name IN (?, ?, ?, ?, ?, ?)`,
		tables[0], tables[1], tables[2], tables[3], tables[4], tables[5]).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("down left %d tables", left)
	}
	if err := goose.UpTo(db, ".", foundrySourceAudiences); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
