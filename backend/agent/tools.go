package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// Tools is every tool the Assistant may offer its model: a closed list,
// intersected with what the user's identity gets. Everything else a session
// registers is removed before the model sees the list, so a tool added to
// the MCP stays out of the Assistant until it is named here.
//
// Left out on purpose: execute_operation and complete_document_upload (only
// the person runs an action; uploads stay in the browser), send_pieces and
// cancel_question (they change things without a confirmation), the use case
// tools (an outside agent's records), get_sign_in, and helix_list_workspaces
// (a turn is of one workspace).
var Tools = map[string]bool{
	"helix_foundry_list_ontologies":      true,
	"helix_foundry_get_ontology":         true,
	"helix_foundry_get_version":          true,
	"helix_foundry_get_context":          true,
	"helix_foundry_read_guide":           true,
	"helix_foundry_get_generation":       true,
	"helix_foundry_get_load":             true,
	"helix_foundry_get_sync":             true,
	"helix_foundry_list_documents":       true,
	"helix_foundry_list_connectors":      true,
	"helix_foundry_list_connector_kinds": true,
	"helix_foundry_get_ai_settings":      true,
	// A local install's only: where the Assistant runs, and its helpers.
	"helix_foundry_get_assistant_runtime": true,
	"helix_foundry_list_review_decisions": true,
	"helix_foundry_list_reviews":          true,
	"helix_foundry_read_graph":            true,
	"helix_foundry_ask":                   true,
	"helix_foundry_get_question":          true,
	"helix_foundry_list_questions":        true,
	// Dashboards: read, and a tile's query tried before it is proposed.
	"helix_foundry_list_dashboards":        true,
	"helix_foundry_get_dashboard":          true,
	"helix_foundry_preview_dashboard_tile": true,
	// Workflows: read, and their runs.
	"helix_foundry_list_workflows":     true,
	"helix_foundry_get_workflow":       true,
	"helix_foundry_list_workflow_runs": true,
	"helix_foundry_get_workflow_run":   true,
	// Link-only: a session's Service offers no sign-in, so it answers with
	// the site's /data?add=<kind>, where the person enters credentials.
	"helix_foundry_connect": true,
	// The host's, which create_ontology needs.
	"helix_list_projects":  true,
	"helix_list_databases": true,
	// Prepare only: its person runs what it prepares (integration.ConfirmByPerson).
	toolPrepare: true,
	// A workflow's turn's report, registered for those turns alone.
	toolReport: true,
	// Asks the person, for a person's turns alone, and ends the turn.
	toolAskPerson: true,
}

// GranteeTools are the tools a restricted member's turn offers (ReachGrants),
// within Tools: reads and asks whose every call the API narrows to their
// grants, and toolAskPerson. Left out: documents (a grantee reads one only by
// id), generations, syncs, connectors, the AI setting, the runtime, reviews,
// a tile's preview, automations, connect, prepare, the report, and the host's
// projects and databases.
var GranteeTools = map[string]bool{
	"helix_foundry_list_ontologies": true,
	"helix_foundry_get_ontology":    true,
	"helix_foundry_get_context":     true,
	"helix_foundry_get_version":     true,
	"helix_foundry_get_load":        true,
	"helix_foundry_read_guide":      true,
	"helix_foundry_read_graph":      true,
	"helix_foundry_ask":             true,
	"helix_foundry_get_question":    true,
	"helix_foundry_list_questions":  true,
	"helix_foundry_list_dashboards": true,
	"helix_foundry_get_dashboard":   true,
	toolAskPerson:                   true,
}

const (
	toolPrepare     = "helix_foundry_prepare_operation"
	toolAsk         = "helix_foundry_ask"
	toolGetQuestion = "helix_foundry_get_question"
)

// Operations are the actions the Assistant may prepare, each drawn as a card
// (describe). The sink refuses every other before it is checked or
// confirmed: upload_document (the person's browser holds the file),
// create_event_source (an API source is set up with its send tokens, in
// Foundry), answer_generation_questions (the person sends a relayed
// generation's answers themselves, from the questions in the dock) and
// pair_assistant_helper (its result is a credential, which
// would pass through the model's provider). publish_dashboard and
// unpublish_dashboard are prepared only where the host offers public links.
var Operations = map[string]bool{
	"create_ontology":     true,
	"generate_ontology":   true,
	"set_current_version": true,
	"create_load":         true,
	"set_current_load":    true,
	"revise_version":      true,
	"sync_connector":      true,
	"add_sample_data":     true,
	"create_document_set": true,
	"update_document_set": true,
	"move_document":       true,
	"settle_review":       true,
	"create_dashboard":    true,
	"update_dashboard":    true,
	"delete_dashboard":    true,
	"pin_dashboard":       true,
	"publish_dashboard":   true,
	"unpublish_dashboard": true,
	// Workflows: the person's click on a create or an edit approves the
	// spec its card showed, and nothing when it showed none (the API's
	// ExecuteFoundryAgentAction).
	"create_workflow": true,
	"update_workflow": true,
	"pause_workflow":  true,
	"resume_workflow": true,
	"delete_workflow": true,
	"run_workflow":    true,
	// A local install's only (Service.AgentHosts).
	"set_assistant_runtime":   true,
	"revoke_assistant_helper": true,
}

// askClientToken is the client token every ask of a turn's tool call sends,
// whatever the model wrote: a replayed attempt asks the same question again
// rather than a second one.
func askClientToken(turnID, toolUseID string) string {
	sum := sha256.Sum256([]byte(turnID + "\x00" + toolUseID))
	return hex.EncodeToString(sum[:])
}

// withAskClientToken is input with its client_token forced, for the ask tool.
func withAskClientToken(input json.RawMessage, turnID, toolUseID string) (json.RawMessage, error) {
	var args map[string]any
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, err
	}
	if args == nil {
		args = map[string]any{}
	}
	args["client_token"] = askClientToken(turnID, toolUseID)
	return json.Marshal(args)
}
