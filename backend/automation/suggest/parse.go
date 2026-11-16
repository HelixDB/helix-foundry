package suggest

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToolName is the one tool a write's call is asked to call: its input is the
// answer.
const ToolName = "suggest_automations"

// ToolDescription says what ToolName records.
const ToolDescription = "Records exactly 4 suggested automations for this ontology. Call it once."

// ToolSchema is ToolName's input: exactly MaxSuggestions suggestions, each a
// title, a prompt and an icon.
var ToolSchema = json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"suggestions":{"type":"array","minItems":%[1]d,"maxItems":%[1]d,`+
	`"items":{"type":"object","properties":{`+
	`"title":{"type":"string","maxLength":%[2]d,"description":"A short outcome, such as Renewal radar."},`+
	`"prompt":{"type":"string","maxLength":%[3]d,"description":"In plain words: when it starts and what it does."},`+
	`"icon":{"type":"string","enum":["briefing","watch","check","alert","refresh","report"]}},`+
	`"required":["title","prompt","icon"]}}},"required":["suggestions"]}`, MaxSuggestions, MaxTitle, MaxPrompt))

// Parse reads ToolName's input as a model wrote it, keeping the first
// MaxSuggestions suggestions that pass, in order, each title and prompt with
// its whitespace collapsed. A suggestion is dropped when its title isn't 1 to
// MaxTitle characters or its prompt 1 to MaxPrompt; either holds a control
// character (a newline among them), a link, markup (<, >, a backtick, **, a
// leading #, a Markdown link) or an id (a ULID, src_…); its icon isn't one of
// Icons; or its title is another kept one's, case aside. ErrInvalid means
// none passed, or the input isn't {"suggestions": [...]}.
func Parse(raw json.RawMessage) ([]Suggestion, error) {
	var answer struct {
		Suggestions []json.RawMessage `json:"suggestions"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	out := []Suggestion{}
	seen := map[string]bool{}
	var problems []string
	for i, item := range answer.Suggestions {
		if len(out) == MaxSuggestions {
			break
		}
		var s struct{ Title, Prompt, Icon string }
		if err := json.Unmarshal(item, &s); err != nil {
			problems = append(problems, fmt.Sprintf("%d: not an object of strings", i))
			continue
		}
		title, err := clean(s.Title, MaxTitle)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%d: title: %v", i, err))
			continue
		}
		prompt, err := clean(s.Prompt, MaxPrompt)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%d: prompt: %v", i, err))
			continue
		}
		icon := Icon(s.Icon)
		switch {
		case !icon.Valid():
			problems = append(problems, fmt.Sprintf("%d: icon %q", i, s.Icon))
			continue
		case seen[strings.ToLower(title)]:
			problems = append(problems, fmt.Sprintf("%d: title repeated", i))
			continue
		}
		seen[strings.ToLower(title)] = true
		out = append(out, Suggestion{Title: title, Prompt: prompt, Icon: icon})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, strings.Join(append([]string{fmt.Sprintf("%d given", len(answer.Suggestions))}, problems...), "; "))
	}
	return out, nil
}
