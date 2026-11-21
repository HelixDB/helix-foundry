package slack

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/agentquestion"
)

// render.go draws a Founder turn in posted mode: its reply (what the model
// said, and a status line with Stop while it runs), folded from the turn's
// events, and the questions it asks its person. Everything a model, a tool
// or a document wrote goes through Site.Text.

// MaxReplyBlocks is the most blocks of a reply's text one message holds,
// leaving room under MaxBlocks for its status line, Stop and how it ended.
const MaxReplyBlocks = 45

// Reply is a turn's reply as its events so far make it (Apply), which
// Site.Reply draws. Its zero value, with the turn's id, is a reply before
// any event.
type Reply struct {
	TurnID string
	// Said is what the model said, in order, each Said event's text.
	Said []string
	// Status is what the turn is doing: "Thinking…", or the title of the
	// tool it called last.
	Status string
	// End is how the turn ended; nil while it is live.
	End Ending
}

// Ending is how a reply's turn ended: a closed set.
type Ending interface{ ending() }

// Finished is a turn that ended with its Finished event.
type Finished struct{ agent.Finished }

// Stopped is a turn stopped before it finished: by a Slack user (By, a
// Slack user id) or elsewhere (By "").
type Stopped struct{ By string }

func (Finished) ending() {}
func (Stopped) ending()  {}

// Due is something a reply's event has the bot post once, apart from the
// reply, under its Key: a closed set.
type Due interface {
	// Key is what the reply records it posted under: "q:" and the
	// question's id, "a:" and the action's, "k:" and the turn's.
	Key() string
}

// AnswerDue is an Analyst question an ask or get_question returned: its
// answer, once the question has ended (Site.Answer).
type AnswerDue struct{ Question agent.QuestionRef }

// CardDue is an action the turn prepared (Site.Card).
type CardDue struct{ Card agent.ActionCard }

// QuestionsDue is the questions the turn asks its person (Site.Questions).
type QuestionsDue struct {
	TurnID string
	Asked  agent.Asked
}

// Key is "q:" and the question's id.
func (d AnswerDue) Key() string { return "q:" + d.Question.ID }

// Key is "a:" and the action's id.
func (d CardDue) Key() string { return "a:" + d.Card.ActionID }

// Key is "k:" and the turn's id.
func (d QuestionsDue) Key() string { return "k:" + d.TurnID }

// Apply folds the turn's next event into the reply, and returns what it has
// the bot post apart from it, nil for nothing. Started sets the status to
// "Thinking…", a tool call to the tool's title (never its input), Said adds
// its text, Finished ends the reply; a tool result naming an Analyst
// question is an AnswerDue, an action card a CardDue and Asked a
// QuestionsDue. Every other event (text deltas, results, reports, answers)
// changes nothing.
func (r *Reply) Apply(e agent.Event) Due {
	switch e := e.(type) {
	case agent.Started:
		r.Status = "Thinking…"
	case agent.ToolCalled:
		r.Status = e.Title
	case agent.Said:
		if strings.TrimSpace(e.Text) != "" {
			r.Said = append(r.Said, e.Text)
		}
	case agent.ToolReturned:
		if e.Question != nil && e.Question.ID != "" {
			return AnswerDue{Question: *e.Question}
		}
	case agent.ActionCard:
		return CardDue{Card: e}
	case agent.Asked:
		return QuestionsDue{TurnID: r.TurnID, Asked: e}
	case agent.Finished:
		r.End = Finished{e}
	}
	return nil
}

// stopText is what a turn that finished for reason says, "" for nothing.
var stopText = map[agent.StopReason]string{
	agent.StopSteps:     "It stopped at its step limit. Ask it to go on.",
	agent.StopWallTime:  "It stopped at its time limit. Ask it to go on.",
	agent.StopBudget:    "It stopped at this message’s spending cap.",
	agent.StopMaxTokens: "Its answer reached its length limit.",
	agent.StopRefused:   "The model declined to answer.",
	agent.StopContext:   "It ran out of room in the model’s context window. Ask for less at once, or start a new thread.",
}

// EndText is how a reply's end reads: a cap or a failure in words, who
// stopped it, or "" for a turn that answered or asked (and while it is
// live).
func (s Site) EndText(end Ending) Mrkdwn {
	switch end := end.(type) {
	case Stopped:
		if end.By == "" {
			return s.Text("Stopped.")
		}
		return Join(s.Text("Stopped by "), UserMention(end.By), s.Text("."))
	case Finished:
		switch end.Reason {
		case agent.StopAnswered, agent.StopAsked:
			return Mrkdwn{}
		case agent.StopFailed:
			if strings.TrimSpace(end.Error) == "" {
				return s.Text("Founder couldn’t finish this answer.")
			}
			return s.Text("Founder couldn’t finish this answer: " + strings.TrimSpace(end.Error))
		}
		if text, ok := stopText[end.Reason]; ok {
			return s.Text(text)
		}
		return s.Text("It stopped.")
	}
	return Mrkdwn{}
}

// Reply draws a reply as one message or more: its text, each Said split
// into sections of at most MaxSectionText, MaxReplyBlocks a message; under
// the last, while the turn is live, its status and Stop, and once it ended,
// how it ended. Every message but the last is full, and never changes as
// later events add to the reply, so the bot posts the next message once one
// fills. A reply with nothing said says it is working, or, once it ended
// with nothing to say of it, that Founder is done.
func (s Site) Reply(r Reply) []Message {
	var body []Block
	for _, said := range r.Said {
		for _, part := range Split(s.Text(said), MaxSectionText) {
			body = append(body, Section{Text: part})
		}
	}
	var chunks [][]Block
	for len(body) > MaxReplyBlocks {
		chunks, body = append(chunks, body[:MaxReplyBlocks]), body[MaxReplyBlocks:]
	}
	last := body
	var tail []Block
	switch end := s.EndText(r.End); {
	case r.End == nil:
		status := strings.TrimSpace(r.Status)
		if status == "" {
			status = "Working…"
		}
		tail = []Block{
			Context{Elements: []Mrkdwn{Cut(s.Text(status), MaxSectionText)}},
			Actions{Elements: []Button{{ActionID: ActionStop, Text: "Stop", Value: ButtonValue{Turn: r.TurnID}}}},
		}
	case end.s != "":
		tail = []Block{Context{Elements: []Mrkdwn{Cut(end, MaxSectionText)}}}
	case len(r.Said) == 0:
		done := "Done."
		if f, ok := r.End.(Finished); ok && f.Reason == agent.StopAsked {
			done = "Founder needs your answers, below."
		}
		tail = []Block{Context{Elements: []Mrkdwn{s.Text(done)}}}
	}
	chunks = append(chunks, append(last, tail...))
	out := make([]Message, 0, len(chunks))
	for _, blocks := range chunks {
		out = append(out, message(blocks))
	}
	return out
}

// message is a message of blocks, its notification text the text of its
// sections a line each, else of its context lines, cut to MaxMessageText.
func message(blocks []Block) Message {
	var sections, contexts []string
	for _, b := range blocks {
		switch b := b.(type) {
		case Section:
			sections = append(sections, b.Text.s)
		case Context:
			for _, e := range b.Elements {
				contexts = append(contexts, e.s)
			}
		}
	}
	if len(sections) == 0 {
		sections = contexts
	}
	return Message{Text: Cut(Mrkdwn{s: strings.Join(sections, "\n")}, MaxMessageText), Blocks: blocks}
}

// messages packs blocks into messages of at most MaxBlocks blocks.
func messages(blocks []Block) []Message {
	var out []Message
	for len(blocks) > MaxBlocks {
		out, blocks = append(out, message(blocks[:MaxBlocks])), blocks[MaxBlocks:]
	}
	return append(out, message(blocks))
}

// ReplyTo is how a person's reply in a thread reaches the bot, which is
// how its questions tell them to answer: a closed set (RepliesIn).
type ReplyTo interface{ replyTo() }

// ReplyPlain is a direct message with the bot, where every reply reaches
// it.
type ReplyPlain struct{}

// ReplyMentioning is a channel, where the bot sees only the messages that
// mention it, as BotUserID: a reply reaches it only by mentioning it.
type ReplyMentioning struct{ BotUserID string }

func (ReplyPlain) replyTo()      {}
func (ReplyMentioning) replyTo() {}

// RepliesIn is how a reply in channel reaches the bot, as botUserID: a
// direct message's (Slack's IM ids start with D) plainly, any other's by
// mentioning it.
func RepliesIn(channel, botUserID string) ReplyTo {
	if strings.HasPrefix(channel, "D") {
		return ReplyPlain{}
	}
	return ReplyMentioning{BotUserID: botUserID}
}

// Questions draws the questions a turn asks its person, each numbered, its
// options numbered under it. Founder's own are answered by replying in the
// thread ("1: 2, 2: my own words"), mentioning the bot outside a direct
// message (to). A generation's, which a reply in Slack can't answer, are
// shown for the person to answer on the draft's page in Foundry, which
// they link to (in workspaceID). A recommended option is marked, and a
// question of Founder's that isn't blocking may be left to it. More than
// MaxBlocks blocks are more messages. With no ReplyTo (nil), Founder's own
// say to mention the app, which reaches the bot anywhere.
func (s Site) Questions(asked agent.Asked, workspaceID string, to ReplyTo) []Message {
	several := len(asked.Questions) > 1
	var intro Mrkdwn
	if g := asked.Generation; g != nil {
		draft := g.OntologyName
		if g.GenerationTitle != "" {
			draft += " (" + g.GenerationTitle + ")"
		}
		what, them := "a question", "it"
		if several {
			what, them = strconv.Itoa(len(asked.Questions))+" questions", "them"
		}
		intro = Cut(s.Text("Founder relays "+what+" from the draft of "+draft+". Answer "+them+
			" on the draft’s page in Foundry: a reply here doesn’t reach the draft."), MaxSectionText)
	} else {
		ask, them := "Founder has a question for you. ", "it"
		if several {
			ask, them = "Founder has "+strconv.Itoa(len(asked.Questions))+" questions for you. ", "them, by their numbers"
		}
		switch to := to.(type) {
		case ReplyPlain:
			intro = s.Text(ask + "Reply in this thread to answer " + them + ".")
		case ReplyMentioning:
			intro = Join(s.Text(ask+"Reply in this thread mentioning "), UserMention(to.BotUserID),
				s.Text(" to answer "+them+". Without the mention, Founder won’t see your reply."))
		default:
			intro = s.Text(ask + "Reply in this thread mentioning the app to answer " + them + ".")
		}
	}
	blocks := []Block{Section{Text: intro}}
	if g := asked.Generation; g != nil {
		page := s.page("/ontology/"+url.PathEscape(g.OntologyID)+"/generations/"+url.PathEscape(g.GenerationID), []string{"workspace", workspaceID}, "")
		blocks = append(blocks, Actions{Elements: []Button{{ActionID: ActionOpen, Text: "Answer in Foundry", URL: &page, Style: StylePrimary}}})
	}
	for i, q := range asked.Questions {
		blocks = append(blocks, Divider{})
		var lines []Mrkdwn
		head := s.Text(q.Text)
		if q.Header != "" {
			head = Join(Bold(s.Text(q.Header)), s.Text(": "), head)
		}
		lines = append(lines, Join(Bold(s.Text(strconv.Itoa(i+1)+".")), s.Text(" "), head))
		lines = append(lines, s.optionLines(q, asked.Generation == nil)...)
		var text strings.Builder
		for _, l := range lines {
			if text.Len() > 0 {
				text.WriteString("\n")
			}
			appendPiece(&text, l.s)
		}
		for _, part := range Split(Mrkdwn{s: text.String()}, MaxSectionText) {
			blocks = append(blocks, Section{Text: part})
		}
	}
	return messages(blocks)
}

// optionLines are a question's options, a line each, and, when replied
// (answered by a reply in the thread), how to answer it.
func (s Site) optionLines(q agentquestion.Question, replied bool) []Mrkdwn {
	var lines []Mrkdwn
	how := "Reply in your own words."
	if c := q.Choice; c != nil {
		for j, o := range c.Options {
			line := Join(s.Text("   "+strconv.Itoa(j+1)+". "), s.Text(o.Label))
			if o.Label == q.Recommended {
				line = Join(line, s.Text(" "), Italic(s.Text("(recommended)")))
			}
			if o.Description != "" {
				line = Join(line, s.Text(" — "+o.Description))
			}
			lines = append(lines, line)
		}
		how = "Reply with an option’s number, or in your own words."
		if c.Multiple {
			how = "Reply with one or more options’ numbers, or in your own words."
		}
	} else if q.Recommended != "" {
		lines = append(lines, Join(Italic(s.Text("Suggested:")), s.Text(" "+q.Recommended)))
	}
	if !replied {
		return lines
	}
	if !q.Blocking {
		how += " You may leave it to Founder."
	}
	return append(lines, Italic(s.Text(how)))
}

// plural is n and noun, with an s when n isn't 1.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
