package foundryslackworker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store"
)

type noSessions struct{}

func (noSessions) Session(context.Context, ports.Actor, integration.ConfirmMode) (integration.Session, error) {
	return integration.Session{}, errors.New("none")
}

func enabled() *config.Config {
	cfg := config.Defaults()
	cfg.Foundry.Slack.Enabled = true
	cfg.Foundry.Slack.SiteURL = "https://foundry.example.com"
	return &cfg
}

func TestNew(t *testing.T) {
	off := config.Defaults()
	task, err := New(Deps{Cfg: &off, Logger: slog.Default()})
	require.NoError(t, err, "off, it needs nothing")
	assert.Equal(t, Name, task.Name())
	assert.Nil(t, task.bot)

	_, err = New(Deps{Cfg: enabled(), Logger: slog.Default()})
	assert.ErrorContains(t, err, "store and a session host")

	db := store.Unimplemented{}
	_, err = New(Deps{Cfg: enabled(), Logger: slog.Default(), DB: db, Sessions: noSessions{}})
	assert.ErrorContains(t, err, "keyring", "installs' tokens are sealed with the connectors' keyring")

	withKeys := enabled()
	withKeys.Foundry.Connectors.CurrentKeyID = "k1"
	withKeys.Foundry.Connectors.CurrentKey = "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE="
	task, err = New(Deps{Cfg: withKeys, Logger: slog.Default(), DB: db, Sessions: noSessions{}})
	require.NoError(t, err)
	assert.NotNil(t, task.bot)

	bad := enabled()
	bad.Foundry.Slack.APIShare = 2
	bad.Foundry.Connectors = withKeys.Foundry.Connectors
	_, err = New(Deps{Cfg: bad, Logger: slog.Default(), DB: db, Sessions: noSessions{}})
	assert.Error(t, err)
}

// fakeBot hands out n jobs of each kind, each blocking until released.
type fakeBot struct {
	mu       sync.Mutex
	intake   int
	follows  int
	claims   atomic.Int32
	ran      atomic.Int32
	sweeps   atomic.Int32
	backfill atomic.Int32
	release  chan struct{}
	claimErr error
}

func (b *fakeBot) next(left *int) (func(context.Context), error) {
	b.claims.Add(1)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.claimErr != nil {
		return nil, b.claimErr
	}
	if *left == 0 {
		return nil, nil
	}
	*left--
	return func(ctx context.Context) {
		b.ran.Add(1)
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	}, nil
}

func (b *fakeBot) NextIntake(context.Context) (func(context.Context), error) {
	return b.next(&b.intake)
}
func (b *fakeBot) NextFollow(context.Context) (func(context.Context), error) {
	return b.next(&b.follows)
}
func (b *fakeBot) Sweep(context.Context)    { b.sweeps.Add(1) }
func (b *fakeBot) Backfill(context.Context) { b.backfill.Add(1) }

func TestDrainHoldsASlotPerJob(t *testing.T) {
	b := &fakeBot{intake: 5, release: make(chan struct{})}
	task := &Task{bot: b, intake: make(chan struct{}, 2)}
	task.drain(context.Background(), task.intake, b.NextIntake, "claim")
	assert.Eventually(t, func() bool { return b.ran.Load() == 2 }, time.Second, time.Millisecond)
	assert.EqualValues(t, 2, b.claims.Load(), "full slots claim nothing more")
	close(b.release)
	task.running.Wait()
	for b.ran.Load() < 5 {
		task.drain(context.Background(), task.intake, b.NextIntake, "claim")
		task.running.Wait()
	}
	assert.EqualValues(t, 5, b.ran.Load())
	assert.Len(t, task.intake, 0, "every slot given back")

	b.claimErr = errors.New("down")
	task.drain(context.Background(), task.intake, b.NextIntake, "claim")
	assert.Len(t, task.intake, 0)
}

func TestStartRunsUntilStopped(t *testing.T) {
	cfg := enabled().Foundry.Slack
	cfg.PollInterval, cfg.HTTP.Listen = 5*time.Millisecond, ""
	b := &fakeBot{intake: 1, follows: 1, release: make(chan struct{})}
	task := &Task{deps: Deps{Logger: slog.Default()}, cfg: cfg, bot: b, intake: make(chan struct{}, 1), follows: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- task.Start(ctx) }()
	assert.Eventually(t, func() bool { return b.ran.Load() == 2 && b.sweeps.Load() >= 1 }, time.Second, time.Millisecond)
	cancel()
	require.NoError(t, <-done, "jobs in flight see the stop and end")
	assert.Zero(t, b.backfill.Load(), "the feed off, nothing backfills")

	cfg.Feed.Enabled, cfg.Backfill.PollInterval = true, 5*time.Millisecond
	fed := &fakeBot{release: make(chan struct{})}
	task = &Task{deps: Deps{Logger: slog.Default()}, cfg: cfg, bot: fed, intake: make(chan struct{}, 1), follows: make(chan struct{}, 1)}
	ctx, cancel = context.WithCancel(context.Background())
	go func() { done <- task.Start(ctx) }()
	assert.Eventually(t, func() bool { return fed.backfill.Load() >= 2 }, time.Second, time.Millisecond, "the feed on, it backfills every poll")
	cancel()
	require.NoError(t, <-done)

	off := &Task{deps: Deps{Logger: slog.Default()}, cfg: config.SlackConfig{}}
	ctx, cancel = context.WithCancel(context.Background())
	go func() { done <- off.Start(ctx) }()
	cancel()
	require.NoError(t, <-done)
	require.NoError(t, off.Stop(context.Background()))
}
