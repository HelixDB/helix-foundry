package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func ExampleSlackFeedGap_Row() {
	gap := SlackMinuteGap("C1", time.Date(2026, 11, 20, 10, 4, 33, 0, time.UTC))
	row := gap.Row("C1")
	fmt.Println(row.Kind, row.ChannelID, row.Day.Format(time.DateOnly), string(row.Payload))
	fmt.Println(row.DedupeKey == gap.Row("C1").DedupeKey, row.DedupeKey == gap.Row("C2").DedupeKey, row.Validate() == nil)
	// Output:
	// gap C1 2026-11-20 {"from":"2026-11-20T10:04:00Z","to":"2026-11-20T10:05:00Z"}
	// true false true
}

func ExampleSlackBackfillDedupe() {
	first := SlackBackfillDedupe("C1", "1712345678.000100", "", SlackRowMessage)
	edited := SlackBackfillDedupe("C1", "1712345678.000100", "1712345690.000200", SlackRowMessage)
	fmt.Println(first[:2], len(first), first == edited, ValidSlackFeedDedupe(first))
	// Output: b: 66 false true
}

func ExampleSlackFeedRow_Minute() {
	appended := time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC) // a relay held the row for hours
	posted := SlackFeedRow{Kind: SlackRowMessage, Payload: json.RawMessage(`{"ts":"1795183473.000100","text":"hi"}`)}
	fmt.Println(posted.Minute(appended).Format(time.RFC3339))
	// Output: 2026-11-20T14:04:00Z
}

func TestSlackFeedRowMinute(t *testing.T) {
	at := time.Date(2026, 11, 20, 18, 0, 30, 0, time.UTC)
	for _, c := range []struct {
		kind    SlackFeedRowKind
		payload string
		want    string
	}{
		{SlackRowMessage, `{"ts":"1795183473.000100"}`, "14:04"},
		{SlackRowChanged, `{"ts":"1795183473.000100","edited":{"ts":"1795184000.000100"}}`, "14:04"},
		{SlackRowDeleted, `{"deleted_ts":"1795183473.000100","ts":"1795186800.000100"}`, "14:04"},
		{SlackRowReplied, `{"ts":"1795176000.000100","latest_reply":"1795183473.000100"}`, "14:04"},
		{SlackRowReplied, `{"ts":"1795183473.000100"}`, "14:04"},
		{SlackRowMessage, `{"ts":"1999999999.000100"}`, "18:00"}, // never after the append
		{SlackRowMessage, `{"ts":"soon"}`, "18:00"},
		{SlackRowMessage, `{}`, "18:00"},
		{SlackRowJoined, `{"ts":"1795183473.000100"}`, "18:00"},
		{SlackRowSpan, `{"from":"1795183473.000100"}`, "18:00"},
	} {
		if got := (SlackFeedRow{Kind: c.kind, Payload: json.RawMessage(c.payload)}).Minute(at).Format("15:04"); got != c.want {
			t.Errorf("%s %s: minute %s, want %s", c.kind, c.payload, got, c.want)
		}
	}
}

func ExampleSlackDay() {
	fmt.Println(SlackDay(time.Date(2026, 11, 20, 23, 30, 0, 0, time.FixedZone("PST", -8*3600))).Format(time.RFC3339))
	// Output: 2026-11-21T00:00:00Z
}

func TestSlackFeedParsers(t *testing.T) {
	for _, c := range []struct {
		parse func(string) (string, error)
		good  []string
	}{
		{func(s string) (string, error) { k, err := ParseSlackFeedRowKind(s); return string(k), err },
			[]string{"message", "changed", "deleted", "replied", "channel", "joined", "left", "span", "complete", "gap"}},
		{func(s string) (string, error) { k, err := ParseSlackFeedSource(s); return string(k), err }, []string{"event", "backfill"}},
		{func(s string) (string, error) { k, err := ParseSlackDayState(s); return string(k), err }, []string{"partial", "complete", "lossy"}},
		{func(s string) (string, error) { k, err := ParseSlackSlotMethod(s); return string(k), err }, []string{"history", "replies"}},
		{func(s string) (string, error) { k, err := ParseSlackBackfillReason(s); return string(k), err }, []string{"bound", "joined", "gap"}},
		{func(s string) (string, error) { k, err := ParseSlackBackfillStatus(s); return string(k), err },
			[]string{"queued", "running", "done", "cancelled", "failed"}},
		{func(s string) (string, error) { k, err := ParseSlackThreadStatus(s); return string(k), err }, []string{"queued", "done", "gone"}},
	} {
		for _, g := range c.good {
			if got, err := c.parse(g); err != nil || got != g {
				t.Errorf("%q reads %q, %v", g, got, err)
			}
		}
		for _, bad := range []string{"", "other", strings.ToUpper(c.good[0])} {
			if _, err := c.parse(bad); err == nil {
				t.Errorf("%q is read", bad)
			}
		}
	}
	if len(SlackSlotMethods) != 2 {
		t.Errorf("every slot method: %v", SlackSlotMethods)
	}
}

func TestSlackFeedRowValidate(t *testing.T) {
	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	good := SlackFeedRow{DedupeKey: SlackEventDedupe("Ev1"), ChannelID: "C1", Day: day, Kind: SlackRowMessage, Source: SlackFeedFromEvent,
		Payload: json.RawMessage(`{}`)}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]func(*SlackFeedRow){
		"key":     func(r *SlackFeedRow) { r.DedupeKey = "x:1" },
		"channel": func(r *SlackFeedRow) { r.ChannelID = "C 1" },
		"day":     func(r *SlackFeedRow) { r.Day = day.Add(time.Hour) },
		"local":   func(r *SlackFeedRow) { r.Day = time.Date(2026, 11, 20, 0, 0, 0, 0, time.FixedZone("X", 3600)) },
		"payload": func(r *SlackFeedRow) { r.Payload = json.RawMessage(`{`) },
		"empty":   func(r *SlackFeedRow) { r.Payload = nil },
		"kind":    func(r *SlackFeedRow) { r.Kind = "edit" },
		"source":  func(r *SlackFeedRow) { r.Source = "relay" },
	} {
		r := good
		bad(&r)
		if r.Validate() == nil {
			t.Errorf("a row with a bad %s is kept", name)
		}
	}
}

func TestValidSlackFeedDedupe(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	for k, want := range map[string]bool{
		"e:Ev1": true, "b:" + hash: true, "g:" + hash: true,
		"e:": false, "e:Ev 1": false, "b:Ev1": false, "g:" + strings.ToUpper(hash): false, "i:" + hash: false, "Ev1": false, "": false,
	} {
		if got := ValidSlackFeedDedupe(k); got != want {
			t.Errorf("%q: %v", k, got)
		}
	}
}

func TestSlackFeedGapValidate(t *testing.T) {
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		gap SlackFeedGap
		ok  bool
	}{
		{SlackMinuteGap("C1", at), true},
		{SlackMinuteGap("", at), true},
		{SlackMinuteGap("C 1", at), false},
		{SlackFeedGap{ChannelID: "C1", From: at, To: at}, false},
		{SlackFeedGap{ChannelID: "C1", From: at, To: at.Add(-time.Second)}, false},
	} {
		if err := c.gap.Validate(); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.gap, err)
		}
	}
}

func TestSlackFeedDayValidate(t *testing.T) {
	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	good := SlackFeedDay{SlackDayKey: SlackDayKey{ChannelID: "C1", Day: day}, ObjectKey: "k", Digest: strings.Repeat("0", 64), State: SlackDayComplete}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]func(*SlackFeedDay){
		"channel":  func(d *SlackFeedDay) { d.ChannelID = "" },
		"day":      func(d *SlackFeedDay) { d.Day = day.Add(time.Minute) },
		"key":      func(d *SlackFeedDay) { d.ObjectKey = "" },
		"long key": func(d *SlackFeedDay) { d.ObjectKey = strings.Repeat("k", 513) },
		"digest":   func(d *SlackFeedDay) { d.Digest = "abc" },
		"messages": func(d *SlackFeedDay) { d.Messages = -1 },
		"bytes":    func(d *SlackFeedDay) { d.Bytes = -1 },
		"state":    func(d *SlackFeedDay) { d.State = "done" },
	} {
		d := good
		bad(&d)
		if d.Validate() == nil {
			t.Errorf("a day with a bad %s is kept", name)
		}
	}
}

func TestSlackFeedBindValidate(t *testing.T) {
	good := SlackFeedBind{InstallID: "I1", NoticeVersion: 1, BoundBy: "user-ada", HistoryDays: SlackAppHistoryDaysMax, Channels: []string{"C1", "C2"}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]func(*SlackFeedBind){
		"install":      func(b *SlackFeedBind) { b.InstallID = "" },
		"bound by":     func(b *SlackFeedBind) { b.BoundBy = "" },
		"notice":       func(b *SlackFeedBind) { b.NoticeVersion = 0 },
		"no history":   func(b *SlackFeedBind) { b.HistoryDays = 0 },
		"long history": func(b *SlackFeedBind) { b.HistoryDays = SlackAppHistoryDaysMax + 1 },
		"channel":      func(b *SlackFeedBind) { b.Channels = []string{""} },
		"twice":        func(b *SlackFeedBind) { b.Channels = []string{"C1", "C1"} },
	} {
		b := good
		bad(&b)
		if b.Validate() == nil {
			t.Errorf("a binding with a bad %s is kept", name)
		}
	}
}

func TestSlackFeedRefusalError(t *testing.T) {
	if got := (SlackFeedRefusal{Reason: SlackFeedAdmin}).Error(); got != "Slack feed refused: admin" {
		t.Error(got)
	}
	if got := (SlackFeedRefusal{Reason: SlackFeedInUse, ConnectorID: "c1"}).Error(); got != "Slack feed refused: in_use (connector c1)" {
		t.Error(got)
	}
}
