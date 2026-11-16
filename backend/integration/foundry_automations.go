package integration

import (
	"context"
	"encoding/json"
	"strings"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/helixdb/helix-foundry/backend/automation"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// Automations (package automation): the read tools, and the operations
// helix_foundry_prepare_operation prepares. An automation runs as its owner,
// the person who made it, and only once they approved its spec with a click
// in Foundry: no tool approves one, so what an agent creates or changes waits
// for that click. "Workflows" (foundry_workflows.go) is another thing: an
// outside agent's recorded runs.
const (
	toolFoundryListAutomations    = "helix_foundry_list_automations"
	toolFoundryGetAutomation      = "helix_foundry_get_automation"
	toolFoundryListAutomationRuns = "helix_foundry_list_automation_runs"
	toolFoundryGetAutomationRun   = "helix_foundry_get_automation_run"
)

// automationSpecDoc says what a spec is, for the tools' and the operations' descriptions.
const automationSpecDoc = `A spec is a graph: {"v": 1, "nodes": [...], "edges": [{"from", "to", "branch"}]}, at most 50 nodes and 100 edges. ` +
	`A node is {"id": 1 to 32 lowercase letters, digits, - or _, "kind", "config", "position": {"x", "y"} (where the canvas draws it)}. ` +
	`Exactly one node is the trigger, with no edge in; every other node is reached from it, with no cycle. Kinds and their configs: ` +
	`trigger {"kind": "schedule", "every": "hour", "minute": 0-59, "time_zone"} | {"kind": "schedule", "every": "day", "at": "HH:MM", "time_zone"} | ` +
	`{"kind": "schedule", "every": "week", "weekday": "monday" … "sunday", "at": "HH:MM", "time_zone"} (an IANA zone, such as Europe/London: the person's) | ` +
	`{"kind": "sync_finished", "connector_id", "outcome": "succeeded" | "failed" | "any"} | {"kind": "load_ready", "ontology_id"} | ` +
	`{"kind": "document_arrived", "set_id"} | {"kind": "generation_finished", "ontology_id", "outcome"} | {"kind": "manual"}; ` +
	`assistant {"instruction": 1 to 2000 characters, run as a Founder turn as the owner, "allow": up to 10 actions it may run unasked (anything else it prepares waits for the owner)}; ` +
	`action {"operation": "sync_connector", "connector_id"} | {"operation": "create_load", "ontology_id"} (its current version) | ` +
	`{"operation": "publish_dashboard", "dashboard_id"} (refresh its public snapshot); ` +
	`condition {"check": "answer", "equals": "yes" | "no"} (of the Founder step before it, which ends by reporting a yes or no) | ` +
	`{"check": "count", "op": "at_least" | "at_most" | "equals", "value"} (of the documents that arrived, the rows a sync wrote, or a Founder step's reported count), ` +
	`with exactly one edge in and its out edges each "branch": "true" or "false"; notify {"message": at most 500 characters}: emails the owner, with what the Founder steps before it said. ` +
	`A step runs once every step before it ended and one of its edges in was taken; a failure stops the steps after it.`

type FoundryAutomationInput struct {
	WorkspaceID  string `json:"workspace_id,omitempty" jsonschema:"Helix workspace identifier. May be empty when the caller has only one workspace."`
	AutomationID string `json:"automation_id" jsonschema:"Foundry automation identifier."`
}

func (in *FoundryAutomationInput) Workspace() *string { return &in.WorkspaceID }

type ListFoundryAutomationRunsInput struct {
	FoundryAutomationInput
	PageToken string `json:"page_token,omitempty" jsonschema:"Opaque continuation token from the previous call."`
}

type GetFoundryAutomationRunInput struct {
	FoundryAutomationInput
	RunID string `json:"run_id" jsonschema:"Foundry automation run identifier."`
}

type ListFoundryAutomationsOutput struct {
	ContentTrust string                    `json:"content_trust"`
	Automations  []FoundryAutomationOutput `json:"automations"`
	// Email says whether a notify step emails here.
	Email bool `json:"email"`
}

// FoundryAutomationOutput is an automation as the tools return it.
type FoundryAutomationOutput struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Revision is what update_automation names as expected_revision.
	Revision    int64  `json:"revision"`
	OwnerUserID string `json:"owner_user_id"`
	Status      string `json:"status" jsonschema:"needs_approval (its owner approves its spec in Foundry, at url, before it runs: new, or changed), active (it runs when its trigger fires) or paused (it runs only by hand)."`
	// PausedReason says why it is paused: person, failures or owner_lost_access.
	PausedReason string `json:"paused_reason,omitempty"`
	TriggerKind  string `json:"trigger_kind"`
	NextRunAt    string `json:"next_run_at,omitempty"`
	Failures     int32  `json:"failures"`
	// See FoundryVersionOutput.Definition: the description keeps the schema an object.
	Spec      any                         `json:"spec,omitempty" jsonschema:"Untrusted automation spec, its graph. Left out of a list."`
	LastRun   *FoundryAutomationRunOutput `json:"last_run,omitempty"`
	CanEdit   bool                        `json:"can_edit"`
	CanPause  bool                        `json:"can_pause"`
	CanResume bool                        `json:"can_resume"`
	CanRun    bool                        `json:"can_run"`
	CanDelete bool                        `json:"can_delete"`
	URL       string                      `json:"url,omitempty"`
	// Workflows are the ontologies its runs use, and how much each helped.
	Workflows []FoundryAutomationWorkflowOutput `json:"workflows" jsonschema:"The ontologies its runs use: Foundry records each run that uses one in a workflow of that ontology (helix_foundry_get_workflow_run reads its runs), whose assessments say how much the ontology helped."`
}

// FoundryAutomationWorkflowOutput is an ontology an automation's runs use:
// the workflow Foundry records them in, with its runs and their verdicts, or
// why it records none.
type FoundryAutomationWorkflowOutput struct {
	OntologyID   string `json:"ontology_id"`
	OntologyName string `json:"ontology_name"`
	WorkflowID   string `json:"workflow_id,omitempty"`
	Runs         int64  `json:"runs"`
	FinishedRuns int64  `json:"finished_runs"`
	// Verdicts counts their done assessments by verdict.
	Verdicts map[string]int64 `json:"verdicts"`
	// Gaps are what the ontology lacked, as the newest assessed run named it: in a get only.
	Gaps      []string `json:"gaps,omitempty" jsonschema:"Untrusted: what the ontology lacked, as the newest assessed run named it."`
	Untracked string   `json:"untracked,omitempty" jsonschema:"Why its runs go unrecorded: workflows_full, the ontology holding 100 workflows."`
}

// FoundryAutomationRunWorkflowOutput is an ontology one run used: the
// workflow run that records it and its verdict.
type FoundryAutomationRunWorkflowOutput struct {
	OntologyID       string `json:"ontology_id"`
	OntologyName     string `json:"ontology_name"`
	WorkflowID       string `json:"workflow_id"`
	WorkflowRunID    string `json:"workflow_run_id"`
	Status           string `json:"status"`
	AssessmentStatus string `json:"assessment_status,omitempty"`
	Verdict          string `json:"verdict,omitempty" jsonschema:"Once assessed: essential, helpful, marginal, unused or misleading."`
}

// FoundryAutomationRunOutput is a run as the tools return it.
type FoundryAutomationRunOutput struct {
	ID           string `json:"id"`
	AutomationID string `json:"automation_id"`
	Status       string `json:"status" jsonschema:"queued, running, succeeded, failed, cancelled or skipped (never started: error says why)."`
	Error        string `json:"error,omitempty"`
	// See FoundryVersionOutput.Definition: the description keeps the schema an object.
	Trigger    any                           `json:"trigger,omitempty" jsonschema:"Untrusted: what fired it, its kind and the ids of the work it found."`
	USDMicros  int64                         `json:"usd_micros"`
	ThreadID   string                        `json:"thread_id,omitempty"`
	CreatedAt  string                        `json:"created_at"`
	FinishedAt string                        `json:"finished_at,omitempty"`
	Steps      []FoundryAutomationStepOutput `json:"steps,omitempty"`
	URL        string                        `json:"url,omitempty"`
	// Workflows are the ontologies it used, in a get only.
	Workflows []FoundryAutomationRunWorkflowOutput `json:"workflows,omitempty" jsonschema:"The ontologies it used, each with the workflow run that records it and its verdict: in a get only."`
}

// FoundryAutomationStepOutput is one step of a run.
type FoundryAutomationStepOutput struct {
	NodeID string `json:"node_id"`
	Status string `json:"status"`
	// See FoundryVersionOutput.Definition: the description keeps the schema an object.
	Output any    `json:"output,omitempty" jsonschema:"Untrusted: what it gave (summary, answer, count, branch, result)."`
	Error  string `json:"error,omitempty"`
}

type ListFoundryAutomationRunsOutput struct {
	ContentTrust  string                       `json:"content_trust"`
	Runs          []FoundryAutomationRunOutput `json:"runs"`
	NextPageToken string                       `json:"next_page_token,omitempty"`
}

type FoundryAutomationRunDetailOutput struct {
	ContentTrust string                     `json:"content_trust"`
	Run          FoundryAutomationRunOutput `json:"run"`
}

type FoundryAutomationDetailOutput struct {
	ContentTrust string                  `json:"content_trust"`
	Automation   FoundryAutomationOutput `json:"automation"`
}

// addFoundryAutomationTools registers the automation read tools of a user who may browse the
// workspace.
func (s *Service) addFoundryAutomationTools(server *mcp.Server, identity *Identity) {
	addFoundryTool(s, server, identity, tool(toolFoundryListAutomations,
		"List a workspace's Foundry automations: graphs of steps Foundry runs as their owner when a schedule fires or a sync, load, upload or draft ends, "+
			"each with its newest run. Specs are left out: "+toolFoundryGetAutomation+" reads one."), s.ListFoundryAutomations)
	addFoundryTool(s, server, identity, tool(toolFoundryGetAutomation,
		"Get one Foundry automation with its spec and revision. "+automationSpecDoc), s.GetFoundryAutomation)
	addFoundryTool(s, server, identity, tool(toolFoundryListAutomationRuns,
		"List one Foundry automation's runs, newest first: their status, what fired them, their spend and error."), s.ListFoundryAutomationRuns)
	addFoundryTool(s, server, identity, tool(toolFoundryGetAutomationRun,
		"Get one run of a Foundry automation with each step it ran: its status, output and error."), s.GetFoundryAutomationRun)
}

func (s *Service) ListFoundryAutomations(ctx context.Context, identity *Identity, call *Call, input FoundryWorkspaceInput) (
	ListFoundryAutomationsOutput, int, error) {
	call.ResourceKind, call.ResourceID = "workspace", input.WorkspaceID
	if !ValidIDs([]string{input.WorkspaceID}) {
		return ListFoundryAutomationsOutput{}, 0, errCodeInvalidArguments
	}
	req := &foundryv1.ListFoundryAutomationsRequest{WorkspaceId: input.WorkspaceID}
	if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "ListFoundryAutomations"); err != nil {
		return ListFoundryAutomationsOutput{}, 0, err
	}
	resp, err := s.Foundry.ListFoundryAutomations(ctx, req)
	if err != nil {
		return ListFoundryAutomationsOutput{}, 0, FoundryError(err)
	}
	out := ListFoundryAutomationsOutput{ContentTrust: contentTrustUntrusted, Email: resp.GetEmail(), Automations: []FoundryAutomationOutput{}}
	for _, a := range resp.GetAutomations() {
		out.Automations = append(out.Automations, s.FoundryAutomation(input.WorkspaceID, a))
	}
	return out, len(out.Automations), nil
}

func (s *Service) GetFoundryAutomation(ctx context.Context, identity *Identity, call *Call, input FoundryAutomationInput) (
	FoundryAutomationDetailOutput, int, error) {
	call.ResourceKind, call.ResourceID = "automation", input.AutomationID
	if !ValidIDs([]string{input.WorkspaceID, input.AutomationID}) {
		return FoundryAutomationDetailOutput{}, 0, errCodeInvalidArguments
	}
	req := &foundryv1.GetFoundryAutomationRequest{WorkspaceId: input.WorkspaceID, Id: input.AutomationID}
	if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "GetFoundryAutomation"); err != nil {
		return FoundryAutomationDetailOutput{}, 0, err
	}
	resp, err := s.Foundry.GetFoundryAutomation(ctx, req)
	if err != nil {
		return FoundryAutomationDetailOutput{}, 0, FoundryError(err)
	}
	return FoundryAutomationDetailOutput{ContentTrust: contentTrustUntrusted, Automation: s.FoundryAutomation(input.WorkspaceID, resp.GetAutomation())}, 1, nil
}

func (s *Service) ListFoundryAutomationRuns(ctx context.Context, identity *Identity, call *Call, input ListFoundryAutomationRunsInput) (
	ListFoundryAutomationRunsOutput, int, error) {
	call.ResourceKind, call.ResourceID = "automation", input.AutomationID
	if !ValidIDs([]string{input.WorkspaceID, input.AutomationID}) || len(input.PageToken) > 1024 {
		return ListFoundryAutomationRunsOutput{}, 0, errCodeInvalidArguments
	}
	req := &foundryv1.ListFoundryAutomationRunsRequest{WorkspaceId: input.WorkspaceID, AutomationId: input.AutomationID, PageToken: input.PageToken}
	if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "ListFoundryAutomationRuns"); err != nil {
		return ListFoundryAutomationRunsOutput{}, 0, err
	}
	resp, err := s.Foundry.ListFoundryAutomationRuns(ctx, req)
	if err != nil {
		return ListFoundryAutomationRunsOutput{}, 0, FoundryError(err)
	}
	out := ListFoundryAutomationRunsOutput{ContentTrust: contentTrustUntrusted, NextPageToken: resp.GetNextPageToken(), Runs: []FoundryAutomationRunOutput{}}
	for _, r := range resp.GetRuns() {
		out.Runs = append(out.Runs, s.FoundryAutomationRun(input.WorkspaceID, r))
	}
	return out, len(out.Runs), nil
}

func (s *Service) GetFoundryAutomationRun(ctx context.Context, identity *Identity, call *Call, input GetFoundryAutomationRunInput) (
	FoundryAutomationRunDetailOutput, int, error) {
	call.ResourceKind, call.ResourceID = "automation", input.AutomationID
	if !ValidIDs([]string{input.WorkspaceID, input.AutomationID, input.RunID}) {
		return FoundryAutomationRunDetailOutput{}, 0, errCodeInvalidArguments
	}
	req := &foundryv1.GetFoundryAutomationRunRequest{WorkspaceId: input.WorkspaceID, AutomationId: input.AutomationID, Id: input.RunID}
	if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "GetFoundryAutomationRun"); err != nil {
		return FoundryAutomationRunDetailOutput{}, 0, err
	}
	resp, err := s.Foundry.GetFoundryAutomationRun(ctx, req)
	if err != nil {
		return FoundryAutomationRunDetailOutput{}, 0, FoundryError(err)
	}
	return FoundryAutomationRunDetailOutput{ContentTrust: contentTrustUntrusted, Run: s.FoundryAutomationRun(input.WorkspaceID, resp.GetRun())}, 1, nil
}

// FoundryAutomation is a as the tools return it, with its page in Foundry, where its owner
// approves it.
func (s *Service) FoundryAutomation(workspaceID string, a *foundryv1.FoundryAutomation) FoundryAutomationOutput {
	out := FoundryAutomationOutput{
		ID: a.GetId(), WorkspaceID: workspaceID, Name: a.GetName(), Description: a.GetDescription(), Revision: a.GetRevision(),
		OwnerUserID: a.GetOwnerUserId(), Status: a.GetStatus(), PausedReason: a.GetPausedReason(), TriggerKind: a.GetTriggerKind(),
		NextRunAt: protoTimestamp(a.GetNextRunAt()), Failures: a.GetFailures(), CanEdit: a.GetCanEdit(), CanPause: a.GetCanPause(),
		CanResume: a.GetCanResume(), CanRun: a.GetCanRun(), CanDelete: a.GetCanDelete(), URL: s.FoundryLink(workspaceID, "automations", a.GetId()),
	}
	if a.GetSpecJson() != "" {
		out.Spec = UntrustedJSON(a.GetSpecJson())
	}
	if a.GetLastRun() != nil {
		last := s.FoundryAutomationRun(workspaceID, a.GetLastRun())
		out.LastRun = &last
	}
	out.Workflows = []FoundryAutomationWorkflowOutput{}
	for _, o := range a.GetOntologies() {
		w := FoundryAutomationWorkflowOutput{OntologyID: o.GetOntologyId(), OntologyName: o.GetOntologyName(), WorkflowID: o.GetWorkflowId(),
			Runs: o.GetRuns(), FinishedRuns: o.GetFinishedRuns(), Verdicts: map[string]int64{}, Gaps: o.GetGaps(), Untracked: o.GetUntracked()}
		for _, v := range o.GetVerdicts() {
			w.Verdicts[v.GetVerdict()] = v.GetRuns()
		}
		out.Workflows = append(out.Workflows, w)
	}
	return out
}

// FoundryAutomationRun is r as the tools return it.
func (s *Service) FoundryAutomationRun(workspaceID string, r *foundryv1.FoundryAutomationRun) FoundryAutomationRunOutput {
	out := FoundryAutomationRunOutput{
		ID: r.GetId(), AutomationID: r.GetAutomationId(), Status: r.GetStatus(), Error: r.GetError(), USDMicros: r.GetUsdMicros(), ThreadID: r.GetThreadId(),
		CreatedAt: protoTimestamp(r.GetCreatedAt()), FinishedAt: protoTimestamp(r.GetFinishedAt()),
		URL: s.FoundryLink(workspaceID, "automations", r.GetAutomationId(), "runs", r.GetId()),
	}
	if r.GetTriggerEventJson() != "" {
		out.Trigger = UntrustedJSON(r.GetTriggerEventJson())
	}
	for _, n := range r.GetNodes() {
		step := FoundryAutomationStepOutput{NodeID: n.GetNodeId(), Status: n.GetStatus(), Error: n.GetError()}
		if n.GetOutputJson() != "" {
			step.Output = UntrustedJSON(n.GetOutputJson())
		}
		out.Steps = append(out.Steps, step)
	}
	for _, o := range r.GetOntologies() {
		out.Workflows = append(out.Workflows, FoundryAutomationRunWorkflowOutput{OntologyID: o.GetOntologyId(), OntologyName: o.GetOntologyName(),
			WorkflowID: o.GetWorkflowId(), WorkflowRunID: o.GetWorkflowRunId(), Status: o.GetStatus(), AssessmentStatus: o.GetAssessmentStatus(),
			Verdict: o.GetVerdict()})
	}
	return out
}

// CreateAutomationPayload is create_automation's.
type CreateAutomationPayload struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Spec        json.RawMessage `json:"spec"`
}

// UpdateAutomationPayload is update_automation's: at least one of name, description and spec.
type UpdateAutomationPayload struct {
	ExpectedRevision int64           `json:"expected_revision"`
	Name             *string         `json:"name,omitempty"`
	Description      *string         `json:"description,omitempty"`
	Spec             json.RawMessage `json:"spec,omitempty"`
}

// automationSpecPayload checks a payload's spec and writes it canonically.
func automationSpecPayload(raw json.RawMessage) (json.RawMessage, error) {
	spec, err := automation.ParseSpec(raw)
	if err != nil {
		return nil, err
	}
	return mustCanonical(spec), nil
}

// validAutomationName is an automation's name as the backend takes it: 1 to 120 characters
// once trimmed, with no control character.
func validAutomationName(name string) bool {
	return ValidFoundryName(name) && !strings.ContainsFunc(name, unicode.IsControl)
}

// validateAutomationOperation checks an automation operation's target and payload and
// authorizes the caller for it, as ValidateFoundryOperation does every operation. Automations
// are people's: every one is user-only. Whose automation it is, and whether it runs, the backend
// decides as it runs; none of them approves one.
func (s *Service) validateAutomationOperation(ctx context.Context, identity *Identity, input FoundryOperationInput, raw []byte) (FoundryOperation, error) {
	invalid := func(message string) error { return detailedToolError{code: errCodeInvalidArguments, message: message} }
	if input.Operation == "create_automation" {
		workspaceID, ok := cutTarget(input.Target, "workspace")
		var payload CreateAutomationPayload
		if !ok || decodeStrict(raw, &payload) != nil || !validAutomationName(payload.Name) || !ValidSetDescription(payload.Description) {
			return nil, invalid("create_automation takes target workspace:<workspace_id> and payload {name, description, spec}: a name of 1 to 120 characters, " +
				"a description of at most 1000, and a spec")
		}
		spec, err := automationSpecPayload(payload.Spec)
		if err != nil {
			return nil, invalid("create_automation's spec: " + err.Error())
		}
		payload.Name, payload.Description, payload.Spec = strings.TrimSpace(payload.Name), strings.TrimSpace(payload.Description), spec
		req := &foundryv1.CreateFoundryAutomationRequest{WorkspaceId: workspaceID, Name: payload.Name, Description: payload.Description, SpecJson: string(spec)}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "CreateFoundryAutomation"); err != nil {
			return nil, err
		}
		return automationOperation[*foundryv1.CreateFoundryAutomationRequest]{payload: payload, req: req, workspaceID: workspaceID,
			call: func(ctx context.Context, s *Service, req *foundryv1.CreateFoundryAutomationRequest, confirmationID string) (*foundryv1.FoundryAutomation, error) {
				req.ClientToken = confirmationID
				resp, err := s.Actions.CreateFoundryAutomation(ctx, req)
				return resp.GetAutomation(), err
			}}, nil
	}
	workspaceID, automationID, ok := CutFoundryTarget(input.Target, "automation")
	if !ok {
		return nil, invalid(input.Operation + " takes target automation:<workspace_id>/<automation_id>")
	}
	if input.Operation != "update_automation" && len(input.Payload) != 0 {
		return nil, invalid(input.Operation + " takes an empty payload")
	}
	switch input.Operation {
	case "update_automation":
		var payload UpdateAutomationPayload
		if decodeStrict(raw, &payload) != nil || payload.ExpectedRevision < 1 || payload.Name == nil && payload.Description == nil && len(payload.Spec) == 0 ||
			payload.Name != nil && !validAutomationName(*payload.Name) || payload.Description != nil && !ValidSetDescription(*payload.Description) {
			return nil, invalid("update_automation takes payload {expected_revision, name, description, spec}: the revision you read " +
				"and at least one change, a name of 1 to 120 characters, a description of at most 1000, a whole spec")
		}
		req := &foundryv1.UpdateFoundryAutomationRequest{WorkspaceId: workspaceID, Id: automationID, ExpectedRevision: payload.ExpectedRevision}
		if payload.Name != nil {
			name := strings.TrimSpace(*payload.Name)
			payload.Name, req.Name = &name, &name
		}
		if payload.Description != nil {
			description := strings.TrimSpace(*payload.Description)
			payload.Description, req.Description = &description, &description
		}
		if len(payload.Spec) > 0 {
			spec, err := automationSpecPayload(payload.Spec)
			if err != nil {
				return nil, invalid("update_automation's spec: " + err.Error())
			}
			specJSON := string(spec)
			payload.Spec, req.SpecJson = spec, &specJSON
		}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "UpdateFoundryAutomation"); err != nil {
			return nil, err
		}
		return automationOperation[*foundryv1.UpdateFoundryAutomationRequest]{payload: payload, req: req, workspaceID: workspaceID,
			call: func(ctx context.Context, s *Service, req *foundryv1.UpdateFoundryAutomationRequest, _ string) (*foundryv1.FoundryAutomation, error) {
				resp, err := s.Actions.UpdateFoundryAutomation(ctx, req)
				return resp.GetAutomation(), err
			}}, nil
	case "pause_automation":
		req := &foundryv1.PauseFoundryAutomationRequest{WorkspaceId: workspaceID, Id: automationID}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "PauseFoundryAutomation"); err != nil {
			return nil, err
		}
		return automationOperation[*foundryv1.PauseFoundryAutomationRequest]{payload: struct{}{}, req: req, workspaceID: workspaceID,
			call: func(ctx context.Context, s *Service, req *foundryv1.PauseFoundryAutomationRequest, _ string) (*foundryv1.FoundryAutomation, error) {
				resp, err := s.Actions.PauseFoundryAutomation(ctx, req)
				return resp.GetAutomation(), err
			}}, nil
	case "resume_automation":
		req := &foundryv1.ResumeFoundryAutomationRequest{WorkspaceId: workspaceID, Id: automationID}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "ResumeFoundryAutomation"); err != nil {
			return nil, err
		}
		return automationOperation[*foundryv1.ResumeFoundryAutomationRequest]{payload: struct{}{}, req: req, workspaceID: workspaceID,
			call: func(ctx context.Context, s *Service, req *foundryv1.ResumeFoundryAutomationRequest, _ string) (*foundryv1.FoundryAutomation, error) {
				resp, err := s.Actions.ResumeFoundryAutomation(ctx, req)
				return resp.GetAutomation(), err
			}}, nil
	case "delete_automation":
		req := &foundryv1.DeleteFoundryAutomationRequest{WorkspaceId: workspaceID, Id: automationID}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "DeleteFoundryAutomation"); err != nil {
			return nil, err
		}
		return DeleteAutomationOperation{req: req}, nil
	case "run_automation":
		req := &foundryv1.RunFoundryAutomationRequest{WorkspaceId: workspaceID, Id: automationID}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "RunFoundryAutomation"); err != nil {
			return nil, err
		}
		return RunAutomationOperation{req: req, workspaceID: workspaceID}, nil
	}
	return nil, invalid("unsupported Foundry operation")
}

// automationOperation is an automation operation whose call answers the automation it made or
// changed.
type automationOperation[R any] struct {
	payload     any
	req         R
	workspaceID string
	call        func(ctx context.Context, s *Service, req R, confirmationID string) (*foundryv1.FoundryAutomation, error)
}

func (o automationOperation[R]) CanonicalPayload() []byte { return mustCanonical(o.payload) }
func (o automationOperation[R]) Execute(ctx context.Context, s *Service, _ *Identity, confirmationID string) (any, error) {
	a, err := o.call(ctx, s, o.req, confirmationID)
	if err != nil {
		return nil, err
	}
	return s.FoundryAutomation(o.workspaceID, a), nil
}

// DeleteAutomationOperation deletes an automation and its runs.
type DeleteAutomationOperation struct {
	req *foundryv1.DeleteFoundryAutomationRequest
}

func (o DeleteAutomationOperation) CanonicalPayload() []byte { return []byte("{}") }
func (o DeleteAutomationOperation) Execute(ctx context.Context, s *Service, _ *Identity, _ string) (any, error) {
	if _, err := s.Actions.DeleteFoundryAutomation(ctx, o.req); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": true, "automation_id": o.req.GetId()}, nil
}

// RunAutomationOperation runs an automation now, its confirmation the run's client token, so a
// replay finds the run it queued.
type RunAutomationOperation struct {
	req         *foundryv1.RunFoundryAutomationRequest
	workspaceID string
}

func (o RunAutomationOperation) CanonicalPayload() []byte { return []byte("{}") }
func (o RunAutomationOperation) Execute(ctx context.Context, s *Service, _ *Identity, confirmationID string) (any, error) {
	req := &foundryv1.RunFoundryAutomationRequest{WorkspaceId: o.req.GetWorkspaceId(), Id: o.req.GetId(), ClientToken: confirmationID}
	resp, err := s.Actions.RunFoundryAutomation(ctx, req)
	if err != nil {
		return nil, err
	}
	return s.FoundryAutomationRun(o.workspaceID, resp.GetRun()), nil
}
