// Package foundryslackworker runs Helix Foundry's Slack bot (package
// slackbot, foundry.slack): it handles what Slack sent, which the API
// recorded in intake, follows the Founder turns it started into Slack, and
// sweeps (uninstalls Slack wasn't told of, Slack-born threads past their
// retention, rows Slack's terms let go). Off (foundry.slack.enabled false)
// it only serves its health listener, so a host may list it everywhere.
//
// Messages and follows run on slots of their own, so turns being followed
// never hold messages back: foundry.slack.concurrency messages at once, and
// followsPerSlot times as many follows, each a goroutine that mostly waits
// for its next read.
package foundryslackworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.uber.org/dig"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/server/endpoint"
	"github.com/helixdb/helix-foundry/backend/server/tasks"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/slackbot"
	"github.com/helixdb/helix-foundry/backend/store"
)

// Name is the task's name in a host's task list.
const Name = "foundry-slack-worker"

const (
	// sweepEvery is how often the worker sweeps.
	sweepEvery = 30 * time.Second
	// followsPerSlot is how many follows the worker runs per message slot:
	// a follow waits for its turn most of its life.
	followsPerSlot = 4
	// stopWait bounds a stop: follows give their replies back at once.
	stopWait = 15 * time.Second
)

// Deps is what the worker needs from its host.
type Deps struct {
	dig.In

	Cfg    *config.Config
	Logger *slog.Logger
	Health endpoint.Health `optional:"true"`
	DB     store.Store     `optional:"true"`
	// Sessions opens a linked person's session, whose Founder API the bot
	// calls as them; required while foundry.slack is on.
	Sessions integration.SessionHost `optional:"true"`
}

// bot is what the worker runs: *slackbot.Bot.
type bot interface {
	NextIntake(ctx context.Context) (func(context.Context), error)
	NextFollow(ctx context.Context) (func(context.Context), error)
	Sweep(ctx context.Context)
}

// Task runs the bot on its slots.
type Task struct {
	deps    Deps
	cfg     config.SlackConfig
	bot     bot
	worker  tasks.Worker
	intake  chan struct{}
	follows chan struct{}
	running sync.WaitGroup
}

// New is the worker on d's store and sessions, with Slack's Web API paced
// at foundry.slack.api_share of its limits and installs' tokens opened with
// the connectors' keyring.
func New(d Deps) (*Task, error) {
	cfg := d.Cfg.Foundry.Slack
	slots := max(cfg.Concurrency, 1)
	t := &Task{deps: d, cfg: cfg, intake: make(chan struct{}, slots), follows: make(chan struct{}, slots*followsPerSlot)}
	if !cfg.Enabled {
		return t, nil
	}
	if d.DB == nil || d.Sessions == nil {
		return nil, errors.New("foundry-slack-worker: the store and a session host are required while foundry.slack.enabled")
	}
	keys, err := connector.NewKeyring(d.Cfg.Foundry.Connectors)
	switch {
	case err != nil:
		return nil, fmt.Errorf("foundry-slack-worker: %w", err)
	case keys == nil:
		return nil, errors.New("foundry-slack-worker: the connectors' keyring, which seals installs' tokens, is required")
	}
	limiter, err := slack.NewLimiter(cfg.APIShare)
	if err != nil {
		return nil, fmt.Errorf("foundry-slack-worker: %w", err)
	}
	client, err := slack.NewClient(slack.ClientOptions{Limiter: limiter})
	if err != nil {
		return nil, fmt.Errorf("foundry-slack-worker: %w", err)
	}
	b, err := slackbot.New(slackbot.Options{DB: d.DB, Sessions: d.Sessions, Slack: client, Keys: keys, Cfg: cfg})
	if err != nil {
		return nil, fmt.Errorf("foundry-slack-worker: %w", err)
	}
	t.bot = b
	return t, nil
}

// Name is Name.
func (t *Task) Name() string { return Name }

// Start handles messages and follows turns every poll interval while a slot
// is free, and sweeps every sweepEvery; off, it only serves its health
// listener.
func (t *Task) Start(ctx context.Context) error {
	return t.worker.Run(ctx, Name, t.cfg.HTTP.Listen, t.deps.Health, t.deps.Logger, func(ctx context.Context) error {
		if !t.cfg.Enabled {
			<-ctx.Done()
			return nil
		}
		var loops sync.WaitGroup
		loops.Go(func() {
			tasks.Tick(ctx, t.cfg.PollInterval, func(ctx context.Context) { t.drain(ctx, t.intake, t.bot.NextIntake, "claim a Slack message") })
		})
		loops.Go(func() {
			tasks.Tick(ctx, t.cfg.PollInterval, func(ctx context.Context) { t.drain(ctx, t.follows, t.bot.NextFollow, "claim a Slack reply") })
		})
		tasks.Tick(ctx, sweepEvery, t.bot.Sweep)
		loops.Wait()
		t.running.Wait()
		return nil
	})
}

// Stop stops claiming; follows in flight give their replies back, due at
// once, and a message being handled ends its claim.
func (t *Task) Stop(ctx context.Context) error {
	waitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), stopWait)
	defer cancel()
	return t.worker.Stop(waitCtx)
}

// drain claims with next while a slot of slots is free and it finds work,
// and runs what it claimed on a goroutine of its own, holding the slot.
func (t *Task) drain(ctx context.Context, slots chan struct{}, next func(context.Context) (func(context.Context), error), what string) {
	for ctx.Err() == nil {
		select {
		case slots <- struct{}{}:
		default:
			return
		}
		run, err := next(ctx)
		if err != nil || run == nil {
			<-slots
			if err != nil {
				log.From(ctx).Error(what+" failed", "err", err)
			}
			return
		}
		t.running.Add(1)
		go func() {
			defer t.running.Done()
			defer func() { <-slots }()
			run(ctx)
		}()
	}
}
