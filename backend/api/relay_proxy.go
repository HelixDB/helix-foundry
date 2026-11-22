package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"golang.org/x/time/rate"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/internal/iplimit"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The relay's Slack Web API proxy (design §10.4, connector §11): a linked
// install calls Slack as one of its installs, and only within what Helix's
// own bot would do there. A linked install is untrusted, so each call is
// checked, in order: the method is one the relay allows (relayMethods; the
// data connector's only while hosted's foundry.slack.feed is on); the body
// is a JSON object within relay.max_call_bytes, naming no mass mention
// (<!channel>, <!here>, <!everyone>, <!subteam^…>, or a rich_text broadcast
// or user group, also in blocks sent as a JSON string); the channel it
// names is one the install was sent something from in 30 days, and the
// users it names ones that wrote to it or were named there; the link's
// calls a minute (relay.calls_per_minute) and the install's share of
// Slack's tier for the method on this replica, or a Tier-1 read's slot
// shared by every replica, have room; and a channel outside a direct
// message isn't shared with another organisation, as Slack's
// conversations.info last said within relaySharedFor (a channel can be
// shared after it was recorded, and a click there carries no word of it).
// A message is
// rewritten as the bot's own are (every mrkdwn text verbatim, parse=none,
// no unfurls, nothing that posts as another name or icon, broadcasts or
// carries metadata). Slack's answer comes back without any token, and
// users.info's without the user's email or phone; users.conversations'
// channels become channels the install may name, as conversations.open's
// direct message does, unless shared with another organisation.

// maxRelayAnswer is the most of Slack's answer the proxy passes back, within
// the door's call.
const maxRelayAnswer = 3 << 20

// relayMethod is what the relay allows of one Slack method.
type relayMethod struct {
	// perMinute is Slack's tier for the method, per app and workspace; 0
	// for a Tier-1 read, paced by its slot instead.
	perMinute int
	// form: Slack takes the method's arguments as a form, each a string.
	form bool
	// channel names the argument holding a channel the install must know;
	// "" for none, which the call may then not name.
	channel string
	// users: the call names users ("user", or "users" comma-separated) the
	// install must know.
	users bool
	// posts: the call says something in Slack, and is rewritten.
	posts bool
	// feed: the data connector's, only while hosted's feed is on.
	feed bool
	// slot is a Tier-1 read's slot.
	slot model.SlackSlotMethod
}

// relayMethods are the methods a linked install may call: the bot's, and
// the data connector's reads.
var relayMethods = map[string]relayMethod{
	"chat.postMessage":          {perMinute: 100, channel: "channel", posts: true},
	"chat.update":               {perMinute: 50, channel: "channel", posts: true},
	"chat.postEphemeral":        {perMinute: 100, channel: "channel", users: true, posts: true},
	"users.info":                {perMinute: 100, form: true, users: true},
	"conversations.info":        {perMinute: 50, form: true, channel: "channel"},
	"conversations.open":        {perMinute: 50, users: true},
	"agents.sessions.setStatus": {perMinute: 50, channel: "channel_id"},
	"conversations.history":     {form: true, channel: "channel", feed: true, slot: model.SlackSlotHistory},
	"conversations.replies":     {form: true, channel: "channel", feed: true, slot: model.SlackSlotReplies},
	"conversations.members":     {perMinute: 100, form: true, channel: "channel", feed: true},
	"users.conversations":       {perMinute: 50, form: true, feed: true},
	"conversations.list":        {perMinute: 20, form: true, feed: true},
	"users.list":                {perMinute: 20, form: true, feed: true},
	"auth.test":                 {perMinute: 100, form: true, feed: true},
}

// relayDropped are a message's arguments the relay drops: they would post
// as another name or icon, broadcast a reply, link names Slack parses, or
// carry hidden data.
var relayDropped = []string{"username", "icon_url", "icon_emoji", "as_user", "reply_broadcast", "link_names", "metadata"}

// relayMassMentions are the mentions no relayed call may carry.
var relayMassMentions = []string{"<!channel", "<!here", "<!everyone", "<!subteam"}

// relaySharedFor is how long Slack's word that an install's channel is or
// isn't shared with another organisation stands before it is asked again.
const relaySharedFor = 5 * time.Minute

// relayShared is what Slack last said of a channel: whether it is shared
// with another organisation, standing until until.
type relayShared struct {
	shared bool
	until  time.Time
}

var relayCalls = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_relay_calls_total",
	Help: "Slack Web API calls linked installs made through the relay, by method (other for one it doesn't allow) and outcome: ok, " +
		"slack_error (Slack's ok false), refused (the relay's checks), limited (the relay's limits or Slack's 429), or failed.",
}, []string{"method", "outcome"})

// relayLimits paces relayed calls on this replica: each link's calls a
// minute, each install's share of each method's tier, and Slack's own
// Retry-After per install and method; and whether each install's channels
// are shared with another organisation, as Slack last said. Its zero value
// is ready; its buckets are made at the first call, from foundry.slack.
type relayLimits struct {
	once    sync.Once
	links   *iplimit.Limiters
	methods map[string]*iplimit.Limiters
	mu      sync.Mutex
	blocked map[string]time.Time
	// shared is by install and channel, at most relayCapacity.
	shared map[string]relayShared
	http   *http.Client
}

// relayCapacity bounds the links and installs a replica paces at once.
const relayCapacity = 10_000

func (l *relayLimits) init(perMinute int, share float64) {
	l.once.Do(func() {
		l.links = iplimit.New(rate.Limit(float64(perMinute)/60), perMinute, relayCapacity)
		l.methods = map[string]*iplimit.Limiters{}
		for name, m := range relayMethods {
			if m.perMinute > 0 {
				per := float64(m.perMinute) * share
				l.methods[name] = iplimit.New(rate.Limit(per/60), max(1, int(per/6)), relayCapacity)
			}
		}
		l.blocked = map[string]time.Time{}
		l.shared = map[string]relayShared{}
		l.http = &http.Client{Timeout: slackCallTimeout}
	})
}

// take takes one of reservations' buckets at now, all or none: the longest
// wait when any has none.
func take(now time.Time, reservations ...*rate.Reservation) time.Duration {
	var wait time.Duration
	for _, r := range reservations {
		wait = max(wait, r.DelayFrom(now))
	}
	if wait > 0 {
		for _, r := range reservations {
			r.CancelAt(now)
		}
	}
	return wait
}

// CallSlackForRelay calls a Slack Web API method for a linked install, as
// its install serving local_workspace_id, within the relay's checks (above).
// A call the relay's limits, the slot, or Slack's 429 hold back answers
// retry_after_s and no body. Each call, refused or made, is counted on the
// link's day.
func (s *Service) CallSlackForRelay(ctx context.Context, req *foundryv1.CallSlackForRelayRequest) (*foundryv1.CallSlackForRelayResponse, error) {
	link, err := s.relayLink(ctx, req.GetTokenSha256())
	if err != nil {
		return nil, err
	}
	name := req.GetMethod()
	method, known := relayMethods[name]
	label := name
	if !known {
		label = "other"
	}
	resp, outcome, err := s.callSlackForRelay(ctx, link, req, method, known)
	relayCalls.WithLabelValues(label, outcome).Inc()
	usage := model.RelayUsage{Calls: 1}
	if outcome == "refused" {
		usage = model.RelayUsage{Refused: 1}
	}
	if countErr := s.deps.DB.CountFoundryRelayUsage(ctx, link.ID, usage); countErr != nil {
		log.From(ctx).Warn("count a relayed call failed", "link_id", link.ID, "err", countErr)
	}
	return resp, err
}

// callSlackForRelay checks and makes one relayed call, naming its outcome.
func (s *Service) callSlackForRelay(ctx context.Context, link *model.RelayLink, req *foundryv1.CallSlackForRelayRequest, method relayMethod,
	known bool) (*foundryv1.CallSlackForRelayResponse, string, error) {
	cfg := s.deps.Cfg.Foundry.Slack
	refuse := func(format string, args ...any) (*foundryv1.CallSlackForRelayResponse, string, error) {
		return nil, "refused", status.Errorf(codes.PermissionDenied, format, args...)
	}
	invalid := func(format string, args ...any) (*foundryv1.CallSlackForRelayResponse, string, error) {
		return nil, "refused", status.Errorf(codes.InvalidArgument, format, args...)
	}
	name, lws := req.GetMethod(), req.GetLocalWorkspaceId()
	switch {
	case !known:
		return refuse("the relay doesn't call %q", truncateUTF8(name, 64))
	case method.feed && !cfg.Feed.Enabled:
		return refuse("%s is the Slack data connector's, which Helix doesn't relay here", name)
	case !localWorkspacePattern.MatchString(lws):
		return invalid("local_workspace_id is the install's workspace id")
	case int64(len(req.GetBodyJson())) > cfg.Relay.MaxCallBytes:
		return invalid("the call's body is %d bytes; the relay takes at most %d", len(req.GetBodyJson()), cfg.Relay.MaxCallBytes)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(req.GetBodyJson()), &args); err != nil || args == nil {
		return invalid("body_json is a JSON object")
	}
	if mention := relayMassMention(args); mention != "" {
		return refuse("the relay posts no %s> mention", mention)
	}
	install, err := s.deps.DB.GetFoundryRelayInstall(ctx, link.ID, lws)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, "refused", status.Error(codes.NotFound, "the link has no Slack install for this workspace")
	case err != nil:
		return nil, "failed", status.Errorf(codes.Unavailable, "read the relayed install: %v", err)
	}
	// What the call names must be the install's.
	if method.channel != "" {
		channel, _ := args[method.channel].(string)
		if !model.ValidSlackID(channel) {
			return invalid("%s names its channel in %s", name, method.channel)
		}
		knows, err := s.deps.DB.KnowsFoundryRelayChannel(ctx, install.ID, channel)
		if err != nil {
			return nil, "failed", status.Errorf(codes.Unavailable, "read the install's channels: %v", err)
		}
		if !knows {
			return refuse("%s names a channel this install wasn't sent anything from", name)
		}
	}
	for _, arg := range []string{"channel", "channel_id"} {
		if _, named := args[arg]; named && arg != method.channel {
			return refuse("%s names no %s through the relay", name, arg)
		}
	}
	var users []string
	if u, ok := args["user"].(string); ok {
		users = append(users, u)
	}
	if u, ok := args["users"].(string); ok {
		users = append(users, strings.Split(u, ",")...)
	}
	_, namesUser := args["user"]
	_, namesUsers := args["users"]
	switch {
	case name == "users.conversations" && namesUser && args["user"] != install.BotUserID:
		return refuse("users.conversations lists the bot's own channels only")
	case name == "users.conversations":
	case method.users && len(users) == 0:
		return invalid("%s names its user in user (users for conversations.open)", name)
	case !method.users && (namesUser || namesUsers):
		return refuse("%s names no user through the relay", name)
	case method.users:
		for _, u := range users {
			if !model.ValidSlackID(u) {
				return invalid("%q is not a Slack user", u)
			}
		}
		knows, err := s.deps.DB.KnowsFoundryRelayUsers(ctx, install.ID, users)
		if err != nil {
			return nil, "failed", status.Errorf(codes.Unavailable, "read the install's users: %v", err)
		}
		if !knows {
			return refuse("%s names a user who never wrote to this install", name)
		}
	}
	if method.form {
		for k, v := range args {
			switch v.(type) {
			case string, float64, bool:
			default:
				return invalid("%s takes a form: %s is a string, number or boolean", name, k)
			}
		}
	}
	if method.posts {
		rewriteRelayMessage(args)
	}
	// The relay's limits, then the slot every replica shares.
	l := &s.relay
	l.init(cfg.Relay.CallsPerMinute, cfg.APIShare)
	now := time.Now()
	key := install.ID + " " + name
	l.mu.Lock()
	blocked := l.blocked[key]
	l.mu.Unlock()
	if wait := blocked.Sub(now); wait > 0 {
		return relayRetry(wait), "limited", nil
	}
	reservations := []*rate.Reservation{l.links.Reserve(link.ID, now)}
	if m, ok := l.methods[name]; ok {
		reservations = append(reservations, m.Reserve(install.ID, now))
	}
	if wait := take(now, reservations...); wait > 0 {
		return relayRetry(wait), "limited", nil
	}
	bot, err := s.slackBot(ctx, install.ID)
	if err != nil {
		return nil, "failed", status.Errorf(codes.Unavailable, "open the install's token: %v", err)
	}
	// conversations.info is how the install's bot asks itself, and answers
	// the same either way.
	if channel, _ := args[method.channel].(string); method.channel != "" && name != "conversations.info" && !strings.HasPrefix(channel, "D") {
		shared, err := s.relayExtShared(ctx, install.ID, bot.Token, channel, now)
		switch {
		case err != nil:
			return nil, "failed", status.Errorf(codes.Unavailable, "ask Slack whether %s is shared with another organisation: %v", channel, err)
		case shared:
			return refuse("%s names a channel shared with another organisation", name)
		}
	}
	if method.slot != "" {
		perMinute := cfg.Backfill.HistoryPerMinute
		if method.slot == model.SlackSlotReplies {
			perMinute = cfg.Backfill.RepliesPerMinute
		}
		every := time.Minute / time.Duration(max(perMinute, 1))
		reserved, err := s.deps.DB.ReserveFoundrySlackSlot(ctx, install.ID, method.slot, every)
		if err != nil {
			return nil, "failed", status.Errorf(codes.Unavailable, "reserve the install's %s slot: %v", method.slot, err)
		}
		if !reserved {
			return relayRetry(every), "limited", nil
		}
	}
	code, header, answer, err := s.sendSlack(ctx, name, method.form, bot.Token, args)
	if err != nil {
		return nil, "failed", status.Errorf(codes.Unavailable, "Slack couldn't be reached: %v", err)
	}
	if code == http.StatusTooManyRequests {
		wait := 30 * time.Second
		if seconds, err := strconv.Atoi(header.Get("Retry-After")); err == nil && seconds > 0 {
			wait = time.Duration(seconds) * time.Second
		}
		l.mu.Lock()
		for k, until := range l.blocked {
			if !until.After(now) {
				delete(l.blocked, k)
			}
		}
		l.blocked[key] = now.Add(wait)
		l.mu.Unlock()
		if method.slot != "" {
			if err := s.deps.DB.DeferFoundrySlackSlot(ctx, install.ID, method.slot, wait); err != nil {
				log.From(ctx).Warn("defer a relayed install's slot failed", "install_id", install.ID, "err", err)
			}
		}
		return relayRetry(wait), "limited", nil
	}
	if code != http.StatusOK {
		return nil, "failed", status.Errorf(codes.Unavailable, "Slack answered HTTP %d", code)
	}
	var out map[string]any
	if err := json.Unmarshal(answer, &out); err != nil || out == nil {
		return nil, "failed", status.Error(codes.Unavailable, "Slack's answer wasn't a JSON object")
	}
	ok, _ := out["ok"].(bool)
	if ok {
		s.learnRelayAnswer(ctx, install.ID, name, out)
	}
	scrubRelayAnswer(name, out)
	body, err := json.Marshal(out)
	if err != nil {
		return nil, "failed", status.Errorf(codes.Internal, "encode Slack's answer: %v", err)
	}
	outcome := "ok"
	if !ok {
		outcome = "slack_error"
	}
	return &foundryv1.CallSlackForRelayResponse{BodyJson: string(body)}, outcome, nil
}

// relayRetry is an answer that the call may go after wait, in whole seconds.
func relayRetry(wait time.Duration) *foundryv1.CallSlackForRelayResponse {
	return &foundryv1.CallSlackForRelayResponse{RetryAfterS: int32(min(math.Ceil(wait.Seconds()), math.MaxInt32))}
}

// relayMassMention is the first mass mention v carries, as its <!…> mention,
// "" for none: in any string, in any JSON a string holds (blocks or
// attachments sent as text), or as a rich_text element, a broadcast (as
// its range) or a user group (<!subteam).
func relayMassMention(v any) string {
	switch v := v.(type) {
	case string:
		for _, m := range relayMassMentions {
			if strings.Contains(v, m) {
				return m
			}
		}
		var inner any
		if t := strings.TrimSpace(v); (strings.HasPrefix(t, "[") || strings.HasPrefix(t, "{")) && json.Unmarshal([]byte(t), &inner) == nil {
			return relayMassMention(inner)
		}
	case map[string]any:
		switch v["type"] {
		case "broadcast":
			if r, _ := v["range"].(string); r == "here" || r == "everyone" {
				return "<!" + r
			}
			return "<!channel"
		case "usergroup":
			return "<!subteam"
		}
		for _, e := range v {
			if m := relayMassMention(e); m != "" {
				return m
			}
		}
	case []any:
		for _, e := range v {
			if m := relayMassMention(e); m != "" {
				return m
			}
		}
	}
	return ""
}

// rewriteRelayMessage makes a message's arguments the bot's own: every
// mrkdwn text verbatim, no parsing, unfurls, other name or icon,
// broadcast or metadata.
func rewriteRelayMessage(args map[string]any) {
	for _, k := range relayDropped {
		delete(args, k)
	}
	args["parse"], args["unfurl_links"], args["unfurl_media"] = "none", false, false
	var verbatim func(v any)
	verbatim = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if v["type"] == "mrkdwn" {
				v["verbatim"] = true
			}
			for _, e := range v {
				verbatim(e)
			}
		case []any:
			for _, e := range v {
				verbatim(e)
			}
		}
	}
	verbatim(args)
}

// scrubRelayAnswer drops from Slack's answer to method anything that is a
// token, and users.info's email and phone; users.list keeps its emails
// (the data connector's people), not their phones.
func scrubRelayAnswer(method string, answer map[string]any) {
	var scrub func(v any)
	scrub = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			for k, e := range v {
				if k == "token" || strings.HasSuffix(k, "_token") {
					delete(v, k)
					continue
				}
				scrub(e)
			}
		case []any:
			for _, e := range v {
				scrub(e)
			}
		}
	}
	scrub(answer)
	profiles := func(users ...any) {
		for _, u := range users {
			user, _ := u.(map[string]any)
			profile, _ := user["profile"].(map[string]any)
			delete(profile, "phone")
			if method == "users.info" {
				delete(profile, "email")
			}
		}
	}
	switch method {
	case "users.info":
		profiles(answer["user"])
	case "users.list":
		members, _ := answer["members"].([]any)
		profiles(members...)
	}
}

// learnRelayAnswer records what an answer lets the install's calls name
// next: the channels users.conversations lists the bot in (never one
// shared with another organisation, and no direct message, whose data no
// connector reads), and the direct message conversations.open opened with
// users it knows (never one shared with another organisation); and what
// conversations.info said of a channel's sharing.
func (s *Service) learnRelayAnswer(ctx context.Context, installID, method string, answer map[string]any) {
	sharedOf := func(ch map[string]any) bool {
		shared, _ := ch["is_ext_shared"].(bool)
		pending, _ := ch["is_pending_ext_shared"].(bool)
		return shared || pending
	}
	var channels []string
	switch method {
	case "conversations.info":
		ch, _ := answer["channel"].(map[string]any)
		if id, _ := ch["id"].(string); model.ValidSlackID(id) {
			s.relay.sawShared(installID, id, sharedOf(ch), time.Now())
		}
	case "users.conversations":
		listed, _ := answer["channels"].([]any)
		for _, c := range listed {
			ch, _ := c.(map[string]any)
			id, _ := ch["id"].(string)
			im, _ := ch["is_im"].(bool)
			mpim, _ := ch["is_mpim"].(bool)
			if model.ValidSlackID(id) && !sharedOf(ch) && !im && !mpim {
				channels = append(channels, id)
			}
		}
	case "conversations.open":
		ch, _ := answer["channel"].(map[string]any)
		if id, _ := ch["id"].(string); model.ValidSlackID(id) && !sharedOf(ch) {
			channels = append(channels, id)
		}
	}
	if len(channels) == 0 {
		return
	}
	if err := s.deps.DB.RecordFoundryRelayChannels(ctx, installID, channels); err != nil {
		log.From(ctx).Warn("record a relayed install's channels failed", "install_id", installID, "err", err)
	}
}

// relayExtShared reports whether the install's channel is shared with
// another organisation, or invited to be: as Slack last said within
// relaySharedFor, else as conversations.info says now, with the install's
// token. An answer that says neither is an error.
func (s *Service) relayExtShared(ctx context.Context, installID, token, channel string, now time.Time) (bool, error) {
	l, key := &s.relay, installID+" "+channel
	l.mu.Lock()
	known, ok := l.shared[key]
	l.mu.Unlock()
	if ok && now.Before(known.until) {
		return known.shared, nil
	}
	code, _, answer, err := s.sendSlack(ctx, "conversations.info", true, token, map[string]any{"channel": channel})
	switch {
	case err != nil:
		return false, err
	case code != http.StatusOK:
		return false, fmt.Errorf("Slack answered HTTP %d", code)
	}
	var out struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Channel struct {
			ID               string `json:"id"`
			ExtShared        bool   `json:"is_ext_shared"`
			PendingExtShared bool   `json:"is_pending_ext_shared"`
		} `json:"channel"`
	}
	switch err := json.Unmarshal(answer, &out); {
	case err != nil:
		return false, fmt.Errorf("Slack's answer wasn't conversations.info's: %w", err)
	case !out.OK:
		return false, fmt.Errorf("Slack answered %s", truncateUTF8(out.Error, 64))
	case out.Channel.ID != channel:
		return false, errors.New("Slack answered for another channel")
	}
	shared := out.Channel.ExtShared || out.Channel.PendingExtShared
	l.sawShared(installID, channel, shared, now)
	return shared, nil
}

// sawShared records what Slack said at now of whether the install's channel
// is shared with another organisation. Past relayCapacity channels it
// forgets those whose word has lapsed, and, still full, all of them.
func (l *relayLimits) sawShared(installID, channel string, shared bool, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.shared) >= relayCapacity {
		maps.DeleteFunc(l.shared, func(_ string, s relayShared) bool { return !now.Before(s.until) })
		if len(l.shared) >= relayCapacity {
			clear(l.shared)
		}
	}
	l.shared[installID+" "+channel] = relayShared{shared: shared, until: now.Add(relaySharedFor)}
}

// sendSlack calls method with token: args as a form (each a string, or a
// number or boolean as JSON writes it) or as JSON. It returns Slack's
// status, headers and at most maxRelayAnswer of its answer.
func (s *Service) sendSlack(ctx context.Context, method string, form bool, token string, args map[string]any) (int, http.Header, []byte, error) {
	var (
		body        []byte
		contentType string
	)
	if form {
		values := url.Values{}
		for k, v := range args {
			if text, ok := v.(string); ok {
				values.Set(k, text)
				continue
			}
			raw, _ := json.Marshal(v) // the caller let only numbers and booleans by
			values.Set(k, string(raw))
		}
		body, contentType = []byte(values.Encode()), "application/x-www-form-urlencoded"
	} else {
		var err error
		if body, err = json.Marshal(args); err != nil {
			return 0, nil, nil, err
		}
		contentType = "application/json; charset=utf-8"
	}
	ctx, cancel := context.WithTimeout(ctx, slackCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.slackURL()+"/api/"+method, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.relay.http.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxRelayAnswer+1))
	switch {
	case err != nil:
		return 0, nil, nil, err
	case len(answer) > maxRelayAnswer:
		return 0, nil, nil, fmt.Errorf("the answer is past %d bytes: ask for fewer", maxRelayAnswer)
	}
	return resp.StatusCode, resp.Header, answer, nil
}
