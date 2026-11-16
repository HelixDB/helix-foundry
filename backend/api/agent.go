package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/access"
	foundryagent "github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/agentquestion"
	"github.com/helixdb/helix-foundry/backend/automation"
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
	// agentThreadSkips is how many store pages a restricted member's list
	// reads before it answers, while their grants leave none of a page.
	agentThreadSkips = 5
)

// agentIDPattern is an id a page context may name.
var agentIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// agentUser is the user the call is for: a person signed in, never a key, an
// agent, or a caller the host sent on as a grantee (SourceGrantee: one the
// host's own check refused, who may not be restricted here at all); a
// restricted member is a user, narrowed by the access check and the turn's
// Reach. Off, the Assistant is FAILED_PRECONDITION, reason
// FOUNDRY_AGENT_DISABLED.
func (s *Service) agentUser(ctx context.Context, workspaceID string) (string, error) {
	if !s.deps.Cfg.Foundry.Agent.Enabled {
		return "", foundryPrecondition(workspaceID, "FOUNDRY_AGENT_DISABLED", "Founder isn't available here")
	}
	a := actor(ctx)
	if a.Source != ports.SourceUser || a.UserID == "" || a.ServiceCredentialID != "" {
		return "", status.Error(codes.PermissionDenied, "Founder answers people signed in to Foundry")
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
		// The answers are to questions no longer open.
		foundrymodel.FoundryAgentQuestionsClosed: "FOUNDRY_AGENT_QUESTIONS_CLOSED",
	}[r.Reason]
	if r.Reason == foundrymodel.FoundryAgentPerHour {
		return status.Error(codes.ResourceExhausted, r.Detail), true
	}
	return foundryPrecondition(workspaceID, reason, r.Detail), true
}

// SendFoundryAgentMessage queues a person's message in their thread, or a new
// one, which takes the workspace's runtime (agentThreadRuntime), or their
// answers to the questions their thread's newest turn asked them, checked by
// agentquestion.Check, opened by an answers event. Answers to a relayed
// generation's questions are sent to it first, as the person
// (foundryagent.SendAnswers), while it still waits on that round
// (FOUNDRY_AGENT_QUESTIONS_CLOSED once it doesn't). The same client token
// returns the turn it queued. A thread a helper runs is refused while no
// helper that runs it is online; one Foundry's worker runs, while the
// workspace's model can't run. A grantee's page context keeps only what its
// grants show (granteePage), and it relays no generation's answers: an owner
// or admin answers a draft.
func (s *Service) SendFoundryAgentMessage(ctx context.Context, req *foundryv1.SendFoundryAgentMessageRequest) (*foundryv1.FoundryAgentTurnResponse, error) {
	ws := req.GetWorkspaceId()
	userID, err := s.agentUser(ctx, ws)
	if err != nil {
		return nil, err
	}
	g, grantee := access.GranteeFrom(ctx)
	token := req.GetClientToken()
	page := foundryagent.PageContext{OntologyID: req.GetPageContext().GetOntologyId(), LoadID: req.GetPageContext().GetLoadId(),
		DashboardID: req.GetPageContext().GetDashboardId(), NewAutomation: req.GetPageContext().GetNewAutomation()}
	// A browser's zone Go's database lacks is dropped: the Assistant asks then.
	if _, err := automation.Zone(req.GetPageContext().GetTimeZone()); err == nil {
		page.TimeZone = req.GetPageContext().GetTimeZone()
	}
	switch {
	case token == "" || len(token) > maxClientTokenLength:
		return nil, status.Errorf(codes.InvalidArgument, "client_token is required, at most %d characters", maxClientTokenLength)
	case req.GetThreadId() != "" && !agentIDPattern.MatchString(req.GetThreadId()):
		return nil, status.Error(codes.InvalidArgument, "thread_id is malformed")
	case page.OntologyID != "" && !agentIDPattern.MatchString(page.OntologyID), page.LoadID != "" && !agentIDPattern.MatchString(page.LoadID),
		page.LoadID != "" && page.OntologyID == "", page.DashboardID != "" && !agentIDPattern.MatchString(page.DashboardID):
		return nil, status.Error(codes.InvalidArgument, "page_context names a load of an ontology, or a dashboard, by their ids")
	}
	if grantee {
		if page, err = s.granteePage(ctx, g, page); err != nil {
			return nil, err
		}
	}
	t := &foundrymodel.FoundryAgentTurn{
		ID: id.New(), WorkspaceID: ws, ThreadID: req.GetThreadId(), UserID: userID, ClientToken: &token, MaxUSDMicros: s.deps.Cfg.Foundry.Agent.MaxMicros(),
	}
	var first json.RawMessage
	// answered is the turn the answers answer.
	var answered *foundrymodel.FoundryAgentTurn
	switch input := req.GetInput().(type) {
	case *foundryv1.SendFoundryAgentMessageRequest_Text:
		text := strings.TrimSpace(input.Text)
		if text == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > maxAgentMessageChars {
			return nil, status.Errorf(codes.InvalidArgument, "text is required, at most %d characters", maxAgentMessageChars)
		}
		t.Kind, t.Input = foundrymodel.FoundryAgentTurnMessage, text
	case *foundryv1.SendFoundryAgentMessageRequest_Answers:
		if t.ThreadID == "" {
			return nil, status.Error(codes.InvalidArgument, "thread_id is required with answers: the thread of the turn they answer")
		}
		asking, err := s.agentTurn(ctx, ws, userID, input.Answers.GetTurnId())
		switch {
		case err != nil:
			return nil, err
		case asking.ThreadID != t.ThreadID:
			return nil, status.Error(codes.NotFound, "turn not found")
		case asking.Asked == nil:
			return nil, foundryPrecondition(ws, "FOUNDRY_AGENT_QUESTIONS_CLOSED", "that turn asked you nothing")
		case grantee && asking.Asked.Generation != nil:
			return nil, foundryPrecondition(ws, "FOUNDRY_AGENT_QUESTIONS_CLOSED",
				"a draft's questions are answered by an owner or admin, on the generation's page in Foundry")
		}
		given := make([]agentquestion.Answer, 0, len(input.Answers.GetAnswers()))
		for _, a := range input.Answers.GetAnswers() {
			given = append(given, agentquestion.Answer{QuestionID: a.GetQuestionId(), Options: a.GetOptions(), Text: a.GetText()})
		}
		answers, err := agentquestion.Check(asking.Asked.Questions, given, false)
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		t.Kind, t.Input, t.Answers, answered = foundrymodel.FoundryAgentTurnAnswers, asking.ID, answers, asking
		if first, err = foundryagent.MarshalEvent(foundryagent.Answered{TurnID: asking.ID, Answers: answers}); err != nil {
			return nil, status.Errorf(codes.Internal, "write the answers: %v", err)
		}
	default:
		return nil, status.Error(codes.InvalidArgument, "text or answers is required")
	}
	// The same client token is the same turn: its kind, and what it says.
	same := func(other *foundrymodel.FoundryAgentTurn) bool {
		return other.Kind == t.Kind && other.Input == t.Input && reflect.DeepEqual(other.Answers, t.Answers)
	}
	existing, err := s.deps.DB.GetFoundryAgentTurnByClientToken(ctx, ws, userID, token)
	switch {
	case err == nil && !same(existing):
		return nil, status.Error(codes.InvalidArgument, "client_token was already used for another message")
	case err == nil:
		// A restricted member reads again only a turn of a thread they
		// still may (agentThread).
		if grantee {
			if _, err := s.agentThread(ctx, ws, userID, existing.ThreadID); err != nil {
				return nil, err
			}
		}
		return s.agentTurnResponse(ctx, existing)
	case !errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Errorf(codes.Internal, "look up the message: %v", err)
	}
	runtime, err := s.agentThreadRuntime(ctx, ws, userID, req.GetThreadId())
	if err != nil {
		return nil, err
	}
	// A helper's thread runs on its owner's subscription, not the workspace's model.
	if !runtime.Subscription() {
		if err := s.requireAI(ctx, ws); err != nil {
			return nil, err
		}
	}
	// sent: the answers reached a relayed generation before the turn is queued.
	sent := answered != nil && answered.Asked.Generation != nil
	if sent {
		if err := s.sendRelayedAnswers(ctx, ws, userID, answered, t.Answers); err != nil {
			// The same click again, which sent them first, has its turn.
			if raced, lookupErr := s.deps.DB.GetFoundryAgentTurnByClientToken(ctx, ws, userID, token); lookupErr == nil && same(raced) {
				return s.agentTurnResponse(ctx, raced)
			}
			return nil, err
		}
	}
	if page != (foundryagent.PageContext{}) {
		t.PageContext, _ = json.Marshal(page) // strings only: it can't fail
	}
	var thread *foundrymodel.FoundryAgentThread
	if t.ThreadID == "" {
		t.ThreadID = id.New()
		thread = &foundrymodel.FoundryAgentThread{ID: t.ThreadID, WorkspaceID: ws, UserID: userID, Title: foundryagent.Title(t.Input),
			Runtime: runtime}
		// A restricted member's thread holds what their grants give now;
		// each of its turns adds what they hold when it runs.
		if grantee {
			targets, err := s.targetsOf(ctx, ws, g.Principals())
			if err != nil {
				return nil, err
			}
			thread.Granted = &foundrymodel.FoundryAgentThreadGrants{Targets: targets}
		}
	}
	err = s.deps.DB.CreateFoundryAgentTurn(ctx, t, thread, s.agentLimits(), first)
	var refusal foundrymodel.FoundryAgentRefusal
	if sent && errors.As(err, &refusal) {
		// The draft has the answers: only the Assistant's reply is refused.
		refusal.Detail = "your answers reached the draft, which carries on from them, but Founder can't reply: " + refusal.Detail
		err = refusal
	}
	if refused, ok := agentRefusal(ws, err); ok {
		return nil, refused
	}
	switch {
	case errors.Is(err, foundrystore.ErrConflict):
		// The same click again won the race.
		raced, lookupErr := s.deps.DB.GetFoundryAgentTurnByClientToken(ctx, ws, userID, token)
		if lookupErr != nil || !same(raced) {
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

// granteePage is page as g may name it to Founder: an ontology whose current
// load its grants cover, that load, and a pinned dashboard; anything else,
// and a new automation, is dropped, so Founder never reads a name the
// grantee can't see. The time zone, the person's own, stays.
func (s *Service) granteePage(ctx context.Context, g access.Grantee, page foundryagent.PageContext) (foundryagent.PageContext, error) {
	out := foundryagent.PageContext{TimeZone: page.TimeZone}
	if page.OntologyID != "" {
		loads, err := s.granteeLoads(ctx, g, []string{page.OntologyID})
		if err != nil {
			return out, err
		}
		if l, ok := loads[page.OntologyID]; ok {
			out.OntologyID = page.OntologyID
			if page.LoadID == l.LoadID {
				out.LoadID = page.LoadID
			}
		}
	}
	if page.DashboardID != "" {
		d, err := s.deps.DB.GetFoundryDashboard(ctx, g.WorkspaceID, page.DashboardID)
		switch {
		case err == nil && d.PinnedAt != nil:
			out.DashboardID = page.DashboardID
		case err != nil && !errors.Is(err, foundrystore.ErrNotFound):
			return out, status.Errorf(codes.Internal, "read the dashboard: %v", err)
		}
	}
	return out, nil
}

// sendRelayedAnswers sends a relayed generation the person's answers, as
// them, while asking is still the thread's newest turn, done, and the
// generation still waits on the round asked: else (the conversation went on,
// or the round was answered on its page or moved on) the questions are
// closed (FOUNDRY_AGENT_QUESTIONS_CLOSED) and nothing is sent. The store
// checks the thread again, under its lock, when it queues the turn.
func (s *Service) sendRelayedAnswers(ctx context.Context, ws, userID string, asking *foundrymodel.FoundryAgentTurn, answers []agentquestion.Answer) error {
	newest, err := s.deps.DB.ListFoundryAgentTurns(ctx, asking.ThreadID, 1)
	switch {
	case err != nil:
		return status.Errorf(codes.Internal, "read the thread: %v", err)
	case len(newest) == 0 || newest[len(newest)-1].ID != asking.ID || asking.Status != foundrymodel.FoundryAgentTurnDone:
		return foundryPrecondition(ws, "FOUNDRY_AGENT_QUESTIONS_CLOSED", "these questions are closed: they were answered, or the conversation went on")
	}
	g := asking.Asked.Generation
	gen, err := s.deps.DB.GetOntologyGeneration(ctx, g.OntologyID, g.GenerationID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || err == nil && (gen.Status != foundrymodel.OntologyGenerationNeedsInput || gen.Questions == nil ||
		gen.Questions.Round != g.Round):
		return foundryPrecondition(ws, "FOUNDRY_AGENT_QUESTIONS_CLOSED",
			"the draft isn't waiting for these answers any more: they were answered on its page, or it moved on")
	case err != nil:
		return status.Errorf(codes.Internal, "read the generation: %v", err)
	}
	sessions, err := s.agentSessions()
	if err != nil {
		return err
	}
	return foundryagent.SendAnswers(ctx, sessions, userID, asking.Asked, ws, answers)
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

// ListFoundryAgentThreads lists the caller's chats; a restricted member's
// only as agentThread finds them: the store leaves out the whole view's, and
// those whose grants they no longer hold are left out here, so a page of
// theirs may be short. Store pages that leave nothing are skipped, at most
// agentThreadSkips of them.
func (s *Service) ListFoundryAgentThreads(ctx context.Context, req *foundryv1.ListFoundryAgentThreadsRequest) (*foundryv1.ListFoundryAgentThreadsResponse, error) {
	ws := req.GetWorkspaceId()
	userID, err := s.agentUser(ctx, ws)
	if err != nil {
		return nil, err
	}
	g, grantee := access.GranteeFrom(ctx)
	var grants access.Grants
	if grantee {
		if grants, err = s.grantsOf(ctx, ws, g.Principals()); err != nil {
			return nil, err
		}
	}
	out := &foundryv1.ListFoundryAgentThreadsResponse{NextPageToken: req.GetPageToken()}
	for range agentThreadSkips {
		threads, next, err := s.deps.DB.ListFoundryAgentThreads(ctx, ws, userID, grantee, out.NextPageToken, agentThreadPage)
		switch {
		case errors.Is(err, foundrystore.ErrInvalidPageToken):
			return nil, status.Error(codes.InvalidArgument, "page_token is not one this list returned")
		case err != nil:
			return nil, status.Errorf(codes.Internal, "list threads: %v", err)
		}
		out.NextPageToken = next
		for _, t := range threads {
			if !grantee || grants.Holds(t.Granted.Targets) {
				out.Threads = append(out.Threads, agentThreadToProto(t))
			}
		}
		if len(out.Threads) > 0 || next == "" {
			break
		}
	}
	return out, nil
}

// agentThread is the caller's live thread. A restricted member's is there
// only while they hold every grant its turns ran under: one made or run with
// the workspace's whole view (before their restriction, or while they were
// an owner or admin) isn't, nor one whose grants they lost, as an Analyst
// answer goes with its grants.
func (s *Service) agentThread(ctx context.Context, workspaceID, userID, threadID string) (*foundrymodel.FoundryAgentThread, error) {
	t, err := s.deps.DB.GetFoundryAgentThread(ctx, workspaceID, threadID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || err == nil && t.UserID != userID:
		return nil, status.Error(codes.NotFound, "thread not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the thread: %v", err)
	}
	g, grantee := access.GranteeFrom(ctx)
	if !grantee {
		return t, nil
	}
	if t.Granted == nil {
		return nil, status.Error(codes.NotFound, "thread not found")
	}
	grants, err := s.grantsOf(ctx, workspaceID, g.Principals())
	if err != nil {
		return nil, err
	}
	if !grants.Holds(t.Granted.Targets) {
		return nil, status.Error(codes.NotFound, "thread not found")
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
	if _, grantee := access.GranteeFrom(ctx); grantee {
		if _, err := s.agentThread(ctx, req.GetWorkspaceId(), userID, req.GetId()); err != nil {
			return nil, err
		}
	}
	switch err := s.deps.DB.DeleteFoundryAgentThread(ctx, req.GetWorkspaceId(), userID, req.GetId()); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "thread not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "delete the thread: %v", err)
	}
	return &emptypb.Empty{}, nil
}

// agentTurn is the caller's turn: a restricted member's only in a thread
// agentThread finds for them.
func (s *Service) agentTurn(ctx context.Context, workspaceID, userID, turnID string) (*foundrymodel.FoundryAgentTurn, error) {
	t, err := s.deps.DB.GetFoundryAgentTurn(ctx, workspaceID, turnID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || err == nil && t.UserID != userID:
		return nil, status.Error(codes.NotFound, "turn not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the turn: %v", err)
	}
	if _, grantee := access.GranteeFrom(ctx); grantee {
		if _, err := s.agentThread(ctx, workspaceID, userID, t.ThreadID); err != nil {
			return nil, err
		}
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
	if _, grantee := access.GranteeFrom(ctx); grantee {
		if _, err := s.agentTurn(ctx, req.GetWorkspaceId(), userID, req.GetId()); err != nil {
			return nil, err
		}
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
		return nil, status.Error(codes.Unavailable, "Founder's actions aren't available on this host")
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
	// From here the click's end is recorded, whatever happens to the caller,
	// and its run ends before a send would sweep it up as unknown.
	ctx = context.WithoutCancel(ctx)
	runCtx, cancel := context.WithTimeout(ctx, foundrymodel.FoundryAgentExecutingFor-agentRunMargin)
	end := foundryagent.RunAction(runCtx, sessions, s.deps.ConnectorKeys, a)
	cancel()
	if end.Status == foundrymodel.FoundryAgentActionSucceeded && (a.Operation == "create_automation" || a.Operation == "update_automation") {
		end = s.approveOnClick(ctx, ws, userID, a.Payload, end)
	}
	continuation := &foundrymodel.FoundryAgentTurn{
		ID: id.New(), WorkspaceID: ws, ThreadID: a.ThreadID, UserID: userID, Kind: foundrymodel.FoundryAgentTurnActionResult, Input: a.ID,
		MaxUSDMicros: s.deps.Cfg.Foundry.Agent.MaxMicros(),
	}
	if !s.agentContinues(ctx, ws, a.ThreadID) {
		// No helper would take it: the click ends, and the thread waits for
		// the person's next message.
		continuation = nil
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
	if continuation == nil {
		return out, nil
	}
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

// agentContinues reports whether a click's thread goes on from it: always
// on Foundry's worker; on a helper's runtime only while a helper that runs
// it is online, since a turn queued for none (the helper just revoked, or
// stopped) would only wait. A failed read goes on, as before helpers.
func (s *Service) agentContinues(ctx context.Context, workspaceID, threadID string) bool {
	thread, err := s.deps.DB.GetFoundryAgentThread(ctx, workspaceID, threadID)
	if err != nil || !thread.Runtime.Subscription() {
		return true
	}
	if s.deps.AgentHosts == nil {
		return false
	}
	online, err := s.deps.AgentHosts.AgentRuntimeOnline(ctx, thread.Runtime)
	return err != nil || online
}

// approveOnClick approves the automation a create or an edit the person just
// ran made: their click on the card, which showed the steps in payload, is
// their approval of exactly those steps, as their click on Approve in the
// site is. An edit whose payload carries no spec (a new name or description)
// showed no steps, so it approves nothing: the automation's approval stays as
// it was. Saved but not approved (what it names is gone, its steps no longer
// the card's), the action still succeeded, and says why.
func (s *Service) approveOnClick(ctx context.Context, workspaceID, userID string, payload json.RawMessage,
	end foundrymodel.FoundryAgentActionEnd) foundrymodel.FoundryAgentActionEnd {
	var card struct {
		Spec json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(payload, &card); err != nil {
		end.Error = "it was saved, but the steps its card showed can't be read: approve it on its page in Foundry"
		return end
	}
	if len(card.Spec) == 0 {
		return end
	}
	shown, err := automation.ParseSpec(card.Spec)
	if err != nil {
		end.Error = "it was saved, but the steps its card showed can't be read: approve it on its page in Foundry"
		return end
	}
	var ran struct {
		Result struct {
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
		} `json:"result"`
	}
	if err := json.Unmarshal(end.Result, &ran); err != nil || ran.Result.ID == "" || ran.Result.Revision < 1 {
		end.Error = "it was saved, but its approval can't be read: approve it on its page in Foundry"
		return end
	}
	if _, err := s.approveAutomation(ctx, workspaceID, userID, ran.Result.ID, ran.Result.Revision, &shown); err != nil {
		end.Error = "it was saved, but not approved (" + status.Convert(err).Message() + "): approve it on its page in Foundry"
	}
	return end
}

// agentRunMargin is how long before foundrymodel.FoundryAgentExecutingFor a
// click's run is stopped, leaving it time to record how it ended.
const agentRunMargin = 15 * time.Second

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
// and with no model: its card is drawn again as they read the workspace now
// (a review must still be in the current load, the load current named as it
// is), and its new confirmation replaces the one that ran out. What the
// card's checks refuse, or the caller may no longer do, is refused in the
// tool's words.
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
	session, err := sessions.Session(ctx, ports.Actor{Source: ports.SourceUser, UserID: userID, WorkspaceID: a.WorkspaceID},
		integration.ConfirmByPerson{Sink: sink})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "open your session: %v", err)
	}
	sink.cards = foundryagent.SessionCards(s.deps.DB, s.deps.Databases, session, a.WorkspaceID)
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
		return nil, status.Error(codes.FailedPrecondition, foundryagent.ToolErrorText(integration.ToolError(err)))
	}
	return &foundryv1.FoundryAgentActionResponse{Action: agentActionToProto(sink.refreshed, time.Now())}, nil
}

// refreshSink gives an action its card drawn again, as its clicker, and the
// confirmation prepared again for it.
type refreshSink struct {
	s         *Service
	action    *foundrymodel.FoundryAgentAction
	userID    string
	cards     foundryagent.Cards
	refreshed *foundrymodel.FoundryAgentAction
}

func (r *refreshSink) Allows(operation string) bool { return foundryagent.Operations[operation] }

func (r *refreshSink) Record(ctx context.Context, p integration.PreparedAction) (integration.Recorded, error) {
	card, err := r.cards.Draw(ctx, p)
	if err != nil {
		return nil, err
	}
	sealed, keyID, err := r.s.deps.ConnectorKeys.Seal([]byte(p.Confirmation.Token), r.action.ID)
	if err != nil {
		return nil, err
	}
	refreshed, err := r.s.deps.DB.RefreshFoundryAgentAction(ctx, r.action.WorkspaceID, r.userID, r.action.ID, p.PayloadHash, card.Display,
		p.Confirmation.ID, sealed, keyID, p.Confirmation.ExpiresAt)
	if err != nil {
		return nil, err
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
	case foundrymodel.FoundryAgentTurnAnswers:
		out.Answers = &foundryv1.FoundryAgentAnswers{TurnId: t.Input}
		for _, a := range t.Answers {
			out.Answers.Answers = append(out.Answers.Answers, agentAnswerToProto(a))
		}
	}
	if asked := t.Asked; asked != nil {
		out.Asked = &foundryv1.FoundryAgentAsked{}
		for _, q := range asked.Questions {
			out.Asked.Questions = append(out.Asked.Questions, agentQuestionToProto(q))
		}
		if g := asked.Generation; g != nil {
			out.Asked.Generation = &foundryv1.FoundryAgentAskedGeneration{OntologyId: g.OntologyID, GenerationId: g.GenerationID, Round: int32(g.Round)}
		}
	}
	var page foundryagent.PageContext
	if len(t.PageContext) > 0 && json.Unmarshal(t.PageContext, &page) == nil {
		out.PageContext = &foundryv1.FoundryAgentPageContext{OntologyId: page.OntologyID, LoadId: page.LoadID, DashboardId: page.DashboardID,
			NewAutomation: page.NewAutomation, TimeZone: page.TimeZone}
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
