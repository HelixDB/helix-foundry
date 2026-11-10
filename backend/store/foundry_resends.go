package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

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
// could not copy. Each joins the list as carried, and its document, if the
// load holds an extraction of it, is owed a resend. A claim the list already
// has a decision for is left as it is, whatever its id. The caller holds the
// ontology's lock.
func (s *mysqlDB) carryFoundryReleases(ctx context.Context, o *model.FoundryOntology) error {
	if o.CurrentLoadID == nil {
		return nil
	}
	loadID := *o.CurrentLoadID
	columns := "r." + strings.ReplaceAll(foundryReviewDecisionColumns, ", ", ", r.")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_load_decisions (load_id, `+foundryReviewDecisionColumns+`, applied_status)
SELECT ?, `+columns+`, 'carried' FROM foundry_review_decisions r
LEFT JOIN foundry_load_decisions l ON l.load_id=? AND l.kind='release' AND l.doc_id=r.doc_id AND l.claim_key=r.claim_key
  AND l.claim_section=r.claim_section AND l.claim_start=r.claim_start AND l.claim_end=r.claim_end
WHERE r.ontology_id=? AND r.kind='release' AND l.id IS NULL`, loadID, loadID, o.ID); err != nil {
		return err
	}
	// A carried decision whose document the load hasn't extracted is said of
	// by the commit that extracts it, so only a held document is owed.
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_load_resends (load_id, document_id, reason)
SELECT DISTINCT l.load_id, l.doc_id, ? FROM foundry_load_decisions l
JOIN foundry_load_extractions e ON e.load_id=l.load_id AND e.document_id=l.doc_id AND e.status='extracted'
WHERE l.load_id=? AND l.kind='release' AND l.applied_status='carried'
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
// sent (FoundryLoadResend.Document is nil) come last.
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
	// Map order is random: by document, so two attempts end them alike.
	slices.SortFunc(unsendable, func(a, b *model.FoundryLoadResend) int {
		return cmp.Or(cmp.Compare(a.DocumentID, b.DocumentID), cmp.Compare(a.Reason, b.Reason))
	})
	return append(out, unsendable...), nil
}

// EndFoundryLoadResend ends a document's resend into the job's load, while
// workerID holds the job's lease; ErrNotFound means it lost it, and nothing
// was written. With end.Refused, the document's release decisions that still
// wait are refused for that reason: the engine said nothing of them. The row
// ends only if it is still the request the job read (end.Requests): one owed
// again meanwhile stays owed, and the job's end queues another resend.
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
		if end.Refused != "" {
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_decisions SET applied_status='refused', applied_reason=?, applied_at=NOW(6)
WHERE load_id=? AND kind='release' AND doc_id=? AND applied_status IN ('pending', 'carried')`, end.Refused, loadID, end.DocumentID); err != nil {
				return err
			}
		}
		_, err := tx.db.ExecContext(ctx, `UPDATE foundry_load_resends SET status='done', ended_at=NOW(6), job_id=?
WHERE load_id=? AND document_id=? AND reason=? AND requests=? AND status='owed'`, jobID, loadID, end.DocumentID, string(end.Reason), end.Requests)
		return err
	})
}

// failFoundryResends ends what a resend job that ended without succeeding
// left owed of its load: the approvals that still wait are refused, saying
// the job failed, so their cards say so rather than wait for a job that
// won't come, and the rows end, so the job's end queues no other. Approving
// again owes the document anew. The caller holds the ontology's lock.
func (s *mysqlDB) failFoundryResends(ctx context.Context, job *model.FoundryGraphJob) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_load_decisions SET applied_status='refused', applied_reason=?, applied_at=NOW(6)
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
// load holds it under the hash the job sent (model.FoundryReleaseStatus.After).
// The caller runs it in the transaction that records the document.
func (s *mysqlDB) recordFoundryReleaseOutcomes(ctx context.Context, loadID string, outcomes []model.FoundryReleaseOutcome) error {
	for _, o := range outcomes {
		var current sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT applied_status FROM foundry_load_decisions
WHERE load_id=? AND id=? AND kind='release' AND statement_hash=? FOR UPDATE`, loadID, o.DecisionID, o.StatementHash).Scan(&current)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return err
		}
		status, reason, ok := model.FoundryReleaseStatus(current.String).After(o)
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

// ListFoundryDecisionsApplied returns what became of the given decisions in
// the ontology's current load, by decision id; a decision that load's list
// doesn't hold is absent, and one it holds that no job has reported on has
// the empty status.
func (s *mysqlDB) ListFoundryDecisionsApplied(ctx context.Context, ontologyID string, ids []string) (map[string]model.FoundryDecisionApplied, error) {
	out := map[string]model.FoundryDecisionApplied{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT l.id, l.applied_status, l.applied_reason, l.applied_at FROM foundry_ontologies o
JOIN foundry_load_decisions l ON l.load_id=o.current_load_id
WHERE o.id=? AND l.id IN (`+idPlaceholders(len(ids))+`)`, append([]any{ontologyID}, toAnySlice(ids)...)...)
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
