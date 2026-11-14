package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
}

func newAgentStore() *agentStore {
	return &agentStore{threads: map[string]*foundrymodel.FoundryAgentThread{}, turns: map[string]*foundrymodel.FoundryAgentTurn{},
		actions: map[string]*foundrymodel.FoundryAgentAction{}, events: map[string][]json.RawMessage{}}
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
	queued := *next
	queued.Status, queued.LastSeq = foundrymodel.FoundryAgentTurnQueued, 1
	s.turns[next.ID] = &queued
	s.events[next.ID] = []json.RawMessage{first}
	return nil
}

func (s *agentStore) RefreshFoundryAgentAction(_ context.Context, _, _, id, hash, confirmationID string, sealed []byte, keyID string, expires time.Time) (*foundrymodel.FoundryAgentAction, error) {
	a := s.actions[id]
	if a.PayloadHash != hash {
		return nil, foundrymodel.FoundryAgentRefusal{Reason: foundrymodel.FoundryAgentChanged, Detail: "changed"}
	}
	a.Status, a.ConfirmationID, a.TokenSealed, a.KeyID, a.ExpiresAt = foundrymodel.FoundryAgentActionPrepared, confirmationID, sealed, keyID, expires
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

// agentLoads makes loads for the actions people run, failing with err when set.
type agentLoads struct {
	integration.FoundryActionAPI
	err   error
	calls int
}

func (l *agentLoads) CreateFoundryLoad(context.Context, *foundryv1.CreateFoundryLoadRequest, ...grpc.CallOption) (*foundryv1.FoundryLoadResponse, error) {
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	return &foundryv1.FoundryLoadResponse{Load: &foundryv1.FoundryLoad{Id: "load-9"}}, nil
}

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

// agentHost opens every session as the person whose call it is.
type agentHost struct {
	service *integration.Service
	err     error
	actors  []ports.Actor
}

func (h *agentHost) Session(ctx context.Context, a ports.Actor, confirm integration.ConfirmMode) (integration.Session, error) {
	h.actors = append(h.actors, a)
	return integration.Session{Ctx: ctx, Service: h.service, Identity: &integration.Identity{
		User: true, HasPermission: func(integration.Permission) bool { return true },
		Authorize: func(context.Context, any, integration.Grant, ...string) error { return nil }, Confirm: confirm,
	}}, h.err
}

type agentFixture struct {
	s       *Service
	store   *agentStore
	loads   *agentLoads
	confirm *agentConfirmations
	host    *agentHost
}

func newAgentFixture(t *testing.T) agentFixture {
	t.Helper()
	cfg := config.Defaults()
	cfg.Foundry.Agent.Enabled = true
	store, loads, confirm := newAgentStore(), &agentLoads{}, &agentConfirmations{kept: map[string]integration.ConfirmationBinding{}}
	host := &agentHost{service: (&integration.Service{Actions: loads, Confirmations: confirm}).ForAgent()}
	s := &Service{deps: &runtimeDeps{Cfg: &cfg, DB: store, AI: hostedAI(t), ConnectorKeys: aiKeyring(t), AgentSessions: host}}
	return agentFixture{s: s, store: store, loads: loads, confirm: confirm, host: host}
}

// prepared stores an action of person's, with its confirmation kept by the host.
func (f agentFixture) prepared(t *testing.T, id string) *foundrymodel.FoundryAgentAction {
	t.Helper()
	sink := &capture{}
	_, err := f.host.service.PrepareAgain(context.Background(), &integration.Identity{User: true, HasPermission: func(integration.Permission) bool { return true },
		Authorize: func(context.Context, any, integration.Grant, ...string) error { return nil }},
		integration.FoundryOperationInput{Operation: "create_load", Target: "ontology:ws-1/ont-1", Payload: map[string]any{"version_id": "ver-1"}}, sink)
	require.NoError(t, err)
	sealed, keyID, err := f.s.deps.ConnectorKeys.Seal([]byte(sink.got.Confirmation.Token), id)
	require.NoError(t, err)
	f.store.threads["th-1"] = &foundrymodel.FoundryAgentThread{ID: "th-1", WorkspaceID: "ws-1", UserID: person.UserID}
	a := &foundrymodel.FoundryAgentAction{ID: id, TurnID: "turn-1", ThreadID: "th-1", WorkspaceID: "ws-1", UserID: person.UserID,
		Operation: "create_load", Target: "ontology:ws-1/ont-1", Payload: sink.got.Payload, PayloadHash: sink.got.PayloadHash,
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
			"no token":       func(r *foundryv1.SendFoundryAgentMessageRequest) { r.ClientToken = "" },
			"long token":     func(r *foundryv1.SendFoundryAgentMessageRequest) { r.ClientToken = strings.Repeat("c", 65) },
			"no text":        func(r *foundryv1.SendFoundryAgentMessageRequest) { r.Text = "   " },
			"long text":      func(r *foundryv1.SendFoundryAgentMessageRequest) { r.Text = strings.Repeat("é", maxAgentMessageChars+1) },
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
		assert.Equal(t, []ports.Actor{person}, f.host.actors, "the session is the clicker's, now")
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
