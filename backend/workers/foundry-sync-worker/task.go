package foundrysyncworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/dig"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/events"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/server/endpoint"
	"github.com/helixdb/helix-foundry/backend/server/tasks"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/helixdb/helix-foundry/backend/syncer"
)

const Name = "foundry-sync-worker"

type Deps struct {
	dig.In

	Cfg    *config.Config
	Logger *slog.Logger
	Health endpoint.Health `optional:"true"`
	DB     store.Store     `optional:"true"`
	// Plans says which plan each workspace holds, and so how its connectors
	// sync; nil where none is sold.
	Plans ports.PlanReader `optional:"true"`
	AWS   aws.Config       `optional:"true"`
	// Accounts gives signed-in connectors their tokens; nil where none sign in.
	Accounts ports.ProviderAccounts `optional:"true"`
	// SlackRelay calls Slack for a standalone install's installs of Helix's
	// Slack app, through Helix's relay; nil hosted, where each install's
	// own token does.
	SlackRelay syncer.SlackCallers `optional:"true"`
}

type syncQueue interface {
	ClaimConnectorSync(ctx context.Context, workerID string, lease time.Duration) (*model.ConnectorSync, error)
	RenewConnectorSyncLease(ctx context.Context, id, workerID string, lease time.Duration) error
	FinishConnectorSync(ctx context.Context, id, workerID string, outcome model.ConnectorSyncOutcome) error
	RequeueConnectorSync(ctx context.Context, id, workerID string) error
	QueueDueConnectorSyncs(ctx context.Context, limit int, eventsAfter time.Duration) (int, error)
	GetConnector(ctx context.Context, workspaceID, id string) (*model.Connector, error)
	ListReplacedSnapshots(ctx context.Context, before time.Time, limit int) ([]*model.ReplacedSnapshot, error)
	DeleteReplacedSnapshot(ctx context.Context, id string) error
	ListPinnedSnapshotKeys(ctx context.Context) ([]string, error)
	ListExpiredChangeBatches(ctx context.Context, before time.Time, limit int) ([]*model.ConnectorChangeBatch, error)
	DeleteConnectorChangeBatch(ctx context.Context, id string) error
	ListDueFoundryRefreshes(ctx context.Context, afterDue time.Time, afterID string, limit int) ([]*model.FoundryOntology, error)
	QueueFoundryRefresh(ctx context.Context, workspaceID, ontologyID string, job *model.FoundryGraphJob) (bool, error)
	ListFoundryRereads(ctx context.Context, classes []string, afterID string, limit int) ([]*model.FoundryOntology, error)
	QueueFoundryReread(ctx context.Context, workspaceID, ontologyID string, classes []string, job *model.FoundryGraphJob,
		budget model.FoundryGraphBudget, now time.Time) (bool, error)
	ListStrandedConnectorCursors(ctx context.Context, kinds []string, limit int) ([]*model.Connector, error)
	ListFoundryDocumentPurges(ctx context.Context, before time.Time, limit int) ([]*model.FoundryDocumentPurge, error)
	DeleteFoundryDocumentPurge(ctx context.Context, documentID string) error
	ListFoundryEventBatchPurges(ctx context.Context, deletedBefore time.Time, limit int) ([]*model.FoundryEventBatch, error)
	DeleteFoundryEventBatch(ctx context.Context, id string, deletedBefore time.Time) error
	ListFoundryEventSourcePurges(ctx context.Context, deletedBefore time.Time, after model.FoundryEventSourcePurge, limit int) (
		[]model.FoundryEventSourcePurge, error)
	FinishFoundryEventSourcePurge(ctx context.Context, p model.FoundryEventSourcePurge, deletedBefore time.Time) error
	DeleteExpiredEventSyncs(ctx context.Context, finishedBefore time.Time, limit int) (int, error)
}

// documentPurger deletes what the bucket holds of a document (document.Store).
type documentPurger interface {
	DeletePrefix(ctx context.Context, prefix string) error
}

// bucket is what the sweep and the purge delete from the Foundry data bucket.
type bucket interface {
	Delete(ctx context.Context, key string) error
	// DeletePrefix deletes every object under a folder, a key ending in "/".
	DeletePrefix(ctx context.Context, prefix string) error
}

type syncRunner interface {
	Run(ctx context.Context, sync *model.ConnectorSync) (syncer.Result, error)
}

// cursorReleaser gives back what a stranded connector holds on its source.
type cursorReleaser interface {
	Release(ctx context.Context, c *model.Connector) error
}

// sweepBatch is how many replaced snapshots, change batches, documents, event
// batches, event sources or event source syncs one sweep or purge tick deletes
// at most.
const sweepBatch = 500

// sweepEvery is how often the sweep runs.
const sweepEvery = time.Minute

// unscheduledRetry is when a sync that failed before reading the plan tries again.
const unscheduledRetry = time.Hour

// releaseEvery is how often stranded cursors are released; a source that
// can't be reached is tried again at the next tick.
const releaseEvery = 10 * time.Minute

// releaseTimeout bounds one connector's release, which dials its source.
const releaseTimeout = 30 * time.Second

// persistTries is how many times persist tries a write, and persistTimeout
// bounds each; three short tries fit a pod's termination grace.
const (
	persistTries   = 3
	persistTimeout = 5 * time.Second
)

// Task runs queued connector syncs one at a time.
type Task struct {
	deps     Deps
	cfg      config.SyncConfig
	queue    syncQueue
	runner   syncRunner
	workerID string
	now      func() time.Time
	worker   tasks.Worker
	// tempDir holds syncs' downloads and snapshot files.
	tempDir string
	// snapshots is where the sweep deletes replaced snapshots and expired
	// batches, and the purge deleted event sources' batch objects; documents
	// where it deletes purged documents.
	snapshots bucket
	documents documentPurger
	// jitter returns a fraction in [0, 1) that stretches an interval by up to 5%.
	jitter func() float64
	// persistWait is how long persist waits between tries.
	persistWait time.Duration
	// lag records how long a scheduled sync waited for its first claim.
	lag prometheus.Observer
	// releaser drops what deleted and paused connectors of releaseKinds hold on their sources.
	releaser     cursorReleaser
	releaseKinds []string
	// spacing is how long an event source waits between syncs (foundry.events).
	spacing model.EventSyncSpacing
	// sourcesPurged is the last deleted event source the purge is done
	// with, every one deleted before it done too (purgeEvents).
	sourcesPurged model.FoundryEventSourcePurge
	// accounts is the store's side of signed-in connectors' accounts, and
	// broker the host's (Deps.Accounts): nil where none sign in, and
	// keepAccounts does nothing.
	accounts accountQueue
	broker   ports.ProviderAccounts
}

func New(d Deps) (*Task, error) {
	if d.DB == nil {
		return nil, errors.New("foundry-sync-worker: db not configured")
	}
	keys, err := connector.NewKeyring(d.Cfg.Foundry.Connectors)
	if err != nil {
		return nil, err
	}
	// Event sources' batch objects share the bucket with the snapshots.
	client := s3.NewFromConfig(d.AWS)
	snapshots := snapshot.NewS3(client, d.Cfg.Foundry.Sync.DataBucket)
	documents := document.New(client, d.Cfg.Foundry.Sync.DataBucket)
	ev := d.Cfg.Foundry.Events
	endpoints := connector.DefaultEndpoints()
	endpoints.GitHubAppInstall = d.Cfg.Foundry.Connectors.GitHubAppInstallURL
	catalog := connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: d.Cfg.Foundry.Connectors.AllowPrivateHosts},
		connector.NewS3Options(d.Cfg, d.AWS), connector.EventsOptions{Batches: d.DB, Objects: snapshots,
			Budget:    events.ReadBudget{Batches: ev.MaxBatchesPerSync, Pieces: ev.MaxPiecesPerSync, Bytes: ev.MaxBytesPerSync},
			Extracted: ev.MaxExtracted})
	// Slack connectors read through Helix's Slack app fold its feed into
	// their day mirror, beside the snapshots; hosted holds the installs'
	// bot tokens they read with, and a standalone install reaches Slack
	// through Helix's relay (Deps.SlackRelay).
	sl := d.Cfg.Foundry.Slack
	var slackApps syncer.SlackApps
	if sl.Enabled && sl.Feed.Enabled {
		catalog = catalog.WithSlackFeed(connector.SlackFeedOptions{Feed: d.DB, Objects: snapshots,
			Budget:     model.SlackFeedBudget{Rows: sl.Feed.ReadBudget.Rows, Bytes: sl.Feed.ReadBudget.Bytes},
			HelixAppID: sl.AppID, DirectoryEvery: sl.Feed.DirectoryEvery, RedocumentEvery: sl.Feed.RedocumentEvery, RepliesDays: sl.Backfill.RepliesDays})
		if !d.Cfg.Host.Local || d.SlackRelay != nil {
			required, _ := slack.Required(sl.Feed.Enabled, sl.Follow.Enabled)
			slackApps = syncer.HostedSlackApps{DB: d.DB, Keys: keys, AppID: sl.AppID, Required: required, OrphanRetention: sl.Feed.OrphanRetention,
				Caller: d.SlackRelay}
		}
	}
	planCatalog, err := foundryplan.NewCatalog(d.Cfg.Billing.Plans, connector.KindNames())
	if err != nil {
		return nil, err
	}
	s := &syncer.Syncer{
		DB:               d.DB,
		Catalog:          catalog,
		Keys:             keys,
		PlanCatalog:      planCatalog,
		Refresh:          d.DB,
		Snapshots:        snapshots,
		MaxRows:          d.Cfg.Foundry.Sync.MaxRowsPerTable,
		MaxBytes:         d.Cfg.Foundry.Sync.MaxTableBytes,
		Documents:        documents,
		MaxDocumentBytes: d.Cfg.Foundry.Documents.MaxBytes,
		MaxPDFBytes:      d.Cfg.Foundry.Documents.MaxPDFBytes,
		MaxPerStream:     d.Cfg.Foundry.Documents.MaxPerStream,
		PurgeGrace:       d.Cfg.Foundry.Sync.ReplacedSnapshotGrace,
		MonthBudget:      d.Cfg.Foundry.Graph.MonthBudget,
		Allowances:       d.Cfg.Foundry.SetAllowances(),
		Plans:            d.Plans,
		Accounts:         d.Accounts,
		SlackApps:        slackApps,
	}
	return &Task{
		deps: d, cfg: d.Cfg.Foundry.Sync, queue: d.DB, runner: s,
		workerID: id.NewWorkerID(), now: time.Now, tempDir: os.TempDir(),
		snapshots: snapshots, documents: documents, jitter: rand.Float64, lag: syncLag, releaser: s, releaseKinds: s.ReleaserKinds(),
		spacing:     model.EventSyncSpacing{After: ev.SyncAfter, PerMillionRows: ev.SyncAfterPerMillionRows},
		persistWait: time.Second, accounts: d.DB, broker: d.Accounts,
	}, nil
}

func (t *Task) Name() string { return Name }

func (t *Task) Start(ctx context.Context) error {
	t.sweepTemp(ctx)
	return t.worker.Run(ctx, Name, t.cfg.HTTP.Listen, t.deps.Health, t.deps.Logger, func(ctx context.Context) error {
		// A sync can run for hours; the schedule and the accounts it keeps,
		// the refreshes, the sweep and the purge tick beside it.
		var side sync.WaitGroup
		for _, tick := range []struct {
			every time.Duration
			fn    func(context.Context)
		}{{t.cfg.SchedulePoll, t.schedule}, {t.cfg.SchedulePoll, t.keepAccounts}, {t.cfg.RefreshPoll, t.refresh}, {t.cfg.RefreshPoll, t.reread}, {sweepEvery, t.sweep},
			{sweepEvery, t.purgeEvents}, {releaseEvery, t.release}} {
			side.Add(1)
			go func() {
				defer side.Done()
				tasks.Tick(ctx, tick.every, tick.fn)
			}()
		}
		tasks.Tick(ctx, t.cfg.PollInterval, t.drain)
		side.Wait()
		return nil
	})
}

func (t *Task) Stop(ctx context.Context) error { return t.worker.Stop(ctx) }

// release drops what deleted and paused connectors still hold on their
// sources: a PostgreSQL replication slot no sync reads keeps WAL until the
// customer's disk fills. One whose source refuses is tried again next tick.
func (t *Task) release(ctx context.Context) {
	if len(t.releaseKinds) == 0 {
		return
	}
	stranded, err := t.queue.ListStrandedConnectorCursors(ctx, t.releaseKinds, sweepBatch)
	if err != nil {
		log.From(ctx).Warn("list stranded connector cursors failed", "err", err)
		return
	}
	for _, c := range stranded {
		releaseCtx, cancel := context.WithTimeout(ctx, releaseTimeout)
		err := t.releaser.Release(releaseCtx, c)
		cancel()
		if err != nil {
			cursorReleases.WithLabelValues("failed").Inc()
			log.From(ctx).Warn("release connector cursor failed", "connector", c.ID, "kind", c.Kind, "err", err)
			continue
		}
		cursorReleases.WithLabelValues("released").Inc()
	}
}

// sweepTemp removes the downloads, snapshot files and rendered pieces (the
// events kind's) a killed container left behind: the scratch volume
// outlives the container, and nothing else in the pod writes there, since it
// runs one sync at a time.
func (t *Task) sweepTemp(ctx context.Context) {
	for _, pattern := range []string{"foundry-s3-*", "foundry-snapshot-*", "foundry-pieces-*"} {
		matches, _ := filepath.Glob(filepath.Join(t.tempDir, pattern)) // the patterns are well formed
		for _, m := range matches {
			if err := os.RemoveAll(m); err != nil {
				log.From(ctx).Warn("remove a leftover sync file failed", "path", m, "err", err)
			}
		}
	}
}

// drain runs due syncs until none is left.
func (t *Task) drain(ctx context.Context) {
	for ctx.Err() == nil {
		sync, err := t.queue.ClaimConnectorSync(ctx, t.workerID, t.cfg.Lease)
		if err != nil {
			log.From(ctx).Error("claim connector sync failed", "err", err)
			return
		}
		if sync == nil {
			return
		}
		t.process(ctx, sync)
	}
}

// process runs one attempt while renewing its lease, then records the outcome.
// Losing the lease cancels the attempt: another worker owns the sync now.
//
// A sync claimed past its last attempt is not run. Only an attempt whose end
// was never recorded comes back past the last: its lease ran out, most likely
// because the sync killed its pod, and running it again would kill the next
// pod too, with no end. It fails for good and pauses the connector, worked
// out from the interval the connector has so the finish's guard lets the
// pause through. A connector that never stored a cursor gives back what its
// first read took, as a first read that fails for good does.
//
// So that only such an attempt comes back past the last, every other end is
// recorded: the outcome is written past the worker's stop and a passing
// error (persist), and an attempt the worker's stop cut short gives the sync
// back uncounted (RequeueConnectorSync), so a deploy never brings a sync
// nearer its last attempt. A pod killed from outside still counts an attempt
// of the sync it was running, such as when the document worker beside it
// runs out of memory or its node goes: the cap holds a few such kills.
func (t *Task) process(ctx context.Context, sync *model.ConnectorSync) {
	if sync.Origin == model.ConnectorSyncSchedule && sync.Attempts == 1 {
		t.lag.Observe(t.now().Sub(sync.CreatedAt).Seconds())
	}
	var result syncer.Result
	var err error
	if sync.Attempts > t.cfg.MaxAttempts {
		c, getErr := t.queue.GetConnector(ctx, sync.WorkspaceID, sync.ConnectorID)
		if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
			// The lease runs out again, and the next claim tries again.
			log.From(ctx).Warn("load the connector of a stopped sync failed", "sync", sync.ID, "err", getErr)
			return
		}
		if c != nil {
			result.SyncEverySeconds = c.SyncEverySeconds
			result.AccountGeneration = syncer.AccountGeneration(c)
			if c.ChangeCursor == nil && slices.Contains(t.releaseKinds, c.Kind) {
				releaseCtx, cancel := context.WithTimeout(ctx, releaseTimeout)
				if releaseErr := t.releaser.Release(releaseCtx, c); releaseErr != nil {
					log.From(ctx).Warn("release after a stopped sync failed", "connector", c.ID, "err", releaseErr)
				}
				cancel()
			}
		}
		err = &syncer.Failure{Message: fmt.Sprintf("the sync stopped its worker after %d attempts; Helix paused it", sync.Attempts-1),
			Permanent: true}
	} else {
		runCtx, cancel := context.WithTimeout(ctx, t.cfg.Timeout)
		defer cancel()
		stopRenewing := t.renewLease(runCtx, cancel, sync.ID)
		result, err = t.runner.Run(runCtx, sync)
		stopRenewing()
		if err != nil && ctx.Err() != nil {
			// The worker is stopping and cut the attempt short; what it
			// wrote, a later attempt writes again.
			if err := t.persist(ctx, func(ctx context.Context) error {
				return t.queue.RequeueConnectorSync(ctx, sync.ID, t.workerID)
			}); err != nil {
				log.From(ctx).Warn("give back a connector sync failed", "sync", sync.ID, "err", err)
				return
			}
			syncRuns.WithLabelValues("requeued").Inc()
			return
		}
	}

	outcome := t.outcome(ctx, sync, result, err)
	rows := result.Rows
	if err := t.persist(ctx, func(ctx context.Context) error {
		return t.queue.FinishConnectorSync(ctx, sync.ID, t.workerID, outcome)
	}); err != nil {
		log.From(ctx).Warn("finish connector sync failed", "sync", sync.ID, "err", err)
		return
	}
	switch {
	case outcome.Status == model.ConnectorSyncSucceeded:
		syncRuns.WithLabelValues("succeeded").Inc()
		syncRows.Add(float64(rows))
		syncChanges.WithLabelValues(string(result.Mode)).Inc()
	case outcome.RetryAt != nil:
		syncRuns.WithLabelValues("retried").Inc()
	default:
		syncRuns.WithLabelValues("failed").Inc()
	}
}

// persist runs write, which records what became of a claimed sync, past the
// worker's stop and a passing error: up to persistTries times, t.persistWait
// apart, each within persistTimeout. ErrNotFound, a lease another worker
// holds now, is final. A write never made leaves the sync to its lease, and
// the next claim counts an attempt that may have succeeded.
func (t *Task) persist(ctx context.Context, write func(context.Context) error) error {
	ctx = context.WithoutCancel(ctx)
	var err error
	for try := range persistTries {
		if try > 0 {
			time.Sleep(t.persistWait)
		}
		writeCtx, cancel := context.WithTimeout(ctx, persistTimeout)
		err = write(writeCtx)
		cancel()
		if err == nil || errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	return err
}

func (t *Task) renewLease(ctx context.Context, cancel context.CancelFunc, syncID string) (stop func()) {
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
				err := t.queue.RenewConnectorSyncLease(ctx, syncID, t.workerID, t.cfg.Lease)
				if errors.Is(err, store.ErrNotFound) {
					log.From(ctx).Warn("connector sync lease lost", "sync", syncID)
					cancel()
					return
				}
				if err != nil {
					log.From(ctx).Warn("renew connector sync lease failed", "sync", syncID, "err", err)
				}
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// outcome decides what an attempt leaves behind: done, retried after a
// backoff that doubles from a minute, or failed for good, and with a terminal
// one the connector's next scheduled sync.
//
// A pushed connector (an event source, every sync of which is origin
// events, or a Slack connection through Helix's Slack app, whose syncs are
// the scheduler's or a settings change's) has no interval: it syncs when
// its data arrives, an arrival scheduling a sync when none is. After a sync, it syncs again by what waits beyond its cursor:
// with nothing, it keeps what a send scheduled; with less than one read
// takes, it syncs after the spacing; with more, at once. The spacing grows
// with its table's rows, since each merge rewrites the table. Giving up on
// an outage tries again after the spacing too, by the rows its table held
// as the sync began or published since, whichever is more (a table only
// grows), so a merge that keeps failing part way runs no more often than
// one that succeeds.
func (t *Task) outcome(ctx context.Context, sync *model.ConnectorSync, result syncer.Result, err error) model.ConnectorSyncOutcome {
	rows := result.Rows
	if err == nil {
		outcome := model.ConnectorSyncOutcome{Status: model.ConnectorSyncSucceeded, RowsSynced: &rows, Mode: result.Mode}
		switch pushed := result.Pushed; {
		case pushed == nil:
			outcome.Schedule = &model.ConnectorScheduleOutcome{NextSyncAt: t.next(result), Every: result.SyncEverySeconds,
				Generation: result.AccountGeneration}
		case pushed.Backlog == connector.BacklogNone:
			outcome.Schedule = &model.ConnectorScheduleOutcome{Pushed: model.PushedKeep{}}
		case pushed.Backlog == connector.BacklogMore:
			outcome.Schedule = &model.ConnectorScheduleOutcome{Pushed: model.PushedAt{At: t.now()}}
		default: // BacklogSome, and any value it may gain: a sync after the spacing strands no piece
			outcome.Schedule = &model.ConnectorScheduleOutcome{Pushed: model.PushedNotBefore{At: t.now().Add(t.spacing.For(pushed.Rows))}}
		}
		return outcome
	}
	var failure *syncer.Failure
	if !errors.As(err, &failure) {
		failure = &syncer.Failure{Message: "the sync failed; Helix will retry", Err: err}
	}
	log.From(ctx).Warn("connector sync attempt failed", "sync", sync.ID, "connector", sync.ConnectorID,
		"attempt", sync.Attempts, "permanent", failure.Permanent, "err", err)
	if !failure.Permanent && sync.Attempts < t.cfg.MaxAttempts {
		retryAt := t.now().Add(time.Minute << (sync.Attempts - 1))
		return model.ConnectorSyncOutcome{RetryAt: &retryAt, Error: failure.Message}
	}
	message := failure.Message
	var schedule *model.ConnectorScheduleOutcome
	switch {
	case failure.PlanRequired:
		// Buying the plan again is the fix, and nothing tells the schedule; it checks back daily.
		day := t.now().Add(24 * time.Hour)
		schedule = &model.ConnectorScheduleOutcome{NextSyncAt: &day, Paused: message, Every: result.SyncEverySeconds,
			Generation: result.AccountGeneration}
	case failure.Permanent:
		// The same refusal comes back every time until the owner changes something.
		schedule = &model.ConnectorScheduleOutcome{Paused: message, Every: result.SyncEverySeconds, Generation: result.AccountGeneration}
	case sync.Origin == model.ConnectorSyncEvents || result.PushedConnector:
		// Arrivals schedule a sync only when none is due: the plan's
		// interval would hold back what they buffer until then.
		message = fmt.Sprintf("%s (gave up after %d attempts)", failure.Message, sync.Attempts)
		schedule = &model.ConnectorScheduleOutcome{Pushed: model.PushedNotBefore{At: t.now().Add(t.spacing.For(max(result.StoredRows, rows)))}}
	default:
		message = fmt.Sprintf("%s (gave up after %d attempts)", failure.Message, sync.Attempts)
		schedule = &model.ConnectorScheduleOutcome{NextSyncAt: t.next(result), Every: result.SyncEverySeconds,
			Generation: result.AccountGeneration}
	}
	return model.ConnectorSyncOutcome{Status: model.ConnectorSyncFailed, Error: message, RowsSynced: &rows, Mode: result.Mode, Schedule: schedule}
}

// next is when the connector syncs again: the plan's interval or the owner's
// longer one, never below the plan's shortest or the kind's own floor,
// stretched by up to 5% so connectors created together spread out. Nil means
// manual only.
func (t *Task) next(result syncer.Result) *time.Time {
	every := unscheduledRetry
	if result.Plan != nil {
		every = result.Plan.Sync.Every
		if choice := result.SyncEverySeconds; choice != nil && *choice > 0 {
			every = max(time.Duration(*choice)*time.Second, result.Plan.Sync.Shortest)
		}
	}
	every = max(every, result.MinEvery)
	if choice := result.SyncEverySeconds; choice != nil && *choice == 0 {
		return nil
	}
	at := t.now().Add(every + time.Duration(t.jitter()*0.05*float64(every)))
	return &at
}

// schedule queues a sync for every connector whose time has come.
func (t *Task) schedule(ctx context.Context) {
	for ctx.Err() == nil {
		n, err := t.queue.QueueDueConnectorSyncs(ctx, t.cfg.ScheduleBatch, t.spacing.After)
		if err != nil {
			log.From(ctx).Error("queue scheduled connector syncs failed", "err", err)
			return
		}
		syncScheduled.Add(float64(n))
		if n < t.cfg.ScheduleBatch {
			return
		}
	}
}

// refresh queues a refresh load for every ontology whose refresh is due and
// can run now; one with a live job stays owed. A spent month holds none
// back: a refresh with nothing to spend replays its documents at $0.
func (t *Task) refresh(ctx context.Context) {
	// A refresh held back by a live job stays due; paging past it keeps a
	// page of those from starving every one after them.
	var afterDue time.Time
	var afterID string
	for ctx.Err() == nil {
		due, err := t.queue.ListDueFoundryRefreshes(ctx, afterDue, afterID, t.cfg.ScheduleBatch)
		if err != nil {
			log.From(ctx).Error("list due foundry refreshes failed", "err", err)
			return
		}
		for _, o := range due {
			job := &model.FoundryGraphJob{ID: id.New()}
			if t.deps.Cfg.Foundry.Graph.Apply {
				job.Kind = model.FoundryGraphJobApply
			}
			queued, err := t.queue.QueueFoundryRefresh(ctx, o.WorkspaceID, o.ID, job)
			if err != nil {
				log.From(ctx).Warn("queue foundry refresh failed", "ontology", o.ID, "err", err)
				continue
			}
			if queued {
				refreshesQueued.Inc()
			}
		}
		if len(due) < t.cfg.ScheduleBatch {
			return
		}
		last := due[len(due)-1]
		afterDue, afterID = *last.RefreshDueAt, last.ID
	}
}

// reread queues a re-read of each current load's documents the model left
// unread for a reason that passes on its own (ai.RereadFailures: the
// provider's quota or account, or a busy provider) once it is due:
// 15 minutes after the job that left them, then twice as long after each
// re-read that left some still unread, up to 6 hours, for about a week
// (model.FoundryLoadUnread.RereadAt). An ontology with a live job waits for
// its end; a month whose budget for work no one started is spent waits for
// the next. Choosing this over a "read again" action a person takes: the
// failure is the provider's, which no owner fixes on Helix's key, a
// re-read the model refuses again costs nothing, and a person may never
// notice (the loads of 2026-11-14 sat unread for days).
func (t *Task) reread(ctx context.Context) {
	classes := ai.RereadFailures()
	afterID := ""
	for ctx.Err() == nil {
		owed, err := t.queue.ListFoundryRereads(ctx, classes, afterID, t.cfg.ScheduleBatch)
		if err != nil {
			log.From(ctx).Error("list foundry re-reads failed", "err", err)
			return
		}
		for _, o := range owed {
			queued, err := t.queue.QueueFoundryReread(ctx, o.WorkspaceID, o.ID, classes, &model.FoundryGraphJob{ID: id.New()},
				t.deps.Cfg.Foundry.Graph.MonthBudget(t.now()), t.now())
			if err != nil {
				log.From(ctx).Warn("queue foundry re-read failed", "ontology", o.ID, "err", err)
				continue
			}
			if queued {
				rereadsQueued.Inc()
				log.From(ctx).Info("queued a re-read of documents the model left unread", "ontology", o.ID, "load", *o.CurrentLoadID)
			}
		}
		if len(owed) < t.cfg.ScheduleBatch {
			return
		}
		afterID = owed[len(owed)-1].ID
	}
}

// sweep deletes purged documents (purge), then replaced snapshots that are
// past the grace and pinned by no live load job (a deleted connector's
// tables are among them), then change batches past their retention or of a
// deleted connector.
func (t *Task) sweep(ctx context.Context) {
	t.purge(ctx)
	replaced, err := t.queue.ListReplacedSnapshots(ctx, t.now().Add(-t.cfg.ReplacedSnapshotGrace), sweepBatch)
	if err != nil {
		log.From(ctx).Error("list replaced snapshots failed", "err", err)
		return
	}
	if len(replaced) > 0 {
		keys, err := t.queue.ListPinnedSnapshotKeys(ctx)
		if err != nil {
			log.From(ctx).Error("list pinned snapshots failed", "err", err)
			return
		}
		pinned := map[string]bool{}
		for _, k := range keys {
			pinned[k] = true
		}
		for _, r := range replaced {
			if pinned[r.S3Key] {
				continue
			}
			if err := t.snapshots.Delete(ctx, r.S3Key); err != nil {
				log.From(ctx).Warn("delete replaced snapshot failed", "key", r.S3Key, "err", err)
				continue
			}
			if err := t.queue.DeleteReplacedSnapshot(ctx, r.ID); err != nil {
				log.From(ctx).Warn("forget replaced snapshot failed", "id", r.ID, "err", err)
				continue
			}
			snapshotsSwept.Inc()
		}
	}
	expired, err := t.queue.ListExpiredChangeBatches(ctx, t.now().Add(-t.cfg.ChangeRetention), sweepBatch)
	if err != nil {
		log.From(ctx).Error("list expired change batches failed", "err", err)
		return
	}
	for _, b := range expired {
		if b.S3Key != nil {
			if err := t.snapshots.Delete(ctx, *b.S3Key); err != nil {
				log.From(ctx).Warn("delete change batch failed", "key", *b.S3Key, "err", err)
				continue
			}
		}
		if err := t.queue.DeleteConnectorChangeBatch(ctx, b.ID); err != nil {
			log.From(ctx).Warn("forget change batch failed", "id", b.ID, "err", err)
			continue
		}
		batchesSwept.Inc()
	}
}

// purge deletes everything the bucket holds of each document whose purge
// row is past the grace and that no live load job holds
// (ListFoundryDocumentPurges): a sync's reservation no publish claimed, or a
// connector's document replaced or removed at its source. A publish claims
// only reservations younger than half the grace, so none of these is being
// published. A document whose prefix fails to delete keeps its row for the
// next tick.
func (t *Task) purge(ctx context.Context) {
	purges, err := t.queue.ListFoundryDocumentPurges(ctx, t.now().Add(-t.cfg.ReplacedSnapshotGrace), sweepBatch)
	if err != nil {
		log.From(ctx).Error("list purged documents failed", "err", err)
		return
	}
	for _, p := range purges {
		if err := t.documents.DeletePrefix(ctx, document.Prefix(p.WorkspaceID, p.DocumentID)); err != nil {
			log.From(ctx).Warn("delete purged document failed", "document", p.DocumentID, "err", err)
			continue
		}
		if err := t.queue.DeleteFoundryDocumentPurge(ctx, p.DocumentID); err != nil {
			log.From(ctx).Warn("forget purged document failed", "document", p.DocumentID, "err", err)
			continue
		}
		documentsPurged.Inc()
	}
}
