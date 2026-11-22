package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// relayCaller calls Slack through the relay as the fixture's link.
type relayCaller struct {
	t     *testing.T
	f     slackFixture
	r     *relayStore
	token string
}

func newRelayCaller(t *testing.T) (relayCaller, *model.SlackInstall) {
	f, r := newRelayFixture(t)
	_, token := r.link(t)
	install := r.relayedInstall(t, f, false)
	r.channels[install.ID+"/C0GEN0001"] = true
	r.users[install.ID+"/"+slackAda] = true
	// Every channel is the team's own, but as a test says.
	f.slack.answers["conversations.info"] = func(_ *http.Request, body []byte) (int, string) {
		form, _ := url.ParseQuery(string(body))
		return http.StatusOK, `{"ok":true,"channel":{"id":"` + form.Get("channel") + `"}}`
	}
	return relayCaller{t: t, f: f, r: r, token: token}, install
}

// slackCalls is how many calls of method Slack got.
func (c relayCaller) slackCalls(method string) int {
	n := 0
	for _, m := range c.f.slack.called() {
		if m == method {
			n++
		}
	}
	return n
}

func (c relayCaller) call(method string, args any) (*foundryv1.CallSlackForRelayResponse, error) {
	c.t.Helper()
	body, err := json.Marshal(args)
	require.NoError(c.t, err)
	return c.f.s.CallSlackForRelay(door(), &foundryv1.CallSlackForRelayRequest{TokenSha256: hashToken(c.token), LocalWorkspaceId: relayLWS,
		Method: method, BodyJson: string(body)})
}

// sent is the body Slack got for method, as JSON or as a form.
func (c relayCaller) sent(method string) map[string]any {
	c.t.Helper()
	call := c.f.slack.call(method)
	require.NotEmpty(c.t, call.body, "Slack got no %s", method)
	assert.Equal(c.t, "Bearer xoxb-live", call.auth, "as the install")
	out := map[string]any{}
	if json.Unmarshal(call.body, &out) == nil {
		return out
	}
	form, err := url.ParseQuery(string(call.body))
	require.NoError(c.t, err)
	for k := range form {
		out[k] = form.Get(k)
	}
	return out
}

func TestCallSlackForRelayRefusals(t *testing.T) {
	c, _ := newRelayCaller(t)
	for name, tc := range map[string]struct {
		method string
		args   any
		code   codes.Code
	}{
		"a method it doesn't relay":          {"admin.users.remove", map[string]any{}, codes.PermissionDenied},
		"history while hosted's feed is off": {"conversations.history", map[string]any{"channel": "C0GEN0001"}, codes.PermissionDenied},
		"a body that isn't an object":        {"chat.postMessage", []string{"x"}, codes.InvalidArgument},
		"null":                               {"chat.postMessage", nil, codes.InvalidArgument},
		"@channel":                           {"chat.postMessage", map[string]any{"channel": "C0GEN0001", "text": "<!channel> look"}, codes.PermissionDenied},
		"@here deep in blocks": {"chat.postMessage", map[string]any{"channel": "C0GEN0001", "blocks": []any{map[string]any{"text": map[string]any{
			"type": "mrkdwn", "text": "hi <!here|here>"}}}}, codes.PermissionDenied},
		"a user group": {"chat.update", map[string]any{"channel": "C0GEN0001", "ts": "1.2", "text": "<!subteam^S1>"}, codes.PermissionDenied},
		"a rich_text broadcast": {"chat.postMessage", map[string]any{"channel": "C0GEN0001", "blocks": []any{map[string]any{"type": "rich_text",
			"elements": []any{map[string]any{"type": "rich_text_section", "elements": []any{map[string]any{"type": "broadcast", "range": "channel"}}}}}}},
			codes.PermissionDenied},
		"a rich_text user group": {"chat.update", map[string]any{"channel": "C0GEN0001", "ts": "1.2", "blocks": []any{map[string]any{"type": "rich_text",
			"elements": []any{map[string]any{"type": "rich_text_section", "elements": []any{map[string]any{"type": "usergroup", "usergroup_id": "S0ALL0001"}}}}}}},
			codes.PermissionDenied},
		"a broadcast in blocks sent as text": {"chat.postEphemeral", map[string]any{"channel": "C0GEN0001", "user": slackAda,
			"blocks": `[{"type":"rich_text","elements":[{"type":"rich_text_section","elements":[{"type":"broadcast","range":"here"}]}]}]`}, codes.PermissionDenied},
		"an escaped mention in attachments sent as text": {"chat.postMessage", map[string]any{"channel": "C0GEN0001",
			"attachments": `[{"text":"\u003c!everyone>"}]`}, codes.PermissionDenied},
		"no channel":                       {"chat.postMessage", map[string]any{"text": "hi"}, codes.InvalidArgument},
		"a channel it was never sent from": {"chat.postMessage", map[string]any{"channel": "C0OTHER01", "text": "hi"}, codes.PermissionDenied},
		"a channel by another name":        {"agents.sessions.setStatus", map[string]any{"channel_id": "C0GEN0001", "channel": "C0OTHER01"}, codes.PermissionDenied},
		"no user":                          {"users.info", map[string]any{}, codes.InvalidArgument},
		"a user who never wrote":           {"users.info", map[string]any{"user": "U0STRANGER"}, codes.PermissionDenied},
		"one known, one not":               {"conversations.open", map[string]any{"users": slackAda + ",U0STRANGER"}, codes.PermissionDenied},
		"a user of no form":                {"conversations.open", map[string]any{"users": "not a user"}, codes.InvalidArgument},
		"a user where none is named":       {"chat.postMessage", map[string]any{"channel": "C0GEN0001", "user": slackAda}, codes.PermissionDenied},
		"a channel to open":                {"conversations.open", map[string]any{"channel": "C0GEN0001", "users": slackAda}, codes.PermissionDenied},
		"an ephemeral to a stranger":       {"chat.postEphemeral", map[string]any{"channel": "C0GEN0001", "user": "U0STRANGER", "text": "x"}, codes.PermissionDenied},
		"a form of objects":                {"users.info", map[string]any{"user": slackAda, "include_locale": map[string]any{}}, codes.InvalidArgument},
	} {
		_, err := c.call(tc.method, tc.args)
		assert.Equal(t, tc.code, status.Code(err), "%s: %v", name, err)
	}
	assert.Empty(t, c.f.slack.called(), "nothing reached Slack")
	assert.EqualValues(t, 22, c.r.usage["link-1"].Refused)

	// Another workspace of the link, a workspace of no form, a body past the
	// cap, a revoked token.
	_, err := c.f.s.CallSlackForRelay(door(), &foundryv1.CallSlackForRelayRequest{TokenSha256: hashToken(c.token), LocalWorkspaceId: "01JANOTHERWORKSPACE0000001",
		Method: "chat.postMessage", BodyJson: `{"channel":"C0GEN0001"}`})
	assert.Equal(t, codes.NotFound, status.Code(err))
	_, err = c.f.s.CallSlackForRelay(door(), &foundryv1.CallSlackForRelayRequest{TokenSha256: hashToken(c.token), LocalWorkspaceId: "nope",
		Method: "chat.postMessage", BodyJson: `{}`})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = c.f.s.CallSlackForRelay(door(), &foundryv1.CallSlackForRelayRequest{TokenSha256: hashToken(c.token), LocalWorkspaceId: relayLWS,
		Method: "chat.postMessage", BodyJson: `{"text":"` + strings.Repeat("x", 64<<10) + `"}`})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = c.f.s.CallSlackForRelay(door(), &foundryv1.CallSlackForRelayRequest{TokenSha256: hashToken("revoked"), LocalWorkspaceId: relayLWS,
		Method: "chat.postMessage", BodyJson: `{}`})
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
	assert.Empty(t, c.f.slack.called())
}

func TestCallSlackForRelayPosts(t *testing.T) {
	c, install := newRelayCaller(t)
	c.f.slack.answer("chat.postMessage", http.StatusOK, `{"ok":true,"ts":"1795000001.000100","channel":"C0GEN0001","token":"xoxb-leak",`+
		`"message":{"bot_token":"x","text":"hi"}}`)
	out, err := c.call("chat.postMessage", map[string]any{
		"channel": "C0GEN0001", "thread_ts": "1795000000.000100", "text": "hi", "username": "Ada", "icon_url": "https://x", "icon_emoji": ":x:",
		"as_user": true, "reply_broadcast": true, "link_names": true, "metadata": map[string]any{"event_type": "x"}, "parse": "full",
		"unfurl_links": true, "unfurl_media": true,
		"blocks": []any{map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "*hi*"},
			"fields": []any{map[string]any{"type": "mrkdwn", "text": "a", "verbatim": false}, map[string]any{"type": "plain_text", "text": "b"}}}},
	})
	require.NoError(t, err)
	assert.Zero(t, out.GetRetryAfterS())
	assert.JSONEq(t, `{"ok":true,"ts":"1795000001.000100","channel":"C0GEN0001","message":{"text":"hi"}}`, out.GetBodyJson(), "no token comes back")
	sent := c.sent("chat.postMessage")
	for _, k := range relayDropped {
		assert.NotContains(t, sent, k)
	}
	assert.Equal(t, "none", sent["parse"])
	assert.Equal(t, false, sent["unfurl_links"])
	assert.Equal(t, false, sent["unfurl_media"])
	block := sent["blocks"].([]any)[0].(map[string]any)
	assert.Equal(t, true, block["text"].(map[string]any)["verbatim"])
	fields := block["fields"].([]any)
	assert.Equal(t, true, fields[0].(map[string]any)["verbatim"])
	assert.NotContains(t, fields[1].(map[string]any), "verbatim", "plain text has none")
	assert.EqualValues(t, 1, c.r.usage["link-1"].Calls)

	// Slack's own error comes back as Slack said it.
	c.f.slack.answer("chat.update", http.StatusOK, `{"ok":false,"error":"message_not_found"}`)
	out, err = c.call("chat.update", map[string]any{"channel": "C0GEN0001", "ts": "1.2", "text": "x"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":false,"error":"message_not_found"}`, out.GetBodyJson())

	// A direct message opened with a user who wrote becomes one to post in.
	c.f.slack.answer("conversations.open", http.StatusOK, `{"ok":true,"channel":{"id":"D0ADA0001"}}`)
	_, err = c.call("conversations.open", map[string]any{"users": slackAda})
	require.NoError(t, err)
	assert.True(t, c.r.channels[install.ID+"/D0ADA0001"])
	_, err = c.call("chat.postEphemeral", map[string]any{"channel": "D0ADA0001", "user": slackAda, "text": "x"})
	require.NoError(t, err)
	assert.Equal(t, "none", c.sent("chat.postEphemeral")["parse"])
	_, err = c.call("agents.sessions.setStatus", map[string]any{"channel_id": "C0GEN0001", "thread_ts": "1.2", "status": "processing"})
	require.NoError(t, err)
	assert.NotContains(t, c.sent("agents.sessions.setStatus"), "parse", "only messages are rewritten")

	// users.info comes back without the person's email or phone, as a form.
	c.f.slack.answer("users.info", http.StatusOK, slackUserJSON(slackAda, "ada@acme.com", func(u map[string]any) {
		u["profile"].(map[string]any)["phone"] = "+44 1"
	}))
	out, err = c.call("users.info", map[string]any{"user": slackAda, "include_locale": true})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"user": slackAda, "include_locale": "true"}, c.sent("users.info"))
	var info struct {
		User struct {
			Profile map[string]any `json:"profile"`
		} `json:"user"`
	}
	require.NoError(t, json.Unmarshal([]byte(out.GetBodyJson()), &info))
	assert.Equal(t, map[string]any{"display_name": "Ada"}, info.User.Profile)
}

func TestCallSlackForRelayConnector(t *testing.T) {
	c, install := newRelayCaller(t)
	c.f.cfg.Foundry.Slack.Feed.Enabled = true

	// users.list keeps emails (the connector's people), not phones.
	c.f.slack.answer("users.list", http.StatusOK, `{"ok":true,"members":[{"id":"U1","profile":{"email":"a@x.com","phone":"1"}},"odd"]}`)
	out, err := c.call("users.list", map[string]any{"limit": 200, "cursor": "abc"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true,"members":[{"id":"U1","profile":{"email":"a@x.com"}},"odd"]}`, out.GetBodyJson())
	assert.Equal(t, map[string]any{"limit": "200", "cursor": "abc"}, c.sent("users.list"))

	// users.conversations' channels become the install's, but shared ones.
	c.f.slack.answer("users.conversations", http.StatusOK, `{"ok":true,"channels":[{"id":"C0NEW0001"},{"id":"C0EXT0001","is_ext_shared":true},`+
		`{"id":"C0PEND001","is_pending_ext_shared":true},{"id":"D0DM00001","is_im":true},{"id":"G0MPIM001","is_mpim":true},{"id":"bad id"}]}`)
	_, err = c.call("users.conversations", map[string]any{"types": "public_channel"})
	require.NoError(t, err)
	assert.True(t, c.r.channels[install.ID+"/C0NEW0001"])
	assert.False(t, c.r.channels[install.ID+"/C0EXT0001"])
	assert.False(t, c.r.channels[install.ID+"/C0PEND001"])
	assert.False(t, c.r.channels[install.ID+"/D0DM00001"])
	assert.False(t, c.r.channels[install.ID+"/G0MPIM001"])
	_, err = c.call("users.conversations", map[string]any{"user": "U0SOMEONE"})
	assert.Equal(t, codes.PermissionDenied, status.Code(err), "only the bot's own channels")
	_, err = c.call("users.conversations", map[string]any{"user": "U0BOT0001"})
	require.NoError(t, err)

	// History takes the install's Tier-1 slot, shared by every replica.
	c.f.slack.answer("conversations.history", http.StatusOK, `{"ok":true,"messages":[]}`)
	out, err = c.call("conversations.history", map[string]any{"channel": "C0NEW0001", "limit": 15})
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true,"messages":[]}`, out.GetBodyJson())
	out, err = c.call("conversations.history", map[string]any{"channel": "C0NEW0001"})
	require.NoError(t, err)
	assert.EqualValues(t, 60, out.GetRetryAfterS(), "the slot isn't due: a minute at 1 a minute")
	_, err = c.call("conversations.history", map[string]any{"channel": "C0EXT0001"})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))

	// Slack's 429 holds the method back and defers the slot.
	c.f.slack.answer("conversations.replies", http.StatusTooManyRequests, `{"ok":false,"error":"ratelimited"}`)
	out, err = c.call("conversations.replies", map[string]any{"channel": "C0NEW0001", "ts": "1.2"})
	require.NoError(t, err)
	assert.EqualValues(t, 30, out.GetRetryAfterS(), "no Retry-After is 30 s")
	assert.Equal(t, 30*time.Second, c.r.deferred[model.SlackSlotReplies])
	c.r.slots[model.SlackSlotReplies] = true
	calls := len(c.f.slack.called())
	out, err = c.call("conversations.replies", map[string]any{"channel": "C0NEW0001", "ts": "1.2"})
	require.NoError(t, err)
	assert.Positive(t, out.GetRetryAfterS())
	assert.Len(t, c.f.slack.called(), calls, "held back without a call")

	// Slack failing is unavailable; an answer that isn't an object too.
	c.f.slack.answer("auth.test", http.StatusInternalServerError, `oops`)
	_, err = c.call("auth.test", map[string]any{})
	assert.Equal(t, codes.Unavailable, status.Code(err))
	c.f.slack.answer("conversations.list", http.StatusOK, `[1]`)
	_, err = c.call("conversations.list", map[string]any{})
	assert.Equal(t, codes.Unavailable, status.Code(err))
}

func TestCallSlackForRelayLimits(t *testing.T) {
	c, _ := newRelayCaller(t)
	c.f.cfg.Foundry.Slack.Relay.CallsPerMinute = 2
	args := map[string]any{"channel": "C0GEN0001", "text": "hi"}
	for range 2 {
		out, err := c.call("chat.postMessage", args)
		require.NoError(t, err)
		require.Zero(t, out.GetRetryAfterS())
	}
	out, err := c.call("chat.postMessage", args)
	require.NoError(t, err)
	assert.Positive(t, out.GetRetryAfterS(), "the link's calls a minute")
	assert.Equal(t, 2, c.slackCalls("chat.postMessage"))

	// Slack's Retry-After holds the install's method.
	d, _ := newRelayCaller(t)
	d.f.slack.answers["chat.update"] = func(*http.Request, []byte) (int, string) { return http.StatusTooManyRequests, `{"ok":false}` }
	d.f.slack.Config.Handler = retryAfter(d.f.slack.Config.Handler, "7")
	out, err = d.call("chat.update", map[string]any{"channel": "C0GEN0001", "ts": "1.2", "text": "x"})
	require.NoError(t, err)
	assert.EqualValues(t, 7, out.GetRetryAfterS())
	out, err = d.call("chat.update", map[string]any{"channel": "C0GEN0001", "ts": "1.2", "text": "x"})
	require.NoError(t, err)
	assert.Positive(t, out.GetRetryAfterS())
	assert.Equal(t, 1, d.slackCalls("chat.update"))
}

// A channel shared with another organisation after the install recorded
// it is refused for every call naming it, as Slack's conversations.info
// says, asked once in relaySharedFor; a direct message isn't asked about.
func TestCallSlackForRelaySharedChannels(t *testing.T) {
	c, install := newRelayCaller(t)
	c.r.channels[install.ID+"/C0EXT0001"] = true
	c.f.slack.answers["conversations.info"] = func(_ *http.Request, body []byte) (int, string) {
		form, _ := url.ParseQuery(string(body))
		ch := form.Get("channel")
		return http.StatusOK, `{"ok":true,"channel":{"id":"` + ch + `","is_ext_shared":` + strconv.FormatBool(ch == "C0EXT0001") + `}}`
	}
	for range 2 {
		_, err := c.call("chat.postMessage", map[string]any{"channel": "C0EXT0001", "text": "hi"})
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
	}
	_, err := c.call("chat.update", map[string]any{"channel": "C0EXT0001", "ts": "1.2", "text": "x"})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Equal(t, 1, c.slackCalls("conversations.info"), "Slack's word stands for a while")
	assert.Zero(t, c.slackCalls("chat.postMessage")+c.slackCalls("chat.update"))
	assert.Equal(t, "Bearer xoxb-live", c.f.slack.call("conversations.info").auth, "asked as the install")

	_, err = c.call("chat.postMessage", map[string]any{"channel": "C0GEN0001", "text": "hi"})
	require.NoError(t, err)
	assert.Equal(t, 1, c.slackCalls("chat.postMessage"), "the team's own channel")

	// Slack's word lapses, and is asked again.
	c.f.s.relay.mu.Lock()
	for k, v := range c.f.s.relay.shared {
		v.until = time.Now().Add(-time.Second)
		c.f.s.relay.shared[k] = v
	}
	c.f.s.relay.mu.Unlock()
	_, err = c.call("chat.postMessage", map[string]any{"channel": "C0EXT0001", "text": "hi"})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Equal(t, 3, c.slackCalls("conversations.info"))

	// The bot's own conversations.info is relayed as Slack answers it, and
	// what it says stands.
	c.r.channels[install.ID+"/C0LATE001"] = true
	c.f.slack.answer("conversations.info", http.StatusOK, `{"ok":true,"channel":{"id":"C0LATE001","is_pending_ext_shared":true}}`)
	out, err := c.call("conversations.info", map[string]any{"channel": "C0LATE001"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true,"channel":{"id":"C0LATE001","is_pending_ext_shared":true}}`, out.GetBodyJson())
	_, err = c.call("chat.postMessage", map[string]any{"channel": "C0LATE001", "text": "hi"})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Equal(t, 4, c.slackCalls("conversations.info"), "the bot's own ask stands")

	// A direct message isn't asked about; one Slack says is shared isn't
	// recorded.
	c.f.slack.answer("conversations.open", http.StatusOK, `{"ok":true,"channel":{"id":"D0ADA0001"}}`)
	_, err = c.call("conversations.open", map[string]any{"users": slackAda})
	require.NoError(t, err)
	_, err = c.call("chat.postMessage", map[string]any{"channel": "D0ADA0001", "text": "hi"})
	require.NoError(t, err)
	assert.Equal(t, 4, c.slackCalls("conversations.info"))
	c.f.slack.answer("conversations.open", http.StatusOK, `{"ok":true,"channel":{"id":"D0EXT0001","is_ext_shared":true}}`)
	_, err = c.call("conversations.open", map[string]any{"users": slackAda})
	require.NoError(t, err)
	assert.False(t, c.r.channels[install.ID+"/D0EXT0001"])

	// Slack not saying is no answer: the call isn't made.
	d, _ := newRelayCaller(t)
	for _, answer := range []string{`{"ok":false,"error":"channel_not_found"}`, `{"ok":true,"channel":{"id":"C0OTHER01"}}`, `[]`} {
		d.f.slack.answer("conversations.info", http.StatusOK, answer)
		_, err = d.call("chat.update", map[string]any{"channel": "C0GEN0001", "ts": "1.2", "text": "x"})
		assert.Equal(t, codes.Unavailable, status.Code(err), answer)
	}
	d.f.slack.answer("conversations.info", http.StatusInternalServerError, `oops`)
	_, err = d.call("chat.update", map[string]any{"channel": "C0GEN0001", "ts": "1.2", "text": "x"})
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.Zero(t, d.slackCalls("chat.update"))
}

// Asking Slack whether a channel is shared takes the install's share of
// conversations.info's tier, and Slack's 429 holds both back as a rate
// limit, never an outage; Slack's word that a channel isn't shared stands
// an hour where channel_shared reaches Helix, which forgets the channel for
// every replica; and a data connector's read of a channel seen within that
// hour asks nothing.
func TestCallSlackForRelaySharedCheckIsPaced(t *testing.T) {
	c, install := newRelayCaller(t)
	for _, ch := range []string{"C0TWO0001", "C0THREE01"} {
		c.r.channels[install.ID+"/"+ch] = true
	}
	c.f.slack.answers["conversations.info"] = func(*http.Request, []byte) (int, string) {
		return http.StatusTooManyRequests, `{"ok":false,"error":"ratelimited"}`
	}
	c.f.slack.Config.Handler = retryAfter(c.f.slack.Config.Handler, "9")
	out, err := c.call("chat.postMessage", map[string]any{"channel": "C0GEN0001", "text": "hi"})
	require.NoError(t, err, "a 429 is a rate limit, not unavailable")
	assert.EqualValues(t, 9, out.GetRetryAfterS())
	out, err = c.call("chat.update", map[string]any{"channel": "C0TWO0001", "ts": "1.2", "text": "x"})
	require.NoError(t, err)
	assert.Positive(t, out.GetRetryAfterS(), "Slack's Retry-After holds the install's conversations.info")
	assert.Equal(t, 1, c.slackCalls("conversations.info"), "held back without asking")
	assert.Zero(t, c.slackCalls("chat.postMessage")+c.slackCalls("chat.update"))

	// The install's share of conversations.info's tier: 50 a minute at 0.12
	// is 6, one at a time.
	d, dInstall := newRelayCaller(t)
	d.f.cfg.Foundry.Slack.APIShare = 0.12
	d.r.channels[dInstall.ID+"/C0TWO0001"] = true
	_, err = d.call("chat.postMessage", map[string]any{"channel": "C0GEN0001", "text": "hi"})
	require.NoError(t, err)
	out, err = d.call("chat.postMessage", map[string]any{"channel": "C0TWO0001", "text": "hi"})
	require.NoError(t, err)
	assert.Positive(t, out.GetRetryAfterS(), "no ask left this second")
	assert.Equal(t, 1, d.slackCalls("conversations.info"))
	assert.Equal(t, 1, d.slackCalls("chat.postMessage"))
	_, err = d.call("chat.postMessage", map[string]any{"channel": "C0GEN0001", "text": "again"})
	require.NoError(t, err)
	assert.Equal(t, 2, d.slackCalls("chat.postMessage"), "a channel Slack answered for needs no ask")

	// Slack's word stands 5 minutes while channel_shared doesn't reach
	// Helix, an hour once it does.
	e, eInstall := newRelayCaller(t)
	stands := func(channel string) time.Duration {
		e.f.s.relay.mu.Lock()
		defer e.f.s.relay.mu.Unlock()
		return time.Until(e.f.s.relay.shared[eInstall.ID+" "+channel].until)
	}
	_, err = e.call("chat.postMessage", map[string]any{"channel": "C0GEN0001", "text": "hi"})
	require.NoError(t, err)
	assert.InDelta(t, relaySharedFor, stands("C0GEN0001"), float64(time.Minute))
	e.f.cfg.Foundry.Slack.Feed.Enabled = true
	e.r.channels[eInstall.ID+"/C0TWO0001"] = true
	_, err = e.call("chat.postMessage", map[string]any{"channel": "C0TWO0001", "text": "hi"})
	require.NoError(t, err)
	assert.InDelta(t, relayUnsharedFor, stands("C0TWO0001"), float64(time.Minute))

	// A data connector's read of a channel seen within the hour asks
	// nothing; one seen longer ago asks.
	asked := e.slackCalls("conversations.info")
	e.r.channels[eInstall.ID+"/C0HIST001"] = true
	e.f.slack.answer("conversations.history", http.StatusOK, `{"ok":true,"messages":[]}`)
	_, err = e.call("conversations.history", map[string]any{"channel": "C0HIST001"})
	require.NoError(t, err)
	assert.Equal(t, asked, e.slackCalls("conversations.info"), "seen just now")
	e.r.channels[eInstall.ID+"/C0OLD0001"] = true
	e.r.seenAgo[eInstall.ID+"/C0OLD0001"] = 2 * time.Hour
	e.r.slots[model.SlackSlotHistory] = true
	_, err = e.call("conversations.history", map[string]any{"channel": "C0OLD0001"})
	require.NoError(t, err)
	assert.Equal(t, asked+1, e.slackCalls("conversations.info"), "seen two hours ago")
	assert.Equal(t, 2, e.slackCalls("conversations.history"))

	// channel_shared forgets the channel for every replica, and this one
	// holds it shared, before the event is dropped as a shared channel's.
	resp, err := e.f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, fixture(t, "events/channel_shared.json")))
	require.NoError(t, err)
	require.EqualValues(t, http.StatusOK, resp.GetHttpStatus())
	assert.False(t, e.r.channels[eInstall.ID+"/C0GEN0001"], "forgotten in the store")
	shared, known := e.f.s.relay.knownShared(eInstall.ID, "C0GEN0001", time.Now())
	assert.True(t, known && shared)
	_, err = e.call("chat.postMessage", map[string]any{"channel": "C0GEN0001", "text": "hi"})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	e.r.relayFail = errors.New("down")
	resp, err = e.f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, fixture(t, "events/channel_shared.json")))
	assert.Equal(t, codes.Unavailable, status.Code(err), "Slack sends it again: %v", resp)
}

// Past relayCapacity channels, Slack's lapsed words are forgotten first,
// then all of them.
func TestRelaySharedIsBounded(t *testing.T) {
	var l relayLimits
	l.init(60, 1)
	now := time.Now()
	for i := range relayCapacity {
		l.sawShared("inst", strconv.Itoa(i), false, now.Add(-relaySharedFor), false)
	}
	l.sawShared("inst", "fresh", true, now, true)
	assert.Len(t, l.shared, 1, "every lapsed word went")
	for i := range relayCapacity - 1 {
		l.sawShared("inst", strconv.Itoa(i), false, now, false)
	}
	l.sawShared("inst", "last", true, now, true)
	assert.Len(t, l.shared, 1, "full of standing words, it starts again")
	assert.True(t, l.shared["inst last"].shared)
}

// retryAfter wraps h to send Retry-After: seconds.
func retryAfter(h http.Handler, seconds string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", seconds)
		h.ServeHTTP(w, r)
	})
}

func TestRelayHelpers(t *testing.T) {
	assert.Equal(t, "abc", truncateUTF8("abc", 3))
	assert.Equal(t, "ab", truncateUTF8("abc", 2))
	assert.Equal(t, "a", truncateUTF8("aé", 2), "never half a character")
	assert.Equal(t, "", truncateUTF8("é", 1))
	assert.Equal(t, "", relayMassMention(map[string]any{"a": []any{1.0, true, nil, "plain"}}))
	assert.Equal(t, "<!everyone", relayMassMention([]any{"x", map[string]any{"k": "<!everyone>"}}))
	assert.Equal(t, "<!here", relayMassMention(map[string]any{"type": "broadcast", "range": "here"}))
	assert.Equal(t, "<!everyone", relayMassMention(map[string]any{"type": "broadcast", "range": "everyone"}))
	assert.Equal(t, "<!channel", relayMassMention(map[string]any{"type": "broadcast"}), "a range it doesn't know is the channel")
	assert.Equal(t, "<!subteam", relayMassMention(`{"type":"usergroup","usergroup_id":"S1"}`))
	assert.Equal(t, "", relayMassMention(`[not json`), "text that only looks like JSON is text")
	assert.Equal(t, "", relayMassMention(`[{"type":"user","user_id":"U1"}]`), "a person's mention notifies them alone")
	assert.EqualValues(t, 2, relayRetry(1500*time.Millisecond).GetRetryAfterS(), "rounded up")
	answer := map[string]any{"user": "not an object"}
	scrubRelayAnswer("users.info", answer)
	assert.Equal(t, map[string]any{"user": "not an object"}, answer, "an odd answer is left as it is")
}

func TestStartAndFinishRelayedInstall(t *testing.T) {
	f, r := newRelayFixture(t)
	link, token := r.link(t)
	start := func(mutate func(*foundryv1.StartFoundryRelaySlackInstallRequest)) (*foundryv1.StartFoundryRelaySlackInstallResponse, error) {
		req := &foundryv1.StartFoundryRelaySlackInstallRequest{TokenSha256: hashToken(token), LocalWorkspaceId: relayLWS, ReturnTo: link.ReturnTo()}
		if mutate != nil {
			mutate(req)
		}
		return f.s.StartFoundryRelaySlackInstall(door(), req)
	}
	_, err := start(func(q *foundryv1.StartFoundryRelaySlackInstallRequest) { q.LocalWorkspaceId = "nope" })
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = start(func(q *foundryv1.StartFoundryRelaySlackInstallRequest) {
		q.ReturnTo = "http://localhost:4000/settings/slack"
	})
	assert.Equal(t, codes.InvalidArgument, status.Code(err), "only the link's own origin")
	f.cfg.Foundry.Slack.InstallAllowlist = []string{"another"}
	_, err = start(nil)
	assert.Equal(t, "FOUNDRY_SLACK_NOT_OFFERED", reasonOf(err))
	f.cfg.Foundry.Slack.InstallAllowlist = nil
	people := f.s.deps.People
	f.s.deps.People = slackPeople{roles: map[string]ports.Role{"user-1": ports.RoleMember}}
	_, err = start(nil)
	assert.Equal(t, codes.PermissionDenied, status.Code(err), "its member lost the role")
	f.s.deps.People = people
	f.cfg.Foundry.Agent.Enabled = false // the install runs its own Founder

	resp, err := start(nil)
	require.NoError(t, err)
	authorize, err := url.Parse(resp.GetAuthorizeUrl())
	require.NoError(t, err)
	assert.Equal(t, "https://foundry.example.com/slack/installed", authorize.Query().Get("redirect_uri"))
	state := authorize.Query().Get("state")
	require.NotEmpty(t, state)

	// Only the link's member finishes it.
	f.slack.answer("oauth.v2.access", http.StatusOK, grantJSON(nil))
	_, err = f.s.FinishFoundrySlackInstall(as("user-3"), &foundryv1.FinishFoundrySlackInstallRequest{Code: "c", State: state})
	assert.Equal(t, "FOUNDRY_SLACK_INSTALL_EXPIRED", reasonOf(err))
	done, err := f.s.FinishFoundrySlackInstall(as("user-1"), &foundryv1.FinishFoundrySlackInstallRequest{Code: "c", State: state})
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:3000/settings/slack", done.GetReturnTo())
	assert.Nil(t, done.GetSlack().GetInstall(), "the workspace's own Slack is untouched")
	installed, err := r.GetFoundryRelayInstall(context.Background(), link.ID, relayLWS)
	require.NoError(t, err)
	assert.Equal(t, model.SlackServesRelay{LinkID: link.ID, LocalWorkspaceID: relayLWS}, installed.Destination)

	// Installed with every scope, it isn't started again.
	_, err = start(nil)
	assert.Equal(t, "FOUNDRY_SLACK_INSTALLED", reasonOf(err))

	// A link revoked before Slack came back finishes nothing.
	r.installs = nil
	resp, err = start(nil)
	require.NoError(t, err)
	authorize, _ = url.Parse(resp.GetAuthorizeUrl())
	r.slackStore.refusal = foundrystore.ErrNotFound
	_, err = f.s.FinishFoundrySlackInstall(as("user-1"), &foundryv1.FinishFoundrySlackInstallRequest{Code: "c", State: authorize.Query().Get("state")})
	assert.Equal(t, "FOUNDRY_RELAY_LINK_REVOKED", reasonOf(err))
	r.slackStore.refusal = nil

	// With the relay off a relayed state finishes nothing.
	resp, err = start(nil)
	require.NoError(t, err)
	authorize, _ = url.Parse(resp.GetAuthorizeUrl())
	f.cfg.Foundry.Slack.Relay.Enabled = false
	_, err = f.s.FinishFoundrySlackInstall(as("user-1"), &foundryv1.FinishFoundrySlackInstallRequest{Code: "c", State: authorize.Query().Get("state")})
	assert.Equal(t, "FOUNDRY_RELAY_DISABLED", reasonOf(err))
}

func TestListAndRevokeRelayLinks(t *testing.T) {
	f, r := newRelayFixture(t)
	link, _ := r.link(t)
	seen := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	link.LastSeenAt = &seen
	r.relayedInstall(t, f, false)
	r.usage[link.ID] = model.RelayUsage{Deliveries: 3, Calls: 9, Refused: 1, FeedRows: 40}
	_, err := f.s.ListFoundryRelayLinks(as("user-2"), &foundryv1.ListFoundryRelayLinksRequest{WorkspaceId: slackWS})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = f.s.ListFoundryRelayLinks(as("user-1"), &foundryv1.ListFoundryRelayLinksRequest{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	out, err := f.s.ListFoundryRelayLinks(as("user-1"), &foundryv1.ListFoundryRelayLinksRequest{WorkspaceId: slackWS})
	require.NoError(t, err)
	require.Len(t, out.GetLinks(), 1)
	got := out.GetLinks()[0]
	assert.Equal(t, "Ada's laptop", got.GetName())
	assert.Equal(t, "Name of user-1", got.GetCreatedByName())
	assert.Equal(t, seen, got.GetLastSeenAt().AsTime())
	assert.EqualValues(t, 3, got.GetDeliveriesToday())
	assert.EqualValues(t, 9, got.GetCallsToday())
	assert.EqualValues(t, 1, got.GetRefusedToday())
	assert.EqualValues(t, 40, got.GetFeedRowsToday())
	require.Len(t, got.GetInstalls(), 1)
	assert.Equal(t, "Acme", got.GetInstalls()[0].GetTeamName())

	_, err = f.s.RevokeFoundryRelayLink(as("user-2"), &foundryv1.RevokeFoundryRelayLinkRequest{WorkspaceId: slackWS, LinkId: link.ID})
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = f.s.RevokeFoundryRelayLink(as("user-1"), &foundryv1.RevokeFoundryRelayLinkRequest{WorkspaceId: slackWS})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = f.s.RevokeFoundryRelayLink(as("user-1"), &foundryv1.RevokeFoundryRelayLinkRequest{WorkspaceId: slackWS, LinkId: link.ID})
	require.NoError(t, err)
	_, err = f.s.RevokeFoundryRelayLink(as("user-1"), &foundryv1.RevokeFoundryRelayLinkRequest{WorkspaceId: slackWS, LinkId: link.ID})
	assert.Equal(t, codes.NotFound, status.Code(err))
	out, err = f.s.ListFoundryRelayLinks(as("user-1"), &foundryv1.ListFoundryRelayLinksRequest{WorkspaceId: slackWS})
	require.NoError(t, err)
	assert.Empty(t, out.GetLinks())

	f.cfg.Foundry.Slack.Relay.Enabled = false
	_, err = f.s.ListFoundryRelayLinks(as("user-1"), &foundryv1.ListFoundryRelayLinksRequest{WorkspaceId: slackWS})
	assert.Equal(t, "FOUNDRY_RELAY_DISABLED", reasonOf(err))
	f.cfg.Host.Local = true
	f.cfg.Foundry.Slack.Relay.Enabled = true
	_, err = f.s.ListFoundryRelayLinks(as("user-1"), &foundryv1.ListFoundryRelayLinksRequest{WorkspaceId: slackWS})
	assert.Equal(t, "FOUNDRY_RELAY_DISABLED", reasonOf(err), "a standalone install relays nothing")
}

func TestRelayedInstallFeed(t *testing.T) {
	f, r := newRelayFixture(t)
	r.link(t)
	install := r.relayedInstall(t, f, true)
	receive := func(name string) {
		t.Helper()
		resp, err := f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, fixture(t, "events/"+name)))
		require.NoError(t, err)
		require.EqualValues(t, http.StatusOK, resp.GetHttpStatus())
	}
	f.cfg.Foundry.Slack.Follow.Enabled = true
	receive("channel_message.json")
	receive("channel_reply.json")
	assert.Empty(t, r.relayFeed, "nothing while hosted's feed is off and the local install takes no follow-ups")

	// A local install that takes follow-ups gets the thread replies, with
	// or without a data connector, while hosted's follow-ups are on.
	// The feed's intake keeps an install it read for a while: read it again.
	install.FeedOn, install.FollowOn = false, true
	f.s.slackFeed.mu.Lock()
	f.s.slackFeed.installs = nil
	f.s.slackFeed.mu.Unlock()
	receive("channel_message.json")
	assert.Empty(t, r.relayFeed, "a channel message only for a data connector")
	receive("channel_reply.json")
	require.Len(t, r.relayFeed, 1, "a reply for its follow-ups, no data connector reading it")
	assert.Len(t, r.relayFeed[0].Rows, 1)
	assert.Empty(t, r.intake, "Helix follows no relayed thread")
	receive("app_rate_limited.json")
	assert.Len(t, r.relayFeed, 1, "gaps are a data connector's")
	f.cfg.Foundry.Slack.Follow.Enabled = false
	receive("channel_reply.json")
	f.cfg.Foundry.Slack.Feed.Enabled = true
	receive("channel_reply.json")
	assert.Len(t, r.relayFeed, 1, "only while hosted's follow-ups are on")
	f.cfg.Foundry.Slack.Follow.Enabled, f.cfg.Foundry.Slack.Feed.Enabled = true, false

	install.FeedOn, install.FollowOn = true, false
	f.s.slackFeed.mu.Lock()
	f.s.slackFeed.installs = nil
	f.s.slackFeed.mu.Unlock()
	f.cfg.Foundry.Slack.Feed.Enabled = true
	receive("channel_message.json")
	require.Len(t, r.relayFeed, 2)
	receive("app_rate_limited.json")
	require.Len(t, r.relayFeed, 3)
	require.Len(t, r.relayFeed[2].Gaps, 1)
	assert.Equal(t, "", r.relayFeed[2].Gaps[0].ChannelID, "every channel's minute")

	install.FeedOn = false
	receive("channel_message.json")
	assert.Len(t, r.relayFeed, 3, "no connector reads it")

	// The door's gaps of a relayed team are its install's.
	install.FeedOn = true
	minute := time.Now().Truncate(time.Minute)
	_, err := f.s.RecordFoundrySlackFeedGaps(door(), &foundryv1.RecordFoundrySlackFeedGapsRequest{Gaps: []*foundryv1.FoundrySlackFeedGap{
		{TeamId: slackTeam, ChannelId: "C0GEN0001", Minute: timestamppb.New(minute)}}})
	require.NoError(t, err)
	require.Len(t, r.relayFeed, 4)
	assert.Equal(t, []model.SlackFeedGap{model.SlackMinuteGap("C0GEN0001", minute)}, r.relayFeed[3].Gaps)
	install.FeedOn = false
	_, err = f.s.RecordFoundrySlackFeedGaps(door(), &foundryv1.RecordFoundrySlackFeedGapsRequest{Gaps: []*foundryv1.FoundrySlackFeedGap{
		{TeamId: slackTeam, ChannelId: "C0GEN0001", Minute: timestamppb.New(minute)}}})
	require.NoError(t, err)
	assert.Len(t, r.relayFeed, 4)

	// A signed event's gap is its local install's too, once verified, and
	// its channel one the install's calls may name from now, so the gap is
	// held there.
	install.FeedOn = true
	req := signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, fixture(t, "events/channel_message.json"))
	resp, err := f.s.RecordFoundrySlackFeedGaps(door(), &foundryv1.RecordFoundrySlackFeedGapsRequest{
		Signed: &foundryv1.FoundrySlackSignedEvent{Body: req.GetBody(), Signature: req.GetSignature(), Timestamp: req.GetTimestamp()}})
	require.NoError(t, err)
	assert.Equal(t, foundryv1.SlackSignedGap_SLACK_SIGNED_GAP_RECORDED, resp.GetSigned())
	require.Len(t, r.relayFeed, 5)
	require.Len(t, r.relayFeed[4].Gaps, 1)
	assert.Equal(t, "C0GEN0001", r.relayFeed[4].Gaps[0].ChannelID)
	assert.True(t, r.channels[install.ID+"/C0GEN0001"])
}
