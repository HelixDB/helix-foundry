package store

import (
	"context"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// GetFoundrySlackInstallByEnterprise returns the one live install in the
// Enterprise Grid org enterpriseID. Slack names a click by the workspace the
// clicker's session is in, which in a channel shared by several workspaces
// of the org may not be the install's: with one live install in the org, the
// click is that install's. ErrNotFound when the org has none (or
// enterpriseID is ""), ErrConflict when it has several, which a click can't
// be told apart between.
func (s *mysqlDB) GetFoundrySlackInstallByEnterprise(ctx context.Context, enterpriseID string) (*model.SlackInstall, error) {
	if enterpriseID == "" {
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+foundrySlackInstallColumns+`
FROM foundry_slack_installs WHERE enterprise_id=? AND revoked_at IS NULL ORDER BY id LIMIT 2`, enterpriseID)
	if err != nil {
		return nil, err
	}
	installs, err := scanAll(rows, scanFoundrySlackInstall)
	switch {
	case err != nil:
		return nil, err
	case len(installs) == 0:
		return nil, ErrNotFound
	case len(installs) > 1:
		return nil, ErrConflict
	}
	return installs[0], nil
}
