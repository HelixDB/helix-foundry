package model

import (
	"encoding/json"
	"fmt"
	"testing"
)

func ExampleFoundryDecisionTypes() {
	types, _ := FoundryDecisionTypes(`[types.Contract]
props = { amount = "float" }`)
	fmt.Println(types["Contract"].Props["amount"])
	// Output: float
}

func TestFoundryReviewDecisionValidate(t *testing.T) {
	ptr := func(s string) *string { return &s }
	resolve := FoundryReviewDecision{ID: "id", OntologyID: "o", Type: "Customer", Kind: FoundryReviewResolve, DocID: ptr("doc"), NormName: ptr("name"), DecidedByUserID: "user"}
	prop := FoundryReviewDecision{ID: "id", OntologyID: "o", Type: "Customer", Kind: FoundryReviewProp, EntityKey: ptr("key"), Prop: ptr("name"), Value: json.RawMessage(`null`), DecidedByUserID: "user"}
	for name, tc := range map[string]struct {
		base   FoundryReviewDecision
		change func(*FoundryReviewDecision)
		valid  bool
	}{
		"resolve none":        {resolve, func(d *FoundryReviewDecision) {}, true},
		"resolve key":         {resolve, func(d *FoundryReviewDecision) { d.EntityKey = ptr("key") }, true},
		"prop null":           {prop, func(d *FoundryReviewDecision) {}, true},
		"prop number":         {prop, func(d *FoundryReviewDecision) { d.Value = json.RawMessage(`2`) }, true},
		"missing identity":    {resolve, func(d *FoundryReviewDecision) { d.ID = "" }, false},
		"missing actor":       {resolve, func(d *FoundryReviewDecision) { d.DecidedByUserID = "" }, false},
		"unknown kind":        {resolve, func(d *FoundryReviewDecision) { d.Kind = "other" }, false},
		"resolve empty key":   {resolve, func(d *FoundryReviewDecision) { d.EntityKey = ptr("") }, false},
		"resolve missing doc": {resolve, func(d *FoundryReviewDecision) { d.DocID = nil }, false},
		"resolve with prop":   {resolve, func(d *FoundryReviewDecision) { d.Prop = ptr("name") }, false},
		"resolve with value":  {resolve, func(d *FoundryReviewDecision) { d.Value = json.RawMessage(`null`) }, false},
		"prop with doc":       {prop, func(d *FoundryReviewDecision) { d.DocID = ptr("doc") }, false},
		"prop missing key":    {prop, func(d *FoundryReviewDecision) { d.EntityKey = nil }, false},
		"prop missing value":  {prop, func(d *FoundryReviewDecision) { d.Value = nil }, false},
		"prop object":         {prop, func(d *FoundryReviewDecision) { d.Value = json.RawMessage(`{}`) }, false},
		"prop array":          {prop, func(d *FoundryReviewDecision) { d.Value = json.RawMessage(`[]`) }, false},
	} {
		t.Run(name, func(t *testing.T) {
			d := tc.base
			tc.change(&d)
			if err := d.Validate(); (err == nil) != tc.valid {
				t.Fatalf("valid=%t: %v", tc.valid, err)
			}
		})
	}
	if _, err := FoundryDecisionTypes(`invalid = [`); err == nil {
		t.Fatal("invalid TOML accepted")
	}
}

// The cases below are the engine's own (schema::Key, schema::PropType,
// extraction::decision, extraction::text::norm_name doc tests and unit
// tests), so the backend refuses what Extract would.

func ExampleFoundryPropType_DecisionValue() {
	for _, c := range []struct {
		ty  FoundryPropType
		raw string
	}{
		{FoundryPropDate, `"2026-03"`},
		{FoundryPropDateTime, `"2026-03-14T09:30:00+01:00"`},
		{FoundryPropString, `"  signed "`},
		{FoundryPropInteger, `null`},
		{FoundryPropInteger, `"12"`},
		{FoundryPropString, `"   "`},
		{FoundryPropDateTime, `"9999-12-31T23:30:00-01:00"`},
	} {
		value, err := c.ty.DecisionValue(json.RawMessage(c.raw))
		fmt.Println(string(value), err != nil)
	}
	// Output:
	// "2026-03" false
	// "2026-03-14T08:30:00Z" false
	// "signed" false
	// null false
	//  true
	//  true
	//  true
}

func ExampleFoundryDecisionType_Identifies() {
	types, _ := FoundryDecisionTypes(`
[types.Customer]
props = { id = "string", since = "date" }
key = ["id", "since"]
[types.Company]
props = { name = "string" }`)
	fmt.Println(types["Customer"].Identifies(`sAA 100|d2026-03-14`), types["Customer"].Identifies(`sAA 100`))
	fmt.Println(types["Company"].Identifies("helio"), types["Company"].Identifies("Helio"))
	// Output:
	// true false
	// true false
}

func ExampleFoundryNormName() {
	fmt.Printf("%q %q %q\n", FoundryNormName("  Helio Biosciences, Inc. "), FoundryNormName("Ｈｅｌｉｏ"), FoundryNormName(".NET"))
	// Output: "helio biosciences inc" "helio" ".net"
}

func TestFoundryDecisionValue(t *testing.T) {
	for _, tc := range []struct {
		ty       FoundryPropType
		raw      string
		accepted string // the value the engine keeps; empty when it refuses raw
	}{
		{FoundryPropInteger, `3`, `3`},
		{FoundryPropInteger, `-0`, `0`},
		{FoundryPropInteger, `9223372036854775807`, `9223372036854775807`},
		{FoundryPropInteger, `9223372036854775808`, ``},
		{FoundryPropInteger, `1.5`, ``},
		{FoundryPropInteger, `3.0`, ``},
		{FoundryPropInteger, `1e2`, ``},
		{FoundryPropInteger, `"3"`, ``},
		{FoundryPropFloat, `1.5`, `1.5`},
		{FoundryPropFloat, `3`, `3`},
		{FoundryPropFloat, `1e400`, ``},
		{FoundryPropFloat, `"1.5"`, ``},
		{FoundryPropBoolean, `true`, `true`},
		{FoundryPropBoolean, `"true"`, ``},
		{FoundryPropBoolean, `1`, ``},
		{FoundryPropString, `"signed"`, `"signed"`},
		{FoundryPropString, `"\tsigned "`, `"signed"`},
		{FoundryPropString, `""`, ``},
		{FoundryPropString, `"   "`, ``},
		{FoundryPropString, `3`, ``},
		{FoundryPropDate, `"2026"`, `"2026"`},
		{FoundryPropDate, `"2026-03"`, `"2026-03"`},
		{FoundryPropDate, `"2026-10-19"`, `"2026-10-19"`},
		{FoundryPropDate, `" 2026-10-19 "`, `"2026-10-19"`},
		{FoundryPropDate, `"2024-02-29"`, `"2024-02-29"`},
		{FoundryPropDate, `"2026-02-29"`, ``},
		{FoundryPropDate, `"2026-04-31"`, ``},
		{FoundryPropDate, `"2026-13"`, ``},
		{FoundryPropDate, `"2026-00"`, ``},
		{FoundryPropDate, `"26"`, ``},
		{FoundryPropDate, `"2026-3"`, ``},
		{FoundryPropDate, `"2026-03-14-1"`, ``},
		{FoundryPropDate, `"+026"`, ``},
		{FoundryPropDate, `"no"`, ``},
		{FoundryPropDateTime, `"2026-10-19T20:00:00Z"`, `"2026-10-19T20:00:00Z"`},
		{FoundryPropDateTime, `"2026-03-14T09:30:00.25+01:00"`, `"2026-03-14T08:30:00.250Z"`},
		{FoundryPropDateTime, `"2026-03-14T08:30:00.0001Z"`, `"2026-03-14T08:30:00.000100Z"`},
		{FoundryPropDateTime, `"2026-03-14T08:30:00.000000001Z"`, `"2026-03-14T08:30:00.000000001Z"`},
		{FoundryPropDateTime, `"0000-01-01T00:30:00+01:00"`, ``},
		{FoundryPropDateTime, `"9999-12-31T23:30:00-01:00"`, ``},
		{FoundryPropDateTime, `"2026-10-19"`, ``},
		{FoundryPropDateTime, `1`, ``},
		{FoundryPropString, `null`, `null`},
		{FoundryPropDateTime, `null`, `null`},
		{FoundryPropString, `[]`, ``},
		{FoundryPropString, `{}`, ``},
		{FoundryPropString, `"a" "b"`, ``},
		{"bytes", `"a"`, ``},
	} {
		got, err := tc.ty.DecisionValue(json.RawMessage(tc.raw))
		if (err == nil) != (tc.accepted != "") || string(got) != tc.accepted {
			t.Errorf("%s %s = (%s, %v), want %q", tc.ty, tc.raw, got, err, tc.accepted)
		}
	}
}

func TestFoundryDecisionTypeIdentifies(t *testing.T) {
	types, err := FoundryDecisionTypes(`
[types.Customer]
props = { id = "string", status = "string", count = "integer", day = "date", at = "datetime", ratio = "float" }
key = ["id"]
[types.Flight]
props = { number = "string", day = "date" }
key = ["number", "day"]
[types.Seat]
props = { row = "integer", at = "datetime" }
key = ["row", "at"]
[types.Score]
props = { ratio = "float" }
key = ["ratio"]
[types.Company]
props = { name = "string" }`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ty, key string
		named   bool
	}{
		{"Customer", "sA", true},
		{"Customer", `sa\|b\\c\u{1f}`, true},
		{"Customer", "i1", false},
		{"Customer", "sA|sB", false},
		{"Customer", "invalid", false},
		{"Customer", "bad", false},
		{"Customer", "s", false},
		{"Customer", "sa|", false},
		{"Customer", `sa\q`, false},
		{"Customer", "sa\u001f", false},
		{"Customer", `sa\u{1f`, false},
		{"Customer", `sa\u{}`, false},
		{"Customer", `sa\u{d800}`, false},
		{"Customer", `sa\u{1F}`, false},
		{"Customer", `sa\u{41}`, false},
		{"Customer", "", false},
		{"Flight", "sAA 100|d2026-03-14", true},
		{"Flight", "sAA 100|d2026-13", false},
		{"Flight", "sAA 100|d 2026", false},
		{"Flight", "d2026-03-14|sAA 100", false},
		{"Seat", "i42|t2026-03-14T08:30:00Z", true},
		{"Seat", "i-7|t2026-03-14T08:30:00.250Z", true},
		{"Seat", "i007|t2026-03-14T08:30:00Z", false},
		{"Seat", "i4.2|t2026-03-14T08:30:00Z", false},
		{"Seat", "i+4|t2026-03-14T08:30:00Z", false},
		{"Seat", "i42|t2026-03-14", false},
		{"Seat", "i42|t2026-03-14T09:30:00+01:00", false},
		{"Seat", "i42|t2026-03-14T08:30:00.25Z", false},
		{"Seat", "x1|t2026-03-14T08:30:00Z", false},
		// A float or boolean can't key; the engine refuses such a type.
		{"Score", "s1.5", false},
		{"Company", "helio", true},
		{"Company", "helio biosciences inc", true},
		{"Company", "Helio", false},
		{"Company", "helio  biosciences", false},
		{"Company", "sacme", true},
		{"Company", "sAcme", false},
		{"Company", "", false},
	} {
		if got := types[tc.ty].Identifies(tc.key); got != tc.named {
			t.Errorf("%s %q identifies = %t, want %t", tc.ty, tc.key, got, tc.named)
		}
	}
}

func TestFoundryNormName(t *testing.T) {
	for in, want := range map[string]string{
		"  Helio Biosciences, Inc. ": "helio biosciences inc",
		"Ｈｅｌｉｏ":                      "helio",
		"C++":                        "c++",
		"C#":                         "c#",
		".NET":                       ".net",
		"Node.js":                    "node.js",
		"J.K. Rowling":               "j k rowling",
		"J. K. Rowling":              "j k rowling",
		"İZMİR":                      "izmir",
		"Izmir":                      "izmir",
		"ΟΔΥΣΣΕΥΣ":                   "οδυσσευσ",
		"Müller & Söhne":             "müller & söhne",
		"cus_123":                    "cus 123",
		"...":                        "",
	} {
		if got := FoundryNormName(in); got != want {
			t.Errorf("FoundryNormName(%q) = %q, want %q", in, got, want)
		}
		if again := FoundryNormName(want); again != want {
			t.Errorf("folding %q again gives %q", want, again)
		}
	}
}

func TestFoundryReviewDecisionFits(t *testing.T) {
	ptr := func(s string) *string { return &s }
	types := func(toml string) map[string]FoundryDecisionType {
		t.Helper()
		types, err := FoundryDecisionTypes(toml)
		if err != nil {
			t.Fatal(err)
		}
		return types
	}
	named := types(`[types.Customer]
props = { tier = "string", account = "string" }`)
	tier := FoundryReviewDecision{ID: "id", OntologyID: "o", Type: "Customer", Kind: FoundryReviewProp, EntityKey: ptr("helio"),
		Prop: ptr("tier"), Value: json.RawMessage(`"gold"`), DecidedByUserID: "u"}
	link := FoundryReviewDecision{ID: "id", OntologyID: "o", Type: "Customer", Kind: FoundryReviewResolve, DocID: ptr("doc"),
		NormName: ptr("helio inc"), EntityKey: ptr("helio"), DecidedByUserID: "u"}
	for name, tc := range map[string]struct {
		d     FoundryReviewDecision
		types map[string]FoundryDecisionType
		fits  bool
	}{
		"a prop decision":             {tier, named, true},
		"a resolve decision":          {link, named, true},
		"not in data":                 {func() FoundryReviewDecision { d := link; d.EntityKey = nil; return d }(), named, true},
		"its type removed":            {tier, types(`[types.Client]`), false},
		"its prop removed":            {tier, types(`[types.Customer]` + "\n" + `props = { account = "string" }`), false},
		"its prop now an integer":     {tier, types(`[types.Customer]` + "\n" + `props = { tier = "integer" }`), false},
		"its type now keyed":          {tier, types(`[types.Customer]` + "\n" + `props = { tier = "string", account = "string" }` + "\n" + `key = ["account"]`), false},
		"a link to a now keyed type":  {link, types(`[types.Customer]` + "\n" + `props = { account = "string" }` + "\n" + `key = ["account"]`), false},
		"a name not normalized":       {func() FoundryReviewDecision { d := link; d.NormName = ptr("Helio"); return d }(), named, false},
		"a blank document":            {func() FoundryReviewDecision { d := link; d.DocID = ptr(" "); return d }(), named, false},
		"a value the engine refuses":  {func() FoundryReviewDecision { d := tier; d.Value = json.RawMessage(`"  "`); return d }(), named, false},
		"a decision Validate refuses": {func() FoundryReviewDecision { d := tier; d.DocID = ptr("doc"); return d }(), named, false},
		"a link to a removed type":    {link, types(`[types.Client]`), false},
		"a link Validate refuses":     {func() FoundryReviewDecision { d := link; d.Prop = ptr("tier"); return d }(), named, false},
	} {
		if got := tc.d.Fits(tc.types); got != tc.fits {
			t.Errorf("%s: fits = %t, want %t", name, got, tc.fits)
		}
		// The engine also takes a link whose entity the type's changed key no
		// longer reads, to review its name again.
		want := tc.fits || name == "a link to a now keyed type"
		if got := tc.d.Extractable(tc.types); got != want {
			t.Errorf("%s: extractable = %t, want %t", name, got, want)
		}
	}
}
