package api

import (
	"context"
	"encoding/json"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// ListFoundryHealFindings is the ontology's shown self-healing findings
// (foundrymodel.FoundryHealShownLimit open ones, by support), its latest
// audit and its latest complete one, and whether this deployment audits
// loads at all
// (foundry.heal.enabled). It only reads what the foundry-heal-worker
// stored.
func (s *Service) ListFoundryHealFindings(ctx context.Context, req *foundryv1.ListFoundryHealFindingsRequest) (*foundryv1.ListFoundryHealFindingsResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	findings, err := s.deps.DB.ListFoundryHealFindings(ctx, o.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list heal findings: %v", err)
	}
	out := &foundryv1.ListFoundryHealFindingsResponse{Enabled: s.deps.Cfg.Foundry.Heal.Enabled}
	// Off, no finding carries a state: its card drafts with the agent.
	states := map[string]foundrymodel.FoundryFindingState{}
	if s.deps.Cfg.Foundry.Heal.AddInPlace {
		if states, findings, err = s.healFindingStates(ctx, o, findings); err != nil {
			return nil, err
		}
	}
	for _, f := range findings {
		p := foundryHealFindingProto(f)
		p.State = foundryHealStateProto(states[f.Fingerprint])
		out.Findings = append(out.Findings, p)
	}
	for _, latest := range []struct {
		get func(context.Context, string) (*foundrymodel.FoundryHealRun, error)
		to  **foundryv1.FoundryHealRun
	}{{s.deps.DB.GetLatestFoundryHealRun, &out.LastRun}, {s.deps.DB.GetLatestCompleteFoundryHealRun, &out.LastCompleteRun}} {
		switch run, err := latest.get(ctx, o.ID); {
		case errors.Is(err, foundrystore.ErrNotFound):
		case err != nil:
			return nil, status.Errorf(codes.Internal, "get heal run: %v", err)
		default:
			*latest.to = &foundryv1.FoundryHealRun{
				Id: run.ID, LoadId: run.LoadID, Status: string(run.Status), Pages: int32(run.Pages), Cuts: int32(run.Cuts),
				RowsMeasured: run.RowsMeasured, StartedAt: timestamppb.New(run.CreatedAt),
			}
			if run.FinishedAt != nil {
				(*latest.to).FinishedAt = timestamppb.New(*run.FinishedAt)
			}
		}
	}
	return out, nil
}

// DismissFoundryHealFinding dismisses an open finding for the caller at
// its support now. The finding stays as it was dismissed, with the
// spellings it holds, until an audit finds it with twice that support or a
// spelling it doesn't hold (foundrymodel.FoundryHealFinding.Next). One
// already dismissed is returned as it is: dismissing is idempotent.
func (s *Service) DismissFoundryHealFinding(ctx context.Context, req *foundryv1.DismissFoundryHealFindingRequest) (*foundryv1.FoundryHealFindingResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if req.GetFingerprint() == "" {
		return nil, status.Error(codes.InvalidArgument, "fingerprint is required")
	}
	userID := actor(ctx).UserID
	if userID == "" {
		return nil, status.Error(codes.Unauthenticated, "a user is required to dismiss a finding")
	}
	f, err := s.deps.DB.GetFoundryHealFinding(ctx, o.ID, req.GetFingerprint())
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "finding not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get heal finding: %v", err)
	}
	if f.Status == foundrymodel.FoundryHealOpen {
		switch err := s.deps.DB.DismissFoundryHealFinding(ctx, o.ID, f.Fingerprint, userID); {
		case errors.Is(err, foundrystore.ErrNotFound):
			// It changed since: dismissed by someone else, or superseded.
		case err != nil:
			return nil, status.Errorf(codes.Internal, "dismiss heal finding: %v", err)
		}
		// Read again: a run may have stored a new support just before.
		if f, err = s.deps.DB.GetFoundryHealFinding(ctx, o.ID, f.Fingerprint); err != nil {
			return nil, status.Errorf(codes.Internal, "get heal finding: %v", err)
		}
	}
	if f.Status != foundrymodel.FoundryHealDismissed {
		return nil, status.Errorf(codes.FailedPrecondition, "the finding is %s, not open", f.Status)
	}
	return &foundryv1.FoundryHealFindingResponse{Finding: foundryHealFindingProto(f)}, nil
}

func foundryHealFindingProto(f *foundrymodel.FoundryHealFinding) *foundryv1.FoundryHealFinding {
	out := &foundryv1.FoundryHealFinding{
		Fingerprint: f.Fingerprint, Kind: f.Kind, Status: string(f.Status), Support: f.Support, Sources: f.Sources,
		LoadId: f.LoadID, BaseVersionId: f.BaseVersionID, FindingJson: string(f.Finding),
	}
	if f.Dismissal != nil {
		out.DismissedByUserId, out.SupportAtDismissal = &f.Dismissal.ByUserID, &f.Dismissal.Support
	}
	return out
}

// healFindingStates is where each finding's "Add this link" stands in the
// ontology's current load (model.FoundryFindingStateOf), by fingerprint, for
// the findings that name a relationship to add; and findings, with those
// the load has a move for that the list lacks (dismissed or superseded
// since), so a move's word stays shown. A version covers a finding when it
// is the ontology's current version or its newest, isn't the load's, and
// declares and routes the finding's link for its set
// (ontologyagent.StatesLink). A load whose own version states the link, as a
// rebuild of a moved load, reads as moved to it.
func (s *Service) healFindingStates(ctx context.Context, o *foundrymodel.FoundryOntology, findings []*foundrymodel.FoundryHealFinding) (
	map[string]foundrymodel.FoundryFindingState, []*foundrymodel.FoundryHealFinding, error) {
	states := map[string]foundrymodel.FoundryFindingState{}
	if o.CurrentLoadID == nil {
		return states, findings, nil
	}
	load, err := s.deps.DB.GetFoundryLoad(ctx, o.ID, *o.CurrentLoadID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return states, findings, nil
	}
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "get current load: %v", err)
	}
	fills, err := s.deps.DB.ListFoundryHealFills(ctx, load.ID)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "list heal fills: %v", err)
	}
	listed := map[string]bool{}
	for _, f := range findings {
		listed[f.Fingerprint] = true
	}
	for fp := range fills {
		if listed[fp] {
			continue
		}
		switch f, err := s.deps.DB.GetFoundryHealFinding(ctx, o.ID, fp); {
		case errors.Is(err, foundrystore.ErrNotFound):
		case err != nil:
			return nil, nil, status.Errorf(codes.Internal, "get heal finding: %v", err)
		default:
			findings = append(findings, f)
		}
	}
	// The versions that may cover a finding, read once: the current one and
	// the newest, of those that aren't the load's.
	var candidates []*foundrymodel.OntologyVersion
	newest, err := s.deps.DB.ListOntologyVersions(ctx, o.ID, 1)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "list versions: %v", err)
	}
	ids := []string{}
	if len(newest) > 0 {
		ids = append(ids, newest[0].ID)
	}
	if o.CurrentVersionID != nil && (len(ids) == 0 || ids[0] != *o.CurrentVersionID) {
		ids = append(ids, *o.CurrentVersionID)
	}
	for _, id := range ids {
		if id == load.VersionID {
			continue
		}
		v, err := s.deps.DB.GetOntologyVersion(ctx, o.ID, id)
		if err != nil {
			return nil, nil, status.Errorf(codes.Internal, "get version: %v", err)
		}
		candidates = append(candidates, v)
	}
	own, err := s.deps.DB.GetOntologyVersion(ctx, o.ID, load.VersionID)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "get the load's version: %v", err)
	}
	var ownDef ontologyagent.Definition
	_ = json.Unmarshal(own.Definition, &ownDef)
	for _, f := range findings {
		canonical, set, ok := unmatchedLink(f)
		if !ok {
			continue
		}
		// A fill of it says how far it got, whatever else.
		if fill := fills[f.Fingerprint]; fill != nil && (fill.State == foundrymodel.FoundryFillApplying ||
			fill.State == foundrymodel.FoundryFillFilled || fill.State == foundrymodel.FoundryFillStopped) {
			states[f.Fingerprint] = foundrymodel.FoundryFindingStateOf(fill, nil)
			continue
		}
		// The load's version states it: moved, whatever a move of it says (one
		// to another version that states it too answered it).
		if ontologyagent.StatesLink(ownDef.OntologyTOML, ownDef.MappingsTOML, canonical, set) {
			states[f.Fingerprint] = foundrymodel.FoundryFindingMoved{
				Version:    foundrymodel.FoundryVersionRef{ID: own.ID, Version: own.Version},
				LinksAdded: []string{canonical},
			}
			continue
		}
		var covered *foundrymodel.FoundryVersionRef
		for _, v := range candidates {
			var def ontologyagent.Definition
			if json.Unmarshal(v.Definition, &def) != nil || !ontologyagent.StatesLink(def.OntologyTOML, def.MappingsTOML, canonical, set) {
				continue
			}
			covered = &foundrymodel.FoundryVersionRef{ID: v.ID, Version: v.Version}
			break
		}
		states[f.Fingerprint] = foundrymodel.FoundryFindingStateOf(fills[f.Fingerprint], covered)
	}
	return states, findings, nil
}

// unmatchedLink is the link a relationship no link covers would be added
// as, and the set its documents are of, from the engine's Finding: its
// signal's canonical and set. ok is false for any other finding.
func unmatchedLink(f *foundrymodel.FoundryHealFinding) (canonical, set string, ok bool) {
	var finding struct {
		Signal struct {
			Kind      string `json:"kind"`
			Canonical string `json:"canonical"`
			Set       string `json:"set"`
		} `json:"signal"`
	}
	if json.Unmarshal(f.Finding, &finding) != nil || finding.Signal.Kind != "unmatched" || finding.Signal.Canonical == "" {
		return "", "", false
	}
	return finding.Signal.Canonical, finding.Signal.Set, true
}

// foundryHealFillProto is a fill on the wire, of the version v it filled.
func foundryHealFillProto(v foundrymodel.FoundryVersionRef, f foundrymodel.FoundryHealFill) *foundryv1.FoundryHealFill {
	out := &foundryv1.FoundryHealFill{
		VersionId: v.ID, Version: int64(v.Version), LinksAdded: f.LinksAdded,
		DocsOwed: f.Counts.Owed, DocsDone: f.Counts.Done(), DocsLeft: f.Counts.Left,
		ReviewsResolved: f.Counts.ReviewsTaken, FactsAdded: f.Counts.FactsAdded,
		SpentUsd: f.SpentUSD, CapUsd: f.CapUSD, StopReason: f.StopReason,
	}
	if f.PausedUntil != nil {
		out.PausedUntil = timestamppb.New(*f.PausedUntil)
	}
	return out
}

// foundryHealStateProto is st on the wire; nil for none.
func foundryHealStateProto(st foundrymodel.FoundryFindingState) *foundryv1.FoundryHealFindingState {
	version := func(v foundrymodel.FoundryVersionRef) *foundryv1.FoundryHealVersion {
		return &foundryv1.FoundryHealVersion{VersionId: v.ID, Version: int64(v.Version)}
	}
	switch st := st.(type) {
	case foundrymodel.FoundryFindingOpen:
		return &foundryv1.FoundryHealFindingState{State: &foundryv1.FoundryHealFindingState_Open{Open: &foundryv1.FoundryHealOpen{}}}
	case foundrymodel.FoundryFindingCovered:
		return &foundryv1.FoundryHealFindingState{State: &foundryv1.FoundryHealFindingState_Covered{Covered: version(st.Version)}}
	case foundrymodel.FoundryFindingMoving:
		return &foundryv1.FoundryHealFindingState{State: &foundryv1.FoundryHealFindingState_Moving{Moving: version(st.Version)}}
	case foundrymodel.FoundryFindingMoved:
		return &foundryv1.FoundryHealFindingState{State: &foundryv1.FoundryHealFindingState_Moved{Moved: &foundryv1.FoundryHealMoved{
			VersionId: st.Version.ID, Version: int64(st.Version.Version), LinksAdded: st.LinksAdded,
		}}}
	case foundrymodel.FoundryFindingApplying:
		return &foundryv1.FoundryHealFindingState{State: &foundryv1.FoundryHealFindingState_Applying{Applying: foundryHealFillProto(st.Version, st.Fill)}}
	case foundrymodel.FoundryFindingFixed:
		return &foundryv1.FoundryHealFindingState{State: &foundryv1.FoundryHealFindingState_Fixed{Fixed: foundryHealFillProto(st.Version, st.Fill)}}
	case foundrymodel.FoundryFindingStopped:
		return &foundryv1.FoundryHealFindingState{State: &foundryv1.FoundryHealFindingState_Stopped{Stopped: foundryHealFillProto(st.Version, st.Fill)}}
	case foundrymodel.FoundryFindingNeedsLoad:
		why := make([]*foundryv1.FoundryFoldWhy, len(st.Why))
		for i, w := range st.Why {
			why[i] = &foundryv1.FoundryFoldWhy{Kind: w.Kind, Item: w.Item}
		}
		return &foundryv1.FoundryHealFindingState{State: &foundryv1.FoundryHealFindingState_NeedsLoad{NeedsLoad: &foundryv1.FoundryHealNeedsLoad{
			VersionId: st.Version.ID, Version: int64(st.Version.Version), Why: why,
		}}}
	}
	return nil
}
