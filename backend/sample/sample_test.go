package sample

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// ids are a connector's source ids for the sample's tables.
func ids() map[string]string {
	sources := map[string]string{}
	for _, t := range Tables {
		sources[t] = ontologyagent.TableSourceID("conn-acme", t)
	}
	return sources
}

func TestDraft(t *testing.T) {
	sources := ids()
	raw, err := Draft(Acme, sources, "01JSET00000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "{{") {
		t.Fatal("a placeholder is left")
	}
	var def ontologyagent.Definition
	if err := json.Unmarshal(raw, &def); err != nil {
		t.Fatal(err)
	}
	if def.Format != string(model.OntologyFormatAgentBundle) {
		t.Fatalf("format %q", def.Format)
	}
	// It loads as an AI draft does, once its generation stores it.
	gen := "gen"
	refusal, err := ontologyagent.LoadRefusal(&model.OntologyVersion{Format: model.OntologyFormatAgentBundle, Definition: raw, GenerationID: &gen})
	if err != nil || refusal != "" {
		t.Fatalf("refused: %q %v", refusal, err)
	}
	// Each table is mapped by its source id.
	for _, t2 := range Tables {
		if !strings.Contains(def.MappingsTOML, fmt.Sprintf("source = %q", sources[t2])) {
			t.Errorf("%s is not mapped by its id", t2)
		}
	}
	routes, err := ontologyagent.ParseDocumentRoutes(def.MappingsTOML)
	if err != nil {
		t.Fatal(err)
	}
	// Its documents, in the sample's set, are read.
	if !routes.Routes("01JSET00000000000000000000") {
		t.Error("the sample's set is routed nowhere")
	}
	// Drafted for nothing: it records no spend.
	if def.Usage != (ontologyagent.Usage{}) {
		t.Errorf("usage %+v", def.Usage)
	}
}

func TestDraftRefuses(t *testing.T) {
	if _, err := Draft("globex", ids(), "set"); err == nil {
		t.Error("another sample was drafted")
	}
	sources := ids()
	delete(sources, "invoices")
	if _, err := Draft(Acme, sources, "set"); err == nil || !strings.Contains(err.Error(), "invoices") {
		t.Errorf("a missing table: %v", err)
	}
}

// The draft names exactly the sample's tables.
func TestTablesAreTheDraftsTables(t *testing.T) {
	named := []string{}
	for _, m := range regexp.MustCompile(`\{\{table:([^{}]+)\}\}`).FindAllStringSubmatch(acme, -1) {
		if !slices.Contains(named, m[1]) {
			named = append(named, m[1])
		}
	}
	slices.Sort(named)
	want := slices.Sorted(slices.Values(Tables))
	if !slices.Equal(named, want) {
		t.Errorf("the draft names %v, Tables are %v", named, want)
	}
}

func ExampleDraft() {
	raw, _ := Draft(Acme, map[string]string{
		"accounts": "src-accounts", "contacts": "src-contacts", "subscriptions": "src-subscriptions",
		"invoices": "src-invoices", "support_tickets": "src-tickets", "product_usage": "src-usage",
	}, "set-acme")
	var def struct {
		MappingsTOML string `json:"mappings_toml"`
	}
	_ = json.Unmarshal(raw, &def)
	fmt.Println(strings.Split(def.MappingsTOML, "\n")[4])
	// Output: source = "src-accounts"
}
