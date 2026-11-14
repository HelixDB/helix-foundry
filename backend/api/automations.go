package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/automation"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// Automations (package automation). Who may do what:
//   - Every member sees every automation of the workspace, and creates their
//     own: an automation runs as its owner, with what they may do then, so
//     making one gives nobody anything.
//   - Only its owner edits, approves, resumes and runs one: it acts as them.
//     An edit to its spec stops it until they approve it again.
//   - Approving is the owner's click in the site, never an agent's call:
//     ApproveFoundryAutomation takes a person signed in to the site
//     (ports.SourceUser), and no MCP tool calls it. So an automation an agent
//     creates or edits through the MCP waits for its owner; one the
//     Assistant prepared runs once its person clicks Run on the card, which
//     approves the spec the card showed (ExecuteFoundryAgentAction).
//   - Its owner, or an owner or admin, pauses, deletes and cancels a run of
//     one: stopping work is everyone's who answers for the workspace.
//   - A grantee of source grants, a service key and an agent see none.

const automationRunsPage = 50

// automationCaller is who calls an automation's route.
type automationCaller struct {
	userID string
	// manager is an owner or admin of the workspace.
	manager bool
	// site is a person signed in to the site, not an agent through the MCP:
	// only they approve.
	site bool
}

func (c automationCaller) actor() foundrymodel.FoundryAutomationActor {
	return foundrymodel.FoundryAutomationActor{UserID: c.userID, Manager: c.manager}
}

// automationCallerOf is the person calling for workspaceID, while
// automations are on: FAILED_PRECONDITION, reason
// FOUNDRY_AUTOMATIONS_DISABLED, when off.
func (s *Service) automationCallerOf(ctx context.Context, workspaceID string) (automationCaller, error) {
	switch {
	case workspaceID == "":
		return automationCaller{}, status.Error(codes.InvalidArgument, "workspace_id is required")
	case !s.deps.Cfg.Foundry.Automations.Enabled:
		return automationCaller{}, foundryPrecondition(workspaceID, "FOUNDRY_AUTOMATIONS_DISABLED", "automations aren't available here")
	}
	if _, ok := access.GranteeFrom(ctx); ok {
		return automationCaller{}, status.Error(codes.PermissionDenied, "automations are people's: sign in to Foundry")
	}
	a := actor(ctx)
	if (a.Source != ports.SourceUser && a.Source != ports.SourceMCP) || a.UserID == "" || a.ServiceCredentialID != "" {
		return automationCaller{}, status.Error(codes.PermissionDenied, "automations are people's: sign in to Foundry")
	}
	c := automationCaller{userID: a.UserID, site: a.Source == ports.SourceUser}
	if s.deps.People != nil {
		m, err := s.deps.People.Member(ctx, workspaceID, a.UserID)
		if err != nil {
			return automationCaller{}, status.Errorf(codes.Unavailable, "read your role: %v", err)
		}
		c.manager = m.Role == ports.RoleOwner || m.Role == ports.RoleAdmin
	}
	return c, nil
}

// automationRefusal is the API's answer to a refusal of the store's; false
// when err is none.
func automationRefusal(workspaceID string, err error) (error, bool) {
	var r foundrymodel.FoundryAutomationRefusal
	if !errors.As(err, &r) {
		return nil, false
	}
	switch r.Reason {
	case foundrymodel.FoundryAutomationNotOwner, foundrymodel.FoundryAutomationNotAllowed:
		return status.Error(codes.PermissionDenied, r.Detail), true
	case foundrymodel.FoundryAutomationRunsPerDay:
		return status.Error(codes.ResourceExhausted, r.Detail), true
	}
	reason := map[foundrymodel.FoundryAutomationRefusalReason]string{
		foundrymodel.FoundryAutomationChanged:    "FOUNDRY_AUTOMATION_CHANGED",
		foundrymodel.FoundryAutomationLimit:      "FOUNDRY_AUTOMATION_LIMIT",
		foundrymodel.FoundryAutomationUnapproved: "FOUNDRY_AUTOMATION_UNAPPROVED",
		foundrymodel.FoundryAutomationBusy:       "FOUNDRY_AUTOMATION_BUSY",
		foundrymodel.FoundryAutomationNotActive:  "FOUNDRY_AUTOMATION_NOT_ACTIVE",
		foundrymodel.FoundryAutomationRunEnded:   "FOUNDRY_AUTOMATION_RUN_ENDED",
	}[r.Reason]
	return foundryPrecondition(workspaceID, reason, r.Detail), true
}

// automationWrite answers a store write's error: a refusal, not found, or internal.
func automationWrite(workspaceID, what string, err error) error {
	if refused, ok := automationRefusal(workspaceID, err); ok {
		return refused
	}
	if errors.Is(err, foundrystore.ErrNotFound) {
		return status.Error(codes.NotFound, "automation not found")
	}
	return status.Errorf(codes.Internal, "%s: %v", what, err)
}

// automationSpec checks a spec of the workspace: its shape (package
// automation), and that every connector, ontology, document set and
// dashboard it names is the workspace's, so an automation never names what
// another workspace holds. It returns the spec as the store keeps it.
func (s *Service) automationSpec(ctx context.Context, workspaceID, raw string) (automation.Spec, *foundrymodel.FoundryAutomationSpec, error) {
	spec, err := automation.ParseSpec([]byte(raw))
	if err != nil {
		return automation.Spec{}, nil, status.Error(codes.InvalidArgument, "spec_json: "+err.Error())
	}
	targets := spec.Targets()
	for _, check := range []struct {
		ids  []string
		what string
		read func(string) error
	}{
		{targets.Connectors, "connector", func(id string) error { _, err := s.deps.DB.GetConnector(ctx, workspaceID, id); return err }},
		{targets.Ontologies, "ontology", func(id string) error {
			o, err := s.deps.DB.GetFoundryOntology(ctx, workspaceID, id)
			if err == nil && o.DeletedAt != nil {
				return foundrystore.ErrNotFound
			}
			return err
		}},
		{targets.Sets, "document set", func(id string) error { _, err := s.deps.DB.GetFoundryDocumentSet(ctx, workspaceID, id); return err }},
		{targets.Dashboards, "dashboard", func(id string) error { _, err := s.deps.DB.GetFoundryDashboard(ctx, workspaceID, id); return err }},
	} {
		for _, target := range check.ids {
			switch err := check.read(target); {
			case errors.Is(err, foundrystore.ErrNotFound):
				return automation.Spec{}, nil, status.Errorf(codes.InvalidArgument, "spec_json names the %s %s, which this workspace hasn't", check.what, target)
			case err != nil:
				return automation.Spec{}, nil, status.Errorf(codes.Internal, "read the %s %s: %v", check.what, target, err)
			}
		}
	}
	canonical, err := json.Marshal(spec)
	if err != nil {
		return automation.Spec{}, nil, status.Errorf(codes.Internal, "write the spec: %v", err)
	}
	stored := &foundrymodel.FoundryAutomationSpec{JSON: canonical, TriggerKind: string(automation.KindOf(spec.Trigger()))}
	if target := automation.Target(spec.Trigger()); target != "" {
		stored.TriggerTarget = &target
	}
	return spec, stored, nil
}

// firstFire is when an automation of spec, made active now, fires first:
// its schedule's next time; nil for any other trigger.
func firstFire(spec automation.Spec, now time.Time) *time.Time {
	schedule, ok := spec.Trigger().(automation.Schedule)
	if !ok {
		return nil
	}
	next := schedule.Next(now).UTC()
	return &next
}

func (s *Service) ListFoundryAutomations(ctx context.Context, req *foundryv1.ListFoundryAutomationsRequest) (*foundryv1.ListFoundryAutomationsResponse, error) {
	c, err := s.automationCallerOf(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	list, err := s.deps.DB.ListFoundryAutomations(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list automations: %v", err)
	}
	out := &foundryv1.ListFoundryAutomationsResponse{CanCreate: true, Email: !s.deps.Cfg.Host.Local,
		Automations: make([]*foundryv1.FoundryAutomation, 0, len(list))}
	for _, a := range list {
		out.Automations = append(out.Automations, automationToProto(a, c, false))
	}
	return out, nil
}

// automationOf is the workspace's automation, NotFound for none.
func (s *Service) automationOf(ctx context.Context, workspaceID, id string) (*foundrymodel.FoundryAutomation, error) {
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	a, err := s.deps.DB.GetFoundryAutomation(ctx, workspaceID, id)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "automation not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "read the automation: %v", err)
	}
	return a, nil
}

func (s *Service) GetFoundryAutomation(ctx context.Context, req *foundryv1.GetFoundryAutomationRequest) (*foundryv1.FoundryAutomationResponse, error) {
	c, err := s.automationCallerOf(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	a, err := s.automationOf(ctx, req.GetWorkspaceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	return &foundryv1.FoundryAutomationResponse{Automation: automationToProto(a, c, true)}, nil
}

func (s *Service) CreateFoundryAutomation(ctx context.Context, req *foundryv1.CreateFoundryAutomationRequest) (*foundryv1.FoundryAutomationResponse, error) {
	ws := req.GetWorkspaceId()
	c, err := s.automationCallerOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	token := req.GetClientToken()
	if token == "" || len(token) > maxClientTokenLength {
		return nil, status.Errorf(codes.InvalidArgument, "client_token is required, at most %d characters", maxClientTokenLength)
	}
	name, description, err := nameAndDescription(req.GetName(), req.GetDescription())
	if err != nil {
		return nil, err
	}
	existing, err := s.deps.DB.GetFoundryAutomationByClientToken(ctx, ws, c.userID, token)
	switch {
	case err == nil:
		return &foundryv1.FoundryAutomationResponse{Automation: automationToProto(existing, c, true)}, nil
	case !errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Errorf(codes.Internal, "look up the automation: %v", err)
	}
	_, spec, err := s.automationSpec(ctx, ws, req.GetSpecJson())
	if err != nil {
		return nil, err
	}
	a := &foundrymodel.FoundryAutomation{ID: id.New(), WorkspaceID: ws, OwnerUserID: c.userID, Name: name, Description: description,
		Spec: spec.JSON, TriggerKind: spec.TriggerKind, TriggerTarget: spec.TriggerTarget, ClientToken: &token}
	err = s.deps.DB.CreateFoundryAutomation(ctx, a, s.deps.Cfg.Foundry.Automations.MaxPerWorkspace)
	if errors.Is(err, foundrystore.ErrConflict) {
		// The same click again won the race.
		raced, lookupErr := s.deps.DB.GetFoundryAutomationByClientToken(ctx, ws, c.userID, token)
		if lookupErr != nil {
			return nil, status.Errorf(codes.Internal, "look up the automation: %v", lookupErr)
		}
		return &foundryv1.FoundryAutomationResponse{Automation: automationToProto(raced, c, true)}, nil
	}
	if err != nil {
		return nil, automationWrite(ws, "create the automation", err)
	}
	stored, err := s.automationOf(ctx, ws, a.ID)
	if err != nil {
		return nil, err
	}
	return &foundryv1.FoundryAutomationResponse{Automation: automationToProto(stored, c, true)}, nil
}

func (s *Service) UpdateFoundryAutomation(ctx context.Context, req *foundryv1.UpdateFoundryAutomationRequest) (*foundryv1.FoundryAutomationResponse, error) {
	ws := req.GetWorkspaceId()
	c, err := s.automationCallerOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	if req.GetId() == "" || req.GetExpectedRevision() < 1 {
		return nil, status.Error(codes.InvalidArgument, "id and expected_revision are required")
	}
	var u foundrymodel.FoundryAutomationUpdate
	if req.Name != nil || req.Description != nil {
		current, err := s.automationOf(ctx, ws, req.GetId())
		if err != nil {
			return nil, err
		}
		name, description := current.Name, current.Description
		if req.Name != nil {
			name = req.GetName()
		}
		if req.Description != nil {
			description = req.GetDescription()
		}
		if name, description, err = nameAndDescription(name, description); err != nil {
			return nil, err
		}
		if req.Name != nil {
			u.Name = &name
		}
		if req.Description != nil {
			u.Description = &description
		}
	}
	if req.SpecJson != nil {
		var spec automation.Spec
		if spec, u.Spec, err = s.automationSpec(ctx, ws, req.GetSpecJson()); err != nil {
			return nil, err
		}
		// Only its positions moved: compared with the revision the edit names,
		// which the store checks it still is, it keeps its approval.
		current, err := s.automationOf(ctx, ws, req.GetId())
		if err != nil {
			return nil, err
		}
		if before, err := automation.ParseSpec(current.Spec); err == nil && current.Revision == req.GetExpectedRevision() {
			u.Spec.LayoutOnly = automation.SameSteps(before, spec)
		}
	}
	if u.Name == nil && u.Description == nil && u.Spec == nil {
		return nil, status.Error(codes.InvalidArgument, "set at least one of name, description and spec_json")
	}
	a, err := s.deps.DB.UpdateFoundryAutomation(ctx, ws, req.GetId(), c.userID, req.GetExpectedRevision(), u)
	if err != nil {
		return nil, automationWrite(ws, "update the automation", err)
	}
	return &foundryv1.FoundryAutomationResponse{Automation: automationToProto(a, c, true)}, nil
}

func (s *Service) DeleteFoundryAutomation(ctx context.Context, req *foundryv1.DeleteFoundryAutomationRequest) (*emptypb.Empty, error) {
	c, err := s.automationCallerOf(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	if err := s.deps.DB.DeleteFoundryAutomation(ctx, req.GetWorkspaceId(), req.GetId(), c.actor()); err != nil {
		return nil, automationWrite(req.GetWorkspaceId(), "delete the automation", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Service) ApproveFoundryAutomation(ctx context.Context, req *foundryv1.ApproveFoundryAutomationRequest) (*foundryv1.FoundryAutomationResponse, error) {
	ws := req.GetWorkspaceId()
	c, err := s.automationCallerOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	if !c.site {
		return nil, status.Error(codes.PermissionDenied, "an automation's owner approves it in Foundry, never through an agent")
	}
	if req.GetExpectedRevision() < 1 {
		return nil, status.Error(codes.InvalidArgument, "expected_revision is required")
	}
	a, err := s.approveAutomation(ctx, ws, c.userID, req.GetId(), req.GetExpectedRevision())
	if err != nil {
		return nil, err
	}
	return &foundryv1.FoundryAutomationResponse{Automation: automationToProto(a, c, true)}, nil
}

// approveAutomation is userID's approval of the automation's spec at
// revision: its targets checked again, as what it names may be gone since it
// was saved, then made active. Only a person's own click reaches it: the
// Approve route from the site, or their Run on an Assistant's card.
func (s *Service) approveAutomation(ctx context.Context, workspaceID, userID, id string, revision int64) (*foundrymodel.FoundryAutomation, error) {
	current, err := s.automationOf(ctx, workspaceID, id)
	if err != nil {
		return nil, err
	}
	spec, _, err := s.automationSpec(ctx, workspaceID, string(current.Spec))
	if err != nil {
		return nil, err
	}
	a, err := s.deps.DB.ApproveFoundryAutomation(ctx, workspaceID, id, userID, revision, firstFire(spec, time.Now()))
	if err != nil {
		return nil, automationWrite(workspaceID, "approve the automation", err)
	}
	return a, nil
}

func (s *Service) PauseFoundryAutomation(ctx context.Context, req *foundryv1.PauseFoundryAutomationRequest) (*foundryv1.FoundryAutomationResponse, error) {
	c, err := s.automationCallerOf(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	a, err := s.deps.DB.PauseFoundryAutomation(ctx, req.GetWorkspaceId(), req.GetId(), c.actor())
	if err != nil {
		return nil, automationWrite(req.GetWorkspaceId(), "pause the automation", err)
	}
	return &foundryv1.FoundryAutomationResponse{Automation: automationToProto(a, c, true)}, nil
}

func (s *Service) ResumeFoundryAutomation(ctx context.Context, req *foundryv1.ResumeFoundryAutomationRequest) (*foundryv1.FoundryAutomationResponse, error) {
	ws := req.GetWorkspaceId()
	c, err := s.automationCallerOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	current, err := s.automationOf(ctx, ws, req.GetId())
	if err != nil {
		return nil, err
	}
	spec, err := automation.ParseSpec(current.Spec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the automation's spec: %v", err)
	}
	a, err := s.deps.DB.ResumeFoundryAutomation(ctx, ws, req.GetId(), c.userID, firstFire(spec, time.Now()))
	if err != nil {
		return nil, automationWrite(ws, "resume the automation", err)
	}
	return &foundryv1.FoundryAutomationResponse{Automation: automationToProto(a, c, true)}, nil
}

func (s *Service) RunFoundryAutomation(ctx context.Context, req *foundryv1.RunFoundryAutomationRequest) (*foundryv1.FoundryAutomationRunResponse, error) {
	ws := req.GetWorkspaceId()
	c, err := s.automationCallerOf(ctx, ws)
	if err != nil {
		return nil, err
	}
	token := req.GetClientToken()
	if token == "" || len(token) > maxClientTokenLength {
		return nil, status.Errorf(codes.InvalidArgument, "client_token is required, at most %d characters", maxClientTokenLength)
	}
	a, err := s.automationOf(ctx, ws, req.GetId())
	if err != nil {
		return nil, err
	}
	spec, err := automation.ParseSpec(a.Spec)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the automation's spec: %v", err)
	}
	now := time.Now().UTC()
	event, _ := json.Marshal(foundrymodel.FoundryAutomationEvent{Kind: "manual", At: now, ByUserID: c.userID}) // plain fields: it can't fail
	output, _ := json.Marshal(automation.Output{Summary: "Run by hand."})
	run, err := s.deps.DB.RunFoundryAutomationNow(ctx, &foundrymodel.FoundryAutomationRun{
		ID: id.New(), AutomationID: a.ID, WorkspaceID: ws, OwnerUserID: c.userID, TriggerKey: "manual:" + token, TriggerEvent: event,
	}, spec.TriggerID(), output, foundrymodel.FoundryAutomationLimits{RunsPerDay: s.deps.Cfg.Foundry.Automations.RunsPerDay})
	if err != nil {
		return nil, automationWrite(ws, "run the automation", err)
	}
	return &foundryv1.FoundryAutomationRunResponse{Run: automationRunToProto(run, a, c, nil)}, nil
}

func (s *Service) ListFoundryAutomationRuns(ctx context.Context, req *foundryv1.ListFoundryAutomationRunsRequest) (*foundryv1.ListFoundryAutomationRunsResponse, error) {
	c, err := s.automationCallerOf(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	a, err := s.automationOf(ctx, req.GetWorkspaceId(), req.GetAutomationId())
	if err != nil {
		return nil, err
	}
	runs, next, err := s.deps.DB.ListFoundryAutomationRuns(ctx, a.ID, req.GetPageToken(), automationRunsPage)
	switch {
	case errors.Is(err, foundrystore.ErrInvalidPageToken):
		return nil, status.Error(codes.InvalidArgument, "page_token is not one this list returned")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "list runs: %v", err)
	}
	out := &foundryv1.ListFoundryAutomationRunsResponse{NextPageToken: next}
	for _, r := range runs {
		out.Runs = append(out.Runs, automationRunToProto(r, a, c, nil))
	}
	return out, nil
}

// automationRunOf is the run of the workspace's automation, NotFound for none.
func (s *Service) automationRunOf(ctx context.Context, workspaceID, automationID, id string) (*foundrymodel.FoundryAutomation, *foundrymodel.FoundryAutomationRun, error) {
	a, err := s.automationOf(ctx, workspaceID, automationID)
	if err != nil {
		return nil, nil, err
	}
	r, err := s.deps.DB.GetFoundryAutomationRun(ctx, workspaceID, id)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || err == nil && r.AutomationID != a.ID:
		return nil, nil, status.Error(codes.NotFound, "run not found")
	case err != nil:
		return nil, nil, status.Errorf(codes.Internal, "read the run: %v", err)
	}
	return a, r, nil
}

func (s *Service) GetFoundryAutomationRun(ctx context.Context, req *foundryv1.GetFoundryAutomationRunRequest) (*foundryv1.FoundryAutomationRunResponse, error) {
	c, err := s.automationCallerOf(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	a, r, err := s.automationRunOf(ctx, req.GetWorkspaceId(), req.GetAutomationId(), req.GetId())
	if err != nil {
		return nil, err
	}
	nodes, err := s.deps.DB.ListFoundryAutomationRunNodes(ctx, r.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list the run's steps: %v", err)
	}
	return &foundryv1.FoundryAutomationRunResponse{Run: automationRunToProto(r, a, c, nodes)}, nil
}

func (s *Service) CancelFoundryAutomationRun(ctx context.Context, req *foundryv1.CancelFoundryAutomationRunRequest) (*foundryv1.FoundryAutomationRunResponse, error) {
	c, err := s.automationCallerOf(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	a, _, err := s.automationRunOf(ctx, req.GetWorkspaceId(), req.GetAutomationId(), req.GetId())
	if err != nil {
		return nil, err
	}
	r, err := s.deps.DB.CancelFoundryAutomationRun(ctx, req.GetWorkspaceId(), req.GetId(), c.actor())
	if err != nil {
		return nil, automationWrite(req.GetWorkspaceId(), "cancel the run", err)
	}
	nodes, err := s.deps.DB.ListFoundryAutomationRunNodes(ctx, r.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list the run's steps: %v", err)
	}
	return &foundryv1.FoundryAutomationRunResponse{Run: automationRunToProto(r, a, c, nodes)}, nil
}

// automationToProto is a as c sees it, its spec only withSpec.
func automationToProto(a *foundrymodel.FoundryAutomation, c automationCaller, withSpec bool) *foundryv1.FoundryAutomation {
	owner := a.OwnerUserID == c.userID
	out := &foundryv1.FoundryAutomation{
		Id: a.ID, Name: a.Name, Description: a.Description, Revision: a.Revision, SpecRevision: a.SpecRevision, OwnerUserId: a.OwnerUserID,
		Status: string(a.Status), TriggerKind: a.TriggerKind, Failures: int32(a.Failures), CreatedAt: timestamppb.New(a.CreatedAt),
		UpdatedAt: timestamppb.New(a.UpdatedAt),
		CanEdit:   owner, CanApprove: owner && c.site && a.Status == foundrymodel.FoundryAutomationNeedsApproval,
		CanPause:  (owner || c.manager) && a.Status == foundrymodel.FoundryAutomationActive,
		CanResume: owner && a.Status == foundrymodel.FoundryAutomationPaused,
		CanRun:    owner && a.Status != foundrymodel.FoundryAutomationNeedsApproval && (a.LastRun == nil || !a.LastRun.Status.Live()),
		CanDelete: owner || c.manager,
	}
	if withSpec {
		out.SpecJson = string(a.Spec)
	}
	if a.TriggerTarget != nil {
		out.TriggerTarget = *a.TriggerTarget
	}
	if a.PausedReason != nil {
		out.PausedReason = string(*a.PausedReason)
	}
	if a.PausedByUserID != nil {
		out.PausedByUserId = *a.PausedByUserID
	}
	if a.ApprovedSpecRevision != nil {
		out.ApprovedSpecRevision = *a.ApprovedSpecRevision
	}
	for _, t := range []struct {
		src *time.Time
		dst **timestamppb.Timestamp
	}{{a.PausedAt, &out.PausedAt}, {a.ApprovedAt, &out.ApprovedAt}, {a.NextFireAt, &out.NextRunAt}, {a.BackoffUntil, &out.BackoffUntil}} {
		if t.src != nil {
			*t.dst = timestamppb.New(*t.src)
		}
	}
	if a.LastRun != nil {
		out.LastRun = automationRunToProto(a.LastRun, a, c, nil)
	}
	return out
}

// automationRunToProto is r, of a, as c sees it, with its steps when given.
func automationRunToProto(r *foundrymodel.FoundryAutomationRun, a *foundrymodel.FoundryAutomation, c automationCaller,
	nodes []*foundrymodel.FoundryAutomationRunNode) *foundryv1.FoundryAutomationRun {
	out := &foundryv1.FoundryAutomationRun{
		Id: r.ID, AutomationId: r.AutomationID, Status: string(r.Status), TriggerEventJson: string(r.TriggerEvent), SpecRevision: r.SpecRevision,
		SpecJson: string(r.Spec), UsdMicros: r.USDMicros, Attempts: int32(r.Attempts), CreatedAt: timestamppb.New(r.CreatedAt),
		CanCancel: r.Status.Live() && (a.OwnerUserID == c.userID || c.manager),
	}
	if r.Error != nil {
		out.Error = *r.Error
	}
	if r.ThreadID != nil {
		out.ThreadId = *r.ThreadID
	}
	if r.StartedAt != nil {
		out.StartedAt = timestamppb.New(*r.StartedAt)
	}
	if r.FinishedAt != nil {
		out.FinishedAt = timestamppb.New(*r.FinishedAt)
	}
	for _, n := range nodes {
		node := &foundryv1.FoundryAutomationRunNode{NodeId: n.NodeID, Status: string(n.Status), OutputJson: string(n.Output), UsdMicros: n.USDMicros,
			StartedAt: timestamppb.New(n.StartedAt)}
		if n.Status == foundrymodel.FoundryAutomationNodeExecuting {
			node.Status = string(foundrymodel.FoundryAutomationNodeRunning)
		}
		if n.Error != nil {
			node.Error = *n.Error
		}
		if n.TurnID != nil {
			node.TurnId = *n.TurnID
		}
		if n.FinishedAt != nil {
			node.FinishedAt = timestamppb.New(*n.FinishedAt)
		}
		out.Nodes = append(out.Nodes, node)
	}
	return out
}
