package api

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/slack"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// maxSlackFeedGaps bounds the gaps of one RecordFoundrySlackFeedGaps: the
// door sends at most this many a call.
const maxSlackFeedGaps = 1000

var slackFeedGapsTaken = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_slack_feed_gaps_taken_total",
	Help: "Minutes of channels whose Slack events the door answered without the API taking them, by outcome: recorded, " +
		"duplicate, unbound (no live feed reads the team), unknown_team, malformed, disabled (foundry.slack.feed off), " +
		"unverified (a signed event whose signature is false) or unread (a signed event no feed reads).",
}, []string{"outcome"})

// RecordFoundrySlackFeedGaps records the minutes of channels whose events
// the door answered on Slack's last retry without this API taking them, as
// gaps of the live feed reading each one's team (store's
// AppendFoundrySlackFeedGaps), which the backfill reads again. Only the
// door calls it. The door read them from bodies no one verified, so a gap
// of no form, of a minute ahead of now, or of a team with no live install
// is skipped and counted, never refused: a refusal would hold the door's
// other gaps back. Wherever foundry.slack.feed is off nothing is recorded.
// A store failure is UNAVAILABLE, and the door sends them all again: a gap
// recorded twice is kept once.
//
// A signed event, which the door couldn't verify, is recorded only once its
// signature is (recordSignedSlackGap), and the answer says what became of
// it.
func (s *Service) RecordFoundrySlackFeedGaps(ctx context.Context, req *foundryv1.RecordFoundrySlackFeedGapsRequest) (
	*foundryv1.RecordFoundrySlackFeedGapsResponse, error) {
	a := actor(ctx)
	if a.Source != ports.SourceEventSource || a.EventSourceTokenID != "" || a.WorkspaceID != "" || a.UserID != "" || a.ServiceCredentialID != "" {
		return nil, status.Error(codes.PermissionDenied, "only Foundry's door records Slack's dropped events")
	}
	if n := len(req.GetGaps()); n > maxSlackFeedGaps {
		return nil, status.Errorf(codes.InvalidArgument, "%d gaps; send at most %d a call", n, maxSlackFeedGaps)
	}
	resp := &foundryv1.RecordFoundrySlackFeedGapsResponse{}
	if signed := req.GetSigned(); signed != nil {
		var err error
		if resp.Signed, err = s.recordSignedSlackGap(ctx, signed); err != nil {
			return nil, err
		}
	}
	cfg := s.deps.Cfg.Foundry.Slack
	if !cfg.Enabled || !cfg.Feed.Enabled {
		slackFeedGapsTaken.WithLabelValues("disabled").Add(float64(len(req.GetGaps())))
		return resp, nil
	}
	// Each team's gaps, in the order its first came, deduplicated: the store
	// takes a team's at once, in one transaction.
	type team struct{ id, enterprise string }
	var teams []team
	gaps := map[team][]model.SlackFeedGap{}
	latest := time.Now().Add(slack.MaxSkew)
	for _, g := range req.GetGaps() {
		t := team{g.GetTeamId(), g.GetEnterpriseId()}
		minute := g.GetMinute()
		if !model.ValidSlackID(t.id) || (t.enterprise != "" && !model.ValidSlackID(t.enterprise)) ||
			minute.CheckValid() != nil || minute.AsTime().After(latest) {
			slackFeedGapsTaken.WithLabelValues("malformed").Inc()
			continue
		}
		gap := model.SlackMinuteGap(g.GetChannelId(), minute.AsTime())
		if gap.Validate() != nil {
			slackFeedGapsTaken.WithLabelValues("malformed").Inc()
			continue
		}
		known, seen := gaps[t]
		if !seen {
			teams = append(teams, t)
		}
		if !slices.Contains(known, gap) {
			gaps[t] = append(known, gap)
		}
	}
	for _, t := range teams {
		install, err := s.slackGapInstall(ctx, t.id, t.enterprise)
		if err != nil {
			return nil, err
		}
		if install == nil {
			slackFeedGapsTaken.WithLabelValues("unknown_team").Add(float64(len(gaps[t])))
			continue
		}
		outcome, err := s.appendSlackGaps(ctx, install, gaps[t], nil)
		if err != nil {
			return nil, err
		}
		slackFeedGapsTaken.WithLabelValues(outcome).Add(float64(len(gaps[t])))
	}
	return resp, nil
}

// recordSignedSlackGap records the gaps of a request to the events URL the
// door refused on Slack's last retry, once its signature verifies with the
// app's signing secret: a minute of the channel of each row its feed would
// have kept of it (slackFeedRows, at the minute its message was posted, but
// for a message past any window), or of every channel for app_rate_limited.
// A store failure is UNAVAILABLE.
func (s *Service) recordSignedSlackGap(ctx context.Context, signed *foundryv1.FoundrySlackSignedEvent) (foundryv1.SlackSignedGap, error) {
	cfg := s.deps.Cfg.Foundry.Slack
	unread := func() (foundryv1.SlackSignedGap, error) {
		slackFeedGapsTaken.WithLabelValues("unread").Inc()
		return foundryv1.SlackSignedGap_SLACK_SIGNED_GAP_UNREAD, nil
	}
	if !cfg.Enabled {
		return unread()
	}
	now := time.Now()
	if slack.Verify(cfg.SigningSecret, signed.GetTimestamp(), signed.GetBody(), signed.GetSignature(), now) != nil {
		slackFeedGapsTaken.WithLabelValues("unverified").Inc()
		return foundryv1.SlackSignedGap_SLACK_SIGNED_GAP_UNVERIFIED, nil
	}
	if !cfg.Feed.Enabled {
		return unread()
	}
	cb, err := slack.ParseEvent(signed.GetBody())
	if err != nil || cb.AppID != "" && cb.AppID != cfg.AppID {
		return unread()
	}
	e, ok := cb.Event.(slack.FeedEvent)
	if !ok {
		return unread()
	}
	team, enterprise, ext := slackFeedPlace(e)
	if ext {
		return unread()
	}
	install, err := s.slackGapInstall(ctx, team, enterprise)
	switch {
	case err != nil:
		return foundryv1.SlackSignedGap_SLACK_SIGNED_GAP_UNSPECIFIED, err
	case install == nil:
		return unread()
	}
	var gaps []model.SlackFeedGap
	var channels []string
	if limited, ok := e.(slack.RateLimited); ok {
		gaps = []model.SlackFeedGap{model.SlackMinuteGap("", limited.Minute)}
	}
	for _, r := range slackFeedRows(e, cb.EventID, install, cfg.AppID, s.deps.Cfg.Foundry.Documents.MaxBytes) {
		if r.Kind != model.SlackRowLeft {
			channels = append(channels, r.ChannelID)
		}
		minute := r.Minute(now)
		if minute.Before(now.AddDate(0, 0, -model.SlackAppHistoryDaysMax-1)) {
			continue // an edit of a message past any window: no day to read back
		}
		if gap := model.SlackMinuteGap(r.ChannelID, minute); !slices.Contains(gaps, gap) {
			gaps = append(gaps, gap)
		}
	}
	if len(gaps) == 0 {
		return unread()
	}
	outcome, err := s.appendSlackGaps(ctx, install, gaps, channels)
	if err != nil {
		return foundryv1.SlackSignedGap_SLACK_SIGNED_GAP_UNSPECIFIED, err
	}
	slackFeedGapsTaken.WithLabelValues(outcome).Add(float64(len(gaps)))
	if outcome == "unbound" {
		return foundryv1.SlackSignedGap_SLACK_SIGNED_GAP_UNREAD, nil
	}
	return foundryv1.SlackSignedGap_SLACK_SIGNED_GAP_RECORDED, nil
}

// slackGapInstall is the live install whose feed takes the gaps of a Slack
// team: the team's own, else its Grid org's (an org-wide install's channels
// are named by the org's workspace the event came from, as a click's are);
// nil when none. A store failure is UNAVAILABLE.
func (s *Service) slackGapInstall(ctx context.Context, team, enterprise string) (*model.SlackInstall, error) {
	install, err := s.deps.DB.GetFoundrySlackInstallByTeam(ctx, team)
	if errors.Is(err, foundrystore.ErrNotFound) && enterprise != "" {
		install, err = s.deps.DB.GetFoundrySlackInstallByEnterprise(ctx, enterprise)
	}
	switch {
	case errors.Is(err, foundrystore.ErrNotFound) || errors.Is(err, foundrystore.ErrConflict):
		return nil, nil
	case err != nil:
		log.From(ctx).Error("find the install of a Slack team's feed gaps failed", "err", err)
		return nil, status.Error(codes.Unavailable, "Foundry couldn't record the gaps just now")
	}
	return install, nil
}

// appendSlackGaps records gaps for install's feed, or, for a relayed
// install, its local install's to read again, and names the outcome:
// recorded, duplicate, or unbound (no live feed reads it). seen are channels
// verified events named, which a relayed install's calls may name from now,
// so their gaps are held. A store failure is UNAVAILABLE.
func (s *Service) appendSlackGaps(ctx context.Context, install *model.SlackInstall, gaps []model.SlackFeedGap, seen []string) (string, error) {
	cfg := s.deps.Cfg.Foundry.Slack
	var appended model.SlackFeedAppended
	var err error
	if _, relayed := install.Destination.(model.SlackServesRelay); relayed {
		if !cfg.Relay.Enabled || !install.FeedOn {
			return string(model.SlackAppendUnbound), nil
		}
		if len(seen) > 0 {
			if err = s.deps.DB.RecordFoundryRelayChannels(ctx, install.ID, seen); errors.Is(err, foundrystore.ErrNotFound) {
				return string(model.SlackAppendUnbound), nil
			}
		}
		if err == nil {
			var held model.RelayFeedAppended
			held, err = s.deps.DB.AppendFoundryRelayFeed(ctx, install.ID, model.RelayFeedAppend{Gaps: gaps, At: time.Now()},
				model.RelayFeedLimits{TTL: cfg.Relay.FeedTTL, MaxRows: int64(cfg.Relay.FeedMaxRows)})
			appended.Outcome = held.Outcome
		}
	} else {
		appended, err = s.deps.DB.AppendFoundrySlackFeedGaps(ctx, install.ID, gaps, cfg.Feed.Spacing)
	}
	if err != nil {
		log.From(ctx).Error("record a Slack team's feed gaps failed", "err", err)
		return "", status.Error(codes.Unavailable, "Foundry couldn't record the gaps just now")
	}
	switch appended.Outcome {
	case model.SlackAppendUnbound:
		return "unbound", nil
	case model.SlackAppendDuplicate:
		return "duplicate", nil
	}
	return "recorded", nil
}
