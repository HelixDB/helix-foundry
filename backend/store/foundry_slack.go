package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Helix Foundry for Slack (migrations/20261213000000_foundry_slack.sql):
// installs, their sealed tokens and the OAuth states that start them; linked
// Slack users and their one-time link tokens; the Founder thread each Slack
// thread continues; intake, what Slack sent, claimed one conversation row at
// a time under a fenced lease; and replies, the turns a worker follows into
// Slack under a lease it keeps for the whole follow.
//
// Leases are fenced: a claim writes a fresh random lease_owner, which every
// write of the claim names, so a worker whose lease was reclaimed changes
// nothing (ErrNotFound). Times are the database's (NOW(6)), so replicas'
// clocks never disagree about a lease.

const foundrySlackInstallColumns = `id, workspace_id, team_id, enterprise_id, team_name, app_id, bot_user_id, scopes, installed_by_user_id,
       installed_by_slack_user_id, created_at, updated_at, revoked_at, uninstalled_at`

const foundrySlackThreadColumns = `s.id, s.install_id, s.channel_id, s.thread_ts, s.slack_user_id, s.agent_thread_id, s.created_at, s.last_at`

const foundrySlackIntakeColumns = `id, install_id, dedupe_key, kind, conversation, slack_user_id, payload,
       JSON_UNQUOTE(JSON_EXTRACT(payload, '$.input')), status, attempts, next_attempt_at, lease_owner, lease_until, waits_on_turn, error,
       created_at, finished_at`

const foundrySlackReplyColumns = `turn_id, install_id, channel_id, thread_ts, slack_user_id, message_ts, after_seq, posted, status, lease_owner,
       lease_until, next_poll_at, next_update_at, created_at, finished_at`

// goneFoundrySlackThread is a Slack thread mapping s whose Founder thread
// was deleted, or is gone.
const goneFoundrySlackThread = `NOT EXISTS (SELECT 1 FROM foundry_agent_threads t WHERE t.id=s.agent_thread_id AND t.deleted_at IS NULL)`

// dueFoundrySlackThread is a Slack thread mapping s whose Founder thread's
// Slack-born text is due for the purge: idle since before its argument (a
// duration in microseconds), its install revoked or gone, or its Founder
// thread deleted or gone. It reads the install and the Founder thread in
// subqueries, so a locking read of s locks s alone: Vitess cannot parse
// FOR UPDATE OF.
const dueFoundrySlackThread = `(s.last_at < NOW(6) - INTERVAL ? MICROSECOND
  OR NOT EXISTS (SELECT 1 FROM foundry_slack_installs i WHERE i.id=s.install_id AND i.revoked_at IS NULL)
  OR ` + goneFoundrySlackThread + `)`

// --- Installs ---

// GetFoundrySlackInstall returns the workspace's live install; ErrNotFound
// when it has none.
func (s *mysqlDB) GetFoundrySlackInstall(ctx context.Context, workspaceID string) (*model.SlackInstall, error) {
	return scanFoundrySlackInstall(s.db.QueryRowContext(ctx, "SELECT "+foundrySlackInstallColumns+`
FROM foundry_slack_installs WHERE live_workspace=?`, workspaceID))
}

// GetFoundrySlackInstallByTeam returns the Slack team's live install, which
// Slack's requests are resolved to; ErrNotFound when it has none.
func (s *mysqlDB) GetFoundrySlackInstallByTeam(ctx context.Context, teamID string) (*model.SlackInstall, error) {
	return scanFoundrySlackInstall(s.db.QueryRowContext(ctx, "SELECT "+foundrySlackInstallColumns+`
FROM foundry_slack_installs WHERE live_team=?`, teamID))
}

// GetFoundrySlackInstallByID returns an install, live or revoked.
func (s *mysqlDB) GetFoundrySlackInstallByID(ctx context.Context, id string) (*model.SlackInstall, error) {
	return scanFoundrySlackInstall(s.db.QueryRowContext(ctx, "SELECT "+foundrySlackInstallColumns+`
FROM foundry_slack_installs WHERE id=?`, id))
}

// GetFoundrySlackInstallToken returns an install's sealed bot token, kept
// until Slack is told of its uninstall (ErrNotFound after).
func (s *mysqlDB) GetFoundrySlackInstallToken(ctx context.Context, installID string) (model.SlackSealedToken, error) {
	var t model.SlackSealedToken
	err := s.db.QueryRowContext(ctx, `SELECT token_sealed, key_id FROM foundry_slack_install_tokens WHERE install_id=?`, installID).
		Scan(&t.Sealed, &t.KeyID)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// ListFoundrySlackUninstalls returns up to limit revoked installs Slack
// hasn't been told of yet, longest revoked first, for the sweep to call
// apps.uninstall with their tokens and then MarkFoundrySlackUninstalled.
func (s *mysqlDB) ListFoundrySlackUninstalls(ctx context.Context, limit int) ([]*model.SlackInstall, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundrySlackInstallColumns+`
FROM foundry_slack_installs WHERE revoked_at IS NOT NULL AND uninstalled_at IS NULL ORDER BY revoked_at, id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundrySlackInstall)
}

// InstallFoundrySlack records an OAuth install of in.TeamID for
// in.WorkspaceID, with the bot token seal makes for the install's id, in one
// transaction holding the workspace's lock (ErrNotFound when it is deleted):
//   - a revoked install of the team that Slack hasn't been told of refuses it
//     (model.SlackDisconnecting): the sweep's apps.uninstall must never reach
//     a newer install;
//   - a live install of the team for another workspace refuses it
//     (model.SlackTeamTaken);
//   - a live install of another team for the workspace refuses it
//     (model.SlackInstalled);
//   - a live install of the team for the workspace is a reinstall: it keeps
//     its id, links and threads, and takes in's names, scopes, installers and
//     a new token (reinstalled true);
//   - else in is inserted with its token.
//
// It returns the install as stored. Two workspaces installing one team at
// once may deadlock on the team's gap lock: the loser runs again, and is
// refused.
func (s *mysqlDB) InstallFoundrySlack(ctx context.Context, in *model.SlackInstall,
	seal func(installID string) (model.SlackSealedToken, error)) (*model.SlackInstall, bool, error) {
	if err := in.Validate(); err != nil {
		return nil, false, err
	}
	var out *model.SlackInstall
	reinstalled := false
	install := func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		out, reinstalled = nil, false
		if err := tx.workspaces.Lock(ctx, tx.db, in.WorkspaceID); err != nil {
			return err
		}
		rows, err := tx.db.QueryContext(ctx, "SELECT "+foundrySlackInstallColumns+`
FROM foundry_slack_installs WHERE (team_id=? AND uninstalled_at IS NULL) OR live_workspace=? ORDER BY revoked_at IS NULL FOR UPDATE`,
			in.TeamID, in.WorkspaceID)
		if err != nil {
			return err
		}
		held, err := scanAll(rows, scanFoundrySlackInstall)
		if err != nil {
			return err
		}
		id := in.ID
		for _, h := range held {
			switch {
			case h.TeamID == in.TeamID && h.RevokedAt != nil:
				return model.SlackRefusal{Reason: model.SlackDisconnecting,
					Detail: "Helix is still disconnecting this Slack workspace; try again in a minute"}
			case h.TeamID == in.TeamID && h.WorkspaceID != in.WorkspaceID:
				return model.SlackRefusal{Reason: model.SlackTeamTaken,
					Detail: "this Slack workspace is connected to another Foundry workspace"}
			case h.TeamID != in.TeamID:
				return model.SlackRefusal{Reason: model.SlackInstalled,
					Detail: "this Foundry workspace is connected to another Slack workspace; disconnect it first"}
			}
			id, reinstalled = h.ID, true
		}
		token, err := seal(id)
		if err != nil {
			return fmt.Errorf("seal the Slack install's token: %w", err)
		}
		if len(token.Sealed) == 0 || token.KeyID == "" {
			return fmt.Errorf("a Slack install's token is sealed under a key")
		}
		scopes := strings.Join(in.Scopes, ",")
		if reinstalled {
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_slack_installs
SET enterprise_id=?, team_name=?, app_id=?, bot_user_id=?, scopes=?, installed_by_user_id=?, installed_by_slack_user_id=?, updated_at=NOW(6)
WHERE id=?`, in.EnterpriseID, in.TeamName, in.AppID, in.BotUserID, scopes, in.InstalledByUserID, in.InstalledBySlackUserID, id); err != nil {
				return err
			}
		} else {
			_, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_slack_installs
  (id, workspace_id, team_id, enterprise_id, team_name, app_id, bot_user_id, scopes, installed_by_user_id, installed_by_slack_user_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, in.WorkspaceID, in.TeamID, in.EnterpriseID, in.TeamName, in.AppID, in.BotUserID, scopes,
				in.InstalledByUserID, in.InstalledBySlackUserID)
			var me *mysql.MySQLError
			switch {
			case errors.As(err, &me) && me.Number == 1062 && strings.Contains(me.Message, "foundry_slack_installs_team_uq"):
				// Another workspace's install of the team committed first.
				return model.SlackRefusal{Reason: model.SlackTeamTaken,
					Detail: "this Slack workspace is connected to another Foundry workspace"}
			case isDuplicateKey(err):
				return ErrConflict
			case err != nil:
				return err
			}
		}
		if _, err := tx.db.ExecContext(ctx, `REPLACE INTO foundry_slack_install_tokens (install_id, token_sealed, key_id) VALUES (?, ?, ?)`,
			id, token.Sealed, token.KeyID); err != nil {
			return err
		}
		out, err = scanFoundrySlackInstall(tx.db.QueryRowContext(ctx, "SELECT "+foundrySlackInstallColumns+` FROM foundry_slack_installs WHERE id=?`, id))
		return err
	}
	err := s.WithTx(ctx, install)
	for tries := 1; isDeadlock(err) && tries < 3; tries++ {
		err = s.WithTx(ctx, install)
	}
	if err != nil {
		return nil, false, err
	}
	return out, reinstalled, nil
}

// RevokeFoundrySlackInstall disconnects the workspace's live install
// installID (ErrNotFound when it isn't one): it is revoked, and, when
// uninstalled (apps.uninstall succeeded), uninstalled with its token
// deleted; else the token stays for the sweep (ListFoundrySlackUninstalls).
// Its links, link tokens, open intake and replies are deleted; its threads'
// Slack-born text is due for PurgeFoundrySlackThreads at once.
func (s *mysqlDB) RevokeFoundrySlackInstall(ctx context.Context, workspaceID, installID string, uninstalled bool) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := execOrNotFound(ctx, tx.db, `UPDATE foundry_slack_installs SET revoked_at=NOW(6), uninstalled_at=IF(?, NOW(6), NULL)
WHERE id=? AND workspace_id=? AND revoked_at IS NULL`, uninstalled, installID, workspaceID); err != nil {
			return err
		}
		if uninstalled {
			if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_slack_install_tokens WHERE install_id=?`, installID); err != nil {
				return err
			}
		}
		return tx.clearFoundrySlackInstall(ctx, `SELECT ?`, installID, false)
	})
}

// MarkFoundrySlackUninstalled records that Slack was told of a revoked
// install's uninstall, deleting its token; ErrNotFound when it isn't revoked
// or Slack knew already.
func (s *mysqlDB) MarkFoundrySlackUninstalled(ctx context.Context, installID string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := execOrNotFound(ctx, tx.db, `UPDATE foundry_slack_installs SET uninstalled_at=NOW(6)
WHERE id=? AND revoked_at IS NOT NULL AND uninstalled_at IS NULL`, installID); err != nil {
			return err
		}
		_, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_slack_install_tokens WHERE install_id=?`, installID)
		return err
	})
}

// UninstallFoundrySlackTeam takes Slack's app_uninstalled or (bot)
// tokens_revoked event for teamID, sent at eventTime, in one transaction:
// a team with no live install is model.SlackUninstallUnknownTeam, and
// nothing is recorded; else the event is recorded in intake, done, as
// intakeID under dedupeKey (model.SlackUninstallDuplicate when it was
// already). An event sent before the live install's last install or
// reinstall (Slack's event_time is whole seconds, so to the second) is
// model.SlackUninstallStale and changes nothing more; else the install is
// revoked and uninstalled, and its token, links, link tokens, open intake
// and replies are deleted (model.SlackUninstallDone). No Slack call is made.
func (s *mysqlDB) UninstallFoundrySlackTeam(ctx context.Context, teamID, intakeID, dedupeKey string, eventTime time.Time) (
	model.SlackUninstallOutcome, error) {
	if intakeID == "" || !model.ValidSlackDedupe(dedupeKey) {
		return "", fmt.Errorf("a Slack uninstall is recorded under an intake id and an event's dedupe key")
	}
	var outcome model.SlackUninstallOutcome
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		live, err := scanFoundrySlackInstall(tx.db.QueryRowContext(ctx, "SELECT "+foundrySlackInstallColumns+`
FROM foundry_slack_installs WHERE live_team=? FOR UPDATE`, teamID))
		if errors.Is(err, ErrNotFound) {
			outcome = model.SlackUninstallUnknownTeam
			return nil
		}
		if err != nil {
			return err
		}
		_, err = tx.db.ExecContext(ctx, `INSERT INTO foundry_slack_intake (id, install_id, dedupe_key, kind, status, finished_at)
VALUES (?, ?, ?, 'event', 'done', NOW(6))`, intakeID, live.ID, dedupeKey)
		if isDuplicateKey(err) {
			outcome = model.SlackUninstallDuplicate
			return nil
		}
		if err != nil {
			return err
		}
		if eventTime.Before(live.UpdatedAt.Truncate(time.Second)) {
			outcome = model.SlackUninstallStale
			return nil
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_slack_installs SET revoked_at=NOW(6), uninstalled_at=NOW(6) WHERE id=?`,
			live.ID); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_slack_install_tokens WHERE install_id=?`, live.ID); err != nil {
			return err
		}
		outcome = model.SlackUninstallDone
		return tx.clearFoundrySlackInstall(ctx, `SELECT ?`, live.ID, false)
	})
	if err != nil {
		return "", err
	}
	return outcome, nil
}

// PurgeFoundrySlack ends a deleted workspace's Slack: its live install is
// revoked (the sweep tells Slack), its OAuth states, and every install's
// links, link tokens, intake and replies, are deleted; its threads'
// Slack-born text is due for PurgeFoundrySlackThreads at once. It runs plain
// statements, so its host may run it in the transaction that deletes the
// workspace (InTx); running it again changes nothing.
func (s *mysqlDB) PurgeFoundrySlack(ctx context.Context, workspaceID string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_slack_installs SET revoked_at=NOW(6) WHERE workspace_id=? AND revoked_at IS NULL`,
		workspaceID); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_slack_install_states WHERE workspace_id=?`, workspaceID); err != nil {
		return err
	}
	return s.clearFoundrySlackInstall(ctx, `SELECT id FROM foundry_slack_installs WHERE workspace_id=?`, workspaceID, true)
}

// clearFoundrySlackInstall deletes the links, link tokens, replies and open
// intake (every intake row when all) of the installs installs selects with
// arg. Done and failed intake rows hold no payload, and keep deduping
// Slack's retries until they are pruned.
func (s *mysqlDB) clearFoundrySlackInstall(ctx context.Context, installs string, arg any, all bool) error {
	intake := `DELETE FROM foundry_slack_intake WHERE install_id IN (` + installs + `) AND status IN ('queued', 'running')`
	if all {
		intake = `DELETE FROM foundry_slack_intake WHERE install_id IN (` + installs + `)`
	}
	for _, stmt := range []string{
		`DELETE FROM foundry_slack_links WHERE install_id IN (` + installs + `)`,
		`DELETE FROM foundry_slack_link_tokens WHERE install_id IN (` + installs + `)`,
		`DELETE FROM foundry_slack_replies WHERE install_id IN (` + installs + `)`,
		intake,
	} {
		if _, err := s.db.ExecContext(ctx, stmt, arg); err != nil {
			return err
		}
	}
	return nil
}

// --- Install states ---

// CreateFoundrySlackInstallState records an install the member userID
// started for workspaceID: its state's SHA-256 (hex), live for ttl.
// ErrConflict when the hash is taken.
func (s *mysqlDB) CreateFoundrySlackInstallState(ctx context.Context, workspaceID, userID, stateHash string, ttl time.Duration) error {
	if workspaceID == "" || userID == "" || !model.ValidSHA256Hex(stateHash) || ttl <= 0 {
		return fmt.Errorf("a Slack install state names its workspace and member, a SHA-256 and a lifetime")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_slack_install_states (state_hash, workspace_id, user_id, expires_at)
VALUES (?, ?, ?, NOW(6) + INTERVAL ? MICROSECOND)`, stateHash, workspaceID, userID, ttl.Microseconds())
	if isDuplicateKey(err) {
		return ErrConflict
	}
	return err
}

// SpendFoundrySlackInstallState spends the state, once, for the member who
// started it: ErrNotFound when it is absent, expired, spent or another's
// (which leaves it unspent).
func (s *mysqlDB) SpendFoundrySlackInstallState(ctx context.Context, stateHash, userID string) (*model.SlackInstallState, error) {
	var out model.SlackInstallState
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		err := tx.db.QueryRowContext(ctx, `SELECT state_hash, workspace_id, user_id, expires_at FROM foundry_slack_install_states
WHERE state_hash=? AND user_id=? AND used_at IS NULL AND expires_at > NOW(6) FOR UPDATE`, stateHash, userID).
			Scan(&out.StateHash, &out.WorkspaceID, &out.UserID, &out.ExpiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_slack_install_states SET used_at=NOW(6) WHERE state_hash=?`, stateHash)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// --- Links and link tokens ---

// GetFoundrySlackLink returns the Slack user's link in the install.
func (s *mysqlDB) GetFoundrySlackLink(ctx context.Context, installID, slackUserID string) (*model.SlackLink, error) {
	return scanFoundrySlackLink(s.db.QueryRowContext(ctx, `SELECT install_id, slack_user_id, user_id, created_at
FROM foundry_slack_links WHERE install_id=? AND slack_user_id=?`, installID, slackUserID))
}

// ListFoundrySlackLinks returns the install's links, oldest first.
func (s *mysqlDB) ListFoundrySlackLinks(ctx context.Context, installID string) ([]*model.SlackLink, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT install_id, slack_user_id, user_id, created_at
FROM foundry_slack_links WHERE install_id=? ORDER BY created_at, slack_user_id`, installID)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundrySlackLink)
}

// DeleteFoundrySlackLink unlinks the Slack user; ErrNotFound when it wasn't
// linked.
func (s *mysqlDB) DeleteFoundrySlackLink(ctx context.Context, installID, slackUserID string) error {
	return execOrNotFound(ctx, s.db, `DELETE FROM foundry_slack_links WHERE install_id=? AND slack_user_id=?`, installID, slackUserID)
}

// CreateFoundrySlackLinkToken records a link token sent to the Slack user of
// the live install (ErrNotFound when it isn't live): its SHA-256 (hex),
// live for ttl. The user's earlier unspent tokens are deleted, so one at
// most is live.
func (s *mysqlDB) CreateFoundrySlackLinkToken(ctx context.Context, installID, slackUserID, tokenHash string, ttl time.Duration) error {
	if installID == "" || !model.ValidSlackID(slackUserID) || !model.ValidSHA256Hex(tokenHash) || ttl <= 0 {
		return fmt.Errorf("a Slack link token names its install and Slack user, a SHA-256 and a lifetime")
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var id string
		err := tx.db.QueryRowContext(ctx, `SELECT id FROM foundry_slack_installs WHERE id=? AND revoked_at IS NULL FOR UPDATE`, installID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_slack_link_tokens WHERE install_id=? AND slack_user_id=? AND used_at IS NULL`,
			installID, slackUserID); err != nil {
			return err
		}
		_, err = tx.db.ExecContext(ctx, `INSERT INTO foundry_slack_link_tokens (token_hash, install_id, slack_user_id, expires_at)
VALUES (?, ?, ?, NOW(6) + INTERVAL ? MICROSECOND)`, tokenHash, installID, slackUserID, ttl.Microseconds())
		if isDuplicateKey(err) {
			return ErrConflict
		}
		return err
	})
}

// GetFoundrySlackLinkToken returns a live link token (unspent, unexpired, of
// a live install); ErrNotFound otherwise.
func (s *mysqlDB) GetFoundrySlackLinkToken(ctx context.Context, tokenHash string) (*model.SlackLinkToken, error) {
	var t model.SlackLinkToken
	err := s.db.QueryRowContext(ctx, `SELECT k.token_hash, k.install_id, k.slack_user_id, k.expires_at
FROM foundry_slack_link_tokens k JOIN foundry_slack_installs i ON i.id=k.install_id AND i.revoked_at IS NULL
WHERE k.token_hash=? AND k.used_at IS NULL AND k.expires_at > NOW(6)`, tokenHash).Scan(&t.TokenHash, &t.InstallID, &t.SlackUserID, &t.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// LinkFoundrySlackUser spends a live link token of workspaceID's live
// install (ErrNotFound otherwise, and nothing is spent) and links its Slack
// user to userID, replacing any link of that Slack user or of userID in the
// install: one Slack user acts as one Foundry user, and one Foundry user has
// one Slack user an install. The caller checks who userID is first.
func (s *mysqlDB) LinkFoundrySlackUser(ctx context.Context, workspaceID, tokenHash, userID string) (*model.SlackLink, error) {
	if userID == "" {
		return nil, fmt.Errorf("a Slack link names its Foundry user")
	}
	var out *model.SlackLink
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var installID, slackUserID string
		err := tx.db.QueryRowContext(ctx, `SELECT k.install_id, k.slack_user_id
FROM foundry_slack_link_tokens k JOIN foundry_slack_installs i ON i.id=k.install_id AND i.revoked_at IS NULL AND i.workspace_id=?
WHERE k.token_hash=? AND k.used_at IS NULL AND k.expires_at > NOW(6) FOR UPDATE`, workspaceID, tokenHash).Scan(&installID, &slackUserID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		for _, stmt := range []struct {
			q    string
			args []any
		}{
			{`UPDATE foundry_slack_link_tokens SET used_at=NOW(6) WHERE token_hash=?`, []any{tokenHash}},
			{`DELETE FROM foundry_slack_links WHERE install_id=? AND (slack_user_id=? OR user_id=?)`, []any{installID, slackUserID, userID}},
			{`INSERT INTO foundry_slack_links (install_id, slack_user_id, user_id) VALUES (?, ?, ?)`, []any{installID, slackUserID, userID}},
		} {
			if _, err := tx.db.ExecContext(ctx, stmt.q, stmt.args...); err != nil {
				return err
			}
		}
		out, err = tx.GetFoundrySlackLink(ctx, installID, slackUserID)
		return err
	})
	return out, err
}

// --- Threads ---

// GetFoundrySlackThread returns the Founder thread the Slack thread
// continues for slackUserID ("" in bot mode); ErrNotFound when it has none,
// or its Founder thread was deleted (which counts as none).
func (s *mysqlDB) GetFoundrySlackThread(ctx context.Context, installID, channelID, threadTS, slackUserID string) (*model.SlackThread, error) {
	return scanFoundrySlackThread(s.db.QueryRowContext(ctx, "SELECT "+foundrySlackThreadColumns+`
FROM foundry_slack_threads s JOIN foundry_agent_threads t ON t.id=s.agent_thread_id AND t.deleted_at IS NULL
WHERE s.install_id=? AND s.channel_id=? AND s.thread_ts=? AND s.slack_user_id=?`, installID, channelID, threadTS, slackUserID))
}

// CreateFoundrySlackThread records the Founder thread a Slack thread now
// continues. A mapping is never overwritten while its Founder thread lives
// (ErrConflict, the existing one kept); one whose Founder thread was deleted
// is purged (PurgeFoundrySlackThreads) and replaced, in one transaction.
func (s *mysqlDB) CreateFoundrySlackThread(ctx context.Context, m *model.SlackThread) error {
	switch {
	case m.ID == "" || m.InstallID == "" || m.AgentThreadID == "":
		return fmt.Errorf("a Slack thread names itself, its install and its Founder thread")
	case !model.ValidSlackID(m.ChannelID) || !model.ValidSlackTS(m.ThreadTS) || m.SlackUserID != "" && !model.ValidSlackID(m.SlackUserID):
		return fmt.Errorf("a Slack thread names its channel, thread and Slack user")
	}
	insert := func(tx *mysqlDB) error {
		_, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_slack_threads (id, install_id, channel_id, thread_ts, slack_user_id, agent_thread_id)
VALUES (?, ?, ?, ?, ?, ?)`, m.ID, m.InstallID, m.ChannelID, m.ThreadTS, m.SlackUserID, m.AgentThreadID)
		return err
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		err := insert(tx)
		if !isDuplicateKey(err) {
			return err
		}
		var oldID string
		err = tx.db.QueryRowContext(ctx, `SELECT id FROM foundry_slack_threads WHERE install_id=? AND channel_id=? AND thread_ts=? AND slack_user_id=?`,
			m.InstallID, m.ChannelID, m.ThreadTS, m.SlackUserID).Scan(&oldID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrConflict // the id was taken
		}
		if err != nil {
			return err
		}
		switch err := tx.purgeDueFoundrySlackThread(ctx, oldID, goneFoundrySlackThread); {
		case errors.Is(err, ErrNotFound):
			// The Slack thread already continues a live Founder thread: one
			// Slack thread never makes two.
			return ErrConflict
		case err != nil:
			return err
		}
		return insert(tx)
	})
}

// TouchFoundrySlackThread records a Slack message the thread took now, which
// keeps its text from the purge for model.SlackThreadIdle.
func (s *mysqlDB) TouchFoundrySlackThread(ctx context.Context, id string) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_slack_threads SET last_at=NOW(6) WHERE id=?`, id)
}

// PurgeFoundrySlackThreads purges up to limit Slack-born Founder threads
// whose text is due: of a revoked install, or idle for idle
// (model.SlackThreadIdle) since their last Slack message. (A Founder thread
// deleted on the site is purged as it is deleted: DeleteFoundryAgentThread.)
// Each in a transaction of its own: its live turn is cancelled (charging
// what it reserved); its messages, events and actions are deleted; its turns
// are kept for the spend sums that read them, with their text (a message's
// input, answers, what they asked, page context) blanked; the thread is
// deleted, its title blanked; and its mapping is deleted. It returns how
// many it purged.
//
// Its candidates are read by index: a revoked install's mappings through the
// installs' revoked_at and the mappings' install_id, the idle ones through
// last_at; each is checked again under its locks.
func (s *mysqlDB) PurgeFoundrySlackThreads(ctx context.Context, idle time.Duration, limit int) (int, error) {
	if idle <= 0 || limit < 1 {
		return 0, fmt.Errorf("a Slack thread purge has an idle time and a limit")
	}
	var ids []string
	for _, q := range []struct {
		query string
		args  []any
	}{
		{foundrySlackRevokedThreads, nil},
		{foundrySlackIdleThreads, []any{idle.Microseconds()}},
	} {
		rows, err := s.db.QueryContext(ctx, q.query, append(q.args, limit-len(ids))...)
		if err != nil {
			return 0, err
		}
		more, err := scanAll(rows, func(r rowScanner) (string, error) {
			var id string
			return id, r.Scan(&id)
		})
		if err != nil {
			return 0, err
		}
		ids = append(ids, more...)
		if len(ids) >= limit {
			break
		}
	}
	purged := 0
	for _, id := range ids {
		err := s.WithTx(ctx, func(txDB Store) error {
			// Due again under the locks: a message since the read keeps it.
			return txDB.(*mysqlDB).purgeDueFoundrySlackThread(ctx, id, dueFoundrySlackThread, idle.Microseconds())
		})
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return purged, err
		default:
			purged++
		}
	}
	return purged, nil
}

// foundrySlackRevokedThreads selects up to a limit of the mappings of
// revoked installs (foundry_slack_installs_sweep_idx, then the mappings'
// unique key's install_id). An install is never deleted, so none is gone.
const foundrySlackRevokedThreads = `SELECT s.id FROM foundry_slack_installs i JOIN foundry_slack_threads s ON s.install_id=i.id
WHERE i.revoked_at IS NOT NULL LIMIT ?`

// foundrySlackIdleThreads selects up to a limit of the mappings idle since
// before its first argument (microseconds), longest idle first
// (foundry_slack_threads_last_idx).
const foundrySlackIdleThreads = `SELECT id FROM foundry_slack_threads WHERE last_at < NOW(6) - INTERVAL ? MICROSECOND ORDER BY last_at, id LIMIT ?`

// purgeDueFoundrySlackThread purges the mapping mappingID
// (purgeFoundrySlackThread) when due, a condition on the mapping s with
// args, holds under its locks; ErrNotFound when it doesn't, or the mapping
// is gone. It locks the Founder thread first, then the mapping: the order
// DeleteFoundryAgentThread takes them in.
func (s *mysqlDB) purgeDueFoundrySlackThread(ctx context.Context, mappingID, due string, args ...any) error {
	var agentThread string
	err := s.db.QueryRowContext(ctx, `SELECT agent_thread_id FROM foundry_slack_threads WHERE id=?`, mappingID).Scan(&agentThread)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var locked string
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM foundry_agent_threads WHERE id=? FOR UPDATE`, agentThread).Scan(&locked); err != nil &&
		!errors.Is(err, sql.ErrNoRows) {
		return err
	}
	err = s.db.QueryRowContext(ctx, `SELECT s.id FROM foundry_slack_threads s WHERE s.id=? AND s.agent_thread_id=? AND `+due+` FOR UPDATE`,
		append([]any{mappingID, agentThread}, args...)...).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.purgeFoundrySlackThread(ctx, mappingID, agentThread)
}

// purgeFoundrySlackThreadsOf purges the Slack-born text of agentThreadID,
// whose lock the caller holds, and deletes its mappings
// (DeleteFoundryAgentThread).
func (s *mysqlDB) purgeFoundrySlackThreadsOf(ctx context.Context, agentThreadID string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM foundry_slack_threads WHERE agent_thread_id=? FOR UPDATE`, agentThreadID)
	if err != nil {
		return err
	}
	mappings, err := scanAll(rows, func(r rowScanner) (string, error) {
		var id string
		return id, r.Scan(&id)
	})
	if err != nil {
		return err
	}
	for _, id := range mappings {
		if err := s.purgeFoundrySlackThread(ctx, id, agentThreadID); err != nil {
			return err
		}
	}
	return nil
}

// purgeFoundrySlackThread purges the Founder thread's Slack-born text and
// deletes the mapping (PurgeFoundrySlackThreads); the caller holds the
// Founder thread's lock and the mapping's.
func (s *mysqlDB) purgeFoundrySlackThread(ctx context.Context, mappingID, agentThreadID string) error {
	for _, stmt := range []string{
		`UPDATE foundry_agent_turns
SET status='cancelled', lease_owner=NULL, lease_until=NULL, usd_micros=usd_micros+reserved_micros, reserved_micros=0, finished_at=NOW(6)
WHERE thread_id=? AND status IN ('queued', 'running')`,
		`DELETE e FROM foundry_agent_events e JOIN foundry_agent_turns t ON t.id=e.turn_id WHERE t.thread_id=?`,
		`DELETE FROM foundry_agent_messages WHERE thread_id=?`,
		`DELETE FROM foundry_agent_actions WHERE thread_id=?`,
		// An answers turn's input is the asking turn's id, an action
		// result's the action's: only a message's is text.
		`UPDATE foundry_agent_turns SET input=IF(kind='message', '', input), answers=IF(answers IS NULL, NULL, JSON_ARRAY()), asked=NULL,
  page_context=NULL WHERE thread_id=?`,
		`UPDATE foundry_agent_threads SET title='', deleted_at=COALESCE(deleted_at, NOW(6)) WHERE id=?`,
	} {
		if _, err := s.db.ExecContext(ctx, stmt, agentThreadID); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM foundry_slack_threads WHERE id=?`, mappingID)
	return err
}

// --- Intake ---

// QueueFoundrySlackIntake queues what Slack sent; ErrConflict when its
// dedupe key was queued already (a retry, which Slack is answered 200).
func (s *mysqlDB) QueueFoundrySlackIntake(ctx context.Context, in *model.SlackIntake) error {
	if err := in.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_slack_intake (id, install_id, dedupe_key, kind, conversation, slack_user_id, payload)
VALUES (?, ?, ?, ?, ?, ?, ?)`, in.ID, in.InstallID, in.DedupeKey, string(in.Kind), in.Conversation, in.SlackUserID, string(in.Payload))
	if isDuplicateKey(err) {
		return ErrConflict
	}
	return err
}

// ClaimFoundrySlackIntake leases one due intake row for lease, under a fresh
// LeaseOwner: queued and due, or running under a lease that ran out; and,
// in a conversation, its oldest open (queued or running) row while no other
// row of it holds a live lease, so a conversation's messages run one at a
// time, in the order they came. (Rows can commit out of the order of their
// created_at: an older one that commits while a newer one runs waits for it.)
// Nil when none is due.
func (s *mysqlDB) ClaimFoundrySlackIntake(ctx context.Context, lease time.Duration) (*model.SlackIntake, error) {
	if lease <= 0 {
		return nil, fmt.Errorf("a Slack intake claim has a lease")
	}
	return claimFoundrySlack(ctx, s, `SELECT i.id FROM foundry_slack_intake i
WHERE ((i.status='queued' AND i.next_attempt_at <= NOW(6)) OR (i.status='running' AND i.lease_until < NOW(6)))
  AND (i.conversation='' OR NOT EXISTS (SELECT 1 FROM foundry_slack_intake o
    WHERE o.conversation=i.conversation AND o.status IN ('queued', 'running') AND o.id<>i.id
      AND ((o.status='running' AND o.lease_until >= NOW(6))
        OR o.created_at < i.created_at OR (o.created_at = i.created_at AND o.id < i.id))))
ORDER BY i.next_attempt_at, i.id LIMIT 1 FOR UPDATE SKIP LOCKED`,
		`UPDATE foundry_slack_intake SET status='running', attempts=attempts+1, lease_owner=?, lease_until=NOW(6) + INTERVAL ? MICROSECOND WHERE id=?`,
		lease, func(tx *mysqlDB, id string) (*model.SlackIntake, error) {
			return scanFoundrySlackIntake(tx.db.QueryRowContext(ctx, "SELECT "+foundrySlackIntakeColumns+` FROM foundry_slack_intake WHERE id=?`, id))
		})
}

// claimFoundrySlack leases the row pick selects (FOR UPDATE SKIP LOCKED) to
// a fresh random owner with lease, which set writes (owner, lease in
// microseconds, id), and reads it back with read, in one transaction. Nil
// when pick finds none.
func claimFoundrySlack[T any](ctx context.Context, s *mysqlDB, pick, set string, lease time.Duration,
	read func(tx *mysqlDB, id string) (*T, error)) (*T, error) {
	var out *T
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		out = nil
		var id string
		err := tx.db.QueryRowContext(ctx, pick).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		owner := make([]byte, 16)
		if _, err := rand.Read(owner); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, set, hex.EncodeToString(owner), lease.Microseconds(), id); err != nil {
			return err
		}
		out, err = read(tx, id)
		return err
	})
	return out, err
}

// SetFoundrySlackIntakeInput keeps the turn input the claim's first attempt
// rendered in the row's payload, for every retry to send again. ErrNotFound
// when the lease was lost, or the row has its input already.
func (s *mysqlDB) SetFoundrySlackIntakeInput(ctx context.Context, id, owner, input string) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_slack_intake SET payload=JSON_SET(payload, '$.input', ?)
WHERE id=? AND status='running' AND lease_owner=? AND JSON_CONTAINS_PATH(payload, 'one', '$.input')=0`, input, id, owner)
}

// EndFoundrySlackIntake ends the claim owner holds of the row with outcome;
// ErrNotFound when the lease was lost, and nothing changed:
//   - model.SlackIntakeEnded ends it done, model.SlackIntakeGaveUp failed with
//     why; either drops its payload;
//   - model.SlackIntakeRetry queues it again after a while, saying why;
//   - model.SlackIntakeWait queues it behind a busy thread's live turn for at
//     most a while, until WakeFoundrySlackIntake; a turn that already ended
//     wakes it at once.
func (s *mysqlDB) EndFoundrySlackIntake(ctx context.Context, id, owner string, outcome model.SlackIntakeOutcome) error {
	const owned = ` WHERE id=? AND status='running' AND lease_owner=?`
	const released = `lease_owner=NULL, lease_until=NULL`
	switch o := outcome.(type) {
	case model.SlackIntakeEnded:
		return execOrNotFound(ctx, s.db, `UPDATE foundry_slack_intake
SET status='done', payload=NULL, error=NULL, waits_on_turn=NULL, finished_at=NOW(6), `+released+owned, id, owner)
	case model.SlackIntakeGaveUp:
		if o.Error == "" {
			return fmt.Errorf("a failed Slack intake row says why")
		}
		return execOrNotFound(ctx, s.db, `UPDATE foundry_slack_intake
SET status='failed', payload=NULL, error=?, waits_on_turn=NULL, finished_at=NOW(6), `+released+owned, o.Error, id, owner)
	case model.SlackIntakeRetry:
		if o.After < 0 || o.Error == "" {
			return fmt.Errorf("a retried Slack intake row waits a while and says why")
		}
		return execOrNotFound(ctx, s.db, `UPDATE foundry_slack_intake
SET status='queued', next_attempt_at=NOW(6) + INTERVAL ? MICROSECOND, error=?, waits_on_turn=NULL, `+released+owned,
			o.After.Microseconds(), o.Error, id, owner)
	case model.SlackIntakeWait:
		if o.TurnID == "" || o.For <= 0 {
			return fmt.Errorf("a waiting Slack intake row names its turn and waits a while")
		}
		// Assignments run left to right: next_attempt_at reads the wait set.
		return execOrNotFound(ctx, s.db, `UPDATE foundry_slack_intake
SET status='queued',
  waits_on_turn=IF(EXISTS (SELECT 1 FROM foundry_agent_turns WHERE id=? AND status IN ('queued', 'running')), ?, NULL),
  next_attempt_at=IF(waits_on_turn IS NULL, NOW(6), NOW(6) + INTERVAL ? MICROSECOND), error=NULL, `+released+owned,
			o.TurnID, o.TurnID, o.For.Microseconds(), id, owner)
	}
	return fmt.Errorf("unknown Slack intake outcome %T", outcome)
}

// WakeFoundrySlackIntake wakes the rows waiting on turnID, which ended: they
// are due at once, their wait cleared. It returns how many.
func (s *mysqlDB) WakeFoundrySlackIntake(ctx context.Context, turnID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE foundry_slack_intake SET next_attempt_at=NOW(6), waits_on_turn=NULL
WHERE waits_on_turn=? AND status='queued'`, turnID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- Replies ---

// GetFoundrySlackReply returns the turn's reply.
func (s *mysqlDB) GetFoundrySlackReply(ctx context.Context, turnID string) (*model.SlackReply, error) {
	return scanFoundrySlackReply(s.db.QueryRowContext(ctx, "SELECT "+foundrySlackReplyColumns+` FROM foundry_slack_replies WHERE turn_id=?`, turnID))
}

// ListFoundrySlackFollows returns the replies still followed in a Slack
// thread, oldest first: what a stop in it stops.
func (s *mysqlDB) ListFoundrySlackFollows(ctx context.Context, installID, channelID, threadTS string) ([]*model.SlackReply, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundrySlackReplyColumns+` FROM foundry_slack_replies
WHERE install_id=? AND channel_id=? AND thread_ts=? AND status='following' ORDER BY created_at, turn_id`, installID, channelID, threadTS)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundrySlackReply)
}

// CountFoundrySlackFollows counts the install's replies still followed,
// which space its message updates.
func (s *mysqlDB) CountFoundrySlackFollows(ctx context.Context, installID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_slack_replies WHERE install_id=? AND status='following'`, installID).Scan(&n)
	return n, err
}

// HoldFoundrySlackReply records the turn's reply, if it has none, held for
// model.SlackReplyHeld: no follower takes it until the intake worker posts
// its first message (StartFoundrySlackReply), or, should that worker die,
// the hold runs out. It returns the reply as stored, whose MessageTS says
// whether the first message was posted.
func (s *mysqlDB) HoldFoundrySlackReply(ctx context.Context, r *model.SlackReply) (*model.SlackReply, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_slack_replies (turn_id, install_id, channel_id, thread_ts, slack_user_id, next_poll_at)
VALUES (?, ?, ?, ?, ?, NOW(6) + INTERVAL ? MICROSECOND) ON DUPLICATE KEY UPDATE turn_id=turn_id`,
		r.TurnID, r.InstallID, r.ChannelID, r.ThreadTS, r.SlackUserID, model.SlackReplyHeld.Microseconds()); err != nil {
		return nil, err
	}
	return s.GetFoundrySlackReply(ctx, r.TurnID)
}

// StartFoundrySlackReply records the reply's first message, messageTS, and
// makes it due for a follower now, while the intake claim (intakeID, owned
// by intakeOwner) that posted it holds its lease: ErrNotFound when it lost
// it, or the reply has its message already.
func (s *mysqlDB) StartFoundrySlackReply(ctx context.Context, turnID, messageTS, intakeID, intakeOwner string) error {
	if !model.ValidSlackTS(messageTS) {
		return fmt.Errorf("%q is not a Slack message ts", messageTS)
	}
	return execOrNotFound(ctx, s.db, `UPDATE foundry_slack_replies SET message_ts=?, next_poll_at=NOW(6)
WHERE turn_id=? AND status='following' AND message_ts IS NULL
  AND EXISTS (SELECT 1 FROM foundry_slack_intake WHERE id=? AND status='running' AND lease_owner=?)`, messageTS, turnID, intakeID, intakeOwner)
}

// ClaimFoundrySlackReply leases one due reply for lease, under a fresh
// LeaseOwner: following, due to be polled, and unleased or leased by a
// follower whose lease ran out. Nil when none is due.
func (s *mysqlDB) ClaimFoundrySlackReply(ctx context.Context, lease time.Duration) (*model.SlackReply, error) {
	if lease <= 0 {
		return nil, fmt.Errorf("a Slack reply claim has a lease")
	}
	return claimFoundrySlack(ctx, s, `SELECT turn_id FROM foundry_slack_replies
WHERE status='following' AND next_poll_at <= NOW(6) AND (lease_until IS NULL OR lease_until < NOW(6))
ORDER BY next_poll_at, turn_id LIMIT 1 FOR UPDATE SKIP LOCKED`,
		`UPDATE foundry_slack_replies SET lease_owner=?, lease_until=NOW(6) + INTERVAL ? MICROSECOND WHERE turn_id=?`,
		lease, func(tx *mysqlDB, id string) (*model.SlackReply, error) { return tx.GetFoundrySlackReply(ctx, id) })
}

// UpdateFoundrySlackReply writes a follower's progress and renews its lease,
// while owner holds it: ErrNotFound when it lost it, and nothing changed.
func (s *mysqlDB) UpdateFoundrySlackReply(ctx context.Context, turnID, owner string, u model.SlackReplyUpdate) error {
	if err := u.Validate(); err != nil {
		return err
	}
	sets := []string{"lease_until=NOW(6) + INTERVAL ? MICROSECOND"}
	args := []any{u.Lease.Microseconds()}
	if u.AfterSeq != nil {
		sets, args = append(sets, "after_seq=?"), append(args, *u.AfterSeq)
	}
	if u.MessageTS != nil {
		sets, args = append(sets, "message_ts=?"), append(args, *u.MessageTS)
	}
	if len(u.Posted) > 0 {
		posted, err := json.Marshal(u.Posted)
		if err != nil {
			return err
		}
		sets, args = append(sets, "posted=JSON_MERGE_PATCH(COALESCE(posted, JSON_OBJECT()), CAST(? AS JSON))"), append(args, string(posted))
	}
	if u.UpdateAfter != nil {
		sets, args = append(sets, "next_update_at=NOW(6) + INTERVAL ? MICROSECOND"), append(args, u.UpdateAfter.Microseconds())
	}
	return execOrNotFound(ctx, s.db, `UPDATE foundry_slack_replies SET `+joinComma(sets)+`
WHERE turn_id=? AND status='following' AND lease_owner=?`, append(args, turnID, owner)...)
}

// ReleaseFoundrySlackReply gives the follow up, due again after a while (a
// Slack rate limit's Retry-After, or a worker stopping), while owner holds
// its lease: ErrNotFound when it lost it.
func (s *mysqlDB) ReleaseFoundrySlackReply(ctx context.Context, turnID, owner string, after time.Duration) error {
	if after < 0 {
		return fmt.Errorf("a Slack reply is released for a while")
	}
	return execOrNotFound(ctx, s.db, `UPDATE foundry_slack_replies
SET next_poll_at=NOW(6) + INTERVAL ? MICROSECOND, lease_owner=NULL, lease_until=NULL
WHERE turn_id=? AND status='following' AND lease_owner=?`, after.Microseconds(), turnID, owner)
}

// FinishFoundrySlackReply ends the follow, done, while owner holds its
// lease: ErrNotFound when it lost it.
func (s *mysqlDB) FinishFoundrySlackReply(ctx context.Context, turnID, owner string) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_slack_replies
SET status='done', finished_at=NOW(6), lease_owner=NULL, lease_until=NULL
WHERE turn_id=? AND status='following' AND lease_owner=?`, turnID, owner)
}

// --- Retention ---

// PruneFoundrySlack deletes, up to limit rows of each, what Slack's
// retention lets go: intake rows older than intake
// (foundry.slack.retention.intake), replies done for replies
// (foundry.slack.retention.replies), and link tokens and install states
// expired for model.SlackSecretsKept. It returns how many rows it deleted.
func (s *mysqlDB) PruneFoundrySlack(ctx context.Context, intake, replies time.Duration, limit int) (int64, error) {
	if intake <= 0 || replies <= 0 || limit < 1 {
		return 0, fmt.Errorf("a Slack prune keeps intake and replies a while, and has a limit")
	}
	var deleted int64
	for _, p := range []struct {
		q    string
		kept time.Duration
	}{
		{`DELETE FROM foundry_slack_intake WHERE created_at < NOW(6) - INTERVAL ? MICROSECOND ORDER BY created_at LIMIT ?`, intake},
		{`DELETE FROM foundry_slack_replies WHERE status='done' AND finished_at < NOW(6) - INTERVAL ? MICROSECOND ORDER BY finished_at LIMIT ?`,
			replies},
		{`DELETE FROM foundry_slack_link_tokens WHERE expires_at < NOW(6) - INTERVAL ? MICROSECOND ORDER BY expires_at LIMIT ?`, model.SlackSecretsKept},
		{`DELETE FROM foundry_slack_install_states WHERE expires_at < NOW(6) - INTERVAL ? MICROSECOND ORDER BY expires_at LIMIT ?`,
			model.SlackSecretsKept},
	} {
		res, err := s.db.ExecContext(ctx, p.q, p.kept.Microseconds(), limit)
		if err != nil {
			return deleted, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return deleted, err
		}
		deleted += n
	}
	return deleted, nil
}

// --- Scans ---

func scanFoundrySlackInstall(scanner rowScanner) (*model.SlackInstall, error) {
	var i model.SlackInstall
	var scopes string
	var revoked, uninstalled sql.NullTime
	if err := scanner.Scan(&i.ID, &i.WorkspaceID, &i.TeamID, &i.EnterpriseID, &i.TeamName, &i.AppID, &i.BotUserID, &scopes, &i.InstalledByUserID,
		&i.InstalledBySlackUserID, &i.CreatedAt, &i.UpdatedAt, &revoked, &uninstalled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	// L1 installs act per person; bot mode (L2) is a column of its own.
	i.Scopes, i.Mode = strings.Split(scopes, ","), model.SlackPerPerson{}
	if revoked.Valid {
		i.RevokedAt = &revoked.Time
	}
	if uninstalled.Valid {
		i.UninstalledAt = &uninstalled.Time
	}
	return &i, nil
}

func scanFoundrySlackLink(scanner rowScanner) (*model.SlackLink, error) {
	var l model.SlackLink
	if err := scanner.Scan(&l.InstallID, &l.SlackUserID, &l.UserID, &l.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &l, nil
}

func scanFoundrySlackThread(scanner rowScanner) (*model.SlackThread, error) {
	var t model.SlackThread
	if err := scanner.Scan(&t.ID, &t.InstallID, &t.ChannelID, &t.ThreadTS, &t.SlackUserID, &t.AgentThreadID, &t.CreatedAt, &t.LastAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &t, nil
}

func scanFoundrySlackIntake(scanner rowScanner) (*model.SlackIntake, error) {
	var in model.SlackIntake
	var kind, status string
	var payload, input, leaseOwner, waits, errText sql.NullString
	var leaseUntil, finished sql.NullTime
	if err := scanner.Scan(&in.ID, &in.InstallID, &in.DedupeKey, &kind, &in.Conversation, &in.SlackUserID, &payload, &input, &status, &in.Attempts,
		&in.NextAttemptAt, &leaseOwner, &leaseUntil, &waits, &errText, &in.CreatedAt, &finished); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var err error
	if in.Kind, err = model.ParseSlackIntakeKind(kind); err != nil {
		return nil, err
	}
	if in.Status, err = model.ParseSlackIntakeStatus(status); err != nil {
		return nil, err
	}
	if payload.Valid {
		in.Payload = json.RawMessage(payload.String)
	}
	for _, f := range []struct {
		src sql.NullString
		dst **string
	}{{input, &in.Input}, {leaseOwner, &in.LeaseOwner}, {waits, &in.WaitsOnTurn}, {errText, &in.Error}} {
		if f.src.Valid {
			*f.dst = &f.src.String
		}
	}
	if leaseUntil.Valid {
		in.LeaseUntil = &leaseUntil.Time
	}
	if finished.Valid {
		in.FinishedAt = &finished.Time
	}
	return &in, nil
}

func scanFoundrySlackReply(scanner rowScanner) (*model.SlackReply, error) {
	var r model.SlackReply
	var status string
	var messageTS, posted, leaseOwner sql.NullString
	var leaseUntil, finished sql.NullTime
	if err := scanner.Scan(&r.TurnID, &r.InstallID, &r.ChannelID, &r.ThreadTS, &r.SlackUserID, &messageTS, &r.AfterSeq, &posted, &status, &leaseOwner,
		&leaseUntil, &r.NextPollAt, &r.NextUpdateAt, &r.CreatedAt, &finished); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var err error
	if r.Status, err = model.ParseSlackReplyStatus(status); err != nil {
		return nil, err
	}
	if posted.Valid {
		if err := json.Unmarshal([]byte(posted.String), &r.Posted); err != nil {
			return nil, fmt.Errorf("read Slack reply %s's posts: %w", r.TurnID, err)
		}
	}
	if messageTS.Valid {
		r.MessageTS = &messageTS.String
	}
	if leaseOwner.Valid {
		r.LeaseOwner = &leaseOwner.String
	}
	if leaseUntil.Valid {
		r.LeaseUntil = &leaseUntil.Time
	}
	if finished.Valid {
		r.FinishedAt = &finished.Time
	}
	return &r, nil
}
