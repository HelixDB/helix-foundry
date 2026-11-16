package foundryautomationworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/automation"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/integration"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	ws    = "ws1"
	owner = "user-ada"
)

// memStore keeps one workspace's automations, runs and steps, and the
// Assistant turns the runs queue, with the store's rules a run's walk reads.
type memStore struct {
	automations map[string]*model.FoundryAutomation
	runs        map[string]*model.FoundryAutomationRun
	nodes       map[string]map[string]*model.FoundryAutomationRunNode
	turns       map[string]*model.FoundryAgentTurn
	events      map[string][]model.FoundryAgentEvent
	actions     []*model.FoundryAgentAction
	threads     []*model.FoundryAgentThread
	ontologies  map[string]*model.FoundryOntology
	dashboards  map[string]*model.FoundryDashboard
	due         []*model.FoundryAutomation
	watched     []*model.FoundryAutomation
	ended       []model.FoundryAutomationEnded
	fires       []model.FoundryAutomationFire
	touched     []string
	yielded     map[string]time.Time
	retried     map[string]string
	finished    map[string]model.FoundryAutomationRunEnd
	notices     []*model.FoundryNotification
	lost        bool
	// renews counts the lease's renewals; busy is a run's thread with a
	// live turn already.
	renews int
	busy   bool
	// pruned are the times prunes deleted before; full is how many more
	// prunes find a batch.
	pruned []time.Time
	full   int
	// uses are the workflow runs that record the runs' uses of ontologies.
	uses workflowUses
}

func (s *memStore) GetConnector(context.Context, string, string) (*model.Connector, error) {
	return nil, foundrystore.ErrNotFound
}

func (s *memStore) GetFoundryDocumentSet(context.Context, string, string) (*model.FoundryDocumentSet, error) {
	return nil, foundrystore.ErrNotFound
}

func (s *memStore) GetFoundryAutomationWorkflowRun(ctx context.Context, automationRunID, ontologyID string) (*model.FoundryWorkflowRun, error) {
	return s.uses.GetFoundryAutomationWorkflowRun(ctx, automationRunID, ontologyID)
}

func (s *memStore) StartFoundryAutomationWorkflowRun(ctx context.Context, start model.FoundryAutomationWorkflowStart) (*model.FoundryWorkflowRun, error) {
	if s.ontologies[start.OntologyID] == nil {
		return nil, foundrystore.ErrNotFound
	}
	return s.uses.StartFoundryAutomationWorkflowRun(ctx, start)
}

// workflowUses keeps the workflow runs that record automation runs' uses of
// ontologies, as the store starts them: one per run and ontology, none on a
// full ontology's (full) or one the workspace hasn't (gone).
type workflowUses struct {
	runs   []*model.FoundryWorkflowRun
	starts []model.FoundryAutomationWorkflowStart
	full   map[string]bool
	gone   map[string]bool
}

func (w *workflowUses) GetFoundryAutomationWorkflowRun(_ context.Context, automationRunID, ontologyID string) (*model.FoundryWorkflowRun, error) {
	for _, r := range w.runs {
		if *r.AutomationRunID == automationRunID && r.OntologyID == ontologyID {
			return r, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (w *workflowUses) StartFoundryAutomationWorkflowRun(ctx context.Context, start model.FoundryAutomationWorkflowStart) (*model.FoundryWorkflowRun, error) {
	switch {
	case w.full[start.OntologyID]:
		return nil, foundrystore.ErrLimitExceeded
	case w.gone[start.OntologyID]:
		return nil, foundrystore.ErrNotFound
	}
	if r, err := w.GetFoundryAutomationWorkflowRun(ctx, start.AutomationRunID, start.OntologyID); err == nil {
		return r, nil
	}
	w.starts = append(w.starts, start)
	runID, input := start.AutomationRunID, start.Input
	r := &model.FoundryWorkflowRun{ID: "wfrun-" + start.OntologyID, WorkflowID: "wf-" + start.OntologyID, WorkspaceID: start.WorkspaceID,
		OntologyID: start.OntologyID, ClientToken: runID, Input: &input, AutomationRunID: &runID}
	w.runs = append(w.runs, r)
	return r, nil
}

func newStore() *memStore {
	return &memStore{automations: map[string]*model.FoundryAutomation{}, runs: map[string]*model.FoundryAutomationRun{},
		nodes: map[string]map[string]*model.FoundryAutomationRunNode{}, turns: map[string]*model.FoundryAgentTurn{},
		events: map[string][]model.FoundryAgentEvent{}, ontologies: map[string]*model.FoundryOntology{}, dashboards: map[string]*model.FoundryDashboard{},
		yielded: map[string]time.Time{}, retried: map[string]string{}, finished: map[string]model.FoundryAutomationRunEnd{}}
}

func (s *memStore) DueFoundryAutomationSchedules(context.Context, int) ([]*model.FoundryAutomation, error) {
	return s.due, nil
}

func (s *memStore) WatchedFoundryAutomations(context.Context, time.Duration, int) ([]*model.FoundryAutomation, error) {
	return s.watched, nil
}

func (s *memStore) FindFoundryAutomationEnded(context.Context, *model.FoundryAutomation, time.Duration, int) ([]model.FoundryAutomationEnded, error) {
	return s.ended, nil
}

func (s *memStore) TouchFoundryAutomation(_ context.Context, id string) error {
	s.touched = append(s.touched, id)
	return nil
}

func (s *memStore) FireFoundryAutomation(_ context.Context, f model.FoundryAutomationFire, _ model.FoundryAutomationLimits) (model.FoundryAutomationFired, error) {
	s.fires = append(s.fires, f)
	if f.Run == nil {
		return model.FoundryAutomationFiredMoved, nil
	}
	return model.FoundryAutomationFiredQueued, nil
}

func (s *memStore) ClaimFoundryAutomationRun(context.Context, string, time.Duration) (*model.FoundryAutomationRun, error) {
	return nil, nil
}

func (s *memStore) RenewFoundryAutomationRunLease(context.Context, string, string, time.Duration) error {
	s.renews++
	if s.lost {
		return foundrystore.ErrNotFound
	}
	return nil
}

func (s *memStore) PruneFoundryAutomationRuns(_ context.Context, before time.Time) (int, error) {
	s.pruned = append(s.pruned, before)
	if s.full == 0 {
		return 0, nil
	}
	s.full--
	return 500, nil
}

func (s *memStore) ListFoundryAutomationRunNodes(_ context.Context, runID string) ([]*model.FoundryAutomationRunNode, error) {
	var out []*model.FoundryAutomationRunNode
	for _, n := range s.nodes[runID] {
		out = append(out, n)
	}
	return out, nil
}

func (s *memStore) GetFoundryAutomation(_ context.Context, workspaceID, id string) (*model.FoundryAutomation, error) {
	a, ok := s.automations[id]
	if !ok || a.WorkspaceID != workspaceID {
		return nil, foundrystore.ErrNotFound
	}
	return a, nil
}

func (s *memStore) node(runID, nodeID string) *model.FoundryAutomationRunNode {
	if s.nodes[runID] == nil {
		s.nodes[runID] = map[string]*model.FoundryAutomationRunNode{}
	}
	return s.nodes[runID][nodeID]
}

func (s *memStore) StartFoundryAutomationNode(_ context.Context, runID, _, nodeID string, status model.FoundryAutomationNodeStatus) error {
	if s.lost {
		return foundrystore.ErrNotFound
	}
	if s.node(runID, nodeID) != nil {
		return foundrystore.ErrConflict
	}
	s.nodes[runID][nodeID] = &model.FoundryAutomationRunNode{RunID: runID, NodeID: nodeID, Status: status}
	return nil
}

func (s *memStore) StartFoundryAutomationAssistant(_ context.Context, runID, _, nodeID string, thread *model.FoundryAgentThread, turn *model.FoundryAgentTurn) error {
	if s.node(runID, nodeID) != nil {
		return foundrystore.ErrConflict
	}
	if s.busy {
		return foundrystore.ErrThreadBusy
	}
	if thread != nil {
		s.threads = append(s.threads, thread)
		s.runs[runID].ThreadID = &thread.ID
	}
	queued := *turn
	queued.Status = model.FoundryAgentTurnQueued
	s.turns[turn.ID] = &queued
	s.nodes[runID][nodeID] = &model.FoundryAutomationRunNode{RunID: runID, NodeID: nodeID, Status: model.FoundryAutomationNodeRunning, TurnID: &queued.ID}
	return nil
}

func (s *memStore) EndFoundryAutomationNode(_ context.Context, runID, _, nodeID string, end model.FoundryAutomationNodeEnd) error {
	if err := end.Validate(); err != nil {
		return err
	}
	if s.lost {
		return foundrystore.ErrNotFound
	}
	n := s.node(runID, nodeID)
	if n != nil && n.Status.Ended() {
		return foundrystore.ErrConflict
	}
	if n == nil {
		n = &model.FoundryAutomationRunNode{RunID: runID, NodeID: nodeID}
		s.nodes[runID][nodeID] = n
	}
	n.Status, n.Output, n.USDMicros = end.Status, end.Output, end.USDMicros
	if end.Error != "" {
		n.Error = &end.Error
	}
	if end.Notification != nil {
		s.notices = append(s.notices, end.Notification)
		n.NotificationID = &end.Notification.SubjectID
	}
	s.runs[runID].USDMicros += end.USDMicros
	return nil
}

func (s *memStore) YieldFoundryAutomationRun(_ context.Context, id, _ string, at time.Time) error {
	s.yielded[id] = at
	return nil
}

func (s *memStore) RetryFoundryAutomationRun(_ context.Context, id, _ string, _ time.Time, why string) error {
	s.retried[id] = why
	return nil
}

func (s *memStore) FinishFoundryAutomationRun(_ context.Context, id, _ string, end model.FoundryAutomationRunEnd) error {
	if err := end.Validate(); err != nil {
		return err
	}
	s.finished[id] = end
	return nil
}

func (s *memStore) GetFoundryAgentTurn(_ context.Context, _, id string) (*model.FoundryAgentTurn, error) {
	t, ok := s.turns[id]
	if !ok {
		return nil, foundrystore.ErrNotFound
	}
	return t, nil
}

func (s *memStore) ListFoundryAgentEvents(_ context.Context, turnID string, afterSeq, limit int) ([]model.FoundryAgentEvent, error) {
	var out []model.FoundryAgentEvent
	for _, e := range s.events[turnID] {
		if e.Seq > afterSeq && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *memStore) ListFoundryAgentActions(context.Context, string) ([]*model.FoundryAgentAction, error) {
	return s.actions, nil
}

func (s *memStore) GetFoundryOntology(_ context.Context, _, id string) (*model.FoundryOntology, error) {
	o, ok := s.ontologies[id]
	if !ok {
		return nil, foundrystore.ErrNotFound
	}
	return o, nil
}

func (s *memStore) GetFoundryDashboard(_ context.Context, _, id string) (*model.FoundryDashboard, error) {
	d, ok := s.dashboards[id]
	if !ok {
		return nil, foundrystore.ErrNotFound
	}
	return d, nil
}

// endTurn ends the queued turn as status, with events.
func (s *memStore) endTurn(t *testing.T, status model.FoundryAgentTurnStatus, events ...agent.Event) *model.FoundryAgentTurn {
	t.Helper()
	require.Len(t, s.turns, 1)
	for _, turn := range s.turns {
		turn.Status, turn.Spend.USDMicros = status, 4200
		for i, e := range events {
			raw, err := agent.MarshalEvent(e)
			require.NoError(t, err)
			s.events[turn.ID] = append(s.events[turn.ID], model.FoundryAgentEvent{Seq: i + 1, Event: raw})
		}
		turn.LastSeq = len(events)
		return turn
	}
	return nil
}

// actions are Foundry's actions as the session runs them, recording each.
type actions struct {
	integration.FoundryActionAPI
	syncs, loads int
	loaded       string
	published    []int64
	err          error
}

func (a *actions) PublishFoundryDashboard(_ context.Context, r *foundryv1.PublishFoundryDashboardRequest, _ ...grpc.CallOption) (
	*foundryv1.FoundryDashboardResponse, error) {
	a.published = append(a.published, r.GetExpectedRevision())
	return &foundryv1.FoundryDashboardResponse{Dashboard: &foundryv1.FoundryDashboard{Id: r.GetId(), Revision: r.GetExpectedRevision()}}, nil
}

func (a *actions) SyncConnector(context.Context, *foundryv1.SyncConnectorRequest, ...grpc.CallOption) (*foundryv1.ConnectorSyncResponse, error) {
	a.syncs++
	if a.err != nil {
		return nil, a.err
	}
	return &foundryv1.ConnectorSyncResponse{Sync: &foundryv1.ConnectorSync{Id: "sync1"}}, nil
}

func (a *actions) CreateFoundryLoad(_ context.Context, r *foundryv1.CreateFoundryLoadRequest, _ ...grpc.CallOption) (*foundryv1.FoundryLoadResponse, error) {
	a.loads++
	a.loaded = r.GetVersionId()
	return &foundryv1.FoundryLoadResponse{Load: &foundryv1.FoundryLoad{Id: "load1"}}, nil
}

// confirmations prepare and consume as a host's do.
type confirmations struct {
	mu   sync.Mutex
	kept map[string]integration.ConfirmationBinding
}

func (c *confirmations) Prepare(_ context.Context, b integration.ConfirmationBinding) (integration.Confirmation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kept == nil {
		c.kept = map[string]integration.ConfirmationBinding{}
	}
	b.ID, b.Token = fmt.Sprintf("conf%d", len(c.kept)), fmt.Sprintf("tok%d", len(c.kept))
	c.kept[b.ID] = b
	return integration.Confirmation{ID: b.ID, Token: b.Token, ExpiresAt: time.Now().Add(5 * time.Minute)}, nil
}

func (c *confirmations) Consume(_ context.Context, b integration.ConfirmationBinding) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kept[b.ID] != b {
		return errors.New("mismatch")
	}
	delete(c.kept, b.ID)
	return nil
}

// sessions opens the owner's session, as one who may do anything, unless err.
type sessions struct {
	service *integration.Service
	actors  []ports.Actor
	err     error
}

func (h *sessions) Session(ctx context.Context, actor ports.Actor, confirm integration.ConfirmMode) (integration.Session, error) {
	h.actors = append(h.actors, actor)
	if h.err != nil {
		return integration.Session{}, h.err
	}
	return integration.Session{Ctx: ctx, Service: h.service, Identity: &integration.Identity{
		User: true, HasPermission: func(integration.Permission) bool { return true },
		Authorize: func(context.Context, any, integration.Grant, ...string) error { return nil },
		Allows:    func(string, string, integration.Grant) bool { return true },
		WithScope: func(ctx context.Context, _ string, _ bool) context.Context { return ctx },
		Confirm:   confirm,
	}}, nil
}

type fixture struct {
	task    *Task
	db      *memStore
	actions *actions
	host    *sessions
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	db, acts := newStore(), &actions{}
	host := &sessions{service: (&integration.Service{Actions: acts, Confirmations: &confirmations{}}).ForAgent()}
	cfg := config.Defaults().Foundry.Automations
	cfg.Enabled = true
	now := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	return fixture{task: &Task{cfg: cfg, db: db, sessions: host, assistant: true, email: true, workerID: "w1", now: func() time.Time { return now }},
		db: db, actions: acts, host: host}
}

func node(id, kind, config string) string {
	return fmt.Sprintf(`{"id":%q,"kind":%q,"config":%s,"position":{"x":0,"y":0}}`, id, kind, config)
}

func spec(edges string, nodes ...string) json.RawMessage {
	return json.RawMessage(`{"v":1,"nodes":[` + strings.Join(nodes, ",") + `],"edges":` + edges + `}`)
}

// weekly syncs Stripe, asks whether invoices are overdue, and emails the
// owner when they are.
var weekly = spec(`[{"from":"start","to":"sync"},{"from":"sync","to":"ask"},{"from":"ask","to":"late"},{"from":"late","to":"mail","branch":"true"}]`,
	node("start", "trigger", `{"kind":"schedule","every":"week","weekday":"monday","at":"09:00","time_zone":"Europe/London"}`),
	node("sync", "action", `{"operation":"sync_connector","connector_id":"stripe"}`),
	node("ask", "assistant", `{"instruction":"Are any invoices overdue?"}`),
	node("late", "condition", `{"check":"answer","equals":"yes"}`),
	node("mail", "notify", `{"message":"Overdue invoices"}`))

// queue stores automation "auto1" of raw and a run of it, its trigger done.
func (f fixture) queue(raw json.RawMessage) *model.FoundryAutomationRun {
	f.db.automations["auto1"] = &model.FoundryAutomation{ID: "auto1", WorkspaceID: ws, OwnerUserID: owner, Name: "Weekly invoices", Spec: raw}
	run := &model.FoundryAutomationRun{ID: "run1", AutomationID: "auto1", WorkspaceID: ws, OwnerUserID: owner, Spec: raw, Attempts: 1,
		Status: model.FoundryAutomationRunRunning, TriggerEvent: json.RawMessage(`{"kind":"schedule"}`)}
	f.db.runs[run.ID] = run
	f.db.nodes[run.ID] = map[string]*model.FoundryAutomationRunNode{"start": {RunID: run.ID, NodeID: "start", Status: model.FoundryAutomationNodeSucceeded}}
	return run
}

func (f fixture) status(runID, nodeID string) model.FoundryAutomationNodeStatus {
	n := f.db.nodes[runID][nodeID]
	if n == nil {
		return ""
	}
	return n.Status
}

func (f fixture) output(t *testing.T, runID, nodeID string) automation.Output {
	t.Helper()
	var out automation.Output
	require.NoError(t, json.Unmarshal(f.db.nodes[runID][nodeID].Output, &out))
	return out
}

func TestARunWalksItsStepsAsItsOwner(t *testing.T) {
	f := newFixture(t)
	run := f.queue(weekly)
	f.task.advance(context.Background(), run)

	assert.Equal(t, 1, f.actions.syncs, "the approved sync ran once")
	assert.Equal(t, model.FoundryAutomationNodeSucceeded, f.status(run.ID, "sync"))
	assert.Equal(t, "Started a sync of its connector.", f.output(t, run.ID, "sync").Summary)
	assert.Equal(t, model.FoundryAutomationNodeRunning, f.status(run.ID, "ask"), "the Assistant's turn is queued")
	require.Contains(t, f.db.yielded, run.ID, "the run waits for it, handed back")
	assert.Equal(t, []ports.Actor{{Source: ports.SourceUser, UserID: owner, WorkspaceID: ws}}, f.host.actors, "it acts as its owner")
	require.Len(t, f.db.threads, 1)
	assert.Equal(t, model.FoundryAgentThreadAutomation, f.db.threads[0].Kind)
	assert.Equal(t, "Weekly invoices", f.db.threads[0].Title)
	var turn *model.FoundryAgentTurn
	for _, queued := range f.db.turns {
		turn = queued
	}
	assert.Equal(t, model.FoundryAgentTurnAutomation, turn.Kind)
	assert.Equal(t, owner, turn.UserID)
	assert.Equal(t, f.task.cfg.MaxMicros(), turn.MaxUSDMicros)
	assert.True(t, strings.HasPrefix(turn.Input, "Are any invoices overdue?\n\n<workflow_context>"), turn.Input)

	// Still running, the run waits again.
	delete(f.db.yielded, run.ID)
	f.task.advance(context.Background(), run)
	require.Contains(t, f.db.yielded, run.ID)

	yes := true
	f.db.endTurn(t, model.FoundryAgentTurnDone, agent.Said{Text: "Looking."}, agent.Reported{Summary: "Two are overdue.", Answer: &yes})
	f.db.actions = []*model.FoundryAgentAction{{TurnID: turn.ID, Status: model.FoundryAgentActionPrepared}}
	f.task.advance(context.Background(), run)
	ask := f.output(t, run.ID, "ask")
	assert.Equal(t, "Two are overdue.", ask.Summary)
	assert.Equal(t, 1, ask.Pending, "an action it left for its owner")
	assert.Equal(t, int64(4200), f.db.nodes[run.ID]["ask"].USDMicros)
	assert.Equal(t, int64(4200), run.USDMicros)
	assert.True(t, *f.output(t, run.ID, "late").Branch)
	require.Len(t, f.db.notices, 1, "the owner is emailed")
	assert.Equal(t, model.FoundryNotifyAutomationRun, f.db.notices[0].Kind)
	assert.Equal(t, owner, f.db.notices[0].UserID)
	assert.Equal(t, model.FoundryAutomationRunSucceeded, f.db.finished[run.ID].Status)
	assert.Equal(t, 1, f.actions.syncs, "nothing ran twice")
}

func TestAConditionTakesItsBranch(t *testing.T) {
	f := newFixture(t)
	run := f.queue(weekly)
	f.task.advance(context.Background(), run)
	no := false
	f.db.endTurn(t, model.FoundryAgentTurnDone, agent.Reported{Summary: "None are overdue.", Answer: &no})
	f.task.advance(context.Background(), run)
	assert.False(t, *f.output(t, run.ID, "late").Branch)
	assert.Equal(t, model.FoundryAutomationNodeSkipped, f.status(run.ID, "mail"))
	assert.Empty(t, f.db.notices)
	assert.Equal(t, model.FoundryAutomationRunSucceeded, f.db.finished[run.ID].Status)

	// An answer the Assistant never gave fails the condition, and the run.
	f = newFixture(t)
	run = f.queue(weekly)
	f.task.advance(context.Background(), run)
	f.db.endTurn(t, model.FoundryAgentTurnDone, agent.Said{Text: "I looked."})
	f.task.advance(context.Background(), run)
	assert.Equal(t, "I looked.", f.output(t, run.ID, "ask").Summary, "with no report, its last words")
	assert.Equal(t, model.FoundryAutomationNodeFailed, f.status(run.ID, "late"))
	assert.Equal(t, model.FoundryAutomationNodeBlocked, f.status(run.ID, "mail"))
	end := f.db.finished[run.ID]
	assert.Equal(t, model.FoundryAutomationRunFailed, end.Status)
	assert.Equal(t, `The condition step "late" failed: Founder gave no yes-or-no answer.`, end.Error)
	require.NotNil(t, end.Notice, "its owner is told when it pauses")
	assert.Equal(t, model.FoundryNotifyAutomationPaused, end.Notice.Kind)
	assert.Equal(t, f.task.cfg.PauseAfterFailures, end.PauseAfter)
}

func TestAFailedTurnFailsItsStep(t *testing.T) {
	f := newFixture(t)
	run := f.queue(weekly)
	f.task.advance(context.Background(), run)
	turn := f.db.endTurn(t, model.FoundryAgentTurnFailed)
	why := "the workspace has spent this month's $20.00 of automations"
	turn.Error = &why
	f.task.advance(context.Background(), run)
	assert.Equal(t, "The workspace has spent this month's $20.00 of automations.", *f.db.nodes[run.ID]["ask"].Error)
	assert.Equal(t, model.FoundryAutomationRunFailed, f.db.finished[run.ID].Status)
}

func TestActionsResolveTheirPayloadWhenTheyRun(t *testing.T) {
	load := spec(`[{"from":"start","to":"load"}]`, node("start", "trigger", `{"kind":"manual"}`),
		node("load", "action", `{"operation":"create_load","ontology_id":"ont1"}`))
	f := newFixture(t)
	current := "ver7"
	f.db.ontologies["ont1"] = &model.FoundryOntology{ID: "ont1", WorkspaceID: ws, CurrentVersionID: &current}
	run := f.queue(load)
	f.task.advance(context.Background(), run)
	assert.Equal(t, "ver7", f.actions.loaded, "the version current when it runs")
	assert.Equal(t, model.FoundryAutomationRunSucceeded, f.db.finished[run.ID].Status)

	for name, setup := range map[string]func(f fixture){
		"no current version": func(f fixture) { f.db.ontologies["ont1"] = &model.FoundryOntology{ID: "ont1", WorkspaceID: ws} },
		"the ontology gone":  func(f fixture) {},
	} {
		f := newFixture(t)
		setup(f)
		run := f.queue(load)
		f.task.advance(context.Background(), run)
		assert.Zero(t, f.actions.loads, name)
		assert.Equal(t, model.FoundryAutomationNodeFailed, f.status(run.ID, "load"), name)
		assert.Equal(t, model.FoundryAutomationRunFailed, f.db.finished[run.ID].Status, name)
	}

	publish := spec(`[{"from":"start","to":"pub"}]`, node("start", "trigger", `{"kind":"manual"}`),
		node("pub", "action", `{"operation":"publish_dashboard","dashboard_id":"dash1"}`))
	f = newFixture(t)
	run = f.queue(publish)
	f.task.advance(context.Background(), run)
	assert.Equal(t, "Its dashboard is gone.", *f.db.nodes[run.ID]["pub"].Error)

	// A publish refreshes the snapshot a person published: never an edit made
	// since, which no one with the right to publish has seen.
	for share, want := range map[*model.FoundryDashboardShare]string{
		nil:               "isn't published",
		{SpecRevision: 3}: "was changed since it was published",
		{SpecRevision: 4}: "",
	} {
		f = newFixture(t)
		f.host.service = (&integration.Service{Actions: f.actions, Confirmations: &confirmations{}, PublicDashboards: true}).ForAgent()
		f.db.dashboards["dash1"] = &model.FoundryDashboard{ID: "dash1", WorkspaceID: ws, Revision: 4, Share: share}
		run = f.queue(publish)
		f.task.advance(context.Background(), run)
		if want != "" {
			assert.Contains(t, *f.db.nodes[run.ID]["pub"].Error, want)
			assert.Empty(t, f.actions.published, "nothing is published")
			continue
		}
		assert.Equal(t, []int64{4}, f.actions.published, "the revision at its link")
		assert.Equal(t, model.FoundryAutomationRunSucceeded, f.db.finished[run.ID].Status)
	}
}

// Every step starts with its lease renewed, so an attempt of many slow
// steps keeps its run; a lease lost to another worker stops it.
func TestEachStepRenewsTheLease(t *testing.T) {
	f := newFixture(t)
	run := f.queue(weekly)
	f.task.advance(context.Background(), run)
	assert.Equal(t, 2, f.db.renews, "before the sync, and before the Assistant step")
	assert.Equal(t, 1, f.actions.syncs)
}

// A card the run left waiting that its owner ran holds the thread: the
// next Assistant step waits for that turn, its attempt uncounted.
func TestAnAssistantStepWaitsForABusyThread(t *testing.T) {
	f := newFixture(t)
	run := f.queue(weekly)
	f.db.busy = true
	f.task.advance(context.Background(), run)
	require.Contains(t, f.db.yielded, run.ID, "it waits")
	assert.Empty(t, f.db.retried, "no failure")
	assert.NotContains(t, f.db.finished, run.ID)
	assert.Equal(t, model.FoundryAutomationNodeStatus(""), f.status(run.ID, "ask"), "not started")

	f.db.busy = false
	delete(f.db.yielded, run.ID)
	f.task.advance(context.Background(), run)
	assert.Equal(t, model.FoundryAutomationNodeRunning, f.status(run.ID, "ask"), "the thread idle, it starts")
	assert.Equal(t, 1, f.actions.syncs, "nothing ran twice")
}

// Runs that ended long ago are deleted a batch at a time; with none left,
// the worker waits before it looks again.
func TestRunsArePrunedByAge(t *testing.T) {
	f := newFixture(t)
	f.db.full = 2
	for range 4 {
		f.task.tick(context.Background())
	}
	now := f.task.now()
	require.Len(t, f.db.pruned, 3, "two full batches, then one that found none")
	assert.Equal(t, now.Add(-runRetention), f.db.pruned[0])
	f.task.now = func() time.Time { return now.Add(pruneWait) }
	f.task.tick(context.Background())
	assert.Len(t, f.db.pruned, 4, "it looks again after its wait")
}

func TestAnActionRunsAtMostOnce(t *testing.T) {
	f := newFixture(t)
	run := f.queue(weekly)
	// An attempt that died in its call left it executing.
	f.db.nodes[run.ID]["sync"] = &model.FoundryAutomationRunNode{RunID: run.ID, NodeID: "sync", Status: model.FoundryAutomationNodeExecuting}
	f.task.advance(context.Background(), run)
	assert.Zero(t, f.actions.syncs, "it isn't run again")
	assert.Contains(t, *f.db.nodes[run.ID]["sync"].Error, "may or may not have taken effect")
	assert.Equal(t, model.FoundryAutomationNodeBlocked, f.status(run.ID, "ask"))
	assert.Equal(t, model.FoundryAutomationRunFailed, f.db.finished[run.ID].Status)

	// A call that fails ends its step failed; one whose answer was lost, unknown.
	f = newFixture(t)
	run = f.queue(weekly)
	f.actions.err = status.Error(codes.FailedPrecondition, "a sync of this connector is already running")
	f.task.advance(context.Background(), run)
	assert.Equal(t, "A sync of this connector is already running.", *f.db.nodes[run.ID]["sync"].Error)
	f = newFixture(t)
	run = f.queue(weekly)
	f.actions.err = status.Error(codes.Unavailable, "gone")
	f.task.advance(context.Background(), run)
	assert.Contains(t, *f.db.nodes[run.ID]["sync"].Error, "may or may not have run")
}

func TestAnOwnerWhoLostAccessPausesIt(t *testing.T) {
	f := newFixture(t)
	f.host.err = status.Error(codes.PermissionDenied, "not a member")
	run := f.queue(weekly)
	f.task.advance(context.Background(), run)
	end := f.db.finished[run.ID]
	assert.True(t, end.LostAccess)
	assert.Nil(t, end.Notice, "no email to someone who left")
	assert.Zero(t, f.actions.syncs)

	f = newFixture(t)
	f.host.err = errors.New("the directory is down")
	run = f.queue(weekly)
	f.task.advance(context.Background(), run)
	assert.Contains(t, f.db.retried, run.ID, "a session that couldn't be read is tried again")
	assert.NotContains(t, f.db.finished, run.ID)
}

func TestRunsThatCantGoOnEnd(t *testing.T) {
	f := newFixture(t)
	run := f.queue(json.RawMessage(`{"v":9}`))
	f.task.advance(context.Background(), run)
	assert.Contains(t, f.db.finished[run.ID].Error, "spec can't be read")

	f = newFixture(t)
	run = f.queue(weekly)
	run.Attempts = f.task.cfg.MaxAttempts + 1
	f.task.advance(context.Background(), run)
	assert.Contains(t, f.db.finished[run.ID].Error, "stopped without finishing")

	f = newFixture(t)
	run = f.queue(weekly)
	run.Attempts = f.task.cfg.MaxAttempts
	f.db.automations = map[string]*model.FoundryAutomation{}
	f.task.advance(context.Background(), run)
	assert.Equal(t, model.FoundryAutomationRunFailed, f.db.finished[run.ID].Status, "attempts spent, a failure that isn't its steps' fails it")

	// A run cancelled meanwhile is lost: nothing more is recorded.
	f = newFixture(t)
	run = f.queue(weekly)
	f.db.lost = true
	f.task.advance(context.Background(), run)
	assert.NotContains(t, f.db.finished, run.ID)
	assert.NotContains(t, f.db.retried, run.ID)
}

func TestWithoutTheAssistantOrEmail(t *testing.T) {
	f := newFixture(t)
	f.task.assistant, f.task.email = false, false
	run := f.queue(spec(`[{"from":"start","to":"ask"},{"from":"start","to":"mail"}]`, node("start", "trigger", `{"kind":"manual"}`),
		node("ask", "assistant", `{"instruction":"Look"}`), node("mail", "notify", `{"message":""}`)))
	f.task.advance(context.Background(), run)
	assert.Equal(t, "Founder isn't available here.", *f.db.nodes[run.ID]["ask"].Error)
	assert.Equal(t, "Not emailed: this install sends no email.", f.output(t, run.ID, "mail").Summary)
	assert.Empty(t, f.db.notices)
	end := f.db.finished[run.ID]
	assert.Equal(t, model.FoundryAutomationRunFailed, end.Status)
	assert.Nil(t, end.Notice, "an install sends no email")
}

func TestSchedulesFire(t *testing.T) {
	f := newFixture(t)
	due := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	f.db.due = []*model.FoundryAutomation{{ID: "auto1", WorkspaceID: ws, OwnerUserID: owner, TriggerKind: "schedule", Spec: weekly, NextFireAt: &due},
		{ID: "broken", WorkspaceID: ws, Spec: json.RawMessage(`{}`), NextFireAt: &due}}
	f.task.fireSchedules(context.Background())
	require.Len(t, f.db.fires, 1, "an automation whose spec can't be read fires nothing")
	fire := f.db.fires[0]
	advance := fire.Advance.(model.FoundryScheduleAdvance)
	assert.Equal(t, due, advance.Due)
	assert.Equal(t, time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC), advance.Next.UTC(), "a week late, it fires once, next at its next time after now")
	assert.Equal(t, "schedule:2026-11-09T09:00:00Z", fire.Run.TriggerKey)
	assert.Equal(t, "start", fire.TriggerNodeID)
	assert.Contains(t, string(fire.TriggerOutput), "Its schedule fired at Mon 9 Nov 09:00 (Europe/London).")
}

func TestEventTriggersFireOnWhatEnded(t *testing.T) {
	cursor := model.FoundryAutomationCursor{At: time.Date(2026, 11, 16, 7, 0, 0, 0, time.UTC)}
	at := func(minutes int, id, outcome string, rows *int64) model.FoundryAutomationEnded {
		return model.FoundryAutomationEnded{Cursor: model.FoundryAutomationCursor{At: cursor.At.Add(time.Duration(minutes) * time.Minute), ID: id},
			Outcome: outcome, Rows: rows}
	}
	watched := func(trigger string) *model.FoundryAutomation {
		return &model.FoundryAutomation{ID: "auto1", WorkspaceID: ws, OwnerUserID: owner, Cursor: &cursor,
			Spec: spec(`[{"from":"start","to":"mail"}]`, node("start", "trigger", trigger), node("mail", "notify", `{"message":""}`))}
	}

	f := newFixture(t)
	f.db.watched = []*model.FoundryAutomation{watched(`{"kind":"sync_finished","connector_id":"stripe","outcome":"succeeded"}`)}
	f.task.watch(context.Background())
	assert.Equal(t, []string{"auto1"}, f.db.touched, "nothing ended: it only looked")

	rows := int64(42)
	f.db.ended = []model.FoundryAutomationEnded{at(1, "s1", "succeeded", nil), at(2, "s2", "succeeded", &rows), at(3, "s3", "failed", nil)}
	f.task.watch(context.Background())
	require.Len(t, f.db.fires, 1)
	fire := f.db.fires[0]
	assert.Equal(t, "s3", fire.Advance.(model.FoundryCursorAdvance).To.ID, "the cursor passes what didn't match too")
	assert.Equal(t, "sync_finished:s2", fire.Run.TriggerKey, "the newest that matched names it")
	var event model.FoundryAutomationEvent
	require.NoError(t, json.Unmarshal(fire.Run.TriggerEvent, &event))
	assert.Equal(t, "s2", event.SyncID)
	assert.Equal(t, int64(2), event.Count)
	assert.Contains(t, string(fire.TriggerOutput), `"count":42`, "a sync's count is the rows it wrote")

	f = newFixture(t)
	f.db.watched = []*model.FoundryAutomation{watched(`{"kind":"sync_finished","connector_id":"stripe","outcome":"failed"}`)}
	f.db.ended = []model.FoundryAutomationEnded{at(1, "s1", "succeeded", nil)}
	f.task.watch(context.Background())
	require.Len(t, f.db.fires, 1)
	assert.Nil(t, f.db.fires[0].Run, "nothing matched: it only moves on")

	f = newFixture(t)
	f.db.watched = []*model.FoundryAutomation{watched(`{"kind":"document_arrived","set_id":"set1"}`)}
	f.db.ended = []model.FoundryAutomationEnded{at(1, "d1", "uploaded", nil), at(2, "d2", "uploaded", nil)}
	f.task.watch(context.Background())
	require.NoError(t, json.Unmarshal(f.db.fires[0].Run.TriggerEvent, &event))
	assert.Equal(t, []string{"d1", "d2"}, event.DocumentIDs)
	assert.Contains(t, string(f.db.fires[0].TriggerOutput), "2 documents arrived in its set.")

	for trigger, outcome := range map[string]string{
		`{"kind":"load_ready","ontology_id":"ont1"}`:                          "partial",
		`{"kind":"generation_finished","ontology_id":"ont1","outcome":"any"}`: "failed",
	} {
		f = newFixture(t)
		f.db.watched = []*model.FoundryAutomation{watched(trigger)}
		f.db.ended = []model.FoundryAutomationEnded{at(1, "x1", outcome, nil)}
		f.task.watch(context.Background())
		require.NotNil(t, f.db.fires[0].Run, trigger)
	}
	f = newFixture(t)
	f.db.watched = []*model.FoundryAutomation{watched(`{"kind":"load_ready","ontology_id":"ont1"}`)}
	f.db.ended = []model.FoundryAutomationEnded{at(1, "x1", "failed", nil)}
	f.task.watch(context.Background())
	assert.Nil(t, f.db.fires[0].Run, "a failed load is no ready one")
}

func TestNewNeedsItsPartsWhileOn(t *testing.T) {
	cfg := config.Defaults()
	task, err := New(Deps{Cfg: &cfg})
	require.NoError(t, err, "off, it needs nothing")
	assert.Equal(t, Name, task.Name())
	cfg.Foundry.Automations.Enabled = true
	_, err = New(Deps{Cfg: &cfg})
	require.Error(t, err)
	cfg.Host.Local = true
	task, err = New(Deps{Cfg: &cfg, DB: struct{ foundrystore.Unimplemented }{}, Sessions: &sessions{}})
	require.NoError(t, err)
	assert.False(t, task.email, "an install sends no email")
}

func TestSentence(t *testing.T) {
	assert.Equal(t, "", sentence(" "))
	assert.Equal(t, "Gone.", sentence("gone"))
	assert.Equal(t, "Done?", sentence("done?"))
	assert.Equal(t, strings.Repeat("a", maxThreadTitle), title(strings.Repeat("a", maxThreadTitle+5)))
}

// A run's action steps use the ontologies they act on, recorded at their
// first use, and its end says what it found: the answer of the workflow
// runs it started, assessed where assessments are on.
func TestARunRecordsItsUsesOfOntologies(t *testing.T) {
	load := spec(`[{"from":"start","to":"load"},{"from":"load","to":"pub"}]`, node("start", "trigger", `{"kind":"manual"}`),
		node("load", "action", `{"operation":"create_load","ontology_id":"ont1"}`),
		node("pub", "action", `{"operation":"publish_dashboard","dashboard_id":"dash1"}`))
	f := newFixture(t)
	f.task.assess = true
	f.host.service = (&integration.Service{Actions: f.actions, Confirmations: &confirmations{}, PublicDashboards: true}).ForAgent()
	current := "ver7"
	f.db.ontologies["ont1"] = &model.FoundryOntology{ID: "ont1", WorkspaceID: ws, Name: "Sales", CurrentVersionID: &current}
	f.db.ontologies["ont2"] = &model.FoundryOntology{ID: "ont2", WorkspaceID: ws, Name: "Support"}
	tiles := `{"v":1,"tiles":[` +
		`{"id":"t1","title":"Deals","ontology_id":"ont1","load":{"kind":"current"},"query":{"start":{"type":"Deal"},"return":{"count":{}}}},` +
		`{"id":"t2","title":"Tickets","ontology_id":"ont2","load":{"kind":"current"},"query":{"start":{"type":"Ticket"},"return":{"count":{}}}}],` +
		`"layout":{"rows":[["t1","t2"]]}}`
	f.db.dashboards["dash1"] = &model.FoundryDashboard{ID: "dash1", WorkspaceID: ws, Name: "Pipeline", Revision: 4, Spec: json.RawMessage(tiles),
		Share: &model.FoundryDashboardShare{SpecRevision: 4}}
	run := f.queue(load)
	f.db.nodes[run.ID]["start"].Output = json.RawMessage(`{"summary":"Run by hand."}`)
	f.task.advance(context.Background(), run)

	require.Len(t, f.db.uses.starts, 2, "the load's ontology, and the published dashboard's other one")
	assert.Equal(t, "ont1", f.db.uses.starts[0].OntologyID)
	assert.Equal(t, "ont2", f.db.uses.starts[1].OntologyID)
	input := f.db.uses.starts[0].Input
	assert.Contains(t, input, `Run by hand. The workflow "Weekly invoices" ran these steps, in order:`)
	assert.Contains(t, input, `1. Action step "load": load the current version of the ontology "Sales".`)
	assert.Contains(t, input, `2. Action step "pub": publish the dashboard "Pipeline" again at its link.`)

	end := f.db.finished[run.ID]
	assert.Equal(t, model.FoundryAutomationRunSucceeded, end.Status)
	assert.True(t, end.Assess)
	assert.Equal(t, "The run succeeded.\nWhat its steps found:"+
		"\n- The action step \"load\" succeeded: Started a load of its ontology's current version."+
		"\n- The action step \"pub\" succeeded: Published its dashboard's snapshot again.", end.Answer)

	// A run that never touched an ontology starts none.
	f = newFixture(t)
	run = f.queue(spec(`[{"from":"start","to":"mail"}]`, node("start", "trigger", `{"kind":"manual"}`), node("mail", "notify", `{"message":"Hi"}`)))
	f.task.advance(context.Background(), run)
	assert.Empty(t, f.db.uses.starts)
	assert.False(t, f.db.finished[run.ID].Assess, "where assessments are off")
}

func TestARunsAnswerSaysWhatItsStepsFound(t *testing.T) {
	f := newFixture(t)
	run := f.queue(weekly)
	f.task.advance(context.Background(), run)
	yes, three := true, int64(3)
	f.db.endTurn(t, model.FoundryAgentTurnDone, agent.Reported{Summary: "Three are overdue.", Answer: &yes, Count: &three})
	f.task.advance(context.Background(), run)
	answer := f.db.finished[run.ID].Answer
	assert.Contains(t, answer, `- The assistant step "ask" succeeded: Three are overdue. (It answered yes.) (It counted 3.)`)
	assert.Contains(t, answer, `- The condition step "late" succeeded: It took its true branch.`)
	assert.Contains(t, answer, `- The notify step "mail" succeeded: Emailed its owner.`)

	// A failed run leads with why.
	f = newFixture(t)
	run = f.queue(weekly)
	f.task.advance(context.Background(), run)
	f.db.endTurn(t, model.FoundryAgentTurnDone, agent.Said{Text: "I looked."})
	f.task.advance(context.Background(), run)
	answer = f.db.finished[run.ID].Answer
	assert.True(t, strings.HasPrefix(answer, `The run failed: The condition step "late" failed: Founder gave no yes-or-no answer.`), answer)
	assert.Contains(t, answer, `- The notify step "mail" blocked`)

	// One that ended before its steps says only why.
	f = newFixture(t)
	run = f.queue(json.RawMessage(`{}`))
	f.task.advance(context.Background(), run)
	assert.True(t, strings.HasPrefix(f.db.finished[run.ID].Answer, "The run failed: "), f.db.finished[run.ID].Answer)
}
