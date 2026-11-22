package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func ExampleParseRelayRedirect() {
	for _, uri := range []string{
		"http://localhost:3000/settings/slack",
		"http://127.0.0.1/settings/slack",
		"https://localhost:3000/settings/slack",
		"http://evil.example:3000/settings/slack",
		"http://localhost:3000/settings/slack?next=/",
	} {
		origin, err := ParseRelayRedirect(uri)
		fmt.Printf("%q %v\n", origin, err == nil)
	}
	// Output:
	// "http://localhost:3000" true
	// "http://127.0.0.1" true
	// "" false
	// "" false
	// "" false
}

func ExampleRelayCodeChallenge() {
	// RFC 7636, appendix B.
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	challenge := RelayCodeChallenge(verifier)
	fmt.Println(challenge, ValidRelayVerifier(verifier), ValidRelayChallenge(challenge))
	// Output: E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM true true
}

func ExampleRelayLink_ReturnTo() {
	fmt.Println(RelayLink{RedirectOrigin: "http://localhost:3000"}.ReturnTo())
	// Output: http://localhost:3000/settings/slack
}

func ExampleSlackDestinationKey() {
	fmt.Println(SlackDestinationKey("ws1", nil), SlackDestinationKey("ws1", SlackServesWorkspace{}),
		SlackDestinationKey("ws1", SlackServesRelay{LinkID: "l1", LocalWorkspaceID: "local1"}))
	// Output: ws1/ ws1/ ws1/local1
}

func ExampleParseSlackLinkAccess() {
	full, _ := ParseSlackLinkAccess("full")
	read, _ := ParseSlackLinkAccess("read")
	_, err := ParseSlackLinkAccess("admin")
	fmt.Printf("%T %T %v\n", full, read, err)
	// Output: model.SlackFullAccess model.SlackReadAccess unknown Slack link access "admin"
}

// Every redirect that isn't a local install's own Settings › Slack is
// refused.
func TestParseRelayRedirect(t *testing.T) {
	for uri, want := range map[string]string{
		"http://localhost:3000/settings/slack":  "http://localhost:3000",
		"http://localhost/settings/slack":       "http://localhost",
		"http://127.0.0.1:65535/settings/slack": "http://127.0.0.1:65535",
		"http://localhost:1/settings/slack":     "http://localhost:1",
	} {
		if got, err := ParseRelayRedirect(uri); err != nil || got != want {
			t.Errorf("%s: %q, %v", uri, got, err)
		}
	}
	for _, uri := range []string{
		"", ":", "localhost:3000/settings/slack", "http:///settings/slack", "http://localhost:3000",
		"http://localhost:3000/", "http://localhost:3000/settings/slack/", "http://localhost:3000/settings/Slack",
		"http://localhost:3000/settings%2Fslack", "http://localhost:3000/settings/slack?x=1", "http://localhost:3000/settings/slack?",
		"http://localhost:3000/settings/slack#t", "http://ada@localhost:3000/settings/slack", "http://LOCALHOST:3000/settings/slack",
		"http://[::1]:3000/settings/slack", "http://localhost.:3000/settings/slack", "http://127.0.0.2:3000/settings/slack",
		"http://localhost:0/settings/slack", "http://localhost:65536/settings/slack", "http://localhost:03000/settings/slack",
		"http://localhost:abc/settings/slack", "http://localhost:/settings/slack", "http:localhost/settings/slack",
		"http://localhost:3000/settings/slack/../slack",
		"http://localhost:3000/" + strings.Repeat("a", 250) + RelaySettingsPath,
	} {
		if got, err := ParseRelayRedirect(uri); err == nil {
			t.Errorf("%q was taken as %q", uri, got)
		}
	}
}

func TestRelayPKCEAndNames(t *testing.T) {
	for _, v := range []string{strings.Repeat("a", 42), strings.Repeat("a", 129), strings.Repeat("a", 42) + "+", strings.Repeat("é", 43)} {
		if ValidRelayVerifier(v) {
			t.Errorf("verifier %q taken", v)
		}
	}
	if !ValidRelayVerifier(strings.Repeat("aZ9-._~", 18) + "ab") {
		t.Error("a 128-character verifier refused")
	}
	for _, c := range []string{"", strings.Repeat("A", 42), strings.Repeat("A", 44), strings.Repeat("+", 43), strings.Repeat("A", 42) + "B"} {
		if ValidRelayChallenge(c) {
			t.Errorf("challenge %q taken", c)
		}
	}
	for name, want := range map[string]bool{
		"Ada's MacBook": true, "Café": true, strings.Repeat("a", 128): true, strings.Repeat("a", 129): false, "": false, "  ": false,
		"a\nb": false, "a‮b": false, "\xff": false,
	} {
		if ValidRelayName(name) != want {
			t.Errorf("name %q: want %v", name, want)
		}
	}
}

func TestRelayCodeValidate(t *testing.T) {
	good := func() *RelayCode {
		return &RelayCode{CodeHash: strings.Repeat("a", 64), WorkspaceID: "ws1", UserID: "user-ada",
			CodeChallenge: RelayCodeChallenge(strings.Repeat("v", 43)), RedirectURI: "http://localhost:3000/settings/slack", Name: "Ada's Mac"}
	}
	if err := good().Validate(); err != nil {
		t.Fatal(err)
	}
	for what, bad := range map[string]func(*RelayCode){
		"workspace": func(c *RelayCode) { c.WorkspaceID = "" },
		"member":    func(c *RelayCode) { c.UserID = "" },
		"hash":      func(c *RelayCode) { c.CodeHash = "code" },
		"challenge": func(c *RelayCode) { c.CodeChallenge = "plain" },
		"redirect":  func(c *RelayCode) { c.RedirectURI = "https://helix-db.com/settings/slack" },
		"name":      func(c *RelayCode) { c.Name = "" },
	} {
		c := good()
		bad(c)
		if err := c.Validate(); err == nil {
			t.Errorf("a code without its %s was taken", what)
		}
	}
}

func TestRelayDeliveryAndSeen(t *testing.T) {
	d := &RelayDelivery{ID: "d1", InstallID: "i1", LinkID: "l1", LocalWorkspaceID: "local1", DedupeKey: SlackEventDedupe("Ev1"),
		Kind: SlackIntakeEvent, Conversation: SlackConversation("i1", "C1", "1.1", "UADA"), SlackUserID: "UADA", Payload: json.RawMessage(`{"a":1}`)}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	in := d.Intake()
	if in.ID != d.ID || in.InstallID != "i1" || in.DedupeKey != d.DedupeKey || in.Kind != d.Kind || in.Conversation != d.Conversation ||
		in.SlackUserID != "UADA" || string(in.Payload) != `{"a":1}` {
		t.Fatalf("intake %+v", in)
	}
	d.Payload = json.RawMessage(`[]`)
	if d.Validate() == nil {
		t.Fatal("a delivery whose payload isn't an object was taken")
	}
	for _, st := range []RelayDeliveryStatus{RelayQueued, RelayLeased, RelayAcked, RelayExpired} {
		if got, err := ParseRelayDeliveryStatus(string(st)); err != nil || got != st {
			t.Fatalf("status %q: %q, %v", st, got, err)
		}
	}
	if _, err := ParseRelayDeliveryStatus("done"); err == nil {
		t.Fatal("an unknown status was read")
	}
	if err := (RelaySeen{ChannelID: "C1", UserIDs: []string{"UADA"}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (RelaySeen{}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []RelaySeen{{ChannelID: "<#C1>"}, {UserIDs: []string{"UADA", ""}}} {
		if bad.Validate() == nil {
			t.Errorf("%+v taken", bad)
		}
	}
}

func TestRelayFeed(t *testing.T) {
	at := time.Date(2026, 11, 20, 10, 30, 15, 0, time.UTC)
	row := SlackFeedRow{DedupeKey: SlackEventDedupe("Ev1"), ChannelID: "C1", Day: SlackDay(at), Kind: SlackRowMessage, Source: SlackFeedFromEvent,
		Payload: json.RawMessage(`{}`)}
	gap := SlackMinuteGap("C1", at)
	if err := ValidRelayFeedRow(row); err != nil {
		t.Fatal(err)
	}
	for what, bad := range map[string]func(*SlackFeedRow){
		"gap":      func(r *SlackFeedRow) { r.Kind = SlackRowGap },
		"backfill": func(r *SlackFeedRow) { r.Source = SlackFeedFromBackfill },
		"channel":  func(r *SlackFeedRow) { r.ChannelID = "" },
	} {
		r := row
		bad(&r)
		if ValidRelayFeedRow(r) == nil {
			t.Errorf("a %s row was taken", what)
		}
	}
	if k := RelayGapDedupe(gap); k != gap.Row("C1").DedupeKey || !ValidSlackFeedDedupe(k) {
		t.Fatalf("gap key %q", k)
	}
	if RelayGapDedupe(SlackFeedGap{From: gap.From, To: gap.To}) == RelayGapDedupe(gap) {
		t.Fatal("every channel's gap keyed as one channel's")
	}
	for what, a := range map[string]RelayFeedAppend{
		"rows": {Rows: []SlackFeedRow{row}, At: at},
		"gaps": {Gaps: []SlackFeedGap{gap, {From: gap.From, To: gap.To}}, At: at},
		"both": {Rows: []SlackFeedRow{row}, Gaps: []SlackFeedGap{gap}, At: at},
		"two":  {Rows: []SlackFeedRow{row, row}, At: at},
	} {
		if err := a.Validate(); err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	bad := row
	bad.Kind = SlackRowGap
	for what, a := range map[string]RelayFeedAppend{
		"nothing": {At: at},
		"no time": {Rows: []SlackFeedRow{row}},
		"a gap":   {Rows: []SlackFeedRow{bad}, At: at},
		"no span": {Gaps: []SlackFeedGap{{ChannelID: "C1", From: at, To: at}}, At: at},
	} {
		if a.Validate() == nil {
			t.Errorf("an append with %s was taken", what)
		}
	}
	for _, b := range []RelayBudget{{Deliveries: 1}, {Deliveries: 10, FeedRows: 500, FeedBytes: 4 << 20}} {
		if err := b.Validate(); err != nil {
			t.Errorf("%+v: %v", b, err)
		}
	}
	for _, b := range []RelayBudget{{}, {Deliveries: 1, FeedRows: -1}, {Deliveries: 1, FeedBytes: -1}} {
		if b.Validate() == nil {
			t.Errorf("%+v taken", b)
		}
	}
}

func TestSlackAccessRequestValidate(t *testing.T) {
	if err := (&SlackAccessRequest{InstallID: "i1", SlackUserID: "UGRACE", SlackName: "Grace Hopper"}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, r := range []SlackAccessRequest{
		{SlackUserID: "UGRACE", SlackName: "Grace"}, {InstallID: "i1", SlackUserID: "<@U1>", SlackName: "Grace"},
		{InstallID: "i1", SlackUserID: "UGRACE", SlackName: " "}, {InstallID: "i1", SlackUserID: "UGRACE", SlackName: strings.Repeat("g", 256)},
		{InstallID: "i1", SlackUserID: "UGRACE", SlackName: "\xff"},
	} {
		if r.Validate() == nil {
			t.Errorf("%+v taken", r)
		}
	}
}
