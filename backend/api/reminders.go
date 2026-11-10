package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/reminder"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// reminderLimitRetry is how long a request refused for the hour's cap is
// told to wait: about when enough of the hour's schedules have aged out.
const reminderLimitRetry = 15 * time.Minute

// CreateFoundryReminder starts the reminders a person asked for on the
// landing page, from a phone, to try Foundry on a computer, or answers the
// schedule the address already has (again). The first goes at the next
// weekday 09:00 in the person's zone, at least reminder.Lead away, and no
// sooner than foundry.reminders.min_gap_days after the address's last one,
// so however often it asks, an address gets at most one in that many days.
//
// Only the door calls it, for the site, with the SHA-256 of the site's
// secret (reminderCaller). It refuses INVALID_ARGUMENT an address that isn't
// one, or a consent other than reminder.Consent, and RESOURCE_EXHAUSTED one
// past foundry.reminders.max_new_per_hour. No answer, log or error quotes the
// address.
func (s *Service) CreateFoundryReminder(ctx context.Context, req *foundryv1.CreateFoundryReminderRequest) (
	*foundryv1.CreateFoundryReminderResponse, error) {
	cfg := s.deps.Cfg.Foundry.Reminders
	if err := reminderCaller(ctx, cfg, req.GetSecretSha256()); err != nil {
		return nil, err
	}
	email, err := reminder.Normalize(req.GetEmail())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if req.GetConsent() != reminder.Consent {
		return nil, status.Errorf(codes.InvalidArgument, "consent must be %q, the words the site showed", reminder.Consent)
	}
	loc, zone := reminder.Zone(req.GetTimeZone())
	answer, err := s.deps.DB.RequestFoundryReminder(ctx, model.FoundryReminderRequest{
		ID: id.New(), Email: email, TimeZone: zone, Consent: reminder.Consent, MaxNewPerHour: cfg.MaxNewPerHour,
		Schedule: reminderSchedule(time.Now(), loc, cfg.MinGapDays),
	})
	switch {
	case errors.Is(err, foundrystore.ErrFoundryReminderLimit):
		st, _ := status.New(codes.ResourceExhausted, "Foundry is taking too many reminders just now; ask again later").
			WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(reminderLimitRetry)})
		return nil, st.Err()
	case err != nil:
		log.From(ctx).Error("take a reminder failed", "err", err)
		return nil, status.Error(codes.Unavailable, "Foundry couldn't take the reminder just now; ask again")
	}
	log.From(ctx).Info("foundry reminder requested", "again", answer.Again, "zone", zone)
	return &foundryv1.CreateFoundryReminderResponse{NextSendAt: timestamppb.New(answer.NextSendAt), Again: answer.Again}, nil
}

// StopFoundryReminders stops an address's reminders once its person has
// signed in to Foundry: the site calls it, through the door, at every
// sign-in, so most addresses it names have none, which is no error. A
// reminder being sent as it stops may still go.
func (s *Service) StopFoundryReminders(ctx context.Context, req *foundryv1.StopFoundryRemindersRequest) (
	*foundryv1.StopFoundryRemindersResponse, error) {
	if err := reminderCaller(ctx, s.deps.Cfg.Foundry.Reminders, req.GetSecretSha256()); err != nil {
		return nil, err
	}
	email, err := reminder.Normalize(req.GetEmail())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	stopped, err := s.deps.DB.StopFoundryReminder(ctx, email, model.FoundryReminderStoppedSignedIn)
	if err != nil {
		log.From(ctx).Error("stop reminders failed", "err", err)
		return nil, status.Error(codes.Unavailable, "Foundry couldn't stop the reminders just now; ask again")
	}
	if stopped {
		log.From(ctx).Info("foundry reminders stopped", "reason", model.FoundryReminderStoppedSignedIn)
	}
	return &foundryv1.StopFoundryRemindersResponse{}, nil
}

// reminderCaller admits a call of the reminders: the door's (an Actor of
// SOURCE_EVENT_SOURCE naming no token, workspace, user or key), bearing the
// SHA-256 of the site's secret, which foundry.reminders.secret_sha256 is.
// Anyone else is PERMISSION_DENIED; with no secret configured reminders are
// off, FAILED_PRECONDITION; another hash is UNAUTHENTICATED, compared in
// constant time.
func reminderCaller(ctx context.Context, cfg config.RemindersConfig, hash string) error {
	a := actor(ctx)
	if a.Source != ports.SourceEventSource || a.EventSourceTokenID != "" || a.WorkspaceID != "" || a.UserID != "" || a.ServiceCredentialID != "" {
		return status.Error(codes.PermissionDenied, "only Foundry's door takes reminders, for the site")
	}
	if cfg.SecretSHA256 == "" {
		return status.Error(codes.FailedPrecondition, "reminders are off: foundry.reminders.secret_sha256 is not set")
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(cfg.SecretSHA256)) != 1 {
		return status.Error(codes.Unauthenticated, "the bearer is not the site's reminders secret")
	}
	return nil
}

// reminderSchedule is when a new schedule asked for at now, in loc, sends
// its first reminder, given when the address's last reminder went (nil for
// none): the next weekday 09:00 at least reminder.Lead away, or, when that
// is sooner than minGapDays after the last, the first weekday 09:00 that
// many days after it.
func reminderSchedule(now time.Time, loc *time.Location, minGapDays int) func(*time.Time) time.Time {
	return func(last *time.Time) time.Time {
		first := reminder.First(now, loc)
		if last == nil {
			return first
		}
		if gap := reminder.After(*last, minGapDays, loc); gap.After(first) {
			return gap
		}
		return first
	}
}
