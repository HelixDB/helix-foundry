package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// ErrWorkspaceDeleting means the workspace is deleted or being deleted: a
// sign-in that finishes then makes no connector.
var ErrWorkspaceDeleting = errors.New("store: the workspace is being deleted")

// SignInInProgressError is another user's sign-in of the same connector,
// not yet ended: UserID started it.
type SignInInProgressError struct {
	UserID string
}

func (e *SignInInProgressError) Error() string {
	return "store: user " + e.UserID + " is signing this connector in"
}

// newID is a fresh row id, for statements whose arguments shadow package id.
func newID() string { return id.New() }

const connectorSignInColumns = `id, workspace_id, kind, name, config, connector_id, status, failure, discovery_problem,
  identity_changed, verifier_ciphertext, verifier_key_id, account_id, created_by_user_id, client_token, created_at, updated_at,
  expires_at`

// liveSignIn is a sign-in that hasn't ended: not connected and not past its
// expiry.
const liveSignIn = `status <> 'connected' AND expires_at > NOW(6)`

// StartConnectorSignIn stores start.SignIn pending, expiring start.TTL from now, in
// one transaction that holds the workspace's lock, and returns it as stored,
// or what stands in its way:
//   - the sign-in the same user started in the workspace with the same
//     client token is returned as it is, whatever its state;
//   - signing a connector in again locks it (ErrNotFound when it is deleted):
//     its live sign-in by the same user is returned, and one by another user
//     is a *SignInInProgressError;
//   - ErrLimitExceeded when the user has start.MaxLive live sign-ins in the
//     workspace, or started start.MaxPerHour in the last hour in any.
//
// The workspace's lock makes the caps exact within it.
func (s *mysqlDB) StartConnectorSignIn(ctx context.Context, start model.ConnectorSignInStart) (*model.ConnectorSignIn, error) {
	si := start.SignIn
	var out *model.ConnectorSignIn
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.workspaces.Lock(ctx, tx.db, si.WorkspaceID); err != nil {
			return err
		}
		if si.ClientToken != nil {
			existing, err := scanConnectorSignIn(tx.db.QueryRowContext(ctx, "SELECT "+connectorSignInColumns+`
FROM foundry_connector_sign_ins WHERE workspace_id=? AND created_by_user_id=? AND client_token=?`,
				si.WorkspaceID, si.CreatedByUserID, *si.ClientToken))
			if err == nil {
				out = existing
				return nil
			}
			if !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("read sign-in by client token: %w", err)
			}
		}
		if si.ConnectorID != nil {
			var locked string
			err := tx.db.QueryRowContext(ctx, `SELECT id FROM connectors WHERE id=? AND workspace_id=? AND deleted_at IS NULL FOR UPDATE`,
				*si.ConnectorID, si.WorkspaceID).Scan(&locked)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("lock connector %s: %w", *si.ConnectorID, err)
			}
			existing, err := scanConnectorSignIn(tx.db.QueryRowContext(ctx, "SELECT "+connectorSignInColumns+`
FROM foundry_connector_sign_ins WHERE workspace_id=? AND connector_id=? AND `+liveSignIn+`
ORDER BY created_at DESC, id DESC LIMIT 1`, si.WorkspaceID, *si.ConnectorID))
			switch {
			case err == nil && existing.CreatedByUserID == si.CreatedByUserID:
				out = existing
				return nil
			case err == nil:
				return &SignInInProgressError{UserID: existing.CreatedByUserID}
			case !errors.Is(err, ErrNotFound):
				return fmt.Errorf("read connector %s sign-ins: %w", *si.ConnectorID, err)
			}
		}
		var live, lastHour int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_connector_sign_ins
WHERE workspace_id=? AND created_by_user_id=? AND `+liveSignIn, si.WorkspaceID, si.CreatedByUserID).Scan(&live); err != nil {
			return fmt.Errorf("count live sign-ins: %w", err)
		}
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_connector_sign_ins
WHERE created_by_user_id=? AND created_at > NOW(6) - INTERVAL 1 HOUR`, si.CreatedByUserID).Scan(&lastHour); err != nil {
			return fmt.Errorf("count recent sign-ins: %w", err)
		}
		if live >= start.MaxLive || lastHour >= start.MaxPerHour {
			return ErrLimitExceeded
		}
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_connector_sign_ins
  (id, workspace_id, kind, name, config, connector_id, status, created_by_user_id, client_token, expires_at)
VALUES (?, ?, ?, ?, CAST(? AS JSON), ?, 'pending', ?, ?, NOW(6) + INTERVAL ? MICROSECOND)`,
			si.ID, si.WorkspaceID, si.Kind, si.Name, string(si.Config), si.ConnectorID, si.CreatedByUserID, si.ClientToken,
			start.TTL.Microseconds()); err != nil {
			return fmt.Errorf("insert sign-in: %w", err)
		}
		var err error
		out, err = tx.GetConnectorSignIn(ctx, si.WorkspaceID, si.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *mysqlDB) GetConnectorSignIn(ctx context.Context, workspaceID, id string) (*model.ConnectorSignIn, error) {
	return scanConnectorSignIn(s.db.QueryRowContext(ctx, "SELECT "+connectorSignInColumns+`
FROM foundry_connector_sign_ins WHERE id=? AND workspace_id=?`, id, workspaceID))
}

// SetConnectorSignInVerifier keeps the sign-in's PKCE verifier, sealed, for
// every authorization of it. ErrNotFound means it has one already, which the
// caller reads again and uses.
func (s *mysqlDB) SetConnectorSignInVerifier(ctx context.Context, workspaceID, id string, ciphertext []byte, keyID string) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_connector_sign_ins SET verifier_ciphertext=?, verifier_key_id=?
WHERE id=? AND workspace_id=? AND verifier_ciphertext IS NULL`, ciphertext, keyID, id, workspaceID)
}

// AuthorizeConnectorSignIn moves a sign-in that may authorize to authorizing,
// expiring ttl from now: one not expired that is pending, authorizing,
// declined or failed, or finishing since more than stale ago, a finish that
// never ended. Such a finish may have left the broker's account on the row:
// it is purged, unless a live connector reads with it, and forgotten in the
// same transaction. ErrNotFound means it may not authorize.
func (s *mysqlDB) AuthorizeConnectorSignIn(ctx context.Context, workspaceID, id string, ttl, stale time.Duration) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var kind, userID string
		var accountID sql.NullString
		err := tx.db.QueryRowContext(ctx, `SELECT kind, account_id, created_by_user_id FROM foundry_connector_sign_ins
WHERE id=? AND workspace_id=? AND expires_at > NOW(6)
  AND (status IN ('pending', 'authorizing', 'declined', 'failed') OR status = 'finishing' AND updated_at < NOW(6) - INTERVAL ? MICROSECOND)
FOR UPDATE`, id, workspaceID, stale.Microseconds()).Scan(&kind, &accountID, &userID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock sign-in %s: %w", id, err)
		}
		if accountID.Valid {
			if err := tx.purgeUnusedAccount(ctx, workspaceID, kind, accountID.String, userID); err != nil {
				return err
			}
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_connector_sign_ins
SET status='authorizing', failure=NULL, account_id=NULL, expires_at=NOW(6) + INTERVAL ? MICROSECOND WHERE id=?`,
			ttl.Microseconds(), id); err != nil {
			return fmt.Errorf("authorize sign-in %s: %w", id, err)
		}
		return nil
	})
}

// ClaimConnectorSignIn moves an authorizing sign-in that hasn't expired to
// finishing, once: the claimant alone ends it. ErrNotFound means it wasn't
// authorizing, or has expired.
func (s *mysqlDB) ClaimConnectorSignIn(ctx context.Context, workspaceID, id string) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_connector_sign_ins SET status='finishing'
WHERE id=? AND workspace_id=? AND status='authorizing' AND expires_at > NOW(6)`, id, workspaceID)
}

// SetConnectorSignInAccount records the account the broker saved for a
// finishing sign-in, at once, so that one the finish never uses is purged.
func (s *mysqlDB) SetConnectorSignInAccount(ctx context.Context, workspaceID, id, accountID string) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_connector_sign_ins SET account_id=?
WHERE id=? AND workspace_id=? AND status='finishing'`, accountID, id, workspaceID)
}

// FailConnectorSignIn ends a finishing sign-in declined or failed. The
// broker's account it holds, and one the broker refused (end.Refused), are
// purged, unless a live connector reads with them (signing in again gave
// the connector its own account back), in the same transaction; the one it
// holds is forgotten. ErrNotFound means it isn't finishing.
func (s *mysqlDB) FailConnectorSignIn(ctx context.Context, workspaceID, id string, end model.ConnectorSignInEnd) error {
	switch {
	case end.Status == model.ConnectorSignInDeclined && end.Failure == "":
	case end.Status == model.ConnectorSignInFailed && end.Failure != "":
	default:
		return fmt.Errorf("sign-in %s: %q with failure %q is no end of a finish", id, end.Status, end.Failure)
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var kind, userID string
		var accountID sql.NullString
		err := tx.db.QueryRowContext(ctx, `SELECT kind, account_id, created_by_user_id FROM foundry_connector_sign_ins
WHERE id=? AND workspace_id=? AND status='finishing' FOR UPDATE`, id, workspaceID).Scan(&kind, &accountID, &userID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock sign-in %s: %w", id, err)
		}
		if accountID.Valid {
			if err := tx.purgeUnusedAccount(ctx, workspaceID, kind, accountID.String, userID); err != nil {
				return err
			}
		}
		if r := end.Refused; r != nil && r.ID != "" {
			refusedUser := r.UserID
			if refusedUser == "" {
				refusedUser = userID
			}
			if err := tx.purgeUnusedAccount(ctx, workspaceID, kind, r.ID, refusedUser); err != nil {
				return err
			}
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_connector_sign_ins SET status=?, failure=NULLIF(?, ''), account_id=NULL
WHERE id=?`, string(end.Status), string(end.Failure), id); err != nil {
			return fmt.Errorf("end sign-in %s: %w", id, err)
		}
		return nil
	})
}

// ConnectConnectorSignIn ends a finishing sign-in connected, in one
// transaction:
//   - a workspace being deleted is refused (ErrWorkspaceDeleting);
//   - a new connector is created with its account, due to sync now, as
//     CreateConnector's caller schedules one;
//   - signing in again locks the connector (ErrNotFound when it was deleted)
//     and gives it the account: a generation more, no longer broken, due to
//     sync now, and with IdentityChanged its change cursor forgotten, so the
//     sync reads in full. An account it no longer reads with is purged;
//   - the sign-in is connected, naming the connector.
//
// ErrNotFound also means the sign-in isn't finishing.
func (s *mysqlDB) ConnectConnectorSignIn(ctx context.Context, c model.ConnectorSignInConnect) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		deleting, err := tx.workspaces.Deleting(ctx, tx.db, c.WorkspaceID)
		if errors.Is(err, ErrNotFound) || err == nil && deleting {
			return ErrWorkspaceDeleting
		}
		if err != nil {
			return fmt.Errorf("read workspace %s: %w", c.WorkspaceID, err)
		}
		var locked string
		err = tx.db.QueryRowContext(ctx, `SELECT id FROM foundry_connector_sign_ins WHERE id=? AND workspace_id=? AND status='finishing' FOR UPDATE`,
			c.SignInID, c.WorkspaceID).Scan(&locked)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock sign-in %s: %w", c.SignInID, err)
		}
		connectorID := c.ConnectorID
		if c.New != nil {
			if c.New.Account == nil {
				return fmt.Errorf("sign-in %s: its connector has no account", c.SignInID)
			}
			if err := tx.CreateConnector(ctx, c.New); err != nil {
				return fmt.Errorf("create connector: %w", err)
			}
			connectorID = c.New.ID
		} else if err := tx.signInAgain(ctx, c); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_connector_sign_ins
SET status='connected', connector_id=?, identity_changed=?, discovery_problem=NULLIF(?, '') WHERE id=?`,
			connectorID, c.IdentityChanged, c.DiscoveryProblem, c.SignInID); err != nil {
			return fmt.Errorf("connect sign-in %s: %w", c.SignInID, err)
		}
		return nil
	})
}

// signInAgain gives a live connector of the workspace the account c names,
// under its lock, and purges the account it read with before when that
// differs.
func (s *mysqlDB) signInAgain(ctx context.Context, c model.ConnectorSignInConnect) error {
	var kind string
	var oldAccount, oldUser sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT kind, account_id, account_user_id FROM connectors
WHERE id=? AND workspace_id=? AND deleted_at IS NULL FOR UPDATE`, c.ConnectorID, c.WorkspaceID).Scan(&kind, &oldAccount, &oldUser)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock connector %s: %w", c.ConnectorID, err)
	}
	a := c.Account
	if _, err := s.db.ExecContext(ctx, `UPDATE connectors
SET account_id=?, account_user_id=?, account_label=?, account_identity=?, account_generation=account_generation + 1,
  account_broken=NULL, account_broken_at=NULL, account_checked_at=NOW(6), account_connected_at=NOW(6), next_sync_at=NOW(6),
  schedule_paused=NULL, change_cursor=IF(?, NULL, change_cursor), updated_at=NOW(6)
WHERE id=?`, a.ID, a.UserID, a.Label, nullString(a.Identity), c.IdentityChanged, c.ConnectorID); err != nil {
		return fmt.Errorf("sign connector %s in again: %w", c.ConnectorID, err)
	}
	if oldAccount.Valid && oldAccount.String != a.ID {
		return s.purgeUnusedAccount(ctx, c.WorkspaceID, kind, oldAccount.String, oldUser.String)
	}
	return nil
}

// purgeUnusedAccount queues the broker's account of kind for deleting as
// userID, unless a live connector of the workspace reads with it. An account
// already queued stays as it is.
func (s *mysqlDB) purgeUnusedAccount(ctx context.Context, workspaceID, kind, accountID, userID string) error {
	if _, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_connector_account_purges (id, workspace_id, kind, account_id, user_id)
SELECT ?, ?, ?, ?, ? FROM DUAL WHERE NOT EXISTS (
  SELECT 1 FROM connectors WHERE workspace_id=? AND kind=? AND account_id=? AND deleted_at IS NULL)`,
		newID(), workspaceID, kind, accountID, userID, workspaceID, kind, accountID); err != nil {
		return fmt.Errorf("purge account %s: %w", accountID, err)
	}
	return nil
}

// DeleteEndedConnectorSignIns deletes up to limit sign-ins that expired more
// than after ago, oldest first, and says how many. The broker's account one
// holds without having connected (its finish failed or never ended) is
// purged, unless a live connector reads with it, in the same transaction.
func (s *mysqlDB) DeleteEndedConnectorSignIns(ctx context.Context, after time.Duration, limit int) (int, error) {
	deleted := 0
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		rows, err := tx.db.QueryContext(ctx, `SELECT id, workspace_id, kind, account_id, created_by_user_id, status
FROM foundry_connector_sign_ins WHERE expires_at < NOW(6) - INTERVAL ? MICROSECOND ORDER BY expires_at, id LIMIT ? FOR UPDATE`,
			after.Microseconds(), limit)
		if err != nil {
			return fmt.Errorf("list ended sign-ins: %w", err)
		}
		type ended struct {
			id, workspaceID, kind, userID, status string
			accountID                             sql.NullString
		}
		var all []ended
		for rows.Next() {
			var e ended
			if err := rows.Scan(&e.id, &e.workspaceID, &e.kind, &e.accountID, &e.userID, &e.status); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan ended sign-in: %w", err)
			}
			all = append(all, e)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("list ended sign-ins: %w", err)
		}
		ids := make([]string, 0, len(all))
		for _, e := range all {
			if e.accountID.Valid && e.status != string(model.ConnectorSignInConnected) {
				if err := tx.purgeUnusedAccount(ctx, e.workspaceID, e.kind, e.accountID.String, e.userID); err != nil {
					return err
				}
			}
			ids = append(ids, e.id)
		}
		if len(ids) == 0 {
			return nil
		}
		res, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_connector_sign_ins WHERE id IN (`+idPlaceholders(len(ids))+`)`,
			toAnySlice(ids)...)
		if err != nil {
			return fmt.Errorf("delete ended sign-ins: %w", err)
		}
		n, _ := res.RowsAffected()
		deleted = int(n)
		return nil
	})
	return deleted, err
}

func scanConnectorSignIn(scanner rowScanner) (*model.ConnectorSignIn, error) {
	var si model.ConnectorSignIn
	var config, status string
	var connectorID, failure, problem, keyID, accountID, clientToken sql.NullString
	if err := scanner.Scan(&si.ID, &si.WorkspaceID, &si.Kind, &si.Name, &config, &connectorID, &status, &failure, &problem,
		&si.IdentityChanged, &si.VerifierCiphertext, &keyID, &accountID, &si.CreatedByUserID, &clientToken, &si.CreatedAt,
		&si.UpdatedAt, &si.ExpiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	si.Config, si.Status = []byte(config), model.ConnectorSignInStatus(status)
	si.ConnectorID, si.DiscoveryProblem, si.VerifierKeyID = stringPtr(connectorID), stringPtr(problem), stringPtr(keyID)
	si.AccountID, si.ClientToken = stringPtr(accountID), stringPtr(clientToken)
	if failure.Valid {
		f := model.ConnectorSignInFailure(failure.String)
		si.Failure = &f
	}
	return &si, nil
}
