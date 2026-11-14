package ontologyagent

import (
	"slices"
	"testing"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// A start names the sources the current version's generation didn't read: a
// table by its connector and name, a table file by its document, a set by its
// id; never People enrichment's profiles, which no one chose.
func TestNewSources(t *testing.T) {
	customers, invoices := TableSourceID("conn-1", "customers"), TableSourceID("conn-1", "invoices")
	sources := []Source{
		{ID: customers}, {ID: invoices}, {ID: TableSourceID("conn-2", "customers")},
		{ID: "doc-old"}, {ID: "doc-new"},
		{Kind: SourceDocuments, ID: "set-old"}, {Kind: SourceDocuments, ID: "set-new"},
		EnrichmentSource("profiles.jsonl", "f"),
	}
	drafted := model.OntologyGenerationInput{
		Tables:         []model.OntologyGenerationTable{{ConnectorID: "conn-1", Table: "customers"}, {ConnectorID: "conn-1", Table: "invoices"}},
		Documents:      []model.OntologyGenerationDocument{{ID: "doc-old", SetID: "set-old"}},
		DocumentSetIDs: []string{"set-old"},
	}
	var got []string
	for _, s := range NewSources(sources, drafted) {
		got = append(got, s.ID)
	}
	if want := []string{TableSourceID("conn-2", "customers"), "doc-new", "set-new"}; !slices.Equal(got, want) {
		t.Fatalf("NewSources = %v, want %v", got, want)
	}
	if len(sources) != 8 {
		t.Fatalf("NewSources changed its argument: %d sources", len(sources))
	}
	if fresh := NewSources(sources[:3], model.OntologyGenerationInput{Tables: drafted.Tables, DocumentSetIDs: []string{"set-x"}}); len(fresh) != 1 {
		t.Fatalf("a table of another connector is new: %v", fresh)
	}
}
