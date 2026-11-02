package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// signedInConnector writes a live GitHub connector of the workspace that
// reads with account, as user.
func signedInConnector(t *testing.T, db store.Store, workspaceID, id, account, user string) {
	t.Helper()
	now := time.Now()
	require.NoError(t, db.CreateConnector(context.Background(), &model.Connector{
		ID: id, WorkspaceID: workspaceID, Kind: "github", Name: "GitHub", Config: json.RawMessage(`{"review_days":90}`),
		SecretCiphertext: []byte("sealed-empty"), SecretKeyID: "k1", NextSyncAt: &now,
		Account: &model.ConnectorAccount{ID: account, UserID: user, Label: "ada-l", Identity: "583231"},
	}))
}

// purges is every queued account purge, as kind/account@user.
func purges(t *testing.T) []string {
	t.Helper()
	rows, err := testDB.Query(`SELECT CONCAT(kind, '/', account_id, '@', user_id) FROM foundry_connector_account_purges ORDER BY account_id`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		require.NoError(t, rows.Scan(&p))
		out = append(out, p)
	}
	return out
}

func TestConnectorAccounts(t *testing.T) {
	ws, ada, grace := id26("ACC-WS"), id26("ACC-ADA"), id26("ACC-GRACE")

	t.Run("stores a signed-in connector's account", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		signedInConnector(t, db, ws, id26("ACC-C1"), "acct-1", ada)
		c, err := db.GetConnector(ctx, ws, id26("ACC-C1"))
		require.NoError(t, err)
		require.NotNil(t, c.Account)
		assert.Equal(t, "acct-1", c.Account.ID)
		assert.Equal(t, ada, c.Account.UserID)
		assert.Equal(t, "ada-l", c.Account.Label)
		assert.Equal(t, "583231", c.Account.Identity)
		assert.Equal(t, 1, c.Account.Generation)
		assert.Nil(t, c.Account.Broken)
		assert.NotNil(t, c.Account.CheckedAt)
		assert.NotNil(t, c.Account.ConnectedAt)

		connectorRow(t, db, ws, id26("ACC-KEY"))
		key, err := db.GetConnector(ctx, ws, id26("ACC-KEY"))
		require.NoError(t, err)
		assert.Nil(t, key.Account, "a connector with a key has no account")
	})

	// A member's leaving stops every connector reading with their access,
	// and only those: its queued sync ends without running.
	t.Run("breaks a member's accounts when they leave", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		signedInConnector(t, db, ws, id26("ACC-ADA1"), "acct-1", ada)
		signedInConnector(t, db, ws, id26("ACC-ADA2"), "acct-2", ada)
		signedInConnector(t, db, ws, id26("ACC-GRACE"), "acct-3", grace)
		signedInConnector(t, db, id26("ACC-OTHER-WS"), id26("ACC-ELSEWHERE"), "acct-4", ada)
		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("ACC-SYNC"), ConnectorID: id26("ACC-ADA1"), WorkspaceID: ws}))

		require.NoError(t, db.BreakConnectorAccounts(ctx, ws, ada, "member_left"))
		for _, tc := range []struct {
			id     string
			broken bool
		}{{id26("ACC-ADA1"), true}, {id26("ACC-ADA2"), true}, {id26("ACC-GRACE"), false}} {
			c, err := db.GetConnector(ctx, ws, tc.id)
			require.NoError(t, err)
			if !tc.broken {
				assert.Nil(t, c.Account.Broken, tc.id)
				assert.NotNil(t, c.NextSyncAt, tc.id)
				continue
			}
			require.NotNil(t, c.Account.Broken, tc.id)
			assert.Equal(t, "member_left", *c.Account.Broken)
			assert.NotNil(t, c.Account.BrokenAt)
			assert.Nil(t, c.NextSyncAt, "a broken account is scheduled for nothing")
		}
		elsewhere, err := db.GetConnector(ctx, id26("ACC-OTHER-WS"), id26("ACC-ELSEWHERE"))
		require.NoError(t, err)
		assert.Nil(t, elsewhere.Account.Broken, "only the workspace the member left")

		sync, err := db.GetConnectorSync(ctx, ws, id26("ACC-SYNC"))
		require.NoError(t, err)
		assert.Equal(t, model.ConnectorSyncFailed, sync.Status)
		_, err = db.GetActiveConnectorSync(ctx, ws, id26("ACC-ADA1"))
		assert.ErrorIs(t, err, store.ErrNotFound, "its queued sync ended")

		require.NoError(t, db.BreakConnectorAccounts(ctx, ws, ada, "access_revoked"))
		c, err := db.GetConnector(ctx, ws, id26("ACC-ADA1"))
		require.NoError(t, err)
		assert.Equal(t, "member_left", *c.Account.Broken, "a broken account keeps its first reason")
		assert.Error(t, db.BreakConnectorAccounts(ctx, ws, ada, "gone"), "an unknown reason is refused")
	})

	// A host removes the member in its own transaction, and breaks the
	// accounts in it, so a rollback keeps them.
	t.Run("breaks accounts inside the host's transaction", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		signedInConnector(t, db, ws, id26("ACC-TX"), "acct-1", ada)
		tx, err := testDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.NoError(t, store.InTx(tx, hostWorkspaces{}).BreakConnectorAccounts(ctx, ws, ada, "member_left"))
		require.NoError(t, tx.Rollback())
		c, err := db.GetConnector(ctx, ws, id26("ACC-TX"))
		require.NoError(t, err)
		assert.Nil(t, c.Account.Broken, "rolled back with the host's transaction")

		tx, err = testDB.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.NoError(t, store.InTx(tx, hostWorkspaces{}).BreakConnectorAccounts(ctx, ws, ada, "member_left"))
		require.NoError(t, tx.Commit())
		c, err = db.GetConnector(ctx, ws, id26("ACC-TX"))
		require.NoError(t, err)
		assert.NotNil(t, c.Account.Broken)
	})

	// A sync that asked for a token before the connector was signed in again
	// can't break the account it reads with now.
	t.Run("breaks one account only at the generation it asked with", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		signedInConnector(t, db, ws, id26("ACC-GEN"), "acct-1", ada)
		broke, err := db.BreakConnectorAccount(ctx, ws, id26("ACC-GEN"), 0, "access_revoked")
		require.NoError(t, err)
		assert.False(t, broke, "an older generation breaks nothing")
		c, err := db.GetConnector(ctx, ws, id26("ACC-GEN"))
		require.NoError(t, err)
		assert.Nil(t, c.Account.Broken)

		broke, err = db.BreakConnectorAccount(ctx, ws, id26("ACC-GEN"), 1, "access_revoked")
		require.NoError(t, err)
		assert.True(t, broke)
		broke, err = db.BreakConnectorAccount(ctx, ws, id26("ACC-GEN"), 1, "expired")
		require.NoError(t, err)
		assert.False(t, broke, "a broken account stays as it broke")
		c, err = db.GetConnector(ctx, ws, id26("ACC-GEN"))
		require.NoError(t, err)
		assert.Equal(t, "access_revoked", *c.Account.Broken)
	})

	// Only a new account fixes one the broker lost: account_missing replaces
	// a reason recorded before, keeping when the data froze.
	t.Run("records a lost account over another reason", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		signedInConnector(t, db, ws, id26("ACC-LOST"), "acct-1", ada)
		_, err := db.BreakConnectorAccount(ctx, ws, id26("ACC-LOST"), 1, "access_revoked")
		require.NoError(t, err)
		before, err := db.GetConnector(ctx, ws, id26("ACC-LOST"))
		require.NoError(t, err)
		for _, tc := range []struct {
			generation int
			reason     string
			want       bool
		}{{1, "expired", false}, {0, "account_missing", false}, {1, "account_missing", true}, {1, "account_missing", false}} {
			broke, err := db.BreakConnectorAccount(ctx, ws, id26("ACC-LOST"), tc.generation, tc.reason)
			require.NoError(t, err)
			assert.Equal(t, tc.want, broke, "%s at %d", tc.reason, tc.generation)
		}
		c, err := db.GetConnector(ctx, ws, id26("ACC-LOST"))
		require.NoError(t, err)
		assert.Equal(t, "account_missing", *c.Account.Broken)
		assert.Equal(t, *before.Account.BrokenAt, *c.Account.BrokenAt, "its data froze when it first broke")
	})

	// Nothing schedules a broken account: not the schedule, and not a sync
	// that ends after the break, even one that succeeded.
	t.Run("keeps a broken account unscheduled", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		signedInConnector(t, db, ws, id26("ACC-DUE"), "acct-1", ada)
		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("ACC-RUN"), ConnectorID: id26("ACC-DUE"), WorkspaceID: ws}))
		claimed, err := db.ClaimConnectorSync(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		require.NotNil(t, claimed)
		_, err = testDB.Exec(`UPDATE connectors SET next_sync_at=NOW(6) - INTERVAL 1 MINUTE WHERE id=?`, id26("ACC-DUE"))
		require.NoError(t, err)

		_, err = db.BreakConnectorAccount(ctx, ws, id26("ACC-DUE"), 1, "member_left")
		require.NoError(t, err)
		_, err = testDB.Exec(`UPDATE connectors SET next_sync_at=NOW(6) - INTERVAL 1 MINUTE WHERE id=?`, id26("ACC-DUE"))
		require.NoError(t, err)
		n, err := db.QueueDueConnectorSyncs(ctx, 10, time.Minute)
		require.NoError(t, err)
		assert.Zero(t, n, "a broken account is never due")

		rows := int64(3)
		next := time.Now().Add(time.Hour)
		require.NoError(t, db.FinishConnectorSync(ctx, claimed.ID, "worker-1", model.ConnectorSyncOutcome{
			Status: model.ConnectorSyncSucceeded, RowsSynced: &rows, Schedule: &model.ConnectorScheduleOutcome{NextSyncAt: &next},
		}))
		c, err := db.GetConnector(ctx, ws, id26("ACC-DUE"))
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(-time.Minute), *c.NextSyncAt, 30*time.Second,
			"the finish leaves the schedule as it found it")
	})

	t.Run("lists accounts due a check, and marks one checked", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		signedInConnector(t, db, ws, id26("ACC-OLD"), "acct-1", ada)
		signedInConnector(t, db, ws, id26("ACC-NEW"), "acct-2", ada)
		signedInConnector(t, db, ws, id26("ACC-BROKE"), "acct-3", grace)
		connectorRow(t, db, ws, id26("ACC-KEY"))
		_, err := testDB.Exec(`UPDATE connectors SET account_checked_at=NOW(6) - INTERVAL 8 DAY WHERE id IN (?, ?)`, id26("ACC-OLD"), id26("ACC-BROKE"))
		require.NoError(t, err)
		require.NoError(t, db.BreakConnectorAccounts(ctx, ws, grace, "expired"))

		due, err := db.ListConnectorAccountChecks(ctx, time.Now().Add(-7*24*time.Hour), 10)
		require.NoError(t, err)
		require.Len(t, due, 1)
		assert.Equal(t, id26("ACC-OLD"), due[0].ID)
		before := due[0].UpdatedAt

		require.NoError(t, db.MarkConnectorAccountChecked(ctx, ws, id26("ACC-OLD")))
		due, err = db.ListConnectorAccountChecks(ctx, time.Now().Add(-7*24*time.Hour), 10)
		require.NoError(t, err)
		assert.Empty(t, due)
		c, err := db.GetConnector(ctx, ws, id26("ACC-OLD"))
		require.NoError(t, err)
		assert.Equal(t, before, c.UpdatedAt, "a check is no change to the connector")

		// A check that got no token comes again an hour later; a deferral
		// never moves a check back.
		_, err = testDB.Exec(`UPDATE connectors SET account_checked_at=NOW(6) - INTERVAL 8 DAY WHERE id=?`, id26("ACC-NEW"))
		require.NoError(t, err)
		retry := time.Now().Add(time.Hour - 7*24*time.Hour)
		require.NoError(t, db.DeferConnectorAccountCheck(ctx, ws, id26("ACC-NEW"), retry))
		due, err = db.ListConnectorAccountChecks(ctx, time.Now().Add(-7*24*time.Hour), 10)
		require.NoError(t, err)
		assert.Empty(t, due, "deferred an hour")
		due, err = db.ListConnectorAccountChecks(ctx, time.Now().Add(2*time.Hour-7*24*time.Hour), 10)
		require.NoError(t, err)
		require.Len(t, due, 1)
		assert.Equal(t, id26("ACC-NEW"), due[0].ID)
		require.NoError(t, db.DeferConnectorAccountCheck(ctx, ws, id26("ACC-OLD"), retry))
		c, err = db.GetConnector(ctx, ws, id26("ACC-OLD"))
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now(), *c.Account.CheckedAt, time.Minute, "a deferral never moves a check back")
	})

	// Deleting a connector, or a workspace's, lets its account go: the purge
	// deletes it from the broker.
	t.Run("purges a deleted connector's account", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		createWorkspace(t, ws)
		signedInConnector(t, db, ws, id26("ACC-DEL"), "acct-1", ada)
		signedInConnector(t, db, ws, id26("ACC-WSDEL"), "acct-2", grace)
		connectorRow(t, db, ws, id26("ACC-KEY"))
		require.NoError(t, db.DeleteConnector(ctx, ws, id26("ACC-DEL")))
		assert.Equal(t, []string{"github/acct-1@" + ada}, purges(t))

		accounts, err := db.ListConnectorAccounts(ctx, ws)
		require.NoError(t, err)
		assert.ElementsMatch(t, []model.ConnectorAccountRef{
			{ConnectorID: id26("ACC-WSDEL"), WorkspaceID: ws, Kind: "github", AccountID: "acct-2", UserID: grace},
			{WorkspaceID: ws, Kind: "github", AccountID: "acct-1", UserID: ada},
		}, accounts, "a host deletes them all before the workspace's connectors")

		require.NoError(t, db.DeleteWorkspaceConnectors(ctx, ws))
		assert.Equal(t, []string{"github/acct-1@" + ada, "github/acct-2@" + grace}, purges(t))
	})

	// An account two live connectors read with goes only with the last.
	t.Run("purges a deleted connector's account only once nothing reads with it", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		createWorkspace(t, ws)
		signedInConnector(t, db, ws, id26("ACC-SHARE1"), "acct-shared", ada)
		signedInConnector(t, db, ws, id26("ACC-SHARE2"), "acct-shared", ada)
		require.NoError(t, db.DeleteConnector(ctx, ws, id26("ACC-SHARE1")))
		assert.Empty(t, purges(t), "the other connector still reads with it")
		require.NoError(t, db.DeleteConnector(ctx, ws, id26("ACC-SHARE2")))
		assert.Equal(t, []string{"github/acct-shared@" + ada}, purges(t))

		signedInConnector(t, db, ws, id26("ACC-SHARE3"), "acct-both", ada)
		signedInConnector(t, db, ws, id26("ACC-SHARE4"), "acct-both", ada)
		require.NoError(t, db.DeleteWorkspaceConnectors(ctx, ws))
		assert.Equal(t, []string{"github/acct-both@" + ada, "github/acct-shared@" + ada}, purges(t),
			"a workspace's connectors let it go once all are deleted")
	})

	t.Run("works through purges with backoff", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		signedInConnector(t, db, ws, id26("ACC-P1"), "acct-1", ada)
		signedInConnector(t, db, ws, id26("ACC-P2"), "acct-2", ada)
		require.NoError(t, db.DeleteConnector(ctx, ws, id26("ACC-P1")))
		require.NoError(t, db.DeleteConnector(ctx, ws, id26("ACC-P2")))
		n, oldest, err := db.ConnectorAccountPurgeBacklog(ctx)
		require.NoError(t, err)
		assert.Equal(t, 2, n)
		require.NotNil(t, oldest)

		due, err := db.ListConnectorAccountPurges(ctx, 10)
		require.NoError(t, err)
		require.Len(t, due, 2)
		require.NoError(t, db.RetryConnectorAccountPurge(ctx, due[0].ID, "unavailable", time.Hour))
		require.NoError(t, db.DeleteConnectorAccountPurge(ctx, due[1].ID))

		due, err = db.ListConnectorAccountPurges(ctx, 10)
		require.NoError(t, err)
		assert.Empty(t, due, "a retried purge waits for its backoff")
		n, _, err = db.ConnectorAccountPurgeBacklog(ctx)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		_, err = testDB.Exec(`UPDATE foundry_connector_account_purges SET next_attempt_at=NOW(6)`)
		require.NoError(t, err)
		due, err = db.ListConnectorAccountPurges(ctx, 10)
		require.NoError(t, err)
		require.Len(t, due, 1)
		assert.Equal(t, 1, due[0].Attempts)
	})
}

func TestConnectorSignIns(t *testing.T) {
	ws, ada, grace := id26("SI-WS"), id26("SI-ADA"), id26("SI-GRACE")
	newSignIn := func(id, user string) *model.ConnectorSignIn {
		return &model.ConnectorSignIn{ID: id26(id), WorkspaceID: ws, Kind: "github", Name: "GitHub",
			Config: json.RawMessage(`{"review_days":90}`), CreatedByUserID: user}
	}
	start := func(t *testing.T, db store.Store, si *model.ConnectorSignIn) (*model.ConnectorSignIn, error) {
		t.Helper()
		return db.StartConnectorSignIn(context.Background(), model.ConnectorSignInStart{SignIn: si, TTL: 15 * time.Minute, MaxLive: 2, MaxPerHour: 3})
	}
	// finishing starts, authorizes and claims a sign-in.
	finishing := func(t *testing.T, db store.Store, si *model.ConnectorSignIn) {
		t.Helper()
		ctx := context.Background()
		_, err := start(t, db, si)
		require.NoError(t, err)
		require.NoError(t, db.AuthorizeConnectorSignIn(ctx, ws, si.ID, 15*time.Minute, 5*time.Minute))
		require.NoError(t, db.ClaimConnectorSignIn(ctx, ws, si.ID))
	}
	get := func(t *testing.T, db store.Store, id string) *model.ConnectorSignIn {
		t.Helper()
		si, err := db.GetConnectorSignIn(context.Background(), ws, id26(id))
		require.NoError(t, err)
		return si
	}

	t.Run("starts a sign-in once per client token, within the caps", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		createWorkspace(t, ws)
		token := "tok-1"
		first := newSignIn("SI-1", ada)
		first.ClientToken = &token
		stored, err := start(t, db, first)
		require.NoError(t, err)
		assert.Equal(t, model.ConnectorSignInPending, stored.Status)
		assert.WithinDuration(t, time.Now().Add(15*time.Minute), stored.ExpiresAt, time.Minute)
		assert.JSONEq(t, `{"review_days":90}`, string(stored.Config))

		replay := newSignIn("SI-1B", ada)
		replay.ClientToken = &token
		again, err := start(t, db, replay)
		require.NoError(t, err)
		assert.Equal(t, id26("SI-1"), again.ID, "a client token seen before returns its sign-in")

		_, err = start(t, db, newSignIn("SI-2", ada))
		require.NoError(t, err)
		_, err = start(t, db, newSignIn("SI-3", ada))
		assert.ErrorIs(t, err, store.ErrLimitExceeded, "two live sign-ins is the cap")
		_, err = start(t, db, newSignIn("SI-G", grace))
		require.NoError(t, err, "the cap is each user's")

		_, err = testDB.Exec(`UPDATE foundry_connector_sign_ins SET expires_at=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, id26("SI-2"))
		require.NoError(t, err)
		_, err = start(t, db, newSignIn("SI-4", ada))
		require.NoError(t, err, "an expired sign-in no longer counts as live")
		_, err = testDB.Exec(`UPDATE foundry_connector_sign_ins SET expires_at=NOW(6) - INTERVAL 1 SECOND WHERE created_by_user_id=?`, ada)
		require.NoError(t, err)
		_, err = start(t, db, newSignIn("SI-5", ada))
		assert.ErrorIs(t, err, store.ErrLimitExceeded, "three starts an hour is the cap")

		_, err = db.StartConnectorSignIn(ctx, model.ConnectorSignInStart{SignIn: &model.ConnectorSignIn{ID: id26("SI-GONE"),
			WorkspaceID: id26("SI-NO-WS"), Kind: "github", Name: "GitHub", Config: json.RawMessage(`{}`), CreatedByUserID: ada},
			TTL: time.Minute, MaxLive: 5, MaxPerHour: 20})
		assert.ErrorIs(t, err, store.ErrNotFound, "a workspace that is gone starts nothing")
	})

	// Signing a connector in again is one member's at a time.
	t.Run("signs a connector in again one member at a time", func(t *testing.T) {
		db := newTestStore(t)
		createWorkspace(t, ws)
		signedInConnector(t, db, ws, id26("SI-CONN"), "acct-1", ada)
		connectorID := id26("SI-CONN")
		mine := newSignIn("SI-AGAIN", ada)
		mine.ConnectorID = &connectorID
		_, err := start(t, db, mine)
		require.NoError(t, err)

		repeat := newSignIn("SI-AGAIN2", ada)
		repeat.ConnectorID = &connectorID
		same, err := start(t, db, repeat)
		require.NoError(t, err)
		assert.Equal(t, id26("SI-AGAIN"), same.ID, "the same member gets their live sign-in back")

		theirs := newSignIn("SI-THEIRS", grace)
		theirs.ConnectorID = &connectorID
		_, err = start(t, db, theirs)
		var busy *store.SignInInProgressError
		require.ErrorAs(t, err, &busy)
		assert.Equal(t, ada, busy.UserID)

		gone := id26("SI-NO-CONN")
		missing := newSignIn("SI-MISSING", grace)
		missing.ConnectorID = &gone
		_, err = start(t, db, missing)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	// Authorizing again keeps the same sign-in, moves its expiry, and ends
	// with the sign-in; a finish is claimed once.
	t.Run("authorizes until it ends, and is claimed once", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		createWorkspace(t, ws)
		_, err := start(t, db, newSignIn("SI-AUTH", ada))
		require.NoError(t, err)
		assert.ErrorIs(t, db.ClaimConnectorSignIn(ctx, ws, id26("SI-AUTH")), store.ErrNotFound, "a pending sign-in has no callback to finish")

		require.NoError(t, db.SetConnectorSignInVerifier(ctx, ws, id26("SI-AUTH"), []byte("sealed"), "k1"))
		assert.ErrorIs(t, db.SetConnectorSignInVerifier(ctx, ws, id26("SI-AUTH"), []byte("other"), "k1"), store.ErrNotFound,
			"one verifier serves every authorization")
		_, err = testDB.Exec(`UPDATE foundry_connector_sign_ins SET expires_at=NOW(6) + INTERVAL 1 MINUTE WHERE id=?`, id26("SI-AUTH"))
		require.NoError(t, err)
		require.NoError(t, db.AuthorizeConnectorSignIn(ctx, ws, id26("SI-AUTH"), 15*time.Minute, 5*time.Minute))
		si := get(t, db, "SI-AUTH")
		assert.Equal(t, model.ConnectorSignInAuthorizing, si.Status)
		assert.Equal(t, []byte("sealed"), si.VerifierCiphertext)
		assert.WithinDuration(t, time.Now().Add(15*time.Minute), si.ExpiresAt, time.Minute, "authorizing moves the expiry")

		require.NoError(t, db.ClaimConnectorSignIn(ctx, ws, id26("SI-AUTH")))
		assert.ErrorIs(t, db.ClaimConnectorSignIn(ctx, ws, id26("SI-AUTH")), store.ErrNotFound, "a second finish claims nothing")
		assert.ErrorIs(t, db.AuthorizeConnectorSignIn(ctx, ws, id26("SI-AUTH"), 15*time.Minute, 5*time.Minute), store.ErrNotFound,
			"a finishing sign-in can't authorize")
		require.NoError(t, db.FailConnectorSignIn(ctx, ws, id26("SI-AUTH"), model.ConnectorSignInEnd{Status: model.ConnectorSignInDeclined}))
		require.NoError(t, db.AuthorizeConnectorSignIn(ctx, ws, id26("SI-AUTH"), 15*time.Minute, 5*time.Minute),
			"a declined sign-in authorizes again")

		_, err = testDB.Exec(`UPDATE foundry_connector_sign_ins SET expires_at=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, id26("SI-AUTH"))
		require.NoError(t, err)
		assert.ErrorIs(t, db.ClaimConnectorSignIn(ctx, ws, id26("SI-AUTH")), store.ErrNotFound, "an expired sign-in can't finish")
		assert.ErrorIs(t, db.AuthorizeConnectorSignIn(ctx, ws, id26("SI-AUTH"), 15*time.Minute, 5*time.Minute), store.ErrNotFound)
		assert.Equal(t, model.ConnectorSignInExpired, func() model.ConnectorSignInStatus {
			status, _ := get(t, db, "SI-AUTH").StatusAt(time.Now(), time.Now().Add(-5*time.Minute))
			return status
		}())
	})

	// A finish that fails after the broker saved the account lets it go.
	t.Run("purges the account of a failed finish", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		createWorkspace(t, ws)
		si := newSignIn("SI-FAIL", ada)
		finishing(t, db, si)
		require.NoError(t, db.SetConnectorSignInAccount(ctx, ws, si.ID, "acct-new"))
		assert.Equal(t, "acct-new", *get(t, db, "SI-FAIL").AccountID)
		require.NoError(t, db.FailConnectorSignIn(ctx, ws, si.ID, model.ConnectorSignInEnd{Status: model.ConnectorSignInFailed,
			Failure: model.SignInSettingsInvalid}))
		failed := get(t, db, "SI-FAIL")
		assert.Equal(t, model.ConnectorSignInFailed, failed.Status)
		assert.Equal(t, model.SignInSettingsInvalid, *failed.Failure)
		assert.Nil(t, failed.AccountID)
		assert.Equal(t, []string{"github/acct-new@" + ada}, purges(t))
		assert.ErrorIs(t, db.FailConnectorSignIn(ctx, ws, si.ID, model.ConnectorSignInEnd{Status: model.ConnectorSignInDeclined}),
			store.ErrNotFound, "only the finish ends it")
		assert.Error(t, db.FailConnectorSignIn(ctx, ws, si.ID, model.ConnectorSignInEnd{Status: model.ConnectorSignInFailed}),
			"a failure names why")
	})

	// An account the broker refused (and may have failed to delete) is
	// purged with the end, unless a live connector reads with it.
	t.Run("purges an account the broker refused", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		createWorkspace(t, ws)
		si := newSignIn("SI-REFUSED", ada)
		finishing(t, db, si)
		require.NoError(t, db.FailConnectorSignIn(ctx, ws, si.ID, model.ConnectorSignInEnd{Status: model.ConnectorSignInFailed,
			Failure: model.SignInNotThisWorkspace, Refused: &model.ConnectorAccount{ID: "acct-theirs", UserID: grace}}))
		assert.Equal(t, []string{"github/acct-theirs@" + grace}, purges(t))

		signedInConnector(t, db, ws, id26("SI-OWNS"), "acct-live", ada)
		other := newSignIn("SI-REFUSED2", ada)
		finishing(t, db, other)
		require.NoError(t, db.FailConnectorSignIn(ctx, ws, other.ID, model.ConnectorSignInEnd{Status: model.ConnectorSignInFailed,
			Failure: model.SignInNotThisWorkspace, Refused: &model.ConnectorAccount{ID: "acct-live"}}))
		assert.Equal(t, []string{"github/acct-theirs@" + grace}, purges(t), "a live connector's account stays")
	})

	// Signing in again may give the connector its own account back: a
	// failure then keeps it.
	t.Run("keeps a live connector's account when a sign-in again fails", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		createWorkspace(t, ws)
		signedInConnector(t, db, ws, id26("SI-OWN"), "acct-1", ada)
		connectorID := id26("SI-OWN")
		si := newSignIn("SI-OWNAGAIN", ada)
		si.ConnectorID = &connectorID
		finishing(t, db, si)
		require.NoError(t, db.SetConnectorSignInAccount(ctx, ws, si.ID, "acct-1"))
		require.NoError(t, db.FailConnectorSignIn(ctx, ws, si.ID, model.ConnectorSignInEnd{Status: model.ConnectorSignInFailed,
			Failure: model.SignInTokenRejected}))
		assert.Empty(t, purges(t))
	})

	t.Run("connects a new connector", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		createWorkspace(t, ws)
		si := newSignIn("SI-NEW", ada)
		finishing(t, db, si)
		require.NoError(t, db.SetConnectorSignInAccount(ctx, ws, si.ID, "acct-1"))
		now := time.Now()
		require.NoError(t, db.ConnectConnectorSignIn(ctx, model.ConnectorSignInConnect{WorkspaceID: ws, SignInID: si.ID,
			New: &model.Connector{ID: id26("SI-MADE"), WorkspaceID: ws, Kind: "github", Name: "GitHub", Config: si.Config,
				SecretCiphertext: []byte("sealed-empty"), SecretKeyID: "k1", NextSyncAt: &now, CreatedByUserID: &ada,
				Account: &model.ConnectorAccount{ID: "acct-1", UserID: ada, Label: "ada-l"}},
			DiscoveryProblem: "install it on acme"}))
		connected := get(t, db, "SI-NEW")
		assert.Equal(t, model.ConnectorSignInConnected, connected.Status)
		assert.Equal(t, id26("SI-MADE"), *connected.ConnectorID)
		assert.Equal(t, "install it on acme", *connected.DiscoveryProblem)
		c, err := db.GetConnector(ctx, ws, id26("SI-MADE"))
		require.NoError(t, err)
		assert.Equal(t, "acct-1", c.Account.ID)
		assert.Equal(t, 1, c.Account.Generation)
		assert.NotNil(t, c.NextSyncAt)
		status, _ := connected.StatusAt(time.Now().Add(time.Hour), time.Now())
		assert.Equal(t, model.ConnectorSignInConnected, status, "a connected sign-in never reads expired")
		assert.ErrorIs(t, db.ClaimConnectorSignIn(ctx, ws, si.ID), store.ErrNotFound)
	})

	// Signing in again resumes a broken connector with one sync, a
	// generation on; another identity reads in full, and the account it no
	// longer reads with goes.
	t.Run("signs a broken connector in again", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		createWorkspace(t, ws)
		connectorID := id26("SI-BROKEN")
		signedInConnector(t, db, ws, connectorID, "acct-1", ada)
		_, err := testDB.Exec(`UPDATE connectors SET change_cursor='{"since":"x"}', schedule_paused='install it' WHERE id=?`, connectorID)
		require.NoError(t, err)
		require.NoError(t, db.BreakConnectorAccounts(ctx, ws, ada, "member_left"))
		si := newSignIn("SI-FIX", grace)
		si.ConnectorID = &connectorID
		finishing(t, db, si)
		require.NoError(t, db.ConnectConnectorSignIn(ctx, model.ConnectorSignInConnect{WorkspaceID: ws, SignInID: si.ID,
			ConnectorID: connectorID, Account: model.ConnectorAccount{ID: "acct-2", UserID: grace, Label: "grace-h", Identity: "77"},
			IdentityChanged: true}))
		c, err := db.GetConnector(ctx, ws, connectorID)
		require.NoError(t, err)
		assert.Equal(t, model.ConnectorAccount{ID: "acct-2", UserID: grace, Label: "grace-h", Identity: "77", Generation: 2,
			CheckedAt: c.Account.CheckedAt, ConnectedAt: c.Account.ConnectedAt}, *c.Account)
		assert.NotNil(t, c.NextSyncAt, "it syncs once, now")
		assert.Nil(t, c.SchedulePaused)
		assert.Nil(t, c.ChangeCursor, "another identity reads in full")
		assert.True(t, get(t, db, "SI-FIX").IdentityChanged)
		assert.Equal(t, []string{"github/acct-1@" + ada}, purges(t))
	})

	// A sync that read with the old account, still running when the
	// connector is signed in again, can't undo what signing in did: its
	// cursor is dropped, the pause stays as signing in left it, and the
	// connector syncs at once, in full when the identity changed.
	t.Run("keeps a sign-in again from a sync of the old account", func(t *testing.T) {
		ctx := context.Background()
		for _, tc := range []struct {
			name    string
			broken  bool
			outcome model.ConnectorSyncOutcome
		}{
			{"that succeeds", false, model.ConnectorSyncOutcome{Status: model.ConnectorSyncSucceeded}},
			{"that fails for good, having read the account broken", true, model.ConnectorSyncOutcome{Status: model.ConnectorSyncFailed,
				Error: "GitHub needs signing in again"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				db := newTestStore(t)
				createWorkspace(t, ws)
				connectorID := id26("SI-RACE")
				signedInConnector(t, db, ws, connectorID, "acct-1", ada)
				_, err := testDB.Exec(`UPDATE connectors SET change_cursor='{"since":"old"}' WHERE id=?`, connectorID)
				require.NoError(t, err)
				require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("SI-S1"), ConnectorID: connectorID, WorkspaceID: ws}))
				s1, err := db.ClaimConnectorSync(ctx, "worker-1", time.Minute)
				require.NoError(t, err)
				if tc.broken {
					require.NoError(t, db.BreakConnectorAccounts(ctx, ws, ada, "member_left"))
				}

				si := newSignIn("SI-AGAIN-RACE", grace)
				si.ConnectorID = &connectorID
				finishing(t, db, si)
				require.NoError(t, db.ConnectConnectorSignIn(ctx, model.ConnectorSignInConnect{WorkspaceID: ws, SignInID: si.ID,
					ConnectorID: connectorID, Account: model.ConnectorAccount{ID: "acct-2", UserID: grace, Label: "grace-h"}, IdentityChanged: true}))
				// The scheduler finds it due while S1 runs, and unschedules it.
				_, err = db.QueueDueConnectorSyncs(ctx, 10, time.Minute)
				require.NoError(t, err)

				require.NoError(t, db.UpdateConnectorCursor(ctx, connectorID, 1, json.RawMessage(`{"since":"s1"}`), s1.ID, "worker-1"))
				generation, later := 1, time.Now().Add(24*time.Hour)
				outcome := tc.outcome
				outcome.Schedule = &model.ConnectorScheduleOutcome{NextSyncAt: &later, Paused: outcome.Error, Generation: &generation}
				require.NoError(t, db.FinishConnectorSync(ctx, s1.ID, "worker-1", outcome))

				c, err := db.GetConnector(ctx, ws, connectorID)
				require.NoError(t, err)
				assert.Nil(t, c.ChangeCursor, "the old account's cursor is dropped: the new identity reads in full")
				assert.Nil(t, c.SchedulePaused)
				require.NotNil(t, c.NextSyncAt, "it syncs at once with the new account")
				assert.WithinDuration(t, time.Now(), *c.NextSyncAt, time.Minute)
				assert.Nil(t, c.Account.Broken)
			})
		}

		// A sync of the current generation schedules as it always has.
		db := newTestStore(t)
		connectorID := id26("SI-CURRENT")
		signedInConnector(t, db, ws, connectorID, "acct-1", ada)
		require.NoError(t, db.EnqueueConnectorSync(ctx, &model.ConnectorSync{ID: id26("SI-S2"), ConnectorID: connectorID, WorkspaceID: ws}))
		s2, err := db.ClaimConnectorSync(ctx, "worker-1", time.Minute)
		require.NoError(t, err)
		require.NoError(t, db.UpdateConnectorCursor(ctx, connectorID, 1, json.RawMessage(`{"since":"s2"}`), s2.ID, "worker-1"))
		generation, later := 1, time.Now().Add(24*time.Hour)
		require.NoError(t, db.FinishConnectorSync(ctx, s2.ID, "worker-1", model.ConnectorSyncOutcome{Status: model.ConnectorSyncSucceeded,
			Schedule: &model.ConnectorScheduleOutcome{NextSyncAt: &later, Generation: &generation}}))
		c, err := db.GetConnector(ctx, ws, connectorID)
		require.NoError(t, err)
		assert.JSONEq(t, `{"since":"s2"}`, string(c.ChangeCursor))
		assert.WithinDuration(t, later, *c.NextSyncAt, time.Second)
	})

	t.Run("refuses a connection the workspace or connector went from", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		createWorkspace(t, ws)
		connectorID := id26("SI-DELETED")
		signedInConnector(t, db, ws, connectorID, "acct-1", ada)
		si := newSignIn("SI-LATE", ada)
		si.ConnectorID = &connectorID
		finishing(t, db, si)
		require.NoError(t, db.DeleteConnector(ctx, ws, connectorID))
		assert.ErrorIs(t, db.ConnectConnectorSignIn(ctx, model.ConnectorSignInConnect{WorkspaceID: ws, SignInID: si.ID,
			ConnectorID: connectorID, Account: model.ConnectorAccount{ID: "acct-1", UserID: ada}}), store.ErrNotFound)

		other := newSignIn("SI-DELWS", ada)
		finishing(t, db, other)
		setWorkspaceStatus(t, ws, "deleting")
		now := time.Now()
		assert.ErrorIs(t, db.ConnectConnectorSignIn(ctx, model.ConnectorSignInConnect{WorkspaceID: ws, SignInID: other.ID,
			New: &model.Connector{ID: id26("SI-NEVER"), WorkspaceID: ws, Kind: "github", Name: "GitHub", Config: other.Config,
				SecretCiphertext: []byte("x"), SecretKeyID: "k1", NextSyncAt: &now,
				Account: &model.ConnectorAccount{ID: "acct-9", UserID: ada}}}), store.ErrWorkspaceDeleting)
		_, err := db.GetConnector(ctx, ws, id26("SI-NEVER"))
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	// A finish that never ended (its process died) reads failed once stale,
	// may authorize again, and lets its account go.
	t.Run("recovers a finish that never ended", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		createWorkspace(t, ws)
		si := newSignIn("SI-STUCK", ada)
		finishing(t, db, si)
		require.NoError(t, db.SetConnectorSignInAccount(ctx, ws, si.ID, "acct-lost"))
		assert.ErrorIs(t, db.AuthorizeConnectorSignIn(ctx, ws, si.ID, 15*time.Minute, 5*time.Minute), store.ErrNotFound,
			"a finish still within its time keeps it")
		_, err := testDB.Exec(`UPDATE foundry_connector_sign_ins SET updated_at=NOW(6) - INTERVAL 6 MINUTE WHERE id=?`, si.ID)
		require.NoError(t, err)
		status, failure := get(t, db, "SI-STUCK").StatusAt(time.Now(), time.Now().Add(-5*time.Minute))
		assert.Equal(t, model.ConnectorSignInFailed, status)
		assert.Equal(t, model.SignInUnavailable, failure)
		require.NoError(t, db.AuthorizeConnectorSignIn(ctx, ws, si.ID, 15*time.Minute, 5*time.Minute))
		assert.Nil(t, get(t, db, "SI-STUCK").AccountID)
		assert.Equal(t, []string{"github/acct-lost@" + ada}, purges(t))
	})

	// A day after a sign-in ends its row goes; an account it holds that
	// nothing used goes to the purge first.
	t.Run("deletes ended sign-ins", func(t *testing.T) {
		ctx := context.Background()
		db := newTestStore(t)
		createWorkspace(t, ws)
		abandoned := newSignIn("SI-ABANDONED", ada)
		finishing(t, db, abandoned)
		require.NoError(t, db.SetConnectorSignInAccount(ctx, ws, abandoned.ID, "acct-orphan"))
		connected := newSignIn("SI-DONE", ada)
		finishing(t, db, connected)
		require.NoError(t, db.SetConnectorSignInAccount(ctx, ws, connected.ID, "acct-used"))
		now := time.Now()
		require.NoError(t, db.ConnectConnectorSignIn(ctx, model.ConnectorSignInConnect{WorkspaceID: ws, SignInID: connected.ID,
			New: &model.Connector{ID: id26("SI-USED"), WorkspaceID: ws, Kind: "github", Name: "GitHub", Config: connected.Config,
				SecretCiphertext: []byte("x"), SecretKeyID: "k1", NextSyncAt: &now,
				Account: &model.ConnectorAccount{ID: "acct-used", UserID: ada}}}))
		_, err := start(t, db, newSignIn("SI-FRESH", grace))
		require.NoError(t, err)

		n, err := db.DeleteEndedConnectorSignIns(ctx, 24*time.Hour, 10)
		require.NoError(t, err)
		assert.Zero(t, n, "none ended a day ago")
		_, err = testDB.Exec(`UPDATE foundry_connector_sign_ins SET expires_at=NOW(6) - INTERVAL 25 HOUR WHERE id IN (?, ?)`,
			abandoned.ID, connected.ID)
		require.NoError(t, err)
		n, err = db.DeleteEndedConnectorSignIns(ctx, 24*time.Hour, 10)
		require.NoError(t, err)
		assert.Equal(t, 2, n)
		assert.Equal(t, []string{"github/acct-orphan@" + ada}, purges(t), "the connector's account stays")
		_, err = db.GetConnectorSignIn(ctx, ws, connected.ID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		get(t, db, "SI-FRESH")
	})
}
