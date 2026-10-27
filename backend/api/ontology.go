package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/ontology"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

const (
	defaultOntologyListLimit = 20
	maxOntologyListLimit     = 100
)

// GetCurrentOntologyVersion returns the version the ontology shows; an empty
// response when it has none.
func (s *Service) GetCurrentOntologyVersion(ctx context.Context, req *foundryv1.GetCurrentOntologyVersionRequest) (*foundryv1.OntologyVersionResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	current, err := s.deps.DB.GetCurrentOntologyVersion(ctx, o.ID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return &foundryv1.OntologyVersionResponse{}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get current ontology: %v", err)
	}
	scope, err := s.ontologyVersionScope(ctx, current)
	if err != nil {
		return nil, err
	}
	return ontologyResponse(current, true, loadEvidence(ctx, o), scope)
}

func (s *Service) ListOntologyVersions(ctx context.Context, req *foundryv1.ListOntologyVersionsRequest) (*foundryv1.ListOntologyVersionsResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultOntologyListLimit
	}
	versions, err := s.deps.DB.ListOntologyVersions(ctx, o.ID, min(limit, maxOntologyListLimit))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list ontology versions: %v", err)
	}
	resp := &foundryv1.ListOntologyVersionsResponse{}
	// Every legacy version of the ontology reads the same sets: read them once.
	var legacy []string
	for _, v := range versions {
		scope := legacy
		switch {
		case v.DocumentSetIDs != nil:
			scope = *v.DocumentSetIDs
		case legacy == nil:
			if legacy, err = s.ontologyVersionScope(ctx, v); err != nil {
				return nil, err
			}
			scope = legacy
		}
		resp.Versions = append(resp.Versions, ontologyVersionToProto(v, o.CurrentVersionID != nil && v.ID == *o.CurrentVersionID, scope))
	}
	return resp, nil
}

func (s *Service) GetOntologyVersion(ctx context.Context, req *foundryv1.GetOntologyVersionRequest) (*foundryv1.OntologyVersionResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	v, err := s.loadOntologyVersion(ctx, o.ID, req.GetId())
	if err != nil {
		return nil, err
	}
	scope, err := s.ontologyVersionScope(ctx, v)
	if err != nil {
		return nil, err
	}
	return ontologyResponse(v, o.CurrentVersionID != nil && v.ID == *o.CurrentVersionID, loadEvidence(ctx, o), scope)
}

// CreateOntologyVersion stores a hand-written version after validating it
// against the workspace's synced tables. It never changes the current version.
// Only Definition v1 is written by hand: an agent draft comes from a
// generation. A Definition v1 can't be loaded, so it reads no document set.
func (s *Service) CreateOntologyVersion(ctx context.Context, req *foundryv1.CreateOntologyVersionRequest) (*foundryv1.OntologyVersionResponse, error) {
	if req.GetDefinition() == nil {
		return nil, status.Error(codes.InvalidArgument, "definition is required")
	}
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if req.GetDefinition().GetFields()["format"].GetStringValue() == string(foundrymodel.OntologyFormatAgentBundle) {
		return nil, status.Error(codes.FailedPrecondition, "agent drafts can't be edited by hand yet; generate a new one")
	}
	raw, err := protojson.Marshal(req.GetDefinition())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "definition is not valid JSON: %v", err)
	}
	if err := s.validateOntology(ctx, o.WorkspaceID, raw); err != nil {
		return nil, err
	}
	v := &foundrymodel.OntologyVersion{
		ID: id.New(), WorkspaceID: o.WorkspaceID, OntologyID: o.ID, Format: foundrymodel.OntologyFormatFoundryV1, Definition: raw,
		Source: foundrymodel.OntologySourceManual, DocumentSetIDs: &[]string{},
	}
	if userID := actor(ctx).UserID; userID != "" {
		v.CreatedByUserID = &userID
	}
	if err := s.deps.DB.CreateOntologyVersion(ctx, v); err != nil {
		if errors.Is(err, foundrystore.ErrConflict) {
			return nil, status.Error(codes.Aborted, "another version was saved at the same time; try again")
		}
		return nil, status.Errorf(codes.Internal, "create ontology version: %v", err)
	}
	stored, err := s.loadOntologyVersion(ctx, o.ID, v.ID)
	if err != nil {
		return nil, err
	}
	// A hand-written Definition v1 has no evidence to withhold, and reads no set.
	return ontologyResponse(stored, false, false, []string{})
}

// SetCurrentOntologyVersion points the ontology at a version, provided it
// still points where the caller saw it; moving back is how a rollback works.
func (s *Service) SetCurrentOntologyVersion(ctx context.Context, req *foundryv1.SetCurrentOntologyVersionRequest) (*foundryv1.OntologyVersionResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	v, err := s.loadOntologyVersion(ctx, o.ID, req.GetVersionId())
	if err != nil {
		return nil, err
	}
	var expected *string
	if e := req.GetExpectedCurrentVersionId(); e != "" {
		expected = &e
	}
	scope, err := s.ontologyVersionScope(ctx, v)
	if err != nil {
		return nil, err
	}
	if expected != nil && *expected == v.ID && o.CurrentVersionID != nil && *o.CurrentVersionID == v.ID {
		return ontologyResponse(v, true, loadEvidence(ctx, o), scope)
	}
	err = s.deps.DB.SetCurrentOntologyVersion(ctx, o.ID, v.ID, expected)
	if errors.Is(err, foundrystore.ErrConflict) {
		return nil, status.Error(codes.FailedPrecondition, "the current ontology changed since you loaded it; reload and try again")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "set current ontology: %v", err)
	}
	return ontologyResponse(v, true, loadEvidence(ctx, o), scope)
}

// validateOntology checks a definition against the tables the workspace has synced.
func (s *Service) validateOntology(ctx context.Context, workspaceID string, raw []byte) error {
	def, err := ontology.Parse(raw)
	if err == nil {
		var tables []ontology.Table
		tables, err = s.syncedTables(ctx, workspaceID)
		if err != nil {
			return err
		}
		err = ontology.Validate(def, tables)
	}
	var invalidErr *ontology.InvalidError
	if errors.As(err, &invalidErr) {
		return status.Error(codes.InvalidArgument, "the ontology is not valid: "+strings.Join(invalidErr.Problems, "; "))
	}
	return err
}

// syncedTables lists every table the workspace's connectors have synced.
func (s *Service) syncedTables(ctx context.Context, workspaceID string) ([]ontology.Table, error) {
	connectors, err := s.deps.DB.ListConnectors(ctx, workspaceID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list connectors: %v", err)
	}
	var out []ontology.Table
	for _, c := range connectors {
		tables, err := s.deps.DB.ListConnectorTables(ctx, workspaceID, c.ID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list connector %s tables: %v", c.ID, err)
		}
		for _, t := range tables {
			var columns []connector.Column
			if err := json.Unmarshal(t.Columns, &columns); err != nil {
				return nil, status.Errorf(codes.Internal, "read connector %s table %s columns: %v", c.ID, t.TableName, err)
			}
			names := make([]string, len(columns))
			for i, col := range columns {
				names[i] = col.Name
			}
			out = append(out, ontology.Table{ConnectorID: c.ID, Name: t.TableName, Columns: names})
		}
	}
	return out, nil
}

func (s *Service) loadOntologyVersion(ctx context.Context, ontologyID, versionID string) (*foundrymodel.OntologyVersion, error) {
	if versionID == "" {
		return nil, status.Error(codes.InvalidArgument, "version id is required")
	}
	v, err := s.deps.DB.GetOntologyVersion(ctx, ontologyID, versionID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "ontology version not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get ontology version: %v", err)
	}
	return v, nil
}

// ontologyVersionScope is the document sets v reads: its own scope, or for
// a legacy version its ontology's live sets (GetOntologyVersionScope).
func (s *Service) ontologyVersionScope(ctx context.Context, v *foundrymodel.OntologyVersion) ([]string, error) {
	if v.DocumentSetIDs != nil {
		return *v.DocumentSetIDs, nil
	}
	scope, err := s.deps.DB.GetOntologyVersionScope(ctx, v.OntologyID, v.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get ontology version scope: %v", err)
	}
	return scope, nil
}

// ontologyResponse answers v with its definition and scope (the sets it
// reads). Without evidence (see loadEvidence), a revised version's draft
// items go without their cards: Revise measured them on the ontology's load.
// A generated draft's cards measure synced rows and document previews,
// which a member reads anyway.
func ontologyResponse(v *foundrymodel.OntologyVersion, current, evidence bool, scope []string) (*foundryv1.OntologyVersionResponse, error) {
	pv := ontologyVersionToProto(v, current, scope)
	var def structpb.Struct
	if err := protojson.Unmarshal(v.Definition, &def); err != nil {
		return nil, status.Errorf(codes.Internal, "read ontology version %s: %v", v.ID, err)
	}
	if !evidence && def.Fields["parent_version_id"].GetStringValue() != "" {
		// definition.draft is the DraftView, its draft the engine's Draft.
		items := def.Fields["draft"].GetStructValue().GetFields()["draft"].GetStructValue().GetFields()["items"].GetStructValue().GetFields()
		for _, item := range items {
			fields := item.GetStructValue().GetFields()
			pv.EvidenceWithheld = pv.EvidenceWithheld || fields["card"].GetStructValue() != nil
			delete(fields, "card")
		}
	}
	pv.Definition = &def
	return &foundryv1.OntologyVersionResponse{Version: pv}, nil
}

// loadEvidence reports whether the caller may see what was measured on o's
// load (a revision's result, a revised draft's cards): only one who may
// query its database, as reading the load itself needs. That is the actor's
// query_read scope of o's workspace, as its front end resolved it; without
// one, it is withheld.
func loadEvidence(ctx context.Context, o *foundrymodel.FoundryOntology) bool {
	read, _ := actor(ctx).Scope(o.WorkspaceID)
	return read.Allows(o.ProjectID)
}

var ontologySourceToProto = map[foundrymodel.OntologySource]foundryv1.OntologySource{
	foundrymodel.OntologySourceManual:    foundryv1.OntologySource_ONTOLOGY_SOURCE_MANUAL,
	foundrymodel.OntologySourceGenerated: foundryv1.OntologySource_ONTOLOGY_SOURCE_GENERATED,
}

var ontologyFormatToProto = map[foundrymodel.OntologyFormat]foundryv1.OntologyFormat{
	foundrymodel.OntologyFormatFoundryV1:   foundryv1.OntologyFormat_ONTOLOGY_FORMAT_FOUNDRY_V1,
	foundrymodel.OntologyFormatAgentBundle: foundryv1.OntologyFormat_ONTOLOGY_FORMAT_AGENT_BUNDLE,
}

// ontologyVersionToProto leaves the definition out; lists stay small. scope
// is the sets v reads (ontologyVersionScope).
func ontologyVersionToProto(v *foundrymodel.OntologyVersion, current bool, scope []string) *foundryv1.OntologyVersion {
	out := &foundryv1.OntologyVersion{
		Id: v.ID, WorkspaceId: v.WorkspaceID, OntologyId: v.OntologyID, Version: int32(v.Version), Source: ontologySourceToProto[v.Source],
		Format: ontologyFormatToProto[v.Format], CreatedAt: timestamppb.New(v.CreatedAt), IsCurrent: current, DocumentSetIds: scope,
	}
	if v.GenerationID != nil {
		out.GenerationId = *v.GenerationID
	}
	if v.CreatedByUserID != nil {
		out.CreatedByUserId = *v.CreatedByUserID
	}
	return out
}

// ReviseOntologyVersion queues edits; the worker owns every engine call.
// It copies the parent's generation's tables so the new manual version
// retains its source names and can be loaded with the normal generation_id
// contract, and takes the parent version's scope as its document sets: the
// version it stores reads them, and the live sets its revised routes name.
func (s *Service) ReviseOntologyVersion(ctx context.Context, req *foundryv1.ReviseOntologyVersionRequest) (*foundryv1.OntologyGenerationResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	parent, err := s.loadOntologyVersion(ctx, o.ID, req.GetVersionId())
	if err != nil {
		return nil, err
	}
	if parent.Format != foundrymodel.OntologyFormatAgentBundle || parent.GenerationID == nil {
		return nil, status.Error(codes.FailedPrecondition, "only an agent version with generation inputs can be revised")
	}
	var def ontologyagent.Definition
	if err := json.Unmarshal(parent.Definition, &def); err != nil || len(def.Draft) == 0 || def.Status != ontologyagent.StatusDone {
		return nil, status.Error(codes.FailedPrecondition, "only a finished agent draft can be revised")
	}
	if len(req.GetEdits()) == 0 || len(req.GetEdits()) > 100 {
		return nil, status.Error(codes.InvalidArgument, "provide 1 to 100 edits")
	}
	revision := &foundrymodel.OntologyRevision{ParentVersionID: parent.ID, DryRun: req.GetDryRun()}
	seen := map[string]bool{}
	bytes := 0
	for _, e := range req.GetEdits() {
		var edit foundrymodel.OntologyEdit
		switch op := e.GetEdit().(type) {
		case *foundryv1.OntologyEdit_Put:
			edit = foundrymodel.OntologyPut{ItemID: op.Put.GetItemId(), TOML: op.Put.GetToml()}
			bytes += len(op.Put.GetToml())
		case *foundryv1.OntologyEdit_Remove:
			edit = foundrymodel.OntologyRemove{ItemID: op.Remove.GetItemId()}
		default:
			return nil, status.Error(codes.InvalidArgument, "each edit is a put or a remove")
		}
		item := edit.Item()
		if strings.TrimSpace(item) == "" || len(item) > 256 || seen[item] {
			return nil, status.Error(codes.InvalidArgument, "edits require distinct item ids of at most 256 bytes")
		}
		// People enrichment's item is Helix's to offer: none is put or removed while its flag is off. Consent
		// isn't checked here; a load without it applies no profiles, and says so.
		if strings.HasPrefix(item, "enrichment.") && !s.deps.Cfg.Foundry.Enrichment.Enabled {
			return nil, errEnrichmentUnavailable
		}
		seen[item] = true
		if bytes > 1<<20 {
			return nil, status.Error(codes.InvalidArgument, "edit TOML exceeds 1 MiB")
		}
		revision.Edits = append(revision.Edits, edit)
	}
	inputs, err := s.deps.DB.GetOntologyGeneration(ctx, o.ID, *parent.GenerationID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Error(codes.FailedPrecondition, "the parent generation is unavailable")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get parent generation: %v", err)
	}
	scope, err := s.ontologyVersionScope(ctx, parent)
	if err != nil {
		return nil, err
	}
	gen := &foundrymodel.OntologyGeneration{ID: id.New(), WorkspaceID: o.WorkspaceID, OntologyID: o.ID, Goal: inputs.Goal,
		Input: foundrymodel.OntologyGenerationInput{Tables: inputs.Input.Tables, Documents: inputs.Input.Documents, DocumentSetIDs: scope, Revise: revision}}
	if user := actor(ctx).UserID; user != "" {
		gen.RequestedByUserID = &user
	}
	if err := s.deps.DB.EnqueueOntologyGeneration(ctx, gen, s.deps.Cfg.Foundry.Ontology.PerHour); errors.Is(err, foundrystore.ErrConflict) {
		return nil, status.Error(codes.AlreadyExists, "an ontology generation is already queued or running")
	} else if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "ontology not found")
	} else if err != nil {
		return nil, status.Errorf(codes.Internal, "queue revision: %v", err)
	}
	// Just queued: no result to withhold.
	return s.ontologyGenerationResponse(ctx, o.ID, gen.ID, false)
}
