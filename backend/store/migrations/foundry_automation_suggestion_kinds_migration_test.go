package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryAutomationSuggestionKinds deletes the suggestions written before
// kinds.
const foundryAutomationSuggestionKinds = 20261207000000

// The suggestion kinds migration deletes every cached row, so each version
// is written again with every kind, and keeps the ledger; down and up again
// run cleanly.
func TestFoundryAutomationSuggestionKindsMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", foundryAutomationSuggestionKinds-1); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO foundry_automation_suggestions (ontology_id, version_id, workspace_id, status, questions, suggestions)
VALUES ('o', 'v1', 'w', 'ready', '[]', '[{"title":"Renewal radar","prompt":"Every Monday, list renewals.","icon":"watch"}]')`,
		`INSERT INTO foundry_automation_suggestions (ontology_id, version_id, workspace_id, status, questions) VALUES ('o', 'v2', 'w', 'queued', '[]')`,
		`INSERT INTO foundry_automation_suggestion_ledger (id, workspace_id, kind) VALUES ('l1', 'w', 'queued')`,
		`INSERT INTO foundry_automation_suggestion_ledger (id, workspace_id, kind, usd_micros) VALUES ('l2', 'w', 'charged', 1200)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := goose.UpTo(db, ".", foundryAutomationSuggestionKinds); err != nil {
		t.Fatal(err)
	}
	count := func(table string) int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count("foundry_automation_suggestions"); n != 0 {
		t.Fatalf("%d suggestion rows left", n)
	}
	if n := count("foundry_automation_suggestion_ledger"); n != 2 {
		t.Fatalf("the ledger holds %d entries, not 2", n)
	}
	if err := goose.DownTo(db, ".", foundryAutomationSuggestionKinds-1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := goose.UpTo(db, ".", foundryAutomationSuggestionKinds); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if n := count("foundry_automation_suggestion_ledger"); n != 2 {
		t.Fatalf("the ledger holds %d entries after down and up, not 2", n)
	}
}
