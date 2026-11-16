package suggest

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ToolName is the one tool a write's call is asked to call: its input is the
// answer.
const ToolName = "suggest_for_founder"

// ToolDescription says what ToolName records.
const ToolDescription = "Records the suggestions for this ontology: 2 questions, 2 dashboards, 2 changes to the ontology and 4 workflows. Call it once."

// fields are the tool input's field of each kind, in Kinds' order.
var fields = map[Kind]string{Question: "questions", Dashboard: "dashboards", Ontology: "ontologies", Workflow: "workflows"}

// fieldDescriptions say what each kind's field holds.
var fieldDescriptions = map[Kind]string{
	Question:  "Questions for the ontology's data, which Founder answers with citations.",
	Dashboard: `Dashboards Founder makes, each prompt starting like "Build me a dashboard of".`,
	Ontology:  `Changes that make the ontology more useful, each prompt starting "Draft a new version".`,
	Workflow:  "Workflows that run by themselves, each prompt saying when it starts and what it does.",
}

// ToolSchema is ToolName's input: each kind's field (questions, dashboards,
// ontologies, workflows) a list of exactly its Count suggestions, each a
// title, a prompt and a topic, and a workflow's an icon too.
var ToolSchema = func() json.RawMessage {
	properties := map[string]any{}
	required := []string{}
	for _, kind := range Kinds {
		item := map[string]any{
			"title":  map[string]any{"type": "string", "maxLength": MaxTitle, "description": "A short outcome, such as Renewal radar."},
			"prompt": map[string]any{"type": "string", "maxLength": MaxPrompt, "description": "What the owner would type to Founder, in plain words."},
			"topic":  map[string]any{"type": "string", "maxLength": MaxTopic, "description": "What it is about in one or two words, such as Revenue or Data quality."},
		}
		needs := []string{"title", "prompt", "topic"}
		if kind == Workflow {
			item["icon"] = map[string]any{"type": "string", "enum": Icons}
			needs = append(needs, "icon")
		}
		properties[fields[kind]] = map[string]any{
			"type": "array", "minItems": kind.Count(), "maxItems": kind.Count(), "description": fieldDescriptions[kind],
			"items": map[string]any{"type": "object", "properties": item, "required": needs},
		}
		required = append(required, fields[kind])
	}
	raw, err := json.Marshal(map[string]any{"type": "object", "properties": properties, "required": required})
	if err != nil {
		panic(err)
	}
	return raw
}()

// Parse reads ToolName's input as a model wrote it, keeping of each kind's
// field the first Count suggestions that pass Suggestion.Clean, in order,
// kinds in Kinds' order. A suggestion is also dropped when its title or
// prompt is another kept one's of its kind, case aside. A field that is
// missing or isn't a list keeps nothing of its kind. ErrInvalid means no
// kind kept anything, or the input isn't an object.
func Parse(raw json.RawMessage) ([]Suggestion, error) {
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	out := []Suggestion{}
	var problems []string
	for _, kind := range Kinds {
		var items []json.RawMessage
		if err := json.Unmarshal(answer[fields[kind]], &items); err != nil || len(items) == 0 {
			problems = append(problems, fields[kind]+": none")
			continue
		}
		seen := map[string]bool{}
		kept := 0
		for i, item := range items {
			if kept == kind.Count() {
				break
			}
			var s struct{ Title, Prompt, Topic, Icon string }
			if err := json.Unmarshal(item, &s); err != nil {
				problems = append(problems, fmt.Sprintf("%s %d: not an object of strings", fields[kind], i))
				continue
			}
			clean, err := Suggestion{Kind: kind, Title: s.Title, Prompt: s.Prompt, Topic: s.Topic, Icon: Icon(s.Icon), Source: SourceModel}.Clean()
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s %d: %v", fields[kind], i, err))
				continue
			}
			title, prompt := "title:"+strings.ToLower(clean.Title), "prompt:"+strings.ToLower(clean.Prompt)
			if seen[title] || seen[prompt] {
				problems = append(problems, fmt.Sprintf("%s %d: repeated", fields[kind], i))
				continue
			}
			seen[title], seen[prompt] = true, true
			out = append(out, clean)
			kept++
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrInvalid, strings.Join(problems, "; "))
	}
	return out, nil
}
