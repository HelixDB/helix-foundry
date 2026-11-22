package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

func email(t *testing.T, raw string) connector.Email {
	t.Helper()
	e, ok := connector.ParseEmail(raw)
	require.True(t, ok, raw)
	return e
}

// slackAudiences are Acme's Slack: Ada, and Grace, a guest; #general is
// public, #design private to both, #empty private to no one known and
// #board private to Bea, who has no account.
func slackAudiences(t *testing.T) connector.Audiences {
	ada, grace := email(t, "ada@example.com"), email(t, "grace@example.com")
	return connector.Audiences{
		People: map[connector.Email]connector.Person{ada: {}, grace: {Guest: true}},
		Items: map[string]connector.Audience{
			"C1": connector.EveryoneAudience(),
			"C2": connector.MembersAudience(ada, grace),
			"C3": connector.MembersAudience(),
			"C4": connector.MembersAudience(email(t, "bea@example.com")),
		},
	}
}

// sharingOf is SourceSharingFor's answer by connector, for the person whose
// email is reader, as connector.ParseEmail reads it (none for no address).
func sharingOf(t *testing.T, db store.Store, workspaceID, reader string) map[string]model.FoundrySourceSharing {
	t.Helper()
	e, _ := connector.ParseEmail(reader)
	return sharingAs(t, db, workspaceID, &e)
}

// sharingAs is SourceSharingFor's answer by connector for reader, nil for
// an agent's key.
func sharingAs(t *testing.T, db store.Store, workspaceID string, reader *connector.Email) map[string]model.FoundrySourceSharing {
	t.Helper()
	sharing, err := db.SourceSharingFor(context.Background(), workspaceID, reader)
	require.NoError(t, err)
	out := map[string]model.FoundrySourceSharing{}
	for _, s := range sharing {
		out[s.ConnectorID] = s
	}
	require.Len(t, out, len(sharing), "a connector is listed once")
	return out
}

// sourceRows counts the rows of each audiences table a connector has.
func sourceRows(t *testing.T, connectorID string) string {
	t.Helper()
	var counts []string
	for _, table := range []string{"foundry_source_audiences", "foundry_source_people", "foundry_source_items", "foundry_source_item_members"} {
		var n int
		require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE connector_id=?`, connectorID).Scan(&n))
		counts = append(counts, fmt.Sprint(n))
	}
	return strings.Join(counts, " ")
}

// A connector's audiences are replaced whole by a newer read, each reader
// sees what the source shares with them, and a failed read keeps what was
// stored beside why it failed.
func TestSourceAudiences(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	ws, other := id26("SA-WS"), id26("SA-OTHER")
	slack, public, elsewhere := id26("SA-SLACK"), id26("SA-PUBLIC"), id26("SA-ELSEWHERE")
	connectorRow(t, db, ws, slack)
	connectorRow(t, db, ws, public)
	connectorRow(t, db, other, elsewhere)
	read := time.Date(2026, 11, 20, 9, 0, 0, 123456789, time.UTC)
	stored := read.Truncate(time.Microsecond)

	require.NoError(t, db.ReplaceSourceAudiences(ctx, ws, slack, read, slackAudiences(t)))
	require.NoError(t, db.ReplaceSourceAudiences(ctx, ws, public, read, connector.Audiences{
		Items: map[string]connector.Audience{"C9": connector.EveryoneAudience()}}))
	require.NoError(t, db.ReplaceSourceAudiences(ctx, other, elsewhere, read, slackAudiences(t)))
	assert.Equal(t, "1 2 4 3", sourceRows(t, slack))

	// A person reads a public channel only while Slack has an account for
	// them that isn't a guest's: one it doesn't know by that email (a guest
	// signed in with another address, someone offboarded), or one with no
	// email, reads only what lists them. An agent's key, no one, reads the
	// public ones.
	t.Run("each reader sees what the source shares with them", func(t *testing.T) {
		for reader, want := range map[string][]string{
			"ada@example.com":       {"C1", "C2"},
			"  ADA@Example.com\n":   {"C1", "C2"},
			"grace@example.com":     {"C2"}, // a guest sees only what lists them
			"zed@example.com":       nil,
			"":                      nil,
			"not an email":          nil,
			"bea@example.com":       {"C4"}, // listed, with no account
			"ada@example.com.other": nil,
		} {
			got := sharingOf(t, db, ws, reader)
			require.Len(t, got, 2, reader)
			s := got[slack]
			assert.Equal(t, want, s.Visible, reader)
			require.NotNil(t, s.ReadAt)
			assert.True(t, s.ReadAt.Equal(stored), reader)
			assert.Nil(t, s.ProblemAt, reader)
			assert.Empty(t, got[public].Visible, "a source with no account for them shares no public item: %s", reader)
		}
		agent := sharingAs(t, db, ws, nil)
		assert.Equal(t, []string{"C1"}, agent[slack].Visible, "an agent's key reads the public channels")
		assert.Equal(t, []string{"C9"}, agent[public].Visible)
		assert.Empty(t, sharingOf(t, db, id26("SA-NONE"), "ada@example.com"), "audiences are the workspace's")
	})

	t.Run("tells whom of a workspace's people each source has an account for", func(t *testing.T) {
		people, err := db.SourcePeopleByEmail(ctx, ws, []connector.Email{email(t, "ada@example.com"), email(t, "GRACE@example.com"),
			email(t, "zed@example.com"), {}})
		require.NoError(t, err)
		assert.Equal(t, map[connector.Email][]model.FoundrySourcePerson{
			email(t, "ada@example.com"):   {{ConnectorID: slack}},
			email(t, "grace@example.com"): {{ConnectorID: slack, Guest: true}},
		}, people)
		elsewherePeople, err := db.SourcePeopleByEmail(ctx, other, []connector.Email{email(t, "ada@example.com")})
		require.NoError(t, err)
		assert.Equal(t, []model.FoundrySourcePerson{{ConnectorID: elsewhere}}, elsewherePeople[email(t, "ada@example.com")], "another workspace's are its own")
		none, err := db.SourcePeopleByEmail(ctx, ws, nil)
		require.NoError(t, err)
		assert.Empty(t, none)
	})

	// The same read again only moves read_at: what is stored isn't written
	// again, so a row the store didn't write survives it, and a read that
	// differs replaces them all.
	t.Run("a read like the one stored writes one row", func(t *testing.T) {
		_, err := testDB.Exec(`INSERT INTO foundry_source_people (connector_id, email, guest) VALUES (?, 'marker@example.com', FALSE)`, slack)
		require.NoError(t, err)
		later := read.Add(time.Minute)
		require.NoError(t, db.ReplaceSourceAudiences(ctx, ws, slack, later, slackAudiences(t)))
		assert.Equal(t, "1 3 4 3", sourceRows(t, slack), "the rows stay as they were")
		last, err := db.SourceAudienceRead(ctx, ws, slack)
		require.NoError(t, err)
		require.NotNil(t, last.ReadAt)
		assert.True(t, last.ReadAt.Equal(later.Truncate(time.Microsecond)), "read_at moves")
		changed := slackAudiences(t)
		changed.Items["C5"] = connector.EveryoneAudience()
		require.NoError(t, db.ReplaceSourceAudiences(ctx, ws, slack, later.Add(time.Minute), changed))
		assert.Equal(t, "1 2 5 3", sourceRows(t, slack), "a read that differs replaces them")
		require.NoError(t, db.ReplaceSourceAudiences(ctx, ws, slack, later.Add(2*time.Minute), slackAudiences(t)))
		assert.Equal(t, "1 2 4 3", sourceRows(t, slack))
		read = later.Add(2 * time.Minute)
		stored = read.Truncate(time.Microsecond)
	})

	t.Run("a newer read replaces them whole; an older or the same one changes nothing", func(t *testing.T) {
		ada := email(t, "ada@example.com")
		newer := connector.Audiences{
			People: map[connector.Email]connector.Person{ada: {Guest: true}},
			Items:  map[string]connector.Audience{"C2": connector.MembersAudience(ada), "C5": connector.EveryoneAudience()},
		}
		for _, at := range []time.Time{read.Add(-time.Hour), read, stored} {
			require.NoError(t, db.ReplaceSourceAudiences(ctx, ws, slack, at, newer))
			assert.Equal(t, "1 2 4 3", sourceRows(t, slack), at)
		}
		require.NoError(t, db.ReplaceSourceAudiences(ctx, ws, slack, read.Add(time.Hour), newer))
		assert.Equal(t, "1 1 2 1", sourceRows(t, slack))
		s := sharingOf(t, db, ws, "ada@example.com")[slack]
		assert.Equal(t, []string{"C2"}, s.Visible, "now a guest, Ada sees only what lists her")
		assert.Empty(t, sharingOf(t, db, ws, "grace@example.com")[slack].Visible, "no longer in the workspace, Grace sees nothing")
		read = read.Add(time.Hour)
	})

	t.Run("a failed read keeps what was stored, until one works", func(t *testing.T) {
		require.Error(t, db.RecordSourceAudienceProblem(ctx, ws, slack, read.Add(time.Minute), " \n"), "a problem says what it is")
		require.NoError(t, db.RecordSourceAudienceProblem(ctx, ws, slack, read.Add(-time.Minute), "older than the read"))
		list := audiencesOf(t, db, ws)
		assert.Nil(t, list[slack].Problem, "a problem before the read that worked is no problem")

		failed := read.Add(time.Hour)
		require.NoError(t, db.RecordSourceAudienceProblem(ctx, ws, slack, failed, "add the users:read.email scope"))
		require.NoError(t, db.RecordSourceAudienceProblem(ctx, ws, slack, failed.Add(-time.Minute), "an earlier failure"))
		assert.Equal(t, "1 1 2 1", sourceRows(t, slack), "the rows stay")
		list = audiencesOf(t, db, ws)
		require.NotNil(t, list[slack].Problem)
		assert.Equal(t, "add the users:read.email scope", list[slack].Problem.Message, "the latest problem stays")
		assert.True(t, list[slack].Problem.At.Equal(failed.Truncate(time.Microsecond)))
		assert.True(t, list[slack].ReadAt.Equal(read.Truncate(time.Microsecond)), "and so does when the last read worked")
		s := sharingOf(t, db, ws, "ada@example.com")[slack]
		require.NotNil(t, s.ProblemAt)
		assert.True(t, s.ProblemAt.Equal(failed.Truncate(time.Microsecond)), "a reader is told a read failed since")
		last, err := db.SourceAudienceRead(ctx, ws, slack)
		require.NoError(t, err)
		require.NotNil(t, last.ProblemAt)
		assert.True(t, last.ProblemAt.Equal(failed.Truncate(time.Microsecond)))

		// A read that worked before the problem, stored late, keeps it.
		require.NoError(t, db.ReplaceSourceAudiences(ctx, ws, slack, read.Add(time.Minute), slackAudiences(t)))
		list = audiencesOf(t, db, ws)
		require.NotNil(t, list[slack].Problem)
		assert.Equal(t, "1 2 4 3", sourceRows(t, slack))
		// One after it clears it, though it reads what the one stored did.
		require.NoError(t, db.ReplaceSourceAudiences(ctx, ws, slack, failed.Add(time.Minute), slackAudiences(t)))
		assert.Nil(t, audiencesOf(t, db, ws)[slack].Problem)
		assert.Nil(t, sharingOf(t, db, ws, "ada@example.com")[slack].ProblemAt)
		read = failed.Add(time.Minute)

		long := strings.Repeat("é", 1500)
		require.NoError(t, db.RecordSourceAudienceProblem(ctx, ws, slack, read.Add(time.Minute), long))
		assert.Equal(t, strings.Repeat("é", 1024), audiencesOf(t, db, ws)[slack].Problem.Message, "a long problem is cut")
	})

	t.Run("a connector whose reads never worked has no items", func(t *testing.T) {
		never := id26("SA-NEVER")
		connectorRow(t, db, ws, never)
		at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
		require.NoError(t, db.RecordSourceAudienceProblem(ctx, ws, never, at, "Slack returned HTTP 500 for users.list"))
		s := sharingOf(t, db, ws, "ada@example.com")[never]
		assert.Nil(t, s.ReadAt)
		assert.Empty(t, s.Visible)
		a := audiencesOf(t, db, ws)[never]
		assert.Nil(t, a.ReadAt)
		require.NotNil(t, a.Problem)
		assert.Equal(t, "Slack returned HTTP 500 for users.list", a.Problem.Message)
		last, err := db.SourceAudienceRead(ctx, ws, never)
		require.NoError(t, err)
		assert.Nil(t, last.ReadAt)
		assert.NotNil(t, last.ProblemAt)
		fresh := id26("SA-FRESH")
		connectorRow(t, db, ws, fresh)
		last, err = db.SourceAudienceRead(ctx, ws, fresh)
		require.NoError(t, err)
		assert.Equal(t, model.FoundrySourceAudienceRead{}, last, "a connector never read nor tried")
	})

	t.Run("lists each connector's read and counts for owners and admins", func(t *testing.T) {
		list := audiencesOf(t, db, ws)
		require.Len(t, list, 3)
		a := list[slack]
		assert.Equal(t, 1, a.EveryoneItems)
		assert.Equal(t, 3, a.MembersItems)
		assert.Equal(t, 1, list[public].EveryoneItems)
		assert.Zero(t, list[public].MembersItems)
		others, err := db.ListSourceAudiences(ctx, other)
		require.NoError(t, err)
		require.Len(t, others, 1)
		assert.Equal(t, elsewhere, others[0].ConnectorID)
		none, err := db.ListSourceAudiences(ctx, id26("SA-NONE"))
		require.NoError(t, err)
		assert.Empty(t, none)
	})

	t.Run("only a live connector of the workspace has audiences", func(t *testing.T) {
		at := read.Add(24 * time.Hour)
		for _, tc := range [][2]string{{ws, id26("SA-ABSENT")}, {other, slack}} {
			assert.ErrorIs(t, db.ReplaceSourceAudiences(ctx, tc[0], tc[1], at, slackAudiences(t)), store.ErrNotFound)
			assert.ErrorIs(t, db.RecordSourceAudienceProblem(ctx, tc[0], tc[1], at, "x"), store.ErrNotFound)
			_, err := db.SourceAudienceRead(ctx, tc[0], tc[1])
			assert.ErrorIs(t, err, store.ErrNotFound)
		}
		assert.Equal(t, "0 0 0 0", sourceRows(t, id26("SA-ABSENT")))
	})

	t.Run("refuses an item key it can't keep, storing nothing", func(t *testing.T) {
		for _, key := range []string{"", strings.Repeat("k", 256)} {
			bad := slackAudiences(t)
			bad.Items[key] = connector.EveryoneAudience()
			require.Error(t, db.ReplaceSourceAudiences(ctx, ws, slack, read.Add(48*time.Hour), bad))
		}
		assert.Equal(t, "1 2 4 3", sourceRows(t, slack))
		long := connector.Audiences{Items: map[string]connector.Audience{strings.Repeat("é", 255): connector.EveryoneAudience()}}
		require.NoError(t, db.ReplaceSourceAudiences(ctx, ws, public, read.Add(48*time.Hour), long), "255 characters are kept")
	})

	t.Run("an item key and an email are matched exactly", func(t *testing.T) {
		at := read.Add(72 * time.Hour)
		require.NoError(t, db.ReplaceSourceAudiences(ctx, ws, public, at, connector.Audiences{
			Items: map[string]connector.Audience{"c1": connector.EveryoneAudience(), "C1": connector.MembersAudience(email(t, "jose@example.com"))}}))
		assert.Empty(t, sharingOf(t, db, ws, "josé@example.com")[public].Visible, "no collation matches josé and jose")
		assert.Equal(t, []string{"C1"}, sharingOf(t, db, ws, "jose@example.com")[public].Visible)
		assert.Equal(t, []string{"c1"}, sharingAs(t, db, ws, nil)[public].Visible, "c1 and C1 are two items")
	})

	t.Run("deleting the connector deletes its audiences", func(t *testing.T) {
		require.NoError(t, db.DeleteConnector(ctx, ws, slack))
		assert.Equal(t, "0 0 0 0", sourceRows(t, slack))
		assert.NotContains(t, sharingOf(t, db, ws, "ada@example.com"), slack)
		assert.NotContains(t, audiencesOf(t, db, ws), slack)
		require.NoError(t, db.DeleteWorkspaceConnectors(ctx, ws))
		assert.Equal(t, "0 0 0 0", sourceRows(t, public), "and so does deleting the workspace's")
		assert.Equal(t, "1 2 4 3", sourceRows(t, elsewhere), "another workspace's stay")
	})
}

// A workspace's people and a private channel's members, past one INSERT's
// rows, are stored whole.
func TestSourceAudiencesInChunks(t *testing.T) {
	ctx := context.Background()
	db := newTestStore(t)
	ws, slack := id26("SAC-WS"), id26("SAC-SLACK")
	connectorRow(t, db, ws, slack)
	a := connector.Audiences{People: map[connector.Email]connector.Person{}, Items: map[string]connector.Audience{}}
	var members []connector.Email
	for i := range 1201 {
		e := email(t, fmt.Sprintf("p%04d@example.com", i))
		a.People[e] = connector.Person{Guest: i%2 == 0}
		members = append(members, e)
		a.Items[fmt.Sprintf("C%04d", i)] = connector.EveryoneAudience()
	}
	a.Items["G"] = connector.MembersAudience(members...)
	require.NoError(t, db.ReplaceSourceAudiences(ctx, ws, slack, time.Now(), a))
	assert.Equal(t, "1 1201 1202 1201", sourceRows(t, slack))
	s := sharingOf(t, db, ws, "p1200@example.com")[slack]
	assert.Equal(t, []string{"G"}, s.Visible, "a guest")
	s = sharingOf(t, db, ws, "p1199@example.com")[slack]
	assert.Len(t, s.Visible, 1202)
	list := audiencesOf(t, db, ws)[slack]
	assert.Equal(t, 1201, list.EveryoneItems)
	emails := make([]connector.Email, 0, 1201)
	for e := range a.People {
		emails = append(emails, e)
	}
	people, err := db.SourcePeopleByEmail(ctx, ws, emails)
	require.NoError(t, err)
	assert.Len(t, people, 1201, "matched past one query's emails")
}

// audiencesOf is ListSourceAudiences' answer by connector.
func audiencesOf(t *testing.T, db store.Store, workspaceID string) map[string]model.FoundrySourceAudience {
	t.Helper()
	list, err := db.ListSourceAudiences(context.Background(), workspaceID)
	require.NoError(t, err)
	out := map[string]model.FoundrySourceAudience{}
	for _, a := range list {
		out[a.ConnectorID] = a
	}
	return out
}
