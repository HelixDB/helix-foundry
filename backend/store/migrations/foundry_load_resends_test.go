package migrations

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryLoadResends adds resend jobs, the documents a load owes one and what
// became of a release decision in its load, after foundryReviewRelease.
const foundryLoadResends = 20261117000000

// appliedColumns counts the columns a load's decision says what became of it in.
func appliedColumns(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE()
AND table_name = 'foundry_load_decisions' AND column_name IN ('applied_status', 'applied_reason', 'applied_at')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The migration runs where Vitess renamed the CHECK it replaces, and its Down
// where Vitess renamed the one it added. It changes no row either way: a
// resend job left stops the Down at the old CHECK, with everything else in
// place, until a person deletes it.
func TestFoundryLoadResendsMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	hostWorkspaces(t, db)
	if err := goose.UpTo(db, ".", foundryReviewRelease); err != nil {
		t.Fatal(err)
	}
	job := func(id, kind string) error {
		_, err := db.Exec(`INSERT INTO foundry_load_jobs (id, workspace_id, ontology_id, version_id, load_id, kind, status, phase)
VALUES (?, 'w', ?, 'v', 'l', ?, 'succeeded', 'documents')`, id, id, kind)
		return err
	}
	if err := job("before", "resend"); err == nil {
		t.Fatal("a resend job before the migration")
	}
	if err := job("extract", "extract"); err != nil {
		t.Fatal(err)
	}
	// A decision a load copied before the migration: it stays as it is.
	if _, err := db.Exec(`INSERT INTO foundry_load_decisions (load_id, id, ontology_id, kind, type, doc_id, norm_name, decided_by_user_id, decided_at)
VALUES ('l', 'd1', 'o', 'resolve', 'T', 'doc', 'n', 'u', NOW(6))`); err != nil {
		t.Fatal(err)
	}

	vitessChecks(t, db, "foundry_load_jobs")
	if err := goose.UpTo(db, ".", foundryLoadResends); err != nil {
		t.Fatalf("up on a renamed CHECK: %v", err)
	}
	if n := appliedColumns(t, db); n != 3 {
		t.Fatalf("foundry_load_decisions has %d applied columns", n)
	}
	var status sql.NullString
	if err := db.QueryRow(`SELECT applied_status FROM foundry_load_decisions WHERE load_id='l' AND id='d1'`).Scan(&status); err != nil || status.Valid {
		t.Fatalf("a decision copied before = (%v, %v), want no status", status, err)
	}
	if err := job("resend", "resend"); err != nil {
		t.Fatalf("a resend job: %v", err)
	}
	if err := job("other", "backfill"); err == nil {
		t.Fatal("a job of a kind no worker runs")
	}
	// A document owed a resend is owed until a job ends it, and ended once.
	owe := func(doc, status string, ended any) error {
		_, err := db.Exec(`INSERT INTO foundry_load_resends (load_id, document_id, reason, status, ended_at) VALUES ('l', ?, 'approval', ?, ?)`,
			doc, status, ended)
		return err
	}
	if _, err := db.Exec(`INSERT INTO foundry_load_resends (load_id, document_id, reason) VALUES ('l', 'doc', 'approval')`); err != nil {
		t.Fatalf("a document owed a resend: %v", err)
	}
	var owed string
	var requests int
	if err := db.QueryRow(`SELECT status, requests FROM foundry_load_resends WHERE load_id='l' AND document_id='doc'`).Scan(&owed, &requests); err != nil ||
		owed != "owed" || requests != 1 {
		t.Fatalf("owed = (%q, %d, %v)", owed, requests, err)
	}
	if err := owe("doc", "owed", nil); err == nil {
		t.Fatal("a document owed twice for one reason")
	}
	if err := owe("doc-2", "done", nil); err == nil {
		t.Fatal("a resend done with no end")
	}
	if err := owe("doc-3", "sent", nil); err == nil {
		t.Fatal("a resend of an unknown status")
	}
	// Another reason needs no migration: the backend's type holds it.
	if _, err := db.Exec(`INSERT INTO foundry_load_resends (load_id, document_id, reason) VALUES ('l', 'doc', 'backfill')`); err != nil {
		t.Fatalf("a second reason: %v", err)
	}

	// A resend job stops the Down, which deletes nothing, with the rest in place.
	vitessChecks(t, db, "foundry_load_jobs")
	if err := goose.Down(db, "."); err == nil {
		t.Fatal("down with a resend job left")
	}
	if n := appliedColumns(t, db); n != 3 {
		t.Fatalf("a refused down left %d applied columns", n)
	}
	if err := job("still", "resend"); err != nil {
		t.Fatalf("a refused down took the CHECK that allows a resend: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM foundry_load_jobs WHERE kind = 'resend'`); err != nil {
		t.Fatal(err)
	}
	if err := goose.Down(db, "."); err != nil {
		t.Fatalf("down on a renamed CHECK: %v", err)
	}
	if n := appliedColumns(t, db); n != 0 {
		t.Fatalf("foundry_load_decisions kept %d applied columns", n)
	}
	if err := job("after", "resend"); err == nil {
		t.Fatal("a resend job after the down")
	}
	if err := goose.UpTo(db, ".", foundryLoadResends); err != nil {
		t.Fatalf("up again: %v", err)
	}
	if err := job("again", "resend"); err != nil {
		t.Fatalf("a resend job after up again: %v", err)
	}
	// And on plain MySQL, where the CHECK keeps the name the Down gives back.
	if _, err := db.Exec(`DELETE FROM foundry_load_jobs WHERE kind = 'resend'`); err != nil {
		t.Fatal(err)
	}
	if err := goose.Down(db, "."); err != nil {
		t.Fatalf("down on the CHECK's own name: %v", err)
	}
	if err := job("plain", "resend"); err == nil {
		t.Fatal("a resend job after the second down")
	}
	if err := job("apply", "apply"); err != nil {
		t.Fatalf("an apply job after the down: %v", err)
	}
}

// The file's DDL is all a deploy request carries: a DELETE in it would do
// nothing there while goose reports OK.
func TestFoundryLoadResendsMigrationIsDDLOnly(t *testing.T) {
	raw, err := FS.ReadFile("20261117000000_foundry_load_resends.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		if up := strings.ToUpper(line); strings.Contains(up, "DELETE FROM") || strings.Contains(up, "UPDATE FOUNDRY") || strings.Contains(up, "INSERT INTO") {
			t.Fatalf("DML in the migration: %s", line)
		}
	}
}
