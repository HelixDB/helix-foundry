package store_test

import (
	"context"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A document no person asked for (an API source's piece) is extracted only
// by work no one requested, so a person's spend never pays for it: it never
// joins a load a person's job is writing, and that job's end queues the
// extract that holds it. Work no one requested takes it as any sync's
// document; so does a person's job when the fan-out is a person's own.
func TestFoundrySystemOnlyFanOut(t *testing.T) {
	day := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	person := id26("U-SYS")
	n1 := id26("FD-n1")
	// live starts the ontology's live job, if any, and returns the load it
	// writes ("" for none). The current load L1 reads the connector's set
	// and the General set.
	type live func(t *testing.T, c connectorSync, tag string) string
	none := func(*testing.T, connectorSync, string) string { return "" }
	systemLoad := func(t *testing.T, c connectorSync, tag string) string { return c.loadReading(t, "L2", c.setID) }
	personLoad := func(t *testing.T, c connectorSync, tag string) string {
		c.versionReading(t, "L2", c.setID)
		job := loadJob(c.ontology, "L2")
		job.RequestedByUserID = &person
		require.NoError(t, c.db.CreateFoundryLoad(context.Background(), job))
		return job.LoadID
	}
	personLoadElsewhere := func(t *testing.T, c connectorSync, tag string) string {
		c.versionReading(t, "L2", generalSetID(c.ontology.ID))
		job := loadJob(c.ontology, "L2")
		job.RequestedByUserID = &person
		require.NoError(t, c.db.CreateFoundryLoad(context.Background(), job))
		return job.LoadID
	}
	// A person's upload queues an extract of the current load in their name.
	personExtract := func(t *testing.T, c connectorSync, tag string) string {
		ctx := context.Background()
		d := foundryDocument(c.ontology, tag+"-upload")
		_, _, err := c.db.CreateFoundryDocument(ctx, d, noCap)
		require.NoError(t, err)
		fan := c.fan(tag + "-upload")
		fan.RequestedByUserID = &person
		_, _, err = c.db.CompleteFoundryDocumentUpload(ctx, c.ontology.WorkspaceID, d.ID, fan)
		require.NoError(t, err)
		job := c.liveJob(t)
		require.NotNil(t, job)
		require.Equal(t, &person, job.RequestedByUserID)
		return job.LoadID
	}
	for _, tc := range []struct {
		name       string
		live       live
		systemOnly bool
		outcome    model.FoundryDocumentReadOutcome
		// joins is the load the document joined: L1 (current), L2 (being
		// written) or none.
		joins string
		// thenHeld means the live job's end queues an extract no one
		// requested that holds the document.
		thenHeld bool
	}{
		{"no job live: queued as today", none, true, model.FoundryDocumentReadQueued, "L1", false},
		{"no job live, without SystemOnly", none, false, model.FoundryDocumentReadQueued, "L1", false},
		{"work no one requested is live: joined", systemLoad, true, model.FoundryDocumentReadOwed, "L2", false},
		{"a person's load is live: owed, not joined", personLoad, true, model.FoundryDocumentReadOwed, "", true},
		{"a person's load is live, without SystemOnly: joined as today", personLoad, false, model.FoundryDocumentReadOwed, "L2", false},
		{"a person's extract of the current load is live: owed, not joined", personExtract, true, model.FoundryDocumentReadOwed, "", true},
		{"a person's extract is live, without SystemOnly: joined as today", personExtract, false, model.FoundryDocumentReadOwed, "L1", false},
		// Its load becomes current and reads the set no more, so nothing is extracted.
		{"a person's load that doesn't read the set is live: owed", personLoadElsewhere, true, model.FoundryDocumentReadOwed, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			const tag = "SYS"
			c := newConnectorSync(t, newFoundryFixture(t, tag), tag)
			current := c.loadReading(t, "L1", c.setID, generalSetID(c.ontology.ID))
			c.loaded(t, "w1")
			writing := tc.live(t, c, tag)
			p := c.publish(tag, []*model.FoundryDocument{c.note("n1", "not_1", "1|a", day)}, nil)
			p.Fan.SystemOnly = tc.systemOnly
			reads := c.published(t, p)
			assert.Equal(t, []model.FoundryDocumentRead{{OntologyID: c.ontology.ID, Outcome: tc.outcome}}, reads)
			loads := map[string]string{"L1": current, "L2": writing}
			for name, load := range loads {
				if load == "" || name == "L2" && load == current {
					continue // no job live, or it writes the current load
				}
				assert.Equal(t, tc.joins == name, slices.Contains(members(t, load), n1), "%s holds the document", name)
			}
			if tc.outcome == model.FoundryDocumentReadQueued {
				job := c.liveJob(t)
				require.NotNil(t, job)
				assert.Equal(t, model.FoundryGraphJobExtract, job.Kind)
				assert.Nil(t, job.RequestedByUserID, "work no one requested")
				return
			}
			assert.True(t, c.ontologyNow(t).ExtractOwed)
			if !tc.thenHeld {
				return
			}
			// The person's job ends having extracted what it holds; the
			// extract it owes, which no one requested, holds the document.
			ended, err := c.db.ListPendingFoundryDocuments(ctx, writing)
			require.NoError(t, err)
			_, finish := c.finish(t, "w1", tag+"-end", model.FoundryGraphJobSucceeded, documentIDs(ended)...)
			assert.Equal(t, model.FoundryGraphJobEnded, finish, "nothing it holds is unrecorded")
			job := c.liveJob(t)
			require.NotNil(t, job, "the owed extract is queued")
			assert.Equal(t, model.FoundryGraphJobExtract, job.Kind)
			assert.Nil(t, job.RequestedByUserID, "work no one requested")
			require.NotNil(t, c.ontologyNow(t).CurrentLoadID)
			assert.Equal(t, *c.ontologyNow(t).CurrentLoadID, job.LoadID)
			pending, err := c.db.ListPendingFoundryDocuments(ctx, job.LoadID)
			require.NoError(t, err)
			assert.Equal(t, []string{n1}, documentIDs(pending))
			assert.False(t, c.ontologyNow(t).ExtractOwed)
		})
	}

	// Work no person started has no requester: a fan-out that says both is
	// refused, and its publish writes nothing.
	t.Run("refuses a requester on work no person started", func(t *testing.T) {
		ctx := context.Background()
		c := newConnectorSync(t, newFoundryFixture(t, "SYSR"), "SYSR")
		load := c.loadReading(t, "L1", c.setID)
		c.loaded(t, "w1")
		p := c.publish("SYSR", []*model.FoundryDocument{c.note("n1", "not_1", "1|a", day)}, nil)
		p.Fan.SystemOnly, p.Fan.RequestedByUserID = true, &person
		require.NoError(t, c.db.ReserveConnectorDocuments(ctx, p.WorkspaceID, documentIDs(p.Add), c.sync.ID, syncWorker))
		_, err := c.db.PublishConnectorDocuments(ctx, p, c.sync.ID, syncWorker)
		assert.ErrorContains(t, err, "work no person started has no requester")
		assert.Empty(t, c.index(t))
		assert.Empty(t, members(t, load))
		assert.Equal(t, []string{n1}, purgeRows(t), "its bytes wait for the sweep")
	})
}
