package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"

	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/events"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/text/unicode/norm"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Foundry's actions take the same two calls as every MCP mutation: prepare returns a five-minute,
// one-time confirmation bound to the operation, its target and its exact payload; Execute
// consumes it and makes the call once. The backend's own guards still hold: one live sync per
// connector and generation per ontology, the hourly generation cap, the load budget, and the
// current version's compare-and-set.
const (
	toolPrepareFoundry = "helix_prepare_foundry_operation"
	toolExecuteFoundry = "helix_execute_foundry_operation"
	// toolFoundryCompleteUpload ends the upload upload_document started. It is not confirmed on
	// its own: it only checks the bytes the confirmed upload declared, and completing twice is
	// no error.
	toolFoundryCompleteUpload = "helix_foundry_complete_document_upload"
)

// foundryOperationPrefix keeps a Foundry confirmation from ever matching another tool's operation.
const foundryOperationPrefix = "foundry:"

// The backend's limits, checked here so a confirmation is never prepared for a payload the
// backend would refuse.
const (
	maxFoundryNameChars      = 120
	maxFoundryGoalChars      = 2000
	maxFoundrySetDescription = 1000
	maxFoundryEdits          = 100
	maxFoundryEditItemBytes  = 256
	maxFoundryEditTOMLBytes  = 1 << 20
	maxFoundryReviewIDBytes  = 256
	// maxFoundrySlugChars is the longest slug the website gives a database or project, inside
	// the backend's 64.
	maxFoundrySlugChars = 63
	// maxFoundrySlugStem is how much of a taken database slug its retry keeps before
	// "-<6 hex>", so the retry is still at most maxFoundrySlugChars.
	maxFoundrySlugStem = 56
)

// FoundryNoHelixSubscription refuses create_ontology's new database in a Workspace whose Helix
// subscription the backend's gate refuses: at prepare, from the host's Directory.AuthorizeCreate,
// and at Execute, from CreateTenant, should it lapse in between. lapse is model.WorkspaceSubscription.Lapse's word for why ("" when the
// backend didn't say). Only a Workspace with none may get one from Foundry, whose wizard gives
// a Foundry Workspace its free plan; one that lapsed is settled in the Helix dashboard.
func FoundryNoHelixSubscription(lapse string) string {
	otherwise := "; or create the ontology on a database the workspace already has, with target tenant:<id>"
	if lapse == "none" {
		return "the workspace needs a Helix subscription for a new database on the shared cluster: open Foundry or the Helix dashboard to get one" + otherwise
	}
	if lapse != "" {
		lapse = " (" + lapse + ")"
	}
	return "the workspace's Helix subscription isn't active" + lapse + ", and a new database on the shared cluster needs one that is: " +
		"an owner or admin settles it or chooses a plan in the Helix dashboard" + otherwise
}

// foundrySlugPattern is a slug as the website makes one: lowercase letters and digits in runs
// joined by single dashes.
var (
	foundrySlugPattern    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	foundrySlugSeparators = regexp.MustCompile(`[^a-z0-9]+`)
)

// FoundryClients is Foundry's API as the MCP calls it: FoundryService, and FoundryEventsService
// for the pieces helix_foundry_send_pieces sends.
type FoundryClients struct {
	foundryv1.FoundryServiceClient
	foundryv1.FoundryEventsServiceClient
}

// FoundryActionAPI is Foundry's mutations.
type FoundryActionAPI interface {
	CreateFoundryOntology(context.Context, *foundryv1.CreateFoundryOntologyRequest, ...grpc.CallOption) (*foundryv1.FoundryOntologyResponse, error)
	SyncConnector(context.Context, *foundryv1.SyncConnectorRequest, ...grpc.CallOption) (*foundryv1.ConnectorSyncResponse, error)
	GenerateOntology(context.Context, *foundryv1.GenerateOntologyRequest, ...grpc.CallOption) (*foundryv1.OntologyGenerationResponse, error)
	SetCurrentOntologyVersion(context.Context, *foundryv1.SetCurrentOntologyVersionRequest, ...grpc.CallOption) (*foundryv1.OntologyVersionResponse, error)
	CreateFoundryLoad(context.Context, *foundryv1.CreateFoundryLoadRequest, ...grpc.CallOption) (*foundryv1.FoundryLoadResponse, error)
	SetCurrentFoundryLoad(context.Context, *foundryv1.SetCurrentFoundryLoadRequest, ...grpc.CallOption) (*foundryv1.FoundryLoadResponse, error)
	ReviseOntologyVersion(context.Context, *foundryv1.ReviseOntologyVersionRequest, ...grpc.CallOption) (*foundryv1.OntologyGenerationResponse, error)
	// The document and set writes go through their ontology-scoped routes: each tool names the
	// ontology, which they find live in the Workspace before acting on its documents and sets.
	CreateFoundryOntologyDocument(context.Context, *foundryv1.CreateFoundryOntologyDocumentRequest, ...grpc.CallOption) (
		*foundryv1.CreateFoundryOntologyDocumentResponse, error)
	CompleteFoundryOntologyDocumentUpload(context.Context, *foundryv1.CompleteFoundryOntologyDocumentUploadRequest, ...grpc.CallOption) (
		*foundryv1.FoundryDocumentReadsResponse, error)
	CreateFoundryOntologyDocumentSet(context.Context, *foundryv1.CreateFoundryOntologyDocumentSetRequest, ...grpc.CallOption) (
		*foundryv1.FoundryDocumentSetResponse, error)
	UpdateFoundryOntologyDocumentSet(context.Context, *foundryv1.UpdateFoundryOntologyDocumentSetRequest, ...grpc.CallOption) (
		*foundryv1.FoundryDocumentSetResponse, error)
	MoveFoundryOntologyDocument(context.Context, *foundryv1.MoveFoundryOntologyDocumentRequest, ...grpc.CallOption) (
		*foundryv1.FoundryDocumentReadsResponse, error)
	SettleFoundryReview(context.Context, *foundryv1.SettleFoundryReviewRequest, ...grpc.CallOption) (*foundryv1.FoundryReviewDecisionResponse, error)
	CancelFoundryQuestion(context.Context, *foundryv1.CancelFoundryQuestionRequest, ...grpc.CallOption) (*foundryv1.FoundryQuestionResponse, error)
	// An event source, made by create_event_source, and the pieces helix_foundry_send_pieces sends it.
	CreateConnector(context.Context, *foundryv1.CreateConnectorRequest, ...grpc.CallOption) (*foundryv1.ConnectorResponse, error)
	SendFoundryEvents(context.Context, *foundryv1.SendFoundryEventsRequest, ...grpc.CallOption) (*foundryv1.SendFoundryEventsResponse, error)
}

// FoundryOperationInput is described by foundryOperationSchema, which says an API source's parts
// only where the host has them.
type FoundryOperationInput struct {
	Operation string         `json:"operation"`
	Target    string         `json:"target"`
	Payload   map[string]any `json:"payload"`
}

type ExecuteFoundryInput struct {
	Operation         string         `json:"operation" jsonschema:"The operation prepared."`
	Target            string         `json:"target" jsonschema:"The target prepared."`
	Payload           map[string]any `json:"payload" jsonschema:"The payload prepared, unchanged."`
	ConfirmationID    string         `json:"confirmation_id"`
	ConfirmationToken string         `json:"confirmation_token"`
}

type FoundryOperationOutput struct {
	ContentTrust string `json:"content_trust"`
	Operation    string `json:"operation"`
	Target       string `json:"target"`
	// See FoundryVersionOutput.Definition: the description keeps the schema an object.
	Result any `json:"result" jsonschema:"Untrusted result: the ontology, sync, generation, version, load, document, document set or review decision the operation made or changed."`
}

type CompleteFoundryUploadInput struct {
	OntologyInput
	DocumentID string `json:"document_id" jsonschema:"The document upload_document created."`
}

type CompleteFoundryUploadOutput struct {
	ContentTrust string                `json:"content_trust"`
	Document     FoundryDocumentOutput `json:"document"`
	// Reads are what each ontology that reads the document's set did with it.
	Reads []FoundryDocumentReadOutput `json:"reads"`
}

// FoundryDocumentReadOutput is one ontology's take on an uploaded or moved document.
type FoundryDocumentReadOutput struct {
	OntologyID string `json:"ontology_id"`
	Outcome    string `json:"outcome" jsonschema:"queued (an extract will read it), owed (the job running now reads it, or its end queues the extract that does), no_budget (the month's extraction budget is spent; the next load reads it), held (the current load already has it), skipped (the caller may not write that ontology's database) or reading (a PDF whose text is still being read: the load holds it and reads it once the read ends)."`
}

// FoundryDocumentReadsOutput is a moved document and what each ontology did with it.
type FoundryDocumentReadsOutput struct {
	Document FoundryDocumentOutput       `json:"document"`
	Reads    []FoundryDocumentReadOutput `json:"reads"`
}

// FoundryOperation is one validated and authorized action. Its payload is canonical, so the
// same action hashes the same at prepare and Execute.
type FoundryOperation interface {
	CanonicalPayload() []byte
	// Execute makes the backend call once. confirmationID is the client token of calls that
	// take one, so the call's own idempotency is bound to the consumed confirmation.
	Execute(ctx context.Context, s *Service, identity *Identity, confirmationID string) (any, error)
}

// AddFoundryActionTools registers prepare and Execute for a user who may manage connectors or
// ontologies, and for a service key with database.query.write on some project.
func (s *Service) AddFoundryActionTools(server *mcp.Server, identity *Identity) {
	switch {
	case identity.Agent:
		return
	case identity.ServiceKey:
		if !identity.HasPermission(PermissionDatabaseQueryWrite) {
			return
		}
	case !identity.HasPermission(PermissionWorkspaceConnectorsManage) && !identity.HasPermission(PermissionWorkspaceOntologyManage):
		return
	}
	prepare := queryTool(toolPrepareFoundry,
		"Validate a Foundry action and prepare a five-minute, one-time confirmation for exactly that action. A create_ontology on workspace:<id> makes the ontology's database first, and its project when new.", false)
	prepare.InputSchema = foundryOperationSchema(s.Events)
	addFoundryTool(s, server, identity, prepare, s.PrepareFoundryOperation)
	addFoundryTool(s, server, identity, queryTool(toolExecuteFoundry,
		"Consume a matching one-time confirmation, then run exactly one Foundry action, without retry.", false), s.ExecuteFoundryOperation)
	no := false
	addFoundryTool(s, server, identity, &mcp.Tool{Name: toolFoundryCompleteUpload,
		Description: "After an upload_document action, once its bytes are PUT to the upload URL: check them and add the document to its document set, which every ontology whose current version reads the set then reads (reads says how). Completing twice is no error.",
		Annotations: &mcp.ToolAnnotations{IdempotentHint: true, DestructiveHint: &no, OpenWorldHint: &no},
	}, s.CompleteFoundryUpload)
}

func (s *Service) CompleteFoundryUpload(
	ctx context.Context, identity *Identity, call *Call, input CompleteFoundryUploadInput,
) (CompleteFoundryUploadOutput, int, error) {
	call.ResourceKind, call.ResourceID = "ontology", input.OntologyID
	if !ValidIDs([]string{input.WorkspaceID, input.OntologyID, input.DocumentID}) {
		return CompleteFoundryUploadOutput{}, 0, errCodeInvalidArguments
	}
	req := &foundryv1.CompleteFoundryOntologyDocumentUploadRequest{WorkspaceId: input.WorkspaceID, OntologyId: input.OntologyID, Id: input.DocumentID}
	if err := s.AuthorizeFoundry(ctx, identity, req, QueryWrite,
		"CompleteFoundryOntologyDocumentUpload"); err != nil {
		return CompleteFoundryUploadOutput{}, 0, err
	}
	ctx = identity.WithScope(ctx, input.WorkspaceID, true)
	response, err := s.Actions.CompleteFoundryOntologyDocumentUpload(ctx, req)
	if err != nil {
		return CompleteFoundryUploadOutput{}, 0, FoundryError(err)
	}
	return CompleteFoundryUploadOutput{ContentTrust: contentTrustUntrusted, Document: FoundryDocument(response.GetDocument()),
		Reads: FoundryDocumentReads(response.GetReads())}, 1, nil
}

func FoundryDocumentReads(reads []*foundryv1.FoundryDocumentRead) []FoundryDocumentReadOutput {
	out := make([]FoundryDocumentReadOutput, 0, len(reads))
	for _, r := range reads {
		out = append(out, FoundryDocumentReadOutput{OntologyID: r.GetOntologyId(), Outcome: EnumName(r.GetOutcome().String(), "FOUNDRY_DOCUMENT_READ_OUTCOME_")})
	}
	return out
}

func (s *Service) PrepareFoundryOperation(
	ctx context.Context, identity *Identity, call *Call, input FoundryOperationInput,
) (confirmationOutput, int, error) {
	call.ResourceKind, call.ResourceID = TargetKind(input.Target), input.Target
	operation, err := s.ValidateFoundryOperation(ctx, identity, input)
	if err != nil {
		return confirmationOutput{}, 0, err
	}
	payloadHash := confirmationPayloadHash(foundryOperationPrefix+input.Operation, input.Target, operation.CanonicalPayload())
	prepared, err := s.Confirmations.Prepare(ctx, ConfirmationBinding{
		Operation: foundryOperationPrefix + input.Operation, Target: input.Target, Audience: s.ResourceURI, PayloadHash: payloadHash,
	})
	if err != nil {
		return confirmationOutput{}, 0, FoundryError(err)
	}
	return confirmationOutput{
		ConfirmationID: prepared.ID, ConfirmationToken: prepared.Token, PayloadHash: payloadHash,
		ExpiresAt: prepared.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}, 1, nil
}

func (s *Service) ExecuteFoundryOperation(
	ctx context.Context, identity *Identity, call *Call, input ExecuteFoundryInput,
) (FoundryOperationOutput, int, error) {
	call.ResourceKind, call.ResourceID = TargetKind(input.Target), input.Target
	operation, err := s.ValidateFoundryOperation(ctx, identity, FoundryOperationInput{
		Operation: input.Operation, Target: input.Target, Payload: input.Payload,
	})
	if err != nil {
		return FoundryOperationOutput{}, 0, err
	}
	if input.ConfirmationID == "" || input.ConfirmationToken == "" {
		return FoundryOperationOutput{}, 0, detailedToolError{code: errCodeInvalidArguments, message: "confirmation_id and confirmation_token are required"}
	}
	err = s.Confirmations.Consume(ctx, ConfirmationBinding{
		ID: input.ConfirmationID, Token: input.ConfirmationToken, Operation: foundryOperationPrefix + input.Operation,
		Target: input.Target, Audience: s.ResourceURI,
		PayloadHash: confirmationPayloadHash(foundryOperationPrefix+input.Operation, input.Target, operation.CanonicalPayload()),
	})
	if err != nil {
		return FoundryOperationOutput{}, 0, FoundryError(err)
	}
	result, err := operation.Execute(ctx, s, identity, input.ConfirmationID)
	if err != nil {
		return FoundryOperationOutput{}, 0, FoundryError(err)
	}
	return FoundryOperationOutput{ContentTrust: contentTrustUntrusted, Operation: input.Operation, Target: input.Target, Result: result}, 1, nil
}

// ValidateFoundryOperation parses an action's target and strict payload, then authorizes the
// caller for it: a user by the WFE method's registry entry, a key by its grant on the project
// of the ontology's database. create_ontology, create_event_source, sync_connector,
// generate_ontology and settle_review are user-only. create_event_source is the one connector
// an agent may make: an event source has no credential to pass through it. A create_ontology
// on a new database is authorized for every call the website's wizard makes: CreateProject
// when it makes the project, CreateTenant and CreateFoundryOntology; and it needs the Helix
// subscription CreateTenant's gate requires.
func (s *Service) ValidateFoundryOperation(ctx context.Context, identity *Identity, input FoundryOperationInput) (FoundryOperation, error) {
	invalid := func(message string) error { return detailedToolError{code: errCodeInvalidArguments, message: message} }
	raw, err := json.Marshal(input.Payload)
	if input.Payload == nil || err != nil {
		return nil, invalid("operation, target and a payload object are required")
	}
	writeGrant := QueryWrite
	switch input.Operation {
	case "create_ontology":
		var payload CreateOntologyPayload
		malformed := invalid("create_ontology takes target tenant:<id> and payload {name}, or target workspace:<workspace_id> and payload " +
			"{name, database: {name, slug, project_id or project_name}}: names of 1 to 120 characters, a slug of lowercase letters, digits and single dashes, at most 63")
		if decodeStrict(raw, &payload) != nil || !ValidFoundryName(payload.Name) {
			return nil, malformed
		}
		if workspaceID, ok := cutTarget(input.Target, "workspace"); ok {
			database := payload.Database
			if database == nil || !ValidFoundryName(database.Name) || len(database.Slug) > maxFoundrySlugChars || !foundrySlugPattern.MatchString(database.Slug) ||
				(database.ProjectID == "") == (database.ProjectName == "") || !ValidIDs(nil, database.ProjectID) ||
				(database.ProjectName != "" && !ValidFoundryName(database.ProjectName)) {
				return nil, malformed
			}
			// As foundryUserOnly on tenant:<id>. Its other calls are the directory's, which the
			// host checks; this holds whatever the host's check reads.
			if !identity.User || identity.ServiceKey || identity.Agent || !identity.HasPermission(PermissionWorkspaceOntologyManage) {
				return nil, errCodeNotFound
			}
			if err := s.Directory.AuthorizeCreate(ctx, workspaceID, database.ProjectID); err != nil {
				return nil, err
			}
			return NewDatabaseOntologyOperation{workspaceID: workspaceID, payload: payload}, nil
		}
		tenantID, ok := cutTarget(input.Target, "tenant")
		if !ok || payload.Database != nil {
			return nil, malformed
		}
		workspaceID, err := s.Directory.TenantWorkspace(ctx, tenantID)
		if status.Code(err) == codes.NotFound {
			return nil, errCodeNotFound
		}
		if err != nil {
			// Not not_found: an agent told the database doesn't exist may make another.
			if s.Logger != nil {
				s.Logger.Error("Foundry database lookup failed", "error_code", "tenant_lookup_failed", "error", err)
			}
			return nil, errCodeBackendUnavailable
		}
		req := &foundryv1.CreateFoundryOntologyRequest{WorkspaceId: workspaceID, TenantId: tenantID, Name: payload.Name}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "CreateFoundryOntology"); err != nil {
			return nil, err
		}
		return CreateOntologyOperation{payload: payload, req: req}, nil
	case "create_event_source":
		if !s.Events {
			return nil, status.Error(codes.FailedPrecondition, "API sources are not available in local mode")
		}
		var payload CreateEventSourcePayload
		workspaceID, ok := cutTarget(input.Target, "workspace")
		// The backend counts a connector's name in bytes.
		if !ok || decodeStrict(raw, &payload) != nil || !ValidFoundryName(payload.Name) || len(strings.TrimSpace(payload.Name)) > maxFoundryNameChars ||
			!ValidSetDescription(payload.Description) {
			return nil, invalid("create_event_source takes target workspace:<workspace_id> and payload {name, description, text_field}: " +
				"a name of 1 to 120 characters, a description of at most 1000, and optionally the name of the field holding each piece's text")
		}
		// A name no piece could carry is refused here as the backend would; any other is locked
		// as a string on the new source, which has no field yet to conflict with it.
		if payload.TextField != nil {
			if err := events.ValidFieldName(*payload.TextField); err != nil {
				return nil, invalid("text_field: " + err.Error())
			}
		}
		req := &foundryv1.CreateConnectorRequest{WorkspaceId: workspaceID, Name: payload.Name, Config: &foundryv1.ConnectorConfig{
			Kind: &foundryv1.ConnectorConfig_Events{Events: &foundryv1.EventsConnectorConfig{Description: proto.String(payload.Description), TextField: payload.TextField}},
		}}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "CreateConnector"); err != nil {
			return nil, err
		}
		return CreateEventSourceOperation{payload: payload, req: req}, nil
	case "sync_connector":
		workspaceID, connectorID, ok := CutFoundryTarget(input.Target, "connector")
		if !ok || len(input.Payload) != 0 {
			return nil, invalid("sync_connector takes target connector:<workspace_id>/<connector_id> and an empty payload")
		}
		req := &foundryv1.SyncConnectorRequest{WorkspaceId: workspaceID, Id: connectorID}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "SyncConnector"); err != nil {
			return nil, err
		}
		return SyncConnectorOperation{req: req}, nil
	}
	workspaceID, ontologyID, ok := CutFoundryTarget(input.Target, "ontology")
	if !ok {
		return nil, invalid(input.Operation + " takes target ontology:<workspace_id>/<ontology_id>")
	}
	switch input.Operation {
	case "generate_ontology":
		var payload GenerateOntologyPayload
		malformed := invalid("generate_ontology takes payload {goal, connector_ids, document_set_ids, no_connectors}, a goal of at most 2000 characters and no connector_ids with no_connectors")
		if s.Events {
			malformed = invalid("generate_ontology takes payload {goal, connector_ids, document_set_ids, no_connectors, event_sources}, a goal of at most 2000 characters, " +
				"no connector_ids with no_connectors, and event_sources naming each API source once as {connector_id, mode}, mode structured or extraction")
		}
		if decodeStrict(raw, &payload) != nil || utf8.RuneCountInString(payload.Goal) > maxFoundryGoalChars ||
			!ValidIDs(payload.ConnectorIDs) || !ValidIDs(payload.DocumentSetIDs) || payload.NoConnectors && len(payload.ConnectorIDs) > 0 {
			return nil, malformed
		}
		req := &foundryv1.GenerateOntologyRequest{
			WorkspaceId: workspaceID, OntologyId: ontologyID, Goal: payload.Goal,
			ConnectorIds: payload.ConnectorIDs, DocumentSetIds: payload.DocumentSetIDs, NoConnectors: payload.NoConnectors,
		}
		if !s.Events && len(payload.EventSources) > 0 {
			return nil, status.Error(codes.FailedPrecondition, "API sources are not available in local mode")
		}
		for _, pick := range payload.EventSources {
			mode, known := eventSourceModes[pick.Mode]
			if !known || !ValidIDs([]string{pick.ConnectorID}) ||
				slices.ContainsFunc(req.EventSources, func(p *foundryv1.FoundryEventSourcePick) bool { return p.GetConnectorId() == pick.ConnectorID }) {
				return nil, malformed
			}
			req.EventSources = append(req.EventSources, &foundryv1.FoundryEventSourcePick{ConnectorId: pick.ConnectorID, Mode: mode})
		}
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "GenerateOntology"); err != nil {
			return nil, err
		}
		return GenerateOntologyOperation{payload: payload, req: req}, nil
	case "set_current_version":
		var payload SetCurrentVersionPayload
		if decodeStrict(raw, &payload) != nil || !ValidIDs([]string{payload.VersionID}, payload.ExpectedCurrentVersionID) {
			return nil, invalid("set_current_version takes payload {version_id, expected_current_version_id}")
		}
		req := &foundryv1.SetCurrentOntologyVersionRequest{
			WorkspaceId: workspaceID, OntologyId: ontologyID, VersionId: payload.VersionID, ExpectedCurrentVersionId: payload.ExpectedCurrentVersionID,
		}
		if err := s.AuthorizeFoundry(ctx, identity, req, writeGrant, "SetCurrentOntologyVersion"); err != nil {
			return nil, err
		}
		return SetCurrentVersionOperation{payload: payload, req: req}, nil
	case "create_load":
		var payload CreateLoadPayload
		if decodeStrict(raw, &payload) != nil || !ValidIDs([]string{payload.VersionID}) {
			return nil, invalid("create_load takes payload {version_id}")
		}
		req := &foundryv1.CreateFoundryLoadRequest{WorkspaceId: workspaceID, OntologyId: ontologyID, VersionId: payload.VersionID}
		if err := s.AuthorizeFoundry(ctx, identity, req, writeGrant, "CreateFoundryLoad"); err != nil {
			return nil, err
		}
		return CreateLoadOperation{payload: payload, req: req}, nil
	case "set_current_load":
		var payload SetCurrentLoadPayload
		if decodeStrict(raw, &payload) != nil || !ValidIDs([]string{payload.LoadID}) {
			return nil, invalid("set_current_load takes payload {load_id}")
		}
		req := &foundryv1.SetCurrentFoundryLoadRequest{WorkspaceId: workspaceID, OntologyId: ontologyID, Id: payload.LoadID}
		if err := s.AuthorizeFoundry(ctx, identity, req, writeGrant, "SetCurrentFoundryLoad"); err != nil {
			return nil, err
		}
		return SetCurrentLoadOperation{payload: payload, req: req}, nil
	case "revise_version":
		var payload ReviseVersionPayload
		if decodeStrict(raw, &payload) != nil || !ValidIDs([]string{payload.VersionID}) {
			return nil, invalid("revise_version takes payload {version_id, edits, dry_run}")
		}
		edits, err := OntologyEdits(payload.Edits)
		if err != nil {
			return nil, invalid(err.Error())
		}
		req := &foundryv1.ReviseOntologyVersionRequest{
			WorkspaceId: workspaceID, OntologyId: ontologyID, VersionId: payload.VersionID, Edits: edits, DryRun: payload.DryRun,
		}
		if err := s.AuthorizeFoundry(ctx, identity, req, writeGrant, "ReviseOntologyVersion"); err != nil {
			return nil, err
		}
		return ReviseVersionOperation{payload: payload, req: req}, nil
	case "upload_document":
		var payload UploadDocumentPayload
		err := decodeStrict(raw, &payload)
		mediaType, known := document.MediaTypeOf(payload.Name)
		if err != nil || !ValidIDs([]string{payload.SetID}) || !document.ValidName(payload.Name) || !known ||
			payload.SizeBytes <= 0 || payload.SizeBytes > s.Documents.MaxBytesFor(mediaType) || !document.ValidSHA256(payload.SHA256) {
			return nil, invalid(fmt.Sprintf("upload_document takes payload {set_id, name, size_bytes, sha256}: a name of 1 to 255 characters with no slash, "+
				"ending .md, .markdown, .mdx, .txt, .json, .csv, .tsv, .jsonl, .ndjson, .parquet or .pdf, the file's size in bytes, "+
				"at most %d KiB for Markdown, MDX and text, %d KiB for CSV, TSV, JSON, JSONL and Parquet, and %d KiB for PDF, "+
				"and its lowercase hex SHA-256", s.Documents.MaxBytes>>10, s.Documents.MaxTableBytes>>10, s.Documents.MaxPDFBytes>>10))
		}
		req := &foundryv1.CreateFoundryOntologyDocumentRequest{
			WorkspaceId: workspaceID, OntologyId: ontologyID, SetId: payload.SetID, Name: payload.Name, MediaType: string(mediaType),
			SizeBytes: payload.SizeBytes, Sha256: payload.SHA256,
		}
		if err := s.AuthorizeFoundry(ctx, identity, req, writeGrant, "CreateFoundryOntologyDocument"); err != nil {
			return nil, err
		}
		return UploadDocumentOperation{payload: payload, Req: req}, nil
	case "create_document_set":
		var payload CreateDocumentSetPayload
		if decodeStrict(raw, &payload) != nil || !ValidFoundryName(payload.Name) || !ValidSetDescription(payload.Description) {
			return nil, invalid("create_document_set takes payload {name, description}: a name of 1 to 120 characters and a description of at most 1000")
		}
		req := &foundryv1.CreateFoundryOntologyDocumentSetRequest{WorkspaceId: workspaceID, OntologyId: ontologyID, Name: payload.Name, Description: payload.Description}
		if err := s.AuthorizeFoundry(ctx, identity, req, writeGrant, "CreateFoundryOntologyDocumentSet"); err != nil {
			return nil, err
		}
		return CreateDocumentSetOperation{payload: payload, req: req}, nil
	case "update_document_set":
		var payload UpdateDocumentSetPayload
		if decodeStrict(raw, &payload) != nil || !ValidIDs([]string{payload.SetID}) || (payload.Name == nil && payload.Description == nil) ||
			(payload.Name != nil && !ValidFoundryName(*payload.Name)) || (payload.Description != nil && !ValidSetDescription(*payload.Description)) {
			return nil, invalid("update_document_set takes payload {set_id, name, description}: a name of 1 to 120 characters, a description of at most 1000, or both")
		}
		req := &foundryv1.UpdateFoundryOntologyDocumentSetRequest{
			WorkspaceId: workspaceID, OntologyId: ontologyID, SetId: payload.SetID, Name: payload.Name, Description: payload.Description,
		}
		if err := s.AuthorizeFoundry(ctx, identity, req, writeGrant, "UpdateFoundryOntologyDocumentSet"); err != nil {
			return nil, err
		}
		return UpdateDocumentSetOperation{payload: payload, req: req}, nil
	case "move_document":
		var payload MoveDocumentPayload
		if decodeStrict(raw, &payload) != nil || !ValidIDs([]string{payload.DocumentID, payload.SetID}) {
			return nil, invalid("move_document takes payload {document_id, set_id}")
		}
		req := &foundryv1.MoveFoundryOntologyDocumentRequest{WorkspaceId: workspaceID, OntologyId: ontologyID, Id: payload.DocumentID, SetId: payload.SetID}
		if err := s.AuthorizeFoundry(ctx, identity, req, writeGrant, "MoveFoundryOntologyDocument"); err != nil {
			return nil, err
		}
		return MoveDocumentOperation{payload: payload, req: req, workspaceID: workspaceID}, nil
	case "settle_review":
		var payload SettleReviewPayload
		if decodeStrict(raw, &payload) != nil || payload.ReviewID == "" || len(payload.ReviewID) > maxFoundryReviewIDBytes ||
			strings.ContainsFunc(payload.ReviewID, unicode.IsControl) {
			return nil, invalid("settle_review takes payload {review_id, decision}")
		}
		choice, err := ReviewChoice(payload.Decision)
		if err != nil {
			return nil, invalid(err.Error())
		}
		req := &foundryv1.SettleFoundryReviewRequest{WorkspaceId: workspaceID, OntologyId: ontologyID, ReviewId: payload.ReviewID, Decision: choice}
		// The backend records who decided, so only a person settles a review.
		if err := s.AuthorizeFoundry(ctx, identity, req, foundryUserOnly, "SettleFoundryReview"); err != nil {
			return nil, err
		}
		return SettleReviewOperation{payload: payload, Req: req}, nil
	}
	return nil, invalid("unsupported Foundry operation")
}

// OntologyEdits checks a revision's edits as the backend does: 1 to 100, each a put or a
// remove of a distinct item id of at most 256 bytes, their TOML at most 1 MiB in all.
func OntologyEdits(edits []OntologyEditPayload) ([]*foundryv1.OntologyEdit, error) {
	if len(edits) == 0 || len(edits) > maxFoundryEdits {
		return nil, errors.New("revise_version takes 1 to 100 edits")
	}
	out := make([]*foundryv1.OntologyEdit, 0, len(edits))
	seen := map[string]bool{}
	tomlBytes := 0
	for _, edit := range edits {
		var item string
		switch {
		case edit.Put != nil && edit.Remove == nil:
			item, tomlBytes = edit.Put.ItemID, tomlBytes+len(edit.Put.TOML)
			out = append(out, &foundryv1.OntologyEdit{Edit: &foundryv1.OntologyEdit_Put{Put: &foundryv1.OntologyPut{ItemId: edit.Put.ItemID, Toml: edit.Put.TOML}}})
		case edit.Remove != nil && edit.Put == nil:
			item = edit.Remove.ItemID
			out = append(out, &foundryv1.OntologyEdit{Edit: &foundryv1.OntologyEdit_Remove{Remove: &foundryv1.OntologyRemove{ItemId: item}}})
		default:
			return nil, errors.New("each edit is {put: {item_id, toml}} or {remove: {item_id}}")
		}
		if strings.TrimSpace(item) == "" || len(item) > maxFoundryEditItemBytes || seen[item] {
			return nil, errors.New("edits need distinct item ids of at most 256 bytes")
		}
		seen[item] = true
	}
	if tomlBytes > maxFoundryEditTOMLBytes {
		return nil, errors.New("the edits' TOML is past 1 MiB")
	}
	return out, nil
}

// ReviewChoice is the one decision a review takes: resolve to an entity or to none, or set
// or remove the property.
func ReviewChoice(decision ReviewDecisionPayload) (*foundryv1.FoundryReviewChoice, error) {
	wrong := errors.New("decision is {resolve: {entity_key}}, {resolve: {not_in_data: true}} or {prop: {text|integer|number|boolean|null: value}}")
	resolve, prop := decision.Resolve, decision.Prop
	switch {
	case resolve != nil && prop == nil:
		switch {
		case resolve.EntityKey != "" && !resolve.NotInData:
			return &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Resolve{
				Resolve: &foundryv1.FoundryResolveChoice{Target: &foundryv1.FoundryResolveChoice_EntityKey{EntityKey: resolve.EntityKey}}}}, nil
		case resolve.EntityKey == "" && resolve.NotInData:
			return &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Resolve{
				Resolve: &foundryv1.FoundryResolveChoice{Target: &foundryv1.FoundryResolveChoice_NotInData{NotInData: true}}}}, nil
		}
	case prop != nil && resolve == nil:
		var value *foundryv1.FoundryPropValue
		set := 0
		if prop.Null {
			set, value = set+1, &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Null{}}
		}
		if prop.Text != nil {
			set, value = set+1, &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Text{Text: *prop.Text}}
		}
		if prop.Integer != nil {
			n, err := strconv.ParseInt(*prop.Integer, 10, 64)
			if err != nil {
				return nil, errors.New("an integer is a string of digits within int64")
			}
			set, value = set+1, &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Integer{Integer: n}}
		}
		if prop.Number != nil {
			set, value = set+1, &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Number{Number: *prop.Number}}
		}
		if prop.Boolean != nil {
			set, value = set+1, &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Boolean{Boolean: *prop.Boolean}}
		}
		if set == 1 {
			return &foundryv1.FoundryReviewChoice{Choice: &foundryv1.FoundryReviewChoice_Prop{Prop: value}}, nil
		}
	}
	return nil, wrong
}

// ValidFoundryName is a name of 1 to 120 characters once trimmed, as the backend takes an
// ontology's, a document set's, a database's and a project's.
func ValidFoundryName(name string) bool {
	trimmed := strings.TrimSpace(name)
	return trimmed != "" && utf8.RuneCountInString(trimmed) <= maxFoundryNameChars
}

func ValidSetDescription(description string) bool {
	return utf8.RuneCountInString(strings.TrimSpace(description)) <= maxFoundrySetDescription
}

// CutFoundryTarget parses <kind>:<workspace_id>/<id>.
func CutFoundryTarget(target, kind string) (string, string, bool) {
	rest, ok := strings.CutPrefix(target, kind+":")
	if !ok {
		return "", "", false
	}
	workspaceID, id, ok := strings.Cut(rest, "/")
	return workspaceID, id, ok && ValidIDs([]string{workspaceID, id})
}

// TargetKind is the audit resource kind of a target, or empty for one that doesn't parse.
func TargetKind(target string) string {
	kind, _, ok := strings.Cut(target, ":")
	if !ok || (kind != "tenant" && kind != "workspace" && kind != "connector" && kind != "ontology") {
		return ""
	}
	return kind
}

type CreateOntologyPayload struct {
	Name string `json:"name"`
	// Database is the new database to make it on, for target Workspace:<id>; nil on a tenant.
	Database *NewDatabasePayload `json:"database,omitempty"`
}

// NewDatabasePayload is a database on the shared cluster in an existing project (ProjectID) or
// in one made of ProjectName: exactly one of the two.
type NewDatabasePayload struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	ProjectID   string `json:"project_id,omitempty"`
	ProjectName string `json:"project_name,omitempty"`
}

type CreateOntologyOperation struct {
	payload CreateOntologyPayload
	req     *foundryv1.CreateFoundryOntologyRequest
}

func (o CreateOntologyOperation) CanonicalPayload() []byte { return mustCanonical(o.payload) }
func (o CreateOntologyOperation) Execute(ctx context.Context, s *Service, identity *Identity, confirmationID string) (any, error) {
	o.req.ClientToken = confirmationID
	response, err := s.Actions.CreateFoundryOntology(ctx, o.req)
	if err != nil {
		return nil, err
	}
	return s.FoundryOntology(identity, o.req.GetWorkspaceId(), response.GetOntology()), nil
}

// NewDatabaseOntologyOperation makes the project when it is new, the database on the shared
// cluster, then the ontology on it, as the website's wizard does. It mirrors the website's
// calls: no client token for the project or database (their RPCs take none), the confirmation
// as the ontology's. CreateTenant returns the database's first key once; it is dropped, as the
// website drops it, and the user makes keys in the Helix dashboard.
type NewDatabaseOntologyOperation struct {
	workspaceID string
	payload     CreateOntologyPayload
}

func (o NewDatabaseOntologyOperation) CanonicalPayload() []byte { return mustCanonical(o.payload) }

// Execute names what it made in the error of any later step, so the agent goes on from it
// (database.project_id, then target tenant:<id>) rather than making another. A step that fails
// for a reason it states (invalid, not found, taken, refused, over a quota, aborted) made
// nothing. Any other failure may have: one without the backend's answer, and Internal, which
// the backend also returns after its transaction committed. CreateProject and CreateTenant take
// no client token, and the ontology's is the confirmation, spent once; so for those the error
// says to look for what may have been made before trying again.
func (o NewDatabaseOntologyOperation) Execute(ctx context.Context, s *Service, identity *Identity, confirmationID string) (any, error) {
	database := o.payload.Database
	made := func(err error, what, next string) error {
		reason := string(stableErrorCode(err))
		var detailed detailedToolError
		if errors.As(FoundryError(err), &detailed) {
			reason = detailed.message
		}
		return detailedToolError{code: stableErrorCode(err), message: what + ": " + reason + "; " + next}
	}
	unsure := func(err error) bool {
		switch status.Code(err) {
		case codes.InvalidArgument, codes.NotFound, codes.AlreadyExists, codes.PermissionDenied, codes.FailedPrecondition,
			codes.ResourceExhausted, codes.Aborted, codes.OutOfRange, codes.Unimplemented, codes.Unauthenticated:
			return false
		}
		return true
	}
	projectID, newProject := database.ProjectID, ""
	if projectID == "" {
		// The project's slug from its name, as the website makes it; the backend suffixes one
		// the Workspace has.
		name := strings.TrimSpace(database.ProjectName)
		plain := strings.Map(func(r rune) rune {
			if unicode.Is(unicode.Mn, r) {
				return -1
			}
			return r
		}, norm.NFKD.String(name))
		slug := strings.Trim(foundrySlugSeparators.ReplaceAllString(strings.ToLower(plain), "-"), "-")
		if slug = strings.TrimRight(slug[:min(len(slug), maxFoundrySlugChars)], "-"); slug == "" {
			slug = "project"
		}
		project, err := s.Directory.CreateProject(ctx, o.workspaceID, slug, name)
		if err != nil && unsure(err) {
			return nil, detailedToolError{code: stableErrorCode(err), message: "Foundry may have made the project: look for it with " + toolListProjects +
				" before trying again, and if it is there prepare create_ontology with its id as database.project_id rather than making another project"}
		}
		if err != nil {
			return nil, err
		}
		projectID = project
		newProject = fmt.Sprintf("Foundry made project %s", projectID)
	}
	create := func(slug string) (string, error) {
		return s.Directory.CreateDatabase(ctx, projectID, strings.TrimSpace(database.Name), slug)
	}
	tenant, err := create(database.Slug)
	if status.Code(err) == codes.AlreadyExists {
		// A slug is unique across the shared cluster, which every Workspace shares.
		suffix := make([]byte, 3)
		_, _ = rand.Read(suffix) // crypto/rand never fails
		tenant, err = create(strings.TrimRight(database.Slug[:min(len(database.Slug), maxFoundrySlugStem)], "-") + "-" + hex.EncodeToString(suffix))
	}
	if err != nil {
		for _, detail := range status.Convert(err).Details() {
			if info, ok := detail.(*errdetails.ErrorInfo); ok && info.GetReason() == "NO_WORKSPACE_SUBSCRIPTION" {
				message := FoundryNoHelixSubscription(info.GetMetadata()["state"])
				if newProject != "" {
					message += "; " + newProject + ", for database.project_id rather than making another"
				}
				return nil, detailedToolError{code: errCodeFailedPrecondition, message: message}
			}
		}
		if unsure(err) {
			what, otherwise := "Foundry may have made the database", ""
			if newProject != "" {
				what, otherwise = newProject+" and may have made the database", ", or else with database.project_id "+projectID+","
			}
			return nil, detailedToolError{code: stableErrorCode(err), message: what + ": look for it with " + toolListDatabases + " in project " + projectID +
				" before trying again, and if it is there prepare create_ontology with target tenant:<its id> and payload {name}" + otherwise +
				" rather than making another"}
		}
		if newProject != "" {
			return nil, made(err, newProject+" but not the database",
				"prepare create_ontology again with database.project_id "+projectID+" rather than making another project")
		}
		return nil, err
	}
	tenantID := tenant
	ontology, err := s.Actions.CreateFoundryOntology(ctx, &foundryv1.CreateFoundryOntologyRequest{
		WorkspaceId: o.workspaceID, TenantId: tenantID, Name: o.payload.Name, ClientToken: confirmationID,
	})
	if err != nil {
		what, next := fmt.Sprintf("Foundry made database tenant:%s in project %s", tenantID, projectID),
			"prepare create_ontology again with target tenant:"+tenantID+" and payload {name} rather than making another database"
		if unsure(err) {
			return nil, detailedToolError{code: stableErrorCode(err), message: what + " and may have made the ontology: look for one on database tenant:" + tenantID +
				" with " + toolFoundryListOntologies + " before trying again, and if there is none " + next}
		}
		return nil, made(err, what+" but not the ontology", next)
	}
	return s.FoundryOntology(identity, o.workspaceID, ontology.GetOntology()), nil
}

type SyncConnectorOperation struct {
	req *foundryv1.SyncConnectorRequest
}

func (o SyncConnectorOperation) CanonicalPayload() []byte { return []byte("{}") }
func (o SyncConnectorOperation) Execute(ctx context.Context, s *Service, _ *Identity, _ string) (any, error) {
	response, err := s.Actions.SyncConnector(ctx, o.req)
	if err != nil {
		return nil, err
	}
	return FoundrySync(response.GetSync()), nil
}

type GenerateOntologyPayload struct {
	Goal           string                   `json:"goal,omitempty"`
	ConnectorIDs   []string                 `json:"connector_ids,omitempty"`
	DocumentSetIDs []string                 `json:"document_set_ids,omitempty"`
	NoConnectors   bool                     `json:"no_connectors,omitempty"`
	EventSources   []EventSourcePickPayload `json:"event_sources,omitempty"`
}

// EventSourcePickPayload is an API source a generation drafts from; mode is a key of
// eventSourceModes.
type EventSourcePickPayload struct {
	ConnectorID string `json:"connector_id"`
	Mode        string `json:"mode"`
}

type GenerateOntologyOperation struct {
	payload GenerateOntologyPayload
	req     *foundryv1.GenerateOntologyRequest
}

func (o GenerateOntologyOperation) CanonicalPayload() []byte { return mustCanonical(o.payload) }

// Execute names the event sources it drafts from by its own picks, in their modes: the backend
// takes every pick or refuses the generation, connector_ids names none, and a source read by
// extraction is its set, which the generation names by id alone.
func (o GenerateOntologyOperation) Execute(ctx context.Context, s *Service, _ *Identity, _ string) (any, error) {
	response, err := s.Actions.GenerateOntology(ctx, o.req)
	if err != nil {
		return nil, err
	}
	out := s.FoundryGeneration(o.req.GetWorkspaceId(), o.req.GetOntologyId(), response.GetGeneration())
	for _, pick := range o.payload.EventSources {
		out.EventSources = append(out.EventSources, FoundryEventSourceReadOutput{EventSourceID: pick.ConnectorID, Mode: pick.Mode})
	}
	return out, nil
}

type SetCurrentVersionPayload struct {
	VersionID                string `json:"version_id"`
	ExpectedCurrentVersionID string `json:"expected_current_version_id"`
}

type SetCurrentVersionOperation struct {
	payload SetCurrentVersionPayload
	req     *foundryv1.SetCurrentOntologyVersionRequest
}

func (o SetCurrentVersionOperation) CanonicalPayload() []byte { return mustCanonical(o.payload) }
func (o SetCurrentVersionOperation) Execute(ctx context.Context, s *Service, _ *Identity, _ string) (any, error) {
	response, err := s.Actions.SetCurrentOntologyVersion(ctx, o.req)
	if err != nil {
		return nil, err
	}
	return s.FoundryVersion(o.req.GetWorkspaceId(), o.req.GetOntologyId(), response.GetVersion(), false), nil
}

type CreateLoadPayload struct {
	VersionID string `json:"version_id"`
}

type CreateLoadOperation struct {
	payload CreateLoadPayload
	req     *foundryv1.CreateFoundryLoadRequest
}

func (o CreateLoadOperation) CanonicalPayload() []byte { return mustCanonical(o.payload) }
func (o CreateLoadOperation) Execute(ctx context.Context, s *Service, _ *Identity, confirmationID string) (any, error) {
	o.req.ClientToken = confirmationID
	response, err := s.Actions.CreateFoundryLoad(ctx, o.req)
	if err != nil {
		return nil, err
	}
	return s.FoundryLoad(o.req.GetWorkspaceId(), o.req.GetOntologyId(), response.GetLoad()), nil
}

type SetCurrentLoadPayload struct {
	LoadID string `json:"load_id"`
}

type SetCurrentLoadOperation struct {
	payload SetCurrentLoadPayload
	req     *foundryv1.SetCurrentFoundryLoadRequest
}

func (o SetCurrentLoadOperation) CanonicalPayload() []byte { return mustCanonical(o.payload) }
func (o SetCurrentLoadOperation) Execute(ctx context.Context, s *Service, _ *Identity, _ string) (any, error) {
	response, err := s.Actions.SetCurrentFoundryLoad(ctx, o.req)
	if err != nil {
		return nil, err
	}
	return s.FoundryLoad(o.req.GetWorkspaceId(), o.req.GetOntologyId(), response.GetLoad()), nil
}

type OntologyEditPayload struct {
	Put    *OntologyPutPayload    `json:"put,omitempty"`
	Remove *OntologyRemovePayload `json:"remove,omitempty"`
}

type OntologyPutPayload struct {
	ItemID string `json:"item_id"`
	TOML   string `json:"toml"`
}

type OntologyRemovePayload struct {
	ItemID string `json:"item_id"`
}

type ReviseVersionPayload struct {
	VersionID string                `json:"version_id"`
	Edits     []OntologyEditPayload `json:"edits"`
	DryRun    bool                  `json:"dry_run,omitempty"`
}

type ReviseVersionOperation struct {
	payload ReviseVersionPayload
	req     *foundryv1.ReviseOntologyVersionRequest
}

func (o ReviseVersionOperation) CanonicalPayload() []byte { return mustCanonical(o.payload) }
func (o ReviseVersionOperation) Execute(ctx context.Context, s *Service, _ *Identity, _ string) (any, error) {
	response, err := s.Actions.ReviseOntologyVersion(ctx, o.req)
	if err != nil {
		return nil, err
	}
	return s.FoundryGeneration(o.req.GetWorkspaceId(), o.req.GetOntologyId(), response.GetGeneration()), nil
}

type UploadDocumentPayload struct {
	SetID     string `json:"set_id"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
}

type UploadDocumentOperation struct {
	payload UploadDocumentPayload
	Req     *foundryv1.CreateFoundryOntologyDocumentRequest
}

type FoundryUploadOutput struct {
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Headers map[string]string `json:"headers"`
	// ExpiresAt is when the URL stops taking the bytes; another upload_document gives a new one.
	ExpiresAt string `json:"expires_at"`
}

type UploadDocumentResult struct {
	Document FoundryDocumentOutput `json:"document"`
	// Upload is where to PUT the file's bytes; unset when the Workspace already holds them.
	Upload *FoundryUploadOutput `json:"upload,omitempty"`
	// Routed says an extraction route of the version that reads the document names its set; a
	// document no route names is stored and read by no model.
	Routed bool   `json:"routed"`
	Next   string `json:"next"`
}

func (o UploadDocumentOperation) CanonicalPayload() []byte { return mustCanonical(o.payload) }

// Execute creates the document with the consumed confirmation as its client token, so a
// retried upload finds the same document and a fresh URL.
func (o UploadDocumentOperation) Execute(ctx context.Context, s *Service, _ *Identity, confirmationID string) (any, error) {
	o.Req.ClientToken = confirmationID
	response, err := s.Actions.CreateFoundryOntologyDocument(ctx, o.Req)
	if err != nil {
		return nil, err
	}
	out := UploadDocumentResult{Document: FoundryDocument(response.GetDocument()), Routed: response.GetRouted(),
		Next: "The workspace already holds these bytes, as this document (its set_id says where); nothing is left to upload."}
	if upload := response.GetUpload(); upload != nil {
		out.Upload = &FoundryUploadOutput{
			URL: upload.GetUrl(), Method: upload.GetMethod(), Headers: upload.GetHeaders(), ExpiresAt: protoTimestamp(upload.GetExpiresAt()),
		}
		out.Next = "Send the file's exact bytes to upload.url with upload.method and every upload.headers entry before upload.expires_at " +
			"(curl -X PUT --data-binary @<file> -H '<name>: <value>' ... '<url>'), then call " + toolFoundryCompleteUpload + " with the document's id."
	}
	return out, nil
}

type CreateDocumentSetPayload struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type CreateDocumentSetOperation struct {
	payload CreateDocumentSetPayload
	req     *foundryv1.CreateFoundryOntologyDocumentSetRequest
}

func (o CreateDocumentSetOperation) CanonicalPayload() []byte { return mustCanonical(o.payload) }
func (o CreateDocumentSetOperation) Execute(ctx context.Context, s *Service, _ *Identity, _ string) (any, error) {
	response, err := s.Actions.CreateFoundryOntologyDocumentSet(ctx, o.req)
	if err != nil {
		return nil, err
	}
	return FoundryDocumentSet(response.GetSet()), nil
}

// UpdateDocumentSetPayload's fields are pointers: an absent field is left as it is, and an empty
// description clears it.
type UpdateDocumentSetPayload struct {
	SetID       string  `json:"set_id"`
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
}

type UpdateDocumentSetOperation struct {
	payload UpdateDocumentSetPayload
	req     *foundryv1.UpdateFoundryOntologyDocumentSetRequest
}

func (o UpdateDocumentSetOperation) CanonicalPayload() []byte { return mustCanonical(o.payload) }
func (o UpdateDocumentSetOperation) Execute(ctx context.Context, s *Service, _ *Identity, _ string) (any, error) {
	response, err := s.Actions.UpdateFoundryOntologyDocumentSet(ctx, o.req)
	if err != nil {
		return nil, err
	}
	return FoundryDocumentSet(response.GetSet()), nil
}

type MoveDocumentPayload struct {
	DocumentID string `json:"document_id"`
	SetID      string `json:"set_id"`
}

type MoveDocumentOperation struct {
	payload MoveDocumentPayload
	req     *foundryv1.MoveFoundryOntologyDocumentRequest
	// workspaceID scopes the move to the ontologies the caller may write (Identity.WithScope).
	workspaceID string
}

func (o MoveDocumentOperation) CanonicalPayload() []byte { return mustCanonical(o.payload) }
func (o MoveDocumentOperation) Execute(ctx context.Context, s *Service, identity *Identity, _ string) (any, error) {
	response, err := s.Actions.MoveFoundryOntologyDocument(identity.WithScope(ctx, o.workspaceID, true), o.req)
	if err != nil {
		return nil, err
	}
	return FoundryDocumentReadsOutput{Document: FoundryDocument(response.GetDocument()), Reads: FoundryDocumentReads(response.GetReads())}, nil
}

type ReviewResolvePayload struct {
	EntityKey string `json:"entity_key,omitempty"`
	NotInData bool   `json:"not_in_data,omitempty"`
}

// PropValuePayload holds one value; integer is a string so every digit of an int64 is kept.
type PropValuePayload struct {
	Null    bool     `json:"null,omitempty"`
	Text    *string  `json:"text,omitempty"`
	Integer *string  `json:"integer,omitempty"`
	Number  *float64 `json:"number,omitempty"`
	Boolean *bool    `json:"boolean,omitempty"`
}

type ReviewDecisionPayload struct {
	Resolve *ReviewResolvePayload `json:"resolve,omitempty"`
	Prop    *PropValuePayload     `json:"prop,omitempty"`
}

type SettleReviewPayload struct {
	ReviewID string                `json:"review_id"`
	Decision ReviewDecisionPayload `json:"decision"`
}

type SettleReviewOperation struct {
	payload SettleReviewPayload
	Req     *foundryv1.SettleFoundryReviewRequest
}

func (o SettleReviewOperation) CanonicalPayload() []byte { return mustCanonical(o.payload) }
func (o SettleReviewOperation) Execute(ctx context.Context, s *Service, _ *Identity, _ string) (any, error) {
	response, err := s.Actions.SettleFoundryReview(ctx, o.Req)
	if err != nil {
		return nil, err
	}
	return FoundryReviewDecision(response.GetDecision()), nil
}
