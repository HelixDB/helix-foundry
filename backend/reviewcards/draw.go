package reviewcards

import (
	"math"
	"slices"
)

// Pool is what a band's spot checks are drawn from: its held claims (decided
// or not, the pool lane's included) with no draw and no label at their
// place, and its quotes of facts less those drawn.
type Pool struct {
	Held   int
	Quotes int
}

// Draw is what the nth draw of a load (seq n) takes: a band, and whether a
// quote of a fact or a held claim.
type Draw struct {
	Band  int
	Quote bool
}

// Next is load loadID's draw n from pools, by band: the bands in turn from
// Bands[n mod 6], passing over one with nothing left, then a held claim or a
// quote in proportion to how many of each the band holds, drawn from a hash
// of the load and n. ok is false when every band is empty.
//
// Example: with only quotes in "0.8", every draw is a quote of "0.8".
func Next(loadID string, n int, pools [6]Pool) (d Draw, ok bool) {
	for i := range Bands {
		band := (n + i) % len(Bands)
		p := pools[band]
		if p.Held+p.Quotes <= 0 {
			continue
		}
		quote := p.Quotes > 0 && HashUnit(loadID, "pick", n)*float64(p.Held+p.Quotes) >= float64(p.Held)
		return Draw{Band: band, Quote: quote}, true
	}
	return Draw{}, false
}

// QuoteTarget is which of a band's quotes draw n of load loadID aims at: an
// offset into the band's count, which picks a document weighted by how many
// of its quotes are in the band.
func QuoteTarget(loadID string, band, n, quotes int) int {
	return min(int(math.Floor(HashUnit(loadID, "quote", Bands[band], n)*float64(quotes))), max(quotes-1, 0))
}

// QuoteOrder is the order a document's quotes are drawn in: the lowest first
// of those no label or draw holds.
func QuoteOrder(loadID, docID string, quotes []Quote) []Quote {
	ordered := slices.Clone(quotes)
	unit := func(q Quote) float64 { return HashUnit(loadID, docID, q.Section, q.Start, q.End, q.Link) }
	slices.SortStableFunc(ordered, func(a, b Quote) int {
		switch ua, ub := unit(a), unit(b); {
		case ua < ub:
			return -1
		case ua > ub:
			return 1
		}
		return 0
	})
	return ordered
}

// Rechecked reports whether a decision or a label of id is one a second
// person is asked to re-check: HashUnit("recheck", id) under 0.05, as SQL
// computes it (CONV(LEFT(SHA2(…), 8), 16, 10) < 214748365).
func Rechecked(id string) bool { return hashWord("recheck", id) < 214748365 }
