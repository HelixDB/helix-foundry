package sourceaccess_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/sourceaccess"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

var (
	now  = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	ada  = access.Principal{Kind: access.PrincipalUser, ID: "user-ada"}
	key  = access.Principal{Kind: access.PrincipalServiceCredential, ID: "cred-1"}
	team = access.Principal{Kind: access.PrincipalTeam, ID: "team-1"}
	conv = access.SetRef{SetID: "set-conv", ConnectorID: "conn-slack"}
)

// fakeStore holds a workspace with a Stripe and a Slack connection; sharing
// is what Slack shares with each reader, by email, and asked counts reads.
type fakeStore struct {
	grants     []model.FoundryAccessGrant
	connectors []*model.Connector
	sharing    map[string][]model.FoundrySourceSharing
	err        map[string]error
	asked      map[string]int
	emails     []string
}

func newStore(targets ...access.Target) *fakeStore {
	s := &fakeStore{
		connectors: []*model.Connector{{ID: "conn-stripe", Kind: "stripe"}, {ID: "conn-slack", Kind: "slack"}},
		sharing:    map[string][]model.FoundrySourceSharing{},
		err:        map[string]error{},
		asked:      map[string]int{},
	}
	for _, p := range []access.Principal{ada, key, team} {
		for _, t := range targets {
			s.grants = append(s.grants, model.FoundryAccessGrant{Principal: p, Target: t})
		}
	}
	return s
}

func (s *fakeStore) ListFoundryAccessGrants(_ context.Context, _ string, principals []access.Principal) ([]model.FoundryAccessGrant, error) {
	s.asked["grants"]++
	var out []model.FoundryAccessGrant
	for _, g := range s.grants {
		for _, p := range principals {
			if g.Principal == p {
				out = append(out, g)
			}
		}
	}
	return out, s.err["grants"]
}

func (s *fakeStore) ListConnectors(context.Context, string) ([]*model.Connector, error) {
	s.asked["connectors"]++
	return s.connectors, s.err["connectors"]
}

func (s *fakeStore) SourceSharingFor(_ context.Context, _, email string) ([]model.FoundrySourceSharing, error) {
	s.asked["sharing"]++
	s.emails = append(s.emails, email)
	return s.sharing[email], s.err["sharing"]
}

// people know Ada's email; err fails every read.
type people struct {
	err   error
	asked *int
}

func (people) MayManageAI(context.Context, string, string) bool { return false }

func (people) DisplayName(context.Context, string) (string, error) { return "", nil }

func (p people) Member(_ context.Context, _, userID string) (ports.Member, error) {
	*p.asked++
	if userID != ada.ID {
		return ports.Member{}, nil
	}
	return ports.Member{Role: ports.RoleMember, Email: "ada@example.com"}, p.err
}

func shared(visible ...string) []model.FoundrySourceSharing {
	at := now.Add(-time.Hour)
	return []model.FoundrySourceSharing{{ConnectorID: "conn-slack", ReadAt: &at, Visible: visible, Items: 2}}
}

func TestGrants(t *testing.T) {
	ctx := context.Background()
	public := access.DocumentRef{ID: "d1", Set: conv, Stream: "conversations", ExternalID: "C1:2026-11-01"}
	private := access.DocumentRef{ID: "d2", Set: conv, Stream: "conversations", ExternalID: "C2:2026-11-01"}
	for name, tc := range map[string]struct {
		reader     access.Principal
		sharing    map[string][]model.FoundrySourceSharing
		wantEmail  string
		reads      [2]bool
		coversConv bool
	}{
		"a member reads the channels Slack shares with her email": {
			reader: ada, sharing: map[string][]model.FoundrySourceSharing{"ada@example.com": shared("C1", "C2"), "": shared("C1")},
			wantEmail: "ada@example.com", reads: [2]bool{true, true}, coversConv: true},
		"a member left out of a private channel": {
			reader: ada, sharing: map[string][]model.FoundrySourceSharing{"ada@example.com": shared("C1")},
			wantEmail: "ada@example.com", reads: [2]bool{true, false}},
		"an agent's key is no one: public channels only": {
			reader: key, sharing: map[string][]model.FoundrySourceSharing{"ada@example.com": shared("C1", "C2"), "": shared("C1")},
			reads: [2]bool{true, false}},
		"a team is no one: each member reads as themselves": {
			reader: team, sharing: map[string][]model.FoundrySourceSharing{"": shared("C1")}, reads: [2]bool{true, false}},
		"audiences never stored: nothing of the connector": {reader: ada, wantEmail: "ada@example.com"},
		"audiences read over a week ago: nothing of the connector": {
			reader: ada, wantEmail: "ada@example.com", sharing: map[string][]model.FoundrySourceSharing{"ada@example.com": func() []model.FoundrySourceSharing {
				at := now.Add(-8 * 24 * time.Hour)
				return []model.FoundrySourceSharing{{ConnectorID: "conn-slack", ReadAt: &at, Visible: []string{"C1", "C2"}, Items: 2}}
			}()}},
	} {
		t.Run(name, func(t *testing.T) {
			store := newStore(access.ConnectorTarget("conn-slack"), access.ConnectorTarget("conn-stripe"))
			store.sharing = tc.sharing
			var asked int
			r := sourceaccess.New(store, people{asked: &asked}, now)
			g, err := r.Grants(ctx, access.Grantee{WorkspaceID: "ws-1", Principal: tc.reader})
			require.NoError(t, err)
			assert.Equal(t, tc.reads, [2]bool{g.Reads(public), g.Reads(private)})
			assert.Equal(t, tc.coversConv, g.CoversSet(conv))
			assert.True(t, g.CoversTable(access.TableRef{ConnectorID: "conn-stripe", Table: "customers"}), "a source that reports nothing isn't narrowed")
			assert.Equal(t, []string{tc.wantEmail}, store.emails)
			// The same call reads none of it again.
			_, err = r.Grants(ctx, access.Grantee{WorkspaceID: "ws-1", Principal: tc.reader})
			require.NoError(t, err)
			assert.Equal(t, 1, store.asked["sharing"])
			assert.Equal(t, 1, store.asked["connectors"])
			assert.LessOrEqual(t, asked, 1)
		})
	}
}

func TestGrantsReadsNoMoreThanItNeeds(t *testing.T) {
	ctx := context.Background()
	t.Run("no grants, nothing to narrow", func(t *testing.T) {
		store := newStore()
		var asked int
		g, err := sourceaccess.New(store, people{asked: &asked}, now).Grants(ctx, access.Grantee{WorkspaceID: "ws-1", Principal: ada})
		require.NoError(t, err)
		assert.True(t, g.Empty())
		assert.Zero(t, store.asked["connectors"]+store.asked["sharing"]+asked)
	})
	t.Run("no connection reports its permissions", func(t *testing.T) {
		store := newStore(access.ConnectorTarget("conn-stripe"))
		store.connectors = store.connectors[:1]
		_, err := sourceaccess.New(store, nil, now).Grants(ctx, access.Grantee{WorkspaceID: "ws-1", Principal: ada})
		require.NoError(t, err)
		assert.Zero(t, store.asked["sharing"])
	})
	t.Run("readers of one workspace read its connections once", func(t *testing.T) {
		store := newStore(access.ConnectorTarget("conn-slack"))
		var asked int
		r := sourceaccess.New(store, people{asked: &asked}, now)
		for _, p := range []access.Principal{ada, key, team} {
			_, err := r.Grants(ctx, access.Grantee{WorkspaceID: "ws-1", Principal: p})
			require.NoError(t, err)
		}
		assert.Equal(t, 1, store.asked["connectors"])
		assert.Equal(t, 2, store.asked["sharing"], "Ada's email, and no one's for the key and the team")
	})
	t.Run("a stored read of a connector deleted since is no one's", func(t *testing.T) {
		store := newStore(access.ConnectorTarget("conn-slack"))
		store.sharing[""] = []model.FoundrySourceSharing{{ConnectorID: "conn-gone", Visible: []string{"C1"}}}
		g, err := sourceaccess.New(store, nil, now).Grants(ctx, access.Grantee{WorkspaceID: "ws-1", Principal: ada})
		require.NoError(t, err)
		assert.False(t, g.ReachesSet(conv), "Slack's own read is missing, so its items are unknown")
	})
}

func TestGrantsFailures(t *testing.T) {
	ctx := context.Background()
	for _, read := range []string{"grants", "connectors", "sharing"} {
		store := newStore(access.ConnectorTarget("conn-slack"))
		store.err[read] = errors.New("down")
		_, err := sourceaccess.New(store, nil, now).Grants(ctx, access.Grantee{WorkspaceID: "ws-1", Principal: key})
		assert.Error(t, err, read)
	}
	var asked int
	_, err := sourceaccess.New(newStore(access.ConnectorTarget("conn-slack")), people{err: errors.New("down"), asked: &asked}, now).
		Grants(ctx, access.Grantee{WorkspaceID: "ws-1", Principal: ada})
	assert.Error(t, err, "the member's email")
}

// A member's grant of a Slack connection gives the channels Slack shares
// with her email; the agent's key, matched to no one, the public ones.
func ExampleReads_Of() {
	store := newStore()
	store.sharing = map[string][]model.FoundrySourceSharing{"ada@example.com": shared("C1", "C2"), "": shared("C1")}
	var asked int
	r := sourceaccess.New(store, people{asked: &asked}, now)
	private := access.DocumentRef{ID: "d2", Set: conv, Stream: "conversations", ExternalID: "C2:2026-11-01"}
	targets := []access.Target{access.ConnectorTarget("conn-slack")}
	asAda, _ := r.Of(context.Background(), "ws-1", ada, targets)
	asKey, _ := r.Of(context.Background(), "ws-1", key, targets)
	fmt.Println(asAda.Reads(private), asKey.Reads(private))
	// Output: true false
}
