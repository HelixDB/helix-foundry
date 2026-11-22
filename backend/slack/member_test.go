package slack_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/helixdb/helix-foundry/backend/slack"
)

func TestUserMemberOf(t *testing.T) {
	ada := slack.User{ID: "U1", TeamID: "T1"}
	grid := slack.User{ID: "U2", TeamID: "T9", EnterpriseID: "E1", EnterpriseTeams: []string{"T9", "T1"}}
	for name, c := range map[string]struct {
		u                slack.User
		team, enterprise string
		want             bool
	}{
		"a member of the workspace":                   {ada, "T1", "", true},
		"a member of another workspace":               {ada, "T2", "", false},
		"no team to be a member of":                   {slack.User{ID: "U1"}, "", "", false},
		"a bot":                                       {slack.User{ID: "U1", TeamID: "T1", IsBot: true}, "T1", "", false},
		"a deactivated account":                       {slack.User{ID: "U1", TeamID: "T1", Deleted: true}, "T1", "", false},
		"a multi-channel guest":                       {slack.User{ID: "U1", TeamID: "T1", Restricted: true}, "T1", "", false},
		"a single-channel guest":                      {slack.User{ID: "U1", TeamID: "T1", UltraRestricted: true}, "T1", "", false},
		"a Slack Connect user":                        {slack.User{ID: "U1", TeamID: "T1", Stranger: true}, "T1", "", false},
		"a Grid member of the workspace, any session": {grid, "T1", "E1", true},
		"a Grid member not in the workspace":          {grid, "T3", "E1", false},
		"a member of another Grid org":                {grid, "T1", "E2", false},
		"outside Grid, a Grid install":                {ada, "T1", "E1", false},
	} {
		assert.Equal(t, c.want, c.u.MemberOf(c.team, c.enterprise), name)
	}
}

func ExampleUser_MemberOf() {
	guest := slack.User{ID: "U2", TeamID: "T1", Restricted: true}
	member := slack.User{ID: "U1", TeamID: "T1"}
	fmt.Println(member.MemberOf("T1", ""), guest.MemberOf("T1", ""))
	// Output: true false
}
