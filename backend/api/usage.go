package api

import (
	"context"
	"time"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// GetFoundryUsage returns the workspace's AI spend this UTC calendar month by
// task, with each task's cap, read as the caps read it: loads as the graph
// worker's budget (config.FoundryGraphConfig.MonthBudget, jobs created this
// month), API sources' extraction no person started as their allowance
// (config.FoundryConfig.SetAllowances, the workspace's share), and questions
// as the questions budget (spent, and what running ones reserve, charged
// this month). Own-key spend is in each sum, priced as the workspace's AI
// setting prices it: the caps count it too. Drafts record tokens, not
// dollars, and titles no spend at all, so neither has an entry. Authz has
// checked the caller may read the workspace.
func (s *Service) GetFoundryUsage(ctx context.Context, req *foundryv1.GetFoundryUsageRequest) (*foundryv1.FoundryUsageResponse, error) {
	ws := req.GetWorkspaceId()
	if ws == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	cfg := s.deps.Cfg.Foundry
	// One month for every sum, so a read across midnight UTC on the 1st
	// can't mix two.
	month := cfg.Graph.MonthBudget(time.Now())
	loads, err := s.deps.DB.SumFoundryGraphJobSpend(ctx, ws, month.Since)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sum loads spend: %v", err)
	}
	sources, err := s.deps.DB.SumFoundryDocumentSetSpend(ctx, ws, foundrymodel.ConnectorKindEvents, month.Since)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sum API sources spend: %v", err)
	}
	questions, err := s.deps.DB.SumFoundryQuestionSpend(ctx, ws, month.Since)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sum questions spend: %v", err)
	}
	sourcesCap := cfg.SetAllowances()[foundrymodel.ConnectorKindEvents].WorkspaceMicros
	questionsCap := cfg.Ask.MonthlyMicros()
	return &foundryv1.FoundryUsageResponse{Usage: &foundryv1.FoundryUsage{
		MonthStart: timestamppb.New(month.Since),
		ResetsAt:   timestamppb.New(month.Since.AddDate(0, 1, 0)),
		// API sources' spend is part of the loads'.
		TotalUsdMicros: loads + questions.SpentMicros,
		Tasks: []*foundryv1.FoundryUsageEntry{
			{Task: foundryv1.FoundryUsageTask_FOUNDRY_USAGE_TASK_LOADS, SpentUsdMicros: loads, CapUsdMicros: &month.CapMicros},
			{Task: foundryv1.FoundryUsageTask_FOUNDRY_USAGE_TASK_API_SOURCES, SpentUsdMicros: sources, CapUsdMicros: &sourcesCap},
			{
				Task: foundryv1.FoundryUsageTask_FOUNDRY_USAGE_TASK_QUESTIONS, SpentUsdMicros: questions.SpentMicros,
				ReservedUsdMicros: questions.ReservedMicros, CapUsdMicros: &questionsCap,
			},
		},
	}}, nil
}
