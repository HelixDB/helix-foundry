package enrichment

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
)

// contextDev is an httptest context.dev that answers each request with the
// next of answers (the last repeats), and records what it was sent.
type contextDev struct {
	mu       sync.Mutex
	answers  []func(w http.ResponseWriter)
	requests []map[string]any
	auth     []string
}

func newContextDev(t *testing.T, answers ...func(w http.ResponseWriter)) (*contextDev, *Client) {
	t.Helper()
	dev := &contextDev{answers: answers}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		dev.mu.Lock()
		n := len(dev.requests)
		dev.requests = append(dev.requests, body)
		dev.auth = append(dev.auth, r.Header.Get("Authorization")+" "+r.URL.Path)
		answer := dev.answers[min(n, len(dev.answers)-1)]
		dev.mu.Unlock()
		answer(w)
	}))
	t.Cleanup(server.Close)
	client := &Client{
		HTTP: &http.Client{Timeout: time.Second}, BaseURL: server.URL + "/v1", APIKey: "key", Limiter: NewLimiter(48),
		Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }, Jitter: func(time.Duration) time.Duration { return 0 },
		Now: func() time.Time { return planNow },
	}
	return dev, client
}

func (d *contextDev) sent() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.requests)
}

func status(code int, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("X-Request-Id", "hdr-1")
		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "120")
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

// candidate is a 200 of POST /v1/people/enrich as context.dev documents it
// (docs.context.dev/api-reference/people/enrich): every field person
// requires, dates as {year, month?, day?}, the current position only as
// current_role, an alpha-3 country code, the credits it charged and the
// request id.
const candidate = `{"match":{"status":"candidate","score":96,"person":{"name":{"full":"Ada Lovelace","first":"Ada","last":"Lovelace"},
"email":"ada@acme.io","avatar_url":"https://img.example/ada.png","bio":"secret",
"location":{"display":"London, United Kingdom","city":"London","region":"England","country":"United Kingdom","country_code":"GBR"},
"social_urls":["https://www.linkedin.com/in/ada-lovelace"],"website_urls":["https://ada.dev"],"skills":["x"],
"current_role":{"title":"VP Engineering","organization":{"name":"Acme","domain":"acme.io"},"location":"London",
"start_date":{"year":2022,"month":3},"is_current":true},"current_role_status":"present",
"experience":[{"title":"Engineer","organization":{"name":"Initech","domain":"initech.com"},"start_date":{"year":2015},"end_date":{"year":2022,"month":2},"is_current":false},
{"title":"Volunteer","organization":{"name":"No domain"},"start_date":{"year":2010},"is_current":false}],
"education":[{"institution":{"name":"University of London"},"degree":"BSc"}],
"last_updated":"2026-09-01T00:00:00Z","checked_at":"2026-10-01T00:00:00Z"}},
"partial":false,"key_metadata":{"credits_consumed":20,"credits_remaining":980},"request_id":"req-1"}`

// notFound is a 200 with no match, which context.dev doesn't charge.
const notFound = `{"match":{"status":"not_found","score":null,"person":null},"partial":false,
"key_metadata":{"credits_consumed":0,"credits_remaining":980},"request_id":"req-1"}`

// refusal is context.dev's error body for code (ErrorResponse: message,
// error_code, key_metadata and request_id; error_data on some).
func refusal(code string) string {
	return `{"message":"refused","error_code":"` + code + `","key_metadata":{"credits_consumed":0,"credits_remaining":980},"request_id":"req-1"}`
}

// always lets every lookup be sent.
func always() string { return "" }

func lookup() Lookup {
	return Lookup{Entity: "c_17", Sent: Sent{Email: "ada@acme.io", Slug: "ada-lovelace"}, LoadID: "L1", CallID: "C1"}
}

func TestClientDo(t *testing.T) {
	// The request carries the clues, zero data retention and the tags a
	// charge is reconciled by, and nothing else of the person.
	t.Run("sends the clues with zdr and tags", func(t *testing.T) {
		dev, client := newContextDev(t, status(200, candidate))
		call := client.Do(context.Background(), always, lookup())
		if !call.Reached || call.Status != 200 || call.RequestID != "req-1" || call.Attempts != 1 || !call.At.Equal(planNow) {
			t.Fatalf("call = %+v", call)
		}
		got := dev.requests[0]
		if got["email"] != "ada@acme.io" || got["zdr"] != "enabled" || len(got) != 5 {
			t.Fatalf("request = %v", got)
		}
		if urls := got["social_urls"].([]any); len(urls) != 1 || urls[0] != "https://www.linkedin.com/in/ada-lovelace" {
			t.Fatalf("social_urls = %v", urls)
		}
		if tags := got["tags"].([]any); len(tags) != 3 || tags[0] != "foundry" || tags[1] != "L1" || tags[2] != "C1" {
			t.Fatalf("tags = %v", tags)
		}
		if ms := got["timeoutOpts"].(map[string]any)["milliseconds"]; ms != float64(20000) {
			t.Fatalf("timeoutOpts = %v", got["timeoutOpts"])
		}
		if dev.auth[0] != "Bearer key /v1/people/enrich" {
			t.Fatalf("auth = %q", dev.auth[0])
		}
		answer, match, partial, err := Judge(call)
		if err != nil || answer != Matched || partial || match.Score != 96 || match.Person.Name.Full != "Ada Lovelace" {
			t.Fatalf("judge = %v %+v %v %v", answer, match, partial, err)
		}
		row := match.Person.Profile(match.Score, Entry{Clue: Clue{Name: "Ada Lovelace", Email: "ada@acme.io"}, Sent: lookup().Sent}, call, planNow)
		if row.CurrentTitle != "VP Engineering" || row.LocationCountry != "United Kingdom" || row.ProfileID != "li:ada-lovelace" ||
			row.ProviderRequestID != "req-1" {
			t.Fatalf("profile = %+v", row)
		}
		// The current position, only in current_role, is the newest employer.
		want := []Experience{
			{ID: 1, Title: "VP Engineering", Organization: Organization{Name: "Acme", Domain: "acme.io"}, Location: "London", StartDate: "2022-03", IsCurrent: true},
			{ID: 2, Title: "Engineer", Organization: Organization{Name: "Initech", Domain: "initech.com"}, StartDate: "2015", EndDate: "2022-02"},
		}
		if len(row.Experience) != len(want) || row.Experience[0] != want[0] || row.Experience[1] != want[1] {
			t.Fatalf("experience = %+v", row.Experience)
		}
	})

	// context.dev puts its request id in the body; an answer without one, such
	// as a gateway's error page, keeps X-Request-Id's.
	t.Run("reads the request id from the body, else the header", func(t *testing.T) {
		_, client := newContextDev(t, status(502, "<html>bad gateway</html>"), status(502, "<html>bad gateway</html>"))
		if call := client.Do(context.Background(), always, lookup()); call.Status != 502 || call.RequestID != "hdr-1" {
			t.Fatalf("call = %+v", call)
		}
	})

	// A 429 is a refusal before any work: wait (capped at a minute), go
	// again, and charge nothing; the replica's limit halves.
	t.Run("waits out a 429 storm and sends again", func(t *testing.T) {
		// context.dev documents a Retry-After of 1 to 60s; the fixture's 120 shows the cap holds anyway.
		dev, client := newContextDev(t, status(429, refusal("RATE_LIMITED")), status(429, refusal("RATE_LIMITED")), status(200, candidate))
		var waits []time.Duration
		client.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
		client.Jitter = func(d time.Duration) time.Duration { return d / 10 }
		call := client.Do(context.Background(), always, lookup())
		if call.Status != 200 || dev.sent() != 3 || call.Attempts != 3 {
			t.Fatalf("call = %+v after %d requests", call, dev.sent())
		}
		if len(waits) != 2 || waits[0] != 66*time.Second {
			t.Fatalf("waits = %v, want Retry-After capped at 60s plus jitter", waits)
		}
		if client.Limiter.Limit() != 12 {
			t.Fatalf("limit = %d after two 429s", client.Limiter.Limit())
		}
	})

	// A lookup the step may no longer send, mid-storm, ends unreached: free,
	// and looked up next load. Its call says why it was not sent, which says
	// how its batch ends the step.
	t.Run("a 429 left unsent is unreached, saying why", func(t *testing.T) {
		dev, client := newContextDev(t, status(429, refusal("RATE_LIMITED")))
		call := client.Do(context.Background(), func() string {
			if dev.sent() == 0 {
				return ""
			}
			return "stopped"
		}, lookup())
		if answer, _, _, _ := Judge(call); answer != Unreached || dev.sent() != 1 || call.Error != "stopped" {
			t.Fatalf("call = %+v, answer %v", call, answer)
		}
		_, client = newContextDev(t, status(200, candidate))
		if call := client.Do(context.Background(), func() string { return "halted" }, lookup()); call.Reached || call.Attempts != 0 ||
			call.Error != "halted" {
			t.Fatalf("a lookup never sent = %+v", call)
		}
	})

	// A context that ends while a lookup waits says why it was not sent,
	// whatever the step says.
	t.Run("a lookup a cancelled context stops is unreached", func(t *testing.T) {
		_, client := newContextDev(t, status(200, candidate))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if call := client.Do(ctx, always, lookup()); call.Reached || call.Error != "not sent: context canceled" {
			t.Fatalf("call = %+v", call)
		}
	})

	// A step's lease fence may pass while a lookup waits for a slot: it is
	// asked again just before the request, and a slot it gives back is free.
	t.Run("asks again once a slot is free", func(t *testing.T) {
		dev, client := newContextDev(t, status(200, candidate))
		asked := 0
		call := client.Do(context.Background(), func() string {
			if asked++; asked == 1 {
				return ""
			}
			return "fenced"
		}, lookup())
		if call.Reached || dev.sent() != 0 || asked != 2 || call.Error != "fenced" {
			t.Fatalf("call = %+v after %d requests, may asked %d times", call, dev.sent(), asked)
		}
		if call := client.Do(context.Background(), always, lookup()); call.Status != 200 || client.Limiter.Limit() != 48 {
			t.Fatalf("after a slot was given back: call %+v, limit %d", call, client.Limiter.Limit())
		}
	})

	// A server error gets one more try; a second is the provider failing.
	t.Run("sends a 5xx once more, then fails", func(t *testing.T) {
		dev, client := newContextDev(t, status(503, refusal("SERVICE_UNAVAILABLE")), status(500, refusal("INTERNAL_ERROR")), status(200, candidate))
		call := client.Do(context.Background(), always, lookup())
		if answer, _, _, _ := Judge(call); answer != Failed || dev.sent() != 2 {
			t.Fatalf("call = %+v after %d", call, dev.sent())
		}
		dev, client = newContextDev(t, status(408, refusal("REQUEST_TIMEOUT")), status(200, candidate))
		if call := client.Do(context.Background(), always, lookup()); call.Status != 200 || dev.sent() != 2 {
			t.Fatalf("408 then 200 = %+v", call)
		}
	})

	// No answer may still have been charged: it is booked uncertain and never
	// sent again, so a person is never charged twice in one load.
	t.Run("never sends a timeout again", func(t *testing.T) {
		dev, client := newContextDev(t, func(http.ResponseWriter) { time.Sleep(300 * time.Millisecond) })
		client.HTTP.Timeout = 50 * time.Millisecond
		call := client.Do(context.Background(), always, lookup())
		if answer, _, _, _ := Judge(call); answer != NoAnswer || call.Error == "" {
			t.Fatalf("call = %+v", call)
		}
		time.Sleep(350 * time.Millisecond)
		if dev.sent() != 1 {
			t.Fatalf("sent %d times", dev.sent())
		}
	})

	// An answer cut off mid-body may have been a charged candidate: it is no
	// answer, booked uncertain and never sent again, as a timeout is, not an
	// unreadable 200 taken for a free provider error.
	t.Run("never sends an answer cut off mid-body again", func(t *testing.T) {
		dev, client := newContextDev(t, func(w http.ResponseWriter) {
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_, _ = fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(candidate),
				candidate[:len(candidate)/2])
			_ = buf.Flush()
		})
		call := client.Do(context.Background(), always, lookup())
		if answer, _, _, _ := Judge(call); answer != NoAnswer || call.Status != 0 || call.Error == "" || dev.sent() != 1 {
			t.Fatalf("call = %+v after %d requests", call, dev.sent())
		}
	})

	// A replica calls context.dev a full Limiter at a time: a connection
	// closed after every answer would make each call dial and handshake again,
	// and leave a closed socket behind for a minute.
	t.Run("keeps a connection per slot open between calls", func(t *testing.T) {
		const slots = 8
		var dialed atomic.Int32
		arrived, answer := make(chan struct{}), make(chan struct{})
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			arrived <- struct{}{}
			<-answer
			_, _ = io.WriteString(w, notFound)
		}))
		server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				dialed.Add(1)
			}
		}
		server.Start()
		t.Cleanup(server.Close)
		client := NewClient(config.EnrichmentConfig{
			Provider: config.EnrichmentContextDev, BaseURL: server.URL + "/v1", APIKey: "key", Concurrency: slots,
		})
		for range 3 {
			var calls sync.WaitGroup
			for range slots {
				calls.Go(func() { client.Do(context.Background(), always, lookup()) })
			}
			for range slots { // every slot in flight at once
				<-arrived
			}
			for range slots {
				answer <- struct{}{}
			}
			calls.Wait()
		}
		if got := dialed.Load(); got != slots {
			t.Fatalf("dialed %d connections for 3 rounds of %d calls, want %d", got, slots, slots)
		}
	})

	// Every other answer is judged once, from the raw call alone, so a worker
	// that takes a batch over judges it the same way.
	for _, tc := range []struct {
		name   string
		answer func(http.ResponseWriter)
		want   Answer
	}{
		{"not found", status(200, notFound), NoMatch},
		{"too few clues", status(400, refusal("INSUFFICIENT_CLUES")), Refused},
		{"a free address", status(422, `{"message":"free","status":"error","error_code":"FREE_EMAIL_DETECTED","key_metadata":{"credits_consumed":0,"credits_remaining":980},"request_id":"req-1"}`), Refused},
		{"a disposable address", status(422, `{"message":"disposable","status":"error","error_code":"DISPOSABLE_EMAIL_DETECTED","key_metadata":{"credits_consumed":0,"credits_remaining":980},"request_id":"req-1"}`), Refused},
		{"no credits", status(402, `{"message":"no credits","error_code":"USAGE_EXCEEDED","error_data":{"is_authenticated":true,"should_retry":false,"retry_condition":"sufficient_credits"},"key_metadata":{"credits_consumed":0,"credits_remaining":0},"request_id":"req-1"}`), Halt},
		{"a bad key", status(401, refusal("INVALID_API_KEY")), Halt},
		{"an organization without zero data retention", status(403, refusal("ZDR_NOT_ENABLED")), Halt},
		{"a key without the scope", status(403, refusal("INSUFFICIENT_PERMISSIONS")), Halt},
		{"a deadline the request set", status(408, refusal("REQUEST_TIMEOUT")), Failed},
		{"an unreadable body", status(200, `<html>`), Failed},
		{"a strange status", status(302, ``), Failed},
	} {
		t.Run("judges "+tc.name, func(t *testing.T) {
			_, client := newContextDev(t, tc.answer)
			if answer, _, _, _ := Judge(client.Do(context.Background(), always, lookup())); answer != tc.want {
				t.Fatalf("answer = %v, want %v", answer, tc.want)
			}
		})
	}
}

func TestLimiter(t *testing.T) {
	// A 429 halves the replica's calls at once, never below 4; twenty
	// answered calls earn one more, up to the start.
	t.Run("halves on a 429 and grows by one per 20 answers", func(t *testing.T) {
		l := NewLimiter(48)
		for range 5 {
			_ = l.Acquire(context.Background())
			l.Release(http.StatusTooManyRequests)
		}
		if l.Limit() != 4 {
			t.Fatalf("limit = %d", l.Limit())
		}
		for range 40 {
			_ = l.Acquire(context.Background())
			l.Release(http.StatusOK)
		}
		if l.Limit() != 6 {
			t.Fatalf("limit = %d", l.Limit())
		}
		for range 2000 {
			_ = l.Acquire(context.Background())
			l.Release(http.StatusOK)
		}
		if l.Limit() != 48 {
			t.Fatalf("limit = %d", l.Limit())
		}
	})

	// A full limiter holds callers until a slot frees, or their context ends.
	t.Run("holds callers at the limit", func(t *testing.T) {
		l := NewLimiter(4)
		for range 4 {
			_ = l.Acquire(context.Background())
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if err := l.Acquire(ctx); err == nil {
			t.Fatal("acquired past the limit")
		}
		var got atomic.Bool
		go func() {
			_ = l.Acquire(context.Background())
			got.Store(true)
		}()
		time.Sleep(10 * time.Millisecond)
		if got.Load() {
			t.Fatal("acquired before a release")
		}
		l.Release(http.StatusOK)
		deadline := time.Now().Add(time.Second)
		for !got.Load() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if !got.Load() {
			t.Fatal("a release did not wake the waiter")
		}
	})
}

// documented is context.dev's example of a candidate: the current position
// only as current_role, and an earlier one without a domain.
const documented = `{"match":{"status":"candidate","score":99,"person":{
"name":{"full":"Jane Doe","first":"Jane","last":"Doe"},"email":"jane@example.com","bio":"Co-founder and CTO",
"location":{"display":"San Francisco","city":"San Francisco","region":"California","country":"United States","country_code":"USA"},
"social_urls":["https://www.linkedin.com/in/example/"],"website_urls":["https://example.com"],"current_role_status":"present",
"current_role":{"title":"CTO","organization":{"name":"Example Inc","domain":"example.com"},"location":"San Francisco",
"description":"Building database infrastructure","start_date":{"year":2024,"month":1},"is_current":true},
"experience":[{"title":"Engineer","organization":{"name":"Previous Company"},"start_date":{"year":2020},"end_date":{"year":2023},"is_current":false}],
"education":[{"institution":{"name":"Example University"},"degree":"BSc","field_of_study":"Computer Science","start_date":{"year":2016},"end_date":{"year":2020}}],
"skills":["Rust","Python"],"last_updated":"2026-10-01T12:00:00Z","checked_at":"2026-10-20T12:00:00Z"}},
"key_metadata":{"credits_consumed":20,"credits_remaining":980},"request_id":"00000000-0000-0000-0000-000000000001"}`

func TestDocumentedAnswers(t *testing.T) {
	// A work email's match passes the gate on its current employer alone, and
	// that employer is its work history.
	t.Run("a candidate", func(t *testing.T) {
		call := Call{CallID: "C1", Reached: true, Status: 200, Body: documented, RequestID: "00000000-0000-0000-0000-000000000001"}
		answer, match, partial, err := Judge(call)
		if err != nil || answer != Matched || partial || match.Score != 99 {
			t.Fatalf("judge = %v %+v %v %v", answer, match, partial, err)
		}
		sent := Sent{Email: "jane@example.com"}
		row := match.Person.Profile(match.Score, Entry{Clue: Clue{Entity: "c_1", Name: "Jane Doe", Email: "jane@example.com"}, Sent: sent}, call, planNow)
		if outcome, reason := Gate(row, match.Score, 90, "Jane Doe", sent); outcome != Accepted {
			t.Fatalf("gate = %s %s", outcome, reason)
		}
		want := Experience{ID: 1, Title: "CTO", Organization: Organization{Name: "Example Inc", Domain: "example.com"},
			Location: "San Francisco", StartDate: "2024-01", IsCurrent: true}
		if len(row.Experience) != 1 || row.Experience[0] != want {
			t.Fatalf("experience = %+v", row.Experience)
		}
		if row.ProfileID != "li:example" || row.CurrentOrgDomain != "example.com" || row.LocationCountry != "United States" ||
			row.CurrentRoleStatus != "present" || row.ProviderRequestID != "00000000-0000-0000-0000-000000000001" {
			t.Fatalf("row = %+v", row)
		}
	})

	// No match is a 200 that charges nothing.
	t.Run("no match", func(t *testing.T) {
		body := `{"match":{"status":"not_found","score":null,"person":null},"key_metadata":{"credits_consumed":0,"credits_remaining":1000},` +
			`"request_id":"00000000-0000-0000-0000-000000000002"}`
		if answer, match, partial, err := Judge(Call{Reached: true, Status: 200, Body: body}); err != nil || answer != NoMatch || match != nil || partial {
			t.Fatalf("judge = %v %v %v %v", answer, match, partial, err)
		}
	})

	// An unfinished answer says so: a candidate whose roles were left out is
	// held domain_unknown on a work email, and no one found may be wrong.
	t.Run("a partial answer", func(t *testing.T) {
		unfinished := strings.Replace(documented, `"key_metadata"`, `"partial":true,"key_metadata"`, 1)
		unfinished = strings.Replace(unfinished, `"current_role_status":"present",`, `"current_role_status":"unknown",`, 1)
		unfinished = unfinished[:strings.Index(unfinished, `"current_role":{`)] + unfinished[strings.Index(unfinished, `"education":`):]
		call := Call{Reached: true, Status: 200, Body: unfinished}
		answer, match, partial, err := Judge(call)
		if err != nil || answer != Matched || !partial {
			t.Fatalf("judge = %v %+v %v %v", answer, match, partial, err)
		}
		sent := Sent{Email: "jane@example.com"}
		row := match.Person.Profile(match.Score, Entry{Clue: Clue{Name: "Jane Doe"}, Sent: sent}, call, planNow)
		if outcome, reason := Gate(row, match.Score, 90, "Jane Doe", sent); outcome != Held || reason != ReasonDomainUnknown {
			t.Fatalf("gate = %s %s", outcome, reason)
		}
		body := `{"match":{"status":"not_found","score":null,"person":null},"partial":true}`
		if answer, _, partial, err := Judge(Call{Reached: true, Status: 200, Body: body}); err != nil || answer != NoMatch || !partial {
			t.Fatalf("judge = %v %v %v", answer, partial, err)
		}
	})
}

func TestPersonProfile(t *testing.T) {
	var person Person
	if err := json.Unmarshal([]byte(`{
  "name": {"first": "Ada", "last": "Lovelace"},
  "email": "ada@acme.io", "bio": "b", "avatar_url": "x", "skills": ["s"], "website_urls": ["https://ada.dev"],
  "location": {"display": "London, United Kingdom", "city": "London", "region": "England", "country": "United Kingdom"},
  "social_urls": ["https://twitter.com/ada", "https://www.linkedin.com/in/countess", "https://uk.linkedin.com/in/Ada-Lovelace/"],
  "current_role": {"title": "VP Engineering", "organization": {"name": "Acme", "domain": "WWW.Acme.io"}, "is_current": true},
  "current_role_status": "present",
  "experience": [
    {"title": "Engineer", "organization": {"name": "Initech", "domain": "initech.com"}, "start_date": {"year": 2015}, "end_date": {"year": 2018, "month": 2}},
    {"title": "VP Engineering", "organization": {"name": "Acme", "domain": "www.acme.io"}, "start_date": {"year": 2022, "month": 3, "day": 1}, "is_current": true},
    {"title": "Lead", "organization": {"name": "Initech", "domain": "Initech.com"}, "start_date": {"year": 2018, "month": 3}},
    {"title": "Volunteer", "organization": {"name": "No domain"}}
  ],
  "last_updated": "2026-09-01T00:00:00Z", "checked_at": "2026-10-01T00:00:00Z"}`), &person); err != nil {
		t.Fatal(err)
	}
	entry := Entry{Clue: Clue{Entity: "c_17", Name: "Ada Lovelace", Email: "ada@acme.io", LinkedInURL: "linkedin.com/in/ada-lovelace"},
		Sent: Sent{Email: "ada@acme.io", Slug: "ada-lovelace"}}
	row := person.Profile(96, entry, Call{CallID: "C1", RequestID: "req-1"}, planNow)

	// The row keeps the sent profile's LinkedIn URL, one employer per domain,
	// newest first, and identifies itself by its slug.
	t.Run("keeps the catalog fields", func(t *testing.T) {
		if row.ProfileID != "li:ada-lovelace" || row.LinkedInURL != "https://www.linkedin.com/in/ada-lovelace" {
			t.Fatalf("profile = %s %s", row.ProfileID, row.LinkedInURL)
		}
		if row.FullName != "Ada Lovelace" || row.CurrentTitle != "VP Engineering" || row.CurrentOrgDomain != "acme.io" ||
			row.LocationCountry != "United Kingdom" || row.MatchedOn != "email+linkedin" || row.FetchedAt != "2026-10-20T12:00:00Z" {
			t.Fatalf("row = %+v", row)
		}
		if row.ClueEmail != "ada@acme.io" || row.ClueName != "Ada Lovelace" || row.ClueLinkedInURL != "linkedin.com/in/ada-lovelace" {
			t.Fatalf("clues = %+v", row)
		}
		want := []Experience{
			{ID: 1, Title: "VP Engineering", Organization: Organization{Name: "Acme", Domain: "acme.io"}, StartDate: "2022-03-01", IsCurrent: true},
			{ID: 2, Title: "Lead", Organization: Organization{Name: "Initech", Domain: "initech.com"}, StartDate: "2018-03"},
		}
		if len(row.Experience) != len(want) || row.Experience[0] != want[0] || row.Experience[1] != want[1] {
			t.Fatalf("experience = %+v", row.Experience)
		}
	})

	// The returned email, websites, skills, education, avatar and bio are
	// never stored.
	t.Run("drops what is never stored", func(t *testing.T) {
		raw, _ := json.Marshal(row)
		for _, field := range []string{"ada.dev", "avatar", "bio", "skills", `"email"`, "twitter"} {
			if strings.Contains(string(raw), field) {
				t.Fatalf("row holds %s: %s", field, raw)
			}
		}
	})

	// A profile without LinkedIn is identified by a digest of its name and
	// employers, stable from one load to the next.
	t.Run("digests a profile without LinkedIn", func(t *testing.T) {
		noLinkedIn := person
		noLinkedIn.SocialURLs = nil
		first := noLinkedIn.Profile(96, entry, Call{}, planNow)
		second := noLinkedIn.Profile(96, entry, Call{}, planNow.Add(time.Hour))
		if !strings.HasPrefix(first.ProfileID, "h:") || len(first.ProfileID) != 18 || first.ProfileID != second.ProfileID {
			t.Fatalf("ids = %s %s", first.ProfileID, second.ProfileID)
		}
	})

	// At most twenty employers are kept.
	t.Run("keeps at most twenty employers", func(t *testing.T) {
		many := person
		many.Experience = nil
		for i := range 30 {
			role := Role{Title: "x"}
			role.Organization.Domain = string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".com"
			many.Experience = append(many.Experience, role)
		}
		if got := many.Profile(96, entry, Call{}, planNow); len(got.Experience) != MaxExperience || got.Experience[19].ID != 20 {
			t.Fatalf("experience = %d", len(got.Experience))
		}
	})
}

func TestFake(t *testing.T) {
	client := NewClient(config.EnrichmentConfig{Provider: config.EnrichmentFake, Concurrency: 4})
	look := func(email, slug string) (Answer, *Profile) {
		entry := Entry{Clue: Clue{Name: "x"}, Sent: Sent{Email: email, Slug: slug}}
		call := client.Do(context.Background(), always, Lookup{Sent: entry.Sent, LoadID: "L", CallID: "C"})
		answer, match, _, err := Judge(call)
		if err != nil {
			t.Fatal(err)
		}
		if match == nil {
			return answer, nil
		}
		return answer, match.Person.Profile(match.Score, entry, call, planNow)
	}
	// Dev and the e2e tests drive every outcome from the clue's text alone.
	t.Run("answers by the clue's text", func(t *testing.T) {
		if answer, p := look("ada.lovelace@acme.io", ""); answer != Matched || p.Score != 96 || p.FullName != "Ada Lovelace" ||
			p.CurrentOrgDomain != "acme.io" || len(p.Experience) != 2 || p.Experience[0].Organization.Domain != "acme.io" ||
			!p.Experience[0].IsCurrent || p.Experience[1].Organization.Domain != "initech.com" {
			t.Fatalf("plain = %v %+v", answer, p)
		}
		if answer, _ := look("ada.bad@acme.io", ""); answer != NoMatch {
			t.Fatalf("bad = %v", answer)
		}
		if _, p := look("ada.lovelace.low@acme.io", ""); p.Score != 72 {
			t.Fatalf("low = %+v", p)
		}
		if _, p := look("", "ada-other"); p.FullName != "Bob Smith" {
			t.Fatalf("other = %+v", p)
		}
		_, a := look("ada.lovelace.dup@acme.io", "")
		_, b := look("grace.hopper.dup@acme.io", "")
		if a.ProfileID != b.ProfileID {
			t.Fatalf("dup profiles differ: %s %s", a.ProfileID, b.ProfileID)
		}
		if answer, _ := look("ada.timeout@acme.io", ""); answer != NoAnswer {
			t.Fatalf("timeout = %v", answer)
		}
		if answer, _ := look("ada@gmail.com", ""); answer != Refused {
			t.Fatalf("free mail = %v", answer)
		}
		if answer, _ := look("sales@acme.io", ""); answer != Refused {
			t.Fatalf("role = %v", answer)
		}
		if _, p := look("", "grace-hopper"); p.ProfileID != "li:grace-hopper" || p.FullName != "Grace Hopper" {
			t.Fatalf("slug = %+v", p)
		}
		call := client.Do(context.Background(), always, Lookup{Sent: Sent{Email: "ada.lovelace.partial@acme.io"}, LoadID: "L", CallID: "C"})
		if answer, match, partial, _ := Judge(call); answer != Matched || !partial || match.Person.CurrentRole != nil ||
			match.Person.Experience != nil || match.Person.Name.Full != "Ada Lovelace" {
			t.Fatalf("partial = %v %+v %v", answer, match, partial)
		}
	})
}
