package slackbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// transportStore is the installs and tokens a transport reads.
type transportStore struct {
	installs map[string]*model.SlackInstall
	tokens   map[string]model.SlackSealedToken
}

func (s transportStore) GetFoundrySlackInstallByID(_ context.Context, id string) (*model.SlackInstall, error) {
	if in, ok := s.installs[id]; ok {
		return in, nil
	}
	return nil, store.ErrNotFound
}

func (s transportStore) GetFoundrySlackInstallToken(_ context.Context, id string) (model.SlackSealedToken, error) {
	if t, ok := s.tokens[id]; ok {
		return t, nil
	}
	return model.SlackSealedToken{}, store.ErrNotFound
}

func TestDirectOpensEachInstallsToken(t *testing.T) {
	keys, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)})
	require.NoError(t, err)
	sealed, keyID, err := keys.Seal([]byte("xoxb-live"), "inst-1")
	require.NoError(t, err)
	other, otherKey, err := keys.Seal([]byte("xoxb-other"), "inst-2")
	require.NoError(t, err)
	d := Direct{DB: transportStore{tokens: map[string]model.SlackSealedToken{
		"inst-1": {Sealed: sealed, KeyID: keyID},
		"inst-3": {Sealed: other, KeyID: otherKey}, // sealed for another install
	}}, Keys: keys, WebAPI: "https://slack.example/api"}
	bot, err := d.Bot(context.Background(), "inst-1")
	require.NoError(t, err)
	assert.Equal(t, slack.Bot{InstallID: "inst-1", Token: "xoxb-live"}, bot)
	_, err = d.Bot(context.Background(), "inst-2")
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = d.Bot(context.Background(), "inst-3")
	assert.ErrorContains(t, err, "open the install's token")
	assert.NotNil(t, d.Caller(bot))
}

// relayAt is a Relayed whose link is to door, and its installs: inst-1
// live, serving ws-1; inst-2 revoked.
func relayAt(door string, linked error) Relayed {
	revoked := time.Now()
	return Relayed{
		DB: transportStore{installs: map[string]*model.SlackInstall{
			"inst-1": {ID: "inst-1", WorkspaceID: "ws-1"},
			"inst-2": {ID: "inst-2", WorkspaceID: "ws-2", RevokedAt: &revoked},
		}},
		Access: func(context.Context) (RelayAccess, error) {
			return RelayAccess{Door: door + "/", Token: relayToken, SlackApprovals: true}, linked
		},
	}
}

func TestRelayedBotNamesItsLocalWorkspace(t *testing.T) {
	r := relayAt("https://ingest.example", nil)
	bot, err := r.Bot(context.Background(), "inst-1")
	require.NoError(t, err)
	assert.Equal(t, slack.Bot{InstallID: "inst-1", Token: "ws-1"}, bot)
	for _, id := range []string{"inst-2", "inst-9"} {
		_, err := r.Bot(context.Background(), id)
		assert.ErrorIs(t, err, store.ErrNotFound, id)
	}
	approvals, err := r.SlackApprovals(context.Background())
	require.NoError(t, err)
	assert.True(t, approvals)
	broken := relayAt("https://ingest.example", errors.New("the directory is down"))
	_, err = broken.SlackApprovals(context.Background())
	assert.Error(t, err)
}

// relayed is one call the door took: its path, bearer and body.
type relayed struct {
	path, auth string
	body       map[string]any
}

// door answers each call with code, header and body, recording it.
func door(t *testing.T, code int, header http.Header, body string, took *[]relayed) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		*took = append(*took, relayed{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: b})
		for k, v := range header {
			w.Header()[k] = v
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

// Every Web API call goes to the relay as the local workspace the bot's
// token names, its arguments a JSON object, whatever form the Web API takes
// them in.
func TestRelayedSendsEachCallThroughTheDoor(t *testing.T) {
	var took []relayed
	d := door(t, http.StatusOK, nil, `{"ok":true,"ts":"1700000000.000001","user":{"id":"U0ADA"}}`, &took)
	r := relayAt(d.URL, nil)
	limiter, err := slack.NewLimiter(1)
	require.NoError(t, err)
	client, err := slack.NewClient(slack.ClientOptions{HTTP: r.Client(), Limiter: limiter})
	require.NoError(t, err)
	bot := slack.Bot{InstallID: "inst-1", Token: "ws-1"}

	site, err := slack.NewSite(siteURL)
	require.NoError(t, err)
	ts, err := client.PostMessage(context.Background(), bot, slack.Post{Channel: "C0GENERAL", Message: slack.Message{Text: site.Text("hi")}})
	require.NoError(t, err)
	assert.Equal(t, "1700000000.000001", ts)
	_, err = client.UserInfo(context.Background(), bot, "U0ADA")
	require.NoError(t, err)
	status, _, body, err := r.Callers(&model.SlackInstall{ID: "inst-1", WorkspaceID: "ws-1"}).Call(context.Background(), "conversations.history",
		url.Values{"channel": {"C0GENERAL"}, "limit": {"15"}})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, string(body), `"ok":true`)

	require.Len(t, took, 3)
	for _, c := range took {
		assert.Equal(t, "Bearer "+relayToken, c.auth, "the link's token, never the bot's")
		assert.Equal(t, "ws-1", c.body["local_workspace_id"])
	}
	assert.Equal(t, "/v1/relay/slack/chat.postMessage", took[0].path)
	assert.Equal(t, "C0GENERAL", took[0].body["args"].(map[string]any)["channel"])
	assert.Equal(t, "none", took[0].body["args"].(map[string]any)["parse"])
	assert.Equal(t, "/v1/relay/slack/users.info", took[1].path)
	assert.Equal(t, map[string]any{"user": "U0ADA"}, took[1].body["args"], "a form's values as strings")
	assert.Equal(t, "/v1/relay/slack/conversations.history", took[2].path)
	assert.Equal(t, map[string]any{"channel": "C0GENERAL", "limit": "15"}, took[2].body["args"], "a query's values as strings")
}

// The relay's answers come back as Slack's: its 429 and 5xx as they are, a
// refusal of its own as Slack's ok:false, which the bot never retries.
func TestRelayedAnswersAsSlack(t *testing.T) {
	call := func(t *testing.T, r Relayed) error {
		limiter, err := slack.NewLimiter(1)
		require.NoError(t, err)
		client, err := slack.NewClient(slack.ClientOptions{HTTP: r.Client(), Limiter: limiter})
		require.NoError(t, err)
		_, err = client.OpenDM(context.Background(), slack.Bot{InstallID: "inst-1", Token: "ws-1"}, "U0ADA")
		return err
	}
	t.Run("a rate limit", func(t *testing.T) {
		var took []relayed
		d := door(t, http.StatusTooManyRequests, http.Header{"Retry-After": {"7"}}, `{"error":{"code":"rate_limited"}}`, &took)
		wait, limited := rateLimited(call(t, relayAt(d.URL, nil)))
		assert.True(t, limited)
		assert.Equal(t, 7*time.Second, wait)
	})
	for code, want := range map[int]string{http.StatusForbidden: "relay_forbidden", http.StatusUnauthorized: "relay_invalid_token",
		http.StatusConflict: "relay_failed_precondition"} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var took []relayed
			body := map[int]string{http.StatusForbidden: "forbidden", http.StatusUnauthorized: "invalid_token", http.StatusConflict: "failed_precondition"}[code]
			d := door(t, code, nil, `{"error":{"code":"`+body+`","message":"no"}}`, &took)
			err := call(t, relayAt(d.URL, nil))
			assert.True(t, slack.IsCode(err, want), "%v", err)
			assert.True(t, slackGone(err))
		})
	}
	t.Run("a refusal it can't read", func(t *testing.T) {
		var took []relayed
		d := door(t, http.StatusBadRequest, nil, `not json`, &took)
		err := call(t, relayAt(d.URL, nil))
		assert.True(t, slack.IsCode(err, "relay_refused"), "%v", err)
	})
	t.Run("the door down", func(t *testing.T) {
		var took []relayed
		d := door(t, http.StatusServiceUnavailable, nil, `{"error":{"code":"backend_unavailable"}}`, &took)
		err := call(t, relayAt(d.URL, nil))
		var api *slack.APIError
		require.ErrorAs(t, err, &api)
		assert.Equal(t, http.StatusServiceUnavailable, api.Status)
		assert.False(t, slackGone(err), "tried again")
	})
	t.Run("unlinked", func(t *testing.T) {
		err := call(t, relayAt("https://ingest.invalid", store.ErrNotFound))
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
	t.Run("unreachable", func(t *testing.T) {
		d := httptest.NewServer(http.NotFoundHandler())
		d.Close()
		assert.Error(t, call(t, relayAt(d.URL, nil)))
	})
}

// A round trip names its method and local workspace, or goes nowhere.
func TestRelayedRefusesACallItCantName(t *testing.T) {
	r := relayAt("https://ingest.invalid", nil)
	for name, req := range map[string]*http.Request{
		"no bearer":  httptest.NewRequest(http.MethodPost, "https://slack.com/api/chat.postMessage", strings.NewReader(`{}`)),
		"no method":  withBearer(httptest.NewRequest(http.MethodPost, "https://slack.com/", strings.NewReader(`{}`))),
		"a bad form": withBearer(httptest.NewRequest(http.MethodPost, "https://slack.com/api/users.info", strings.NewReader(`%zz`))),
	} {
		_, err := r.RoundTrip(req)
		assert.Error(t, err, name)
	}
}

func withBearer(r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer ws-1")
	return r
}
