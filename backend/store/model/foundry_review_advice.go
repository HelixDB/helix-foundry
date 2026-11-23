package model

import (
	"encoding/json"
	"time"

	"github.com/helixdb/helix-foundry/backend/reviewadvice"
	"github.com/helixdb/helix-foundry/backend/reviewcards"
)

// FoundryReviewAdviceStop is why the advisor's last claim of a load stopped:
// a closed set, foundry_review_advice_loads.stopped.
type FoundryReviewAdviceStop string

const (
	// FoundryReviewAdviceDone: no card left to advise.
	FoundryReviewAdviceDone FoundryReviewAdviceStop = "done"
	// FoundryReviewAdviceLimit: the claim sent foundry.reviews.advise.
	// max_per_load cards, or considered ten times that.
	FoundryReviewAdviceLimit FoundryReviewAdviceStop = "limit"
	// FoundryReviewAdviceBudget: the workspace's month of advice is spent.
	FoundryReviewAdviceBudget FoundryReviewAdviceStop = "budget"
	// FoundryReviewAdvicePlan: a hosted workspace on no sold plan.
	FoundryReviewAdvicePlan FoundryReviewAdviceStop = "plan"
	// FoundryReviewAdviceNoModel: no model advises the workspace: none
	// resolves, a hosted workspace's own provider, Ollama, or a server
	// taking one call at a time.
	FoundryReviewAdviceNoModel FoundryReviewAdviceStop = "no_model"
	// FoundryReviewAdviceFailed: the model's provider failed three claims
	// in a row.
	FoundryReviewAdviceFailed FoundryReviewAdviceStop = "failed"
)

// FoundryReviewAdviceClaim is a worker's claim of a load owed advice, held
// for a lease: OwedAt is when it was last owed, which a re-owe during the
// claim moves on. Its back-offs and failures are the row's, counted by the
// store as they happen.
type FoundryReviewAdviceClaim struct {
	LoadID      string
	WorkspaceID string
	OntologyID  string
	OwedAt      time.Time
}

// FoundryReviewAdviceFailure is what failed when a claim backs off.
type FoundryReviewAdviceFailure int

const (
	// FoundryReviewAdviceReadFailed: a read of Foundry's store, the
	// database or the plan; it is retried and never stops the load.
	FoundryReviewAdviceReadFailed FoundryReviewAdviceFailure = iota
	// FoundryReviewAdviceProviderFailed: the model's provider; three claims
	// in a row stop the load, failed.
	FoundryReviewAdviceProviderFailed
)

// FoundryReviewAdviceOutcome is what came of one card: a closed set,
// foundry_review_advice.outcome.
type FoundryReviewAdviceOutcome string

const (
	// FoundryReviewAdvised: the model picked one of the card's choices.
	FoundryReviewAdvised FoundryReviewAdviceOutcome = "advised"
	// FoundryReviewAdviceNone: the model was asked and picked none, or its
	// answer was unusable after every call.
	FoundryReviewAdviceNone FoundryReviewAdviceOutcome = "none"
	// FoundryReviewAdviceRules: a rule covers the card; it wasn't sent.
	FoundryReviewAdviceRules FoundryReviewAdviceOutcome = "rules"
	// FoundryReviewAdviceSkipped: never sent: its review is gone from the
	// load, or the card is too large to send whole.
	FoundryReviewAdviceSkipped FoundryReviewAdviceOutcome = "skipped"
)

// FoundryReviewAdviceRow is one card's advice as the advisor stores it.
// Offered, Model and PromptVersion are an asked card's (advised or none);
// Choice, Rationale and Cites an advised card's.
type FoundryReviewAdviceRow struct {
	CardID        reviewcards.ID
	Outcome       FoundryReviewAdviceOutcome
	Offered       []reviewadvice.Choice
	Choice        *reviewadvice.Choice
	Rationale     string
	Cites         []reviewadvice.Cite
	Model         string
	PromptVersion string
}

// FoundryReviewAdvice is an advised card's advice as a listing reads it:
// its choice, the choices it was offered and its cites as stored (JSON of
// reviewadvice.Choice, []Choice and []Cite), and its rationale.
type FoundryReviewAdvice struct {
	Choice    json.RawMessage
	Offered   json.RawMessage
	Rationale string
	Cites     json.RawMessage
}
