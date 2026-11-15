package agentquestion

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func ExampleCheck() {
	asked := []Question{
		{ID: "q1", Text: "Is a customer an account?", Blocking: true,
			Choice: &Choice{Options: []Option{{Label: "An account"}, {Label: "A person"}}, Recommended: "An account"}},
		{ID: "q2", Text: "Which sources count as revenue?",
			Choice: &Choice{Options: []Option{{Label: "Stripe"}, {Label: "Invoices"}}, Multiple: true}},
		{ID: "q3", Text: "Which currency?"},
	}
	fmt.Println(Check(asked, []Answer{{QuestionID: "q1", Options: []string{"A person"}}}, false))
	fmt.Println(Check(asked, []Answer{{QuestionID: "q2", Options: []string{"Invoices", "Stripe"}, Text: " and refunds "}}, true))
	fmt.Println(Check(asked, nil, false))
	// Output:
	// [{q1 [A person] }] <nil>
	// [{q1 [An account] } {q2 [Stripe Invoices] and refunds} {q3 [] No preference: decide from the data and the goal.}] <nil>
	// [] question q1 needs an answer
}

func ExampleWords() {
	fmt.Println(Words(Answer{QuestionID: "q1", Options: []string{"Monthly", "Yearly"}, Text: "and \"quarterly\""}))
	fmt.Println(Words(Answer{QuestionID: "q2", Options: []string{}, Text: "GBP"}))
	// Output:
	// Monthly; Yearly; in their words: "and \"quarterly\""
	// in their words: "GBP"
}

func one(labels ...string) *Choice {
	c := &Choice{}
	for _, l := range labels {
		c.Options = append(c.Options, Option{Label: l})
	}
	return c
}

func TestJSON(t *testing.T) {
	t.Run("round-trips each kind", func(t *testing.T) {
		for _, q := range []Question{
			{ID: "q1", Text: "Which currency?"},
			{ID: "q2", Header: "Renewals", Text: "How often?", Blocking: true, Choice: &Choice{
				Options: []Option{{Label: "Monthly", Description: "Every month"}, {Label: "Yearly"}}, Recommended: "Yearly"}},
			{ID: "q3", Text: "Which?", Choice: &Choice{Options: []Option{{Label: "A"}, {Label: "B"}}, Multiple: true}},
		} {
			raw, err := json.Marshal(q)
			if err != nil {
				t.Fatal(err)
			}
			var back Question
			if err := json.Unmarshal(raw, &back); err != nil || !reflect.DeepEqual(back, q) {
				t.Errorf("%s: read back %+v (%v)", raw, back, err)
			}
		}
	})

	t.Run("writes the documented shape", func(t *testing.T) {
		raw, _ := json.Marshal(Question{ID: "q1", Header: "H", Text: "T", Blocking: true,
			Choice: &Choice{Options: []Option{{Label: "A", Description: "d"}, {Label: "B"}}, Multiple: true, Recommended: "A"}})
		want := `{"id":"q1","header":"H","text":"T","options":[{"label":"A","description":"d"},{"label":"B"}],"multiple":true,"recommended":"A","blocking":true}`
		if string(raw) != want {
			t.Errorf("got %s, want %s", raw, want)
		}
		free, _ := json.Marshal(Question{ID: "q2", Text: "T"})
		if string(free) != `{"id":"q2","text":"T","blocking":false}` {
			t.Errorf("a free question: %s", free)
		}
		answer, _ := json.Marshal(Answer{QuestionID: "q2", Options: []string{}, Text: "x"})
		if string(answer) != `{"question_id":"q2","options":[],"text":"x"}` {
			t.Errorf("an answer: %s", answer)
		}
	})

	t.Run("refuses what a question can't be", func(t *testing.T) {
		for name, raw := range map[string]string{
			"no id":                      `{"text":"T","blocking":true}`,
			"a long id":                  `{"id":"` + strings.Repeat("q", MaxIDBytes+1) + `","text":"T"}`,
			"a control in its id":        `{"id":"q\u0007","text":"T"}`,
			"blank text":                 `{"id":"q","text":"  "}`,
			"one option":                 `{"id":"q","text":"T","options":[{"label":"A"}]}`,
			"too many options":           `{"id":"q","text":"T","options":[` + strings.TrimSuffix(strings.Repeat(`{"label":"x"},`, MaxOptions+1), ",") + `]}`,
			"a blank label":              `{"id":"q","text":"T","options":[{"label":"A"},{"label":" "}]}`,
			"a label twice":              `{"id":"q","text":"T","options":[{"label":"A"},{"label":"A"}]}`,
			"a recommendation outside":   `{"id":"q","text":"T","options":[{"label":"A"},{"label":"B"}],"recommended":"C"}`,
			"multiple without options":   `{"id":"q","text":"T","multiple":true}`,
			"recommended without one":    `{"id":"q","text":"T","recommended":"A"}`,
			"options that aren't a list": `{"id":"q","text":"T","options":"A"}`,
		} {
			var q Question
			if err := json.Unmarshal([]byte(raw), &q); err == nil {
				t.Errorf("%s: %s read as %+v", name, raw, q)
			}
		}
	})
}

func TestCheck(t *testing.T) {
	asked := []Question{
		{ID: "free", Text: "Which currency?", Blocking: true},
		{ID: "one", Text: "How often?", Blocking: true, Choice: &Choice{Options: []Option{{Label: "Monthly"}, {Label: "Yearly"}}, Recommended: "Yearly"}},
		{ID: "many", Text: "Which?", Choice: &Choice{Options: []Option{{Label: "A"}, {Label: "B"}, {Label: "C"}}, Multiple: true}},
	}
	free := Answer{QuestionID: "free", Text: "GBP"}
	monthly := Answer{QuestionID: "one", Options: []string{"Monthly"}}

	t.Run("takes each kind's answers, in the order asked", func(t *testing.T) {
		got, err := Check(asked, []Answer{
			{QuestionID: "many", Options: []string{"C", "A"}, Text: "and D"}, {QuestionID: "one", Text: " Quarterly "}, free,
		}, false)
		want := []Answer{{QuestionID: "free", Options: []string{}, Text: "GBP"}, {QuestionID: "one", Options: []string{}, Text: "Quarterly"},
			{QuestionID: "many", Options: []string{"A", "C"}, Text: "and D"}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Check = (%v, %v), want %v", got, err, want)
		}
		got, err = Check(asked, []Answer{free, monthly, {QuestionID: "many", Text: "none of them"}}, false)
		if err != nil || len(got) != 3 || got[2].Text != "none of them" {
			t.Fatalf("words alone for a choice of several: (%v, %v)", got, err)
		}
	})

	t.Run("leaves out a question that doesn't block, or defaults it", func(t *testing.T) {
		got, err := Check(asked, []Answer{free, monthly}, false)
		if err != nil || len(got) != 2 {
			t.Fatalf("Check = (%v, %v)", got, err)
		}
		got, err = Check(asked, nil, true)
		want := []Answer{{QuestionID: "free", Options: []string{}, Text: NoPreference}, {QuestionID: "one", Options: []string{"Yearly"}},
			{QuestionID: "many", Options: []string{}, Text: NoPreference}}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("Check with defaults = (%v, %v), want %v", got, err, want)
		}
	})

	t.Run("refuses what the question doesn't take", func(t *testing.T) {
		for name, c := range map[string]struct {
			given []Answer
			want  string
		}{
			"a question not asked":    {[]Answer{{QuestionID: "q9", Text: "x"}}, `no question "q9"`},
			"a question twice":        {[]Answer{free, free}, "answered twice"},
			"blank words":             {[]Answer{{QuestionID: "free", Text: " \n"}}, "1 to 2000 characters"},
			"long words":              {[]Answer{{QuestionID: "free", Text: strings.Repeat("é", MaxAnswerChars+1)}}, "1 to 2000 characters"},
			"invalid UTF-8":           {[]Answer{{QuestionID: "free", Text: "\xff"}}, "1 to 2000 characters"},
			"an option to a free one": {[]Answer{{QuestionID: "free", Options: []string{"GBP"}}}, "in words, and no options"},
			"nothing for a free one":  {[]Answer{{QuestionID: "free"}}, "in words, and no options"},
			"an option not offered":   {[]Answer{{QuestionID: "one", Options: []string{"Weekly"}}}, `no option "Weekly"`},
			"two of a choice of one":  {[]Answer{{QuestionID: "one", Options: []string{"Monthly", "Yearly"}}}, "takes one option, not 2"},
			"an option and words":     {[]Answer{{QuestionID: "one", Options: []string{"Monthly"}, Text: "or weekly"}}, "not both"},
			"nothing for a choice":    {[]Answer{{QuestionID: "one"}}, "needs an option chosen"},
			"an option twice":         {[]Answer{{QuestionID: "many", Options: []string{"A", "A"}}}, "chosen twice"},
			"a blocking one left out": {[]Answer{free}, "question one needs an answer"},
			"nothing for several":     {[]Answer{{QuestionID: "many", Options: []string{}}}, "needs an option chosen"},
		} {
			for _, useDefaults := range []bool{false, true} {
				if c.want == "question one needs an answer" && useDefaults {
					continue // its default answers it
				}
				if _, err := Check(asked, c.given, useDefaults); err == nil || !strings.Contains(err.Error(), c.want) {
					t.Errorf("%s (defaults %t): err = %v, want %q", name, useDefaults, err, c.want)
				}
			}
		}
		if got, err := Check(asked, []Answer{{QuestionID: "free", Text: strings.Repeat("é", MaxAnswerChars)}, monthly}, false); err != nil || len(got) != 2 {
			t.Fatalf("words of %d characters: (%v, %v)", MaxAnswerChars, got, err)
		}
	})

	t.Run("a default without a recommendation leaves it to the agent", func(t *testing.T) {
		if got := Default(Question{ID: "q", Text: "T", Choice: one("A", "B")}); got.Text != NoPreference || len(got.Options) != 0 {
			t.Errorf("Default = %+v", got)
		}
	})
}
