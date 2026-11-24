package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Company research for a new ontology's goal
// (migrations/20261224000000_foundry_company_research.sql): a row per model
// call, started before it under the caller's hourly cap and the workspace's
// monthly budget, and finished with what it spent.

// ErrBudgetSpent means a call's worst case would take a workspace past its
// monthly budget.
var ErrBudgetSpent = errors.New("store: the month's budget would be spent")

// StartFoundryCompanyResearch records a call about to be made, its result
// none and its spend nothing yet, in one transaction holding the
// workspace's lock, so concurrent calls count each other: ErrLimitExceeded
// when the caller made limits.PerUserPerHour calls of either kind in the
// past hour, else ErrBudgetSpent when the workspace's spend since
// limits.Since and limits.WorstUSDMicros would pass limits.MonthlyMicros;
// ErrNotFound when the workspace is gone.
func (s *mysqlDB) StartFoundryCompanyResearch(ctx context.Context, r *model.FoundryCompanyResearch, limits model.FoundryCompanyResearchLimits) error {
	switch {
	case r.ID == "" || r.WorkspaceID == "" || r.UserID == "" || r.Domain == "":
		return fmt.Errorf("a company research is a person's, of a workspace and a domain")
	case r.Kind != model.FoundryCompanyResearchOfSite && r.Kind != model.FoundryCompanyResearchGoal:
		return fmt.Errorf("unknown company research kind %q", r.Kind)
	case limits.PerUserPerHour < 1 || limits.MonthlyMicros < 1 || limits.WorstUSDMicros < 0:
		return fmt.Errorf("company research's limits must be positive")
	}
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.workspaces.Lock(ctx, tx.db, r.WorkspaceID); err != nil {
			return err
		}
		var lastHour int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_company_research
WHERE user_id=? AND created_at >= NOW(6) - INTERVAL 1 HOUR`, r.UserID).Scan(&lastHour); err != nil {
			return err
		}
		if lastHour >= limits.PerUserPerHour {
			return ErrLimitExceeded
		}
		var spent int64
		if err := tx.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(usd_micros), 0) FROM foundry_company_research
WHERE workspace_id=? AND created_at >= ?`, r.WorkspaceID, limits.Since.UTC()).Scan(&spent); err != nil {
			return err
		}
		if spent+limits.WorstUSDMicros > limits.MonthlyMicros {
			return ErrBudgetSpent
		}
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_company_research (id, workspace_id, user_id, kind, domain)
VALUES (?, ?, ?, ?, ?)`, r.ID, r.WorkspaceID, r.UserID, string(r.Kind), r.Domain); err != nil {
			return err
		}
		return tx.db.QueryRowContext(ctx, `SELECT created_at FROM foundry_company_research WHERE id=?`, r.ID).Scan(&r.CreatedAt)
	})
}

// FinishFoundryCompanyResearch records what a started call spent and, for
// a research that ended with one, its result (nil for a goal or a
// failure). ErrNotFound means no started call of the workspace has the id.
func (s *mysqlDB) FinishFoundryCompanyResearch(ctx context.Context, workspaceID, id string, result json.RawMessage, usdMicros int64) error {
	if usdMicros < 0 {
		return fmt.Errorf("a company research spends nothing or more")
	}
	var stored any
	if result != nil {
		stored = string(result)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE foundry_company_research SET result=CAST(? AS JSON), usd_micros=?
WHERE workspace_id=? AND id=?`, stored, usdMicros, workspaceID, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		// An update that changes nothing (the same spend again) affects no
		// row: the row is there if it reads.
		var found int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_company_research WHERE workspace_id=? AND id=?`, workspaceID, id).Scan(&found); err != nil {
			return err
		}
		if found == 0 {
			return ErrNotFound
		}
	}
	return nil
}

// GetFoundryCompanyResearch returns a workspace's research that ended with
// one; ErrNotFound for any other id, another workspace's, a goal's, or a
// research that failed or hasn't ended.
func (s *mysqlDB) GetFoundryCompanyResearch(ctx context.Context, workspaceID, id string) (*model.FoundryCompanyResearch, error) {
	var out model.FoundryCompanyResearch
	var kind string
	var result []byte
	err := s.db.QueryRowContext(ctx, `SELECT id, workspace_id, user_id, kind, domain, result, usd_micros, created_at FROM foundry_company_research
WHERE workspace_id=? AND id=? AND kind='research' AND result IS NOT NULL`, workspaceID, id).
		Scan(&out.ID, &out.WorkspaceID, &out.UserID, &kind, &out.Domain, &result, &out.USDMicros, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	out.Kind, out.Result = model.FoundryCompanyResearchKind(kind), result
	return &out, nil
}
