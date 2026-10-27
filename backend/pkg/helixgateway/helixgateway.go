// Package helixgateway sends queries to Helix databases through their
// gateway, as a control plane does: with the gateway's provisioner key and,
// for a tenant database, its tenant header.
package helixgateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// NewHTTPClient calls gateways directly: no proxy from the environment, and
// redirects are returned rather than followed.
func NewHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Doer sends HTTP requests; *http.Client is one.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// ErrResponseTooLarge is a gateway response past the client's limit.
var ErrResponseTooLarge = errors.New("helixgateway: the response exceeds the limit")

// Response is a gateway's answer to a query, read whole.
type Response struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

// Client sends queries to databases as the control plane: with the gateway
// provisioner key and, for a tenant, its tenant header.
type Client struct {
	HTTP Doer
	// ProvisionerKey is the gateway's bearer token. A client without one
	// refuses to send, unless it is Keyless.
	ProvisionerKey string
	// Keyless sends no Authorization when there is no ProvisionerKey, for a
	// HelixDB that checks no key (a standalone install's).
	Keyless bool
	// MaxResponseBytes caps a response; a larger one is ErrResponseTooLarge.
	MaxResponseBytes int64
}

// Query POSTs body to endpoint for tenantID (none for a dedicated cluster),
// adding extra headers, such as x-helix-require-writer, before its own.
func (c *Client) Query(ctx context.Context, endpoint, tenantID string, body []byte, extra http.Header) (Response, error) {
	if (c.ProvisionerKey == "" && !c.Keyless) || c.HTTP == nil || c.MaxResponseBytes <= 0 {
		return Response{}, fmt.Errorf("helixgateway: no provisioner key, HTTP client or response limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, err
	}
	for name, values := range extra {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Del("Authorization")
	if c.ProvisionerKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.ProvisionerKey)
	}
	if tenantID != "" {
		req.Header.Set("X-Helix-Tenant-ID", tenantID)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer func() { _ = resp.Body.Close() }() // best-effort: read whole below
	if resp.ContentLength > c.MaxResponseBytes {
		return Response{}, ErrResponseTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, c.MaxResponseBytes+1))
	if err != nil {
		return Response{}, err
	}
	if int64(len(data)) > c.MaxResponseBytes {
		return Response{}, ErrResponseTooLarge
	}
	return Response{StatusCode: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Body: data}, nil
}
