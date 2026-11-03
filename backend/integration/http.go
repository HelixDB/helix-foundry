package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"time"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// HTTPAskTool and HTTPGetQuestionTool name the two routes in a host's audit and metrics.
	HTTPAskTool         = "helix_foundry_http_ask"
	HTTPGetQuestionTool = "helix_foundry_http_get_question"

	// HTTPClientDisconnected is an outcome's code when the client's hang-up is what ended the
	// request, rather than a refusal it would have had anyway.
	HTTPClientDisconnected = "client_disconnected"

	httpRequestTimeout = 30 * time.Second
	// httpBodyTimeout is how long a client has to send a question's body, once the request is
	// resolved. It leaves most of the request's deadline for asking and waiting, so a slow client
	// is told so and never looks like a slow backend.
	httpBodyTimeout = 10 * time.Second
	httpMaxBody     = 64 << 10
)

// HTTPCall is what a host resolved for one request. Context carries trusted backend credentials;
// tool authorization is still performed by Identity.Authorize for each operation.
type HTTPCall struct {
	Context     context.Context
	Service     *Service
	Identity    *Identity
	WorkspaceID string
	// Done, when set, is told how the request ended once its response is written. A host returns
	// it alongside a refusal too, for one it means to audit.
	Done func(HTTPOutcome)
}

// HTTPOutcome is what a host's audit and metrics may keep of a request: never its question or
// bearer.
type HTTPOutcome struct {
	Tool, RequestID string
	// Status is the status sent, and Code the stable error code, empty below 400.
	Status int
	Code   string
	// QuestionID is the question the request asked or read. It is set even when the request then
	// failed: a client that hung up during the wait leaves a question running and spending.
	QuestionID string
}

// ResolveHTTP authenticates the request and resolves the ontology's workspace.
type ResolveHTTP func(*http.Request, string) (HTTPCall, error)

type httpRequestIDKey struct{}

// HTTPRequestID is the response's request ID, also used by downstream audit logs.
func HTTPRequestID(ctx context.Context) string {
	value, _ := ctx.Value(httpRequestIDKey{}).(string)
	return value
}

func NewHTTPHandler(resolve ResolveHTTP) http.Handler {
	return newHTTPHandler(resolve, httpBodyTimeout)
}

func newHTTPHandler(resolve ResolveHTTP, bodyTimeout time.Duration) http.Handler {
	mux := http.NewServeMux()
	route := func(tool string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { serveHTTP(w, r, tool, resolve, bodyTimeout) }
	}
	mux.HandleFunc("POST /v1/ontologies/{ontologyId}/ask", route(HTTPAskTool))
	mux.HandleFunc("GET /v1/ontologies/{ontologyId}/questions/{questionId}", route(HTTPGetQuestionTool))
	return mux
}

func serveHTTP(w http.ResponseWriter, r *http.Request, tool string, resolve ResolveHTTP, bodyTimeout time.Duration) {
	// The GET route also matches HEAD, which would read no question and look pending.
	if r.Method == http.MethodHead {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	client := r.Context()
	requestID := id.New()
	ctx, cancel := context.WithTimeout(client, httpRequestTimeout)
	defer cancel()
	r = r.WithContext(context.WithValue(ctx, httpRequestIDKey{}, requestID))
	w.Header().Set("X-Request-ID", requestID)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	outcome := HTTPOutcome{Tool: tool, RequestID: requestID}
	// See readHTTPQuestion: a refusal must not wait on the body it never reads either. Without a
	// body, net/http is already watching for a hang-up, a read this deadline would fail.
	if tool == HTTPAskTool && r.ContentLength != 0 {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyTimeout))
	}
	// An id that can't be one is the caller's error, before anything looks it up.
	if !ValidIDs([]string{r.PathValue("ontologyId")}, r.PathValue("questionId")) {
		WriteHTTPError(w, status.Error(codes.InvalidArgument, "invalid ontology or question id"), requestID)
		return
	}
	resolved, err := resolve(r, r.PathValue("ontologyId"))
	if resolved.Done != nil {
		defer func() { resolved.Done(outcome) }()
	}
	if err != nil {
		outcome.Status, outcome.Code = writeHTTPError(w, err, requestID, client)
		return
	}
	started := time.Now()
	var out FoundryQuestionOutput
	call := &Call{}
	if tool == HTTPAskTool {
		out, err = askHTTP(w, r, resolved, call, bodyTimeout)
	} else {
		out, _, err = resolved.Service.GetFoundryQuestion(resolved.Context, resolved.Identity, call, GetFoundryQuestionInput{
			OntologyInput: OntologyInput{WorkspaceID: resolved.WorkspaceID, OntologyID: r.PathValue("ontologyId")}, QuestionID: r.PathValue("questionId"),
		})
	}
	outcome.QuestionID = call.QuestionID
	if err != nil {
		outcome.Status, outcome.Code = writeHTTPError(w, err, requestID, client)
	} else if out.Done {
		outcome.Status = http.StatusOK
	} else {
		outcome.Status = http.StatusAccepted
		w.Header().Set("Location", "/v1/ontologies/"+url.PathEscape(r.PathValue("ontologyId"))+"/questions/"+url.PathEscape(out.ID))
		w.Header().Set("Retry-After", "1")
	}
	if resolved.Service.Logger != nil {
		resolved.Service.Logger.Info("Foundry HTTP Ask", "request_id", requestID, "method", r.Method,
			"duration_ms", time.Since(started).Milliseconds(), "status", out.Status, "error_code", outcome.Code)
	}
	if err == nil {
		w.WriteHeader(outcome.Status)
		_ = json.NewEncoder(w).Encode(out)
	}
}

func askHTTP(w http.ResponseWriter, r *http.Request, resolved HTTPCall, call *Call, bodyTimeout time.Duration) (FoundryQuestionOutput, error) {
	question, err := readHTTPQuestion(w, r, bodyTimeout)
	if err != nil {
		return FoundryQuestionOutput{}, err
	}
	// gRPC can't marshal a string that isn't UTF-8, and headers may hold any byte above 0x7f.
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 64 || !utf8.ValidString(key) {
		return FoundryQuestionOutput{}, status.Error(codes.InvalidArgument, "Idempotency-Key is required, at most 64 bytes of UTF-8")
	}
	out, _, err := resolved.Service.AskFoundryQuestion(resolved.Context, resolved.Identity, call, AskFoundryInput{
		OntologyInput: OntologyInput{WorkspaceID: resolved.WorkspaceID, OntologyID: r.PathValue("ontologyId")}, Question: question, ClientToken: key,
	})
	return out, err
}

// readHTTPQuestion reads the body under a read deadline: a request's context doesn't bound reading
// its body, and a server's ReadHeaderTimeout stops at the headers, so a client could otherwise
// hold the request by sending a byte now and then. Nothing here clears the deadline: net/http does
// once the body ends, and until then the deadline also bounds the read of what a handler left
// of the body, which net/http makes before it writes the response, with no end of its own if
// the client stalls. A host whose writer has no read deadlines leaves all of this to its server's
// own timeouts.
func readHTTPQuestion(w http.ResponseWriter, r *http.Request, bodyTimeout time.Duration) (string, error) {
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		return "", status.Error(codes.InvalidArgument, "Content-Type must be application/json")
	}
	// Resolving took some of what was first set, and the time was no fault of the client's.
	deadline := time.Now().Add(bodyTimeout)
	if end, ok := r.Context().Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	_ = http.NewResponseController(w).SetReadDeadline(deadline)
	var input struct {
		Question string `json:"question"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, httpMaxBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return "", httpBodyError(r, err, "expected a JSON object containing question")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", httpBodyError(r, err, "expected exactly one JSON object")
	}
	return input.Question, nil
}

// httpBodyError tells a body the client didn't finish sending from one that is wrong. A failed read
// cancels the request's context, as a hang-up does, before the read returns.
func httpBodyError(r *http.Request, err error, invalid string) error {
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return errCodeRequestTimeout
	case r.Context().Err() != nil:
		return r.Context().Err()
	}
	return status.Error(codes.InvalidArgument, invalid)
}

// writeHTTPError answers with err and returns the status sent and the code to report. The code is
// the client's hang-up only where the hang-up caused the error: a refusal stays what it was.
func writeHTTPError(w http.ResponseWriter, err error, requestID string, client context.Context) (int, string) {
	statusCode, code := writeHTTPErrorCode(w, err, requestID)
	if client.Err() != nil && (code == string(errCodeInternal) || code == string(errCodeBackendUnavailable)) {
		code = HTTPClientDisconnected
	}
	return statusCode, code
}

// WriteHTTPError never exposes internal backend messages or authentication internals.
func WriteHTTPError(w http.ResponseWriter, err error, requestID string) {
	writeHTTPErrorCode(w, err, requestID)
}

// writeHTTPErrorCode is WriteHTTPError, returning the status sent and the stable code.
func writeHTTPErrorCode(w http.ResponseWriter, err error, requestID string) (int, string) {
	code, message, stable := ToolErrorDetails(err)
	statusCode := http.StatusInternalServerError
	if !stable {
		message = status.Convert(err).Message()
		switch errorCode(err) {
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
		case codes.AlreadyExists:
			code = "already_exists"
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
	case "request_timeout":
		statusCode, message = http.StatusRequestTimeout, "the request body was not received in time"
	case "unauthenticated":
		statusCode, message = http.StatusUnauthorized, "a valid bearer token is required"
		w.Header().Set("WWW-Authenticate", "Bearer")
	case "permission_denied":
		statusCode, message = http.StatusForbidden, "permission denied"
	case "not_found":
		statusCode, message = http.StatusNotFound, "resource not found"
	case "failed_precondition", "foundry_pro_required":
		statusCode = http.StatusPreconditionFailed
	case "already_exists":
		statusCode = http.StatusConflict
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
	WriteHTTPRefusal(w, statusCode, code, message, requestID)
	return statusCode, code
}

// WriteHTTPRefusal writes the contract's error as it is given, for a host's own refusal of a
// request, whose message says nothing of the backend.
func WriteHTTPRefusal(w http.ResponseWriter, statusCode int, code, message, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message, "request_id": requestID}})
}
