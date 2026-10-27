package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"time"
)

// FoundryDocumentSetSpend returns, of the workspace's sets setIDs, those a
// connector owns, by id, with what work no person started spent extracting
// each one's documents in month's UTC month (any time in it;
// RecordFoundryDocumentExtraction adds to it), what it spent on all of the
// workspace's sets of the set's connector kind that month (a deleted
// connector's included, so deleting a source frees nothing), and whether
// any month's sync of the set skipped documents. A set of another
// workspace, a person's set and an unknown id are left out. One query: the
// sets and their connectors by primary key, each set's rows by the spend
// table's primary key, and the workspace's month through its workspace
// index.
func (s *mysqlDB) FoundryDocumentSetSpend(ctx context.Context, workspaceID string, setIDs []string, month time.Time) (
	map[string]model.FoundryDocumentSetSpend, error) {
	out := map[string]model.FoundryDocumentSetSpend{}
	if len(setIDs) == 0 {
		return out, nil
	}
	m := utcMonth(month)
	rows, err := s.db.QueryContext(ctx, `SELECT s.id, c.kind, c.name, COALESCE(p.usd_micros, 0), COALESCE(k.usd_micros, 0),
       EXISTS (SELECT 1 FROM foundry_document_set_spend x WHERE x.set_id=s.id AND x.skipped)
FROM foundry_document_sets s
JOIN connectors c ON c.id=s.connector_id
LEFT JOIN foundry_document_set_spend p ON p.set_id=s.id AND p.month=?
LEFT JOIN (SELECT wc.kind, SUM(w.usd_micros) AS usd_micros FROM foundry_document_set_spend w
           JOIN foundry_document_sets ws ON ws.id=w.set_id
           JOIN connectors wc ON wc.id=ws.connector_id
           WHERE w.workspace_id=? AND w.month=? GROUP BY wc.kind) k ON k.kind=c.kind
WHERE s.id IN (`+idPlaceholders(len(setIDs))+`) AND s.workspace_id=?`,
		append(append([]any{m, workspaceID, m}, toAnySlice(setIDs)...), workspaceID)...)
	if err != nil {
		return nil, fmt.Errorf("read foundry document set spend: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var spend model.FoundryDocumentSetSpend
		if err := rows.Scan(&spend.SetID, &spend.ConnectorKind, &spend.ConnectorName, &spend.USDMicros, &spend.WorkspaceUSDMicros,
			&spend.Skipped); err != nil {
			return nil, err
		}
		out[spend.SetID] = spend
	}
	return out, rows.Err()
}

// SumFoundryDocumentSetSpend is what work no person started spent, in
// month's UTC month (any time in it), extracting the documents of all the
// workspace's sets that connectors of connectorKind own, a deleted
// connector's included: the workspace's side of the kind's allowance
// (model.FoundryDocumentSetSpend.WorkspaceUSDMicros, as
// FoundryDocumentSetSpend reads it for each set). The spend rows through
// the spend table's workspace index, each one's set and connector by
// primary key.
func (s *mysqlDB) SumFoundryDocumentSetSpend(ctx context.Context, workspaceID, connectorKind string, month time.Time) (int64, error) {
	var micros int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(w.usd_micros), 0) FROM foundry_document_set_spend w
JOIN foundry_document_sets s ON s.id=w.set_id
JOIN connectors c ON c.id=s.connector_id
WHERE w.workspace_id=? AND w.month=? AND c.kind=?`, workspaceID, utcMonth(month), connectorKind).Scan(&micros)
	if err != nil {
		return 0, fmt.Errorf("sum foundry document set spend: %w", err)
	}
	return micros, nil
}

// MarkFoundryDocumentSetSkipped records that a sync of a connector's set's
// stream left documents unwritten in month's UTC month (any time in it)
// because work no person started could spend no more on the set (its
// allowance, or the workspace's, was spent), so the next sync with room
// reads in full. Marking again changes nothing. ErrNotFound means the
// workspace has no set with this id that a connector owns; nothing is
// written.
func (s *mysqlDB) MarkFoundryDocumentSetSkipped(ctx context.Context, workspaceID, setID string, month time.Time) error {
	return execOrNotFound(ctx, s.db, `INSERT INTO foundry_document_set_spend (set_id, month, workspace_id, skipped)
SELECT s.id, ?, s.workspace_id, TRUE FROM foundry_document_sets s WHERE s.id=? AND s.workspace_id=? AND s.connector_id IS NOT NULL
ON DUPLICATE KEY UPDATE skipped=TRUE`, utcMonth(month), setID, workspaceID)
}

// ClearFoundryDocumentSetSkipped forgets, in every month, that syncs of a
// connector's set skipped documents, once a full read of its stream has
// published: while workerID holds syncID's lease, and only for a sync of
// the set's connector. ErrNotFound means the lease was lost, or the
// workspace has no set with this id that the sync's connector owns; nothing
// is written.
func (s *mysqlDB) ClearFoundryDocumentSetSkipped(ctx context.Context, workspaceID, setID, syncID, workerID string) error {
	return s.withConnectorSyncLease(ctx, syncID, workerID, func(tx *sql.Tx) error {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT s.id FROM foundry_document_sets s JOIN connector_syncs y ON y.connector_id=s.connector_id
WHERE s.id=? AND s.workspace_id=? AND y.id=?`, setID, workspaceID, syncID).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("read connector document set %s: %w", setID, err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE foundry_document_set_spend SET skipped=FALSE WHERE set_id=? AND skipped`, setID)
		return err
	})
}

// utcMonth is the first day of the UTC month t is in, as a DATE.
func utcMonth(t time.Time) string {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
}
