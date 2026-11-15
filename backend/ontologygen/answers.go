package ontologygen

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// MaxAnswerChars bounds one answer's text.
const MaxAnswerChars = 2000

// NoPreference answers a question whose default is taken when the agent
// offered none: the agent decides.
const NoPreference = "No preference: decide from the data and the goal."

// Answers checks a person's answers to the questions a generation waits on
// and returns what its feedback sends, in the order the questions were
// asked. Each answer names a question asked, at most once, with 1 to
// MaxAnswerChars characters once trimmed. With useDefaults, every question
// left out takes the agent's default, or NoPreference when it offered none;
// without, every blocking question must be answered, and one that isn't
// blocking may be left out, open in the draft. The error says what is wrong,
// for the person. ExampleAnswers shows each.
func Answers(asked []model.OntologyQuestion, given []model.OntologyAnswer, useDefaults bool) ([]model.OntologyAnswer, error) {
	byID := map[string]string{}
	for _, a := range given {
		text := strings.TrimSpace(a.Text)
		switch {
		case !slices.ContainsFunc(asked, func(q model.OntologyQuestion) bool { return q.ID == a.QuestionID }):
			return nil, fmt.Errorf("the generation asked no question %q", a.QuestionID)
		case byID[a.QuestionID] != "":
			return nil, fmt.Errorf("question %s is answered twice", a.QuestionID)
		case text == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > MaxAnswerChars:
			return nil, fmt.Errorf("the answer to question %s must be 1 to %d characters", a.QuestionID, MaxAnswerChars)
		}
		byID[a.QuestionID] = text
	}
	out := make([]model.OntologyAnswer, 0, len(asked))
	for _, q := range asked {
		text, ok := byID[q.ID]
		switch {
		case ok:
		case useDefaults && q.Default != nil && strings.TrimSpace(*q.Default) != "":
			text = *q.Default
		case useDefaults:
			text = NoPreference
		case q.Blocking:
			return nil, fmt.Errorf("question %s needs an answer, or use the agent's defaults", q.ID)
		default:
			continue
		}
		out = append(out, model.OntologyAnswer{QuestionID: q.ID, Text: text})
	}
	return out, nil
}
