package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// accountBrokenSync is the error a sync queued for a connector whose account
// broke ends with: it never runs.
const accountBrokenSync = "cancelled: the account it reads with needs signing in again"

// ListConnectorAccounts lists the broker accounts the workspace holds: those
// its live connectors read with, those waiting to be purged (a deleted
// connector's), and those its sign-ins hold without having connected. Each
// account once, ConnectorID set for a live connector's.
func (s *mysqlDB) ListConnectorAccounts(ctx context.Context, workspaceID string) ([]model.ConnectorAccountRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, workspace_id, kind, account_id, account_user_id FROM connectors
WHERE workspace_id=? AND account_id IS NOT NULL AND deleted_at IS NULL
UNION ALL
SELECT '', workspace_id, kind, account_id, user_id FROM foundry_connector_account_purges WHERE workspace_id=?
UNION ALL
SELECT '', workspace_id, kind, account_id, created_by_user_id FROM foundry_connector_sign_ins
WHERE workspace_id=? AND account_id IS NOT NULL AND status <> 'connected'`, workspaceID, workspaceID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list connector accounts: %w", err)
	}
	defer rows.Close()
	var out []model.ConnectorAccountRef
	for rows.Next() {
		var a model.ConnectorAccountRef
		var userID sql.NullString
		if err := rows.Scan(&a.ConnectorID, &a.WorkspaceID, &a.Kind, &a.AccountID, &userID); err != nil {
			return nil, fmt.Errorf("scan connector account: %w", err)
		}
		a.UserID = userID.String
		if !slices.ContainsFunc(out, func(b model.ConnectorAccountRef) bool { return b.Kind == a.Kind && b.AccountID == a.AccountID }) {
			out = append(out, a)
		}
	}
	return out, rows.Err()
}

// BreakConnectorAccounts marks every live connector of the workspace whose
// account acts as userID, and isn't broken already, as needing signing in
// again for reason: it stops syncing (breakAccounts). A host calls it in the
// transaction that removes the member (InTx); otherwise it runs in one of
// its own.
func (s *mysqlDB) BreakConnectorAccounts(ctx context.Context, workspaceID, userID, reason string) error {
	if !slices.Contains(model.ConnectorAccountReasons, reason) {
		return fmt.Errorf("break connector accounts: %q is no reason", reason)
	}
	return s.inTx(ctx, func(tx *mysqlDB) error {
		rows, err := tx.db.QueryContext(ctx, `SELECT id FROM connectors
WHERE workspace_id=? AND account_user_id=? AND account_id IS NOT NULL AND account_broken IS NULL AND deleted_at IS NULL
ORDER BY id FOR UPDATE`, workspaceID, userID)
		if err != nil {
			return fmt.Errorf("lock connectors of user %s: %w", userID, err)
		}
		ids, err := scanStrings(rows)
		if err != nil {
			return fmt.Errorf("lock connectors of user %s: %w", userID, err)
		}
		return tx.breakAccounts(ctx, ids, reason)
	})
}

// BreakConnectorAccount marks one connector's account as needing signing in
// again for reason, as BreakConnectorAccounts does, only while the account
// is live, not broken already, and its generation is still generation: a
// sync that asked for a token before the connector was signed in again
// can't break the account it reads with now. account_missing alone replaces
// a reason recorded before, keeping when the account broke: only a new
// account fixes a lost one, so signing in again must make one. It reports
// whether it broke it.
func (s *mysqlDB) BreakConnectorAccount(ctx context.Context, workspaceID, connectorID string, generation int, reason string) (bool, error) {
	if !slices.Contains(model.ConnectorAccountReasons, reason) {
		return false, fmt.Errorf("break connector account: %q is no reason", reason)
	}
	broke := false
	err := s.inTx(ctx, func(tx *mysqlDB) error {
		var locked string
		err := tx.db.QueryRowContext(ctx, `SELECT id FROM connectors
WHERE id=? AND workspace_id=? AND account_id IS NOT NULL AND account_generation=? AND deleted_at IS NULL
  AND (account_broken IS NULL OR ? = 'account_missing' AND account_broken <> 'account_missing')
FOR UPDATE`, connectorID, workspaceID, generation, reason).Scan(&locked)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock connector %s: %w", connectorID, err)
		}
		broke = true
		return tx.breakAccounts(ctx, []string{connectorID}, reason)
	})
	return broke, err
}

// breakAccounts marks the connectors' accounts broken for reason (when they
// weren't broken already, now), and
// stops them syncing: none is scheduled, and each queued sync ends failed
// without running. A running sync goes on, but schedules nothing when it
// ends (FinishConnectorSync), and its next attempt is refused before it
// asks for a token. The caller holds the connectors' locks.
func (s *mysqlDB) breakAccounts(ctx context.Context, ids []string, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	in, args := idPlaceholders(len(ids)), toAnySlice(ids)
	if _, err := s.db.ExecContext(ctx, `UPDATE connectors SET account_broken=?, account_broken_at=COALESCE(account_broken_at, NOW(6)), next_sync_at=NULL
WHERE id IN (`+in+`)`, append([]any{reason}, args...)...); err != nil {
		return fmt.Errorf("break connector accounts: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE connector_syncs
SET status='failed', error=?, active_key=NULL, lease_owner=NULL, lease_until=NULL, finished_at=NOW(6)
WHERE connector_id IN (`+in+`) AND status='queued'`, append([]any{accountBrokenSync}, args...)...); err != nil {
		return fmt.Errorf("cancel syncs of broken accounts: %w", err)
	}
	return nil
}

// inTx runs fn in a transaction of its own, or, inside a host's (InTx), in
// that one.
func (s *mysqlDB) inTx(ctx context.Context, fn func(*mysqlDB) error) error {
	if s.root == nil {
		return fn(s)
	}
	return s.WithTx(ctx, func(txDB Store) error { return fn(txDB.(*mysqlDB)) })
}

// ListConnectorAccountChecks lists up to limit live connectors whose
// accounts, not broken, the broker last vended a token for before before,
// least recently first: the weekly check asks for one, which renews what the
// broker holds and finds an account that broke outside Foundry.
func (s *mysqlDB) ListConnectorAccountChecks(ctx context.Context, before time.Time, limit int) ([]*model.Connector, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+connectorColumns+`
FROM connectors WHERE account_checked_at < ? AND account_id IS NOT NULL AND account_broken IS NULL AND deleted_at IS NULL
ORDER BY account_checked_at, id LIMIT ?`, before.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("list connector account checks: %w", err)
	}
	defer rows.Close()
	var out []*model.Connector
	for rows.Next() {
		c, err := scanConnector(rows)
		if err != nil {
			return nil, fmt.Errorf("scan connector account check: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkConnectorAccountChecked records that the broker vended a token for the
// connector's account now. The connector's updated_at stays.
func (s *mysqlDB) MarkConnectorAccountChecked(ctx context.Context, workspaceID, connectorID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE connectors SET account_checked_at=NOW(6), updated_at=updated_at
WHERE id=? AND workspace_id=? AND account_id IS NOT NULL`, connectorID, workspaceID)
	if err != nil {
		return fmt.Errorf("mark connector %s account checked: %w", connectorID, err)
	}
	return nil
}

// DeferConnectorAccountCheck records a weekly check that got no token for
// the connector's account: its check time moves on to checkedAt (never
// back), so it falls due again once checkedAt is a check interval ago, and
// the accounts after it are checked meanwhile. The weekly check sets it so
// the next comes an hour later.
func (s *mysqlDB) DeferConnectorAccountCheck(ctx context.Context, workspaceID, connectorID string, checkedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE connectors SET account_checked_at=GREATEST(COALESCE(account_checked_at, ?), ?), updated_at=updated_at
WHERE id=? AND workspace_id=? AND account_id IS NOT NULL`, checkedAt.UTC(), checkedAt.UTC(), connectorID, workspaceID)
	if err != nil {
		return fmt.Errorf("defer connector %s account check: %w", connectorID, err)
	}
	return nil
}

// ListConnectorAccountPurges lists up to limit broker accounts whose purge is
// due, longest due first.
func (s *mysqlDB) ListConnectorAccountPurges(ctx context.Context, limit int) ([]*model.ConnectorAccountPurge, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, workspace_id, kind, account_id, user_id, attempts, created_at
FROM foundry_connector_account_purges WHERE next_attempt_at <= NOW(6) ORDER BY next_attempt_at, id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list connector account purges: %w", err)
	}
	defer rows.Close()
	var out []*model.ConnectorAccountPurge
	for rows.Next() {
		var p model.ConnectorAccountPurge
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.Kind, &p.AccountID, &p.UserID, &p.Attempts, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan connector account purge: %w", err)
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

// ConnectorAccountPurgeBacklog is how many broker accounts wait to be purged,
// and when the oldest was queued; nil when none waits.
func (s *mysqlDB) ConnectorAccountPurgeBacklog(ctx context.Context) (int, *time.Time, error) {
	var n int
	var oldest sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(created_at) FROM foundry_connector_account_purges`).Scan(&n, &oldest); err != nil {
		return 0, nil, fmt.Errorf("read connector account purge backlog: %w", err)
	}
	return n, timePtr(oldest), nil
}

// DeleteConnectorAccountPurge forgets a purge the broker confirmed.
func (s *mysqlDB) DeleteConnectorAccountPurge(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_connector_account_purges WHERE id=?`, id); err != nil {
		return fmt.Errorf("forget connector account purge %s: %w", id, err)
	}
	return nil
}

// RetryConnectorAccountPurge counts a failed attempt of a purge, keeps why,
// and makes it due again after.
func (s *mysqlDB) RetryConnectorAccountPurge(ctx context.Context, id, lastError string, after time.Duration) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_connector_account_purges
SET attempts=attempts + 1, last_error=?, next_attempt_at=NOW(6) + INTERVAL ? MICROSECOND WHERE id=?`,
		clipChars(lastError, 512), after.Microseconds(), id); err != nil {
		return fmt.Errorf("retry connector account purge %s: %w", id, err)
	}
	return nil
}
