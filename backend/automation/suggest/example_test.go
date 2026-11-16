package suggest_test

import (
	"encoding/json"
	"fmt"

	"github.com/helixdb/helix-foundry/backend/automation/suggest"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// A competency question becomes a fallback suggestion: a few words for its
// title, and a weekly report that answers it.
func ExampleFallback() {
	for _, s := range suggest.Fallback("Customer contracts", []string{"Which contracts renew in the next 30 days?"}) {
		fmt.Println(s.Title)
		fmt.Println(s.Prompt)
		fmt.Println(s.Icon)
	}
	// Output:
	// Contracts renew in the next 30
	// Every Monday, answer: "Which contracts renew in the next 30 days?" from Customer contracts, and email me the answer.
	// report
}

// A title drops a leading interrogative only where the rest still reads.
func ExampleTitle() {
	fmt.Println(suggest.Title("How many invoices are overdue?"))
	fmt.Println(suggest.Title("What does each term cost?"))
	fmt.Println(suggest.Title("What is the total revenue?"))
	// Output:
	// Invoices are overdue
	// What does each term cost
	// The total revenue
}

// Parse keeps what passes of each kind of a model's answer and drops the
// rest.
func ExampleParse() {
	kept, err := suggest.Parse(json.RawMessage(`{
	  "questions":[{"title":"Renewals due","prompt":"Which contracts renew in the next 60 days?","topic":"Renewals"}],
	  "dashboards":[{"title":"Revenue","prompt":"Show me revenue by month.","topic":"Revenue"}],
	  "workflows":[
	    {"title":"Renewal radar","prompt":"Every Monday at 9am, list the contracts that renew in the next 30 days and email me the list.","topic":"Renewals","icon":"watch"},
	    {"title":"Open the link","prompt":"Every day, read https://example.com and email me.","topic":"News","icon":"report"}]}`))
	for _, s := range kept {
		fmt.Printf("%s: %s (%s) %q\n", s.Kind, s.Title, s.Topic, s.Icon)
	}
	fmt.Println(err)
	// Output:
	// question: Renewals due (Renewals) ""
	// workflow: Renewal radar (Renewals) "watch"
	// <nil>
}

// Clean checks a suggestion as an answer carries it: a dashboard's prompt
// says dashboard, so Founder makes one at once.
func ExampleSuggestion_Clean() {
	_, err := suggest.Suggestion{Kind: suggest.Dashboard, Title: "Revenue", Prompt: "Show me revenue by month.", Topic: "Revenue"}.Clean()
	fmt.Println(err)
	s, err := suggest.Suggestion{Kind: suggest.Dashboard, Title: " Revenue ", Prompt: "Build me a dashboard of revenue by month.", Topic: "Revenue"}.Clean()
	fmt.Println(s.Title, err)
	// Output:
	// a dashboard's prompt doesn't say dashboard
	// Revenue <nil>
}

// SetupOf reads what an ontology doesn't read or hold yet: its open
// "unmatched" findings, and the connections and document sets its version
// doesn't read.
func ExampleSetupOf() {
	setup := suggest.SetupOf(
		[]*model.FoundryHealFinding{{Kind: "unmatched", Status: model.FoundryHealOpen, Support: 12,
			Finding: json.RawMessage(`{"signal":{"kind":"unmatched","canonical":"supplier_of"}}`)}},
		[]*model.Connector{{ID: "c1", Name: "Stripe", Kind: "stripe"}, {ID: "c2", Name: "Shop database", Kind: "postgres"}},
		[]string{"c1"},
		[]*model.FoundryDocumentSet{{ID: "s1", Name: "Contracts", Documents: 3}},
		nil,
	)
	fmt.Println(setup.Links, setup.Connections, setup.Sets)
	// Output:
	// [{supplier of 12}] [Shop database] [Contracts]
}

// Fill answers with every kind's count: questions from the version's
// competency questions, changes from its setup, and the rest from fixed
// examples until the model has written them.
func ExampleFill() {
	list := suggest.Fill("Customer contracts", []string{"Which contracts renew in the next 30 days?"}, nil,
		suggest.Setup{Links: []suggest.MissingLink{{Words: "supplier of", Claims: 12}}}, true)
	for _, s := range list {
		fmt.Printf("%s/%s/%s: %s\n", s.Kind, s.Source, s.Topic, s.Prompt)
	}
	// Output:
	// question/questions/Question: Which contracts renew in the next 30 days?
	// question/fixed/Question: What does Customer contracts hold? List its types and how many of each it has.
	// dashboard/questions/Dashboard: Build me a dashboard that answers: Which contracts renew in the next 30 days?
	// dashboard/fixed/Dashboard: Build me a dashboard of Customer contracts at a glance: how many of each type it holds and what was added most recently.
	// ontology/setup/Missing link: Draft a new version of Customer contracts with a link for “supplier of”, which 12 claims in your documents state.
	// ontology/fixed/Ontology: Draft a new version of Customer contracts that adds the properties people ask about most but it doesn't hold yet.
	// workflow/questions/Workflow: Every Monday, answer: "Which contracts renew in the next 30 days?" from Customer contracts, and email me the answer.
	// workflow/fixed/Workflow: Every Monday at 9am, summarise what changed in Customer contracts last week and email me.
	// workflow/fixed/Workflow: Every day at 6am, sync my connections, then load Customer contracts.
	// workflow/fixed/Workflow: When a load of Customer contracts is ready, check for records missing key details and email me any it finds.
}

// Starter answers an ontology without a current version: fixed examples,
// its changes helping to draft the first version.
func ExampleStarter() {
	for _, s := range suggest.Starter("Customer contracts", false) {
		fmt.Printf("%s: %s\n", s.Kind, s.Title)
	}
	// Output:
	// question: What it holds
	// question: What changed recently
	// dashboard: Overview
	// dashboard: Data quality
	// ontology: Connect your data
	// ontology: Start from documents
}
