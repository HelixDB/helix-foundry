package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/helixdb/helix-foundry/backend/store/model"
	"slices"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// draftVersion is an agent draft of ontologyTOML, FV-<tag>.
func (f foundryFixture) draftVersion(t *testing.T, tag, ontologyTOML string) *model.OntologyVersion {
	t.Helper()
	def, err := json.Marshal(map[string]string{"ontology_toml": ontologyTOML})
	require.NoError(t, err)
	v := &model.OntologyVersion{ID: id26("FV-" + tag), WorkspaceID: f.ontology.WorkspaceID, OntologyID: f.ontology.ID,
		Format: model.OntologyFormatAgentBundle, Source: model.OntologySourceGenerated, Definition: def, DocumentSetIDs: &[]string{generalSetID(f.ontology.ID)}}
	require.NoError(t, f.db.CreateOntologyVersion(context.Background(), v))
	return v
}

// currentLoad writes a draft of ontologyTOML, loads it and ends the load
// ready, so it is the load reviews are settled from; it returns the version.
func (f foundryFixture) currentLoad(t *testing.T, tag, ontologyTOML string) *model.OntologyVersion {
	t.Helper()
	v := f.draftVersion(t, tag, ontologyTOML)
	job := loadJob(f.ontology, tag)
	job.VersionID = v.ID
	require.NoError(t, f.db.CreateFoundryLoad(context.Background(), job))
	f.loaded(t, "w-"+tag)
	return v
}

// decisionIDs lists the ontology's decisions' ids.
func (f foundryFixture) decisionIDs(t *testing.T) []string {
	t.Helper()
	all, err := f.db.ListFoundryReviewDecisions(context.Background(), f.ontology.ID, "", nil, 100)
	require.NoError(t, err)
	var ids []string
	for _, d := range all {
		ids = append(ids, d.ID)
	}
	return ids
}

const customerTOML = "[types.Customer]\nprops = { name = \"string\", tier = \"string\" }"

func TestFoundryReviewDecisions(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "DEC")
	resolve := &model.FoundryReviewDecision{ID: id26("R1"), OntologyID: f.ontology.ID, Kind: model.FoundryReviewResolve, Type: "Customer", DocID: ptr("doc-1"), NormName: ptr("a name"), EntityKey: ptr("customer 1"), DecidedByUserID: id26("user")}
	assert.ErrorIs(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, resolve), store.ErrConflict, "no load to settle a review of")
	f.currentLoad(t, "DEC-load", customerTOML)
	require.NoError(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, resolve))
	assert.False(t, resolve.DecidedAt.IsZero())
	// The database itself enforces identity uniqueness despite nullable columns.
	_, duplicateErr := testDB.ExecContext(ctx, `INSERT INTO foundry_review_decisions (id, ontology_id, kind, type, doc_id, norm_name, entity_key, prop, value, decided_by_user_id)
 SELECT ?, ontology_id, kind, type, doc_id, norm_name, entity_key, prop, value, decided_by_user_id FROM foundry_review_decisions WHERE id=?`, id26("duplicate"), resolve.ID)
	require.ErrorContains(t, duplicateErr, "Duplicate entry")

	// NULL-bearing identities are unique, and choosing none replaces the link.
	replacement := *resolve
	replacement.ID = id26("R2")
	replacement.EntityKey = nil
	require.NoError(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, &replacement))
	assert.Equal(t, resolve.ID, replacement.ID)
	other := *resolve
	other.ID = id26("R3")
	other.DocID = ptr("doc-2")
	require.NoError(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, &other))
	prop := &model.FoundryReviewDecision{ID: id26("P1"), OntologyID: f.ontology.ID, Kind: model.FoundryReviewProp, Type: "Customer", EntityKey: ptr("customer 1"), Prop: ptr("name"), Value: json.RawMessage(`"Chosen"`), DecidedByUserID: id26("user")}
	require.NoError(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, prop))
	changed := *prop
	changed.ID = id26("P2")
	changed.Value = json.RawMessage(`null`)
	require.NoError(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, &changed))
	assert.Equal(t, prop.ID, changed.ID)
	all, err := f.db.ListFoundryReviewDecisions(ctx, f.ontology.ID, "", nil, 100)
	require.NoError(t, err)
	require.Len(t, all, 3)
	for _, d := range all {
		require.NoError(t, d.Validate())
	}
	filtered, err := f.db.ListFoundryReviewDecisions(ctx, f.ontology.ID, "", []string{"doc-1"}, 100)
	require.NoError(t, err)
	require.Len(t, filtered, 2)
	for _, d := range filtered {
		if d.Kind == model.FoundryReviewResolve {
			assert.Nil(t, d.EntityKey)
			assert.Equal(t, "doc-1", *d.DocID)
		} else {
			assert.JSONEq(t, `null`, string(d.Value))
		}
	}
	props, err := f.db.ListFoundryReviewDecisions(ctx, f.ontology.ID, "", []string{}, 100)
	require.NoError(t, err)
	require.Len(t, props, 1)
	page, err := f.db.ListFoundryReviewDecisions(ctx, f.ontology.ID, all[0].ID, nil, 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, all[1].ID, page[0].ID)
	_, err = f.db.ListFoundryReviewDecisions(ctx, f.ontology.ID, "", nil, 10002)
	require.Error(t, err)
	isolated, err := f.db.ListFoundryReviewDecisions(ctx, id26("another"), "", nil, 100)
	require.NoError(t, err)
	assert.Empty(t, isolated)
	assert.ErrorIs(t, f.db.DeleteFoundryReviewDecision(ctx, id26("another"), resolve.ID), store.ErrNotFound)
	assert.ErrorIs(t, f.db.SettleFoundryReviewDecision(ctx, id26("wrong-workspace"), resolve), store.ErrNotFound)
	require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.ID, resolve.ID))
	assert.ErrorIs(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.ID, resolve.ID), store.ErrNotFound)

	// Settling checks what the current load's version takes, as the engine
	// would: a value it refuses, a key that isn't a normalized name, a prop
	// or type the version lacks.
	for name, change := range map[string]func(*model.FoundryReviewDecision){
		"a blank value":   func(d *model.FoundryReviewDecision) { d.Value = json.RawMessage(`"  "`) },
		"a number":        func(d *model.FoundryReviewDecision) { d.Value = json.RawMessage(`3`) },
		"an unnormalized": func(d *model.FoundryReviewDecision) { d.EntityKey = ptr("Customer-1") },
		"a missing prop":  func(d *model.FoundryReviewDecision) { d.Prop = ptr("industry") },
		"a missing type":  func(d *model.FoundryReviewDecision) { d.Type = "Contract" },
	} {
		stale := *prop
		stale.ID = id26("P-" + name)
		change(&stale)
		assert.ErrorIs(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, &stale), store.ErrConflict, name)
	}
}

// An integer decision keeps every digit through storage and the next load's
// snapshot, which the graph worker sends the engine.
func TestFoundryReviewDecisionIntegerValues(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "DINT")
	const countTOML = "[types.Customer]\nprops = { name = \"string\", count = \"integer\" }"
	f.currentLoad(t, "DINT-1", countTOML)
	values := []string{`9007199254740993`, `9223372036854775807`, `-9223372036854775808`}
	for i, value := range values {
		d := &model.FoundryReviewDecision{ID: id26(fmt.Sprintf("DINT-%d", i)), OntologyID: f.ontology.ID, Kind: model.FoundryReviewProp, Type: "Customer",
			EntityKey: ptr(fmt.Sprintf("customer %d", i)), Prop: ptr("count"), Value: json.RawMessage(value), DecidedByUserID: id26("user")}
		require.NoError(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, d))
		assert.Equal(t, value, string(d.Value), "the settled row")
	}
	stored, err := f.db.ListFoundryReviewDecisions(ctx, f.ontology.ID, "", nil, 100)
	require.NoError(t, err)
	next := loadJob(f.ontology, "DINT-2")
	next.VersionID = f.draftVersion(t, "DINT-2", countTOML).ID
	require.NoError(t, f.db.CreateFoundryLoad(ctx, next))
	snapshot, err := f.db.ListFoundryLoadDecisions(ctx, next.LoadID, "", nil, 100)
	require.NoError(t, err)
	for _, got := range [][]*model.FoundryReviewDecision{stored, snapshot} {
		require.Len(t, got, len(values))
		for i, d := range got {
			assert.Equal(t, values[i], string(d.Value))
		}
	}
}

// Decisions are kept across loads and versions, and dropped once an Extract of
// the version the ontology points at doesn't take them. A draft that never
// becomes current, or a Definition v1, which no load writes, prunes nothing.
func TestFoundryReviewDecisionPrune(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "DPRUNE")
	current := f.currentLoad(t, "DPRUNE-1", customerTOML)
	require.NoError(t, f.db.SetCurrentOntologyVersion(ctx, f.ontology.ID, current.ID, nil))
	settle := func(d *model.FoundryReviewDecision) *model.FoundryReviewDecision {
		t.Helper()
		d.OntologyID, d.Type, d.EntityKey, d.DecidedByUserID = f.ontology.ID, "Customer", ptr("helio"), id26("user")
		require.NoError(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, d))
		return d
	}
	tier := settle(&model.FoundryReviewDecision{ID: id26("D-tier"), Kind: model.FoundryReviewProp, Prop: ptr("tier"), Value: json.RawMessage(`"gold"`)})
	name := settle(&model.FoundryReviewDecision{ID: id26("D-name"), Kind: model.FoundryReviewProp, Prop: ptr("name"), Value: json.RawMessage(`"Helio"`)})
	link := settle(&model.FoundryReviewDecision{ID: id26("D-link"), Kind: model.FoundryReviewResolve, DocID: ptr("doc-1"), NormName: ptr("helio inc")})
	all := []string{tier.ID, name.ID, link.ID}
	slices.Sort(all)
	require.Equal(t, all, f.decisionIDs(t))

	// A generated draft without Customer, never made current, and a
	// hand-written Definition v1 leave every decision, and settling still
	// reads the current load's version.
	client := f.draftVersion(t, "DPRUNE-client", "[types.Client]\nprops = { name = \"string\" }")
	v1 := &model.OntologyVersion{ID: id26("FV-DPRUNE-v1"), WorkspaceID: f.ontology.WorkspaceID, OntologyID: f.ontology.ID,
		Format: model.OntologyFormatFoundryV1, Source: model.OntologySourceManual, Definition: json.RawMessage(`{"entities":[]}`), DocumentSetIDs: &[]string{}}
	require.NoError(t, f.db.CreateOntologyVersion(ctx, v1))
	require.Equal(t, all, f.decisionIDs(t))
	require.NoError(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, tier))
	require.NoError(t, f.db.SetCurrentOntologyVersion(ctx, f.ontology.ID, v1.ID, &current.ID))
	require.Equal(t, all, f.decisionIDs(t), "a Definition v1 names no types to check against")
	empty := f.draftVersion(t, "DPRUNE-empty", "")
	require.NoError(t, f.db.SetCurrentOntologyVersion(ctx, f.ontology.ID, empty.ID, &v1.ID))
	require.Equal(t, all, f.decisionIDs(t), "a draft with no types is no ontology to drop decisions for")

	// Making current a version where tier is an integer drops the tier's
	// string, then one where Customer is keyed drops the name's named key.
	retyped := f.draftVersion(t, "DPRUNE-retyped", "[types.Customer]\nprops = { name = \"string\", tier = \"integer\" }")
	require.NoError(t, f.db.SetCurrentOntologyVersion(ctx, f.ontology.ID, retyped.ID, &empty.ID))
	kept := []string{name.ID, link.ID}
	slices.Sort(kept)
	assert.Equal(t, kept, f.decisionIDs(t))
	keyed := f.draftVersion(t, "DPRUNE-keyed", "[types.Customer]\nprops = { account = \"string\", name = \"string\" }\nkey = [\"account\"]")
	require.NoError(t, f.db.SetCurrentOntologyVersion(ctx, f.ontology.ID, keyed.ID, &retyped.ID))

	// The resolve stays, though the key no longer reads the entity it chose:
	// the next load copies it, and the engine reviews its name again
	// (Choice::Stale) rather than match it another way.
	assert.Equal(t, []string{link.ID}, f.decisionIDs(t))
	job := loadJob(f.ontology, "DPRUNE-keyed")
	job.VersionID = keyed.ID
	require.NoError(t, f.db.CreateFoundryLoad(ctx, job))
	snapshot, err := f.db.ListFoundryLoadDecisions(ctx, job.LoadID, "", []string{"doc-1"}, 100)
	require.NoError(t, err)
	require.Len(t, snapshot, 1)
	assert.Equal(t, *link, *snapshot[0], "the load sends the stale choice")

	// Settled again from that load's review, the choice replaces it.
	f.loaded(t, "w-DPRUNE-keyed")
	again := &model.FoundryReviewDecision{ID: id26("D-link-again"), OntologyID: f.ontology.ID, Kind: model.FoundryReviewResolve, Type: "Customer",
		DocID: ptr("doc-1"), NormName: ptr("helio inc"), EntityKey: ptr("sA-1"), DecidedByUserID: id26("user")}
	require.NoError(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, again))
	assert.Equal(t, link.ID, again.ID)
	assert.Equal(t, []string{link.ID}, f.decisionIDs(t))

	// A version without Customer drops it.
	require.NoError(t, f.db.SetCurrentOntologyVersion(ctx, f.ontology.ID, client.ID, &keyed.ID))
	assert.Empty(t, f.decisionIDs(t))
	assert.ErrorIs(t, f.db.SetCurrentOntologyVersion(ctx, f.ontology.ID, current.ID, &retyped.ID), store.ErrConflict, "the pointer moved on")
}

func TestFoundryReviewResolutionCounts(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "DCOUNT")
	load := f.load(t, "DCOUNT")
	doc := f.uploaded(t, "DCOUNT")
	job, err := f.db.ClaimFoundryGraphJob(ctx, "counts", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, job)
	counts := model.FoundryExtractionCounts{Sections: 1, Attributes: 2, AttributeReviews: 3, PropConflicts: 4, Created: 5, IdentifiersUnverified: 6, AttributesOverEntries: 7, ResolvedUser: 8, ResolvedAdjudicated: 9, ResolvedIdentifier: 10, ResolvedName: 11, ResolvedAlias: 12, ResolvedCreated: 13}
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "counts", &model.FoundryDocumentExtraction{DocumentID: doc, Status: model.FoundryExtractionExtracted, Counts: counts}, false))
	got, err := f.db.GetFoundryDocumentExtraction(ctx, load, doc)
	require.NoError(t, err)
	assert.Equal(t, counts, got.Counts)
	counts.ResolvedUser = 17
	counts.ResolvedAdjudicated = 19
	require.NoError(t, f.db.RecordFoundryDocumentExtraction(ctx, job.ID, "counts", &model.FoundryDocumentExtraction{DocumentID: doc, Status: model.FoundryExtractionExtracted, Counts: counts}, false))
	listed, err := f.db.ListFoundryDocumentExtractions(ctx, load, []string{doc})
	require.NoError(t, err)
	assert.Equal(t, counts, listed[doc].Counts)
}

// Every extraction attempt uses the load's choices, even if live choices are
// replaced, deleted or pruned by a new version before that attempt runs.
func TestFoundryLoadDecisionSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "DSNAP")
	version := f.currentLoad(t, "DSNAP", "[types.Customer]\nprops = { name = \"string\" }")
	choices := []*model.FoundryReviewDecision{
		{ID: id26("snap-1"), OntologyID: f.ontology.ID, Kind: model.FoundryReviewResolve, Type: "Customer", DocID: ptr("doc-1"), NormName: ptr("variant"), EntityKey: ptr("original"), DecidedByUserID: id26("user")},
		{ID: id26("snap-2"), OntologyID: f.ontology.ID, Kind: model.FoundryReviewResolve, Type: "Customer", DocID: ptr("doc-2"), NormName: ptr("variant"), DecidedByUserID: id26("user")},
		{ID: id26("snap-3"), OntologyID: f.ontology.ID, Kind: model.FoundryReviewProp, Type: "Customer", EntityKey: ptr("original"), Prop: ptr("name"), Value: json.RawMessage(`"Original"`), DecidedByUserID: id26("user")},
	}
	for _, d := range choices {
		require.NoError(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, d))
	}
	job := loadJob(f.ontology, "DSNAP1")
	job.VersionID = version.ID
	require.NoError(t, f.db.CreateFoundryLoad(ctx, job))
	original, err := f.db.ListFoundryLoadDecisions(ctx, job.LoadID, "", nil, 100)
	require.NoError(t, err)
	require.Len(t, original, 3)
	replacement := *choices[0]
	replacement.EntityKey = ptr("replacement")
	require.NoError(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, &replacement))
	require.NoError(t, f.db.DeleteFoundryReviewDecision(ctx, f.ontology.ID, choices[1].ID))
	pruning := f.draftVersion(t, "DSNAP2", "[types.Customer]")
	require.NoError(t, f.db.SetCurrentOntologyVersion(ctx, f.ontology.ID, pruning.ID, nil))
	version = pruning
	for range 2 { // retry / catch-up reads are identical
		got, err := f.db.ListFoundryLoadDecisions(ctx, job.LoadID, "", nil, 100)
		require.NoError(t, err)
		assert.Equal(t, original, got)
	}
	filtered, err := f.db.ListFoundryLoadDecisions(ctx, job.LoadID, "", []string{"doc-1"}, 100)
	require.NoError(t, err)
	assert.Len(t, filtered, 2)
	page, err := f.db.ListFoundryLoadDecisions(ctx, job.LoadID, original[0].ID, nil, 1)
	require.NoError(t, err)
	require.Len(t, page, 1)
	assert.Equal(t, original[1], page[0])
	f.finish(t, "snapshot", "done", model.FoundryGraphJobFailed)
	next := loadJob(f.ontology, "DSNAP2")
	next.VersionID = version.ID
	require.NoError(t, f.db.CreateFoundryLoad(ctx, next))
	got, err := f.db.ListFoundryLoadDecisions(ctx, next.LoadID, "", nil, 100)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, replacement, *got[0])
	// A conflicting creation cannot leave a partial snapshot or load behind.
	failed := loadJob(f.ontology, "DSNAP3")
	failed.VersionID = version.ID
	assert.ErrorIs(t, f.db.CreateFoundryLoad(ctx, failed), store.ErrConflict)
	got, err = f.db.ListFoundryLoadDecisions(ctx, failed.LoadID, "", nil, 100)
	require.NoError(t, err)
	assert.Empty(t, got)
}
