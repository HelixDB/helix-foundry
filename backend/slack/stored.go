package slack

import (
	"encoding/json"
	"fmt"
)

// Kind names a stored event or click: the intake row's kind, beside its
// payload (EncodeEvent, EncodeInteraction).
type Kind string

// The kinds of what intake stores.
const (
	KindMention       Kind = "mention"
	KindDM            Kind = "dm"
	KindReply         Kind = "reply"
	KindStopped       Kind = "stopped"
	KindUninstalled   Kind = "uninstalled"
	KindTokensRevoked Kind = "tokens_revoked"
	KindApprove       Kind = "approve"
	KindDismiss       Kind = "dismiss"
	KindRefresh       Kind = "refresh"
	KindReview        Kind = "review"
	KindRun           Kind = "run"
	KindStop          Kind = "stop"
	KindUnlink        Kind = "unlink"
)

// kinds are the kinds ParseKind reads.
var kinds = map[Kind]bool{
	KindMention: true, KindDM: true, KindReply: true, KindStopped: true, KindUninstalled: true, KindTokensRevoked: true,
	KindApprove: true, KindDismiss: true, KindRefresh: true, KindReview: true, KindRun: true, KindStop: true, KindUnlink: true,
}

// ParseKind reads a stored kind, refusing any other.
func ParseKind(s string) (Kind, error) {
	if !kinds[Kind(s)] {
		return "", fmt.Errorf("slack: unknown kind %q", s)
	}
	return Kind(s), nil
}

// EncodeEvent is an event's kind and JSON, to store in intake.
// URLVerification, Ignored and FeedEvents aren't stored there.
func EncodeEvent(e Event) (Kind, json.RawMessage, error) {
	var k Kind
	switch e.(type) {
	case Mention:
		k = KindMention
	case DirectMessage:
		k = KindDM
	case ThreadReply:
		k = KindReply
	case SessionStopped:
		k = KindStopped
	case Uninstalled:
		k = KindUninstalled
	case TokensRevoked:
		k = KindTokensRevoked
	default:
		return "", nil, fmt.Errorf("slack: %T isn't stored", e)
	}
	b, err := json.Marshal(e)
	return k, b, err
}

// EncodeInteraction is a click's kind and JSON, to store. Unhandled isn't
// stored.
func EncodeInteraction(in Interaction) (Kind, json.RawMessage, error) {
	var k Kind
	switch in.(type) {
	case Approve:
		k = KindApprove
	case Dismiss:
		k = KindDismiss
	case Refresh:
		k = KindRefresh
	case Review:
		k = KindReview
	case Run:
		k = KindRun
	case Stop:
		k = KindStop
	case Unlink:
		k = KindUnlink
	default:
		return "", nil, fmt.Errorf("slack: %T isn't stored", in)
	}
	b, err := json.Marshal(in)
	return k, b, err
}

// DecodeEvent reads what EncodeEvent stored as k.
func DecodeEvent(k Kind, data []byte) (Event, error) {
	switch k {
	case KindMention:
		return decodeEvent[Mention](k, data)
	case KindDM:
		return decodeEvent[DirectMessage](k, data)
	case KindReply:
		return decodeEvent[ThreadReply](k, data)
	case KindStopped:
		return decodeEvent[SessionStopped](k, data)
	case KindUninstalled:
		return decodeEvent[Uninstalled](k, data)
	case KindTokensRevoked:
		return decodeEvent[TokensRevoked](k, data)
	}
	return nil, fmt.Errorf("slack: %q is no event's kind", k)
}

// DecodeInteraction reads what EncodeInteraction stored as k.
func DecodeInteraction(k Kind, data []byte) (Interaction, error) {
	switch k {
	case KindApprove:
		return decodeInteraction[Approve](k, data)
	case KindDismiss:
		return decodeInteraction[Dismiss](k, data)
	case KindRefresh:
		return decodeInteraction[Refresh](k, data)
	case KindReview:
		return decodeInteraction[Review](k, data)
	case KindRun:
		return decodeInteraction[Run](k, data)
	case KindStop:
		return decodeInteraction[Stop](k, data)
	case KindUnlink:
		return decodeInteraction[Unlink](k, data)
	}
	return nil, fmt.Errorf("slack: %q is no click's kind", k)
}

func decodeEvent[T Event](k Kind, data []byte) (Event, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("slack: decode %s: %w", k, err)
	}
	return v, nil
}

func decodeInteraction[T Interaction](k Kind, data []byte) (Interaction, error) {
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("slack: decode %s: %w", k, err)
	}
	return v, nil
}
