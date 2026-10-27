package ontologyagent

import (
	"fmt"
	"slices"
	"testing"
)

func ExampleParseDocumentRoutes() {
	routes, _ := ParseDocumentRoutes(`
[mappings.customers]
source = "src_1"

[extractions.contracts]
documents = ["01JDXSETCONTRACTS000000000"]
links = ["party_to"]

[extractions.contracts.types.Contract]
create = true
props = ["amount"]
`)
	fmt.Println(routes.Routes("01JDXSETCONTRACTS000000000"), routes.Routes("01JDXSETOTHER0000000000000"), routes.Sets())
	// Output: true false [01JDXSETCONTRACTS000000000]
}

func TestParseDocumentRoutes(t *testing.T) {
	// Sets is what a revision adds to its scope: the sets named, never "all".
	for name, tc := range map[string]struct {
		mappings string
		routed   map[string]bool
		sets     []string
	}{
		"no extractions":     {`[mappings.customers]` + "\n" + `source = "src_1"`, map[string]bool{"a": false}, []string{}},
		"empty mappings":     {``, map[string]bool{"a": false}, []string{}},
		"all":                {"[extractions.everything]\ndocuments = \"all\"", map[string]bool{"a": true, "b": true}, []string{}},
		"all beside a list":  {"[extractions.x]\ndocuments = [\"a\"]\n[extractions.y]\ndocuments = \"all\"", map[string]bool{"a": true, "b": true}, []string{"a"}},
		"union of two lists": {"[extractions.x]\ndocuments = [\"a\"]\n[extractions.y]\ndocuments = [\"b\", \"a\"]", map[string]bool{"a": true, "b": true, "c": false}, []string{"a", "b"}},
		"an empty list":      {"[extractions.x]\ndocuments = []", map[string]bool{"a": false}, []string{}},
	} {
		routes, err := ParseDocumentRoutes(tc.mappings)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for set, want := range tc.routed {
			if got := routes.Routes(set); got != want {
				t.Errorf("%s: Routes(%q) = %t, want %t", name, set, got, want)
			}
		}
		if got := routes.Sets(); !slices.Equal(got, tc.sets) {
			t.Errorf("%s: Sets() = %v, want %v", name, got, tc.sets)
		}
	}
	for name, mappings := range map[string]string{
		"not TOML":          `[extractions.x`,
		"another word":      "[extractions.x]\ndocuments = \"some\"",
		"a number in sets":  "[extractions.x]\ndocuments = [\"a\", 3]",
		"a table":           "[extractions.x.documents]\nset = \"a\"",
		"no documents":      "[extractions.x]\nlinks = []",
		"extractions value": `extractions = "all"`,
		"a route value":     `extractions = { x = "all" }`,
	} {
		if _, err := ParseDocumentRoutes(mappings); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestMappedSources(t *testing.T) {
	// A load reads the sources its mappings name: a nested mapping reads its
	// parent's, so it names none; each source once, sorted.
	t.Run("lists the sources mappings read", func(t *testing.T) {
		got, err := MappedSources(`
[mappings.offices]
source = "doc_b"
records = "/offices"
[mappings.office_visits]
parent = "offices"
records = "/visits"
[mappings.customers]
source = "src_1"
[mappings.orders]
source = "doc_b"
records = "/orders"
[extractions.docs]
documents = "all"
`)
		if err != nil || !slices.Equal(got, []string{"doc_b", "src_1"}) {
			t.Fatalf("MappedSources = (%v, %v)", got, err)
		}
		none, err := MappedSources(`[extractions.docs]` + "\n" + `documents = "all"`)
		if err != nil || len(none) != 0 {
			t.Fatalf("routes alone = (%v, %v)", none, err)
		}
	})
	// Mappings it can't read are an error, never "reads nothing".
	t.Run("refuses mappings it can't read", func(t *testing.T) {
		for name, toml := range map[string]string{
			"not TOML":              "[mappings",
			"mappings not a table":  `mappings = 1`,
			"a mapping not a table": "[mappings]\norders = 1",
			"a source not a string": "[mappings.orders]\nsource = 1",
		} {
			if _, err := MappedSources(toml); err == nil {
				t.Errorf("%s: no error", name)
			}
		}
	})
}

func ExampleParseEnrichments() {
	item, _ := ParseEnrichments(`
[mappings.contacts]
source = "src_1"

[enrichments.people]
type = "Person"
clues = { email = "email", linkedin_url = "linkedin_url" }
props = { current_title = "current_title", country = "location_country", profile_url = "linkedin_url" }
work_history = { link = "worked_at", match = "domain", props = { title = "title" } }
`)
	fmt.Println(item.Name, item.Type, item.Clues.Email, item.Fields(), item.WorkHistory.Link)
	// Output: people Person email [current_title linkedin_url location_country] worked_at
}

func TestParseEnrichments(t *testing.T) {
	// A version without the item loads as before: no step, no profiles.
	t.Run("reads no item as none", func(t *testing.T) {
		for _, mappings := range []string{``, "[mappings.contacts]\nsource = \"src_1\""} {
			item, err := ParseEnrichments(mappings)
			if err != nil || item != nil {
				t.Fatalf("%q: %v %v", mappings, item, err)
			}
		}
	})

	// One clue is enough; work history is optional.
	t.Run("reads an item with one clue and no work history", func(t *testing.T) {
		item, err := ParseEnrichments("[enrichments.people]\ntype = \"Person\"\nclues = { linkedin_url = \"li\" }\nprops = { t = \"current_title\" }")
		if err != nil || item.Clues.LinkedInURL != "li" || item.Clues.Email != "" || item.WorkHistory != nil {
			t.Fatalf("item = %+v, %v", item, err)
		}
	})

	// The engine refuses these too; the backend never plans from a half item.
	for name, mappings := range map[string]string{
		"two items": "[enrichments.a]\ntype = \"P\"\nclues = { email = \"e\" }\n[enrichments.b]\ntype = \"P\"\nclues = { email = \"e\" }",
		"no type":   "[enrichments.a]\nclues = { email = \"e\" }",
		"no clue":   "[enrichments.a]\ntype = \"P\"",
		"not toml":  "[enrichments.a",
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			if _, err := ParseEnrichments(mappings); err == nil {
				t.Fatalf("%s accepted", name)
			}
		})
	}
}
