package store

import (
	"cmp"
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
// the next load; a resolve is settled by SettleFoundryResolve, a release by
// SettleFoundryReleases and a value by SettleFoundryValue, which apply them
// to the current one. A resolve that chooses an entity is also an alias of
// every later load (ListFoundryLoadAliasesNamed), sent only with the
// Extracts whose documents write its name, so an ontology takes any number.
// A resolve saved here is not the current load's: it reaches a load created
// before it as that load becomes current (carryFoundryResolves). A person's
// resolve of a name the current load links already is settled by
// SettleFoundryBoundResolve, which knows the load its review was read from.
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
		return tx.saveFoundryReviewDecision(ctx, d, false)
	})
}

// SettleFoundryResolve settles a resolve decision, read from a review of load
// loadID, and applies it to that load at once: under the ontology's lock it
// is checked and saved as SettleFoundryReviewDecision saves one, joins the
// load's own list as pending in place of any copy the list holds of its
// identity (its type, document and name: the engine refuses an Extract that
// decides a name of a document twice), with its name key, and the load owes
// the engine a resend of documents (model.FoundryResendName): a changed
// choice is a new decision under d's id, and the same choice again keeps
// its decision; the decision's
// own, and those the caller found whose reviews ask about the name, as an
// alias reaches them. Then resend is queued as SettleFoundryReleases queues
// one (queueFoundryResendBehind). The engine settles the name in place by
// replaying each document's saved model work, and its word on the decision's
// own document is recorded on the copy (recordFoundryNameOutcomes). queued
// reports a job queued here. d comes back as stored, with what the load holds
// of it (Applied).
//
// Another load may have become current since the review was read (a load
// job ended, or a person made one current), as both take the ontology's
// lock. The decision is still saved, checked against the version of the
// load that is current, and applied to that one, not to loadID, as one saved
// before it became current is (carryFoundryResolves): documentIDs, read from
// loadID, are another load's, so an alias joins carried, its document owed,
// and the resend job owes the documents of that load whose reviews ask about
// its name (OweFoundryAskingResends); "not in data" joins pending, its
// document owed, where that load holds it extracted, else with no status and
// nothing owed. Applied says carried or pending, or is nil for the last. The
// entity chosen was checked against loadID's records: one the current load
// doesn't hold the engine refuses there (absent), linking nothing, and the
// copy says so. ErrConflict means the ontology has no current load, or the
// version of the one it has doesn't take d; ErrNotFound that the ontology is
// not live.
func (s *mysqlDB) SettleFoundryResolve(ctx context.Context, workspaceID, loadID string, d *model.FoundryReviewDecision, documentIDs []string,
	resend *model.FoundryGraphJob) (queued bool, err error) {
	return s.settleFoundryResolve(ctx, workspaceID, loadID, d, documentIDs, false, resend)
}

// SettleFoundryBoundResolve settles a resolve decision of a name load loadID
// links already (review reasons adjudicated, legal_form, part_name,
// own_name, or a link a person undid), read from a review of that load. It
// is saved as SettleFoundryReviewDecision saves one, its id kept when the
// name was chosen before, and is the next load's while loadID is current:
// the engine would say bound. Another load made current since the review was
// read (both take the ontology's lock) gets it as SettleFoundryResolve gives
// it one, and as the carry would have, had the switch come after the settle
// (carryFoundryResolves): that load read no review for it, so in it the
// decision is one saved while another load was current. queued, d's
// Applied, ErrConflict and ErrNotFound are as SettleFoundryResolve says;
// Applied is nil while loadID is current.
func (s *mysqlDB) SettleFoundryBoundResolve(ctx context.Context, workspaceID, loadID string, d *model.FoundryReviewDecision,
	resend *model.FoundryGraphJob) (queued bool, err error) {
	return s.settleFoundryResolve(ctx, workspaceID, loadID, d, nil, true, resend)
}

// settleFoundryResolve is SettleFoundryResolve, or with bound
// SettleFoundryBoundResolve, which reads no asking documents.
func (s *mysqlDB) settleFoundryResolve(ctx context.Context, workspaceID, loadID string, d *model.FoundryReviewDecision, documentIDs []string,
	bound bool, resend *model.FoundryGraphJob) (queued bool, err error) {
	if err := d.Validate(); err != nil {
		return false, err
	}
	if d.Kind != model.FoundryReviewResolve {
		return false, fmt.Errorf("decision %s is not a resolve decision", d.ID)
	}
	err = s.WithTx(ctx, func(db Store) error {
		tx := db.(*mysqlDB)
		o, err := tx.lockLiveFoundryOntology(ctx, workspaceID, d.OntologyID)
		if err != nil {
			return err
		}
		if o.CurrentLoadID == nil {
			return ErrConflict
		}
		current := *o.CurrentLoadID
		if takes, err := tx.foundryLoadTakes(ctx, current, d); err != nil || !takes {
			return cmp.Or(err, ErrConflict)
		}
		if bound && current == loadID {
			return tx.saveFoundryReviewDecision(ctx, d, false)
		}
		// The live job's row before any decision's, as SettleFoundryReleases
		// takes them.
		live, err := tx.liveFoundryGraphJob(ctx, d.OntologyID)
		if err != nil {
			return err
		}
		// A changed choice is a new decision, under d's id: the engine's word
		// on the earlier one, from a job already sent, then finds no copy to
		// land on. The same choice again keeps its decision, sent again. A
		// bound name chosen again keeps its id, as SettleFoundryReviewDecision
		// keeps it.
		if !bound {
			if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_review_decisions WHERE ontology_id=? AND kind='resolve' AND type=? AND doc_id=?
  AND norm_name=? AND NOT (entity_key <=> ?)`, d.OntologyID, d.Type, *d.DocID, *d.NormName, d.EntityKey); err != nil {
				return err
			}
		}
		if err := tx.saveFoundryReviewDecision(ctx, d, false); err != nil {
			return err
		}
		// Read against the current load, the decision waits there, its own
		// document and those the caller found owed. Read against another, it
		// joins the current load as one saved before that load became current
		// does (carryFoundryResolves): an alias carried, its document owed,
		// and the documents of this load that ask about its name owed by the
		// resend job; "not in data" pending with its document owed where the
		// load holds it extracted, else with no status, nothing owed.
		var status any = string(model.FoundryReleasePending)
		docs := []string{*d.DocID}
		if current == loadID {
			seen := map[string]bool{*d.DocID: true}
			for _, doc := range documentIDs {
				if !seen[doc] {
					seen[doc] = true
					docs = append(docs, doc)
				}
			}
		} else {
			var extracted bool
			if err := tx.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_load_extractions
  WHERE load_id=? AND document_id=? AND status='extracted')`, current, *d.DocID).Scan(&extracted); err != nil {
				return err
			}
			switch {
			case d.EntityKey != nil:
				status = string(model.FoundryReleaseCarried)
			case !extracted:
				status, docs = nil, nil
			}
		}
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_load_decisions
WHERE load_id=? AND kind='resolve' AND type=? AND doc_id=? AND norm_name=? AND id<>?`, current, d.Type, *d.DocID, *d.NormName, d.ID); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_load_decisions (load_id, `+foundryReviewDecisionColumns+`, name_key, applied_status)
SELECT ?, `+foundryReviewDecisionColumns+`, name_key, ? FROM foundry_review_decisions WHERE ontology_id=? AND id=?
ON DUPLICATE KEY UPDATE entity_key=VALUES(entity_key), decided_by_user_id=VALUES(decided_by_user_id), decided_at=VALUES(decided_at),
  name_key=VALUES(name_key), applied_status=VALUES(applied_status), applied_reason=NULL, applied_at=NULL`,
			current, status, d.OntologyID, d.ID); err != nil {
			return err
		}
		if st, ok := status.(string); ok {
			d.Applied = &model.FoundryDecisionApplied{Status: model.FoundryReleaseStatus(st)}
		}
		if err := tx.oweFoundryResends(ctx, current, model.FoundryResendName, docs); err != nil {
			return err
		}
		queued, err = tx.queueFoundryResendBehind(ctx, o, live, resend)
		return err
	})
	return queued, err
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
//
// The engine keeps a decision once, by its id: one it is sent again under
// an id it holds is not news, whatever its value, and sets nothing. So a
// property decided again with another value is a new decision, under d's id,
// and the earlier one goes: the engine withdraws the id it is no longer sent
// and sets the new one. The same value chosen again is the decision that
// stands, with its id and the time it was decided, so a load it is applied
// to in place and a rebuild weigh it from the same time; it is only sent
// again. A load that copied this id with another value, as before a changed
// value had a new id, can't be told the value under it: there too the
// decision is new. The new decision keeps the documents the ones it replaces
// were decided from, so no reader covers the property by fewer documents
// than before.
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
		// The property's decision so far, and whether it stands: it chose
		// this value, and the load holds no copy of its id from another time.
		var prior string
		var stands bool
		switch err := tx.db.QueryRowContext(ctx, `SELECT r.id, COALESCE(r.value = CAST(? AS JSON), FALSE) AND NOT EXISTS (
  SELECT 1 FROM foundry_load_decisions l WHERE l.load_id=? AND l.id=r.id AND l.kind='prop' AND l.decided_at<>r.decided_at)
FROM foundry_review_decisions r WHERE r.ontology_id=? AND r.kind='prop' AND r.type=? AND r.entity_key=? AND r.prop=? FOR UPDATE`,
			string(d.Value), loadID, d.OntologyID, d.Type, d.EntityKey, d.Prop).Scan(&prior, &stands); {
		case err == sql.ErrNoRows:
			var held int
			if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_review_decisions WHERE ontology_id=? AND kind='prop'`,
				d.OntologyID).Scan(&held); err != nil {
				return err
			}
			if held >= model.FoundryExtractKindDecisionsMax {
				return FoundryValueDecisionsFull
			}
		case err != nil:
			return err
		case !stands:
			if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_review_decisions WHERE ontology_id=? AND id=?`, d.OntologyID, prior); err != nil {
				return err
			}
		}
		if err := tx.saveFoundryReviewDecision(ctx, d, stands); err != nil {
			return err
		}
		// What the decisions it replaces were decided from: the one that
		// stood, and the load's copies of the property, one a person took back
		// that the load still holds the value of (model.FoundryValueRemoved)
		// included.
		if _, err := tx.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_review_decision_documents (decision_id, document_id)
SELECT ?, dd.document_id FROM foundry_review_decision_documents dd
WHERE dd.decision_id<>? AND (dd.decision_id=? OR dd.decision_id IN (
  SELECT l.id FROM foundry_load_decisions l WHERE l.load_id=? AND l.kind='prop' AND l.type=? AND l.entity_key=? AND l.prop=?))`,
			d.ID, d.ID, prior, loadID, d.Type, d.EntityKey, d.Prop); err != nil {
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
// keeping that one's id, and reads it back into d. With stands, the choice of
// its identity is d's already and is left as it is, with who made it and
// when: only d's documents are added to it. A release of the statement its
// claim's approval already binds stands too: from contract 4.14.0 the
// approval's time says which of a link's approvals is its decision, on a
// link that holds one value, and dates the decision of a quote that gives no
// date and has no document date, so it stays the time the person first
// approved what they read, in every Extract that sends it, as a load that
// wrote it keeps it. The caller holds the ontology's lock and has checked d.
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

// SettleFoundryReleases settles release decisions, each read from a review of
// load loadID, and applies them to that load at once: under the ontology's
// lock each is saved as SettleFoundryReviewDecision saves one, in place of a
// rejection of its claim, joins the load's own list as pending (in place of
// any earlier decision the list holds for its claim), and its document is
// owed a resend (oweFoundryResends). A claim is its key at its place, or the
// statement read at its place under another key (foundryClaimAt): an
// approval of that statement there, saved under the key the claim had before
// a rebuild moved it, is the decision approved again. It takes the claim's
// key now and keeps its id, its time and who made it, and no rival decision
// is made: the engine would weigh the two apart, and which of them a load
// wrote would depend on how it keyed the sentence.
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
		owed := false
		for _, d := range decisions {
			claim := foundryClaimArgs(d)
			// The first approval of what the person read at this place, under
			// any key, stands: every other approval of the claim goes, and it
			// takes the claim's key, so saving d below finds it and keeps its
			// time (saveFoundryReviewDecision).
			var first string
			switch err := tx.db.QueryRowContext(ctx, `SELECT id FROM foundry_review_decisions t
WHERE t.ontology_id=? AND t.kind='release' AND t.doc_id=? AND t.claim_section=? AND t.claim_start=? AND t.claim_end=? AND t.statement_hash=?
ORDER BY t.decided_at, t.id LIMIT 1 FOR UPDATE`, ontologyID, d.DocID, d.ClaimSpan.Section, d.ClaimSpan.Start, d.ClaimSpan.End, d.StatementHash).Scan(&first); {
			case err == sql.ErrNoRows:
			case err != nil:
				return err
			default:
				if _, err := tx.db.ExecContext(ctx, `DELETE t FROM foundry_review_decisions t
WHERE t.ontology_id=? AND t.kind='release' AND `+foundryClaimAt("t")+` AND t.id<>?`, append(append([]any{ontologyID}, claim...), first)...); err != nil {
					return err
				}
				if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_review_decisions SET claim_key=? WHERE ontology_id=? AND id=?`,
					d.ClaimKey, ontologyID, first); err != nil {
					return err
				}
			}
			if err := tx.saveFoundryReviewDecision(ctx, d, false); err != nil {
				return err
			}
			// A claim is approved or rejected, never both.
			if _, err := tx.db.ExecContext(ctx, `DELETE t FROM foundry_review_decisions t
WHERE t.ontology_id=? AND t.kind='reject' AND `+foundryClaimAt("t"), append([]any{ontologyID}, claim...)...); err != nil {
				return err
			}
			// One release a claim in a load's list: the engine refuses an
			// Extract that lists a release twice, and would weigh two of one
			// claim apart. Another the load may have written is taken back
			// (withdrawFoundryReleases): the engine takes it back first,
			// then writes this one.
			withdrew, err := tx.withdrawFoundryReleases(ctx, loadID, foundryClaimAt("t")+` AND t.id<>?`, append(claim, d.ID))
			if err != nil {
				return err
			}
			owed = owed || withdrew
			// The same decision approved again as the load wrote it (one
			// approval read before its job wrote it, saved after) stays
			// written and owes nothing: sent again it would read not_held,
			// and count the write a second time (MovesForward).
			var writtenAt sql.NullTime
			if err := tx.db.QueryRowContext(ctx, `SELECT MAX(applied_at) FROM foundry_load_decisions
WHERE load_id=? AND id=? AND kind='release' AND applied_status='written' AND statement_hash=? FOR UPDATE`,
				loadID, d.ID, d.StatementHash).Scan(&writtenAt); err != nil {
				return err
			}
			if writtenAt.Valid {
				if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_decisions l JOIN foundry_review_decisions r ON r.ontology_id=? AND r.id=l.id
SET l.claim_key=r.claim_key, l.decided_by_user_id=r.decided_by_user_id, l.decided_at=r.decided_at WHERE l.load_id=? AND l.id=?`,
					ontologyID, loadID, d.ID); err != nil {
					return err
				}
				// Written, so removing it takes it back (foundryMayHaveWritten).
				d.Applied = &model.FoundryDecisionApplied{Status: model.FoundryReleaseWritten, At: &writtenAt.Time, TakesBack: true}
				continue
			}
			if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_load_decisions (load_id, `+foundryReviewDecisionColumns+`, applied_status)
SELECT ?, `+foundryReviewDecisionColumns+`, 'pending' FROM foundry_review_decisions WHERE ontology_id=? AND id=?
ON DUPLICATE KEY UPDATE kind=VALUES(kind), claim_key=VALUES(claim_key), statement_hash=VALUES(statement_hash), decided_by_user_id=VALUES(decided_by_user_id),
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
		// Each approved again as the load wrote it, with nothing taken back:
		// nothing is owed, and the live job is left as it is (a delete is
		// not put off for a resend that won't come).
		if len(documents) == 0 && !owed {
			return nil
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
// claim goes, and with it its copy in every load of the ontology that has
// not written it. The claim is its key at its place, or what the person read
// at its place under another key (foundryClaimAt): the engine writes an
// approval saved under the key the claim had before a rebuild moved it, by
// its place and statement hash, so that one goes too, and so does a
// rejection saved under such a key, which this one replaces. The current load's would be sent by the next resend of
// its document; a load being written copied the approval when it was
// created and would release it at the end of its run; a load that is not
// current would send its copy if it were made current again. One a load
// wrote stays in that load: the fact is there. ErrConflict means loadID is
// no longer the ontology's current load; or that load wrote the claim on an
// approval since the review was read, so it holds no claim there to reject;
// or a resend of that load is running while the claim's approval waits for
// it, so the engine may be writing the claim now: once that job has ended
// the claim is a fact, or its review can be rejected. ErrNotFound means the
// ontology is not live.
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
		claim := foundryClaimArgs(d)
		// What the current load's list says of the claim's approvals (by its
		// key, or of what the person read at its place under another key,
		// which the engine binds to this review: foundryClaimAt): one written,
		// or one waiting for a job with nothing said of it yet while a resend
		// of the load is in flight (foundryInFlight: running, queued again
		// after an attempt that may have written it, or cancelled with its
		// attempt's lease not yet ended; it reads the load id twice).
		var written, sending bool
		if err := tx.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(t.applied_status='written'), 0) > 0,
  COALESCE(SUM(t.applied_status IN ('pending', 'carried') AND t.applied_reason IS NULL), 0) > 0 AND `+
			foundryInFlight("?", model.FoundryGraphJobResend)+` FROM foundry_load_decisions t
WHERE t.load_id=? AND t.kind='release' AND `+foundryClaimAt("t"), append([]any{loadID, loadID, loadID}, claim...)...).Scan(&written, &sending); err != nil {
			return err
		}
		if written || sending {
			return ErrConflict
		}
		// A deleted load's list went with it. A copy a load may have
		// written stays for its carry, which takes it back
		// (carryFoundryReleases), as its decision is gone.
		if _, err := tx.db.ExecContext(ctx, `DELETE t FROM foundry_load_decisions t JOIN foundry_loads l ON l.id=t.load_id
WHERE l.ontology_id=? AND t.kind='release' AND `+foundryClaimAt("t")+` AND NOT `+foundryMayHaveWritten("t"),
			append([]any{d.OntologyID}, claim...)...); err != nil {
			return err
		}
		// Its approvals go, and so does a rejection of what was read here under
		// another key: one rejection a claim, which removing lists it again.
		if _, err := tx.db.ExecContext(ctx, `DELETE t FROM foundry_review_decisions t
WHERE t.ontology_id=? AND `+foundryClaimAt("t")+` AND (t.kind='release' OR t.kind='reject' AND t.claim_key<>?)`,
			append(append([]any{d.OntologyID}, claim...), d.ClaimKey)...); err != nil {
			return err
		}
		return tx.saveFoundryReviewDecision(ctx, d, false)
	})
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
// since takes the claim's approvals from every list (SettleFoundryReject),
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
		}
		if len(page) < 1000 {
			return nil
		}
		after = page[len(page)-1].ID
	}
}
