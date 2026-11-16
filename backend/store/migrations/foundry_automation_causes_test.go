package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryAutomationCauses adds what an automation's run started: the run and
// chain depth on syncs and loads, and the job an action step waits for.
const foundryAutomationCauses = 20261205000000

// causeColumns are the columns the migration adds, by table.
var causeColumns = map[string][]string{
	"connector_syncs":              {"started_by_automation_run_id", "automation_chain_depth"},
	"foundry_loads":                {"started_by_automation_run_id", "automation_chain_depth"},
	"foundry_automation_run_nodes": {"job_id"},
	"foundry_ontologies":           {"refresh_cause_run_id", "refresh_cause_depth"},
}

// The automation causes migration adds nullable columns only, which rows an
// older binary writes leave NULL, and goes down and up again cleanly.
func TestFoundryAutomationCausesMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", foundryAutomationCauses-1); err != nil {
		t.Fatal(err)
	}
	var checksBefore int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.table_constraints WHERE constraint_schema = DATABASE() AND constraint_type = 'CHECK'`).
		Scan(&checksBefore); err != nil {
		t.Fatal(err)
	}
	// A sync, a load and a step written before it.
	for _, q := range []string{
		`INSERT INTO connector_syncs (id, connector_id, workspace_id, status) VALUES ('SYNC1', 'CON1', 'WS1', 'succeeded')`,
		`INSERT INTO foundry_loads (id, workspace_id, ontology_id, version_id, status, finished_at) VALUES ('LOAD1', 'WS1', 'ONT1', 'V1', 'ready', NOW(6))`,
		`INSERT INTO foundry_automation_run_nodes (run_id, node_id, status) VALUES ('RUN1', 'load', 'executing')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := goose.UpTo(db, ".", foundryAutomationCauses); err != nil {
		t.Fatal(err)
	}
	for table, columns := range causeColumns {
		for _, column := range columns {
			var nullable string
			if err := db.QueryRow(`SELECT is_nullable FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`,
				table, column).Scan(&nullable); err != nil {
				t.Fatalf("%s.%s: %v", table, column, err)
			}
			if nullable != "YES" {
				t.Fatalf("%s.%s is not nullable", table, column)
			}
		}
	}
	var checksAfter int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.table_constraints WHERE constraint_schema = DATABASE() AND constraint_type = 'CHECK'`).
		Scan(&checksAfter); err != nil {
		t.Fatal(err)
	}
	if checksAfter != checksBefore {
		t.Fatalf("it added %d CHECK constraints; it adds columns only", checksAfter-checksBefore)
	}
	var unset int
	if err := db.QueryRow(`SELECT
  (SELECT COUNT(*) FROM connector_syncs WHERE started_by_automation_run_id IS NULL AND automation_chain_depth IS NULL) +
  (SELECT COUNT(*) FROM foundry_loads WHERE started_by_automation_run_id IS NULL AND automation_chain_depth IS NULL) +
  (SELECT COUNT(*) FROM foundry_automation_run_nodes WHERE job_id IS NULL)`).Scan(&unset); err != nil {
		t.Fatal(err)
	}
	if unset != 3 {
		t.Fatalf("%d of the 3 earlier rows are unset", unset)
	}
	// An older binary's inserts name none of the columns.
	for _, q := range []string{
		`INSERT INTO connector_syncs (id, connector_id, workspace_id, status, active_key) VALUES ('SYNC2', 'CON1', 'WS1', 'queued', 1)`,
		`INSERT INTO foundry_loads (id, workspace_id, ontology_id, version_id, status) VALUES ('LOAD2', 'WS1', 'ONT1', 'V1', 'writing')`,
		`UPDATE foundry_loads SET started_by_automation_run_id = 'RUN1', automation_chain_depth = 1 WHERE id = 'LOAD2'`,
		`UPDATE foundry_automation_run_nodes SET status = 'running', job_id = 'LOAD2' WHERE run_id = 'RUN1'`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := goose.DownTo(db, ".", foundryAutomationCauses-1); err != nil {
		t.Fatalf("down: %v", err)
	}
	var left int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE()
AND column_name IN ('started_by_automation_run_id', 'automation_chain_depth', 'job_id', 'refresh_cause_run_id', 'refresh_cause_depth')
AND table_name IN ('connector_syncs', 'foundry_loads', 'foundry_automation_run_nodes', 'foundry_ontologies')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Fatalf("down left %d columns", left)
	}
	var rows int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM connector_syncs) + (SELECT COUNT(*) FROM foundry_loads)`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 4 {
		t.Fatalf("down kept %d of 4 rows", rows)
	}
	if err := goose.UpTo(db, ".", foundryAutomationCauses); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
