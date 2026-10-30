// Package foundrynotifyworker emails people about Foundry's work: the
// requester of an ontology's first draft, asked to load when ready, once
// it has ended (loaded, loaded up to the month's budget, current but its
// load failed, not drafted, or drafted for review); and, once, a person who
// watched the sample draft an ontology a day earlier and has brought no
// data of their own since, to try Foundry on theirs. Each tick it finds the
// emails due in the store (foundry_notifications), then claims and sends
// each one through the host's mail (ports.Mail). It runs as a task of the
// foundry-sync-worker role where the host sends mail, never in a standalone
// install, and serves no HTTP of its own.
package foundrynotifyworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.uber.org/dig"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/server/endpoint"
	"github.com/helixdb/helix-foundry/backend/server/tasks"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const Name = "foundry-notify-worker"

// sendWait bounds what one email takes: its reads, its recipient and its
// send.
const sendWait = time.Minute

// lease is how long a claim holds an email, well past sendWait and
// finishWait, so another replica never claims an email while it is being
// sent. Only a worker killed between sending an email and recording it
// leaves one to be claimed, and sent, again.
const lease = 5 * time.Minute

// finishWait is how long recording an email's end may take, even while the
// worker stops.
const finishWait = 10 * time.Second

// discoverBatch is the most generations one tick reads of each kind; any
// more are found on the next.
const discoverBatch = 200

// maxRetryBackoff caps the wait before an email is tried again.
const maxRetryBackoff = time.Hour

type Deps struct {
	dig.In

	Cfg    *config.Config
	Logger *slog.Logger
	Health endpoint.Health `optional:"true"`
	DB     store.Store     `optional:"true"`
	// Mail sends the emails; without it the worker doesn't start.
	Mail ports.Mail `optional:"true"`
}

// outbox is foundry_notifications, and what the emails read.
type outbox interface {
	DiscoverFoundryOntologyReady(ctx context.Context, since time.Time, limit int, newID func() string) (int, error)
	DiscoverFoundrySampleFollowUps(ctx context.Context, from, to time.Time, limit int, newID func() string) (int, error)
	ClaimFoundryNotification(ctx context.Context, token string, lease time.Duration) (*model.FoundryNotification, error)
	FinishFoundryNotification(ctx context.Context, id, token string, outcome model.FoundryNotificationOutcome) error
	WorkspaceDeleting(ctx context.Context, id string) (bool, error)
	GetFoundryOntology(ctx context.Context, workspaceID, id string) (*model.FoundryOntology, error)
	GetOntologyGeneration(ctx context.Context, ontologyID, id string) (*model.OntologyGeneration, error)
	GetFoundryGraphJobByClientToken(ctx context.Context, ontologyID, clientToken string) (*model.FoundryGraphJob, error)
	GetFoundryLoad(ctx context.Context, ontologyID, id string) (*model.FoundryLoad, error)
}

// Task finds the emails due and sends them one at a time.
type Task struct {
	deps   Deps
	cfg    config.NotifyConfig
	box    outbox
	mail   ports.Mail
	site   site
	now    func() time.Time
	worker tasks.Worker
}

// New is the worker on d's store and mail, which it needs both of, and
// foundry.notify, whose site_url its links need.
func New(d Deps) (*Task, error) {
	switch {
	case d.DB == nil:
		return nil, errors.New("foundry-notify-worker: db not configured")
	case d.Mail == nil:
		return nil, errors.New("foundry-notify-worker: the host sends no mail (ports.Mail)")
	}
	cfg := d.Cfg.Foundry.Notify
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("foundry-notify-worker: %w", err)
	}
	return &Task{deps: d, cfg: cfg, box: d.DB, mail: d.Mail, site: site(cfg.SiteURL), now: time.Now}, nil
}

func (t *Task) Name() string { return Name }

// Start sends until Stop. It serves no HTTP of its own: the pod's health
// and metrics are the sync worker's listener's.
func (t *Task) Start(ctx context.Context) error {
	return t.worker.Run(ctx, Name, "", t.deps.Health, t.deps.Logger, func(ctx context.Context) error {
		tasks.Tick(ctx, t.cfg.PollInterval, t.tick)
		return nil
	})
}

func (t *Task) Stop(ctx context.Context) error { return t.worker.Stop(ctx) }

// tick finds the emails due, then sends every one due until none is left.
// A first draft's email is found while it ended within follow_up_window; a
// sample run's follow-up once it ended follow_up_after ago, and no longer
// past the window.
func (t *Task) tick(ctx context.Context) {
	now := t.now()
	window := now.Add(-t.cfg.FollowUpWindow)
	for _, d := range []struct {
		kind     model.FoundryNotificationKind
		discover func() (int, error)
	}{
		{model.FoundryNotifyOntologyReady, func() (int, error) {
			return t.box.DiscoverFoundryOntologyReady(ctx, window, discoverBatch, id.New)
		}},
		{model.FoundryNotifySampleFollowUp, func() (int, error) {
			return t.box.DiscoverFoundrySampleFollowUps(ctx, window, now.Add(-t.cfg.FollowUpAfter), discoverBatch, id.New)
		}},
	} {
		found, err := d.discover()
		notifications.WithLabelValues(string(d.kind), "found").Add(float64(found))
		if err != nil {
			log.From(ctx).Error("find emails to send failed", "kind", d.kind, "err", err)
		}
	}
	for ctx.Err() == nil {
		n, err := t.box.ClaimFoundryNotification(ctx, id.New(), lease)
		if err != nil {
			log.From(ctx).Error("claim an email failed", "err", err)
			return
		}
		if n == nil {
			return
		}
		t.deliver(ctx, n)
	}
}

// deliver sends one claimed email, then records how that went. A worker
// stopping lets the email it is sending finish, within sendWait, rather
// than cut a send the host may have made short and send it again later.
func (t *Task) deliver(ctx context.Context, n *model.FoundryNotification) {
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sendWait)
	defer cancel()
	outcome, label := t.send(sendCtx, n)
	finishCtx, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), finishWait)
	defer cancelFinish()
	switch err := t.box.FinishFoundryNotification(finishCtx, n.ID, *n.ClaimToken, outcome); {
	case errors.Is(err, store.ErrNotFound):
		log.From(ctx).Warn("an email's claim lapsed before it was recorded", "notification", n.ID, "kind", n.Kind, "outcome", label)
		label = "lost"
	case err != nil:
		log.From(ctx).Warn("record an email failed; its claim lapses and it is tried again", "notification", n.ID, "kind", n.Kind,
			"outcome", label, "err", err)
		label = "unrecorded"
	}
	notifications.WithLabelValues(string(n.Kind), label).Inc()
}

// send writes and sends n, and says how it ended, with its metric's label.
// An email whose person, ontology or workspace is gone, or that has nothing
// to say, is skipped. One that couldn't be read or sent is tried again
// after a wait that doubles from a minute, up to maxRetryBackoff, and fails
// once max_attempts claims have tried it; a claim past them (its workers
// stopped while sending it) fails it unsent.
func (t *Task) send(ctx context.Context, n *model.FoundryNotification) (model.FoundryNotificationOutcome, string) {
	retry := func(err error) (model.FoundryNotificationOutcome, string) {
		log.From(ctx).Warn("send an email failed", "notification", n.ID, "kind", n.Kind, "attempt", n.Attempts, "err", err)
		if n.Attempts >= t.cfg.MaxAttempts {
			return model.FoundryNotificationGiveUp{Error: fmt.Sprintf("%v (gave up after %d attempts)", err, n.Attempts)}, "failed"
		}
		wait := min(time.Minute<<min(max(n.Attempts-1, 0), 10), maxRetryBackoff)
		return model.FoundryNotificationRetry{At: t.now().Add(wait), Error: err.Error()}, "retried"
	}
	if n.Attempts > t.cfg.MaxAttempts {
		return model.FoundryNotificationGiveUp{Error: fmt.Sprintf("claimed %d times, past max_attempts, without an end recorded", n.Attempts)}, "failed"
	}
	m, skip, err := t.compose(ctx, n)
	switch {
	case err != nil:
		return retry(err)
	case skip != "":
		return model.FoundryNotificationSkip{Reason: skip}, "skipped"
	}
	to, err := t.mail.Recipient(ctx, n.UserID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return model.FoundryNotificationSkip{Reason: "the person is gone, or has no address"}, "skipped"
	case err != nil:
		return retry(fmt.Errorf("find the person's address: %w", err))
	}
	email, err := render(to.FirstName, m)
	if err != nil {
		return retry(fmt.Errorf("render the email: %w", err))
	}
	if err := t.mail.Send(ctx, to, email); err != nil {
		return retry(fmt.Errorf("send: %w", err))
	}
	return model.FoundryNotificationDelivered{}, "sent"
}

// compose is n's message, or why n is skipped; an error is one of the
// reads, which a retry may get past.
func (t *Task) compose(ctx context.Context, n *model.FoundryNotification) (_ message, skip string, _ error) {
	switch deleting, err := t.box.WorkspaceDeleting(ctx, n.WorkspaceID); {
	case errors.Is(err, store.ErrNotFound) || err == nil && deleting:
		return message{}, "the workspace is gone", nil
	case err != nil:
		return message{}, "", fmt.Errorf("read the workspace: %w", err)
	}
	switch n.Kind {
	case model.FoundryNotifyOntologyReady:
		return t.ontologyReady(ctx, n)
	case model.FoundryNotifySampleFollowUp:
		return sampleFollowUp(t.site.connect(n.WorkspaceID)), "", nil
	}
	return message{}, fmt.Sprintf("Foundry sends no email of kind %q", n.Kind), nil
}

// ontologyReady is the email of how a first draft ended, which its
// generation, its load job (FirstDraftLoadTokenPrefix) and that job's load
// say. A load that is no longer ready, partial or failed (deleted since)
// has nothing left to tell.
func (t *Task) ontologyReady(ctx context.Context, n *model.FoundryNotification) (_ message, skip string, _ error) {
	if n.OntologyID == nil {
		return message{}, "the email names no ontology", nil
	}
	o, err := t.box.GetFoundryOntology(ctx, n.WorkspaceID, *n.OntologyID)
	switch {
	case errors.Is(err, store.ErrNotFound) || err == nil && o.DeletedAt != nil:
		return message{}, "the ontology is gone", nil
	case err != nil:
		return message{}, "", fmt.Errorf("read the ontology: %w", err)
	}
	gen, err := t.box.GetOntologyGeneration(ctx, o.ID, n.SubjectID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return message{}, "the generation is gone", nil
	case err != nil:
		return message{}, "", fmt.Errorf("read the generation: %w", err)
	}
	name, start := ontologyName(o.Name)
	overview := t.site.overview(o.WorkspaceID, o.ID)
	switch gen.Status {
	case model.OntologyGenerationFailed:
		failure := ""
		if gen.Error != nil {
			failure = *gen.Error
		}
		return draftFailed(name, start, failure, overview), "", nil
	case model.OntologyGenerationSucceeded:
	default:
		return message{}, fmt.Sprintf("the generation is %s, not ended", gen.Status), nil
	}
	job, err := t.box.GetFoundryGraphJobByClientToken(ctx, o.ID, model.FirstDraftLoadTokenPrefix+gen.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return draftOnly(name, start, overview), "", nil
	case err != nil:
		return message{}, "", fmt.Errorf("read the first draft's load job: %w", err)
	}
	load, err := t.box.GetFoundryLoad(ctx, o.ID, job.LoadID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return message{}, "the first draft's load is gone", nil
	case err != nil:
		return message{}, "", fmt.Errorf("read the first draft's load: %w", err)
	}
	ask := t.site.ask(o.WorkspaceID, o.ID)
	switch load.Status {
	case model.FoundryLoadReady:
		return loadReady(start, ask, overview), "", nil
	case model.FoundryLoadPartial:
		return loadPartial(start, ask, overview), "", nil
	case model.FoundryLoadFailed:
		failure := ""
		if job.Error != nil {
			failure = *job.Error
		}
		return loadFailed(name, start, failure, overview), "", nil
	}
	return message{}, fmt.Sprintf("the first draft's load is %s", load.Status), nil
}
