package filesource

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// sniffBytes is how much of a CSV the delimiter is guessed from.
const sniffBytes = 64 << 10

// sniffLines is how many lines the delimiter must count consistently.
const sniffLines = 20

var delimiters = []byte{',', ';', '\t', '|'}

// errNotUTF8 restarts a CSV scan as Windows-1252; it is never returned.
var errNotUTF8 = errors.New("filesource: not UTF-8")

// csvLayout is how one CSV file is read.
type csvLayout struct {
	charset charset
}

// csvRecords streams the records of one CSV text after its header.
type csvRecords struct {
	r      *csv.Reader
	br     *budgetReader
	base   int64
	header []string
	// maxRecord caps one line, its line ending included.
	maxRecord int64
	// record counts the records read so far, the header excluded.
	record int64
	// fields is the current record, valid until the next.
	fields []string
}

// newCSVRecords reads the delimiter and header from br.
func newCSVRecords(br *budgetReader, name string, maxRecord, maxColumns int) (*csvRecords, error) {
	prefix := make([]byte, sniffBytes)
	n, err := io.ReadFull(br, prefix)
	atEOF := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
	if err != nil && !atEOF {
		return nil, err
	}
	prefix = prefix[:n]
	delim, skip := sniffDelimiter(prefix, atEOF, isTSV(name))
	r := csv.NewReader(io.MultiReader(bytes.NewReader(prefix[skip:]), br))
	r.Comma = delim
	r.FieldsPerRecord = -1
	r.ReuseRecord = true
	r.LazyQuotes = true
	c := &csvRecords{r: r, br: br, base: int64(skip), maxRecord: int64(maxRecord)}
	header, err := c.read()
	if errors.Is(err, io.EOF) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if len(header) > maxColumns {
		return nil, because(ErrTooManyColumns, "more than %d columns", maxColumns)
	}
	c.header = headerNames(header)
	return c, nil
}

// read reads one line's fields, checking its size and moving the budget's
// record boundary past it.
func (c *csvRecords) read() ([]string, error) {
	start := c.r.InputOffset()
	rec, err := c.r.Read()
	if err != nil {
		if errors.Is(err, io.EOF) && c.br.cause() == nil {
			return nil, io.EOF
		}
		return nil, csvError(err, c.br)
	}
	end := c.r.InputOffset()
	if end-start > c.maxRecord {
		return nil, because(ErrRecordTooLarge, "a line over %s", sizeText(c.maxRecord))
	}
	c.br.mark(c.base + end)
	return rec, nil
}

// next reads the next record into fields; false is the end of the file.
func (c *csvRecords) next() (bool, error) {
	if c.header == nil {
		return false, nil
	}
	rec, err := c.read()
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	if err != nil {
		return false, recordError(err, c.record+1)
	}
	c.record++
	if len(rec) > len(c.header) {
		line, _ := c.r.FieldPos(0)
		return false, recordError(because(ErrExtraFields, "line %d has %d fields; the header has %d", line, len(rec), len(c.header)), c.record)
	}
	c.fields = rec
	return true, nil
}

// value is the record's field for header column i; a short row's missing
// fields are empty, which reads as null.
func (c *csvRecords) value(i int) string {
	if i < len(c.fields) {
		return c.fields[i]
	}
	return ""
}

func csvError(err error, br *budgetReader) error {
	if c := br.cause(); c != nil {
		return c
	}
	var parse *csv.ParseError
	if errors.As(err, &parse) {
		return &reason{kind: ErrMalformed, msg: fmt.Sprintf("malformed CSV on line %d", parse.Line), cause: err}
	}
	return &reason{kind: ErrMalformed, msg: "malformed CSV", cause: err}
}

// headerNames trims a header: an empty name becomes column_<n> (1-based)
// and a repeated one name_2, name_3.
func headerNames(raw []string) []string {
	names := make([]string, len(raw))
	taken := make(map[string]bool, len(raw))
	for i, n := range raw {
		n = strings.TrimSpace(n)
		if n == "" {
			n = fmt.Sprintf("column_%d", i+1)
		}
		if taken[n] {
			base := n
			for k := 2; taken[n]; k++ {
				n = fmt.Sprintf("%s_%d", base, k)
			}
		}
		taken[n] = true
		names[i] = n
	}
	return names
}

// sniffDelimiter picks the delimiter from a CSV's first bytes. An Excel
// "sep=X" first line names it and is skipped; a .tsv uses tabs; otherwise
// it is the candidate counted most consistently over the first lines, and
// a comma when none is.
func sniffDelimiter(prefix []byte, atEOF, tsv bool) (delim rune, skip int) {
	if d, n, ok := sepLine(prefix); ok {
		return d, n
	}
	if tsv {
		return '\t', 0
	}
	lines := splitLines(prefix, atEOF)
	best, bestScore := rune(','), 0
	for _, d := range delimiters {
		counts := map[int]int{}
		for _, l := range lines {
			if n := countOutsideQuotes(l, d); n > 0 {
				counts[n]++
			}
		}
		score := 0
		for _, c := range counts {
			score = max(score, c)
		}
		if score > bestScore {
			best, bestScore = rune(d), score
		}
	}
	return best, 0
}

// sepLine reads an Excel "sep=X" first line.
func sepLine(prefix []byte) (rune, int, bool) {
	rest, ok := bytes.CutPrefix(prefix, []byte("sep="))
	if !ok {
		return 0, 0, false
	}
	d, size := utf8.DecodeRune(rest)
	if d == utf8.RuneError || d == '"' || d == '\r' || d == '\n' {
		return 0, 0, false
	}
	end := 4 + size
	switch {
	case bytes.HasPrefix(prefix[end:], []byte("\r\n")):
		end += 2
	case bytes.HasPrefix(prefix[end:], []byte("\n")):
		end++
	case end != len(prefix):
		return 0, 0, false
	}
	return d, end, true
}

// splitLines cuts the first sniffLines lines, keeping quoted newlines
// inside their line. An unfinished last line counts only at the end of
// the file.
func splitLines(b []byte, atEOF bool) [][]byte {
	var lines [][]byte
	quoted, start := false, 0
	for i, c := range b {
		switch {
		case c == '"':
			quoted = !quoted
		case c == '\n' && !quoted:
			lines = append(lines, b[start:i])
			start = i + 1
			if len(lines) == sniffLines {
				return lines
			}
		}
	}
	if atEOF && start < len(b) {
		lines = append(lines, b[start:])
	}
	return lines
}

func countOutsideQuotes(line []byte, d byte) int {
	n, quoted := 0, false
	for _, c := range line {
		switch {
		case c == '"':
			quoted = !quoted
		case c == d && !quoted:
			n++
		}
	}
	return n
}

// csvType is the type a CSV text value reads as. Leading zeros and
// integers past int64 keep the text a string, so nothing is lost.
func csvType(v string) Type {
	switch {
	case strings.EqualFold(v, "true"), strings.EqualFold(v, "false"):
		return TypeBoolean
	case isIntText(v):
		if _, err := strconv.ParseInt(v, 10, 64); err == nil {
			return TypeInteger
		}
	case isFloatText(v):
		if f, err := strconv.ParseFloat(v, 64); err == nil && !math.IsInf(f, 0) {
			return TypeFloat
		}
	}
	return TypeString
}

// isIntText matches -?(0|[1-9][0-9]*).
func isIntText(v string) bool { return isNatural(strings.TrimPrefix(v, "-")) }

// isNatural matches 0|[1-9][0-9]*.
func isNatural(v string) bool { return v != "" && (v[0] != '0' || len(v) == 1) && allDigits(v) }

// isFloatText matches -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?.
func isFloatText(v string) bool {
	v = strings.TrimPrefix(v, "-")
	mant, exp, hasExp := v, "", false
	if i := strings.IndexAny(v, "eE"); i >= 0 {
		mant, exp, hasExp = v[:i], v[i+1:], true
	}
	whole, frac, hasFrac := strings.Cut(mant, ".")
	if !isNatural(whole) || hasFrac && (frac == "" || !allDigits(frac)) {
		return false
	}
	if hasExp {
		if exp != "" && (exp[0] == '+' || exp[0] == '-') {
			exp = exp[1:]
		}
		return exp != "" && allDigits(exp)
	}
	return true
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// appendCSVValue writes a CSV text value as its column's final type: an
// empty field is null, booleans and numbers are JSON literals, anything
// else a string.
func appendCSVValue(dst []byte, v string, t Type) []byte {
	switch {
	case v == "":
		return append(dst, "null"...)
	case t == TypeBoolean && strings.EqualFold(v, "true"):
		return append(dst, "true"...)
	case t == TypeBoolean && strings.EqualFold(v, "false"):
		return append(dst, "false"...)
	case t == TypeInteger || t == TypeFloat:
		if k := csvType(v); k == TypeInteger || k == TypeFloat {
			return append(dst, v...)
		}
	}
	return appendString(dst, v)
}
