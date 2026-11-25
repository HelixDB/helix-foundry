package companyresearch

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ParseResearch reads ResearchTool's input as a model wrote it into a
// Research of domain's site (Sources, Fetched), each string on one line
// and cut to its cap. Questions without a question, or repeating one kept,
// case aside, are dropped, then the first MaxQuestions are kept, numbered
// q1 on; options that are empty or repeat one are dropped, then the first
// MaxOptions kept. An empty suggested name is "<company> operations".
// errInvalid means no company name or summary, no question kept, or input
// that isn't an object.
func ParseResearch(raw json.RawMessage, site Site) (Research, error) {
	var in struct {
		CompanyName   string `json:"company_name"`
		Summary       string `json:"summary"`
		SuggestedName string `json:"suggested_name"`
		Questions     []struct {
			Question  string   `json:"question"`
			BestGuess string   `json:"best_guess"`
			Options   []string `json:"options"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Research{}, fmt.Errorf("%w: %v", errInvalid, err)
	}
	r := Research{CompanyName: data(in.CompanyName, MaxCompanyName), Summary: data(in.Summary, MaxSummary),
		SuggestedName: data(in.SuggestedName, MaxName), Questions: []Question{}, Sources: site.Sources(), Fetched: len(site.Pages) > 0}
	seen := map[string]bool{}
	for _, q := range in.Questions {
		question := data(q.Question, MaxQuestion)
		if question == "" || seen[strings.ToLower(question)] || len(r.Questions) == MaxQuestions {
			continue
		}
		seen[strings.ToLower(question)] = true
		kept := Question{ID: fmt.Sprintf("%s%d", questionIDPrefix, len(r.Questions)+1), Question: question, BestGuess: data(q.BestGuess, MaxBestGuess),
			Options: []string{}}
		chosen := map[string]bool{}
		for _, o := range q.Options {
			if o = data(o, MaxOption); o != "" && !chosen[strings.ToLower(o)] && len(kept.Options) < MaxOptions {
				chosen[strings.ToLower(o)] = true
				kept.Options = append(kept.Options, o)
			}
		}
		r.Questions = append(r.Questions, kept)
	}
	switch {
	case r.CompanyName == "" || r.Summary == "":
		return Research{}, fmt.Errorf("%w: no company name or summary", errInvalid)
	case len(r.Questions) == 0:
		return Research{}, fmt.Errorf("%w: no questions", errInvalid)
	case r.SuggestedName == "":
		r.SuggestedName = cut(r.CompanyName+" operations", MaxName)
	}
	return r, nil
}

// ParseGoal reads GoalTool's input as a model wrote it: the goal keeping
// its lines, each collapsed, at most MaxGoal characters, and the name on
// one line, at most MaxName, else fallback. errInvalid means no goal, or
// input that isn't an object.
func ParseGoal(raw json.RawMessage, fallback string) (Goal, error) {
	var in struct{ Goal, Name string }
	if err := json.Unmarshal(raw, &in); err != nil {
		return Goal{}, fmt.Errorf("%w: %v", errInvalid, err)
	}
	g := Goal{Goal: block(in.Goal, MaxGoal), Name: data(in.Name, MaxName)}
	if g.Goal == "" {
		return Goal{}, fmt.Errorf("%w: no goal", errInvalid)
	}
	if g.Name == "" {
		g.Name = cut(fallback, MaxName)
	}
	return g, nil
}

// Answer is a person's answer to one of a research's questions.
type Answer struct{ QuestionID, Text string }

// AnswerError is an answer refused, in a sentence.
type AnswerError string

func (e AnswerError) Error() string { return string(e) }

// Answered is answers by question id, each trimmed, an empty one left out.
// An answer to a question r didn't ask, a second answer to one, or one
// longer than MaxAnswer characters is an AnswerError.
func (r Research) Answered(answers []Answer) (map[string]string, error) {
	asked := map[string]bool{}
	for _, q := range r.Questions {
		asked[q.ID] = true
	}
	out := map[string]string{}
	given := map[string]bool{}
	for _, a := range answers {
		text := strings.TrimSpace(a.Text)
		switch {
		case !asked[a.QuestionID]:
			return nil, AnswerError(fmt.Sprintf("the research asked no question %.40q", a.QuestionID))
		case given[a.QuestionID]:
			return nil, AnswerError(fmt.Sprintf("question %s is answered twice", a.QuestionID))
		case utf8.RuneCountInString(text) > MaxAnswer:
			return nil, AnswerError(fmt.Sprintf("the answer to question %s is longer than %d characters", a.QuestionID, MaxAnswer))
		}
		given[a.QuestionID] = true
		if text != "" {
			out[a.QuestionID] = text
		}
	}
	return out, nil
}
