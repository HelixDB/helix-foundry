package connector

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

const (
	testConnectorRole = "arn:aws:iam::111111111111:role/helix-test-foundry-connector"
	customerRole      = "arn:aws:iam::222222222222:role/helix-foundry"
	testBucket        = "acme-exports"
)

// fakeObject is one object the fake bucket holds.
type fakeObject struct {
	body         []byte
	etag         string
	storageClass string
	modified     time.Time
}

// fakeRequest is what the fake bucket saw of one request.
type fakeRequest struct {
	method, key, accessKey, rangeHeader, ifMatch string
	list                                         bool
}

// fakeS3 is a path-style S3 bucket over HTTP. Its hooks inject failures.
type fakeS3 struct {
	t       *testing.T
	url     string
	mu      sync.Mutex
	objects map[string]fakeObject
	// region answers HeadBucket; listRegion, when set, is the only region
	// whose signed listings succeed (others get a 301 naming it).
	region, listRegion string
	pageSize           int
	requests           []fakeRequest
	// fail answers a request instead of the bucket: status, error code.
	fail func(r *http.Request, key string) (int, string)
	// slow delays an object's body; stall holds a response's headers.
	slow, stall time.Duration
	// cut sends half an object's body that many times before a whole one.
	cut map[string]int
	// repeatToken hands out the same continuation token forever.
	repeatToken bool
	// inFlight and maxInFlight count object GETs being answered at once.
	inFlight, maxInFlight int
}

// externalIDRE is a workspace's external ID: a ULID after the prefix.
var externalIDRE = regexp.MustCompile(`^helix-foundry-[0-9A-HJKMNP-TV-Z]{26}$`)

var credentialRE = regexp.MustCompile(`Credential=([^/]+)/[^/]+/([^/]+)/`)

func newFakeS3(t *testing.T) *fakeS3 {
	f := &fakeS3{t: t, objects: map[string]fakeObject{}, region: "eu-west-2", pageSize: 1000, cut: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	f.url = server.URL
	return f
}

func (f *fakeS3) put(key, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = fakeObject{body: []byte(body), etag: fmt.Sprintf(`"%x"`, len(f.objects)+1), modified: time.Date(2026, 10, 1, 0, len(f.objects), 0, 0, time.UTC)}
}

func (f *fakeS3) seen() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

// gets counts the object GETs the bucket answered.
func (f *fakeS3) gets() int {
	n := 0
	for _, r := range f.seen() {
		if r.method == http.MethodGet && !r.list {
			n++
		}
	}
	return n
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	key, _ = url.PathUnescape(key)
	accessKey, region := "", ""
	if m := credentialRE.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
		accessKey, region = m[1], m[2]
	}
	list := r.URL.Query().Get("list-type") == "2"
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{method: r.Method, key: key, accessKey: accessKey, list: list,
		rangeHeader: r.Header.Get("Range"), ifMatch: r.Header.Get("If-Match")})
	fail, stall := f.fail, f.stall
	f.mu.Unlock()
	if stall > 0 {
		select {
		case <-time.After(stall):
		case <-r.Context().Done():
			return
		}
	}
	if fail != nil {
		if status, code := fail(r, key); status != 0 {
			writeS3Error(w, r, status, code, "SECRET-MARKER arn:aws:iam::999999999999:role/internal 10.0.0.1")
			return
		}
	}
	if bucket != testBucket {
		writeS3Error(w, r, http.StatusNotFound, "NoSuchBucket", "no bucket")
		return
	}
	switch {
	case r.Method == http.MethodHead && key == "":
		w.Header().Set("X-Amz-Bucket-Region", f.region)
		if region != f.region {
			w.WriteHeader(http.StatusMovedPermanently)
		}
	case list:
		if f.listRegion != "" && region != f.listRegion {
			w.Header().Set("X-Amz-Bucket-Region", f.listRegion)
			writeS3Error(w, r, http.StatusMovedPermanently, "PermanentRedirect", "wrong region")
			return
		}
		f.list(w, r)
	case r.Method == http.MethodGet:
		f.get(w, r, key)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func writeS3Error(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message><RequestId>req</RequestId></Error>`, code, message)
	}
}

type listContents struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int    `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type listResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	KeyCount              int            `xml:"KeyCount"`
	IsTruncated           bool           `xml:"IsTruncated"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	Contents              []listContents `xml:"Contents"`
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	start, _ := strconv.Atoi(r.URL.Query().Get("continuation-token"))
	f.mu.Lock()
	var keys []string
	for k := range f.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	out := listResult{Name: testBucket, Prefix: prefix}
	end := min(start+f.pageSize, len(keys))
	for _, k := range keys[start:end] {
		o := f.objects[k]
		class := o.storageClass
		if class == "" {
			class = "STANDARD"
		}
		out.Contents = append(out.Contents, listContents{Key: k, LastModified: o.modified.Format(time.RFC3339), ETag: o.etag, Size: len(o.body), StorageClass: class})
	}
	if end < len(keys) || f.repeatToken {
		out.IsTruncated, out.NextContinuationToken = true, strconv.Itoa(end)
		if f.repeatToken {
			out.NextContinuationToken = "same"
		}
	}
	f.mu.Unlock()
	out.KeyCount = len(out.Contents)
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(out)
}

func (f *fakeS3) get(w http.ResponseWriter, r *http.Request, key string) {
	f.mu.Lock()
	o, ok := f.objects[key]
	cut := f.cut[key]
	if cut > 0 {
		f.cut[key] = cut - 1
	}
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	slow := f.slow
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	if !ok {
		writeS3Error(w, r, http.StatusNotFound, "NoSuchKey", "no key")
		return
	}
	if match := r.Header.Get("If-Match"); match != "" && match != o.etag {
		writeS3Error(w, r, http.StatusPreconditionFailed, "PreconditionFailed", "etag")
		return
	}
	body, status := o.body, http.StatusOK
	if rg := r.Header.Get("Range"); rg != "" {
		var from, to int
		if _, err := fmt.Sscanf(rg, "bytes=%d-%d", &from, &to); err == nil && from < len(body) {
			to = min(to, len(body)-1)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(body)))
			body, status = body[from:to+1], http.StatusPartialContent
		}
	}
	w.Header().Set("ETag", o.etag)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if cut > 0 {
		_, _ = w.Write(body[:len(body)/2])
		w.(http.Flusher).Flush() // the half reaches the reader before the connection drops
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
		return
	}
	if slow > 0 {
		for i := range body {
			select {
			case <-time.After(slow):
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write(body[i : i+1])
			w.(http.Flusher).Flush()
		}
		return
	}
	_, _ = w.Write(body)
}

// trust is how a fake customer role treats an AssumeRole.
type trust uint8

const (
	// trustEnforced accepts only the external ID of allowed workspaces.
	trustEnforced trust = iota
	// trustOpen accepts any external ID, or none.
	trustOpen
	// trustPattern accepts any Helix workspace's external ID, as a
	// StringLike "helix-foundry-*" condition does.
	trustPattern
	trustNobody
	trustPackedPolicy
	trustInvalid
)

type stsCall struct {
	roleARN, externalID, sessionName, accessKey string
}

// fakeSTS issues sessions: the foundry-connector role to the ambient
// identity, and customer roles to it per their trust.
type fakeSTS struct {
	url    string
	mu     sync.Mutex
	calls  []stsCall
	roles  map[string]trust
	allow  map[string][]string // role to the workspaces its trust names
	expiry time.Duration
	issued int
}

func newFakeSTS(t *testing.T) *fakeSTS {
	f := &fakeSTS{roles: map[string]trust{customerRole: trustEnforced}, allow: map[string][]string{customerRole: {"ws-1"}}, expiry: time.Hour}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	f.url = server.URL
	return f
}

func (f *fakeSTS) seen() []stsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// isProbe is an AssumeRole testing the trust with a random external ID.
func (c stsCall) isProbe() bool { return strings.HasPrefix(c.sessionName, "foundry-probe-") }

// sessions are the AssumeRoles of customer roles that are not probes.
func (f *fakeSTS) sessions() []stsCall {
	var out []stsCall
	for _, c := range f.seen() {
		if c.roleARN != testConnectorRole && !c.isProbe() {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeSTS) serve(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	accessKey := ""
	if m := credentialRE.FindStringSubmatch(r.Header.Get("Authorization")); m != nil {
		accessKey = m[1]
	}
	call := stsCall{roleARN: r.Form.Get("RoleArn"), externalID: r.Form.Get("ExternalId"), sessionName: r.Form.Get("RoleSessionName"), accessKey: accessKey}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	behaviour, allowed, expiry := f.roles[call.roleARN], f.allow[call.roleARN], f.expiry
	f.issued++
	issued := f.issued
	f.mu.Unlock()
	deny := func(status int, code, message string) {
		w.WriteHeader(status)
		fmt.Fprintf(w, `<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type><Code>%s</Code><Message>%s</Message></Error><RequestId>req</RequestId></ErrorResponse>`, code, message)
	}
	switch {
	case call.roleARN == testConnectorRole:
		if accessKey != "AMBIENT" {
			deny(http.StatusForbidden, "AccessDenied", "only the pod may assume the connector role")
			return
		}
	case accessKey != "ASIAHOP1":
		deny(http.StatusForbidden, "AccessDenied", "only the connector role is trusted")
		return
	case behaviour == trustOpen:
	case behaviour == trustPattern && strings.HasPrefix(call.externalID, "helix-foundry-"):
	case behaviour == trustNobody:
		deny(http.StatusForbidden, "AccessDenied", "SECRET-MARKER not trusted")
		return
	case behaviour == trustPackedPolicy:
		deny(http.StatusBadRequest, "PackedPolicyTooLarge", "SECRET-MARKER arn:aws:iam::999999999999:role/internal is too large")
		return
	case behaviour == trustInvalid:
		deny(http.StatusBadRequest, "ValidationError", "bad arn")
		return
	case !slices.ContainsFunc(allowed, func(ws string) bool { return call.externalID == S3ExternalID(ws) }):
		deny(http.StatusForbidden, "AccessDenied", "external id")
		return
	}
	key := "ASIAHOP1"
	if call.roleARN != testConnectorRole {
		key = fmt.Sprintf("ASIACUST%d", issued)
	}
	w.Header().Set("Content-Type", "text/xml")
	fmt.Fprintf(w, `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult>
<Credentials><AccessKeyId>%s</AccessKeyId><SecretAccessKey>secret</SecretAccessKey><SessionToken>token</SessionToken><Expiration>%s</Expiration></Credentials>
<AssumedRoleUser><Arn>%s/session</Arn><AssumedRoleId>AROA:session</AssumedRoleId></AssumedRoleUser></AssumeRoleResult>
<ResponseMetadata><RequestId>req</RequestId></ResponseMetadata></AssumeRoleResponse>`, key, time.Now().Add(expiry).UTC().Format(time.RFC3339), call.roleARN)
}

// fastTimeouts keep failure tests in milliseconds.
var fastTimeouts = s3Timeouts{
	header: 300 * time.Millisecond, idle: 300 * time.Millisecond, object: 3 * time.Second, perMiB: 0,
	expiry: 5 * time.Minute, test: 5 * time.Second, sync: 5 * time.Second, sample: 3 * time.Second, retryMax: time.Millisecond,
}

// s3Harness is the s3 kind wired to a fake bucket and STS.
type s3Harness struct {
	kind *s3Kind
	s3   *fakeS3
	sts  *fakeSTS
}

func newS3Harness(t *testing.T, edit func(*S3Options)) *s3Harness {
	t.Helper()
	h := &s3Harness{s3: newFakeS3(t), sts: newFakeSTS(t)}
	opts := S3Options{
		Base:    aws.Config{Region: "us-east-2", Credentials: credentials.NewStaticCredentialsProvider("AMBIENT", "AMBIENT-SECRET", "")},
		RoleARN: testConnectorRole, MaxSourceBytes: 1 << 20, MaxConnectorBytes: 4 << 20, MaxTableBytes: 16 << 20,
		Reserved: []string{"example-foundry-data"}, TempDir: t.TempDir(),
	}
	if edit != nil {
		edit(&opts)
	}
	h.kind = newS3Kind(Endpoints{S3: h.s3.url, STS: h.sts.url}, Guard{AllowPrivate: true}, opts)
	h.kind.timeouts = fastTimeouts
	return h
}

// prepared runs settings through Prepare, as a test or edit does.
func (h *s3Harness) prepared(t *testing.T, settings, secret string) Source {
	t.Helper()
	raw, err := h.kind.Prepare(Source{Settings: []byte(settings), Secret: secret})
	if err != nil {
		t.Fatalf("Prepare(%s) = %v", settings, err)
	}
	return Source{Settings: raw, Secret: secret, WorkspaceID: "ws-1"}
}

func (h *s3Harness) roleSource(t *testing.T, extra string) Source {
	return h.prepared(t, `{"bucket":"acme-exports","prefix":"e2e","role":{"arn":"`+customerRole+`"}`+extra+`}`, "")
}

func (h *s3Harness) keySource(t *testing.T, extra string) Source {
	return h.prepared(t, `{"bucket":"acme-exports","prefix":"e2e","keys":{"access_key_id":"AKIACUSTOMER"}`+extra+`}`, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
}

func TestS3Security(t *testing.T) {
	ctx := context.Background()
	// The process's own identity reads Helix's buckets, so it must
	// never sign a request to a customer's.
	t.Run("never signs a bucket request with the process's identity", func(t *testing.T) {
		for mode, wantPrefix := range map[string]string{"role": "ASIACUST", "keys": "AKIACUSTOMER"} {
			h := newS3Harness(t, nil)
			putE2E(h.s3)
			src := h.roleSource(t, "")
			if mode == "keys" {
				src = h.keySource(t, "")
			}
			if _, err := h.kind.Discover(ctx, src); err != nil {
				t.Fatal(err)
			}
			syncSrc, table := h.discovered(t, src, "customers")
			if _, err := h.kind.Sync(ctx, syncSrc, table, &rowSink{}); err != nil {
				t.Fatal(err)
			}
			for _, r := range h.s3.seen() {
				if !strings.HasPrefix(r.accessKey, wantPrefix) {
					t.Fatalf("%s: a request signed by %q: %+v", mode, r.accessKey, r)
				}
			}
		}
	})
	// The external ID comes from the workspace, never from settings.
	t.Run("assumes the role with the workspace's external ID", func(t *testing.T) {
		h := newS3Harness(t, nil)
		putE2E(h.s3)
		h.sts.allow[customerRole] = []string{"ws-1", "ws-2"}
		src := h.roleSource(t, `,"external_id":"helix-foundry-ws-2","region":"eu-west-2"`)
		if strings.Contains(string(src.Settings), "external_id") {
			t.Fatalf("settings kept %s", src.Settings)
		}
		if _, err := h.kind.Discover(ctx, src); err != nil {
			t.Fatal(err)
		}
		other := src
		other.WorkspaceID = "ws-2"
		if _, err := h.kind.Discover(ctx, other); err != nil {
			t.Fatal(err)
		}
		var hop1, probes int
		var sessions []stsCall
		for _, c := range h.sts.seen() {
			switch {
			case c.roleARN == testConnectorRole:
				hop1++
				if c.accessKey != "AMBIENT" || c.sessionName != "foundry-connector" {
					t.Fatalf("hop1 = %+v", c)
				}
			case c.isProbe():
				probes++
				if c.externalID == S3ExternalID("ws-1") || c.externalID == S3ExternalID("ws-2") || !externalIDRE.MatchString(c.externalID) {
					t.Fatalf("probe = %+v", c)
				}
			default:
				sessions = append(sessions, c)
			}
		}
		if hop1 != 1 || probes != 2 || len(sessions) != 2 ||
			sessions[0] != (stsCall{customerRole, "helix-foundry-ws-1", "foundry-ws-1", "ASIAHOP1"}) ||
			sessions[1] != (stsCall{customerRole, "helix-foundry-ws-2", "foundry-ws-2", "ASIAHOP1"}) {
			t.Fatalf("hop1 %d, probes %d, sessions %+v", hop1, probes, sessions)
		}
	})
	// A trust admitting every Helix workspace by pattern protects nothing:
	// any workspace that learned the ARN could read the bucket.
	t.Run("refuses a role whose trust does not require the external ID", func(t *testing.T) {
		for _, trust := range []trust{trustOpen, trustPattern} {
			h := newS3Harness(t, nil)
			putE2E(h.s3)
			h.sts.roles[customerRole] = trust
			_, err := h.kind.Discover(ctx, h.roleSource(t, ""))
			if got := invalidMessage(t, err); got != "your role's trust policy must require external ID helix-foundry-ws-1; Helix won't use a role that doesn't" {
				t.Fatalf("trust %d: message = %q", trust, got)
			}
			if n := len(h.s3.seen()); n != 0 || len(h.sts.sessions()) != 0 {
				t.Fatalf("trust %d: %d bucket requests, sessions %+v", trust, n, h.sts.sessions())
			}
		}
	})
	t.Run("maps STS refusals", func(t *testing.T) {
		for behaviour, want := range map[trust]string{
			trustNobody:       "Helix could not assume the role; check its trust policy names " + testConnectorRole + " and requires external ID helix-foundry-ws-1",
			trustInvalid:      msgRoleARN,
			trustPackedPolicy: msgUnreadable,
		} {
			h := newS3Harness(t, nil)
			h.sts.roles[customerRole] = behaviour
			_, err := h.kind.Discover(ctx, h.roleSource(t, ""))
			var got string
			var inv *InvalidError
			if errors.As(err, &inv) {
				got = inv.Message
			} else {
				got = refusedMessage(t, err)
			}
			if got != want || strings.Contains(got, "SECRET-MARKER") || strings.Contains(got, "999999999999") {
				t.Fatalf("trust %d: message = %q", behaviour, got)
			}
		}
		// The workspace's own external ID, which a role trusting another workspace refuses.
		h := newS3Harness(t, nil)
		src := h.roleSource(t, "")
		src.WorkspaceID = "ws-3"
		_, err := h.kind.Discover(ctx, src)
		if got := refusedMessage(t, err); !strings.Contains(got, "requires external ID helix-foundry-ws-3") {
			t.Fatalf("message = %q", got)
		}
		// The connector role itself denied, as before the deployment's trust is
		// applied, is Helix's failure: the customer's trust is not blamed,
		// and their role is never asked.
		off := newS3Harness(t, nil)
		off.kind.opts.Base.Credentials = credentials.NewStaticCredentialsProvider("OTHER", "x", "")
		off.kind.hop1 = newS3Kind(Endpoints{STS: off.sts.url}, Guard{}, off.kind.opts).hop1
		for _, sync := range []bool{false, true} {
			src := off.roleSource(t, "")
			src.ForSync = sync
			_, err = off.kind.Discover(ctx, src)
			if got := refusedMessage(t, err); got != msgUnreadable {
				t.Fatalf("sync %v: message = %q", sync, got)
			}
		}
		_, err = off.kind.Sync(ctx, off.roleSource(t, ""), Table{Name: "customers"}, &rowSink{})
		if got := refusedMessage(t, err); got != msgUnreadable {
			t.Fatalf("Sync message = %q", got)
		}
		for _, c := range off.sts.seen() {
			if c.roleARN != testConnectorRole {
				t.Fatalf("asked the customer's role: %+v", c)
			}
		}
		// Settings stored before the deployment turned role access off.
		none := newS3Harness(t, nil)
		src = none.roleSource(t, "")
		none.kind.hop1 = nil
		_, err = none.kind.Discover(ctx, src)
		if got := invalidMessage(t, err); got != msgNoRole {
			t.Fatalf("message = %q", got)
		}
	})
	// A custom endpoint is the customer's choice of host.
	t.Run("refuses an endpoint inside Helix's network", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("reached %s", r.URL)
		}))
		t.Cleanup(server.Close)
		h := newS3Harness(t, nil)
		h.kind.guard = Guard{}
		src := h.prepared(t, `{"bucket":"acme-exports","keys":{"access_key_id":"AKIAR2","endpoint":"`+server.URL+`"}}`, "secret")
		_, err := h.kind.Discover(ctx, src)
		if got := invalidMessage(t, err); got != msgPrivate {
			t.Fatalf("message = %q", got)
		}
	})
	t.Run("follows no redirect", func(t *testing.T) {
		var reached atomic.Int64
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
		t.Cleanup(target.Close)
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/latest/meta-data/", http.StatusTemporaryRedirect)
		}))
		t.Cleanup(server.Close)
		h := newS3Harness(t, nil)
		h.kind.tlsConfig = server.Client().Transport.(*http.Transport).TLSClientConfig
		src := h.prepared(t, `{"bucket":"acme-exports","keys":{"access_key_id":"AKIAR2","endpoint":"`+server.URL+`"}}`, "secret")
		_, err := h.kind.Discover(ctx, src)
		if got := refusedMessage(t, err); got != msgUnreadable || reached.Load() != 0 {
			t.Fatalf("message = %q, target reached %d times", got, reached.Load())
		}
	})
	// Upstream text names Helix's internals; it stays in the log.
	t.Run("keeps upstream text out of messages", func(t *testing.T) {
		h := newS3Harness(t, nil)
		h.kind.guard = Guard{}
		src := h.prepared(t, `{"bucket":"acme-exports","keys":{"access_key_id":"AKIAR2","endpoint":"https://bucket.nonexistent.invalid"}}`, "secret")
		_, err := h.kind.Discover(ctx, src)
		if got := refusedMessage(t, err); got != msgUnreadable {
			t.Fatalf("a DNS failure = %q", got)
		}
		for code, want := range map[string]string{
			"AccessDenied":          "Helix cannot list s3://acme-exports/e2e/; allow s3:ListBucket on this folder",
			"InvalidAccessKeyId":    msgKeyRejected,
			"SignatureDoesNotMatch": msgKeyRejected,
			"InternalError":         msgThrottled,
			"WeirdError":            msgUnreadable,
		} {
			h := newS3Harness(t, nil)
			putE2E(h.s3)
			status := http.StatusForbidden
			switch code {
			case "InternalError":
				status = http.StatusInternalServerError
			case "WeirdError":
				status = http.StatusBadRequest
			}
			h.s3.fail = func(r *http.Request, _ string) (int, string) {
				if r.URL.Query().Get("list-type") == "2" {
					return status, code
				}
				return 0, ""
			}
			_, err := h.kind.Discover(ctx, h.keySource(t, `,"region":"eu-west-2"`))
			if got := refusedMessage(t, err); got != want || strings.Contains(err.(*RefusedError).Message, "SECRET-MARKER") {
				t.Fatalf("%s: message = %q", code, got)
			}
		}
		h = newS3Harness(t, nil)
		_, err = h.kind.Discover(ctx, h.prepared(t, `{"bucket":"missing-bucket","region":"eu-west-2","keys":{"access_key_id":"AKIAX"}}`, "s"))
		if got := invalidMessage(t, err); got != "bucket missing-bucket does not exist" {
			t.Fatalf("message = %q", got)
		}
		h = newS3Harness(t, nil)
		_, err = h.kind.Discover(ctx, h.prepared(t, `{"bucket":"missing-bucket","keys":{"access_key_id":"AKIAX"}}`, "s"))
		if got := invalidMessage(t, err); got != "bucket missing-bucket does not exist" {
			t.Fatalf("message from the region lookup = %q", got)
		}
	})
	t.Run("rejects malformed settings", func(t *testing.T) {
		h := newS3Harness(t, nil)
		for _, settings := range []string{`[`, `{"bucket":"a"}`} {
			if _, err := h.kind.Discover(ctx, Source{Settings: []byte(settings)}); invalidMessage(t, err) != "S3 settings are malformed" {
				t.Fatalf("Discover(%s) = %v", settings, err)
			}
			if _, err := h.kind.Sync(ctx, Source{Settings: []byte(settings)}, Table{}, &rowSink{}); invalidMessage(t, err) != "S3 settings are malformed" {
				t.Fatalf("Sync(%s) = %v", settings, err)
			}
		}
	})
}
