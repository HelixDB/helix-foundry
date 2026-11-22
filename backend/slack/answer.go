package slack

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// answer.go draws an Analyst question's answer as the bot posts it in the
// thread: its paragraphs and lists with citations linked to the site, and
// its visuals. A count or an aggregate is a figure, read from the output the
// agent recorded, never from the model's words; every other visual is a
// line opening the thread in Foundry, where it is drawn.

// Question is an Analyst question as its answer shows it: where it is, its
// ontology's name, how it stands, its error and its stored result.
type Question struct {
	QuestionPage
	OntologyName string
	Status       model.FoundryQuestionStatus
	// Error is why a failed question failed, as the worker recorded it.
	Error  string
	Result json.RawMessage
}

// visualWhat names what a visual is in Foundry, by its kind.
var visualWhat = map[ontologyagent.VisualKind]string{
	ontologyagent.VisualAuto: "the table or chart", ontologyagent.VisualTable: "the table", ontologyagent.VisualBar: "the bar chart",
	ontologyagent.VisualLine: "the line chart", ontologyagent.VisualFigure: "the figure",
}

// Answer draws a question as its answer: a line naming the ontology and
// opening the thread in Foundry, then the answer, or why there is none: a
// question still live (the bot stopped waiting for it), one that can't be
// answered (the agent's reason and what the ontology lacks), one stopped at
// its spending cap, one that failed or was cancelled. A result that can't
// be read says to open Foundry. More than MaxBlocks blocks are more
// messages.
func (s Site) Answer(q Question) []Message {
	thread := s.Thread(q.QuestionPage)
	name := strings.TrimSpace(q.OntologyName)
	if name == "" {
		name = "your ontology"
	}
	blocks := []Block{Context{Elements: []Mrkdwn{Cut(Join(s.Text("Answer from "), Bold(s.Text(name)), s.Text(" · "),
		s.Link(thread, "Open in Foundry")), MaxSectionText)}}}
	say := func(text Mrkdwn) {
		for _, part := range Split(text, MaxSectionText) {
			blocks = append(blocks, Section{Text: part})
		}
	}
	unread := func() {
		say(Join(s.Text("This answer can’t be shown here: "), s.Link(thread, "open it in Foundry"), s.Text(".")))
	}
	switch {
	case q.Status.Live():
		say(Join(s.Text("The Analyst is still answering. "), s.Link(thread, "Follow it in Foundry"), s.Text(".")))
	case q.Status == model.FoundryQuestionCancelled:
		say(s.Text("The question was cancelled."))
	case len(q.Result) == 0 && q.Status == model.FoundryQuestionFailed:
		say(s.Text(failedText(q.Error, "")))
	default:
		r, err := ontologyagent.DecodeAskResult(q.Result)
		if err != nil {
			unread()
			break
		}
		switch r.Status {
		case ontologyagent.AskAnswered:
			blocks = append(blocks, s.answerBlocks(q.QuestionPage, r)...)
		case ontologyagent.AskCannotAnswer:
			say(s.Text("The Analyst can’t answer this from " + name + ": " + strings.TrimSpace(r.Reason)))
			if gap := strings.TrimSpace(r.Gap); gap != "" {
				say(s.Text("What it would need: " + gap))
			}
		case ontologyagent.AskBudgetExhausted:
			say(s.Text("The Analyst stopped at its spending cap before it could answer."))
		default:
			say(s.Text(failedText(q.Error, r.Failure)))
		}
	}
	return messages(blocks)
}

// failedText says why a question failed: its recorded error, else the
// agent's reason.
func failedText(why string, failure ontologyagent.AskFailure) string {
	if why = strings.TrimSpace(why); why == "" {
		why = strings.ReplaceAll(string(failure), "_", " ")
	}
	if why == "" {
		return "The Analyst couldn’t answer."
	}
	return "The Analyst couldn’t answer: " + why
}

// answerBlocks are an answer's blocks: each paragraph a section, each list
// its items as "• " lines, with citations linked and numbered by first use,
// and each visual after the block it follows (after the last without one).
func (s Site) answerBlocks(p QuestionPage, r ontologyagent.AskResult) []Block {
	citations := make(map[string]ontologyagent.CitationTarget, len(r.Citations))
	for _, c := range r.Citations {
		citations[c.Marker] = c.Target
	}
	outputs := make(map[string]json.RawMessage, len(r.Queries))
	for _, q := range r.Queries {
		outputs[q.EvidenceID] = q.Output
	}
	after := make(map[int][]ontologyagent.AskVisual, len(r.Answer.Visuals))
	for _, v := range r.Answer.Visuals {
		at := len(r.Answer.Blocks) - 1
		if v.After != nil {
			at = int(*v.After)
		}
		after[at] = append(after[at], v)
	}
	numbers := map[string]int{}
	var blocks []Block
	for i, b := range r.Answer.Blocks {
		text := Mrkdwn{}
		if b.Para != nil {
			text = s.Cited(p, *b.Para, citations, numbers)
		} else {
			items := make([]Mrkdwn, 0, len(b.List)*2)
			for j, item := range b.List {
				if j > 0 {
					items = append(items, Mrkdwn{s: "\n"})
				}
				items = append(items, Mrkdwn{s: "• "}, s.Cited(p, item, citations, numbers))
			}
			text = Join(items...)
		}
		if strings.TrimSpace(text.s) != "" {
			for _, part := range Split(text, MaxSectionText) {
				blocks = append(blocks, Section{Text: part})
			}
		}
		for _, v := range after[i] {
			blocks = append(blocks, s.visualBlocks(p, v, outputs[v.EvidenceID])...)
		}
	}
	return blocks
}

// visualBlocks draw a visual: a count or an aggregate as its title and its
// figure, with what it is over and what was cut in a line under it; any
// other a line opening the thread in Foundry, where it is drawn.
func (s Site) visualBlocks(p QuestionPage, v ontologyagent.AskVisual, raw json.RawMessage) []Block {
	title := strings.TrimSpace(v.Title)
	output, err := ontologyagent.DecodeQueryOutput(raw)
	var figure, detail, cut string
	switch o := output.(type) {
	case ontologyagent.CountOutput:
		figure, cut = groupDigits(strconv.FormatUint(o.Count, 10)), o.Cut
		if title == "" {
			title = "Count"
		}
	case ontologyagent.AggregateOutput:
		figure, cut = "none", o.Cut
		if o.Value != nil {
			figure = formatNumber(*o.Value)
		}
		detail = "Over " + plural(int(min(o.Values, math.MaxInt32)), "value")
		if o.Skipped > 0 {
			detail += " · " + strconv.FormatUint(o.Skipped, 10) + " without one, skipped"
		}
		if title == "" {
			title = "Result"
		}
	}
	if err != nil || figure == "" {
		what := visualWhat[v.Kind]
		if title != "" {
			return []Block{Context{Elements: []Mrkdwn{Cut(Join(Bold(s.Text(title)), s.Text(": "), s.Link(s.Thread(p), "Open in Foundry"),
				s.Text(" for "+what+".")), MaxSectionText)}}}
		}
		return []Block{Context{Elements: []Mrkdwn{Join(s.Link(s.Thread(p), "Open in Foundry"), s.Text(" for "+what+"."))}}}
	}
	out := []Block{Section{Text: Cut(Join(Bold(s.Text(title)), s.Text(": "+figure)), MaxSectionText)}}
	var notes []Mrkdwn
	if detail != "" {
		notes = append(notes, s.Text(detail))
	}
	if cut = strings.TrimSpace(cut); cut != "" {
		notes = append(notes, Cut(s.Text(cut), MaxSectionText))
	}
	if len(notes) > 0 {
		out = append(out, Context{Elements: notes})
	}
	return out
}

// formatNumber writes a number as Foundry does: an integer with thousands
// separators, any other with at most 4 decimals.
func formatNumber(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return groupDigits(strconv.FormatFloat(v, 'f', 0, 64))
	}
	text := strings.TrimRight(strings.TrimRight(strconv.FormatFloat(v, 'f', 4, 64), "0"), ".")
	if text == "-0" {
		text = "0"
	}
	whole, frac, _ := strings.Cut(text, ".")
	if frac == "" {
		return groupDigits(whole)
	}
	return groupDigits(whole) + "." + frac
}

// groupDigits puts a comma between every three digits of an integer's
// text, keeping its sign.
func groupDigits(text string) string {
	sign := ""
	if strings.HasPrefix(text, "-") {
		sign, text = "-", text[1:]
	}
	var b strings.Builder
	for i, d := range text {
		if i > 0 && (len(text)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	return sign + b.String()
}
