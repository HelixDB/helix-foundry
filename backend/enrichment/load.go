package enrichment

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// JobStore is what a load job's attempt reads and writes of its step. Writes
// are fenced on the job's lease: store.ErrNotFound means the attempt lost
// it.
type JobStore interface {
	StartFoundryLoadEnrichment(ctx context.Context, jobWorker string, job *model.FoundryGraphJob) (*model.FoundryLoadEnrichment, error)
	GetFoundryLoadEnrichment(ctx context.Context, loadID string) (*model.FoundryLoadEnrichment, error)
	GetFoundryEnrichmentConsent(ctx context.Context, workspaceID string) (*model.FoundryEnrichmentConsent, error)
	GetLatestFoundryEnrichmentResults(ctx context.Context, ontologyID string) (*model.FoundryLoadEnrichment, error)
	GetLatestFoundryEnrichmentApplied(ctx context.Context, ontologyID string) (*model.FoundryLoadEnrichment, error)
	GetFoundryEnrichmentLedger(ctx context.Context, workspaceID string, since time.Time) (model.FoundryEnrichmentLedger, error)
	GetFoundryEnrichmentProvider(ctx context.Context, provider string) (*model.FoundryEnrichmentProvider, error)
	QueueFoundryLoadEnrichment(ctx context.Context, loadID, jobWorker string, q model.FoundryEnrichmentQueue) (*model.FoundryLoadEnrichment, error)
	EndPreparedFoundryLoadEnrichment(ctx context.Context, loadID, jobWorker string, p model.FoundryEnrichmentPrepared) error
	ApplyFoundryLoadEnrichment(ctx context.Context, loadID, jobWorker string, applied model.FoundryEnrichmentApplied) (model.FoundryEnrichmentApplied, error)
	EndBusyFoundryLoadEnrichment(ctx context.Context, loadID, jobWorker string) error
}

// ProPlans reads a workspace's Foundry plan (ports.PlanReader): on Pro is
// any plan sold, Pro or above; an error is the billing system failing to say.
type ProPlans interface {
	Status(ctx context.Context, workspaceID string) (foundryplan.Status, error)
}

// ReadPeople reads a load's people with the engine's Clues: it hands add
// every entity of the item's type the load's tables create, in entity
// order, and returns how many lost a clue value another entity holds too
// (Clues' shared). An error add returns ends the read with it.
type ReadPeople func(ctx context.Context, add func(Clue) error) (shared int64, err error)

// LoadFailure is an error of the load's own from reading its people (the
// mappings are invalid, a source is unavailable): the step stays preparing,
// and the attempt ends with Err as its Ingest would have.
type LoadFailure struct{ Err error }

func (f *LoadFailure) Error() string { return f.Err.Error() }
func (f *LoadFailure) Unwrap() error { return f.Err }

// Next is what a load does after its step: apply a file and go on to Ingest,
// or, with Apply nil, park until Until.
type Next struct {
	Apply *model.FoundryEnrichmentApplied
	Until time.Time
}

// busyRecheck is how soon a job parks again when its step's claim_by has
// passed by the attempt's clock but not yet by the database's.
const busyRecheck = 5 * time.Second

// Load is a load job's side of its enrichment step: the graph worker calls
// Step on every attempt of a load whose version has an [enrichments] item,
// before its Ingest.
type Load struct {
	DB    JobStore
	Files *Files
	Cfg   config.EnrichmentConfig
	// Pro is nil where Foundry Pro is not sold: no workspace is on it.
	Pro ProPlans
	Now func() time.Time
}

// Step decides what job's attempt does about its step, as jobWorker, which
// holds the job's lease:
//   - a step that already chose its file applies it again, so every retry of
//     the load ingests the same bytes;
//   - a new step (or one an attempt that died left preparing) is prepared:
//     with consent off it ends off and applies no profiles; otherwise read
//     reads the load's people (the engine's Clues) into a file, at most
//     Cfg.MaxPeople of them, the planner writes the plan and the carried
//     profiles, and the step either ends at once (no one to look up, or
//     lookups not allowed) applying the carried profiles, or is queued for
//     the enrichment worker until claim_by and the job parks;
//   - a step queued or running is waited for (wake): see wake.
//
// A *LoadFailure from read is returned as its Err, the step left
// preparing; store.ErrNotFound means the lease is lost. Any other failure
// to prepare, more than Cfg.MaxPeople people included, ends the step failed,
// applying the ontology's newest applied profiles: enrichment never fails a
// load. Preparing holds PersonBytes of memory per person at most, whatever
// their clues hold.
func (l *Load) Step(ctx context.Context, job *model.FoundryGraphJob, jobWorker string, item *ontologyagent.Enrichment,
	read ReadPeople) (Next, error) {
	row, err := l.DB.StartFoundryLoadEnrichment(ctx, jobWorker, job)
	switch {
	case err != nil:
		return Next{}, err
	case row.Applied != nil:
		return Next{Apply: row.Applied}, nil
	case row.Status == model.FoundryEnrichmentPreparing:
		return l.prepare(ctx, row, jobWorker, item, read)
	}
	return l.wake(ctx, row, jobWorker)
}

// prepare plans a preparing step and ends or queues it.
func (l *Load) prepare(ctx context.Context, row *model.FoundryLoadEnrichment, jobWorker string, item *ontologyagent.Enrichment,
	read ReadPeople) (Next, error) {
	consent, err := l.DB.GetFoundryEnrichmentConsent(ctx, row.WorkspaceID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Next{}, err
	}
	if consent == nil || !consent.Enabled {
		return l.endPrepared(ctx, row, jobWorker, model.FoundryEnrichmentPrepared{
			End: model.EndFoundryEnrichment(model.FoundryEnrichmentOff), Applied: model.FoundryEnrichmentNone,
		})
	}
	found, err := l.Files.People(l.Cfg.MaxPeople)
	if err != nil {
		return l.fail(ctx, row, jobWorker, err)
	}
	defer found.Close()
	found.Shared, err = read(ctx, found.Add)
	var loadFailure *LoadFailure
	if errors.As(err, &loadFailure) {
		return Next{}, loadFailure.Err
	}
	if err != nil {
		return l.fail(ctx, row, jobWorker, fmt.Errorf("read people: %w", err))
	}
	prepared, queue, err := l.plan(ctx, row, item, consent, found)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Next{}, err
	case err != nil:
		return l.fail(ctx, row, jobWorker, err)
	case queue == nil:
		return l.endPrepared(ctx, row, jobWorker, prepared)
	}
	queued, err := l.DB.QueueFoundryLoadEnrichment(ctx, row.LoadID, jobWorker, *queue)
	if err != nil {
		return Next{}, err
	}
	if queued.Status.Ended() { // a Stop or a cancel came while it was prepared: the load never parked
		return l.apply(ctx, queued, jobWorker)
	}
	return Next{Until: *queued.ClaimBy}, nil
}

// plan writes the step's plan and carried profiles, and decides whether it
// ends now (prepared) or is queued for lookups (queue). It ends now with no
// one to look up (done), or when lookups aren't allowed: the flag off
// (partial(flag)), the workspace off Pro (pro), under one lookup's credits
// left once the month's charges are counted (cap), or the provider halted
// (provider_credits). An ending step also writes its results, the carried
// ones, which the next load carries forward.
func (l *Load) plan(ctx context.Context, row *model.FoundryLoadEnrichment, item *ontologyagent.Enrichment, consent *model.FoundryEnrichmentConsent,
	found *People) (model.FoundryEnrichmentPrepared, *model.FoundryEnrichmentQueue, error) {
	layout := LoadLayout(row.WorkspaceID, row.OntologyID, row.LoadID)
	now := l.Now().UTC()
	previous, err := l.previous(ctx, row.OntologyID)
	if err != nil {
		return model.FoundryEnrichmentPrepared{}, nil, err
	}
	w, err := l.Files.Create()
	if err != nil {
		return model.FoundryEnrichmentPrepared{}, nil, err
	}
	planner := Planner{MinScore: l.Cfg.MinScore, NotFoundFor: days(l.Cfg.NotFoundDays), Now: now}
	planned, err := planner.Plan(found, previous, func(e Entry) error { return w.Write(e) })
	if err != nil {
		w.Close()
		return model.FoundryEnrichmentPrepared{}, nil, fmt.Errorf("plan: %w", err)
	}
	planFile, err := w.Upload(ctx, l.Files.Store, layout.Plan())
	if err != nil {
		return model.FoundryEnrichmentPrepared{}, nil, err
	}
	end, err := l.allowed(ctx, row.WorkspaceID, consent, planned.Call, now)
	if err != nil {
		return model.FoundryEnrichmentPrepared{}, nil, err
	}
	resultsKey := ""
	if end != nil {
		resultsKey = layout.Results()
	}
	out, err := l.Files.publish(ctx, item, planFile.Key, func() iter.Seq2[Fresh, error] { return none[Fresh] }, layout.Carried(), resultsKey)
	if err != nil {
		return model.FoundryEnrichmentPrepared{}, nil, err
	}
	counts := found.Counts(planned, out.counts.Profiles)
	if end == nil {
		return model.FoundryEnrichmentPrepared{}, &model.FoundryEnrichmentQueue{
			PlanKey: planFile.Key, CarriedKey: out.profiles.Key, Counts: counts, ClaimBy: now.Add(l.Cfg.ClaimWait),
		}, nil
	}
	return model.FoundryEnrichmentPrepared{
		End: *end, PlanKey: planFile.Key, CarriedKey: out.profiles.Key, ResultsKey: out.results.Key, Counts: counts,
		Applied: out.applied(),
	}, nil, nil
}

// allowed is how a step with calls to make ends at once, nil when it may
// look people up.
func (l *Load) allowed(ctx context.Context, workspaceID string, consent *model.FoundryEnrichmentConsent, calls int64, now time.Time) (*model.FoundryEnrichmentEnd, error) {
	switch {
	case calls == 0:
		return ended(model.FoundryEnrichmentDone), nil
	case !l.Cfg.Enabled:
		return partial(model.FoundryEnrichmentStopFlag), nil
	}
	switch pro, err := onPro(ctx, l.Pro, workspaceID); {
	case err != nil:
		return nil, err
	case !pro:
		return partial(model.FoundryEnrichmentStopPro), nil
	}
	ledger, err := l.DB.GetFoundryEnrichmentLedger(ctx, workspaceID, monthStart(now))
	if err != nil {
		return nil, err
	}
	// Running steps' reservations are not spent: one of this workspace's
	// ends before this step is claimed, and the claim reserves around the
	// others'. A step they hold up queues, and ends busy if it waits too
	// long.
	if ledger.Unspent(caps(l.Cfg, consent, now)) < model.FoundryEnrichmentCredits {
		return partial(model.FoundryEnrichmentStopCap), nil
	}
	halt, err := l.DB.GetFoundryEnrichmentProvider(ctx, string(l.Cfg.Provider))
	if err != nil {
		return nil, err
	}
	if halt.Halted(now) {
		return partial(model.FoundryEnrichmentStopProviderCredits), nil
	}
	return nil, nil
}

// previous reads the ontology's newest results, which the planner carries
// forward; none when no load has any, or its file was swept.
func (l *Load) previous(ctx context.Context, ontologyID string) (iter.Seq2[Result, error], error) {
	latest, err := l.DB.GetLatestFoundryEnrichmentResults(ctx, ontologyID)
	if errors.Is(err, store.ErrNotFound) {
		return none[Result], nil
	}
	if err != nil {
		return nil, err
	}
	results := Read[Result](ctx, l.Files.Store, *latest.ResultsKey)
	return func(yield func(Result, error) bool) {
		for r, err := range results {
			if errors.Is(err, snapshot.ErrMissing) {
				return
			}
			if !yield(r, err) || err != nil {
				return
			}
		}
	}, nil
}

// fail ends a preparing step failed, applying the ontology's newest applied
// profiles (the name guard binds them only to the people they were found
// for), or none.
func (l *Load) fail(ctx context.Context, row *model.FoundryLoadEnrichment, jobWorker string, cause error) (Next, error) {
	log.From(ctx).Warn("foundry enrichment step failed to prepare; the load applies the newest profiles", "load", row.LoadID, "err", cause)
	applied := model.FoundryEnrichmentNone
	switch latest, err := l.DB.GetLatestFoundryEnrichmentApplied(ctx, row.OntologyID); {
	case err == nil:
		applied = *latest.Applied
	case !errors.Is(err, store.ErrNotFound):
		return Next{}, err
	}
	return l.endPrepared(ctx, row, jobWorker, model.FoundryEnrichmentPrepared{
		End: model.EndFoundryEnrichment(model.FoundryEnrichmentFailed), Counts: row.Counts, Applied: applied,
	})
}

// endPrepared ends a preparing step and returns the file it applies, which
// a cancel asked for meanwhile turns to none.
func (l *Load) endPrepared(ctx context.Context, row *model.FoundryLoadEnrichment, jobWorker string, p model.FoundryEnrichmentPrepared) (Next, error) {
	if err := l.DB.EndPreparedFoundryLoadEnrichment(ctx, row.LoadID, jobWorker, p); err != nil {
		return Next{}, err
	}
	ended, err := l.DB.GetFoundryLoadEnrichment(ctx, row.LoadID)
	if err != nil {
		return Next{}, err
	}
	steps.WithLabelValues(string(ended.Status), stopLabel(ended.StoppedBy)).Inc()
	return Next{Apply: ended.Applied}, nil
}

// wake decides what a load does about a step it waits for:
//   - an ended step applies its published profiles; one that published none
//     (it failed, or a stop or the cap came before any) applies the carried
//     ones; a cancelled step applies none;
//   - a step still queued at its claim_by ends partial(busy) (only if still
//     queued: a worker may just have claimed it) and applies the carried;
//   - a step still running past its deadline and the grace applies the
//     carried: the worker's late publish only feeds the next load's
//     carry-forward;
//   - otherwise the job parks until then; the step's end wakes it sooner.
//
// The choice is written once: whatever an attempt chose first applies.
// How long the load waited on its step is observed as it applies.
func (l *Load) wake(ctx context.Context, row *model.FoundryLoadEnrichment, jobWorker string) (Next, error) {
	now := l.Now()
	switch {
	case row.Status == model.FoundryEnrichmentQueued && now.Before(*row.ClaimBy):
		return Next{Until: *row.ClaimBy}, nil
	case row.Status == model.FoundryEnrichmentQueued:
		if err := l.DB.EndBusyFoundryLoadEnrichment(ctx, row.LoadID, jobWorker); err != nil {
			return Next{}, err
		}
		again, err := l.DB.GetFoundryLoadEnrichment(ctx, row.LoadID)
		if err != nil {
			return Next{}, err
		}
		if again.Status == model.FoundryEnrichmentQueued { // claim_by has not passed by the database's clock yet
			return Next{Until: now.Add(busyRecheck)}, nil
		}
		return l.wake(ctx, again, jobWorker)
	case row.Status == model.FoundryEnrichmentRunning && now.Before(row.Deadline.Add(l.Cfg.Grace)):
		return Next{Until: row.Deadline.Add(l.Cfg.Grace)}, nil
	}
	next, err := l.apply(ctx, row, jobWorker)
	if err == nil {
		parked.Observe(l.Now().Sub(row.CreatedAt).Seconds())
	}
	return next, err
}

// apply chooses the file a load applies once its step ended, or the load
// stopped waiting for it (wake): its published profiles; the carried ones
// when it published none; none when it was cancelled or purged. A step that
// ended before any worker claimed it (busy, or a Stop or a cancel while it
// was queued or prepared) has its end counted here, as no worker counted it.
func (l *Load) apply(ctx context.Context, row *model.FoundryLoadEnrichment, jobWorker string) (Next, error) {
	applied := model.FoundryEnrichmentNone
	var key *string
	switch {
	case row.Status == model.FoundryEnrichmentCancelled || row.PurgedAt != nil:
	case row.ProfilesKey != nil:
		key = row.ProfilesKey
	default:
		key = row.CarriedKey
	}
	// Counts.Profiles is the rows of the newest file the step wrote: the
	// carried, until the worker publishes. An empty file applies as none.
	if key != nil && row.Counts.Profiles > 0 {
		digest, err := Digest(ctx, l.Files.Store, *key)
		if err != nil {
			return Next{}, err
		}
		applied = digest
	}
	stored, err := l.DB.ApplyFoundryLoadEnrichment(ctx, row.LoadID, jobWorker, applied)
	if err != nil {
		return Next{}, err
	}
	if row.Status.Ended() && row.ClaimedAt == nil {
		steps.WithLabelValues(string(row.Status), stopLabel(row.StoppedBy)).Inc()
		if row.StoppedBy != nil && *row.StoppedBy == model.FoundryEnrichmentStopBusy && row.EndedAt != nil {
			claimWait.Observe(row.EndedAt.Sub(row.CreatedAt).Seconds())
		}
	}
	return Next{Apply: &stored}, nil
}

// onPro reports a workspace on Foundry Pro; with no plans sold, none is.
func onPro(ctx context.Context, plans ProPlans, workspaceID string) (bool, error) {
	if plans == nil {
		return false, nil
	}
	status, err := plans.Status(ctx, workspaceID)
	if err != nil {
		return false, fmt.Errorf("read foundry plan: %w", err)
	}
	return status.Plan.Sold(), nil
}

// caps are the month's credit caps: the owner's, at most the most an owner
// may set, and Helix's across workspaces.
func caps(cfg config.EnrichmentConfig, consent *model.FoundryEnrichmentConsent, now time.Time) model.FoundryEnrichmentCaps {
	return model.FoundryEnrichmentCaps{
		Workspace: min(consent.MonthlyCreditCap, cfg.MonthlyCreditsMax), Global: cfg.GlobalMonthlyCredits, Since: monthStart(now),
	}
}

// monthStart is the start of now's calendar month, UTC: the charging month.
func monthStart(now time.Time) time.Time {
	now = now.UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// none is an empty file's lines.
func none[T any](func(T, error) bool) {}
