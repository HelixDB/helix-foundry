package helixgateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

// doer records the request it is sent and answers with response.
type doer struct {
	got      *http.Request
	body     []byte
	response *http.Response
	err      error
}

func (d *doer) Do(req *http.Request) (*http.Response, error) {
	d.got = req
	d.body, _ = io.ReadAll(req.Body)
	return d.response, d.err
}

func answer(code int, body string, length int64) *http.Response {
	return &http.Response{
		StatusCode: code, ContentLength: length, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(bytes.NewReader([]byte(body))),
	}
}

func TestClientQuery(t *testing.T) {
	// The control plane's key and the tenant replace whatever the caller sent; extra headers ride along.
	t.Run("sends the provisioner key, the tenant and the extra headers", func(t *testing.T) {
		d := &doer{response: answer(200, `{"ok":true}`, -1)}
		c := Client{HTTP: d, ProvisionerKey: "key", MaxResponseBytes: 64}
		extra := http.Header{"X-Helix-Require-Writer": {"true"}, "Authorization": {"Bearer theirs"}}
		resp, err := c.Query(context.Background(), "https://gw.example/v2/query", "tenant-1", []byte(`{"q":1}`), extra)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || string(resp.Body) != `{"ok":true}` || resp.ContentType != "application/json" {
			t.Fatalf("response = %+v", resp)
		}
		h := d.got.Header
		if h.Get("Authorization") != "Bearer key" || len(h.Values("Authorization")) != 1 {
			t.Fatalf("authorization = %q", h.Values("Authorization"))
		}
		if h.Get("X-Helix-Tenant-ID") != "tenant-1" || h.Get("X-Helix-Require-Writer") != "true" {
			t.Fatalf("headers = %v", h)
		}
		if d.got.Method != http.MethodPost || string(d.body) != `{"q":1}` {
			t.Fatalf("request = %s %s", d.got.Method, d.body)
		}
	})

	// A dedicated cluster's database has no tenant header.
	t.Run("sends no tenant header without a tenant", func(t *testing.T) {
		d := &doer{response: answer(200, `{}`, 2)}
		c := Client{HTTP: d, ProvisionerKey: "key", MaxResponseBytes: 64}
		if _, err := c.Query(context.Background(), "https://gw.example/v2/query", "", []byte(`{}`), nil); err != nil {
			t.Fatal(err)
		}
		if _, ok := d.got.Header["X-Helix-Tenant-Id"]; ok {
			t.Fatalf("tenant header sent: %v", d.got.Header)
		}
	})

	// A response past the cap is refused whether or not it declared its length.
	t.Run("refuses a response past the limit", func(t *testing.T) {
		for name, length := range map[string]int64{"declared": 10, "streamed": -1} {
			t.Run(name, func(t *testing.T) {
				d := &doer{response: answer(200, `0123456789`, length)}
				c := Client{HTTP: d, ProvisionerKey: "key", MaxResponseBytes: 9}
				_, err := c.Query(context.Background(), "https://gw.example/v2/query", "t", nil, nil)
				if !errors.Is(err, ErrResponseTooLarge) {
					t.Fatalf("err = %v", err)
				}
			})
		}
	})

	// A HelixDB that checks no key gets no Authorization, not even the caller's.
	t.Run("sends no authorization when keyless", func(t *testing.T) {
		d := &doer{response: answer(200, `{}`, 2)}
		c := Client{HTTP: d, Keyless: true, MaxResponseBytes: 64}
		extra := http.Header{"Authorization": {"Bearer theirs"}}
		if _, err := c.Query(context.Background(), "http://helix:8080/v2/query", "tenant-1", []byte(`{}`), extra); err != nil {
			t.Fatal(err)
		}
		if got := d.got.Header.Values("Authorization"); len(got) != 0 {
			t.Fatalf("authorization = %q", got)
		}
		if d.got.Header.Get("X-Helix-Tenant-ID") != "tenant-1" {
			t.Fatalf("headers = %v", d.got.Header)
		}
	})

	// Without a key (unless keyless), a client or a limit nothing is sent.
	t.Run("refuses to send unconfigured", func(t *testing.T) {
		for name, c := range map[string]Client{
			"no key":    {HTTP: &doer{}, MaxResponseBytes: 1},
			"no client": {ProvisionerKey: "k", MaxResponseBytes: 1},
			"no limit":  {HTTP: &doer{}, ProvisionerKey: "k"},
		} {
			if _, err := c.Query(context.Background(), "https://gw.example/v2/query", "t", nil, nil); err == nil {
				t.Fatalf("%s: sent", name)
			}
		}
	})

	t.Run("returns the transport's error", func(t *testing.T) {
		c := Client{HTTP: &doer{err: errors.New("dial")}, ProvisionerKey: "k", MaxResponseBytes: 1}
		if _, err := c.Query(context.Background(), "https://gw.example/v2/query", "t", nil, nil); err == nil || err.Error() != "dial" {
			t.Fatalf("err = %v", err)
		}
	})
}
