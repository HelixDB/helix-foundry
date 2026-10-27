package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"time"
)

const ontologyGenerationColumns = `id, workspace_id, ontology_id, status, goal, title, input, lease_owner, lease_until, attempts, next_attempt_at,
       error, result_version_id, model, input_tokens, output_tokens, requested_by_user_id, created_at, started_at, finished_at, revise_result`

// EnqueueOntologyGeneration queues a generation in one transaction that
// holds its workspace's lock, then its ontology's (in that order, as every
// writer takes them).
//
// The workspace lock enforces the hourly cap, so concurrent requests can't
// pass it together. Every generation started in the last hour counts,
// whatever became of it. A revision (Input.Revise) runs the engine, not the
// model, so it neither counts nor is capped, and skips that lock.
//
// The ontology lock orders it against a delete: DeleteFoundryOntology either
// ends it (it fails the live generation) or went first and refuses it. A
// generation of a deleted ontology could never store its draft, and workers
// would claim it forever.
//
// ErrLimitExceeded means the workspace has started perHour generations;
// ErrConflict that the ontology already has a live one
// (GetActiveOntologyGeneration returns it); ErrNotFound that the workspace or
// the ontology is gone (or the ontology is another workspace's).
func (s *mysqlDB) EnqueueOntologyGeneration(ctx context.Context, gen *model.OntologyGeneration, perHour int) error {
	if perHour < 1 {
		return fmt.Errorf("ontology generations per hour must be positive")
	}
	input, err := json.Marshal(gen.Input)
	if err != nil {
		return fmt.Errorf("encode ontology generation input: %w", err)
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if gen.Input.Revise == nil {
			if err := tx.workspaces.Lock(ctx, tx.db, gen.WorkspaceID); err != nil {
				return err
			}
			var lastHour int
			if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_ontology_generations
WHERE workspace_id=? AND created_at >= NOW(6) - INTERVAL 1 HOUR AND JSON_EXTRACT(input, '$.revise') IS NULL`, gen.WorkspaceID).Scan(&lastHour); err != nil {
				return err
			}
			if lastHour >= perHour {
				return ErrLimitExceeded
			}
		}
		if _, err := tx.lockLiveFoundryOntology(ctx, gen.WorkspaceID, gen.OntologyID); err != nil {
			return err
		}
		_, err := tx.db.ExecContext(ctx, `
INSERT INTO foundry_ontology_generations (id, workspace_id, ontology_id, status, active_key, goal, title, input, requested_by_user_id)
VALUES (?, ?, ?, 'queued', 1, ?, ?, CAST(? AS JSON), ?)`, gen.ID, gen.WorkspaceID, gen.OntologyID, gen.Goal, gen.Title, string(input), gen.RequestedByUserID)
		if isDuplicateKey(err) {
			return ErrConflict
		}
		return err
	})
}

// SetOntologyGenerationTitle titles a queued generation. GenerateOntology
// titles one only after EnqueueOntologyGeneration admits it, so a refused
// request never pays for a title. ErrNotFound means no such generation.
func (s *mysqlDB) SetOntologyGenerationTitle(ctx context.Context, id, title string) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_ontology_generations SET title=? WHERE id=?`, title, id)
}

func (s *mysqlDB) GetActiveOntologyGeneration(ctx context.Context, ontologyID string) (*model.OntologyGeneration, error) {
	return scanOntologyGeneration(s.db.QueryRowContext(ctx, "SELECT "+ontologyGenerationColumns+`
FROM foundry_ontology_generations WHERE ontology_id=? AND active_key=1`, ontologyID))
}

func (s *mysqlDB) GetOntologyGeneration(ctx context.Context, ontologyID, id string) (*model.OntologyGeneration, error) {
	return scanOntologyGeneration(s.db.QueryRowContext(ctx, "SELECT "+ontologyGenerationColumns+`
FROM foundry_ontology_generations WHERE id=? AND ontology_id=?`, id, ontologyID))
}

// ontologyGenerationListColumns are ontologyGenerationColumns without what
// can run to megabytes: a revision's edits (up to 1 MiB of TOML) and its
// result (up to 3 MiB of cards). Lists poll, a hundred generations at a time,
// so they stay small; GetOntologyGeneration has both.
const ontologyGenerationListColumns = `id, workspace_id, ontology_id, status, goal, title, JSON_REMOVE(input, '$.revise.edits'), lease_owner,
       lease_until, attempts, next_attempt_at, error, result_version_id, model, input_tokens, output_tokens, requested_by_user_id,
       created_at, started_at, finished_at, NULL`

// ListOntologyGenerations returns an ontology's generations, newest first,
// without a revision's edits or result (ontologyGenerationListColumns).
func (s *mysqlDB) ListOntologyGenerations(ctx context.Context, ontologyID string, limit int) ([]*model.OntologyGeneration, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+ontologyGenerationListColumns+`
FROM foundry_ontology_generations WHERE ontology_id=? ORDER BY created_at DESC, id DESC LIMIT ?`, ontologyID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.OntologyGeneration
	for rows.Next() {
		gen, err := scanOntologyGeneration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, gen)
	}
	return out, rows.Err()
}

// ClaimOntologyGeneration leases the oldest due generation to workerID: a
// queued one whose retry time has come, or a running one whose lease expired.
// Nil means none is due.
func (s *mysqlDB) ClaimOntologyGeneration(ctx context.Context, workerID string, lease time.Duration) (_ *model.OntologyGeneration, err error) {
	if s.root == nil {
		return nil, fmt.Errorf("ontology generation claim requires a root database connection")
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin ontology generation claim: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	gen, err := scanOntologyGeneration(tx.QueryRowContext(ctx, "SELECT "+ontologyGenerationColumns+`
FROM foundry_ontology_generations
WHERE (status='queued' AND next_attempt_at <= NOW(6)) OR (status='running' AND lease_until < NOW(6))
ORDER BY next_attempt_at, id
LIMIT 1 FOR UPDATE SKIP LOCKED`))
	if err == ErrNotFound {
		_ = tx.Rollback()
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select ontology generation claim: %w", err)
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(lease)
	if _, err = tx.ExecContext(ctx, `
UPDATE foundry_ontology_generations
SET status='running', attempts=attempts+1, lease_owner=?, lease_until=?, started_at=COALESCE(started_at, NOW(6))
WHERE id=?`, workerID, leaseUntil, gen.ID); err != nil {
		return nil, fmt.Errorf("claim ontology generation: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit ontology generation claim: %w", err)
	}
	gen.Status, gen.LeaseOwner, gen.LeaseUntil = model.OntologyGenerationRunning, &workerID, &leaseUntil
	gen.Attempts++
	if gen.StartedAt == nil {
		gen.StartedAt = &now
	}
	return gen, nil
}

// RenewOntologyGenerationLease extends the lease; ErrNotFound means workerID lost it.
func (s *mysqlDB) RenewOntologyGenerationLease(ctx context.Context, id, workerID string, lease time.Duration) error {
	return execOrNotFound(ctx, s.db, `UPDATE foundry_ontology_generations SET lease_until=?
WHERE id=? AND status='running' AND lease_owner=?`, time.Now().UTC().Add(lease), id, workerID)
}

// FinishOntologyGeneration ends workerID's attempt: requeued for RetryAt, or
// terminal and no longer live. A success stores its draft version in the same
// transaction. Revision previews succeed without a version; their findings
// and cards, and those of refused edits, are kept in ReviseResult.
// ErrNotFound means the lease was lost and nothing was stored, as when the
// ontology was deleted meanwhile (DeleteFoundryOntology ends its generation).
func (s *mysqlDB) FinishOntologyGeneration(ctx context.Context, id, workerID string, outcome model.OntologyGenerationOutcome) (err error) {
	const owned = ` WHERE id=? AND status='running' AND lease_owner=?`
	usage := `model=COALESCE(NULLIF(?, ''), model), model_provider=COALESCE(NULLIF(?, ''), model_provider),
    input_tokens=COALESCE(input_tokens, 0)+?, output_tokens=COALESCE(output_tokens, 0)+?`
	if outcome.RetryAt != nil {
		return execOrNotFound(ctx, s.db, `UPDATE foundry_ontology_generations
SET status='queued', next_attempt_at=?, error=NULLIF(?, ''), lease_owner=NULL, lease_until=NULL, `+usage+owned,
			outcome.RetryAt.UTC(), outcome.Error, outcome.Model, outcome.ModelProvider, outcome.InputTokens, outcome.OutputTokens, id, workerID)
	}
	var report any
	if len(outcome.ReviseResult) > 0 {
		report = string(outcome.ReviseResult)
	}
	switch {
	case outcome.Status == model.OntologyGenerationSucceeded && outcome.Result == nil:
		// Only an actual preview may succeed without a version.
		return execOrNotFound(ctx, s.db, `UPDATE foundry_ontology_generations
SET status='succeeded', error=NULL, revise_result=CAST(? AS JSON), active_key=NULL, lease_owner=NULL, lease_until=NULL, finished_at=NOW(6), `+usage+owned+` AND JSON_EXTRACT(input, '$.revise.dry_run') = true`,
			report, outcome.Model, outcome.ModelProvider, outcome.InputTokens, outcome.OutputTokens, id, workerID)
	case outcome.Status == model.OntologyGenerationFailed:
		return execOrNotFound(ctx, s.db, `UPDATE foundry_ontology_generations
SET status='failed', error=NULLIF(?, ''), revise_result=CAST(? AS JSON), active_key=NULL, lease_owner=NULL, lease_until=NULL, finished_at=NOW(6), `+usage+owned,
			outcome.Error, report, outcome.Model, outcome.ModelProvider, outcome.InputTokens, outcome.OutputTokens, id, workerID)
	case outcome.Status != model.OntologyGenerationSucceeded || outcome.Result == nil:
		return fmt.Errorf("ontology generation outcome %q is not terminal or has no result", outcome.Status)
	}
	if s.root == nil {
		return fmt.Errorf("ontology generation result requires a root database connection")
	}
	tx, err := s.root.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ontology generation result: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	// The ontology first, as DeleteFoundryOntology locks it before ending its
	// generation: the other order could deadlock with a delete.
	var locked string
	if err = tx.QueryRowContext(ctx, `SELECT id FROM foundry_ontologies WHERE id=? FOR UPDATE`, outcome.Result.OntologyID).Scan(&locked); err != nil {
		if err == sql.ErrNoRows {
			err = ErrNotFound
		}
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE foundry_ontology_generations
SET status='succeeded', error=NULL, result_version_id=?, revise_result=CAST(? AS JSON), active_key=NULL, lease_owner=NULL, lease_until=NULL,
    finished_at=NOW(6), `+usage+owned,
		outcome.Result.ID, report, outcome.Model, outcome.ModelProvider, outcome.InputTokens, outcome.OutputTokens, id, workerID)
	if err != nil {
		return fmt.Errorf("finish ontology generation: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		_ = tx.Rollback()
		return ErrNotFound
	}
	if err = insertOntologyVersion(ctx, tx, outcome.Result); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit ontology generation result: %w", err)
	}
	return nil
}

func scanOntologyGeneration(scanner rowScanner) (*model.OntologyGeneration, error) {
	var gen model.OntologyGeneration
	var status, input string
	var reviseResult sql.NullString
	var goal, title, leaseOwner, errText, resultVersion, modelName, requestedBy sql.NullString
	var leaseUntil, startedAt, finishedAt sql.NullTime
	var inputTokens, outputTokens sql.NullInt64
	if err := scanner.Scan(&gen.ID, &gen.WorkspaceID, &gen.OntologyID, &status, &goal, &title, &input, &leaseOwner, &leaseUntil, &gen.Attempts,
		&gen.NextAttemptAt, &errText, &resultVersion, &modelName, &inputTokens, &outputTokens, &requestedBy,
		&gen.CreatedAt, &startedAt, &finishedAt, &reviseResult,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if reviseResult.Valid {
		gen.ReviseResult = json.RawMessage(reviseResult.String)
	}
	gen.Status = model.OntologyGenerationStatus(status)
	if err := json.Unmarshal([]byte(input), &gen.Input); err != nil {
		return nil, fmt.Errorf("decode ontology generation %s input: %w", gen.ID, err)
	}
	for _, f := range []struct {
		src sql.NullString
		dst **string
	}{{goal, &gen.Goal}, {title, &gen.Title}, {leaseOwner, &gen.LeaseOwner}, {errText, &gen.Error}, {resultVersion, &gen.ResultVersionID},
		{modelName, &gen.Model}, {requestedBy, &gen.RequestedByUserID}} {
		if f.src.Valid {
			*f.dst = &f.src.String
		}
	}
	for _, f := range []struct {
		src sql.NullTime
		dst **time.Time
	}{{leaseUntil, &gen.LeaseUntil}, {startedAt, &gen.StartedAt}, {finishedAt, &gen.FinishedAt}} {
		if f.src.Valid {
			*f.dst = &f.src.Time
		}
	}
	if inputTokens.Valid {
		n := int(inputTokens.Int64)
		gen.InputTokens = &n
	}
	if outputTokens.Valid {
		n := int(outputTokens.Int64)
		gen.OutputTokens = &n
	}
	return &gen, nil
}
