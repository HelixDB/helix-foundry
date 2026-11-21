package slack_test

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/slack"
)

const cardAction = "01JBACTXN0000000000000000A"

var (
	sales     = agent.Ref{ID: "ont_1", Name: "Sales"}
	unnamed   = agent.Ref{ID: "ont_2"}
	v2        = agent.VersionRef{ID: "ver_2", Number: 2}
	v3        = agent.VersionRef{ID: "ver_3", Number: 3}
	stripe    = agent.ConnectorRef{Ref: agent.Ref{ID: "con_1", Name: "Stripe"}, Kind: "stripe"}
	sampleDB  = agent.ConnectorRef{Ref: agent.Ref{ID: "con_2", Name: "Acme sample"}, Kind: "postgres", Sample: true}
	general   = agent.Ref{ID: "set_1", Name: "General"}
	revenue   = agent.DashboardRef{Ref: agent.Ref{ID: "dash_1", Name: "Revenue"}, Pinned: true, Published: true}
	quiet     = agent.DashboardRef{Ref: agent.Ref{ID: "dash_2"}}
	renewals  = agent.AutomationRef{Ref: agent.Ref{ID: "wf_1", Name: "Renewals"}, Status: "active"}
	createdAt = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	text      = func(s string) *string { return &s }
	days      = func(n int) *int { return &n }
)

// cardsOf are cards of each operation: the first of each is its main card,
// the rest its variants.
var cardsOf = map[string][]any{
	"create_ontology": {
		agent.CreateOntologyDisplay{Name: "Sales", Database: &agent.Ref{ID: "tenant_1", Name: "sales"}},
		agent.CreateOntologyDisplay{Name: "Sales", NewDatabase: &agent.NewDatabaseDisplay{Name: "sales", Slug: "sales-1", NewProject: "Production"}},
		agent.CreateOntologyDisplay{Name: "Sales", NewDatabase: &agent.NewDatabaseDisplay{Name: "sales", Slug: "sales-1", ProjectID: "proj_1"}},
	},
	"generate_ontology": {
		agent.GenerateDisplay{Ontology: sales, Goal: "Customers and their contracts", ConnectorsRead: agent.ConnectorsChosen,
			Connectors: []agent.ConnectorRef{stripe}, DocumentSets: []agent.Ref{general},
			EventSources: []agent.EventSourceDisplay{{Connector: agent.ConnectorRef{Ref: agent.Ref{ID: "con_3", Name: "Notes"}, Kind: "events"}, Mode: "extraction"},
				{Connector: agent.ConnectorRef{Ref: agent.Ref{ID: "con_4"}}, Mode: "other"}}},
		agent.GenerateDisplay{Ontology: sales, ConnectorsRead: agent.ConnectorsNone, Connectors: []agent.ConnectorRef{}, DocumentSets: []agent.Ref{}},
		agent.GenerateDisplay{Ontology: sales, ConnectorsRead: agent.ConnectorsEvery, Connectors: []agent.ConnectorRef{}, DocumentSets: []agent.Ref{}},
		agent.GenerateDisplay{Ontology: sales, ConnectorsRead: agent.ConnectorsEvery, Connectors: []agent.ConnectorRef{stripe}, DocumentSets: []agent.Ref{}},
		agent.GenerateDisplay{Ontology: sales, ConnectorsRead: agent.ConnectorsChosen, Connectors: []agent.ConnectorRef{stripe, sampleDB}, DocumentSets: []agent.Ref{}},
		agent.GenerateDisplay{Ontology: sales, ConnectorsRead: agent.ConnectorsChosen, Connectors: []agent.ConnectorRef{sampleDB}, DocumentSets: []agent.Ref{}, Sample: "acme"},
	},
	"set_current_version": {
		agent.SetCurrentVersionDisplay{Ontology: sales, Version: v3, Current: &v2},
		agent.SetCurrentVersionDisplay{Ontology: unnamed, Version: agent.VersionRef{ID: "ver_x"}},
	},
	"create_load": {agent.CreateLoadDisplay{Ontology: sales, Version: v3}},
	"set_current_load": {
		agent.SetCurrentLoadDisplay{Ontology: sales, Load: agent.LoadRef{ID: "load_2", Version: v3, Status: "ready", CreatedAt: &createdAt},
			Current: &agent.LoadRef{ID: "load_1", Version: v2, Status: "partial"}},
		agent.SetCurrentLoadDisplay{Ontology: sales, Load: agent.LoadRef{ID: "load_2"}},
	},
	"revise_version": {
		agent.ReviseDisplay{Ontology: sales, Version: v3, Edits: []agent.EditDisplay{{Kind: "put", ItemID: "type.Person"}, {Kind: "remove", ItemID: "rule.old"}}},
		agent.ReviseDisplay{Ontology: sales, Version: v3, Edits: []agent.EditDisplay{{Kind: "put", ItemID: "type.Person"}}, DryRun: true},
	},
	"sync_connector": {
		agent.SyncDisplay{Connector: stripe},
		agent.SyncDisplay{Connector: agent.ConnectorRef{Ref: agent.Ref{ID: "con_9"}}},
	},
	"add_sample_data":     {agent.OntologyDisplay{Ontology: sales}},
	"create_document_set": {agent.CreateSetDisplay{Ontology: sales, Name: "Contracts", Description: "Signed MSAs"}, agent.CreateSetDisplay{Ontology: sales, Name: "Contracts"}},
	"update_document_set": {
		agent.UpdateSetDisplay{Ontology: sales, Set: general, Name: text("Contracts")},
		agent.UpdateSetDisplay{Ontology: sales, Set: general, Description: text("")},
		agent.UpdateSetDisplay{Ontology: sales, Set: general, Name: text("Contracts"), Description: text("Signed MSAs")},
	},
	"move_document": {
		agent.MoveDisplay{Ontology: sales, Document: agent.Ref{ID: "doc_1", Name: "msa.md"}, From: general, To: agent.Ref{ID: "set_2", Name: "Contracts"}},
		agent.MoveDisplay{Ontology: sales, Document: agent.Ref{ID: "doc_1"}, To: agent.Ref{ID: "set_2"}},
	},
	"settle_review": {
		settle(agent.DecisionDisplay{Kind: agent.DecisionLink, EntityKey: "scus_1", Candidate: true}),
		settle(agent.DecisionDisplay{Kind: agent.DecisionLink, EntityKey: "scus_9"}),
		settle(agent.DecisionDisplay{Kind: agent.DecisionNotInData}),
		agent.SettleDisplay{Ontology: sales, LoadID: "load_1", Decision: agent.DecisionDisplay{Kind: agent.DecisionSetValue, Value: map[string]any{"text": "GBP"}},
			Review: integration.FoundryReviewOutput{ReviewKey: "r2", Reason: "prop_conflict", Settle: "prop", Type: "Customer", EntityKey: "scus_1", Prop: "currency",
				Entities: []integration.FoundryReviewEntityOutput{{Type: "Customer", EntityKey: "scus_1", Name: "Globex"}}}},
		agent.SettleDisplay{Ontology: sales, LoadID: "load_1", Decision: agent.DecisionDisplay{Kind: agent.DecisionSetValue, Value: map[string]any{"integer": "12000000000000000001"}},
			Review: integration.FoundryReviewOutput{ReviewKey: "r2", Settle: "prop", Type: "Customer", Prop: "seats"}},
		agent.SettleDisplay{Ontology: sales, Decision: agent.DecisionDisplay{Kind: agent.DecisionSetValue, Value: map[string]any{"boolean": false}},
			Review: integration.FoundryReviewOutput{Settle: "prop"}},
		agent.SettleDisplay{Ontology: sales, Decision: agent.DecisionDisplay{Kind: agent.DecisionSetValue, Value: map[string]any{}},
			Review: integration.FoundryReviewOutput{Settle: "prop"}},
		agent.SettleDisplay{Ontology: sales, Decision: agent.DecisionDisplay{Kind: agent.DecisionRemoveValue},
			Review: integration.FoundryReviewOutput{Settle: "prop", Type: "Customer", Prop: "currency", EntityKey: "scus_7"}},
		agent.SettleDisplay{Ontology: sales, Decision: agent.DecisionDisplay{Kind: agent.DecisionApprove},
			Review: integration.FoundryReviewOutput{Settle: "release", Link: "signed_with",
				Statement: `{"subject":{"type":"Contract","name":"MSA"},"object":{"type":"Customer"}}`}},
		agent.SettleDisplay{Ontology: sales, Decision: agent.DecisionDisplay{Kind: agent.DecisionReject},
			Review: integration.FoundryReviewOutput{Settle: "release", Statement: `{"props":{}}`, Quote: "We promise support."}},
	},
	"create_dashboard": {
		agent.CreateDashboardDisplay{Name: "Invoices", Spec: agent.SpecDisplay{Tiles: []agent.TileDisplay{{Title: "Open invoices", Ontology: sales}, {Ontology: unnamed}}}},
		agent.CreateDashboardDisplay{Name: "Empty", Spec: agent.SpecDisplay{Tiles: []agent.TileDisplay{}}},
	},
	"update_dashboard": {
		agent.UpdateDashboardDisplay{Dashboard: revenue, Name: text("Revenue 2027"), Description: text("By month"),
			Spec: &agent.SpecDisplay{Tiles: []agent.TileDisplay{{Title: "MRR", Ontology: sales}}}},
		agent.UpdateDashboardDisplay{Dashboard: quiet, Description: text("")},
	},
	"delete_dashboard":    {agent.DashboardDisplay{Dashboard: revenue}},
	"pin_dashboard":       {agent.PinDashboardDisplay{Dashboard: quiet, Pinned: true}, agent.PinDashboardDisplay{Dashboard: revenue}},
	"publish_dashboard":   {agent.PublishDashboardDisplay{Dashboard: quiet, ExpiresInDays: days(7)}, agent.PublishDashboardDisplay{Dashboard: revenue}, agent.PublishDashboardDisplay{Dashboard: revenue, ExpiresInDays: days(0)}, agent.PublishDashboardDisplay{Dashboard: quiet, ExpiresInDays: days(1)}},
	"unpublish_dashboard": {agent.DashboardDisplay{Dashboard: revenue}},
	"create_workflow":     {agent.CreateAutomationDisplay{Name: "Weekly renewals"}},
	"update_workflow": {
		agent.UpdateAutomationDisplay{Automation: renewals, Name: text("Renewals weekly"), Steps: true},
		agent.UpdateAutomationDisplay{Automation: agent.AutomationRef{Ref: agent.Ref{ID: "wf_2"}, Status: "needs_approval"}},
	},
	"pause_workflow":  {agent.AutomationDisplay{Automation: renewals}},
	"resume_workflow": {agent.AutomationDisplay{Automation: agent.AutomationRef{Ref: agent.Ref{ID: "wf_1", Name: "Renewals"}, Status: "paused"}}},
	"delete_workflow": {agent.AutomationDisplay{Automation: renewals}},
	"run_workflow":    {agent.AutomationDisplay{Automation: renewals}},
	"set_assistant_runtime": {
		agent.AssistantRuntimeDisplay{Runtime: "claude_code", Current: "api"},
		agent.AssistantRuntimeDisplay{Runtime: "codex", Current: "claude_code", Online: true},
		agent.AssistantRuntimeDisplay{Runtime: "api", Current: "codex", Online: true},
		agent.AssistantRuntimeDisplay{Runtime: "future", Current: "api"},
	},
	"revoke_assistant_helper": {
		agent.AssistantHelperDisplay{Host: agent.Ref{ID: "host_1", Name: "Ada's laptop"}, Runtimes: []string{"claude_code", "codex"}, Online: true},
		agent.AssistantHelperDisplay{Host: agent.Ref{ID: "host_2"}, Runtimes: []string{}},
	},
}

// settle is a resolve review of "globex" in msa.md, decided as d.
func settle(d agent.DecisionDisplay) agent.SettleDisplay {
	return agent.SettleDisplay{Ontology: sales, LoadID: "load_1", Document: &agent.Ref{ID: "doc_1", Name: "msa.md"}, Decision: d,
		Review: integration.FoundryReviewOutput{ReviewKey: "r1", Reason: "ambiguous", Settle: "resolve", Type: "Customer", NormName: "globex",
			Quote: "Globex Ltd signs for <acme.com>.", Candidates: []string{"scus_1", "scus_2"},
			Entities: []integration.FoundryReviewEntityOutput{{Type: "Customer", EntityKey: "scus_1", Name: "Globex"}, {Type: "Customer", EntityKey: "scus_2"},
				{Type: "Other", EntityKey: "scus_9", Name: "Not this"}}}}
}

// actionCard is an action of operation with display.
func actionCard(t testing.TB, operation string, display any) agent.ActionCard {
	raw, err := json.Marshal(display)
	require.NoError(t, err)
	return agent.ActionCard{ActionID: cardAction, Operation: operation, PayloadHash: strings.Repeat("c", 64), Display: raw}
}

func TestCardsCoverEveryOperation(t *testing.T) {
	s := site(t)
	for operation := range agent.Operations {
		displays := cardsOf[operation]
		require.NotEmpty(t, displays, "%s has no card here", operation)
		_, err := s.Card(actionCard(t, operation, displays[0]), ada, slack.CardOpen{})
		require.NoError(t, err, operation)
	}
	for operation := range cardsOf {
		require.True(t, agent.Operations[operation], "%s isn't an operation", operation)
	}
}

func TestCardRefusals(t *testing.T) {
	s := site(t)
	_, err := s.Card(actionCard(t, "drop_database", agent.OntologyDisplay{}), ada, slack.CardOpen{})
	require.ErrorContains(t, err, `no card for "drop_database"`)
	_, err = s.Card(agent.ActionCard{Operation: "create_load", Display: json.RawMessage(`[1]`)}, ada, slack.CardOpen{})
	require.ErrorContains(t, err, "read the create_load card")
	_, err = s.Card(agent.ActionCard{Operation: "create_load"}, ada, slack.CardOpen{})
	require.Error(t, err)
}

func TestCards(t *testing.T) {
	s := site(t)
	cases := map[string][]slack.Message{}
	operations := make([]string, 0, len(cardsOf))
	for operation := range cardsOf {
		operations = append(operations, operation)
	}
	sort.Strings(operations)
	for _, operation := range operations {
		for i, display := range cardsOf[operation] {
			m, err := s.Card(actionCard(t, operation, display), ada, slack.CardOpen{})
			require.NoError(t, err, operation)
			cases[fmt.Sprintf("%s %d", operation, i)] = []slack.Message{m}
		}
	}
	load := actionCard(t, "create_load", cardsOf["create_load"][0])
	for name, state := range map[string]slack.CardState{
		"ran":                 slack.CardRan{By: ada},
		"ran on the site":     slack.CardRan{},
		"failed":              slack.CardRan{By: ada, Failed: true, Error: "the version <isn't> done"},
		"failed without why":  slack.CardRan{Failed: true},
		"expired":             slack.CardExpired{},
		"dismissed":           slack.CardDismissed{By: grace},
		"dismissed elsewhere": slack.CardDismissed{},
	} {
		m, err := s.Card(load, ada, state)
		require.NoError(t, err, name)
		cases["create_load "+name] = []slack.Message{m}
	}
	long := strings.Repeat("Very long name ", 20)
	m, err := s.Card(actionCard(t, "create_workflow", agent.CreateAutomationDisplay{Name: long}), ada, slack.CardOpen{})
	require.NoError(t, err)
	cases["a long title"] = []slack.Message{m}
	golden(t, "cards", cases)

	// A delete's Run is red and asks first; any other's is green and doesn't.
	actions := func(m slack.Message) slack.Actions {
		for _, b := range m.Blocks {
			if a, ok := b.(slack.Actions); ok {
				return a
			}
		}
		t.Fatal("no buttons")
		return slack.Actions{}
	}
	for _, operation := range operations {
		run := actions(cases[operation+" 0"][0]).Elements[0]
		require.Equal(t, slack.ActionApprove, run.ActionID)
		require.Equal(t, slack.ButtonValue{Action: cardAction, PayloadHash: strings.Repeat("c", 64)}, run.Value)
		if strings.HasPrefix(operation, "delete_") {
			require.Equal(t, slack.StyleDanger, run.Style, operation)
			require.NotNil(t, run.Confirm, operation)
		} else {
			require.Equal(t, slack.StylePrimary, run.Style, operation)
			require.Nil(t, run.Confirm, operation)
		}
	}
	refresh := actions(cases["create_load expired"][0]).Elements[0]
	require.Equal(t, slack.Button{ActionID: slack.ActionRefresh, Text: "Prepare again", Value: slack.ButtonValue{Action: cardAction}}, refresh)
}

func ExampleSite_Card() {
	s, _ := slack.NewSite("https://foundry.helix-db.com")
	display, _ := json.Marshal(agent.CreateLoadDisplay{Ontology: agent.Ref{ID: "o1", Name: "Sales"}, Version: agent.VersionRef{ID: "v", Number: 2}})
	m, _ := s.Card(agent.ActionCard{ActionID: "01JBACTXN0000000000000000A", Operation: "create_load", PayloadHash: strings.Repeat("c", 64), Display: display},
		"U0ADA0001", slack.CardRan{By: "U0ADA0001"})
	fmt.Println(m.Text)
	fmt.Println(m.Blocks[0].(slack.Header).Text)
	// Output:
	// Load v2 of “Sales”
	// Load v2 of “Sales”
}
