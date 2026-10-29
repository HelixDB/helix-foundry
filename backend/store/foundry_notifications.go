package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

const foundryNotificationColumns = `id, kind, subject_id, workspace_id, user_id, ontology_id, status, attempts, next_attempt_at, claim_token,
       claimed_until, last_error, created_at, sent_at`

// maxNotificationErrorChars is as much of a failure as last_error keeps.
const maxNotificationErrorChars = 1000

// sampleRun keeps the generations (aliased g) that drafted from a sample.
const sampleRun = ` COALESCE(JSON_UNQUOTE(JSON_EXTRACT(g.input, '$.sample')), '') <> ''`

// DiscoverFoundryOntologyReady queues the ontology_ready email of each
// first draft (OntologyGenerationInput.LoadWhenReady) with a requester that
// finished at or after since and whose end is known: it failed; it
// succeeded and its load (the job under FirstDraftLoadTokenPrefix and its
// id) ended ready, partial or failed; or it succeeded and queued no such
// load, so the draft waits for review. One whose load is still writing is
// found by a later call; one whose load was cancelled (its ontology was
// deleted) never is. It reads at most limit of them, oldest first, among
// those with no email yet, and returns how many it queued.
//
// The worker calls it every tick. The generations' finished_at index reads
// only those that finished since, a window of days that
// foundry.ontology.per_hour bounds per workspace; each one's load is read by
// its keys.
func (s *mysqlDB) DiscoverFoundryOntologyReady(ctx context.Context, since time.Time, limit int, newID func() string) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT g.id, g.workspace_id, g.ontology_id, g.requested_by_user_id FROM foundry_ontology_generations g
LEFT JOIN foundry_load_jobs j ON j.ontology_id=g.ontology_id AND j.client_token=CONCAT(?, g.id)
LEFT JOIN foundry_loads l ON l.id=j.load_id
WHERE g.finished_at >= ? AND g.status IN ('succeeded', 'failed') AND g.requested_by_user_id IS NOT NULL
  AND JSON_EXTRACT(g.input, '$.load_when_ready') = true
  AND (g.status='failed' OR j.id IS NULL OR l.status IN ('ready', 'partial', 'failed'))
  AND NOT EXISTS (SELECT 1 FROM foundry_notifications n WHERE n.kind='ontology_ready' AND n.subject_id=g.id)
ORDER BY g.finished_at, g.id LIMIT ?`, model.FirstDraftLoadTokenPrefix, since.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("find first drafts that ended: %w", err)
	}
	var found []*model.FoundryNotification
	for rows.Next() {
		n := &model.FoundryNotification{Kind: model.FoundryNotifyOntologyReady, Status: model.FoundryNotificationPending}
		var ontologyID string
		if err := rows.Scan(&n.SubjectID, &n.WorkspaceID, &ontologyID, &n.UserID); err != nil {
			_ = rows.Close()
			return 0, err
		}
		n.OntologyID = &ontologyID
		found = append(found, n)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	queued := 0
	for _, n := range found {
		n.ID = newID()
		inserted, err := s.insertFoundryNotification(ctx, n)
		if err != nil {
			return queued, err
		}
		if inserted {
			queued++
		}
	}
	return queued, nil
}

// DiscoverFoundrySampleFollowUps queues the sample_follow_up email of each
// person who ran the sample (a succeeded generation whose input names one)
// that finished in [from, to) and who has had none: pending when the run's
// workspace still has no data of its own, and else skipped for good, as
// they have tried Foundry on their own (foundryWorkspaceHasOwnData). A
// person with several runs among those read is asked about the first whose
// workspace has none. It reads at most limit runs, oldest first, among those
// of people with no follow-up yet, and returns how many emails it queued,
// pending or skipped.
//
// The worker calls it every tick. The generations' finished_at index reads
// only the runs that finished in the window, a couple of days; each person
// found is decided once, by reading their workspace's ontologies and their
// sample runs, its connectors and its documents, which the workspace's caps
// bound.
func (s *mysqlDB) DiscoverFoundrySampleFollowUps(ctx context.Context, from, to time.Time, limit int, newID func() string) (int, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT g.id, g.workspace_id, g.requested_by_user_id FROM foundry_ontology_generations g
WHERE g.finished_at >= ? AND g.finished_at < ? AND g.status='succeeded' AND g.requested_by_user_id IS NOT NULL AND`+sampleRun+`
  AND NOT EXISTS (SELECT 1 FROM foundry_notifications n WHERE n.kind='sample_follow_up' AND n.subject_id=g.requested_by_user_id)
ORDER BY g.finished_at, g.id LIMIT ?`, from.UTC(), to.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("find sample runs to follow up: %w", err)
	}
	type run struct{ generationID, workspaceID string }
	var people []string
	runs := map[string][]run{}
	for rows.Next() {
		var r run
		var userID string
		if err := rows.Scan(&r.generationID, &r.workspaceID, &userID); err != nil {
			_ = rows.Close()
			return 0, err
		}
		if runs[userID] == nil {
			people = append(people, userID)
		}
		runs[userID] = append(runs[userID], r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	queued := 0
	for _, userID := range people {
		why := "the workspace has data of its own"
		n := &model.FoundryNotification{
			ID: newID(), Kind: model.FoundryNotifySampleFollowUp, SubjectID: userID, UserID: userID, WorkspaceID: runs[userID][0].workspaceID,
			Status: model.FoundryNotificationSkipped, LastError: &why,
		}
		for _, r := range runs[userID] {
			own, err := s.foundryWorkspaceHasOwnData(ctx, r.workspaceID)
			if err != nil {
				return queued, err
			}
			if !own {
				n.WorkspaceID, n.Status, n.LastError = r.workspaceID, model.FoundryNotificationPending, nil
				break
			}
		}
		inserted, err := s.insertFoundryNotification(ctx, n)
		if err != nil {
			return queued, err
		}
		if inserted {
			queued++
		}
	}
	return queued, nil
}

// foundryWorkspaceHasOwnData reports whether a workspace holds data no
// sample put there: a live ontology no sample run drafted, a live
// connector no sample run drafted from, or a document not deleted outside
// the sets the sample runs read. The sample runs are those of any of the
// workspace's ontologies, live or not.
func (s *mysqlDB) foundryWorkspaceHasOwnData(ctx context.Context, workspaceID string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.ontology_id, g.input FROM foundry_ontologies o
JOIN foundry_ontology_generations g ON g.ontology_id=o.id WHERE o.workspace_id=? AND`+sampleRun, workspaceID)
	if err != nil {
		return false, fmt.Errorf("read the workspace's sample runs: %w", err)
	}
	var ontologies, connectors, sets []string
	for rows.Next() {
		var ontologyID, raw string
		var input model.OntologyGenerationInput
		if err := rows.Scan(&ontologyID, &raw); err != nil {
			_ = rows.Close()
			return false, err
		}
		if err := json.Unmarshal([]byte(raw), &input); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("decode a sample run's input: %w", err)
		}
		ontologies = append(ontologies, ontologyID)
		for _, t := range input.Tables {
			connectors = append(connectors, t.ConnectorID)
		}
		sets = append(sets, input.DocumentSetIDs...)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, check := range []struct {
		from, column string
		samples      []string
	}{
		{"foundry_ontologies WHERE workspace_id=? AND deleted_at IS NULL", "id", ontologies},
		{"connectors WHERE workspace_id=? AND deleted_at IS NULL", "id", connectors},
		{"foundry_documents WHERE workspace_id=? AND status<>'deleted'", "set_id", sets},
	} {
		query, args := "SELECT EXISTS (SELECT 1 FROM "+check.from, []any{workspaceID}
		if len(check.samples) > 0 {
			query += " AND " + check.column + " NOT IN (" + idPlaceholders(len(check.samples)) + ")"
			args = append(args, toAnySlice(check.samples)...)
		}
		var own bool
		if err := s.db.QueryRowContext(ctx, query+")", args...).Scan(&own); err != nil || own {
			return own, err
		}
	}
	return false, nil
}

// insertFoundryNotification writes n, pending or skipped, unless its kind
// already has an email of its subject; false when it had.
func (s *mysqlDB) insertFoundryNotification(ctx context.Context, n *model.FoundryNotification) (bool, error) {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO foundry_notifications (id, kind, subject_id, workspace_id, user_id, ontology_id, status, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, n.ID, string(n.Kind), n.SubjectID, n.WorkspaceID, n.UserID, n.OntologyID, string(n.Status), n.LastError)
	switch {
	case isDuplicateKey(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("queue %s email of %s: %w", n.Kind, n.SubjectID, err)
	}
	return true, nil
}

// ClaimFoundryNotification claims the pending email due first, for lease:
// one whose time has come that no claim holds, or whose claim lapsed (its
// worker stopped while sending it). Each claim counts an attempt and holds
// the email under token, a new one per claim, which only its holder can
// end it with (FinishFoundryNotification). nil means none is due.
func (s *mysqlDB) ClaimFoundryNotification(ctx context.Context, token string, lease time.Duration) (_ *model.FoundryNotification, err error) {
	if s.root == nil {
		return nil, fmt.Errorf("foundry notification claim requires a root database connection")
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin foundry notification claim: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	// A claimed email keeps the next_attempt_at it was due at, so lapsed
	// claims fall in the index's range too.
	n, err := scanFoundryNotification(tx.QueryRowContext(ctx, "SELECT "+foundryNotificationColumns+` FROM foundry_notifications
WHERE status='pending' AND next_attempt_at <= NOW(6) AND (claimed_until IS NULL OR claimed_until < NOW(6))
ORDER BY next_attempt_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`))
	if errors.Is(err, ErrNotFound) {
		_ = tx.Rollback()
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select foundry notification claim: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE foundry_notifications
SET attempts=attempts+1, claim_token=?, claimed_until=NOW(6) + INTERVAL ? MICROSECOND WHERE id=?`, token, lease.Microseconds(), n.ID); err != nil {
		return nil, fmt.Errorf("claim foundry notification: %w", err)
	}
	if n, err = scanFoundryNotification(tx.QueryRowContext(ctx, "SELECT "+foundryNotificationColumns+` FROM foundry_notifications
WHERE id=?`, n.ID)); err != nil {
		return nil, fmt.Errorf("read claimed foundry notification: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit foundry notification claim: %w", err)
	}
	return n, nil
}

// FinishFoundryNotification ends token's claim of email id with outcome:
// sent, which it never is again; skipped or failed for good; or due again
// at a retry's time. ErrNotFound means the claim was lost (it lapsed, and
// another claim holds the email or ended it) and nothing changed.
func (s *mysqlDB) FinishFoundryNotification(ctx context.Context, id, token string, outcome model.FoundryNotificationOutcome) error {
	const claimed = `, claim_token=NULL, claimed_until=NULL WHERE id=? AND status='pending' AND claim_token=?`
	var query string
	var args []any
	switch o := outcome.(type) {
	case model.FoundryNotificationDelivered:
		query = `UPDATE foundry_notifications SET status='sent', sent_at=NOW(6), last_error=NULL` + claimed
	case model.FoundryNotificationSkip:
		if o.Reason == "" {
			return fmt.Errorf("a skipped email needs its reason")
		}
		query, args = `UPDATE foundry_notifications SET status='skipped', last_error=?`+claimed, []any{clipChars(o.Reason, maxNotificationErrorChars)}
	case model.FoundryNotificationRetry:
		if o.Error == "" {
			return fmt.Errorf("a retried email needs its failure")
		}
		query, args = `UPDATE foundry_notifications SET next_attempt_at=?, last_error=?`+claimed,
			[]any{o.At.UTC(), clipChars(o.Error, maxNotificationErrorChars)}
	case model.FoundryNotificationGiveUp:
		if o.Error == "" {
			return fmt.Errorf("a failed email needs its failure")
		}
		query, args = `UPDATE foundry_notifications SET status='failed', last_error=?`+claimed, []any{clipChars(o.Error, maxNotificationErrorChars)}
	default:
		return fmt.Errorf("foundry notification outcome %T is not one FinishFoundryNotification takes", outcome)
	}
	return execOrNotFound(ctx, s.db, query, append(args, id, token)...)
}

func scanFoundryNotification(scanner rowScanner) (*model.FoundryNotification, error) {
	var n model.FoundryNotification
	var kind, status string
	var ontologyID, token, lastError sql.NullString
	var claimedUntil, sentAt sql.NullTime
	if err := scanner.Scan(&n.ID, &kind, &n.SubjectID, &n.WorkspaceID, &n.UserID, &ontologyID, &status, &n.Attempts, &n.NextAttemptAt,
		&token, &claimedUntil, &lastError, &n.CreatedAt, &sentAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	n.Kind, n.Status = model.FoundryNotificationKind(kind), model.FoundryNotificationStatus(status)
	for _, f := range []struct {
		src sql.NullString
		dst **string
	}{{ontologyID, &n.OntologyID}, {token, &n.ClaimToken}, {lastError, &n.LastError}} {
		if f.src.Valid {
			*f.dst = &f.src.String
		}
	}
	for _, f := range []struct {
		src sql.NullTime
		dst **time.Time
	}{{claimedUntil, &n.ClaimedUntil}, {sentAt, &n.SentAt}} {
		if f.src.Valid {
			*f.dst = &f.src.Time
		}
	}
	return &n, nil
}
