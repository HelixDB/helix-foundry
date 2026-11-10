package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// maxTeamName is foundry_teams.name's length, in characters.
const maxTeamName = 120

// SyncFoundryDirectoryTeams makes the workspace's teams of directoryID the
// directory's groups: a team per group, named as it is (or with a suffix when
// another team has the name) and of its members, and no team for a group the
// directory no longer has (its grants go with it; whom they restricted stays
// restricted). A team holding grants leaves out its group's unconfinable
// members and restricts the rest, as a team made by hand restricts whoever
// joins it; one holding none has the whole group. It runs under the
// workspace's lock, which grants and team changes take too. The host bounds
// the groups and their members; provider names the directory ("Okta").
func (s *mysqlDB) SyncFoundryDirectoryTeams(ctx context.Context, workspaceID, directoryID, provider string,
	groups []model.FoundryDirectoryTeam) (model.FoundryDirectorySync, error) {
	out := model.FoundryDirectorySync{TeamIDs: map[string]string{}, LeftOut: map[string][]string{}}
	if workspaceID == "" || directoryID == "" {
		return out, fmt.Errorf("sync directory teams: workspace and directory are required")
	}
	err := s.inTx(ctx, func(tx *mysqlDB) error {
		if err := tx.workspaces.Lock(ctx, tx.db, workspaceID); err != nil {
			return err
		}
		type team struct{ id, name, directory, group string }
		rows, err := tx.db.QueryContext(ctx, `SELECT id, name, directory_id, directory_group_id FROM foundry_teams WHERE workspace_id=? FOR UPDATE`, workspaceID)
		if err != nil {
			return err
		}
		var teams []team
		for rows.Next() {
			var t team
			var directory, group sql.NullString
			if err := rows.Scan(&t.id, &t.name, &directory, &group); err != nil {
				rows.Close()
				return err
			}
			t.directory, t.group = directory.String, group.String
			teams = append(teams, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		byGroup := map[string]team{}
		for _, t := range teams {
			if t.group != "" {
				byGroup[t.group] = t
			}
		}

		// The directory's groups that are gone take their teams with them.
		wanted := map[string]bool{}
		for _, g := range groups {
			wanted[g.GroupID] = true
		}
		taken := map[string]string{} // folded name -> team id
		for _, t := range teams {
			if t.directory == directoryID && !wanted[t.group] {
				if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_teams WHERE id=?`, t.id); err != nil {
					return err
				}
				if err := tx.dropFoundryTeamRows(ctx, workspaceID, t.id); err != nil {
					return err
				}
				out.Deleted++
				continue
			}
			taken[foldName(t.name)] = t.id
		}

		// Names first, each team clear of the others' (a rename may take a
		// name another group is giving up), then members.
		type plan struct {
			group model.FoundryDirectoryTeam
			id    string
			name  string
			isNew bool
		}
		plans := make([]plan, len(groups))
		current := map[string]string{} // team id -> its name now
		for i, g := range groups {
			plans[i] = plan{group: g}
			if t, ok := byGroup[g.GroupID]; ok {
				plans[i].id = t.id
				current[t.id] = t.name
				delete(taken, foldName(t.name))
			} else {
				plans[i].id, plans[i].isNew = id.New(), true
			}
		}
		// A team keeping its name keeps it before the others choose.
		for _, keeping := range []bool{true, false} {
			for i := range plans {
				p := &plans[i]
				want := teamName(p.group.Name)
				if (current[p.id] == want) != keeping {
					continue
				}
				p.name = freeTeamName(want, provider, taken)
				taken[foldName(p.name)] = p.id
			}
		}
		for _, p := range plans {
			if p.isNew {
				continue
			}
			if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_teams SET name=? WHERE id=?`, "~"+p.id, p.id); err != nil {
				return err
			}
		}
		for _, p := range plans {
			// The name key's collation also ignores accents, which taken
			// doesn't: a name it refuses is taken, and the next one is tried.
			for attempt := 0; ; attempt++ {
				var err error
				if p.isNew {
					_, err = tx.db.ExecContext(ctx, `INSERT INTO foundry_teams (id, workspace_id, name, directory_id, directory_group_id, directory_provider)
VALUES (?, ?, ?, ?, ?, ?)`, p.id, workspaceID, p.name, directoryID, p.group.GroupID, provider)
				} else {
					_, err = tx.db.ExecContext(ctx, `UPDATE foundry_teams SET name=?, directory_id=?, directory_provider=? WHERE id=?`,
						p.name, directoryID, provider, p.id)
				}
				if err == nil {
					break
				}
				if !isDuplicateKey(err) || attempt >= 20 {
					return err
				}
				taken[foldName(p.name)] = "?"
				p.name = freeTeamName(teamName(p.group.Name), provider, taken)
				taken[foldName(p.name)] = p.id
			}
			var holds bool
			if err := tx.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_access_grants WHERE workspace_id=? AND principal_kind=? AND principal_id=?)`,
				workspaceID, access.PrincipalTeam.String(), p.id).Scan(&holds); err != nil {
				return err
			}
			var in, left []string
			for _, m := range p.group.Members {
				switch {
				case slices.Contains(in, m.UserID) || slices.Contains(left, m.UserID):
				case holds && m.Unconfinable:
					left = append(left, m.UserID)
				default:
					in = append(in, m.UserID)
				}
			}
			if err := tx.setFoundryTeamMembers(ctx, workspaceID, p.id, in); err != nil {
				return err
			}
			if err := tx.setFoundryTeamLeftOut(ctx, workspaceID, p.id, left); err != nil {
				return err
			}
			if holds {
				if err := tx.restrictFoundryTeamMembers(ctx, workspaceID, p.id, nil); err != nil {
					return err
				}
			}
			out.TeamIDs[p.group.GroupID] = p.id
			if len(left) > 0 {
				out.LeftOut[p.group.GroupID] = left
			}
		}
		return nil
	})
	return out, err
}

// ReleaseFoundryDirectoryTeams makes the teams of directoryID, which the
// workspace no longer has, teams like those made by hand: each keeps its
// name, members and grants, and owners and admins change them from now on.
func (s *mysqlDB) ReleaseFoundryDirectoryTeams(ctx context.Context, workspaceID, directoryID string) error {
	return s.inTx(ctx, func(tx *mysqlDB) error {
		if err := tx.workspaces.Lock(ctx, tx.db, workspaceID); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `DELETE l FROM foundry_team_left_out l JOIN foundry_teams t ON t.id = l.team_id
WHERE t.workspace_id=? AND t.directory_id=?`, workspaceID, directoryID); err != nil {
			return err
		}
		_, err := tx.db.ExecContext(ctx, `UPDATE foundry_teams SET directory_id=NULL, directory_group_id=NULL, directory_provider=NULL
WHERE workspace_id=? AND directory_id=?`, workspaceID, directoryID)
		return err
	})
}

// LeaveOutFoundryTeamMembers takes userIDs off a synced team, keeping them as
// left out, before it is given grants that can't confine them. The next sync
// puts them back once it holds none.
func (s *mysqlDB) LeaveOutFoundryTeamMembers(ctx context.Context, workspaceID, teamID string, userIDs []string) error {
	if len(userIDs) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *mysqlDB) error {
		args := append([]any{teamID, workspaceID}, toAnySlice(userIDs)...)
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_team_members WHERE team_id=? AND workspace_id=? AND user_id IN (`+
			idPlaceholders(len(userIDs))+`)`, args...); err != nil {
			return err
		}
		rows := make([]any, 0, 3*len(userIDs))
		for _, u := range userIDs {
			rows = append(rows, teamID, workspaceID, u)
		}
		_, err := tx.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_team_left_out (team_id, workspace_id, user_id) VALUES `+
			strings.TrimSuffix(strings.Repeat("(?, ?, ?), ", len(userIDs)), ", "), rows...)
		return err
	})
}

// setFoundryTeamLeftOut replaces whom a synced team leaves out.
func (s *mysqlDB) setFoundryTeamLeftOut(ctx context.Context, workspaceID, teamID string, userIDs []string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_team_left_out WHERE team_id=?`, teamID); err != nil {
		return err
	}
	if len(userIDs) == 0 {
		return nil
	}
	args := make([]any, 0, 3*len(userIDs))
	for _, u := range userIDs {
		args = append(args, teamID, workspaceID, u)
	}
	_, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_team_left_out (team_id, workspace_id, user_id) VALUES `+
		strings.TrimSuffix(strings.Repeat("(?, ?, ?), ", len(userIDs)), ", "), args...)
	return err
}

// teamName is a group's name as a team's: trimmed, at most 120 characters,
// "Directory group" for none.
func teamName(name string) string {
	name = strings.TrimSpace(strings.ToValidUTF8(name, ""))
	if name == "" {
		return "Directory group"
	}
	if utf8.RuneCountInString(name) > maxTeamName {
		name = strings.TrimSpace(string([]rune(name)[:maxTeamName]))
	}
	return name
}

// freeTeamName is name, or, when another team has it, "name (provider)",
// then "name (2)", "name (3)"…, each within 120 characters.
func freeTeamName(name, provider string, taken map[string]string) string {
	try := func(suffix string) string {
		base := name
		if n := maxTeamName - utf8.RuneCountInString(suffix); utf8.RuneCountInString(base) > n {
			base = strings.TrimSpace(string([]rune(base)[:n]))
		}
		return base + suffix
	}
	if _, ok := taken[foldName(name)]; !ok {
		return name
	}
	if provider != "" {
		if c := try(" (" + provider + ")"); taken[foldName(c)] == "" {
			return c
		}
	}
	for i := 2; ; i++ {
		if c := try(fmt.Sprintf(" (%d)", i)); taken[foldName(c)] == "" {
			return c
		}
	}
}

// foldName is how foundry_teams' name key compares names: its collation
// ignores case.
func foldName(name string) string { return strings.ToLower(name) }
