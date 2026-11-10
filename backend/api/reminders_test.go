package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/reminder"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// reminderStore records the reminder requests and stops it is handed, and
// answers them with answer and err.
type reminderStore struct {
	foundrystore.Unimplemented
	requests []model.FoundryReminderRequest
	stops    []string
	answer   model.FoundryReminderAnswer
	err      error
}

func (r *reminderStore) RequestFoundryReminder(_ context.Context, req model.FoundryReminderRequest) (model.FoundryReminderAnswer, error) {
	r.requests = append(r.requests, req)
	return r.answer, r.err
}

func (r *reminderStore) StopFoundryReminder(_ context.Context, email, reason string) (bool, error) {
	r.stops = append(r.stops, email+" "+reason)
	return r.err == nil, r.err
}

const siteSecret = "the site's reminders secret, 32 bytes or more of it"

func secretHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// remindersService is the API with reminders on, under the site's secret.
func remindersService(db foundrystore.Store) *Service {
	cfg := config.Defaults()
	cfg.Foundry.Reminders.SecretSHA256 = secretHash(siteSecret)
	return &Service{deps: &runtimeDeps{Cfg: &cfg, DB: db}}
}

// door is a call as the foundry-events door makes it.
func door() context.Context {
	return ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceEventSource})
}

func TestCreateFoundryReminder(t *testing.T) {
	next := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	request := func(email, zone string) *foundryv1.CreateFoundryReminderRequest {
		return &foundryv1.CreateFoundryReminderRequest{SecretSha256: secretHash(siteSecret), Email: email, TimeZone: zone, Consent: reminder.Consent}
	}

	t.Run("schedules the address as kept, in its zone", func(t *testing.T) {
		db := &reminderStore{answer: model.FoundryReminderAnswer{NextSendAt: next}}
		before := time.Now()
		resp, err := remindersService(db).CreateFoundryReminder(door(), request("  Ada@Example.com ", "Europe/London"))
		require.NoError(t, err)
		assert.Equal(t, next, resp.GetNextSendAt().AsTime())
		assert.False(t, resp.GetAgain())
		require.Len(t, db.requests, 1)
		got := db.requests[0]
		assert.Equal(t, "ada@example.com", got.Email)
		assert.Equal(t, "Europe/London", got.TimeZone)
		assert.Equal(t, reminder.Consent, got.Consent)
		assert.Equal(t, 300, got.MaxNewPerHour)
		assert.Len(t, got.ID, 26)
		london, _ := reminder.Zone("Europe/London")
		first := got.Schedule(nil)
		assert.True(t, !first.Before(reminder.First(before, london)) && !first.After(reminder.First(time.Now(), london)),
			"a new address's first reminder is the next weekday 09:00: %s", first)
	})
	t.Run("answers a schedule running as it is", func(t *testing.T) {
		db := &reminderStore{answer: model.FoundryReminderAnswer{NextSendAt: next, Again: true}}
		resp, err := remindersService(db).CreateFoundryReminder(door(), request("ada@example.com", "Europe/London"))
		require.NoError(t, err)
		assert.True(t, resp.GetAgain())
	})
	t.Run("keeps a zone it doesn't know as UTC", func(t *testing.T) {
		db := &reminderStore{answer: model.FoundryReminderAnswer{NextSendAt: next}}
		_, err := remindersService(db).CreateFoundryReminder(door(), request("ada@example.com", "Mars/Olympus_Mons"))
		require.NoError(t, err)
		assert.Equal(t, "UTC", db.requests[0].TimeZone)
	})
	t.Run("refuses what it can't take, quoting no address", func(t *testing.T) {
		for name, req := range map[string]*foundryv1.CreateFoundryReminderRequest{
			"no address":       request("", "Europe/London"),
			"not an address":   request("ada at example dot com", "Europe/London"),
			"a named address":  request("Ada <ada@example.com>", "Europe/London"),
			"no consent":       {SecretSha256: secretHash(siteSecret), Email: "ada@example.com", TimeZone: "UTC"},
			"another wording":  {SecretSha256: secretHash(siteSecret), Email: "ada@example.com", TimeZone: "UTC", Consent: "remind-v0"},
			"a stuffed answer": request("ada@example.com\nBcc: grace@example.com", "UTC"),
		} {
			db := &reminderStore{}
			_, err := remindersService(db).CreateFoundryReminder(door(), req)
			assert.Equal(t, codes.InvalidArgument, status.Code(err), name)
			assert.NotContains(t, status.Convert(err).Message(), "ada@", name)
			assert.Empty(t, db.requests, name)
		}
	})
	t.Run("refuses past the hour's cap, saying when to ask again", func(t *testing.T) {
		db := &reminderStore{err: foundrystore.ErrFoundryReminderLimit}
		_, err := remindersService(db).CreateFoundryReminder(door(), request("ada@example.com", "UTC"))
		require.Equal(t, codes.ResourceExhausted, status.Code(err))
		var retry *errdetails.RetryInfo
		for _, d := range status.Convert(err).Details() {
			if info, ok := d.(*errdetails.RetryInfo); ok {
				retry = info
			}
		}
		require.NotNil(t, retry)
		assert.Equal(t, reminderLimitRetry, retry.GetRetryDelay().AsDuration())
	})
	t.Run("is unavailable when the store fails, saying nothing of it", func(t *testing.T) {
		db := &reminderStore{err: errors.New("Error 1205: Lock wait timeout exceeded for ada@example.com")}
		_, err := remindersService(db).CreateFoundryReminder(door(), request("ada@example.com", "UTC"))
		require.Equal(t, codes.Unavailable, status.Code(err))
		assert.NotContains(t, status.Convert(err).Message(), "ada")
	})
}

func TestStopFoundryReminders(t *testing.T) {
	db := &reminderStore{}
	_, err := remindersService(db).StopFoundryReminders(door(),
		&foundryv1.StopFoundryRemindersRequest{SecretSha256: secretHash(siteSecret), Email: "Grace@Example.com"})
	require.NoError(t, err)
	assert.Equal(t, []string{"grace@example.com signed_in"}, db.stops, "the address as kept, stopped as signed in")

	_, err = remindersService(db).StopFoundryReminders(door(), &foundryv1.StopFoundryRemindersRequest{SecretSha256: secretHash(siteSecret), Email: "grace"})
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	db.err = errors.New("connection refused")
	_, err = remindersService(db).StopFoundryReminders(door(),
		&foundryv1.StopFoundryRemindersRequest{SecretSha256: secretHash(siteSecret), Email: "grace@example.com"})
	assert.Equal(t, codes.Unavailable, status.Code(err))
}

// Only the door, bearing the site's secret, reaches the reminders, and only
// while they are on.
func TestReminderCaller(t *testing.T) {
	create := func(s *Service, ctx context.Context, hash string) error {
		_, err := s.CreateFoundryReminder(ctx, &foundryv1.CreateFoundryReminderRequest{
			SecretSha256: hash, Email: "ada@example.com", TimeZone: "UTC", Consent: reminder.Consent})
		return err
	}
	stop := func(s *Service, ctx context.Context, hash string) error {
		_, err := s.StopFoundryReminders(ctx, &foundryv1.StopFoundryRemindersRequest{SecretSha256: hash, Email: "ada@example.com"})
		return err
	}
	for name, call := range map[string]func(*Service, context.Context, string) error{"create": create, "stop": stop} {
		t.Run(name, func(t *testing.T) {
			for who, a := range map[string]ports.Actor{
				"a user":         {Source: ports.SourceUser, UserID: "user-1"},
				"an MCP client":  {Source: ports.SourceMCP, UserID: "user-1"},
				"a send token's": {Source: ports.SourceEventSource, EventSourceTokenID: "token-1", WorkspaceID: "ws-1"},
				"a grantee":      {Source: ports.SourceGrantee, UserID: "user-1", WorkspaceID: "ws-1"},
				"no one":         {},
			} {
				db := &reminderStore{}
				err := call(remindersService(db), ports.WithActor(context.Background(), a), secretHash(siteSecret))
				assert.Equal(t, codes.PermissionDenied, status.Code(err), who)
				assert.Empty(t, db.requests, who)
				assert.Empty(t, db.stops, who)
			}
			for what, hash := range map[string]string{
				"another secret's hash": secretHash("another secret"),
				"the secret itself":     siteSecret,
				"no hash":               "",
				"the hash upper-cased":  "A" + secretHash(siteSecret)[1:],
			} {
				err := call(remindersService(&reminderStore{}), door(), hash)
				assert.Equal(t, codes.Unauthenticated, status.Code(err), what)
			}
			off := remindersService(&reminderStore{})
			off.deps.Cfg.Foundry.Reminders.SecretSHA256 = ""
			assert.Equal(t, codes.FailedPrecondition, status.Code(call(off, door(), secretHash(siteSecret))), "off, whatever it bears")
			assert.Equal(t, codes.FailedPrecondition, status.Code(call(off, door(), "")))
		})
	}
}

// A new schedule's first reminder is the next weekday 09:00, but never
// sooner than min_gap_days after the address's last.
func TestReminderSchedule(t *testing.T) {
	london, _ := reminder.Zone("Europe/London")
	now := time.Date(2026, 11, 10, 7, 30, 0, 0, time.UTC) // a Tuesday, 07:30 in London
	schedule := reminderSchedule(now, london, 7)
	tuesday := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	assert.Equal(t, tuesday, schedule(nil), "no reminder before: the next weekday 09:00")
	longAgo := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	assert.Equal(t, tuesday, schedule(&longAgo), "the last long ago")
	recent := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC) // the Friday before
	assert.Equal(t, time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC), schedule(&recent), "a week after the last, not sooner")
}
