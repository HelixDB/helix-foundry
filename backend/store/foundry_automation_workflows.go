package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Automations' workflows (migrations/20261203000000_foundry_automation_workflows.sql):
// Foundry's own record of how much an ontology helps an automation. Each
// ontology an automation uses has one workflow of the automation's
// (foundry_workflows.automation_id), and each run of the automation that
// uses it one run of that workflow (automation_run_id), started at its first
// use and finished when the automation's run ends, in the write that ends
// it. Only Foundry starts and finishes them.
//
// Lock order, the automations' and the workflows' together: an automation,
// its run, an ontology, its workflows, their runs.

// automationWorkflowToken is the client token of an automation's workflow
// of an ontology.
func automationWorkflowToken(automationID string) string { return "automation:" + automationID }

// EnsureFoundryAutomationWorkflow makes the automation's workflow of the
// ontology, unless it has one, and returns it: named after the automation and
// made by its owner, whoever asks. ErrNotFound when the automation, or the
// live ontology, is not the workspace's; ErrLimitExceeded when the ontology
// holds model.FoundryWorkflowsMax workflows, which is recorded as why the
// automation's runs that use it go untracked.
func (s *mysqlDB) EnsureFoundryAutomationWorkflow(ctx context.Context, workspaceID, automationID, ontologyID string) (*model.FoundryWorkflow, error) {
	var workflowID string
	err := s.inTx(ctx, func(tx *mysqlDB) error {
		owner, name, err := tx.shareFoundryAutomation(ctx, workspaceID, automationID)
		if err != nil {
			return err
		}
		workflowID, err = tx.ensureFoundryAutomationWorkflow(ctx, workspaceID, automationID, owner, name, ontologyID)
		return err
	})
	if errors.Is(err, ErrLimitExceeded) {
		return nil, s.untrackFoundryAutomation(ctx, workspaceID, automationID, ontologyID, err)
	}
	if err != nil {
		return nil, err
	}
	return s.GetFoundryWorkflow(ctx, ontologyID, workflowID)
}

// GetFoundryAutomationWorkflowRun reads the workflow run that records the
// automation run's use of the ontology: ErrNotFound before its first use.
func (s *mysqlDB) GetFoundryAutomationWorkflowRun(ctx context.Context, automationRunID, ontologyID string) (*model.FoundryWorkflowRun, error) {
	return scanFoundryWorkflowRun(s.db.QueryRowContext(ctx, "SELECT "+foundryWorkflowRunColumns+`
FROM foundry_workflow_runs WHERE automation_run_id=? AND ontology_id=?`, automationRunID, ontologyID))
}

// StartFoundryAutomationWorkflowRun starts the workflow run that records the
// automation run's use of an ontology, with start.Input as its request, in
// the automation's workflow of the ontology, which it makes at need; a run
// started already is returned as it is. The run is the automation's owner's.
// ErrNotFound when the automation run isn't live, or the live ontology isn't
// the workspace's; ErrLimitExceeded as EnsureFoundryAutomationWorkflow.
func (s *mysqlDB) StartFoundryAutomationWorkflowRun(ctx context.Context, start model.FoundryAutomationWorkflowStart) (*model.FoundryWorkflowRun, error) {
	if start.WorkspaceID == "" || start.AutomationRunID == "" || start.OntologyID == "" || strings.TrimSpace(start.Input) == "" ||
		len(start.Input) > model.FoundryWorkflowInputMaxBytes {
		return nil, fmt.Errorf("an automation's workflow run needs its run, its ontology and a request of at most %d bytes", model.FoundryWorkflowInputMaxBytes)
	}
	var automationID string
	err := s.db.QueryRowContext(ctx, `SELECT automation_id FROM foundry_automation_runs WHERE id=? AND workspace_id=?`,
		start.AutomationRunID, start.WorkspaceID).Scan(&automationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var runID string
	err = s.inTx(ctx, func(tx *mysqlDB) error {
		owner, name, err := tx.shareFoundryAutomation(ctx, start.WorkspaceID, automationID)
		if err != nil {
			return err
		}
		// The run stays live while this write starts its workflow run: the
		// write that ends it finishes its workflow runs.
		var status string
		switch err := tx.db.QueryRowContext(ctx, `SELECT status FROM foundry_automation_runs WHERE id=? FOR SHARE`, start.AutomationRunID).Scan(&status); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		case !model.FoundryAutomationRunStatus(status).Live():
			return ErrNotFound
		}
		workflowID, err := tx.ensureFoundryAutomationWorkflow(ctx, start.WorkspaceID, automationID, owner, name, start.OntologyID)
		if err != nil {
			return err
		}
		input := start.Input
		r := &model.FoundryWorkflowRun{ID: id.New(), WorkflowID: workflowID, WorkspaceID: start.WorkspaceID, OntologyID: start.OntologyID,
			ClientToken: start.AutomationRunID, StartedBy: access.Principal{Kind: access.PrincipalUser, ID: owner}, Input: &input,
			AutomationRunID: &start.AutomationRunID}
		switch err := tx.StartFoundryWorkflowRun(ctx, r); {
		case errors.Is(err, ErrConflict):
			// Another use started it first.
			return tx.db.QueryRowContext(ctx, `SELECT id FROM foundry_workflow_runs WHERE workflow_id=? AND client_token=?`,
				workflowID, start.AutomationRunID).Scan(&runID)
		case err != nil:
			return err
		}
		runID = r.ID
		return nil
	})
	if errors.Is(err, ErrLimitExceeded) {
		return nil, s.untrackFoundryAutomation(ctx, start.WorkspaceID, automationID, start.OntologyID, err)
	}
	if err != nil {
		return nil, err
	}
	return s.GetFoundryWorkflowRun(ctx, start.OntologyID, runID)
}

// shareFoundryAutomation reads the workspace's automation's owner and name,
// holding it against a deletion until the transaction ends.
func (s *mysqlDB) shareFoundryAutomation(ctx context.Context, workspaceID, automationID string) (owner, name string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT owner_user_id, name FROM foundry_automations WHERE id=? AND workspace_id=? FOR SHARE`,
		automationID, workspaceID).Scan(&owner, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return owner, name, err
}

// ensureFoundryAutomationWorkflow is the id of the automation's workflow of
// the live ontology, made under the ontology's lock (as
// CreateFoundryWorkflow makes one) when it has none, which clears why the
// automation went untracked there.
func (s *mysqlDB) ensureFoundryAutomationWorkflow(ctx context.Context, workspaceID, automationID, owner, name, ontologyID string) (string, error) {
	if _, err := s.lockLiveFoundryOntology(ctx, workspaceID, ontologyID); err != nil {
		return "", err
	}
	var workflowID string
	switch err := s.db.QueryRowContext(ctx, `SELECT id FROM foundry_workflows WHERE ontology_id=? AND automation_id=? FOR UPDATE`,
		ontologyID, automationID).Scan(&workflowID); {
	case err == nil:
		return workflowID, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", err
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_workflows WHERE ontology_id=?`, ontologyID).Scan(&n); err != nil {
		return "", err
	}
	if n >= model.FoundryWorkflowsMax {
		return "", ErrLimitExceeded
	}
	workflowID = id.New()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_workflows
  (id, workspace_id, ontology_id, name, description, client_token, created_by_user_id, automation_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		workflowID, workspaceID, ontologyID, model.FoundryAutomationWorkflowName(name), model.FoundryAutomationWorkflowDescription,
		automationWorkflowToken(automationID), owner, automationID); err != nil {
		return "", err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM foundry_automation_untracked WHERE automation_id=? AND ontology_id=?`, automationID, ontologyID)
	return workflowID, err
}

// untrackFoundryAutomation records that the automation's runs that use the
// ontology go untracked, the ontology's workflows being full, and returns
// full (ErrLimitExceeded) unless the record fails.
func (s *mysqlDB) untrackFoundryAutomation(ctx context.Context, workspaceID, automationID, ontologyID string, full error) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_automation_untracked (automation_id, ontology_id, workspace_id, reason)
SELECT id, ?, workspace_id, ? FROM foundry_automations WHERE id=? AND workspace_id=?
ON DUPLICATE KEY UPDATE reason=VALUES(reason), at=NOW(6)`, ontologyID, string(model.FoundryAutomationWorkflowsFull), automationID, workspaceID); err != nil {
		return fmt.Errorf("record an untracked automation: %w", err)
	}
	return full
}

// finishFoundryAutomationWorkflowRuns finishes the open workflow runs the
// automation runs runs selects (with arg) started, with answer, queueing
// their assessments with assess: inside the transaction that ends those
// runs, which holds them.
func (s *mysqlDB) finishFoundryAutomationWorkflowRuns(ctx context.Context, runs string, arg any, answer string, assess bool) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM foundry_workflow_runs WHERE automation_run_id IN (`+runs+`) AND status='open' FOR UPDATE`, arg)
	if err != nil {
		return err
	}
	open, err := scanStrings(rows)
	if err != nil || len(open) == 0 {
		return err
	}
	in := idPlaceholders(len(open))
	if _, err := s.db.ExecContext(ctx, `UPDATE foundry_workflow_runs SET status='finished', finished_at=NOW(6), output=?, input=COALESCE(input, ?)
WHERE id IN (`+in+`)`, append([]any{answer, answer}, toAnySlice(open)...)...); err != nil {
		return fmt.Errorf("finish an automation's workflow runs: %w", err)
	}
	if !assess {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO foundry_workflow_run_assessments (run_id, workspace_id, ontology_id, workflow_id, status)
SELECT id, workspace_id, ontology_id, workflow_id, 'queued' FROM foundry_workflow_runs WHERE id IN (`+in+`)`, toAnySlice(open)...)
	return err
}

// ListFoundryAutomationOntologies returns the ontologies each of the
// workspace's automations named uses, by automation id (one with none is
// missing), by ontology name: each live ontology it has a workflow of, with
// that workflow's runs and verdicts counted, and each it went untracked on.
func (s *mysqlDB) ListFoundryAutomationOntologies(ctx context.Context, workspaceID string, automationIDs []string) (map[string][]model.FoundryAutomationOntology, error) {
	out := map[string][]model.FoundryAutomationOntology{}
	if len(automationIDs) == 0 {
		return out, nil
	}
	in := idPlaceholders(len(automationIDs))
	rows, err := s.db.QueryContext(ctx, `SELECT o.name, `+foundryWorkflowColumns+`
FROM foundry_workflows w JOIN foundry_ontologies o ON o.id = w.ontology_id AND o.deleted_at IS NULL
WHERE w.workspace_id=? AND w.automation_id IN (`+in+`) ORDER BY o.name, o.id`, append([]any{workspaceID}, toAnySlice(automationIDs)...)...)
	if err != nil {
		return nil, err
	}
	type named struct {
		w    *model.FoundryWorkflow
		name string
	}
	workflows, err := scanAll(rows, func(r rowScanner) (named, error) {
		var n named
		var err error
		n.w, err = scanFoundryWorkflow(prefixedScanner{row: r, first: &n.name})
		return n, err
	})
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(workflows))
	for i, n := range workflows {
		ids[i] = n.w.ID
	}
	verdicts, err := s.CountFoundryWorkflowVerdicts(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, n := range workflows {
		counted := verdicts[n.w.ID]
		if counted == nil {
			counted = map[string]int64{}
		}
		out[n.w.Automation.ID] = append(out[n.w.Automation.ID], model.FoundryAutomationOntology{OntologyID: n.w.OntologyID, OntologyName: n.name,
			Tracking: model.FoundryAutomationTracked{Workflow: n.w, Verdicts: counted}})
	}
	untracked, err := s.db.QueryContext(ctx, `SELECT u.automation_id, u.ontology_id, o.name, u.reason, u.at
FROM foundry_automation_untracked u JOIN foundry_ontologies o ON o.id = u.ontology_id AND o.deleted_at IS NULL
WHERE u.workspace_id=? AND u.automation_id IN (`+in+`) ORDER BY o.name, o.id`, append([]any{workspaceID}, toAnySlice(automationIDs)...)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = untracked.Close() }()
	for untracked.Next() {
		var automationID, reason string
		var o model.FoundryAutomationOntology
		var u model.FoundryAutomationUntracked
		if err := untracked.Scan(&automationID, &o.OntologyID, &o.OntologyName, &reason, &u.At); err != nil {
			return nil, err
		}
		u.Reason = model.FoundryAutomationUntrackedReason(reason)
		o.Tracking = u
		out[automationID] = append(out[automationID], o)
	}
	return out, untracked.Err()
}

// ListFoundryAutomationRunOntologies returns the ontologies the automation's
// run used, by name: the workflow run that records each, and its
// assessment.
func (s *mysqlDB) ListFoundryAutomationRunOntologies(ctx context.Context, automationRunID string) ([]model.FoundryAutomationRunOntology, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT o.name, `+prefixed("r", foundryWorkflowRunColumns)+`
FROM foundry_workflow_runs r JOIN foundry_ontologies o ON o.id = r.ontology_id AND o.deleted_at IS NULL
WHERE r.automation_run_id=? ORDER BY o.name, o.id`, automationRunID)
	if err != nil {
		return nil, err
	}
	out, err := scanAll(rows, func(r rowScanner) (model.FoundryAutomationRunOntology, error) {
		var o model.FoundryAutomationRunOntology
		var name string
		run, err := scanFoundryWorkflowRun(prefixedScanner{row: r, first: &name})
		if err != nil {
			return o, err
		}
		o.OntologyID, o.OntologyName, o.Run = run.OntologyID, name, run
		return o, nil
	})
	if err != nil || len(out) == 0 {
		return out, err
	}
	ids := make([]string, len(out))
	for i, o := range out {
		ids[i] = o.Run.ID
	}
	assessments, err := s.ListFoundryWorkflowRunAssessments(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Assessment = assessments[out[i].Run.ID]
	}
	return out, nil
}

// LatestFoundryWorkflowAssessments returns the newest done assessment of each
// workflow named that has one, by workflow id.
func (s *mysqlDB) LatestFoundryWorkflowAssessments(ctx context.Context, workflowIDs []string) (map[string]*model.FoundryWorkflowAssessment, error) {
	out := map[string]*model.FoundryWorkflowAssessment{}
	for _, workflowID := range workflowIDs {
		a, err := scanFoundryWorkflowAssessment(s.db.QueryRowContext(ctx, `SELECT `+foundryWorkflowAssessmentColumns+`
FROM foundry_workflow_run_assessments WHERE workflow_id=? AND status='done' ORDER BY finished_at DESC, run_id DESC LIMIT 1`, workflowID))
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return nil, err
		default:
			out[workflowID] = a
		}
	}
	return out, nil
}

// prefixed is a column list with each column qualified by table.
func prefixed(table, columns string) string {
	parts := strings.Split(columns, ",")
	for i, p := range parts {
		parts[i] = table + "." + strings.TrimSpace(p)
	}
	return strings.Join(parts, ", ")
}

// prefixedScanner scans a row's first column into first, and the rest as
// row's scanner would.
type prefixedScanner struct {
	row   rowScanner
	first any
}

func (p prefixedScanner) Scan(dest ...any) error {
	return p.row.Scan(append([]any{p.first}, dest...)...)
}
