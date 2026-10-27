package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"slices"
)

const ontologyVersionColumns = `id, workspace_id, ontology_id, version, format, definition, source, generation_id, document_set_ids,
       created_by_user_id, created_at`

// CreateOntologyVersion stores v as its ontology's next version and sets v.Version.
func (s *mysqlDB) CreateOntologyVersion(ctx context.Context, v *model.OntologyVersion) (err error) {
	if s.root == nil {
		return fmt.Errorf("ontology version insert requires a root database connection")
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ontology version insert: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = insertOntologyVersion(ctx, tx, v); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit ontology version: %w", err)
	}
	return nil
}

// insertOntologyVersion numbers v after its ontology's highest version and
// inserts it with its scope. The row lock on that version serializes two
// writers. Every new version has a scope, which keeps only live sets of the
// workspace, in the order given and once each: a set deleted since it was
// picked holds nothing, and an id a revision's routes made up names none.
// v.DocumentSetIDs is set to what is stored.
func insertOntologyVersion(ctx context.Context, tx *sql.Tx, v *model.OntologyVersion) error {
	if v.Format != model.OntologyFormatFoundryV1 && v.Format != model.OntologyFormatAgentBundle {
		return fmt.Errorf("ontology version %s has format %q", v.ID, v.Format)
	}
	if v.DocumentSetIDs == nil {
		return fmt.Errorf("ontology version %s has no document scope", v.ID)
	}
	var live string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM foundry_ontologies WHERE id=? AND workspace_id=? AND deleted_at IS NULL FOR UPDATE`, v.OntologyID, v.WorkspaceID).Scan(&live); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	scope := []string{}
	if picked := *v.DocumentSetIDs; len(picked) > 0 {
		rows, err := tx.QueryContext(ctx, `SELECT s.id FROM foundry_document_sets s
WHERE s.workspace_id=? AND s.live_key=1 AND s.id IN (`+idPlaceholders(len(picked))+`) AND`+foundrySetOwnerLive,
			append([]any{v.WorkspaceID}, toAnySlice(picked)...)...)
		if err != nil {
			return fmt.Errorf("read ontology version scope: %w", err)
		}
		live := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			live[id] = true
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range picked {
			if live[id] && !slices.Contains(scope, id) {
				scope = append(scope, id)
			}
		}
	}
	stored, err := json.Marshal(scope)
	if err != nil {
		return err
	}
	var next int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM foundry_ontology_versions
WHERE ontology_id=? FOR UPDATE`, v.OntologyID).Scan(&next); err != nil {
		return fmt.Errorf("next ontology version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO foundry_ontology_versions (id, workspace_id, ontology_id, version, format, definition, source, generation_id,
  document_set_ids, created_by_user_id)
VALUES (?, ?, ?, ?, ?, CAST(? AS JSON), ?, ?, CAST(? AS JSON), ?)`,
		v.ID, v.WorkspaceID, v.OntologyID, next, string(v.Format), string(v.Definition), string(v.Source), v.GenerationID,
		string(stored), v.CreatedByUserID); err != nil {
		if isDuplicateKey(err) {
			return ErrConflict
		}
		return fmt.Errorf("insert ontology version: %w", err)
	}
	v.Version, v.DocumentSetIDs = next, &scope
	return nil
}

// GetOntologyVersionScope returns the document sets a version of the
// ontology reads: its stored scope, or for a legacy version (NULL) the
// ontology's live sets, oldest first, which is what the
// workspace-foundry-documents step stores for it. ErrNotFound means the
// ontology has no such version.
func (s *mysqlDB) GetOntologyVersionScope(ctx context.Context, ontologyID, versionID string) ([]string, error) {
	var raw sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT document_set_ids FROM foundry_ontology_versions WHERE id=? AND ontology_id=?`,
		versionID, ontologyID).Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	stored, err := decodeDocumentSetIDs(raw)
	if err != nil {
		return nil, err
	}
	if stored != nil {
		return *stored, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM foundry_document_sets WHERE ontology_id=? AND live_key=1 ORDER BY created_at, id`,
		ontologyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	scope := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		scope = append(scope, id)
	}
	return scope, rows.Err()
}

// GetOntologyVersionMappings returns an agent draft's mappings.toml without
// reading the rest of its definition, which runs to megabytes; "" for a
// Definition v1. ErrNotFound means the ontology has no such version.
func (s *mysqlDB) GetOntologyVersionMappings(ctx context.Context, ontologyID, versionID string) (string, error) {
	var format string
	var raw sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT format, JSON_EXTRACT(definition, '$.mappings_toml') FROM foundry_ontology_versions
WHERE id=? AND ontology_id=?`, versionID, ontologyID).Scan(&format, &raw); err != nil {
		if err == sql.ErrNoRows {
			return "", ErrNotFound
		}
		return "", err
	}
	var mappings string
	if model.OntologyFormat(format) != model.OntologyFormatAgentBundle || !raw.Valid {
		return "", nil
	}
	if err := json.Unmarshal([]byte(raw.String), &mappings); err != nil {
		return "", fmt.Errorf("ontology version %s mappings: %w", versionID, err)
	}
	return mappings, nil
}

// decodeDocumentSetIDs reads a version's stored scope: nil for NULL (a
// legacy version), else its ids.
func decodeDocumentSetIDs(raw sql.NullString) (*[]string, error) {
	if !raw.Valid {
		return nil, nil
	}
	ids := []string{}
	if err := json.Unmarshal([]byte(raw.String), &ids); err != nil {
		return nil, fmt.Errorf("decode document_set_ids: %w", err)
	}
	return &ids, nil
}

// GetOntologyVersion returns one of an ontology's versions.
func (s *mysqlDB) GetOntologyVersion(ctx context.Context, ontologyID, id string) (*model.OntologyVersion, error) {
	return scanOntologyVersion(s.db.QueryRowContext(ctx, "SELECT "+ontologyVersionColumns+`
FROM foundry_ontology_versions WHERE id=? AND ontology_id=?`, id, ontologyID))
}

// GetCurrentOntologyVersion returns the version the ontology points at;
// ErrNotFound when none.
func (s *mysqlDB) GetCurrentOntologyVersion(ctx context.Context, ontologyID string) (*model.OntologyVersion, error) {
	return scanOntologyVersion(s.db.QueryRowContext(ctx, `
SELECT v.id, v.workspace_id, v.ontology_id, v.version, v.format, v.definition, v.source, v.generation_id, v.document_set_ids,
       v.created_by_user_id, v.created_at
FROM foundry_ontologies o JOIN foundry_ontology_versions v ON v.id = o.current_version_id AND v.ontology_id = o.id
WHERE o.id=?`, ontologyID))
}

// ListOntologyVersions returns an ontology's versions, newest first, without
// their definitions: an agent draft can run to megabytes.
func (s *mysqlDB) ListOntologyVersions(ctx context.Context, ontologyID string, limit int) ([]*model.OntologyVersion, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, workspace_id, ontology_id, version, format, NULL, source, generation_id, document_set_ids, created_by_user_id, created_at
FROM foundry_ontology_versions WHERE ontology_id=? ORDER BY version DESC LIMIT ?`, ontologyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.OntologyVersion
	for rows.Next() {
		v, err := scanOntologyVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SetCurrentOntologyVersion points a live ontology at versionID if it still
// points at expected (nil: no version), and in the same transaction drops the
// review decisions that version doesn't take (pruneFoundryReviewDecisions).
// ErrConflict means another change won or the ontology is deleted.
func (s *mysqlDB) SetCurrentOntologyVersion(ctx context.Context, ontologyID, versionID string, expected *string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		err := execOrNotFound(ctx, tx.db, `UPDATE foundry_ontologies SET current_version_id=?
WHERE id=? AND deleted_at IS NULL AND current_version_id <=> ?`, versionID, ontologyID, expected)
		if err == ErrNotFound {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if err := tx.pruneFoundryReviewDecisions(ctx, ontologyID, versionID); err != nil {
			return fmt.Errorf("prune review decisions: %w", err)
		}
		return nil
	})
}

func scanOntologyVersion(scanner rowScanner) (*model.OntologyVersion, error) {
	var v model.OntologyVersion
	var format, source string
	var definition, generationID, scope, createdBy sql.NullString
	if err := scanner.Scan(&v.ID, &v.WorkspaceID, &v.OntologyID, &v.Version, &format, &definition, &source, &generationID, &scope,
		&createdBy, &v.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var err error
	if v.DocumentSetIDs, err = decodeDocumentSetIDs(scope); err != nil {
		return nil, fmt.Errorf("ontology version %s: %w", v.ID, err)
	}
	v.Format, v.Source = model.OntologyFormat(format), model.OntologySource(source)
	if definition.Valid {
		v.Definition = []byte(definition.String)
	}
	if generationID.Valid {
		v.GenerationID = &generationID.String
	}
	if createdBy.Valid {
		v.CreatedByUserID = &createdBy.String
	}
	return &v, nil
}
