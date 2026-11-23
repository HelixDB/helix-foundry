package ontologyagent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeQueryOutput(t *testing.T) {
	half := 2.5
	for raw, want := range map[string]QueryOutput{
		`{"kind":"rows","rows":[{"cells":[1,{"untrusted":"x"}],"edges":[]}],"total":3,"cut":"rows past 1"}`: RowsOutput{
			Rows: []OutputRow{{Cells: []json.RawMessage{json.RawMessage(`1`), json.RawMessage(`{"untrusted":"x"}`)}, Edges: []json.RawMessage{}}}, Total: 3, Cut: "rows past 1"},
		`{"kind":"rows","rows":[],"total":0}`:                       RowsOutput{Rows: []OutputRow{}},
		`{"kind":"count","count":12}`:                               CountOutput{Count: 12},
		`{"kind":"aggregate","value":2.5,"values":4,"skipped":1}`:   AggregateOutput{Value: &half, Values: 4, Skipped: 1},
		`{"kind":"aggregate","value":null,"values":0,"cut":"some"}`: AggregateOutput{Cut: "some"},
		`{"kind":"group_count","groups":[{"value":null,"count":3}],"total_groups":5}`: GroupCountOutput{
			Groups: []GroupCount{{Value: json.RawMessage(`null`), Count: 3}}, TotalGroups: 5},
		`{"kind":"grouped_aggregate","groups":[{"value":"a","aggregate":2.5,"values":2,"skipped":1},{"value":"b","aggregate":null,"values":0}],"total_groups":2}`: GroupedAggregateOutput{
			Groups: []GroupAggregate{{Value: json.RawMessage(`"a"`), Aggregate: &half, Values: 2, Skipped: 1}, {Value: json.RawMessage(`"b"`), Values: 0}}, TotalGroups: 2},
	} {
		got, err := DecodeQueryOutput(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %#v, want %#v", raw, got, want)
		}
	}
	rows := `{"kind":"rows","rows":[` + strings.TrimSuffix(strings.Repeat(`{"cells":[],"edges":[]},`, maxOutputRows+1), ",") + `],"total":1}`
	groups := `{"kind":"group_count","groups":[` + strings.TrimSuffix(strings.Repeat(`{"value":1,"count":1},`, maxOutputGroups+1), ",") + `],"total_groups":1}`
	for _, raw := range []string{
		`[]`, `{"kind":"table"}`, `{}`, `{"kind":"count"}`, `{"kind":"count","count":-1}`, `{"kind":"count","count":1,"cut":3}`,
		`{"kind":"rows","total":1}`, `{"kind":"rows","rows":[]}`, rows, `{"kind":"rows","rows":[{"cells":[]}],"total":1}`,
		`{"kind":"aggregate","values":1}`, `{"kind":"aggregate","value":"1","values":1}`, `{"kind":"aggregate","value":1}`,
		`{"kind":"group_count","total_groups":1}`, `{"kind":"group_count","groups":[]}`, groups,
		`{"kind":"group_count","groups":[{"count":1}],"total_groups":1}`, `{"kind":"group_count","groups":[{"value":1}],"total_groups":1}`,
		`{"kind":"grouped_aggregate","groups":[{"value":1,"values":1}],"total_groups":1}`,
		`{"kind":"grouped_aggregate","groups":[{"value":1,"aggregate":"x","values":1}],"total_groups":1}`,
		`{"kind":"grouped_aggregate","groups":[{"value":1,"aggregate":1}],"total_groups":1}`,
	} {
		if _, err := DecodeQueryOutput(json.RawMessage(raw)); err == nil {
			t.Fatalf("%.80s: no error", raw)
		}
	}
}

func ExampleDecodeQueryOutput() {
	out, _ := DecodeQueryOutput(json.RawMessage(`{"kind":"group_count","groups":[{"value":{"untrusted":"paid"},"count":40}],"total_groups":1}`))
	groups := out.(GroupCountOutput)
	fmt.Println(string(groups.Groups[0].Value), groups.Groups[0].Count, groups.TotalGroups)
	// Output: {"untrusted":"paid"} 40 1
}
