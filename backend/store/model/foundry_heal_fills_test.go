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
			Why: []FoundryFoldWhy{{Kind: "type_added", Item: "types.Deal"}}}
	}
	for name, c := range map[string]struct {
		fill    *FoundryHealFill
		covered *FoundryVersionRef
		want    FoundryFindingState
	}{
		"nothing done":          {nil, nil, FoundryFindingOpen{}},
		"a version adds it":     {nil, &v8, FoundryFindingCovered{Version: v8}},
		"moving":                {fill(FoundryFillMoving), &v8, FoundryFindingMoving{Version: v8}},
		"moved":                 {fill(FoundryFillMoved), nil, FoundryFindingMoved{Version: v8, LinksAdded: []string{"ceo_of"}}},
		"needs a load":          {fill(FoundryFillNeedsLoad), &v8, FoundryFindingNeedsLoad{Version: v8, Why: []FoundryFoldWhy{{"type_added", "types.Deal"}}}},
		"a fill after the move": {fill(FoundryFillApplying), nil, FoundryFindingMoved{Version: v8, LinksAdded: []string{"ceo_of"}}},
		"a fill stopped":        {fill(FoundryFillStopped), nil, FoundryFindingMoved{Version: v8, LinksAdded: []string{"ceo_of"}}},
		"a fill done":           {fill(FoundryFillFilled), nil, FoundryFindingMoved{Version: v8, LinksAdded: []string{"ceo_of"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := FoundryFindingStateOf(c.fill, c.covered); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %#v, want %#v", got, c.want)
			}
		})
	}
}
