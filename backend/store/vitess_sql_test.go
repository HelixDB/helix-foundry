package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forUpdateOf is a locking clause naming its tables, which MySQL takes and
// Vitess (PlanetScale, where hosted Helix runs this store) cannot parse.
var forUpdateOf = regexp.MustCompile(`(?is)\bFOR\s+(UPDATE|SHARE)\s+OF\b`)

// No SQL of the store's names the tables it locks: a locking read locks its
// outer table alone, and reads any other in a subquery.
func TestNoForUpdateOf(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	more, err := filepath.Glob(filepath.Join("migrations", "*.go"))
	require.NoError(t, err)
	checked := 0
	for _, path := range append(files, more...) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			require.NoError(t, err)
			assert.False(t, forUpdateOf.MatchString(text), "%s: Vitess cannot parse FOR UPDATE OF: %s", path, text)
			return true
		})
		checked++
	}
	require.Greater(t, checked, 10, "the store's files are read")
	assert.True(t, forUpdateOf.MatchString("SELECT 1 FROM a JOIN b\nFOR UPDATE OF a"), "the check sees a clause across lines")
}
