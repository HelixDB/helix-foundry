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
// something of the document that the load holds changed after its commit: so
// far only a person's approval of a claim its review holds back, which joins
// the load's decisions as it is settled (SettleFoundryReleases). A resend job
// sends each owed document with its saved extraction, and the engine replays
// it and writes what the approval releases; the document never leaves the
// load. One job at a time writes an ontology's database, so an approval that
// lands while a job is live stays owed, and that job's end queues the resend
// (queueAfterFoundryGraphJob).
//
// A load brought up to date this way is not always the load a rebuild
// makes. The engine admits a released claim against what the load holds
// when the release runs: here that is when the person approves, while a new
// load releases every approval once all its documents have committed, in
// document order. Claims that agree end the same either way. Claims that
// conflict on a link holding one value can end differently, and nothing
// flags it: an approval written here is refused as a conflict by a rebuild
// that committed a later document stating otherwise first, and two
// conflicting approvals are written here in the order they were approved,
// there in document order. This is accepted, not guarded against: each load
// says what it made of an approval (applied_status), and none promises what
// the next will.

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
// list already has a decision for is left as it is, whatever its id.
//
// First the list loses the releases it has not written whose decision is
// gone. Removing a decision takes it from the load that is current then
// (DeleteFoundryReviewDecision), so a copy is left behind in a load that was
// not: one made current again later, whose list still held the approval
// waiting, refused or unconfirmed. An older binary's removal leaves the
// current load's copy too. A resend sends every release its load's list
// holds of a document, and every resend job is queued through here
// (queueFoundryResend), so none sends an approval a person took back; the
// document such a copy owed is sent with what is left. The caller holds the
// ontology's lock.
func (s *mysqlDB) carryFoundryReleases(ctx context.Context, o *model.FoundryOntology) error {
	if o.CurrentLoadID == nil {
		return nil
	}
	loadID := *o.CurrentLoadID
	// <=> is NULL-safe: a copy no job has reported on has no status.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_load_decisions
WHERE load_id=? AND kind='release' AND NOT (applied_status <=> 'written')
  AND id NOT IN (SELECT id FROM foundry_review_decisions WHERE ontology_id=?)`, loadID, o.ID); err != nil {
		return err
	}
	columns := "r." + strings.ReplaceAll(foundryReviewDecisionColumns, ", ", ", r.")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_load_decisions (load_id, `+foundryReviewDecisionColumns+`, applied_status)
SELECT ?, `+columns+`, 'carried' FROM foundry_review_decisions r
JOIN foundry_load_extractions e ON e.load_id=? AND e.document_id=r.doc_id AND e.status='extracted'
LEFT JOIN foundry_load_decisions l ON l.load_id=? AND l.kind='release' AND l.doc_id=r.doc_id AND l.claim_key=r.claim_key
  AND l.claim_section=r.claim_section AND l.claim_start=r.claim_start AND l.claim_end=r.claim_end
WHERE r.ontology_id=? AND r.kind='release' AND l.id IS NULL`, loadID, loadID, loadID, o.ID); err != nil {
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
	return err
}

// queueFoundryResend queues job, a resend into the ontology's current load,
// when that load owes a document one, first carrying over the approvals the
// load hasn't read (carryFoundryReleases); queued reports whether it did. The
// job is due after wait, which a request that comes meanwhile puts off again
// (SettleFoundryReleases). The caller holds the ontology's lock and no job is
// live.
func (s *mysqlDB) queueFoundryResend(ctx context.Context, o *model.FoundryOntology, job *model.FoundryGraphJob, wait time.Duration) (queued bool, err error) {
	if o.CurrentLoadID == nil {
		return false, nil
	}
	if err := s.carryFoundryReleases(ctx, o); err != nil {
		return false, err
	}
	loadID := *o.CurrentLoadID
	var owed bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_load_resends WHERE load_id=? AND status='owed')`, loadID).
		Scan(&owed); err != nil || !owed {
		return false, err
	}
	if job.ID == "" {
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

// ListFoundryLoadResends returns the documents a load owes a resend, in the
// order a load sends its documents (ListPendingFoundryDocuments), so a
// resend's releases go in the order a fresh load's would; those that can't be
// sent (FoundryLoadResend.Document is nil) come last, each saying whether the
// load still holds its document (FoundryLoadResend.Unsendable).
func (s *mysqlDB) ListFoundryLoadResends(ctx context.Context, loadID string) ([]*model.FoundryLoadResend, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT document_id, reason, requests FROM foundry_load_resends
WHERE load_id=? AND status='owed' ORDER BY document_id, reason`, loadID)
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

// EndFoundryLoadResend ends a document's resend into the job's load, while
// workerID holds the job's lease; ErrNotFound means it lost it, and nothing
// was written. The row ends only if it is still the request the job read
// (end.Requests): one owed again meanwhile stays owed, and the job's end
// queues another resend. With end.Refused, the document's release decisions
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
		if n, err := ended.RowsAffected(); err != nil || n == 0 || end.Refused == "" {
			return err
		}
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_load_decisions SET applied_status='refused', applied_reason=?, applied_at=NOW(6)
WHERE load_id=? AND kind='release' AND doc_id=? AND applied_status IN ('pending', 'carried') AND applied_reason IS NULL`,
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
// Approving again owes the document anew. The caller holds the ontology's
// lock.
func (s *mysqlDB) failFoundryResends(ctx context.Context, job *model.FoundryGraphJob) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_load_decisions SET applied_reason=?
WHERE load_id=? AND kind='release' AND applied_status IN ('pending', 'carried')
  AND doc_id IN (SELECT document_id FROM foundry_load_resends WHERE load_id=? AND reason=? AND status='owed')`,
		model.FoundryReleaseJobFailed, job.LoadID, job.LoadID, string(model.FoundryResendApproval)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_load_resends SET status='done', ended_at=NOW(6), job_id=? WHERE load_id=? AND status='owed'`,
		job.ID, job.LoadID)
	return err
}

// recordFoundryReleaseOutcomes records what the engine said of the release
// decisions a job sent with a document of loadID, each on the decision as the
// load holds it under the hash the job sent (model.FoundryDecisionApplied.After).
// The caller runs it in the transaction that records the document.
func (s *mysqlDB) recordFoundryReleaseOutcomes(ctx context.Context, loadID string, outcomes []model.FoundryReleaseOutcome) error {
	for _, o := range outcomes {
		var current, currentReason sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT applied_status, applied_reason FROM foundry_load_decisions
WHERE load_id=? AND id=? AND kind='release' AND statement_hash=? FOR UPDATE`, loadID, o.DecisionID, o.StatementHash).Scan(&current, &currentReason)
		if err == sql.ErrNoRows {
			// A decision removed, or approved as it reads now, after the job
			// read it: the engine's word on what the job sent is dropped, and
			// if that word is written, the load holds a fact no decision in
			// its list accounts for.
			log.From(ctx).Warn("graph job: the engine's word on a release the load's list no longer holds was dropped",
				"load", loadID, "decision", o.DecisionID, "outcome", string(o.Kind))
			continue
		}
		if err != nil {
			return err
		}
		applied := model.FoundryDecisionApplied{Status: model.FoundryReleaseStatus(current.String), Reason: currentReason.String}
		status, reason, ok := applied.After(o)
		if !ok {
			continue
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
	return nil
}

// ListFoundryDecisionsApplied returns what became of the given release
// decisions in the ontology's current load, by decision id; a decision that
// load's list doesn't hold is absent, as is one of another kind, and one it
// holds that no job has reported on has the empty status. One that waits
// with a reason is unconfirmed (model.FoundryDecisionApplied.Unconfirmed).
func (s *mysqlDB) ListFoundryDecisionsApplied(ctx context.Context, ontologyID string, ids []string) (map[string]model.FoundryDecisionApplied, error) {
	out := map[string]model.FoundryDecisionApplied{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT l.id, l.applied_status, l.applied_reason, l.applied_at FROM foundry_ontologies o
JOIN foundry_load_decisions l ON l.load_id=o.current_load_id
WHERE o.id=? AND l.kind='release' AND l.id IN (`+idPlaceholders(len(ids))+`)`, append([]any{ontologyID}, toAnySlice(ids)...)...)
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
