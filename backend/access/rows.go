package access

import (
	"maps"
	"slices"
)

// MaxHeldItems is the most items of one source a reader may hold the rows
// of: past it, every table its rows are told apart in goes to full readers
// only (plans/SOURCE_ACLS.md). The engine (contract 4.32.0) repeats every
// held item, once per mapping whose row key names the field (about 290
// bytes each), in each partly held entity's read of who states it, and one
// entity's read must fit Helix's 2 MiB request: a reader in 1,000 channels
// of a version with Slack's four row tables sends about 1.2 MB a read, and
// up to about seven such mappings fit. Past that every one of their partial
// reads would fail, so the cap is the engine's budget, not the API's; it
// rises once the engine sends a source's held items as one set.
const MaxHeldItems = 1_000

// RowMatch is how a table of an audienced connector tells which of its rows
// a reader holds.
type RowMatch uint8

const (
	// RowsUnheld is a table whose rows aren't told apart by item: only full
	// readers read it.
	RowsUnheld RowMatch = iota
	// RowsByItem holds the rows whose row-key field is an item the source
	// shares with the reader (a Slack channel's row, by channel_id).
	RowsByItem
	// RowsByItemPrefix holds the rows whose row-key field starts with such
	// an item and then the separator (a Slack message's "<channel>:<ts>").
	RowsByItemPrefix
	// RowsDirectory is the source's directory of its people (Slack's users):
	// held whole by a reader the source has an account for that isn't a
	// guest's, and by no one else, whatever items it shares.
	RowsDirectory
)

// RowRule is how one table of an audienced connector tells which of its rows
// a reader holds. Make one with ItemRows, ItemPrefixRows or DirectoryRows;
// the zero RowRule is RowsUnheld.
type RowRule struct {
	match            RowMatch
	field, separator string
}

// ItemRows holds the rows whose row-key field is one of the reader's items.
//
//	access.ItemRows("channel_id") // a channel's own rows
func ItemRows(field string) RowRule { return RowRule{match: RowsByItem, field: field} }

// ItemPrefixRows holds the rows whose row-key field starts with one of the
// reader's items and then separator, which must not be empty: an empty one
// would hold the rows of every item that starts with the reader's.
//
//	access.ItemPrefixRows("message_id", ":") // "C1:1700000000.000100" is C1's
func ItemPrefixRows(field, separator string) RowRule {
	if separator == "" {
		panic("access: an item's prefix ends with a separator")
	}
	return RowRule{match: RowsByItemPrefix, field: field, separator: separator}
}

// DirectoryRows is the source's directory of people.
func DirectoryRows() RowRule { return RowRule{match: RowsDirectory} }

// Match is how the rule holds rows.
func (r RowRule) Match() RowMatch { return r.match }

// TableRows are the rows of one table of an audienced connector a reader
// holds: those of Items, the items its source shares with the reader, told
// by Rule (RowsByItem or RowsByItemPrefix).
type TableRows struct {
	Table TableRef
	Rule  RowRule
	// Items are sorted and unique, 1 to MaxHeldItems of them.
	Items []string
}

// Held is what the rows are held by, as the agent is told it: a row is held
// when its row key's field is one of texts, or, with prefix, starts with one
// of them (each item and the rule's separator).
//
//	rows := access.TableRows{Rule: access.ItemPrefixRows("message_id", ":"), Items: []string{"C1"}}
//	rows.Held() // "message_id", true, ["C1:"]
func (r TableRows) Held() (field string, prefix bool, texts []string) {
	texts = make([]string, len(r.Items))
	for i, item := range r.Items {
		texts[i] = item + r.Rule.separator
	}
	return r.Rule.field, r.Rule.match == RowsByItemPrefix, texts
}

// holds reports whether r holds every row before held: the same table, by
// the same rule, of every item it held. Items are sorted, so each is found by
// binary search: a retry compares up to MaxHeldItems of them a table.
func (r TableRows) holds(before TableRows) bool {
	return r.Table == before.Table && r.Rule == before.Rule &&
		!slices.ContainsFunc(before.Items, func(i string) bool {
			_, found := slices.BinarySearch(r.Items, i)
			return !found
		})
}

// tableHold is what a reader holds of one table: all of it, some of its rows,
// or nothing.
type tableHold uint8

const (
	holdsNone tableHold = iota
	holdsWhole
	holdsRows
)

// table is what the grants hold of t, and its rows when they hold some: a
// granted table of a connector no source narrows whole, else as its kind
// tells its rows (RowRule) by what the source shares with the reader now. A
// connector whose items are unknown, a kind that doesn't tell a table's rows
// apart, no item shared or more than MaxHeldItems of them hold nothing.
func (g Grants) table(t TableRef) (tableHold, TableRows) {
	switch {
	case !g.workspace && !g.connectors[t.ConnectorID] && !g.tables[t]:
		return holdsNone, TableRows{}
	case !g.sharing.audienced(t.ConnectorID):
		return holdsWhole, TableRows{}
	}
	kind, ok := g.sharing.kinds[t.ConnectorID]
	reach := g.sharing.reach[t.ConnectorID]
	if !ok || !reach.Known() {
		return holdsNone, TableRows{}
	}
	switch rule := kind.TableRows(t.Table); rule.match {
	case RowsDirectory:
		if reach.member {
			return holdsWhole, TableRows{}
		}
	case RowsByItem, RowsByItemPrefix:
		if n := len(reach.items); n > 0 && n <= MaxHeldItems {
			return holdsRows, TableRows{Table: t, Rule: rule, Items: slices.Sorted(maps.Keys(reach.items))}
		}
	}
	return holdsNone, TableRows{}
}

// ReadsTable reports whether the grants give a reader anything of table t: all
// of it (CoversTable, or an audienced source's directory it is one of), or
// some of its rows, by the items its source shares with it.
//
//	at := time.Now()
//	g := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
//		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&at, nil, access.Shared{Items: []string{"C1"}}, at)}, kinds))
//	g.ReadsTable(access.TableRef{ConnectorID: "slack", Table: "messages"}) // true: #general's (C1)
func (g Grants) ReadsTable(t TableRef) bool {
	hold, _ := g.table(t)
	return hold != holdsNone
}
