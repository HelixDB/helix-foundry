package enrichment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/snapshot"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// BatchSize is how many of the plan's calls one batch makes: batch b is
	// the calls from b*BatchSize, in the plan's order.
	BatchSize = 50
	// recheckEvery is how many batches run between a step's re-checks of the
	// flag, consent, Pro and the ontology.
	recheckEvery = 10
	// failStreak is how many failed calls in a row stop a step
	// partial(provider).
	failStreak = 10
	// haltFor is how long a refused key or credits halt the provider.
	haltFor = 15 * time.Minute
	// maxLeaseOwner is the longest lease owner a step's row keeps
	// (foundry_load_enrichments.lease_owner).
	maxLeaseOwner = 64
	// lateFor is how long past its deadline and the grace a step that keeps
	// failing is tried again, a lease apart, before it ends failed. Its load
	// no longer waits for it, and it holds no running slot, but its late
	// publish keeps the profiles it paid for in the next load's
	// carry-forward: an hour outlasts an outage of S3 or MySQL.
	lateFor = time.Hour
)

// errUnfenced is a step whose lease no renewal has confirmed for long
// enough that a call could outlive it: the step is handed on.
var errUnfenced = errors.New("the step's lease is not confirmed past a call's timeout")

// errHandOn is a step handed on with its batch in flight, lookups the
// worker stopped before sending left for the next claim to send.
var errHandOn = errors.New("the step is handed on with lookups of its batch not sent")

// Why a lookup was not sent, as its call's Error says when it ended
// unanswered (Unreached): the step's reasons say how its batch ends the
// step (callsEnd); the worker stopping (SIGTERM, or its lease lost) and the
// lease's fence hand the batch on, for the next claim to send the rest.
const (
	unsentCancel   = "not sent: the step was cancelled"
	unsentHalt     = "not sent: the provider is halted"
	unsentFailing  = "not sent: the provider kept failing"
	unsentStop     = "not sent: the lookups were stopped"
	unsentDeadline = "not sent: the step's deadline passed"
	unsentStopping = "not sent: the worker stopped"
	unsentLease    = "not sent: the step's lease is not confirmed"
	// resumed marks, in calls/<b>, a lookup a claim sends that a claim
	// before it left unsent, until its answer is written: one never written
	// is booked uncertain, as a call that may have been charged.
	resumed = "resumed: no answer written"
)

// stepReasons are the reasons not to send that end the step.
var stepReasons = map[string]bool{unsentCancel: true, unsentHalt: true, unsentFailing: true, unsentStop: true, unsentDeadline: true}

// RunStore is what the enrichment worker reads and writes of the steps it
// runs. Writes are fenced on the step's lease: store.ErrNotFound means
// the worker lost it.
type RunStore interface {
	ClaimFoundryLoadEnrichment(ctx context.Context, workerID, provider string, lease, loadWait, grace time.Duration, maxRunning int) (step *model.FoundryLoadEnrichment, first bool, err error)
	RenewFoundryLoadEnrichmentLease(ctx context.Context, loadID, workerID, provider string, lease time.Duration) (model.FoundryEnrichmentSignals, error)
	ReleaseFoundryLoadEnrichment(ctx context.Context, loadID, workerID string) error
	CheckFoundryLoadEnrichment(ctx context.Context, loadID, provider string) (model.FoundryEnrichmentChecks, error)
	GetFoundryLoad(ctx context.Context, ontologyID, id string) (*model.FoundryLoad, error)
	GetOntologyVersion(ctx context.Context, ontologyID, id string) (*model.OntologyVersion, error)
	HaltFoundryEnrichmentProvider(ctx context.Context, provider string, until time.Time, code string) error
	ReserveFoundryEnrichmentCredits(ctx context.Context, loadID, workerID, provider string, want int64, caps model.FoundryEnrichmentCaps) (int64, error)
	BeginFoundryEnrichmentBatch(ctx context.Context, loadID, workerID, provider string, batch, want int,
		caps model.FoundryEnrichmentCaps) (model.FoundryEnrichmentBegun, error)
	ChargeFoundryEnrichmentBatch(ctx context.Context, loadID, workerID string, charge model.FoundryEnrichmentCharge, counts model.FoundryEnrichmentCounts) error
	EndFoundryLoadEnrichment(ctx context.Context, loadID, workerID string, end model.FoundryEnrichmentEnd, published *model.FoundryEnrichmentPublished,
		counts model.FoundryEnrichmentCounts) error
}

// Worker runs queued steps. The foundry-enrichment-worker is the only
// holder of the provider's key.
type Worker struct {
	DB     RunStore
	Files  *Files
	Client *Client
	// Pro is nil where Foundry Pro is not sold: no workspace is on it.
	Pro ProPlans
	Cfg config.EnrichmentConfig
	// WorkerID names the process (host:pid) in each claim's lease owner.
	WorkerID string
	Now      func() time.Time
}

// Claim leases the next step due: a running one whose worker died, or a
// queued one before its claim_by, within the running limits (at most
// Cfg.MaxRunning, one per workspace; a step past its deadline and the grace
// counts against neither). Nil means none is due.
//
// Each claim leases the step under an owner of its own, a fresh ULID before
// WorkerID (cut to what the row keeps), which every write of the claim is
// fenced on. A step this process claims again (its lease lapsed while a
// claim of it was still stuck, say on a dead connection) is then fenced
// against that claim too, as against another process: the stuck claim's
// writes find the lease gone and it stops, rather than writing, or
// releasing, over the claim that took the step.
//
// A step's first claim records how long it waited for one (claim_wait):
// once per step, as a reclaim keeps claimed_at.
func (w *Worker) Claim(ctx context.Context) (*model.FoundryLoadEnrichment, error) {
	owner := id.New() + "@" + w.WorkerID
	if len(owner) > maxLeaseOwner { // keep the ULID, and the end of host:pid that tells pods apart
		owner = owner[:27] + owner[len(owner)-(maxLeaseOwner-27):]
	}
	step, first, err := w.DB.ClaimFoundryLoadEnrichment(ctx, owner, string(w.Cfg.Provider), w.Cfg.Lease, w.Cfg.LoadWait, w.Cfg.Grace, w.Cfg.MaxRunning)
	if err != nil {
		return nil, err
	}
	if first {
		claimWait.Observe(step.ClaimedAt.Sub(step.CreatedAt).Seconds())
	}
	return step, nil
}

// Run runs a step Claim returned, whose lease began no later than leased,
// until it ends:
//  1. read the version's item and the plan's calls; take over a batch a
//     claim before this one left in flight (reclaim);
//  2. check the flag, consent, the ontology, the provider's halt and Pro;
//     send what a batch handed on left unsent; and reserve the credits the
//     remaining calls may cost;
//  3. run the batches from the cursor. Each is begun (shrunk to what the
//     reservation and the month's headroom leave, and persisted in flight),
//     called, written to calls/<b> and, judged, to results/<b>, then charged
//     in one transaction fenced on the lease, which moves the cursor on.
//     Step 2's checks run again every recheckEvery batches; a Stop, a
//     cancel, the provider's halt (whoever recorded it), the deadline and the
//     lease's fence are seen before every batch, and before every call: the
//     lease's renewal reads the Stop, the cancel and the halt. A batch's
//     calls say how it ends the step (callsEnd);
//  4. publish the profiles and the results, and end the step, which wakes
//     its load. A cancelled step settles its batch and publishes nothing.
//
// When stopping ends (SIGTERM) no call starts; the calls already sent run
// to their answers, calls/<b> records them and the lookups not sent, and the
// lease is handed on with the batch in flight: the next claim sends the rest
// of it. So does a lease no renewal confirmed. A failure (Autumn, S3 or
// MySQL failing) leaves the lease to lapse: the step is tried again a lease
// later, not every poll, as each try reads the version and the plan again.
// Past the deadline and the grace the load has applied the carried
// profiles, and the step's late publish only feeds the next load's
// carry-forward; once lateFor has passed too, a failure ends it failed. A
// step past its deadline makes no call, so it neither asks Autumn about Pro
// nor keeps a reservation.
func (w *Worker) Run(stopping context.Context, step *model.FoundryLoadEnrichment, leased time.Time) error {
	switch {
	case step.LeaseOwner == nil:
		return fmt.Errorf("foundry enrichment step %s was not claimed", step.LoadID)
	case w.Client.HTTP.Timeout <= 0: // the lease's fence counts on it
		return errors.New("the foundry enrichment client has no timeout")
	}
	owner := *step.LeaseOwner
	runCtx, cancel := context.WithCancel(context.WithoutCancel(stopping))
	defer cancel()
	// No call starts once the worker stops or loses the lease. A request
	// already sent runs to its answer or its timeout (Client.Do), which comes
	// before the lease could lapse.
	startCtx, stopStarts := context.WithCancel(runCtx)
	defer stopStarts()
	defer context.AfterFunc(stopping, stopStarts)()
	held := w.hold(runCtx, cancel, step.LoadID, owner, leased)
	defer held.release()
	r := &run{
		w: w, step: step, owner: owner, layout: LoadLayout(step.WorkspaceID, step.OntologyID, step.LoadID), ctx: runCtx, startCtx: startCtx,
		stopping: stopping, held: held, cursor: step.BatchCursor, inflight: step.InflightCredits, counts: step.Counts,
	}
	defer func() { r.plan.close() }() // read once the claim starts
	end, err := r.steps()
	if err == nil && end != nil {
		if err = r.end(*end); err == nil {
			return nil
		}
	}
	switch {
	case errors.Is(err, store.ErrNotFound) || runCtx.Err() != nil: // a renewal found the lease gone, which ends runCtx
		log.From(stopping).Warn("foundry enrichment step lease lost", "load", step.LoadID)
		return nil
	case err == nil, errors.Is(err, errHandOn):
		return held.handOn(runCtx, w.DB, step.LoadID, owner)
	case errors.Is(err, errUnfenced):
		return errors.Join(err, held.handOn(runCtx, w.DB, step.LoadID, owner))
	case w.Now().Before(step.Deadline.Add(w.Cfg.Grace + lateFor)):
		return err // tried again once the lease lapses
	}
	log.From(stopping).Error("foundry enrichment step failed past its deadline and the late window", "load", step.LoadID, "err", err)
	return errors.Join(err, r.abandon())
}

// run is one claim of a step.
type run struct {
	w    *Worker
	step *model.FoundryLoadEnrichment
	// owner is the claim's lease owner, which its writes are fenced on.
	owner  string
	layout Layout
	// ctx bounds the claim's reads and writes; startCtx the starting of its
	// calls, which ends when the worker stops or loses the lease.
	ctx      context.Context
	startCtx context.Context
	stopping context.Context
	held     *lease
	// item is the version's [enrichments] item; plan the plan's call
	// entries, in its order, read a batch at a time.
	item *ontologyagent.Enrichment
	plan *planCalls
	// cursor, inflight and counts are the row's, as this claim moves them.
	cursor   int
	inflight int64
	counts   model.FoundryEnrichmentCounts
	// pending is calls/<b> of the batch in flight a claim before this one
	// left, which reclaim read: this claim sends what it left unsent, or
	// settles it as it stands. Nil once settled, or with none.
	pending []Call
	// consent is the workspace's setting as last checked.
	consent *model.FoundryEnrichmentConsent
	// halt is the status of a refused key or credits seen; streak the
	// failed calls in a row.
	halt   atomic.Pointer[string]
	streak atomic.Int32
}

// steps runs the step until it ends, returning how; nil with no error, or
// errHandOn, hands it on.
func (r *run) steps() (*model.FoundryEnrichmentEnd, error) {
	if err := r.read(); err != nil {
		return nil, err
	}
	if r.inflight > 0 {
		if err := r.reclaim(); err != nil {
			return nil, err
		}
	}
	if end, err := r.check(); end != nil || err != nil {
		return end, err
	}
	start := r.cursor
	if calls := r.pending; calls != nil {
		r.pending = nil // settled or handed on by call; a failure leaves it to a reclaim
		if end, err := r.call(r.cursor, calls); end != nil || err != nil {
			return end, err
		}
	} else if r.cursor > 0 {
		// A claim before this one may have ended the step with the batch it
		// settled last, and lost that end with its publish, or died first.
		if end, err := r.settled(r.cursor - 1); end != nil || err != nil {
			return end, err
		}
	}
	remaining := int64(0) // past the deadline no call starts: what the step held is freed
	if r.w.Now().Before(*r.step.Deadline) {
		remaining = int64(max(r.plan.total-r.cursor*BatchSize, 0))
	}
	if _, err := r.w.DB.ReserveFoundryEnrichmentCredits(r.ctx, r.step.LoadID, r.owner, string(r.w.Cfg.Provider),
		remaining*model.FoundryEnrichmentCredits, caps(r.w.Cfg, r.consent, r.w.Now())); err != nil {
		return nil, err
	}
	for r.cursor*BatchSize < r.plan.total {
		if r.stopping.Err() != nil {
			return nil, nil
		}
		if r.cursor > start && (r.cursor-start)%recheckEvery == 0 {
			if end, err := r.check(); end != nil || err != nil {
				return end, err
			}
		}
		signals := r.held.signals()
		switch {
		case signals.Cancel:
			return ended(model.FoundryEnrichmentCancelled), nil
		case signals.Stop:
			return partial(model.FoundryEnrichmentStopStopped), nil
		case signals.Halted:
			return partial(model.FoundryEnrichmentStopProviderCredits), nil
		case !r.w.Now().Before(*r.step.Deadline):
			return partial(model.FoundryEnrichmentStopTime), nil
		case !r.w.Now().Before(r.held.fence()):
			return nil, errUnfenced
		}
		batch, err := r.plan.batch(r.cursor)
		if err != nil {
			return nil, err
		}
		begun, err := r.w.DB.BeginFoundryEnrichmentBatch(r.ctx, r.step.LoadID, r.owner, string(r.w.Cfg.Provider), r.cursor, len(batch),
			caps(r.w.Cfg, r.consent, r.w.Now()))
		switch {
		case err != nil:
			return nil, err
		case begun.Cancel:
			return ended(model.FoundryEnrichmentCancelled), nil
		case begun.Stop:
			return partial(model.FoundryEnrichmentStopStopped), nil
		case begun.Halted:
			return partial(model.FoundryEnrichmentStopProviderCredits), nil
		case begun.N == 0:
			return partial(model.FoundryEnrichmentStopCap), nil
		}
		r.inflight = int64(begun.N) * model.FoundryEnrichmentCredits
		// A streak that ended the batch before on its last call holds every
		// lookup of this one back, which its calls then say.
		if end, err := r.call(r.cursor, make([]Call, begun.N)); end != nil || err != nil {
			return end, err
		}
	}
	return ended(model.FoundryEnrichmentDone), nil
}

// call sends the lookups of batch b, the batch in flight, that calls (one
// per lookup in flight, in the plan's order) leaves unsent, at once within
// the replica's limiter, unless the batch's calls already end the step;
// writes calls/<b>; and settles the batch. Lookups the worker stopped
// before sending (SIGTERM, or its lease lost) or the lease's fence held
// back leave the batch in flight, handed on (errHandOn, errUnfenced): the
// next claim sends them. A batch a claim before this one left first has
// those lookups marked resumed in calls/<b>, so one whose answer this claim
// then fails to write is booked uncertain, never sent a third time. It
// returns how the batch ends the step (callsEnd), or partial(cap) when the
// cap shrank it; nil goes on.
func (r *run) call(b int, calls []Call) (*model.FoundryEnrichmentEnd, error) {
	batch, err := r.plan.batch(b)
	switch {
	case err != nil:
		return nil, err
	case len(calls) > len(batch):
		return nil, fmt.Errorf("batch %d has %d calls for %d lookups", b, len(calls), len(batch))
	}
	lookups := batch[:len(calls)]
	end, left := callsEnd(calls)
	if end == nil && left != "" {
		var send []int
		for i, c := range calls {
			if answer, _, _, _ := Judge(c); answer == Unreached {
				send = append(send, i)
			}
		}
		if slices.ContainsFunc(calls, func(c Call) bool { return c.CallID != "" }) { // written by a claim before this one
			marked := slices.Clone(calls)
			for _, i := range send {
				marked[i].Reached, marked[i].Status, marked[i].Error, marked[i].At = true, 0, resumed, r.w.Now().UTC()
			}
			if _, err := Put(r.ctx, r.w.Files, r.layout.Calls(b), marked); err != nil {
				return nil, err
			}
		}
		var wg sync.WaitGroup
		for _, i := range send {
			wg.Go(func() {
				calls[i] = r.w.Client.Do(r.startCtx, r.unsent, Lookup{
					Entity: lookups[i].Entity, Sent: lookups[i].Sent, LoadID: r.step.LoadID, CallID: fmt.Sprintf("%s-%06d-%02d", r.step.LoadID, b, i),
				})
				r.observe(calls[i])
			})
		}
		wg.Wait()
		if _, err := Put(r.ctx, r.w.Files, r.layout.Calls(b), calls); err != nil {
			return nil, err
		}
		end, left = callsEnd(calls)
	}
	switch {
	case end == nil && left == unsentLease:
		return nil, errUnfenced
	case end == nil && left != "":
		return nil, errHandOn
	}
	if err := r.settle(b, lookups, calls); err != nil {
		return nil, err
	}
	if end == nil && len(calls) < r.plan.batchLen(b) { // the rest of the batch waits for next month's credits
		end = partial(model.FoundryEnrichmentStopCap)
	}
	return end, nil
}

// callsEnd is how a batch's calls end the step: by why lookups, or the
// resend of a 408 or 5xx, were not sent (their calls' Error), a call's halt
// (a 402, 401 or 403), or failStreak failed calls in a row in the batch's
// order, in this order: a cancel (cancelled), a halt
// (partial(provider_credits)), the provider failing (partial(provider)), a
// Stop (partial(stopped)), the deadline (partial(time)). The calls say it,
// so a claim that takes a batch over, or finishes a step whose publish
// failed, ends it as the claim that ran the batch did. Lookups left unsent
// for no reason of the step's (the worker stopped, the lease's fence, or
// not sent yet) are left, to send: unsentLease when the fence held one
// back.
func callsEnd(calls []Call) (end *model.FoundryEnrichmentEnd, left string) {
	why := map[string]bool{}
	failed := 0
	for _, c := range calls {
		answer, _, _, _ := Judge(c)
		switch {
		case c.Error == resumed: // its answer was never written: it says nothing of the provider
		case answer == Failed || answer == NoAnswer:
			if failed++; failed >= failStreak {
				why[unsentFailing] = true
			}
		case answer == Matched || answer == NoMatch || answer == Refused:
			failed = 0
		}
		switch {
		case answer == Halt:
			why[unsentHalt] = true
		case answer == Unreached || stepReasons[c.Error]:
			why[c.Error] = true
		}
	}
	switch {
	case why[unsentCancel]:
		return ended(model.FoundryEnrichmentCancelled), ""
	case why[unsentHalt]:
		return partial(model.FoundryEnrichmentStopProviderCredits), ""
	case why[unsentFailing]:
		return partial(model.FoundryEnrichmentStopProvider), ""
	case why[unsentStop]:
		return partial(model.FoundryEnrichmentStopStopped), ""
	case why[unsentDeadline]:
		return partial(model.FoundryEnrichmentStopTime), ""
	case why[unsentLease]:
		return nil, unsentLease
	case len(why) > 0:
		return nil, unsentStopping
	}
	return nil, ""
}

// settled is how batch b, which a claim before this one settled, ended the
// step: as that claim chose then, from b's calls/<b> (callsEnd), which hold
// every lookup b began, sent or not, so a batch the cap shrank is known
// too. Nil when it didn't, or calls/<b> is gone (a reclaim booked b
// uncertain).
func (r *run) settled(b int) (*model.FoundryEnrichmentEnd, error) {
	calls, err := ReadAll[Call](r.ctx, r.w.Files.Store, r.layout.Calls(b))
	switch {
	case errors.Is(err, snapshot.ErrMissing):
		return nil, nil
	case err != nil:
		return nil, err
	}
	if end, _ := callsEnd(calls); end != nil {
		return end, nil
	}
	if len(calls) < r.plan.batchLen(b) {
		return partial(model.FoundryEnrichmentStopCap), nil
	}
	return nil, nil
}

// read reads the version's item and the plan's calls. Loads and versions
// are never deleted, a deleted ontology's included (check sees it gone and
// cancels the step): one missing fails the step, and is not taken for the
// lease lost.
func (r *run) read() error {
	load, err := r.w.DB.GetFoundryLoad(r.ctx, r.step.OntologyID, r.step.LoadID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("foundry enrichment step %s has no load", r.step.LoadID)
	case err != nil:
		return err
	}
	version, err := r.w.DB.GetOntologyVersion(r.ctx, r.step.OntologyID, load.VersionID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("foundry enrichment step %s has no version %s", r.step.LoadID, load.VersionID)
	case err != nil:
		return err
	}
	var def ontologyagent.Definition
	if err := json.Unmarshal(version.Definition, &def); err != nil {
		return fmt.Errorf("read version %s: %w", version.ID, err)
	}
	if r.item, err = ontologyagent.ParseEnrichments(def.MappingsTOML); err != nil {
		return err
	}
	if r.item == nil || r.step.PlanKey == nil {
		return fmt.Errorf("foundry enrichment step %s has no item or plan", r.step.LoadID)
	}
	r.plan, err = r.w.Files.planCalls(r.ctx, *r.step.PlanKey)
	return err
}

// check re-checks what may end a step: a cancel, consent off or the
// ontology or workspace being deleted (cancelled), the flag (partial(flag)),
// a Stop (partial(stopped)), the provider's halt (partial(provider_credits))
// and Pro (partial(pro)), which is not asked past the deadline: no call
// starts then, and Autumn failing must not hold a late publish up.
func (r *run) check() (*model.FoundryEnrichmentEnd, error) {
	c, err := r.w.DB.CheckFoundryLoadEnrichment(r.ctx, r.step.LoadID, string(r.w.Cfg.Provider))
	if err != nil {
		return nil, err
	}
	switch {
	case c.Cancel || c.Gone || c.Consent == nil || !c.Consent.Enabled:
		return ended(model.FoundryEnrichmentCancelled), nil
	case !r.w.Cfg.Enabled:
		return partial(model.FoundryEnrichmentStopFlag), nil
	case c.Stop:
		return partial(model.FoundryEnrichmentStopStopped), nil
	case c.Halted:
		return partial(model.FoundryEnrichmentStopProviderCredits), nil
	}
	r.consent = c.Consent
	if !r.w.Now().Before(*r.step.Deadline) {
		return nil, nil
	}
	switch pro, err := onPro(r.ctx, r.w.Pro, r.step.WorkspaceID); {
	case err != nil:
		return nil, err
	case !pro:
		return partial(model.FoundryEnrichmentStopPro), nil
	}
	return nil, nil
}

// unsent is why a lookup may not be sent now, or sent again; "" when it
// may: a cancel, a halt (a call's, or the provider's as the lease's renewal
// last read it), a failure streak, a Stop, the deadline, the worker
// stopping or losing the lease, or the lease's fence (a call's timeout
// before the lease could lapse). A batch held up by 429s stops resending
// within a renewal of a halt an operator or another step recorded.
func (r *run) unsent() string {
	now, signals := r.w.Now(), r.held.signals()
	switch {
	case signals.Cancel:
		return unsentCancel
	case r.halt.Load() != nil || signals.Halted:
		return unsentHalt
	case r.streak.Load() >= failStreak:
		return unsentFailing
	case signals.Stop:
		return unsentStop
	case !now.Before(*r.step.Deadline):
		return unsentDeadline
	case r.startCtx.Err() != nil:
		return unsentStopping
	case !now.Before(r.held.fence()):
		return unsentLease
	}
	return ""
}

// observe notes what a call came to as it comes: a halt and a failure
// streak stop the batch's lookups not yet sent.
func (r *run) observe(c Call) {
	calls.WithLabelValues(r.note(c).String()).Inc()
}

// note records a halt or the failure streak a call adds to, and returns
// what it came to.
func (r *run) note(c Call) Answer {
	answer, _, _, _ := Judge(c)
	switch answer {
	case Halt:
		code := strconv.Itoa(c.Status)
		r.halt.CompareAndSwap(nil, &code)
	case Failed, NoAnswer:
		r.streak.Add(1)
	case Matched, NoMatch, Refused:
		r.streak.Store(0)
	}
	return answer
}

// recordHalt halts the provider for haltFor once a call saw the key or the
// credits refused. The halt is the provider's, in MySQL: every step and
// replica stops calling before its next batch, and a restart keeps it.
func (r *run) recordHalt() error {
	code := r.halt.Load()
	if code == nil {
		return nil
	}
	return r.w.DB.HaltFoundryEnrichmentProvider(r.ctx, string(r.w.Cfg.Provider), r.w.Now().Add(haltFor), *code)
}

// judge turns a batch's calls into its results: a match goes through the
// gate, with its profile as the catalog keeps it; a lookup not sent, or
// sent only into a halt, has none and is looked up next load. calls[i] is
// lookups[i]'s.
func (r *run) judge(lookups []Entry, calls []Call) ([]Fresh, error) {
	if len(calls) != len(lookups) {
		return nil, fmt.Errorf("batch %d has %d calls for %d lookups", r.cursor, len(calls), len(lookups))
	}
	var fresh []Fresh
	for i, c := range calls {
		e := lookups[i]
		if c.Entity != e.Entity {
			// Named by position: errors are logged, and an entity's key may be an email.
			return nil, fmt.Errorf("batch %d call %d is not for its lookup's entity", r.cursor, i)
		}
		answer, match, partial, err := Judge(c)
		if err != nil {
			log.From(r.ctx).Warn("foundry enrichment answer unreadable", "load", r.step.LoadID, "err", err)
		}
		res := Result{Sent: e.Sent, CheckedAt: c.At, Partial: partial}
		switch answer {
		case Unreached:
			continue
		case Halt:
			code := strconv.Itoa(c.Status)
			r.halt.CompareAndSwap(nil, &code)
			continue
		case Matched:
			row := match.Person.Profile(match.Score, e, c, c.At)
			row.LoadID = r.step.LoadID
			res.Outcome, res.Reason = Gate(row, match.Score, r.w.Cfg.MinScore, e.Name, e.Sent)
			res.Score, res.ProfileID, res.Row = &match.Score, row.ProfileID, row
		case NoMatch:
			res.Outcome = NotFound
		case Refused:
			res.Outcome, res.Reason = Skipped, ReasonRefused
		case Failed:
			res.Outcome, res.Reason = Skipped, ReasonProviderError
		case NoAnswer:
			res.Outcome = Uncertain
		}
		fresh = append(fresh, Fresh{Entity: e.Entity, Result: res})
	}
	return fresh, nil
}

// charge settles batch b with its results in one transaction fenced on the
// lease: 20 credits for each match returned, accepted or held, and 20,
// booked as uncertain, for each call that got no answer. The step keeps
// reserved at most what the calls after b may cost.
func (r *run) charge(b int, fresh []Fresh) error {
	charge := model.FoundryEnrichmentCharge{Batch: b, Remaining: int64(max(r.plan.total-(b+1)*BatchSize, 0)) * model.FoundryEnrichmentCredits}
	counts := r.counts
	for _, f := range fresh {
		counts.LookedUp++
		switch f.Outcome {
		case Accepted:
			counts.Found++
			charge.Credits += model.FoundryEnrichmentCredits
		case Held:
			counts.Held++
			charge.Credits += model.FoundryEnrichmentCredits
		case NotFound:
			counts.NotFound++
		case Uncertain:
			counts.Uncertain++
			charge.UncertainCredits += model.FoundryEnrichmentCredits
		}
	}
	counts.Credits += charge.Credits + charge.UncertainCredits
	if err := r.w.DB.ChargeFoundryEnrichmentBatch(r.ctx, r.step.LoadID, r.owner, charge, counts); err != nil {
		return err
	}
	r.counts, r.cursor, r.inflight = counts, b+1, 0
	credits.WithLabelValues("charged").Add(float64(charge.Credits))
	credits.WithLabelValues("uncertain").Add(float64(charge.UncertainCredits))
	return nil
}

// settle judges batch b's calls into results/<b>, charges the batch, which
// moves the cursor on, and records a halt a call saw. calls[i] is
// lookups[i]'s.
func (r *run) settle(b int, lookups []Entry, calls []Call) error {
	fresh, err := r.judge(lookups, calls)
	if err != nil {
		return err
	}
	if _, err := Put(r.ctx, r.w.Files, r.layout.BatchResults(b), fresh); err != nil {
		return err
	}
	if err := r.charge(b, fresh); err != nil {
		return err
	}
	return r.recordHalt()
}

// settlePending settles the batch in flight a claim before this one left as
// it stands, its lookups left unsent looked up next load: the step ends
// before this claim sent them.
func (r *run) settlePending() error {
	calls := r.pending
	batch, err := r.plan.batch(r.cursor)
	switch {
	case err != nil:
		return err
	case len(calls) > len(batch):
		return fmt.Errorf("batch %d has %d calls for %d lookups", r.cursor, len(calls), len(batch))
	}
	if err := r.settle(r.cursor, batch[:len(calls)], calls); err != nil {
		return err
	}
	r.pending = nil
	return nil
}

// reclaim takes over the batch in flight a claim before this one left,
// without a call: it is settled from its results/<b>; failing that, its
// calls/<b> are pending, for this claim to send what they left unsent, or
// to settle as they stand; failing that, every call it may have made is
// booked uncertain.
func (r *run) reclaim() error {
	b := r.cursor
	lookups, err := r.plan.batch(b)
	if err != nil {
		return err
	}
	lookups = lookups[:min(int(r.inflight/model.FoundryEnrichmentCredits), len(lookups))]
	fresh, err := ReadAll[Fresh](r.ctx, r.w.Files.Store, r.layout.BatchResults(b))
	switch {
	case err == nil:
		return r.charge(b, fresh)
	case !errors.Is(err, snapshot.ErrMissing):
		return err
	}
	calls, err := ReadAll[Call](r.ctx, r.w.Files.Store, r.layout.Calls(b))
	switch {
	case err == nil:
		r.pending = calls
		return nil
	case !errors.Is(err, snapshot.ErrMissing):
		return err
	}
	fresh = nil
	for _, e := range lookups {
		fresh = append(fresh, Fresh{Entity: e.Entity, Result: Result{Sent: e.Sent, Outcome: Uncertain, CheckedAt: r.w.Now().UTC()}})
	}
	if _, err := Put(r.ctx, r.w.Files, r.layout.BatchResults(b), fresh); err != nil {
		return err
	}
	return r.charge(b, fresh)
}

// end publishes, unless the step is cancelled or failed, and ends the step,
// waking its load. Found, Held, NotFound and Uncertain count the step's own
// results as published, a profile on two people held. A batch a claim
// before this one left in flight, which this claim has not sent on, is
// settled first.
func (r *run) end(end model.FoundryEnrichmentEnd) error {
	if r.pending != nil {
		if err := r.settlePending(); err != nil {
			return err
		}
	}
	var published *model.FoundryEnrichmentPublished
	counts := r.counts
	if status := end.Status(); status != model.FoundryEnrichmentCancelled && status != model.FoundryEnrichmentFailed {
		out, err := r.w.Files.publish(r.ctx, r.item, *r.step.PlanKey, r.fresh, r.layout.Profiles(), r.layout.Results())
		if err != nil {
			return err
		}
		published = &model.FoundryEnrichmentPublished{ProfilesKey: out.profiles.Key, ResultsKey: out.results.Key}
		counts.Found, counts.Held = out.counts.Fresh[Accepted], out.counts.Fresh[Held]
		counts.NotFound, counts.Uncertain = out.counts.Fresh[NotFound], out.counts.Fresh[Uncertain]
		counts.Profiles = out.counts.Profiles
	}
	if err := r.w.DB.EndFoundryLoadEnrichment(r.ctx, r.step.LoadID, r.owner, end, published, counts); err != nil {
		return err
	}
	steps.WithLabelValues(string(end.Status()), stopLabel(end.StoppedBy())).Inc()
	return nil
}

// abandon ends a step that can't go on failed, without publishing. A
// batch in flight is settled first, as a reclaim would, as it stands, or,
// when even that fails, booked as uncertain; what a publish that failed
// wrote is deleted, as no row will name it for the sweep.
func (r *run) abandon() error {
	for _, key := range []string{r.layout.Results(), r.layout.Profiles()} {
		if err := r.w.Files.Store.Delete(r.ctx, key); err != nil && !errors.Is(err, snapshot.ErrMissing) {
			return err
		}
	}
	if r.inflight > 0 && r.pending == nil {
		if err := r.reclaim(); err != nil {
			log.From(r.ctx).Warn("foundry enrichment step books its batch in flight as uncertain", "load", r.step.LoadID, "err", err)
		}
	}
	if r.inflight > 0 && r.pending != nil {
		if err := r.settlePending(); err != nil {
			log.From(r.ctx).Warn("foundry enrichment step books its batch in flight as uncertain", "load", r.step.LoadID, "err", err)
		}
	}
	if r.inflight > 0 {
		counts := r.counts
		counts.Credits += r.inflight
		if err := r.w.DB.ChargeFoundryEnrichmentBatch(r.ctx, r.step.LoadID, r.owner,
			model.FoundryEnrichmentCharge{Batch: r.cursor, UncertainCredits: r.inflight}, counts); err != nil {
			return err
		}
		r.counts, r.cursor, r.inflight = counts, r.cursor+1, 0
	}
	if err := r.w.DB.EndFoundryLoadEnrichment(r.ctx, r.step.LoadID, r.owner, model.EndFoundryEnrichment(model.FoundryEnrichmentFailed), nil,
		r.counts); err != nil {
		return err
	}
	steps.WithLabelValues(string(model.FoundryEnrichmentFailed), "").Inc()
	return nil
}

// fresh is the step's results, batch by batch: the plan's order of calls.
func (r *run) fresh() iter.Seq2[Fresh, error] {
	return func(yield func(Fresh, error) bool) {
		for b := range r.cursor {
			for f, err := range Read[Fresh](r.ctx, r.w.Files.Store, r.layout.BatchResults(b)) {
				if !yield(f, err) || err != nil {
					return
				}
			}
		}
	}
}

// planCalls are a plan's call entries, read a batch at a time from a local
// copy of the plan: a step holds one batch of a plan of any size in memory.
type planCalls struct {
	file *os.File
	key  string
	// total is how many call entries the plan has.
	total int
	// next yields the call entries from the copy's start; at is how many it
	// has yielded. last is the batch read last, kept (cached) for a reclaim
	// or an abandon asking for it again.
	next   func() (Entry, error, bool)
	stop   func()
	at     int
	last   int
	cached []Entry
}

// planCalls copies the plan under key to a temporary file in f.Dir and
// counts its call entries.
func (f *Files) planCalls(ctx context.Context, key string) (_ *planCalls, err error) {
	file, err := f.temp("foundry-enrichment-plan-*.jsonl.gz")
	if err != nil {
		return nil, fmt.Errorf("copy %s: %w", key, err)
	}
	p := &planCalls{file: file, key: key, last: -1}
	defer func() {
		if err != nil {
			p.close()
		}
	}()
	body, err := f.Store.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	_, err = io.Copy(file, body)
	_ = body.Close() // best-effort: read in full
	if err != nil {
		return nil, fmt.Errorf("copy %s: %w", key, err)
	}
	for _, err := range p.entries() {
		if err != nil {
			return nil, err
		}
		p.total++
	}
	return p, nil
}

// entries yields the copy's call entries from its start.
func (p *planCalls) entries() iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) {
		if _, err := p.file.Seek(0, io.SeekStart); err != nil {
			yield(Entry{}, fmt.Errorf("read %s: %w", p.key, err))
			return
		}
		for e, err := range decode[Entry](p.file, p.key) {
			if err != nil || e.Action == ActionCall {
				if !yield(e, err) || err != nil {
					return
				}
			}
		}
	}
}

// batch is batch b's call entries: batchLen(b) of them, from b*BatchSize.
// Batches are read forward; reading an earlier one than the last reads the
// copy again from its start.
func (p *planCalls) batch(b int) ([]Entry, error) {
	if p == nil {
		return nil, errors.New("the step's plan was not read")
	}
	if b == p.last {
		return p.cached, nil
	}
	from := b * BatchSize
	if p.next == nil || from < p.at {
		p.rewind()
	}
	var out []Entry
	for p.at < min(from+BatchSize, p.total) {
		e, err, ok := p.next()
		switch {
		case err != nil:
			return nil, err
		case !ok:
			return nil, fmt.Errorf("plan %s ended at call %d of %d", p.key, p.at, p.total)
		}
		if p.at >= from {
			out = append(out, e)
		}
		p.at++
	}
	p.last, p.cached = b, out
	return out, nil
}

// batchLen is how many call entries batch b has.
func (p *planCalls) batchLen(b int) int {
	if p == nil {
		return 0
	}
	return max(min(BatchSize, p.total-b*BatchSize), 0)
}

// rewind reads the copy again from its start.
func (p *planCalls) rewind() {
	if p.stop != nil {
		p.stop()
	}
	p.next, p.stop = iter.Pull2(p.entries())
	p.at = 0
}

// close closes the copy, which goes with it; a nil plan is none.
func (p *planCalls) close() {
	if p == nil {
		return
	}
	if p.stop != nil {
		p.stop()
	}
	_ = p.file.Close() // best-effort: unlinked, the copy is gone either way
}

// lease keeps a step's lease while it runs, and fences its calls on it.
type lease struct {
	// fenceAt is when, in Unix nanoseconds, a call may no longer start: a
	// call's timeout before the lease could lapse.
	fenceAt atomic.Int64
	stop    atomic.Bool
	cancel  atomic.Bool
	halted  atomic.Bool
	done    chan struct{}
	stopped sync.Once
	wg      sync.WaitGroup
}

// hold renews the step's lease every third of it, reading whether a Stop or
// a cancel was asked for and whether the provider is halted. Losing the
// lease (another claim has the step) cancels the claim; a renewal that
// fails otherwise leaves the fence where the last one put it, so calls stop
// before the lease could lapse. The client's timeout bounds a call.
func (w *Worker) hold(ctx context.Context, cancel context.CancelFunc, loadID, owner string, leased time.Time) *lease {
	margin := w.Cfg.Lease - w.Client.HTTP.Timeout
	l := &lease{done: make(chan struct{})}
	l.fenceAt.Store(leased.Add(margin).UnixNano())
	l.wg.Go(func() {
		ticker := time.NewTicker(w.Cfg.Lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-l.done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewed := w.Now()
				renewCtx, cancelRenew := context.WithTimeout(ctx, w.Cfg.Lease/3)
				signals, err := w.DB.RenewFoundryLoadEnrichmentLease(renewCtx, loadID, owner, string(w.Cfg.Provider), w.Cfg.Lease)
				cancelRenew()
				switch {
				case errors.Is(err, store.ErrNotFound):
					log.From(ctx).Warn("foundry enrichment step lease lost", "load", loadID)
					cancel()
					return
				case err != nil:
					log.From(ctx).Warn("renew foundry enrichment step lease failed", "load", loadID, "err", err)
				default:
					l.fenceAt.Store(renewed.Add(margin).UnixNano())
					l.stop.Store(signals.Stop)
					l.cancel.Store(signals.Cancel)
					l.halted.Store(signals.Halted)
				}
			}
		}
	})
	return l
}

func (l *lease) fence() time.Time { return time.Unix(0, l.fenceAt.Load()) }

func (l *lease) signals() model.FoundryEnrichmentSignals {
	return model.FoundryEnrichmentSignals{Stop: l.stop.Load(), Cancel: l.cancel.Load(), Halted: l.halted.Load()}
}

// release stops renewing, once a renewal in flight has landed; releasing
// again does nothing.
func (l *lease) release() {
	l.stopped.Do(func() { close(l.done) })
	l.wg.Wait()
}

// handOn lets the lease run out now, for the next claim to take the step at
// once. Renewing stops first: a renewal landing after the release would put
// the lease back for its whole length.
func (l *lease) handOn(ctx context.Context, db RunStore, loadID, workerID string) error {
	l.release()
	return db.ReleaseFoundryLoadEnrichment(ctx, loadID, workerID)
}

func ended(status model.FoundryEnrichmentStatus) *model.FoundryEnrichmentEnd {
	end := model.EndFoundryEnrichment(status)
	return &end
}

func partial(why model.FoundryEnrichmentStop) *model.FoundryEnrichmentEnd {
	end := model.PartialFoundryEnrichment(why)
	return &end
}
