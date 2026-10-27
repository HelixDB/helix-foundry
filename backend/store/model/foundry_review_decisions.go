package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	"golang.org/x/text/unicode/norm"
)

// FoundryReviewDecisionKind selects one of the two decision shapes.
type FoundryReviewDecisionKind string

const (
	FoundryReviewResolve FoundryReviewDecisionKind = "resolve"
	FoundryReviewProp    FoundryReviewDecisionKind = "prop"
)

// FoundryReviewDecision is an ontology-wide override, applied from the next
// load. Resolve identifies a document name; a nil EntityKey means not in data.
// Prop identifies an entity property and carries a JSON scalar (including null).
// Validate guards this sum type at every storage boundary.
type FoundryReviewDecision struct {
	ID              string
	OntologyID      string
	Kind            FoundryReviewDecisionKind
	Type            string
	DocID           *string
	NormName        *string
	EntityKey       *string
	Prop            *string
	Value           json.RawMessage
	DecidedByUserID string
	DecidedAt       time.Time
}

func (d FoundryReviewDecision) Validate() error {
	nonempty := func(s *string) bool { return s != nil && *s != "" }
	if d.ID == "" || d.OntologyID == "" || d.Type == "" || d.DecidedByUserID == "" {
		return fmt.Errorf("decision identity and actor are required")
	}
	switch d.Kind {
	case FoundryReviewResolve:
		if !nonempty(d.DocID) || !nonempty(d.NormName) || d.Prop != nil || len(d.Value) != 0 || (d.EntityKey != nil && !nonempty(d.EntityKey)) {
			return fmt.Errorf("invalid resolve decision")
		}
	case FoundryReviewProp:
		if d.DocID != nil || d.NormName != nil || !nonempty(d.EntityKey) || !nonempty(d.Prop) || !json.Valid(d.Value) {
			return fmt.Errorf("invalid prop decision")
		}
		var v any
		if err := json.Unmarshal(d.Value, &v); err != nil {
			return err
		}
		switch v.(type) {
		case []any, map[string]any:
			return fmt.Errorf("property value must be a scalar")
		}
	default:
		return fmt.Errorf("unknown decision kind %q", d.Kind)
	}
	return nil
}

// FoundryPropType is a property's declared type in an ontology's TOML
// (schema::PropType): what a prop decision's value must parse as.
type FoundryPropType string

const (
	FoundryPropString   FoundryPropType = "string"
	FoundryPropInteger  FoundryPropType = "integer"
	FoundryPropFloat    FoundryPropType = "float"
	FoundryPropBoolean  FoundryPropType = "boolean"
	FoundryPropDate     FoundryPropType = "date"
	FoundryPropDateTime FoundryPropType = "datetime"
)

// FoundryDecisionType is what a decision is checked against of one type of
// an ontology: its props' types, and its key props, none for a named type.
type FoundryDecisionType struct {
	Props map[string]FoundryPropType
	Key   []string
}

// FoundryDecisionTypes reads the type/property identities of an ontology TOML.
// Missing types means an empty ontology. Invalid TOML fails closed.
func FoundryDecisionTypes(ontologyTOML string) (map[string]FoundryDecisionType, error) {
	var schema struct {
		Types map[string]FoundryDecisionType
	}
	_, err := toml.Decode(ontologyTOML, &schema)
	return schema.Types, err
}

// Fits reports whether the ontology of types still takes d as the engine
// checks an Extract's decisions (decision::Decisions::validate), which
// refuses the whole request over one that doesn't: d's type is there, its
// entity key names an entity of that type (Identifies), a resolve's
// normalized name is one, and a prop decision's prop is there and its value
// parses as the prop's type. A version that changes a prop's type, or a
// type's key, leaves decisions that no longer fit.
func (d FoundryReviewDecision) Fits(types map[string]FoundryDecisionType) bool {
	ty, ok := types[d.Type]
	if !ok || d.Validate() != nil || d.EntityKey != nil && !ty.Identifies(*d.EntityKey) {
		return false
	}
	if d.Kind == FoundryReviewResolve {
		return strings.TrimSpace(*d.DocID) != "" && FoundryNormName(*d.NormName) == *d.NormName
	}
	kind, ok := ty.Props[*d.Prop]
	if !ok {
		return false
	}
	_, err := kind.DecisionValue(d.Value)
	return err == nil
}

// Extractable reports whether an Extract of a version of types takes d, as
// the engine reads its decisions (decision::Decisions::fit): every decision
// that Fits, and a resolve of a type still there, with its document and
// normalized name, whose entity_key the type's identity no longer reads (a
// key changed since it was settled). The engine keeps that resolve
// (decision::Choice::Stale), so its name is reviewed again rather than
// matched another way, and counts it in stale_decisions.
func (d FoundryReviewDecision) Extractable(types map[string]FoundryDecisionType) bool {
	if d.Kind != FoundryReviewResolve {
		return d.Fits(types)
	}
	_, ok := types[d.Type]
	return ok && d.Validate() == nil && strings.TrimSpace(*d.DocID) != "" && FoundryNormName(*d.NormName) == *d.NormName
}

// Identifies reports whether key names an entity of t as a decision's
// entity_key does (decision::entity): for a named type its normalized name
// (FoundryNormName of itself), for a keyed one the canonical encoding
// (schema::Key) of one value per key prop, each tagged with its prop's type.
func (t FoundryDecisionType) Identifies(key string) bool {
	if len(t.Key) == 0 {
		return key != "" && FoundryNormName(key) == key
	}
	values, ok := decodeFoundryKey(key)
	if !ok || len(values) != len(t.Key) {
		return false
	}
	tags := map[FoundryPropType]rune{FoundryPropString: 's', FoundryPropInteger: 'i', FoundryPropDate: 'd', FoundryPropDateTime: 't'}
	for i, v := range values {
		tag, keyable := tags[t.Props[t.Key[i]]]
		if !keyable || tag != v.tag {
			return false
		}
	}
	return true
}

// foundryKeyValue is one value of a key's encoding: its type's tag and its
// text, unescaped.
type foundryKeyValue struct {
	tag  rune
	body string
}

// decodeFoundryKey reads a key's encoding as schema::Key::parse does: values
// separated by '|', each tagged s(tring), i(nteger), d(ate) or t (datetime);
// a string escapes '\', '|' and control characters (\u{hex}). It accepts
// only an encoding the engine makes: canonical for every value's type, so
// encoding the values again gives the key back.
func decodeFoundryKey(encoded string) ([]foundryKeyValue, bool) {
	var values []foundryKeyValue
	runes := []rune(encoded)
	for i := 0; ; {
		if i >= len(runes) {
			return nil, false
		}
		v := foundryKeyValue{tag: runes[i]}
		i++
		var body strings.Builder
		ended := false
		for i < len(runes) && !ended {
			c := runes[i]
			i++
			switch {
			case c == '|':
				ended = true
			case c == '\\' && v.tag == 's':
				if i >= len(runes) {
					return nil, false
				}
				e := runes[i]
				i++
				switch e {
				case '\\', '|':
					body.WriteRune(e)
				case 'u':
					end := slices.Index(runes[i:], '}')
					if end < 0 || end == 0 || runes[i] != '{' {
						return nil, false
					}
					code, err := strconv.ParseUint(string(runes[i+1:i+end]), 16, 32)
					if err != nil || !utf8.ValidRune(rune(code)) {
						return nil, false
					}
					body.WriteRune(rune(code))
					i += end + 1
				default:
					return nil, false
				}
			case unicode.IsControl(c):
				return nil, false
			default:
				body.WriteRune(c)
			}
		}
		v.body = body.String()
		values = append(values, v)
		if !ended {
			break
		}
	}
	var again strings.Builder
	for i, v := range values {
		if i > 0 {
			again.WriteRune('|')
		}
		again.WriteRune(v.tag)
		switch v.tag {
		case 's':
			if v.body == "" {
				return nil, false
			}
			for _, c := range v.body {
				switch {
				case c == '\\':
					again.WriteString(`\\`)
				case c == '|':
					again.WriteString(`\|`)
				case unicode.IsControl(c):
					fmt.Fprintf(&again, `\u{%x}`, c)
				default:
					again.WriteRune(c)
				}
			}
		case 'i':
			n, err := strconv.ParseInt(v.body, 10, 64)
			if err != nil {
				return nil, false
			}
			again.WriteString(strconv.FormatInt(n, 10))
		case 'd':
			if !foundryISODate(v.body) {
				return nil, false
			}
			again.WriteString(v.body)
		case 't':
			canonical, ok := foundryDateTime(v.body)
			if !ok {
				return nil, false
			}
			again.WriteString(canonical)
		default:
			return nil, false
		}
	}
	return values, again.String() == encoded
}

// DecisionValue reads raw, a decision's JSON value, as the engine does
// (decision::Value::parse, then schema::PropType::parse): null removes the
// prop, and anything else must be of the JSON kind ty takes, with no
// coercion: a string for string, date and datetime, true or false for
// boolean, an integer that fits 64 bits for integer and a finite number for
// float. Strings are trimmed and must not be empty; a date is YYYY, YYYY-MM
// or YYYY-MM-DD with a real month and day; a datetime is RFC 3339 whose UTC
// year has four digits. It returns the value as the engine keeps it: a
// trimmed string, or a datetime in UTC with its fraction in groups of
// three; an error is a value the engine refuses.
func (ty FoundryPropType) DecisionValue(raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil || decoder.More() {
		return nil, fmt.Errorf("decision value %s is not one JSON value", raw)
	}
	refused := fmt.Errorf("decision value %s is not a %s", raw, ty)
	var parsed any
	switch v := value.(type) {
	case nil:
		return json.RawMessage("null"), nil
	case string:
		text := strings.TrimSpace(v)
		switch ty {
		case FoundryPropString:
			if text == "" {
				return nil, refused
			}
			parsed = text
		case FoundryPropDate:
			if !foundryISODate(text) {
				return nil, refused
			}
			parsed = text
		case FoundryPropDateTime:
			canonical, ok := foundryDateTime(text)
			if !ok {
				return nil, refused
			}
			parsed = canonical
		default:
			return nil, refused
		}
	case bool:
		if ty != FoundryPropBoolean {
			return nil, refused
		}
		parsed = v
	case json.Number:
		switch ty {
		case FoundryPropInteger:
			n, err := strconv.ParseInt(v.String(), 10, 64)
			if err != nil {
				return nil, refused
			}
			parsed = n
		case FoundryPropFloat:
			f, err := strconv.ParseFloat(v.String(), 64)
			if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
				return nil, refused
			}
			parsed = f
		default:
			return nil, refused
		}
	default:
		return nil, refused
	}
	return json.Marshal(parsed)
}

// foundryISODate is schema's is_iso_date: YYYY, YYYY-MM or YYYY-MM-DD with a
// real month and a real day for that month.
func foundryISODate(value string) bool {
	parts := strings.Split(value, "-")
	number := func(part string, digits int) (int, bool) {
		if len(part) != digits || strings.Trim(part, "0123456789") != "" {
			return 0, false
		}
		n, err := strconv.Atoi(part)
		return n, err == nil
	}
	year, ok := number(parts[0], 4)
	if !ok || len(parts) > 3 {
		return false
	}
	if len(parts) == 1 {
		return true
	}
	month, ok := number(parts[1], 2)
	if !ok || month < 1 || month > 12 {
		return false
	}
	if len(parts) == 2 {
		return true
	}
	day, ok := number(parts[2], 2)
	// The day after the month's last rolls into the next month.
	return ok && day >= 1 && time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC).Day() == day
}

// foundryDateTime is an RFC 3339 instant as schema keeps a datetime: in UTC,
// written with Z and as many fractional digits as it needs in groups of
// three; false when it isn't one, or its UTC year doesn't have four digits.
func foundryDateTime(value string) (string, bool) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "", false
	}
	utc := t.UTC()
	if utc.Year() < 0 || utc.Year() > 9999 {
		return "", false
	}
	fraction := ""
	switch ns := utc.Nanosecond(); {
	case ns == 0:
	case ns%1_000_000 == 0:
		fraction = fmt.Sprintf(".%03d", ns/1_000_000)
	case ns%1_000 == 0:
		fraction = fmt.Sprintf(".%06d", ns/1_000)
	default:
		fraction = fmt.Sprintf(".%09d", ns)
	}
	return utc.Format("2006-01-02T15:04:05") + fraction + "Z", true
}

// FoundryNormName is the name the engine matches entities by and keys a
// named type's entities with (text::norm_name): NFKC, lowercased (İ as I,
// final sigma as sigma), NFC, then every run of separators one space,
// trimmed. Letters, digits, combining marks and + # & @ $ are kept, and a
// '.' that starts a word (".NET") or joins a run of two or more to a letter
// or digit ("Node.js"). Folding a name twice gives what folding it once did.
func FoundryNormName(name string) string {
	folded := []rune(norm.NFC.String(strings.Map(func(r rune) rune {
		if r == '\u0130' {
			r = 'I'
		}
		if r = unicode.ToLower(r); r == 'ς' {
			r = 'σ'
		}
		return r
	}, norm.NFKC.String(name))))
	alphanumeric := func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Other_Alphabetic, r)
	}
	var out []rune
	for i, c := range folded {
		joinsDot := func() bool {
			wordStart := len(out) == 0 || out[len(out)-1] == ' '
			run := 0
			for j := len(out) - 1; j >= 0 && alphanumeric(out[j]); j-- {
				run++
			}
			return i+1 < len(folded) && alphanumeric(folded[i+1]) && (wordStart || run >= 2)
		}
		switch {
		case alphanumeric(c) || unicode.Is(unicode.M, c) || strings.ContainsRune("+#&@$", c) || c == '.' && joinsDot():
			out = append(out, c)
		case len(out) > 0 && out[len(out)-1] != ' ':
			out = append(out, ' ')
		}
	}
	return strings.TrimSuffix(string(out), " ")
}
