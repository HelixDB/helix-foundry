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
