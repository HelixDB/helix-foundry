package api

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/connector"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// ListFoundrySourceAudiences returns each live connection whose kind follows
// its source's permissions (connector.Audienced), oldest first, with what
// its syncs last read of them; one never read has none, and is stale, as
// one read longer ago than access.MaxAudienceAge is. Authz has checked the
// caller manages the workspace's members; grantees are refused
// (access.Deny).
func (s *Service) ListFoundrySourceAudiences(ctx context.Context, req *foundryv1.ListFoundrySourceAudiencesRequest) (*foundryv1.ListFoundrySourceAudiencesResponse, error) {
	ws := req.GetWorkspaceId()
	if ws == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	connectors, err := s.deps.DB.ListConnectors(ctx, ws)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list connectors: %v", err)
	}
	stored, err := s.deps.DB.ListSourceAudiences(ctx, ws)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list source audiences: %v", err)
	}
	byConnector := make(map[string]foundrymodel.FoundrySourceAudience, len(stored))
	for _, a := range stored {
		byConnector[a.ConnectorID] = a
	}
	now := time.Now()
	resp := &foundryv1.ListFoundrySourceAudiencesResponse{}
	for _, c := range connectors {
		if _, ok := connector.AudiencedKind(c.Kind); !ok {
			continue
		}
		a := byConnector[c.ID]
		out := &foundryv1.FoundrySourceAudience{ConnectorId: c.ID, Kind: c.Kind, Stale: !access.ReachOf(a.ReadAt, nil, 0, now).Known(),
			EveryoneItems: int32(a.EveryoneItems), MembersItems: int32(a.MembersItems)}
		if a.ReadAt != nil {
			out.ReadAt = timestamppb.New(*a.ReadAt)
		}
		if a.Problem != nil {
			out.Problem = &foundryv1.FoundrySourceAudienceProblem{Message: a.Problem.Message, At: timestamppb.New(a.Problem.At)}
		}
		for _, p := range a.People {
			out.People = append(out.People, &foundryv1.FoundrySourcePerson{Email: p.Email, Guest: p.Guest})
		}
		resp.Sources = append(resp.Sources, out)
	}
	return resp, nil
}
