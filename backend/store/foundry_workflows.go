package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// ErrWorkflowRunFinished is an access recorded under a run that finished:
// it records nothing more.
var ErrWorkflowRunFinished = errors.New("store: the workflow run finished")

// Lock order, so the writers below never wait on each other in a cycle: an
// ontology, then its workflows, then their runs, then the runs' accesses.

const foundryWorkflowColumns = `w.id, w.workspace_id, w.ontology_id, w.name, w.description, w.client_token, w.created_by_user_id, w.created_at,
  (SELECT COUNT(*) FROM foundry_workflow_runs r WHERE r.workflow_id = w.id),
  (SELECT COUNT(*) FROM foundry_workflow_runs r WHERE r.workflow_id = w.id AND r.status = 'finished'),
  (SELECT MAX(r.started_at) FROM foundry_workflow_runs r WHERE r.workflow_id = w.id)`

func scanFoundryWorkflow(row rowScanner) (*model.FoundryWorkflow, error) {
	var w model.FoundryWorkflow
	var last sql.NullTime
	err := row.Scan(&w.ID, &w.WorkspaceID, &w.OntologyID, &w.Name, &w.Description, &w.ClientToken, &w.CreatedByUserID,
		&w.CreatedAt, &w.Runs, &w.FinishedRuns, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	w.LastRunAt = timePtr(last)
	return &w, nil
}

// CreateFoundryWorkflow stores w under its ontology's lock, so a deletion of
// the ontology can't leave it behind: ErrNotFound when the ontology is
// deleted or absent, ErrLimitExceeded at model.FoundryWorkflowsMax, and
// ErrConflict when the ontology has a workflow with w's client token.
func (s *mysqlDB) CreateFoundryWorkflow(ctx context.Context, w *model.FoundryWorkflow) error {
	switch {
	case w.ID == "" || w.WorkspaceID == "" || w.OntologyID == "" || w.ClientToken == "" || w.CreatedByUserID == "":
		return fmt.Errorf("foundry workflow is missing an id")
	case strings.TrimSpace(w.Name) == "":
		return fmt.Errorf("foundry workflow %s has no name", w.ID)
	}
	return s.inTx(ctx, func(tx *mysqlDB) error {
		if _, err := tx.lockLiveFoundryOntology(ctx, w.WorkspaceID, w.OntologyID); err != nil {
			return err
		}
		var n int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_workflows WHERE ontology_id=?`, w.OntologyID).Scan(&n); err != nil {
			return err
		}
		if n >= model.FoundryWorkflowsMax {
			return ErrLimitExceeded
		}
		_, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_workflows
  (id, workspace_id, ontology_id, name, description, client_token, created_by_user_id) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			w.ID, w.WorkspaceID, w.OntologyID, w.Name, w.Description, w.ClientToken, w.CreatedByUserID)
		if isDuplicateKey(err) {
			return ErrConflict
		}
		return err
	})
}

// GetFoundryWorkflow reads one of an ontology's workflows.
func (s *mysqlDB) GetFoundryWorkflow(ctx context.Context, ontologyID, id string) (*model.FoundryWorkflow, error) {
	return scanFoundryWorkflow(s.db.QueryRowContext(ctx, "SELECT "+foundryWorkflowColumns+`
FROM foundry_workflows w WHERE w.id=? AND w.ontology_id=?`, id, ontologyID))
}

// GetFoundryWorkflowByClientToken reads the workflow an ontology created
// with clientToken.
func (s *mysqlDB) GetFoundryWorkflowByClientToken(ctx context.Context, ontologyID, clientToken string) (*model.FoundryWorkflow, error) {
	return scanFoundryWorkflow(s.db.QueryRowContext(ctx, "SELECT "+foundryWorkflowColumns+`
FROM foundry_workflows w WHERE w.ontology_id=? AND w.client_token=?`, ontologyID, clientToken))
}

// ListFoundryWorkflows lists an ontology's workflows newest first, limit a
// page.
func (s *mysqlDB) ListFoundryWorkflows(ctx context.Context, ontologyID, pageToken string, limit int) ([]*model.FoundryWorkflow, string, error) {
	conds, args := []string{"w.ontology_id=?"}, []any{ontologyID}
	if pageToken != "" {
		cursor, err := decodeTimeIDCursor(pageToken)
		if err != nil {
			return nil, "", err
		}
		conds = append(conds, "(w.created_at < ? OR (w.created_at = ? AND w.id < ?))")
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryWorkflowColumns+" FROM foundry_workflows w"+whereClause(conds)+
		fmt.Sprintf(" ORDER BY w.created_at DESC, w.id DESC LIMIT %d", limit+1), args...)
	if err != nil {
		return nil, "", err
	}
	out, err := scanAll(rows, scanFoundryWorkflow)
	if err != nil || len(out) <= limit {
		return out, "", err
	}
	next, err := encodeTimeIDCursor(out[limit-1].CreatedAt, out[limit-1].ID)
	return out[:limit], next, err
}

// DeleteFoundryWorkflow deletes one of an ontology's workflows with its runs
// and their accesses. ErrNotFound when the ontology has no such workflow.
func (s *mysqlDB) DeleteFoundryWorkflow(ctx context.Context, ontologyID, id string) error {
	if err := s.purgeFoundryWorkflowAccesses(ctx, "workflow_id", id, "ontology_id", ontologyID); err != nil {
		return err
	}
	return s.inTx(ctx, func(tx *mysqlDB) error {
		var locked string
		err := tx.db.QueryRowContext(ctx, `SELECT id FROM foundry_workflows WHERE id=? AND ontology_id=? FOR UPDATE`, id, ontologyID).Scan(&locked)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return tx.deleteFoundryWorkflowRows(ctx, "workflow_id", id)
	})
}

// purgeBatch is how many accesses one statement of a purge deletes.
const purgeBatch = 5000

// purgeFoundryWorkflowAccesses deletes the accesses of one workflow's runs
// (column workflow_id, within its ontology) or of every workflow of an
// ontology (ontology_id, within its workspace), purgeBatch at a time, each
// statement on its own, before the transaction that deletes the rest: a long
// history never holds its locks all at once. Only inside a host's
// transaction (InTx) does it leave them to it.
func (s *mysqlDB) purgeFoundryWorkflowAccesses(ctx context.Context, column, id, within, scope string) error {
	if s.root == nil {
		return nil
	}
	for {
		res, err := s.db.ExecContext(ctx, `DELETE FROM foundry_workflow_run_accesses WHERE run_id IN (
  SELECT id FROM foundry_workflow_runs WHERE `+column+`=? AND `+within+`=?) LIMIT `+fmt.Sprint(purgeBatch), id, scope)
		if err != nil {
			return fmt.Errorf("purge workflow accesses: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil || n < purgeBatch {
			return err
		}
	}
}

// deleteFoundryWorkflowRows deletes one workflow (column workflow_id) or
// every workflow of an ontology (ontology_id), with their runs and accesses,
// locking the workflows, then the runs, before deleting anything, in the
// order StartFoundryWorkflowRun and RecordFoundryWorkflowAccess lock them.
func (s *mysqlDB) deleteFoundryWorkflowRows(ctx context.Context, column, id string) error {
	workflowsBy := column
	if column == "workflow_id" {
		workflowsBy = "id"
	}
	for _, lock := range []string{
		`SELECT id FROM foundry_workflows WHERE ` + workflowsBy + `=? FOR UPDATE`,
		`SELECT id FROM foundry_workflow_runs WHERE ` + column + `=? FOR UPDATE`,
	} {
		rows, err := s.db.QueryContext(ctx, lock, id)
		if err != nil {
			return err
		}
		if _, err := scanStrings(rows); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`DELETE a FROM foundry_workflow_run_assessments a JOIN foundry_workflow_runs r ON r.id = a.run_id WHERE r.` + column + `=?`,
		`DELETE a FROM foundry_workflow_run_accesses a JOIN foundry_workflow_runs r ON r.id = a.run_id WHERE r.` + column + `=?`,
		`DELETE FROM foundry_workflow_runs WHERE ` + column + `=?`,
		`DELETE FROM foundry_workflows WHERE ` + workflowsBy + `=?`,
	} {
		if _, err := s.db.ExecContext(ctx, q, id); err != nil {
			return fmt.Errorf("delete workflows: %w", err)
		}
	}
	return nil
}

const foundryWorkflowRunColumns = `id, workflow_id, workspace_id, ontology_id, status, input, output, client_token,
  started_by_user_id, started_by_service_credential_id, started_at, finished_at,
  accesses, reads_count, questions, sends, uploads, pieces, results_omitted, result_bytes`

func scanFoundryWorkflowRun(row rowScanner) (*model.FoundryWorkflowRun, error) {
	var r model.FoundryWorkflowRun
	var status string
	var input, output, user, credential sql.NullString
	var finished sql.NullTime
	c := &r.Counts
	err := row.Scan(&r.ID, &r.WorkflowID, &r.WorkspaceID, &r.OntologyID, &status, &input, &output, &r.ClientToken,
		&user, &credential, &r.StartedAt, &finished,
		&c.Accesses, &c.Reads, &c.Questions, &c.Sends, &c.Uploads, &c.Pieces, &c.ResultsOmitted, &c.ResultBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if input.Valid {
		r.Input = &input.String
	}
	switch {
	case user.Valid:
		r.StartedBy = access.Principal{Kind: access.PrincipalUser, ID: user.String}
	default:
		r.StartedBy = access.Principal{Kind: access.PrincipalServiceCredential, ID: credential.String}
	}
	switch model.FoundryWorkflowRunStatus(status) {
	case model.FoundryWorkflowRunOpen:
	case model.FoundryWorkflowRunFinished:
		r.Finished = &model.FoundryWorkflowRunEnd{At: finished.Time, Output: output.String}
	default:
		return nil, fmt.Errorf("workflow run %s has status %q", r.ID, status)
	}
	return &r, nil
}

// StartFoundryWorkflowRun opens r under its workflow's lock, then deletes
// the workflow's runs past model.FoundryWorkflowRunsKept, oldest first, with
// their accesses. ErrNotFound when the workflow is deleted, ErrConflict when
// it has a run with r's client token.
func (s *mysqlDB) StartFoundryWorkflowRun(ctx context.Context, r *model.FoundryWorkflowRun) error {
	if r.ID == "" || r.WorkflowID == "" || r.WorkspaceID == "" || r.OntologyID == "" || r.ClientToken == "" || r.StartedBy.ID == "" {
		return fmt.Errorf("foundry workflow run is missing an id")
	}
	var user, credential any
	switch r.StartedBy.Kind {
	case access.PrincipalUser:
		user = r.StartedBy.ID
	case access.PrincipalServiceCredential:
		credential = r.StartedBy.ID
	default:
		return fmt.Errorf("foundry workflow run %s is started by a %s", r.ID, r.StartedBy.Kind)
	}
	return s.inTx(ctx, func(tx *mysqlDB) error {
		var locked string
		err := tx.db.QueryRowContext(ctx, `SELECT id FROM foundry_workflows WHERE id=? AND ontology_id=? FOR UPDATE`,
			r.WorkflowID, r.OntologyID).Scan(&locked)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var input any
		if r.Input != nil {
			input = *r.Input
		}
		_, err = tx.db.ExecContext(ctx, `INSERT INTO foundry_workflow_runs
  (id, workflow_id, workspace_id, ontology_id, status, input, client_token, started_by_user_id, started_by_service_credential_id)
VALUES (?, ?, ?, ?, 'open', ?, ?, ?, ?)`,
			r.ID, r.WorkflowID, r.WorkspaceID, r.OntologyID, input, r.ClientToken, user, credential)
		if isDuplicateKey(err) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		// A run started in the last day that is still open is an agent's at
		// work: it outlives the cap until it finishes or the day is out.
		rows, err := tx.db.QueryContext(ctx, `SELECT id FROM (
  SELECT id, status, started_at FROM foundry_workflow_runs WHERE workflow_id=?
  ORDER BY started_at DESC, id DESC LIMIT 18446744073709551615 OFFSET ?) old
WHERE status='finished' OR started_at < NOW(6) - INTERVAL 1 DAY`, r.WorkflowID, model.FoundryWorkflowRunsKept)
		if err != nil {
			return err
		}
		old, err := scanStrings(rows)
		if err != nil || len(old) == 0 {
			return err
		}
		// Runs before their accesses, as RecordFoundryWorkflowAccess locks them.
		lockedRuns, err := tx.db.QueryContext(ctx, `SELECT id FROM foundry_workflow_runs WHERE id IN (`+idPlaceholders(len(old))+`) FOR UPDATE`,
			toAnySlice(old)...)
		if err != nil {
			return err
		}
		if _, err := scanStrings(lockedRuns); err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM foundry_workflow_run_assessments WHERE run_id IN (` + idPlaceholders(len(old)) + `)`,
			`DELETE FROM foundry_workflow_run_accesses WHERE run_id IN (` + idPlaceholders(len(old)) + `)`,
			`DELETE FROM foundry_workflow_runs WHERE id IN (` + idPlaceholders(len(old)) + `)`,
		} {
			if _, err := tx.db.ExecContext(ctx, q, toAnySlice(old)...); err != nil {
				return fmt.Errorf("delete old workflow runs: %w", err)
			}
		}
		return nil
	})
}

// GetFoundryWorkflowRun reads one of an ontology's runs.
func (s *mysqlDB) GetFoundryWorkflowRun(ctx context.Context, ontologyID, id string) (*model.FoundryWorkflowRun, error) {
	return scanFoundryWorkflowRun(s.db.QueryRowContext(ctx, "SELECT "+foundryWorkflowRunColumns+`
FROM foundry_workflow_runs WHERE id=? AND ontology_id=?`, id, ontologyID))
}

// GetFoundryWorkflowRunInWorkspace reads one of a workspace's runs, for a
// call that isn't of an ontology (a send to an API source).
func (s *mysqlDB) GetFoundryWorkflowRunInWorkspace(ctx context.Context, workspaceID, id string) (*model.FoundryWorkflowRun, error) {
	return scanFoundryWorkflowRun(s.db.QueryRowContext(ctx, "SELECT "+foundryWorkflowRunColumns+`
FROM foundry_workflow_runs WHERE id=? AND workspace_id=?`, id, workspaceID))
}

// GetFoundryWorkflowRunByClientToken reads the run a workflow started with
// clientToken.
func (s *mysqlDB) GetFoundryWorkflowRunByClientToken(ctx context.Context, workflowID, clientToken string) (*model.FoundryWorkflowRun, error) {
	return scanFoundryWorkflowRun(s.db.QueryRowContext(ctx, "SELECT "+foundryWorkflowRunColumns+`
FROM foundry_workflow_runs WHERE workflow_id=? AND client_token=?`, workflowID, clientToken))
}

// ListFoundryWorkflowRuns lists a workflow's runs newest first, limit a page:
// those startedBy started, when it names anyone.
func (s *mysqlDB) ListFoundryWorkflowRuns(ctx context.Context, workflowID string, startedBy *access.Principal, pageToken string, limit int) ([]*model.FoundryWorkflowRun, string, error) {
	conds, args := []string{"workflow_id=?"}, []any{workflowID}
	if startedBy != nil {
		column, ok := startedByColumn(*startedBy)
		if !ok {
			return nil, "", nil
		}
		conds, args = append(conds, column+"=?"), append(args, startedBy.ID)
	}
	if pageToken != "" {
		cursor, err := decodeTimeIDCursor(pageToken)
		if err != nil {
			return nil, "", err
		}
		conds = append(conds, "(started_at < ? OR (started_at = ? AND id < ?))")
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundryWorkflowRunColumns+" FROM foundry_workflow_runs"+whereClause(conds)+
		fmt.Sprintf(" ORDER BY started_at DESC, id DESC LIMIT %d", limit+1), args...)
	if err != nil {
		return nil, "", err
	}
	out, err := scanAll(rows, scanFoundryWorkflowRun)
	if err != nil || len(out) <= limit {
		return out, "", err
	}
	next, err := encodeTimeIDCursor(out[limit-1].StartedAt, out[limit-1].ID)
	return out[:limit], next, err
}

// FinishFoundryWorkflowRun finishes an open run with output, and input when
// the run has none: one it was started with stays. With assess, its
// assessment is queued in the same transaction. ErrNotFound when no open
// run has id: it finished, or is gone.
func (s *mysqlDB) FinishFoundryWorkflowRun(ctx context.Context, id string, input *string, output string, assess bool) error {
	var in any
	if input != nil {
		in = *input
	}
	return s.inTx(ctx, func(tx *mysqlDB) error {
		if err := execOrNotFound(ctx, tx.db, `UPDATE foundry_workflow_runs
SET status='finished', finished_at=NOW(6), output=?, input=COALESCE(input, ?)
WHERE id=? AND status='open'`, output, in, id); err != nil || !assess {
			return err
		}
		_, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_workflow_run_assessments (run_id, workspace_id, ontology_id, workflow_id, status)
SELECT id, workspace_id, ontology_id, workflow_id, 'queued' FROM foundry_workflow_runs WHERE id=?`, id)
		return err
	})
}

// RecordFoundryWorkflowAccess records call as the run's next access under
// its lock, and returns it. A call with a dedupe key the run recorded
// already returns that access and records nothing. A read's result is kept
// unless it passes model.FoundryWorkflowResultMaxBytes, or the run's results
// would pass model.FoundryWorkflowRunResultMaxBytes; the access returned
// says which. ErrNotFound when the run is gone, ErrWorkflowRunFinished when
// it finished, and ErrLimitExceeded at model.FoundryWorkflowRunMaxAccesses.
func (s *mysqlDB) RecordFoundryWorkflowAccess(ctx context.Context, runID string, call model.FoundryWorkflowCall) (*model.FoundryWorkflowAccess, error) {
	detail, err := json.Marshal(call)
	if err != nil {
		return nil, err
	}
	var out *model.FoundryWorkflowAccess
	err = s.inTx(ctx, func(tx *mysqlDB) error {
		var status string
		var accesses int32
		var resultBytes int64
		err := tx.db.QueryRowContext(ctx, `SELECT status, accesses, result_bytes FROM foundry_workflow_runs WHERE id=? FOR UPDATE`, runID).
			Scan(&status, &accesses, &resultBytes)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		case status != string(model.FoundryWorkflowRunOpen):
			return ErrWorkflowRunFinished
		}
		key := call.DedupeKey()
		if key != "" {
			recorded, err := scanFoundryWorkflowAccess(tx.db.QueryRowContext(ctx, `SELECT `+foundryWorkflowAccessColumns+`
FROM foundry_workflow_run_accesses WHERE run_id=? AND dedupe_key=?`, runID, key))
			if err == nil {
				out = recorded
				return nil
			}
			if !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		if accesses >= model.FoundryWorkflowRunMaxAccesses {
			return ErrLimitExceeded
		}
		var result any
		var counts struct{ reads, questions, sends, uploads, pieces, omitted int }
		var kept int64
		switch c := call.(type) {
		case *model.FoundryWorkflowRead:
			counts.reads = 1
			if n := int64(len(c.Result)); n > 0 && n <= model.FoundryWorkflowResultMaxBytes && resultBytes+n <= model.FoundryWorkflowRunResultMaxBytes {
				result, kept = string(c.Result), n
			} else {
				counts.omitted = 1
			}
		case *model.FoundryWorkflowQuestion:
			counts.questions = 1
		case *model.FoundryWorkflowSend:
			counts.sends, counts.pieces = 1, len(c.PieceIDs)
		case *model.FoundryWorkflowUpload:
			counts.uploads = 1
		default:
			return fmt.Errorf("unknown workflow call %T", call)
		}
		seq := accesses + 1
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_workflow_run_accesses (run_id, seq, kind, dedupe_key, detail, result)
VALUES (?, ?, ?, ?, ?, ?)`, runID, seq, string(call.Kind()), nullString(key), string(detail), result); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_workflow_runs SET accesses=accesses+1, reads_count=reads_count+?,
  questions=questions+?, sends=sends+?, uploads=uploads+?, pieces=pieces+?, results_omitted=results_omitted+?, result_bytes=result_bytes+?
WHERE id=?`, counts.reads, counts.questions, counts.sends, counts.uploads, counts.pieces, counts.omitted, kept, runID); err != nil {
			return err
		}
		out, err = scanFoundryWorkflowAccess(tx.db.QueryRowContext(ctx, `SELECT `+foundryWorkflowAccessColumns+`
FROM foundry_workflow_run_accesses WHERE run_id=? AND seq=?`, runID, seq))
		return err
	})
	return out, err
}

const foundryWorkflowAccessColumns = `seq, at, kind, detail, result`

func scanFoundryWorkflowAccess(row rowScanner) (*model.FoundryWorkflowAccess, error) {
	var a model.FoundryWorkflowAccess
	var kind string
	var detail []byte
	var result sql.NullString
	err := row.Scan(&a.Seq, &a.At, &kind, &detail, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var stored []byte
	if result.Valid {
		stored = []byte(result.String)
	}
	if a.Call, err = model.ParseFoundryWorkflowCall(kind, detail, stored); err != nil {
		return nil, err
	}
	return &a, nil
}

// ListFoundryWorkflowRunAccesses returns up to limit of a run's accesses
// after afterSeq, in order, and whether more follow.
func (s *mysqlDB) ListFoundryWorkflowRunAccesses(ctx context.Context, runID string, afterSeq int32, limit int) ([]*model.FoundryWorkflowAccess, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+foundryWorkflowAccessColumns+`
FROM foundry_workflow_run_accesses WHERE run_id=? AND seq>? ORDER BY seq LIMIT ?`, runID, afterSeq, limit+1)
	if err != nil {
		return nil, false, err
	}
	out, err := scanAll(rows, scanFoundryWorkflowAccess)
	if err != nil || len(out) <= limit {
		return out, false, err
	}
	return out[:limit], true, nil
}

const foundryWorkflowAssessmentColumns = `run_id, workspace_id, ontology_id, workflow_id, status, attempts, verdict, assessment,
  left_out, failure, error, usd_micros, queued_at, finished_at`

func scanFoundryWorkflowAssessment(row rowScanner) (*model.FoundryWorkflowAssessment, error) {
	var a model.FoundryWorkflowAssessment
	var status string
	var verdict, failure, why sql.NullString
	var assessment, leftOut []byte
	var finished sql.NullTime
	err := row.Scan(&a.RunID, &a.WorkspaceID, &a.OntologyID, &a.WorkflowID, &status, &a.Attempts, &verdict, &assessment,
		&leftOut, &failure, &why, &a.USDMicros, &a.QueuedAt, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Status, a.Verdict, a.Failure, a.Error = model.FoundryWorkflowAssessmentStatus(status), verdict.String, failure.String, why.String
	a.FinishedAt = timePtr(finished)
	if len(assessment) > 0 {
		a.Assessment = json.RawMessage(assessment)
	}
	if len(leftOut) > 0 {
		if err := json.Unmarshal(leftOut, &a.LeftOut); err != nil {
			return nil, fmt.Errorf("assessment of %s: left_out: %w", a.RunID, err)
		}
	}
	return &a, nil
}

// QueueFoundryWorkflowRunAssessment queues a finished run's assessment, again
// if it had one, keeping what earlier attempts spent. ErrNotFound when the
// run is gone or still open, ErrConflict while its assessment is queued or
// running.
func (s *mysqlDB) QueueFoundryWorkflowRunAssessment(ctx context.Context, runID string) error {
	return s.inTx(ctx, func(tx *mysqlDB) error {
		var status string
		err := tx.db.QueryRowContext(ctx, `SELECT status FROM foundry_workflow_runs WHERE id=? FOR UPDATE`, runID).Scan(&status)
		switch {
		case errors.Is(err, sql.ErrNoRows) || err == nil && status != string(model.FoundryWorkflowRunFinished):
			return ErrNotFound
		case err != nil:
			return err
		}
		var was string
		switch err := tx.db.QueryRowContext(ctx, `SELECT status FROM foundry_workflow_run_assessments WHERE run_id=? FOR UPDATE`, runID).Scan(&was); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case model.FoundryWorkflowAssessmentStatus(was).Live():
			return ErrConflict
		}
		_, err = tx.db.ExecContext(ctx, `INSERT INTO foundry_workflow_run_assessments (run_id, workspace_id, ontology_id, workflow_id, status)
SELECT id, workspace_id, ontology_id, workflow_id, 'queued' FROM foundry_workflow_runs WHERE id=?
ON DUPLICATE KEY UPDATE status='queued', attempts=0, lease_owner=NULL, lease_until=NULL, next_attempt_at=NOW(6), verdict=NULL,
  assessment=NULL, left_out=NULL, failure=NULL, error=NULL, queued_at=NOW(6), finished_at=NULL`, runID)
		return err
	})
}

// ClaimFoundryWorkflowRunAssessment leases the next due assessment to owner
// for lease: a queued one whose time has come, or a running one whose lease
// lapsed, each attempt counted. ErrNotFound when none is due.
func (s *mysqlDB) ClaimFoundryWorkflowRunAssessment(ctx context.Context, owner string, lease time.Duration) (*model.FoundryWorkflowAssessment, error) {
	if owner == "" || lease <= 0 {
		return nil, fmt.Errorf("a claim needs an owner and a lease")
	}
	var out *model.FoundryWorkflowAssessment
	err := s.inTx(ctx, func(tx *mysqlDB) error {
		var runID string
		err := tx.db.QueryRowContext(ctx, `SELECT run_id FROM foundry_workflow_run_assessments
WHERE (status='queued' AND next_attempt_at <= NOW(6)) OR (status='running' AND lease_until < NOW(6))
ORDER BY next_attempt_at, run_id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&runID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_workflow_run_assessments
SET status='running', attempts=attempts+1, lease_owner=?, lease_until=NOW(6) + INTERVAL ? MICROSECOND WHERE run_id=?`,
			owner, lease.Microseconds(), runID); err != nil {
			return err
		}
		out, err = scanFoundryWorkflowAssessment(tx.db.QueryRowContext(ctx, `SELECT `+foundryWorkflowAssessmentColumns+`
FROM foundry_workflow_run_assessments WHERE run_id=?`, runID))
		return err
	})
	return out, err
}

// RenewFoundryWorkflowRunAssessment extends owner's lease. ErrNotFound when
// owner no longer holds it.
func (s *mysqlDB) RenewFoundryWorkflowRunAssessment(ctx context.Context, runID, owner string, lease time.Duration) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_workflow_run_assessments SET lease_until=NOW(6) + INTERVAL ? MICROSECOND
WHERE run_id=? AND status='running' AND lease_owner=?`, lease.Microseconds(), runID, owner)
}

// FinishFoundryWorkflowRunAssessment ends owner's attempt as o says, adding
// what it spent to the assessment's spend, charged now. ErrNotFound when
// owner no longer holds it: what it spent is then not counted, as a lost
// attempt's isn't.
func (s *mysqlDB) FinishFoundryWorkflowRunAssessment(ctx context.Context, runID, owner string, o model.FoundryWorkflowAssessmentOutcome) error {
	if err := o.Check(); err != nil {
		return err
	}
	var leftOut, assessment, verdict, failure, why, finished any
	if o.Status == model.FoundryWorkflowAssessmentDone {
		raw, err := json.Marshal(o.LeftOut)
		if err != nil {
			return err
		}
		leftOut, assessment, verdict = string(raw), string(o.Assessment), o.Verdict
	}
	if o.Status != model.FoundryWorkflowAssessmentDone {
		why = o.Error
	}
	if o.Failure != "" {
		failure = o.Failure
	}
	next := time.Now().UTC()
	if o.Status == model.FoundryWorkflowAssessmentQueued {
		next = o.RetryAt.UTC()
	} else {
		finished = next
	}
	uncounted := 0
	if o.Uncounted {
		uncounted = 1
	}
	return s.inTx(ctx, func(tx *mysqlDB) error {
		var workspaceID string
		err := tx.db.QueryRowContext(ctx, `SELECT workspace_id FROM foundry_workflow_run_assessments
WHERE run_id=? AND status='running' AND lease_owner=? FOR UPDATE`, runID, owner).Scan(&workspaceID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_workflow_run_assessments
SET status=?, lease_owner=NULL, lease_until=NULL, verdict=?, assessment=?, left_out=?, failure=?, error=?,
  usd_micros=usd_micros+?, attempts=GREATEST(attempts-?, 0), finished_at=?, next_attempt_at=?
WHERE run_id=?`, string(o.Status), verdict, assessment, leftOut, failure, why, o.USDMicros, uncounted, finished, next, runID); err != nil {
			return err
		}
		if o.USDMicros == 0 {
			return nil
		}
		_, err = tx.db.ExecContext(ctx, `INSERT INTO foundry_workflow_assessment_spend (workspace_id, month, usd_micros)
VALUES (?, ?, ?) ON DUPLICATE KEY UPDATE usd_micros=usd_micros+VALUES(usd_micros)`, workspaceID, monthOf(time.Now()), o.USDMicros)
		return err
	})
}

// monthOf is the first day of t's UTC month, as the spend ledger keys it.
func monthOf(t time.Time) string {
	return t.UTC().Format("2006-01") + "-01"
}

// GetFoundryWorkflowRunAssessment reads a run's assessment: ErrNotFound when
// it has none.
func (s *mysqlDB) GetFoundryWorkflowRunAssessment(ctx context.Context, runID string) (*model.FoundryWorkflowAssessment, error) {
	return scanFoundryWorkflowAssessment(s.db.QueryRowContext(ctx, `SELECT `+foundryWorkflowAssessmentColumns+`
FROM foundry_workflow_run_assessments WHERE run_id=?`, runID))
}

// ListFoundryWorkflowRunAssessments reads the assessments of the runs named,
// by run id; a run without one is absent.
func (s *mysqlDB) ListFoundryWorkflowRunAssessments(ctx context.Context, runIDs []string) (map[string]*model.FoundryWorkflowAssessment, error) {
	out := map[string]*model.FoundryWorkflowAssessment{}
	if len(runIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+foundryWorkflowAssessmentColumns+`
FROM foundry_workflow_run_assessments WHERE run_id IN (`+idPlaceholders(len(runIDs))+`)`, toAnySlice(runIDs)...)
	if err != nil {
		return nil, err
	}
	all, err := scanAll(rows, scanFoundryWorkflowAssessment)
	for _, a := range all {
		out[a.RunID] = a
	}
	return out, err
}

// CountFoundryWorkflowVerdicts counts the done assessments of each workflow
// named, by verdict.
func (s *mysqlDB) CountFoundryWorkflowVerdicts(ctx context.Context, workflowIDs []string) (map[string]map[string]int64, error) {
	out := map[string]map[string]int64{}
	if len(workflowIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT workflow_id, verdict, COUNT(*) FROM foundry_workflow_run_assessments
WHERE status='done' AND workflow_id IN (`+idPlaceholders(len(workflowIDs))+`) GROUP BY workflow_id, verdict`, toAnySlice(workflowIDs)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var workflow, verdict string
		var n int64
		if err := rows.Scan(&workflow, &verdict, &n); err != nil {
			return nil, err
		}
		if out[workflow] == nil {
			out[workflow] = map[string]int64{}
		}
		out[workflow][verdict] = n
	}
	return out, rows.Err()
}

// startedByColumn is the column of foundry_workflow_runs naming who started a
// run of p's kind: a user or a service credential. A team starts no run, and
// has none (false).
func startedByColumn(p access.Principal) (string, bool) {
	switch p.Kind {
	case access.PrincipalUser:
		return "started_by_user_id", true
	case access.PrincipalServiceCredential:
		return "started_by_service_credential_id", true
	}
	return "", false
}

// TallyFoundryWorkflowRuns counts the runs by of each of workflowIDs, as
// GetFoundryWorkflow and CountFoundryWorkflowVerdicts count all of a
// workflow's: every workflow has a tally, none for one by never ran.
func (s *mysqlDB) TallyFoundryWorkflowRuns(ctx context.Context, workflowIDs []string, by access.Principal) (map[string]model.FoundryWorkflowRunTally, error) {
	out := map[string]model.FoundryWorkflowRunTally{}
	if len(workflowIDs) == 0 {
		return out, nil
	}
	for _, w := range workflowIDs {
		out[w] = model.FoundryWorkflowRunTally{Verdicts: map[string]int64{}}
	}
	column, ok := startedByColumn(by)
	if !ok {
		return out, nil
	}
	args := append(toAnySlice(workflowIDs), by.ID)
	rows, err := s.db.QueryContext(ctx, `SELECT workflow_id, COUNT(*), SUM(status = 'finished'), MAX(started_at) FROM foundry_workflow_runs
WHERE workflow_id IN (`+idPlaceholders(len(workflowIDs))+`) AND `+column+`=? GROUP BY workflow_id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var workflow string
		var last sql.NullTime
		t := model.FoundryWorkflowRunTally{}
		if err := rows.Scan(&workflow, &t.Runs, &t.FinishedRuns, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			t.LastRunAt = &last.Time
		}
		t.Verdicts = out[workflow].Verdicts
		out[workflow] = t
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	verdicts, err := s.db.QueryContext(ctx, `SELECT a.workflow_id, a.verdict, COUNT(*) FROM foundry_workflow_run_assessments a
JOIN foundry_workflow_runs r ON r.id = a.run_id
WHERE a.status='done' AND a.workflow_id IN (`+idPlaceholders(len(workflowIDs))+`) AND r.`+column+`=? GROUP BY a.workflow_id, a.verdict`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = verdicts.Close() }()
	for verdicts.Next() {
		var workflow, verdict string
		var n int64
		if err := verdicts.Scan(&workflow, &verdict, &n); err != nil {
			return nil, err
		}
		out[workflow].Verdicts[verdict] = n
	}
	return out, verdicts.Err()
}

// SumFoundryWorkflowAssessmentSpend is what a workspace's assessments
// charged in the UTC months from since's on, from the ledger, which keeps
// the spend of runs, workflows and ontologies deleted since.
func (s *mysqlDB) SumFoundryWorkflowAssessmentSpend(ctx context.Context, workspaceID string, since time.Time) (int64, error) {
	var spent int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(usd_micros), 0) FROM foundry_workflow_assessment_spend
WHERE workspace_id=? AND month >= ?`, workspaceID, monthOf(since)).Scan(&spent)
	return spent, err
}
