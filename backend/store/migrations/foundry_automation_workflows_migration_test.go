package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryAutomationWorkflows ties workflows and their runs to automations.
const foundryAutomationWorkflows = 20261203000000

// The automation workflows migration goes up and down cleanly, also on a host
// that names its CHECKs as Vitess does (the first 38 characters and a hash),
// which a Down naming them exactly would fail on.
func TestFoundryAutomationWorkflowsMigration(t *testing.T) {
	for _, vitess := range []bool{true, false} {
		t.Run(map[bool]string{true: "vitess names", false: "mysql names"}[vitess], func(t *testing.T) {
			db := isolatedTestDB(t)
			if err := setup(db); err != nil {
				t.Fatal(err)
			}
			if err := goose.UpTo(db, ".", foundryAutomationWorkflows); err != nil {
				t.Fatal(err)
			}
			if vitess {
				if _, err := db.Exec(`ALTER TABLE foundry_workflow_runs
  DROP CHECK foundry_workflow_runs_automation_chk,
  DROP CHECK foundry_workflow_runs_automation_key_chk,
  ADD CONSTRAINT foundry_workflow_runs_automation_chk_5z0d1kqv0b8r2m3n4p5q6r7s8
    CHECK (automation_run_id IS NULL OR started_by_user_id IS NOT NULL),
  ADD CONSTRAINT foundry_workflow_runs_automation_key_ch_9a8b7c6d5e4f3g2h1j0k9l8m7
    CHECK ((automation_run_id IS NULL) = (automation_key IS NULL))`); err != nil {
					t.Fatal(err)
				}
			}
			if err := goose.DownTo(db, ".", foundryAutomationWorkflows-1); err != nil {
				t.Fatalf("down: %v", err)
			}
			var checks int
			if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.table_constraints
WHERE constraint_schema = DATABASE() AND table_name = 'foundry_workflow_runs' AND constraint_name LIKE 'foundry_workflow_runs_automation%'`).Scan(&checks); err != nil {
				t.Fatal(err)
			}
			if checks != 0 {
				t.Fatalf("down left %d automation checks", checks)
			}
			if err := goose.UpTo(db, ".", foundryAutomationWorkflows); err != nil {
				t.Fatalf("up again: %v", err)
			}
		})
	}
}
