// Package foundrydocumentworker reads each Foundry table file and PDF once,
// when its upload completes, so loads and generations read a table file as
// a table of its own and a PDF as its text (foundry_document_reads,
// document/read). It runs as a second task of the
// foundry-sync-worker role, which already reads untrusted files in a pod
// with a scratch volume and room for them. A PDF's read that ends wakes the
// loads waiting for it.
package foundrydocumentworker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"go.uber.org/dig"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/document/read"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/server/endpoint"
	"github.com/helixdb/helix-foundry/backend/server/tasks"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const Name = "foundry-document-worker"

// finishWait is how long recording an outcome may take once the read ended,
// even while the worker stops.
const finishWait = 10 * time.Second

// maxRetryBackoff caps how long a read waits after a failure that is not the
// file's: an outage never fails a file, and it is tried again a few times a
// day however long it lasts.
const maxRetryBackoff = 6 * time.Hour

type Deps struct {
	dig.In

	Cfg    *config.Config
	Logger *slog.Logger
	Health endpoint.Health `optional:"true"`
	DB     store.Store     `optional:"true"`
	AWS    aws.Config      `optional:"true"`
}

// readQueue is foundry_document_reads, and the documents they read.
type readQueue interface {
	ClaimFoundryFileRead(ctx context.Context, mediaTypes []model.FoundryMediaType, token string, lease time.Duration,
		wake model.FoundryFileReadWake) (*model.FoundryFileReadClaim, error)
	RenewFoundryFileReadLease(ctx context.Context, documentID string, mode model.FoundryFileReadMode, token string, lease time.Duration) error
	FinishFoundryFileRead(ctx context.Context, documentID string, mode model.FoundryFileReadMode, token string,
		outcome model.FoundryFileReadOutcome, wake model.FoundryFileReadWake) error
	GetFoundryDocument(ctx context.Context, workspaceID, id string) (*model.FoundryDocument, error)
}

// cleaner removes what a read wrote for a document deleted under it.
type cleaner interface {
	DeletePrefix(ctx context.Context, prefix string) error
}

// Task runs due reads one at a time.
type Task struct {
	deps  Deps
	cfg   config.DocumentWorkerConfig
	queue readQueue
	// graph is foundry.graph, whose month's budget the extracts a PDF's read
	// wakes count against (MonthBudget).
	graph config.GraphConfig
	// readers read each kind it reads; it claims documents of those kinds
	// only.
	readers map[model.FoundryReadKind]read.Reader
	store   cleaner
	now     func() time.Time
	worker  tasks.Worker
	// tempDir holds each file and its converted rows while it is read.
	tempDir string
}

func New(d Deps) (*Task, error) {
	if d.DB == nil {
		return nil, errors.New("foundry-document-worker: db not configured")
	}
	store := document.New(s3.NewFromConfig(d.AWS), d.Cfg.Foundry.Sync.DataBucket)
	tempDir := os.TempDir()
	cfg := d.Cfg.Foundry.Documents
	return &Task{
		deps: d, cfg: cfg.Worker, queue: d.DB, store: store, now: time.Now, tempDir: tempDir, graph: d.Cfg.Foundry.Graph,
		readers: map[model.FoundryReadKind]read.Reader{
			model.FoundryReadTable: &read.Tables{Store: store, TempDir: tempDir, Limits: read.DocumentLimits},
			model.FoundryReadPDF: &read.PDFs{Store: store, PDFium: read.NewPDFium(read.PDFMemoryLimit),
				Limits: read.PDFLimits{MaxPages: cfg.Worker.MaxPages, MaxTextBytes: cfg.MaxBytes}},
		},
	}, nil
}

func (t *Task) Name() string { return Name }

// Start reads until Stop. It serves no HTTP of its own: the pod's health
// and metrics are the sync worker's listener's.
func (t *Task) Start(ctx context.Context) error {
	t.sweepTemp(ctx)
	return t.worker.Run(ctx, Name, "", t.deps.Health, t.deps.Logger, func(ctx context.Context) error {
		tasks.Tick(ctx, t.cfg.PollInterval, t.drain)
		return nil
	})
}

func (t *Task) Stop(ctx context.Context) error { return t.worker.Stop(ctx) }

// sweepTemp removes the files a killed container left behind; the sync
// worker sweeps its own.
func (t *Task) sweepTemp(ctx context.Context) {
	for _, pattern := range []string{"foundry-read-*", "foundry-rows-*"} {
		matches, _ := filepath.Glob(filepath.Join(t.tempDir, pattern)) // the patterns are well formed
		for _, m := range matches {
			if err := os.RemoveAll(m); err != nil {
				log.From(ctx).Warn("remove a leftover read file failed", "path", m, "err", err)
			}
		}
	}
}

// mediaTypes are the types of the kinds it has readers for.
func (t *Task) mediaTypes() []model.FoundryMediaType {
	kinds := make([]model.FoundryReadKind, 0, len(t.readers))
	for kind := range t.readers {
		kinds = append(kinds, kind)
	}
	return model.FoundryMediaTypesRead(kinds...)
}

// wake is what a PDF's read that ends hands the loads waiting for it: an id
// per extract, and this month's budget.
func (t *Task) wake() model.FoundryFileReadWake {
	return model.FoundryFileReadWake{NewJobID: id.New, Budget: t.graph.MonthBudget(t.now())}
}

// drain reads due documents until none is left.
func (t *Task) drain(ctx context.Context) {
	types := t.mediaTypes()
	for ctx.Err() == nil {
		claim, err := t.queue.ClaimFoundryFileRead(ctx, types, id.New(), t.cfg.Lease, t.wake())
		if err != nil {
			log.From(ctx).Error("claim foundry document read failed", "err", err)
			return
		}
		if claim == nil {
			return
		}
		t.process(ctx, claim)
	}
}

// process reads one claimed document while renewing its claim, then records
// what it read. Losing the claim cancels the read: another worker holds it.
// Stopping gives the read back as it was. A read past its timeout records
// nothing and leaves its claim to lapse, which counts it as a file that
// kills the worker does: a file that keeps reading too long fails after
// model.MaxFoundryFileReadLapses. A read whose document was deleted under
// it records nothing, and what it wrote is removed: the delete removed the
// document's prefix, perhaps before the write.
func (t *Task) process(ctx context.Context, claim *model.FoundryFileReadClaim) {
	doc, r := claim.Document, claim.Read
	start := t.now()
	format := doc.MediaType.Format()
	runCtx, cancel := context.WithTimeout(ctx, t.cfg.Timeout)
	defer cancel()
	stopRenewing := t.renewLease(runCtx, cancel, r)
	outcome, err := t.read(runCtx, doc)
	stopRenewing()

	var label string
	switch {
	case err == nil:
		label = outcomeLabel(outcome)
	case ctx.Err() != nil:
		outcome, label = model.FoundryFileReadReleased{}, "released"
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		log.From(ctx).Warn("foundry document read timed out; its claim lapses", "document", doc.ID, "workspace", doc.WorkspaceID,
			"format", format, "attempt", r.Attempts, "timeout", t.cfg.Timeout)
		reads.WithLabelValues(format, "timed_out").Inc()
		readSeconds.WithLabelValues(format).Observe(t.now().Sub(start).Seconds())
		return
	default:
		// It waits as long as the read has existed, so waits double with no
		// counter: at least retry_backoff, at most maxRetryBackoff.
		wait := min(max(t.now().Sub(r.CreatedAt), t.cfg.RetryBackoff), maxRetryBackoff)
		log.From(ctx).Warn("foundry document read failed; it will be read again", "document", doc.ID, "workspace", doc.WorkspaceID,
			"format", format, "attempt", r.Attempts, "wait", wait, "err", err)
		outcome = model.FoundryFileReadRetry{At: t.now().Add(wait), Message: "the file could not be read; Helix will retry"}
		label = "retried"
	}
	finishCtx, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), finishWait)
	defer cancelFinish()
	err = t.queue.FinishFoundryFileRead(finishCtx, doc.ID, r.Mode, *r.ClaimToken, outcome, t.wake())
	if errors.Is(err, store.ErrNotFound) {
		label = "lost"
		t.clean(finishCtx, doc)
	} else if err != nil {
		log.From(ctx).Warn("record foundry document read failed; its claim lapses and it is read again", "document", doc.ID, "err", err)
		label = "unrecorded"
	}
	reads.WithLabelValues(format, label).Inc()
	readSeconds.WithLabelValues(format).Observe(t.now().Sub(start).Seconds())
	switch o := outcome.(type) {
	case model.FoundryTableRead:
		if label == "read" {
			readRows.WithLabelValues(format).Add(float64(o.Rows))
		}
	case model.FoundryPDFRead:
		if label == "read" {
			readPages.Add(float64(o.Pages))
		}
	}
}

// read runs the reader of doc's kind.
func (t *Task) read(ctx context.Context, doc *model.FoundryDocument) (model.FoundryFileReadOutcome, error) {
	reader, ok := t.readers[doc.MediaType.ReadKind()]
	if !ok {
		return nil, errors.New("foundry-document-worker: no reader for " + string(doc.MediaType))
	}
	return reader.Read(ctx, doc)
}

// clean removes what a read wrote when its document is gone; a claim lost
// to another worker leaves it, since that worker's read is the same.
func (t *Task) clean(ctx context.Context, doc *model.FoundryDocument) {
	now, err := t.queue.GetFoundryDocument(ctx, doc.WorkspaceID, doc.ID)
	if err == nil && now.Status != model.FoundryDocumentDeleted {
		return
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		log.From(ctx).Warn("read a document after its read was lost failed", "document", doc.ID, "err", err)
		return
	}
	if err := t.store.DeletePrefix(ctx, document.ReadPrefix(doc.WorkspaceID, doc.ID)); err != nil {
		log.From(ctx).Warn("remove a deleted document's read failed", "document", doc.ID, "err", err)
	}
}

func (t *Task) renewLease(ctx context.Context, cancel context.CancelFunc, r *model.FoundryFileRead) (stop func()) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(t.cfg.Lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				err := t.queue.RenewFoundryFileReadLease(ctx, r.DocumentID, r.Mode, *r.ClaimToken, t.cfg.Lease)
				if errors.Is(err, store.ErrNotFound) {
					log.From(ctx).Warn("foundry document read claim lost", "document", r.DocumentID)
					cancel()
					return
				}
				if err != nil {
					log.From(ctx).Warn("renew foundry document read claim failed", "document", r.DocumentID, "err", err)
				}
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// outcomeLabel names what a read gave, for metrics.
func outcomeLabel(outcome model.FoundryFileReadOutcome) string {
	switch o := outcome.(type) {
	case model.FoundryTableRead, model.FoundryPDFRead:
		return "read"
	case model.FoundryFileUnreadable:
		return string(o.Error.Code)
	case model.FoundryFileReadRetry:
		return "retried"
	case model.FoundryFileReadReleased:
		return "released"
	}
	return "unknown"
}
