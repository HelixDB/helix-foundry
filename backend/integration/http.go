package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"golang.org/x/time/rate"
	"google.golang.org/grpc/codes"
)

// The routes' names in a host's audit and metrics.
const (
	HTTPListOntologiesTool = "helix_foundry_http_list_ontologies"
	HTTPGetOntologyTool    = "helix_foundry_http_get_ontology"
	HTTPGetSchemaTool      = "helix_foundry_http_get_schema"
	HTTPReadTool           = "helix_foundry_http_read"
	HTTPGetLoadTool        = "helix_foundry_http_get_load"
	HTTPListDocumentsTool  = "helix_foundry_http_list_documents"
	HTTPGetDocumentTool    = "helix_foundry_http_get_document"
	HTTPAskTool            = "helix_foundry_http_ask"
	HTTPListQuestionsTool  = "helix_foundry_http_list_questions"
	HTTPGetQuestionTool    = "helix_foundry_http_get_question"
)

const (
	// HTTPClientDisconnected is an outcome's code when the client's hang-up is what ended the
	// request, rather than a refusal it would have had anyway.
	HTTPClientDisconnected = "client_disconnected"

	httpRequestTimeout = 30 * time.Second
	// httpBodyTimeout is how long a client has to send a request's body, once the request is
	// resolved. It leaves most of the request's deadline for asking and waiting, so a slow client
	// is told so and never looks like a slow backend.
	httpBodyTimeout = 10 * time.Second
	httpMaxBody     = 64 << 10

	// httpCallerRate is the requests a second one caller makes on one replica, and
	// httpCallerBurst how many it may make at once after a pause. A host with one caller (a
	// local install) has no such limit.
	httpCallerRate  = 10
	httpCallerBurst = 20
	// httpCallerReads is how many graph reads one caller runs at once on one replica: half of
	// what the workspace may (foundryread.MaxReadsPerWorkspace), so one integration never takes
	// every read the workspace's own people make.
	httpCallerReads = 2
	// httpCallerCapacity bounds the callers tracked on one replica.
	httpCallerCapacity = 10_000
)

// HTTPCall is what a host resolved for one request. Context carries trusted backend credentials;
// tool authorization is still performed by Identity.Authorize for each operation.
type HTTPCall struct {
	Context     context.Context
	Service     *Service
	Identity    *Identity
	WorkspaceID string
	// Caller names who is asking, among the callers of this host, so that one caller's
	// Idempotency-Key can't meet another's (httpClientToken), and each caller has limits of its
	// own (httpCallerRate, httpCallerReads). A host with one caller leaves it empty.
	Caller string
	// Done, when set, is told how the request ended once its response is written. A host returns
	// it alongside a refusal too, for one it means to audit.
	Done func(HTTPOutcome)
}

// HTTPOutcome is what a host's audit and metrics may keep of a request: never its question,
// reads or bearer.
type HTTPOutcome struct {
	Tool, RequestID string
	// Status is the status sent, and Code the stable error code, empty below 400. A Status of 0
	// means the handler panicked before it answered, with no Code.
	Status int
	Code   string
	// QuestionID is the question the request asked or read. It is set even when the request then
	// failed or panicked: a client that hung up during the wait leaves a question running and
	// spending, and so does a crash.
	QuestionID string
}

// ResolveHTTP authenticates the request and resolves the workspace it reads. The string is the
// ontology a route names, and empty for a route of the caller's workspace (GET /v1/ontologies),
// which a host resolves to the caller's one workspace. It refuses a malformed ontology id
// (ValidIDs) once it has authenticated and before it looks anything up: a host's lookup may not
// be able to take one.
type ResolveHTTP func(*http.Request, string) (HTTPCall, error)

type httpRequestIDKey struct{}

// HTTPRequestID is the response's request ID, also used by downstream audit logs.
func HTTPRequestID(ctx context.Context) string {
	value, _ := ctx.Value(httpRequestIDKey{}).(string)
	return value
}

// httpRoute is one route of the API: its name in audit and metrics, the query parameters it
// takes (any other is refused, so a misspelt one is never silently ignored), and how it answers
// a resolved request. serve returns the body and, for a question still being answered, where to
// read it again (a 202).
type httpRoute struct {
	method, pattern, tool string
	query                 []string
	serve                 func(httpRequest) (body any, pending string, err error)
}

// httpRoutes is the API: every route reads, except ask, which asks. Each calls the Service
// method its MCP tool calls, with the same authorization, and answers with the same JSON.
var httpRoutes = []httpRoute{
	{method: http.MethodGet, pattern: "/v1/ontologies", tool: HTTPListOntologiesTool, query: []string{"page_token"},
		serve: func(q httpRequest) (any, string, error) {
			out, _, err := q.Service.ListFoundryOntologies(q.Context, q.Identity, q.call, ListFoundryOntologiesInput{
				WorkspaceID: q.WorkspaceID, PageToken: q.r.URL.Query().Get("page_token"),
			})
			return out, "", err
		}},
	{method: http.MethodGet, pattern: "/v1/ontologies/{ontologyId}", tool: HTTPGetOntologyTool,
		serve: func(q httpRequest) (any, string, error) {
			out, _, err := q.Service.GetFoundryOntology(q.Context, q.Identity, q.call, q.ontology())
			return out, "", err
		}},
	{method: http.MethodGet, pattern: "/v1/ontologies/{ontologyId}/schema", tool: HTTPGetSchemaTool,
		serve: func(q httpRequest) (any, string, error) {
			out, _, err := q.Service.GetFoundryContext(q.Context, q.Identity, q.call, q.ontology())
			return out, "", err
		}},
	{method: http.MethodPost, pattern: "/v1/ontologies/{ontologyId}/reads", tool: HTTPReadTool,
		serve: func(q httpRequest) (any, string, error) {
			var body httpReads
			if err := q.body(&body, "expected a JSON object containing reads"); err != nil {
				return nil, "", err
			}
			if len(body.Reads) == 0 {
				return nil, "", invalidHTTPRequest("reads must hold 1 to 20 reads")
			}
			out, _, err := q.Service.ReadFoundryGraph(q.Context, q.Identity, q.call, ReadFoundryGraphInput{
				OntologyInput: q.ontology(), LoadID: body.LoadID, Reads: body.Reads, ValidAt: body.ValidAt,
			})
			return out, "", err
		}},
	{method: http.MethodGet, pattern: "/v1/ontologies/{ontologyId}/loads/{loadId}", tool: HTTPGetLoadTool, query: []string{"wait"},
		serve: func(q httpRequest) (any, string, error) {
			wait, err := q.bool("wait")
			if err != nil {
				return nil, "", err
			}
			out, _, err := q.Service.GetFoundryLoad(q.Context, q.Identity, q.call, GetFoundryLoadInput{
				OntologyInput: q.ontology(), LoadID: q.r.PathValue("loadId"), Wait: wait,
			})
			return out, "", err
		}},
	{method: http.MethodGet, pattern: "/v1/ontologies/{ontologyId}/documents", tool: HTTPListDocumentsTool, query: []string{"set_id", "page_token"},
		serve: func(q httpRequest) (any, string, error) {
			out, _, err := q.Service.ListFoundryDocuments(q.Context, q.Identity, q.call, ListFoundryDocumentsInput{
				OntologyInput: q.ontology(), SetID: q.r.URL.Query().Get("set_id"), PageToken: q.r.URL.Query().Get("page_token"),
			})
			return out, "", err
		}},
	{method: http.MethodGet, pattern: "/v1/ontologies/{ontologyId}/documents/{documentId}", tool: HTTPGetDocumentTool,
		serve: func(q httpRequest) (any, string, error) {
			out, _, err := q.Service.GetFoundryDocumentText(q.Context, q.Identity, q.call, GetFoundryDocumentTextInput{
				OntologyInput: q.ontology(), DocumentID: q.r.PathValue("documentId"),
			})
			return out, "", err
		}},
	{method: http.MethodPost, pattern: "/v1/ontologies/{ontologyId}/ask", tool: HTTPAskTool,
		serve: func(q httpRequest) (any, string, error) {
			var body httpQuestion
			if err := q.body(&body, "expected a JSON object containing question"); err != nil {
				return nil, "", err
			}
			// gRPC can't marshal a string that isn't UTF-8, and headers may hold any byte above 0x7f.
			key := q.r.Header.Get("Idempotency-Key")
			if key == "" || len(key) > 64 || !utf8.ValidString(key) {
				return nil, "", invalidHTTPRequest("Idempotency-Key is required, at most 64 bytes of UTF-8")
			}
			out, _, err := q.Service.AskFoundryQuestion(q.Context, q.Identity, q.call, AskFoundryInput{
				OntologyInput: q.ontology(), Question: body.Question, ClientToken: httpClientToken(q.Caller, key), ValidAt: body.ValidAt,
			})
			return out, q.pending(out), err
		}},
	{method: http.MethodGet, pattern: "/v1/ontologies/{ontologyId}/questions", tool: HTTPListQuestionsTool, query: []string{"thread_id", "page_token"},
		serve: func(q httpRequest) (any, string, error) {
			out, _, err := q.Service.ListFoundryQuestions(q.Context, q.Identity, q.call, ListFoundryQuestionsInput{
				OntologyInput: q.ontology(), ThreadID: q.r.URL.Query().Get("thread_id"), PageToken: q.r.URL.Query().Get("page_token"),
			})
			return out, "", err
		}},
	{method: http.MethodGet, pattern: "/v1/ontologies/{ontologyId}/questions/{questionId}", tool: HTTPGetQuestionTool,
		serve: func(q httpRequest) (any, string, error) {
			out, _, err := q.Service.GetFoundryQuestion(q.Context, q.Identity, q.call, GetFoundryQuestionInput{
				OntologyInput: q.ontology(), QuestionID: q.r.PathValue("questionId"),
			})
			return out, q.pending(out), err
		}},
}

// httpRequest is one resolved request as a route serves it.
type httpRequest struct {
	HTTPCall
	w           http.ResponseWriter
	r           *http.Request
	call        *Call
	bodyTimeout time.Duration
}

func (q httpRequest) ontology() OntologyInput {
	return OntologyInput{WorkspaceID: q.WorkspaceID, OntologyID: q.r.PathValue("ontologyId")}
}

// pending is where a question not yet done is read again; empty once it is.
func (q httpRequest) pending(out FoundryQuestionOutput) string {
	if out.Done || out.ID == "" {
		return ""
	}
	return "/v1/ontologies/" + url.PathEscape(q.r.PathValue("ontologyId")) + "/questions/" + url.PathEscape(out.ID)
}

// bool reads a query parameter that is true, false or absent (false).
func (q httpRequest) bool(name string) (bool, error) {
	switch value := q.r.URL.Query().Get(name); value {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	}
	return false, invalidHTTPRequest(name + " must be true or false")
}

// httpReads is a read's body: 1 to 20 reads of read_request.json's items, and optionally the
// load to read (else the current one) and the day to read as of.
type httpReads struct {
	Reads   []map[string]any `json:"reads"`
	LoadID  string           `json:"load_id"`
	ValidAt string           `json:"valid_at"`
}

func NewHTTPHandler(resolve ResolveHTTP) http.Handler {
	return newHTTPHandler(resolve, httpBodyTimeout)
}

func newHTTPHandler(resolve ResolveHTTP, bodyTimeout time.Duration) http.Handler {
	mux := http.NewServeMux()
	limits := newHTTPCallerLimits()
	allowed := map[string][]string{}
	for _, route := range httpRoutes {
		mux.HandleFunc(route.method+" "+route.pattern, func(w http.ResponseWriter, r *http.Request) {
			serveHTTP(w, r, route, resolve, limits, bodyTimeout)
		})
		allowed[route.pattern] = append(allowed[route.pattern], route.method)
	}
	// The same paths by any other method, and whatever else reaches this handler, answer in the
	// contract's error shape, as net/http's own plain text wouldn't.
	for pattern, methods := range allowed {
		mux.HandleFunc(pattern, httpMethodNotAllowed(strings.Join(methods, ", ")))
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		refuseHTTPRoute(w, http.StatusNotFound, "not_found", "resource not found")
	})
	return mux
}

func httpMethodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Allow", allow)
		refuseHTTPRoute(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

// refuseHTTPRoute answers a request no route took, which has no request ID yet.
func refuseHTTPRoute(w http.ResponseWriter, statusCode int, code, message string) {
	requestID := id.New()
	w.Header().Set("X-Request-ID", requestID)
	WriteHTTPRefusal(w, statusCode, code, message, requestID)
}

func serveHTTP(w http.ResponseWriter, r *http.Request, route httpRoute, resolve ResolveHTTP, limits *httpCallerLimits, bodyTimeout time.Duration) {
	// A GET route also matches HEAD, which would read nothing and look pending.
	if r.Method == http.MethodHead {
		httpMethodNotAllowed(route.method)(w, r)
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
	outcome := HTTPOutcome{Tool: route.tool, RequestID: requestID}
	// See httpRequest.body: a refusal must not wait on the body it never reads either. Without a
	// body, net/http is already watching for a hang-up, a read this deadline would fail.
	if route.method == http.MethodPost && r.ContentLength != 0 {
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyTimeout))
	}
	call := &Call{}
	resolved, err := resolve(r, r.PathValue("ontologyId"))
	if resolved.Done != nil {
		// A panic skips everything between here and the end, so the ID is read as this runs.
		defer func() {
			outcome.QuestionID = call.QuestionID
			resolved.Done(outcome)
		}()
	}
	if err == nil {
		err = httpQuery(r, route.query)
	}
	if err == nil {
		var release func()
		release, err = limits.admit(resolved.Caller, route.tool == HTTPReadTool)
		if release != nil {
			defer release()
		}
	}
	if err != nil {
		outcome.Status, outcome.Code = writeHTTPError(w, err, requestID, client)
		return
	}
	started := time.Now()
	body, pending, err := route.serve(httpRequest{HTTPCall: resolved, w: w, r: r, call: call, bodyTimeout: bodyTimeout})
	switch {
	case err != nil:
		outcome.Status, outcome.Code = writeHTTPError(w, err, requestID, client)
	case pending != "":
		outcome.Status = http.StatusAccepted
		w.Header().Set("Location", pending)
		w.Header().Set("Retry-After", "1")
	default:
		outcome.Status = http.StatusOK
	}
	if resolved.Service.Logger != nil {
		resolved.Service.Logger.Info("Foundry HTTP", "request_id", requestID, "tool", route.tool, "method", r.Method,
			"duration_ms", time.Since(started).Milliseconds(), "status", outcome.Status, "error_code", outcome.Code)
	}
	if err == nil {
		w.WriteHeader(outcome.Status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

// httpQuery refuses a query parameter the route doesn't take, or one given twice.
func httpQuery(r *http.Request, names []string) error {
	for name, values := range r.URL.Query() {
		if !slices.Contains(names, name) {
			return invalidHTTPRequest("unknown query parameter " + strconv.Quote(name))
		}
		if len(values) > 1 {
			return invalidHTTPRequest("query parameter " + strconv.Quote(name) + " is given more than once")
		}
	}
	return nil
}

// httpCallerLimits are each caller's limits on one replica: httpCallerRate requests a second, and
// httpCallerReads graph reads at once. They are kept in memory, per replica, and stored nowhere;
// a replica tracks at most httpCallerCapacity callers, forgetting idle ones first.
type httpCallerLimits struct {
	mu      sync.Mutex
	callers map[string]*httpCallerLimit
}

type httpCallerLimit struct {
	requests *rate.Limiter
	reads    int
}

func newHTTPCallerLimits() *httpCallerLimits {
	return &httpCallerLimits{callers: map[string]*httpCallerLimit{}}
}

// admit takes one of the caller's requests, and one of its reads when read is true. It returns
// what gives the read back; nil when it took none.
func (l *httpCallerLimits) admit(caller string, read bool) (func(), error) {
	if caller == "" {
		return nil, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	limit, known := l.callers[caller]
	if !known {
		if len(l.callers) >= httpCallerCapacity {
			// An idle caller with every request back is forgotten as if it was never seen.
			maps.DeleteFunc(l.callers, func(_ string, c *httpCallerLimit) bool {
				return c.reads == 0 && c.requests.Tokens() >= httpCallerBurst
			})
		}
		limit = &httpCallerLimit{requests: rate.NewLimiter(httpCallerRate, httpCallerBurst)}
		if len(l.callers) < httpCallerCapacity {
			l.callers[caller] = limit
		}
	}
	if !limit.requests.Allow() {
		return nil, detailedToolError{code: errCodeResourceExhausted,
			message: "this credential is making more than " + strconv.Itoa(httpCallerRate) + " requests a second; slow down and retry"}
	}
	if !read {
		return nil, nil
	}
	if limit.reads >= httpCallerReads {
		return nil, detailedToolError{code: errCodeResourceExhausted,
			message: "this credential already runs " + strconv.Itoa(httpCallerReads) + " reads at once; wait for one to finish"}
	}
	limit.reads++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		limit.reads--
	}, nil
}

// httpClientToken is the client token of a caller's Idempotency-Key. A client token is unique per
// ontology, not per caller, so one caller's key would otherwise find another's question, billed to
// them, or be refused as used for one the caller never asked. Hashing it in the caller keeps those
// apart, and keeps a key from meeting an MCP client's token. It also leaves nothing of the key for the
// column's collation to fold: only the same bytes from the same caller are the same question.
func httpClientToken(caller, key string) string {
	sum := sha256.Sum256([]byte(caller + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

// invalidHTTPRequest is a refusal of the handler's own, whose message is for the client.
func invalidHTTPRequest(message string) error {
	return detailedToolError{code: errCodeInvalidArguments, message: message}
}

// body reads the request's body, one JSON object with no field into doesn't name, under a read
// deadline: a request's context doesn't bound reading its body, and a server's ReadHeaderTimeout
// stops at the headers, so a client could otherwise hold the request by sending a byte now and
// then. Nothing here clears the deadline: net/http does once the body ends, and until then the
// deadline also bounds the read of what a handler left of the body, which net/http makes before
// it writes the response, with no end of its own if the client stalls. A host whose writer has
// no read deadlines leaves all of this to its server's own timeouts.
func (q httpRequest) body(into any, invalid string) error {
	r := q.r
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" {
		return invalidHTTPRequest("Content-Type must be application/json")
	}
	// Resolving took some of what was first set, and the time was no fault of the client's.
	deadline := time.Now().Add(q.bodyTimeout)
	if end, ok := r.Context().Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	_ = http.NewResponseController(q.w).SetReadDeadline(deadline)
	decoder := json.NewDecoder(http.MaxBytesReader(q.w, r.Body, httpMaxBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return httpBodyError(r, err, invalid)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return httpBodyError(r, err, "expected exactly one JSON object")
	}
	return nil
}

// httpQuestion is an HTTP Ask's body: the question and, optionally, the day to answer it as of
// (YYYY-MM-DD, checked by the backend).
type httpQuestion struct {
	Question string `json:"question"`
	ValidAt  string `json:"valid_at"`
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
	return invalidHTTPRequest(invalid)
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

// WriteHTTPError never exposes internal backend messages or authentication internals. A message
// reaches the client only from a stable error: the handler's own, or one FoundryError let through.
// Any other is a code and its fixed message, since a host's refusal or an Authorize error may name
// internals. Every denial is not_found, as for the tools, so a caller can't probe for what exists.
func WriteHTTPError(w http.ResponseWriter, err error, requestID string) {
	writeHTTPErrorCode(w, err, requestID)
}

// writeHTTPErrorCode is WriteHTTPError, returning the status sent and the stable code.
func writeHTTPErrorCode(w http.ResponseWriter, err error, requestID string) (int, string) {
	code, message, stable := ToolErrorDetails(err)
	statusCode := http.StatusInternalServerError
	if !stable {
		switch errorCode(err) {
		case codes.InvalidArgument:
			code = "invalid_arguments"
		case codes.Unauthenticated:
			code = "unauthenticated"
		case codes.PermissionDenied, codes.NotFound:
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
	case "not_found":
		statusCode, message = http.StatusNotFound, "resource not found"
	case "failed_precondition", "foundry_pro_required", "account_needs_sign_in":
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
