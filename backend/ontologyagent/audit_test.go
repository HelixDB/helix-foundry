package ontologyagent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
)

// fp is a fingerprint of hex digit d.
func fp(d string) string { return strings.Repeat(d, 64) }

// unmatchedFinding is an unmatched finding of canonical, backed by count
// reviews from sources documents with examples of these documents, and its
// route proposal; extra fields are added to its signal as sent.
func unmatchedFinding(fingerprint, canonical string, count, sources int, docs []string, extra string) string {
	var examples []string
	for _, d := range docs {
		examples = append(examples, fmt.Sprintf(`{"doc_id":%q,"quote":"Acme supplies Helio"}`, d))
	}
	return fmt.Sprintf(`{"fingerprint":%q,"signal":{"kind":"unmatched","canonical":%q,"set":"set_1","backing":{"count":%d,"sources":%d,"examples":[%s]}%s},
"proposal":{"proposal":"route","set":"set_1","links":["supplies"],"target":{"count":"unmatched","canonicals":[%q]}}}`,
		fingerprint, canonical, count, sources, strings.Join(examples, ","), extra, canonical)
}

const rowsFinding = `{"fingerprint":"` + "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc" + `","signal":{"kind":"row_errors","mapping":"people",
"errors":{"count":3,"examples":["row 4"]}},"proposal":{"proposal":"draft","brief":{"brief":"mapping","mapping":"people"},"target":{"count":"row_errors","mapping":"people"}}}`

// done is a done result holding findings, mined as given.
func done(mined string, findings ...string) string {
	return fmt.Sprintf(`{"outcome":"done","findings":[%s],"mined":%s,"rows":"measured","documents":2,"unread":0,"new_link_support":{"docs":7,"quotes":7}}`,
		strings.Join(findings, ","), mined)
}

const complete = `{"mined":"complete"}`

func TestAuditRequestContract(t *testing.T) {
	schema := contracttest.Schema(t, "audit_request.json")
	base := AuditRequest{
		RequestID: "run:0", Database: "tenant", Load: "load_01", OntologyTOML: "version = \"v1\"\n", OntologyVersion: "foundry-v3",
		MappingsTOML: "version = \"v1\"\n", Helix: HelixTarget{URL: "http://127.0.0.1:7001", Token: "t", MaxRequestBytes: 1 << 20, TimeoutSecs: 30},
		DocIDs: []string{"doc_1", "doc_2"},
	}
	withRows := base
	withRows.DocIDs = []string{}
	withRows.Rows = &AuditRows{RowErrors: []AuditMappingErrors{{Mapping: "people", Count: 2, Examples: []string{"row 4"}}}, SourceDrift: []AuditMappingDrift{}}
	for name, req := range map[string]AuditRequest{"a page": base, "rows alone": withRows} {
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := contracttest.Validate(t, schema, body); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, body)
		}
		if name == "a page" && strings.Contains(string(body), `"rows"`) {
			t.Fatalf("a page without rows sends them: %s", body)
		}
	}
}

func TestDecodeAuditResult(t *testing.T) {
	schema := contracttest.Schema(t, "audit_result.json")
	valid := map[string]string{
		"complete":      done(complete, unmatchedFinding(fp("a"), "supplier_of", 2, 2, []string{"d1", "d2"}, ""), rowsFinding),
		"cut":           done(`{"mined":"cut","kinds":["unresolved"]}`),
		"load_unknown":  `{"outcome":"load_unknown"}`,
		"load_mismatch": `{"outcome":"load_mismatch","stored_version":"foundry-v2"}`,
	}
	for name, raw := range valid {
		if err := contracttest.Validate(t, schema, []byte(raw)); err != nil {
			t.Fatalf("%s isn't the contract's: %v", name, err)
		}
	}
	r, err := DecodeAuditResult([]byte(valid["complete"]))
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != AuditDone || r.Cut != nil || !r.RowsMeasured || len(r.Findings) != 2 {
		t.Fatalf("complete = %+v", r)
	}
	if f := r.Findings[0]; f.Fingerprint != fp("a") || f.Kind != "unmatched" || f.backing == nil || f.backing.Count != 2 || f.backing.Sources != 2 {
		t.Fatalf("unmatched = %+v", f)
	}
	if f := r.Findings[1]; f.Kind != "row_errors" || f.backing != nil || f.rowSupport() != 3 {
		t.Fatalf("rows = %+v", f)
	}
	if r, err = DecodeAuditResult([]byte(valid["cut"])); err != nil || len(r.Cut) != 1 || r.Cut[0] != "unresolved" {
		t.Fatalf("cut = %+v, %v", r, err)
	}
	if r, err = DecodeAuditResult([]byte(valid["load_unknown"])); err != nil || r.Outcome != AuditLoadUnknown {
		t.Fatalf("load_unknown = %+v, %v", r, err)
	}
	if r, err = DecodeAuditResult([]byte(valid["load_mismatch"])); err != nil || r.StoredVersion != "foundry-v2" {
		t.Fatalf("load_mismatch = %+v, %v", r, err)
	}
	unmeasured := strings.Replace(valid["cut"], `"rows":"measured"`, `"rows":"unmeasured"`, 1)
	if r, err = DecodeAuditResult([]byte(unmeasured)); err != nil || r.RowsMeasured {
		t.Fatalf("unmeasured = %+v, %v", r, err)
	}

	// A later contract's fields and kinds are kept, not refused: 4.8.0's
	// spellings, a route's gathered and a draft's origin, and a kind this
	// contract lacks.
	later := done(complete,
		unmatchedFinding(fp("a"), "date_of_death", 2, 2, []string{"d1"}, `,"spellings":["date_of_death","date_of_death_no_link"]`),
		`{"fingerprint":"`+fp("d")+`","signal":{"kind":"held","link":"ceo_of","backing":{"count":2,"sources":2,"examples":[]}},"proposal":{"proposal":"draft","origin":"finding","brief":{"brief":"description","link":"ceo_of"},"target":{"count":"facts","link":"ceo_of"}},"later":true}`)
	later = strings.Replace(later, `"target":{"count":"unmatched"`, `"gathered":true,"target":{"count":"unmatched"`, 1)
	r, err = DecodeAuditResult([]byte(later))
	if err != nil {
		t.Fatal(err)
	}
	var sums AuditSums
	if err := sums.Add(r.Findings); err != nil {
		t.Fatal(err)
	}
	seen, err := sums.Seen()
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{`"spellings":["date_of_death","date_of_death_no_link"]`, `"gathered":true`} {
		if !strings.Contains(string(seen[0].Finding), kept) {
			t.Fatalf("%s lost: %s", kept, seen[0].Finding)
		}
	}
	if seen[1].Kind != "held" || !strings.Contains(string(seen[1].Finding), `"later":true`) || !strings.Contains(string(seen[1].Finding), `"origin":"finding"`) {
		t.Fatalf("a later kind = %+v", seen[1])
	}

	for name, raw := range map[string]string{
		"not json":                    `{`,
		"another outcome":             `{"outcome":"busy"}`,
		"a mismatch with no version":  `{"outcome":"load_mismatch"}`,
		"no findings":                 `{"outcome":"done","mined":{"mined":"complete"},"rows":"measured"}`,
		"no mined":                    `{"outcome":"done","findings":[],"rows":"measured"}`,
		"another mined":               done(`{"mined":"partly"}`),
		"a cut of nothing":            done(`{"mined":"cut","kinds":[]}`),
		"a short fingerprint":         done(complete, strings.Replace(rowsFinding, fp("c"), "cc", 1)),
		"an uppercase fingerprint":    done(complete, strings.Replace(rowsFinding, fp("c"), fp("C"), 1)),
		"no signal":                   done(complete, `{"fingerprint":"`+fp("a")+`","proposal":null}`),
		"no kind":                     done(complete, `{"fingerprint":"`+fp("a")+`","signal":{"mapping":"m"},"proposal":null}`),
		"a kind too long to store":    done(complete, `{"fingerprint":"`+fp("a")+`","signal":{"kind":"`+strings.Repeat("k", 33)+`"},"proposal":null}`),
		"a backing of nothing":        done(complete, unmatchedFinding(fp("a"), "x", 0, 1, nil, "")),
		"a backing of no documents":   done(complete, unmatchedFinding(fp("a"), "x", 1, 0, nil, "")),
		"a backing that isn't one":    done(complete, `{"fingerprint":"`+fp("a")+`","signal":{"kind":"unmatched","backing":3},"proposal":null}`),
		"a finding that isn't object": done(complete, `3`),
	} {
		if _, err := DecodeAuditResult([]byte(raw)); err == nil {
			t.Fatalf("%s was read", name)
		}
	}
}

func TestAuditSums(t *testing.T) {
	docs := func(from, n int) []string {
		var out []string
		for i := range n {
			out = append(out, fmt.Sprintf("d%03d", from+i))
		}
		return out
	}
	page := func(findings ...string) []AuditFinding {
		t.Helper()
		r, err := DecodeAuditResult([]byte(done(complete, findings...)))
		if err != nil {
			t.Fatal(err)
		}
		return r.Findings
	}
	var sums AuditSums
	for _, p := range [][]AuditFinding{
		page(unmatchedFinding(fp("a"), "supplier_of", 15, 12, docs(0, 12), `,"spellings":["supplier_of"]`), rowsFinding),
		page(unmatchedFinding(fp("b"), "ceo_of", 2, 2, docs(100, 2), "")),
		page(unmatchedFinding(fp("a"), "supplier_of", 10, 9, docs(200, 9), `,"spellings":["supplier_of_parts","supplier_of"]`)),
	} {
		if err := sums.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	if sums.Len() != 3 {
		t.Fatalf("sums hold %d findings", sums.Len())
	}
	seen, err := sums.Seen()
	if err != nil {
		t.Fatal(err)
	}
	if seen[0].Fingerprint != fp("a") || seen[1].Fingerprint != fp("c") || seen[2].Fingerprint != fp("b") {
		t.Fatalf("not in the order first found: %v", seen)
	}
	a := seen[0]
	if a.Support != 25 || a.Sources != 21 {
		t.Fatalf("summed = %d reviews, %d documents", a.Support, a.Sources)
	}
	var finding struct {
		Signal struct {
			Spellings []string `json:"spellings"`
			Backing   struct {
				Count    int `json:"count"`
				Sources  int `json:"sources"`
				Examples []struct {
					DocID string `json:"doc_id"`
				} `json:"examples"`
			} `json:"backing"`
		} `json:"signal"`
		Proposal struct {
			Proposal string `json:"proposal"`
		} `json:"proposal"`
	}
	if err := json.Unmarshal(a.Finding, &finding); err != nil {
		t.Fatal(err)
	}
	b := finding.Signal.Backing
	if b.Count != 25 || b.Sources != 21 || len(b.Examples) != MaxAuditExamples || b.Examples[0].DocID != "d000" || b.Examples[12].DocID != "d200" {
		t.Fatalf("backing = %+v", b)
	}
	if strings.Join(finding.Signal.Spellings, ",") != "supplier_of,supplier_of_parts" || finding.Proposal.Proposal != "route" {
		t.Fatalf("finding = %+v", finding)
	}
	if seen[1].Kind != "row_errors" || seen[1].Support != 3 || seen[1].Sources != 0 {
		t.Fatalf("rows = %+v", seen[1])
	}

	// Each stays the contract's finding.
	var findings []string
	for _, s := range seen {
		findings = append(findings, string(s.Finding))
	}
	if err := contracttest.Validate(t, contracttest.Schema(t, "audit_result.json"), []byte(done(complete, findings...))); err != nil {
		t.Fatal(err)
	}

	if err := sums.Add(page(strings.Replace(rowsFinding, `"kind":"row_errors"`, `"kind":"source_drift"`, 1))); err == nil {
		t.Fatal("one fingerprint of two kinds was summed")
	}
	if err := sums.Add(page(`{"fingerprint":"` + fp("a") + `","signal":{"kind":"unmatched","canonical":"supplier_of"},"proposal":null}`)); err == nil {
		t.Fatal("one fingerprint backed and not was summed")
	}
}

// A finding one document backs is held thin, its counts alone, until a
// page backs it with a second: then it is whole, from that page's finding,
// its examples and spellings that page's, with the thin sum's counts. One
// still thin isn't seen, so neither stored nor shown.
func TestAuditSumsThin(t *testing.T) {
	page := func(findings ...string) []AuditFinding {
		t.Helper()
		r, err := DecodeAuditResult([]byte(done(complete, findings...)))
		if err != nil {
			t.Fatal(err)
		}
		return r.Findings
	}
	var sums AuditSums
	for _, p := range [][]AuditFinding{
		page(unmatchedFinding(fp("a"), "supplier_of", 3, 1, []string{"d000", "d009"}, `,"spellings":["supplier_of"]`),
			unmatchedFinding(fp("b"), "ceo_of", 1, 1, []string{"d001"}, "")),
		page(rowsFinding),
	} {
		if err := sums.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	if a := sums.by[fp("a")]; !a.thin() || len(a.backing.Examples) != 0 || a.signal != nil || a.backing.Count != 3 || a.backing.Sources != 1 {
		t.Fatalf("a one-document finding is held as more than its counts: %+v", a)
	}
	if err := sums.Add(page(unmatchedFinding(fp("a"), "supplier_of", 2, 1, []string{"d100"}, `,"spellings":["vendor_of"]`))); err != nil {
		t.Fatal(err)
	}
	if sums.Len() != 3 {
		t.Fatalf("sums hold %d findings", sums.Len())
	}
	seen, err := sums.Seen()
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0].Fingerprint != fp("a") || seen[1].Fingerprint != fp("c") {
		t.Fatalf("seen = %v, want a, now backed by two documents, and the rows", seen)
	}
	var finding struct {
		Signal struct {
			Canonical string   `json:"canonical"`
			Spellings []string `json:"spellings"`
			Backing   struct {
				Count    int `json:"count"`
				Sources  int `json:"sources"`
				Examples []struct {
					DocID string `json:"doc_id"`
				} `json:"examples"`
			} `json:"backing"`
		} `json:"signal"`
		Proposal struct {
			Proposal string `json:"proposal"`
		} `json:"proposal"`
	}
	if err := json.Unmarshal(seen[0].Finding, &finding); err != nil {
		t.Fatal(err)
	}
	b := finding.Signal.Backing
	if seen[0].Support != 5 || seen[0].Sources != 2 || b.Count != 5 || b.Sources != 2 || len(b.Examples) != 1 || b.Examples[0].DocID != "d100" {
		t.Fatalf("a = %+v, backing %+v", seen[0], b)
	}
	if finding.Signal.Canonical != "supplier_of" || strings.Join(finding.Signal.Spellings, ",") != "vendor_of" || finding.Proposal.Proposal != "route" {
		t.Fatalf("finding = %+v", finding)
	}
	if err := contracttest.Validate(t, contracttest.Schema(t, "audit_result.json"), []byte(done(complete, string(seen[0].Finding)))); err != nil {
		t.Fatal(err)
	}
	// A thin sum is still checked: one fingerprint, one kind.
	if err := sums.Add(page(strings.Replace(unmatchedFinding(fp("b"), "ceo_of", 1, 1, nil, ""), `"kind":"unmatched"`, `"kind":"unresolved"`, 1))); err == nil {
		t.Fatal("a thin finding of two kinds was summed")
	}
}

func TestAuditRowSupport(t *testing.T) {
	for raw, want := range map[string]int64{
		`{"kind":"row_errors","errors":{"count":4,"examples":[]}}`:                4,
		`{"kind":"source_drift","drift":{"added":["email"],"removed":["a","b"]}}`: 3,
		`{"kind":"later"}`: 1,
	} {
		var signal map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &signal); err != nil {
			t.Fatal(err)
		}
		if got := (&AuditFinding{signal: signal}).rowSupport(); got != want {
			t.Fatalf("%s: support %d, want %d", raw, got, want)
		}
	}
}

func TestUnionSpellings(t *testing.T) {
	for _, c := range []struct{ a, b, want string }{
		{`["b","a"]`, `["c","a"]`, `["a","b","c"]`},
		{`3`, `["a"]`, `["a"]`},
		{`["a"]`, `3`, `["a"]`},
	} {
		if got := string(unionSpellings(json.RawMessage(c.a), json.RawMessage(c.b))); got != c.want {
			t.Fatalf("%s ∪ %s = %s, want %s", c.a, c.b, got, c.want)
		}
	}
}

func TestIngestRowErrors(t *testing.T) {
	report := func(mappings string) json.RawMessage {
		return json.RawMessage(`{"ingest":{"status":"done","report":{"mappings":[` + mappings + `],"created":{"sweep":"swept"}}}}`)
	}
	pass := func(count int, examples ...string) string {
		encoded, _ := json.Marshal(examples)
		return fmt.Sprintf(`{"loaded":1,"row_errors":{"count":%d,"examples":%s}}`, count, encoded)
	}
	rows := IngestRowErrors(report(`{"mapping":"people","create":` + pass(2, "row 4", "row 9", "row 12") + `,"resolve":` + pass(0) + `},` +
		`{"mapping":"offices","create":` + pass(0) + `,"resolve":` + pass(1, "row 3") + `}`))
	if rows == nil || len(rows.RowErrors) != 2 || len(rows.SourceDrift) != 0 {
		t.Fatalf("rows = %+v", rows)
	}
	if e := rows.RowErrors[0]; e.Mapping != "people" || e.Count != 2 || len(e.Examples) != 2 {
		t.Fatalf("people = %+v: examples are clipped to the count", e)
	}
	if e := rows.RowErrors[1]; e.Mapping != "offices" || e.Count != 1 || e.Examples[0] != "row 3" {
		t.Fatalf("offices = %+v", e)
	}
	if clean := IngestRowErrors(report(`{"mapping":"people","create":` + pass(0) + `,"resolve":` + pass(0) + `}`)); clean == nil || len(clean.RowErrors) != 0 {
		t.Fatalf("a clean report = %+v, want measured and empty", clean)
	}
	for name, raw := range map[string]string{
		"cut to its status":   `{"ingest":{"status":"done"}}`,
		"no ingest":           `{"extract":{"status":"done"}}`,
		"an ingest not done":  `{"ingest":{"status":"cancelled","report":{"mappings":[]}}}`,
		"a pass with no rows": string(report(`{"mapping":"people","create":{"loaded":1},"resolve":` + pass(0) + `}`)),
		"not json":            `{`,
	} {
		if rows := IngestRowErrors(json.RawMessage(raw)); rows != nil {
			t.Fatalf("%s: rows %+v, want unmeasured", name, rows)
		}
	}
}
