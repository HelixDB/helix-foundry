package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Automations (migrations/20261202000000_foundry_automations.sql): a person's
// automations, which only their owner edits, approves, resumes and runs, and
// owners and admins pause, delete and cancel too; the foundry-automation-
// worker's fires (a schedule due, work an event trigger found), and its
// leased walk of each run a step at a time.
//
// Every write to an automation, and every fire, locks its row first, so one
// automation's runs are queued one at a time: a trigger key is unique per
// automation (no run twice for one fire) and a live run is unique per
// automation (no two at once).

// ErrThreadBusy means an automation's Assistant step can't queue its turn
// yet: its run's thread has a live turn, one its owner's click on a card the
// run left waiting started. The step waits for it.
var ErrThreadBusy = errors.New("store: the thread has a live turn")

const foundryAutomationColumns = `id, workspace_id, owner_user_id, name, description, spec, revision, spec_revision, approved_spec_revision,
       approved_at, trigger_kind, trigger_target, status, paused_reason, paused_by_user_id, paused_at, failures, backoff_until,
       next_fire_at, cursor_at, cursor_id, checked_at, notice_id, client_token, created_at, updated_at`

const foundryAutomationRunColumns = `id, automation_id, workspace_id, owner_user_id, spec_revision, trigger_key, trigger_event, status, error,
       thread_id, lease_owner, lease_until, attempts, next_attempt_at, usd_micros, created_at, started_at, finished_at`

const foundryAutomationNodeColumns = `run_id, node_id, status, output, error, turn_id, notification_id, usd_micros, started_at, finished_at`

// foundryAutomationEventKinds are the triggers that find ended work.
const foundryAutomationEventKinds = `('sync_finished', 'load_ready', 'document_arrived', 'generation_finished')`

// foundryAutomationLookBack bounds how long before an event trigger's cursor
// the work it looks for was created: no sync, load or generation runs longer,
// and an upload completes within minutes of its document's creation.
const foundryAutomationLookBack = 3 * 24 * time.Hour

// dueFoundryAutomationRun is a run a claim may take: queued and due, or
// running under a lease that ran out.
const dueFoundryAutomationRun = `((status='queued' AND next_attempt_at <= NOW(6)) OR (status='running' AND lease_until < NOW(6)))`

// ownedFoundryAutomationRun is the run while workerID holds its lease.
const ownedFoundryAutomationRun = ` WHERE id=? AND status='running' AND lease_owner=?`

// CreateFoundryAutomation stores a, waiting for its owner's approval, under
// its workspace's lock: past max automations is a FoundryAutomationLimit
// refusal, and the owner's client token used before is ErrConflict.
func (s *mysqlDB) CreateFoundryAutomation(ctx context.Context, a *model.FoundryAutomation, max int) error {
	if a.ID == "" || a.WorkspaceID == "" || a.OwnerUserID == "" || len(a.Spec) == 0 || a.TriggerKind == "" {
		return fmt.Errorf("foundry automation is incomplete")
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.workspaces.Lock(ctx, tx.db, a.WorkspaceID); err != nil {
			return err
		}
		var count int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_automations WHERE workspace_id=?`, a.WorkspaceID).Scan(&count); err != nil {
			return err
		}
		if count >= max {
			return model.FoundryAutomationRefusal{Reason: model.FoundryAutomationLimit,
				Detail: fmt.Sprintf("the workspace has %d workflows, as many as it may; delete one first", count)}
		}
		_, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_automations
  (id, workspace_id, owner_user_id, name, description, spec, trigger_kind, trigger_target, status, client_token)
VALUES (?, ?, ?, ?, ?, CAST(? AS JSON), ?, ?, 'needs_approval', ?)`,
			a.ID, a.WorkspaceID, a.OwnerUserID, a.Name, a.Description, string(a.Spec), a.TriggerKind, a.TriggerTarget, a.ClientToken)
		if isDuplicateKey(err) {
			return ErrConflict
		}
		return err
	})
}

func (s *mysqlDB) GetFoundryAutomation(ctx context.Context, workspaceID, id string) (*model.FoundryAutomation, error) {
	a, err := scanFoundryAutomation(s.db.QueryRowContext(ctx, "SELECT "+foundryAutomationColumns+`
FROM foundry_automations WHERE id=? AND workspace_id=?`, id, workspaceID))
	if err != nil {
		return nil, err
	}
	return a, s.withLastRun(ctx, a)
}

func (s *mysqlDB) GetFoundryAutomationByClientToken(ctx context.Context, workspaceID, ownerUserID, clientToken string) (*model.FoundryAutomation, error) {
	a, err := scanFoundryAutomation(s.db.QueryRowContext(ctx, "SELECT "+foundryAutomationColumns+`
FROM foundry_automations WHERE workspace_id=? AND owner_user_id=? AND client_token=?`, workspaceID, ownerUserID, clientToken))
	if err != nil {
		return nil, err
	}
	return a, s.withLastRun(ctx, a)
}

// ListFoundryAutomations returns the workspace's automations, most recently
// changed first, each with its newest run and when it last failed: all of
// them, as a workspace keeps at most foundry.automations.max_per_workspace.
// Their runs are read in three statements, whatever their number: the
// newest run's time and the newest failure's, each one index dive per
// automation (the history and outcome indexes), then those runs.
func (s *mysqlDB) ListFoundryAutomations(ctx context.Context, workspaceID string) ([]*model.FoundryAutomation, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryAutomationColumns+`
FROM foundry_automations WHERE workspace_id=? ORDER BY updated_at DESC, id DESC`, workspaceID)
	if err != nil {
		return nil, err
	}
	out, err := scanAll(rows, scanFoundryAutomation)
	if err != nil || len(out) == 0 {
		return out, err
	}
	byID := make(map[string]*model.FoundryAutomation, len(out))
	ids := make([]any, len(out))
	for i, a := range out {
		byID[a.ID], ids[i] = a, a.ID
	}
	in := idPlaceholders(len(ids))
	newest := func(failed bool) (map[string]time.Time, error) {
		where := ""
		if failed {
			where = " AND status='failed'"
		}
		rows, err := s.db.QueryContext(ctx, `SELECT automation_id, MAX(created_at) FROM foundry_automation_runs
WHERE automation_id IN (`+in+`)`+where+` GROUP BY automation_id`, ids...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		at := map[string]time.Time{}
		for rows.Next() {
			var id string
			var t time.Time
			if err := rows.Scan(&id, &t); err != nil {
				return nil, err
			}
			at[id] = t
		}
		return at, rows.Err()
	}
	failed, err := newest(true)
	if err != nil {
		return nil, err
	}
	for id, t := range failed {
		byID[id].LastFailedAt = &t
	}
	last, err := newest(false)
	if err != nil || len(last) == 0 {
		return out, err
	}
	conds, args := make([]string, 0, len(last)), make([]any, 0, 2*len(last))
	for id, t := range last {
		conds, args = append(conds, "(automation_id=? AND created_at=?)"), append(args, id, t)
	}
	runs, err := s.db.QueryContext(ctx, "SELECT "+foundryAutomationRunColumns+`
FROM foundry_automation_runs WHERE `+strings.Join(conds, " OR "), args...)
	if err != nil {
		return nil, err
	}
	found, err := scanAll(runs, func(r rowScanner) (*model.FoundryAutomationRun, error) { return scanFoundryAutomationRun(r, false) })
	if err != nil {
		return nil, err
	}
	// Two runs made in the same microsecond: the newest is the greater id,
	// as withLastRun orders them.
	for _, r := range found {
		a := byID[r.AutomationID]
		if a.LastRun == nil || r.ID > a.LastRun.ID {
			a.LastRun = r
		}
	}
	return out, nil
}

// FoundryAutomationRunStats counts the workspace's runs created since
// since: by automation and status, with what they spent; by quarter hour
// and status; and the actions their Assistant steps prepared that still
// wait. Each statement reads the runs of that time by the day index, which
// the workspace's runs a day (foundry.automations.runs_per_day) bound, each
// joined to its automation by primary key, so runs a deletion has yet to
// reach (DeleteFoundryAutomation takes them a batch at a time after the
// automation) count nowhere; and a waiting action by its turn
// (foundry_agent_actions' payload key).
func (s *mysqlDB) FoundryAutomationRunStats(ctx context.Context, workspaceID string, since time.Time) (*model.FoundryAutomationRunStats, error) {
	out := &model.FoundryAutomationRunStats{ByAutomation: map[string]model.FoundryAutomationRunCounts{}}
	since = since.UTC()
	rows, err := s.db.QueryContext(ctx, `SELECT r.automation_id, r.status, COUNT(*), SUM(r.usd_micros) FROM foundry_automation_runs r
JOIN foundry_automations a ON a.id=r.automation_id
WHERE r.workspace_id=? AND r.created_at>=? GROUP BY r.automation_id, r.status`, workspaceID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, status string
		var n int
		var usd int64
		if err := rows.Scan(&id, &status, &n, &usd); err != nil {
			return nil, err
		}
		c := out.ByAutomation[id]
		c.Runs, c.USDMicros = c.Runs+n, c.USDMicros+usd
		switch model.FoundryAutomationRunStatus(status) {
		case model.FoundryAutomationRunSucceeded:
			c.Succeeded += n
		case model.FoundryAutomationRunFailed:
			c.Failed += n
		}
		out.ByAutomation[id] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slots, err := s.db.QueryContext(ctx, `SELECT DATE_FORMAT(r.created_at, '%Y-%m-%d %H') AS hour, FLOOR(MINUTE(r.created_at) / 15) AS quarter,
       r.status, COUNT(*)
FROM foundry_automation_runs r JOIN foundry_automations a ON a.id=r.automation_id
WHERE r.workspace_id=? AND r.created_at>=? GROUP BY hour, quarter, r.status`, workspaceID, since)
	if err != nil {
		return nil, err
	}
	defer slots.Close()
	for slots.Next() {
		var hour, status string
		var quarter, n int
		if err := slots.Scan(&hour, &quarter, &status, &n); err != nil {
			return nil, err
		}
		at, err := time.ParseInLocation("2006-01-02 15", hour, time.UTC)
		if err != nil {
			return nil, fmt.Errorf("read a run's hour %q: %w", hour, err)
		}
		out.Slots = append(out.Slots, model.FoundryAutomationRunSlot{At: at.Add(time.Duration(quarter) * 15 * time.Minute),
			Status: model.FoundryAutomationRunStatus(status), Runs: n})
	}
	if err := slots.Err(); err != nil {
		return nil, err
	}
	waiting, err := s.db.QueryContext(ctx, `SELECT r.automation_id, COUNT(*) FROM foundry_automation_runs r
JOIN foundry_automations a ON a.id=r.automation_id
JOIN foundry_automation_run_nodes n ON n.run_id=r.id
JOIN foundry_agent_actions x ON x.turn_id=n.turn_id AND x.workspace_id=r.workspace_id
WHERE r.workspace_id=? AND r.created_at>=? AND n.turn_id IS NOT NULL AND x.status='prepared'
GROUP BY r.automation_id`, workspaceID, since)
	if err != nil {
		return nil, err
	}
	defer waiting.Close()
	for waiting.Next() {
		var id string
		var n int
		if err := waiting.Scan(&id, &n); err != nil {
			return nil, err
		}
		c := out.ByAutomation[id]
		c.Waiting = n
		out.ByAutomation[id] = c
	}
	return out, waiting.Err()
}

// withLastRun reads a's newest run, without its spec.
func (s *mysqlDB) withLastRun(ctx context.Context, a *model.FoundryAutomation) error {
	r, err := scanFoundryAutomationRun(s.db.QueryRowContext(ctx, "SELECT "+foundryAutomationRunColumns+`
FROM foundry_automation_runs WHERE automation_id=? ORDER BY created_at DESC, id DESC LIMIT 1`, a.ID), false)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	a.LastRun = r
	return nil
}

// lockFoundryAutomation locks the workspace's automation: ErrNotFound when
// there is none.
func (s *mysqlDB) lockFoundryAutomation(ctx context.Context, workspaceID, id string) (*model.FoundryAutomation, error) {
	return scanFoundryAutomation(s.db.QueryRowContext(ctx, "SELECT "+foundryAutomationColumns+`
FROM foundry_automations WHERE id=? AND workspace_id=? FOR UPDATE`, id, workspaceID))
}

// UpdateFoundryAutomation edits the owner's automation at expected: a spec
// that changes (as JSON values, not bytes) waits for its owner's approval
// again, stopping it, unless it only lays the same steps out anew
// (LayoutOnly, which the caller decided against the spec at expected); a name
// or description alone changes nothing else, but for its workflows' names.
// Refusals are a FoundryAutomationRefusal: another's automation
// (FoundryAutomationNotOwner), another revision (FoundryAutomationChanged).
func (s *mysqlDB) UpdateFoundryAutomation(ctx context.Context, workspaceID, id, ownerUserID string, expected int64,
	u model.FoundryAutomationUpdate) (*model.FoundryAutomation, error) {
	if u.Name == nil && u.Description == nil && u.Spec == nil {
		return nil, fmt.Errorf("foundry automation update changes nothing")
	}
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		a, err := tx.lockFoundryAutomation(ctx, workspaceID, id)
		if err != nil {
			return err
		}
		if err := ownsFoundryAutomation(a, ownerUserID, expected); err != nil {
			return err
		}
		sets, args := []string{"revision=revision+1", "updated_at=NOW(6)"}, []any{}
		if u.Name != nil {
			sets, args = append(sets, "name=?"), append(args, *u.Name)
		}
		if u.Description != nil {
			sets, args = append(sets, "description=?"), append(args, *u.Description)
		}
		if u.Spec != nil {
			var same bool
			if err := tx.db.QueryRowContext(ctx, `SELECT spec = CAST(? AS JSON) FROM foundry_automations WHERE id=?`,
				string(u.Spec.JSON), id).Scan(&same); err != nil {
				return err
			}
			switch {
			case same:
			case u.Spec.LayoutOnly:
				// The same steps, laid out anew: what was approved runs as it was.
				sets, args = append(sets, "spec=CAST(? AS JSON)"), append(args, string(u.Spec.JSON))
			default:
				sets = append(sets, "spec=CAST(? AS JSON)", "spec_revision=spec_revision+1", "trigger_kind=?", "trigger_target=?",
					"status='needs_approval'", "paused_reason=NULL", "paused_by_user_id=NULL", "paused_at=NULL", "next_fire_at=NULL",
					"cursor_at=NULL", "cursor_id=''", "backoff_until=NULL", "failures=0")
				args = append(args, string(u.Spec.JSON), u.Spec.TriggerKind, u.Spec.TriggerTarget)
			}
		}
		if _, err = tx.db.ExecContext(ctx, "UPDATE foundry_automations SET "+strings.Join(sets, ", ")+" WHERE id=?", append(args, id)...); err != nil {
			return err
		}
		if u.Name == nil {
			return nil
		}
		// Its workflows are named after it.
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_workflows SET name=? WHERE automation_id=?`, model.FoundryAutomationWorkflowName(*u.Name), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetFoundryAutomation(ctx, workspaceID, id)
}

// ownsFoundryAutomation refuses another's automation, and a revision a has not.
func ownsFoundryAutomation(a *model.FoundryAutomation, ownerUserID string, expected int64) error {
	switch {
	case a.OwnerUserID != ownerUserID:
		return model.FoundryAutomationRefusal{Reason: model.FoundryAutomationNotOwner, Detail: "only its owner changes a workflow"}
	case a.Revision != expected:
		return model.FoundryAutomationRefusal{Reason: model.FoundryAutomationChanged,
			Detail: fmt.Sprintf("the workflow is at revision %d, not %d: read it again", a.Revision, expected)}
	}
	return nil
}

// activateFoundryAutomation is the SET of an automation becoming active,
// approved as it is (approved_at now, for a first parameter of true): no
// pause, no failures, its schedule's first time (the second parameter; nil
// for an event trigger, whose cursor starts now, so work that ended before
// fires nothing).
const activateFoundryAutomation = `status='active', paused_reason=NULL, paused_by_user_id=NULL, paused_at=NULL, failures=0, backoff_until=NULL,
    approved_spec_revision=spec_revision, approved_at=IF(?, NOW(6), approved_at), next_fire_at=?,
    cursor_at=IF(trigger_kind IN ` + foundryAutomationEventKinds + `, NOW(6), NULL), cursor_id='', checked_at=NULL, updated_at=NOW(6)`

// ApproveFoundryAutomation is its owner's click on the spec they read at
// expected: it becomes active, its schedule firing first at next (nil for
// any other trigger). Approving the approved spec again changes nothing.
// Refusals are those of UpdateFoundryAutomation.
func (s *mysqlDB) ApproveFoundryAutomation(ctx context.Context, workspaceID, id, ownerUserID string, expected int64, next *time.Time) (
	*model.FoundryAutomation, error) {
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		a, err := tx.lockFoundryAutomation(ctx, workspaceID, id)
		if err != nil {
			return err
		}
		if err := ownsFoundryAutomation(a, ownerUserID, expected); err != nil {
			return err
		}
		if a.Status != model.FoundryAutomationNeedsApproval {
			return nil
		}
		if (next != nil) != (a.TriggerKind == "schedule") {
			return fmt.Errorf("approve foundry automation %s: a schedule, and only a schedule, has its first time", id)
		}
		_, err = tx.db.ExecContext(ctx, "UPDATE foundry_automations SET "+activateFoundryAutomation+" WHERE id=?", true, utcOrNil(next), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetFoundryAutomation(ctx, workspaceID, id)
}

// PauseFoundryAutomation stops the active automation's runs, as by: its
// owner, or an owner or admin of the workspace (FoundryAutomationNotAllowed
// otherwise). A paused one is returned as it is; one that waits for approval
// isn't running (FoundryAutomationNotActive). A live run goes on.
func (s *mysqlDB) PauseFoundryAutomation(ctx context.Context, workspaceID, id string, by model.FoundryAutomationActor) (*model.FoundryAutomation, error) {
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		a, err := tx.lockFoundryAutomation(ctx, workspaceID, id)
		switch {
		case err != nil:
			return err
		case a.OwnerUserID != by.UserID && !by.Manager:
			return model.FoundryAutomationRefusal{Reason: model.FoundryAutomationNotAllowed, Detail: "only its owner, or an owner or admin, pauses a workflow"}
		case a.Status == model.FoundryAutomationPaused:
			return nil
		case a.Status != model.FoundryAutomationActive:
			return model.FoundryAutomationRefusal{Reason: model.FoundryAutomationNotActive, Detail: "the workflow waits for its owner's approval, so it isn't running"}
		}
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_automations
SET status='paused', paused_reason='person', paused_by_user_id=?, paused_at=NOW(6), next_fire_at=NULL, cursor_at=NULL, cursor_id='', updated_at=NOW(6)
WHERE id=?`, by.UserID, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetFoundryAutomation(ctx, workspaceID, id)
}

// ResumeFoundryAutomation makes the owner's paused automation active again,
// as ApproveFoundryAutomation does, from now: what ended while it was paused
// fires nothing. An active one is returned as it is; one waiting for
// approval is FoundryAutomationUnapproved.
func (s *mysqlDB) ResumeFoundryAutomation(ctx context.Context, workspaceID, id, ownerUserID string, next *time.Time) (*model.FoundryAutomation, error) {
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		a, err := tx.lockFoundryAutomation(ctx, workspaceID, id)
		switch {
		case err != nil:
			return err
		case a.OwnerUserID != ownerUserID:
			return model.FoundryAutomationRefusal{Reason: model.FoundryAutomationNotOwner, Detail: "only its owner resumes a workflow: it runs as them"}
		case a.Status == model.FoundryAutomationActive:
			return nil
		case a.Status == model.FoundryAutomationNeedsApproval:
			return model.FoundryAutomationRefusal{Reason: model.FoundryAutomationUnapproved, Detail: "the workflow's changes wait for your approval: approve them to run it"}
		case (next != nil) != (a.TriggerKind == "schedule"):
			return fmt.Errorf("resume foundry automation %s: a schedule, and only a schedule, has its first time", id)
		}
		_, err = tx.db.ExecContext(ctx, "UPDATE foundry_automations SET "+activateFoundryAutomation+" WHERE id=?", false, utcOrNil(next), id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.GetFoundryAutomation(ctx, workspaceID, id)
}

// DeleteFoundryAutomation deletes the automation, as by (its owner, or an
// owner or admin), ending its live run and the Assistant turn that run waits
// on in the same write, finishing the workflow runs that run started,
// unassessed; then its runs and their steps, a batch at a time, so no one
// transaction holds a long history. Runs a crash leaves behind ended, and
// PruneFoundryAutomationRuns deletes them in time. Its workflows stay, with
// their runs, as history.
func (s *mysqlDB) DeleteFoundryAutomation(ctx context.Context, workspaceID, id string, by model.FoundryAutomationActor) error {
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		a, err := tx.lockFoundryAutomation(ctx, workspaceID, id)
		switch {
		case err != nil:
			return err
		case a.OwnerUserID != by.UserID && !by.Manager:
			return model.FoundryAutomationRefusal{Reason: model.FoundryAutomationNotAllowed, Detail: "only its owner, or an owner or admin, deletes a workflow"}
		}
		if err := tx.cancelFoundryAutomationTurns(ctx, `SELECT id FROM foundry_automation_runs WHERE live_automation=?`, id, "the workflow was deleted"); err != nil {
			return err
		}
		// Its workflow runs first, while live_automation still names its live run.
		if err := tx.finishFoundryAutomationWorkflowRuns(ctx, `SELECT id FROM foundry_automation_runs WHERE live_automation=?`, id,
			"The workflow was deleted before its run finished.", false); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_automation_runs
SET status='cancelled', error='The automation was deleted.', lease_owner=NULL, lease_until=NULL, finished_at=NOW(6) WHERE live_automation=?`, id); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_automation_untracked WHERE automation_id=?`, id); err != nil {
			return err
		}
		_, err = tx.db.ExecContext(ctx, `DELETE FROM foundry_automations WHERE id=?`, id)
		return err
	})
	if err != nil {
		return err
	}
	for {
		deleted, err := s.deleteFoundryAutomationRuns(ctx, `SELECT id FROM foundry_automation_runs WHERE automation_id=? AND finished_at IS NOT NULL LIMIT ?`, id)
		if err != nil || deleted < foundryAutomationRunsBatch {
			return err
		}
	}
}

// foundryAutomationRunsBatch is how many runs one write deletes.
const foundryAutomationRunsBatch = 500

// deleteFoundryAutomationRuns deletes up to foundryAutomationRunsBatch of the
// runs pick selects (with arg, then the batch's size: ended ones only), their
// steps, and their Assistant threads, in one write, skipping a run another
// write holds; it returns how many it deleted.
func (s *mysqlDB) deleteFoundryAutomationRuns(ctx context.Context, pick string, arg any) (int, error) {
	var deleted int
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		rows, err := tx.db.QueryContext(ctx, pick+` FOR UPDATE SKIP LOCKED`, arg, foundryAutomationRunsBatch)
		if err != nil {
			return err
		}
		ids, err := scanAll(rows, func(r rowScanner) (any, error) {
			var id string
			if err := r.Scan(&id); err != nil {
				return nil, err
			}
			return id, nil
		})
		if err != nil || len(ids) == 0 {
			return err
		}
		in := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(ids)), ", ") + ")"
		for _, q := range []string{
			`UPDATE foundry_agent_threads SET deleted_at=COALESCE(deleted_at, NOW(6))
WHERE id IN (SELECT thread_id FROM foundry_automation_runs WHERE id IN ` + in + `)`,
			`DELETE FROM foundry_automation_run_nodes WHERE run_id IN ` + in,
			`DELETE FROM foundry_automation_runs WHERE id IN ` + in,
		} {
			if _, err := tx.db.ExecContext(ctx, q, ids...); err != nil {
				return err
			}
		}
		deleted = len(ids)
		return nil
	})
	return deleted, err
}

// PruneFoundryAutomationRuns deletes up to a batch of the runs that ended
// before before, with their steps and Assistant threads, oldest first, and
// returns how many it deleted: a workspace's run history is what ended within
// the worker's retention.
func (s *mysqlDB) PruneFoundryAutomationRuns(ctx context.Context, before time.Time) (int, error) {
	return s.deleteFoundryAutomationRuns(ctx, `SELECT id FROM foundry_automation_runs WHERE finished_at < ? ORDER BY finished_at LIMIT ?`, before.UTC())
}

// cancelFoundryAutomationTurns cancels the live Assistant turns of the runs
// runs selects (with arg), charging what they reserved.
func (s *mysqlDB) cancelFoundryAutomationTurns(ctx context.Context, runs string, arg any, why string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_agent_turns
SET status='cancelled', error=?, lease_owner=NULL, lease_until=NULL, usd_micros=usd_micros+reserved_micros, reserved_micros=0, finished_at=NOW(6)
WHERE automation_run_id IN (`+runs+`) AND status IN ('queued', 'running')`, why, arg)
	return err
}

// RunFoundryAutomationNow queues run, its owner's run by hand of the
// automation's approved spec (active or paused), with its trigger's output
// (triggerNodeID's, the trigger of the spec at run.SpecRevision, which the
// caller read); the same trigger key (the owner's client token) returns the
// run it queued. Refusals are a FoundryAutomationRefusal: another's
// (FoundryAutomationNotOwner), one waiting for approval
// (FoundryAutomationUnapproved), a spec changed since the caller read it
// (FoundryAutomationChanged), a live run (FoundryAutomationBusy), the day's
// runs spent (FoundryAutomationRunsPerDay).
func (s *mysqlDB) RunFoundryAutomationNow(ctx context.Context, run *model.FoundryAutomationRun, triggerNodeID string, triggerOutput json.RawMessage,
	limits model.FoundryAutomationLimits) (*model.FoundryAutomationRun, error) {
	if run.ID == "" || run.TriggerKey == "" || len(run.TriggerEvent) == 0 || triggerNodeID == "" {
		return nil, fmt.Errorf("foundry automation run is incomplete")
	}
	var out *model.FoundryAutomationRun
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		// The workspace's lock first, so its runs of the day are counted one
		// fire at a time.
		if err := tx.workspaces.Lock(ctx, tx.db, run.WorkspaceID); err != nil {
			return err
		}
		a, err := tx.lockFoundryAutomation(ctx, run.WorkspaceID, run.AutomationID)
		if err != nil {
			return err
		}
		if existing, err := scanFoundryAutomationRun(tx.db.QueryRowContext(ctx, "SELECT "+foundryAutomationRunColumns+`
FROM foundry_automation_runs WHERE automation_id=? AND trigger_key=?`, a.ID, run.TriggerKey), false); err == nil {
			out = existing
			return nil
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
		switch {
		case a.OwnerUserID != run.OwnerUserID:
			return model.FoundryAutomationRefusal{Reason: model.FoundryAutomationNotOwner, Detail: "only its owner runs a workflow: it runs as them"}
		case a.Status == model.FoundryAutomationNeedsApproval:
			return model.FoundryAutomationRefusal{Reason: model.FoundryAutomationUnapproved, Detail: "the workflow's changes wait for its owner's approval"}
		case a.SpecRevision != run.SpecRevision:
			return model.FoundryAutomationRefusal{Reason: model.FoundryAutomationChanged, Detail: "the workflow's steps changed meanwhile: run it again"}
		}
		refusal, err := tx.foundryAutomationRoom(ctx, a, limits)
		switch {
		case err != nil:
			return err
		case refusal != nil:
			return *refusal
		}
		if err := tx.insertFoundryAutomationRun(ctx, run, "queued", "", triggerNodeID, triggerOutput); err != nil {
			return err
		}
		out, err = scanFoundryAutomationRun(tx.db.QueryRowContext(ctx, "SELECT "+foundryAutomationRunColumns+` FROM foundry_automation_runs WHERE id=?`, run.ID), false)
		return err
	})
	return out, err
}

// foundryAutomationRoom is why the locked automation can't start a run now:
// one is live, or its workspace's runs of the last 24 hours reached the
// cap; nil when it can. Its caller holds the workspace's lock, taken before
// the automation's, so two automations of one workspace never both take the
// last run of the day.
func (s *mysqlDB) foundryAutomationRoom(ctx context.Context, a *model.FoundryAutomation, limits model.FoundryAutomationLimits) (*model.FoundryAutomationRefusal, error) {
	var live, today int
	if err := s.db.QueryRowContext(ctx, `SELECT
  (SELECT COUNT(*) FROM foundry_automation_runs WHERE live_automation=?),
  (SELECT COUNT(*) FROM foundry_automation_runs WHERE workspace_id=? AND created_at >= NOW(6) - INTERVAL 1 DAY AND status <> 'skipped')`,
		a.ID, a.WorkspaceID).Scan(&live, &today); err != nil {
		return nil, err
	}
	switch {
	case live > 0:
		return &model.FoundryAutomationRefusal{Reason: model.FoundryAutomationBusy, Detail: "a run of the workflow is still going"}, nil
	case today >= limits.RunsPerDay:
		return &model.FoundryAutomationRefusal{Reason: model.FoundryAutomationRunsPerDay,
			Detail: fmt.Sprintf("the workspace's workflows ran %d times in the last 24 hours, as many as they may", today)}, nil
	}
	return nil, nil
}

// insertFoundryAutomationRun stores run, of the locked automation's spec as
// it is, with status (and why, for a skipped one), and its trigger's step,
// succeeded with its output. A skipped run runs nothing, so it keeps no spec.
func (s *mysqlDB) insertFoundryAutomationRun(ctx context.Context, run *model.FoundryAutomationRun, status, why, triggerNodeID string,
	triggerOutput json.RawMessage) error {
	finished := "NULL"
	if status == string(model.FoundryAutomationRunSkipped) {
		finished = "NOW(6)"
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_automation_runs
  (id, automation_id, workspace_id, owner_user_id, spec, spec_revision, trigger_key, trigger_event, status, error, finished_at)
SELECT ?, id, workspace_id, owner_user_id, IF(? = 'skipped', NULL, spec), spec_revision, ?, CAST(? AS JSON), ?, NULLIF(?, ''), `+finished+`
FROM foundry_automations WHERE id=?`, run.ID, status, run.TriggerKey, string(run.TriggerEvent), status, why, run.AutomationID); err != nil {
		return err
	}
	if status == string(model.FoundryAutomationRunSkipped) {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_automation_run_nodes (run_id, node_id, status, output, finished_at)
VALUES (?, ?, 'succeeded', ?, NOW(6))`, run.ID, triggerNodeID, nullJSON(triggerOutput))
	return err
}

func (s *mysqlDB) GetFoundryAutomationRun(ctx context.Context, workspaceID, id string) (*model.FoundryAutomationRun, error) {
	return scanFoundryAutomationRun(s.db.QueryRowContext(ctx, "SELECT spec, "+foundryAutomationRunColumns+`
FROM foundry_automation_runs WHERE id=? AND workspace_id=?`, id, workspaceID), true)
}

// ListFoundryAutomationRuns returns the automation's runs, newest first,
// without their specs, a page of limit at a time.
func (s *mysqlDB) ListFoundryAutomationRuns(ctx context.Context, automationID, pageToken string, limit int) ([]*model.FoundryAutomationRun, string, error) {
	conds, args := []string{"automation_id=?"}, []any{automationID}
	if pageToken != "" {
		cursor, err := decodeTimeIDCursor(pageToken)
		if err != nil {
			return nil, "", err
		}
		conds = append(conds, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryAutomationRunColumns+" FROM foundry_automation_runs"+whereClause(conds)+
		fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT %d", limit+1), args...)
	if err != nil {
		return nil, "", err
	}
	out, err := scanAll(rows, func(r rowScanner) (*model.FoundryAutomationRun, error) { return scanFoundryAutomationRun(r, false) })
	if err != nil || len(out) <= limit {
		return out, "", err
	}
	next, err := encodeTimeIDCursor(out[limit-1].CreatedAt, out[limit-1].ID)
	return out[:limit], next, err
}

// ListFoundryAutomationRunNodes returns the run's steps that started, in the
// order they did.
func (s *mysqlDB) ListFoundryAutomationRunNodes(ctx context.Context, runID string) ([]*model.FoundryAutomationRunNode, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryAutomationNodeColumns+`
FROM foundry_automation_run_nodes WHERE run_id=? ORDER BY started_at, node_id`, runID)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundryAutomationRunNode)
}

// CancelFoundryAutomationRun ends the live run cancelled, as by (its
// automation's owner, or an owner or admin), with the steps it was running
// and the Assistant turn it waited on, and finishes the workflow runs it
// started, unassessed. Cancelling it again returns it; one that ended
// otherwise is FoundryAutomationRunEnded.
func (s *mysqlDB) CancelFoundryAutomationRun(ctx context.Context, workspaceID, id string, by model.FoundryAutomationActor) (*model.FoundryAutomationRun, error) {
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var automationID string
		if err := tx.db.QueryRowContext(ctx, `SELECT automation_id FROM foundry_automation_runs WHERE id=? AND workspace_id=?`, id, workspaceID).
			Scan(&automationID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		a, err := tx.lockFoundryAutomation(ctx, workspaceID, automationID)
		if err != nil {
			return err
		}
		if a.OwnerUserID != by.UserID && !by.Manager {
			return model.FoundryAutomationRefusal{Reason: model.FoundryAutomationNotAllowed, Detail: "only its owner, or an owner or admin, cancels a workflow's run"}
		}
		var status string
		if err := tx.db.QueryRowContext(ctx, `SELECT status FROM foundry_automation_runs WHERE id=? FOR UPDATE`, id).Scan(&status); err != nil {
			return err
		}
		switch model.FoundryAutomationRunStatus(status) {
		case model.FoundryAutomationRunCancelled:
			return nil
		case model.FoundryAutomationRunQueued, model.FoundryAutomationRunRunning:
		default:
			return model.FoundryAutomationRefusal{Reason: model.FoundryAutomationRunEnded, Detail: "the run already ended: it " + status}
		}
		if err := tx.cancelFoundryAutomationTurns(ctx, `?`, id, "the run was cancelled"); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_automation_run_nodes
SET status='failed', error='The run was cancelled.', finished_at=NOW(6) WHERE run_id=? AND status IN ('running', 'executing')`, id); err != nil {
			return err
		}
		if _, err = tx.db.ExecContext(ctx, `UPDATE foundry_automation_runs
SET status='cancelled', error='Cancelled by a person.', lease_owner=NULL, lease_until=NULL, finished_at=NOW(6) WHERE id=?`, id); err != nil {
			return err
		}
		return tx.finishFoundryAutomationWorkflowRuns(ctx, `?`, id, "A person cancelled the workflow's run before it finished.", false)
	})
	if err != nil {
		return nil, err
	}
	return s.GetFoundryAutomationRun(ctx, workspaceID, id)
}

// PauseFoundryAutomationsOf pauses every active automation userID owns in
// the workspace, as one whose owner lost access: its host calls it in the
// transaction that removes the member. The worker pauses one itself when it
// finds its owner can't act, but this stops them at once.
func (s *mysqlDB) PauseFoundryAutomationsOf(ctx context.Context, workspaceID, userID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_automations
SET status='paused', paused_reason='owner_lost_access', paused_by_user_id=NULL, paused_at=NOW(6), next_fire_at=NULL, cursor_at=NULL, cursor_id='',
    updated_at=NOW(6)
WHERE workspace_id=? AND owner_user_id=? AND status='active'`, workspaceID, userID)
	return err
}

// DueFoundryAutomationSchedules returns up to limit active schedules whose
// time came, outside a backoff, earliest first.
func (s *mysqlDB) DueFoundryAutomationSchedules(ctx context.Context, limit int) ([]*model.FoundryAutomation, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryAutomationColumns+`
FROM foundry_automations WHERE status='active' AND next_fire_at <= NOW(6) AND (backoff_until IS NULL OR backoff_until <= NOW(6))
ORDER BY next_fire_at, id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundryAutomation)
}

// WatchedFoundryAutomations returns up to limit active event triggers that
// haven't looked for ended work in every, outside a backoff, those that
// looked longest ago first.
func (s *mysqlDB) WatchedFoundryAutomations(ctx context.Context, every time.Duration, limit int) ([]*model.FoundryAutomation, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryAutomationColumns+`
FROM foundry_automations WHERE status='active' AND trigger_kind IN `+foundryAutomationEventKinds+`
  AND (checked_at IS NULL OR checked_at <= NOW(6) - INTERVAL ? MICROSECOND) AND (backoff_until IS NULL OR backoff_until <= NOW(6))
ORDER BY checked_at IS NOT NULL, checked_at, id LIMIT ?`, every.Microseconds(), limit)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, scanFoundryAutomation)
}

// FindFoundryAutomationEnded returns up to limit rows of the work a's event
// trigger listens to that ended after its cursor and at least settle ago, in
// cursor order, each with how it ended: syncs of its connector, loads and
// drafts (not revisions) of its ontology, documents that arrived in its set.
// Each is read through its target's own index, bounded to what was created
// within foundryAutomationLookBack of the cursor.
//
// Waiting settle before a row counts lets every transaction that set an end
// time before it commit first: the cursor then never passes a row that
// becomes visible later with an earlier end time. Rows a fire passes without
// a run (another outcome, a load that failed) move the cursor all the same.
func (s *mysqlDB) FindFoundryAutomationEnded(ctx context.Context, a *model.FoundryAutomation, settle time.Duration, limit int) ([]model.FoundryAutomationEnded, error) {
	if a.Status != model.FoundryAutomationActive || a.Cursor == nil || a.TriggerTarget == nil {
		return nil, fmt.Errorf("foundry automation %s watches nothing", a.ID)
	}
	var q string
	switch a.TriggerKind {
	case "sync_finished":
		q = `SELECT finished_at, id, status, rows_synced FROM connector_syncs
WHERE connector_id=? AND workspace_id=? AND created_at >= ? AND status IN ('succeeded', 'failed')`
	case "load_ready":
		q = `SELECT finished_at, id, status, NULL FROM foundry_loads
WHERE ontology_id=? AND workspace_id=? AND created_at >= ? AND finished_at IS NOT NULL`
	case "generation_finished":
		q = `SELECT finished_at, id, status, NULL FROM foundry_ontology_generations
WHERE ontology_id=? AND workspace_id=? AND created_at >= ? AND status IN ('succeeded', 'failed') AND JSON_EXTRACT(input, '$.revise') IS NULL`
	case "document_arrived":
		q = `SELECT uploaded_at AS finished_at, id, status, NULL FROM foundry_documents
WHERE set_id=? AND workspace_id=? AND created_at >= ? AND status='uploaded' AND uploaded_at IS NOT NULL`
	default:
		return nil, fmt.Errorf("foundry automation %s: no ended work of trigger %q", a.ID, a.TriggerKind)
	}
	end := "finished_at"
	if a.TriggerKind == "document_arrived" {
		end = "uploaded_at"
	}
	q += ` AND ` + end + ` <= NOW(6) - INTERVAL ? MICROSECOND AND (` + end + ` > ? OR (` + end + ` = ? AND id > ?))
ORDER BY ` + end + `, id LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, *a.TriggerTarget, a.WorkspaceID, a.Cursor.At.Add(-foundryAutomationLookBack), settle.Microseconds(),
		a.Cursor.At, a.Cursor.At, a.Cursor.ID, limit)
	if err != nil {
		return nil, err
	}
	return scanAll(rows, func(r rowScanner) (model.FoundryAutomationEnded, error) {
		var e model.FoundryAutomationEnded
		var rowsSynced sql.NullInt64
		if err := r.Scan(&e.Cursor.At, &e.Cursor.ID, &e.Outcome, &rowsSynced); err != nil {
			return e, err
		}
		if rowsSynced.Valid {
			e.Rows = &rowsSynced.Int64
		}
		return e, nil
	})
}

// TouchFoundryAutomation records that the automation looked for ended work.
func (s *mysqlDB) TouchFoundryAutomation(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_automations SET checked_at=NOW(6) WHERE id=?`, id)
	return err
}

// FireFoundryAutomation moves the automation on as f says and queues f.Run,
// in one write under the automation's lock, unless it is no longer active
// or another fire moved it first (FoundryAutomationFiredLost). A schedule
// that fires while a run is live, or past its workspace's runs of the day,
// records its run as skipped; work an event trigger found waits while a run
// is live (FoundryAutomationFiredWaiting), and past the runs of the day
// records it skipped. A trigger key already used moves it on with no run:
// the fire was delivered before.
func (s *mysqlDB) FireFoundryAutomation(ctx context.Context, f model.FoundryAutomationFire, limits model.FoundryAutomationLimits) (
	model.FoundryAutomationFired, error) {
	if f.Run != nil && (f.Run.ID == "" || f.Run.TriggerKey == "" || len(f.Run.TriggerEvent) == 0 || f.TriggerNodeID == "") {
		return "", fmt.Errorf("foundry automation run is incomplete")
	}
	var fired model.FoundryAutomationFired
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		// A fire that may queue a run takes the workspace's lock first, as
		// foundryAutomationRoom's count needs.
		if f.Run != nil {
			if err := tx.workspaces.Lock(ctx, tx.db, f.WorkspaceID); err != nil {
				return err
			}
		}
		a, err := tx.lockFoundryAutomation(ctx, f.WorkspaceID, f.AutomationID)
		if errors.Is(err, ErrNotFound) {
			fired = model.FoundryAutomationFiredLost
			return nil
		}
		if err != nil {
			return err
		}
		var move string
		var moveArgs []any
		switch adv := f.Advance.(type) {
		case model.FoundryScheduleAdvance:
			if a.Status != model.FoundryAutomationActive || a.NextFireAt == nil || !a.NextFireAt.Equal(adv.Due) {
				fired = model.FoundryAutomationFiredLost
				return nil
			}
			move, moveArgs = "next_fire_at=?", []any{adv.Next.UTC()}
		case model.FoundryCursorAdvance:
			if a.Status != model.FoundryAutomationActive || a.Cursor == nil || *a.Cursor != adv.From {
				fired = model.FoundryAutomationFiredLost
				return nil
			}
			move, moveArgs = "cursor_at=?, cursor_id=?", []any{adv.To.At.UTC(), adv.To.ID}
		default:
			return fmt.Errorf("fire foundry automation %s: no advance", f.AutomationID)
		}
		moveOn := func() error {
			_, err := tx.db.ExecContext(ctx, "UPDATE foundry_automations SET "+move+", checked_at=NOW(6) WHERE id=?", append(moveArgs, a.ID)...)
			return err
		}
		if f.Run == nil {
			fired = model.FoundryAutomationFiredMoved
			return moveOn()
		}
		var used int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_automation_runs WHERE automation_id=? AND trigger_key=?`,
			a.ID, f.Run.TriggerKey).Scan(&used); err != nil {
			return err
		}
		if used > 0 {
			fired = model.FoundryAutomationFiredMoved
			return moveOn()
		}
		refusal, err := tx.foundryAutomationRoom(ctx, a, limits)
		if err != nil {
			return err
		}
		_, schedule := f.Advance.(model.FoundryScheduleAdvance)
		switch {
		case refusal != nil && refusal.Reason == model.FoundryAutomationBusy && !schedule:
			fired = model.FoundryAutomationFiredWaiting
			_, err := tx.db.ExecContext(ctx, `UPDATE foundry_automations SET checked_at=NOW(6) WHERE id=?`, a.ID)
			return err
		case refusal != nil:
			fired = model.FoundryAutomationFiredSkipped
			why := "Skipped: the run before it was still going."
			if refusal.Reason == model.FoundryAutomationRunsPerDay {
				why = "Skipped: the workspace's workflows ran as many times as they may in 24 hours."
			}
			if err := tx.insertFoundryAutomationRun(ctx, f.Run, string(model.FoundryAutomationRunSkipped), why, "", nil); err != nil {
				return err
			}
			return moveOn()
		}
		fired = model.FoundryAutomationFiredQueued
		if err := tx.insertFoundryAutomationRun(ctx, f.Run, string(model.FoundryAutomationRunQueued), "", f.TriggerNodeID, f.TriggerOutput); err != nil {
			return err
		}
		return moveOn()
	})
	return fired, err
}

// ClaimFoundryAutomationRun leases the run due first to workerID, with its
// spec: queued and due, or running under a lease that ran out. nil means none
// is due.
func (s *mysqlDB) ClaimFoundryAutomationRun(ctx context.Context, workerID string, lease time.Duration) (*model.FoundryAutomationRun, error) {
	return claimLease(ctx, s.root, "foundry automation run", "foundry_automation_runs", workerID, lease,
		func(tx *sql.Tx) (*model.FoundryAutomationRun, string, error) {
			r, err := scanFoundryAutomationRun(tx.QueryRowContext(ctx, "SELECT spec, "+foundryAutomationRunColumns+`
FROM foundry_automation_runs WHERE `+dueFoundryAutomationRun+` ORDER BY next_attempt_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`), true)
			if err != nil {
				return nil, "", err
			}
			return r, r.ID, nil
		},
		func(_ *sql.Tx, r *model.FoundryAutomationRun, now, leaseUntil time.Time) error {
			r.Status, r.LeaseOwner, r.LeaseUntil, r.Attempts = model.FoundryAutomationRunRunning, &workerID, &leaseUntil, r.Attempts+1
			if r.StartedAt == nil {
				r.StartedAt = &now
			}
			return nil
		})
}

// ownFoundryAutomationRun locks the run while workerID holds its lease;
// ErrNotFound means it lost it, to a cancel, a deletion or another worker.
func (s *mysqlDB) ownFoundryAutomationRun(ctx context.Context, id, workerID string) error {
	var held string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM foundry_automation_runs`+ownedFoundryAutomationRun+` FOR UPDATE`, id, workerID).Scan(&held)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// RenewFoundryAutomationRunLease extends the lease; ErrNotFound means
// workerID lost it.
func (s *mysqlDB) RenewFoundryAutomationRunLease(ctx context.Context, id, workerID string, lease time.Duration) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_automation_runs SET lease_until=?`+ownedFoundryAutomationRun,
		time.Now().UTC().Add(lease), id, workerID)
}

// StartFoundryAutomationNode records a step of the run workerID holds as
// started: running, or executing (an action, whose call follows). A step
// that started before is ErrConflict.
func (s *mysqlDB) StartFoundryAutomationNode(ctx context.Context, runID, workerID, nodeID string, status model.FoundryAutomationNodeStatus) error {
	if status != model.FoundryAutomationNodeRunning && status != model.FoundryAutomationNodeExecuting {
		return fmt.Errorf("a step starts running or executing, not %q", status)
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.ownFoundryAutomationRun(ctx, runID, workerID); err != nil {
			return err
		}
		_, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_automation_run_nodes (run_id, node_id, status) VALUES (?, ?, ?)`, runID, nodeID, string(status))
		if isDuplicateKey(err) {
			return ErrConflict
		}
		return err
	})
}

// StartFoundryAutomationAssistant starts an Assistant step of the run
// workerID holds: turn, queued in the run's thread (thread, new, when the run
// has none yet), and the step running, waiting on it. A step that started
// before is ErrConflict; a thread with a live turn already (its owner ran a
// card the run left waiting) is ErrThreadBusy, and nothing is written.
func (s *mysqlDB) StartFoundryAutomationAssistant(ctx context.Context, runID, workerID, nodeID string, thread *model.FoundryAgentThread,
	turn *model.FoundryAgentTurn) error {
	switch {
	case turn.Kind != model.FoundryAgentTurnAutomation || turn.AutomationRunID == nil || *turn.AutomationRunID != runID ||
		turn.AutomationNodeID == nil || *turn.AutomationNodeID != nodeID || turn.MaxUSDMicros <= 0:
		return fmt.Errorf("foundry automation %s: the turn is not its step's", runID)
	case thread != nil && (thread.ID != turn.ThreadID || thread.Kind != model.FoundryAgentThreadAutomation):
		return fmt.Errorf("foundry automation %s: the thread is not its turn's", runID)
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.ownFoundryAutomationRun(ctx, runID, workerID); err != nil {
			return err
		}
		if thread != nil {
			if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_agent_threads (id, workspace_id, user_id, kind, title, runtime)
VALUES (?, ?, ?, 'automation', ?, 'api')`, thread.ID, thread.WorkspaceID, thread.UserID, thread.Title); err != nil {
				return err
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_automation_runs SET thread_id=? WHERE id=?`, thread.ID, runID); err != nil {
				return err
			}
		}
		_, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_automation_run_nodes (run_id, node_id, status, turn_id) VALUES (?, ?, 'running', ?)`,
			runID, nodeID, turn.ID)
		if isDuplicateKey(err) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		_, err = tx.db.ExecContext(ctx, `INSERT INTO foundry_agent_turns
  (id, workspace_id, thread_id, user_id, runtime, kind, input, status, max_usd_micros, automation_run_id, automation_node_id)
VALUES (?, ?, ?, ?, 'api', 'automation', ?, 'queued', ?, ?, ?)`, turn.ID, turn.WorkspaceID, turn.ThreadID, turn.UserID, turn.Input, turn.MaxUSDMicros,
			runID, nodeID)
		if isDuplicateKey(err) {
			// The turn's id is new: only the thread's one live turn clashes.
			return ErrThreadBusy
		}
		if err != nil {
			return err
		}
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_agent_threads SET updated_at=NOW(6) WHERE id=?`, turn.ThreadID)
		return err
	})
}

// EndFoundryAutomationNode ends a step of the run workerID holds: one that
// started, or one that ends as it starts (a condition, a notify step, a step
// skipped or blocked), adding its spend to the run's and queueing its email,
// once: a step that already ended is ErrConflict.
func (s *mysqlDB) EndFoundryAutomationNode(ctx context.Context, runID, workerID, nodeID string, end model.FoundryAutomationNodeEnd) error {
	if err := end.Validate(); err != nil {
		return fmt.Errorf("end foundry automation step %s/%s: %w", runID, nodeID, err)
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.ownFoundryAutomationRun(ctx, runID, workerID); err != nil {
			return err
		}
		var started string
		switch err := tx.db.QueryRowContext(ctx, `SELECT status FROM foundry_automation_run_nodes WHERE run_id=? AND node_id=? FOR UPDATE`,
			runID, nodeID).Scan(&started); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case model.FoundryAutomationNodeStatus(started).Ended():
			return ErrConflict
		}
		var notificationID any
		if n := end.Notification; n != nil {
			notificationID = n.SubjectID
			if _, err := tx.insertFoundryNotification(ctx, n); err != nil {
				return err
			}
		}
		args := []any{string(end.Status), nullJSON(end.Output), end.Error, notificationID, end.USDMicros, runID, nodeID}
		if started == "" {
			if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_automation_run_nodes
  (status, output, error, notification_id, usd_micros, run_id, node_id, finished_at)
VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, NOW(6))`, args...); err != nil {
				return err
			}
		} else if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_automation_run_nodes
SET status=?, output=?, error=NULLIF(?, ''), notification_id=?, usd_micros=?, finished_at=NOW(6) WHERE run_id=? AND node_id=?`, args...); err != nil {
			return err
		}
		_, err := tx.db.ExecContext(ctx, `UPDATE foundry_automation_runs SET usd_micros=usd_micros+? WHERE id=?`, end.USDMicros, runID)
		return err
	})
}

// YieldFoundryAutomationRun hands the run workerID holds back to the queue
// until at, its attempt uncounted: it waits on a step, not on a failure.
func (s *mysqlDB) YieldFoundryAutomationRun(ctx context.Context, id, workerID string, at time.Time) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_automation_runs
SET status='queued', attempts=GREATEST(attempts-1, 0), lease_owner=NULL, lease_until=NULL, next_attempt_at=?`+ownedFoundryAutomationRun,
		at.UTC(), id, workerID)
}

// RetryFoundryAutomationRun hands the run workerID holds back to the queue
// until at, saying why its attempt failed.
func (s *mysqlDB) RetryFoundryAutomationRun(ctx context.Context, id, workerID string, at time.Time, why string) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_automation_runs
SET status='queued', error=?, lease_owner=NULL, lease_until=NULL, next_attempt_at=?`+ownedFoundryAutomationRun, why, at.UTC(), id, workerID)
}

// FinishFoundryAutomationRun ends the run workerID holds as end says, and
// counts it toward its automation's failures in a row: a success clears
// them; a failure holds the automation's next run back by end.Backoff,
// doubling with each one in a row up to a day, and pauses it once they reach
// end.PauseAfter, queueing end.Notice for its owner. end.LostAccess pauses it
// at once, unnoticed: its owner can't act in the workspace. The workflow runs
// it started finish with end.Answer, assessed with end.Assess.
func (s *mysqlDB) FinishFoundryAutomationRun(ctx context.Context, id, workerID string, end model.FoundryAutomationRunEnd) error {
	if err := end.Validate(); err != nil {
		return fmt.Errorf("finish foundry automation run %s: %w", id, err)
	}
	var automationID, workspaceID string
	if err := s.db.QueryRowContext(ctx, `SELECT automation_id, workspace_id FROM foundry_automation_runs WHERE id=?`, id).
		Scan(&automationID, &workspaceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		// The automation's lock first, as every write to it takes, then the run's.
		a, err := tx.lockFoundryAutomation(ctx, workspaceID, automationID)
		if err != nil {
			return err
		}
		if err := tx.ownFoundryAutomationRun(ctx, id, workerID); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_automation_runs
SET status=?, error=NULLIF(?, ''), lease_owner=NULL, lease_until=NULL, finished_at=NOW(6) WHERE id=?`, string(end.Status), end.Error, id); err != nil {
			return err
		}
		if err := tx.finishFoundryAutomationWorkflowRuns(ctx, `?`, id, end.Answer, end.Assess); err != nil {
			return err
		}
		switch {
		case end.Status == model.FoundryAutomationRunSucceeded:
			_, err = tx.db.ExecContext(ctx, `UPDATE foundry_automations SET failures=0, backoff_until=NULL WHERE id=?`, a.ID)
			return err
		case end.LostAccess:
			// MySQL assigns left to right, each reading the ones before: status last.
			_, err = tx.db.ExecContext(ctx, `UPDATE foundry_automations
SET paused_reason=IF(status='active', 'owner_lost_access', paused_reason), paused_at=IF(status='active', NOW(6), paused_at),
    status=IF(status='active', 'paused', status), next_fire_at=NULL, cursor_at=NULL, cursor_id='', failures=failures+1, updated_at=NOW(6)
WHERE id=?`, a.ID)
			return err
		}
		failures := a.Failures + 1
		backoff := min(end.Backoff<<min(failures-1, 16), 24*time.Hour)
		if failures < end.PauseAfter || a.Status != model.FoundryAutomationActive {
			_, err = tx.db.ExecContext(ctx, `UPDATE foundry_automations SET failures=?, backoff_until=NOW(6) + INTERVAL ? MICROSECOND WHERE id=?`,
				failures, backoff.Microseconds(), a.ID)
			return err
		}
		var notice any
		if end.Notice != nil {
			notice = end.Notice.SubjectID
			if _, err := tx.insertFoundryNotification(ctx, end.Notice); err != nil {
				return err
			}
		}
		_, err = tx.db.ExecContext(ctx, `UPDATE foundry_automations
SET status='paused', paused_reason='failures', paused_at=NOW(6), next_fire_at=NULL, cursor_at=NULL, cursor_id='', failures=?, backoff_until=NULL,
    notice_id=COALESCE(?, notice_id), updated_at=NOW(6)
WHERE id=?`, failures, notice, a.ID)
		return err
	})
}

// GetFoundryAutomationNotice reads what an automation's email tells: its
// notify step's run, step and the steps before (automation_run), or the
// automation that paused itself (automation_paused). ErrNotFound when the
// automation, run or step is gone.
func (s *mysqlDB) GetFoundryAutomationNotice(ctx context.Context, n *model.FoundryNotification) (*model.FoundryAutomationNotice, error) {
	switch n.Kind {
	case model.FoundryNotifyAutomationPaused:
		a, err := scanFoundryAutomation(s.db.QueryRowContext(ctx, "SELECT "+foundryAutomationColumns+`
FROM foundry_automations WHERE notice_id=? AND workspace_id=?`, n.SubjectID, n.WorkspaceID))
		if err != nil {
			return nil, err
		}
		return &model.FoundryAutomationNotice{Automation: a}, nil
	case model.FoundryNotifyAutomationRun:
		node, err := scanFoundryAutomationRunNode(s.db.QueryRowContext(ctx, "SELECT "+foundryAutomationNodeColumns+`
FROM foundry_automation_run_nodes WHERE notification_id=?`, n.SubjectID))
		if err != nil {
			return nil, err
		}
		run, err := s.GetFoundryAutomationRun(ctx, n.WorkspaceID, node.RunID)
		if err != nil {
			return nil, err
		}
		a, err := s.GetFoundryAutomation(ctx, n.WorkspaceID, run.AutomationID)
		if err != nil {
			return nil, err
		}
		nodes, err := s.ListFoundryAutomationRunNodes(ctx, run.ID)
		if err != nil {
			return nil, err
		}
		out := &model.FoundryAutomationNotice{Automation: a, Run: run, Node: node}
		for _, other := range nodes {
			if other.NodeID != node.NodeID && other.Status.Ended() {
				out.Earlier = append(out.Earlier, other)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("an email of kind %q is no automation's", n.Kind)
}

func utcOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

func scanFoundryAutomation(scanner rowScanner) (*model.FoundryAutomation, error) {
	var a model.FoundryAutomation
	var spec, status string
	var approved sql.NullInt64
	var target, pausedReason, pausedBy, notice, clientToken sql.NullString
	var approvedAt, pausedAt, backoff, nextFire, cursorAt, checked sql.NullTime
	var cursorID string
	if err := scanner.Scan(&a.ID, &a.WorkspaceID, &a.OwnerUserID, &a.Name, &a.Description, &spec, &a.Revision, &a.SpecRevision, &approved,
		&approvedAt, &a.TriggerKind, &target, &status, &pausedReason, &pausedBy, &pausedAt, &a.Failures, &backoff,
		&nextFire, &cursorAt, &cursorID, &checked, &notice, &clientToken, &a.CreatedAt, &a.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	a.Spec, a.Status = json.RawMessage(spec), model.FoundryAutomationStatus(status)
	if approved.Valid {
		a.ApprovedSpecRevision = &approved.Int64
	}
	if pausedReason.Valid {
		reason := model.FoundryAutomationPause(pausedReason.String)
		a.PausedReason = &reason
	}
	if cursorAt.Valid {
		a.Cursor = &model.FoundryAutomationCursor{At: cursorAt.Time, ID: cursorID}
	}
	for _, f := range []struct {
		src sql.NullString
		dst **string
	}{{target, &a.TriggerTarget}, {pausedBy, &a.PausedByUserID}, {notice, &a.NoticeID}, {clientToken, &a.ClientToken}} {
		if f.src.Valid {
			*f.dst = &f.src.String
		}
	}
	for _, f := range []struct {
		src sql.NullTime
		dst **time.Time
	}{{approvedAt, &a.ApprovedAt}, {pausedAt, &a.PausedAt}, {backoff, &a.BackoffUntil}, {nextFire, &a.NextFireAt}, {checked, &a.CheckedAt}} {
		if f.src.Valid {
			*f.dst = &f.src.Time
		}
	}
	return &a, nil
}

// scanFoundryAutomationRun scans a run's columns, after its spec when
// withSpec.
func scanFoundryAutomationRun(scanner rowScanner, withSpec bool) (*model.FoundryAutomationRun, error) {
	var r model.FoundryAutomationRun
	var event, status string
	var spec, errText, thread, leaseOwner sql.NullString
	var leaseUntil, startedAt, finishedAt sql.NullTime
	dest := []any{&r.ID, &r.AutomationID, &r.WorkspaceID, &r.OwnerUserID, &r.SpecRevision, &r.TriggerKey, &event, &status, &errText,
		&thread, &leaseOwner, &leaseUntil, &r.Attempts, &r.NextAttemptAt, &r.USDMicros, &r.CreatedAt, &startedAt, &finishedAt}
	if withSpec {
		dest = append([]any{&spec}, dest...)
	}
	if err := scanner.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	r.TriggerEvent, r.Status = json.RawMessage(event), model.FoundryAutomationRunStatus(status)
	if spec.Valid {
		r.Spec = json.RawMessage(spec.String)
	}
	for _, f := range []struct {
		src sql.NullString
		dst **string
	}{{errText, &r.Error}, {thread, &r.ThreadID}, {leaseOwner, &r.LeaseOwner}} {
		if f.src.Valid {
			*f.dst = &f.src.String
		}
	}
	for _, f := range []struct {
		src sql.NullTime
		dst **time.Time
	}{{leaseUntil, &r.LeaseUntil}, {startedAt, &r.StartedAt}, {finishedAt, &r.FinishedAt}} {
		if f.src.Valid {
			*f.dst = &f.src.Time
		}
	}
	return &r, nil
}

func scanFoundryAutomationRunNode(scanner rowScanner) (*model.FoundryAutomationRunNode, error) {
	var n model.FoundryAutomationRunNode
	var status string
	var output, errText, turn, notification sql.NullString
	var finishedAt sql.NullTime
	if err := scanner.Scan(&n.RunID, &n.NodeID, &status, &output, &errText, &turn, &notification, &n.USDMicros, &n.StartedAt, &finishedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	n.Status = model.FoundryAutomationNodeStatus(status)
	if output.Valid {
		n.Output = json.RawMessage(output.String)
	}
	for _, f := range []struct {
		src sql.NullString
		dst **string
	}{{errText, &n.Error}, {turn, &n.TurnID}, {notification, &n.NotificationID}} {
		if f.src.Valid {
			*f.dst = &f.src.String
		}
	}
	if finishedAt.Valid {
		n.FinishedAt = &finishedAt.Time
	}
	return &n, nil
}
