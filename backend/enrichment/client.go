package enrichment

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// providerTimeout is what context.dev is asked to give up after, inside
	// the client's own config.EnrichmentCallTimeout.
	providerTimeout = 20 * time.Second
	// maxRetryAfter bounds a 429's wait, before jitter.
	maxRetryAfter = 60 * time.Second
	// serverRetry is the wait before a 408 or 5xx is sent once more.
	serverRetry = 2 * time.Second
	// maxBody bounds the body a call reads.
	maxBody = 1 << 20
)

// Lookup is one person to look up.
type Lookup struct {
	Entity string
	Sent   Sent
	LoadID string
	CallID string
}

// Call is one line of calls/<b>.jsonl.gz: a lookup's last request and what
// came back, raw, so a worker that took over a batch can judge it without
// calling again. Status 0 is no answer: Error says why. A lookup never sent
// (the step stopped first), or only refused with 429s, is not Reached, and
// its Error says why it was not sent (Do's unsent); so does that of a 408
// or 5xx not sent again, which keeps its answer.
type Call struct {
	CallID    string          `json:"call_id"`
	Entity    string          `json:"entity"`
	Request   json.RawMessage `json:"request"`
	Reached   bool            `json:"reached"`
	Status    int             `json:"status"`
	RequestID string          `json:"request_id,omitempty"`
	Body      string          `json:"body,omitempty"`
	Error     string          `json:"error,omitempty"`
	Attempts  int             `json:"attempts"`
	At        time.Time       `json:"at"`
}

// Answer is what a call came to.
type Answer int

const (
	// Unreached is a lookup never sent, or only refused with 429s: free, and
	// looked up again next load.
	Unreached Answer = iota
	// Matched is a candidate; the gate decides it.
	Matched
	// NoMatch is context.dev finding no one.
	NoMatch
	// Refused is a 400 or 422: the clues were not enough, or a free address.
	Refused
	// Failed is a 408 or 5xx, twice.
	Failed
	// NoAnswer is a timeout or a network error: the call may have been charged.
	NoAnswer
	// Halt is the key or the credits refused (402, 401, 403).
	Halt
)

// String names the answer for metrics.
func (a Answer) String() string {
	switch a {
	case Matched:
		return "matched"
	case NoMatch:
		return "not_found"
	case Refused:
		return "refused"
	case Failed:
		return "failed"
	case NoAnswer:
		return "no_answer"
	case Halt:
		return "halt"
	}
	return "unreached"
}

// Match is a candidate context.dev found, and its score.
type Match struct {
	Person *Person
	Score  int
}

// Judge reads what a call came to: for Matched, the candidate; for Matched
// and NoMatch, whether context.dev said the answer is partial, its
// unfinished fields left out.
func Judge(c Call) (answer Answer, match *Match, partial bool, err error) {
	switch {
	case !c.Reached:
		return Unreached, nil, false, nil
	case c.Status == 0:
		return NoAnswer, nil, false, nil
	case c.Status == http.StatusTooManyRequests:
		return Unreached, nil, false, nil
	case c.Status == http.StatusPaymentRequired || c.Status == http.StatusUnauthorized || c.Status == http.StatusForbidden:
		return Halt, nil, false, nil
	case c.Status == http.StatusBadRequest || c.Status == http.StatusUnprocessableEntity:
		return Refused, nil, false, nil
	case c.Status == http.StatusRequestTimeout || c.Status >= 500:
		return Failed, nil, false, nil
	case c.Status != http.StatusOK:
		return Failed, nil, false, nil
	}
	var body struct {
		Match struct {
			Status string  `json:"status"`
			Score  *int    `json:"score"`
			Person *Person `json:"person"`
		} `json:"match"`
		Partial bool `json:"partial"`
	}
	if err := json.Unmarshal([]byte(c.Body), &body); err != nil {
		return Failed, nil, false, fmt.Errorf("call %s: unreadable answer: %w", c.CallID, err)
	}
	switch m := body.Match; {
	case m.Status == "not_found":
		return NoMatch, nil, body.Partial, nil
	case m.Status == "candidate" && m.Person != nil && m.Score != nil:
		return Matched, &Match{Person: m.Person, Score: *m.Score}, body.Partial, nil
	}
	return Failed, nil, false, fmt.Errorf("call %s: answer %q is neither a candidate nor not_found", c.CallID, body.Match.Status)
}

// Person is context.dev's profile of a candidate, the parts Helix reads.
// Its email, websites, skills, education, avatar and bio are never kept.
type Person struct {
	Name struct {
		Full  string `json:"full"`
		First string `json:"first"`
		Last  string `json:"last"`
	} `json:"name"`
	Location struct {
		Display string `json:"display"`
		City    string `json:"city"`
		Region  string `json:"region"`
		Country string `json:"country"`
	} `json:"location"`
	SocialURLs        []string `json:"social_urls"`
	CurrentRole       *Role    `json:"current_role"`
	CurrentRoleStatus string   `json:"current_role_status"`
	Experience        []Role   `json:"experience"`
	LastUpdated       string   `json:"last_updated"`
	CheckedAt         string   `json:"checked_at"`
}

// Role is one position.
type Role struct {
	Title        string `json:"title"`
	Organization struct {
		Name   string `json:"name"`
		Domain string `json:"domain"`
	} `json:"organization"`
	Location  string `json:"location"`
	StartDate *Date  `json:"start_date"`
	EndDate   *Date  `json:"end_date"`
	IsCurrent bool   `json:"is_current"`
}

// Date is context.dev's partial date.
type Date struct {
	Year  int `json:"year"`
	Month int `json:"month"`
	Day   int `json:"day"`
}

// String is the date as YYYY, YYYY-MM or YYYY-MM-DD; "" for none.
func (d *Date) String() string {
	switch {
	case d == nil || d.Year == 0:
		return ""
	case d.Month == 0:
		return fmt.Sprintf("%04d", d.Year)
	case d.Day == 0:
		return fmt.Sprintf("%04d-%02d", d.Year, d.Month)
	}
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Day)
}

// Profile turns a candidate into a catalog row for the entity it was looked
// up for: the LinkedIn URL is the canonical one (the slug sent, when the
// profile lists it), domains are lowercase without www., and the newest
// position per domain, of current_role and experience, is kept, at most
// MaxExperience, numbered from 1. context.dev may list the current position
// only as current_role. Positions without a domain are dropped: nothing
// links to them.
func (p *Person) Profile(score int, e Entry, c Call, fetched time.Time) *Profile {
	row := &Profile{
		ClueEmail: e.Email, ClueLinkedInURL: e.LinkedInURL, ClueName: e.Name,
		FullName: p.Name.Full, FirstName: p.Name.First, LastName: p.Name.Last, CurrentRoleStatus: p.CurrentRoleStatus,
		Location: p.Location.Display, LocationCity: p.Location.City, LocationRegion: p.Location.Region,
		LocationCountry: p.Location.Country, Score: score, LastUpdated: p.LastUpdated, CheckedAt: p.CheckedAt,
		FetchedAt: fetched.UTC().Format(time.RFC3339), CallID: c.CallID, ProviderRequestID: c.RequestID,
	}
	switch {
	case e.Sent.Email != "" && e.Sent.Slug != "":
		row.MatchedOn = "email+linkedin"
	case e.Sent.Email != "":
		row.MatchedOn = "email"
	default:
		row.MatchedOn = "linkedin"
	}
	if row.FullName == "" {
		row.FullName = strings.TrimSpace(p.Name.First + " " + p.Name.Last)
	}
	if role := p.CurrentRole; role != nil {
		row.CurrentTitle, row.CurrentOrgName, row.CurrentOrgDomain = role.Title, role.Organization.Name, domainOf(role.Organization.Domain)
	}
	var slugs []string
	for _, u := range p.SocialURLs {
		if slug, ok := CanonicalLinkedIn(u); ok {
			slugs = append(slugs, slug)
		}
	}
	switch {
	case e.Sent.Slug != "" && slices.Contains(slugs, e.Sent.Slug):
		row.LinkedInURL = LinkedInURL(e.Sent.Slug)
	case len(slugs) > 0:
		row.LinkedInURL = LinkedInURL(slugs[0])
	}
	roles := slices.Clone(p.Experience)
	if p.CurrentRole != nil {
		roles = append([]Role{*p.CurrentRole}, roles...)
	}
	// Newest first: current positions, then by start date; on a tie,
	// current_role.
	slices.SortStableFunc(roles, func(a, b Role) int {
		switch {
		case a.IsCurrent && !b.IsCurrent:
			return -1
		case b.IsCurrent && !a.IsCurrent:
			return 1
		}
		return strings.Compare(b.StartDate.String(), a.StartDate.String())
	})
	seen := map[string]bool{}
	for _, r := range roles {
		domain := domainOf(r.Organization.Domain)
		if domain == "" || seen[domain] || len(row.Experience) == MaxExperience {
			continue
		}
		seen[domain] = true
		row.Experience = append(row.Experience, Experience{
			ID: len(row.Experience) + 1, Title: r.Title, Organization: Organization{Name: r.Organization.Name, Domain: domain},
			Location: r.Location, StartDate: r.StartDate.String(), EndDate: r.EndDate.String(), IsCurrent: r.IsCurrent,
		})
	}
	if row.LinkedInURL != "" {
		slug, _ := CanonicalLinkedIn(row.LinkedInURL)
		row.ProfileID = "li:" + slug
	} else {
		domains := experienceDomains(row.Experience)
		slices.Sort(domains)
		sum := sha256.Sum256([]byte(strings.Join(append([]string{strings.Join(NameTokens(row.FullName), " ")}, domains...), "\x00")))
		row.ProfileID = "h:" + hex.EncodeToString(sum[:8])
	}
	return row
}

// domainOf lowercases a domain and strips www.
func domainOf(domain string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), "www.")
}

// Client calls context.dev's people enrichment, one lookup at a time per
// call of Do, sharing one Limiter across the replica. The call is the one
// context.dev documents (docs.context.dev/api-reference/people/enrich):
// POST <base>/people/enrich with a Bearer key, a body of only email,
// social_urls, zdr "enabled" (a 403 ZDR_NOT_ENABLED unless Helix's
// organization has zero data retention), tags (at most 20, each 1 to 50
// characters) and timeoutOpts.milliseconds (its default behavior, fail,
// answers 408 at the deadline); a 200 is match {status candidate, score,
// person} or {status not_found}. context.dev charges 20 credits per
// candidate and nothing for not_found, refusals, 408, 429 or 5xx.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	APIKey  string
	Limiter *Limiter
	// Sleep waits d or until ctx ends; Jitter adds to a 429's wait.
	Sleep  func(ctx context.Context, d time.Duration) error
	Jitter func(d time.Duration) time.Duration
	Now    func() time.Time
}

// NewClient is a client of cfg's provider: context.dev, or the fake, which
// answers in process. context.dev's connections stay open between calls, one
// per slot of the Limiter: http.DefaultTransport keeps two per host, so at
// Concurrency calls in flight every other answer would close its connection
// and the next call would dial and handshake again.
func NewClient(cfg config.EnrichmentConfig) *Client {
	var transport http.RoundTripper = Fake{}
	if cfg.Provider != config.EnrichmentFake {
		pooled := http.DefaultTransport.(*http.Transport).Clone()
		pooled.MaxIdleConnsPerHost = cfg.Concurrency
		transport = pooled
	}
	return &Client{
		HTTP: &http.Client{Transport: transport, Timeout: config.EnrichmentCallTimeout}, BaseURL: cfg.BaseURL, APIKey: cfg.APIKey,
		Limiter: NewLimiter(cfg.Concurrency), Sleep: sleep, Now: time.Now,
		Jitter: func(d time.Duration) time.Duration { return time.Duration(rand.Int64N(int64(d/10) + 1)) },
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Do looks one person up, unless unsent says why it may not be sent now
// ("" when it may): a lookup it may not send, or send again after a 429,
// ends Unreached, and one whose 408 or 5xx it may not send again keeps that
// answer; either way the reason is its Error. unsent is asked again once a
// slot is free, just before each request, so a step's lease fence holds
// however long the slot took. A 429 waits its Retry-After (at most 60s) and
// a little more, then goes again, as often as it takes; a 408 or 5xx goes
// once more after 2s. A timeout or a network error is never sent again: it
// may have been charged.
//
// ctx bounds the waits (for a slot, and before sending again): once it ends
// no request starts (its error is the reason, if unsent gives none). A
// request already sent runs to its answer or the client's timeout whatever
// ctx does: cut short by Helix, it would be a call context.dev may have
// answered and charged, booked as unanswered.
func (c *Client) Do(ctx context.Context, unsent func() string, l Lookup) Call {
	request := map[string]any{
		"zdr": "enabled", "tags": []string{"foundry", l.LoadID, l.CallID},
		"timeoutOpts": map[string]any{"milliseconds": providerTimeout.Milliseconds()},
	}
	if l.Sent.Email != "" {
		request["email"] = l.Sent.Email
	}
	if l.Sent.Slug != "" {
		request["social_urls"] = []string{LinkedInURL(l.Sent.Slug)}
	}
	raw, _ := json.Marshal(request) // a map of strings and numbers always encodes
	call := Call{CallID: l.CallID, Entity: l.Entity, Request: raw}
	why := func() string {
		if reason := unsent(); reason != "" {
			return reason
		}
		if err := ctx.Err(); err != nil {
			return "not sent: " + err.Error()
		}
		return ""
	}
	resent := false
	for {
		if reason := why(); reason != "" {
			call.Error = reason
			return call
		}
		if err := c.Limiter.Acquire(ctx); err != nil {
			continue // ctx ended: why says so
		}
		if reason := why(); reason != "" {
			c.Limiter.Release(0)
			call.Error = reason
			return call
		}
		call.Reached, call.Attempts, call.At = true, call.Attempts+1, c.Now().UTC()
		a := c.send(context.WithoutCancel(ctx), raw)
		responses.WithLabelValues(statusLabel(a.status)).Inc()
		call.Status, call.RequestID, call.Body, call.Error = a.status, a.requestID, a.body, a.failure
		c.Limiter.Release(call.Status)
		var wait time.Duration
		switch {
		case call.Status == http.StatusTooManyRequests:
			call.Reached = false
			wait = a.retryAfter + c.Jitter(a.retryAfter)
		case (call.Status == http.StatusRequestTimeout || call.Status >= 500) && !resent:
			resent, wait = true, serverRetry
		default:
			return call
		}
		_ = c.Sleep(ctx, wait) // a wait ctx cut short is seen next
	}
}

// answer is one request's response; status 0 is none.
type answer struct {
	status     int
	requestID  string
	body       string
	failure    string
	retryAfter time.Duration
}

// send posts one request.
func (c *Client) send(ctx context.Context, body []byte) answer {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(c.BaseURL, "/")+"/people/enrich", bytes.NewReader(body))
	if err != nil {
		return answer{failure: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return answer{failure: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }() // best-effort: read in full below
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		// The answer was cut off: what it held is unknown, as with a timeout.
		return answer{requestID: resp.Header.Get("X-Request-Id"), failure: err.Error()}
	}
	// context.dev answers with its request id in the body, error or not.
	var ids struct {
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal(raw, &ids) // best-effort: a body that isn't JSON has none, and Judge reads it again
	return answer{
		status: resp.StatusCode, requestID: cmp.Or(ids.RequestID, resp.Header.Get("X-Request-Id")), body: string(raw),
		retryAfter: retryAfter(resp.Header.Get("Retry-After")),
	}
}

// retryAfter is a 429's wait, from its Retry-After seconds, at most
// maxRetryAfter; a missing or unreadable one waits a second.
func retryAfter(header string) time.Duration {
	secs, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || secs < 1 {
		return time.Second
	}
	return min(time.Duration(secs)*time.Second, maxRetryAfter)
}

// Limiter bounds a replica's calls in flight: it starts at max, halves on a
// 429 (never below 4) and grows by one every 20 answered calls, up to max.
type Limiter struct {
	mu        sync.Mutex
	limit     int
	max       int
	inUse     int
	successes int
	changed   chan struct{}
}

// minLimit is the least a 429 halves the limit to.
const minLimit = 4

// NewLimiter starts at max calls at once.
func NewLimiter(max int) *Limiter {
	return &Limiter{limit: max, max: max, changed: make(chan struct{})}
}

// Acquire waits for a slot, or for ctx to end.
func (l *Limiter) Acquire(ctx context.Context) error {
	for {
		l.mu.Lock()
		if l.inUse < l.limit {
			l.inUse++
			l.mu.Unlock()
			return nil
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Release frees a slot and adjusts the limit to the call's status.
func (l *Limiter) Release(status int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inUse--
	switch {
	case status == http.StatusTooManyRequests:
		l.limit, l.successes = max(l.limit/2, min(minLimit, l.max)), 0
	case status >= 200 && status < 300:
		l.successes++
		if l.successes >= 20 {
			l.limit, l.successes = min(l.limit+1, l.max), 0
		}
	}
	close(l.changed)
	l.changed = make(chan struct{})
}

// Limit is the current limit.
func (l *Limiter) Limit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

// Fake answers context.dev's people enrichment in process, from the clue's
// text, so dev runs the whole flow and no person's data leaves Helix:
//   - "bad" is not found, "low" scores 72, "dup" shares one profile with
//     every other "dup", "other" is someone else (Bob Smith), "partial" is
//     an unfinished answer without roles, and "timeout" never answers;
//   - a free-mail address is refused (422), a role mailbox too (400);
//   - anyone else scores 96: named from the email's local part or the slug,
//     working at the email's domain, which, as context.dev may, it lists
//     only as current_role, with an earlier employer in experience.
type Fake struct{}

// fakeTimeout is the network error a "timeout" clue gets.
type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "fake context.dev: timeout" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return true }

// RoundTrip answers one request.
func (Fake) RoundTrip(req *http.Request) (*http.Response, error) {
	var in struct {
		Email      string   `json:"email"`
		SocialURLs []string `json:"social_urls"`
	}
	raw, err := io.ReadAll(req.Body)
	_ = req.Body.Close() // best-effort: read in full
	if err != nil || json.Unmarshal(raw, &in) != nil {
		return fakeAnswer(req, http.StatusBadRequest, fakeRefusal("INVALID_REQUEST")), nil
	}
	slug := ""
	if len(in.SocialURLs) > 0 {
		slug, _ = CanonicalLinkedIn(in.SocialURLs[0])
	}
	local, domain, _ := strings.Cut(in.Email, "@")
	clue := in.Email + " " + slug
	switch {
	case in.Email == "" && slug == "":
		return fakeAnswer(req, http.StatusBadRequest, fakeRefusal("INSUFFICIENT_CLUES")), nil
	case strings.Contains(clue, "timeout"):
		return nil, fakeTimeout{}
	case in.Email != "" && FreeMail(domain):
		return fakeAnswer(req, http.StatusUnprocessableEntity, fakeRefusal("FREE_EMAIL_DETECTED")), nil
	case in.Email != "" && RoleMailbox(local):
		return fakeAnswer(req, http.StatusBadRequest, fakeRefusal("INSUFFICIENT_CLUES")), nil
	case strings.Contains(clue, "bad"):
		return fakeAnswer(req, http.StatusOK, `{"match":{"status":"not_found","score":null,"person":null},"partial":false,`+
			`"key_metadata":{"credits_consumed":0,"credits_remaining":100000},"request_id":"fake"}`), nil
	}
	name := local
	if name == "" {
		name = slug
	}
	var tokens []string
	for _, t := range strings.FieldsFunc(name, func(r rune) bool { return r == '.' || r == '-' || r == '_' }) {
		if !slices.Contains([]string{"bad", "low", "dup", "other", "partial", "timeout"}, t) && t != "" {
			tokens = append(tokens, strings.ToUpper(t[:1])+t[1:])
		}
	}
	if len(tokens) < 2 {
		tokens = append(tokens, "Person")
	}
	first, last := tokens[0], tokens[len(tokens)-1]
	score := 96
	if strings.Contains(clue, "low") {
		score = 72
	}
	if strings.Contains(clue, "other") {
		first, last = "Bob", "Smith"
	}
	profileSlug := strings.ToLower(first + "-" + last)
	switch {
	case strings.Contains(clue, "dup"):
		profileSlug = "shared-profile"
	case slug != "":
		profileSlug = slug
	}
	if domain == "" {
		domain = "example.com"
	}
	org := strings.ToUpper(domain[:1]) + strings.SplitN(domain, ".", 2)[0][1:]
	person := map[string]any{
		"name":       map[string]string{"full": first + " " + last, "first": first, "last": last},
		"email":      in.Email,
		"avatar_url": "https://img.example/fake.png",
		"location": map[string]string{
			"display": "London, United Kingdom", "city": "London", "region": "England", "country": "United Kingdom", "country_code": "GBR",
		},
		"social_urls": []string{"https://www.linkedin.com/in/" + profileSlug},
		"current_role": map[string]any{
			"title": "VP Engineering", "organization": map[string]string{"name": org, "domain": domain},
			"start_date": map[string]int{"year": 2022, "month": 3}, "is_current": true,
		},
		"current_role_status": "present",
		"experience": []map[string]any{
			{"title": "Engineer", "organization": map[string]string{"name": "Initech", "domain": "www.initech.com"},
				"start_date": map[string]int{"year": 2015}, "end_date": map[string]int{"year": 2022, "month": 2}, "is_current": false},
		},
		"website_urls": []string{}, "education": []any{}, "skills": []string{},
		"last_updated": "2026-09-01T00:00:00Z", "checked_at": "2026-10-01T00:00:00Z",
	}
	partial := strings.Contains(clue, "partial")
	if partial {
		delete(person, "current_role")
		delete(person, "experience")
		person["current_role_status"] = "unknown"
	}
	body, _ := json.Marshal(map[string]any{ // maps of strings and numbers always encode
		"match": map[string]any{"status": "candidate", "score": score, "person": person}, "request_id": "fake", "partial": partial,
		"key_metadata": map[string]int{"credits_consumed": model.FoundryEnrichmentCredits, "credits_remaining": 100000},
	})
	return fakeAnswer(req, http.StatusOK, string(body)), nil
}

// fakeRefusal is context.dev's error body (ErrorResponse) for code.
func fakeRefusal(code string) string {
	return `{"message":"refused by the fake","error_code":"` + code + `","key_metadata":{"credits_consumed":0,"credits_remaining":100000},"request_id":"fake"}`
}

func fakeAnswer(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Request: req,
		Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)),
	}
}
