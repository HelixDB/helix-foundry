package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/pkg/namewords"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// addLoadResolve writes a resolve of the load's list: name of doc linked to
// entity (nil for "not in data"), keyed as the settle keys it unless keyed is
// false, or a prop decision.
func addLoadResolve(t *testing.T, f resendFixture, load, id, kind, doc, name string, entity any, decided time.Time, keyed bool) {
	t.Helper()
	var prop, value, key any
	if kind == "prop" {
		prop, value = "tier", `"gold"`
	}
	if k, ok := namewords.Key(name); ok && keyed {
		key = k
	}
	_, err := testDB.Exec(`INSERT INTO foundry_load_decisions (load_id, id, ontology_id, kind, type, doc_id, norm_name, entity_key, prop, value,
  decided_by_user_id, decided_at, name_key) VALUES (?, ?, ?, ?, 'Customer', NULLIF(?, ''), NULLIF(?, ''), ?, ?, CAST(? AS JSON), ?, ?, ?)`,
		load, id26(id), f.ontology.ID, kind, doc, name, entity, prop, value, id26("user"), decided, key)
	require.NoError(t, err)
}

func decisionIDs(ds []*model.FoundryReviewDecision) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.ID)
	}
	slices.Sort(out)
	return out
}

// The aliases a document may write (contract 4.23.0) are read by the keys
// of its words: every resolve of the load's list that chose an entity and
// whose name has one of the keys, of any document, several of one name
// included; "not in data", another kind, another load and another name give
// none. At most FoundryAliasKeysPerRead keys a read.
func TestFoundryLoadAliasesNamed(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "ALIAS")
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	addLoadResolve(t, f, f.load, "AL-1", "resolve", id26("doc-x"), "acme corp", "acme", at.Add(time.Hour), true)
	addLoadResolve(t, f, f.load, "AL-2", "resolve", id26("doc-y"), "acme corp", "acme", at, true)
	addLoadResolve(t, f, f.load, "AL-3", "resolve", id26("doc-y"), "helio", "acme", at, true)
	addLoadResolve(t, f, f.load, "AL-4", "resolve", id26("doc-z"), "helio", "globex", at, true)
	addLoadResolve(t, f, f.load, "AL-5", "resolve", id26("doc-z"), "initech", nil, at, true)
	addLoadResolve(t, f, f.load, "AL-6", "prop", "", "", "acme", at, true)
	addLoadResolve(t, f, f.load, "AL-7", "resolve", id26("doc-w"), "umbrella", "acme", at, true)
	addLoadResolve(t, f, id26("FJ-other"), "AL-0", "resolve", id26("doc-x"), "acme corp", "acme", at, true)

	text := namewords.NewText("ACME Corp. paid Helio; Initech watched.")
	got, err := f.db.ListFoundryLoadAliasesNamed(ctx, f.load, text.Keys())
	require.NoError(t, err)
	assert.Equal(t, []string{id26("AL-1"), id26("AL-2"), id26("AL-3"), id26("AL-4")}, decisionIDs(got))
	for _, d := range got {
		require.NoError(t, d.Validate(), "each is a resolve as it was stored")
		assert.True(t, text.Writes(*d.NormName))
	}
	none, err := f.db.ListFoundryLoadAliasesNamed(ctx, f.load, nil)
	require.NoError(t, err)
	assert.Empty(t, none)
	_, err = f.db.ListFoundryLoadAliasesNamed(ctx, f.load, make([]int64, store.FoundryAliasKeysPerRead+1))
	assert.Error(t, err)

	// The read stays on the load's name index.
	var plan string
	require.NoError(t, testDB.QueryRow(`EXPLAIN FORMAT=JSON SELECT id FROM foundry_load_decisions FORCE INDEX (foundry_load_decisions_names_idx)
WHERE load_id=? AND kind='resolve' AND name_key IN (1, 2, 3) AND entity_key IS NOT NULL`, f.load).Scan(&plan))
	assert.Contains(t, plan, "foundry_load_decisions_names_idx")
	require.NoError(t, testDB.QueryRow(`EXPLAIN FORMAT=JSON SELECT id FROM foundry_load_decisions
WHERE load_id=? AND kind='resolve' AND name_key IN (1, 2, 3) AND entity_key IS NOT NULL`, f.load).Scan(&plan))
	assert.Contains(t, plan, `"key": "foundry_load_decisions_names_idx"`, plan)
}

// A resolve settled before name keys, or copied by an older binary, has
// none until the worker fills the load's: then it is read as any other.
// A second fill finds nothing to do.
func TestFillFoundryLoadNameKeys(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "AFILL")
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	for i := range 1500 {
		addLoadResolve(t, f, f.load, fmt.Sprintf("AF-%d", i), "resolve", id26("doc-x"), fmt.Sprintf("customer %d", i), "acme", at, false)
	}
	addLoadResolve(t, f, f.load, "AF-prop", "prop", "", "", "acme", at, false)
	addLoadResolve(t, f, id26("FJ-other"), "AF-other", "resolve", id26("doc-x"), "customer 7", "acme", at, false)
	text := namewords.NewText("Customer 7 and customer 1499 paid.")
	before, err := f.db.ListFoundryLoadAliasesNamed(ctx, f.load, text.Keys())
	require.NoError(t, err)
	assert.Empty(t, before, "unkeyed, so unread")

	filled, err := f.db.FillFoundryLoadNameKeys(ctx, f.load)
	require.NoError(t, err)
	assert.Equal(t, 1500, filled, "the load's resolves, in pages")
	again, err := f.db.FillFoundryLoadNameKeys(ctx, f.load)
	require.NoError(t, err)
	assert.Zero(t, again)
	after, err := f.db.ListFoundryLoadAliasesNamed(ctx, f.load, text.Keys())
	require.NoError(t, err)
	assert.Equal(t, []string{id26("AF-1499"), id26("AF-7")}, decisionIDs(after))
	var unkeyed int
	require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_decisions WHERE load_id=? AND name_key IS NULL`, id26("FJ-other")).Scan(&unkeyed))
	assert.Equal(t, 1, unkeyed, "another load's are its own")
}

// The settle keys a resolve's name, and a new load's list copies the key,
// so its Extracts find it with no fill; any other kind has none. An
// ontology takes any number of linked names (contract 4.23.0).
func TestSettleKeysAResolvesName(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "AKEY")
	acme := "acme"
	name := "acme corp"
	d := &model.FoundryReviewDecision{ID: id26("AK-1"), OntologyID: f.ontology.ID, Kind: model.FoundryReviewResolve, Type: "Customer",
		DocID: &f.a, NormName: &name, EntityKey: &acme, DecidedByUserID: id26("user")}
	require.NoError(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, d))
	want, _ := namewords.Key(name)
	var key sql.NullInt64
	require.NoError(t, testDB.QueryRow(`SELECT name_key FROM foundry_review_decisions WHERE id=?`, d.ID).Scan(&key))
	assert.Equal(t, sql.NullInt64{Int64: want, Valid: true}, key)

	f.draftVersion(t, "AKEY2", customerTOML)
	next := loadJob(f.ontology, "AKEY2")
	require.NoError(t, f.db.CreateFoundryLoad(ctx, next))
	got, err := f.db.ListFoundryLoadAliasesNamed(ctx, next.LoadID, namewords.NewText("Paid by ACME Corp.").Keys())
	require.NoError(t, err)
	assert.Equal(t, []string{d.ID}, decisionIDs(got))
	filled, err := f.db.FillFoundryLoadNameKeys(ctx, next.LoadID)
	require.NoError(t, err)
	assert.Zero(t, filled, "the copy carried its key")
}

// 100,000 aliases in one load, ten times what one Extract takes: a document
// that writes 50 of them reads those 50 (and their other decisions of the
// same names), by its own words' keys, in reads bounded by the document and
// not by the load. The numbers are logged for the record.
func TestFoundryLoadAliasesAtScale(t *testing.T) {
	ctx := context.Background()
	f := newResendFixture(t, "ASCALE")
	const total, batch = 100000, 2000
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	start := time.Now()
	for from := 0; from < total; from += batch {
		rows := strings.TrimSuffix(strings.Repeat("(?, ?, ?, 'resolve', 'Customer', ?, ?, 'acme', ?, ?, ?), ", batch), ", ")
		args := make([]any, 0, 9*batch)
		for i := from; i < from+batch; i++ {
			name := fmt.Sprintf("customer %06d holdings", i)
			key, _ := namewords.Key(name)
			args = append(args, f.load, fmt.Sprintf("AS%024d", i), f.ontology.ID, id26("doc-x"), name, id26("user"), at, key)
		}
		_, err := testDB.Exec(`INSERT INTO foundry_load_decisions (load_id, id, ontology_id, kind, type, doc_id, norm_name, entity_key, decided_by_user_id, decided_at, name_key)
VALUES `+rows, args...)
		require.NoError(t, err)
	}
	t.Logf("wrote %d aliases in %v", total, time.Since(start))

	// A 60 KB document: 50 of the names among other words.
	var doc strings.Builder
	var wanted []string
	for i := range 50 {
		n := i * (total / 50)
		wanted = append(wanted, fmt.Sprintf("AS%024d", n))
		fmt.Fprintf(&doc, "On Monday Customer %06d Holdings paid the invoice in full, as agreed with the supplier and its auditors. ", n)
		doc.WriteString(strings.Repeat("Nothing more was said of it that day, nor the next. ", 20))
	}
	slices.Sort(wanted)
	start = time.Now()
	text := namewords.NewText(doc.String())
	keys := text.Keys()
	var found []*model.FoundryReviewDecision
	reads := 0
	for chunk := range slices.Chunk(keys, store.FoundryAliasKeysPerRead) {
		page, err := f.db.ListFoundryLoadAliasesNamed(ctx, f.load, chunk)
		require.NoError(t, err)
		reads++
		for _, d := range page {
			if text.Writes(*d.NormName) {
				found = append(found, d)
			}
		}
	}
	took := time.Since(start)
	assert.Equal(t, wanted, decisionIDs(found))
	aliasBytes := 0
	for _, d := range found {
		raw, _ := json.Marshal(map[string]any{"kind": "alias", "type": d.Type, "norm_name": *d.NormName, "entity_key": *d.EntityKey,
			"decision_id": d.ID, "decided_at": d.DecidedAt.UTC()})
		aliasBytes += len(raw) + 1
	}
	t.Logf("document %d bytes, %d keys, %d reads, %d aliases (%d request bytes; all %d would be ~%d), in %v",
		doc.Len(), len(keys), reads, len(found), aliasBytes, total, aliasBytes/len(found)*total, took)
	assert.Less(t, took, 10*time.Second, "bounded by the document")
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
