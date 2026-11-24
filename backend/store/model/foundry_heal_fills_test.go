package model

import (
	"reflect"
	"testing"
)

// Each state a finding reads as, from its fill row and a covering version.
func TestFoundryFindingStateOf(t *testing.T) {
	v8 := FoundryVersionRef{ID: "ver-8", Version: 8}
	fill := func(state FoundryHealFillState) *FoundryHealFill {
		return &FoundryHealFill{ToVersionID: "ver-8", ToVersion: 8, State: state, LinksAdded: []string{"ceo_of"},
			Why: []FoundryFoldWhy{{Kind: "type_added", Item: "types.Deal"}}, Counts: FoundryFillCounts{ReviewsTaken: 2}}
	}
	untaken := fill(FoundryFillFilled)
	untaken.Counts = FoundryFillCounts{Owed: 3, Refused: 2, Nothing: 1}
	for name, c := range map[string]struct {
		fill    *FoundryHealFill
		covered *FoundryVersionRef
		want    FoundryFindingState
	}{
		"nothing done":          {nil, nil, FoundryFindingOpen{}},
		"a version adds it":     {nil, &v8, FoundryFindingCovered{Version: v8}},
		"moving":                {fill(FoundryFillMoving), &v8, FoundryFindingMoving{Version: v8}},
		"moved":                 {fill(FoundryFillMoved), nil, FoundryFindingMoved{Version: v8, LinksAdded: []string{"ceo_of"}, FillPending: true}},
		"needs a load":          {fill(FoundryFillNeedsLoad), &v8, FoundryFindingNeedsLoad{Version: v8, Why: []FoundryFoldWhy{{"type_added", "types.Deal"}}}},
		"a fill after the move": {fill(FoundryFillApplying), nil, FoundryFindingApplying{Version: v8, Fill: *fill(FoundryFillApplying)}},
		"a fill stopped":        {fill(FoundryFillStopped), &v8, FoundryFindingStopped{Version: v8, Fill: *fill(FoundryFillStopped)}},
		"a fill done":           {fill(FoundryFillFilled), nil, FoundryFindingFixed{Version: v8, Fill: *fill(FoundryFillFilled)}},
		"a fill that took no review": {untaken, nil, FoundryFindingNeedsLoad{Version: v8,
			Why: []FoundryFoldWhy{{Kind: FoundryWhyNotFilled, Item: "2"}}}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := FoundryFindingStateOf(c.fill, c.covered); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %#v, want %#v", got, c.want)
			}
		})
	}
}

// A fill's done documents are those that ended, however each did.
func TestFoundryFillCountsDone(t *testing.T) {
	c := FoundryFillCounts{Owed: 9, Left: 3, Filled: 2, Nothing: 1, Refused: 1, Native: 1, ReviewsTaken: 7, FactsAdded: 4}
	if c.Done() != 5 {
		t.Fatalf("done %d, want 5", c.Done())
	}
	if len(FoundryFillBackoff) != 3 || FoundryFillBackoff[0] >= FoundryFillBackoff[2] {
		t.Fatalf("backoff %v grows over three failures", FoundryFillBackoff)
	}
}
