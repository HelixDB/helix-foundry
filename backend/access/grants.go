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

// Grants are the targets a grantee holds, through itself and its teams.
type Grants struct {
	connectors map[string]bool
	tables     map[TableRef]bool
	sets       map[string]bool
	enrichment bool
}

// NewGrants indexes targets.
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

// Empty reports grants that give nothing.
func (g Grants) Empty() bool {
	return len(g.connectors)+len(g.tables)+len(g.sets) == 0 && !g.enrichment
}

// CoversTable reports whether the grants cover a table.
func (g Grants) CoversTable(t TableRef) bool { return g.connectors[t.ConnectorID] || g.tables[t] }

// CoversSet reports whether the grants cover a document in set s.
func (g Grants) CoversSet(s SetRef) bool {
	return s.SetID != "" && (g.sets[s.SetID] || (s.ConnectorID != "" && g.connectors[s.ConnectorID]))
}

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

// Reaches reports whether the grants hold anything of in, so a grantee may
// read the part of its load they hold. What holds what can't be told isn't
// read at all.
//
//	g := access.NewGrants([]access.Target{access.ConnectorTarget("stripe")})
//	g.Reaches(access.Inputs{Tables: []access.TableRef{{ConnectorID: "stripe", Table: "customers"}, {ConnectorID: "crm", Table: "accounts"}}}) // true
func (g Grants) Reaches(in Inputs) bool {
	if in.Unknown {
		return false
	}
	return in.Enrichment && g.enrichment || slices.ContainsFunc(in.Tables, g.CoversTable) || slices.ContainsFunc(in.Sets, g.CoversSet)
}

// DocumentRef is a document or table file of a load, and the set it is in now.
type DocumentRef struct {
	ID  string
	Set SetRef
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
	Tables       []TableRef
	Files        []string
	AllDocuments bool
	Documents    []string
	Sets         []SetRef
	// DecisionSets are the sets of the documents the decisions it reads came
	// from: its answers carry those values, so grants must keep covering them.
	DecisionSets []SetRef
	Enrichment   bool
	// Withheld are the properties decided from documents it doesn't hold.
	Withheld []PropRef
}

// Part is what the grants hold of h.
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
		if g.CoversTable(t) {
			out.Tables = append(out.Tables, t)
		}
	}
	for _, f := range h.Files {
		if g.CoversSet(f.Set) {
			out.Files = append(out.Files, f.ID)
			addSet(f.Set)
		}
	}
	for _, d := range h.Documents {
		if g.CoversSet(d.Set) {
			out.Documents = append(out.Documents, d.ID)
			addSet(d.Set)
		} else {
			out.AllDocuments = false
		}
	}
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
	withheld := !slices.ContainsFunc(p.Withheld, func(w PropRef) bool { return !slices.Contains(before.Withheld, w) })
	return documents && tables && withheld && subset(p.Files, before.Files) && (p.Enrichment || !before.Enrichment)
}

// Inputs is what p reads, for telling later whether grants still hold it.
func (p Part) Inputs() Inputs {
	return Inputs{Tables: p.Tables, Sets: p.Sets, DecisionSets: p.DecisionSets, Enrichment: p.Enrichment}
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
