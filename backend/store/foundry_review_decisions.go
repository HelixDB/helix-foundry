package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"strings"
)

const foundryReviewDecisionColumns = "id, ontology_id, kind, type, doc_id, norm_name, entity_key, prop, value, decided_by_user_id, decided_at"

// SettleFoundryReviewDecision replaces the choice for this identity atomically.
// It preserves the id of an existing choice. Under the ontology's lock it
// checks d against the version of the ontology's current load, the load whose
// review it settles: ErrConflict means that version doesn't take d (its type,
// prop or key changed, as a new load of another version may have done since
// the review was read), or the ontology has no current load.
func (s *mysqlDB) SettleFoundryReviewDecision(ctx context.Context, workspaceID string, d *model.FoundryReviewDecision) error {
	if err := d.Validate(); err != nil {
		return err
	}
	return s.WithTx(ctx, func(db Store) error {
		tx := db.(*mysqlDB)
		o, err := tx.lockLiveFoundryOntology(ctx, workspaceID, d.OntologyID)
		if err != nil {
			return err
		}
		if o.CurrentLoadID == nil {
			return ErrConflict
		}
		var definition []byte
		if err := tx.db.QueryRowContext(ctx, `SELECT v.definition FROM foundry_loads l
JOIN foundry_ontology_versions v ON v.id = l.version_id WHERE l.id=?`, *o.CurrentLoadID).Scan(&definition); err != nil {
			return err
		}
		var def struct {
			OntologyTOML string `json:"ontology_toml"`
		}
		if err := json.Unmarshal(definition, &def); err != nil {
			return err
		}
		types, err := model.FoundryDecisionTypes(def.OntologyTOML)
		if err != nil {
			return err
		}
		if !d.Fits(types) {
			return ErrConflict
		}
		var value any
		if len(d.Value) > 0 {
			value = string(d.Value)
		}
		_, err = tx.db.ExecContext(ctx, `INSERT INTO foundry_review_decisions
 (id, ontology_id, kind, type, doc_id, norm_name, entity_key, prop, value, decided_by_user_id)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, CAST(? AS JSON), ?)
 ON DUPLICATE KEY UPDATE entity_key=VALUES(entity_key), value=VALUES(value),
 decided_by_user_id=VALUES(decided_by_user_id), decided_at=NOW(6)`,
			d.ID, d.OntologyID, d.Kind, d.Type, d.DocID, d.NormName, d.EntityKey, d.Prop, value, d.DecidedByUserID)
		if err != nil {
			return err
		}
		// <=> is NULL-safe equality; resolve's chosen entity is not its identity.
		stored, err := scanFoundryReviewDecision(tx.db.QueryRowContext(ctx, "SELECT "+foundryReviewDecisionColumns+`
 FROM foundry_review_decisions WHERE ontology_id=? AND kind=? AND type=? AND doc_id <=> ? AND norm_name <=> ?
 AND prop <=> ? AND (kind='resolve' OR entity_key <=> ?)`, d.OntologyID, d.Kind, d.Type, d.DocID, d.NormName, d.Prop, d.EntityKey))
		if err == nil {
			*d = *stored
		}
		return err
	})
}

// ListFoundryReviewDecisions uses id keyset paging. A nil documentIDs lists all
// decisions; a non-nil list includes all prop choices and only those documents'
// resolve choices. Callers request limit+1 to detect an extraction over the cap.
func (s *mysqlDB) ListFoundryReviewDecisions(ctx context.Context, ontologyID, afterID string, documentIDs []string, limit int) ([]*model.FoundryReviewDecision, error) {
	return s.listFoundryDecisions(ctx, "foundry_review_decisions", "ontology_id", ontologyID, afterID, documentIDs, limit)
}

// ListFoundryLoadDecisions reads choices captured atomically at load creation.
// Replacements, deletion and schema pruning affect only future loads.
func (s *mysqlDB) ListFoundryLoadDecisions(ctx context.Context, loadID, afterID string, documentIDs []string, limit int) ([]*model.FoundryReviewDecision, error) {
	return s.listFoundryDecisions(ctx, "foundry_load_decisions", "load_id", loadID, afterID, documentIDs, limit)
}

// table and key are internal SQL identifiers, never request values.
func (s *mysqlDB) listFoundryDecisions(ctx context.Context, table, key, ontologyID, afterID string, documentIDs []string, limit int) ([]*model.FoundryReviewDecision, error) {
	if limit < 1 || limit > 10001 {
		return nil, fmt.Errorf("decision limit must be 1..10001")
	}
	query := "SELECT " + foundryReviewDecisionColumns + " FROM " + table + " WHERE " + key + "=? AND id>?"
	args := []any{ontologyID, afterID}
	if documentIDs != nil {
		query += " AND (kind='prop'"
		if len(documentIDs) > 0 {
			query += " OR doc_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(documentIDs)), ",") + ")"
			for _, id := range documentIDs {
				args = append(args, id)
			}
		}
		query += ")"
	}
	query += " ORDER BY id LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.FoundryReviewDecision
	for rows.Next() {
		d, err := scanFoundryReviewDecision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *mysqlDB) DeleteFoundryReviewDecision(ctx context.Context, ontologyID, id string) error {
	return execOrNotFound(ctx, s.db, `DELETE FROM foundry_review_decisions WHERE ontology_id=? AND id=?`, ontologyID, id)
}

func scanFoundryReviewDecision(row rowScanner) (*model.FoundryReviewDecision, error) {
	var d model.FoundryReviewDecision
	var doc, name, entity, prop sql.NullString
	var value []byte
	err := row.Scan(&d.ID, &d.OntologyID, &d.Kind, &d.Type, &doc, &name, &entity, &prop, &value, &d.DecidedByUserID, &d.DecidedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if doc.Valid {
		d.DocID = &doc.String
	}
	if name.Valid {
		d.NormName = &name.String
	}
	if entity.Valid {
		d.EntityKey = &entity.String
	}
	if prop.Valid {
		d.Prop = &prop.String
	}
	d.Value = value
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("stored decision: %w", err)
	}
	return &d, nil
}

// pruneFoundryReviewDecisions drops the ontology's decisions that an Extract
// of the version it now points at doesn't take
// (FoundryReviewDecision.Extractable): their type or prop left it, or a prop
// decision's key or value no longer fits it. A resolve whose chosen entity a
// changed key no longer reads stays: the next load copies it, and the engine
// reviews its name again (settling it again replaces it), where dropped the
// name would be matched another way. SetCurrentOntologyVersion runs it in its
// transaction, so a draft that never becomes current leaves the decisions
// alone. Only an agent draft whose ontology names types prunes: a Definition
// v1, which a load can't write, or a draft with no types, changes nothing.
// Loads already made keep their snapshot (foundry_load_decisions), and the
// graph worker leaves out what their own version doesn't take.
func (s *mysqlDB) pruneFoundryReviewDecisions(ctx context.Context, ontologyID, versionID string) error {
	var format string
	var definition []byte
	if err := s.db.QueryRowContext(ctx, `SELECT format, definition FROM foundry_ontology_versions WHERE id=? AND ontology_id=?`,
		versionID, ontologyID).Scan(&format, &definition); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return err
	}
	if model.OntologyFormat(format) != model.OntologyFormatAgentBundle {
		return nil
	}
	var def struct {
		OntologyTOML string `json:"ontology_toml"`
	}
	if err := json.Unmarshal(definition, &def); err != nil {
		return err
	}
	types, err := model.FoundryDecisionTypes(def.OntologyTOML)
	if err != nil || len(types) == 0 {
		return nil // not an ontology to check decisions against: keep them
	}
	// A decision list may run past 10,000: page by id, and delete each page's
	// stale ones by id.
	for after := ""; ; {
		page, err := s.listFoundryDecisions(ctx, "foundry_review_decisions", "ontology_id", ontologyID, after, nil, 1000)
		if err != nil {
			return err
		}
		var stale []any
		for _, d := range page {
			if !d.Extractable(types) {
				stale = append(stale, d.ID)
			}
		}
		if len(stale) > 0 {
			if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_review_decisions WHERE ontology_id=? AND id IN (`+
				idPlaceholders(len(stale))+`)`, append([]any{ontologyID}, stale...)...); err != nil {
				return err
			}
		}
		if len(page) < 1000 {
			return nil
		}
		after = page[len(page)-1].ID
	}
}
