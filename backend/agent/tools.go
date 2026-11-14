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
// cancel_question (they change things without a confirmation), the workflow
// tools (an outside agent's records), get_sign_in, and helix_list_workspaces
// (a turn is of one workspace).
var Tools = map[string]bool{
	"helix_foundry_list_ontologies":       true,
	"helix_foundry_get_ontology":          true,
	"helix_foundry_get_version":           true,
	"helix_foundry_get_context":           true,
	"helix_foundry_read_guide":            true,
	"helix_foundry_get_generation":        true,
	"helix_foundry_get_load":              true,
	"helix_foundry_get_sync":              true,
	"helix_foundry_list_documents":        true,
	"helix_foundry_list_connectors":       true,
	"helix_foundry_list_connector_kinds":  true,
	"helix_foundry_list_review_decisions": true,
	"helix_foundry_read_graph":            true,
	"helix_foundry_ask":                   true,
	"helix_foundry_get_question":          true,
	"helix_foundry_list_questions":        true,
	// Link-only: a session's Service offers no sign-in, so it answers with
	// the site's /data?add=<kind>, where the person enters credentials.
	"helix_foundry_connect": true,
	// The host's, which create_ontology needs.
	"helix_list_projects":  true,
	"helix_list_databases": true,
	// Prepare only: its person runs what it prepares (integration.ConfirmByPerson).
	toolPrepare: true,
}

const (
	toolPrepare = "helix_foundry_prepare_operation"
	toolAsk     = "helix_foundry_ask"
)

// Operations are the actions the Assistant may prepare: create_load for now.
// The sink refuses every other before it is checked or confirmed.
var Operations = map[string]bool{
	"create_load": true,
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
