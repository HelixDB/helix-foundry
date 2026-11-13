package integration

import (
	"encoding/json"
	"strings"
	"testing"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// A decision made on a card of the review queue lists how it was drawn; one
// made elsewhere lists no draw at all, so a client reading the schema before
// it sees nothing new.
func TestFoundryReviewDecisionDrawn(t *testing.T) {
	out := FoundryReviewDecision(&foundryv1.FoundryReviewDecision{Id: "d", Kind: "release", Drawn: "random"})
	if out.Drawn != "random" {
		t.Fatalf("drawn %q", out.Drawn)
	}
	raw, err := json.Marshal(FoundryReviewDecision(&foundryv1.FoundryReviewDecision{Id: "d", Kind: "resolve"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "drawn") {
		t.Fatalf("a decision made elsewhere lists a draw: %s", raw)
	}
}
