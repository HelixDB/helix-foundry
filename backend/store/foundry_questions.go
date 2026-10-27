package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"strings"
	"time"
)

const foundryQuestionColumns = `id, workspace_id, ontology_id, load_id, version_id, thread_id, question, focus, status, result, error,
       client_token, lease_owner, lease_until, attempts, next_attempt_at, max_usd_micros, reserved_micros, usd_micros,
       input_tokens, output_tokens, last_seq, asked_by_user_id, created_at, started_at, finished_at`

// claimFoundryQuestionTries bounds the questions one claim looks at: each one
// its workspace's limits hold back is deferred, or failed, so the next is
// tried, as is the next after one another claim took first.
const claimFoundryQuestionTries = 20

const ownedFoundryQuestion = ` WHERE id=? AND status='running' AND lease_owner=?`

// CreateFoundryQuestion queues q under its workspace's limits, in one
// transaction that holds the workspace's lock, so concurrent asks can't pass
// them together:
//   - at most limits.PerHour questions created in the last hour;
//   - what the questions charged to the month settled, plus q's cap, within
//     limits.CapMicros. Running questions' reservations don't count here: they
//     settle at most at what they reserved, and the claim checks them.
//
// A refusal is a model.FoundryQuestionRefusal. ErrConflict means the
// ontology has a question with q's client token; ErrNotFound a workspace
// that is gone. The caller checked the ontology, its load and the thread.
func (s *mysqlDB) CreateFoundryQuestion(ctx context.Context, q *model.FoundryQuestion, limits model.FoundryQuestionLimits) error {
	switch {
	case q.ID == "" || q.WorkspaceID == "" || q.OntologyID == "" || q.LoadID == "" || q.VersionID == "" || q.ThreadID == "":
		return fmt.Errorf("foundry question is missing an id")
	case strings.TrimSpace(q.Question) == "" || q.MaxUSDMicros <= 0:
		return fmt.Errorf("foundry question %s has no question or cap", q.ID)
	case limits.PerHour < 1 || limits.CapMicros <= 0:
		return fmt.Errorf("foundry question limits must be positive")
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.workspaces.Lock(ctx, tx.db, q.WorkspaceID); err != nil {
			return err
		}
		var lastHour int
		var settled int64
		if err := tx.db.QueryRowContext(ctx, `SELECT
  (SELECT COUNT(*) FROM foundry_questions WHERE workspace_id=? AND created_at >= NOW(6) - INTERVAL 1 HOUR),
  (SELECT COALESCE(SUM(usd_micros), 0) FROM foundry_questions WHERE workspace_id=? AND charged_at >= ?)`,
			q.WorkspaceID, q.WorkspaceID, limits.Since.UTC()).Scan(&lastHour, &settled); err != nil {
			return err
		}
		switch {
		case lastHour >= limits.PerHour:
			return model.FoundryQuestionRefusal{Reason: fmt.Sprintf("the workspace has asked %d questions in the last hour; try again later", lastHour)}
		case settled+q.MaxUSDMicros > limits.CapMicros:
			return model.FoundryQuestionRefusal{Budget: true,
				Reason: fmt.Sprintf("the workspace has spent this month's $%.2f of questions", float64(limits.CapMicros)/1e6)}
		}
		_, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_questions
  (id, workspace_id, ontology_id, load_id, version_id, thread_id, question, focus, status, client_token, max_usd_micros, asked_by_user_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?)`,
			q.ID, q.WorkspaceID, q.OntologyID, q.LoadID, q.VersionID, q.ThreadID, q.Question, nullJSON(q.Focus), q.ClientToken,
			q.MaxUSDMicros, q.AskedByUserID)
		if isDuplicateKey(err) {
			return ErrConflict
		}
		return err
	})
}

func (s *mysqlDB) GetFoundryQuestion(ctx context.Context, ontologyID, id string) (*model.FoundryQuestion, error) {
	return scanFoundryQuestion(s.db.QueryRowContext(ctx, "SELECT "+foundryQuestionColumns+`
FROM foundry_questions WHERE id=? AND ontology_id=?`, id, ontologyID))
}

func (s *mysqlDB) GetFoundryQuestionByClientToken(ctx context.Context, ontologyID, clientToken string) (*model.FoundryQuestion, error) {
	return scanFoundryQuestion(s.db.QueryRowContext(ctx, "SELECT "+foundryQuestionColumns+`
FROM foundry_questions WHERE ontology_id=? AND client_token=?`, ontologyID, clientToken))
}

// ListFoundryQuestions returns the ontology's questions, or one thread's when
// threadID is set, newest first, a page of limit at a time.
func (s *mysqlDB) ListFoundryQuestions(ctx context.Context, ontologyID, threadID, pageToken string, limit int) ([]*model.FoundryQuestion, string, error) {
	conds := []string{"ontology_id=?"}
	args := []any{ontologyID}
	if threadID != "" {
		conds = append(conds, "thread_id=?")
		args = append(args, threadID)
	}
	if pageToken != "" {
		cursor, err := decodeTimeIDCursor(pageToken)
		if err != nil {
			return nil, "", err
		}
		conds = append(conds, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	out, err := s.queryFoundryQuestions(ctx, "SELECT "+foundryQuestionColumns+" FROM foundry_questions"+whereClause(conds)+
		fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT %d", limit+1), args...)
	if err != nil || len(out) <= limit {
		return out, "", err
	}
	next, err := encodeTimeIDCursor(out[limit-1].CreatedAt, out[limit-1].ID)
	return out[:limit], next, err
}

// ListFoundryQuestionHistory returns up to limit answered questions of q's
// thread asked before q, oldest first: what the agent is told was said.
func (s *mysqlDB) ListFoundryQuestionHistory(ctx context.Context, q *model.FoundryQuestion, limit int) ([]*model.FoundryQuestion, error) {
	out, err := s.queryFoundryQuestions(ctx, "SELECT "+foundryQuestionColumns+` FROM foundry_questions
WHERE thread_id=? AND ontology_id=? AND status='answered' AND (created_at < ? OR (created_at = ? AND id < ?))
ORDER BY created_at DESC, id DESC LIMIT ?`, q.ThreadID, q.OntologyID, q.CreatedAt, q.CreatedAt, q.ID, limit)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, err
}

// SumFoundryQuestionSpend is what the workspace's questions charged since
// since (claimed since then, whenever they were asked) spent, and what the
// running ones among them hold back: the month's side of the budget the claim
// checks.
func (s *mysqlDB) SumFoundryQuestionSpend(ctx context.Context, workspaceID string, since time.Time) (model.FoundryQuestionSpend, error) {
	var out model.FoundryQuestionSpend
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(usd_micros), 0), COALESCE(SUM(reserved_micros), 0)
FROM foundry_questions WHERE workspace_id=? AND charged_at >= ?`, workspaceID, since.UTC()).Scan(&out.SpentMicros, &out.ReservedMicros)
	return out, err
}

// dueFoundryQuestion is the condition of a question a claim may take: a
// queued one whose time has come, or a running one whose lease ran out.
const dueFoundryQuestion = `((status='queued' AND next_attempt_at <= NOW(6)) OR (status='running' AND lease_until < NOW(6)))`

// ClaimFoundryQuestion leases the oldest due question to workerID. It finds
// a candidate without locking it, then decides it in a transaction of its
// own that locks its workspace, then the question, as CreateFoundryQuestion
// locks the workspace before it inserts: a claim's lock on a question never
// waits for a workspace an ask holds while the ask waits for it. The
// workspace's running questions and month are read after any claim that
// changed them committed. The month holds what the questions charged to it
// (claimed in it, whenever they were asked) spent and reserve; a question
// that runs is charged to it from now on, with all it spent before:
//  1. Its month can't fit it (what the month's other questions settled, plus
//     what it spent, plus its cap, passes the budget): it fails for good.
//  2. Its workspace runs claim.Limits.Running questions under live leases, or
//     the month's reservations leave too little of it: it is queued again
//     claim.Defer from now, behind the questions after it.
//  3. Otherwise it runs, reserving its cap, charged to the month; a
//     reservation a dead attempt left is charged as spent first, since what
//     that attempt spent is unknown.
//
// A backlog asked before the month and claimed in it is held to the month's
// budget like any other question.
//
// A candidate another claim or a cancel took first is passed over. Nil means
// no question is due, or none of the first few could run.
func (s *mysqlDB) ClaimFoundryQuestion(ctx context.Context, workerID string, claim model.FoundryQuestionClaim) (*model.FoundryQuestion, error) {
	l := claim.Limits
	if workerID == "" || claim.Lease <= 0 || claim.Defer <= 0 || l.CapMicros <= 0 || l.Running < 1 {
		return nil, fmt.Errorf("invalid foundry question claim")
	}
	for range claimFoundryQuestionTries {
		var candidate, workspaceID string
		err := s.db.QueryRowContext(ctx, `SELECT id, workspace_id FROM foundry_questions WHERE `+dueFoundryQuestion+`
ORDER BY next_attempt_at, id LIMIT 1`).Scan(&candidate, &workspaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("find a due foundry question: %w", err)
		}
		var claimed *model.FoundryQuestion
		err = s.WithTx(ctx, func(txDB Store) error {
			tx := txDB.(*mysqlDB)
			claimed = nil
			gone := tx.workspaces.Lock(ctx, tx.db, workspaceID)
			if gone != nil && !errors.Is(gone, ErrNotFound) {
				return gone
			}
			q, err := scanFoundryQuestion(tx.db.QueryRowContext(ctx, "SELECT "+foundryQuestionColumns+`
FROM foundry_questions WHERE id=? AND `+dueFoundryQuestion+` FOR UPDATE`, candidate))
			switch {
			case errors.Is(err, ErrNotFound):
				return nil
			case err != nil:
				return err
			case gone != nil:
				return tx.endFoundryQuestion(ctx, q.ID, model.FoundryQuestionFailed, "the workspace was deleted")
			}
			var running int
			var settled, reserved int64
			if err := tx.db.QueryRowContext(ctx, `SELECT
  COUNT(CASE WHEN status='running' AND lease_until >= NOW(6) THEN 1 END),
  COALESCE(SUM(CASE WHEN charged_at >= ? THEN usd_micros END), 0),
  COALESCE(SUM(CASE WHEN charged_at >= ? THEN reserved_micros END), 0)
FROM foundry_questions WHERE workspace_id=? AND id<>? AND (status='running' OR charged_at >= ?)`,
				l.Since.UTC(), l.Since.UTC(), q.WorkspaceID, q.ID, l.Since.UTC()).Scan(&running, &settled, &reserved); err != nil {
				return err
			}
			own := q.Spend.USDMicros + q.ReservedMicros + q.MaxUSDMicros
			switch {
			case settled+own > l.CapMicros:
				return tx.endFoundryQuestion(ctx, q.ID, model.FoundryQuestionFailed,
					fmt.Sprintf("the workspace has spent this month's $%.2f of questions", float64(l.CapMicros)/1e6))
			case running >= l.Running || settled+reserved+own > l.CapMicros:
				_, err := tx.db.ExecContext(ctx, `UPDATE foundry_questions
SET status='queued', lease_owner=NULL, lease_until=NULL, usd_micros=usd_micros+reserved_micros, reserved_micros=0,
    next_attempt_at=NOW(6) + INTERVAL ? MICROSECOND
WHERE id=?`, claim.Defer.Microseconds(), q.ID)
				return err
			}
			leaseUntil := time.Now().UTC().Add(claim.Lease)
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_questions
SET status='running', attempts=attempts+1, lease_owner=?, lease_until=?, usd_micros=usd_micros+reserved_micros,
    reserved_micros=max_usd_micros, charged_at=NOW(6), started_at=COALESCE(started_at, NOW(6))
WHERE id=?`, workerID, leaseUntil, q.ID); err != nil {
				return err
			}
			claimed, err = scanFoundryQuestion(tx.db.QueryRowContext(ctx, "SELECT "+foundryQuestionColumns+` FROM foundry_questions WHERE id=?`, q.ID))
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("claim foundry question: %w", err)
		}
		if claimed != nil {
			return claimed, nil
		}
	}
	return nil, nil
}

// endFoundryQuestion ends a live question for good with status and why,
// charging any reservation it holds as spent.
func (s *mysqlDB) endFoundryQuestion(ctx context.Context, id string, status model.FoundryQuestionStatus, why string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_questions
SET status=?, error=?, lease_owner=NULL, lease_until=NULL, usd_micros=usd_micros+reserved_micros, reserved_micros=0,
    finished_at=NOW(6)
WHERE id=?`, string(status), why, id)
	return err
}

// RenewFoundryQuestionLease extends the lease; ErrNotFound means workerID
// lost it, to another worker or to a cancel.
func (s *mysqlDB) RenewFoundryQuestionLease(ctx context.Context, id, workerID string, lease time.Duration) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_questions SET lease_until=?`+ownedFoundryQuestion,
		time.Now().UTC().Add(lease), id, workerID)
}

// AppendFoundryQuestionEvents stores events after the question's newest, in
// order, while workerID holds its lease; ErrNotFound means it lost it, to
// another worker or to a cancel, and nothing was stored.
func (s *mysqlDB) AppendFoundryQuestionEvents(ctx context.Context, id, workerID string, events []json.RawMessage) error {
	if len(events) == 0 {
		return nil
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var last int
		err := tx.db.QueryRowContext(ctx, `SELECT last_seq FROM foundry_questions`+ownedFoundryQuestion+` FOR UPDATE`, id, workerID).Scan(&last)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		args := make([]any, 0, 3*len(events))
		for i, e := range events {
			args = append(args, id, last+1+i, string(e))
		}
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_question_events (question_id, seq, event) VALUES `+
			strings.TrimSuffix(strings.Repeat("(?, ?, ?), ", len(events)), ", "), args...); err != nil {
			return err
		}
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_questions SET last_seq=? WHERE id=?`, last+len(events), id)
		return err
	})
}

// ListFoundryQuestionEvents returns up to limit of the question's events
// after afterSeq, in order.
func (s *mysqlDB) ListFoundryQuestionEvents(ctx context.Context, questionID string, afterSeq, limit int) ([]model.FoundryQuestionEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, event, created_at FROM foundry_question_events
WHERE question_id=? AND seq > ? ORDER BY seq LIMIT ?`, questionID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FoundryQuestionEvent
	for rows.Next() {
		var e model.FoundryQuestionEvent
		var event string
		if err := rows.Scan(&e.Seq, &event, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Event = json.RawMessage(event)
		out = append(out, e)
	}
	return out, rows.Err()
}

// SettleFoundryQuestion ends workerID's attempt with outcome, releasing its
// reservation and adding what it spent; ErrNotFound means it lost the lease,
// to another worker or to a cancel, and nothing was recorded.
func (s *mysqlDB) SettleFoundryQuestion(ctx context.Context, id, workerID string, o model.FoundryQuestionOutcome) error {
	if err := o.Validate(); err != nil {
		return fmt.Errorf("settle foundry question %s: %w", id, err)
	}
	spend := o.Spend
	if o.RetryAt != nil {
		given := 0
		if o.Uncounted {
			given = 1
		}
		return execOrNotFound(ctx, s.db, `UPDATE foundry_questions
SET status='queued', lease_owner=NULL, lease_until=NULL, next_attempt_at=?, attempts=attempts-?, error=?, reserved_micros=0,
    usd_micros=usd_micros+?, input_tokens=input_tokens+?, output_tokens=output_tokens+?`+ownedFoundryQuestion,
			o.RetryAt.UTC(), given, nullString(o.Error), spend.USDMicros, spend.InputTokens, spend.OutputTokens, id, workerID)
	}
	return execOrNotFound(ctx, s.db, `UPDATE foundry_questions
SET status=?, result=?, error=?, lease_owner=NULL, lease_until=NULL, reserved_micros=0, finished_at=NOW(6),
    usd_micros=usd_micros+?, input_tokens=input_tokens+?, output_tokens=output_tokens+?`+ownedFoundryQuestion,
		string(o.Status), nullJSON(o.Result), nullString(o.Error), spend.USDMicros, spend.InputTokens, spend.OutputTokens, id, workerID)
}

// CancelFoundryQuestion ends a queued or running question as cancelled. A
// running one's reservation is charged as spent: its attempt loses its lease,
// stops, and records nothing. Cancelling a cancelled question returns it;
// ErrConflict means the question ended otherwise.
func (s *mysqlDB) CancelFoundryQuestion(ctx context.Context, ontologyID, id string) (*model.FoundryQuestion, error) {
	var out *model.FoundryQuestion
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		q, err := scanFoundryQuestion(tx.db.QueryRowContext(ctx, "SELECT "+foundryQuestionColumns+`
FROM foundry_questions WHERE id=? AND ontology_id=? FOR UPDATE`, id, ontologyID))
		switch {
		case err != nil:
			return err
		case q.Status == model.FoundryQuestionCancelled:
			out = q
			return nil
		case !q.Status.Live():
			return ErrConflict
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_questions
SET status='cancelled', lease_owner=NULL, lease_until=NULL, usd_micros=usd_micros+reserved_micros, reserved_micros=0,
    finished_at=NOW(6)
WHERE id=?`, id); err != nil {
			return err
		}
		out, err = scanFoundryQuestion(tx.db.QueryRowContext(ctx, "SELECT "+foundryQuestionColumns+` FROM foundry_questions WHERE id=?`, id))
		return err
	})
	return out, err
}

func (s *mysqlDB) queryFoundryQuestions(ctx context.Context, query string, args ...any) ([]*model.FoundryQuestion, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.FoundryQuestion
	for rows.Next() {
		q, err := scanFoundryQuestion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func scanFoundryQuestion(scanner rowScanner) (*model.FoundryQuestion, error) {
	var q model.FoundryQuestion
	var status string
	var focus, result, errText, clientToken, leaseOwner, askedBy sql.NullString
	var leaseUntil, startedAt, finishedAt sql.NullTime
	spend := &q.Spend
	if err := scanner.Scan(&q.ID, &q.WorkspaceID, &q.OntologyID, &q.LoadID, &q.VersionID, &q.ThreadID, &q.Question, &focus, &status,
		&result, &errText, &clientToken, &leaseOwner, &leaseUntil, &q.Attempts, &q.NextAttemptAt, &q.MaxUSDMicros,
		&q.ReservedMicros, &spend.USDMicros, &spend.InputTokens, &spend.OutputTokens, &q.LastSeq, &askedBy, &q.CreatedAt,
		&startedAt, &finishedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	q.Status = model.FoundryQuestionStatus(status)
	for _, f := range []struct {
		src sql.NullString
		dst *json.RawMessage
	}{{focus, &q.Focus}, {result, &q.Result}} {
		if f.src.Valid {
			*f.dst = json.RawMessage(f.src.String)
		}
	}
	for _, f := range []struct {
		src sql.NullString
		dst **string
	}{{errText, &q.Error}, {clientToken, &q.ClientToken}, {leaseOwner, &q.LeaseOwner}, {askedBy, &q.AskedByUserID}} {
		if f.src.Valid {
			*f.dst = &f.src.String
		}
	}
	for _, f := range []struct {
		src sql.NullTime
		dst **time.Time
	}{{leaseUntil, &q.LeaseUntil}, {startedAt, &q.StartedAt}, {finishedAt, &q.FinishedAt}} {
		if f.src.Valid {
			*f.dst = &f.src.Time
		}
	}
	return &q, nil
}

// nullString is s as a nullable column's value: NULL when empty.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
