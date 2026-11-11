package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"slices"
	"strings"
)

const foundryReviewDecisionColumns = "id, ontology_id, kind, type, doc_id, norm_name, entity_key, prop, value, claim_key, claim_section, claim_start, claim_end, statement_hash, decided_by_user_id, decided_at"

// FoundryValueDecisionsFull refuses a value decision of a property the
// ontology has decided no value of: it holds
// model.FoundryExtractDecisionsMax value decisions already, and every one
// goes with every Extract of a load, which takes no more. Choosing again for
// a property already decided replaces that decision and is not refused.
var FoundryValueDecisionsFull = errors.New("the ontology holds as many value decisions as one extract takes")

// SettleFoundryReviewDecision replaces the choice for this identity atomically.
// It preserves the id of an existing choice. Under the ontology's lock it
// checks d against the version of the ontology's current load, the load whose
// review it settles: ErrConflict means that version doesn't take d (its type,
// prop or key changed, as a new load of another version may have done since
// the review was read), or the ontology has no current load. It applies from
// the next load, as a resolve does; a release is settled by
// SettleFoundryReleases and a value by SettleFoundryValue, which apply them
// to the current one.
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
		if takes, err := tx.foundryLoadTakes(ctx, *o.CurrentLoadID, d); err != nil || !takes {
			return cmp.Or(err, ErrConflict)
		}
		return tx.saveFoundryReviewDecision(ctx, d)
	})
}

// foundryLoadTakes reports whether the version load loadID was made from
// takes d (model.FoundryReviewDecision.Fits).
func (s *mysqlDB) foundryLoadTakes(ctx context.Context, loadID string, d *model.FoundryReviewDecision) (bool, error) {
	var definition []byte
	if err := s.db.QueryRowContext(ctx, `SELECT v.definition FROM foundry_loads l
JOIN foundry_ontology_versions v ON v.id = l.version_id WHERE l.id=?`, loadID).Scan(&definition); err != nil {
		return false, err
	}
	var def struct {
		OntologyTOML string `json:"ontology_toml"`
	}
	if err := json.Unmarshal(definition, &def); err != nil {
		return false, err
	}
	types, err := model.FoundryDecisionTypes(def.OntologyTOML)
	if err != nil {
		return false, err
	}
	return d.Fits(types), nil
}

// SettleFoundryValue settles a value decision (kind prop), read from a
// review of load loadID, and applies it to that load at once: under the
// ontology's lock it is checked and saved as SettleFoundryReviewDecision
// saves one, joins the load's own list as pending, in place of any decision
// the list holds for its property (the engine refuses an Extract that decides
// a property twice), and the load owes the engine its decisions again
// (model.FoundryResendDecisions). Then resend is queued as SettleFoundryReleases
// queues one (queueFoundryResendBehind): one Extract of no documents carries
// the list, and the engine sets the value. queued reports a job queued here.
// d comes back as stored, with what the load holds of it (Applied).
// ErrConflict means loadID is no longer the ontology's current load, or its
// version doesn't take d; ErrNotFound that the ontology is not live;
// FoundryValueDecisionsFull that the ontology takes no value decision of
// another property.
func (s *mysqlDB) SettleFoundryValue(ctx context.Context, workspaceID, loadID string, d *model.FoundryReviewDecision,
	resend *model.FoundryGraphJob) (queued bool, err error) {
	if err := d.Validate(); err != nil {
		return false, err
	}
	if d.Kind != model.FoundryReviewProp {
		return false, fmt.Errorf("decision %s is not a value decision", d.ID)
	}
	err = s.WithTx(ctx, func(db Store) error {
		tx := db.(*mysqlDB)
		o, err := tx.lockLiveFoundryOntology(ctx, workspaceID, d.OntologyID)
		if err != nil {
			return err
		}
		if o.CurrentLoadID == nil || *o.CurrentLoadID != loadID {
			return ErrConflict
		}
		if takes, err := tx.foundryLoadTakes(ctx, loadID, d); err != nil || !takes {
			return cmp.Or(err, ErrConflict)
		}
		// The live job's row before any decision's, as SettleFoundryReleases
		// takes them.
		live, err := tx.liveFoundryGraphJob(ctx, d.OntologyID)
		if err != nil {
			return err
		}
		var decided bool
		var held int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(type=? AND entity_key=? AND prop=?), 0) > 0
FROM foundry_review_decisions WHERE ontology_id=? AND kind='prop'`, d.Type, d.EntityKey, d.Prop, d.OntologyID).Scan(&held, &decided); err != nil {
			return err
		}
		if !decided && held >= model.FoundryExtractDecisionsMax {
			return FoundryValueDecisionsFull
		}
		if err := tx.saveFoundryReviewDecision(ctx, d); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_load_decisions
WHERE load_id=? AND kind='prop' AND type=? AND entity_key=? AND prop=? AND id<>?`, loadID, d.Type, d.EntityKey, d.Prop, d.ID); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_load_decisions (load_id, `+foundryReviewDecisionColumns+`, applied_status)
SELECT ?, `+foundryReviewDecisionColumns+`, 'pending' FROM foundry_review_decisions WHERE ontology_id=? AND id=?
ON DUPLICATE KEY UPDATE value=VALUES(value), decided_by_user_id=VALUES(decided_by_user_id),
 decided_at=VALUES(decided_at), applied_status='pending', applied_reason=NULL, applied_at=NULL`, loadID, d.OntologyID, d.ID); err != nil {
			return err
		}
		d.Applied = &model.FoundryDecisionApplied{Status: model.FoundryReleasePending}
		if err := tx.oweFoundryResends(ctx, loadID, model.FoundryResendDecisions, []string{""}); err != nil {
			return err
		}
		queued, err = tx.queueFoundryResendBehind(ctx, o, live, resend)
		return err
	})
	return queued, err
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
// lock each is saved as SettleFoundryReviewDecision saves one, in place of a
// rejection of its claim, joins the load's own list as pending (in place of
// any earlier decision the list holds for its claim), and its document is
// owed a resend (oweFoundryResends).
// Then resend is queued, or left to the live job's end
// (queueFoundryResendBehind). queued reports a job queued here. Each decision
// comes back as stored, with what the load holds of it (Applied). ErrConflict
// means loadID is no longer the ontology's current load, so its reviews are
// not the ones to settle; ErrNotFound that the ontology is not live.
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
			// A claim is approved or rejected, never both.
			if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_review_decisions
WHERE ontology_id=? AND kind='reject' AND doc_id=? AND claim_key=? AND claim_section=? AND claim_start=? AND claim_end=?`,
				ontologyID, d.DocID, d.ClaimKey, span.Section, span.Start, span.End); err != nil {
				return err
			}
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
		queued, err = tx.queueFoundryResendBehind(ctx, o, live, resend)
		return err
	})
	return queued, err
}

// SettleFoundryReject settles a reject decision, read from a review of load
// loadID: a person's word that the claim the review holds back is wrong. It
// is saved as SettleFoundryReviewDecision saves one and goes nowhere else:
// no load's list takes it, nothing is sent to the engine and no job is
// queued, so the graph is as it was. Readers leave the claim's card out of
// this load and every later one while the decision stands
// (ListFoundryReviewDecisions), and removing it brings the card back.
//
// A claim is approved or rejected, never both: an approval saved for the
// claim goes, and its copy in the load's list with it, as removing it would
// take it (DeleteFoundryReviewDecision). ErrConflict means loadID is no
// longer the ontology's current load, or that load wrote the claim on an
// approval since the review was read, so it holds no claim there to reject;
// ErrNotFound that the ontology is not live.
func (s *mysqlDB) SettleFoundryReject(ctx context.Context, workspaceID, loadID string, d *model.FoundryReviewDecision) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if d.Kind != model.FoundryReviewReject {
		return fmt.Errorf("decision %s is not a reject", d.ID)
	}
	return s.WithTx(ctx, func(db Store) error {
		tx := db.(*mysqlDB)
		o, err := tx.lockLiveFoundryOntology(ctx, workspaceID, d.OntologyID)
		if err != nil {
			return err
		}
		if o.CurrentLoadID == nil || *o.CurrentLoadID != loadID {
			return ErrConflict
		}
		// The live job's row before any decision's, as every settle takes them.
		if _, err := tx.liveFoundryGraphJob(ctx, d.OntologyID); err != nil {
			return err
		}
		span := d.ClaimSpan
		claim := []any{d.DocID, d.ClaimKey, span.Section, span.Start, span.End}
		var written bool
		if err := tx.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_load_decisions
WHERE load_id=? AND kind='release' AND doc_id=? AND claim_key=? AND claim_section=? AND claim_start=? AND claim_end=? AND applied_status='written')`,
			append([]any{loadID}, claim...)...).Scan(&written); err != nil {
			return err
		}
		if written {
			return ErrConflict
		}
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_load_decisions
WHERE load_id=? AND kind='release' AND doc_id=? AND claim_key=? AND claim_section=? AND claim_start=? AND claim_end=?`,
			append([]any{loadID}, claim...)...); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_review_decisions
WHERE ontology_id=? AND kind='release' AND doc_id=? AND claim_key=? AND claim_section=? AND claim_start=? AND claim_end=?`,
			append([]any{d.OntologyID}, claim...)...); err != nil {
			return err
		}
		return tx.saveFoundryReviewDecision(ctx, d)
	})
}

// ListFoundryReviewDecisions uses id keyset paging. A nil documentIDs lists all
// decisions; a non-nil list includes all prop choices and only those documents'
// resolve choices. Callers request limit+1 to detect an extraction over the cap.
func (s *mysqlDB) ListFoundryReviewDecisions(ctx context.Context, ontologyID, afterID string, documentIDs []string, limit int) ([]*model.FoundryReviewDecision, error) {
	return s.listFoundryDecisions(ctx, "foundry_review_decisions", "ontology_id", ontologyID, afterID, documentIDs, limit)
}

// ListFoundryLoadDecisions reads the decisions a load sends the engine: those
// the ontology held when the load was created, the releases and value
// decisions settled against it or carried to it since, and no value decision
// removed since. A resolve replaced or removed, and schema pruning, affect
// only future loads.
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
// copy, and takes it back from the loads that hold it as far as each kind
// can be. A resolve stays in the loads that copied it, with what it wrote. A
// reject is in no load: with it gone, its claim's card is listed again.
//
// The current load's copy of a release it has not written goes with it,
// whatever a job said of it (waiting, refused, missed, or nothing yet): a
// resend sends every release its load's list holds of a document, so a copy
// left there would be written by the next approval of another claim of that
// document, after the person took it back, with no decision left to show
// for it. One the load wrote stays: the fact is in the load, and sent again
// it finds no review. A copy in a load that is not current is not reached
// here: it goes when that load is made current again, before any resend of
// it is queued (carryFoundryReleases).
//
// A value decision leaves the list of every load of the ontology that holds
// it, and each of those owes the engine its decisions again
// (model.FoundryResendDecisions): the engine withdraws a value decision it is
// no longer sent, and the property goes back to what its other statements
// give. The current load is sent its list at once, by resend, queued as a
// settle queues one (queueFoundryResendBehind); a load being written sends
// its list with what it has yet to send, and once more when its job ends
// and it is current; any other is sent it if it is made current. So no load
// goes on applying a decision that no longer exists.
//
// It holds the ontology's lock, as every writer of a load's decisions does,
// so the current load is the same throughout. ErrNotFound means the ontology
// is not live or has no such decision.
func (s *mysqlDB) DeleteFoundryReviewDecision(ctx context.Context, workspaceID, ontologyID, id string, resend *model.FoundryGraphJob) error {
	return s.WithTx(ctx, func(db Store) error {
		tx := db.(*mysqlDB)
		o, err := tx.lockLiveFoundryOntology(ctx, workspaceID, ontologyID)
		if err != nil {
			return err
		}
		// The live job's row before any decision's, as a settle takes them.
		live, err := tx.liveFoundryGraphJob(ctx, ontologyID)
		if err != nil {
			return err
		}
		var kind string
		if err := tx.db.QueryRowContext(ctx, `SELECT kind FROM foundry_review_decisions WHERE ontology_id=? AND id=? FOR UPDATE`, ontologyID, id).
			Scan(&kind); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_review_decisions WHERE ontology_id=? AND id=?`, ontologyID, id); err != nil {
			return err
		}
		switch model.FoundryReviewDecisionKind(kind) {
		case model.FoundryReviewRelease:
			if o.CurrentLoadID == nil {
				return nil
			}
			// <=> is NULL-safe: a copy no job has reported on has no status.
			_, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_load_decisions
WHERE load_id=? AND id=? AND kind='release' AND NOT (applied_status <=> 'written')`, *o.CurrentLoadID, id)
			return err
		case model.FoundryReviewProp:
			// Retention keeps an ontology's loads few.
			rows, err := tx.db.QueryContext(ctx, `SELECT d.load_id FROM foundry_loads l
JOIN foundry_load_decisions d ON d.load_id=l.id AND d.id=? AND d.kind='prop'
WHERE l.ontology_id=? AND l.status<>'deleted' ORDER BY l.id`, id, ontologyID)
			if err != nil {
				return err
			}
			var loads []string
			for rows.Next() {
				var load string
				if err := rows.Scan(&load); err != nil {
					_ = rows.Close()
					return err
				}
				loads = append(loads, load)
			}
			_ = rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			for _, load := range loads {
				if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_load_decisions WHERE load_id=? AND id=? AND kind='prop'`, load, id); err != nil {
					return err
				}
				if err := tx.oweFoundryResends(ctx, load, model.FoundryResendDecisions, []string{""}); err != nil {
					return err
				}
			}
			if o.CurrentLoadID == nil || !slices.Contains(loads, *o.CurrentLoadID) {
				return nil
			}
			_, err = tx.queueFoundryResendBehind(ctx, o, live, resend)
			return err
		}
		return nil
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
