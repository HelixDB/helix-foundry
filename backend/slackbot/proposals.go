package slackbot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// proposals.go is bot mode's approvals (the spec's §9.4): the install's bot
// proposes an action with a card in its thread that names only the
// operation and who asked; an owner or admin of the workspace who linked
// their Slack account clicks Review, which draws the card as them, checked
// as them, in a message only they see; Run there runs it as them, once
// (Approver, agent.Proposals), and the thread's card says who ran it and
// whether it succeeded, never what it returned, which only they read. A
// proposal whose run approves workflow steps never runs from Slack, which
// can't show them (slack.RunsInFoundry).

// proposalMax is the most of a run's result an approver reads in Slack.
const proposalMax = 1500

// admin is the Foundry user slackUser linked, and whether they own or
// administer the install's workspace now: false, with no error, for
// someone not linked, not such a member, or where no one can approve
// (Options.People unset).
func (b *Bot) admin(ctx context.Context, c claim, slackUser string) (string, bool, error) {
	if b.people == nil {
		return "", false, nil
	}
	link, err := b.db.GetFoundrySlackLink(ctx, c.install.ID, slackUser)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("read the link: %w", err)
	}
	m, err := b.people.Member(ctx, c.install.WorkspaceID, link.UserID)
	if err != nil {
		return "", false, fmt.Errorf("read the member's role: %w", err)
	}
	return link.UserID, m.Role >= ports.RoleAdmin, nil
}

// proposalOf is the bot's proposal actionID as k's clicker may act on it,
// and the Foundry user they act as; nil when they were told why not:
//   - the install isn't in bot mode, or can't approve here (no People or
//     Proposals);
//   - the proposal is gone, or another bot's;
//   - the channel is shared with another organisation;
//   - the clicker isn't linked (they get a link prompt), or isn't an owner
//     or admin.
func (b *Bot) proposalOf(ctx context.Context, c claim, k slack.Clicked, actionID string) (*model.FoundryAgentAction, string, error) {
	tell := func(text string) (*model.FoundryAgentAction, string, error) {
		return nil, "", b.tell(ctx, c.bot, k.ChannelID, k.ThreadTS, k.UserID, b.say(text))
	}
	shared, isBot := c.install.Mode.(model.SlackSharedBot)
	switch {
	case !isBot:
		return tell("Bot mode is off for this Slack workspace, so the bot's proposals don't run.")
	case b.people == nil || b.proposals == nil:
		return tell("The bot's proposals can't be approved on this Foundry.")
	}
	a, err := b.db.GetFoundryAgentAction(ctx, c.install.WorkspaceID, actionID)
	switch {
	case errors.Is(err, store.ErrNotFound) || err == nil && a.Principal != model.AgentBot{CredentialID: shared.CredentialID}:
		return tell("This proposal is gone.")
	case err != nil:
		return nil, "", fmt.Errorf("read the proposal: %w", err)
	}
	if refusal, err := b.sharedChannel(ctx, c, k); refusal != "" || err != nil {
		if err != nil {
			return nil, "", err
		}
		return tell(refusal)
	}
	approver, ok, err := b.admin(ctx, c, k.UserID)
	switch {
	case err != nil:
		return nil, "", err
	case approver == "":
		return nil, "", b.linkPrompt(ctx, c, k.ChannelID, k.ThreadTS, k.UserID, linkToApprove)
	case !ok:
		return tell("Only an owner or admin of the Foundry workspace can review and run the bot's proposals.")
	}
	return a, approver, nil
}

// reviewProposal is Review on a proposal's card: the card drawn and checked
// as the clicker, an owner or admin, in a message only they see, with Run
// (CardReview). A proposal no longer waiting, or that their checks refuse,
// is said so, its card redrawn as it stands.
func (b *Bot) reviewProposal(ctx context.Context, c claim, in slack.Review) (model.SlackIntakeOutcome, error) {
	a, approver, err := b.proposalOf(ctx, c, in.Clicked, in.ActionID)
	if a == nil || err != nil {
		return model.SlackIntakeEnded{}, err
	}
	if a.PayloadHash != in.PayloadHash {
		return b.clickTell(ctx, c, in.Clicked, "This card isn't the proposal's: ask the bot again.")
	}
	reviewed, err := b.proposals.Review(ctx, c.install.WorkspaceID, a.ID, approver)
	if err != nil {
		return b.proposalRefused(ctx, c, in.Clicked, a, err, "review the proposal")
	}
	name, err := b.people.DisplayName(ctx, approver)
	if err != nil {
		name = "" // the card says "as you" without it
	}
	card := agent.ActionCard{ActionID: a.ID, Operation: a.Operation, Target: a.Target, Payload: a.Payload, PayloadHash: a.PayloadHash,
		Display: reviewed.Display, ExpiresAt: a.ExpiresAt}
	msg, err := b.site.Card(card, slack.Chat{WorkspaceID: c.install.WorkspaceID, ThreadID: a.ThreadID}, in.UserID,
		slack.CardReview{As: name, SpecHash: reviewed.SpecHash})
	if err != nil {
		return model.SlackIntakeGaveUp{Error: err.Error()}, nil
	}
	if err := b.tell(ctx, c.bot, in.ChannelID, in.ThreadTS, in.UserID, msg); err != nil {
		return nil, err
	}
	return model.SlackIntakeEnded{}, nil
}

// runProposal is Run on a reviewed proposal: it runs as the clicker, an
// owner or admin checked again, for the payload and workflow steps they
// reviewed, once. They read how it ended in full, in a message only they
// see, which the row keeps (its input) before it is posted, so a retry
// says it again rather than run anything; the thread's card says who ran
// it and whether it succeeded; the turn the bot goes on in, which reads
// only that, is held in the proposal's thread for the asker and handed to
// a follower (follow), a retry finding it held (heldContinuation).
func (b *Bot) runProposal(ctx context.Context, c claim, in slack.Run) (model.SlackIntakeOutcome, error) {
	a, approver, err := b.proposalOf(ctx, c, in.Clicked, in.ActionID)
	if a == nil || err != nil {
		return model.SlackIntakeEnded{}, err
	}
	said, next := "", ""
	if c.in.Input != nil {
		// An earlier attempt ran it, and kept what it said.
		said = *c.in.Input
		if next, err = b.heldContinuation(ctx, a); err != nil {
			return nil, err
		}
	} else {
		run, err := b.proposals.Run(ctx, c.install.WorkspaceID, a.ID, in.PayloadHash, in.SpecHash, approver)
		if err != nil {
			return b.proposalRefused(ctx, c, in.Clicked, a, err, "run the proposal")
		}
		a = run.Action
		if run.Continuation != nil {
			next = run.Continuation.ID
			channel, thread, asker := b.proposalThread(ctx, a, in.Clicked)
			if _, err := b.hold(ctx, c, next, channel, thread, asker); err != nil {
				return nil, err
			}
		}
		said = ranAs(run.End)
		switch err := b.db.SetFoundrySlackIntakeInput(ctx, c.in.ID, c.owner, said); {
		case errors.Is(err, store.ErrNotFound):
			return nil, errLost
		case err != nil:
			return nil, fmt.Errorf("keep how the run ended: %w", err)
		}
	}
	if err := b.tell(ctx, c.bot, in.ChannelID, in.ThreadTS, in.UserID, b.say(said)); err != nil {
		return nil, err
	}
	if err := b.redrawProposal(ctx, c, a, in.UserID, in.Clicked); err != nil {
		return nil, err
	}
	if next != "" {
		channel, thread, _ := b.proposalThread(ctx, a, in.Clicked)
		b.follow(ctx, c, next, channel, thread)
	}
	return model.SlackIntakeEnded{}, nil
}

// follow hands the reply held for turnID in a thread to a follower now,
// which posts its first message: a click posts one message at most (its
// runner's), as a channel takes one a second and a deferred click starts
// over. A failure only logs: the hold runs out, and the follower posts it
// then.
func (b *Bot) follow(ctx context.Context, c claim, turnID, channel, thread string) {
	if err := b.db.ReleaseFoundrySlackHold(ctx, turnID); err != nil {
		log.From(ctx).Warn("release a held Slack reply failed; it is followed once its hold runs out", "turn", turnID, "err", err)
	}
	b.setAgentStatus(ctx, c.bot, channel, thread, slack.AgentProcessing)
}

// ranAs is how a run ended, for its runner alone: its error, or what it
// returned (untrusted), cut to proposalMax.
func ranAs(end model.FoundryAgentActionEnd) string {
	switch end.Status {
	case model.FoundryAgentActionFailed:
		return "Ran it as you: it failed: " + strings.TrimSpace(end.Error)
	case model.FoundryAgentActionUnknown:
		return "Ran it as you, but Foundry lost its answer, so it may or may not have taken effect: check in Foundry before running it again."
	}
	text := "Ran it as you: it succeeded."
	if why := strings.TrimSpace(end.Error); why != "" {
		text = "Ran it as you: " + why
	}
	if result := strings.TrimSpace(string(end.Result)); result != "" && result != "null" && result != "{}" {
		if r := []rune(result); len(r) > proposalMax {
			result = string(r[:proposalMax-1]) + "…"
		}
		text += "\nIt returned: " + result
	}
	return text
}

// dismissProposal is Dismiss on a proposal's card: an owner's or admin's
// no, which its card then says.
func (b *Bot) dismissProposal(ctx context.Context, c claim, in slack.Dismiss) (model.SlackIntakeOutcome, error) {
	a, approver, err := b.proposalOf(ctx, c, in.Clicked, in.ActionID)
	if a == nil || err != nil {
		return model.SlackIntakeEnded{}, err
	}
	done, err := b.db.DismissFoundryAgentProposal(ctx, c.install.WorkspaceID, a.ID, approver)
	if err != nil {
		return b.proposalRefused(ctx, c, in.Clicked, a, err, "dismiss the proposal")
	}
	if err := b.redrawProposal(ctx, c, done, in.UserID, in.Clicked); err != nil {
		return nil, err
	}
	return model.SlackIntakeEnded{}, nil
}

// proposalRefused answers a review, run or dismissal of a that failed with
// err: a proposal gone, no longer waiting (its card redrawn as it stands,
// and a continuation an earlier attempt held followed), changed since it
// was reviewed, or the thread busy is said so; a refusal in words is said;
// anything else is tried again, then given up with a word.
func (b *Bot) proposalRefused(ctx context.Context, c claim, k slack.Clicked, a *model.FoundryAgentAction, err error, what string) (
	model.SlackIntakeOutcome, error) {
	var refusal model.FoundryAgentRefusal
	switch {
	case errors.Is(err, store.ErrNotFound):
		return b.clickTell(ctx, c, k, "This proposal is gone.")
	case errors.As(err, &refusal) && refusal.Reason == model.FoundryAgentChanged:
		return b.clickTell(ctx, c, k, "It changed since you reviewed it: Review it again.")
	case errors.As(err, &refusal) && refusal.Reason == model.FoundryAgentThreadBusy:
		return b.clickTell(ctx, c, k, "The bot is still answering in this thread; try again in a moment.")
	case errors.As(err, &refusal) && (refusal.Reason == model.FoundryAgentExpired || refusal.Reason == model.FoundryAgentNotPrepared):
		now, err := b.db.GetFoundryAgentAction(ctx, c.install.WorkspaceID, a.ID)
		if err != nil {
			return nil, fmt.Errorf("read the proposal again: %w", err)
		}
		by, err := b.slackUserOf(ctx, c, now.DecidedBy)
		if err != nil {
			return nil, err
		}
		if err := b.redrawProposal(ctx, c, now, by, k); err != nil {
			return nil, err
		}
		if err := b.tell(ctx, c.bot, k.ChannelID, k.ThreadTS, k.UserID, b.already(now, by)); err != nil {
			return nil, err
		}
		next, err := b.heldContinuation(ctx, now)
		if next == "" || err != nil {
			return model.SlackIntakeEnded{}, err
		}
		channel, thread, _ := b.proposalThread(ctx, now, k)
		b.follow(ctx, c, next, channel, thread)
		return model.SlackIntakeEnded{}, nil
	case refusedInWords(err):
		return b.clickTell(ctx, c, k, sentence(err))
	case b.lastTry(c.in):
		if _, tellErr := b.clickTell(ctx, c, k, "Foundry couldn't do that just now; try again."); tellErr != nil {
			return nil, tellErr
		}
		return model.SlackIntakeGaveUp{Error: fmt.Sprintf("%s: %v (gave up after %d attempts)", what, err, c.in.Failures+1)}, nil
	}
	return b.retry(c.in, fmt.Errorf("%s: %w", what, err)), nil
}

// already says how a proposal no longer waiting stands, by by (a Slack
// user id, "" when unknown).
func (b *Bot) already(a *model.FoundryAgentAction, by string) slack.Message {
	who := slack.Mrkdwn{}
	if by != "" {
		who = slack.Join(b.site.Text(" by "), slack.UserMention(by))
	}
	var m slack.Mrkdwn
	switch cardState(a, by, b.now()).(type) {
	case slack.CardRan:
		m = slack.Join(b.site.Text("It was already run"), who, b.site.Text("."))
	case slack.CardDismissed:
		m = slack.Join(b.site.Text("It was already dismissed"), who, b.site.Text("."))
	case slack.CardExpired:
		m = b.site.Text("This proposal expired: ask the bot again.")
	default:
		m = slack.Join(b.site.Text("It's being run"), who, b.site.Text(" now."))
	}
	return slack.Message{Text: m, Blocks: []slack.Block{slack.Section{Text: m}}}
}

// slackUserOf is the Slack user of the install who linked the Foundry user
// userID (nil or unknown: "").
func (b *Bot) slackUserOf(ctx context.Context, c claim, userID *string) (string, error) {
	if userID == nil {
		return "", nil
	}
	links, err := b.db.ListFoundrySlackLinks(ctx, c.install.ID)
	if err != nil {
		return "", fmt.Errorf("read the install's links: %w", err)
	}
	for _, l := range links {
		if l.UserID == *userID {
			return l.SlackUserID, nil
		}
	}
	return "", nil
}

// proposalThread is the Slack thread a's card was posted in, and who asked
// for it: the reply of the turn that proposed it, else where k was clicked,
// by its clicker.
func (b *Bot) proposalThread(ctx context.Context, a *model.FoundryAgentAction, k slack.Clicked) (channel, thread, asker string) {
	r, err := b.db.GetFoundrySlackReply(ctx, a.TurnID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.From(ctx).Warn("read a proposal's reply failed; its thread is taken from the click", "action", a.ID, "err", err)
		}
		return k.ChannelID, threadOf(k), k.UserID
	}
	return r.ChannelID, r.ThreadTS, r.SlackUserID
}

// redrawProposal draws a's card in its thread as it stands, decided by by
// (a Slack user id, "" when unknown): the message its reply posted it as,
// else k's own when k isn't ephemeral. One not posted, or that Slack no
// longer lets the bot change, is left.
func (b *Bot) redrawProposal(ctx context.Context, c claim, a *model.FoundryAgentAction, by string, k slack.Clicked) error {
	state := cardState(a, by, b.now())
	if state == nil {
		return nil // a run goes on; it redraws once it ends
	}
	channel, ts, asker := k.ChannelID, "", ""
	if !k.Ephemeral {
		ts = k.MessageTS
	}
	switch r, err := b.db.GetFoundrySlackReply(ctx, a.TurnID); {
	case err == nil:
		asker = r.SlackUserID
		if posted := r.Posted["a:"+a.ID]; model.ValidSlackTS(posted) {
			channel, ts = r.ChannelID, posted
		}
	case !errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("read the proposal's reply: %w", err)
	}
	if ts == "" {
		return nil
	}
	msg, err := b.site.Proposal(slack.Proposal{ActionID: a.ID, Operation: a.Operation, PayloadHash: a.PayloadHash}, asker, state)
	if err != nil {
		log.From(ctx).Warn("a proposal's card can't be drawn", "action", a.ID, "err", err)
		return nil
	}
	switch err := b.slack.UpdateMessage(ctx, c.bot, slack.Update{Channel: channel, TS: ts, Message: msg}); {
	case err != nil && slackGone(err):
		log.From(ctx).Info("a proposal's card couldn't be redrawn", "action", a.ID, "err", err)
	case err != nil:
		return err
	}
	return nil
}
