package syncer

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/ports/portstest"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// rejectingKind is a fakeKind whose provider rejects the tokens in rejects,
// as GitHub answers 401; onReject runs at each rejection.
type rejectingKind struct {
	*fakeKind
	rejects  map[string]bool
	onReject func()
	// onDiscover runs at each discovery, as the connector is signed in
	// again during a sync.
	onDiscover func()
}

func (k *rejectingKind) Discover(ctx context.Context, src connector.Source) (connector.Discovery, error) {
	if k.onDiscover != nil {
		k.onDiscover()
	}
	if k.rejects[src.Secret] {
		if k.onReject != nil {
			k.onReject()
		}
		return connector.Discovery{}, &connector.RefusedError{Message: "Fake rejected this token; check it and enter it again",
			Err: &connector.AuthError{Provider: "Fake"}}
	}
	return k.fakeKind.Discover(ctx, src)
}

// signedInFixture is a fixture whose connector reads with account acct-1,
// generation 3, as user-1, through broker.
func signedInFixture(t *testing.T, kind connector.Kind, broker *portstest.Accounts) fixture {
	t.Helper()
	f := newFixture(t, "http://unused", "fake", `{}`, nil)
	f.syncer.Catalog = connector.NewCatalogOf(kind)
	c := f.store.connectors["conn-1"]
	c.SecretCiphertext, c.SecretKeyID = nil, ""
	c.Account = &model.ConnectorAccount{ID: "acct-1", UserID: "user-1", Generation: 3}
	if broker != nil {
		broker.Add(ports.AccountRef{WorkspaceID: "ws-1", UserID: "user-1", Kind: "fake", AccountID: "acct-1"})
		f.syncer.Accounts = broker
	}
	return f
}

func signedInKind() *fakeKind {
	return &fakeKind{discovery: connector.Discovery{Tables: []connector.Table{{Name: "orders", PrimaryKey: []string{"id"}}}},
		rows: map[string][]string{"orders": {`{"id":1}`}}}
}

func TestSignedInSyncs(t *testing.T) {
	ctx := context.Background()

	// The token is the broker's, asked for on every sync, and never stored.
	t.Run("asks the broker for a token on every sync", func(t *testing.T) {
		broker := &portstest.Accounts{}
		kind := signedInKind()
		f := signedInFixture(t, kind, broker)
		for range 2 {
			if _, err := f.syncer.Run(ctx, f.sync); err != nil {
				t.Fatal(err)
			}
			broker.Rotate("acct-1")
		}
		if broker.Calls("Token") != 2 || f.store.checked != 2 {
			t.Fatalf("tokens = %d, checked = %d; want one each per sync", broker.Calls("Token"), f.store.checked)
		}
		var secrets []string
		for _, src := range kind.sources {
			if !src.SignIn {
				t.Fatalf("source %+v isn't marked signed in", src)
			}
			secrets = append(secrets, src.Secret)
		}
		if !slices.Contains(secrets, "token-acct-1-1") || !slices.Contains(secrets, "token-acct-1-2") {
			t.Fatalf("secrets = %v, want each sync's own token", secrets)
		}
	})

	// The sync reports the account generation it read with, and a cursor
	// it stores after the connector was signed in again is dropped.
	t.Run("reads at one account generation", func(t *testing.T) {
		broker := &portstest.Accounts{}
		kind := &rejectingKind{fakeKind: signedInKind()}
		f := signedInFixture(t, kind, broker)
		kind.onDiscover = func() {
			f.store.connectors["conn-1"].Account = &model.ConnectorAccount{ID: "acct-2", UserID: "user-2", Generation: 4}
		}
		res, err := f.syncer.Run(ctx, f.sync)
		if err != nil || res.AccountGeneration == nil || *res.AccountGeneration != 3 || len(f.store.discarded) != 1 {
			t.Fatalf("Run = %+v, %v; discarded %v", res, err, f.store.discarded)
		}
	})

	// Without a broker, or with one that can't be reached, a signed-in
	// connector waits: a pause would stop it for good.
	t.Run("retries when no broker can vend its token", func(t *testing.T) {
		for name, broker := range map[string]*portstest.Accounts{"no broker": nil, "unreachable": {Unavailable: true}} {
			f := signedInFixture(t, signedInKind(), broker)
			if broker != nil {
				broker.Unavailable = true
			}
			_, err := f.syncer.Run(ctx, f.sync)
			if fl := failure(t, err); fl.Permanent || !strings.Contains(fl.Message, "Helix will retry") {
				t.Fatalf("%s: failure = %+v, want a retry", name, fl)
			}
			if len(f.store.breaks) != 0 {
				t.Fatalf("%s: broke %v", name, f.store.breaks)
			}
		}
	})

	t.Run("freezes an account the broker says is broken", func(t *testing.T) {
		broker := &portstest.Accounts{}
		kind := signedInKind()
		f := signedInFixture(t, kind, broker)
		broker.Break("acct-1", ports.AccountMemberLeft)
		_, err := f.syncer.Run(ctx, f.sync)
		want := "Fake needs signing in again: the member who connected it left the workspace. Its data is frozen as of its last sync. " +
			"An owner or admin signs it in again from its page."
		if fl := failure(t, err); !fl.Permanent || fl.Message != want {
			t.Fatalf("failure = %+v, want %q", fl, want)
		}
		if !slices.Equal(f.store.breaks, []string{"conn-1@3:member_left"}) || len(kind.sources) != 0 {
			t.Fatalf("breaks = %v, sources = %d", f.store.breaks, len(kind.sources))
		}

		// Foundry's record decides from then on: the broker isn't asked.
		tokens := broker.Calls("Token")
		_, err = f.syncer.Run(ctx, f.sync)
		if fl := failure(t, err); !fl.Permanent || fl.Message != want || broker.Calls("Token") != tokens {
			t.Fatalf("failure = %+v, tokens %d → %d", fl, tokens, broker.Calls("Token"))
		}
	})

	// A sync that asked before the connector was signed in again breaks
	// nothing: it retries, with the account it reads with now.
	t.Run("leaves an account signed in again since the sync asked", func(t *testing.T) {
		broker := &portstest.Accounts{}
		f := signedInFixture(t, signedInKind(), broker)
		broker.Break("acct-1", ports.AccountAccessRevoked)
		f.syncer.DB = &resignedStore{memoryStore: f.store}
		_, err := f.syncer.Run(ctx, f.sync)
		if fl := failure(t, err); fl.Permanent || len(f.store.breaks) != 0 {
			t.Fatalf("failure = %+v, breaks = %v; want a retry and no break", fl, f.store.breaks)
		}
	})

	// A token that ran out mid-sync is renewed by the broker: one fresh
	// token recovers the sync, and two rejections break the account.
	t.Run("asks once more when the provider rejects a token", func(t *testing.T) {
		broker := &portstest.Accounts{}
		kind := &rejectingKind{fakeKind: signedInKind(), rejects: map[string]bool{"token-acct-1-1": true}}
		kind.onReject = func() { broker.Rotate("acct-1") }
		f := signedInFixture(t, kind, broker)
		res, err := f.syncer.Run(ctx, f.sync)
		if err != nil || res.Rows != 1 {
			t.Fatalf("Run = %+v, %v; want the fresh token to sync", res, err)
		}
		if broker.Calls("Token") != 2 || len(f.store.breaks) != 0 {
			t.Fatalf("tokens = %d, breaks = %v", broker.Calls("Token"), f.store.breaks)
		}

		kind.rejects["token-acct-1-2"], kind.rejects["token-acct-1-3"] = true, true
		_, err = f.syncer.Run(ctx, f.sync)
		if fl := failure(t, err); !fl.Permanent || !strings.Contains(fl.Message, "no longer accepts the access it signed in with") {
			t.Fatalf("failure = %+v, want access revoked", fl)
		}
		if !slices.Equal(f.store.breaks, []string{"conn-1@3:access_revoked"}) {
			t.Fatalf("breaks = %v", f.store.breaks)
		}
	})

	// An empty token is one no provider takes: the sync asks once more, and
	// a second empty one breaks the account as a second rejection does.
	t.Run("asks once more for a token after an empty one", func(t *testing.T) {
		broker := &portstest.Accounts{}
		kind := signedInKind()
		f := signedInFixture(t, kind, broker)
		broker.Empty("acct-1", 1)
		if _, err := f.syncer.Run(ctx, f.sync); err != nil || len(kind.sources) == 0 || strings.TrimSpace(kind.sources[0].Secret) == "" {
			t.Fatalf("Run = %v, sources %+v: want the second token used", err, kind.sources)
		}
		broker.Empty("acct-1", 2)
		_, err := f.syncer.Run(ctx, f.sync)
		if fl := failure(t, err); !fl.Permanent || !slices.Equal(f.store.breaks, []string{"conn-1@3:access_revoked"}) {
			t.Fatalf("failure = %+v, breaks = %v", fl, f.store.breaks)
		}
	})

	// A fresh token, asked for after a rejection, that is empty is a second
	// rejection.
	t.Run("breaks the account when the fresh token is empty", func(t *testing.T) {
		broker := &portstest.Accounts{}
		kind := &rejectingKind{fakeKind: signedInKind(), rejects: map[string]bool{"token-acct-1-1": true}}
		kind.onReject = func() { broker.Empty("acct-1", 1) }
		f := signedInFixture(t, kind, broker)
		_, err := f.syncer.Run(ctx, f.sync)
		if fl := failure(t, err); !fl.Permanent || !slices.Equal(f.store.breaks, []string{"conn-1@3:access_revoked"}) {
			t.Fatalf("failure = %+v, breaks = %v", fl, f.store.breaks)
		}
	})

	// A key connector's rejected key is a refusal, retried, as before.
	t.Run("retries a key connector's rejected key", func(t *testing.T) {
		f := newFakeFixture(t, nil, `{}`)
		f.syncer.Catalog = connector.NewCatalogOf(&rejectingKind{fakeKind: signedInKind(), rejects: map[string]bool{"sk_test_abc": true}})
		_, err := f.syncer.Run(ctx, f.sync)
		var authErr *connector.AuthError
		if fl := failure(t, err); fl.Permanent || !errors.As(err, &authErr) || len(f.store.breaks) != 0 {
			t.Fatalf("failure = %+v", fl)
		}
	})
}

// resignedStore is a store whose connector was signed in again after the
// sync read it: a break at the generation the sync asked with breaks nothing.
type resignedStore struct{ *memoryStore }

func (s *resignedStore) BreakConnectorAccount(context.Context, string, string, int, string) (bool, error) {
	return false, nil
}

func TestAccountBrokenMessage(t *testing.T) {
	for reason, want := range map[string]string{
		ports.AccountMemberLeft:     "the member who connected it left the workspace",
		ports.AccountAccessRevoked:  "Linear no longer accepts the access it signed in with",
		ports.AccountExpired:        "its access lapsed and couldn't be renewed",
		ports.AccountAccountMissing: "the account it signed in with is gone",
	} {
		if got := AccountBrokenMessage("Linear", reason); !strings.HasPrefix(got, "Linear needs signing in again: "+want+". ") {
			t.Fatalf("%s: %q", reason, got)
		}
	}
}
