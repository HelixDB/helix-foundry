package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

const foundryReminderColumns = `id, email, time_zone, consent, status, sent, next_send_at, attempts, claim_token, claimed_until, last_error,
       requested_at, created_at, last_sent_at, stopped_at, stop_reason`

// ErrFoundryReminderLimit is a request for reminders past
// foundry.reminders.max_new_per_hour: about that many addresses started a
// schedule in the past hour.
var ErrFoundryReminderLimit = errors.New("store: too many reminders were asked for in the past hour")

// RequestFoundryReminder starts the reminders req asks for, or answers the
// schedule its address already has, in a transaction of its own that locks
// the address's row:
//   - a schedule running is answered as it is (Again), whatever req's zone,
//     and counts nothing against the hour's cap;
//   - otherwise a new schedule's first reminder goes at req.Schedule of the
//     address's last reminder (nil for an address with none), unless about
//     MaxNewPerHour schedules started in the past hour, which is
//     ErrFoundryReminderLimit. A new address gets a row; one whose schedule
//     ended (done, stopped or failed) starts again in place, sent from 0,
//     in req's zone and with req's consent.
//
// Two first requests of one address at once insert it once: the second
// waits on the first's row and answers its schedule. The cap is counted
// inside the transaction, but two transactions may count the same hour at
// once, so it is about MaxNewPerHour, never far past it.
func (s *mysqlDB) RequestFoundryReminder(ctx context.Context, req model.FoundryReminderRequest) (model.FoundryReminderAnswer, error) {
	if s.root == nil {
		return model.FoundryReminderAnswer{}, fmt.Errorf("foundry reminder request requires a root database connection")
	}
	if req.ID == "" || req.Email == "" || req.TimeZone == "" || req.Consent == "" || req.Schedule == nil || req.MaxNewPerHour < 1 {
		return model.FoundryReminderAnswer{}, fmt.Errorf("a reminder request needs its id, address, zone, consent, schedule and cap")
	}
	for attempt := 0; ; attempt++ {
		answer, err := s.requestFoundryReminder(ctx, req)
		// A first request of the address that another inserted first, or
		// one InnoDB rolled back to break a deadlock with it, is asked again:
		// it then finds the other's row.
		if attempt < 2 && (isDuplicateKey(err) || isDeadlock(err)) {
			continue
		}
		// MySQL's duplicate-key message quotes the address, which no error
		// of Foundry's does.
		if isDuplicateKey(err) {
			return answer, errors.New("schedule foundry reminder: other requests of the address kept racing this one")
		}
		return answer, err
	}
}

func (s *mysqlDB) requestFoundryReminder(ctx context.Context, req model.FoundryReminderRequest) (_ model.FoundryReminderAnswer, err error) {
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return model.FoundryReminderAnswer{}, fmt.Errorf("begin foundry reminder request: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	r, err := scanFoundryReminder(tx.QueryRowContext(ctx, "SELECT "+foundryReminderColumns+` FROM foundry_reminders WHERE email=? FOR UPDATE`,
		req.Email))
	switch {
	case errors.Is(err, ErrNotFound):
		r = nil
	case err != nil:
		return model.FoundryReminderAnswer{}, fmt.Errorf("read foundry reminder: %w", err)
	case r.Status == model.FoundryReminderScheduled && r.NextSendAt != nil:
		if err = tx.Commit(); err != nil {
			return model.FoundryReminderAnswer{}, fmt.Errorf("commit foundry reminder request: %w", err)
		}
		return model.FoundryReminderAnswer{NextSendAt: *r.NextSendAt, Again: true}, nil
	}
	var started int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_reminders WHERE requested_at >= NOW(6) - INTERVAL 1 HOUR`).
		Scan(&started); err != nil {
		return model.FoundryReminderAnswer{}, fmt.Errorf("count foundry reminders: %w", err)
	}
	if started >= req.MaxNewPerHour {
		_ = tx.Rollback()
		return model.FoundryReminderAnswer{}, ErrFoundryReminderLimit
	}
	var last *time.Time
	if r != nil {
		last = r.LastSentAt
	}
	next := req.Schedule(last).UTC()
	if r == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO foundry_reminders (id, email, time_zone, consent, next_send_at) VALUES (?, ?, ?, ?, ?)`,
			req.ID, req.Email, req.TimeZone, req.Consent, next)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE foundry_reminders SET status='scheduled', sent=0, next_send_at=?, attempts=0, last_error=NULL,
  time_zone=?, consent=?, requested_at=NOW(6), stopped_at=NULL, stop_reason=NULL WHERE id=?`, next, req.TimeZone, req.Consent, r.ID)
	}
	if err != nil {
		return model.FoundryReminderAnswer{}, fmt.Errorf("schedule foundry reminder: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return model.FoundryReminderAnswer{}, fmt.Errorf("commit foundry reminder request: %w", err)
	}
	return model.FoundryReminderAnswer{NextSendAt: next}, nil
}

// StopFoundryReminder stops email's schedule, if one is running, for
// reason (model.FoundryReminderStoppedSignedIn), and says whether it did.
// A reminder being sent as it stops may still go: its claim then ends
// nothing (FinishFoundryReminder's ErrNotFound).
func (s *mysqlDB) StopFoundryReminder(ctx context.Context, email, reason string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE foundry_reminders SET status='stopped', stopped_at=NOW(6), stop_reason=?, next_send_at=NULL,
  claim_token=NULL, claimed_until=NULL WHERE email=? AND status='scheduled'`, reason, email)
	if err != nil {
		return false, fmt.Errorf("stop foundry reminders: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ClaimFoundryReminder claims the scheduled reminder due first, for lease:
// one whose time has come that no claim holds, or whose claim lapsed (its
// worker stopped while sending it). Each claim counts an attempt and holds
// the row under token, a new one per claim, which only its holder can end
// it with (FinishFoundryReminder). nil means none is due.
func (s *mysqlDB) ClaimFoundryReminder(ctx context.Context, token string, lease time.Duration) (_ *model.FoundryReminder, err error) {
	if s.root == nil {
		return nil, fmt.Errorf("foundry reminder claim requires a root database connection")
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin foundry reminder claim: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	// A claimed reminder keeps the next_send_at it was due at, so lapsed
	// claims fall in the index's range too.
	r, err := scanFoundryReminder(tx.QueryRowContext(ctx, "SELECT "+foundryReminderColumns+` FROM foundry_reminders
WHERE status='scheduled' AND next_send_at <= NOW(6) AND (claimed_until IS NULL OR claimed_until < NOW(6))
ORDER BY next_send_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`))
	if errors.Is(err, ErrNotFound) {
		_ = tx.Rollback()
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select foundry reminder claim: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE foundry_reminders
SET attempts=attempts+1, claim_token=?, claimed_until=NOW(6) + INTERVAL ? MICROSECOND WHERE id=?`, token, lease.Microseconds(), r.ID); err != nil {
		return nil, fmt.Errorf("claim foundry reminder: %w", err)
	}
	if r, err = scanFoundryReminder(tx.QueryRowContext(ctx, "SELECT "+foundryReminderColumns+` FROM foundry_reminders WHERE id=?`, r.ID)); err != nil {
		return nil, fmt.Errorf("read claimed foundry reminder: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit foundry reminder claim: %w", err)
	}
	return r, nil
}

// FinishFoundryReminder ends token's claim of reminder id with outcome:
// sent, which counts it and schedules the next or ends the schedule, done;
// due again at a retry's time; or failed, ending the schedule. ErrNotFound
// means the claim was lost (it lapsed, and another claim holds the row or
// ended it, or the schedule was stopped) and nothing changed.
func (s *mysqlDB) FinishFoundryReminder(ctx context.Context, id, token string, outcome model.FoundryReminderOutcome) error {
	const claimed = `, claim_token=NULL, claimed_until=NULL WHERE id=? AND status='scheduled' AND claim_token=?`
	var query string
	var args []any
	switch o := outcome.(type) {
	case model.FoundryReminderSent:
		if o.Next == nil {
			query = `UPDATE foundry_reminders SET status='done', sent=sent+1, last_sent_at=NOW(6), next_send_at=NULL, attempts=0, last_error=NULL` +
				claimed
		} else {
			query, args = `UPDATE foundry_reminders SET sent=sent+1, last_sent_at=NOW(6), next_send_at=?, attempts=0, last_error=NULL`+claimed,
				[]any{o.Next.UTC()}
		}
	case model.FoundryReminderRetry:
		if o.Error == "" {
			return fmt.Errorf("a retried reminder needs its failure")
		}
		query, args = `UPDATE foundry_reminders SET next_send_at=?, last_error=?`+claimed, []any{o.At.UTC(), clipChars(o.Error, maxNotificationErrorChars)}
	case model.FoundryReminderGiveUp:
		if o.Error == "" {
			return fmt.Errorf("a failed reminder needs its failure")
		}
		query, args = `UPDATE foundry_reminders SET status='failed', next_send_at=NULL, last_error=?`+claimed,
			[]any{clipChars(o.Error, maxNotificationErrorChars)}
	default:
		return fmt.Errorf("foundry reminder outcome %T is not one FinishFoundryReminder takes", outcome)
	}
	return execOrNotFound(ctx, s.db, query, append(args, id, token)...)
}

func scanFoundryReminder(scanner rowScanner) (*model.FoundryReminder, error) {
	var r model.FoundryReminder
	var status string
	var next, claimedUntil, lastSent, stopped sql.NullTime
	var token, lastError, reason sql.NullString
	if err := scanner.Scan(&r.ID, &r.Email, &r.TimeZone, &r.Consent, &status, &r.Sent, &next, &r.Attempts, &token, &claimedUntil, &lastError,
		&r.RequestedAt, &r.CreatedAt, &lastSent, &stopped, &reason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	r.Status = model.FoundryReminderStatus(status)
	r.NextSendAt, r.ClaimedUntil, r.LastSentAt, r.StoppedAt = timePtr(next), timePtr(claimedUntil), timePtr(lastSent), timePtr(stopped)
	r.ClaimToken, r.LastError, r.StopReason = stringPtr(token), stringPtr(lastError), stringPtr(reason)
	r.RequestedAt, r.CreatedAt = r.RequestedAt.UTC(), r.CreatedAt.UTC()
	return &r, nil
}
