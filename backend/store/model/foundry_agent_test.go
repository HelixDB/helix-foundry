package model

import (
	"fmt"
	"testing"

	"github.com/helixdb/helix-foundry/backend/access"
)

func ExampleParseFoundryAgentPrincipal() {
	ada, _ := ParseFoundryAgentPrincipal("user", "user_01ADA")
	bot, _ := ParseFoundryAgentPrincipal("service_credential", "01CRED")
	_, err := ParseFoundryAgentPrincipal("team", "group_finance")
	fmt.Printf("%#v %#v %v\n", ada, bot, err)
	fmt.Println(bot.Access().Kind, bot.Access().ID)
	// Output:
	// model.AgentPerson{UserID:"user_01ADA"} model.AgentBot{CredentialID:"01CRED"} unknown foundry agent principal kind "team"
	// service_credential 01CRED
}

// A principal reads back as it is kept, and nothing else reads at all.
func TestParseFoundryAgentPrincipal(t *testing.T) {
	for _, p := range []FoundryAgentPrincipal{AgentPerson{UserID: "user_01ADA"}, AgentBot{CredentialID: "01CRED"}} {
		p.agentPrincipal()
		a := p.Access()
		back, err := ParseFoundryAgentPrincipal(a.Kind.String(), a.ID)
		if err != nil || back != p {
			t.Fatalf("%#v read back as %#v, %v", p, back, err)
		}
	}
	if (AgentPerson{UserID: "u"}).Access() != (access.Principal{Kind: access.PrincipalUser, ID: "u"}) {
		t.Fatal("a person is a user grant principal")
	}
	for _, bad := range [][2]string{{"user", ""}, {"service_credential", ""}, {"team", "t1"}, {"", "u"}, {"User", "u"}} {
		if p, err := ParseFoundryAgentPrincipal(bad[0], bad[1]); err == nil {
			t.Fatalf("%q read as %#v", bad, p)
		}
	}
}

// A person's action has its whole confirmation, and a bot's none.
func TestFoundryAgentActionValidateApproval(t *testing.T) {
	whole := PersonApproval{ConfirmationID: "c1", TokenSealed: []byte("sealed"), KeyID: "k1"}
	whole.agentApproval()
	AdminApproval{}.agentApproval()
	ada, bot := AgentPerson{UserID: "user_01ADA"}, AgentBot{CredentialID: "01CRED"}
	for _, c := range []struct {
		name      string
		principal FoundryAgentPrincipal
		approval  FoundryAgentApproval
		ok        bool
	}{
		{"a person's confirmed action", ada, whole, true},
		{"a bot's proposal", bot, AdminApproval{}, true},
		{"a person's action without a confirmation", ada, AdminApproval{}, false},
		{"a person's action with no approval", ada, nil, false},
		{"no confirmation id", ada, PersonApproval{TokenSealed: []byte("s"), KeyID: "k"}, false},
		{"no token", ada, PersonApproval{ConfirmationID: "c", KeyID: "k"}, false},
		{"no key", ada, PersonApproval{ConfirmationID: "c", TokenSealed: []byte("s")}, false},
		{"a bot's action with a confirmation", bot, whole, false},
		{"a bot's action with no approval", bot, nil, false},
		{"no principal", nil, AdminApproval{}, false},
	} {
		a := &FoundryAgentAction{ID: "a1", Principal: c.principal, Approval: c.approval}
		if err := a.ValidateApproval(); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
