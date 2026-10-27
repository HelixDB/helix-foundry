package foundryevents

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/events"
)

// A body's read errors are answered by what went wrong.
func TestReadBody(t *testing.T) {
	capped := func(body string, limit int64) io.Reader {
		return http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(strings.NewReader(body)), limit)
	}
	for _, c := range []struct {
		name   string
		reader io.Reader
		status int
		code   string
	}{
		{"past the cap", capped(validBody, 10), http.StatusRequestEntityTooLarge, codePayloadTooLarge},
		{"past the cap after the value", capped(validBody+strings.Repeat(" ", 20), int64(len(validBody)+10)), http.StatusRequestEntityTooLarge, codePayloadTooLarge},
		{"past its deadline", io.MultiReader(strings.NewReader(`{"pieces":`), failing{&net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}}),
			http.StatusRequestTimeout, codeRequestTimeout},
		{"cut off", io.MultiReader(strings.NewReader(`{"pieces":`), failing{errors.New("connection reset")}), http.StatusBadRequest, codeInvalidArguments},
		{"cut off after the value", io.MultiReader(strings.NewReader(validBody), failing{errors.New("connection reset")}), http.StatusBadRequest, codeInvalidArguments},
		{"empty", strings.NewReader(""), http.StatusBadRequest, codeInvalidArguments},
		{"only space", strings.NewReader(" \n"), http.StatusBadRequest, codeInvalidArguments},
	} {
		t.Run("answers a body "+c.name, func(t *testing.T) {
			_, fail := readBody(c.reader)
			require.NotNil(t, fail)
			assert.Equal(t, c.status, fail.status)
			assert.Equal(t, c.code, fail.code)
			assert.NotEmpty(t, fail.message)
		})
	}

	t.Run("reads a send, keeping data as written", func(t *testing.T) {
		b, fail := readBody(strings.NewReader(`{"client_token":"t","pieces":[{"id":"o-1","occurred_at":"2026-01-02T03:04:05Z","data":{ "n" : 1.10 }}]}`))
		require.Nil(t, fail)
		assert.Equal(t, body{ClientToken: "t", Pieces: []bodyPiece{{ID: "o-1", OccurredAt: "2026-01-02T03:04:05Z", Data: json.RawMessage(`{ "n" : 1.10 }`)}}}, b)
	})
}

// failing is a reader that fails with err.
type failing struct{ err error }

func (f failing) Read([]byte) (int, error) { return 0, f.err }

// sendRequest takes only a send the backend's rules take, and forwards its data compacted.
func TestSendRequest(t *testing.T) {
	limits := events.Limits{MaxPieces: 3, MaxBytes: 100, MaxPieceBytes: 50}
	piece := bodyPiece{ID: "o-1", Data: json.RawMessage(`{"a": 1}`)}
	for _, c := range []struct {
		name  string
		token string
		ok    bool
	}{
		{"a UUID", clientToken, true},
		{"16 characters", strings.Repeat("a", 16), true},
		{"64 characters", strings.Repeat("~", 64), true},
		{"15 characters", strings.Repeat("a", 15), false},
		{"65 characters", strings.Repeat("a", 65), false},
		{"a space", "0123456789 abcdef", false},
		{"a tab", "0123456789\tabcdef", false},
		{"a non-ASCII character", "0123456789abcdeé", false},
		{"a DEL", "0123456789abcdef\x7f", false},
		{"none", "", false},
	} {
		t.Run("client_token of "+c.name, func(t *testing.T) {
			req, fail := sendRequest(body{ClientToken: c.token, Pieces: []bodyPiece{piece}}, "workspace-1", "source-1", limits)
			if c.ok {
				require.Nil(t, fail)
				assert.Equal(t, c.token, req.GetClientToken())
				return
			}
			require.NotNil(t, fail)
			assert.Equal(t, http.StatusBadRequest, fail.status)
			assert.Contains(t, fail.message, "client_token is required")
		})
	}

	t.Run("forwards each piece's data compacted, and its time", func(t *testing.T) {
		at := "2026-01-02T03:04:05.5+01:00"
		req, fail := sendRequest(body{ClientToken: clientToken, Pieces: []bodyPiece{
			{ID: "o-1", OccurredAt: at, Data: json.RawMessage("{\n  \"n\": 1.10,\n  \"s\": \"\\ud83d\\ude00 a b\"\n}")},
			{ID: "o-2", Data: json.RawMessage(`{}`)},
		}}, "workspace-1", "source-1", limits)
		require.Nil(t, fail)
		assert.Equal(t, "workspace-1", req.GetWorkspaceId())
		assert.Equal(t, "source-1", req.GetConnectorId())
		require.Len(t, req.GetPieces(), 2)
		assert.Equal(t, `{"n":1.10,"s":"\ud83d\ude00 a b"}`, req.GetPieces()[0].GetDataJson())
		assert.Equal(t, time.Date(2026, 1, 2, 2, 4, 5, 500_000_000, time.UTC), req.GetPieces()[0].GetOccurredAt().AsTime())
		assert.Equal(t, `{}`, req.GetPieces()[1].GetDataJson())
		assert.Nil(t, req.GetPieces()[1].GetOccurredAt())
	})

	t.Run("measures the limits on the data compacted", func(t *testing.T) {
		// 120 bytes as sent, 48 compacted: within max_piece_bytes.
		data := "{" + strings.Repeat(" ", 70) + `"a":"` + strings.Repeat("x", 40) + `"}`
		_, fail := sendRequest(body{ClientToken: clientToken, Pieces: []bodyPiece{{ID: "o-1", Data: json.RawMessage(data)}}}, "w", "s", limits)
		require.Nil(t, fail)
		_, fail = sendRequest(body{ClientToken: clientToken, Pieces: []bodyPiece{
			{ID: "o-1", Data: json.RawMessage(`{"a":"` + strings.Repeat("x", 40) + `"}`)},
			{ID: "o-2", Data: json.RawMessage(`{"a":"` + strings.Repeat("x", 40) + `"}`)},
			{ID: "o-3", Data: json.RawMessage(`{"a":"` + strings.Repeat("x", 40) + `"}`)},
		}}, "w", "s", limits)
		require.NotNil(t, fail, "144 bytes compacted, past max_send_bytes")
		assert.Zero(t, fail.piece)
	})
}

// A refusal's body is {"error": {...}}, with its headers.
func TestFailureWrite(t *testing.T) {
	for _, c := range []struct {
		name       string
		failure    failure
		body       string
		retryAfter string
		challenge  string
	}{
		{"a piece", failure{status: 400, code: codeInvalidArguments, message: "bad", piece: 2},
			`{"error":{"code":"invalid_arguments","message":"bad","piece":2}}`, "", ""},
		{"a reason", failure{status: 409, code: codeFailedPrecondition, message: "full", reason: "FOUNDRY_EVENT_SOURCE_FULL"},
			`{"error":{"code":"failed_precondition","message":"full","reason":"FOUNDRY_EVENT_SOURCE_FULL"}}`, "", ""},
		{"a wait rounded up", failure{status: 429, code: codeResourceExhausted, message: "later", retryAfter: 1200 * time.Millisecond},
			`{"error":{"code":"resource_exhausted","message":"later"}}`, "2", ""},
		{"a short wait", failure{status: 429, code: codeRateLimited, message: "later", retryAfter: time.Millisecond},
			`{"error":{"code":"rate_limited","message":"later"}}`, "1", ""},
		{"a challenge", failure{status: 401, code: codeInvalidToken, message: "no", challenge: `Bearer error="invalid_token"`},
			`{"error":{"code":"invalid_token","message":"no"}}`, "", `Bearer error="invalid_token"`},
		{"a cause, unsent", failure{status: 503, code: codeBackendUnavailable, message: "again", cause: errors.New("dial 10.0.0.1")},
			`{"error":{"code":"backend_unavailable","message":"again"}}`, "", ""},
	} {
		t.Run("writes "+c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c.failure.write(rec)
			assert.Equal(t, c.failure.status, rec.Code)
			assert.JSONEq(t, c.body, rec.Body.String())
			assert.Equal(t, c.retryAfter, rec.Header().Get("Retry-After"))
			assert.Equal(t, c.challenge, rec.Header().Get("WWW-Authenticate"))
		})
	}
}

// An events.Refusal names its piece; any other error is a 400 of the whole send.
func TestRefused(t *testing.T) {
	_, err := events.Validate([]events.Piece{{ID: "o-1", Data: []byte(`{}`)}, {ID: "o 2", Data: []byte(`{}`)}}, events.Limits{MaxPieces: 5, MaxBytes: 100, MaxPieceBytes: 100})
	require.Error(t, err)
	assert.Equal(t, 2, refused(err).piece)
	assert.Equal(t, 2, refused(fmt.Errorf("wrapped: %w", err)).piece)
	plain := refused(errors.New("plain"))
	assert.Equal(t, http.StatusBadRequest, plain.status)
	assert.Zero(t, plain.piece)
}
