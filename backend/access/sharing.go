package access

import (
	"cmp"
	"slices"
	"time"
)

// MaxAudienceAge is how old the last read of a connector's audiences may be
// and still say what a reader reads of it: past it, every item reads as
// unknown, and the connector's documents and tables go to full readers only
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
// items it reads, when they are known, and whether the source has an account
// for the reader that isn't a guest's. The zero SourceReach is unknown: the
// reader reads none of its items, and none of its tables.
type SourceReach struct {
	// items are the item keys the reader reads; nil for unknown.
	items map[string]bool
	// member says the source has a non-guest account for the reader, who
	// then reads its directory of people (RowsDirectory).
	member bool
}

// DirectoryItem is the item that stands for an audienced source's directory
// of its people (RowsDirectory, Slack's users) wherever a reader's items are
// told or recorded: the source shares it with each reader it has a non-guest
// account for (Shared.Member), so VisibleItems names it and the API tells it
// (GetMyFoundryAccess's source_items), and a Part that reads the directory
// is of it, so what that part said stops being the reader's once they stop
// being one of the source's people (Keeps, SeesItems). No source's own item
// is it: "*" is in none of their keys.
const DirectoryItem = "*directory"

// Shared is what a source shares with one reader at its last read: the keys
// of the items it shares with them, and whether it has an account for them
// by their email that isn't a guest's (Member), which a person matched to it
// is and an agent's key, a team or an unmatched person never is. DirectoryItem
// among Items says Member too, as the API tells it.
type Shared struct {
	Items  []string
	Member bool
}

// ReachOf is the reach that a connector's audiences, last read at readAt
// (nil: none has worked) with a failed read at problemAt (nil: none since),
// give a reader the source shares shared with, as of now: unknown unless
// AudiencesFresh.
//
//	at := time.Now()
//	access.ReachOf(&at, nil, access.Shared{Items: []string{"C1"}}, at).Sees("C1") // true
func ReachOf(readAt, problemAt *time.Time, shared Shared, now time.Time) SourceReach {
	if !AudiencesFresh(readAt, problemAt, now) {
		return SourceReach{}
	}
	return SharedReach(shared)
}

// SharedReach is the known reach of a connector whose source shares shared
// with the reader, as the API tells a grantee what it reads now
// (GetMyFoundryAccess's source_items, Member as DirectoryItem): Founder's
// worker reads a call's part of a load with it.
//
//	access.SharedReach(access.Shared{Items: []string{"C1"}}).Sees("C1") // true
//	access.SharedReach(access.Shared{}).Known()                        // true: it reads none of them
//	access.SharedReach(access.Shared{Items: []string{access.DirectoryItem}}).Sees(access.DirectoryItem) // true: a member
func SharedReach(shared Shared) SourceReach {
	r := SourceReach{items: make(map[string]bool, len(shared.Items)), member: shared.Member}
	for _, key := range shared.Items {
		if key == DirectoryItem {
			r.member = true
			continue
		}
		r.items[key] = true
	}
	return r
}

// Sees reports whether the reader reads item key: DirectoryItem while the
// source has a non-guest account for them.
func (r SourceReach) Sees(key string) bool {
	if key == DirectoryItem {
		return r.member
	}
	return r.items[key]
}

// Known reports whether the connector's items are known: a read of them
// worked recently enough (AudiencesFresh).
func (r SourceReach) Known() bool { return r.items != nil }

// ItemRef is one item of an audienced connector's source (a Slack channel).
type ItemRef struct{ ConnectorID, Item string }

// SourceKind is what Sharing reads of an audienced connector's kind
// (connector.Audienced): which item each document of its sets is of, and how
// each of its tables tells which item its rows are of.
type SourceKind interface {
	// ItemOfDocument is the item a document of stream belongs to, by its
	// external id; ok is false for a document of no item the kind knows.
	ItemOfDocument(stream, externalID string) (item string, ok bool)
	// TableRows is how table tells its rows' items; the zero RowRule for a
	// table whose rows it doesn't tell apart, which full readers only read.
	TableRows(table string) RowRule
}

// Sharing is what one reader reads of each of a workspace's connectors whose
// sources report their permissions, by connector id. A connector it doesn't
// name is no such connector: grants alone say what of it is read. The zero
// Sharing names none, so it narrows nothing.
type Sharing struct {
	reach map[string]SourceReach
	kinds map[string]SourceKind
}

// NewSharing is reach, by audienced connector id, with kinds telling which
// item each of their documents and rows is of. A connector of reach whose
// reach is the zero SourceReach is one whose items are unknown; one kinds
// doesn't name tells no document's or row's item.
func NewSharing(reach map[string]SourceReach, kinds map[string]SourceKind) Sharing {
	return Sharing{reach: reach, kinds: kinds}
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
	kind, ok := s.kinds[d.Set.ConnectorID]
	if !s.audienced(d.Set.ConnectorID) || !ok {
		return ItemRef{}, false
	}
	item, ok := kind.ItemOfDocument(d.Stream, d.ExternalID)
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
// its directory (DirectoryItem) among them while it is one of the source's
// people, sorted.
func (s Sharing) visible() []ItemRef {
	var out []ItemRef
	for id, reach := range s.reach {
		if reach.member {
			out = append(out, ItemRef{ConnectorID: id, Item: DirectoryItem})
		}
		for item := range reach.items {
			out = append(out, ItemRef{ConnectorID: id, Item: item})
		}
	}
	return SortItems(out)
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
