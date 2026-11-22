package api

import (
	"context"
	"slices"
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
// its syncs last read of them (one never read has none, and is stale, as is
// one no longer access.AudiencesFresh), and whom of the people it narrows
// (restricted members, and the members of teams with grants) it matches:
// those it has no account for by their verified email, and those it knows
// as guests. The source's own directory never leaves the API. Authz has
// checked the caller manages the workspace's members; grantees are refused
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
	connectors = slices.DeleteFunc(connectors, func(c *foundrymodel.Connector) bool {
		_, ok := connector.AudiencedKind(c.Kind)
		return !ok
	})
	resp := &foundryv1.ListFoundrySourceAudiencesResponse{}
	if len(connectors) == 0 {
		return resp, nil
	}
	stored, err := s.deps.DB.ListSourceAudiences(ctx, ws)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list source audiences: %v", err)
	}
	byConnector := make(map[string]foundrymodel.FoundrySourceAudience, len(stored))
	for _, a := range stored {
		byConnector[a.ConnectorID] = a
	}
	users, err := s.narrowedUsers(ctx, ws)
	if err != nil {
		return nil, err
	}
	emails := make([]connector.Email, len(users))
	for i, u := range users {
		if emails[i], err = s.sourceReads(ctx).Email(ctx, ws, u); err != nil {
			return nil, status.Errorf(codes.Internal, "read members' emails: %v", err)
		}
	}
	people, err := s.deps.DB.SourcePeopleByEmail(ctx, ws, emails)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "match members: %v", err)
	}
	now := time.Now()
	for _, c := range connectors {
		a := byConnector[c.ID]
		var problemAt *time.Time
		if a.Problem != nil {
			problemAt = &a.Problem.At
		}
		out := &foundryv1.FoundrySourceAudience{ConnectorId: c.ID, Kind: c.Kind, Stale: !access.AudiencesFresh(a.ReadAt, problemAt, now),
			EveryoneItems: int32(a.EveryoneItems), MembersItems: int32(a.MembersItems), Grantees: int32(len(users))}
		if a.ReadAt != nil {
			out.ReadAt = timestamppb.New(*a.ReadAt)
		}
		if a.Problem != nil {
			out.Problem = &foundryv1.FoundrySourceAudienceProblem{Message: a.Problem.Message, At: timestamppb.New(a.Problem.At)}
		}
		for i, u := range users {
			at := slices.IndexFunc(people[emails[i]], func(p foundrymodel.FoundrySourcePerson) bool { return p.ConnectorID == c.ID })
			switch {
			case at < 0:
				out.UnmatchedUserIds = append(out.UnmatchedUserIds, u)
			case people[emails[i]][at].Guest:
				out.GuestUserIds = append(out.GuestUserIds, u)
			}
		}
		resp.Sources = append(resp.Sources, out)
	}
	return resp, nil
}

// narrowedUsers are the users of the workspace whom sources' permissions
// narrow, sorted: its restricted members, and the members of its teams with
// grants.
func (s *Service) narrowedUsers(ctx context.Context, ws string) ([]string, error) {
	restricted, err := s.deps.DB.ListFoundryRestrictedMembers(ctx, ws)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read restricted members: %v", err)
	}
	grants, err := s.deps.DB.ListFoundryAccessGrants(ctx, ws, nil)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read grants: %v", err)
	}
	teams, err := s.deps.DB.ListFoundryTeams(ctx, ws)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read teams: %v", err)
	}
	users := slices.Clone(restricted)
	for _, t := range teams {
		if slices.ContainsFunc(grants, func(g foundrymodel.FoundryAccessGrant) bool {
			return g.Principal == access.Principal{Kind: access.PrincipalTeam, ID: t.ID}
		}) {
			users = append(users, t.MemberUserIDs...)
		}
	}
	slices.Sort(users)
	return slices.Compact(users), nil
}
