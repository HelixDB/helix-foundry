package slackbot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/integration"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// cardOf is an action as its card draws it.
func cardOf(a *model.FoundryAgentAction) agent.ActionCard {
	return agent.ActionCard{ActionID: a.ID, Operation: a.Operation, Target: a.Target, Payload: a.Payload, PayloadHash: a.PayloadHash,
		Display: a.Display, TargetNamedByUser: a.TargetNamedByUser, ExpiresAt: a.ExpiresAt}
}

// cardState is how an action's card stands at now, as the store has it,
// clicked by by (a Slack user, "" when unknown); nil while a click runs it,
// which draws nothing new.
func cardState(a *model.FoundryAgentAction, by string, now time.Time) slack.CardState {
	why := ""
	if a.Error != nil {
		why = *a.Error
	}
	switch a.Shown(now) {
	case model.FoundryAgentActionPrepared:
		return slack.CardOpen{}
	case model.FoundryAgentActionExpired:
		return slack.CardExpired{}
	case model.FoundryAgentActionDismissed:
		return slack.CardDismissed{By: by}
	case model.FoundryAgentActionSucceeded:
		return slack.CardRan{By: by}
	case model.FoundryAgentActionFailed:
		return slack.CardRan{By: by, Failed: true, Error: why}
	case model.FoundryAgentActionUnknown:
		return slack.CardRan{By: by, Failed: true, Error: "Foundry lost its answer, so it may or may not have taken effect"}
	}
	return nil
}

// click handles a click (the spec's §8.9): Run, Dismiss and Prepare again
// on a card, by the action's own person; Stop on a reply, by its person;
// Unlink, of the clicker's own link. Review and Run of a bot's proposal are
// bot mode's, which no L1 install has.
func (b *Bot) click(ctx context.Context, c claim, in slack.Interaction) (model.SlackIntakeOutcome, error) {
	switch in := in.(type) {
	case slack.Approve:
		return b.cardClick(ctx, c, in.Clicked, in.ActionID, approveCall(in.PayloadHash))
	case slack.Dismiss:
		return b.cardClick(ctx, c, in.Clicked, in.ActionID, dismissCall)
	case slack.Refresh:
		return b.cardClick(ctx, c, in.Clicked, in.ActionID, refreshCall)
	case slack.Stop:
		return b.stopClick(ctx, c, in)
	case slack.Unlink:
		return b.unlink(ctx, c, in.Clicked)
	case slack.Review:
		return b.clickTell(ctx, c, in.Clicked, "Approving a bot's proposals isn't on for this Slack workspace.")
	case slack.Run:
		return b.clickTell(ctx, c, in.Clicked, "Approving a bot's proposals isn't on for this Slack workspace.")
	}
	return model.SlackIntakeEnded{}, nil
}

// clickTell tells the clicker text where they clicked, and ends the row.
func (b *Bot) clickTell(ctx context.Context, c claim, k slack.Clicked, text string) (model.SlackIntakeOutcome, error) {
	if err := b.tell(ctx, c.bot, k.ChannelID, k.ThreadTS, k.UserID, b.say(text)); err != nil {
		return nil, err
	}
	return model.SlackIntakeEnded{}, nil
}

// threadOf is the thread a clicked message is in: its own ts at top level.
func threadOf(k slack.Clicked) string {
	if k.ThreadTS != "" {
		return k.ThreadTS
	}
	return k.MessageTS
}

// cardCall is what a card's click calls as its person, and how its card then
// stands.
type cardCall func(ctx context.Context, s integration.Session, workspaceID, actionID string) (*foundryv1.FoundryAgentActionResponse, error)

func approveCall(payloadHash string) cardCall {
	return func(ctx context.Context, s integration.Session, ws, id string) (*foundryv1.FoundryAgentActionResponse, error) {
		return s.Founder.ExecuteFoundryAgentAction(ctx, &foundryv1.ExecuteFoundryAgentActionRequest{WorkspaceId: ws, Id: id, PayloadHash: payloadHash})
	}
}

func dismissCall(ctx context.Context, s integration.Session, ws, id string) (*foundryv1.FoundryAgentActionResponse, error) {
	return s.Founder.DismissFoundryAgentAction(ctx, &foundryv1.DismissFoundryAgentActionRequest{WorkspaceId: ws, Id: id})
}

func refreshCall(ctx context.Context, s integration.Session, ws, id string) (*foundryv1.FoundryAgentActionResponse, error) {
	return s.Founder.RefreshFoundryAgentAction(ctx, &foundryv1.RefreshFoundryAgentActionRequest{WorkspaceId: ws, Id: id})
}

// cardClick runs call on actionID as the clicker, who must be linked to the
// action's person, then redraws its card as the store has it: ran, by the
// clicker, with the turn the agent goes on in followed in the card's
// thread; dismissed; prepared again; or, for a click the action's state
// refused, as it stands. An expired confirmation offers Prepare again.
func (b *Bot) cardClick(ctx context.Context, c claim, k slack.Clicked, actionID string, call cardCall) (model.SlackIntakeOutcome, error) {
	notYours := "Only the person this was prepared for can act on it."
	link, err := b.db.GetFoundrySlackLink(ctx, c.install.ID, k.UserID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return b.clickTell(ctx, c, k, notYours)
	case err != nil:
		return nil, fmt.Errorf("read the link: %w", err)
	}
	action, err := b.db.GetFoundryAgentAction(ctx, c.install.WorkspaceID, actionID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return b.clickTell(ctx, c, k, "This action is gone.")
	case err != nil:
		return nil, fmt.Errorf("read the action: %w", err)
	case action.UserID != link.UserID:
		return b.clickTell(ctx, c, k, notYours)
	}
	session, done, err := b.session(ctx, c, link, k.ChannelID, threadOf(k))
	if done || err != nil {
		return model.SlackIntakeEnded{}, err
	}
	resp, callErr := call(session.Ctx, session, c.install.WorkspaceID, actionID)
	reason := reasonOf(callErr)
	switch {
	case callErr == nil, reason == "FOUNDRY_AGENT_ACTION_EXPIRED", reason == "FOUNDRY_AGENT_ACTION_NOT_PREPARED", reason == "FOUNDRY_AGENT_ACTION_CHANGED":
	case reason == "FOUNDRY_AGENT_THREAD_BUSY":
		return b.clickTell(ctx, c, k, "Founder is still answering; try again in a moment.")
	case refusedInWords(callErr):
		return b.clickTell(ctx, c, k, sentence(callErr))
	case status.Code(callErr) == codes.NotFound:
		return b.clickTell(ctx, c, k, "This action is gone.")
	default:
		if c.in.Attempts >= b.cfg.MaxAttempts {
			if _, tellErr := b.clickTell(ctx, c, k, "Foundry couldn't do that just now; try again."); tellErr != nil {
				return nil, tellErr
			}
			return model.SlackIntakeGaveUp{Error: fmt.Sprintf("act on the card: %v (gave up after %d attempts)", callErr, c.in.Attempts)}, nil
		}
		return b.retry(c.in, fmt.Errorf("act on the card: %w", callErr)), nil
	}
	if action, err = b.db.GetFoundryAgentAction(ctx, c.install.WorkspaceID, actionID); err != nil {
		return nil, fmt.Errorf("read the action again: %w", err)
	}
	state := cardState(action, k.UserID, b.now())
	if reason == "FOUNDRY_AGENT_ACTION_EXPIRED" {
		state = slack.CardExpired{}
	}
	if state != nil {
		msg, err := b.site.Card(cardOf(action), k.UserID, state)
		if err != nil {
			return model.SlackIntakeGaveUp{Error: err.Error()}, nil
		}
		if k.Ephemeral {
			err = b.tell(ctx, c.bot, k.ChannelID, k.ThreadTS, k.UserID, msg)
		} else {
			err = b.slack.UpdateMessage(ctx, c.bot, slack.Update{Channel: k.ChannelID, TS: k.MessageTS, Message: msg})
		}
		if err != nil {
			return nil, err
		}
	}
	next := resp.GetContinuation().GetId()
	if next == "" {
		// An earlier attempt ran it and held its continuation's reply, but
		// couldn't post it (a rate limit): post it now.
		if next, err = b.heldContinuation(ctx, action); err != nil {
			return nil, err
		}
	}
	if next != "" {
		if err := b.startReply(ctx, c, next, k.ChannelID, threadOf(k), k.UserID); err != nil {
			return nil, err
		}
	}
	return model.SlackIntakeEnded{}, nil
}

// heldContinuation is the turn the agent goes on from a's result in, when
// a click in Slack held its reply without posting it; "" for none.
func (b *Bot) heldContinuation(ctx context.Context, a *model.FoundryAgentAction) (string, error) {
	switch a.Status {
	case model.FoundryAgentActionSucceeded, model.FoundryAgentActionFailed, model.FoundryAgentActionUnknown:
	default:
		return "", nil
	}
	turns, err := b.db.ListFoundryAgentTurns(ctx, a.ThreadID, 5)
	if err != nil {
		return "", fmt.Errorf("read the thread's turns: %w", err)
	}
	for _, t := range turns {
		if t.Kind != model.FoundryAgentTurnActionResult || t.Input != a.ID {
			continue
		}
		switch r, err := b.db.GetFoundrySlackReply(ctx, t.ID); {
		case errors.Is(err, store.ErrNotFound):
			return "", nil
		case err != nil:
			return "", fmt.Errorf("read the continuation's reply: %w", err)
		case r.MessageTS == nil:
			return t.ID, nil
		}
	}
	return "", nil
}

// stopClick stops the turn a reply's Stop names, for the reply's person.
func (b *Bot) stopClick(ctx context.Context, c claim, in slack.Stop) (model.SlackIntakeOutcome, error) {
	r, err := b.db.GetFoundrySlackReply(ctx, in.TurnID)
	switch {
	case errors.Is(err, store.ErrNotFound) || err == nil && (r.InstallID != c.install.ID || r.Status != model.SlackReplyFollowing):
		return model.SlackIntakeEnded{}, nil
	case err != nil:
		return nil, fmt.Errorf("read the reply: %w", err)
	}
	stopped, err := b.stop(ctx, c, r, in.UserID)
	if err != nil {
		return nil, err
	}
	if !stopped {
		return b.clickTell(ctx, c, in.Clicked, "Only the person who asked can stop this.")
	}
	return model.SlackIntakeEnded{}, nil
}

// sessionStopped stops the turns Slack's stop button stopped: those
// followed in its thread whose person clicked it. Anyone else is told they
// can't, and the turn goes on.
func (b *Bot) sessionStopped(ctx context.Context, c claim, e slack.SessionStopped) (model.SlackIntakeOutcome, error) {
	follows, err := b.db.ListFoundrySlackFollows(ctx, c.install.ID, e.At.ChannelID, e.At.ThreadTS)
	if err != nil {
		return nil, fmt.Errorf("read the thread's replies: %w", err)
	}
	refused := false
	for _, r := range follows {
		stopped, err := b.stop(ctx, c, r, e.UserID)
		if err != nil {
			return nil, err
		}
		refused = refused || !stopped
	}
	if refused {
		if err := b.tell(ctx, c.bot, e.At.ChannelID, e.At.ThreadTS, e.UserID, b.say("Only the person who asked can stop Founder's answer.")); err != nil {
			return nil, err
		}
		// The answer goes on: show it working again.
		b.setAgentStatus(ctx, c.bot, e.At.ChannelID, e.At.ThreadTS, slack.AgentProcessing)
	}
	return model.SlackIntakeEnded{}, nil
}

// stop cancels r's turn as its person, when clicker is that person, linked
// to the turn's user: false, and nothing done, for anyone else. The thread's
// agent session goes back to active; the follower shows who stopped it.
func (b *Bot) stop(ctx context.Context, c claim, r *model.SlackReply, clicker string) (bool, error) {
	if r.SlackUserID != clicker {
		return false, nil
	}
	link, err := b.db.GetFoundrySlackLink(ctx, c.install.ID, clicker)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read the link: %w", err)
	}
	turn, err := b.db.GetFoundryAgentTurn(ctx, c.install.WorkspaceID, r.TurnID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("read the turn: %w", err)
	case turn.UserID != link.UserID:
		return false, nil
	}
	session, done, err := b.session(ctx, c, link, r.ChannelID, r.ThreadTS)
	if done || err != nil {
		return true, err
	}
	_, err = session.Founder.CancelFoundryAgentTurn(session.Ctx, &foundryv1.CancelFoundryAgentTurnRequest{WorkspaceId: c.install.WorkspaceID, Id: r.TurnID})
	switch status.Code(err) {
	case codes.OK, codes.FailedPrecondition, codes.NotFound:
		// Stopped, or it ended or went meanwhile.
	default:
		return false, fmt.Errorf("stop the turn: %w", err)
	}
	log.From(ctx).Info("a Slack user stopped a Founder turn", "turn", r.TurnID)
	b.setAgentStatus(ctx, c.bot, r.ChannelID, r.ThreadTS, slack.AgentActive)
	return true, nil
}

// unlink removes the clicker's own link, and says so in place of the
// message they clicked on.
func (b *Bot) unlink(ctx context.Context, c claim, k slack.Clicked) (model.SlackIntakeOutcome, error) {
	if err := b.db.DeleteFoundrySlackLink(ctx, c.install.ID, k.UserID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("unlink: %w", err)
	}
	log.From(ctx).Info("a Slack user unlinked their account", "workspace_id", c.install.WorkspaceID)
	msg := b.say("Unlinked: I no longer answer you as your Foundry account. Message me to link it again.")
	if k.Ephemeral {
		return b.clickTell(ctx, c, k, "Unlinked: I no longer answer you as your Foundry account. Message me to link it again.")
	}
	if err := b.slack.UpdateMessage(ctx, c.bot, slack.Update{Channel: k.ChannelID, TS: k.MessageTS, Message: msg}); err != nil {
		return nil, err
	}
	return model.SlackIntakeEnded{}, nil
}
