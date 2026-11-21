package slack

import (
	"encoding/json"
	"fmt"
)

// Payload is what intake keeps of an event or a click until it is handled
// (the store's SlackIntake.Payload): its Kind and JSON, as EncodeEvent and
// EncodeInteraction write them. The worker reads it back with DecodeEvent or
// DecodeInteraction, and keeps the turn input it renders beside them
// ("input", the store's SlackIntake.Input).
type Payload struct {
	Kind Kind            `json:"kind"`
	Data json.RawMessage `json:"data"`
}

// EventPayload is e's Payload, as JSON.
func EventPayload(e Event) (json.RawMessage, error) {
	k, data, err := EncodeEvent(e)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Payload{Kind: k, Data: data})
}

// InteractionPayload is in's Payload, as JSON.
func InteractionPayload(in Interaction) (json.RawMessage, error) {
	k, data, err := EncodeInteraction(in)
	if err != nil {
		return nil, err
	}
	return json.Marshal(Payload{Kind: k, Data: data})
}

// ReadPayload reads a stored Payload, refusing one of no kind ParseKind
// reads, or with no data.
func ReadPayload(raw []byte) (Payload, error) {
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return Payload{}, fmt.Errorf("slack: decode payload: %w", err)
	}
	if _, err := ParseKind(string(p.Kind)); err != nil {
		return Payload{}, err
	}
	if len(p.Data) == 0 || string(p.Data) == "null" {
		return Payload{}, fmt.Errorf("slack: a %s payload without its data", p.Kind)
	}
	return p, nil
}
