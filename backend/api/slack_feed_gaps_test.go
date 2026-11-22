package api

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// gapStore is slackStore, recording the feed gaps appended.
type gapStore struct {
	*slackStore
	appended []gapCall
	outcome  model.SlackAppendOutcome
	failGaps error
}

type gapCall struct {
	installID string
	gaps      []model.SlackFeedGap
	spacing   time.Duration
}

func (g *gapStore) AppendFoundrySlackFeedGaps(_ context.Context, installID string, gaps []model.SlackFeedGap, spacing time.Duration) (
	model.SlackFeedAppended, error) {
	if g.failGaps != nil {
		return model.SlackFeedAppended{}, g.failGaps
	}
	g.appended = append(g.appended, gapCall{installID, gaps, spacing})
	return model.SlackFeedAppended{Outcome: g.outcome, Rows: len(gaps)}, nil
}

func TestRecordFoundrySlackFeedGaps(t *testing.T) {
	minute := time.Now().UTC().Truncate(time.Minute).Add(-5 * time.Minute)
	gap := func(team, enterprise, channel string, at time.Time) *foundryv1.FoundrySlackFeedGap {
		return &foundryv1.FoundrySlackFeedGap{TeamId: team, EnterpriseId: enterprise, ChannelId: channel, Minute: timestamppb.New(at)}
	}
	setup := func(t *testing.T) (slackFixture, *gapStore, *model.SlackInstall) {
		t.Helper()
		f := newSlackFixture(t)
		f.cfg.Foundry.Slack.Feed.Enabled = true
		install := f.db.install(t, f.keys, model.SlackPerPerson{})
		db := &gapStore{slackStore: f.db, outcome: model.SlackAppended}
		f.s.deps.DB = db
		return f, db, install
	}
	record := func(t *testing.T, f slackFixture, gaps ...*foundryv1.FoundrySlackFeedGap) error {
		t.Helper()
		_, err := f.s.RecordFoundrySlackFeedGaps(door(), &foundryv1.RecordFoundrySlackFeedGapsRequest{Gaps: gaps})
		return err
	}
	taken := func(outcome string) float64 { return testutil.ToFloat64(slackFeedGapsTaken.WithLabelValues(outcome)) }

	t.Run("only the door records gaps", func(t *testing.T) {
		f, db, _ := setup(t)
		for name, ctx := range map[string]context.Context{
			"a person":          as("user-1"),
			"a sender":          ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceEventSource, EventSourceTokenID: "tok"}),
			"a door for a user": ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceEventSource, UserID: "u"}),
		} {
			_, err := f.s.RecordFoundrySlackFeedGaps(ctx, &foundryv1.RecordFoundrySlackFeedGapsRequest{Gaps: []*foundryv1.FoundrySlackFeedGap{
				gap(slackTeam, "", "C1", minute)}})
			assert.Equal(t, codes.PermissionDenied, status.Code(err), name)
		}
		assert.Empty(t, db.appended)
	})

	t.Run("refuses more gaps than a call holds", func(t *testing.T) {
		f, db, _ := setup(t)
		gaps := make([]*foundryv1.FoundrySlackFeedGap, maxSlackFeedGaps+1)
		for i := range gaps {
			gaps[i] = gap(slackTeam, "", "C1", minute)
		}
		assert.Equal(t, codes.InvalidArgument, status.Code(record(t, f, gaps...)))
		require.NoError(t, record(t, f, gaps[:maxSlackFeedGaps]...), "a full call")
		require.Len(t, db.appended, 1)
		assert.Len(t, db.appended[0].gaps, 1, "the same minute of a channel is kept once")
	})

	t.Run("records each team's gaps in one append, a minute each, at the feed's spacing", func(t *testing.T) {
		f, db, install := setup(t)
		before := taken("recorded")
		require.NoError(t, record(t, f,
			gap(slackTeam, "", "C1", minute.Add(30*time.Second)),
			gap(slackTeam, "", "", minute),
			gap(slackTeam, "", "C1", minute.Add(10*time.Second)),
			gap(slackTeam, "", "C2", minute.Add(time.Minute)),
		))
		require.Len(t, db.appended, 1)
		assert.Equal(t, install.ID, db.appended[0].installID)
		assert.Equal(t, f.cfg.Foundry.Slack.Feed.Spacing, db.appended[0].spacing)
		assert.Equal(t, []model.SlackFeedGap{
			model.SlackMinuteGap("C1", minute), model.SlackMinuteGap("", minute), model.SlackMinuteGap("C2", minute.Add(time.Minute)),
		}, db.appended[0].gaps)
		assert.Equal(t, before+3, taken("recorded"))
	})

	t.Run("finds an org-wide install by its Grid org", func(t *testing.T) {
		f, db, install := setup(t)
		install.EnterpriseID = "E0ORG"
		require.NoError(t, record(t, f, gap("T0OTHER", "E0ORG", "C1", minute)))
		require.Len(t, db.appended, 1)
		assert.Equal(t, install.ID, db.appended[0].installID)
	})

	t.Run("skips what it can't record, and records the rest", func(t *testing.T) {
		f, db, _ := setup(t)
		malformed, unknown := taken("malformed"), taken("unknown_team")
		require.NoError(t, record(t, f,
			gap("", "", "C1", minute),
			gap("T 1", "", "C1", minute),
			gap(slackTeam, "E 1", "C1", minute),
			gap(slackTeam, "", "C 1", minute),
			&foundryv1.FoundrySlackFeedGap{TeamId: slackTeam, ChannelId: "C1"},
			&foundryv1.FoundrySlackFeedGap{TeamId: slackTeam, ChannelId: "C1", Minute: &timestamppb.Timestamp{Nanos: -1}},
			gap(slackTeam, "", "C1", time.Now().Add(time.Hour)),
			gap("T0NOBODY", "", "C1", minute),
			gap("T0NOBODY", "E0NONE", "C1", minute),
			gap(slackTeam, "", "C1", minute),
		))
		assert.Equal(t, malformed+7, taken("malformed"))
		assert.Equal(t, unknown+2, taken("unknown_team"))
		require.Len(t, db.appended, 1)
		assert.Equal(t, []model.SlackFeedGap{model.SlackMinuteGap("C1", minute)}, db.appended[0].gaps)
	})

	t.Run("counts a team no feed reads, and gaps it had", func(t *testing.T) {
		for outcome, want := range map[model.SlackAppendOutcome]string{
			model.SlackAppendUnbound: "unbound", model.SlackAppendDuplicate: "duplicate",
		} {
			f, db, _ := setup(t)
			db.outcome = outcome
			before := taken(want)
			require.NoError(t, record(t, f, gap(slackTeam, "", "C1", minute)))
			assert.Equal(t, before+1, taken(want))
		}
	})

	t.Run("records nothing while the feed or Slack is off", func(t *testing.T) {
		for name, off := range map[string]func(slackFixture){
			"feed":  func(f slackFixture) { f.cfg.Foundry.Slack.Feed.Enabled = false },
			"slack": func(f slackFixture) { f.cfg.Foundry.Slack.Enabled = false },
		} {
			f, db, _ := setup(t)
			off(f)
			before := taken("disabled")
			require.NoError(t, record(t, f, gap(slackTeam, "", "C1", minute), gap(slackTeam, "", "C2", minute)), name)
			assert.Empty(t, db.appended, name)
			assert.Equal(t, before+2, taken("disabled"), name)
		}
	})

	t.Run("is unavailable when the store fails, for the door to send again", func(t *testing.T) {
		f, db, _ := setup(t)
		db.failGaps = errors.New("deadlock")
		assert.Equal(t, codes.Unavailable, status.Code(record(t, f, gap(slackTeam, "", "C1", minute))))
		db.failGaps = nil
		db.fail = errors.New("connection refused")
		assert.Equal(t, codes.Unavailable, status.Code(record(t, f, gap(slackTeam, "", "C1", minute))))
	})
}

// An event the door refused on Slack's last retry before it could verify it
// (its own address's room, or the read room) comes signed: its gap is
// recorded only once its signature verifies.
func TestRecordSignedSlackFeedGap(t *testing.T) {
	setup := func(t *testing.T) (slackFixture, *gapStore) {
		t.Helper()
		f := newSlackFixture(t)
		f.cfg.Foundry.Slack.Feed.Enabled = true
		f.db.install(t, f.keys, model.SlackPerPerson{})
		db := &gapStore{slackStore: f.db, outcome: model.SlackAppended}
		f.s.deps.DB = db
		return f, db
	}
	sign := func(body []byte) *foundryv1.FoundrySlackSignedEvent {
		req := signed(foundryv1.SlackRoute_SLACK_ROUTE_EVENTS, body)
		return &foundryv1.FoundrySlackSignedEvent{Body: req.GetBody(), Signature: req.GetSignature(), Timestamp: req.GetTimestamp()}
	}
	record := func(t *testing.T, f slackFixture, event *foundryv1.FoundrySlackSignedEvent) (foundryv1.SlackSignedGap, error) {
		t.Helper()
		resp, err := f.s.RecordFoundrySlackFeedGaps(door(), &foundryv1.RecordFoundrySlackFeedGapsRequest{Signed: event})
		return resp.GetSigned(), err
	}
	taken := func(outcome string) float64 { return testutil.ToFloat64(slackFeedGapsTaken.WithLabelValues(outcome)) }

	t.Run("records the gap of a verified channel message, at the minute it was posted", func(t *testing.T) {
		f, db := setup(t)
		got, err := record(t, f, sign(fixture(t, "events/channel_message.json")))
		require.NoError(t, err)
		assert.Equal(t, foundryv1.SlackSignedGap_SLACK_SIGNED_GAP_RECORDED, got)
		require.Len(t, db.appended, 1)
		require.Len(t, db.appended[0].gaps, 1)
		gap := db.appended[0].gaps[0]
		assert.Equal(t, "C0GEN0001", gap.ChannelID)
		assert.False(t, gap.From.After(time.Unix(1795000015, 0)), "the message's minute, not a later one: %s", gap.From)
	})

	t.Run("records nothing of an event whose signature is false", func(t *testing.T) {
		for name, event := range map[string]*foundryv1.FoundrySlackSignedEvent{
			"another secret's": func() *foundryv1.FoundrySlackSignedEvent {
				e := sign(fixture(t, "events/channel_message.json"))
				e.Signature = slack.Sign("not the app's secret", e.GetTimestamp(), e.GetBody())
				return e
			}(),
			"a body changed after signing": func() *foundryv1.FoundrySlackSignedEvent {
				e := sign(fixture(t, "events/channel_message.json"))
				e.Body = []byte(strings.Replace(string(e.GetBody()), "C0GEN0001", "C0OTHER01", 1))
				return e
			}(),
			"a stale timestamp": func() *foundryv1.FoundrySlackSignedEvent {
				body := fixture(t, "events/channel_message.json")
				ts := strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
				return &foundryv1.FoundrySlackSignedEvent{Body: body, Timestamp: ts, Signature: slack.Sign(slackSigningSecret, ts, body)}
			}(),
		} {
			f, db := setup(t)
			before := taken("unverified")
			got, err := record(t, f, event)
			require.NoError(t, err, name)
			assert.Equal(t, foundryv1.SlackSignedGap_SLACK_SIGNED_GAP_UNVERIFIED, got, name)
			assert.Empty(t, db.appended, name)
			assert.Equal(t, before+1, taken("unverified"), name)
		}
	})

	t.Run("records nothing of a verified event no feed reads", func(t *testing.T) {
		for name, event := range map[string][]byte{
			"a mention":         fixture(t, "events/mention.json"),
			"an ext-shared one": fixture(t, "events/channel_ext_shared.json"),
			"another team's":    []byte(strings.ReplaceAll(string(fixture(t, "events/channel_message.json")), slackTeam, "T0NOBODY1")),
			"another app's":     []byte(strings.Replace(string(fixture(t, "events/channel_message.json")), "A0FOUNDRY", "A0OTHER01", 1)),
			"no JSON":           []byte("payload=x"),
		} {
			f, db := setup(t)
			got, err := record(t, f, sign(event))
			require.NoError(t, err, name)
			assert.Equal(t, foundryv1.SlackSignedGap_SLACK_SIGNED_GAP_UNREAD, got, name)
			assert.Empty(t, db.appended, name)
		}
		f, db := setup(t)
		db.outcome = model.SlackAppendUnbound
		got, err := record(t, f, sign(fixture(t, "events/channel_message.json")))
		require.NoError(t, err)
		assert.Equal(t, foundryv1.SlackSignedGap_SLACK_SIGNED_GAP_UNREAD, got, "no live feed")
		f.cfg.Foundry.Slack.Feed.Enabled = false
		got, err = record(t, f, sign(fixture(t, "events/channel_message.json")))
		require.NoError(t, err)
		assert.Equal(t, foundryv1.SlackSignedGap_SLACK_SIGNED_GAP_UNREAD, got, "the feed off")
	})

	t.Run("is unavailable when the store fails, so the door counts it unrecorded", func(t *testing.T) {
		f, db := setup(t)
		db.failGaps = errors.New("deadlock")
		_, err := record(t, f, sign(fixture(t, "events/channel_message.json")))
		assert.Equal(t, codes.Unavailable, status.Code(err))
	})
}
