package api

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"time"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/document"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

// maxFoundryDocumentIDs bounds the ids ListFoundryDocuments reads in one
// call.
const maxFoundryDocumentIDs = 100

// foundryDocumentStore is where documents' bytes live (document.Store).
type foundryDocumentStore interface {
	PresignPut(ctx context.Context, key string, d document.Declared, ttl time.Duration) (document.Upload, error)
	Verify(ctx context.Context, key string, d document.Declared) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
	DeletePrefix(ctx context.Context, prefix string) error
}

// CreateFoundryDocument writes a document of the workspace waiting for its
// bytes, and presigns their PUT.
//  1. Check the name, the type (which must agree with the extension and be
//     one of foundry.documents.media_types), the size (at most
//     foundry.documents.max_bytes), the checksum and that a set is named.
//  2. The same client token, or the same bytes, return the workspace's
//     document already there (existing), in the set it is in: with a fresh
//     upload while it waits for its bytes, without one once they are in.
//     The website names that set.
//  3. Otherwise write the row into the set, which must be a live set of the
//     workspace, unless the workspace is at its document cap, which counts
//     uploaded documents and uploads still within upload_ttl. An upload
//     asked for again after its URL expired is checked against the cap
//     again. Both lookups and the cap run under the workspace's lock.
//  4. Presign a PUT of exactly the declared bytes.
//
// On the ontology-scoped alias (ontology_id set, a live ontology of the
// workspace) the response also says, as it did before documents were the
// workspace's, whether that ontology reads the set and routes it.
func (s *Service) CreateFoundryDocument(ctx context.Context, req *foundryv1.CreateFoundryDocumentRequest) (*foundryv1.CreateFoundryDocumentResponse, error) {
	out, err := s.createFoundryDocument(ctx, "", req)
	if err != nil {
		return nil, err
	}
	return &foundryv1.CreateFoundryDocumentResponse{Document: out.GetDocument(), Upload: out.GetUpload(), Existing: out.GetExisting()}, nil
}

// createFoundryDocument is CreateFoundryDocument, and with ontologyID its
// ontology-scoped alias, whose answer also says whether the ontology routes
// the document (routed).
func (s *Service) createFoundryDocument(ctx context.Context, ontologyID string, req *foundryv1.CreateFoundryDocumentRequest) (*foundryv1.CreateFoundryOntologyDocumentResponse, error) {
	cfg := s.deps.Cfg.Foundry.Documents
	mediaType := foundrymodel.FoundryMediaType(req.GetMediaType())
	switch extType, ok := document.MediaTypeOf(req.GetName()); {
	case req.GetWorkspaceId() == "":
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	case !document.ValidName(req.GetName()):
		return nil, status.Error(codes.InvalidArgument, "name must be 1 to 255 characters with no slash, backslash or control character")
	case !ok || !slices.Contains(cfg.MediaTypes, mediaType):
		return nil, status.Errorf(codes.InvalidArgument, "Foundry takes Markdown (.md, .markdown), MDX (.mdx), plain text (.txt), JSON (.json), CSV (.csv), "+
			"TSV (.tsv), JSONL (.jsonl, .ndjson), Parquet (.parquet) and PDF (.pdf) documents; %q is none of them", req.GetName())
	case extType != mediaType:
		return nil, status.Errorf(codes.InvalidArgument, "media_type %q does not match %q, which is %s", mediaType, req.GetName(), extType)
	case req.GetSizeBytes() <= 0 || req.GetSizeBytes() > cfg.MaxBytesFor(mediaType):
		return nil, status.Errorf(codes.InvalidArgument, "a %s document holds 1 byte to %d KiB; this one is %d bytes",
			mediaType.Format(), cfg.MaxBytesFor(mediaType)>>10, req.GetSizeBytes())
	case !document.ValidSHA256(req.GetSha256()):
		return nil, status.Error(codes.InvalidArgument, "sha256 must be the lowercase hex SHA-256 of the bytes")
	case req.GetClientToken() == "" || len(req.GetClientToken()) > maxClientTokenLength:
		return nil, status.Errorf(codes.InvalidArgument, "client_token is required, at most %d characters", maxClientTokenLength)
	case req.GetSetId() == "":
		return nil, status.Error(codes.InvalidArgument, "set_id is required: the document set to upload into")
	}
	ws := req.GetWorkspaceId()
	var alias *foundrymodel.FoundryOntology
	if ontologyID != "" {
		o, err := s.liveFoundryOntology(ctx, ws, ontologyID)
		if err != nil {
			return nil, err
		}
		alias = o
	}
	if s.deps.Documents == nil {
		return nil, status.Error(codes.Unavailable, "documents are not available here")
	}
	docID := id.New()
	doc := &foundrymodel.FoundryDocument{
		ID: docID, WorkspaceID: ws, SetID: req.GetSetId(), Name: req.GetName(), MediaType: mediaType,
		SizeBytes: req.GetSizeBytes(), SHA256: req.GetSha256(), ClientToken: new(req.GetClientToken()), S3Key: document.Key(ws, docID),
	}
	if userID := actor(ctx).UserID; userID != "" {
		doc.CreatedByUserID = &userID
	}
	limit := foundrymodel.FoundryDocumentCap{Max: cfg.MaxPerWorkspace, PendingSince: time.Now().Add(-cfg.UploadTTL)}
	stored, existing, err := s.deps.DB.CreateFoundryDocument(ctx, doc, limit)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "document set not found")
	case errors.Is(err, foundrystore.ErrConnectorOwned):
		return nil, s.connectorOwned(ctx, ws, req.GetSetId(), "set")
	case errors.Is(err, foundrystore.ErrLimitExceeded):
		return nil, status.Errorf(codes.FailedPrecondition, "a workspace holds up to %d documents; delete one first", cfg.MaxPerWorkspace)
	case errors.Is(err, foundrystore.ErrConflict):
		return nil, status.Error(codes.InvalidArgument, "client_token was already used; use a new one")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "create document: %v", err)
	}
	if stored.ClientToken != nil && *stored.ClientToken == req.GetClientToken() && stored.SHA256 != req.GetSha256() {
		return nil, status.Error(codes.InvalidArgument, "client_token was already used for other bytes")
	}
	resp := &foundryv1.CreateFoundryOntologyDocumentResponse{Document: foundryDocumentProto(stored, nil), Existing: existing}
	if alias != nil {
		reader, err := s.foundryReaderOf(ctx, alias)
		if err != nil {
			return nil, err
		}
		resp.Routed = slices.Contains(reader.scope, stored.SetID) && reader.routes.Routes(stored.SetID)
	}
	if stored.Status != foundrymodel.FoundryDocumentPendingUpload {
		return resp, nil
	}
	// The document found may be another upload's; its own declaration is what S3 checks.
	upload, err := s.deps.Documents.PresignPut(ctx, stored.S3Key, document.Declared{
		Size: stored.SizeBytes, MediaType: stored.MediaType, SHA256: stored.SHA256,
	}, cfg.UploadTTL)
	if err != nil {
		log.From(ctx).Warn("presign document upload failed", "document", stored.ID, "err", err)
		return nil, status.Error(codes.Unavailable, "the upload could not be prepared; try again")
	}
	resp.Upload = &foundryv1.FoundryDocumentUpload{
		Url: upload.URL, Method: upload.Method, Headers: upload.Headers, ExpiresAt: timestamppb.New(upload.ExpiresAt),
	}
	return resp, nil
}

// CompleteFoundryDocumentUpload checks the bytes the browser PUT and marks the
// document uploaded. In the same transaction, every ontology whose live or
// current load reads its set reads it: an extract is queued into the
// current load within the month's budget or, while a job is live, owed by
// that job's end. An ontology whose database the caller may not write
// (write_scope) is skipped. reads says what happened for each ontology.
// Completing twice is no error, and reads nothing again. A local install
// with no AI provider, which could read no document, refuses it
// (FOUNDRY_AI_NOT_CONFIGURED).
func (s *Service) CompleteFoundryDocumentUpload(ctx context.Context, req *foundryv1.CompleteFoundryDocumentUploadRequest) (*foundryv1.FoundryDocumentReadsResponse, error) {
	return s.completeFoundryDocumentUpload(ctx, "", req)
}

func (s *Service) completeFoundryDocumentUpload(ctx context.Context, ontologyID string, req *foundryv1.CompleteFoundryDocumentUploadRequest) (*foundryv1.FoundryDocumentReadsResponse, error) {
	if _, err := s.foundryDocumentsWorkspace(ctx, req.GetWorkspaceId(), ontologyID); err != nil {
		return nil, err
	}
	doc, err := s.foundryDocument(ctx, req.GetWorkspaceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	switch doc.Status {
	case foundrymodel.FoundryDocumentUploaded:
		return &foundryv1.FoundryDocumentReadsResponse{Document: foundryDocumentProto(doc, nil)}, nil
	case foundrymodel.FoundryDocumentDeleted:
		return nil, status.Error(codes.FailedPrecondition, "the document is deleted")
	}
	if s.deps.Documents == nil {
		return nil, status.Error(codes.Unavailable, "documents are not available here")
	}
	if err := s.requireAI(ctx, doc.WorkspaceID); err != nil {
		return nil, err
	}
	err = s.deps.Documents.Verify(ctx, doc.S3Key, document.Declared{Size: doc.SizeBytes, MediaType: doc.MediaType, SHA256: doc.SHA256})
	switch {
	case errors.Is(err, document.ErrMissing):
		return nil, status.Error(codes.FailedPrecondition, "upload the document's bytes to its URL first")
	case errors.Is(err, document.ErrMismatch):
		return nil, status.Errorf(codes.FailedPrecondition, "the uploaded bytes are not the declared ones (%v); upload them again", err)
	case err != nil:
		log.From(ctx).Warn("verify document upload failed", "document", doc.ID, "err", err)
		return nil, status.Error(codes.Unavailable, "the upload could not be checked; try again")
	}
	completed, reads, err := s.deps.DB.CompleteFoundryDocumentUpload(ctx, doc.WorkspaceID, doc.ID, s.foundryFanOut(ctx, doc.WorkspaceID))
	if errors.Is(err, foundrystore.ErrNotFound) {
		// Completed or deleted concurrently; report where it is now.
		completed, err = s.deps.DB.GetFoundryDocument(ctx, doc.WorkspaceID, doc.ID)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "complete document upload: %v", err)
	}
	return &foundryv1.FoundryDocumentReadsResponse{Document: foundryDocumentProto(completed, nil), Reads: foundryReadsProto(reads)}, nil
}

// MoveFoundryDocument puts a document that is not deleted in another live set
// of the workspace. A load keeps what it read, with the set it read it in:
// the move applies to it from the next load. An uploaded document is then
// read as an upload into the set is (CompleteFoundryDocumentUpload), and
// reads says so per ontology. Moving it to the set it is in is no error. A
// connector's document never moves, and nothing moves into a connector's
// set (connectorOwned).
func (s *Service) MoveFoundryDocument(ctx context.Context, req *foundryv1.MoveFoundryDocumentRequest) (*foundryv1.FoundryDocumentReadsResponse, error) {
	return s.moveFoundryDocument(ctx, "", req)
}

func (s *Service) moveFoundryDocument(ctx context.Context, ontologyID string, req *foundryv1.MoveFoundryDocumentRequest) (*foundryv1.FoundryDocumentReadsResponse, error) {
	if _, err := s.foundryDocumentsWorkspace(ctx, req.GetWorkspaceId(), ontologyID); err != nil {
		return nil, err
	}
	doc, err := s.foundryDocument(ctx, req.GetWorkspaceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	if doc.Status == foundrymodel.FoundryDocumentDeleted {
		return nil, status.Error(codes.FailedPrecondition, "the document is deleted")
	}
	if req.GetSetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "set_id is required: the document set to move into")
	}
	moved, reads, err := s.deps.DB.MoveFoundryDocument(ctx, doc.WorkspaceID, doc.ID, req.GetSetId(), s.foundryFanOut(ctx, doc.WorkspaceID))
	switch {
	case errors.Is(err, foundrystore.ErrConnectorOwned) && doc.Source != nil:
		return nil, s.connectorOwned(ctx, doc.WorkspaceID, doc.SetID, "document")
	case errors.Is(err, foundrystore.ErrConnectorOwned):
		return nil, s.connectorOwned(ctx, doc.WorkspaceID, req.GetSetId(), "set")
	case errors.Is(err, foundrystore.ErrNotFound):
		// The document was just found, so the set is gone, or the document was deleted since.
		return nil, status.Error(codes.NotFound, "document set not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "move document: %v", err)
	}
	return &foundryv1.FoundryDocumentReadsResponse{Document: foundryDocumentProto(moved, nil), Reads: foundryReadsProto(reads)}, nil
}

// foundryFanOut is what completing or moving a document of workspaceID hands
// the ontologies that read its set: the caller as the requester of the
// extracts it queues, the month's budget, and the databases the caller may
// write (the actor's query_write scope of the workspace; none without one).
func (s *Service) foundryFanOut(ctx context.Context, workspaceID string) foundrymodel.FoundryDocumentFanOut {
	caller := actor(ctx)
	_, write := caller.Scope(workspaceID)
	fan := foundrymodel.FoundryDocumentFanOut{
		NewJobID: id.New, Budget: s.deps.Cfg.Foundry.Graph.MonthBudget(time.Now()),
		CanWrite: write.Allows,
	}
	if userID := caller.UserID; userID != "" {
		fan.RequestedByUserID = &userID
	}
	return fan
}

// ListFoundryDocuments lists the workspace's uploads that are not deleted,
// newest first; with set_id, that set's documents of either origin (a
// connector's documents are many, and are listed by their set). With ids it
// returns those documents only, of either origin and deleted or not, in the
// order asked, for pages that name documents another read returned (an
// entity's sources, say).
func (s *Service) ListFoundryDocuments(ctx context.Context, req *foundryv1.ListFoundryDocumentsRequest) (*foundryv1.ListFoundryDocumentsResponse, error) {
	if req.GetWorkspaceId() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id is required")
	}
	if ids := req.GetIds(); len(ids) > 0 {
		switch {
		case len(ids) > maxFoundryDocumentIDs:
			return nil, status.Errorf(codes.InvalidArgument, "ids may name at most %d documents", maxFoundryDocumentIDs)
		case req.GetSetId() != "" || req.GetPageToken() != "":
			return nil, status.Error(codes.InvalidArgument, "ids takes no set_id or page_token: it lists those documents only")
		}
		byID, err := s.deps.DB.ListFoundryDocumentsByID(ctx, req.GetWorkspaceId(), ids)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "list documents: %v", err)
		}
		resp := &foundryv1.ListFoundryDocumentsResponse{}
		for _, id := range ids {
			// Deleting from the map lists an id asked for twice once.
			if d, ok := byID[id]; ok {
				resp.Documents = append(resp.Documents, foundryDocumentProto(d, nil))
				delete(byID, id)
			}
		}
		return resp, nil
	}
	docs, next, err := s.deps.DB.ListFoundryDocuments(ctx, req.GetWorkspaceId(), req.GetSetId(), req.GetPageToken(), foundryPageSize)
	switch {
	case errors.Is(err, foundrystore.ErrInvalidPageToken):
		return nil, status.Error(codes.InvalidArgument, "page_token is not one this list returned")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "list documents: %v", err)
	}
	resp := &foundryv1.ListFoundryDocumentsResponse{NextPageToken: next}
	for _, d := range docs {
		resp.Documents = append(resp.Documents, foundryDocumentProto(d, nil))
	}
	return resp, nil
}

// GetFoundryDocument returns a document of the workspace with the
// ontologies it is associated with: each live ontology whose current load
// holds it, or whose reading version (foundryReader) has its set in scope,
// with whether that version routes the set and the document's extraction in
// the current load. A workspace has few ontologies; each costs a few reads.
func (s *Service) GetFoundryDocument(ctx context.Context, req *foundryv1.GetFoundryDocumentRequest) (*foundryv1.FoundryDocumentResponse, error) {
	doc, err := s.foundryDocument(ctx, req.GetWorkspaceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	readers, err := s.foundryReaders(ctx, doc.WorkspaceID)
	if err != nil {
		return nil, err
	}
	out := foundryDocumentProto(doc, nil)
	for _, r := range readers {
		var extraction *foundrymodel.FoundryDocumentExtraction
		holds := false
		if load := r.ontology.CurrentLoadID; load != nil {
			if holds, err = s.deps.DB.FoundryLoadHoldsDocument(ctx, *load, doc.ID); err != nil {
				return nil, status.Errorf(codes.Internal, "read current load: %v", err)
			}
			if extraction, err = s.foundryExtraction(ctx, *load, doc.ID); err != nil {
				return nil, err
			}
		}
		if !holds && !slices.Contains(r.scope, doc.SetID) {
			continue
		}
		out.Ontologies = append(out.Ontologies, &foundryv1.FoundryDocumentOntology{
			OntologyId: r.ontology.ID, Routed: r.routes.Routes(doc.SetID), Extraction: foundryExtractionProto(extraction),
		})
	}
	return &foundryv1.FoundryDocumentResponse{Document: out}, nil
}

// UpdateFoundryDocument renames a document that is not deleted, by the
// rules an upload's name follows: 1 to 255 characters once trimmed, with no
// slash, backslash or control character. Its extension needn't change: the
// type was fixed at upload. A connector's document is named by its source
// (connectorOwned).
func (s *Service) UpdateFoundryDocument(ctx context.Context, req *foundryv1.UpdateFoundryDocumentRequest) (*foundryv1.FoundryDocumentResponse, error) {
	name := strings.TrimSpace(req.GetName())
	if !document.ValidName(name) {
		return nil, status.Error(codes.InvalidArgument, "name must be 1 to 255 characters with no slash, backslash or control character")
	}
	doc, err := s.foundryDocument(ctx, req.GetWorkspaceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	switch err := s.deps.DB.RenameFoundryDocument(ctx, doc.WorkspaceID, doc.ID, name); {
	case errors.Is(err, foundrystore.ErrConnectorOwned):
		return nil, s.connectorOwned(ctx, doc.WorkspaceID, doc.SetID, "document")
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.FailedPrecondition, "the document is deleted")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "rename document: %v", err)
	}
	renamed, err := s.deps.DB.GetFoundryDocument(ctx, doc.WorkspaceID, doc.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get document: %v", err)
	}
	return &foundryv1.FoundryDocumentResponse{Document: foundryDocumentProto(renamed, nil)}, nil
}

// DeleteFoundryDocument marks the document deleted, which also removes its
// reads, then removes everything under its prefix: its original, what
// reading it gave, and its extraction artifacts. Marking first means no
// worker records a read of it after; one that wrote under the prefix since
// removes what it wrote. Deleting a deleted document removes its prefix
// again, so a retry after a failed removal finishes it. What it put in
// databases stays there. A connector's document goes when its source's row
// does, or when its stream is turned off (connectorOwned).
func (s *Service) DeleteFoundryDocument(ctx context.Context, req *foundryv1.DeleteFoundryDocumentRequest) (*foundryv1.FoundryDocumentResponse, error) {
	return s.deleteFoundryDocument(ctx, "", req)
}

func (s *Service) deleteFoundryDocument(ctx context.Context, ontologyID string, req *foundryv1.DeleteFoundryDocumentRequest) (*foundryv1.FoundryDocumentResponse, error) {
	if _, err := s.foundryDocumentsWorkspace(ctx, req.GetWorkspaceId(), ontologyID); err != nil {
		return nil, err
	}
	doc, err := s.foundryDocument(ctx, req.GetWorkspaceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	if doc.Source != nil {
		return nil, s.connectorOwned(ctx, doc.WorkspaceID, doc.SetID, "document")
	}
	if s.deps.Documents == nil {
		return nil, status.Error(codes.Unavailable, "documents are not available here")
	}
	if err := s.deps.DB.DeleteFoundryDocument(ctx, doc.WorkspaceID, doc.ID); err != nil && !errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Errorf(codes.Internal, "delete document: %v", err)
	}
	if err := s.deps.Documents.DeletePrefix(ctx, document.Prefix(doc.WorkspaceID, doc.ID)); err != nil {
		log.From(ctx).Warn("delete document files failed", "document", doc.ID, "err", err)
		return nil, status.Error(codes.Unavailable, "the document's files could not be removed; try again")
	}
	deleted, err := s.deps.DB.GetFoundryDocument(ctx, doc.WorkspaceID, doc.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get document: %v", err)
	}
	return &foundryv1.FoundryDocumentResponse{Document: foundryDocumentProto(deleted, nil)}, nil
}

// GetFoundryDocumentContent returns an uploaded document's text, for the
// Data document page: a text or table file's bytes (foundryDocumentBytes),
// or the text a PDF's read stored, as loads read it (its pages split by
// form feeds). Bytes that are not UTF-8 read as U+FFFD. Parquet, and a PDF
// whose read has not succeeded, are FAILED_PRECONDITION:
// GetFoundryDocumentBytes reads their bytes.
func (s *Service) GetFoundryDocumentContent(ctx context.Context, req *foundryv1.GetFoundryDocumentContentRequest) (*foundryv1.GetFoundryDocumentContentResponse, error) {
	doc, err := s.foundryDocument(ctx, req.GetWorkspaceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	var text []byte
	switch {
	case doc.MediaType.ReadKind() == foundrymodel.FoundryReadPDF:
		text, err = s.foundryPDFText(ctx, doc)
	case doc.MediaType.Binary():
		return nil, status.Errorf(codes.FailedPrecondition, "%s documents have no text; use :bytes", doc.MediaType.Format())
	default:
		text, err = s.foundryDocumentBytes(ctx, doc)
	}
	if err != nil {
		return nil, err
	}
	return &foundryv1.GetFoundryDocumentContentResponse{Text: strings.ToValidUTF8(string(text), "�")}, nil
}

// foundryPDFText reads the text an uploaded PDF's read stored
// (foundrymodel.FoundryDocument.TextKey), at most foundry.documents.max_bytes, the
// cap the read held it to. A PDF still being read, or whose read failed, is
// FAILED_PRECONDITION, saying which.
func (s *Service) foundryPDFText(ctx context.Context, doc *foundrymodel.FoundryDocument) ([]byte, error) {
	key, ok := doc.TextKey()
	switch {
	case doc.Status != foundrymodel.FoundryDocumentUploaded:
		return nil, status.Errorf(codes.FailedPrecondition, "the document is %s; only an uploaded document can be read", doc.Status)
	case !ok && doc.ReadState() == foundrymodel.FoundryFileReadReading:
		return nil, status.Error(codes.FailedPrecondition, "the PDF's text is still being read; try again shortly")
	case !ok && doc.Read != nil && doc.Read.Error != nil:
		return nil, status.Errorf(codes.FailedPrecondition, "the PDF's text can't be read: %s", doc.Read.Error.Message)
	case !ok:
		return nil, status.Error(codes.FailedPrecondition, "the PDF's text can't be read")
	case s.deps.Documents == nil:
		return nil, status.Error(codes.Unavailable, "documents are not available here")
	}
	body, err := s.deps.Documents.Open(ctx, key)
	if errors.Is(err, document.ErrMissing) {
		return nil, status.Error(codes.FailedPrecondition, "the PDF's text is gone")
	}
	if err != nil {
		log.From(ctx).Warn("read a PDF's text failed", "document", doc.ID, "err", err)
		return nil, status.Error(codes.Unavailable, "the document could not be read; try again")
	}
	defer func() { _ = body.Close() }() // best-effort: only read
	text, err := io.ReadAll(io.LimitReader(body, s.deps.Cfg.Foundry.Documents.MaxBytes))
	if err != nil {
		log.From(ctx).Warn("read a PDF's text failed", "document", doc.ID, "err", err)
		return nil, status.Error(codes.Unavailable, "the document could not be read; try again")
	}
	return text, nil
}

// GetFoundryDocumentBytes returns an uploaded document's bytes and type, of
// any type, for the Data document page (foundryDocumentBytes).
func (s *Service) GetFoundryDocumentBytes(ctx context.Context, req *foundryv1.GetFoundryDocumentBytesRequest) (*foundryv1.GetFoundryDocumentBytesResponse, error) {
	doc, err := s.foundryDocument(ctx, req.GetWorkspaceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	data, err := s.foundryDocumentBytes(ctx, doc)
	if err != nil {
		return nil, err
	}
	return &foundryv1.GetFoundryDocumentBytesResponse{MediaType: string(doc.MediaType), Data: data}, nil
}

// foundryDocumentBytes reads an uploaded document's original whole, at most
// its type's cap (foundry.documents.MaxBytesFor, the cap its upload was held
// to), through the backend: the bucket's CORS lets a browser PUT, not GET. A
// document past the cap, which a later, lower cap leaves behind, is
// FAILED_PRECONDITION.
func (s *Service) foundryDocumentBytes(ctx context.Context, doc *foundrymodel.FoundryDocument) ([]byte, error) {
	maxBytes := s.deps.Cfg.Foundry.Documents.MaxBytesFor(doc.MediaType)
	switch {
	case doc.Status != foundrymodel.FoundryDocumentUploaded:
		return nil, status.Errorf(codes.FailedPrecondition, "the document is %s; only an uploaded document can be read", doc.Status)
	case doc.SizeBytes > maxBytes:
		return nil, status.Errorf(codes.FailedPrecondition, "the document is larger than %d KiB", maxBytes>>10)
	case s.deps.Documents == nil:
		return nil, status.Error(codes.Unavailable, "documents are not available here")
	}
	body, err := s.deps.Documents.Open(ctx, doc.S3Key)
	if errors.Is(err, document.ErrMissing) {
		return nil, status.Error(codes.FailedPrecondition, "the document's original is gone")
	}
	if err != nil {
		log.From(ctx).Warn("read document original failed", "document", doc.ID, "err", err)
		return nil, status.Error(codes.Unavailable, "the document could not be read; try again")
	}
	defer func() { _ = body.Close() }() // best-effort: only read
	data, err := io.ReadAll(io.LimitReader(body, maxBytes))
	if err != nil {
		log.From(ctx).Warn("read document original failed", "document", doc.ID, "err", err)
		return nil, status.Error(codes.Unavailable, "the document could not be read; try again")
	}
	return data, nil
}

// ListFoundryOntologyDocuments returns what an ontology reads, newest first:
// its current load's documents, and the uploaded documents of its reading
// version's scope, each with its extraction in the current load. status
// keeps those the current load holds so (foundrymodel.FoundryDocumentFilter).
func (s *Service) ListFoundryOntologyDocuments(ctx context.Context, req *foundryv1.ListFoundryOntologyDocumentsRequest) (*foundryv1.ListFoundryDocumentsResponse, error) {
	filter := foundrymodel.FoundryDocumentFilter(req.GetStatus())
	if !filter.Valid() {
		return nil, status.Errorf(codes.InvalidArgument, "status %q is none of waiting, extracted, review, duplicate, failed or out_of_scope", filter)
	}
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	reader, err := s.foundryReaderOf(ctx, o)
	if err != nil {
		return nil, err
	}
	var currentLoad string
	if o.CurrentLoadID != nil {
		currentLoad = *o.CurrentLoadID
	}
	docs, next, err := s.deps.DB.ListFoundryOntologyDocuments(ctx, o.WorkspaceID, currentLoad, reader.scope, filter, req.GetPageToken(), foundryPageSize)
	switch {
	case errors.Is(err, foundrystore.ErrInvalidPageToken):
		return nil, status.Error(codes.InvalidArgument, "page_token is not one this list returned")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "list documents: %v", err)
	}
	extractions := map[string]*foundrymodel.FoundryDocumentExtraction{}
	if currentLoad != "" {
		ids := make([]string, 0, len(docs))
		for _, d := range docs {
			ids = append(ids, d.ID)
		}
		if extractions, err = s.deps.DB.ListFoundryDocumentExtractions(ctx, currentLoad, ids); err != nil {
			return nil, status.Errorf(codes.Internal, "list document extractions: %v", err)
		}
	}
	resp := &foundryv1.ListFoundryDocumentsResponse{NextPageToken: next}
	for _, d := range docs {
		resp.Documents = append(resp.Documents, foundryDocumentProto(d, extractions[d.ID]))
	}
	return resp, nil
}

// GetFoundryOntologyDocument returns any document of the workspace (NotFound
// only outside it) with its extraction in the ontology's current load, or
// none: a load keeps documents that have since moved out of scope, so a
// citation of one still opens.
func (s *Service) GetFoundryOntologyDocument(ctx context.Context, req *foundryv1.GetFoundryOntologyDocumentRequest) (*foundryv1.FoundryDocumentResponse, error) {
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	doc, err := s.foundryDocument(ctx, o.WorkspaceID, req.GetId())
	if err != nil {
		return nil, err
	}
	if o.CurrentLoadID == nil {
		return &foundryv1.FoundryDocumentResponse{Document: foundryDocumentProto(doc, nil)}, nil
	}
	extraction, err := s.foundryExtraction(ctx, *o.CurrentLoadID, doc.ID)
	if err != nil {
		return nil, err
	}
	return &foundryv1.FoundryDocumentResponse{Document: foundryDocumentProto(doc, extraction)}, nil
}

// foundryDocument is a document of the workspace.
func (s *Service) foundryDocument(ctx context.Context, workspaceID, docID string) (*foundrymodel.FoundryDocument, error) {
	if workspaceID == "" || docID == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id and id are required")
	}
	doc, err := s.deps.DB.GetFoundryDocument(ctx, workspaceID, docID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "document not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get document: %v", err)
	}
	return doc, nil
}

// foundryExtraction is what extracting a document into a load made of it;
// nil when the load has not extracted it.
func (s *Service) foundryExtraction(ctx context.Context, loadID, docID string) (*foundrymodel.FoundryDocumentExtraction, error) {
	extraction, err := s.deps.DB.GetFoundryDocumentExtraction(ctx, loadID, docID)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get document extraction: %v", err)
	}
	return extraction, nil
}

var foundryReadOutcomeToProto = map[foundrymodel.FoundryDocumentReadOutcome]foundryv1.FoundryDocumentReadOutcome{
	foundrymodel.FoundryDocumentReadQueued:   foundryv1.FoundryDocumentReadOutcome_FOUNDRY_DOCUMENT_READ_OUTCOME_QUEUED,
	foundrymodel.FoundryDocumentReadOwed:     foundryv1.FoundryDocumentReadOutcome_FOUNDRY_DOCUMENT_READ_OUTCOME_OWED,
	foundrymodel.FoundryDocumentReadNoBudget: foundryv1.FoundryDocumentReadOutcome_FOUNDRY_DOCUMENT_READ_OUTCOME_NO_BUDGET,
	foundrymodel.FoundryDocumentReadHeld:     foundryv1.FoundryDocumentReadOutcome_FOUNDRY_DOCUMENT_READ_OUTCOME_HELD,
	foundrymodel.FoundryDocumentReadSkipped:  foundryv1.FoundryDocumentReadOutcome_FOUNDRY_DOCUMENT_READ_OUTCOME_SKIPPED,
	foundrymodel.FoundryDocumentReadReading:  foundryv1.FoundryDocumentReadOutcome_FOUNDRY_DOCUMENT_READ_OUTCOME_READING,
}

func foundryReadsProto(reads []foundrymodel.FoundryDocumentRead) []*foundryv1.FoundryDocumentRead {
	out := make([]*foundryv1.FoundryDocumentRead, 0, len(reads))
	for _, r := range reads {
		out = append(out, &foundryv1.FoundryDocumentRead{OntologyId: r.OntologyID, Outcome: foundryReadOutcomeToProto[r.Outcome]})
	}
	return out
}

func foundryDocumentProto(d *foundrymodel.FoundryDocument, e *foundrymodel.FoundryDocumentExtraction) *foundryv1.FoundryDocument {
	out := &foundryv1.FoundryDocument{
		Id: d.ID, Name: d.Name, MediaType: string(d.MediaType), SizeBytes: d.SizeBytes, Sha256: d.SHA256,
		Status: string(d.Status), CreatedAt: timestamppb.New(d.CreatedAt), SetId: d.SetID, Extraction: foundryExtractionProto(e),
		Read: foundryFileReadProto(d),
	}
	if src := d.Source; src != nil {
		out.Source = &foundryv1.FoundryDocumentSource{ExternalId: src.ExternalID, Version: src.Version, At: timestamppb.New(src.At)}
		if src.URL != nil {
			out.Source.Url = *src.URL
		}
	}
	return out
}

var foundryReadKindToProto = map[foundrymodel.FoundryReadKind]foundryv1.FoundryReadKind{
	foundrymodel.FoundryReadText:  foundryv1.FoundryReadKind_FOUNDRY_READ_KIND_TEXT,
	foundrymodel.FoundryReadTable: foundryv1.FoundryReadKind_FOUNDRY_READ_KIND_TABLE,
	foundrymodel.FoundryReadPDF:   foundryv1.FoundryReadKind_FOUNDRY_READ_KIND_PDF,
}

var foundryFileReadStateToProto = map[foundrymodel.FoundryFileReadState]foundryv1.FoundryFileReadState{
	foundrymodel.FoundryFileReadReady:       foundryv1.FoundryFileReadState_FOUNDRY_FILE_READ_STATE_READY,
	foundrymodel.FoundryFileReadReading:     foundryv1.FoundryFileReadState_FOUNDRY_FILE_READ_STATE_READING,
	foundrymodel.FoundryFileReadFailedState: foundryv1.FoundryFileReadState_FOUNDRY_FILE_READ_STATE_FAILED,
}

var foundryFileReadModeToProto = map[foundrymodel.FoundryFileReadMode]foundryv1.FoundryFileReadMode{
	foundrymodel.FoundryFileReadStandard: foundryv1.FoundryFileReadMode_FOUNDRY_FILE_READ_MODE_STANDARD,
	foundrymodel.FoundryFileReadDeep:     foundryv1.FoundryFileReadMode_FOUNDRY_FILE_READ_MODE_DEEP,
}

// foundryFileReadProto is how ontologies read an uploaded document and what
// reading it has come to; nil before its upload completes and once deleted.
func foundryFileReadProto(d *foundrymodel.FoundryDocument) *foundryv1.FoundryFileRead {
	if d.Status != foundrymodel.FoundryDocumentUploaded {
		return nil
	}
	out := &foundryv1.FoundryFileRead{Kind: foundryReadKindToProto[d.MediaType.ReadKind()], State: foundryFileReadStateToProto[d.ReadState()]}
	r := d.Read
	if r == nil {
		return out
	}
	out.Mode, out.Rows, out.Retrying = foundryFileReadModeToProto[r.Mode], r.RowCount, r.Retrying()
	if r.PageCount != nil {
		pages := int32(*r.PageCount)
		out.Pages = &pages
	}
	if r.Error != nil {
		out.Error = &foundryv1.FoundryFileReadError{Code: string(r.Error.Code), Message: r.Error.Message}
	}
	return out
}

// foundryExtractionProto is nil for no extraction.
func foundryExtractionProto(e *foundrymodel.FoundryDocumentExtraction) *foundryv1.FoundryDocumentExtraction {
	if e == nil {
		return nil
	}
	c := e.Counts
	out := &foundryv1.FoundryDocumentExtraction{
		Status: string(e.Status), DuplicateOf: e.DuplicateOf, Sections: int32(c.Sections), Entities: int32(c.Entities),
		Mentions: int32(c.Mentions), Facts: int32(c.Facts), Reviews: int32(c.Reviews), UsdMicros: e.USDMicros,
		FinishedAt:            timestamppb.New(e.FinishedAt),
		Attributes:            int32(c.Attributes),
		AttributeReviews:      int32(c.AttributeReviews),
		PropConflicts:         int32(c.PropConflicts),
		Created:               int32(c.Created),
		IdentifiersUnverified: int32(c.IdentifiersUnverified),
		AttributesOverEntries: int32(c.AttributesOverEntries),
		ResolvedUser:          int32(c.ResolvedUser),
		ResolvedAdjudicated:   int32(c.ResolvedAdjudicated),
		ResolvedIdentifier:    int32(c.ResolvedIdentifier),
		ResolvedName:          int32(c.ResolvedName),
		ResolvedAlias:         int32(c.ResolvedAlias),
		ResolvedCreated:       int32(c.ResolvedCreated),
	}
	if e.Error != nil {
		out.Error = &foundryv1.FoundryExtractionError{Code: string(e.Error.Code), Retryable: e.Error.Retryable}
	}
	return out
}
