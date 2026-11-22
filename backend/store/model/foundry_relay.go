package model

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Helix Foundry for Slack's relay (L3): local Foundry installs linked to a
// Helix workspace, which Slack's requests for their Slack teams are relayed
// to (migrations/20261216000000_foundry_slack_relay.sql). Hosted keeps the
// links, the codes that make them, the deliveries and channel messages
// waiting for a local install, what its proxied calls may name, and daily
// counts. A local install keeps its own Slack rows, and the Slack users
// its owner let use it.

const (
	// RelaySettingsPath is where a local install's site takes Slack's answer
	// back: a link's redirect URI's path, and its relayed installs'
	// return_to's.
	RelaySettingsPath = "/settings/slack"
	// RelaySeenEvery is how often a link's last contact is written: at most
	// once a minute, whatever it calls.
	RelaySeenEvery = time.Minute
	// RelayChannelSeen is how long a channel a relayed install was sent
	// something from stays one its proxied calls may name.
	RelayChannelSeen = 30 * 24 * time.Hour
	// RelayFinishedKept is how long an acked or expired delivery is kept, its
	// payload gone, to dedupe Slack's retries.
	RelayFinishedKept = 24 * time.Hour
	// RelayFeedLateness is how long after its message a channel message may
	// reach Helix (Slack's retries): a gap of expired rows starts this long
	// before the first of them arrived.
	RelayFeedLateness = 10 * time.Minute
)

// RelayLink is a local install linked to a Helix workspace: Slack's requests
// for its installs wait for it, and its proxied calls act as them. Its
// hx_rl_ token is kept only as its SHA-256.
type RelayLink struct {
	ID              string
	WorkspaceID     string
	CreatedByUserID string
	// Name is the install's own, as its owner named it.
	Name string
	// RedirectOrigin is the loopback origin the link was authorized for.
	RedirectOrigin string
	CreatedAt      time.Time
	// LastSeenAt and LastSeenAddr are its last claim or call, written at
	// most every RelaySeenEvery; nil and "" until then.
	LastSeenAt   *time.Time
	LastSeenAddr string
	RevokedAt    *time.Time
}

// ReturnTo is where Slack's answer to an install the link started goes
// back to: its origin's Settings › Slack, and nowhere else.
func (l RelayLink) ReturnTo() string { return l.RedirectOrigin + RelaySettingsPath }

// RelayCode is a one-time code the site gave the owner's browser for one
// install, spent once with its PKCE verifier before it expires.
type RelayCode struct {
	// CodeHash is the code's SHA-256, in hex.
	CodeHash    string
	WorkspaceID string
	UserID      string
	// CodeChallenge is the verifier's S256 challenge (RelayCodeChallenge).
	CodeChallenge string
	// RedirectURI is a loopback Settings › Slack (ParseRelayRedirect).
	RedirectURI string
	// Name is the install's, which the link takes.
	Name      string
	ExpiresAt time.Time
}

// Validate refuses a code the site never gives: every id, hashes, a
// challenge, a loopback redirect and a name.
func (c *RelayCode) Validate() error {
	_, redirectErr := ParseRelayRedirect(c.RedirectURI)
	switch {
	case c.WorkspaceID == "" || c.UserID == "":
		return fmt.Errorf("a relay code names its workspace and member")
	case !ValidSHA256Hex(c.CodeHash):
		return fmt.Errorf("a relay code is kept as a SHA-256 in hex")
	case !ValidRelayChallenge(c.CodeChallenge):
		return fmt.Errorf("a relay code's challenge is an S256 PKCE challenge")
	case redirectErr != nil:
		return redirectErr
	case !ValidRelayName(c.Name):
		return fmt.Errorf("a relay link's name is 1 to 128 bytes of text")
	}
	return nil
}

// ParseRelayRedirect reads a link's redirect URI, which must be a local
// install's Settings › Slack on this computer:
// http://localhost[:port]/settings/slack or http://127.0.0.1[:port]/settings/slack,
// nothing else in it. It returns its origin.
func ParseRelayRedirect(uri string) (origin string, err error) {
	u, err := url.Parse(uri)
	refuse := fmt.Errorf("a relay redirect is http://localhost:<port>%s or http://127.0.0.1:<port>%s", RelaySettingsPath, RelaySettingsPath)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		u.RawFragment != "" || u.RawPath != "" || u.Path != RelaySettingsPath || len(uri) > 255 {
		return "", refuse
	}
	host, port := u.Hostname(), u.Port()
	if host != "localhost" && host != "127.0.0.1" {
		return "", refuse
	}
	if u.Host != host {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port || u.Host != net.JoinHostPort(host, port) {
			return "", refuse
		}
	}
	return "http://" + u.Host, nil
}

// RelayCodeChallenge is a PKCE verifier's S256 challenge: the unpadded
// base64url of its SHA-256.
func RelayCodeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ValidRelayVerifier reports a PKCE verifier (RFC 7636): 43 to 128 of
// letters, digits, '-', '.', '_' and '~'.
func ValidRelayVerifier(s string) bool {
	return len(s) >= 43 && len(s) <= 128 && strings.Trim(s, pkceChars) == ""
}

// ValidRelayChallenge reports an S256 challenge: 43 base64url characters.
func ValidRelayChallenge(s string) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return len(s) == 43 && err == nil && len(b) == sha256.Size
}

const pkceChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

// ValidRelayName reports a local install's name, as Settings shows it: 1 to
// 128 bytes of UTF-8 with no control or bidi control character, which could
// make one row read as another.
func ValidRelayName(s string) bool {
	return strings.TrimSpace(s) != "" && len(s) <= 128 && utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r)
	})
}

// RelayDeliveryStatus is where a delivery is: a closed set.
type RelayDeliveryStatus string

const (
	// RelayQueued waits for its local install's claim.
	RelayQueued RelayDeliveryStatus = "queued"
	// RelayLeased was claimed, and is claimed again once its lease runs out
	// unacked.
	RelayLeased RelayDeliveryStatus = "leased"
	// RelayAcked was taken by its local install.
	RelayAcked RelayDeliveryStatus = "acked"
	// RelayExpired was never taken: its install was offline, or went.
	RelayExpired RelayDeliveryStatus = "expired"
)

// ParseRelayDeliveryStatus reads a status, refusing any other.
func ParseRelayDeliveryStatus(s string) (RelayDeliveryStatus, error) {
	switch st := RelayDeliveryStatus(s); st {
	case RelayQueued, RelayLeased, RelayAcked, RelayExpired:
		return st, nil
	}
	return "", fmt.Errorf("unknown relay delivery status %q", s)
}

// RelayDelivery is something Slack sent for a relayed install, held until
// its local install takes it: what a hosted install's intake row holds
// (its dedupe key, kind, conversation, Slack user and payload), which the
// local install queues in its own intake.
type RelayDelivery struct {
	ID        string
	InstallID string
	// LinkID and LocalWorkspaceID are its install's, which the store reads.
	LinkID           string
	LocalWorkspaceID string
	DedupeKey        string
	Kind             SlackIntakeKind
	Conversation     string
	SlackUserID      string
	// Payload is the parsed event or click, a JSON object; nil once acked or
	// expired.
	Payload    json.RawMessage
	Status     RelayDeliveryStatus
	Attempts   int
	LeaseUntil *time.Time
	// NoticedAt is when its conversation was told its install is offline.
	NoticedAt  *time.Time
	CreatedAt  time.Time
	ExpiresAt  time.Time
	FinishedAt *time.Time
}

// Validate refuses a delivery the intake never queues: as an intake row.
func (d *RelayDelivery) Validate() error { return d.Intake().Validate() }

// Intake is the intake row a local install queues for the delivery: the
// same id, dedupe key, kind, conversation, Slack user and payload.
func (d *RelayDelivery) Intake() *SlackIntake {
	return &SlackIntake{ID: d.ID, InstallID: d.InstallID, DedupeKey: d.DedupeKey, Kind: d.Kind, Conversation: d.Conversation,
		SlackUserID: d.SlackUserID, Payload: d.Payload}
}

// RelaySeen is what a delivery tells the relay its install may name in its
// proxied calls: the channel it came from ("" for none; never an
// externally shared one, which the caller leaves out) and its Slack users
// (the sender and those it mentions).
type RelaySeen struct {
	ChannelID string
	UserIDs   []string
}

// Validate refuses a channel or a user that isn't a Slack id.
func (s RelaySeen) Validate() error {
	if s.ChannelID != "" && !ValidSlackID(s.ChannelID) {
		return fmt.Errorf("relay channel %q is not a Slack id", s.ChannelID)
	}
	for _, u := range s.UserIDs {
		if !ValidSlackID(u) {
			return fmt.Errorf("relay user %q is not a Slack id", u)
		}
	}
	return nil
}

// RelayFeedItem is what a relayed install's channel messages hold for its
// local install: a closed set.
type RelayFeedItem interface{ relayFeedItem() }

// RelayFeedRow is a channel message's buffer row, which the local install
// appends to its feed as it is (its source is always the event).
type RelayFeedRow struct {
	ID               string
	InstallID        string
	LocalWorkspaceID string
	Row              SlackFeedRow
}

// RelayFeedGap is a span of a channel's messages (every channel's, for
// ChannelID "") the local install may never get: Slack dropped them, the
// door refused them, hosted held too many, or they expired unclaimed. The
// local install records it as a gap of its feed.
type RelayFeedGap struct {
	ID               string
	InstallID        string
	LocalWorkspaceID string
	Gap              SlackFeedGap
}

func (RelayFeedRow) relayFeedItem() {}
func (RelayFeedGap) relayFeedItem() {}

// ValidRelayFeedRow refuses a row the relay never holds: a valid event row
// that isn't a gap (gaps are RelayFeedGap).
func ValidRelayFeedRow(r SlackFeedRow) error {
	switch {
	case r.Kind == SlackRowGap:
		return fmt.Errorf("a relayed gap is a gap, not a row")
	case r.Source != SlackFeedFromEvent:
		return fmt.Errorf("a relayed row is an event's")
	}
	return r.Validate()
}

// RelayGapDedupe is a relayed gap's key: "g:" and the SHA-256, in hex, of
// its channel ("" for every channel) and span.
func RelayGapDedupe(g SlackFeedGap) string {
	return g.Row(g.ChannelID).DedupeKey
}

// RelayFeedAppend is what one Slack request gives a relayed install's
// feed: its channel messages, which past the cap become a gap of At's
// minute in each of their channels, and its gaps, always kept.
type RelayFeedAppend struct {
	Rows []SlackFeedRow
	Gaps []SlackFeedGap
	At   time.Time
}

// Validate refuses an append with nothing, no time, or a row or gap the
// relay never holds.
func (a RelayFeedAppend) Validate() error {
	if len(a.Rows)+len(a.Gaps) == 0 || a.At.IsZero() {
		return fmt.Errorf("a relay feed append has rows or gaps, and a time")
	}
	for _, r := range a.Rows {
		if err := ValidRelayFeedRow(r); err != nil {
			return err
		}
	}
	for _, g := range a.Gaps {
		if err := g.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// RelayFeedLimits bound a relayed install's feed: a row is held for TTL
// (relay.feed_ttl), and an install holds at most MaxRows message rows
// (relay.feed_max_rows).
type RelayFeedLimits struct {
	TTL     time.Duration
	MaxRows int64
}

// RelayFeedAppended is what an append did: its outcome (SlackAppendUnbound
// when the install isn't a live relayed one with its feed on) and how many
// rows and gaps it kept.
type RelayFeedAppended struct {
	Outcome SlackAppendOutcome
	Rows    int
}

// RelayFeedOn is whether a local workspace's Slack data connector reads
// its relayed install, as a claim says.
type RelayFeedOn struct {
	LocalWorkspaceID string
	On               bool
}

// RelayBudget bounds one claim: at most Deliveries deliveries, then at most
// FeedRows feed items and FeedBytes of their payloads, unless the first
// alone is more.
type RelayBudget struct {
	Deliveries int
	FeedRows   int
	FeedBytes  int64
}

// Validate refuses a budget that takes nothing.
func (b RelayBudget) Validate() error {
	if b.Deliveries < 1 || b.FeedRows < 0 || b.FeedBytes < 0 {
		return fmt.Errorf("a relay claim takes deliveries, and no negative feed")
	}
	return nil
}

// RelayClaim is what one claim leased: deliveries first, then the feed,
// each oldest first.
type RelayClaim struct {
	Deliveries []*RelayDelivery
	Feed       []RelayFeedItem
}

// RelayUsage is what a link did, counted per UTC day: deliveries queued,
// proxied calls made and refused, and channel messages held.
type RelayUsage struct {
	Deliveries, Calls, Refused, FeedRows, FeedBytes int64
}

// SlackDestination is whom a hosted install serves: a closed set, derived
// from its row.
type SlackDestination interface{ slackDestination() }

// SlackServesWorkspace serves its own workspace: Helix runs its bot.
type SlackServesWorkspace struct{}

// SlackServesRelay serves a local install linked to its workspace (LinkID),
// whose workspace LocalWorkspaceID it is: Slack's requests wait for that
// install's claims, and its calls come through the relay.
type SlackServesRelay struct {
	LinkID           string
	LocalWorkspaceID string
}

func (SlackServesWorkspace) slackDestination() {}
func (SlackServesRelay) slackDestination()     {}

// SlackDestinationKey is the key a live install of workspaceID serving d is
// unique by: "<workspace>/" for its own, "<workspace>/<local workspace>" for
// a relayed one (live_destination). A nil d is the workspace's own.
func SlackDestinationKey(workspaceID string, d SlackDestination) string {
	if r, ok := d.(SlackServesRelay); ok {
		return workspaceID + "/" + r.LocalWorkspaceID
	}
	return workspaceID + "/"
}

// SlackLinkAccess is what a linked Slack user may do: a closed set.
type SlackLinkAccess interface{ slackLinkAccess() }

// SlackFullAccess acts as its Foundry user in full: every hosted link, and
// locally the owner's own (the app's installer).
type SlackFullAccess struct{}

// SlackReadAccess only reads and asks, as the owner (local, allowed by the
// owner): its turns are read-only.
type SlackReadAccess struct{}

func (SlackFullAccess) slackLinkAccess() {}
func (SlackReadAccess) slackLinkAccess() {}

// ParseSlackLinkAccess reads an access as its column keeps it, refusing any
// other.
func ParseSlackLinkAccess(s string) (SlackLinkAccess, error) {
	switch s {
	case "full":
		return SlackFullAccess{}, nil
	case "read":
		return SlackReadAccess{}, nil
	}
	return nil, fmt.Errorf("unknown Slack link access %q", s)
}

// SlackAccessRequest is a Slack user who asked to use a local install, once,
// until its owner allows them (SlackReadAccess) or drops the request.
type SlackAccessRequest struct {
	InstallID   string
	SlackUserID string
	// SlackName is their name in Slack, as Settings shows it.
	SlackName string
	CreatedAt time.Time
}

// Validate refuses a request the bot never records.
func (r *SlackAccessRequest) Validate() error {
	switch {
	case r.InstallID == "" || !ValidSlackID(r.SlackUserID):
		return fmt.Errorf("an access request names its install and Slack user")
	case strings.TrimSpace(r.SlackName) == "" || len(r.SlackName) > 255 || !utf8.ValidString(r.SlackName):
		return fmt.Errorf("an access request's Slack name is 1 to 255 bytes of text")
	}
	return nil
}
