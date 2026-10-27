package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

const (
	// maxQuestionChars bounds a question's text, in characters.
	maxQuestionChars = 4000
	// maxQuestionFocusBytes bounds the focus a question passes to the agent.
	maxQuestionFocusBytes = 4 << 10
	// foundryQuestionPageSize is how many questions a list returns at once.
	foundryQuestionPageSize = 50
	// foundryQuestionEventsPage is how many events one poll returns.
	foundryQuestionEventsPage = 200
)

// AskFoundryQuestion queues a question of one of the ontology's loads; the
// foundry-ask-worker answers it. Authz has checked the caller may read the
// workspace and query the database.
//  1. The focus, when set, must be an entity by type and id, stored as
//     such. The same client token returns the question it queued.
//  2. The load (the current one when unset) must be ready or partial, and
//     the thread, when set, one of the ontology's.
//  3. The workspace's limits must allow it: questions asked in the last
//     hour, and what the month's questions settled plus this one's cap.
//
// A local install with no AI provider refuses it (FOUNDRY_AI_NOT_CONFIGURED).
func (s *Service) AskFoundryQuestion(ctx context.Context, req *foundryv1.AskFoundryQuestionRequest) (*foundryv1.FoundryQuestionResponse, error) {
	question := strings.TrimSpace(req.GetQuestion())
	switch {
	case req.GetClientToken() == "" || len(req.GetClientToken()) > maxClientTokenLength:
		return nil, status.Errorf(codes.InvalidArgument, "client_token is required, at most %d characters", maxClientTokenLength)
	case question == "" || !utf8.ValidString(question) || utf8.RuneCountInString(question) > maxQuestionChars:
		return nil, status.Errorf(codes.InvalidArgument, "question is required, at most %d characters", maxQuestionChars)
	}
	var focus json.RawMessage
	if raw := req.GetFocusJson(); raw != "" {
		if len(raw) > maxQuestionFocusBytes {
			return nil, status.Errorf(codes.InvalidArgument, "focus_json is at most %d bytes", maxQuestionFocusBytes)
		}
		ref, err := ontologyagent.ParseFocus(raw)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "focus_json: %v", err)
		}
		focus, _ = json.Marshal(ref) // two strings: it can't fail
	}
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if err := s.requireAI(ctx, o.WorkspaceID); err != nil {
		return nil, err
	}
	token := req.GetClientToken()
	existing, err := s.deps.DB.GetFoundryQuestionByClientToken(ctx, o.ID, token)
	switch {
	case err == nil && existing.Question != question:
		return nil, status.Error(codes.InvalidArgument, "client_token was already used for another question")
	case err == nil:
		return &foundryv1.FoundryQuestionResponse{Question: foundryQuestionToProto(existing)}, nil
	case !errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Errorf(codes.Internal, "look up question: %v", err)
	}
	loadID := req.GetLoadId()
	if loadID == "" {
		if o.CurrentLoadID == nil {
			return nil, status.Error(codes.FailedPrecondition, "the ontology has no load to ask yet; load a version first")
		}
		loadID = *o.CurrentLoadID
	}
	load, err := s.deps.DB.GetFoundryLoad(ctx, o.ID, loadID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || err == nil && load.Status == foundrymodel.FoundryLoadDeleted:
		return nil, status.Error(codes.NotFound, "load not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "get load: %v", err)
	case !load.Status.Servable():
		return nil, status.Error(codes.FailedPrecondition, "only a ready or partial load can be asked")
	}
	q := &foundrymodel.FoundryQuestion{
		ID: id.New(), WorkspaceID: o.WorkspaceID, OntologyID: o.ID, LoadID: load.ID, VersionID: load.VersionID, Question: question,
		Focus: focus, ClientToken: &token, MaxUSDMicros: s.deps.Cfg.Foundry.Ask.MaxMicros(),
	}
	q.ThreadID = q.ID
	if thread := req.GetThreadId(); thread != "" {
		first, err := s.deps.DB.GetFoundryQuestion(ctx, o.ID, thread)
		switch {
		case errors.Is(err, foundrystore.ErrNotFound) || err == nil && first.ThreadID != first.ID:
			return nil, status.Error(codes.NotFound, "thread not found")
		case err != nil:
			return nil, status.Errorf(codes.Internal, "get thread: %v", err)
		}
		q.ThreadID = thread
	}
	if userID := actor(ctx).UserID; userID != "" {
		q.AskedByUserID = &userID
	}
	var refusal foundrymodel.FoundryQuestionRefusal
	switch err := s.deps.DB.CreateFoundryQuestion(ctx, q, s.foundryQuestionLimits()); {
	case errors.As(err, &refusal) && refusal.Budget:
		return nil, status.Error(codes.FailedPrecondition, refusal.Reason)
	case errors.As(err, &refusal):
		return nil, status.Error(codes.ResourceExhausted, refusal.Reason)
	case errors.Is(err, foundrystore.ErrConflict):
		// The same click again won the race.
		raced, lookupErr := s.deps.DB.GetFoundryQuestionByClientToken(ctx, o.ID, token)
		if lookupErr != nil || raced.Question != question {
			return nil, status.Error(codes.InvalidArgument, "client_token was already used for another question")
		}
		return &foundryv1.FoundryQuestionResponse{Question: foundryQuestionToProto(raced)}, nil
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "workspace not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "queue question: %v", err)
	}
	return s.foundryQuestionResponse(ctx, o.ID, q.ID)
}

// foundryQuestionLimits are the workspace limits of foundry.ask, this month.
func (s *Service) foundryQuestionLimits() foundrymodel.FoundryQuestionLimits {
	a := s.deps.Cfg.Foundry.Ask
	now := time.Now().UTC()
	return foundrymodel.FoundryQuestionLimits{
		Since: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), CapMicros: a.MonthlyMicros(),
		PerHour: a.PerHour, Running: a.MaxRunning,
	}
}

func (s *Service) GetFoundryQuestion(ctx context.Context, req *foundryv1.GetFoundryQuestionRequest) (*foundryv1.FoundryQuestionResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	return s.foundryQuestionResponse(ctx, o.ID, req.GetId())
}

// ListFoundryQuestions returns the ontology's questions, or one thread's,
// newest first, with the workspace's questions budget this month. Their
// results leave out the queries' outputs, which a page of answers doesn't
// need and which can run to 512 KiB an answer: GetFoundryQuestion returns a
// question's whole result, for its visuals.
func (s *Service) ListFoundryQuestions(ctx context.Context, req *foundryv1.ListFoundryQuestionsRequest) (*foundryv1.ListFoundryQuestionsResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	questions, next, err := s.deps.DB.ListFoundryQuestions(ctx, o.ID, req.GetThreadId(), req.GetPageToken(), foundryQuestionPageSize)
	switch {
	case errors.Is(err, foundrystore.ErrInvalidPageToken):
		return nil, status.Error(codes.InvalidArgument, "page_token is not one this list returned")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "list questions: %v", err)
	}
	limits := s.foundryQuestionLimits()
	spend, err := s.deps.DB.SumFoundryQuestionSpend(ctx, o.WorkspaceID, limits.Since)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sum questions spend: %v", err)
	}
	a := s.deps.Cfg.Foundry.Ask
	resp := &foundryv1.ListFoundryQuestionsResponse{NextPageToken: next, Budget: &foundryv1.FoundryQuestionBudget{
		MonthlyUsdMicros: limits.CapMicros, SpentUsdMicros: spend.SpentMicros, ReservedUsdMicros: spend.ReservedMicros,
		QuestionUsdMicros: a.MaxMicros(), MaxSteps: int32(a.MaxSteps), MaxWallSecs: int32(a.MaxWallSecs),
		ConcurrentLimit: int32(limits.Running), HourlyLimit: int32(limits.PerHour),
	}}
	for _, q := range questions {
		listed := *q
		listed.Result = ontologyagent.WithoutOutputs(q.Result)
		resp.Questions = append(resp.Questions, foundryQuestionToProto(&listed))
	}
	return resp, nil
}

// ListFoundryQuestionEvents returns a question's events after after_seq, with
// its status read first: a status that ended means every event was stored
// before it, so polling until the events reach last_seq misses none.
func (s *Service) ListFoundryQuestionEvents(ctx context.Context, req *foundryv1.ListFoundryQuestionEventsRequest) (*foundryv1.ListFoundryQuestionEventsResponse, error) {
	if req.GetAfterSeq() < 0 {
		return nil, status.Error(codes.InvalidArgument, "after_seq must not be negative")
	}
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	q, err := s.foundryQuestion(ctx, o.ID, req.GetId())
	if err != nil {
		return nil, err
	}
	events, err := s.deps.DB.ListFoundryQuestionEvents(ctx, q.ID, int(req.GetAfterSeq()), foundryQuestionEventsPage)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list question events: %v", err)
	}
	resp := &foundryv1.ListFoundryQuestionEventsResponse{Status: string(q.Status), LastSeq: int32(q.LastSeq)}
	for _, e := range events {
		resp.Events = append(resp.Events, &foundryv1.FoundryQuestionEvent{
			Seq: int32(e.Seq), EventJson: string(e.Event), CreatedAt: timestamppb.New(e.CreatedAt),
		})
	}
	return resp, nil
}

// CancelFoundryQuestion ends a queued or running question; its attempt stops
// at its next write.
func (s *Service) CancelFoundryQuestion(ctx context.Context, req *foundryv1.CancelFoundryQuestionRequest) (*foundryv1.FoundryQuestionResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	q, err := s.deps.DB.CancelFoundryQuestion(ctx, o.ID, req.GetId())
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "question not found")
	case errors.Is(err, foundrystore.ErrConflict):
		return nil, status.Error(codes.FailedPrecondition, "the question already ended")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "cancel question: %v", err)
	}
	return &foundryv1.FoundryQuestionResponse{Question: foundryQuestionToProto(q)}, nil
}

func (s *Service) foundryQuestion(ctx context.Context, ontologyID, id string) (*foundrymodel.FoundryQuestion, error) {
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	q, err := s.deps.DB.GetFoundryQuestion(ctx, ontologyID, id)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "question not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "get question: %v", err)
	}
	return q, nil
}

func (s *Service) foundryQuestionResponse(ctx context.Context, ontologyID, id string) (*foundryv1.FoundryQuestionResponse, error) {
	q, err := s.foundryQuestion(ctx, ontologyID, id)
	if err != nil {
		return nil, err
	}
	return &foundryv1.FoundryQuestionResponse{Question: foundryQuestionToProto(q)}, nil
}

func foundryQuestionToProto(q *foundrymodel.FoundryQuestion) *foundryv1.FoundryQuestion {
	out := &foundryv1.FoundryQuestion{
		Id: q.ID, LoadId: q.LoadID, VersionId: q.VersionID, ThreadId: q.ThreadID, Question: q.Question, FocusJson: string(q.Focus),
		Status: string(q.Status), ResultJson: string(q.Result), UsdMicros: q.Spend.USDMicros, LastSeq: int32(q.LastSeq),
		Attempts: int32(q.Attempts), ReservedMicros: q.ReservedMicros, CreatedAt: timestamppb.New(q.CreatedAt),
	}
	if q.Error != nil {
		out.Error = *q.Error
	}
	if q.AskedByUserID != nil {
		out.AskedByUserId = *q.AskedByUserID
	}
	if q.StartedAt != nil {
		out.StartedAt = timestamppb.New(*q.StartedAt)
	}
	if q.FinishedAt != nil {
		out.FinishedAt = timestamppb.New(*q.FinishedAt)
	}
	return out
}
