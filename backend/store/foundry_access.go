package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// ListFoundryAccessGrants returns the workspace's grants, of principals
// when it names any (all of them otherwise), oldest first.
func (s *mysqlDB) ListFoundryAccessGrants(ctx context.Context, workspaceID string, principals []access.Principal) ([]model.FoundryAccessGrant, error) {
	query := `SELECT id, principal_kind, principal_id, target_kind, connector_id, table_name, document_set_id, created_by_user_id, created_at
FROM foundry_access_grants WHERE workspace_id=?`
	args := []any{workspaceID}
	if len(principals) > 0 {
		query += ` AND (principal_kind, principal_id) IN (` + strings.TrimSuffix(strings.Repeat("(?, ?), ", len(principals)), ", ") + `)`
		for _, p := range principals {
			args = append(args, p.Kind.String(), p.ID)
		}
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FoundryAccessGrant
	for rows.Next() {
		var g model.FoundryAccessGrant
		var principalKind, targetKind, connectorID, table, setID string
		var by sql.NullString
		if err := rows.Scan(&g.ID, &principalKind, &g.Principal.ID, &targetKind, &connectorID, &table, &setID, &by, &g.CreatedAt); err != nil {
			return nil, err
		}
		if g.Principal.Kind, err = access.ParsePrincipalKind(principalKind); err != nil {
			return nil, err
		}
		if g.Target, err = access.ParseTarget(targetKind, connectorID, table, setID); err != nil {
			return nil, err
		}
		if by.Valid {
			g.CreatedByUserID = &by.String
		}
		g.WorkspaceID = workspaceID
		out = append(out, g)
	}
	return out, rows.Err()
}

// SetFoundryAccessGrants replaces principal's grants in the workspace with
// targets, which must be of its live connectors and sets: ErrNotFound means
// one is not. Grants kept keep their id and creation. Giving a member or a
// team any restricts that member or the team's members in the same
// transaction, and taking them away leaves them restricted: taking access
// away never gives anyone more.
func (s *mysqlDB) SetFoundryAccessGrants(ctx context.Context, workspaceID string, principal access.Principal, targets []access.Target,
	byUserID *string) error {
	return s.WithTx(ctx, func(txDB Store) error {
		tx := txDB.(*mysqlDB)
		if err := tx.workspaces.Lock(ctx, tx.db, workspaceID); err != nil {
			return err
		}
		var connectors, sets []string
		for _, t := range targets {
			switch t.Kind() {
			case access.TargetConnector, access.TargetTable:
				connectors = append(connectors, t.ConnectorID())
			case access.TargetDocumentSet:
				sets = append(sets, t.DocumentSetID())
			}
		}
		for _, check := range []struct {
			table string
			ids   []string
		}{{"connectors", connectors}, {"foundry_document_sets", sets}} {
			ids := slices.Compact(slices.Sorted(slices.Values(check.ids)))
			if len(ids) == 0 {
				continue
			}
			var n int
			if err := tx.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+check.table+` WHERE workspace_id=? AND deleted_at IS NULL AND id IN (`+
				idPlaceholders(len(ids))+`)`, append([]any{workspaceID}, toAnySlice(ids)...)...).Scan(&n); err != nil {
				return err
			}
			if n != len(ids) {
				return ErrNotFound
			}
		}
		held, err := tx.ListFoundryAccessGrants(ctx, workspaceID, []access.Principal{principal})
		if err != nil {
			return err
		}
		var dropped []string
		for _, g := range held {
			if !slices.Contains(targets, g.Target) {
				dropped = append(dropped, g.ID)
			}
		}
		if len(dropped) > 0 {
			if _, err := tx.db.ExecContext(ctx, `DELETE FROM foundry_access_grants WHERE id IN (`+idPlaceholders(len(dropped))+`)`, toAnySlice(dropped)...); err != nil {
				return err
			}
		}
		if len(targets) == 0 {
			return nil
		}
		rows := make([]string, 0, len(targets))
		args := make([]any, 0, 9*len(targets))
		for _, t := range targets {
			rows = append(rows, "(?, ?, ?, ?, ?, ?, ?, ?, ?)")
			args = append(args, id.New(), workspaceID, principal.Kind.String(), principal.ID, t.Kind().String(), t.ConnectorID(), t.Table(),
				t.DocumentSetID(), byUserID)
		}
		_, err = tx.db.ExecContext(ctx, `INSERT IGNORE INTO foundry_access_grants
 (id, workspace_id, principal_kind, principal_id, target_kind, connector_id, table_name, document_set_id, created_by_user_id)
VALUES `+strings.Join(rows, ", "), args...)
		if err != nil {
			return err
		}
		switch principal.Kind {
		case access.PrincipalUser:
			return tx.SetFoundryMemberRestricted(ctx, workspaceID, principal.ID, true, byUserID)
		case access.PrincipalTeam:
			return tx.restrictFoundryTeamMembers(ctx, workspaceID, principal.ID, byUserID)
		}
		return nil
	})
}

// DeleteFoundryAccessPrincipal deletes every grant principal holds in the
// workspace: the host calls it when it revokes a service credential, in its
// own transaction or not.
func (s *mysqlDB) DeleteFoundryAccessPrincipal(ctx context.Context, workspaceID string, principal access.Principal) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM foundry_access_grants WHERE workspace_id=? AND principal_kind=? AND principal_id=?`,
		workspaceID, principal.Kind.String(), principal.ID)
	return err
}

// ListFoundryCurrentLoadInputs returns what the servable current load of
// each live ontology of the workspace holds (of ontologyIDs, when it names
// any), by ontology.
func (s *mysqlDB) ListFoundryCurrentLoadInputs(ctx context.Context, workspaceID string, ontologyIDs []string) (map[string]model.FoundryLoadInputs, error) {
	query := `SELECT o.id, l.id, l.version_id, COALESCE(g.input, CAST('null' AS JSON)), a.load_id IS NOT NULL FROM foundry_ontologies o
JOIN foundry_loads l ON l.id = o.current_load_id
LEFT JOIN foundry_load_access a ON a.load_id = l.id
JOIN foundry_ontology_versions v ON v.id = l.version_id
LEFT JOIN foundry_ontology_generations g ON g.id = v.generation_id
WHERE o.workspace_id=? AND o.deleted_at IS NULL AND l.status IN ('ready', 'partial')`
	args := []any{workspaceID}
	if len(ontologyIDs) > 0 {
		query += ` AND o.id IN (` + idPlaceholders(len(ontologyIDs)) + `)`
		args = append(args, toAnySlice(ontologyIDs)...)
	}
	loads, err := s.foundryLoadInputs(ctx, workspaceID, query, args)
	if err != nil {
		return nil, err
	}
	out := make(map[string]model.FoundryLoadInputs, len(loads))
	for _, l := range loads {
		out[l.OntologyID] = l
	}
	return out, nil
}

// ListFoundryLoadInputs returns what each servable load of loadIDs holds, of
// any live ontology of the workspace, by load: a load being deleted, or gone,
// holds nothing to read and isn't returned.
func (s *mysqlDB) ListFoundryLoadInputs(ctx context.Context, workspaceID string, loadIDs []string) (map[string]model.FoundryLoadInputs, error) {
	out := make(map[string]model.FoundryLoadInputs, len(loadIDs))
	if len(loadIDs) == 0 {
		return out, nil
	}
	loads, err := s.foundryLoadInputs(ctx, workspaceID, `SELECT o.id, l.id, l.version_id, COALESCE(g.input, CAST('null' AS JSON)), a.load_id IS NOT NULL FROM foundry_loads l
JOIN foundry_ontologies o ON o.id = l.ontology_id
LEFT JOIN foundry_load_access a ON a.load_id = l.id
JOIN foundry_ontology_versions v ON v.id = l.version_id
LEFT JOIN foundry_ontology_generations g ON g.id = v.generation_id
WHERE o.workspace_id=? AND o.deleted_at IS NULL AND l.status IN ('ready', 'partial') AND l.id IN (`+idPlaceholders(len(loadIDs))+`)`,
		append([]any{workspaceID}, toAnySlice(loadIDs)...))
	if err != nil {
		return nil, err
	}
	for _, l := range loads {
		out[l.LoadID] = l
	}
	return out, nil
}

// foundryLoadInputs reads what the loads query selects hold (its columns the
// ontology, the load, its version, its generation's input and whether it has
// its marker). A load made before grants could cover one holds what nothing
// covers (Uncoverable).
//
// A load's tables and table files are its version's generation's inputs: a
// load reads no others, though it may not hold all of them. Its documents,
// table files and the documents its review decisions came from count by the
// set each is in now, so moving one out of a granted set takes the load away
// from whoever held only that set. A prop decision whose documents were never
// recorded holds what nothing covers.
func (s *mysqlDB) foundryLoadInputs(ctx context.Context, workspaceID, query string, args []any) ([]model.FoundryLoadInputs, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []model.FoundryLoadInputs
	byLoad := map[string]*access.Inputs{}
	files := map[string][]string{} // table file document id to the loads reading it
	for rows.Next() {
		var ontologyID, loadID, versionID string
		var raw []byte
		var marked bool
		if err := rows.Scan(&ontologyID, &loadID, &versionID, &raw, &marked); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var input *model.OntologyGenerationInput
		if err := json.Unmarshal(raw, &input); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("read load %s's generation input: %w", loadID, err)
		}
		// A load names its tables only through its generation.
		in := &access.Inputs{Unknown: !marked || input == nil}
		if input != nil {
			for _, t := range input.Tables {
				in.Tables = append(in.Tables, access.TableRef{ConnectorID: t.ConnectorID, Table: t.Table})
			}
			for _, d := range input.Documents {
				files[d.ID] = append(files[d.ID], loadID)
			}
		}
		out = append(out, model.FoundryLoadInputs{OntologyID: ontologyID, LoadID: loadID, VersionID: versionID})
		byLoad[loadID] = in
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(byLoad) == 0 {
		return out, nil
	}
	loads := slices.Sorted(maps.Keys(byLoad))
	in := idPlaceholders(len(loads))
	sets := func(query string, args []any, add func(loadOrDoc string, set access.SetRef, recorded bool)) error {
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			var set access.SetRef
			recorded := true
			if err := rows.Scan(&key, &set.SetID, &set.ConnectorID, &recorded); err != nil {
				return err
			}
			add(key, set, recorded)
		}
		return rows.Err()
	}
	addSet := func(loadID string, set access.SetRef, _ bool) {
		if l := byLoad[loadID]; !slices.Contains(l.Sets, set) {
			l.Sets = append(l.Sets, set)
		}
	}
	// The documents the load holds, by the set each is in now.
	if err := sets(`SELECT ld.load_id, COALESCE(d.set_id, ''), COALESCE(s.connector_id, ''), TRUE FROM foundry_load_documents ld
LEFT JOIN foundry_documents d ON d.id = ld.document_id
LEFT JOIN foundry_document_sets s ON s.id = d.set_id
WHERE ld.load_id IN (`+in+`) GROUP BY 1, 2, 3`, toAnySlice(loads), addSet); err != nil {
		return nil, err
	}
	// The documents its prop decisions came from; a decision with none
	// recorded holds what nothing covers.
	if err := sets(`SELECT ld.load_id, COALESCE(d.set_id, ''), COALESCE(s.connector_id, ''), dd.decision_id IS NOT NULL
FROM foundry_load_decisions ld
LEFT JOIN foundry_review_decision_documents dd ON dd.decision_id = ld.id
LEFT JOIN foundry_documents d ON d.id = dd.document_id
LEFT JOIN foundry_document_sets s ON s.id = d.set_id
WHERE ld.load_id IN (`+in+`) AND ld.kind='prop' GROUP BY 1, 2, 3, 4`, toAnySlice(loads), func(loadID string, set access.SetRef, recorded bool) {
		l := byLoad[loadID]
		switch {
		case !recorded:
			l.Uncoverable = true
		case !slices.Contains(l.DecisionSets, set):
			l.DecisionSets = append(l.DecisionSets, set)
		}
	}); err != nil {
		return nil, err
	}
	// Its table files, by the set each is in now; one gone is in no set.
	if len(files) > 0 {
		docs := slices.Sorted(maps.Keys(files))
		found := map[string]bool{}
		if err := sets(`SELECT d.id, COALESCE(d.set_id, ''), COALESCE(s.connector_id, ''), TRUE FROM foundry_documents d
LEFT JOIN foundry_document_sets s ON s.id = d.set_id
WHERE d.workspace_id=? AND d.id IN (`+idPlaceholders(len(docs))+`)`, append([]any{workspaceID}, toAnySlice(docs)...),
			func(docID string, set access.SetRef, _ bool) {
				found[docID] = true
				for _, loadID := range files[docID] {
					addSet(loadID, set, true)
				}
			}); err != nil {
			return nil, err
		}
		for _, docID := range docs {
			if !found[docID] {
				for _, loadID := range files[docID] {
					addSet(loadID, access.SetRef{}, true)
				}
			}
		}
	}
	// Whether it applied People enrichment's profiles: every load of a version
	// that enriches has a step.
	enriched, err := s.db.QueryContext(ctx, `SELECT load_id FROM foundry_load_enrichments WHERE load_id IN (`+in+`)`, toAnySlice(loads)...)
	if err != nil {
		return nil, err
	}
	ids, err := scanStrings(enriched)
	if err != nil {
		return nil, err
	}
	for _, loadID := range ids {
		byLoad[loadID].Enrichment = true
	}
	for i := range out {
		out[i].Inputs = *byLoad[out[i].LoadID]
	}
	return out, nil
}

// FoundryVersionInputs is what a new load of the ontology's version would
// hold, as far as grants go: its generation's tables and table files, the
// sets of its scope, and the documents of the ontology's prop decisions,
// which a load copies. Whether it applies People enrichment depends on its
// mappings, which the caller reads.
func (s *mysqlDB) FoundryVersionInputs(ctx context.Context, workspaceID, ontologyID, versionID string) (access.Inputs, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(g.input, CAST('null' AS JSON)) FROM foundry_ontology_versions v
LEFT JOIN foundry_ontology_generations g ON g.id = v.generation_id
WHERE v.id=? AND v.ontology_id=? AND v.workspace_id=?`, versionID, ontologyID, workspaceID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return access.Inputs{}, ErrNotFound
	}
	if err != nil {
		return access.Inputs{}, err
	}
	var input *model.OntologyGenerationInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return access.Inputs{}, err
	}
	var in access.Inputs
	if input == nil {
		in.Unknown = true
		return in, nil
	}
	for _, t := range input.Tables {
		in.Tables = append(in.Tables, access.TableRef{ConnectorID: t.ConnectorID, Table: t.Table})
	}
	scope, err := s.GetOntologyVersionScope(ctx, ontologyID, versionID)
	if err != nil {
		return access.Inputs{}, err
	}
	files := make([]string, 0, len(input.Documents))
	for _, d := range input.Documents {
		files = append(files, d.ID)
	}
	add := func(query string, ids []string) error {
		if len(ids) == 0 {
			return nil
		}
		rows, err := s.db.QueryContext(ctx, query+` IN (`+idPlaceholders(len(ids))+`)`, append([]any{workspaceID}, toAnySlice(ids)...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var set access.SetRef
			if err := rows.Scan(&set.SetID, &set.ConnectorID); err != nil {
				return err
			}
			if !slices.Contains(in.Sets, set) {
				in.Sets = append(in.Sets, set)
			}
		}
		return rows.Err()
	}
	if err := add(`SELECT id, COALESCE(connector_id, '') FROM foundry_document_sets WHERE workspace_id=? AND id`, scope); err != nil {
		return access.Inputs{}, err
	}
	if err := add(`SELECT DISTINCT COALESCE(d.set_id, ''), COALESCE(st.connector_id, '') FROM foundry_documents d
LEFT JOIN foundry_document_sets st ON st.id = d.set_id WHERE d.workspace_id=? AND d.id`, files); err != nil {
		return access.Inputs{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(d.set_id, ''), COALESCE(st.connector_id, ''), dd.decision_id IS NOT NULL
FROM foundry_review_decisions r
LEFT JOIN foundry_review_decision_documents dd ON dd.decision_id = r.id
LEFT JOIN foundry_documents d ON d.id = dd.document_id
LEFT JOIN foundry_document_sets st ON st.id = d.set_id
WHERE r.ontology_id=? AND r.kind='prop' GROUP BY 1, 2, 3`, ontologyID)
	if err != nil {
		return access.Inputs{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var set access.SetRef
		var recorded bool
		if err := rows.Scan(&set.SetID, &set.ConnectorID, &recorded); err != nil {
			return access.Inputs{}, err
		}
		switch {
		case !recorded:
			in.Uncoverable = true
		case !slices.Contains(in.DecisionSets, set):
			in.DecisionSets = append(in.DecisionSets, set)
		}
	}
	return in, rows.Err()
}

// FoundryLoadHoldings is what a load of the workspace holds, document by
// document, each by the set it is in now, for reading the part of it a
// grantee holds. A load it can't tell is ErrNotFound.
func (s *mysqlDB) FoundryLoadHoldings(ctx context.Context, workspaceID, loadID string) (access.Holdings, error) {
	var raw []byte
	var enriched bool
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(g.input, CAST('null' AS JSON)),
  EXISTS (SELECT 1 FROM foundry_load_enrichments e WHERE e.load_id = l.id)
FROM foundry_loads l
JOIN foundry_ontologies o ON o.id = l.ontology_id
JOIN foundry_load_access a ON a.load_id = l.id
JOIN foundry_ontology_versions v ON v.id = l.version_id
LEFT JOIN foundry_ontology_generations g ON g.id = v.generation_id
WHERE l.id=? AND o.workspace_id=? AND o.deleted_at IS NULL AND l.status IN ('ready', 'partial')`, loadID, workspaceID).Scan(&raw, &enriched)
	if errors.Is(err, sql.ErrNoRows) {
		return access.Holdings{}, ErrNotFound
	}
	if err != nil {
		return access.Holdings{}, err
	}
	var input *model.OntologyGenerationInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return access.Holdings{}, fmt.Errorf("read load %s's generation input: %w", loadID, err)
	}
	if input == nil {
		return access.Holdings{}, ErrNotFound
	}
	out := access.Holdings{Enrichment: enriched}
	for _, t := range input.Tables {
		out.Tables = append(out.Tables, access.TableRef{ConnectorID: t.ConnectorID, Table: t.Table})
	}
	documents := func(query string, args []any) ([]access.DocumentRef, error) {
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var refs []access.DocumentRef
		for rows.Next() {
			var d access.DocumentRef
			if err := rows.Scan(&d.ID, &d.Set.SetID, &d.Set.ConnectorID); err != nil {
				return nil, err
			}
			refs = append(refs, d)
		}
		return refs, rows.Err()
	}
	if out.Documents, err = documents(`SELECT ld.document_id, COALESCE(d.set_id, ''), COALESCE(st.connector_id, '') FROM foundry_load_documents ld
LEFT JOIN foundry_documents d ON d.id = ld.document_id
LEFT JOIN foundry_document_sets st ON st.id = d.set_id
WHERE ld.load_id=? ORDER BY ld.document_id`, []any{loadID}); err != nil {
		return access.Holdings{}, err
	}
	if len(input.Documents) > 0 {
		ids := make([]string, len(input.Documents))
		for i, d := range input.Documents {
			ids[i] = d.ID
		}
		if out.Files, err = documents(`SELECT d.id, COALESCE(d.set_id, ''), COALESCE(st.connector_id, '') FROM foundry_documents d
LEFT JOIN foundry_document_sets st ON st.id = d.set_id
WHERE d.workspace_id=? AND d.id IN (`+idPlaceholders(len(ids))+`) ORDER BY d.id`, append([]any{workspaceID}, toAnySlice(ids)...)); err != nil {
			return access.Holdings{}, err
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT ldc.type, ldc.prop, dd.decision_id IS NOT NULL, COALESCE(d.set_id, ''), COALESCE(st.connector_id, '')
FROM foundry_load_decisions ldc
LEFT JOIN foundry_review_decision_documents dd ON dd.decision_id = ldc.id
LEFT JOIN foundry_documents d ON d.id = dd.document_id
LEFT JOIN foundry_document_sets st ON st.id = d.set_id
WHERE ldc.load_id=? AND ldc.kind='prop' ORDER BY ldc.type, ldc.prop`, loadID)
	if err != nil {
		return access.Holdings{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var ty, prop string
		var recorded bool
		var set access.SetRef
		if err := rows.Scan(&ty, &prop, &recorded, &set.SetID, &set.ConnectorID); err != nil {
			return access.Holdings{}, err
		}
		i := slices.IndexFunc(out.Decisions, func(d access.PropDecision) bool { return d.Type == ty && d.Prop == prop })
		if i < 0 {
			out.Decisions = append(out.Decisions, access.PropDecision{Type: ty, Prop: prop, Recorded: true})
			i = len(out.Decisions) - 1
		}
		out.Decisions[i].Recorded = out.Decisions[i].Recorded && recorded
		if recorded {
			out.Decisions[i].Sets = append(out.Decisions[i].Sets, set)
		}
	}
	return out, rows.Err()
}

// ListFoundryOntologiesHoldingDocument returns the live ontologies of the
// workspace whose current load holds the document: as one of its documents,
// one of its table files, or what one of its prop decisions came from.
func (s *mysqlDB) ListFoundryOntologiesHoldingDocument(ctx context.Context, workspaceID, documentID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT o.id FROM foundry_ontologies o
JOIN foundry_load_documents ld ON ld.load_id = o.current_load_id AND ld.document_id = ?
WHERE o.workspace_id=? AND o.deleted_at IS NULL
UNION SELECT o.id FROM foundry_ontologies o
JOIN foundry_load_decisions ldc ON ldc.load_id = o.current_load_id AND ldc.kind = 'prop'
JOIN foundry_review_decision_documents dd ON dd.decision_id = ldc.id AND dd.document_id = ?
WHERE o.workspace_id=? AND o.deleted_at IS NULL
UNION SELECT o.id FROM foundry_ontologies o
JOIN foundry_loads l ON l.id = o.current_load_id
JOIN foundry_ontology_versions v ON v.id = l.version_id
JOIN foundry_ontology_generations g ON g.id = v.generation_id
WHERE o.workspace_id=? AND o.deleted_at IS NULL AND JSON_CONTAINS(COALESCE(JSON_EXTRACT(g.input, '$.documents[*].id'), JSON_ARRAY()), JSON_QUOTE(?))
ORDER BY 1`, documentID, workspaceID, documentID, workspaceID, workspaceID, documentID)
	if err != nil {
		return nil, err
	}
	return scanStrings(rows)
}
