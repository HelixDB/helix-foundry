// Package document keeps Foundry documents in the Foundry data bucket. The
// browser PUTs a document's bytes straight to S3 through a presigned URL that
// signs their length, type and SHA-256, so the bytes never pass through the
// backend; completing the upload reads them back and checks all three. The
// document worker writes what reading a table file or a PDF gave beside it,
// once, and the graph worker saves each document's extraction artifacts
// there too.
// Deleting a document removes everything under its prefix.
//
// Layout, under foundry.sync.data_bucket:
//
//	ws/<workspace>/documents/<document>/original
//	ws/<workspace>/documents/<document>/read/rows.jsonl.gz
//	ws/<workspace>/documents/<document>/read/<mode>-<text sha256>.txt
//	ws/<workspace>/documents/<document>/extractions/<artifact key>.json.gz
package document

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// ErrMissing is an object S3 does not hold.
var ErrMissing = errors.New("document: no such object")

// ErrMismatch is stored bytes that differ from what was declared.
var ErrMismatch = errors.New("document: the stored object differs from the declared one")

// Prefix is everything of a document: its original, what reading it gave,
// and its extraction artifacts.
func Prefix(workspaceID, documentID string) string {
	return fmt.Sprintf("ws/%s/documents/%s/", workspaceID, documentID)
}

// Key is where a document's original lives.
func Key(workspaceID, documentID string) string {
	return Prefix(workspaceID, documentID) + "original"
}

// ReadPrefix is where what reading a document gave lives.
func ReadPrefix(workspaceID, documentID string) string {
	return Prefix(workspaceID, documentID) + "read/"
}

// RowsKey is where a table file's rows live once read: gzipped JSON Lines,
// one object per row with every column.
func RowsKey(workspaceID, documentID string) string {
	return ReadPrefix(workspaceID, documentID) + "rows.jsonl.gz"
}

// TextKey is where a PDF's text lives once read in mode: UTF-8 plain text,
// its pages split by form feeds. The key holds the text's SHA-256, so each
// text written has an object of its own and a read records the object it
// wrote.
func TextKey(workspaceID, documentID string, mode model.FoundryFileReadMode, sha256 string) string {
	return ReadPrefix(workspaceID, documentID) + string(mode) + "-" + sha256 + ".txt"
}

// ArtifactPrefix is where a document's extraction artifacts live.
func ArtifactPrefix(workspaceID, documentID string) string {
	return Prefix(workspaceID, documentID) + "extractions/"
}

// artifactKeyPattern is what an artifact key may be: the agent hashes what the
// output depends on, and the key names an S3 object and a local file.
var artifactKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ArtifactKey is where the artifact with the agent's key lives; false for a key
// that can't name a file.
func ArtifactKey(workspaceID, documentID, key string) (string, bool) {
	if !artifactKeyPattern.MatchString(key) {
		return "", false
	}
	return ArtifactPrefix(workspaceID, documentID) + key + ".json.gz", true
}

// MediaTypeOf is the format a file name's extension says, in any case, which
// the declared type must agree with; false for an extension Foundry does not
// store. A rename may change the extension later; the stored type decides.
//
//	.md, .markdown   text/markdown
//	.mdx             text/mdx
//	.txt             text/plain
//	.json            application/json
//	.csv             text/csv
//	.tsv             text/tab-separated-values
//	.jsonl, .ndjson  application/x-ndjson
//	.parquet         application/vnd.apache.parquet
//	.pdf             application/pdf
func MediaTypeOf(name string) (model.FoundryMediaType, bool) {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown":
		return model.FoundryMediaMarkdown, true
	case ".mdx":
		return model.FoundryMediaMDX, true
	case ".txt":
		return model.FoundryMediaPlainText, true
	case ".json":
		return model.FoundryMediaJSON, true
	case ".csv":
		return model.FoundryMediaCSV, true
	case ".tsv":
		return model.FoundryMediaTSV, true
	case ".jsonl", ".ndjson":
		return model.FoundryMediaJSONL, true
	case ".parquet":
		return model.FoundryMediaParquet, true
	case ".pdf":
		return model.FoundryMediaPDF, true
	}
	return "", false
}

// ValidName reports a display name of 1 to 255 characters with no path
// separator or control character.
func ValidName(name string) bool {
	if name == "" || utf8.RuneCountInString(name) > 255 || !utf8.ValidString(name) || strings.TrimSpace(name) != name {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool { return r == '/' || r == '\\' || unicode.IsControl(r) })
}

// ValidSHA256 reports a lowercase hex SHA-256 digest.
func ValidSHA256(sum string) bool {
	if len(sum) != sha256.Size*2 || strings.ToLower(sum) != sum {
		return false
	}
	_, err := hex.DecodeString(sum)
	return err == nil
}

// Upload is a presigned PUT: the browser sends the bytes to URL with Method and
// Headers; the browser adds Content-Length itself, which is signed too.
type Upload struct {
	URL       string
	Method    string
	Headers   map[string]string
	ExpiresAt time.Time
}

// Declared is what an upload promised: the size, type and SHA-256 its PUT signed.
type Declared struct {
	Size      int64
	MediaType model.FoundryMediaType
	SHA256    string
}

type s3API interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// Store keeps documents and their artifacts in one bucket.
type Store struct {
	api       s3API
	presigner *s3.PresignClient
	bucket    string
	now       func() time.Time
}

// New is a Store of bucket. Presigning signs locally with the client's
// credentials; it makes no request.
func New(client *s3.Client, bucket string) *Store {
	return &Store{api: client, presigner: s3.NewPresignClient(client), bucket: bucket, now: time.Now}
}

// PresignPut signs a PUT of the declared bytes to key, valid for ttl. S3 refuses
// a body of another length, type or SHA-256.
func (s *Store) PresignPut(ctx context.Context, key string, d Declared, ttl time.Duration) (Upload, error) {
	sum, err := hex.DecodeString(d.SHA256)
	if err != nil || len(sum) != sha256.Size {
		return Upload{}, fmt.Errorf("presign %s: %q is not a SHA-256 digest", key, d.SHA256)
	}
	expires := s.now().Add(ttl)
	req, err := s.presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), ContentType: aws.String(string(d.MediaType)),
		ContentLength: aws.Int64(d.Size), ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(sum)),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return Upload{}, fmt.Errorf("presign s3://%s/%s: %w", s.bucket, key, err)
	}
	headers := map[string]string{}
	for name, values := range req.SignedHeader {
		// The browser sets these itself and refuses to have them set.
		if canonical := http.CanonicalHeaderKey(name); canonical != "Host" && canonical != "Content-Length" && len(values) > 0 {
			headers[canonical] = values[0]
		}
	}
	return Upload{URL: req.URL, Method: req.Method, Headers: headers, ExpiresAt: expires}, nil
}

// Verify reads back what was uploaded to key and checks it is the declared
// bytes: ErrMissing when nothing was uploaded, ErrMismatch when they differ.
// It reads at most the declared size plus one byte.
func (s *Store) Verify(ctx context.Context, key string, d Declared) error {
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if notFound(err) {
		return ErrMissing
	}
	if err != nil {
		return fmt.Errorf("get s3://%s/%s: %w", s.bucket, key, err)
	}
	defer func() { _ = out.Body.Close() }() // best-effort: only read
	if got := aws.ToString(out.ContentType); got != string(d.MediaType) {
		return fmt.Errorf("%w: its type is %q, not %q", ErrMismatch, got, d.MediaType)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(out.Body, d.Size+1))
	if err != nil {
		return fmt.Errorf("read s3://%s/%s: %w", s.bucket, key, err)
	}
	if n != d.Size {
		return fmt.Errorf("%w: it has %d bytes, not %d", ErrMismatch, n, d.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != d.SHA256 {
		return fmt.Errorf("%w: its SHA-256 is %s, not %s", ErrMismatch, got, d.SHA256)
	}
	return nil
}

// Open reads an object.
func (s *Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if notFound(err) {
		return nil, fmt.Errorf("get s3://%s/%s: %w", s.bucket, key, ErrMissing)
	}
	if err != nil {
		return nil, fmt.Errorf("get s3://%s/%s: %w", s.bucket, key, err)
	}
	return out.Body, nil
}

// Delete removes an object; a missing one is no error.
func (s *Store) Delete(ctx context.Context, key string) error {
	if _, err := s.api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}); err != nil && !notFound(err) {
		return fmt.Errorf("delete s3://%s/%s: %w", s.bucket, key, err)
	}
	return nil
}

// PutOnce stores body under key unless an object is there already, which it
// leaves as it is: what is written there never changes, so a second write of
// it, such as a retry's, is the same bytes. body is read whole; a file is
// seekable, so it is sent with its length.
func (s *Store) PutOnce(ctx context.Context, key, contentType string, body io.ReadSeeker) error {
	_, err := s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), ContentType: aws.String(contentType), Body: body, IfNoneMatch: aws.String("*"),
	})
	var api smithy.APIError
	if errors.As(err, &api) && api.ErrorCode() == "PreconditionFailed" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("put s3://%s/%s: %w", s.bucket, key, err)
	}
	return nil
}

// DeletePrefix removes every object under prefix.
func (s *Store) DeletePrefix(ctx context.Context, prefix string) error {
	keys, err := s.List(ctx, prefix)
	if err != nil {
		return err
	}
	var errs []error
	for _, key := range keys {
		errs = append(errs, s.Delete(ctx, key))
	}
	return errors.Join(errs...)
}

// PutArtifact stores an artifact's JSON gzipped under key.
func (s *Store) PutArtifact(ctx context.Context, key string, artifact []byte) error {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(artifact); err != nil {
		return fmt.Errorf("compress artifact: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("compress artifact: %w", err)
	}
	if _, err := s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(gz.Bytes()),
		ContentType: aws.String("application/json"), ContentEncoding: aws.String("gzip"),
	}); err != nil {
		return fmt.Errorf("put s3://%s/%s: %w", s.bucket, key, err)
	}
	return nil
}

// OpenArtifact reads an artifact's JSON.
func (s *Store) OpenArtifact(ctx context.Context, key string) ([]byte, error) {
	body, err := s.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }() // best-effort: only read
	gz, err := gzip.NewReader(body)
	if err != nil {
		return nil, fmt.Errorf("read artifact %s: %w", key, err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		return nil, fmt.Errorf("read artifact %s: %w", key, err)
	}
	return raw, nil
}

// List returns every key under prefix.
func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	var token *string
	for {
		out, err := s.api.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(s.bucket), Prefix: aws.String(prefix), ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list s3://%s/%s: %w", s.bucket, prefix, err)
		}
		for _, obj := range out.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}
		if !aws.ToBool(out.IsTruncated) {
			return keys, nil
		}
		token = out.NextContinuationToken
	}
}

// notFound reports S3's answer for a missing key. S3 gives that answer (404)
// only to a role that may list the bucket; any other gets 403 AccessDenied,
// which is a failure here, never a missing object: the roles reading
// documents are granted s3:ListBucket for that reason.
func notFound(err error) bool {
	var noKey *s3types.NoSuchKey
	var notFound *s3types.NotFound
	var api smithy.APIError
	return errors.As(err, &noKey) || errors.As(err, &notFound) || errors.As(err, &api) && api.ErrorCode() == "NoSuchKey"
}
