package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	dashboardSpec = `{"v":1,"tiles":[],"layout":{"rows":[]}}`
	dashAda       = "user-ada"
	dashAlan      = "user-alan"
)

var (
	adaEdits   = model.FoundryDashboardEditor{UserID: dashAda}
	alanEdits  = model.FoundryDashboardEditor{UserID: dashAlan}
	adminEdits = model.FoundryDashboardEditor{UserID: "user-admin", Manager: true}
)

func newDashboard(workspaceID string, n int, by string) *model.FoundryDashboard {
	token := fmt.Sprintf("create-%d", n)
	return &model.FoundryDashboard{ID: id26(fmt.Sprintf("DB-%03d-", n)), WorkspaceID: workspaceID, Name: fmt.Sprintf("Dashboard %d", n),
		Spec: json.RawMessage(dashboardSpec), CreatedByUserID: by, ClientToken: &token}
}

func dashboardShare(d *model.FoundryDashboard, revision int64, token string, ontologies ...string) *model.FoundryDashboardShare {
	return &model.FoundryDashboardShare{
		DashboardID: d.ID, WorkspaceID: d.WorkspaceID, TokenSHA256: strings.Repeat(token, 64)[:64], TokenSealed: []byte("sealed-" + token),
		TokenKeyID: "k1", Snapshot: json.RawMessage(`{"v":1,"tiles":[]}`), SpecRevision: revision, OntologyIDs: ontologies, PublishedByUserID: "user-admin",
	}
}

// keepExpiry publishes without changing a link's expiry: a new link never ends.
var keepExpiry = model.FoundryDashboardLinkExpiry{Keep: true}

func dashboardRefusal(t *testing.T, err error) model.FoundryDashboardRefusalReason {
	t.Helper()
	var r model.FoundryDashboardRefusal
	require.ErrorAs(t, err, &r)
	require.NotEmpty(t, r.Detail)
	return r.Reason
}

func TestFoundryDashboards(t *testing.T) {
	ctx := context.Background()
	workspaceID := id26("DBW")

	t.Run("creates, lists and finds a create by its client token", func(t *testing.T) {
		db := newTestStore(t)
		createWorkspace(t, workspaceID)
		d := newDashboard(workspaceID, 1, dashAda)
		require.NoError(t, db.CreateFoundryDashboard(ctx, d, 10))
		assert.ErrorIs(t, db.CreateFoundryDashboard(ctx, newDashboard(workspaceID, 1, dashAda), 10), store.ErrConflict, "the same click twice")
		other := newDashboard(workspaceID, 1, dashAlan)
		other.ID = id26("DB-ALAN")
		require.NoError(t, db.CreateFoundryDashboard(ctx, other, 10), "client tokens are each creator's own")

		got, err := db.GetFoundryDashboard(ctx, workspaceID, d.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(1), got.Revision)
		assert.Equal(t, dashAda, got.UpdatedByUserID)
		assert.JSONEq(t, dashboardSpec, string(got.Spec))
		assert.Nil(t, got.PinnedAt)
		assert.Nil(t, got.Share)
		byToken, err := db.GetFoundryDashboardByClientToken(ctx, workspaceID, dashAda, "create-1")
		require.NoError(t, err)
		assert.Equal(t, d.ID, byToken.ID)
		_, err = db.GetFoundryDashboardByClientToken(ctx, workspaceID, dashAlan, "create-2")
		assert.ErrorIs(t, err, store.ErrNotFound)

		_, err = db.GetFoundryDashboard(ctx, id26("DBW-other"), d.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "dashboards are their workspace's")

		list, err := db.ListFoundryDashboards(ctx, workspaceID, false)
		require.NoError(t, err)
		require.Len(t, list, 2)
		assert.Nil(t, list[0].Spec, "a list leaves specs out")

		assert.ErrorIs(t, db.CreateFoundryDashboard(ctx, newDashboard(workspaceID, 1, dashAda), 2), store.ErrConflict,
			"a retry of the create that filled the cap finds its dashboard, not the cap")
	})

	t.Run("caps a workspace's dashboards", func(t *testing.T) {
		db := newTestStore(t)
		createWorkspace(t, workspaceID)
		require.NoError(t, db.CreateFoundryDashboard(ctx, newDashboard(workspaceID, 1, dashAda), 2))
		require.NoError(t, db.CreateFoundryDashboard(ctx, newDashboard(workspaceID, 2, dashAda), 2))
		assert.Equal(t, model.FoundryDashboardLimit, dashboardRefusal(t, db.CreateFoundryDashboard(ctx, newDashboard(workspaceID, 3, dashAda), 2)))
		assert.ErrorIs(t, db.CreateFoundryDashboard(ctx, newDashboard(id26("DBW-none"), 4, dashAda), 2), store.ErrNotFound, "no such workspace")
		deleteWorkspace(t, workspaceID)
		assert.ErrorIs(t, db.CreateFoundryDashboard(ctx, newDashboard(workspaceID, 5, dashAda), 20), store.ErrNotFound, "a deleted workspace takes none")
	})

	t.Run("holds concurrent creates to the cap", func(t *testing.T) {
		db := newTestStore(t)
		createWorkspace(t, workspaceID)
		var wg sync.WaitGroup
		errs := make([]error, 6)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = db.CreateFoundryDashboard(ctx, newDashboard(workspaceID, i+1, dashAda), 3)
			}()
		}
		wg.Wait()
		created := 0
		for _, err := range errs {
			if err == nil {
				created++
			}
		}
		assert.Equal(t, 3, created)
	})

	t.Run("edits at a revision, as who may", func(t *testing.T) {
		db := newTestStore(t)
		createWorkspace(t, workspaceID)
		d := newDashboard(workspaceID, 1, dashAda)
		require.NoError(t, db.CreateFoundryDashboard(ctx, d, 10))
		name := "Revenue"
		require.NoError(t, db.UpdateFoundryDashboard(ctx, workspaceID, d.ID, adaEdits, 1, model.FoundryDashboardUpdate{Name: &name}))
		got, err := db.GetFoundryDashboard(ctx, workspaceID, d.ID)
		require.NoError(t, err)
		assert.Equal(t, "Revenue", got.Name)
		assert.Equal(t, int64(2), got.Revision)

		assert.Equal(t, model.FoundryDashboardChanged, dashboardRefusal(t, db.UpdateFoundryDashboard(ctx, workspaceID, d.ID, adaEdits, 1,
			model.FoundryDashboardUpdate{Name: &name})), "an edit of an older revision")
		assert.Equal(t, model.FoundryDashboardNotEditable, dashboardRefusal(t, db.UpdateFoundryDashboard(ctx, workspaceID, d.ID, alanEdits, 2,
			model.FoundryDashboardUpdate{Name: &name})), "another member's dashboard")
		description, spec := "Weekly", json.RawMessage(`{"v":1,"tiles":[],"layout":{"rows":[]}}`)
		require.NoError(t, db.UpdateFoundryDashboard(ctx, workspaceID, d.ID, adminEdits, 2,
			model.FoundryDashboardUpdate{Description: &description, Spec: spec}), "an owner or admin edits anyone's")
		got, err = db.GetFoundryDashboard(ctx, workspaceID, d.ID)
		require.NoError(t, err)
		assert.Equal(t, "Revenue", got.Name, "what an edit leaves out stays")
		assert.Equal(t, "Weekly", got.Description)
		assert.Equal(t, "user-admin", got.UpdatedByUserID)
		assert.Equal(t, dashAda, got.CreatedByUserID)

		require.NoError(t, db.SetFoundryDashboardPinned(ctx, workspaceID, d.ID, "user-admin", 3, true, 5))
		assert.Equal(t, model.FoundryDashboardNotEditable, dashboardRefusal(t, db.UpdateFoundryDashboard(ctx, workspaceID, d.ID, adaEdits, 3,
			model.FoundryDashboardUpdate{Name: &name})), "its creator, once it is pinned")
		assert.Equal(t, model.FoundryDashboardNotEditable, dashboardRefusal(t, db.DeleteFoundryDashboard(ctx, workspaceID, d.ID, adaEdits)))

		assert.ErrorIs(t, db.UpdateFoundryDashboard(ctx, workspaceID, id26("DB-none"), adminEdits, 1, model.FoundryDashboardUpdate{Name: &name}),
			store.ErrNotFound)
		assert.Error(t, db.UpdateFoundryDashboard(ctx, workspaceID, d.ID, adminEdits, 3, model.FoundryDashboardUpdate{}), "an edit changes something")
	})

	t.Run("pins up to the cap, keeping the revision", func(t *testing.T) {
		db := newTestStore(t)
		createWorkspace(t, workspaceID)
		first, second := newDashboard(workspaceID, 1, dashAda), newDashboard(workspaceID, 2, dashAlan)
		require.NoError(t, db.CreateFoundryDashboard(ctx, first, 10))
		require.NoError(t, db.CreateFoundryDashboard(ctx, second, 10))
		require.NoError(t, db.SetFoundryDashboardPinned(ctx, workspaceID, first.ID, "user-admin", 1, true, 1))
		require.NoError(t, db.SetFoundryDashboardPinned(ctx, workspaceID, first.ID, "user-admin", 1, true, 1), "pinning twice is one pin")
		assert.Equal(t, model.FoundryDashboardLimit, dashboardRefusal(t, db.SetFoundryDashboardPinned(ctx, workspaceID, second.ID, "user-admin", 1, true, 1)))

		pinned, err := db.ListFoundryDashboards(ctx, workspaceID, true)
		require.NoError(t, err)
		require.Len(t, pinned, 1)
		assert.Equal(t, first.ID, pinned[0].ID)
		require.NotNil(t, pinned[0].PinnedByUserID)
		assert.Equal(t, "user-admin", *pinned[0].PinnedByUserID)
		assert.Equal(t, int64(1), pinned[0].Revision)
		all, err := db.ListFoundryDashboards(ctx, workspaceID, false)
		require.NoError(t, err)
		require.Len(t, all, 2)
		assert.Equal(t, first.ID, all[0].ID, "pinned ones first")

		require.NoError(t, db.SetFoundryDashboardPinned(ctx, workspaceID, first.ID, "user-admin", 1, false, 1))
		require.NoError(t, db.SetFoundryDashboardPinned(ctx, workspaceID, first.ID, "user-admin", 1, false, 1))
		require.NoError(t, db.SetFoundryDashboardPinned(ctx, workspaceID, second.ID, "user-admin", 1, true, 1), "unpinning freed its place")
		assert.ErrorIs(t, db.SetFoundryDashboardPinned(ctx, workspaceID, id26("DB-none"), "user-admin", 1, true, 1), store.ErrNotFound)
		assert.Error(t, db.SetFoundryDashboardPinned(ctx, workspaceID, first.ID, "user-admin", 0, true, 1), "a pin names the revision it saw")
	})

	t.Run("pins only the revision its pinner saw", func(t *testing.T) {
		db := newTestStore(t)
		createWorkspace(t, workspaceID)
		d := newDashboard(workspaceID, 1, dashAda)
		require.NoError(t, db.CreateFoundryDashboard(ctx, d, 10))
		name := "Changed while an owner looked"
		require.NoError(t, db.UpdateFoundryDashboard(ctx, workspaceID, d.ID, adaEdits, 1, model.FoundryDashboardUpdate{Name: &name}))
		assert.Equal(t, model.FoundryDashboardChanged, dashboardRefusal(t, db.SetFoundryDashboardPinned(ctx, workspaceID, d.ID, "user-admin", 1, true, 5)))
		got, err := db.GetFoundryDashboard(ctx, workspaceID, d.ID)
		require.NoError(t, err)
		assert.Nil(t, got.PinnedAt, "nothing it didn't see is pinned")
		require.NoError(t, db.SetFoundryDashboardPinned(ctx, workspaceID, d.ID, "user-admin", 2, true, 5))
	})

	t.Run("publishes at a revision, keeping the first token", func(t *testing.T) {
		db := newTestStore(t)
		o1, o2, o3 := sampleOntology(t, db, workspaceID, "P1"), sampleOntology(t, db, workspaceID, "P2"), sampleOntology(t, db, workspaceID, "P3")
		d := newDashboard(workspaceID, 1, dashAda)
		require.NoError(t, db.CreateFoundryDashboard(ctx, d, 10))

		stored, err := db.PublishFoundryDashboard(ctx, dashboardShare(d, 1, "a", o2.ID, o1.ID, o2.ID), keepExpiry)
		require.NoError(t, err)
		assert.Equal(t, strings.Repeat("a", 64), stored.TokenSHA256)
		assert.Equal(t, []string{o1.ID, o2.ID}, stored.OntologyIDs)
		assert.Nil(t, stored.Snapshot)
		first := stored.FirstPublishedAt

		shared, err := db.GetSharedFoundryDashboard(ctx, strings.Repeat("a", 64))
		require.NoError(t, err)
		assert.Equal(t, d.ID, shared.DashboardID)
		assert.JSONEq(t, `{"v":1,"tiles":[]}`, string(shared.Snapshot))

		name := "Renamed"
		require.NoError(t, db.UpdateFoundryDashboard(ctx, workspaceID, d.ID, adaEdits, 1, model.FoundryDashboardUpdate{Name: &name}))
		assert.Equal(t, model.FoundryDashboardChanged, dashboardRefusal(t, func() error {
			_, err := db.PublishFoundryDashboard(ctx, dashboardShare(d, 1, "b"), keepExpiry)
			return err
		}()), "a publish of a revision since edited")

		again := dashboardShare(d, 2, "b", o3.ID)
		again.Snapshot = json.RawMessage(`{"v":1,"tiles":[],"name":"Renamed"}`)
		stored, err = db.PublishFoundryDashboard(ctx, again, keepExpiry)
		require.NoError(t, err)
		assert.Equal(t, strings.Repeat("a", 64), stored.TokenSHA256, "republishing keeps the link")
		assert.Equal(t, []byte("sealed-a"), stored.TokenSealed)
		assert.Equal(t, int64(2), stored.SpecRevision)
		assert.Equal(t, []string{o3.ID}, stored.OntologyIDs)
		assert.Equal(t, first, stored.FirstPublishedAt)
		_, err = db.GetSharedFoundryDashboard(ctx, strings.Repeat("b", 64))
		assert.ErrorIs(t, err, store.ErrNotFound, "the second token is no link")
		shared, err = db.GetSharedFoundryDashboard(ctx, strings.Repeat("a", 64))
		require.NoError(t, err)
		assert.Contains(t, string(shared.Snapshot), "Renamed")

		got, err := db.GetFoundryDashboard(ctx, workspaceID, d.ID)
		require.NoError(t, err)
		require.NotNil(t, got.Share)
		assert.Equal(t, int64(2), got.Share.SpecRevision)

		require.NoError(t, db.UnpublishFoundryDashboard(ctx, workspaceID, d.ID))
		assert.ErrorIs(t, db.UnpublishFoundryDashboard(ctx, workspaceID, d.ID), store.ErrNotFound)
		_, err = db.GetSharedFoundryDashboard(ctx, strings.Repeat("a", 64))
		assert.ErrorIs(t, err, store.ErrNotFound, "unpublishing ends the link")

		stored, err = db.PublishFoundryDashboard(ctx, dashboardShare(d, 2, "c"), keepExpiry)
		require.NoError(t, err)
		assert.Equal(t, strings.Repeat("c", 64), stored.TokenSHA256, "publishing again makes a new link")

		_, err = db.PublishFoundryDashboard(ctx, dashboardShare(&model.FoundryDashboard{ID: id26("DB-none"), WorkspaceID: workspaceID}, 1, "d"), keepExpiry)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = db.PublishFoundryDashboard(ctx, &model.FoundryDashboardShare{DashboardID: d.ID}, keepExpiry)
		assert.Error(t, err, "a share needs its parts")
	})

	t.Run("ends a link at its expiry, and makes a new one once it has", func(t *testing.T) {
		db := newTestStore(t)
		d := newDashboard(workspaceID, 1, dashAda)
		require.NoError(t, db.CreateFoundryDashboard(ctx, d, 10))
		_, err := db.PublishFoundryDashboard(ctx, dashboardShare(d, 1, "a"), model.FoundryDashboardLinkExpiry{Keep: true, At: &time.Time{}})
		assert.Error(t, err, "an expiry both kept and set")

		in30 := time.Now().Add(30 * 24 * time.Hour).Truncate(time.Microsecond)
		stored, err := db.PublishFoundryDashboard(ctx, dashboardShare(d, 1, "a"), model.FoundryDashboardLinkExpiry{At: &in30})
		require.NoError(t, err)
		require.NotNil(t, stored.ExpiresAt)
		assert.True(t, in30.Equal(*stored.ExpiresAt))
		shared, err := db.GetSharedFoundryDashboard(ctx, strings.Repeat("a", 64))
		require.NoError(t, err)
		require.NotNil(t, shared.ExpiresAt)
		assert.True(t, in30.Equal(*shared.ExpiresAt))

		stored, err = db.PublishFoundryDashboard(ctx, dashboardShare(d, 1, "b"), keepExpiry)
		require.NoError(t, err)
		assert.Equal(t, strings.Repeat("a", 64), stored.TokenSHA256)
		require.NotNil(t, stored.ExpiresAt, "kept")
		assert.True(t, in30.Equal(*stored.ExpiresAt))
		stored, err = db.PublishFoundryDashboard(ctx, dashboardShare(d, 1, "b"), model.FoundryDashboardLinkExpiry{})
		require.NoError(t, err)
		assert.Nil(t, stored.ExpiresAt, "never, when asked")
		assert.Equal(t, strings.Repeat("a", 64), stored.TokenSHA256, "a new expiry keeps the link")

		// Once it has expired, a publish that chooses an expiry makes a new
		// link from its own token, one that keeps it is refused, and the old
		// link stays dead.
		past := time.Now().Add(-time.Minute).Truncate(time.Microsecond)
		_, err = db.PublishFoundryDashboard(ctx, dashboardShare(d, 1, "b"), model.FoundryDashboardLinkExpiry{At: &past})
		require.NoError(t, err)
		got, err := db.GetFoundryDashboard(ctx, workspaceID, d.ID)
		require.NoError(t, err)
		require.NotNil(t, got.Share)
		assert.True(t, got.Share.Expired(time.Now()))
		assert.Equal(t, model.FoundryDashboardExpired, dashboardRefusal(t, func() error {
			_, err := db.PublishFoundryDashboard(ctx, dashboardShare(d, 1, "c"), keepExpiry)
			return err
		}()))
		stored, err = db.PublishFoundryDashboard(ctx, dashboardShare(d, 1, "c"), model.FoundryDashboardLinkExpiry{})
		require.NoError(t, err)
		assert.Equal(t, strings.Repeat("c", 64), stored.TokenSHA256)
		assert.Nil(t, stored.ExpiresAt, "never, as chosen")
		assert.Equal(t, stored.PublishedAt, stored.FirstPublishedAt, "first published now")
		_, err = db.GetSharedFoundryDashboard(ctx, strings.Repeat("a", 64))
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("keeps a workspace's public links on until an owner turns them off", func(t *testing.T) {
		db := newTestStore(t)
		settings, err := db.GetFoundryDashboardSettings(ctx, workspaceID)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryDashboardSettings{WorkspaceID: workspaceID, PublicLinks: true}, settings, "no row: on")
		require.NoError(t, db.SetFoundryDashboardSettings(ctx, workspaceID, dashAda, false))
		settings, err = db.GetFoundryDashboardSettings(ctx, workspaceID)
		require.NoError(t, err)
		assert.False(t, settings.PublicLinks)
		assert.Equal(t, dashAda, settings.UpdatedByUserID)
		require.NotNil(t, settings.UpdatedAt)
		require.NoError(t, db.SetFoundryDashboardSettings(ctx, workspaceID, dashAlan, true))
		settings, err = db.GetFoundryDashboardSettings(ctx, workspaceID)
		require.NoError(t, err)
		assert.True(t, settings.PublicLinks)
		assert.Equal(t, dashAlan, settings.UpdatedByUserID)
		other, err := db.GetFoundryDashboardSettings(ctx, id26("DBW-other"))
		require.NoError(t, err)
		assert.True(t, other.PublicLinks, "each workspace's own")
		assert.Error(t, db.SetFoundryDashboardSettings(ctx, workspaceID, "", false), "who changed it is kept")
	})

	t.Run("deletes a dashboard with its snapshot", func(t *testing.T) {
		db := newTestStore(t)
		createWorkspace(t, workspaceID)
		d := newDashboard(workspaceID, 1, dashAda)
		require.NoError(t, db.CreateFoundryDashboard(ctx, d, 10))
		_, err := db.PublishFoundryDashboard(ctx, dashboardShare(d, 1, "a"), keepExpiry)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryDashboardNotEditable, dashboardRefusal(t, db.DeleteFoundryDashboard(ctx, workspaceID, d.ID, alanEdits)))
		assert.Equal(t, model.FoundryDashboardNotEditable, dashboardRefusal(t, db.DeleteFoundryDashboard(ctx, workspaceID, d.ID, adaEdits)),
			"its creator, a member, can't take its public link down")
		_, err = db.GetSharedFoundryDashboard(ctx, strings.Repeat("a", 64))
		require.NoError(t, err)
		require.NoError(t, db.UnpublishFoundryDashboard(ctx, workspaceID, d.ID))
		require.NoError(t, db.DeleteFoundryDashboard(ctx, workspaceID, d.ID, adaEdits), "unpublished, its creator deletes it")
		assert.ErrorIs(t, db.DeleteFoundryDashboard(ctx, workspaceID, d.ID, adaEdits), store.ErrNotFound)

		d = newDashboard(workspaceID, 2, dashAda)
		require.NoError(t, db.CreateFoundryDashboard(ctx, d, 10))
		_, err = db.PublishFoundryDashboard(ctx, dashboardShare(d, 1, "a"), keepExpiry)
		require.NoError(t, err)
		require.NoError(t, db.DeleteFoundryDashboard(ctx, workspaceID, d.ID, adminEdits), "an owner or admin deletes a published one")
		_, err = db.GetSharedFoundryDashboard(ctx, strings.Repeat("a", 64))
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("ends links with their workspace or an ontology they read", func(t *testing.T) {
		db := newTestStore(t)
		o, elsewhere := sampleOntology(t, db, workspaceID, "DS"), sampleOntology(t, db, workspaceID, "DS-else")
		d, other := newDashboard(workspaceID, 1, dashAda), newDashboard(workspaceID, 2, dashAda)
		require.NoError(t, db.CreateFoundryDashboard(ctx, d, 10))
		require.NoError(t, db.CreateFoundryDashboard(ctx, other, 10))
		_, err := db.PublishFoundryDashboard(ctx, dashboardShare(d, 1, "a", o.ID, elsewhere.ID), keepExpiry)
		require.NoError(t, err)
		_, err = db.PublishFoundryDashboard(ctx, dashboardShare(other, 1, "b", elsewhere.ID), keepExpiry)
		require.NoError(t, err)

		require.NoError(t, db.DeleteFoundryOntology(ctx, workspaceID, o.ID, id26("FN-DS")))
		_, err = db.GetSharedFoundryDashboard(ctx, strings.Repeat("a", 64))
		assert.ErrorIs(t, err, store.ErrNotFound, "deleting an ontology deletes the snapshots that read it")
		_, err = db.GetSharedFoundryDashboard(ctx, strings.Repeat("b", 64))
		require.NoError(t, err, "and no other")

		// A publish that read it before it was deleted commits after: refused,
		// so nothing of it is left public.
		assert.Equal(t, model.FoundryDashboardUnreadable, dashboardRefusal(t, func() error {
			_, err := db.PublishFoundryDashboard(ctx, dashboardShare(d, 1, "c", o.ID, elsewhere.ID), keepExpiry)
			return err
		}()))
		assert.Equal(t, model.FoundryDashboardUnreadable, dashboardRefusal(t, func() error {
			_, err := db.PublishFoundryDashboard(ctx, dashboardShare(d, 1, "c", id26("FO-none")), keepExpiry)
			return err
		}()), "an ontology the workspace never had")
		_, err = db.GetSharedFoundryDashboard(ctx, strings.Repeat("c", 64))
		assert.ErrorIs(t, err, store.ErrNotFound)

		setWorkspaceStatus(t, workspaceID, "deleting")
		_, err = db.GetSharedFoundryDashboard(ctx, strings.Repeat("b", 64))
		assert.ErrorIs(t, err, store.ErrNotFound, "a workspace being deleted serves no link")
		setWorkspaceStatus(t, workspaceID, "active")
		require.NoError(t, db.WithTx(ctx, func(tx store.Store) error { return tx.CancelWorkspaceFoundryWork(ctx, workspaceID) }))
		_, err = db.GetSharedFoundryDashboard(ctx, strings.Repeat("b", 64))
		assert.ErrorIs(t, err, store.ErrNotFound, "deleting the workspace deletes its snapshots")
	})
}
