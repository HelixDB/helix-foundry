package access_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

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
		{"workspace", "", "", "", access.WorkspaceTarget()},
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
		{"workspace", "c", "", ""}, {"workspace", "", "t", ""}, {"workspace", "", "", "s"}, {"workspace", "c", "t", "s"},
		{"Workspace", "", "", ""}, {"everything", "", "", ""},
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
		"nothing held, nothing granted": {covers: false},
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
		"the workspace covers every table, uploaded set and connector's set": {
			grants: []access.Target{access.WorkspaceTarget()},
			in:     access.Inputs{Tables: []access.TableRef{stripe, invoices, postgres}, Sets: []access.SetRef{contracts, notes}, DecisionSets: []access.SetRef{contracts}},
			covers: true},
		// It names nothing, so what the workspace adds after the grant is covered too.
		"the workspace covers a connector and a set added later": {
			grants: []access.Target{access.WorkspaceTarget()},
			in:     access.Inputs{Tables: []access.TableRef{{ConnectorID: "new", Table: "t"}}, Sets: []access.SetRef{{SetID: "new-set"}, {SetID: "new-notes", ConnectorID: "new"}}},
			covers: true},
		"the workspace never gives People enrichment": {
			grants: []access.Target{access.WorkspaceTarget()}, in: access.Inputs{Tables: []access.TableRef{postgres}, Enrichment: true},
			miss: access.Inputs{Enrichment: true}},
		"the workspace and People enrichment cover a load that applied profiles": {
			grants: []access.Target{access.WorkspaceTarget(), access.EnrichmentTarget()},
			in:     access.Inputs{Tables: []access.TableRef{postgres}, Sets: []access.SetRef{notes}, Enrichment: true}, covers: true},
		"the workspace with other grants covers as much": {
			grants: []access.Target{access.TableTarget("stripe", "customers"), access.WorkspaceTarget(), access.DocumentSetTarget("contracts")},
			in:     access.Inputs{Tables: []access.TableRef{stripe, invoices}, Sets: []access.SetRef{contracts, notes}}, covers: true},
		"the workspace covers no document in no set": {
			grants: []access.Target{access.WorkspaceTarget()}, in: access.Inputs{Tables: []access.TableRef{stripe}, Sets: []access.SetRef{gone}, DecisionSets: []access.SetRef{gone}},
			miss: access.Inputs{Sets: []access.SetRef{gone}, DecisionSets: []access.SetRef{gone}}},
		"the workspace covers nothing that can't be covered or told": {
			grants: []access.Target{access.WorkspaceTarget(), access.EnrichmentTarget()}, in: access.Inputs{Tables: []access.TableRef{stripe}, Uncoverable: true, Unknown: true},
			miss: access.Inputs{Uncoverable: true, Unknown: true}},
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
	for _, target := range []access.Target{access.ConnectorTarget("c"), access.TableTarget("c", "t"), access.DocumentSetTarget("s"), access.EnrichmentTarget(),
		access.WorkspaceTarget()} {
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

func TestGrantsReach(t *testing.T) {
	stripe := access.TableRef{ConnectorID: "stripe", Table: "customers"}
	crm := access.TableRef{ConnectorID: "crm", Table: "accounts"}
	general := access.SetRef{SetID: "general"}
	g := access.NewGrants([]access.Target{access.ConnectorTarget("stripe"), access.DocumentSetTarget("general"), access.EnrichmentTarget()})
	for name, tc := range map[string]struct {
		in   access.Inputs
		want bool
	}{
		"a table it holds":          {access.Inputs{Tables: []access.TableRef{crm, stripe}}, true},
		"a set it holds":            {access.Inputs{Sets: []access.SetRef{{SetID: "hr"}, general}}, true},
		"profiles":                  {access.Inputs{Enrichment: true}, true},
		"a decision it can't cover": {access.Inputs{Tables: []access.TableRef{stripe}, Uncoverable: true}, true},
		"nothing it holds":          {access.Inputs{Tables: []access.TableRef{crm}, Sets: []access.SetRef{{SetID: "hr"}}}, false},
		"what can't be told":        {access.Inputs{Tables: []access.TableRef{stripe}, Unknown: true}, false},
	} {
		if got := g.Reaches(tc.in); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
	if access.NewGrants(nil).Reaches(access.Inputs{Enrichment: true}) {
		t.Error("no grants reach profiles")
	}
	if g.Covers(access.Inputs{Tables: []access.TableRef{stripe}, Unknown: true}) {
		t.Error("what can't be told isn't covered")
	}
	workspace := access.NewGrants([]access.Target{access.WorkspaceTarget()})
	for name, tc := range map[string]struct {
		in   access.Inputs
		want bool
	}{
		"any table":                   {access.Inputs{Tables: []access.TableRef{crm}}, true},
		"any set":                     {access.Inputs{Sets: []access.SetRef{{SetID: "hr"}}}, true},
		"profiles alone":              {access.Inputs{Enrichment: true}, false},
		"profiles and a table":        {access.Inputs{Tables: []access.TableRef{crm}, Enrichment: true}, true},
		"only a document in no set":   {access.Inputs{Sets: []access.SetRef{{}}}, false},
		"what can't be told, even so": {access.Inputs{Tables: []access.TableRef{crm}, Unknown: true}, false},
	} {
		assert.Equal(t, tc.want, workspace.Reaches(tc.in), "workspace: %s", name)
	}
}

func TestGrantsPart(t *testing.T) {
	stripe := access.TableRef{ConnectorID: "stripe", Table: "customers"}
	crm := access.TableRef{ConnectorID: "crm", Table: "accounts"}
	general, hr := access.SetRef{SetID: "general"}, access.SetRef{SetID: "hr"}
	holdings := access.Holdings{
		Tables:     []access.TableRef{stripe, crm},
		Files:      []access.DocumentRef{{ID: "f1", Set: general}, {ID: "f2", Set: hr}},
		Documents:  []access.DocumentRef{{ID: "d1", Set: general}, {ID: "d2", Set: hr}, {ID: "d3", Set: general}},
		Enrichment: true,
		Decisions: []access.PropDecision{
			{Type: "A", Prop: "kept", Sets: []access.SetRef{general}, Recorded: true},
			{Type: "A", Prop: "other", Sets: []access.SetRef{hr}, Recorded: true},
			{Type: "A", Prop: "other", Sets: []access.SetRef{hr}, Recorded: true},
			{Type: "B", Prop: "unrecorded"},
		},
	}
	part := access.NewGrants([]access.Target{access.ConnectorTarget("stripe"), access.DocumentSetTarget("general")}).Part(holdings)
	want := access.Part{
		Tables: []access.TableRef{stripe}, Files: []string{"f1"}, Documents: []string{"d1", "d3"}, Sets: []access.SetRef{general}, DecisionSets: []access.SetRef{general},
		Withheld: []access.PropRef{{Type: "A", Prop: "other"}, {Type: "B", Prop: "unrecorded"}},
	}
	if fmt.Sprintf("%+v", part) != fmt.Sprintf("%+v", want) {
		t.Fatalf("%+v, want %+v", part, want)
	}
	if got := part.Inputs(); fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", access.Inputs{Tables: []access.TableRef{stripe}, Sets: []access.SetRef{general}, DecisionSets: []access.SetRef{general}}) {
		t.Fatalf("%+v", got)
	}
	all := access.NewGrants([]access.Target{access.ConnectorTarget("stripe"), access.DocumentSetTarget("general"), access.DocumentSetTarget("hr"), access.EnrichmentTarget()}).Part(holdings)
	if !all.AllDocuments || !all.Enrichment {
		t.Fatalf("%+v", all)
	}
	// The workspace holds every table, file, document and decision of it, but
	// not the profiles.
	workspace := access.NewGrants([]access.Target{access.WorkspaceTarget()}).Part(holdings)
	want = access.Part{
		Tables: []access.TableRef{stripe, crm}, Files: []string{"f1", "f2"}, AllDocuments: true, Documents: []string{"d1", "d2", "d3"},
		Sets: []access.SetRef{general, hr}, DecisionSets: []access.SetRef{general, hr}, Withheld: []access.PropRef{{Type: "B", Prop: "unrecorded"}},
	}
	if fmt.Sprintf("%+v", workspace) != fmt.Sprintf("%+v", want) {
		t.Fatalf("workspace: %+v, want %+v", workspace, want)
	}
	if !workspace.Holds(part) || part.Holds(workspace) {
		t.Fatal("the workspace's part holds a connector's and a set's, and not the other way round")
	}
}

func TestPartHolds(t *testing.T) {
	stripe := access.TableRef{ConnectorID: "stripe", Table: "customers"}
	before := access.Part{Tables: []access.TableRef{stripe}, Files: []string{"f"}, Documents: []string{"d"}, Enrichment: true,
		Withheld: []access.PropRef{{Type: "A", Prop: "p"}}}
	for name, tc := range map[string]struct {
		now  access.Part
		want bool
	}{
		"the same":        {before, true},
		"more":            {access.Part{Tables: []access.TableRef{stripe, {ConnectorID: "crm", Table: "x"}}, Files: []string{"f", "g"}, Documents: []string{"d", "e"}, Enrichment: true}, true},
		"every document":  {access.Part{Tables: []access.TableRef{stripe}, Files: []string{"f"}, AllDocuments: true, Enrichment: true}, true},
		"a table less":    {access.Part{Files: []string{"f"}, Documents: []string{"d"}, Enrichment: true}, false},
		"a file less":     {access.Part{Tables: []access.TableRef{stripe}, Documents: []string{"d"}, Enrichment: true}, false},
		"a document less": {access.Part{Tables: []access.TableRef{stripe}, Files: []string{"f"}, Enrichment: true}, false},
		"no profiles":     {access.Part{Tables: []access.TableRef{stripe}, Files: []string{"f"}, Documents: []string{"d"}}, false},
		"more withheld": {access.Part{Tables: []access.TableRef{stripe}, Files: []string{"f"}, Documents: []string{"d"}, Enrichment: true,
			Withheld: []access.PropRef{{Type: "A", Prop: "q"}}}, false},
	} {
		if got := tc.now.Holds(before); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
	wasAll := access.Part{AllDocuments: true}
	if (access.Part{Documents: []string{"d"}}).Holds(wasAll) {
		t.Error("some documents don't hold what every document did")
	}
}

func TestCoversNothing(t *testing.T) {
	g := access.NewGrants([]access.Target{access.ConnectorTarget("stripe")})
	if g.Covers(access.Inputs{}) {
		t.Error("a load holding nothing is no one's to read")
	}
	if g.Reaches(access.Inputs{}) {
		t.Error("nor is it reached")
	}
	noDocuments := g.Part(access.Holdings{Tables: []access.TableRef{{ConnectorID: "stripe", Table: "customers"}}})
	if noDocuments.AllDocuments {
		t.Error("no documents aren't every document, which would hold every route")
	}
}

func TestInputsUnion(t *testing.T) {
	a, b := access.TableRef{ConnectorID: "a", Table: "t"}, access.TableRef{ConnectorID: "b", Table: "t"}
	x, y := access.SetRef{SetID: "x"}, access.SetRef{SetID: "y"}
	got := access.Inputs{Tables: []access.TableRef{a}, Sets: []access.SetRef{x}, Enrichment: true}.Union(
		access.Inputs{Tables: []access.TableRef{a, b}, Sets: []access.SetRef{y, x}, Uncoverable: true, Unknown: true})
	want := access.Inputs{Tables: []access.TableRef{a, b}, Sets: []access.SetRef{x, y}, Enrichment: true, Uncoverable: true, Unknown: true}
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Fatalf("%+v", got)
	}
}

// A set only a decision came from must be covered, but isn't what a load holds to read.
func TestDecisionSets(t *testing.T) {
	stripe := access.TableRef{ConnectorID: "stripe", Table: "customers"}
	hr, general := access.SetRef{SetID: "hr"}, access.SetRef{SetID: "general"}
	in := access.Inputs{Tables: []access.TableRef{stripe}, DecisionSets: []access.SetRef{hr}}
	onlyHR := access.NewGrants([]access.Target{access.DocumentSetTarget("hr")})
	if onlyHR.Reaches(in) {
		t.Error("holding only what a decision came from reaches nothing of the load")
	}
	both := access.NewGrants([]access.Target{access.DocumentSetTarget("hr"), access.ConnectorTarget("stripe")})
	if !both.Covers(in) {
		t.Error("holding it and the rest covers it")
	}
	if access.NewGrants([]access.Target{access.ConnectorTarget("stripe")}).Covers(in) {
		t.Error("the table alone doesn't cover what a decision came from")
	}
	got := in.Union(access.Inputs{DecisionSets: []access.SetRef{general, hr}})
	if len(got.DecisionSets) != 2 {
		t.Fatalf("%+v", got)
	}
}

func ExampleGrants_Holds() {
	g := access.NewGrants([]access.Target{access.ConnectorTarget("stripe")})
	fmt.Println(g.Holds([]access.Target{access.TableTarget("stripe", "customers")}))
	fmt.Println(g.Holds([]access.Target{access.ConnectorTarget("stripe"), access.DocumentSetTarget("general")}))
	// Output:
	// true
	// false
}

// Holds asks for at least each target: a table by itself or its connector,
// everything else by itself; nothing is held by everything, and an unknown
// target never.
func TestGrantsHolds(t *testing.T) {
	g := access.NewGrants([]access.Target{access.ConnectorTarget("stripe"), access.TableTarget("crm", "accounts"),
		access.DocumentSetTarget("general"), access.EnrichmentTarget()})
	for name, tc := range map[string]struct {
		targets []access.Target
		want    bool
	}{
		"nothing":                     {nil, true},
		"its connector":               {[]access.Target{access.ConnectorTarget("stripe")}, true},
		"a table of its connector":    {[]access.Target{access.TableTarget("stripe", "invoices")}, true},
		"a table it holds alone":      {[]access.Target{access.TableTarget("crm", "accounts")}, true},
		"the connector of that table": {[]access.Target{access.ConnectorTarget("crm")}, false},
		"another table":               {[]access.Target{access.TableTarget("crm", "deals")}, false},
		"its set":                     {[]access.Target{access.DocumentSetTarget("general")}, true},
		"another set":                 {[]access.Target{access.DocumentSetTarget("hr")}, false},
		"profiles":                    {[]access.Target{access.EnrichmentTarget()}, true},
		"all of them":                 {[]access.Target{access.ConnectorTarget("stripe"), access.TableTarget("crm", "accounts"), access.DocumentSetTarget("general"), access.EnrichmentTarget()}, true},
		"one too many":                {[]access.Target{access.ConnectorTarget("stripe"), access.DocumentSetTarget("hr")}, false},
		"an unknown target":           {[]access.Target{{}}, false},
	} {
		assert.Equal(t, tc.want, g.Holds(tc.targets), name)
	}
	assert.False(t, access.NewGrants(nil).Holds([]access.Target{access.EnrichmentTarget()}), "no grants hold profiles")
}

// A workspace grant holds every connector, table and set, named or not, and
// is held only by itself; People enrichment it neither holds nor gives.
func TestWorkspaceGrantHolds(t *testing.T) {
	workspace := access.NewGrants([]access.Target{access.WorkspaceTarget()})
	for name, tc := range map[string]struct {
		targets []access.Target
		want    bool
	}{
		"any connector":         {[]access.Target{access.ConnectorTarget("stripe"), access.ConnectorTarget("later")}, true},
		"any table":             {[]access.Target{access.TableTarget("crm", "accounts")}, true},
		"any set":               {[]access.Target{access.DocumentSetTarget("hr")}, true},
		"itself":                {[]access.Target{access.WorkspaceTarget()}, true},
		"profiles":              {[]access.Target{access.EnrichmentTarget()}, false},
		"a set and the profile": {[]access.Target{access.DocumentSetTarget("hr"), access.EnrichmentTarget()}, false},
		"an unknown target":     {[]access.Target{{}}, false},
	} {
		assert.Equal(t, tc.want, workspace.Holds(tc.targets), name)
	}
	assert.True(t, access.NewGrants([]access.Target{access.WorkspaceTarget(), access.EnrichmentTarget()}).
		Holds([]access.Target{access.ConnectorTarget("c"), access.EnrichmentTarget()}), "with profiles granted too")
	// Nothing narrower holds it: a thread read under the workspace is lost
	// once the person holds every source by name instead.
	for name, targets := range map[string][]access.Target{
		"a connector":                  {access.ConnectorTarget("stripe")},
		"every source by name":         {access.ConnectorTarget("stripe"), access.TableTarget("crm", "accounts"), access.DocumentSetTarget("hr")},
		"profiles":                     {access.EnrichmentTarget()},
		"every source and the profile": {access.ConnectorTarget("stripe"), access.DocumentSetTarget("hr"), access.EnrichmentTarget()},
		"nothing":                      nil,
	} {
		assert.False(t, access.NewGrants(targets).Holds([]access.Target{access.WorkspaceTarget()}), name)
	}
}

func ExampleWorkspaceTarget() {
	g := access.NewGrants([]access.Target{access.WorkspaceTarget()})
	fmt.Println(g.Covers(access.Inputs{Tables: []access.TableRef{{ConnectorID: "added-later", Table: "accounts"}}}))
	fmt.Println(g.Covers(access.Inputs{Sets: []access.SetRef{{SetID: "contracts"}}, Enrichment: true}))
	fmt.Println(g.Holds([]access.Target{access.ConnectorTarget("stripe")}), access.NewGrants([]access.Target{access.ConnectorTarget("stripe")}).Holds([]access.Target{access.WorkspaceTarget()}))
	// Output:
	// true
	// false
	// true false
}
