package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// outbox is a table of emails a worker claims one at a time and sends: the
// notifications' (foundry_notifications) and the reminders'
// (foundry_reminders). Each row has an id, attempts, and a claim
// (claim_token, claimed_until) that holds it while a worker sends it.
type outbox[T any] struct {
	// what names the row in errors, such as "foundry reminder".
	what, table, columns string
	// due is the WHERE and ORDER BY of the rows due, first first, which
	// leaves out those a live claim holds.
	due  string
	scan func(rowScanner) (*T, error)
	id   func(*T) string
}

// claimDue claims the row of box due first for lease under token, a new one
// per claim: it counts an attempt and holds the row until claimed_until,
// and only token's holder can end the claim. Rows other workers are
// claiming are skipped (SKIP LOCKED), so workers never wait on each other.
// nil means none is due.
func claimDue[T any](ctx context.Context, root *sql.DB, box outbox[T], token string, lease time.Duration) (_ *T, err error) {
	if root == nil {
		return nil, fmt.Errorf("%s claim requires a root database connection", box.what)
	}
	tx, err := root.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin %s claim: %w", box.what, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	row, err := box.scan(tx.QueryRowContext(ctx, "SELECT "+box.columns+" FROM "+box.table+"\n"+box.due+" LIMIT 1 FOR UPDATE SKIP LOCKED"))
	if errors.Is(err, ErrNotFound) {
		_ = tx.Rollback()
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select %s claim: %w", box.what, err)
	}
	if _, err = tx.ExecContext(ctx, "UPDATE "+box.table+`
SET attempts=attempts+1, claim_token=?, claimed_until=NOW(6) + INTERVAL ? MICROSECOND WHERE id=?`, token, lease.Microseconds(), box.id(row)); err != nil {
		return nil, fmt.Errorf("claim %s: %w", box.what, err)
	}
	if row, err = box.scan(tx.QueryRowContext(ctx, "SELECT "+box.columns+" FROM "+box.table+" WHERE id=?", box.id(row))); err != nil {
		return nil, fmt.Errorf("read claimed %s: %w", box.what, err)
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit %s claim: %w", box.what, err)
	}
	return row, nil
}
