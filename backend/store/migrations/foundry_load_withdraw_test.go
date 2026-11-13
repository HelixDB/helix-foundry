package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

const (
	// foundryWorkflowRunsStartedIdx is the migration before foundryLoadWithdraw.
	foundryWorkflowRunsStartedIdx = 20261123000000
	// foundryLoadWithdraw widens a load decision's applied_status for a
	// withdraw's outcome (contract 4.16.0).
	foundryLoadWithdraw = 20261125000000
)

// The migration widens applied_status so a withdraw's outcome, the engine's
// word, fits, and changes no row. Its Down narrows it again only once no
// longer status is left: a withdraw's stops it until a person deletes those
// rows, as the rollback note says.
func TestFoundryLoadWithdrawMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	hostWorkspaces(t, db)
	if err := goose.UpTo(db, ".", foundryWorkflowRunsStartedIdx); err != nil {
		t.Fatal(err)
	}
	insert := func(id, kind, status string) error {
		_, err := db.Exec(`INSERT INTO foundry_load_decisions (load_id, id, ontology_id, kind, type, doc_id, decided_by_user_id, decided_at, applied_status)
VALUES ('l', ?, 'o', ?, '', 'd', 'u', NOW(6), ?)`, id, kind, status)
		return err
	}
	if err := insert("r1", "release", "written"); err != nil {
		t.Fatalf("a written release before the migration: %v", err)
	}
	if err := insert("w0", "withdraw", "withdrawn"); err == nil {
		t.Fatal("a withdraw's outcome before the migration")
	}

	if err := goose.UpTo(db, ".", foundryLoadWithdraw); err != nil {
		t.Fatalf("up: %v", err)
	}
	var status string
	if err := db.QueryRow(`SELECT applied_status FROM foundry_load_decisions WHERE id='r1'`).Scan(&status); err != nil || status != "written" {
		t.Fatalf("the release it held = (%q, %v)", status, err)
	}
	for id, s := range map[string]string{"w1": "withdrawn", "w2": "not_written", "w3": "pending"} {
		if err := insert(id, "withdraw", s); err != nil {
			t.Fatalf("a withdraw %s: %v", s, err)
		}
	}

	if err := goose.DownTo(db, ".", foundryWorkflowRunsStartedIdx); err == nil {
		t.Fatal("down narrowed the column under a withdraw's outcome")
	}
	if _, err := db.Exec(`DELETE FROM foundry_load_decisions WHERE kind='withdraw'`); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownTo(db, ".", foundryWorkflowRunsStartedIdx); err != nil {
		t.Fatalf("down once the withdraws are gone: %v", err)
	}
	if err := db.QueryRow(`SELECT applied_status FROM foundry_load_decisions WHERE id='r1'`).Scan(&status); err != nil || status != "written" {
		t.Fatalf("after down, the release = (%q, %v)", status, err)
	}
}
