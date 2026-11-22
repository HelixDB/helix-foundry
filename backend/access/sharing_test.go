package access_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/helixdb/helix-foundry/backend/access"
)

// slackItem is a Slack-like rule: a conversations document's external id is
// "<channel>:<day>".
func slackItem(connectorID, stream, externalID string) (string, bool) {
	channel, day, ok := strings.Cut(externalID, ":")
	return channel, connectorID == "slack" && stream == "conversations" && ok && channel != "" && day != ""
}

func TestAudiencesFresh(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	at := func(age time.Duration) *time.Time { t := now.Add(-age); return &t }
	for name, tc := range map[string]struct {
		readAt, problemAt *time.Time
		fresh             bool
	}{
		"never read":                                 {},
		"never read, a failure since":                {problemAt: at(time.Hour)},
		"read within the week":                       {readAt: at(access.MaxAudienceAge), fresh: true},
		"read over a week ago":                       {readAt: at(access.MaxAudienceAge + time.Second)},
		"a failure before the read is forgotten":     {readAt: at(3 * 24 * time.Hour), problemAt: at(4 * 24 * time.Hour), fresh: true},
		"a failure since: a day":                     {readAt: at(access.MaxFailingAudienceAge), problemAt: at(time.Hour), fresh: true},
		"a failure since, over a day":                {readAt: at(access.MaxFailingAudienceAge + time.Second), problemAt: at(time.Hour)},
		"a read dated ahead of this clock is fresh":  {readAt: at(-time.Minute), fresh: true},
		"a failure at the same moment is no failure": {readAt: at(2 * 24 * time.Hour), problemAt: at(2 * 24 * time.Hour), fresh: true},
	} {
		assert.Equal(t, tc.fresh, access.AudiencesFresh(tc.readAt, tc.problemAt, now), name)
		assert.Equal(t, tc.fresh, access.ReachOf(tc.readAt, tc.problemAt, nil, now).Known(), name)
	}
}

func TestReachOf(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	read := now.Add(-time.Hour)
	r := access.ReachOf(&read, nil, []string{"C1", "C1"}, now)
	assert.True(t, r.Known())
	assert.True(t, r.Sees("C1"))
	assert.False(t, r.Sees("C9"))
	none := access.ReachOf(&read, nil, nil, now)
	assert.True(t, none.Known(), "a read that shares nothing with the reader is known")
	assert.False(t, none.Sees("C1"))
	var zero access.SourceReach
	assert.False(t, zero.Known(), "the zero reach is unknown")
	assert.False(t, zero.Sees("C1"))
}

func TestGrantsWithSharing(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	fresh, stale := now.Add(-time.Hour), now.Add(-8*24*time.Hour)
	conversations := access.SetRef{SetID: "conv", ConnectorID: "slack"}
	contracts := access.SetRef{SetID: "contracts"}
	messages := access.TableRef{ConnectorID: "slack", Table: "messages"}
	customers := access.TableRef{ConnectorID: "stripe", Table: "customers"}
	doc := func(id, externalID string) access.DocumentRef {
		return access.DocumentRef{ID: id, Set: conversations, Stream: "conversations", ExternalID: externalID}
	}
	public, private := doc("d-public", "C1:2026-11-01"), doc("d-private", "C2:2026-11-01")
	noItem, unlisted := doc("d-no-item", "loose"), doc("d-unlisted", "C9:2026-11-01")
	upload := access.DocumentRef{ID: "d-upload", Set: contracts}
	holdings := access.Holdings{Tables: []access.TableRef{messages, customers}, Documents: []access.DocumentRef{public, private, noItem, unlisted, upload}}
	in := access.Inputs{Tables: holdings.Tables, Sets: []access.SetRef{conversations, contracts}}
	everything := []access.Target{access.ConnectorTarget("slack"), access.ConnectorTarget("stripe"), access.DocumentSetTarget("contracts")}
	reach := func(readAt *time.Time, visible ...string) access.SourceReach {
		return access.ReachOf(readAt, nil, visible, now)
	}
	c1, c2 := access.ItemRef{ConnectorID: "slack", Item: "C1"}, access.ItemRef{ConnectorID: "slack", Item: "C2"}

	for name, tc := range map[string]struct {
		targets []access.Target
		// reach is the Slack connector's; none is no audienced connector at all.
		reach   *access.SourceReach
		covers  bool
		reaches bool
		table   bool
		docs    []string
		items   []access.ItemRef
		keeps   bool
	}{
		"no source narrows grants": {
			targets: everything, covers: true, reaches: true, table: true, keeps: true,
			docs: []string{"d-public", "d-private", "d-no-item", "d-unlisted", "d-upload"}},
		// The load still holds documents of C9, a channel the last read didn't
		// list (the app left it, it was deleted, or it was joined since): no
		// reader of every listed channel reads the load whole, or the table.
		"a reader of every channel listed reads the load in part, and no document of no channel the read listed": {
			targets: everything, reach: ptr(reach(&fresh, "C1", "C2")), reaches: true, keeps: true,
			docs: []string{"d-public", "d-private", "d-upload"}, items: []access.ItemRef{c1, c2}},
		"a reader of the public channel only reads its documents, not the table": {
			targets: everything, reach: ptr(reach(&fresh, "C1")), reaches: true, keeps: true, docs: []string{"d-public", "d-upload"},
			items: []access.ItemRef{c1}},
		"a guest in the private channel only reads its documents": {
			targets: everything, reach: ptr(reach(&fresh, "C2")), reaches: true, keeps: true, docs: []string{"d-private", "d-upload"},
			items: []access.ItemRef{c2}},
		"a reader the source shares nothing with reads only the rest": {
			targets: everything, reach: ptr(reach(&fresh)), reaches: true, docs: []string{"d-upload"}},
		"audiences never read: the connector goes to full readers only": {
			targets: everything, reach: &access.SourceReach{}, reaches: true, docs: []string{"d-upload"}},
		"audiences read over a week ago are unknown": {
			targets: everything, reach: ptr(reach(&stale, "C1", "C2")), reaches: true, docs: []string{"d-upload"}},
		"a set's grant is narrowed as its connector's is": {
			targets: []access.Target{access.DocumentSetTarget("conv")}, reach: ptr(reach(&fresh, "C1")), reaches: true, keeps: true,
			docs: []string{"d-public"}, items: []access.ItemRef{c1}},
		"a table's grant gives nothing of an audienced connector": {
			targets: []access.Target{access.TableTarget("slack", "messages")}, reach: ptr(reach(&fresh, "C1"))},
		"a table's grant, every channel shared, still gives nothing": {
			targets: []access.Target{access.TableTarget("slack", "messages")}, reach: ptr(reach(&fresh, "C1", "C2"))},
		"sharing gives nothing ungranted": {
			targets: []access.Target{access.DocumentSetTarget("contracts")}, reach: ptr(reach(&fresh, "C1", "C2")), reaches: true,
			docs: []string{"d-upload"}},
	} {
		g := access.NewGrants(tc.targets)
		if tc.reach != nil {
			g = g.WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": *tc.reach}, slackItem))
		}
		assert.Equal(t, tc.covers, g.Covers(in), "%s: covers", name)
		if tc.reach != nil {
			assert.False(t, g.CoversSet(conversations), "%s: an audienced connector's set is never covered whole", name)
		}
		assert.Equal(t, tc.reaches, g.Reaches(in), "%s: reaches", name)
		assert.Equal(t, tc.table, g.CoversTable(messages), "%s: the Slack table", name)
		part := g.Part(holdings)
		assert.ElementsMatch(t, tc.docs, part.Documents, "%s: documents", name)
		assert.Equal(t, tc.items, part.Items, "%s: items", name)
		assert.Equal(t, len(tc.docs) == len(holdings.Documents), part.AllDocuments, "%s: all documents", name)
		for _, d := range holdings.Documents {
			assert.Equal(t, slices.Contains(tc.docs, d.ID), g.Reads(d), "%s: reads %s", name, d.ID)
		}
		seen := access.Inputs{Sets: []access.SetRef{conversations}}
		assert.Equal(t, tc.keeps, g.Keeps(seen, part.Items), "%s: keeps what it read of the Slack set", name)
		// Holds compares grants as given, whatever the source shares.
		assert.True(t, g.Holds(tc.targets), "%s: holds its own targets", name)
	}
}

func TestGrantsKeeps(t *testing.T) {
	now := time.Now()
	conversations := access.SetRef{SetID: "conv", ConnectorID: "slack"}
	g := access.NewGrants([]access.Target{access.ConnectorTarget("slack"), access.ConnectorTarget("stripe")}).
		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&now, nil, []string{"C1", "C3"}, now)}, slackItem))
	c1, c2, c3 := access.ItemRef{ConnectorID: "slack", Item: "C1"}, access.ItemRef{ConnectorID: "slack", Item: "C2"},
		access.ItemRef{ConnectorID: "slack", Item: "C3"}
	for name, tc := range map[string]struct {
		seen  access.Inputs
		items []access.ItemRef
		keeps bool
	}{
		// She asked while in #exec (C2) and #general (C1), and was removed from #exec since.
		"a Slack set read in part, a channel it read no longer hers": {
			seen: access.Inputs{Sets: []access.SetRef{conversations}}, items: []access.ItemRef{c1, c2}},
		"a Slack set read in part, every channel it read still hers": {
			seen: access.Inputs{Sets: []access.SetRef{conversations}}, items: []access.ItemRef{c1, c3}, keeps: true},
		"a channel of a connector deleted since": {
			seen: access.Inputs{Sets: []access.SetRef{conversations}}, items: []access.ItemRef{c1, {ConnectorID: "gone", Item: "C1"}}},
		"nothing read":                       {},
		"a Slack set read in part":           {seen: access.Inputs{Sets: []access.SetRef{conversations}}, keeps: true},
		"a granted table":                    {seen: access.Inputs{Tables: []access.TableRef{{ConnectorID: "stripe", Table: "customers"}}}, keeps: true},
		"a Slack table, not every channel":   {seen: access.Inputs{Tables: []access.TableRef{{ConnectorID: "slack", Table: "messages"}}}},
		"a set not granted":                  {seen: access.Inputs{Sets: []access.SetRef{conversations, {SetID: "hr"}}}},
		"a decision from the Slack set":      {seen: access.Inputs{Sets: []access.SetRef{conversations}, DecisionSets: []access.SetRef{conversations}}},
		"People enrichment, not granted":     {seen: access.Inputs{Sets: []access.SetRef{conversations}, Enrichment: true}},
		"what nothing covers":                {seen: access.Inputs{Sets: []access.SetRef{conversations}, Uncoverable: true}},
		"what can't be told":                 {seen: access.Inputs{Sets: []access.SetRef{conversations}, Unknown: true}},
		"a decision from a set fully shared": {seen: access.Inputs{Tables: []access.TableRef{{ConnectorID: "stripe", Table: "x"}}, DecisionSets: []access.SetRef{{SetID: "s", ConnectorID: "stripe"}}}, keeps: true},
	} {
		assert.Equal(t, tc.keeps, g.Keeps(tc.seen, tc.items), name)
	}
}

func TestGrantsVisibleItems(t *testing.T) {
	now := time.Now()
	sharing := access.NewSharing(map[string]access.SourceReach{
		"slack":  access.ReachOf(&now, nil, []string{"C2", "C1"}, now),
		"other":  access.ReachOf(&now, nil, []string{"X"}, now),
		"stale":  {},
		"silent": access.ReachOf(&now, nil, nil, now),
	}, slackItem)
	g := access.NewGrants([]access.Target{access.DocumentSetTarget("conv")}).WithSharing(sharing)
	assert.Equal(t, []access.ItemRef{{ConnectorID: "other", Item: "X"}, {ConnectorID: "slack", Item: "C1"}, {ConnectorID: "slack", Item: "C2"}},
		g.VisibleItems())
	assert.True(t, g.SeesItems(g.VisibleItems()))
	assert.True(t, g.SeesItems(nil), "no items asked are all seen")
	assert.False(t, g.SeesItems([]access.ItemRef{{ConnectorID: "stale", Item: "C1"}}), "an item of a source whose permissions are unknown isn't seen")
	assert.Nil(t, access.NewGrants(nil).WithSharing(sharing).VisibleItems(), "grants that give nothing see no items")
	assert.Equal(t, []access.ItemRef{{ConnectorID: "a", Item: "1"}, {ConnectorID: "a", Item: "2"}, {ConnectorID: "b", Item: "1"}},
		access.SortItems([]access.ItemRef{{ConnectorID: "b", Item: "1"}, {ConnectorID: "a", Item: "2"}, {ConnectorID: "a", Item: "1"}, {ConnectorID: "b", Item: "1"}}))
}

func TestSharingWithoutItemRule(t *testing.T) {
	now := time.Now()
	g := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&now, nil, []string{"C1"}, now)}, nil))
	d := access.DocumentRef{ID: "d", Set: access.SetRef{SetID: "conv", ConnectorID: "slack"}, Stream: "conversations", ExternalID: "C1:2026-11-01"}
	assert.False(t, g.Reads(d), "with no rule to tell a document's item, it is of none")
}

func ptr[T any](v T) *T { return &v }

func ExampleReachOf() {
	now := time.Now()
	r := access.ReachOf(&now, nil, []string{"C1"}, now)
	fmt.Println(r.Known(), r.Sees("C1"), r.Sees("C2"))
	// Output: true true false
}

func ExampleAudiencesFresh() {
	now := time.Now()
	read := now.Add(-48 * time.Hour)
	failed := read.Add(time.Hour)
	fmt.Println(access.AudiencesFresh(&read, nil, now), access.AudiencesFresh(&read, &failed, now), access.AudiencesFresh(nil, nil, now))
	// Output: true false false
}

func ExampleSortItems() {
	fmt.Println(access.SortItems([]access.ItemRef{{ConnectorID: "s", Item: "C2"}, {ConnectorID: "s", Item: "C1"}, {ConnectorID: "s", Item: "C2"}}))
	// Output: [{s C1} {s C2}]
}

func ExampleGrants_SeesItems() {
	now := time.Now()
	g := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&now, nil, []string{"C1"}, now)}, slackItem))
	fmt.Println(g.SeesItems([]access.ItemRef{{ConnectorID: "slack", Item: "C1"}}), g.SeesItems([]access.ItemRef{{ConnectorID: "slack", Item: "C2"}}),
		g.VisibleItems())
	// Output: true false [{slack C1}]
}

func ExampleGrants_WithSharing() {
	now := time.Now()
	g := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&now, nil, []string{"C1"}, now)}, slackItem))
	conversations := access.SetRef{SetID: "conv", ConnectorID: "slack"}
	fmt.Println(g.CoversTable(access.TableRef{ConnectorID: "slack", Table: "messages"}), g.CoversSet(conversations),
		g.Reaches(access.Inputs{Sets: []access.SetRef{conversations}}))
	// Output: false false true
}

func ExampleGrants_Reads() {
	now := time.Now()
	g := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&now, nil, []string{"C1"}, now)}, slackItem))
	conversations := access.SetRef{SetID: "conv", ConnectorID: "slack"}
	fmt.Println(g.Reads(access.DocumentRef{ID: "d1", Set: conversations, Stream: "conversations", ExternalID: "C1:2026-11-01"}),
		g.Reads(access.DocumentRef{ID: "d2", Set: conversations, Stream: "conversations", ExternalID: "C2:2026-11-01"}))
	// Output: true false
}

func ExampleGrants_Keeps() {
	now := time.Now()
	g := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&now, nil, []string{"C1"}, now)}, slackItem))
	seen := access.Inputs{Sets: []access.SetRef{{SetID: "conv", ConnectorID: "slack"}}}
	fmt.Println(g.Covers(seen), g.Keeps(seen, []access.ItemRef{{ConnectorID: "slack", Item: "C1"}}),
		g.Keeps(seen, []access.ItemRef{{ConnectorID: "slack", Item: "C2"}}))
	// Output: false true false
}
