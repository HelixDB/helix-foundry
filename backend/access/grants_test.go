package access_test

import (
	"fmt"
	"testing"

	"github.com/helixdb/helix-foundry/backend/access"
)

func TestParseTarget(t *testing.T) {
	for _, tc := range []struct {
		kind, connector, table, set string
		want                        access.Target
	}{
		{"connector", "c", "", "", access.ConnectorTarget("c")},
		{"table", "c", "customers", "", access.TableTarget("c", "customers")},
		{"document_set", "", "", "s", access.DocumentSetTarget("s")},
		{"enrichment", "", "", "", access.EnrichmentTarget()},
	} {
		got, err := access.ParseTarget(tc.kind, tc.connector, tc.table, tc.set)
		if err != nil || got != tc.want {
			t.Fatalf("%s: %+v, %v", tc.kind, got, err)
		}
		if got.Kind().String() != tc.kind || got.ConnectorID() != tc.connector || got.Table() != tc.table || got.DocumentSetID() != tc.set {
			t.Fatalf("%s reads back as %v %q %q %q", tc.kind, got.Kind(), got.ConnectorID(), got.Table(), got.DocumentSetID())
		}
	}
	// Each kind names exactly what it needs.
	for _, bad := range [][4]string{
		{"", "", "", ""}, {"agent", "c", "", ""},
		{"connector", "", "", ""}, {"connector", "c", "t", ""}, {"connector", "c", "", "s"},
		{"table", "c", "", ""}, {"table", "", "t", ""}, {"table", "c", "t", "s"},
		{"document_set", "", "", ""}, {"document_set", "c", "", "s"}, {"document_set", "", "t", "s"},
		{"enrichment", "c", "", ""}, {"enrichment", "", "t", ""}, {"enrichment", "", "", "s"},
	} {
		if got, err := access.ParseTarget(bad[0], bad[1], bad[2], bad[3]); err == nil {
			t.Fatalf("%q parsed as %+v", bad, got)
		}
	}
}

func TestGrantsCover(t *testing.T) {
	stripe := access.TableRef{ConnectorID: "stripe", Table: "customers"}
	invoices := access.TableRef{ConnectorID: "stripe", Table: "invoices"}
	postgres := access.TableRef{ConnectorID: "pg", Table: "people"}
	contracts := access.SetRef{SetID: "contracts"}
	notes := access.SetRef{SetID: "notes", ConnectorID: "granola"}
	gone := access.SetRef{}
	for name, tc := range map[string]struct {
		grants []access.Target
		in     access.Inputs
		covers bool
		miss   access.Inputs
	}{
		"nothing held, nothing granted": {covers: true},
		"nothing granted":               {in: access.Inputs{Tables: []access.TableRef{stripe}}, miss: access.Inputs{Tables: []access.TableRef{stripe}}},
		"a connector covers its tables, now and later": {
			grants: []access.Target{access.ConnectorTarget("stripe")}, in: access.Inputs{Tables: []access.TableRef{stripe, invoices}}, covers: true},
		"a table covers itself only": {
			grants: []access.Target{access.TableTarget("stripe", "customers")}, in: access.Inputs{Tables: []access.TableRef{stripe, invoices}},
			miss: access.Inputs{Tables: []access.TableRef{invoices}}},
		"a connector covers the sets it owns": {
			grants: []access.Target{access.ConnectorTarget("granola")}, in: access.Inputs{Sets: []access.SetRef{notes}}, covers: true},
		"a connector covers no one else's set": {
			grants: []access.Target{access.ConnectorTarget("granola")}, in: access.Inputs{Sets: []access.SetRef{contracts}},
			miss: access.Inputs{Sets: []access.SetRef{contracts}}},
		"a set covers its documents": {
			grants: []access.Target{access.DocumentSetTarget("contracts"), access.DocumentSetTarget("notes")},
			in:     access.Inputs{Sets: []access.SetRef{contracts, notes}}, covers: true},
		"a document in no set is covered by nothing": {
			grants: []access.Target{access.DocumentSetTarget(""), access.ConnectorTarget("")}, in: access.Inputs{Sets: []access.SetRef{gone}},
			miss: access.Inputs{Sets: []access.SetRef{gone}}},
		"People enrichment needs its own grant": {
			grants: []access.Target{access.ConnectorTarget("pg")}, in: access.Inputs{Tables: []access.TableRef{postgres}, Enrichment: true},
			miss: access.Inputs{Enrichment: true}},
		"People enrichment granted": {
			grants: []access.Target{access.ConnectorTarget("pg"), access.EnrichmentTarget()},
			in:     access.Inputs{Tables: []access.TableRef{postgres}, Enrichment: true}, covers: true},
		"what nothing covers": {
			grants: []access.Target{access.ConnectorTarget("pg"), access.EnrichmentTarget()}, in: access.Inputs{Uncoverable: true},
			miss: access.Inputs{Uncoverable: true}},
	} {
		g := access.NewGrants(tc.grants)
		if got := g.Covers(tc.in); got != tc.covers {
			t.Errorf("%s: covers %v", name, got)
		}
		if got := g.Missing(tc.in); fmt.Sprint(got) != fmt.Sprint(tc.miss) {
			t.Errorf("%s: missing %+v, want %+v", name, got, tc.miss)
		}
	}
}

func TestGrantsEmpty(t *testing.T) {
	if !access.NewGrants(nil).Empty() {
		t.Fatal("no grants aren't empty")
	}
	for _, target := range []access.Target{access.ConnectorTarget("c"), access.TableTarget("c", "t"), access.DocumentSetTarget("s"), access.EnrichmentTarget()} {
		if access.NewGrants([]access.Target{target}).Empty() {
			t.Fatalf("%v is empty", target.Kind())
		}
	}
}

func ExampleParseTarget() {
	t, err := access.ParseTarget("table", "conn_1", "customers", "")
	fmt.Println(t.Kind(), t.ConnectorID(), t.Table(), err)
	// Output: table conn_1 customers <nil>
}

func ExampleGrants_Covers() {
	g := access.NewGrants([]access.Target{access.ConnectorTarget("stripe")})
	fmt.Println(g.Covers(access.Inputs{Tables: []access.TableRef{{ConnectorID: "stripe", Table: "customers"}}}))
	fmt.Println(g.Covers(access.Inputs{Tables: []access.TableRef{{ConnectorID: "stripe", Table: "customers"}}, Enrichment: true}))
	// Output:
	// true
	// false
}
