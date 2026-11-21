package slack

import "slices"

// MemberOf reports whether u is someone the bot answers for an install of
// teamID (in the Enterprise Grid org enterpriseID, "" outside one): a person
// (not a bot), active, a full member (no guest), not of another
// organisation (a Slack Connect user), and of the installed workspace: in a
// Grid org, a member of the org who belongs to that workspace among its
// others; outside one, of that workspace. Inside a Grid org a user's own
// team_id depends on their session, so it isn't read there.
func (u User) MemberOf(teamID, enterpriseID string) bool {
	switch {
	case teamID == "" || u.IsBot || u.Deleted || u.Restricted || u.UltraRestricted || u.Stranger:
		return false
	case enterpriseID != "":
		return u.EnterpriseID == enterpriseID && slices.Contains(u.EnterpriseTeams, teamID)
	}
	return u.TeamID == teamID
}
