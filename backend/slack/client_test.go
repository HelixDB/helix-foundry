package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/slack"
)

// fakeSlack is a Web API that records each call and answers what the test
// says for its method.
type fakeSlack struct {
	t       *testing.T
	mu      sync.Mutex
	calls   []call
	answers map[string]func(w http.ResponseWriter)
}

type call struct {
	method      string
	contentType string
	auth        string
	json        map[string]any
	form        url.Values
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	require.Equal(f.t, http.MethodPost, r.Method)
	body, err := io.ReadAll(r.Body)
	require.NoError(f.t, err)
	c := call{method: r.URL.Path[len("/api/"):], contentType: r.Header.Get("Content-Type"), auth: r.Header.Get("Authorization")}
	if c.contentType == "application/x-www-form-urlencoded" {
		c.form, err = url.ParseQuery(string(body))
	} else {
		err = json.Unmarshal(body, &c.json)
	}
	require.NoError(f.t, err)
	f.mu.Lock()
	f.calls = append(f.calls, c)
	answer := f.answers[c.method]
	f.mu.Unlock()
	if answer == nil {
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	}
	answer(w)
}

func (f *fakeSlack) last() call {
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(f.t, f.calls)
	return f.calls[len(f.calls)-1]
}

func (f *fakeSlack) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func answer(status int, body string, header ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		for i := 0; i+1 < len(header); i += 2 {
			w.Header().Set(header[i], header[i+1])
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// newFake is a fake Slack and a client of it, its limiter at full share
// with a clock the test moves.
func newFake(t *testing.T) (*fakeSlack, *slack.Client, *time.Time) {
	fake := &fakeSlack{t: t, answers: map[string]func(http.ResponseWriter){}}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	now := t0
	client, err := slack.NewClient(slack.ClientOptions{BaseURL: server.URL + "/api/", HTTP: server.Client(), Limiter: newLimiter(t, 1), Now: func() time.Time { return now }})
	require.NoError(t, err)
	return fake, client, &now
}

var bot = slack.Bot{InstallID: "I1", Token: "xoxb-1"}

func TestNewClient(t *testing.T) {
	_, err := slack.NewClient(slack.ClientOptions{})
	require.Error(t, err)
	for _, bad := range []string{"ftp://slack.com/api", "slack.com/api", "https://%zz"} {
		_, err := slack.NewClient(slack.ClientOptions{BaseURL: bad, Limiter: newLimiter(t, 1)})
		require.Error(t, err, bad)
	}
	c, err := slack.NewClient(slack.ClientOptions{Limiter: newLimiter(t, 1)})
	require.NoError(t, err)
	require.NotNil(t, c)
}

// Every message goes as JSON with the bot's token, parse=none and no
// unfurls, and its blocks verbatim.
func TestMessages(t *testing.T) {
	fake, client, now := newFake(t)
	ctx := context.Background()
	m := card(t)
	fake.answers["chat.postMessage"] = answer(200, `{"ok":true,"channel":"C1","ts":"1795000200.000100"}`)
	ts, err := client.PostMessage(ctx, bot, slack.Post{Channel: "C1", ThreadTS: "1795000000.000100", Message: m})
	require.NoError(t, err)
	require.Equal(t, "1795000200.000100", ts)
	c := fake.last()
	require.Equal(t, "chat.postMessage", c.method)
	require.Equal(t, "application/json; charset=utf-8", c.contentType)
	require.Equal(t, "Bearer xoxb-1", c.auth)
	require.Equal(t, "none", c.json["parse"])
	require.Equal(t, false, c.json["unfurl_links"])
	require.Equal(t, false, c.json["unfurl_media"])
	require.Equal(t, "1795000000.000100", c.json["thread_ts"])
	require.Equal(t, "Delete the dashboard Revenue?", c.json["text"])
	require.NotContains(t, c.json, "link_names")
	blocks, err := json.Marshal(c.json["blocks"])
	require.NoError(t, err)
	requireVerbatim(t, blocks)

	*now = now.Add(time.Minute)
	require.NoError(t, client.UpdateMessage(ctx, bot, slack.Update{Channel: "C1", TS: ts, Message: m}))
	c = fake.last()
	require.Equal(t, "chat.update", c.method)
	require.Equal(t, ts, c.json["ts"])
	require.Equal(t, "none", c.json["parse"])
	require.Equal(t, false, c.json["unfurl_links"])

	require.NoError(t, client.PostEphemeral(ctx, bot, slack.Ephemeral{Channel: "C2", User: "U1", Message: slack.Message{Text: site(t).Text("Link your account")}}))
	c = fake.last()
	require.Equal(t, "chat.postEphemeral", c.method)
	require.Equal(t, "U1", c.json["user"])
	require.Equal(t, "none", c.json["parse"])
	require.Equal(t, []any{}, c.json["blocks"])
	require.NotContains(t, c.json, "thread_ts")

	// What Slack would refuse is never sent.
	calls := fake.count()
	_, err = client.PostMessage(ctx, bot, slack.Post{Channel: "C1", Message: slack.Message{}})
	require.Error(t, err)
	_, err = client.PostMessage(ctx, bot, slack.Post{Channel: "c1", Message: m})
	require.Error(t, err)
	require.Error(t, client.UpdateMessage(ctx, bot, slack.Update{Channel: "C1", TS: "x", Message: m}))
	require.Error(t, client.UpdateMessage(ctx, bot, slack.Update{Channel: "C1", TS: ts}))
	require.Error(t, client.PostEphemeral(ctx, bot, slack.Ephemeral{Channel: "C1", User: "u", Message: m}))
	require.Error(t, client.PostEphemeral(ctx, bot, slack.Ephemeral{Channel: "C1", User: "U1"}))
	require.Equal(t, calls, fake.count())

	// An answer without a ts is an error.
	fake.answers["chat.postMessage"] = answer(200, `{"ok":true}`)
	_, err = client.PostMessage(ctx, bot, slack.Post{Channel: "C3", Message: m})
	require.Error(t, err)
	fake.answers["chat.postMessage"] = answer(200, `{"ok":false,"error":"channel_not_found"}`)
	_, err = client.PostMessage(ctx, bot, slack.Post{Channel: "C4", Message: m})
	require.True(t, slack.IsCode(err, "channel_not_found"))
}

// Slack's 429 (or ratelimited) is a RateLimitedError with its Retry-After,
// and holds the method for that long without calling Slack.
func TestRateLimited(t *testing.T) {
	fake, client, now := newFake(t)
	ctx := context.Background()
	fake.answers["chat.update"] = answer(429, ``, "Retry-After", "7")
	err := client.UpdateMessage(ctx, bot, slack.Update{Channel: "C1", TS: "1.2", Message: card(t)})
	var limited *slack.RateLimitedError
	require.ErrorAs(t, err, &limited)
	require.Equal(t, slack.RateLimitedError{Method: "chat.update", RetryAfter: 7 * time.Second}, *limited)
	require.Contains(t, err.Error(), "rate limited for 7s")

	calls := fake.count()
	*now = now.Add(5 * time.Second)
	err = client.UpdateMessage(ctx, bot, slack.Update{Channel: "C1", TS: "1.2", Message: card(t)})
	require.ErrorAs(t, err, &limited)
	require.Equal(t, slack.RateLimitedError{Method: "chat.update", RetryAfter: 2 * time.Second, Deferred: true}, *limited)
	require.Equal(t, calls, fake.count())
	// Another install isn't held.
	require.ErrorAs(t, client.UpdateMessage(ctx, slack.Bot{InstallID: "I2", Token: "t"}, slack.Update{Channel: "C1", TS: "1.2", Message: card(t)}), &limited)
	require.Equal(t, calls+1, fake.count())

	// ratelimited in a 200, with a Retry-After that isn't a number: 30 s.
	fake.answers["users.info"] = answer(200, `{"ok":false,"error":"ratelimited"}`, "Retry-After", "soon")
	_, err = client.UserInfo(ctx, bot, "U1")
	require.ErrorAs(t, err, &limited)
	require.Equal(t, 30*time.Second, limited.RetryAfter)
	require.False(t, limited.Deferred)
}

func TestErrors(t *testing.T) {
	fake, client, _ := newFake(t)
	ctx := context.Background()
	for name, c := range map[string]struct {
		answer func(http.ResponseWriter)
		check  func(t *testing.T, err error)
	}{
		"http 500": {answer(500, `oops`), func(t *testing.T, err error) {
			var api *slack.APIError
			require.ErrorAs(t, err, &api)
			require.Equal(t, slack.APIError{Method: "agents.sessions.setStatus", Status: 500}, *api)
			require.Equal(t, "slack: agents.sessions.setStatus: HTTP 500", err.Error())
		}},
		"not json": {answer(200, `<html>`), func(t *testing.T, err error) {
			require.ErrorContains(t, err, "decode")
		}},
		"missing scope": {answer(200, `{"ok":false,"error":"missing_scope","needed":"assistant:write"}`), func(t *testing.T, err error) {
			var api *slack.APIError
			require.ErrorAs(t, err, &api)
			require.Equal(t, "assistant:write", api.Needed)
			require.True(t, slack.IsCode(err, "feature_disabled", "missing_scope"))
			require.False(t, slack.IsCode(err, "not_authorized"))
			require.Equal(t, "slack: agents.sessions.setStatus: missing_scope", err.Error())
		}},
		"no code": {answer(200, `{"ok":false}`), func(t *testing.T, err error) {
			require.True(t, slack.IsCode(err, "unknown_error"))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			fake.answers["agents.sessions.setStatus"] = c.answer
			c.check(t, client.SetAgentStatus(ctx, slack.Bot{InstallID: name, Token: "t"}, "C1", "1.2", slack.AgentProcessing))
		})
	}
	require.False(t, slack.IsCode(errors.New("x"), "x"))

	// Unreachable, and an answer that isn't the method's shape.
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	gone, err := slack.NewClient(slack.ClientOptions{BaseURL: server.URL, Limiter: newLimiter(t, 1)})
	require.NoError(t, err)
	require.ErrorContains(t, gone.SetAgentStatus(ctx, bot, "C1", "1.2", slack.AgentActive), "agents.sessions.setStatus")
	// An answer cut short.
	fake.answers["users.info"] = answer(200, `{"ok":`, "Content-Length", "100")
	_, err = client.UserInfo(ctx, bot, "U1")
	require.ErrorContains(t, err, "users.info: read")
	fake.answers["users.info"] = answer(200, `{"ok":true,"user":"not an object"}`)
	_, err = client.UserInfo(ctx, bot, "U1")
	require.ErrorContains(t, err, "decode")
}

func TestSetAgentStatus(t *testing.T) {
	fake, client, _ := newFake(t)
	ctx := context.Background()
	for _, status := range []slack.AgentStatus{slack.AgentProcessing, slack.AgentActive, slack.AgentSuspended, slack.AgentClosed} {
		require.NoError(t, client.SetAgentStatus(ctx, slack.Bot{InstallID: status.String(), Token: "t"}, "D1", "1.2", status))
		require.Equal(t, map[string]any{"channel_id": "D1", "thread_ts": "1.2", "status": status.String()}, fake.last().json)
	}
	require.Error(t, client.SetAgentStatus(ctx, bot, "D1", "1.2", slack.AgentStatus{}))
	require.Error(t, client.SetAgentStatus(ctx, bot, "d1", "1.2", slack.AgentActive))
	require.Error(t, client.SetAgentStatus(ctx, bot, "D1", "", slack.AgentActive))
}

func TestUserInfo(t *testing.T) {
	fake, client, _ := newFake(t)
	ctx := context.Background()
	fake.answers["users.info"] = answer(200, `{"ok":true,"user":{"id":"W1","team_id":"T1","name":"ada","real_name":"","tz":"Europe/London",
		"is_bot":false,"deleted":false,"is_restricted":true,"is_ultra_restricted":false,"is_stranger":true,"is_email_confirmed":true,
		"profile":{"display_name":"","real_name":"Ada Lovelace","email":"ada@acme.com"},
		"enterprise_user":{"enterprise_id":"E1","teams":["T1","T2"]}}}`)
	u, err := client.UserInfo(ctx, bot, "W1")
	require.NoError(t, err)
	require.Equal(t, slack.User{ID: "W1", TeamID: "T1", Name: "ada", RealName: "Ada Lovelace", TZ: "Europe/London", Email: "ada@acme.com",
		EmailConfirmed: true, Restricted: true, Stranger: true, EnterpriseID: "E1", EnterpriseTeams: []string{"T1", "T2"}}, u)
	require.Equal(t, "Ada Lovelace", u.Shown())
	c := fake.last()
	require.Equal(t, "application/x-www-form-urlencoded", c.contentType)
	require.Equal(t, "Bearer xoxb-1", c.auth)
	require.Equal(t, url.Values{"user": {"W1"}}, c.form)

	require.Equal(t, "ada", slack.User{ID: "U1", Name: "ada"}.Shown())
	require.Equal(t, "Ada", slack.User{ID: "U1", DisplayName: " Ada ", Name: "ada"}.Shown())
	require.Equal(t, "U1", slack.User{ID: "U1"}.Shown())

	_, err = client.UserInfo(ctx, bot, "w1")
	require.Error(t, err)
	_, err = client.UserInfo(ctx, bot, "U2") // answered for W1
	require.Error(t, err)
}

func TestUserInfoAdmin(t *testing.T) {
	fake, client, _ := newFake(t)
	ctx := context.Background()
	for flag, admin := range map[string]bool{"is_admin": true, "is_owner": true, "is_primary_owner": true, "is_restricted": false} {
		fake.answers["users.info"] = answer(200, `{"ok":true,"user":{"id":"U1","team_id":"T1","`+flag+`":true}}`)
		u, err := client.UserInfo(ctx, bot, "U1")
		require.NoError(t, err)
		require.Equal(t, admin, u.Admin, flag)
	}
}

func TestBotID(t *testing.T) {
	fake, client, _ := newFake(t)
	ctx := context.Background()
	fake.answers["auth.test"] = answer(200, `{"ok":true,"team_id":"T1","user_id":"U0BOT","bot_id":"B0FOUNDRY"}`)
	id, err := client.BotID(ctx, bot)
	require.NoError(t, err)
	require.Equal(t, "B0FOUNDRY", id)
	c := fake.last()
	require.Equal(t, "Bearer xoxb-1", c.auth)
	require.Equal(t, "application/x-www-form-urlencoded", c.contentType)
	for _, body := range []string{`{"ok":true}`, `{"ok":true,"bot_id":"b 1"}`} {
		fake.answers["auth.test"] = answer(200, body)
		_, err = client.BotID(ctx, bot)
		require.ErrorContains(t, err, "without a bot id", body)
	}
	fake.answers["auth.test"] = answer(200, `{"ok":false,"error":"invalid_auth"}`)
	_, err = client.BotID(ctx, bot)
	require.True(t, slack.IsCode(err, "invalid_auth"))
}

func TestOpenDM(t *testing.T) {
	fake, client, _ := newFake(t)
	ctx := context.Background()
	fake.answers["conversations.open"] = answer(200, `{"ok":true,"channel":{"id":"D069C7QFK"}}`)
	channel, err := client.OpenDM(ctx, bot, "U1")
	require.NoError(t, err)
	require.Equal(t, "D069C7QFK", channel)
	require.Equal(t, map[string]any{"users": "U1"}, fake.last().json)
	_, err = client.OpenDM(ctx, bot, "")
	require.Error(t, err)
	fake.answers["conversations.open"] = answer(200, `{"ok":false,"error":"user_not_found"}`)
	_, err = client.OpenDM(ctx, slack.Bot{InstallID: "I3", Token: "t"}, "U1")
	require.True(t, slack.IsCode(err, "user_not_found"))
	fake.answers["conversations.open"] = answer(200, `{"ok":true}`)
	_, err = client.OpenDM(ctx, slack.Bot{InstallID: "I2", Token: "t"}, "U1")
	require.Error(t, err)
}

func TestConversationInfo(t *testing.T) {
	fake, client, _ := newFake(t)
	ctx := context.Background()
	for body, want := range map[string]slack.ChannelInfo{
		`{"ok":true,"channel":{"id":"C1","is_ext_shared":false}}`:                              {},
		`{"ok":true,"channel":{"id":"C1","is_shared":true,"is_member":true}}`:                  {Member: true},
		`{"ok":true,"channel":{"id":"C1","is_ext_shared":true}}`:                               {ExtShared: true},
		`{"ok":true,"channel":{"id":"C1","is_ext_shared":false,"is_pending_ext_shared":true}}`: {ExtShared: true},
	} {
		fake.answers["conversations.info"] = answer(200, body)
		info, err := client.ConversationInfo(ctx, slack.Bot{InstallID: "I" + body, Token: "t"}, "C1")
		require.NoError(t, err, body)
		require.Equal(t, want, info, body)
	}
	c := fake.last()
	require.Equal(t, "application/x-www-form-urlencoded", c.contentType)
	require.Equal(t, url.Values{"channel": {"C1"}}, c.form)
	_, err := client.ConversationInfo(ctx, bot, "c 1")
	require.Error(t, err)
	fake.answers["conversations.info"] = answer(200, `{"ok":true,"channel":{"id":"C2"}}`)
	_, err = client.ConversationInfo(ctx, slack.Bot{InstallID: "I4", Token: "t"}, "C1")
	require.Error(t, err, "answered for another channel")
	fake.answers["conversations.info"] = answer(200, `{"ok":false,"error":"missing_scope","needed":"groups:read"}`)
	_, err = client.ConversationInfo(ctx, slack.Bot{InstallID: "I5", Token: "t"}, "G1")
	require.True(t, slack.IsCode(err, "missing_scope"))
}

func TestOAuthAccess(t *testing.T) {
	fake, client, _ := newFake(t)
	ctx := context.Background()
	grant := `{"ok":true,"access_token":"xoxb-test-bot-token","token_type":"bot",
		"scope":"app_mentions:read, im:history,chat:write","bot_user_id":"U0KRQLJ9H","app_id":"A0KRD7HC3",
		"team":{"name":"Slack Softball Team","id":"T9TK3CUKW"},"enterprise":{"name":"slack-sports","id":"E12345678"},
		"authed_user":{"id":"U1234","scope":"","token_type":"user"},"is_enterprise_install":false}`
	fake.answers["oauth.v2.access"] = answer(200, grant)
	g, err := client.OAuthAccess(ctx, "client-id", "client-secret", "code-1", "https://foundry.helix-db.com/slack/installed")
	require.NoError(t, err)
	require.Equal(t, slack.Grant{AppID: "A0KRD7HC3", TeamID: "T9TK3CUKW", TeamName: "Slack Softball Team", BotUserID: "U0KRQLJ9H",
		BotToken: "xoxb-test-bot-token", Scopes: []string{"app_mentions:read", "im:history", "chat:write"},
		EnterpriseID: "E12345678", InstallerID: "U1234"}, g)
	c := fake.last()
	require.Equal(t, "application/x-www-form-urlencoded", c.contentType)
	user, pass, ok := (&http.Request{Header: http.Header{"Authorization": {c.auth}}}).BasicAuth()
	require.True(t, ok)
	require.Equal(t, [2]string{"client-id", "client-secret"}, [2]string{user, pass})
	require.Equal(t, url.Values{"code": {"code-1"}, "redirect_uri": {"https://foundry.helix-db.com/slack/installed"}, "grant_type": {"authorization_code"}}, c.form)

	// An org-wide install comes back for the caller to refuse, team or not.
	fake.answers["oauth.v2.access"] = answer(200, `{"ok":true,"is_enterprise_install":true,"team":null,"enterprise":{"id":"E1"},"app_id":"A1"}`)
	g, err = client.OAuthAccess(ctx, "c", "s", "x", "r")
	require.NoError(t, err)
	require.True(t, g.EnterpriseInstall)
	for name, body := range map[string]string{
		"user token": `{"ok":true,"access_token":"xoxp-1","token_type":"user","app_id":"A1","team":{"id":"T1"},"bot_user_id":"U1","authed_user":{"id":"U2"}}`,
		"no team":    `{"ok":true,"access_token":"xoxb-1","token_type":"bot","app_id":"A1","bot_user_id":"U1","authed_user":{"id":"U2"}}`,
		"no app":     `{"ok":true,"access_token":"xoxb-1","token_type":"bot","team":{"id":"T1"},"bot_user_id":"U1","authed_user":{"id":"U2"}}`,
		"bad org":    `{"ok":true,"access_token":"xoxb-1","token_type":"bot","app_id":"A1","team":{"id":"T1"},"enterprise":{"id":"e"},"bot_user_id":"U1","authed_user":{"id":"U2"}}`,
		"refused":    `{"ok":false,"error":"invalid_code"}`,
	} {
		fake.answers["oauth.v2.access"] = answer(200, body)
		_, err := client.OAuthAccess(ctx, "c", "s", "x", "r")
		require.Error(t, err, name)
	}
}

func TestUninstall(t *testing.T) {
	fake, client, _ := newFake(t)
	ctx := context.Background()
	require.NoError(t, client.Uninstall(ctx, bot, "client-id", "client-secret"))
	c := fake.last()
	require.Equal(t, "apps.uninstall", c.method)
	require.Equal(t, "Bearer xoxb-1", c.auth)
	require.Equal(t, url.Values{"client_id": {"client-id"}, "client_secret": {"client-secret"}}, c.form)
	// Tier 1: the next for the install waits.
	var limited *slack.RateLimitedError
	require.ErrorAs(t, client.Uninstall(ctx, bot, "client-id", "client-secret"), &limited)
	require.True(t, limited.Deferred)
}
