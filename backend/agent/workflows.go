package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/automation"
	"github.com/helixdb/helix-foundry/backend/dashboard"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// A workflow's run (an automation's, as the code calls it) records how much
// each ontology it uses helped it: Foundry keeps a use case of each ontology
// a workflow uses (package store's automation workflows), and each run that
// uses one starts a run of that use case at its first use, which the run's
// end finishes with what it found, so the use case's assessments say what
// the ontology was worth to the workflow. A use is an Assistant step's call of a tool that names the
// ontology (AutomationUses.Called) or an action step on it. Its Assistant
// steps' reads and questions of the ontology are recorded under the
// use case run, whose id Foundry gives the tool: a model never chooses or
// forges one, and only these calls hold its key (FoundryWorkflowRun.
// Recording), so no other caller records under it. Tracking never stops a
// run: a use that can't be recorded goes untracked.

// WorkflowStore is what recording an automation run's uses of ontologies
// reads and writes.
type WorkflowStore interface {
	GetConnector(ctx context.Context, workspaceID, id string) (*model.Connector, error)
	GetFoundryOntology(ctx context.Context, workspaceID, id string) (*model.FoundryOntology, error)
	GetFoundryDocumentSet(ctx context.Context, workspaceID, id string) (*model.FoundryDocumentSet, error)
	GetFoundryDashboard(ctx context.Context, workspaceID, id string) (*model.FoundryDashboard, error)
	ListFoundryAutomationRunNodes(ctx context.Context, runID string) ([]*model.FoundryAutomationRunNode, error)
	GetFoundryAutomationWorkflowRun(ctx context.Context, automationRunID, ontologyID string) (*model.FoundryWorkflowRun, error)
	StartFoundryAutomationWorkflowRun(ctx context.Context, start model.FoundryAutomationWorkflowStart) (*model.FoundryWorkflowRun, error)
}

// AutomationUses are one automation run's uses of ontologies, as one
// attempt of a step finds them: the use case run that records each.
type AutomationUses struct {
	db   WorkflowStore
	run  *model.FoundryAutomationRun
	name string
	spec automation.Spec
	// runs is how calls name each ontology's use case run (its Recording),
	// "" for one gone untracked.
	runs map[string]string
}

// NewAutomationUses are the uses of run, an automation named name running
// spec.
func NewAutomationUses(db WorkflowStore, run *model.FoundryAutomationRun, name string, spec automation.Spec) *AutomationUses {
	return &AutomationUses{db: db, run: run, name: name, spec: spec, runs: map[string]string{}}
}

// workflowToolsRecorded are the Assistant's tools whose calls a use case run
// records, by their use_case_run_id: reads and questions of a load.
var workflowToolsRecorded = map[string]bool{"helix_foundry_read_graph": true, toolAsk: true}

// workflowRunField is the input field a tool records its call under.
const workflowRunField = "use_case_run_id"

// Use is how calls name the open use case run that records the run's use of
// ontologyID (its Recording), started at its first: "" when it goes untracked (the ontology holds as
// many use cases as it may, it isn't a live ontology of the workspace, or
// the run ended). An error is the store's: the use goes untracked too, and
// the caller logs it.
func (u *AutomationUses) Use(ctx context.Context, ontologyID string) (string, error) {
	if recording, ok := u.runs[ontologyID]; ok {
		return recording, nil
	}
	r, err := u.db.GetFoundryAutomationWorkflowRun(ctx, u.run.ID, ontologyID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		input, inputErr := u.request(ctx)
		if inputErr != nil {
			return "", inputErr
		}
		r, err = u.db.StartFoundryAutomationWorkflowRun(ctx, model.FoundryAutomationWorkflowStart{WorkspaceID: u.run.WorkspaceID,
			AutomationRunID: u.run.ID, OntologyID: ontologyID, Input: input})
	}
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || errors.Is(err, foundrystore.ErrLimitExceeded):
		u.runs[ontologyID] = ""
		return "", nil
	case err != nil:
		return "", err
	case r.Finished != nil:
		u.runs[ontologyID] = ""
		return "", nil
	}
	u.runs[ontologyID] = r.Recording()
	return u.runs[ontologyID], nil
}

// request is the run in words (automation.Request): what fired it, as its
// trigger's output says, and its steps, naming what they act on.
func (u *AutomationUses) request(ctx context.Context) (string, error) {
	nodes, err := u.db.ListFoundryAutomationRunNodes(ctx, u.run.ID)
	if err != nil {
		return "", err
	}
	fired := ""
	for _, n := range nodes {
		var out automation.Output
		if n.NodeID == u.spec.TriggerID() && json.Unmarshal(n.Output, &out) == nil {
			fired = out.Summary
		}
	}
	ws, t := u.run.WorkspaceID, u.spec.Targets()
	names := map[string]string{}
	for _, id := range t.Connectors {
		if c, err := u.db.GetConnector(ctx, ws, id); err == nil {
			names[id] = c.Name
		}
	}
	for _, id := range t.Ontologies {
		if o, err := u.db.GetFoundryOntology(ctx, ws, id); err == nil {
			names[id] = o.Name
		}
	}
	for _, id := range t.Sets {
		if s, err := u.db.GetFoundryDocumentSet(ctx, ws, id); err == nil {
			names[id] = s.Name
		}
	}
	for _, id := range t.Dashboards {
		if d, err := u.db.GetFoundryDashboard(ctx, ws, id); err == nil {
			names[id] = d.Name
		}
	}
	return CutText(automation.Request(u.name, fired, u.spec, names), model.FoundryWorkflowInputMaxBytes), nil
}

// Called records an Assistant step's tool call as a use of the ontologies
// it names (its input's ontology_id; an action it prepares on an ontology,
// or a publish of a dashboard, whose tiles read theirs), and sets args, its
// input, as the tool takes it: a read's or a question's with the
// use_case_run_id of the run that records it, any other's without one,
// whatever the model wrote. An error is the store's: the call runs
// unrecorded.
func (u *AutomationUses) Called(ctx context.Context, tool string, args map[string]any) error {
	delete(args, workflowRunField)
	if tool == toolPrepare {
		return u.UseAll(ctx, u.prepared(ctx, args))
	}
	ontologyID, _ := args["ontology_id"].(string)
	if ontologyID == "" {
		return nil
	}
	recording, err := u.Use(ctx, ontologyID)
	if recording != "" && workflowToolsRecorded[tool] {
		args[workflowRunField] = recording
	}
	return err
}

// UseAll records a use of each ontology, untracked where it can't be: the
// first error is the store's.
func (u *AutomationUses) UseAll(ctx context.Context, ontologyIDs []string) error {
	var first error
	for _, id := range ontologyIDs {
		if _, err := u.Use(ctx, id); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// prepared are the ontologies of the workspace an action the step prepares
// acts on: its target's, or a published dashboard's tiles'.
func (u *AutomationUses) prepared(ctx context.Context, args map[string]any) []string {
	target, _ := args["target"].(string)
	ws := u.run.WorkspaceID
	if id, ok := strings.CutPrefix(target, "ontology:"+ws+"/"); ok && id != "" && !strings.Contains(id, "/") {
		return []string{id}
	}
	if id, ok := strings.CutPrefix(target, "dashboard:"+ws+"/"); ok && args["operation"] == "publish_dashboard" {
		return u.DashboardOntologies(ctx, id)
	}
	return nil
}

// DashboardOntologies are the ontologies a dashboard of the workspace's
// tiles read: none when it can't be read.
func (u *AutomationUses) DashboardOntologies(ctx context.Context, dashboardID string) []string {
	d, err := u.db.GetFoundryDashboard(ctx, u.run.WorkspaceID, dashboardID)
	if err != nil {
		return nil
	}
	spec, err := dashboard.ParseSpec(d.Spec)
	if err != nil {
		return nil
	}
	return spec.Ontologies()
}

// withoutWorkflowRunArg is a call's input (JSON) without use_case_run_id:
// input itself when it names none or isn't an object.
func withoutWorkflowRunArg(input json.RawMessage) json.RawMessage {
	var args map[string]json.RawMessage
	if json.Unmarshal(input, &args) != nil || args == nil {
		return input
	}
	if _, ok := args[workflowRunField]; !ok {
		return input
	}
	delete(args, workflowRunField)
	out, err := json.Marshal(args)
	if err != nil {
		return input
	}
	return out
}

// withoutWorkflowRun is a tool's input schema (JSON) without use_case_run_id:
// Foundry gives it, never the model.
func withoutWorkflowRun(schema json.RawMessage) json.RawMessage {
	var s map[string]any
	if json.Unmarshal(schema, &s) != nil {
		return schema
	}
	props, _ := s["properties"].(map[string]any)
	if _, ok := props[workflowRunField]; !ok {
		return schema
	}
	delete(props, workflowRunField)
	if required, ok := s["required"].([]any); ok {
		kept := required[:0]
		for _, r := range required {
			if r != workflowRunField {
				kept = append(kept, r)
			}
		}
		s["required"] = kept
	}
	out, err := json.Marshal(s)
	if err != nil {
		return schema
	}
	return out
}

// CutText is text at most limit bytes long, on a rune boundary, ending with
// an ellipsis when cut.
//
//	agent.CutText("héllo", 4) // "h…"
func CutText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit - len("…")
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "…"
}
