package connector

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/helixdb/helix-foundry/backend/filesource"
)

// fileCursor is where a file source's change reads start: per table, the
// files its snapshot was read from.
type fileCursor struct {
	Tables map[string]fileList `json:"tables"`
}

// fileList is a table's files as sorted 8-byte hashes of key, ETag and size,
// so the 10,000 files a listing may hold stay near 110 KB; Last is the
// greatest key.
type fileList struct {
	Files string `json:"files"`
	Last  string `json:"last"`
}

func fileHash(o Object) uint64 {
	sum := sha256.Sum256([]byte(o.Key + "\x00" + o.ETag + "\x00" + strconv.FormatInt(o.Size, 10)))
	return binary.BigEndian.Uint64(sum[:8])
}

func listOf(files []Object) fileList {
	var l fileList
	hashes := make([]uint64, len(files))
	for i, o := range files {
		hashes[i] = fileHash(o)
		l.Last = max(l.Last, o.Key)
	}
	slices.Sort(hashes)
	raw := make([]byte, 0, 8*len(hashes))
	for _, h := range hashes {
		raw = binary.BigEndian.AppendUint64(raw, h)
	}
	l.Files = base64.RawStdEncoding.EncodeToString(raw)
	return l
}

// appended returns the files added since l was taken, or why the table must
// be read in full. A new file must sort after every earlier one: a full read
// takes files in key order, so one among them can reorder the columns.
func (l fileList) appended(files []Object) ([]Object, string) {
	raw, err := base64.RawStdEncoding.DecodeString(l.Files)
	if err != nil || len(raw)%8 != 0 {
		return nil, "the stored file list is unreadable"
	}
	earlier := make(map[uint64]bool, len(raw)/8)
	for i := 0; i < len(raw); i += 8 {
		earlier[binary.BigEndian.Uint64(raw[i:])] = true
	}
	var added []Object
	for _, o := range files {
		// Without an ETag a rewrite of the same size looks unchanged.
		if o.ETag == "" {
			return nil, "the store lists files without ETags"
		}
		h := fileHash(o)
		switch {
		case earlier[h]:
			delete(earlier, h)
		case o.Key <= l.Last:
			return nil, o.Key + " is new or rewritten among the files read before"
		default:
			added = append(added, o)
		}
	}
	if len(earlier) > 0 {
		return nil, "a file read before was rewritten or removed"
	}
	return added, ""
}

func decodeFileCursor(raw json.RawMessage) (fileCursor, error) {
	var c fileCursor
	if err := json.Unmarshal(raw, &c); err != nil || c.Tables == nil {
		return c, fmt.Errorf("%w: unreadable file list", ErrCursorExpired)
	}
	return c, nil
}

// fileStart takes the file lists a full read of tables begins from; Discover
// listed them, and Sync reads exactly those files.
func fileStart(tables []Table) (Start, error) {
	c := fileCursor{Tables: make(map[string]fileList, len(tables))}
	for _, t := range tables {
		c.Tables[t.Name] = listOf(t.Files)
	}
	raw, err := json.Marshal(c)
	return Start{Cursor: raw}, err
}

// Capture follows every table by its file list. A table without a key is
// read in full whenever its list changes, which only Changes can tell.
func (k *s3Kind) Capture(Source, string) Capture { return CaptureCursor }

func (k *s3Kind) Start(_ context.Context, _ Source, tables []Table) (Start, error) {
	return fileStart(tables)
}

// Changes compares each table's files, as this sync's Discover listed them,
// with the cursor's: an unchanged list writes nothing, and files appended
// after the rest are read alone, their rows upserted by id. Anything else,
// or a table without the columns and key its snapshot was written with,
// reads the table in full.
func (k *s3Kind) Changes(ctx context.Context, src Source, tables []Table, cursor json.RawMessage, w ChangeWriter) (json.RawMessage, error) {
	prev, err := decodeFileCursor(cursor)
	if err != nil {
		return nil, err
	}
	s, err := parseS3Settings(src.Settings)
	if err != nil {
		return nil, err
	}
	next := fileCursor{Tables: make(map[string]fileList, len(tables))}
	var sess *s3Session
	for _, t := range tables {
		next.Tables[t.Name] = listOf(t.Files)
		list, ok := prev.Tables[t.Name]
		if !ok {
			w.Reset(t.Name, "no file list yet")
			continue
		}
		added, reason := list.appended(t.Files)
		if reason == "" && len(added) > 0 {
			if sess == nil {
				// Discover proved the role's trust for this sync.
				if sess, err = k.open(ctx, s, src, false); err != nil {
					return nil, err
				}
			}
			if reason, err = k.appendRows(ctx, sess, t, added, w); err != nil {
				return nil, err
			}
		}
		if reason != "" {
			w.Reset(t.Name, reason)
		}
	}
	return json.Marshal(next)
}

// appendRows upserts the rows of the files appended to t, each as a full
// read writes it, or says why it can't.
func (k *s3Kind) appendRows(ctx context.Context, sess *s3Session, t Table, added []Object, w ChangeWriter) (string, error) {
	// A keyless table's rows can't be merged by key.
	if !slices.Equal(t.PrimaryKey, []string{"id"}) || len(t.Columns) == 0 {
		return "the table has no key or no stored columns", nil
	}
	columns := make([]filesource.Column, len(t.Columns))
	for i, c := range t.Columns {
		typ, ok := filesource.ParseType(c.Type)
		if !ok {
			return "column " + c.Name + " has no stored type", nil
		}
		columns[i] = filesource.Column{Name: c.Name, Type: typ, Nullable: c.Nullable}
	}
	dir, err := os.MkdirTemp(k.opts.TempDir, "foundry-s3-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }() // best-effort: the worker sweeps leftovers when it starts
	files, err := k.download(ctx, sess, Table{Name: t.Name, Files: added}, dir)
	if err != nil {
		return "", err
	}
	isAdded := make(map[string]bool, len(added))
	for _, o := range added {
		isAdded[o.Key] = true
	}
	var earlier []filesource.File
	for _, o := range t.Files {
		if !isAdded[o.Key] {
			earlier = append(earlier, filesource.File{Name: o.Key, Format: o.Format,
				Partitions: filesource.Partitions(strings.TrimPrefix(o.Key, sess.settings.Prefix))})
		}
	}
	lim := filesource.DefaultLimits
	lim.MaxDecompressedBytes = 2 * k.opts.MaxTableBytes
	schema, err := filesource.Append(ctx, columns, t.PrimaryKey, earlier, files, lim)
	if errors.Is(err, filesource.ErrAppendChanged) {
		return "the new files change the table's columns or key", nil
	}
	if err != nil {
		return "", readFailure(err, files)
	}
	for row, err := range schema.Rows(ctx) {
		if err != nil {
			return "", readFailure(err, files)
		}
		if err := w.UpsertJSON(t.Name, row); err != nil {
			return "", err
		}
	}
	countRead(schema, files)
	return "", nil
}
