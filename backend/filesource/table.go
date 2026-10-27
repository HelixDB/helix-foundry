package filesource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/maphash"
	"io"
	"iter"
	"os"
	"slices"
	"unicode/utf8"
)

// Limits bound reading one table at sync.
type Limits struct {
	// MaxColumns caps a table's columns, and a Parquet file's leaf columns.
	MaxColumns int
	// MaxDepth caps JSON nesting; a record's own braces are level 1.
	MaxDepth int
	// MaxRecordBytes caps one JSON record or one CSV line.
	MaxRecordBytes int
	// MaxDecompressedBytes caps what one pass decompresses from a table's
	// JSON and CSV files, which stops decompression bombs.
	MaxDecompressedBytes int64
	// MaxWindow caps a zstd frame's window, which the decoder allocates up front.
	MaxWindow uint64
	// MaxRowGroupBytes caps the memory one Parquet row group needs to read,
	// estimated from the footer before any page is decoded.
	MaxRowGroupBytes int64
}

// DefaultLimits are the sync's limits.
var DefaultLimits = Limits{
	MaxColumns:           1000,
	MaxDepth:             64,
	MaxRecordBytes:       16 << 20,
	MaxDecompressedBytes: 4 << 30,
	MaxWindow:            128 << 20,
	MaxRowGroupBytes:     1536 << 20,
}

// maxKeyRows bounds the id hashes kept to prove a primary key (8 bytes
// each); a larger table gets no key, and fails the row limit anyway.
const maxKeyRows = 2_000_000

// File is one downloaded object of a table.
type File struct {
	// Path is the local copy.
	Path string
	// Name is the object key: messages name it, and a .tsv name reads tabs.
	Name   string
	Format Format
	// Partitions are the key's Hive folders, from [Partitions].
	Partitions []Partition
}

// Schema is what pass 1 learned about a table: its columns, each file's
// shape (JSON layout, CSV charset) and whether id is a key.
type Schema struct {
	files   []filePlan
	columns []Column
	// keys are the columns' names encoded as `"name":`.
	keys [][]byte
	// dataColumns counts the columns read from the files; the rest are
	// Hive partition columns.
	dataColumns int
	lim         Limits
	key         []string
	nulled      nulledCounts
}

// filePlan is how pass 2 reads one file.
type filePlan struct {
	file File
	json jsonLayout
	csv  csvLayout
	// partitions are the file's value of each partition column, as JSON.
	partitions [][]byte
}

// Scan reads every row of files (Parquet: the footers and the id column) to
// find the exact columns and primary key. Files are read in the order given;
// [Layout] orders them by key. Unreadable data is an *Error; any other
// error, such as ctx's or a failed read, is returned as it is.
func Scan(ctx context.Context, files []File, lim Limits) (*Schema, error) {
	t := newTableStats(lim.MaxColumns)
	s := &Schema{lim: lim}
	total := newBudget(lim.MaxDecompressedBytes)
	for _, f := range files {
		if err := s.scanFile(ctx, f, t, total); err != nil {
			return nil, err
		}
	}
	s.dataColumns = len(t.cols)
	if err := s.finish(t); err != nil {
		return nil, err
	}
	return s, nil
}

// ErrAppendChanged is appended files that change what a full read writes
// for the table's earlier rows: a new column, a wider type or a lost key.
var ErrAppendChanged = errors.New("appended files change the table")

// Append scans files appended to a table whose earlier files a full read
// wrote with columns and key, so Rows yields just the appended rows exactly
// as a full read of every file writes them. The files must sort after
// earlier, as [Layout] orders them; earlier are read for their partitions only.
func Append(ctx context.Context, columns []Column, key []string, earlier, files []File, lim Limits) (*Schema, error) {
	// The stored columns do not say which were partitions: a trailing column
	// named like an earlier file's Hive folder is taken as that partition.
	hive := map[string]bool{}
	for _, f := range earlier {
		for _, p := range f.Partitions {
			hive[p.Key] = true
		}
	}
	data := len(columns)
	for data > 0 && hive[columns[data-1].Name] {
		data--
	}
	t := newTableStats(lim.MaxColumns)
	for _, c := range columns[:data] {
		t.cols[t.add(c.Name)].Type = c.Type
	}
	t.fileRecords = make([]int64, len(earlier))
	s := &Schema{lim: lim}
	for _, f := range earlier {
		s.files = append(s.files, filePlan{file: f})
	}
	total := newBudget(lim.MaxDecompressedBytes)
	for _, f := range files {
		if err := s.scanFile(ctx, f, t, total); err != nil {
			return nil, err
		}
	}
	s.dataColumns = len(t.cols)
	if s.dataColumns != data {
		return nil, ErrAppendChanged
	}
	if err := s.finish(t); err != nil {
		return nil, err
	}
	s.files = s.files[len(earlier):]
	if !slices.Equal(s.key, key) || !slices.EqualFunc(s.columns, columns, func(a, b Column) bool { return a.Name == b.Name && a.Type == b.Type }) {
		return nil, ErrAppendChanged
	}
	return s, nil
}

// scanFile reads one file in pass 1 and merges what it learned into t.
func (s *Schema) scanFile(ctx context.Context, f File, t *tableStats, total *budget) error {
	lim := s.lim
	plan := filePlan{file: f}
	fs := newFileStats(lim.MaxColumns, t.seed)
	err := safe(f.Format, func() (err error) {
		switch f.Format {
		case FormatJSON:
			plan.json, err = scanJSON(ctx, f, total, lim, fs)
		case FormatCSV:
			plan.csv, err = scanCSV(ctx, f, total, lim, fs)
		case FormatParquet:
			err = scanParquet(ctx, f, lim, fs)
		default:
			err = because(ErrMalformed, "unknown format")
		}
		return err
	})
	if err != nil {
		return inFile(err, f.Name)
	}
	fs.finish()
	if err := t.merge(fs); err != nil {
		return inFile(err, f.Name)
	}
	s.files = append(s.files, plan)
	return nil
}

// finish adds the partition columns after the data columns and settles the
// table's columns and key.
func (s *Schema) finish(t *tableStats) error {
	if err := s.addPartitions(t); err != nil {
		return err
	}
	s.columns = t.columnsOut()
	s.keys = make([][]byte, len(s.columns))
	for i, c := range s.columns {
		s.keys[i] = append(appendString(nil, c.Name), ':')
	}
	s.key = t.primaryKey(s.dataColumns)
	return nil
}

// addPartitions adds a column for each Hive key no file holds as data,
// typed as CSV text, and each file's value for it.
func (s *Schema) addPartitions(t *tableStats) error {
	for i := range s.files {
		p := &s.files[i]
		for _, part := range lastPartitions(p.file.Partitions) {
			j, ok := t.index[part.Key]
			if ok && j < s.dataColumns {
				continue
			}
			if !ok {
				if len(t.cols) >= t.maxColumns {
					return &Error{File: p.file.Name, Err: because(ErrTooManyColumns, "more than %d columns", t.maxColumns)}
				}
				j = t.add(part.Key)
			}
			if part.Value != nil && *part.Value != "" {
				t.cols[j].Type = join(t.cols[j].Type, csvType(*part.Value))
				t.present[j] += t.fileRecords[i]
			}
		}
	}
	for i := range s.files {
		p := &s.files[i]
		p.partitions = make([][]byte, len(t.cols)-s.dataColumns)
		for _, part := range lastPartitions(p.file.Partitions) {
			if j, ok := t.index[part.Key]; ok && j >= s.dataColumns {
				v := ""
				if part.Value != nil {
					v = *part.Value
				}
				p.partitions[j-s.dataColumns] = appendCSVValue(nil, v, t.cols[j].Type)
			}
		}
	}
	return nil
}

// lastPartitions keeps the last value of a key a path repeats.
func lastPartitions(parts []Partition) []Partition {
	out := make([]Partition, 0, len(parts))
	for _, p := range parts {
		if i := slices.IndexFunc(out, func(q Partition) bool { return q.Key == p.Key }); i >= 0 {
			out[i] = p
			continue
		}
		out = append(out, p)
	}
	return out
}

// Columns are the data columns in first-seen order, then the Hive partition
// columns no file holds as data.
func (s *Schema) Columns() []Column { return s.columns }

// PrimaryKey is ["id"] when every row has a non-null, unique id, else nil.
func (s *Schema) PrimaryKey() []string { return s.key }

// Nulled counts values Rows wrote as null because JSON cannot hold them, by
// reason: "timestamp_range" (a year outside 0001-9999 or a time outside one
// day) and "non_finite" (NaN and infinite floats). It covers the last Rows.
func (s *Schema) Nulled() map[string]int64 {
	out := map[string]int64{}
	if s.nulled.timestampRange > 0 {
		out["timestamp_range"] = s.nulled.timestampRange
	}
	if s.nulled.nonFinite > 0 {
		out["non_finite"] = s.nulled.nonFinite
	}
	return out
}

// rowSource is one file's rows in pass 2.
type rowSource interface {
	// next moves to the next row; false is the end of the file.
	next() (bool, error)
	// appendValue writes the row's value of data column c, null if absent.
	appendValue(dst []byte, c int) ([]byte, error)
	close()
}

// Rows reads the files again and yields each row as one JSON object holding
// every column, null where a row lacks it. A row's bytes are valid only
// until the next iteration. The first error ends the sequence.
func (s *Schema) Rows(ctx context.Context) iter.Seq2[[]byte, error] {
	return func(yield func([]byte, error) bool) {
		s.nulled = nulledCounts{}
		total := newBudget(s.lim.MaxDecompressedBytes)
		var buf []byte
		for i := range s.files {
			p := &s.files[i]
			var src rowSource
			err := safe(p.file.Format, func() (err error) {
				src, err = s.open(ctx, p, total)
				return err
			})
			if err != nil {
				yield(nil, inFile(err, p.file.Name))
				return
			}
			for {
				var ok bool
				err := safe(p.file.Format, func() (err error) {
					buf, ok, err = s.row(src, p, buf[:0])
					return err
				})
				if err != nil {
					src.close()
					yield(nil, inFile(err, p.file.Name))
					return
				}
				if !ok {
					break
				}
				if !yield(buf, nil) {
					src.close()
					return
				}
			}
			src.close()
		}
	}
}

// row reads the next row of src and writes it with every column.
func (s *Schema) row(src rowSource, p *filePlan, dst []byte) ([]byte, bool, error) {
	ok, err := src.next()
	if err != nil || !ok {
		return dst, false, err
	}
	dst = append(dst, '{')
	for c := range s.columns {
		if c > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, s.keys[c]...)
		if c >= s.dataColumns {
			if v := p.partitions[c-s.dataColumns]; v != nil {
				dst = append(dst, v...)
			} else {
				dst = append(dst, "null"...)
			}
			continue
		}
		if dst, err = src.appendValue(dst, c); err != nil {
			return dst, false, err
		}
	}
	return append(dst, '}'), true, nil
}

func (s *Schema) open(ctx context.Context, p *filePlan, total *budget) (rowSource, error) {
	switch p.file.Format {
	case FormatJSON:
		return s.openJSON(ctx, p, total)
	case FormatCSV:
		return s.openCSV(ctx, p, total)
	}
	return s.openParquet(ctx, p)
}

// openText opens a local file as decompressed UTF-8 text.
func openText(path string, d decoding, cs charset) (io.Reader, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	br, closeCodec, err := decompress(f, d)
	if err != nil {
		_ = f.Close() // best-effort: read-only
		return nil, nil, err
	}
	return decodeText(br, cs), func() {
		closeCodec()
		_ = f.Close() // best-effort: read-only
	}, nil
}

func (lim Limits) decoding() decoding {
	return decoding{maxWindow: lim.MaxWindow, windowErr: ErrCompression}
}

// scanJSON reads one JSON file in pass 1: its shape, then every record.
func scanJSON(ctx context.Context, f File, total *budget, lim Limits, fs *fileStats) (jsonLayout, error) {
	text, closeText, err := openText(f.Path, lim.decoding(), charsetDetect)
	if err != nil {
		return jsonLayout{}, err
	}
	// Detection may read a whole envelope; it gets its own budget so the
	// table's is spent once.
	br := newBudgetReader(ctx, text, newBudget(lim.MaxDecompressedBytes), lim.MaxRecordBytes)
	det, err := detectJSON(br, lim.MaxRecordBytes, false, isJSONLines(f.Name))
	closeText()
	if err != nil {
		return jsonLayout{}, recordError(jsonError(err, br), det.elements+1)
	}
	layout, err := det.layout()
	if err != nil {
		return jsonLayout{}, err
	}
	text, closeText, err = openText(f.Path, lim.decoding(), charsetDetect)
	if err != nil {
		return jsonLayout{}, err
	}
	defer closeText()
	recs, err := newJSONRecords(newBudgetReader(ctx, text, total, lim.MaxRecordBytes), layout, lim.MaxRecordBytes, false)
	if err != nil {
		return jsonLayout{}, err
	}
	var scratch []byte
	for {
		ok, err := recs.next()
		if err != nil || !ok {
			return layout, err
		}
		if err := fs.addJSON(recs.fields, &scratch, lim.MaxDepth); err != nil {
			return layout, recordError(err, recs.record)
		}
	}
}

// jsonSource is one JSON file's rows in pass 2.
type jsonSource struct {
	recs      *jsonRecords
	columns   []Column
	maxDepth  int
	closeText func()
}

func (s *Schema) openJSON(ctx context.Context, p *filePlan, total *budget) (rowSource, error) {
	text, closeText, err := openText(p.file.Path, s.lim.decoding(), charsetDetect)
	if err != nil {
		return nil, err
	}
	recs, err := newJSONRecords(newBudgetReader(ctx, text, total, s.lim.MaxRecordBytes), p.json, s.lim.MaxRecordBytes, false)
	if err != nil {
		closeText()
		return nil, err
	}
	return &jsonSource{recs: recs, columns: s.columns, maxDepth: s.lim.MaxDepth, closeText: closeText}, nil
}

func (j *jsonSource) next() (bool, error) { return j.recs.next() }

func (j *jsonSource) appendValue(dst []byte, c int) ([]byte, error) {
	i, ok := j.recs.index[j.columns[c].Name]
	if !ok {
		return append(dst, "null"...), nil
	}
	out, err := normalize(dst, j.recs.fields[i].raw, 1, j.maxDepth)
	if err != nil {
		return dst, recordError(err, j.recs.record)
	}
	return out, nil
}

func (j *jsonSource) close() { j.closeText() }

// scanCSV reads one CSV file in pass 1. A file that is not valid UTF-8 is
// read again as Windows-1252, the usual charset of such exports.
func scanCSV(ctx context.Context, f File, total *budget, lim Limits, fs *fileStats) (csvLayout, error) {
	left := total.left
	layout := csvLayout{charset: charsetDetect}
	err := scanCSVAs(ctx, f, total, lim, fs, layout.charset)
	if errors.Is(err, errNotUTF8) {
		total.left = left
		fs.reset()
		layout.charset = charsetWindows1252
		err = scanCSVAs(ctx, f, total, lim, fs, layout.charset)
	}
	return layout, err
}

func scanCSVAs(ctx context.Context, f File, total *budget, lim Limits, fs *fileStats, cs charset) error {
	text, closeText, err := openText(f.Path, lim.decoding(), cs)
	if err != nil {
		return err
	}
	defer closeText()
	recs, err := newCSVRecords(newBudgetReader(ctx, text, total, lim.MaxRecordBytes), f.Name, lim.MaxRecordBytes, lim.MaxColumns)
	if err != nil {
		return err
	}
	checkUTF8 := cs == charsetDetect
	cols := make([]int, len(recs.header))
	for i, name := range recs.header {
		if checkUTF8 && !utf8.ValidString(name) {
			return errNotUTF8
		}
		cols[i], _ = fs.column(name) // newCSVRecords held the header to MaxColumns
	}
	id := fs.idColumn(recs.header)
	for {
		ok, err := recs.next()
		if err != nil || !ok {
			return err
		}
		for i, col := range cols {
			v := recs.value(i)
			if checkUTF8 && !utf8.ValidString(v) {
				return errNotUTF8
			}
			if v != "" {
				fs.observe(col, csvType(v))
			}
		}
		fs.records++
		if id < 0 || recs.value(id) == "" {
			fs.idNull = true
			continue
		}
		fs.addID('v', []byte(recs.value(id)))
	}
}

// csvSource is one CSV file's rows in pass 2.
type csvSource struct {
	recs *csvRecords
	// field maps a data column to the header field holding it, -1 for none.
	field     []int
	types     []Type
	closeText func()
}

func (s *Schema) openCSV(ctx context.Context, p *filePlan, total *budget) (rowSource, error) {
	text, closeText, err := openText(p.file.Path, s.lim.decoding(), p.csv.charset)
	if err != nil {
		return nil, err
	}
	recs, err := newCSVRecords(newBudgetReader(ctx, text, total, s.lim.MaxRecordBytes), p.file.Name, s.lim.MaxRecordBytes, s.lim.MaxColumns)
	if err != nil {
		closeText()
		return nil, err
	}
	src := &csvSource{recs: recs, field: make([]int, s.dataColumns), types: make([]Type, s.dataColumns), closeText: closeText}
	at := map[string]int{}
	for i, n := range recs.header {
		at[n] = i
	}
	for c := range src.field {
		src.types[c] = s.columns[c].Type
		if i, ok := at[s.columns[c].Name]; ok {
			src.field[c] = i
		} else {
			src.field[c] = -1
		}
	}
	return src, nil
}

func (c *csvSource) next() (bool, error) { return c.recs.next() }

func (c *csvSource) appendValue(dst []byte, col int) ([]byte, error) {
	i := c.field[col]
	if i < 0 {
		return append(dst, "null"...), nil
	}
	return appendCSVValue(dst, c.recs.value(i), c.types[col]), nil
}

func (c *csvSource) close() { c.closeText() }

// scanParquet reads one Parquet file in pass 1: its footer for columns and
// row count, and its id column, if any, for the primary key.
func scanParquet(ctx context.Context, f File, lim Limits, fs *fileStats) error {
	pf, err := openParquet(f.Path, lim, &nulledCounts{})
	if err != nil {
		return err
	}
	defer pf.close()
	fs.counted = false
	id := -1
	for i, field := range pf.fields {
		c, _ := fs.column(field.name) // openParquet held the leaf columns to MaxColumns
		fs.cols[c].Type = join(fs.cols[c].Type, field.typ)
		fs.cols[c].Nullable = fs.cols[c].Nullable || field.nullable
		if field.name == "id" && field.leaf >= 0 {
			id = i
		}
	}
	fs.records = pf.reader.NumRows()
	if id < 0 {
		fs.idNull = fs.records > 0
		return nil
	}
	var scratch []byte
	for rg := 0; rg < pf.reader.NumRowGroups() && !fs.idNull; rg++ {
		rr, err := pf.arrow.GetRecordReader(ctx, []int{pf.fields[id].leaf}, []int{rg})
		if err != nil {
			return parquetError(err)
		}
		for rr.Next() && !fs.idNull {
			col := rr.RecordBatch().Column(0)
			for i := 0; i < col.Len(); i++ {
				if col.IsNull(i) {
					fs.idNull = true
					break
				}
				scratch = pf.fields[id].conv(scratch[:0], col, i)
				fs.addID('p', scratch)
			}
		}
		err = rr.Err()
		rr.Release()
		if err != nil && !errors.Is(err, io.EOF) {
			return parquetError(err)
		}
	}
	return ctx.Err()
}

func (s *Schema) openParquet(ctx context.Context, p *filePlan) (rowSource, error) {
	pf, err := openParquet(p.file.Path, s.lim, &s.nulled)
	if err != nil {
		return nil, err
	}
	at := map[string]int{}
	for i, f := range pf.fields {
		at[f.name] = i
	}
	colOf := make([]int, s.dataColumns)
	for c := range colOf {
		if i, ok := at[s.columns[c].Name]; ok {
			colOf[c] = i
		} else {
			colOf[c] = -1
		}
	}
	return &parquetRows{ctx: ctx, file: pf, colOf: colOf}, nil
}

// safe runs a decoder, turning a panic on malformed input into an error so
// a bad file never stops the process.
func safe(f Format, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			kind := ErrMalformed
			if f == FormatParquet {
				kind = ErrParquet
			}
			err = &reason{kind: kind, msg: "the file could not be decoded", cause: fmt.Errorf("panic: %v", r)}
		}
	}()
	return fn()
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// inFile names the file unreadable data is in. Other errors stay bare.
func inFile(err error, name string) error {
	if !isUnreadable(err) {
		return err
	}
	var e *Error
	if errors.As(err, &e) {
		return &Error{File: name, Record: e.Record, Err: e.Err}
	}
	return &Error{File: name, Err: err}
}

// fileStats is what pass 1 learned from one file.
type fileStats struct {
	cols  []Column
	index map[string]int
	// counted files (JSON, CSV) derive nullability from nonNull; Parquet
	// files read it from metadata.
	counted    bool
	nonNull    []int64
	records    int64
	maxColumns int
	seed       maphash.Seed
	ids        []uint64
	// idNull is a row without an id, which rules out the primary key.
	idNull bool
}

func newFileStats(maxColumns int, seed maphash.Seed) *fileStats {
	return &fileStats{index: map[string]int{}, counted: true, maxColumns: maxColumns, seed: seed}
}

func (fs *fileStats) reset() {
	*fs = *newFileStats(fs.maxColumns, fs.seed)
}

// column is the index of the column named name, added if new.
func (fs *fileStats) column(name string) (int, error) {
	if i, ok := fs.index[name]; ok {
		return i, nil
	}
	if len(fs.cols) >= fs.maxColumns {
		return 0, because(ErrTooManyColumns, "more than %d columns", fs.maxColumns)
	}
	fs.index[name] = len(fs.cols)
	fs.cols = append(fs.cols, Column{Name: name})
	fs.nonNull = append(fs.nonNull, 0)
	return len(fs.cols) - 1, nil
}

func (fs *fileStats) observe(i int, t Type) {
	fs.cols[i].Type = join(fs.cols[i].Type, t)
	if t != TypeNull {
		fs.nonNull[i]++
	}
}

// idColumn is the index of the id field in a header, or -1.
func (fs *fileStats) idColumn(header []string) int {
	for i, n := range header {
		if n == "id" {
			return i
		}
	}
	return -1
}

// addID records one id value; kind separates encodings that may not collide.
func (fs *fileStats) addID(kind byte, value []byte) {
	if fs.idNull {
		return
	}
	if len(fs.ids) >= maxKeyRows {
		fs.idNull, fs.ids = true, nil
		return
	}
	var h maphash.Hash
	h.SetSeed(fs.seed)
	_ = h.WriteByte(kind) // maphash writes never fail
	_, _ = h.Write(value) // maphash writes never fail
	fs.ids = append(fs.ids, h.Sum64())
}

// addJSON records one JSON record's fields, checking each value's nesting.
func (fs *fileStats) addJSON(fields []field, scratch *[]byte, maxDepth int) error {
	sawID := false
	for _, f := range fields {
		var err error
		if *scratch, err = normalize((*scratch)[:0], f.raw, 1, maxDepth); err != nil {
			return err
		}
		i, err := fs.column(f.key)
		if err != nil {
			return err
		}
		t := jsonType(f.raw)
		fs.observe(i, t)
		if f.key != "id" {
			continue
		}
		sawID = true
		switch t {
		case TypeNull:
			fs.idNull = true
		case TypeString:
			// A string id and a number id with the same text are the same key
			// to anything reading them as text, so they hash alike.
			var s string
			_ = json.Unmarshal(f.raw, &s) // the decoder validated the string
			fs.addID('v', []byte(s))
		default:
			fs.addID('v', *scratch)
		}
	}
	if !sawID {
		fs.idNull = true
	}
	fs.records++
	return nil
}

// finish derives counted columns' nullability: a value was null or absent.
func (fs *fileStats) finish() {
	if !fs.counted {
		return
	}
	for i := range fs.cols {
		fs.cols[i].Nullable = fs.nonNull[i] < fs.records
	}
}

// tableStats merges files' stats into the table's.
type tableStats struct {
	cols       []Column
	index      map[string]int
	present    []int64 // rows of the files holding each column
	records    int64
	maxColumns int
	// fileRecords is each merged file's row count.
	fileRecords []int64
	seed        maphash.Seed
	ids         map[uint64]struct{}
	idDropped   bool
}

func newTableStats(maxColumns int) *tableStats {
	return &tableStats{index: map[string]int{}, maxColumns: maxColumns, seed: maphash.MakeSeed(), ids: map[uint64]struct{}{}}
}

func (t *tableStats) add(name string) int {
	t.index[name] = len(t.cols)
	t.cols = append(t.cols, Column{Name: name})
	t.present = append(t.present, 0)
	return len(t.cols) - 1
}

func (t *tableStats) merge(fs *fileStats) error {
	for _, c := range fs.cols {
		j, ok := t.index[c.Name]
		if !ok {
			if len(t.cols) >= t.maxColumns {
				return because(ErrTooManyColumns, "more than %d columns", t.maxColumns)
			}
			j = t.add(c.Name)
		}
		t.cols[j].Type = join(t.cols[j].Type, c.Type)
		t.cols[j].Nullable = t.cols[j].Nullable || c.Nullable
		t.present[j] += fs.records
	}
	t.records += fs.records
	t.fileRecords = append(t.fileRecords, fs.records)
	if t.idDropped {
		return nil
	}
	if fs.idNull || len(t.ids)+len(fs.ids) > maxKeyRows {
		t.idDropped, t.ids = true, nil
		return nil
	}
	for _, h := range fs.ids {
		if _, dup := t.ids[h]; dup {
			t.idDropped, t.ids = true, nil
			return nil
		}
		t.ids[h] = struct{}{}
	}
	return nil
}

// columnsOut are the merged columns; one absent from rows of some file is
// nullable.
func (t *tableStats) columnsOut() []Column {
	out := make([]Column, len(t.cols))
	for i, c := range t.cols {
		c.Nullable = c.Nullable || t.present[i] < t.records
		out[i] = c
	}
	return out
}

// primaryKey is ["id"] when the id data column was never null or repeated.
func (t *tableStats) primaryKey(dataColumns int) []string {
	i, ok := t.index["id"]
	if !ok || i >= dataColumns || t.idDropped {
		return nil
	}
	switch t.cols[i].Type {
	case TypeNull, TypeJSON:
		return nil
	}
	return []string{"id"}
}
