package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The Slack data connector on Helix's Slack app: the store's rows for a
// connector's feed of a Slack team, its buffer of what Slack sent and the
// backfill read, the day mirror's index, and the backfill's windows,
// threads, slots and gap budget
// (migrations/20261214000000_foundry_slack_feed.sql).

// ConnectorKindSlack is the kind of a Slack connector's connectors row,
// read with a token or through Helix's Slack app.
const ConnectorKindSlack = "slack"

// SlackAppHistoryDaysMax is the most days of history a feed keeps: Slack's
// free plans keep no more, and data mustn't outlive them.
const SlackAppHistoryDaysMax = 90

// SlackFeed is a connector's binding to the Slack team its install
// reads: one live feed per connector, per install and per Enterprise Grid
// org. Unbinding marks it deleting; the sweep deletes what it holds.
type SlackFeed struct {
	ID           string
	ConnectorID  string
	WorkspaceID  string
	TeamID       string
	EnterpriseID string // "" outside an Enterprise Grid org
	// InstallID is the team's install it reads through; a reinstall after a
	// removal points it at the new one.
	InstallID string
	// LastSeq numbers the newest buffer row; FoldedSeq is how far the
	// mirror holds them.
	LastSeq   int64
	FoldedSeq int64
	// NoticeVersion is the storage notice BoundBy accepted at BoundAt.
	NoticeVersion int
	BoundBy       string
	BoundAt       time.Time
	DeletingAt    *time.Time
}

// SlackFeedRowKind is what a buffer row holds: a closed set.
type SlackFeedRowKind string

const (
	SlackRowMessage  SlackFeedRowKind = "message"
	SlackRowChanged  SlackFeedRowKind = "changed"
	SlackRowDeleted  SlackFeedRowKind = "deleted"
	SlackRowReplied  SlackFeedRowKind = "replied"
	SlackRowChannel  SlackFeedRowKind = "channel"
	SlackRowJoined   SlackFeedRowKind = "joined"
	SlackRowLeft     SlackFeedRowKind = "left"
	SlackRowSpan     SlackFeedRowKind = "span"
	SlackRowComplete SlackFeedRowKind = "complete"
	SlackRowGap      SlackFeedRowKind = "gap"
)

// ParseSlackFeedRowKind reads a kind, refusing any other.
func ParseSlackFeedRowKind(s string) (SlackFeedRowKind, error) {
	switch k := SlackFeedRowKind(s); k {
	case SlackRowMessage, SlackRowChanged, SlackRowDeleted, SlackRowReplied, SlackRowChannel, SlackRowJoined, SlackRowLeft,
		SlackRowSpan, SlackRowComplete, SlackRowGap:
		return k, nil
	}
	return "", fmt.Errorf("unknown Slack feed row kind %q", s)
}

// SlackFeedSource is where a buffer row came from: a closed set.
type SlackFeedSource string

const (
	SlackFeedFromEvent    SlackFeedSource = "event"
	SlackFeedFromBackfill SlackFeedSource = "backfill"
)

// ParseSlackFeedSource reads a source, refusing any other.
func ParseSlackFeedSource(s string) (SlackFeedSource, error) {
	switch src := SlackFeedSource(s); src {
	case SlackFeedFromEvent, SlackFeedFromBackfill:
		return src, nil
	}
	return "", fmt.Errorf("unknown Slack feed source %q", s)
}

// SlackFeedRow is one buffer row: what Slack sent, or a page the backfill
// read, until the fold applies it to the mirror. Seq is the store's.
type SlackFeedRow struct {
	Seq       int64
	DedupeKey string
	ChannelID string
	// Day is the UTC day the row lands on, at midnight.
	Day     time.Time
	Kind    SlackFeedRowKind
	Source  SlackFeedSource
	Payload json.RawMessage
}

// Validate refuses a row the store can't keep: its key, channel, day, kind,
// source, and a JSON payload.
func (r SlackFeedRow) Validate() error {
	switch {
	case !ValidSlackFeedDedupe(r.DedupeKey):
		return fmt.Errorf("Slack feed row key %q is not a feed key", r.DedupeKey)
	case !ValidSlackID(r.ChannelID):
		return fmt.Errorf("Slack feed row channel %q is not a Slack id", r.ChannelID)
	case !r.Day.Equal(SlackDay(r.Day)):
		return fmt.Errorf("a Slack feed row's day is a UTC midnight, not %s", r.Day)
	case !json.Valid(r.Payload):
		return fmt.Errorf("a Slack feed row's payload is JSON")
	}
	if _, err := ParseSlackFeedRowKind(string(r.Kind)); err != nil {
		return err
	}
	_, err := ParseSlackFeedSource(string(r.Source))
	return err
}

// Minute is the minute the row's message was posted, which a gap in its
// place re-reads: its payload's ts (a deleted message's deleted_ts, the
// latest reply a replied row names), never after fallback; fallback, to
// the minute, for a row naming no message (a channel's lifecycle, a page's
// span, a day's close).
func (r SlackFeedRow) Minute(fallback time.Time) time.Time {
	var p struct {
		TS          string `json:"ts"`
		DeletedTS   string `json:"deleted_ts"`
		LatestReply string `json:"latest_reply"`
	}
	_ = json.Unmarshal(r.Payload, &p) // Validate: JSON
	ts := p.TS
	switch {
	case r.Kind == SlackRowDeleted && p.DeletedTS != "":
		ts = p.DeletedTS
	case r.Kind == SlackRowReplied && p.LatestReply != "":
		ts = p.LatestReply
	case r.Kind != SlackRowMessage && r.Kind != SlackRowChanged && r.Kind != SlackRowDeleted && r.Kind != SlackRowReplied:
		ts = ""
	}
	minute := fallback.UTC().Truncate(time.Minute)
	if !ValidSlackTS(ts) {
		return minute
	}
	secs, _, _ := strings.Cut(ts, ".")
	n, err := strconv.ParseInt(secs, 10, 64)
	if err != nil {
		return minute
	}
	if at := time.Unix(n, 0).UTC().Truncate(time.Minute); at.Before(minute) {
		return at
	}
	return minute
}

// SlackDay is the UTC day t falls on, at midnight: the day a row lands on.
func SlackDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// SlackBackfillDedupe is a backfilled row's key: "b:" and the SHA-256, in
// hex, of its channel, ts, edited ts and kind, so a page read twice is kept
// once and an edit read later is kept beside it.
func SlackBackfillDedupe(channelID, ts, editedTS string, kind SlackFeedRowKind) string {
	sum := sha256.Sum256([]byte(channelID + "\x00" + ts + "\x00" + editedTS + "\x00" + string(kind)))
	return "b:" + hex.EncodeToString(sum[:])
}

// ValidSlackFeedDedupe reports a buffer row's key: an event's ("e:"), a
// backfilled row's ("b:") or a gap's ("g:").
func ValidSlackFeedDedupe(k string) bool {
	kind, rest, ok := strings.Cut(k, ":")
	switch {
	case !ok:
		return false
	case kind == "e":
		return ValidSlackID(rest)
	case kind == "b" || kind == "g":
		return ValidSHA256Hex(rest)
	}
	return false
}

// SlackFeedGap is a span of a channel's messages that may never have
// arrived: Slack dropped them, the door refused them on Slack's last try, the
// buffer was full, or the install was gone. A gap row marks its days partial,
// and a gap window reads it again within the budget.
type SlackFeedGap struct {
	// ChannelID "" is every channel the feed covers.
	ChannelID string
	From, To  time.Time
}

// Validate refuses a gap with no span, or naming a channel that isn't one.
func (g SlackFeedGap) Validate() error {
	switch {
	case g.ChannelID != "" && !ValidSlackID(g.ChannelID):
		return fmt.Errorf("Slack feed gap channel %q is not a Slack id", g.ChannelID)
	case !g.From.Before(g.To):
		return fmt.Errorf("a Slack feed gap spans time: %s is not before %s", g.From, g.To)
	}
	return nil
}

// SlackMinuteGap is the gap of one minute, the one t falls in.
func SlackMinuteGap(channelID string, t time.Time) SlackFeedGap {
	from := t.UTC().Truncate(time.Minute)
	return SlackFeedGap{ChannelID: channelID, From: from, To: from.Add(time.Minute)}
}

// Row is the gap's buffer row for channelID (the gap's own when it names
// one): on From's day, its payload {"from", "to"} in RFC 3339, keyed by the
// channel and span, so the same gap recorded twice is kept once.
func (g SlackFeedGap) Row(channelID string) SlackFeedRow {
	from, to := g.From.UTC().Format(time.RFC3339Nano), g.To.UTC().Format(time.RFC3339Nano)
	sum := sha256.Sum256([]byte(channelID + "\x00" + from + "\x00" + to))
	payload, _ := json.Marshal(struct {
		From string `json:"from"`
		To   string `json:"to"`
	}{from, to})
	return SlackFeedRow{DedupeKey: "g:" + hex.EncodeToString(sum[:]), ChannelID: channelID, Day: SlackDay(g.From), Kind: SlackRowGap,
		Source: SlackFeedFromEvent, Payload: payload}
}

// SlackFeedBatch is what one Slack request appends: its rows, and when it
// happened, the minute a full buffer records as a gap of each row's channel.
type SlackFeedBatch struct {
	Rows []SlackFeedRow
	At   time.Time
}

// SlackFeedLimits bound appends: a pushed connector's sync is due Spacing
// after the first row that finds none due, and a feed holds at most MaxRows
// unfolded rows.
type SlackFeedLimits struct {
	Spacing time.Duration
	MaxRows int64
}

// SlackAppendOutcome is what an append did: a closed set.
type SlackAppendOutcome string

const (
	// SlackAppended kept the rows (a repeat of one already kept is dropped).
	SlackAppended SlackAppendOutcome = "appended"
	// SlackAppendDuplicate kept none: every row was kept already.
	SlackAppendDuplicate SlackAppendOutcome = "duplicate"
	// SlackAppendFull kept a gap for each row's channel in their place.
	SlackAppendFull SlackAppendOutcome = "full"
	// SlackAppendUnbound kept nothing: no live feed reads the install, or it
	// is revoked.
	SlackAppendUnbound SlackAppendOutcome = "unbound"
)

// SlackFeedAppended is what an append did, and to which feed.
type SlackFeedAppended struct {
	Outcome SlackAppendOutcome
	FeedID  string
	// Rows is how many rows it kept.
	Rows int
}

// SlackFeedBudget bounds one read of the buffer: at most Rows rows, and
// Bytes of payload unless the first row alone is more.
type SlackFeedBudget struct {
	Rows  int
	Bytes int64
}

// SlackDayState is how much of a channel-day the mirror holds: a closed set.
type SlackDayState string

const (
	// SlackDayPartial: coverage of the day isn't known whole; no document.
	SlackDayPartial SlackDayState = "partial"
	// SlackDayComplete: the backfill finished the day, or events covered it
	// from its start with no open gap.
	SlackDayComplete SlackDayState = "complete"
	// SlackDayLossy: a gap or failure the budget couldn't repair; its
	// document says messages may be missing.
	SlackDayLossy SlackDayState = "lossy"
)

// ParseSlackDayState reads a state, refusing any other.
func ParseSlackDayState(s string) (SlackDayState, error) {
	switch st := SlackDayState(s); st {
	case SlackDayPartial, SlackDayComplete, SlackDayLossy:
		return st, nil
	}
	return "", fmt.Errorf("unknown Slack day state %q", s)
}

// SlackDayKey names one channel-day of a feed's mirror.
type SlackDayKey struct {
	ChannelID string
	Day       time.Time
}

// SlackFeedDay is one channel-day of the mirror's index: the immutable
// object holding it, what it holds, and when its document was offered.
type SlackFeedDay struct {
	SlackDayKey
	ObjectKey string
	// Digest is the SHA-256, in hex, of what the object holds.
	Digest   string
	Messages int
	Bytes    int
	State    SlackDayState
	// UpdatedSeq is the fold that wrote it: the change read renders the days
	// above its cursor.
	UpdatedSeq   int64
	DocumentedAt *time.Time
}

// Validate refuses an index row the store can't keep.
func (d SlackFeedDay) Validate() error {
	switch {
	case !ValidSlackID(d.ChannelID):
		return fmt.Errorf("Slack day channel %q is not a Slack id", d.ChannelID)
	case !d.Day.Equal(SlackDay(d.Day)):
		return fmt.Errorf("a Slack day is a UTC midnight, not %s", d.Day)
	case d.ObjectKey == "" || len(d.ObjectKey) > 512:
		return fmt.Errorf("a Slack day's object key is 1 to 512 bytes")
	case !ValidSHA256Hex(d.Digest):
		return fmt.Errorf("a Slack day's digest is a SHA-256")
	case d.Messages < 0 || d.Bytes < 0:
		return fmt.Errorf("a Slack day holds no fewer than nothing")
	}
	_, err := ParseSlackDayState(string(d.State))
	return err
}

// SlackFold is a fold's commit: the buffer rows (From, To] applied, the
// days it wrote (each at UpdatedSeq To), the objects they replaced, and the
// days it dropped (left the window), whose objects go too.
type SlackFold struct {
	FeedID   string
	From, To int64
	Days     []SlackFeedDay
	Replaced []string
	Dropped  []SlackDayKey
}

// SlackFeedBind is a connector's binding to its workspace's install of
// Helix's Slack app, as the API asks it: the install it read the channels
// through, the scopes a feed needs, the notice the binding owner or admin
// accepted, and the channels to read, in the order the backfill reads them.
type SlackFeedBind struct {
	InstallID     string
	Scopes        []string
	NoticeVersion int
	BoundBy       string
	HistoryDays   int
	Channels      []string
}

// Validate refuses a binding the store can't keep.
func (b SlackFeedBind) Validate() error {
	switch {
	case b.InstallID == "" || b.BoundBy == "":
		return fmt.Errorf("a Slack feed binding names its install and who bound it")
	case b.NoticeVersion < 1:
		return fmt.Errorf("a Slack feed binding names the notice accepted")
	case b.HistoryDays < 1 || b.HistoryDays > SlackAppHistoryDaysMax:
		return fmt.Errorf("a Slack feed keeps 1 to %d days of history, not %d", SlackAppHistoryDaysMax, b.HistoryDays)
	}
	seen := map[string]bool{}
	for _, c := range b.Channels {
		if !ValidSlackID(c) || seen[c] {
			return fmt.Errorf("Slack feed channel %q is not a Slack id, or is named twice", c)
		}
		seen[c] = true
	}
	return nil
}

// SlackAppSwitch is what an update does to a Slack connector's credential:
// a closed set.
type SlackAppSwitch interface{ slackAppSwitch() }

// SlackAppBindNow reads through the app from now: the connector is pushed
// and reads in full from the mirror.
type SlackAppBindNow struct{ Bind SlackFeedBind }

// SlackAppBindStaged binds the app while the connector keeps syncing by its
// token, cursor and all, until the backfill covers the window.
type SlackAppBindStaged struct{ Bind SlackFeedBind }

// SlackAppUnbind reads with a token again: the feed is unbound and the
// connector reads in full.
type SlackAppUnbind struct{}

func (SlackAppBindNow) slackAppSwitch()    {}
func (SlackAppBindStaged) slackAppSwitch() {}
func (SlackAppUnbind) slackAppSwitch()     {}

// SlackFeedRefusalReason is why a binding was refused: a closed set the API
// words for the person.
type SlackFeedRefusalReason string

const (
	// SlackFeedNotInstalled: the workspace has no live install, or another
	// than the one the channels were read through.
	SlackFeedNotInstalled SlackFeedRefusalReason = "not_installed"
	// SlackFeedScopes: the install lacks a scope the feed needs.
	SlackFeedScopes SlackFeedRefusalReason = "scopes"
	// SlackFeedAdmin: no Slack admin or owner installed it.
	SlackFeedAdmin SlackFeedRefusalReason = "admin"
	// SlackFeedBotUnknown: the install's bot_id isn't known.
	SlackFeedBotUnknown SlackFeedRefusalReason = "bot_unknown"
	// SlackFeedInUse: another connector reads the team (ConnectorID).
	SlackFeedInUse SlackFeedRefusalReason = "in_use"
	// SlackFeedEnterprise: a connector reads another team of the Grid org
	// (ConnectorID, of WorkspaceID, maybe another workspace's).
	SlackFeedEnterprise SlackFeedRefusalReason = "enterprise"
)

// SlackFeedRefusal is a binding the rules refuse now.
type SlackFeedRefusal struct {
	Reason SlackFeedRefusalReason
	// ConnectorID and WorkspaceID name the feed in the way, when one is.
	ConnectorID string
	WorkspaceID string
}

func (r SlackFeedRefusal) Error() string {
	if r.ConnectorID == "" {
		return "Slack feed refused: " + string(r.Reason)
	}
	return "Slack feed refused: " + string(r.Reason) + " (connector " + r.ConnectorID + ")"
}

// SlackSlotMethod is a Tier-1 method the slots pace: a closed set.
type SlackSlotMethod string

const (
	SlackSlotHistory SlackSlotMethod = "history"
	SlackSlotReplies SlackSlotMethod = "replies"
)

// SlackSlotMethods are every method an install has a slot for.
var SlackSlotMethods = []SlackSlotMethod{SlackSlotHistory, SlackSlotReplies}

// ParseSlackSlotMethod reads a method, refusing any other.
func ParseSlackSlotMethod(s string) (SlackSlotMethod, error) {
	switch m := SlackSlotMethod(s); m {
	case SlackSlotHistory, SlackSlotReplies:
		return m, nil
	}
	return "", fmt.Errorf("unknown Slack slot method %q", s)
}

// SlackBackfillReason is why a window is read: a closed set.
type SlackBackfillReason string

const (
	// SlackBackfillBound: the channel was readable when the feed bound.
	SlackBackfillBound SlackBackfillReason = "bound"
	// SlackBackfillJoined: the channel became readable later.
	SlackBackfillJoined SlackBackfillReason = "joined"
	// SlackBackfillGap: messages of a gap may be missing.
	SlackBackfillGap SlackBackfillReason = "gap"
)

// ParseSlackBackfillReason reads a reason, refusing any other.
func ParseSlackBackfillReason(s string) (SlackBackfillReason, error) {
	switch r := SlackBackfillReason(s); r {
	case SlackBackfillBound, SlackBackfillJoined, SlackBackfillGap:
		return r, nil
	}
	return "", fmt.Errorf("unknown Slack backfill reason %q", s)
}

// SlackBackfillStatus is where a window is: a closed set.
type SlackBackfillStatus string

const (
	SlackBackfillQueued    SlackBackfillStatus = "queued"
	SlackBackfillRunning   SlackBackfillStatus = "running"
	SlackBackfillDone      SlackBackfillStatus = "done"
	SlackBackfillCancelled SlackBackfillStatus = "cancelled"
	SlackBackfillFailed    SlackBackfillStatus = "failed"
)

// ParseSlackBackfillStatus reads a status, refusing any other.
func ParseSlackBackfillStatus(s string) (SlackBackfillStatus, error) {
	switch st := SlackBackfillStatus(s); st {
	case SlackBackfillQueued, SlackBackfillRunning, SlackBackfillDone, SlackBackfillCancelled, SlackBackfillFailed:
		return st, nil
	}
	return "", fmt.Errorf("unknown Slack backfill status %q", s)
}

// SlackThreadStatus is where a backfill thread is: a closed set.
type SlackThreadStatus string

const (
	SlackThreadQueued SlackThreadStatus = "queued"
	SlackThreadDone   SlackThreadStatus = "done"
	// SlackThreadGone: Slack has no such thread any more.
	SlackThreadGone SlackThreadStatus = "gone"
)

// ParseSlackThreadStatus reads a status, refusing any other.
func ParseSlackThreadStatus(s string) (SlackThreadStatus, error) {
	switch st := SlackThreadStatus(s); st {
	case SlackThreadQueued, SlackThreadDone, SlackThreadGone:
		return st, nil
	}
	return "", fmt.Errorf("unknown Slack backfill thread status %q", s)
}

// SlackBackfillItem is one piece of backfill work: a closed set.
type SlackBackfillItem interface{ slackBackfillItem() }

// SlackBackfillWindow is a span of a channel's history to read, newest
// first: [Oldest, Latest), its next page ending at Latest.
type SlackBackfillWindow struct {
	FeedID    string
	ChannelID string
	Oldest    time.Time
	Reason    SlackBackfillReason
	// Place is a bound window's in the channel order the bind was given.
	Place    int
	Status   SlackBackfillStatus
	Latest   time.Time
	Cursor   string // "" before the first page, or between pages
	Attempts int
	Pages    int
	Messages int
	Threads  int
	// CoveredS is how many seconds of the window were read.
	CoveredS  int64
	Error     string
	UpdatedAt time.Time
}

// SlackBackfillThread is a thread whose replies the backfill reads.
type SlackBackfillThread struct {
	FeedID    string
	ChannelID string
	ThreadTS  string
	// Day is the UTC day of its root, which its replies land on.
	Day    time.Time
	Cursor string
	Status SlackThreadStatus
}

func (*SlackBackfillWindow) slackBackfillItem() {}
func (*SlackBackfillThread) slackBackfillItem() {}

// SlackBackfillWork is a feed's next piece of backfill work for a method
// whose slot is due.
type SlackBackfillWork struct {
	Feed *SlackFeed
	Item SlackBackfillItem
}

// SlackBackfillStep is what one call's answer does to its work: a closed set.
type SlackBackfillStep interface{ slackBackfillStep() }

// SlackHistoryStep is a conversations.history page of Window, as the store
// gave it (its Latest and Cursor are the compare-and-set): its rows, where
// the next page ends, the threads it found and what it counted.
type SlackHistoryStep struct {
	Window   SlackBackfillWindow
	Latest   time.Time
	Cursor   string
	Done     bool
	Rows     []SlackFeedRow
	Threads  []SlackBackfillThread
	Messages int
	CoveredS int64
}

// SlackRepliesStep is a conversations.replies page of Thread, as the store
// gave it (its Cursor is the compare-and-set): its rows, the next page's
// cursor, and the thread's status after it.
type SlackRepliesStep struct {
	Thread SlackBackfillThread
	Cursor string
	Status SlackThreadStatus
	Rows   []SlackFeedRow
}

func (SlackHistoryStep) slackBackfillStep() {}
func (SlackRepliesStep) slackBackfillStep() {}

// SlackChannelJoin is a channel newly readable by a feed: events hold it
// from CoveredFrom, and a joined window reads [Oldest, CoveredFrom).
type SlackChannelJoin struct {
	ChannelID   string
	CoveredFrom time.Time
	Oldest      time.Time
}

// SlackBackfillProgress is what a feed's backfill has left: its bound and
// joined windows, how many gap windows are still queued or being read, the
// threads still queued, when each slot may call next, and how many days are
// lossy.
type SlackBackfillProgress struct {
	Windows       []SlackBackfillWindow
	LiveGaps      int
	QueuedThreads int
	HistoryAt     time.Time
	RepliesAt     time.Time
	LossyDays     int
}

// SlackInstallBotUpdate is what Finish learned of an install's bot and its
// installer; nil keeps the stored value.
type SlackInstallBotUpdate struct {
	BotID          *string
	InstallerAdmin *bool
}
