package llm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The wires the Assistant speaks without a provider's SDK (Ollama's native
// API, Bedrock's Converse) send their calls here.

// retries is how many times a call is sent again after one a retry may pass,
// as the SDKs do (option.WithMaxRetries(2)); retryBackoff is the first wait,
// doubled for each one after.
const retries = 2

var retryBackoff = time.Second

// errorBodyBytes is the most of a refusal's body read for its message.
const errorBodyBytes = 4 << 10

// post sends body to url with header and returns the answer of a call that
// was accepted (200), for its caller to stream and close. A call that got no
// answer, or a 429 or 5xx, is sent again up to retries times; anything else,
// or the last attempt, is an *Error carrying the server's own words, which
// message reads from a refusal's body.
func post(ctx context.Context, client *http.Client, url string, header http.Header, body []byte, message func([]byte) string) (*http.Response, error) {
	var last *Error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(retryBackoff << (attempt - 1)):
			case <-ctx.Done():
				return nil, &Error{Kind: ErrUnavailable, Err: ctx.Err()}
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, &Error{Kind: ErrInvalid, Err: err}
		}
		req.Header = header.Clone()
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			if last = transportError(err); last.Kind != ErrUnavailable || ctx.Err() != nil {
				return nil, last
			}
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyBytes))
		_ = resp.Body.Close()
		words := strings.TrimSpace(message(raw))
		if words == "" {
			words = http.StatusText(resp.StatusCode)
		}
		last = statusError(resp.StatusCode, fmt.Errorf("%d: %s", resp.StatusCode, words))
		if !last.Retryable() {
			return nil, last
		}
	}
	return nil, last
}

// errBroken is a stream that ended before its answer did.
var errBroken = errors.New("the stream ended before its answer did")
