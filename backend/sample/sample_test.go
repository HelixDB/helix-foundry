package sample

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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

// The sample an agent builds through the MCP is the website's: the same documents and CA bundle
// the site serves, and the names its sample tour gives what it makes.
func TestSampleMatchesTheWebsite(t *testing.T) {
	served, err := os.ReadDir("../../public/samples/acme")
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := fs.ReadDir(Documents, "acme")
	if err != nil {
		t.Fatal(err)
	}
	if len(embedded) != len(served) {
		t.Fatalf("%d documents embedded, %d served", len(embedded), len(served))
	}
	tour, err := os.ReadFile("../../src/web/areas/ontology/sample.ts")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range served {
		want, err := os.ReadFile(filepath.Join("../../public/samples/acme", file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		got, err := fs.ReadFile(Documents, "acme/"+file.Name())
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s differs from the one the website serves (%v)", file.Name(), err)
		}
		if !strings.Contains(string(tour), strconv.Quote(file.Name())) {
			t.Errorf("the sample tour doesn't upload %s", file.Name())
		}
	}
	ca, err := os.ReadFile("../../public/samples/rds-ca.pem")
	if err != nil || string(ca) != RDSCA {
		t.Errorf("RDSCA differs from the website's rds-ca.pem (%v)", err)
	}
	for _, s := range []string{OntologyName, ConnectorName, SetName, SetDescription, Goal, DatabaseName, DatabaseUser, DatabasePassword} {
		if !strings.Contains(string(tour), strconv.Quote(s)) {
			t.Errorf("the sample tour has no %q", s)
		}
	}
	if !strings.Contains(string(tour), fmt.Sprintf("port: %d,", DatabasePort)) {
		t.Errorf("the sample tour's port isn't %d", DatabasePort)
	}
}
