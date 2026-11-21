package slack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
)

// The action_id of each button the bot draws. A button's value is compact
// JSON of at most MaxValue bytes naming what it acts on; a click never uses
// or keeps its response_url.
const (
	ActionApprove = "foundry.approve" // Run a person's card: {"a": action, "h": payload hash}
	ActionDismiss = "foundry.dismiss" // Dismiss a card: {"a": action}
	ActionRefresh = "foundry.refresh" // Prepare an expired card again: {"a": action}
	ActionReview  = "foundry.review"  // Review a bot's proposal (L2): {"a": action, "h": payload hash}
	ActionRun     = "foundry.run"     // Run a reviewed proposal (L2): {"a", "h", "s": reviewed spec hash, or ""}
	ActionStop    = "foundry.stop"    // Stop a turn: {"t": turn}
	ActionUnlink  = "foundry.unlink"  // Unlink the clicker's Slack account: no value
	// ActionLink is the link prompt's URL button. Slack reports its click
	// too; ParseInteraction leaves it Unhandled (the page does the work).
	ActionLink = "foundry.link"
	// ActionOpen is a URL button opening a page of the site where something
	// is done there (a workflow card's Review in Foundry, a draft's
	// questions); Unhandled too.
	ActionOpen = "foundry.open"
)

// Interaction is a click the bot acts on: a closed set. ParseInteraction
// reads every other interactivity request into Unhandled.
type Interaction interface{ slackInteraction() }

// Clicked is who clicked what, where.
type Clicked struct {
	// TeamID is the clicker's workspace as the payload's team names it.
	TeamID       string `json:"team_id"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	UserID       string `json:"user_id"`
	ChannelID    string `json:"channel_id"`
	// MessageTS is the message the button is on.
	MessageTS string `json:"message_ts"`
	// ThreadTS is the thread that message is in, "" at top level.
	ThreadTS string `json:"thread_ts,omitempty"`
	// Ephemeral is a button on an ephemeral message, which can't be updated.
	Ephemeral bool `json:"ephemeral,omitempty"`
	// ActionTS is when the click happened: with the team, user and action_id,
	// what the intake dedupes on (InteractionKey).
	ActionTS string `json:"action_ts"`
}

// Approve is Run on a person's card (per_person).
type Approve struct {
	Clicked
	ActionID    string `json:"action_id"`
	PayloadHash string `json:"payload_hash"`
}

// Dismiss is Dismiss on a card.
type Dismiss struct {
	Clicked
	ActionID string `json:"action_id"`
}

// Refresh is Prepare again on an expired card.
type Refresh struct {
	Clicked
	ActionID string `json:"action_id"`
}

// Review is Review on a bot's proposal (L2).
type Review struct {
	Clicked
	ActionID    string `json:"action_id"`
	PayloadHash string `json:"payload_hash"`
}

// Run is Run on a reviewed proposal (L2). SpecHash is the SHA-256 of the
// workflow spec the reviewer was shown, "" for other operations.
type Run struct {
	Clicked
	ActionID    string `json:"action_id"`
	PayloadHash string `json:"payload_hash"`
	SpecHash    string `json:"spec_hash,omitempty"`
}

// Stop is Stop on a turn's reply.
type Stop struct {
	Clicked
	TurnID string `json:"turn_id"`
}

// Unlink is Unlink on the message that confirms a link.
type Unlink struct {
	Clicked
}

// Unhandled is an interactivity request the bot does nothing with, and why.
type Unhandled struct{ Why string }

func (Approve) slackInteraction()   {}
func (Dismiss) slackInteraction()   {}
func (Refresh) slackInteraction()   {}
func (Review) slackInteraction()    {}
func (Run) slackInteraction()       {}
func (Stop) slackInteraction()      {}
func (Unlink) slackInteraction()    {}
func (Unhandled) slackInteraction() {}

// MaxValue is the most a button's value holds.
const MaxValue = 2000

var (
	// foundryIDRE is a Foundry id: a ULID.
	foundryIDRE = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	// hashRE is a hex SHA-256.
	hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ButtonValue is what a button's value names, as compact JSON.
type ButtonValue struct {
	Action      string `json:"a,omitempty"`
	PayloadHash string `json:"h,omitempty"`
	SpecHash    string `json:"s,omitempty"`
	Turn        string `json:"t,omitempty"`
}

// String is the value as a button carries it.
func (v ButtonValue) String() string {
	b, _ := json.Marshal(v) // four strings always marshal
	return string(b)
}

// payload is the fields of a block_actions payload the bot reads.
type payload struct {
	Type string `json:"type"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Team *struct {
		ID           string `json:"id"`
		EnterpriseID string `json:"enterprise_id"`
	} `json:"team"`
	Enterprise *struct {
		ID string `json:"id"`
	} `json:"enterprise"`
	IsEnterpriseInstall bool `json:"is_enterprise_install"`
	Container           struct {
		Type        string `json:"type"`
		MessageTS   string `json:"message_ts"`
		ThreadTS    string `json:"thread_ts"`
		ChannelID   string `json:"channel_id"`
		IsEphemeral bool   `json:"is_ephemeral"`
	} `json:"container"`
	Channel *struct {
		ID string `json:"id"`
	} `json:"channel"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
		ActionTS string `json:"action_ts"`
	} `json:"actions"`
}

// ParseInteraction reads a request to the interactivity URL: a form whose
// payload field is JSON. It returns an error only for a body that isn't one;
// a request the bot doesn't act on (another type, an org-wide install, a
// button not its own, a value it didn't write) is Unhandled.
func ParseInteraction(body []byte) (Interaction, error) {
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, fmt.Errorf("slack: decode interaction form: %w", err)
	}
	raw := form.Get("payload")
	if raw == "" {
		return nil, errors.New("slack: interaction without a payload")
	}
	var p payload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return nil, fmt.Errorf("slack: decode interaction payload: %w", err)
	}
	if p.Type != "block_actions" {
		return Unhandled{Why: fmt.Sprintf("interaction type %q", p.Type)}, nil
	}
	if p.IsEnterpriseInstall || p.Team == nil {
		return Unhandled{Why: "an org-wide install"}, nil
	}
	if len(p.Actions) != 1 {
		return Unhandled{Why: fmt.Sprintf("%d actions", len(p.Actions))}, nil
	}
	action := p.Actions[0]
	c := Clicked{
		TeamID: p.Team.ID, EnterpriseID: p.Team.EnterpriseID, UserID: p.User.ID,
		ChannelID: p.Container.ChannelID, MessageTS: p.Container.MessageTS, ThreadTS: p.Container.ThreadTS,
		Ephemeral: p.Container.IsEphemeral, ActionTS: action.ActionTS,
	}
	if c.EnterpriseID == "" && p.Enterprise != nil {
		c.EnterpriseID = p.Enterprise.ID
	}
	if c.ChannelID == "" && p.Channel != nil {
		c.ChannelID = p.Channel.ID
	}
	switch {
	case p.Container.Type != "message":
		return Unhandled{Why: fmt.Sprintf("a click in a %q", p.Container.Type)}, nil
	case !idRE.MatchString(c.TeamID) || (c.EnterpriseID != "" && !idRE.MatchString(c.EnterpriseID)) ||
		!idRE.MatchString(c.UserID) || !idRE.MatchString(c.ChannelID):
		return Unhandled{Why: "a click without its team, user or channel"}, nil
	case !tsRE.MatchString(c.MessageTS) || !tsRE.MatchString(c.ActionTS) || (c.ThreadTS != "" && !tsRE.MatchString(c.ThreadTS)):
		return Unhandled{Why: "a click without its message or time"}, nil
	}
	var v ButtonValue
	if action.Value != "" {
		if len(action.Value) > MaxValue || json.Unmarshal([]byte(action.Value), &v) != nil {
			return Unhandled{Why: "a button value the bot didn't write"}, nil
		}
	}
	// names is a value naming an action, with a payload hash when hash and
	// perhaps a spec hash when spec, and nothing else.
	names := func(hash, spec bool) bool {
		hashOK := v.PayloadHash == ""
		if hash {
			hashOK = hashRE.MatchString(v.PayloadHash)
		}
		specOK := v.SpecHash == "" || (spec && hashRE.MatchString(v.SpecHash))
		return foundryIDRE.MatchString(v.Action) && v.Turn == "" && hashOK && specOK
	}
	switch action.ActionID {
	case ActionApprove:
		if names(true, false) {
			return Approve{Clicked: c, ActionID: v.Action, PayloadHash: v.PayloadHash}, nil
		}
	case ActionDismiss:
		if names(false, false) {
			return Dismiss{Clicked: c, ActionID: v.Action}, nil
		}
	case ActionRefresh:
		if names(false, false) {
			return Refresh{Clicked: c, ActionID: v.Action}, nil
		}
	case ActionReview:
		if names(true, false) {
			return Review{Clicked: c, ActionID: v.Action, PayloadHash: v.PayloadHash}, nil
		}
	case ActionRun:
		if names(true, true) {
			return Run{Clicked: c, ActionID: v.Action, PayloadHash: v.PayloadHash, SpecHash: v.SpecHash}, nil
		}
	case ActionStop:
		if foundryIDRE.MatchString(v.Turn) && v == (ButtonValue{Turn: v.Turn}) {
			return Stop{Clicked: c, TurnID: v.Turn}, nil
		}
	case ActionUnlink:
		if v == (ButtonValue{}) {
			return Unlink{Clicked: c}, nil
		}
	default:
		return Unhandled{Why: fmt.Sprintf("action %q", action.ActionID)}, nil
	}
	return Unhandled{Why: fmt.Sprintf("a %s value the bot didn't write", action.ActionID)}, nil
}

// InteractionKey is the intake dedupe key of a click: "i:" and the hex
// SHA-256 of its team, user, action_ts and action_id, NUL-separated.
// Unhandled has none ("").
func InteractionKey(in Interaction) string {
	var (
		c        Clicked
		actionID string
	)
	switch in := in.(type) {
	case Approve:
		c, actionID = in.Clicked, ActionApprove
	case Dismiss:
		c, actionID = in.Clicked, ActionDismiss
	case Refresh:
		c, actionID = in.Clicked, ActionRefresh
	case Review:
		c, actionID = in.Clicked, ActionReview
	case Run:
		c, actionID = in.Clicked, ActionRun
	case Stop:
		c, actionID = in.Clicked, ActionStop
	case Unlink:
		c, actionID = in.Clicked, ActionUnlink
	default:
		return ""
	}
	sum := sha256.Sum256([]byte(c.TeamID + "\x00" + c.UserID + "\x00" + c.ActionTS + "\x00" + actionID))
	return "i:" + hex.EncodeToString(sum[:])
}
