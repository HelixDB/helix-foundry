package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryagent "github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/integration"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// The Assistant's routes (foundry.agent). Every thread, turn and action is its
// user's own: another user's answers NotFound, as one that doesn't exist.

const (
	// maxAgentMessageChars bounds a message's text, in characters.
	maxAgentMessageChars = 4000
	// agentThreadPage is how many threads a list returns at once.
	agentThreadPage = 50
	// agentThreadTurns is how many of its newest turns a thread returns.
	agentThreadTurns = 100
	// agentEventsPage is how many events one poll returns.
	agentEventsPage = 200
)

// agentIDPattern is an id a page context may name.
var agentIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// agentUser is the user the call is for: a person signed in, never a key, an
// agent or a grantee (whose methods' rules deny every Assistant route). Off,
// the Assistant is FAILED_PRECONDITION, reason FOUNDRY_AGENT_DISABLED.
func (s *Service) agentUser(ctx context.Context, workspaceID string) (string, error) {
	if !s.deps.Cfg.Foundry.Agent.Enabled {
		return "", foundryPrecondition(workspaceID, "FOUNDRY_AGENT_DISABLED", "the Assistant isn't available here")
	}
	a := actor(ctx)
	if a.Source != ports.SourceUser || a.UserID == "" || a.ServiceCredentialID != "" {
		return "", status.Error(codes.PermissionDenied, "the Assistant answers people signed in to Foundry")
	}
	return a.UserID, nil
}

// agentRefusal is a refusal of the Assistant's rules as the API answers it;
// false when err is none.
func agentRefusal(workspaceID string, err error) (error, bool) {
	var r foundrymodel.FoundryAgentRefusal
	if !errors.As(err, &r) {
		return nil, false
	}
	reason := map[foundrymodel.FoundryAgentRefusalReason]string{
		foundrymodel.FoundryAgentThreadBusy:   "FOUNDRY_AGENT_THREAD_BUSY",
		foundrymodel.FoundryAgentBudget:       "FOUNDRY_AGENT_BUDGET",
		foundrymodel.FoundryAgentTooManyCards: "FOUNDRY_AGENT_TOO_MANY_ACTIONS",
		foundrymodel.FoundryAgentNotPrepared:  "FOUNDRY_AGENT_ACTION_NOT_PREPARED",
		foundrymodel.FoundryAgentChanged:      "FOUNDRY_AGENT_ACTION_CHANGED",
		foundrymodel.FoundryAgentExpired:      "FOUNDRY_AGENT_ACTION_EXPIRED",
	}[r.Reason]
	if r.Reason == foundrymodel.FoundryAgentPerHour {
		return status.Error(codes.ResourceExhausted, r.Detail), true
	}
	return foundryPrecondition(workspaceID, reason, r.Detail), true
}

// SendFoundryAgentMessage queues a person's message in their thread, or a new
// one. The same client token returns the turn it queued.
func (s *Service) SendFoundryAgentMessage(ctx context.Context, req *foundryv1.SendFoundryAgentMessageRequest) (*foundryv1.FoundryAgentTurnResponse, error) {
	ws := req.GetWorkspaceId()
	userID, err := s.agentUser(ctx, ws)
	if err != nil {
		return nil, err
	}
	text, token := strings.TrimSpace(req.GetText()), req.GetClientToken()
	page := foundryagent.PageContext{OntologyID: req.GetPageContext().GetOntologyId(), LoadID: req.GetPageContext().GetLoadId()}
	switch {
	case token == "" || len(token) > maxClientTokenLength:
		return nil, status.Errorf(codes.InvalidArgument, "client_token is required, at most %d characters", maxClientTokenLength)
	case text == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > maxAgentMessageChars:
		return nil, status.Errorf(codes.InvalidArgument, "text is required, at most %d characters", maxAgentMessageChars)
	case req.GetThreadId() != "" && !agentIDPattern.MatchString(req.GetThreadId()):
		return nil, status.Error(codes.InvalidArgument, "thread_id is malformed")
	case page.OntologyID != "" && !agentIDPattern.MatchString(page.OntologyID), page.LoadID != "" && !agentIDPattern.MatchString(page.LoadID),
		page.LoadID != "" && page.OntologyID == "":
		return nil, status.Error(codes.InvalidArgument, "page_context names a load of an ontology, by their ids")
	}
	if err := s.requireAI(ctx, ws); err != nil {
		return nil, err
	}
	existing, err := s.deps.DB.GetFoundryAgentTurnByClientToken(ctx, ws, userID, token)
	switch {
	case err == nil && existing.Input != text:
		return nil, status.Error(codes.InvalidArgument, "client_token was already used for another message")
	case err == nil:
		return s.agentTurnResponse(ctx, existing)
	case !errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Errorf(codes.Internal, "look up the message: %v", err)
	}
	t := &foundrymodel.FoundryAgentTurn{
		ID: id.New(), WorkspaceID: ws, UserID: userID, Kind: foundrymodel.FoundryAgentTurnMessage, Input: text, ClientToken: &token,
		MaxUSDMicros: s.deps.Cfg.Foundry.Agent.MaxMicros(),
	}
	if page.OntologyID != "" {
		t.PageContext, _ = json.Marshal(page) // strings only: it can't fail
	}
	var thread *foundrymodel.FoundryAgentThread
	if t.ThreadID = req.GetThreadId(); t.ThreadID == "" {
		t.ThreadID = id.New()
		thread = &foundrymodel.FoundryAgentThread{ID: t.ThreadID, WorkspaceID: ws, UserID: userID, Title: foundryagent.Title(text),
			Runtime: foundrymodel.FoundryAgentRuntimeAPI}
	}
	err = s.deps.DB.CreateFoundryAgentTurn(ctx, t, thread, s.agentLimits())
	if refused, ok := agentRefusal(ws, err); ok {
		return nil, refused
	}
	switch {
	case errors.Is(err, foundrystore.ErrConflict):
		// The same click again won the race.
		raced, lookupErr := s.deps.DB.GetFoundryAgentTurnByClientToken(ctx, ws, userID, token)
		if lookupErr != nil || raced.Input != text {
			return nil, status.Error(codes.InvalidArgument, "client_token was already used for another message")
		}
		return s.agentTurnResponse(ctx, raced)
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "thread not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "queue the message: %v", err)
	}
	stored, err := s.deps.DB.GetFoundryAgentTurn(ctx, ws, t.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the message: %v", err)
	}
	return s.agentTurnResponse(ctx, stored)
}

// agentLimits are foundry.agent's limits this month.
func (s *Service) agentLimits() foundrymodel.FoundryAgentLimits {
	a := s.deps.Cfg.Foundry.Agent
	now := time.Now().UTC()
	return foundrymodel.FoundryAgentLimits{
		Since: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), CapMicros: a.MonthlyMicros(), PerHour: a.PerHour, Running: a.MaxRunning,
	}
}

func (s *Service) agentTurnResponse(ctx context.Context, t *foundrymodel.FoundryAgentTurn) (*foundryv1.FoundryAgentTurnResponse, error) {
	thread, err := s.deps.DB.GetFoundryAgentThread(ctx, t.WorkspaceID, t.ThreadID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the thread: %v", err)
	}
	return &foundryv1.FoundryAgentTurnResponse{Turn: agentTurnToProto(t), Thread: agentThreadToProto(thread)}, nil
}

func (s *Service) ListFoundryAgentThreads(ctx context.Context, req *foundryv1.ListFoundryAgentThreadsRequest) (*foundryv1.ListFoundryAgentThreadsResponse, error) {
	userID, err := s.agentUser(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	threads, next, err := s.deps.DB.ListFoundryAgentThreads(ctx, req.GetWorkspaceId(), userID, req.GetPageToken(), agentThreadPage)
	switch {
	case errors.Is(err, foundrystore.ErrInvalidPageToken):
		return nil, status.Error(codes.InvalidArgument, "page_token is not one this list returned")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "list threads: %v", err)
	}
	out := &foundryv1.ListFoundryAgentThreadsResponse{NextPageToken: next}
	for _, t := range threads {
		out.Threads = append(out.Threads, agentThreadToProto(t))
	}
	return out, nil
}

// agentThread is the caller's live thread.
func (s *Service) agentThread(ctx context.Context, workspaceID, userID, threadID string) (*foundrymodel.FoundryAgentThread, error) {
	t, err := s.deps.DB.GetFoundryAgentThread(ctx, workspaceID, threadID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || err == nil && t.UserID != userID:
		return nil, status.Error(codes.NotFound, "thread not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the thread: %v", err)
	}
	return t, nil
}

// GetFoundryAgentThread returns the caller's thread, its newest turns and its
// actions.
func (s *Service) GetFoundryAgentThread(ctx context.Context, req *foundryv1.GetFoundryAgentThreadRequest) (*foundryv1.FoundryAgentThreadResponse, error) {
	userID, err := s.agentUser(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	thread, err := s.agentThread(ctx, req.GetWorkspaceId(), userID, req.GetId())
	if err != nil {
		return nil, err
	}
	turns, err := s.deps.DB.ListFoundryAgentTurns(ctx, thread.ID, agentThreadTurns)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list turns: %v", err)
	}
	actions, err := s.deps.DB.ListFoundryAgentActions(ctx, thread.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list actions: %v", err)
	}
	out := &foundryv1.FoundryAgentThreadResponse{Thread: agentThreadToProto(thread)}
	for _, t := range turns {
		out.Turns = append(out.Turns, agentTurnToProto(t))
	}
	now := time.Now()
	for _, a := range actions {
		out.Actions = append(out.Actions, agentActionToProto(a, now))
	}
	return out, nil
}

func (s *Service) DeleteFoundryAgentThread(ctx context.Context, req *foundryv1.DeleteFoundryAgentThreadRequest) (*emptypb.Empty, error) {
	userID, err := s.agentUser(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	switch err := s.deps.DB.DeleteFoundryAgentThread(ctx, req.GetWorkspaceId(), userID, req.GetId()); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "thread not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "delete the thread: %v", err)
	}
	return &emptypb.Empty{}, nil
}

// agentTurn is the caller's turn.
func (s *Service) agentTurn(ctx context.Context, workspaceID, userID, turnID string) (*foundrymodel.FoundryAgentTurn, error) {
	t, err := s.deps.DB.GetFoundryAgentTurn(ctx, workspaceID, turnID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || err == nil && t.UserID != userID:
		return nil, status.Error(codes.NotFound, "turn not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the turn: %v", err)
	}
	return t, nil
}

// ListFoundryAgentTurnEvents returns a turn's events after after_seq, with
// its status read first: a status that ended means every event was stored
// before it, so polling until the events reach last_seq misses none.
func (s *Service) ListFoundryAgentTurnEvents(ctx context.Context, req *foundryv1.ListFoundryAgentTurnEventsRequest) (*foundryv1.ListFoundryAgentTurnEventsResponse, error) {
	if req.GetAfterSeq() < 0 {
		return nil, status.Error(codes.InvalidArgument, "after_seq must not be negative")
	}
	userID, err := s.agentUser(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	t, err := s.agentTurn(ctx, req.GetWorkspaceId(), userID, req.GetId())
	if err != nil {
		return nil, err
	}
	events, err := s.deps.DB.ListFoundryAgentEvents(ctx, t.ID, int(req.GetAfterSeq()), agentEventsPage)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list turn events: %v", err)
	}
	out := &foundryv1.ListFoundryAgentTurnEventsResponse{Status: string(t.Status), LastSeq: int32(t.LastSeq)}
	for _, e := range events {
		out.Events = append(out.Events, &foundryv1.FoundryAgentEvent{Seq: int32(e.Seq), EventJson: string(e.Event), CreatedAt: timestamppb.New(e.CreatedAt)})
	}
	return out, nil
}

func (s *Service) CancelFoundryAgentTurn(ctx context.Context, req *foundryv1.CancelFoundryAgentTurnRequest) (*foundryv1.FoundryAgentTurnResponse, error) {
	userID, err := s.agentUser(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	t, err := s.deps.DB.CancelFoundryAgentTurn(ctx, req.GetWorkspaceId(), userID, req.GetId())
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "turn not found")
	case errors.Is(err, foundrystore.ErrConflict):
		return nil, status.Error(codes.FailedPrecondition, "the turn already ended")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "cancel the turn: %v", err)
	}
	return s.agentTurnResponse(ctx, t)
}

// agentSessions is the host's session port, refusing while it has none.
func (s *Service) agentSessions() (integration.SessionHost, error) {
	if s.deps.AgentSessions == nil || s.deps.ConnectorKeys == nil {
		return nil, status.Error(codes.Unavailable, "the Assistant's actions aren't available on this host")
	}
	return s.deps.AgentSessions, nil
}

// ExecuteFoundryAgentAction runs the caller's prepared action, once:
//  1. it must be theirs, prepared, for the payload they were shown, with its
//     confirmation live and its thread idle, and moves to executing in the
//     same write, so a second click finds it running;
//  2. it runs as the caller now, through a session that confirms as the
//     caller: validated and authorized again, its confirmation consumed;
//  3. it ends succeeded, failed, or unknown when its answer was lost after the
//     confirmation was spent, never to run again; and the agent's next turn,
//     which goes on from it, is queued with it.
func (s *Service) ExecuteFoundryAgentAction(ctx context.Context, req *foundryv1.ExecuteFoundryAgentActionRequest) (*foundryv1.FoundryAgentActionResponse, error) {
	ws := req.GetWorkspaceId()
	userID, err := s.agentUser(ctx, ws)
	if err != nil {
		return nil, err
	}
	sessions, err := s.agentSessions()
	if err != nil {
		return nil, err
	}
	if _, err := hex.DecodeString(req.GetPayloadHash()); err != nil || len(req.GetPayloadHash()) != 64 {
		return nil, status.Error(codes.InvalidArgument, "payload_hash is the action's 64 hex digits")
	}
	a, err := s.deps.DB.ClaimFoundryAgentAction(ctx, ws, userID, req.GetId(), req.GetPayloadHash())
	if refused, ok := agentRefusal(ws, err); ok {
		return nil, refused
	}
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "action not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "start the action: %v", err)
	}
	// From here the click's end is recorded, whatever happens to the caller.
	ctx = context.WithoutCancel(ctx)
	end := s.runAgentAction(ctx, sessions, a)
	continuation := &foundrymodel.FoundryAgentTurn{
		ID: id.New(), WorkspaceID: ws, ThreadID: a.ThreadID, UserID: userID, Kind: foundrymodel.FoundryAgentTurnActionResult, Input: a.ID,
		MaxUSDMicros: s.deps.Cfg.Foundry.Agent.MaxMicros(),
	}
	first, err := foundryagent.MarshalEvent(foundryagent.ActionResult{ActionID: a.ID, Operation: a.Operation, Status: string(end.Status), Result: end.Result, Error: end.Error})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "write the action's result: %v", err)
	}
	if err := s.deps.DB.FinishFoundryAgentAction(ctx, a.ID, end, continuation, first); err != nil {
		return nil, status.Errorf(codes.Internal, "record the action's result: %v", err)
	}
	done, err := s.deps.DB.GetFoundryAgentAction(ctx, ws, a.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the action: %v", err)
	}
	out := &foundryv1.FoundryAgentActionResponse{Action: agentActionToProto(done, time.Now())}
	switch queued, err := s.deps.DB.GetFoundryAgentTurn(ctx, ws, continuation.ID); {
	case errors.Is(err, foundrystore.ErrNotFound):
		// Another turn went live first: the click ended with no continuation.
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the next turn: %v", err)
	default:
		out.Continuation = agentTurnToProto(queued)
	}
	return out, nil
}

// runAgentAction runs an action that is executing, as its person now, and
// says how it ended.
func (s *Service) runAgentAction(ctx context.Context, sessions integration.SessionHost, a *foundrymodel.FoundryAgentAction) foundrymodel.FoundryAgentActionEnd {
	failed := func(why string) foundrymodel.FoundryAgentActionEnd {
		return foundrymodel.FoundryAgentActionEnd{Status: foundrymodel.FoundryAgentActionFailed, Error: why}
	}
	token, err := s.deps.ConnectorKeys.Open(a.TokenSealed, a.KeyID, a.ID)
	if err != nil {
		return failed("the action's confirmation can't be opened; ask the Assistant to prepare it again")
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(a.Payload))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return failed("the action's payload can't be read")
	}
	session, err := sessions.Session(ctx, actor(ctx), integration.ConfirmByCaller{})
	if err != nil {
		return failed("your access to this workspace couldn't be read; nothing ran, so you can ask again")
	}
	switch done := session.Service.ExecuteForPerson(session.Ctx, session.Identity, integration.ExecuteFoundryInput{
		Operation: a.Operation, Target: a.Target, Payload: payload, ConfirmationID: a.ConfirmationID, ConfirmationToken: string(token),
	}).(type) {
	case integration.ExecutedOK:
		result, err := json.Marshal(done.Result)
		if err != nil {
			return foundrymodel.FoundryAgentActionEnd{Status: foundrymodel.FoundryAgentActionUnknown, Error: "it ran, and its result can't be shown"}
		}
		return foundrymodel.FoundryAgentActionEnd{Status: foundrymodel.FoundryAgentActionSucceeded, Result: result}
	case integration.ExecutedUnknown:
		return foundrymodel.FoundryAgentActionEnd{Status: foundrymodel.FoundryAgentActionUnknown,
			Error: "it may or may not have run: Foundry lost its answer (" + toolErrorText(done.Err) + ")"}
	case integration.ExecutedFailed:
		return failed(toolErrorText(done.Err))
	default:
		return failed("it ended in a way Foundry doesn't know")
	}
}

// toolErrorText is a tool's error for the person: its message, else its code.
func toolErrorText(err error) string {
	code, message, ok := integration.ToolErrorDetails(err)
	switch {
	case ok && message != "":
		return message
	case ok:
		return strings.ReplaceAll(code, "_", " ")
	default:
		return "internal error"
	}
}

// agentAction is the caller's action.
func (s *Service) agentAction(ctx context.Context, workspaceID, userID, actionID string) (*foundrymodel.FoundryAgentAction, error) {
	a, err := s.deps.DB.GetFoundryAgentAction(ctx, workspaceID, actionID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || err == nil && a.UserID != userID:
		return nil, status.Error(codes.NotFound, "action not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the action: %v", err)
	}
	return a, nil
}

// RefreshFoundryAgentAction prepares the caller's action again, as the caller
// and with no model: its new confirmation replaces the one that ran out.
func (s *Service) RefreshFoundryAgentAction(ctx context.Context, req *foundryv1.RefreshFoundryAgentActionRequest) (*foundryv1.FoundryAgentActionResponse, error) {
	ws := req.GetWorkspaceId()
	userID, err := s.agentUser(ctx, ws)
	if err != nil {
		return nil, err
	}
	sessions, err := s.agentSessions()
	if err != nil {
		return nil, err
	}
	a, err := s.agentAction(ctx, ws, userID, req.GetId())
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(a.Payload))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, status.Errorf(codes.Internal, "read the action's payload: %v", err)
	}
	sink := &refreshSink{s: s, action: a, userID: userID}
	session, err := sessions.Session(ctx, actor(ctx), integration.ConfirmByPerson{Sink: sink})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "open your session: %v", err)
	}
	_, err = session.Service.PrepareAgain(session.Ctx, session.Identity, integration.FoundryOperationInput{
		Operation: a.Operation, Target: a.Target, Payload: payload,
	}, sink)
	if refused, ok := agentRefusal(ws, err); ok {
		return nil, refused
	}
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "action not found")
	case err != nil:
		// Checked again as the caller, now: what they may no longer do is refused.
		return nil, status.Error(codes.FailedPrecondition, toolErrorText(integration.ToolError(err)))
	}
	return &foundryv1.FoundryAgentActionResponse{Action: agentActionToProto(sink.refreshed, time.Now())}, nil
}

// refreshSink gives an action the confirmation prepared again for it.
type refreshSink struct {
	s         *Service
	action    *foundrymodel.FoundryAgentAction
	userID    string
	refreshed *foundrymodel.FoundryAgentAction
}

func (r *refreshSink) Allows(operation string) bool { return foundryagent.Operations[operation] }

func (r *refreshSink) Record(ctx context.Context, p integration.PreparedAction) (integration.AwaitingPerson, error) {
	sealed, keyID, err := r.s.deps.ConnectorKeys.Seal([]byte(p.Confirmation.Token), r.action.ID)
	if err != nil {
		return integration.AwaitingPerson{}, err
	}
	refreshed, err := r.s.deps.DB.RefreshFoundryAgentAction(ctx, r.action.WorkspaceID, r.userID, r.action.ID, p.PayloadHash,
		p.Confirmation.ID, sealed, keyID, p.Confirmation.ExpiresAt)
	if err != nil {
		return integration.AwaitingPerson{}, err
	}
	r.refreshed = refreshed
	return integration.AwaitingPerson{ActionID: refreshed.ID, ExpiresAt: refreshed.ExpiresAt}, nil
}

func (s *Service) DismissFoundryAgentAction(ctx context.Context, req *foundryv1.DismissFoundryAgentActionRequest) (*foundryv1.FoundryAgentActionResponse, error) {
	ws := req.GetWorkspaceId()
	userID, err := s.agentUser(ctx, ws)
	if err != nil {
		return nil, err
	}
	a, err := s.deps.DB.DismissFoundryAgentAction(ctx, ws, userID, req.GetId())
	if refused, ok := agentRefusal(ws, err); ok {
		return nil, refused
	}
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "action not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "dismiss the action: %v", err)
	}
	return &foundryv1.FoundryAgentActionResponse{Action: agentActionToProto(a, time.Now())}, nil
}

func agentThreadToProto(t *foundrymodel.FoundryAgentThread) *foundryv1.FoundryAgentThread {
	return &foundryv1.FoundryAgentThread{Id: t.ID, Title: t.Title, Runtime: string(t.Runtime), CreatedAt: timestamppb.New(t.CreatedAt),
		UpdatedAt: timestamppb.New(t.UpdatedAt)}
}

func agentTurnToProto(t *foundrymodel.FoundryAgentTurn) *foundryv1.FoundryAgentTurn {
	out := &foundryv1.FoundryAgentTurn{
		Id: t.ID, ThreadId: t.ThreadID, Kind: string(t.Kind), Status: string(t.Status), UsdMicros: t.Spend.USDMicros, Steps: int32(t.Steps),
		LastSeq: int32(t.LastSeq), Attempts: int32(t.Attempts), CreatedAt: timestamppb.New(t.CreatedAt),
	}
	switch t.Kind {
	case foundrymodel.FoundryAgentTurnMessage:
		out.Text = t.Input
	case foundrymodel.FoundryAgentTurnActionResult:
		out.ActionId = t.Input
	}
	var page foundryagent.PageContext
	if len(t.PageContext) > 0 && json.Unmarshal(t.PageContext, &page) == nil {
		out.PageContext = &foundryv1.FoundryAgentPageContext{OntologyId: page.OntologyID, LoadId: page.LoadID}
	}
	if t.Error != nil {
		out.Error = *t.Error
	}
	if t.StartedAt != nil {
		out.StartedAt = timestamppb.New(*t.StartedAt)
	}
	if t.FinishedAt != nil {
		out.FinishedAt = timestamppb.New(*t.FinishedAt)
	}
	return out
}

func agentActionToProto(a *foundrymodel.FoundryAgentAction, now time.Time) *foundryv1.FoundryAgentAction {
	out := &foundryv1.FoundryAgentAction{
		Id: a.ID, TurnId: a.TurnID, ThreadId: a.ThreadID, Operation: a.Operation, Target: a.Target, PayloadJson: string(a.Payload),
		PayloadHash: a.PayloadHash, DisplayJson: string(a.Display), TargetNamedByUser: a.TargetNamedByUser, Status: string(a.Shown(now)),
		ResultJson: string(a.Result), ExpiresAt: timestamppb.New(a.ExpiresAt), CreatedAt: timestamppb.New(a.CreatedAt),
	}
	if a.Error != nil {
		out.Error = *a.Error
	}
	if a.DecidedBy != nil {
		out.DecidedByUserId = *a.DecidedBy
	}
	if a.DecidedAt != nil {
		out.DecidedAt = timestamppb.New(*a.DecidedAt)
	}
	return out
}
