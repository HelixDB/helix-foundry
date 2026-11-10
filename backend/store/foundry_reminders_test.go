package store_test

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/reminder"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	foundrynotifyworker "github.com/helixdb/helix-foundry/backend/workers/foundry-notify-worker"
)

// storedReminder is a row of foundry_reminders, as the tests read it.
type storedReminder struct {
	Status, TimeZone, Consent   string
	Sent, Attempts              int
	Next, LastSent, Stopped     sql.NullTime
	StopReason, LastError, Hold sql.NullString
	RequestedAt                 time.Time
}

func readReminder(t *testing.T, email string) storedReminder {
	t.Helper()
	var r storedReminder
	require.NoError(t, testDB.QueryRow(`SELECT status, time_zone, consent, sent, attempts, next_send_at, last_sent_at, stopped_at, stop_reason,
  last_error, claim_token, requested_at FROM foundry_reminders WHERE email=?`, email).Scan(&r.Status, &r.TimeZone, &r.Consent, &r.Sent,
		&r.Attempts, &r.Next, &r.LastSent, &r.Stopped, &r.StopReason, &r.LastError, &r.Hold, &r.RequestedAt))
	return r
}

// reminderRequest asks for reminders at email in zone, the first at first
// unless the address had one before (then a week after it), recording the
// last reminder each schedule was asked with.
func reminderRequest(email, zone string, first time.Time, cap int, asked *[]*time.Time) model.FoundryReminderRequest {
	return model.FoundryReminderRequest{
		ID: id26("RMD-" + email), Email: email, TimeZone: zone, Consent: reminder.Consent, MaxNewPerHour: cap,
		Schedule: func(last *time.Time) time.Time {
			if asked != nil {
				*asked = append(*asked, last)
			}
			if last != nil {
				return last.Add(7 * 24 * time.Hour)
			}
			return first
		},
	}
}

func TestRequestFoundryReminder(t *testing.T) {
	ctx := context.Background()
	first := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

	t.Run("schedules a new address, then answers it as it is", func(t *testing.T) {
		db := newTestStore(t)
		var asked []*time.Time
		answer, err := db.RequestFoundryReminder(ctx, reminderRequest("ada@example.com", "Europe/London", first, 300, &asked))
		require.NoError(t, err)
		assert.Equal(t, model.FoundryReminderAnswer{NextSendAt: first}, answer)
		assert.Equal(t, []*time.Time{nil}, asked, "a new address had no reminder before")
		r := readReminder(t, "ada@example.com")
		assert.Equal(t, "scheduled", r.Status)
		assert.Equal(t, "Europe/London", r.TimeZone)
		assert.Equal(t, reminder.Consent, r.Consent)
		assert.True(t, r.Next.Valid && r.Next.Time.Equal(first))

		again, err := db.RequestFoundryReminder(ctx, reminderRequest("ada@example.com", "America/New_York", first.Add(time.Hour), 300, &asked))
		require.NoError(t, err)
		assert.Equal(t, model.FoundryReminderAnswer{NextSendAt: first, Again: true}, again)
		assert.Len(t, asked, 1, "a schedule running isn't scheduled again")
		assert.Equal(t, "Europe/London", readReminder(t, "ada@example.com").TimeZone, "nor moved to another zone")
	})
	t.Run("caps the schedules started in an hour", func(t *testing.T) {
		db := newTestStore(t)
		for _, email := range []string{"ada@example.com", "grace@example.com"} {
			_, err := db.RequestFoundryReminder(ctx, reminderRequest(email, "UTC", first, 2, nil))
			require.NoError(t, err)
		}
		_, err := db.RequestFoundryReminder(ctx, reminderRequest("alan@example.com", "UTC", first, 2, nil))
		require.ErrorIs(t, err, store.ErrFoundryReminderLimit)
		var rows int
		require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_reminders`).Scan(&rows))
		assert.Equal(t, 2, rows, "nothing is stored past the cap")
		again, err := db.RequestFoundryReminder(ctx, reminderRequest("ada@example.com", "UTC", first, 2, nil))
		require.NoError(t, err, "asking again counts nothing")
		assert.True(t, again.Again)

		_, err = testDB.Exec(`UPDATE foundry_reminders SET requested_at=NOW(6) - INTERVAL 61 MINUTE WHERE email='ada@example.com'`)
		require.NoError(t, err)
		_, err = db.RequestFoundryReminder(ctx, reminderRequest("alan@example.com", "UTC", first, 2, nil))
		require.NoError(t, err, "an hour on, the schedule asked for then no longer counts")
	})
	t.Run("starts a schedule that ended again, no sooner than its last reminder allows", func(t *testing.T) {
		db := newTestStore(t)
		_, err := db.RequestFoundryReminder(ctx, reminderRequest("ada@example.com", "Europe/London", first, 300, nil))
		require.NoError(t, err)
		lastSent := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
		_, err = testDB.Exec(`UPDATE foundry_reminders SET status='done', sent=4, next_send_at=NULL, last_sent_at=?, attempts=2, last_error='x',
  requested_at=NOW(6) - INTERVAL 30 DAY WHERE email='ada@example.com'`, lastSent)
		require.NoError(t, err)

		var asked []*time.Time
		answer, err := db.RequestFoundryReminder(ctx, reminderRequest("ada@example.com", "America/New_York", first, 300, &asked))
		require.NoError(t, err)
		require.Len(t, asked, 1)
		require.NotNil(t, asked[0])
		assert.True(t, asked[0].Equal(lastSent), "the schedule is given the last reminder, %v", asked[0])
		assert.Equal(t, model.FoundryReminderAnswer{NextSendAt: lastSent.Add(7 * 24 * time.Hour)}, answer)
		r := readReminder(t, "ada@example.com")
		assert.Equal(t, "scheduled", r.Status)
		assert.Equal(t, 0, r.Sent)
		assert.Equal(t, 0, r.Attempts)
		assert.False(t, r.LastError.Valid)
		assert.Equal(t, "America/New_York", r.TimeZone, "in the zone it asks from now")
		assert.True(t, r.LastSent.Valid, "its last reminder is kept, for the next gap")
		assert.WithinDuration(t, time.Now(), r.RequestedAt, time.Minute)
	})
	// Replicas of the API take the same address at once: it is stored once,
	// one request starts its schedule, and every other answers it.
	t.Run("takes one address asked for at once", func(t *testing.T) {
		db := newTestStore(t)
		const n = 8
		answers := make([]model.FoundryReminderAnswer, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := reminderRequest("ada@example.com", "UTC", first, 300, nil)
				req.ID = id26(fmt.Sprintf("RMD-RACE-%d", i))
				answers[i], errs[i] = db.RequestFoundryReminder(ctx, req)
			}()
		}
		wg.Wait()
		started := 0
		for i := range n {
			require.NoError(t, errs[i])
			assert.True(t, answers[i].NextSendAt.Equal(first))
			if !answers[i].Again {
				started++
			}
		}
		assert.Equal(t, 1, started)
		var rows int
		require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_reminders`).Scan(&rows))
		assert.Equal(t, 1, rows)
	})
}

func TestStopFoundryReminder(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	first := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	_, err := db.RequestFoundryReminder(ctx, reminderRequest("ada@example.com", "UTC", first, 300, nil))
	require.NoError(t, err)

	stopped, err := db.StopFoundryReminder(ctx, "ada@example.com", model.FoundryReminderStoppedSignedIn)
	require.NoError(t, err)
	assert.True(t, stopped)
	r := readReminder(t, "ada@example.com")
	assert.Equal(t, "stopped", r.Status)
	assert.Equal(t, "signed_in", r.StopReason.String)
	assert.True(t, r.Stopped.Valid)
	assert.False(t, r.Next.Valid)

	for _, email := range []string{"ada@example.com", "nobody@example.com"} {
		stopped, err = db.StopFoundryReminder(ctx, email, model.FoundryReminderStoppedSignedIn)
		require.NoError(t, err)
		assert.False(t, stopped, "%s has no schedule running", email)
	}
	// Asked for again, a stopped address starts a schedule anew.
	answer, err := db.RequestFoundryReminder(ctx, reminderRequest("ada@example.com", "UTC", first, 300, nil))
	require.NoError(t, err)
	assert.False(t, answer.Again)
	r = readReminder(t, "ada@example.com")
	assert.Equal(t, "scheduled", r.Status)
	assert.False(t, r.StopReason.Valid || r.Stopped.Valid)
}

// dueReminder schedules email's reminder due a second ago.
func dueReminder(t *testing.T, db store.Store, email string) {
	t.Helper()
	_, err := db.RequestFoundryReminder(context.Background(), reminderRequest(email, "Europe/London", time.Now().Add(-time.Second), 300, nil))
	require.NoError(t, err)
}

func TestFoundryReminderClaims(t *testing.T) {
	ctx := context.Background()
	claim := func(t *testing.T, db store.Store, token string, lease time.Duration) *model.FoundryReminder {
		t.Helper()
		r, err := db.ClaimFoundryReminder(ctx, token, lease)
		require.NoError(t, err)
		return r
	}

	t.Run("claims nothing before its time", func(t *testing.T) {
		db := newTestStore(t)
		assert.Nil(t, claim(t, db, "t1", time.Minute))
		_, err := db.RequestFoundryReminder(ctx, reminderRequest("ada@example.com", "UTC", time.Now().Add(time.Hour), 300, nil))
		require.NoError(t, err)
		assert.Nil(t, claim(t, db, "t1", time.Minute))
	})
	t.Run("holds a reminder for its lease, then lets it go", func(t *testing.T) {
		db := newTestStore(t)
		dueReminder(t, db, "ada@example.com")
		first := claim(t, db, "t1", 200*time.Millisecond)
		require.NotNil(t, first)
		assert.Equal(t, "ada@example.com", first.Email)
		assert.Equal(t, "Europe/London", first.TimeZone)
		assert.Equal(t, model.FoundryReminderScheduled, first.Status)
		assert.Equal(t, 1, first.Attempts)
		assert.Equal(t, 0, first.Sent)
		require.NotNil(t, first.ClaimToken)
		assert.Equal(t, "t1", *first.ClaimToken)
		assert.Nil(t, claim(t, db, "t2", time.Minute), "a held reminder is not claimed twice")

		time.Sleep(300 * time.Millisecond)
		again := claim(t, db, "t2", time.Minute)
		require.NotNil(t, again)
		assert.Equal(t, 2, again.Attempts)
		assert.ErrorIs(t, db.FinishFoundryReminder(ctx, first.ID, "t1", model.FoundryReminderSent{}), store.ErrNotFound,
			"the lapsed claim ends nothing")
	})
	t.Run("counts a reminder sent and schedules the next, then ends the schedule", func(t *testing.T) {
		db := newTestStore(t)
		dueReminder(t, db, "ada@example.com")
		r := claim(t, db, "t1", time.Minute)
		require.NotNil(t, r)
		next := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
		require.NoError(t, db.FinishFoundryReminder(ctx, r.ID, "t1", model.FoundryReminderSent{Next: &next}))
		got := readReminder(t, "ada@example.com")
		assert.Equal(t, "scheduled", got.Status)
		assert.Equal(t, 1, got.Sent)
		assert.Equal(t, 0, got.Attempts, "the next reminder's attempts start from 0")
		assert.True(t, got.Next.Time.Equal(next))
		assert.True(t, got.LastSent.Valid)
		assert.False(t, got.Hold.Valid, "the claim ends with it")
		assert.ErrorIs(t, db.FinishFoundryReminder(ctx, r.ID, "t1", model.FoundryReminderSent{}), store.ErrNotFound, "a claim ends once")

		_, err := testDB.Exec(`UPDATE foundry_reminders SET next_send_at=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, r.ID)
		require.NoError(t, err)
		r = claim(t, db, "t2", time.Minute)
		require.NotNil(t, r)
		assert.Equal(t, 1, r.Sent)
		require.NoError(t, db.FinishFoundryReminder(ctx, r.ID, "t2", model.FoundryReminderSent{}))
		got = readReminder(t, "ada@example.com")
		assert.Equal(t, "done", got.Status)
		assert.Equal(t, 2, got.Sent)
		assert.False(t, got.Next.Valid)
		assert.Nil(t, claim(t, db, "t3", time.Minute), "a schedule that ended is never claimed")
	})
	t.Run("retries a reminder at its time, then gives up on it", func(t *testing.T) {
		db := newTestStore(t)
		dueReminder(t, db, "ada@example.com")
		r := claim(t, db, "t1", time.Minute)
		require.NotNil(t, r)
		later := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
		require.NoError(t, db.FinishFoundryReminder(ctx, r.ID, "t1", model.FoundryReminderRetry{At: later, Error: "throttled"}))
		got := readReminder(t, "ada@example.com")
		assert.Equal(t, "scheduled", got.Status)
		assert.True(t, got.Next.Time.Equal(later))
		assert.Equal(t, "throttled", got.LastError.String)
		assert.Equal(t, 1, got.Attempts, "a retry keeps its attempts")
		assert.Nil(t, claim(t, db, "t2", time.Minute), "a retry waits for its time")

		_, err := testDB.Exec(`UPDATE foundry_reminders SET next_send_at=NOW(6) - INTERVAL 1 SECOND WHERE id=?`, r.ID)
		require.NoError(t, err)
		r = claim(t, db, "t2", time.Minute)
		require.NotNil(t, r)
		require.NoError(t, db.FinishFoundryReminder(ctx, r.ID, "t2", model.FoundryReminderGiveUp{Error: "throttled (gave up after 2 attempts)"}))
		got = readReminder(t, "ada@example.com")
		assert.Equal(t, "failed", got.Status)
		assert.False(t, got.Next.Valid)
		assert.Equal(t, "throttled (gave up after 2 attempts)", got.LastError.String)

		require.Error(t, db.FinishFoundryReminder(ctx, r.ID, "t2", model.FoundryReminderRetry{At: later}), "a retry needs its failure")
		require.Error(t, db.FinishFoundryReminder(ctx, r.ID, "t2", model.FoundryReminderGiveUp{}), "a failure needs its error")
	})
	// A person signing in while their reminder is sent stops the schedule:
	// the claim then records nothing.
	t.Run("loses a claim of a schedule stopped meanwhile", func(t *testing.T) {
		db := newTestStore(t)
		dueReminder(t, db, "ada@example.com")
		r := claim(t, db, "t1", time.Minute)
		require.NotNil(t, r)
		stopped, err := db.StopFoundryReminder(ctx, "ada@example.com", model.FoundryReminderStoppedSignedIn)
		require.NoError(t, err)
		require.True(t, stopped)
		next := time.Now().Add(7 * 24 * time.Hour)
		assert.ErrorIs(t, db.FinishFoundryReminder(ctx, r.ID, "t1", model.FoundryReminderSent{Next: &next}), store.ErrNotFound)
		got := readReminder(t, "ada@example.com")
		assert.Equal(t, "stopped", got.Status)
		assert.False(t, got.Hold.Valid)
	})
}

// The worker runs in a role with replicas: two of them sending the same
// reminders at once send each one once, and each schedules its next.
func TestFoundryReminderWorkerReplicas(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	addresses := []string{"ada@example.com", "grace@example.com", "alan@example.com", "katherine@example.com", "edsger@example.com"}
	for _, email := range addresses {
		dueReminder(t, db, email)
	}
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
		require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_reminders WHERE sent=1`).Scan(&n))
		return n
	}
	require.Eventually(t, func() bool { return sent() == len(addresses) }, 20*time.Second, 50*time.Millisecond)
	time.Sleep(300 * time.Millisecond) // a few more ticks of each replica
	for _, task := range replicas {
		require.NoError(t, task.Stop(ctx))
	}
	wg.Wait()

	mail.mu.Lock()
	defer mail.mu.Unlock()
	require.Len(t, mail.sent, len(addresses))
	for _, email := range addresses {
		assert.Equal(t, 1, mail.sent[email+" Your reminder to try Helix Foundry"], "%s", email)
		r := readReminder(t, email)
		assert.Equal(t, "scheduled", r.Status)
		london, _ := reminder.Zone("Europe/London")
		next := r.Next.Time.In(london)
		assert.Equal(t, reminder.Hour, next.Hour(), "the next one goes at 09:00 in the address's zone")
		assert.WithinDuration(t, time.Now().Add(7*24*time.Hour), r.Next.Time, 4*24*time.Hour, "about a week on")
	}
}

// replicaMail's addresses are its recipients' own: a reminder names no user.
var _ ports.Mail = (*replicaMail)(nil)
