package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// maxGrantsPerPrincipal bounds one principal's grants: a workspace's
// connectors' tables and its 20 sets come to far fewer.
const maxGrantsPerPrincipal = 1000

// guarded is desc with authorize run on every call to it, after the
// server's own interceptors (ActorInterceptor among them): a host that
// registers the service through Register can't serve a method around it.
// Foundry's services stream nothing; a stream would pass unchecked, so one
// is refused here.
func guarded(desc *grpc.ServiceDesc, authorize grpc.UnaryServerInterceptor) *grpc.ServiceDesc {
	if len(desc.Streams) > 0 {
		panic(fmt.Sprintf("api: %s streams, which the access check doesn't cover", desc.ServiceName))
	}
	out := *desc
	out.Methods = make([]grpc.MethodDesc, len(desc.Methods))
	for i, m := range desc.Methods {
		handler := m.Handler
		out.Methods[i] = grpc.MethodDesc{MethodName: m.MethodName,
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				if interceptor == nil {
					return handler(srv, ctx, dec, authorize)
				}
				return handler(srv, ctx, dec, func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
					return interceptor(ctx, req, info, func(ctx context.Context, req any) (any, error) {
						return authorize(ctx, req, info, next)
					})
				})
			}}
	}
	return &out
}

// authorize serves the host's caller as before grants, and a grantee only
// as access's rule for the method lets it, with its Grantee (and, for an
// ontology's method, the current load its grants cover) on the handler's
// context, for the handler to narrow what it answers.
func (s *Service) authorize(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	g, ok, err := s.granteeOf(ctx, actor(ctx), req)
	if err != nil {
		return nil, err
	}
	if !ok {
		return handler(ctx, req)
	}
	ctx, err = s.admit(access.WithCaller(ctx, g), g, access.RuleOf(info.FullMethod), req)
	if err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// granteeOf is the Grantee a call is for, if it is one: a SourceGrantee
// call, of its user or service credential, or a member restricted in the
// request's workspace while their role is member. A user's grantee holds
// their teams' grants too.
func (s *Service) granteeOf(ctx context.Context, a ports.Actor, req any) (access.Grantee, bool, error) {
	var g access.Grantee
	switch {
	case a.Source == ports.SourceGrantee && a.ServiceCredentialID != "":
		return access.Grantee{WorkspaceID: a.WorkspaceID, Principal: access.Principal{Kind: access.PrincipalServiceCredential, ID: a.ServiceCredentialID}}, true, nil
	case a.Source == ports.SourceGrantee:
		g = access.Grantee{WorkspaceID: a.WorkspaceID, Principal: access.Principal{Kind: access.PrincipalUser, ID: a.UserID}}
	case (a.Source == ports.SourceUser || a.Source == ports.SourceMCP) && a.UserID != "":
		workspaceID := a.WorkspaceID
		if r, ok := req.(interface{ GetWorkspaceId() string }); ok && r.GetWorkspaceId() != "" {
			workspaceID = r.GetWorkspaceId()
		}
		if workspaceID == "" {
			return access.Grantee{}, false, nil
		}
		restricted, err := s.deps.DB.IsFoundryMemberRestricted(ctx, workspaceID, a.UserID)
		if err != nil {
			return access.Grantee{}, false, status.Errorf(codes.Internal, "read restrictions: %v", err)
		}
		if !restricted || s.promoted(ctx, workspaceID, a.UserID) {
			return access.Grantee{}, false, nil
		}
		g = access.Grantee{WorkspaceID: workspaceID, Principal: access.Principal{Kind: access.PrincipalUser, ID: a.UserID}}
	default:
		return access.Grantee{}, false, nil
	}
	teams, err := s.deps.DB.FoundryTeamsOf(ctx, g.WorkspaceID, g.Principal.ID)
	if err != nil {
		return access.Grantee{}, false, status.Errorf(codes.Internal, "read teams: %v", err)
	}
	g.Teams = teams
	return g, true, nil
}

// promoted reports a restricted member made an owner or admin since, whose
// restriction no longer holds; it holds again should they be a member once
// more. Any doubt keeps it.
func (s *Service) promoted(ctx context.Context, workspaceID, userID string) bool {
	if s.deps.People == nil {
		return false
	}
	m, err := s.deps.People.Member(ctx, workspaceID, userID)
	return err == nil && (m.Role == ports.RoleOwner || m.Role == ports.RoleAdmin)
}

// errNotGranted refuses a grantee a method its grants can't admit.
var errNotGranted = status.Error(codes.PermissionDenied, "source grants don't allow this call")

type granteeLoadKey struct{}

// granteeLoad is the current load a grantee's call to an ontology's method
// was admitted on: the handler serves that load and no other, whatever is
// current by the time it reads.
func granteeLoad(ctx context.Context) (foundrymodel.FoundryLoadInputs, bool) {
	l, ok := ctx.Value(granteeLoadKey{}).(foundrymodel.FoundryLoadInputs)
	return l, ok
}

// admit checks a grantee's call against rule: every rule but Deny needs the
// grantee's own workspace, and Ontology an ontology whose current load it
// covers, which is NotFound otherwise, as an ontology it can't see is.
func (s *Service) admit(ctx context.Context, g access.Grantee, rule access.Rule, req any) (context.Context, error) {
	if rule == access.Deny {
		return nil, errNotGranted
	}
	ws, ok := req.(interface{ GetWorkspaceId() string })
	if !ok || ws.GetWorkspaceId() != g.WorkspaceID {
		return nil, errNotGranted
	}
	if rule != access.Ontology {
		return ctx, nil
	}
	o, ok := req.(interface{ GetOntologyId() string })
	if !ok || o.GetOntologyId() == "" {
		return nil, errNotGranted
	}
	loads, err := s.granteeLoads(ctx, g, []string{o.GetOntologyId()})
	if err != nil {
		return nil, err
	}
	load, ok := loads[o.GetOntologyId()]
	if !ok {
		return nil, status.Error(codes.NotFound, "ontology not found")
	}
	return context.WithValue(ctx, granteeLoadKey{}, load), nil
}

// grantsOf are the targets the principals hold together.
func (s *Service) grantsOf(ctx context.Context, workspaceID string, principals []access.Principal) (access.Grants, error) {
	grants, err := s.deps.DB.ListFoundryAccessGrants(ctx, workspaceID, principals)
	if err != nil {
		return access.Grants{}, status.Errorf(codes.Internal, "read grants: %v", err)
	}
	targets := make([]access.Target, len(grants))
	for i, gr := range grants {
		targets[i] = gr.Target
	}
	return access.NewGrants(targets), nil
}

// granteeLoads are the current loads g's grants hold anything of, by ontology:
// of ontologyIDs, or of every live ontology when it names none. One they hold
// only some of is Partial.
func (s *Service) granteeLoads(ctx context.Context, g access.Grantee, ontologyIDs []string) (map[string]foundrymodel.FoundryLoadInputs, error) {
	grants, err := s.grantsOf(ctx, g.WorkspaceID, g.Principals())
	if err != nil || grants.Empty() {
		return map[string]foundrymodel.FoundryLoadInputs{}, err
	}
	loads, err := s.deps.DB.ListFoundryCurrentLoadInputs(ctx, g.WorkspaceID, ontologyIDs)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read current loads: %v", err)
	}
	held := map[string]foundrymodel.FoundryLoadInputs{}
	for ontologyID, l := range loads {
		switch {
		case grants.Covers(l.Inputs):
			held[ontologyID] = l
		case grants.Reaches(l.Inputs):
			l.Partial = true
			held[ontologyID] = l
		}
	}
	return held, nil
}

// granteePart is what g holds of a load it holds only some of.
func (s *Service) granteePart(ctx context.Context, g access.Grantee, loadID string) (access.Part, error) {
	grants, err := s.grantsOf(ctx, g.WorkspaceID, g.Principals())
	if err != nil {
		return access.Part{}, err
	}
	holdings, err := s.deps.DB.FoundryLoadHoldings(ctx, g.WorkspaceID, loadID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return access.Part{}, status.Error(codes.NotFound, "load not found")
	}
	if err != nil {
		return access.Part{}, status.Errorf(codes.Internal, "read the load's holdings: %v", err)
	}
	return grants.Part(holdings), nil
}

// granteeSets are the workspace's document sets g's grants cover.
func (s *Service) granteeSets(ctx context.Context, g access.Grantee) ([]string, error) {
	grants, err := s.grantsOf(ctx, g.WorkspaceID, g.Principals())
	if err != nil {
		return nil, err
	}
	sets, err := s.deps.DB.ListFoundryDocumentSets(ctx, g.WorkspaceID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list sets: %v", err)
	}
	held := []string{}
	for _, set := range sets {
		ref := access.SetRef{SetID: set.ID}
		if set.Owner != nil {
			ref.ConnectorID = set.Owner.ConnectorID
		}
		if grants.CoversSet(ref) {
			held = append(held, set.ID)
		}
	}
	return held, nil
}

// granteeVisibility is what a grantee's call sends the agent of a load it holds
// only some of, as JSON.
func (s *Service) granteeVisibility(ctx context.Context, g access.Grantee, loadID string) (string, error) {
	part, err := s.granteePart(ctx, g, loadID)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(ontologyagent.VisibilityOf(part))
	if err != nil {
		return "", status.Errorf(codes.Internal, "encode visibility: %v", err)
	}
	return string(raw), nil
}

// ListFoundryAccess returns every grant and, for each live ontology with a
// current load, what it holds and whose own grants cover it. Authz has
// checked the caller manages the workspace's members.
func (s *Service) ListFoundryAccess(ctx context.Context, req *foundryv1.ListFoundryAccessRequest) (*foundryv1.ListFoundryAccessResponse, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	grants, err := s.deps.DB.ListFoundryAccessGrants(ctx, req.GetWorkspaceId(), nil)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read grants: %v", err)
	}
	loads, err := s.deps.DB.ListFoundryCurrentLoadInputs(ctx, req.GetWorkspaceId(), nil)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read current loads: %v", err)
	}
	teams, err := s.deps.DB.ListFoundryTeams(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read teams: %v", err)
	}
	restricted, err := s.deps.DB.ListFoundryRestrictedMembers(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read restricted members: %v", err)
	}
	resp := &foundryv1.ListFoundryAccessResponse{RestrictedUserIds: restricted}
	for _, t := range teams {
		resp.Teams = append(resp.Teams, teamToProto(t))
	}
	held := map[access.Principal][]access.Target{}
	var principals []access.Principal // in the order their first grant was made
	for _, gr := range grants {
		resp.Grants = append(resp.Grants, grantToProto(gr))
		if _, ok := held[gr.Principal]; !ok {
			principals = append(principals, gr.Principal)
		}
		held[gr.Principal] = append(held[gr.Principal], gr.Target)
	}
	for _, ontologyID := range slices.Sorted(maps.Keys(loads)) {
		l := loads[ontologyID]
		out := &foundryv1.FoundryOntologyAccess{OntologyId: ontologyID, LoadId: l.LoadID, Inputs: inputsToProto(l.Inputs)}
		for _, p := range principals {
			switch grants := access.NewGrants(held[p]); {
			case grants.Covers(l.Inputs):
				out.Readers = append(out.Readers, principalToProto(p))
			case grants.Reaches(l.Inputs):
				out.PartialReaders = append(out.PartialReaders, principalToProto(p))
			}
		}
		resp.Ontologies = append(resp.Ontologies, out)
	}
	return resp, nil
}

// SetFoundryAccessGrants replaces a principal's grants. Authz has checked
// the caller manages the workspace's members.
func (s *Service) SetFoundryAccessGrants(ctx context.Context, req *foundryv1.SetFoundryAccessGrantsRequest) (*foundryv1.SetFoundryAccessGrantsResponse, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	principal, err := principalFromProto(req.GetPrincipal())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if len(req.GetTargets()) > maxGrantsPerPrincipal {
		return nil, status.Errorf(codes.InvalidArgument, "a principal holds at most %d grants", maxGrantsPerPrincipal)
	}
	var targets []access.Target
	for _, t := range req.GetTargets() {
		target, err := access.ParseTarget(targetKindNames[t.GetKind()], t.GetConnectorId(), t.GetTable(), t.GetDocumentSetId())
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if !slices.Contains(targets, target) {
			targets = append(targets, target)
		}
	}
	// Only what's new has to confine them: taking some away is always allowed,
	// also from someone given raw access since.
	held, err := s.deps.DB.ListFoundryAccessGrants(ctx, req.GetWorkspaceId(), []access.Principal{principal})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read grants: %v", err)
	}
	heldTargets := make([]access.Target, len(held))
	for i, g := range held {
		heldTargets[i] = g.Target
	}
	covered := access.NewGrants(heldTargets)
	if slices.ContainsFunc(targets, func(t access.Target) bool {
		if t.Kind() == access.TargetTable {
			return !covered.CoversTable(access.TableRef{ConnectorID: t.ConnectorID(), Table: t.Table()})
		}
		return !slices.Contains(heldTargets, t)
	}) {
		if err := s.checkGrantee(ctx, req.GetWorkspaceId(), principal); err != nil {
			return nil, err
		}
	}
	switch err := s.deps.DB.SetFoundryAccessGrants(ctx, req.GetWorkspaceId(), principal, targets, byUser(ctx)); {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "a connector or document set to grant is not in the workspace")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "set grants: %v", err)
	}
	grants, err := s.deps.DB.ListFoundryAccessGrants(ctx, req.GetWorkspaceId(), []access.Principal{principal})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read grants: %v", err)
	}
	resp := &foundryv1.SetFoundryAccessGrantsResponse{}
	for _, gr := range grants {
		resp.Grants = append(resp.Grants, grantToProto(gr))
	}
	return resp, nil
}

// checkGrantee refuses grants for a user who isn't a member of the workspace or
// a team it doesn't have, who would hold them on joining, or as a team by that
// name. A service credential is the host's to tell: one of another workspace
// holds nothing, its calls being refused there.
func (s *Service) checkGrantee(ctx context.Context, workspaceID string, principal access.Principal) error {
	switch principal.Kind {
	case access.PrincipalUser:
		if s.deps.People == nil {
			return nil
		}
		return s.restrictable(ctx, workspaceID, principal.ID)
	case access.PrincipalTeam:
		teams, err := s.deps.DB.ListFoundryTeams(ctx, workspaceID)
		if err != nil {
			return status.Errorf(codes.Internal, "read teams: %v", err)
		}
		i := slices.IndexFunc(teams, func(t foundrymodel.FoundryTeam) bool { return t.ID == principal.ID })
		if i < 0 {
			return status.Error(codes.NotFound, "the workspace has no such team")
		}
		return s.teamRestrictable(ctx, workspaceID, teams[i].MemberUserIDs)
	}
	return nil
}

// GetMyFoundryAccess says what the caller reads through grants: as a
// grantee, or as the host's caller holding grants of its own (a member the
// front end sends on as a grantee where its own permissions fall short).
func (s *Service) GetMyFoundryAccess(ctx context.Context, req *foundryv1.GetMyFoundryAccessRequest) (*foundryv1.GetMyFoundryAccessResponse, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	g, grantee := access.GranteeFrom(ctx)
	if !grantee {
		a := actor(ctx)
		switch {
		case a.UserID != "":
			teams, err := s.deps.DB.FoundryTeamsOf(ctx, req.GetWorkspaceId(), a.UserID)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "read teams: %v", err)
			}
			g = access.Grantee{WorkspaceID: req.GetWorkspaceId(), Principal: access.Principal{Kind: access.PrincipalUser, ID: a.UserID}, Teams: teams}
		case a.ServiceCredentialID != "":
			g = access.Grantee{WorkspaceID: req.GetWorkspaceId(), Principal: access.Principal{Kind: access.PrincipalServiceCredential, ID: a.ServiceCredentialID}}
		default:
			return &foundryv1.GetMyFoundryAccessResponse{}, nil
		}
	}
	grants, err := s.grantsOf(ctx, g.WorkspaceID, g.Principals())
	if err != nil {
		return nil, err
	}
	loads, err := s.granteeLoads(ctx, g, nil)
	if err != nil {
		return nil, err
	}
	resp := &foundryv1.GetMyFoundryAccessResponse{Grantee: grantee, HasGrants: !grants.Empty(), OntologyIds: slices.Sorted(maps.Keys(loads))}
	for _, id := range resp.OntologyIds {
		if loads[id].Partial {
			resp.PartialOntologyIds = append(resp.PartialOntologyIds, id)
		}
	}
	return resp, nil
}

// PreviewFoundryAccessChange returns each principal whose own grants cover
// an ontology's current load now and wouldn't after the change. Authz has
// checked the caller may read the workspace.
func (s *Service) PreviewFoundryAccessChange(ctx context.Context, req *foundryv1.PreviewFoundryAccessChangeRequest) (*foundryv1.PreviewFoundryAccessChangeResponse, error) {
	ws := req.GetWorkspaceId()
	if ws == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	grants, err := s.deps.DB.ListFoundryAccessGrants(ctx, ws, nil)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read grants: %v", err)
	}
	if len(grants) == 0 {
		return &foundryv1.PreviewFoundryAccessChangeResponse{}, nil
	}
	loads, err := s.deps.DB.ListFoundryCurrentLoadInputs(ctx, ws, nil)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read current loads: %v", err)
	}
	before := map[string]access.Inputs{}
	for ontologyID, l := range loads {
		before[ontologyID] = l.Inputs
	}
	after := map[string]access.Inputs{}
	keep := func(foundrymodel.FoundryAccessGrant) bool { return true }
	switch change := req.GetChange().GetChange().(type) {
	case *foundryv1.FoundryAccessChange_LoadVersion_:
		in, err := s.versionInputs(ctx, ws, change.LoadVersion.GetOntologyId(), change.LoadVersion.GetVersionId())
		if err != nil {
			return nil, err
		}
		after[change.LoadVersion.GetOntologyId()] = in
	case *foundryv1.FoundryAccessChange_DeleteConnector:
		connector, err := s.deps.DB.GetConnector(ctx, ws, change.DeleteConnector)
		if errors.Is(err, foundrystore.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "connector not found")
		}
		if err != nil {
			return nil, status.Errorf(codes.Internal, "get connector: %v", err)
		}
		sets, err := s.deps.DB.ListFoundryDocumentSets(ctx, ws)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list sets: %v", err)
		}
		var owned []string
		for _, set := range sets {
			if set.Owner != nil && set.Owner.ConnectorID == connector.ID {
				owned = append(owned, set.ID)
			}
		}
		// Deleting a connector deletes its grants and its sets' grants.
		keep = func(gr foundrymodel.FoundryAccessGrant) bool {
			return gr.Target.ConnectorID() != connector.ID && !slices.Contains(owned, gr.Target.DocumentSetID())
		}
		after = before
	case *foundryv1.FoundryAccessChange_MoveDocument_:
		set, err := s.deps.DB.GetFoundryDocumentSet(ctx, ws, change.MoveDocument.GetDocumentSetId())
		if errors.Is(err, foundrystore.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "document set not found")
		}
		if err != nil {
			return nil, status.Errorf(codes.Internal, "get set: %v", err)
		}
		holding, err := s.deps.DB.ListFoundryOntologiesHoldingDocument(ctx, ws, change.MoveDocument.GetDocumentId())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "find loads holding the document: %v", err)
		}
		// The document's old set may stay for its others; only the new one can
		// take a load away.
		moved := access.SetRef{SetID: set.ID}
		if set.Owner != nil {
			moved.ConnectorID = set.Owner.ConnectorID
		}
		for _, ontologyID := range holding {
			in := before[ontologyID]
			in.Sets = append(slices.Clone(in.Sets), moved)
			after[ontologyID] = in
		}
	default:
		return nil, status.Error(codes.InvalidArgument, "name a change")
	}
	held, heldAfter := map[access.Principal][]access.Target{}, map[access.Principal][]access.Target{}
	var principals []access.Principal
	for _, gr := range grants {
		if _, ok := held[gr.Principal]; !ok {
			principals = append(principals, gr.Principal)
		}
		held[gr.Principal] = append(held[gr.Principal], gr.Target)
		if keep(gr) {
			heldAfter[gr.Principal] = append(heldAfter[gr.Principal], gr.Target)
		}
	}
	resp := &foundryv1.PreviewFoundryAccessChangeResponse{}
	for _, ontologyID := range slices.Sorted(maps.Keys(after)) {
		was, ok := before[ontologyID]
		for _, p := range principals {
			// Whoever read it whole and no longer would, and whoever read part of it and would read none.
			then, next, now := access.NewGrants(held[p]), access.NewGrants(heldAfter[p]), after[ontologyID]
			if ok && (then.Covers(was) && !next.Covers(now) || then.Reaches(was) && !next.Reaches(now)) {
				resp.Losses = append(resp.Losses, &foundryv1.FoundryAccessLoss{Principal: principalToProto(p), OntologyId: ontologyID,
					KeepsPart: next.Reaches(now)})
			}
		}
	}
	return resp, nil
}

// versionInputs is what a new load of the ontology's version would hold.
func (s *Service) versionInputs(ctx context.Context, workspaceID, ontologyID, versionID string) (access.Inputs, error) {
	if _, err := s.liveFoundryOntology(ctx, workspaceID, ontologyID); err != nil {
		return access.Inputs{}, err
	}
	in, err := s.deps.DB.FoundryVersionInputs(ctx, workspaceID, ontologyID, versionID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return access.Inputs{}, status.Error(codes.NotFound, "version not found")
	}
	if err != nil {
		return access.Inputs{}, status.Errorf(codes.Internal, "read the version's inputs: %v", err)
	}
	version, err := s.loadOntologyVersion(ctx, ontologyID, versionID)
	if err != nil {
		return access.Inputs{}, err
	}
	var def ontologyagent.Definition
	if json.Unmarshal(version.Definition, &def) == nil {
		item, err := ontologyagent.ParseEnrichments(def.MappingsTOML)
		in.Enrichment = err != nil || item != nil // a load fails on mappings it can't read; say it would enrich
	}
	return in, nil
}

var principalKindToProto = map[access.PrincipalKind]foundryv1.FoundryPrincipal_Kind{
	access.PrincipalUser:              foundryv1.FoundryPrincipal_KIND_USER,
	access.PrincipalServiceCredential: foundryv1.FoundryPrincipal_KIND_SERVICE_CREDENTIAL,
	access.PrincipalTeam:              foundryv1.FoundryPrincipal_KIND_TEAM,
}

var targetKindToProto = map[access.TargetKind]foundryv1.FoundryAccessTarget_Kind{
	access.TargetConnector:   foundryv1.FoundryAccessTarget_KIND_CONNECTOR,
	access.TargetTable:       foundryv1.FoundryAccessTarget_KIND_TABLE,
	access.TargetDocumentSet: foundryv1.FoundryAccessTarget_KIND_DOCUMENT_SET,
	access.TargetEnrichment:  foundryv1.FoundryAccessTarget_KIND_ENRICHMENT,
}

// targetKindNames are the proto's kinds by the names ParseTarget reads; an
// unknown kind is "", which it refuses.
var targetKindNames = func() map[foundryv1.FoundryAccessTarget_Kind]string {
	out := map[foundryv1.FoundryAccessTarget_Kind]string{}
	for k, p := range targetKindToProto {
		out[p] = k.String()
	}
	return out
}()

func principalToProto(p access.Principal) *foundryv1.FoundryPrincipal {
	return &foundryv1.FoundryPrincipal{Kind: principalKindToProto[p.Kind], Id: p.ID}
}

func principalFromProto(p *foundryv1.FoundryPrincipal) (access.Principal, error) {
	for k, kind := range principalKindToProto {
		if kind == p.GetKind() && p.GetId() != "" {
			return access.Principal{Kind: k, ID: p.GetId()}, nil
		}
	}
	return access.Principal{}, errors.New("name a principal: a user, a service credential or a team, and its id")
}

func grantToProto(gr foundrymodel.FoundryAccessGrant) *foundryv1.FoundryAccessGrant {
	out := &foundryv1.FoundryAccessGrant{
		Id: gr.ID, Principal: principalToProto(gr.Principal), CreatedAt: timestamppb.New(gr.CreatedAt),
		Target: &foundryv1.FoundryAccessTarget{Kind: targetKindToProto[gr.Target.Kind()], ConnectorId: gr.Target.ConnectorID(),
			Table: gr.Target.Table(), DocumentSetId: gr.Target.DocumentSetID()},
	}
	if gr.CreatedByUserID != nil {
		out.CreatedByUserId = *gr.CreatedByUserID
	}
	return out
}

func inputsToProto(in access.Inputs) *foundryv1.FoundryLoadAccessInputs {
	in.Uncoverable = in.Uncoverable || in.Unknown
	out := &foundryv1.FoundryLoadAccessInputs{Enrichment: in.Enrichment, Uncoverable: in.Uncoverable}
	for _, t := range in.Tables {
		out.Tables = append(out.Tables, &foundryv1.FoundryLoadAccessInputs_Table{ConnectorId: t.ConnectorID, Table: t.Table})
	}
	for _, set := range in.Sets {
		out.DocumentSets = append(out.DocumentSets, &foundryv1.FoundryLoadAccessInputs_DocumentSet{DocumentSetId: set.SetID, ConnectorId: set.ConnectorID})
	}
	for _, set := range in.DecisionSets {
		if !slices.Contains(in.Sets, set) {
			out.DocumentSets = append(out.DocumentSets, &foundryv1.FoundryLoadAccessInputs_DocumentSet{DocumentSetId: set.SetID, ConnectorId: set.ConnectorID})
		}
	}
	return out
}
