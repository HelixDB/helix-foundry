package foundrydocumentworker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/document/read"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// queue hands out its claims in order and records how each ended, and the
// wake each was handed.
type queue struct {
	mu       sync.Mutex
	pending  []*model.FoundryFileReadClaim
	types    [][]model.FoundryMediaType
	outcomes map[string]model.FoundryFileReadOutcome
	wakes    []model.FoundryFileReadWake
	renewals int
	// loseClaim makes renewals and the finish find the claim gone.
	loseClaim bool
	// deleted are documents deleted under their read.
	deleted map[string]bool
}

func newQueue(claims ...*model.FoundryFileReadClaim) *queue {
	return &queue{pending: claims, outcomes: map[string]model.FoundryFileReadOutcome{}, deleted: map[string]bool{}}
}

func (q *queue) ClaimFoundryFileRead(_ context.Context, types []model.FoundryMediaType, _ string, _ time.Duration,
	wake model.FoundryFileReadWake) (*model.FoundryFileReadClaim, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.types = append(q.types, types)
	q.wakes = append(q.wakes, wake)
	if len(q.pending) == 0 {
		return nil, nil
	}
	next := q.pending[0]
	q.pending = q.pending[1:]
	return next, nil
}

func (q *queue) RenewFoundryFileReadLease(context.Context, string, model.FoundryFileReadMode, string, time.Duration) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.renewals++
	if q.loseClaim {
		return store.ErrNotFound
	}
	return nil
}

func (q *queue) FinishFoundryFileRead(_ context.Context, documentID string, _ model.FoundryFileReadMode, _ string,
	outcome model.FoundryFileReadOutcome, wake model.FoundryFileReadWake) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.wakes = append(q.wakes, wake)
	if q.loseClaim || q.deleted[documentID] {
		return store.ErrNotFound
	}
	q.outcomes[documentID] = outcome
	return nil
}

func (q *queue) GetFoundryDocument(_ context.Context, workspaceID, id string) (*model.FoundryDocument, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	status := model.FoundryDocumentUploaded
	if q.deleted[id] {
		status = model.FoundryDocumentDeleted
	}
	return &model.FoundryDocument{ID: id, WorkspaceID: workspaceID, Status: status}, nil
}

// bucket is the documents bucket in memory.
type bucket struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (b *bucket) Open(_ context.Context, key string) (io.ReadCloser, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.objects[key]
	if !ok {
		return nil, document.ErrMissing
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *bucket) PutOnce(_ context.Context, key, _ string, body io.ReadSeeker) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.objects[key]; !ok {
		b.objects[key] = data
	}
	return nil
}

func (b *bucket) DeletePrefix(_ context.Context, prefix string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key := range b.objects {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			delete(b.objects, key)
		}
	}
	return nil
}

// reader answers every read with its function.
type reader func(ctx context.Context, doc *model.FoundryDocument) (model.FoundryFileReadOutcome, error)

func (r reader) Read(ctx context.Context, doc *model.FoundryDocument) (model.FoundryFileReadOutcome, error) {
	return r(ctx, doc)
}

// claimOf is a claimed standard read of a document of mediaType holding data in b.
func claimOf(b *bucket, id string, mediaType model.FoundryMediaType, data string) *model.FoundryFileReadClaim {
	token := "token-" + id
	doc := &model.FoundryDocument{
		ID: id, WorkspaceID: "ws-1", Name: id, MediaType: mediaType, SizeBytes: int64(len(data)), Status: model.FoundryDocumentUploaded,
		S3Key: document.Key("ws-1", id),
	}
	b.objects[doc.S3Key] = []byte(data)
	return &model.FoundryFileReadClaim{Document: doc, Read: &model.FoundryFileRead{
		DocumentID: id, WorkspaceID: "ws-1", Mode: model.FoundryFileReadStandard, Status: model.FoundryFileReadRunning, Attempts: 1, ClaimToken: &token,
	}}
}

// pdfium is the worker's PDFium, compiled once for every test: each read
// still gets an instance of its own.
var pdfium = read.NewPDFium(read.PDFMemoryLimit)

// newTask reads with the real table and PDF readers over b, its timings
// short and a $100 month.
func newTask(t *testing.T, q *queue, b *bucket) *Task {
	dir := t.TempDir()
	return &Task{
		cfg: config.DocumentWorkerConfig{PollInterval: time.Millisecond, Lease: 30 * time.Millisecond, Timeout: time.Minute,
			RetryBackoff: time.Minute, MaxPages: 150},
		queue: q, store: b, tempDir: dir, now: func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }, graph: config.GraphConfig{MaxJobUSD: 20, MonthlyUSD: 100},
		readers: map[model.FoundryReadKind]read.Reader{
			model.FoundryReadTable: &read.Tables{Store: b, TempDir: dir, Limits: read.DocumentLimits},
			model.FoundryReadPDF:   &read.PDFs{Store: b, PDFium: pdfium, Limits: read.PDFLimits{MaxPages: 150, MaxTextBytes: 1 << 20}},
		},
	}
}

// onePagePDF is a one-page PDF whose page reads as text, written by hand.
func onePagePDF(text string) string {
	content := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET\n", text)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
	}
	var out strings.Builder
	out.WriteString("%PDF-1.7\n")
	offsets := make([]int, len(objects))
	for i, o := range objects {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return out.String()
}

func TestDocumentReads(t *testing.T) {
	ctx := context.Background()

	// The worker drains every due read in turn, recording what each file
	// gave: a CSV's rows stored beside it, a JSON Lines file as it is, a
	// PDF's text stored beside it, and files it can't read for good. It asks
	// only for the types it reads.
	t.Run("reads every due file and records what it gave", func(t *testing.T) {
		b := &bucket{objects: map[string][]byte{}}
		q := newQueue(
			claimOf(b, "csv", model.FoundryMediaCSV, "id,name\n1,Acme\n2,Globex\n"),
			claimOf(b, "jsonl", model.FoundryMediaJSONL, "{\"id\":1}\n"),
			claimOf(b, "bad", model.FoundryMediaJSON, "{\"id\":1}"),
			claimOf(b, "pdf", model.FoundryMediaPDF, onePagePDF("Invoice 1042")),
			claimOf(b, "notpdf", model.FoundryMediaPDF, "id,name\n1,Acme\n"),
		)
		newTask(t, q, b).drain(ctx)
		if got := q.outcomes["csv"]; got != (model.FoundryTableRead{ArtifactKey: document.RowsKey("ws-1", "csv"), Rows: 2}) {
			t.Fatalf("csv = %+v", got)
		}
		if _, ok := b.objects[document.RowsKey("ws-1", "csv")]; !ok {
			t.Fatal("the CSV's rows were not stored")
		}
		if got := q.outcomes["jsonl"]; got != (model.FoundryTableRead{ArtifactKey: document.Key("ws-1", "jsonl"), Rows: 1}) {
			t.Fatalf("jsonl = %+v", got)
		}
		bad, ok := q.outcomes["bad"].(model.FoundryFileUnreadable)
		if !ok || bad.Error.Code != model.FoundryFileReadUnsupported {
			t.Fatalf("bad = %+v", q.outcomes["bad"])
		}
		pdf, ok := q.outcomes["pdf"].(model.FoundryPDFRead)
		if !ok || pdf.Pages != 1 || string(b.objects[pdf.ArtifactKey]) != "Invoice 1042\n" ||
			pdf.ArtifactKey != document.TextKey("ws-1", "pdf", model.FoundryFileReadStandard, pdf.TextSHA256) {
			t.Fatalf("pdf = %+v, stored %q", q.outcomes["pdf"], b.objects[pdf.ArtifactKey])
		}
		if notPDF, ok := q.outcomes["notpdf"].(model.FoundryFileUnreadable); !ok || notPDF.Error.Code != model.FoundryFileReadUnsupported {
			t.Fatalf("notpdf = %+v", q.outcomes["notpdf"])
		}
		want := model.FoundryMediaTypesRead(model.FoundryReadTable, model.FoundryReadPDF)
		for _, types := range q.types {
			if !slices.Equal(types, want) {
				t.Fatalf("claimed %v, want the table and PDF types %v", types, want)
			}
		}
	})

	// A failure that is not the file's gives the read back to the queue,
	// saying so; the file is never blamed. It waits as long as the read has
	// existed, so waits double: at least the backoff, at most six hours.
	t.Run("retries a read whose storage failed, waiting longer each time", func(t *testing.T) {
		for _, tc := range []struct {
			age, wait time.Duration
		}{{10 * time.Second, time.Minute}, {20 * time.Minute, 20 * time.Minute}, {3 * 24 * time.Hour, 6 * time.Hour}} {
			b := &bucket{objects: map[string][]byte{}}
			claim := claimOf(b, "csv", model.FoundryMediaCSV, "id\n1\n")
			q := newQueue(claim)
			task := newTask(t, q, b)
			claim.Read.CreatedAt = task.now().Add(-tc.age)
			task.readers[model.FoundryReadTable] = reader(func(context.Context, *model.FoundryDocument) (model.FoundryFileReadOutcome, error) {
				return nil, errors.New("s3 is down")
			})
			task.drain(ctx)
			want := model.FoundryFileReadRetry{At: task.now().Add(tc.wait), Message: "the file could not be read; Helix will retry"}
			if got := q.outcomes["csv"]; got != want {
				t.Fatalf("a read %s old: outcome = %+v, want %+v", tc.age, got, want)
			}
		}
	})

	// A worker that stops mid-read gives the read back as it was, and
	// records it even though its context is done.
	t.Run("gives a read back when it stops", func(t *testing.T) {
		b := &bucket{objects: map[string][]byte{}}
		q := newQueue(claimOf(b, "csv", model.FoundryMediaCSV, "id\n1\n"))
		task := newTask(t, q, b)
		stopping, stop := context.WithCancel(ctx)
		task.readers[model.FoundryReadTable] = reader(func(ctx context.Context, _ *model.FoundryDocument) (model.FoundryFileReadOutcome, error) {
			stop()
			<-ctx.Done()
			return nil, ctx.Err()
		})
		task.drain(stopping)
		if got := q.outcomes["csv"]; got != (model.FoundryFileReadReleased{}) {
			t.Fatalf("outcome = %+v", got)
		}
	})

	// A read past its timeout records nothing: its claim lapses and counts,
	// so a file that keeps taking too long fails as one that kills the
	// worker does, rather than retrying forever.
	t.Run("leaves a read past its timeout to lapse", func(t *testing.T) {
		b := &bucket{objects: map[string][]byte{}}
		q := newQueue(claimOf(b, "csv", model.FoundryMediaCSV, "id\n1\n"))
		task := newTask(t, q, b)
		task.cfg.Timeout = 10 * time.Millisecond
		task.readers[model.FoundryReadTable] = reader(func(ctx context.Context, _ *model.FoundryDocument) (model.FoundryFileReadOutcome, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		task.drain(ctx)
		if len(q.outcomes) != 0 {
			t.Fatalf("recorded %+v", q.outcomes)
		}
	})

	// The claim is renewed while a read runs; losing it stops the read,
	// which records nothing, and leaves what it wrote to the worker that
	// holds the claim now.
	t.Run("stops a read whose claim is lost", func(t *testing.T) {
		b := &bucket{objects: map[string][]byte{}}
		q := newQueue(claimOf(b, "csv", model.FoundryMediaCSV, "id\n1\n"))
		q.loseClaim = true
		b.objects[document.RowsKey("ws-1", "csv")] = []byte("rows")
		task := newTask(t, q, b)
		task.readers[model.FoundryReadTable] = reader(func(ctx context.Context, _ *model.FoundryDocument) (model.FoundryFileReadOutcome, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		task.drain(ctx)
		if q.renewals == 0 || len(q.outcomes) != 0 {
			t.Fatalf("%d renewals, outcomes %+v", q.renewals, q.outcomes)
		}
		if _, ok := b.objects[document.RowsKey("ws-1", "csv")]; !ok {
			t.Fatal("removed the rows another worker's read wrote")
		}
	})

	// A document deleted while it was read: the delete removed its prefix,
	// perhaps before the rows were written, so the worker removes them.
	t.Run("removes what it wrote for a document deleted under it", func(t *testing.T) {
		b := &bucket{objects: map[string][]byte{}}
		q := newQueue(claimOf(b, "csv", model.FoundryMediaCSV, "id\n1\n"))
		task := newTask(t, q, b)
		tables := task.readers[model.FoundryReadTable]
		task.readers[model.FoundryReadTable] = reader(func(ctx context.Context, doc *model.FoundryDocument) (model.FoundryFileReadOutcome, error) {
			outcome, err := tables.Read(ctx, doc)
			q.mu.Lock()
			q.deleted[doc.ID] = true
			q.mu.Unlock()
			return outcome, err
		})
		task.drain(ctx)
		if _, ok := b.objects[document.RowsKey("ws-1", "csv")]; ok || len(q.outcomes) != 0 {
			t.Fatalf("left %v, recorded %+v", b.objects, q.outcomes)
		}
	})

	// A start removes the files a killed container left in the scratch
	// directory, and only its own.
	t.Run("sweeps its leftovers at start", func(t *testing.T) {
		task := newTask(t, newQueue(), &bucket{objects: map[string][]byte{}})
		for _, name := range []string{"foundry-read-1", "foundry-rows-2", "foundry-s3-3"} {
			if err := os.WriteFile(filepath.Join(task.tempDir, name), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		task.sweepTemp(ctx)
		left, _ := filepath.Glob(filepath.Join(task.tempDir, "*"))
		if len(left) != 1 || filepath.Base(left[0]) != "foundry-s3-3" {
			t.Fatalf("left %v", left)
		}
	})
}

func TestPDFReadWakes(t *testing.T) {
	ctx := context.Background()

	// Every claim and finish hands the provider a wake for the loads a
	// PDF's read lets go: fresh extract ids, and this month's budget from
	// foundry.graph.monthly_usd.
	t.Run("hands this month's budget to the loads it wakes", func(t *testing.T) {
		b := &bucket{objects: map[string][]byte{}}
		q := newQueue(claimOf(b, "pdf", model.FoundryMediaPDF, onePagePDF("Terms")))
		newTask(t, q, b).drain(ctx)
		if len(q.wakes) < 2 {
			t.Fatalf("%d wakes", len(q.wakes))
		}
		for _, w := range q.wakes {
			want := model.FoundryGraphBudget{Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), CapMicros: 100_000_000, ReserveMicros: 20_000_000}
			if w.Budget != want || w.NewJobID == nil {
				t.Fatalf("wake = %+v", w)
			}
			if first, second := w.NewJobID(), w.NewJobID(); first == second {
				t.Fatalf("extract ids repeat: %s", first)
			}
		}
	})

	// A PDF that keeps a read past its timeout is left to lapse, as any
	// file is: PDFium is killed where it is and nothing is recorded.
	t.Run("leaves a PDF past its timeout to lapse", func(t *testing.T) {
		b := &bucket{objects: map[string][]byte{}}
		q := newQueue(claimOf(b, "pdf", model.FoundryMediaPDF, onePagePDF("slow")))
		task := newTask(t, q, b)
		task.cfg.Timeout = time.Nanosecond
		task.drain(ctx)
		if len(q.outcomes) != 0 {
			t.Fatalf("recorded %+v", q.outcomes)
		}
	})
}
