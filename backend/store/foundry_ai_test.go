package store_test

import (
	"context"
	"encoding/json"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store"
)

// aiWorkspace creates workspace WS-AI<tag> and returns its id.
func aiWorkspace(t *testing.T, db store.Store, tag string) string {
	t.Helper()
	ws := id26("WS-AI" + tag)
	createWorkspace(t, ws)
	return ws
}

// saveAIConfig saves config AI<tag> of ws, keyed with a sealed key, and
// returns its id.
func saveAIConfig(t *testing.T, db store.Store, ws, tag string) string {
	t.Helper()
	c := &model.FoundryAIConfig{
		ID: id26("AI" + tag), WorkspaceID: ws, Kind: "anthropic", Settings: json.RawMessage(`{"v":1}`),
		Secret: &model.FoundryAISecret{Ciphertext: []byte("sealed-" + tag), KeyID: "k1"}, SecretHint: ptr("sk-ant…" + tag),
		CreatedBy: id26("US-OWNER"),
	}
	require.NoError(t, db.CreateFoundryAIConfig(context.Background(), c, 10))
	return c.ID
}

// claimAIProbe claims the next probe as owner and checks it is want's.
func claimAIProbe(t *testing.T, db store.Store, owner, want string) *model.FoundryAIConfig {
	t.Helper()
	c, err := db.ClaimFoundryAIProbe(context.Background(), owner, time.Minute, 3)
	require.NoError(t, err)
	require.NotNil(t, c, "a probe waits")
	require.Equal(t, want, c.ID)
	return c
}

var aiPassed = model.FoundryAIProbeOutcome{Report: json.RawMessage(`{"checks":[{"role":"draft","outcome":"passed"}]}`)}

// activateAIConfig saves config AI<tag> and passes its probe.
func activateAIConfig(t *testing.T, db store.Store, ws, tag string) string {
	t.Helper()
	configID := saveAIConfig(t, db, ws, tag)
	claimAIProbe(t, db, "probe-1", configID)
	state, err := db.FinishFoundryAIProbe(context.Background(), configID, "probe-1", aiPassed)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAIProbePassed, state)
	return configID
}

// aiConfigRow re-reads a config straight from its table.
func aiConfigRow(t *testing.T, id string) (state string, keyed bool) {
	t.Helper()
	require.NoError(t, testDB.QueryRow(`SELECT probe_state, secret_ciphertext IS NOT NULL FROM foundry_ai_configs WHERE id=?`, id).
		Scan(&state, &keyed))
	return state, keyed
}

// expireAIProbe lets a running probe's lease run out by the database's clock.
func expireAIProbe(t *testing.T, id string) {
	t.Helper()
	_, err := testDB.Exec(`UPDATE foundry_ai_configs SET probe_until = NOW(6) - INTERVAL 1 SECOND WHERE id=?`, id)
	require.NoError(t, err)
}

// assertAIKeys checks the stored-key invariant for ws: a sealed key exists
// only on the active config and on at most one pending config.
func assertAIKeys(t *testing.T, db store.Store, ws string) {
	t.Helper()
	view, err := db.GetFoundryAIView(context.Background(), ws)
	require.NoError(t, err)
	rows, err := testDB.Query(`SELECT id, probe_state FROM foundry_ai_configs WHERE workspace_id=? AND secret_ciphertext IS NOT NULL`, ws)
	require.NoError(t, err)
	defer rows.Close()
	pending := 0
	for rows.Next() {
		var id, state string
		require.NoError(t, rows.Scan(&id, &state))
		switch {
		case view.Active != nil && id == view.Active.ID:
		case model.FoundryAIProbeState(state).Pending():
			pending++
		default:
			t.Fatalf("config %s (%s) holds a key but is neither the setting nor pending", id, state)
		}
	}
	require.NoError(t, rows.Err())
	require.LessOrEqual(t, pending, 1, "more than one pending config holds a key")
}

func TestSaveFoundryAIConfig(t *testing.T) {
	ctx := context.Background()

	// An owner's newest save is the one tested: the one before it, waiting or
	// in its probe, is superseded and drops its key.
	t.Run("supersedes the pending config and drops its key", func(t *testing.T) {
		db := newTestStore(t)
		ws := aiWorkspace(t, db, "S1")
		first := saveAIConfig(t, db, ws, "S1A")
		claimAIProbe(t, db, "probe-1", first)
		second := saveAIConfig(t, db, ws, "S1B")
		state, keyed := aiConfigRow(t, first)
		assert.Equal(t, "superseded", state)
		assert.False(t, keyed)
		view, err := db.GetFoundryAIView(ctx, ws)
		require.NoError(t, err)
		require.NotNil(t, view.Pending)
		assert.Equal(t, second, view.Pending.ID)
		assert.Equal(t, model.FoundryAIProbeQueued, view.Pending.ProbeState)
		require.NotNil(t, view.Pending.Secret)
		assert.Equal(t, []byte("sealed-S1B"), view.Pending.Secret.Ciphertext)
		assert.Equal(t, "k1", view.Pending.Secret.KeyID)
		assert.Equal(t, "sk-ant…S1B", *view.Pending.SecretHint)
		assert.Nil(t, view.Active, "nothing passed yet: the default")
		assertAIKeys(t, db, ws)
		// The superseded probe's worker lost it.
		_, err = db.FinishFoundryAIProbe(ctx, first, "probe-1", aiPassed)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	// Each save costs a probe on the workspace's key, so saves are capped per hour.
	t.Run("caps the saves of an hour", func(t *testing.T) {
		db := newTestStore(t)
		ws := aiWorkspace(t, db, "S2")
		for i, tag := range []string{"S2A", "S2B"} {
			c := &model.FoundryAIConfig{ID: id26("AI" + tag), WorkspaceID: ws, Kind: "anthropic", Settings: json.RawMessage(`{}`), CreatedBy: "u"}
			require.NoError(t, db.CreateFoundryAIConfig(ctx, c, 2), "save %d", i)
		}
		c := &model.FoundryAIConfig{ID: id26("AIS2C"), WorkspaceID: ws, Kind: "anthropic", Settings: json.RawMessage(`{}`), CreatedBy: "u"}
		assert.ErrorIs(t, db.CreateFoundryAIConfig(ctx, c, 2), store.ErrLimitExceeded)
		view, err := db.GetFoundryAIView(ctx, ws)
		require.NoError(t, err)
		assert.Equal(t, id26("AIS2B"), view.Pending.ID, "the refused save left the pending one")
	})

	// A config belongs to a workspace that exists.
	t.Run("refuses a workspace that is gone", func(t *testing.T) {
		db := newTestStore(t)
		c := &model.FoundryAIConfig{ID: id26("AIS3"), WorkspaceID: id26("WS-NONE"), Kind: "anthropic", Settings: json.RawMessage(`{}`), CreatedBy: "u"}
		assert.ErrorIs(t, db.CreateFoundryAIConfig(ctx, c, 10), store.ErrNotFound)
	})

	// Deleting a workspace drops its AI keys once, as it is marked deleting:
	// until it is gone no save, probe's end or default may write its setting
	// again, so no key comes back.
	t.Run("refuses a workspace being deleted", func(t *testing.T) {
		db := newTestStore(t)
		ws := aiWorkspace(t, db, "S4")
		probing := saveAIConfig(t, db, ws, "S4A")
		claimAIProbe(t, db, "probe-1", probing)
		setWorkspaceStatus(t, ws, "deleting")
		_, err := db.FinishFoundryAIProbe(ctx, probing, "probe-1", aiPassed)
		assert.ErrorIs(t, err, store.ErrNotFound)
		c := &model.FoundryAIConfig{ID: id26("AIS4B"), WorkspaceID: ws, Kind: "anthropic", Settings: json.RawMessage(`{}`), CreatedBy: "u",
			Secret: &model.FoundryAISecret{Ciphertext: []byte("sealed"), KeyID: "k1"}}
		assert.ErrorIs(t, db.CreateFoundryAIConfig(ctx, c, 10), store.ErrNotFound)
		assert.ErrorIs(t, db.ActivateFoundryAIDefault(ctx, ws, "u"), store.ErrNotFound)
		_, err = db.GetActiveFoundryAIConfig(ctx, ws)
		assert.ErrorIs(t, err, store.ErrNotFound, "nothing was activated")
		var configs int
		require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_ai_configs WHERE workspace_id=?`, ws).Scan(&configs))
		assert.Equal(t, 1, configs, "the refused save stored nothing")
	})
}

func TestClaimFoundryAIProbe(t *testing.T) {
	ctx := context.Background()

	// The probe loop takes configs in the order they were saved, with the
	// sealed key it opens to test them.
	t.Run("claims the oldest queued config with its key", func(t *testing.T) {
		db := newTestStore(t)
		first := saveAIConfig(t, db, aiWorkspace(t, db, "C1"), "C1")
		second := saveAIConfig(t, db, aiWorkspace(t, db, "C2"), "C2")
		c := claimAIProbe(t, db, "probe-1", first)
		assert.Equal(t, model.FoundryAIProbeRunning, c.ProbeState)
		assert.Equal(t, 1, c.ProbeAttempts)
		assert.Equal(t, "probe-1", *c.ProbeOwner)
		require.NotNil(t, c.Secret)
		assert.Equal(t, []byte("sealed-C1"), c.Secret.Ciphertext)
		claimAIProbe(t, db, "probe-2", second)
		none, err := db.ClaimFoundryAIProbe(ctx, "probe-3", time.Minute, 3)
		require.NoError(t, err)
		assert.Nil(t, none, "nothing else waits")
	})

	// A worker that died mid-probe loses it once its lease runs out, and
	// another takes it, counting the attempt.
	t.Run("reclaims a probe whose lease ran out", func(t *testing.T) {
		db := newTestStore(t)
		configID := saveAIConfig(t, db, aiWorkspace(t, db, "C3"), "C3")
		claimAIProbe(t, db, "probe-1", configID)
		none, err := db.ClaimFoundryAIProbe(ctx, "probe-2", time.Minute, 3)
		require.NoError(t, err)
		assert.Nil(t, none, "a live lease is kept")
		expireAIProbe(t, configID)
		c := claimAIProbe(t, db, "probe-2", configID)
		assert.Equal(t, 2, c.ProbeAttempts)
		_, err = db.FinishFoundryAIProbe(ctx, configID, "probe-1", aiPassed)
		assert.ErrorIs(t, err, store.ErrNotFound, "the first worker lost it")
	})

	// A probe that keeps losing its worker ends, unavailable, rather than
	// being tried forever on the workspace's key.
	t.Run("fails a probe whose lease ran out on its last attempt", func(t *testing.T) {
		db := newTestStore(t)
		ws := aiWorkspace(t, db, "C4")
		configID := saveAIConfig(t, db, ws, "C4")
		for attempt := 1; attempt <= 3; attempt++ {
			claimAIProbe(t, db, "probe-1", configID)
			expireAIProbe(t, configID)
		}
		none, err := db.ClaimFoundryAIProbe(ctx, "probe-2", time.Minute, 3)
		require.NoError(t, err)
		assert.Nil(t, none)
		view, err := db.GetFoundryAIView(ctx, ws)
		require.NoError(t, err)
		require.NotNil(t, view.LastFailed)
		assert.Equal(t, "unavailable", *view.LastFailed.ProbeFailure)
		assert.Equal(t, "none", *view.LastFailed.ProbeDetail)
		assert.Nil(t, view.LastFailed.Secret)
		assert.Nil(t, view.Pending)
	})

	// An engine that could not run the probe gives it back with the attempt
	// counted; after the last attempt the config fails, unavailable.
	t.Run("a released probe waits again until its last attempt", func(t *testing.T) {
		db := newTestStore(t)
		configID := saveAIConfig(t, db, aiWorkspace(t, db, "C5"), "C5")
		claimAIProbe(t, db, "probe-1", configID)
		assert.ErrorIs(t, db.ReleaseFoundryAIProbe(ctx, configID, "probe-2", 2), store.ErrNotFound, "another worker's probe")
		require.NoError(t, db.ReleaseFoundryAIProbe(ctx, configID, "probe-1", 2))
		state, keyed := aiConfigRow(t, configID)
		assert.Equal(t, "queued", state)
		assert.True(t, keyed)
		c, err := db.ClaimFoundryAIProbe(ctx, "probe-1", time.Minute, 2)
		require.NoError(t, err)
		require.Equal(t, 2, c.ProbeAttempts)
		require.NoError(t, db.ReleaseFoundryAIProbe(ctx, configID, "probe-1", 2))
		state, keyed = aiConfigRow(t, configID)
		assert.Equal(t, "failed", state)
		assert.False(t, keyed)
	})

	// A worker that stops mid-probe gives it back uncounted: deploys don't
	// use up a correct setting's attempts.
	t.Run("a returned probe keeps its attempts", func(t *testing.T) {
		db := newTestStore(t)
		configID := saveAIConfig(t, db, aiWorkspace(t, db, "C6"), "C6")
		for range 3 {
			claimAIProbe(t, db, "probe-1", configID)
			assert.ErrorIs(t, db.ReturnFoundryAIProbe(ctx, configID, "probe-2"), store.ErrNotFound, "another worker's probe")
			require.NoError(t, db.ReturnFoundryAIProbe(ctx, configID, "probe-1"))
		}
		state, keyed := aiConfigRow(t, configID)
		assert.Equal(t, "queued", state)
		assert.True(t, keyed)
		c, err := db.ClaimFoundryAIProbe(ctx, "probe-1", time.Minute, 1)
		require.NoError(t, err)
		require.NotNil(t, c, "the last attempt is still there")
		assert.Equal(t, 1, c.ProbeAttempts)
	})
}

func TestFinishFoundryAIProbe(t *testing.T) {
	ctx := context.Background()

	// A passing probe is what makes a config the setting; the config it
	// replaces will never be used again, so its key goes.
	t.Run("a pass makes the config the setting and drops the previous key", func(t *testing.T) {
		db := newTestStore(t)
		ws := aiWorkspace(t, db, "F1")
		first := activateAIConfig(t, db, ws, "F1A")
		active, err := db.GetActiveFoundryAIConfig(ctx, ws)
		require.NoError(t, err)
		assert.Equal(t, first, active.ID)
		assert.Equal(t, model.FoundryAIProbePassed, active.ProbeState)
		assert.JSONEq(t, string(aiPassed.Report), string(active.ProbeReport))
		require.NotNil(t, active.Secret, "the setting keeps its key")
		second := activateAIConfig(t, db, ws, "F1B")
		active, err = db.GetActiveFoundryAIConfig(ctx, ws)
		require.NoError(t, err)
		assert.Equal(t, second, active.ID)
		state, keyed := aiConfigRow(t, first)
		assert.Equal(t, "passed", state)
		assert.False(t, keyed)
		assertAIKeys(t, db, ws)
		var activations int
		require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_ai_activations WHERE workspace_id=? AND user_id=?`,
			ws, id26("US-OWNER")).Scan(&activations))
		assert.Equal(t, 2, activations, "each activation is the saver's")
	})

	// A failed test leaves the setting as it was, shows why, and keeps no
	// key it would never use.
	t.Run("a failure keeps the setting and shows why", func(t *testing.T) {
		db := newTestStore(t)
		ws := aiWorkspace(t, db, "F2")
		kept := activateAIConfig(t, db, ws, "F2A")
		tried := saveAIConfig(t, db, ws, "F2B")
		claimAIProbe(t, db, "probe-1", tried)
		state, err := db.FinishFoundryAIProbe(ctx, tried, "probe-1", model.FoundryAIProbeOutcome{
			Failure: &model.FoundryAIProbeFailure{Failure: "auth", Detail: "none"}, Report: json.RawMessage(`{"checks":[]}`),
		})
		require.NoError(t, err)
		assert.Equal(t, model.FoundryAIProbeFailed, state)
		view, err := db.GetFoundryAIView(ctx, ws)
		require.NoError(t, err)
		require.NotNil(t, view.Active)
		assert.Equal(t, kept, view.Active.ID)
		assert.NotNil(t, view.Active.Secret)
		require.NotNil(t, view.LastFailed)
		assert.Equal(t, tried, view.LastFailed.ID)
		assert.Equal(t, "auth", *view.LastFailed.ProbeFailure)
		assert.Nil(t, view.LastFailed.Secret)
		assert.Equal(t, "sk-ant…F2B", *view.LastFailed.SecretHint, "the hint still says which key failed")
		assert.Nil(t, view.Pending)
		assertAIKeys(t, db, ws)
		// Saving again hides the old failure behind the new attempt.
		saveAIConfig(t, db, ws, "F2C")
		view, err = db.GetFoundryAIView(ctx, ws)
		require.NoError(t, err)
		assert.Nil(t, view.LastFailed)
	})

	// The race rule: a probe that passes after the setting changed under it
	// does not override that change.
	t.Run("a pass after a newer activation is superseded", func(t *testing.T) {
		db := newTestStore(t)
		ws := aiWorkspace(t, db, "F3")
		configID := saveAIConfig(t, db, ws, "F3")
		claimAIProbe(t, db, "probe-1", configID)
		// An activation newer than the config, as another writer would leave.
		_, err := testDB.Exec(`INSERT INTO foundry_ai_activations (id, workspace_id, config_id, user_id, created_at)
VALUES (?, ?, NULL, 'u', NOW(6) + INTERVAL 1 SECOND)`, id26("ACT-F3"), ws)
		require.NoError(t, err)
		state, err := db.FinishFoundryAIProbe(ctx, configID, "probe-1", aiPassed)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryAIProbeSuperseded, state)
		row, keyed := aiConfigRow(t, configID)
		assert.Equal(t, "superseded", row)
		assert.False(t, keyed)
		_, err = db.GetActiveFoundryAIConfig(ctx, ws)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	// The race rule: only the workspace's newest config may become the
	// setting.
	t.Run("a pass of a config no longer the newest is superseded", func(t *testing.T) {
		db := newTestStore(t)
		ws := aiWorkspace(t, db, "F4")
		older := saveAIConfig(t, db, ws, "F4A")
		claimAIProbe(t, db, "probe-1", older)
		// A newer config, as another writer would leave, without superseding.
		_, err := testDB.Exec(`INSERT INTO foundry_ai_configs (id, workspace_id, kind, settings, probe_state, probe_failure, created_by, created_at)
VALUES (?, ?, 'anthropic', '{}', 'failed', 'auth', 'u', NOW(6) + INTERVAL 1 SECOND)`, id26("AIF4B"), ws)
		require.NoError(t, err)
		state, err := db.FinishFoundryAIProbe(ctx, older, "probe-1", aiPassed)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryAIProbeSuperseded, state)
	})
}

func TestActivateFoundryAIDefault(t *testing.T) {
	ctx := context.Background()

	// Going back to the deployment's default is immediate and leaves no
	// workspace key stored: neither the setting's nor a pending test's.
	t.Run("drops every key and supersedes the pending config", func(t *testing.T) {
		db := newTestStore(t)
		ws := aiWorkspace(t, db, "D1")
		active := activateAIConfig(t, db, ws, "D1A")
		pending := saveAIConfig(t, db, ws, "D1B")
		claimAIProbe(t, db, "probe-1", pending)
		require.NoError(t, db.ActivateFoundryAIDefault(ctx, ws, id26("US-OWNER")))
		_, err := db.GetActiveFoundryAIConfig(ctx, ws)
		assert.ErrorIs(t, err, store.ErrNotFound, "the default")
		for id, want := range map[string]string{active: "passed", pending: "superseded"} {
			state, keyed := aiConfigRow(t, id)
			assert.Equal(t, want, state, id)
			assert.False(t, keyed, id)
		}
		view, err := db.GetFoundryAIView(ctx, ws)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryAIView{}, view)
		_, err = db.FinishFoundryAIProbe(ctx, pending, "probe-1", aiPassed)
		assert.ErrorIs(t, err, store.ErrNotFound, "the pending probe lost its config")
	})

	// A workspace's setting changes only while the workspace exists.
	t.Run("refuses a workspace that is gone", func(t *testing.T) {
		db := newTestStore(t)
		assert.ErrorIs(t, db.ActivateFoundryAIDefault(ctx, id26("WS-NONE"), "u"), store.ErrNotFound)
	})

	// Deleting a workspace forgets its model keys with the rest of its
	// Foundry work.
	t.Run("deleting the workspace drops its keys", func(t *testing.T) {
		db := newTestStore(t)
		ws := aiWorkspace(t, db, "D3")
		active := activateAIConfig(t, db, ws, "D3A")
		pending := saveAIConfig(t, db, ws, "D3B")
		other := aiWorkspace(t, db, "D3X")
		theirs := saveAIConfig(t, db, other, "D3X")
		require.NoError(t, db.WithTx(ctx, func(tx store.Store) error { return tx.CancelWorkspaceFoundryWork(ctx, ws) }))
		for id, want := range map[string]string{active: "passed", pending: "superseded"} {
			state, keyed := aiConfigRow(t, id)
			assert.Equal(t, want, state, id)
			assert.False(t, keyed, id)
		}
		state, keyed := aiConfigRow(t, theirs)
		assert.Equal(t, "queued", state, "another workspace's config is left alone")
		assert.True(t, keyed)
	})
}

// Every way a workspace's setting moves keeps the stored-key invariant: a
// sealed key only on the setting's config and on at most one pending one.
func TestFoundryAIKeyInvariant(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	ws := aiWorkspace(t, db, "I1")
	steps := []struct {
		name string
		run  func(t *testing.T)
	}{
		{"save", func(t *testing.T) { saveAIConfig(t, db, ws, "I1A") }},
		{"save over a pending one", func(t *testing.T) { saveAIConfig(t, db, ws, "I1B") }},
		{"pass", func(t *testing.T) {
			claimAIProbe(t, db, "probe-1", id26("AII1B"))
			_, err := db.FinishFoundryAIProbe(ctx, id26("AII1B"), "probe-1", aiPassed)
			require.NoError(t, err)
		}},
		{"save beside the setting", func(t *testing.T) { saveAIConfig(t, db, ws, "I1C") }},
		{"fail", func(t *testing.T) {
			claimAIProbe(t, db, "probe-1", id26("AII1C"))
			_, err := db.FinishFoundryAIProbe(ctx, id26("AII1C"), "probe-1", model.FoundryAIProbeOutcome{
				Failure: &model.FoundryAIProbeFailure{Failure: "not_found", Detail: "none"},
			})
			require.NoError(t, err)
		}},
		{"replace the setting", func(t *testing.T) { activateAIConfig(t, db, ws, "I1D") }},
		{"default", func(t *testing.T) { require.NoError(t, db.ActivateFoundryAIDefault(ctx, ws, "u")) }},
		{"save after the default", func(t *testing.T) { saveAIConfig(t, db, ws, "I1E") }},
	}
	for _, step := range steps {
		step.run(t)
		t.Run("after "+step.name, func(t *testing.T) { assertAIKeys(t, db, ws) })
	}
}

// A custom OpenAI-compatible server may take no key: its config saves,
// passes and becomes the setting with no secret, and the keyed setting it
// replaces drops its key once it does, so no key outlives the endpoint it
// was given for.
func TestFoundryAIKeylessSetting(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	ws := aiWorkspace(t, db, "K1")
	keyed := activateAIConfig(t, db, ws, "K1A")
	server := &model.FoundryAIConfig{ID: id26("AIK1B"), WorkspaceID: ws, Kind: "openai_compatible", CreatedBy: id26("US-OWNER"),
		Settings: json.RawMessage(`{"v":1,"provider":{"kind":"openai_compatible","base_url":"http://host.docker.internal:1234/v1",` +
			`"api":"chat_completions","max_calls_in_flight":4}}`)}
	require.NoError(t, db.CreateFoundryAIConfig(ctx, server, 10))
	_, stillKeyed := aiConfigRow(t, keyed)
	assert.True(t, stillKeyed, "the setting keeps its key while the server is tested")
	assertAIKeys(t, db, ws)

	claimAIProbe(t, db, "probe-1", server.ID)
	state, err := db.FinishFoundryAIProbe(ctx, server.ID, "probe-1", aiPassed)
	require.NoError(t, err)
	require.Equal(t, model.FoundryAIProbePassed, state)
	active, err := db.GetActiveFoundryAIConfig(ctx, ws)
	require.NoError(t, err)
	assert.Equal(t, server.ID, active.ID)
	assert.Equal(t, "openai_compatible", active.Kind)
	assert.Nil(t, active.Secret)
	assert.Nil(t, active.SecretHint)
	assert.JSONEq(t, string(server.Settings), string(active.Settings))
	_, stillKeyed = aiConfigRow(t, keyed)
	assert.False(t, stillKeyed, "the replaced setting dropped its key")
	assertAIKeys(t, db, ws)
}
