package ontologyagent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func ExampleAsksAboutName() {
	fmt.Println(AsksAboutName("ambiguous"), AsksAboutName("adjudicated"))
	// Output: true false
}

func ExampleAskingDocuments() {
	docs, past, _ := AskingDocuments([]byte(`{"items":[
	  {"doc_id":"doc-b","reason":"unresolved"},
	  {"doc_id":"doc-b","reason":"ambiguous"},
	  {"doc_id":"doc-c","reason":"collision","entity_key":null},
	  {"doc_id":"doc-d","reason":"unresolved","entity_key":"acme"},
	  {"doc_id":"doc-e","reason":"adjudicated"},
	  {"doc_id":"","reason":"unresolved"}]}`))
	fmt.Println(docs, past)
	// Output: [doc-b doc-c] false
}

// The read names the load, the review label, the type and the name, each as
// a string constant, and reads AskingReviewsMax reviews at most.
func TestAskingQuery(t *testing.T) {
	body, err := AskingQuery("load-1", "Customer", `acme "corp"`)
	if err != nil {
		t.Fatal(err)
	}
	eq := func(property, value string) string {
		v, _ := json.Marshal(value)
		return `{"eq":{"left":{"property":"` + property + `"},"right":{"constant":{"string":` + string(v) + `}}}}`
	}
	want := `{"request_type":"read","query":{"read":{"entries":[{"query":{"name":"items","root":{"value_map":{"input":{"limit":{
	  "input":{"nodes_where":{"predicate":{"and":{"predicates":[` + eq("foundry_load", "load-1") + `,` + eq("$label", "FoundryReview") + `,` +
		eq("type", "Customer") + `,` + eq("norm_name", `acme "corp"`) + `]}}}},
	  "count":{"literal":300000}}},"properties":["doc_id","reason","entity_key"]}}}}],"returns":["items"]}}}`
	var got, wanted any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wanted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wanted) {
		t.Fatalf("%s", body)
	}
}

// Every document that asks is kept, however many (the engine indexes
// reviews by name from contract 4.24.0): only a read that fills
// AskingReviewsMax says some may be left out. An answer that isn't JSON is
// an error.
func TestAskingDocumentsPastTheBound(t *testing.T) {
	item := func(i int) string { return `{"doc_id":"doc-` + strconv.Itoa(i) + `","reason":"unresolved"}` }
	var many []string
	for i := range 50000 {
		many = append(many, item(i))
	}
	docs, past, err := AskingDocuments([]byte(`{"items":[` + strings.Join(many, ",") + `]}`))
	if err != nil || past || len(docs) != 50000 || docs[0] != "doc-0" || docs[49999] != "doc-49999" {
		t.Fatalf("%d docs, past %v, %v", len(docs), past, err)
	}
	full := make([]string, AskingReviewsMax)
	for i := range full {
		full[i] = item(i % 7)
	}
	docs, past, err = AskingDocuments([]byte(`{"items":[` + strings.Join(full, ",") + `]}`))
	if err != nil || !past || len(docs) != 7 {
		t.Fatalf("a full read: %d docs, past %v, %v", len(docs), past, err)
	}
	if _, _, err := AskingDocuments([]byte(`<html>`)); err == nil {
		t.Fatal("not JSON")
	}
}
