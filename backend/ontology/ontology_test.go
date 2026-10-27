package ontology

import (
	"errors"
	"strings"
	"testing"
)

var tables = []Table{
	{ConnectorID: "c1", Name: "customers", Columns: []string{"customer_id", "email", "created"}},
	{ConnectorID: "c1", Name: "invoices", Columns: []string{"invoice_id", "customer_id", "total"}},
	{ConnectorID: "c2", Name: "events", Columns: nil},
}

const valid = `{
  "entities": [
    {"name": "Customer", "description": "A paying account", "source": {"connector_id": "c1", "table": "customers"},
     "primary_key": "customer_id", "properties": [{"name": "email", "type": "string", "description": "Billing email"}]},
    {"name": "Invoice", "description": "", "source": {"connector_id": "c1", "table": "invoices"},
     "primary_key": "invoice_id", "properties": [{"name": "customer_id", "type": "string", "description": ""}]},
    {"name": "Event", "description": "", "source": {"connector_id": "c2", "table": "events"},
     "primary_key": "event_id", "properties": []}
  ],
  "relationships": [
    {"name": "pays", "from": "Customer", "to": "Invoice", "from_property": "customer_id", "to_property": "customer_id", "cardinality": "one_to_many"}
  ],
  "rules": [{"name": "has_email", "description": "", "entity": "Customer", "expression": "email IS NOT NULL"}],
  "constraints": [{"name": "email_unique", "entity": "Customer", "kind": "unique", "properties": ["email"]}]
}`

func problems(t *testing.T, raw string) []string {
	t.Helper()
	d, err := Parse([]byte(raw))
	if err == nil {
		err = Validate(d, tables)
	}
	if err == nil {
		return nil
	}
	var invalidErr *InvalidError
	if !errors.As(err, &invalidErr) {
		t.Fatalf("error = %v, want *InvalidError", err)
	}
	return invalidErr.Problems
}

func TestValidate(t *testing.T) {
	t.Run("accepts a definition on synced tables", func(t *testing.T) {
		if got := problems(t, valid); got != nil {
			t.Fatalf("problems = %q", got)
		}
	})
	// Each broken reference is named so the author knows what to fix.
	for name, tc := range map[string]struct{ from, to, want string }{
		"an unsynced table":           {`"table": "invoices"`, `"table": "refunds"`, `reads refunds of connector c1, which is not a synced table`},
		"a missing key column":        {`"primary_key": "invoice_id"`, `"primary_key": "id"`, `entity "Invoice" primary key "id" is not a column of invoices`},
		"a missing property column":   {`{"name": "email", "type"`, `{"name": "mail", "type"`, `entity "Customer" property "mail" is not a column of customers`},
		"a relationship to nothing":   {`"to": "Invoice"`, `"to": "Bill"`, `relationship "pays" to "Bill" is not a defined entity`},
		"a relationship on a non-key": {`"to_property": "customer_id"`, `"to_property": "total"`, `relationship "pays" uses "total", which is not a property of "Invoice"`},
		"an unknown cardinality":      {`"one_to_many"`, `"several"`, `relationship "pays" cardinality "several" is not one of`},
		"a rule on nothing":           {`"entity": "Customer", "expression"`, `"entity": "Client", "expression"`, `rule "has_email" applies to entity "Client"`},
		"an unknown constraint kind":  {`"kind": "unique"`, `"kind": "primary"`, `constraint "email_unique" kind "primary" is not one of`},
		"a duplicate entity":          {`"name": "Event"`, `"name": "Invoice"`, `entity "Invoice" is defined twice`},
	} {
		t.Run("names "+name, func(t *testing.T) {
			if !strings.Contains(valid, tc.from) {
				t.Fatalf("fixture lacks %q", tc.from)
			}
			got := strings.Join(problems(t, strings.Replace(valid, tc.from, tc.to, 1)), "; ")
			if !strings.Contains(got, tc.want) {
				t.Fatalf("problems = %q, want %q", got, tc.want)
			}
		})
	}
	// A typo in a field name would otherwise drop the field silently.
	t.Run("rejects unknown fields", func(t *testing.T) {
		got := problems(t, `{"entities": [], "entitys": []}`)
		if len(got) != 1 || !strings.Contains(got[0], "unknown field") {
			t.Fatalf("problems = %q", got)
		}
	})
	t.Run("rejects an empty ontology", func(t *testing.T) {
		got := problems(t, `{"entities": [], "relationships": [], "rules": [], "constraints": []}`)
		if len(got) != 1 || got[0] != "define at least one entity" {
			t.Fatalf("problems = %q", got)
		}
	})
	t.Run("rejects an oversized definition", func(t *testing.T) {
		if got := problems(t, `{"rules":[{"name":"`+strings.Repeat("x", MaxBytes)+`"}]}`); len(got) != 1 {
			t.Fatalf("problems = %q", got)
		}
	})
}
