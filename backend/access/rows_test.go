package access_test

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/helixdb/helix-foundry/backend/access"
)

// Each Slack table tells its rows' channel by its own rule, and its
// directory of people goes whole to a person Slack has an account for that
// isn't a guest's, and to no one else.
func TestGrantsTableRows(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	fresh, stale := now.Add(-time.Hour), now.Add(-8*24*time.Hour)
	table := func(name string) access.TableRef { return access.TableRef{ConnectorID: "slack", Table: name} }
	users, channels, members, messages, reactions := table("users"), table("channels"), table("channel_members"), table("messages"), table("reactions")
	holdings := access.Holdings{Tables: []access.TableRef{users, channels, members, messages, reactions}}
	grants := func(reach access.SourceReach) access.Grants {
		return access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
			WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": reach}, kinds))
	}
	byItem := func(t access.TableRef, items ...string) access.TableRows {
		return access.TableRows{Table: t, Rule: access.ItemRows("channel_id"), Items: items}
	}
	for name, tc := range map[string]struct {
		reach  access.SourceReach
		tables []access.TableRef
		rows   []access.TableRows
	}{
		"a matched person reads the directory and her channels' rows": {
			reach: access.ReachOf(&fresh, nil, access.Shared{Items: []string{"C2", "C1"}, Member: true}, now), tables: []access.TableRef{users},
			rows: []access.TableRows{byItem(channels, "C1", "C2"), byItem(members, "C1", "C2"),
				{Table: messages, Rule: access.ItemPrefixRows("message_id", ":"), Items: []string{"C1", "C2"}}}},
		// A guest, an unmatched person and an agent's key: none is Slack's member.
		"a guest or anyone unmatched reads only the rows of what lists them": {
			reach: access.ReachOf(&fresh, nil, access.Shared{Items: []string{"C2"}}, now),
			rows: []access.TableRows{byItem(channels, "C2"), byItem(members, "C2"),
				{Table: messages, Rule: access.ItemPrefixRows("message_id", ":"), Items: []string{"C2"}}}},
		"a member shared no channel reads the directory alone": {
			reach: access.ReachOf(&fresh, nil, access.Shared{Member: true}, now), tables: []access.TableRef{users}},
		"stale audiences hold nothing, the directory included": {
			reach: access.ReachOf(&stale, nil, access.Shared{Items: []string{"C1"}, Member: true}, now)},
		"never read": {},
	} {
		g := grants(tc.reach)
		part := g.Part(holdings)
		assert.Equal(t, tc.tables, part.Tables, "%s: whole", name)
		assert.Equal(t, tc.rows, part.Rows, "%s: rows", name)
		assert.False(t, g.ReadsTable(reactions), "%s: a table no rule tells apart goes to full readers only", name)
		assert.False(t, g.Covers(access.Inputs{Tables: holdings.Tables}), "%s: never whole", name)
		assert.Equal(t, len(tc.tables)+len(tc.rows) > 0, g.Reaches(access.Inputs{Tables: holdings.Tables}), "%s: reaches", name)
		for _, r := range part.Rows {
			assert.True(t, g.Keeps(access.Inputs{Tables: []access.TableRef{r.Table}}, part.Items), "%s: keeps %s", name, r.Table.Table)
		}
	}
	t.Run("a table not granted gives no rows", func(t *testing.T) {
		g := access.NewGrants([]access.Target{access.TableTarget("slack", "channels")}).
			WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&fresh, nil, access.Shared{Items: []string{"C1"}, Member: true}, now)}, kinds))
		part := g.Part(holdings)
		assert.Empty(t, part.Tables, "the directory is granted as any table")
		assert.Equal(t, []access.TableRows{byItem(channels, "C1")}, part.Rows)
	})
	t.Run("a kind Sharing isn't told reads no table", func(t *testing.T) {
		g := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
			WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&fresh, nil, access.Shared{Items: []string{"C1"}, Member: true}, now)}, nil))
		assert.False(t, g.ReadsTable(channels))
		assert.False(t, g.ReadsTable(users))
	})
}

// Past MaxHeldItems a source's tables go to full readers only, since every
// read and question would carry the items; its documents are still read one
// by one.
func TestGrantsTableRowsCap(t *testing.T) {
	now := time.Now()
	items := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = "C" + strconv.Itoa(i)
		}
		return out
	}
	channels := access.TableRef{ConnectorID: "slack", Table: "channels"}
	conversations := access.SetRef{SetID: "conv", ConnectorID: "slack"}
	doc := access.DocumentRef{ID: "d", Set: conversations, Stream: "conversations", ExternalID: "C1:2026-11-01"}
	for n, held := range map[int]bool{access.MaxHeldItems: true, access.MaxHeldItems + 1: false} {
		g := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
			WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&now, nil, access.Shared{Items: items(n)}, now)}, kinds))
		assert.Equal(t, held, g.ReadsTable(channels), "%d items", n)
		part := g.Part(access.Holdings{Tables: []access.TableRef{channels}, Documents: []access.DocumentRef{doc}})
		assert.Equal(t, held, len(part.Rows) == 1, "%d items", n)
		assert.Equal(t, []string{"d"}, part.Documents, "%d items: documents are read one by one", n)
	}
}

// What a part read by its rows held stays held while every row is: the same
// table, by the same rule, of at least the items it held.
func TestPartHoldsRows(t *testing.T) {
	messages := access.TableRef{ConnectorID: "slack", Table: "messages"}
	rule := access.ItemPrefixRows("message_id", ":")
	rows := func(t access.TableRef, r access.RowRule, items ...string) access.Part {
		return access.Part{Rows: []access.TableRows{{Table: t, Rule: r, Items: items}}}
	}
	before := rows(messages, rule, "C1")
	assert.True(t, rows(messages, rule, "C1", "C2").Holds(before), "more channels")
	assert.True(t, rows(messages, rule, "C1").Holds(before))
	assert.False(t, rows(messages, rule, "C2").Holds(before), "a channel lost")
	assert.False(t, access.Part{}.Holds(before), "the table lost")
	assert.False(t, rows(access.TableRef{ConnectorID: "slack", Table: "channel_days"}, rule, "C1").Holds(before), "another table")
	assert.False(t, rows(messages, access.ItemRows("message_id"), "C1").Holds(before), "another rule")
	assert.True(t, before.Holds(access.Part{}))
	assert.Equal(t, []access.TableRef{{ConnectorID: "stripe", Table: "customers"}, messages},
		access.Part{Tables: []access.TableRef{{ConnectorID: "stripe", Table: "customers"}}, Rows: before.Rows}.Inputs().Tables,
		"a part's inputs name the tables it read by their rows")
}

func TestItemPrefixRowsNeedsASeparator(t *testing.T) {
	assert.Panics(t, func() { access.ItemPrefixRows("message_id", "") })
	assert.Equal(t, access.RowsUnheld, access.RowRule{}.Match(), "the zero rule holds no row")
	assert.Equal(t, access.RowsDirectory, access.DirectoryRows().Match())
}

func ExampleTableRows_Held() {
	for _, r := range []access.TableRows{
		{Rule: access.ItemRows("channel_id"), Items: []string{"C1", "C2"}},
		{Rule: access.ItemPrefixRows("message_id", ":"), Items: []string{"C1", "C2"}},
	} {
		fmt.Println(r.Held())
	}
	// Output:
	// channel_id false [C1 C2]
	// message_id true [C1: C2:]
}

func ExampleGrants_ReadsTable() {
	now := time.Now()
	asAda := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&now, nil, access.Shared{Items: []string{"C1"}, Member: true}, now)}, kinds))
	asKey := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&now, nil, access.Shared{Items: []string{"C1"}}, now)}, kinds))
	messages, users := access.TableRef{ConnectorID: "slack", Table: "messages"}, access.TableRef{ConnectorID: "slack", Table: "users"}
	fmt.Println(asAda.ReadsTable(messages), asAda.ReadsTable(users), asKey.ReadsTable(messages), asKey.ReadsTable(users))
	fmt.Println(asAda.Part(access.Holdings{Tables: []access.TableRef{messages, users}}).Items)
	// Output:
	// true true true false
	// [{slack C1}]
}
