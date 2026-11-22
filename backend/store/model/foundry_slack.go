package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Helix Foundry for Slack: the store's rows for installs of Helix's "Helix
// Foundry" app, linked Slack users, the Founder thread a Slack thread
// continues, what Slack sent (intake) and the Founder turns shown in Slack
// (replies) (migrations/20261213000000_foundry_slack.sql).

// SlackIdentityMode is whom an install's bot acts as: a closed set, derived
// from the install's row, never stored as a value of its own.
type SlackIdentityMode interface{ slackIdentityMode() }

// SlackPerPerson acts as each linked Slack user's Foundry user.
type SlackPerPerson struct{}

// SlackSharedBot acts as the install's own bot, a service credential of the
// workspace's that no one holds the token of (L2).
type SlackSharedBot struct{ CredentialID string }

func (SlackPerPerson) slackIdentityMode() {}
func (SlackSharedBot) slackIdentityMode() {}

// Retention of Slack's data (the spec's §12): minimise, never train. How
// long intake and replies stay is foundry.slack.retention's.
const (
	// SlackSecretsKept is how long a link token or install state is kept
	// after it expires.
	SlackSecretsKept = 24 * time.Hour
	// SlackThreadIdle is how long a Slack-born Founder thread is kept after
	// its last Slack message before its text is purged.
	SlackThreadIdle = 30 * 24 * time.Hour
	// SlackReplyHeld is how long a reply row waits for the intake worker to
	// post its first message before a follower takes it over.
	SlackReplyHeld = time.Hour
)

// SlackInstall is a Slack team's install of the app for one Foundry
// workspace. A live install (RevokedAt nil) is the only one of its team and
// of its workspace.
type SlackInstall struct {
	ID           string
	WorkspaceID  string
	TeamID       string
	EnterpriseID string // "" outside an Enterprise Grid org
	TeamName     string
	AppID        string
	BotUserID    string
	// BotID is auth.test's bot_id, which Helix's own messages carry: "" until
	// Finish reads it (a feed doesn't bind then).
	BotID string
	// InstallerAdmin says a Slack workspace admin or owner installed it: a
	// feed binds only then.
	InstallerAdmin bool
	// EventsLimitedAt is the last minute Slack dropped the team's events
	// (app_rate_limited); nil when it never has.
	EventsLimitedAt *time.Time
	// Scopes are the bot token's, as Slack granted them.
	Scopes            []string
	InstalledByUserID string
	// InstalledBySlackUserID is who in Slack installed it (oauth.v2.access
	// authed_user.id).
	InstalledBySlackUserID string
	Mode                   SlackIdentityMode
	// Destination is whom it serves, read from its row: never nil once
	// stored. InstallFoundrySlack takes nil as SlackServesWorkspace.
	Destination SlackDestination
	// FeedOn says a relayed install's local Slack data connector reads it:
	// hosted holds its channel messages for its local install.
	FeedOn    bool
	CreatedAt time.Time
	// UpdatedAt is the last install or reinstall.
	UpdatedAt time.Time
	// RevokedAt is set once it is disconnected or uninstalled; UninstalledAt
	// once Slack knows (apps.uninstall, or Slack's own event). A revoked,
	// not yet uninstalled install keeps its token for the sweep, and its team
	// can't be installed again until then.
	RevokedAt     *time.Time
	UninstalledAt *time.Time
}

// Validate refuses an install the store can't keep: every id, a name, and
// scopes Slack could have named.
func (i *SlackInstall) Validate() error {
	for _, f := range []struct{ name, value string }{
		{"id", i.ID}, {"workspace", i.WorkspaceID}, {"installer", i.InstalledByUserID},
	} {
		if f.value == "" {
			return fmt.Errorf("a Slack install names its %s", f.name)
		}
	}
	for _, f := range []struct {
		name, value string
		optional    bool
	}{
		{"team", i.TeamID, false}, {"enterprise", i.EnterpriseID, true}, {"app", i.AppID, false},
		{"bot user", i.BotUserID, false}, {"installer's Slack user", i.InstalledBySlackUserID, false},
	} {
		if (!f.optional || f.value != "") && !ValidSlackID(f.value) {
			return fmt.Errorf("a Slack install's %s id %q is not a Slack id", f.name, f.value)
		}
	}
	switch {
	case strings.TrimSpace(i.TeamName) == "" || len(i.TeamName) > 255:
		return fmt.Errorf("a Slack install's team name is 1 to 255 bytes")
	case len(i.Scopes) == 0 || len(strings.Join(i.Scopes, ",")) > 1024:
		return fmt.Errorf("a Slack install has scopes, at most 1024 bytes of them")
	}
	for _, s := range i.Scopes {
		if s == "" || strings.ContainsAny(s, ", \t\n") {
			return fmt.Errorf("Slack scope %q is not a scope", s)
		}
	}
	return nil
}

// ValidSlackID reports a Slack id the store keeps: a team, enterprise, user,
// app or channel id, or a message's ts: 1 to 32 bytes of letters, digits,
// '.', '_' and '-'.
func ValidSlackID(s string) bool {
	if s == "" || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// ValidSlackTS reports a Slack message ts: seconds, '.', and a sequence.
func ValidSlackTS(s string) bool {
	secs, seq, ok := strings.Cut(s, ".")
	digits := func(d string, most int) bool {
		return d != "" && len(d) <= most && strings.Trim(d, "0123456789") == ""
	}
	return ok && digits(secs, 20) && digits(seq, 10)
}

// SlackSealedToken is an install's bot token, sealed under KeyID with the
// install's id as its context. It never leaves the backend.
type SlackSealedToken struct {
	Sealed []byte
	KeyID  string
}

// SlackInstallState is an OAuth install in flight: its state's SHA-256,
// bound to the member who started it and their workspace.
type SlackInstallState struct {
	StateHash   string
	WorkspaceID string
	UserID      string
	// ReturnPath is where the site goes once Slack hands the install back;
	// "" is Settings › Slack.
	ReturnPath string
	// Destination is whom the install will serve: SlackServesRelay for one
	// a local install started through its link, whose ReturnTo is that
	// link's Settings › Slack (RelayLink.ReturnTo).
	Destination SlackDestination
	ReturnTo    string
	ExpiresAt   time.Time
}

// SlackLink is a Slack user of an install linked to a Foundry user, who the
// bot acts as in per_person mode.
type SlackLink struct {
	InstallID   string
	SlackUserID string
	UserID      string
	// Access is what the Slack user may do as UserID: SlackFullAccess but
	// for the people a local install's owner allowed (SlackReadAccess).
	Access    SlackLinkAccess
	CreatedAt time.Time
}

// SlackLinkToken is a one-time link the bot sent one Slack user: its
// token's SHA-256, live until it expires or is spent.
type SlackLinkToken struct {
	TokenHash   string
	InstallID   string
	SlackUserID string
	ExpiresAt   time.Time
}

// SlackThread is the Founder thread a Slack thread continues: per Slack user
// in per_person mode, one per Slack thread (SlackUserID "") in bot mode.
type SlackThread struct {
	ID            string
	InstallID     string
	ChannelID     string
	ThreadTS      string
	SlackUserID   string
	AgentThreadID string
	CreatedAt     time.Time
	// LastAt is the last Slack message the thread took: its text is purged
	// SlackThreadIdle after it.
	LastAt time.Time
	// BotAt is when the bot last posted in the Slack thread, and OthersAt
	// when anyone but the bot and SlackUserID last did; nil when never. An
	// unmentioned reply is taken while BotAt is the newer.
	BotAt    *time.Time
	OthersAt *time.Time
}

// SlackIntakeKind is what an intake row holds: a closed set.
type SlackIntakeKind string

const (
	// SlackIntakeEvent is an Events API event (a mention, a direct message,
	// an agent session stopped, or an uninstall kept only for dedupe).
	SlackIntakeEvent SlackIntakeKind = "event"
	// SlackIntakeInteraction is a click (block_actions).
	SlackIntakeInteraction SlackIntakeKind = "interaction"
)

// ParseSlackIntakeKind reads a kind, refusing any other.
func ParseSlackIntakeKind(s string) (SlackIntakeKind, error) {
	switch k := SlackIntakeKind(s); k {
	case SlackIntakeEvent, SlackIntakeInteraction:
		return k, nil
	}
	return "", fmt.Errorf("unknown Slack intake kind %q", s)
}

// SlackIntakeStatus is where an intake row is: a closed set.
type SlackIntakeStatus string

const (
	SlackIntakeQueued  SlackIntakeStatus = "queued"
	SlackIntakeRunning SlackIntakeStatus = "running"
	SlackIntakeDone    SlackIntakeStatus = "done"
	SlackIntakeFailed  SlackIntakeStatus = "failed"
)

// ParseSlackIntakeStatus reads a status, refusing any other.
func ParseSlackIntakeStatus(s string) (SlackIntakeStatus, error) {
	switch st := SlackIntakeStatus(s); st {
	case SlackIntakeQueued, SlackIntakeRunning, SlackIntakeDone, SlackIntakeFailed:
		return st, nil
	}
	return "", fmt.Errorf("unknown Slack intake status %q", s)
}

// SlackIntake is one thing Slack sent, once: queued, claimed by one worker
// at a time under a lease, and ended done or failed.
type SlackIntake struct {
	ID        string
	InstallID string
	// DedupeKey is SlackEventDedupe's or SlackClickDedupe's.
	DedupeKey string
	Kind      SlackIntakeKind
	// Conversation is SlackConversation's for a message, "" for a click: a
	// conversation's rows are claimed one at a time, oldest first.
	Conversation string
	SlackUserID  string
	// Payload is the parsed event, a JSON object; nil once done or failed.
	Payload json.RawMessage
	// Input is the turn input the first attempt rendered (payload's
	// "input"), which every retry sends again; nil until then.
	Input  *string
	Status SlackIntakeStatus
	// Attempts counts claims, this one included: whatever ended them, so
	// no row is claimed for ever.
	Attempts int
	// Failures counts the earlier claims that failed and were tried again
	// (SlackIntakeRetry), this one not included: what max_attempts bounds.
	// A deferral (SlackIntakeDeferred) or a wait (SlackIntakeWait) isn't
	// one.
	Failures      int
	NextAttemptAt time.Time
	// LeaseOwner is the claim's own random token: every write of the claim
	// names it, and one that finds another (the lease was reclaimed) changes
	// nothing.
	LeaseOwner *string
	LeaseUntil *time.Time
	// WaitsOnTurn, on a claimed row, is the busy turn it waited for until its
	// wait ran out (a wake clears it).
	WaitsOnTurn *string
	Error       *string
	CreatedAt   time.Time
	FinishedAt  *time.Time
}

// Validate refuses a row the intake never queues.
func (in *SlackIntake) Validate() error {
	_, kindErr := ParseSlackIntakeKind(string(in.Kind))
	var object map[string]json.RawMessage
	switch {
	case in.ID == "" || in.InstallID == "":
		return fmt.Errorf("a Slack intake row names itself and its install")
	case kindErr != nil:
		return kindErr
	case !ValidSlackDedupe(in.DedupeKey):
		return fmt.Errorf("Slack dedupe key %q is neither an event's nor a click's", in.DedupeKey)
	case in.Conversation != "" && !ValidSHA256Hex(in.Conversation):
		return fmt.Errorf("a Slack conversation is a SHA-256 in hex")
	case in.SlackUserID != "" && !ValidSlackID(in.SlackUserID):
		return fmt.Errorf("%q is not a Slack user id", in.SlackUserID)
	case json.Unmarshal(in.Payload, &object) != nil || object == nil:
		return fmt.Errorf("a Slack intake payload is a JSON object")
	}
	return nil
}

// SlackIntakeOutcome ends a claim of an intake row: a closed set.
type SlackIntakeOutcome interface{ slackIntakeOutcome() }

// SlackIntakeEnded ends the row done: its payload is dropped.
type SlackIntakeEnded struct{}

// SlackIntakeGaveUp ends the row failed, saying why; its payload is dropped.
type SlackIntakeGaveUp struct{ Error string }

// SlackIntakeRetry queues the row again After from now, saying why: a
// failure, which counts toward its Failures.
type SlackIntakeRetry struct {
	After time.Duration
	Error string
}

// SlackIntakeDeferred queues the row again After from now, saying why,
// without counting a failure: a rate limit, or a thread busy with no turn
// to wait on.
type SlackIntakeDeferred struct {
	After time.Duration
	Error string
}

// SlackIntakeWait queues the row behind TurnID, a busy thread's live turn,
// for at most For: WakeFoundrySlackIntake wakes it when the turn ends. The
// conversation's later rows wait behind it.
type SlackIntakeWait struct {
	TurnID string
	For    time.Duration
}

func (SlackIntakeEnded) slackIntakeOutcome()    {}
func (SlackIntakeGaveUp) slackIntakeOutcome()   {}
func (SlackIntakeRetry) slackIntakeOutcome()    {}
func (SlackIntakeDeferred) slackIntakeOutcome() {}
func (SlackIntakeWait) slackIntakeOutcome()     {}

// SlackReplyStatus is where a reply is: a closed set.
type SlackReplyStatus string

const (
	// SlackReplyFollowing: the turn is shown as it goes on.
	SlackReplyFollowing SlackReplyStatus = "following"
	// SlackReplyDone: the turn ended and its final message is written.
	SlackReplyDone SlackReplyStatus = "done"
)

// ParseSlackReplyStatus reads a status, refusing any other.
func ParseSlackReplyStatus(s string) (SlackReplyStatus, error) {
	switch st := SlackReplyStatus(s); st {
	case SlackReplyFollowing, SlackReplyDone:
		return st, nil
	}
	return "", fmt.Errorf("unknown Slack reply status %q", s)
}

// SlackPostPending is a posted entry written before its post: a follower
// that finds it posts again (at-least-once).
const SlackPostPending = "pending"

// SlackPostRefused is a posted entry Slack refused for good (or that can't
// be sent at all, its blocks past Block Kit's limits): never posted, never
// tried again.
const SlackPostRefused = "refused"

// SlackPostDone reports a posted entry its follower is done with: posted
// (a message's ts) or refused for good.
func SlackPostDone(v string) bool { return v == SlackPostRefused || ValidSlackTS(v) }

// SlackReply is a Founder turn shown in Slack, followed by one worker at a
// time under a lease it keeps for the whole follow.
type SlackReply struct {
	TurnID      string
	InstallID   string
	ChannelID   string
	ThreadTS    string
	SlackUserID string
	// MessageTS is the reply message the follower updates; nil until posted.
	MessageTS *string
	// AfterSeq is the turn's last event shown.
	AfterSeq int
	// Posted is what else was posted, by key ("q:<question>", "a:<action>",
	// "k:<asking turn>", and "m:<n>" for the reply's n-th message past its
	// first, once the first filled): the message's ts, SlackPostPending or
	// SlackPostRefused. A post of several messages records each, its i-th
	// under its key and ".<i>", while it goes, then its key: its first
	// message's ts.
	Posted       map[string]string
	Status       SlackReplyStatus
	LeaseOwner   *string
	LeaseUntil   *time.Time
	NextPollAt   time.Time
	NextUpdateAt time.Time
	CreatedAt    time.Time
	FinishedAt   *time.Time
}

// Validate refuses a reply the worker never follows.
func (r *SlackReply) Validate() error {
	switch {
	case r.TurnID == "" || r.InstallID == "":
		return fmt.Errorf("a Slack reply names its turn and install")
	case !ValidSlackID(r.ChannelID) || !ValidSlackTS(r.ThreadTS) || !ValidSlackID(r.SlackUserID):
		return fmt.Errorf("a Slack reply names its channel, thread and Slack user")
	}
	return nil
}

// SlackReplyUpdate is a follower's progress, written under its lease, which
// it renews for Lease. Nil fields stay; Posted merges into what was posted;
// UpdateAfter sets the reply message's next update that long from now.
type SlackReplyUpdate struct {
	Lease       time.Duration
	AfterSeq    *int
	MessageTS   *string
	Posted      map[string]string
	UpdateAfter *time.Duration
}

// Validate refuses an update no follower writes.
func (u SlackReplyUpdate) Validate() error {
	switch {
	case u.Lease <= 0:
		return fmt.Errorf("a Slack reply's update renews its lease")
	case u.AfterSeq != nil && *u.AfterSeq < 0:
		return fmt.Errorf("a Slack reply's after_seq is not negative")
	case u.MessageTS != nil && !ValidSlackTS(*u.MessageTS):
		return fmt.Errorf("%q is not a Slack message ts", *u.MessageTS)
	case u.UpdateAfter != nil && *u.UpdateAfter < 0:
		return fmt.Errorf("a Slack reply's next update is not in the past")
	}
	for k, v := range u.Posted {
		if !validSlackPostKey(k) || v != SlackPostPending && !SlackPostDone(v) {
			return fmt.Errorf("Slack post %q=%q is not a post's key and ts", k, v)
		}
	}
	return nil
}

// validSlackPostKey reports "q:", "a:", "k:" or "m:" and an id.
func validSlackPostKey(k string) bool {
	kind, id, ok := strings.Cut(k, ":")
	return ok && (kind == "q" || kind == "a" || kind == "k" || kind == "m") && ValidSlackID(id)
}

// SlackRefusalReason is why an install was refused: a closed set the API
// words for the person.
type SlackRefusalReason string

const (
	// SlackDisconnecting: the team's last install is revoked but Slack
	// hasn't been told yet; the sweep tells it shortly.
	SlackDisconnecting SlackRefusalReason = "disconnecting"
	// SlackTeamTaken: the team is installed for another Foundry workspace.
	SlackTeamTaken SlackRefusalReason = "team_taken"
	// SlackInstalled: the workspace has a live install of another team.
	SlackInstalled SlackRefusalReason = "installed"
)

// SlackRefusal is an install the rules refuse now.
type SlackRefusal struct {
	Reason SlackRefusalReason
	Detail string
}

func (r SlackRefusal) Error() string { return string(r.Reason) + ": " + r.Detail }

// SlackUninstallOutcome is what an uninstall event did: a closed set.
type SlackUninstallOutcome string

const (
	// SlackUninstallDone revoked the team's live install.
	SlackUninstallDone SlackUninstallOutcome = "uninstalled"
	// SlackUninstallStale was sent before the live install was made (a
	// retry after a reinstall): nothing changed.
	SlackUninstallStale SlackUninstallOutcome = "stale"
	// SlackUninstallUnknownTeam: the team has no live install.
	SlackUninstallUnknownTeam SlackUninstallOutcome = "unknown_team"
	// SlackUninstallDuplicate: the event was taken already.
	SlackUninstallDuplicate SlackUninstallOutcome = "duplicate"
)

// SlackConversation is an intake row's conversation for a message: the
// SHA-256, in hex, of the install, channel and thread, and of the Slack
// user in per_person mode ("" in bot mode, where one Slack thread is one
// conversation).
func SlackConversation(installID, channelID, threadTS, slackUserID string) string {
	sum := sha256.Sum256([]byte(installID + "\x00" + channelID + "\x00" + threadTS + "\x00" + slackUserID))
	return hex.EncodeToString(sum[:])
}

// SlackEventDedupe is an event's intake dedupe key: "e:" and its event_id.
func SlackEventDedupe(eventID string) string { return "e:" + eventID }

// SlackClickDedupe is a click's intake dedupe key: "i:" and the SHA-256, in
// hex, of its team, user, action_ts and action_id (Slack gives a click no id).
func SlackClickDedupe(teamID, userID, actionTS, actionID string) string {
	sum := sha256.Sum256([]byte(teamID + "\x00" + userID + "\x00" + actionTS + "\x00" + actionID))
	return "i:" + hex.EncodeToString(sum[:])
}

// ValidSlackDedupe reports an intake dedupe key: an event's (SlackEventDedupe
// of a Slack event id) or a click's (SlackClickDedupe).
func ValidSlackDedupe(k string) bool {
	kind, rest, ok := strings.Cut(k, ":")
	switch {
	case !ok:
		return false
	case kind == "e":
		return ValidSlackID(rest)
	case kind == "i":
		return ValidSHA256Hex(rest)
	}
	return false
}

// ValidSHA256Hex reports a SHA-256 in lowercase hex, as the store keeps
// tokens', states' and conversations' hashes.
func ValidSHA256Hex(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
