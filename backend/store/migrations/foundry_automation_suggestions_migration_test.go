package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryAutomationSuggestions adds the suggestions for a new automation and
// their ledger.
const foundryAutomationSuggestions = 20261204000000

// The automation suggestions migration goes up and down cleanly, also on a
// host that names its CHECKs as Vitess does (the first 38 characters and a
// hash), and its CHECKs hold.
func TestFoundryAutomationSuggestionsMigration(t *testing.T) {
	for _, vitess := range []bool{true, false} {
		t.Run(map[bool]string{true: "vitess names", false: "mysql names"}[vitess], func(t *testing.T) {
			db := isolatedTestDB(t)
			if err := setup(db); err != nil {
				t.Fatal(err)
			}
			if err := goose.UpTo(db, ".", foundryAutomationSuggestions); err != nil {
				t.Fatal(err)
			}
			for _, bad := range []string{
				`INSERT INTO foundry_automation_suggestions (ontology_id, version_id, workspace_id, status, questions) VALUES ('o', 'v1', 'w', 'done', '[]')`,
				`INSERT INTO foundry_automation_suggestions (ontology_id, version_id, workspace_id, status, questions) VALUES ('o', 'v2', 'w', 'ready', '[]')`,
				`INSERT INTO foundry_automation_suggestions (ontology_id, version_id, workspace_id, status, questions, suggestions)
VALUES ('o', 'v3', 'w', 'queued', '[]', '[]')`,
				`INSERT INTO foundry_automation_suggestion_ledger (id, workspace_id, kind) VALUES ('l1', 'w', 'refunded')`,
				`INSERT INTO foundry_automation_suggestion_ledger (id, workspace_id, kind, usd_micros) VALUES ('l2', 'w', 'charged', -1)`,
			} {
				if _, err := db.Exec(bad); err == nil {
					t.Fatalf("a CHECK let through %s", bad)
				}
			}
			for _, good := range []string{
				`INSERT INTO foundry_automation_suggestions (ontology_id, version_id, workspace_id, status, questions, suggestions)
VALUES ('o', 'v5', 'w', 'ready', '[]', '[]')`,
				`INSERT INTO foundry_automation_suggestion_ledger (id, workspace_id, kind) VALUES ('l3', 'w', 'queued')`,
				`INSERT INTO foundry_automation_suggestion_ledger (id, workspace_id, kind, usd_micros) VALUES ('l4', 'w', 'charged', 1200)`,
			} {
				if _, err := db.Exec(good); err != nil {
					t.Fatal(err)
				}
			}
			if vitess {
				for _, rename := range []string{`ALTER TABLE foundry_automation_suggestions
  DROP CHECK foundry_automation_suggestions_status_chk,
  DROP CHECK foundry_automation_suggestions_ready_chk,
  ADD CONSTRAINT foundry_automation_suggestions_status__5z0d1kqv0b8r2m3n4p5q6r7s8
    CHECK (status IN ('queued', 'running', 'ready', 'failed')),
  ADD CONSTRAINT foundry_automation_suggestions_ready_c_9a8b7c6d5e4f3g2h1j0k9l8m7
    CHECK ((status = 'ready') = (suggestions IS NOT NULL))`, `ALTER TABLE foundry_automation_suggestion_ledger
  DROP CHECK foundry_automation_suggestion_ledger_kind_chk,
  DROP CHECK foundry_automation_suggestion_ledger_spend_chk,
  ADD CONSTRAINT foundry_automation_suggestion_ledger_k_1q2w3e4r5t6y7u8i9o0p1a2s3
    CHECK (kind IN ('queued', 'charged')),
  ADD CONSTRAINT foundry_automation_suggestion_ledger_s_6y7u8i9o0p1a2s3d4f5g6h7j8
    CHECK (usd_micros >= 0)`} {
					if _, err := db.Exec(rename); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := goose.DownTo(db, ".", foundryAutomationSuggestions-1); err != nil {
				t.Fatalf("down: %v", err)
			}
			var left int
			if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.table_constraints
WHERE constraint_schema = DATABASE() AND constraint_name LIKE 'foundry_automation_suggestion%'`).Scan(&left); err != nil {
				t.Fatal(err)
			}
			if left != 0 {
				t.Fatalf("down left %d checks", left)
			}
			if err := goose.UpTo(db, ".", foundryAutomationSuggestions); err != nil {
				t.Fatalf("up again: %v", err)
			}
		})
	}
}
