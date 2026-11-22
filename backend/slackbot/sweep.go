package slackbot

import (
	"context"
	"errors"
	"time"

	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/slack"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// sweepUninstalls is the most revoked installs one sweep tells Slack of.
	sweepUninstalls = 20
	// sweepThreads is the most Slack-born threads one sweep purges.
	sweepThreads = 100
	// sweepPrune is the most rows of each kind one sweep prunes.
	sweepPrune = 1000
	// uninstallPatience is how long after its revoke an install's sweep
	// keeps asking Slack to uninstall it (apps.uninstall) through failures
	// that may clear (Slack's own, or the app's credentials refused); then
	// it is taken as uninstalled, so its team can install again.
	uninstallPatience = 24 * time.Hour
)

// Sweep does what no message starts (the spec's §8.2 and §12): it tells
// Slack of the uninstalls it wasn't told of yet (apps.uninstall), so their
// teams may install again; purges the Slack-born Founder threads idle past
// foundry.slack.retention.threads or of a revoked install; and prunes the
// intake and replies past foundry.slack.retention.intake and .replies, and
// the link tokens and install states Slack's terms let go. Each step goes on
// whatever another's failure; each is counted.
func (b *Bot) Sweep(ctx context.Context) {
	installs, err := b.db.ListFoundrySlackUninstalls(ctx, sweepUninstalls)
	if err != nil {
		sweeps.WithLabelValues("uninstall", "error").Inc()
		log.From(ctx).Error("list the Slack installs to uninstall failed", "err", err)
	}
	for _, install := range installs {
		sweeps.WithLabelValues("uninstall", b.uninstall(ctx, install)).Inc()
	}
	switch n, err := b.db.PurgeFoundrySlackThreads(ctx, b.cfg.Retention.Threads, sweepThreads); {
	case err != nil:
		sweeps.WithLabelValues("purge", "error").Inc()
		log.From(ctx).Error("purge Slack-born Founder threads failed", "err", err)
	default:
		sweeps.WithLabelValues("purge", "purged").Add(float64(n))
	}
	switch n, err := b.db.PruneFoundrySlack(ctx, b.cfg.Retention.Intake, b.cfg.Retention.Replies, sweepPrune); {
	case err != nil:
		sweeps.WithLabelValues("prune", "error").Inc()
		log.From(ctx).Error("prune Slack's rows failed", "err", err)
	default:
		sweeps.WithLabelValues("prune", "pruned").Add(float64(n))
	}
}

// uninstall tells Slack a revoked install is gone, with its token, and
// records that it knows, which deletes the token: a token Slack no longer
// takes means Slack knows already. Slack refusing the call for good (any
// other code but its own faults, such as no_permission) or a failure past
// uninstallPatience after the revoke records it as known too, so its team
// may install the app again; the app then stays in that Slack until its
// admin removes it. The app's credentials refused are logged as an error:
// an operator fixes them. It names the outcome: uninstalled, abandoned
// (recorded without Slack's word), deferred (a rate limit), or error,
// tried at the next sweep.
func (b *Bot) uninstall(ctx context.Context, install *model.SlackInstall) string {
	logger := log.From(ctx).With("install_id", install.ID)
	bot, err := b.token(ctx, install.ID)
	if err == nil {
		err = b.slack.Uninstall(ctx, bot, b.cfg.ClientID, b.cfg.ClientSecret)
	}
	if _, limited := rateLimited(err); limited {
		return "deferred"
	}
	outcome := "uninstalled"
	credentials := slack.IsCode(err, "bad_client_secret", "invalid_client_id", "client_id_token_mismatch")
	switch {
	case err == nil || errors.Is(err, store.ErrNotFound) ||
		slack.IsCode(err, "invalid_auth", "account_inactive", "token_revoked", "token_expired", "not_authed"):
	case slackRefused(err) && !credentials:
		logger.Warn("Slack refused apps.uninstall for good; the install is taken as uninstalled and its team's admin removes the app in Slack",
			"err", err)
		outcome = "abandoned"
	case install.RevokedAt != nil && b.now().Sub(*install.RevokedAt) >= uninstallPatience:
		logger.Error("slack apps.uninstall kept failing; the install is taken as uninstalled and its team's admin removes the app in Slack",
			"err", err)
		outcome = "abandoned"
	case credentials:
		logger.Error("Slack refused the app's credentials for apps.uninstall: check foundry.slack.client_id and client_secret; "+
			"the next sweep tries again", "err", err)
		return "error"
	default:
		logger.Warn("slack apps.uninstall failed; the next sweep tries again", "err", err)
		return "error"
	}
	if err := b.db.MarkFoundrySlackUninstalled(ctx, install.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		logger.Warn("record a Slack uninstall failed; the next sweep tries again", "err", err)
		return "error"
	}
	return outcome
}
