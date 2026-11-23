package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/pkg/namewords"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"slices"
	"strings"
)

const foundryReviewDecisionColumns = "id, ontology_id, kind, type, doc_id, norm_name, entity_key, prop, value, claim_key, claim_section, claim_start, claim_end, statement_hash, decided_by_user_id, decided_at"

// foundryReviewDecisionDrawnColumns are a saved decision's columns with how
// the review queue drew its card, which only foundry_review_decisions keeps:
// a load's list copies foundryReviewDecisionColumns, and no Extract carries it.
const foundryReviewDecisionDrawnColumns = foundryReviewDecisionColumns + ", drawn"

// FoundryValueDecisionsFull refuses a value decision of a property the
// ontology has decided no value of: it holds
// model.FoundryExtractKindDecisionsMax value decisions already, and every one
// goes with every Extract of a load, which takes no more. Choosing again for
// a property already decided replaces that decision and is not refused.
var FoundryValueDecisionsFull = errors.New("the ontology holds as many value decisions as one extract takes")

// SettleFoundryReviewDecision replaces the choice for this identity atomically.
// It preserves the id of an existing choice. Under the ontology's lock it
// checks d against the version of the ontology's current load, the load whose
// review it settles: ErrConflict means that version doesn't take d (its type,
// prop or key changed, as a new load of another version may have done since
// the review was read), or the ontology has no current load. It applies from
// the next load; a person's settle (SettleFoundryReviews) applies a resolve,
// a release or a value to the current one too. A resolve that chooses an entity is also an alias of
// every later load (ListFoundryLoadAliasesNamed), sent only with the
// Extracts whose documents write its name, so an ontology takes any number.
// A resolve saved here is not the current load's: it reaches a load created
// before it as that load becomes current (carryFoundryResolves).
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
		types, err := tx.foundryLoadTypes(ctx, *o.CurrentLoadID)
		if err != nil {
			return err
		}
		if !d.Fits(types) {
			return ErrConflict
		}
		if err := tx.saveFoundryReviewDecision(ctx, d, false); err != nil {
			return err
		}
		// A decision may close a card, or open one it replaced.
		return tx.markFoundryReviewsDirty(ctx, d.OntologyID)
	})
}

// saveFoundryReviewDecision writes d, or replaces the choice of its identity,
// keeping that one's id, and reads it back into d. With stands, the choice of
// its identity is d's already and is left as it is, with who made it and
// when: only d's documents are added to it. A release of the statement its
// claim's approval already binds stands too: from contract 4.14.0 the
// approval's time says which of a link's approvals is its decision, on a
// link that holds one value, and dates the decision of a quote that gives no
// date and has no document date, so it stays the time the person first
// approved what they read, in every Extract that sends it, as a load that
// wrote it keeps it. The caller holds the ontology's lock and has checked d,
// and marks the open counts dirty (markFoundryReviewsDirty) once its
// transaction has saved what it saves.
func (s *mysqlDB) saveFoundryReviewDecision(ctx context.Context, d *model.FoundryReviewDecision, stands bool) error {
	var value, section, start, end any
	if len(d.Value) > 0 {
		value = string(d.Value)
	}
	if span := d.ClaimSpan; span != nil {
		section, start, end = span.Section, span.Start, span.End
	}
	if !stands {
		// drawn goes with who decided and when: an approval of the statement
		// it approved keeps all three, as it is the decision first made; any
		// other settle is a decision made now, from the queue or not.
		if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_review_decisions
 (id, ontology_id, kind, type, doc_id, norm_name, entity_key, prop, value, claim_key, claim_section, claim_start, claim_end, statement_hash, decided_by_user_id, drawn, name_key)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, CAST(? AS JSON), ?, ?, ?, ?, ?, ?, ?, ?)
 ON DUPLICATE KEY UPDATE
 drawn=IF(kind='release' AND statement_hash <=> VALUES(statement_hash), drawn, VALUES(drawn)),
 decided_by_user_id=IF(kind='release' AND statement_hash <=> VALUES(statement_hash), decided_by_user_id, VALUES(decided_by_user_id)),
 decided_at=IF(kind='release' AND statement_hash <=> VALUES(statement_hash), decided_at, NOW(6)),
 entity_key=VALUES(entity_key), value=VALUES(value), statement_hash=VALUES(statement_hash), name_key=VALUES(name_key)`,
			d.ID, d.OntologyID, d.Kind, d.Type, d.DocID, d.NormName, d.EntityKey, d.Prop, value, d.ClaimKey, section, start, end, d.StatementHash, d.DecidedByUserID, d.Drawn,
			foundryNameKey(d)); err != nil {
			return err
		}
	}
	// <=> is NULL-safe equality; resolve's chosen entity is not its identity.
	stored, err := scanFoundryReviewDecisionDrawn(s.db.QueryRowContext(ctx, "SELECT "+foundryReviewDecisionDrawnColumns+`
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

// foundryClaimAt is the SQL condition, with alias t's columns, that a decision
// is of the claim at a place: its document and place, and its key or the
// statement it binds. From contract 4.14.0 the engine binds a release whose
// key names no review of its document by its place and statement hash, so a
// decision saved under the key the claim had before a rebuild moved it is
// this claim's too. Its arguments are foundryClaimArgs's.
func foundryClaimAt(t string) string {
	return t + ".doc_id=? AND " + t + ".claim_section=? AND " + t + ".claim_start=? AND " + t + ".claim_end=? AND (" +
		t + ".claim_key=? OR " + t + ".statement_hash=?)"
}

// foundryRejected is the SQL condition that the ontology `ontology` (a column
// or a placeholder) holds a rejection of the claim the release with alias t
// names (foundryClaimAt): a release no Extract sends.
func foundryRejected(t, ontology string) string {
	return `EXISTS (SELECT 1 FROM foundry_review_decisions j WHERE j.ontology_id=` + ontology + ` AND j.kind='reject'
  AND j.doc_id=` + t + `.doc_id AND j.claim_section=` + t + `.claim_section AND j.claim_start=` + t + `.claim_start
  AND j.claim_end=` + t + `.claim_end AND (j.claim_key=` + t + `.claim_key OR j.statement_hash=` + t + `.statement_hash))`
}

// foundryClaimArgs are foundryClaimAt's arguments for the claim d names.
func foundryClaimArgs(d *model.FoundryReviewDecision) []any {
	span := d.ClaimSpan
	return []any{d.DocID, span.Section, span.Start, span.End, d.ClaimKey, d.StatementHash}
}

// ListFoundryReviewDecisions uses id keyset paging. A nil documentIDs lists all
// decisions; a non-nil list includes all prop choices and only those documents'
// resolve choices. Callers request limit+1 to detect an extraction over the cap.
func (s *mysqlDB) ListFoundryReviewDecisions(ctx context.Context, ontologyID, afterID string, documentIDs []string, limit int) ([]*model.FoundryReviewDecision, error) {
	return s.listFoundryDecisions(ctx, "foundry_review_decisions", "ontology_id", ontologyID, afterID, documentIDs, limit)
}

// ListFoundryShownDecisions lists what a reader is shown of an ontology's
// decisions, limit at a time by id: its decisions (ListFoundryReviewDecisions),
// and its current load's withdraws (model.FoundryReviewWithdraw), approvals
// taken back from that load that the ontology no longer holds, which a page
// shows with what became of each while that load is current. A withdraw
// keeps the id of its release, which the ontology no longer holds, so no id
// is listed twice. Two reads, each of at most limit rows in id order, merged.
func (s *mysqlDB) ListFoundryShownDecisions(ctx context.Context, ontologyID, afterID string, limit int) ([]*model.FoundryReviewDecision, error) {
	decisions, err := s.listFoundryDecisions(ctx, "foundry_review_decisions", "ontology_id", ontologyID, afterID, nil, limit)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryReviewDecisionColumns+` FROM foundry_load_decisions
WHERE load_id=(SELECT current_load_id FROM foundry_ontologies WHERE id=?) AND kind=? AND id>? ORDER BY id LIMIT ?`,
		ontologyID, string(model.FoundryReviewWithdraw), afterID, limit)
	if err != nil {
		return nil, err
	}
	withdraws, err := scanAll(rows, scanFoundryReviewDecision)
	if err != nil || len(withdraws) == 0 {
		return decisions, err
	}
	shown := append(decisions, withdraws...)
	slices.SortFunc(shown, func(a, b *model.FoundryReviewDecision) int { return strings.Compare(a.ID, b.ID) })
	return shown[:min(len(shown), limit)], nil
}

// ListFoundryLoadDecisions reads the decisions a load sends the engine: those
// the ontology held when the load was created, the releases and value
// decisions settled against it or carried to it since, and no value decision
// removed since: one the load has yet to be told is gone stays in its list
// (model.FoundryValueRemoved) and is left out here, which is what tells the
// engine to withdraw it. So is a release of a claim the ontology holds a
// rejection of (foundryClaimAt), whatever put it there: a rejection settled
// since takes the claim's approvals from every list (settleReject),
// and this keeps one saved before that did, or by an older binary, from
// writing a claim a person said is wrong. A withdraw is sent while it waits
// (pending), and never once the engine has said what it did. A resolve
// replaced or removed, and schema pruning, affect only future loads.
func (s *mysqlDB) ListFoundryLoadDecisions(ctx context.Context, loadID, afterID string, documentIDs []string, limit int) ([]*model.FoundryReviewDecision, error) {
	return s.listFoundryDecisions(ctx, "foundry_load_decisions", "load_id", loadID, afterID, documentIDs, limit)
}

// foundryNameKey is the name_key of a resolve decision that chose an entity,
// an alias (namewords.Key of its norm_name); NULL for "not in data", which
// carries nothing, for any other kind, and for a name with no word.
func foundryNameKey(d *model.FoundryReviewDecision) any {
	if d.Kind != model.FoundryReviewResolve || d.NormName == nil || d.EntityKey == nil {
		return nil
	}
	key, ok := namewords.Key(*d.NormName)
	if !ok {
		return nil
	}
	return key
}

// FoundryAliasKeysPerRead is the most name keys one ListFoundryLoadAliasesNamed
// reads: an IN list MySQL's range optimizer takes whole, so the read stays
// on foundry_load_decisions_names_idx.
const FoundryAliasKeysPerRead = 2000

// ListFoundryLoadAliasesNamed reads the aliases of a load's list whose
// name_key is one of keys (at most FoundryAliasKeysPerRead): the names a
// person linked to an entity that a document whose words have those keys may
// write (namewords.Text.Keys), which the load carries to it (contract
// 4.23.0). One a type, name and entity, of any document: the earliest
// decided, the smallest id among those decided at once, so every Extract
// sends the same alias; a name chosen as two entities gives both. The caller
// checks that the document writes each name. Point reads on
// foundry_load_decisions_names_idx (load_id, kind, name_key), numbered over
// the rows they find alone: what it costs is bounded by the keys and their
// rows, not by the load.
func (s *mysqlDB) ListFoundryLoadAliasesNamed(ctx context.Context, loadID string, keys []int64) ([]*model.FoundryReviewDecision, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	if len(keys) > FoundryAliasKeysPerRead {
		return nil, fmt.Errorf("at most %d name keys a read", FoundryAliasKeysPerRead)
	}
	args := make([]any, 0, len(keys)+1)
	args = append(args, loadID)
	for _, k := range keys {
		args = append(args, k)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryReviewDecisionColumns+" FROM (SELECT "+foundryReviewDecisionColumns+`,
  ROW_NUMBER() OVER (PARTITION BY type, norm_name, entity_key ORDER BY decided_at, id) AS earliest
  FROM foundry_load_decisions WHERE load_id=? AND kind='resolve' AND name_key IN (`+strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")+`)
  AND entity_key IS NOT NULL) a
WHERE earliest=1`, args...)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundryReviewDecision)
}

// FoundryLoadHasAliases reports whether the load's list holds an alias with
// its name key: with none, no document needs an alias read. One index probe.
func (s *mysqlDB) FoundryLoadHasAliases(ctx context.Context, loadID string) (bool, error) {
	var has bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_load_decisions
WHERE load_id=? AND kind='resolve' AND name_key IS NOT NULL)`, loadID).Scan(&has)
	return has, err
}

// FillFoundryLoadNameKeys gives the load's aliases (resolves that chose an
// entity) that have no name_key theirs, and the ontology's saved decisions
// they were copied from (foundry_review_decisions, by id), so the next load
// copies them keyed: those settled before name keys, and copies an older
// binary made. A thousand at a time, through foundry_load_decisions_names_idx,
// so with none left it reads one empty range. It returns how many it filled.
func (s *mysqlDB) FillFoundryLoadNameKeys(ctx context.Context, loadID string) (int, error) {
	type unkeyed struct{ id, ontology, name string }
	filled := 0
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT id, ontology_id, norm_name FROM foundry_load_decisions
WHERE load_id=? AND kind='resolve' AND name_key IS NULL AND norm_name IS NOT NULL AND entity_key IS NOT NULL ORDER BY id LIMIT 1000`, loadID)
		if err != nil {
			return filled, err
		}
		page, err := scanAll(rows, func(row rowScanner) (unkeyed, error) {
			var u unkeyed
			return u, row.Scan(&u.id, &u.ontology, &u.name)
		})
		if err != nil {
			return filled, err
		}
		if len(page) == 0 {
			return filled, nil
		}
		// A name with no word is keyed 0, which no text's words hash to in
		// practice, so it is keyed once and never read.
		var cases strings.Builder
		keys := make([]any, 0, 2*len(page))
		ids := make([]any, 0, len(page))
		for _, u := range page {
			key, _ := namewords.Key(u.name)
			cases.WriteString(" WHEN ? THEN ?")
			keys = append(keys, u.id, key)
			ids = append(ids, u.id)
		}
		in := strings.TrimSuffix(strings.Repeat("?,", len(page)), ",")
		if err := s.WithTx(ctx, func(db Store) error {
			tx := db.(*mysqlDB)
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_decisions SET name_key = CASE id`+cases.String()+` END
WHERE load_id=? AND id IN (`+in+`)`, append(append(append([]any{}, keys...), loadID), ids...)...); err != nil {
				return err
			}
			// The saved decision it copies, if it is still the same name.
			_, err := tx.db.ExecContext(ctx, `UPDATE foundry_review_decisions SET name_key = CASE id`+cases.String()+` END
WHERE ontology_id=? AND id IN (`+in+`) AND kind='resolve' AND name_key IS NULL AND entity_key IS NOT NULL`,
				append(append(append([]any{}, keys...), page[0].ontology), ids...)...)
			return err
		}); err != nil {
			return filled, err
		}
		filled += len(page)
	}
}

// table and key are internal SQL identifiers, never request values.
func (s *mysqlDB) listFoundryDecisions(ctx context.Context, table, key, ontologyID, afterID string, documentIDs []string, limit int) ([]*model.FoundryReviewDecision, error) {
	if limit < 1 || limit > 10001 {
		return nil, fmt.Errorf("decision limit must be 1..10001")
	}
	columns, scan := foundryReviewDecisionDrawnColumns, scanFoundryReviewDecisionDrawn
	if table == "foundry_load_decisions" {
		columns, scan = foundryReviewDecisionColumns, scanFoundryReviewDecision
	}
	query := "SELECT " + columns + " FROM " + table + " WHERE " + key + "=? AND id>?"
	if table == "foundry_load_decisions" {
		// <=> is NULL-safe: a copy no job has reported on has no status.
		query += " AND NOT (applied_status <=> '" + string(model.FoundryValueRemoved) + `')
 AND NOT (kind='withdraw' AND NOT (applied_status <=> 'pending'))
 AND NOT (kind='release' AND ` + foundryRejected("foundry_load_decisions",
			"(SELECT o.ontology_id FROM foundry_loads o WHERE o.id=foundry_load_decisions.load_id)") + `)`
	}
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
	return scanAll(rows, scan)
}

// DeleteFoundryReviewDecision removes a decision from those later loads
// copy, and takes it back from the loads that hold it as far as each kind
// can be. A resolve stays in the loads that copied it, with what it wrote;
// its copy in the current load that waits, or was refused, goes, and so does
// one in another load when that load is current again (carryFoundryResolves),
// so no resend sends a choice a person took back. A
// reject is in no load: with it gone, its claim's card is listed again.
//
// A release is taken back from the current load at once (contract 4.16.0).
// Its copy there, if the load may have written it (foundryMayHaveWritten:
// written, unconfirmed, or of a load a job of which is in flight), becomes
// a withdraw, its document owes a resend, and the resend is queued as a
// settle queues one (queueFoundryResendBehind): the engine takes back what
// the approval wrote, and the claim is held again, its card back. A copy the
// load never wrote goes, whatever a job said of it (waiting, refused, missed,
// or nothing yet), and nothing is sent: a resend sends every release its
// load's list holds of a document, so a copy left there would be written by
// the next approval of another claim of that document. A copy in a load
// that is not current is not reached here: when that load is current again,
// its carry takes it back or drops it alike, before any resend of it is
// queued (carryFoundryReleases).
//
// A value decision is sent to no load again: its copy in every load of the
// ontology that holds one, under its id or one the property's decision had
// before its value changed, is marked removed (model.FoundryValueRemoved),
// which no Extract sends, and each of those owes the engine its decisions
// again (model.FoundryResendDecisions): the engine withdraws a value decision
// it is no longer sent, and the property goes back to what its other
// statements give. The current load is sent its list at once, by resend,
// queued as a settle queues one (queueFoundryResendBehind); a load being
// written sends its list with what it has yet to send, and once more when
// its job ends and it is current; any other is sent it if it is made
// current. So no load goes on applying a decision that no longer exists.
// The copy itself stays until the load's request ends (EndFoundryLoadResend):
// until then the load holds the value, and what a reader with partial access
// is withheld is read from the list, so the property stays withheld for as
// long as the value is there. A resend that fails leaves the copy, marked
// so, and the end of the ontology's next job sends the list again
// (FinishFoundryGraphJob), so a withdrawal is never dropped unsaid.
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
		var kind, ty string
		var entity, prop sql.NullString
		if err := tx.db.QueryRowContext(ctx, `SELECT kind, type, entity_key, prop FROM foundry_review_decisions WHERE ontology_id=? AND id=? FOR UPDATE`,
			ontologyID, id).Scan(&kind, &ty, &entity, &prop); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_review_decisions WHERE ontology_id=? AND id=?`, ontologyID, id); err != nil {
			return err
		}
		if err := tx.markFoundryReviewsDirty(ctx, ontologyID); err != nil {
			return err
		}
		switch model.FoundryReviewDecisionKind(kind) {
		case model.FoundryReviewResolve:
			// A copy the current load waits to settle, or one it refused, has
			// written nothing, and is not sent again; one it wrote stays, as
			// what it wrote stays, and so does one the load was created with.
			if o.CurrentLoadID == nil {
				return nil
			}
			_, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_load_decisions WHERE load_id=? AND id=? AND kind='resolve'
  AND applied_status IN ('pending', 'carried', 'refused')`, *o.CurrentLoadID, id)
			return err
		case model.FoundryReviewRelease:
			if o.CurrentLoadID == nil {
				return nil
			}
			withdrew, err := tx.withdrawFoundryReleases(ctx, *o.CurrentLoadID, "t.id=?", []any{id})
			if err != nil || !withdrew {
				return err
			}
			_, err = tx.queueFoundryResendBehind(ctx, o, live, resend)
			return err
		case model.FoundryReviewProp:
			// Every copy of the property's decision, under this id or one it
			// had before its value changed: a load that is not current may
			// hold an earlier one. Retention keeps an ontology's loads few.
			rows, err := tx.db.QueryContext(ctx, `SELECT DISTINCT d.load_id FROM foundry_loads l
JOIN foundry_load_decisions d ON d.load_id=l.id AND d.kind='prop' AND d.type=? AND d.entity_key=? AND d.prop=?
WHERE l.ontology_id=? AND l.status<>'deleted' ORDER BY d.load_id`, ty, entity, prop, ontologyID)
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
				if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_decisions SET applied_status=?, applied_reason=NULL, applied_at=NULL
WHERE load_id=? AND kind='prop' AND type=? AND entity_key=? AND prop=?`, string(model.FoundryValueRemoved), load, ty, entity, prop); err != nil {
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

// scanFoundryReviewDecision reads foundryReviewDecisionColumns, which both
// tables keep.
func scanFoundryReviewDecision(row rowScanner) (*model.FoundryReviewDecision, error) {
	return scanDecision(row, false)
}

// scanFoundryReviewDecisionDrawn reads foundryReviewDecisionDrawnColumns: a
// saved decision, with how the review queue drew its card.
func scanFoundryReviewDecisionDrawn(row rowScanner) (*model.FoundryReviewDecision, error) {
	return scanDecision(row, true)
}

func scanDecision(row rowScanner, withDrawn bool) (*model.FoundryReviewDecision, error) {
	var d model.FoundryReviewDecision
	var doc, name, entity, prop, claim, hash, drawn sql.NullString
	var section, start, end sql.NullInt64
	var value []byte
	dest := []any{&d.ID, &d.OntologyID, &d.Kind, &d.Type, &doc, &name, &entity, &prop, &value, &claim, &section, &start, &end, &hash, &d.DecidedByUserID, &d.DecidedAt}
	if withDrawn {
		dest = append(dest, &drawn)
	}
	err := row.Scan(dest...)
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
	if drawn.Valid {
		v := model.FoundryReviewDrawn(drawn.String)
		d.Drawn = &v
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
			if err := s.markFoundryReviewsDirty(ctx, ontologyID); err != nil {
				return err
			}
		}
		if len(page) < 1000 {
			return nil
		}
		after = page[len(page)-1].ID
	}
}
