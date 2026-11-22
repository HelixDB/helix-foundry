package directory_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/local/directory"
	"github.com/helixdb/helix-foundry/backend/store"
)

// needRelayDB is needDB with no link and no pending link.
func needRelayDB(t *testing.T) *directory.Store {
	t.Helper()
	st := needDB(t)
	for _, table := range []string{"foundry_local_relay", "foundry_local_relay_pending"} {
		if _, err := testDB.Exec("DELETE FROM " + table); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func relayHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func relayLink(id string) directory.RelayLink {
	return directory.RelayLink{HostedOrigin: "https://foundry.helix-db.com", LinkID: id, TokenSealed: []byte("sealed:" + id), KeyID: "k1",
		AccountEmail: "ada@example.com", SlackApprovals: true}
}

func TestRelayLinkIsOneRowTheOwnerControls(t *testing.T) {
	st := needRelayDB(t)
	ctx := context.Background()

	if _, err := st.RelayLink(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unlinked: %v", err)
	}
	if err := st.SetRelaySlackApprovals(ctx, false); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("approvals while unlinked: %v", err)
	}
	if _, err := st.UnlinkRelay(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unlink while unlinked: %v", err)
	}
	for what, bad := range map[string]func(*directory.RelayLink){
		"an http origin":       func(l *directory.RelayLink) { l.HostedOrigin = "http://foundry.helix-db.com" },
		"a path":               func(l *directory.RelayLink) { l.HostedOrigin = "https://foundry.helix-db.com/x" },
		"no host":              func(l *directory.RelayLink) { l.HostedOrigin = "https://" },
		"a user":               func(l *directory.RelayLink) { l.HostedOrigin = "https://a@foundry.helix-db.com" },
		"a query":              func(l *directory.RelayLink) { l.HostedOrigin = "https://foundry.helix-db.com?x" },
		"not a URL":            func(l *directory.RelayLink) { l.HostedOrigin = "://" },
		"a long origin":        func(l *directory.RelayLink) { l.HostedOrigin = "https://" + strings.Repeat("a", 250) + ".com" },
		"a short id":           func(l *directory.RelayLink) { l.LinkID = "l1" },
		"no token":             func(l *directory.RelayLink) { l.TokenSealed = nil },
		"a long token":         func(l *directory.RelayLink) { l.TokenSealed = make([]byte, 513) },
		"no key":               func(l *directory.RelayLink) { l.KeyID = "" },
		"no account":           func(l *directory.RelayLink) { l.AccountEmail = "" },
		"a long account email": func(l *directory.RelayLink) { l.AccountEmail = strings.Repeat("a", 321) },
	} {
		l := relayLink("01LINK0000000000000000000A")
		bad(&l)
		if err := st.LinkRelay(ctx, l); err == nil {
			t.Errorf("a link with %s was kept", what)
		}
	}

	dev := relayLink("01LINK0000000000000000000A")
	dev.HostedOrigin = "http://localhost:3000"
	if err := st.LinkRelay(ctx, dev); err != nil {
		t.Fatalf("a development origin on this computer: %v", err)
	}
	if err := st.LinkRelay(ctx, relayLink("01LINK0000000000000000000B")); err != nil {
		t.Fatal(err)
	}
	got, err := st.RelayLink(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.LinkID != "01LINK0000000000000000000B" || got.HostedOrigin != "https://foundry.helix-db.com" || string(got.TokenSealed) != "sealed:"+got.LinkID ||
		got.KeyID != "k1" || got.AccountEmail != "ada@example.com" || !got.SlackApprovals || got.LastOKAt != nil || got.LinkedAt.IsZero() {
		t.Fatalf("a new link replaces the old: %+v", got)
	}

	// Approvals through Slack turn off and on, idempotently.
	for _, on := range []bool{false, false, true, true} {
		if err := st.SetRelaySlackApprovals(ctx, on); err != nil {
			t.Fatal(err)
		}
		if got, err := st.RelayLink(ctx); err != nil || got.SlackApprovals != on {
			t.Fatalf("approvals %v: %+v, %v", on, got, err)
		}
	}

	// Helix's answers are recorded for the link they were for only.
	if err := st.RelayOK(ctx, "01LINK0000000000000000000A"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.RelayLink(ctx); got.LastOKAt != nil {
		t.Fatal("an old link's answer was recorded")
	}
	if err := st.RelayOK(ctx, "01LINK0000000000000000000B"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.RelayLink(ctx); got.LastOKAt == nil {
		t.Fatal("the link's answer wasn't recorded")
	}

	// Unlinking forgets the link and the links in flight.
	if _, err := st.StartRelayLink(ctx, relayHash("state"), []byte("v"), "k1"); err != nil {
		t.Fatal(err)
	}
	gone, err := st.UnlinkRelay(ctx)
	if err != nil || gone.LinkID != "01LINK0000000000000000000B" || gone.LastOKAt == nil {
		t.Fatalf("unlink: %+v, %v", gone, err)
	}
	if _, err := st.RelayLink(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after unlink: %v", err)
	}
	if _, _, err := st.SpendRelayLink(ctx, relayHash("state")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a pending link outlived the unlink: %v", err)
	}
}

func TestAPendingRelayLinkIsSpentOnce(t *testing.T) {
	st := needRelayDB(t)
	ctx := context.Background()

	for _, bad := range []struct {
		hash   string
		sealed []byte
		key    string
	}{
		{"state", []byte("v"), "k1"}, {relayHash("s"), nil, "k1"}, {relayHash("s"), make([]byte, 513), "k1"}, {relayHash("s"), []byte("v"), ""},
		{relayHash("s"), []byte("v"), strings.Repeat("k", 65)},
	} {
		if _, err := st.StartRelayLink(ctx, bad.hash, bad.sealed, bad.key); err == nil {
			t.Errorf("%+v was kept", bad)
		}
	}
	expires, err := st.StartRelayLink(ctx, relayHash("s1"), []byte("verifier-1"), "k1")
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(expires); d <= 9*time.Minute || d > directory.RelayPendingTTL {
		t.Fatalf("expires in %s", d)
	}
	if _, err := st.StartRelayLink(ctx, relayHash("s1"), []byte("verifier-2"), "k1"); err == nil {
		t.Fatal("a state kept twice")
	}
	if _, _, err := st.SpendRelayLink(ctx, relayHash("unknown")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	sealed, key, err := st.SpendRelayLink(ctx, relayHash("s1"))
	if err != nil || string(sealed) != "verifier-1" || key != "k1" {
		t.Fatalf("spend: %q %q %v", sealed, key, err)
	}
	if _, _, err := st.SpendRelayLink(ctx, relayHash("s1")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("spent twice: %v", err)
	}

	// An expired one is spent by no one, and forgotten a day on.
	if _, err := st.StartRelayLink(ctx, relayHash("s2"), []byte("v"), "k1"); err != nil {
		t.Fatal(err)
	}
	if _, err := testDB.Exec(`UPDATE foundry_local_relay_pending SET expires_at=? WHERE state_hash=?`, time.Now().UTC().Add(-25*time.Hour),
		relayHash("s2")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SpendRelayLink(ctx, relayHash("s2")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := st.StartRelayLink(ctx, relayHash("s3"), []byte("v"), "k1"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := testDB.QueryRow(`SELECT COUNT(*) FROM foundry_local_relay_pending WHERE state_hash=?`, relayHash("s2")).Scan(&n); err != nil || n != 0 {
		t.Fatalf("an expired pending link kept (%d, %v)", n, err)
	}
}
