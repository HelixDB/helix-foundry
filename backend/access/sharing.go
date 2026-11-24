package access

import (
	"cmp"
	"maps"
	"slices"
	"time"
)

// MaxAudienceAge is how old the last read of a connector's audiences may be
// and still say what a reader reads of it: past it, every item reads as
// unknown, and the connector's documents go to full readers only
// (plans/SOURCE_ACLS.md).
const MaxAudienceAge = 7 * 24 * time.Hour

// MaxFailingAudienceAge is how old that read may be once a later read of
// them failed: a source that stops answering stops vouching for who was
// removed from an item sooner.
const MaxFailingAudienceAge = 24 * time.Hour

// AudiencesFresh reports whether the last read of a connector's audiences
// that worked, at readAt (nil: none has), still says who reads its items at
// now: within MaxAudienceAge, or MaxFailingAudienceAge once a read after it
// failed (problemAt, nil for none). A read dated after now (another clock)
// is fresh.
//
//	read := time.Now().Add(-48 * time.Hour)
//	access.AudiencesFresh(&read, nil, time.Now())   // true
//	failed := read.Add(time.Hour)
//	access.AudiencesFresh(&read, &failed, time.Now()) // false: over a day old, and a read since failed
func AudiencesFresh(readAt, problemAt *time.Time, now time.Time) bool {
	if readAt == nil {
		return false
	}
	limit := MaxAudienceAge
	if problemAt != nil && problemAt.After(*readAt) {
		limit = MaxFailingAudienceAge
	}
	return now.Sub(*readAt) <= limit
}

// SourceReach is what one reader reads of one connector whose source reports
// who each of its items is shared with (a Slack connector's channels): the
// items it reads, when they are known. The zero SourceReach is unknown: the
// reader reads none of its items.
type SourceReach struct {
	// items are the item keys the reader reads; nil for unknown.
	items map[string]bool
}

// ReachOf is the reach that a connector's audiences, last read at readAt
// (nil: none has worked) with a failed read at problemAt (nil: none since),
// give a reader the source shares visible of its items with, as of now:
// unknown unless AudiencesFresh.
//
//	at := time.Now()
//	access.ReachOf(&at, nil, []string{"C1"}, at).Sees("C1") // true
func ReachOf(readAt, problemAt *time.Time, visible []string, now time.Time) SourceReach {
	if !AudiencesFresh(readAt, problemAt, now) {
		return SourceReach{}
	}
	return SharedReach(visible)
}

// SharedReach is the known reach of a connector whose source shares visible
// of its items with the reader, as the API tells a grantee what it reads now
// (GetMyFoundryAccess's source_items): Founder's worker reads a call's part
// of a load with it.
//
//	access.SharedReach([]string{"C1"}).Sees("C1") // true
//	access.SharedReach(nil).Known()               // true: it reads none of them
func SharedReach(visible []string) SourceReach {
	r := SourceReach{items: make(map[string]bool, len(visible))}
	for _, key := range visible {
		r.items[key] = true
	}
	return r
}

// Sees reports whether the reader reads item key.
func (r SourceReach) Sees(key string) bool { return r.items[key] }

// Known reports whether the connector's items are known: a read of them
// worked recently enough (AudiencesFresh).
func (r SourceReach) Known() bool { return r.items != nil }

// ItemRef is one item of an audienced connector's source (a Slack channel).
type ItemRef struct{ ConnectorID, Item string }

// ItemOf is the item a document of a connector's set belongs to, by the
// set's stream and the document's external id; ok is false for a document of
// no item the connector's kind knows.
type ItemOf func(connectorID, stream, externalID string) (item string, ok bool)

// Sharing is what one reader reads of each of a workspace's connectors whose
// sources report their permissions, by connector id. A connector it doesn't
// name is no such connector: grants alone say what of it is read. The zero
// Sharing names none, so it narrows nothing.
type Sharing struct {
	reach  map[string]SourceReach
	itemOf ItemOf
}

// NewSharing is reach, by audienced connector id, with itemOf telling which
// item each of their documents is of. A connector of reach whose reach is the
// zero SourceReach is one whose items are unknown.
func NewSharing(reach map[string]SourceReach, itemOf ItemOf) Sharing {
	return Sharing{reach: reach, itemOf: itemOf}
}

// audienced reports whether connectorID's source reports its permissions:
// no grant gives all of it, since the items its documents and rows are of
// may be ones the last read of them didn't list (left, deleted, or joined
// since).
func (s Sharing) audienced(connectorID string) bool {
	_, ok := s.reach[connectorID]
	return ok && connectorID != ""
}

// some reports whether the reader reads anything of connectorID that grants
// give it: an item, for an audienced one.
func (s Sharing) some(connectorID string) bool {
	r, ok := s.reach[connectorID]
	return !ok || len(r.items) > 0
}

// item is the item of an audienced connector's source that d is of; ok is
// false for a document of a set no audienced connector owns, or of no item
// its kind knows.
func (s Sharing) item(d DocumentRef) (ItemRef, bool) {
	if !s.audienced(d.Set.ConnectorID) || s.itemOf == nil {
		return ItemRef{}, false
	}
	item, ok := s.itemOf(d.Set.ConnectorID, d.Stream, d.ExternalID)
	return ItemRef{ConnectorID: d.Set.ConnectorID, Item: item}, ok
}

// reads reports whether the reader reads d as far as its source goes: any
// document of a set no audienced connector owns, else one whose item the
// source shares with the reader. A document of no item the kind knows, or of
// a connector whose items are unknown, it doesn't.
func (s Sharing) reads(d DocumentRef) bool {
	if !s.audienced(d.Set.ConnectorID) {
		return true
	}
	i, ok := s.item(d)
	return ok && s.sees(i)
}

// sees reports whether the source of i's connector shares i with the reader
// now.
func (s Sharing) sees(i ItemRef) bool { return s.reach[i.ConnectorID].Sees(i.Item) }

// visible are the items every audienced source shares with the reader now,
// sorted.
func (s Sharing) visible() []ItemRef {
	var out []ItemRef
	for _, id := range slices.Sorted(maps.Keys(s.reach)) {
		for _, item := range slices.Sorted(maps.Keys(s.reach[id].items)) {
			out = append(out, ItemRef{ConnectorID: id, Item: item})
		}
	}
	return out
}

// SortItems sorts items by connector, then item, without repeats.
//
//	access.SortItems([]access.ItemRef{{"s", "C2"}, {"s", "C1"}, {"s", "C2"}}) // [{s C1} {s C2}]
func SortItems(items []ItemRef) []ItemRef {
	slices.SortFunc(items, func(a, b ItemRef) int {
		return cmp.Or(cmp.Compare(a.ConnectorID, b.ConnectorID), cmp.Compare(a.Item, b.Item))
	})
	return slices.Compact(items)
}
