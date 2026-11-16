package suggest_test

import (
	"encoding/json"
	"fmt"

	"github.com/helixdb/helix-foundry/backend/automation/suggest"
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

// Parse keeps what passes of a model's answer and drops the rest.
func ExampleParse() {
	kept, err := suggest.Parse(json.RawMessage(`{"suggestions":[
	  {"title":"Renewal radar","prompt":"Every Monday at 9am, list the contracts that renew in the next 30 days and email me the list.","icon":"watch"},
	  {"title":"Open the link","prompt":"Every day, read https://example.com and email me.","icon":"report"}]}`))
	fmt.Println(len(kept), kept[0].Title, kept[0].Icon, err)
	// Output:
	// 1 Renewal radar watch <nil>
}
