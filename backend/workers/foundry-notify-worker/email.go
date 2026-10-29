package foundrynotifyworker

import (
	"html/template"
	"net/url"
	"strings"
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
