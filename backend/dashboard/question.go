package dashboard

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// QuestionVisual is what an Analyst answer's query result gives a tile: the
// query the question recorded, and the kind and title of the visual that
// shows it, if one does.
type QuestionVisual struct {
	Query  json.RawMessage
	Visual VisualKind
	// Title is the visual's; empty when no visual titles the result.
	Title string
}

// FromQuestion reads the query result evidenceID (such as e2) of an answered
// question's result (ask_result.json): its recorded query, checked and
// written as a tile's is, and the visual of it, if the answer has one. A
// question that wasn't answered, or recorded no such query, is an error.
func FromQuestion(result json.RawMessage, evidenceID string) (QuestionVisual, error) {
	var r struct {
		Status string `json:"status"`
		Answer struct {
			Visuals []struct {
				EvidenceID string  `json:"evidence_id"`
				Kind       *string `json:"kind"`
				Title      *string `json:"title"`
			} `json:"visuals"`
		} `json:"answer"`
		Queries []struct {
			EvidenceID string          `json:"evidence_id"`
			Query      json.RawMessage `json:"query"`
		} `json:"queries"`
	}
	if err := json.Unmarshal(result, &r); err != nil || r.Status != "answered" {
		return QuestionVisual{}, fmt.Errorf("the question has no answer to take a result from")
	}
	var out QuestionVisual
	for _, q := range r.Queries {
		if q.EvidenceID != evidenceID {
			continue
		}
		query, err := CanonicalQuery(q.Query)
		if err != nil {
			return QuestionVisual{}, fmt.Errorf("the question's %s: %w", evidenceID, err)
		}
		out.Query = query
	}
	if out.Query == nil {
		return QuestionVisual{}, fmt.Errorf("the question recorded no query result %s", evidenceID)
	}
	for _, v := range r.Answer.Visuals {
		if v.EvidenceID != evidenceID {
			continue
		}
		if v.Kind != nil {
			// An unknown kind is no hint, as the site reads one.
			out.Visual, _ = ParseVisualKind(*v.Kind)
		}
		if v.Title != nil {
			out.Title = TileTitle(*v.Title)
		}
	}
	return out, nil
}

// TileTitle is text made a tile's title: trimmed, on one line, without
// control characters, and cut to MaxTitleChars characters with "…"; empty
// when nothing is left.
func TileTitle(text string) string {
	t := strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)), " ")
	if utf8.RuneCountInString(t) <= MaxTitleChars {
		return t
	}
	return string([]rune(t)[:MaxTitleChars-1]) + "…"
}
