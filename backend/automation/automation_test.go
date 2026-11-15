package automation_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/automation"
)

const weekly = `{"kind":"schedule","every":"week","weekday":"monday","at":"09:00","time_zone":"Europe/London"}`

func node(id, kind, config string) string {
	return fmt.Sprintf(`{"id":%q,"kind":%q,"config":%s,"position":{"x":0,"y":0}}`, id, kind, config)
}

func spec(nodes []string, edges string) string {
	return fmt.Sprintf(`{"v":1,"nodes":[%s],"edges":%s}`, strings.Join(nodes, ","), edges)
}

// overdue is the Assistant's own example: every Monday at 9, sync Stripe, ask
// about overdue invoices, and email the owner when there are any.
var overdue = spec([]string{
	node("start", "trigger", weekly),
	node("sync", "action", `{"operation":"sync_connector","connector_id":"01STRIPE"}`),
	node("ask", "assistant", `{"instruction":" Count the overdue invoices. ","allow":[{"operation":"create_load","ontology_id":"01ONT"}]}`),
	node("any", "condition", `{"check":"count","op":"at_least","value":1}`),
	node("mail", "notify", `{"message":"Overdue invoices this week"}`),
}, `[{"from":"start","to":"sync"},{"from":"sync","to":"ask"},{"from":"ask","to":"any"},{"from":"any","to":"mail","branch":"true"}]`)

func TestParseSpecRoundTrips(t *testing.T) {
	s, err := automation.ParseSpec([]byte(overdue))
	require.NoError(t, err)
	require.Equal(t, []string{"start", "sync", "ask", "any", "mail"}, s.Order())
	require.Equal(t, "start", s.TriggerID())
	trigger, ok := s.Trigger().(automation.Schedule)
	require.True(t, ok)
	require.Equal(t, automation.EveryWeek, trigger.Every)
	require.Equal(t, time.Monday, trigger.Weekday)
	require.Equal(t, 9, trigger.Hour)
	ask, _ := s.Node("ask")
	require.Equal(t, automation.AssistantNode{Instruction: "Count the overdue invoices.", Allow: []automation.Action{automation.CreateLoad{OntologyID: "01ONT"}}},
		ask.Config, "instructions are trimmed")
	_, ok = s.Node("missing")
	require.False(t, ok)

	raw, err := json.Marshal(s)
	require.NoError(t, err)
	again, err := automation.ParseSpec(raw)
	require.NoError(t, err)
	rawAgain, err := json.Marshal(again)
	require.NoError(t, err)
	require.Equal(t, string(raw), string(rawAgain), "canonical: the same spec, the same bytes")
	require.Equal(t, automation.Targets{Connectors: []string{"01STRIPE"}, Ontologies: []string{"01ONT"}}, s.Targets())
	require.Equal(t, []string{"start", "sync", "ask"}, s.Ancestors("any"))
	require.Len(t, s.Edges(), 4)
	require.Len(t, s.Nodes(), 5)
}

func TestParseSpecRoundTripsEveryKind(t *testing.T) {
	triggers := []string{
		`{"kind":"schedule","every":"hour","minute":0,"time_zone":"UTC"}`,
		`{"kind":"schedule","every":"day","at":"23:59","time_zone":"America/New_York"}`,
		weekly,
		`{"kind":"sync_finished","connector_id":"01C","outcome":"any"}`,
		`{"kind":"load_ready","ontology_id":"01O"}`,
		`{"kind":"document_arrived","set_id":"01S"}`,
		`{"kind":"generation_finished","ontology_id":"01O","outcome":"failed"}`,
		`{"kind":"manual"}`,
	}
	for _, trigger := range triggers {
		raw := spec([]string{node("t", "trigger", trigger),
			node("a", "action", `{"operation":"publish_dashboard","dashboard_id":"01D"}`),
			node("c", "condition", `{"check":"answer","equals":"no"}`),
			node("ai", "assistant", `{"instruction":"Look"}`),
			node("n", "notify", `{"message":""}`)},
			`[{"from":"t","to":"a"},{"from":"t","to":"ai"},{"from":"ai","to":"c"},{"from":"c","to":"n","branch":"false"}]`)
		s, err := automation.ParseSpec([]byte(raw))
		require.NoError(t, err, trigger)
		out, err := json.Marshal(s)
		require.NoError(t, err)
		require.Contains(t, string(out), trigger, "the trigger is written back as given")
	}
}

func TestParseSpecRefuses(t *testing.T) {
	start, act := node("start", "trigger", `{"kind":"manual"}`), node("act", "action", `{"operation":"sync_connector","connector_id":"01C"}`)
	ask := node("ask", "assistant", `{"instruction":"Is it late?"}`)
	cond := node("if", "condition", `{"check":"answer","equals":"yes"}`)
	for _, c := range []struct{ name, raw, want string }{
		{"not JSON", `{`, "not {v, nodes, edges}"},
		{"unknown field", `{"v":1,"nodes":[],"edges":[],"x":1}`, "not {v, nodes, edges}"},
		{"version", `{"v":2,"nodes":[],"edges":[]}`, "v is 2"},
		{"no step", spec([]string{start}, `[]`), "at least one step"},
		{"too large", `{"v":1,"pad":"` + strings.Repeat("x", automation.MaxSpecBytes) + `"}`, "at most 64 KiB"},
		{"too many nodes", spec(append([]string{start}, func() []string {
			var out []string
			for i := range automation.MaxNodes {
				out = append(out, node(fmt.Sprintf("n%d", i), "notify", `{"message":"hi"}`))
			}
			return out
		}()...), `[]`), "at most 50 nodes"},
		{"too many edges", spec([]string{start, act}, "["+strings.TrimSuffix(strings.Repeat(`{"from":"start","to":"act"},`, automation.MaxEdges+1), ",")+"]"), "at most 100 edges"},
		{"bad id", spec([]string{start, node("Act", "notify", `{"message":""}`)}, `[]`), `node id "Act"`},
		{"duplicate id", spec([]string{start, act, act}, `[]`), "two nodes have the id act"},
		{"position", spec([]string{start, `{"id":"far","kind":"notify","config":{"message":""},"position":{"x":1e9,"y":0}}`}, `[]`), "off the canvas"},
		{"unknown kind", spec([]string{start, node("x", "webhook", `{}`)}, `[]`), `kind is "webhook"`},
		{"two triggers", spec([]string{start, node("again", "trigger", `{"kind":"manual"}`)}, `[]`), "exactly one trigger, not 2"},
		{"no trigger", spec([]string{act, ask}, `[]`), "exactly one trigger, not 0"},
		{"edge from nowhere", spec([]string{start, act}, `[{"from":"x","to":"act"}]`), `comes from "x"`},
		{"edge to nowhere", spec([]string{start, act}, `[{"from":"start","to":"x"}]`), `goes to "x"`},
		{"self edge", spec([]string{start, act}, `[{"from":"start","to":"act"},{"from":"act","to":"act"}]`), "edge to itself"},
		{"into trigger", spec([]string{start, act}, `[{"from":"start","to":"act"},{"from":"act","to":"start"}]`), "no edge goes into it"},
		{"duplicate edge", spec([]string{start, act}, `[{"from":"start","to":"act"},{"from":"start","to":"act"}]`), "two edges go"},
		{"branch off condition", spec([]string{start, act}, `[{"from":"start","to":"act","branch":"true"}]`), "only a condition's edges"},
		{"condition without branch", spec([]string{start, ask, cond, act}, `[{"from":"start","to":"ask"},{"from":"ask","to":"if"},{"from":"if","to":"act"}]`),
			"its true or its false branch"},
		{"two true branches", spec([]string{start, ask, cond, act, node("act2", "notify", `{"message":""}`)},
			`[{"from":"start","to":"ask"},{"from":"ask","to":"if"},{"from":"if","to":"act","branch":"true"},{"from":"if","to":"act2","branch":"true"}]`),
			"2 true branches"},
		{"condition with two in", spec([]string{start, ask, cond, act},
			`[{"from":"start","to":"ask"},{"from":"ask","to":"if"},{"from":"start","to":"if"},{"from":"if","to":"act","branch":"true"}]`),
			"exactly one edge goes into it, not 2"},
		{"answer of an action", spec([]string{start, act, cond}, `[{"from":"start","to":"act"},{"from":"act","to":"if"}]`), "an answer is an Assistant step's"},
		{"count of a manual trigger", spec([]string{start, node("if", "condition", `{"check":"count","op":"equals","value":0}`)}, `[{"from":"start","to":"if"}]`),
			"a count is"},
		{"cycle", spec([]string{start, ask, act}, `[{"from":"start","to":"ask"},{"from":"ask","to":"act"},{"from":"act","to":"ask"}]`), "in a cycle"},
		{"unreached", spec([]string{start, act, ask}, `[{"from":"start","to":"act"}]`), "ask isn't reached"},
		{"unreached cycle", spec([]string{start, act, ask, node("n", "notify", `{"message":""}`)},
			`[{"from":"start","to":"act"},{"from":"ask","to":"n"},{"from":"n","to":"ask"}]`), "isn't reached"},
		{"empty instruction", spec([]string{start, node("ai", "assistant", `{"instruction":"  "}`)}, `[{"from":"start","to":"ai"}]`), "its instruction"},
		{"control in instruction", spec([]string{start, node("ai", "assistant", `{"instruction":"a\u0000b"}`)}, `[{"from":"start","to":"ai"}]`), "control"},
		{"long message", spec([]string{start, node("n", "notify", fmt.Sprintf(`{"message":%q}`, strings.Repeat("m", automation.MaxMessageChars+1)))}, `[{"from":"start","to":"n"}]`),
			"its message"},
		{"assistant fields", spec([]string{start, node("ai", "assistant", `{"instruction":"x","model":"y"}`)}, `[{"from":"start","to":"ai"}]`), "{instruction, allow}"},
		{"notify fields", spec([]string{start, node("n", "notify", `{"to":"x"}`)}, `[{"from":"start","to":"n"}]`), "{message}"},
		{"allow twice", spec([]string{start, node("ai", "assistant",
			`{"instruction":"x","allow":[{"operation":"create_load","ontology_id":"O"},{"operation":"create_load","ontology_id":"O"}]}`)}, `[{"from":"start","to":"ai"}]`),
			"twice"},
		{"allow too many", spec([]string{start, node("ai", "assistant", `{"instruction":"x","allow":[`+strings.TrimSuffix(strings.Repeat(
			`{"operation":"sync_connector","connector_id":"C"},`, automation.MaxAllowed+1), ",")+`]}`)}, `[{"from":"start","to":"ai"}]`), "at most 10"},
		{"allow other operation", spec([]string{start, node("ai", "assistant", `{"instruction":"x","allow":[{"operation":"delete_dashboard","dashboard_id":"D"}]}`)},
			`[{"from":"start","to":"ai"}]`), "allow: an action is"},
		{"action fields", spec([]string{start, node("a", "action", `{"operation":"create_load","ontology_id":"O","connector_id":"C"}`)}, `[{"from":"start","to":"a"}]`),
			"an action is"},
		{"action id", spec([]string{start, node("a", "action", `{"operation":"create_load","ontology_id":"a b"}`)}, `[{"from":"start","to":"a"}]`), "an action is"},
		{"condition shape", spec([]string{start, ask, node("if", "condition", `{"check":"answer","equals":"maybe"}`)}, `[{"from":"start","to":"ask"},{"from":"ask","to":"if"}]`),
			"a condition is"},
		{"negative count", spec([]string{start, ask, node("if", "condition", `{"check":"count","op":"at_least","value":-1}`)}, `[{"from":"start","to":"ask"},{"from":"ask","to":"if"}]`),
			"a condition is"},
		{"unknown field in condition", spec([]string{start, ask, node("if", "condition", `{"check":"count","op":"at_least","value":1,"x":1}`)},
			`[{"from":"start","to":"ask"},{"from":"ask","to":"if"}]`), "a condition is"},
		{"config not object", spec([]string{node("start", "trigger", `[]`), act}, `[{"from":"start","to":"act"}]`), "config is a JSON object"},
		{"trigger kind", spec([]string{node("start", "trigger", `{"kind":"cron","expr":"* * * * *"}`), act}, `[{"from":"start","to":"act"}]`), `kind is "cron"`},
		{"trigger missing config", spec([]string{`{"id":"start","kind":"trigger","position":{"x":0,"y":0}}`, act}, `[]`), "config is a JSON object"},
		{"sync on its own sync", spec([]string{node("start", "trigger", `{"kind":"sync_finished","connector_id":"01C","outcome":"any"}`), act},
			`[{"from":"start","to":"act"}]`), "act syncs the connector its trigger waits on"},
		{"load on its own load", spec([]string{node("start", "trigger", `{"kind":"load_ready","ontology_id":"O"}`),
			node("ai", "assistant", `{"instruction":"x","allow":[{"operation":"create_load","ontology_id":"O"}]}`)}, `[{"from":"start","to":"ai"}]`),
			"ai loads the ontology its trigger waits on"},
	} {
		_, err := automation.ParseSpec([]byte(c.raw))
		require.ErrorContains(t, err, c.want, c.name)
	}
}

func TestParseTriggerRefuses(t *testing.T) {
	for _, c := range []struct{ trigger, want string }{
		{`{"kind":"schedule","every":"hour","time_zone":"UTC"}`, "minute: 0 to 59"},
		{`{"kind":"schedule","every":"hour","minute":60,"time_zone":"UTC"}`, "minute: 0 to 59"},
		{`{"kind":"schedule","every":"hour","minute":5,"at":"09:00","time_zone":"UTC"}`, "minute: 0 to 59"},
		{`{"kind":"schedule","every":"day","at":"24:00","time_zone":"UTC"}`, `"HH:MM"`},
		{`{"kind":"schedule","every":"day","at":"9:00","time_zone":"UTC"}`, `"HH:MM"`},
		{`{"kind":"schedule","every":"day","at":"09:00","weekday":"monday","time_zone":"UTC"}`, `"HH:MM"`},
		{`{"kind":"schedule","every":"week","at":"09:00","time_zone":"UTC"}`, `"HH:MM"`},
		{`{"kind":"schedule","every":"week","weekday":"someday","at":"09:00","time_zone":"UTC"}`, `"HH:MM"`},
		{`{"kind":"schedule","every":"day","at":"09:00","minute":1,"time_zone":"UTC"}`, `"HH:MM"`},
		{`{"kind":"schedule","every":"month","at":"09:00","time_zone":"UTC"}`, `"HH:MM"`},
		{`{"kind":"schedule","every":"day","at":"09:00","time_zone":"Mars/Olympus"}`, "not an IANA time zone"},
		{`{"kind":"schedule","every":"day","at":"09:00","time_zone":"Local"}`, "not an IANA time zone"},
		{`{"kind":"schedule","every":"day","at":"09:00"}`, "not an IANA time zone"},
		{`{"kind":"schedule","every":"day","at":"09:00","time_zone":"UTC","set_id":"S"}`, `"HH:MM"`},
		{`{"kind":"sync_finished","connector_id":"C"}`, "outcome"},
		{`{"kind":"sync_finished","connector_id":"C","outcome":"sometimes"}`, "outcome"},
		{`{"kind":"sync_finished","connector_id":"C","outcome":"any","set_id":"S"}`, "outcome"},
		{`{"kind":"load_ready"}`, "ontology_id"},
		{`{"kind":"load_ready","ontology_id":"O","outcome":"any"}`, "ontology_id"},
		{`{"kind":"document_arrived","set_id":""}`, "set_id"},
		{`{"kind":"generation_finished","ontology_id":"O"}`, "outcome"},
		{`{"kind":"manual","time_zone":"UTC"}`, `{kind: "manual"}`},
		{`{"kind":"manual","other":1}`, `{kind: "manual"}`},
		{`{"kind":3}`, "names its kind"},
	} {
		raw := spec([]string{node("start", "trigger", c.trigger), node("n", "notify", `{"message":""}`)}, `[{"from":"start","to":"n"}]`)
		_, err := automation.ParseSpec([]byte(raw))
		require.ErrorContains(t, err, c.want, c.trigger)
	}
}

func TestZone(t *testing.T) {
	loc, err := automation.Zone("Asia/Kolkata")
	require.NoError(t, err)
	require.Equal(t, "Asia/Kolkata", loc.String())
	for _, bad := range []string{"", "Local", "Nowhere/City", "Europe/ London", strings.Repeat("A", 65)} {
		_, err := automation.Zone(bad)
		require.Error(t, err, bad)
	}
}

func schedule(t *testing.T, trigger string) automation.Schedule {
	t.Helper()
	s, err := automation.ParseSpec([]byte(spec([]string{node("start", "trigger", trigger), node("n", "notify", `{"message":""}`)},
		`[{"from":"start","to":"n"}]`)))
	require.NoError(t, err)
	return s.Trigger().(automation.Schedule)
}

func TestScheduleNext(t *testing.T) {
	london, _ := time.LoadLocation("Europe/London")
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, london)
		require.NoError(t, err)
		return v
	}
	hourly := schedule(t, `{"kind":"schedule","every":"hour","minute":15,"time_zone":"Europe/London"}`)
	require.Equal(t, at("2026-11-16 10:15"), hourly.Next(at("2026-11-16 10:00")))
	require.Equal(t, at("2026-11-16 11:15"), hourly.Next(at("2026-11-16 10:15")), "strictly after")
	require.Equal(t, at("2026-11-17 00:15"), hourly.Next(at("2026-11-16 23:30")))

	daily := schedule(t, `{"kind":"schedule","every":"day","at":"09:30","time_zone":"Europe/London"}`)
	require.Equal(t, at("2026-11-16 09:30"), daily.Next(at("2026-11-16 08:00")))
	require.Equal(t, at("2026-11-17 09:30"), daily.Next(at("2026-11-16 09:30")))
	// Across the spring change (29 March 2026), 09:30 stays 09:30 on the wall.
	require.Equal(t, at("2026-03-29 09:30"), daily.Next(at("2026-03-28 10:00")))
	require.Equal(t, 23*time.Hour, daily.Next(at("2026-03-28 10:00")).Sub(at("2026-03-28 09:30")).Round(time.Hour))

	weeklySchedule := schedule(t, weekly)
	require.Equal(t, at("2026-11-16 09:00"), weeklySchedule.Next(at("2026-11-14 12:00")), "Saturday to Monday")
	require.Equal(t, at("2026-11-16 09:00"), weeklySchedule.Next(at("2026-11-16 08:59")))
	require.Equal(t, at("2026-11-23 09:00"), weeklySchedule.Next(at("2026-11-16 09:00")), "a week on from the fire")
	require.Equal(t, at("2026-11-23 09:00"), weeklySchedule.Next(at("2026-11-17 09:00")))

	// The fire is in the owner's zone, whatever the caller's.
	tokyo := schedule(t, `{"kind":"schedule","every":"day","at":"09:00","time_zone":"Asia/Tokyo"}`)
	require.Equal(t, time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC), tokyo.Next(time.Date(2026, 11, 15, 1, 0, 0, 0, time.UTC)).UTC())

	// A time the spring change skips (01:30 on 29 March in London) fires once that day.
	skipped := schedule(t, `{"kind":"schedule","every":"day","at":"01:30","time_zone":"Europe/London"}`)
	next := skipped.Next(at("2026-03-29 00:00"))
	require.Equal(t, 29, next.In(london).Day())
	require.True(t, next.After(at("2026-03-29 00:00")))
}

func TestFits(t *testing.T) {
	load := automation.CreateLoad{OntologyID: "01ONT"}
	require.True(t, automation.Fits(load, "W", "create_load", "ontology:W/01ONT", json.RawMessage(`{"version_id":"01V"}`)))
	for _, c := range []struct {
		name, operation, target, payload string
	}{
		{"another operation", "set_current_load", "ontology:W/01ONT", `{"version_id":"01V"}`},
		{"another target", "create_load", "ontology:W/02ONT", `{"version_id":"01V"}`},
		{"another workspace", "create_load", "ontology:X/01ONT", `{"version_id":"01V"}`},
		{"more fields", "create_load", "ontology:W/01ONT", `{"version_id":"01V","x":1}`},
		{"no version", "create_load", "ontology:W/01ONT", `{}`},
		{"empty version", "create_load", "ontology:W/01ONT", `{"version_id":""}`},
		{"version not a string", "create_load", "ontology:W/01ONT", `{"version_id":3}`},
		{"not an object", "create_load", "ontology:W/01ONT", `[]`},
		{"null", "create_load", "ontology:W/01ONT", `null`},
	} {
		require.False(t, automation.Fits(load, "W", c.operation, c.target, json.RawMessage(c.payload)), c.name)
	}
	sync := automation.SyncConnector{ConnectorID: "01C"}
	require.True(t, automation.Fits(sync, "W", "sync_connector", "connector:W/01C", json.RawMessage(`{}`)))
	require.False(t, automation.Fits(sync, "W", "sync_connector", "connector:W/01C", json.RawMessage(`{"full":true}`)))
	publish := automation.PublishDashboard{DashboardID: "01D"}
	require.True(t, automation.Fits(publish, "W", "publish_dashboard", "dashboard:W/01D", json.RawMessage(`{"expected_revision":4}`)))
	require.False(t, automation.Fits(publish, "W", "publish_dashboard", "dashboard:W/01D", json.RawMessage(`{"expected_revision":0}`)))
	require.False(t, automation.Fits(publish, "W", "publish_dashboard", "dashboard:W/01D", json.RawMessage(`{"expected_revision":"4"}`)))
	for op := range automation.Operations {
		_, err := automation.ParseAction(json.RawMessage(fmt.Sprintf(`{"operation":%q}`, op)))
		require.Error(t, err, "%s needs its target", op)
	}
}

func TestChecksEvaluate(t *testing.T) {
	yes, no, three := true, false, int64(3)
	ok, err := automation.AnswerIs{Yes: true}.Evaluate(automation.Output{Answer: &yes})
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = automation.AnswerIs{Yes: true}.Evaluate(automation.Output{Answer: &no})
	require.NoError(t, err)
	require.False(t, ok)
	ok, err = automation.AnswerIs{Yes: false}.Evaluate(automation.Output{Answer: &no})
	require.NoError(t, err)
	require.True(t, ok)
	_, err = automation.AnswerIs{Yes: true}.Evaluate(automation.Output{})
	require.ErrorContains(t, err, "no yes-or-no answer")
	for _, c := range []struct {
		op    automation.CountOp
		value int64
		want  bool
	}{
		{automation.AtLeast, 3, true}, {automation.AtLeast, 4, false},
		{automation.AtMost, 3, true}, {automation.AtMost, 2, false},
		{automation.Equals, 3, true}, {automation.Equals, 0, false},
	} {
		got, err := automation.CountIs{Op: c.op, Value: c.value}.Evaluate(automation.Output{Count: &three})
		require.NoError(t, err)
		require.Equal(t, c.want, got, "%s %d", c.op, c.value)
	}
	_, err = automation.CountIs{Op: automation.AtLeast}.Evaluate(automation.Output{})
	require.ErrorContains(t, err, "no count")
}

func succeeded(out automation.Output) automation.NodeState {
	return automation.NodeState{Status: automation.NodeSucceeded, Output: out}
}

func TestNextWalksBranches(t *testing.T) {
	s, err := automation.ParseSpec([]byte(overdue))
	require.NoError(t, err)
	states := map[string]automation.NodeState{"start": succeeded(automation.Output{})}
	require.Equal(t, automation.RunNode{ID: "sync"}, s.Next(states))
	states["sync"] = automation.NodeState{Status: automation.NodeRunning}
	require.Equal(t, automation.WaitNode{ID: "sync"}, s.Next(states))
	states["sync"] = succeeded(automation.Output{})
	require.Equal(t, automation.RunNode{ID: "ask"}, s.Next(states))
	states["ask"] = succeeded(automation.Output{})
	require.Equal(t, automation.RunNode{ID: "any"}, s.Next(states))

	falseBranch := false
	states["any"] = succeeded(automation.Output{Branch: &falseBranch})
	require.Equal(t, automation.SkipNode{ID: "mail"}, s.Next(states), "the true branch wasn't taken")
	states["mail"] = automation.NodeState{Status: automation.NodeSkipped}
	require.Equal(t, automation.Done{}, s.Next(states))

	trueBranch := true
	states["any"] = succeeded(automation.Output{Branch: &trueBranch})
	delete(states, "mail")
	require.Equal(t, automation.RunNode{ID: "mail"}, s.Next(states))
}

func TestNextStopsDownstreamOfAFailure(t *testing.T) {
	// start → a → b, start → c: a's failure blocks b, and c still runs.
	s, err := automation.ParseSpec([]byte(spec([]string{
		node("start", "trigger", `{"kind":"manual"}`),
		node("a", "action", `{"operation":"sync_connector","connector_id":"C"}`),
		node("b", "notify", `{"message":""}`),
		node("c", "notify", `{"message":""}`),
	}, `[{"from":"start","to":"a"},{"from":"a","to":"b"},{"from":"start","to":"c"}]`)))
	require.NoError(t, err)
	require.Equal(t, []string{"start", "a", "c", "b"}, s.Order())
	states := map[string]automation.NodeState{"start": succeeded(automation.Output{}), "a": {Status: automation.NodeFailed}}
	require.Equal(t, automation.RunNode{ID: "c"}, s.Next(states))
	states["c"] = succeeded(automation.Output{})
	require.Equal(t, automation.SkipNode{ID: "b", AfterFailure: true}, s.Next(states))
	states["b"] = automation.NodeState{Status: automation.NodeBlocked}
	require.Equal(t, automation.Done{Failed: true}, s.Next(states))
}

func TestNextJoins(t *testing.T) {
	// A join runs when one of its edges was taken and blocks past a failure.
	s, err := automation.ParseSpec([]byte(spec([]string{
		node("start", "trigger", `{"kind":"document_arrived","set_id":"S"}`),
		node("many", "condition", `{"check":"count","op":"at_least","value":10}`),
		node("ask", "assistant", `{"instruction":"Summarise them"}`),
		node("mail", "notify", `{"message":""}`),
	}, `[{"from":"start","to":"many"},{"from":"many","to":"ask","branch":"true"},{"from":"many","to":"mail","branch":"false"},{"from":"ask","to":"mail"}]`)))
	require.NoError(t, err)
	no := false
	states := map[string]automation.NodeState{"start": succeeded(automation.Output{}), "many": succeeded(automation.Output{Branch: &no})}
	require.Equal(t, automation.SkipNode{ID: "ask"}, s.Next(states))
	states["ask"] = automation.NodeState{Status: automation.NodeSkipped}
	require.Equal(t, automation.RunNode{ID: "mail"}, s.Next(states), "the false branch reaches it")
	states["ask"] = automation.NodeState{Status: automation.NodeBlocked}
	require.Equal(t, automation.SkipNode{ID: "mail", AfterFailure: true}, s.Next(states), "a blocked step blocks what follows")
}

func TestNodeStatusEnded(t *testing.T) {
	for status, ended := range map[automation.NodeStatus]bool{
		"": false, automation.NodeRunning: false, automation.NodeSucceeded: true, automation.NodeFailed: true,
		automation.NodeSkipped: true, automation.NodeBlocked: true,
	} {
		require.Equal(t, ended, status.Ended(), status)
	}
}

func TestTargetAndKind(t *testing.T) {
	for trigger, want := range map[automation.Trigger]string{
		automation.SyncFinished{ConnectorID: "C"}:      "C",
		automation.LoadReady{OntologyID: "O"}:          "O",
		automation.DocumentArrived{SetID: "S"}:         "S",
		automation.GenerationFinished{OntologyID: "G"}: "G",
		automation.Manual{}:                            "",
	} {
		require.Equal(t, want, automation.Target(trigger))
	}
	require.Equal(t, automation.TriggerDocumentArrived, automation.KindOf(automation.DocumentArrived{}))
	require.Equal(t, "connector:W/C", automation.SyncConnector{ConnectorID: "C"}.Target("W"))
	require.Equal(t, "dashboard:W/D", automation.PublishDashboard{DashboardID: "D"}.Target("W"))
}

func TestSameSteps(t *testing.T) {
	before, err := automation.ParseSpec([]byte(overdue))
	require.NoError(t, err)
	moved, err := automation.ParseSpec([]byte(strings.Replace(overdue, `"position":{"x":0,"y":0}}`, `"position":{"x":120.5,"y":-40}}`, 1)))
	require.NoError(t, err)
	require.True(t, automation.SameSteps(before, moved), "moving a node changes nothing that runs")
	require.Equal(t, 0.0, before.Nodes()[0].Position.X, "the specs compared are left as they were")
	changed, err := automation.ParseSpec([]byte(strings.Replace(overdue, "Count the overdue invoices.", "Count the paid invoices.", 1)))
	require.NoError(t, err)
	require.False(t, automation.SameSteps(before, changed))
	rewired, err := automation.ParseSpec([]byte(strings.Replace(overdue, `"branch":"true"`, `"branch":"false"`, 1)))
	require.NoError(t, err)
	require.False(t, automation.SameSteps(before, rewired))
}
