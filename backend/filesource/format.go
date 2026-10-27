// Package filesource turns data files into rows of JSON objects.
//
// It reads Parquet, JSON (an array, records under one key, or one object),
// JSON Lines and CSV, plain or compressed with gzip, zstd or bzip2. It knows
// nothing about where files live: callers list objects, run [Layout] to group
// them into tables, download a table's files and hand them to [Scan], which
// computes the exact schema (pass 1). [Schema.Rows] then emits every row with
// every column (pass 2). [Sample] and [ParquetFooter] read bounded prefixes
// for a preview.
//
// Every read is bounded in bytes, not rows: see [Limits] and [SampleLimits].
package filesource

import (
	"path"
	"strings"
)

// Format is how a file's records are encoded. The zero value is no format.
type Format uint8

const (
	FormatParquet Format = iota + 1
	FormatJSON
	FormatCSV
)

// String is the format's lower-case name: parquet, json or csv.
func (f Format) String() string {
	switch f {
	case FormatParquet:
		return "parquet"
	case FormatJSON:
		return "json"
	case FormatCSV:
		return "csv"
	}
	return "unknown"
}

// compressionExts are peeled from a name before its format extension. The
// bytes decide the codec; the extension only hides the format.
var compressionExts = []string{".gz", ".gzip", ".zst", ".zstd", ".bz2"}

// formatExts maps a lower-case extension to its format.
var formatExts = map[string]Format{
	".parquet": FormatParquet,
	".pq":      FormatParquet,
	".json":    FormatJSON,
	".jsonl":   FormatJSON,
	".ndjson":  FormatJSON,
	".csv":     FormatCSV,
	".tsv":     FormatCSV,
}

// FormatOf is the format a key's extension names, after any compression
// extensions. Extensions match in any case.
func FormatOf(key string) (Format, bool) {
	_, ext, ok := splitName(path.Base(key))
	return formatExts[ext], ok
}

// splitName peels compression extensions off base, then its format
// extension. It returns the rest and the lower-case format extension.
func splitName(base string) (stem, ext string, ok bool) {
	for peeled := true; peeled; {
		peeled = false
		for _, c := range compressionExts {
			if hasSuffixFold(base, c) {
				base, peeled = base[:len(base)-len(c)], true
			}
		}
	}
	raw := path.Ext(base)
	ext = strings.ToLower(raw)
	if _, ok := formatExts[ext]; !ok {
		return "", "", false
	}
	return base[:len(base)-len(raw)], ext, true
}

// hasSuffixFold is strings.HasSuffix ignoring ASCII case; suffix is ASCII.
func hasSuffixFold(s, suffix string) bool {
	return len(s) >= len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix)
}

// isJSONLines reports a name whose format extension is .jsonl or .ndjson:
// each of its objects is a row, whatever it holds.
func isJSONLines(name string) bool {
	_, ext, ok := splitName(path.Base(name))
	return ok && (ext == ".jsonl" || ext == ".ndjson")
}

// isTSV reports a name whose format extension is .tsv, read with tabs.
func isTSV(name string) bool {
	_, ext, ok := splitName(path.Base(name))
	return ok && ext == ".tsv"
}
