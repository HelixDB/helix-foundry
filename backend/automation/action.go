package automation

import (
	"encoding/json"
	"fmt"
	"slices"
)

// Action is one operation of Foundry's prepare tool with its target fixed, as
// an owner approves it for an automation: a closed set. Its payload is
// bounded by its kind: sync_connector's is empty; create_load's names a
// version of the target ontology (any: an Assistant step's is the one it
// chose, an action step's the ontology's current version when it runs);
// publish_dashboard's names the dashboard's revision (an action step's is
// the one it has when it runs), which refreshes its public snapshot.
type Action interface {
	// Operation is its operation's name, as the prepare tool takes it.
	Operation() string
	// Target is its target in workspaceID, as the prepare tool takes it.
	Target(workspaceID string) string
}

// SyncConnector syncs one connector.
type SyncConnector struct{ ConnectorID string }

// CreateLoad loads a version of one ontology into its database.
type CreateLoad struct{ OntologyID string }

// PublishDashboard publishes one dashboard's snapshot again, at its link.
type PublishDashboard struct{ DashboardID string }

func (SyncConnector) Operation() string    { return "sync_connector" }
func (CreateLoad) Operation() string       { return "create_load" }
func (PublishDashboard) Operation() string { return "publish_dashboard" }

func (a SyncConnector) Target(workspaceID string) string { return "connector:" + workspaceID + "/" + a.ConnectorID }
func (a CreateLoad) Target(workspaceID string) string    { return "ontology:" + workspaceID + "/" + a.OntologyID }
func (a PublishDashboard) Target(workspaceID string) string {
	return "dashboard:" + workspaceID + "/" + a.DashboardID
}

// Operations are the operations an owner may approve for an automation.
var Operations = map[string]bool{"sync_connector": true, "create_load": true, "publish_dashboard": true}

// Fits reports whether a prepared action is a, in workspaceID: the same
// operation on the same target, with a payload a's kind bounds. The prepare
// tool has checked the payload's values already (a version of the target
// ontology, a revision of the target dashboard) and authorized its caller;
// Fits decides only whether the owner approved it.
//
//	automation.Fits(automation.CreateLoad{OntologyID: "01ONT"}, "01WS", "create_load", "ontology:01WS/01ONT",
//		json.RawMessage(`{"version_id": "01VERSION"}`)) // true
func Fits(a Action, workspaceID, operation, target string, payload json.RawMessage) bool {
	if a.Operation() != operation || a.Target(workspaceID) != target {
		return false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(payload, &fields) != nil || fields == nil {
		return false
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	switch a.(type) {
	case SyncConnector:
		return len(keys) == 0
	case CreateLoad:
		var version string
		return slices.Equal(keys, []string{"version_id"}) && json.Unmarshal(fields["version_id"], &version) == nil && version != ""
	case PublishDashboard:
		var revision int64
		return slices.Equal(keys, []string{"expected_revision"}) && json.Unmarshal(fields["expected_revision"], &revision) == nil && revision > 0
	}
	return false
}

type wireAction struct {
	Operation   string `json:"operation"`
	ConnectorID string `json:"connector_id,omitempty"`
	OntologyID  string `json:"ontology_id,omitempty"`
	DashboardID string `json:"dashboard_id,omitempty"`
}

// ParseAction reads an action as a spec writes it: {operation:
// "sync_connector", connector_id}, {operation: "create_load", ontology_id} or
// {operation: "publish_dashboard", dashboard_id}.
func ParseAction(raw json.RawMessage) (Action, error) {
	wrong := fmt.Errorf(`an action is {operation: "sync_connector", connector_id}, {operation: "create_load", ontology_id} or {operation: "publish_dashboard", dashboard_id}`)
	var w wireAction
	if err := decodeStrict(raw, &w); err != nil {
		return nil, fmt.Errorf("%w: %w", wrong, err)
	}
	switch {
	case w.Operation == "sync_connector" && w == (wireAction{Operation: w.Operation, ConnectorID: w.ConnectorID}) && idPattern.MatchString(w.ConnectorID):
		return SyncConnector{ConnectorID: w.ConnectorID}, nil
	case w.Operation == "create_load" && w == (wireAction{Operation: w.Operation, OntologyID: w.OntologyID}) && idPattern.MatchString(w.OntologyID):
		return CreateLoad{OntologyID: w.OntologyID}, nil
	case w.Operation == "publish_dashboard" && w == (wireAction{Operation: w.Operation, DashboardID: w.DashboardID}) && idPattern.MatchString(w.DashboardID):
		return PublishDashboard{DashboardID: w.DashboardID}, nil
	}
	return nil, wrong
}

func wireActionOf(a Action) wireAction {
	switch a := a.(type) {
	case SyncConnector:
		return wireAction{Operation: a.Operation(), ConnectorID: a.ConnectorID}
	case CreateLoad:
		return wireAction{Operation: a.Operation(), OntologyID: a.OntologyID}
	case PublishDashboard:
		return wireAction{Operation: a.Operation(), DashboardID: a.DashboardID}
	}
	panic(fmt.Sprintf("automation: no wire form of %T", a))
}
