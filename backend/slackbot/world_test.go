package slackbot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/api"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/integration"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/migrations"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The bot's tests run on the test MySQL server (HELIX_TEST_ROOT_DSN), in a
// schema of their own, against a fake Slack (an httptest server), Foundry's
// own API for Founder's messages, and a stub model: the tests run each turn
// themselves, as the agent worker would, writing its events to the store.

const testSchema = "foundry_slackbot_test"

var testDB *sql.DB

func TestMain(m *testing.M) {
	rootDSN := os.Getenv("HELIX_TEST_ROOT_DSN")
	if rootDSN == "" {
		rootDSN = "root:helix@tcp(127.0.0.1:3307)/"
	}
	if err := openTestSchema(rootDSN); err != nil {
		fmt.Fprintf(os.Stderr, "slackbot tests: %v. Run `docker compose -f compose.test.yaml up -d --wait` first.\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = testDB.Close()
	os.Exit(code)
}

func openTestSchema(rootDSN string) error {
	root, err := sql.Open("mysql", rootDSN)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, stmt := range []string{"DROP DATABASE IF EXISTS " + testSchema, "CREATE DATABASE " + testSchema} {
		if _, err := root.Exec(stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	mc, err := gomysql.ParseDSN(rootDSN + testSchema)
	if err != nil {
		return err
	}
	mc.ParseTime, mc.InterpolateParams, mc.ClientFoundRows = true, true, true
	if testDB, err = sql.Open("mysql", mc.FormatDSN()); err != nil {
		return err
	}
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("mysql"); err != nil {
		return err
	}
	goose.SetLogger(goose.NopLogger())
	if err := goose.Up(testDB, "."); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	_, err = testDB.Exec(`CREATE TABLE workspaces (id CHAR(26) NOT NULL PRIMARY KEY, status VARCHAR(16) NOT NULL DEFAULT 'active', deleted_at DATETIME(6) NULL)`)
	return err
}

// workspaces is the tests' host table of workspaces.
type workspaces struct{}

func (workspaces) Table() string { return "workspaces" }

func (workspaces) Lock(ctx context.Context, q store.Executor, id string) error {
	var locked string
	if err := q.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE id=? AND deleted_at IS NULL FOR UPDATE`, id).Scan(&locked); errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	} else if err != nil {
		return err
	}
	return nil
}

func (workspaces) Deleting(ctx context.Context, q store.Executor, id string) (bool, error) {
	var deleting bool
	err := q.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL OR status IN ('deleting', 'deleted') FROM workspaces WHERE id=?`, id).Scan(&deleting)
	if errors.Is(err, sql.ErrNoRows) {
		return false, store.ErrNotFound
	}
	return deleting, err
}

// The fixtures' Slack team, bot, people and channels.
const (
	team    = "T0TEAM"
	botUser = "U0BOT"
	ada     = "U0ADA"   // linked to adaID
	grace   = "U0GRACE" // linked to graceID
	alan    = "U0ALAN"  // not linked
	guest   = "U0GUEST" // a guest
	adaID   = "user_ada"
	graceID = "user_grace"
	dmAda   = "D0ADA"
	general = "C0GENERAL"
	siteURL = "https://foundry.example.com"
)

// call is one Web API call the fake Slack took.
type call struct {
	method string
	args   map[string]any
}

// fakeSlack is Slack's Web API: it records each call and answers it, with
// an answer queued for its method if any, else as Slack would.
type fakeSlack struct {
	*httptest.Server
	mu      sync.Mutex
	calls   []call
	queued  map[string][]func(call) (int, http.Header, string)
	users   map[string]map[string]any
	lastTS  atomic.Int64
	onCall  func(call)
	deleted map[string]bool
}

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{queued: map[string][]func(call) (int, http.Header, string){}, users: map[string]map[string]any{}, deleted: map[string]bool{}}
	f.lastTS.Store(1_700_000_000_000_000)
	for _, u := range []string{ada, grace, alan} {
		f.users[u] = map[string]any{"id": u, "team_id": team, "name": strings.ToLower(u[2:]), "real_name": "Person " + u[2:], "tz": "Europe/London",
			"profile": map[string]any{"display_name": strings.ToLower(u[2:])}}
	}
	f.users[guest] = map[string]any{"id": guest, "team_id": team, "name": "guest", "is_restricted": true}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c := call{method: strings.TrimPrefix(r.URL.Path, "/api/"), args: map[string]any{}}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			_ = json.Unmarshal(body, &c.args)
		} else if form, err := url.ParseQuery(string(body)); err == nil {
			for k := range form {
				c.args[k] = form.Get(k)
			}
		}
		f.mu.Lock()
		f.calls = append(f.calls, c)
		var answer func(call) (int, http.Header, string)
		if q := f.queued[c.method]; len(q) > 0 {
			answer, f.queued[c.method] = q[0], q[1:]
		}
		onCall := f.onCall
		f.mu.Unlock()
		if onCall != nil {
			onCall(c)
		}
		code, header, out := http.StatusOK, http.Header{}, ""
		if answer != nil {
			code, header, out = answer(c)
		} else {
			out = f.answer(c)
		}
		for k, v := range header {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(out))
	}))
	t.Cleanup(f.Close)
	return f
}

// answer is Slack's answer to c when none is queued.
func (f *fakeSlack) answer(c call) string {
	switch c.method {
	case "chat.postMessage":
		n := f.lastTS.Add(1)
		return fmt.Sprintf(`{"ok":true,"ts":"%d.%06d"}`, n/1_000_000, n%1_000_000)
	case "users.info":
		f.mu.Lock()
		u, ok := f.users[fmt.Sprint(c.args["user"])]
		f.mu.Unlock()
		if !ok {
			return `{"ok":false,"error":"user_not_found"}`
		}
		b, _ := json.Marshal(map[string]any{"ok": true, "user": u})
		return string(b)
	case "conversations.open":
		return `{"ok":true,"channel":{"id":"D0OPENED"}}`
	}
	return `{"ok":true}`
}

// queue answers method's next call with code and body (and Retry-After,
// when set).
func (f *fakeSlack) queue(method string, code int, retryAfter string, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued[method] = append(f.queued[method], func(call) (int, http.Header, string) {
		h := http.Header{}
		if retryAfter != "" {
			h.Set("Retry-After", retryAfter)
		}
		return code, h, body
	})
}

// of is the calls of method, in order.
func (f *fakeSlack) of(method string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

// posts is the messages posted (chat.postMessage) whose text holds text.
func (f *fakeSlack) posts(text string) []call {
	var out []call
	for _, c := range f.of("chat.postMessage") {
		if strings.Contains(c.text(), text) {
			out = append(out, c)
		}
	}
	return out
}

// text is a message call's notification text and blocks, as JSON.
func (c call) text() string {
	b, _ := json.Marshal(c.args["blocks"])
	return fmt.Sprint(c.args["text"]) + " " + string(b)
}

// founder is Founder's API as the bot calls it: Foundry's own API for
// messages, events, dismissals and stops; for a card's Run and Prepare
// again, the store's rules as the API keeps them (the API itself would run
// the operation), ending a run succeeded with its continuation turn.
type founder struct {
	api     *api.Service
	db      store.Store
	keys    *secretbox.Keyring
	mu      sync.Mutex
	sendErr error
	sends   int
}

func actorOf(ctx context.Context) ports.Actor {
	a, _ := ports.ActorFrom(ctx)
	return a
}

func (f *founder) SendFoundryAgentMessage(ctx context.Context, req *foundryv1.SendFoundryAgentMessageRequest, _ ...grpc.CallOption) (
	*foundryv1.FoundryAgentTurnResponse, error) {
	f.mu.Lock()
	f.sends++
	err := f.sendErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.api.SendFoundryAgentMessage(ctx, req)
}

func (f *founder) ListFoundryAgentTurnEvents(ctx context.Context, req *foundryv1.ListFoundryAgentTurnEventsRequest, _ ...grpc.CallOption) (
	*foundryv1.ListFoundryAgentTurnEventsResponse, error) {
	return f.api.ListFoundryAgentTurnEvents(ctx, req)
}

func (f *founder) DismissFoundryAgentAction(ctx context.Context, req *foundryv1.DismissFoundryAgentActionRequest, _ ...grpc.CallOption) (
	*foundryv1.FoundryAgentActionResponse, error) {
	return f.api.DismissFoundryAgentAction(ctx, req)
}

func (f *founder) CancelFoundryAgentTurn(ctx context.Context, req *foundryv1.CancelFoundryAgentTurnRequest, _ ...grpc.CallOption) (
	*foundryv1.FoundryAgentTurnResponse, error) {
	return f.api.CancelFoundryAgentTurn(ctx, req)
}

// refusal is the API's answer to a refusal of the store's.
func refusal(err error) error {
	var r model.FoundryAgentRefusal
	if !errors.As(err, &r) {
		return status.Error(codes.Internal, err.Error())
	}
	reason := map[model.FoundryAgentRefusalReason]string{
		model.FoundryAgentThreadBusy: "FOUNDRY_AGENT_THREAD_BUSY", model.FoundryAgentNotPrepared: "FOUNDRY_AGENT_ACTION_NOT_PREPARED",
		model.FoundryAgentChanged: "FOUNDRY_AGENT_ACTION_CHANGED", model.FoundryAgentExpired: "FOUNDRY_AGENT_ACTION_EXPIRED",
	}[r.Reason]
	st, _ := status.New(codes.FailedPrecondition, r.Detail).WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: "foundry.helix-db.com"})
	return st.Err()
}

func (f *founder) ExecuteFoundryAgentAction(ctx context.Context, req *foundryv1.ExecuteFoundryAgentActionRequest, _ ...grpc.CallOption) (
	*foundryv1.FoundryAgentActionResponse, error) {
	user := actorOf(ctx).UserID
	a, err := f.db.ClaimFoundryAgentAction(ctx, req.GetWorkspaceId(), user, req.GetId(), req.GetPayloadHash())
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, status.Error(codes.NotFound, "action not found")
	case err != nil:
		return nil, refusal(err)
	}
	next := &model.FoundryAgentTurn{ID: id.New(), WorkspaceID: a.WorkspaceID, ThreadID: a.ThreadID, UserID: user, Kind: model.FoundryAgentTurnActionResult,
		Input: a.ID, MaxUSDMicros: 1_000_000}
	first, err := agent.MarshalEvent(agent.ActionResult{ActionID: a.ID, Operation: a.Operation, Status: "succeeded", Result: json.RawMessage(`{}`)})
	if err != nil {
		return nil, err
	}
	if err := f.db.FinishFoundryAgentAction(ctx, a.ID, model.FoundryAgentActionEnd{Status: model.FoundryAgentActionSucceeded,
		Result: json.RawMessage(`{}`)}, next, first); err != nil {
		return nil, err
	}
	return &foundryv1.FoundryAgentActionResponse{Action: &foundryv1.FoundryAgentAction{Id: a.ID, Status: "succeeded"},
		Continuation: &foundryv1.FoundryAgentTurn{Id: next.ID}}, nil
}

func (f *founder) RefreshFoundryAgentAction(ctx context.Context, req *foundryv1.RefreshFoundryAgentActionRequest, _ ...grpc.CallOption) (
	*foundryv1.FoundryAgentActionResponse, error) {
	user := actorOf(ctx).UserID
	a, err := f.db.GetFoundryAgentAction(ctx, req.GetWorkspaceId(), req.GetId())
	if err != nil {
		return nil, status.Error(codes.NotFound, "action not found")
	}
	sealed, keyID, err := f.keys.Seal([]byte("token-2"), a.ID)
	if err != nil {
		return nil, err
	}
	a, err = f.db.RefreshFoundryAgentAction(ctx, req.GetWorkspaceId(), user, a.ID, a.PayloadHash, a.Display, "conf-"+id.New(), sealed, keyID,
		time.Now().Add(time.Hour))
	if err != nil {
		return nil, refusal(err)
	}
	return &foundryv1.FoundryAgentActionResponse{Action: &foundryv1.FoundryAgentAction{Id: a.ID, Status: string(a.Status)}}, nil
}

// sessions opens a session for any member but those refused.
type sessions struct {
	founder *founder
	mu      sync.Mutex
	refused map[string]error
}

func (s *sessions) Session(ctx context.Context, actor ports.Actor, _ integration.ConfirmMode) (integration.Session, error) {
	s.mu.Lock()
	err := s.refused[actor.UserID]
	s.mu.Unlock()
	if err != nil {
		return integration.Session{}, err
	}
	return integration.Session{Ctx: ports.WithActor(ctx, actor), Founder: s.founder}, nil
}

func (s *sessions) refuse(userID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refused[userID] = err
}

// world is one test's install, people and services.
type world struct {
	t        *testing.T
	ctx      context.Context
	db       store.Store
	slack    *fakeSlack
	keys     *secretbox.Keyring
	founder  *founder
	sessions *sessions
	bot      *Bot
	cfg      config.SlackConfig
	ws       string
	install  *model.SlackInstall
	events   atomic.Int64
	clicks   atomic.Int64
}

// slackTables and agentTables are cleared before each test, so a test's
// claims see only its own rows.
var slackTables = []string{"foundry_slack_installs", "foundry_slack_install_tokens", "foundry_slack_install_states", "foundry_slack_links",
	"foundry_slack_link_tokens", "foundry_slack_threads", "foundry_slack_intake", "foundry_slack_replies", "foundry_agent_threads",
	"foundry_agent_turns", "foundry_agent_events", "foundry_agent_messages", "foundry_agent_actions", "foundry_questions", "foundry_ontologies",
	"workspaces"}

// newWorld is a workspace with an install of team, Ada and Grace linked,
// and a bot on it; with mutates its options first.
func newWorld(t *testing.T, with ...func(*Options)) *world {
	t.Helper()
	ctx := context.Background()
	for _, table := range slackTables {
		_, err := testDB.Exec("DELETE FROM " + table)
		require.NoError(t, err)
	}
	db := store.NewMySQL(testDB, workspaces{})
	ws := id.New()
	_, err := testDB.Exec(`INSERT INTO workspaces (id) VALUES (?)`, ws)
	require.NoError(t, err)
	keys, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)})
	require.NoError(t, err)

	apiCfg := config.Defaults()
	apiCfg.Foundry.Agent.Enabled = true
	apiCfg.Foundry.AI.Default = config.AIDefault{Provider: "anthropic", APIKey: "sk-ant-helix-0123456789", Models: config.AIModels{
		Draft: "claude-opus-5-5", Ask: "claude-sonnet-5-5", Extract: "claude-haiku-5-5", Verify: "claude-sonnet-5-5"}}
	service, err := api.New(api.Deps{Cfg: &apiCfg, DB: db})
	require.NoError(t, err)
	f := &founder{api: service, db: db, keys: keys}
	s := &sessions{founder: f, refused: map[string]error{}}

	fake := newFakeSlack(t)
	limiter, err := slack.NewLimiter(1)
	require.NoError(t, err)
	client, err := slack.NewClient(slack.ClientOptions{BaseURL: fake.URL + "/api", Limiter: limiter})
	require.NoError(t, err)
	cfg := config.Defaults().Foundry.Slack
	cfg.Enabled, cfg.SiteURL, cfg.ClientID, cfg.ClientSecret = true, siteURL, "1.2", "client secret"
	cfg.PollInterval, cfg.RetryBackoff, cfg.Lease = 20*time.Millisecond, 50*time.Millisecond, 30*time.Second
	o := Options{DB: db, Sessions: s, Slack: client, Keys: keys, Cfg: cfg}
	for _, w := range with {
		w(&o)
	}
	b, err := New(o)
	require.NoError(t, err)

	install, _, err := db.InstallFoundrySlack(ctx, &model.SlackInstall{ID: id.New(), WorkspaceID: ws, TeamID: team, TeamName: "Acme", AppID: "A0FOUNDRY",
		BotUserID: botUser, Scopes: slack.Scopes, InstalledByUserID: adaID, InstalledBySlackUserID: ada},
		func(installID string) (model.SlackSealedToken, error) {
			sealed, keyID, err := keys.Seal([]byte("xoxb-token"), installID)
			return model.SlackSealedToken{Sealed: sealed, KeyID: keyID}, err
		})
	require.NoError(t, err)
	w := &world{t: t, ctx: ctx, db: db, slack: fake, keys: keys, founder: f, sessions: s, bot: b, cfg: o.Cfg, ws: ws, install: install}
	w.link(ada, adaID)
	w.link(grace, graceID)
	return w
}

// link links a Slack user to a Foundry user, as the site does.
func (w *world) link(slackUser, userID string) {
	w.t.Helper()
	token := "hx_sl_" + id.New()
	sum := sha256.Sum256([]byte(token))
	require.NoError(w.t, w.db.CreateFoundrySlackLinkToken(w.ctx, w.install.ID, slackUser, hex.EncodeToString(sum[:]), time.Minute))
	_, err := w.db.LinkFoundrySlackUser(w.ctx, w.ws, hex.EncodeToString(sum[:]), userID)
	require.NoError(w.t, err)
}

// ts is a Slack ts for n.
func ts(n int) string { return fmt.Sprintf("1700000000.%06d", n) }

// message queues a message from user in channel's thread threadTS (its own
// ts at top level), as the API records one; a channel not a DM is a
// mention.
func (w *world) message(user, channel, threadTS, msgTS, text string) string {
	w.t.Helper()
	at := slack.Conversation{TeamID: team, ChannelID: channel, ThreadTS: threadTS}
	var e slack.Event = slack.DirectMessage{At: at, UserID: user, TS: msgTS, Text: text, EventTime: time.Now()}
	if !inDM(channel) {
		e = slack.Mention{At: at, UserID: user, TS: msgTS, Text: text, EventTime: time.Now()}
	}
	return w.queueEvent(e, user, model.SlackConversation(w.install.ID, channel, threadTS, user))
}

func (w *world) queueEvent(e slack.Event, user, conversation string) string {
	w.t.Helper()
	payload, err := slack.EventPayload(e)
	require.NoError(w.t, err)
	in := &model.SlackIntake{ID: id.New(), InstallID: w.install.ID, DedupeKey: model.SlackEventDedupe(fmt.Sprintf("Ev%04d", w.events.Add(1))),
		Kind: model.SlackIntakeEvent, Conversation: conversation, SlackUserID: user, Payload: payload}
	require.NoError(w.t, w.db.QueueFoundrySlackIntake(w.ctx, in))
	return in.ID
}

// click queues a click, as the API records one.
func (w *world) click(in slack.Interaction, user string) string {
	w.t.Helper()
	payload, err := slack.InteractionPayload(in)
	require.NoError(w.t, err)
	row := &model.SlackIntake{ID: id.New(), InstallID: w.install.ID, DedupeKey: slack.InteractionKey(in), Kind: model.SlackIntakeInteraction,
		SlackUserID: user, Payload: payload}
	require.NoError(w.t, w.db.QueueFoundrySlackIntake(w.ctx, row))
	return row.ID
}

// clicked is user's click on message in channel's thread, a fresh one.
func (w *world) clicked(user, channel, threadTS, messageTS string) slack.Clicked {
	return slack.Clicked{TeamID: team, UserID: user, ChannelID: channel, ThreadTS: threadTS, MessageTS: messageTS,
		ActionTS: fmt.Sprintf("1700000500.%06d", w.clicks.Add(1))}
}

// intakeRow is an intake row's status, attempts and wait.
type intakeRow struct {
	status   string
	attempts int
	waits    sql.NullString
	next     time.Time
	error    sql.NullString
}

func (w *world) row(id string) intakeRow {
	w.t.Helper()
	var r intakeRow
	require.NoError(w.t, testDB.QueryRow(`SELECT status, attempts, waits_on_turn, next_attempt_at, error FROM foundry_slack_intake WHERE id=?`, id).
		Scan(&r.status, &r.attempts, &r.waits, &r.next, &r.error))
	return r
}

// handle handles every intake row due now, one at a time, and says how many.
func (w *world) handle() int {
	w.t.Helper()
	n := 0
	for {
		run, err := w.bot.NextIntake(w.ctx)
		require.NoError(w.t, err)
		if run == nil {
			return n
		}
		run(w.ctx)
		n++
	}
}

// follow follows the replies due now for at most d in all: a live turn's
// follow gives its reply back then, and is not claimed again in this call.
func (w *world) follow(d time.Duration) int {
	w.t.Helper()
	ctx, cancel := context.WithTimeout(w.ctx, d)
	defer cancel()
	n := 0
	for ctx.Err() == nil {
		run, err := w.bot.NextFollow(w.ctx)
		require.NoError(w.t, err)
		if run == nil {
			return n
		}
		run(ctx)
		n++
	}
	return n
}

// until handles and follows until done says so, or fails the test after a
// while.
func (w *world) until(what string, done func() bool) {
	w.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			w.t.Fatalf("timed out waiting for %s", what)
		}
		if w.handle()+w.follow(300*time.Millisecond) == 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// settle handles and follows until no intake row is open.
func (w *world) settle() {
	w.t.Helper()
	w.until("every message handled", func() bool {
		var open int
		require.NoError(w.t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_slack_intake WHERE status IN ('queued', 'running')`).Scan(&open))
		return open == 0
	})
}

// replyDone reports whether turnID's reply finished.
func (w *world) replyDone(turnID string) func() bool {
	return func() bool {
		r, err := w.db.GetFoundrySlackReply(w.ctx, turnID)
		return err == nil && r.Status == model.SlackReplyDone
	}
}

// turns is the workspace's turns, oldest first.
func (w *world) turns() []*model.FoundryAgentTurn {
	w.t.Helper()
	rows, err := testDB.Query(`SELECT id FROM foundry_agent_turns WHERE workspace_id=? ORDER BY created_at, id`, w.ws)
	require.NoError(w.t, err)
	defer func() { _ = rows.Close() }()
	var out []*model.FoundryAgentTurn
	for rows.Next() {
		var id string
		require.NoError(w.t, rows.Scan(&id))
		turn, err := w.db.GetFoundryAgentTurn(w.ctx, w.ws, id)
		require.NoError(w.t, err)
		out = append(out, turn)
	}
	return out
}

// model is the stub model: it claims the next turn, which must be turnID,
// and runs it as the agent worker would: Started, then do's events, then
// it settles the turn done (or failed, when the last event says so).
type stubRun struct {
	w      *world
	turn   *model.FoundryAgentTurn
	worker string
}

func (w *world) claimTurn(turnID string) *stubRun {
	w.t.Helper()
	worker := id.NewWorkerID()
	now := time.Now().UTC()
	limits := model.FoundryAgentLimits{Since: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), CapMicros: 1e12, PerHour: 1000, Running: 100}
	turn, err := w.db.ClaimFoundryAgentTurn(w.ctx, worker, model.FoundryAgentClaim{Runtime: model.FoundryAgentRuntimeAPI, Lease: time.Minute,
		Defer: time.Second, Limits: limits, Automations: limits})
	require.NoError(w.t, err)
	require.NotNil(w.t, turn, "no turn to claim")
	require.Equal(w.t, turnID, turn.ID)
	r := &stubRun{w: w, turn: turn, worker: worker}
	r.emit(agent.Started{Model: "stub", Attempt: 1})
	return r
}

func (r *stubRun) emit(events ...agent.Event) {
	r.w.t.Helper()
	raw := make([]json.RawMessage, 0, len(events))
	for _, e := range events {
		b, err := agent.MarshalEvent(e)
		require.NoError(r.w.t, err)
		raw = append(raw, b)
	}
	require.NoError(r.w.t, r.w.db.AppendFoundryAgentEvents(r.w.ctx, r.turn.ID, r.worker, raw))
}

// card records an action the turn prepared (create_load), its confirmation
// expiring at expires, with its card event.
func (r *stubRun) card(expires time.Time) *model.FoundryAgentAction {
	r.w.t.Helper()
	display, err := json.Marshal(agent.CreateLoadDisplay{Ontology: agent.Ref{ID: "ont_1", Name: "Sales"}, Version: agent.VersionRef{ID: "ver_3", Number: 3}})
	require.NoError(r.w.t, err)
	actionID := id.New()
	sealed, keyID, err := r.w.keys.Seal([]byte("token"), actionID)
	require.NoError(r.w.t, err)
	hash := fmt.Sprintf("%064x", time.Now().UnixNano())
	a, err := r.w.db.RecordFoundryAgentAction(r.w.ctx, r.worker, &model.FoundryAgentAction{ID: actionID, TurnID: r.turn.ID, ThreadID: r.turn.ThreadID,
		WorkspaceID: r.w.ws, UserID: r.turn.UserID, Operation: "create_load", Target: "ont_1", Payload: json.RawMessage(`{"version_id":"ver_3"}`),
		PayloadHash: hash, Display: display, ConfirmationID: "conf-1", TokenSealed: sealed, KeyID: keyID, Status: model.FoundryAgentActionPrepared,
		ExpiresAt: expires}, 5, func(a *model.FoundryAgentAction) (json.RawMessage, error) {
		return agent.MarshalEvent(cardOf(a))
	})
	require.NoError(r.w.t, err)
	return a
}

// ask records the questions the turn asks its person, with its event.
func (r *stubRun) ask(questions ...string) {
	r.w.t.Helper()
	asked := &model.FoundryAgentAsked{}
	for i, q := range questions {
		asked.Questions = append(asked.Questions, agentQuestion(fmt.Sprintf("q%d", i+1), q))
	}
	event, err := agent.MarshalEvent(agent.Asked{Questions: asked.Questions})
	require.NoError(r.w.t, err)
	require.NoError(r.w.t, r.w.db.AskFoundryAgentQuestions(r.w.ctx, r.turn.ID, r.worker, asked, event))
}

// finish ends the turn: done with reason, or failed with why.
func (r *stubRun) finish(reason agent.StopReason, why string) {
	r.w.t.Helper()
	outcome := model.FoundryAgentTurnOutcome{Status: model.FoundryAgentTurnDone, Steps: 1}
	if reason == agent.StopFailed {
		outcome = model.FoundryAgentTurnOutcome{Status: model.FoundryAgentTurnFailed, Error: why, Steps: 1}
	}
	r.emit(agent.Finished{Status: string(outcome.Status), Reason: reason, Error: why})
	require.NoError(r.w.t, r.w.db.SettleFoundryAgentTurn(r.w.ctx, r.turn.ID, r.worker, outcome))
}

// hashOf is a token's SHA-256, in hex, as the store keeps it.
func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
