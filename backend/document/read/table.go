package read

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/filesource"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Tables reads table files (model.FoundryReadTable).
type Tables struct {
	Store Store
	// TempDir holds a file and its converted rows while it is read; "" is
	// os.TempDir.
	TempDir string
	Limits  filesource.Limits
}

// Read reads a table file:
//  1. Download its original to TempDir. A file whose first bytes show
//     compression is refused: filesource would decompress it, past the size
//     its upload was held to.
//  2. CSV, TSV and Parquet: filesource scans it under Limits and writes
//     every row, with every column, to gzipped JSON Lines, at most
//     Limits.MaxDecompressedBytes of them, stored once at document.RowsKey.
//  3. JSON Lines and JSON: checked as the engine reads them (CheckJSONLines,
//     CheckJSON); the original is what loads read.
//
// Unreadable data is model.FoundryFileUnreadable: too_large past a limit,
// unsupported otherwise. A file whose original is gone is unsupported too.
func (t *Tables) Read(ctx context.Context, doc *model.FoundryDocument) (model.FoundryFileReadOutcome, error) {
	if doc.MediaType.ReadKind() != model.FoundryReadTable {
		return nil, fmt.Errorf("read: %s is not a table file but %s", doc.ID, doc.MediaType)
	}
	original, err := t.download(ctx, doc)
	if errors.Is(err, document.ErrMissing) {
		return unreadable(model.FoundryFileReadUnsupported, "the file's bytes are gone"), nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(original) }() // best-effort: the worker sweeps TempDir at start
	head := make([]byte, 16)
	f, err := os.Open(original)
	if err != nil {
		return nil, err
	}
	n, _ := io.ReadFull(f, head) // a shorter file is all head
	_ = f.Close()                // best-effort: only read
	if c := filesource.Compression(head[:n]); c != "" {
		return unreadable(model.FoundryFileReadUnsupported, "it is compressed with %s; upload the file uncompressed", c), nil
	}
	switch doc.MediaType {
	case model.FoundryMediaJSONL, model.FoundryMediaJSON:
		data, err := os.ReadFile(original)
		if err != nil {
			return nil, err
		}
		var records int64
		var bad *model.FoundryFileUnreadable
		if doc.MediaType == model.FoundryMediaJSONL {
			records, bad = CheckJSONLines(data, t.Limits.MaxDepth, t.Limits.MaxRecordBytes)
		} else {
			records, bad = CheckJSON(data, t.Limits.MaxDepth)
		}
		if bad != nil {
			return *bad, nil
		}
		return model.FoundryTableRead{ArtifactKey: doc.S3Key, Rows: records}, nil
	case model.FoundryMediaCSV, model.FoundryMediaTSV, model.FoundryMediaParquet:
		return t.convert(ctx, doc, original)
	}
	return nil, fmt.Errorf("read: no table reader for %s", doc.MediaType)
}

// download copies doc's original to a file of its own under TempDir, at
// most the size its upload declared.
func (t *Tables) download(ctx context.Context, doc *model.FoundryDocument) (string, error) {
	body, err := t.Store.Open(ctx, doc.S3Key)
	if err != nil {
		return "", err
	}
	defer func() { _ = body.Close() }() // best-effort: only read
	f, err := os.CreateTemp(t.TempDir, "foundry-read-*")
	if err != nil {
		return "", err
	}
	n, err := io.Copy(f, io.LimitReader(body, doc.SizeBytes+1))
	err = errors.Join(err, f.Close())
	if err == nil && n != doc.SizeBytes {
		err = fmt.Errorf("read: document %s holds %d bytes, not the %d uploaded", doc.ID, n, doc.SizeBytes)
	}
	if err != nil {
		_ = os.Remove(f.Name()) // best-effort: the worker sweeps TempDir at start
		return "", err
	}
	return f.Name(), nil
}

// convert writes a CSV, TSV or Parquet file's rows to document.RowsKey.
func (t *Tables) convert(ctx context.Context, doc *model.FoundryDocument, original string) (model.FoundryFileReadOutcome, error) {
	format := filesource.FormatParquet
	ext := ".parquet"
	switch doc.MediaType {
	case model.FoundryMediaCSV:
		format, ext = filesource.FormatCSV, ".csv"
	case model.FoundryMediaTSV:
		format, ext = filesource.FormatCSV, ".tsv"
	}
	// filesource reads tabs from a .tsv name and names the file in its
	// messages; a rename may have changed the extension the type kept.
	name := doc.Name
	if typ, _ := document.MediaTypeOf(name); typ != doc.MediaType || path.Ext(name) == "" {
		name += ext
	}
	schema, err := filesource.Scan(ctx, []filesource.File{{Path: original, Name: name, Format: format}}, t.Limits)
	if bad, ok := fromFilesource(err); ok {
		return bad, nil
	}
	if err != nil {
		return nil, err
	}
	out, err := os.CreateTemp(t.TempDir, "foundry-rows-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = out.Close()           // best-effort: closed below when stored
		_ = os.Remove(out.Name()) // best-effort: the worker sweeps TempDir at start
	}()
	rows, outcome, err := t.writeRows(ctx, schema, out)
	if outcome != nil || err != nil {
		return outcome, err
	}
	if _, err := out.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	key := document.RowsKey(doc.WorkspaceID, doc.ID)
	if err := t.Store.PutOnce(ctx, key, "application/gzip", out); err != nil {
		return nil, err
	}
	return model.FoundryTableRead{ArtifactKey: key, Rows: rows}, nil
}

// writeRows writes schema's rows to out as gzipped JSON Lines. Rows past
// Limits.MaxDecompressedBytes are a file too large: a narrow file of empty
// cells under long column names grows many times over as JSON.
func (t *Tables) writeRows(ctx context.Context, schema *filesource.Schema, out io.Writer) (int64, model.FoundryFileReadOutcome, error) {
	gz := gzip.NewWriter(out)
	buf := bufio.NewWriter(gz)
	var rows, written int64
	for row, err := range schema.Rows(ctx) {
		if bad, ok := fromFilesource(err); ok {
			return 0, bad, nil
		}
		if err != nil {
			return 0, nil, err
		}
		if written += int64(len(row)) + 1; written > t.Limits.MaxDecompressedBytes {
			return 0, unreadable(model.FoundryFileReadTooLarge, "its rows come to more than %d MiB as JSON", t.Limits.MaxDecompressedBytes>>20), nil
		}
		if _, err := buf.Write(row); err != nil {
			return 0, nil, err
		}
		if err := buf.WriteByte('\n'); err != nil {
			return 0, nil, err
		}
		rows++
	}
	if err := buf.Flush(); err != nil {
		return 0, nil, err
	}
	return rows, nil, gz.Close()
}
