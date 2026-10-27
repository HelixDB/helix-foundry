package filesource

// Type is a column's type, inferred from the values or read from Parquet
// metadata. It describes the values; the JSON emitted is not coerced to it
// except for CSV, whose text values have no type of their own.
type Type uint8

const (
	TypeNull Type = iota
	TypeBoolean
	TypeInteger
	TypeFloat
	TypeDecimal
	TypeString
	TypeDate
	TypeTime
	TypeTimestamp
	TypeTimestampTZ
	TypeBinary
	TypeJSON
)

var typeNames = [...]string{
	TypeNull:        "null",
	TypeBoolean:     "boolean",
	TypeInteger:     "integer",
	TypeFloat:       "float",
	TypeDecimal:     "decimal",
	TypeString:      "string",
	TypeDate:        "date",
	TypeTime:        "time",
	TypeTimestamp:   "timestamp",
	TypeTimestampTZ: "timestamptz",
	TypeBinary:      "binary",
	TypeJSON:        "json",
}

// String is the type's lower-case name, as stored with a connector table.
func (t Type) String() string {
	if int(t) < len(typeNames) {
		return typeNames[t]
	}
	return "unknown"
}

// ParseType reads back a type's stored name.
func ParseType(name string) (Type, bool) {
	for t, n := range typeNames {
		if n == name {
			return Type(t), true
		}
	}
	return TypeNull, false
}

// join is the least type holding both: a type joins itself, null joins
// anything, numbers widen to float, a date widens to a timestamp, JSON
// absorbs anything, and every other mix is a string.
func join(a, b Type) Type {
	switch {
	case a == b:
		return a
	case a == TypeNull:
		return b
	case b == TypeNull:
		return a
	case a == TypeJSON || b == TypeJSON:
		return TypeJSON
	case numeric(a) && numeric(b):
		return TypeFloat
	case a == TypeDate && b == TypeTimestamp, a == TypeTimestamp && b == TypeDate:
		return TypeTimestamp
	}
	return TypeString
}

func numeric(t Type) bool { return t == TypeInteger || t == TypeFloat || t == TypeDecimal }

// Column is one field of every row.
type Column struct {
	Name string
	Type Type
	// Nullable is set when a value is null or a row lacks the column.
	Nullable bool
}

// Merge unions column sets by name in first-seen order, joining types.
// A column missing from any set is nullable.
func Merge(sets ...[]Column) []Column {
	var out []Column
	index := map[string]int{}
	var present []int
	for _, set := range sets {
		seen := make(map[string]bool, len(set))
		for _, c := range set {
			i, ok := index[c.Name]
			if !ok {
				i = len(out)
				index[c.Name] = i
				out = append(out, Column{Name: c.Name, Type: c.Type})
				present = append(present, 0)
			}
			out[i].Type = join(out[i].Type, c.Type)
			out[i].Nullable = out[i].Nullable || c.Nullable
			if !seen[c.Name] {
				seen[c.Name] = true
				present[i]++
			}
		}
	}
	for i := range out {
		if present[i] < len(sets) {
			out[i].Nullable = true
		}
	}
	return out
}
