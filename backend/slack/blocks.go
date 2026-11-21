package slack

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Block Kit's limits the bot keeps to.
const (
	MaxBlocks         = 50   // blocks in a message
	MaxSectionText    = 3000 // a section's text
	MaxSectionFields  = 10   // a section's fields
	MaxFieldText      = 2000 // a section field's text
	MaxContextItems   = 10   // a context block's elements
	MaxHeaderText     = 150  // a header's text
	MaxButtonText     = 75   // a button's text
	MaxActionElements = 25   // an actions block's buttons
	MaxMessageText    = 3000 // a message's notification text, as the bot cuts it
)

// Plain is plain_text: shown as written, emoji codes aside.
type Plain string

// MarshalJSON writes a plain_text object.
func (p Plain) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type  string `json:"type"`
		Text  string `json:"text"`
		Emoji bool   `json:"emoji"`
	}{"plain_text", string(p), true})
}

// MarshalJSON writes a mrkdwn text object with verbatim set, so Slack
// neither links URLs and channel names nor reads mentions in it: the text
// says exactly what Site.Text made it say.
func (m Mrkdwn) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Verbatim bool   `json:"verbatim"`
	}{"mrkdwn", m.s, true})
}

// Block is a Block Kit layout block: a closed set.
type Block interface {
	slackBlock()
	check() error
}

// Section is text, with fields beside it and perhaps a button.
type Section struct {
	Text      Mrkdwn
	Fields    []Mrkdwn
	Accessory *Button
}

// Context is a line of small text.
type Context struct{ Elements []Mrkdwn }

// Header is a large plain-text title.
type Header struct{ Text Plain }

// Divider is a rule.
type Divider struct{}

// Actions is a row of buttons.
type Actions struct{ Elements []Button }

func (Section) slackBlock() {}
func (Context) slackBlock() {}
func (Header) slackBlock()  {}
func (Divider) slackBlock() {}
func (Actions) slackBlock() {}

// ButtonStyle colours a button.
type ButtonStyle string

// The styles a button takes; the default is neither.
const (
	StyleDefault ButtonStyle = ""
	StylePrimary ButtonStyle = "primary"
	StyleDanger  ButtonStyle = "danger"
)

// Button is a button: one that acts (ActionID and Value, which
// ParseInteraction reads) or one that opens a page of the site (URL).
type Button struct {
	ActionID string
	Text     Plain
	Value    ButtonValue
	// URL, when set, opens that page; Slack still reports the click.
	URL     *Link
	Style   ButtonStyle
	Confirm *Confirm
}

// Confirm asks before a button acts.
type Confirm struct {
	Title   Plain
	Text    Mrkdwn
	Confirm Plain
	Deny    Plain
	Style   ButtonStyle
}

// MarshalJSON writes a section block.
func (s Section) MarshalJSON() ([]byte, error) {
	var text *Mrkdwn
	if s.Text.s != "" {
		text = &s.Text
	}
	return json.Marshal(struct {
		Type      string   `json:"type"`
		Text      *Mrkdwn  `json:"text,omitempty"`
		Fields    []Mrkdwn `json:"fields,omitempty"`
		Accessory *Button  `json:"accessory,omitempty"`
	}{"section", text, s.Fields, s.Accessory})
}

// MarshalJSON writes a context block.
func (c Context) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type     string   `json:"type"`
		Elements []Mrkdwn `json:"elements"`
	}{"context", c.Elements})
}

// MarshalJSON writes a header block.
func (h Header) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
		Text Plain  `json:"text"`
	}{"header", h.Text})
}

// MarshalJSON writes a divider block.
func (Divider) MarshalJSON() ([]byte, error) { return []byte(`{"type":"divider"}`), nil }

// MarshalJSON writes an actions block.
func (a Actions) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type     string   `json:"type"`
		Elements []Button `json:"elements"`
	}{"actions", a.Elements})
}

// MarshalJSON writes a button element.
func (b Button) MarshalJSON() ([]byte, error) {
	var value, url string
	if b.URL != nil {
		url = b.URL.u
	}
	if v := b.Value.String(); v != "{}" {
		value = v
	}
	return json.Marshal(struct {
		Type     string      `json:"type"`
		ActionID string      `json:"action_id"`
		Text     Plain       `json:"text"`
		Value    string      `json:"value,omitempty"`
		URL      string      `json:"url,omitempty"`
		Style    ButtonStyle `json:"style,omitempty"`
		Confirm  *Confirm    `json:"confirm,omitempty"`
	}{"button", b.ActionID, b.Text, value, url, b.Style, b.Confirm})
}

// MarshalJSON writes a confirmation dialog.
func (c Confirm) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Title   Plain       `json:"title"`
		Text    Mrkdwn      `json:"text"`
		Confirm Plain       `json:"confirm"`
		Deny    Plain       `json:"deny"`
		Style   ButtonStyle `json:"style,omitempty"`
	}{c.Title, c.Text, c.Confirm, c.Deny, c.Style})
}

func (s Section) check() error {
	switch {
	case s.Text.s == "" && len(s.Fields) == 0:
		return errors.New("a section with no text")
	case s.Text.Len() > MaxSectionText:
		return fmt.Errorf("a section's text of %d characters", s.Text.Len())
	case len(s.Fields) > MaxSectionFields:
		return fmt.Errorf("a section with %d fields", len(s.Fields))
	}
	for _, f := range s.Fields {
		if f.s == "" || f.Len() > MaxFieldText {
			return fmt.Errorf("a section field of %d characters", f.Len())
		}
	}
	if s.Accessory != nil {
		return s.Accessory.check()
	}
	return nil
}

func (c Context) check() error {
	if len(c.Elements) == 0 || len(c.Elements) > MaxContextItems {
		return fmt.Errorf("a context block with %d elements", len(c.Elements))
	}
	for _, e := range c.Elements {
		if e.s == "" || e.Len() > MaxSectionText {
			return fmt.Errorf("a context element of %d characters", e.Len())
		}
	}
	return nil
}

func (h Header) check() error {
	if n := utf8.RuneCountInString(string(h.Text)); n == 0 || n > MaxHeaderText {
		return fmt.Errorf("a header of %d characters", n)
	}
	return nil
}

func (Divider) check() error { return nil }

func (a Actions) check() error {
	if len(a.Elements) == 0 || len(a.Elements) > MaxActionElements {
		return fmt.Errorf("an actions block with %d buttons", len(a.Elements))
	}
	for _, b := range a.Elements {
		if err := b.check(); err != nil {
			return err
		}
	}
	return nil
}

func (b Button) check() error {
	n := utf8.RuneCountInString(string(b.Text))
	switch {
	case !strings.HasPrefix(b.ActionID, "foundry."):
		return fmt.Errorf("a button with action_id %q", b.ActionID)
	case n == 0 || n > MaxButtonText:
		return fmt.Errorf("a button text of %d characters", n)
	case len(b.Value.String()) > MaxValue:
		return errors.New("a button value too long")
	case b.Style != StyleDefault && b.Style != StylePrimary && b.Style != StyleDanger:
		return fmt.Errorf("a button style %q", b.Style)
	case b.Confirm != nil && (b.Confirm.Title == "" || b.Confirm.Text.s == "" || b.Confirm.Confirm == "" || b.Confirm.Deny == ""):
		return errors.New("a confirmation without its words")
	}
	return nil
}

// Message is what the bot posts, updates or sends ephemerally: blocks, and
// the notification text Slack shows where it can't show them.
type Message struct {
	Text   Mrkdwn
	Blocks []Block
}

// Check refuses a message Slack would: no notification text, a notification
// text over MaxMessageText, more than MaxBlocks blocks, or a block past
// Block Kit's limits.
func (m Message) Check() error {
	switch {
	case m.Text.s == "":
		return errors.New("slack: a message without notification text")
	case m.Text.Len() > MaxMessageText:
		return fmt.Errorf("slack: notification text of %d characters", m.Text.Len())
	case len(m.Blocks) > MaxBlocks:
		return fmt.Errorf("slack: a message of %d blocks", len(m.Blocks))
	}
	for i, b := range m.Blocks {
		if b == nil {
			return fmt.Errorf("slack: block %d is nil", i)
		}
		if err := b.check(); err != nil {
			return fmt.Errorf("slack: block %d: %w", i, err)
		}
	}
	return nil
}
