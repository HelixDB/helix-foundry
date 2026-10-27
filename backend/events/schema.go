package events

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
)

// Type is the JSON type a field holds. TypeNull, the zero Type, is a field
// that has only ever been null: its first non-null value locks its type.
type Type int

const (
	TypeNull Type = iota
	TypeString
	TypeNumber
	TypeBoolean
	TypeObject
	TypeArray
)

var typeNames = [...]string{"null", "string", "number", "boolean", "object", "array"}

// String is the type's name as config.schema and connector_tables.columns
// store it.
func (t Type) String() string {
	if t < 0 || int(t) >= len(typeNames) {
		return fmt.Sprintf("Type(%d)", int(t))
	}
	return typeNames[t]
}

func (t Type) MarshalText() ([]byte, error) {
	if t < 0 || int(t) >= len(typeNames) {
		return nil, fmt.Errorf("events: no type %d", int(t))
	}
	return []byte(typeNames[t]), nil
}

func (t *Type) UnmarshalText(text []byte) error {
	i := slices.Index(typeNames[:], string(text))
	if i < 0 {
		return fmt.Errorf("events: no type %q", text)
	}
	*t = Type(i)
	return nil
}

// phrase is the type with its article, as messages say it.
func (t Type) phrase() string {
	if t == TypeObject || t == TypeArray {
		return "an " + t.String()
	}
	return "a " + t.String()
}

// Field is one top-level field of a source's pieces.
type Field struct {
	Name string
	Type Type
}

// Schema is the fields a source's pieces have carried: each top-level key
// with the JSON type it holds. Fields only grow, and a type once locked never
// changes. The zero Schema has no fields. It is stored in connectors.config
// as {"schema": {"<field>": "<type>"}}, through MarshalJSON and
// UnmarshalJSON.
type Schema struct {
	types map[string]Type
	// fields are types' entries by byte order of name, as rows order them,
	// each with its name JSON-encoded.
	fields []schemaField
}

type schemaField struct {
	Field
	json []byte
}

func newSchema(types map[string]Type) Schema {
	fields := make([]schemaField, 0, len(types))
	for _, name := range slices.Sorted(maps.Keys(types)) {
		fields = append(fields, schemaField{Field: Field{Name: name, Type: types[name]}, json: jsonString(name)})
	}
	return Schema{types: types, fields: fields}
}

// Len is how many fields the schema has.
func (s Schema) Len() int { return len(s.fields) }

// Fields are the schema's fields by byte order of name.
func (s Schema) Fields() []Field {
	out := make([]Field, len(s.fields))
	for i, f := range s.fields {
		out[i] = f.Field
	}
	return out
}

// Columns are the columns of the source's events table, in the order its
// rows have them: id, received_at and occurred_at, then the fields.
func (s Schema) Columns() []Field {
	return append([]Field{{"id", TypeString}, {"received_at", TypeString}, {"occurred_at", TypeString}}, s.Fields()...)
}

// NullFill is what a row that has none of the schema's fields spends on
// them: `,"<name>":null` each, the name JSON-encoded as rows write it.
func (s Schema) NullFill() int64 {
	var n int64
	for _, f := range s.fields {
		n += nullFill(f)
	}
	return n
}

func nullFill(f schemaField) int64 { return int64(len(f.json) + len(`,:null`)) }

func (s Schema) MarshalJSON() ([]byte, error) {
	if s.types == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(s.types)
}

// UnmarshalJSON reads a stored schema. A name no piece could have sent, or a
// type that is not one, is corrupt config and an error. JSON null is the
// empty schema.
func (s *Schema) UnmarshalJSON(b []byte) error {
	// Type names are read as strings: encoding/json would leave a JSON null
	// as the zero Type, which is the type "null".
	var names map[string]string
	if err := json.Unmarshal(b, &names); err != nil {
		return fmt.Errorf("events: read schema: %w", err)
	}
	types := make(map[string]Type, len(names))
	for name, typ := range names {
		var t Type
		if err := t.UnmarshalText([]byte(typ)); err != nil || checkKey(name) != nil {
			return fmt.Errorf("events: read schema: %q is not a field of a type", name)
		}
		types[name] = t
	}
	*s = newSchema(types)
	return nil
}

// Merged is a schema after a send's fields joined it.
type Merged struct {
	Schema Schema
	// New are the fields the send added to the source, by name, each with
	// its type after the send: the response's new_fields. Their columns are
	// padded into every stored row at the next merge.
	New []Field
	// Locked are fields that had only been null and now hold a type. They
	// change the stored schema but add no column.
	Locked []Field
}

// Changed reports a schema the send's transaction must write.
func (m Merged) Changed() bool { return len(m.New)+len(m.Locked) > 0 }

// Merge adds a send's fields to the schema, in piece order, without changing
// s. A non-null value of a field must have the field's locked type: the first
// that does not is a *Refusal of kind Invalid whose Piece is its piece's
// place. Null is always allowed, and a field seen only as null takes the type
// of its first non-null value. A send that adds fields beyond maxFields is a
// *Refusal of kind TooManyFields, of no one piece; a send that adds none is
// never refused for the count, so lowering max_fields never stops a source's
// sends.
func (s Schema) Merge(send Send, maxFields int) (Merged, error) {
	types := maps.Clone(s.types)
	if types == nil {
		types = map[string]Type{}
	}
	var added, locked []string
	for i, p := range send.pieces {
		for _, f := range p.fields {
			held, ok := types[f.Name]
			switch {
			case !ok:
				types[f.Name] = f.Type
				added = append(added, f.Name)
			case f.Type == TypeNull || f.Type == held:
			case held == TypeNull:
				types[f.Name] = f.Type
				// A field this send added as null and then typed is new, not locked.
				if _, before := s.types[f.Name]; before {
					locked = append(locked, f.Name)
				}
			default:
				return Merged{}, refusePiece(Invalid, i, p.id, "%q is %s; this source holds %q as %s", f.Name, f.Type.phrase(), f.Name, held.phrase())
			}
		}
	}
	if len(added) > 0 && len(types) > maxFields {
		return Merged{}, refuse(TooManyFields, "this source has %d fields and this send adds %d, past the %d a source may have: send pieces without new fields, or make another source", len(s.types), len(added), maxFields)
	}
	merged := newSchema(types)
	named := func(names []string) []Field {
		slices.Sort(names)
		out := make([]Field, len(names))
		for i, name := range names {
			out[i] = Field{Name: name, Type: types[name]}
		}
		return out
	}
	return Merged{Schema: merged, New: named(added), Locked: named(locked)}, nil
}

// ValidFieldName refuses, as a *Refusal of kind Invalid, a name no piece's
// data could carry as a top-level field: the rules a send's keys meet (1 to
// 64 bytes, no control character, no backtick, not id, received_at or
// occurred_at), in words for a person naming a field, such as a source's
// text field.
func ValidFieldName(name string) error {
	switch {
	case len(name) < 1 || len(name) > maxKeyBytes:
		return refuse(Invalid, "a field name is 1 to %d bytes; this one is %d", maxKeyBytes, len(name))
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return refuse(Invalid, "a field name has no control characters")
	case strings.ContainsRune(name, '`'):
		return refuse(Invalid, "a field name has no backticks")
	case reserved[name] != "":
		return refuse(Invalid, "%s is a column of every piece, not a field of its data", name)
	}
	return nil
}

// WithText is the schema once the field named name is the source's text
// field, without changing s: the field must hold strings, so it is locked
// as a string, and pieces that hold it as anything else are refused from
// then on. A field held as a string leaves the schema as it is; one only
// ever null is locked; one no piece has carried yet is added. It refuses a
// name ValidFieldName refuses and a field of another type, as a *Refusal of
// kind Invalid, and a new field past maxFields as one of kind
// TooManyFields, as a send's merge would. Clearing a source's text field
// ("") changes no schema and needs no WithText.
func (s Schema) WithText(name string, maxFields int) (Schema, error) {
	if err := ValidFieldName(name); err != nil {
		return Schema{}, err
	}
	held, ok := s.types[name]
	switch {
	case held == TypeString:
		return s, nil
	case ok && held != TypeNull:
		return Schema{}, refuse(Invalid, "a text field holds strings; this source holds %q as %s", name, held.phrase())
	case !ok && len(s.types) >= maxFields:
		return Schema{}, refuse(TooManyFields, "this source has %d fields and a source may have %d: choose a text field its pieces already carry", len(s.types), maxFields)
	}
	types := maps.Clone(s.types)
	if types == nil {
		types = map[string]Type{}
	}
	types[name] = TypeString
	return newSchema(types), nil
}

// jsonString encodes s as snapshot rows do: encoding/json without HTML
// escaping.
func jsonString(s string) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}
