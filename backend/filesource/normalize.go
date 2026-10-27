package filesource

import (
	"math"
	"strconv"
	"unicode/utf8"
)

// maxExactDigits is the longest integer literal that cannot overflow a
// float64 (whose largest value is about 1.8e308).
const maxExactDigits = 308

// normalize appends raw, one JSON value, as the engine can read it:
// whitespace dropped, invalid UTF-8 and lone surrogate escapes replaced by
// U+FFFD, and numbers beyond float64's range written as strings. Big
// integers stay exact. depth is the nesting raw sits at (a record's field is
// at 1); a container past maxDepth is ErrTooDeep. raw must be valid JSON, as
// a decoder returns it; a malformed token is ErrMalformed.
func normalize(dst, raw []byte, depth, maxDepth int) ([]byte, error) {
	d := depth
	for i := 0; i < len(raw); {
		switch c := raw[i]; c {
		case ' ', '\t', '\n', '\r':
			i++
		case '{', '[':
			if d++; d > maxDepth {
				return dst, because(ErrTooDeep, "nested more than %d levels deep", maxDepth)
			}
			dst = append(dst, c)
			i++
		case '}', ']':
			if d--; d < depth {
				return dst, errBadJSON
			}
			dst = append(dst, c)
			i++
		case ',', ':':
			dst = append(dst, c)
			i++
		case '"':
			end, ok := stringEnd(raw, i)
			if !ok {
				return dst, errBadJSON
			}
			dst = appendStringToken(dst, raw[i:end])
			i = end
		case 't', 'f', 'n':
			lit := literal(raw[i:])
			if lit == "" {
				return dst, errBadJSON
			}
			dst = append(dst, lit...)
			i += len(lit)
		default:
			end := numberEnd(raw, i)
			if end == i {
				return dst, errBadJSON
			}
			dst = appendNumberToken(dst, raw[i:end])
			i = end
		}
	}
	if d != depth {
		return dst, errBadJSON
	}
	return dst, nil
}

var errBadJSON = because(ErrMalformed, "malformed JSON")

func literal(b []byte) string {
	for _, l := range []string{"true", "false", "null"} {
		if len(b) >= len(l) && string(b[:len(l)]) == l {
			return l
		}
	}
	return ""
}

// stringEnd is the index after the closing quote of the string at raw[i].
func stringEnd(raw []byte, i int) (int, bool) {
	for j := i + 1; j < len(raw); j++ {
		switch raw[j] {
		case '\\':
			j++
		case '"':
			return j + 1, true
		}
	}
	return 0, false
}

// numberEnd is the index after the number starting at raw[i], or i.
func numberEnd(raw []byte, i int) int {
	j := i
	for j < len(raw) {
		switch c := raw[j]; {
		case c >= '0' && c <= '9', c == '-', c == '+', c == '.', c == 'e', c == 'E':
			j++
		default:
			return j
		}
	}
	return j
}

// appendNumberToken writes a number literal, or a string holding it when
// float64 cannot: a reader would fail on the infinity.
func appendNumberToken(dst, num []byte) []byte {
	exact := len(num) <= maxExactDigits
	for _, c := range num {
		if c == '.' || c == 'e' || c == 'E' {
			exact = false
			break
		}
	}
	if !exact {
		if f, err := strconv.ParseFloat(string(num), 64); err != nil && math.IsInf(f, 0) {
			return appendString(dst, string(num))
		}
	}
	return append(dst, num...)
}

// appendStringToken copies a JSON string token (quotes included), replacing
// invalid UTF-8 and unpaired surrogate escapes, which strict readers
// reject, with U+FFFD.
func appendStringToken(dst, tok []byte) []byte {
	dst = append(dst, '"')
	body := tok[1 : len(tok)-1]
	for i := 0; i < len(body); {
		c := body[i]
		switch {
		case c == '\\' && i+1 < len(body) && body[i+1] == 'u':
			r, n := unicodeEscape(body[i:])
			switch {
			case n == 0:
				dst = append(dst, body[i:i+2]...)
				i += 2
			case r == utf8.RuneError:
				dst = utf8.AppendRune(dst, utf8.RuneError)
				i += n
			default:
				dst = append(dst, body[i:i+n]...)
				i += n
			}
		case c == '\\' && i+1 < len(body):
			dst = append(dst, c, body[i+1])
			i += 2
		case c < 0x20:
			dst = append(dst, `\u00`...)
			dst = append(dst, hexDigits[c>>4], hexDigits[c&0xf])
			i++
		case c < utf8.RuneSelf:
			dst = append(dst, c)
			i++
		default:
			r, n := utf8.DecodeRune(body[i:])
			if r == utf8.RuneError && n == 1 {
				dst = utf8.AppendRune(dst, utf8.RuneError)
			} else {
				dst = append(dst, body[i:i+n]...)
			}
			i += n
		}
	}
	return append(dst, '"')
}

const hexDigits = "0123456789abcdef"

// unicodeEscape reads a \uXXXX escape, or a surrogate pair of them. n is the
// bytes consumed (0 for a malformed escape); r is RuneError for a lone
// surrogate.
func unicodeEscape(b []byte) (r rune, n int) {
	hi, ok := hex4(b)
	if !ok {
		return 0, 0
	}
	switch {
	case hi < 0xd800 || hi > 0xdfff:
		return hi, 6
	case hi >= 0xdc00:
		return utf8.RuneError, 6
	}
	if len(b) >= 12 && b[6] == '\\' && b[7] == 'u' {
		if lo, ok := hex4(b[6:]); ok && lo >= 0xdc00 && lo <= 0xdfff {
			return 0x10000 + (hi-0xd800)<<10 + (lo - 0xdc00), 12
		}
	}
	return utf8.RuneError, 6
}

func hex4(b []byte) (rune, bool) {
	if len(b) < 6 {
		return 0, false
	}
	var r rune
	for _, c := range b[2:6] {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			r |= rune(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return r, true
}

// appendString writes s as a JSON string without HTML escaping; invalid
// UTF-8 becomes U+FFFD.
func appendString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' && c < utf8.RuneSelf {
			i++
			continue
		}
		if c < utf8.RuneSelf {
			dst = append(dst, s[start:i]...)
			switch c {
			case '"', '\\':
				dst = append(dst, '\\', c)
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, `\u00`...)
				dst = append(dst, hexDigits[c>>4], hexDigits[c&0xf])
			}
			i++
			start = i
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && n == 1 {
			dst = append(dst, s[start:i]...)
			dst = utf8.AppendRune(dst, utf8.RuneError)
			i++
			start = i
			continue
		}
		i += n
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

// appendFloat writes f as encoding/json does: the shortest exact form, in
// exponent notation only for very large or small magnitudes.
func appendFloat(dst []byte, f float64, bits int) []byte {
	abs := math.Abs(f)
	format := byte('f')
	if abs != 0 && (bits == 64 && (abs < 1e-6 || abs >= 1e21) || bits == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21)) {
		format = 'e'
	}
	dst = strconv.AppendFloat(dst, f, format, -1, bits)
	if format == 'e' {
		// e-09 becomes e-9, as encoding/json writes it.
		if n := len(dst); n >= 4 && dst[n-4] == 'e' && dst[n-3] == '-' && dst[n-2] == '0' {
			dst[n-2] = dst[n-1]
			dst = dst[:n-1]
		}
	}
	return dst
}
