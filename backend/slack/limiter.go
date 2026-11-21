package slack

import (
	"errors"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Method is a Web API method the bot calls: a closed set, each with
// Slack's rate limit for it.
type Method struct {
	name string
	// perMinute is Slack's limit per app, workspace and method: Tier 1
	// allows about 1 call a minute, Tier 3 50, Tier 4 100. 0 is a call not
	// made for an install (oauth.v2.access), which no Limiter paces.
	perMinute int
	// posts is a method that says something in a channel, which Slack also
	// limits to about one message a second per channel.
	posts bool
	// form is a method that takes only a form body; the others take JSON.
	form bool
}

// The methods the bot calls, with Slack's tiers.
var (
	// ChatPostMessage is a special tier: about 1 a second per channel, and
	// hundreds a minute per workspace.
	ChatPostMessage = Method{name: "chat.postMessage", perMinute: 100, posts: true}
	// ChatUpdate is Tier 3.
	ChatUpdate = Method{name: "chat.update", perMinute: 50}
	// ChatPostEphemeral is Tier 4, and posts.
	ChatPostEphemeral = Method{name: "chat.postEphemeral", perMinute: 100, posts: true}
	// UsersInfo is Tier 4.
	UsersInfo = Method{name: "users.info", perMinute: 100, form: true}
	// ConversationsOpen is Tier 3.
	ConversationsOpen = Method{name: "conversations.open", perMinute: 50}
	// AgentSessionSetStatus is Tier 3.
	AgentSessionSetStatus = Method{name: "agents.sessions.setStatus", perMinute: 50}
	// AppsUninstall is Tier 1.
	AppsUninstall = Method{name: "apps.uninstall", perMinute: 1, form: true}
	// OAuthV2Access is the app's, not an install's: no Limiter paces it.
	OAuthV2Access = Method{name: "oauth.v2.access", form: true}
)

// String is the method's name.
func (m Method) String() string { return m.name }

// channelPerSecond is Slack's limit on messages in one channel.
const channelPerSecond = 1.0

// limiterIdle is how long a bucket unused (and not blocked) is kept.
const limiterIdle = 10 * time.Minute

// Limiter paces the Web API calls one process makes: per (install, method)
// at its share of the method's tier, and per (install, channel) at its share
// of a message a second for methods that post. It never waits: Take says how
// long until a call may go, and the caller defers its work that long. Block
// records Slack's own Retry-After. It is safe for concurrent use.
type Limiter struct {
	share float64

	mu      sync.Mutex
	buckets map[bucketKey]*bucket
	sweptAt time.Time
}

type bucketKey struct {
	install, method, channel string
}

type bucket struct {
	limit   *rate.Limiter
	blocked time.Time // no call before this (Slack's Retry-After)
	usedAt  time.Time
}

// NewLimiter is a Limiter that uses share of each of Slack's limits: one
// worker replica's part (config's api_share, 1/replicas). share is in (0, 1].
func NewLimiter(share float64) (*Limiter, error) {
	if !(share > 0 && share <= 1) {
		return nil, errors.New("slack: a limiter's share is in (0, 1]")
	}
	return &Limiter{share: share, buckets: map[bucketKey]*bucket{}}, nil
}

// get is key's bucket, made at rate perSecond with a burst of burst. l.mu
// is held.
func (l *Limiter) get(key bucketKey, perSecond float64, burst int, now time.Time) *bucket {
	if now.Sub(l.sweptAt) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.usedAt) > limiterIdle && !now.Before(b.blocked) {
				delete(l.buckets, k)
			}
		}
		l.sweptAt = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{limit: rate.NewLimiter(rate.Limit(perSecond), burst)}
		l.buckets[key] = b
	}
	b.usedAt = now
	return b
}

// Take takes one call of m for install at now, and of channel when m posts:
// 0 when it may go now, else how long until it may (taking nothing). A
// method no install's limit covers always goes.
func (l *Limiter) Take(install string, m Method, channel string, now time.Time) time.Duration {
	if m.perMinute == 0 {
		return 0
	}
	perMinute := float64(m.perMinute) * l.share
	l.mu.Lock()
	defer l.mu.Unlock()
	method := l.get(bucketKey{install: install, method: m.name}, perMinute/60, max(1, int(perMinute/6)), now)
	if now.Before(method.blocked) {
		return method.blocked.Sub(now)
	}
	r := method.limit.ReserveN(now, 1)
	if wait := r.DelayFrom(now); wait > 0 {
		r.CancelAt(now)
		return wait
	}
	if !m.posts {
		return 0
	}
	c := l.get(bucketKey{install: install, channel: channel}, channelPerSecond*l.share, 1, now)
	rc := c.limit.ReserveN(now, 1)
	if wait := rc.DelayFrom(now); wait > 0 {
		rc.CancelAt(now)
		r.CancelAt(now)
		return wait
	}
	return 0
}

// Block holds install's calls of m for retryAfter from now: Slack answered
// 429 with Retry-After.
func (l *Limiter) Block(install string, m Method, now time.Time, retryAfter time.Duration) {
	if m.perMinute == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	perMinute := float64(m.perMinute) * l.share
	b := l.get(bucketKey{install: install, method: m.name}, perMinute/60, max(1, int(perMinute/6)), now)
	if until := now.Add(retryAfter); until.After(b.blocked) {
		b.blocked = until
	}
}
