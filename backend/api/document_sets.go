package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

const (
	maxDocumentSetNameChars        = 120
	maxDocumentSetDescriptionChars = 1000
	// maxDocumentSets bounds the live sets a create may bring a workspace to:
	// a generation gives the agent each set it picks as a source with up to
	// ontologyagent.MaxSetDocuments documents. A workspace whose ontologies'
	// sets became its own may hold more; it can't create another then.
	maxDocumentSets = 20
)

// CreateFoundryDocumentSet adds a named set to the workspace, a folder of
// the Data tree for documents read together (contracts, say, apart from
// everything else). Names are unique among the workspace's live sets, case
// and accents aside: AlreadyExists otherwise. A workspace creates up to
// maxDocumentSets. The ontology-scoped alias makes the same workspace set.
func (s *Service) CreateFoundryDocumentSet(ctx context.Context, req *foundryv1.CreateFoundryDocumentSetRequest) (*foundryv1.FoundryDocumentSetResponse, error) {
	return s.createFoundryDocumentSet(ctx, "", req)
}

func (s *Service) createFoundryDocumentSet(ctx context.Context, ontologyID string, req *foundryv1.CreateFoundryDocumentSetRequest) (*foundryv1.FoundryDocumentSetResponse, error) {
	name, err := documentSetName(req.GetName())
	if err != nil {
		return nil, err
	}
	description, err := documentSetDescription(req.GetDescription())
	if err != nil {
		return nil, err
	}
	ws, err := s.foundryDocumentsWorkspace(ctx, req.GetWorkspaceId(), ontologyID)
	if err != nil {
		return nil, err
	}
	set := &foundrymodel.FoundryDocumentSet{ID: id.New(), WorkspaceID: ws, Name: name, Description: description}
	if userID := actor(ctx).UserID; userID != "" {
		set.CreatedByUserID = &userID
	}
	switch err := s.deps.DB.CreateFoundryDocumentSet(ctx, set, maxDocumentSets); {
	case errors.Is(err, foundrystore.ErrConflict):
		return nil, status.Errorf(codes.AlreadyExists, "the workspace already has a document set named %q", name)
	case errors.Is(err, foundrystore.ErrLimitExceeded):
		return nil, status.Errorf(codes.FailedPrecondition, "a workspace holds up to %d document sets; delete an empty one first", maxDocumentSets)
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "workspace not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "create document set: %v", err)
	}
	return s.foundryDocumentSetResponse(ctx, ws, set.ID)
}

// ListFoundryDocumentSets returns the workspace's live sets, oldest first,
// each with its count of documents that are not deleted, the ontologies
// that read it (read_by): those whose reading version (foundryReader) has it
// in scope, with whether that version's routes name it, and, for a set with
// an allowance, what work no person started spent on it this month
// (foundryDocumentSetProtos).
func (s *Service) ListFoundryDocumentSets(ctx context.Context, req *foundryv1.ListFoundryDocumentSetsRequest) (*foundryv1.ListFoundryDocumentSetsResponse, error) {
	return s.listFoundryDocumentSets(ctx, "", req)
}

func (s *Service) listFoundryDocumentSets(ctx context.Context, ontologyID string, req *foundryv1.ListFoundryDocumentSetsRequest) (*foundryv1.ListFoundryDocumentSetsResponse, error) {
	ws, err := s.foundryDocumentsWorkspace(ctx, req.GetWorkspaceId(), ontologyID)
	if err != nil {
		return nil, err
	}
	sets, err := s.deps.DB.ListFoundryDocumentSets(ctx, ws)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list document sets: %v", err)
	}
	readers, err := s.foundryReaders(ctx, ws)
	if err != nil {
		return nil, err
	}
	out, err := s.foundryDocumentSetProtos(ctx, ws, sets)
	if err != nil {
		return nil, err
	}
	for i, set := range sets {
		for _, r := range readers {
			if slices.Contains(r.scope, set.ID) {
				out[i].ReadBy = append(out[i].ReadBy, &foundryv1.FoundryDocumentSetReader{OntologyId: r.ontology.ID, Routed: r.routes.Routes(set.ID)})
			}
		}
	}
	return &foundryv1.ListFoundryDocumentSetsResponse{Sets: out}, nil
}

// UpdateFoundryDocumentSet renames a set or changes its description; a field
// left unset stays as it is. Scopes and routes name sets by id, so a rename
// changes what no version reads. A connector's set is FailedPrecondition
// (connectorOwned).
func (s *Service) UpdateFoundryDocumentSet(ctx context.Context, req *foundryv1.UpdateFoundryDocumentSetRequest) (*foundryv1.FoundryDocumentSetResponse, error) {
	return s.updateFoundryDocumentSet(ctx, "", req)
}

func (s *Service) updateFoundryDocumentSet(ctx context.Context, ontologyID string, req *foundryv1.UpdateFoundryDocumentSetRequest) (*foundryv1.FoundryDocumentSetResponse, error) {
	var update foundrymodel.FoundryDocumentSetUpdate
	if req.Name != nil {
		name, err := documentSetName(req.GetName())
		if err != nil {
			return nil, err
		}
		update.Name = &name
	}
	if req.Description != nil {
		description, err := documentSetDescription(req.GetDescription())
		if err != nil {
			return nil, err
		}
		update.Description = &description
	}
	ws, err := s.foundryDocumentsWorkspace(ctx, req.GetWorkspaceId(), ontologyID)
	if err != nil {
		return nil, err
	}
	switch err := s.deps.DB.UpdateFoundryDocumentSet(ctx, ws, req.GetSetId(), update); {
	case errors.Is(err, foundrystore.ErrConnectorOwned):
		return nil, s.connectorOwned(ctx, ws, req.GetSetId(), "set")
	case errors.Is(err, foundrystore.ErrConflict):
		return nil, status.Errorf(codes.AlreadyExists, "the workspace already has a document set named %q", *update.Name)
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "document set not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "update document set: %v", err)
	}
	return s.foundryDocumentSetResponse(ctx, ws, req.GetSetId())
}

// DeleteFoundryDocumentSet deletes a set that holds no document:
// FailedPrecondition while one is there, to move or delete first, and for a
// connector's set, which lives as long as the connector (connectorOwned). A
// version whose scope names the set reads nothing more through it.
func (s *Service) DeleteFoundryDocumentSet(ctx context.Context, req *foundryv1.DeleteFoundryDocumentSetRequest) (*foundryv1.DeleteFoundryDocumentSetResponse, error) {
	return s.deleteFoundryDocumentSet(ctx, "", req)
}

func (s *Service) deleteFoundryDocumentSet(ctx context.Context, ontologyID string, req *foundryv1.DeleteFoundryDocumentSetRequest) (*foundryv1.DeleteFoundryDocumentSetResponse, error) {
	ws, err := s.foundryDocumentsWorkspace(ctx, req.GetWorkspaceId(), ontologyID)
	if err != nil {
		return nil, err
	}
	switch err := s.deps.DB.DeleteFoundryDocumentSet(ctx, ws, req.GetSetId()); {
	case errors.Is(err, foundrystore.ErrConnectorOwned):
		return nil, s.connectorOwned(ctx, ws, req.GetSetId(), "set")
	case errors.Is(err, foundrystore.ErrNotEmpty):
		return nil, status.Error(codes.FailedPrecondition, "the document set still holds documents; move or delete them first")
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "document set not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "delete document set: %v", err)
	}
	return &foundryv1.DeleteFoundryDocumentSetResponse{}, nil
}

// connectorOwned is FailedPrecondition for a person's change to a connector's
// set, or to one of its documents (what: "set" or "document"), of set setID
// (foundrystore.ErrConnectorOwned). Only the connector's syncs write them: what
// the set holds changes with the connection's settings.
func (s *Service) connectorOwned(ctx context.Context, workspaceID, setID, what string) error {
	connection := "a connection"
	if set, err := s.deps.DB.GetFoundryDocumentSet(ctx, workspaceID, setID); err == nil && set.Owner != nil {
		connection = fmt.Sprintf("the %s connection", set.Owner.ConnectorName)
	}
	return status.Errorf(codes.FailedPrecondition, "this %s belongs to %s; change its settings there", what, connection)
}

// foundryDocumentsWorkspace is the workspace a document or set route acts
// on. An ontology-scoped alias (ontologyID set, until release 2 of workspace
// documents) acts on the workspace's documents and sets too, and needs a
// live ontology of it, as the route did before.
func (s *Service) foundryDocumentsWorkspace(ctx context.Context, workspaceID, ontologyID string) (string, error) {
	if workspaceID == "" {
		return "", status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	if ontologyID == "" {
		return workspaceID, nil
	}
	o, err := s.liveFoundryOntology(ctx, workspaceID, ontologyID)
	if err != nil {
		return "", err
	}
	return o.WorkspaceID, nil
}

// documentSetName trims a set's name, which must then be 1 to 120 characters.
func documentSetName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxDocumentSetNameChars {
		return "", status.Errorf(codes.InvalidArgument, "name must be 1 to %d characters", maxDocumentSetNameChars)
	}
	return name, nil
}

// documentSetDescription trims a set's description, which must then be at
// most 1000 characters.
func documentSetDescription(description string) (string, error) {
	description = strings.TrimSpace(description)
	if utf8.RuneCountInString(description) > maxDocumentSetDescriptionChars {
		return "", status.Errorf(codes.InvalidArgument, "description must be at most %d characters", maxDocumentSetDescriptionChars)
	}
	return description, nil
}

func (s *Service) foundryDocumentSetResponse(ctx context.Context, workspaceID, setID string) (*foundryv1.FoundryDocumentSetResponse, error) {
	set, err := s.deps.DB.GetFoundryDocumentSet(ctx, workspaceID, setID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "document set not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get document set: %v", err)
	}
	out, err := s.foundryDocumentSetProtos(ctx, workspaceID, []*foundrymodel.FoundryDocumentSet{set})
	if err != nil {
		return nil, err
	}
	return &foundryv1.FoundryDocumentSetResponse{Set: out[0]}, nil
}

// foundryReader is the version that reads an ontology's new uploads: its
// current load's, as an upload is extracted into the current load, or with
// no load current its current version; none before either. scope is the
// sets that version reads (empty with none), routes the sets its extraction
// routes name. A version whose mappings can't be read routes nothing; that
// is logged, not refused.
type foundryReader struct {
	ontology *foundrymodel.FoundryOntology
	scope    []string
	routes   ontologyagent.DocumentRoutes
}

// foundryReaderOf finds o's reading version and reads its scope and routes.
func (s *Service) foundryReaderOf(ctx context.Context, o *foundrymodel.FoundryOntology) (foundryReader, error) {
	reader := foundryReader{ontology: o}
	versionID := o.CurrentVersionID
	if o.CurrentLoadID != nil {
		load, err := s.deps.DB.GetFoundryLoad(ctx, o.ID, *o.CurrentLoadID)
		if err != nil {
			return reader, status.Errorf(codes.Internal, "get current load: %v", err)
		}
		versionID = &load.VersionID
	}
	if versionID == nil {
		return reader, nil
	}
	scope, err := s.deps.DB.GetOntologyVersionScope(ctx, o.ID, *versionID)
	if err != nil {
		return reader, status.Errorf(codes.Internal, "get ontology version scope: %v", err)
	}
	mappings, err := s.deps.DB.GetOntologyVersionMappings(ctx, o.ID, *versionID)
	if err != nil {
		return reader, status.Errorf(codes.Internal, "get ontology version mappings: %v", err)
	}
	reader.scope = scope
	if reader.routes, err = ontologyagent.ParseDocumentRoutes(mappings); err != nil {
		log.From(ctx).Warn("read document routes failed", "ontology", o.ID, "version", *versionID, "err", err)
	}
	return reader, nil
}

// foundryReaders are the reading versions of the workspace's live
// ontologies, newest ontology first. Each costs a few reads; a workspace has
// few ontologies (every one holds a database).
func (s *Service) foundryReaders(ctx context.Context, workspaceID string) ([]foundryReader, error) {
	var readers []foundryReader
	for page := ""; ; {
		ontologies, next, err := s.deps.DB.ListFoundryOntologies(ctx, workspaceID, page, foundryPageSize)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list ontologies: %v", err)
		}
		for _, o := range ontologies {
			reader, err := s.foundryReaderOf(ctx, o)
			if err != nil {
				return nil, err
			}
			readers = append(readers, reader)
		}
		if next == "" {
			return readers, nil
		}
		page = next
	}
}

// foundryDocumentSetProtos maps sets, in order. A connector's set says
// whose it is and the most documents it holds: an API source's set its
// source's newest foundry.events.max_extracted pieces, at every commit, any
// other foundry.documents.max_per_stream. A set whose connector's kind has
// an allowance (config.FoundryConfig.SetAllowances) also says what work no
// person started spent extracting it this UTC month, its allowance, and
// whether its syncs are paused, as the syncer decides (syncer's pause): it
// holds documents, and its allowance or its workspace's is spent
// (foundrymodel.FoundryDocumentSetSpend.Left), or the month leaves work no person
// started nothing (foundrymodel.FoundryGraphBudget.CapFor). What that work spent
// is read for all such sets in one read, with one of the month's graph
// spend, and not at all when none is among them.
func (s *Service) foundryDocumentSetProtos(ctx context.Context, workspaceID string, sets []*foundrymodel.FoundryDocumentSet) ([]*foundryv1.FoundryDocumentSet, error) {
	cfg := s.deps.Cfg.Foundry
	allowances := cfg.SetAllowances()
	var allowed []string
	for _, set := range sets {
		if set.Owner == nil {
			continue
		}
		if _, ok := allowances[set.Owner.ConnectorKind]; ok {
			allowed = append(allowed, set.ID)
		}
	}
	spend := map[string]foundrymodel.FoundryDocumentSetSpend{}
	// monthSpent is whether the month leaves work no person started nothing.
	monthSpent := false
	if len(allowed) > 0 {
		now := time.Now()
		var err error
		if spend, err = s.deps.DB.FoundryDocumentSetSpend(ctx, workspaceID, allowed, now); err != nil {
			return nil, status.Errorf(codes.Internal, "read document set spend: %v", err)
		}
		budget := cfg.Graph.MonthBudget(now)
		spent, err := s.deps.DB.SumFoundryGraphJobSpend(ctx, workspaceID, budget.Since)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read the month's graph spend: %v", err)
		}
		monthSpent = spent >= budget.CapFor(nil)
	}
	out := make([]*foundryv1.FoundryDocumentSet, len(sets))
	for i, set := range sets {
		out[i] = &foundryv1.FoundryDocumentSet{
			Id: set.ID, Name: set.Name, Description: set.Description, Documents: int32(set.Documents), CreatedAt: timestamppb.New(set.CreatedAt),
		}
		owner := set.Owner
		if owner == nil {
			continue
		}
		c := &foundryv1.FoundryDocumentSetConnector{
			ConnectorId: owner.ConnectorID, Stream: owner.Stream, MaxDocuments: int32(cfg.Documents.MaxPerStream), ConnectorName: owner.ConnectorName,
			ConnectorKind: owner.ConnectorKind,
		}
		if owner.ConnectorKind == foundrymodel.ConnectorKindEvents {
			c.MaxDocuments = int32(cfg.Events.MaxExtracted)
		}
		if allowance, ok := allowances[owner.ConnectorKind]; ok {
			c.MonthSystemUsdMicros, c.MaxMonthSystemUsdMicros = spend[set.ID].USDMicros, &allowance.SetMicros
			c.Paused = set.Documents > 0 && (spend[set.ID].Left(allowance) <= 0 || monthSpent)
		}
		out[i].Connector = c
	}
	return out, nil
}
