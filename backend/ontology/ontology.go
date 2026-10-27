package ontology

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// MaxBytes caps one definition; anything larger is not something a person or
// the generation job should be writing.
const MaxBytes = 1 << 20

// maxErrors bounds the messages one validation reports.
const maxErrors = 20

// Definition is a workspace ontology: entities backed by synced tables, the
// relationships between them, and rules and constraints on them.
type Definition struct {
	Entities      []Entity       `json:"entities"`
	Relationships []Relationship `json:"relationships"`
	Rules         []Rule         `json:"rules"`
	Constraints   []Constraint   `json:"constraints"`
}

type Entity struct {
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Source      Source     `json:"source"`
	PrimaryKey  string     `json:"primary_key"`
	Properties  []Property `json:"properties"`
}

// Source is the synced table an entity reads.
type Source struct {
	ConnectorID string `json:"connector_id"`
	Table       string `json:"table"`
}

type Property struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

type Relationship struct {
	Name         string `json:"name"`
	From         string `json:"from"`
	To           string `json:"to"`
	FromProperty string `json:"from_property"`
	ToProperty   string `json:"to_property"`
	Cardinality  string `json:"cardinality"`
}

type Rule struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Entity is optional: a rule can span the whole ontology.
	Entity     string `json:"entity,omitempty"`
	Expression string `json:"expression"`
}

type Constraint struct {
	Name       string   `json:"name"`
	Entity     string   `json:"entity"`
	Kind       string   `json:"kind"`
	Properties []string `json:"properties"`
	Expression string   `json:"expression,omitempty"`
}

// Cardinalities and ConstraintKinds are the values a definition may use.
var (
	Cardinalities   = []string{"one_to_one", "one_to_many", "many_to_one", "many_to_many"}
	ConstraintKinds = []string{"unique", "not_null", "reference", "check"}
)

// Table is a synced table a definition may reference. Columns may be empty
// when the table had no rows, and then any column name is accepted.
type Table struct {
	ConnectorID string
	Name        string
	Columns     []string
}

// InvalidError lists what is wrong with a definition, in terms a person can fix.
type InvalidError struct{ Problems []string }

func (e *InvalidError) Error() string { return strings.Join(e.Problems, "; ") }

// Parse decodes a definition, refusing unknown fields and oversized input.
func Parse(raw []byte) (Definition, error) {
	var d Definition
	if len(raw) > MaxBytes {
		return d, &InvalidError{Problems: []string{fmt.Sprintf("the definition is larger than %d bytes", MaxBytes)}}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return d, &InvalidError{Problems: []string{"the definition is not valid: " + err.Error()}}
	}
	return d, nil
}

// Validate checks every reference: entities name synced tables, keys and
// properties name their columns, and everything else names entities.
func Validate(d Definition, tables []Table) error {
	v := &validator{}
	columns := map[Source][]string{}
	for _, t := range tables {
		columns[Source{ConnectorID: t.ConnectorID, Table: t.Name}] = t.Columns
	}
	if len(d.Entities) == 0 {
		v.add("define at least one entity")
	}
	entities := map[string]Entity{}
	for i, e := range d.Entities {
		where := fmt.Sprintf("entity %d", i+1)
		if e.Name != "" {
			where = fmt.Sprintf("entity %q", e.Name)
		}
		switch {
		case strings.TrimSpace(e.Name) == "":
			v.add("%s has no name", where)
		case entities[e.Name].Name != "":
			v.add("entity %q is defined twice", e.Name)
		default:
			entities[e.Name] = e
		}
		cols, synced := columns[e.Source]
		if !synced {
			v.add("%s reads %s, which is not a synced table of this workspace", where, describe(e.Source))
			continue
		}
		hasColumn := func(name string) bool { return len(cols) == 0 || slices.Contains(cols, name) }
		if e.PrimaryKey == "" || !hasColumn(e.PrimaryKey) {
			v.add("%s primary key %q is not a column of %s", where, e.PrimaryKey, e.Source.Table)
		}
		seen := map[string]bool{}
		for _, p := range e.Properties {
			switch {
			case seen[p.Name]:
				v.add("%s lists property %q twice", where, p.Name)
			case !hasColumn(p.Name):
				v.add("%s property %q is not a column of %s", where, p.Name, e.Source.Table)
			}
			seen[p.Name] = true
		}
	}
	for i, r := range d.Relationships {
		where := fmt.Sprintf("relationship %d", i+1)
		if r.Name != "" {
			where = fmt.Sprintf("relationship %q", r.Name)
		}
		if r.Name == "" {
			v.add("%s has no name", where)
		}
		v.field(entities, where, "from", r.From, r.FromProperty)
		v.field(entities, where, "to", r.To, r.ToProperty)
		if !slices.Contains(Cardinalities, r.Cardinality) {
			v.add("%s cardinality %q is not one of %s", where, r.Cardinality, strings.Join(Cardinalities, ", "))
		}
	}
	for i, r := range d.Rules {
		where := fmt.Sprintf("rule %d", i+1)
		if r.Name != "" {
			where = fmt.Sprintf("rule %q", r.Name)
		}
		if r.Name == "" || strings.TrimSpace(r.Expression) == "" {
			v.add("%s needs a name and an expression", where)
		}
		if r.Entity != "" && entities[r.Entity].Name == "" {
			v.add("%s applies to entity %q, which is not defined", where, r.Entity)
		}
	}
	for i, c := range d.Constraints {
		where := fmt.Sprintf("constraint %d", i+1)
		if c.Name != "" {
			where = fmt.Sprintf("constraint %q", c.Name)
		}
		if c.Name == "" {
			v.add("%s has no name", where)
		}
		if !slices.Contains(ConstraintKinds, c.Kind) {
			v.add("%s kind %q is not one of %s", where, c.Kind, strings.Join(ConstraintKinds, ", "))
		}
		if c.Kind == "check" && strings.TrimSpace(c.Expression) == "" {
			v.add("%s is a check and needs an expression", where)
		}
		if c.Kind != "check" && len(c.Properties) == 0 {
			v.add("%s needs at least one property", where)
		}
		for _, p := range c.Properties {
			v.field(entities, where, "entity", c.Entity, p)
		}
		if len(c.Properties) == 0 && entities[c.Entity].Name == "" {
			v.add("%s applies to entity %q, which is not defined", where, c.Entity)
		}
	}
	return v.err()
}

type validator struct{ problems []string }

func (v *validator) add(format string, args ...any) {
	if len(v.problems) < maxErrors {
		v.problems = append(v.problems, fmt.Sprintf(format, args...))
	}
}

// field checks that entity exists and has property as its key or a property.
func (v *validator) field(entities map[string]Entity, where, role, entity, property string) {
	e, ok := entities[entity]
	if !ok {
		v.add("%s %s %q is not a defined entity", where, role, entity)
		return
	}
	if property == e.PrimaryKey || slices.ContainsFunc(e.Properties, func(p Property) bool { return p.Name == property }) {
		return
	}
	v.add("%s uses %q, which is not a property of %q", where, property, entity)
}

func (v *validator) err() error {
	if len(v.problems) == 0 {
		return nil
	}
	return &InvalidError{Problems: v.problems}
}

func describe(s Source) string {
	if s.ConnectorID == "" || s.Table == "" {
		return "no table"
	}
	return fmt.Sprintf("%s of connector %s", s.Table, s.ConnectorID)
}

// IsInvalid reports a definition problem the author must fix.
func IsInvalid(err error) bool {
	var invalidErr *InvalidError
	return errors.As(err, &invalidErr)
}
