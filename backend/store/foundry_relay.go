package store

import (
	"cmp"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Helix Foundry for Slack's relay (migrations/20261216000000_foundry_slack_relay.sql):
// links of local installs to a hosted workspace and the PKCE codes that
// make them; Slack's requests for a relayed install (deliveries) and its
// channel messages (the relay feed), leased to the install's claims until
// it acks them or they expire; what its proxied calls may name; daily
// counts; and, on the local install, its mirror of its installs.
//
// A claim polls without locks (FoundryRelayWaiting) and locks only once it
// finds something (LeaseFoundryRelay). A relayed install's feed rows are
// counted on its install row (relay_feed_rows), whose lock every write of
// its feed takes first.

const foundryRelayLinkColumns = `id, workspace_id, created_by_user_id, name, redirect_origin, created_at, last_seen_at, last_seen_addr, revoked_at`

const foundryRelayDeliveryColumns = `id, install_id, link_id, relay_workspace_id, dedupe_key, kind, conversation, slack_user_id, payload, status,
       attempts, lease_until, noticed_at, created_at, expires_at, finished_at`

const foundryRelayFeedColumns = `id, install_id, relay_workspace_id, dedupe_key, channel_id, day, kind, payload`

// foundryRelayMessageKinds are the relay feed's kinds but a gap's: the rows
// that expire, and that relay_feed_rows counts.
const foundryRelayMessageKinds = `'message', 'changed', 'deleted', 'replied', 'channel', 'joined', 'left', 'span', 'complete'`

// maxRelayAcks bounds the ids one claim acks: a claim's deliveries and feed.
const maxRelayAcks = 2048

// --- Links and codes ---

// CreateFoundryRelayCode records a code the site gave c.UserID's browser to
// link a local install to c.WorkspaceID, live for ttl; ErrConflict when its
// hash is taken. The caller checks the member may link one.
func (s *mysqlDB) CreateFoundryRelayCode(ctx context.Context, c *model.RelayCode, ttl time.Duration) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if ttl <= 0 {
		return fmt.Errorf("a relay code has a lifetime")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_relay_codes (code_hash, workspace_id, user_id, code_challenge, redirect_uri, name, expires_at)
VALUES (?, ?, ?, ?, ?, ?, NOW(6) + INTERVAL ? MICROSECOND)`, c.CodeHash, c.WorkspaceID, c.UserID, c.CodeChallenge, c.RedirectURI, c.Name,
		ttl.Microseconds())
	if isDuplicateKey(err) {
		return ErrConflict
	}
	return err
}

// RedeemFoundryRelayCode spends the live code codeHash, once, for the PKCE
// verifier whose challenge it holds, and links its install as linkID with
// the hx_rl_ token tokenHash (a SHA-256 in hex), in one transaction holding
// its workspace's lock. ErrNotFound when the code is absent, expired, spent,
// another verifier's (which leaves it unspent) or its workspace is gone;
// ErrConflict when the token hash is taken.
func (s *mysqlDB) RedeemFoundryRelayCode(ctx context.Context, codeHash, verifier, linkID, tokenHash string) (*model.RelayLink, error) {
	switch {
	case !model.ValidSHA256Hex(codeHash) || !model.ValidSHA256Hex(tokenHash):
		return nil, fmt.Errorf("a relay code and token are kept as SHA-256s in hex")
	case !model.ValidRelayVerifier(verifier):
		return nil, fmt.Errorf("a relay code is redeemed with a PKCE verifier")
	case linkID == "":
		return nil, fmt.Errorf("a relay link has an id")
	}
	var out *model.RelayLink
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var c model.RelayCode
		err := tx.db.QueryRowContext(ctx, `SELECT code_hash, workspace_id, user_id, code_challenge, redirect_uri, name FROM foundry_relay_codes
WHERE code_hash=? AND used_at IS NULL AND expires_at > NOW(6) FOR UPDATE`, codeHash).
			Scan(&c.CodeHash, &c.WorkspaceID, &c.UserID, &c.CodeChallenge, &c.RedirectURI, &c.Name)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(model.RelayCodeChallenge(verifier)), []byte(c.CodeChallenge)) != 1 {
			return ErrNotFound
		}
		origin, err := model.ParseRelayRedirect(c.RedirectURI)
		if err != nil {
			return err
		}
		if err := tx.workspaces.Lock(ctx, tx.db, c.WorkspaceID); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_relay_codes SET used_at=NOW(6) WHERE code_hash=?`, codeHash); err != nil {
			return err
		}
		_, err = tx.db.ExecContext(ctx, `INSERT INTO foundry_relay_links (id, workspace_id, created_by_user_id, name, token_hash, redirect_origin)
VALUES (?, ?, ?, ?, ?, ?)`, linkID, c.WorkspaceID, c.UserID, c.Name, tokenHash, origin)
		if isDuplicateKey(err) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		out, err = scanFoundryRelayLink(tx.db.QueryRowContext(ctx, "SELECT "+foundryRelayLinkColumns+` FROM foundry_relay_links WHERE id=?`, linkID))
		return err
	})
	return out, err
}

// GetFoundryRelayLinkByToken returns the live link whose hx_rl_ token hashes
// to tokenHash; ErrNotFound when none does, or it was revoked.
func (s *mysqlDB) GetFoundryRelayLinkByToken(ctx context.Context, tokenHash string) (*model.RelayLink, error) {
	return scanFoundryRelayLink(s.db.QueryRowContext(ctx, "SELECT "+foundryRelayLinkColumns+`
FROM foundry_relay_links WHERE token_hash=? AND revoked_at IS NULL`, tokenHash))
}

// ListFoundryRelayLinks returns the workspace's live links, newest first.
func (s *mysqlDB) ListFoundryRelayLinks(ctx context.Context, workspaceID string) ([]*model.RelayLink, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryRelayLinkColumns+`
FROM foundry_relay_links WHERE workspace_id=? AND revoked_at IS NULL ORDER BY created_at DESC, id DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundryRelayLink)
}

// SeeFoundryRelayLink records the live link's contact from addr (at most 64
// bytes), unless it was recorded within model.RelaySeenEvery: a claim every
// few seconds writes once a minute.
func (s *mysqlDB) SeeFoundryRelayLink(ctx context.Context, linkID, addr string) error {
	if len(addr) > 64 {
		return fmt.Errorf("a relay link's address is at most 64 bytes")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_relay_links SET last_seen_at=NOW(6), last_seen_addr=?
WHERE id=? AND revoked_at IS NULL AND (last_seen_at IS NULL OR last_seen_at < NOW(6) - INTERVAL ? MICROSECOND)`,
		addr, linkID, model.RelaySeenEvery.Microseconds())
	return err
}

// RevokeFoundryRelayLink revokes the workspace's live link (ErrNotFound when
// it isn't one) and, in the same transaction, its installs, which the sweep
// uninstalls from Slack, as a disconnect does (RevokeFoundrySlackInstall):
// their open deliveries expire unnoticed, their feed and what their calls
// may name are deleted, and the installs the link started and hasn't
// finished are forgotten.
func (s *mysqlDB) RevokeFoundryRelayLink(ctx context.Context, workspaceID, linkID string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := execOrNotFound(ctx, tx.db, `UPDATE foundry_relay_links SET revoked_at=NOW(6) WHERE id=? AND workspace_id=? AND revoked_at IS NULL`,
			linkID, workspaceID); err != nil {
			return err
		}
		for _, stmt := range []string{
			`UPDATE foundry_slack_installs SET revoked_at=NOW(6) WHERE relay_link_id=? AND revoked_at IS NULL`,
			`DELETE FROM foundry_slack_install_states WHERE relay_link_id=?`,
		} {
			if _, err := tx.db.ExecContext(ctx, stmt, linkID); err != nil {
				return err
			}
		}
		return tx.clearFoundrySlackInstall(ctx, `SELECT id FROM foundry_slack_installs WHERE relay_link_id=?`, false, linkID)
	})
}

// --- Relayed installs ---

// ListFoundryRelayInstalls returns the link's live installs, oldest first:
// what every claim tells the local install it has.
func (s *mysqlDB) ListFoundryRelayInstalls(ctx context.Context, linkID string) ([]*model.SlackInstall, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundrySlackInstallColumns+`
FROM foundry_slack_installs WHERE relay_link_id=? AND revoked_at IS NULL ORDER BY created_at, id`, linkID)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundrySlackInstall)
}

// GetFoundryRelayInstall returns the link's live install serving the local
// workspace localWorkspaceID: the one install its proxied calls act as.
// ErrNotFound when it has none.
func (s *mysqlDB) GetFoundryRelayInstall(ctx context.Context, linkID, localWorkspaceID string) (*model.SlackInstall, error) {
	return scanFoundrySlackInstall(s.db.QueryRowContext(ctx, "SELECT "+foundrySlackInstallColumns+`
FROM foundry_slack_installs WHERE relay_link_id=? AND relay_workspace_id=? AND revoked_at IS NULL`, linkID, localWorkspaceID))
}

// SetFoundryRelayFeeds records, for each of the link's live installs a feed
// names (others are left as they are), whether its local Slack data
// connector reads it. An install whose feed goes off drops what it held.
// The caller turns one on only while hosted's own flags allow it.
func (s *mysqlDB) SetFoundryRelayFeeds(ctx context.Context, linkID string, feeds []model.RelayFeedOn) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		for _, f := range feeds {
			var installID string
			var on bool
			err := tx.db.QueryRowContext(ctx, `SELECT id, feed_on FROM foundry_slack_installs
WHERE relay_link_id=? AND relay_workspace_id=? AND revoked_at IS NULL FOR UPDATE`, linkID, f.LocalWorkspaceID).Scan(&installID, &on)
			if errors.Is(err, sql.ErrNoRows) || err == nil && on == f.On {
				continue
			}
			if err != nil {
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_slack_installs SET feed_on=?, relay_feed_rows=IF(?, relay_feed_rows, 0) WHERE id=?`,
				f.On, f.On, installID); err != nil {
				return err
			}
			if !f.On {
				if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_relay_feed WHERE install_id=?`, installID); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// --- Deliveries ---

// QueueFoundryRelayDelivery queues what Slack sent for the live relayed
// install d.InstallID (ErrNotFound otherwise), for its local install to
// claim within ttl, and records what d tells the relay its install's calls
// may name (seen) and the link's day's count, in one transaction. d's link
// and local workspace are its install's. ErrConflict when its dedupe key
// was queued already (a retry, which Slack is answered 200).
func (s *mysqlDB) QueueFoundryRelayDelivery(ctx context.Context, d *model.RelayDelivery, ttl time.Duration, seen model.RelaySeen) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if err := seen.Validate(); err != nil {
		return err
	}
	if ttl <= 0 {
		return fmt.Errorf("a relay delivery has a lifetime")
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		// The install is read under a shared lock: a revoke either comes
		// first, or waits for the delivery and expires it.
		var linkID, localWorkspaceID string
		err := tx.db.QueryRowContext(ctx, `SELECT relay_link_id, relay_workspace_id FROM foundry_slack_installs
WHERE id=? AND revoked_at IS NULL AND relay_link_id IS NOT NULL FOR SHARE`, d.InstallID).Scan(&linkID, &localWorkspaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.db.ExecContext(ctx, `INSERT INTO foundry_relay_deliveries
  (id, link_id, install_id, relay_workspace_id, dedupe_key, conversation, slack_user_id, kind, payload, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(6) + INTERVAL ? MICROSECOND)`, d.ID, linkID, d.InstallID, localWorkspaceID, d.DedupeKey, d.Conversation,
			d.SlackUserID, string(d.Kind), string(d.Payload), ttl.Microseconds())
		if isDuplicateKey(err) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		var channels []string
		if seen.ChannelID != "" {
			channels = []string{seen.ChannelID}
		}
		if err := tx.seeFoundryRelay(ctx, "foundry_relay_channels", "channel_id", d.InstallID, channels); err != nil {
			return err
		}
		if err := tx.seeFoundryRelay(ctx, "foundry_relay_users", "slack_user_id", d.InstallID, seen.UserIDs); err != nil {
			return err
		}
		return tx.CountFoundryRelayUsage(ctx, linkID, model.RelayUsage{Deliveries: 1})
	})
}

// FoundryRelayWaiting reports, without a lock, whether anything waits for
// the link's claim: a delivery queued, or leased under a lease that ran out,
// and not expired; or a feed item unleased, or leased under a lease that ran
// out. A claim polls it and leases only once it says so.
func (s *mysqlDB) FoundryRelayWaiting(ctx context.Context, linkID string) (bool, error) {
	var waiting bool
	err := s.db.QueryRowContext(ctx, `SELECT
  EXISTS (SELECT 1 FROM foundry_relay_deliveries WHERE link_id=? AND (status='queued' OR (status='leased' AND lease_until < NOW(6)))
    AND expires_at > NOW(6))
  OR EXISTS (SELECT 1 FROM foundry_relay_feed WHERE link_id=? AND (lease_until IS NULL OR lease_until < NOW(6)))`, linkID, linkID).Scan(&waiting)
	return waiting, err
}

// LeaseFoundryRelay leases to the link's claim, for lease, within budget:
// its deliveries first (queued, or leased under a lease that ran out, and
// not expired), each attempt counted, then its feed items (unleased, or
// leased under a lease that ran out), each oldest first. Another claim's
// live lease is skipped, never waited for.
func (s *mysqlDB) LeaseFoundryRelay(ctx context.Context, linkID string, lease time.Duration, budget model.RelayBudget) (model.RelayClaim, error) {
	if err := budget.Validate(); err != nil {
		return model.RelayClaim{}, err
	}
	if lease <= 0 {
		return model.RelayClaim{}, fmt.Errorf("a relay claim has a lease")
	}
	var out model.RelayClaim
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		out = model.RelayClaim{}
		rows, err := tx.db.QueryContext(ctx, `SELECT id FROM foundry_relay_deliveries
WHERE link_id=? AND (status='queued' OR (status='leased' AND lease_until < NOW(6))) AND expires_at > NOW(6)
ORDER BY created_at, id LIMIT ? FOR UPDATE SKIP LOCKED`, linkID, budget.Deliveries)
		if err != nil {
			return err
		}
		ids, err := scanStrings(rows)
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			in, args := idPlaceholders(len(ids)), toAnySlice(ids)
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_relay_deliveries
SET status='leased', attempts=attempts+1, lease_until=NOW(6) + INTERVAL ? MICROSECOND WHERE id IN (`+in+`)`,
				append([]any{lease.Microseconds()}, args...)...); err != nil {
				return err
			}
			rows, err := tx.db.QueryContext(ctx, "SELECT "+foundryRelayDeliveryColumns+` FROM foundry_relay_deliveries WHERE id IN (`+in+`)
ORDER BY created_at, id`, args...)
			if err != nil {
				return err
			}
			if out.Deliveries, err = scanAll(rows, scanFoundryRelayDelivery); err != nil {
				return err
			}
		}
		if budget.FeedRows == 0 {
			return nil
		}
		rows, err = tx.db.QueryContext(ctx, `SELECT id, bytes FROM foundry_relay_feed
WHERE link_id=? AND (lease_until IS NULL OR lease_until < NOW(6)) ORDER BY created_at, id LIMIT ? FOR UPDATE SKIP LOCKED`, linkID, budget.FeedRows)
		if err != nil {
			return err
		}
		type sized struct {
			id    string
			bytes int64
		}
		items, err := scanAll(rows, func(r rowScanner) (sized, error) {
			var it sized
			return it, r.Scan(&it.id, &it.bytes)
		})
		if err != nil {
			return err
		}
		// Within the bytes, unless the first alone is more.
		var feed []string
		var total int64
		for _, it := range items {
			if total += it.bytes; len(feed) > 0 && total > budget.FeedBytes {
				break
			}
			feed = append(feed, it.id)
		}
		if len(feed) == 0 {
			return nil
		}
		in, args := idPlaceholders(len(feed)), toAnySlice(feed)
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_relay_feed SET lease_until=NOW(6) + INTERVAL ? MICROSECOND WHERE id IN (`+in+`)`,
			append([]any{lease.Microseconds()}, args...)...); err != nil {
			return err
		}
		rows, err = tx.db.QueryContext(ctx, "SELECT "+foundryRelayFeedColumns+` FROM foundry_relay_feed WHERE id IN (`+in+`) ORDER BY created_at, id`,
			args...)
		if err != nil {
			return err
		}
		out.Feed, err = scanAll(rows, scanFoundryRelayFeedItem)
		return err
	})
	return out, err
}

// AckFoundryRelay acks what the link's local install took, by id (at most
// 2,048): its leased deliveries are acked, their payloads dropped, and its
// leased feed items deleted. Ids of anything else, or another link's, are
// ignored. It returns how many it acked.
func (s *mysqlDB) AckFoundryRelay(ctx context.Context, linkID string, ids []string) (int, error) {
	switch {
	case len(ids) == 0:
		return 0, nil
	case len(ids) > maxRelayAcks:
		return 0, fmt.Errorf("a relay claim acks at most %d ids", maxRelayAcks)
	}
	acked := 0
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		acked = 0
		in, args := idPlaceholders(len(ids)), toAnySlice(ids)
		res, err := tx.db.ExecContext(ctx, `UPDATE foundry_relay_deliveries
SET status='acked', payload=NULL, lease_until=NULL, finished_at=NOW(6) WHERE link_id=? AND status='leased' AND id IN (`+in+`)`,
			append([]any{linkID}, args...)...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		acked = int(n)
		// Message rows are counted on their install, whose lock comes first.
		rows, err := tx.db.QueryContext(ctx, `SELECT DISTINCT install_id FROM foundry_relay_feed WHERE link_id=? AND id IN (`+in+`) ORDER BY install_id`,
			append([]any{linkID}, args...)...)
		if err != nil {
			return err
		}
		installs, err := scanStrings(rows)
		if err != nil {
			return err
		}
		for _, installID := range installs {
			var locked string
			if err := tx.db.QueryRowContext(ctx, `SELECT id FROM foundry_slack_installs WHERE id=? FOR UPDATE`, installID).Scan(&locked); err != nil {
				return err
			}
			owned := ` FROM foundry_relay_feed WHERE install_id=? AND link_id=? AND lease_until IS NOT NULL AND id IN (` + in + `)`
			owner := append([]any{installID, linkID}, args...)
			var messages, items int
			if err := tx.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(kind IN (`+foundryRelayMessageKinds+`)), 0), COUNT(*)`+owned+` FOR UPDATE`,
				owner...).Scan(&messages, &items); err != nil {
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `DELETE`+owned, owner...); err != nil {
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_slack_installs SET relay_feed_rows=GREATEST(relay_feed_rows-?, 0) WHERE id=?`,
				messages, installID); err != nil {
				return err
			}
			acked += items
		}
		return nil
	})
	return acked, err
}

// ExpireFoundryRelayDeliveries expires up to limit deliveries their local
// install never took: queued past their expiry, or leased past it and
// grace (the time to ack). Their payloads are dropped. It returns them as
// they were, payloads included, one per conversation (each click its own),
// for the worker to tell each that it wasn't delivered.
func (s *mysqlDB) ExpireFoundryRelayDeliveries(ctx context.Context, grace time.Duration, limit int) ([]*model.RelayDelivery, error) {
	if grace < 0 || limit < 1 {
		return nil, fmt.Errorf("a relay expiry has a grace and a limit")
	}
	return s.takeFoundryRelayDeliveries(ctx, `SELECT `+foundryRelayDeliveryColumns+` FROM foundry_relay_deliveries
WHERE (status='queued' AND expires_at < NOW(6)) OR (status='leased' AND expires_at < NOW(6) - INTERVAL ? MICROSECOND)
ORDER BY expires_at, id LIMIT ? FOR UPDATE SKIP LOCKED`, []any{grace.Microseconds(), limit},
		`UPDATE foundry_relay_deliveries SET status='expired', payload=NULL, lease_until=NULL, finished_at=NOW(6) WHERE id IN (%s)`)
}

// NoticeFoundryRelayOffline takes up to limit queued deliveries of live
// links last seen more than offline ago (or never), not yet noticed nor
// expired (an expired one is told it wasn't delivered instead), in
// conversations not noticed within every; marks them noticed; and returns
// one per conversation (each click its own), for the worker to tell each
// its install is offline. A notice is given at most once.
func (s *mysqlDB) NoticeFoundryRelayOffline(ctx context.Context, offline, every time.Duration, limit int) ([]*model.RelayDelivery, error) {
	if offline <= 0 || every <= 0 || limit < 1 {
		return nil, fmt.Errorf("a relay offline notice has an offline time, a spacing and a limit")
	}
	// The link and the conversation's notices are read in subqueries, so the
	// locking read locks the deliveries alone.
	return s.takeFoundryRelayDeliveries(ctx, `SELECT `+foundryRelayDeliveryColumns+` FROM foundry_relay_deliveries d
WHERE d.status='queued' AND d.noticed_at IS NULL AND d.expires_at > NOW(6)
  AND d.link_id IN (SELECT l.id FROM foundry_relay_links l WHERE l.revoked_at IS NULL
    AND (l.last_seen_at IS NULL OR l.last_seen_at < NOW(6) - INTERVAL ? MICROSECOND))
  AND (d.conversation='' OR NOT EXISTS (SELECT 1 FROM foundry_relay_deliveries o
    WHERE o.conversation=d.conversation AND o.noticed_at > NOW(6) - INTERVAL ? MICROSECOND))
ORDER BY d.created_at, d.id LIMIT ? FOR UPDATE SKIP LOCKED`, []any{offline.Microseconds(), every.Microseconds(), limit},
		`UPDATE foundry_relay_deliveries SET noticed_at=NOW(6) WHERE id IN (%s)`)
}

// takeFoundryRelayDeliveries reads the deliveries pick locks with args,
// runs set (its "%s" their ids) on them, and returns them as read, the first
// of each conversation (each click its own), in one transaction.
func (s *mysqlDB) takeFoundryRelayDeliveries(ctx context.Context, pick string, args []any, set string) ([]*model.RelayDelivery, error) {
	var out []*model.RelayDelivery
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		out = nil
		rows, err := tx.db.QueryContext(ctx, pick, args...)
		if err != nil {
			return err
		}
		taken, err := scanAll(rows, scanFoundryRelayDelivery)
		if err != nil || len(taken) == 0 {
			return err
		}
		ids := make([]string, len(taken))
		told := map[string]bool{}
		for i, d := range taken {
			ids[i] = d.ID
			if d.Conversation == "" || !told[d.Conversation] {
				told[d.Conversation] = true
				out = append(out, d)
			}
		}
		in, idArgs := idPlaceholders(len(ids)), toAnySlice(ids)
		_, err = tx.db.ExecContext(ctx, fmt.Sprintf(set, in), idArgs...)
		return err
	})
	return out, err
}

// --- The relay feed ---

// AppendFoundryRelayFeed holds what a Slack request gives the live relayed
// install installID's feed for limits.TTL, in one transaction holding the
// install's lock. Unbound when the install isn't a live relayed one with its
// feed on. Rows past limits.MaxRows held become a gap of the minute each
// message was posted (model.SlackFeedRow.Minute, else a.At's) in its channel
// (full); a row held already is dropped (duplicate when every one was, and
// no gap was new). a.Rows come from requests the API verified, so each
// row's channel but a left one's (the bot is in it) is one the install's
// calls may name from now (foundry_relay_channels), full or not. a.Gaps
// naming a channel are held only for a channel the install's calls may name
// (within model.RelayChannelSeen): the door holds gaps from requests no one
// verified, and a channel the feed reads is recorded by its first row, a
// joined one or a message. Gaps are held merged with their channel's newest unclaimed gap
// they meet or touch; past model.RelayFeedMaxGaps gaps held, one widens the
// newest unclaimed gap of every channel instead. The link's day counts the
// rows and their bytes.
func (s *mysqlDB) AppendFoundryRelayFeed(ctx context.Context, installID string, a model.RelayFeedAppend, limits model.RelayFeedLimits) (
	model.RelayFeedAppended, error) {
	if err := a.Validate(); err != nil {
		return model.RelayFeedAppended{}, err
	}
	if limits.TTL <= 0 || limits.MaxRows < 1 {
		return model.RelayFeedAppended{}, fmt.Errorf("a relay feed append has a lifetime and a cap")
	}
	var out model.RelayFeedAppended
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		out = model.RelayFeedAppended{Outcome: model.SlackAppendUnbound}
		in, err := tx.lockFoundryRelayFeedInstall(ctx, installID)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		out.Outcome = model.SlackAppended
		var channels []string
		for _, r := range a.Rows {
			if r.Kind != model.SlackRowLeft {
				channels = append(channels, r.ChannelID)
			}
		}
		if err := tx.seeFoundryRelay(ctx, "foundry_relay_channels", "channel_id", installID, channels); err != nil {
			return err
		}
		gaps, err := tx.knownFoundryRelayGaps(ctx, installID, a.Gaps, a.At)
		if err != nil {
			return err
		}
		if len(a.Rows) > 0 && in.rows+int64(len(a.Rows)) > limits.MaxRows {
			out.Outcome = model.SlackAppendFull
			for _, r := range a.Rows {
				gaps = append(gaps, model.SlackMinuteGap(r.ChannelID, r.Minute(a.At)))
			}
		} else if len(a.Rows) > 0 {
			var usage model.RelayUsage
			for _, r := range a.Rows {
				res, err := tx.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_relay_feed
  (id, link_id, install_id, relay_workspace_id, dedupe_key, channel_id, day, kind, payload, bytes, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NOW(6) + INTERVAL ? MICROSECOND)`, id.New(), in.linkID, installID, in.localWorkspaceID, r.DedupeKey, r.ChannelID,
					r.Day, string(r.Kind), string(r.Payload), len(r.Payload), limits.TTL.Microseconds())
				if err != nil {
					return err
				}
				n, err := res.RowsAffected()
				if err != nil {
					return err
				}
				// A row held already is dropped.
				usage.FeedRows, usage.FeedBytes = usage.FeedRows+n, usage.FeedBytes+n*int64(len(r.Payload))
			}
			if usage.FeedRows > 0 {
				if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_slack_installs SET relay_feed_rows=relay_feed_rows+? WHERE id=?`,
					usage.FeedRows, installID); err != nil {
					return err
				}
				if err := tx.CountFoundryRelayUsage(ctx, in.linkID, usage); err != nil {
					return err
				}
			}
			out.Rows = int(usage.FeedRows)
		}
		var held int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_relay_feed WHERE install_id=? AND kind='gap'`, installID).
			Scan(&held); err != nil {
			return err
		}
		for _, g := range gaps {
			widen := held >= model.RelayFeedMaxGaps
			if widen {
				g.ChannelID = ""
			}
			added, err := tx.addFoundryRelayGap(ctx, in, installID, g, limits.TTL, widen)
			if err != nil {
				return err
			}
			if added {
				out.Rows, held = out.Rows+1, held+1
			}
		}
		if out.Outcome == model.SlackAppended && out.Rows == 0 {
			out.Outcome = model.SlackAppendDuplicate
		}
		return nil
	})
	return out, err
}

// foundryRelayFeedInstall is a live relayed install with its feed on, as its
// locked row says.
type foundryRelayFeedInstall struct {
	linkID, localWorkspaceID string
	rows                     int64
}

// lockFoundryRelayFeedInstall locks the install's row: ErrNotFound unless it
// is a live relayed install with its feed on.
func (s *mysqlDB) lockFoundryRelayFeedInstall(ctx context.Context, installID string) (foundryRelayFeedInstall, error) {
	var in foundryRelayFeedInstall
	err := s.db.QueryRowContext(ctx, `SELECT relay_link_id, relay_workspace_id, relay_feed_rows FROM foundry_slack_installs
WHERE id=? AND revoked_at IS NULL AND relay_link_id IS NOT NULL AND feed_on FOR UPDATE`, installID).Scan(&in.linkID, &in.localWorkspaceID, &in.rows)
	if errors.Is(err, sql.ErrNoRows) {
		return in, ErrNotFound
	}
	return in, err
}

// knownFoundryRelayGaps are the gaps of gaps the install may be told of:
// those of every channel, and those of a channel its proxied calls may name,
// each cut to the days any window keeps before at (none left: dropped).
func (s *mysqlDB) knownFoundryRelayGaps(ctx context.Context, installID string, gaps []model.SlackFeedGap, at time.Time) (
	[]model.SlackFeedGap, error) {
	oldest := model.SlackDay(at).AddDate(0, 0, -model.SlackAppHistoryDaysMax-1)
	var named []string
	for _, g := range gaps {
		if g.ChannelID != "" {
			named = append(named, g.ChannelID)
		}
	}
	known := map[string]bool{"": true}
	if len(named) > 0 {
		rows, err := s.db.QueryContext(ctx, `SELECT channel_id FROM foundry_relay_channels
WHERE install_id=? AND last_at > NOW(6) - INTERVAL ? MICROSECOND AND channel_id IN (`+idPlaceholders(len(named))+`)`,
			append([]any{installID, model.RelayChannelSeen.Microseconds()}, toAnySlice(named)...)...)
		if err != nil {
			return nil, err
		}
		channels, err := scanStrings(rows)
		if err != nil {
			return nil, err
		}
		for _, c := range channels {
			known[c] = true
		}
	}
	var out []model.SlackFeedGap
	for _, g := range gaps {
		if !known[g.ChannelID] || !g.To.After(oldest) {
			continue
		}
		g.From = maxTime(g.From, oldest)
		out = append(out, g)
	}
	return out, nil
}

// addFoundryRelayGap holds gap for the install, whose lock the caller holds:
// merged into its channel's newest unclaimed gap when they meet or touch, or
// whatever their spans when widen, else a gap of its own, held for ttl. It
// reports whether anything changed.
func (s *mysqlDB) addFoundryRelayGap(ctx context.Context, in foundryRelayFeedInstall, installID string, gap model.SlackFeedGap,
	ttl time.Duration, widen bool) (bool, error) {
	var heldID, heldPayload string
	err := s.db.QueryRowContext(ctx, `SELECT id, payload FROM foundry_relay_feed
WHERE install_id=? AND kind='gap' AND channel_id=? AND lease_until IS NULL ORDER BY created_at DESC, id DESC LIMIT 1`, installID, gap.ChannelID).
		Scan(&heldID, &heldPayload)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, err
	default:
		held, err := parseFoundryRelayGap(gap.ChannelID, heldPayload)
		if err != nil {
			return false, err
		}
		if widen || !gap.From.After(held.To) && !gap.To.Before(held.From) {
			merged := model.SlackFeedGap{ChannelID: gap.ChannelID, From: minTime(held.From, gap.From), To: maxTime(held.To, gap.To)}
			if merged == held {
				return false, nil
			}
			row := merged.Row(gap.ChannelID)
			_, err := s.db.ExecContext(ctx, `UPDATE foundry_relay_feed SET dedupe_key=?, day=?, payload=?, bytes=? WHERE id=?`,
				row.DedupeKey, row.Day, string(row.Payload), len(row.Payload), heldID)
			if isDuplicateKey(err) {
				// A claimed gap is the same span: it holds both.
				_, err = s.db.ExecContext(ctx, `DELETE FROM foundry_relay_feed WHERE id=?`, heldID)
			}
			return err == nil, err
		}
	}
	row := gap.Row(gap.ChannelID)
	res, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_relay_feed
  (id, link_id, install_id, relay_workspace_id, dedupe_key, channel_id, day, kind, payload, bytes, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, 'gap', ?, ?, NOW(6) + INTERVAL ? MICROSECOND)`, id.New(), in.linkID, installID, in.localWorkspaceID, row.DedupeKey,
		gap.ChannelID, row.Day, string(row.Payload), len(row.Payload), ttl.Microseconds())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ExpireFoundryRelayFeed turns up to limit message rows held past their
// expiry, and not under a live lease, into gaps: per install and channel,
// from model.RelayFeedLateness before the first of them arrived to the
// minute after the last, merged as AppendFoundryRelayFeed merges gaps and
// held for ttl. Gaps themselves never expire: they hold no message. It
// returns how many rows it expired.
func (s *mysqlDB) ExpireFoundryRelayFeed(ctx context.Context, ttl time.Duration, limit int) (int, error) {
	if ttl <= 0 || limit < 1 {
		return 0, fmt.Errorf("a relay feed expiry has a lifetime and a limit")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT install_id FROM foundry_relay_feed
WHERE kind IN (`+foundryRelayMessageKinds+`) AND expires_at < NOW(6) LIMIT ?`, limit)
	if err != nil {
		return 0, err
	}
	installs, err := scanStrings(rows)
	if err != nil {
		return 0, err
	}
	expired := 0
	for _, installID := range installs {
		if expired >= limit {
			break
		}
		err := s.WithTx(ctx, func(txDB Store) error {
			tx := txDB.(*mysqlDB)
			var in foundryRelayFeedInstall
			err := tx.db.QueryRowContext(ctx, `SELECT relay_link_id, relay_workspace_id, relay_feed_rows FROM foundry_slack_installs WHERE id=? FOR UPDATE`,
				installID).Scan(&in.linkID, &in.localWorkspaceID, &in.rows)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			const due = ` FROM foundry_relay_feed WHERE install_id=? AND kind IN (` + foundryRelayMessageKinds + `) AND expires_at < NOW(6)
  AND (lease_until IS NULL OR lease_until < NOW(6))`
			rows, err := tx.db.QueryContext(ctx, `SELECT id, channel_id, created_at`+due+` ORDER BY created_at, id LIMIT ?`, installID, limit-expired)
			if err != nil {
				return err
			}
			type held struct {
				id, channel string
				at          time.Time
			}
			stale, err := scanAll(rows, func(r rowScanner) (held, error) {
				var h held
				return h, r.Scan(&h.id, &h.channel, &h.at)
			})
			if err != nil || len(stale) == 0 {
				return err
			}
			spans := map[string]model.SlackFeedGap{}
			var channels []string
			ids := make([]string, len(stale))
			for i, h := range stale {
				ids[i] = h.id
				from, to := h.at.UTC().Truncate(time.Minute).Add(-model.RelayFeedLateness), h.at.UTC().Truncate(time.Minute).Add(time.Minute)
				g, ok := spans[h.channel]
				if !ok {
					channels = append(channels, h.channel)
					g = model.SlackFeedGap{ChannelID: h.channel, From: from, To: to}
				}
				spans[h.channel] = model.SlackFeedGap{ChannelID: h.channel, From: minTime(g.From, from), To: maxTime(g.To, to)}
			}
			if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_relay_feed WHERE id IN (`+idPlaceholders(len(ids))+`)`, toAnySlice(ids)...); err != nil {
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_slack_installs SET relay_feed_rows=GREATEST(relay_feed_rows-?, 0) WHERE id=?`,
				len(ids), installID); err != nil {
				return err
			}
			for _, ch := range channels {
				if _, err := tx.addFoundryRelayGap(ctx, in, installID, spans[ch], ttl, false); err != nil {
					return err
				}
			}
			expired += len(ids)
			return nil
		})
		if err != nil {
			return expired, err
		}
	}
	return expired, nil
}

// --- What proxied calls may name, and counts ---

// KnowsFoundryRelayChannel reports whether the relayed install was sent
// something from the channel within model.RelayChannelSeen: a channel its
// proxied calls may name.
func (s *mysqlDB) KnowsFoundryRelayChannel(ctx context.Context, installID, channelID string) (bool, error) {
	var known bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_relay_channels
WHERE install_id=? AND channel_id=? AND last_at > NOW(6) - INTERVAL ? MICROSECOND)`, installID, channelID, model.RelayChannelSeen.Microseconds()).
		Scan(&known)
	return known, err
}

// KnowsFoundryRelayUsers reports whether every one of the Slack users (at
// least one) sent the relayed install something, or was named in it, within
// model.RelayChannelSeen: users its proxied calls may name.
func (s *mysqlDB) KnowsFoundryRelayUsers(ctx context.Context, installID string, userIDs []string) (bool, error) {
	if len(userIDs) == 0 {
		return false, nil
	}
	in, args := idPlaceholders(len(userIDs)), toAnySlice(userIDs)
	var known int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT slack_user_id) FROM foundry_relay_users
WHERE install_id=? AND last_at > NOW(6) - INTERVAL ? MICROSECOND AND slack_user_id IN (`+in+`)`,
		append([]any{installID, model.RelayChannelSeen.Microseconds()}, args...)...).Scan(&known)
	distinct := map[string]bool{}
	for _, u := range userIDs {
		distinct[u] = true
	}
	return err == nil && known == len(distinct), err
}

// RecordFoundryRelayChannels records channels the live relayed install's
// proxied calls may name from now (users.conversations' answer, which the
// caller rid of externally shared ones); ErrNotFound when it isn't one.
func (s *mysqlDB) RecordFoundryRelayChannels(ctx context.Context, installID string, channelIDs []string) error {
	for _, c := range channelIDs {
		if !model.ValidSlackID(c) {
			return fmt.Errorf("relay channel %q is not a Slack id", c)
		}
	}
	var live bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_slack_installs WHERE id=? AND revoked_at IS NULL AND relay_link_id IS NOT NULL)`,
		installID).Scan(&live)
	if err != nil || !live {
		return cmp.Or(err, ErrNotFound)
	}
	return s.seeFoundryRelay(ctx, "foundry_relay_channels", "channel_id", installID, channelIDs)
}

// seeFoundryRelay records ids, in table's column, as seen now by the
// install, 500 a statement, in order, so two writers lock their rows in the
// same order.
func (s *mysqlDB) seeFoundryRelay(ctx context.Context, table, column, installID string, ids []string) error {
	ids = slices.Compact(slices.Sorted(slices.Values(ids)))
	for len(ids) > 0 {
		batch := ids[:min(len(ids), 500)]
		ids = ids[len(batch):]
		values := strings.TrimSuffix(strings.Repeat("(?, ?, NOW(6)), ", len(batch)), ", ")
		args := make([]any, 0, 2*len(batch))
		for _, v := range batch {
			args = append(args, installID, v)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO `+table+` (install_id, `+column+`, last_at) VALUES `+values+`
ON DUPLICATE KEY UPDATE last_at=NOW(6)`, args...); err != nil {
			return err
		}
	}
	return nil
}

// CountFoundryRelayUsage adds u to the link's counts of today (UTC).
func (s *mysqlDB) CountFoundryRelayUsage(ctx context.Context, linkID string, u model.RelayUsage) error {
	if u.Deliveries < 0 || u.Calls < 0 || u.Refused < 0 || u.FeedRows < 0 || u.FeedBytes < 0 {
		return fmt.Errorf("relay usage only grows")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_relay_usage (link_id, day, deliveries, calls, refused, feed_rows, feed_bytes)
VALUES (?, UTC_DATE(), ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE deliveries=deliveries+VALUES(deliveries), calls=calls+VALUES(calls), refused=refused+VALUES(refused),
  feed_rows=feed_rows+VALUES(feed_rows), feed_bytes=feed_bytes+VALUES(feed_bytes)`, linkID, u.Deliveries, u.Calls, u.Refused, u.FeedRows, u.FeedBytes)
	return err
}

// GetFoundryRelayUsage returns the link's counts of a UTC day (zero when it
// did nothing).
func (s *mysqlDB) GetFoundryRelayUsage(ctx context.Context, linkID string, day time.Time) (model.RelayUsage, error) {
	var u model.RelayUsage
	err := s.db.QueryRowContext(ctx, `SELECT deliveries, calls, refused, feed_rows, feed_bytes FROM foundry_relay_usage WHERE link_id=? AND day=?`,
		linkID, model.SlackDay(day)).Scan(&u.Deliveries, &u.Calls, &u.Refused, &u.FeedRows, &u.FeedBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return u, nil
	}
	return u, err
}

// --- The local install's mirror ---

// MirrorFoundrySlackInstalls makes a local install's Slack installs the ones
// its link's claim listed, in one transaction: each listed one (serving its
// own workspace; never a token: its calls go through the relay) is written
// as it is, unless it was revoked here, which it stays; a new one revokes
// the live install of its workspace or team it replaces, gets its Tier-1
// slots and takes over the feeds of its team (repointFoundrySlackFeeds); one
// of a workspace that is gone is left out. Every live install not listed is
// revoked and, with no token to tell Slack with, uninstalled, as a
// disconnect clears one. An unlinked install mirrors none.
func (s *mysqlDB) MirrorFoundrySlackInstalls(ctx context.Context, installs []*model.SlackInstall) error {
	listed := make([]string, 0, len(installs))
	for _, in := range installs {
		if err := in.Validate(); err != nil {
			return err
		}
		if _, ok := in.Destination.(model.SlackServesRelay); ok {
			return fmt.Errorf("a mirrored Slack install serves its own workspace")
		}
		listed = append(listed, in.ID)
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		gone := `SELECT id FROM foundry_slack_installs WHERE revoked_at IS NULL`
		var args []any
		if len(listed) > 0 {
			in, idArgs := idPlaceholders(len(listed)), toAnySlice(listed)
			gone, args = gone+` AND id NOT IN (`+in+`)`, idArgs
		}
		if err := tx.revokeFoundrySlackMirror(ctx, gone, args...); err != nil {
			return err
		}
		for _, in := range installs {
			if err := tx.mirrorFoundrySlackInstall(ctx, in); err != nil {
				return err
			}
		}
		return nil
	})
}

// mirrorFoundrySlackInstall writes one listed install (MirrorFoundrySlackInstalls).
func (s *mysqlDB) mirrorFoundrySlackInstall(ctx context.Context, in *model.SlackInstall) error {
	scopes := strings.Join(in.Scopes, ",")
	var revoked bool
	err := s.db.QueryRowContext(ctx, `SELECT revoked_at IS NOT NULL FROM foundry_slack_installs WHERE id=? FOR UPDATE`, in.ID).Scan(&revoked)
	switch {
	case err == nil && revoked:
		return nil
	case err == nil:
		_, err := s.db.ExecContext(ctx, `UPDATE foundry_slack_installs
SET enterprise_id=?, team_name=?, app_id=?, bot_user_id=?, bot_id=?, installer_admin=?, scopes=?, installed_by_user_id=?,
  installed_by_slack_user_id=?, updated_at=NOW(6)
WHERE id=?`, in.EnterpriseID, in.TeamName, in.AppID, in.BotUserID, in.BotID, in.InstallerAdmin, scopes, in.InstalledByUserID,
			in.InstalledBySlackUserID, in.ID)
		return err
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	switch err := s.workspaces.Lock(ctx, s.db, in.WorkspaceID); {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	if err := s.revokeFoundrySlackMirror(ctx, `SELECT id FROM foundry_slack_installs WHERE live_destination=? OR live_team=?`,
		model.SlackDestinationKey(in.WorkspaceID, model.SlackServesWorkspace{}), in.TeamID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_slack_installs
  (id, workspace_id, team_id, enterprise_id, team_name, app_id, bot_user_id, bot_id, installer_admin, scopes, installed_by_user_id,
   installed_by_slack_user_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, in.ID, in.WorkspaceID, in.TeamID, in.EnterpriseID, in.TeamName, in.AppID, in.BotUserID, in.BotID,
		in.InstallerAdmin, scopes, in.InstalledByUserID, in.InstalledBySlackUserID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_slack_slots (install_id, method) VALUES (?, ?), (?, ?)`,
		in.ID, model.SlackSlotHistory, in.ID, model.SlackSlotReplies); err != nil {
		return err
	}
	return s.repointFoundrySlackFeeds(ctx, in.WorkspaceID, in.TeamID, in.ID)
}

// revokeFoundrySlackMirror revokes and uninstalls the live installs pick
// selects with args, clearing them as a disconnect does.
func (s *mysqlDB) revokeFoundrySlackMirror(ctx context.Context, pick string, args ...any) error {
	rows, err := s.db.QueryContext(ctx, pick+` FOR UPDATE`, args...)
	if err != nil {
		return err
	}
	ids, err := scanStrings(rows)
	if err != nil || len(ids) == 0 {
		return err
	}
	in, idArgs := idPlaceholders(len(ids)), toAnySlice(ids)
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_slack_installs SET revoked_at=NOW(6), uninstalled_at=NOW(6) WHERE id IN (`+in+`)`,
		idArgs...); err != nil {
		return err
	}
	return s.clearFoundrySlackInstall(ctx, `SELECT id FROM foundry_slack_installs WHERE id IN (`+in+`)`, false, idArgs...)
}

// --- Scans ---

func scanFoundryRelayLink(scanner rowScanner) (*model.RelayLink, error) {
	var l model.RelayLink
	var seen, revoked sql.NullTime
	var addr sql.NullString
	if err := scanner.Scan(&l.ID, &l.WorkspaceID, &l.CreatedByUserID, &l.Name, &l.RedirectOrigin, &l.CreatedAt, &seen, &addr, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	l.LastSeenAt, l.LastSeenAddr, l.RevokedAt = timePtr(seen), addr.String, timePtr(revoked)
	return &l, nil
}

func scanFoundryRelayDelivery(scanner rowScanner) (*model.RelayDelivery, error) {
	var d model.RelayDelivery
	var kind, status string
	var payload sql.NullString
	var lease, noticed, finished sql.NullTime
	if err := scanner.Scan(&d.ID, &d.InstallID, &d.LinkID, &d.LocalWorkspaceID, &d.DedupeKey, &kind, &d.Conversation, &d.SlackUserID, &payload,
		&status, &d.Attempts, &lease, &noticed, &d.CreatedAt, &d.ExpiresAt, &finished); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var err error
	if d.Kind, err = model.ParseSlackIntakeKind(kind); err != nil {
		return nil, fmt.Errorf("read relay delivery %s: %w", d.ID, err)
	}
	if d.Status, err = model.ParseRelayDeliveryStatus(status); err != nil {
		return nil, fmt.Errorf("read relay delivery %s: %w", d.ID, err)
	}
	if payload.Valid {
		d.Payload = json.RawMessage(payload.String)
	}
	d.LeaseUntil, d.NoticedAt, d.FinishedAt = timePtr(lease), timePtr(noticed), timePtr(finished)
	return &d, nil
}

func scanFoundryRelayFeedItem(scanner rowScanner) (model.RelayFeedItem, error) {
	var it model.RelayFeedRow
	var kind, payload string
	if err := scanner.Scan(&it.ID, &it.InstallID, &it.LocalWorkspaceID, &it.Row.DedupeKey, &it.Row.ChannelID, &it.Row.Day, &kind, &payload); err != nil {
		return nil, err
	}
	var err error
	if it.Row.Kind, err = model.ParseSlackFeedRowKind(kind); err != nil {
		return nil, fmt.Errorf("read relay feed item %s: %w", it.ID, err)
	}
	if it.Row.Kind == model.SlackRowGap {
		gap, err := parseFoundryRelayGap(it.Row.ChannelID, payload)
		if err != nil {
			return nil, fmt.Errorf("read relay feed item %s: %w", it.ID, err)
		}
		return model.RelayFeedGap{ID: it.ID, InstallID: it.InstallID, LocalWorkspaceID: it.LocalWorkspaceID, Gap: gap}, nil
	}
	it.Row.Day, it.Row.Source, it.Row.Payload = model.SlackDay(it.Row.Day), model.SlackFeedFromEvent, json.RawMessage(payload)
	return it, nil
}

// parseFoundryRelayGap reads a gap row's payload (model.SlackFeedGap.Row's).
func parseFoundryRelayGap(channelID, payload string) (model.SlackFeedGap, error) {
	var span struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	}
	if err := json.Unmarshal([]byte(payload), &span); err != nil {
		return model.SlackFeedGap{}, fmt.Errorf("a relay gap's payload is its span: %w", err)
	}
	g := model.SlackFeedGap{ChannelID: channelID, From: span.From.UTC(), To: span.To.UTC()}
	return g, g.Validate()
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
