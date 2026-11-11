package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"slices"
	"strings"
)

const foundryReviewDecisionColumns = "id, ontology_id, kind, type, doc_id, norm_name, entity_key, prop, value, claim_key, claim_section, claim_start, claim_end, statement_hash, decided_by_user_id, decided_at"

// SettleFoundryReviewDecision replaces the choice for this identity atomically.
// It preserves the id of an existing choice. Under the ontology's lock it
// checks d against the version of the ontology's current load, the load whose
// review it settles: ErrConflict means that version doesn't take d (its type,
// prop or key changed, as a new load of another version may have done since
// the review was read), or the ontology has no current load. It applies from
// the next load; a release is settled by SettleFoundryReleases, which applies
// it to the current one.
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
		return tx.saveFoundryReviewDecision(ctx, d)
	})
}

// saveFoundryReviewDecision writes d, or replaces the choice of its identity,
// keeping that one's id, and reads it back into d. The caller holds the
// ontology's lock and has checked d.
func (s *mysqlDB) saveFoundryReviewDecision(ctx context.Context, d *model.FoundryReviewDecision) error {
	var value, section, start, end any
	if len(d.Value) > 0 {
		value = string(d.Value)
	}
	if span := d.ClaimSpan; span != nil {
		section, start, end = span.Section, span.Start, span.End
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_review_decisions
 (id, ontology_id, kind, type, doc_id, norm_name, entity_key, prop, value, claim_key, claim_section, claim_start, claim_end, statement_hash, decided_by_user_id)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, CAST(? AS JSON), ?, ?, ?, ?, ?, ?)
 ON DUPLICATE KEY UPDATE entity_key=VALUES(entity_key), value=VALUES(value), statement_hash=VALUES(statement_hash),
 decided_by_user_id=VALUES(decided_by_user_id), decided_at=NOW(6)`,
		d.ID, d.OntologyID, d.Kind, d.Type, d.DocID, d.NormName, d.EntityKey, d.Prop, value, d.ClaimKey, section, start, end, d.StatementHash, d.DecidedByUserID)
	if err != nil {
		return err
	}
	// <=> is NULL-safe equality; resolve's chosen entity is not its identity.
	stored, err := scanFoundryReviewDecision(s.db.QueryRowContext(ctx, "SELECT "+foundryReviewDecisionColumns+`
 FROM foundry_review_decisions WHERE ontology_id=? AND kind=? AND type=? AND doc_id <=> ? AND norm_name <=> ?
 AND prop <=> ? AND claim_key <=> ? AND claim_section <=> ? AND claim_start <=> ? AND claim_end <=> ?
 AND (kind='resolve' OR entity_key <=> ?)`, d.OntologyID, d.Kind, d.Type, d.DocID, d.NormName, d.Prop, d.ClaimKey, section, start, end, d.EntityKey))
	if err != nil {
		return err
	}
	// Added to, never replaced: a load made before this settle copied the
	// decision's earlier value, which came from the earlier documents.
	if len(d.DocumentIDs) > 0 {
		rows := strings.Repeat("(?, ?), ", len(d.DocumentIDs))
		args := make([]any, 0, 2*len(d.DocumentIDs))
		for _, doc := range d.DocumentIDs {
			args = append(args, stored.ID, doc)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_review_decision_documents (decision_id, document_id) VALUES `+
			rows[:len(rows)-2], args...); err != nil {
			return err
		}
	}
	*d = *stored
	return nil
}

// SettleFoundryReleases settles release decisions, each read from a review of
// load loadID, and applies them to that load at once: under the ontology's
// lock each is saved as SettleFoundryReviewDecision saves one, joins the
// load's own list as pending (in place of any earlier decision the list holds
// for its claim), and its document is owed a resend (oweFoundryResends).
// Then, with no job live, resend is queued as a resend job of the load, due
// after model.FoundryResendSettle; a queued resend of the load is put off by
// as much instead, so approvals that come together go in one job; a live
// delete gives way, as to a new load, and resumes after; any other live job's
// end queues the resend (queueAfterFoundryGraphJob). queued reports a job
// queued here. Each decision comes back as stored, with what the load holds
// of it (Applied). ErrConflict means loadID is no longer the ontology's
// current load, so its reviews are not the ones to settle; ErrNotFound that
// the ontology is not live.
func (s *mysqlDB) SettleFoundryReleases(ctx context.Context, workspaceID, loadID string, decisions []*model.FoundryReviewDecision,
	resend *model.FoundryGraphJob) (queued bool, err error) {
	if len(decisions) == 0 {
		return false, nil
	}
	ontologyID := decisions[0].OntologyID
	for _, d := range decisions {
		if err := d.Validate(); err != nil {
			return false, err
		}
		if d.Kind != model.FoundryReviewRelease || d.OntologyID != ontologyID {
			return false, fmt.Errorf("decision %s is not a release of ontology %s", d.ID, ontologyID)
		}
	}
	err = s.WithTx(ctx, func(db Store) error {
		tx := db.(*mysqlDB)
		o, err := tx.lockLiveFoundryOntology(ctx, workspaceID, ontologyID)
		if err != nil {
			return err
		}
		if o.CurrentLoadID == nil || *o.CurrentLoadID != loadID {
			return ErrConflict
		}
		// The live job's row before any decision's: a worker recording what
		// became of a decision holds its job's row first, so the two never
		// wait on each other.
		live, err := tx.liveFoundryGraphJob(ctx, ontologyID)
		if err != nil {
			return err
		}
		var documents []string
		for _, d := range decisions {
			if err := tx.saveFoundryReviewDecision(ctx, d); err != nil {
				return err
			}
			span := d.ClaimSpan
			// One decision a claim in a load's list: the engine refuses an
			// Extract that lists a release twice.
			if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_load_decisions
WHERE load_id=? AND kind='release' AND doc_id=? AND claim_key=? AND claim_section=? AND claim_start=? AND claim_end=? AND id<>?`,
				loadID, d.DocID, d.ClaimKey, span.Section, span.Start, span.End, d.ID); err != nil {
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_load_decisions (load_id, `+foundryReviewDecisionColumns+`, applied_status)
SELECT ?, `+foundryReviewDecisionColumns+`, 'pending' FROM foundry_review_decisions WHERE ontology_id=? AND id=?
ON DUPLICATE KEY UPDATE statement_hash=VALUES(statement_hash), decided_by_user_id=VALUES(decided_by_user_id),
 decided_at=VALUES(decided_at), applied_status='pending', applied_reason=NULL, applied_at=NULL`, loadID, ontologyID, d.ID); err != nil {
				return err
			}
			d.Applied = &model.FoundryDecisionApplied{Status: model.FoundryReleasePending}
			if !slices.Contains(documents, *d.DocID) {
				documents = append(documents, *d.DocID)
			}
		}
		if err := tx.oweFoundryResends(ctx, loadID, model.FoundryResendApproval, documents); err != nil {
			return err
		}
		switch {
		case live == nil:
		case live.Kind == model.FoundryGraphJobDelete:
			if err := tx.cancelLiveFoundryGraphJob(ctx, ontologyID, "an approval went first; the delete resumes after it"); err != nil {
				return err
			}
		case live.Kind == model.FoundryGraphJobResend && live.LoadID == loadID && live.Status == model.FoundryGraphJobQueued:
			_, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_jobs
SET next_attempt_at=GREATEST(next_attempt_at, NOW(6) + INTERVAL ? MICROSECOND) WHERE id=? AND status='queued'`,
				model.FoundryResendSettle.Microseconds(), live.ID)
			return err
		default:
			return nil
		}
		queued, err = tx.queueFoundryResend(ctx, o, resend, model.FoundryResendSettle)
		return err
	})
	return queued, err
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
	return scanAll(rows, scanFoundryReviewDecision)
}

// DeleteFoundryReviewDecision removes a decision from those later loads
// copy. A load that copied it keeps its copy, and what it wrote. The current
// load's copy of a release it has not written goes with it, whatever a job
// said of it (waiting, refused, missed, or nothing yet): a resend sends every
// release its load's list holds of a document, so a copy left there would be
// written by the next approval of another claim of that document, after the
// person took it back, with no decision left to show for it. One the load
// wrote stays: the fact is in the load, and sent again it finds no review.
// A copy in a load that is not current is not reached here: it goes when
// that load is made current again, before any resend of it is queued
// (carryFoundryReleases). It holds the ontology's lock, as every writer of a load's decisions does,
// so the current load is the same throughout. ErrNotFound means the ontology
// has no such decision.
func (s *mysqlDB) DeleteFoundryReviewDecision(ctx context.Context, ontologyID, id string) error {
	return s.WithTx(ctx, func(db Store) error {
		tx := db.(*mysqlDB)
		var current sql.NullString
		if err := tx.db.QueryRowContext(ctx, `SELECT current_load_id FROM foundry_ontologies WHERE id=? FOR UPDATE`, ontologyID).
			Scan(&current); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if err := execOrNotFound(ctx, tx.db, `DELETE FROM foundry_review_decisions WHERE ontology_id=? AND id=?`, ontologyID, id); err != nil {
			return err
		}
		if !current.Valid {
			return nil
		}
		// <=> is NULL-safe: a copy no job has reported on has no status.
		_, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_load_decisions
WHERE load_id=? AND id=? AND kind='release' AND NOT (applied_status <=> 'written')`, current.String, id)
		return err
	})
}

func scanFoundryReviewDecision(row rowScanner) (*model.FoundryReviewDecision, error) {
	var d model.FoundryReviewDecision
	var doc, name, entity, prop, claim, hash sql.NullString
	var section, start, end sql.NullInt64
	var value []byte
	err := row.Scan(&d.ID, &d.OntologyID, &d.Kind, &d.Type, &doc, &name, &entity, &prop, &value, &claim, &section, &start, &end, &hash, &d.DecidedByUserID, &d.DecidedAt)
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
	if claim.Valid {
		d.ClaimKey = &claim.String
	}
	if hash.Valid {
		d.StatementHash = &hash.String
	}
	if section.Valid && start.Valid && end.Valid {
		d.ClaimSpan = &model.FoundryClaimSpan{Section: uint32(section.Int64), Start: uint32(start.Int64), End: uint32(end.Int64)}
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
