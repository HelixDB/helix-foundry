package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSchemaMerge(t *testing.T) {
	// Merge runs once without the lock and again under it: the first run
	// must leave the schema it read untouched.
	t.Run("leaves the schema it merges into unchanged", func(t *testing.T) {
		s := schemaOf(map[string]Type{"a": TypeNull})
		merged, err := s.Merge(validSend(t, Piece{ID: "o-1", Data: []byte(`{"a":1,"b":"x"}`)}), 200)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(s.Fields()); got != "[{a null}]" {
			t.Fatalf("schema became %s", got)
		}
		if got := fmt.Sprint(merged.Schema.Fields()); got != "[{a number} {b string}]" {
			t.Fatalf("merged %s", got)
		}
	})

	// new_fields lists what the send added; a field that was only null and
	// now has a type changes the stored schema but is not new, and adds no
	// column.
	t.Run("tells new fields from locked ones", func(t *testing.T) {
		s := schemaOf(map[string]Type{"held": TypeString, "note": TypeNull})
		merged, err := s.Merge(validSend(t,
			Piece{ID: "o-1", Data: []byte(`{"zeta":null,"note":true,"held":"x"}`)},
			Piece{ID: "o-2", Data: []byte(`{"alpha":[],"zeta":{}}`)},
		), 200)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprint(merged.New); got != "[{alpha array} {zeta object}]" {
			t.Errorf("new %s", got)
		}
		if got := fmt.Sprint(merged.Locked); got != "[{note boolean}]" {
			t.Errorf("locked %s", got)
		}
		if !merged.Changed() {
			t.Error("not changed")
		}
	})

	// A send of held fields with held types writes nothing to the
	// connectors row, so its updated_at moves only on real changes.
	t.Run("is unchanged by a send of held fields", func(t *testing.T) {
		s := schemaOf(map[string]Type{"a": TypeNumber, "b": TypeNull})
		merged, err := s.Merge(validSend(t, Piece{ID: "o-1", Data: []byte(`{"a":2,"b":null}`)}), 200)
		if err != nil {
			t.Fatal(err)
		}
		if merged.Changed() || len(merged.New) != 0 || len(merged.Locked) != 0 {
			t.Fatalf("changed: %+v", merged)
		}
	})

	// A type conflict is one piece's, which a front end names by its place;
	// the field cap is the send's as a whole.
	t.Run("names the piece that breaks a lock, and none past the field cap", func(t *testing.T) {
		s := schemaOf(map[string]Type{"a": TypeNumber})
		send := validSend(t, Piece{ID: "o-1", Data: []byte(`{"a":1}`)}, Piece{ID: "o-2", Data: []byte(`{"b":null}`)}, Piece{ID: "o-3", Data: []byte(`{"b":[],"a":"1"}`)})
		_, err := s.Merge(send, 200)
		var r *Refusal
		if !errors.As(err, &r) || r.Kind != Invalid || r.Piece != 3 || r.Message != `piece 3 (id "o-3"): "a" is a string; this source holds "a" as a number` {
			t.Fatalf("got %+v", err)
		}
		_, err = s.Merge(send, 1)
		if !errors.As(err, &r) || r.Kind != Invalid || r.Piece != 3 {
			t.Fatalf("a type conflict past the cap = %+v, want the conflict first", err)
		}
		_, err = s.Merge(validSend(t, Piece{ID: "o-1", Data: []byte(`{"a":1}`)}, Piece{ID: "o-2", Data: []byte(`{"b":1}`)}), 1)
		if !errors.As(err, &r) || r.Kind != TooManyFields || r.Piece != 0 {
			t.Fatalf("got %+v", err)
		}
	})

	// Lowering max_fields must not stop a source that is already past it
	// from sending pieces of the fields it has.
	t.Run("never refuses a send without new fields for the count", func(t *testing.T) {
		s := numberFields(5)
		if _, err := s.Merge(validSend(t, Piece{ID: "o-1", Data: []byte(`{"f1":1}`)}), 3); err != nil {
			t.Fatal(err)
		}
		_, err := s.Merge(validSend(t, Piece{ID: "o-1", Data: []byte(`{"g":1}`)}), 3)
		var r *Refusal
		if !errors.As(err, &r) || r.Kind != TooManyFields {
			t.Fatalf("got %v", err)
		}
	})
}

func TestSchemaJSON(t *testing.T) {
	// config.schema is stored data: field to type name, which
	// reads back to the same schema.
	t.Run("round-trips through config.schema", func(t *testing.T) {
		in := `{"a":"string","b":"number","c":"boolean","d":"object","e":"array","f":"null"}`
		var s Schema
		if err := json.Unmarshal([]byte(in), &s); err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != in {
			t.Fatalf("got %s", out)
		}
	})

	// A source with no fields yet stores an object, not null, so JSON_SET
	// paths into it work.
	t.Run("writes no fields as an empty object", func(t *testing.T) {
		out, err := json.Marshal(Schema{})
		if err != nil || string(out) != "{}" {
			t.Fatalf("got %s, %v", out, err)
		}
		var s Schema
		if err := json.Unmarshal([]byte("null"), &s); err != nil || s.Len() != 0 {
			t.Fatalf("null read as %v, %v", s.Fields(), err)
		}
	})

	// A stored schema no send could have written is corrupt config: reading
	// it must fail rather than build columns from it.
	for name, in := range map[string]string{
		"an unknown type":   `{"a":"integer"}`,
		"a reserved name":   `{"id":"string"}`,
		"a backtick":        `{"a` + "`" + `":"string"}`,
		"a control char":    `{"a\u0001":"string"}`,
		"an empty name":     `{"":"string"}`,
		"a 65-byte name":    `{"` + strings.Repeat("k", 65) + `":"string"}`,
		"a non-string type": `{"a":1}`,
		"a null type":       `{"a":null}`,
		"not an object":     `[]`,
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			var s Schema
			if err := json.Unmarshal([]byte(in), &s); err == nil {
				t.Fatalf("read %v", s.Fields())
			}
		})
	}

	// A Type outside the enum has no stored name.
	t.Run("refuses to write a type outside the enum", func(t *testing.T) {
		if _, err := Type(9).MarshalText(); err == nil {
			t.Fatal("wrote Type(9)")
		}
		if got := Type(-1).String(); got != "Type(-1)" {
			t.Fatalf("got %s", got)
		}
	})
}

func TestSchemaColumns(t *testing.T) {
	// connector_tables.columns of events lists the row's own columns first,
	// then the fields by name, each with its learned type.
	s := schemaOf(map[string]Type{"b": TypeNull, "a": TypeObject})
	if got := fmt.Sprint(s.Columns()); got != "[{id string} {received_at string} {occurred_at string} {a object} {b null}]" {
		t.Fatalf("got %s", got)
	}
}

func TestNullFill(t *testing.T) {
	// NullFill is exactly what padding adds to a row (len(json name) + 6 per
	// field), escapes included, so the byte cap and the read budget count
	// what a merge writes.
	s := schemaOf(map[string]Type{"a": TypeNumber, `q"b\`: TypeString, "<\u2028é>": TypeNull})
	if got, want := s.NullFill(), int64(len(`,"a":null`)+len(`,"q\"b\\":null`)+len(`,"<\u2028é>":null`)); got != want {
		t.Fatalf("NullFill %d, want %d", got, want)
	}
	received := time.Date(2026, 10, 23, 18, 25, 1, 0, time.UTC)
	l, err := DecodeLine([]byte(lines(t, validSend(t, Piece{ID: "o-1", Data: []byte(`{}`)}), received)))
	if err != nil {
		t.Fatal(err)
	}
	bare, _ := l.Row(Schema{})
	padded, err := l.Row(s)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(padded)-len(bare)) != s.NullFill() {
		t.Fatalf("padding wrote %d bytes, NullFill %d", len(padded)-len(bare), s.NullFill())
	}
}

func TestTextFields(t *testing.T) {
	raw, err := os.ReadFile("testdata/pieces.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Limits struct {
			MaxFields int `json:"max_fields"`
		} `json:"limits"`
		TextFields []struct {
			Name         string            `json:"name"`
			Schema       map[string]string `json:"schema"`
			SchemaFields int               `json:"schema_fields"`
			TextField    string            `json:"text_field"`
			Want         struct {
				Refused      string            `json:"refused"`
				Message      string            `json:"message"`
				Schema       map[string]string `json:"schema"`
				SchemaFields int               `json:"schema_fields"`
			} `json:"want"`
		} `json:"text_fields"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.TextFields) == 0 {
		t.Fatal("pieces.json has no text_fields")
	}
	kinds := map[string]RefusalKind{"invalid": Invalid, "fields": TooManyFields}
	// Every rule a source's text field meets, from the table the site's
	// mock shares: a text field the backend refuses must be one the mock
	// refuses, with the same words, and one it accepts must lock the same
	// schema, or the site promises extraction a source can't have.
	for _, c := range fx.TextFields {
		t.Run(c.Name, func(t *testing.T) {
			held := map[string]string{}
			for i := range c.SchemaFields {
				held[fmt.Sprint("f", i+1)] = "number"
			}
			maps.Copy(held, c.Schema)
			stored, _ := json.Marshal(held)
			var s Schema
			if err := json.Unmarshal(stored, &s); err != nil {
				t.Fatal(err)
			}
			got, err := s.WithText(c.TextField, fx.Limits.MaxFields)
			if c.Want.Refused != "" {
				var r *Refusal
				if !errors.As(err, &r) || r.Kind != kinds[c.Want.Refused] || r.Message != c.Want.Message {
					t.Fatalf("got %v, want refused %s: %s", err, c.Want.Refused, c.Want.Message)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.types[c.TextField] != TypeString {
				t.Errorf("%q is %s, not a string", c.TextField, got.types[c.TextField])
			}
			if c.Want.SchemaFields > 0 {
				if got.Len() != c.Want.SchemaFields {
					t.Errorf("schema of %d fields, want %d", got.Len(), c.Want.SchemaFields)
				}
				return
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(c.Want.Schema)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("schema %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestWithText(t *testing.T) {
	// The setting is checked once before the row lock and again under it:
	// the schema read first must stay as it was, whichever way it goes.
	t.Run("leaves the schema it locks in unchanged", func(t *testing.T) {
		s := schemaOf(map[string]Type{"note": TypeNull, "n": TypeNumber})
		for _, name := range []string{"note", "reply"} {
			got, err := s.WithText(name, 200)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(s.Fields()) != "[{n number} {note null}]" {
				t.Fatalf("WithText(%q) changed the schema to %v", name, s.Fields())
			}
			if got.types[name] != TypeString {
				t.Fatalf("WithText(%q) gave %v", name, got.Fields())
			}
		}
	})

	// A source no piece has reached yet can still name its text field, so
	// an agent sets it when it creates the source.
	t.Run("adds a field to the zero schema", func(t *testing.T) {
		got, err := Schema{}.WithText("reply", 1)
		if err != nil || fmt.Sprint(got.Fields()) != "[{reply string}]" {
			t.Fatalf("got %v, %v", got.Fields(), err)
		}
		if _, err := (Schema{}).WithText("reply", 0); err == nil {
			t.Fatal("added a field past a cap of 0")
		}
	})

	// A field the text field adds is a column like any other: its JSON name
	// pads every row (NullFill), escapes included.
	t.Run("pads the field it adds", func(t *testing.T) {
		got, err := Schema{}.WithText(`q"b`, 200)
		if err != nil {
			t.Fatal(err)
		}
		if want := int64(len(`,"q\"b":null`)); got.NullFill() != want {
			t.Fatalf("NullFill %d, want %d", got.NullFill(), want)
		}
	})
}

func TestValidFieldName(t *testing.T) {
	// A text field may name only a field a piece could send: the names a
	// send's keys may have, no more and no fewer.
	for _, name := range []string{
		"", "a", "ID", "data", "a b", "a b", "é",
		strings.Repeat("k", 64), strings.Repeat("k", 65),
		strings.Repeat("é", 32), strings.Repeat("é", 33),
		"a\u0001", "\t", "a\u007f", "a\u0085", "a`", "id", "received_at", "occurred_at",
	} {
		named, keyed := ValidFieldName(name), checkKey(name)
		if (named == nil) != (keyed == nil) {
			t.Errorf("%q: ValidFieldName %v, but a send's key %v", name, named, keyed)
		}
		var r *Refusal
		if named != nil && (!errors.As(named, &r) || r.Kind != Invalid) {
			t.Errorf("%q: refused %v, not as Invalid", name, named)
		}
	}
}
