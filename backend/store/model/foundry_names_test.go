package model

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
)

func ExampleMergeFoundryDocumentNames() {
	id := "01JDDEC"
	held := []FoundryDocumentName{{Decision: FoundryNameResolve, Type: "Customer", NormName: "acme", DecisionID: &id, Outcome: FoundryNameWritten}}
	// Sent again, the decision written finds nothing waiting on it.
	again := []FoundryDocumentName{{Decision: FoundryNameResolve, Type: "Customer", NormName: "acme", DecisionID: &id, Outcome: FoundryNameNotAsked}}
	for _, n := range MergeFoundryDocumentNames(held, again) {
		fmt.Println(n.NormName, n.Outcome)
	}
	// Output: acme written
}

// A later event of a document the load holds adds to its names: an entry is
// one decision of one name, and a later word on it replaces the earlier one,
// except not_asked, which says only that nothing waited on it this time, and
// not_replayed, which says only that the send had no model work to settle it.
func TestMergeFoundryDocumentNames(t *testing.T) {
	a, b := "dec-a", "dec-b"
	name := func(decision FoundryNameDecision, norm string, id *string, outcome FoundryNameOutcomeKind, reason FoundryNameRefusal) FoundryDocumentName {
		return FoundryDocumentName{Decision: decision, Type: "Customer", NormName: norm, DecisionID: id, Outcome: outcome, Reason: reason}
	}
	held := []FoundryDocumentName{
		name(FoundryNameResolve, "acme", &a, FoundryNameWritten, ""),
		name(FoundryNameAlias, "helio", &b, FoundryNameRefused, FoundryNameNotReplayed),
		name(FoundryNameAlias, "initech", nil, FoundryNameNotAsked, ""),
	}
	for title, tc := range map[string]struct {
		earlier, later, want []FoundryDocumentName
	}{
		"nothing held":        {nil, held, held},
		"nothing said":        {held, nil, held},
		"not_asked stays out": {held, []FoundryDocumentName{name(FoundryNameResolve, "acme", &a, FoundryNameNotAsked, "")}, held},
		// A resend carries no model work: its not_replayed leaves the
		// commit's word, a written name or a refusal, as it was.
		"not_replayed stays out": {
			[]FoundryDocumentName{held[0], name(FoundryNameAlias, "globex", &b, FoundryNameRefused, FoundryNameTwoRows)},
			[]FoundryDocumentName{name(FoundryNameResolve, "acme", &a, FoundryNameRefused, FoundryNameNotReplayed),
				name(FoundryNameAlias, "globex", &b, FoundryNameRefused, FoundryNameNotReplayed)},
			[]FoundryDocumentName{held[0], name(FoundryNameAlias, "globex", &b, FoundryNameRefused, FoundryNameTwoRows)}},
		"a later word replaces in place": {held,
			[]FoundryDocumentName{name(FoundryNameAlias, "helio", &b, FoundryNameWritten, "")},
			[]FoundryDocumentName{held[0], name(FoundryNameAlias, "helio", &b, FoundryNameWritten, ""), held[2]}},
		"a word after not_asked replaces it": {held,
			[]FoundryDocumentName{name(FoundryNameAlias, "initech", nil, FoundryNameRefused, "a_later_reason")},
			[]FoundryDocumentName{held[0], held[1], name(FoundryNameAlias, "initech", nil, FoundryNameRefused, "a_later_reason")}},
		// Kind, type, name and id each tell decisions apart: the same name
		// as an alias, or under another id, is another entry, added after.
		"other keys are added": {held,
			[]FoundryDocumentName{
				name(FoundryNameAlias, "acme", &a, FoundryNameWritten, ""),
				name(FoundryNameResolve, "acme", &b, FoundryNameNotNamed, ""),
				{Decision: FoundryNameResolve, Type: "Supplier", NormName: "acme", DecisionID: &a, Outcome: FoundryNameNotAsked},
			},
			append(append([]FoundryDocumentName{}, held...),
				name(FoundryNameAlias, "acme", &a, FoundryNameWritten, ""),
				name(FoundryNameResolve, "acme", &b, FoundryNameNotNamed, ""),
				FoundryDocumentName{Decision: FoundryNameResolve, Type: "Supplier", NormName: "acme", DecisionID: &a, Outcome: FoundryNameNotAsked})},
	} {
		t.Run(title, func(t *testing.T) {
			before := slices.Clone(tc.earlier)
			if got := MergeFoundryDocumentNames(tc.earlier, tc.later); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
			if !reflect.DeepEqual(before, tc.earlier) {
				t.Fatal("the earlier list changed")
			}
		})
	}
}

func ExampleFoundryNameApplied() {
	for _, n := range []FoundryDocumentName{
		{Outcome: FoundryNameWritten},
		{Outcome: FoundryNameRefused, Reason: FoundryNameNotReplayed},
		{Outcome: FoundryNameNotAsked},
		{Outcome: FoundryNameNotNamed},
	} {
		status, reason := FoundryNameApplied(n)
		fmt.Printf("%s %q\n", status, reason)
	}
	// Output:
	// written ""
	// refused "not_replayed"
	// written "not_asked"
	// refused "not_named"
}
