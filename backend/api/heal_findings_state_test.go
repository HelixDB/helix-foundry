package api

import (
	"testing"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
	"google.golang.org/protobuf/encoding/protojson"
)

// A finding that names a relationship to add has a link and a set; others none.
func TestUnmatchedLink(t *testing.T) {
	for name, c := range map[string]struct {
		finding        string
		canonical, set string
		ok             bool
	}{
		"unmatched":         {`{"signal":{"kind":"unmatched","canonical":"ceo_of","set":"set_1"}}`, "ceo_of", "set_1", true},
		"unmatched, no set": {`{"signal":{"kind":"unmatched","canonical":"ceo_of"}}`, "ceo_of", "", true},
		"no canonical":      {`{"signal":{"kind":"unmatched"}}`, "", "", false},
		"another kind":      {`{"signal":{"kind":"clash","canonical":"x"}}`, "", "", false},
		"not JSON":          {`{`, "", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			canonical, set, ok := unmatchedLink(&foundrymodel.FoundryHealFinding{Finding: []byte(c.finding)})
			if canonical != c.canonical || set != c.set || ok != c.ok {
				t.Fatalf("got %q %q %v", canonical, set, ok)
			}
		})
	}
}

// Each state on the wire, as grpc-gateway writes it for the site.
func TestFoundryHealStateProto(t *testing.T) {
	v8 := foundrymodel.FoundryVersionRef{ID: "ver-8", Version: 8}
	for name, c := range map[string]struct {
		state foundrymodel.FoundryFindingState
		json  string
	}{
		"open":    {foundrymodel.FoundryFindingOpen{}, `{"open":{}}`},
		"covered": {foundrymodel.FoundryFindingCovered{Version: v8}, `{"covered":{"versionId":"ver-8","version":"8"}}`},
		"moving":  {foundrymodel.FoundryFindingMoving{Version: v8}, `{"moving":{"versionId":"ver-8","version":"8"}}`},
		"moved": {foundrymodel.FoundryFindingMoved{Version: v8, LinksAdded: []string{"ceo_of"}},
			`{"moved":{"versionId":"ver-8","version":"8","linksAdded":["ceo_of"]}}`},
		"needs a load": {foundrymodel.FoundryFindingNeedsLoad{Version: v8, Why: []foundrymodel.FoundryFoldWhy{{Kind: "type_added", Item: "types.Deal"}}},
			`{"needsLoad":{"versionId":"ver-8","version":"8","why":[{"kind":"type_added","item":"types.Deal"}]}}`},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := protojson.Marshal(foundryHealStateProto(c.state))
			if err != nil {
				t.Fatal(err)
			}
			if !jsonEqual(t, raw, c.json) {
				t.Fatalf("got %s, want %s", raw, c.json)
			}
		})
	}
	if foundryHealStateProto(nil) != (*foundryv1.FoundryHealFindingState)(nil) {
		t.Fatal("no state is no state")
	}
}
