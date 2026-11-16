package foundrynotifyworker

import (
	"fmt"
	"html/template"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/ports"
)

// message is an email before its greeting, which names its recipient: its
// subject, its paragraphs and its subscription topic (ports.Email.Topic).
type message struct {
	subject    string
	paragraphs []paragraph
	topic      string
}

// paragraph is a sentence or a few, ending with a link when Link is set:
// the text says what the link is for.
type paragraph struct {
	Text string
	Link string
}

// signature closes every email, a line each.
var signature = []string{"Xav", "Helix Foundry"}

// site builds the links the emails take people to, on the site's origin
// (foundry.notify.site_url). Every id is escaped.
type site string

// ask is where a question of the ontology is asked.
func (s site) ask(workspaceID, ontologyID string) string {
	return string(s) + "/ask/" + url.PathEscape(ontologyID) + "?workspace=" + url.QueryEscape(workspaceID)
}

// overview is the ontology's page: its versions, loads and what it holds.
func (s site) overview(workspaceID, ontologyID string) string {
	return string(s) + "/ontology/" + url.PathEscape(ontologyID) + "?workspace=" + url.QueryEscape(workspaceID)
}

// home is the site's front page, marked as reached from a reminder
// (?via=email), which the page welcomes back.
func (s site) home() string {
	return string(s) + "/?via=email"
}

// automation is an automation's page.
func (s site) automation(workspaceID, automationID string) string {
	return string(s) + "/automations/" + url.PathEscape(automationID) + "?workspace=" + url.QueryEscape(workspaceID)
}

// automationRun is a run's page, its steps on the automation's canvas.
func (s site) automationRun(workspaceID, automationID, runID string) string {
	return string(s) + "/automations/" + url.PathEscape(automationID) + "/runs/" + url.PathEscape(runID) + "?workspace=" + url.QueryEscape(workspaceID)
}

// connect is where a new ontology starts from the workspace's own data.
func (s site) connect(workspaceID string) string {
	return string(s) + "/ontology/new?workspace=" + url.QueryEscape(workspaceID)
}

// ontologyName is how the emails call an ontology: its name on one line, or
// "your ontology" without one, and the same to start a sentence with.
func ontologyName(name string) (mid, start string) {
	name = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, name)), " ")
	if name == "" {
		return "your ontology", "Your ontology"
	}
	return name, name
}

// sentence is a customer-facing error as a sentence of its own: its first
// letter upper case, ending with a full stop unless it ends with other
// punctuation. "" stays "".
func sentence(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	first, size := utf8.DecodeRuneInString(text)
	text = string(unicode.ToUpper(first)) + text[size:]
	if last, _ := utf8.DecodeLastRuneInString(text); !strings.ContainsRune(".!?", last) {
		text += "."
	}
	return text
}

// emailHTML is the HTML part: the same paragraphs as the text, inline styled
// for mail clients, its links as links. html/template escapes every value.
var emailHTML = template.Must(template.New("email").Parse(`<!DOCTYPE html>
<html>
<body style="margin:0;padding:24px;background-color:#ffffff;">
<div style="max-width:560px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;font-size:15px;line-height:1.6;color:#1f2328;">
<p style="margin:0 0 16px;">{{.Greeting}}</p>
{{- range .Paragraphs}}
<p style="margin:0 0 16px;">{{.Text}}{{with .Link}} <a href="{{.}}" style="color:#0b57d0;">{{.}}</a>{{end}}</p>
{{- end}}
<p style="margin:24px 0 0;">{{range $i, $line := .Signature}}{{if $i}}<br>{{end}}{{$line}}{{end}}</p>
</div>
</body>
</html>
`))

// render writes m to a person whose first name is firstName ("" greets no
// one by name), as text and as HTML.
func render(firstName string, m message) (ports.Email, error) {
	greeting := "Hi,"
	if name := strings.TrimSpace(firstName); name != "" {
		greeting = "Hi " + name + ","
	}
	var text strings.Builder
	text.WriteString(greeting + "\n\n")
	for _, p := range m.paragraphs {
		text.WriteString(p.Text)
		if p.Link != "" {
			text.WriteString(" " + p.Link)
		}
		text.WriteString("\n\n")
	}
	text.WriteString(strings.Join(signature, "\n") + "\n")
	var html strings.Builder
	if err := emailHTML.Execute(&html, struct {
		Greeting   string
		Paragraphs []paragraph
		Signature  []string
	}{greeting, m.paragraphs, signature}); err != nil {
		return ports.Email{}, err
	}
	return ports.Email{Subject: m.subject, Text: text.String(), HTML: html.String(), Topic: m.topic}, nil
}

// loadReady is a first draft's email once its load is ready.
func loadReady(start, ask, overview string) message {
	return message{subject: start + " is ready to ask", paragraphs: []paragraph{
		{Text: start + " is loaded and ready. Foundry drafted it from your data, made it current and loaded it into its Helix database."},
		{Text: "Ask it a question:", Link: ask},
		{Text: "See what it holds:", Link: overview},
	}}
}

// loadPartial is a first draft's email once its load stopped at the month's budget.
func loadPartial(start, ask, overview string) message {
	return message{subject: start + " is ready to ask", paragraphs: []paragraph{
		{Text: start + " is loaded, though the load stopped at this month's budget, so some of its documents weren't read."},
		{Text: "Ask it a question:", Link: ask},
		{Text: "See the load:", Link: overview},
	}}
}

// loadFailed is a first draft's email once its load failed, as failure says.
func loadFailed(name, start, failure, overview string) message {
	first := "Foundry drafted " + name + " and made it current, but its load failed."
	if failure = sentence(failure); failure != "" {
		first = "Foundry drafted " + name + " and made it current, but its load failed: " + failure
	}
	return message{subject: start + " couldn't be loaded", paragraphs: []paragraph{
		{Text: first},
		{Text: "Open it to try again:", Link: overview},
	}}
}

// draftFailed is a first draft's email once its generation failed, as
// failure says.
func draftFailed(name, start, failure, overview string) message {
	first := "Foundry's agent couldn't draft " + name + "."
	if failure = sentence(failure); failure != "" {
		first = "Foundry's agent couldn't draft " + name + ": " + failure
	}
	return message{subject: start + " couldn't be drafted", paragraphs: []paragraph{
		{Text: first},
		{Text: "Open it to try again:", Link: overview},
	}}
}

// draftOnly is a first draft's email when it queued no load: the draft
// waits for review.
func draftOnly(name, start, overview string) message {
	return message{subject: start + "'s first draft is ready", paragraphs: []paragraph{
		{Text: "Foundry's agent drafted " + name + ". Review the draft and make it current, then load it:", Link: overview},
	}}
}

// draftQuestions is a first draft's email once its agent asked questions
// the draft waits on, which take the agent's defaults at due (UTC).
func draftQuestions(name, start string, due time.Time, overview string) message {
	return message{subject: start + " has questions for you", paragraphs: []paragraph{
		{Text: "Foundry's agent is drafting " + name + " and needs a few answers from you before it can finish."},
		{Text: "Answer them:", Link: overview},
		{Text: "If no one answers by " + due.UTC().Format("January 2") + ", it carries on with its own best guesses."},
	}}
}

// sampleFollowUp asks a person who watched the sample to try their own
// data; they didn't ask for it, so it goes under ports.TopicFoundryTips.
func sampleFollowUp(connect string) message {
	return message{subject: "Try Foundry on your own data", topic: ports.TopicFoundryTips, paragraphs: []paragraph{
		{Text: "Yesterday you watched Foundry turn Acme Corp's database and documents into an ontology you could ask questions of. It does the same with yours."},
		{Text: "Connect a database or an app, or upload documents, and say what the ontology should serve. Foundry drafts it, loads it and emails you when it's ready, in a few minutes."},
		{Text: "Connect your data:", Link: connect},
		{Text: "If you get stuck, reply to this email and I'll help."},
	}}
}

// reminderEmail is the nth reminder (from 1) a person asked for on the
// landing page, from a phone, to try Foundry on a computer, linking home;
// last says it is the schedule's last, which it then says. It goes under
// ports.TopicFoundryReminder, so its person can leave the reminders and
// keep Foundry's other emails.
func reminderEmail(n int, last bool, home string) message {
	m := message{topic: ports.TopicFoundryReminder}
	switch {
	case n == 1:
		m.subject = "Your reminder to try Helix Foundry"
		m.paragraphs = []paragraph{
			{Text: "You asked for a reminder to try Helix Foundry on your computer, so here it is."},
			{Text: "Foundry connects your company's data, from your database and apps to your documents, into one ontology you can explore and ask questions of."},
			{Text: "The quickest way in is the sample company: Foundry turns its database and documents into an ontology in a couple of minutes, ready for your questions. Open Foundry on your computer:", Link: home},
		}
	case last:
		m.subject = "One last reminder to try Helix Foundry"
		m.paragraphs = []paragraph{
			{Text: "Here's the last reminder you asked for to try Helix Foundry on your computer."},
			{Text: "Whenever you have a few minutes at your computer, Foundry is here, with a sample company to try it on before you connect your own data:", Link: home},
		}
	case n == 2:
		m.subject = "Still want to try Helix Foundry?"
		m.paragraphs = []paragraph{
			{Text: "You asked for a reminder to try Helix Foundry on your computer, and here's another."},
			{Text: "Connect a database or an app, or upload documents, and Foundry drafts an ontology from them and loads it, in a few minutes. Or start with the sample company, which needs nothing of yours:", Link: home},
		}
	default:
		m.subject = "A reminder to try Helix Foundry"
		m.paragraphs = []paragraph{
			{Text: "Here's your reminder to try Helix Foundry on your computer."},
			{Text: "Ask Foundry about your own data in plain words, and get answers you can check: each one cites the rows and documents it read. Open Foundry:", Link: home},
		}
	}
	if last {
		m.paragraphs = append(m.paragraphs, paragraph{Text: "I won't send another reminder."})
	}
	m.paragraphs = append(m.paragraphs, paragraph{Text: "If you get stuck, reply to this email and I'll help."})
	return m
}

// links finds what reads as a link in text a model wrote, or what a mail
// client would make one of: an automation's Assistant step's summary, which
// may quote what it read. It takes a URL with a scheme (or one of the schemes
// written without slashes, such as mailto:), www., an email address, an IPv4
// address, and a bare domain: labels, a dot and a top-level domain of
// letters, with any port or path after it. A file name such as report.pdf
// reads as a domain too, and goes with them: Gmail and Outlook link names
// under top-level domains like .zip and .mov.
var links = regexp.MustCompile(`(?i)` +
	`(?:\b[a-z][a-z0-9+.-]*://|\bwww\.)\S*` +
	`|\b(?:mailto|tel|sms|callto|javascript|data|file|news|xmpp):\S+` +
	`|[\p{L}\p{N}._%+-]+@[\p{L}\p{N}-]+(?:\.[\p{L}\p{N}-]+)+` +
	`|\b\d{1,3}(?:\.\d{1,3}){3}(?:[:/?#]\S*)?` +
	`|(?:[\p{L}\p{N}](?:[\p{L}\p{N}-]*[\p{L}\p{N}])?\.)+(?:xn--[a-z0-9-]+|\p{L}{2,63})(?:[:/?#]\S*)?`)

// unlinked is a model's text for an email: on one line per paragraph, with
// every link taken out, so an email never carries a link that text a
// workspace's documents planted in an answer put there.
func unlinked(text string) string {
	return strings.TrimSpace(links.ReplaceAllString(text, "[link removed]"))
}

// automationNoticeName is how the emails call an automation: its name on one
// line, or "your automation" without one.
func automationNoticeName(name string) string {
	mid, _ := ontologyName(name)
	if mid == "your ontology" {
		return "your automation"
	}
	return mid
}

// automationRunEmail is an automation's notify step's email: its message, what
// each Assistant step before it said, and the actions they left for the
// owner, then the run's page.
func automationRunEmail(name, note string, said []string, pending int, run string) message {
	m := message{subject: name + ": " + firstLine(note, "what it did")}
	if note = strings.TrimSpace(note); note != "" {
		m.paragraphs = append(m.paragraphs, paragraph{Text: note})
	}
	for _, s := range said {
		if s = unlinked(s); s != "" {
			m.paragraphs = append(m.paragraphs, paragraph{Text: "Founder: " + s})
		}
	}
	switch {
	case pending == 1:
		m.paragraphs = append(m.paragraphs, paragraph{Text: "One action waits for you: open the run to see it and run it, or not."})
	case pending > 1:
		m.paragraphs = append(m.paragraphs, paragraph{Text: fmt.Sprintf("%d actions wait for you: open the run to see them and run them, or not.", pending)})
	}
	m.paragraphs = append(m.paragraphs, paragraph{Text: "See every step of the run:", Link: run})
	return m
}

// automationPausedEmail tells an automation's owner it paused itself after
// failures failed runs in a row, the last saying why.
func automationPausedEmail(name string, failures int, why, page string) message {
	first := fmt.Sprintf("Your automation %s failed %d times in a row, so Foundry paused it.", name, failures)
	if why = sentence(unlinked(why)); why != "" {
		first += " Its last run said: " + why
	}
	return message{subject: name + " paused after failing", paragraphs: []paragraph{
		{Text: first},
		{Text: "Fix what it needs, then resume it from its page:", Link: page},
	}}
}

// firstLine is text's first line, cut to a subject's length, or fallback for
// none.
func firstLine(text, fallback string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return fallback
	}
	if utf8.RuneCountInString(line) > 80 {
		return string([]rune(line)[:79]) + "…"
	}
	return line
}
