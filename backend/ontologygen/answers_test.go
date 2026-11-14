package ontologygen

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

func ExampleAnswers() {
	yearly := "Yearly"
	asked := []model.OntologyQuestion{
		{ID: "q1", Text: "Is a customer an account?", Blocking: true},
		{ID: "q2", Text: "How often do they renew?", Options: []string{"Monthly", "Yearly"}, Default: &yearly},
		{ID: "q3", Text: "Which currency?"},
	}
	fmt.Println(Answers(asked, []model.OntologyAnswer{{QuestionID: "q1", Text: " Yes "}}, false))
	fmt.Println(Answers(asked, []model.OntologyAnswer{{QuestionID: "q1", Text: "Yes"}}, true))
	fmt.Println(Answers(asked, nil, false))
	// Output:
	// [{q1 Yes}] <nil>
	// [{q1 Yes} {q2 Yearly} {q3 No preference: decide from the data and the goal.}] <nil>
	// [] question q1 needs an answer, or use the agent's defaults
}

func TestAnswers(t *testing.T) {
	monthly := "Monthly"
	blank := "  "
	asked := []model.OntologyQuestion{
		{ID: "q1", Text: "Is a customer an account?", Blocking: true},
		{ID: "q2", Text: "How often?", Options: []string{"Monthly", "Yearly"}, Default: &monthly, Blocking: true},
		{ID: "q3", Text: "Which currency?", Default: &blank},
	}

	t.Run("keeps the questions' order and the person's words", func(t *testing.T) {
		got, err := Answers(asked, []model.OntologyAnswer{{QuestionID: "q3", Text: "GBP"}, {QuestionID: "q2", Text: "Yearly"}, {QuestionID: "q1", Text: "No"}}, false)
		want := []model.OntologyAnswer{{QuestionID: "q1", Text: "No"}, {QuestionID: "q2", Text: "Yearly"}, {QuestionID: "q3", Text: "GBP"}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Answers = (%v, %v), want %v", got, err, want)
		}
	})

	// Every question left out takes the agent's default; a blank default is none.
	t.Run("fills the rest with defaults", func(t *testing.T) {
		got, err := Answers(asked, nil, true)
		want := []model.OntologyAnswer{{QuestionID: "q1", Text: NoPreference}, {QuestionID: "q2", Text: "Monthly"}, {QuestionID: "q3", Text: NoPreference}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Answers = (%v, %v), want %v", got, err, want)
		}
	})

	t.Run("leaves out a question that doesn't block", func(t *testing.T) {
		got, err := Answers(asked, []model.OntologyAnswer{{QuestionID: "q1", Text: "No"}, {QuestionID: "q2", Text: "Yearly"}}, false)
		if err != nil || len(got) != 2 {
			t.Fatalf("Answers = (%v, %v)", got, err)
		}
	})

	t.Run("refuses what the agent can't take", func(t *testing.T) {
		for name, c := range map[string]struct {
			given []model.OntologyAnswer
			want  string
		}{
			"a question not asked":  {[]model.OntologyAnswer{{QuestionID: "q9", Text: "x"}}, `asked no question "q9"`},
			"a question twice":      {[]model.OntologyAnswer{{QuestionID: "q1", Text: "a"}, {QuestionID: "q1", Text: "b"}}, "answered twice"},
			"a blank answer":        {[]model.OntologyAnswer{{QuestionID: "q1", Text: " \n"}}, "1 to 2000 characters"},
			"a long answer":         {[]model.OntologyAnswer{{QuestionID: "q1", Text: strings.Repeat("é", MaxAnswerChars+1)}}, "1 to 2000 characters"},
			"invalid UTF-8":         {[]model.OntologyAnswer{{QuestionID: "q1", Text: "\xff"}}, "1 to 2000 characters"},
			"a blocking one missed": {[]model.OntologyAnswer{{QuestionID: "q1", Text: "Yes"}}, "question q2 needs an answer"},
		} {
			for _, useDefaults := range []bool{false, true} {
				if c.want == "question q2 needs an answer" && useDefaults {
					continue // a default answers it
				}
				if _, err := Answers(asked, c.given, useDefaults); err == nil || !strings.Contains(err.Error(), c.want) {
					t.Errorf("%s (defaults %t): err = %v, want %q", name, useDefaults, err, c.want)
				}
			}
		}
		if got, err := Answers(asked, []model.OntologyAnswer{{QuestionID: "q1", Text: strings.Repeat("é", MaxAnswerChars)}}, true); err != nil || len(got) != 3 {
			t.Fatalf("an answer of %d characters: (%v, %v)", MaxAnswerChars, got, err)
		}
	})
}
