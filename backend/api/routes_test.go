package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// routeRecorder records which Foundry RPC each HTTP request reached, with its ids.
type routeRecorder struct {
	foundryv1.UnimplementedFoundryServiceServer
	got string
}

type ids interface {
	GetWorkspaceId() string
	GetOntologyId() string
}

func (r *routeRecorder) hit(name string, req ids, id string) {
	r.got = name + " " + req.GetWorkspaceId() + " " + req.GetOntologyId() + " " + id
}

func (r *routeRecorder) CreateFoundryOntology(_ context.Context, req *foundryv1.CreateFoundryOntologyRequest) (*foundryv1.FoundryOntologyResponse, error) {
	r.got = "CreateFoundryOntology " + req.GetWorkspaceId() + " " + req.GetTenantId()
	return &foundryv1.FoundryOntologyResponse{}, nil
}

func (r *routeRecorder) ListFoundryOntologies(_ context.Context, req *foundryv1.ListFoundryOntologiesRequest) (*foundryv1.ListFoundryOntologiesResponse, error) {
	r.got = "ListFoundryOntologies " + req.GetWorkspaceId() + " " + req.GetPageToken()
	return &foundryv1.ListFoundryOntologiesResponse{}, nil
}

func (r *routeRecorder) GetFoundryOntology(_ context.Context, req *foundryv1.GetFoundryOntologyRequest) (*foundryv1.FoundryOntologyResponse, error) {
	r.hit("GetFoundryOntology", req, "")
	return &foundryv1.FoundryOntologyResponse{}, nil
}

func (r *routeRecorder) DeleteFoundryOntology(_ context.Context, req *foundryv1.DeleteFoundryOntologyRequest) (*foundryv1.FoundryOntologyResponse, error) {
	r.hit("DeleteFoundryOntology", req, "")
	return &foundryv1.FoundryOntologyResponse{}, nil
}

func (r *routeRecorder) GetCurrentOntologyVersion(_ context.Context, req *foundryv1.GetCurrentOntologyVersionRequest) (*foundryv1.OntologyVersionResponse, error) {
	r.hit("GetCurrentOntologyVersion", req, "")
	return &foundryv1.OntologyVersionResponse{}, nil
}

func (r *routeRecorder) GetOntologyVersion(_ context.Context, req *foundryv1.GetOntologyVersionRequest) (*foundryv1.OntologyVersionResponse, error) {
	r.hit("GetOntologyVersion", req, req.GetId())
	return &foundryv1.OntologyVersionResponse{}, nil
}

func (r *routeRecorder) SetCurrentOntologyVersion(_ context.Context, req *foundryv1.SetCurrentOntologyVersionRequest) (*foundryv1.OntologyVersionResponse, error) {
	r.hit("SetCurrentOntologyVersion", req, req.GetVersionId())
	return &foundryv1.OntologyVersionResponse{}, nil
}

func (r *routeRecorder) GenerateOntology(_ context.Context, req *foundryv1.GenerateOntologyRequest) (*foundryv1.OntologyGenerationResponse, error) {
	picks := ""
	for _, pick := range req.GetEventSources() {
		picks += " " + pick.GetConnectorId() + ":" + pick.GetMode().String()
	}
	r.hit("GenerateOntology", req, req.GetGoal()+picks)
	return &foundryv1.OntologyGenerationResponse{}, nil
}

func (r *routeRecorder) GetOntologyGeneration(_ context.Context, req *foundryv1.GetOntologyGenerationRequest) (*foundryv1.OntologyGenerationResponse, error) {
	r.hit("GetOntologyGeneration", req, req.GetId())
	return &foundryv1.OntologyGenerationResponse{}, nil
}

// workspaceHit records a workspace route, which names no ontology.
func (r *routeRecorder) workspaceHit(name, workspaceID, id string) {
	r.got = name + " " + workspaceID + " - " + id
}

func (r *routeRecorder) CreateFoundryDocument(_ context.Context, req *foundryv1.CreateFoundryDocumentRequest) (*foundryv1.CreateFoundryDocumentResponse, error) {
	r.workspaceHit("CreateFoundryDocument", req.GetWorkspaceId(), req.GetName()+" in "+req.GetSetId())
	return &foundryv1.CreateFoundryDocumentResponse{}, nil
}

func (r *routeRecorder) CreateFoundryOntologyDocument(_ context.Context, req *foundryv1.CreateFoundryOntologyDocumentRequest) (*foundryv1.CreateFoundryOntologyDocumentResponse, error) {
	r.hit("CreateFoundryOntologyDocument", req, req.GetName())
	return &foundryv1.CreateFoundryOntologyDocumentResponse{}, nil
}

func (r *routeRecorder) ListFoundryDocuments(_ context.Context, req *foundryv1.ListFoundryDocumentsRequest) (*foundryv1.ListFoundryDocumentsResponse, error) {
	r.workspaceHit("ListFoundryDocuments", req.GetWorkspaceId(), req.GetSetId()+"/"+req.GetPageToken()+"/"+strings.Join(req.GetIds(), ","))
	return &foundryv1.ListFoundryDocumentsResponse{}, nil
}

func (r *routeRecorder) GetFoundryDocument(_ context.Context, req *foundryv1.GetFoundryDocumentRequest) (*foundryv1.FoundryDocumentResponse, error) {
	r.workspaceHit("GetFoundryDocument", req.GetWorkspaceId(), req.GetId())
	return &foundryv1.FoundryDocumentResponse{}, nil
}

func (r *routeRecorder) UpdateFoundryDocument(_ context.Context, req *foundryv1.UpdateFoundryDocumentRequest) (*foundryv1.FoundryDocumentResponse, error) {
	r.workspaceHit("UpdateFoundryDocument", req.GetWorkspaceId(), req.GetId()+" "+req.GetName())
	return &foundryv1.FoundryDocumentResponse{}, nil
}

func (r *routeRecorder) DeleteFoundryDocument(_ context.Context, req *foundryv1.DeleteFoundryDocumentRequest) (*foundryv1.FoundryDocumentResponse, error) {
	r.workspaceHit("DeleteFoundryDocument", req.GetWorkspaceId(), req.GetId())
	return &foundryv1.FoundryDocumentResponse{}, nil
}

func (r *routeRecorder) DeleteFoundryOntologyDocument(_ context.Context, req *foundryv1.DeleteFoundryOntologyDocumentRequest) (*foundryv1.FoundryDocumentResponse, error) {
	r.hit("DeleteFoundryOntologyDocument", req, req.GetId())
	return &foundryv1.FoundryDocumentResponse{}, nil
}

func (r *routeRecorder) GetFoundryDocumentContent(_ context.Context, req *foundryv1.GetFoundryDocumentContentRequest) (*foundryv1.GetFoundryDocumentContentResponse, error) {
	r.workspaceHit("GetFoundryDocumentContent", req.GetWorkspaceId(), req.GetId())
	return &foundryv1.GetFoundryDocumentContentResponse{}, nil
}

func (r *routeRecorder) GetFoundryDocumentBytes(_ context.Context, req *foundryv1.GetFoundryDocumentBytesRequest) (*foundryv1.GetFoundryDocumentBytesResponse, error) {
	r.workspaceHit("GetFoundryDocumentBytes", req.GetWorkspaceId(), req.GetId())
	return &foundryv1.GetFoundryDocumentBytesResponse{}, nil
}

func (r *routeRecorder) CompleteFoundryDocumentUpload(_ context.Context, req *foundryv1.CompleteFoundryDocumentUploadRequest) (*foundryv1.FoundryDocumentReadsResponse, error) {
	r.workspaceHit("CompleteFoundryDocumentUpload", req.GetWorkspaceId(), req.GetId())
	return &foundryv1.FoundryDocumentReadsResponse{}, nil
}

func (r *routeRecorder) CompleteFoundryOntologyDocumentUpload(_ context.Context, req *foundryv1.CompleteFoundryOntologyDocumentUploadRequest) (*foundryv1.FoundryDocumentReadsResponse, error) {
	r.hit("CompleteFoundryOntologyDocumentUpload", req, req.GetId())
	return &foundryv1.FoundryDocumentReadsResponse{}, nil
}

func (r *routeRecorder) MoveFoundryDocument(_ context.Context, req *foundryv1.MoveFoundryDocumentRequest) (*foundryv1.FoundryDocumentReadsResponse, error) {
	r.workspaceHit("MoveFoundryDocument", req.GetWorkspaceId(), req.GetId()+">"+req.GetSetId())
	return &foundryv1.FoundryDocumentReadsResponse{}, nil
}

func (r *routeRecorder) MoveFoundryOntologyDocument(_ context.Context, req *foundryv1.MoveFoundryOntologyDocumentRequest) (*foundryv1.FoundryDocumentReadsResponse, error) {
	r.hit("MoveFoundryOntologyDocument", req, req.GetId()+">"+req.GetSetId())
	return &foundryv1.FoundryDocumentReadsResponse{}, nil
}

func (r *routeRecorder) ListFoundryOntologyDocuments(_ context.Context, req *foundryv1.ListFoundryOntologyDocumentsRequest) (*foundryv1.ListFoundryDocumentsResponse, error) {
	r.hit("ListFoundryOntologyDocuments", req, req.GetStatus()+"/"+req.GetPageToken())
	return &foundryv1.ListFoundryDocumentsResponse{}, nil
}

func (r *routeRecorder) GetFoundryOntologyDocument(_ context.Context, req *foundryv1.GetFoundryOntologyDocumentRequest) (*foundryv1.FoundryDocumentResponse, error) {
	r.hit("GetFoundryOntologyDocument", req, req.GetId())
	return &foundryv1.FoundryDocumentResponse{}, nil
}

func (r *routeRecorder) CreateFoundryDocumentSet(_ context.Context, req *foundryv1.CreateFoundryDocumentSetRequest) (*foundryv1.FoundryDocumentSetResponse, error) {
	r.workspaceHit("CreateFoundryDocumentSet", req.GetWorkspaceId(), req.GetName()+"/"+req.GetDescription())
	return &foundryv1.FoundryDocumentSetResponse{}, nil
}

func (r *routeRecorder) CreateFoundryOntologyDocumentSet(_ context.Context, req *foundryv1.CreateFoundryOntologyDocumentSetRequest) (*foundryv1.FoundryDocumentSetResponse, error) {
	r.hit("CreateFoundryOntologyDocumentSet", req, req.GetName()+"/"+req.GetDescription())
	return &foundryv1.FoundryDocumentSetResponse{}, nil
}

func (r *routeRecorder) ListFoundryDocumentSets(_ context.Context, req *foundryv1.ListFoundryDocumentSetsRequest) (*foundryv1.ListFoundryDocumentSetsResponse, error) {
	r.workspaceHit("ListFoundryDocumentSets", req.GetWorkspaceId(), "")
	return &foundryv1.ListFoundryDocumentSetsResponse{}, nil
}

func (r *routeRecorder) ListFoundryOntologyDocumentSets(_ context.Context, req *foundryv1.ListFoundryOntologyDocumentSetsRequest) (*foundryv1.ListFoundryDocumentSetsResponse, error) {
	r.hit("ListFoundryOntologyDocumentSets", req, "")
	return &foundryv1.ListFoundryDocumentSetsResponse{}, nil
}

func (r *routeRecorder) UpdateFoundryDocumentSet(_ context.Context, req *foundryv1.UpdateFoundryDocumentSetRequest) (*foundryv1.FoundryDocumentSetResponse, error) {
	// Which fields the PATCH body set: an unset field stays as it is.
	r.workspaceHit("UpdateFoundryDocumentSet", req.GetWorkspaceId(), req.GetSetId()+" name="+fmt.Sprint(req.Name != nil)+" description="+fmt.Sprint(req.Description != nil))
	return &foundryv1.FoundryDocumentSetResponse{}, nil
}

func (r *routeRecorder) UpdateFoundryOntologyDocumentSet(_ context.Context, req *foundryv1.UpdateFoundryOntologyDocumentSetRequest) (*foundryv1.FoundryDocumentSetResponse, error) {
	r.hit("UpdateFoundryOntologyDocumentSet", req, req.GetSetId()+" name="+fmt.Sprint(req.Name != nil)+" description="+fmt.Sprint(req.Description != nil))
	return &foundryv1.FoundryDocumentSetResponse{}, nil
}

func (r *routeRecorder) DeleteFoundryDocumentSet(_ context.Context, req *foundryv1.DeleteFoundryDocumentSetRequest) (*foundryv1.DeleteFoundryDocumentSetResponse, error) {
	r.workspaceHit("DeleteFoundryDocumentSet", req.GetWorkspaceId(), req.GetSetId())
	return &foundryv1.DeleteFoundryDocumentSetResponse{}, nil
}

func (r *routeRecorder) DeleteFoundryOntologyDocumentSet(_ context.Context, req *foundryv1.DeleteFoundryOntologyDocumentSetRequest) (*foundryv1.DeleteFoundryDocumentSetResponse, error) {
	r.hit("DeleteFoundryOntologyDocumentSet", req, req.GetSetId())
	return &foundryv1.DeleteFoundryDocumentSetResponse{}, nil
}

func (r *routeRecorder) GetFoundryDataLayout(_ context.Context, req *foundryv1.GetFoundryDataLayoutRequest) (*foundryv1.FoundryDataLayoutResponse, error) {
	r.workspaceHit("GetFoundryDataLayout", req.GetWorkspaceId(), "")
	return &foundryv1.FoundryDataLayoutResponse{}, nil
}

func (r *routeRecorder) PutFoundryDataLayout(_ context.Context, req *foundryv1.PutFoundryDataLayoutRequest) (*foundryv1.FoundryDataLayoutResponse, error) {
	r.workspaceHit("PutFoundryDataLayout", req.GetWorkspaceId(), fmt.Sprint(req.GetExpectedRevision(), " ", len(req.GetLayout().GetFields())))
	return &foundryv1.FoundryDataLayoutResponse{}, nil
}

func (r *routeRecorder) SettleFoundryReview(_ context.Context, req *foundryv1.SettleFoundryReviewRequest) (*foundryv1.FoundryReviewDecisionResponse, error) {
	r.hit("SettleFoundryReview", req, req.GetReviewId())
	return &foundryv1.FoundryReviewDecisionResponse{}, nil
}

func (r *routeRecorder) ListFoundryReviewDecisions(_ context.Context, req *foundryv1.ListFoundryReviewDecisionsRequest) (*foundryv1.ListFoundryReviewDecisionsResponse, error) {
	r.hit("ListFoundryReviewDecisions", req, req.GetPageToken())
	return &foundryv1.ListFoundryReviewDecisionsResponse{}, nil
}

func (r *routeRecorder) DeleteFoundryReviewDecision(_ context.Context, req *foundryv1.DeleteFoundryReviewDecisionRequest) (*foundryv1.DeleteFoundryReviewDecisionResponse, error) {
	r.hit("DeleteFoundryReviewDecision", req, req.GetId())
	return &foundryv1.DeleteFoundryReviewDecisionResponse{}, nil
}

func (r *routeRecorder) CreateFoundryLoad(_ context.Context, req *foundryv1.CreateFoundryLoadRequest) (*foundryv1.FoundryLoadResponse, error) {
	r.hit("CreateFoundryLoad", req, req.GetVersionId())
	return &foundryv1.FoundryLoadResponse{}, nil
}

func (r *routeRecorder) ListFoundryLoads(_ context.Context, req *foundryv1.ListFoundryLoadsRequest) (*foundryv1.ListFoundryLoadsResponse, error) {
	r.hit("ListFoundryLoads", req, "")
	return &foundryv1.ListFoundryLoadsResponse{}, nil
}

func (r *routeRecorder) SetCurrentFoundryLoad(_ context.Context, req *foundryv1.SetCurrentFoundryLoadRequest) (*foundryv1.FoundryLoadResponse, error) {
	r.hit("SetCurrentFoundryLoad", req, req.GetId())
	return &foundryv1.FoundryLoadResponse{}, nil
}

func (r *routeRecorder) DeleteFoundryLoad(_ context.Context, req *foundryv1.DeleteFoundryLoadRequest) (*foundryv1.FoundryLoadResponse, error) {
	r.hit("DeleteFoundryLoad", req, req.GetId())
	return &foundryv1.FoundryLoadResponse{}, nil
}

func (r *routeRecorder) GetFoundryLoad(_ context.Context, req *foundryv1.GetFoundryLoadRequest) (*foundryv1.FoundryLoadResponse, error) {
	r.hit("GetFoundryLoad", req, req.GetId())
	return &foundryv1.FoundryLoadResponse{}, nil
}

func (r *routeRecorder) ReadFoundryGraph(_ context.Context, req *foundryv1.ReadFoundryGraphRequest) (*foundryv1.ReadFoundryGraphResponse, error) {
	r.hit("ReadFoundryGraph", req, req.GetLoadId()+" "+req.GetReadsJson())
	return &foundryv1.ReadFoundryGraphResponse{}, nil
}

func (r *routeRecorder) AskFoundryQuestion(_ context.Context, req *foundryv1.AskFoundryQuestionRequest) (*foundryv1.FoundryQuestionResponse, error) {
	r.hit("AskFoundryQuestion", req, req.GetLoadId()+"/"+req.GetThreadId()+"/"+req.GetQuestion())
	return &foundryv1.FoundryQuestionResponse{}, nil
}

func (r *routeRecorder) GetFoundryQuestion(_ context.Context, req *foundryv1.GetFoundryQuestionRequest) (*foundryv1.FoundryQuestionResponse, error) {
	r.hit("GetFoundryQuestion", req, req.GetId())
	return &foundryv1.FoundryQuestionResponse{}, nil
}

func (r *routeRecorder) ListFoundryQuestions(_ context.Context, req *foundryv1.ListFoundryQuestionsRequest) (*foundryv1.ListFoundryQuestionsResponse, error) {
	r.hit("ListFoundryQuestions", req, req.GetThreadId()+"/"+req.GetPageToken())
	return &foundryv1.ListFoundryQuestionsResponse{}, nil
}

func (r *routeRecorder) ListFoundryQuestionEvents(_ context.Context, req *foundryv1.ListFoundryQuestionEventsRequest) (*foundryv1.ListFoundryQuestionEventsResponse, error) {
	r.hit("ListFoundryQuestionEvents", req, req.GetId()+"/"+strconv.Itoa(int(req.GetAfterSeq())))
	return &foundryv1.ListFoundryQuestionEventsResponse{}, nil
}

func (r *routeRecorder) CancelFoundryQuestion(_ context.Context, req *foundryv1.CancelFoundryQuestionRequest) (*foundryv1.FoundryQuestionResponse, error) {
	r.hit("CancelFoundryQuestion", req, req.GetId())
	return &foundryv1.FoundryQuestionResponse{}, nil
}

func (r *routeRecorder) StopFoundryLoadEnrichment(_ context.Context, req *foundryv1.StopFoundryLoadEnrichmentRequest) (*foundryv1.FoundryLoadResponse, error) {
	r.hit("StopFoundryLoadEnrichment", req, req.GetId())
	return &foundryv1.FoundryLoadResponse{}, nil
}

func (r *routeRecorder) CreateFoundryEventSourceToken(_ context.Context, req *foundryv1.CreateFoundryEventSourceTokenRequest) (*foundryv1.CreateFoundryEventSourceTokenResponse, error) {
	r.workspaceHit("CreateFoundryEventSourceToken", req.GetWorkspaceId(), req.GetConnectorId())
	return &foundryv1.CreateFoundryEventSourceTokenResponse{}, nil
}

func (r *routeRecorder) ListFoundryEventSourceTokens(_ context.Context, req *foundryv1.ListFoundryEventSourceTokensRequest) (*foundryv1.ListFoundryEventSourceTokensResponse, error) {
	r.workspaceHit("ListFoundryEventSourceTokens", req.GetWorkspaceId(), req.GetConnectorId())
	return &foundryv1.ListFoundryEventSourceTokensResponse{}, nil
}

func (r *routeRecorder) RevokeFoundryEventSourceToken(_ context.Context, req *foundryv1.RevokeFoundryEventSourceTokenRequest) (*foundryv1.FoundryEventSourceTokenResponse, error) {
	r.workspaceHit("RevokeFoundryEventSourceToken", req.GetWorkspaceId(), req.GetConnectorId()+"/"+req.GetId())
	return &foundryv1.FoundryEventSourceTokenResponse{}, nil
}

func (r *routeRecorder) ListFoundryEventBatches(_ context.Context, req *foundryv1.ListFoundryEventBatchesRequest) (*foundryv1.ListFoundryEventBatchesResponse, error) {
	r.workspaceHit("ListFoundryEventBatches", req.GetWorkspaceId(), req.GetConnectorId()+"/"+strconv.Itoa(int(req.GetLimit())))
	return &foundryv1.ListFoundryEventBatchesResponse{}, nil
}

func (r *routeRecorder) GetFoundryUsage(_ context.Context, req *foundryv1.GetFoundryUsageRequest) (*foundryv1.FoundryUsageResponse, error) {
	r.got = "GetFoundryUsage " + req.GetWorkspaceId()
	return &foundryv1.FoundryUsageResponse{}, nil
}

func (r *routeRecorder) GetFoundryEnrichment(_ context.Context, req *foundryv1.GetFoundryEnrichmentRequest) (*foundryv1.FoundryEnrichmentResponse, error) {
	r.got = "GetFoundryEnrichment " + req.GetWorkspaceId()
	return &foundryv1.FoundryEnrichmentResponse{}, nil
}

func (r *routeRecorder) UpdateFoundryEnrichment(_ context.Context, req *foundryv1.UpdateFoundryEnrichmentRequest) (*foundryv1.FoundryEnrichmentResponse, error) {
	r.got = fmt.Sprintf("UpdateFoundryEnrichment %s %t %d %s", req.GetWorkspaceId(), req.GetEnabled(), req.GetMonthlyCreditCap(), req.GetNoticeVersion())
	return &foundryv1.FoundryEnrichmentResponse{}, nil
}

func (r *routeRecorder) GetFoundryAI(_ context.Context, req *foundryv1.GetFoundryAIRequest) (*foundryv1.FoundryAIResponse, error) {
	r.got = "GetFoundryAI " + req.GetWorkspaceId()
	return &foundryv1.FoundryAIResponse{}, nil
}

func (r *routeRecorder) UpdateFoundryAI(_ context.Context, req *foundryv1.UpdateFoundryAIRequest) (*foundryv1.FoundryAIResponse, error) {
	choice := "use_default"
	if p := req.GetProvider(); p != nil {
		choice = fmt.Sprintf("%T", p.GetKind())
	}
	r.got = fmt.Sprintf("UpdateFoundryAI %s %s %s %s", req.GetWorkspaceId(), choice, req.GetModels().GetDraft().GetName(), req.GetApiKey())
	return &foundryv1.FoundryAIResponse{}, nil
}

// The website calls these paths; each must reach its RPC with the ids in the
// path, the verb routes (:generate, :set-current, :complete, :read, :cancel,
// :move, :content, :bytes, :stop-enrichment, :revoke) included. A workspace
// document or event source route names no ontology; the ontology-scoped
// aliases reach their own RPCs. A generation's event source picks arrive as
// the site sends them: camelCase, with the mode by its enum name.
func TestFoundryRoutes(t *testing.T) {
	recorder := &routeRecorder{}
	mux := runtime.NewServeMux()
	if err := foundryv1.RegisterFoundryServiceHandlerServer(context.Background(), mux, recorder); err != nil {
		t.Fatal(err)
	}
	const w = "/v1/workspaces/ws-1/foundry"
	const o = w + "/ontologies"
	for _, tc := range []struct{ method, path, body, want string }{
		{http.MethodPost, o, `{"name":"n","tenantId":"t-1","clientToken":"c"}`, "CreateFoundryOntology ws-1 t-1"},
		{http.MethodGet, o + "?pageToken=p", "", "ListFoundryOntologies ws-1 p"},
		{http.MethodGet, o + "/ont-1", "", "GetFoundryOntology ws-1 ont-1 "},
		{http.MethodDelete, o + "/ont-1", "", "DeleteFoundryOntology ws-1 ont-1 "},
		{http.MethodGet, o + "/ont-1/current", "", "GetCurrentOntologyVersion ws-1 ont-1 "},
		{http.MethodGet, o + "/ont-1/versions/v-1", "", "GetOntologyVersion ws-1 ont-1 v-1"},
		{http.MethodPost, o + "/ont-1:set-current", `{"versionId":"v-1"}`, "SetCurrentOntologyVersion ws-1 ont-1 v-1"},
		{http.MethodPost, o + "/ont-1:generate", `{"goal":"g"}`, "GenerateOntology ws-1 ont-1 g"},
		{http.MethodPost, o + "/ont-1:generate", `{"goal":"g","noConnectors":true,"eventSources":[{"connectorId":"c-1","mode":"FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED"}]}`,
			"GenerateOntology ws-1 ont-1 g c-1:FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED"},
		{http.MethodGet, o + "/ont-1/generations/g-1", "", "GetOntologyGeneration ws-1 ont-1 g-1"},
		{http.MethodPost, o + "/ont-1/reviews/r-1:settle", `{"decision":{"resolve":{"notInData":true}}}`, "SettleFoundryReview ws-1 ont-1 r-1"},
		{http.MethodGet, o + "/ont-1/review-decisions?pageToken=d-1", "", "ListFoundryReviewDecisions ws-1 ont-1 d-1"},
		{http.MethodDelete, o + "/ont-1/review-decisions/d-1", "", "DeleteFoundryReviewDecision ws-1 ont-1 d-1"},
		{http.MethodPost, w + "/documents", `{"name":"a.md","setId":"s-1"}`, "CreateFoundryDocument ws-1 - a.md in s-1"},
		{http.MethodGet, w + "/documents?setId=s-1&pageToken=p", "", "ListFoundryDocuments ws-1 - s-1/p/"},
		{http.MethodGet, w + "/documents?ids=d-2&ids=d-1", "", "ListFoundryDocuments ws-1 - //d-2,d-1"},
		{http.MethodGet, w + "/documents/d-1", "", "GetFoundryDocument ws-1 - d-1"},
		{http.MethodGet, w + "/documents/d-1:content", "", "GetFoundryDocumentContent ws-1 - d-1"},
		{http.MethodGet, w + "/documents/d-1:bytes", "", "GetFoundryDocumentBytes ws-1 - d-1"},
		{http.MethodPatch, w + "/documents/d-1", `{"name":"b.md"}`, "UpdateFoundryDocument ws-1 - d-1 b.md"},
		{http.MethodDelete, w + "/documents/d-1", "", "DeleteFoundryDocument ws-1 - d-1"},
		{http.MethodPost, w + "/documents/d-1:complete", `{}`, "CompleteFoundryDocumentUpload ws-1 - d-1"},
		{http.MethodPost, w + "/documents/d-1:move", `{"setId":"s-2"}`, "MoveFoundryDocument ws-1 - d-1>s-2"},
		{http.MethodPost, w + "/document-sets", `{"name":"Contracts","description":"signed"}`, "CreateFoundryDocumentSet ws-1 - Contracts/signed"},
		{http.MethodGet, w + "/document-sets", "", "ListFoundryDocumentSets ws-1 - "},
		{http.MethodPatch, w + "/document-sets/s-1", `{"name":"Agreements"}`, "UpdateFoundryDocumentSet ws-1 - s-1 name=true description=false"},
		{http.MethodPatch, w + "/document-sets/s-1", `{"description":""}`, "UpdateFoundryDocumentSet ws-1 - s-1 name=false description=true"},
		{http.MethodDelete, w + "/document-sets/s-1", "", "DeleteFoundryDocumentSet ws-1 - s-1"},
		{http.MethodGet, w + "/data-layout", "", "GetFoundryDataLayout ws-1 - "},
		{http.MethodPost, w + "/connectors/c-1/tokens", `{}`, "CreateFoundryEventSourceToken ws-1 - c-1"},
		{http.MethodGet, w + "/connectors/c-1/tokens", "", "ListFoundryEventSourceTokens ws-1 - c-1"},
		{http.MethodPost, w + "/connectors/c-1/tokens/t-1:revoke", `{}`, "RevokeFoundryEventSourceToken ws-1 - c-1/t-1"},
		{http.MethodGet, w + "/connectors/c-1/event-batches?limit=5", "", "ListFoundryEventBatches ws-1 - c-1/5"},
		{http.MethodPut, w + "/data-layout", `{"layout":{"v":1},"expectedRevision":"3"}`, "PutFoundryDataLayout ws-1 - 3 1"},
		{http.MethodGet, o + "/ont-1/documents?status=review&pageToken=p", "", "ListFoundryOntologyDocuments ws-1 ont-1 review/p"},
		{http.MethodGet, o + "/ont-1/documents/d-1", "", "GetFoundryOntologyDocument ws-1 ont-1 d-1"},
		{http.MethodPost, o + "/ont-1/documents", `{"name":"a.md"}`, "CreateFoundryOntologyDocument ws-1 ont-1 a.md"},
		{http.MethodPost, o + "/ont-1/documents/d-1:complete", `{}`, "CompleteFoundryOntologyDocumentUpload ws-1 ont-1 d-1"},
		{http.MethodDelete, o + "/ont-1/documents/d-1", "", "DeleteFoundryOntologyDocument ws-1 ont-1 d-1"},
		{http.MethodPost, o + "/ont-1/documents/d-1:move", `{"setId":"s-2"}`, "MoveFoundryOntologyDocument ws-1 ont-1 d-1>s-2"},
		{http.MethodPost, o + "/ont-1/document-sets", `{"name":"Contracts","description":"signed"}`, "CreateFoundryOntologyDocumentSet ws-1 ont-1 Contracts/signed"},
		{http.MethodGet, o + "/ont-1/document-sets", "", "ListFoundryOntologyDocumentSets ws-1 ont-1 "},
		{http.MethodPatch, o + "/ont-1/document-sets/s-1", `{"name":"Agreements"}`, "UpdateFoundryOntologyDocumentSet ws-1 ont-1 s-1 name=true description=false"},
		{http.MethodDelete, o + "/ont-1/document-sets/s-1", "", "DeleteFoundryOntologyDocumentSet ws-1 ont-1 s-1"},
		{http.MethodPost, o + "/ont-1/loads", `{"versionId":"v-1","clientToken":"c"}`, "CreateFoundryLoad ws-1 ont-1 v-1"},
		{http.MethodGet, o + "/ont-1/loads", "", "ListFoundryLoads ws-1 ont-1 "},
		{http.MethodGet, o + "/ont-1/loads/l-1", "", "GetFoundryLoad ws-1 ont-1 l-1"},
		{http.MethodPost, o + "/ont-1/loads/l-1:set-current", `{}`, "SetCurrentFoundryLoad ws-1 ont-1 l-1"},
		{http.MethodDelete, o + "/ont-1/loads/l-1", "", "DeleteFoundryLoad ws-1 ont-1 l-1"},
		{http.MethodPost, o + "/ont-1/loads/l-1:read", `{"readsJson":"[]"}`, "ReadFoundryGraph ws-1 ont-1 l-1 []"},
		{http.MethodPost, o + "/ont-1/questions", `{"loadId":"l-1","threadId":"t-1","question":"who?","clientToken":"c"}`,
			"AskFoundryQuestion ws-1 ont-1 l-1/t-1/who?"},
		{http.MethodGet, o + "/ont-1/questions/q-1", "", "GetFoundryQuestion ws-1 ont-1 q-1"},
		{http.MethodGet, o + "/ont-1/questions?threadId=t-1&pageToken=p", "", "ListFoundryQuestions ws-1 ont-1 t-1/p"},
		{http.MethodGet, o + "/ont-1/questions/q-1/events?afterSeq=7", "", "ListFoundryQuestionEvents ws-1 ont-1 q-1/7"},
		{http.MethodPost, o + "/ont-1/questions/q-1:cancel", `{}`, "CancelFoundryQuestion ws-1 ont-1 q-1"},
		{http.MethodPost, o + "/ont-1/loads/l-1:stop-enrichment", `{}`, "StopFoundryLoadEnrichment ws-1 ont-1 l-1"},
		{http.MethodGet, "/v1/workspaces/ws-1/foundry/usage", "", "GetFoundryUsage ws-1"},
		{http.MethodGet, "/v1/workspaces/ws-1/foundry/enrichment", "", "GetFoundryEnrichment ws-1"},
		{http.MethodPatch, "/v1/workspaces/ws-1/foundry/enrichment", `{"enabled":true,"monthlyCreditCap":"50000","noticeVersion":"2026-10"}`,
			"UpdateFoundryEnrichment ws-1 true 50000 2026-10"},
		{http.MethodGet, "/v1/workspaces/ws-1/foundry/ai", "", "GetFoundryAI ws-1"},
		{http.MethodPut, "/v1/workspaces/ws-1/foundry/ai", `{"provider":{"anthropic":{}},"models":{"draft":{"name":"claude-opus-5-5","table":{}}},"apiKey":"sk-ant-x"}`,
			"UpdateFoundryAI ws-1 *foundryv1.FoundryAIProvider_Anthropic claude-opus-5-5 sk-ant-x"},
		{http.MethodPut, "/v1/workspaces/ws-1/foundry/ai", `{"useDefault":{}}`, "UpdateFoundryAI ws-1 use_default  "},
	} {
		recorder.got = ""
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || recorder.got != tc.want {
			t.Fatalf("%s %s: status %d reached %q, want %q (%s)", tc.method, tc.path, rec.Code, recorder.got, tc.want, rec.Body)
		}
	}
}

// jsonServer answers the routes whose JSON the site reads closely: a
// checkout, the month's usage and a review's settle.
type jsonServer struct {
	foundryv1.UnimplementedFoundryServiceServer
	got proto.Message
}

func (s *jsonServer) CreateFoundryProCheckout(_ context.Context, req *foundryv1.CreateFoundryProCheckoutRequest) (
	*foundryv1.CreateFoundryProCheckoutResponse, error) {
	s.got = req
	return &foundryv1.CreateFoundryProCheckoutResponse{Plan: &foundryv1.FoundryPlan{Tier: foundryv1.FoundryTier_FOUNDRY_TIER_MAX}}, nil
}

// GetFoundryUsage answers a month of two tasks, one without a cap.
func (s *jsonServer) GetFoundryUsage(_ context.Context, req *foundryv1.GetFoundryUsageRequest) (*foundryv1.FoundryUsageResponse, error) {
	s.got = req
	capMicros := int64(15_000_000)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	return &foundryv1.FoundryUsageResponse{Usage: &foundryv1.FoundryUsage{
		MonthStart: timestamppb.New(start), ResetsAt: timestamppb.New(start.AddDate(0, 1, 0)), TotalUsdMicros: 4_250_000,
		Tasks: []*foundryv1.FoundryUsageEntry{
			{Task: foundryv1.FoundryUsageTask_FOUNDRY_USAGE_TASK_LOADS, SpentUsdMicros: 4_000_000},
			{Task: foundryv1.FoundryUsageTask_FOUNDRY_USAGE_TASK_QUESTIONS, SpentUsdMicros: 250_000, ReservedUsdMicros: 500_000, CapUsdMicros: &capMicros},
		},
	}}, nil
}

// SettleFoundryReview answers a prop choice with the value decided, as the store keeps it.
func (s *jsonServer) SettleFoundryReview(_ context.Context, req *foundryv1.SettleFoundryReviewRequest) (*foundryv1.FoundryReviewDecisionResponse, error) {
	s.got = req
	return &foundryv1.FoundryReviewDecisionResponse{Decision: &foundryv1.FoundryReviewDecision{Id: "decision", Kind: "resolve", Type: "Customer",
		Value: req.GetDecision().GetProp()}}, nil
}

func jsonMux(t *testing.T) (*runtime.ServeMux, *jsonServer) {
	t.Helper()
	server := &jsonServer{}
	mux := runtime.NewServeMux()
	if err := foundryv1.RegisterFoundryServiceHandlerServer(context.Background(), mux, server); err != nil {
		t.Fatal(err)
	}
	return mux, server
}

// The website names the tier by its proto JSON name; the service must receive it.
func TestFoundryCheckoutRouteTier(t *testing.T) {
	mux, server := jsonMux(t)
	body := `{"workspaceId":"ws-1","clientToken":"click-1","promoCode":"LAUNCH50","tier":"FOUNDRY_TIER_MAX"}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/foundry/plan:checkout", strings.NewReader(body)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"FOUNDRY_TIER_MAX"`) {
		t.Fatalf("checkout = %d %s", rec.Code, rec.Body)
	}
	got := server.got.(*foundryv1.CreateFoundryProCheckoutRequest)
	if got.GetTier() != foundryv1.FoundryTier_FOUNDRY_TIER_MAX || got.GetWorkspaceId() != "ws-1" ||
		got.GetClientToken() != "click-1" || got.GetPromoCode() != "LAUNCH50" {
		t.Fatalf("service got %+v", got)
	}
}

// The site reads the month as grpc-gateway's JSON: camelCase, the task by
// its enum name, int64 as strings, and a task without a cap has no cap.
func TestFoundryUsageRoute(t *testing.T) {
	mux, server := jsonMux(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/workspaces/ws-1/foundry/usage", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("usage = %d %s", rec.Code, rec.Body)
	}
	if got := server.got.(*foundryv1.GetFoundryUsageRequest).GetWorkspaceId(); got != "ws-1" {
		t.Fatalf("service got workspace %q", got)
	}
	var body struct {
		Usage struct {
			MonthStart, ResetsAt, TotalUsdMicros string
			Tasks                                []map[string]any
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	u := body.Usage
	if u.MonthStart != "2026-10-01T00:00:00Z" || u.ResetsAt != "2026-11-01T00:00:00Z" || u.TotalUsdMicros != "4250000" || len(u.Tasks) != 2 {
		t.Fatalf("usage = %s", rec.Body)
	}
	loads, questions := u.Tasks[0], u.Tasks[1]
	if _, ok := loads["capUsdMicros"]; ok || loads["task"] != "FOUNDRY_USAGE_TASK_LOADS" || loads["spentUsdMicros"] != "4000000" {
		t.Fatalf("loads = %v", loads)
	}
	if questions["task"] != "FOUNDRY_USAGE_TASK_QUESTIONS" || questions["spentUsdMicros"] != "250000" ||
		questions["reservedUsdMicros"] != "500000" || questions["capUsdMicros"] != "15000000" {
		t.Fatalf("questions = %v", questions)
	}
}

// The website sends an integer choice as int64's JSON string (a JS number
// rounds past 2^53); the service must get, and the answer must give back,
// every digit.
func TestFoundryReviewSettleIntegerRoute(t *testing.T) {
	mux, server := jsonMux(t)
	for _, tc := range []struct {
		prop, answer string
		want         *foundryv1.FoundryPropValue
	}{
		{`{"integer":"9223372036854775807"}`, `{"integer":"9223372036854775807"}`, &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Integer{Integer: math.MaxInt64}}},
		{`{"integer":"9007199254740993"}`, `{"integer":"9007199254740993"}`, &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Integer{Integer: 9007199254740993}}},
		{`{"integer":9007199254740993}`, `{"integer":"9007199254740993"}`, &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Integer{Integer: 9007199254740993}}},
		{`{"number":2.5}`, `{"number":2.5}`, &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Number{Number: 2.5}}},
		{`{"text":"Chosen"}`, `{"text":"Chosen"}`, &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Text{Text: "Chosen"}}},
		{`{"boolean":false}`, `{"boolean":false}`, &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Boolean{}}},
		{`{"null":null}`, `{"null":null}`, &foundryv1.FoundryPropValue{Value: &foundryv1.FoundryPropValue_Null{}}},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws/foundry/ontologies/o/reviews/r:settle", strings.NewReader(`{"decision":{"prop":`+tc.prop+`}}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.prop, rec.Code, rec.Body)
		}
		if got := server.got.(*foundryv1.SettleFoundryReviewRequest).GetDecision().GetProp(); !proto.Equal(got, tc.want) {
			t.Errorf("%s: service got %v", tc.prop, got)
		}
		var answer struct {
			Decision struct {
				Value json.RawMessage `json:"value"`
			} `json:"decision"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, answer.Decision.Value); err != nil || compact.String() != tc.answer {
			t.Errorf("%s: answered %s", tc.prop, answer.Decision.Value)
		}
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws/foundry/ontologies/o/reviews/r:settle", strings.NewReader(`{"decision":{"prop":{"integer":"9223372036854775808"}}}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an integer past int64 = %d %s", rec.Code, rec.Body)
	}
}
