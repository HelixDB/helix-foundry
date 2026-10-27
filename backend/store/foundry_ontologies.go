package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"slices"
	"time"
)

const foundryOntologyColumns = `id, workspace_id, name, project_id, tenant_id, current_version_id, current_load_id, extract_owed,
       client_token, created_by_user_id, created_at, deleted_at, auto_refresh, refresh_due_at`

// CreateFoundryOntology writes a live ontology. It has no document sets of
// its own: sets are the workspace's, and its versions read those they pick.
// ErrConflict means the workspace already used the client token, or another
// ontology holds the tenant (a live one, or a deleted one whose loads are
// still being deleted); GetFoundryOntologyByClientToken and
// GetFoundryOntologyByTenant tell which.
func (s *mysqlDB) CreateFoundryOntology(ctx context.Context, o *model.FoundryOntology) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO foundry_ontologies (id, workspace_id, name, project_id, tenant_id, client_token, created_by_user_id, live_key)
VALUES (?, ?, ?, ?, ?, ?, ?, 1)`, o.ID, o.WorkspaceID, o.Name, o.ProjectID, o.TenantID, o.ClientToken, o.CreatedByUserID)
	if isDuplicateKey(err) {
		return ErrConflict
	}
	return err
}

// GetFoundryOntology returns a workspace's ontology, deleted or not.
func (s *mysqlDB) GetFoundryOntology(ctx context.Context, workspaceID, id string) (*model.FoundryOntology, error) {
	return scanFoundryOntology(s.db.QueryRowContext(ctx, "SELECT "+foundryOntologyColumns+`
FROM foundry_ontologies WHERE id=? AND workspace_id=?`, id, workspaceID))
}

func (s *mysqlDB) GetFoundryOntologyByClientToken(ctx context.Context, workspaceID, clientToken string) (*model.FoundryOntology, error) {
	return scanFoundryOntology(s.db.QueryRowContext(ctx, "SELECT "+foundryOntologyColumns+`
FROM foundry_ontologies WHERE workspace_id=? AND client_token=?`, workspaceID, clientToken))
}

// GetFoundryOntologyByTenant returns the ontology holding a tenant: its live
// ontology, or a deleted one whose loads are still being deleted from it.
func (s *mysqlDB) GetFoundryOntologyByTenant(ctx context.Context, tenantID string) (*model.FoundryOntology, error) {
	return scanFoundryOntology(s.db.QueryRowContext(ctx, "SELECT "+foundryOntologyColumns+`
FROM foundry_ontologies WHERE tenant_id=? AND live_key=1`, tenantID))
}

// ListFoundryOntologies returns a workspace's live ontologies, newest first, a
// page at a time.
func (s *mysqlDB) ListFoundryOntologies(ctx context.Context, workspaceID, pageToken string, limit int) ([]*model.FoundryOntology, string, error) {
	conds := []string{"workspace_id=?", "deleted_at IS NULL"}
	args := []any{workspaceID}
	if pageToken != "" {
		cursor, err := decodeTimeIDCursor(pageToken)
		if err != nil {
			return nil, "", err
		}
		conds = append(conds, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryOntologyColumns+" FROM foundry_ontologies"+whereClause(conds)+
		fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT %d", limit+1), args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*model.FoundryOntology
	for rows.Next() {
		o, err := scanFoundryOntology(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(out) <= limit {
		return out, "", nil
	}
	next, err := encodeTimeIDCursor(out[limit-1].CreatedAt, out[limit-1].ID)
	return out[:limit], next, err
}

// DeleteFoundryOntology soft-deletes a live ontology and deletes its loads
// from its database, in one transaction under its lock:
//   - its live job is cancelled: a worker running it loses its lease and stops;
//   - it has no current load any more, and owes no extract;
//   - every load not yet deleted is marked deleting, and a delete job
//     (deleteJobID) is queued for the oldest. Each delete's end queues the
//     next;
//   - the document sets and documents that still name it as their owner
//     (from before they belonged to the workspace) stay the workspace's,
//     and name it no more.
//
// The ontology holds its database (another can't be bound to it) until the
// last of its loads is deleted, or at once when it has none; a load whose
// delete fails for good holds it until ResumeFoundryOntologyDrain deletes
// it. The rest of the database is left as it is. ErrNotFound means no live
// ontology has this id.
func (s *mysqlDB) DeleteFoundryOntology(ctx context.Context, workspaceID, id, deleteJobID string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		o, err := tx.lockFoundryOntology(ctx, workspaceID, id)
		if err != nil {
			return err
		}
		if o.DeletedAt != nil {
			return ErrNotFound
		}
		if err := tx.cancelLiveFoundryGraphJob(ctx, id, "the ontology was deleted"); err != nil {
			return err
		}
		if err := tx.cancelFoundryLoadEnrichments(ctx, id, true); err != nil {
			return err
		}
		// Its generation can't store a draft any more. Ending it means no
		// worker claims it, and pays for its turn, again; one running it
		// loses its lease at its next renewal.
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_ontology_generations
SET status='failed', error='the ontology was deleted', active_key=NULL, lease_owner=NULL, lease_until=NULL, finished_at=NOW(6)
WHERE ontology_id=? AND active_key=1`, id); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_loads SET status='deleting', finished_at=COALESCE(finished_at, NOW(6))
WHERE ontology_id=? AND status NOT IN ('deleting', 'deleted')`, id); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_ontologies SET deleted_at=NOW(6), current_load_id=NULL, extract_owed=FALSE
WHERE id=?`, id); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_document_sets SET ontology_id=NULL WHERE ontology_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_documents SET ontology_id=NULL WHERE ontology_id=?`, id); err != nil {
			return err
		}
		deleted := time.Now().UTC()
		o.DeletedAt, o.CurrentLoadID, o.ExtractOwed = &deleted, nil, false
		return tx.queueNextFoundryDelete(ctx, o, deleteJobID)
	})
}

// ResumeFoundryOntologyDrain retries, under its lock, the deletes that
// failed for good of a deleted ontology still holding its database: its
// delete_failed loads become deleting and, with no job live, a delete job
// (deleteJobID) is queued for the oldest, each delete's end queueing the
// next as before. A drain still running is left as it is, and one with
// nothing left lets go of the database. Nothing calls it by itself, so a
// load that can't be deleted never loops: binding the database to a new
// ontology does, which is how the user retries. ErrNotFound means the
// workspace has no deleted ontology with this id.
func (s *mysqlDB) ResumeFoundryOntologyDrain(ctx context.Context, workspaceID, id, deleteJobID string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		o, err := tx.lockFoundryOntology(ctx, workspaceID, id)
		if err != nil {
			return err
		}
		if o.DeletedAt == nil {
			return ErrNotFound
		}
		live, err := tx.liveFoundryGraphJob(ctx, id)
		if err != nil || live != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_loads SET status='deleting' WHERE ontology_id=? AND status='delete_failed'`,
			id); err != nil {
			return err
		}
		return tx.queueNextFoundryDelete(ctx, o, deleteJobID)
	})
}

// lockFoundryOntology locks an ontology's row, deleted or not, until the
// transaction ends, and returns it. Every write that queues a job or changes
// a load takes it first (completing an upload, creating, setting current and
// deleting a load, finishing a job, deleting the ontology), so they see each
// other's writes whole and one live job per ontology holds. ErrNotFound means
// the workspace has no such ontology.
func (s *mysqlDB) lockFoundryOntology(ctx context.Context, workspaceID, id string) (*model.FoundryOntology, error) {
	return scanFoundryOntology(s.db.QueryRowContext(ctx, "SELECT "+foundryOntologyColumns+`
FROM foundry_ontologies WHERE id=? AND workspace_id=? FOR UPDATE`, id, workspaceID))
}

// lockLiveFoundryOntology is lockFoundryOntology for a live ontology:
// ErrNotFound also means it is deleted.
func (s *mysqlDB) lockLiveFoundryOntology(ctx context.Context, workspaceID, id string) (*model.FoundryOntology, error) {
	o, err := s.lockFoundryOntology(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	if o.DeletedAt != nil {
		return nil, ErrNotFound
	}
	return o, nil
}

func scanFoundryOntology(scanner rowScanner) (*model.FoundryOntology, error) {
	var o model.FoundryOntology
	var current, currentLoad, createdBy sql.NullString
	var deletedAt, refreshDue sql.NullTime
	if err := scanner.Scan(&o.ID, &o.WorkspaceID, &o.Name, &o.ProjectID, &o.TenantID, &current, &currentLoad, &o.ExtractOwed,
		&o.ClientToken, &createdBy, &o.CreatedAt, &deletedAt, &o.AutoRefresh, &refreshDue); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	for _, f := range []struct {
		src sql.NullString
		dst **string
	}{{current, &o.CurrentVersionID}, {currentLoad, &o.CurrentLoadID}, {createdBy, &o.CreatedByUserID}} {
		if f.src.Valid {
			*f.dst = &f.src.String
		}
	}
	if deletedAt.Valid {
		t := deletedAt.Time
		o.DeletedAt = &t
	}
	if refreshDue.Valid {
		t := refreshDue.Time
		o.RefreshDueAt = &t
	}
	return &o, nil
}

// SetFoundryOntologyAutoRefresh turns refresh on or off; off forgets an owed refresh.
func (s *mysqlDB) SetFoundryOntologyAutoRefresh(ctx context.Context, workspaceID, id string, on bool) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_ontologies
SET auto_refresh=?, refresh_due_at=IF(?, refresh_due_at, NULL)
WHERE id=? AND workspace_id=? AND deleted_at IS NULL`, on, on, id, workspaceID)
}

// OweFoundryRefreshes marks every live ontology of the workspace with refresh
// on whose current load reads one of the connector's tables: its refresh is
// due at the later of now and its last refresh's start plus refreshEvery, or
// earlier if one was already due.
func (s *mysqlDB) OweFoundryRefreshes(ctx context.Context, workspaceID, connectorID string, tables []string, refreshEvery time.Duration) error {
	rows, err := s.db.QueryContext(ctx, `SELECT o.id, g.input,
  (SELECT MAX(r.created_at) FROM foundry_loads r WHERE r.ontology_id = o.id AND r.origin = 'refresh')
FROM foundry_ontologies o
JOIN foundry_loads l ON l.id = o.current_load_id
JOIN foundry_ontology_versions v ON v.id = l.version_id
JOIN foundry_ontology_generations g ON g.id = v.generation_id
WHERE o.workspace_id=? AND o.deleted_at IS NULL AND o.auto_refresh`, workspaceID)
	if err != nil {
		return fmt.Errorf("list ontologies to refresh: %w", err)
	}
	type owed struct {
		id   string
		last sql.NullTime
	}
	var marks []owed
	for rows.Next() {
		var id, input string
		var lastRefresh sql.NullTime
		if err := rows.Scan(&id, &input, &lastRefresh); err != nil {
			rows.Close()
			return fmt.Errorf("scan ontology to refresh: %w", err)
		}
		var in model.OntologyGenerationInput
		if err := json.Unmarshal([]byte(input), &in); err != nil {
			rows.Close()
			return fmt.Errorf("decode generation input of ontology %s: %w", id, err)
		}
		if !slices.ContainsFunc(in.Tables, func(t model.OntologyGenerationTable) bool {
			return t.ConnectorID == connectorID && slices.Contains(tables, t.Table)
		}) {
			continue
		}
		marks = append(marks, owed{id, lastRefresh})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list ontologies to refresh: %w", err)
	}
	for _, m := range marks {
		if err := s.oweFoundryRefresh(ctx, m.id, m.last, refreshEvery); err != nil {
			return err
		}
	}
	return nil
}

// oweFoundryRefresh makes a live ontology with refresh on owe a refresh, due
// at the later of now and its last refresh's start (last, if it had one)
// plus refreshEvery, or earlier if one was already due.
func (s *mysqlDB) oweFoundryRefresh(ctx context.Context, ontologyID string, last sql.NullTime, refreshEvery time.Duration) error {
	due := time.Now().UTC()
	if last.Valid && last.Time.Add(refreshEvery).After(due) {
		due = last.Time.Add(refreshEvery).UTC()
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_ontologies
SET refresh_due_at = IF(refresh_due_at IS NULL OR refresh_due_at > ?, ?, refresh_due_at)
WHERE id=? AND auto_refresh AND deleted_at IS NULL`, due, due, ontologyID); err != nil {
		return fmt.Errorf("owe refresh of ontology %s: %w", ontologyID, err)
	}
	return nil
}

// ListDueFoundryRefreshes returns up to limit live ontologies whose refresh is
// due, longest owed first, after the one due at afterDue with afterID (a zero
// afterDue starts at the first). Those that can't be queued yet stay due, so
// a tick pages past them.
func (s *mysqlDB) ListDueFoundryRefreshes(ctx context.Context, afterDue time.Time, afterID string, limit int) ([]*model.FoundryOntology, error) {
	after, args := "", []any{}
	if !afterDue.IsZero() {
		after = " AND (refresh_due_at > ? OR refresh_due_at = ? AND id > ?)"
		args = append(args, afterDue.UTC(), afterDue.UTC(), afterID)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryOntologyColumns+` FROM foundry_ontologies
WHERE refresh_due_at <= NOW(6) AND deleted_at IS NULL`+after+` ORDER BY refresh_due_at, id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.FoundryOntology
	for rows.Next() {
		o, err := scanFoundryOntology(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
