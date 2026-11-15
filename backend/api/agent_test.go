package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryagent "github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/integration"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// agentStore keeps the Assistant's rows in memory. The MySQL tests hold the
// store's rules (one live turn, one click, expiry); this one answers with
// refusal when it is set.
type agentStore struct {
	foundrystore.Unimplemented
	mu       sync.Mutex
	threads  map[string]*foundrymodel.FoundryAgentThread
	turns    map[string]*foundrymodel.FoundryAgentTurn
	actions  map[string]*foundrymodel.FoundryAgentAction
	events   map[string][]json.RawMessage
	refusal  error
	finished []foundrymodel.FoundryAgentActionEnd
	// currentLoad is Sales graph's (ont-1).
	currentLoad string
}

func newAgentStore() *agentStore {
	return &agentStore{threads: map[string]*foundrymodel.FoundryAgentThread{}, turns: map[string]*foundrymodel.FoundryAgentTurn{},
		actions: map[string]*foundrymodel.FoundryAgentAction{}, events: map[string][]json.RawMessage{}, currentLoad: "load-1"}
}

// What a card names of ws-1: Sales graph (ont-1), its versions ver-<n> and
// loads, Stripe (c-1), the sets General (set-1) and Contracts (set-2), and
// contract.md (doc-1).
func (s *agentStore) GetFoundryOntology(_ context.Context, ws, id string) (*foundrymodel.FoundryOntology, error) {
	if ws != "ws-1" || id != "ont-1" {
		return nil, foundrystore.ErrNotFound
	}
	return &foundrymodel.FoundryOntology{ID: id, WorkspaceID: ws, Name: "Sales graph", CurrentLoadID: &s.currentLoad}, nil
}

func (s *agentStore) ListFoundryOntologies(ctx context.Context, ws, _ string, _ int) ([]*foundrymodel.FoundryOntology, string, error) {
	o, err := s.GetFoundryOntology(ctx, ws, "ont-1")
	if err != nil {
		return nil, "", nil
	}
	return []*foundrymodel.FoundryOntology{o}, "", nil
}

func (s *agentStore) GetOntologyVersion(_ context.Context, _, id string) (*foundrymodel.OntologyVersion, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "ver-"))
	if err != nil {
		return nil, foundrystore.ErrNotFound
	}
	return &foundrymodel.OntologyVersion{ID: id, Version: n}, nil
}

func (s *agentStore) GetFoundryLoad(_ context.Context, _, id string) (*foundrymodel.FoundryLoad, error) {
	return &foundrymodel.FoundryLoad{ID: id, VersionID: "ver-1", Version: 1, Status: foundrymodel.FoundryLoadReady,
		CreatedAt: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}, nil
}

var agentConnectors = []*foundrymodel.Connector{{ID: "c-1", WorkspaceID: "ws-1", Name: "Stripe", Kind: "stripe"}}

func (s *agentStore) GetConnector(_ context.Context, ws, id string) (*foundrymodel.Connector, error) {
	for _, c := range agentConnectors {
		if c.WorkspaceID == ws && c.ID == id {
			return c, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *agentStore) ListConnectors(context.Context, string) ([]*foundrymodel.Connector, error) {
	return agentConnectors, nil
}

func (s *agentStore) GetFoundryDocumentSet(_ context.Context, ws, id string) (*foundrymodel.FoundryDocumentSet, error) {
	name := map[string]string{"set-1": "General", "set-2": "Contracts"}[id]
	if name == "" {
		return nil, foundrystore.ErrNotFound
	}
	return &foundrymodel.FoundryDocumentSet{ID: id, WorkspaceID: ws, Name: name}, nil
}

func (s *agentStore) GetFoundryDocument(_ context.Context, ws, id string) (*foundrymodel.FoundryDocument, error) {
	if id != "doc-1" {
		return nil, foundrystore.ErrNotFound
	}
	return &foundrymodel.FoundryDocument{ID: id, WorkspaceID: ws, SetID: "set-1", Name: "contract.md"}, nil
}

func (s *agentStore) GetActiveFoundryAIConfig(context.Context, string) (*foundrymodel.FoundryAIConfig, error) {
	return nil, foundrystore.ErrNotFound
}

func (s *agentStore) CreateFoundryAgentTurn(_ context.Context, t *foundrymodel.FoundryAgentTurn, thread *foundrymodel.FoundryAgentThread, _ foundrymodel.FoundryAgentLimits) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refusal != nil {
		return s.refusal
	}
	if thread != nil {
		s.threads[thread.ID] = thread
	} else if th, ok := s.threads[t.ThreadID]; !ok || th.UserID != t.UserID {
		return foundrystore.ErrNotFound
	}
	stored := *t
	stored.Status = foundrymodel.FoundryAgentTurnQueued
	s.turns[t.ID] = &stored
	return nil
}

func (s *agentStore) GetFoundryAgentTurnByClientToken(_ context.Context, ws, userID, token string) (*foundrymodel.FoundryAgentTurn, error) {
	for _, t := range s.turns {
		if t.WorkspaceID == ws && t.UserID == userID && t.ClientToken != nil && *t.ClientToken == token {
			return t, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *agentStore) GetFoundryAgentTurn(_ context.Context, ws, id string) (*foundrymodel.FoundryAgentTurn, error) {
	if t, ok := s.turns[id]; ok && t.WorkspaceID == ws {
		return t, nil
	}
	return nil, foundrystore.ErrNotFound
}

func (s *agentStore) GetFoundryAgentThread(_ context.Context, ws, id string) (*foundrymodel.FoundryAgentThread, error) {
	if t, ok := s.threads[id]; ok && t.WorkspaceID == ws {
		return t, nil
	}
	return nil, foundrystore.ErrNotFound
}

func (s *agentStore) ListFoundryAgentTurns(_ context.Context, threadID string, _ int) ([]*foundrymodel.FoundryAgentTurn, error) {
	var out []*foundrymodel.FoundryAgentTurn
	for _, t := range s.turns {
		if t.ThreadID == threadID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *agentStore) ListFoundryAgentActions(_ context.Context, threadID string) ([]*foundrymodel.FoundryAgentAction, error) {
	var out []*foundrymodel.FoundryAgentAction
	for _, a := range s.actions {
		if a.ThreadID == threadID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *agentStore) ListFoundryAgentEvents(_ context.Context, turnID string, afterSeq, _ int) ([]foundrymodel.FoundryAgentEvent, error) {
	var out []foundrymodel.FoundryAgentEvent
	for i, e := range s.events[turnID] {
		if i+1 > afterSeq {
			out = append(out, foundrymodel.FoundryAgentEvent{Seq: i + 1, Event: e})
		}
	}
	return out, nil
}

func (s *agentStore) GetFoundryAgentAction(_ context.Context, ws, id string) (*foundrymodel.FoundryAgentAction, error) {
	if a, ok := s.actions[id]; ok && a.WorkspaceID == ws {
		return a, nil
	}
	return nil, foundrystore.ErrNotFound
}

func (s *agentStore) ClaimFoundryAgentAction(_ context.Context, ws, userID, id, hash string) (*foundrymodel.FoundryAgentAction, error) {
	if s.refusal != nil {
		return nil, s.refusal
	}
	a, ok := s.actions[id]
	switch {
	case !ok || a.WorkspaceID != ws || a.UserID != userID:
		return nil, foundrystore.ErrNotFound
	case a.Status != foundrymodel.FoundryAgentActionPrepared:
		return nil, foundrymodel.FoundryAgentRefusal{Reason: foundrymodel.FoundryAgentNotPrepared, Detail: "the action is " + string(a.Status)}
	case a.PayloadHash != hash:
		return nil, foundrymodel.FoundryAgentRefusal{Reason: foundrymodel.FoundryAgentChanged, Detail: "changed"}
	}
	a.Status = foundrymodel.FoundryAgentActionExecuting
	return a, nil
}

func (s *agentStore) FinishFoundryAgentAction(_ context.Context, id string, end foundrymodel.FoundryAgentActionEnd, next *foundrymodel.FoundryAgentTurn, first json.RawMessage) error {
	if err := end.Validate(); err != nil {
		return err
	}
	a := s.actions[id]
	a.Status, a.Result = end.Status, end.Result
	if end.Error != "" {
		a.Error = &end.Error
	}
	s.finished = append(s.finished, end)
	if next == nil {
		return nil
	}
	queued := *next
	queued.Status, queued.LastSeq = foundrymodel.FoundryAgentTurnQueued, 1
	s.turns[next.ID] = &queued
	s.events[next.ID] = []json.RawMessage{first}
	return nil
}

func (s *agentStore) RefreshFoundryAgentAction(_ context.Context, _, _, id, hash string, display json.RawMessage, confirmationID string, sealed []byte,
	keyID string, expires time.Time) (*foundrymodel.FoundryAgentAction, error) {
	a := s.actions[id]
	if a.PayloadHash != hash {
		return nil, foundrymodel.FoundryAgentRefusal{Reason: foundrymodel.FoundryAgentChanged, Detail: "changed"}
	}
	a.Status, a.Display, a.ConfirmationID, a.TokenSealed, a.KeyID, a.ExpiresAt = foundrymodel.FoundryAgentActionPrepared, display, confirmationID, sealed,
		keyID, expires
	return a, nil
}

func (s *agentStore) DismissFoundryAgentAction(_ context.Context, _, userID, id string) (*foundrymodel.FoundryAgentAction, error) {
	a, ok := s.actions[id]
	if !ok || a.UserID != userID {
		return nil, foundrystore.ErrNotFound
	}
	a.Status = foundrymodel.FoundryAgentActionDismissed
	return a, nil
}

// agentLoads runs the actions people run, failing with err when set: calls
// counts them, and ran names each one's backend call.
type agentLoads struct {
	integration.FoundryActionAPI
	err   error
	calls int
	ran   []string
}

// done records a call and answers it with out, or err.
func done[T any](l *agentLoads, method string, out T) (T, error) {
	l.calls++
	l.ran = append(l.ran, method)
	if l.err != nil {
		var none T
		return none, l.err
	}
	return out, nil
}

func (l *agentLoads) CreateFoundryLoad(context.Context, *foundryv1.CreateFoundryLoadRequest, ...grpc.CallOption) (*foundryv1.FoundryLoadResponse, error) {
	return done(l, "CreateFoundryLoad", &foundryv1.FoundryLoadResponse{Load: &foundryv1.FoundryLoad{Id: "load-9"}})
}

func (l *agentLoads) CreateFoundryOntology(context.Context, *foundryv1.CreateFoundryOntologyRequest, ...grpc.CallOption) (*foundryv1.FoundryOntologyResponse, error) {
	return done(l, "CreateFoundryOntology", &foundryv1.FoundryOntologyResponse{Ontology: &foundryv1.FoundryOntology{Id: "ont-2"}})
}

func (l *agentLoads) GenerateOntology(context.Context, *foundryv1.GenerateOntologyRequest, ...grpc.CallOption) (*foundryv1.OntologyGenerationResponse, error) {
	return done(l, "GenerateOntology", &foundryv1.OntologyGenerationResponse{Generation: &foundryv1.OntologyGeneration{Id: "gen-1"}})
}

func (l *agentLoads) SetCurrentOntologyVersion(context.Context, *foundryv1.SetCurrentOntologyVersionRequest, ...grpc.CallOption) (*foundryv1.OntologyVersionResponse, error) {
	return done(l, "SetCurrentOntologyVersion", &foundryv1.OntologyVersionResponse{Version: &foundryv1.OntologyVersion{Id: "ver-1"}})
}

func (l *agentLoads) SetCurrentFoundryLoad(context.Context, *foundryv1.SetCurrentFoundryLoadRequest, ...grpc.CallOption) (*foundryv1.FoundryLoadResponse, error) {
	return done(l, "SetCurrentFoundryLoad", &foundryv1.FoundryLoadResponse{Load: &foundryv1.FoundryLoad{Id: "load-1"}})
}

func (l *agentLoads) ReviseOntologyVersion(context.Context, *foundryv1.ReviseOntologyVersionRequest, ...grpc.CallOption) (*foundryv1.OntologyGenerationResponse, error) {
	return done(l, "ReviseOntologyVersion", &foundryv1.OntologyGenerationResponse{Generation: &foundryv1.OntologyGeneration{Id: "gen-2"}})
}

func (l *agentLoads) SyncConnector(context.Context, *foundryv1.SyncConnectorRequest, ...grpc.CallOption) (*foundryv1.ConnectorSyncResponse, error) {
	return done(l, "SyncConnector", &foundryv1.ConnectorSyncResponse{Sync: &foundryv1.ConnectorSync{Id: "sync-1"}})
}

func (l *agentLoads) CreateFoundryOntologyDocumentSet(context.Context, *foundryv1.CreateFoundryOntologyDocumentSetRequest, ...grpc.CallOption) (
	*foundryv1.FoundryDocumentSetResponse, error) {
	return done(l, "CreateFoundryOntologyDocumentSet", &foundryv1.FoundryDocumentSetResponse{Set: &foundryv1.FoundryDocumentSet{Id: "set-2"}})
}

func (l *agentLoads) UpdateFoundryOntologyDocumentSet(context.Context, *foundryv1.UpdateFoundryOntologyDocumentSetRequest, ...grpc.CallOption) (
	*foundryv1.FoundryDocumentSetResponse, error) {
	return done(l, "UpdateFoundryOntologyDocumentSet", &foundryv1.FoundryDocumentSetResponse{Set: &foundryv1.FoundryDocumentSet{Id: "set-1"}})
}

func (l *agentLoads) MoveFoundryOntologyDocument(context.Context, *foundryv1.MoveFoundryOntologyDocumentRequest, ...grpc.CallOption) (
	*foundryv1.FoundryDocumentReadsResponse, error) {
	return done(l, "MoveFoundryOntologyDocument", &foundryv1.FoundryDocumentReadsResponse{Document: &foundryv1.FoundryDocument{Id: "doc-1"}})
}

func (l *agentLoads) SettleFoundryReview(context.Context, *foundryv1.SettleFoundryReviewRequest, ...grpc.CallOption) (*foundryv1.FoundryReviewDecisionResponse, error) {
	return done(l, "SettleFoundryReview", &foundryv1.FoundryReviewDecisionResponse{Decision: &foundryv1.FoundryReviewDecision{Id: "dec-1", Kind: "release"}})
}

// agentReviews is the current load's reviews, as the person reads them.
type agentReviews struct {
	foundryv1.FoundryServiceClient
	reviews []*foundryv1.FoundryReview
}

func (r *agentReviews) ListFoundryReviews(_ context.Context, req *foundryv1.ListFoundryReviewsRequest, _ ...grpc.CallOption) (
	*foundryv1.ListFoundryReviewsResponse, error) {
	out := &foundryv1.ListFoundryReviewsResponse{LoadId: "load-1"}
	for _, review := range r.reviews {
		if slices.Contains(req.GetReviewKeys(), review.GetReviewKey()) {
			out.Reviews = append(out.Reviews, review)
		}
	}
	return out, nil
}

// agentDatabases holds t-1 in ws-1.
type agentDatabases struct{ ports.Databases }

func (agentDatabases) Get(_ context.Context, tenantID string) (ports.Database, error) {
	if tenantID != "t-1" {
		return ports.Database{}, foundrystore.ErrNotFound
	}
	return ports.Database{TenantID: tenantID, WorkspaceID: "ws-1", Name: "sales"}, nil
}

// agentDirectory holds every database in ws-1.
type agentDirectory struct{ integration.Directory }

func (agentDirectory) TenantWorkspace(context.Context, string) (string, error) { return "ws-1", nil }

// agentConfirmations spends a confirmation once, bound to its action.
type agentConfirmations struct {
	kept     map[string]integration.ConfirmationBinding
	prepared int
}

func (c *agentConfirmations) Prepare(_ context.Context, b integration.ConfirmationBinding) (integration.Confirmation, error) {
	c.prepared++
	b.ID, b.Token = "conf-new", "hlxc_new"
	c.kept[b.ID] = b
	return integration.Confirmation{ID: b.ID, Token: b.Token, ExpiresAt: time.Now().Add(5 * time.Minute)}, nil
}

func (c *agentConfirmations) Consume(_ context.Context, b integration.ConfirmationBinding) error {
	if c.kept[b.ID] != b {
		return status.Error(codes.FailedPrecondition, "confirmation does not match")
	}
	delete(c.kept, b.ID)
	return nil
}

// agentHost opens every session as the person whose call it is, who may do
// anything unless refuse says why not.
type agentHost struct {
	service *integration.Service
	err     error
	refuse  error
	actors  []ports.Actor
	// deadlines is how long each session had to run.
	deadlines []time.Duration
}

func (h *agentHost) Session(ctx context.Context, a ports.Actor, confirm integration.ConfirmMode) (integration.Session, error) {
	h.actors = append(h.actors, a)
	if d, ok := ctx.Deadline(); ok {
		h.deadlines = append(h.deadlines, time.Until(d))
	}
	return integration.Session{Ctx: ctx, Service: h.service, Identity: &integration.Identity{
		User: true, HasPermission: func(integration.Permission) bool { return true },
		Authorize: func(context.Context, any, integration.Grant, ...string) error { return h.refuse }, Confirm: confirm,
		WithScope: func(ctx context.Context, _ string, _ bool) context.Context { return ctx },
		Allows:    func(string, string, integration.Grant) bool { return true },
	}}, h.err
}

type agentFixture struct {
	s       *Service
	store   *agentStore
	loads   *agentLoads
	confirm *agentConfirmations
	host    *agentHost
	reviews *agentReviews
}

// agentStatement is the statement review r-1 holds back.
var agentStatement = strings.Repeat("ab", 32)

func newAgentFixture(t *testing.T) agentFixture {
	t.Helper()
	cfg := config.Defaults()
	cfg.Foundry.Agent.Enabled = true
	store, loads, confirm := newAgentStore(), &agentLoads{}, &agentConfirmations{kept: map[string]integration.ConfirmationBinding{}}
	reviews := &agentReviews{reviews: []*foundryv1.FoundryReview{{ReviewKey: "r-1", Reason: "low_confidence",
		Settle: foundryv1.FoundryReviewSettle_FOUNDRY_REVIEW_SETTLE_RELEASE, DocId: "doc-1", Link: "signed_with", StatementHash: agentStatement}}}
	host := &agentHost{service: (&integration.Service{Foundry: reviews, Actions: loads, Confirmations: confirm, Directory: agentDirectory{}}).ForAgent()}
	s := &Service{deps: &runtimeDeps{Cfg: &cfg, DB: store, AI: hostedAI(t), ConnectorKeys: aiKeyring(t), AgentSessions: host, Databases: agentDatabases{}}}
	return agentFixture{s: s, store: store, loads: loads, confirm: confirm, host: host, reviews: reviews}
}

// prepared stores a create_load of person's, with its confirmation kept by the host.
func (f agentFixture) prepared(t *testing.T, id string) *foundrymodel.FoundryAgentAction {
	t.Helper()
	return f.preparedAs(t, id, integration.FoundryOperationInput{Operation: "create_load", Target: "ontology:ws-1/ont-1",
		Payload: map[string]any{"version_id": "ver-1"}})
}

// preparedAs stores an action of person's for input.
func (f agentFixture) preparedAs(t *testing.T, id string, input integration.FoundryOperationInput) *foundrymodel.FoundryAgentAction {
	t.Helper()
	sink := &capture{}
	_, err := f.host.service.PrepareAgain(context.Background(), &integration.Identity{User: true, HasPermission: func(integration.Permission) bool { return true },
		Authorize: func(context.Context, any, integration.Grant, ...string) error { return nil }}, input, sink)
	require.NoError(t, err)
	sealed, keyID, err := f.s.deps.ConnectorKeys.Seal([]byte(sink.got.Confirmation.Token), id)
	require.NoError(t, err)
	f.store.threads["th-1"] = &foundrymodel.FoundryAgentThread{ID: "th-1", WorkspaceID: "ws-1", UserID: person.UserID}
	a := &foundrymodel.FoundryAgentAction{ID: id, TurnID: "turn-1", ThreadID: "th-1", WorkspaceID: "ws-1", UserID: person.UserID,
		Operation: input.Operation, Target: input.Target, Payload: sink.got.Payload, PayloadHash: sink.got.PayloadHash,
		ConfirmationID: sink.got.Confirmation.ID, TokenSealed: sealed, KeyID: keyID, Status: foundrymodel.FoundryAgentActionPrepared,
		ExpiresAt: sink.got.Confirmation.ExpiresAt}
	f.store.actions[id] = a
	return a
}

type capture struct{ got integration.PreparedAction }

func (c *capture) Allows(string) bool { return true }
func (c *capture) Record(_ context.Context, p integration.PreparedAction) (integration.AwaitingPerson, error) {
	c.got = p
	return integration.AwaitingPerson{ActionID: "x", ExpiresAt: p.Confirmation.ExpiresAt}, nil
}

var personCtx = ports.WithActor(context.Background(), person)

func TestTheAssistantAnswersPeopleOnlyAndOnlyWhenOn(t *testing.T) {
	f := newAgentFixture(t)
	req := &foundryv1.ListFoundryAgentThreadsRequest{WorkspaceId: "ws-1"}
	for _, a := range []ports.Actor{{Source: ports.SourceMCP, UserID: "u"}, {Source: ports.SourceGrantee, UserID: "u", WorkspaceID: "ws-1"},
		{Source: ports.SourceUser}, {Source: ports.SourceEventSource, EventSourceTokenID: "tok"}} {
		_, err := f.s.ListFoundryAgentThreads(ports.WithActor(context.Background(), a), req)
		assert.Equal(t, codes.PermissionDenied, status.Code(err), "%+v", a)
	}
	f.s.deps.Cfg.Foundry.Agent.Enabled = false
	_, err := f.s.ListFoundryAgentThreads(personCtx, req)
	assert.Equal(t, "FOUNDRY_AGENT_DISABLED", reasonOf(err))
}

func TestSendFoundryAgentMessage(t *testing.T) {
	send := func(f agentFixture, mutate func(*foundryv1.SendFoundryAgentMessageRequest)) (*foundryv1.FoundryAgentTurnResponse, error) {
		req := &foundryv1.SendFoundryAgentMessageRequest{WorkspaceId: "ws-1", Text: "  Load the sales graph\nplease ", ClientToken: "c1",
			PageContext: &foundryv1.FoundryAgentPageContext{OntologyId: "ont-1", LoadId: "load-1"}}
		if mutate != nil {
			mutate(req)
		}
		return f.s.SendFoundryAgentMessage(personCtx, req)
	}
	t.Run("a new thread, titled by the message", func(t *testing.T) {
		f := newAgentFixture(t)
		resp, err := send(f, nil)
		require.NoError(t, err)
		assert.Equal(t, "Load the sales graph", resp.GetThread().GetTitle())
		assert.Equal(t, "message", resp.GetTurn().GetKind())
		assert.Equal(t, "Load the sales graph\nplease", resp.GetTurn().GetText())
		assert.Equal(t, "load-1", resp.GetTurn().GetPageContext().GetLoadId())
		stored := f.store.turns[resp.GetTurn().GetId()]
		assert.Equal(t, person.UserID, stored.UserID)
		assert.EqualValues(t, 500_000, stored.MaxUSDMicros)
		// The same click is the same message.
		again, err := send(f, nil)
		require.NoError(t, err)
		assert.Equal(t, resp.GetTurn().GetId(), again.GetTurn().GetId())
		_, err = send(f, func(r *foundryv1.SendFoundryAgentMessageRequest) { r.Text = "something else" })
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})
	t.Run("malformed", func(t *testing.T) {
		f := newAgentFixture(t)
		for name, mutate := range map[string]func(*foundryv1.SendFoundryAgentMessageRequest){
			"no token":   func(r *foundryv1.SendFoundryAgentMessageRequest) { r.ClientToken = "" },
			"long token": func(r *foundryv1.SendFoundryAgentMessageRequest) { r.ClientToken = strings.Repeat("c", 65) },
			"no text":    func(r *foundryv1.SendFoundryAgentMessageRequest) { r.Text = "   " },
			"long text": func(r *foundryv1.SendFoundryAgentMessageRequest) {
				r.Text = strings.Repeat("é", maxAgentMessageChars+1)
			},
			"bad thread":     func(r *foundryv1.SendFoundryAgentMessageRequest) { r.ThreadId = "a/b" },
			"bad page":       func(r *foundryv1.SendFoundryAgentMessageRequest) { r.PageContext.OntologyId = "x y" },
			"load, no owner": func(r *foundryv1.SendFoundryAgentMessageRequest) { r.PageContext.OntologyId = "" },
		} {
			_, err := send(f, mutate)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), name)
		}
	})
	t.Run("another user's thread isn't found", func(t *testing.T) {
		f := newAgentFixture(t)
		f.store.threads["th-eve"] = &foundrymodel.FoundryAgentThread{ID: "th-eve", WorkspaceID: "ws-1", UserID: "user-eve"}
		_, err := send(f, func(r *foundryv1.SendFoundryAgentMessageRequest) { r.ThreadId = "th-eve" })
		assert.Equal(t, codes.NotFound, status.Code(err))
		_, err = f.s.GetFoundryAgentThread(personCtx, &foundryv1.GetFoundryAgentThreadRequest{WorkspaceId: "ws-1", Id: "th-eve"})
		assert.Equal(t, codes.NotFound, status.Code(err))
	})
	t.Run("refusals keep their reasons", func(t *testing.T) {
		for reason, want := range map[foundrymodel.FoundryAgentRefusalReason]string{
			foundrymodel.FoundryAgentThreadBusy: "FOUNDRY_AGENT_THREAD_BUSY", foundrymodel.FoundryAgentBudget: "FOUNDRY_AGENT_BUDGET",
		} {
			f := newAgentFixture(t)
			f.store.refusal = foundrymodel.FoundryAgentRefusal{Reason: reason, Detail: "no"}
			_, err := send(f, nil)
			assert.Equal(t, codes.FailedPrecondition, status.Code(err))
			assert.Equal(t, want, reasonOf(err))
		}
		f := newAgentFixture(t)
		f.store.refusal = foundrymodel.FoundryAgentRefusal{Reason: foundrymodel.FoundryAgentPerHour, Detail: "slow down"}
		_, err := send(f, nil)
		assert.Equal(t, codes.ResourceExhausted, status.Code(err))
	})
}

func TestExecuteFoundryAgentAction(t *testing.T) {
	execute := func(f agentFixture, ctx context.Context, id, hash string) (*foundryv1.FoundryAgentActionResponse, error) {
		return f.s.ExecuteFoundryAgentAction(ctx, &foundryv1.ExecuteFoundryAgentActionRequest{WorkspaceId: "ws-1", Id: id, PayloadHash: hash})
	}
	t.Run("runs once, as the clicker, and the agent goes on", func(t *testing.T) {
		f := newAgentFixture(t)
		a := f.prepared(t, "act-1")
		resp, err := execute(f, personCtx, "act-1", a.PayloadHash)
		require.NoError(t, err)
		assert.Equal(t, "succeeded", resp.GetAction().GetStatus())
		assert.Contains(t, resp.GetAction().GetResultJson(), "load-9")
		assert.Equal(t, 1, f.loads.calls)
		assert.Equal(t, []ports.Actor{{Source: ports.SourceUser, UserID: person.UserID, WorkspaceID: "ws-1"}}, f.host.actors,
			"the session is the clicker's, now, in the action's workspace")
		require.Len(t, f.host.deadlines, 1, "a click's run has a deadline")
		assert.Less(t, f.host.deadlines[0], foundrymodel.FoundryAgentExecutingFor, "it ends before a send reads it as unknown")
		next := resp.GetContinuation()
		assert.Equal(t, "action_result", next.GetKind())
		assert.Equal(t, "act-1", next.GetActionId())
		assert.Contains(t, string(f.store.events[next.GetId()][0]), `"type":"action_result"`)
		_, err = execute(f, personCtx, "act-1", a.PayloadHash)
		assert.Equal(t, "FOUNDRY_AGENT_ACTION_NOT_PREPARED", reasonOf(err), "a double click runs once")
		assert.Equal(t, 1, f.loads.calls)
	})
	t.Run("another user's action isn't found", func(t *testing.T) {
		f := newAgentFixture(t)
		a := f.prepared(t, "act-1")
		_, err := execute(f, ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceUser, UserID: "user-eve"}), "act-1", a.PayloadHash)
		assert.Equal(t, codes.NotFound, status.Code(err))
		assert.Zero(t, f.loads.calls)
	})
	t.Run("another payload is refused", func(t *testing.T) {
		f := newAgentFixture(t)
		f.prepared(t, "act-1")
		_, err := execute(f, personCtx, "act-1", "nothex")
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		_, err = execute(f, personCtx, "act-1", strings.Repeat("a", 64))
		assert.Equal(t, "FOUNDRY_AGENT_ACTION_CHANGED", reasonOf(err))
	})
	t.Run("an expired card asks for a refresh", func(t *testing.T) {
		f := newAgentFixture(t)
		a := f.prepared(t, "act-1")
		f.store.refusal = foundrymodel.FoundryAgentRefusal{Reason: foundrymodel.FoundryAgentExpired, Detail: "expired"}
		_, err := execute(f, personCtx, "act-1", a.PayloadHash)
		assert.Equal(t, "FOUNDRY_AGENT_ACTION_EXPIRED", reasonOf(err))
	})
	t.Run("a lost answer is unknown, a refusal failed", func(t *testing.T) {
		for want, backendErr := range map[string]error{"unknown": status.Error(codes.Unavailable, "gone"), "failed": status.Error(codes.FailedPrecondition, "over the cap")} {
			f := newAgentFixture(t)
			a := f.prepared(t, "act-1")
			f.loads.err = backendErr
			resp, err := execute(f, personCtx, "act-1", a.PayloadHash)
			require.NoError(t, err)
			assert.Equal(t, want, resp.GetAction().GetStatus())
			assert.NotEmpty(t, resp.GetAction().GetError())
			assert.NotNil(t, resp.GetContinuation(), "the agent learns how it ended")
		}
	})
	t.Run("a session that won't open runs nothing", func(t *testing.T) {
		f := newAgentFixture(t)
		a := f.prepared(t, "act-1")
		f.host.err = errors.New("hydrate failed")
		resp, err := execute(f, personCtx, "act-1", a.PayloadHash)
		require.NoError(t, err)
		assert.Equal(t, "failed", resp.GetAction().GetStatus())
		assert.Zero(t, f.loads.calls)
	})
	t.Run("a token sealed for another action doesn't open", func(t *testing.T) {
		f := newAgentFixture(t)
		a := f.prepared(t, "act-1")
		other := f.prepared(t, "act-2")
		a.TokenSealed, a.KeyID = other.TokenSealed, other.KeyID
		resp, err := execute(f, personCtx, "act-1", a.PayloadHash)
		require.NoError(t, err)
		assert.Equal(t, "failed", resp.GetAction().GetStatus())
		assert.Zero(t, f.loads.calls)
	})
	t.Run("a helper's thread goes on only while a helper runs it", func(t *testing.T) {
		f, hosts := withHosts(t)
		a := f.prepared(t, "act-1")
		f.store.threads["th-1"].Runtime = foundrymodel.FoundryAgentRuntimeCodex
		resp, err := execute(f, personCtx, "act-1", a.PayloadHash)
		require.NoError(t, err)
		assert.Equal(t, "succeeded", resp.GetAction().GetStatus(), "the click runs")
		assert.Nil(t, resp.GetContinuation(), "no helper runs Codex: a turn queued for none would only wait")

		b := f.prepared(t, "act-2")
		f.store.threads["th-1"].Runtime = foundrymodel.FoundryAgentRuntimeClaudeCode
		resp, err = execute(f, personCtx, "act-2", b.PayloadHash)
		require.NoError(t, err)
		assert.NotNil(t, resp.GetContinuation(), "Ada's Mac runs Claude Code")

		hosts.hosts = nil
		c := f.prepared(t, "act-3")
		f.store.threads["th-1"].Runtime = foundrymodel.FoundryAgentRuntimeClaudeCode
		resp, err = execute(f, personCtx, "act-3", c.PayloadHash)
		require.NoError(t, err)
		assert.Nil(t, resp.GetContinuation(), "a helper just revoked runs nothing more")
	})
	t.Run("a helper's thread on a host without helpers ends with its click", func(t *testing.T) {
		f := newAgentFixture(t)
		a := f.prepared(t, "act-1")
		f.store.threads["th-1"].Runtime = foundrymodel.FoundryAgentRuntimeClaudeCode
		resp, err := execute(f, personCtx, "act-1", a.PayloadHash)
		require.NoError(t, err)
		assert.Nil(t, resp.GetContinuation())
	})
	t.Run("a host without sessions refuses", func(t *testing.T) {
		f := newAgentFixture(t)
		a := f.prepared(t, "act-1")
		f.s.deps.AgentSessions = nil
		_, err := execute(f, personCtx, "act-1", a.PayloadHash)
		assert.Equal(t, codes.Unavailable, status.Code(err))
	})
}

func TestRefreshAndDismissFoundryAgentAction(t *testing.T) {
	f := newAgentFixture(t)
	a := f.prepared(t, "act-1")
	a.Status = foundrymodel.FoundryAgentActionExpired
	before := f.confirm.prepared
	resp, err := f.s.RefreshFoundryAgentAction(personCtx, &foundryv1.RefreshFoundryAgentActionRequest{WorkspaceId: "ws-1", Id: "act-1"})
	require.NoError(t, err)
	assert.Equal(t, "prepared", resp.GetAction().GetStatus())
	assert.Equal(t, before+1, f.confirm.prepared, "no model: the same payload, confirmed again")
	token, err := f.s.deps.ConnectorKeys.Open(a.TokenSealed, a.KeyID, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "hlxc_new", string(token))
	_, err = f.s.RefreshFoundryAgentAction(ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceUser, UserID: "user-eve"}),
		&foundryv1.RefreshFoundryAgentActionRequest{WorkspaceId: "ws-1", Id: "act-1"})
	assert.Equal(t, codes.NotFound, status.Code(err))

	dismissed, err := f.s.DismissFoundryAgentAction(personCtx, &foundryv1.DismissFoundryAgentActionRequest{WorkspaceId: "ws-1", Id: "act-1"})
	require.NoError(t, err)
	assert.Equal(t, "dismissed", dismissed.GetAction().GetStatus())
	assert.Empty(t, f.store.finished, "dismissing starts no turn")
}

// A refresh draws the card again as its clicker reads the workspace now,
// in the same write as its new confirmation; what its checks refuse is
// refused, and leaves the action as it was.
func TestRefreshDrawsTheCardAgain(t *testing.T) {
	refresh := func(f agentFixture) (*foundryv1.FoundryAgentActionResponse, error) {
		return f.s.RefreshFoundryAgentAction(personCtx, &foundryv1.RefreshFoundryAgentActionRequest{WorkspaceId: "ws-1", Id: "act-1"})
	}
	t.Run("the current load named as it is now", func(t *testing.T) {
		f := newAgentFixture(t)
		a := f.preparedAs(t, "act-1", integration.FoundryOperationInput{Operation: "set_current_load", Target: "ontology:ws-1/ont-1",
			Payload: map[string]any{"load_id": "load-2"}})
		a.Status, a.Display = foundrymodel.FoundryAgentActionExpired, json.RawMessage(`{"ontology":{"id":"ont-1"},"load":{"id":"load-2"},"current":{"id":"load-1"}}`)
		f.store.currentLoad = "load-7"
		resp, err := refresh(f)
		require.NoError(t, err)
		var d foundryagent.SetCurrentLoadDisplay
		require.NoError(t, json.Unmarshal([]byte(resp.GetAction().GetDisplayJson()), &d))
		require.NotNil(t, d.Current)
		assert.Equal(t, "load-7", d.Current.ID, "in place of the load current now")
		assert.Equal(t, foundryagent.Ref{ID: "ont-1", Name: "Sales graph"}, d.Ontology)
	})
	t.Run("a review the current load no longer holds", func(t *testing.T) {
		f := newAgentFixture(t)
		a := f.preparedAs(t, "act-1", integration.FoundryOperationInput{Operation: "settle_review", Target: "ontology:ws-1/ont-1",
			Payload: map[string]any{"review_id": "r-1", "decision": map[string]any{"release": map[string]any{"statement_hash": agentStatement}}}})
		a.Status = foundrymodel.FoundryAgentActionExpired
		before := *a
		f.reviews.reviews = nil
		_, err := refresh(f)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Contains(t, status.Convert(err).Message(), "holds no review r-1")
		assert.Equal(t, before, *a, "nothing of it changes")
	})
	t.Run("a statement that reads differently now", func(t *testing.T) {
		f := newAgentFixture(t)
		a := f.preparedAs(t, "act-1", integration.FoundryOperationInput{Operation: "settle_review", Target: "ontology:ws-1/ont-1",
			Payload: map[string]any{"review_id": "r-1", "decision": map[string]any{"release": map[string]any{"statement_hash": agentStatement}}}})
		a.Status = foundrymodel.FoundryAgentActionExpired
		f.reviews.reviews[0].StatementHash = strings.Repeat("cd", 32)
		_, err := refresh(f)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Contains(t, status.Convert(err).Message(), "reads differently now")
	})
	t.Run("an ontology the workspace no longer holds", func(t *testing.T) {
		f := newAgentFixture(t)
		a := f.preparedAs(t, "act-1", integration.FoundryOperationInput{Operation: "create_load", Target: "ontology:ws-1/ont-9",
			Payload: map[string]any{"version_id": "ver-1"}})
		a.Status = foundrymodel.FoundryAgentActionExpired
		_, err := refresh(f)
		assert.Equal(t, codes.FailedPrecondition, status.Code(err))
		assert.Contains(t, status.Convert(err).Message(), "holds no ontology ont-9")
	})
}

// Every operation the Assistant prepares runs from its person's click
// through its own backend call, once, and its card refreshes without a
// model. What the person may no longer do fails, running nothing.
func TestEveryAssistantOperationRunsFromAClick(t *testing.T) {
	hash := agentStatement
	for _, c := range []struct {
		input  integration.FoundryOperationInput
		method string
	}{
		{integration.FoundryOperationInput{Operation: "create_ontology", Target: "tenant:t-1", Payload: map[string]any{"name": "Renewals"}}, "CreateFoundryOntology"},
		{integration.FoundryOperationInput{Operation: "generate_ontology", Target: "ontology:ws-1/ont-1", Payload: map[string]any{"goal": "Renewals"}}, "GenerateOntology"},
		{integration.FoundryOperationInput{Operation: "set_current_version", Target: "ontology:ws-1/ont-1",
			Payload: map[string]any{"version_id": "ver-2", "expected_current_version_id": "ver-1"}}, "SetCurrentOntologyVersion"},
		{integration.FoundryOperationInput{Operation: "create_load", Target: "ontology:ws-1/ont-1", Payload: map[string]any{"version_id": "ver-1"}}, "CreateFoundryLoad"},
		{integration.FoundryOperationInput{Operation: "set_current_load", Target: "ontology:ws-1/ont-1", Payload: map[string]any{"load_id": "load-1"}}, "SetCurrentFoundryLoad"},
		{integration.FoundryOperationInput{Operation: "revise_version", Target: "ontology:ws-1/ont-1", Payload: map[string]any{"version_id": "ver-1",
			"edits": []any{map[string]any{"remove": map[string]any{"item_id": "rule.old"}}}}}, "ReviseOntologyVersion"},
		{integration.FoundryOperationInput{Operation: "sync_connector", Target: "connector:ws-1/c-1", Payload: map[string]any{}}, "SyncConnector"},
		{integration.FoundryOperationInput{Operation: "create_document_set", Target: "ontology:ws-1/ont-1", Payload: map[string]any{"name": "Leases"}},
			"CreateFoundryOntologyDocumentSet"},
		{integration.FoundryOperationInput{Operation: "update_document_set", Target: "ontology:ws-1/ont-1", Payload: map[string]any{"set_id": "set-1", "name": "Signed"}},
			"UpdateFoundryOntologyDocumentSet"},
		{integration.FoundryOperationInput{Operation: "move_document", Target: "ontology:ws-1/ont-1", Payload: map[string]any{"document_id": "doc-1", "set_id": "set-2"}},
			"MoveFoundryOntologyDocument"},
		{integration.FoundryOperationInput{Operation: "settle_review", Target: "ontology:ws-1/ont-1", Payload: map[string]any{"review_id": "r-1",
			"decision": map[string]any{"release": map[string]any{"statement_hash": hash}}}}, "SettleFoundryReview"},
	} {
		t.Run(c.input.Operation, func(t *testing.T) {
			f := newAgentFixture(t)
			a := f.preparedAs(t, "act-1", c.input)
			a.Status = foundrymodel.FoundryAgentActionExpired
			refreshed, err := f.s.RefreshFoundryAgentAction(personCtx, &foundryv1.RefreshFoundryAgentActionRequest{WorkspaceId: "ws-1", Id: "act-1"})
			require.NoError(t, err)
			assert.Equal(t, "prepared", refreshed.GetAction().GetStatus())
			assert.True(t, json.Valid([]byte(refreshed.GetAction().GetDisplayJson())) && refreshed.GetAction().GetDisplayJson() != "",
				"its card is drawn again")
			a.Status = foundrymodel.FoundryAgentActionPrepared
			resp, err := f.s.ExecuteFoundryAgentAction(personCtx, &foundryv1.ExecuteFoundryAgentActionRequest{WorkspaceId: "ws-1", Id: "act-1", PayloadHash: a.PayloadHash})
			require.NoError(t, err)
			assert.Equal(t, "succeeded", resp.GetAction().GetStatus(), resp.GetAction().GetError())
			assert.Equal(t, []string{c.method}, f.loads.ran)
			assert.Equal(t, "action_result", resp.GetContinuation().GetKind(), "the agent goes on from it")
		})
	}
	t.Run("what the person may no longer do", func(t *testing.T) {
		f := newAgentFixture(t)
		a := f.preparedAs(t, "act-1", integration.FoundryOperationInput{Operation: "sync_connector", Target: "connector:ws-1/c-1", Payload: map[string]any{}})
		f.host.refuse = status.Error(codes.PermissionDenied, "not an admin")
		resp, err := f.s.ExecuteFoundryAgentAction(personCtx, &foundryv1.ExecuteFoundryAgentActionRequest{WorkspaceId: "ws-1", Id: "act-1", PayloadHash: a.PayloadHash})
		require.NoError(t, err)
		assert.Equal(t, "failed", resp.GetAction().GetStatus())
		assert.Zero(t, f.loads.calls)
		_, err = f.s.RefreshFoundryAgentAction(personCtx, &foundryv1.RefreshFoundryAgentActionRequest{WorkspaceId: "ws-1", Id: "act-1"})
		assert.Equal(t, codes.FailedPrecondition, status.Code(err), "nor is it prepared again")
	})
}
