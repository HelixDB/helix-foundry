package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"unicode/utf8"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/connector"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/sample"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

const maxOntologyGoalChars = 2000

// GenerateOntology queues a draft of a version of the ontology for the
// foundry-ontology-worker, from the synced tables of the chosen connectors
// (all of them by default, none with no_connectors) and the chosen document
// sets of the workspace (none by default), at most maxDocumentSets sets,
// which the version it stores reads (its scope). Each set's table files
// whose read succeeded are tables of their own beside the connectors',
// recorded on the generation (a file still being read is left out); the
// worker shows the agent each set's newest text documents, so the draft
// routes them to the types, props and links they fill (a Contract linked to
// the tables' Customer). It needs a source, of either kind: a synced table,
// a table file, or a set holding text. Tables and table files count
// together toward foundry.ontology.max_tables. A goal gets a one-line title
// for lists. An ontology has at most one live generation, and a workspace
// starts at most foundry.ontology.per_hour of them an hour. A local install
// with no AI provider refuses it (FOUNDRY_AI_NOT_CONFIGURED).
//
// A sample generation drafts from the sample company's database (one
// connector, each of the sample's tables synced) and its documents' set,
// and the worker stores the sample's ready-made draft (package sample) in
// place of the AI's. Its title is the draft's, with no model called.
//
// Event sources are never among the connectors, chosen or by default: each
// comes in event_sources, with how this ontology reads it. Structured mode
// adds its events table, one of the tables, once its first sync has given it
// rows. Extraction adds the source's own document set (its newest pieces as
// Markdown documents, stream connector.EventsStream), one of the sets, once
// the source has a text field and a sync has made a document of a piece;
// document_set_ids never names such a set, so each is read once, and only
// with a mode. The stored input is the same either way, so readers tell an
// event source's table, or its set, by its connector's kind.
func (s *Service) GenerateOntology(ctx context.Context, req *foundryv1.GenerateOntologyRequest) (*foundryv1.OntologyGenerationResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	ws := o.WorkspaceID
	if utf8.RuneCountInString(req.GetGoal()) > maxOntologyGoalChars {
		return nil, status.Errorf(codes.InvalidArgument, "goal is longer than %d characters", maxOntologyGoalChars)
	}
	if req.GetNoConnectors() && len(req.GetConnectorIds()) > 0 {
		return nil, status.Error(codes.InvalidArgument, "connector_ids must be empty with no_connectors")
	}
	switch name := req.GetSample(); {
	case name == "":
	case name != sample.Kestrel:
		return nil, status.Errorf(codes.InvalidArgument, "no sample is called %q", name)
	case len(req.GetConnectorIds()) != 1 || len(req.GetDocumentSetIds()) != 1 || len(req.GetEventSources()) > 0:
		return nil, status.Error(codes.InvalidArgument, "a sample is drafted from its database and its documents: name its one connector and its one document set, and no event source")
	}
	if err := s.requireAI(ctx, ws); err != nil {
		return nil, err
	}
	// Refused before the tables are read; Enqueue still refuses one that races in.
	switch _, err := s.deps.DB.GetActiveOntologyGeneration(ctx, o.ID); {
	case err == nil:
		return nil, status.Error(codes.AlreadyExists, "an ontology generation is already queued or running")
	case !errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Errorf(codes.Internal, "get live ontology generation: %v", err)
	}
	connectors, err := s.deps.DB.ListConnectors(ctx, ws)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list connectors: %v", err)
	}
	byID := map[string]*foundrymodel.Connector{}
	for _, c := range connectors {
		byID[c.ID] = c
	}
	for _, want := range req.GetConnectorIds() {
		switch c, ok := byID[want]; {
		case !ok:
			return nil, status.Errorf(codes.NotFound, "connector %s not found", want)
		case c.Kind == foundrymodel.ConnectorKindEvents:
			return nil, status.Errorf(codes.InvalidArgument, "connector %s is an API source: choose it in event_sources, with a mode", want)
		}
	}
	gen := &foundrymodel.OntologyGeneration{ID: id.New(), WorkspaceID: ws, OntologyID: o.ID}
	for _, c := range connectors {
		if c.Kind == foundrymodel.ConnectorKindEvents || req.GetNoConnectors() || len(req.GetConnectorIds()) > 0 && !slices.Contains(req.GetConnectorIds(), c.ID) {
			continue
		}
		tables, err := s.deps.DB.ListConnectorTables(ctx, ws, c.ID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list connector %s tables: %v", c.ID, err)
		}
		for _, t := range tables {
			gen.Input.Tables = append(gen.Input.Tables, foundrymodel.OntologyGenerationTable{ConnectorID: c.ID, Table: t.TableName, SyncID: t.SyncID})
		}
	}
	sets, err := s.deps.DB.ListFoundryDocumentSets(ctx, ws)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list document sets: %v", err)
	}
	// wanted are the sets the generation reads: those chosen, and the own set
	// of each source it extracts.
	wanted := slices.Clone(req.GetDocumentSetIds())
	picked := map[string]bool{}
	for _, pick := range req.GetEventSources() {
		c, ok := byID[pick.GetConnectorId()]
		switch {
		case !ok:
			return nil, status.Errorf(codes.NotFound, "connector %s not found", pick.GetConnectorId())
		case c.Kind != foundrymodel.ConnectorKindEvents:
			return nil, status.Errorf(codes.InvalidArgument, "connector %s is not an API source: choose it in connector_ids", c.ID)
		case picked[c.ID]:
			return nil, status.Errorf(codes.InvalidArgument, "event_sources names the API source %s twice", c.ID)
		}
		picked[c.ID] = true
		switch pick.GetMode() {
		case foundryv1.FoundryEventSourceMode_FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED:
			tables, err := s.deps.DB.ListConnectorTables(ctx, ws, c.ID)
			if err != nil {
				return nil, status.Errorf(codes.Internal, "list connector %s tables: %v", c.ID, err)
			}
			// A sync over no batches publishes an empty table, so a table alone
			// is not enough: the draft needs rows to map.
			i := slices.IndexFunc(tables, func(t *foundrymodel.ConnectorTable) bool { return t.TableName == "events" && t.RowCount > 0 })
			if i < 0 {
				return nil, status.Errorf(codes.FailedPrecondition,
					"the API source %q has no pieces in its table yet: send some, and wait for its first sync (about a minute)", c.Name)
			}
			gen.Input.Tables = append(gen.Input.Tables, foundrymodel.OntologyGenerationTable{ConnectorID: c.ID, Table: tables[i].TableName, SyncID: tables[i].SyncID})
		case foundryv1.FoundryEventSourceMode_FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION:
			var stored struct {
				TextField string `json:"text_field"`
			}
			if err := json.Unmarshal(c.Config, &stored); err != nil {
				return nil, status.Errorf(codes.Internal, "read API source %s settings: %v", c.ID, err)
			}
			if stored.TextField == "" {
				return nil, status.Errorf(codes.FailedPrecondition, "the API source %q has no text field: choose the field that holds each piece's text "+
					"on its page in Foundry, then wait for its next sync", c.Name)
			}
			// A source's set appears with its first document, and a sync after
			// its text field is cleared empties it.
			i := slices.IndexFunc(sets, func(set *foundrymodel.FoundryDocumentSet) bool {
				return set.Owner != nil && set.Owner.ConnectorID == c.ID && set.Owner.Stream == connector.EventsStream
			})
			if i < 0 || sets[i].Documents == 0 {
				return nil, status.Errorf(codes.FailedPrecondition,
					"the API source %q has no pieces as documents yet: send some, and wait for its next sync (about a minute)", c.Name)
			}
			wanted = append(wanted, sets[i].ID)
		default:
			return nil, status.Errorf(codes.InvalidArgument, "choose how this ontology reads the API source %s: structured mappings or entity extraction", c.ID)
		}
	}
	for _, want := range req.GetDocumentSetIds() {
		i := slices.IndexFunc(sets, func(set *foundrymodel.FoundryDocumentSet) bool { return set.ID == want })
		switch {
		case i < 0:
			return nil, status.Errorf(codes.NotFound, "document set %s not found", want)
		case sets[i].Owner != nil && sets[i].Owner.ConnectorKind == foundrymodel.ConnectorKindEvents:
			return nil, status.Errorf(codes.InvalidArgument,
				"document set %s holds the API source %s's pieces: choose that source in event_sources with mode extraction", want, sets[i].Owner.ConnectorID)
		}
	}
	for _, set := range sets {
		if slices.Contains(wanted, set.ID) {
			gen.Input.DocumentSetIDs = append(gen.Input.DocumentSetIDs, set.ID)
		}
	}
	// The agent's turn is sized for a workspace's maxDocumentSets sets; one
	// that inherited more from its ontologies picks among them.
	if len(gen.Input.DocumentSetIDs) > maxDocumentSets {
		return nil, status.Errorf(codes.InvalidArgument, "a generation reads at most %d document sets; choose fewer", maxDocumentSets)
	}
	files, err := s.deps.DB.ListReadFoundryTableDocuments(ctx, ws, gen.Input.DocumentSetIDs)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list table files: %v", err)
	}
	for _, f := range files {
		gen.Input.Documents = append(gen.Input.Documents, foundrymodel.OntologyGenerationDocument{ID: f.ID, SetID: f.SetID})
	}
	if name := req.GetSample(); name != "" {
		for _, t := range sample.Tables {
			if !slices.ContainsFunc(gen.Input.Tables, func(g foundrymodel.OntologyGenerationTable) bool { return g.Table == t }) {
				return nil, status.Errorf(codes.FailedPrecondition, "the sample's table %s isn't synced: wait for its database's first sync", t)
			}
		}
		gen.Input.Sample = name
	}
	tables := len(gen.Input.Tables) + len(gen.Input.Documents)
	if tables == 0 {
		text, err := s.deps.DB.CountReadableFoundryTextDocuments(ctx, ws, gen.Input.DocumentSetIDs)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "count documents: %v", err)
		}
		if text == 0 {
			return nil, status.Error(codes.FailedPrecondition, "connect your data or upload documents first: a generation reads at least one synced table, "+
				"one CSV, TSV, JSON, JSONL or Parquet file that has been read, or one document set holding Markdown, MDX, text or a PDF whose text has been read")
		}
	}
	if limit := s.deps.Cfg.Foundry.Ontology.MaxTables; limit > 0 && tables > limit {
		return nil, status.Errorf(codes.InvalidArgument,
			"these connectors and sets have %d tables (%d synced, %d files) and one generation covers at most %d; choose fewer connectors or sets",
			tables, len(gen.Input.Tables), len(gen.Input.Documents), limit)
	}
	if o.CurrentVersionID != nil {
		gen.Input.BaseVersionID = *o.CurrentVersionID
	}
	if goal := req.GetGoal(); goal != "" {
		gen.Goal = &goal
	}
	if userID := actor(ctx).UserID; userID != "" {
		gen.RequestedByUserID = &userID
	}
	perHour := s.deps.Cfg.Foundry.Ontology.PerHour
	err = s.deps.DB.EnqueueOntologyGeneration(ctx, gen, perHour)
	switch {
	case errors.Is(err, foundrystore.ErrConflict):
		return nil, status.Error(codes.AlreadyExists, "an ontology generation is already queued or running")
	case errors.Is(err, foundrystore.ErrLimitExceeded):
		return nil, status.Errorf(codes.ResourceExhausted, "a workspace may start %d ontology generations an hour; try again later", perHour)
	case errors.Is(err, foundrystore.ErrNotFound):
		// Deleted, or its workspace was, since it was read above.
		return nil, status.Error(codes.NotFound, "ontology not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "queue ontology generation: %v", err)
	}
	// Titled once queued, so a request the cap refuses never pays for a title.
	// Waits on the title model at most ontologygen.TitleTimeout, and never
	// fails the generation. A sample's title is its draft's.
	if gen.Input.Sample != "" {
		if err := s.deps.DB.SetOntologyGenerationTitle(ctx, gen.ID, sample.Title); err != nil {
			log.From(ctx).Warn("title ontology generation failed", "generation", gen.ID, "err", err)
		}
	} else if gen.Goal != nil {
		if title := s.deps.GenerationTitles.Title(ctx, ws, gen.ID, *gen.Goal); title != "" {
			if err := s.deps.DB.SetOntologyGenerationTitle(ctx, gen.ID, title); err != nil {
				log.From(ctx).Warn("title ontology generation failed", "generation", gen.ID, "err", err)
			}
		}
	}
	// Just queued: no result to withhold.
	return s.ontologyGenerationResponse(ctx, o.ID, gen.ID, false)
}

func (s *Service) GetOntologyGeneration(ctx context.Context, req *foundryv1.GetOntologyGenerationRequest) (*foundryv1.OntologyGenerationResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	return s.ontologyGenerationResponse(ctx, o.ID, req.GetId(), loadEvidence(ctx, o))
}

func (s *Service) ListOntologyGenerations(ctx context.Context, req *foundryv1.ListOntologyGenerationsRequest) (*foundryv1.ListOntologyGenerationsResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = defaultOntologyListLimit
	}
	gens, err := s.deps.DB.ListOntologyGenerations(ctx, o.ID, min(limit, maxOntologyListLimit))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list ontology generations: %v", err)
	}
	resp := &foundryv1.ListOntologyGenerationsResponse{}
	for _, gen := range gens {
		// Lists read no revision's result.
		resp.Generations = append(resp.Generations, ontologyGenerationToProto(gen, false))
	}
	return resp, nil
}

func (s *Service) ontologyGenerationResponse(ctx context.Context, ontologyID, generationID string, evidence bool) (*foundryv1.OntologyGenerationResponse, error) {
	gen, err := s.deps.DB.GetOntologyGeneration(ctx, ontologyID, generationID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "ontology generation not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get ontology generation: %v", err)
	}
	return &foundryv1.OntologyGenerationResponse{Generation: ontologyGenerationToProto(gen, evidence)}, nil
}

var generationStatusToProto = map[foundrymodel.OntologyGenerationStatus]foundryv1.OntologyGenerationStatus{
	foundrymodel.OntologyGenerationQueued:    foundryv1.OntologyGenerationStatus_ONTOLOGY_GENERATION_STATUS_QUEUED,
	foundrymodel.OntologyGenerationRunning:   foundryv1.OntologyGenerationStatus_ONTOLOGY_GENERATION_STATUS_RUNNING,
	foundrymodel.OntologyGenerationSucceeded: foundryv1.OntologyGenerationStatus_ONTOLOGY_GENERATION_STATUS_SUCCEEDED,
	foundrymodel.OntologyGenerationFailed:    foundryv1.OntologyGenerationStatus_ONTOLOGY_GENERATION_STATUS_FAILED,
}

// ontologyGenerationToProto answers gen; without evidence (see loadEvidence),
// a revision's result is withheld, as it measures the ontology's load.
func ontologyGenerationToProto(gen *foundrymodel.OntologyGeneration, evidence bool) *foundryv1.OntologyGeneration {
	out := &foundryv1.OntologyGeneration{
		Id: gen.ID, WorkspaceId: gen.WorkspaceID, OntologyId: gen.OntologyID, Status: generationStatusToProto[gen.Status],
		Attempts:      int32(gen.Attempts),
		BaseVersionId: gen.Input.BaseVersionID,
		Sample:        gen.Input.Sample,
		CreatedAt:     timestamppb.New(gen.CreatedAt), NextAttemptAt: timestamppb.New(gen.NextAttemptAt),
	}
	for _, t := range gen.Input.Tables {
		out.Tables = append(out.Tables, &foundryv1.OntologyGenerationTable{ConnectorId: t.ConnectorID, Table: t.Table})
	}
	out.DocumentSetIds = gen.Input.DocumentSetIDs
	for _, d := range gen.Input.Documents {
		out.Documents = append(out.Documents, &foundryv1.OntologyGenerationDocument{Id: d.ID, SetId: d.SetID})
	}
	if rev := gen.Input.Revise; rev != nil {
		out.Revision = &foundryv1.OntologyRevision{ParentVersionId: rev.ParentVersionID, DryRun: rev.DryRun}
		switch {
		case len(gen.ReviseResult) == 0:
		case evidence:
			out.Revision.Result = &foundryv1.OntologyRevision_ResultJson{ResultJson: string(gen.ReviseResult)}
		default:
			out.Revision.Result = &foundryv1.OntologyRevision_ResultWithheld{ResultWithheld: true}
		}
	}
	for _, f := range []struct {
		src *string
		dst *string
	}{{gen.Goal, &out.Goal}, {gen.Title, &out.Title}, {gen.Error, &out.Error}, {gen.ResultVersionID, &out.ResultVersionId},
		{gen.Model, &out.Model}, {gen.RequestedByUserID, &out.RequestedByUserId}} {
		if f.src != nil {
			*f.dst = *f.src
		}
	}
	if gen.StartedAt != nil {
		out.StartedAt = timestamppb.New(*gen.StartedAt)
	}
	if gen.FinishedAt != nil {
		out.FinishedAt = timestamppb.New(*gen.FinishedAt)
	}
	return out
}
