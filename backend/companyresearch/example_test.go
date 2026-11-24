package companyresearch_test

import (
	"encoding/json"
	"fmt"

	"github.com/helixdb/helix-foundry/backend/companyresearch"
)

// A domain is read however it is typed, down to its registrable domain; a
// personal email provider's is refused, in words the person is shown.
func ExampleNormalizeDomain() {
	for _, typed := range []string{"https://www.Acme.co.uk/about", "ada@acme.com", "gmail.com"} {
		domain, err := companyresearch.NormalizeDomain(typed)
		fmt.Printf("%q %v\n", domain, err)
	}
	// Output:
	// "acme.co.uk" <nil>
	// "acme.com" <nil>
	// "" That’s a personal email provider’s domain: enter your company’s website instead.
}

// ParseResearch numbers the questions it keeps and names the ontology after
// the company when the model didn't.
func ExampleParseResearch() {
	r, err := companyresearch.ParseResearch(json.RawMessage(`{"company_name":"Acme","summary":"Acme sells anvils.","questions":[
	  {"question":"Who will use it?","best_guess":"Sales","options":["Sales","Support"]},
	  {"question":"","best_guess":"dropped"}]}`), companyresearch.Site{})
	fmt.Println(err, r.SuggestedName, r.Fetched)
	for _, q := range r.Questions {
		fmt.Println(q.ID, q.Question, q.BestGuess, q.Options)
	}
	// Output:
	// <nil> Acme operations false
	// q1 Who will use it? Sales [Sales Support]
}

// Answered keeps the answers given, and refuses one to a question the
// research didn't ask.
func ExampleResearch_Answered() {
	r := companyresearch.Research{Questions: []companyresearch.Question{{ID: "q1", Question: "Who will use it?"}}}
	fmt.Println(r.Answered([]companyresearch.Answer{{QuestionID: "q1", Text: " The sales team "}}))
	fmt.Println(r.Answered([]companyresearch.Answer{{QuestionID: "q7", Text: "Anyone"}}))
	// Output:
	// map[q1:The sales team] <nil>
	// map[] the research asked no question "q7"
}
