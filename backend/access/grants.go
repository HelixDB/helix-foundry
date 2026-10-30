package access

import "fmt"

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
// People enrichment's profiles, and whether it holds anything no grant can
// cover (a review decision whose documents were never recorded).
type Inputs struct {
	Tables      []TableRef
	Sets        []SetRef
	Enrichment  bool
	Uncoverable bool
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

func (g Grants) coversTable(t TableRef) bool { return g.connectors[t.ConnectorID] || g.tables[t] }

// CoversSet reports whether the grants cover a document in set s.
func (g Grants) CoversSet(s SetRef) bool {
	return s.SetID != "" && (g.sets[s.SetID] || (s.ConnectorID != "" && g.connectors[s.ConnectorID]))
}

// Missing is what of in the grants don't cover: the tables, sets and
// People enrichment, and whether in holds what nothing covers.
func (g Grants) Missing(in Inputs) Inputs {
	var out Inputs
	for _, t := range in.Tables {
		if !g.coversTable(t) {
			out.Tables = append(out.Tables, t)
		}
	}
	for _, s := range in.Sets {
		if !g.CoversSet(s) {
			out.Sets = append(out.Sets, s)
		}
	}
	out.Enrichment = in.Enrichment && !g.enrichment
	out.Uncoverable = in.Uncoverable
	return out
}

// Covers reports whether the grants cover everything in holds.
//
//	g := access.NewGrants([]access.Target{access.ConnectorTarget("stripe")})
//	g.Covers(access.Inputs{Tables: []access.TableRef{{ConnectorID: "stripe", Table: "customers"}}}) // true
func (g Grants) Covers(in Inputs) bool {
	m := g.Missing(in)
	return len(m.Tables)+len(m.Sets) == 0 && !m.Enrichment && !m.Uncoverable
}
