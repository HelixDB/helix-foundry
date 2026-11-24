package connector

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/helixdb/helix-foundry/backend/access"
)

// Audienced is a kind that reports its source's own permissions: who each
// item it shares (a Slack channel) is shared with, and which item each of
// its documents belongs to. Foundry narrows what a grantee reads of the
// connector to the items the source shares with them (plans/SOURCE_ACLS.md).
type Audienced interface {
	// Audiences reads the source's people and every item's audience, in
	// full. It is given prepared settings. An error reads nothing: the
	// audiences stored before stay as they were.
	Audiences(ctx context.Context, src Source) (Audiences, error)
	// ItemOfDocument is the item a document of stream belongs to, by its
	// external id; ok is false for a document of no item the kind knows,
	// which only full readers read.
	ItemOfDocument(stream, externalID string) (item string, ok bool)
	// TableRows is how a table the kind syncs tells which item each of its
	// rows is of: by a field of the row key a version's mapping reads it by
	// (the table's primary key, as Discover gives it), the item itself or
	// the item and a separator before the rest, or as the source's
	// directory of people. The zero access.RowRule is a table whose rows
	// aren't told apart, which only full readers read.
	TableRows(table string) access.RowRule
}

var _ access.SourceKind = Audienced(nil)

// audiencedKinds are the catalog's kinds that are Audienced, by name. Their
// ItemOfDocument reads nothing of the network, so a catalog wired to none
// tells them.
var audiencedKinds = sync.OnceValue(func() map[string]Audienced {
	out := map[string]Audienced{}
	for _, k := range NewCatalog(Endpoints{}, Guard{}, S3Options{}, EventsOptions{}).Kinds() {
		if a, ok := k.(Audienced); ok {
			out[k.Name()] = a
		}
	}
	return out
})

// AudiencedKind is the kind named kind when it reports its source's
// permissions; ok is false for any other kind, or none.
func AudiencedKind(kind string) (_ Audienced, ok bool) {
	a, ok := audiencedKinds()[kind]
	return a, ok
}

// Audiences are a source's permissions at one read: its people, by email,
// and its items, by key, each with who it is shared with. An item the map
// doesn't hold is unknown to it.
type Audiences struct {
	People map[Email]Person
	Items  map[string]Audience
}

// Person is one of a source's accounts.
type Person struct {
	// Guest is an account the source limits to what it is let into (a Slack
	// guest): it reads no item shared with everyone, only those that list it.
	Guest bool
}

// Email is an email address in the one form people are matched by across
// a source and Foundry: trimmed and lowercased. The zero Email is none, and
// matches no one.
type Email struct{ address string }

// maxEmailBytes is the longest address kept: 64 bytes of local part, an @
// and 255 of domain.
const maxEmailBytes = 320

// ParseEmail is raw in the form people are matched by; ok is false for an
// address that is blank, has no @ between a local part and a domain, holds
// white space or is longer than maxEmailBytes.
func ParseEmail(raw string) (_ Email, ok bool) {
	address := strings.ToLower(strings.TrimSpace(raw))
	local, domain, found := strings.Cut(address, "@")
	if !found || local == "" || domain == "" || strings.ContainsAny(address, " \t\r\n") || len(address) > maxEmailBytes {
		return Email{}, false
	}
	return Email{address: address}, true
}

// String is the address, empty for the zero Email.
func (e Email) String() string { return e.address }

// AudienceKind is who an Audience shares an item with.
type AudienceKind uint8

const (
	// AudienceMembers shares an item with the people it lists only (a
	// private Slack channel's members).
	AudienceMembers AudienceKind = iota
	// AudienceEveryone shares an item with any reader the source doesn't
	// mark as a guest (a public Slack channel).
	AudienceEveryone
)

// Audience is who one item is shared with. The zero Audience lists no
// one: an item only full readers read.
type Audience struct {
	everyone bool
	// members are sorted and unique, and nil for everyone.
	members []Email
}

// EveryoneAudience shares an item with every reader the source doesn't mark
// as a guest.
func EveryoneAudience() Audience { return Audience{everyone: true} }

// MembersAudience shares an item with members alone, sorted, without
// repeats and without the zero Email.
func MembersAudience(members ...Email) Audience {
	kept := slices.DeleteFunc(slices.Clone(members), func(e Email) bool { return e == Email{} })
	slices.SortFunc(kept, func(a, b Email) int { return strings.Compare(a.address, b.address) })
	return Audience{members: slices.Compact(kept)}
}

// Kind is who the audience shares its item with.
func (a Audience) Kind() AudienceKind {
	if a.everyone {
		return AudienceEveryone
	}
	return AudienceMembers
}

// Members are the people a members audience lists, sorted; none for
// everyone.
func (a Audience) Members() []Email { return slices.Clone(a.members) }
