package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"time"
)

// A workspace's model provider (foundry_ai_configs, foundry_ai_activations).
// Every write that decides which config is the setting, or holds a key,
// takes the workspace's row lock first, so saving, a probe's end and going
// back to the default are serialized per workspace, and refused once the
// workspace is being deleted. A sealed key exists only
// on the active config and on at most one pending config: a config's key is
// nulled as it fails, is superseded, or stops being the setting.

const foundryAIConfigColumns = `c.id, c.workspace_id, c.kind, c.settings, c.secret_ciphertext, c.secret_key_id, c.secret_hint,
       c.probe_state, c.probe_attempts, c.probe_owner, c.probe_until, c.probe_failure, c.probe_detail, c.probe_report,
       c.created_by, c.created_at, c.probed_at`

// dropFoundryAISecret is the SET clause that forgets a config's key.
const dropFoundryAISecret = `secret_ciphertext=NULL, secret_key_id=NULL`

func scanFoundryAIConfig(row rowScanner) (*model.FoundryAIConfig, error) {
	var c model.FoundryAIConfig
	var settings, ciphertext, report []byte
	var keyID sql.NullString
	err := row.Scan(&c.ID, &c.WorkspaceID, &c.Kind, &settings, &ciphertext, &keyID, &c.SecretHint,
		&c.ProbeState, &c.ProbeAttempts, &c.ProbeOwner, &c.ProbeUntil, &c.ProbeFailure, &c.ProbeDetail, &report,
		&c.CreatedBy, &c.CreatedAt, &c.ProbedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.Settings = settings
	if len(report) > 0 {
		c.ProbeReport = report
	}
	if keyID.Valid {
		c.Secret = &model.FoundryAISecret{Ciphertext: ciphertext, KeyID: keyID.String}
	}
	return &c, nil
}

// CreateFoundryAIConfig queues c for its probe, as the workspace's pending
// config: an older queued or running config is superseded and its key
// dropped. ErrLimitExceeded once the workspace saved perHour configs in the
// last hour; ErrNotFound for a workspace that is gone.
func (s *mysqlDB) CreateFoundryAIConfig(ctx context.Context, c *model.FoundryAIConfig, perHour int) error {
	switch {
	case perHour < 1:
		return fmt.Errorf("foundry ai configs per hour must be positive")
	case c.ID == "" || c.WorkspaceID == "" || c.Kind == "" || c.CreatedBy == "" || len(c.Settings) == 0:
		return fmt.Errorf("invalid foundry ai config")
	}
	var ciphertext []byte
	var keyID any
	if c.Secret != nil {
		ciphertext, keyID = c.Secret.Ciphertext, c.Secret.KeyID
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.lockFoundryAIWorkspace(ctx, c.WorkspaceID); err != nil {
			return err
		}
		var lastHour int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_ai_configs
WHERE workspace_id=? AND created_at >= NOW(6) - INTERVAL 1 HOUR`, c.WorkspaceID).Scan(&lastHour); err != nil {
			return err
		}
		if lastHour >= perHour {
			return ErrLimitExceeded
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_ai_configs SET probe_state='superseded', `+dropFoundryAISecret+`
WHERE workspace_id=? AND probe_state IN ('queued', 'running')`, c.WorkspaceID); err != nil {
			return err
		}
		_, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_ai_configs
  (id, workspace_id, kind, settings, secret_ciphertext, secret_key_id, secret_hint, probe_state, created_by)
VALUES (?, ?, ?, CAST(? AS JSON), ?, ?, ?, 'queued', ?)`,
			c.ID, c.WorkspaceID, c.Kind, string(c.Settings), ciphertext, keyID, c.SecretHint, c.CreatedBy)
		return err
	})
}

// GetActiveFoundryAIConfig returns the config the workspace's newest
// activation names; ErrNotFound means the deployment's default, whether
// chosen or never changed.
func (s *mysqlDB) GetActiveFoundryAIConfig(ctx context.Context, workspaceID string) (*model.FoundryAIConfig, error) {
	return scanFoundryAIConfig(s.db.QueryRowContext(ctx, `SELECT `+foundryAIConfigColumns+`
FROM (SELECT config_id FROM foundry_ai_activations WHERE workspace_id=? ORDER BY created_at DESC, id DESC LIMIT 1) a
JOIN foundry_ai_configs c ON c.id = a.config_id AND c.workspace_id = ?`, workspaceID, workspaceID))
}

// GetFoundryAIView reads what the workspace's AI settings show.
func (s *mysqlDB) GetFoundryAIView(ctx context.Context, workspaceID string) (model.FoundryAIView, error) {
	var view model.FoundryAIView
	active, err := s.GetActiveFoundryAIConfig(ctx, workspaceID)
	switch {
	case errors.Is(err, ErrNotFound):
	case err != nil:
		return view, err
	default:
		view.Active = active
	}
	view.Pending, err = scanFoundryAIConfig(s.db.QueryRowContext(ctx, `SELECT `+foundryAIConfigColumns+`
FROM foundry_ai_configs c WHERE c.workspace_id=? AND c.probe_state IN ('queued', 'running')
ORDER BY c.created_at DESC, c.id DESC LIMIT 1`, workspaceID))
	if errors.Is(err, ErrNotFound) {
		err = nil
	}
	if err != nil {
		return view, err
	}
	// The newest config failed since the setting last changed: it is the
	// last thing the owner tried, and the setting they kept.
	view.LastFailed, err = scanFoundryAIConfig(s.db.QueryRowContext(ctx, `SELECT `+foundryAIConfigColumns+`
FROM (SELECT id FROM foundry_ai_configs WHERE workspace_id=? ORDER BY created_at DESC, id DESC LIMIT 1) n
JOIN foundry_ai_configs c ON c.id = n.id
WHERE c.probe_state='failed' AND NOT EXISTS (SELECT 1 FROM foundry_ai_activations a
  WHERE a.workspace_id=c.workspace_id AND a.created_at >= c.created_at)`, workspaceID))
	if errors.Is(err, ErrNotFound) {
		err = nil
	}
	return view, err
}

// ClaimFoundryAIProbe leases the oldest config waiting for its probe to
// owner: a queued one, or a running one whose lease ran out with attempts
// left. A running one whose lease ran out on its last attempt fails first,
// unavailable. Nil when none waits.
func (s *mysqlDB) ClaimFoundryAIProbe(ctx context.Context, owner string, lease time.Duration, maxAttempts int) (*model.FoundryAIConfig, error) {
	if owner == "" || lease <= 0 || maxAttempts < 1 {
		return nil, fmt.Errorf("invalid foundry ai probe claim")
	}
	var claimed *model.FoundryAIConfig
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_ai_configs SET probe_state='failed', probe_failure='unavailable',
  probe_detail='none', probe_owner=NULL, probe_until=NULL, probed_at=NOW(6), `+dropFoundryAISecret+`
WHERE probe_state='running' AND probe_until < NOW(6) AND probe_attempts >= ?`, maxAttempts); err != nil {
			return err
		}
		var configID string
		err := tx.db.QueryRowContext(ctx, `SELECT id FROM foundry_ai_configs
WHERE probe_state='queued' OR (probe_state='running' AND probe_until < NOW(6) AND probe_attempts < ?)
ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, maxAttempts).Scan(&configID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_ai_configs SET probe_state='running', probe_owner=?,
  probe_until=NOW(6) + INTERVAL ? MICROSECOND, probe_attempts=probe_attempts+1 WHERE id=?`,
			owner, lease.Microseconds(), configID); err != nil {
			return err
		}
		claimed, err = scanFoundryAIConfig(tx.db.QueryRowContext(ctx, `SELECT `+foundryAIConfigColumns+`
FROM foundry_ai_configs c WHERE c.id=?`, configID))
		return err
	})
	return claimed, err
}

// ReleaseFoundryAIProbe gives back owner's probe of a config that could not
// run, the attempt counted: it waits again, or fails unavailable once it has
// had maxAttempts. ErrNotFound when owner no longer holds it.
func (s *mysqlDB) ReleaseFoundryAIProbe(ctx context.Context, configID, owner string, maxAttempts int) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_ai_configs SET
  probe_failure=IF(probe_attempts >= ?, 'unavailable', NULL),
  probe_detail=IF(probe_attempts >= ?, 'none', NULL),
  probed_at=IF(probe_attempts >= ?, NOW(6), probed_at),
  secret_ciphertext=IF(probe_attempts >= ?, NULL, secret_ciphertext),
  secret_key_id=IF(probe_attempts >= ?, NULL, secret_key_id),
  probe_state=IF(probe_attempts >= ?, 'failed', 'queued'),
  probe_owner=NULL, probe_until=NULL
WHERE id=? AND probe_state='running' AND probe_owner=?`,
		maxAttempts, maxAttempts, maxAttempts, maxAttempts, maxAttempts, maxAttempts, configID, owner)
}

// ReturnFoundryAIProbe gives back owner's probe of a config uncounted: the
// worker stopped before the probe could end, which is no fault of the
// setting or the agent. It waits again with the attempts it had before the
// claim. ErrNotFound when owner no longer holds it.
func (s *mysqlDB) ReturnFoundryAIProbe(ctx context.Context, configID, owner string) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_ai_configs SET probe_state='queued',
  probe_attempts=GREATEST(probe_attempts - 1, 0), probe_owner=NULL, probe_until=NULL
WHERE id=? AND probe_state='running' AND probe_owner=?`, configID, owner)
}

// FinishFoundryAIProbe records how owner's probe of a config ended, and
// returns the state it left the config in. A failed probe fails it and drops
// its key. A passed one makes it the setting only while it is the
// workspace's newest config and no activation came after it was saved; the
// setting before it then drops its key. Otherwise it is superseded and drops
// its own. ErrNotFound when owner no longer holds the probe.
func (s *mysqlDB) FinishFoundryAIProbe(ctx context.Context, configID, owner string, outcome model.FoundryAIProbeOutcome) (model.FoundryAIProbeState, error) {
	var report any
	if len(outcome.Report) > 0 {
		report = string(outcome.Report)
	}
	var state model.FoundryAIProbeState
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var workspaceID string
		if err := tx.db.QueryRowContext(ctx, `SELECT workspace_id FROM foundry_ai_configs
WHERE id=? AND probe_state='running' AND probe_owner=?`, configID, owner).Scan(&workspaceID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if err := tx.lockFoundryAIWorkspace(ctx, workspaceID); err != nil {
			return err
		}
		var createdAt time.Time
		if err := tx.db.QueryRowContext(ctx, `SELECT created_at FROM foundry_ai_configs
WHERE id=? AND probe_state='running' AND probe_owner=? FOR UPDATE`, configID, owner).Scan(&createdAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		const ended = `probe_report=CAST(? AS JSON), probed_at=NOW(6), probe_owner=NULL, probe_until=NULL`
		if f := outcome.Failure; f != nil {
			state = model.FoundryAIProbeFailed
			_, err := tx.db.ExecContext(ctx, `UPDATE foundry_ai_configs SET probe_state='failed', probe_failure=?, probe_detail=?, `+
				ended+`, `+dropFoundryAISecret+` WHERE id=?`, f.Failure, f.Detail, report, configID)
			return err
		}
		var newest string
		if err := tx.db.QueryRowContext(ctx, `SELECT id FROM foundry_ai_configs WHERE workspace_id=?
ORDER BY created_at DESC, id DESC LIMIT 1`, workspaceID).Scan(&newest); err != nil {
			return err
		}
		var later int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_ai_activations
WHERE workspace_id=? AND created_at >= ?`, workspaceID, createdAt).Scan(&later); err != nil {
			return err
		}
		if newest != configID || later > 0 {
			state = model.FoundryAIProbeSuperseded
			_, err := tx.db.ExecContext(ctx, `UPDATE foundry_ai_configs SET probe_state='superseded', `+
				ended+`, `+dropFoundryAISecret+` WHERE id=?`, report, configID)
			return err
		}
		state = model.FoundryAIProbePassed
		previous, err := tx.GetActiveFoundryAIConfig(ctx, workspaceID)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return err
		default:
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_ai_configs SET `+dropFoundryAISecret+` WHERE id=?`, previous.ID); err != nil {
				return err
			}
		}
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_ai_activations (id, workspace_id, config_id, user_id)
SELECT ?, workspace_id, id, created_by FROM foundry_ai_configs WHERE id=?`, id.New(), configID); err != nil {
			return err
		}
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_ai_configs SET probe_state='passed', `+ended+` WHERE id=?`, report, configID)
		return err
	})
	return state, err
}

// ActivateFoundryAIDefault makes the deployment's default the workspace's
// setting, at once: no config is then active or pending, so every one drops
// its key, and a pending one is superseded. ErrNotFound for a workspace
// that is gone.
func (s *mysqlDB) ActivateFoundryAIDefault(ctx context.Context, workspaceID, userID string) error {
	if workspaceID == "" || userID == "" {
		return fmt.Errorf("invalid foundry ai default activation")
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.lockFoundryAIWorkspace(ctx, workspaceID); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_ai_activations (id, workspace_id, config_id, user_id)
VALUES (?, ?, NULL, ?)`, id.New(), workspaceID, userID); err != nil {
			return err
		}
		return tx.dropFoundryAIConfigs(ctx, workspaceID)
	})
}

// lockFoundryAIWorkspace takes the workspace's row lock for a write of its
// AI setting. ErrNotFound for a workspace that is gone or being deleted: its
// deletion dropped every key of its configs (CancelWorkspaceFoundryWork,
// under the same lock), and no save, probe's end or default may bring one
// back.
func (s *mysqlDB) lockFoundryAIWorkspace(ctx context.Context, workspaceID string) error {
	var deleting bool
	err := s.db.QueryRowContext(ctx, `SELECT status IN ('deleting', 'deleted') FROM `+s.workspaces.Table()+`
WHERE id=? AND deleted_at IS NULL FOR UPDATE`, workspaceID).Scan(&deleting)
	if errors.Is(err, sql.ErrNoRows) || err == nil && deleting {
		return ErrNotFound
	}
	return err
}

// dropFoundryAIConfigs drops every key of the workspace's configs and
// supersedes a pending one.
func (s *mysqlDB) dropFoundryAIConfigs(ctx context.Context, workspaceID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_ai_configs SET `+dropFoundryAISecret+`,
  probe_state=IF(probe_state IN ('queued', 'running'), 'superseded', probe_state)
WHERE workspace_id=? AND (secret_key_id IS NOT NULL OR probe_state IN ('queued', 'running'))`, workspaceID)
	return err
}
