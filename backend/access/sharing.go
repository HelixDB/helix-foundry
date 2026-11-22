package access

import "time"

// MaxAudienceAge is how old the last read of a connector's audiences may be
// and still say what a reader reads of it: past it, every item reads as
// unknown, and the connector's documents and tables go to full readers only
// (plans/SOURCE_ACLS.md).
const MaxAudienceAge = 7 * 24 * time.Hour

// reachKind is how much of an audienced connector's items one reader reads.
type reachKind uint8

const (
	// reachUnknown: the items aren't known, never read or read too long ago;
	// the reader reads none of them.
	reachUnknown reachKind = iota
	// reachSome: the reader reads the items it lists, not every one.
	reachSome
	// reachAll: the reader reads every item the last read listed.
	reachAll
)

// SourceReach is what one reader reads of one connector whose source reports
// who each of its items is shared with (a Slack connector's channels). The
// zero SourceReach is unknown: the reader reads none of its items.
type SourceReach struct {
	kind reachKind
	// items are the item keys the reader reads, by key; nil for unknown.
	items map[string]bool
}

// ReachOf is the reach a read of a connector's audiences at readAt (nil:
// none has worked) gives a reader the source shares visible of its items
// items with, as of now. A read older than MaxAudienceAge is unknown.
//
//	at := time.Now()
//	access.ReachOf(&at, []string{"C1"}, 2, at).Sees("C1") // true
func ReachOf(readAt *time.Time, visible []string, items int, now time.Time) SourceReach {
	if readAt == nil || now.Sub(*readAt) > MaxAudienceAge {
		return SourceReach{}
	}
	r := SourceReach{kind: reachSome, items: make(map[string]bool, len(visible))}
	for _, key := range visible {
		r.items[key] = true
	}
	if len(r.items) >= items {
		r.kind = reachAll
	}
	return r
}

// Sees reports whether the reader reads item key.
func (r SourceReach) Sees(key string) bool { return r.items[key] }

// Known reports whether the connector's items are known: a read of them
// worked within MaxAudienceAge.
func (r SourceReach) Known() bool { return r.kind != reachUnknown }

// All reports whether the reader reads every item the last read listed.
func (r SourceReach) All() bool { return r.kind == reachAll }

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

// whole reports whether the reader reads all of connectorID that grants give
// it: every item of an audienced one, anything of another.
func (s Sharing) whole(connectorID string) bool {
	r, ok := s.reach[connectorID]
	return !ok || r.All()
}

// some reports whether the reader reads anything of connectorID that grants
// give it: an item, for an audienced one.
func (s Sharing) some(connectorID string) bool {
	r, ok := s.reach[connectorID]
	return !ok || r.Known() && len(r.items) > 0
}

// reads reports whether the reader reads d as far as its source goes: any
// document of a set no audienced connector owns, else one whose item the
// source shares with the reader. A document of no item the kind knows, or of
// a connector whose items are unknown, it doesn't.
func (s Sharing) reads(d DocumentRef) bool {
	r, ok := s.reach[d.Set.ConnectorID]
	if !ok || d.Set.ConnectorID == "" {
		return true
	}
	if !r.Known() || s.itemOf == nil {
		return false
	}
	item, ok := s.itemOf(d.Set.ConnectorID, d.Stream, d.ExternalID)
	return ok && r.Sees(item)
}
