package snapshot

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

// Pair is one row's identity and content: Key hashes the primary key's
// values and Row the snapshot line. A keyless table's pairs have a zero Key.
type Pair struct {
	Key [16]byte
	Row [8]byte
}

// Counts is what a comparison or merge found.
type Counts struct {
	Inserts, Updates, Deletes int64
}

func (c Counts) Changed() bool { return c.Inserts+c.Updates+c.Deletes > 0 }

// canonical re-encodes one JSON value so the same value always has the same
// bytes, whichever source encoded it: numbers keep their text, strings lose
// any escaping a database added.
func canonical(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("null"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return encodeValue(v)
}

func encodeValue(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), newline), nil
}

// keyValues returns the canonical JSON array of a row's key values, null for
// a missing column.
func keyValues(fields map[string]json.RawMessage, key []string) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, col := range key {
		if i > 0 {
			buf.WriteByte(',')
		}
		v, err := canonical(fields[col])
		if err != nil {
			return nil, fmt.Errorf("key column %s: %w", col, err)
		}
		buf.Write(v)
	}
	buf.WriteByte(']')
	return buf.Bytes(), nil
}

func hashKey(values []byte) [16]byte {
	sum := sha256.Sum256(values)
	var k [16]byte
	copy(k[:], sum[:16])
	return k
}

func hashRow(line []byte) [8]byte {
	sum := sha256.Sum256(line)
	var r [8]byte
	copy(r[:], sum[:8])
	return r
}

// pairOf hashes one snapshot line; key empty means a keyless table.
func pairOf(line []byte, key []string) (Pair, map[string]json.RawMessage, error) {
	p := Pair{Row: hashRow(line)}
	if len(key) == 0 {
		return p, nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return Pair{}, nil, fmt.Errorf("decode row: %w", err)
	}
	values, err := keyValues(fields, key)
	if err != nil {
		return Pair{}, nil, err
	}
	p.Key = hashKey(values)
	return p, fields, nil
}

// KeyHash is the identity of a row with these key values, as Pair.Key.
func KeyHash(key []string, values map[string]any) ([16]byte, error) {
	fields := make(map[string]json.RawMessage, len(key))
	for _, col := range key {
		raw, err := encodeValue(values[col])
		if err != nil {
			return [16]byte{}, err
		}
		fields[col] = raw
	}
	encoded, err := keyValues(fields, key)
	if err != nil {
		return [16]byte{}, err
	}
	return hashKey(encoded), nil
}

// Digest is the SHA-256 of a table's pairs, independent of row order. A key
// seen twice counts once, its last row winning.
func Digest(pairs []Pair, keyed bool) []byte {
	pairs = settle(pairs, keyed)
	slices.SortFunc(pairs, func(a, b Pair) int {
		if c := bytes.Compare(a.Key[:], b.Key[:]); c != 0 {
			return c
		}
		return bytes.Compare(a.Row[:], b.Row[:])
	})
	h := sha256.New()
	for _, p := range pairs {
		h.Write(p.Key[:])
		h.Write(p.Row[:])
	}
	return h.Sum(nil)
}

// settle keeps the last row of each key; a keyless table keeps every row.
func settle(pairs []Pair, keyed bool) []Pair {
	if !keyed {
		return slices.Clone(pairs)
	}
	last := make(map[[16]byte][8]byte, len(pairs))
	for _, p := range pairs {
		last[p.Key] = p.Row
	}
	out := make([]Pair, 0, len(last))
	for k, r := range last {
		out = append(out, Pair{Key: k, Row: r})
	}
	return out
}

// Download copies a snapshot to a local gzip file in dir for repeated reads.
// The caller removes the file.
func Download(ctx context.Context, store Store, key, dir string) (string, error) {
	body, err := store.Open(ctx, key)
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }() // best-effort: only read
	f, err := os.CreateTemp(dir, "foundry-snapshot-prev-*.jsonl.gz")
	if err != nil {
		return "", fmt.Errorf("create snapshot download: %w", err)
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("download snapshot %s: %w", key, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("download snapshot %s: %w", key, err)
	}
	return f.Name(), nil
}

// eachLine calls fn with every line of a gzipped NDJSON file, without its newline.
func eachLine(path string, fn func(line []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // best-effort: only read
	gz, err := gzip.NewReader(bufio.NewReaderSize(f, 1<<16))
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	r := bufio.NewReaderSize(gz, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		line = bytes.TrimSuffix(line, newline)
		if len(line) > 0 {
			if err := fn(line); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
	}
}

// BatchWriter writes a change batch: gzipped NDJSON lines of upserts and
// deletes by key.
type BatchWriter struct {
	file *os.File
	gz   *gzip.Writer
	buf  *bufio.Writer
}

func NewBatchWriter(dir string) (*BatchWriter, error) {
	file, err := os.CreateTemp(dir, "foundry-snapshot-batch-*.jsonl.gz")
	if err != nil {
		return nil, fmt.Errorf("create change batch: %w", err)
	}
	buf := bufio.NewWriterSize(file, 1<<16)
	return &BatchWriter{file: file, buf: buf, gz: gzip.NewWriter(buf)}, nil
}

func (b *BatchWriter) upsert(keyFields map[string]json.RawMessage, key []string, row []byte) error {
	k, err := keyObject(keyFields, key)
	if err != nil {
		return err
	}
	return b.line(`{"op":"upsert","key":`, k, `,"row":`, row, `}`)
}

func (b *BatchWriter) delete(keyFields map[string]json.RawMessage, key []string) error {
	k, err := keyObject(keyFields, key)
	if err != nil {
		return err
	}
	return b.line(`{"op":"delete","key":`, k, `}`)
}

func (b *BatchWriter) line(parts ...any) error {
	for _, p := range parts {
		var err error
		switch v := p.(type) {
		case string:
			_, err = b.gz.Write([]byte(v))
		case []byte:
			_, err = b.gz.Write(v)
		}
		if err != nil {
			return err
		}
	}
	_, err := b.gz.Write(newline)
	return err
}

func keyObject(fields map[string]json.RawMessage, key []string) ([]byte, error) {
	obj := make(map[string]json.RawMessage, len(key))
	for _, col := range key {
		v, err := canonical(fields[col])
		if err != nil {
			return nil, err
		}
		obj[col] = v
	}
	return encodeValue(obj)
}

// Upload finishes the batch and stores it under key.
func (b *BatchWriter) Upload(ctx context.Context, store Store, key string) error {
	if err := b.gz.Close(); err != nil {
		return fmt.Errorf("finish change batch: %w", err)
	}
	if err := b.buf.Flush(); err != nil {
		return fmt.Errorf("flush change batch: %w", err)
	}
	if _, err := b.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind change batch: %w", err)
	}
	return store.Put(ctx, key, b.file)
}

// Close removes the temporary file.
func (b *BatchWriter) Close() error {
	_ = b.file.Close() // best-effort: the file is removed next either way
	return os.Remove(b.file.Name())
}

// Compare finds how the snapshot w wrote differs from the previous one in
// prevPath, by key, and writes the differences to batch. A windowed table
// reports no deletes: its rows leave the window, not the source.
func Compare(prevPath string, w *Writer, windowed bool, batch *BatchWriter) (Counts, error) {
	key := w.key
	old := map[[16]byte][8]byte{}
	if err := eachLine(prevPath, func(line []byte) error {
		p, _, err := pairOf(line, key)
		if err != nil {
			return err
		}
		old[p.Key] = p.Row
		return nil
	}); err != nil {
		return Counts{}, err
	}
	latest := map[[16]byte][8]byte{}
	for _, p := range w.pairs {
		latest[p.Key] = p.Row
	}
	var counts Counts
	changed := map[[16]byte]bool{}
	for k, row := range latest {
		prev, ok := old[k]
		switch {
		case !ok:
			counts.Inserts++
			changed[k] = true
		case prev != row:
			counts.Updates++
			changed[k] = true
		}
	}
	deleted := map[[16]byte]bool{}
	if !windowed {
		for k := range old {
			if _, ok := latest[k]; !ok {
				counts.Deletes++
				deleted[k] = true
			}
		}
	}
	if !counts.Changed() {
		return counts, nil
	}
	if err := w.finish(); err != nil {
		return Counts{}, err
	}
	written := map[[16]byte]bool{}
	if err := eachLine(w.file.Name(), func(line []byte) error {
		p, fields, err := pairOf(line, key)
		if err != nil {
			return err
		}
		if !changed[p.Key] || written[p.Key] || latest[p.Key] != p.Row {
			return nil
		}
		written[p.Key] = true
		return batch.upsert(fields, key, line)
	}); err != nil {
		return Counts{}, err
	}
	if len(deleted) == 0 {
		return counts, nil
	}
	err := eachLine(prevPath, func(line []byte) error {
		p, fields, err := pairOf(line, key)
		if err != nil {
			return err
		}
		if !deleted[p.Key] {
			return nil
		}
		delete(deleted, p.Key)
		return batch.delete(fields, key)
	})
	return counts, err
}

// ChangeSet spools one table's changes to a local file in the order a source
// reported them; Merge applies them, the last change to a key winning.
type ChangeSet struct {
	file  *os.File
	buf   *bufio.Writer
	ops   int
	where []deleteWhere
	// Columns are the keys the upserted rows carried, in the order first seen.
	Columns []string
	seen    map[string]bool
}

type deleteWhere struct {
	column string
	value  []byte
}

type spooled struct {
	Op      string          `json:"op"`
	Row     json.RawMessage `json:"row,omitempty"`
	Key     json.RawMessage `json:"key,omitempty"`
	Version json.RawMessage `json:"version,omitempty"`
}

func NewChangeSet(dir string) (*ChangeSet, error) {
	file, err := os.CreateTemp(dir, "foundry-snapshot-changes-*.jsonl")
	if err != nil {
		return nil, fmt.Errorf("create change set: %w", err)
	}
	return &ChangeSet{file: file, buf: bufio.NewWriterSize(file, 1<<16), seen: map[string]bool{}}, nil
}

// Upsert records a row as the source has it now.
func (c *ChangeSet) Upsert(row map[string]any) error {
	keys := make([]string, 0, len(row))
	for k := range row {
		if !c.seen[k] {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		c.seen[k] = true
		c.Columns = append(c.Columns, k)
	}
	line, err := encodeValue(row)
	if err != nil {
		return fmt.Errorf("encode row: %w", err)
	}
	return c.spool(spooled{Op: "upsert", Row: line})
}

// UpsertJSON records a row the source already encoded as one JSON object.
func (c *ChangeSet) UpsertJSON(row []byte) error {
	return c.spool(spooled{Op: "upsert", Row: slices.Clone(row)})
}

// Delete records that the row with these key values is gone.
func (c *ChangeSet) Delete(key map[string]any) error {
	raw, err := encodeValue(key)
	if err != nil {
		return fmt.Errorf("encode key: %w", err)
	}
	return c.spool(spooled{Op: "delete", Key: raw})
}

// DeleteVersion records that the row with these key values is gone as of
// version: Merge with a version column keeps a stored row that is newer.
func (c *ChangeSet) DeleteVersion(key map[string]any, version any) error {
	raw, err := encodeValue(key)
	if err != nil {
		return fmt.Errorf("encode key: %w", err)
	}
	v, err := encodeValue(version)
	if err != nil {
		return fmt.Errorf("encode version: %w", err)
	}
	return c.spool(spooled{Op: "delete", Key: raw, Version: v})
}

// DeleteWhere removes every previous row whose column holds value, before
// this set's upserts apply: a cascade the source doesn't report row by row.
func (c *ChangeSet) DeleteWhere(column string, value any) error {
	raw, err := encodeValue(value)
	if err != nil {
		return fmt.Errorf("encode value: %w", err)
	}
	c.where = append(c.where, deleteWhere{column: column, value: raw})
	c.ops++
	return nil
}

// spool appends one change. The row is written verbatim: json.Marshal would
// compact a database's JSON and change the row's hash.
func (c *ChangeSet) spool(op spooled) error {
	var line []byte
	if op.Op == "upsert" {
		line = append(append([]byte(`{"op":"upsert","row":`), op.Row...), '}')
	} else {
		var err error
		if line, err = json.Marshal(op); err != nil {
			return err
		}
	}
	c.ops++
	if _, err := c.buf.Write(line); err != nil {
		return err
	}
	return c.buf.WriteByte('\n')
}

// Empty reports a set no source change reached.
func (c *ChangeSet) Empty() bool { return c.ops == 0 }

// Close removes the spool file.
func (c *ChangeSet) Close() error {
	_ = c.file.Close() // best-effort: the file is removed next either way
	return os.Remove(c.file.Name())
}

// Merge writes the previous snapshot in prevPath with the set applied into
// w, records the effect in batch, and returns the counts. Rows the set does
// not touch are copied byte for byte, so their hashes stay as they were.
// With a version column, a change older than the stored row is dropped and
// the stored row kept: a source can replay an old export after a newer one.
// With dense columns (a Dense table), each previous row written lacking one
// of them gets `,"<name>":null` appended before its closing brace, so every
// row carries every column; padding is not a change and no count moves. The
// upserted rows are written as the source gave them.
//  1. Index the spooled changes by key, the last change to a key winning.
//  2. Copy each previous row the set neither changes nor deletes by column.
//  3. Write each upsert, an insert when its key was not there before.
func Merge(prevPath string, set *ChangeSet, w *Writer, batch *BatchWriter, version string, dense []string) (Counts, error) {
	key := w.key
	// Padding reads the fields pairOf parses, which it parses only for a keyed table.
	if len(dense) > 0 && len(key) == 0 {
		return Counts{}, errors.New("merge: dense columns need the table's key")
	}
	names := make([][]byte, len(dense))
	for i, name := range dense {
		var err error
		if names[i], err = encodeValue(name); err != nil {
			return Counts{}, fmt.Errorf("encode column %q: %w", name, err)
		}
	}
	// pad appends the dense columns line lacks. A valid line whose fields
	// parsed but that does not end in } is a JSON null, not a row.
	pad := func(line []byte, fields map[string]json.RawMessage) ([]byte, error) {
		var missing [][]byte
		for i, name := range dense {
			if _, ok := fields[name]; !ok {
				missing = append(missing, names[i])
			}
		}
		if len(missing) == 0 {
			return line, nil
		}
		end := bytes.LastIndexByte(line, '}')
		if end < 0 || len(bytes.TrimSpace(line[end+1:])) > 0 {
			return nil, errors.New("pad row: the row is not a JSON object")
		}
		out := slices.Clone(line[:end])
		for i, name := range missing {
			if i > 0 || len(fields) > 0 {
				out = append(out, ',')
			}
			out = append(append(out, name...), ":null"...)
		}
		return append(out, '}'), nil
	}
	if err := set.buf.Flush(); err != nil {
		return Counts{}, fmt.Errorf("flush change set: %w", err)
	}
	type change struct {
		order   int
		row     []byte
		fields  map[string]json.RawMessage
		delete  bool
		version json.RawMessage
	}
	last := map[[16]byte]*change{}
	if _, err := set.file.Seek(0, io.SeekStart); err != nil {
		return Counts{}, fmt.Errorf("rewind change set: %w", err)
	}
	scanner := bufio.NewScanner(set.file)
	scanner.Buffer(make([]byte, 0, 1<<16), maxSpoolLine)
	for order := 0; scanner.Scan(); order++ {
		var op spooled
		if err := json.Unmarshal(scanner.Bytes(), &op); err != nil {
			return Counts{}, fmt.Errorf("decode change: %w", err)
		}
		source := op.Row
		if op.Op == "delete" {
			source = op.Key
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(source, &fields); err != nil {
			return Counts{}, fmt.Errorf("decode change: %w", err)
		}
		values, err := keyValues(fields, key)
		if err != nil {
			return Counts{}, err
		}
		c := &change{order: order, row: op.Row, fields: fields, delete: op.Op == "delete", version: op.Version}
		if !c.delete && version != "" {
			c.version = fields[version]
		}
		last[hashKey(values)] = c
	}
	if err := scanner.Err(); err != nil {
		return Counts{}, fmt.Errorf("read change set: %w", err)
	}
	var counts Counts
	// existed holds the previous row hash of each key the set changes, and
	// stored the previous row itself where a version may keep it. The hash is
	// of the row as stored, unpadded: a change that differs from it only by
	// padding is an update, so the merge publishes the padded table.
	existed := map[[16]byte][8]byte{}
	type storedRow struct {
		line    []byte
		version json.RawMessage
	}
	stored := map[[16]byte]storedRow{}
	if prevPath != "" {
		err := eachLine(prevPath, func(line []byte) error {
			p, fields, err := pairOf(line, key)
			if err != nil {
				return err
			}
			if _, ok := last[p.Key]; ok {
				existed[p.Key] = p.Row
				if version != "" {
					padded, err := pad(line, fields)
					if err != nil {
						return err
					}
					stored[p.Key] = storedRow{line: slices.Clone(padded), version: fields[version]}
				}
				return nil
			}
			for _, dw := range set.where {
				v, err := canonical(fields[dw.column])
				if err != nil {
					return err
				}
				if bytes.Equal(v, dw.value) {
					counts.Deletes++
					return batch.delete(fields, key)
				}
			}
			padded, err := pad(line, fields)
			if err != nil {
				return err
			}
			return w.WriteJSON(padded)
		})
		if err != nil {
			return Counts{}, err
		}
	}
	ordered := make([]*change, 0, len(last))
	for _, c := range last {
		ordered = append(ordered, c)
	}
	slices.SortFunc(ordered, func(a, b *change) int { return a.order - b.order })
	for _, c := range ordered {
		values, err := keyValues(c.fields, key)
		if err != nil {
			return Counts{}, err
		}
		k := hashKey(values)
		prev, had := existed[k]
		if s, ok := stored[k]; ok && olderVersion(c.version, s.version) {
			if err := w.WriteJSON(s.line); err != nil {
				return Counts{}, err
			}
			continue
		}
		if c.delete {
			if had {
				counts.Deletes++
				if err := batch.delete(c.fields, key); err != nil {
					return Counts{}, err
				}
			}
			continue
		}
		if err := w.WriteJSON(c.row); err != nil {
			return Counts{}, err
		}
		// A row read again unchanged, as an overlap or a re-fetch does, is no change.
		switch {
		case had && prev == hashRow(c.row):
			continue
		case had:
			counts.Updates++
		default:
			counts.Inserts++
		}
		if err := batch.upsert(c.fields, key, c.row); err != nil {
			return Counts{}, err
		}
	}
	return counts, nil
}

// olderVersion reports a change version below the stored one. A missing or
// non-numeric version on either side never counts as older.
func olderVersion(change, stored json.RawMessage) bool {
	a, errA := strconv.ParseFloat(string(change), 64)
	b, errB := strconv.ParseFloat(string(stored), 64)
	return errA == nil && errB == nil && a < b
}

// maxSpoolLine bounds one spooled change; a row past it is refused.
const maxSpoolLine = 64 << 20

// MergeBatches writes the change batches at keys, oldest first, to dst as one
// uncompressed file of the same lines: each key's last change, once, in the
// order keys first changed. It returns how many lines it wrote.
func MergeBatches(ctx context.Context, store Store, keys []string, dst string) (int, error) {
	last := map[string][]byte{}
	var order []string
	for _, key := range keys {
		path, err := Download(ctx, store, key, filepath.Dir(dst))
		if err != nil {
			return 0, err
		}
		err = eachLine(path, func(line []byte) error {
			var change struct {
				Key json.RawMessage `json:"key"`
			}
			if err := json.Unmarshal(line, &change); err != nil || len(change.Key) == 0 {
				return fmt.Errorf("change batch %s holds a line with no key", key)
			}
			k := string(change.Key)
			if _, seen := last[k]; !seen {
				order = append(order, k)
			}
			last[k] = slices.Clone(line)
			return nil
		})
		_ = os.Remove(path) // best-effort: a temp copy
		if err != nil {
			return 0, err
		}
	}
	f, err := os.Create(dst)
	if err != nil {
		return 0, fmt.Errorf("create change file: %w", err)
	}
	w := bufio.NewWriterSize(f, 1<<16)
	for _, k := range order {
		_, _ = w.Write(last[k]) // bufio keeps the first error for WriteByte to return
		if err := w.WriteByte('\n'); err != nil {
			_ = f.Close()
			return 0, fmt.Errorf("write change file: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("write change file: %w", err)
	}
	return len(order), f.Close()
}
