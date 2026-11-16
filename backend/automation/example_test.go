package automation_test

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/helixdb/helix-foundry/backend/automation"
)

// A spec is read once, then walked step by step as its run goes.
func ExampleSpec_Next() {
	spec, err := automation.ParseSpec([]byte(`{"v": 1,
	  "nodes": [
	    {"id": "start", "kind": "trigger", "config": {"kind": "manual"}, "position": {"x": 0, "y": 0}},
	    {"id": "sync", "kind": "action", "config": {"operation": "sync_connector", "connector_id": "01STRIPE"}, "position": {"x": 240, "y": 0}},
	    {"id": "mail", "kind": "notify", "config": {"message": "Stripe synced"}, "position": {"x": 480, "y": 0}}],
	  "edges": [{"from": "start", "to": "sync"}, {"from": "sync", "to": "mail"}]}`))
	if err != nil {
		panic(err)
	}
	states := map[string]automation.NodeState{"start": {Status: automation.NodeSucceeded}}
	fmt.Println(spec.Next(states))
	states["sync"] = automation.NodeState{Status: automation.NodeFailed}
	fmt.Println(spec.Next(states))
	states["mail"] = automation.NodeState{Status: automation.NodeBlocked}
	fmt.Println(spec.Next(states))
	// Output:
	// {sync}
	// {mail true}
	// {true}
}

// A weekly schedule fires on its day at its time, in its own zone.
func ExampleSchedule_Next() {
	zone, _ := automation.Zone("Europe/London")
	monday9 := automation.Schedule{Every: automation.EveryWeek, Weekday: time.Monday, Hour: 9, Zone: zone, ZoneName: "Europe/London"}
	fmt.Println(monday9.Next(time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)).UTC())
	// Output: 2026-11-16 09:00:00 +0000 UTC
}

// An Assistant step's prepared action runs unasked only when an approved
// action fits it: the same operation and target, its payload in bounds.
func ExampleFits() {
	approved := automation.CreateLoad{OntologyID: "01ONT"}
	fmt.Println(automation.Fits(approved, "01WS", "create_load", "ontology:01WS/01ONT", json.RawMessage(`{"version_id": "01VERSION"}`)))
	fmt.Println(automation.Fits(approved, "01WS", "create_load", "ontology:01WS/02ONT", json.RawMessage(`{"version_id": "01VERSION"}`)))
	// Output:
	// true
	// false
}

// A run, in words, is the request of the workflow runs that record its uses
// of ontologies: what fired it and each of its steps.
func ExampleRequest() {
	spec, err := automation.ParseSpec([]byte(`{"v": 1,
	  "nodes": [
	    {"id": "start", "kind": "trigger", "config": {"kind": "manual"}, "position": {"x": 0, "y": 0}},
	    {"id": "ask", "kind": "assistant", "config": {"instruction": "Are any renewals due?", "allow": [{"operation": "sync_connector", "connector_id": "01STRIPE"}]}, "position": {"x": 1, "y": 0}},
	    {"id": "due", "kind": "condition", "config": {"check": "answer", "equals": "yes"}, "position": {"x": 2, "y": 0}},
	    {"id": "count", "kind": "condition", "config": {"check": "count", "op": "at_least", "value": 2}, "position": {"x": 2, "y": 1}},
	    {"id": "load", "kind": "action", "config": {"operation": "create_load", "ontology_id": "01SALES"}, "position": {"x": 3, "y": 0}},
	    {"id": "pub", "kind": "action", "config": {"operation": "publish_dashboard", "dashboard_id": "01DASH"}, "position": {"x": 3, "y": 1}},
	    {"id": "mail", "kind": "notify", "config": {"message": "Renewals"}, "position": {"x": 4, "y": 0}},
	    {"id": "quiet", "kind": "notify", "config": {"message": ""}, "position": {"x": 4, "y": 1}}],
	  "edges": [{"from": "start", "to": "ask"}, {"from": "ask", "to": "due"}, {"from": "ask", "to": "count"},
	            {"from": "due", "to": "load", "branch": "true"}, {"from": "count", "to": "pub", "branch": "false"},
	            {"from": "load", "to": "mail"}, {"from": "pub", "to": "quiet"}]}`))
	if err != nil {
		panic(err)
	}
	fmt.Println(automation.Request("Renewals", "Run by hand.", spec, map[string]string{"01SALES": "Sales", "01STRIPE": "Stripe"}))
	// Output:
	// Run by hand. The automation "Renewals" ran these steps, in order:
	// 1. Assistant step "ask": Are any renewals due? (It may sync the connector "Stripe" unasked.)
	// 2. Condition "due": go on one way if the step before answered yes, the other way if not.
	// 3. Condition "count": go on one way if the step before counted at least 2, the other way if not.
	// 4. Action step "load": load the current version of the ontology "Sales".
	// 5. Action step "pub": publish the dashboard 01DASH again at its link.
	// 6. Notify step "mail": email its owner "Renewals", with what the Assistant steps before it said.
	// 7. Notify step "quiet": email its owner, with what the Assistant steps before it said.
}

// The steps' targets leave out what the trigger listens to.
func ExampleSpec_StepTargets() {
	spec, err := automation.ParseSpec([]byte(`{"v": 1,
	  "nodes": [
	    {"id": "start", "kind": "trigger", "config": {"kind": "generation_finished", "ontology_id": "01DRAFTED", "outcome": "any"}, "position": {"x": 0, "y": 0}},
	    {"id": "load", "kind": "action", "config": {"operation": "create_load", "ontology_id": "01SALES"}, "position": {"x": 1, "y": 0}}],
	  "edges": [{"from": "start", "to": "load"}]}`))
	if err != nil {
		panic(err)
	}
	fmt.Println(spec.Targets().Ontologies, spec.StepTargets().Ontologies)
	// Output: [01DRAFTED 01SALES] [01SALES]
}
