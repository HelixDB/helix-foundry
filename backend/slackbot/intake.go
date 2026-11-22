package slackbot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/integration"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// claim is a claimed intake row as its handler reads it: the row, its
// install and the install's bot, and the turn whose reply it held (hold),
// "" for none, which its handler shares with every copy of the claim.
type claim struct {
	in      *model.SlackIntake
	owner   string
	install *model.SlackInstall
	bot     slack.Bot
	held    *string
}

// errLost is a claim whose lease was taken by another worker: it writes
// nothing more.
var errLost = errors.New("slackbot: the claim's lease was lost")

// NextIntake claims the oldest due intake row, and returns what handles it
// and ends its claim; nil when none was due. An error is the store's,
// claiming; how a row ended is counted, and logged, never returned.
func (b *Bot) NextIntake(ctx context.Context) (func(context.Context), error) {
	in, err := b.db.ClaimFoundrySlackIntake(ctx, b.cfg.Lease)
	if err != nil || in == nil {
		return nil, err
	}
	return func(ctx context.Context) { b.handleClaimed(ctx, in) }, nil
}

// handleClaimed handles one claimed row and ends its claim, as handle says.
func (b *Bot) handleClaimed(ctx context.Context, in *model.SlackIntake) {
	if in.LeaseOwner == nil {
		log.From(ctx).Error("a claimed Slack intake row has no lease owner", "intake", in.ID)
		return
	}
	kind := "unknown"
	if p, err := slack.ReadPayload(in.Payload); err == nil {
		kind = string(p.Kind)
	}
	logger := log.From(ctx).With("intake", in.ID, "install_id", in.InstallID, "kind", kind, "attempt", in.Attempts, "failures", in.Failures)
	ctx = log.Into(ctx, logger)
	held := ""
	outcome, err := b.handle(ctx, in, &held)
	if errors.Is(err, errLost) {
		intake.WithLabelValues(kind, "lost").Inc()
		logger.Warn("lost a Slack intake row's lease while handling it")
		return
	}
	label := "done"
	switch outcome.(type) {
	case model.SlackIntakeGaveUp:
		label = "failed"
	case model.SlackIntakeRetry:
		label = "retried"
	case model.SlackIntakeDeferred:
		label = "deferred"
	case model.SlackIntakeWait:
		label = "waiting"
	}
	switch err := b.db.EndFoundrySlackIntake(context.WithoutCancel(ctx), in.ID, *in.LeaseOwner, outcome); {
	case errors.Is(err, store.ErrNotFound):
		label = "lost"
	case err != nil:
		logger.Error("end a Slack intake row failed; its lease lapses and it is handled again", "err", err)
		label = "unrecorded"
	}
	if g, ok := outcome.(model.SlackIntakeGaveUp); ok {
		logger.Warn("gave up a Slack intake row", "why", g.Error)
		// A reply it held, unposted, is the follower's now, not in an hour.
		if label == "failed" && held != "" {
			if err := b.db.ReleaseFoundrySlackHold(context.WithoutCancel(ctx), held); err != nil {
				logger.Warn("release a held Slack reply failed; it is followed once its hold runs out", "turn", held, "err", err)
			}
		}
	}
	intake.WithLabelValues(kind, label).Inc()
}

// handle handles a claimed row and says how its claim ends, or errLost once
// its lease was lost; held is set to the turn whose reply it held. A row
// whose install is gone, or that the bot can't read, ends; a Slack rate
// limit defers it by its Retry-After.
func (b *Bot) handle(ctx context.Context, in *model.SlackIntake, held *string) (model.SlackIntakeOutcome, error) {
	if in.Attempts > maxClaims {
		return model.SlackIntakeGaveUp{Error: fmt.Sprintf("claimed %d times without ending", in.Attempts)}, nil
	}
	install, err := b.db.GetFoundrySlackInstallByID(ctx, in.InstallID)
	switch {
	case errors.Is(err, store.ErrNotFound) || err == nil && install.RevokedAt != nil:
		return model.SlackIntakeEnded{}, nil
	case err != nil:
		return b.retry(in, fmt.Errorf("read the install: %w", err)), nil
	}
	bot, err := b.token(ctx, install.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return model.SlackIntakeEnded{}, nil
	case err != nil:
		return b.retry(in, err), nil
	}
	p, err := slack.ReadPayload(in.Payload)
	if err != nil {
		return model.SlackIntakeGaveUp{Error: err.Error()}, nil
	}
	c := claim{in: in, owner: *in.LeaseOwner, install: install, bot: bot, held: held}
	var outcome model.SlackIntakeOutcome
	switch in.Kind {
	case model.SlackIntakeEvent:
		e, err := slack.DecodeEvent(p.Kind, p.Data)
		if err != nil {
			return model.SlackIntakeGaveUp{Error: err.Error()}, nil
		}
		outcome, err = b.event(ctx, c, e)
		if err != nil {
			return b.failed(c, err)
		}
	case model.SlackIntakeInteraction:
		click, err := slack.DecodeInteraction(p.Kind, p.Data)
		if err != nil {
			return model.SlackIntakeGaveUp{Error: err.Error()}, nil
		}
		outcome, err = b.click(ctx, c, click)
		if err != nil {
			return b.failed(c, err)
		}
	default:
		return model.SlackIntakeGaveUp{Error: fmt.Sprintf("an intake row of kind %q", in.Kind)}, nil
	}
	return outcome, nil
}

// failed is how a handler's error ends its claim: errLost stays lost; a
// rate limit defers the row, counting no failure; Slack saying the bot
// can't act there gives it up; anything else is tried again.
func (b *Bot) failed(c claim, err error) (model.SlackIntakeOutcome, error) {
	if errors.Is(err, errLost) {
		return nil, err
	}
	if wait, ok := rateLimited(err); ok {
		return model.SlackIntakeDeferred{After: wait, Error: err.Error()}, nil
	}
	if slackGone(err) {
		return model.SlackIntakeGaveUp{Error: err.Error()}, nil
	}
	return b.retry(c.in, err), nil
}

// retry tries the row again after a back-off that doubles from
// foundry.slack.retry_backoff, or gives it up once this claim's failure is
// its max_attempts-th (lastTry). Claims that waited or were deferred don't
// count.
func (b *Bot) retry(in *model.SlackIntake, err error) model.SlackIntakeOutcome {
	if b.lastTry(in) {
		return model.SlackIntakeGaveUp{Error: fmt.Sprintf("%v (gave up after %d attempts)", err, in.Failures+1)}
	}
	return model.SlackIntakeRetry{After: b.cfg.RetryBackoff << min(in.Failures, 10), Error: err.Error()}
}

// lastTry reports a claim whose failure would be the row's max_attempts-th:
// it gives the row up rather than try again.
func (b *Bot) lastTry(in *model.SlackIntake) bool { return in.Failures+1 >= b.cfg.MaxAttempts }

// event handles an event: a message (a mention, a direct message, or a
// reply the API took unmentioned, foundry.slack.follow), or a stop of an
// agent session.
// Uninstalls end in the API, as Slack sends them; one queued here ends.
func (b *Bot) event(ctx context.Context, c claim, e slack.Event) (model.SlackIntakeOutcome, error) {
	switch e := e.(type) {
	case slack.Mention:
		return b.message(ctx, c, said{at: e.At, user: e.UserID, ts: e.TS, userTeam: e.UserTeamID, text: e.Text})
	case slack.DirectMessage:
		return b.message(ctx, c, said{at: e.At, user: e.UserID, ts: e.TS, userTeam: e.UserTeamID, text: e.Text})
	case slack.ThreadReply:
		return b.message(ctx, c, said{at: e.At, user: e.UserID, ts: e.TS, userTeam: e.UserTeamID, text: e.Text, unmentioned: true})
	case slack.SessionStopped:
		return b.sessionStopped(ctx, c, e)
	}
	return model.SlackIntakeEnded{}, nil
}

// said is a message someone wrote to the bot; unmentioned is a reply in a
// thread the bot follows that doesn't mention it, which gets no link
// prompt and no word when it says nothing.
type said struct {
	at          slack.Conversation
	user, ts    string
	userTeam    string
	text        string
	unmentioned bool
}

// memberOf reports a Slack user the bot answers for install: a full member
// of its workspace (slack.User.MemberOf), who, outside an Enterprise Grid
// org, says so with the message's own team too.
func memberOf(u slack.User, install *model.SlackInstall, userTeam string) bool {
	if !u.MemberOf(install.TeamID, install.EnterpriseID) {
		return false
	}
	return install.EnterpriseID != "" || userTeam == "" || userTeam == install.TeamID
}

// message handles a mention or a direct message (the spec's §8.5): it
// checks who wrote it and where, renders its text once, then sends it to
// Founder in the Founder thread its Slack thread continues, and posts the
// reply the follower takes over. per_person, it goes as the sender's linked
// person, in their own thread (someone not linked gets a link prompt); in
// bot mode (§9.5), as the install's bot, in the Slack thread's one thread,
// for anyone in the Slack workspace within
// foundry.slack.bot.per_user_per_hour, its text saying who wrote it, and the
// sender then counts among the thread's people, whose replies reach the bot
// unmentioned.
func (b *Bot) message(ctx context.Context, c claim, m said) (model.SlackIntakeOutcome, error) {
	tell := func(text string) (model.SlackIntakeOutcome, error) {
		if err := b.tell(ctx, c.bot, m.at.ChannelID, m.at.ThreadTS, m.user, b.say(text)); err != nil {
			return nil, err
		}
		return model.SlackIntakeEnded{}, nil
	}
	if m.at.ExtShared {
		return tell("Helix Foundry doesn't answer in channels shared with other organisations. Message me directly instead.")
	}
	u, err := b.user(ctx, c.bot, m.user)
	if err != nil && !slack.IsCode(err, "user_not_found") {
		return nil, fmt.Errorf("users.info: %w", err)
	}
	if err != nil || !memberOf(u, c.install, m.userTeam) {
		return tell("Helix Foundry answers members of " + c.install.TeamName + " only.")
	}
	shared, isBot := c.install.Mode.(model.SlackSharedBot)
	input := ""
	if c.in.Input != nil {
		input = *c.in.Input
	} else {
		if isBot {
			// Counted from the row's own time, so a retry counts the same.
			n, err := b.db.CountFoundrySlackUserMessages(ctx, c.install.ID, m.user, c.in.ID, time.Hour)
			switch {
			case err != nil:
				return nil, fmt.Errorf("count the sender's messages: %w", err)
			case n >= b.cfg.Bot.PerUserPerHour && m.unmentioned:
				return model.SlackIntakeEnded{}, nil
			case n >= b.cfg.Bot.PerUserPerHour:
				return tell(fmt.Sprintf("You've sent me %d messages in the last hour, the most one person may: try again later.", n))
			}
		}
		names := map[string]string{}
		for i, mentioned := range slack.MentionedUsers(m.text) {
			if i >= maxMentionNames || mentioned == c.install.BotUserID {
				continue
			}
			if who, err := b.user(ctx, c.bot, mentioned); err == nil {
				names[mentioned] = who.Shown()
			}
		}
		text := slack.InputText(m.text, c.install.BotUserID, func(id string) string { return names[id] })
		// The bot's thread is everyone's: its text says who wrote it.
		prefix := ""
		if isBot {
			prefix = "[Slack] " + speaker(u) + " wrote:\n"
		}
		switch room := maxInputChars - utf8.RuneCountInString(prefix); {
		case text == "" && m.unmentioned:
			return model.SlackIntakeEnded{}, nil
		case text == "":
			return tell("Ask me something about your Foundry data.")
		case utf8.RuneCountInString(text) > room:
			return tell(fmt.Sprintf("That message is too long for Founder: keep it under %d characters.", room))
		}
		input = prefix + text
		switch err := b.db.SetFoundrySlackIntakeInput(ctx, c.in.ID, c.owner, input); {
		case errors.Is(err, store.ErrNotFound):
			return nil, errLost
		case err != nil:
			return nil, fmt.Errorf("keep the message's input: %w", err)
		}
	}
	// principal is whom the turn is for, person the Slack user whose
	// Founder thread the Slack thread continues ("" for the bot's).
	var principal model.FoundryAgentPrincipal = model.AgentBot(shared)
	person := ""
	if !isBot {
		link, err := b.db.GetFoundrySlackLink(ctx, c.install.ID, m.user)
		switch {
		case errors.Is(err, store.ErrNotFound) && m.unmentioned:
			return model.SlackIntakeEnded{}, nil
		case errors.Is(err, store.ErrNotFound):
			if err := b.linkPrompt(ctx, c, m.at.ChannelID, m.at.ThreadTS, m.user, linkToAsk); err != nil {
				return nil, err
			}
			return model.SlackIntakeEnded{}, nil
		case err != nil:
			return nil, fmt.Errorf("read the link: %w", err)
		}
		principal, person = model.AgentPerson{UserID: link.UserID}, m.user
	}
	session, done, err := b.session(ctx, c, principal, m.user, m.at.ChannelID, m.at.ThreadTS)
	if done || err != nil {
		return model.SlackIntakeEnded{}, err
	}
	thread, err := b.db.GetFoundrySlackThread(ctx, c.install.ID, m.at.ChannelID, m.at.ThreadTS, person)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("read the thread: %w", err)
	}
	sum := sha256.Sum256([]byte("slack\x00" + m.at.TeamID + "\x00" + m.at.ChannelID + "\x00" + m.ts))
	req := &foundryv1.SendFoundryAgentMessageRequest{
		WorkspaceId: c.install.WorkspaceID, Input: &foundryv1.SendFoundryAgentMessageRequest_Text{Text: input},
		ClientToken: hex.EncodeToString(sum[:]), Surface: foundryv1.FoundryAgentSurface_FOUNDRY_AGENT_SURFACE_SLACK,
		PageContext: &foundryv1.FoundryAgentPageContext{TimeZone: u.TZ},
	}
	if thread != nil {
		req.ThreadId = thread.AgentThreadID
	}
	resp, err := session.Founder.SendFoundryAgentMessage(session.Ctx, req)
	var turn *model.FoundryAgentTurn
	switch {
	case err == nil:
		if turn, err = b.db.GetFoundryAgentTurn(ctx, c.install.WorkspaceID, resp.GetTurn().GetId()); err != nil {
			return nil, fmt.Errorf("read the turn: %w", err)
		}
	case status.Code(err) == codes.InvalidArgument && status.Convert(err).Message() == "client_token was already used for another message":
		// An earlier attempt sent it: follow its turn.
		if turn, err = b.db.GetFoundryAgentTurnByClientToken(ctx, c.install.WorkspaceID, principal, req.ClientToken); err != nil {
			return nil, fmt.Errorf("find the message's turn: %w", err)
		}
	case reasonOf(err) == "FOUNDRY_AGENT_THREAD_BUSY":
		return b.busy(ctx, c, m, thread)
	case isBot && status.Code(err) == codes.PermissionDenied:
		// The API no longer takes the key as the install's bot.
		return tell(botOff)
	case refusedInWords(err):
		return tell(sentence(err))
	default:
		if b.lastTry(c.in) {
			if _, tellErr := tell("Founder couldn't take this message; try again."); tellErr != nil {
				return nil, tellErr
			}
			return model.SlackIntakeGaveUp{Error: fmt.Sprintf("send the message: %v (gave up after %d attempts)", err, c.in.Failures+1)}, nil
		}
		return b.retry(c.in, fmt.Errorf("send the message: %w", err)), nil
	}
	if thread == nil {
		thread = &model.SlackThread{ID: id.New(), InstallID: c.install.ID, ChannelID: m.at.ChannelID, ThreadTS: m.at.ThreadTS, SlackUserID: person,
			AgentThreadID: turn.ThreadID}
		switch err := b.db.CreateFoundrySlackThread(ctx, thread); {
		case errors.Is(err, store.ErrConflict):
			// The conversation's rows run one at a time, so no other sender
			// mapped it: a bug, and the mapping kept is the first.
			log.From(ctx).Error("a Slack thread was mapped twice; the first mapping is kept", "agent_thread", turn.ThreadID)
			if thread, err = b.db.GetFoundrySlackThread(ctx, c.install.ID, m.at.ChannelID, m.at.ThreadTS, person); err != nil {
				return nil, fmt.Errorf("read the thread mapped first: %w", err)
			}
		case err != nil:
			return nil, fmt.Errorf("map the thread: %w", err)
		}
	} else if err := b.db.TouchFoundrySlackThread(ctx, thread.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("touch the thread: %w", err)
	}
	if isBot {
		switch err := b.db.AddFoundrySlackThreadPerson(ctx, thread.ID, m.user); {
		case errors.Is(err, store.ErrNotFound):
			// Its mapping went (bot mode turned off meanwhile): no reply of
			// theirs reaches the bot unmentioned there.
		case err != nil:
			return nil, fmt.Errorf("record who spoke in the thread: %w", err)
		}
	}
	if err := b.startReply(ctx, c, turn.ID, m.at.ChannelID, m.at.ThreadTS, m.user); err != nil {
		return nil, err
	}
	return model.SlackIntakeEnded{}, nil
}

// botOff is what someone talking to the install's bot is told once its key
// no longer works: Founder refuses it, revoked, changed, or bot mode off.
const botOff = "Bot mode is off: an owner or admin can turn it on again in Settings › Slack."

// speaker is a Slack user's name as a bot's turn says who wrote it: on one
// line, without control characters, at most maxSpeakerChars.
func speaker(u slack.User) string {
	name := strings.Join(strings.FieldsFunc(u.Shown(), func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if utf8.RuneCountInString(name) > maxSpeakerChars {
		name = string([]rune(name)[:maxSpeakerChars-1]) + "…"
	}
	if name == "" {
		return u.ID
	}
	return name
}

// busy waits for a busy thread's live turn (the follower wakes the row when
// it ends), at most busyPoll at a time, until foundry.slack.busy_wait after
// the message came; then it tells its person to send it again. A thread
// busy with an action running has no turn to wait on, and is deferred.
// Neither counts as a failure.
func (b *Bot) busy(ctx context.Context, c claim, m said, thread *model.SlackThread) (model.SlackIntakeOutcome, error) {
	waited := b.now().Sub(c.in.CreatedAt)
	if waited >= b.cfg.BusyWait {
		if err := b.tell(ctx, c.bot, m.at.ChannelID, m.at.ThreadTS, m.user,
			b.say("Founder is still answering in this thread; send it again when it's done.")); err != nil {
			return nil, err
		}
		return model.SlackIntakeEnded{}, nil
	}
	wait := min(b.cfg.BusyWait-waited, busyPoll)
	if thread != nil {
		turns, err := b.db.ListFoundryAgentTurns(ctx, thread.AgentThreadID, 5)
		if err != nil {
			return nil, fmt.Errorf("read the thread's turns: %w", err)
		}
		for _, t := range turns {
			if t.Status.Live() {
				return model.SlackIntakeWait{TurnID: t.ID, For: wait}, nil
			}
		}
	}
	return model.SlackIntakeDeferred{After: wait, Error: "the thread is busy"}, nil
}

// session opens principal's Session in the install's workspace: a linked
// person's (ConfirmByCaller), or the install's bot's, a grantee session of
// its key (ConfirmByAdmin; the bot only calls Founder's API with it, so it
// holds no proposal sink). A person who is no longer a member
// (PermissionDenied) is unlinked, as slackUser, and told so; a bot whose
// key the host refuses tells slackUser bot mode is off; a workspace gone
// (NotFound) revokes the install, whose sweep tells Slack. done is true for
// these: the row ends.
func (b *Bot) session(ctx context.Context, c claim, principal model.FoundryAgentPrincipal, slackUser, channel, threadTS string) (
	integration.Session, bool, error) {
	ws := c.install.WorkspaceID
	var (
		actor   ports.Actor
		confirm integration.ConfirmMode
	)
	switch p := principal.(type) {
	case model.AgentPerson:
		actor, confirm = ports.Actor{Source: ports.SourceUser, UserID: p.UserID, WorkspaceID: ws}, integration.ConfirmByCaller{}
	case model.AgentBot:
		actor, confirm = ports.Actor{Source: ports.SourceGrantee, ServiceCredentialID: p.CredentialID, WorkspaceID: ws}, integration.ConfirmByAdmin{}
	default:
		return integration.Session{}, false, fmt.Errorf("a session for %T", principal)
	}
	s, err := b.sessions.Session(ctx, actor, confirm)
	_, isBot := principal.(model.AgentBot)
	switch status.Code(err) {
	case codes.OK:
		if s.Founder == nil {
			return s, false, errors.New("the host's sessions offer no Founder API")
		}
		return s, false, nil
	case codes.PermissionDenied:
		if isBot {
			log.From(ctx).Info("the host refused a Slack bot's key", "workspace_id", ws, "err", err)
			return s, true, b.tell(ctx, c.bot, channel, threadTS, slackUser, b.say(botOff))
		}
		if err := b.db.DeleteFoundrySlackLink(ctx, c.install.ID, slackUser); err != nil && !errors.Is(err, store.ErrNotFound) {
			return s, false, fmt.Errorf("unlink a former member: %w", err)
		}
		log.From(ctx).Info("unlinked a Slack user no longer in the workspace", "workspace_id", ws)
		return s, true, b.tell(ctx, c.bot, channel, threadTS, slackUser,
			b.say("You're no longer in the Foundry workspace "+c.install.TeamName+" is connected to, so your link was removed."))
	case codes.NotFound:
		if err := b.db.RevokeFoundrySlackInstall(ctx, ws, c.install.ID, false); err != nil && !errors.Is(err, store.ErrNotFound) {
			return s, false, fmt.Errorf("revoke the install of a workspace gone: %w", err)
		}
		log.From(ctx).Info("revoked the Slack install of a workspace gone", "workspace_id", ws)
		return s, true, nil
	}
	return s, false, fmt.Errorf("open the session: %w", err)
}

// linkToAsk and linkToApprove are what a link prompt says it is for: a
// person's messages (per_person), or an owner's or admin's approvals of the
// bot's proposals (bot mode).
const (
	linkToAsk     = "Link your Slack account to your Foundry account, and I'll answer you as you."
	linkToApprove = "Link your Slack account to your Foundry account to review and run the bot's proposals as yourself."
)

// linkPrompt sends a Slack user who isn't linked a one-time link to link
// their account, live linkTokenTTL, its token in the URL's fragment, which
// no server logs: ephemerally in a channel, in the direct message itself;
// why is what linking is for.
func (b *Bot) linkPrompt(ctx context.Context, c claim, channel, threadTS, user, why string) error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := linkTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	switch err := b.db.CreateFoundrySlackLinkToken(ctx, c.install.ID, user, hex.EncodeToString(sum[:]), linkTokenTTL); {
	case errors.Is(err, store.ErrNotFound):
		return nil // the install was revoked meanwhile
	case err != nil:
		return fmt.Errorf("record the link token: %w", err)
	}
	page, err := b.site.Page("/slack/link?workspace=" + url.QueryEscape(c.install.WorkspaceID) + "#t=" + token)
	if err != nil {
		return err
	}
	text := b.site.Text(why + " This link works once, for 10 minutes.")
	msg := slack.Message{Text: text, Blocks: []slack.Block{
		slack.Section{Text: text},
		slack.Actions{Elements: []slack.Button{{ActionID: slack.ActionLink, Text: "Link your account", URL: &page, Style: slack.StylePrimary}}},
	}}
	return b.tell(ctx, c.bot, channel, threadTS, user, msg)
}

// hold records turnID's reply in a Slack thread, if it has none, held for
// the claim c to post its first message (HoldFoundrySlackReply), and keeps
// the turn as the one c held: should c give up first, the follower posts it.
func (b *Bot) hold(ctx context.Context, c claim, turnID, channel, threadTS, slackUser string) (*model.SlackReply, error) {
	r, err := b.db.HoldFoundrySlackReply(ctx, &model.SlackReply{TurnID: turnID, InstallID: c.install.ID, ChannelID: channel, ThreadTS: threadTS,
		SlackUserID: slackUser})
	if err != nil {
		return nil, fmt.Errorf("hold the reply: %w", err)
	}
	*c.held = turnID
	return r, nil
}

// startReply makes turnID's reply in a Slack thread, if it has none, while
// the claim c holds its lease: it posts the first message ("Working…", with
// Stop), records it, and so hands the reply to a follower. A worker killed
// between the post and the record posts it once more. Then the thread's
// agent session shows Founder working.
func (b *Bot) startReply(ctx context.Context, c claim, turnID, channel, threadTS, slackUser string) error {
	r, err := b.hold(ctx, c, turnID, channel, threadTS, slackUser)
	if err != nil {
		return err
	}
	if r.MessageTS == nil {
		first := b.site.Reply(slack.Reply{TurnID: turnID})[0]
		ts, err := b.slack.PostMessage(ctx, c.bot, slack.Post{Channel: channel, ThreadTS: threadTS, Message: first})
		if err != nil {
			return err
		}
		switch err := b.db.StartFoundrySlackReply(ctx, turnID, ts, c.in.ID, c.owner); {
		case errors.Is(err, store.ErrNotFound):
			return errLost
		case err != nil:
			return fmt.Errorf("record the reply's message: %w", err)
		}
		b.posted(ctx, c.install.ID, channel, threadTS, ts)
	}
	b.setAgentStatus(ctx, c.bot, channel, threadTS, slack.AgentProcessing)
	return nil
}

// posted records that the bot posted at ts in a channel's thread, which a
// reply there needs to reach it unmentioned (foundry.slack.follow): never
// in a direct message, where every reply does. A failure only logs: the
// next reply needs a mention.
func (b *Bot) posted(ctx context.Context, installID, channel, threadTS, ts string) {
	if !b.cfg.Follow.Enabled || inDM(channel) || !model.ValidSlackTS(ts) {
		return
	}
	if err := b.db.MarkFoundrySlackThreadBot(ctx, installID, channel, threadTS, slackTime(ts)); err != nil {
		log.From(ctx).Warn("record the bot's post in a Slack thread failed", "err", err)
	}
}
