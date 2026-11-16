// Package foundryautomationworker runs automations (package automation):
// each tick it fires the schedules that came due, looks for the work each
// event trigger listens to that ended (a sync, a load, an upload, a draft),
// and walks the queued runs a step at a time, as each automation's owner.
//
// Event triggers poll, with a cursor each: an automation reads the rows of
// its own target that ended after its cursor, through that target's index,
// once their end is foundry.automations.settle old, so every transaction
// that set an earlier end has committed and the cursor never passes a row
// that shows up later. The run it queues and the cursor's move commit
// together (store.FireFoundryAutomation), and a run's trigger key is unique
// per automation, so a fire is delivered at most once. Polling was chosen
// over an outbox written where work ends: it reads only the rows the work
// already writes, so no code path that ends work can forget to tell
// automations, and nothing is added to those transactions.
//
// A step runs as the owner, through the host's session (integration.
// SessionHost), opened afresh each attempt: an owner who lost access pauses
// the automation. An Assistant step queues an Assistant turn in the run's
// thread, which the foundry-agent-worker runs, and the run waits for it,
// handed back to the queue. An action step runs once: marked executing first,
// so an attempt that dies in it leaves it to be found failed, never run
// again. A sync or a load it started is recorded on the step, in the write
// that marks the work as the run's (store.AwaitFoundryAutomationJob), and the
// step waits for it the same way, ending as it ends: a run picked up again
// reads the step's job and waits on, never starting another.
//
// Work an automation's run started never fires that automation again, nor
// an automation whose run set that run off, however many runs between: a
// sync or a load carries the run that started it and how many runs led to
// it, and an event trigger records the work that would loop, or that comes
// past maxChainDepth runs, as a skipped run saying why. Off
// (foundry.automations.enabled false) it fires and walks nothing.
package foundryautomationworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/dig"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/automation"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/server/endpoint"
	"github.com/helixdb/helix-foundry/backend/server/tasks"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const Name = "foundry-automation-worker"

const (
	// fireBatch is the most schedules one tick fires; any more fire on the next.
	fireBatch = 100
	// endedBatch is the most ended rows one automation reads in a tick.
	endedBatch = 100
	// runsPerTick is the most runs one tick walks.
	runsPerTick = 20
	// stepsPerAttempt bounds one attempt's steps: a spec has at most
	// automation.MaxNodes, each ended once.
	stepsPerAttempt = automation.MaxNodes + 1
	// actionTimeout bounds one action step's call; a run's lease, renewed
	// before each step, outlasts it (config.MinAutomationLease).
	actionTimeout = config.AutomationActionTimeout
	// syncWait and loadWait bound how long an action step waits for the sync
	// or the load it started to end: a load may take hours, a sync seldom
	// more than minutes. The run is handed back between reads, so no lease
	// is held meanwhile; it reads again after a tenth of the time the work has
	// taken so far, between foundry.automations.step_wait and jobPollMax.
	syncWait   = time.Hour
	loadWait   = 6 * time.Hour
	jobPollMax = time.Minute
	// maxChainDepth is how many automation runs in a row may each set off the
	// next through a sync or a load it started; work past it fires nothing.
	maxChainDepth = 3
	// runRetention is how long a run is kept after it ended; pruneWait how
	// long the worker waits to prune again once nothing is left to.
	runRetention = 90 * 24 * time.Hour
	pruneWait    = time.Minute
	// writeTimeout bounds recording a step's or a run's end, even while the
	// worker stops.
	writeTimeout = 15 * time.Second
	// maxThreadTitle bounds the run's thread's title.
	maxThreadTitle = 200
)

type Deps struct {
	dig.In

	Cfg    *config.Config
	Logger *slog.Logger
	Health endpoint.Health    `optional:"true"`
	DB     foundrystore.Store `optional:"true"`
	// Sessions opens an owner's tools; required while automations are on.
	Sessions integration.SessionHost `optional:"true"`
}

// Store is what the worker reads and writes of Foundry's store.
type Store interface {
	DueFoundryAutomationSchedules(ctx context.Context, limit int) ([]*model.FoundryAutomation, error)
	WatchedFoundryAutomations(ctx context.Context, every time.Duration, limit int) ([]*model.FoundryAutomation, error)
	FindFoundryAutomationEnded(ctx context.Context, a *model.FoundryAutomation, settle time.Duration, limit int) ([]model.FoundryAutomationEnded, error)
	TouchFoundryAutomation(ctx context.Context, id string) error
	FireFoundryAutomation(ctx context.Context, f model.FoundryAutomationFire, limits model.FoundryAutomationLimits) (model.FoundryAutomationFired, error)
	ClaimFoundryAutomationRun(ctx context.Context, workerID string, lease time.Duration) (*model.FoundryAutomationRun, error)
	RenewFoundryAutomationRunLease(ctx context.Context, id, workerID string, lease time.Duration) error
	ListFoundryAutomationRunNodes(ctx context.Context, runID string) ([]*model.FoundryAutomationRunNode, error)
	GetFoundryAutomation(ctx context.Context, workspaceID, id string) (*model.FoundryAutomation, error)
	StartFoundryAutomationNode(ctx context.Context, runID, workerID, nodeID string, status model.FoundryAutomationNodeStatus) error
	StartFoundryAutomationAssistant(ctx context.Context, runID, workerID, nodeID string, thread *model.FoundryAgentThread, turn *model.FoundryAgentTurn) error
	EndFoundryAutomationNode(ctx context.Context, runID, workerID, nodeID string, end model.FoundryAutomationNodeEnd) error
	AwaitFoundryAutomationJob(ctx context.Context, runID, workerID, nodeID string, job model.FoundryAutomationJob, output json.RawMessage) error
	GetConnectorSync(ctx context.Context, workspaceID, id string) (*model.ConnectorSync, error)
	GetFoundryLoad(ctx context.Context, ontologyID, id string) (*model.FoundryLoad, error)
	ListFoundryLoadJobs(ctx context.Context, loadIDs []string, newest int) (map[string]model.FoundryLoadJobs, error)
	YieldFoundryAutomationRun(ctx context.Context, id, workerID string, at time.Time) error
	RetryFoundryAutomationRun(ctx context.Context, id, workerID string, at time.Time, why string) error
	FinishFoundryAutomationRun(ctx context.Context, id, workerID string, end model.FoundryAutomationRunEnd) error
	PruneFoundryAutomationRuns(ctx context.Context, before time.Time) (int, error)
	GetFoundryAgentTurn(ctx context.Context, workspaceID, id string) (*model.FoundryAgentTurn, error)
	ListFoundryAgentEvents(ctx context.Context, turnID string, afterSeq, limit int) ([]model.FoundryAgentEvent, error)
	ListFoundryAgentActions(ctx context.Context, threadID string) ([]*model.FoundryAgentAction, error)
	// The uses of ontologies its runs record (agent.AutomationUses).
	agent.WorkflowStore
}

// Task fires automations and walks their runs.
type Task struct {
	deps     Deps
	cfg      config.AutomationsConfig
	db       Store
	sessions integration.SessionHost
	// assistant is whether the Assistant runs turns here (foundry.agent.enabled).
	assistant bool
	// email is whether a notify step emails: the host sends mail (hosted),
	// not a standalone install.
	email bool
	// assess is whether a run's end queues its workflow runs' assessments
	// (foundry.workflows.assess.enabled).
	assess   bool
	workerID string
	now      func() time.Time
	worker   tasks.Worker
	// pruneAt is when the worker next deletes runs past runRetention.
	pruneAt time.Time
}

func New(d Deps) (*Task, error) {
	cfg := d.Cfg.Foundry.Automations
	t := &Task{deps: d, cfg: cfg, sessions: d.Sessions, assistant: d.Cfg.Foundry.Agent.Enabled, email: !d.Cfg.Host.Local,
		assess: d.Cfg.Foundry.Workflows.Assess.Enabled, workerID: id.NewWorkerID(), now: time.Now}
	if !cfg.Enabled {
		return t, nil
	}
	if d.DB == nil || d.Sessions == nil {
		return nil, errors.New("foundry-automation-worker: the store and a session host are required while foundry.automations.enabled")
	}
	t.db = d.DB
	return t, nil
}

func (t *Task) Name() string { return Name }

// Start fires and walks every PollInterval; off, it only serves its health
// listener.
func (t *Task) Start(ctx context.Context) error {
	return t.worker.Run(ctx, Name, t.cfg.HTTP.Listen, t.deps.Health, t.deps.Logger, func(ctx context.Context) error {
		if !t.cfg.Enabled {
			<-ctx.Done()
			return nil
		}
		tasks.Tick(ctx, t.cfg.PollInterval, t.tick)
		return nil
	})
}

func (t *Task) Stop(ctx context.Context) error { return t.worker.Stop(ctx) }

func (t *Task) tick(ctx context.Context) {
	t.fireSchedules(ctx)
	t.watch(ctx)
	t.prune(ctx)
	for range runsPerTick {
		if ctx.Err() != nil {
			return
		}
		run, err := t.db.ClaimFoundryAutomationRun(ctx, t.workerID, t.cfg.Lease)
		if err != nil {
			log.From(ctx).Error("claim an automation run failed", "err", err)
			return
		}
		if run == nil {
			return
		}
		t.advance(ctx, run)
	}
}

// prune deletes a batch of the runs that ended more than runRetention ago,
// then waits pruneWait once none is left: run history is bounded by time,
// whatever fires.
func (t *Task) prune(ctx context.Context) {
	if t.now().Before(t.pruneAt) {
		return
	}
	n, err := t.db.PruneFoundryAutomationRuns(ctx, t.now().Add(-runRetention))
	if err != nil {
		log.From(ctx).Warn("prune automation runs failed", "err", err)
	}
	if err != nil || n == 0 {
		t.pruneAt = t.now().Add(pruneWait)
	}
}

func (t *Task) limits() model.FoundryAutomationLimits {
	return model.FoundryAutomationLimits{RunsPerDay: t.cfg.RunsPerDay}
}

// fireSchedules fires every schedule that came due: one run each, however
// long it was due (a worker that was down fires once, not once a missed
// time), its next time the first after now.
func (t *Task) fireSchedules(ctx context.Context) {
	due, err := t.db.DueFoundryAutomationSchedules(ctx, fireBatch)
	if err != nil {
		log.From(ctx).Error("find automation schedules due failed", "err", err)
		return
	}
	for _, a := range due {
		spec, err := automation.ParseSpec(a.Spec)
		if err != nil || a.NextFireAt == nil {
			log.From(ctx).Error("an automation's schedule can't be read", "automation", a.ID, "err", err)
			continue
		}
		schedule, ok := spec.Trigger().(automation.Schedule)
		if !ok {
			log.From(ctx).Error("an automation due to fire has no schedule", "automation", a.ID)
			continue
		}
		at := a.NextFireAt.UTC()
		output := automation.Output{Summary: "Its schedule fired at " + at.In(schedule.Zone).Format("Mon 2 Jan 15:04") + " (" + schedule.ZoneName + ")."}
		t.fire(ctx, a, spec, model.FoundryScheduleAdvance{Due: *a.NextFireAt, Next: schedule.Next(maxTime(t.now(), at))},
			"schedule:"+at.Format(time.RFC3339Nano), model.FoundryAutomationEvent{Kind: string(automation.TriggerSchedule), At: at}, &output, "")
	}
}

// watch looks for the work each event trigger listens to that ended, and
// fires one run for what it found: the newest of it names the run.
func (t *Task) watch(ctx context.Context) {
	watched, err := t.db.WatchedFoundryAutomations(ctx, t.cfg.WatchEvery, t.cfg.WatchBatch)
	if err != nil {
		log.From(ctx).Error("find watched automations failed", "err", err)
		return
	}
	for _, a := range watched {
		if ctx.Err() != nil {
			return
		}
		ended, err := t.db.FindFoundryAutomationEnded(ctx, a, t.cfg.Settle, endedBatch)
		if err != nil {
			log.From(ctx).Error("find an automation's ended work failed", "automation", a.ID, "err", err)
			continue
		}
		if len(ended) == 0 {
			if err := t.db.TouchFoundryAutomation(ctx, a.ID); err != nil {
				log.From(ctx).Warn("record an automation's look failed", "automation", a.ID, "err", err)
			}
			continue
		}
		spec, err := automation.ParseSpec(a.Spec)
		if err != nil {
			log.From(ctx).Error("an automation's spec can't be read", "automation", a.ID, "err", err)
			continue
		}
		advance := model.FoundryCursorAdvance{From: *a.Cursor, To: ended[len(ended)-1].Cursor}
		var fired, looped []model.FoundryAutomationEnded
		why := ""
		for _, e := range matching(spec.Trigger(), ended) {
			if loop := loops(a.ID, spec.Trigger(), e); loop != "" {
				looped, why = append(looped, e), loop
				continue
			}
			fired = append(fired, e)
		}
		switch {
		case len(fired) > 0:
			// Work that would loop beside it is passed over.
			event, output := eventOf(spec.Trigger(), fired)
			t.fire(ctx, a, spec, advance, event.Kind+":"+fired[len(fired)-1].Cursor.ID, event, &output, "")
		case len(looped) > 0:
			// Recorded, so its owner sees why it didn't run.
			event, output := eventOf(spec.Trigger(), looped)
			t.fire(ctx, a, spec, advance, event.Kind+":"+looped[len(looped)-1].Cursor.ID, event, &output, why)
		default:
			t.fire(ctx, a, spec, advance, "", model.FoundryAutomationEvent{}, nil, "")
		}
	}
}

// loops says why automationID's trigger passes over ended work that would
// loop: a sync or a load its own run started, or a document a sync it
// started wrote, or such work of a run of an automation that one of its runs
// set off (through any number of others), or that came after more than
// maxChainDepth automation runs in a row; "" when it may fire. Work a person
// or Foundry itself started has no cause and always may.
func loops(automationID string, trigger automation.Trigger, e model.FoundryAutomationEnded) string {
	c := e.Cause
	if c == nil {
		return ""
	}
	what, started := "the sync", "was started by"
	switch trigger.(type) {
	case automation.LoadReady:
		what = "the load"
	case automation.DocumentArrived:
		what, started = "the document", "was written by a sync started by"
	}
	switch {
	case c.AutomationID == automationID:
		return "Skipped: " + what + " " + started + " this workflow itself; running on it would loop."
	case slices.Contains(c.Chain, automationID):
		return "Skipped: " + what + " " + started + " a workflow this one set off; running on it would loop."
	case c.Depth > maxChainDepth:
		return fmt.Sprintf("Skipped: %s came from a chain of %d workflows setting each other off; a chain stops after %d.", what, c.Depth, maxChainDepth)
	}
	return ""
}

// matching is the ended work trigger fires on, in order: a sync or a draft
// that ended as its outcome says, a load ready or partial, any document.
func matching(trigger automation.Trigger, ended []model.FoundryAutomationEnded) []model.FoundryAutomationEnded {
	var out []model.FoundryAutomationEnded
	for _, e := range ended {
		ok := false
		switch tr := trigger.(type) {
		case automation.SyncFinished:
			ok = tr.Outcome == automation.OutcomeAny || e.Outcome == string(tr.Outcome)
		case automation.GenerationFinished:
			ok = tr.Outcome == automation.OutcomeAny || e.Outcome == string(tr.Outcome)
		case automation.LoadReady:
			ok = e.Outcome == string(model.FoundryLoadReady) || e.Outcome == string(model.FoundryLoadPartial)
		case automation.DocumentArrived:
			ok = true
		}
		if ok {
			out = append(out, e)
		}
	}
	return out
}

// eventOf is what fired a run on matched work, and the trigger's output: the
// newest names it; a sync's count is the rows it wrote, documents' how many
// arrived.
func eventOf(trigger automation.Trigger, matched []model.FoundryAutomationEnded) (model.FoundryAutomationEvent, automation.Output) {
	newest := matched[len(matched)-1]
	event := model.FoundryAutomationEvent{Kind: string(automation.KindOf(trigger)), At: newest.Cursor.At.UTC(), Outcome: newest.Outcome,
		Count: int64(len(matched))}
	// The run's cause is every run that started what it found, and what set
	// those off: the work it starts carries it on.
	for _, e := range matched {
		if c := e.Cause; c != nil {
			event.CausedBy = append(event.CausedBy, c.Chain...)
			if c.AutomationID != "" {
				event.CausedBy = append(event.CausedBy, c.AutomationID)
			}
			event.ChainDepth = max(event.ChainDepth, c.Depth)
		}
	}
	slices.Sort(event.CausedBy)
	event.CausedBy = slices.Compact(event.CausedBy)
	var out automation.Output
	switch trigger.(type) {
	case automation.SyncFinished:
		event.SyncID, event.Rows = newest.Cursor.ID, newest.Rows
		out.Summary, out.Count = "A sync of its connector "+newest.Outcome+".", newest.Rows
	case automation.LoadReady:
		event.LoadID = newest.Cursor.ID
		out.Summary = "A load of its ontology is " + newest.Outcome + "."
	case automation.GenerationFinished:
		event.GenerationID = newest.Cursor.ID
		out.Summary = "A draft of its ontology " + newest.Outcome + "."
	case automation.DocumentArrived:
		for _, e := range matched {
			event.DocumentIDs = append(event.DocumentIDs, e.Cursor.ID)
		}
		count := int64(len(matched))
		out.Summary, out.Count = fmt.Sprintf("%d documents arrived in its set.", count), &count
		if count == 1 {
			out.Summary = "A document arrived in its set."
		}
	}
	return event, out
}

// fire moves a on as advance says and, with output, queues its run, which
// trigger key names and event says the cause of; with skip, records it
// skipped saying so.
func (t *Task) fire(ctx context.Context, a *model.FoundryAutomation, spec automation.Spec, advance model.FoundryAutomationAdvance, key string,
	event model.FoundryAutomationEvent, output *automation.Output, skip string) {
	f := model.FoundryAutomationFire{AutomationID: a.ID, WorkspaceID: a.WorkspaceID, Advance: advance, Skip: skip}
	if output != nil {
		rawEvent, err := json.Marshal(event)
		if err != nil {
			log.From(ctx).Error("write an automation's event failed", "automation", a.ID, "err", err)
			return
		}
		rawOutput, err := json.Marshal(output)
		if err != nil {
			log.From(ctx).Error("write an automation's trigger output failed", "automation", a.ID, "err", err)
			return
		}
		f.Run = &model.FoundryAutomationRun{ID: id.New(), AutomationID: a.ID, WorkspaceID: a.WorkspaceID, OwnerUserID: a.OwnerUserID,
			TriggerKey: key, TriggerEvent: rawEvent}
		f.TriggerNodeID, f.TriggerOutput = spec.TriggerID(), rawOutput
	}
	fired, err := t.db.FireFoundryAutomation(ctx, f, t.limits())
	if err != nil {
		log.From(ctx).Error("fire an automation failed", "automation", a.ID, "err", err)
		return
	}
	fires.WithLabelValues(a.TriggerKind, string(fired)).Inc()
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// runState is an attempt of a run: its spec, its steps' states, the
// owner's session, and the run's uses of ontologies.
type runState struct {
	run    *model.FoundryAutomationRun
	spec   automation.Spec
	states map[string]automation.NodeState
	turns  map[string]string
	// jobs are the syncs and loads action steps wait for, by step.
	jobs    map[string]string
	session integration.Session
	name    string
	uses    *agent.AutomationUses
}

// advance walks the claimed run until it ends or waits, then hands it on:
// finished, yielded until its wait ends, or retried after a failure that
// isn't its steps'.
func (t *Task) advance(ctx context.Context, run *model.FoundryAutomationRun) {
	logger := log.From(ctx).With("run", run.ID, "automation", run.AutomationID, "attempt", run.Attempts)
	ctx = log.Into(ctx, logger)
	if run.Attempts > t.cfg.MaxAttempts {
		t.finish(ctx, run, model.FoundryAutomationRunEnd{Status: model.FoundryAutomationRunFailed,
			Error: fmt.Sprintf("The run stopped without finishing %d times.", run.Attempts-1)})
		return
	}
	st, err := t.open(ctx, run)
	var refused accessRefused
	switch {
	case errors.As(err, &refused):
		t.finish(ctx, run, model.FoundryAutomationRunEnd{Status: model.FoundryAutomationRunFailed, LostAccess: true,
			Error: "Its owner can no longer act in this workspace, so it paused."})
		return
	case errors.Is(err, errBroken):
		t.finish(ctx, run, model.FoundryAutomationRunEnd{Status: model.FoundryAutomationRunFailed, Error: err.Error()})
		return
	case err != nil:
		t.retry(ctx, run, err)
		return
	}
	for range stepsPerAttempt {
		// Each step starts with a lease that outlasts it: an attempt walks
		// many steps, and an action may take actionTimeout.
		err := t.db.RenewFoundryAutomationRunLease(ctx, run.ID, t.workerID, t.cfg.Lease)
		if errors.Is(err, foundrystore.ErrNotFound) {
			runs.WithLabelValues("lost").Inc()
			logger.Info("automation run: the lease was lost; nothing more is recorded")
			return
		}
		if err != nil {
			t.retry(ctx, run, err)
			return
		}
		next := st.spec.Next(st.states)
		switch step := next.(type) {
		case automation.Done:
			end := model.FoundryAutomationRunEnd{Status: model.FoundryAutomationRunSucceeded}
			if step.Failed {
				end = model.FoundryAutomationRunEnd{Status: model.FoundryAutomationRunFailed, Error: st.failure()}
			}
			end.Answer = st.answer(end)
			t.finish(ctx, run, end)
			return
		case automation.SkipNode:
			end := model.FoundryAutomationNodeEnd{Status: model.FoundryAutomationNodeSkipped}
			if step.AfterFailure {
				end.Status = model.FoundryAutomationNodeBlocked
			}
			err = t.end(ctx, st, step.ID, end)
		case automation.WaitNode:
			var pause time.Duration
			if pause, err = t.wait(ctx, st, step.ID); err == nil && pause > 0 {
				t.yield(ctx, run, pause)
				return
			}
		case automation.RunNode:
			var pause time.Duration
			if pause, err = t.runNode(ctx, st, step.ID); err == nil && pause > 0 {
				t.yield(ctx, run, pause)
				return
			}
		}
		switch {
		case errors.Is(err, foundrystore.ErrNotFound):
			// Cancelled, deleted, or taken by another worker.
			runs.WithLabelValues("lost").Inc()
			logger.Info("automation run: the lease was lost; nothing more is recorded")
			return
		case err != nil:
			t.retry(ctx, run, err)
			return
		}
	}
	t.retry(ctx, run, errors.New("the run took more steps than its spec has"))
}

// errBroken is a run that can never go on: its spec can't be read.
var errBroken = errors.New("the run's spec can't be read")

// accessRefused is the owner's session refused: they can't act in the
// workspace.
type accessRefused struct{ err error }

func (r accessRefused) Error() string { return "the owner's access was refused: " + r.err.Error() }

// open reads the run's spec and steps and opens its owner's session. A step
// an attempt that died left executing ends failed, never run again.
func (t *Task) open(ctx context.Context, run *model.FoundryAutomationRun) (*runState, error) {
	spec, err := automation.ParseSpec(run.Spec)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBroken, err)
	}
	a, err := t.db.GetFoundryAutomation(ctx, run.WorkspaceID, run.AutomationID)
	if err != nil {
		return nil, err
	}
	session, err := t.sessions.Session(ctx, ports.Actor{Source: ports.SourceUser, UserID: run.OwnerUserID, WorkspaceID: run.WorkspaceID},
		integration.ConfirmByCaller{})
	switch status.Code(err) {
	case codes.OK:
	case codes.PermissionDenied, codes.NotFound, codes.Unauthenticated:
		return nil, accessRefused{err: err}
	default:
		return nil, fmt.Errorf("open the owner's session: %w", err)
	}
	nodes, err := t.db.ListFoundryAutomationRunNodes(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	st := &runState{run: run, spec: spec, states: map[string]automation.NodeState{}, turns: map[string]string{}, jobs: map[string]string{},
		session: session, name: a.Name,
		uses: agent.NewAutomationUses(t.db, run, a.Name, spec)}
	for _, n := range nodes {
		state := automation.NodeState{Status: automation.NodeStatus(n.Status)}
		if n.Status == model.FoundryAutomationNodeExecuting {
			if err := t.end(ctx, st, n.NodeID, model.FoundryAutomationNodeEnd{Status: model.FoundryAutomationNodeFailed,
				Error: "It stopped while its action ran, so it may or may not have taken effect; it isn't run again."}); err != nil {
				return nil, err
			}
			continue
		}
		if len(n.Output) > 0 {
			if err := json.Unmarshal(n.Output, &state.Output); err != nil {
				return nil, fmt.Errorf("%w: step %s's output: %v", errBroken, n.NodeID, err)
			}
		}
		if n.Error != nil {
			state.Output.Summary = *n.Error
		}
		if n.TurnID != nil {
			st.turns[n.NodeID] = *n.TurnID
		}
		if n.JobID != nil {
			st.jobs[n.NodeID] = *n.JobID
		}
		st.states[n.NodeID] = state
	}
	return st, nil
}

// answer is what the run found, in words, as its workflow runs' answer: how
// it ended, then each step that ended, in order, with what it found.
func (st *runState) answer(end model.FoundryAutomationRunEnd) string {
	var b strings.Builder
	if end.Status == model.FoundryAutomationRunSucceeded {
		b.WriteString("The run succeeded.")
	} else {
		b.WriteString("The run failed: " + end.Error)
	}
	b.WriteString("\nWhat its steps found:")
	for _, id := range st.spec.Order()[1:] {
		state, ok := st.states[id]
		if !ok {
			continue
		}
		node, _ := st.spec.Node(id)
		fmt.Fprintf(&b, "\n- The %s step %q %s", node.Kind(), id, state.Status)
		if out := state.Output; out.Summary != "" {
			b.WriteString(": " + out.Summary)
		}
		if out := state.Output; out.Answer != nil {
			fmt.Fprintf(&b, " (It answered %s.)", map[bool]string{true: "yes", false: "no"}[*out.Answer])
		}
		if out := state.Output; out.Count != nil && node.Kind() == automation.KindAssistant {
			fmt.Fprintf(&b, " (It counted %d.)", *out.Count)
		}
	}
	return agent.CutText(b.String(), model.FoundryWorkflowOutputMaxBytes)
}

// failure is why the run failed: its first failed step's error.
func (st *runState) failure() string {
	for _, id := range st.spec.Order() {
		if s := st.states[id]; s.Status == automation.NodeFailed {
			node, _ := st.spec.Node(id)
			return fmt.Sprintf("The %s step %q failed: %s", node.Kind(), id, s.Output.Summary)
		}
	}
	return "A step failed."
}

// end ends a step of the run as end says, and keeps its state.
func (t *Task) end(ctx context.Context, st *runState, nodeID string, end model.FoundryAutomationNodeEnd) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if len(end.Output) == 0 {
		end.Output = json.RawMessage("{}")
	}
	err := t.db.EndFoundryAutomationNode(writeCtx, st.run.ID, t.workerID, nodeID, end)
	if errors.Is(err, foundrystore.ErrConflict) {
		// An earlier attempt ended it: read it again on the next.
		return errors.New("a step had ended already")
	}
	if err != nil {
		return err
	}
	var out automation.Output
	_ = json.Unmarshal(end.Output, &out)
	if end.Error != "" {
		out.Summary = end.Error
	}
	st.states[nodeID] = automation.NodeState{Status: automation.NodeStatus(end.Status), Output: out}
	steps.WithLabelValues(string(end.Status)).Inc()
	return nil
}

// endWith ends a step succeeded with output.
func (t *Task) endWith(ctx context.Context, st *runState, nodeID string, output automation.Output, usd int64) error {
	raw, err := json.Marshal(output)
	if err != nil {
		return err
	}
	return t.end(ctx, st, nodeID, model.FoundryAutomationNodeEnd{Status: model.FoundryAutomationNodeSucceeded, Output: raw, USDMicros: usd})
}

// fail ends a step failed, saying why.
func (t *Task) fail(ctx context.Context, st *runState, nodeID, why string) error {
	return t.end(ctx, st, nodeID, model.FoundryAutomationNodeEnd{Status: model.FoundryAutomationNodeFailed, Error: why})
}

// runNode starts the step and, unless it waits (an Assistant step's turn,
// an action's sync or load), ends it. pause is how long the run waits before
// it reads the step again; 0 when it goes on.
func (t *Task) runNode(ctx context.Context, st *runState, nodeID string) (pause time.Duration, err error) {
	node, _ := st.spec.Node(nodeID)
	switch c := node.Config.(type) {
	case automation.TriggerNode:
		return 0, t.endWith(ctx, st, nodeID, automation.Output{}, 0)
	case automation.AssistantNode:
		return t.startAssistant(ctx, st, nodeID, c)
	case automation.ActionNode:
		return t.runAction(ctx, st, nodeID, c.Action)
	case automation.ConditionNode:
		from := st.spec.In(nodeID)[0].From
		branch, err := c.Check.Evaluate(st.states[from].Output)
		if err != nil {
			return 0, t.fail(ctx, st, nodeID, sentence(err.Error()))
		}
		summary := "It took its false branch."
		if branch {
			summary = "It took its true branch."
		}
		return 0, t.endWith(ctx, st, nodeID, automation.Output{Summary: summary, Branch: &branch}, 0)
	case automation.NotifyNode:
		return 0, t.notify(ctx, st, nodeID)
	}
	return 0, fmt.Errorf("%w: step %s is of no kind this worker runs", errBroken, nodeID)
}

// startAssistant queues the step's turn in the run's thread, as its owner,
// and waits for it.
func (t *Task) startAssistant(ctx context.Context, st *runState, nodeID string, step automation.AssistantNode) (time.Duration, error) {
	if !t.assistant {
		return 0, t.fail(ctx, st, nodeID, "Founder isn't available here.")
	}
	run := st.run
	var thread *model.FoundryAgentThread
	threadID := ""
	if run.ThreadID != nil {
		threadID = *run.ThreadID
	} else {
		threadID = id.New()
		thread = &model.FoundryAgentThread{ID: threadID, WorkspaceID: run.WorkspaceID, UserID: run.OwnerUserID, Kind: model.FoundryAgentThreadAutomation,
			Title: title(st.name), Runtime: model.FoundryAgentRuntimeAPI}
	}
	runID, node := run.ID, nodeID
	turn := &model.FoundryAgentTurn{ID: id.New(), WorkspaceID: run.WorkspaceID, ThreadID: threadID, UserID: run.OwnerUserID,
		Kind: model.FoundryAgentTurnAutomation, Input: instruction(step.Instruction, run), AutomationRunID: &runID, AutomationNodeID: &node,
		MaxUSDMicros: t.cfg.MaxMicros()}
	err := t.db.StartFoundryAutomationAssistant(ctx, run.ID, t.workerID, nodeID, thread, turn)
	switch {
	case errors.Is(err, foundrystore.ErrThreadBusy):
		// Its owner ran a card an earlier step left: the step starts once
		// that turn ends.
		return t.cfg.StepWait, nil
	case errors.Is(err, foundrystore.ErrConflict):
		return 0, errors.New("the step had started already")
	}
	if err != nil {
		return 0, err
	}
	if thread != nil {
		run.ThreadID = &threadID
	}
	st.turns[nodeID] = turn.ID
	st.states[nodeID] = automation.NodeState{Status: automation.NodeRunning}
	return t.cfg.StepWait, nil
}

// instruction is an Assistant step's message: its owner's instruction, and
// what fired the run, as data.
func instruction(text string, run *model.FoundryAutomationRun) string {
	about, _ := json.Marshal(map[string]json.RawMessage{"trigger": run.TriggerEvent}) // JSON already: it can't fail
	return text + "\n\n<workflow_context>" + string(about) + "</workflow_context>"
}

// title is a run's thread's title: its automation's name.
func title(name string) string {
	if utf8.RuneCountInString(name) <= maxThreadTitle {
		return name
	}
	return string([]rune(name)[:maxThreadTitle])
}

// wait reads what the running step waits for: an action step's sync or
// load (waitJob), else its Assistant turn. Still live, the run waits on, for
// pause; ended, the step ends with it: succeeded with its report (else its
// last words) and spend, or failed with why.
func (t *Task) wait(ctx context.Context, st *runState, nodeID string) (pause time.Duration, err error) {
	if jobID, ok := st.jobs[nodeID]; ok {
		return t.waitJob(ctx, st, nodeID, jobID)
	}
	turnID, ok := st.turns[nodeID]
	if !ok {
		return 0, t.fail(ctx, st, nodeID, "Its Founder turn can't be found.")
	}
	turn, err := t.db.GetFoundryAgentTurn(ctx, st.run.WorkspaceID, turnID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return 0, t.fail(ctx, st, nodeID, "Its Founder turn is gone.")
	}
	if err != nil {
		return 0, err
	}
	if turn.Status.Live() {
		return t.cfg.StepWait, nil
	}
	usd := turn.Spend.USDMicros
	if turn.Status != model.FoundryAgentTurnDone {
		why := "Founder's turn was " + string(turn.Status) + "."
		if turn.Error != nil {
			why = sentence(*turn.Error)
		}
		return 0, t.end(ctx, st, nodeID, model.FoundryAutomationNodeEnd{Status: model.FoundryAutomationNodeFailed, Error: why, USDMicros: usd})
	}
	out, err := t.reported(ctx, turn)
	if err != nil {
		return 0, err
	}
	actions, err := t.db.ListFoundryAgentActions(ctx, turn.ThreadID)
	if err != nil {
		return 0, err
	}
	for _, a := range actions {
		if a.TurnID == turn.ID && a.Status == model.FoundryAgentActionPrepared {
			out.Pending++
		}
	}
	return 0, t.endWith(ctx, st, nodeID, out, usd)
}

// waitJob reads the sync or the load (jobID) an action step started: still
// live, the step waits on, until syncWait or loadWait after the work was
// queued, when it fails saying so (the work goes on); ended, the step ends as
// it did: a sync that succeeded, or a load ready or partial, succeeds; a sync
// or a load that failed fails it, with why.
func (t *Task) waitJob(ctx context.Context, st *runState, nodeID, jobID string) (time.Duration, error) {
	node, _ := st.spec.Node(nodeID)
	step, ok := node.Config.(automation.ActionNode)
	if !ok {
		return 0, fmt.Errorf("%w: step %s waits for work it can't have started", errBroken, nodeID)
	}
	started := st.states[nodeID].Output.Result
	// pending waits on for the work, queued at created, unless it took longer than limit.
	pending := func(what string, created time.Time, limit time.Duration, within string) (time.Duration, error) {
		took := t.now().Sub(created)
		if took >= limit {
			return 0, t.fail(ctx, st, nodeID, fmt.Sprintf("Its %s didn't end within %s, so the step stopped waiting; the %s may still finish.", what, within, what))
		}
		return min(max(took/10, t.cfg.StepWait), jobPollMax), nil
	}
	ws := st.run.WorkspaceID
	switch a := step.Action.(type) {
	case automation.SyncConnector:
		sync, err := t.db.GetConnectorSync(ctx, ws, jobID)
		switch {
		case errors.Is(err, foundrystore.ErrNotFound):
			return 0, t.fail(ctx, st, nodeID, "Its sync is gone.")
		case err != nil:
			return 0, err
		}
		switch sync.Status {
		case model.ConnectorSyncSucceeded:
			out := automation.Output{Summary: "Synced its connector.", Count: sync.RowsSynced, Result: started}
			if sync.RowsSynced != nil {
				out.Summary = fmt.Sprintf("Synced its connector: it wrote %d rows.", *sync.RowsSynced)
			}
			return 0, t.endWith(ctx, st, nodeID, out, 0)
		case model.ConnectorSyncFailed:
			why := "The sync failed."
			if sync.Error != nil && strings.TrimSpace(*sync.Error) != "" {
				why = "The sync failed: " + sentence(*sync.Error)
			}
			return 0, t.fail(ctx, st, nodeID, why)
		}
		return pending("sync", sync.CreatedAt, syncWait, "an hour")
	case automation.CreateLoad:
		load, err := t.db.GetFoundryLoad(ctx, a.OntologyID, jobID)
		switch {
		case errors.Is(err, foundrystore.ErrNotFound):
			return 0, t.fail(ctx, st, nodeID, "Its load is gone.")
		case err != nil:
			return 0, err
		}
		switch load.Status {
		case model.FoundryLoadWriting:
			return pending("load", load.CreatedAt, loadWait, "6 hours")
		case model.FoundryLoadReady:
			return 0, t.endWith(ctx, st, nodeID, automation.Output{Summary: "Loaded its ontology's current version: the load is ready.", Result: started}, 0)
		case model.FoundryLoadPartial:
			return 0, t.endWith(ctx, st, nodeID, automation.Output{
				Summary: "Loaded its ontology's current version: the load is partial, its tables in and some documents unread.", Result: started}, 0)
		case model.FoundryLoadFailed:
			jobs, err := t.db.ListFoundryLoadJobs(ctx, []string{load.ID}, 1)
			if err != nil {
				return 0, err
			}
			why := "The load failed."
			if newest := jobs[load.ID].Newest; len(newest) > 0 && newest[0].Error != nil && strings.TrimSpace(*newest[0].Error) != "" {
				why = "The load failed: " + sentence(*newest[0].Error)
			}
			return 0, t.fail(ctx, st, nodeID, why)
		}
		return 0, t.fail(ctx, st, nodeID, "Its load was deleted before the step saw it end.")
	}
	return 0, fmt.Errorf("%w: step %s waits for work its action doesn't start", errBroken, nodeID)
}

// reported is a turn's step's output: its newest report, else its last words.
func (t *Task) reported(ctx context.Context, turn *model.FoundryAgentTurn) (automation.Output, error) {
	var out automation.Output
	var said string
	for after := 0; after < turn.LastSeq; {
		events, err := t.db.ListFoundryAgentEvents(ctx, turn.ID, after, 200)
		if err != nil {
			return out, err
		}
		if len(events) == 0 {
			break
		}
		for _, e := range events {
			after = e.Seq
			var head struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(e.Event, &head) != nil {
				continue
			}
			switch head.Type {
			case "automation_report":
				var report agent.Reported
				if json.Unmarshal(e.Event, &report) == nil {
					out = automation.Output{Summary: report.Summary, Answer: report.Answer, Count: report.Count}
				}
			case "text":
				var text agent.Said
				if json.Unmarshal(e.Event, &text) == nil {
					said = text.Text
				}
			}
		}
	}
	if out.Summary == "" {
		out.Summary = strings.TrimSpace(said)
	}
	return out, nil
}

// runAction runs an action step once, as the owner approved it: its payload
// resolved from what its target is now, its step marked executing first. A
// sync or a load it started is recorded on the step, which then waits for it
// (pause); a publish ends the step.
func (t *Task) runAction(ctx context.Context, st *runState, nodeID string, a automation.Action) (pause time.Duration, err error) {
	ws := st.run.WorkspaceID
	payload := map[string]any{}
	switch a := a.(type) {
	case automation.CreateLoad:
		o, err := t.db.GetFoundryOntology(ctx, ws, a.OntologyID)
		switch {
		case errors.Is(err, foundrystore.ErrNotFound) || err == nil && o.DeletedAt != nil:
			return 0, t.fail(ctx, st, nodeID, "Its ontology is gone.")
		case err != nil:
			return 0, err
		case o.CurrentVersionID == nil:
			return 0, t.fail(ctx, st, nodeID, "Its ontology has no current version to load.")
		}
		payload["version_id"] = *o.CurrentVersionID
	case automation.PublishDashboard:
		// It refreshes what a person published: the snapshot of the revision
		// already at the link, never an edit no one published since.
		d, err := t.db.GetFoundryDashboard(ctx, ws, a.DashboardID)
		switch {
		case errors.Is(err, foundrystore.ErrNotFound):
			return 0, t.fail(ctx, st, nodeID, "Its dashboard is gone.")
		case err != nil:
			return 0, err
		case d.Share == nil:
			return 0, t.fail(ctx, st, nodeID, "Its dashboard isn't published: publish it on its page, and the workflow refreshes its snapshot from then on.")
		case d.Share.SpecRevision != d.Revision:
			return 0, t.fail(ctx, st, nodeID, "Its dashboard was changed since it was published: publish it again on its page to put the change at its link.")
		}
		payload["expected_revision"] = d.Revision
	}
	// The ontologies it acts on are used: the workflow runs that record the
	// run's uses of them start, if they haven't.
	var used []string
	switch a := a.(type) {
	case automation.CreateLoad:
		used = []string{a.OntologyID}
	case automation.PublishDashboard:
		used = st.uses.DashboardOntologies(ctx, a.DashboardID)
	}
	if err := st.uses.UseAll(ctx, used); err != nil {
		log.From(ctx).Warn("automation workflows: a use of an ontology went unrecorded", "step", nodeID, "err", err)
	}
	if err := t.db.StartFoundryAutomationNode(ctx, st.run.ID, t.workerID, nodeID, model.FoundryAutomationNodeExecuting); err != nil {
		if errors.Is(err, foundrystore.ErrConflict) {
			return 0, errors.New("the step had started already")
		}
		return 0, err
	}
	// The call runs to its end, or its timeout, even while the worker stops:
	// cut short, it would be unknown.
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(st.session.Ctx), actionTimeout)
	done := st.session.Service.RunApproved(callCtx, st.session.Identity, integration.FoundryOperationInput{
		Operation: a.Operation(), Target: a.Target(ws), Payload: payload})
	cancel()
	switch done := done.(type) {
	case integration.ExecutedOK:
		result, err := json.Marshal(done.Result.Result)
		if err != nil {
			return 0, t.fail(ctx, st, nodeID, "It ran, and its result can't be read.")
		}
		var kind model.FoundryAutomationJobKind
		switch a.(type) {
		case automation.SyncConnector:
			kind = model.FoundryAutomationJobSync
		case automation.CreateLoad:
			kind = model.FoundryAutomationJobLoad
		default:
			return 0, t.endWith(ctx, st, nodeID, automation.Output{Summary: "Published its dashboard's snapshot again.", Result: result}, 0)
		}
		return t.await(ctx, st, nodeID, model.FoundryAutomationJob{Kind: kind}, result)
	case integration.ExecutedUnknown:
		return 0, t.fail(ctx, st, nodeID, "It may or may not have run: Foundry lost its answer ("+agent.ToolErrorText(done.Err)+").")
	case integration.ExecutedFailed:
		return 0, t.fail(ctx, st, nodeID, sentence(agent.ToolErrorText(done.Err)))
	}
	return 0, t.fail(ctx, st, nodeID, "It ended in a way Foundry doesn't know.")
}

// await records that the action step started job, a sync or a load whose id
// is result's (as Foundry's tools return it), and waits for it: the step runs
// on, saying it waits, its job recorded with it and the work marked as the
// run's, in one write that holds even while the worker stops. An attempt that
// dies before it leaves the step executing, so the next fails it unknown;
// none starts the work again.
func (t *Task) await(ctx context.Context, st *runState, nodeID string, job model.FoundryAutomationJob, result json.RawMessage) (time.Duration, error) {
	var started struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(result, &started) != nil || started.ID == "" {
		return 0, t.fail(ctx, st, nodeID, "It started its "+string(job.Kind)+", and Foundry can't tell which one to wait for.")
	}
	job.ID = started.ID
	out := automation.Output{Summary: "Waiting for the " + string(job.Kind) + "…", Result: result}
	raw, err := json.Marshal(out)
	if err != nil {
		return 0, err
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if err := t.db.AwaitFoundryAutomationJob(writeCtx, st.run.ID, t.workerID, nodeID, job, raw); err != nil {
		if errors.Is(err, foundrystore.ErrConflict) {
			return 0, errors.New("the step was no longer executing")
		}
		return 0, err
	}
	st.jobs[nodeID] = job.ID
	st.states[nodeID] = automation.NodeState{Status: automation.NodeRunning, Output: out}
	return t.cfg.StepWait, nil
}

// notify emails the owner what the run did, where the host sends mail: the
// email (store foundry_notifications) is queued with the step's end, once.
// A standalone install sends none, and says so.
func (t *Task) notify(ctx context.Context, st *runState, nodeID string) error {
	if !t.email {
		return t.endWith(ctx, st, nodeID, automation.Output{Summary: "Not emailed: this install sends no email."}, 0)
	}
	raw, err := json.Marshal(automation.Output{Summary: "Emailed its owner."})
	if err != nil {
		return err
	}
	return t.end(ctx, st, nodeID, model.FoundryAutomationNodeEnd{Status: model.FoundryAutomationNodeSucceeded, Output: raw,
		Notification: &model.FoundryNotification{ID: id.New(), Kind: model.FoundryNotifyAutomationRun, SubjectID: id.New(), WorkspaceID: st.run.WorkspaceID,
			UserID: st.run.OwnerUserID, Status: model.FoundryNotificationPending}})
}

// yield hands the run back to the queue until its waiting step is read
// again, after pause.
func (t *Task) yield(ctx context.Context, run *model.FoundryAutomationRun, pause time.Duration) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if err := t.db.YieldFoundryAutomationRun(writeCtx, run.ID, t.workerID, t.now().Add(pause)); err != nil && !errors.Is(err, foundrystore.ErrNotFound) {
		log.From(ctx).Warn("hand an automation run back failed; its lease lapses and it is claimed again", "err", err)
	}
	runs.WithLabelValues("waiting").Inc()
}

// retry hands the run back after a failure of its own, not its steps', after
// a wait that doubles with its attempts.
func (t *Task) retry(ctx context.Context, run *model.FoundryAutomationRun, err error) {
	log.From(ctx).Warn("automation run attempt failed", "err", err)
	if run.Attempts >= t.cfg.MaxAttempts {
		t.finish(ctx, run, model.FoundryAutomationRunEnd{Status: model.FoundryAutomationRunFailed,
			Error: fmt.Sprintf("The run couldn't go on after %d attempts.", run.Attempts)})
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	at := t.now().Add(t.cfg.RetryBackoff << min(max(run.Attempts-1, 0), 8))
	if err := t.db.RetryFoundryAutomationRun(writeCtx, run.ID, t.workerID, at, "Foundry will try the run again."); err != nil &&
		!errors.Is(err, foundrystore.ErrNotFound) {
		log.From(ctx).Warn("requeue an automation run failed; its lease lapses and it is claimed again", "err", err)
	}
	runs.WithLabelValues("retried").Inc()
}

// finish ends the run, counting it toward its automation's failures in a
// row: past pause_after_failures it pauses, and its owner is emailed where
// the host sends mail. The workflow runs it started finish with its answer.
func (t *Task) finish(ctx context.Context, run *model.FoundryAutomationRun, end model.FoundryAutomationRunEnd) {
	end.PauseAfter, end.Backoff, end.Assess = t.cfg.PauseAfterFailures, t.cfg.FailureBackoff, t.assess
	if end.Answer == "" {
		// It ended before its steps: why is all it found.
		end.Answer = "The run failed: " + end.Error
	}
	if end.Status == model.FoundryAutomationRunFailed && !end.LostAccess && t.email {
		end.Notice = &model.FoundryNotification{ID: id.New(), Kind: model.FoundryNotifyAutomationPaused, SubjectID: id.New(), WorkspaceID: run.WorkspaceID,
			UserID: run.OwnerUserID, Status: model.FoundryNotificationPending}
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	err := t.db.FinishFoundryAutomationRun(writeCtx, run.ID, t.workerID, end)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		runs.WithLabelValues("lost").Inc()
	case err != nil:
		log.From(ctx).Warn("finish an automation run failed; its lease lapses and it is claimed again", "err", err)
	default:
		runs.WithLabelValues(string(end.Status)).Inc()
	}
}

// sentence is text as a sentence of its own: its first letter upper case,
// ending with a full stop unless it ends with other punctuation.
func sentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(text)
	text = strings.ToUpper(string(first)) + text[size:]
	if last, _ := utf8.DecodeLastRuneInString(text); !strings.ContainsRune(".!?", last) {
		text += "."
	}
	return text
}
