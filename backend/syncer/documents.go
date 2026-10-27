package syncer

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/sync/errgroup"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

var syncDocuments = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_sync_documents_total",
	Help: "Connector documents by kind and what a sync did with each: added, replaced, updated, kept, over_cap, outside_window, paused, removed or truncated.",
}, []string{"kind", "outcome"})

// documentStore holds documents' bytes (document.Store).
type documentStore interface {
	PutOnce(ctx context.Context, key, contentType string, body io.ReadSeeker) error
}

const (
	// A stream's batch flushes at flushDocuments documents or flushBytes of
	// their text, whichever comes first, and puts flushPuts originals at once.
	flushDocuments = 100
	flushBytes     = 8 << 20
	flushPuts      = 8
	// removeChunk is the most documents one publish of finish or prune
	// removes. A KeepNewest window trims one document per Put, which
	// publishes with its batch, but for a set over its n (n lowered).
	removeChunk = 500
	// indexPage is the most entries one read of a set's live index returns.
	indexPage = 1000
)

// documents is one attempt's DocumentWriter: the connector's on streams
// whose table the sync reads, each with its set and live index, and the
// read of its table in progress. A stream's documents publish in batches
// during the read (flush), all before its table's snapshot does (finish).
// The first error ends the attempt: Want then says no, and Put returns it.
type documents struct {
	r *run
	// streams are by name; tables lists each read table's streams.
	streams map[string]*stream
	tables  map[string][]*stream
	// sets are the connector's sets when the attempt began.
	sets         []*model.FoundryDocumentSet
	refreshEvery time.Duration
	// systemOnly is set when the kind has an allowance (Syncer.Allowances):
	// its documents are work no person started
	// (model.FoundryDocumentFanOut.SystemOnly).
	systemOnly bool
	err        error
}

// stream is one on stream of the sync.
type stream struct {
	connector.Stream
	setID string
	// key is the column of its table's rows that holds a document's external id.
	key string
	// held is the set's live index by external id, kept as each flush publishes.
	held map[string]model.ConnectorDocument
	// pause is what the stream does this attempt under its kind's allowance.
	pause pauseState
	// reading is how its table is being read; offers, seen and wanted are
	// that read's: the offers counted against the cap, the ids offered
	// within it, and those Want said yes to that Put has not taken.
	reading readMode
	offers  int
	seen    map[string]bool
	wanted  map[string]bool
	batch   batch
}

// pauseState is what a stream of a kind with an allowance
// (Syncer.Allowances) does this attempt, decided as it opens (pause).
type pauseState uint8

const (
	// unpaused takes documents as every stream does.
	unpaused pauseState = iota
	// paused takes none and removes none: what work no person started may
	// spend extracting its set this month is spent, so the set keeps what
	// its graph last extracted. Its set is marked skipped.
	paused
	// catchingUp is an unpaused stream whose set a paused sync skipped: the
	// sync reads in full, and once that read of its table publishes its
	// documents, the set's skip is cleared.
	catchingUp
)

// readMode is how a stream's table is being read.
type readMode uint8

const (
	// notReading: Want and Put are a kind bug.
	notReading readMode = iota
	// readingFull offers the document of every row, newest first; the first
	// MaxPerStream offered are kept, or n under KeepNewest(n).
	readingFull
	// readingChanges offers the documents of the rows it upserts, with no
	// cap but a KeepNewest window.
	readingChanges
	// discarding is a table a pass reads that an earlier attempt of the sync
	// published: its documents are left as they are.
	discarding
)

// batch is what a stream's next flush publishes.
type batch struct {
	add    []*model.FoundryDocument
	text   [][]byte // add's bytes, in order
	update []model.ConnectorDocumentUpdate
	remove []string
	size   int
	// held and gone are what the publish makes of the live index, by
	// external id; no id is in both, and each of gone is held now.
	held map[string]model.ConnectorDocument
	gone map[string]bool
	// outcomes count the documents by what publishing does with them.
	outcomes map[string]int
}

func (b *batch) empty() bool { return len(b.add)+len(b.update)+len(b.remove) == 0 }

// openDocuments takes, after table selection, the kind's on streams whose
// table the sync reads, makes sure each has its set (EnsureConnectorDocumentSet,
// unless it is already as the stream names it), and loads its live index.
// A kind with no streams has none. For a kind with an allowance, it then
// decides which streams pause (pause).
func (r *run) openDocuments(ctx context.Context, tables []connector.Table, refreshEvery time.Duration) (*documents, error) {
	d := &documents{r: r, streams: map[string]*stream{}, tables: map[string][]*stream{}, refreshEvery: refreshEvery}
	kind, ok := r.kind.(connector.DocumentKind)
	if !ok {
		return d, nil
	}
	// Zero limits would cut every text to nothing or remove every document.
	if r.s.Documents == nil || r.s.MonthBudget == nil || r.s.MaxDocumentBytes < 1 || r.s.MaxPerStream < 1 || r.s.PurgeGrace <= 0 {
		return nil, retryable(errors.New("the syncer's document store, budget or limits are not set"))
	}
	sets, err := r.s.DB.ListFoundryDocumentSets(ctx, r.sync.WorkspaceID)
	if err != nil {
		return nil, retryable(fmt.Errorf("list document sets: %w", err))
	}
	for _, set := range sets {
		if set.Owner != nil && set.Owner.ConnectorID == r.sync.ConnectorID {
			d.sets = append(d.sets, set)
		}
	}
	for _, s := range kind.Streams(r.src) {
		i := slices.IndexFunc(tables, func(t connector.Table) bool { return t.Name == s.Table })
		if i < 0 {
			continue
		}
		var problem string
		switch {
		case s.Problem() != "":
			problem = s.Problem()
		case d.streams[s.Name] != nil:
			problem = "two streams have its name"
		case len(tables[i].PrimaryKey) != 1:
			problem = fmt.Sprintf("its table %s is keyed by %d columns, not one", s.Table, len(tables[i].PrimaryKey))
		case s.Keep.Newest() > r.s.MaxPerStream:
			problem = fmt.Sprintf("it keeps its newest %d, more than foundry.documents.max_per_stream (%d)", s.Keep.Newest(), r.s.MaxPerStream)
		}
		if problem != "" {
			return nil, d.fail(&connector.DocumentError{Stream: s.Name, Reason: problem})
		}
		st := &stream{Stream: s, key: tables[i].PrimaryKey[0]}
		if st.setID, err = d.ensure(ctx, s); err != nil {
			return nil, err
		}
		if st.held, err = d.index(ctx, st.setID); err != nil {
			return nil, err
		}
		d.streams[s.Name] = st
		d.tables[s.Table] = append(d.tables[s.Table], st)
	}
	allowance, limited := r.s.Allowances[r.kind.Name()]
	d.systemOnly = limited
	if !limited || len(d.streams) == 0 {
		return d, nil
	}
	if err := d.pause(ctx, allowance); err != nil {
		return nil, err
	}
	return d, nil
}

// pause decides, once an attempt, whether each stream of a kind with an
// allowance takes documents. A stream is paused while work no person
// started can spend nothing more extracting its set this month: its
// allowance, or the workspace's for the kind, is spent
// (FoundryDocumentSetSpend.Left), or the workspace's month leaves such work
// nothing (FoundryGraphBudget.CapFor of the month's graph spend). Its set
// is marked skipped. Two never pause, since the pause only keeps what the
// graph last extracted, and writing documents spends nothing: the graph
// worker holds what the allowance can't pay for, and a refresh that held
// them keeps the load it refreshed (FinishFoundryGraphJob):
//   - a stream whose set holds no documents, which has nothing to keep, so
//     a new source's first pieces are documents a person's load can read;
//   - a sync a person requested (a text field's change, or Sync now),
//     whose documents that person asked for.
//
// An unpaused stream whose set a paused sync skipped, in any month,
// catches up: the sync reads in full (catchUp). One read of the sets'
// spend, and one of the month's.
func (d *documents) pause(ctx context.Context, allowance model.FoundrySetAllowance) error {
	r := d.r
	ws, now := r.sync.WorkspaceID, r.s.now()
	var ids []string
	for _, s := range d.streams {
		ids = append(ids, s.setID)
	}
	spend, err := r.s.DB.FoundryDocumentSetSpend(ctx, ws, ids, now)
	if err != nil {
		return retryable(fmt.Errorf("read document sets' spend: %w", err))
	}
	budget := r.s.MonthBudget(now)
	spent, err := r.s.DB.SumFoundryGraphJobSpend(ctx, ws, budget.Since)
	if err != nil {
		return retryable(fmt.Errorf("read the month's graph spend: %w", err))
	}
	for _, s := range d.streams {
		set, ok := spend[s.setID]
		switch {
		case !ok:
			// Its connector was deleted since the set was ensured.
			return retryable(fmt.Errorf("read the spend of document set %s: it is gone", s.setID))
		case (set.Left(allowance) <= 0 || spent >= budget.CapFor(nil)) && len(s.held) > 0 && r.sync.RequestedByUserID == nil:
			s.pause = paused
			if err := r.s.DB.MarkFoundryDocumentSetSkipped(ctx, ws, s.setID, now); err != nil {
				return retryable(fmt.Errorf("mark document set %s skipped: %w", s.setID, err))
			}
		case set.Skipped:
			s.pause = catchingUp
		}
	}
	return nil
}

// catchUp reports a stream catching up on what a paused sync skipped,
// which makes the sync read in full.
func (d *documents) catchUp() bool {
	for _, s := range d.streams {
		if s.pause == catchingUp {
			return true
		}
	}
	return false
}

// ensure returns the id of s's set, writing it only when it has none or
// the stream's title or description changed.
func (d *documents) ensure(ctx context.Context, s connector.Stream) (string, error) {
	for _, set := range d.sets {
		if set.Owner.Stream == s.Name && set.Name == s.Title && set.Description == s.Description {
			return set.ID, nil
		}
	}
	setID, err := d.r.s.DB.EnsureConnectorDocumentSet(ctx, &model.FoundryDocumentSet{
		ID: id.New(), WorkspaceID: d.r.sync.WorkspaceID, Name: s.Title, Description: s.Description,
		Owner: &model.FoundryDocumentSetOwner{ConnectorID: d.r.sync.ConnectorID, Stream: s.Name},
	}, d.r.sync.ID, leaseOwner(d.r.sync))
	if err != nil {
		return "", published(err, "keep the "+s.Name+" document set")
	}
	return setID, nil
}

// index reads a set's live index, a page at a time.
func (d *documents) index(ctx context.Context, setID string) (map[string]model.ConnectorDocument, error) {
	held := map[string]model.ConnectorDocument{}
	after := ""
	for {
		page, err := d.r.s.DB.ListConnectorDocuments(ctx, d.r.sync.WorkspaceID, setID, after, indexPage)
		if err != nil {
			return nil, retryable(fmt.Errorf("read document set %s: %w", setID, err))
		}
		for _, doc := range page {
			held[doc.ExternalID] = doc
		}
		if len(page) < indexPage {
			return held, nil
		}
		after = page[len(page)-1].ExternalID
	}
}

// carries reports a table an on stream belongs to.
func (d *documents) carries(table string) bool { return len(d.tables[table]) > 0 }

// begin starts a read of table's streams. What an earlier read of them in
// this attempt Put, a change read's of a table now read in full, publishes first.
func (d *documents) begin(ctx context.Context, table string, mode readMode) error {
	for _, s := range d.tables[table] {
		if err := d.flush(ctx, s); err != nil {
			return err
		}
		s.reading, s.offers, s.seen, s.wanted = mode, 0, map[string]bool{}, map[string]bool{}
	}
	return nil
}

func (d *documents) Want(name, externalID, version string) bool {
	if d.err != nil {
		return false
	}
	s, err := d.reading(name)
	if err != nil {
		_ = d.fail(err)
		return false
	}
	reason := ""
	switch {
	case !printable(externalID, 255, false):
		reason = "its id must be 1 to 255 printable ASCII characters, with no space"
	case !printable(version, 128, true):
		reason = "its version must be 1 to 128 printable ASCII characters"
	}
	if reason != "" {
		_ = d.fail(&connector.DocumentError{Stream: name, ExternalID: externalID, Reason: reason})
		return false
	}
	if s.reading == discarding || s.seen[externalID] {
		return false
	}
	if s.pause == paused {
		d.count("paused", 1)
		return false
	}
	newest := s.Keep.Newest()
	if s.reading == readingFull {
		s.offers++
		if s.offers > cmp.Or(newest, d.r.s.MaxPerStream) {
			d.count("over_cap", 1)
			return false
		}
	}
	s.seen[externalID] = true
	// Under KeepNewest the held version may come with a newer time: Put decides.
	if held, ok := s.held[externalID]; ok && held.Version == version && newest == 0 {
		d.count("kept", 1)
		return false
	}
	s.wanted[externalID] = true
	return true
}

// Put cleans and checks doc, then queues it for the stream's next flush: an
// update in place when its bytes are the held document's, else a new
// document that replaces the held one, if any. Under KeepNewest, the held
// document at the same version, bytes and time is kept, with nothing
// queued, and the window is then trimmed back to n (trim).
func (d *documents) Put(ctx context.Context, name string, doc connector.Document) error {
	if d.err != nil {
		return d.err
	}
	s, err := d.reading(name)
	if err != nil {
		return d.fail(err)
	}
	reason := ""
	switch {
	case !s.wanted[doc.ExternalID]:
		reason = "Want did not say yes to it"
	case !printable(doc.Version, 128, true):
		reason = "its version must be 1 to 128 printable ASCII characters"
	case doc.At.IsZero() || doc.At.Year() < 1000 || doc.At.Year() > 9999:
		reason = "its time is missing or out of range"
	case len(doc.Text) == 0 || !utf8.Valid(doc.Text):
		reason = "its text is empty or not UTF-8"
	}
	if reason != "" {
		return d.fail(&connector.DocumentError{Stream: name, ExternalID: doc.ExternalID, Reason: reason})
	}
	delete(s.wanted, doc.ExternalID)
	b := &s.batch
	if b.held == nil {
		b.held, b.gone, b.outcomes = map[string]model.ConnectorDocument{}, map[string]bool{}, map[string]int{}
	}
	text, truncated := truncate(doc.Text, d.r.s.MaxDocumentBytes)
	sum := sha256.Sum256(text)
	// source_at holds microseconds, so a held time compares as it is stored.
	at := doc.At.UTC().Truncate(time.Microsecond)
	entry := model.ConnectorDocument{ExternalID: doc.ExternalID, Version: doc.Version, SHA256: hex.EncodeToString(sum[:]), At: at}
	title, link := documentName(doc.Title, doc.ExternalID), sourceURL(doc.URL)
	held, ok := s.held[doc.ExternalID]
	if b.gone[doc.ExternalID] {
		// A trim of this batch took the held document out, before the read
		// offered it again, perhaps newer: it is held until trimmed again.
		delete(b.gone, doc.ExternalID)
		b.remove = slices.DeleteFunc(b.remove, func(id string) bool { return id == held.ID })
		b.outcomes["removed"]--
	}
	newest := s.Keep.Newest()
	kept := newest > 0 && ok && held.Version == entry.Version && held.SHA256 == entry.SHA256 && held.At.Equal(at)
	if truncated && !kept {
		b.outcomes["truncated"]++
	}
	switch {
	case kept:
		d.count("kept", 1)
	case ok && held.SHA256 == entry.SHA256:
		entry.ID = held.ID
		b.update = append(b.update, model.ConnectorDocumentUpdate{ID: held.ID, Name: title, Version: doc.Version, At: at, URL: link})
		b.outcomes["updated"]++
		b.held[doc.ExternalID] = entry
	default:
		entry.ID = id.New()
		ws := d.r.sync.WorkspaceID
		b.add = append(b.add, &model.FoundryDocument{
			ID: entry.ID, WorkspaceID: ws, SetID: s.setID, Name: title, MediaType: model.FoundryMediaType(s.Format.MediaType()),
			SizeBytes: int64(len(text)), SHA256: entry.SHA256, Status: model.FoundryDocumentUploaded, S3Key: document.Key(ws, entry.ID),
			Source: &model.FoundryDocumentSource{ExternalID: doc.ExternalID, Version: doc.Version, At: at, URL: link},
		})
		b.text = append(b.text, text)
		b.size += len(text)
		if ok {
			b.remove = append(b.remove, held.ID)
			b.outcomes["replaced"]++
		} else {
			b.outcomes["added"]++
		}
		b.held[doc.ExternalID] = entry
	}
	if newest > 0 {
		d.trim(s, newest)
	}
	if len(b.add)+len(b.update) < flushDocuments && b.size < flushBytes {
		return nil
	}
	return d.flush(ctx, s)
}

// trim takes the oldest documents (older) out of a KeepNewest(n) stream's
// window, its live index as the batch will publish it, until it holds n,
// so each flush adds and trims in one transaction and the set never holds
// more than n:
//   - a held document joins the batch's removals (removed);
//   - a pending update leaves the batch, and its document joins the removals;
//   - a pending replacement leaves the batch, and the document it replaces
//     stays removed;
//   - a pending new document leaves the batch, and is never written
//     (outside_window).
//
// Each pass scans the window, n plus the batch: n is at most
// foundry.documents.max_per_stream (1,000), and a Put trims one document,
// but for a set already over n.
func (d *documents) trim(s *stream, n int) {
	b := &s.batch
	for {
		count, oldest := 0, model.ConnectorDocument{}
		consider := func(doc model.ConnectorDocument) {
			count++
			if count == 1 || older(doc, oldest) < 0 {
				oldest = doc
			}
		}
		for externalID, doc := range s.held {
			if _, pending := b.held[externalID]; !pending && !b.gone[externalID] {
				consider(doc)
			}
		}
		for _, doc := range b.held {
			consider(doc)
		}
		if count <= n {
			return
		}
		held, isHeld := s.held[oldest.ExternalID]
		_, pending := b.held[oldest.ExternalID]
		delete(b.held, oldest.ExternalID)
		unadd := func() {
			i := slices.IndexFunc(b.add, func(doc *model.FoundryDocument) bool { return doc.ID == oldest.ID })
			b.size -= len(b.text[i])
			b.add, b.text = slices.Delete(b.add, i, i+1), slices.Delete(b.text, i, i+1)
		}
		switch {
		case pending && !isHeld:
			unadd()
			b.outcomes["added"]--
			d.count("outside_window", 1)
			continue
		case pending && oldest.ID == held.ID:
			b.update = slices.DeleteFunc(b.update, func(u model.ConnectorDocumentUpdate) bool { return u.ID == held.ID })
			b.outcomes["updated"]--
			b.remove = append(b.remove, held.ID)
		case pending:
			unadd()
			b.outcomes["replaced"]--
		default:
			b.remove = append(b.remove, held.ID)
		}
		b.gone[oldest.ExternalID] = true
		b.outcomes["removed"]++
	}
}

// older orders documents oldest first, as a KeepNewest window trims them:
// by At, then the higher external id by byte order.
func older(a, b model.ConnectorDocument) int {
	return cmp.Or(a.At.Compare(b.At), strings.Compare(b.ExternalID, a.ExternalID))
}

func (d *documents) Held(name, externalID string) (string, bool) {
	s := d.streams[name]
	if s == nil {
		return "", false
	}
	held, ok := s.held[externalID]
	return held.Version, ok
}

func (d *documents) Paused(name string) bool {
	s := d.streams[name]
	return s != nil && s.pause == paused
}

// reading is the stream name while its table is being read.
func (d *documents) reading(name string) (*stream, error) {
	s := d.streams[name]
	switch {
	case s == nil:
		return nil, &connector.DocumentError{Stream: name, Reason: "the stream is off, or its table is not synced"}
	case s.reading == notReading:
		return nil, &connector.DocumentError{Stream: name, Reason: "its table " + s.Table + " is not being read"}
	}
	return s, nil
}

// flush publishes the stream's batch, so the bytes never exist without a
// row that owns them: it reserves the new documents' ids (a purge row
// each), puts their originals, then publishes the batch in one
// transaction, which claims the reservations.
func (d *documents) flush(ctx context.Context, s *stream) error {
	b := s.batch
	if b.empty() {
		return nil
	}
	sync := d.r.sync
	if len(b.add) > 0 {
		ids := make([]string, len(b.add))
		for i, doc := range b.add {
			ids[i] = doc.ID
		}
		if err := d.r.s.DB.ReserveConnectorDocuments(ctx, sync.WorkspaceID, ids, sync.ID, leaseOwner(sync)); err != nil {
			return d.fail(published(err, "reserve documents"))
		}
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(flushPuts)
		for i, doc := range b.add {
			g.Go(func() error {
				return d.r.s.Documents.PutOnce(gctx, doc.S3Key, string(doc.MediaType), bytes.NewReader(b.text[i]))
			})
		}
		if err := g.Wait(); err != nil {
			return d.fail(retryable(fmt.Errorf("store documents: %w", err)))
		}
	}
	_, err := d.r.s.DB.PublishConnectorDocuments(ctx, model.ConnectorDocumentsPublish{
		WorkspaceID: sync.WorkspaceID, ConnectorID: sync.ConnectorID, SetID: s.setID,
		Add: b.add, Update: b.update, Remove: b.remove, Grace: d.r.s.PurgeGrace, RefreshEvery: d.refreshEvery,
		Fan: model.FoundryDocumentFanOut{NewJobID: id.New, Budget: d.r.s.MonthBudget(d.r.s.now()), CanWrite: func(string) bool { return true },
			SystemOnly: d.systemOnly},
	}, sync.ID, leaseOwner(sync))
	if err != nil {
		return d.fail(published(err, "publish documents"))
	}
	for externalID, entry := range b.held {
		s.held[externalID] = entry
	}
	for externalID := range b.gone {
		delete(s.held, externalID)
	}
	for outcome, n := range b.outcomes {
		d.count(outcome, n)
	}
	s.batch = batch{}
	return nil
}

// finish ends the read of table's streams once its new snapshot is hashed
// and before it publishes: it flushes what the read Put, then removes, in
// chunks, the documents the stream no longer keeps: in a full read, those
// it did not offer within the cap; in any read, those whose row is not in
// the snapshot, matched by key (snapshot.KeyHash); under KeepNewest(n), the
// oldest of the rest beyond n. pairs are the snapshot's rows hashed by key;
// nil when the read deleted no row. A read that failed never gets here, so
// it removes nothing, and neither does a paused stream's. A stream catching
// up clears its set's skip once its full read has published.
func (d *documents) finish(ctx context.Context, table string, key []string, pairs []snapshot.Pair) error {
	for _, s := range d.tables[table] {
		if err := d.flush(ctx, s); err != nil {
			return err
		}
		if pairs != nil && !slices.Equal(key, []string{s.key}) {
			return d.fail(&connector.DocumentError{Stream: s.Name, Reason: fmt.Sprintf("its table %s is keyed by %v now, not %s", table, key, s.key)})
		}
		var gone []string
		if s.pause != paused {
			rows := map[[16]byte]string{}
			for externalID := range s.held {
				if s.reading == readingFull && !s.seen[externalID] {
					gone = append(gone, externalID)
					continue
				}
				if pairs == nil {
					continue
				}
				hash, err := snapshot.KeyHash(key, map[string]any{s.key: externalID})
				if err != nil {
					return d.fail(retryable(fmt.Errorf("hash the key of document %s: %w", externalID, err)))
				}
				rows[hash] = externalID
			}
			for _, p := range pairs {
				delete(rows, p.Key)
			}
			for _, externalID := range rows {
				gone = append(gone, externalID)
			}
			// A read with no Put leaves a set over n as it found it (n lowered).
			if n := s.Keep.Newest(); n > 0 && len(s.held)-len(gone) > n {
				out := map[string]bool{}
				for _, externalID := range gone {
					out[externalID] = true
				}
				var window []model.ConnectorDocument
				for externalID, doc := range s.held {
					if !out[externalID] {
						window = append(window, doc)
					}
				}
				slices.SortFunc(window, older)
				for _, doc := range window[:len(window)-n] {
					gone = append(gone, doc.ExternalID)
				}
			}
		}
		if err := d.remove(ctx, s, gone); err != nil {
			return err
		}
		if s.pause == catchingUp && s.reading == readingFull {
			sync := d.r.sync
			if err := d.r.s.DB.ClearFoundryDocumentSetSkipped(ctx, sync.WorkspaceID, s.setID, sync.ID, leaseOwner(sync)); err != nil {
				return d.fail(published(err, "clear the "+s.Name+" document set's skip"))
			}
			s.pause = unpaused
		}
		s.reading, s.offers, s.seen, s.wanted = notReading, 0, nil, nil
	}
	return nil
}

// prune empties the connector's sets that this sync writes no stream of:
// the stream was turned off, or its table is no longer synced. Each set
// stays, so turning the stream on again fills the same one. A paused
// stream is written, so its set stays as it is.
func (d *documents) prune(ctx context.Context) error {
	for _, set := range d.sets {
		if d.streams[set.Owner.Stream] != nil || set.Documents == 0 {
			continue
		}
		held, err := d.index(ctx, set.ID)
		if err != nil {
			return err
		}
		s := &stream{Stream: connector.Stream{Name: set.Owner.Stream}, setID: set.ID, held: held}
		if err := d.remove(ctx, s, slices.Collect(maps.Keys(held))); err != nil {
			return err
		}
	}
	return nil
}

// remove publishes the removal of the stream's documents of externalIDs, in
// chunks of removeChunk.
func (d *documents) remove(ctx context.Context, s *stream, externalIDs []string) error {
	slices.Sort(externalIDs)
	for chunk := range slices.Chunk(externalIDs, removeChunk) {
		s.batch = batch{gone: map[string]bool{}, outcomes: map[string]int{"removed": len(chunk)}}
		for _, externalID := range chunk {
			s.batch.gone[externalID] = true
			s.batch.remove = append(s.batch.remove, s.held[externalID].ID)
		}
		if err := d.flush(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// fail records the first error that ends the attempt, a DocumentError as
// the permanent Failure it is, and returns the one recorded.
func (d *documents) fail(err error) error {
	if d.err == nil {
		var docErr *connector.DocumentError
		if errors.As(err, &docErr) {
			err = documentFailure(docErr)
		}
		d.err = err
	}
	return d.err
}

func (d *documents) count(outcome string, n int) {
	syncDocuments.WithLabelValues(d.r.kind.Name(), outcome).Add(float64(n))
}

// documentFailure is a kind's document bug, which no retry fixes.
func documentFailure(err *connector.DocumentError) error {
	return &Failure{Message: "a document this connection read can't be stored: " + err.Error(), Permanent: true, Err: err}
}

// printable reports 1 to max bytes of printable ASCII, the space only when space.
func printable(s string, max int, space bool) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; c > '~' || c < ' ' || c == ' ' && !space {
			return false
		}
	}
	return true
}

// documentName cleans a title to a document name (document.ValidName):
// valid UTF-8 with no slash or control character, trimmed, at most 255
// characters; the external id, cleaned the same way, when nothing is left.
func documentName(title, externalID string) string {
	clean := func(s string) string {
		s = strings.Map(func(r rune) rune {
			switch {
			case r == '/' || r == '\\':
				return '-'
			case unicode.IsControl(r):
				return ' '
			}
			return r
		}, strings.ToValidUTF8(s, "�"))
		s = strings.TrimSpace(s)
		if runes := []rune(s); len(runes) > 255 {
			s = strings.TrimSpace(string(runes[:255]))
		}
		return s
	}
	if name := clean(title); name != "" {
		return name
	}
	return clean(externalID)
}

// sourceURL is raw when it is an absolute https URL of at most 2048 bytes; nil otherwise.
func sourceURL(raw string) *string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || len(raw) > 2048 {
		return nil
	}
	return &raw
}

// truncate cuts text over max bytes at the last line that fits, with a
// marker saying how long it was, and reports whether it did. When that line
// would keep less than half of what fits, such as a heading before one
// long line, the cut is at the last character that fits instead.
func truncate(text []byte, max int64) ([]byte, bool) {
	if int64(len(text)) <= max {
		return text, false
	}
	marker := fmt.Appendf(nil, "\n[Truncated: the source holds %d bytes]\n", len(text))
	keep := max - int64(len(marker))
	if keep < 0 {
		marker, keep = nil, max
	}
	head := text[:keep]
	if i := bytes.LastIndexByte(head, '\n'); i >= 0 && i >= len(head)/2 {
		head = head[:i]
	} else {
		for len(head) > 0 && !utf8.RuneStart(text[len(head)]) {
			head = head[:len(head)-1]
		}
	}
	return append(slices.Clip(head), marker...), true
}
