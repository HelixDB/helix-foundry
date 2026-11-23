package model

import (
	"time"

	"github.com/helixdb/helix-foundry/backend/reviewcards"
)

// FoundryReviewQuarantine is how many failed reads set a document's owed
// rebuild aside: it is claimed no more, and counts as indexed, until it is
// owed again (a new Extract of it, or the load's next full pass).
const FoundryReviewQuarantine = 8

// FoundryReviewSummary is what an ontology's current load asks a person to
// decide: its open cards, at most 10,000 and whether there are more, and
// whether the index is still being built (its count is then too low).
type FoundryReviewSummary struct {
	OntologyID string
	LoadID     string
	Open       int
	More       bool
	Indexing   bool
}

// FoundryReviewOwed is one document of a load whose cards are owed a
// rebuild, as it was owed when claimed.
type FoundryReviewOwed struct {
	DocID  string
	OwedAt time.Time
}

// FoundryReviewClaim is a worker's claim of a load's owed documents, held
// for a lease: what it reads of the graph and stores back, if the lease
// still holds then.
type FoundryReviewClaim struct {
	LoadID      string
	OntologyID  string
	WorkspaceID string
	VersionID   string
	Owed        []FoundryReviewOwed
}

// DocIDs are the claimed documents' ids.
func (c FoundryReviewClaim) DocIDs() []string {
	ids := make([]string, len(c.Owed))
	for i, o := range c.Owed {
		ids[i] = o.DocID
	}
	return ids
}

// FoundryReviewIndexed is what a read of a claimed document found: whether
// the load's graph holds it, its cards and its quotes of facts by band.
type FoundryReviewIndexed struct {
	DocID   string
	Present bool
	Cards   []*reviewcards.Card
	Quotes  [6]uint32
}

// FoundryReviewCardFilter is which cards a walk lists: one lane, kinds (none
// is every kind), a type (a card's, or either end's of a held claim) and a
// document.
type FoundryReviewCardFilter struct {
	Lane       reviewcards.Lane
	Kinds      []reviewcards.Kind
	Type       string
	DocumentID string
}

// FoundryReviewCardCursor is where a page of cards starts: after the card
// with these keys, in the walk's order.
type FoundryReviewCardCursor struct {
	PerDecision float64
	Facts       int
	KindOrder   int
	Shuffle     uint32
	ID          reviewcards.ID
}

// FoundryReviewListedCard is a card of a page, and for a name the held
// claims that wait on it while it is open.
type FoundryReviewListedCard struct {
	*reviewcards.Card
	Held []*reviewcards.Card
}

// FoundryReviewNameDecisions are the resolve decisions of one name across
// the ontology's documents: how many, how many choices they make (an
// entity, or not in the data), and the newest, at most 20.
type FoundryReviewNameDecisions struct {
	Type     string
	NormName string
	Total    int
	Choices  int
	Newest   []*FoundryReviewDecision
}

// FoundryReviewBandPrecision is how often a band's claims drawn at random
// were right: each place's first label.
type FoundryReviewBandPrecision struct {
	Band  string
	Right int
	Total int
}

// FoundryReviewPrecision is the spot checks' word per band, and how often a
// second person agreed with a re-check.
type FoundryReviewPrecision struct {
	Bands          []FoundryReviewBandPrecision
	RechecksAgreed int
	RechecksTotal  int
}

// FoundryReviewDrawKind is what a spot check drew.
type FoundryReviewDrawKind string

const (
	// FoundryReviewDrawHeld is a held claim, decided or not.
	FoundryReviewDrawHeld FoundryReviewDrawKind = "held"
	// FoundryReviewDrawQuote is a fact's quote the load holds.
	FoundryReviewDrawQuote FoundryReviewDrawKind = "quote"
)

// FoundryReviewDraw is one spot check of a load, at its place: a held claim
// (its key and the hash of its statement) or a fact's quote (its fact's
// key). A draw never changes once made.
type FoundryReviewDraw struct {
	LoadID     string
	Seq        int
	Kind       FoundryReviewDrawKind
	Band       string
	DocID      string
	Span       FoundryClaimSpan
	Link       *string
	Confidence *float64
	// ClaimKey and StatementHash are a held draw's; FactKey a quote's.
	ClaimKey      *string
	StatementHash *string
	FactKey       *string
	// ReviewKey is a held draw's review while the load's cards still hold
	// its claim; empty once its card is gone (its approval written).
	ReviewKey string
	// Done says someone labelled the place at random, in any load.
	Done bool
}
