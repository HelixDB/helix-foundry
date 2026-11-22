package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
)

// A Grid org's one live install is found by the org's id; an org with none,
// no org, or an org with several live installs (a click couldn't tell which)
// finds none. A revoked install, or one in another org, doesn't count.
func TestFoundrySlackInstallByEnterprise(t *testing.T) {
	ctx := context.Background()
	f := newSlackFixture(t, "grid")
	second, third := id26("SWS-grid-second"), id26("SWS-grid-third")
	createWorkspace(t, second)
	createWorkspace(t, third)
	byOrg := func(org string) (string, error) {
		i, err := f.db.GetFoundrySlackInstallByEnterprise(ctx, org)
		if err != nil {
			return "", err
		}
		return i.ID, nil
	}
	for _, org := range []string{"", "EGRID1"} {
		_, err := byOrg(org)
		assert.ErrorIs(t, err, store.ErrNotFound, "org %q", org)
	}

	first := f.install(1, f.workspaceID, "TGRID1")
	first.EnterpriseID = "EGRID1"
	_, _, err := f.db.InstallFoundrySlack(ctx, first, sealAs("x"))
	require.NoError(t, err)
	outside := f.install(2, third, "TGRID3")
	_, _, err = f.db.InstallFoundrySlack(ctx, outside, sealAs("x"))
	require.NoError(t, err)
	got, err := byOrg("EGRID1")
	require.NoError(t, err)
	assert.Equal(t, first.ID, got)
	_, err = byOrg("")
	assert.ErrorIs(t, err, store.ErrNotFound, "an install outside any org is no org's")

	other := f.install(3, second, "TGRID2")
	other.EnterpriseID = "EGRID1"
	_, _, err = f.db.InstallFoundrySlack(ctx, other, sealAs("x"))
	require.NoError(t, err)
	_, err = byOrg("EGRID1")
	assert.ErrorIs(t, err, store.ErrConflict, "two live installs in the org")
	_, err = byOrg("EGRID2")
	assert.ErrorIs(t, err, store.ErrNotFound)

	require.NoError(t, f.db.RevokeFoundrySlackInstall(ctx, second, other.ID, true))
	got, err = byOrg("EGRID1")
	require.NoError(t, err)
	assert.Equal(t, first.ID, got, "a revoked install doesn't count")
}
