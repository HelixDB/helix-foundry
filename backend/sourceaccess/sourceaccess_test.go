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
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/sourceaccess"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

var (
	now  = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	ada  = access.Principal{Kind: access.PrincipalUser, ID: "user-ada"}
	bob  = access.Principal{Kind: access.PrincipalUser, ID: "user-bob"}
	eve  = access.Principal{Kind: access.PrincipalUser, ID: "user-eve"}
	key  = access.Principal{Kind: access.PrincipalServiceCredential, ID: "cred-1"}
	team = access.Principal{Kind: access.PrincipalTeam, ID: "team-1"}
	conv = access.SetRef{SetID: "set-conv", ConnectorID: "conn-slack"}
)

// fakeStore holds a workspace with a Stripe and a Slack connection; sharing
// is what Slack shares with each reader, by email ("agent" for no one), and
// asked counts reads.
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
	for _, p := range []access.Principal{ada, bob, eve, key, team} {
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

func (s *fakeStore) SourceSharingFor(_ context.Context, _ string, reader *connector.Email) ([]model.FoundrySourceSharing, error) {
	s.asked["sharing"]++
	email := "agent"
	if reader != nil {
		email = reader.String()
	}
	s.emails = append(s.emails, email)
	return s.sharing[email], s.err["sharing"]
}

// people know Ada's email, written as a person might, and Bob's, which is no
// address; err fails every read.
type people struct {
	err   error
	asked *int
}

func (people) MayManageAI(context.Context, string, string) bool { return false }

func (people) DisplayName(context.Context, string) (string, error) { return "", nil }

func (p people) Member(_ context.Context, _, userID string) (ports.Member, error) {
	*p.asked++
	switch userID {
	case ada.ID:
		return ports.Member{Role: ports.RoleMember, Email: " Ada@Example.com "}, p.err
	case bob.ID:
		return ports.Member{Role: ports.RoleMember, Email: "bob"}, p.err
	}
	return ports.Member{}, p.err
}

func shared(visible ...string) []model.FoundrySourceSharing {
	at := now.Add(-time.Hour)
	return []model.FoundrySourceSharing{{ConnectorID: "conn-slack", ReadAt: &at, Visible: visible}}
}

// asMember is sharing as the store reads it for a person Slack has an account
// for that isn't a guest's.
func asMember(sharing []model.FoundrySourceSharing) []model.FoundrySourceSharing {
	for i := range sharing {
		sharing[i].Member = true
	}
	return sharing
}

func TestGrants(t *testing.T) {
	ctx := context.Background()
	public := access.DocumentRef{ID: "d1", Set: conv, Stream: "conversations", ExternalID: "C1:2026-11-01"}
	private := access.DocumentRef{ID: "d2", Set: conv, Stream: "conversations", ExternalID: "C2:2026-11-01"}
	for name, tc := range map[string]struct {
		reader    access.Principal
		sharing   map[string][]model.FoundrySourceSharing
		wantEmail []string
		reads     [2]bool
		// directory says it reads Slack's users.
		directory bool
	}{
		"a member reads the channels Slack shares with her email, in one form, and its directory": {
			reader: ada, sharing: map[string][]model.FoundrySourceSharing{"ada@example.com": asMember(shared("C1", "C2")), "agent": shared("C1")},
			wantEmail: []string{"ada@example.com"}, reads: [2]bool{true, true}, directory: true},
		"a guest reads what lists her, and no directory": {
			reader: ada, sharing: map[string][]model.FoundrySourceSharing{"ada@example.com": shared("C2")},
			wantEmail: []string{"ada@example.com"}, reads: [2]bool{false, true}},
		"audiences read over a week ago: no directory either": {
			reader: ada, wantEmail: []string{"ada@example.com"}, sharing: map[string][]model.FoundrySourceSharing{"ada@example.com": func() []model.FoundrySourceSharing {
				at := now.Add(-8 * 24 * time.Hour)
				return []model.FoundrySourceSharing{{ConnectorID: "conn-slack", ReadAt: &at, Visible: []string{"C1"}, Member: true}}
			}()}},
		"a member left out of a private channel": {
			reader: ada, sharing: map[string][]model.FoundrySourceSharing{"ada@example.com": shared("C1")},
			wantEmail: []string{"ada@example.com"}, reads: [2]bool{true, false}},
		// The store gives a person Slack has no account for no public
		// channel; she is asked as herself, never as no one.
		"a member whose email is no address is asked as no address": {
			reader: bob, sharing: map[string][]model.FoundrySourceSharing{"": shared(), "agent": shared("C1")}, wantEmail: []string{""}},
		"a member with no email is asked as no address, never as an agent": {
			reader: eve, sharing: map[string][]model.FoundrySourceSharing{"": shared(), "agent": shared("C1")}, wantEmail: []string{""}},
		"an agent's key is no one: public channels only": {
			reader: key, sharing: map[string][]model.FoundrySourceSharing{"ada@example.com": shared("C1", "C2"), "agent": shared("C1")},
			wantEmail: []string{"agent"}, reads: [2]bool{true, false}},
		"a team is no one: each member reads as themselves": {
			reader: team, sharing: map[string][]model.FoundrySourceSharing{"agent": shared("C1")}, wantEmail: []string{"agent"}, reads: [2]bool{true, false}},
		"audiences never stored: nothing of the connector": {reader: ada, wantEmail: []string{"ada@example.com"}},
		"audiences read over a week ago: nothing of the connector": {
			reader: ada, wantEmail: []string{"ada@example.com"}, sharing: map[string][]model.FoundrySourceSharing{"ada@example.com": func() []model.FoundrySourceSharing {
				at := now.Add(-8 * 24 * time.Hour)
				return []model.FoundrySourceSharing{{ConnectorID: "conn-slack", ReadAt: &at, Visible: []string{"C1", "C2"}}}
			}()}},
		"audiences read two days ago, a read since failed: nothing of the connector": {
			reader: ada, wantEmail: []string{"ada@example.com"}, sharing: map[string][]model.FoundrySourceSharing{"ada@example.com": func() []model.FoundrySourceSharing {
				at, failed := now.Add(-48*time.Hour), now.Add(-time.Hour)
				return []model.FoundrySourceSharing{{ConnectorID: "conn-slack", ReadAt: &at, ProblemAt: &failed, Visible: []string{"C1", "C2"}}}
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
			assert.False(t, g.CoversSet(conv), "an audienced connector's set is never covered whole")
			assert.False(t, g.CoversTable(access.TableRef{ConnectorID: "conn-slack", Table: "messages"}), "nor its tables")
			assert.Equal(t, tc.reads[0] || tc.reads[1], g.ReadsTable(access.TableRef{ConnectorID: "conn-slack", Table: "messages"}),
				"its messages are read by the channels shared")
			assert.Equal(t, tc.directory, g.ReadsTable(access.TableRef{ConnectorID: "conn-slack", Table: "users"}), "its directory")
			assert.True(t, g.CoversTable(access.TableRef{ConnectorID: "conn-stripe", Table: "customers"}), "a source that reports nothing isn't narrowed")
			assert.Equal(t, tc.wantEmail, store.emails)
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
		var asked int
		_, err := sourceaccess.New(store, people{asked: &asked}, now).Grants(ctx, access.Grantee{WorkspaceID: "ws-1", Principal: ada})
		require.NoError(t, err)
		assert.Zero(t, store.asked["sharing"])
	})
	// Without a way to tell emails, a user is never read as no one, which
	// would give them every public channel, guests included.
	t.Run("nothing tells a user's email: their sources' items are unknown", func(t *testing.T) {
		store := newStore(access.ConnectorTarget("conn-slack"))
		store.sharing["agent"] = shared("C1", "C2")
		r := sourceaccess.New(store, nil, now)
		g, err := r.Grants(ctx, access.Grantee{WorkspaceID: "ws-1", Principal: ada})
		require.NoError(t, err)
		assert.False(t, g.ReachesSet(conv))
		assert.Nil(t, g.VisibleItems())
		assert.Zero(t, store.asked["sharing"])
		email, err := r.Email(ctx, "ws-1", ada.ID)
		require.NoError(t, err)
		assert.Equal(t, connector.Email{}, email)
		asKey, err := r.Grants(ctx, access.Grantee{WorkspaceID: "ws-1", Principal: key})
		require.NoError(t, err)
		assert.True(t, asKey.ReachesSet(conv), "an agent's key still reads as no one")
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
		email, err := r.Email(ctx, "ws-1", ada.ID)
		require.NoError(t, err)
		assert.Equal(t, "ada@example.com", email.String())
		assert.Equal(t, 1, asked, "her email is read once a call")
	})
	t.Run("a stored read of a connector deleted since is no one's", func(t *testing.T) {
		store := newStore(access.ConnectorTarget("conn-slack"))
		store.sharing["agent"] = []model.FoundrySourceSharing{{ConnectorID: "conn-gone", Visible: []string{"C1"}}}
		g, err := sourceaccess.New(store, nil, now).Grants(ctx, access.Grantee{WorkspaceID: "ws-1", Principal: key})
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
	store.sharing = map[string][]model.FoundrySourceSharing{"ada@example.com": shared("C1", "C2"), "agent": shared("C1")}
	var asked int
	r := sourceaccess.New(store, people{asked: &asked}, now)
	private := access.DocumentRef{ID: "d2", Set: conv, Stream: "conversations", ExternalID: "C2:2026-11-01"}
	targets := []access.Target{access.ConnectorTarget("conn-slack")}
	asAda, _ := r.Of(context.Background(), "ws-1", ada, targets)
	asKey, _ := r.Of(context.Background(), "ws-1", key, targets)
	fmt.Println(asAda.Reads(private), asKey.Reads(private))
	// Output: true false
}

// Audienced keeps the connectors whose kinds report their sources'
// permissions alone.
func TestAudienced(t *testing.T) {
	kinds := sourceaccess.Audienced(newStore().connectors)
	assert.Len(t, kinds, 1)
	assert.Contains(t, kinds, "conn-slack")
	assert.Empty(t, sourceaccess.Audienced(nil))
}

// Told, built from what the API told a grantee is shared with it
// (VisibleItems), reads a load as the grants it was told from: the part of
// it the API narrows a read to, its items included, is the same, however
// fresh the source's audiences are.
func TestToldReadsALoadAsTheGrantsItWasToldFrom(t *testing.T) {
	ctx := context.Background()
	day := func(id, channel string) access.DocumentRef {
		return access.DocumentRef{ID: id, Set: conv, Stream: "conversations", ExternalID: channel + ":2026-11-01"}
	}
	other := access.DocumentRef{ID: "d-notes", Set: access.SetRef{SetID: "set-notes"}}
	holdings := access.Holdings{
		Tables:    []access.TableRef{{ConnectorID: "conn-slack", Table: "messages"}, {ConnectorID: "conn-stripe", Table: "customers"}},
		Files:     []access.DocumentRef{day("f-general", "C1")},
		Documents: []access.DocumentRef{day("d-general", "C1"), day("d-leadership", "C2"), day("d-random", "C3"), other},
	}
	stale := now.Add(-8 * 24 * time.Hour)
	for name, sharing := range map[string][]model.FoundrySourceSharing{
		"two channels shared":       shared("C1", "C2"),
		"one channel shared":        shared("C1"),
		"none shared":               shared(),
		"audiences never read":      nil,
		"audiences over a week old": {{ConnectorID: "conn-slack", ReadAt: &stale, Visible: []string{"C1"}}},
	} {
		t.Run(name, func(t *testing.T) {
			targets := []access.Target{access.ConnectorTarget("conn-slack"), access.ConnectorTarget("conn-stripe"), access.DocumentSetTarget("set-notes")}
			store := newStore(targets...)
			store.sharing = map[string][]model.FoundrySourceSharing{"ada@example.com": sharing}
			var asked int
			g, err := sourceaccess.New(store, people{asked: &asked}, now).Grants(ctx, access.Grantee{WorkspaceID: "ws-1", Principal: ada})
			require.NoError(t, err)
			told := sourceaccess.Told(sourceaccess.Audienced(store.connectors), targets, g.VisibleItems())
			assert.Equal(t, g.Part(holdings), told.Part(holdings))
		})
	}
	// The API tells a grantee no directory, which holds no item: what Told
	// reads of the load's documents and rows, and so their items, is the same.
	t.Run("a directory is no item", func(t *testing.T) {
		targets := []access.Target{access.ConnectorTarget("conn-slack")}
		store := newStore(targets...)
		store.sharing = map[string][]model.FoundrySourceSharing{"ada@example.com": asMember(shared("C1"))}
		var asked int
		g, err := sourceaccess.New(store, people{asked: &asked}, now).Grants(ctx, access.Grantee{WorkspaceID: "ws-1", Principal: ada})
		require.NoError(t, err)
		told := sourceaccess.Told(sourceaccess.Audienced(store.connectors), targets, g.VisibleItems())
		withUsers := access.Holdings{Tables: append([]access.TableRef{{ConnectorID: "conn-slack", Table: "users"}}, holdings.Tables...), Documents: holdings.Documents}
		assert.Equal(t, []access.TableRef{{ConnectorID: "conn-slack", Table: "users"}}, g.Part(withUsers).Tables)
		assert.Empty(t, told.Part(withUsers).Tables)
		assert.Equal(t, g.Part(withUsers).Items, told.Part(withUsers).Items)
	})
	t.Run("both channels of one load", func(t *testing.T) {
		told := sourceaccess.Told(sourceaccess.Audienced(newStore().connectors), []access.Target{access.ConnectorTarget("conn-slack")},
			[]access.ItemRef{{ConnectorID: "conn-slack", Item: "C1"}, {ConnectorID: "conn-slack", Item: "C2"}})
		assert.Equal(t, []access.ItemRef{{ConnectorID: "conn-slack", Item: "C1"}, {ConnectorID: "conn-slack", Item: "C2"}}, told.Part(holdings).Items)
	})
}
