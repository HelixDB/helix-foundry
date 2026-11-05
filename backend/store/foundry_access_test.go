package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// targetsOf lists p's grants' targets, oldest first.
func targetsOf(t *testing.T, db store.Store, workspaceID string, p access.Principal) []access.Target {
	t.Helper()
	grants, err := db.ListFoundryAccessGrants(context.Background(), workspaceID, []access.Principal{p})
	require.NoError(t, err)
	var out []access.Target
	for _, g := range grants {
		assert.Equal(t, p, g.Principal)
		out = append(out, g.Target)
	}
	return out
}

// A principal's grants are replaced whole, only with the workspace's live
// connectors and sets, and go with their connector, their set or the
// principal.
func TestFoundryAccessGrants(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "ACG")
	ws := f.ontology.WorkspaceID
	general := generalSetID(f.ontology.ID)
	stripe, pg := id26("CONN-ACG-S"), id26("CONN-ACG-P")
	connectorRow(t, f.db, ws, stripe)
	connectorRow(t, f.db, ws, pg)
	agent := access.Principal{Kind: access.PrincipalServiceCredential, ID: id26("SC-ACG")}
	team := access.Principal{Kind: access.PrincipalTeam, ID: id26("TEAM-ACG")}
	by := id26("U-ACG")

	all := []access.Target{access.ConnectorTarget(stripe), access.TableTarget(pg, "people"), access.DocumentSetTarget(general), access.EnrichmentTarget()}
	require.NoError(t, f.db.SetFoundryAccessGrants(ctx, ws, agent, all, &by))
	require.NoError(t, f.db.SetFoundryAccessGrants(ctx, ws, team, []access.Target{access.ConnectorTarget(pg)}, nil))
	assert.ElementsMatch(t, all, targetsOf(t, f.db, ws, agent))
	everyone, err := f.db.ListFoundryAccessGrants(ctx, ws, nil)
	require.NoError(t, err)
	assert.Len(t, everyone, 5)
	for _, g := range everyone {
		if g.Principal == agent {
			assert.Equal(t, &by, g.CreatedByUserID)
		}
	}
	both, err := f.db.ListFoundryAccessGrants(ctx, ws, []access.Principal{agent, team})
	require.NoError(t, err)
	assert.Len(t, both, 5)
	other, err := f.db.ListFoundryAccessGrants(ctx, id26("ACG-OTHER"), nil)
	require.NoError(t, err)
	assert.Empty(t, other, "grants are the workspace's")

	t.Run("replaced whole", func(t *testing.T) {
		require.NoError(t, f.db.SetFoundryAccessGrants(ctx, ws, agent, []access.Target{access.DocumentSetTarget(general)}, &by))
		assert.Equal(t, []access.Target{access.DocumentSetTarget(general)}, targetsOf(t, f.db, ws, agent))
		assert.Equal(t, []access.Target{access.ConnectorTarget(pg)}, targetsOf(t, f.db, ws, team), "another principal's are its own")
	})
	t.Run("only of live connectors and sets", func(t *testing.T) {
		for _, target := range []access.Target{access.ConnectorTarget(id26("CONN-NONE")), access.TableTarget(id26("CONN-NONE"), "t"),
			access.DocumentSetTarget(id26("SET-NONE"))} {
			assert.ErrorIs(t, f.db.SetFoundryAccessGrants(ctx, ws, agent, []access.Target{target}, nil), store.ErrNotFound)
		}
		assert.Equal(t, []access.Target{access.DocumentSetTarget(general)}, targetsOf(t, f.db, ws, agent), "a refused set changes nothing")
	})
	t.Run("deleting a connector deletes its grants", func(t *testing.T) {
		require.NoError(t, f.db.SetFoundryAccessGrants(ctx, ws, agent, []access.Target{access.ConnectorTarget(pg), access.TableTarget(pg, "people"),
			access.ConnectorTarget(stripe)}, nil))
		// What a source's going takes away widens nothing.
		held := id26("U-ACG-HELD")
		require.NoError(t, f.db.SetFoundryAccessGrants(ctx, ws, access.Principal{Kind: access.PrincipalUser, ID: held}, []access.Target{access.ConnectorTarget(pg)}, &by))
		require.NoError(t, f.db.DeleteConnector(ctx, ws, pg))
		restricted, err := f.db.IsFoundryMemberRestricted(ctx, ws, held)
		require.NoError(t, err)
		assert.True(t, restricted, "its connector deleted, they stay restricted")
		require.NoError(t, f.db.SetFoundryMemberRestricted(ctx, ws, held, false, &by), "holding nothing now, it can be lifted")
		assert.Equal(t, []access.Target{access.ConnectorTarget(stripe)}, targetsOf(t, f.db, ws, agent))
		assert.Empty(t, targetsOf(t, f.db, ws, team))
	})
	t.Run("deleting a set deletes its grants", func(t *testing.T) {
		set := f.documentSet("ACG-EMPTY")
		require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, set, 1000))
		require.NoError(t, f.db.SetFoundryAccessGrants(ctx, ws, agent, []access.Target{access.DocumentSetTarget(set.ID), access.ConnectorTarget(stripe)}, nil))
		require.NoError(t, f.db.DeleteFoundryDocumentSet(ctx, ws, set.ID))
		assert.Equal(t, []access.Target{access.ConnectorTarget(stripe)}, targetsOf(t, f.db, ws, agent))
	})
	t.Run("a connector's sets' grants go with it", func(t *testing.T) {
		granola, notes := id26("CONN-ACG-G"), id26("FS-ACG-G")
		connectorRow(t, f.db, ws, granola)
		_, err := testDB.Exec(`INSERT INTO foundry_document_sets (id, workspace_id, connector_id, stream, name, live_key) VALUES (?, ?, ?, 'notes', 'Notes', 1)`,
			notes, ws, granola)
		require.NoError(t, err)
		require.NoError(t, f.db.SetFoundryAccessGrants(ctx, ws, team, []access.Target{access.DocumentSetTarget(notes)}, nil))
		require.NoError(t, f.db.DeleteConnector(ctx, ws, granola))
		assert.Empty(t, targetsOf(t, f.db, ws, team))
	})
	t.Run("the host deletes a principal's grants", func(t *testing.T) {
		require.NoError(t, f.db.DeleteFoundryAccessPrincipal(ctx, ws, agent))
		assert.Empty(t, targetsOf(t, f.db, ws, agent))
		require.NoError(t, f.db.DeleteFoundryAccessPrincipal(ctx, ws, agent), "again changes nothing")
	})
}

// A load's inputs are its version's generation's tables and table files,
// the sets its documents are in now, the documents of the prop decisions it
// copied, and its People enrichment step; a load made before grants could
// cover one holds what nothing covers.
func TestFoundryCurrentLoadInputs(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "ACL")
	ws := f.ontology.WorkspaceID
	general := generalSetID(f.ontology.ID)
	doc := f.uploaded(t, "ACL-doc")
	file := f.uploadedAs(t, "ACL-file", model.FoundryMediaCSV)
	tables := []model.OntologyGenerationTable{{ConnectorID: "stripe", Table: "customers"}, {ConnectorID: "pg", Table: "people"}}

	versionID := f.versionReading(t, "ACL", general)
	input, err := json.Marshal(model.OntologyGenerationInput{Tables: tables, Documents: []model.OntologyGenerationDocument{{ID: file, SetID: general}},
		DocumentSetIDs: []string{general}})
	require.NoError(t, err)
	_, err = testDB.Exec(`INSERT INTO foundry_ontology_generations (id, workspace_id, ontology_id, status, input) VALUES (?, ?, ?, 'succeeded', ?)`,
		id26("GN-ACL"), ws, f.ontology.ID, string(input))
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_ontology_versions SET generation_id=? WHERE id=?`, id26("GN-ACL"), versionID)
	require.NoError(t, err)

	inputs := func(t *testing.T) (model.FoundryLoadInputs, bool) {
		t.Helper()
		all, err := f.db.ListFoundryCurrentLoadInputs(ctx, ws, nil)
		require.NoError(t, err)
		one, err := f.db.ListFoundryCurrentLoadInputs(ctx, ws, []string{f.ontology.ID})
		require.NoError(t, err)
		assert.Equal(t, all, one)
		l, ok := all[f.ontology.ID]
		return l, ok
	}
	_, ok := inputs(t)
	assert.False(t, ok, "no current load, nothing to read")

	require.NoError(t, f.db.CreateFoundryLoad(ctx, loadJob(f.ontology, "ACL")))
	_, ok = inputs(t)
	assert.False(t, ok, "a load still writing is not current")
	f.loaded(t, "w-ACL", doc)
	l, ok := inputs(t)
	require.True(t, ok)
	assert.Equal(t, id26("FJ-ACL"), l.LoadID)
	assert.Equal(t, versionID, l.VersionID)
	assert.Equal(t, access.Inputs{Tables: []access.TableRef{{ConnectorID: "stripe", Table: "customers"}, {ConnectorID: "pg", Table: "people"}},
		Sets: []access.SetRef{{SetID: general}}}, l.Inputs, "its document and its table file are in General")

	byLoad, err := f.db.ListFoundryLoadInputs(ctx, ws, []string{l.LoadID, id26("FJ-NONE")})
	require.NoError(t, err)
	assert.Equal(t, map[string]model.FoundryLoadInputs{l.LoadID: l}, byLoad, "the same load read by its id; a load that isn't there is left out")
	byLoad, err = f.db.ListFoundryLoadInputs(ctx, id26("ACL-ELSEWHERE"), []string{l.LoadID})
	require.NoError(t, err)
	assert.Empty(t, byLoad, "another workspace's load")
	byLoad, err = f.db.ListFoundryLoadInputs(ctx, ws, nil)
	require.NoError(t, err)
	assert.Empty(t, byLoad, "no loads named")
	_, err = testDB.Exec(`UPDATE foundry_loads SET status='deleting' WHERE id=?`, l.LoadID)
	require.NoError(t, err)
	byLoad, err = f.db.ListFoundryLoadInputs(ctx, ws, []string{l.LoadID})
	require.NoError(t, err)
	assert.Empty(t, byLoad, "a load being deleted holds nothing to read")
	_, err = testDB.Exec(`UPDATE foundry_loads SET status='ready' WHERE id=?`, l.LoadID)
	require.NoError(t, err)

	holding, err := f.db.ListFoundryOntologiesHoldingDocument(ctx, ws, doc)
	require.NoError(t, err)
	assert.Equal(t, []string{f.ontology.ID}, holding, "as one of its documents")
	holding, err = f.db.ListFoundryOntologiesHoldingDocument(ctx, ws, file)
	require.NoError(t, err)
	assert.Equal(t, []string{f.ontology.ID}, holding, "as one of its table files")
	holding, err = f.db.ListFoundryOntologiesHoldingDocument(ctx, ws, id26("FD-NONE"))
	require.NoError(t, err)
	assert.Empty(t, holding)

	t.Run("a document counts by the set it is in now", func(t *testing.T) {
		moved := f.documentSet("ACL-MOVED")
		require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, moved, 1000))
		_, _, err := f.db.MoveFoundryDocument(ctx, ws, doc, moved.ID, f.fan("ACL-move"))
		require.NoError(t, err)
		l, _ := inputs(t)
		assert.ElementsMatch(t, []access.SetRef{{SetID: general}, {SetID: moved.ID}}, l.Inputs.Sets)
		_, _, err = f.db.MoveFoundryDocument(ctx, ws, doc, general, f.fan("ACL-back"))
		require.NoError(t, err)
	})
	t.Run("a prop decision counts by its documents, and one without any holds what nothing covers", func(t *testing.T) {
		decision := id26("DEC-ACL")
		_, err := testDB.Exec(`INSERT INTO foundry_load_decisions (load_id, id, ontology_id, kind, type, entity_key, prop, value, decided_by_user_id, decided_at)
VALUES (?, ?, ?, 'prop', 'Customer', 'c1', 'name', '"x"', ?, NOW(6))`, l.LoadID, decision, f.ontology.ID, id26("U-ACL"))
		require.NoError(t, err)
		got, _ := inputs(t)
		assert.True(t, got.Inputs.Uncoverable)
		quoted := f.documentSet("ACL-QUOTED")
		require.NoError(t, f.db.CreateFoundryDocumentSet(ctx, quoted, 1000))
		other := foundryDocument(f.ontology, "ACL-quoted")
		other.SetID = quoted.ID
		_, _, err = f.db.CreateFoundryDocument(ctx, other, noCap)
		require.NoError(t, err)
		_, err = testDB.Exec(`INSERT INTO foundry_review_decision_documents (decision_id, document_id) VALUES (?, ?)`, decision, other.ID)
		require.NoError(t, err)
		got, _ = inputs(t)
		assert.False(t, got.Inputs.Uncoverable)
		assert.Equal(t, []access.SetRef{{SetID: general}}, got.Inputs.Sets)
		assert.Equal(t, []access.SetRef{{SetID: quoted.ID}}, got.Inputs.DecisionSets)
		holding, err := f.db.ListFoundryOntologiesHoldingDocument(ctx, ws, other.ID)
		require.NoError(t, err)
		assert.Equal(t, []string{f.ontology.ID}, holding, "as what one of its decisions came from")
		_, err = testDB.Exec(`DELETE FROM foundry_load_decisions WHERE id=?`, decision)
		require.NoError(t, err)
	})
	t.Run("People enrichment", func(t *testing.T) {
		_, err := testDB.Exec(`INSERT INTO foundry_load_enrichments (load_id, workspace_id, ontology_id, job_id, status) VALUES (?, ?, ?, ?, 'preparing')`,
			l.LoadID, ws, f.ontology.ID, l.LoadID)
		require.NoError(t, err)
		got, _ := inputs(t)
		assert.True(t, got.Inputs.Enrichment)
		_, err = testDB.Exec(`DELETE FROM foundry_load_enrichments WHERE load_id=?`, l.LoadID)
		require.NoError(t, err)
	})
	t.Run("a table file gone is in no set", func(t *testing.T) {
		_, err := testDB.Exec(`UPDATE foundry_documents SET workspace_id=? WHERE id=?`, id26("ACL-ELSEWHERE"), file)
		require.NoError(t, err)
		got, _ := inputs(t)
		assert.Contains(t, got.Inputs.Sets, access.SetRef{})
		_, err = testDB.Exec(`UPDATE foundry_documents SET workspace_id=? WHERE id=?`, ws, file)
		require.NoError(t, err)
	})
	t.Run("a load made before grants holds what nothing covers", func(t *testing.T) {
		_, err := testDB.Exec(`DELETE FROM foundry_load_access WHERE load_id=?`, l.LoadID)
		require.NoError(t, err)
		got, _ := inputs(t)
		assert.True(t, got.Inputs.Unknown)
		_, err = f.db.FoundryLoadHoldings(ctx, ws, l.LoadID)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, err = testDB.Exec(`INSERT INTO foundry_load_access (load_id) VALUES (?)`, l.LoadID)
		require.NoError(t, err)
	})
	t.Run("what a load holds, document by document", func(t *testing.T) {
		decision := id26("DEC-HOLD")
		_, err := testDB.Exec(`INSERT INTO foundry_load_decisions (load_id, id, ontology_id, kind, type, entity_key, prop, value, decided_by_user_id, decided_at)
VALUES (?, ?, ?, 'prop', 'Customer', 'c1', 'name', '"x"', ?, NOW(6))`, l.LoadID, decision, f.ontology.ID, id26("U-ACL"))
		require.NoError(t, err)
		_, err = testDB.Exec(`INSERT INTO foundry_load_enrichments (load_id, workspace_id, ontology_id, job_id, status) VALUES (?, ?, ?, ?, 'preparing')`,
			l.LoadID, ws, f.ontology.ID, l.LoadID)
		require.NoError(t, err)
		h, err := f.db.FoundryLoadHoldings(ctx, ws, l.LoadID)
		require.NoError(t, err)
		set := access.SetRef{SetID: general}
		assert.Equal(t, []access.TableRef{{ConnectorID: "stripe", Table: "customers"}, {ConnectorID: "pg", Table: "people"}}, h.Tables)
		assert.Equal(t, []access.DocumentRef{{ID: doc, Set: set}}, h.Documents)
		assert.Equal(t, []access.DocumentRef{{ID: file, Set: set}}, h.Files)
		assert.True(t, h.Enrichment)
		assert.Equal(t, []access.PropDecision{{Type: "Customer", Prop: "name"}}, h.Decisions, "no documents recorded")
		_, err = testDB.Exec(`INSERT INTO foundry_review_decision_documents (decision_id, document_id) VALUES (?, ?)`, decision, doc)
		require.NoError(t, err)
		h, err = f.db.FoundryLoadHoldings(ctx, ws, l.LoadID)
		require.NoError(t, err)
		assert.Equal(t, []access.PropDecision{{Type: "Customer", Prop: "name", Sets: []access.SetRef{set}, Recorded: true}}, h.Decisions)
		_, err = f.db.FoundryLoadHoldings(ctx, id26("ACL-ELSEWHERE"), l.LoadID)
		assert.ErrorIs(t, err, store.ErrNotFound, "another workspace's")
		_, err = testDB.Exec(`DELETE FROM foundry_load_decisions WHERE id=?`, decision)
		require.NoError(t, err)
		_, err = testDB.Exec(`DELETE FROM foundry_load_enrichments WHERE load_id=?`, l.LoadID)
		require.NoError(t, err)
	})
	t.Run("a version's inputs", func(t *testing.T) {
		in, err := f.db.FoundryVersionInputs(ctx, ws, f.ontology.ID, versionID)
		require.NoError(t, err)
		assert.Equal(t, access.Inputs{Tables: l.Inputs.Tables, Sets: []access.SetRef{{SetID: general}}}, in)
		_, err = f.db.FoundryVersionInputs(ctx, ws, f.ontology.ID, id26("FV-NONE"))
		assert.ErrorIs(t, err, store.ErrNotFound)
		bare := f.version(t, "ACL-bare")
		in, err = f.db.FoundryVersionInputs(ctx, ws, f.ontology.ID, bare)
		require.NoError(t, err)
		assert.True(t, in.Unknown, "a version names its tables only through its generation")
	})
}

// Deleting a load deletes its mark with the rest of its rows.
func TestFoundryLoadAccessMarkDeleted(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "ACD")
	first := f.load(t, "ACD-1")
	f.loaded(t, "w-ACD-1")
	second := f.load(t, "ACD-2")
	f.loaded(t, "w-ACD-2")
	marks := func() int {
		var n int
		require.NoError(t, testDB.QueryRow(`SELECT COUNT(*) FROM foundry_load_access WHERE load_id IN (?, ?)`, first, second).Scan(&n))
		return n
	}
	require.Equal(t, 2, marks(), "every load made since grants is marked")
	require.NoError(t, f.db.DeleteFoundryLoad(ctx, f.ontology.WorkspaceID, f.ontology.ID, first, id26("DEL-ACD")))
	f.finish(t, "w-ACD-del", "ACD-del", model.FoundryGraphJobSucceeded)
	assert.Equal(t, model.FoundryLoadDeleted, f.loadStatus(t, first))
	assert.Equal(t, 1, marks(), "the deleted load's mark is gone, the other's kept")
}

// A grantee's questions are its own: listed only to it.
// A grantee's questions are bounded by its own limits, on top of the
// workspace's, and another's questions don't count against them.
func TestFoundryQuestionGranteeLimits(t *testing.T) {
	ctx := context.Background()
	f := newQuestionFixture(t, "QGL")
	agent := access.Principal{Kind: access.PrincipalServiceCredential, ID: id26("SC-QGL")}
	other := access.Principal{Kind: access.PrincipalUser, ID: id26("U-QGL")}
	ask := func(n int, grantee *access.Principal, limits model.FoundryQuestionLimits) (*model.FoundryQuestion, error) {
		token := fmt.Sprintf("ask-%d", n)
		q := &model.FoundryQuestion{
			ID: f.id(n), WorkspaceID: f.workspaceID, OntologyID: f.ontologyID, LoadID: id26("FL-QGL"), VersionID: id26("FV-QGL"),
			ThreadID: f.id(n), Question: "how many?", ClientToken: &token, MaxUSDMicros: questionCap, Grantee: grantee,
		}
		return q, f.db.CreateFoundryQuestion(ctx, q, limits)
	}
	limits := f.limits()
	limits.GranteePerHour = 2
	_, err := ask(1, &agent, limits)
	require.NoError(t, err)
	_, err = ask(2, &agent, limits)
	require.NoError(t, err)

	_, err = ask(3, &agent, limits)
	var refusal model.FoundryQuestionRefusal
	require.ErrorAs(t, err, &refusal)
	assert.False(t, refusal.Budget)
	assert.Contains(t, refusal.Reason, "you have asked 2 questions")
	_, err = ask(4, &other, limits)
	require.NoError(t, err, "another grantee's questions aren't this one's")
	_, err = ask(5, nil, limits)
	require.NoError(t, err, "the host's caller has only the workspace's limits")

	// What it settled this month, plus this question's cap, within its budget.
	_, err = testDB.Exec(`UPDATE foundry_questions SET status='answered', result='{"status":"answered"}', finished_at=NOW(6), usd_micros=?, charged_at=NOW(6) WHERE id=?`, 400, f.id(1))
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_questions SET status='running', lease_owner='w', lease_until=NOW(6) + INTERVAL 1 HOUR, attempts=1,
reserved_micros=?, charged_at=NOW(6) WHERE id=?`, 50, f.id(2))
	require.NoError(t, err)
	_, err = testDB.Exec(`UPDATE foundry_questions SET usd_micros=?, charged_at=NOW(6) WHERE id=?`, 900, f.id(5))
	require.NoError(t, err)
	spend, err := f.db.SumFoundryGranteeQuestionSpend(ctx, f.workspaceID, agent, limits.Since)
	require.NoError(t, err)
	assert.Equal(t, model.FoundryQuestionSpend{SpentMicros: 400, ReservedMicros: 50}, spend, "its own, not the workspace's")

	limits.GranteePerHour = 30
	// Its running question counts at its cap, as what it may yet spend.
	limits.GranteeCapMicros = 400 + 2*questionCap - 1
	_, err = ask(6, &agent, limits)
	require.ErrorAs(t, err, &refusal)
	assert.True(t, refusal.Budget)
	assert.Contains(t, refusal.Reason, "you have spent")
	limits.GranteeCapMicros = 400 + 2*questionCap
	_, err = ask(7, &agent, limits)
	require.NoError(t, err)

	// What the workspace has asked and spent isn't told to a grantee.
	full := f.limits()
	full.PerHour = 1
	_, err = ask(10, &other, full)
	require.ErrorAs(t, err, &refusal)
	assert.NotContains(t, refusal.Reason, "the workspace has asked")
	_, err = ask(11, nil, full)
	require.ErrorAs(t, err, &refusal)
	assert.Contains(t, refusal.Reason, "the workspace has asked")

	seen := access.Inputs{Sets: []access.SetRef{{SetID: "set-1"}}}
	require.NoError(t, f.db.SetFoundryQuestionSeen(ctx, f.id(1), &seen))
	stored, err := f.db.GetFoundryQuestion(ctx, f.ontologyID, f.id(1))
	require.NoError(t, err)
	assert.Equal(t, &seen, stored.Seen)
	require.NoError(t, f.db.SetFoundryQuestionSeen(ctx, f.id(1), nil))
	stored, err = f.db.GetFoundryQuestion(ctx, f.ontologyID, f.id(1))
	require.NoError(t, err)
	assert.Nil(t, stored.Seen, "the whole load, after all")
	notSeen, err := f.db.GetFoundryQuestion(ctx, f.ontologyID, f.id(2))
	require.NoError(t, err)
	assert.Nil(t, notSeen.Seen)

	limits.GranteePerHour = 0
	_, err = ask(8, &agent, limits)
	require.Error(t, err, "a grantee's question needs its limits")
	_, err = ask(9, nil, limits)
	require.NoError(t, err, "the host's caller's doesn't")
}

func TestFoundryQuestionGrantees(t *testing.T) {
	ctx := context.Background()
	f := newQuestionFixture(t, "QGR")
	agent := access.Principal{Kind: access.PrincipalServiceCredential, ID: id26("SC-QGR")}
	other := access.Principal{Kind: access.PrincipalUser, ID: id26("U-QGR")}
	ask := func(n int, grantee *access.Principal) *model.FoundryQuestion {
		token := fmt.Sprintf("ask-%d", n)
		q := &model.FoundryQuestion{
			ID: f.id(n), WorkspaceID: f.workspaceID, OntologyID: f.ontologyID, LoadID: id26("FL-QGR"), VersionID: id26("FV-QGR"),
			ThreadID: f.id(n), Question: "how many?", ClientToken: &token, MaxUSDMicros: questionCap, Grantee: grantee,
		}
		require.NoError(t, f.db.CreateFoundryQuestion(ctx, q, f.limits()))
		return q
	}
	asked, host := ask(1, &agent), ask(2, nil)
	// A thread the grantee and the host's caller both asked in: the grantee's
	// next question is told only its own earlier turns.
	for _, q := range []*model.FoundryQuestion{
		{ID: f.id(3), ThreadID: asked.ID}, {ID: f.id(4), ThreadID: asked.ID, Grantee: &agent}, {ID: f.id(5), ThreadID: asked.ID, Grantee: &agent},
	} {
		token := "ask-" + q.ID
		q.WorkspaceID, q.OntologyID, q.LoadID, q.VersionID = f.workspaceID, f.ontologyID, id26("FL-QGR"), id26("FV-QGR")
		q.Question, q.ClientToken, q.MaxUSDMicros = "and then?", &token, questionCap
		require.NoError(t, f.db.CreateFoundryQuestion(ctx, q, f.limits()))
	}
	_, err := testDB.Exec(`UPDATE foundry_questions SET status='answered', result='{"status":"answered"}', finished_at=NOW(6) WHERE id IN (?, ?, ?)`,
		asked.ID, f.id(3), f.id(4))
	require.NoError(t, err)
	last, err := f.db.GetFoundryQuestion(ctx, f.ontologyID, f.id(5))
	require.NoError(t, err)
	history, err := f.db.ListFoundryQuestionHistory(ctx, last, 10)
	require.NoError(t, err)
	var told []string
	for _, h := range history {
		told = append(told, h.ID)
	}
	assert.Equal(t, []string{asked.ID, f.id(4)}, told)
	last.Grantee = nil
	history, err = f.db.ListFoundryQuestionHistory(ctx, last, 10)
	require.NoError(t, err)
	assert.Len(t, history, 3, "the host's caller is told every turn")

	got, err := f.db.GetFoundryQuestion(ctx, f.ontologyID, asked.ID)
	require.NoError(t, err)
	assert.Equal(t, &agent, got.Grantee)
	got, err = f.db.GetFoundryQuestion(ctx, f.ontologyID, host.ID)
	require.NoError(t, err)
	assert.Nil(t, got.Grantee)

	ids := func(qs []*model.FoundryQuestion) []string {
		var out []string
		for _, q := range qs {
			out = append(out, q.ID)
		}
		return out
	}
	everyone, _, err := f.db.ListFoundryQuestions(ctx, f.ontologyID, "", "", 10, nil)
	require.NoError(t, err)
	assert.Len(t, everyone, 5)
	assert.Contains(t, ids(everyone), host.ID)
	mine, _, err := f.db.ListFoundryQuestions(ctx, f.ontologyID, asked.ID, "", 10, &agent)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{asked.ID, f.id(4), f.id(5)}, ids(mine))
	theirs, _, err := f.db.ListFoundryQuestions(ctx, f.ontologyID, "", "", 10, &other)
	require.NoError(t, err)
	assert.Empty(t, theirs)
}

// Settling a prop decision records the documents its review quoted, adding
// to those of earlier settles, which loads made before still apply.
func TestSettleRecordsDecisionDocuments(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "SDD")
	f.currentLoad(t, "SDD-load", customerTOML)
	documents := func(decisionID string) []string {
		rows, err := testDB.Query(`SELECT document_id FROM foundry_review_decision_documents WHERE decision_id=? ORDER BY document_id`, decisionID)
		require.NoError(t, err)
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			require.NoError(t, rows.Scan(&id))
			ids = append(ids, id)
		}
		require.NoError(t, rows.Err())
		return ids
	}
	prop := &model.FoundryReviewDecision{ID: id26("SDD-P1"), OntologyID: f.ontology.ID, Kind: model.FoundryReviewProp, Type: "Customer",
		EntityKey: ptr("c1"), Prop: ptr("name"), Value: json.RawMessage(`"A"`), DecidedByUserID: id26("user"), DocumentIDs: []string{"doc-a"}}
	require.NoError(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, prop))
	assert.Equal(t, []string{"doc-a"}, documents(prop.ID))
	again := &model.FoundryReviewDecision{ID: id26("SDD-P2"), OntologyID: f.ontology.ID, Kind: model.FoundryReviewProp, Type: "Customer",
		EntityKey: ptr("c1"), Prop: ptr("name"), Value: json.RawMessage(`"B"`), DecidedByUserID: id26("user"), DocumentIDs: []string{"doc-b", "doc-a"}}
	require.NoError(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, again))
	assert.Equal(t, prop.ID, again.ID, "settled again, the decision keeps its id")
	assert.Equal(t, []string{"doc-a", "doc-b"}, documents(prop.ID))
	resolve := &model.FoundryReviewDecision{ID: id26("SDD-R1"), OntologyID: f.ontology.ID, Kind: model.FoundryReviewResolve, Type: "Customer",
		DocID: ptr("doc-1"), NormName: ptr("a name"), DecidedByUserID: id26("user")}
	require.NoError(t, f.db.SettleFoundryReviewDecision(ctx, f.ontology.WorkspaceID, resolve))
	assert.Empty(t, documents(resolve.ID), "a resolve decision applies no value")
}

// Members are restricted and let go; teams hold members and go with their
// grants; a member who leaves takes their grants and teams, not their
// restriction.
func TestFoundryTeamsAndRestrictedMembers(t *testing.T) {
	ctx := context.Background()
	f := newFoundryFixture(t, "ATM")
	ws := f.ontology.WorkspaceID
	ada, grace := id26("U-ATM-ADA"), id26("U-ATM-GRACE")
	by := id26("U-ATM-OWNER")

	restricted := func() bool {
		r, err := f.db.IsFoundryMemberRestricted(ctx, ws, ada)
		require.NoError(t, err)
		return r
	}
	assert.False(t, restricted())
	require.NoError(t, f.db.SetFoundryMemberRestricted(ctx, ws, ada, true, &by))
	require.NoError(t, f.db.SetFoundryMemberRestricted(ctx, ws, ada, true, &by), "again changes nothing")
	assert.True(t, restricted())
	members, err := f.db.ListFoundryRestrictedMembers(ctx, ws)
	require.NoError(t, err)
	assert.Equal(t, []string{ada}, members)
	other, err := f.db.IsFoundryMemberRestricted(ctx, id26("ATM-OTHER"), ada)
	require.NoError(t, err)
	assert.False(t, other, "restricted in its workspace only")

	team := &model.FoundryTeam{ID: id26("TEAM-ATM"), WorkspaceID: ws, Name: "Finance", MemberUserIDs: []string{ada, grace}, CreatedByUserID: &by}
	require.NoError(t, f.db.CreateFoundryTeam(ctx, team))
	assert.ErrorIs(t, f.db.CreateFoundryTeam(ctx, &model.FoundryTeam{ID: id26("TEAM-ATM-2"), WorkspaceID: ws, Name: "Finance"}), store.ErrConflict)
	require.NoError(t, f.db.CreateFoundryTeam(ctx, &model.FoundryTeam{ID: id26("TEAM-ATM-3"), WorkspaceID: ws, Name: "Ops"}))
	teams, err := f.db.ListFoundryTeams(ctx, ws)
	require.NoError(t, err)
	require.Len(t, teams, 2)
	assert.Equal(t, "Finance", teams[0].Name)
	assert.ElementsMatch(t, []string{ada, grace}, teams[0].MemberUserIDs)
	assert.Equal(t, &by, teams[0].CreatedByUserID)
	assert.Empty(t, teams[1].MemberUserIDs)
	of, err := f.db.FoundryTeamsOf(ctx, ws, ada)
	require.NoError(t, err)
	assert.Equal(t, []string{team.ID}, of)

	require.NoError(t, f.db.UpdateFoundryTeam(ctx, ws, team.ID, "Billing", []string{grace}))
	assert.ErrorIs(t, f.db.UpdateFoundryTeam(ctx, ws, team.ID, "Ops", nil), store.ErrConflict)
	assert.ErrorIs(t, f.db.UpdateFoundryTeam(ctx, ws, id26("TEAM-NONE"), "X", nil), store.ErrNotFound)
	assert.ErrorIs(t, f.db.UpdateFoundryTeam(ctx, id26("ATM-OTHER"), team.ID, "X", nil), store.ErrNotFound, "another workspace's")
	of, err = f.db.FoundryTeamsOf(ctx, ws, ada)
	require.NoError(t, err)
	assert.Empty(t, of)

	teamPrincipal := access.Principal{Kind: access.PrincipalTeam, ID: team.ID}
	adaPrincipal := access.Principal{Kind: access.PrincipalUser, ID: ada}
	isRestricted := func(userID string) bool {
		r, err := f.db.IsFoundryMemberRestricted(ctx, ws, userID)
		require.NoError(t, err)
		return r
	}
	t.Run("grants restrict whoever they reach, and taking them away widens nothing", func(t *testing.T) {
		alone, joined, added := id26("U-ATM-ALONE"), id26("U-ATM-JOINED"), id26("U-ATM-ADDED")
		enrichment := []access.Target{access.EnrichmentTarget()}
		// Holding nothing restricts no one.
		require.NoError(t, f.db.SetFoundryAccessGrants(ctx, ws, access.Principal{Kind: access.PrincipalUser, ID: alone}, nil, &by))
		assert.False(t, isRestricted(alone))
		require.NoError(t, f.db.SetFoundryAccessGrants(ctx, ws, access.Principal{Kind: access.PrincipalUser, ID: alone}, enrichment, &by))
		assert.True(t, isRestricted(alone))
		assert.ErrorIs(t, f.db.SetFoundryMemberRestricted(ctx, ws, alone, false, &by), store.ErrConflict, "holding sources, it can't be lifted")
		assert.True(t, isRestricted(alone))
		require.NoError(t, f.db.SetFoundryAccessGrants(ctx, ws, access.Principal{Kind: access.PrincipalUser, ID: alone}, nil, &by))
		assert.True(t, isRestricted(alone), "its last grant taken away, it reads nothing, not everything")

		// A team of no grants restricts no one who joins it.
		crew := &model.FoundryTeam{ID: id26("TEAM-ATM-CREW"), WorkspaceID: ws, Name: "Crew", MemberUserIDs: []string{joined}}
		require.NoError(t, f.db.CreateFoundryTeam(ctx, crew))
		assert.False(t, isRestricted(joined))
		crewPrincipal := access.Principal{Kind: access.PrincipalTeam, ID: crew.ID}
		require.NoError(t, f.db.SetFoundryAccessGrants(ctx, ws, crewPrincipal, enrichment, &by))
		assert.True(t, isRestricted(joined), "a team given sources restricts its members")
		assert.ErrorIs(t, f.db.SetFoundryMemberRestricted(ctx, ws, joined, false, &by), store.ErrConflict, "nor while on a team holding some")
		require.NoError(t, f.db.UpdateFoundryTeam(ctx, ws, crew.ID, "Crew", []string{added}))
		assert.True(t, isRestricted(added), "whoever joins a team that holds some")
		assert.True(t, isRestricted(joined), "whoever leaves stays")
		require.NoError(t, f.db.SetFoundryAccessGrants(ctx, ws, crewPrincipal, nil, &by))
		other := id26("U-ATM-LATER")
		require.NoError(t, f.db.UpdateFoundryTeam(ctx, ws, crew.ID, "Crew", []string{added, other}))
		assert.False(t, isRestricted(other), "a team that holds none any more restricts no one new")
		require.NoError(t, f.db.DeleteFoundryTeam(ctx, ws, crew.ID))
		assert.True(t, isRestricted(added), "nor does deleting the team lift it")
		// A team with no members restricts no one, and its grants still save.
		empty := &model.FoundryTeam{ID: id26("TEAM-ATM-EMPTY"), WorkspaceID: ws, Name: "Empty"}
		require.NoError(t, f.db.CreateFoundryTeam(ctx, empty))
		require.NoError(t, f.db.SetFoundryAccessGrants(ctx, ws, access.Principal{Kind: access.PrincipalTeam, ID: empty.ID}, enrichment, &by))
		require.NoError(t, f.db.DeleteFoundryTeam(ctx, ws, empty.ID))
		for _, userID := range []string{alone, joined, added} {
			require.NoError(t, f.db.SetFoundryMemberRestricted(ctx, ws, userID, false, &by))
		}
	})
	require.NoError(t, f.db.SetFoundryAccessGrants(ctx, ws, teamPrincipal, []access.Target{access.EnrichmentTarget()}, nil))
	require.NoError(t, f.db.SetFoundryAccessGrants(ctx, ws, adaPrincipal, []access.Target{access.EnrichmentTarget()}, nil))
	require.NoError(t, f.db.UpdateFoundryTeam(ctx, ws, team.ID, "Billing", []string{ada, grace}))

	t.Run("a member who leaves", func(t *testing.T) {
		require.NoError(t, f.db.RemoveFoundryAccessMember(ctx, ws, ada))
		assert.Empty(t, targetsOf(t, f.db, ws, adaPrincipal))
		of, err := f.db.FoundryTeamsOf(ctx, ws, ada)
		require.NoError(t, err)
		assert.Empty(t, of)
		assert.True(t, restricted(), "let back in, still restricted")
		of, err = f.db.FoundryTeamsOf(ctx, ws, grace)
		require.NoError(t, err)
		assert.Equal(t, []string{team.ID}, of, "the others stay")
		require.NoError(t, f.db.RemoveFoundryAccessMember(ctx, ws, ada), "again changes nothing")
	})
	t.Run("a team deleted", func(t *testing.T) {
		require.NoError(t, f.db.DeleteFoundryTeam(ctx, ws, team.ID))
		assert.ErrorIs(t, f.db.DeleteFoundryTeam(ctx, ws, team.ID), store.ErrNotFound)
		assert.Empty(t, targetsOf(t, f.db, ws, teamPrincipal))
		of, err := f.db.FoundryTeamsOf(ctx, ws, grace)
		require.NoError(t, err)
		assert.Empty(t, of)
	})
	require.NoError(t, f.db.SetFoundryMemberRestricted(ctx, ws, ada, false, nil))
	assert.False(t, restricted())
}
