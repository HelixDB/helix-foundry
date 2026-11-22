package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

func (s *slackStore) AppendFoundrySlackFeed(_ context.Context, installID string, batch model.SlackFeedBatch, limits model.SlackFeedLimits) (
	model.SlackFeedAppended, error) {
	for _, r := range batch.Rows {
		if err := r.Validate(); err != nil {
			return model.SlackFeedAppended{}, err
		}
	}
	if limits.Spacing <= 0 || limits.MaxRows < 1 {
		return model.SlackFeedAppended{}, errors.New("no limits")
	}
	if s.feed.appendErr != nil {
		return model.SlackFeedAppended{}, s.feed.appendErr
	}
	s.feed.appended[installID] = append(s.feed.appended[installID], batch)
	outcome := s.feed.outcome[installID]
	if outcome == "" {
		outcome = model.SlackAppended
	}
	return model.SlackFeedAppended{Outcome: outcome, FeedID: "feed-1", Rows: len(batch.Rows)}, nil
}

func (s *slackStore) RecordFoundrySlackEventsLimited(_ context.Context, installID string, minute time.Time, _ time.Duration) (
	model.SlackFeedAppended, error) {
	if s.feed.appendErr != nil {
		return model.SlackFeedAppended{}, s.feed.appendErr
	}
	s.feed.limited = append(s.feed.limited, installID+"@"+minute.UTC().Format(time.RFC3339))
	return model.SlackFeedAppended{Outcome: model.SlackAppended}, nil
}

func (s *slackStore) ListFoundrySlackThreadMappings(_ context.Context, installID, channelID, threadTS string) ([]*model.SlackThread, error) {
	if s.feed.mapErr != nil {
		return nil, s.feed.mapErr
	}
	var out []*model.SlackThread
	for _, m := range s.feed.mappings {
		if m.InstallID == installID && m.ChannelID == channelID && m.ThreadTS == threadTS {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *slackStore) IsFoundrySlackThreadPerson(_ context.Context, threadID, slackUserID string) (bool, error) {
	if s.feed.peopleErr != nil {
		return false, s.feed.peopleErr
	}
	return slices.Contains(s.feed.people[threadID], slackUserID), nil
}

func (s *slackStore) MarkFoundrySlackThreadOthers(_ context.Context, installID, channelID, threadTS string, at time.Time) error {
	s.feed.others = append(s.feed.others, installID+" "+channelID+" "+threadTS+" "+at.UTC().Format(time.RFC3339))
	return nil
}

// feedFixture is a Slack fixture with the feed and follow-ups on, the
// fixtures' team installed, its bot B0BOT0001.
func feedFixture(t *testing.T) (slackFixture, *model.SlackInstall) {
	t.Helper()
	f := newSlackFixture(t)
	f.cfg.Foundry.Slack.Feed.Enabled, f.cfg.Foundry.Slack.Follow.Enabled = true, true
	f.cfg.Foundry.Documents.MaxBytes = 1 << 20
	i := f.db.install(t, f.keys, model.SlackPerPerson{})
	i.BotID = "B0BOT0001"
	return f, i
}

func receive(t *testing.T, f slackFixture, body []byte) *foundryv1.ReceiveSlackRequestResponse {
	t.Helper()
	resp, err := f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, body))
	require.NoError(t, err)
	return resp
}

// rows are the rows appended for the fixtures' install.
func (f slackFixture) rows() []model.SlackFeedRow {
	var out []model.SlackFeedRow
	for _, b := range f.db.feed.appended["inst-1"] {
		out = append(out, b.Rows...)
	}
	return out
}

func TestReceiveSlackFeed(t *testing.T) {
	day := func(ts string) time.Time { return model.SlackDay(slackTSTime(ts)) }
	for name, c := range map[string]struct {
		fixture string
		kind    model.SlackFeedRowKind
		channel string
		day     time.Time
		payload string
	}{
		"a message": {"channel_message", model.SlackRowMessage, "C0GEN0001", day("1795000015.000100"), `"text":"hi"`},
		"a reply, on its root's day": {"channel_reply", model.SlackRowMessage, "C0GEN0001", day("1795000000.000100"),
			`"thread_ts":"1795000000.000100"`},
		"a broadcast, on its own day": {"channel_broadcast", model.SlackRowMessage, "C0GEN0001", day("1795000021.000100"),
			`"subtype":"thread_broadcast"`},
		"a private channel's": {"channel_message_private", model.SlackRowMessage, "", time.Time{}, `"ts"`},
		"an edit":             {"message_changed", model.SlackRowChanged, "C0GEN0001", day("1795000020.000100"), `"ts":"1795000020.000100"`},
		"a delete":            {"message_deleted", model.SlackRowDeleted, "C0GEN0001", day("1795000020.000100"), `"deleted_ts":"1795000020.000100"`},
		"a root's replies":    {"message_replied", model.SlackRowReplied, "C0GEN0001", day("1795000000.000100"), `"reply_count"`},
		"a tombstone":         {"message_tombstone", model.SlackRowChanged, "", time.Time{}, `"subtype":"tombstone"`},
		"a rename":            {"channel_rename", model.SlackRowChannel, "C0GEN0001", time.Time{}, `{"channel_name":"announcements"}`},
		"a rename, said":      {"channel_name_message", model.SlackRowChannel, "", time.Time{}, `"channel_name"`},
		"an archive":          {"channel_archive", model.SlackRowChannel, "C0GEN0001", time.Time{}, `{"archived":true}`},
		"an unarchive":        {"channel_unarchive", model.SlackRowChannel, "", time.Time{}, `{"archived":false}`},
		"a deleted channel":   {"channel_deleted", model.SlackRowLeft, "C0GEN0001", time.Time{}, `{"why":"deleted"}`},
		"a channel shared":    {"channel_shared", model.SlackRowLeft, "", time.Time{}, `{"why":"shared"}`},
		"the bot removed":     {"channel_left", model.SlackRowLeft, "", time.Time{}, `{"why":"left"}`},
		"the bot leaves":      {"member_left", model.SlackRowLeft, "C0PRIV0001", time.Time{}, `{"why":"left"}`},
		"the bot joins":       {"channel_join_bot", model.SlackRowJoined, "C0GEN0001", day("1795000028.000100"), `"joined_at"`},
	} {
		f, _ := feedFixture(t)
		resp := receive(t, f, fixture(t, "events/"+c.fixture+".json"))
		assert.EqualValues(t, http.StatusOK, resp.GetHttpStatus(), name)
		rows := f.rows()
		require.Len(t, rows, 1, name)
		r := rows[0]
		assert.Equal(t, c.kind, r.Kind, name)
		assert.Equal(t, model.SlackFeedFromEvent, r.Source, name)
		if c.channel != "" {
			assert.Equal(t, c.channel, r.ChannelID, name)
		}
		if !c.day.IsZero() {
			assert.Equal(t, c.day, r.Day, name)
		}
		assert.Contains(t, string(r.Payload), c.payload, name)
		assert.NoError(t, r.Validate(), name)
		batch := f.db.feed.appended["inst-1"][0]
		assert.WithinDuration(t, time.Now(), batch.At, time.Minute, name)
	}
}

func TestReceiveSlackFeedDrops(t *testing.T) {
	t.Run("what no feed keeps", func(t *testing.T) {
		for name, fx := range map[string]string{
			"a channel shared with another organisation": "channel_ext_shared",
			"someone else joins":                         "member_joined",
		} {
			f, _ := feedFixture(t)
			assert.EqualValues(t, http.StatusOK, receive(t, f, fixture(t, "events/"+fx+".json")).GetHttpStatus(), name)
			assert.Empty(t, f.rows(), name)
		}
		f, _ := feedFixture(t)
		left := strings.Replace(string(fixture(t, "events/member_left.json")), `"user": "U0BOT0001"`, `"user": "U0ADA0001"`, 1)
		require.Contains(t, left, "U0ADA0001")
		receive(t, f, []byte(left))
		assert.Empty(t, f.rows(), "someone else leaves")
	})
	t.Run("a team with no install", func(t *testing.T) {
		f := newSlackFixture(t)
		f.cfg.Foundry.Slack.Feed.Enabled = true
		assert.EqualValues(t, http.StatusOK, receive(t, f, fixture(t, "events/channel_message.json")).GetHttpStatus())
		assert.Empty(t, f.db.feed.appended)
	})
	t.Run("off here", func(t *testing.T) {
		f, _ := feedFixture(t)
		f.cfg.Foundry.Slack.Feed.Enabled, f.cfg.Foundry.Slack.Follow.Enabled = false, false
		receive(t, f, fixture(t, "events/channel_message.json"))
		receive(t, f, fixture(t, "events/app_rate_limited.json"))
		assert.Empty(t, f.db.feed.appended)
		assert.Empty(t, f.db.feed.limited)
		f.cfg.Foundry.Slack.Follow.Enabled = true
		receive(t, f, fixture(t, "events/channel_message.json"))
		assert.Empty(t, f.db.feed.appended, "follow-ups alone keep no feed")
	})
	t.Run("an event id that isn't one", func(t *testing.T) {
		f, _ := feedFixture(t)
		body := strings.Replace(string(fixture(t, "events/channel_message.json")), `"Ev0CHANMSG1"`, `"Ev 1"`, 1)
		assert.EqualValues(t, http.StatusBadRequest, receive(t, f, []byte(body)).GetHttpStatus())
	})
}

func TestReceiveSlackFeedText(t *testing.T) {
	message := func(t *testing.T, f slackFixture, mutate func(ev map[string]any)) map[string]any {
		t.Helper()
		var body map[string]any
		require.NoError(t, json.Unmarshal(fixture(t, "events/channel_message.json"), &body))
		mutate(body["event"].(map[string]any))
		raw, _ := json.Marshal(body)
		f.db.feed.appended = map[string][]model.SlackFeedBatch{}
		receive(t, f, raw)
		rows := f.rows()
		require.Len(t, rows, 1)
		var m map[string]any
		require.NoError(t, json.Unmarshal(rows[0].Payload, &m))
		return m
	}
	f, _ := feedFixture(t)
	for name, mutate := range map[string]func(ev map[string]any){
		"by its user":      func(ev map[string]any) { ev["user"] = "U0BOT0001" },
		"by its bot id":    func(ev map[string]any) { ev["bot_id"], ev["user"] = "B0BOT0001", nil },
		"by its app":       func(ev map[string]any) { ev["app_id"] = "A0FOUNDRY" },
		"by its profile's": func(ev map[string]any) { ev["bot_profile"] = map[string]any{"app_id": "A0FOUNDRY"} },
	} {
		m := message(t, f, mutate)
		assert.Equal(t, "", m["text"], "Helix's own message keeps no text, "+name)
		assert.Equal(t, "1795000015.000100", m["ts"], name)
	}
	other := message(t, f, func(ev map[string]any) { ev["bot_id"], ev["app_id"] = "B0OTHER01", "A0OTHER" })
	assert.Equal(t, "hi", other["text"], "another bot's is kept")
	f.cfg.Foundry.Documents.MaxBytes = 2
	cut := message(t, f, func(ev map[string]any) { ev["text"] = "héllo" })
	assert.Equal(t, "h", cut["text"], "cut whole characters within the cap")
}

func TestReceiveSlackFeedStore(t *testing.T) {
	t.Run("the store failing is retried", func(t *testing.T) {
		f, _ := feedFixture(t)
		f.db.feed.appendErr = errors.New("down")
		_, err := f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, fixture(t, "events/channel_message.json")))
		assert.Equal(t, codes.Unavailable, status.Code(err))
		_, err = f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, fixture(t, "events/app_rate_limited.json")))
		assert.Equal(t, codes.Unavailable, status.Code(err))
		f.db.feed.appendErr = nil
		f.db.fail = errors.New("down")
		f.s.slackFeed.installs = nil
		_, err = f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, fixture(t, "events/channel_message.json")))
		assert.Equal(t, codes.Unavailable, status.Code(err), "the install unreadable")
	})
	t.Run("Slack's rate limit", func(t *testing.T) {
		f, _ := feedFixture(t)
		receive(t, f, fixture(t, "events/app_rate_limited.json"))
		assert.Equal(t, []string{"inst-1@2018-02-12T20:37:00Z"}, f.db.feed.limited)
	})
	t.Run("the slots are taken", func(t *testing.T) {
		f, _ := feedFixture(t)
		f.cfg.Foundry.Slack.Feed.BackendInFlight = 1
		receive(t, f, fixture(t, "events/channel_message.json"))
		f.s.slackFeed.slots <- struct{}{}
		_, err := f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, fixture(t, "events/channel_message.json")))
		assert.Equal(t, codes.Unavailable, status.Code(err))
		mention := receive(t, f, fixture(t, "events/mention.json"))
		assert.EqualValues(t, http.StatusOK, mention.GetHttpStatus(), "a mention still goes through")
		<-f.s.slackFeed.slots
		receive(t, f, fixture(t, "events/channel_message.json"))
		assert.Len(t, f.rows(), 2)
	})
	t.Run("a cached install gone: found again", func(t *testing.T) {
		f, old := feedFixture(t)
		receive(t, f, fixture(t, "events/channel_message.json"))
		now := time.Now()
		old.RevokedAt = &now
		fresh := *old
		fresh.ID, fresh.RevokedAt = "inst-2", nil
		f.db.installs = append(f.db.installs, &fresh)
		f.db.feed.outcome["inst-1"] = model.SlackAppendUnbound
		receive(t, f, fixture(t, "events/channel_reply.json"))
		require.Len(t, f.db.feed.appended["inst-2"], 1, "appended to the team's new install")
		f.db.feed.outcome["inst-2"] = model.SlackAppendUnbound
		receive(t, f, fixture(t, "events/channel_broadcast.json"))
		assert.Len(t, f.db.feed.appended["inst-2"], 2, "unbound there too: no feed reads it")
	})
	t.Run("an install found once is cached", func(t *testing.T) {
		f, _ := feedFixture(t)
		receive(t, f, fixture(t, "events/channel_message.json"))
		f.db.fail = errors.New("down")
		f.db.feed.mapErr = nil
		f.cfg.Foundry.Slack.Follow.Enabled = false
		receive(t, f, fixture(t, "events/channel_reply.json"))
		assert.Len(t, f.rows(), 2, "no read of the install")
	})
	t.Run("a Grid org's install", func(t *testing.T) {
		ctx := context.Background()
		f, i := feedFixture(t)
		i.TeamID, i.EnterpriseID = "T0OTHER01", "E0ORG0001"
		got, cached, err := f.s.slackFeed.install(ctx, f.db, slackTeam, "E0ORG0001", false)
		require.NoError(t, err)
		assert.Equal(t, "inst-1", got.ID, "the org's one install")
		assert.False(t, cached)
		_, cached, err = f.s.slackFeed.install(ctx, f.db, slackTeam, "E0ORG0001", false)
		require.NoError(t, err)
		assert.True(t, cached)
		second := *i
		second.ID, second.WorkspaceID = "inst-2", "ws-2"
		f.db.installs = append(f.db.installs, &second)
		got, _, err = f.s.slackFeed.install(ctx, f.db, slackTeam, "E0ORG0001", true)
		require.NoError(t, err)
		assert.Nil(t, got, "two of the org's: neither")
		assert.NotContains(t, f.s.slackFeed.installs, slackTeam, "a miss isn't cached")
		got, _, err = f.s.slackFeed.install(ctx, f.db, "T0NONE001", "", false)
		require.NoError(t, err)
		assert.Nil(t, got)
	})
}

func TestFollowSlackReply(t *testing.T) {
	// The reply fixture: Ada, in #general's thread 1795000000.000100, at
	// event_time 1795000000.
	at := time.Unix(1795000000, 0).UTC()
	mapping := func(user string, botAt, othersAt *time.Time) *model.SlackThread {
		return &model.SlackThread{ID: "th-" + user, InstallID: "inst-1", ChannelID: "C0GEN0001", ThreadTS: "1795000000.000100", SlackUserID: user,
			BotAt: botAt, OthersAt: othersAt}
	}
	ago := func(d time.Duration) *time.Time {
		t := at.Add(-d)
		return &t
	}
	reply := func(t *testing.T, f slackFixture, mutate func(ev map[string]any)) {
		t.Helper()
		var body map[string]any
		require.NoError(t, json.Unmarshal(fixture(t, "events/channel_reply.json"), &body))
		if mutate != nil {
			mutate(body["event"].(map[string]any))
		}
		raw, _ := json.Marshal(body)
		assert.EqualValues(t, http.StatusOK, receive(t, f, raw).GetHttpStatus())
	}
	t.Run("taken while the bot posted last apart from its person", func(t *testing.T) {
		f, _ := feedFixture(t)
		f.cfg.Foundry.Slack.Follow.Window = 24 * time.Hour
		f.db.feed.mappings = []*model.SlackThread{mapping(slackAda, ago(time.Hour), ago(2*time.Hour)), mapping("U0GRACE01", ago(time.Hour), nil)}
		reply(t, f, nil)
		require.Len(t, f.db.intake, 1)
		in := f.db.intake[0]
		assert.Equal(t, "e:Ev0REPLY001", in.DedupeKey)
		assert.Equal(t, model.SlackConversation("inst-1", "C0GEN0001", "1795000000.000100", slackAda), in.Conversation)
		assert.Equal(t, slackAda, in.SlackUserID)
		p, err := slack.ReadPayload(in.Payload)
		require.NoError(t, err)
		e, err := slack.DecodeEvent(p.Kind, p.Data)
		require.NoError(t, err)
		got, ok := e.(slack.ThreadReply)
		require.True(t, ok, "%T", e)
		assert.Equal(t, "and last quarter?", got.Text)
		assert.Empty(t, f.db.feed.others, "its person is no one else")
		assert.Len(t, f.rows(), 1, "and kept in the feed")
		reply(t, f, nil)
		assert.Len(t, f.db.intake, 1, "Slack's retry is taken once")
	})
	for name, c := range map[string]struct {
		mappings []*model.SlackThread
		mutate   func(ev map[string]any)
		others   bool
	}{
		"no mapping":          {nil, nil, false},
		"never posted":        {[]*model.SlackThread{mapping(slackAda, nil, nil)}, nil, false},
		"someone spoke after": {[]*model.SlackThread{mapping(slackAda, ago(2*time.Hour), ago(time.Hour))}, nil, false},
		"a day ago":           {[]*model.SlackThread{mapping(slackAda, ago(25*time.Hour), nil)}, nil, false},
		"mentioning it":       {[]*model.SlackThread{mapping(slackAda, ago(time.Hour), nil)}, func(ev map[string]any) { ev["text"] = "<@U0BOT0001> and?" }, false},
		"someone else":        {[]*model.SlackThread{mapping("U0GRACE01", ago(time.Hour), nil)}, nil, true},
		"another bot":         {[]*model.SlackThread{mapping(slackAda, ago(time.Hour), nil)}, func(ev map[string]any) { ev["user"], ev["bot_id"] = nil, "B0OTHER01" }, true},
		"the bot itself":      {[]*model.SlackThread{mapping(slackAda, ago(time.Hour), nil)}, func(ev map[string]any) { ev["user"] = "U0BOT0001" }, false},
		"the bot by its id":   {[]*model.SlackThread{mapping(slackAda, ago(time.Hour), nil)}, func(ev map[string]any) { ev["user"], ev["bot_id"] = nil, "B0BOT0001" }, false},
		"a thread's root":     {[]*model.SlackThread{mapping(slackAda, ago(time.Hour), nil)}, func(ev map[string]any) { delete(ev, "thread_ts") }, false},
	} {
		t.Run(name, func(t *testing.T) {
			f, _ := feedFixture(t)
			f.cfg.Foundry.Slack.Follow.Window = 24 * time.Hour
			f.db.feed.mappings = c.mappings
			reply(t, f, c.mutate)
			assert.Empty(t, f.db.intake)
			assert.Equal(t, c.others, len(f.db.feed.others) == 1, "%v", f.db.feed.others)
		})
	}
	// Bot mode: the thread's one mapping, and the people who spoke to the
	// bot in it.
	for name, c := range map[string]struct {
		mappings []*model.SlackThread
		people   []string
		taken    bool
		others   bool
	}{
		"one of its people, the bot posting last": {[]*model.SlackThread{mapping("", ago(time.Hour), ago(2*time.Hour))}, []string{"U0GRACE01", slackAda}, true,
			false},
		"someone who never spoke to the bot": {[]*model.SlackThread{mapping("", ago(time.Hour), nil)}, []string{"U0GRACE01"}, false, true},
		"someone else spoke after":           {[]*model.SlackThread{mapping("", ago(2*time.Hour), ago(time.Hour))}, []string{slackAda}, false, false},
		"a day ago":                          {[]*model.SlackThread{mapping("", ago(25*time.Hour), nil)}, []string{slackAda}, false, false},
		"only a person's mapping":            {[]*model.SlackThread{mapping(slackAda, ago(time.Hour), nil)}, nil, false, true},
	} {
		t.Run("bot mode: "+name, func(t *testing.T) {
			f, i := feedFixture(t)
			i.Mode = model.SlackSharedBot{CredentialID: "cred-1"}
			f.cfg.Foundry.Slack.Follow.Window = 24 * time.Hour
			f.db.feed.mappings = c.mappings
			f.db.feed.people = map[string][]string{"th-": c.people}
			reply(t, f, nil)
			assert.Equal(t, c.others, len(f.db.feed.others) == 1, "%v", f.db.feed.others)
			if !c.taken {
				assert.Empty(t, f.db.intake)
				return
			}
			require.Len(t, f.db.intake, 1)
			assert.Equal(t, model.SlackConversation("inst-1", "C0GEN0001", "1795000000.000100", ""), f.db.intake[0].Conversation,
				"the thread's one conversation")
			assert.Equal(t, slackAda, f.db.intake[0].SlackUserID)
		})
	}
	t.Run("bot mode: the thread's people unread is retried", func(t *testing.T) {
		f, i := feedFixture(t)
		i.Mode = model.SlackSharedBot{CredentialID: "cred-1"}
		f.db.feed.mappings = []*model.SlackThread{mapping("", ago(time.Hour), nil)}
		f.db.feed.peopleErr = errors.New("down")
		_, err := f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, fixture(t, "events/channel_reply.json")))
		assert.Equal(t, codes.Unavailable, status.Code(err))
		assert.Empty(t, f.db.intake)
	})
	t.Run("with the feed off", func(t *testing.T) {
		f, _ := feedFixture(t)
		f.cfg.Foundry.Slack.Feed.Enabled = false
		f.cfg.Foundry.Slack.Follow.Window = 24 * time.Hour
		f.db.feed.mappings = []*model.SlackThread{mapping(slackAda, ago(time.Hour), nil)}
		reply(t, f, nil)
		assert.Len(t, f.db.intake, 1)
		assert.Empty(t, f.db.feed.appended)
	})
	t.Run("the store failing is retried", func(t *testing.T) {
		f, _ := feedFixture(t)
		f.db.feed.mapErr = errors.New("down")
		_, err := f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, fixture(t, "events/channel_reply.json")))
		assert.Equal(t, codes.Unavailable, status.Code(err))
		f.db.feed.mapErr = nil
		f.cfg.Foundry.Slack.Follow.Window = 24 * time.Hour
		f.db.feed.mappings = []*model.SlackThread{mapping(slackAda, ago(time.Hour), nil)}
		f.db.intake = []*model.SlackIntake{{DedupeKey: "e:Ev0REPLY001"}}
		receive(t, f, fixture(t, "events/channel_reply.json"))
		assert.Len(t, f.db.intake, 1, "a duplicate")
		f.db.fail = errors.New("down")
		f.db.intake = nil
		_, err = f.s.ReceiveSlackRequest(door(), signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, fixture(t, "events/channel_reply.json")))
		assert.Equal(t, codes.Unavailable, status.Code(err), "the intake unwritable")
	})
}

func TestSlackTSTime(t *testing.T) {
	assert.Equal(t, time.Unix(1795000015, 100000).UTC(), slackTSTime("1795000015.000100"))
	assert.Equal(t, time.Unix(1795000015, 123456000).UTC(), slackTSTime("1795000015.1234567"))
	assert.Equal(t, time.Unix(0, 0).UTC(), slackTSTime("nope"))
}
