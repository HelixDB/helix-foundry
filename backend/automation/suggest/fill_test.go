package suggest_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/automation/suggest"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

func unmatched(canonical string, support int64) *model.FoundryHealFinding {
	return &model.FoundryHealFinding{Kind: "unmatched", Status: model.FoundryHealOpen, Support: support,
		Finding: json.RawMessage(fmt.Sprintf(`{"signal":{"kind":"unmatched","canonical":%q}}`, canonical))}
}

func TestSetupOf(t *testing.T) {
	findings := []*model.FoundryHealFinding{
		unmatched("works_with", 3),
		unmatched("supplier_of", 12),
		{Kind: "unresolved", Status: model.FoundryHealOpen, Support: 40, Finding: json.RawMessage(`{"signal":{"canonical":"not_a_link"}}`)},
		{Kind: "unmatched", Status: model.FoundryHealDismissed, Support: 50, Finding: json.RawMessage(`{"signal":{"canonical":"dismissed"}}`)},
		{Kind: "unmatched", Status: model.FoundryHealOpen, Support: 30, Finding: json.RawMessage(`not json`)},
		{Kind: "unmatched", Status: model.FoundryHealOpen, Support: 20, Finding: json.RawMessage(`{"signal":{}}`)},
		unmatched("supplier__of", 2), // the same words
	}
	connectors := []*model.Connector{
		{ID: "con-stripe", Name: "Stripe", Kind: "stripe"},
		{ID: "con-pg", Name: "Shop database", Kind: "postgres", Config: json.RawMessage(`{"host":"db.example","database":"shop","user":"reader"}`)},
		{ID: "con-events", Name: "Signups", Kind: model.ConnectorKindEvents},
		{ID: "con-sample", Name: "Acme Corp", Kind: "postgres", Config: json.RawMessage(`{"host":"acme-db","database":"acme","user":"acme_reader"}`)},
		{ID: "con-bad", Name: "Odd", Kind: "postgres", Config: json.RawMessage(`not json`)},
	}
	sets := []*model.FoundryDocumentSet{
		{ID: "set-contracts", Name: "Contracts", Documents: 4},
		{ID: "set-empty", Name: "Empty", Documents: 0},
		{ID: "set-notes", Name: "Notes", Documents: 9, Owner: &model.FoundryDocumentSetOwner{ConnectorID: "con-granola", ConnectorName: "Granola", ConnectorKind: "granola"}},
		{ID: "set-pieces", Name: "Pieces", Documents: 9, Owner: &model.FoundryDocumentSetOwner{ConnectorID: "con-events", ConnectorName: "Signups", ConnectorKind: model.ConnectorKindEvents}},
		{ID: "set-sample", Name: "Acme Corp sample", Documents: 12},
		{ID: "set-read", Name: "Read already", Documents: 3},
	}
	got := suggest.SetupOf(findings, connectors, []string{"con-stripe", "con-stripe"}, sets, []string{"set-read"})
	assert.Equal(t, suggest.Setup{
		Links:       []suggest.MissingLink{{Words: "supplier of", Claims: 12}, {Words: "works with", Claims: 3}},
		Connections: []string{"Shop database", "Odd"},
		Sets:        []string{"Contracts", "Granola / Notes"},
	}, got)

	// Each list is capped.
	findings, connectors, sets = nil, nil, nil
	for i := range suggest.MaxSetup + 2 {
		findings = append(findings, unmatched(fmt.Sprintf("link_%c", 'a'+i), int64(100-i)))
		connectors = append(connectors, &model.Connector{ID: fmt.Sprint(i), Name: fmt.Sprintf("C%d", i), Kind: "stripe"})
		sets = append(sets, &model.FoundryDocumentSet{ID: fmt.Sprint(i), Name: fmt.Sprintf("S%d", i), Documents: 1})
	}
	got = suggest.SetupOf(findings, connectors, nil, sets, nil)
	assert.Len(t, got.Links, suggest.MaxSetup)
	assert.Equal(t, "link a", got.Links[0].Words)
	assert.Len(t, got.Connections, suggest.MaxSetup)
	assert.Len(t, got.Sets, suggest.MaxSetup)

	assert.Equal(t, suggest.Setup{Links: []suggest.MissingLink{}, Connections: []string{}, Sets: []string{}}, suggest.SetupOf(nil, nil, nil, nil, nil))
}

// byKind is the answer's titles of kind.
func byKind(list []suggest.Suggestion, kind suggest.Kind) []suggest.Suggestion {
	out := []suggest.Suggestion{}
	for _, s := range list {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

func sources(list []suggest.Suggestion) []suggest.Source {
	out := []suggest.Source{}
	for _, s := range list {
		out = append(out, s.Source)
	}
	return out
}

var questions = []string{"Which customers are under contract?", "How many invoices are overdue?", "When does each term end?", "What is the total revenue?"}

// Every kind is filled to its count, in kind order, whatever there is to
// fill it from.
func TestFillWithoutAModel(t *testing.T) {
	got := suggest.Fill("Customer contracts", questions, nil, suggest.Setup{}, true)
	require.Len(t, got, suggest.MaxSuggestions)
	assert.Equal(t, []suggest.Kind{suggest.Question, suggest.Question, suggest.Dashboard, suggest.Dashboard, suggest.Ontology, suggest.Ontology,
		suggest.Workflow, suggest.Workflow, suggest.Workflow, suggest.Workflow}, kinds(got))
	assert.Equal(t, []suggest.Suggestion{
		{Kind: suggest.Question, Title: "Customers are under contract", Prompt: "Which customers are under contract?", Topic: "Question", Source: suggest.SourceQuestions},
		{Kind: suggest.Question, Title: "Invoices are overdue", Prompt: "How many invoices are overdue?", Topic: "Question", Source: suggest.SourceQuestions},
		{Kind: suggest.Dashboard, Title: "When does each term end", Prompt: "Build me a dashboard that answers: When does each term end?", Topic: "Dashboard",
			Source: suggest.SourceQuestions},
		{Kind: suggest.Dashboard, Title: "The total revenue", Prompt: "Build me a dashboard that answers: What is the total revenue?", Topic: "Dashboard",
			Source: suggest.SourceQuestions},
	}, got[:4], "dashboards answer the questions after the first two")
	assert.Equal(t, []suggest.Source{suggest.SourceFixed, suggest.SourceFixed}, sources(got[4:6]), "no setup: the fixed changes")
	assert.Equal(t, "Draft a new version of Customer contracts that adds the properties people ask about most but it doesn't hold yet.", got[4].Prompt)
	assert.Equal(t, []string{"Customers are under contract", "Invoices are overdue", "When does each term end", "The total revenue"}, titles(got[6:]))
	assert.Equal(t, []suggest.Source{suggest.SourceQuestions, suggest.SourceQuestions, suggest.SourceQuestions, suggest.SourceQuestions}, sources(got[6:]))

	// Fewer than four questions: dashboards wrap round to the first two,
	// and workflows end with fixed examples.
	got = suggest.Fill("Customer contracts", questions[:1], nil, suggest.Setup{}, true)
	assert.Equal(t, []suggest.Source{suggest.SourceQuestions, suggest.SourceFixed}, sources(byKind(got, suggest.Question)))
	dashboards := byKind(got, suggest.Dashboard)
	assert.Equal(t, "Build me a dashboard that answers: Which customers are under contract?", dashboards[0].Prompt)
	assert.Equal(t, suggest.SourceFixed, dashboards[1].Source)
	workflows := byKind(got, suggest.Workflow)
	assert.Equal(t, []suggest.Source{suggest.SourceQuestions, suggest.SourceFixed, suggest.SourceFixed, suggest.SourceFixed}, sources(workflows))
	assert.Equal(t, []string{"Customers are under contract", "Monday brief", "Fresh data daily", "Load check"}, titles(workflows))

	// No questions: every kind is fixed.
	got = suggest.Fill("Customer contracts", nil, nil, suggest.Setup{}, true)
	require.Len(t, got, suggest.MaxSuggestions)
	for _, s := range got {
		assert.Equal(t, suggest.SourceFixed, s.Source, s.Title)
		assert.Equal(t, strings.ToUpper(string(s.Kind[:1]))+string(s.Kind[1:]), s.Topic, s.Title)
	}
}

func TestFillPrefersTheModel(t *testing.T) {
	written, err := suggest.Parse(answer(fullAnswer()))
	require.NoError(t, err)
	got := suggest.Fill("Customer contracts", questions, written, suggest.Setup{}, true)
	require.Len(t, got, suggest.MaxSuggestions)
	assert.Equal(t, []string{"Customers are under contract", "Invoices are overdue"}, titles(byKind(got, suggest.Question)), "questions come first")
	assert.Equal(t, []string{"Revenue overview", "Renewals board"}, titles(byKind(got, suggest.Dashboard)))
	assert.Equal(t, []string{"Renewal dates", "Contract rules"}, titles(byKind(got, suggest.Ontology)))
	assert.Equal(t, []string{"Renewal radar", "Contract check", "Stripe refresh", "Monday brief"}, titles(byKind(got, suggest.Workflow)))
	for _, s := range got[2:] {
		assert.Equal(t, suggest.SourceModel, s.Source, s.Title)
	}

	// Without questions, the model's questions answer; a short answer is
	// filled from the rest; a written source is always the model's.
	short := []suggest.Suggestion{written[0], written[6]}
	short[1].Source = ""
	got = suggest.Fill("Customer contracts", nil, short, suggest.Setup{}, true)
	assert.Equal(t, []suggest.Source{suggest.SourceModel, suggest.SourceFixed}, sources(byKind(got, suggest.Question)))
	assert.Equal(t, []suggest.Source{suggest.SourceModel, suggest.SourceFixed, suggest.SourceFixed, suggest.SourceFixed},
		sources(byKind(got, suggest.Workflow)))

	// What the store held that doesn't pass is skipped.
	bad := []suggest.Suggestion{{Kind: suggest.Dashboard, Title: "No word", Prompt: "Show me revenue.", Topic: "Revenue"}}
	got = suggest.Fill("Customer contracts", nil, bad, suggest.Setup{}, true)
	assert.Equal(t, []suggest.Source{suggest.SourceFixed, suggest.SourceFixed}, sources(byKind(got, suggest.Dashboard)))
}

func TestFillDedupesWithinAKind(t *testing.T) {
	written := []suggest.Suggestion{
		{Kind: suggest.Question, Title: "Same", Prompt: "which customers are under contract?", Topic: "Contracts"},
		{Kind: suggest.Workflow, Title: "Repeat", Icon: suggest.Report, Topic: "Contracts",
			Prompt: `EVERY MONDAY, ANSWER: "WHICH CUSTOMERS ARE UNDER CONTRACT?" FROM CUSTOMER CONTRACTS, AND EMAIL ME THE ANSWER.`},
	}
	got := suggest.Fill("Customer contracts", questions[:1], written, suggest.Setup{}, true)
	q := byKind(got, suggest.Question)
	assert.Equal(t, []suggest.Source{suggest.SourceQuestions, suggest.SourceFixed}, sources(q), "the model's repeats the question's prompt")
	w := byKind(got, suggest.Workflow)
	assert.Equal(t, "Repeat", w[0].Title)
	assert.NotEqual(t, suggest.SourceQuestions, w[1].Source, "the fallback repeats the model's prompt")

	// The same prompt in two kinds stays.
	got = suggest.Fill("Sales", []string{"Which deals closed?"}, nil, suggest.Setup{}, true)
	assert.Equal(t, "Which deals closed?", byKind(got, suggest.Question)[0].Prompt)
	assert.Equal(t, `Every Monday, answer: "Which deals closed?" from Sales, and email me the answer.`, byKind(got, suggest.Workflow)[0].Prompt)
}

func TestFillChangesFromSetup(t *testing.T) {
	setup := suggest.Setup{
		Links:       []suggest.MissingLink{{Words: "supplier of", Claims: 12}, {Words: "works with", Claims: 1}},
		Connections: []string{"Shop database"},
		Sets:        []string{"Granola / Notes"},
	}
	written, err := suggest.Parse(answer(fullAnswer()))
	require.NoError(t, err)
	got := byKind(suggest.Fill("Customer contracts", questions, written, setup, true), suggest.Ontology)
	assert.Equal(t, []suggest.Suggestion{
		{Kind: suggest.Ontology, Title: "Add the “supplier of” link", Topic: "Missing link", Source: suggest.SourceSetup,
			Prompt: "Draft a new version of Customer contracts with a link for “supplier of”, which 12 claims in your documents state."},
		{Kind: suggest.Ontology, Title: "Add the “works with” link", Topic: "Missing link", Source: suggest.SourceSetup,
			Prompt: "Draft a new version of Customer contracts with a link for “works with”, which 1 claim in your documents states."},
	}, got, "setup comes before the model's")

	got = byKind(suggest.Fill("Customer contracts", nil, nil, suggest.Setup{Connections: setup.Connections, Sets: setup.Sets}, true), suggest.Ontology)
	assert.Equal(t, []suggest.Suggestion{
		{Kind: suggest.Ontology, Title: "Add Shop database", Topic: "New source", Source: suggest.SourceSetup,
			Prompt: "Draft a new version of Customer contracts that also reads Shop database."},
		{Kind: suggest.Ontology, Title: "Add Granola / Notes", Topic: "New documents", Source: suggest.SourceSetup,
			Prompt: "Draft a new version of Customer contracts that also reads the documents in Granola / Notes."},
	}, got)

	// Long names take a short title; a name no prompt could hold is skipped.
	long := strings.Repeat("x", 40)
	got = byKind(suggest.Fill("Customer contracts", nil, nil, suggest.Setup{
		Links:       []suggest.MissingLink{{Words: long, Claims: 2}, {Words: "see http links", Claims: 2}},
		Connections: []string{long, "con_123 <b>"},
		Sets:        []string{long},
	}, true), suggest.Ontology)
	assert.Equal(t, []string{"Add a missing link", "Add a connection"}, titles(got))
	got = byKind(suggest.Fill("Customer contracts", nil, nil, suggest.Setup{Sets: []string{long}}, true), suggest.Ontology)
	assert.Equal(t, "Add a document set", got[0].Title)
}

func TestFillWithoutWorkflows(t *testing.T) {
	got := suggest.Fill("Customer contracts", questions, nil, suggest.Setup{}, false)
	assert.Len(t, got, 6)
	assert.Empty(t, byKind(got, suggest.Workflow))
	assert.Empty(t, byKind(suggest.Starter("Customer contracts", false), suggest.Workflow))
}

func TestStarter(t *testing.T) {
	got := suggest.Starter("Customer contracts", true)
	require.Len(t, got, suggest.MaxSuggestions)
	for _, s := range got {
		assert.Equal(t, suggest.SourceFixed, s.Source)
		assert.Contains(t, s.Prompt, "Customer contracts")
	}
	assert.Equal(t, []string{
		"Help me connect my data and draft the first version of Customer contracts.",
		"Help me upload documents and draft the first version of Customer contracts from them.",
	}, []string{got[4].Prompt, got[5].Prompt})

	// A name no prompt could hold is "this ontology".
	got = suggest.Starter("See https://example.com", true)
	require.Len(t, got, suggest.MaxSuggestions)
	assert.Equal(t, "Help me connect my data and draft the first version of this ontology.", got[4].Prompt)
}

// Every fixed example passes Clean with a name as long as a prompt may
// hold, so every kind always has its count.
func TestFixedExamplesFitAnyName(t *testing.T) {
	for _, name := range []string{"S", strings.Repeat("n", 300), "Customer contracts"} {
		assert.Len(t, suggest.Fill(name, nil, nil, suggest.Setup{}, true), suggest.MaxSuggestions, name)
		assert.Len(t, suggest.Starter(name, true), suggest.MaxSuggestions, name)
	}
}

func TestClean(t *testing.T) {
	s, err := suggest.Suggestion{Kind: suggest.Workflow, Title: " Radar ", Prompt: okWorkflow, Topic: " Renewals ", Icon: suggest.Watch,
		Source: suggest.SourceFixed}.Clean()
	require.NoError(t, err)
	assert.Equal(t, suggest.Suggestion{Kind: suggest.Workflow, Title: "Radar", Prompt: okWorkflow, Topic: "Renewals", Icon: suggest.Watch,
		Source: suggest.SourceFixed}, s)
	s, err = suggest.Suggestion{Kind: suggest.Ontology, Title: "Dates", Prompt: okChange, Topic: "Renewals", Icon: suggest.Watch}.Clean()
	require.NoError(t, err)
	assert.Empty(t, s.Icon, "only a workflow keeps its icon")
}
