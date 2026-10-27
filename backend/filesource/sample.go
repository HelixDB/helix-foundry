package filesource

import (
	"bytes"
	"context"
	"errors"
	"hash/maphash"
	"io"
	"math"
	"slices"
	"unicode/utf8"

	"github.com/apache/arrow-go/v18/parquet/metadata"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"golang.org/x/text/encoding/charmap"
)

// SampleLimits bound one preview read, which runs in a shared API process.
type SampleLimits struct {
	// MaxDecompressed caps the bytes decoded from one sample; more marks the
	// sample Truncated rather than failing it.
	MaxDecompressed int64
	// MaxFooter caps a Parquet footer read for its schema.
	MaxFooter int64
	// MaxWindow caps a zstd frame's window.
	MaxWindow uint64
	// MaxRecords caps the records inspected for columns.
	MaxRecords int
	// MaxRows caps the preview rows kept, and MaxRowBytes each one's size.
	MaxRows, MaxRowBytes int
}

// DefaultSampleLimits are the preview's limits.
var DefaultSampleLimits = SampleLimits{
	MaxDecompressed: 8 << 20,
	MaxFooter:       1 << 20,
	MaxWindow:       8 << 20,
	MaxRecords:      1000,
	MaxRows:         5,
	MaxRowBytes:     4 << 10,
}

// Sampled is what a preview read found.
type Sampled struct {
	Columns []Column
	// Rows are at most MaxRows JSON objects holding every sampled column.
	Rows [][]byte
	// Truncated is set when the sample stopped before the end of the file.
	Truncated bool
	// PrimaryKey guesses the key from the records inspected: ["id"] when
	// each had a non-null id and no two the same, as Scan decides for all.
	PrimaryKey []string
}

// Sample reads the start of a JSON or CSV object. truncated says r holds
// only a prefix of the object (a ranged read), so a record cut off at the
// end ends the sample instead of failing it. A window or footer over the
// limits is ErrSampleBudget. Unreadable data is an *Error; any other error,
// such as ctx's or r's own, is returned as it is.
func Sample(ctx context.Context, r io.Reader, name string, f Format, truncated bool, lim SampleLimits) (Sampled, error) {
	var out Sampled
	err := safe(f, func() error {
		if f != FormatJSON && f != FormatCSV {
			return because(ErrParquet, "a Parquet sample reads only the footer")
		}
		br, closeCodec, err := decompress(r, decoding{maxWindow: lim.MaxWindow, windowErr: ErrSampleBudget, cut: truncated})
		if err != nil {
			return err
		}
		defer closeCodec()
		data, cut, err := readSample(ctx, decodeText(br, charsetDetect), lim.MaxDecompressed)
		if err != nil {
			return err
		}
		cut = cut || truncated
		if f == FormatJSON {
			out, err = sampleJSON(ctx, data, cut, isJSONLines(name), lim)
		} else {
			out, err = sampleCSV(ctx, data, name, cut, lim)
		}
		return err
	})
	if err != nil {
		return Sampled{}, inFile(err, name)
	}
	return out, nil
}

// readSample reads at most limit bytes; cut reports more text after them, or
// cut input ending early.
func readSample(ctx context.Context, r io.Reader, limit int64) ([]byte, bool, error) {
	b := newBudget(limit)
	b.cut = true
	data, err := io.ReadAll(newBudgetReader(ctx, r, b, math.MaxInt32-recordSlack))
	if errors.Is(err, errCut) {
		return data, true, nil
	}
	return data, false, err
}

// sampleReader reads sampled text with no further limits: it is in memory.
func sampleReader(ctx context.Context, data []byte) *budgetReader {
	return newBudgetReader(ctx, bytes.NewReader(data), newBudget(int64(len(data))), len(data))
}

func sampleJSON(ctx context.Context, data []byte, cut, lines bool, lim SampleLimits) (Sampled, error) {
	br := sampleReader(ctx, data)
	det, err := detectJSON(br, len(data), cut, lines)
	if err != nil {
		return Sampled{}, recordError(jsonError(err, br), det.elements+1)
	}
	layout, err := det.layout()
	if err != nil {
		return Sampled{}, err
	}
	recs, err := newJSONRecords(sampleReader(ctx, data), layout, len(data), cut)
	if err != nil {
		return Sampled{}, err
	}
	fs := newFileStats(DefaultLimits.MaxColumns, maphash.MakeSeed())
	var scratch []byte
	var kept [][]field
	out := Sampled{Truncated: cut}
	for {
		if fs.records == int64(lim.MaxRecords) {
			out.Truncated = true
			break
		}
		ok, err := recs.next()
		if err != nil {
			return Sampled{}, err
		}
		if !ok {
			break
		}
		if err := fs.addJSON(recs.fields, &scratch, DefaultLimits.MaxDepth); err != nil {
			return Sampled{}, recordError(err, recs.record)
		}
		if len(kept) < lim.MaxRows && fieldsSize(recs.fields) <= lim.MaxRowBytes {
			kept = append(kept, cloneFields(recs.fields))
		}
	}
	out.Columns, out.PrimaryKey = sampleKey(fs)
	for _, rec := range kept {
		row := []byte{'{'}
		for c, col := range fs.cols {
			if c > 0 {
				row = append(row, ',')
			}
			row = append(appendString(row, col.Name), ':')
			i := slices.IndexFunc(rec, func(f field) bool { return f.key == col.Name })
			if i < 0 {
				row = append(row, "null"...)
				continue
			}
			row, _ = normalize(row, rec[i].raw, 1, DefaultLimits.MaxDepth) // addJSON checked it
		}
		if row = append(row, '}'); len(row) <= lim.MaxRowBytes {
			out.Rows = append(out.Rows, row)
		}
	}
	return out, nil
}

// sampleKey finishes a sample's stats: its columns, and its key guess.
func sampleKey(fs *fileStats) ([]Column, []string) {
	fs.finish()
	t := newTableStats(DefaultLimits.MaxColumns)
	_ = t.merge(fs) // one file's columns are within the limit merge checks
	return fs.cols, t.primaryKey(len(fs.cols))
}

func fieldsSize(fields []field) int {
	n := 2
	for _, f := range fields {
		n += len(f.key) + len(f.raw) + 4
	}
	return n
}

func cloneFields(fields []field) []field {
	out := make([]field, len(fields))
	for i, f := range fields {
		out[i] = field{key: f.key, raw: bytes.Clone(f.raw)}
	}
	return out
}

func sampleCSV(ctx context.Context, data []byte, name string, cut bool, lim SampleLimits) (Sampled, error) {
	// A cut can split a UTF-8 sequence; only the bytes before it decide.
	whole := data
	if cut {
		whole = data[:max(0, len(data)-utf8.UTFMax)]
	}
	if !utf8.Valid(whole) {
		data, _ = charmap.Windows1252.NewDecoder().Bytes(data) // every byte is a Windows-1252 character
	}
	recs, err := newCSVRecords(sampleReader(ctx, data), name, len(data), DefaultLimits.MaxColumns)
	if err != nil {
		return Sampled{}, err
	}
	fs := newFileStats(DefaultLimits.MaxColumns, maphash.MakeSeed())
	cols := make([]int, len(recs.header))
	for i, h := range recs.header {
		cols[i], _ = fs.column(h) // the header is within MaxColumns
	}
	out := Sampled{Truncated: cut}
	var kept [][]string
	id := fs.idColumn(recs.header)
	take := func(rec []string) {
		for i, col := range cols {
			if i < len(rec) && rec[i] != "" {
				fs.observe(col, csvType(rec[i]))
			}
		}
		fs.records++
		if id < 0 || id >= len(rec) || rec[id] == "" {
			fs.idNull = true
		} else {
			fs.addID('v', []byte(rec[id]))
		}
		if len(kept) < lim.MaxRows && csvSize(rec) <= lim.MaxRowBytes {
			row := make([]string, len(cols))
			copy(row, rec)
			kept = append(kept, row)
		}
	}
	// The last record of cut input may be cut too, so a record is taken only
	// once the next one starts.
	var pending []string
	have := false
	for {
		ok, err := recs.next()
		if err != nil {
			return Sampled{}, err
		}
		if !ok {
			if have && !cut {
				take(pending)
			}
			break
		}
		if have {
			take(pending)
		}
		if fs.records == int64(lim.MaxRecords) {
			out.Truncated = true
			break
		}
		pending, have = append(pending[:0], recs.fields...), true
	}
	out.Columns, out.PrimaryKey = sampleKey(fs)
	for _, rec := range kept {
		row := []byte{'{'}
		for c, col := range fs.cols {
			if c > 0 {
				row = append(row, ',')
			}
			row = appendCSVValue(append(appendString(row, col.Name), ':'), rec[c], col.Type)
		}
		if row = append(row, '}'); len(row) <= lim.MaxRowBytes {
			out.Rows = append(out.Rows, row)
		}
	}
	return out, nil
}

func csvSize(rec []string) int {
	n := 2
	for _, v := range rec {
		n += len(v) + 1
	}
	return n
}

// ParquetFooter reads a Parquet object's columns and row count from its
// footer alone, through ranged reads of at most lim.MaxFooter + 8 bytes. A
// footer over lim.MaxFooter is ErrSampleBudget.
func ParquetFooter(ctx context.Context, r io.ReaderAt, size int64, lim SampleLimits) (cols []Column, rows int64, err error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	n, err := footerLength(r, size, lim.MaxFooter, ErrSampleBudget)
	if err != nil {
		return nil, 0, err
	}
	footer := make([]byte, n)
	if _, err := r.ReadAt(footer, size-footerTail-n); err != nil {
		return nil, 0, err
	}
	err = safe(FormatParquet, func() error {
		if err := checkFooterSchema(bytes.NewReader(footer), DefaultLimits.MaxColumns); err != nil {
			return err
		}
		md, err := metadata.NewFileMetaData(footer, nil)
		if err != nil {
			return &reason{kind: ErrParquet, msg: "unreadable Parquet footer", cause: err}
		}
		if md.Schema.NumColumns() > DefaultLimits.MaxColumns {
			return because(ErrTooManyColumns, "more than %d columns", DefaultLimits.MaxColumns)
		}
		manifest, err := pqarrow.NewSchemaManifest(md.Schema, md.KeyValueMetadata(), &pqarrow.ArrowReadProperties{})
		if err != nil {
			return &reason{kind: ErrParquet, msg: "unreadable Parquet schema", cause: err}
		}
		fields, err := parquetFields(manifest, md.Schema, DefaultLimits.MaxDepth, &nulledCounts{})
		if err != nil {
			return err
		}
		for _, f := range fields {
			cols = append(cols, Column{Name: f.name, Type: f.typ, Nullable: f.nullable})
		}
		rows = md.NumRows
		return nil
	})
	return cols, rows, err
}
