package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/slack"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The Slack tests run the API against an in-memory store of Slack's rows
// (the store's own tests hold its rules on MySQL) and a fake Slack.

const (
	slackSigningSecret = "the app's signing secret"
	slackTeam          = "T0AAA1111" // the fixtures' team
	slackAda           = "U0ADA0001" // the fixtures' person
	slackWS            = "ws-1"
)

// slackStore keeps the Slack rows the API reads and writes.
type slackStore struct {
	foundrystore.Unimplemented
	mu          sync.Mutex
	installs    []*model.SlackInstall
	tokens      map[string]model.SlackSealedToken
	states      map[string]*slackState
	linkTokens  map[string]*slackLinkToken
	links       []*model.SlackLink
	intake      []*model.SlackIntake
	uninstalls  []string
	uninstalled model.SlackUninstallOutcome
	refusal     error
	revoked     map[string]bool // install id → told Slack
	fail        error
}

type slackState struct {
	model.SlackInstallState
	ttl  time.Duration
	used bool
}

type slackLinkToken struct {
	model.SlackLinkToken
	used bool
}

func newSlackStore() *slackStore {
	return &slackStore{tokens: map[string]model.SlackSealedToken{}, states: map[string]*slackState{}, linkTokens: map[string]*slackLinkToken{},
		uninstalled: model.SlackUninstallDone, revoked: map[string]bool{}}
}

func (s *slackStore) live(match func(*model.SlackInstall) bool) (*model.SlackInstall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	for _, i := range s.installs {
		if i.RevokedAt == nil && match(i) {
			c := *i
			return &c, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *slackStore) GetFoundrySlackInstall(_ context.Context, ws string) (*model.SlackInstall, error) {
	return s.live(func(i *model.SlackInstall) bool { return i.WorkspaceID == ws })
}

func (s *slackStore) GetFoundrySlackInstallByTeam(_ context.Context, team string) (*model.SlackInstall, error) {
	return s.live(func(i *model.SlackInstall) bool { return i.TeamID == team })
}

func (s *slackStore) GetFoundrySlackInstallByEnterprise(_ context.Context, org string) (*model.SlackInstall, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return nil, s.fail
	}
	var found *model.SlackInstall
	for _, i := range s.installs {
		if i.RevokedAt != nil || org == "" || i.EnterpriseID != org {
			continue
		}
		if found != nil {
			return nil, foundrystore.ErrConflict
		}
		c := *i
		found = &c
	}
	if found == nil {
		return nil, foundrystore.ErrNotFound
	}
	return found, nil
}

func (s *slackStore) GetFoundrySlackInstallToken(_ context.Context, id string) (model.SlackSealedToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[id]
	if !ok {
		return model.SlackSealedToken{}, foundrystore.ErrNotFound
	}
	return t, nil
}

func (s *slackStore) InstallFoundrySlack(_ context.Context, in *model.SlackInstall, seal func(string) (model.SlackSealedToken, error)) (
	*model.SlackInstall, bool, error) {
	if err := in.Validate(); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refusal != nil {
		return nil, false, s.refusal
	}
	stored := *in
	reinstalled := false
	for i, held := range s.installs {
		if held.RevokedAt == nil && held.WorkspaceID == in.WorkspaceID && held.TeamID == in.TeamID {
			stored.ID, stored.CreatedAt, reinstalled = held.ID, held.CreatedAt, true
			s.installs = append(s.installs[:i], s.installs[i+1:]...)
			break
		}
	}
	token, err := seal(stored.ID)
	if err != nil {
		return nil, false, err
	}
	if stored.CreatedAt.IsZero() {
		stored.CreatedAt = time.Now()
	}
	stored.UpdatedAt, stored.Mode = time.Now(), model.SlackPerPerson{}
	s.tokens[stored.ID] = token
	s.installs = append(s.installs, &stored)
	c := stored
	return &c, reinstalled, nil
}

func (s *slackStore) RevokeFoundrySlackInstall(_ context.Context, ws, id string, uninstalled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, i := range s.installs {
		if i.ID == id && i.WorkspaceID == ws && i.RevokedAt == nil {
			now := time.Now()
			i.RevokedAt = &now
			s.revoked[id] = uninstalled
			if uninstalled {
				i.UninstalledAt = &now
				delete(s.tokens, id)
			}
			s.links = nil
			return nil
		}
	}
	return foundrystore.ErrNotFound
}

func (s *slackStore) UninstallFoundrySlackTeam(_ context.Context, team, intakeID, key string, at time.Time) (model.SlackUninstallOutcome, error) {
	if s.fail != nil {
		return "", s.fail
	}
	s.uninstalls = append(s.uninstalls, team+" "+key+" "+at.UTC().Format(time.RFC3339)+" "+strconv.Itoa(len(intakeID)))
	return s.uninstalled, nil
}

func (s *slackStore) CreateFoundrySlackInstallState(_ context.Context, ws, userID, hash, returnPath string, ttl time.Duration) error {
	s.states[hash] = &slackState{SlackInstallState: model.SlackInstallState{StateHash: hash, WorkspaceID: ws, UserID: userID,
		ReturnPath: returnPath, ExpiresAt: time.Now().Add(ttl)}, ttl: ttl}
	return nil
}

func (s *slackStore) SpendFoundrySlackInstallState(_ context.Context, hash, userID string) (*model.SlackInstallState, error) {
	st, ok := s.states[hash]
	if !ok || st.used || st.UserID != userID || time.Now().After(st.ExpiresAt) {
		return nil, foundrystore.ErrNotFound
	}
	st.used = true
	out := st.SlackInstallState
	return &out, nil
}

func (s *slackStore) GetFoundrySlackLinkToken(_ context.Context, hash string) (*model.SlackLinkToken, error) {
	t, ok := s.linkTokens[hash]
	if !ok || t.used || time.Now().After(t.ExpiresAt) {
		return nil, foundrystore.ErrNotFound
	}
	out := t.SlackLinkToken
	return &out, nil
}

func (s *slackStore) LinkFoundrySlackUser(ctx context.Context, ws, hash, userID string) (*model.SlackLink, error) {
	t, err := s.GetFoundrySlackLinkToken(ctx, hash)
	if err != nil {
		return nil, err
	}
	if _, err := s.live(func(i *model.SlackInstall) bool { return i.ID == t.InstallID && i.WorkspaceID == ws }); err != nil {
		return nil, err
	}
	s.linkTokens[hash].used = true
	kept := s.links[:0]
	for _, l := range s.links {
		if l.InstallID != t.InstallID || (l.SlackUserID != t.SlackUserID && l.UserID != userID) {
			kept = append(kept, l)
		}
	}
	link := &model.SlackLink{InstallID: t.InstallID, SlackUserID: t.SlackUserID, UserID: userID, CreatedAt: time.Now()}
	s.links = append(kept, link)
	return link, nil
}

func (s *slackStore) ListFoundrySlackLinks(_ context.Context, installID string) ([]*model.SlackLink, error) {
	if s.fail != nil {
		return nil, s.fail
	}
	var out []*model.SlackLink
	for _, l := range s.links {
		if l.InstallID == installID {
			out = append(out, l)
		}
	}
	return out, nil
}

func (s *slackStore) GetFoundrySlackLink(_ context.Context, installID, slackUserID string) (*model.SlackLink, error) {
	for _, l := range s.links {
		if l.InstallID == installID && l.SlackUserID == slackUserID {
			return l, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *slackStore) DeleteFoundrySlackLink(_ context.Context, installID, slackUserID string) error {
	for i, l := range s.links {
		if l.InstallID == installID && l.SlackUserID == slackUserID {
			s.links = append(s.links[:i], s.links[i+1:]...)
			return nil
		}
	}
	return foundrystore.ErrNotFound
}

func (s *slackStore) QueueFoundrySlackIntake(_ context.Context, in *model.SlackIntake) error {
	if err := in.Validate(); err != nil {
		return err
	}
	if s.fail != nil {
		return s.fail
	}
	for _, held := range s.intake {
		if held.DedupeKey == in.DedupeKey {
			return foundrystore.ErrConflict
		}
	}
	s.intake = append(s.intake, in)
	return nil
}

// install adds a live install of the fixtures' team for slackWS, its bot
// token sealed as the API seals it.
func (s *slackStore) install(t *testing.T, keys *secretbox.Keyring, mode model.SlackIdentityMode) *model.SlackInstall {
	t.Helper()
	i := &model.SlackInstall{ID: "inst-1", WorkspaceID: slackWS, TeamID: slackTeam, TeamName: "Acme", AppID: "A0FOUNDRY", BotUserID: "U0BOT0001",
		Scopes: slack.Scopes, InstalledByUserID: "user-1", InstalledBySlackUserID: slackAda, Mode: mode,
		CreatedAt: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)}
	sealed, keyID, err := keys.Seal([]byte("xoxb-live"), i.ID)
	require.NoError(t, err)
	s.installs = append(s.installs, i)
	s.tokens[i.ID] = model.SlackSealedToken{Sealed: sealed, KeyID: keyID}
	return i
}

// linkToken records a live link token for the Slack user, returning it.
func (s *slackStore) linkToken(installID, slackUserID string) string {
	raw := slackLinkTokenPrefix + strings.Repeat("k", 43) + slackUserID
	s.linkTokens[hashToken(raw)] = &slackLinkToken{SlackLinkToken: model.SlackLinkToken{TokenHash: hashToken(raw), InstallID: installID,
		SlackUserID: slackUserID, ExpiresAt: time.Now().Add(10 * time.Minute)}}
	return raw
}

// fakeSlack is Slack's Web API, recording each call.
type fakeSlack struct {
	*httptest.Server
	mu    sync.Mutex
	calls []slackCall
	// answers by method; a method without one answers {"ok": true}.
	answers map[string]func(r *http.Request, body []byte) (int, string)
}

type slackCall struct {
	method, auth string
	body         []byte
}

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{answers: map[string]func(*http.Request, []byte) (int, string){}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		method := strings.TrimPrefix(r.URL.Path, "/api/")
		f.mu.Lock()
		f.calls = append(f.calls, slackCall{method: method, auth: r.Header.Get("Authorization"), body: body})
		answer := f.answers[method]
		f.mu.Unlock()
		code, out := http.StatusOK, `{"ok":true}`
		if answer != nil {
			code, out = answer(r, body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSlack) answer(method string, code int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[method] = func(*http.Request, []byte) (int, string) { return code, body }
}

// called is the methods called, in order.
func (f *fakeSlack) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.method)
	}
	return out
}

func (f *fakeSlack) call(method string) slackCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.method == method {
			return c
		}
	}
	return slackCall{}
}

// slackUserJSON is users.info's answer for a user of the fixtures' team.
func slackUserJSON(id, email string, mutate func(map[string]any)) string {
	u := map[string]any{"id": id, "team_id": slackTeam, "name": "ada", "real_name": "Ada Lovelace", "is_email_confirmed": true,
		"profile": map[string]any{"display_name": "Ada", "email": email}}
	if mutate != nil {
		mutate(u)
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "user": u})
	return string(b)
}

// slackPeople is the workspace's members: their roles, names and emails.
type slackPeople struct {
	roles  map[string]ports.Role
	emails map[string]string
	err    error
}

func (p slackPeople) MayManageAI(context.Context, string, string) bool { return false }

func (p slackPeople) DisplayName(_ context.Context, userID string) (string, error) {
	if userID == "gone" {
		return "", foundrystore.ErrNotFound
	}
	return "Name of " + userID, nil
}

func (p slackPeople) Member(_ context.Context, ws, userID string) (ports.Member, error) {
	if p.err != nil {
		return ports.Member{}, p.err
	}
	if ws != slackWS {
		return ports.Member{}, nil
	}
	return ports.Member{Role: p.roles[userID]}, nil
}

func (p slackPeople) UserEmail(_ context.Context, userID string) (string, error) {
	if p.err != nil {
		return "", p.err
	}
	email, ok := p.emails[userID]
	if !ok {
		return "", foundrystore.ErrNotFound
	}
	return email, nil
}

type slackFixture struct {
	s     *Service
	db    *slackStore
	slack *fakeSlack
	keys  *secretbox.Keyring
	cfg   *config.Config
}

func newSlackFixture(t *testing.T) slackFixture {
	t.Helper()
	cfg := config.Defaults()
	cfg.Foundry.Agent.Enabled = true
	cfg.Foundry.Slack.Enabled = true
	cfg.Foundry.Slack.ClientID, cfg.Foundry.Slack.AppID = "1.2", "A0FOUNDRY"
	cfg.Foundry.Slack.ClientSecret, cfg.Foundry.Slack.SigningSecret = "client secret", slackSigningSecret
	cfg.Foundry.Slack.SiteURL = "https://foundry.example.com"
	keys, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)})
	require.NoError(t, err)
	fake := newFakeSlack(t)
	limiter, err := slack.NewLimiter(1)
	require.NoError(t, err)
	client, err := slack.NewClient(slack.ClientOptions{BaseURL: fake.URL + "/api", Limiter: limiter})
	require.NoError(t, err)
	people := slackPeople{roles: map[string]ports.Role{"user-1": ports.RoleOwner, "user-2": ports.RoleMember, "user-3": ports.RoleAdmin},
		emails: map[string]string{"user-1": "Ada@Acme.com", "user-2": "grace@acme.com"}}
	db := newSlackStore()
	return slackFixture{s: &Service{deps: &runtimeDeps{Cfg: &cfg, DB: db, People: people, Emails: people, ConnectorKeys: keys, Slack: client,
		SlackURL: fake.URL}}, db: db, slack: fake, keys: keys, cfg: &cfg}
}

// slackReason is a refusal's ErrorInfo reason, and its metadata.
func slackReason(t *testing.T, err error) (string, map[string]string) {
	t.Helper()
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.GetReason(), info.GetMetadata()
		}
	}
	return "", nil
}

// signed is a request Slack signed now.
func signed(route foundryv1.SlackRoute, body []byte) *foundryv1.ReceiveSlackRequestRequest {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	return &foundryv1.ReceiveSlackRequestRequest{Route: route, Body: body, Timestamp: ts, Signature: slack.Sign(slackSigningSecret, ts, body)}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../slack/testdata/" + name)
	require.NoError(t, err)
	return b
}

// clickBody is an interactivity form around a fixture's payload.
func clickBody(t *testing.T, name string) []byte {
	return []byte(url.Values{"payload": {string(fixture(t, "interactions/"+name))}}.Encode())
}

func TestReceiveSlackRequestRefusals(t *testing.T) {
	f := newSlackFixture(t)
	body := fixture(t, "events/mention.json")
	for name, ctx := range map[string]context.Context{
		"a person":          as("user-1"),
		"a sender":          ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceEventSource, EventSourceTokenID: "tok"}),
		"a door for a user": ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceEventSource, UserID: "u"}),
	} {
		_, err := f.s.ReceiveSlackRequest(ctx, signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, body))
		assert.Equal(t, codes.PermissionDenied, status.Code(err), name)
	}
	_, err := f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_UNSPECIFIED, body))
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	answers := func(req *foundryv1.ReceiveSlackRequestRequest) int32 {
		resp, err := f.s.ReceiveSlackRequest(door(), req)
		require.NoError(t, err)
		return resp.GetHttpStatus()
	}
	bad := signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, body)
	bad.Signature = slack.Sign("another secret", bad.Timestamp, body)
	assert.EqualValues(t, http.StatusUnauthorized, answers(bad), "another app's signature")
	tampered := signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, body)
	tampered.Body = append([]byte(nil), body...)
	tampered.Body[len(tampered.Body)-2] = ' '
	assert.EqualValues(t, http.StatusUnauthorized, answers(tampered), "a body changed on the way")
	stale := &foundryv1.ReceiveSlackRequestRequest{Route: foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, Body: body,
		Timestamp: strconv.FormatInt(time.Now().Add(-6*time.Minute).Unix(), 10)}
	stale.Signature = slack.Sign(slackSigningSecret, stale.Timestamp, body)
	assert.EqualValues(t, http.StatusUnauthorized, answers(stale), "a replay")
	malformed := signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, body)
	malformed.Signature = "v1=abc"
	assert.EqualValues(t, http.StatusUnauthorized, answers(malformed))
	f.cfg.Foundry.Slack.SigningSecret = ""
	assert.EqualValues(t, http.StatusUnauthorized, answers(signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, body)), "no secret verifies nothing")
	f.cfg.Foundry.Slack.Enabled = false
	assert.EqualValues(t, http.StatusNotFound, answers(signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, body)), "off")
	assert.Empty(t, f.db.intake)
}

func TestReceiveSlackEvents(t *testing.T) {
	receive := func(t *testing.T, f slackFixture, body []byte) *foundryv1.ReceiveSlackRequestResponse {
		t.Helper()
		resp, err := f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, body))
		require.NoError(t, err)
		return resp
	}
	t.Run("answers Slack's handshake", func(t *testing.T) {
		f := newSlackFixture(t)
		resp := receive(t, f, fixture(t, "events/url_verification.json"))
		assert.EqualValues(t, http.StatusOK, resp.GetHttpStatus())
		assert.Equal(t, "application/json", resp.GetContentType())
		var got map[string]string
		require.NoError(t, json.Unmarshal(resp.GetBody(), &got))
		var sent map[string]string
		require.NoError(t, json.Unmarshal(fixture(t, "events/url_verification.json"), &sent))
		assert.Equal(t, map[string]string{"challenge": sent["challenge"]}, got)
	})
	t.Run("queues a mention once, as its person's conversation", func(t *testing.T) {
		f := newSlackFixture(t)
		install := f.db.install(t, f.keys, model.SlackPerPerson{})
		resp := receive(t, f, fixture(t, "events/mention.json"))
		assert.EqualValues(t, http.StatusOK, resp.GetHttpStatus())
		assert.Empty(t, resp.GetBody())
		require.Len(t, f.db.intake, 1)
		in := f.db.intake[0]
		assert.Equal(t, install.ID, in.InstallID)
		assert.Equal(t, "e:Ev0MENTION1", in.DedupeKey)
		assert.Equal(t, model.SlackIntakeEvent, in.Kind)
		assert.Equal(t, slackAda, in.SlackUserID)
		assert.Equal(t, model.SlackConversation(install.ID, "C0GEN0001", "1795000000.000100", slackAda), in.Conversation)
		p, err := slack.ReadPayload(in.Payload)
		require.NoError(t, err)
		e, err := slack.DecodeEvent(p.Kind, p.Data)
		require.NoError(t, err)
		mention, ok := e.(slack.Mention)
		require.True(t, ok)
		assert.Equal(t, "<@U0BOT0001> which customers renewed last month?", mention.Text)
		// Slack's retry of it is taken, once.
		receive(t, f, fixture(t, "events/mention.json"))
		assert.Len(t, f.db.intake, 1)
	})
	t.Run("a bot's install has one conversation a thread", func(t *testing.T) {
		f := newSlackFixture(t)
		install := f.db.install(t, f.keys, model.SlackSharedBot{CredentialID: "cred-1"})
		receive(t, f, fixture(t, "events/dm.json"))
		require.Len(t, f.db.intake, 1)
		assert.Equal(t, model.SlackConversation(install.ID, "D0DM00001", "1795000010.000100", ""), f.db.intake[0].Conversation)
		assert.Equal(t, slackAda, f.db.intake[0].SlackUserID)
	})
	t.Run("a stop waits behind nothing", func(t *testing.T) {
		f := newSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		receive(t, f, fixture(t, "events/agent_session_stopped.json"))
		require.Len(t, f.db.intake, 1)
		assert.Empty(t, f.db.intake[0].Conversation)
		p, err := slack.ReadPayload(f.db.intake[0].Payload)
		require.NoError(t, err)
		assert.Equal(t, slack.KindStopped, p.Kind)
	})
	t.Run("takes nothing it doesn't act on", func(t *testing.T) {
		f := newSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		for _, name := range []string{"dm_bot_message.json", "dm_edit.json", "member_joined.json", "tokens_revoked_user.json"} {
			resp := receive(t, f, fixture(t, "events/"+name))
			assert.EqualValues(t, http.StatusOK, resp.GetHttpStatus(), name)
		}
		var other map[string]any
		require.NoError(t, json.Unmarshal(fixture(t, "events/mention.json"), &other))
		other["api_app_id"] = "A0OTHER"
		b, _ := json.Marshal(other)
		assert.EqualValues(t, http.StatusOK, receive(t, f, b).GetHttpStatus(), "another app's event")
		assert.Empty(t, f.db.intake)
		assert.Empty(t, f.db.uninstalls)
	})
	t.Run("a team with no install", func(t *testing.T) {
		f := newSlackFixture(t)
		resp := receive(t, f, fixture(t, "events/mention.json"))
		assert.EqualValues(t, http.StatusOK, resp.GetHttpStatus())
		assert.Empty(t, f.db.intake)
	})
	t.Run("a body Slack doesn't send", func(t *testing.T) {
		f := newSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		long := strings.Replace(string(fixture(t, "events/mention.json")), "Ev0MENTION1", "Ev"+strings.Repeat("X", 40), 1)
		for name, body := range map[string][]byte{"not JSON": []byte("{"), "an event id the store can't keep": []byte(long)} {
			assert.EqualValues(t, http.StatusBadRequest, receive(t, f, body).GetHttpStatus(), name)
		}
		uninstall := strings.Replace(string(fixture(t, "events/app_uninstalled.json")), "Ev0UNINST01", "Ev"+strings.Repeat("X", 40), 1)
		assert.EqualValues(t, http.StatusBadRequest, receive(t, f, []byte(uninstall)).GetHttpStatus())
		assert.Empty(t, f.db.intake)
	})
	t.Run("ends an uninstalled install, by the event's time", func(t *testing.T) {
		for outcome, want := range map[model.SlackUninstallOutcome]int32{
			model.SlackUninstallDone: http.StatusOK, model.SlackUninstallStale: http.StatusOK,
			model.SlackUninstallUnknownTeam: http.StatusOK, model.SlackUninstallDuplicate: http.StatusOK,
		} {
			f := newSlackFixture(t)
			f.db.uninstalled = outcome
			assert.EqualValues(t, want, receive(t, f, fixture(t, "events/app_uninstalled.json")).GetHttpStatus(), outcome)
			assert.Equal(t, []string{"T0AAA1111 e:Ev0UNINST01 2026-11-18T11:06:40Z 26"}, f.db.uninstalls, outcome)
		}
		f := newSlackFixture(t)
		receive(t, f, fixture(t, "events/tokens_revoked.json"))
		assert.Equal(t, []string{"T0AAA1111 e:Ev0REVOKE01 2026-11-18T11:06:40Z 26"}, f.db.uninstalls, "the bot's token revoked")
		f.db.uninstalled = "something else"
		_, err := f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, fixture(t, "events/app_uninstalled.json")))
		assert.Equal(t, codes.Unavailable, status.Code(err), "an outcome the API doesn't know")
	})
	t.Run("a store that fails is retried by Slack", func(t *testing.T) {
		for _, name := range []string{"mention.json", "app_uninstalled.json"} {
			f := newSlackFixture(t)
			f.db.install(t, f.keys, model.SlackPerPerson{})
			f.db.fail = errors.New("the database is down")
			_, err := f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, fixture(t, "events/"+name)))
			assert.Equal(t, codes.Unavailable, status.Code(err), name)
			assert.NotContains(t, status.Convert(err).Message(), "database", "the door's answer says nothing of why")
		}
	})
}

func TestReceiveSlackClicks(t *testing.T) {
	receive := func(t *testing.T, f slackFixture, body []byte) int32 {
		t.Helper()
		resp, err := f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_INTERACTIVITY, body))
		require.NoError(t, err)
		return resp.GetHttpStatus()
	}
	t.Run("queues each click the bot acts on, once", func(t *testing.T) {
		f := newSlackFixture(t)
		install := f.db.install(t, f.keys, model.SlackPerPerson{})
		for _, name := range []string{"approve.json", "dismiss.json", "refresh.json", "review.json", "run_ephemeral.json"} {
			assert.EqualValues(t, http.StatusOK, receive(t, f, clickBody(t, name)), name)
		}
		require.Len(t, f.db.intake, 5)
		for _, in := range f.db.intake {
			assert.Equal(t, install.ID, in.InstallID)
			assert.Equal(t, model.SlackIntakeInteraction, in.Kind)
			assert.Empty(t, in.Conversation, "a click waits behind no message")
			p, err := slack.ReadPayload(in.Payload)
			require.NoError(t, err)
			click, err := slack.DecodeInteraction(p.Kind, p.Data)
			require.NoError(t, err)
			assert.Equal(t, slack.InteractionKey(click), in.DedupeKey)
		}
		receive(t, f, clickBody(t, "approve.json"))
		assert.Len(t, f.db.intake, 5, "a click taken once")
	})
	t.Run("stop and unlink buttons", func(t *testing.T) {
		f := newSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		for _, button := range [][2]string{{slack.ActionStop, `{"t":"01JBTRN0000000000000000000"}`}, {slack.ActionUnlink, ""}} {
			var p map[string]any
			require.NoError(t, json.Unmarshal(fixture(t, "interactions/dismiss.json"), &p))
			action := p["actions"].([]any)[0].(map[string]any)
			action["action_id"], action["value"] = button[0], button[1]
			b, _ := json.Marshal(p)
			assert.EqualValues(t, http.StatusOK, receive(t, f, []byte(url.Values{"payload": {string(b)}}.Encode())), button[0])
		}
		require.Len(t, f.db.intake, 2)
		for i, kind := range []slack.Kind{slack.KindStop, slack.KindUnlink} {
			p, err := slack.ReadPayload(f.db.intake[i].Payload)
			require.NoError(t, err)
			assert.Equal(t, kind, p.Kind)
		}
	})
	t.Run("leaves what isn't the bot's", func(t *testing.T) {
		f := newSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		for _, name := range []string{"forged_value.json", "in_view.json", "link_button.json", "not_json_value.json", "org_install.json"} {
			assert.EqualValues(t, http.StatusOK, receive(t, f, clickBody(t, name)), name)
		}
		assert.EqualValues(t, http.StatusBadRequest, receive(t, f, []byte("payload=%7B")), "not a payload")
		assert.Empty(t, f.db.intake)
	})
	t.Run("a team with no install", func(t *testing.T) {
		f := newSlackFixture(t)
		assert.EqualValues(t, http.StatusOK, receive(t, f, clickBody(t, "approve.json")))
		assert.Empty(t, f.db.intake)
	})
	t.Run("a Grid click from another workspace of the org", func(t *testing.T) {
		// approve_grid.json is clicked from T0GRIDWS01's session in a channel
		// the org's workspaces share; the app is installed in another of them.
		f := newSlackFixture(t)
		install := f.db.install(t, f.keys, model.SlackPerPerson{})
		install.TeamID, install.EnterpriseID = "T0GRIDWS02", "E0ACME001"
		assert.EqualValues(t, http.StatusOK, receive(t, f, clickBody(t, "approve_grid.json")))
		require.Len(t, f.db.intake, 1, "the org's one install takes it")
		assert.Equal(t, install.ID, f.db.intake[0].InstallID)

		f.db.intake = nil
		f.db.installs = append(f.db.installs, &model.SlackInstall{ID: "inst-2", WorkspaceID: "ws-2", TeamID: "T0GRIDWS03", EnterpriseID: "E0ACME001"})
		assert.EqualValues(t, http.StatusOK, receive(t, f, clickBody(t, "approve_grid.json")))
		assert.Empty(t, f.db.intake, "with two installs in the org, the click can't be told apart")

		f.db.installs = f.db.installs[:1]
		install.EnterpriseID = "E0OTHER01"
		assert.EqualValues(t, http.StatusOK, receive(t, f, clickBody(t, "approve_grid.json")))
		assert.Empty(t, f.db.intake, "another org's install never takes it")
		install.EnterpriseID = ""
		assert.EqualValues(t, http.StatusOK, receive(t, f, clickBody(t, "approve_grid.json")))
		assert.Empty(t, f.db.intake, "nor an install outside any org")
	})
	t.Run("a store that fails", func(t *testing.T) {
		f := newSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		f.db.fail = errors.New("down")
		_, err := f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_INTERACTIVITY, clickBody(t, "approve.json")))
		assert.Equal(t, codes.Unavailable, status.Code(err))
	})
}

func TestGetFoundrySlack(t *testing.T) {
	get := func(f slackFixture, user string) (*foundryv1.FoundrySlack, error) {
		resp, err := f.s.GetFoundrySlack(as(user), &foundryv1.GetFoundrySlackRequest{WorkspaceId: slackWS})
		return resp.GetSlack(), err
	}
	t.Run("before an install", func(t *testing.T) {
		f := newSlackFixture(t)
		got, err := get(f, "user-1")
		require.NoError(t, err)
		assert.True(t, got.GetInstallAllowed())
		assert.True(t, got.GetCanManage())
		assert.Nil(t, got.GetInstall())
		got, err = get(f, "user-2")
		require.NoError(t, err)
		assert.False(t, got.GetCanManage(), "a member")
		f.cfg.Foundry.Slack.InstallAllowlist = []string{"ws-other"}
		got, err = get(f, "user-3")
		require.NoError(t, err)
		assert.False(t, got.GetInstallAllowed())
		assert.True(t, got.GetCanManage(), "an admin")
	})
	t.Run("an install and the caller's link", func(t *testing.T) {
		f := newSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		f.db.links = []*model.SlackLink{{InstallID: "inst-1", SlackUserID: "U0GRACE01", UserID: "user-2"}, {InstallID: "inst-1", SlackUserID: slackAda, UserID: "user-1"}}
		got, err := get(f, "user-1")
		require.NoError(t, err)
		assert.Equal(t, "Acme", got.GetInstall().GetTeamName())
		assert.Equal(t, slackTeam, got.GetInstall().GetTeamId())
		assert.Equal(t, "U0BOT0001", got.GetInstall().GetBotUserId())
		assert.Equal(t, "Name of user-1", got.GetInstall().GetInstalledByName())
		assert.Equal(t, time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), got.GetInstall().GetCreatedAt().AsTime())
		assert.Equal(t, slackAda, got.GetMyLink().GetSlackUserId())
		got, err = get(f, "user-3")
		require.NoError(t, err)
		assert.Nil(t, got.GetMyLink(), "an admin not linked")
	})
	t.Run("refused", func(t *testing.T) {
		f := newSlackFixture(t)
		_, err := f.s.GetFoundrySlack(as("user-1"), &foundryv1.GetFoundrySlackRequest{})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		for name, ctx := range map[string]context.Context{
			"an agent": ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceMCP, UserID: "user-1"}),
			"a key":    ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceMCP, ServiceCredentialID: "cred"}),
			"a grantee from the host": ports.WithActor(context.Background(),
				ports.Actor{Source: ports.SourceGrantee, UserID: "user-2", WorkspaceID: slackWS}),
		} {
			_, err := f.s.GetFoundrySlack(ctx, &foundryv1.GetFoundrySlackRequest{WorkspaceId: slackWS})
			assert.Equal(t, codes.PermissionDenied, status.Code(err), name)
		}
		f.db.fail = errors.New("down")
		_, err = get(f, "user-1")
		assert.Equal(t, codes.Internal, status.Code(err))
		f.db.fail = nil
		f.s.deps.People = slackPeople{err: errors.New("down")}
		_, err = get(f, "user-1")
		assert.Equal(t, codes.Unavailable, status.Code(err))
		f.cfg.Foundry.Slack.Enabled = false
		_, err = get(f, "user-1")
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_DISABLED", reason)
	})
	t.Run("a host with no one to ask manages nothing", func(t *testing.T) {
		f := newSlackFixture(t)
		f.s.deps.People = nil
		got, err := get(f, "user-1")
		require.NoError(t, err)
		assert.False(t, got.GetCanManage())
	})
}

func TestStartFoundrySlackInstall(t *testing.T) {
	start := func(f slackFixture, user string) (*foundryv1.StartFoundrySlackInstallResponse, error) {
		return f.s.StartFoundrySlackInstall(as(user), &foundryv1.StartFoundrySlackInstallRequest{WorkspaceId: slackWS})
	}
	t.Run("sends an owner to Slack with a state of theirs", func(t *testing.T) {
		f := newSlackFixture(t)
		resp, err := start(f, "user-1")
		require.NoError(t, err)
		u, err := url.Parse(resp.GetAuthorizeUrl())
		require.NoError(t, err)
		assert.Equal(t, f.slack.URL+"/oauth/v2/authorize", u.Scheme+"://"+u.Host+u.Path)
		q := u.Query()
		assert.Equal(t, "1.2", q.Get("client_id"))
		assert.Equal(t, strings.Join(slack.Scopes, ","), q.Get("scope"))
		assert.Equal(t, "https://foundry.example.com/slack/installed", q.Get("redirect_uri"))
		assert.Empty(t, q.Get("user_scope"), "no user scopes")
		state := q.Get("state")
		assert.Len(t, state, 43, "32 random bytes")
		st := f.db.states[hashToken(state)]
		require.NotNil(t, st, "only its hash is kept")
		assert.Equal(t, slackWS, st.WorkspaceID)
		assert.Equal(t, "user-1", st.UserID)
		assert.Equal(t, 10*time.Minute, st.ttl)
		again, err := start(f, "user-3")
		require.NoError(t, err)
		assert.NotEqual(t, resp.GetAuthorizeUrl(), again.GetAuthorizeUrl(), "each start its own state")
	})
	t.Run("refused", func(t *testing.T) {
		f := newSlackFixture(t)
		_, err := start(f, "user-2")
		assert.Equal(t, codes.PermissionDenied, status.Code(err), "a member")
		_, err = start(f, "stranger")
		assert.Equal(t, codes.PermissionDenied, status.Code(err), "no member")
		_, err = f.s.StartFoundrySlackInstall(as("user-1"), &foundryv1.StartFoundrySlackInstallRequest{})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		f.cfg.Foundry.Slack.InstallAllowlist = []string{"ws-other"}
		_, err = start(f, "user-1")
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_NOT_OFFERED", reason)
		f.cfg.Foundry.Slack.InstallAllowlist = []string{"ws-other", slackWS}
		f.cfg.Foundry.Agent.Enabled = false
		_, err = start(f, "user-1")
		reason, _ = slackReason(t, err)
		assert.Equal(t, "FOUNDRY_AGENT_DISABLED", reason)
		f.cfg.Foundry.Agent.Enabled = true
		f.db.install(t, f.keys, model.SlackPerPerson{})
		_, err = start(f, "user-1")
		reason, _ = slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_INSTALLED", reason)
		f.db.fail = errors.New("down")
		_, err = start(f, "user-1")
		assert.Equal(t, codes.Internal, status.Code(err))
		assert.Empty(t, f.db.states)
	})
}

// grantJSON is oauth.v2.access's answer for the fixtures' team.
func grantJSON(mutate func(map[string]any)) string {
	g := map[string]any{"ok": true, "app_id": "A0FOUNDRY", "access_token": "xoxb-new", "token_type": "bot", "scope": strings.Join(slack.Scopes, ","),
		"bot_user_id": "U0BOT0001", "team": map[string]any{"id": slackTeam, "name": "Acme"}, "enterprise": nil,
		"is_enterprise_install": false, "authed_user": map[string]any{"id": slackAda}}
	if mutate != nil {
		mutate(g)
	}
	b, _ := json.Marshal(g)
	return string(b)
}

func TestFinishFoundrySlackInstall(t *testing.T) {
	// started is a state user-1 started for slackWS.
	started := func(t *testing.T, f slackFixture) string {
		t.Helper()
		resp, err := f.s.StartFoundrySlackInstall(as("user-1"), &foundryv1.StartFoundrySlackInstallRequest{WorkspaceId: slackWS})
		require.NoError(t, err)
		u, _ := url.Parse(resp.GetAuthorizeUrl())
		return u.Query().Get("state")
	}
	finish := func(f slackFixture, user, state string) (*foundryv1.FoundrySlack, error) {
		resp, err := f.s.FinishFoundrySlackInstall(as(user), &foundryv1.FinishFoundrySlackInstallRequest{Code: "the code", State: state})
		return resp.GetSlack(), err
	}
	t.Run("installs, with the token sealed under the install", func(t *testing.T) {
		f := newSlackFixture(t)
		f.slack.answer("oauth.v2.access", http.StatusOK, grantJSON(nil))
		state := started(t, f)
		got, err := finish(f, "user-1", state)
		require.NoError(t, err)
		assert.Equal(t, "Acme", got.GetInstall().GetTeamName())
		require.Len(t, f.db.installs, 1)
		in := f.db.installs[0]
		assert.Equal(t, slackWS, in.WorkspaceID)
		assert.Equal(t, "user-1", in.InstalledByUserID)
		assert.Equal(t, slackAda, in.InstalledBySlackUserID)
		assert.Equal(t, slack.Scopes, in.Scopes)
		sealed := f.db.tokens[in.ID]
		token, err := f.keys.Open(sealed.Sealed, sealed.KeyID, in.ID)
		require.NoError(t, err)
		assert.Equal(t, "xoxb-new", string(token))
		call := f.slack.call("oauth.v2.access")
		form, err := url.ParseQuery(string(call.body))
		require.NoError(t, err)
		assert.Equal(t, "the code", form.Get("code"))
		assert.Equal(t, "https://foundry.example.com/slack/installed", form.Get("redirect_uri"))
		assert.True(t, strings.HasPrefix(call.auth, "Basic "), "the app's credentials")
		// The state is spent.
		_, err = finish(f, "user-1", state)
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_INSTALL_EXPIRED", reason)
	})
	t.Run("again is a reinstall", func(t *testing.T) {
		f := newSlackFixture(t)
		f.slack.answer("oauth.v2.access", http.StatusOK, grantJSON(nil))
		state := started(t, f)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		_, err := finish(f, "user-1", state)
		require.NoError(t, err)
		require.Len(t, f.db.installs, 1)
		assert.Equal(t, "inst-1", f.db.installs[0].ID, "the same install")
		token, err := f.keys.Open(f.db.tokens["inst-1"].Sealed, f.db.tokens["inst-1"].KeyID, "inst-1")
		require.NoError(t, err)
		assert.Equal(t, "xoxb-new", string(token), "with its new token")
	})
	t.Run("a state that isn't the caller's to spend", func(t *testing.T) {
		f := newSlackFixture(t)
		state := started(t, f)
		for name, s := range map[string]string{"unknown": "nope", "another's": state} {
			_, err := finish(f, "user-3", s)
			reason, _ := slackReason(t, err)
			assert.Equal(t, "FOUNDRY_SLACK_INSTALL_EXPIRED", reason, name)
		}
		f.db.states[hashToken(state)].ExpiresAt = time.Now().Add(-time.Second)
		_, err := finish(f, "user-1", state)
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_INSTALL_EXPIRED", reason, "expired")
		assert.Empty(t, f.slack.called(), "Slack isn't asked")
	})
	t.Run("an owner no longer one before Slack returns", func(t *testing.T) {
		f := newSlackFixture(t)
		state := started(t, f)
		f.s.deps.People = slackPeople{roles: map[string]ports.Role{"user-1": ports.RoleMember}}
		_, err := finish(f, "user-1", state)
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		assert.Empty(t, f.slack.called())
		assert.Empty(t, f.db.installs)
	})
	t.Run("a workspace taken off the list meanwhile", func(t *testing.T) {
		f := newSlackFixture(t)
		state := started(t, f)
		f.cfg.Foundry.Slack.InstallAllowlist = []string{"ws-other"}
		_, err := finish(f, "user-1", state)
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_NOT_OFFERED", reason)
	})
	t.Run("what Slack grants is refused", func(t *testing.T) {
		for name, c := range map[string]struct {
			code   int
			answer string
			reason string
			grpc   codes.Code
		}{
			"a code Slack refuses": {http.StatusOK, `{"ok":false,"error":"invalid_code"}`, "FOUNDRY_SLACK_REFUSED", codes.FailedPrecondition},
			"Slack down":           {http.StatusBadGateway, `{}`, "", codes.Unavailable},
			"an org-wide install": {http.StatusOK, grantJSON(func(g map[string]any) {
				g["is_enterprise_install"], g["team"], g["enterprise"] = true, nil, map[string]any{"id": "E0ORG0001"}
			}), "FOUNDRY_SLACK_ENTERPRISE_INSTALL", codes.FailedPrecondition},
			"another app": {http.StatusOK, grantJSON(func(g map[string]any) { g["app_id"] = "A0OTHER" }), "", codes.Internal},
			"scopes missing": {http.StatusOK, grantJSON(func(g map[string]any) { g["scope"] = "chat:write,users:read" }),
				"FOUNDRY_SLACK_SCOPES", codes.FailedPrecondition},
		} {
			f := newSlackFixture(t)
			f.slack.answer("oauth.v2.access", c.code, c.answer)
			_, err := finish(f, "user-1", started(t, f))
			assert.Equal(t, c.grpc, status.Code(err), name)
			reason, _ := slackReason(t, err)
			assert.Equal(t, c.reason, reason, name)
			assert.Empty(t, f.db.installs, name)
		}
	})
	t.Run("the store refuses", func(t *testing.T) {
		for reason, refusal := range map[string]error{
			"FOUNDRY_SLACK_TEAM_TAKEN":    model.SlackRefusal{Reason: model.SlackTeamTaken},
			"FOUNDRY_SLACK_DISCONNECTING": model.SlackRefusal{Reason: model.SlackDisconnecting},
			"FOUNDRY_SLACK_INSTALLED":     model.SlackRefusal{Reason: model.SlackInstalled},
		} {
			f := newSlackFixture(t)
			f.slack.answer("oauth.v2.access", http.StatusOK, grantJSON(nil))
			f.db.refusal = refusal
			_, err := finish(f, "user-1", started(t, f))
			got, _ := slackReason(t, err)
			assert.Equal(t, reason, got)
		}
		for want, err := range map[codes.Code]error{
			codes.NotFound: foundrystore.ErrNotFound,
			codes.Internal: model.SlackRefusal{Reason: "unheard of"},
		} {
			f := newSlackFixture(t)
			f.slack.answer("oauth.v2.access", http.StatusOK, grantJSON(nil))
			f.db.refusal = err
			_, got := finish(f, "user-1", started(t, f))
			assert.Equal(t, want, status.Code(got), err.Error())
		}
		f := newSlackFixture(t)
		f.slack.answer("oauth.v2.access", http.StatusOK, grantJSON(nil))
		f.db.refusal = errors.New("down")
		_, err := finish(f, "user-1", started(t, f))
		assert.Equal(t, codes.Internal, status.Code(err))
	})
	t.Run("malformed, or nowhere to keep a token", func(t *testing.T) {
		f := newSlackFixture(t)
		for name, req := range map[string]*foundryv1.FinishFoundrySlackInstallRequest{
			"no code":    {State: "s"},
			"no state":   {Code: "c"},
			"long state": {Code: "c", State: strings.Repeat("s", maxSlackArg+1)},
		} {
			_, err := f.s.FinishFoundrySlackInstall(as("user-1"), req)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), name)
		}
		state := started(t, f)
		f.s.deps.ConnectorKeys = nil
		_, err := finish(f, "user-1", state)
		assert.Equal(t, codes.Unavailable, status.Code(err))
		assert.False(t, f.db.states[hashToken(state)].used, "the state isn't spent")
		f.cfg.Foundry.Slack.Enabled = false
		_, err = finish(f, "user-1", state)
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_DISABLED", reason)
	})
}

func TestDisconnectFoundrySlack(t *testing.T) {
	disconnect := func(f slackFixture, user string) (*foundryv1.FoundrySlack, error) {
		resp, err := f.s.DisconnectFoundrySlack(as(user), &foundryv1.DisconnectFoundrySlackRequest{WorkspaceId: slackWS})
		return resp.GetSlack(), err
	}
	t.Run("tells Slack, then ends the install", func(t *testing.T) {
		f := newSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		f.db.links = []*model.SlackLink{{InstallID: "inst-1", SlackUserID: slackAda, UserID: "user-1"}}
		got, err := disconnect(f, "user-3")
		require.NoError(t, err)
		assert.Nil(t, got.GetInstall())
		assert.Equal(t, map[string]bool{"inst-1": true}, f.db.revoked)
		call := f.slack.call("apps.uninstall")
		assert.Equal(t, "Bearer xoxb-live", call.auth)
		form, _ := url.ParseQuery(string(call.body))
		assert.Equal(t, "1.2", form.Get("client_id"))
		assert.Equal(t, "client secret", form.Get("client_secret"))
		assert.Empty(t, f.db.links)
	})
	t.Run("a token Slack no longer takes is gone there", func(t *testing.T) {
		f := newSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		f.slack.answer("apps.uninstall", http.StatusOK, `{"ok":false,"error":"invalid_auth"}`)
		_, err := disconnect(f, "user-1")
		require.NoError(t, err)
		assert.Equal(t, map[string]bool{"inst-1": true}, f.db.revoked)
	})
	t.Run("leaves the sweep to tell Slack when it can't", func(t *testing.T) {
		for name, answer := range map[string][2]any{
			"Slack down":        {http.StatusBadGateway, `{}`},
			"rate limited":      {http.StatusTooManyRequests, `{"ok":false,"error":"ratelimited"}`},
			"another of errors": {http.StatusOK, `{"ok":false,"error":"internal_error"}`},
		} {
			f := newSlackFixture(t)
			f.db.install(t, f.keys, model.SlackPerPerson{})
			f.slack.answer("apps.uninstall", answer[0].(int), answer[1].(string))
			got, err := disconnect(f, "user-1")
			require.NoError(t, err, name)
			assert.Nil(t, got.GetInstall(), name)
			assert.Equal(t, map[string]bool{"inst-1": false}, f.db.revoked, name)
		}
		f := newSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		f.s.deps.ConnectorKeys = nil
		_, err := disconnect(f, "user-1")
		require.NoError(t, err)
		assert.Equal(t, map[string]bool{"inst-1": false}, f.db.revoked, "a token that can't be opened")
		assert.Empty(t, f.slack.called())
	})
	t.Run("refused", func(t *testing.T) {
		f := newSlackFixture(t)
		_, err := disconnect(f, "user-1")
		assert.Equal(t, codes.NotFound, status.Code(err), "nothing to disconnect")
		f.db.install(t, f.keys, model.SlackPerPerson{})
		_, err = disconnect(f, "user-2")
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		_, err = f.s.DisconnectFoundrySlack(as("user-1"), &foundryv1.DisconnectFoundrySlackRequest{})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		f.db.fail = errors.New("down")
		_, err = disconnect(f, "user-1")
		assert.Equal(t, codes.Internal, status.Code(err))
		assert.Empty(t, f.slack.called())
		f.cfg.Foundry.Slack.Enabled = false
		_, err = disconnect(f, "user-1")
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_DISABLED", reason)
	})
}

// revokeRaces is a store whose install is revoked by Slack's own uninstall
// between the API's read and its revoke.
type revokeRaces struct{ *slackStore }

func (r revokeRaces) RevokeFoundrySlackInstall(context.Context, string, string, bool) error {
	return foundrystore.ErrNotFound
}

func TestDisconnectFoundrySlackRaced(t *testing.T) {
	f := newSlackFixture(t)
	f.db.install(t, f.keys, model.SlackPerPerson{})
	f.s.deps.DB = revokeRaces{f.db}
	_, err := f.s.DisconnectFoundrySlack(as("user-1"), &foundryv1.DisconnectFoundrySlackRequest{WorkspaceId: slackWS})
	require.NoError(t, err, "Slack's uninstall came first")
}

func TestListFoundrySlackLinks(t *testing.T) {
	f := newSlackFixture(t)
	list := func(user string) ([]*foundryv1.FoundrySlackLink, error) {
		resp, err := f.s.ListFoundrySlackLinks(as(user), &foundryv1.ListFoundrySlackLinksRequest{WorkspaceId: slackWS})
		return resp.GetLinks(), err
	}
	got, err := list("user-1")
	require.NoError(t, err)
	assert.Empty(t, got, "no install")
	f.db.install(t, f.keys, model.SlackPerPerson{})
	f.db.links = []*model.SlackLink{{InstallID: "inst-1", SlackUserID: slackAda, UserID: "user-1"}, {InstallID: "inst-1", SlackUserID: "U0GONE001", UserID: "gone"}}
	got, err = list("user-3")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "Name of user-1", got[0].GetUserName())
	assert.Empty(t, got[1].GetUserName(), "a user the host no longer names")
	_, err = list("user-2")
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = f.s.ListFoundrySlackLinks(as("user-1"), &foundryv1.ListFoundrySlackLinksRequest{})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	f.db.fail = errors.New("down")
	_, err = list("user-1")
	assert.Equal(t, codes.Internal, status.Code(err))
}

// failingLinks is a store that reads an install but not its links.
type failingLinks struct{ *slackStore }

func (failingLinks) ListFoundrySlackLinks(context.Context, string) ([]*model.SlackLink, error) {
	return nil, errors.New("down")
}

func TestSlackLinksUnreadable(t *testing.T) {
	f := newSlackFixture(t)
	f.db.install(t, f.keys, model.SlackPerPerson{})
	f.s.deps.DB = failingLinks{f.db}
	_, err := f.s.ListFoundrySlackLinks(as("user-1"), &foundryv1.ListFoundrySlackLinksRequest{WorkspaceId: slackWS})
	assert.Equal(t, codes.Internal, status.Code(err))
	_, err = f.s.GetFoundrySlack(as("user-1"), &foundryv1.GetFoundrySlackRequest{WorkspaceId: slackWS})
	assert.Equal(t, codes.Internal, status.Code(err))
}

func TestPreviewFoundrySlackLink(t *testing.T) {
	preview := func(f slackFixture, user, token string) (*foundryv1.PreviewFoundrySlackLinkResponse, error) {
		return f.s.PreviewFoundrySlackLink(as(user), &foundryv1.PreviewFoundrySlackLinkRequest{WorkspaceId: slackWS, Token: token})
	}
	setup := func(t *testing.T, userJSON string) (slackFixture, string) {
		f := newSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		f.slack.answer("users.info", http.StatusOK, userJSON)
		return f, f.db.linkToken("inst-1", slackAda)
	}
	t.Run("the caller's own Slack account", func(t *testing.T) {
		f, token := setup(t, slackUserJSON(slackAda, "ada@acme.com", nil))
		got, err := preview(f, "user-1", token)
		require.NoError(t, err)
		assert.True(t, got.GetMatchesYou(), "the same email, case aside")
		assert.Equal(t, foundryv1.FoundrySlackLinkRefusal_FOUNDRY_SLACK_LINK_REFUSAL_UNSPECIFIED, got.GetRefusal())
		assert.Equal(t, "Acme", got.GetTeamName())
		assert.Equal(t, slackAda, got.GetSlackUserId())
		assert.Equal(t, "Ada", got.GetSlackName())
		assert.Equal(t, "ada", got.GetSlackHandle())
		assert.Equal(t, "a•••@acme.com", got.GetSlackEmailMasked())
		assert.Equal(t, "Bearer xoxb-live", f.slack.call("users.info").auth)
		assert.False(t, f.db.linkTokens[hashToken(token)].used, "a preview spends nothing")
	})
	t.Run("someone else's", func(t *testing.T) {
		for name, c := range map[string]struct {
			user string
			json string
			want foundryv1.FoundrySlackLinkRefusal
		}{
			"another email": {"user-2", slackUserJSON(slackAda, "ada@acme.com", nil),
				foundryv1.FoundrySlackLinkRefusal_FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_MISMATCH},
			"a hidden email": {"user-1", slackUserJSON(slackAda, "", nil), foundryv1.FoundrySlackLinkRefusal_FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_MISMATCH},
			"a caller with no email": {"user-3", slackUserJSON(slackAda, "ada@acme.com", nil),
				foundryv1.FoundrySlackLinkRefusal_FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_MISMATCH},
			"an unconfirmed email": {"user-1", slackUserJSON(slackAda, "ada@acme.com", func(u map[string]any) { u["is_email_confirmed"] = false }),
				foundryv1.FoundrySlackLinkRefusal_FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_UNCONFIRMED},
			"a guest": {"user-1", slackUserJSON(slackAda, "ada@acme.com", func(u map[string]any) { u["is_restricted"] = true }),
				foundryv1.FoundrySlackLinkRefusal_FOUNDRY_SLACK_LINK_REFUSAL_NOT_A_MEMBER},
			"another workspace's": {"user-1", slackUserJSON(slackAda, "ada@acme.com", func(u map[string]any) { u["team_id"] = "T0OTHER01" }),
				foundryv1.FoundrySlackLinkRefusal_FOUNDRY_SLACK_LINK_REFUSAL_NOT_A_MEMBER},
			"no longer in Slack": {"user-1", `{"ok":false,"error":"user_not_found"}`,
				foundryv1.FoundrySlackLinkRefusal_FOUNDRY_SLACK_LINK_REFUSAL_NOT_A_MEMBER},
		} {
			f, token := setup(t, c.json)
			got, err := preview(f, c.user, token)
			require.NoError(t, err, name)
			assert.False(t, got.GetMatchesYou(), name)
			assert.Equal(t, c.want, got.GetRefusal(), name)
		}
	})
	t.Run("a Grid member of the installed workspace", func(t *testing.T) {
		f := newSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		f.db.installs[0].EnterpriseID = "E0ORG0001"
		f.slack.answer("users.info", http.StatusOK, slackUserJSON(slackAda, "ada@acme.com", func(u map[string]any) {
			u["team_id"] = "T0ELSE001" // the session's
			u["enterprise_user"] = map[string]any{"enterprise_id": "E0ORG0001", "teams": []string{"T0ELSE001", slackTeam}}
		}))
		got, err := preview(f, "user-1", f.db.linkToken("inst-1", slackAda))
		require.NoError(t, err)
		assert.True(t, got.GetMatchesYou())
	})
	t.Run("a token not live in the workspace's install", func(t *testing.T) {
		f, token := setup(t, slackUserJSON(slackAda, "ada@acme.com", nil))
		other := f.db.linkToken("inst-other", slackAda)
		used := f.db.linkToken("inst-1", "U0USED001")
		f.db.linkTokens[hashToken(used)].used = true
		for name, tok := range map[string]string{"unknown": slackLinkTokenPrefix + "nope", "another install's": other, "used": used} {
			_, err := preview(f, "user-1", tok)
			reason, _ := slackReason(t, err)
			assert.Equal(t, "FOUNDRY_SLACK_LINK_EXPIRED", reason, name)
		}
		for name, tok := range map[string]string{"not a link's": "xoxb-1", "long": slackLinkTokenPrefix + strings.Repeat("a", maxSlackArg)} {
			_, err := preview(f, "user-1", tok)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), name)
		}
		_, err := f.s.PreviewFoundrySlackLink(as("user-1"), &foundryv1.PreviewFoundrySlackLinkRequest{Token: token})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		f.db.installs = nil
		_, err = preview(f, "user-1", token)
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_LINK_EXPIRED", reason, "no install")
		assert.Empty(t, f.slack.called())
	})
	t.Run("what can't be asked", func(t *testing.T) {
		f, token := setup(t, `{"ok":false,"error":"internal_error"}`)
		_, err := preview(f, "user-1", token)
		assert.Equal(t, codes.Unavailable, status.Code(err), "Slack fails")
		f.slack.answer("users.info", http.StatusTooManyRequests, `{}`)
		_, err = preview(f, "user-1", token)
		assert.Equal(t, codes.Unavailable, status.Code(err), "Slack is busy")
		f.s.deps.ConnectorKeys = nil
		_, err = preview(f, "user-1", token)
		assert.Equal(t, codes.Unavailable, status.Code(err), "no token to ask with")
		f.s.deps.Emails = nil
		_, err = preview(f, "user-1", token)
		assert.Equal(t, codes.Unavailable, status.Code(err), "no email to compare")
		f.s.deps.Emails = slackPeople{err: errors.New("down")}
		_, err = preview(f, "user-1", token)
		assert.Equal(t, codes.Unavailable, status.Code(err), "the email can't be read")
		f.db.fail = errors.New("down")
		_, err = preview(f, "user-1", token)
		assert.Equal(t, codes.Internal, status.Code(err))
	})
}

// failingLinkTokens is a store whose link tokens can't be read.
type failingLinkTokens struct{ *slackStore }

func (failingLinkTokens) GetFoundrySlackLinkToken(context.Context, string) (*model.SlackLinkToken, error) {
	return nil, errors.New("down")
}

func TestSlackLinkTokensUnreadable(t *testing.T) {
	f := newSlackFixture(t)
	f.db.install(t, f.keys, model.SlackPerPerson{})
	f.s.deps.DB = failingLinkTokens{f.db}
	_, err := f.s.PreviewFoundrySlackLink(as("user-1"), &foundryv1.PreviewFoundrySlackLinkRequest{WorkspaceId: slackWS, Token: slackLinkTokenPrefix + "x"})
	assert.Equal(t, codes.Internal, status.Code(err))
}

func TestCreateFoundrySlackLink(t *testing.T) {
	create := func(f slackFixture, user, token string) (*foundryv1.FoundrySlackLink, error) {
		resp, err := f.s.CreateFoundrySlackLink(as(user), &foundryv1.CreateFoundrySlackLinkRequest{WorkspaceId: slackWS, Token: token})
		return resp.GetLink(), err
	}
	setup := func(t *testing.T, userJSON string) (slackFixture, string) {
		f := newSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		f.slack.answer("users.info", http.StatusOK, userJSON)
		f.slack.answer("conversations.open", http.StatusOK, `{"ok":true,"channel":{"id":"D0DM00001"}}`)
		f.slack.answer("chat.postMessage", http.StatusOK, `{"ok":true,"ts":"1795000099.000100"}`)
		return f, f.db.linkToken("inst-1", slackAda)
	}
	t.Run("links the caller, once, and tells them in Slack", func(t *testing.T) {
		f, token := setup(t, slackUserJSON(slackAda, "ada@acme.com", nil))
		f.db.links = []*model.SlackLink{{InstallID: "inst-1", SlackUserID: "U0OLDADA1", UserID: "user-1"}}
		got, err := create(f, "user-1", token)
		require.NoError(t, err)
		assert.Equal(t, slackAda, got.GetSlackUserId())
		assert.Equal(t, "user-1", got.GetUserId())
		assert.Equal(t, "Name of user-1", got.GetUserName())
		require.Len(t, f.db.links, 1, "the caller's earlier Slack account is unlinked")
		assert.Equal(t, slackAda, f.db.links[0].SlackUserID)
		assert.Equal(t, []string{"users.info", "conversations.open", "chat.postMessage"}, f.slack.called())
		var post map[string]any
		require.NoError(t, json.Unmarshal(f.slack.call("chat.postMessage").body, &post))
		assert.Equal(t, "D0DM00001", post["channel"])
		assert.Equal(t, "none", post["parse"])
		assert.Contains(t, post["text"], "`ada@acme.com`", "an address is never a link")
		blocks, _ := json.Marshal(post["blocks"])
		assert.Contains(t, string(blocks), `"action_id":"foundry.unlink"`)
		_, err = create(f, "user-1", token)
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_LINK_EXPIRED", reason, "spent")
	})
	t.Run("the link stands when Slack can't be told", func(t *testing.T) {
		f, token := setup(t, slackUserJSON(slackAda, "ada@acme.com", nil))
		f.slack.answer("conversations.open", http.StatusOK, `{"ok":false,"error":"internal_error"}`)
		_, err := create(f, "user-1", token)
		require.NoError(t, err)
		assert.Len(t, f.db.links, 1)
		assert.NotContains(t, f.slack.called(), "chat.postMessage")
		f2, token2 := setup(t, slackUserJSON(slackAda, "ada@acme.com", nil))
		f2.cfg.Foundry.Slack.SiteURL = "not an origin"
		_, err = create(f2, "user-1", token2)
		require.NoError(t, err)
		assert.NotContains(t, f2.slack.called(), "conversations.open")
	})
	t.Run("refuses someone else's Slack account", func(t *testing.T) {
		for name, c := range map[string]struct {
			user, json, refusal string
		}{
			"another email": {"user-2", slackUserJSON(slackAda, "ada@acme.com", nil), "FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_MISMATCH"},
			"unconfirmed": {"user-1", slackUserJSON(slackAda, "ada@acme.com", func(u map[string]any) { u["is_email_confirmed"] = false }),
				"FOUNDRY_SLACK_LINK_REFUSAL_EMAIL_UNCONFIRMED"},
			"a Slack Connect user": {"user-1", slackUserJSON(slackAda, "ada@acme.com", func(u map[string]any) { u["is_stranger"] = true }),
				"FOUNDRY_SLACK_LINK_REFUSAL_NOT_A_MEMBER"},
		} {
			f, token := setup(t, c.json)
			_, err := create(f, c.user, token)
			reason, meta := slackReason(t, err)
			assert.Equal(t, "FOUNDRY_SLACK_LINK_REFUSED", reason, name)
			assert.Equal(t, c.refusal, meta["refusal"], name)
			assert.Empty(t, f.db.links, name)
			assert.False(t, f.db.linkTokens[hashToken(token)].used, name)
		}
	})
	t.Run("refuses who isn't a member", func(t *testing.T) {
		f, token := setup(t, slackUserJSON(slackAda, "ada@acme.com", nil))
		_, err := create(f, "stranger", token)
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		f.s.deps.People = slackPeople{err: errors.New("down")}
		_, err = create(f, "user-1", token)
		assert.Equal(t, codes.Unavailable, status.Code(err))
		_, err = f.s.CreateFoundrySlackLink(as("user-1"), &foundryv1.CreateFoundrySlackLinkRequest{Token: token})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
		assert.Empty(t, f.slack.called())
	})
	t.Run("a token spent meanwhile", func(t *testing.T) {
		f, token := setup(t, slackUserJSON(slackAda, "ada@acme.com", nil))
		f.s.deps.DB = spentMeanwhile{f.db}
		_, err := create(f, "user-1", token)
		reason, _ := slackReason(t, err)
		assert.Equal(t, "FOUNDRY_SLACK_LINK_EXPIRED", reason)
		f.s.deps.DB = linkFails{f.db}
		_, err = create(f, "user-1", token)
		assert.Equal(t, codes.Internal, status.Code(err))
	})
	t.Run("a member links their own account", func(t *testing.T) {
		f, token := setup(t, slackUserJSON(slackAda, "grace@acme.com", nil))
		_, err := create(f, "user-2", token)
		require.NoError(t, err)
	})
}

type spentMeanwhile struct{ *slackStore }

func (spentMeanwhile) LinkFoundrySlackUser(context.Context, string, string, string) (*model.SlackLink, error) {
	return nil, foundrystore.ErrNotFound
}

type linkFails struct{ *slackStore }

func (linkFails) LinkFoundrySlackUser(context.Context, string, string, string) (*model.SlackLink, error) {
	return nil, errors.New("down")
}

func TestDeleteFoundrySlackLink(t *testing.T) {
	setup := func(t *testing.T) slackFixture {
		f := newSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		f.db.links = []*model.SlackLink{{InstallID: "inst-1", SlackUserID: slackAda, UserID: "user-1"}, {InstallID: "inst-1", SlackUserID: "U0GRACE01", UserID: "user-2"}}
		return f
	}
	unlink := func(f slackFixture, user, slackUser string) error {
		_, err := f.s.DeleteFoundrySlackLink(as(user), &foundryv1.DeleteFoundrySlackLinkRequest{WorkspaceId: slackWS, SlackUserId: slackUser})
		return err
	}
	t.Run("a member unlinks their own", func(t *testing.T) {
		f := setup(t)
		require.NoError(t, unlink(f, "user-2", "U0GRACE01"))
		assert.Len(t, f.db.links, 1)
		assert.Equal(t, codes.PermissionDenied, status.Code(unlink(f, "user-2", slackAda)), "not someone else's")
		assert.Equal(t, codes.NotFound, status.Code(unlink(f, "user-2", "U0GRACE01")), "gone")
	})
	t.Run("an owner or admin unlinks anyone", func(t *testing.T) {
		f := setup(t)
		require.NoError(t, unlink(f, "user-3", "U0GRACE01"))
		require.NoError(t, unlink(f, "user-3", slackAda))
		assert.Empty(t, f.db.links)
	})
	t.Run("refused", func(t *testing.T) {
		f := setup(t)
		assert.Equal(t, codes.InvalidArgument, status.Code(unlink(f, "user-1", "not an id!")))
		assert.Equal(t, codes.NotFound, status.Code(unlink(f, "user-1", "U0NOONE01")))
		f.s.deps.DB = deleteRaces{f.db}
		assert.Equal(t, codes.NotFound, status.Code(unlink(f, "user-1", slackAda)), "unlinked meanwhile")
		f.s.deps.DB = deleteFails{f.db}
		assert.Equal(t, codes.Internal, status.Code(unlink(f, "user-1", slackAda)))
		f.s.deps.DB = f.db
		f.db.fail = errors.New("down")
		assert.Equal(t, codes.Internal, status.Code(unlink(f, "user-1", slackAda)))
		f.db.fail = nil
		f.db.installs = nil
		assert.Equal(t, codes.NotFound, status.Code(unlink(f, "user-1", slackAda)), "no install")
	})
}

type deleteRaces struct{ *slackStore }

func (deleteRaces) DeleteFoundrySlackLink(context.Context, string, string) error {
	return foundrystore.ErrNotFound
}

type deleteFails struct{ *slackStore }

func (deleteFails) DeleteFoundrySlackLink(context.Context, string, string) error {
	return errors.New("down")
}

func TestMaskEmail(t *testing.T) {
	for in, want := range map[string]string{
		"ada@acme.com":     "a•••@acme.com",
		"Émile@acme.fr":    "É•••@acme.fr",
		"a@b@acme.com":     "a•••@acme.com",
		"":                 "",
		"@acme.com":        "",
		"no address here.": "",
	} {
		assert.Equal(t, want, maskEmail(in), in)
	}
}

// queueFails is a store that resolves installs but can't queue.
type queueFails struct{ *slackStore }

func (queueFails) QueueFoundrySlackIntake(context.Context, *model.SlackIntake) error {
	return errors.New("down")
}

func TestReceiveSlackQueueFails(t *testing.T) {
	f := newSlackFixture(t)
	f.db.install(t, f.keys, model.SlackPerPerson{})
	f.s.deps.DB = queueFails{f.db}
	for route, body := range map[foundryv1.SlackRoute][]byte{
		foundryv1.SlackRoute_SLACK_ROUTE_EVENTS:        fixture(t, "events/dm.json"),
		foundryv1.SlackRoute_SLACK_ROUTE_INTERACTIVITY: clickBody(t, "dismiss.json"),
	} {
		_, err := f.s.ReceiveSlackRequest(door(), signed(route, body))
		assert.Equal(t, codes.Unavailable, status.Code(err), route)
	}
}

func TestSlackSettingsEdges(t *testing.T) {
	t.Run("Slack's own origin by default", func(t *testing.T) {
		f := newSlackFixture(t)
		f.s.deps.SlackURL = ""
		resp, err := f.s.StartFoundrySlackInstall(as("user-1"), &foundryv1.StartFoundrySlackInstallRequest{WorkspaceId: slackWS})
		require.NoError(t, err)
		assert.True(t, strings.HasPrefix(resp.GetAuthorizeUrl(), "https://slack.com/oauth/v2/authorize?"))
	})
	t.Run("members that can't be read", func(t *testing.T) {
		f := newSlackFixture(t)
		f.s.deps.People = slackPeople{err: errors.New("down")}
		_, err := f.s.StartFoundrySlackInstall(as("user-1"), &foundryv1.StartFoundrySlackInstallRequest{WorkspaceId: slackWS})
		assert.Equal(t, codes.Unavailable, status.Code(err))
	})
	t.Run("a token that can't be opened", func(t *testing.T) {
		f := newSlackFixture(t)
		f.db.install(t, f.keys, model.SlackPerPerson{})
		f.slack.answer("users.info", http.StatusOK, slackUserJSON(slackAda, "ada@acme.com", nil))
		token := f.db.linkToken("inst-1", slackAda)
		other, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{8}, 32)})
		require.NoError(t, err)
		f.s.deps.ConnectorKeys = other
		_, err = f.s.PreviewFoundrySlackLink(as("user-1"), &foundryv1.PreviewFoundrySlackLinkRequest{WorkspaceId: slackWS, Token: token})
		assert.Equal(t, codes.Unavailable, status.Code(err), "sealed under another key")
		delete(f.db.tokens, "inst-1")
		_, err = f.s.PreviewFoundrySlackLink(as("user-1"), &foundryv1.PreviewFoundrySlackLinkRequest{WorkspaceId: slackWS, Token: token})
		assert.Equal(t, codes.Unavailable, status.Code(err), "no token kept")
		assert.Empty(t, f.slack.called())
	})
	t.Run("off, or not a person", func(t *testing.T) {
		f := newSlackFixture(t)
		agent := ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceMCP, UserID: "user-1"})
		_, err := f.s.PreviewFoundrySlackLink(agent, &foundryv1.PreviewFoundrySlackLinkRequest{WorkspaceId: slackWS, Token: "hx_sl_x"})
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		_, err = f.s.CreateFoundrySlackLink(agent, &foundryv1.CreateFoundrySlackLinkRequest{WorkspaceId: slackWS, Token: "hx_sl_x"})
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		_, err = f.s.DeleteFoundrySlackLink(agent, &foundryv1.DeleteFoundrySlackLinkRequest{WorkspaceId: slackWS, SlackUserId: slackAda})
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		_, err = f.s.ListFoundrySlackLinks(agent, &foundryv1.ListFoundrySlackLinksRequest{WorkspaceId: slackWS})
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		_, err = f.s.StartFoundrySlackInstall(agent, &foundryv1.StartFoundrySlackInstallRequest{WorkspaceId: slackWS})
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		_, err = f.s.FinishFoundrySlackInstall(agent, &foundryv1.FinishFoundrySlackInstallRequest{Code: "c", State: "s"})
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		_, err = f.s.DisconnectFoundrySlack(agent, &foundryv1.DisconnectFoundrySlackRequest{WorkspaceId: slackWS})
		assert.Equal(t, codes.PermissionDenied, status.Code(err))
		_, err = f.s.PreviewFoundrySlackLink(as("user-1"), &foundryv1.PreviewFoundrySlackLinkRequest{Token: "hx_sl_x"})
		assert.Equal(t, codes.InvalidArgument, status.Code(err))
	})
}
