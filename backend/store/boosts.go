package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// boostHolders is, per kind of holder, the lock on the attempt that
// holds a boost: its work's id, worker and attempt.
var boostHolders = map[model.FoundryBoostHolder]string{
	model.FoundryBoostLoad:     `SELECT 1 FROM foundry_load_jobs` + ownedFoundryGraphJob + ` AND attempts=? FOR UPDATE`,
	model.FoundryBoostQuestion: `SELECT 1 FROM foundry_questions` + ownedFoundryQuestion + ` AND attempts=? FOR UPDATE`,
	// A revision is a generation with a revise input, of a live ontology.
	model.FoundryBoostRevision: `SELECT 1 FROM foundry_ontology_generations g
JOIN foundry_ontologies o ON o.id=g.ontology_id AND o.workspace_id=g.workspace_id AND o.deleted_at IS NULL
WHERE g.id=? AND g.status='running' AND g.lease_owner=? AND g.attempts=? AND g.lease_until>NOW(6)
 AND JSON_EXTRACT(g.input, '$.revise') IS NOT NULL FOR UPDATE`,
}

// LockBoostHolder locks the work whose attempt holds a rate-limit boost, in
// q's transaction, the host's write of the boost: workerID must hold the
// work's lease on attempt, a revision's ontology must be live. ErrNotFound
// when it does not.
func LockBoostHolder(ctx context.Context, q Executor, holder model.FoundryBoostHolder, id, workerID string, attempt int) error {
	lock, ok := boostHolders[holder]
	if !ok {
		return fmt.Errorf("invalid foundry load boost holder %q", holder)
	}
	var held int
	err := q.QueryRowContext(ctx, lock, id, workerID, attempt).Scan(&held)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
