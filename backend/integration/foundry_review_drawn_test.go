package integration

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
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

// A decision carries what became of it in the current load, a withdraw's
// included, so an agent can tell a take-back done from one refused; one the
// load's list doesn't hold carries nothing.
func TestFoundryReviewDecisionApplied(t *testing.T) {
	at := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	out := FoundryReviewDecision(&foundryv1.FoundryReviewDecision{Id: "w", Kind: "withdraw",
		Applied: &foundryv1.FoundryDecisionApplied{Status: "refused", Reason: "differs", At: timestamppb.New(at)}})
	if out.Applied == nil || *out.Applied != (FoundryDecisionAppliedOutput{Status: "refused", Reason: "differs", At: "2026-11-13T09:00:00Z"}) {
		t.Fatalf("applied %+v", out.Applied)
	}
	release := FoundryReviewDecision(&foundryv1.FoundryReviewDecision{Id: "r", Kind: "release",
		Applied: &foundryv1.FoundryDecisionApplied{Status: "written", TakesBack: true}})
	if release.Applied == nil || !release.Applied.TakesBack || release.Applied.At != "" {
		t.Fatalf("release applied %+v", release.Applied)
	}
	raw, err := json.Marshal(FoundryReviewDecision(&foundryv1.FoundryReviewDecision{Id: "d", Kind: "resolve"}))
	if err != nil || strings.Contains(string(raw), `"applied"`) {
		t.Fatalf("%s %v", raw, err)
	}
}
