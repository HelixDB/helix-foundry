package slackbot

import (
	"context"
	"errors"

	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store"
)

const (
	// sweepUninstalls is the most revoked installs one sweep tells Slack of.
	sweepUninstalls = 20
	// sweepThreads is the most Slack-born threads one sweep purges.
	sweepThreads = 100
	// sweepPrune is the most rows of each kind one sweep prunes.
	sweepPrune = 1000
)

// Sweep does what no message starts (the spec's §8.2 and §12): it tells
// Slack of the uninstalls it wasn't told of yet (apps.uninstall), so their
// teams may install again; purges the Slack-born Founder threads idle past
// foundry.slack.retention.threads, of a revoked install, or deleted; and
// prunes the intake, replies, link tokens and install states Slack's terms
// let go. Each step goes on whatever another's failure; each is counted.
func (b *Bot) Sweep(ctx context.Context) {
	installs, err := b.db.ListFoundrySlackUninstalls(ctx, sweepUninstalls)
	if err != nil {
		sweeps.WithLabelValues("uninstall", "error").Inc()
		log.From(ctx).Error("list the Slack installs to uninstall failed", "err", err)
	}
	for _, install := range installs {
		sweeps.WithLabelValues("uninstall", b.uninstall(ctx, install.ID)).Inc()
	}
	switch n, err := b.db.PurgeFoundrySlackThreads(ctx, b.cfg.Retention.Threads, sweepThreads); {
	case err != nil:
		sweeps.WithLabelValues("purge", "error").Inc()
		log.From(ctx).Error("purge Slack-born Founder threads failed", "err", err)
	default:
		sweeps.WithLabelValues("purge", "purged").Add(float64(n))
	}
	switch n, err := b.db.PruneFoundrySlack(ctx, sweepPrune); {
	case err != nil:
		sweeps.WithLabelValues("prune", "error").Inc()
		log.From(ctx).Error("prune Slack's rows failed", "err", err)
	default:
		sweeps.WithLabelValues("prune", "pruned").Add(float64(n))
	}
}

// uninstall tells Slack a revoked install is gone, with its token, and
// records that it knows, which deletes the token: a token Slack no longer
// takes means Slack knows already. It names the outcome: uninstalled,
// deferred (a rate limit), or error, tried at the next sweep.
func (b *Bot) uninstall(ctx context.Context, installID string) string {
	bot, err := b.token(ctx, installID)
	if err == nil {
		err = b.slack.Uninstall(ctx, bot, b.cfg.ClientID, b.cfg.ClientSecret)
	}
	if _, limited := rateLimited(err); limited {
		return "deferred"
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) &&
		!slack.IsCode(err, "invalid_auth", "account_inactive", "token_revoked", "token_expired", "not_authed") {
		log.From(ctx).Warn("slack apps.uninstall failed; the next sweep tries again", "install_id", installID, "err", err)
		return "error"
	}
	if err := b.db.MarkFoundrySlackUninstalled(ctx, installID); err != nil && !errors.Is(err, store.ErrNotFound) {
		log.From(ctx).Warn("record a Slack uninstall failed; the next sweep tries again", "install_id", installID, "err", err)
		return "error"
	}
	return "uninstalled"
}
