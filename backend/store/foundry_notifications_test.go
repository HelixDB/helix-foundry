package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/sample"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	foundrynotifyworker "github.com/helixdb/helix-foundry/backend/workers/foundry-notify-worker"
)

// notificationID hands out FN-1, FN-2, …, across the tests, as id.New
// never repeats.
var notificationID = func() func() string {
	var mu sync.Mutex
	n := 0
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return id26(fmt.Sprintf("FN-%d", n))
	}
}()

// storedNotification is a row of foundry_notifications as discovery wrote it.
type storedNotification struct {
	Kind, SubjectID, WorkspaceID, UserID, Status string
	OntologyID, LastError                        *string
}

// notifications are the emails queued, oldest first.
func notifications(t *testing.T) []storedNotification {
	t.Helper()
	rows, err := testDB.Query(`SELECT kind, subject_id, workspace_id, user_id, status, ontology_id, last_error FROM foundry_notifications
ORDER BY created_at, id`)
	require.NoError(t, err)
	defer rows.Close()
	var out []storedNotification
	for rows.Next() {
		var n storedNotification
		var ontologyID, lastError sql.NullString
		require.NoError(t, rows.Scan(&n.Kind, &n.SubjectID, &n.WorkspaceID, &n.UserID, &n.Status, &ontologyID, &lastError))
		if ontologyID.Valid {
			n.OntologyID = &ontologyID.String
		}
		if lastError.Valid {
			n.LastError = &lastError.String
		}
		out = append(out, n)
	}
	require.NoError(t, rows.Err())
	return out
}

// finishedAgo backdates a generation's end.
func finishedAgo(t *testing.T, generationID string, ago time.Duration) {
	t.Helper()
	_, err := testDB.Exec(`UPDATE foundry_ontology_generations SET finished_at=NOW(6) - INTERVAL ? MICROSECOND WHERE id=?`,
		ago.Microseconds(), generationID)
	require.NoError(t, err)
}

func TestDiscoverFoundryOntologyReady(t *testing.T) {
	since := func() time.Time { return time.Now().Add(-time.Hour) }
	discover := func(t *testing.T, db store.Store, limit int) int {
		t.Helper()
		n, err := db.DiscoverFoundryOntologyReady(context.Background(), since(), limit, notificationID)
		require.NoError(t, err)
		return n
	}
	// ended stores f's first draft FDL-GEN-<tag> as outcome leaves it.
	ended := func(t *testing.T, f foundryFixture, tag string, outcome func(model.OntologyGenerationOutcome) model.OntologyGenerationOutcome) *model.OntologyGeneration {
		t.Helper()
		gen := startedGeneration(t, f.db, firstDraft(f, tag))
		require.NoError(t, f.db.FinishOntologyGeneration(context.Background(), gen.ID, "worker-1", outcome(firstDraftOutcome(f, gen, tag))))
		return gen
	}
	loads := func(o model.OntologyGenerationOutcome) model.OntologyGenerationOutcome { return o }

	// The email waits for the load: it tells how the first draft ended.
	t.Run("queues a first draft's email once its load ends, once", func(t *testing.T) {
		f := newFoundryFixture(t, "NR1")
		gen := ended(t, f, "1", loads)
		assert.Zero(t, discover(t, f.db, 100), "the load is still being written")
		f.loaded(t, "w1")
		assert.Equal(t, 1, discover(t, f.db, 100))
		assert.Equal(t, []storedNotification{{
			Kind: "ontology_ready", SubjectID: gen.ID, WorkspaceID: f.ontology.WorkspaceID, UserID: firstDraftUser, Status: "pending",
			OntologyID: &f.ontology.ID,
		}}, notifications(t))
		assert.Zero(t, discover(t, f.db, 100), "a generation has one email")
	})
	for name, end := range map[string]func(*testing.T, foundryFixture){
		"a load that ran out of budget": func(t *testing.T, f foundryFixture) {
			ended(t, f, "1", loads)
			f.finish(t, "w1", "p", model.FoundryGraphJobBudgetExhausted)
		},
		"a load that failed": func(t *testing.T, f foundryFixture) {
			ended(t, f, "1", loads)
			f.finish(t, "w1", "f", model.FoundryGraphJobFailed)
		},
		"a draft that failed": func(t *testing.T, f foundryFixture) {
			gen := startedGeneration(t, f.db, firstDraft(f, "1"))
			require.NoError(t, f.db.FinishOntologyGeneration(context.Background(), gen.ID, "worker-1", model.OntologyGenerationOutcome{
				Status: model.OntologyGenerationFailed, Error: "the generated ontology is not valid",
			}))
		},
		"a draft left for review": func(t *testing.T, f foundryFixture) {
			ended(t, f, "1", func(o model.OntologyGenerationOutcome) model.OntologyGenerationOutcome {
				o.Load = nil
				return o
			})
		},
		"a draft made current while a load was live": func(t *testing.T, f foundryFixture) {
			f.load(t, "LIVE")
			ended(t, f, "1", loads)
		},
	} {
		t.Run("queues the email of "+name, func(t *testing.T) {
			f := newFoundryFixture(t, "NR2")
			end(t, f)
			assert.Equal(t, 1, discover(t, f.db, 100))
			got := notifications(t)
			require.Len(t, got, 1)
			assert.Equal(t, id26("FDL-GEN-1"), got[0].SubjectID)
			assert.Equal(t, "pending", got[0].Status)
		})
	}
	// Only a first draft asked for by a person, that ended since, is emailed;
	// a load cancelled because its ontology was deleted never ends.
	for name, leave := range map[string]func(*testing.T, foundryFixture){
		"a draft without load_when_ready": func(t *testing.T, f foundryFixture) {
			gen := firstDraft(f, "1")
			gen.Input.LoadWhenReady = false
			gen = startedGeneration(t, f.db, gen)
			outcome := firstDraftOutcome(f, gen, "1")
			outcome.Load = nil
			require.NoError(t, f.db.FinishOntologyGeneration(context.Background(), gen.ID, "worker-1", outcome))
		},
		"a draft no person asked for": func(t *testing.T, f foundryFixture) {
			gen := firstDraft(f, "1")
			gen.RequestedByUserID = nil
			gen = startedGeneration(t, f.db, gen)
			outcome := firstDraftOutcome(f, gen, "1")
			outcome.Load = nil
			require.NoError(t, f.db.FinishOntologyGeneration(context.Background(), gen.ID, "worker-1", outcome))
		},
		"a draft that ended before the window": func(t *testing.T, f foundryFixture) {
			gen := ended(t, f, "1", func(o model.OntologyGenerationOutcome) model.OntologyGenerationOutcome {
				o.Load = nil
				return o
			})
			finishedAgo(t, gen.ID, 2*time.Hour)
		},
		"a draft still running": func(t *testing.T, f foundryFixture) {
			startedGeneration(t, f.db, firstDraft(f, "1"))
		},
		"a draft whose ontology was deleted while it loaded": func(t *testing.T, f foundryFixture) {
			ended(t, f, "1", loads)
			require.NoError(t, f.db.DeleteFoundryOntology(context.Background(), f.ontology.WorkspaceID, f.ontology.ID, id26("FNR-DEL")))
		},
	} {
		t.Run("leaves "+name, func(t *testing.T) {
			f := newFoundryFixture(t, "NR3")
			leave(t, f)
			assert.Zero(t, discover(t, f.db, 100))
			assert.Empty(t, notifications(t))
		})
	}
	t.Run("reads at most limit, oldest first", func(t *testing.T) {
		db := newTestStore(t)
		var gens []string
		for i, tag := range []string{"NR4A", "NR4B", "NR4C"} {
			f := foundryFixture{db: db, ontology: sampleOntology(t, db, id26("FWS-NR4"), tag)}
			gen := ended(t, f, tag, func(o model.OntologyGenerationOutcome) model.OntologyGenerationOutcome {
				o.Load = nil
				return o
			})
			finishedAgo(t, gen.ID, time.Duration(30-i)*time.Minute)
			gens = append(gens, gen.ID)
		}
		assert.Equal(t, 2, discover(t, db, 2))
		assert.Equal(t, 1, discover(t, db, 2))
		got := notifications(t)
		require.Len(t, got, 3)
		for i, n := range got {
			assert.Equal(t, gens[i], n.SubjectID)
		}
	})
}

// sampleRun is a workspace FNS-WS-<tag> whose ontology ran the sample for
// user: from connector FNS-C-<tag> and set FNS-S-<tag>, which holds one
// document, it finished ago. The ontology also has its General set, empty.
type sampleRun struct {
	f          foundryFixture
	generation string
}

func newSampleRun(t *testing.T, db store.Store, tag, user string, ago time.Duration) sampleRun {
	t.Helper()
	r := sampleRun{f: foundryFixture{db: db, ontology: sampleOntology(t, db, id26("FNS-WS-"+tag), "FNS-"+tag)}}
	r.generation = r.run(t, tag, user, ago).ID
	return r
}

// run has the sample drafted again in r's ontology, for user, from a
// connector and set of its own (FNS-C-<tag>, FNS-S-<tag>, made if new).
func (r sampleRun) run(t *testing.T, tag, user string, ago time.Duration) *model.OntologyGeneration {
	t.Helper()
	ctx := context.Background()
	ws, db := r.f.ontology.WorkspaceID, r.f.db
	connector, setID := id26("FNS-C-"+tag), id26("FNS-S-"+tag)
	if _, err := db.GetConnector(ctx, ws, connector); err != nil {
		require.NoError(t, db.CreateConnector(ctx, &model.Connector{ID: connector, WorkspaceID: ws, Kind: "postgres", Name: "Acme Corp " + tag,
			Config: json.RawMessage(`{}`), SecretCiphertext: []byte{1}, SecretKeyID: "k1", SecretHint: "…"}))
		require.NoError(t, db.CreateFoundryDocumentSet(ctx, &model.FoundryDocumentSet{ID: setID, WorkspaceID: ws, Name: "Acme Corp sample " + tag}, 1000))
		doc := foundryDocument(r.f.ontology, "FNS-"+tag)
		doc.SetID = setID
		_, _, err := db.CreateFoundryDocument(ctx, doc, noCap)
		require.NoError(t, err)
		_, _, err = db.CompleteFoundryDocumentUpload(ctx, ws, doc.ID, r.f.fan("FNS-"+tag))
		require.NoError(t, err)
	}
	gen := startedGeneration(t, db, &model.OntologyGeneration{ID: id26("FNS-G-" + tag), WorkspaceID: ws, OntologyID: r.f.ontology.ID,
		RequestedByUserID: &user, Input: model.OntologyGenerationInput{Sample: sample.Acme, DocumentSetIDs: []string{setID},
			Tables: []model.OntologyGenerationTable{{ConnectorID: connector, Table: "accounts", SyncID: id26("FNS-SYNC")}}}})
	require.NoError(t, db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", model.OntologyGenerationOutcome{
		Status: model.OntologyGenerationSucceeded, Result: &model.OntologyVersion{
			ID: id26("FNS-V-" + tag), WorkspaceID: ws, OntologyID: r.f.ontology.ID, Source: model.OntologySourceGenerated,
			Format: model.OntologyFormatAgentBundle, GenerationID: &gen.ID, Definition: json.RawMessage(`{"format":"agent_bundle"}`),
			DocumentSetIDs: &[]string{setID},
		},
	}))
	finishedAgo(t, gen.ID, ago)
	return gen
}

func TestDiscoverFoundrySampleFollowUps(t *testing.T) {
	ctx := context.Background()
	ada, alan := id26("FNS-ADA"), id26("FNS-ALAN")
	day := 24 * time.Hour
	discover := func(t *testing.T, db store.Store, limit int) int {
		t.Helper()
		now := time.Now()
		n, err := db.DiscoverFoundrySampleFollowUps(ctx, now.Add(-3*day), now.Add(-day), limit, notificationID)
		require.NoError(t, err)
		return n
	}
	owned := "the workspace has data of its own"

	t.Run("asks a person whose workspace has nothing of their own, once ever", func(t *testing.T) {
		db := newTestStore(t)
		r := newSampleRun(t, db, "1", ada, 30*time.Hour)
		assert.Equal(t, 1, discover(t, db, 100))
		assert.Equal(t, []storedNotification{{
			Kind: "sample_follow_up", SubjectID: ada, WorkspaceID: r.f.ontology.WorkspaceID, UserID: ada, Status: "pending",
		}}, notifications(t))
		assert.Zero(t, discover(t, db, 100))
		newSampleRun(t, db, "2", ada, 30*time.Hour)
		assert.Zero(t, discover(t, db, 100), "another run of the same person's asks nothing more")
		assert.Len(t, notifications(t), 1)
	})
	// A day after the run, and never for a run older than the window, so a
	// first deploy asks no one about old runs.
	t.Run("asks only about runs that ended in the window", func(t *testing.T) {
		db := newTestStore(t)
		newSampleRun(t, db, "1", ada, 10*time.Hour)
		newSampleRun(t, db, "2", alan, 80*time.Hour)
		assert.Zero(t, discover(t, db, 100))
		assert.Empty(t, notifications(t))
	})
	// A person who has brought data of their own has tried Foundry on it:
	// they are never asked, and their run is not read again.
	for name, own := range map[string]func(*testing.T, sampleRun){
		"an ontology no sample drafted": func(t *testing.T, r sampleRun) {
			sampleOntology(t, r.f.db, r.f.ontology.WorkspaceID, "FNS-OWN")
		},
		"a connector no sample drafted from": func(t *testing.T, r sampleRun) {
			require.NoError(t, r.f.db.CreateConnector(ctx, &model.Connector{ID: id26("FNS-OWN-C"), WorkspaceID: r.f.ontology.WorkspaceID,
				Kind: "stripe", Name: "Billing", Config: json.RawMessage(`{}`), SecretCiphertext: []byte{1}, SecretKeyID: "k1", SecretHint: "…"}))
		},
		"an upload outside the sample's set": func(t *testing.T, r sampleRun) {
			r.f.uploaded(t, "FNS-OWN")
		},
	} {
		t.Run("skips a person whose workspace has "+name, func(t *testing.T) {
			db := newTestStore(t)
			r := newSampleRun(t, db, "1", ada, 30*time.Hour)
			own(t, r)
			assert.Equal(t, 1, discover(t, db, 100))
			got := notifications(t)
			require.Len(t, got, 1)
			assert.Equal(t, "skipped", got[0].Status)
			require.NotNil(t, got[0].LastError)
			assert.Equal(t, owned, *got[0].LastError)
			assert.Zero(t, discover(t, db, 100))
		})
	}
	// What is deleted is no data of the workspace's.
	t.Run("asks a person whose own data is deleted", func(t *testing.T) {
		db := newTestStore(t)
		r := newSampleRun(t, db, "1", ada, 30*time.Hour)
		ws := r.f.ontology.WorkspaceID
		other := sampleOntology(t, db, ws, "FNS-OWN")
		require.NoError(t, db.DeleteFoundryOntology(ctx, ws, other.ID, id26("FNS-DEL")))
		require.NoError(t, db.CreateConnector(ctx, &model.Connector{ID: id26("FNS-OWN-C"), WorkspaceID: ws, Kind: "stripe", Name: "Billing",
			Config: json.RawMessage(`{}`), SecretCiphertext: []byte{1}, SecretKeyID: "k1", SecretHint: "…"}))
		require.NoError(t, db.DeleteConnector(ctx, ws, id26("FNS-OWN-C")))
		require.NoError(t, db.DeleteFoundryDocument(ctx, ws, r.f.uploaded(t, "FNS-OWN")))
		assert.Equal(t, 1, discover(t, db, 100))
		got := notifications(t)
		require.Len(t, got, 1)
		assert.Equal(t, "pending", got[0].Status)
	})
	// Every sample run's ontology, connector and set are the sample's: a
	// second run, or another person's, is no data of anyone's own.
	t.Run("asks each person who ran the sample in a workspace", func(t *testing.T) {
		db := newTestStore(t)
		r := newSampleRun(t, db, "1", ada, 30*time.Hour)
		second := sampleRun{f: foundryFixture{db: db, ontology: sampleOntology(t, db, r.f.ontology.WorkspaceID, "FNS-2")}}
		second.run(t, "2", alan, 29*time.Hour)
		assert.Equal(t, 2, discover(t, db, 100))
		got := notifications(t)
		require.Len(t, got, 2)
		for _, n := range got {
			assert.Equal(t, "pending", n.Status)
		}
		assert.ElementsMatch(t, []string{ada, alan}, []string{got[0].SubjectID, got[1].SubjectID})
	})
	t.Run("asks about the run whose workspace has nothing of their own", func(t *testing.T) {
		db := newTestStore(t)
		busy := newSampleRun(t, db, "1", ada, 40*time.Hour)
		busy.f.uploaded(t, "FNS-OWN")
		quiet := newSampleRun(t, db, "2", ada, 30*time.Hour)
		assert.Equal(t, 1, discover(t, db, 100))
		got := notifications(t)
		require.Len(t, got, 1)
		assert.Equal(t, "pending", got[0].Status)
		assert.Equal(t, quiet.f.ontology.WorkspaceID, got[0].WorkspaceID)
	})
	for name, run := range map[string]func(*testing.T, store.Store){
		"a sample run that failed": func(t *testing.T, db store.Store) {
			o := sampleOntology(t, db, id26("FNS-WS-F"), "FNS-F")
			gen := startedGeneration(t, db, &model.OntologyGeneration{ID: id26("FNS-G-F"), WorkspaceID: o.WorkspaceID, OntologyID: o.ID,
				RequestedByUserID: &ada, Input: model.OntologyGenerationInput{Sample: sample.Acme}})
			require.NoError(t, db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", model.OntologyGenerationOutcome{
				Status: model.OntologyGenerationFailed, Error: "the sample's database is missing a table: add the sample again"}))
			finishedAgo(t, gen.ID, 30*time.Hour)
		},
		"an AI draft": func(t *testing.T, db store.Store) {
			f := foundryFixture{db: db, ontology: sampleOntology(t, db, id26("FNS-WS-A"), "FNS-A")}
			gen := firstDraft(f, "A")
			gen.Input.LoadWhenReady = false
			gen = startedGeneration(t, db, gen)
			outcome := firstDraftOutcome(f, gen, "A")
			outcome.Load = nil
			require.NoError(t, db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", outcome))
			finishedAgo(t, gen.ID, 30*time.Hour)
		},
	} {
		t.Run("leaves "+name, func(t *testing.T) {
			db := newTestStore(t)
			run(t, db)
			assert.Zero(t, discover(t, db, 100))
			assert.Empty(t, notifications(t))
		})
	}
	t.Run("leaves a run no person asked for", func(t *testing.T) {
		db := newTestStore(t)
		r := newSampleRun(t, db, "1", ada, 30*time.Hour)
		_, err := testDB.Exec(`UPDATE foundry_ontology_generations SET requested_by_user_id=NULL WHERE id=?`, r.generation)
		require.NoError(t, err)
		assert.Zero(t, discover(t, db, 100))
	})
	t.Run("reads at most limit runs, oldest first", func(t *testing.T) {
		db := newTestStore(t)
		newSampleRun(t, db, "1", ada, 30*time.Hour)
		newSampleRun(t, db, "2", alan, 40*time.Hour)
		assert.Equal(t, 1, discover(t, db, 1))
		got := notifications(t)
		require.Len(t, got, 1)
		assert.Equal(t, alan, got[0].SubjectID, "the older run first")
		assert.Equal(t, 1, discover(t, db, 1))
	})
}

// pendingNotification queues a follow-up for user by hand, due now.
func pendingNotification(t *testing.T, id, user string) {
	t.Helper()
	_, err := testDB.Exec(`INSERT INTO foundry_notifications (id, kind, subject_id, workspace_id, user_id, next_attempt_at)
VALUES (?, 'sample_follow_up', ?, ?, ?, NOW(6) - INTERVAL 1 SECOND)`, id, user, id26("FNC-WS"), user)
	require.NoError(t, err)
}

func TestFoundryNotificationClaims(t *testing.T) {
	ctx := context.Background()
	claim := func(t *testing.T, db store.Store, token string, lease time.Duration) *model.FoundryNotification {
		t.Helper()
		n, err := db.ClaimFoundryNotification(ctx, token, lease)
		require.NoError(t, err)
		return n
	}

	t.Run("claims nothing when nothing is due", func(t *testing.T) {
		db := newTestStore(t)
		assert.Nil(t, claim(t, db, "t1", time.Minute))
		_, err := testDB.Exec(`INSERT INTO foundry_notifications (id, kind, subject_id, workspace_id, user_id, next_attempt_at)
VALUES (?, 'sample_follow_up', ?, ?, ?, NOW(6) + INTERVAL 1 HOUR)`, id26("FNC-LATER"), id26("FNC-U"), id26("FNC-WS"), id26("FNC-U"))
		require.NoError(t, err)
		assert.Nil(t, claim(t, db, "t1", time.Minute), "a retry waits for its time")
	})
	// A claim holds the email for its lease; one that lapsed (its worker
	// stopped) is claimed again, counting another attempt, and the old token
	// ends nothing.
	t.Run("holds an email for its lease, then lets it go", func(t *testing.T) {
		db := newTestStore(t)
		pendingNotification(t, id26("FNC-1"), id26("FNC-U"))
		first := claim(t, db, "t1", 200*time.Millisecond)
		require.NotNil(t, first)
		assert.Equal(t, id26("FNC-1"), first.ID)
		assert.Equal(t, model.FoundryNotifySampleFollowUp, first.Kind)
		assert.Equal(t, model.FoundryNotificationPending, first.Status)
		assert.Equal(t, 1, first.Attempts)
		require.NotNil(t, first.ClaimToken)
		assert.Equal(t, "t1", *first.ClaimToken)
		require.NotNil(t, first.ClaimedUntil)
		assert.Nil(t, first.OntologyID)
		assert.Nil(t, claim(t, db, "t2", time.Minute), "a held email is not claimed twice")

		time.Sleep(300 * time.Millisecond)
		again := claim(t, db, "t2", time.Minute)
		require.NotNil(t, again)
		assert.Equal(t, 2, again.Attempts)
		assert.ErrorIs(t, db.FinishFoundryNotification(ctx, first.ID, "t1", model.FoundryNotificationDelivered{}), store.ErrNotFound,
			"the lapsed claim ends nothing")
		require.NoError(t, db.FinishFoundryNotification(ctx, again.ID, "t2", model.FoundryNotificationDelivered{}))
	})
	// Sent is final: an email is never claimed, or sent, again.
	t.Run("never claims a sent email again", func(t *testing.T) {
		db := newTestStore(t)
		pendingNotification(t, id26("FNC-1"), id26("FNC-U"))
		n := claim(t, db, "t1", time.Minute)
		require.NotNil(t, n)
		require.NoError(t, db.FinishFoundryNotification(ctx, n.ID, "t1", model.FoundryNotificationDelivered{}))
		var status string
		var sentAt sql.NullTime
		var token sql.NullString
		require.NoError(t, testDB.QueryRow(`SELECT status, sent_at, claim_token FROM foundry_notifications WHERE id=?`, n.ID).Scan(&status, &sentAt, &token))
		assert.Equal(t, "sent", status)
		assert.True(t, sentAt.Valid)
		assert.False(t, token.Valid, "the claim ends with it")
		_, err := testDB.Exec(`UPDATE foundry_notifications SET next_attempt_at=NOW(6) - INTERVAL 1 HOUR WHERE id=?`, n.ID)
		require.NoError(t, err)
		assert.Nil(t, claim(t, db, "t2", time.Minute))
		assert.ErrorIs(t, db.FinishFoundryNotification(ctx, n.ID, "t1", model.FoundryNotificationDelivered{}), store.ErrNotFound)
	})
	t.Run("retries an email at its time, then gives up or skips it", func(t *testing.T) {
		db := newTestStore(t)
		pendingNotification(t, id26("FNC-1"), id26("FNC-U1"))
		pendingNotification(t, id26("FNC-2"), id26("FNC-U2"))
		n := claim(t, db, "t1", time.Minute)
		require.NotNil(t, n)
		require.NoError(t, db.FinishFoundryNotification(ctx, n.ID, "t1", model.FoundryNotificationRetry{At: time.Now().Add(time.Hour), Error: "mail is down"}))
		other := claim(t, db, "t2", time.Minute)
		require.NotNil(t, other)
		assert.NotEqual(t, n.ID, other.ID, "the retried email waits for its time")
		require.NoError(t, db.FinishFoundryNotification(ctx, other.ID, "t2", model.FoundryNotificationSkip{Reason: "the user is gone"}))
		assert.Nil(t, claim(t, db, "t3", time.Minute))

		_, err := testDB.Exec(`UPDATE foundry_notifications SET next_attempt_at=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, n.ID)
		require.NoError(t, err)
		retried := claim(t, db, "t3", time.Minute)
		require.NotNil(t, retried)
		assert.Equal(t, n.ID, retried.ID)
		assert.Equal(t, 2, retried.Attempts)
		require.NotNil(t, retried.LastError)
		assert.Equal(t, "mail is down", *retried.LastError)
		require.NoError(t, db.FinishFoundryNotification(ctx, retried.ID, "t3", model.FoundryNotificationGiveUp{Error: "mail is still down"}))
		assert.Nil(t, claim(t, db, "t4", time.Minute))
		statuses := map[string]string{}
		for _, row := range notifications(t) {
			statuses[row.SubjectID] = row.Status + ": " + *row.LastError
		}
		assert.Equal(t, map[string]string{id26("FNC-U1"): "failed: mail is still down", id26("FNC-U2"): "skipped: the user is gone"}, statuses)
	})
	t.Run("refuses an outcome without its reason", func(t *testing.T) {
		db := newTestStore(t)
		pendingNotification(t, id26("FNC-1"), id26("FNC-U"))
		n := claim(t, db, "t1", time.Minute)
		require.NotNil(t, n)
		for _, outcome := range []model.FoundryNotificationOutcome{
			model.FoundryNotificationSkip{}, model.FoundryNotificationRetry{At: time.Now()}, model.FoundryNotificationGiveUp{}, nil,
		} {
			assert.Error(t, db.FinishFoundryNotification(ctx, n.ID, "t1", outcome), "%T", outcome)
		}
		require.NoError(t, db.FinishFoundryNotification(ctx, n.ID, "t1", model.FoundryNotificationDelivered{}), "the claim still holds")
	})
	// Workers claiming at once each get their own email, and every email
	// is claimed once.
	t.Run("hands each email to one worker", func(t *testing.T) {
		db := newTestStore(t)
		const emails, workers = 12, 6
		for i := range emails {
			pendingNotification(t, id26(fmt.Sprintf("FNC-%02d", i)), id26(fmt.Sprintf("FNC-U%02d", i)))
		}
		var mu sync.Mutex
		claimed := map[string]int{}
		var wg sync.WaitGroup
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					n, err := db.ClaimFoundryNotification(ctx, fmt.Sprintf("w%d", w), time.Minute)
					if !assert.NoError(t, err) || n == nil {
						return
					}
					mu.Lock()
					claimed[n.ID]++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		assert.Len(t, claimed, emails)
		for id, n := range claimed {
			assert.Equal(t, 1, n, id)
		}
	})
}

// replicaMail is the host's mail for TestFoundryNotifyWorkerReplicas: every
// person has an address, and it counts each email sent.
type replicaMail struct {
	mu   sync.Mutex
	sent map[string]int
}

func (m *replicaMail) Recipient(_ context.Context, userID string) (ports.Recipient, error) {
	return ports.Recipient{Email: userID + "@example.com", FirstName: "Ada"}, nil
}

func (m *replicaMail) Send(_ context.Context, to ports.Recipient, e ports.Email) error {
	time.Sleep(20 * time.Millisecond) // long enough for the other replica to claim meanwhile
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent[to.Email+" "+e.Subject]++
	return nil
}

// The worker runs in a role with replicas: two of them finding and sending
// the same emails at once queue each email once and send it once.
func TestFoundryNotifyWorkerReplicas(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	for _, tag := range []string{"NRA", "NRB", "NRC", "NRD"} {
		f := foundryFixture{db: db, ontology: sampleOntology(t, db, id26("FWS-NREP"), tag)}
		gen := startedGeneration(t, db, firstDraft(f, tag))
		outcome := firstDraftOutcome(f, gen, tag)
		outcome.Load = nil
		require.NoError(t, db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", outcome))
	}
	newSampleRun(t, db, "REP", id26("FNS-ADA"), 30*time.Hour)

	cfg := config.Defaults()
	cfg.Foundry.Notify.SiteURL, cfg.Foundry.Notify.PollInterval = "https://foundry.example.com", 50*time.Millisecond
	mail := &replicaMail{sent: map[string]int{}}
	var replicas []*foundrynotifyworker.Task
	for range 2 {
		task, err := foundrynotifyworker.New(foundrynotifyworker.Deps{Cfg: &cfg, DB: db, Mail: mail})
		require.NoError(t, err)
		replicas = append(replicas, task)
	}
	var wg sync.WaitGroup
	for _, task := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(t, task.Start(ctx))
		}()
	}
	sent := func() int {
		var n int
		require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_notifications WHERE status='sent'`).Scan(&n))
		return n
	}
	require.Eventually(t, func() bool { return sent() == 5 }, 20*time.Second, 50*time.Millisecond)
	time.Sleep(300 * time.Millisecond) // a few more ticks of each replica
	for _, task := range replicas {
		require.NoError(t, task.Stop(ctx))
	}
	wg.Wait()

	assert.Len(t, notifications(t), 5, "one email per subject")
	mail.mu.Lock()
	defer mail.mu.Unlock()
	assert.Len(t, mail.sent, 5)
	for email, n := range mail.sent {
		assert.Equal(t, 1, n, "%s sent %d times", email, n)
	}
}

// Replicas find the same emails at once: each is queued once, by one of
// them, and neither fails.
func TestDiscoverFoundryNotificationsConcurrently(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	for _, tag := range []string{"NCA", "NCB", "NCC"} {
		f := foundryFixture{db: db, ontology: sampleOntology(t, db, id26("FWS-NCON"), tag)}
		gen := startedGeneration(t, db, firstDraft(f, tag))
		outcome := firstDraftOutcome(f, gen, tag)
		outcome.Load = nil
		require.NoError(t, db.FinishOntologyGeneration(ctx, gen.ID, "worker-1", outcome))
	}
	newSampleRun(t, db, "CON1", id26("FNS-ADA"), 30*time.Hour)
	newSampleRun(t, db, "CON2", id26("FNS-ALAN"), 30*time.Hour)
	now := time.Now()
	var mu sync.Mutex
	queued := 0
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready, err := db.DiscoverFoundryOntologyReady(ctx, now.Add(-time.Hour), 100, notificationID)
			assert.NoError(t, err)
			followUps, err := db.DiscoverFoundrySampleFollowUps(ctx, now.Add(-72*time.Hour), now.Add(-24*time.Hour), 100, notificationID)
			assert.NoError(t, err)
			mu.Lock()
			queued += ready + followUps
			mu.Unlock()
		}()
	}
	wg.Wait()
	assert.Equal(t, 5, queued, "each email is counted by the one that queued it")
	assert.Len(t, notifications(t), 5)
}
