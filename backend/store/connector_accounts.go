package store

import (
	"context"
	"errors"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// errAccountsNotBuilt stands in until the account columns exist; see
// plans/SIGN_IN_CONNECTORS.md.
var errAccountsNotBuilt = errors.New("store: connector accounts are not built yet")

func (s *mysqlDB) ListConnectorAccounts(context.Context, string) ([]model.ConnectorAccountRef, error) {
	return nil, errAccountsNotBuilt
}

func (s *mysqlDB) BreakConnectorAccounts(context.Context, string, string, string) error {
	return errAccountsNotBuilt
}
