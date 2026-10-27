package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"io"
	"strings"
)

// BackfillFoundryDocumentReads queues the standard read of every uploaded
// table file and PDF that has none: those uploaded before completing an
// upload queued it, which the document worker would otherwise never read.
// It skips the documents of an ontology deleted before documents were the
// workspace's, which workspace-foundry-documents removes. A read already
// there is left as it is, so a re-run is safe.
func BackfillFoundryDocumentReads(ctx context.Context, db *sql.DB, apply bool, out io.Writer) error {
	types := model.FoundryMediaTypesRead(model.FoundryReadTable, model.FoundryReadPDF)
	args := make([]any, len(types))
	for i, t := range types {
		args[i] = string(t)
	}
	unread := ` FROM foundry_documents d
WHERE d.status='uploaded' AND d.media_type IN (` + strings.TrimSuffix(strings.Repeat("?, ", len(types)), ", ") + `)
  AND NOT EXISTS (SELECT 1 FROM foundry_document_reads r WHERE r.document_id=d.id AND r.mode='standard')
  AND NOT EXISTS (SELECT 1 FROM foundry_ontologies gone WHERE gone.id=d.ontology_id AND gone.deleted_at IS NOT NULL)`
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*)`+unread, args...).Scan(&n); err != nil {
		return fmt.Errorf("count unread documents: %w", err)
	}
	if !apply {
		fmt.Fprintf(out, "%d documents would get a queued read; re-run with --apply to write\n", n)
		return nil
	}
	res, err := db.ExecContext(ctx, `
INSERT INTO foundry_document_reads (document_id, mode, workspace_id, status)
SELECT d.id, 'standard', d.workspace_id, 'queued'`+unread+`
ON DUPLICATE KEY UPDATE document_id=foundry_document_reads.document_id`, args...)
	if err != nil {
		return fmt.Errorf("queue document reads: %w", err)
	}
	queued, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("count queued document reads: %w", err)
	}
	fmt.Fprintf(out, "queued %d document reads\n", queued)
	return nil
}
