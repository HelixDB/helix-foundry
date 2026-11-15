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
