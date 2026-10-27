package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// GetFoundryDataLayout returns how the workspace's Data tree is arranged; a
// workspace that never wrote a layout has revision 0 and {}.
func (s *mysqlDB) GetFoundryDataLayout(ctx context.Context, workspaceID string) (*model.FoundryDataLayout, error) {
	l := model.FoundryDataLayout{WorkspaceID: workspaceID}
	var layout string
	var updatedBy sql.NullString
	var updatedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT layout, revision, updated_by_user_id, updated_at FROM foundry_data_layouts WHERE workspace_id=?`,
		workspaceID).Scan(&layout, &l.Revision, &updatedBy, &updatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		l.Layout = json.RawMessage(`{}`)
		return &l, nil
	case err != nil:
		return nil, err
	}
	l.Layout = json.RawMessage(layout)
	if updatedBy.Valid {
		l.UpdatedByUserID = &updatedBy.String
	}
	if updatedAt.Valid {
		l.UpdatedAt = &updatedAt.Time
	}
	return &l, nil
}

// PutFoundryDataLayout stores l.Layout over the layout the caller read at
// expectedRevision, as the next revision, and sets l.Revision to it. The
// layout is the caller's to check. ErrConflict means another write came
// first: the workspace's revision is not expectedRevision, which includes a
// first write (0) racing another first write.
func (s *mysqlDB) PutFoundryDataLayout(ctx context.Context, l *model.FoundryDataLayout, expectedRevision int64) error {
	if expectedRevision == 0 {
		_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_data_layouts (workspace_id, layout, revision, updated_by_user_id)
VALUES (?, CAST(? AS JSON), 1, ?)`, l.WorkspaceID, string(l.Layout), l.UpdatedByUserID)
		if isDuplicateKey(err) {
			return ErrConflict
		}
		if err == nil {
			l.Revision = 1
		}
		return err
	}
	err := execOrNotFound(ctx, s.db, `UPDATE foundry_data_layouts SET layout=CAST(? AS JSON), revision=revision+1, updated_by_user_id=?,
  updated_at=NOW(6) WHERE workspace_id=? AND revision=?`, string(l.Layout), l.UpdatedByUserID, l.WorkspaceID, expectedRevision)
	if errors.Is(err, ErrNotFound) {
		return ErrConflict
	}
	if err == nil {
		l.Revision = expectedRevision + 1
	}
	return err
}
