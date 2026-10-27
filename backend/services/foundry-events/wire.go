package foundryevents

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/events"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// body is a send as a request carries it. Its fields are matched as encoding/json does, and any
// other field is refused.
type body struct {
	ClientToken string      `json:"client_token"`
	Pieces      []bodyPiece `json:"pieces"`
}

// bodyPiece is one piece as sent: its data is kept as written, so its number literals and string
// escapes reach the backend as they were, only its spaces gone.
type bodyPiece struct {
	ID string `json:"id"`
	// OccurredAt is RFC 3339 text; empty or null for none.
	OccurredAt string          `json:"occurred_at"`
	Data       json.RawMessage `json:"data"`
}

// readBody decodes the one JSON value a request's body holds, from a reader the caller has
// capped. Nothing but space may follow it.
func readBody(r io.Reader) (body, *failure) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var b body
	if err := dec.Decode(&b); err != nil {
		return body{}, bodyFailure(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return body{}, invalid("the body holds more than one JSON value")
		}
		return body{}, bodyFailure(err)
	}
	return b, nil
}

// bodyFailure answers a body that couldn't be read or decoded: 413 past the cap, 408 when it
// took longer than its deadline, and 400 for anything else.
func bodyFailure(err error) *failure {
	if tooBig, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return tooLarge(tooBig.Limit)
	}
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return &failure{status: http.StatusRequestTimeout, code: codeRequestTimeout, message: "the body took too long to arrive; send it again"}
	case errors.Is(err, io.EOF):
		return invalid("the body is empty; it must be one JSON object: {\"client_token\": \"…\", \"pieces\": [{\"id\": \"…\", \"data\": {…}}]}")
	case errors.Is(err, io.ErrUnexpectedEOF):
		return invalid("the body ended before its JSON did")
	}
	return invalid("the body is not a send: " + strings.TrimPrefix(err.Error(), "json: "))
}

// tooLarge is a 413 for a body past limit bytes.
func tooLarge(limit int64) *failure {
	return &failure{status: http.StatusRequestEntityTooLarge, code: codePayloadTooLarge,
		message: fmt.Sprintf("a body holds at most %d bytes; send fewer pieces at once", limit)}
}

// validClientToken is 16 to 64 bytes, each printable ASCII other than space, as the backend
// checks it again: a UUID fits.
func validClientToken(token string) bool {
	return len(token) >= 16 && len(token) <= 64 && strings.IndexFunc(token, func(r rune) bool { return r < 0x21 || r > 0x7E }) < 0
}

// sendRequest checks b as the backend's stateless rules do, under limits, and makes the request
// forwarding it to the backend as sender's source: each piece's data compacted, so the request
// holds no more than max_send_bytes of data, and its time parsed. A refusal of one piece names it.
func sendRequest(b body, workspaceID, connectorID string, limits events.Limits) (*foundryv1.SendFoundryEventsRequest, *failure) {
	if !validClientToken(b.ClientToken) {
		return nil, invalid("client_token is required: 16 to 64 printable ASCII characters with no spaces, " +
			"a new random value (such as a UUID) for each send, and the same one to retry it")
	}
	pieces := make([]events.Piece, len(b.Pieces))
	for i, p := range b.Pieces {
		pieces[i] = events.Piece{ID: p.ID, Data: p.Data}
		if p.OccurredAt == "" {
			continue
		}
		at, err := events.ParseOccurredAt(i, p.ID, p.OccurredAt)
		if err != nil {
			return nil, refused(err)
		}
		pieces[i].OccurredAt = &at
	}
	send, err := events.Validate(pieces, limits)
	if err != nil {
		return nil, refused(err)
	}
	req := &foundryv1.SendFoundryEventsRequest{
		WorkspaceId: workspaceID, ConnectorId: connectorID, ClientToken: b.ClientToken,
		Pieces: make([]*foundryv1.FoundryEventPiece, len(pieces)),
	}
	for i, p := range pieces {
		req.Pieces[i] = &foundryv1.FoundryEventPiece{Id: p.ID, DataJson: string(send.Data(i))}
		if p.OccurredAt != nil {
			req.Pieces[i].OccurredAt = timestamppb.New(*p.OccurredAt)
		}
	}
	return req, nil
}

// refused answers an *events.Refusal: 400, naming its piece when it refuses one.
func refused(err error) *failure {
	refusal, ok := errors.AsType[*events.Refusal](err)
	if !ok {
		return invalid(err.Error())
	}
	return &failure{status: http.StatusBadRequest, code: codeInvalidArguments, message: refusal.Message, piece: refusal.Piece}
}

// Sent is a 200's body: the MCP's answer to helix_foundry_send_pieces, without its content_trust.
type Sent struct {
	EventSourceID string  `json:"event_source_id"`
	Seq           string  `json:"seq"`
	Pieces        int32   `json:"pieces"`
	ReceivedAt    string  `json:"received_at"`
	Replayed      bool    `json:"replayed"`
	NewFields     []Field `json:"new_fields"`
	// Capacity is how full the source's table will be once its waiting pieces are synced.
	Capacity Capacity `json:"capacity"`
	// Warning is set from 80% full.
	Warning string `json:"warning,omitempty"`
	Next    string `json:"next"`
}

// Field is a field a send added to its source, and the JSON type it locked.
type Field struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Capacity is a source's rows and bytes against its caps, the counts as strings of digits.
type Capacity struct {
	Rows        string `json:"rows"`
	MaxRows     string `json:"max_rows"`
	Bytes       string `json:"bytes"`
	MaxBytes    string `json:"max_bytes"`
	PercentFull int    `json:"percent_full"`
}

// next says when sent pieces are seen, as the MCP does.
const next = "Pieces reach the source's table at its next sync, about a minute away; ontologies that map it see them at their next refresh."

// sent is the 200's body of the backend's answer to a send to connectorID.
func sent(connectorID string, resp *foundryv1.SendFoundryEventsResponse) Sent {
	batch, c := resp.GetBatch(), resp.GetCapacity()
	out := Sent{
		EventSourceID: connectorID, Seq: strconv.FormatInt(batch.GetSeq(), 10), Pieces: batch.GetPieces(),
		Replayed: resp.GetReplayed(), NewFields: make([]Field, len(resp.GetNewFields())), Warning: resp.GetWarning(), Next: next,
		Capacity: Capacity{
			Rows: strconv.FormatInt(c.GetRows(), 10), MaxRows: strconv.FormatInt(c.GetMaxRows(), 10),
			Bytes: strconv.FormatInt(c.GetBytes(), 10), MaxBytes: strconv.FormatInt(c.GetMaxBytes(), 10),
		},
	}
	if at := batch.GetCreatedAt(); at.IsValid() {
		out.ReceivedAt = at.AsTime().UTC().Format(time.RFC3339Nano)
	}
	for i, f := range resp.GetNewFields() {
		out.NewFields[i] = Field{Name: f.GetName(), Type: f.GetType()}
	}
	if c.GetMaxRows() > 0 && c.GetMaxBytes() > 0 {
		out.Capacity.PercentFull = events.Capacity{Rows: c.GetRows(), Bytes: c.GetBytes(), Caps: events.Caps{MaxRows: c.GetMaxRows(), MaxBytes: c.GetMaxBytes()}}.Percent()
	}
	return out
}

// The codes of the error bodies. Those the MCP's send tool also answers have its names.
const (
	codeInvalidArguments     = "invalid_arguments"
	codeNotFound             = "not_found"
	codeFailedPrecondition   = "failed_precondition"
	codeResourceExhausted    = "resource_exhausted"
	codeBackendUnavailable   = "backend_unavailable"
	codeUnauthorized         = "unauthorized"
	codeInvalidToken         = "invalid_token"
	codeRateLimited          = "rate_limited"
	codePayloadTooLarge      = "payload_too_large"
	codeUnsupportedMediaType = "unsupported_media_type"
	codeRequestTimeout       = "request_timeout"
	codeInternal             = "internal"
	// codeOK labels a 200 in the requests metric; no body carries it.
	codeOK = "ok"
)

// failure is a request the door answers with an error body. Its message is written for the
// sender, and may quote the sender's ids; it never holds a token or an address.
type failure struct {
	status  int
	code    string
	message string
	// piece is the refused piece's 1-based place; 0 for none.
	piece int
	// reason is a 409's ErrorInfo reason, such as FOUNDRY_EVENT_SOURCE_FULL.
	reason string
	// retryAfter, when above 0, is sent as Retry-After in whole seconds, rounded up.
	retryAfter time.Duration
	// challenge, when set, is sent as WWW-Authenticate.
	challenge string
	// cause is the backend's error behind a 5xx, logged and never sent.
	cause error
	// quiet leaves the request out of the log: a refusal a flood is answered with, as often as
	// it asks.
	quiet bool
}

func invalid(message string) *failure {
	return &failure{status: http.StatusBadRequest, code: codeInvalidArguments, message: message}
}

// limited is a 429 of the door's own buckets, unlogged.
func limited(after time.Duration, message string) *failure {
	return &failure{status: http.StatusTooManyRequests, code: codeRateLimited, message: message, retryAfter: after, quiet: true}
}

// errorBody is every answer but a 200.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Piece   int    `json:"piece,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// write answers f on w.
func (f *failure) write(w http.ResponseWriter) {
	if f.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(f.retryAfter.Seconds()))))
	}
	if f.challenge != "" {
		w.Header().Set("WWW-Authenticate", f.challenge)
	}
	writeJSON(w, f.status, errorBody{Error: errorDetail{Code: f.code, Message: f.message, Piece: f.piece, Reason: f.reason}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // best-effort: the client may have gone
}
