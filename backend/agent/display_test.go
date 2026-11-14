package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/agent/llm"
	"github.com/helixdb/helix-foundry/backend/integration"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/sample"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// workspaceHarness is a harness whose workspace holds connectors, sets, a
// document and reviews, and whose ontology has a current load.
func workspaceHarness(t *testing.T, steps ...[]llm.Chunk) *harness {
	t.Helper()
	h := newHarness(t, steps...)
	current := "load0"
	h.db.ontologies[0].CurrentLoadID = &current
	h.db.connectors = []*model.Connector{
		{ID: "c1", WorkspaceID: ws, Name: "Shop database", Kind: "postgres"},
		{ID: "c2", WorkspaceID: ws, Name: "Stripe", Kind: "stripe"},
		{ID: "ev1", WorkspaceID: ws, Name: "Notes API", Kind: model.ConnectorKindEvents},
		{ID: "c9", WorkspaceID: "ws2", Name: "Theirs", Kind: "stripe"},
	}
	h.db.sets = []*model.FoundryDocumentSet{
		{ID: "set1", WorkspaceID: ws, Name: "General"},
		{ID: "set2", WorkspaceID: ws, Name: "Contracts"},
		{ID: "set3", WorkspaceID: ws, Name: "Notes", Owner: &model.FoundryDocumentSetOwner{ConnectorID: "c3", ConnectorName: "Granola"}},
	}
	h.db.documents = []*model.FoundryDocument{{ID: "doc1", WorkspaceID: ws, SetID: "set1", Name: "contract.md"}}
	h.backend.reviews = []*foundryv1.FoundryReview{
		{ReviewKey: "r1", Reason: "unresolved", Settle: foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_RESOLVE, Type: "Customer", DocId: "doc1",
			NormName: "globexx", Candidates: []string{"scus_1"}, Quote: "Globexx",
			Entities: []*foundryv1.FoundryReviewEntity{{Type: "Customer", EntityKey: "scus_1", Name: "Globex Corporation"}}},
		{ReviewKey: "r2", Reason: "prop_conflict", Settle: foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_PROP, Type: "Customer", EntityKey: "scus_1",
			Prop: "currency", Values: []*foundryv1.FoundryReviewValue{{ValueJson: `"GBP"`, DocId: "doc1", Quote: "billed in GBP"}}},
		{ReviewKey: "r3", Reason: "low_confidence", Settle: foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_RELEASE, DocId: "doc1", Link: "promised_to",
			StatementJson: `{"subject":{"type":"Commitment","name":"support"}}`, StatementHash: statementHash},
		{ReviewKey: "r4", Reason: "ambiguous", Settle: foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_STALE},
		{ReviewKey: "r5", Reason: "conflict"},
	}
	return h
}

var statementHash = strings.Repeat("ab", 32)

// prepared runs a turn whose model prepares one action after the person
// said said: the action it stored, if any, and what the model was told.
func prepared(t *testing.T, h *harness, said, operation, target, payload string) (*model.FoundryAgentAction, llm.ToolResult) {
	t.Helper()
	h.model.steps = [][]llm.Chunk{
		call("toolu_1", toolPrepare, fmt.Sprintf(`{"operation":%q,"target":%q,"payload":%s}`, operation, target, payload)),
		say("Run it when you're ready."),
	}
	turn := message(said)
	// A turn of its own, so a test may prepare the same action twice.
	turn.ID = fmt.Sprint("turn-", len(h.model.requests))
	h.db.turns = []*model.FoundryAgentTurn{turn}
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	told := h.model.requests[len(h.model.requests)-1].Messages
	result := told[len(told)-1].Content[0].(llm.ToolResult)
	if len(h.db.actions) == 0 {
		return nil, result
	}
	return h.db.actions[len(h.db.actions)-1], result
}

// Every operation the Assistant prepares is a card naming what it acts on
// as Foundry holds it, and runs nothing.
func TestEveryOperationIsACard(t *testing.T) {
	for _, c := range []struct {
		name, said, operation, target, payload, display string
		named                                           bool
	}{
		{"an ontology on a database", "Make an ontology on the sales database", "create_ontology", "tenant:t1", `{"name":"Renewals"}`,
			`{"name":"Renewals","database":{"id":"t1","name":"sales"}}`, true},
		{"an ontology on a new database", "Create an ontology called Renewals", "create_ontology", "workspace:ws1",
			`{"name":"Renewals","database":{"name":"Renewals db","slug":"renewals","project_name":"Contracts"}}`,
			`{"name":"Renewals","new_database":{"name":"Renewals db","slug":"renewals","new_project":"Contracts"}}`, true},
		{"an ontology in an existing project", "Make one", "create_ontology", "workspace:ws1",
			`{"name":"Renewals","database":{"name":"Renewals db","slug":"renewals","project_id":"p1"}}`,
			`{"name":"Renewals","new_database":{"name":"Renewals db","slug":"renewals","project_id":"p1"}}`, false},
		{"a generation of chosen sources", "Draft the Sales graph again", "generate_ontology", "ontology:ws1/ont1",
			`{"goal":"Track renewals","connector_ids":["c1","gone1"],"document_set_ids":["set1","set3"],"event_sources":[]}`,
			`{"ontology":{"id":"ont1","name":"Sales graph"},"goal":"Track renewals","connectors_read":"chosen",
			  "connectors":[{"id":"c1","name":"Shop database","kind":"postgres"},{"id":"gone1"}],
			  "document_sets":[{"id":"set1","name":"General"},{"id":"set3","name":"Granola / Notes"}]}`, true},
		{"a generation of every connector", "Draft it", "generate_ontology", "ontology:ws1/ont1", `{}`,
			`{"ontology":{"id":"ont1","name":"Sales graph"},"connectors_read":"every",
			  "connectors":[{"id":"c1","name":"Shop database","kind":"postgres"},{"id":"c2","name":"Stripe","kind":"stripe"}],"document_sets":[]}`, false},
		{"a generation of documents alone", "Draft ont1 from General", "generate_ontology", "ontology:ws1/ont1",
			`{"no_connectors":true,"document_set_ids":["set1"]}`,
			`{"ontology":{"id":"ont1","name":"Sales graph"},"connectors_read":"none","connectors":[],"document_sets":[{"id":"set1","name":"General"}]}`, true},
		{"a version made current", "Make v3 of the sales graph current", "set_current_version", "ontology:ws1/ont1",
			`{"version_id":"ver3","expected_current_version_id":"ver2"}`,
			`{"ontology":{"id":"ont1","name":"Sales graph"},"version":{"id":"ver3","number":3},"current":{"id":"ver2","number":2}}`, true},
		{"a first version made current", "Make it current", "set_current_version", "ontology:ws1/ont1",
			`{"version_id":"ver1","expected_current_version_id":""}`,
			`{"ontology":{"id":"ont1","name":"Sales graph"},"version":{"id":"ver1","number":1}}`, false},
		{"a load", "Load the sales graph", "create_load", "ontology:ws1/ont1", `{"version_id":"gone"}`,
			`{"ontology":{"id":"ont1","name":"Sales graph"},"version":{"id":"gone"}}`, true},
		{"a load made current", "Go back to the earlier load", "set_current_load", "ontology:ws1/ont1", `{"load_id":"load5"}`,
			`{"ontology":{"id":"ont1","name":"Sales graph"},
			  "load":{"id":"load5","version":{"id":"ver2","number":2},"status":"ready","created_at":"2026-11-01T09:00:00Z"},
			  "current":{"id":"load0","version":{"id":"ver2","number":2},"status":"ready","created_at":"2026-11-01T09:00:00Z"}}`, false},
		{"a revision", "Revise it", "revise_version", "ontology:ws1/ont1",
			`{"version_id":"ver3","edits":[{"put":{"item_id":"rule.overdue","toml":"x = 1"}},{"remove":{"item_id":"type.Old"}}],"dry_run":true}`,
			`{"ontology":{"id":"ont1","name":"Sales graph"},"version":{"id":"ver3","number":3},
			  "edits":[{"kind":"put","item_id":"rule.overdue"},{"kind":"remove","item_id":"type.Old"}],"dry_run":true}`, false},
		{"a sync", "Sync Stripe", "sync_connector", "connector:ws1/c2", `{}`,
			`{"connector":{"id":"c2","name":"Stripe","kind":"stripe"}}`, true},
		{"the sample", "Add the sample to the Sales graph", "add_sample_data", "ontology:ws1/ont1", `{}`,
			`{"ontology":{"id":"ont1","name":"Sales graph"}}`, true},
		{"a set", "Make a set for leases", "create_document_set", "ontology:ws1/ont1", `{"name":"Leases","description":"Signed leases"}`,
			`{"ontology":{"id":"ont1","name":"Sales graph"},"name":"Leases","description":"Signed leases"}`, false},
		{"a set renamed", "Rename General", "update_document_set", "ontology:ws1/ont1", `{"set_id":"set1","name":"Signed"}`,
			`{"ontology":{"id":"ont1","name":"Sales graph"},"set":{"id":"set1","name":"General"},"name":"Signed"}`, false},
		{"a document moved", "Move contract.md to Contracts", "move_document", "ontology:ws1/ont1", `{"document_id":"doc1","set_id":"set2"}`,
			`{"ontology":{"id":"ont1","name":"Sales graph"},"document":{"id":"doc1","name":"contract.md"},
			  "from":{"id":"set1","name":"General"},"to":{"id":"set2","name":"Contracts"}}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := workspaceHarness(t)
			a, result := prepared(t, h, c.said, c.operation, c.target, c.payload)
			require.NotNil(t, a, result.Content)
			assert.False(t, result.IsError)
			assert.JSONEq(t, c.display, string(a.Display))
			assert.Equal(t, c.named, a.TargetNamedByUser)
			assert.Zero(t, h.backend.loads, "preparing runs nothing")
			i := slices.Index(h.db.eventTypes(), "action_card")
			require.GreaterOrEqual(t, i, 0)
			var card ActionCard
			require.NoError(t, json.Unmarshal(h.db.events[i], &card))
			assert.JSONEq(t, c.display, string(card.Display), "the card carries the display")
		})
	}
}

// What the Assistant may not prepare isn't offered to its model, and is
// refused before it is checked.
func TestUploadsAndAPISourcesAreRefused(t *testing.T) {
	for _, events := range []bool{false, true} {
		h := workspaceHarness(t)
		h.host.service.Events = events
		a, result := prepared(t, h, "Do it", "upload_document", "ontology:ws1/ont1", `{"set_id":"set1","name":"a.md","size_bytes":1,"sha256":"`+statementHash+`"}`)
		assert.Nil(t, a)
		assert.True(t, result.IsError)
		assert.Contains(t, result.Content, "does not equal any of", "the schema the model is given leaves it out")
		var prepare llm.Tool
		for _, tool := range h.model.requests[0].Tools {
			if tool.Name == toolPrepare {
				prepare = tool
			}
		}
		schema := string(prepare.InputSchema)
		for operation := range Operations {
			assert.Contains(t, schema, `"`+operation+`"`)
		}
		for _, refused := range []string{"upload_document", "create_event_source", "delete_ontology"} {
			assert.NotContains(t, schema, `"`+refused+`"`)
			assert.NotContains(t, schema, "- "+refused+":")
		}
		assert.NotContains(t, schema, "create_event_source: workspace")
	}
	// The sink refuses what slips past a schema.
	sink := &actionSink{}
	for _, refused := range []string{"upload_document", "create_event_source", "delete_ontology"} {
		assert.False(t, sink.Allows(refused))
	}
}

// A target of another workspace, or one whose workspace can't be told, is
// refused before a card is drawn.
func TestTargetsAreOfTheTurnsWorkspace(t *testing.T) {
	for name, c := range map[string]struct{ target, payload, says string }{
		"another workspace's database": {"tenant:t2", `{"name":"x"}`, "another workspace"},
		"a database no one has":        {"tenant:t9", `{"name":"x"}`, "another workspace"},
		"another workspace":            {"workspace:ws2", `{"name":"x","database":{"name":"d","slug":"d","project_id":"p1"}}`, "another workspace"},
		"another workspace's sync":     {"connector:ws2/c9", `{}`, "another workspace"},
	} {
		t.Run(name, func(t *testing.T) {
			h := workspaceHarness(t)
			operation := "create_ontology"
			if strings.HasPrefix(c.target, "connector:") {
				operation = "sync_connector"
			}
			a, result := prepared(t, h, "Do it", operation, c.target, c.payload)
			assert.Nil(t, a)
			assert.True(t, result.IsError)
			assert.Contains(t, result.Content, c.says)
		})
	}
	h := workspaceHarness(t)
	h.runner.Databases = nil
	a, result := prepared(t, h, "Do it", "create_ontology", "tenant:t1", `{"name":"x"}`)
	assert.Nil(t, a)
	assert.Contains(t, result.Content, "can't tell whose a database is")
}

// A connector named by a longer connector's name isn't named.
func TestAConnectorIsNamedByItsWholeName(t *testing.T) {
	h := workspaceHarness(t)
	h.db.connectors = append(h.db.connectors, &model.Connector{ID: "c4", WorkspaceID: ws, Name: "Stripe EU", Kind: "stripe"})
	a, _ := prepared(t, h, "Sync Stripe EU", "sync_connector", "connector:ws1/c2", `{}`)
	require.NotNil(t, a)
	assert.False(t, a.TargetNamedByUser)
	a, _ = prepared(t, h, "Sync c2 please", "sync_connector", "connector:ws1/c2", `{}`)
	require.NotNil(t, a)
	assert.True(t, a.TargetNamedByUser, "by its id")
}

// A review's card holds the review as the person reads it and the decision.
func TestSettleReviewCards(t *testing.T) {
	for _, c := range []struct {
		name, payload, decision string
	}{
		{"a link", `{"review_id":"r1","decision":{"resolve":{"entity_key":"scus_1"}}}`, `{"kind":"link","entity_key":"scus_1","candidate":true}`},
		{"a link to another entity", `{"review_id":"r1","decision":{"resolve":{"entity_key":"scus_9"}}}`, `{"kind":"link","entity_key":"scus_9"}`},
		{"no one", `{"review_id":"r1","decision":{"resolve":{"not_in_data":true}}}`, `{"kind":"not_in_data"}`},
		{"a value", `{"review_id":"r2","decision":{"prop":{"text":"GBP"}}}`, `{"kind":"set_value","value":{"text":"GBP"}}`},
		{"an integer", `{"review_id":"r2","decision":{"prop":{"integer":"12"}}}`, `{"kind":"set_value","value":{"integer":"12"}}`},
		{"a number", `{"review_id":"r2","decision":{"prop":{"number":1.5}}}`, `{"kind":"set_value","value":{"number":1.5}}`},
		{"a boolean", `{"review_id":"r2","decision":{"prop":{"boolean":false}}}`, `{"kind":"set_value","value":{"boolean":false}}`},
		{"no value", `{"review_id":"r2","decision":{"prop":{"null":true}}}`, `{"kind":"remove_value"}`},
		{"an approval", `{"review_id":"r3","decision":{"release":{"statement_hash":"` + statementHash + `"}}}`, `{"kind":"approve"}`},
		{"a reject", `{"review_id":"r3","decision":{"reject":{"statement_hash":"` + statementHash + `"}}}`, `{"kind":"reject"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := workspaceHarness(t)
			a, result := prepared(t, h, "Settle the Globexx review in the Sales graph", "settle_review", "ontology:ws1/ont1", c.payload)
			require.NotNil(t, a, result.Content)
			var d SettleDisplay
			require.NoError(t, json.Unmarshal(a.Display, &d))
			assert.Equal(t, Ref{ID: "ont1", Name: "Sales graph"}, d.Ontology)
			assert.Equal(t, "load1", d.LoadID)
			if d.Review.DocID != "" {
				assert.Equal(t, &Ref{ID: "doc1", Name: "contract.md"}, d.Document)
			} else {
				assert.Nil(t, d.Document, "a property's review names the documents of its values")
			}
			raw, err := json.Marshal(d.Decision)
			require.NoError(t, err)
			assert.JSONEq(t, c.decision, string(raw))
			assert.True(t, a.TargetNamedByUser)
		})
	}
	h := workspaceHarness(t)
	a, _ := prepared(t, h, "Settle it", "settle_review", "ontology:ws1/ont1", `{"review_id":"r1","decision":{"resolve":{"not_in_data":true}}}`)
	require.NotNil(t, a)
	var d SettleDisplay
	require.NoError(t, json.Unmarshal(a.Display, &d))
	assert.Equal(t, integration.FoundryReviewOutput{ReviewKey: "r1", Reason: "unresolved", Settle: "resolve", Type: "Customer", DocID: "doc1",
		Quote: "Globexx", NormName: "globexx", Candidates: []string{"scus_1"},
		Entities: []integration.FoundryReviewEntityOutput{{Type: "Customer", EntityKey: "scus_1", Name: "Globex Corporation"}}}, d.Review,
		"the card names the entities the review concerns")
	var raw map[string]any
	require.NoError(t, json.Unmarshal(a.Display, &raw))
	assert.Equal(t, []any{map[string]any{"type": "Customer", "entity_key": "scus_1", "name": "Globex Corporation"}}, raw["review"].(map[string]any)["entities"],
		"in the shape the site reads")
}

// A replayed attempt finds the card the first drew, though the review it
// read is gone since: it isn't drawn, or refused, again.
func TestAReplayedPrepareIsNotDrawnAgain(t *testing.T) {
	prepare := `{"operation":"settle_review","target":"ontology:ws1/ont1","payload":{"review_id":"r1","decision":{"resolve":{"not_in_data":true}}}}`
	h := workspaceHarness(t, call("toolu_1", toolPrepare, prepare), call("toolu_2", toolPrepare, prepare), say("ok"))
	turn := message("Settle the Globexx review")
	h.db.turns = []*model.FoundryAgentTurn{turn}
	h.backend.onReviews = func() { h.backend.reviews = nil }
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	require.Len(t, h.db.actions, 1)
	replayed := h.model.requests[2].Messages
	result := replayed[len(replayed)-1].Content[0].(llm.ToolResult)
	assert.False(t, result.IsError, result.Content)
	assert.Contains(t, result.Content, h.db.actions[0].ID)
	assert.Equal(t, 1, strings.Count(strings.Join(h.db.eventTypes(), " "), "action_card"), "one card")
}

// A card reads nothing of an ontology the workspace doesn't hold: its
// versions and loads are another workspace's.
func TestAnOntologyTheWorkspaceDoesntHoldIsRefused(t *testing.T) {
	for _, c := range []struct{ operation, payload string }{
		{"set_current_version", `{"version_id":"ver3","expected_current_version_id":"ver2"}`},
		{"set_current_load", `{"load_id":"load5"}`},
		{"create_load", `{"version_id":"ver3"}`},
	} {
		h := workspaceHarness(t)
		h.db.ontologies = append(h.db.ontologies, &model.FoundryOntology{ID: "theirs", WorkspaceID: "ws2", Name: "Theirs"})
		deleted := time.Now()
		h.db.ontologies = append(h.db.ontologies, &model.FoundryOntology{ID: "gone", WorkspaceID: ws, Name: "Gone", DeletedAt: &deleted})
		for _, id := range []string{"theirs", "gone", "nowhere"} {
			a, result := prepared(t, h, "Do it to "+id, c.operation, "ontology:ws1/"+id, c.payload)
			assert.Nil(t, a, id)
			assert.True(t, result.IsError)
			assert.Contains(t, result.Content, "holds no ontology "+id)
			assert.Zero(t, h.db.versionReads, "nothing of it is read")
		}
	}
}

// A generation names the API sources it reads and how, as the payload says
// them (structured or extraction).
func TestAGenerationNamesItsAPISources(t *testing.T) {
	h := workspaceHarness(t)
	h.host.service.Events = true
	a, result := prepared(t, h, "Draft the Sales graph from the notes", "generate_ontology", "ontology:ws1/ont1",
		`{"connector_ids":["c1"],"event_sources":[{"connector_id":"ev1","mode":"extraction"},{"connector_id":"gone2","mode":"structured"}]}`)
	require.NotNil(t, a, result.Content)
	assert.JSONEq(t, `{"ontology":{"id":"ont1","name":"Sales graph"},"connectors_read":"chosen",
		"connectors":[{"id":"c1","name":"Shop database","kind":"postgres"}],"document_sets":[],
		"event_sources":[{"connector":{"id":"ev1","name":"Notes API","kind":"events"},"mode":"extraction"},
		  {"connector":{"id":"gone2"},"mode":"structured"}]}`, string(a.Display))
}

// The sample company's made-up data is read only when named: every
// connector would read it too, and a card naming it says so.
func TestTheSamplesConnectorIsReadOnlyWhenNamed(t *testing.T) {
	h := workspaceHarness(t)
	h.db.connectors = append(h.db.connectors, &model.Connector{ID: "acme", WorkspaceID: ws, Name: "Acme Corp", Kind: "postgres",
		Config: json.RawMessage(`{"host":"acme-db","port":5432,"database":"` + sample.DatabaseName + `","user":"` + sample.DatabaseUser + `"}`)})
	a, result := prepared(t, h, "Draft the Sales graph", "generate_ontology", "ontology:ws1/ont1", `{}`)
	assert.Nil(t, a)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content, "the made-up sample company's (acme) too")
	a, result = prepared(t, h, "Draft the Sales graph", "generate_ontology", "ontology:ws1/ont1", `{"connector_ids":["c1","acme"]}`)
	require.NotNil(t, a, result.Content)
	var d GenerateDisplay
	require.NoError(t, json.Unmarshal(a.Display, &d))
	assert.Equal(t, []ConnectorRef{{Ref: Ref{ID: "c1", Name: "Shop database"}, Kind: "postgres"},
		{Ref: Ref{ID: "acme", Name: "Acme Corp"}, Kind: "postgres", Sample: true}}, d.Connectors)
	a, _ = prepared(t, h, "Draft the Sales graph from documents", "generate_ontology", "ontology:ws1/ont1", `{"no_connectors":true}`)
	require.NotNil(t, a, "documents alone read no connector")
	// Another host's sample database isn't this one's.
	h.host.service.SampleDatabase = &integration.SampleDatabase{Host: "rds.example"}
	a, result = prepared(t, h, "Draft the Sales graph", "generate_ontology", "ontology:ws1/ont1", `{}`)
	require.NotNil(t, a, result.Content)
	h.host.service.SampleDatabase = nil
	a, result = prepared(t, h, "Draft the Sales graph", "generate_ontology", "ontology:ws1/ont1", `{"goal":"x"}`)
	require.NotNil(t, a, "a host without a sample database has none to leave out: "+result.Content)
}

// A decision the review doesn't take is refused with why, before a card.
func TestSettleReviewRefusals(t *testing.T) {
	for _, c := range []struct {
		name, payload, says string
		readErr             error
	}{
		{"no such review", `{"review_id":"r9","decision":{"resolve":{"not_in_data":true}}}`, "holds no review r9", nil},
		{"the wrong kind", `{"review_id":"r1","decision":{"prop":{"text":"x"}}}`, "takes a resolve decision", nil},
		{"a resolve of a held statement", `{"review_id":"r3","decision":{"resolve":{"not_in_data":true}}}`, "takes a release decision", nil},
		{"another statement", `{"review_id":"r3","decision":{"release":{"statement_hash":"` + strings.Repeat("cd", 32) + `"}}}`, "reads differently now", nil},
		{"a stale review", `{"review_id":"r4","decision":{"resolve":{"not_in_data":true}}}`, "a new load writes it again", nil},
		{"one nothing settles", `{"review_id":"r5","decision":{"release":{"statement_hash":"` + statementHash + `"}}}`, "no decision settles", nil},
		{"no load", `{"review_id":"r1","decision":{"resolve":{"not_in_data":true}}}`, "load the ontology",
			status.Error(codes.FailedPrecondition, "load the ontology before reading its reviews")},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := workspaceHarness(t)
			h.backend.reviewErr = c.readErr
			a, result := prepared(t, h, "Settle it", "settle_review", "ontology:ws1/ont1", c.payload)
			assert.Nil(t, a)
			assert.True(t, result.IsError)
			assert.Contains(t, result.Content, c.says)
			assert.NotContains(t, h.db.eventTypes(), "action_card")
		})
	}
}

// A member who manages nothing gets the read tools alone: nothing to
// prepare and no connector links.
func TestMembersGetReadToolsAndTheirOwnDashboards(t *testing.T) {
	h := workspaceHarness(t, say("Here is what I can see."))
	h.host.may = func(p integration.Permission) bool {
		return p == integration.PermissionWorkspaceRead || p == integration.PermissionDatabaseQueryRead
	}
	_, err := h.runner.Run(context.Background(), message("What can you do?"))
	require.NoError(t, err)
	var offered []string
	var prepare llm.Tool
	for _, tool := range h.model.requests[0].Tools {
		offered = append(offered, tool.Name)
		if tool.Name == toolPrepare {
			prepare = tool
		}
	}
	assert.NotContains(t, offered, "helix_foundry_connect")
	assert.Contains(t, offered, "helix_foundry_list_reviews")
	assert.Contains(t, offered, toolAsk)
	// A member makes dashboards and changes their own, as in the site; nothing else.
	require.Contains(t, offered, toolPrepare)
	schema := string(prepare.InputSchema)
	for _, op := range []string{"create_dashboard", "update_dashboard", "delete_dashboard"} {
		assert.Contains(t, schema, `"`+op+`"`)
	}
	for _, op := range []string{"create_load", "generate_ontology", "pin_dashboard", "publish_dashboard", "settle_review"} {
		assert.NotContains(t, schema, `"`+op+`"`)
	}
}

// The model reads at most 16 KiB of an action's result.
func TestAnActionResultIsCut(t *testing.T) {
	h := newHarness(t, say("Done."))
	h.db.actions = []*model.FoundryAgentAction{{ID: "act1", Operation: "generate_ontology", Status: model.FoundryAgentActionSucceeded,
		Result: json.RawMessage(`{"result":"` + strings.Repeat("x", 40<<10) + `"}`)}}
	turn := message("act1")
	turn.Kind = model.FoundryAgentTurnActionResult
	_, err := h.runner.Run(context.Background(), turn)
	require.NoError(t, err)
	text := h.model.requests[0].Messages[0].Content[0].(llm.Text).Text
	assert.Less(t, len(text), 17<<10)
	assert.Contains(t, text, "[Cut at 16 KiB")
}

// Connecting is a link to Foundry's form, even for a kind that signs in
// elsewhere: credentials never pass through the Assistant.
func TestConnectIsALinkToFoundry(t *testing.T) {
	for _, events := range []bool{false, true} {
		h := workspaceHarness(t,
			call("toolu_1", "helix_foundry_connect", `{"workspace_id":"ws1","kind":"stripe"}`),
			call("toolu_2", "helix_foundry_connect", `{"workspace_id":"ws1","kind":"events"}`),
			say("Open the link to connect Stripe."))
		// A host that signs in at providers, as the Assistant uses it.
		host := *h.host.service
		host.Events, host.SignIn = events, true
		h.host.service = host.ForAgent()
		_, err := h.runner.Run(context.Background(), message("Connect Stripe"))
		require.NoError(t, err)
		told := h.model.requests[1].Messages
		stripe := told[len(told)-1].Content[0].(llm.ToolResult)
		require.False(t, stripe.IsError, stripe.Content)
		assert.Contains(t, stripe.Content, `"url":"http://localhost:3000/data?add=stripe\u0026workspace=ws1"`)
		assert.NotContains(t, stripe.Content, "sign_in_id")
		told = h.model.requests[2].Messages
		source := told[len(told)-1].Content[0].(llm.ToolResult)
		if !events {
			assert.True(t, source.IsError, "a host without API sources offers none")
			continue
		}
		require.False(t, source.IsError, source.Content)
		assert.NotContains(t, source.Content, "create_event_source", "it isn't sent to an operation it can't prepare")
	}
}

// An API source is made in its form, which makes its send tokens: nothing
// the Assistant reads points it at an operation it can't prepare.
func TestAnAPISourceIsMadeInItsForm(t *testing.T) {
	h := workspaceHarness(t, call("toolu_1", "helix_foundry_list_connector_kinds", `{"workspace_id":"ws1"}`), say("Open its form."))
	h.host.service.Events = true
	_, err := h.runner.Run(context.Background(), message("How do I make an API source?"))
	require.NoError(t, err)
	for _, tool := range h.model.requests[0].Tools {
		assert.NotContains(t, tool.Description, "create_event_source", tool.Name)
	}
	told := h.model.requests[1].Messages
	kinds := told[len(told)-1].Content[0].(llm.ToolResult)
	require.False(t, kinds.IsError, kinds.Content)
	assert.NotContains(t, kinds.Content, "create_event_source")
	assert.Contains(t, kinds.Content, "helix_foundry_connect links it")
}
