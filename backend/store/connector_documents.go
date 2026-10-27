package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"slices"
	"strings"
	"time"
)

// ErrReservationExpired means a publish came more than half the purge
// sweep's grace after its documents' ids were reserved: the sweep may be
// deleting their bytes, so nothing of the publish is written. The sync's
// next attempt puts them again under new ids, and the sweep deletes these.
var ErrReservationExpired = errors.New("store: reservation expired")

// EnsureConnectorDocumentSet makes the set of set.Owner's connector and
// stream live with set's name and description, while workerID holds syncID's
// lease, and returns its id: set.ID when the stream had no set, else the id
// its set has kept since. A connector's set lives as long as the connector,
// so a sync runs this for every stream it writes. ErrNotFound means the
// lease was lost or the connector is deleted.
func (s *mysqlDB) EnsureConnectorDocumentSet(ctx context.Context, set *model.FoundryDocumentSet, syncID, workerID string) (string, error) {
	if set.Owner == nil || set.CreatedByUserID != nil {
		return "", fmt.Errorf("ensure connector document set: a connector's set has its owner and no creator")
	}
	var id string
	err := s.withConnectorSyncLease(ctx, syncID, workerID, func(tx *sql.Tx) error {
		if err := shareLiveConnector(ctx, tx, set.WorkspaceID, set.Owner.ConnectorID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO foundry_document_sets (id, workspace_id, connector_id, stream, name, description, live_key)
VALUES (?, ?, ?, ?, ?, ?, 1)
ON DUPLICATE KEY UPDATE name=VALUES(name), description=VALUES(description)`,
			set.ID, set.WorkspaceID, set.Owner.ConnectorID, set.Owner.Stream, set.Name, set.Description); err != nil {
			return fmt.Errorf("upsert connector document set %s: %w", set.Owner.Stream, err)
		}
		return tx.QueryRowContext(ctx, `SELECT id FROM foundry_document_sets WHERE connector_id=? AND stream=?`,
			set.Owner.ConnectorID, set.Owner.Stream).Scan(&id)
	})
	return id, err
}

// ListConnectorDocuments returns a page of a connector set's live index:
// its uploaded documents whose external id sorts after after ("" for the
// first page), by external id, at most limit, each with its time at the
// source. It reads one range of foundry_documents_live_external_uq.
func (s *mysqlDB) ListConnectorDocuments(ctx context.Context, workspaceID, setID, after string, limit int) ([]model.ConnectorDocument, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, external_id, source_version, sha256, source_at FROM foundry_documents
WHERE set_id=? AND live_external_id > ? AND workspace_id=? ORDER BY live_external_id LIMIT ?`, setID, after, workspaceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ConnectorDocument
	for rows.Next() {
		var d model.ConnectorDocument
		if err := rows.Scan(&d.ID, &d.ExternalID, &d.Version, &d.SHA256, &d.At); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ReserveConnectorDocuments writes a purge row for each of ids, while
// workerID holds syncID's lease, before a sync puts those documents' bytes,
// so every object in S3 has a row that owns it. Publishing a document claims
// its row; the sweep deletes the bytes of one never published.
// ErrNotFound means the lease was lost; ErrConflict that an id has a purge
// row already.
func (s *mysqlDB) ReserveConnectorDocuments(ctx context.Context, workspaceID string, ids []string, syncID, workerID string) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, 2*len(ids))
	for _, id := range ids {
		args = append(args, id, workspaceID)
	}
	return s.withConnectorSyncLease(ctx, syncID, workerID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO foundry_document_purges (document_id, workspace_id) VALUES `+
			strings.TrimSuffix(strings.Repeat("(?, ?), ", len(ids)), ", "), args...)
		if isDuplicateKey(err) {
			return ErrConflict
		}
		return err
	})
}

// PublishConnectorDocuments writes one flush of a connector's sync into its
// set in one transaction, while workerID holds syncID's lease. It locks as
// every writer of documents does: the workspace's live ontologies, then the
// connector, which must not be deleted, then the set, then documents. Then:
//   - each of p.Remove is deleted, and its prefix waits for the sweep (a
//     purge row);
//   - p.Add's reservations are claimed, each younger than p.Grace/2, or
//     nothing is written (ErrReservationExpired);
//   - p.Add is written uploaded, and read by every ontology that reads the
//     set (fanOutFoundryDocuments, with p.Fan): reads holds what that did;
//   - each of p.Update takes its new name, version, time and link;
//   - when a document was removed, every ontology with refresh on that
//     reads the set owes a refresh (oweFoundrySetRefreshes): what the
//     document put in the graph stays until a load without it is current.
//
// A document of Remove or Update no longer uploaded in the set (the graph
// worker deleted it, its original gone) is left as it is: the next sync's
// index lacks it and adds its row's document again. An empty publish
// writes nothing. ErrNotFound means the lease was lost, or the connector or
// its set is deleted; ErrConflict that an added document's row already has
// an uploaded document in the set the publish does not remove.
func (s *mysqlDB) PublishConnectorDocuments(ctx context.Context, p model.ConnectorDocumentsPublish, syncID, workerID string) (
	reads []model.FoundryDocumentRead, err error) {
	if len(p.Add) == 0 && len(p.Update) == 0 && len(p.Remove) == 0 {
		return []model.FoundryDocumentRead{}, nil
	}
	if len(p.Add) > 0 && (p.Grace <= 0 || p.Fan.NewJobID == nil || p.Fan.CanWrite == nil) {
		return nil, fmt.Errorf("publish connector documents: adds need the sweep's grace and a whole fan-out")
	}
	added := make([]string, len(p.Add))
	rows := make([]any, 0, 12*len(p.Add))
	for i, d := range p.Add {
		if d.Source == nil || d.ClientToken != nil || d.CreatedByUserID != nil || d.WorkspaceID != p.WorkspaceID || d.SetID != p.SetID {
			return nil, fmt.Errorf("publish connector documents: %s is not a connector's document of set %s", d.ID, p.SetID)
		}
		added[i] = d.ID
		rows = append(rows, d.ID, d.WorkspaceID, d.SetID, d.Source.ExternalID, d.Source.Version, d.Source.At.UTC(), d.Source.URL,
			d.Name, string(d.MediaType), d.SizeBytes, d.SHA256, d.S3Key)
	}
	err = s.withConnectorSyncLease(ctx, syncID, workerID, func(sqlTx *sql.Tx) error {
		tx := &mysqlDB{db: sqlTx, workspaces: s.workspaces}
		ontologies, err := tx.lockLiveFoundryOntologies(ctx, p.WorkspaceID)
		if err != nil {
			return err
		}
		if err := shareLiveConnector(ctx, sqlTx, p.WorkspaceID, p.ConnectorID); err != nil {
			return err
		}
		var set string
		if err := sqlTx.QueryRowContext(ctx, `SELECT id FROM foundry_document_sets
WHERE id=? AND workspace_id=? AND connector_id=? AND live_key=1 FOR SHARE`, p.SetID, p.WorkspaceID, p.ConnectorID).Scan(&set); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock connector document set %s: %w", p.SetID, err)
		}
		removed, err := tx.removeConnectorDocuments(ctx, p)
		if err != nil {
			return err
		}
		if len(p.Add) > 0 {
			claim, err := sqlTx.ExecContext(ctx, `DELETE FROM foundry_document_purges
WHERE document_id IN (`+idPlaceholders(len(added))+`) AND workspace_id=? AND created_at > NOW(6) - INTERVAL ? MICROSECOND`,
				append(append(toAnySlice(added), p.WorkspaceID), (p.Grace/2).Microseconds())...)
			if err != nil {
				return fmt.Errorf("claim connector document reservations: %w", err)
			}
			n, err := claim.RowsAffected()
			if err != nil {
				return err
			}
			if n != int64(len(added)) {
				return ErrReservationExpired
			}
			if _, err := sqlTx.ExecContext(ctx, `INSERT INTO foundry_documents (id, workspace_id, set_id, external_id, source_version, source_at,
  source_url, name, media_type, size_bytes, sha256, status, s3_key, uploaded_at) VALUES `+
				strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'uploaded', ?, NOW(6)), ", len(p.Add)), ", "), rows...); err != nil {
				if isDuplicateKey(err) {
					return ErrConflict
				}
				return fmt.Errorf("insert connector documents: %w", err)
			}
		}
		for _, u := range p.Update {
			if _, err := sqlTx.ExecContext(ctx, `UPDATE foundry_documents SET name=?, source_version=?, source_at=?, source_url=?
WHERE id=? AND set_id=? AND status='uploaded'`, u.Name, u.Version, u.At.UTC(), u.URL, u.ID, p.SetID); err != nil {
				return fmt.Errorf("update connector document %s: %w", u.ID, err)
			}
		}
		if reads, err = tx.fanOutFoundryDocuments(ctx, ontologies, p.SetID, added, p.Fan); err != nil {
			return err
		}
		if removed == 0 {
			return nil
		}
		return tx.oweFoundrySetRefreshes(ctx, ontologies, []string{p.SetID}, p.RefreshEvery)
	})
	if err != nil {
		return nil, err
	}
	return reads, nil
}

// removeConnectorDocuments deletes the documents of p.Remove still uploaded
// in p.SetID, with a purge row each, and counts them. The caller holds the
// set's lock.
func (s *mysqlDB) removeConnectorDocuments(ctx context.Context, p model.ConnectorDocumentsPublish) (int, error) {
	if len(p.Remove) == 0 {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM foundry_documents
WHERE id IN (`+idPlaceholders(len(p.Remove))+`) AND set_id=? AND status='uploaded' FOR UPDATE`, append(toAnySlice(p.Remove), p.SetID)...)
	if err != nil {
		return 0, fmt.Errorf("lock removed connector documents: %w", err)
	}
	var ids []string
	args := []any{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		args = append(args, id, p.WorkspaceID)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_document_purges (document_id, workspace_id) VALUES `+
		strings.TrimSuffix(strings.Repeat("(?, ?), ", len(ids)), ", "), args...); err != nil {
		return 0, fmt.Errorf("purge removed connector documents: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_documents SET status='deleted', deleted_at=NOW(6)
WHERE id IN (`+idPlaceholders(len(ids))+`)`, toAnySlice(ids)...); err != nil {
		return 0, fmt.Errorf("delete removed connector documents: %w", err)
	}
	return len(ids), nil
}

// oweFoundrySetRefreshes owes a refresh of each of ontologies with refresh
// on that reads one of setIDs (as fanOutFoundryDocuments reads a set: its
// live job's load, unless the job deletes one, or its current load is of a
// version whose scope holds it), spaced by refreshEvery as a table change's
// is (oweFoundryRefresh). The caller holds the ontologies' locks.
func (s *mysqlDB) oweFoundrySetRefreshes(ctx context.Context, ontologies []*model.FoundryOntology, setIDs []string,
	refreshEvery time.Duration) error {
	for _, o := range ontologies {
		if !o.AutoRefresh {
			continue
		}
		live, err := s.liveFoundryGraphJob(ctx, o.ID)
		if err != nil {
			return err
		}
		var loads []string
		if live != nil && live.Kind != model.FoundryGraphJobDelete {
			loads = append(loads, live.LoadID)
		}
		if o.CurrentLoadID != nil {
			loads = append(loads, *o.CurrentLoadID)
		}
		reads := false
		for _, l := range loads {
			_, scope, err := s.foundryLoadScope(ctx, l)
			if err != nil {
				return err
			}
			if reads = slices.ContainsFunc(scope, func(id string) bool { return slices.Contains(setIDs, id) }); reads {
				break
			}
		}
		if !reads {
			continue
		}
		last, err := s.lastFoundryRefresh(ctx, o.ID)
		if err != nil {
			return err
		}
		if err := s.oweFoundryRefresh(ctx, o.ID, last, refreshEvery); err != nil {
			return err
		}
	}
	return nil
}

// shareLiveConnector locks a live connector of the workspace FOR SHARE, so
// it is not deleted before the transaction ends; ErrNotFound means it is
// deleted.
func shareLiveConnector(ctx context.Context, tx *sql.Tx, workspaceID, connectorID string) error {
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM connectors WHERE id=? AND workspace_id=? AND deleted_at IS NULL FOR SHARE`,
		connectorID, workspaceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock connector %s: %w", connectorID, err)
	}
	return nil
}

// ListFoundryDocumentPurges returns up to limit purge rows written before
// before, oldest first, leaving out those of documents a live job's load
// holds: the job may still read the original. The sweep deletes each one's
// prefix, then the row (DeleteFoundryDocumentPurge). A publish claims only
// reservations younger than half the grace, so with before at least the
// grace ago, a row listed here is never published.
func (s *mysqlDB) ListFoundryDocumentPurges(ctx context.Context, before time.Time, limit int) ([]*model.FoundryDocumentPurge, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.document_id, p.workspace_id, p.created_at FROM foundry_document_purges p
WHERE p.created_at < ? AND NOT EXISTS (SELECT 1 FROM foundry_load_jobs j
  JOIN foundry_load_documents m ON m.load_id=j.load_id AND m.document_id=p.document_id
  WHERE j.status IN ('queued', 'running'))
ORDER BY p.created_at, p.document_id LIMIT ?`, before.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.FoundryDocumentPurge
	for rows.Next() {
		var p model.FoundryDocumentPurge
		if err := rows.Scan(&p.DocumentID, &p.WorkspaceID, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// DeleteFoundryDocumentPurge forgets a purge row once the sweep deleted the
// document's prefix.
func (s *mysqlDB) DeleteFoundryDocumentPurge(ctx context.Context, documentID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM foundry_document_purges WHERE document_id=?`, documentID)
	return err
}
