// Package migrations is Foundry's schema: its goose migrations, embedded,
// the checks a run of them needs first (Preflight), and the data steps a
// migration cannot carry. A host runs them in one goose sequence and version
// table with its own migrations; versions increase across both.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
)

// FS holds the migrations, named <version>_<name>.sql at its root.
//
//go:embed *.sql
var FS embed.FS

// foundryDocumentSetRequired makes foundry_documents.set_id NOT NULL.
const foundryDocumentSetRequired = 20261028000000

// foundryWorkspaceDocuments makes a document's client token unique per
// workspace (foundry_documents_workspace_client_token_uq).
const foundryWorkspaceDocuments = 20261031000200

// Preflight refuses a migration run whose pending migrations the
// destination's rows would fail. MySQL refuses them itself, but a deploy that
// applies the migrations to a copy of the schema with no rows and then the
// schema diff to the destination does not, so it runs this against the
// destination first. It reads the versions applied from goose's version
// table (goose_db_version). Query errors fail closed. It refuses:
//   - foundry_documents.set_id NOT NULL (foundryDocumentSetRequired) while
//     a document there has no set: one written before document sets
//     (20261025000000), which backfill-foundry-document-sets, in the
//     releases before workspace documents, moves into its ontology's
//     General set;
//   - a unique client token per workspace (foundryWorkspaceDocuments) while
//     two documents of a workspace share one, in the column's collation.
//
// A database that has applied a migration, or has no documents table,
// passes its check.
func Preflight(ctx context.Context, db *sql.DB) error {
	var versions int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='goose_db_version'`).Scan(&versions); err != nil {
		return err
	}
	applied := func(version int64) (bool, error) {
		if versions == 0 {
			return false, nil
		}
		var n int
		err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM goose_db_version WHERE version_id=? AND is_applied`, version).Scan(&n)
		return n > 0, err
	}
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='foundry_documents'`).Scan(&tables); err != nil {
		return err
	}
	if tables == 0 {
		return nil
	}
	required, err := applied(foundryDocumentSetRequired)
	if err != nil {
		return err
	}
	if !required {
		var columns int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='foundry_documents' AND column_name='set_id'`).Scan(&columns); err != nil {
			return err
		}
		// Before document sets every document would have none.
		missing := `SELECT COUNT(*) FROM foundry_documents`
		if columns > 0 {
			missing += ` WHERE set_id IS NULL`
		}
		var unset int64
		if err := db.QueryRowContext(ctx, missing).Scan(&unset); err != nil {
			return err
		}
		if unset > 0 && columns == 0 {
			return fmt.Errorf("foundry document set preflight: %d documents predate document sets; deploy 20261025000000 (document sets) in a release of its own, run backfill-foundry-document-sets --apply, verify 0 NULL set_id, then deploy the set_id contraction", unset)
		}
		if unset > 0 {
			return fmt.Errorf("foundry document set preflight: %d NULL set_id rows; run backfill-foundry-document-sets --apply (a release before workspace documents has it), verify 0 NULLs, then deploy the set_id contraction", unset)
		}
	}
	unique, err := applied(foundryWorkspaceDocuments)
	if err != nil || unique {
		return err
	}
	var repeated int64
	var example sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(CONCAT(workspace_id, '/', client_token)) FROM (
  SELECT workspace_id, MIN(client_token) AS client_token FROM foundry_documents GROUP BY workspace_id, client_token HAVING COUNT(*) > 1
) repeats`).Scan(&repeated, &example); err != nil {
		return err
	}
	if repeated > 0 {
		return fmt.Errorf("foundry workspace documents preflight: %d client tokens repeat within a workspace (first %s); a document's token must be unique in its workspace before %d", repeated, example.String, foundryWorkspaceDocuments)
	}
	return nil
}
