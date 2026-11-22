package slackbot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/helixdb/helix-foundry/backend/syncer"
)

// Transport is how the bot reaches Slack's Web API as an install (the
// spec's §10.4): hosted Helix holds each install's bot token (Direct); a
// standalone install calls Slack through Helix's relay, as the install
// Helix holds for it (Relayed).
type Transport interface {
	// Bot is installID's bot as the bot's slack.Client calls Slack for it:
	// store.ErrNotFound once the install has none.
	Bot(ctx context.Context, installID string) (slack.Bot, error)
	// Caller sends the Slack data connector's reads (the backfill's) as bot.
	Caller(bot slack.Bot) connector.SlackCaller
}

// Direct calls Slack with each install's own bot token, sealed by Keys
// under the install's id: hosted Helix.
type Direct struct {
	DB interface {
		GetFoundrySlackInstallToken(ctx context.Context, installID string) (model.SlackSealedToken, error)
	}
	Keys *secretbox.Keyring
	// WebAPI is Slack's Web API the data connector's reads go to:
	// https://slack.com/api when empty.
	WebAPI string
}

var _ Transport = Direct{}

// Bot is installID's bot with its token opened: store.ErrNotFound once
// Slack was told of its uninstall, which deletes the token.
func (d Direct) Bot(ctx context.Context, installID string) (slack.Bot, error) {
	sealed, err := d.DB.GetFoundrySlackInstallToken(ctx, installID)
	if err != nil {
		return slack.Bot{}, err
	}
	token, err := d.Keys.Open(sealed.Sealed, sealed.KeyID, installID)
	if err != nil {
		return slack.Bot{}, fmt.Errorf("open the install's token: %w", err)
	}
	return slack.Bot{InstallID: installID, Token: string(token)}, nil
}

// Caller sends bot's reads to WebAPI with its token.
func (d Direct) Caller(bot slack.Bot) connector.SlackCaller {
	return syncer.SlackBotCaller{BaseURL: d.WebAPI, Token: bot.Token}
}

// RelayAccess is a standalone install's link to Helix's relay, as its calls
// to Slack use it.
type RelayAccess struct {
	// Door is the origin of Helix's door the link was made with, whose
	// /v1/relay/slack/{method} calls Slack.
	Door string
	// Token is the link's hx_rl_ token, the relay's bearer.
	Token string
	// SlackApprovals says the owner's Slack account may approve changes
	// through Slack; off, every turn of the install's Slack only reads and
	// asks.
	SlackApprovals bool
}

// relayTimeout bounds one call through the relay: the door answers within
// its own 15 seconds.
const relayTimeout = 30 * time.Second

// maxRelayAnswer is the most of the relay's refusal read.
const maxRelayAnswer = 64 << 10

// relayRefusal starts the code of a call Helix's relay refused for good, as
// the bot's Web API calls read it (Relayed).
const relayRefusal = "relay_"

// Relayed calls Slack through Helix's relay, as the install Helix holds for
// a local workspace: a standalone install, which holds no bot token. It is
// an http.RoundTripper (Client): a Web API call of a slack.Client or a
// syncer.SlackBotCaller sent through it goes to the door's
// POST /v1/relay/slack/{method} as {"local_workspace_id", "args"}, under
// the link's token, whatever host it named. A relayed slack.Bot's Token is
// no secret: it names the local workspace whose install the relay calls
// as, which the round trip reads, and never leaves it.
//
// The relay's answer comes back as Slack's: Slack's body on 200, its 429 and
// Retry-After as they are (the relay's limits answer the same), and a
// refusal of the relay's own (a method, channel or user it doesn't allow, a
// link revoked) as Slack's ok:false with the code "relay_<code>", which the
// bot never tries again. Its 5xx stay HTTP errors, tried again.
type Relayed struct {
	DB interface {
		GetFoundrySlackInstallByID(ctx context.Context, id string) (*model.SlackInstall, error)
	}
	// Access reads the link as it stands: store.ErrNotFound while the
	// install isn't linked to Helix.
	Access func(ctx context.Context) (RelayAccess, error)
	// HTTP sends the calls to the door; one with relayTimeout when nil.
	HTTP *http.Client
}

var (
	_ Transport         = Relayed{}
	_ http.RoundTripper = Relayed{}
)

// Bot is installID's bot, while the install is live: its Token names the
// install's local workspace; store.ErrNotFound once it is revoked.
func (r Relayed) Bot(ctx context.Context, installID string) (slack.Bot, error) {
	install, err := r.DB.GetFoundrySlackInstallByID(ctx, installID)
	switch {
	case err != nil:
		return slack.Bot{}, err
	case install.RevokedAt != nil:
		return slack.Bot{}, store.ErrNotFound
	}
	return slack.Bot{InstallID: installID, Token: install.WorkspaceID}, nil
}

// Caller sends bot's reads through the relay.
func (r Relayed) Caller(bot slack.Bot) connector.SlackCaller {
	return syncer.SlackBotCaller{HTTP: r.Client(), Token: bot.Token}
}

// Callers is the data connector's way to Slack for a live install, as its
// syncs read it (syncer.SlackCallers).
func (r Relayed) Callers(install *model.SlackInstall) connector.SlackCaller {
	return r.Caller(slack.Bot{InstallID: install.ID, Token: install.WorkspaceID})
}

// Client is an HTTP client whose calls go through the relay (RoundTrip).
func (r Relayed) Client() *http.Client {
	return &http.Client{Transport: r, Timeout: relayTimeout}
}

// SlackApprovals reports whether the owner's Slack account may approve
// changes through Slack: never while the install isn't linked.
func (r Relayed) SlackApprovals(ctx context.Context) (bool, error) {
	access, err := r.Access(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return access.SlackApprovals, err
}

// RoundTrip sends one Web API call through the relay (Relayed): its method
// is the request path's last segment, its arguments the JSON body's object,
// the form's values or the query's, each as Slack takes them.
func (r Relayed) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		defer req.Body.Close()
	}
	method := path.Base(req.URL.Path)
	workspace, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !ok || workspace == "" || method == "" || method == "/" || method == "." {
		return nil, errors.New("slackbot: a relayed call names a method and, as its bearer, a local workspace")
	}
	var raw []byte
	if req.Body != nil {
		var err error
		if raw, err = io.ReadAll(req.Body); err != nil {
			return nil, err
		}
	}
	var args any
	switch {
	case req.Method == http.MethodGet:
		args = firstValues(req.URL.Query())
	case strings.HasPrefix(req.Header.Get("Content-Type"), "application/json"):
		args = json.RawMessage(raw)
	default:
		form, err := url.ParseQuery(string(raw))
		if err != nil {
			return nil, fmt.Errorf("slackbot: a relayed call's form: %w", err)
		}
		args = firstValues(form)
	}
	body, err := json.Marshal(map[string]any{"local_workspace_id": workspace, "args": args})
	if err != nil {
		return nil, err
	}
	access, err := r.Access(req.Context())
	if err != nil {
		return nil, fmt.Errorf("slackbot: the relay: %w", err)
	}
	out, err := http.NewRequestWithContext(req.Context(), http.MethodPost, strings.TrimRight(access.Door, "/")+"/v1/relay/slack/"+url.PathEscape(method),
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	out.Header.Set("Content-Type", "application/json")
	out.Header.Set("Authorization", "Bearer "+access.Token)
	client := r.HTTP
	if client == nil {
		client = &http.Client{Timeout: relayTimeout}
	}
	resp, err := client.Do(out)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= http.StatusInternalServerError {
		return resp, nil
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, maxRelayAnswer))
	var refusal struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	code := "refused"
	if json.Unmarshal(answer, &refusal) == nil && refusal.Error.Code != "" {
		code = refusal.Error.Code
	}
	said, _ := json.Marshal(map[string]any{"ok": false, "error": relayRefusal + code})
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Proto: resp.Proto, ProtoMajor: resp.ProtoMajor, ProtoMinor: resp.ProtoMinor,
		Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(said)), ContentLength: int64(len(said)),
		Request: req}, nil
}

// firstValues is each key's first value: Slack's arguments are one each.
func firstValues(v url.Values) map[string]string {
	out := make(map[string]string, len(v))
	for k := range v {
		out[k] = v.Get(k)
	}
	return out
}
