// Package reviewcards builds the review cards of a load's documents: what
// the review walk asks a person to decide, one card a decision, built per
// document from the document's reviews as the load's graph holds them. It
// is pure: the graph worker's indexer reads the graph (ReadQuery,
// ParseRead), builds (Build) and stores the cards, and the API pages them;
// the walk renders them.
//
// A card is one of:
//   - a name (kind resolve): every review of one name of one document,
//     settled by one resolve decision. It writes, from the next load, every
//     accepted claim it blocks there, and lets each held claim that waits on
//     it be approved;
//   - a held claim (kind release): one claim a verifier doubted, approved or
//     rejected on its own. One that waits on a name points at the name's
//     card (Card.OnCard): it folds into that card while the name is open,
//     and is a card of its own once the name is decided. One no approval
//     writes (read two ways at its place, or a conflict on a link that
//     holds many values) is in the pool lane, for spot checks only;
//   - a value (kind prop): a property documents state differently.
//
// Cards go by facts written per decision (PerDecision), then facts, then
// held claims before names before values (KindOrder), then an order drawn
// from the load's id (Shuffle). A card's id hashes its decision's identity,
// never its review's key, so a review keyed again keeps its card.
//
// This is the rule set of the site's review queue (src/web/areas/reviews:
// queue.ts buildQueue, model.ts settleable and markDiffering), moved here so
// counts and order are the whole load's; the mock serves the same rules
// (mock/review-cards.ts), and testdata/cards.json is run against both.
package reviewcards

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	// Version is the builder's version. A load whose last full pass ran an
	// older one is indexed again from scratch; raise it whenever Build would
	// make other cards of the same graph.
	Version = 1
	// FullPassEvery is how soon after a load's last full pass another may
	// start, but for a load never indexed or indexed by an older Version.
	FullPassEvery = time.Hour
	// RefreshEvery is how old a load's last full pass may grow before the
	// indexer asks for another, catching what nothing else asked for.
	RefreshEvery = 24 * time.Hour
	// MaxReviewKeys is how many review keys a name's card names: its
	// settling review first, then the rest in document order.
	MaxReviewKeys = 200
	// SpotChecks is how many undone spot checks a load keeps drawn.
	SpotChecks = 5
)

// Lane is where a card goes in the walk.
type Lane string

const (
	// LaneDecide is a card to decide.
	LaneDecide Lane = "decide"
	// LaneFYI is a card for information: a name only the model linked, or
	// a value whose table row's value stands.
	LaneFYI Lane = "fyi"
	// LanePool is a held claim no approval writes: drawn for spot checks,
	// never asked.
	LanePool Lane = "pool"
)

// Kind is the decision a card takes, as foundry_review_decisions names it.
type Kind string

const (
	KindName  Kind = "resolve"
	KindValue Kind = "prop"
	KindClaim Kind = "release"
)

// KindOrder is a kind's place among cards that rank and write alike.
func (k Kind) KindOrder() int {
	switch k {
	case KindClaim:
		return 0
	case KindName:
		return 1
	default:
		return 2
	}
}

// Bands are the score bands of the spot checks, in the order draws take
// them: accepted facts' first.
var Bands = [6]string{"0.9", "0.8", "0.7", "0.6", "low", "none"}

// BandOf is a verifier's score's band, an index into Bands: "none" for no
// score, or for 0, a claim with no score that a person approved.
//
// Example: BandOf(ptr(0.93)) is 0 ("0.9"), BandOf(nil) is 5 ("none").
func BandOf(confidence *float64) int {
	switch c := confidence; {
	case c == nil || math.IsNaN(*c) || *c <= 0:
		return 5
	case *c >= 0.9:
		return 0
	case *c >= 0.8:
		return 1
	case *c >= 0.7:
		return 2
	case *c >= 0.6:
		return 3
	default:
		return 4
	}
}

// HashUnit is a hash of parts as a number in [0, 1): the first four bytes of
// the SHA-256 of the parts joined by U+001F, over 2³², as the site's
// hashUnit computes it, so a draw or an order is the same wherever made.
func HashUnit(parts ...any) float64 {
	return float64(hashWord(parts...)) / (1 << 32)
}

func hashWord(parts ...any) uint32 {
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = fmt.Sprint(p)
	}
	sum := sha256.Sum256([]byte(strings.Join(texts, "\x1f")))
	return binary.BigEndian.Uint32(sum[:4])
}

// StatementHash binds an approval to the claim a person read: the SHA-256 of
// its link and its statement as stored, as the engine hashes it
// (held::Statement::hash), in lowercase hexadecimal.
func StatementHash(link, stored string) string {
	sum := sha256.Sum256([]byte(link + "\x1f" + stored))
	return hex.EncodeToString(sum[:])
}

// ID is a card's id: the SHA-256 of its decision's identity.
type ID [32]byte

func cardID(parts ...any) ID {
	texts := make([]string, len(parts))
	for i, p := range parts {
		texts[i] = fmt.Sprint(p)
	}
	return sha256.Sum256([]byte(strings.Join(texts, "\x1f")))
}

// NameID is a name card's id: its type, document and normalized name.
func NameID(ty, docID, normName string) ID { return cardID("name", ty, docID, normName) }

// ValueID is a value card's id: its document, type, entity and property. The
// document is part of it, as two documents' reviews of one value are two
// cards of the load (one decision settles both).
func ValueID(docID, ty, entityKey, prop string) ID {
	return cardID("prop", docID, ty, entityKey, prop)
}

// ClaimID is a held claim's id: its document, place and claim key.
func ClaimID(docID string, section, start, end uint32, claimKey string) ID {
	return cardID("release", docID, section, start, end, claimKey)
}

// Shuffle is a card's place among cards that rank alike, drawn from the
// load's id: HashUnit(load, "order", id in hex) × 2³².
func Shuffle(loadID string, id ID) uint32 {
	return hashWord(loadID, "order", hex.EncodeToString(id[:]))
}
