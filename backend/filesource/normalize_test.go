package filesource

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		` { "a" : [ 1 , 2.5 , -3e2 ] , "b" : null }`: `{"a":[1,2.5,-3e2],"b":null}`,
		`18446744073709551616`:                       `18446744073709551616`,
		strings.Repeat("9", 309):                     `"` + strings.Repeat("9", 309) + `"`,
		`1e400`:                                      `"1e400"`,
		`-1.7976931348623159e308`:                    `"-1.7976931348623159e308"`,
		`1e-400`:                                     `1e-400`,
		esc(`"~ud83d~ude00 ~ud83d x ~udc00 ~u0041 \"\\"`): esc(`"~ud83d~ude00 ` + replacement + ` x ` + replacement + ` ~u0041 \"\\"`),
		"\"\xff\xc3\x28ok\"":                              `"` + replacement + replacement + `(ok"`,
		"\"é\"":                                           "\"é\"",
		`[true,false,null,{}]`:                            `[true,false,null,{}]`,
	} {
		out, err := normalize(nil, []byte(in), 1, 64)
		require.NoError(t, err, in)
		require.Equal(t, want, string(out), in)
		require.True(t, json.Valid(out), in)
	}
	for _, in := range []string{`{"a":1`, `]`, `"abc`, `tru`, `{"a":@}`} {
		_, err := normalize(nil, []byte(in), 1, 64)
		require.ErrorIs(t, err, ErrMalformed, in)
	}
	_, err := normalize(nil, []byte(`[[1]]`), 1, 2)
	require.ErrorIs(t, err, ErrTooDeep)
	out, err := normalize(nil, []byte(`[[1]]`), 1, 3)
	require.NoError(t, err)
	require.Equal(t, `[[1]]`, string(out))
	// Control characters and malformed escapes cannot come from a decoder,
	// but still produce valid output.
	out, err = normalize(nil, []byte("\"a\x01\\uZZZZ\""), 1, 3)
	require.NoError(t, err)
	require.Equal(t, `"a\u0001\uZZZZ"`, string(out))
}

func TestAppendString(t *testing.T) {
	require.Equal(t, `"a\"b\\c\n\r\t\u0001<>&é`+replacement+`"`, string(appendString(nil, "a\"b\\c\n\r\t\x01<>&é\xff")))
	require.Equal(t, `""`, string(appendString(nil, "")))
}

func TestAppendFloat(t *testing.T) {
	for _, tc := range []struct {
		f    float64
		bits int
		want string
	}{
		{1.5, 64, "1.5"}, {0, 64, "0"}, {1e21, 64, "1e+21"}, {1e-7, 64, "1e-7"}, {123456789, 64, "123456789"},
		{float64(float32(0.1)), 32, "0.1"}, {float64(float32(1e22)), 32, "1e+22"}, {float64(float32(1e-7)), 32, "1e-7"},
		{math.Copysign(0, -1), 64, "-0"}, {2.5e-10, 64, "2.5e-10"},
	} {
		require.Equal(t, tc.want, string(appendFloat(nil, tc.f, tc.bits)))
	}
}

// FuzzNormalize checks normalize against encoding/json: valid JSON comes out
// valid, on one line, within the depth limit, and equal to json.Compact
// unless it held an overflowing number, invalid UTF-8 or a lone surrogate.
func FuzzNormalize(f *testing.F) {
	for _, seed := range []string{`{"a":[1,2,{"b":null}]}`, `1e400`, `"\ud800"`, "\"\xff\"", ` [ ] `, esc(`{"a":"~u00e9"}`), `-0.5e-3`, `[[[[[]]]]]`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		out, err := normalize(nil, in, 1, 5)
		if !json.Valid(in) {
			return
		}
		if err != nil {
			require.ErrorIs(t, err, ErrTooDeep, "%q", in)
			return
		}
		require.True(t, json.Valid(out), "%q -> %q", in, out)
		require.NotContains(t, string(out), "\n")
		require.True(t, utf8.Valid(out))
		require.LessOrEqual(t, maxNesting(out), 4)
		var compact bytes.Buffer
		require.NoError(t, json.Compact(&compact, in))
		if utf8.Valid(in) && !bytes.Contains(in, []byte(`\u`)) && !bytes.ContainsAny(in, "eE") && len(in) < maxExactDigits {
			require.Equal(t, compact.String(), string(out))
		}
	})
}

func maxNesting(b []byte) int {
	depth, deepest, quoted := 0, 0, false
	for i := 0; i < len(b); i++ {
		switch c := b[i]; {
		case quoted && c == '\\':
			i++
		case c == '"':
			quoted = !quoted
		case !quoted && (c == '{' || c == '['):
			depth++
			deepest = max(deepest, depth)
		case !quoted && (c == '}' || c == ']'):
			depth--
		}
	}
	return deepest
}
