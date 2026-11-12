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

// ErrFoundryReminderClientLimit is a request for reminders past
// foundry.reminders.max_new_per_client_per_hour: its client started that
// many schedules in the past hour.
var ErrFoundryReminderClientLimit = errors.New("store: this client asked for too many reminders in the past hour")

// RequestFoundryReminder starts the reminders req asks for, or answers the
// schedule its address already has:
//   - a schedule running is answered as it is (Again), whatever req's zone,
//     and counts nothing against the hour's cap;
//   - otherwise a new schedule's first reminder goes at req.Schedule of the
//     address's last reminder (nil for an address with none), unless about
//     MaxNewPerHour schedules started in the past hour, which is
//     ErrFoundryReminderLimit, or req's Client started
//     MaxNewPerClientPerHour, ErrFoundryReminderClientLimit. A new address
//     gets a row; one whose schedule ended (done, stopped or failed) starts
//     again in place, sent from 0, in req's zone, with req's consent and
//     client. A reminder still being sent from a stopped schedule counts as
//     its last, sent when its claim ends.
//
// Two first requests of one address at once insert it once: the second's
// insert fails on the first's row, and it answers that row's schedule. An
// ended schedule starts again under its row's lock, so once. The cap's
// count takes no lock, so requests counting the same hour at once may pass
// it together: it is about MaxNewPerHour, never far past it.
func (s *mysqlDB) RequestFoundryReminder(ctx context.Context, req model.FoundryReminderRequest) (model.FoundryReminderAnswer, error) {
	if s.root == nil {
		return model.FoundryReminderAnswer{}, fmt.Errorf("foundry reminder request requires a root database connection")
	}
	if req.ID == "" || req.Email == "" || req.TimeZone == "" || req.Consent == "" || req.Schedule == nil || req.MaxNewPerHour < 1 ||
		req.MaxNewPerClientPerHour < 1 {
		return model.FoundryReminderAnswer{}, fmt.Errorf("a reminder request needs its id, address, zone, consent, schedule and caps")
	}
	for attempt := 0; ; attempt++ {
		answer, err := s.requestFoundryReminder(ctx, req)
		// A first request of the address that another inserted first, or
		// one InnoDB rolled back to break a deadlock, is asked again: it then
		// finds the other's row.
		if attempt < 4 && (isDuplicateKey(err) || isDeadlock(err)) {
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

// requestFoundryReminder is one try of RequestFoundryReminder. It never reads
// a row that isn't there under a lock: a locking read of an address not yet
// stored takes a gap lock, which other first requests of the address share,
// and their inserts then wait on each other's gap locks until InnoDB breaks
// the deadlock. So an address asked for the first time is one INSERT on its
// own, which a racing first request fails with a duplicate key; only an
// address whose schedule ended is read under a lock, by its id, to start it
// again.
func (s *mysqlDB) requestFoundryReminder(ctx context.Context, req model.FoundryReminderRequest) (_ model.FoundryReminderAnswer, err error) {
	r, err := scanFoundryReminder(s.root.QueryRowContext(ctx, "SELECT "+foundryReminderColumns+` FROM foundry_reminders WHERE email=?`,
		req.Email))
	switch {
	case errors.Is(err, ErrNotFound):
		return s.insertFoundryReminder(ctx, req)
	case err != nil:
		return model.FoundryReminderAnswer{}, fmt.Errorf("read foundry reminder: %w", err)
	case r.Status == model.FoundryReminderScheduled && r.NextSendAt != nil:
		return model.FoundryReminderAnswer{NextSendAt: *r.NextSendAt, Again: true}, nil
	}

	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return model.FoundryReminderAnswer{}, fmt.Errorf("begin foundry reminder request: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	// The row exists: its primary key's record lock, and no gap.
	r, err = scanFoundryReminder(tx.QueryRowContext(ctx, "SELECT "+foundryReminderColumns+` FROM foundry_reminders WHERE id=? FOR UPDATE`,
		r.ID))
	if err != nil {
		return model.FoundryReminderAnswer{}, fmt.Errorf("read foundry reminder: %w", err)
	}
	// Another request started it again since the read above.
	if r.Status == model.FoundryReminderScheduled && r.NextSendAt != nil {
		if err = tx.Commit(); err != nil {
			return model.FoundryReminderAnswer{}, fmt.Errorf("commit foundry reminder request: %w", err)
		}
		return model.FoundryReminderAnswer{NextSendAt: *r.NextSendAt, Again: true}, nil
	}
	if err = s.underFoundryReminderLimit(ctx, tx, req); err != nil {
		return model.FoundryReminderAnswer{}, err
	}
	// A claim a stopped schedule still holds is a reminder that may have
	// gone since last_sent_at: its claim's end is when it went at the
	// latest, so the new schedule keeps its gap from then. The claim ends
	// here, so its worker records nothing in the new schedule.
	last := r.LastSentAt
	if r.ClaimedUntil != nil && (last == nil || r.ClaimedUntil.After(*last)) {
		last = r.ClaimedUntil
	}
	next := req.Schedule(last).UTC()
	if _, err = tx.ExecContext(ctx, `UPDATE foundry_reminders SET status='scheduled', sent=0, next_send_at=?, attempts=0, last_error=NULL,
  claim_token=NULL, claimed_until=NULL, time_zone=?, consent=?, client_key=?, requested_at=NOW(6), stopped_at=NULL, stop_reason=NULL
WHERE id=?`, next, req.TimeZone, req.Consent, nullString(req.Client), r.ID); err != nil {
		return model.FoundryReminderAnswer{}, fmt.Errorf("schedule foundry reminder: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return model.FoundryReminderAnswer{}, fmt.Errorf("commit foundry reminder request: %w", err)
	}
	return model.FoundryReminderAnswer{NextSendAt: next}, nil
}

// insertFoundryReminder starts the schedule of an address asked for the
// first time: one INSERT, outside any transaction. A first request of the
// same address that came first fails it with a duplicate key, which the
// caller asks again, finding that request's row.
func (s *mysqlDB) insertFoundryReminder(ctx context.Context, req model.FoundryReminderRequest) (model.FoundryReminderAnswer, error) {
	if err := s.underFoundryReminderLimit(ctx, s.root, req); err != nil {
		return model.FoundryReminderAnswer{}, err
	}
	next := req.Schedule(nil).UTC()
	if _, err := s.root.ExecContext(ctx, `INSERT INTO foundry_reminders (id, email, time_zone, consent, client_key, next_send_at)
VALUES (?, ?, ?, ?, ?, ?)`, req.ID, req.Email, req.TimeZone, req.Consent, nullString(req.Client), next); err != nil {
		return model.FoundryReminderAnswer{}, fmt.Errorf("schedule foundry reminder: %w", err)
	}
	return model.FoundryReminderAnswer{NextSendAt: next}, nil
}

// underFoundryReminderLimit is ErrFoundryReminderLimit once req's
// MaxNewPerHour schedules started, or started again, within the hour, and
// ErrFoundryReminderClientLimit once its Client started
// MaxNewPerClientPerHour. The counts aren't locked, so requests at the same
// moment may pass them together: the caps bound a flood, not the exact
// number.
func (s *mysqlDB) underFoundryReminderLimit(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, req model.FoundryReminderRequest) error {
	var started int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_reminders WHERE requested_at >= NOW(6) - INTERVAL 1 HOUR`).
		Scan(&started); err != nil {
		return fmt.Errorf("count foundry reminders: %w", err)
	}
	if started >= req.MaxNewPerHour {
		return ErrFoundryReminderLimit
	}
	if req.Client == "" {
		return nil
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_reminders WHERE client_key=? AND requested_at >= NOW(6) - INTERVAL 1 HOUR`,
		req.Client).Scan(&started); err != nil {
		return fmt.Errorf("count a client's foundry reminders: %w", err)
	}
	if started >= req.MaxNewPerClientPerHour {
		return ErrFoundryReminderClientLimit
	}
	return nil
}

// StopFoundryReminder stops email's schedule, if one is running, for
// reason (model.FoundryReminderStoppedSignedIn), and says whether it did.
// A reminder being sent as it stops may still go: the schedule keeps its
// claim, so FinishFoundryReminder still counts it sent.
func (s *mysqlDB) StopFoundryReminder(ctx context.Context, email, reason string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE foundry_reminders SET status='stopped', stopped_at=NOW(6), stop_reason=?, next_send_at=NULL
WHERE email=? AND status='scheduled'`, reason, email)
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
func (s *mysqlDB) ClaimFoundryReminder(ctx context.Context, token string, lease time.Duration) (*model.FoundryReminder, error) {
	// A claimed reminder keeps the next_send_at it was due at, so lapsed
	// claims fall in the index's range too.
	return claimDue(ctx, s.root, outbox[model.FoundryReminder]{
		what: "foundry reminder", table: "foundry_reminders", columns: foundryReminderColumns,
		due: `WHERE status='scheduled' AND next_send_at <= NOW(6) AND (claimed_until IS NULL OR claimed_until < NOW(6))
ORDER BY next_send_at, id`,
		scan: scanFoundryReminder, id: func(r *model.FoundryReminder) string { return r.ID },
	}, token, lease)
}

// FinishFoundryReminder ends token's claim of reminder id with outcome:
// sent, which counts it and schedules the next or ends the schedule, done;
// due again at a retry's time; or failed, ending the schedule. A schedule
// stopped while its reminder was sent stays stopped: a send is counted
// (sent, last_sent_at), so a new schedule keeps its gap from it, and any
// other outcome only ends the claim. ErrNotFound means the claim was lost
// (it lapsed, and another claim holds the row or ended it, or the schedule
// was started again) and nothing changed.
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
	err := execOrNotFound(ctx, s.db, query, append(args, id, token)...)
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	stopped := `UPDATE foundry_reminders SET claim_token=NULL, claimed_until=NULL WHERE id=? AND status='stopped' AND claim_token=?`
	if _, sent := outcome.(model.FoundryReminderSent); sent {
		stopped = `UPDATE foundry_reminders SET sent=sent+1, last_sent_at=NOW(6), claim_token=NULL, claimed_until=NULL
WHERE id=? AND status='stopped' AND claim_token=?`
	}
	return execOrNotFound(ctx, s.db, stopped, id, token)
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
