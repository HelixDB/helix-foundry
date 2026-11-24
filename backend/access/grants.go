package access

import (
	"fmt"
	"slices"
)

// TargetKind is what a grant gives.
type TargetKind uint8

const (
	// TargetConnector is a connector: every table it syncs, now and later,
	// and every document set it owns (Granola's notes, an API source's
	// pieces).
	TargetConnector TargetKind = iota + 1
	// TargetTable is one table of a connector.
	TargetTable
	// TargetDocumentSet is one document set.
	TargetDocumentSet
	// TargetEnrichment is the People enrichment profiles a load applies.
	TargetEnrichment
)

var targetKindNames = map[TargetKind]string{
	TargetConnector:   "connector",
	TargetTable:       "table",
	TargetDocumentSet: "document_set",
	TargetEnrichment:  "enrichment",
}

// String is the kind as the store keeps it.
func (k TargetKind) String() string { return targetKindNames[k] }

// Target is what one grant gives. Make one with ConnectorTarget,
// TableTarget, DocumentSetTarget, EnrichmentTarget or ParseTarget: each
// kind names exactly what it needs.
type Target struct {
	kind                      TargetKind
	connectorID, table, setID string
}

// ConnectorTarget grants connector connectorID.
func ConnectorTarget(connectorID string) Target {
	return Target{kind: TargetConnector, connectorID: connectorID}
}

// TableTarget grants one table of connector connectorID.
func TableTarget(connectorID, table string) Target {
	return Target{kind: TargetTable, connectorID: connectorID, table: table}
}

// DocumentSetTarget grants document set setID.
func DocumentSetTarget(setID string) Target {
	return Target{kind: TargetDocumentSet, setID: setID}
}

// EnrichmentTarget grants People enrichment's profiles.
func EnrichmentTarget() Target { return Target{kind: TargetEnrichment} }

// ParseTarget reads a target as the store and the API carry it: its kind
// and the ids that kind names, the others empty.
//
//	t, err := access.ParseTarget("table", "conn_1", "customers", "") // a table of conn_1
func ParseTarget(kind, connectorID, table, setID string) (Target, error) {
	var t Target
	var ok bool
	switch kind {
	case TargetConnector.String():
		t, ok = ConnectorTarget(connectorID), connectorID != "" && table == "" && setID == ""
	case TargetTable.String():
		t, ok = TableTarget(connectorID, table), connectorID != "" && table != "" && setID == ""
	case TargetDocumentSet.String():
		t, ok = DocumentSetTarget(setID), connectorID == "" && table == "" && setID != ""
	case TargetEnrichment.String():
		t, ok = EnrichmentTarget(), connectorID == "" && table == "" && setID == ""
	default:
		return Target{}, fmt.Errorf("access: unknown grant target kind %q", kind)
	}
	if !ok {
		return Target{}, fmt.Errorf("access: a %s grant can't name connector %q, table %q and set %q", kind, connectorID, table, setID)
	}
	return t, nil
}

func (t Target) Kind() TargetKind      { return t.kind }
func (t Target) ConnectorID() string   { return t.connectorID }
func (t Target) Table() string         { return t.table }
func (t Target) DocumentSetID() string { return t.setID }

// TableRef is a connector's table.
type TableRef struct{ ConnectorID, Table string }

// SetRef is the document set a document of a load is in now, and the
// connector that owns the set, if one does. A document whose set is gone
// has an empty SetID, which no grant covers.
type SetRef struct{ SetID, ConnectorID string }

// Inputs are what a load holds, as far as grants go: the tables it read,
// the sets its documents and table files are in now, whether it applied
// People enrichment's profiles, whether it holds anything no grant can cover
// (a review decision whose documents were never recorded), and whether what it
// holds can't be told at all (Unknown: a load made before grants could cover
// one, or with no generation to read its inputs from).
type Inputs struct {
	Tables []TableRef
	Sets   []SetRef
	// DecisionSets are the sets of the documents its prop decisions came from,
	// which must be covered for the load to be, but are nothing of it to read.
	DecisionSets []SetRef
	Enrichment   bool
	Uncoverable  bool
	Unknown      bool
}

// Grants are the targets a grantee holds, through itself and its teams, and
// what the sources of those targets share with it (WithSharing): a grant of
// a connector whose source reports its permissions gives only the items the
// source shares with the reader.
type Grants struct {
	connectors map[string]bool
	tables     map[TableRef]bool
	sets       map[string]bool
	enrichment bool
	sharing    Sharing
}

// NewGrants indexes targets, as the grants give them: no source narrows them
// until WithSharing.
func NewGrants(targets []Target) Grants {
	g := Grants{connectors: map[string]bool{}, tables: map[TableRef]bool{}, sets: map[string]bool{}}
	for _, t := range targets {
		switch t.kind {
		case TargetConnector:
			g.connectors[t.connectorID] = true
		case TargetTable:
			g.tables[TableRef{t.connectorID, t.table}] = true
		case TargetDocumentSet:
			g.sets[t.setID] = true
		case TargetEnrichment:
			g.enrichment = true
		}
	}
	return g
}

// WithSharing is g narrowed by what the sources of its connectors share with
// the reader (plans/SOURCE_ACLS.md): neither a table nor a set of an
// audienced connector is ever covered whole, so a load holding one is always
// read in part: a table's rows (ReadsTable) and a set's documents (Reads)
// only while the source shares their item.
//
//	at := time.Now()
//	g := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
//		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&at, nil, access.Shared{Items: []string{"C1", "C2"}}, at)}, kinds))
//	g.CoversTable(access.TableRef{ConnectorID: "slack", Table: "messages"}) // false, though it reads every channel listed
func (g Grants) WithSharing(s Sharing) Grants {
	g.sharing = s
	return g
}

// Empty reports grants that give nothing.
func (g Grants) Empty() bool {
	return len(g.connectors)+len(g.tables)+len(g.sets) == 0 && !g.enrichment
}

// Holds reports whether the grants give at least what every one of targets
// gives: a table held by its own grant or its connector's, a connector, a set
// and People enrichment only by their own. What a restricted member's
// Founder thread read under targets stays theirs only while they hold them.
// It compares grants as given, whatever their sources share: a thread
// records the items its reads were narrowed to apart (SeesItems).
//
//	g := access.NewGrants([]access.Target{access.ConnectorTarget("stripe")})
//	g.Holds([]access.Target{access.TableTarget("stripe", "customers")}) // true
func (g Grants) Holds(targets []Target) bool {
	return !slices.ContainsFunc(targets, func(t Target) bool {
		switch t.kind {
		case TargetConnector:
			return !g.connectors[t.connectorID]
		case TargetTable:
			return !g.connectors[t.connectorID] && !g.tables[TableRef{t.connectorID, t.table}]
		case TargetDocumentSet:
			return !g.sets[t.setID]
		case TargetEnrichment:
			return !g.enrichment
		}
		return true
	})
}

// CoversTable reports whether the grants cover a table: it is granted, by
// itself or its connector, and its connector's source doesn't report its
// permissions. A table of one that does is never covered whole: its rows may
// be of items the last read of the source's permissions didn't list, so a
// grantee reads only the rows of the items shared with it (ReadsTable).
func (g Grants) CoversTable(t TableRef) bool {
	return (g.connectors[t.ConnectorID] || g.tables[t]) && !g.sharing.audienced(t.ConnectorID)
}

// givesSet reports whether a grant gives set s, whatever its source shares.
func (g Grants) givesSet(s SetRef) bool {
	return s.SetID != "" && (g.sets[s.SetID] || (s.ConnectorID != "" && g.connectors[s.ConnectorID]))
}

// CoversSet reports whether the grants cover every document in set s: it is
// granted, and no connector whose source reports its permissions owns it. A
// set of one that does is never covered whole, so a load holding it is read
// in part, document by document (Reads): what the set holds may be of items
// the last read didn't list, which only full readers read.
func (g Grants) CoversSet(s SetRef) bool { return g.givesSet(s) && !g.sharing.audienced(s.ConnectorID) }

// ReachesSet reports whether the grants give some documents of set s: it is
// granted, and the source of an audienced connector that owns it shares an
// item with the reader, so a reader of part of a load is told of the set.
func (g Grants) ReachesSet(s SetRef) bool { return g.givesSet(s) && g.sharing.some(s.ConnectorID) }

// Reads reports whether the grants give document d of a load: its set is
// granted, and the source of an audienced connector that owns the set shares
// d's item with the reader.
//
//	g := access.NewGrants([]access.Target{access.DocumentSetTarget("contracts")})
//	g.Reads(access.DocumentRef{ID: "d1", Set: access.SetRef{SetID: "contracts"}}) // true
func (g Grants) Reads(d DocumentRef) bool { return g.givesSet(d.Set) && g.sharing.reads(d) }

// Missing is what of in the grants don't cover: the tables, sets and
// People enrichment, and whether in holds what nothing covers.
func (g Grants) Missing(in Inputs) Inputs {
	var out Inputs
	for _, t := range in.Tables {
		if !g.CoversTable(t) {
			out.Tables = append(out.Tables, t)
		}
	}
	for _, s := range in.Sets {
		if !g.CoversSet(s) {
			out.Sets = append(out.Sets, s)
		}
	}
	for _, s := range in.DecisionSets {
		if !g.CoversSet(s) {
			out.DecisionSets = append(out.DecisionSets, s)
		}
	}
	out.Enrichment = in.Enrichment && !g.enrichment
	out.Uncoverable = in.Uncoverable
	out.Unknown = in.Unknown
	return out
}

// Covers reports whether the grants cover everything in holds, and in holds
// something: a load holding nothing is no one's to read.
//
//	g := access.NewGrants([]access.Target{access.ConnectorTarget("stripe")})
//	g.Covers(access.Inputs{Tables: []access.TableRef{{ConnectorID: "stripe", Table: "customers"}}}) // true
func (g Grants) Covers(in Inputs) bool {
	m := g.Missing(in)
	holds := len(in.Tables)+len(in.Sets) > 0 || in.Enrichment
	return holds && len(m.Tables)+len(m.Sets)+len(m.DecisionSets) == 0 && !m.Enrichment && !m.Uncoverable && !m.Unknown
}

// Keeps reports whether a grantee that read seen, part of a load, and the
// items of audienced sources that part held documents and rows of
// (Part.Items) may still read what it said (a question's answer): as Covers,
// but a set of an audienced connector counts while it is granted and its
// source shares any item with the reader, a table of one while the grants
// still read some of it (ReadsTable), and every one of items must still be
// shared with them, so an answer goes with the channel it read.
//
//	at := time.Now()
//	g := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
//		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&at, nil, access.Shared{Items: []string{"C1"}}, at)}, kinds))
//	seen := access.Inputs{Sets: []access.SetRef{{SetID: "conversations", ConnectorID: "slack"}}}
//	g.Keeps(seen, []access.ItemRef{{ConnectorID: "slack", Item: "C1"}}) // true
//	g.Keeps(seen, []access.ItemRef{{ConnectorID: "slack", Item: "C2"}}) // false: C2 isn't shared with them now
func (g Grants) Keeps(seen Inputs, items []ItemRef) bool {
	held := len(seen.Tables)+len(seen.Sets) > 0 || seen.Enrichment
	sets := !slices.ContainsFunc(seen.Sets, func(s SetRef) bool { return !g.ReachesSet(s) }) &&
		!slices.ContainsFunc(seen.DecisionSets, func(s SetRef) bool { return !g.CoversSet(s) })
	return held && sets && !slices.ContainsFunc(seen.Tables, func(t TableRef) bool { return !g.ReadsTable(t) }) &&
		(g.enrichment || !seen.Enrichment) && !seen.Uncoverable && !seen.Unknown && g.SeesItems(items)
}

// SeesItems reports whether every one of items is still shared with the
// reader by its source: an item of a connector deleted since, or whose
// source's permissions are unknown now, isn't.
//
//	at := time.Now()
//	g := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
//		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&at, nil, access.Shared{Items: []string{"C1"}}, at)}, kinds))
//	g.SeesItems([]access.ItemRef{{ConnectorID: "slack", Item: "C1"}}) // true
func (g Grants) SeesItems(items []ItemRef) bool {
	return !slices.ContainsFunc(items, func(i ItemRef) bool { return !g.sharing.sees(i) })
}

// VisibleItems are the items every audienced source shares with the reader
// now, sorted; none for grants that give nothing. GetMyFoundryAccess tells a
// grantee them, which Founder's worker reads each call of a restricted
// member's thread under: the thread records only the items of what its calls
// read, and is theirs only while every one is still shared with them
// (SeesItems).
func (g Grants) VisibleItems() []ItemRef {
	if g.Empty() {
		return nil
	}
	return g.sharing.visible()
}

// Reaches reports whether the grants hold anything of in, so a grantee may
// read the part of its load they hold: a table they read (ReadsTable), a
// granted set (of an audienced connector, while its source shares an item
// with the reader) or People enrichment. What holds what can't be told isn't
// read at all.
//
//	g := access.NewGrants([]access.Target{access.ConnectorTarget("stripe")})
//	g.Reaches(access.Inputs{Tables: []access.TableRef{{ConnectorID: "stripe", Table: "customers"}, {ConnectorID: "crm", Table: "accounts"}}}) // true
func (g Grants) Reaches(in Inputs) bool {
	if in.Unknown {
		return false
	}
	return in.Enrichment && g.enrichment || slices.ContainsFunc(in.Tables, g.ReadsTable) || slices.ContainsFunc(in.Sets, g.ReachesSet)
}

// DocumentRef is a document or table file of a load, and the set it is in now.
// A connector's document has the stream of its set and its row's key
// (ExternalID), which say what item of its source it is of; an upload has
// neither.
type DocumentRef struct {
	ID                 string
	Set                SetRef
	Stream, ExternalID string
}

// PropDecision is a value someone decided for a property of a type: the sets
// of the documents it was decided from, which none are if they were never
// recorded.
type PropDecision struct {
	Type, Prop string
	Sets       []SetRef
	Recorded   bool
}

// Holdings are what a load holds, document by document: Inputs in the detail a
// read of part of it needs.
type Holdings struct {
	Tables     []TableRef
	Files      []DocumentRef
	Documents  []DocumentRef
	Enrichment bool
	Decisions  []PropDecision
}

// PropRef is a property of a type.
type PropRef struct{ Type, Prop string }

// Part is what a grantee holds of a load it doesn't hold all of. AllDocuments
// says the load holds documents and it holds every one of them.
type Part struct {
	// Tables are those it holds whole.
	Tables []TableRef
	// Rows are the tables of audienced connectors it holds some rows of, in
	// the load's order.
	Rows         []TableRows
	Files        []string
	AllDocuments bool
	Documents    []string
	Sets         []SetRef
	// Items are the items of audienced sources the documents, files and rows
	// it reads are of, and the directory (DirectoryItem) of each source whose
	// directory it reads, sorted: what it says can be of them, so it stays
	// the reader's only while they are (Keeps).
	Items []ItemRef
	// DecisionSets are the sets of the documents the decisions it reads came
	// from: its answers carry those values, so grants must keep covering them.
	DecisionSets []SetRef
	Enrichment   bool
	// Withheld are the properties decided from documents it doesn't hold.
	Withheld []PropRef
}

// Part is what the grants hold of h: the tables they cover, or hold some
// rows of (ReadsTable), and the documents and table files they read (Reads),
// each by the item its source shares with the reader when its set's or
// table's connector reports its permissions.
//
//	g := access.NewGrants([]access.Target{access.DocumentSetTarget("contracts")})
//	g.Part(access.Holdings{Documents: []access.DocumentRef{{ID: "d1", Set: access.SetRef{SetID: "contracts"}}, {ID: "d2", Set: access.SetRef{SetID: "hr"}}}}).Documents // [d1]
func (g Grants) Part(h Holdings) Part {
	out := Part{Enrichment: h.Enrichment && g.enrichment, AllDocuments: len(h.Documents) > 0}
	addSet := func(s SetRef) {
		if !slices.Contains(out.Sets, s) {
			out.Sets = append(out.Sets, s)
		}
	}
	for _, t := range h.Tables {
		switch hold, rows := g.table(t); hold {
		case holdsWhole:
			out.Tables = append(out.Tables, t)
			// An audienced source's table held whole is its directory.
			if g.sharing.audienced(t.ConnectorID) {
				out.Items = append(out.Items, ItemRef{ConnectorID: t.ConnectorID, Item: DirectoryItem})
			}
		case holdsRows:
			out.Rows = append(out.Rows, rows)
			for _, item := range rows.Items {
				out.Items = append(out.Items, ItemRef{ConnectorID: t.ConnectorID, Item: item})
			}
		}
	}
	addItem := func(d DocumentRef) {
		if i, ok := g.sharing.item(d); ok {
			out.Items = append(out.Items, i)
		}
	}
	for _, f := range h.Files {
		if g.Reads(f) {
			out.Files = append(out.Files, f.ID)
			addSet(f.Set)
			addItem(f)
		}
	}
	for _, d := range h.Documents {
		if g.Reads(d) {
			out.Documents = append(out.Documents, d.ID)
			addSet(d.Set)
			addItem(d)
		} else {
			out.AllDocuments = false
		}
	}
	out.Items = SortItems(out.Items)
	for _, d := range h.Decisions {
		if d.Recorded && g.Covers(Inputs{Sets: d.Sets}) {
			for _, s := range d.Sets {
				if !slices.Contains(out.DecisionSets, s) {
					out.DecisionSets = append(out.DecisionSets, s)
				}
			}
			continue
		}
		prop := PropRef{Type: d.Type, Prop: d.Prop}
		if !slices.Contains(out.Withheld, prop) {
			out.Withheld = append(out.Withheld, prop)
		}
	}
	return out
}

// Holds reports whether p holds all of before: nothing it could read then is
// out of its reach now.
//
//	(access.Part{Documents: []string{"d1", "d2"}}).Holds(access.Part{Documents: []string{"d1"}}) // true
func (p Part) Holds(before Part) bool {
	subset := func(have, had []string) bool {
		return !slices.ContainsFunc(had, func(x string) bool { return !slices.Contains(have, x) })
	}
	documents := p.AllDocuments || !before.AllDocuments && subset(p.Documents, before.Documents)
	tables := !slices.ContainsFunc(before.Tables, func(t TableRef) bool { return !slices.Contains(p.Tables, t) })
	rows := !slices.ContainsFunc(before.Rows, func(b TableRows) bool {
		return !slices.ContainsFunc(p.Rows, func(r TableRows) bool { return r.holds(b) })
	})
	withheld := !slices.ContainsFunc(p.Withheld, func(w PropRef) bool { return !slices.Contains(before.Withheld, w) })
	return documents && tables && rows && withheld && subset(p.Files, before.Files) && (p.Enrichment || !before.Enrichment)
}

// Inputs is what p reads, for telling later whether grants still hold it:
// its tables, whole or by their rows (whose items are p.Items).
func (p Part) Inputs() Inputs {
	tables := slices.Clone(p.Tables)
	for _, r := range p.Rows {
		tables = append(tables, r.Table)
	}
	return Inputs{Tables: tables, Sets: p.Sets, DecisionSets: p.DecisionSets, Enrichment: p.Enrichment}
}

// Union is what in and other hold between them.
func (in Inputs) Union(other Inputs) Inputs {
	out := in
	out.Tables = slices.Clone(in.Tables)
	out.Sets = slices.Clone(in.Sets)
	for _, t := range other.Tables {
		if !slices.Contains(out.Tables, t) {
			out.Tables = append(out.Tables, t)
		}
	}
	for _, s := range other.Sets {
		if !slices.Contains(out.Sets, s) {
			out.Sets = append(out.Sets, s)
		}
	}
	out.DecisionSets = slices.Clone(in.DecisionSets)
	for _, s := range other.DecisionSets {
		if !slices.Contains(out.DecisionSets, s) {
			out.DecisionSets = append(out.DecisionSets, s)
		}
	}
	out.Enrichment = in.Enrichment || other.Enrichment
	out.Uncoverable = in.Uncoverable || other.Uncoverable
	out.Unknown = in.Unknown || other.Unknown
	return out
}
