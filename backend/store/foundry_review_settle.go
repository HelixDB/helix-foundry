package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// FoundryReviewDecidedElsewhere refuses an item of a batch settled with
// expect open: another person's decision decides its card already
// (model.FoundrySettleDecided).
var FoundryReviewDecidedElsewhere = errors.New("another person decided this review")

// SettleFoundryReviews settles items, each read from a review of load loadID
// of ontology ontologyID, in one transaction under the ontology's lock, and
// applies them to the current load as each kind is applied:
//
//   - a resolve (settleResolve) of a name the load asks about joins the
//     current load's list as pending, and its document and the asking ones
//     owe the engine a resend; one of a name the load links already
//     (model.FoundryNameLinked) is the next load's while loadID is current. A
//     resolve read from a load that is no longer current carries to the one
//     that is, as one saved before it became current does.
//   - a value (settleValue) joins loadID's list as pending, in place of its
//     property's other decision, and the load owes its decisions again.
//   - a release (settleRelease) joins loadID's list as pending, in place of
//     its claim's rejection, and its document owes a resend.
//   - a reject (settleReject) is only saved.
//
// The current load's version is read once. Each item is saved under a
// savepoint: an item refused rolls back to it and the rest go on, so refusals
// (aligned with items, nil where saved) say why each was refused: its kind's
// current-load rule or its version (model.FoundrySettleStale, Err
// ErrConflict), the value cap (model.FoundrySettleFull, Err
// FoundryValueDecisionsFull), or, with expectOpen, another person's decision
// of its card (model.FoundrySettleDecided, foundryDecidedElsewhere). Any other
// error aborts the batch and saves nothing: a savepoint can't keep a
// transaction InnoDB rolled back (a deadlock), and one failed statement
// leaves the batch half known. A saved item's D comes back as stored, with
// what the load holds of it (Applied); a refused one's as it was sent.
//
// The open counts are marked dirty once, and one resend is queued (or left to
// the live job: queueFoundryResendBehind) when any item owes one; queued
// reports a job queued here. ErrNotFound means the ontology is not live.
func (s *mysqlDB) SettleFoundryReviews(ctx context.Context, workspaceID, ontologyID, loadID string, items []*model.FoundrySettle,
	expectOpen bool, resend *model.FoundryGraphJob) (refusals []*model.FoundrySettleRefusal, queued bool, err error) {
	for _, it := range items {
		if err := it.Validate(); err != nil {
			return nil, false, err
		}
		if it.D.OntologyID != ontologyID {
			return nil, false, fmt.Errorf("decision %s is not of ontology %s", it.D.ID, ontologyID)
		}
	}
	if len(items) == 0 {
		return nil, false, nil
	}
	err = s.WithTx(ctx, func(db Store) error {
		tx := db.(*mysqlDB)
		refusals = make([]*model.FoundrySettleRefusal, len(items))
		o, err := tx.lockLiveFoundryOntology(ctx, workspaceID, ontologyID)
		if err != nil {
			return err
		}
		// The live job's row before any decision's: a worker recording what
		// became of a decision holds its job's row first, so the two never
		// wait on each other.
		live, err := tx.liveFoundryGraphJob(ctx, ontologyID)
		if err != nil {
			return err
		}
		// The current load's version, read once: only names and values are
		// checked against it.
		var types map[string]model.FoundryDecisionType
		typed := slices.ContainsFunc(items, func(it *model.FoundrySettle) bool { return !it.D.OfClaim() })
		if o.CurrentLoadID != nil && typed {
			if types, err = tx.foundryLoadTypes(ctx, *o.CurrentLoadID); err != nil {
				return err
			}
		}
		saved, owes := false, false
		for i, it := range items {
			refused, owed, err := tx.settleItem(ctx, o, loadID, types, it, expectOpen)
			if err != nil {
				return err
			}
			refusals[i] = refused
			saved, owes = saved || refused == nil, owes || owed
		}
		if saved {
			// A decision may close a card, or open one it replaced.
			if err := tx.markFoundryReviewsDirty(ctx, ontologyID); err != nil {
				return err
			}
		}
		if owes {
			queued, err = tx.queueFoundryResendBehind(ctx, o, live, resend)
		}
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return refusals, queued, nil
}

// settleItem settles one item of SettleFoundryReviews under a savepoint:
// refused, it rolls back to it and says why; owes reports a resend the
// current load owes for it.
func (s *mysqlDB) settleItem(ctx context.Context, o *model.FoundryOntology, loadID string, types map[string]model.FoundryDecisionType,
	it *model.FoundrySettle, expectOpen bool) (refused *model.FoundrySettleRefusal, owes bool, err error) {
	if _, err := s.db.ExecContext(ctx, "SAVEPOINT foundry_settle"); err != nil {
		return nil, false, err
	}
	sent := *it.D
	refuse := func(r *model.FoundrySettleRefusal) (*model.FoundrySettleRefusal, bool, error) {
		if _, err := s.db.ExecContext(ctx, "ROLLBACK TO SAVEPOINT foundry_settle"); err != nil {
			return nil, false, err
		}
		*it.D = sent
		return r, false, nil
	}
	if o.CurrentLoadID == nil {
		return refuse(&model.FoundrySettleRefusal{Reason: model.FoundrySettleStale, Err: ErrConflict})
	}
	if expectOpen {
		decided, err := s.foundryDecidedElsewhere(ctx, it.D)
		if err != nil {
			return nil, false, err
		}
		if decided != nil {
			return refuse(&model.FoundrySettleRefusal{Reason: model.FoundrySettleDecided, Err: FoundryReviewDecidedElsewhere, Decided: decided})
		}
	}
	switch it.D.Kind {
	case model.FoundryReviewResolve:
		owes, err = s.settleResolve(ctx, o, loadID, types, it.D, it.Name)
	case model.FoundryReviewProp:
		owes, err = s.settleValue(ctx, o, loadID, types, it.D)
	case model.FoundryReviewRelease:
		owes, err = s.settleRelease(ctx, o, loadID, it.D)
	case model.FoundryReviewReject:
		err = s.settleReject(ctx, o, loadID, it.D)
	}
	switch {
	case errors.Is(err, ErrConflict):
		return refuse(&model.FoundrySettleRefusal{Reason: model.FoundrySettleStale, Err: ErrConflict})
	case errors.Is(err, FoundryValueDecisionsFull):
		return refuse(&model.FoundrySettleRefusal{Reason: model.FoundrySettleFull, Err: FoundryValueDecisionsFull})
	case err != nil:
		return nil, false, err
	}
	if _, err := s.db.ExecContext(ctx, "RELEASE SAVEPOINT foundry_settle"); err != nil {
		return nil, false, err
	}
	return nil, owes, nil
}

// foundryDecidedElsewhere is the decision of another person than d's that
// decides the card d would decide (foundryDecidesCard, the definition the
// open cards read), the earliest; nil when none does. The card is built from
// d as the index builds it: a held claim's place, key and the statement d
// read; a name's or a value's identity_key. A card closed only through an
// alias (another document's link of the name) is not decided: no card reads
// it so. The caller holds the ontology's lock, so the read sees every
// settle before it.
func (s *mysqlDB) foundryDecidedElsewhere(ctx context.Context, d *model.FoundryReviewDecision) (*model.FoundryReviewDecision, error) {
	var card string
	var args []any
	if d.OfClaim() {
		span := d.ClaimSpan
		card = `SELECT ? AS ontology_id, ? AS doc_id, ? AS claim_section, ? AS claim_start, ? AS claim_end, ? AS claim_key, ? AS statement_hash`
		args = []any{d.OntologyID, d.DocID, span.Section, span.Start, span.End, d.ClaimKey, d.StatementHash}
	} else {
		// The cards' identity_key: kind, type, a name's document and name, a
		// value's entity and property, and no claim.
		var entity any
		if d.Kind == model.FoundryReviewProp {
			entity = d.EntityKey
		}
		card = `SELECT ? AS ontology_id, UNHEX(SHA2(CAST(JSON_ARRAY(?, ?, ?, ?, ?, ?, NULL, NULL, NULL, NULL) AS CHAR), 256)) AS identity_key`
		args = []any{d.OntologyID, string(d.Kind), d.Type, d.DocID, d.NormName, entity, d.Prop}
	}
	columns := "d." + strings.ReplaceAll(foundryReviewDecisionDrawnColumns, ", ", ", d.")
	decided, err := scanFoundryReviewDecisionDrawn(s.db.QueryRowContext(ctx, "SELECT "+columns+" FROM ("+card+`) c
JOIN foundry_review_decisions d ON `+foundryDecidesCard("d", "c", d.OfClaim())+`
WHERE d.decided_by_user_id<>? ORDER BY d.decided_at, d.id LIMIT 1`, append(args, d.DecidedByUserID)...))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return decided, err
}

// foundryLoadTypes reads the types of the version load loadID was made from,
// which a name's or a value's decision is checked against
// (model.FoundryReviewDecision.Fits).
func (s *mysqlDB) foundryLoadTypes(ctx context.Context, loadID string) (map[string]model.FoundryDecisionType, error) {
	var definition []byte
	if err := s.db.QueryRowContext(ctx, `SELECT v.definition FROM foundry_loads l
JOIN foundry_ontology_versions v ON v.id = l.version_id WHERE l.id=?`, loadID).Scan(&definition); err != nil {
		return nil, err
	}
	var def struct {
		OntologyTOML string `json:"ontology_toml"`
	}
	if err := json.Unmarshal(definition, &def); err != nil {
		return nil, err
	}
	return model.FoundryDecisionTypes(def.OntologyTOML)
}

// settleResolve settles a resolve decision read from a review of load
// loadID, and applies it to the current load at once: it is saved, joins the
// load's own list as pending in place of any copy the list holds of its
// identity (its type, document and name: the engine refuses an Extract that
// decides a name of a document twice), with its name key, and the load owes
// the engine a resend of documents (model.FoundryResendName): a changed
// choice is a new decision under d's id, and the same choice again keeps its
// decision; the decision's own, and those name's asking documents
// (model.FoundryNameAsking), as an alias reaches them. The engine settles the
// name in place by replaying each document's saved model work, and its word
// on the decision's own document is recorded on the copy
// (recordFoundryNameOutcomes).
//
// A name loadID links already (model.FoundryNameLinked) is saved for the next
// load while loadID is current, its id kept when the name was chosen before:
// the engine would say bound.
//
// Another load may have become current since the review was read (a load
// job ended, or a person made one current), as both take the ontology's
// lock. The decision is still saved, checked against the version of the
// load that is current, and applied to that one, not to loadID, as one saved
// before it became current is (carryFoundryResolves): the asking documents,
// read from loadID, are another load's, so an alias joins carried, its
// document owed, and the resend job owes the documents of that load whose
// reviews ask about its name (OweFoundryAskingResends); "not in data" joins
// pending, its document owed, where that load holds it extracted, else with
// no status and nothing owed. Applied says carried or pending, or is nil for
// the last. The entity chosen was checked against loadID's records: one the
// current load doesn't hold the engine refuses there (absent), linking
// nothing, and the copy says so. ErrConflict means the current load's
// version doesn't take d.
func (s *mysqlDB) settleResolve(ctx context.Context, o *model.FoundryOntology, loadID string, types map[string]model.FoundryDecisionType,
	d *model.FoundryReviewDecision, name model.FoundryNameLink) (owes bool, err error) {
	current := *o.CurrentLoadID
	if !d.Fits(types) {
		return false, ErrConflict
	}
	asking, isAsking := name.(model.FoundryNameAsking)
	bound := !isAsking
	if bound && current == loadID {
		return false, s.saveFoundryReviewDecision(ctx, d, false)
	}
	// A changed choice is a new decision, under d's id: the engine's word on
	// the earlier one, from a job already sent, then finds no copy to land
	// on. The same choice again keeps its decision, sent again. A bound name
	// chosen again keeps its id, as SettleFoundryReviewDecision keeps it.
	if !bound {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_review_decisions WHERE ontology_id=? AND kind='resolve' AND type=? AND doc_id=?
  AND norm_name=? AND NOT (entity_key <=> ?)`, d.OntologyID, d.Type, *d.DocID, *d.NormName, d.EntityKey); err != nil {
			return false, err
		}
	}
	if err := s.saveFoundryReviewDecision(ctx, d, false); err != nil {
		return false, err
	}
	// Read against the current load, the decision waits there, its own
	// document and the asking ones owed. Read against another, it joins the
	// current load as one saved before that load became current does
	// (carryFoundryResolves): an alias carried, its document owed, and the
	// documents of this load that ask about its name owed by the resend job;
	// "not in data" pending with its document owed where the load holds it
	// extracted, else with no status, nothing owed.
	var status any = string(model.FoundryReleasePending)
	docs := []string{*d.DocID}
	if current == loadID {
		seen := map[string]bool{*d.DocID: true}
		for _, doc := range asking.Documents {
			if !seen[doc] {
				seen[doc] = true
				docs = append(docs, doc)
			}
		}
	} else {
		var extracted bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_load_extractions
  WHERE load_id=? AND document_id=? AND status='extracted')`, current, *d.DocID).Scan(&extracted); err != nil {
			return false, err
		}
		switch {
		case d.EntityKey != nil:
			status = string(model.FoundryReleaseCarried)
		case !extracted:
			status, docs = nil, nil
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_load_decisions
WHERE load_id=? AND kind='resolve' AND type=? AND doc_id=? AND norm_name=? AND id<>?`, current, d.Type, *d.DocID, *d.NormName, d.ID); err != nil {
		return false, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_load_decisions (load_id, `+foundryReviewDecisionColumns+`, name_key, applied_status)
SELECT ?, `+foundryReviewDecisionColumns+`, name_key, ? FROM foundry_review_decisions WHERE ontology_id=? AND id=?
ON DUPLICATE KEY UPDATE entity_key=VALUES(entity_key), decided_by_user_id=VALUES(decided_by_user_id), decided_at=VALUES(decided_at),
  name_key=VALUES(name_key), applied_status=VALUES(applied_status), applied_reason=NULL, applied_at=NULL`,
		current, status, d.OntologyID, d.ID); err != nil {
		return false, err
	}
	if st, ok := status.(string); ok {
		d.Applied = &model.FoundryDecisionApplied{Status: model.FoundryReleaseStatus(st)}
	}
	return true, s.oweFoundryResends(ctx, current, model.FoundryResendName, docs)
}

// settleValue settles a value decision (kind prop), read from a review of
// load loadID, and applies it to that load at once: it is saved, joins the
// load's own list as pending, in place of any decision the list holds for
// its property (the engine refuses an Extract that decides a property
// twice), and the load owes the engine its decisions again
// (model.FoundryResendDecisions): one Extract of no documents carries the
// list, and the engine sets the value.
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
// than before. ErrConflict means loadID is no longer the ontology's current
// load, or its version doesn't take d; FoundryValueDecisionsFull that the
// ontology takes no value decision of another property.
func (s *mysqlDB) settleValue(ctx context.Context, o *model.FoundryOntology, loadID string, types map[string]model.FoundryDecisionType,
	d *model.FoundryReviewDecision) (owes bool, err error) {
	if *o.CurrentLoadID != loadID || !d.Fits(types) {
		return false, ErrConflict
	}
	// The property's decision so far, and whether it stands: it chose this
	// value, and the load holds no copy of its id from another time.
	var prior string
	var stands bool
	switch err := s.db.QueryRowContext(ctx, `SELECT r.id, COALESCE(r.value = CAST(? AS JSON), FALSE) AND NOT EXISTS (
  SELECT 1 FROM foundry_load_decisions l WHERE l.load_id=? AND l.id=r.id AND l.kind='prop' AND l.decided_at<>r.decided_at)
FROM foundry_review_decisions r WHERE r.ontology_id=? AND r.kind='prop' AND r.type=? AND r.entity_key=? AND r.prop=? FOR UPDATE`,
		string(d.Value), loadID, d.OntologyID, d.Type, d.EntityKey, d.Prop).Scan(&prior, &stands); {
	case err == sql.ErrNoRows:
		var held int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_review_decisions WHERE ontology_id=? AND kind='prop'`,
			d.OntologyID).Scan(&held); err != nil {
			return false, err
		}
		if held >= model.FoundryExtractKindDecisionsMax {
			return false, FoundryValueDecisionsFull
		}
	case err != nil:
		return false, err
	case !stands:
		if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_review_decisions WHERE ontology_id=? AND id=?`, d.OntologyID, prior); err != nil {
			return false, err
		}
	}
	if err := s.saveFoundryReviewDecision(ctx, d, stands); err != nil {
		return false, err
	}
	// What the decisions it replaces were decided from: the one that stood,
	// and the load's copies of the property, one a person took back that the
	// load still holds the value of (model.FoundryValueRemoved) included.
	if _, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_review_decision_documents (decision_id, document_id)
SELECT ?, dd.document_id FROM foundry_review_decision_documents dd
WHERE dd.decision_id<>? AND (dd.decision_id=? OR dd.decision_id IN (
  SELECT l.id FROM foundry_load_decisions l WHERE l.load_id=? AND l.kind='prop' AND l.type=? AND l.entity_key=? AND l.prop=?))`,
		d.ID, d.ID, prior, loadID, d.Type, d.EntityKey, d.Prop); err != nil {
		return false, err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_load_decisions
WHERE load_id=? AND kind='prop' AND type=? AND entity_key=? AND prop=? AND id<>?`, loadID, d.Type, d.EntityKey, d.Prop, d.ID); err != nil {
		return false, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_load_decisions (load_id, `+foundryReviewDecisionColumns+`, applied_status)
SELECT ?, `+foundryReviewDecisionColumns+`, 'pending' FROM foundry_review_decisions WHERE ontology_id=? AND id=?
ON DUPLICATE KEY UPDATE value=VALUES(value), decided_by_user_id=VALUES(decided_by_user_id),
 decided_at=VALUES(decided_at), applied_status='pending', applied_reason=NULL, applied_at=NULL`, loadID, d.OntologyID, d.ID); err != nil {
		return false, err
	}
	d.Applied = &model.FoundryDecisionApplied{Status: model.FoundryReleasePending}
	return true, s.oweFoundryResends(ctx, loadID, model.FoundryResendDecisions, []string{""})
}

// settleRelease settles a release decision read from a review of load
// loadID, and applies it to that load at once: it is saved in place of a
// rejection of its claim, joins the load's own list as pending (in place of
// any earlier decision the list holds for its claim), and its document is
// owed a resend (oweFoundryResends). A claim is its key at its place, or the
// statement read at its place under another key (foundryClaimAt): an
// approval of that statement there, saved under the key the claim had before
// a rebuild moved it, is the decision approved again. It takes the claim's
// key now and keeps its id, its time and who made it, and no rival decision
// is made: the engine would weigh the two apart, and which of them a load
// wrote would depend on how it keyed the sentence. The same decision
// approved again as the load wrote it stays written and owes nothing.
// ErrConflict means loadID is no longer the ontology's current load, so its
// reviews are not the ones to settle.
func (s *mysqlDB) settleRelease(ctx context.Context, o *model.FoundryOntology, loadID string, d *model.FoundryReviewDecision) (owes bool, err error) {
	if *o.CurrentLoadID != loadID {
		return false, ErrConflict
	}
	claim := foundryClaimArgs(d)
	// The first approval of what the person read at this place, under any
	// key, stands: every other approval of the claim goes, and it takes the
	// claim's key, so saving d below finds it and keeps its time
	// (saveFoundryReviewDecision).
	var first string
	switch err := s.db.QueryRowContext(ctx, `SELECT id FROM foundry_review_decisions t
WHERE t.ontology_id=? AND t.kind='release' AND t.doc_id=? AND t.claim_section=? AND t.claim_start=? AND t.claim_end=? AND t.statement_hash=?
ORDER BY t.decided_at, t.id LIMIT 1 FOR UPDATE`, d.OntologyID, d.DocID, d.ClaimSpan.Section, d.ClaimSpan.Start, d.ClaimSpan.End, d.StatementHash).Scan(&first); {
	case err == sql.ErrNoRows:
	case err != nil:
		return false, err
	default:
		if _, err := s.db.ExecContext(ctx, `DELETE t FROM foundry_review_decisions t
WHERE t.ontology_id=? AND t.kind='release' AND `+foundryClaimAt("t")+` AND t.id<>?`, append(append([]any{d.OntologyID}, claim...), first)...); err != nil {
			return false, err
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE foundry_review_decisions SET claim_key=? WHERE ontology_id=? AND id=?`,
			d.ClaimKey, d.OntologyID, first); err != nil {
			return false, err
		}
	}
	if err := s.saveFoundryReviewDecision(ctx, d, false); err != nil {
		return false, err
	}
	// A claim is approved or rejected, never both.
	if _, err := s.db.ExecContext(ctx, `DELETE t FROM foundry_review_decisions t
WHERE t.ontology_id=? AND t.kind='reject' AND `+foundryClaimAt("t"), append([]any{d.OntologyID}, claim...)...); err != nil {
		return false, err
	}
	// One release a claim in a load's list: the engine refuses an Extract
	// that lists a release twice, and would weigh two of one claim apart.
	// Another the load may have written is taken back
	// (withdrawFoundryReleases): the engine takes it back first, then writes
	// this one.
	withdrew, err := s.withdrawFoundryReleases(ctx, loadID, foundryClaimAt("t")+` AND t.id<>?`, append(claim, d.ID))
	if err != nil {
		return false, err
	}
	// The same decision approved again as the load wrote it (one approval
	// read before its job wrote it, saved after) stays written and owes
	// nothing: sent again it would read not_held, and count the write a
	// second time (MovesForward).
	var writtenAt sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(applied_at) FROM foundry_load_decisions
WHERE load_id=? AND id=? AND kind='release' AND applied_status='written' AND statement_hash=? FOR UPDATE`,
		loadID, d.ID, d.StatementHash).Scan(&writtenAt); err != nil {
		return false, err
	}
	if writtenAt.Valid {
		if _, err := s.db.ExecContext(ctx, `UPDATE foundry_load_decisions l JOIN foundry_review_decisions r ON r.ontology_id=? AND r.id=l.id
SET l.claim_key=r.claim_key, l.decided_by_user_id=r.decided_by_user_id, l.decided_at=r.decided_at WHERE l.load_id=? AND l.id=?`,
			d.OntologyID, loadID, d.ID); err != nil {
			return false, err
		}
		// Written, so removing it takes it back (foundryMayHaveWritten).
		d.Applied = &model.FoundryDecisionApplied{Status: model.FoundryReleaseWritten, At: &writtenAt.Time, TakesBack: true}
		return withdrew, nil
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_load_decisions (load_id, `+foundryReviewDecisionColumns+`, applied_status)
SELECT ?, `+foundryReviewDecisionColumns+`, 'pending' FROM foundry_review_decisions WHERE ontology_id=? AND id=?
ON DUPLICATE KEY UPDATE kind=VALUES(kind), claim_key=VALUES(claim_key), statement_hash=VALUES(statement_hash), decided_by_user_id=VALUES(decided_by_user_id),
 decided_at=VALUES(decided_at), applied_status='pending', applied_reason=NULL, applied_at=NULL`, loadID, d.OntologyID, d.ID); err != nil {
		return false, err
	}
	d.Applied = &model.FoundryDecisionApplied{Status: model.FoundryReleasePending}
	return true, s.oweFoundryResends(ctx, loadID, model.FoundryResendApproval, []string{*d.DocID})
}

// settleReject settles a reject decision, read from a review of load loadID:
// a person's word that the claim the review holds back is wrong. It is saved
// and goes nowhere else: no load's list takes it, nothing is sent to the
// engine and no job is queued, so the graph is as it was. Readers leave the
// claim's card out of this load and every later one while the decision
// stands (ListFoundryReviewDecisions), and removing it brings the card back.
//
// A claim is approved or rejected, never both: an approval saved for the
// claim goes, and with it its copy in every load of the ontology that has
// not written it. The claim is its key at its place, or what the person read
// at its place under another key (foundryClaimAt): the engine writes an
// approval saved under the key the claim had before a rebuild moved it, by
// its place and statement hash, so that one goes too, and so does a
// rejection saved under such a key, which this one replaces. The current
// load's would be sent by the next resend of its document; a load being
// written copied the approval when it was created and would release it at
// the end of its run; a load that is not current would send its copy if it
// were made current again. One a load wrote stays in that load: the fact is
// there. ErrConflict means loadID is no longer the ontology's current load;
// or that load wrote the claim on an approval since the review was read, so
// it holds no claim there to reject; or a resend of that load is running
// while the claim's approval waits for it, so the engine may be writing the
// claim now: once that job has ended the claim is a fact, or its review can
// be rejected.
func (s *mysqlDB) settleReject(ctx context.Context, o *model.FoundryOntology, loadID string, d *model.FoundryReviewDecision) error {
	if *o.CurrentLoadID != loadID {
		return ErrConflict
	}
	claim := foundryClaimArgs(d)
	// What the current load's list says of the claim's approvals (by its
	// key, or of what the person read at its place under another key, which
	// the engine binds to this review: foundryClaimAt): one written, or one
	// waiting for a job with nothing said of it yet while a resend of the
	// load is in flight (foundryInFlight: running, queued again after an
	// attempt that may have written it, or cancelled with its attempt's
	// lease not yet ended; it reads the load id twice).
	var written, sending bool
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(t.applied_status='written'), 0) > 0,
  COALESCE(SUM(t.applied_status IN ('pending', 'carried') AND t.applied_reason IS NULL), 0) > 0 AND `+
		foundryInFlight("?", model.FoundryGraphJobResend)+` FROM foundry_load_decisions t
WHERE t.load_id=? AND t.kind='release' AND `+foundryClaimAt("t"), append([]any{loadID, loadID, loadID}, claim...)...).Scan(&written, &sending); err != nil {
		return err
	}
	if written || sending {
		return ErrConflict
	}
	// A deleted load's list went with it. A copy a load may have written
	// stays for its carry, which takes it back (carryFoundryReleases), as its
	// decision is gone.
	if _, err := s.db.ExecContext(ctx, `DELETE t FROM foundry_load_decisions t JOIN foundry_loads l ON l.id=t.load_id
WHERE l.ontology_id=? AND t.kind='release' AND `+foundryClaimAt("t")+` AND NOT `+foundryMayHaveWritten("t"),
		append([]any{d.OntologyID}, claim...)...); err != nil {
		return err
	}
	// Its approvals go, and so does a rejection of what was read here under
	// another key: one rejection a claim, which removing lists it again.
	if _, err := s.db.ExecContext(ctx, `DELETE t FROM foundry_review_decisions t
WHERE t.ontology_id=? AND `+foundryClaimAt("t")+` AND (t.kind='release' OR t.kind='reject' AND t.claim_key<>?)`,
		append(append([]any{d.OntologyID}, claim...), d.ClaimKey)...); err != nil {
		return err
	}
	return s.saveFoundryReviewDecision(ctx, d, false)
}
