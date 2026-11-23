package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Review advice (migration 20261219000000): the graph worker's advisor asks
// the workspace's verify model about the cards of a current load the rules
// leave undecided (backend/reviewadvice), and stores what it picked.
//
// A load is owed advice (foundry_review_advice_loads, one row a load) as it
// becomes current (makeFoundryLoadCurrent, every path that makes one) and
// whenever its index stores cards (StoreFoundryReviewDocuments), 30 s on, so
// a burst of stores is one owe. A claim takes one due load SKIP LOCKED from
// that table alone, so a settle never waits on it, checks it with plain
// reads (gone: deleted; not current: due no more; not indexed yet: in a
// minute) and holds it for a lease (due_at). Renew, Finish, BackOff and
// Store act only while the caller holds its claim; a re-owe during a claim
// (owed_at past the claim's) wins over its Finish and BackOff, and extends
// its lease. Advice rows are kept until their load is no longer live
// (ClearFoundryReviewLoads): they were paid for, so a park keeps them.

const (
	// foundryReviewAdviceOwe is how long after an owe its load is due.
	foundryReviewAdviceOwe = 30 * time.Second
	// foundryReviewAdviceRecheck is when a load not indexed yet is looked at
	// again.
	foundryReviewAdviceRecheck = time.Minute
	// foundryReviewAdviceClaimRows is how many due loads one claim reads.
	foundryReviewAdviceClaimRows = 10
	// foundryReviewAdviceMaxBackoff caps a back-off.
	foundryReviewAdviceMaxBackoff = time.Hour
)

// oweFoundryReviewAdvice owes load loadID advice, in the caller's
// transaction: due 30 s on (a held lease or a later recheck stays), its stop
// and failed attempts forgotten.
func (s *mysqlDB) oweFoundryReviewAdvice(ctx context.Context, workspaceID, ontologyID, loadID string) error {
	owe := foundryReviewAdviceOwe.Microseconds()
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_review_advice_loads (load_id, workspace_id, ontology_id, due_at)
VALUES (?, ?, ?, NOW(6) + INTERVAL ? MICROSECOND)
ON DUPLICATE KEY UPDATE owed_at=NOW(6), due_at=GREATEST(COALESCE(due_at, NOW(6)), NOW(6) + INTERVAL ? MICROSECOND), stopped=NULL, attempts=0`,
		loadID, workspaceID, ontologyID, owe, owe)
	return err
}

// ClaimFoundryReviewAdvice claims a load due advice for workerID for lease:
// the earliest due of foundryReviewAdviceClaimRows rows no one else holds
// (SKIP LOCKED), checked with plain reads. A load or ontology gone loses its
// row, a load no longer current is due no more (it is owed again as it
// becomes current), and a load whose index isn't whole yet is looked at
// again in a minute. Nil when none is due.
func (s *mysqlDB) ClaimFoundryReviewAdvice(ctx context.Context, workerID string, lease time.Duration) (*model.FoundryReviewAdviceClaim, error) {
	var claim *model.FoundryReviewAdviceClaim
	err := s.WithTx(ctx, func(db Store) error {
		tx := db.(*mysqlDB)
		rows, err := tx.db.QueryContext(ctx, `SELECT load_id, workspace_id, ontology_id, owed_at, attempts FROM foundry_review_advice_loads
WHERE due_at <= NOW(6) ORDER BY due_at LIMIT ? FOR UPDATE SKIP LOCKED`, foundryReviewAdviceClaimRows)
		if err != nil {
			return err
		}
		due, err := scanAll(rows, func(r rowScanner) (model.FoundryReviewAdviceClaim, error) {
			var c model.FoundryReviewAdviceClaim
			return c, r.Scan(&c.LoadID, &c.WorkspaceID, &c.OntologyID, &c.OwedAt, &c.Attempts)
		})
		if err != nil {
			return err
		}
		for _, c := range due {
			var current, indexed bool
			err := tx.db.QueryRowContext(ctx, `SELECT o.current_load_id <=> l.id,
  EXISTS (SELECT 1 FROM foundry_review_loads r WHERE r.load_id=l.id AND `+foundryReviewIndexed("r")+`)
FROM foundry_loads l JOIN foundry_ontologies o ON o.id=l.ontology_id WHERE l.id=? AND o.deleted_at IS NULL`, c.LoadID).Scan(&current, &indexed)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				_, err = tx.db.ExecContext(ctx, `DELETE FROM foundry_review_advice_loads WHERE load_id=?`, c.LoadID)
			case err != nil:
			case !current:
				_, err = tx.db.ExecContext(ctx, `UPDATE foundry_review_advice_loads SET due_at=NULL, claimed_by=NULL WHERE load_id=?`, c.LoadID)
			case !indexed:
				_, err = tx.db.ExecContext(ctx, `UPDATE foundry_review_advice_loads SET due_at=NOW(6) + INTERVAL ? MICROSECOND, claimed_by=NULL
WHERE load_id=?`, foundryReviewAdviceRecheck.Microseconds(), c.LoadID)
			default:
				if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_review_advice_loads SET claimed_by=?, due_at=NOW(6) + INTERVAL ? MICROSECOND
WHERE load_id=?`, workerID, lease.Microseconds(), c.LoadID); err != nil {
					return err
				}
				claim = &c
				return nil
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	return claim, err
}

// heldFoundryReviewAdvice locks claim's row and reports whether workerID
// still holds it (its lease unexpired), and whether it was owed again since
// the claim.
func (s *mysqlDB) heldFoundryReviewAdvice(ctx context.Context, workerID string, claim model.FoundryReviewAdviceClaim) (held, owed bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT claimed_by <=> ? AND due_at > NOW(6), owed_at > ? FROM foundry_review_advice_loads
WHERE load_id=? FOR UPDATE`, workerID, claim.OwedAt, claim.LoadID).Scan(&held, &owed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	return held, owed, err
}

// withHeldFoundryReviewAdvice runs fn in a transaction while workerID holds
// claim, and reports whether it did.
func (s *mysqlDB) withHeldFoundryReviewAdvice(ctx context.Context, workerID string, claim model.FoundryReviewAdviceClaim,
	fn func(tx *mysqlDB, owed bool) error) (bool, error) {
	ran := false
	err := s.WithTx(ctx, func(db Store) error {
		tx := db.(*mysqlDB)
		held, owed, err := tx.heldFoundryReviewAdvice(ctx, workerID, claim)
		if err != nil || !held {
			return err
		}
		ran = true
		return fn(tx, owed)
	})
	return ran && err == nil, err
}

// releaseOwed ends a claim of a load owed again during it: due 30 s after
// that owe, its stop cleared.
const releaseOwed = `UPDATE foundry_review_advice_loads SET claimed_by=NULL, stopped=NULL,
  due_at=owed_at + INTERVAL ? MICROSECOND WHERE load_id=?`

// RenewFoundryReviewAdvice pushes workerID's lease of claim out to lease
// from now; false when the lease was lost.
func (s *mysqlDB) RenewFoundryReviewAdvice(ctx context.Context, workerID string, claim model.FoundryReviewAdviceClaim, lease time.Duration) (bool, error) {
	return s.withHeldFoundryReviewAdvice(ctx, workerID, claim, func(tx *mysqlDB, _ bool) error {
		_, err := tx.db.ExecContext(ctx, `UPDATE foundry_review_advice_loads SET due_at=GREATEST(due_at, NOW(6) + INTERVAL ? MICROSECOND)
WHERE load_id=?`, lease.Microseconds(), claim.LoadID)
		return err
	})
}

// FinishFoundryReviewAdvice ends workerID's claim, stopped for why: due
// again recheck from now (0: not until it is owed again). A load owed again
// during the claim is due 30 s after that owe instead, with no stop. False
// when the lease was lost.
func (s *mysqlDB) FinishFoundryReviewAdvice(ctx context.Context, workerID string, claim model.FoundryReviewAdviceClaim,
	why model.FoundryReviewAdviceStop, recheck time.Duration) (bool, error) {
	return s.withHeldFoundryReviewAdvice(ctx, workerID, claim, func(tx *mysqlDB, owed bool) error {
		var err error
		if owed {
			_, err = tx.db.ExecContext(ctx, releaseOwed, foundryReviewAdviceOwe.Microseconds(), claim.LoadID)
		} else {
			_, err = tx.db.ExecContext(ctx, `UPDATE foundry_review_advice_loads SET claimed_by=NULL, stopped=?,
  due_at=IF(? > 0, NOW(6) + INTERVAL ? MICROSECOND, NULL) WHERE load_id=?`, string(why), recheck.Microseconds(), recheck.Microseconds(), claim.LoadID)
		}
		return err
	})
}

// BackOffFoundryReviewAdvice ends workerID's claim after a failure: one
// more failed attempt, due 30 s × 2ⁿ on (n the attempts now), at most an
// hour. A load owed again during the claim is due 30 s after that owe
// instead. False when the lease was lost.
func (s *mysqlDB) BackOffFoundryReviewAdvice(ctx context.Context, workerID string, claim model.FoundryReviewAdviceClaim) (bool, error) {
	return s.withHeldFoundryReviewAdvice(ctx, workerID, claim, func(tx *mysqlDB, owed bool) error {
		if owed {
			_, err := tx.db.ExecContext(ctx, releaseOwed, foundryReviewAdviceOwe.Microseconds(), claim.LoadID)
			return err
		}
		wait := foundryReviewAdviceOwe
		for range min(claim.Attempts, 7) {
			wait *= 2
		}
		_, err := tx.db.ExecContext(ctx, `UPDATE foundry_review_advice_loads SET claimed_by=NULL, attempts=LEAST(attempts + 1, 60000),
  due_at=NOW(6) + INTERVAL ? MICROSECOND WHERE load_id=?`, min(wait, foundryReviewAdviceMaxBackoff).Microseconds(), claim.LoadID)
		return err
	})
}

// StoreFoundryReviewAdvice stores rows of cards of claim's load while
// workerID holds it, a card's earlier row kept, and forgets the load's
// failed attempts. False, and nothing written, when the lease was lost.
func (s *mysqlDB) StoreFoundryReviewAdvice(ctx context.Context, workerID string, claim model.FoundryReviewAdviceClaim,
	rows []model.FoundryReviewAdviceRow) (bool, error) {
	return s.withHeldFoundryReviewAdvice(ctx, workerID, claim, func(tx *mysqlDB, _ bool) error {
		for chunk := range slices.Chunk(rows, 100) {
			query := `INSERT INTO foundry_review_advice (load_id, card_id, outcome, offered, choice, rationale, cites, model, prompt_version) VALUES `
			args := make([]any, 0, 9*len(chunk))
			for i, r := range chunk {
				if i > 0 {
					query += ", "
				}
				query += "(?, ?, ?, ?, ?, ?, ?, ?, ?)"
				offered, choice, cites, err := adviceJSON(r)
				if err != nil {
					return err
				}
				args = append(args, claim.LoadID, r.CardID[:], string(r.Outcome), offered, choice, nullIfEmpty(r.Rationale), cites,
					nullIfEmpty(r.Model), nullIfEmpty(r.PromptVersion))
			}
			if _, err := tx.db.ExecContext(ctx, query+` ON DUPLICATE KEY UPDATE load_id=load_id`, args...); err != nil {
				return err
			}
		}
		_, err := tx.db.ExecContext(ctx, `UPDATE foundry_review_advice_loads SET attempts=0 WHERE load_id=?`, claim.LoadID)
		return err
	})
}

// adviceJSON is a row's JSON columns: offered for an asked card, choice and
// cites for an advised one, else NULL.
func adviceJSON(r model.FoundryReviewAdviceRow) (offered, choice, cites any, err error) {
	asJSON := func(v any) (any, error) {
		raw, err := json.Marshal(v)
		return string(raw), err
	}
	if r.Outcome == model.FoundryReviewAdvised || r.Outcome == model.FoundryReviewAdviceNone {
		if offered, err = asJSON(r.Offered); err != nil {
			return nil, nil, nil, err
		}
	}
	if r.Choice != nil {
		if choice, err = asJSON(r.Choice); err != nil {
			return nil, nil, nil, err
		}
		if cites, err = asJSON(r.Cites); err != nil {
			return nil, nil, nil, err
		}
	}
	return offered, choice, cites, nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// adviceMonth is the first of the month of at, in UTC: a spend's month.
func adviceMonth(at time.Time) string {
	at = at.UTC()
	return time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
}

// ChargeFoundryReviewAdvice adds usdMicros to workspaceID's advice spend
// this month (UTC).
func (s *mysqlDB) ChargeFoundryReviewAdvice(ctx context.Context, workspaceID string, usdMicros int64) error {
	if usdMicros < 0 {
		return fmt.Errorf("a negative charge of review advice: %d", usdMicros)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_review_advice_spend (workspace_id, month, usd_micros) VALUES (?, ?, ?)
ON DUPLICATE KEY UPDATE usd_micros=usd_micros + ?`, workspaceID, adviceMonth(time.Now()), usdMicros, usdMicros)
	return err
}

// SumFoundryReviewAdviceSpend is what workspaceID's advice spent in the
// month (UTC) of month.
func (s *mysqlDB) SumFoundryReviewAdviceSpend(ctx context.Context, workspaceID string, month time.Time) (int64, error) {
	var spent int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(usd_micros), 0) FROM foundry_review_advice_spend WHERE workspace_id=? AND month=?`,
		workspaceID, adviceMonth(month)).Scan(&spent)
	return spent, err
}
