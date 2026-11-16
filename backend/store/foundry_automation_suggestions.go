package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Suggestions for a new automation
// (migrations/20261204000000_foundry_automation_suggestions.sql): one row per
// ontology version, queued by a read, claimed and leased by the Assistant's
// worker, and ended ready or failed while it holds the lease. Their queues
// and spend are in foundry_automation_suggestion_ledger, which nothing
// deletes but the workspace's end.

const foundryAutomationSuggestionsColumns = `ontology_id, version_id, workspace_id, status, questions, suggestions, failure, attempts,
       lease_owner, lease_until, requested_at, updated_at`

// dueFoundryAutomationSuggestions is a row a claim may take: queued, or
// running under a lease that ran out.
const dueFoundryAutomationSuggestions = `(status='queued' OR (status='running' AND lease_until < NOW(6)))`

// foundryAutomationSuggestionsSpent is what automation suggestions were
// charged since a time. Its arguments are the workspace and the time.
const foundryAutomationSuggestionsSpent = `(SELECT COALESCE(SUM(usd_micros), 0) FROM foundry_automation_suggestion_ledger
   WHERE workspace_id=? AND kind='charged' AND at >= ?)`

// foundryAssistantSettled is what the Assistant's people's pool settled
// since a time: people's turns (an automation's turns are its own pool's)
// and automation suggestions. Its arguments are the workspace and the time,
// twice.
const foundryAssistantSettled = `(SELECT COALESCE(SUM(usd_micros), 0) FROM foundry_agent_turns
   WHERE workspace_id=? AND charged_at >= ? AND automation_run_id IS NULL)
 + ` + foundryAutomationSuggestionsSpent

// GetFoundryAutomationSuggestions returns a version's suggestions row;
// ErrNotFound when none was queued.
func (s *mysqlDB) GetFoundryAutomationSuggestions(ctx context.Context, ontologyID, versionID string) (*model.FoundryAutomationSuggestions, error) {
	return scanFoundryAutomationSuggestions(s.db.QueryRowContext(ctx, "SELECT "+foundryAutomationSuggestionsColumns+`
FROM foundry_automation_suggestions WHERE ontology_id=? AND version_id=?`, ontologyID, versionID))
}

// QueueFoundryAutomationSuggestions queues the suggestions of the
// workspace's ontology version, with its competency questions, in one
// transaction holding the workspace's lock, so concurrent first reads queue
// once:
//   - a row that isn't failed, or failed less than retryAfter ago, is
//     returned as it is;
//   - else, with perHour queues of the workspace in the last hour (the
//     ledger's, so deleting an ontology doesn't reset it),
//     ErrLimitExceeded;
//   - else the row is made, or the failed one reset, queued with no
//     attempts, and the queue is entered in the ledger.
func (s *mysqlDB) QueueFoundryAutomationSuggestions(ctx context.Context, workspaceID, ontologyID, versionID string, questions []string,
	perHour int, retryAfter time.Duration) (*model.FoundryAutomationSuggestions, error) {
	switch {
	case workspaceID == "" || ontologyID == "" || versionID == "":
		return nil, fmt.Errorf("automation suggestions are queued for a workspace's ontology version")
	case perHour < 1 || retryAfter <= 0:
		return nil, fmt.Errorf("automation suggestions' limits must be positive")
	}
	if questions == nil {
		questions = []string{}
	}
	asked, err := json.Marshal(questions)
	if err != nil {
		return nil, err
	}
	var out *model.FoundryAutomationSuggestions
	err = s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.workspaces.Lock(ctx, tx.db, workspaceID); err != nil {
			return err
		}
		var status string
		var retry bool
		err := tx.db.QueryRowContext(ctx, `SELECT status, status='failed' AND updated_at < NOW(6) - INTERVAL ? MICROSECOND
FROM foundry_automation_suggestions WHERE ontology_id=? AND version_id=? FOR UPDATE`, retryAfter.Microseconds(), ontologyID, versionID).Scan(&status, &retry)
		exists := err == nil
		switch {
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return err
		case exists && !retry:
			out, err = tx.GetFoundryAutomationSuggestions(ctx, ontologyID, versionID)
			return err
		}
		var lastHour int
		if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_automation_suggestion_ledger
WHERE workspace_id=? AND kind='queued' AND at >= NOW(6) - INTERVAL 1 HOUR`, workspaceID).Scan(&lastHour); err != nil {
			return err
		}
		if lastHour >= perHour {
			return ErrLimitExceeded
		}
		if exists {
			_, err = tx.db.ExecContext(ctx, `UPDATE foundry_automation_suggestions
SET status='queued', questions=?, failure=NULL, attempts=0, lease_owner=NULL, lease_until=NULL, requested_at=NOW(6), updated_at=NOW(6)
WHERE ontology_id=? AND version_id=?`, string(asked), ontologyID, versionID)
		} else {
			_, err = tx.db.ExecContext(ctx, `INSERT INTO foundry_automation_suggestions (ontology_id, version_id, workspace_id, status, questions)
VALUES (?, ?, ?, 'queued', ?)`, ontologyID, versionID, workspaceID, string(asked))
		}
		if err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_automation_suggestion_ledger (id, workspace_id, kind)
VALUES (?, ?, 'queued')`, id.New(), workspaceID); err != nil {
			return err
		}
		out, err = tx.GetFoundryAutomationSuggestions(ctx, ontologyID, versionID)
		return err
	})
	return out, err
}

// ClaimFoundryAutomationSuggestions leases the oldest due row to workerID:
// running, one more attempt. A row whose lease ran out is due again, its
// attempt counted; the worker ends one past its attempts. Nil means none is
// due.
func (s *mysqlDB) ClaimFoundryAutomationSuggestions(ctx context.Context, workerID string, lease time.Duration) (*model.FoundryAutomationSuggestions, error) {
	if workerID == "" || lease <= 0 {
		return nil, fmt.Errorf("invalid automation suggestions claim")
	}
	var out *model.FoundryAutomationSuggestions
	err := s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		var ontologyID, versionID string
		err := tx.db.QueryRowContext(ctx, `SELECT ontology_id, version_id FROM foundry_automation_suggestions
WHERE `+dueFoundryAutomationSuggestions+` ORDER BY requested_at, ontology_id, version_id LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&ontologyID, &versionID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_automation_suggestions
SET status='running', attempts=attempts+1, lease_owner=?, lease_until=?, updated_at=NOW(6)
WHERE ontology_id=? AND version_id=?`, workerID, time.Now().UTC().Add(lease), ontologyID, versionID); err != nil {
			return err
		}
		out, err = tx.GetFoundryAutomationSuggestions(ctx, ontologyID, versionID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("claim automation suggestions: %w", err)
	}
	return out, nil
}

// RenewFoundryAutomationSuggestionsLease moves the lease workerID holds to
// lease from now, for a write that may take longer than its claim's lease.
// ErrNotFound means workerID lost it.
func (s *mysqlDB) RenewFoundryAutomationSuggestionsLease(ctx context.Context, ontologyID, versionID, workerID string, lease time.Duration) error {
	if lease <= 0 {
		return fmt.Errorf("invalid automation suggestions lease")
	}
	return execOrNotFound(ctx, s.db, `UPDATE foundry_automation_suggestions SET lease_until=?, updated_at=NOW(6)
WHERE ontology_id=? AND version_id=? AND status='running' AND lease_owner=?`, time.Now().UTC().Add(lease), ontologyID, versionID, workerID)
}

// FinishFoundryAutomationSuggestions ends the write workerID holds the lease
// of, as end says. ErrNotFound means workerID lost the lease (another
// worker took it, or the row went with its ontology): nothing is recorded.
func (s *mysqlDB) FinishFoundryAutomationSuggestions(ctx context.Context, ontologyID, versionID, workerID string,
	end model.FoundryAutomationSuggestionsEnd) error {
	if err := end.Validate(); err != nil {
		return err
	}
	return execOrNotFound(ctx, s.db, `UPDATE foundry_automation_suggestions
SET status=?, suggestions=?, failure=?, lease_owner=NULL, lease_until=NULL, updated_at=NOW(6)
WHERE ontology_id=? AND version_id=? AND status='running' AND lease_owner=?`,
		string(end.Status()), nullJSON(end.Suggestions), nullString(end.Failure), ontologyID, versionID, workerID)
}

// ChargeFoundryAutomationSuggestions enters what a write's calls spent in
// the workspace's ledger, charged now, whether or not the write ended or
// still holds its lease: it was spent. Nothing is entered for nothing.
func (s *mysqlDB) ChargeFoundryAutomationSuggestions(ctx context.Context, workspaceID string, usdMicros int64) error {
	switch {
	case workspaceID == "" || usdMicros < 0:
		return fmt.Errorf("invalid automation suggestions charge")
	case usdMicros == 0:
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_automation_suggestion_ledger (id, workspace_id, kind, usd_micros)
VALUES (?, ?, 'charged', ?)`, id.New(), workspaceID, usdMicros)
	return err
}

// SumFoundryAssistantSpend is what the Assistant's people's pool spent
// since: people's settled turns and automation suggestions, the spend
// foundry.agent's monthly budget bounds.
func (s *mysqlDB) SumFoundryAssistantSpend(ctx context.Context, workspaceID string, since time.Time) (int64, error) {
	var spent int64
	err := s.db.QueryRowContext(ctx, `SELECT `+foundryAssistantSettled, workspaceID, since.UTC(), workspaceID, since.UTC()).Scan(&spent)
	return spent, err
}

func scanFoundryAutomationSuggestions(scanner rowScanner) (*model.FoundryAutomationSuggestions, error) {
	var out model.FoundryAutomationSuggestions
	var status string
	var questions []byte
	var suggestions, failure, leaseOwner sql.NullString
	var leaseUntil sql.NullTime
	if err := scanner.Scan(&out.OntologyID, &out.VersionID, &out.WorkspaceID, &status, &questions, &suggestions, &failure, &out.Attempts,
		&leaseOwner, &leaseUntil, &out.RequestedAt, &out.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	out.Status = model.FoundryAutomationSuggestionsStatus(status)
	if err := json.Unmarshal(questions, &out.Questions); err != nil {
		return nil, fmt.Errorf("automation suggestions' questions: %w", err)
	}
	if suggestions.Valid {
		out.Suggestions = []byte(suggestions.String)
	}
	if failure.Valid {
		out.Failure = &failure.String
	}
	if leaseOwner.Valid {
		out.LeaseOwner = &leaseOwner.String
	}
	if leaseUntil.Valid {
		out.LeaseUntil = &leaseUntil.Time
	}
	return &out, nil
}
