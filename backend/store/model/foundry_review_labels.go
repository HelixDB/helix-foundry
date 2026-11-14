package model

import (
	"fmt"
	"math"
	"time"
)

// FoundryLabelDrawn is how a label came to be: a card of the review queue's
// random slice, or a second person's re-check of another's word.
type FoundryLabelDrawn string

const (
	FoundryLabelRandom  FoundryLabelDrawn = "random"
	FoundryLabelRecheck FoundryLabelDrawn = "recheck"
)

// FoundryVerdict is a person's word on a claim.
type FoundryVerdict string

const (
	FoundryVerdictRight FoundryVerdict = "right"
	FoundryVerdictWrong FoundryVerdict = "wrong"
)

// FoundryReviewLabel is a person's word on a claim at its place, kept to
// measure precision and read into nothing else. The claim is a held claim
// (ClaimKey and the StatementHash read, kept as its decision of the random
// slice is saved) or an accepted fact's quote (FactKey), never both, at DocID
// and Span. Confidence is the verifier's score as the claim was read, so a
// label keeps its band once the review or the decision is gone. A re-check
// (Drawn recheck) carries the verdict it checks (Against): the decision's
// (an approval right, a rejection wrong) or the other person's label's.
// DecisionID is the decision a held claim's label came with, or the one
// re-checked. One label per person and claim (store.SaveFoundryReviewLabel).
type FoundryReviewLabel struct {
	ID              string
	OntologyID      string
	LoadID          string
	Drawn           FoundryLabelDrawn
	DocID           string
	Span            FoundryClaimSpan
	ClaimKey        *string
	StatementHash   *string
	FactKey         *string
	Link            *string
	Confidence      *float64
	Verdict         FoundryVerdict
	DecisionID      *string
	Against         *FoundryVerdict
	DecidedByUserID string
	DecidedAt       time.Time
}

// FoundryFactKeyMax is the longest fact key a label stores.
const FoundryFactKeyMax = 128

// Validate guards the label's shape at every storage boundary, as the
// table's CHECK does.
func (l FoundryReviewLabel) Validate() error {
	if l.ID == "" || l.OntologyID == "" || l.LoadID == "" || l.DocID == "" || l.DecidedByUserID == "" {
		return fmt.Errorf("label identity and actor are required")
	}
	if l.Drawn != FoundryLabelRandom && l.Drawn != FoundryLabelRecheck {
		return fmt.Errorf("invalid label drawn %q", l.Drawn)
	}
	if !l.Verdict.valid() {
		return fmt.Errorf("invalid verdict %q", l.Verdict)
	}
	if l.Span.Start >= l.Span.End {
		return fmt.Errorf("label of an empty span")
	}
	held := l.ClaimKey != nil || l.StatementHash != nil
	switch {
	case held && l.FactKey != nil:
		return fmt.Errorf("a label is of a held claim or an accepted fact, not both")
	case held && (!foundrySHA256(l.ClaimKey) || !foundrySHA256(l.StatementHash)):
		return fmt.Errorf("invalid held claim label")
	case !held && (l.FactKey == nil || !foundryFactKey(*l.FactKey)):
		return fmt.Errorf("invalid fact label")
	}
	if (l.Drawn == FoundryLabelRecheck) != (l.Against != nil) || (l.Against != nil && !l.Against.valid()) {
		return fmt.Errorf("a re-check, and only a re-check, says what it checks")
	}
	if c := l.Confidence; c != nil && (math.IsNaN(*c) || *c < 0 || *c > 1) {
		return fmt.Errorf("confidence out of range")
	}
	if l.Link != nil && (*l.Link == "" || len(*l.Link) > 255) {
		return fmt.Errorf("invalid link")
	}
	return nil
}

func (v FoundryVerdict) valid() bool { return v == FoundryVerdictRight || v == FoundryVerdictWrong }

// ParseFoundryVerdict reads a verdict the API was sent.
func ParseFoundryVerdict(s string) (FoundryVerdict, error) {
	if v := FoundryVerdict(s); v.valid() {
		return v, nil
	}
	return "", fmt.Errorf("verdict must be right or wrong, not %q", s)
}

// foundryFactKey reports whether key is one a label stores: printable ASCII,
// at most FoundryFactKeyMax bytes. The engine's keys are hexadecimal digests.
func foundryFactKey(key string) bool {
	if key == "" || len(key) > FoundryFactKeyMax {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] <= ' ' || key[i] > '~' {
			return false
		}
	}
	return true
}
