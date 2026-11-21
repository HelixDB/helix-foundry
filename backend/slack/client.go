package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// DefaultBaseURL is the Web API's.
const DefaultBaseURL = "https://slack.com/api"

// maxAnswer is the most of an answer the client reads.
const maxAnswer = 1 << 20

// defaultRetryAfter is the wait a 429 without a usable Retry-After gets.
const defaultRetryAfter = 30 * time.Second

var apiCalls = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_slack_api_calls_total",
	Help: "Slack Web API calls by method and outcome: ok, error (Slack's or HTTP), ratelimited (Slack's 429), deferred (the limiter's, not sent) or unreachable.",
}, []string{"method", "outcome"})

// Bot is the install a call is made for, and its bot token.
type Bot struct {
	InstallID string
	Token     string
}

// APIError is a Web API answer that isn't ok: Slack's error code, or an
// HTTP status other than 200 (Code empty).
type APIError struct {
	Method string
	Code   string
	// Needed is the scope a missing_scope answer names.
	Needed string
	Status int
}

// Error says the method and Slack's code, or the HTTP status.
func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("slack: %s: HTTP %d", e.Method, e.Status)
	}
	return fmt.Sprintf("slack: %s: %s", e.Method, e.Code)
}

// RateLimitedError is a call that may not go yet: Slack answered 429 (or
// ratelimited), or the Limiter held it back without sending it (Deferred).
// The caller defers its work by RetryAfter; nothing sleeps.
type RateLimitedError struct {
	Method     string
	RetryAfter time.Duration
	Deferred   bool
}

// Error says the method and how long to wait.
func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("slack: %s: rate limited for %s", e.Method, e.RetryAfter)
}

// IsCode reports whether err is an APIError with one of codes.
func IsCode(err error, codes ...string) bool {
	var api *APIError
	return errors.As(err, &api) && slices.Contains(codes, api.Code)
}

// ClientOptions configure a Client.
type ClientOptions struct {
	// BaseURL is the Web API's root; DefaultBaseURL when empty. Tests point
	// it at a fake Slack.
	BaseURL string
	// HTTP sends the calls; a client with a 10-second timeout when nil.
	HTTP *http.Client
	// Limiter paces the calls; required.
	Limiter *Limiter
	// Now is the clock the Limiter reads; time.Now when nil.
	Now func() time.Time
}

// Client calls the Slack Web API: every call through its Limiter, every
// message with parse=none and no unfurls.
type Client struct {
	base    string
	http    *http.Client
	limiter *Limiter
	now     func() time.Time
}

// NewClient is a Client.
func NewClient(o ClientOptions) (*Client, error) {
	if o.Limiter == nil {
		return nil, errors.New("slack: a client needs a limiter")
	}
	base := strings.TrimRight(o.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	if u, err := url.Parse(base); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("slack: base URL %q", o.BaseURL)
	}
	c := &Client{base: base, http: o.HTTP, limiter: o.Limiter, now: o.Now}
	if c.http == nil {
		c.http = &http.Client{Timeout: 10 * time.Second}
	}
	if c.now == nil {
		c.now = time.Now
	}
	return c, nil
}

// request is one call: a JSON body (args) or a form (form), with the
// bot's token or the app's client credentials.
type request struct {
	method  Method
	bot     Bot
	channel string
	args    any
	form    url.Values
	// basic is the client id and secret, sent as HTTP Basic auth.
	basic *[2]string
}

// call makes r and decodes an ok answer into out (when not nil).
func (c *Client) call(ctx context.Context, r request, out any) error {
	name := r.method.name
	if r.bot.InstallID != "" {
		if wait := c.limiter.Take(r.bot.InstallID, r.method, r.channel, c.now()); wait > 0 {
			apiCalls.WithLabelValues(name, "deferred").Inc()
			return &RateLimitedError{Method: name, RetryAfter: wait, Deferred: true}
		}
	}
	var (
		body        []byte
		contentType string
	)
	if r.method.form {
		body, contentType = []byte(r.form.Encode()), "application/x-www-form-urlencoded"
	} else {
		var err error
		if body, err = json.Marshal(r.args); err != nil {
			return fmt.Errorf("slack: %s: encode: %w", name, err)
		}
		contentType = "application/json; charset=utf-8"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+name, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("slack: %s: %w", name, err)
	}
	req.Header.Set("Content-Type", contentType)
	switch {
	case r.basic != nil:
		req.SetBasicAuth(r.basic[0], r.basic[1])
	case r.bot.Token != "":
		req.Header.Set("Authorization", "Bearer "+r.bot.Token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		apiCalls.WithLabelValues(name, "unreachable").Inc()
		return fmt.Errorf("slack: %s: %w", name, err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		apiCalls.WithLabelValues(name, "unreachable").Inc()
		return fmt.Errorf("slack: %s: read: %w", name, err)
	}
	var status struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Needed string `json:"needed"`
	}
	decodeErr := json.Unmarshal(answer, &status)
	if resp.StatusCode == http.StatusTooManyRequests || (decodeErr == nil && status.Error == "ratelimited") {
		wait := defaultRetryAfter
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
			wait = time.Duration(seconds) * time.Second
		}
		if r.bot.InstallID != "" {
			c.limiter.Block(r.bot.InstallID, r.method, c.now(), wait)
		}
		apiCalls.WithLabelValues(name, "ratelimited").Inc()
		return &RateLimitedError{Method: name, RetryAfter: wait}
	}
	switch {
	case resp.StatusCode != http.StatusOK:
		apiCalls.WithLabelValues(name, "error").Inc()
		return &APIError{Method: name, Status: resp.StatusCode}
	case decodeErr != nil:
		apiCalls.WithLabelValues(name, "error").Inc()
		return fmt.Errorf("slack: %s: decode: %w", name, decodeErr)
	case !status.OK:
		apiCalls.WithLabelValues(name, "error").Inc()
		code := status.Error
		if code == "" {
			code = "unknown_error"
		}
		return &APIError{Method: name, Code: code, Needed: status.Needed, Status: resp.StatusCode}
	}
	apiCalls.WithLabelValues(name, "ok").Inc()
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(answer, out); err != nil {
		return fmt.Errorf("slack: %s: decode: %w", name, err)
	}
	return nil
}

// messageArgs are the arguments every message the bot sends carries: Slack
// reads no formatting the text didn't write, links nothing and unfurls
// nothing.
type messageArgs struct {
	Channel     string  `json:"channel"`
	TS          string  `json:"ts,omitempty"`
	ThreadTS    string  `json:"thread_ts,omitempty"`
	User        string  `json:"user,omitempty"`
	Text        string  `json:"text"`
	Blocks      []Block `json:"blocks"`
	Parse       string  `json:"parse"`
	UnfurlLinks bool    `json:"unfurl_links"`
	UnfurlMedia bool    `json:"unfurl_media"`
}

func newMessageArgs(channel string, m Message) (messageArgs, error) {
	if err := m.Check(); err != nil {
		return messageArgs{}, err
	}
	if !idRE.MatchString(channel) {
		return messageArgs{}, fmt.Errorf("slack: channel %q", channel)
	}
	blocks := m.Blocks
	if blocks == nil {
		blocks = []Block{}
	}
	return messageArgs{Channel: channel, Text: m.Text.s, Blocks: blocks, Parse: "none"}, nil
}

// Post is a message to post in a channel, in a thread when ThreadTS is set.
type Post struct {
	Channel  string
	ThreadTS string
	Message  Message
}

// PostMessage posts p (chat.postMessage) and returns the message's ts.
func (c *Client) PostMessage(ctx context.Context, bot Bot, p Post) (string, error) {
	args, err := newMessageArgs(p.Channel, p.Message)
	if err != nil {
		return "", err
	}
	args.ThreadTS = p.ThreadTS
	var out struct {
		TS string `json:"ts"`
	}
	if err := c.call(ctx, request{method: ChatPostMessage, bot: bot, channel: p.Channel, args: args}, &out); err != nil {
		return "", err
	}
	if !tsRE.MatchString(out.TS) {
		return "", fmt.Errorf("slack: chat.postMessage answered without a ts")
	}
	return out.TS, nil
}

// Update is a new version of a message the bot posted.
type Update struct {
	Channel string
	TS      string
	Message Message
}

// UpdateMessage replaces the message u names (chat.update).
func (c *Client) UpdateMessage(ctx context.Context, bot Bot, u Update) error {
	args, err := newMessageArgs(u.Channel, u.Message)
	if err != nil {
		return err
	}
	if !tsRE.MatchString(u.TS) {
		return fmt.Errorf("slack: chat.update of ts %q", u.TS)
	}
	args.TS = u.TS
	return c.call(ctx, request{method: ChatUpdate, bot: bot, channel: u.Channel, args: args}, nil)
}

// Ephemeral is a message only User sees, in Channel (and ThreadTS's thread).
type Ephemeral struct {
	Channel  string
	User     string
	ThreadTS string
	Message  Message
}

// PostEphemeral sends e (chat.postEphemeral). Slack shows it only to a user
// active in the channel, and it can't be updated.
func (c *Client) PostEphemeral(ctx context.Context, bot Bot, e Ephemeral) error {
	args, err := newMessageArgs(e.Channel, e.Message)
	if err != nil {
		return err
	}
	if !idRE.MatchString(e.User) {
		return fmt.Errorf("slack: chat.postEphemeral to user %q", e.User)
	}
	args.User, args.ThreadTS = e.User, e.ThreadTS
	return c.call(ctx, request{method: ChatPostEphemeral, bot: bot, channel: e.Channel, args: args}, nil)
}

// User is what users.info says of a Slack user.
type User struct {
	ID       string
	TeamID   string
	Name     string // the handle
	RealName string
	// DisplayName is the profile's display name, "" when unset.
	DisplayName string
	TZ          string
	// Email is the profile's email (users:read.email), "" when hidden.
	Email          string
	EmailConfirmed bool
	IsBot          bool
	Deleted        bool
	// Restricted is a multi-channel guest, UltraRestricted a single-channel one.
	Restricted      bool
	UltraRestricted bool
	// Stranger is a user of another organisation, seen through a shared channel.
	Stranger bool
	// EnterpriseID and EnterpriseTeams are the Grid org the user is in and
	// the org's workspaces they belong to; empty outside Grid.
	EnterpriseID    string
	EnterpriseTeams []string
}

// Shown is how the user's name shows: the display name, else the real
// name, else the handle, else the id.
func (u User) Shown() string {
	for _, name := range []string{u.DisplayName, u.RealName, u.Name} {
		if name = strings.TrimSpace(name); name != "" {
			return name
		}
	}
	return u.ID
}

// UserInfo reads a user (users.info).
func (c *Client) UserInfo(ctx context.Context, bot Bot, userID string) (User, error) {
	if !idRE.MatchString(userID) {
		return User{}, fmt.Errorf("slack: users.info of %q", userID)
	}
	var out struct {
		User struct {
			ID                string `json:"id"`
			TeamID            string `json:"team_id"`
			Name              string `json:"name"`
			RealName          string `json:"real_name"`
			TZ                string `json:"tz"`
			IsBot             bool   `json:"is_bot"`
			Deleted           bool   `json:"deleted"`
			IsRestricted      bool   `json:"is_restricted"`
			IsUltraRestricted bool   `json:"is_ultra_restricted"`
			IsStranger        bool   `json:"is_stranger"`
			IsEmailConfirmed  bool   `json:"is_email_confirmed"`
			Profile           struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
				Email       string `json:"email"`
			} `json:"profile"`
			EnterpriseUser *struct {
				EnterpriseID string   `json:"enterprise_id"`
				Teams        []string `json:"teams"`
			} `json:"enterprise_user"`
		} `json:"user"`
	}
	form := url.Values{"user": {userID}}
	if err := c.call(ctx, request{method: UsersInfo, bot: bot, form: form}, &out); err != nil {
		return User{}, err
	}
	u := out.User
	if u.ID != userID {
		return User{}, fmt.Errorf("slack: users.info answered for %q, not %q", u.ID, userID)
	}
	user := User{
		ID: u.ID, TeamID: u.TeamID, Name: u.Name, RealName: u.RealName, DisplayName: u.Profile.DisplayName, TZ: u.TZ,
		Email: u.Profile.Email, EmailConfirmed: u.IsEmailConfirmed, IsBot: u.IsBot, Deleted: u.Deleted,
		Restricted: u.IsRestricted, UltraRestricted: u.IsUltraRestricted, Stranger: u.IsStranger,
	}
	if user.RealName == "" {
		user.RealName = u.Profile.RealName
	}
	if u.EnterpriseUser != nil {
		user.EnterpriseID, user.EnterpriseTeams = u.EnterpriseUser.EnterpriseID, u.EnterpriseUser.Teams
	}
	return user, nil
}

// OpenDM opens the bot's direct message with a user (conversations.open)
// and returns its channel.
func (c *Client) OpenDM(ctx context.Context, bot Bot, userID string) (string, error) {
	if !idRE.MatchString(userID) {
		return "", fmt.Errorf("slack: conversations.open with %q", userID)
	}
	var out struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	args := map[string]string{"users": userID}
	if err := c.call(ctx, request{method: ConversationsOpen, bot: bot, args: args}, &out); err != nil {
		return "", err
	}
	if !idRE.MatchString(out.Channel.ID) {
		return "", errors.New("slack: conversations.open answered without a channel")
	}
	return out.Channel.ID, nil
}

// AgentStatus is an agent session's status: a closed set.
type AgentStatus struct{ s string }

// The statuses an agent session takes.
var (
	// AgentProcessing shows the agent working (Slack drops it to active
	// after an hour).
	AgentProcessing = AgentStatus{"processing"}
	// AgentActive is a session waiting on its person.
	AgentActive = AgentStatus{"active"}
	// AgentSuspended is a session waiting on a card or questions.
	AgentSuspended = AgentStatus{"suspended"}
	// AgentClosed is a session ended.
	AgentClosed = AgentStatus{"closed"}
)

// String is the status as Slack names it.
func (s AgentStatus) String() string { return s.s }

// SetAgentStatus sets the agent session of a thread
// (agents.sessions.setStatus), making it when there is none.
// feature_disabled (a plan without agents) and missing_scope come back as
// an APIError like any other.
func (c *Client) SetAgentStatus(ctx context.Context, bot Bot, channel, threadTS string, status AgentStatus) error {
	if status.s == "" || !idRE.MatchString(channel) || !tsRE.MatchString(threadTS) {
		return errors.New("slack: agents.sessions.setStatus needs a status, a channel and a thread")
	}
	args := map[string]string{"channel_id": channel, "thread_ts": threadTS, "status": status.s}
	return c.call(ctx, request{method: AgentSessionSetStatus, bot: bot, channel: channel, args: args}, nil)
}

// Grant is what oauth.v2.access gives for an install.
type Grant struct {
	AppID     string
	TeamID    string
	TeamName  string
	BotUserID string
	BotToken  string
	// Scopes are the bot's scopes, as granted.
	Scopes []string
	// EnterpriseID is the Grid org the team is in, "" outside one.
	EnterpriseID string
	// EnterpriseInstall is an org-wide install, which Foundry refuses.
	EnterpriseInstall bool
	// InstallerID is the Slack user who installed the app.
	InstallerID string
}

// OAuthAccess exchanges an install's code for its bot token
// (oauth.v2.access), with the app's credentials as HTTP Basic auth.
func (c *Client) OAuthAccess(ctx context.Context, clientID, clientSecret, code, redirectURI string) (Grant, error) {
	var out struct {
		AppID       string `json:"app_id"`
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		BotUserID   string `json:"bot_user_id"`
		Team        *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"team"`
		Enterprise *struct {
			ID string `json:"id"`
		} `json:"enterprise"`
		IsEnterpriseInstall bool `json:"is_enterprise_install"`
		AuthedUser          struct {
			ID string `json:"id"`
		} `json:"authed_user"`
	}
	form := url.Values{"code": {code}, "redirect_uri": {redirectURI}, "grant_type": {"authorization_code"}}
	r := request{method: OAuthV2Access, form: form, basic: &[2]string{clientID, clientSecret}}
	if err := c.call(ctx, r, &out); err != nil {
		return Grant{}, err
	}
	g := Grant{
		AppID: out.AppID, BotUserID: out.BotUserID, BotToken: out.AccessToken,
		EnterpriseInstall: out.IsEnterpriseInstall, InstallerID: out.AuthedUser.ID,
	}
	for scope := range strings.SplitSeq(out.Scope, ",") {
		if scope = strings.TrimSpace(scope); scope != "" {
			g.Scopes = append(g.Scopes, scope)
		}
	}
	if out.Team != nil {
		g.TeamID, g.TeamName = out.Team.ID, out.Team.Name
	}
	if out.Enterprise != nil {
		g.EnterpriseID = out.Enterprise.ID
	}
	switch {
	case g.EnterpriseInstall:
		return g, nil // the caller refuses it; the team may be null
	case out.TokenType != "bot" || !strings.HasPrefix(g.BotToken, "xoxb-"):
		return Grant{}, errors.New("slack: oauth.v2.access gave no bot token")
	case !idRE.MatchString(g.AppID) || !idRE.MatchString(g.TeamID) || !idRE.MatchString(g.BotUserID) ||
		!idRE.MatchString(g.InstallerID) || (g.EnterpriseID != "" && !idRE.MatchString(g.EnterpriseID)):
		return Grant{}, errors.New("slack: oauth.v2.access answered without its app, team, bot or installer")
	}
	return g, nil
}

// Uninstall removes the app from an install's team (apps.uninstall), with
// the app's credentials and the install's token.
func (c *Client) Uninstall(ctx context.Context, bot Bot, clientID, clientSecret string) error {
	form := url.Values{"client_id": {clientID}, "client_secret": {clientSecret}}
	return c.call(ctx, request{method: AppsUninstall, bot: bot, form: form}, nil)
}
