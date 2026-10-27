// Package read reads a Foundry document once, at upload, and stores what it
// gave beside it, where loads and generations find it: what a table file
// holds, as rows the ontology engine reads. The foundry-document-worker
// claims each read and records its outcome; a Reader does one kind.
//
// A table file is one table whose source id is its document id. CSV, TSV and
// Parquet are converted once, by filesource, to gzipped JSON Lines at
// document.RowsKey, which gives typed values, delimiter and charset
// sniffing, and Parquet. JSON Lines and JSON are checked as the engine reads
// them, then sent as they are: the engine catalogs a JSON document's lists
// of records itself, and mappings name them by pointer.
//
// Every read is bounded (DocumentLimits), refuses compressed files, and
// never changes once stored: a change here reads only new uploads.
package read

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/helixdb/helix-foundry/backend/filesource"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Reader reads one kind of document.
type Reader interface {
	// Read reads doc, stores what it gave, and returns the outcome to
	// record: what it read, or model.FoundryFileUnreadable for a file it
	// can't read, for good. An error is a failure that is not the file's,
	// such as storage's or ctx's: reading it again may get past it.
	Read(ctx context.Context, doc *model.FoundryDocument) (model.FoundryFileReadOutcome, error)
}

// Store is where documents live (document.Store).
type Store interface {
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	PutOnce(ctx context.Context, key, contentType string, body io.ReadSeeker) error
}

// DocumentLimits bound reading one uploaded table file, well within the
// document worker's pod: 64 MiB to read a Parquet row group, 1 MiB a record
// or CSV line, 256 MiB decompressed (and as converted rows), 1000 columns,
// and nesting 64 deep (the engine's JSON parser stops at 128). Compressed
// files are refused before any of it.
var DocumentLimits = filesource.Limits{
	MaxColumns:           1000,
	MaxDepth:             64,
	MaxRecordBytes:       1 << 20,
	MaxDecompressedBytes: 256 << 20,
	MaxWindow:            8 << 20,
	MaxRowGroupBytes:     64 << 20,
}

// unreadable fails a read for good, with what to fix.
func unreadable(code model.FoundryFileReadErrorCode, format string, args ...any) model.FoundryFileUnreadable {
	return model.FoundryFileUnreadable{Error: model.FoundryFileReadError{Code: code, Message: fmt.Sprintf(format, args...)}}
}

// fromFilesource is the outcome of unreadable data filesource reported:
// past a limit is too_large, anything else unsupported. Any other error is
// not the file's, and nil.
func fromFilesource(err error) (model.FoundryFileUnreadable, bool) {
	var unreadableData *filesource.Error
	if !errors.As(err, &unreadableData) {
		return model.FoundryFileUnreadable{}, false
	}
	code := model.FoundryFileReadUnsupported
	for _, limit := range []error{filesource.ErrTooLarge, filesource.ErrRecordTooLarge, filesource.ErrTooManyColumns, filesource.ErrTooDeep} {
		if errors.Is(err, limit) {
			code = model.FoundryFileReadTooLarge
		}
	}
	return unreadable(code, "%s", err.Error()), true
}
