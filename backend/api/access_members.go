package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/access"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// maxFoundryTeams bounds a workspace's teams, and maxFoundryTeamMembers
	// a team's members.
	maxFoundryTeams       = 100
	maxFoundryTeamMembers = 1000
	maxFoundryTeamName    = 120
)

// restrictable refuses a member grants can't confine: an owner or admin sees
// everything, and a member with raw access reads the databases outside Foundry.
func (s *Service) restrictable(ctx context.Context, workspaceID, userID string) error {
	m, err := s.deps.People.Member(ctx, workspaceID, userID)
	switch {
	case err != nil:
		return status.Errorf(codes.Internal, "read the member: %v", err)
	case m.Role == ports.RoleNone:
		return status.Error(codes.NotFound, "member not found")
	case m.Role != ports.RoleMember:
		return status.Error(codes.FailedPrecondition, "only a member can be restricted: owners and admins see everything")
	case m.RawAccess:
		return status.Error(codes.FailedPrecondition,
			"this member can write a project or query its databases outside Foundry; take that access away in the Helix dashboard first")
	}
	return nil
}

// teamRestrictable refuses a team's grants while one of these members has raw
// access. Owners and admins may sit on a team; they stay as they are.
func (s *Service) teamRestrictable(ctx context.Context, workspaceID string, userIDs []string) error {
	if s.deps.People == nil {
		return nil
	}
	for _, userID := range userIDs {
		m, err := s.deps.People.Member(ctx, workspaceID, userID)
		if err != nil {
			return status.Errorf(codes.Internal, "read the member: %v", err)
		}
		if m.Role == ports.RoleMember && m.RawAccess {
			return status.Error(codes.FailedPrecondition,
				"a member of this team can write a project or query its databases outside Foundry; take that access away in the Helix dashboard, or take them off the team, first")
		}
	}
	return nil
}

// SetFoundryMemberRestricted restricts a plain member to what grants give, or
// lifts it. A member holding sources, their own or a team's, stays restricted
// until those are gone: holding them is what restricts. Authz has checked the
// caller manages the workspace's members.
func (s *Service) SetFoundryMemberRestricted(ctx context.Context, req *foundryv1.SetFoundryMemberRestrictedRequest) (*foundryv1.SetFoundryMemberRestrictedResponse, error) {
	if req.GetWorkspaceId() == "" || req.GetUserId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id and user_id are required")
	}
	if req.GetRestricted() {
		if s.deps.People == nil {
			return nil, status.Error(codes.FailedPrecondition, "this install has no members to restrict")
		}
		if err := s.restrictable(ctx, req.GetWorkspaceId(), req.GetUserId()); err != nil {
			return nil, err
		}
	}
	switch err := s.deps.DB.SetFoundryMemberRestricted(ctx, req.GetWorkspaceId(), req.GetUserId(), req.GetRestricted(), byUser(ctx)); {
	case errors.Is(err, foundrystore.ErrConflict):
		return nil, status.Error(codes.FailedPrecondition, "this member holds sources, their own or a team's; take those away first")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "set the member's restriction: %v", err)
	}
	return &foundryv1.SetFoundryMemberRestrictedResponse{Restricted: req.GetRestricted()}, nil
}

// CreateFoundryTeam makes a team of the workspace's members. Authz has
// checked the caller manages the workspace's members.
func (s *Service) CreateFoundryTeam(ctx context.Context, req *foundryv1.CreateFoundryTeamRequest) (*foundryv1.FoundryTeamResponse, error) {
	name, members, err := s.foundryTeam(ctx, req.GetWorkspaceId(), req.GetName(), req.GetMemberUserIds())
	if err != nil {
		return nil, err
	}
	teams, err := s.deps.DB.ListFoundryTeams(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read teams: %v", err)
	}
	if len(teams) >= maxFoundryTeams {
		return nil, status.Errorf(codes.FailedPrecondition, "a workspace has at most %d teams", maxFoundryTeams)
	}
	team := &foundrymodel.FoundryTeam{ID: id.New(), WorkspaceID: req.GetWorkspaceId(), Name: name, MemberUserIDs: members, CreatedByUserID: byUser(ctx)}
	switch err := s.deps.DB.CreateFoundryTeam(ctx, team); {
	case errors.Is(err, foundrystore.ErrConflict):
		return nil, status.Error(codes.AlreadyExists, "the workspace has a team of that name")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "create team: %v", err)
	}
	return s.foundryTeamResponse(ctx, req.GetWorkspaceId(), team.ID)
}

// UpdateFoundryTeam renames a team and replaces its members. Whoever joins a
// team holding grants is restricted to them, and whoever leaves stays
// restricted (the store does both). Authz has checked the caller manages the
// workspace's members.
func (s *Service) UpdateFoundryTeam(ctx context.Context, req *foundryv1.UpdateFoundryTeamRequest) (*foundryv1.FoundryTeamResponse, error) {
	name, members, err := s.foundryTeam(ctx, req.GetWorkspaceId(), req.GetName(), req.GetMemberUserIds())
	if err != nil {
		return nil, err
	}
	held, err := s.deps.DB.ListFoundryAccessGrants(ctx, req.GetWorkspaceId(), []access.Principal{{Kind: access.PrincipalTeam, ID: req.GetTeamId()}})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the team's grants: %v", err)
	}
	if len(held) > 0 {
		// Only who joins: someone given raw access since doesn't stop a rename.
		teams, err := s.deps.DB.ListFoundryTeams(ctx, req.GetWorkspaceId())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read teams: %v", err)
		}
		var had []string
		if i := slices.IndexFunc(teams, func(t foundrymodel.FoundryTeam) bool { return t.ID == req.GetTeamId() }); i >= 0 {
			had = teams[i].MemberUserIDs
		}
		joining := slices.DeleteFunc(slices.Clone(members), func(userID string) bool { return slices.Contains(had, userID) })
		if err := s.teamRestrictable(ctx, req.GetWorkspaceId(), joining); err != nil {
			return nil, err
		}
	}
	switch err := s.deps.DB.UpdateFoundryTeam(ctx, req.GetWorkspaceId(), req.GetTeamId(), name, members); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "team not found")
	case errors.Is(err, foundrystore.ErrConflict):
		return nil, status.Error(codes.AlreadyExists, "the workspace has a team of that name")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "update team: %v", err)
	}
	return s.foundryTeamResponse(ctx, req.GetWorkspaceId(), req.GetTeamId())
}

// DeleteFoundryTeam deletes a team and its grants. Authz has checked the
// caller manages the workspace's members.
func (s *Service) DeleteFoundryTeam(ctx context.Context, req *foundryv1.DeleteFoundryTeamRequest) (*emptypb.Empty, error) {
	if req.GetWorkspaceId() == "" || req.GetTeamId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id and team_id are required")
	}
	switch err := s.deps.DB.DeleteFoundryTeam(ctx, req.GetWorkspaceId(), req.GetTeamId()); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "team not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "delete team: %v", err)
	}
	return &emptypb.Empty{}, nil
}

// foundryTeam checks a team's name and members: a trimmed name of 1 to 120
// characters, and at most 1,000 members, each one of the workspace's,
// once each.
func (s *Service) foundryTeam(ctx context.Context, workspaceID, name string, members []string) (string, []string, error) {
	name = strings.TrimSpace(name)
	switch {
	case workspaceID == "":
		return "", nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	case name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > maxFoundryTeamName:
		return "", nil, status.Errorf(codes.InvalidArgument, "a team's name is 1 to %d characters", maxFoundryTeamName)
	case len(members) > maxFoundryTeamMembers:
		return "", nil, status.Errorf(codes.InvalidArgument, "a team has at most %d members", maxFoundryTeamMembers)
	case len(members) > 0 && s.deps.People == nil:
		return "", nil, status.Error(codes.FailedPrecondition, "this install has no members to put in a team")
	}
	var out []string
	for _, userID := range members {
		if slices.Contains(out, userID) {
			continue
		}
		m, err := s.deps.People.Member(ctx, workspaceID, userID)
		if err != nil {
			return "", nil, status.Errorf(codes.Internal, "read the member: %v", err)
		}
		if m.Role == ports.RoleNone {
			return "", nil, status.Errorf(codes.InvalidArgument, "%s is not a member of the workspace", userID)
		}
		out = append(out, userID)
	}
	return name, out, nil
}

func (s *Service) foundryTeamResponse(ctx context.Context, workspaceID, teamID string) (*foundryv1.FoundryTeamResponse, error) {
	teams, err := s.deps.DB.ListFoundryTeams(ctx, workspaceID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read teams: %v", err)
	}
	for _, t := range teams {
		if t.ID == teamID {
			return &foundryv1.FoundryTeamResponse{Team: teamToProto(t)}, nil
		}
	}
	return nil, status.Error(codes.NotFound, "team not found")
}

func teamToProto(t foundrymodel.FoundryTeam) *foundryv1.FoundryTeam {
	out := &foundryv1.FoundryTeam{Id: t.ID, Name: t.Name, MemberUserIds: t.MemberUserIDs, CreatedAt: timestamppb.New(t.CreatedAt)}
	if t.CreatedByUserID != nil {
		out.CreatedByUserId = *t.CreatedByUserID
	}
	return out
}

// byUser is the calling user, nil for no user.
func byUser(ctx context.Context) *string {
	if userID := actor(ctx).UserID; userID != "" {
		return &userID
	}
	return nil
}
