package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// modelFailed is a document the model failed with class f.
func modelFailed(documentID, f string) *model.FoundryDocumentExtraction {
	return &model.FoundryDocumentExtraction{DocumentID: documentID, Status: model.FoundryExtractionFailed,
		Error: &model.FoundryExtractionError{Code: model.FoundryExtractionModel, Retryable: true, ModelFailure: f}}
}

// rereadClasses are the classes the sync worker passes (ai.RereadFailures).
var rereadClasses = []string{"quota", "auth", "permission", "not_found", "unavailable", "rate_limited", "timeout"}

// unreadLoad writes four documents and a load of them whose job records
// them by record (a, b, c, d) and ends with status, the load current.
func unreadLoad(t *testing.T, tag string, status model.FoundryGraphJobStatus,
	record func(f foundryFixture, job *model.FoundryGraphJob, docs map[string]string)) (foundryFixture, string, map[string]string) {
	t.Helper()
	ctx := context.Background()
	f := newFoundryFixture(t, tag)
	docs := map[string]string{}
	for _, d := range []string{"a", "b", "c", "d"} {
		docs[d] = f.uploaded(t, tag+"-"+d)
	}
	loadID := f.load(t, "L1")
	job := f.claimed(t, "w1", loadID)
	record(f, job, docs)
	_, err := f.db.FinishFoundryGraphJob(ctx, job.ID, "w1", model.FoundryGraphJobOutcome{Status: status, IngestDone: true, Next: next(tag + "-1")})
	require.NoError(t, err)
	return f, loadID, docs
}

func TestFoundryLoadUnread(t *testing.T) {
	ctx := context.Background()

	// The class is kept on a model-failed row, read back with it, and
	// cleared when a later job reads the document; no other code takes one.
	t.Run("keeps the model failure class with its row", func(t *testing.T) {
		f := newFoundryFixture(t, "LU1")
		doc := f.uploaded(t, "LU1-a")
		other := f.uploaded(t, "LU1-b")
		loadID := f.load(t, "L1")
		job := f.claimed(t, "w1", loadID)
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", modelFailed(doc, "quota"), false))
		e, err := f.db.GetFoundryDocumentExtraction(ctx, loadID, doc)
		require.NoError(t, err)
		assert.Equal(t, &model.FoundryExtractionError{Code: model.FoundryExtractionModel, Retryable: true, ModelFailure: "quota"}, e.Error)

		// Failing again records the newer class.
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", modelFailed(doc, "rate_limited"), false))
		e, err = f.db.GetFoundryDocumentExtraction(ctx, loadID, doc)
		require.NoError(t, err)
		assert.Equal(t, "rate_limited", e.Error.ModelFailure)

		// A class on a failure that is not the model's is refused by the
		// table, so no row can say a store failure was the provider's quota.
		bad := &model.FoundryDocumentExtraction{DocumentID: other, Status: model.FoundryExtractionFailed,
			Error: &model.FoundryExtractionError{Code: model.FoundryExtractionStore, Retryable: true, ModelFailure: "quota"}}
		err = f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", bad, false)
		require.Error(t, err)
		assert.True(t, strings.Contains(err.Error(), "model_failure_chk"), err.Error())

		// Read in the end, by a new extraction or by one held all along.
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", extracted(doc), false))
		e, err = f.db.GetFoundryDocumentExtraction(ctx, loadID, doc)
		require.NoError(t, err)
		assert.Nil(t, e.Error)
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", modelFailed(other, "timeout"), false))
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", extracted(other), true))
		e, err = f.db.GetFoundryDocumentExtraction(ctx, loadID, other)
		require.NoError(t, err)
		assert.Equal(t, model.FoundryExtractionExtracted, e.Status)
		assert.Nil(t, e.Error)
	})

	// A model-failed document is unread, never read; documents no job
	// recorded count with them once no job writes the load.
	t.Run("counts what the model left unread", func(t *testing.T) {
		f := newFoundryFixture(t, "LU2")
		docs := []string{f.uploaded(t, "LU2-a"), f.uploaded(t, "LU2-b"), f.uploaded(t, "LU2-c"), f.uploaded(t, "LU2-d"), f.uploaded(t, "LU2-e")}
		loadID := f.load(t, "L1")
		job := f.claimed(t, "w1", loadID)
		for _, e := range []*model.FoundryDocumentExtraction{
			extracted(docs[0]), {DocumentID: docs[1], Status: model.FoundryExtractionDuplicate, DuplicateOf: &docs[0]},
			modelFailed(docs[2], "quota"), modelFailed(docs[3], ""),
		} {
			require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", e, false))
		}
		got, err := f.db.ListFoundryLoadUnread(ctx, []string{loadID, id26("FJ-none")})
		require.NoError(t, err)
		u := got[loadID]
		assert.Equal(t, 2, u.Read)
		assert.Equal(t, map[string]int{"quota": 1, "": 1}, u.Failed)
		assert.Equal(t, 1, u.Unrecorded)
		assert.True(t, u.Live, "the load job is running")
		assert.Equal(t, 2, u.Failures(), "what no job reached waits for a reason of its own")
		assert.Nil(t, u.LastEnd)
		assert.Equal(t, model.FoundryLoadUnread{}, got[id26("FJ-none")])

		_, err = f.db.FinishFoundryGraphJob(ctx, job.ID, "w1", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobBudgetExhausted, IngestDone: true, Next: next("LU2-1")})
		require.NoError(t, err)
		got, err = f.db.ListFoundryLoadUnread(ctx, []string{loadID})
		require.NoError(t, err)
		u = got[loadID]
		assert.False(t, u.Live)
		assert.Equal(t, 2, u.Failures())
		assert.Equal(t, 1, u.Unrecorded)
		require.NotNil(t, u.LastEnd)
		assert.Equal(t, 0, u.Rereads)
		assert.Equal(t, model.FoundryLoadPartial, f.loadStatus(t, loadID))
	})

	// The sweep finds a current load owed a re-read once, queues it once it
	// is due, and the re-read that reads everything makes the load ready.
	t.Run("re-reads a current load when due, then makes it ready", func(t *testing.T) {
		f, loadID, docs := unreadLoad(t, "LU3", model.FoundryGraphJobBudgetExhausted, func(f foundryFixture, job *model.FoundryGraphJob, docs map[string]string) {
			for _, e := range []*model.FoundryDocumentExtraction{
				extracted(docs["a"]), extracted(docs["b"]), modelFailed(docs["c"], "quota"), modelFailed(docs["d"], "quota"),
			} {
				require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", e, false))
			}
		})
		require.Equal(t, model.FoundryLoadPartial, f.loadStatus(t, loadID))
		require.Equal(t, loadID, *f.ontologyNow(t).CurrentLoadID)

		owed, err := f.db.ListFoundryRereads(ctx, rereadClasses, "", 100)
		require.NoError(t, err)
		require.Contains(t, ids(owed), f.ontology.ID)

		// Not before 15 minutes have passed since the job ended.
		ws, o := f.ontology.WorkspaceID, f.ontology.ID
		queued, err := f.db.QueueFoundryReread(ctx, ws, o, rereadClasses, &model.FoundryGraphJob{ID: id26("FRR-LU3-0")}, noBudget, time.Now())
		require.NoError(t, err)
		assert.False(t, queued)
		assert.Nil(t, f.liveJob(t))

		// A spent month waits.
		spent := model.FoundryGraphBudget{Since: time.Unix(0, 0), CapMicros: 1}
		queued, err = f.db.QueueFoundryReread(ctx, ws, o, rereadClasses, &model.FoundryGraphJob{ID: id26("FRR-LU3-1")}, spent,
			time.Now().Add(16*time.Minute))
		require.NoError(t, err)
		assert.False(t, queued)

		queued, err = f.db.QueueFoundryReread(ctx, ws, o, rereadClasses, &model.FoundryGraphJob{ID: id26("FRR-LU3-2")}, noBudget,
			time.Now().Add(16*time.Minute))
		require.NoError(t, err)
		require.True(t, queued)
		live := f.liveJob(t)
		require.NotNil(t, live)
		assert.Equal(t, model.FoundryGraphJobExtract, live.Kind)
		assert.Equal(t, loadID, live.LoadID)
		assert.Equal(t, model.FoundryRereadToken+id26("FRR-LU3-2"), *live.ClientToken)

		// With a job live, the sweep passes it by.
		owed, err = f.db.ListFoundryRereads(ctx, rereadClasses, "", 100)
		require.NoError(t, err)
		assert.NotContains(t, ids(owed), f.ontology.ID)

		// The re-read reads one: still partial, the next one twice as late.
		job := f.claimed(t, "w2", live.ID)
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w2", extracted(docs["c"]), false))
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w2", modelFailed(docs["d"], "rate_limited"), false))
		f.ended(t, "w2", job.ID, model.FoundryGraphJobSucceeded, next("LU3-2"))
		assert.Equal(t, model.FoundryLoadPartial, f.loadStatus(t, loadID))
		got, err := f.db.ListFoundryLoadUnread(ctx, []string{loadID})
		require.NoError(t, err)
		u := got[loadID]
		assert.Equal(t, 1, u.Rereads)
		assert.Equal(t, map[string]int{"rate_limited": 1}, u.Failed)
		at, ok := u.RereadAt()
		require.True(t, ok)
		assert.Equal(t, u.LastEnd.Add(30*time.Minute), at)
		queued, err = f.db.QueueFoundryReread(ctx, ws, o, rereadClasses, &model.FoundryGraphJob{ID: id26("FRR-LU3-3")}, noBudget,
			time.Now().Add(20*time.Minute))
		require.NoError(t, err)
		assert.False(t, queued, "the second re-read waits 30 minutes")
		queued, err = f.db.QueueFoundryReread(ctx, ws, o, rereadClasses, &model.FoundryGraphJob{ID: id26("FRR-LU3-4")}, noBudget,
			time.Now().Add(31*time.Minute))
		require.NoError(t, err)
		require.True(t, queued)

		// The last one read: the load is ready, and owed nothing.
		job = f.claimed(t, "w3", id26("FRR-LU3-4"))
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w3", extracted(docs["d"]), false))
		f.ended(t, "w3", job.ID, model.FoundryGraphJobSucceeded, next("LU3-4"))
		assert.Equal(t, model.FoundryLoadReady, f.loadStatus(t, loadID))
		owed, err = f.db.ListFoundryRereads(ctx, rereadClasses, "", 100)
		require.NoError(t, err)
		assert.NotContains(t, ids(owed), f.ontology.ID)
		got, err = f.db.ListFoundryLoadUnread(ctx, []string{loadID})
		require.NoError(t, err)
		assert.Equal(t, 0, got[loadID].Failures())
	})

	// A failure that doesn't pass on its own (the model rejected the
	// request) waits for the next job; a failed load is never re-read.
	t.Run("re-reads neither failures that don't pass nor a failed load", func(t *testing.T) {
		f, _, _ := unreadLoad(t, "LU4", model.FoundryGraphJobBudgetExhausted, func(f foundryFixture, job *model.FoundryGraphJob, docs map[string]string) {
			require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", extracted(docs["a"]), false))
			require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", modelFailed(docs["b"], "unsupported"), false))
		})
		owed, err := f.db.ListFoundryRereads(ctx, rereadClasses, "", 100)
		require.NoError(t, err)
		assert.NotContains(t, ids(owed), f.ontology.ID)
		queued, err := f.db.QueueFoundryReread(ctx, f.ontology.WorkspaceID, f.ontology.ID, rereadClasses,
			&model.FoundryGraphJob{ID: id26("FRR-LU4")}, noBudget, time.Now().Add(time.Hour))
		require.NoError(t, err)
		assert.False(t, queued)

		g, loadID, _ := unreadLoad(t, "LU5", model.FoundryGraphJobFailed, func(f foundryFixture, job *model.FoundryGraphJob, docs map[string]string) {
			for _, d := range docs {
				require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", modelFailed(d, "quota"), false))
			}
		})
		assert.Equal(t, model.FoundryLoadFailed, g.loadStatus(t, loadID))
		assert.Nil(t, g.ontologyNow(t).CurrentLoadID)
		owed, err = g.db.ListFoundryRereads(ctx, rereadClasses, "", 100)
		require.NoError(t, err)
		assert.NotContains(t, ids(owed), g.ontology.ID)
	})

	// A person's new load the model left documents unread of replaces the
	// current load, as a ready one would, so its documents are re-read; one a
	// spent budget stopped doesn't.
	t.Run("makes a person's load the model left documents of current", func(t *testing.T) {
		f := newFoundryFixture(t, "LU7")
		a, b := f.uploaded(t, "LU7-a"), f.uploaded(t, "LU7-b")
		first := f.load(t, "L1")
		f.loaded(t, "w1", a, b)
		require.Equal(t, first, *f.ontologyNow(t).CurrentLoadID)

		budget := f.load(t, "L2")
		f.finish(t, "w1", "LU7-2", model.FoundryGraphJobBudgetExhausted, a)
		assert.Equal(t, model.FoundryLoadPartial, f.loadStatus(t, budget))
		assert.Equal(t, first, *f.ontologyNow(t).CurrentLoadID, "a budget stop waits for a person")

		unread := f.load(t, "L3")
		job := f.claimed(t, "w1", unread)
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", extracted(a), false))
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", modelFailed(b, "rate_limited"), false))
		_, err := f.db.FinishFoundryGraphJob(ctx, job.ID, "w1", model.FoundryGraphJobOutcome{
			Status: model.FoundryGraphJobBudgetExhausted, IngestDone: true, Next: next("LU7-3")})
		require.NoError(t, err)
		assert.Equal(t, model.FoundryLoadPartial, f.loadStatus(t, unread))
		assert.Equal(t, unread, *f.ontologyNow(t).CurrentLoadID)
		owed, err := f.db.ListFoundryRereads(ctx, rereadClasses, "", 100)
		require.NoError(t, err)
		assert.Contains(t, ids(owed), f.ontology.ID)
	})

	// Not yet due, a re-read is refused without the ontology's lock; so is one
	// past the limit.
	t.Run("refuses a re-read not yet due, or past the limit", func(t *testing.T) {
		f, loadID, _ := unreadLoad(t, "LU8", model.FoundryGraphJobBudgetExhausted, func(f foundryFixture, job *model.FoundryGraphJob, docs map[string]string) {
			for _, e := range []*model.FoundryDocumentExtraction{
				extracted(docs["a"]), extracted(docs["c"]), extracted(docs["d"]), modelFailed(docs["b"], "quota"),
			} {
				require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "w1", e, false))
			}
		})
		ws, o := f.ontology.WorkspaceID, f.ontology.ID
		for i := range model.FoundryRereadLimit {
			queued, err := f.db.QueueFoundryReread(ctx, ws, o, rereadClasses, &model.FoundryGraphJob{ID: id26(fmt.Sprintf("FRR-LU8-%02d-", i))},
				noBudget, time.Now().Add(7*time.Hour))
			require.NoError(t, err)
			require.True(t, queued, "re-read %d", i)
			job := f.claimed(t, "w2", id26(fmt.Sprintf("FRR-LU8-%02d-", i)))
			f.ended(t, "w2", job.ID, model.FoundryGraphJobSucceeded, next(fmt.Sprintf("LU8-%d", i)))
		}
		got, err := f.db.ListFoundryLoadUnread(ctx, []string{loadID})
		require.NoError(t, err)
		assert.Equal(t, model.FoundryRereadLimit, got[loadID].Rereads)
		queued, err := f.db.QueueFoundryReread(ctx, ws, o, rereadClasses, &model.FoundryGraphJob{ID: id26("FRR-LU8-x")}, noBudget,
			time.Now().Add(30*24*time.Hour))
		require.NoError(t, err)
		assert.False(t, queued, "past the limit")
		assert.Nil(t, f.liveJob(t))
	})

	t.Run("lists nothing for no classes", func(t *testing.T) {
		f := newFoundryFixture(t, "LU6")
		owed, err := f.db.ListFoundryRereads(ctx, nil, "", 100)
		require.NoError(t, err)
		assert.Empty(t, owed)
		queued, err := f.db.QueueFoundryReread(ctx, f.ontology.WorkspaceID, f.ontology.ID, nil, &model.FoundryGraphJob{ID: id26("FRR-LU6")},
			noBudget, time.Now())
		require.NoError(t, err)
		assert.False(t, queued)
	})
}

// ids are the ontologies' ids.
func ids(os []*model.FoundryOntology) []string {
	out := make([]string, 0, len(os))
	for _, o := range os {
		out = append(out, o.ID)
	}
	return out
}
