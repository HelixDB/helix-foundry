package foundryevents

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryapi "github.com/helixdb/helix-foundry/backend/api"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
)

const (
	// maxFeedGaps bounds the gaps a pod holds between flushes; past it a new one is lost, and
	// counted.
	maxFeedGaps = 10_000
	// gapBatch is how many gaps one RecordFoundrySlackFeedGaps carries: as many as the backend
	// takes in a call.
	gapBatch = 1000
	// gapFlushEvery is how often a pod sends the gaps it holds.
	gapFlushEvery = time.Minute
	// gapFlushFor bounds one RecordFoundrySlackFeedGaps.
	gapFlushFor = 10 * time.Second
)

// slackID is a Slack id's form: a team, Grid org or channel.
var slackID = regexp.MustCompile(`^[A-Z0-9]{1,32}$`)

// feedGap is a minute of a channel's events Slack drops: its install's team and Grid org ("" outside
// one), its channel ("" for every channel the feed reads), and the minute's start in Unix seconds.
type feedGap struct {
	team, enterprise, channel string
	minute                    int64
}

// feedGapOf is the gap of an event to the events URL taken at now, read from its body unverified:
// its team and Grid org as slack.ParseEvent reads them (authorizations' first, else the context's,
// else the envelope's), its channel (an id, or an object's id, as channel_rename sends it), and the
// minute of its event_time, else of now. False for a body that isn't JSON or names no team of an
// id's form, or an event of a channel shared with another organisation, which no feed reads; a
// channel or Grid org of no id's form is left out.
func feedGapOf(body []byte, now time.Time) (feedGap, bool) {
	var req struct {
		TeamID              string `json:"team_id"`
		EnterpriseID        string `json:"enterprise_id"`
		ContextTeamID       string `json:"context_team_id"`
		ContextEnterpriseID string `json:"context_enterprise_id"`
		EventTime           int64  `json:"event_time"`
		ExtShared           bool   `json:"is_ext_shared_channel"`
		Authorizations      []struct {
			TeamID       string `json:"team_id"`
			EnterpriseID string `json:"enterprise_id"`
		} `json:"authorizations"`
		Event struct {
			Channel json.RawMessage `json:"channel"`
		} `json:"event"`
	}
	if json.Unmarshal(body, &req) != nil || req.ExtShared {
		return feedGap{}, false
	}
	team, enterprise := req.TeamID, req.EnterpriseID
	switch {
	case len(req.Authorizations) > 0 && req.Authorizations[0].TeamID != "":
		team, enterprise = req.Authorizations[0].TeamID, req.Authorizations[0].EnterpriseID
	case req.ContextTeamID != "":
		team, enterprise = req.ContextTeamID, req.ContextEnterpriseID
	}
	if enterprise == "" {
		enterprise = req.EnterpriseID
	}
	if !slackID.MatchString(team) {
		return feedGap{}, false
	}
	if !slackID.MatchString(enterprise) {
		enterprise = ""
	}
	var channel struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(req.Event.Channel, &channel.ID) != nil {
		_ = json.Unmarshal(req.Event.Channel, &channel) // an object, or nothing to read: no channel either way
	}
	if !slackID.MatchString(channel.ID) {
		channel.ID = ""
	}
	at := now
	if req.EventTime > 0 {
		at = time.Unix(req.EventTime, 0)
	}
	return feedGap{team: team, enterprise: enterprise, channel: channel.ID, minute: at.Unix() - at.Unix()%60}, true
}

// feedGaps are the gaps a pod holds until it flushes them: a set, at most maxFeedGaps.
type feedGaps struct {
	mu   sync.Mutex
	held map[feedGap]struct{}
}

// add holds gaps, each once. It returns how many it couldn't hold, the set being full.
func (g *feedGaps) add(gaps ...feedGap) (lost int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, gap := range gaps {
		if _, ok := g.held[gap]; !ok && len(g.held) >= maxFeedGaps {
			lost++
			continue
		}
		g.held[gap] = struct{}{}
	}
	return lost
}

// take is every gap held, in order (team, Grid org, channel, minute), holding none after.
func (g *feedGaps) take() []feedGap {
	g.mu.Lock()
	defer g.mu.Unlock()
	gaps := slices.SortedFunc(maps.Keys(g.held), func(a, b feedGap) int {
		return cmp.Or(cmp.Compare(a.team, b.team), cmp.Compare(a.enterprise, b.enterprise), cmp.Compare(a.channel, b.channel),
			cmp.Compare(a.minute, b.minute))
	})
	clear(g.held)
	return gaps
}

// holdGap holds the gap of an event's body that Slack is about to drop, taken at now. It returns
// the request's outcome: gap_recorded once held, or gap_lost when the body names no team or the
// gaps are full.
func (s *slackDoor) holdGap(body []byte, now time.Time) string {
	gap, ok := feedGapOf(body, now)
	if !ok || s.gaps.add(gap) > 0 {
		feedGapsTotal.WithLabelValues("lost").Inc()
		return "gap_lost"
	}
	feedGapsTotal.WithLabelValues("held").Inc()
	return "gap_recorded"
}

// flushGaps sends the gaps held to the backend, gapBatch a call, as the door. The gaps of a call
// the backend fails are held again, with those it hasn't sent, for the next flush, and the flush
// stops; but a call the backend refuses for good (an argument, the door's identity, a backend
// without the method) is lost, as no later flush would take it. Past the cap, what can't be held
// again is lost. Each is counted.
func (s *slackDoor) flushGaps(ctx context.Context) {
	gaps := s.gaps.take()
	for len(gaps) > 0 {
		batch := gaps[:min(len(gaps), gapBatch)]
		req := &foundryv1.RecordFoundrySlackFeedGapsRequest{Gaps: make([]*foundryv1.FoundrySlackFeedGap, len(batch))}
		for i, g := range batch {
			req.Gaps[i] = &foundryv1.FoundrySlackFeedGap{
				TeamId: g.team, EnterpriseId: g.enterprise, ChannelId: g.channel, Minute: timestamppb.New(time.Unix(g.minute, 0)),
			}
		}
		call, cancel := context.WithTimeout(foundryapi.AttachActor(ctx, ports.Actor{Source: ports.SourceEventSource}), gapFlushFor)
		_, err := s.backend.RecordFoundrySlackFeedGaps(call, req)
		cancel()
		switch status.Code(err) {
		case codes.OK:
			feedGapsTotal.WithLabelValues("flushed").Add(float64(len(batch)))
		case codes.InvalidArgument, codes.PermissionDenied, codes.Unimplemented:
			feedGapsTotal.WithLabelValues("lost").Add(float64(len(batch)))
			s.logger.Error("foundry slack gaps refused", "gaps", len(batch), "err", err)
		default:
			lost := s.gaps.add(gaps...)
			feedGapsTotal.WithLabelValues("retried").Add(float64(len(gaps) - lost))
			feedGapsTotal.WithLabelValues("lost").Add(float64(lost))
			s.logger.Warn("foundry slack gaps not sent; held for the next flush", "gaps", len(gaps), "lost", lost, "err", err)
			return
		}
		gaps = gaps[len(batch):]
	}
}

// flushGapsEvery flushes the gaps held every period until ctx ends.
func (s *slackDoor) flushGapsEvery(ctx context.Context, period time.Duration) {
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.flushGaps(ctx)
		}
	}
}
