package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// SetFoundryMemberRestricted restricts the member to what grants give, or
// lifts it; either is a no-op when it already holds. ErrConflict means they
// hold grants, their own or a team's, which keep them restricted: the
// workspace's lock, which grants and team changes take too, keeps a grant from
// arriving between that check and the lift.
func (s *mysqlDB) SetFoundryMemberRestricted(ctx context.Context, workspaceID, userID string, restricted bool, byUserID *string) error {
	if !restricted {
		return s.WithTx(ctx, func(txDB Store) error {
			tx := txDB.(*mysqlDB)
			if err := tx.workspaces.Lock(ctx, tx.db, workspaceID); err != nil {
				return err
			}
			var holds bool
			if err := tx.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_access_grants WHERE workspace_id=? AND (
 (principal_kind=? AND principal_id=?) OR
 (principal_kind=? AND principal_id IN (SELECT team_id FROM foundry_team_members WHERE workspace_id=? AND user_id=?))))`,
				workspaceID, access.PrincipalUser.String(), userID, access.PrincipalTeam.String(), workspaceID, userID).Scan(&holds); err != nil {
				return err
			}
			if holds {
				return ErrConflict
			}
			_, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_restricted_members WHERE workspace_id=? AND user_id=?`, workspaceID, userID)
			return err
		})
	}
	_, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_restricted_members (workspace_id, user_id, created_by_user_id) VALUES (?, ?, ?)`,
		workspaceID, userID, byUserID)
	return err
}

// IsFoundryMemberRestricted reports whether the member is restricted.
func (s *mysqlDB) IsFoundryMemberRestricted(ctx context.Context, workspaceID, userID string) (bool, error) {
	var restricted bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_restricted_members WHERE workspace_id=? AND user_id=?)`,
		workspaceID, userID).Scan(&restricted)
	return restricted, err
}

// ListFoundryRestrictedMembers returns the workspace's restricted members.
func (s *mysqlDB) ListFoundryRestrictedMembers(ctx context.Context, workspaceID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id FROM foundry_restricted_members WHERE workspace_id=? ORDER BY created_at, user_id`, workspaceID)
	if err != nil {
		return nil, err
	}
	return scanStrings(rows)
}

// CreateFoundryTeam makes team with its members. ErrConflict means the
// workspace has a team of its name.
func (s *mysqlDB) CreateFoundryTeam(ctx context.Context, team *model.FoundryTeam) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if _, err := tx.db.ExecContext(ctx, `INSERT INTO foundry_teams (id, workspace_id, name, created_by_user_id) VALUES (?, ?, ?, ?)`,
			team.ID, team.WorkspaceID, team.Name, team.CreatedByUserID); err != nil {
			if isDuplicateKey(err) {
				return ErrConflict
			}
			return err
		}
		return tx.setFoundryTeamMembers(ctx, team.WorkspaceID, team.ID, team.MemberUserIDs)
	})
}

// UpdateFoundryTeam renames the team and replaces its members. ErrNotFound
// means the workspace has no such team, ErrConflict that another has the
// name.
func (s *mysqlDB) UpdateFoundryTeam(ctx context.Context, workspaceID, teamID, name string, memberUserIDs []string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.workspaces.Lock(ctx, tx.db, workspaceID); err != nil {
			return err
		}
		var id string
		err := tx.db.QueryRowContext(ctx, `SELECT id FROM foundry_teams WHERE id=? AND workspace_id=? FOR UPDATE`, teamID, workspaceID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `UPDATE foundry_teams SET name=? WHERE id=?`, name, teamID); err != nil {
			if isDuplicateKey(err) {
				return ErrConflict
			}
			return err
		}
		if err := tx.setFoundryTeamMembers(ctx, workspaceID, teamID, memberUserIDs); err != nil {
			return err
		}
		var holds bool
		if err := tx.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM foundry_access_grants WHERE workspace_id=? AND principal_kind=? AND principal_id=?)`,
			workspaceID, access.PrincipalTeam.String(), teamID).Scan(&holds); err != nil {
			return err
		}
		if !holds {
			return nil
		}
		return tx.restrictFoundryTeamMembers(ctx, workspaceID, teamID, nil)
	})
}

// restrictFoundryTeamMembers restricts a team's members, its grants being all
// they read. A restriction applies only while its holder's role is member, so
// an owner or admin on the team is confined only if they stop being one.
func (s *mysqlDB) restrictFoundryTeamMembers(ctx context.Context, workspaceID, teamID string, byUserID *string) error {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id FROM foundry_team_members WHERE team_id=? AND workspace_id=?`, teamID, workspaceID)
	if err != nil {
		return err
	}
	members, err := scanStrings(rows)
	if err != nil || len(members) == 0 {
		return err
	}
	args := make([]any, 0, 3*len(members))
	for _, userID := range members {
		args = append(args, workspaceID, userID, byUserID)
	}
	_, err = s.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_restricted_members (workspace_id, user_id, created_by_user_id) VALUES `+
		strings.TrimSuffix(strings.Repeat("(?, ?, ?), ", len(members)), ", "), args...)
	return err
}

// setFoundryTeamMembers replaces a team's members.
func (s *mysqlDB) setFoundryTeamMembers(ctx context.Context, workspaceID, teamID string, userIDs []string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_team_members WHERE team_id=?`, teamID); err != nil {
		return err
	}
	if len(userIDs) == 0 {
		return nil
	}
	args := make([]any, 0, 3*len(userIDs))
	for _, u := range userIDs {
		args = append(args, teamID, workspaceID, u)
	}
	_, err := s.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_team_members (team_id, workspace_id, user_id) VALUES `+
		strings.TrimSuffix(strings.Repeat("(?, ?, ?), ", len(userIDs)), ", "), args...)
	return err
}

// DeleteFoundryTeam deletes the team, its members and its grants;
// ErrNotFound means the workspace has no such team.
func (s *mysqlDB) DeleteFoundryTeam(ctx context.Context, workspaceID, teamID string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := execOrNotFound(ctx, tx.db, `DELETE FROM foundry_teams WHERE id=? AND workspace_id=?`, teamID, workspaceID); err != nil {
			return err
		}
		if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_team_members WHERE team_id=?`, teamID); err != nil {
			return err
		}
		return tx.DeleteFoundryAccessPrincipal(ctx, workspaceID, access.Principal{Kind: access.PrincipalTeam, ID: teamID})
	})
}

// ListFoundryTeams returns the workspace's teams by name, each with its
// members.
func (s *mysqlDB) ListFoundryTeams(ctx context.Context, workspaceID string) ([]model.FoundryTeam, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.name, t.created_by_user_id, t.created_at, m.user_id FROM foundry_teams t
LEFT JOIN foundry_team_members m ON m.team_id = t.id
WHERE t.workspace_id=? ORDER BY t.name, t.id, m.user_id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FoundryTeam
	for rows.Next() {
		var t model.FoundryTeam
		var by, member sql.NullString
		if err := rows.Scan(&t.ID, &t.Name, &by, &t.CreatedAt, &member); err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].ID != t.ID {
			t.WorkspaceID = workspaceID
			if by.Valid {
				t.CreatedByUserID = &by.String
			}
			out = append(out, t)
		}
		if member.Valid {
			last := &out[len(out)-1]
			last.MemberUserIDs = append(last.MemberUserIDs, member.String)
		}
	}
	return out, rows.Err()
}

// FoundryTeamsOf returns the teams the member belongs to.
func (s *mysqlDB) FoundryTeamsOf(ctx context.Context, workspaceID, userID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT team_id FROM foundry_team_members WHERE workspace_id=? AND user_id=? ORDER BY team_id`, workspaceID, userID)
	if err != nil {
		return nil, err
	}
	return scanStrings(rows)
}

// RemoveFoundryAccessMember takes what grants gave a member who left the
// workspace: their own grants and their teams. Their restriction stays, so
// one let back in is still restricted. The host calls it when it removes a
// member, in its own transaction or not.
func (s *mysqlDB) RemoveFoundryAccessMember(ctx context.Context, workspaceID, userID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM foundry_team_members WHERE workspace_id=? AND user_id=?`, workspaceID, userID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM foundry_access_grants WHERE workspace_id=? AND principal_kind='user' AND principal_id=?`,
		workspaceID, userID)
	return err
}
