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

// click handles a click (the spec's §8.9 and §9.4): Run, Dismiss and
// Prepare again on a card, by the action's own person; Review, Run and
// Dismiss of a bot's proposal, by a linked owner or admin; Stop on a reply,
// by its person (in bot mode, an owner or admin too); Unlink, of the
// clicker's own link.
func (b *Bot) click(ctx context.Context, c claim, in slack.Interaction) (model.SlackIntakeOutcome, error) {
	switch in := in.(type) {
	case slack.Approve:
		return b.cardClick(ctx, c, in.Clicked, in.ActionID, approveCall(in.PayloadHash), true)
	case slack.Dismiss:
		proposal, err := b.isProposal(ctx, c, in.ActionID)
		if err != nil {
			return nil, err
		}
		if proposal {
			return b.dismissProposal(ctx, c, in)
		}
		return b.cardClick(ctx, c, in.Clicked, in.ActionID, dismissCall, false)
	case slack.Refresh:
		return b.cardClick(ctx, c, in.Clicked, in.ActionID, refreshCall, false)
	case slack.Stop:
		return b.stopClick(ctx, c, in)
	case slack.Unlink:
		return b.unlink(ctx, c, in.Clicked)
	case slack.Review:
		return b.reviewProposal(ctx, c, in)
	case slack.Run:
		return b.runProposal(ctx, c, in)
	}
	return model.SlackIntakeEnded{}, nil
}

// isProposal reports whether actionID is a bot's proposal in the install's
// workspace; one that is gone isn't.
func (b *Bot) isProposal(ctx context.Context, c claim, actionID string) (bool, error) {
	a, err := b.db.GetFoundryAgentAction(ctx, c.install.WorkspaceID, actionID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read the action: %w", err)
	}
	_, bot := a.Principal.(model.AgentBot)
	return bot, nil
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
//
// On a standalone install, where every link acts as its owner, only the
// owner's own Slack account clicks, and only while they approve changes
// from Slack. Outside a direct message it first asks Slack whether the
// channel is shared with another organisation (conversations.info), where
// nothing runs and nothing is posted; a relayed install's never is. A run (run true) of a card that runs only in
// Foundry (slack.RunsInFoundry) runs nothing: the clicker is sent to
// Foundry, and the card redrawn with its link.
//
// A run's continuation turn has its reply held before the card is redrawn:
// a retry, which the API answers NOT_PREPARED with no continuation, finds
// it held and posts it (heldContinuation), and a card Slack lost meanwhile
// doesn't keep the continuation from being followed.
func (b *Bot) cardClick(ctx context.Context, c claim, k slack.Clicked, actionID string, call cardCall, run bool) (model.SlackIntakeOutcome, error) {
	notYours := "Only the person this was prepared for can act on it."
	link, err := b.db.GetFoundrySlackLink(ctx, c.install.ID, k.UserID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return b.clickTell(ctx, c, k, notYours)
	case err != nil:
		return nil, fmt.Errorf("read the link: %w", err)
	}
	// On a standalone install every link acts as its owner: only the
	// owner's own Slack account acts on their cards, and only while they
	// approve changes from Slack (the spec's §10.5).
	if b.relayed != nil {
		if _, full := link.Access.(model.SlackFullAccess); !full {
			return b.clickTell(ctx, c, k, notYours)
		}
		approvals, err := b.relayed.SlackApprovals(ctx)
		switch {
		case err != nil:
			return nil, fmt.Errorf("read whether changes are approved from Slack: %w", err)
		case !approvals:
			return b.clickTell(ctx, c, k, "Approving changes from Slack is off for this Foundry: act on this in Foundry, "+
				"or turn Approve changes from Slack on in Settings › Slack.")
		}
	}
	action, err := b.db.GetFoundryAgentAction(ctx, c.install.WorkspaceID, actionID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return b.clickTell(ctx, c, k, "This action is gone.")
	case err != nil:
		return nil, fmt.Errorf("read the action: %w", err)
	case action.Principal != model.AgentPerson{UserID: link.UserID}:
		return b.clickTell(ctx, c, k, notYours)
	}
	if refusal, err := b.sharedChannel(ctx, c, k); refusal != "" || err != nil {
		if err != nil {
			return nil, err
		}
		return b.clickTell(ctx, c, k, refusal)
	}
	var resp *foundryv1.FoundryAgentActionResponse
	reason := ""
	if run && slack.RunsInFoundry(cardOf(action)) {
		if _, err := b.clickTell(ctx, c, k, "Review this in Foundry and run it there: Slack can't show what it changes."); err != nil {
			return nil, err
		}
	} else {
		session, done, err := b.session(ctx, c, action.Principal, k.UserID, k.ChannelID, threadOf(k))
		if done || err != nil {
			return model.SlackIntakeEnded{}, err
		}
		var callErr error
		resp, callErr = call(session.Ctx, session, c.install.WorkspaceID, actionID)
		reason = reasonOf(callErr)
		switch {
		case callErr == nil, reason == "FOUNDRY_AGENT_ACTION_EXPIRED", reason == "FOUNDRY_AGENT_ACTION_NOT_PREPARED", reason == "FOUNDRY_AGENT_ACTION_CHANGED":
		case reason == "FOUNDRY_AGENT_THREAD_BUSY":
			return b.clickTell(ctx, c, k, "Founder is still answering; try again in a moment.")
		case refusedInWords(callErr):
			return b.clickTell(ctx, c, k, sentence(callErr))
		case status.Code(callErr) == codes.NotFound:
			return b.clickTell(ctx, c, k, "This action is gone.")
		default:
			if b.lastTry(c.in) {
				if _, tellErr := b.clickTell(ctx, c, k, "Foundry couldn't do that just now; try again."); tellErr != nil {
					return nil, tellErr
				}
				return model.SlackIntakeGaveUp{Error: fmt.Sprintf("act on the card: %v (gave up after %d attempts)", callErr, c.in.Failures+1)}, nil
			}
			return b.retry(c.in, fmt.Errorf("act on the card: %w", callErr)), nil
		}
	}
	next := resp.GetContinuation().GetId()
	if next != "" {
		if _, err := b.hold(ctx, c, next, k.ChannelID, threadOf(k), k.UserID); err != nil {
			return nil, err
		}
	}
	if action, err = b.db.GetFoundryAgentAction(ctx, c.install.WorkspaceID, actionID); err != nil {
		return nil, fmt.Errorf("read the action again: %w", err)
	}
	state := cardState(action, k.UserID, b.now())
	if reason == "FOUNDRY_AGENT_ACTION_EXPIRED" {
		state = slack.CardExpired{}
	}
	if state != nil {
		msg, err := b.site.Card(cardOf(action), slack.Chat{WorkspaceID: c.install.WorkspaceID, ThreadID: action.ThreadID}, k.UserID, state)
		if err != nil {
			return model.SlackIntakeGaveUp{Error: err.Error()}, nil
		}
		if k.Ephemeral {
			err = b.tell(ctx, c.bot, k.ChannelID, k.ThreadTS, k.UserID, msg)
		} else {
			err = b.slack.UpdateMessage(ctx, c.bot, slack.Update{Channel: k.ChannelID, TS: k.MessageTS, Message: msg})
		}
		switch {
		case err != nil && slackGone(err) && next != "":
			// The card is gone, or closed to the bot: its thread may still
			// take the continuation, which startReply tries.
			log.From(ctx).Info("a ran card couldn't be redrawn; its continuation is followed", "action", action.ID, "err", err)
		case err != nil:
			return nil, err
		}
	}
	if next == "" {
		// An earlier attempt ran it and held its continuation's reply, but
		// couldn't post it (a rate limit, or a card it couldn't redraw):
		// post it now.
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

// sharedChannel is why nothing acts where k was clicked, "" for nothing:
// outside a direct message it asks Slack whether the channel is shared
// with another organisation (conversations.info), where Helix Foundry acts
// on nothing, or can't tell (an install without the scope). A relayed
// install asks too: a channel it was sent a card in may have been shared
// since, and Helix relays clicks without asking.
func (b *Bot) sharedChannel(ctx context.Context, c claim, k slack.Clicked) (string, error) {
	if inDM(k.ChannelID) {
		return "", nil
	}
	info, err := b.slack.ConversationInfo(ctx, c.bot, k.ChannelID)
	switch {
	case slack.IsCode(err, "missing_scope"):
		return "Helix Foundry needs to be added to Slack again to act in channels: an owner or admin of your Foundry " +
			"workspace can do it in Settings › Slack.", nil
	case err != nil:
		return "", fmt.Errorf("conversations.info: %w", err)
	case info.ExtShared:
		return "Helix Foundry doesn't act in channels shared with other organisations. Message me directly instead.", nil
	}
	return "", nil
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

// stopClick stops the turn a reply's Stop names, for whom stop lets.
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
		return b.clickTell(ctx, c, in.Clicked, b.onlyStoppers(c)+" can stop this.")
	}
	return model.SlackIntakeEnded{}, nil
}

// onlyStoppers is who may stop the install's turns, as a sentence's start.
func (b *Bot) onlyStoppers(c claim) string {
	if _, bot := c.install.Mode.(model.SlackSharedBot); bot {
		return "Only the person who asked, or an owner or admin of the Foundry workspace,"
	}
	return "Only the person who asked"
}

// sessionStopped stops the turns Slack's stop button stopped: those
// followed in its thread that its clicker may stop. Someone who may stop
// none there is told they can't; while anyone else's turn goes on, the
// thread shows Founder working.
func (b *Bot) sessionStopped(ctx context.Context, c claim, e slack.SessionStopped) (model.SlackIntakeOutcome, error) {
	follows, err := b.db.ListFoundrySlackFollows(ctx, c.install.ID, e.At.ChannelID, e.At.ThreadTS)
	if err != nil {
		return nil, fmt.Errorf("read the thread's replies: %w", err)
	}
	mine, others := false, false
	for _, r := range follows {
		stopped, err := b.stop(ctx, c, r, e.UserID)
		if err != nil {
			return nil, err
		}
		mine, others = mine || stopped, others || !stopped
	}
	if others && !mine {
		if err := b.tell(ctx, c.bot, e.At.ChannelID, e.At.ThreadTS, e.UserID, b.say(b.onlyStoppers(c)+" can stop Founder's answer.")); err != nil {
			return nil, err
		}
	}
	if others {
		// Someone else's answer goes on: show it working again.
		b.setAgentStatus(ctx, c.bot, e.At.ChannelID, e.At.ThreadTS, slack.AgentProcessing)
	}
	return model.SlackIntakeEnded{}, nil
}

// stop cancels r's turn as its principal, when clicker may: the reply's
// person, linked to the turn's own person, or, for the install's bot's
// turn, the person whose message it answers or a linked owner or admin of
// the workspace. false, and nothing done, for anyone else. The thread's
// agent session goes back to active; the follower shows it stopped.
func (b *Bot) stop(ctx context.Context, c claim, r *model.SlackReply, clicker string) (bool, error) {
	turn, err := b.db.GetFoundryAgentTurn(ctx, c.install.WorkspaceID, r.TurnID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return r.SlackUserID == clicker, nil // nothing left to stop
	case err != nil:
		return false, fmt.Errorf("read the turn: %w", err)
	}
	switch p := turn.Principal.(type) {
	case model.AgentPerson:
		if r.SlackUserID != clicker {
			return false, nil
		}
		link, err := b.db.GetFoundrySlackLink(ctx, c.install.ID, clicker)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return false, nil
		case err != nil:
			return false, fmt.Errorf("read the link: %w", err)
		case link.UserID != p.UserID:
			return false, nil
		}
	case model.AgentBot:
		if r.SlackUserID != clicker {
			if _, ok, err := b.admin(ctx, c, clicker); !ok || err != nil {
				return false, err
			}
		}
	default:
		return false, nil
	}
	session, done, err := b.session(ctx, c, turn.Principal, clicker, r.ChannelID, r.ThreadTS)
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
