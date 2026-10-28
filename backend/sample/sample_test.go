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

// ids are a workspace's document ids for the sample's table files.
func ids() map[string]string {
	files := map[string]string{}
	for i, t := range Tables {
		files[t] = fmt.Sprintf("01JDOC%020d", i)
	}
	return files
}

func TestDraft(t *testing.T) {
	files := ids()
	raw, err := Draft(Kestrel, files, "01JSET00000000000000000000")
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
	// Each table file is mapped by its document id.
	for _, t2 := range Tables {
		if !strings.Contains(def.MappingsTOML, fmt.Sprintf("source = %q", files[t2])) {
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
	if _, err := Draft("acme", ids(), "set"); err == nil {
		t.Error("another sample was drafted")
	}
	files := ids()
	delete(files, "invoices.csv")
	if _, err := Draft(Kestrel, files, "set"); err == nil || !strings.Contains(err.Error(), "invoices.csv") {
		t.Errorf("a missing table file: %v", err)
	}
}

// The draft names exactly the sample's table files.
func TestTablesAreTheDraftsFiles(t *testing.T) {
	named := []string{}
	for _, m := range regexp.MustCompile(`\{\{file:([^{}]+)\}\}`).FindAllStringSubmatch(kestrel, -1) {
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
	raw, _ := Draft(Kestrel, map[string]string{
		"accounts.csv": "doc-accounts", "contacts.csv": "doc-contacts", "subscriptions.csv": "doc-subscriptions",
		"invoices.csv": "doc-invoices", "support_tickets.csv": "doc-tickets", "product_usage.csv": "doc-usage",
	}, "set-kestrel")
	var def struct {
		MappingsTOML string `json:"mappings_toml"`
	}
	_ = json.Unmarshal(raw, &def)
	fmt.Println(strings.Split(def.MappingsTOML, "\n")[4])
	// Output: source = "doc-accounts"
}
