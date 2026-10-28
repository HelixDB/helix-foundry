package syncer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"time"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Failure is why a sync stopped. Message is written for the customer; Err
// holds the detail for the log. A permanent failure cannot be fixed by retrying.
type Failure struct {
	Message   string
	Permanent bool
	// PlanRequired is a kind the workspace's plan no longer includes; buying
	// the plan again fixes it, so the schedule checks back daily.
	PlanRequired bool
	Err          error
}

func (f *Failure) Error() string {
	if f.Err == nil {
		return f.Message
	}
	return f.Message + ": " + f.Err.Error()
}

func (f *Failure) Unwrap() error { return f.Err }

type store interface {
	WorkspaceDeleting(ctx context.Context, id string) (bool, error)
	GetConnector(ctx context.Context, workspaceID, id string) (*model.Connector, error)
	ListConnectorTables(ctx context.Context, workspaceID, connectorID string) ([]*model.ConnectorTable, error)
	UpsertConnectorTable(ctx context.Context, t *model.ConnectorTable, batch *model.ConnectorChangeBatch, workerID string) error
	TouchConnectorTable(ctx context.Context, connectorID, table, syncID string, reconciled bool, workerID string) error
	PruneConnectorTables(ctx context.Context, workspaceID, connectorID string, keep []string, syncID, workerID string) ([]string, error)
	UpdateConnectorCursor(ctx context.Context, connectorID string, cursor json.RawMessage, syncID, workerID string) error
	ClearConnectorCursor(ctx context.Context, connectorID string, cursor json.RawMessage) error
	ListConnectorChangeBatches(ctx context.Context, workspaceID, syncID string) ([]*model.ConnectorChangeBatch, error)
	ListFoundryDocumentSets(ctx context.Context, workspaceID string) ([]*model.FoundryDocumentSet, error)
	EnsureConnectorDocumentSet(ctx context.Context, set *model.FoundryDocumentSet, syncID, workerID string) (string, error)
	ListConnectorDocuments(ctx context.Context, workspaceID, setID, after string, limit int) ([]model.ConnectorDocument, error)
	ReserveConnectorDocuments(ctx context.Context, workspaceID string, ids []string, syncID, workerID string) error
	PublishConnectorDocuments(ctx context.Context, p model.ConnectorDocumentsPublish, syncID, workerID string) ([]model.FoundryDocumentRead, error)
	FoundryDocumentSetSpend(ctx context.Context, workspaceID string, setIDs []string, month time.Time) (map[string]model.FoundryDocumentSetSpend, error)
	MarkFoundryDocumentSetSkipped(ctx context.Context, workspaceID, setID string, month time.Time) error
	ClearFoundryDocumentSetSkipped(ctx context.Context, workspaceID, setID, syncID, workerID string) error
	SumFoundryGraphJobSpend(ctx context.Context, workspaceID string, since time.Time) (int64, error)
}

type planReader interface {
	Status(ctx context.Context, workspaceID string) (foundryplan.Status, error)
}

// Refresher marks the ontologies whose current load reads a changed table.
type Refresher interface {
	OweFoundryRefreshes(ctx context.Context, workspaceID, connectorID string, tables []string, refreshEvery time.Duration) error
}

// Syncer copies a connector's tables into snapshots.
type Syncer struct {
	DB      store
	Catalog *connector.Catalog
	Keys    *secretbox.Keyring
	// Plans is nil when the deployment sells no Foundry plan; every workspace is then Free.
	Plans       planReader
	PlanCatalog *foundryplan.Catalog
	// Refresh is nil where graphs are not refreshed.
	Refresh   Refresher
	Snapshots snapshot.Store
	MaxRows   int64
	// MaxBytes caps a table's snapshot, uncompressed.
	MaxBytes int64
	TempDir  string
	Now      func() time.Time
	// Documents holds the originals of connectors' documents, beside
	// uploads' (foundry.sync.data_bucket); a kind with streams needs it and
	// MonthBudget.
	Documents documentStore
	// MaxDocumentBytes caps one document's text (foundry.documents.max_bytes);
	// MaxPerStream the documents a full read keeps of one stream
	// (foundry.documents.max_per_stream).
	MaxDocumentBytes int64
	MaxPerStream     int
	// PurgeGrace is how long the sweep leaves a purge row
	// (foundry.sync.replaced_snapshot_grace): a flush publishes within half of it.
	PurgeGrace time.Duration
	// MonthBudget is the graph budget the extracts of new documents draw on
	// (config.FoundryGraphConfig.MonthBudget).
	MonthBudget func(now time.Time) model.FoundryGraphBudget
	// Allowances are, by connector kind, what work no person started may
	// spend in a UTC month extracting the sets a kind's streams keep
	// (config.FoundryConfig.SetAllowances). A sync of such a kind pauses a
	// stream while its set's allowance or the workspace's is spent, or the
	// month's graph budget leaves such work nothing: it writes and removes
	// none of the stream's documents, and the first sync with room again
	// reads in full. Their documents fan out as work no person started
	// (model.FoundryDocumentFanOut.SystemOnly). A kind it leaves out has none.
	Allowances map[string]model.FoundrySetAllowance
}

// Result is what a sync did, for the worker to schedule the next one.
type Result struct {
	Rows int64
	// StoredRows are the rows the connector's tables held as the sync began,
	// once it read them; 0 when it stopped before. An event source's
	// spacing grows with them when its sync fails, since a sync that failed
	// part way wrote fewer rows than the merge it may have run read.
	StoredRows int64
	// Plan is the plan the sync ran under; nil when it stopped before reading it.
	Plan *foundryplan.Plan
	// Mode is how the sync read: changes when it followed its tables' changes.
	Mode model.ConnectorSyncMode
	// Changed are the tables whose content the sync changed.
	Changed []string
	// SyncEverySeconds is the connector's own interval: nil the plan's, 0 manual only.
	SyncEverySeconds *int
	// MinEvery is the kind's own floor on the interval under these settings; 0 when it has none.
	MinEvery time.Duration
	// Pushed is set for a Pushed kind once the sync stored its cursor; nil
	// for any other kind, and for a sync that stopped before.
	Pushed *PushedResult
}

// PushedResult is what the worker schedules a Pushed kind's next sync by.
type PushedResult struct {
	// Backlog is what waits beyond the cursor the sync stored.
	Backlog connector.Backlog
	// Rows are the rows the kind's tables hold, which the spacing grows with.
	Rows int64
}

func (s *Syncer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// releaseTimeout bounds giving back what a failed first read took, which dials the source.
const releaseTimeout = 30 * time.Second

// Run copies every selected table and returns what it did. Tables are
// independent: one that completes keeps its new snapshot even if a later one fails.
// Every connector_tables write is refused once the sync's lease is lost.
// A kind's documents (connector.DocumentKind) are read with their table's
// rows and publish before its snapshot does (documents).
//  1. Load the workspace and connector; either being deleted ends the sync.
//  2. Read the plan, which may have lapsed since the sync was queued; its
//     rules say which capture methods the sync may use.
//  3. Re-discover with the stored credential, which also proves it still works.
//     A table in scope (chosen, or synced before when none is chosen) that the
//     source now cannot read fails the sync rather than vanishing.
//  4. Read in full or read changes (readMode); in full when a stream
//     catches up on what a paused sync skipped (Allowances). A full read
//     compares each table with its snapshot; a change read merges into it.
//     A table that did not change keeps its snapshot; one that did gets a
//     new snapshot and a change batch, in one lease-guarded write.
//  5. Once every table is in, empty the document sets of streams no longer
//     written, drop the tables no longer selected or discovered, then store
//     the cursor the next change read starts from.
//  6. Mark the ontologies that read a changed table as owing a refresh.
//  7. For a Pushed kind, read what waits beyond the stored cursor.
func (s *Syncer) Run(ctx context.Context, sync *model.ConnectorSync) (res Result, err error) {
	deleting, err := s.DB.WorkspaceDeleting(ctx, sync.WorkspaceID)
	if errors.Is(err, foundrystore.ErrNotFound) || err == nil && deleting {
		return res, &Failure{Message: "the workspace was deleted", Permanent: true}
	}
	if err != nil {
		return res, retryable(fmt.Errorf("load workspace: %w", err))
	}
	c, err := s.DB.GetConnector(ctx, sync.WorkspaceID, sync.ConnectorID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return res, &Failure{Message: "the connector was deleted", Permanent: true}
	}
	if err != nil {
		return res, retryable(fmt.Errorf("load connector: %w", err))
	}
	res.SyncEverySeconds = c.SyncEverySeconds
	kind, err := s.Catalog.Kind(c.Kind)
	if err != nil {
		return res, &Failure{Message: "this connector kind is no longer supported", Permanent: true, Err: err}
	}
	if floor, ok := kind.(connector.IntervalFloor); ok {
		res.MinEvery = floor.MinInterval(connector.Source{Settings: c.Config, WorkspaceID: c.WorkspaceID, ConnectorID: c.ID})
	}
	plan, err := s.plan(ctx, c.WorkspaceID, kind)
	if err != nil {
		return res, err
	}
	res.Plan = plan
	secret, err := s.Keys.Open(c.SecretCiphertext, c.SecretKeyID, connector.SecretContext(c.ID))
	if err != nil {
		return res, retryable(fmt.Errorf("open connector credential: %w", err))
	}
	src := connector.Source{Settings: c.Config, Secret: string(secret), WorkspaceID: c.WorkspaceID, ConnectorID: c.ID, ForSync: true}
	// A first full read whose Start took a slot, and that then fails for good,
	// stores no cursor: no later sync or release tick would know to drop it.
	if releaser, ok := kind.(connector.Releaser); ok && c.ChangeCursor == nil {
		defer func() {
			var failure *Failure
			if !errors.As(err, &failure) || !failure.Permanent {
				return
			}
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
			defer cancel()
			if rerr := releaser.Release(releaseCtx, src, nil); rerr != nil {
				log.From(ctx).Warn("release after a failed first read failed", "connector", c.ID, "err", rerr)
			}
		}()
	}
	for _, capture := range plan.Sync.Capture {
		src.Captures = append(src.Captures, connector.Capture(capture))
	}
	discovery, err := kind.Discover(ctx, src)
	if err != nil {
		return res, classify(err)
	}
	// Discovery may correct a setting, such as a bucket's region, for this
	// sync only: the next test or edit stores it.
	if discovery.Settings != nil {
		src.Settings = discovery.Settings
	}
	stored, err := s.DB.ListConnectorTables(ctx, c.WorkspaceID, c.ID)
	if err != nil {
		return res, retryable(fmt.Errorf("list connector tables: %w", err))
	}
	previous := map[string]*model.ConnectorTable{}
	for _, t := range stored {
		previous[t.TableName] = t
		res.StoredRows += t.RowCount
	}
	choice := tableChoice(c.Config)
	for _, u := range discovery.Unavailable {
		if _, wasSynced := previous[u.Name]; slices.Contains(choice, u.Name) || len(choice) == 0 && wasSynced {
			reason := u.Reason
			if reason == "" {
				reason = "the connection cannot read it"
			}
			return res, &Failure{Message: fmt.Sprintf("table %s can't be synced: %s", u.Name, reason), Permanent: true}
		}
	}
	tables := selected(discovery, choice)
	r := &run{s: s, sync: sync, kind: kind, src: src, previous: previous}
	if r.docs, err = r.openDocuments(ctx, tables); err != nil {
		return res, err
	}
	// An earlier attempt's changes still owe their refreshes, though this
	// attempt keeps those tables or finds nothing new in them.
	if sync.Attempts > 1 {
		batches, err := s.DB.ListConnectorChangeBatches(ctx, c.WorkspaceID, sync.ID)
		if err != nil {
			return res, retryable(fmt.Errorf("list earlier attempts' changes: %w", err))
		}
		for _, b := range batches {
			if b.Kind != model.ConnectorDropBatch && !slices.Contains(r.earlier, b.TableName) {
				r.earlier = append(r.earlier, b.TableName)
			}
		}
	}
	reader, follows := kind.(connector.ChangeReader)
	// A Pushed kind's rows arrive only through Helix: a change read is the
	// only way to see them, so the plan's capture methods don't apply.
	pushed, isPushed := kind.(connector.Pushed)
	followed := map[string]bool{}
	if follows {
		for _, t := range tables {
			capture := reader.Capture(src, t.Name)
			// Changes merge by key, so a keyless table is read in full on every sync.
			keyed := len(t.PrimaryKey) > 0 || previous[t.Name] != nil && len(prevKey(previous[t.Name])) > 0
			followed[t.Name] = keyed && capture != connector.CaptureFull && (isPushed || plan.Sync.Allows(foundryplan.Capture(capture)))
		}
	}
	cursor := c.ChangeCursor
	res.Mode = model.ConnectorSyncFull
	var expired error
	if !r.docs.catchUp() && s.readMode(sync, c, plan, tables, previous, followed, isPushed) == model.ConnectorSyncChanges {
		next, err := r.readChanges(ctx, reader, tables, followed, c.ChangeCursor)
		switch {
		case errors.Is(err, connector.ErrCursorExpired):
			expired = err
		case err != nil:
			return r.result(res), err
		default:
			cursor, res.Mode = next, model.ConnectorSyncChanges
		}
	}
	if res.Mode == model.ConnectorSyncFull {
		if cursor, err = r.readFull(ctx, reader, tables, followed); err != nil {
			return r.result(res), err
		}
		cursor = connector.KeepLastReset(cursor, c.ChangeCursor, expired)
	}
	if err := r.docs.prune(ctx); err != nil {
		return r.result(res), err
	}
	if _, err := s.DB.PruneConnectorTables(ctx, c.WorkspaceID, c.ID, r.synced, sync.ID, leaseOwner(sync)); err != nil {
		return r.result(res), published(err, "drop unselected tables")
	}
	// A connector that stops following changes gives back what it held, such as a replication slot.
	if cursor == nil && c.ChangeCursor != nil {
		if releaser, ok := kind.(connector.Releaser); ok {
			if err := releaser.Release(ctx, src, c.ChangeCursor); err != nil {
				return r.result(res), classify(err)
			}
		}
	}
	if err := s.DB.UpdateConnectorCursor(ctx, c.ID, cursor, sync.ID, leaseOwner(sync)); err != nil {
		return r.result(res), published(err, "store the change cursor")
	}
	res = r.result(res)
	if len(res.Changed) > 0 && s.Refresh != nil {
		if err := s.Refresh.OweFoundryRefreshes(ctx, c.WorkspaceID, c.ID, res.Changed, plan.Sync.RefreshEvery); err != nil {
			return res, retryable(fmt.Errorf("mark graph refreshes: %w", err))
		}
	}
	// Read last, so a failure here has already owed this sync's refreshes,
	// and the retry merges nothing twice.
	if isPushed {
		backlog, err := pushed.Backlog(ctx, src, cursor)
		if err != nil {
			return res, retryable(fmt.Errorf("read what waits beyond the cursor: %w", err))
		}
		res.Pushed = &PushedResult{Backlog: backlog, Rows: res.Rows}
	}
	return res, nil
}

// Release gives back what a connector's change cursor holds on its source,
// such as a replication slot, then forgets the cursor. The sync worker calls
// it for connectors no sync will read again.
func (s *Syncer) Release(ctx context.Context, c *model.Connector) error {
	kind, err := s.Catalog.Kind(c.Kind)
	if err != nil {
		return err
	}
	if releaser, ok := kind.(connector.Releaser); ok {
		secret, err := s.Keys.Open(c.SecretCiphertext, c.SecretKeyID, connector.SecretContext(c.ID))
		if err != nil {
			return fmt.Errorf("open connector %s credential: %w", c.ID, err)
		}
		src := connector.Source{Settings: c.Config, Secret: string(secret), WorkspaceID: c.WorkspaceID, ConnectorID: c.ID}
		if err := releaser.Release(ctx, src, c.ChangeCursor); err != nil {
			return fmt.Errorf("release connector %s: %w", c.ID, err)
		}
	}
	return s.DB.ClearConnectorCursor(ctx, c.ID, c.ChangeCursor)
}

// ReleaserKinds are the catalog's kinds that hold something on their source between syncs.
func (s *Syncer) ReleaserKinds() []string {
	var kinds []string
	for _, k := range s.Catalog.Kinds() {
		if _, ok := k.(connector.Releaser); ok {
			kinds = append(kinds, k.Name())
		}
	}
	return kinds
}

// readMode decides between reading changes and reading in full. A sync reads
// in full when asked to, when the connector has no cursor or no table can be
// followed under the plan, when a table is new or predates digests, when a
// followed table's primary key changed (a merge matches rows by the stored
// key), and when a followed table's reconcile is due: the full read is what
// catches deletes and edits a change read can't see. A pushed kind's rows
// change nowhere a change read misses, so it is never reconciled.
func (s *Syncer) readMode(sync *model.ConnectorSync, c *model.Connector, plan *foundryplan.Plan, tables []connector.Table,
	previous map[string]*model.ConnectorTable, followed map[string]bool, pushed bool) model.ConnectorSyncMode {
	if sync.Mode == model.ConnectorSyncFull || c.ChangeCursor == nil || plan.Sync.ReconcileEvery <= 0 && !pushed {
		return model.ConnectorSyncFull
	}
	anyFollowed := false
	for _, t := range tables {
		p := previous[t.Name]
		if p == nil || p.ContentDigest == nil {
			return model.ConnectorSyncFull
		}
		if followed[t.Name] {
			anyFollowed = true
			if !pushed && (p.ReconciledAt == nil || !s.now().Before(p.ReconciledAt.Add(plan.Sync.ReconcileEvery))) {
				return model.ConnectorSyncFull
			}
			if len(t.PrimaryKey) > 0 && !slices.Equal(t.PrimaryKey, prevKey(p)) {
				return model.ConnectorSyncFull
			}
		}
	}
	if !anyFollowed {
		return model.ConnectorSyncFull
	}
	return model.ConnectorSyncChanges
}

// plan reads the workspace's plan and refuses a kind it doesn't include. An
// Autumn outage runs a kind Free includes on Free's rules, so it never stops
// a Free connector, and retries any other.
func (s *Syncer) plan(ctx context.Context, workspaceID string, kind connector.Kind) (*foundryplan.Plan, error) {
	free := s.PlanCatalog.Free()
	plan := free
	if s.Plans != nil {
		status, err := s.Plans.Status(ctx, workspaceID)
		switch {
		case err != nil && free.Includes(kind.Name()):
		case err != nil:
			return nil, retryable(fmt.Errorf("read foundry plan: %w", err))
		default:
			plan = status.Plan
		}
	}
	if plan.Includes(kind.Name()) {
		return plan, nil
	}
	needed := "a plan this installation does not sell"
	if lowest := s.PlanCatalog.LowestWith(kind.Name()); lowest != nil {
		needed = "Foundry " + lowest.DisplayName
	}
	return nil, &Failure{Message: "this connector needs " + needed, Permanent: true, PlanRequired: true}
}

// run is one attempt's state: what it read, and the tables it wrote.
type run struct {
	s        *Syncer
	sync     *model.ConnectorSync
	kind     connector.Kind
	src      connector.Source
	previous map[string]*model.ConnectorTable
	docs     *documents
	rows     int64
	synced   []string
	changed  []string
	// earlier are the tables an earlier attempt of this sync changed.
	earlier []string
}

func (r *run) result(res Result) Result {
	res.Rows, res.Changed = r.rows, slices.Clone(r.changed)
	for _, t := range r.earlier {
		if !slices.Contains(res.Changed, t) {
			res.Changed = append(res.Changed, t)
		}
	}
	return res
}

// done reports a table this sync already wrote on an earlier attempt, which
// is not read again. A followed table is always read again: this attempt's
// cursor is later than the one the earlier attempt read up to, so skipping
// it would lose the changes in between.
func (r *run) done(table string, followed bool) bool {
	if p := r.previous[table]; !followed && p != nil && p.SyncID == r.sync.ID {
		r.keep(p)
		return true
	}
	return false
}

// keep counts a table whose snapshot stays as it is.
func (r *run) keep(p *model.ConnectorTable) {
	r.rows += p.RowCount
	r.synced = append(r.synced, p.TableName)
}

// readFull reads every table in full. When the kind can follow some of
// them, the cursor is taken first, so every change made during the read is
// read again next time; it is stored once every table is in.
func (r *run) readFull(ctx context.Context, reader connector.ChangeReader, tables []connector.Table, followed map[string]bool) (json.RawMessage, error) {
	var cursor json.RawMessage
	src := r.src
	var follow []connector.Table
	for _, t := range tables {
		if followed[t.Name] {
			follow = append(follow, t)
		}
	}
	if len(follow) > 0 {
		start, err := reader.Start(ctx, r.src, follow)
		if err != nil {
			return nil, classify(err)
		}
		if start.Close != nil {
			defer start.Close()
		}
		cursor = start.Cursor
		if start.Source != nil {
			src = *start.Source
		}
	}
	if err := r.readTables(ctx, src, tables, followed); err != nil {
		return nil, err
	}
	return cursor, nil
}

// readTables reads tables in full, one at a time, or all in one pass for a
// PassKind. A table an earlier attempt of this sync wrote is not read again
// (done); a pass still reads it, into writers that discard it.
func (r *run) readTables(ctx context.Context, src connector.Source, tables []connector.Table, followed map[string]bool) error {
	if pass, ok := r.kind.(connector.PassKind); ok {
		return r.passTables(ctx, pass, src, tables, followed)
	}
	for _, table := range tables {
		if r.done(table.Name, followed[table.Name]) {
			continue
		}
		if err := r.fullTable(ctx, src, table); err != nil {
			return err
		}
	}
	return nil
}

// passTables reads tables in one SyncPass, then publishes each in turn.
func (r *run) passTables(ctx context.Context, pass connector.PassKind, src connector.Source, tables []connector.Table,
	followed map[string]bool) error {
	rows := map[string]connector.RowWriter{}
	writers := map[string]*passWriter{}
	var read []connector.Table
	for _, table := range tables {
		mode := readingFull
		if r.done(table.Name, followed[table.Name]) {
			rows[table.Name], mode = discard{}, discarding
		} else {
			w, err := snapshot.NewWriter(r.s.TempDir, r.s.MaxRows, r.s.MaxBytes)
			if err != nil {
				return retryable(err)
			}
			defer func() { _ = w.Close() }() // best-effort: removes the temp file
			writers[table.Name] = &passWriter{Writer: w}
			rows[table.Name] = writers[table.Name]
			read = append(read, table)
		}
		if err := r.docs.begin(ctx, table.Name, mode); err != nil {
			return err
		}
	}
	if len(read) == 0 {
		return nil
	}
	written, err := pass.SyncPass(ctx, src, tables, rows, r.docs)
	// The writer that hit a limit names the table that went over it.
	over := ""
	for _, t := range read {
		if errors.Is(writers[t.Name].err, snapshot.ErrRowLimit) || errors.Is(writers[t.Name].err, snapshot.ErrByteLimit) {
			over = t.Name
			break
		}
	}
	if err := r.readFailure(over, err); err != nil {
		return err
	}
	for _, table := range read {
		got := table
		if i := slices.IndexFunc(written, func(t connector.Table) bool { return t.Name == table.Name }); i >= 0 {
			got = written[i]
		}
		if err := r.publishFull(ctx, table, got, writers[table.Name].Writer); err != nil {
			return err
		}
	}
	return nil
}

// passWriter is a table's snapshot in a pass, keeping the error that
// stopped it, so a limit names its table.
type passWriter struct {
	*snapshot.Writer
	err error
}

func (w *passWriter) Write(row map[string]any) error { return w.keep(w.Writer.Write(row)) }
func (w *passWriter) WriteJSON(row []byte) error     { return w.keep(w.Writer.WriteJSON(row)) }

func (w *passWriter) keep(err error) error {
	if w.err == nil {
		w.err = err
	}
	return err
}

// discard takes the rows of a table a pass reads that an earlier attempt
// of the sync already published.
type discard struct{}

func (discard) Write(map[string]any) error { return nil }
func (discard) WriteJSON([]byte) error     { return nil }

// readChanges reads every followed table's changes in one pass, merges each
// into its snapshot, and reads in full the tables the kind can't follow or
// asked to reset.
func (r *run) readChanges(ctx context.Context, reader connector.ChangeReader, tables []connector.Table,
	followed map[string]bool, cursor json.RawMessage) (json.RawMessage, error) {
	var follow []connector.Table
	for _, t := range tables {
		if !followed[t.Name] {
			continue
		}
		// A sync's discovery may list a table without its schema; the change read gets the stored one.
		if p := r.previous[t.Name]; p != nil && len(t.Columns) == 0 {
			_ = json.Unmarshal(p.Columns, &t.Columns) // written by jsonArray, always an array
			t.PrimaryKey = prevKey(p)
		}
		follow = append(follow, t)
	}
	for _, t := range follow {
		if err := r.docs.begin(ctx, t.Name, readingChanges); err != nil {
			return nil, err
		}
	}
	w := &changeSets{DocumentWriter: r.docs, dir: r.s.TempDir, sets: map[string]*snapshot.ChangeSet{}, resets: map[string]bool{},
		columns: map[string][]connector.Column{}}
	defer w.close()
	next, err := reader.Changes(ctx, r.src, follow, cursor, w)
	if r.docs.err != nil {
		return nil, r.docs.err
	}
	if errors.Is(err, connector.ErrCursorExpired) {
		return nil, err
	}
	if err != nil {
		return nil, classify(err)
	}
	if w.err != nil {
		return nil, retryable(fmt.Errorf("spool changes: %w", w.err))
	}
	// The tables read in full go last, together: a PassKind reads them in one pass.
	var full []connector.Table
	for _, table := range tables {
		set := w.sets[table.Name]
		switch {
		case !followed[table.Name] || w.resets[table.Name]:
			full = append(full, table)
		case set == nil || set.Empty():
			r.keep(r.previous[table.Name])
			err = r.docs.finish(ctx, table.Name, nil, nil)
		default:
			err = r.mergeTable(ctx, table, set, w.columns[table.Name])
		}
		if err != nil {
			return nil, err
		}
	}
	if err := r.readTables(ctx, r.src, full, followed); err != nil {
		return nil, err
	}
	return next, nil
}

// limitFailure names the limit a table went over, or returns nil.
func (r *run) limitFailure(table string, err error) error {
	switch {
	case errors.Is(err, snapshot.ErrRowLimit):
		return &Failure{Message: fmt.Sprintf("table %s has more than %d rows; choose fewer tables", table, r.s.MaxRows), Permanent: true, Err: err}
	case errors.Is(err, snapshot.ErrByteLimit):
		return &Failure{Message: fmt.Sprintf("table %s holds more than %s, the most one table may hold", table, connector.FormatBytes(r.s.MaxBytes)),
			Permanent: true, Err: err}
	}
	return nil
}

// readFailure is why a kind's read of table failed, or nil: a document it
// offered wrong or that could not be stored, the table over a limit, or the
// kind's own error.
func (r *run) readFailure(table string, err error) error {
	if r.docs.err != nil {
		return r.docs.err
	}
	if limit := r.limitFailure(table, err); limit != nil {
		return limit
	}
	if err != nil {
		return classify(err)
	}
	return nil
}

// fullTable reads one table in full, with its streams' documents, then
// publishes it (publishFull).
func (r *run) fullTable(ctx context.Context, src connector.Source, table connector.Table) error {
	s := r.s
	w, err := snapshot.NewWriter(s.TempDir, s.MaxRows, s.MaxBytes)
	if err != nil {
		return retryable(err)
	}
	defer func() { _ = w.Close() }() // best-effort: removes the temp file
	if err := r.docs.begin(ctx, table.Name, readingFull); err != nil {
		return err
	}
	var written connector.Table
	if kind, ok := r.kind.(connector.DocumentKind); ok && r.docs.carries(table.Name) {
		written, err = kind.SyncDocuments(ctx, src, table, w, r.docs)
	} else {
		written, err = r.kind.Sync(ctx, src, table, w)
	}
	if err := r.readFailure(table.Name, err); err != nil {
		return err
	}
	return r.publishFull(ctx, table, written, w)
}

// publishFull hashes a table read in full, finishes its streams' documents
// against the hashes, and compares it with its snapshot: a table that did
// not change keeps it, one that did publishes a new one.
func (r *run) publishFull(ctx context.Context, table, written connector.Table, w *snapshot.Writer) error {
	s := r.s
	pairs, err := w.Hash(written.PrimaryKey)
	if err != nil {
		return retryable(fmt.Errorf("hash table %s: %w", table.Name, err))
	}
	if err := r.docs.finish(ctx, table.Name, written.PrimaryKey, pairs); err != nil {
		return err
	}
	digest := snapshot.Digest(pairs, len(written.PrimaryKey) > 0)
	// The kind's own columns win; a source known only from its rows has the writer's.
	columns := written.Columns
	if len(columns) == 0 {
		for _, name := range w.Columns() {
			columns = append(columns, connector.Column{Name: name})
		}
	}
	reconciled := s.now()
	row := &model.ConnectorTable{
		ID: id.New(), ConnectorID: r.sync.ConnectorID, WorkspaceID: r.sync.WorkspaceID, TableName: table.Name,
		Columns: jsonArray(columns), PrimaryKey: jsonArray(written.PrimaryKey), ForeignKeys: jsonArray(written.ForeignKeys),
		RowCount: w.Rows(), SyncID: r.sync.ID, SyncedAt: reconciled, ContentDigest: digest, ReconciledAt: &reconciled,
	}
	prev := r.previous[table.Name]
	if prev != nil && bytes.Equal(prev.ContentDigest, digest) {
		if sameJSON(prev.Columns, row.Columns) && sameJSON(prev.PrimaryKey, row.PrimaryKey) && sameJSON(prev.ForeignKeys, row.ForeignKeys) {
			if err := s.DB.TouchConnectorTable(ctx, r.sync.ConnectorID, table.Name, r.sync.ID, true, leaseOwner(r.sync)); err != nil {
				return published(err, "record table "+table.Name)
			}
			r.keep(prev)
			return nil
		}
		// The same rows under a new schema, such as a widened column type: the
		// snapshot stays, and the table records the schema later readers see.
		row.S3Key = prev.S3Key
		if err := s.DB.UpsertConnectorTable(ctx, row, nil, leaseOwner(r.sync)); err != nil {
			return published(err, "record table "+table.Name)
		}
		r.keep(row)
		return nil
	}
	batch := r.batch(table.Name, model.ConnectorReplaceBatch)
	batch.Inserts = w.Rows()
	var file *snapshot.BatchWriter
	if prev != nil && prev.ContentDigest != nil && len(written.PrimaryKey) > 0 && slices.Equal(prevKey(prev), written.PrimaryKey) {
		counts, f, err := r.compare(ctx, prev, w, written.Windowed || table.Windowed)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }() // best-effort: removes the temp file
		batch.Kind, batch.Inserts, batch.Updates, batch.Deletes = model.ConnectorChangesBatch, counts.Inserts, counts.Updates, counts.Deletes
		file = f
		if !counts.Changed() {
			// Rows moved out of a window without any changing.
			batch, file = nil, nil
		}
	}
	return r.publish(ctx, row, w, batch, file)
}

func (r *run) batch(table string, kind model.ConnectorChangeKind) *model.ConnectorChangeBatch {
	return &model.ConnectorChangeBatch{ID: id.New(), WorkspaceID: r.sync.WorkspaceID, ConnectorID: r.sync.ConnectorID,
		TableName: table, SyncID: r.sync.ID, Kind: kind}
}

// compare downloads the previous snapshot and finds what the new one changed.
func (r *run) compare(ctx context.Context, prev *model.ConnectorTable, w *snapshot.Writer, windowed bool) (snapshot.Counts, *snapshot.BatchWriter, error) {
	prevPath, err := snapshot.Download(ctx, r.s.Snapshots, prev.S3Key, r.s.TempDir)
	if err != nil {
		return snapshot.Counts{}, nil, retryable(fmt.Errorf("read the previous snapshot of %s: %w", prev.TableName, err))
	}
	defer func() { _ = os.Remove(prevPath) }() // best-effort: scratch file
	batch, err := snapshot.NewBatchWriter(r.s.TempDir)
	if err != nil {
		return snapshot.Counts{}, nil, retryable(err)
	}
	counts, err := snapshot.Compare(prevPath, w, windowed, batch)
	if err != nil {
		_ = batch.Close()
		return snapshot.Counts{}, nil, retryable(fmt.Errorf("compare table %s: %w", prev.TableName, err))
	}
	return counts, batch, nil
}

// mergeTable applies one table's changes to its snapshot. A Dense table
// whose read declared its columns has its stored rows padded to them, and
// stores them, types included, in place of the stored columns: a column
// stored while its type was still null takes its locked type.
func (r *run) mergeTable(ctx context.Context, table connector.Table, set *snapshot.ChangeSet, declared []connector.Column) error {
	s := r.s
	prev := r.previous[table.Name]
	key := prevKey(prev)
	prevPath, err := snapshot.Download(ctx, s.Snapshots, prev.S3Key, s.TempDir)
	if err != nil {
		return retryable(fmt.Errorf("read the previous snapshot of %s: %w", table.Name, err))
	}
	defer func() { _ = os.Remove(prevPath) }() // best-effort: scratch file
	w, err := snapshot.NewWriter(s.TempDir, s.MaxRows, s.MaxBytes)
	if err != nil {
		return retryable(err)
	}
	defer func() { _ = w.Close() }() // best-effort: removes the temp file
	file, err := snapshot.NewBatchWriter(s.TempDir)
	if err != nil {
		return retryable(err)
	}
	defer func() { _ = file.Close() }() // best-effort: removes the temp file
	w.SetKey(key)
	columns, dense := mergeColumns(prev.Columns, set.Columns), []string(nil)
	if table.Dense && declared != nil {
		columns = jsonArray(declared)
		for _, c := range declared {
			dense = append(dense, c.Name)
		}
	}
	counts, err := snapshot.Merge(prevPath, set, w, file, table.Version, dense)
	if limit := r.limitFailure(table.Name, err); limit != nil {
		return limit
	}
	if err != nil {
		return retryable(fmt.Errorf("merge table %s: %w", table.Name, err))
	}
	if !counts.Changed() {
		r.keep(prev)
		return r.docs.finish(ctx, table.Name, nil, nil)
	}
	pairs, err := w.Hash(key)
	if err != nil {
		return retryable(fmt.Errorf("hash table %s: %w", table.Name, err))
	}
	if err := r.docs.finish(ctx, table.Name, key, pairs); err != nil {
		return err
	}
	row := &model.ConnectorTable{
		ID: id.New(), ConnectorID: r.sync.ConnectorID, WorkspaceID: r.sync.WorkspaceID, TableName: table.Name,
		Columns: columns, PrimaryKey: prev.PrimaryKey, ForeignKeys: prev.ForeignKeys,
		RowCount: w.Rows(), SyncID: r.sync.ID, SyncedAt: s.now(), ContentDigest: snapshot.Digest(pairs, len(key) > 0),
		ReconciledAt: prev.ReconciledAt,
	}
	batch := r.batch(table.Name, model.ConnectorChangesBatch)
	batch.Inserts, batch.Updates, batch.Deletes = counts.Inserts, counts.Updates, counts.Deletes
	return r.publish(ctx, row, w, batch, file)
}

// publish uploads a table's new snapshot and its batch file, then points
// connector_tables at them with the batch recorded in the same write. A nil
// batch is a snapshot whose rows moved without changing.
func (r *run) publish(ctx context.Context, row *model.ConnectorTable, w *snapshot.Writer, batch *model.ConnectorChangeBatch,
	file *snapshot.BatchWriter) error {
	s := r.s
	row.S3Key = snapshot.Key(r.sync.WorkspaceID, r.sync.ConnectorID, row.TableName, id.New())
	if err := w.Upload(ctx, s.Snapshots, row.S3Key); err != nil {
		return retryable(err)
	}
	if file != nil {
		key := snapshot.BatchKey(r.sync.WorkspaceID, r.sync.ConnectorID, row.TableName, batch.ID)
		if err := file.Upload(ctx, s.Snapshots, key); err != nil {
			_ = s.Snapshots.Delete(ctx, row.S3Key) // best-effort: nothing points at it
			return retryable(err)
		}
		batch.S3Key = &key
	}
	if err := s.DB.UpsertConnectorTable(ctx, row, batch, leaseOwner(r.sync)); err != nil {
		_ = s.Snapshots.Delete(ctx, row.S3Key) // best-effort: nothing points at it
		if batch != nil && batch.S3Key != nil {
			_ = s.Snapshots.Delete(ctx, *batch.S3Key) // best-effort: nothing points at it
		}
		return published(err, "record table "+row.TableName)
	}
	r.rows += row.RowCount
	r.synced = append(r.synced, row.TableName)
	if batch != nil {
		r.changed = append(r.changed, row.TableName)
	}
	return nil
}

// changeSets is the ChangeWriter one change read fills: a spooled set per
// table, the columns the read declared for it, and the attempt's documents.
// The first spool error stops further writes and fails the read.
type changeSets struct {
	connector.DocumentWriter
	dir     string
	sets    map[string]*snapshot.ChangeSet
	resets  map[string]bool
	columns map[string][]connector.Column
	err     error
}

// The events kind declares its columns through it, by a type assertion: a
// change read of an event source refuses a writer that is not one.
var _ connector.ColumnWriter = (*changeSets)(nil)

func (c *changeSets) record(table string, fn func(*snapshot.ChangeSet) error) error {
	if c.err != nil {
		return c.err
	}
	set, ok := c.sets[table]
	if !ok {
		var err error
		if set, err = snapshot.NewChangeSet(c.dir); err != nil {
			c.err = err
			return err
		}
		c.sets[table] = set
	}
	if err := fn(set); err != nil {
		c.err = err
	}
	return c.err
}

func (c *changeSets) Upsert(table string, row map[string]any) error {
	return c.record(table, func(s *snapshot.ChangeSet) error { return s.Upsert(row) })
}

func (c *changeSets) UpsertJSON(table string, row []byte) error {
	return c.record(table, func(s *snapshot.ChangeSet) error { return s.UpsertJSON(row) })
}

func (c *changeSets) Delete(table string, key map[string]any) error {
	return c.record(table, func(s *snapshot.ChangeSet) error { return s.Delete(key) })
}

func (c *changeSets) DeleteVersion(table string, key map[string]any, version any) error {
	return c.record(table, func(s *snapshot.ChangeSet) error { return s.DeleteVersion(key, version) })
}

func (c *changeSets) DeleteWhere(table, column string, value any) error {
	return c.record(table, func(s *snapshot.ChangeSet) error { return s.DeleteWhere(column, value) })
}

func (c *changeSets) Reset(table, _ string) { c.resets[table] = true }

func (c *changeSets) Columns(table string, columns []connector.Column) {
	c.columns[table] = slices.Clone(columns)
}

func (c *changeSets) close() {
	for _, set := range c.sets {
		_ = set.Close() // best-effort: removes the spool file
	}
}

// prevKey is a stored table's primary key.
func prevKey(t *model.ConnectorTable) []string {
	var key []string
	_ = json.Unmarshal(t.PrimaryKey, &key) // written by jsonArray, always an array of names
	return key
}

// sameJSON compares stored JSON by value, since MySQL rewrites it on the way in.
func sameJSON(a, b json.RawMessage) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// mergeColumns adds columns first seen in upserted rows to the stored ones.
func mergeColumns(stored json.RawMessage, seen []string) json.RawMessage {
	var columns []connector.Column
	_ = json.Unmarshal(stored, &columns) // written by jsonArray, always an array
	for _, name := range seen {
		if !slices.ContainsFunc(columns, func(c connector.Column) bool { return c.Name == name }) {
			columns = append(columns, connector.Column{Name: name})
		}
	}
	return jsonArray(columns)
}

// leaseOwner is the worker the claim gave the sync to.
func leaseOwner(sync *model.ConnectorSync) string {
	if sync.LeaseOwner == nil {
		return ""
	}
	return *sync.LeaseOwner
}

func retryable(err error) error {
	return &Failure{Message: "the sync failed; Helix will retry", Err: err}
}

// published classifies a failed connector_tables write; a lost lease means
// another attempt owns the sync now, and this one's result is dropped.
func published(err error, what string) error {
	if errors.Is(err, foundrystore.ErrNotFound) {
		return retryable(fmt.Errorf("%s: lease lost", what))
	}
	return retryable(fmt.Errorf("%s: %w", what, err))
}

// tableChoice is the settings' chosen tables; none means every table.
func tableChoice(settings json.RawMessage) []string {
	var choice struct {
		Tables []string `json:"tables"`
	}
	_ = json.Unmarshal(settings, &choice) // settings without a choice sync every table
	return choice.Tables
}

// selected is the discovered tables, narrowed to the choice when there is one.
func selected(d connector.Discovery, choice []string) []connector.Table {
	if len(choice) == 0 {
		return d.Tables
	}
	var out []connector.Table
	for _, t := range d.Tables {
		if slices.Contains(choice, t.Name) {
			out = append(out, t)
		}
	}
	return out
}

// classify gives a connector error its customer message. A refusal can be a
// blip or a revoked key, so it is retried until attempts run out.
func classify(err error) error {
	var invalidErr *connector.InvalidError
	var refusedErr *connector.RefusedError
	var documentErr *connector.DocumentError
	switch {
	case errors.As(err, &documentErr):
		return documentFailure(documentErr)
	case errors.As(err, &invalidErr):
		return &Failure{Message: invalidErr.Message, Permanent: true, Err: err}
	case errors.As(err, &refusedErr):
		return &Failure{Message: refusedErr.Message, Err: err}
	case errors.Is(err, context.DeadlineExceeded):
		return &Failure{Message: "the sync took too long; Helix will retry", Err: err}
	}
	return retryable(err)
}

// jsonArray encodes a slice, nil as []. The connector package's plain structs cannot fail to encode.
func jsonArray[T any](items []T) json.RawMessage {
	if items == nil {
		items = []T{}
	}
	b, err := json.Marshal(items)
	if err != nil {
		panic(err)
	}
	return b
}
