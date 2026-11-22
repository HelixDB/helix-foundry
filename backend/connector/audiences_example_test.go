package connector_test

import (
	"fmt"

	"github.com/helixdb/helix-foundry/backend/connector"
)

// People are matched by one form of their email, whatever case and spaces
// a source or the host gives it in.
func ExampleParseEmail() {
	e, ok := connector.ParseEmail("  Ada@Example.COM ")
	fmt.Println(e, ok)
	_, ok = connector.ParseEmail("ada")
	fmt.Println(ok)
	// Output:
	// ada@example.com true
	// false
}

// A members audience lists its people once each, in order; everyone lists
// no one.
func ExampleMembersAudience() {
	ada, _ := connector.ParseEmail("ada@example.com")
	grace, _ := connector.ParseEmail("grace@example.com")
	private := connector.MembersAudience(grace, ada, grace)
	fmt.Println(private.Kind() == connector.AudienceMembers, private.Members())
	public := connector.EveryoneAudience()
	fmt.Println(public.Kind() == connector.AudienceEveryone, public.Members())
	// Output:
	// true [ada@example.com grace@example.com]
	// true []
}

// Slack reports who each channel is shared with, and tells a conversations
// document's channel by its row's key; Stripe reports nothing.
func ExampleAudiencedKind() {
	slack, ok := connector.AudiencedKind("slack")
	fmt.Println(ok)
	fmt.Println(slack.ItemOfDocument("conversations", "C123:2026-11-01"))
	_, ok = connector.AudiencedKind("stripe")
	fmt.Println(ok)
	// Output:
	// true
	// C123 true
	// false
}
