// Package suggest is what a suggestion for a new automation is, and how one
// is written for an ontology's current version: the four prompts the new
// automation page offers, each filling its composer for a person to edit and
// send, never run.
//
// Founder's worker writes them once per version on the workspace's
// cheapest task model (Prompt, then Parse of the model's one tool call);
// until it has, and wherever it can't, the API offers Fallback, made from the
// version's competency questions without a model. A model wrote what Parse
// keeps, from data people and other models wrote: it is checked as untrusted
// text, and only ever fills a composer.
//
// Nothing here talks to a store, a model or the network.
package suggest

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// PerHour is how many writes a workspace may queue in an hour.
	PerHour = 20
	// MaxSuggestions is the most suggestions a version has.
	MaxSuggestions = 4
	// MaxTitle and MaxPrompt bound a suggestion's title and prompt, in
	// characters (runes).
	MaxTitle  = 32
	MaxPrompt = 400
	// Timeout bounds a write's model calls, its retries included.
	Timeout = 60 * time.Second
	// MaxOutputTokens is the most one call may write.
	MaxOutputTokens = 1500
	// RetryAfter is how long a failed write stands before a read queues it
	// again.
	RetryAfter = time.Hour
	// StaleAfter is how long after it was queued a write still counts as
	// under way: past it, the worker is taken to be down, and readers stop
	// waiting for it.
	StaleAfter = 2 * time.Minute
)

// Icon is the kind of thing a suggestion does, for its icon: a closed set.
type Icon string

const (
	// Briefing is a regular summary.
	Briefing Icon = "briefing"
	// Watch keeps an eye on something coming up.
	Watch Icon = "watch"
	// Check checks new data against a standard.
	Check Icon = "check"
	// Alert tells someone when something goes wrong.
	Alert Icon = "alert"
	// Refresh keeps data fresh: syncs and loads.
	Refresh Icon = "refresh"
	// Report answers a question and sends the answer.
	Report Icon = "report"
)

// Icons is every Icon, in the API enum's order.
var Icons = []Icon{Briefing, Watch, Check, Alert, Refresh, Report}

// Valid reports whether i is one of Icons.
func (i Icon) Valid() bool {
	switch i {
	case Briefing, Watch, Check, Alert, Refresh, Report:
		return true
	}
	return false
}

// Suggestion is one prompt for a new automation, as the store keeps it.
type Suggestion struct {
	// Title is an outcome, 1 to MaxTitle characters: "Renewal radar".
	Title string `json:"title"`
	// Prompt says when it starts and what it does, in plain words: 1 to
	// MaxPrompt characters.
	Prompt string `json:"prompt"`
	Icon   Icon   `json:"icon"`
}

// ErrInvalid is a model's answer Parse keeps nothing of.
var ErrInvalid = errors.New("suggest: no valid suggestion")

// interrogatives are the leading words a fallback title drops, when the rest
// still reads: longest first, so "How many" is tried before any one word.
var interrogatives = []string{"How many", "Which", "What"}

// auxiliaries are the words after an interrogative that mean the rest
// doesn't read without it ("What does each term cost").
var auxiliaries = map[string]bool{
	"is": true, "are": true, "was": true, "were": true, "do": true, "does": true, "did": true, "has": true, "have": true,
	"had": true, "can": true, "could": true, "will": true, "would": true, "should": true, "shall": true, "may": true,
	"might": true, "must": true,
}

// copulas and determiners: an interrogative followed by a copula and a
// determiner drops all but the determiner's phrase ("What is the total
// revenue" is "The total revenue").
var (
	copulas     = map[string]bool{"is": true, "are": true, "was": true, "were": true}
	determiners = map[string]bool{
		"the": true, "each": true, "every": true, "a": true, "an": true, "our": true, "my": true, "your": true,
		"their": true, "its": true, "this": true, "these": true, "those": true, "all": true,
	}
)

// trailing are the words a fallback title never ends on.
var trailing = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "and": true, "or": true, "to": true, "in": true, "for": true,
	"with": true, "on": true, "by": true, "at": true,
}

// maxFallbackName bounds the ontology's name in a fallback prompt.
const maxFallbackName = 100

// Fallback is the suggestions made from a version's competency questions,
// without a model: for each of the first MaxSuggestions questions, a report
// that answers it every Monday by email. A title another question's already
// took is skipped; no questions, no suggestions.
//
// A title is the question in a few words (Title); the prompt quotes the
// question whole, cut with "…" only where the prompt would pass MaxPrompt.
func Fallback(ontologyName string, questions []string) []Suggestion {
	name := cut(collapse(ontologyName), maxFallbackName)
	out := []Suggestion{}
	seen := map[string]bool{}
	for _, q := range questions {
		if len(out) == MaxSuggestions {
			break
		}
		q = collapse(q)
		title := Title(q)
		if title == "" || seen[strings.ToLower(title)] {
			continue
		}
		seen[strings.ToLower(title)] = true
		before, after := `Every Monday, answer: "`, `" from `+name+`, and email me the answer.`
		room := MaxPrompt - utf8.RuneCountInString(before) - utf8.RuneCountInString(after)
		out = append(out, Suggestion{Title: title, Prompt: before + cut(q, room) + after, Icon: Report})
	}
	return out
}

// Title is a competency question in at most MaxTitle characters, as Fallback
// names its suggestion:
//  1. its whitespace collapsed, and trailing punctuation (?.!,;:) dropped;
//  2. a leading "How many", "Which" or "What" dropped, unless the next word
//     is an auxiliary ("What plans do customers hold" drops it; "What does
//     each term cost" keeps it) or nothing follows; followed by "is", "are",
//     "was" or "were" and then a determiner such as "the" or "each", both
//     words go ("What is the total revenue" is "The total revenue");
//  3. cut after the last whole word that fits, then trailing words such as
//     "the" and "of" and punctuation dropped (a first word longer than the
//     limit is cut at it);
//  4. its first letter capitalised.
func Title(question string) string {
	q := strings.TrimRight(collapse(question), "?.!,;: ")
	for _, word := range interrogatives {
		rest, ok := cutPrefixFold(q, word+" ")
		if !ok {
			continue
		}
		next, after, _ := strings.Cut(rest, " ")
		then, _, _ := strings.Cut(after, " ")
		switch {
		case copulas[strings.ToLower(next)] && determiners[strings.ToLower(then)]:
			q = after
		case rest != "" && !auxiliaries[strings.ToLower(next)]:
			q = rest
		}
		break
	}
	words := strings.Fields(q)
	if len(words) == 0 {
		return ""
	}
	kept := []string{}
	size := 0
	for _, w := range words {
		n := utf8.RuneCountInString(w)
		if len(kept) > 0 {
			n++
		}
		if size+n > MaxTitle {
			break
		}
		kept, size = append(kept, w), size+n
	}
	if len(kept) == 0 {
		kept = []string{string([]rune(words[0])[:MaxTitle])}
	}
	for len(kept) > 1 && trailing[strings.ToLower(strings.TrimRight(kept[len(kept)-1], ",;:"))] {
		kept = kept[:len(kept)-1]
	}
	title := strings.TrimRight(strings.Join(kept, " "), "?.!,;: ")
	if title == "" {
		return ""
	}
	first, size1 := utf8.DecodeRuneInString(title)
	return string(unicode.ToUpper(first)) + title[size1:]
}

// cutPrefixFold is strings.CutPrefix ignoring case.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

// collapse trims s and makes every run of whitespace one space.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// cut is s in at most limit characters, ending "…" when cut.
func cut(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	if limit < 1 {
		return ""
	}
	return strings.TrimRight(string(r[:limit-1]), " ") + "…"
}

var (
	// ulid is a 26-character Crockford base32 token with a digit: Foundry's
	// ids, never a word.
	ulid = regexp.MustCompile(`(?i)\b[0-9A-HJKMNP-TV-Z]{26}\b`)
	// prefixedID is an id such as src_1a2b3c4d: a short prefix, then a
	// token holding a digit, or any src_ or ont_ token.
	prefixedID = regexp.MustCompile(`(?i)\b[a-z]{2,10}_[a-z0-9_-]*[0-9][a-z0-9_-]*\b|\b(src|ont)_\w+`)
	// markdownLink is [text](target).
	markdownLink = regexp.MustCompile(`\[[^\]]*\]\(`)
)

// clean is s as a suggestion's field keeps it, its whitespace collapsed, or
// why it can't be one: empty or past limit characters, a control character
// (a newline among them) or an invisible format character (a bidi override,
// a zero-width space: text that reads otherwise than it holds), a link,
// markup, or an id.
func clean(s string, limit int) (string, error) {
	for _, r := range s {
		switch {
		case unicode.IsControl(r):
			return "", fmt.Errorf("a control character")
		case unicode.Is(unicode.Cf, r):
			return "", fmt.Errorf("an invisible format character")
		}
	}
	s = collapse(s)
	lower := strings.ToLower(s)
	switch n := utf8.RuneCountInString(s); {
	case n == 0:
		return "", fmt.Errorf("empty")
	case n > limit:
		return "", fmt.Errorf("%d characters, past %d", n, limit)
	case strings.Contains(lower, "http") || strings.Contains(lower, "www.") || strings.Contains(lower, "://"):
		return "", fmt.Errorf("a link")
	case strings.ContainsAny(s, "<>`") || strings.Contains(s, "**") || strings.HasPrefix(s, "#") || markdownLink.MatchString(s):
		return "", fmt.Errorf("markup")
	}
	for _, token := range ulid.FindAllString(s, -1) {
		if strings.ContainsAny(token, "0123456789") {
			return "", fmt.Errorf("an id")
		}
	}
	if prefixedID.MatchString(s) {
		return "", fmt.Errorf("an id")
	}
	return s, nil
}
