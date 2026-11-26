package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/agent/llm"
	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/companyresearch"
	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// Company research for a new ontology's goal (package companyresearch): the
// wizard has the workspace's model read the company's website and ask what
// it can't say, then draft the goal from the answers. Both calls answer at
// once, so each runs within researchDeadline, under the site's BFF's
// upstream timeout (60 s).
const (
	// researchDeadline bounds a call, the site's read and the model's
	// calls included: room for a slow or local model.
	researchDeadline = 55 * time.Second
	// researchCalls is the most model calls one answer makes: one, and one
	// more for an answer that doesn't read or a passing refusal.
	researchCalls = 2
	// researchRetryWait is the wait before a call after a passing refusal.
	researchRetryWait = time.Second
	// researchCharsPerToken estimates a prompt's tokens from its bytes, on
	// the generous side, for a call's worst case against the budget.
	researchCharsPerToken = 3
	// finishResearchTimeout bounds recording a call's spend once it ended,
	// however its caller's context ended.
	finishResearchTimeout = 5 * time.Second
)

// researchModel is what a call of company research runs on.
type researchModel struct {
	binding  llm.Binding
	resolved ai.Resolved
}

// ResearchFoundryCompany reads the company's website and has the
// workspace's model research it (companyresearch.ParseResearch): an owner
// or admin's, checked in this order: the caller; the domain
// (companyresearch.NormalizeDomain, INVALID_ARGUMENT in its words); a model
// (requireAI); the caller's hour (CheckFoundryCompanyResearchHour); the
// site's read; the caller's hour again and the workspace's month with the
// prompt read (StartFoundryCompanyResearch); then the model's call, whose
// spend is recorded whatever came of it, with the research when one was
// made.
func (s *Service) ResearchFoundryCompany(ctx context.Context, req *foundryv1.ResearchFoundryCompanyRequest) (*foundryv1.ResearchFoundryCompanyResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, researchDeadline)
	defer cancel()
	ws := req.GetWorkspaceId()
	userID, err := s.researchCallerOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	domain, err := companyresearch.NormalizeDomain(req.GetDomain())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	m, err := s.researchModelOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	cfg := s.deps.Cfg.Foundry.Onboarding.Research
	if err := researchRefusal(ws, cfg, s.deps.DB.CheckFoundryCompanyResearchHour(ctx, userID, cfg.PerUserPerHour)); err != nil {
		return nil, err
	}
	site := s.deps.Sites.Fetch(ctx, domain)
	system, user := companyresearch.ResearchPrompt(domain, site)
	row := &foundrymodel.FoundryCompanyResearch{ID: id.New(), WorkspaceID: ws, UserID: userID, Kind: foundrymodel.FoundryCompanyResearchOfSite, Domain: domain}
	if err := s.startResearch(ctx, row, m, len(system)+len(user)+len(companyresearch.ResearchSchema), companyresearch.ResearchMaxOutput); err != nil {
		return nil, err
	}
	research, cost, failure := llm.CallTool(ctx, m.binding, researchRequest(system, user, companyresearch.ResearchTool,
		companyresearch.ResearchToolDescription, companyresearch.ResearchSchema, companyresearch.ResearchMaxOutput), companyresearch.ResearchTool,
		researchCalls, researchRetryWait, func(raw json.RawMessage) (companyresearch.Research, error) {
			return companyresearch.ParseResearch(raw, site)
		})
	var result json.RawMessage
	if failure == "" {
		if result, err = json.Marshal(research); err != nil {
			failure = "the research couldn't be stored"
		}
	}
	// The row holds the research a goal reads: one it couldn't keep fails.
	if err := s.finishResearch(ctx, row, result, cost, failure); err != nil && failure == "" {
		return nil, status.Errorf(codes.Unavailable, "store the research: %v", err)
	}
	if failure != "" {
		return nil, researchFailed(ws, m.resolved, "Foundry couldn’t research "+domain, failure)
	}
	questions := make([]*foundryv1.FoundryResearchQuestion, 0, len(research.Questions))
	for _, q := range research.Questions {
		questions = append(questions, &foundryv1.FoundryResearchQuestion{Id: q.ID, Question: q.Question, BestGuess: q.BestGuess, Options: q.Options})
	}
	return &foundryv1.ResearchFoundryCompanyResponse{Research: &foundryv1.FoundryCompanyResearch{
		Id: row.ID, Domain: domain, CompanyName: research.CompanyName, Summary: research.Summary, SuggestedName: research.SuggestedName,
		Questions: questions, Sources: research.Sources, Fetched: research.Fetched, CreatedAt: timestamppb.New(row.CreatedAt),
	}}, nil
}

// DraftFoundryGoal drafts a goal, and a name, from a research of the
// workspace's, the person's answers and their data: an owner or admin's,
// checked in this order: the caller; the research (NOT_FOUND unless the
// workspace's and ended with one); the answers and the data
// (INVALID_ARGUMENT); a model; the caller's hour and the workspace's month
// with the prompt; then the model's call, whose spend is recorded whatever
// came of it.
func (s *Service) DraftFoundryGoal(ctx context.Context, req *foundryv1.DraftFoundryGoalRequest) (*foundryv1.DraftFoundryGoalResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, researchDeadline)
	defer cancel()
	ws := req.GetWorkspaceId()
	userID, err := s.researchCallerOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	if req.GetResearchId() == "" {
		return nil, status.Error(codes.InvalidArgument, "research_id is required")
	}
	stored, err := s.deps.DB.GetFoundryCompanyResearch(ctx, ws, req.GetResearchId())
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "research not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the research: %v", err)
	}
	var research companyresearch.Research
	if err := json.Unmarshal(stored.Result, &research); err != nil {
		return nil, status.Errorf(codes.Internal, "read the research: %v", err)
	}
	given := make([]companyresearch.Answer, 0, len(req.GetAnswers()))
	for _, a := range req.GetAnswers() {
		given = append(given, companyresearch.Answer{QuestionID: a.GetQuestionId(), Text: a.GetAnswer()})
	}
	answers, err := research.Answered(given)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if utf8.RuneCountInString(req.GetData()) > companyresearch.MaxData {
		return nil, status.Errorf(codes.InvalidArgument, "data is longer than %d characters", companyresearch.MaxData)
	}
	if summary := strings.TrimSpace(req.GetSummary()); summary != "" {
		if utf8.RuneCountInString(summary) > companyresearch.MaxSummary {
			return nil, status.Errorf(codes.InvalidArgument, "summary is longer than %d characters", companyresearch.MaxSummary)
		}
		research.Summary = summary
	}
	m, err := s.researchModelOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	system, user := companyresearch.GoalPrompt(stored.Domain, research, answers, req.GetData())
	row := &foundrymodel.FoundryCompanyResearch{ID: id.New(), WorkspaceID: ws, UserID: userID, Kind: foundrymodel.FoundryCompanyResearchGoal, Domain: stored.Domain}
	if err := s.startResearch(ctx, row, m, len(system)+len(user)+len(companyresearch.GoalSchema), companyresearch.GoalMaxOutput); err != nil {
		return nil, err
	}
	goal, cost, failure := llm.CallTool(ctx, m.binding, researchRequest(system, user, companyresearch.GoalTool, companyresearch.GoalToolDescription,
		companyresearch.GoalSchema, companyresearch.GoalMaxOutput), companyresearch.GoalTool, researchCalls, researchRetryWait,
		func(raw json.RawMessage) (companyresearch.Goal, error) {
			return companyresearch.ParseGoal(raw, research.SuggestedName)
		})
	// A goal's row holds its spend alone: one that couldn't be recorded is
	// logged, its reservation still charged, and the goal paid for answered.
	_ = s.finishResearch(ctx, row, nil, cost, failure)
	if failure != "" {
		return nil, researchFailed(ws, m.resolved, "Foundry couldn’t write a goal", failure)
	}
	return &foundryv1.DraftFoundryGoalResponse{Goal: goal.Goal, Name: goal.Name}, nil
}

// researchCallerOf is the owner or admin calling for workspaceID, as
// themselves (personCallerOf): PERMISSION_DENIED for a member, a grantee
// or a service key. Where the host reads no roles, its front end has
// checked them.
func (s *Service) researchCallerOf(ctx context.Context, workspaceID string) (string, error) {
	c, err := s.personCallerOf(ctx, workspaceID)
	switch {
	case status.Code(err) == codes.PermissionDenied:
		// personCallerOf refuses anyone but a person as themselves: a
		// grantee or a service key.
		return "", status.Error(codes.PermissionDenied, "company research is for the workspace's owners and admins, signed in to Foundry as themselves")
	case err != nil:
		return "", err
	case s.deps.People != nil && !c.manager:
		return "", status.Error(codes.PermissionDenied, "only the workspace's owners and admins set up ontologies")
	}
	return c.userID, nil
}

// researchModelOf is the workspace's extraction model, its cheapest task
// model, refused as requireAI refuses.
func (s *Service) researchModelOf(ctx context.Context, workspaceID string) (researchModel, error) {
	if err := s.requireAI(ctx, workspaceID); err != nil {
		return researchModel{}, err
	}
	resolved, err := (&ai.Resolver{DB: s.deps.DB, Keyring: s.deps.ConnectorKeys, Deployment: s.deps.AI, Plans: s.deps.AIPlans}).Resolve(ctx, workspaceID)
	if err != nil {
		return researchModel{}, status.Errorf(codes.Unavailable, "open the workspace's model: %v", err)
	}
	models := s.deps.ResearchModels
	if models == nil {
		models = llm.NewFor
	}
	binding, err := models(resolved, s.deps.AI.Catalog, ai.RoleExtract)
	if err != nil {
		return researchModel{}, status.Errorf(codes.Unavailable, "open the workspace's model: %v", err)
	}
	return researchModel{binding: binding, resolved: resolved}, nil
}

// startResearch records row's call before it is made, reserving its worst
// case: every call at a prompt of promptBytes, as review advice estimates
// one, and maxOutput tokens. It is refused as researchRefusal says.
func (s *Service) startResearch(ctx context.Context, row *foundrymodel.FoundryCompanyResearch, m researchModel, promptBytes, maxOutput int) error {
	cfg := s.deps.Cfg.Foundry.Onboarding.Research
	now := time.Now().UTC()
	worst := researchCalls * m.binding.Pricer.Micros(llm.Usage{In: int64(promptBytes/researchCharsPerToken + 1), Out: int64(maxOutput)})
	return researchRefusal(row.WorkspaceID, cfg, s.deps.DB.StartFoundryCompanyResearch(ctx, row, foundrymodel.FoundryCompanyResearchLimits{
		PerUserPerHour: cfg.PerUserPerHour, Since: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC),
		WorstUSDMicros: worst, MonthlyMicros: cfg.MonthlyMicros(),
	}))
}

// researchRefusal is the API's answer to a refusal of the store's, nil for
// none: the caller's hour is RESOURCE_EXHAUSTED, the workspace's month
// FAILED_PRECONDITION, reason FOUNDRY_RESEARCH_BUDGET.
func researchRefusal(workspaceID string, cfg config.CompanyResearchConfig, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, foundrystore.ErrLimitExceeded):
		return status.Errorf(codes.ResourceExhausted, "you can research and draft %d times an hour: try again later, or write the goal yourself", cfg.PerUserPerHour)
	case errors.Is(err, foundrystore.ErrBudgetSpent):
		return foundryPrecondition(workspaceID, "FOUNDRY_RESEARCH_BUDGET",
			"this workspace has used this month’s allowance for company research: write the goal yourself, or try again next month")
	case errors.Is(err, foundrystore.ErrNotFound):
		return status.Error(codes.NotFound, "workspace not found")
	}
	return status.Errorf(codes.Unavailable, "record the research: %v", err)
}

// finishResearch records what row's call spent, and its result, with a
// bound of its own, so a call cut short by its caller still counts: a call
// out of time (failure llm.FailureTimeout) keeps its reservation unless it
// reported more. A record that fails is logged, the reservation charged.
func (s *Service) finishResearch(ctx context.Context, row *foundrymodel.FoundryCompanyResearch, result json.RawMessage, cost int64, failure string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishResearchTimeout)
	defer cancel()
	err := s.deps.DB.FinishFoundryCompanyResearch(ctx, row.WorkspaceID, row.ID, result,
		foundrymodel.FoundryCompanyResearchSpend{USDMicros: cost, CutShort: failure == llm.FailureTimeout})
	if err != nil {
		log.From(ctx).Error("record company research failed: the budget keeps its reservation", "err", err, "usd_micros", cost, "kind", row.Kind)
	}
	return err
}

// researchRequest asks for tool's one call.
func researchRequest(system, user, tool, description string, schema json.RawMessage, maxOutput int) llm.Request {
	return llm.Request{System: system, MaxOutput: maxOutput, Messages: []llm.Message{{Role: llm.User, Content: []llm.Block{llm.Text{Text: user}}}},
		Tools: []llm.Tool{{Name: tool, Description: description, InputSchema: schema}}}
}

// researchFailed is a model's failure (llm.CallTool's): UNAVAILABLE, reason
// FOUNDRY_RESEARCH_FAILED, naming the provider, and for one out of time
// how long it had.
func researchFailed(workspaceID string, r ai.Resolved, what, failure string) error {
	msg := fmt.Sprintf("%s: %s (%s). Try again, or skip this step.", what, failure, r.Label())
	if failure == llm.FailureTimeout {
		msg = fmt.Sprintf("%s: the model (%s) didn’t answer within %d seconds, which a slow or local model can need more than. Try again, or skip this step.",
			what, r.Label(), int(researchDeadline/time.Second))
	}
	st := status.New(codes.Unavailable, msg)
	detail, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason: "FOUNDRY_RESEARCH_FAILED", Domain: foundryDomain, Metadata: map[string]string{"workspace_id": workspaceID},
	})
	if err != nil {
		return st.Err()
	}
	return detail.Err()
}
