package directory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The install's link to Helix (migrations/00004_slack_relay.sql), which the
// owner opts in to: once linked, Slack reaches the install through Helix's
// relay. The store keeps what the relay component seals (the hx_rl_ token,
// and a link's PKCE verifier while it is made); it never sees them in the
// clear.

// RelayPendingTTL is how long a link the owner started waits for Helix to
// hand its code back.
const RelayPendingTTL = 10 * time.Minute

// RelayLink is the install's link to Helix.
type RelayLink struct {
	// HostedOrigin is Helix's site and door the link was made with: an
	// https origin, or http on a loopback address (development).
	HostedOrigin string
	// LinkID is the link's id at Helix.
	LinkID string
	// TokenSealed is its hx_rl_ token, sealed under KeyID.
	TokenSealed []byte
	KeyID       string
	// AccountEmail is the Helix account that authorized it.
	AccountEmail string
	// SlackApprovals says the owner's Slack account may approve actions
	// through Slack; off, the relay only reads and asks.
	SlackApprovals bool
	LinkedAt       time.Time
	// LastOKAt is the last claim Helix answered; nil before the first.
	LastOKAt *time.Time
}

// validate refuses a link the table can't keep, or that names no origin
// Helix could answer from.
func (l RelayLink) validate() error {
	u, err := url.Parse(l.HostedOrigin)
	switch {
	case err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || len(l.HostedOrigin) > 255 ||
		u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")):
		return fmt.Errorf("a relay link's Helix origin is an https origin, at most 255 bytes")
	case len(l.LinkID) != 26:
		return fmt.Errorf("a relay link's id is Helix's, 26 characters")
	case len(l.TokenSealed) == 0 || len(l.TokenSealed) > 512 || l.KeyID == "" || len(l.KeyID) > 64:
		return fmt.Errorf("a relay link's token is sealed under a key, at most 512 bytes")
	case l.AccountEmail == "" || len(l.AccountEmail) > 320:
		return fmt.Errorf("a relay link names the Helix account that made it, at most 320 bytes")
	}
	return nil
}

const relayLinkColumns = `hosted_origin, link_id, token_sealed, key_id, account_email, slack_approvals, linked_at, last_ok_at`

// RelayLink returns the install's link: store.ErrNotFound while unlinked.
func (s *Store) RelayLink(ctx context.Context) (RelayLink, error) {
	var l RelayLink
	var ok sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT `+relayLinkColumns+` FROM foundry_local_relay WHERE id=1`).
		Scan(&l.HostedOrigin, &l.LinkID, &l.TokenSealed, &l.KeyID, &l.AccountEmail, &l.SlackApprovals, &l.LinkedAt, &ok)
	if errors.Is(err, sql.ErrNoRows) {
		return RelayLink{}, store.ErrNotFound
	}
	if ok.Valid {
		l.LastOKAt = &ok.Time
	}
	return l, err
}

// LinkRelay makes l the install's link, linked now, replacing any before
// (whose token stops being read here; Helix revokes it on its side). Its
// approvals through Slack start on, as l says.
func (s *Store) LinkRelay(ctx context.Context, l RelayLink) error {
	if err := l.validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `REPLACE INTO foundry_local_relay
  (id, hosted_origin, link_id, token_sealed, key_id, account_email, slack_approvals, linked_at, last_ok_at)
VALUES (1, ?, ?, ?, ?, ?, ?, ?, NULL)`, l.HostedOrigin, l.LinkID, l.TokenSealed, l.KeyID, l.AccountEmail, l.SlackApprovals, time.Now().UTC())
	return err
}

// UnlinkRelay forgets the install's link and the links it started; it
// returns the link it forgot, store.ErrNotFound when it had none.
func (s *Store) UnlinkRelay(ctx context.Context) (RelayLink, error) {
	var out RelayLink
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		var ok sql.NullTime
		err := tx.QueryRowContext(ctx, `SELECT `+relayLinkColumns+` FROM foundry_local_relay WHERE id=1 FOR UPDATE`).
			Scan(&out.HostedOrigin, &out.LinkID, &out.TokenSealed, &out.KeyID, &out.AccountEmail, &out.SlackApprovals, &out.LinkedAt, &ok)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		if ok.Valid {
			out.LastOKAt = &ok.Time
		}
		for _, stmt := range []string{`DELETE FROM foundry_local_relay WHERE id=1`, `DELETE FROM foundry_local_relay_pending`} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// SetRelaySlackApprovals turns approvals through Slack on or off;
// store.ErrNotFound while unlinked.
func (s *Store) SetRelaySlackApprovals(ctx context.Context, on bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE foundry_local_relay SET slack_approvals=? WHERE id=1`, on)
	if err != nil {
		return err
	}
	// MySQL counts a row whose value was already on as unaffected: tell it
	// apart from no row.
	if n, err := res.RowsAffected(); err != nil || n == 1 {
		return err
	}
	_, err = s.RelayLink(ctx)
	return err
}

// RelayOK records that Helix answered the link's claim now, if the link is
// still linkID (an unlink or a new link since leaves it be).
func (s *Store) RelayOK(ctx context.Context, linkID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_local_relay SET last_ok_at=? WHERE id=1 AND link_id=?`, time.Now().UTC(), linkID)
	return err
}

// StartRelayLink records a link the owner started: its state's SHA-256 (hex)
// and its PKCE verifier, sealed under keyID, for RelayPendingTTL. It returns
// when it expires. Pending links expired a day ago are forgotten.
func (s *Store) StartRelayLink(ctx context.Context, stateHash string, verifierSealed []byte, keyID string) (time.Time, error) {
	if !model.ValidSHA256Hex(stateHash) || len(verifierSealed) == 0 || len(verifierSealed) > 512 || keyID == "" || len(keyID) > 64 {
		return time.Time{}, fmt.Errorf("a pending relay link is a SHA-256, and a verifier sealed under a key")
	}
	now := time.Now().UTC()
	expires := now.Add(RelayPendingTTL)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_local_relay_pending (state_hash, verifier_sealed, key_id, expires_at) VALUES (?, ?, ?, ?)`,
		stateHash, verifierSealed, keyID, expires); err != nil {
		return time.Time{}, err
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM foundry_local_relay_pending WHERE expires_at < ?`, now.Add(-24*time.Hour))
	return expires, nil
}

// SpendRelayLink spends the pending link stateHash, once, before it
// expires, returning its sealed verifier and key; store.ErrNotFound when it
// is unknown, spent or expired.
func (s *Store) SpendRelayLink(ctx context.Context, stateHash string) (verifierSealed []byte, keyID string, err error) {
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		err := tx.QueryRowContext(ctx, `SELECT verifier_sealed, key_id FROM foundry_local_relay_pending
WHERE state_hash=? AND used_at IS NULL AND expires_at > ? FOR UPDATE`, stateHash, now).Scan(&verifierSealed, &keyID)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE foundry_local_relay_pending SET used_at=? WHERE state_hash=?`, now, stateHash)
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return verifierSealed, keyID, nil
}
