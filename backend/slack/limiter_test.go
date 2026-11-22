package slack_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/slack"
)

var t0 = time.Unix(1795000000, 0)

func newLimiter(t *testing.T, share float64) *slack.Limiter {
	t.Helper()
	l, err := slack.NewLimiter(share)
	require.NoError(t, err)
	return l
}

func TestMethodNames(t *testing.T) {
	require.Equal(t, "agents.sessions.setStatus", slack.AgentSessionSetStatus.String())
	require.Equal(t, "chat.postMessage", fmt.Sprint(slack.ChatPostMessage))
}

func TestNewLimiterShare(t *testing.T) {
	for _, share := range []float64{0, -0.5, 1.01} {
		_, err := slack.NewLimiter(share)
		require.Error(t, err, share)
	}
	_, err := slack.NewLimiter(1)
	require.NoError(t, err)
}

// A tier's burst goes at once, then calls are spaced at its share of the
// tier; installs and methods have their own buckets.
func TestLimiterTiers(t *testing.T) {
	l := newLimiter(t, 0.5)
	// chat.update, Tier 3: 50 a minute, half of it 25: a burst of 4, then one
	// every 2.4 s.
	for i := range 4 {
		require.Zero(t, l.Take("I1", slack.ChatUpdate, "", t0), i)
	}
	wait := l.Take("I1", slack.ChatUpdate, "", t0)
	require.InDelta(t, 2.4, wait.Seconds(), 0.01)
	// Refused takes take nothing: the wait doesn't grow.
	require.Equal(t, wait, l.Take("I1", slack.ChatUpdate, "", t0))
	require.Zero(t, l.Take("I1", slack.ChatUpdate, "", t0.Add(wait)))
	// Another install, another method: their own buckets.
	require.Zero(t, l.Take("I2", slack.ChatUpdate, "", t0))
	require.Zero(t, l.Take("I1", slack.UsersInfo, "", t0))
	// apps.uninstall, Tier 1: one, then a wait of two minutes at half.
	require.Zero(t, l.Take("I1", slack.AppsUninstall, "", t0))
	require.InDelta(t, 120, l.Take("I1", slack.AppsUninstall, "", t0).Seconds(), 0.01)
	// oauth.v2.access is no install's: never paced.
	for range 1000 {
		require.Zero(t, l.Take("", slack.OAuthV2Access, "", t0))
	}
}

// Posts are also paced per channel, at the share of a message a second; a
// post a channel refuses takes nothing from the method's bucket.
func TestLimiterChannels(t *testing.T) {
	l := newLimiter(t, 0.5)
	require.Zero(t, l.Take("I1", slack.ChatPostMessage, "C1", t0))
	wait := l.Take("I1", slack.ChatPostMessage, "C1", t0)
	require.Equal(t, 2*time.Second, wait)
	// An ephemeral in the same channel waits too; another channel doesn't.
	require.Equal(t, 2*time.Second, l.Take("I1", slack.ChatPostEphemeral, "C1", t0))
	require.Zero(t, l.Take("I1", slack.ChatPostMessage, "C2", t0))
	// chat.update doesn't post: no channel limit.
	require.Zero(t, l.Take("I1", slack.ChatUpdate, "C1", t0))
	require.Zero(t, l.Take("I1", slack.ChatUpdate, "C1", t0))
	// chat.postMessage at half is 50 a minute, a burst of 8: two used above,
	// and the refusals in C1 gave theirs back.
	for i := range 6 {
		require.Zero(t, l.Take("I1", slack.ChatPostMessage, fmt.Sprintf("D%d", i), t0), i)
	}
	require.NotZero(t, l.Take("I1", slack.ChatPostMessage, "D9", t0))
	require.Zero(t, l.Take("I1", slack.ChatPostMessage, "C1", t0.Add(2*time.Second)))
}

// Block holds a method for Slack's Retry-After, then lets it go; a shorter
// block doesn't shorten a longer one.
func TestLimiterBlock(t *testing.T) {
	l := newLimiter(t, 1)
	l.Block("I1", slack.ChatUpdate, t0, 30*time.Second)
	l.Block("I1", slack.ChatUpdate, t0, 10*time.Second)
	require.Equal(t, 30*time.Second, l.Take("I1", slack.ChatUpdate, "", t0))
	require.Equal(t, 5*time.Second, l.Take("I1", slack.ChatUpdate, "", t0.Add(25*time.Second)))
	require.Zero(t, l.Take("I1", slack.ChatUpdate, "", t0.Add(30*time.Second)))
	require.Zero(t, l.Take("I2", slack.ChatUpdate, "", t0))
	require.Zero(t, l.Take("I1", slack.UsersInfo, "", t0))
	l.Block("", slack.OAuthV2Access, t0, time.Hour)
	require.Zero(t, l.Take("", slack.OAuthV2Access, "", t0))
}

// Buckets idle ten minutes are forgotten (a fresh burst), but never while
// blocked.
func TestLimiterForgetsIdle(t *testing.T) {
	l := newLimiter(t, 1)
	require.Zero(t, l.Take("I1", slack.AppsUninstall, "", t0))
	require.NotZero(t, l.Take("I1", slack.AppsUninstall, "", t0.Add(time.Second)))
	l.Block("I2", slack.AppsUninstall, t0, time.Hour)
	later := t0.Add(11 * time.Minute)
	require.Zero(t, l.Take("I3", slack.ChatUpdate, "", later)) // sweeps
	require.Zero(t, l.Take("I1", slack.AppsUninstall, "", later.Add(time.Second)))
	require.Equal(t, 49*time.Minute-time.Second, l.Take("I2", slack.AppsUninstall, "", later.Add(time.Second)))
}

func ExampleLimiter() {
	limiter, _ := slack.NewLimiter(1)
	now := time.Unix(1795000000, 0)
	fmt.Println(limiter.Take("install", slack.ChatPostMessage, "C1", now))
	fmt.Println(limiter.Take("install", slack.ChatPostMessage, "C1", now))
	limiter.Block("install", slack.ChatUpdate, now, 30*time.Second)
	fmt.Println(limiter.Take("install", slack.ChatUpdate, "C1", now))
	// Output:
	// 0s
	// 1s
	// 30s
}
