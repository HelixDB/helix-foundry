package migrations

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// foundryDocumentCauses adds the cause of the sync that wrote a connector's
// document: the automation run that started it and the chain's depth.
const foundryDocumentCauses = 20261208000000

// The document causes migration adds two nullable columns, which rows an
// older binary writes leave NULL, and goes down and up again cleanly.
func TestFoundryDocumentCausesMigration(t *testing.T) {
	db := isolatedTestDB(t)
	if err := setup(db); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpTo(db, ".", foundryDocumentCauses-1); err != nil {
		t.Fatal(err)
	}
	insert := func(id string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO foundry_documents (id, workspace_id, set_id, name, media_type, size_bytes, sha256, client_token, status, s3_key)
VALUES (?, 'WS1', 'SET1', ?, 'text/markdown', 1, REPEAT('a', 64), ?, 'pending_upload', ?)`, id, id+".md", id, "ws/"+id); err != nil {
			t.Fatal(err)
		}
	}
	insert("DOC1")
	if err := goose.UpTo(db, ".", foundryDocumentCauses); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"started_by_automation_run_id", "automation_chain_depth"} {
		var nullable string
		if err := db.QueryRow(`SELECT is_nullable FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'foundry_documents' AND column_name = ?`,
			column).Scan(&nullable); err != nil {
			t.Fatalf("foundry_documents.%s: %v", column, err)
		}
		if nullable != "YES" {
			t.Fatalf("foundry_documents.%s is not nullable", column)
		}
	}
	// An older binary's insert names neither column.
	insert("DOC2")
	var unset int
	if err := db.QueryRow(`SELECT COUNT(*) FROM foundry_documents WHERE started_by_automation_run_id IS NULL AND automation_chain_depth IS NULL`).
		Scan(&unset); err != nil {
		t.Fatal(err)
	}
	if unset != 2 {
		t.Fatalf("%d of the 2 rows are unset", unset)
	}
	if _, err := db.Exec(`UPDATE foundry_documents SET started_by_automation_run_id = 'RUN1', automation_chain_depth = 1 WHERE id = 'DOC2'`); err != nil {
		t.Fatal(err)
	}
	if err := goose.DownTo(db, ".", foundryDocumentCauses-1); err != nil {
		t.Fatalf("down: %v", err)
	}
	var left, rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'foundry_documents'
AND column_name IN ('started_by_automation_run_id', 'automation_chain_depth')`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM foundry_documents`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if left != 0 || rows != 2 {
		t.Fatalf("down left %d columns and kept %d of 2 rows", left, rows)
	}
	if err := goose.UpTo(db, ".", foundryDocumentCauses); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
