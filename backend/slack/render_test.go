package slack_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/agentquestion"
	"github.com/helixdb/helix-foundry/backend/slack"
)

// regexpLinks are the links mrkdwn makes, and the URLs buttons open.
var regexpLinks = regexp.MustCompile(`<([a-z]+://[^|>]*)\||"url": "([^"]*)"`)

var updateRender = flag.Bool("update-render", false, "write the renderers' goldens in testdata/render")

// golden checks each case's messages pass Message.Check, have every mrkdwn
// object verbatim and none but the site's links, and equal
// testdata/render/<name>.json (written with -update-render).
func golden(t *testing.T, name string, cases map[string][]slack.Message) {
	t.Helper()
	type shown struct {
		Text   string        `json:"text"`
		Blocks []slack.Block `json:"blocks"`
	}
	all := map[string][]shown{}
	for c, messages := range cases {
		require.NotEmpty(t, messages, c)
		for i, m := range messages {
			require.NoError(t, m.Check(), "%s message %d", c, i)
			all[c] = append(all[c], shown{Text: m.Text.String(), Blocks: m.Blocks})
		}
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	require.NoError(t, e.Encode(all))
	requireVerbatim(t, b.Bytes())
	for _, link := range regexpLinks.FindAllStringSubmatch(b.String(), -1) {
		require.True(t, strings.HasPrefix(link[1]+link[2], origin+"/"), "a link off the site: %s", link[0])
	}
	require.NotContains(t, b.String(), "<!", "a special mention")
	path := filepath.Join("testdata", "render", name+".json")
	if *updateRender {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, b.Bytes(), 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "run go test ./slack -update-render")
	require.JSONEq(t, string(want), b.String())
}

const (
	turn  = "01JBTVRN0000000000000000T1"
	ada   = "U0ADA0001"
	grace = "U0GRACE01"
)

// replyOf folds events into a reply of turn.
func replyOf(events ...agent.Event) slack.Reply {
	r := slack.Reply{TurnID: turn}
	for _, e := range events {
		r.Apply(e)
	}
	return r
}

func TestReplyApply(t *testing.T) {
	r := slack.Reply{TurnID: turn}
	require.Nil(t, r.Apply(agent.Started{Model: "m", Attempt: 1}))
	require.Equal(t, "Thinking…", r.Status)
	require.Nil(t, r.Apply(agent.ToolCalled{ToolUseID: "t1", Name: "helix_foundry_ask", Title: "Ask the Analyst", Input: json.RawMessage(`{"secret":1}`)}))
	require.Equal(t, "Ask the Analyst", r.Status)
	require.Nil(t, r.Apply(agent.TextDelta{Step: 1, Text: "Hel"}))
	require.Nil(t, r.Apply(agent.Said{Step: 1, Text: "  "}))
	require.Nil(t, r.Apply(agent.Said{Step: 1, Text: "Hello"}))
	require.Equal(t, []string{"Hello"}, r.Said)
	require.Nil(t, r.Apply(agent.ToolReturned{ToolUseID: "t0", Name: "helix_foundry_list_ontologies"}))
	require.Nil(t, r.Apply(agent.ToolReturned{ToolUseID: "t0", Name: "helix_foundry_ask", Question: &agent.QuestionRef{}}))

	ref := agent.QuestionRef{OntologyID: "o1", ID: "q1", LoadID: "l1", ThreadID: "q1", Status: "running"}
	due := r.Apply(agent.ToolReturned{ToolUseID: "t1", Name: "helix_foundry_ask", Question: &ref})
	require.Equal(t, slack.AnswerDue{Question: ref}, due)
	require.Equal(t, "q:q1", due.Key())

	card := agent.ActionCard{ActionID: "a1", Operation: "create_load"}
	due = r.Apply(card)
	require.Equal(t, slack.CardDue{Card: card}, due)
	require.Equal(t, "a:a1", due.Key())

	asked := agent.Asked{Questions: []agentquestion.Question{{ID: "q", Text: "Which?"}}}
	due = r.Apply(asked)
	require.Equal(t, slack.QuestionsDue{TurnID: turn, Asked: asked}, due)
	require.Equal(t, "k:"+turn, due.Key())

	for _, e := range []agent.Event{agent.ActionResult{ActionID: "a1"}, agent.Answered{TurnID: turn}, agent.Reported{Summary: "s"}} {
		require.Nil(t, r.Apply(e))
	}
	require.Nil(t, r.End)
	require.Nil(t, r.Apply(agent.Finished{Status: "done", Reason: agent.StopAnswered}))
	require.Equal(t, slack.Finished{Finished: agent.Finished{Status: "done", Reason: agent.StopAnswered}}, r.End)
}

func TestEndText(t *testing.T) {
	s := site(t)
	for _, c := range []struct {
		end  slack.Ending
		want string
	}{
		{nil, ""},
		{slack.Finished{Finished: agent.Finished{Reason: agent.StopAnswered}}, ""},
		{slack.Finished{Finished: agent.Finished{Reason: agent.StopAsked}}, ""},
		{slack.Finished{Finished: agent.Finished{Reason: agent.StopSteps}}, "It stopped at its step limit. Ask it to go on."},
		{slack.Finished{Finished: agent.Finished{Reason: agent.StopWallTime}}, "It stopped at its time limit. Ask it to go on."},
		{slack.Finished{Finished: agent.Finished{Reason: agent.StopBudget}}, "It stopped at this message’s spending cap."},
		{slack.Finished{Finished: agent.Finished{Reason: agent.StopMaxTokens}}, "Its answer reached its length limit."},
		{slack.Finished{Finished: agent.Finished{Reason: agent.StopRefused}}, "The model declined to answer."},
		{slack.Finished{Finished: agent.Finished{Reason: agent.StopContext}},
			"It ran out of room in the model’s context window. Ask for less at once, or start a new thread."},
		{slack.Finished{Finished: agent.Finished{Reason: agent.StopFailed}}, "Founder couldn’t finish this answer."},
		{slack.Finished{Finished: agent.Finished{Reason: agent.StopFailed, Error: " the model <failed> at example.com "}},
			"Founder couldn’t finish this answer: the model &lt;failed&gt; at `example.com`"},
		{slack.Finished{Finished: agent.Finished{Reason: "new_reason"}}, "It stopped."},
		{slack.Stopped{}, "Stopped."},
		{slack.Stopped{By: ada}, "Stopped by <@" + ada + ">."},
	} {
		require.Equal(t, c.want, s.EndText(c.end).String(), "%#v", c.end)
	}
}

func TestReplyRenders(t *testing.T) {
	s := site(t)
	said := "Here is what I found, *Ada*: <!channel> see " + origin + "/ontology/o1?workspace=w1 and not evil.example.com/x.\n\n" +
		"Run `helix ask` to check."
	golden(t, "reply", map[string][]slack.Message{
		"before any event": s.Reply(slack.Reply{TurnID: turn}),
		"thinking":         s.Reply(replyOf(agent.Started{Model: "m", Attempt: 1})),
		"calling a tool":   s.Reply(replyOf(agent.Started{}, agent.Said{Text: said}, agent.ToolCalled{Name: "x", Title: "Read <the> graph"})),
		"answered":         s.Reply(replyOf(agent.Started{}, agent.Said{Text: said}, agent.Finished{Status: "done", Reason: agent.StopAnswered})),
		"answered with nothing said": s.Reply(replyOf(agent.Started{},
			agent.ActionCard{ActionID: "a1"}, agent.Finished{Status: "done", Reason: agent.StopAnswered})),
		"asked with nothing said": s.Reply(replyOf(agent.Started{}, agent.Asked{}, agent.Finished{Status: "done", Reason: agent.StopAsked})),
		"failed": s.Reply(replyOf(agent.Started{}, agent.Said{Text: "Looking."},
			agent.Finished{Status: "failed", Reason: agent.StopFailed, Error: "the provider is busy"})),
		"at its step limit": s.Reply(replyOf(agent.Said{Text: "Partly."}, agent.Finished{Status: "done", Reason: agent.StopSteps})),
		"stopped in slack":  s.Reply(slack.Reply{TurnID: turn, Said: []string{"Part"}, End: slack.Stopped{By: ada}}),
		"long text": s.Reply(replyOf(agent.Said{Text: strings.Repeat("A sentence of words. ", 200)},
			agent.Finished{Status: "done", Reason: agent.StopAnswered})),
	})
}

func TestReplyOverflow(t *testing.T) {
	s := site(t)
	r := slack.Reply{TurnID: turn}
	r.Apply(agent.Started{})
	for range slack.MaxReplyBlocks {
		r.Apply(agent.Said{Text: "Line."})
	}
	messages := s.Reply(r)
	require.Len(t, messages, 1)
	require.Len(t, messages[0].Blocks, slack.MaxReplyBlocks+2) // and the status and Stop

	r.Apply(agent.Said{Text: "One more."})
	messages = s.Reply(r)
	require.Len(t, messages, 2)
	first := messages[0]
	require.Len(t, first.Blocks, slack.MaxReplyBlocks)
	require.Len(t, messages[1].Blocks, 3)
	require.Equal(t, "One more.", messages[1].Text.String())

	// A full message never changes as the reply grows or ends.
	for range 3 * slack.MaxReplyBlocks {
		r.Apply(agent.Said{Text: strings.Repeat("x", 4000)})
	}
	r.Apply(agent.Finished{Reason: agent.StopAnswered})
	messages = s.Reply(r)
	require.Equal(t, first, messages[0])
	for i, m := range messages {
		require.NoError(t, m.Check(), "message %d", i)
		require.LessOrEqual(t, len(m.Blocks), slack.MaxBlocks)
	}
	last := messages[len(messages)-1].Blocks
	require.IsType(t, slack.Section{}, last[len(last)-1], "an answered reply ends with its text")
}

func TestQuestions(t *testing.T) {
	s := site(t)
	choice, err := agentquestion.New("q1", "Customers", "Is a customer an *account* or a person at acme.com?", true,
		[]agentquestion.Option{{Label: "An account", Description: "Billing accounts"}, {Label: "A person"}}, false, "An account")
	require.NoError(t, err)
	several, err := agentquestion.New("q2", "", "Which sources?", false,
		[]agentquestion.Option{{Label: "Stripe"}, {Label: "Postgres"}, {Label: "Docs"}}, true, "")
	require.NoError(t, err)
	free, err := agentquestion.New("q3", "", "What do you call a deal?", true, nil, false, "An opportunity")
	require.NoError(t, err)
	open, err := agentquestion.New("q4", "", "Anything else?", false, nil, false, "")
	require.NoError(t, err)
	many := make([]agentquestion.Question, 30)
	for i := range many {
		many[i] = open
	}
	dm, channel := slack.RepliesIn("D0ADA0001", "U0BOT0001"), slack.RepliesIn("C0GENERAL", "U0BOT0001")
	draft := &agent.AskedGeneration{OntologyID: "o1", OntologyName: "Sales", GenerationID: "g1", GenerationTitle: "Contracts", Round: 1}
	cases := map[string][]slack.Message{
		"one":                    s.Questions(agent.Asked{Questions: []agentquestion.Question{choice}}, "w1", dm),
		"four":                   s.Questions(agent.Asked{Questions: []agentquestion.Question{choice, several, free, open}}, "w1", dm),
		"one in a channel":       s.Questions(agent.Asked{Questions: []agentquestion.Question{choice}}, "w1", channel),
		"four in a channel":      s.Questions(agent.Asked{Questions: []agentquestion.Question{choice, several, free, open}}, "w1", channel),
		"one with no ReplyTo":    s.Questions(agent.Asked{Questions: []agentquestion.Question{open}}, "w1", nil),
		"a generation's":         s.Questions(agent.Asked{Questions: []agentquestion.Question{free}, Generation: draft}, "w1", channel),
		"a generation's in a DM": s.Questions(agent.Asked{Questions: []agentquestion.Question{choice, open}, Generation: draft}, "w1", dm),
		"a generation's without a title": s.Questions(agent.Asked{Questions: []agentquestion.Question{free},
			Generation: &agent.AskedGeneration{OntologyID: "o1", OntologyName: "Sales", GenerationID: "g1", Round: 1}}, "w1", dm),
		"thirty": s.Questions(agent.Asked{Questions: many}, "w1", dm),
	}
	golden(t, "questions", cases)
	require.Len(t, cases["thirty"], 2)

	intro := func(name string) string { return cases[name][0].Blocks[0].(slack.Section).Text.String() }
	// In a channel the bot sees only mentions, so a reply must mention it; a
	// DM's reply needn't.
	require.Equal(t, "Founder has a question for you. Reply in this thread mentioning <@U0BOT0001> to answer it. Without the mention, Founder won’t see your reply.",
		intro("one in a channel"))
	require.Contains(t, intro("four in a channel"), "<@U0BOT0001> to answer them, by their numbers.")
	require.Equal(t, "Founder has a question for you. Reply in this thread to answer it.", intro("one"))
	require.NotContains(t, intro("one"), "<@")
	// A draft's questions aren't answered by a reply in Slack: they link to
	// the draft's page, wherever they are asked, and never say to reply.
	for _, name := range []string{"a generation's", "a generation's in a DM", "a generation's without a title"} {
		answer := cases[name][0].Blocks[1].(slack.Actions).Elements[0]
		require.Equal(t, slack.ActionOpen, answer.ActionID, name)
		require.Equal(t, origin+"/ontology/o1/generations/g1?workspace=w1", answer.URL.String(), name)
		for _, m := range cases[name] {
			b, err := json.Marshal(m.Blocks)
			require.NoError(t, err)
			require.NotContains(t, string(b), "Reply", name)
		}
	}
	require.Equal(t, "Founder relays 2 questions from the draft of Sales (Contracts). Answer them on the draft’s page in Foundry: a reply here doesn’t reach the draft.",
		intro("a generation's in a DM"))
}
