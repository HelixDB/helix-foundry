package api

import (
	"context"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// The ontology-scoped aliases of the document and document set routes, from
// before documents were the workspace's. Each acts on the workspace's
// documents and sets as its route does, once its ontology is found to be a
// live ontology of the workspace (foundryDocumentsWorkspace).

func (s *Service) CreateFoundryOntologyDocument(ctx context.Context, req *foundryv1.CreateFoundryOntologyDocumentRequest) (
	*foundryv1.CreateFoundryOntologyDocumentResponse, error) {
	return s.createFoundryDocument(ctx, req.GetOntologyId(), &foundryv1.CreateFoundryDocumentRequest{
		WorkspaceId: req.GetWorkspaceId(), Name: req.GetName(), MediaType: req.GetMediaType(), SizeBytes: req.GetSizeBytes(),
		Sha256: req.GetSha256(), ClientToken: req.GetClientToken(), SetId: req.GetSetId(),
	})
}

func (s *Service) CompleteFoundryOntologyDocumentUpload(ctx context.Context, req *foundryv1.CompleteFoundryOntologyDocumentUploadRequest) (
	*foundryv1.FoundryDocumentReadsResponse, error) {
	return s.completeFoundryDocumentUpload(ctx, req.GetOntologyId(), &foundryv1.CompleteFoundryDocumentUploadRequest{
		WorkspaceId: req.GetWorkspaceId(), Id: req.GetId(),
	})
}

func (s *Service) MoveFoundryOntologyDocument(ctx context.Context, req *foundryv1.MoveFoundryOntologyDocumentRequest) (
	*foundryv1.FoundryDocumentReadsResponse, error) {
	return s.moveFoundryDocument(ctx, req.GetOntologyId(), &foundryv1.MoveFoundryDocumentRequest{
		WorkspaceId: req.GetWorkspaceId(), Id: req.GetId(), SetId: req.GetSetId(),
	})
}

func (s *Service) DeleteFoundryOntologyDocument(ctx context.Context, req *foundryv1.DeleteFoundryOntologyDocumentRequest) (
	*foundryv1.FoundryDocumentResponse, error) {
	return s.deleteFoundryDocument(ctx, req.GetOntologyId(), &foundryv1.DeleteFoundryDocumentRequest{
		WorkspaceId: req.GetWorkspaceId(), Id: req.GetId(),
	})
}

func (s *Service) ListFoundryOntologyDocumentSets(ctx context.Context, req *foundryv1.ListFoundryOntologyDocumentSetsRequest) (
	*foundryv1.ListFoundryDocumentSetsResponse, error) {
	return s.listFoundryDocumentSets(ctx, req.GetOntologyId(), &foundryv1.ListFoundryDocumentSetsRequest{WorkspaceId: req.GetWorkspaceId()})
}

func (s *Service) CreateFoundryOntologyDocumentSet(ctx context.Context, req *foundryv1.CreateFoundryOntologyDocumentSetRequest) (
	*foundryv1.FoundryDocumentSetResponse, error) {
	return s.createFoundryDocumentSet(ctx, req.GetOntologyId(), &foundryv1.CreateFoundryDocumentSetRequest{
		WorkspaceId: req.GetWorkspaceId(), Name: req.GetName(), Description: req.GetDescription(),
	})
}

func (s *Service) UpdateFoundryOntologyDocumentSet(ctx context.Context, req *foundryv1.UpdateFoundryOntologyDocumentSetRequest) (
	*foundryv1.FoundryDocumentSetResponse, error) {
	return s.updateFoundryDocumentSet(ctx, req.GetOntologyId(), &foundryv1.UpdateFoundryDocumentSetRequest{
		WorkspaceId: req.GetWorkspaceId(), SetId: req.GetSetId(), Name: req.Name, Description: req.Description,
	})
}

func (s *Service) DeleteFoundryOntologyDocumentSet(ctx context.Context, req *foundryv1.DeleteFoundryOntologyDocumentSetRequest) (
	*foundryv1.DeleteFoundryDocumentSetResponse, error) {
	return s.deleteFoundryDocumentSet(ctx, req.GetOntologyId(), &foundryv1.DeleteFoundryDocumentSetRequest{
		WorkspaceId: req.GetWorkspaceId(), SetId: req.GetSetId(),
	})
}
