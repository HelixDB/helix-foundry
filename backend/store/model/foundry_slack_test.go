package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func ExampleSlackConversation() {
	// Per person, each Slack user is a conversation of their own in a thread;
	// in bot mode the thread is one conversation.
	ada := SlackConversation("01INSTALL", "C1", "1712345678.000100", "UADA")
	grace := SlackConversation("01INSTALL", "C1", "1712345678.000100", "UGRACE")
	fmt.Println(len(ada), ada == grace, ValidSHA256Hex(ada))
	// Output: 64 false true
}

func ExampleSlackEventDedupe() {
	fmt.Println(SlackEventDedupe("Ev0123ABCD"), ValidSlackDedupe(SlackEventDedupe("Ev0123ABCD")))
	// Output: e:Ev0123ABCD true
}

func ExampleSlackClickDedupe() {
	k := SlackClickDedupe("T1", "UADA", "1712345678.000200", "foundry.approve")
	fmt.Println(k[:2], len(k), ValidSlackDedupe(k))
	// Output: i: 66 true
}

func ExampleValidSlackTS() {
	fmt.Println(ValidSlackTS("1712345678.000100"), ValidSlackTS("1712345678"), ValidSlackTS("a.1"), ValidSlackTS(".1"), ValidSlackTS("1."))
	// Output: true false false false false
}

func ExampleValidSlackID() {
	fmt.Println(ValidSlackID("T0123ABCD"), ValidSlackID("1712345678.000100"), ValidSlackID("<@U1>"), ValidSlackID(""))
	// Output: true true false false
}

func ExampleParseFoundryAgentSurface() {
	slack, err := ParseFoundryAgentSurface("slack")
	fmt.Println(slack, err, FoundryAgentSurface(0))
	_, err = ParseFoundryAgentSurface("email")
	fmt.Println(err)
	// Output:
	// slack <nil> site
	// unknown foundry agent surface "email"
}

// Every enum reads back what it writes and refuses anything else.
func TestSlackEnumsParse(t *testing.T) {
	for _, k := range []SlackIntakeKind{SlackIntakeEvent, SlackIntakeInteraction, SlackIntakeShortcut} {
		if got, err := ParseSlackIntakeKind(string(k)); err != nil || got != k {
			t.Fatalf("kind %q: %q, %v", k, got, err)
		}
	}
	for _, s := range []SlackIntakeStatus{SlackIntakeQueued, SlackIntakeRunning, SlackIntakeDone, SlackIntakeFailed} {
		if got, err := ParseSlackIntakeStatus(string(s)); err != nil || got != s {
			t.Fatalf("status %q: %q, %v", s, got, err)
		}
	}
	for _, s := range []SlackReplyStatus{SlackReplyFollowing, SlackReplyDone} {
		if got, err := ParseSlackReplyStatus(string(s)); err != nil || got != s {
			t.Fatalf("reply status %q: %q, %v", s, got, err)
		}
	}
	for _, s := range []FoundryAgentSurface{FoundryAgentSurfaceSite, FoundryAgentSurfaceSlack} {
		if got, err := ParseFoundryAgentSurface(s.String()); err != nil || got != s {
			t.Fatalf("surface %v: %v, %v", s, got, err)
		}
	}
	for _, bad := range []string{"", "Event", "click", "unknown"} {
		if _, err := ParseSlackIntakeKind(bad); err == nil {
			t.Fatalf("kind %q read", bad)
		}
		if _, err := ParseSlackIntakeStatus(bad); err == nil {
			t.Fatalf("status %q read", bad)
		}
		if _, err := ParseSlackReplyStatus(bad); err == nil {
			t.Fatalf("reply status %q read", bad)
		}
		if _, err := ParseFoundryAgentSurface(bad); err == nil {
			t.Fatalf("surface %q read", bad)
		}
	}
	if got := FoundryAgentSurface(7).String(); got != "FoundryAgentSurface(7)" {
		t.Fatalf("an unknown surface reads %q", got)
	}
	if _, err := ParseFoundryAgentSurface(FoundryAgentSurface(7).String()); err == nil {
		t.Fatal("an unknown surface round-trips")
	}
	// The closed sets' markers.
	for _, m := range []SlackIdentityMode{SlackPerPerson{}, SlackSharedBot{CredentialID: "c"}} {
		m.slackIdentityMode()
	}
	for _, o := range []SlackIntakeOutcome{SlackIntakeEnded{}, SlackIntakeGaveUp{}, SlackIntakeRetry{}, SlackIntakeWait{}} {
		o.slackIntakeOutcome()
	}
	if got := (SlackRefusal{Reason: SlackTeamTaken, Detail: "taken"}).Error(); got != "team_taken: taken" {
		t.Fatalf("refusal reads %q", got)
	}
}

func validInstall() *SlackInstall {
	return &SlackInstall{ID: "01INSTALL", WorkspaceID: "01WS", TeamID: "T1", TeamName: "Acme", AppID: "A1", BotUserID: "UBOT",
		Scopes: []string{"chat:write", "users:read"}, InstalledByUserID: "user-ada", InstalledBySlackUserID: "UADA"}
}

func TestSlackInstallValidate(t *testing.T) {
	if err := validInstall().Validate(); err != nil {
		t.Fatal(err)
	}
	grid := validInstall()
	grid.EnterpriseID = "E1"
	if err := grid.Validate(); err != nil {
		t.Fatalf("a Grid workspace's install: %v", err)
	}
	for name, change := range map[string]func(*SlackInstall){
		"no id":                   func(i *SlackInstall) { i.ID = "" },
		"no workspace":            func(i *SlackInstall) { i.WorkspaceID = "" },
		"no installer":            func(i *SlackInstall) { i.InstalledByUserID = "" },
		"no team":                 func(i *SlackInstall) { i.TeamID = "" },
		"a team that isn't an id": func(i *SlackInstall) { i.TeamID = "T 1" },
		"a long enterprise":       func(i *SlackInstall) { i.EnterpriseID = strings.Repeat("E", 33) },
		"no app":                  func(i *SlackInstall) { i.AppID = "" },
		"no bot user":             func(i *SlackInstall) { i.BotUserID = "" },
		"no Slack installer":      func(i *SlackInstall) { i.InstalledBySlackUserID = "" },
		"a blank name":            func(i *SlackInstall) { i.TeamName = "  " },
		"a long name":             func(i *SlackInstall) { i.TeamName = strings.Repeat("n", 256) },
		"no scopes":               func(i *SlackInstall) { i.Scopes = nil },
		"too many scopes":         func(i *SlackInstall) { i.Scopes = []string{strings.Repeat("s", 1025)} },
		"an empty scope":          func(i *SlackInstall) { i.Scopes = []string{"chat:write", ""} },
		"a scope with a comma":    func(i *SlackInstall) { i.Scopes = []string{"chat:write,users:read"} },
		"a scope with a space":    func(i *SlackInstall) { i.Scopes = []string{"chat:write users:read"} },
	} {
		i := validInstall()
		change(i)
		if err := i.Validate(); err == nil {
			t.Fatalf("%s was valid", name)
		}
	}
}

func TestSlackIntakeValidate(t *testing.T) {
	valid := func() *SlackIntake {
		return &SlackIntake{ID: "01IN", InstallID: "01INSTALL", DedupeKey: SlackEventDedupe("Ev1"), Kind: SlackIntakeEvent,
			Conversation: SlackConversation("01INSTALL", "C1", "1.2", "UADA"), SlackUserID: "UADA", Payload: json.RawMessage(`{"type":"mention"}`)}
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	click := valid()
	click.Kind, click.DedupeKey, click.Conversation = SlackIntakeInteraction, SlackClickDedupe("T1", "UADA", "1.3", "foundry.stop"), ""
	if err := click.Validate(); err != nil {
		t.Fatalf("a click: %v", err)
	}
	for name, change := range map[string]func(*SlackIntake){
		"no id":                        func(i *SlackIntake) { i.ID = "" },
		"no install":                   func(i *SlackIntake) { i.InstallID = "" },
		"an unknown kind":              func(i *SlackIntake) { i.Kind = "message" },
		"no dedupe key":                func(i *SlackIntake) { i.DedupeKey = "" },
		"a dedupe key of no kind":      func(i *SlackIntake) { i.DedupeKey = "Ev1" },
		"an unknown dedupe kind":       func(i *SlackIntake) { i.DedupeKey = "x:Ev1" },
		"an event id that isn't one":   func(i *SlackIntake) { i.DedupeKey = "e:" },
		"a click key that isn't a sum": func(i *SlackIntake) { i.DedupeKey = "i:abc" },
		"an uppercase sum":             func(i *SlackIntake) { i.DedupeKey = "i:" + strings.Repeat("A", 64) },
		"a conversation not hashed":    func(i *SlackIntake) { i.Conversation = "C1/1.2" },
		"a sum that isn't hex":         func(i *SlackIntake) { i.Conversation = strings.Repeat("g", 64) },
		"a user that isn't an id":      func(i *SlackIntake) { i.SlackUserID = "@ada" },
		"no payload":                   func(i *SlackIntake) { i.Payload = nil },
		"a null payload":               func(i *SlackIntake) { i.Payload = json.RawMessage(`null`) },
		"a payload that isn't an object": func(i *SlackIntake) {
			i.Payload = json.RawMessage(`["mention"]`)
		},
	} {
		i := valid()
		change(i)
		if err := i.Validate(); err == nil {
			t.Fatalf("%s was valid", name)
		}
	}
}

func TestSlackReplyValidate(t *testing.T) {
	r := &SlackReply{TurnID: "01TURN", InstallID: "01INSTALL", ChannelID: "C1", ThreadTS: "1.2", SlackUserID: "UADA"}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*SlackReply){
		"no turn":       func(r *SlackReply) { r.TurnID = "" },
		"no install":    func(r *SlackReply) { r.InstallID = "" },
		"no channel":    func(r *SlackReply) { r.ChannelID = "" },
		"no thread":     func(r *SlackReply) { r.ThreadTS = "" },
		"no Slack user": func(r *SlackReply) { r.SlackUserID = "" },
	} {
		bad := *r
		change(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatalf("%s was valid", name)
		}
	}

	seq, ts, wait := 3, "1712345678.000300", 1500*time.Millisecond
	full := SlackReplyUpdate{Lease: time.Minute, AfterSeq: &seq, MessageTS: &ts, UpdateAfter: &wait,
		Posted: map[string]string{"q:01Q": SlackPostPending, "a:01A": ts, "k:01TURN": ts, "q:01R": SlackPostRefused, "q:01R.1": ts}}
	if err := full.Validate(); err != nil {
		t.Fatal(err)
	}
	for v, done := range map[string]bool{ts: true, SlackPostRefused: true, SlackPostPending: false, "": false, "posted": false} {
		if SlackPostDone(v) != done {
			t.Fatalf("SlackPostDone(%q) is %v", v, !done)
		}
	}
	neg, badTS, past := -1, "not a ts", -time.Second
	for name, u := range map[string]SlackReplyUpdate{
		"no lease":                  {},
		"a negative seq":            {Lease: time.Minute, AfterSeq: &neg},
		"a ts that isn't one":       {Lease: time.Minute, MessageTS: &badTS},
		"an update in the past":     {Lease: time.Minute, UpdateAfter: &past},
		"a post of no kind":         {Lease: time.Minute, Posted: map[string]string{"01Q": ts}},
		"a post of an unknown kind": {Lease: time.Minute, Posted: map[string]string{"x:01Q": ts}},
		"a post with no id":         {Lease: time.Minute, Posted: map[string]string{"q:": ts}},
		"a post's value not a ts":   {Lease: time.Minute, Posted: map[string]string{"q:01Q": "posted"}},
	} {
		if err := u.Validate(); err == nil {
			t.Fatalf("%s was valid", name)
		}
	}
}
