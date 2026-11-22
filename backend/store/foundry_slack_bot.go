package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Helix Foundry for Slack's bot mode (L2,
// migrations/20261215000000_foundry_agent_principals.sql): an install whose
// bot acts as a service credential of its workspace (bot_credential_id),
// the Founder threads that credential owns (principal_kind
// 'service_credential'), and who has spoken to the bot in each of its Slack
// threads (foundry_slack_thread_people).

// foundrySlackRetired is the last_at a retired bot thread's mapping takes:
// long idle, so the purge's idle read (foundry_slack_threads_last_idx)
// finds it first.
const foundrySlackRetired = "2000-01-01 00:00:00"

// SetFoundrySlackMode sets whom the workspace's live install installID acts
// as, and returns the mode it had (ErrNotFound when it isn't the
// workspace's live install). Setting the mode it has changes nothing.
//   - model.SlackSharedBot from per_person stores the credential; from
//     another credential it is ErrConflict: per_person comes first.
//   - model.SlackPerPerson from a bot clears it, and retires the bot's
//     threads in the same transaction: its live turns are cancelled
//     (charging what they reserved), its Founder threads deleted, and their
//     Slack mappings left due for PurgeFoundrySlackThreads, which purges
//     their Slack-born text. The caller revokes the credential it returns.
func (s *mysqlDB) SetFoundrySlackMode(ctx context.Context, workspaceID, installID string, mode model.SlackIdentityMode) (model.SlackIdentityMode, error) {
	switch m := mode.(type) {
	case model.SlackPerPerson:
	case model.SlackSharedBot:
		if m.CredentialID == "" || len(m.CredentialID) > 26 {
			return nil, fmt.Errorf("a Slack bot's credential id is 1 to 26 bytes")
		}
	default:
		return nil, fmt.Errorf("unknown Slack identity mode %T", mode)
	}
	var was model.SlackIdentityMode
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		// The workspace's lock first, as a send takes it before its thread's.
		if err := tx.workspaces.Lock(ctx, tx.db, workspaceID); err != nil {
			return err
		}
		install, err := scanFoundrySlackInstall(tx.db.QueryRowContext(ctx, "SELECT "+foundrySlackInstallColumns+`
FROM foundry_slack_installs WHERE id=? AND workspace_id=? AND revoked_at IS NULL FOR UPDATE`, installID, workspaceID))
		if err != nil {
			return err
		}
		was = install.Mode
		if was == mode {
			return nil
		}
		bot, isBot := was.(model.SlackSharedBot)
		if next, ok := mode.(model.SlackSharedBot); ok {
			if isBot {
				return ErrConflict
			}
			_, err := tx.db.ExecContext(ctx, `UPDATE foundry_slack_installs SET bot_credential_id=? WHERE id=?`, next.CredentialID, installID)
			return err
		}
		for _, stmt := range []struct {
			q    string
			args []any
		}{
			{`UPDATE foundry_slack_installs SET bot_credential_id=NULL WHERE id=?`, []any{installID}},
			{`UPDATE foundry_agent_threads SET deleted_at=NOW(6)
WHERE workspace_id=? AND user_id=? AND principal_kind='service_credential' AND deleted_at IS NULL`, []any{workspaceID, bot.CredentialID}},
			{`UPDATE foundry_agent_turns
SET status='cancelled', error='bot mode was turned off', lease_owner=NULL, lease_until=NULL, usd_micros=usd_micros+reserved_micros,
    reserved_micros=0, finished_at=NOW(6)
WHERE workspace_id=? AND user_id=? AND principal_kind='service_credential' AND status IN ('queued', 'running')`,
				[]any{workspaceID, bot.CredentialID}},
			{`UPDATE foundry_slack_threads SET last_at=? WHERE install_id=? AND slack_user_id=''`, []any{foundrySlackRetired, installID}},
		} {
			if _, err := tx.db.ExecContext(ctx, stmt.q, stmt.args...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return was, nil
}

// IsFoundrySlackBot reports whether credentialID is the bot of the
// workspace's live install: the one non-person principal Founder serves.
func (s *mysqlDB) IsFoundrySlackBot(ctx context.Context, workspaceID, credentialID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_slack_installs
WHERE bot_credential_id=? AND workspace_id=? AND revoked_at IS NULL`, credentialID, workspaceID).Scan(&n)
	return n > 0, err
}

// IsFoundrySlackBotCredential reports whether any install, live or revoked,
// names credentialID as its bot: hosted's credential pages lock such a key,
// whose sources are chosen in Foundry, until it is revoked.
func (s *mysqlDB) IsFoundrySlackBotCredential(ctx context.Context, credentialID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_slack_installs WHERE bot_credential_id=?`, credentialID).Scan(&n)
	return n > 0, err
}

// AddFoundrySlackThreadPerson records that slackUserID spoke to the bot in
// the bot-mode Slack thread threadID (a mapping with no Slack user); again
// changes nothing. ErrNotFound when threadID is no such mapping.
func (s *mysqlDB) AddFoundrySlackThreadPerson(ctx context.Context, threadID, slackUserID string) error {
	if !model.ValidSlackID(slackUserID) {
		return fmt.Errorf("a Slack thread's person is a Slack user id")
	}
	res, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_slack_thread_people (thread_id, slack_user_id)
SELECT id, ? FROM foundry_slack_threads WHERE id=? AND slack_user_id=''`, slackUserID, threadID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	var mapped string
	err = s.db.QueryRowContext(ctx, `SELECT id FROM foundry_slack_threads WHERE id=? AND slack_user_id=''`, threadID).Scan(&mapped)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// IsFoundrySlackThreadPerson reports whether slackUserID has spoken to the
// bot in the bot-mode Slack thread threadID: only their unmentioned replies
// reach it.
func (s *mysqlDB) IsFoundrySlackThreadPerson(ctx context.Context, threadID, slackUserID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_slack_thread_people WHERE thread_id=? AND slack_user_id=?`,
		threadID, slackUserID).Scan(&n)
	return n > 0, err
}

// CountFoundrySlackUserMessages counts the events intake recorded from
// slackUserID in installID within window before the row intakeID came, that
// row not counted: what one Slack user sent a bot-mode install's bot in the
// hour its per-user cap reads (foundry_slack_intake_user_idx). Read from
// the row's own time, a retry of it counts the same; with no such row it is
// 0.
func (s *mysqlDB) CountFoundrySlackUserMessages(ctx context.Context, installID, slackUserID, intakeID string, window time.Duration) (int, error) {
	if window <= 0 {
		return 0, fmt.Errorf("a Slack user's messages are counted over a positive window")
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_slack_intake me
JOIN foundry_slack_intake i ON i.install_id=me.install_id AND i.slack_user_id=? AND i.kind=? AND i.id<>me.id
  AND i.created_at <= me.created_at AND i.created_at > me.created_at - INTERVAL ? MICROSECOND
WHERE me.id=? AND me.install_id=?`, slackUserID, string(model.SlackIntakeEvent), window.Microseconds(), intakeID, installID).Scan(&n)
	return n, err
}
