package foundrysyncworker

import (
	"context"
	"errors"
	"time"

	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/helixdb/helix-foundry/backend/syncer"
)

// accountQueue is the store's side of signed-in connectors' accounts.
type accountQueue interface {
	DeleteEndedConnectorSignIns(ctx context.Context, after time.Duration, limit int) (int, error)
	ListConnectorAccountPurges(ctx context.Context, limit int) ([]*model.ConnectorAccountPurge, error)
	DeleteConnectorAccountPurge(ctx context.Context, id string) error
	RetryConnectorAccountPurge(ctx context.Context, id, lastError string, after time.Duration) error
	ConnectorAccountPurgeBacklog(ctx context.Context) (int, *time.Time, error)
	ListConnectorAccountChecks(ctx context.Context, before time.Time, limit int) ([]*model.Connector, error)
	BreakConnectorAccount(ctx context.Context, workspaceID, connectorID string, generation int, reason string) (bool, error)
	MarkConnectorAccountChecked(ctx context.Context, workspaceID, connectorID string) error
	DeferConnectorAccountCheck(ctx context.Context, workspaceID, connectorID string, checkedAt time.Time) error
}

const (
	// signInRetention is how long a sign-in's row outlives its end.
	signInRetention = 24 * time.Hour
	// accountCheckEvery is how long an account goes without a token vended
	// before the check asks for one; accountCheckRetry how long after a check
	// that got none the next comes.
	accountCheckEvery = 7 * 24 * time.Hour
	accountCheckRetry = time.Hour
	// maxUnavailableChecks is how many checks in a row the broker may answer
	// unavailable before the tick stops asking: it is likely down.
	maxUnavailableChecks = 3
	// purgeBackoffMax caps the wait between a purge's attempts, which
	// doubles from a minute.
	purgeBackoffMax = 6 * time.Hour
)

// keepAccounts looks after signed-in connectors' accounts, each schedule
// tick, beside the schedule. Where no broker is (Deps.Accounts nil) no
// connector signs in, and it does nothing.
//
//  1. Sign-ins that ended a day ago go; one that holds the broker's account
//     unused (its finish failed or never ended) purges it first.
//  2. Due purges delete their accounts from the broker. One that fails is
//     tried again later, its wait doubling from a minute to six hours.
//  3. Each live account the broker hasn't vended a token for in a week,
//     paused and manual-only connectors' included, is asked for one. That
//     keeps what the broker holds from lapsing (GitHub's refresh token
//     lasts six months unused), and finds an account that broke outside
//     Foundry, such as a membership removed in WorkOS or access revoked at
//     the provider, which is recorded broken as a sync records it.
//
// A broker that can't be reached ends step 2 until the next tick, and step 3
// after maxUnavailableChecks answers in a row; what they leave stays due. A
// check that gets no token is tried again an hour later, so one account
// that keeps failing never holds back the rest.
func (t *Task) keepAccounts(ctx context.Context) {
	if t.broker == nil {
		return
	}
	if n, err := t.accounts.DeleteEndedConnectorSignIns(ctx, signInRetention, sweepBatch); err != nil {
		log.From(ctx).Error("delete ended connector sign-ins failed", "err", err)
	} else {
		signInsDeleted.Add(float64(n))
	}
	t.purgeAccounts(ctx)
	t.checkAccounts(ctx)
}

// purgeAccounts deletes due purges' accounts from the broker.
func (t *Task) purgeAccounts(ctx context.Context) {
	purges, err := t.accounts.ListConnectorAccountPurges(ctx, sweepBatch)
	if err != nil {
		log.From(ctx).Error("list connector account purges failed", "err", err)
		return
	}
	for _, p := range purges {
		err := t.broker.Delete(ctx, ports.AccountRef{WorkspaceID: p.WorkspaceID, UserID: p.UserID, Kind: p.Kind, AccountID: p.AccountID})
		if err == nil {
			if err := t.accounts.DeleteConnectorAccountPurge(ctx, p.ID); err != nil {
				log.From(ctx).Warn("forget connector account purge failed", "purge", p.ID, "err", err)
				continue
			}
			accountPurges.WithLabelValues("deleted").Inc()
			log.From(ctx).Info("purged a connector account", "workspace", p.WorkspaceID, "kind", p.Kind, "account", p.AccountID,
				"user", p.UserID)
			continue
		}
		accountPurges.WithLabelValues("failed").Inc()
		wait := min(time.Minute<<min(p.Attempts, 16), purgeBackoffMax)
		log.From(ctx).Warn("purge a connector account failed", "workspace", p.WorkspaceID, "kind", p.Kind, "account", p.AccountID,
			"user", p.UserID, "attempts", p.Attempts+1, "retry_in", wait, "err", err)
		if rerr := t.accounts.RetryConnectorAccountPurge(ctx, p.ID, err.Error(), wait); rerr != nil {
			log.From(ctx).Warn("retry connector account purge failed", "purge", p.ID, "err", rerr)
		}
		if errors.Is(err, ports.ErrAccountsUnavailable) {
			break
		}
	}
	n, oldest, err := t.accounts.ConnectorAccountPurgeBacklog(ctx)
	if err != nil {
		log.From(ctx).Warn("read connector account purge backlog failed", "err", err)
		return
	}
	accountPurgeBacklog.Set(float64(n))
	age := 0.0
	if oldest != nil {
		age = t.now().Sub(*oldest).Seconds()
	}
	accountPurgeOldest.Set(age)
}

// checkAccounts asks the broker for a token for each account due its weekly
// check.
func (t *Task) checkAccounts(ctx context.Context) {
	due, err := t.accounts.ListConnectorAccountChecks(ctx, t.now().Add(-accountCheckEvery), sweepBatch)
	if err != nil {
		log.From(ctx).Error("list connector account checks failed", "err", err)
		return
	}
	credentials := syncer.Credentials{Accounts: t.broker, Checked: t.accounts}
	unavailable := 0
	for _, c := range due {
		_, err := credentials.Token(ctx, c)
		var broken *ports.AccountBroken
		switch {
		case err == nil:
			accountChecks.WithLabelValues("vended").Inc()
			unavailable = 0
			continue
		case errors.As(err, &broken):
			unavailable = 0
			broke, berr := t.accounts.BreakConnectorAccount(ctx, c.WorkspaceID, c.ID, c.Account.Generation, broken.Reason)
			if berr == nil {
				accountChecks.WithLabelValues("broken").Inc()
				if broke {
					syncer.CountAccountBreak(broken.Reason)
					log.From(ctx).Warn("the weekly check found a connector's account broken", "workspace", c.WorkspaceID, "connector", c.ID,
						"kind", c.Kind, "user", c.Account.UserID, "account", c.Account.ID, "reason", broken.Reason, "generation", c.Account.Generation)
				}
				continue
			}
			log.From(ctx).Warn("record a broken connector account failed", "connector", c.ID, "err", berr)
		case errors.Is(err, ports.ErrAccountsUnavailable):
			accountChecks.WithLabelValues("unavailable").Inc()
			unavailable++
			log.From(ctx).Warn("check a connector account: the broker is unavailable", "connector", c.ID, "err", err)
		default:
			accountChecks.WithLabelValues("failed").Inc()
			unavailable = 0
			log.From(ctx).Warn("check a connector account failed", "connector", c.ID, "err", err)
		}
		if derr := t.accounts.DeferConnectorAccountCheck(ctx, c.WorkspaceID, c.ID, t.now().Add(accountCheckRetry-accountCheckEvery)); derr != nil {
			log.From(ctx).Warn("defer a connector account check failed", "connector", c.ID, "err", derr)
		}
		if unavailable == maxUnavailableChecks {
			log.From(ctx).Warn("check connector accounts: the broker is unavailable; stopping until the next tick")
			return
		}
	}
}
