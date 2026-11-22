package slackbot

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/connector"
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

// Sweep does what no message starts (the spec's §8.2, §10.3 and §12, and
// the connector spec's §5.4, §7.3, §7.5, §11 and §12): it tells the
// conversations waiting for an offline linked install that they wait, and
// those whose deliveries expired that they weren't delivered, and turns
// the channel messages a relayed install never took into gaps (relay); it
// tells Slack of the
// uninstalls it wasn't told of yet (apps.uninstall), relayed installs of a
// revoked link among them, so their teams may install again; revokes the
// keys of revoked installs' bots, with their source grants; purges the Slack-born Founder threads idle past
// foundry.slack.retention.threads or of a revoked install; prunes the
// intake and replies past foundry.slack.retention.intake and .replies, the
// link tokens and install states Slack's terms let go, and the feeds'
// buffer rows the mirror holds, and what unbound feeds held; deletes the
// connectors of installs removed past foundry.slack.feed.orphan_retention;
// and, with the feed on, flips staged switches whose backfill is done and
// closes yesterday's days once a day. Each step goes on whatever another's
// failure; each is counted.
func (b *Bot) Sweep(ctx context.Context) {
	b.relay(ctx)
	installs, err := b.db.ListFoundrySlackUninstalls(ctx, sweepUninstalls)
	if err != nil {
		sweeps.WithLabelValues("uninstall", "error").Inc()
		log.From(ctx).Error("list the Slack installs to uninstall failed", "err", err)
	}
	for _, install := range installs {
		sweeps.WithLabelValues("uninstall", b.uninstall(ctx, install)).Inc()
	}
	if b.botKeys != nil {
		bots, err := b.db.ListFoundrySlackRetiredBots(ctx, sweepUninstalls)
		if err != nil {
			sweeps.WithLabelValues("bot_key", "error").Inc()
			log.From(ctx).Error("list the revoked Slack installs' bots failed", "err", err)
		}
		for _, install := range bots {
			sweeps.WithLabelValues("bot_key", b.retireBot(ctx, install)).Inc()
		}
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
	switch n, err := b.db.PruneFoundrySlackFeed(ctx, sweepFeedRows); {
	case err != nil:
		sweeps.WithLabelValues("prune_feed", "error").Inc()
		log.From(ctx).Error("prune Slack feeds' rows failed", "err", err)
	default:
		sweeps.WithLabelValues("prune_feed", "pruned").Add(float64(n))
	}
	if b.cfg.Feed.OrphanRetention > 0 {
		b.orphans(ctx)
	}
	if b.feed != nil {
		b.feeds(ctx)
	}
}

// orphans deletes the Slack connectors whose install was removed more than
// foundry.slack.feed.orphan_retention ago (their data frozen since, as
// their page said), through the connector delete, which takes their
// documents out of every load. A staged switch, which still syncs by its
// token, reads by its token alone instead: its feed is unbound. A
// connector with a sync live waits for the next sweep.
func (b *Bot) orphans(ctx context.Context) {
	feeds, err := b.db.ListFoundrySlackOrphans(ctx, b.cfg.Feed.OrphanRetention, sweepOrphans)
	if err != nil {
		sweeps.WithLabelValues("orphan", "error").Inc()
		log.From(ctx).Error("list the Slack feeds of removed installs failed", "err", err)
		return
	}
	for _, feed := range feeds {
		sweeps.WithLabelValues("orphan", b.orphan(ctx, feed)).Inc()
	}
}

// orphan ends one feed of a removed install, and names how: deleted,
// unbound (a staged switch), busy (a sync live), gone or error.
func (b *Bot) orphan(ctx context.Context, feed *model.SlackFeed) string {
	logger := log.From(ctx).With("feed", feed.ID, "connector_id", feed.ConnectorID, "workspace_id", feed.WorkspaceID)
	c, err := b.db.GetConnector(ctx, feed.WorkspaceID, feed.ConnectorID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "gone"
	case err != nil:
		logger.Warn("read a removed Slack install's connector failed", "err", err)
		return "error"
	}
	switch _, err := b.db.GetActiveConnectorSync(ctx, c.WorkspaceID, c.ID); {
	case err == nil:
		return "busy"
	case !errors.Is(err, store.ErrNotFound):
		logger.Warn("read a removed Slack install's connector's sync failed", "err", err)
		return "error"
	}
	var s connector.SlackSettings
	if err := json.Unmarshal(c.Config, &s); err != nil {
		logger.Warn("a Slack connector's settings can't be read", "err", err)
		return "error"
	}
	if s.Staged {
		token, _ := json.Marshal(connector.SlackSettings{HistoryDays: s.HistoryDays}) // plain fields
		err = b.db.UpdateSlackAppConnector(ctx, c.WorkspaceID, c.ID, model.ConnectorUpdate{Config: token, SecretCiphertext: c.SecretCiphertext,
			SecretKeyID: c.SecretKeyID, SecretHint: c.SecretHint}, model.SlackAppUnbind{})
	} else {
		err = b.db.DeleteConnector(ctx, c.WorkspaceID, c.ID)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "gone"
	case err != nil:
		logger.Warn("end a removed Slack install's connector failed; the next sweep tries again", "err", err)
		return "error"
	case s.Staged:
		logger.Info("a staged switch to a removed Slack install reads by its token again")
		return "unbound"
	}
	logger.Info("deleted a Slack connector whose install was removed past its retention")
	return "deleted"
}

// feeds goes through the live feeds of live installs: each staged switch
// whose windows and threads are read flips to the app (flip), and,
// once a day at foundry.slack.feed.close_at past midnight UTC, yesterday's
// days close and every pushed connector syncs, so their documents are
// offered.
func (b *Bot) feeds(ctx context.Context) {
	day := model.SlackDay(b.now().Add(-b.cfg.Feed.CloseAt))
	b.mu.Lock()
	closing := day.After(b.closed)
	b.mu.Unlock()
	failed := false
	for after := ""; ; {
		feeds, err := b.db.ListFoundrySlackFeeds(ctx, after, sweepFeeds)
		if err != nil {
			sweeps.WithLabelValues("feeds", "error").Inc()
			log.From(ctx).Error("list the Slack feeds failed", "err", err)
			return
		}
		for _, feed := range feeds {
			if closing {
				switch _, err := b.db.CloseFoundrySlackDays(ctx, feed.ID, day, b.cfg.Feed.Spacing); {
				case err != nil && !errors.Is(err, store.ErrNotFound):
					failed = true
					sweeps.WithLabelValues("close", "error").Inc()
					log.From(ctx).Warn("close a Slack feed's days failed; the next sweep tries again", "feed", feed.ID, "err", err)
				default:
					sweeps.WithLabelValues("close", "closed").Inc()
				}
			}
			if outcome := b.flip(ctx, feed); outcome != "" {
				sweeps.WithLabelValues("flip", outcome).Inc()
			}
		}
		if len(feeds) < sweepFeeds {
			break
		}
		after = feeds[len(feeds)-1].ID
	}
	if closing && !failed {
		b.mu.Lock()
		b.closed = day
		b.mu.Unlock()
	}
}

// flip ends feed's connector's staged switch once the backfill read what
// its windows (bound, joined and gap ones) and threads hold, so its mirror
// covers its window: its days are closed and folded, then, unless that
// fold found channels to read, the connector reads through the app,
// in full (store.FlipSlackAppConnector). It names how it went: flipped,
// waiting (a sync live, or the settings changed meanwhile) or error; ""
// for a connector not staged, or whose backfill is still reading.
func (b *Bot) flip(ctx context.Context, feed *model.SlackFeed) string {
	logger := log.From(ctx).With("feed", feed.ID, "connector_id", feed.ConnectorID)
	c, err := b.db.GetConnector(ctx, feed.WorkspaceID, feed.ConnectorID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return ""
	case err != nil:
		logger.Warn("read a Slack feed's connector failed", "err", err)
		return "error"
	}
	var s connector.SlackSettings
	if json.Unmarshal(c.Config, &s) != nil || !s.Staged {
		return ""
	}
	// Each day a window still reads is partial, so the flip's full read
	// would give it no document: it waits for every window, whatever its
	// reason, and asks again once the settle's fold has queued the joined
	// windows of the channels it found.
	backfilling := func() (bool, string) {
		progress, err := b.db.FoundrySlackBackfillProgress(ctx, feed.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return true, ""
		case err != nil:
			logger.Warn("read a Slack feed's backfill failed", "err", err)
			return true, "error"
		}
		live := progress.QueuedThreads > 0 || progress.LiveGaps > 0 || slices.ContainsFunc(progress.Windows, func(w model.SlackBackfillWindow) bool {
			return w.Status == model.SlackBackfillQueued || w.Status == model.SlackBackfillRunning
		})
		return live, ""
	}
	if waiting, outcome := backfilling(); waiting {
		return outcome
	}
	install, err := b.db.GetFoundrySlackInstallByID(ctx, feed.InstallID)
	if err != nil {
		logger.Warn("read a Slack feed's install failed", "err", err)
		return "error"
	}
	b.settle(ctx, feed, b.appOf(install))
	if waiting, outcome := backfilling(); waiting {
		return outcome
	}
	s.Staged = false
	flipped, _ := json.Marshal(s) // plain fields
	sealed, keyID, err := b.keys.Seal([]byte(""), connector.SecretContext(c.ID))
	if err != nil {
		logger.Warn("seal a flipped Slack connector's empty secret failed", "err", err)
		return "error"
	}
	switch err := b.db.FlipSlackAppConnector(ctx, c.WorkspaceID, c.ID, c.Config, flipped, sealed, keyID); {
	case errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound):
		return "waiting"
	case err != nil:
		logger.Warn("flip a staged Slack connector failed; the next sweep tries again", "err", err)
		return "error"
	}
	logger.Info("a staged Slack connector reads through Helix's Slack app now")
	return "flipped"
}

// retireBot revokes the key of a revoked install's bot, which Slack's
// uninstall, the worker's revoke of a gone workspace, a purge or a
// disconnect left on it: the host revokes it, its source grants are
// dropped, then the install forgets it, which lifts the host's lock on it.
// It names the outcome: revoked, or error, tried at the next sweep.
func (b *Bot) retireBot(ctx context.Context, install *model.SlackInstall) string {
	bot, ok := install.Mode.(model.SlackSharedBot)
	if !ok {
		log.From(ctx).Error("the store listed a revoked Slack install with no bot key as a retired bot", "install_id", install.ID)
		return "error"
	}
	logger := log.From(ctx).With("install_id", install.ID, "credential_id", bot.CredentialID)
	if err := b.botKeys.Revoke(ctx, install.WorkspaceID, bot.CredentialID); err != nil {
		logger.Warn("revoke a revoked Slack install's bot key failed; the next sweep tries again", "err", err)
		return "error"
	}
	err := b.db.DeleteFoundryAccessPrincipal(ctx, install.WorkspaceID, access.Principal{Kind: access.PrincipalServiceCredential, ID: bot.CredentialID})
	if err == nil {
		err = b.db.ForgetFoundrySlackBot(ctx, install.ID, bot.CredentialID)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		logger.Warn("forget a revoked Slack install's bot key failed; the next sweep tries again", "err", err)
		return "error"
	}
	logger.Info("revoked the bot key of a revoked Slack install")
	return "revoked"
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
