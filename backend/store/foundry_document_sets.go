package store

import (
	"context"
	"database/sql"
	"errors"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// ErrNotEmpty means the row still holds what must go first: a document set
// with documents that are not deleted.
var ErrNotEmpty = errors.New("store: not empty")

// ErrConnectorOwned means a person asked to change a connector's document
// set, or one of its documents: only the connector's syncs write them, and
// the next sync would undo the change.
var ErrConnectorOwned = errors.New("store: owned by a connector")

// Until the workspace-foundry-documents step has run, a set or document of
// an ontology deleted before sets belonged to the workspace still names that
// ontology. Such a row is gone to every read, count and fan-out: these keep
// the set (alias s) or document (alias d) of a row that no deleted ontology
// owns. Only plain reads and the outer table of a locking read may use them:
// InnoDB share-locks the ontology rows a subquery of an UPDATE or an
// INSERT ... SELECT reads, which would invert the lock order of every writer
// that holds an ontology's lock. Scopes name only sets such a row is not in,
// so a load's snapshot needs no such filter.
const (
	foundrySetOwnerLive      = ` NOT EXISTS (SELECT 1 FROM foundry_ontologies gone WHERE gone.id = s.ontology_id AND gone.deleted_at IS NOT NULL)`
	foundryDocumentOwnerLive = ` NOT EXISTS (SELECT 1 FROM foundry_ontologies gone WHERE gone.id = d.ontology_id AND gone.deleted_at IS NOT NULL)`
)

// foundryDocumentSetColumns reads a live set with its owner, if a connector
// owns it, and its documents that are not deleted counted, from
// foundryDocumentSets. A connector's set holds only its connector's
// documents, which are never pending (foundry_documents_source_chk), so its
// count is of the uploaded ones: one range of
// foundry_documents_live_external_uq, which never reads the deleted rows a
// stream's turnover leaves behind. A person's set counts its pending
// uploads too.
const foundryDocumentSetColumns = `s.id, s.workspace_id, s.name, s.description, s.connector_id, s.stream, c.kind, c.name, s.created_by_user_id,
       s.created_at,
       IF(s.connector_id IS NULL,
          (SELECT COUNT(*) FROM foundry_documents d WHERE d.workspace_id=s.workspace_id AND d.set_id=s.id AND d.status<>'deleted'),
          (SELECT COUNT(*) FROM foundry_documents cd WHERE cd.set_id=s.id AND cd.live_external_id IS NOT NULL))`

// foundryDocumentSets is the sets table with the connector that owns each
// set, if one does, for its name.
const foundryDocumentSets = ` FROM foundry_document_sets s LEFT JOIN connectors c ON c.id=s.connector_id`

// CreateFoundryDocumentSet writes a live set of the workspace, unless it has
// max live sets already: ErrLimitExceeded. The count, the name check and the
// insert run under the workspace's lock, so sets created at once can't pass
// the limit together or share a name. The limit binds creates only: a
// workspace whose ontologies' sets were merged into it may be over it.
// Connectors' sets take no place and no name: their syncs make one per
// stream however many people have made. ErrConflict means a live set of the
// workspace no connector owns has the name (compared in the column's
// collation, which ignores case and accents); ErrNotFound means the
// workspace is gone.
func (s *mysqlDB) CreateFoundryDocumentSet(ctx context.Context, set *model.FoundryDocumentSet, max int) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.workspaces.Lock(ctx, tx.db, set.WorkspaceID); err != nil {
			return err
		}
		var n int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_document_sets s
WHERE s.workspace_id=? AND s.live_key=1 AND s.connector_id IS NULL AND`+foundrySetOwnerLive, set.WorkspaceID).Scan(&n); err != nil {
			return err
		}
		if n >= max {
			return ErrLimitExceeded
		}
		taken, err := tx.foundryDocumentSetNameTaken(ctx, set.WorkspaceID, set.Name, set.ID)
		if err != nil {
			return err
		}
		if taken {
			return ErrConflict
		}
		_, err = tx.db.ExecContext(ctx, `
INSERT INTO foundry_document_sets (id, workspace_id, name, description, created_by_user_id, live_key)
VALUES (?, ?, ?, ?, ?, 1)`, set.ID, set.WorkspaceID, set.Name, set.Description, set.CreatedByUserID)
		if isDuplicateKey(err) {
			return ErrConflict
		}
		return err
	})
}

// foundryDocumentSetNameTaken reports whether a live set of the workspace
// that no connector owns, other than exceptID, has the name, in the column's
// collation. The caller holds the workspace's lock.
func (s *mysqlDB) foundryDocumentSetNameTaken(ctx context.Context, workspaceID, name, exceptID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_document_sets s
WHERE s.workspace_id=? AND s.name=? AND s.id<>? AND s.live_key=1 AND s.connector_id IS NULL AND`+foundrySetOwnerLive,
		workspaceID, name, exceptID).Scan(&n)
	return n > 0, err
}

// GetFoundryDocumentSet returns a live set of the workspace; ErrNotFound
// means it has none with this id, or it is deleted.
func (s *mysqlDB) GetFoundryDocumentSet(ctx context.Context, workspaceID, id string) (*model.FoundryDocumentSet, error) {
	return scanFoundryDocumentSet(s.db.QueryRowContext(ctx, "SELECT "+foundryDocumentSetColumns+foundryDocumentSets+`
WHERE s.id=? AND s.workspace_id=? AND s.live_key=1 AND`+foundrySetOwnerLive, id, workspaceID))
}

// ListFoundryDocumentSets returns the workspace's live sets, oldest first. A
// workspace has a bounded number of them (CreateFoundryDocumentSet's max,
// give or take the sets of merged ontologies).
func (s *mysqlDB) ListFoundryDocumentSets(ctx context.Context, workspaceID string) ([]*model.FoundryDocumentSet, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryDocumentSetColumns+foundryDocumentSets+`
WHERE s.workspace_id=? AND s.live_key=1 AND`+foundrySetOwnerLive+` ORDER BY s.created_at, s.id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.FoundryDocumentSet
	for rows.Next() {
		set, err := scanFoundryDocumentSet(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, set)
	}
	return out, rows.Err()
}

// UpdateFoundryDocumentSet renames a live set or changes its description. A
// rename checks the name under the workspace's lock, as a create does; the
// set's own name, byte for byte, is no rename.
// ErrNotFound means the workspace has no live set with this id; ErrConflict
// means another of its live sets has the new name; ErrConnectorOwned means
// a connector owns the set.
func (s *mysqlDB) UpdateFoundryDocumentSet(ctx context.Context, workspaceID, id string, update model.FoundryDocumentSetUpdate) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if update.Name != nil {
			if err := tx.workspaces.Lock(ctx, tx.db, workspaceID); err != nil {
				return err
			}
		}
		if err := tx.lockFoundryDocumentSet(ctx, workspaceID, id); err != nil {
			return err
		}
		if update.Name != nil {
			// The website sends the name with every edit: the same name is no
			// rename, even where another set has it from before the data step.
			var current string
			if err := tx.db.QueryRowContext(ctx, `SELECT name FROM foundry_document_sets WHERE id=?`, id).Scan(&current); err != nil {
				return err
			}
			taken := false
			if *update.Name != current {
				var err error
				if taken, err = tx.foundryDocumentSetNameTaken(ctx, workspaceID, *update.Name, id); err != nil {
					return err
				}
			}
			if taken {
				return ErrConflict
			}
		}
		_, err := tx.db.ExecContext(ctx, `UPDATE foundry_document_sets SET name=COALESCE(?, name), description=COALESCE(?, description)
WHERE id=?`, update.Name, update.Description, id)
		if isDuplicateKey(err) {
			return ErrConflict
		}
		return err
	})
}

// DeleteFoundryDocumentSet deletes a live set that holds no document that is
// not deleted: ErrNotEmpty otherwise. It holds the set's lock while it
// counts, and creating or moving a document into a set takes the same lock
// first, so no document lands in a deleted set. A version whose scope names
// the set reads nothing more through it. ErrNotFound means the workspace has
// no live set with this id; ErrConnectorOwned means a connector owns it, and
// it lives as long as the connector.
func (s *mysqlDB) DeleteFoundryDocumentSet(ctx context.Context, workspaceID, id string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.lockFoundryDocumentSet(ctx, workspaceID, id); err != nil {
			return err
		}
		// A locking read sees what committed while this waited for the set.
		var n int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_documents
WHERE workspace_id=? AND set_id=? AND status<>'deleted' FOR UPDATE`, workspaceID, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return ErrNotEmpty
		}
		_, err := tx.db.ExecContext(ctx, `UPDATE foundry_document_sets SET deleted_at=NOW(6), live_key=NULL WHERE id=?`, id)
		return err
	})
}

// lockFoundryDocumentSet locks a live set of the workspace, for a person to
// put a document in it, change it or delete it; ErrNotFound means the
// workspace has no live set with this id, and ErrConnectorOwned that a
// connector owns it.
func (s *mysqlDB) lockFoundryDocumentSet(ctx context.Context, workspaceID, id string) error {
	var owned bool
	err := s.db.QueryRowContext(ctx, `SELECT s.connector_id IS NOT NULL FROM foundry_document_sets s
WHERE s.id=? AND s.workspace_id=? AND s.live_key=1 AND`+foundrySetOwnerLive+` FOR UPDATE`, id, workspaceID).Scan(&owned)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return err
	case owned:
		return ErrConnectorOwned
	}
	return nil
}

func scanFoundryDocumentSet(scanner rowScanner) (*model.FoundryDocumentSet, error) {
	var set model.FoundryDocumentSet
	var connectorID, stream, connectorKind, connectorName, createdBy sql.NullString
	if err := scanner.Scan(&set.ID, &set.WorkspaceID, &set.Name, &set.Description, &connectorID, &stream, &connectorKind, &connectorName,
		&createdBy, &set.CreatedAt, &set.Documents); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	// foundry_document_sets_owner_chk: a connector's set has its stream.
	if connectorID.Valid {
		set.Owner = &model.FoundryDocumentSetOwner{
			ConnectorID: connectorID.String, ConnectorKind: connectorKind.String, ConnectorName: connectorName.String, Stream: stream.String,
		}
	}
	if createdBy.Valid {
		set.CreatedByUserID = &createdBy.String
	}
	return &set, nil
}
