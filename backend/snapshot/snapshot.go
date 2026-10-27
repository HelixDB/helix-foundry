package snapshot

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// ErrRowLimit is a table with more rows than one snapshot may hold.
var ErrRowLimit = errors.New("snapshot: row limit reached")

// ErrMissing is an object the bucket does not hold. S3 says so (404) only to
// a role that may list the bucket; any other gets 403, which is an error.
var ErrMissing = errors.New("snapshot: not found")

// ErrByteLimit is a table whose rows, uncompressed, are more than one
// snapshot may hold: the generation and graph workers unpack it whole.
var ErrByteLimit = errors.New("snapshot: byte limit reached")

// Store keeps immutable snapshot objects.
type Store interface {
	Put(ctx context.Context, key string, body io.ReadSeeker) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// BatchKey is where one change batch of a table lives, beside its snapshots.
func BatchKey(workspaceID, connectorID, table, batchID string) string {
	return fmt.Sprintf("ws/%s/connectors/%s/%s/changes/%s.jsonl.gz", workspaceID, connectorID, url.PathEscape(table), batchID)
}

// Key is where one upload of a table lives; each upload gets a new objectID so
// it never overwrites a published snapshot. Table names are escaped because a
// database table name can hold a slash.
func Key(workspaceID, connectorID, table, objectID string) string {
	return fmt.Sprintf("ws/%s/connectors/%s/%s/%s.jsonl.gz", workspaceID, connectorID, url.PathEscape(table), objectID)
}

type s3API interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(context.Context, *s3.DeleteObjectsInput, ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// S3 stores snapshots in one bucket.
type S3 struct {
	api    s3API
	bucket string
}

func NewS3(api s3API, bucket string) *S3 { return &S3{api: api, bucket: bucket} }

func (s *S3) Put(ctx context.Context, key string, body io.ReadSeeker) error {
	_, err := s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), Body: body,
		ContentType: aws.String("application/x-ndjson"), ContentEncoding: aws.String("gzip"),
	})
	if err != nil {
		return fmt.Errorf("put s3://%s/%s: %w", s.bucket, key, err)
	}
	return nil
}

func (s *S3) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.get(ctx, key, nil)
}

// Tail reads the last n bytes of an object with one ranged GET, or all of an
// object shorter than n: a gzip file's trailer without its body. A store that
// ignores the range is refused rather than read to the end.
func (s *S3) Tail(ctx context.Context, key string, n int64) ([]byte, error) {
	if n <= 0 {
		return nil, fmt.Errorf("tail s3://%s/%s: n is %d, want above zero", s.bucket, key, n)
	}
	body, err := s.get(ctx, key, aws.String(fmt.Sprintf("bytes=-%d", n)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }() // best-effort: only read
	tail, err := io.ReadAll(io.LimitReader(body, n+1))
	if err != nil {
		return nil, fmt.Errorf("get s3://%s/%s: %w", s.bucket, key, err)
	}
	if int64(len(tail)) > n {
		return nil, fmt.Errorf("get s3://%s/%s: the store ignored the range bytes=-%d", s.bucket, key, n)
	}
	return tail, nil
}

// get reads an object, or only byteRange of it when set.
func (s *S3) get(ctx context.Context, key string, byteRange *string) (io.ReadCloser, error) {
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Range: byteRange})
	var noKey *types.NoSuchKey
	var notFound *types.NotFound
	var api smithy.APIError
	if errors.As(err, &noKey) || errors.As(err, &notFound) || errors.As(err, &api) && api.ErrorCode() == "NoSuchKey" {
		return nil, fmt.Errorf("get s3://%s/%s: %w", s.bucket, key, ErrMissing)
	}
	if err != nil {
		return nil, fmt.Errorf("get s3://%s/%s: %w", s.bucket, key, err)
	}
	return out.Body, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if _, err := s.api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}); err != nil {
		return fmt.Errorf("delete s3://%s/%s: %w", s.bucket, key, err)
	}
	return nil
}

// DeletePrefix deletes every object under prefix, one listed page at a time.
// The prefix must name a folder, ending in "/": "" would empty the bucket,
// and "a/b" would also take "a/bc/". A key S3 refuses fails the call, once
// the rest of its page is deleted.
func (s *S3) DeletePrefix(ctx context.Context, prefix string) error {
	if !strings.HasSuffix(prefix, "/") || strings.Trim(prefix, "/") == "" {
		return fmt.Errorf("delete s3://%s/%s: the prefix must name a folder, ending in /", s.bucket, prefix)
	}
	pages := s3.NewListObjectsV2Paginator(s.api, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list s3://%s/%s: %w", s.bucket, prefix, err)
		}
		if len(page.Contents) == 0 {
			continue
		}
		objects := make([]types.ObjectIdentifier, 0, len(page.Contents))
		for _, o := range page.Contents {
			objects = append(objects, types.ObjectIdentifier{Key: o.Key})
		}
		// A page holds at most 1,000 keys, the most one DeleteObjects takes.
		out, err := s.api.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket), Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fmt.Errorf("delete under s3://%s/%s: %w", s.bucket, prefix, err)
		}
		if len(out.Errors) > 0 {
			first := out.Errors[0]
			return fmt.Errorf("delete s3://%s/%s: %s: %s (%d of %d keys refused)", s.bucket, aws.ToString(first.Key),
				aws.ToString(first.Code), aws.ToString(first.Message), len(out.Errors), len(objects))
		}
	}
	return nil
}

// Writer collects one table's rows as gzipped NDJSON in a temporary file, so
// a table larger than memory still uploads as one object.
type Writer struct {
	file    *os.File
	gz      *gzip.Writer
	buf     *bufio.Writer
	rows    int64
	maxRows int64
	// bytes counts the uncompressed rows written, newlines included.
	bytes    int64
	maxBytes int64
	columns  []string
	seen     map[string]bool
	scratch  bytes.Buffer
	finished bool
	// key and pairs are set by Hash.
	key   []string
	pairs []Pair
}

// NewWriter opens a writer in dir (the system temp dir when empty) for at
// most maxRows rows and maxBytes uncompressed bytes.
func NewWriter(dir string, maxRows, maxBytes int64) (*Writer, error) {
	file, err := os.CreateTemp(dir, "foundry-snapshot-*.jsonl.gz")
	if err != nil {
		return nil, fmt.Errorf("create snapshot file: %w", err)
	}
	buf := bufio.NewWriterSize(file, 1<<16)
	return &Writer{file: file, buf: buf, gz: gzip.NewWriter(buf), maxRows: maxRows, maxBytes: maxBytes, seen: map[string]bool{}}, nil
}

// Write appends a row; its keys become the table's columns in the order first seen.
func (w *Writer) Write(row map[string]any) error {
	keys := make([]string, 0, len(row))
	for k := range row {
		if !w.seen[k] {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		w.seen[k] = true
		w.columns = append(w.columns, k)
	}
	w.scratch.Reset()
	enc := json.NewEncoder(&w.scratch)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(row); err != nil {
		return fmt.Errorf("encode row: %w", err)
	}
	// Encode ends the row with its newline.
	return w.append(w.scratch.Bytes(), nil)
}

// WriteJSON appends a row the source already encoded as one JSON object.
func (w *Writer) WriteJSON(row []byte) error { return w.append(row, newline) }

var newline = []byte{'\n'}

func (w *Writer) append(line, end []byte) error {
	if w.rows >= w.maxRows {
		return fmt.Errorf("%w of %d", ErrRowLimit, w.maxRows)
	}
	size := int64(len(line) + len(end))
	if w.bytes+size > w.maxBytes {
		return fmt.Errorf("%w of %d", ErrByteLimit, w.maxBytes)
	}
	w.rows++
	w.bytes += size
	if _, err := w.gz.Write(line); err != nil {
		return err
	}
	_, err := w.gz.Write(end)
	return err
}

func (w *Writer) Rows() int64 { return w.rows }

// Columns are the keys Write saw, for sources whose schema is only known from rows.
func (w *Writer) Columns() []string { return slices.Clone(w.columns) }

// finish completes the gzip stream; no row can be written after it.
func (w *Writer) finish() error {
	if w.finished {
		return nil
	}
	if err := w.gz.Close(); err != nil {
		return fmt.Errorf("finish snapshot: %w", err)
	}
	if err := w.buf.Flush(); err != nil {
		return fmt.Errorf("flush snapshot: %w", err)
	}
	w.finished = true
	return nil
}

// SetKey names the table's key columns before rows are merged in.
func (w *Writer) SetKey(key []string) { w.key = slices.Clone(key) }

// Hash finishes the file and hashes every row by key (none for a keyless
// table), for Digest and Compare. The key is known only once a source has
// been read whole, so hashing reads the finished file again.
func (w *Writer) Hash(key []string) ([]Pair, error) {
	if err := w.finish(); err != nil {
		return nil, err
	}
	w.key, w.pairs = slices.Clone(key), make([]Pair, 0, w.rows)
	err := eachLine(w.file.Name(), func(line []byte) error {
		p, _, err := pairOf(line, w.key)
		if err != nil {
			return err
		}
		w.pairs = append(w.pairs, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return w.pairs, nil
}

// Upload finishes the file and stores it under key.
func (w *Writer) Upload(ctx context.Context, store Store, key string) error {
	if err := w.finish(); err != nil {
		return err
	}
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind snapshot: %w", err)
	}
	return store.Put(ctx, key, w.file)
}

// Close removes the temporary file.
func (w *Writer) Close() error {
	_ = w.file.Close() // best-effort: the file is removed next either way
	return os.Remove(w.file.Name())
}

// Head decodes at most n rows from the start of a snapshot without reading the rest.
func Head(ctx context.Context, store Store, key string, n int) (_ []map[string]any, err error) {
	body, err := store.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }() // best-effort: only read
	gz, err := gzip.NewReader(body)
	if err != nil {
		return nil, fmt.Errorf("read snapshot %s: %w", key, err)
	}
	dec := json.NewDecoder(gz)
	dec.UseNumber()
	var rows []map[string]any
	for len(rows) < n {
		var row map[string]any
		if err := dec.Decode(&row); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode snapshot %s row %d: %w", key, len(rows)+1, err)
		}
		rows = append(rows, row)
	}
	return rows, nil
}
