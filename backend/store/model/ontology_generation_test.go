package model

import (
	"encoding/json"
	"fmt"
	"testing"
)

func ExampleOntologyEdits() {
	edits := OntologyEdits{
		OntologyPut{ItemID: "rule.overdue", TOML: "[rules.overdue]"},
		OntologyRemove{ItemID: "constraint.old"},
	}
	raw, _ := json.Marshal(edits)
	fmt.Println(string(raw))
	var back OntologyEdits
	_ = json.Unmarshal(raw, &back)
	fmt.Println(back[0].Item(), back[1].Item())
	// Output:
	// [{"put":{"item_id":"rule.overdue","toml":"[rules.overdue]"}},{"remove":{"item_id":"constraint.old"}}]
	// rule.overdue constraint.old
}

func TestOntologyEdits(t *testing.T) {
	edits := OntologyEdits{OntologyPut{ItemID: "extraction.contracts", TOML: ""}, OntologyRemove{ItemID: "constraint.old"}}
	raw, err := json.Marshal(edits)
	if err != nil {
		t.Fatal(err)
	}
	var got OntologyEdits
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != edits[0] || got[1] != edits[1] {
		t.Fatalf("%#v", got)
	}
	if raw, _ := json.Marshal(OntologyEdits{}); string(raw) != `[]` {
		t.Fatalf("no edits = %s", raw)
	}
	if _, err := json.Marshal(OntologyEdits{nil}); err == nil {
		t.Fatal("wrote a nil edit")
	}
	for _, raw := range []string{
		`{`, `{}`, `[null]`, `[{}]`, `[{"put":"wrong"}]`, `[{"put":{"item_id":"rule.x"}}]`, `[{"put":{"toml":"x"}}]`,
		`[{"remove":{"item_id":"rule.x","toml":""}}]`, `[{"remove":{}}]`, `[{"unknown":{"item_id":"rule.x"}}]`,
		`[{"put":{"item_id":"rule.x","toml":""},"remove":{"item_id":"rule.x"}}]`, `[{"put":{"item_id":"rule.x","toml":"","extra":1}}]`,
	} {
		var edits OntologyEdits
		if err := json.Unmarshal([]byte(raw), &edits); err == nil {
			t.Errorf("accepted %s as %#v", raw, edits)
		}
	}
}

// A question reads and writes as the ontology agent does: its recommended
// answer named default, and header and default null for none.
func TestOntologyQuestionJSON(t *testing.T) {
	for _, raw := range []string{
		`{"id":"Q1","items":["type.Customer"],"header":"Customers","text":"Is a customer an account?",` +
			`"options":[{"label":"An account","description":"One per billing account"},{"label":"A person"}],"multiple":false,"default":"An account","blocking":true}`,
		`{"id":"Q2","items":[],"header":null,"text":"Which sources?","options":[{"label":"Stripe"},{"label":"Documents"}],"multiple":true,"default":null,"blocking":false}`,
		`{"id":"Q3","items":[],"header":null,"text":"Which currency?","options":[],"multiple":false,"default":"GBP","blocking":false}`,
	} {
		var q OntologyQuestion
		if err := json.Unmarshal([]byte(raw), &q); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		back, err := json.Marshal(q)
		if err != nil || string(back) != raw {
			t.Errorf("%s wrote back as %s (%v)", raw, back, err)
		}
	}
	var q OntologyQuestion
	_ = json.Unmarshal([]byte(`{"id":"Q3","items":[],"text":"Which currency?","default":"GBP","blocking":false}`), &q)
	if q.Choice != nil || q.Recommended != "GBP" || q.Items == nil {
		t.Errorf("a free answer with suggested words: %+v", q)
	}
	for _, bad := range []string{
		`{"id":"Q1","items":[],"text":"Which?","options":[],"multiple":true,"blocking":true}`,
		`{"id":"Q1","items":[],"text":"Which?","options":[{"label":"A"},{"label":"B"}],"default":"C","blocking":true}`,
		`{"id":"Q1","items":[],"text":"Which?","options":[{"label":"A"}],"blocking":true}`,
		`{"id":"","items":[],"text":"Which?","blocking":true}`,
	} {
		if err := json.Unmarshal([]byte(bad), &q); err == nil {
			t.Errorf("%s read as %+v", bad, q)
		}
	}
}
