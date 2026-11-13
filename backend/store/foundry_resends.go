package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// A load owes the engine a document again (foundry_load_resends) when
// something of the document that the load holds changed after its commit: a
// person's approval of a claim its review holds back, which joins the load's
// decisions as it is settled (SettleFoundryReleases). A resend job sends each
// owed document with its saved extraction, and the engine replays it and
// writes what the approval releases; the document never leaves the load. One
// job at a time writes an ontology's database, so an approval that lands
// while a job is live stays owed, and that job's end queues the resend
// (queueAfterFoundryGraphJob).
//
// A load owes the engine its own decisions again (model.FoundryResendDecisions,
// a row that names no document) when its list of value decisions changed: a
// person chose a value of a property (SettleFoundryValue) or took a choice
// back (DeleteFoundryReviewDecision). Every Extract of a load carries all of
// its value decisions, and the engine sets each and withdraws those it is no
// longer sent, so the same job answers it, with an Extract of no documents
// when no document is owed. The engine reports nothing of a value decision:
// one is applied (model.FoundryValueApplied) once an Extract that carried the
// list has ended, and that is all a load says of it. A value decision is a
// statement of its property from when it was decided, weighed by date and
// never by the order it arrived in, so a load it was applied to in place
// holds what a rebuild with the same decisions holds. The engine keeps a
// decision once, by its id, so a property decided again with another value
// is a new decision (SettleFoundryValue). One taken back stays in the list,
// marked removed and sent in no Extract, until the request ends: the load
// holds its value until then, and the access rules read the list.
//
// From contract 4.14.0 a load brought up to date this way holds what a
// rebuild given the same decisions makes, on a link that holds one value
// too: of the link's approvals the last decided (decided_at, the same in
// every Extract that sends it) is a decision of the link, which outranks
// commit order. It takes effect from its quote's date, the one the quote
// gives or else its document's, and from decided_at's day only for a quote
// with neither; a quote dated after the load recorded it beats nothing until
// that date. It beats undated statements and those dated up to then,
// whichever commits first; a statement dated later, an earlier approval's
// quote included, still holds the link. Each load still says what it made of
// an approval (applied_status).
//
// From contract 4.16.0 an approval a load may have written is taken back from
// it when its decision goes (a person took it back, approved the claim anew
// under another id, or rejected it): the load's copy becomes a withdraw
// (model.FoundryReviewWithdraw, withdrawFoundryReleases), owed with its
// document as an approval is, and the same resend sends it. The engine takes
// back what the release wrote, before any document is read and with no model
// call, so the load holds what one that never had the approval holds, and
// says what it did (withdrawn, not_written, or refused). A copy the load
// never wrote is only deleted, as before: there is nothing to take back.

// oweFoundryResends owes each of documentIDs a resend into loadID for reason,
// or owes it again: a request a running job hasn't read stays owed when that
// job ends the one it did read. The caller holds the ontology's lock.
func (s *mysqlDB) oweFoundryResends(ctx context.Context, loadID string, reason model.FoundryResendReason, documentIDs []string) error {
	if len(documentIDs) == 0 {
		return nil
	}
	args := make([]any, 0, 3*len(documentIDs))
	for _, doc := range documentIDs {
		args = append(args, loadID, doc, string(reason))
	}
	rows := strings.TrimSuffix(strings.Repeat("(?, ?, ?), ", len(documentIDs)), ", ")
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_load_resends (load_id, document_id, reason) VALUES `+rows+`
ON DUPLICATE KEY UPDATE status='owed', requests=requests+1, requested_at=NOW(6), ended_at=NULL, job_id=NULL`, args...)
	return err
}

// foundryMayHaveWritten is the SQL condition that a load's copy (alias t) of
// a release may be written in that load: the engine said so; the job that
// sent it failed before its word was recorded (unconfirmed); the load copied
// it when it was created and holds its document extracted, with no job's
// word on it (an engine before contract 4.12.0 said nothing of releases);
// or a job of the load is in flight (foundryInFlight), whatever the copy's
// status: it may be sending the copy now, a copy made with the load and not
// yet sent included, or have sent it in an attempt whose word was never
// recorded. Any other copy is written nowhere: no job sent it, or the engine
// refused it or found no claim for it. It is never NULL, so NOT of it holds
// of every copy it doesn't: a copy no job reported on has no status.
func foundryMayHaveWritten(t string) string {
	return `COALESCE(` + t + `.applied_status='written'
  OR ` + t + `.applied_status IN ('pending', 'carried') AND ` + t + `.applied_reason IS NOT NULL
  OR ` + t + `.applied_status IS NULL AND EXISTS (SELECT 1 FROM foundry_load_extractions e
    WHERE e.load_id=` + t + `.load_id AND e.document_id=` + t + `.doc_id AND e.status='extracted')
  OR ` + foundryInFlight(t+".load_id") + `, FALSE)`
}

// foundryInFlight is the SQL condition that a job of the load `load` (a
// column or a placeholder), of one of kinds or of any kind, is in flight: the
// ontology's live job, running, or queued again after an attempt started (a
// retry, or one requeued for documents that came during it). An attempt may
// have sent the engine the load's decisions, and the engine written some,
// with no word of it recorded: the next attempt sends what the list then
// holds, and absence never takes anything back.
func foundryInFlight(load string, kinds ...model.FoundryGraphJobKind) string {
	cond := `EXISTS (SELECT 1 FROM foundry_load_jobs j WHERE j.load_id=` + load + ` AND j.active_key=1
    AND (j.status='running' OR j.status='queued' AND j.started_at IS NOT NULL)`
	if len(kinds) > 0 {
		names := make([]string, 0, len(kinds))
		for _, k := range kinds {
			names = append(names, "'"+string(k)+"'")
		}
		cond += ` AND j.kind IN (` + strings.Join(names, ", ") + `)`
	}
	return cond + `)`
}

// withdrawFoundryReleases takes the releases of load loadID that where (SQL
// over alias t, with args) names out of its list: each the load may have
// written (foundryMayHaveWritten) becomes a withdraw of
// it, waiting (pending), and its document is owed a resend
// (model.FoundryResendApproval), which sends it to the engine; the others
// are deleted, as nothing of them is in the load. A withdraw keeps the
// release's id, time, claim and statement, as the engine takes back what
// that decision wrote, so the list never holds both for one id. It keeps
// whether the load had recorded the release written (was_written), which
// tells what the engine's later word, or its silence, means
// (model.FoundryDecisionApplied.After, EndFoundryLoadResend). withdrew
// reports a withdraw made here. Its caller queues the resend
// (queueFoundryResendBehind), or is the carry that comes before one. The
// caller holds the ontology's lock.
func (s *mysqlDB) withdrawFoundryReleases(ctx context.Context, loadID, where string, args []any) (withdrew bool, err error) {
	scope := append([]any{loadID}, args...)
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT t.doc_id FROM foundry_load_decisions t
WHERE t.load_id=? AND t.kind='release' AND (`+where+`) AND `+foundryMayHaveWritten("t")+` ORDER BY t.doc_id`, scope...)
	if err != nil {
		return false, err
	}
	var documents []string
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			_ = rows.Close()
			return false, err
		}
		documents = append(documents, doc)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(documents) > 0 {
		// was_written first: MySQL assigns left to right, and it reads the
		// release's status before the withdraw's replaces it.
		if _, err := s.db.ExecContext(ctx, `UPDATE foundry_load_decisions t
SET t.was_written=(t.applied_status <=> 'written'), t.kind=?, t.applied_status=?, t.applied_reason=NULL, t.applied_at=NULL
WHERE t.load_id=? AND t.kind='release' AND (`+where+`) AND `+foundryMayHaveWritten("t"),
			append([]any{string(model.FoundryReviewWithdraw), string(model.FoundryReleasePending)}, scope...)...); err != nil {
			return false, err
		}
	}
	if _, err := s.db.ExecContext(ctx, `DELETE t FROM foundry_load_decisions t WHERE t.load_id=? AND t.kind='release' AND (`+where+`)`, scope...); err != nil {
		return false, err
	}
	return len(documents) > 0, s.oweFoundryResends(ctx, loadID, model.FoundryResendApproval, documents)
}

// carryFoundryReleases brings the ontology's current load the release
// decisions saved for claims its list has no word on: those settled while
// another load was current, as during this one's writing, which its creation
// could not copy. Each whose document the load holds an extraction of joins
// the list as carried, and that document is owed a resend. One whose
// document the load has not extracted (it is a duplicate there, out of the
// load's scope, failed, deleted, or yet to be read) stays out of the list:
// nothing could say what became of it, and a decision in the list that waits
// is one a reader waits for. It is carried by the first job end that finds
// its document extracted, as after the extract that reads it. A claim the
// list already has a decision for, by its key or by the statement at its
// place (as the engine binds a release), is left as it is, whatever its id;
// so is a decision the list holds a copy of under the key its claim had
// before it was approved again (SettleFoundryReleases).
//
// A release of a claim the ontology holds a rejection of (foundryClaimAt) is
// never carried, and a copy of one the list has not written goes: the load
// list never sends it (ListFoundryLoadDecisions), so it would wait for good,
// owing its document a resend at every job's end. Only rows saved before a
// rejection took a claim's approvals under every key, or by an older binary,
// are such.
//
// First the list gives up the releases whose decision is gone, or whose
// claim the ontology holds a rejection of (withdrawFoundryReleases): one the
// load may have written becomes a withdraw, which the resend sends, and the
// rest are deleted. Removing or replacing a decision takes it from the load
// that is current then (DeleteFoundryReviewDecision, SettleFoundryReleases),
// so a copy is left behind in a load that was not: one being written then,
// which copied the approval as it was created and may have written it, or
// one made current again later. So is a copy an older binary's removal
// left, written or not: from contract 4.16.0 a written one is taken back
// here, as a removal now takes it back. A resend sends every release its
// load's list holds of a document, and every resend job is queued through
// here (queueFoundryResend), so none sends an approval a person took back.
// A withdraw that waits with nothing owed for it (its job failed, and the
// end of the next job of the ontology cleared that: FinishFoundryGraphJob)
// is owed again here, so none waits for good. The caller holds the
// ontology's lock, and no job is live.
func (s *mysqlDB) carryFoundryReleases(ctx context.Context, o *model.FoundryOntology) error {
	if o.CurrentLoadID == nil {
		return nil
	}
	loadID := *o.CurrentLoadID
	if _, err := s.withdrawFoundryReleases(ctx, loadID, `t.id NOT IN (SELECT id FROM foundry_review_decisions WHERE ontology_id=?) OR `+
		foundryRejected("t", "?"), []any{o.ID, o.ID}); err != nil {
		return err
	}
	// Two anti-joins, each on an index (the list's primary key, and its
	// documents' index), where one with an OR would scan the list for each
	// decision.
	columns := "r." + strings.ReplaceAll(foundryReviewDecisionColumns, ", ", ", r.")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_load_decisions (load_id, `+foundryReviewDecisionColumns+`, applied_status)
SELECT ?, `+columns+`, 'carried' FROM foundry_review_decisions r
JOIN foundry_load_extractions e ON e.load_id=? AND e.document_id=r.doc_id AND e.status='extracted'
WHERE r.ontology_id=? AND r.kind='release' AND NOT `+foundryRejected("r", "r.ontology_id")+`
  AND NOT EXISTS (SELECT 1 FROM foundry_load_decisions l WHERE l.load_id=? AND l.id=r.id)
  AND NOT EXISTS (SELECT 1 FROM foundry_load_decisions l WHERE l.load_id=? AND l.kind='release' AND l.doc_id=r.doc_id
    AND l.claim_section=r.claim_section AND l.claim_start=r.claim_start AND l.claim_end=r.claim_end
    AND (l.claim_key=r.claim_key OR l.statement_hash=r.statement_hash))`, loadID, loadID, o.ID, loadID, loadID); err != nil {
		return err
	}
	// A document whose row left extracted since (a failure recorded over it)
	// is owed nothing until it is extracted again, and a decision whose job
	// failed (applied_reason) is not sent again unasked.
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_load_resends (load_id, document_id, reason)
SELECT DISTINCT l.load_id, l.doc_id, ? FROM foundry_load_decisions l
JOIN foundry_load_extractions e ON e.load_id=l.load_id AND e.document_id=l.doc_id AND e.status='extracted'
WHERE l.load_id=? AND l.kind='release' AND l.applied_status='carried' AND l.applied_reason IS NULL
  AND NOT EXISTS (SELECT 1 FROM foundry_load_resends w WHERE w.load_id=l.load_id AND w.document_id=l.doc_id AND w.reason=? AND w.status='owed')
ON DUPLICATE KEY UPDATE status='owed', requests=requests+1, requested_at=NOW(6), ended_at=NULL, job_id=NULL`,
		string(model.FoundryResendApproval), loadID, string(model.FoundryResendApproval))
	if err != nil {
		return err
	}
	// A withdraw is owed whether or not its document is extracted: the
	// resend refuses one it can't send (document_unread, document_gone), so
	// it never waits for good.
	_, err = s.db.ExecContext(ctx, `INSERT INTO foundry_load_resends (load_id, document_id, reason)
SELECT DISTINCT l.load_id, l.doc_id, ? FROM foundry_load_decisions l
WHERE l.load_id=? AND l.kind='withdraw' AND l.applied_status='pending' AND l.applied_reason IS NULL
  AND NOT EXISTS (SELECT 1 FROM foundry_load_resends w WHERE w.load_id=l.load_id AND w.document_id=l.doc_id AND w.reason=? AND w.status='owed')
ON DUPLICATE KEY UPDATE status='owed', requests=requests+1, requested_at=NOW(6), ended_at=NULL, job_id=NULL`,
		string(model.FoundryResendApproval), loadID, string(model.FoundryResendApproval))
	return err
}

// carryFoundryValues brings the ontology's current load the value decisions
// saved while another load was current, as during this one's writing, which
// its creation could not copy: each the list lacks joins it as carried, and
// one the list holds of a property since decided anew, under another id,
// gives way to what was chosen last, so the list never decides a property
// twice, which the engine refuses; so does one a person took back that the
// load was not yet told of (model.FoundryValueRemoved). The decision that
// takes a copy's place keeps the documents that copy was decided from, so
// the property is withheld from no fewer readers. Then the load owes the
// engine its decisions again (model.FoundryResendDecisions). A value decision
// names no document, so there is none to wait for: the resend that follows
// carries every one, and none is left waiting.
//
// A copy under the id of the decision that stands, from another time, is
// left as it is: before a changed value had a new id the load read another
// value under that id, and the engine, which keeps a decision once by its
// id, would set nothing if it were sent the new one. That load says nothing
// of the decision (ListFoundryDecisionsApplied) until it is chosen again
// there, as a new decision.
//
// A value that waits, or one taken back, with nothing owed for it is owed
// here: an older binary ends the load's own request unsent (it reads it as a
// document that is gone), and a job that was requeued in place of its end
// queued nothing. One whose job failed is not owed again until the next job
// ends (FinishFoundryGraphJob). The caller holds the ontology's lock.
func (s *mysqlDB) carryFoundryValues(ctx context.Context, o *model.FoundryOntology) error {
	if o.CurrentLoadID == nil {
		return nil
	}
	loadID := *o.CurrentLoadID
	if _, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_review_decision_documents (decision_id, document_id)
SELECT r.id, dd.document_id FROM foundry_load_decisions l
JOIN foundry_review_decisions r ON r.ontology_id=? AND r.kind='prop' AND r.type=l.type AND r.entity_key=l.entity_key AND r.prop=l.prop AND r.id<>l.id
JOIN foundry_review_decision_documents dd ON dd.decision_id=l.id
WHERE l.load_id=? AND l.kind='prop'`, o.ID, loadID); err != nil {
		return err
	}
	replaced, err := s.db.ExecContext(ctx, `DELETE l FROM foundry_load_decisions l
JOIN foundry_review_decisions r ON r.ontology_id=? AND r.kind='prop' AND r.type=l.type AND r.entity_key=l.entity_key AND r.prop=l.prop
WHERE l.load_id=? AND l.kind='prop' AND r.id<>l.id`, o.ID, loadID)
	if err != nil {
		return err
	}
	columns := "r." + strings.ReplaceAll(foundryReviewDecisionColumns, ", ", ", r.")
	carried, err := s.db.ExecContext(ctx, `INSERT INTO foundry_load_decisions (load_id, `+foundryReviewDecisionColumns+`, applied_status)
SELECT ?, `+columns+`, 'carried' FROM foundry_review_decisions r
LEFT JOIN foundry_load_decisions l ON l.load_id=? AND l.id=r.id
WHERE r.ontology_id=? AND r.kind='prop' AND l.id IS NULL`, loadID, loadID, o.ID)
	if err != nil {
		return err
	}
	var changed int64
	for _, res := range []sql.Result{replaced, carried} {
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		changed += n
	}
	if changed == 0 {
		var unowed bool
		if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_load_decisions
  WHERE load_id=? AND kind='prop' AND applied_status IN ('pending', 'carried', 'removed') AND applied_reason IS NULL)
AND NOT EXISTS (SELECT 1 FROM foundry_load_resends WHERE load_id=? AND document_id='' AND reason=? AND status='owed')`,
			loadID, loadID, string(model.FoundryResendDecisions)).Scan(&unowed); err != nil || !unowed {
			return err
		}
	}
	return s.oweFoundryResends(ctx, loadID, model.FoundryResendDecisions, []string{""})
}

// queueFoundryResend queues job, a resend into the ontology's current load,
// when that load owes the engine one, first carrying over the approvals and
// the value decisions the load hasn't read (carryFoundryReleases,
// carryFoundryValues); queued reports whether it did. The job is due after
// wait, which a request that comes meanwhile puts off again
// (queueFoundryResendBehind). The caller holds the ontology's lock and no job
// is live.
func (s *mysqlDB) queueFoundryResend(ctx context.Context, o *model.FoundryOntology, job *model.FoundryGraphJob, wait time.Duration) (queued bool, err error) {
	if o.CurrentLoadID == nil {
		return false, nil
	}
	if err := s.carryFoundryReleases(ctx, o); err != nil {
		return false, err
	}
	if err := s.carryFoundryValues(ctx, o); err != nil {
		return false, err
	}
	loadID := *o.CurrentLoadID
	var owed bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_load_resends WHERE load_id=? AND status='owed')`, loadID).
		Scan(&owed); err != nil || !owed {
		return false, err
	}
	if job == nil || job.ID == "" {
		return false, fmt.Errorf("a load that owes a resend needs the resend job's id")
	}
	var versionID string
	if err := s.db.QueryRowContext(ctx, `SELECT version_id FROM foundry_loads WHERE id=?`, loadID).Scan(&versionID); err != nil {
		return false, err
	}
	job.WorkspaceID, job.OntologyID, job.VersionID, job.LoadID = o.WorkspaceID, o.ID, versionID, loadID
	job.Kind, job.Phase = model.FoundryGraphJobResend, model.FoundryGraphJobDocuments
	if err := s.insertFoundryGraphJob(ctx, job); err != nil {
		return false, err
	}
	if wait <= 0 {
		return true, nil
	}
	_, err = s.db.ExecContext(ctx, `UPDATE foundry_load_jobs SET next_attempt_at=NOW(6) + INTERVAL ? MICROSECOND WHERE id=?`,
		wait.Microseconds(), job.ID)
	return true, err
}

// queueFoundryResendBehind queues resend for what the ontology's current
// load was just owed, where no job is in its way. With no job live it is
// queued as a resend job of the load, due after model.FoundryResendSettle; a
// queued resend of the load is put off by as much instead, so requests that
// come together go in one job; a live delete gives way, as to a new load,
// and resumes after; any other live job's end queues the resend
// (queueAfterFoundryGraphJob). live is the ontology's live job, read before
// any decision's row was written: a worker recording what became of a
// decision holds its job's row first, so the two never wait on each other.
// queued reports a job queued here. The caller holds the ontology's lock.
func (s *mysqlDB) queueFoundryResendBehind(ctx context.Context, o *model.FoundryOntology, live, resend *model.FoundryGraphJob) (queued bool, err error) {
	switch {
	case live == nil:
	case live.Kind == model.FoundryGraphJobDelete:
		if err := s.cancelLiveFoundryGraphJob(ctx, o.ID, "a decision went first; the delete resumes after it"); err != nil {
			return false, err
		}
	case live.Kind == model.FoundryGraphJobResend && o.CurrentLoadID != nil && live.LoadID == *o.CurrentLoadID &&
		live.Status == model.FoundryGraphJobQueued:
		_, err := s.db.ExecContext(ctx, `UPDATE foundry_load_jobs
SET next_attempt_at=GREATEST(next_attempt_at, NOW(6) + INTERVAL ? MICROSECOND) WHERE id=? AND status='queued'`,
			model.FoundryResendSettle.Microseconds(), live.ID)
		return false, err
	default:
		return false, nil
	}
	return s.queueFoundryResend(ctx, o, resend, model.FoundryResendSettle)
}

// FoundryLoadDecisionsOwed returns the request a load owes the engine its
// value decisions for (model.FoundryResendDecisions), or nil when it owes
// none. A job reads it before it sends an Extract, so the request it ends is
// one every decision it sent was part of (EndFoundryLoadResend).
func (s *mysqlDB) FoundryLoadDecisionsOwed(ctx context.Context, loadID string) (*model.FoundryLoadResend, error) {
	r := &model.FoundryLoadResend{LoadID: loadID, Reason: model.FoundryResendDecisions}
	err := s.db.QueryRowContext(ctx, `SELECT requests FROM foundry_load_resends
WHERE load_id=? AND document_id='' AND reason=? AND status='owed'`, loadID, string(model.FoundryResendDecisions)).Scan(&r.Requests)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// ListFoundryLoadResends returns the documents a load owes a resend, in the
// order a load sends its documents (ListPendingFoundryDocuments), so a
// resend's releases go in the order a fresh load's would; those that can't be
// sent (FoundryLoadResend.Document is nil) come last, each saying whether the
// load still holds its document (FoundryLoadResend.Unsendable).
func (s *mysqlDB) ListFoundryLoadResends(ctx context.Context, loadID string) ([]*model.FoundryLoadResend, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT document_id, reason, requests FROM foundry_load_resends
WHERE load_id=? AND status='owed' AND reason<>? ORDER BY document_id, reason`, loadID, string(model.FoundryResendDecisions))
	if err != nil {
		return nil, err
	}
	owed := map[string][]*model.FoundryLoadResend{}
	total := 0
	for rows.Next() {
		r := &model.FoundryLoadResend{LoadID: loadID}
		var reason string
		if err := rows.Scan(&r.DocumentID, &reason, &r.Requests); err != nil {
			_ = rows.Close()
			return nil, err
		}
		r.Reason = model.FoundryResendReason(reason)
		owed[r.DocumentID] = append(owed[r.DocumentID], r)
		total++
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || total == 0 {
		return nil, err
	}
	// The documents the load can send again: uploaded, of its membership, and
	// extracted into it.
	docRows, err := s.db.QueryContext(ctx, "SELECT "+strings.Replace(foundryDocumentColumns, "d.set_id", "m.set_id", 1)+
		foundryLoadDocuments+`
 JOIN foundry_load_extractions e ON e.load_id=m.load_id AND e.document_id=d.id AND e.status='extracted'
 WHERE d.status='uploaded' AND EXISTS (SELECT 1 FROM foundry_load_resends w WHERE w.load_id=m.load_id AND w.document_id=d.id AND w.status='owed')
 ORDER BY d.external_id IS NOT NULL, d.source_at DESC, d.created_at, d.id`, loadID)
	if err != nil {
		return nil, err
	}
	docs, err := scanAll(docRows, scanFoundryDocument)
	if err != nil {
		return nil, err
	}
	out := make([]*model.FoundryLoadResend, 0, total)
	for _, doc := range docs {
		for _, r := range owed[doc.ID] {
			r.Document = doc
			out = append(out, r)
		}
		delete(owed, doc.ID)
	}
	var unsendable []*model.FoundryLoadResend
	for _, rs := range owed {
		unsendable = append(unsendable, rs...)
	}
	if len(unsendable) == 0 {
		return out, nil
	}
	// Of those, the load still holds the uploaded ones of its membership:
	// what they lack is an extraction to replay.
	ids := make([]any, 0, len(owed))
	for id := range owed {
		ids = append(ids, id)
	}
	heldRows, err := s.db.QueryContext(ctx, `SELECT d.id FROM foundry_documents d
JOIN foundry_load_documents m ON m.document_id=d.id AND m.load_id=?
WHERE d.status='uploaded' AND d.id IN (`+idPlaceholders(len(ids))+`)`, append([]any{loadID}, ids...)...)
	if err != nil {
		return nil, err
	}
	held := map[string]bool{}
	for heldRows.Next() {
		var id string
		if err := heldRows.Scan(&id); err != nil {
			_ = heldRows.Close()
			return nil, err
		}
		held[id] = true
	}
	_ = heldRows.Close()
	if err := heldRows.Err(); err != nil {
		return nil, err
	}
	for _, r := range unsendable {
		r.Unsendable = model.FoundryReleaseDocumentGone
		if held[r.DocumentID] {
			r.Unsendable = model.FoundryReleaseDocumentUnread
		}
	}
	// Map order is random: by document, so two attempts end them alike.
	slices.SortFunc(unsendable, func(a, b *model.FoundryLoadResend) int {
		return cmp.Or(cmp.Compare(a.DocumentID, b.DocumentID), cmp.Compare(a.Reason, b.Reason))
	})
	return append(out, unsendable...), nil
}

// EndFoundryLoadResend ends a document's resend into the job's load, or the
// load's own (model.FoundryResendDecisions, no document), while workerID
// holds the job's lease; ErrNotFound means it lost it, and nothing was
// written. The row ends only if it is still the request the job read
// (end.Requests): one owed again meanwhile stays owed, and the job's end
// queues another resend. Ending the load's own request marks every value
// decision of the load that waited applied (model.FoundryValueApplied), one
// left unconfirmed by a failed job included: no value was decided or removed
// since the job read the request, so the Extract that just ended carried
// each of them. The ones taken back (model.FoundryValueRemoved) leave the
// list then: that Extract carried none of them, so the engine has withdrawn
// each, and the load no longer holds a value to withhold. With end.TakenBack
// the document's withdraws that wait end withdrawn: the run took them back
// before it read any document, and said nothing of them
// (takeBackFoundryWithdraws). With end.Refused,
// the document's release decisions, and withdraws never sent,
// that wait for the request are refused for that reason, as the engine said
// nothing of them; a request owed again since is not this job's to answer,
// so its decisions go on waiting for the job that sends them all. One left
// unconfirmed by a job that failed stays so: it may be written, which this
// refusal knows nothing of.
func (s *mysqlDB) EndFoundryLoadResend(ctx context.Context, jobID, workerID string, end model.FoundryResendEnd) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var loadID string
		if err := tx.db.QueryRowContext(ctx, `SELECT load_id FROM foundry_load_jobs`+ownedFoundryGraphJob+` FOR UPDATE`, jobID, workerID).
			Scan(&loadID); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		ended, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_resends SET status='done', ended_at=NOW(6), job_id=?
WHERE load_id=? AND document_id=? AND reason=? AND requests=? AND status='owed'`, jobID, loadID, end.DocumentID, string(end.Reason), end.Requests)
		if err != nil {
			return err
		}
		n, err := ended.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		if end.TakenBack {
			if err := tx.takeBackFoundryWithdraws(ctx, loadID, end.DocumentID); err != nil {
				return err
			}
		}
		if end.Reason == model.FoundryResendDecisions {
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_decisions SET applied_status=?, applied_reason=NULL, applied_at=NOW(6)
WHERE load_id=? AND kind='prop' AND applied_status IN ('pending', 'carried')`, string(model.FoundryValueApplied), loadID); err != nil {
				return err
			}
			_, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_load_decisions WHERE load_id=? AND kind='prop' AND applied_status=?`,
				loadID, string(model.FoundryValueRemoved))
			return err
		}
		if end.Refused == "" {
			return nil
		}
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_load_decisions SET applied_status='refused', applied_reason=?, applied_at=NOW(6)
WHERE load_id=? AND kind IN ('release', 'withdraw') AND doc_id=? AND applied_status IN ('pending', 'carried') AND applied_reason IS NULL`,
			end.Refused, loadID, end.DocumentID)
		return err
	})
}

// failFoundryResends ends what a resend job that ended without succeeding
// left owed of its load. The approvals that still wait are marked
// unconfirmed (model.FoundryDecisionApplied.Unconfirmed): the job may have
// failed after the engine wrote one and before its event was recorded, so
// nothing here says a claim stays held. They keep their status, so the next
// resend of their document records the engine's word as for any that waits
// (a pending one the engine no longer holds a review for was written), and
// their reason says the job failed, so their cards say so rather than wait
// for a job that won't come. The rows end, so the job's end queues no other.
// Approving again owes the document anew. A withdraw that still waits is
// unconfirmed the same way, and has no card to send it from: as a value
// decision is, it is sent again once the next job of the ontology ends
// (FinishFoundryGraphJob, carryFoundryReleases). The value decisions that still
// wait are unconfirmed the same way, and so are the ones taken back that the
// load was to be told of: the load's list holds them, so its next Extract of
// any kind carries the first and none of the second, and the next resend
// that ends the load's own request marks the first applied and drops the
// second. Choosing a value again queues that resend, and so does the end of
// the ontology's next job, which owes the list again
// (FinishFoundryGraphJob): the engine keeps a value decision once and
// withdraws one it is not sent, so sending the list again costs nothing and
// changes nothing it already did, and a withdrawal has no card a person
// could send it from. The caller holds the ontology's lock.
func (s *mysqlDB) failFoundryResends(ctx context.Context, job *model.FoundryGraphJob) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_load_decisions SET applied_reason=?
WHERE load_id=? AND kind='prop' AND applied_status IN ('pending', 'carried', 'removed')`, model.FoundryReleaseJobFailed, job.LoadID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_load_decisions SET applied_reason=?
WHERE load_id=? AND kind IN ('release', 'withdraw') AND applied_status IN ('pending', 'carried')
  AND doc_id IN (SELECT document_id FROM foundry_load_resends WHERE load_id=? AND reason=? AND status='owed')`,
		model.FoundryReleaseJobFailed, job.LoadID, job.LoadID, string(model.FoundryResendApproval)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_load_resends SET status='done', ended_at=NOW(6), job_id=? WHERE load_id=? AND status='owed'`,
		job.ID, job.LoadID)
	return err
}

// recordFoundryReleaseOutcomes records what the engine said of the release
// decisions and withdraws a job sent with document docID of loadID, each on
// the decision as the load holds it, of the kind and under the hash the job
// sent (model.FoundryDecisionApplied.After). Each withdraw that takes back
// what the load recorded written moves the document's counts back
// (MovesBack). The caller runs it in the transaction that records the
// document, or RecordFoundryReleaseOutcomes does.
func (s *mysqlDB) recordFoundryReleaseOutcomes(ctx context.Context, loadID, docID string, outcomes []model.FoundryReleaseOutcome) error {
	back := 0
	for _, o := range outcomes {
		kind := model.FoundryReviewRelease
		if o.Withdraw {
			kind = model.FoundryReviewWithdraw
		}
		var current, currentReason sql.NullString
		var wasWritten sql.NullBool
		err := s.db.QueryRowContext(ctx, `SELECT applied_status, applied_reason, was_written FROM foundry_load_decisions
WHERE load_id=? AND id=? AND kind=? AND statement_hash=? FOR UPDATE`, loadID, o.DecisionID, string(kind), o.StatementHash).
			Scan(&current, &currentReason, &wasWritten)
		if err == sql.ErrNoRows {
			// A decision removed, or approved as it reads now, after the job
			// read it: the engine's word on what the job sent is dropped. A
			// release the load may have written then became a withdraw
			// (withdrawFoundryReleases), which the next resend sends, so a
			// fact no decision accounts for is taken back.
			log.From(ctx).Warn("graph job: the engine's word on a release the load's list no longer holds was dropped",
				"load", loadID, "decision", o.DecisionID, "outcome", string(o.Kind))
			continue
		}
		if err != nil {
			return err
		}
		applied := model.FoundryDecisionApplied{Status: model.FoundryReleaseStatus(current.String), Reason: currentReason.String,
			WasWritten: wasWritten.Bool}
		status, reason, ok := applied.After(o)
		if !ok {
			continue
		}
		if o.Withdraw && applied.MovesBack(status) {
			back++
		}
		var why any
		if reason != "" {
			why = reason
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE foundry_load_decisions SET applied_status=?, applied_reason=?, applied_at=NOW(6)
WHERE load_id=? AND id=?`, string(status), why, loadID, o.DecisionID); err != nil {
			return err
		}
	}
	return s.moveFoundryCountsBack(ctx, loadID, docID, back)
}

// moveFoundryCountsBack moves a document's counts back by n take-backs of
// what its load recorded written: n facts fewer and n reviews more, as
// recording each write moved them forward.
func (s *mysqlDB) moveFoundryCountsBack(ctx context.Context, loadID, docID string, n int) error {
	if n == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_load_extractions SET facts=GREATEST(facts-?, 0), reviews=reviews+?
WHERE load_id=? AND document_id=?`, n, n, loadID, docID)
	return err
}

// takeBackFoundryWithdraws ends the withdraws of document docID in loadID
// that wait, one unconfirmed by an earlier job included, as taken back: an
// Extract carried them to a run that reached its documents, and the engine
// takes every withdraw back before it reads any document, at no cost, saying
// so only in the document's event, which this run never sent (the document
// needed a model call, so got none; contract 4.16.0). Sent again they would
// read not_written. Each of an approval the load recorded written moves the
// document's counts back. The caller holds the job's row.
func (s *mysqlDB) takeBackFoundryWithdraws(ctx context.Context, loadID, docID string) error {
	var back int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_load_decisions
WHERE load_id=? AND kind='withdraw' AND doc_id=? AND applied_status IN ('pending', 'carried') AND was_written`, loadID, docID).Scan(&back); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_load_decisions SET applied_status=?, applied_reason=NULL, applied_at=NOW(6)
WHERE load_id=? AND kind='withdraw' AND doc_id=? AND applied_status IN ('pending', 'carried')`,
		string(model.FoundryWithdrawn), loadID, docID); err != nil {
		return err
	}
	return s.moveFoundryCountsBack(ctx, loadID, docID, back)
}

// RecordFoundryReleaseOutcomes records what the engine said of the decisions
// a job sent with document docID, where the job keeps no row of the
// document's own event: a document sent again whose event is failed still
// lists what became of each withdraw, taken back before the document was
// read (contract 4.16.0). The job's load is the one its row names, while
// workerID holds its lease; ErrNotFound means it lost it, and nothing was
// written.
func (s *mysqlDB) RecordFoundryReleaseOutcomes(ctx context.Context, jobID, workerID, docID string, outcomes []model.FoundryReleaseOutcome) error {
	if len(outcomes) == 0 {
		return nil
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var loadID string
		if err := tx.db.QueryRowContext(ctx, `SELECT load_id FROM foundry_load_jobs`+ownedFoundryGraphJob+` FOR UPDATE`, jobID, workerID).
			Scan(&loadID); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		return tx.recordFoundryReleaseOutcomes(ctx, loadID, docID, outcomes)
	})
}

// ListFoundryDecisionsApplied returns what became of the given release and
// value decisions, and withdraws, in the ontology's current load, by decision id; a decision
// that load's list doesn't hold is absent, as is a resolve, and one it holds
// that no job has reported on has the empty status. One that waits with a
// reason is unconfirmed (model.FoundryDecisionApplied.Unconfirmed). A value
// the list holds under the decision's id from another time is not that
// decision (carryFoundryValues), and is absent too.
func (s *mysqlDB) ListFoundryDecisionsApplied(ctx context.Context, ontologyID string, ids []string) (map[string]model.FoundryDecisionApplied, error) {
	out := map[string]model.FoundryDecisionApplied{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT l.id, l.applied_status, l.applied_reason, l.applied_at FROM foundry_ontologies o
JOIN foundry_load_decisions l ON l.load_id=o.current_load_id
WHERE o.id=? AND l.id IN (`+idPlaceholders(len(ids))+`) AND (l.kind IN ('release', 'withdraw') OR l.kind='prop' AND EXISTS (
  SELECT 1 FROM foundry_review_decisions r WHERE r.ontology_id=o.id AND r.id=l.id AND r.decided_at=l.decided_at))`,
		append([]any{ontologyID}, toAnySlice(ids)...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var status, reason sql.NullString
		var at sql.NullTime
		if err := rows.Scan(&id, &status, &reason, &at); err != nil {
			return nil, err
		}
		applied := model.FoundryDecisionApplied{Status: model.FoundryReleaseStatus(status.String), Reason: reason.String}
		if at.Valid {
			applied.At = &at.Time
		}
		out[id] = applied
	}
	return out, rows.Err()
}
