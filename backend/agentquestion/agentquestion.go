// Package agentquestion is a question an agent asks the person, and the
// person's answer: one shape for the ontology agent's questions about a
// draft (a generation that needs_input) and the Assistant's own
// (helix_foundry_ask_person), which the site shows one at a time.
//
// A question is a free answer (no Choice: the person writes it) or a choice
// of 2 to MaxOptions options, of which the person picks one, or several when
// the asking agent made it Multiple; the person may always answer in their
// own words instead (one) or as well (several). The agent may recommend an
// answer: one of its options, marked, or, for a free answer, its words. A
// blocking question needs an answer; one that isn't may be left to the
// asking agent (Answer.Left), which then takes its recommended answer, or
// decides, as its own choice, never as the person's.
//
// Its JSON, as the store keeps it and the API and the agents read it:
//
//	{"id": "q1", "header": "Customers", "text": "Is a customer an account?",
//	 "options": [{"label": "An account", "description": "..."}, {"label": "A person"}],
//	 "multiple": false, "recommended": "An account", "blocking": true}
//
// (the ontology agent's own JSON names recommended "default":
// model.OntologyQuestion reads it), and an answer's {"question_id": "q1", "options": ["An account"], "text": "..."},
// or {"question_id": "q1", "left": true} for one left to the agent.
// Every string an agent wrote may quote the data: it is shown as plain text.
package agentquestion

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxIDBytes bounds a question's id.
	MaxIDBytes = 256
	// MaxOptions is the most options a question offers.
	MaxOptions = 20
	// MaxAnswerChars bounds an answer's own words, once trimmed.
	MaxAnswerChars = 2000
)

// Option is one answer a question offers: a short label, which an answer
// names, and an optional line saying what choosing it means.
type Option struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Choice is what a question offers to choose from.
type Choice struct {
	// Options are 2 to MaxOptions, their labels unique.
	Options []Option
	// Multiple lets the person pick one or more; else exactly one.
	Multiple bool
}

// Question is a question an agent asks the person. Validate holds its
// invariants; UnmarshalJSON refuses one that breaks them.
type Question struct {
	ID string
	// Header names it in a few words, for the progress strip; may be empty.
	Header string
	Text   string
	// Blocking needs an answer; one that isn't may be left to the agent.
	Blocking bool
	// Choice is nil for a free answer, which the person writes.
	Choice *Choice
	// Recommended is the answer the agent recommends, which the question
	// takes when left to it: one of Choice's labels, or, for a free answer,
	// its words; empty for none.
	Recommended string
}

// wire is a question's JSON.
type wire struct {
	ID          string   `json:"id"`
	Header      string   `json:"header,omitempty"`
	Text        string   `json:"text"`
	Options     []Option `json:"options,omitempty"`
	Multiple    bool     `json:"multiple,omitempty"`
	Recommended string   `json:"recommended,omitempty"`
	Blocking    bool     `json:"blocking"`
}

// MarshalJSON writes the question's JSON (the package's doc).
func (q Question) MarshalJSON() ([]byte, error) {
	w := wire{ID: q.ID, Header: q.Header, Text: q.Text, Recommended: q.Recommended, Blocking: q.Blocking}
	if c := q.Choice; c != nil {
		w.Options, w.Multiple = c.Options, c.Multiple
	}
	return json.Marshal(w)
}

// UnmarshalJSON reads a question's JSON, refusing one Validate refuses, or
// multiple without options.
func (q *Question) UnmarshalJSON(data []byte) error {
	var w wire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	out, err := New(w.ID, w.Header, w.Text, w.Blocking, w.Options, w.Multiple, w.Recommended)
	if err != nil {
		return err
	}
	*q = out
	return nil
}

// New is a question from its fields as JSON carries them: a choice when it
// has options, else a free answer, which can't be multiple; refused as
// Validate refuses. A free answer's recommended words are trimmed, and none
// when they are blank or longer than an answer may be, so an agent's
// suggestion never makes its question unreadable.
func New(id, header, text string, blocking bool, options []Option, multiple bool, recommended string) (Question, error) {
	out := Question{ID: id, Header: header, Text: text, Blocking: blocking, Recommended: recommended}
	switch {
	case len(options) > 0:
		out.Choice = &Choice{Options: options, Multiple: multiple}
	case multiple:
		return Question{}, fmt.Errorf("question %q: multiple comes only with options", id)
	default:
		out.Recommended = strings.TrimSpace(recommended)
		if utf8.RuneCountInString(out.Recommended) > MaxAnswerChars {
			out.Recommended = ""
		}
	}
	if err := out.Validate(); err != nil {
		return Question{}, err
	}
	return out, nil
}

// Validate checks the question's invariants: an id of 1 to MaxIDBytes
// bytes without control characters, text that isn't blank, and, for a
// choice, 2 to MaxOptions options whose labels aren't blank and differ, the
// recommended one among them; for a free answer, recommended words of at
// most MaxAnswerChars.
func (q Question) Validate() error {
	switch {
	case q.ID == "" || len(q.ID) > MaxIDBytes || !utf8.ValidString(q.ID) || strings.ContainsFunc(q.ID, unicode.IsControl):
		return fmt.Errorf("a question's id is 1 to %d bytes without control characters", MaxIDBytes)
	case strings.TrimSpace(q.Text) == "" || !utf8.ValidString(q.Text) || !utf8.ValidString(q.Header):
		return fmt.Errorf("question %s has no text", q.ID)
	case !utf8.ValidString(q.Recommended) || utf8.RuneCountInString(q.Recommended) > MaxAnswerChars:
		return fmt.Errorf("question %s recommends words of more than %d characters", q.ID, MaxAnswerChars)
	case q.Choice == nil:
		return nil
	}
	c := q.Choice
	if len(c.Options) < 2 || len(c.Options) > MaxOptions {
		return fmt.Errorf("question %s offers 2 to %d options, not %d", q.ID, MaxOptions, len(c.Options))
	}
	for i, o := range c.Options {
		switch {
		case strings.TrimSpace(o.Label) == "" || !utf8.ValidString(o.Label) || !utf8.ValidString(o.Description):
			return fmt.Errorf("question %s has an option without a label", q.ID)
		case slices.ContainsFunc(c.Options[:i], func(p Option) bool { return p.Label == o.Label }):
			return fmt.Errorf("question %s offers %q twice", q.ID, o.Label)
		}
	}
	if q.Recommended != "" && !q.offers(q.Recommended) {
		return fmt.Errorf("question %s recommends %q, which isn't one of its options", q.ID, q.Recommended)
	}
	return nil
}

// offers reports a label among the question's options.
func (q Question) offers(label string) bool {
	return q.Choice != nil && slices.ContainsFunc(q.Choice.Options, func(o Option) bool { return o.Label == label })
}

// Answer is the person's answer to one question: the labels of the options
// they chose, in the question's order, and their own words, at least one;
// or, Left, that they left it to the agent, with neither. Only Check leaves
// a question: an answer given with Left is refused.
type Answer struct {
	QuestionID string
	Options    []string
	Text       string
	// Left: the person chose nothing and wrote nothing, and the agent takes
	// its recommended answer, or decides, as its own choice.
	Left bool
}

// answerJSON is an answer's JSON: options and text, or left.
type answerJSON struct {
	QuestionID string   `json:"question_id"`
	Options    []string `json:"options,omitempty"`
	Text       string   `json:"text,omitempty"`
	Left       bool     `json:"left,omitempty"`
}

// MarshalJSON writes {"question_id", "options": [...], "text"?}, its options
// an array even when it has none, or {"question_id", "left": true}.
func (a Answer) MarshalJSON() ([]byte, error) {
	if a.Left {
		return json.Marshal(answerJSON{QuestionID: a.QuestionID, Left: true})
	}
	options := a.Options
	if options == nil {
		options = []string{}
	}
	return json.Marshal(struct {
		QuestionID string   `json:"question_id"`
		Options    []string `json:"options"`
		Text       string   `json:"text,omitempty"`
	}{a.QuestionID, options, a.Text})
}

// UnmarshalJSON reads what MarshalJSON writes, refusing a left answer with
// options or text.
func (a *Answer) UnmarshalJSON(data []byte) error {
	var w answerJSON
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	if w.Left && (len(w.Options) > 0 || w.Text != "") {
		return fmt.Errorf("question %s's answer is left to the agent, so it has no options or text", w.QuestionID)
	}
	if w.Options == nil && !w.Left {
		w.Options = []string{}
	}
	*a = Answer(w)
	return nil
}

// Check checks the person's answers to asked and returns them as the agent
// takes them, in the order asked: each names a question asked, once, with
// its own words (if any) trimmed to 1 to MaxAnswerChars characters, and
//   - for a free question, their words alone;
//   - for a choice of one, one of its options or their words, not both;
//   - for a choice of several, one or more of its options, once each, with
//     their words or without, or their words alone.
//
// With useDefaults, every question left out is left to the agent (Left);
// without, every blocking question must be answered, and one that isn't is
// left out. The error says what is wrong, for the person.
func Check(asked []Question, given []Answer, useDefaults bool) ([]Answer, error) {
	byID := make(map[string]Answer, len(given))
	for _, a := range given {
		i := slices.IndexFunc(asked, func(q Question) bool { return q.ID == a.QuestionID })
		if i < 0 {
			return nil, fmt.Errorf("no question %q was asked", a.QuestionID)
		}
		if _, twice := byID[a.QuestionID]; twice {
			return nil, fmt.Errorf("question %s is answered twice", a.QuestionID)
		}
		checked, err := checkOne(asked[i], a)
		if err != nil {
			return nil, err
		}
		byID[a.QuestionID] = checked
	}
	out := make([]Answer, 0, len(asked))
	for _, q := range asked {
		a, ok := byID[q.ID]
		switch {
		case ok:
		case useDefaults:
			a = Answer{QuestionID: q.ID, Left: true}
		case q.Blocking:
			return nil, fmt.Errorf("question %s needs an answer", q.ID)
		default:
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

// checkOne checks one answer to q, as Check says.
func checkOne(q Question, a Answer) (Answer, error) {
	if a.Left {
		return Answer{}, fmt.Errorf("question %s: leave it out to leave it to the agent", q.ID)
	}
	text := strings.TrimSpace(a.Text)
	if a.Text != "" && (text == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > MaxAnswerChars) {
		return Answer{}, fmt.Errorf("the words answering question %s are 1 to %d characters", q.ID, MaxAnswerChars)
	}
	out := Answer{QuestionID: q.ID, Options: []string{}, Text: text}
	if q.Choice == nil {
		if len(a.Options) > 0 || text == "" {
			return Answer{}, fmt.Errorf("question %s takes an answer in words, and no options", q.ID)
		}
		return out, nil
	}
	for _, label := range a.Options {
		switch {
		case !q.offers(label):
			return Answer{}, fmt.Errorf("question %s offers no option %q", q.ID, label)
		case slices.Contains(out.Options, label):
			return Answer{}, fmt.Errorf("question %s's option %q is chosen twice", q.ID, label)
		}
		out.Options = append(out.Options, label)
	}
	// The question's order, whatever the order chosen in.
	slices.SortFunc(out.Options, func(x, y string) int {
		return slices.IndexFunc(q.Choice.Options, func(o Option) bool { return o.Label == x }) -
			slices.IndexFunc(q.Choice.Options, func(o Option) bool { return o.Label == y })
	})
	switch n := len(out.Options); {
	case n == 0 && text == "":
		return Answer{}, fmt.Errorf("question %s needs an option chosen or an answer in words", q.ID)
	case !q.Choice.Multiple && n > 1:
		return Answer{}, fmt.Errorf("question %s takes one option, not %d", q.ID, n)
	case !q.Choice.Multiple && n == 1 && text != "":
		return Answer{}, fmt.Errorf("question %s takes one option or an answer in words, not both", q.ID)
	}
	return out, nil
}

// Words is an answer in plain words, as an agent reads it: the options
// chosen, then the person's own words, quoted; or that they left it to the
// agent.
//
//	Words(Answer{Options: []string{"Monthly", "Yearly"}, Text: "and quarterly"}) // Monthly; Yearly; in their words: "and quarterly"
func Words(a Answer) string {
	if a.Left {
		return "left to the agent"
	}
	parts := slices.Clone(a.Options)
	if a.Text != "" {
		parts = append(parts, "in their words: "+strconv.Quote(a.Text))
	}
	return strings.Join(parts, "; ")
}
