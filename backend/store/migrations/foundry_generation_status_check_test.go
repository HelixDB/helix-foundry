package migrations

import (
	"database/sql"
	"testing"

	"github.com/pressly/goose/v3"
)

const (
	// foundryDecisionNameKeys is the migration before generation questions.
	foundryDecisionNameKeys = 20261130000000
	// foundryGenerationQuestions widens the status check, by a prefix a
	// Vitess host's names miss.
	foundryGenerationQuestions = 20261130100000
	// foundryGenerationStatusCheck drops every status check, however named.
	foundryGenerationStatusCheck = 20261202100000
)

// statusChecks returns the status checks foundry_ontology_generations holds.
func statusChecks(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT constraint_name FROM information_schema.table_constraints
WHERE constraint_schema = DATABASE() AND table_name = 'foundry_ontology_generations'
  AND constraint_type = 'CHECK' AND constraint_name LIKE 'foundry_ontology_generations_status_ch%'
ORDER BY constraint_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	return names
}

// On a host that names checks as Vitess does (the first 38 characters and a
// hash), 20261130100000 leaves the old status check, which refuses
// needs_input and cancelled; 20261202100000 drops it, and on any host leaves
// exactly the wide check, which refuses an unknown status.
func TestFoundryGenerationStatusCheckMigration(t *testing.T) {
	for _, vitess := range []bool{true, false} {
		t.Run(map[bool]string{true: "vitess names", false: "mysql names"}[vitess], func(t *testing.T) {
			db := isolatedTestDB(t)
			if err := setup(db); err != nil {
				t.Fatal(err)
			}
			if err := goose.UpTo(db, ".", foundryDecisionNameKeys); err != nil {
				t.Fatal(err)
			}
			exec := func(query string, args ...any) error {
				_, err := db.Exec(query, args...)
				return err
			}
			if vitess {
				if err := exec(`ALTER TABLE foundry_ontology_generations
  DROP CHECK foundry_ontology_generations_status_chk,
  ADD CONSTRAINT foundry_ontology_generations_status_ch_1uj2fris1di3lsi2unz9apmom
    CHECK (status IN ('queued', 'running', 'succeeded', 'failed'))`); err != nil {
					t.Fatal(err)
				}
			}
			if err := goose.UpTo(db, ".", foundryGenerationQuestions); err != nil {
				t.Fatal(err)
			}
			if err := exec(`INSERT INTO foundry_ontology_generations (id, workspace_id, ontology_id, status, input)
VALUES ('g1', 'w1', 'o1', 'running', '{}')`); err != nil {
				t.Fatal(err)
			}
			asks := func() error {
				return exec(`UPDATE foundry_ontology_generations SET status='needs_input', turn_state='s', draft_rev=1,
  questions='[]', asked_at=NOW(6), rounds=1 WHERE id='g1'`)
			}
			if err := asks(); (err == nil) == vitess {
				t.Fatalf("vitess=%v: before the fix, asking: %v", vitess, err)
			}
			if err := goose.UpTo(db, ".", foundryGenerationStatusCheck); err != nil {
				t.Fatal(err)
			}
			if got := statusChecks(t, db); len(got) != 1 || got[0] != "foundry_ontology_generations_status_chk" {
				t.Fatalf("vitess=%v: status checks %v, want only the wide one", vitess, got)
			}
			if err := asks(); err != nil {
				t.Fatalf("vitess=%v: a generation can't wait for answers: %v", vitess, err)
			}
			if err := exec(`UPDATE foundry_ontology_generations SET status='cancelled', turn_state=NULL WHERE id='g1'`); err != nil {
				t.Fatalf("vitess=%v: a generation can't be cancelled: %v", vitess, err)
			}
			if err := exec(`UPDATE foundry_ontology_generations SET status='paused' WHERE id='g1'`); err == nil {
				t.Fatalf("vitess=%v: an unknown status was stored", vitess)
			}
			if err := goose.DownTo(db, ".", foundryGenerationQuestions); err != nil {
				t.Fatal(err)
			}
			if got := statusChecks(t, db); len(got) != 1 {
				t.Fatalf("vitess=%v: down left status checks %v", vitess, got)
			}
		})
	}
}
