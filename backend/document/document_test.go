package document

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// bucket is S3 in memory: objects by key, with their content type.
type bucket struct {
	mu       sync.Mutex
	objects  map[string][]byte
	types    map[string]string
	pageSize int
	// refused, when set, is every GetObject's answer, as S3 answers a role
	// it denies.
	refused error
	// undeletable are keys whose delete S3 refuses.
	undeletable map[string]bool
}

func newBucket() *bucket {
	return &bucket{objects: map[string][]byte{}, types: map[string]string{}, pageSize: 1000}
}

func (b *bucket) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.refused != nil {
		return nil, b.refused
	}
	data, ok := b.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &s3types.NoSuchKey{}
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(data)), ContentType: aws.String(b.types[aws.ToString(in.Key)])}, nil
}

func (b *bucket) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.objects[aws.ToString(in.Key)]; ok && aws.ToString(in.IfNoneMatch) == "*" {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "At least one of the pre-conditions you specified did not hold"}
	}
	b.objects[aws.ToString(in.Key)], b.types[aws.ToString(in.Key)] = data, aws.ToString(in.ContentType)
	return &s3.PutObjectOutput{}, nil
}

func (b *bucket) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.undeletable[aws.ToString(in.Key)] {
		return nil, &smithy.GenericAPIError{Code: "AccessDenied", Message: "Access Denied"}
	}
	delete(b.objects, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func (b *bucket) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var keys []string
	for k := range b.objects {
		if strings.HasPrefix(k, aws.ToString(in.Prefix)) && k > aws.ToString(in.ContinuationToken) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(len(keys) > b.pageSize)}
	if len(keys) > b.pageSize {
		keys = keys[:b.pageSize]
		out.NextContinuationToken = aws.String(keys[len(keys)-1])
	}
	for _, k := range keys {
		out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k)})
	}
	return out, nil
}

// newStore signs with static credentials, which presigning needs and never sends.
func newStore(b *bucket) *Store {
	client := s3.NewFromConfig(aws.Config{Region: "us-east-2", Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"}, nil
	})})
	s := New(client, "helix-test-foundry-data")
	s.api = b
	return s
}

func digest(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

const contract = "# Master services agreement\n\nAcme Corp buys from us.\n"

func TestPresignPut(t *testing.T) {
	ctx := context.Background()
	// S3 checks a presigned PUT against its signature, so the length, type and checksum must all be signed.
	t.Run("signs the length, type and checksum of the declared bytes", func(t *testing.T) {
		s := newStore(newBucket())
		s.now = func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC) }
		up, err := s.PresignPut(ctx, Key("ws-1", "doc-1"), Declared{Size: int64(len(contract)), MediaType: model.FoundryMediaMarkdown, SHA256: digest(contract)}, 15*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(up.URL)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		if up.Method != "PUT" || !strings.HasSuffix(u.Path, "/ws/ws-1/documents/doc-1/original") ||
			q.Get("X-Amz-SignedHeaders") != "content-length;content-type;host" || q.Get("X-Amz-Expires") != "900" ||
			q.Get("X-Amz-Checksum-Sha256") != "GwgwFXBrZG8KP7+7P1optqciWoqescOb7S3nJZH/4s0=" {
			t.Fatalf("presigned %s %s", up.Method, up.URL)
		}
		if len(up.Headers) != 1 || up.Headers["Content-Type"] != "text/markdown" {
			t.Fatalf("headers %v: the browser sends the type and sets the length itself", up.Headers)
		}
		if !up.ExpiresAt.Equal(time.Date(2026, 10, 18, 12, 15, 0, 0, time.UTC)) {
			t.Fatalf("expires %s", up.ExpiresAt)
		}
	})
	t.Run("refuses a checksum that is not a SHA-256", func(t *testing.T) {
		if _, err := newStore(newBucket()).PresignPut(ctx, "k", Declared{Size: 1, MediaType: model.FoundryMediaPlainText, SHA256: "abc"}, time.Minute); err == nil {
			t.Fatal("presigned")
		}
	})
}

func TestVerify(t *testing.T) {
	ctx := context.Background()
	declared := Declared{Size: int64(len(contract)), MediaType: model.FoundryMediaMarkdown, SHA256: digest(contract)}
	put := func(b *bucket, body, contentType string) {
		b.objects["k"], b.types["k"] = []byte(body), contentType
	}
	t.Run("accepts the declared bytes", func(t *testing.T) {
		b := newBucket()
		put(b, contract, "text/markdown")
		if err := newStore(b).Verify(ctx, "k", declared); err != nil {
			t.Fatal(err)
		}
	})
	// A binary document at the 1 MiB cap is checked the same way, streamed.
	t.Run("accepts a binary document at the cap", func(t *testing.T) {
		pdf := "%PDF-1.7\n" + strings.Repeat("\x00\xff", (1<<20-9)/2) + "\n"
		b := newBucket()
		put(b, pdf, "application/pdf")
		if err := newStore(b).Verify(ctx, "k", Declared{Size: 1 << 20, MediaType: model.FoundryMediaPDF, SHA256: digest(pdf)}); err != nil {
			t.Fatal(err)
		}
	})
	// Completing before the PUT landed is a client mistake, not a mismatch.
	t.Run("reports an upload that never arrived", func(t *testing.T) {
		if err := newStore(newBucket()).Verify(ctx, "k", declared); !errors.Is(err, ErrMissing) {
			t.Fatalf("err = %v", err)
		}
	})
	// S3 refuses a missing key with 403 to a role that can't list the bucket.
	// A refusal must never read as missing: the worker marks a document whose
	// original is missing deleted, and a refused role would lose them all.
	t.Run("reports a refused read as a failure, not as missing", func(t *testing.T) {
		b := newBucket()
		put(b, contract, "text/markdown")
		b.refused = &smithy.GenericAPIError{Code: "AccessDenied", Message: "Access Denied"}
		s := newStore(b)
		if err := s.Verify(ctx, "k", declared); err == nil || errors.Is(err, ErrMissing) {
			t.Fatalf("verify err = %v", err)
		}
		if _, err := s.Open(ctx, "k"); err == nil || errors.Is(err, ErrMissing) {
			t.Fatalf("open err = %v", err)
		}
	})
	// Each differs from the declaration in one way; a mismatch is never accepted.
	for name, stored := range map[string][2]string{
		"another size":     {contract + "more", "text/markdown"},
		"a shorter body":   {contract[:10], "text/markdown"},
		"another checksum": {strings.ToUpper(contract), "text/markdown"},
		"another type":     {contract, "text/plain"},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			b := newBucket()
			put(b, stored[0], stored[1])
			if err := newStore(b).Verify(ctx, "k", declared); !errors.Is(err, ErrMismatch) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestArtifacts(t *testing.T) {
	ctx := context.Background()
	// Artifacts are saved gzipped and read back as written, and a document's are listed past one page.
	t.Run("stores, lists and reads back artifacts", func(t *testing.T) {
		b := newBucket()
		b.pageSize = 2
		s := newStore(b)
		var want []string
		for i := range 3 {
			key, ok := ArtifactKey("ws-1", "doc-1", fmt.Sprintf("key%d", i))
			if !ok {
				t.Fatal("valid key refused")
			}
			if err := s.PutArtifact(ctx, key, []byte(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
				t.Fatal(err)
			}
			want = append(want, key)
		}
		b.objects[Key("ws-1", "doc-1")] = []byte(contract)
		got, err := s.List(ctx, ArtifactPrefix("ws-1", "doc-1"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("listed %v, want %v", got, want)
		}
		raw, err := s.OpenArtifact(ctx, want[1])
		if err != nil || string(raw) != `{"n":1}` {
			t.Fatalf("read (%s, %v)", raw, err)
		}
		if _, err := s.OpenArtifact(ctx, Key("ws-1", "doc-1")); err == nil {
			t.Fatal("read a non-gzip object as an artifact")
		}
		if _, err := s.OpenArtifact(ctx, "missing"); !errors.Is(err, ErrMissing) {
			t.Fatalf("err = %v", err)
		}
		if err := s.Delete(ctx, "missing"); err != nil {
			t.Fatalf("deleting a missing object: %v", err)
		}
	})
	// The agent's key names an S3 object and a local file, so it can't climb out of either.
	t.Run("refuses an artifact key that is not a plain name", func(t *testing.T) {
		for _, key := range []string{"", "../x", "a/b", "a.b", strings.Repeat("a", 129)} {
			if _, ok := ArtifactKey("ws", "doc", key); ok {
				t.Errorf("accepted %q", key)
			}
		}
	})
}

func TestReadsAndDeletes(t *testing.T) {
	ctx := context.Background()
	// What reading a file gave is written once: a retry that writes it
	// again keeps the first, which is the same bytes, and is no error.
	t.Run("writes a read once", func(t *testing.T) {
		b := newBucket()
		s := newStore(b)
		key := RowsKey("ws-1", "doc-1")
		if key != "ws/ws-1/documents/doc-1/read/rows.jsonl.gz" {
			t.Fatalf("rows key = %s", key)
		}
		if err := s.PutOnce(ctx, key, "application/gzip", strings.NewReader("first")); err != nil {
			t.Fatal(err)
		}
		if err := s.PutOnce(ctx, key, "application/gzip", strings.NewReader("second")); err != nil {
			t.Fatalf("a second write: %v", err)
		}
		if got := string(b.objects[key]); got != "first" || b.types[key] != "application/gzip" {
			t.Fatalf("stored %q as %s", got, b.types[key])
		}
	})
	// Deleting a document removes everything under its prefix: the
	// original, its read and its extraction artifacts, and nothing of
	// another document.
	t.Run("removes a document's whole prefix", func(t *testing.T) {
		b := newBucket()
		b.pageSize = 2
		s := newStore(b)
		artifact, _ := ArtifactKey("ws-1", "doc-1", "key1")
		for _, key := range []string{Key("ws-1", "doc-1"), RowsKey("ws-1", "doc-1"), artifact, Key("ws-1", "doc-10"), Key("ws-1", "doc-2")} {
			b.objects[key] = []byte("x")
		}
		if err := s.DeletePrefix(ctx, Prefix("ws-1", "doc-1")); err != nil {
			t.Fatal(err)
		}
		left := make([]string, 0, len(b.objects))
		for key := range b.objects {
			left = append(left, key)
		}
		sort.Strings(left)
		if strings.Join(left, ",") != Key("ws-1", "doc-10")+","+Key("ws-1", "doc-2") {
			t.Fatalf("left %v", left)
		}
	})
	// A delete S3 refuses is an error, after every other object of the
	// prefix is gone, so a retry has only it left to remove.
	t.Run("reports a delete S3 refuses", func(t *testing.T) {
		b := newBucket()
		s := newStore(b)
		b.objects[Key("ws-1", "doc-1")], b.objects[RowsKey("ws-1", "doc-1")] = []byte("x"), []byte("y")
		b.undeletable = map[string]bool{Key("ws-1", "doc-1"): true}
		if err := s.DeletePrefix(ctx, Prefix("ws-1", "doc-1")); err == nil {
			t.Fatal("no error")
		}
		if _, ok := b.objects[RowsKey("ws-1", "doc-1")]; ok || len(b.objects) != 1 {
			t.Fatalf("left %v", b.objects)
		}
	})
}

func TestNames(t *testing.T) {
	// The declared type must agree with the extension, in any case.
	t.Run("reads the format from the extension", func(t *testing.T) {
		for name, want := range map[string]model.FoundryMediaType{
			"contract.md": model.FoundryMediaMarkdown, "Notes.MARKDOWN": model.FoundryMediaMarkdown, "a.txt": model.FoundryMediaPlainText,
			"guide.mdx": model.FoundryMediaMDX, "Guide.MDX": model.FoundryMediaMDX,
			"orders.json": model.FoundryMediaJSON, "orders.CSV": model.FoundryMediaCSV, "orders.tsv": model.FoundryMediaTSV,
			"events.jsonl": model.FoundryMediaJSONL, "events.ndjson": model.FoundryMediaJSONL,
			"orders.parquet": model.FoundryMediaParquet, "contract.Pdf": model.FoundryMediaPDF,
		} {
			if got, ok := MediaTypeOf(name); !ok || got != want {
				t.Errorf("%s: %q, %t", name, got, ok)
			}
			if got, _ := MediaTypeOf(name); got.Format() == "" {
				t.Errorf("%s: %q has no format name", name, got)
			}
		}
		for _, name := range []string{"a.docx", "a.html", "a.xlsx", "a.json.gz", "md", "a.", "pdf"} {
			if _, ok := MediaTypeOf(name); ok {
				t.Errorf("%s accepted", name)
			}
		}
	})
	t.Run("accepts display names without paths or control characters", func(t *testing.T) {
		for _, name := range []string{"contract.md", "Q3 notes (final).txt", "contrat signé.md"} {
			if !ValidName(name) {
				t.Errorf("%q refused", name)
			}
		}
		for _, name := range []string{"", " a.md", "a/b.md", `a\b.md`, "a\x00.md", strings.Repeat("a", 256), "\xff.md"} {
			if ValidName(name) {
				t.Errorf("%q accepted", name)
			}
		}
	})
	t.Run("accepts only lowercase hex SHA-256 digests", func(t *testing.T) {
		if !ValidSHA256(digest("x")) || ValidSHA256(strings.ToUpper(digest("x"))) || ValidSHA256("abc") || ValidSHA256(strings.Repeat("g", 64)) {
			t.Fatal("wrong verdict")
		}
	})
}

func ExampleArtifactKey() {
	key, ok := ArtifactKey("01WS", "01DOC", "3f9a")
	fmt.Println(key, ok)
	// Output: ws/01WS/documents/01DOC/extractions/3f9a.json.gz true
}
