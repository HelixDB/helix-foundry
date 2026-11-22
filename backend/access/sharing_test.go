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

func TestReachOf(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	at := func(age time.Duration) *time.Time { t := now.Add(-age); return &t }
	for name, tc := range map[string]struct {
		readAt            *time.Time
		visible           []string
		items             int
		known, all, seeC1 bool
	}{
		"never read":                       {visible: []string{"C1"}, items: 1},
		"read too long ago":                {readAt: at(access.MaxAudienceAge + time.Second), visible: []string{"C1"}, items: 1},
		"read just within the week":        {readAt: at(access.MaxAudienceAge), visible: []string{"C1"}, items: 1, known: true, all: true, seeC1: true},
		"every item":                       {readAt: at(time.Hour), visible: []string{"C1", "C2"}, items: 2, known: true, all: true, seeC1: true},
		"some items":                       {readAt: at(time.Hour), visible: []string{"C1"}, items: 2, known: true, seeC1: true},
		"no item shared with the reader":   {readAt: at(time.Hour), items: 2, known: true},
		"a source with no items reads all": {readAt: at(time.Hour), known: true, all: true},
	} {
		r := access.ReachOf(tc.readAt, tc.visible, tc.items, now)
		assert.Equal(t, tc.known, r.Known(), name)
		assert.Equal(t, tc.all, r.All(), name)
		assert.Equal(t, tc.seeC1, r.Sees("C1"), name)
		assert.False(t, r.Sees("C9"), name)
	}
	var zero access.SourceReach
	assert.False(t, zero.Known(), "the zero reach is unknown")
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
		return access.ReachOf(readAt, visible, 2, now)
	}

	for name, tc := range map[string]struct {
		targets []access.Target
		// reach is the Slack connector's; none is no audienced connector at all.
		reach   *access.SourceReach
		covers  bool
		reaches bool
		table   bool
		docs    []string
		keeps   bool
	}{
		"no source narrows grants": {
			targets: everything, covers: true, reaches: true, table: true, keeps: true,
			docs: []string{"d-public", "d-private", "d-no-item", "d-unlisted", "d-upload"}},
		"a reader of every channel reads the connector whole, but no document of no channel the read listed": {
			targets: everything, reach: ptr(reach(&fresh, "C1", "C2")), covers: true, reaches: true, table: true, keeps: true,
			docs: []string{"d-public", "d-private", "d-upload"}},
		"a reader of the public channel only (no email, or not in the private one) reads its documents, not the table": {
			targets: everything, reach: ptr(reach(&fresh, "C1")), reaches: true, keeps: true, docs: []string{"d-public", "d-upload"}},
		"a guest in the private channel only reads its documents": {
			targets: everything, reach: ptr(reach(&fresh, "C2")), reaches: true, keeps: true, docs: []string{"d-private", "d-upload"}},
		"a reader the source shares nothing with reads only the rest": {
			targets: everything, reach: ptr(reach(&fresh)), reaches: true, docs: []string{"d-upload"}},
		"audiences never read: the connector goes to full readers only": {
			targets: everything, reach: &access.SourceReach{}, reaches: true, docs: []string{"d-upload"}},
		"audiences read over a week ago are unknown": {
			targets: everything, reach: ptr(reach(&stale, "C1", "C2")), reaches: true, docs: []string{"d-upload"}},
		"a set's grant is narrowed as its connector's is": {
			targets: []access.Target{access.DocumentSetTarget("conv")}, reach: ptr(reach(&fresh, "C1")), reaches: true, keeps: true,
			docs: []string{"d-public"}},
		"a table's grant gives nothing the source doesn't share whole": {
			targets: []access.Target{access.TableTarget("slack", "messages")}, reach: ptr(reach(&fresh, "C1"))},
		"a table's grant, every channel shared": {
			targets: []access.Target{access.TableTarget("slack", "messages")}, reach: ptr(reach(&fresh, "C1", "C2")), reaches: true, table: true},
		"sharing gives nothing ungranted": {
			targets: []access.Target{access.DocumentSetTarget("contracts")}, reach: ptr(reach(&fresh, "C1", "C2")), reaches: true,
			docs: []string{"d-upload"}},
	} {
		g := access.NewGrants(tc.targets)
		if tc.reach != nil {
			g = g.WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": *tc.reach}, slackItem))
		}
		assert.Equal(t, tc.covers, g.Covers(in), "%s: covers", name)
		assert.Equal(t, tc.reaches, g.Reaches(in), "%s: reaches", name)
		assert.Equal(t, tc.table, g.CoversTable(messages), "%s: the Slack table", name)
		part := g.Part(holdings)
		assert.ElementsMatch(t, tc.docs, part.Documents, "%s: documents", name)
		assert.Equal(t, len(tc.docs) == len(holdings.Documents), part.AllDocuments, "%s: all documents", name)
		for _, d := range holdings.Documents {
			assert.Equal(t, slices.Contains(tc.docs, d.ID), g.Reads(d), "%s: reads %s", name, d.ID)
		}
		seen := access.Inputs{Sets: []access.SetRef{conversations}}
		assert.Equal(t, tc.keeps, g.Keeps(seen), "%s: keeps what it read of the Slack set", name)
		// Holds compares grants as given, whatever the source shares.
		assert.True(t, g.Holds(tc.targets), "%s: holds its own targets", name)
	}
}

func TestGrantsKeeps(t *testing.T) {
	now := time.Now()
	conversations := access.SetRef{SetID: "conv", ConnectorID: "slack"}
	g := access.NewGrants([]access.Target{access.ConnectorTarget("slack"), access.ConnectorTarget("stripe")}).
		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&now, []string{"C1"}, 2, now)}, slackItem))
	for name, tc := range map[string]struct {
		seen  access.Inputs
		keeps bool
	}{
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
		assert.Equal(t, tc.keeps, g.Keeps(tc.seen), name)
	}
}

func TestSharingWithoutItemRule(t *testing.T) {
	now := time.Now()
	g := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&now, []string{"C1"}, 1, now)}, nil))
	d := access.DocumentRef{ID: "d", Set: access.SetRef{SetID: "conv", ConnectorID: "slack"}, Stream: "conversations", ExternalID: "C1:2026-11-01"}
	assert.False(t, g.Reads(d), "with no rule to tell a document's item, it is of none")
}

func ptr[T any](v T) *T { return &v }

func ExampleReachOf() {
	now := time.Now()
	r := access.ReachOf(&now, []string{"C1"}, 2, now)
	fmt.Println(r.Known(), r.All(), r.Sees("C1"), r.Sees("C2"))
	// Output: true false true false
}

func ExampleGrants_WithSharing() {
	now := time.Now()
	g := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&now, []string{"C1"}, 2, now)}, slackItem))
	conversations := access.SetRef{SetID: "conv", ConnectorID: "slack"}
	fmt.Println(g.CoversTable(access.TableRef{ConnectorID: "slack", Table: "messages"}), g.CoversSet(conversations),
		g.Reaches(access.Inputs{Sets: []access.SetRef{conversations}}))
	// Output: false false true
}

func ExampleGrants_Reads() {
	now := time.Now()
	g := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&now, []string{"C1"}, 2, now)}, slackItem))
	conversations := access.SetRef{SetID: "conv", ConnectorID: "slack"}
	fmt.Println(g.Reads(access.DocumentRef{ID: "d1", Set: conversations, Stream: "conversations", ExternalID: "C1:2026-11-01"}),
		g.Reads(access.DocumentRef{ID: "d2", Set: conversations, Stream: "conversations", ExternalID: "C2:2026-11-01"}))
	// Output: true false
}

func ExampleGrants_Keeps() {
	now := time.Now()
	g := access.NewGrants([]access.Target{access.ConnectorTarget("slack")}).
		WithSharing(access.NewSharing(map[string]access.SourceReach{"slack": access.ReachOf(&now, []string{"C1"}, 2, now)}, slackItem))
	seen := access.Inputs{Sets: []access.SetRef{{SetID: "conv", ConnectorID: "slack"}}}
	fmt.Println(g.Covers(seen), g.Keeps(seen))
	// Output: false true
}
