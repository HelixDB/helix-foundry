package events_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/events"
)

var limits = events.Limits{MaxPieces: 500, MaxBytes: 1 << 20, MaxPieceBytes: 256 << 10}

func ExampleValidate() {
	at := time.Date(2026, 10, 23, 20, 20, 0, 0, time.FixedZone("+02", 2*60*60))
	send, err := events.Validate([]events.Piece{
		{ID: "o-17", OccurredAt: &at, Data: []byte(`{"amount": 12.5, "customer": "c_1"}`)},
		{ID: "o-18", Data: []byte(`{"amount": 3}`)},
	}, limits)
	fmt.Println(send.Pieces(), send.Bytes(), err)

	_, err = events.Validate([]events.Piece{{ID: "o-19", Data: []byte(`{}`)}, {ID: "o-20", Data: []byte(`{"id": "o-20"}`)}}, limits)
	var r *events.Refusal
	fmt.Println(errors.As(err, &r) && r.Kind == events.Invalid, r.Piece, r.Message)
	// Output:
	// 2 213 <nil>
	// true 2 piece 2 (id "o-20"): data may not have a top-level id; send it as the piece's id
}

func ExampleSend_Data() {
	send, err := events.Validate([]events.Piece{
		{ID: "o-17", Data: []byte(`{ "amount": 12.50, "total": 18446744073709551617, "note": "\ud83d\ude00" }`)},
	}, limits)
	fmt.Println(string(send.Data(0)), err)
	// Output:
	// {"amount":12.50,"total":18446744073709551617,"note":"\ud83d\ude00"} <nil>
}

func ExampleParseOccurredAt() {
	at, err := events.ParseOccurredAt(0, "o-17", "2026-10-23T20:20:00.1234567+02:00")
	fmt.Println(at, err)
	_, err = events.ParseOccurredAt(2, "o-19", "yesterday")
	fmt.Println(err)
	// Output:
	// 2026-10-23 18:20:00.123456 +0000 UTC <nil>
	// piece 3 (id "o-19"): occurred_at is not an RFC 3339 time
}

func ExampleSchema_Merge() {
	var schema events.Schema
	if err := json.Unmarshal([]byte(`{"amount":"number","note":"null"}`), &schema); err != nil {
		panic(err)
	}
	send, _ := events.Validate([]events.Piece{{ID: "o-1", Data: []byte(`{"amount":3,"note":"late","tags":[]}`)}}, limits)
	merged, err := schema.Merge(send, 200)
	fmt.Println(merged.New, merged.Locked, merged.Changed(), err)
	stored, _ := json.Marshal(merged.Schema)
	fmt.Println(string(stored))

	send, _ = events.Validate([]events.Piece{{ID: "o-2", Data: []byte(`{"amount":"3"}`)}}, limits)
	_, err = merged.Schema.Merge(send, 200)
	fmt.Println(err)
	// Output:
	// [{tags array}] [{note string}] true <nil>
	// {"amount":"number","note":"string","tags":"array"}
	// piece 1 (id "o-2"): "amount" is a string; this source holds "amount" as a number
}

func ExampleEncodeLines() {
	at := time.Date(2026, 10, 23, 18, 20, 0, 0, time.UTC)
	send, _ := events.Validate([]events.Piece{
		{ID: "o-17", OccurredAt: &at, Data: []byte(`{"amount":12.5,"customer":"c_1"}`)},
		{ID: "o-18", Data: []byte(`{}`)},
	}, limits)
	obj, err := events.EncodeLines(send, time.Date(2026, 10, 23, 18, 25, 1, 123456789, time.UTC))
	if err != nil {
		panic(err)
	}
	gz, _ := gzip.NewReader(bytes.NewReader(obj.Body))
	lines, _ := io.ReadAll(gz)
	fmt.Print(string(lines))
	fmt.Println(len(lines) == int(send.Bytes()), obj.ReceivedAt, obj.FirstAt, obj.LastAt)
	// Output:
	// {"id":"o-17","received_at":"2026-10-23T18:25:01.123456Z","occurred_at":"2026-10-23T18:20:00Z","data":{"amount":12.5,"customer":"c_1"}}
	// {"id":"o-18","received_at":"2026-10-23T18:25:01.123456Z","data":{}}
	// true 2026-10-23 18:25:01.123456 +0000 UTC 2026-10-23 18:20:00 +0000 UTC 2026-10-23 18:25:01.123456 +0000 UTC
}

func ExampleLine_Row() {
	line, err := events.DecodeLine([]byte(`{"id":"o-17","received_at":"2026-10-23T18:25:01.123456Z","data":{"customer":"c_1","amount":12.50}}`))
	if err != nil {
		panic(err)
	}
	var schema events.Schema
	_ = json.Unmarshal([]byte(`{"amount":"number","customer":"string","note":"null"}`), &schema)
	row, err := line.Row(schema)
	fmt.Println(line.ID(), string(row), err)
	fmt.Println(line.ReceivedAt())
	// Output:
	// o-17 {"id":"o-17","received_at":"2026-10-23T18:25:01.123456Z","occurred_at":null,"amount":12.50,"customer":"c_1","note":null} <nil>
	// 2026-10-23 18:25:01.123456 +0000 UTC
}

func ExampleRender() {
	var schema events.Schema
	_ = json.Unmarshal([]byte(`{"amount":"number","customer":"string","reply":"string"}`), &schema)
	row := []byte(`{"id":"o-17","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":"2026-10-23T18:20:00Z","amount":12.50,"customer":"c_1\nVIP","reply":"Refund sent\n\nThe customer asked for a refund of order o-17."}`)
	doc, err := events.Render(row, schema, "reply")
	fmt.Printf("%s %v\n%s\n", doc.Title, err, doc.Body)
	// Output:
	// Refund sent <nil>
	// # Refund sent
	//
	// - Piece: o-17
	// - Occurred: 2026-10-23T18:20:00Z
	// - amount: 12.50
	// - customer: c_1 VIP
	//
	// Refund sent
	//
	// The customer asked for a refund of order o-17.
}

func ExampleVersion() {
	var schema events.Schema
	_ = json.Unmarshal([]byte(`{"reply":"string"}`), &schema)
	sent, _ := events.Render([]byte(`{"id":"o-17","received_at":"2026-10-24T09:00:00.000000Z","occurred_at":null,"reply":"Refund sent"}`), schema, "reply")
	resent, _ := events.Render([]byte(`{"id":"o-17","received_at":"2026-10-24T11:30:00.000000Z","occurred_at":null,"reply":"Refund sent"}`), schema, "reply")
	fmt.Println(len(events.Version(sent.Body)), events.Version(sent.Body) == events.Version(resent.Body))
	// Output:
	// 64 true
}

func ExampleNewest() {
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	window := events.Newest[int](2)
	for line, id := range []string{"o-1", "o-2", "o-3"} {
		window.Push(at.Add(time.Duration(line)*time.Minute), id, line)
	}
	window.Push(at.Add(2*time.Minute), "o-0", 9)
	for _, p := range window.Pieces() {
		fmt.Println(p.ID, p.ReceivedAt.Format(time.Kitchen), p.Loc)
	}
	// Output:
	// o-0 9:02AM 9
	// o-3 9:02AM 2
}

func ExampleSchema_WithText() {
	var schema events.Schema
	_ = json.Unmarshal([]byte(`{"amount":"number","reply":"null"}`), &schema)
	locked, err := schema.WithText("reply", 200)
	stored, _ := json.Marshal(locked)
	fmt.Println(string(stored), err)
	_, err = schema.WithText("amount", 200)
	fmt.Println(err)
	// Output:
	// {"amount":"number","reply":"string"} <nil>
	// a text field holds strings; this source holds "amount" as a number
}

func ExampleValidFieldName() {
	fmt.Println(events.ValidFieldName("reply"))
	fmt.Println(events.ValidFieldName("received_at"))
	// Output:
	// <nil>
	// received_at is a column of every piece, not a field of its data
}

func ExampleSameSend() {
	pieces := []events.Piece{{ID: "o-17", Data: []byte(`{"amount":12.5}`)}}
	send, _ := events.Validate(pieces, limits)
	stored, _ := events.EncodeLines(send, time.Date(2026, 10, 23, 18, 25, 1, 0, time.UTC))

	retry, _ := events.Validate(pieces, limits)
	same, err := events.SameSend(bytes.NewReader(stored.Body), retry)
	fmt.Println(same, err)

	other, _ := events.Validate([]events.Piece{{ID: "o-17", Data: []byte(`{"amount":12.6}`)}}, limits)
	same, err = events.SameSend(bytes.NewReader(stored.Body), other)
	fmt.Println(other.Bytes() == send.Bytes(), same, err)
	// Output:
	// true <nil>
	// true false <nil>
}

func ExampleHolding_Project() {
	caps := events.Caps{MaxRows: 2_000_000, MaxBytes: 2 << 30}
	h := events.Holding{TableRows: 1_659_000, TableBytes: 400 << 20, TableFields: []string{"id", "received_at", "occurred_at", "amount"}, PendingPieces: 500, PendingBytes: 60_000}
	var schema events.Schema
	_ = json.Unmarshal([]byte(`{"amount":"number"}`), &schema)
	send, _ := events.Validate([]events.Piece{{ID: "o-1", Data: []byte(`{"amount":1}`)}}, limits)
	c, err := h.Project(send, schema, caps)
	fmt.Println(c.Rows, c.Bytes, c.Percent(), err)
	fmt.Println(c.Warning())

	h.TableRows = 1_999_100
	big := make([]events.Piece, 500)
	for i := range big {
		big[i] = events.Piece{ID: fmt.Sprint("p", i), Data: []byte(`{}`)}
	}
	send, _ = events.Validate(big, limits)
	_, err = h.Project(send, schema, caps)
	fmt.Println(err)
	// Output:
	// 1659501 419502501 82 <nil>
	// This source is 82% full (1,659,501 of 2,000,000 pieces). Once it is full, sends are refused: send new pieces to a new source before then.
	// This source holds 1,999,600 pieces, and one source holds at most 2,000,000: this send's 500 would pass that. Send to a new source.
}

func ExampleReadBudget_Cut() {
	budget := events.ReadBudget{Batches: 1000, Pieces: 50_000, Bytes: 128 << 20}
	waiting := []events.Batch{{Seq: 1, Pieces: 30_000, Bytes: 9 << 20}, {Seq: 2, Pieces: 30_000, Bytes: 9 << 20}}
	fmt.Println(len(budget.Cut(events.Schema{}, waiting)))
	// Output:
	// 1
}

func ExampleISIZE() {
	var body bytes.Buffer
	gz := gzip.NewWriter(&body)
	_, _ = gz.Write([]byte(strings.Repeat(`{"id":"o-1"}`+"\n", 1000)))
	_ = gz.Close()
	size, err := events.ISIZE(body.Bytes()[body.Len()-4:])
	fmt.Println(size, err)
	// Output:
	// 13000 <nil>
}

func ExampleBatchKey() {
	fmt.Println(events.BatchKey("ws-1", "conn-1", "01JDBATCH00000000000000000"))
	fmt.Println(events.BatchPrefix("ws-1", "conn-1"))
	// Output:
	// ws/ws-1/events/conn-1/01JDBATCH00000000000000000.jsonl.gz
	// ws/ws-1/events/conn-1/
}
