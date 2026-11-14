package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The aliases a load carries (contract 4.22.0) are its list's resolves that
// chose an entity, of every document: one a type, name and entity, the
// earliest decided, the smallest id of those decided at once; a name chosen
// as two entities gives both; "not in data", another kind and another load
// give none. Paged by id.
func TestFoundryLoadAliases(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "ALIAS")
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	add := func(load, id, kind, doc, name string, entity any, decided time.Time) {
		t.Helper()
		var prop, value any
		if kind == "prop" {
			prop, value = "tier", `"gold"`
		}
		_, err := testDB.Exec(`INSERT INTO foundry_load_decisions (load_id, id, ontology_id, kind, type, doc_id, norm_name, entity_key, prop, value,
  decided_by_user_id, decided_at) VALUES (?, ?, ?, ?, 'Customer', NULLIF(?, ''), NULLIF(?, ''), ?, ?, CAST(? AS JSON), ?, ?)`,
			load, id26(id), f.ontology.ID, kind, doc, name, entity, prop, value, id26("user"), decided)
		require.NoError(t, err)
	}
	add(f.load, "AL-1", "resolve", id26("doc-x"), "acme corp", "acme", at.Add(time.Hour))
	add(f.load, "AL-2", "resolve", id26("doc-y"), "acme corp", "acme", at)
	add(f.load, "AL-4", "resolve", id26("doc-x"), "helio", "acme", at)
	add(f.load, "AL-3", "resolve", id26("doc-y"), "helio", "acme", at)
	add(f.load, "AL-5", "resolve", id26("doc-z"), "helio", "globex", at)
	add(f.load, "AL-6", "resolve", id26("doc-z"), "initech", nil, at)
	add(f.load, "AL-7", "prop", "", "", "acme", at)
	add(id26("FJ-other"), "AL-0", "resolve", id26("doc-x"), "umbrella", "acme", at)

	ids := func(ds []*model.FoundryReviewDecision) []string {
		var out []string
		for _, d := range ds {
			out = append(out, d.ID)
		}
		return out
	}
	all, err := f.db.ListFoundryLoadAliases(ctx, f.load, "", 100)
	require.NoError(t, err)
	assert.Equal(t, []string{id26("AL-2"), id26("AL-3"), id26("AL-5")}, ids(all))
	assert.Equal(t, "acme corp", *all[0].NormName)
	assert.Equal(t, "globex", *all[2].EntityKey)
	assert.True(t, all[0].DecidedAt.Equal(at), "the earliest decided's time: %v", all[0].DecidedAt)
	for _, d := range all {
		require.NoError(t, d.Validate(), "each is a resolve as it was stored")
	}

	first, err := f.db.ListFoundryLoadAliases(ctx, f.load, "", 1)
	require.NoError(t, err)
	rest, err := f.db.ListFoundryLoadAliases(ctx, f.load, first[0].ID, 100)
	require.NoError(t, err)
	assert.Equal(t, ids(all), append(ids(first), ids(rest)...))
	_, err = f.db.ListFoundryLoadAliases(ctx, f.load, "", 0)
	assert.Error(t, err)
}

// Each name an ontology's resolves link to an entity is an alias every
// Extract of a load sends, which takes model.FoundryExtractKindDecisionsMax:
// the ontology takes no resolve that would make one more, though a link
// some decision already makes, a choice that replaces its own document's
// link, and "not in data" still go.
func TestFoundryAliasesFull(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "AFULL")
	// One short of the limit.
	_, err := testDB.Exec(`INSERT INTO foundry_review_decisions (id, ontology_id, kind, type, doc_id, norm_name, entity_key, decided_by_user_id)
SELECT CONCAT('AF', LPAD(a.n + 10*b.n + 100*c.n + 1000*e.n, 24, '0')), ?, 'resolve', 'Customer', ?,
  CONCAT('name ', a.n + 10*b.n + 100*c.n + 1000*e.n), CONCAT('c', a.n + 10*b.n + 100*c.n + 1000*e.n), ?
FROM (SELECT 0 n UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) a,
  (SELECT 0 n UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) b,
  (SELECT 0 n UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) c,
  (SELECT 0 n UNION ALL SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4 UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8 UNION ALL SELECT 9) e
WHERE a.n + 10*b.n + 100*c.n + 1000*e.n < ?`, f.ontology.ID, id26("doc-old"), id26("user"), model.FoundryExtractKindDecisionsMax-1)
	require.NoError(t, err)
	resolve := func(tag, doc, name string, entity *string) *model.FoundryReviewDecision {
		return &model.FoundryReviewDecision{ID: id26("AR-" + tag), OntologyID: f.ontology.ID, Kind: model.FoundryReviewResolve, Type: "Customer",
			DocID: &doc, NormName: &name, EntityKey: entity, DecidedByUserID: id26("user")}
	}
	settle := func(d *model.FoundryReviewDecision) error {
		return f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, d)
	}
	acme, globex, initech := "acme", "globex", "initech"
	require.NoError(t, settle(resolve("1", f.a, "acme corp", &acme)), "the last link the limit takes")
	assert.ErrorIs(t, settle(resolve("2", f.b, "globex", &globex)), store.FoundryAliasesFull)
	require.NoError(t, settle(resolve("3", f.a, "acme corp", &initech)), "a choice that replaces its document's own link")
	require.NoError(t, settle(resolve("4", f.b, "acme corp", &initech)), "a link another document's decision makes")
	require.NoError(t, settle(resolve("5", f.b, "globex", nil)), "not in data carries nothing")
	// Taking a link away makes room.
	_, err = testDB.Exec(`DELETE FROM foundry_review_decisions WHERE ontology_id=? AND id=?`, f.ontology.ID, "AF"+strings.Repeat("0", 24))
	require.NoError(t, err)
	require.NoError(t, settle(resolve("6", f.b, "globex", &globex)))
}

// A document's names and the entities an alias linked are kept on its row
// (contract 4.22.0). A commit's replace the row's; an event of a document
// the load holds, a resend's failed event, and a failure over a failed row
// add to them (model.MergeFoundryDocumentNames), and their carried links
// add to the count.
func TestFoundryDocumentNamesKept(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "NAMES")
	f.approve(t, "1", f.held(f.a, 1, strings.Repeat("cd", 32)))
	job := f.claim(t)
	id1, id2 := "dec-1", "dec-2"
	name := func(decision model.FoundryNameDecision, norm string, id *string, outcome model.FoundryNameOutcomeKind, reason model.FoundryNameRefusal) model.FoundryDocumentName {
		return model.FoundryDocumentName{Decision: decision, Type: "Customer", NormName: norm, DecisionID: id, Outcome: outcome, Reason: reason}
	}
	record := func(doc string, status model.FoundryExtractionStatus, keep bool, carried int, names ...model.FoundryDocumentName) {
		t.Helper()
		e := &model.FoundryDocumentExtraction{DocumentID: doc, Status: status, Counts: model.FoundryExtractionCounts{ResolvedCarried: carried}, Names: names}
		switch status {
		case model.FoundryExtractionFailed:
			e.Error = &model.FoundryExtractionError{Code: model.FoundryExtractionStore, Retryable: true}
		case model.FoundryExtractionDuplicate:
			e.DuplicateOf = &f.b
		}
		require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, f.worker, e, keep))
	}
	kept := func(doc string) *model.FoundryDocumentExtraction {
		t.Helper()
		e, err := f.db.GetFoundryDocumentExtraction(ctx, f.load, doc)
		require.NoError(t, err)
		listed, err := f.db.ListFoundryDocumentExtractions(ctx, f.load, []string{doc})
		require.NoError(t, err)
		assert.Equal(t, e.Names, listed[doc].Names)
		return e
	}
	assert.Nil(t, kept(f.a).Names, "a row written before names were kept has none")

	record(f.a, model.FoundryExtractionExtracted, true, 2,
		name(model.FoundryNameResolve, "acme corp", &id1, model.FoundryNameWritten, ""),
		name(model.FoundryNameAlias, "helio", &id2, model.FoundryNameRefused, model.FoundryNameTwoRows))
	got := kept(f.a)
	assert.Equal(t, 2, got.Counts.ResolvedCarried)
	assert.Equal(t, []model.FoundryDocumentName{
		name(model.FoundryNameResolve, "acme corp", &id1, model.FoundryNameWritten, ""),
		name(model.FoundryNameAlias, "helio", &id2, model.FoundryNameRefused, model.FoundryNameTwoRows),
	}, got.Names)

	// Sent again: what was written reads not_asked and stays written; a
	// later word replaces an earlier one; a new name is added.
	record(f.a, model.FoundryExtractionExtracted, true, 1,
		name(model.FoundryNameResolve, "acme corp", &id1, model.FoundryNameNotAsked, ""),
		name(model.FoundryNameAlias, "helio", &id2, model.FoundryNameWritten, ""),
		name(model.FoundryNameAlias, "initech", nil, model.FoundryNameNotNamed, ""))
	got = kept(f.a)
	assert.Equal(t, 3, got.Counts.ResolvedCarried)
	assert.Equal(t, []model.FoundryDocumentName{
		name(model.FoundryNameResolve, "acme corp", &id1, model.FoundryNameWritten, ""),
		name(model.FoundryNameAlias, "helio", &id2, model.FoundryNameWritten, ""),
		name(model.FoundryNameAlias, "initech", nil, model.FoundryNameNotNamed, ""),
	}, got.Names)

	// A resend's failed event says what its settling wrote.
	require.NoError(t, f.db.RecordFoundryReleaseOutcomes(ctx, job.ID, f.worker, f.a, nil,
		[]model.FoundryDocumentName{name(model.FoundryNameAlias, "initech", nil, model.FoundryNameRefused, "a_later_reason")}))
	assert.Equal(t, name(model.FoundryNameAlias, "initech", nil, model.FoundryNameRefused, "a_later_reason"), kept(f.a).Names[2])
	assert.ErrorIs(t, f.db.RecordFoundryReleaseOutcomes(ctx, job.ID, "another-worker", f.a, nil,
		[]model.FoundryDocumentName{name(model.FoundryNameAlias, "initech", nil, model.FoundryNameWritten, "")}), store.ErrNotFound)

	// A commit replaces them, and its count.
	record(f.a, model.FoundryExtractionExtracted, false, 4, name(model.FoundryNameResolve, "globex", &id1, model.FoundryNameWritten, ""))
	got = kept(f.a)
	assert.Equal(t, 4, got.Counts.ResolvedCarried)
	assert.Equal(t, []model.FoundryDocumentName{name(model.FoundryNameResolve, "globex", &id1, model.FoundryNameWritten, "")}, got.Names)
	record(f.a, model.FoundryExtractionDuplicate, false, 0)
	assert.Nil(t, kept(f.a).Names, "a commit that says none leaves none")

	// A failure over a failure adds to what the first said, as its counts
	// keep the greater.
	record(f.b, model.FoundryExtractionFailed, false, 1, name(model.FoundryNameResolve, "acme corp", &id1, model.FoundryNameWritten, ""))
	record(f.b, model.FoundryExtractionFailed, false, 0, name(model.FoundryNameAlias, "helio", &id2, model.FoundryNameNotAsked, ""))
	record(f.b, model.FoundryExtractionFailed, false, 0)
	got = kept(f.b)
	assert.Equal(t, 1, got.Counts.ResolvedCarried)
	assert.Equal(t, []model.FoundryDocumentName{
		name(model.FoundryNameResolve, "acme corp", &id1, model.FoundryNameWritten, ""),
		name(model.FoundryNameAlias, "helio", &id2, model.FoundryNameNotAsked, ""),
	}, got.Names)
}
