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
		"duplicate, unbound (no live feed reads the team), unknown_team, malformed or disabled (foundry.slack.feed off).",
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
func (s *Service) RecordFoundrySlackFeedGaps(ctx context.Context, req *foundryv1.RecordFoundrySlackFeedGapsRequest) (
	*foundryv1.RecordFoundrySlackFeedGapsResponse, error) {
	a := actor(ctx)
	if a.Source != ports.SourceEventSource || a.EventSourceTokenID != "" || a.WorkspaceID != "" || a.UserID != "" || a.ServiceCredentialID != "" {
		return nil, status.Error(codes.PermissionDenied, "only Foundry's door records Slack's dropped events")
	}
	if n := len(req.GetGaps()); n > maxSlackFeedGaps {
		return nil, status.Errorf(codes.InvalidArgument, "%d gaps; send at most %d a call", n, maxSlackFeedGaps)
	}
	cfg := s.deps.Cfg.Foundry.Slack
	if !cfg.Enabled || !cfg.Feed.Enabled {
		slackFeedGapsTaken.WithLabelValues("disabled").Add(float64(len(req.GetGaps())))
		return &foundryv1.RecordFoundrySlackFeedGapsResponse{}, nil
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
		install, err := s.deps.DB.GetFoundrySlackInstallByTeam(ctx, t.id)
		if errors.Is(err, foundrystore.ErrNotFound) && t.enterprise != "" {
			// An org-wide install's channels are named by the org's
			// workspace the event came from, as a click's are.
			install, err = s.deps.DB.GetFoundrySlackInstallByEnterprise(ctx, t.enterprise)
		}
		switch {
		case errors.Is(err, foundrystore.ErrNotFound) || errors.Is(err, foundrystore.ErrConflict):
			slackFeedGapsTaken.WithLabelValues("unknown_team").Add(float64(len(gaps[t])))
			continue
		case err != nil:
			log.From(ctx).Error("find the install of a Slack team's feed gaps failed", "err", err)
			return nil, status.Error(codes.Unavailable, "Foundry couldn't record the gaps just now")
		}
		appended, err := s.deps.DB.AppendFoundrySlackFeedGaps(ctx, install.ID, gaps[t], cfg.Feed.Spacing)
		if err != nil {
			log.From(ctx).Error("record a Slack team's feed gaps failed", "err", err)
			return nil, status.Error(codes.Unavailable, "Foundry couldn't record the gaps just now")
		}
		outcome := "recorded"
		switch appended.Outcome {
		case model.SlackAppendUnbound:
			outcome = "unbound"
		case model.SlackAppendDuplicate:
			outcome = "duplicate"
		}
		slackFeedGapsTaken.WithLabelValues(outcome).Add(float64(len(gaps[t])))
	}
	return &foundryv1.RecordFoundrySlackFeedGapsResponse{}, nil
}
