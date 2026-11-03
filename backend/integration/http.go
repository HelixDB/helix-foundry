package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"time"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ResolveHTTP authenticates the request and resolves the ontology's workspace.
// The returned context carries trusted backend credentials; tool authorization is
// still performed by Identity.Authorize for each operation.
type ResolveHTTP func(*http.Request, string) (context.Context, *Service, *Identity, string, error)

type httpRequestIDKey struct{}

// HTTPRequestID is the response's request ID, also used by downstream audit logs.
func HTTPRequestID(ctx context.Context) string {
	value, _ := ctx.Value(httpRequestIDKey{}).(string)
	return value
}

func NewHTTPHandler(resolve ResolveHTTP) http.Handler {
	mux := http.NewServeMux()
	serve := func(w http.ResponseWriter, r *http.Request) {
		// The GET route also matches HEAD, which would read no question and look pending.
		if r.Method == http.MethodHead {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		requestID := id.New()
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		r = r.WithContext(context.WithValue(ctx, httpRequestIDKey{}, requestID))
		w.Header().Set("X-Request-ID", requestID)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		ctx, service, identity, workspaceID, err := resolve(r, r.PathValue("ontologyId"))
		if err != nil {
			WriteHTTPError(w, err, requestID)
			return
		}
		started := time.Now()
		var out FoundryQuestionOutput
		call := &Call{}
		switch r.Method {
		case http.MethodPost:
			mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if mediaErr != nil || mediaType != "application/json" {
				WriteHTTPError(w, status.Error(codes.InvalidArgument, "Content-Type must be application/json"), requestID)
				return
			}
			var input struct {
				Question string `json:"question"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&input); err != nil {
				WriteHTTPError(w, status.Error(codes.InvalidArgument, "expected a JSON object containing question"), requestID)
				return
			}
			var trailing any
			if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
				WriteHTTPError(w, status.Error(codes.InvalidArgument, "expected exactly one JSON object"), requestID)
				return
			}
			// gRPC can't marshal a string that isn't UTF-8, and headers may hold any byte above 0x7f.
			key := r.Header.Get("Idempotency-Key")
			if key == "" || len(key) > 64 || !utf8.ValidString(key) {
				WriteHTTPError(w, status.Error(codes.InvalidArgument, "Idempotency-Key is required, at most 64 bytes of UTF-8"), requestID)
				return
			}
			out, _, err = service.AskFoundryQuestion(ctx, identity, call, AskFoundryInput{
				OntologyInput: OntologyInput{WorkspaceID: workspaceID, OntologyID: r.PathValue("ontologyId")}, Question: input.Question, ClientToken: key,
			})
		case http.MethodGet:
			out, _, err = service.GetFoundryQuestion(ctx, identity, call, GetFoundryQuestionInput{
				OntologyInput: OntologyInput{WorkspaceID: workspaceID, OntologyID: r.PathValue("ontologyId")}, QuestionID: r.PathValue("questionId"),
			})
		}
		if service.Logger != nil {
			service.Logger.Info("Foundry HTTP Ask", "request_id", requestID, "method", r.Method,
				"duration_ms", time.Since(started).Milliseconds(), "status", out.Status, "error_code", httpErrorCode(err))
		}
		if err != nil {
			WriteHTTPError(w, err, requestID)
			return
		}
		if out.Done {
			w.WriteHeader(http.StatusOK)
		} else {
			w.Header().Set("Location", "/v1/ontologies/"+url.PathEscape(r.PathValue("ontologyId"))+"/questions/"+url.PathEscape(out.ID))
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusAccepted)
		}
		_ = json.NewEncoder(w).Encode(out)
	}
	mux.HandleFunc("POST /v1/ontologies/{ontologyId}/ask", serve)
	mux.HandleFunc("GET /v1/ontologies/{ontologyId}/questions/{questionId}", serve)
	return mux
}

func httpErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if code, _, ok := ToolErrorDetails(err); ok {
		return code
	}
	return status.Code(err).String()
}

// WriteHTTPError never exposes internal backend messages or authentication internals.
func WriteHTTPError(w http.ResponseWriter, err error, requestID string) {
	code, message, stable := ToolErrorDetails(err)
	statusCode := http.StatusInternalServerError
	if !stable {
		message = status.Convert(err).Message()
		switch status.Code(err) {
		case codes.InvalidArgument:
			code = "invalid_arguments"
		case codes.Unauthenticated:
			code = "unauthenticated"
		case codes.PermissionDenied:
			code = "permission_denied"
		case codes.NotFound:
			code = "not_found"
		case codes.FailedPrecondition:
			code = "failed_precondition"
		case codes.ResourceExhausted:
			code = "resource_exhausted"
		case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
			code = "backend_unavailable"
		default:
			code = "internal_error"
		}
	}
	switch code {
	case "invalid_arguments":
		statusCode = http.StatusBadRequest
	case "unauthenticated":
		statusCode, message = http.StatusUnauthorized, "a valid bearer token is required"
		w.Header().Set("WWW-Authenticate", "Bearer")
	case "permission_denied":
		statusCode, message = http.StatusForbidden, "permission denied"
	case "not_found":
		statusCode, message = http.StatusNotFound, "resource not found"
	case "failed_precondition", "foundry_pro_required", "data_not_ready":
		statusCode = http.StatusPreconditionFailed
	case "resource_exhausted":
		statusCode = http.StatusTooManyRequests
	case "backend_unavailable":
		statusCode, message = http.StatusServiceUnavailable, "Foundry is temporarily unavailable"
	default:
		message = "internal error"
	}
	if message == "" {
		message = code
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message, "request_id": requestID}})
}
