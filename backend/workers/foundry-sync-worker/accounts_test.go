package foundrysyncworker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/ports/portstest"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// accountRows keeps what the accounts tick reads and writes, as the store
// does: purges due by their wait, connectors due a check by the time a
// token was last vended, and what each call did, in order.
type accountRows struct {
	purges     []*model.ConnectorAccountPurge
	waits      map[string]time.Duration
	connectors []*model.Connector
	checkedAt  map[string]time.Time
	breaks     []string
	log        []string
	// signInsAfter is the retention the cleanup was given.
	signInsAfter time.Duration
}

func (r *accountRows) DeleteEndedConnectorSignIns(_ context.Context, after time.Duration, _ int) (int, error) {
	r.signInsAfter = after
	return 0, nil
}

func (r *accountRows) ListConnectorAccountPurges(context.Context, int) ([]*model.ConnectorAccountPurge, error) {
	var due []*model.ConnectorAccountPurge
	for _, p := range r.purges {
		if r.waits[p.ID] == 0 {
			due = append(due, p)
		}
	}
	return due, nil
}

func (r *accountRows) DeleteConnectorAccountPurge(_ context.Context, id string) error {
	r.log = append(r.log, "forget "+id)
	r.purges = slices.DeleteFunc(r.purges, func(p *model.ConnectorAccountPurge) bool { return p.ID == id })
	return nil
}

func (r *accountRows) RetryConnectorAccountPurge(_ context.Context, id, _ string, after time.Duration) error {
	r.log = append(r.log, fmt.Sprintf("retry %s in %s", id, after))
	r.waits[id] = after
	return nil
}

func (r *accountRows) ConnectorAccountPurgeBacklog(context.Context) (int, *time.Time, error) {
	return len(r.purges), nil, nil
}

func (r *accountRows) ListConnectorAccountChecks(_ context.Context, before time.Time, _ int) ([]*model.Connector, error) {
	var due []*model.Connector
	for _, c := range r.connectors {
		if c.Account.Broken == nil && r.checkedAt[c.ID].Before(before) {
			due = append(due, c)
		}
	}
	return due, nil
}

func (r *accountRows) BreakConnectorAccount(_ context.Context, _, connectorID string, generation int, reason string) (bool, error) {
	r.breaks = append(r.breaks, fmt.Sprintf("%s@%d:%s", connectorID, generation, reason))
	return true, nil
}

func (r *accountRows) MarkConnectorAccountChecked(_ context.Context, _, connectorID string) error {
	r.checkedAt[connectorID] = now
	return nil
}

func (r *accountRows) DeferConnectorAccountCheck(_ context.Context, _, connectorID string, checkedAt time.Time) error {
	r.log = append(r.log, "defer "+connectorID)
	if checkedAt.After(r.checkedAt[connectorID]) {
		r.checkedAt[connectorID] = checkedAt
	}
	return nil
}

func signedIn(id, account string, checked time.Time, rows *accountRows) *model.Connector {
	rows.checkedAt[id] = checked
	return &model.Connector{ID: id, WorkspaceID: "ws-1", Kind: "github",
		Account: &model.ConnectorAccount{ID: account, UserID: "user-1", Generation: 2}}
}

func TestKeepAccounts(t *testing.T) {
	ctx := context.Background()
	newAccountTask := func(broker *portstest.Accounts) (*Task, *accountRows) {
		rows := &accountRows{waits: map[string]time.Duration{}, checkedAt: map[string]time.Time{}}
		task := newTask(newQueue(), done(0))
		task.accounts, task.broker = rows, broker
		return task, rows
	}

	t.Run("does nothing where no connector signs in", func(t *testing.T) {
		task, rows := newAccountTask(nil)
		task.broker = nil
		rows.purges = []*model.ConnectorAccountPurge{{ID: "p-1", AccountID: "acct-1"}}
		task.keepAccounts(ctx)
		if len(rows.log) != 0 || rows.signInsAfter != 0 {
			t.Fatalf("log = %v", rows.log)
		}
	})

	// A purge the broker confirms goes; one it can't reach waits, doubling,
	// and the pass stops.
	t.Run("deletes purged accounts from the broker, backing off", func(t *testing.T) {
		broker := &portstest.Accounts{}
		task, rows := newAccountTask(broker)
		broker.Add(ports.AccountRef{WorkspaceID: "ws-1", UserID: "user-1", Kind: "github", AccountID: "acct-1"})
		rows.purges = []*model.ConnectorAccountPurge{
			{ID: "p-1", WorkspaceID: "ws-1", Kind: "github", AccountID: "acct-1", UserID: "user-1"},
			{ID: "p-2", WorkspaceID: "ws-1", Kind: "github", AccountID: "acct-gone", UserID: "user-1"},
		}
		task.keepAccounts(ctx)
		if broker.Has("acct-1") || len(rows.purges) != 0 || rows.signInsAfter != 24*time.Hour {
			t.Fatalf("purges = %v, broker has acct-1 = %v, sign-ins after %s", rows.purges, broker.Has("acct-1"), rows.signInsAfter)
		}

		broker.Unavailable = true
		rows.purges = []*model.ConnectorAccountPurge{{ID: "p-3", Attempts: 2}, {ID: "p-4"}}
		task.keepAccounts(ctx)
		if want := []string{"forget p-1", "forget p-2", "retry p-3 in 4m0s"}; !slices.Equal(rows.log, want) {
			t.Fatalf("log = %v, want %v", rows.log, want)
		}
		rows.waits = map[string]time.Duration{}
		rows.purges = []*model.ConnectorAccountPurge{{ID: "p-5", Attempts: 40}}
		task.keepAccounts(ctx)
		if rows.waits["p-5"] != purgeBackoffMax {
			t.Fatalf("wait = %s, want the cap", rows.waits["p-5"])
		}
	})

	// The weekly check keeps the broker's grant from lapsing, and finds an
	// account that broke outside Foundry.
	t.Run("checks each account not vended for a week", func(t *testing.T) {
		broker := &portstest.Accounts{}
		task, rows := newAccountTask(broker)
		for _, id := range []string{"acct-1", "acct-2"} {
			broker.Add(ports.AccountRef{WorkspaceID: "ws-1", UserID: "user-1", Kind: "github", AccountID: id})
		}
		broker.Break("acct-2", ports.AccountMemberLeft)
		rows.connectors = []*model.Connector{
			signedIn("c-stale", "acct-1", now.Add(-8*24*time.Hour), rows),
			signedIn("c-left", "acct-2", now.Add(-8*24*time.Hour), rows),
			signedIn("c-fresh", "acct-3", now.Add(-time.Hour), rows),
		}
		task.keepAccounts(ctx)
		if broker.Calls("Token") != 2 || !rows.checkedAt["c-stale"].Equal(now) {
			t.Fatalf("tokens = %d, c-stale checked at %v", broker.Calls("Token"), rows.checkedAt["c-stale"])
		}
		if !slices.Equal(rows.breaks, []string{"c-left@2:member_left"}) {
			t.Fatalf("breaks = %v", rows.breaks)
		}

	})

	// An account the broker can't reach, or fails on, is checked again an
	// hour later, and the accounts after it are checked meanwhile.
	t.Run("checks past an account that fails, and again an hour later", func(t *testing.T) {
		broker := &portstest.Accounts{}
		task, rows := newAccountTask(broker)
		for _, id := range []string{"acct-down", "acct-odd", "acct-ok"} {
			broker.Add(ports.AccountRef{WorkspaceID: "ws-1", UserID: "user-1", Kind: "github", AccountID: id})
		}
		broker.Fail("acct-down", fmt.Errorf("pipes: %w", ports.ErrAccountsUnavailable))
		broker.Fail("acct-odd", errors.New("pipes: an answer it doesn't know"))
		week := now.Add(-8 * 24 * time.Hour)
		rows.connectors = []*model.Connector{
			signedIn("c-down", "acct-down", week, rows), signedIn("c-odd", "acct-odd", week, rows), signedIn("c-ok", "acct-ok", week, rows),
		}
		task.keepAccounts(ctx)
		if !rows.checkedAt["c-ok"].Equal(now) || !slices.Equal(rows.log, []string{"defer c-down", "defer c-odd"}) {
			t.Fatalf("c-ok checked at %v, log %v", rows.checkedAt["c-ok"], rows.log)
		}
		tokens := broker.Calls("Token")
		task.keepAccounts(ctx)
		if broker.Calls("Token") != tokens {
			t.Fatalf("tokens %d → %d: a failed check waits an hour", tokens, broker.Calls("Token"))
		}
		task.now = func() time.Time { return now.Add(accountCheckRetry + time.Minute) }
		task.keepAccounts(ctx)
		if broker.Calls("Token") != tokens+2 {
			t.Fatalf("tokens %d → %d: the failed checks come again an hour later", tokens, broker.Calls("Token"))
		}
	})

	// A broker that answers unavailable time after time is likely down: the
	// tick stops asking, and breaks nothing.
	t.Run("stops checking while the broker is down", func(t *testing.T) {
		broker := &portstest.Accounts{}
		task, rows := newAccountTask(broker)
		for i := range 5 {
			id := fmt.Sprintf("acct-%d", i)
			broker.Add(ports.AccountRef{WorkspaceID: "ws-1", UserID: "user-1", Kind: "github", AccountID: id})
			rows.connectors = append(rows.connectors, signedIn("c-"+id, id, now.Add(-8*24*time.Hour), rows))
		}
		broker.Unavailable = true
		task.keepAccounts(ctx)
		if broker.Calls("Token") != maxUnavailableChecks || len(rows.breaks) != 0 {
			t.Fatalf("tokens = %d, breaks %v", broker.Calls("Token"), rows.breaks)
		}
	})
}
