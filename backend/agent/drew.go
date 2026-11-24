package agent

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// toolReadGraph reads a load of an ontology.
const toolReadGraph = "helix_foundry_read_graph"

// drew are the items of audienced sources (Slack channels) that a restricted
// member's or a bot's call of name, with input, which returned output, drew
// on, read under targets (their grants) with visible shared with them, as the
// API told them on either side of the call:
//   - a graph read's: the items of the documents and table files of its load
//     that they read (access.Grants.Part, as the API narrows the read). A
//     result names the documents of its quotes alone, not those its rows,
//     entities, counts and derived facts came from, so a read draws on every
//     one it could read; one whose load can't be told, on every item visible;
//   - an ask's or a get_question's: its question's (SeenItems), which its
//     answer stays the reader's with;
//   - none for any other call: no other tool a grantee has returns what a
//     document says.
//
// A table of an audienced source is never theirs (access.Grants.CoversTable),
// so no row's item is.
func drew(ctx context.Context, db Store, workspaceID, name string, input json.RawMessage, output any,
	targets []access.Target, visible []access.ItemRef) ([]access.ItemRef, error) {
	switch name {
	case toolAsk, toolGetQuestion:
		q := questionOf(name, input, output)
		if q == nil {
			return nil, nil
		}
		question, err := db.GetFoundryQuestion(ctx, q.OntologyID, q.ID)
		if err != nil {
			return nil, err
		}
		return question.SeenItems, nil
	case toolReadGraph:
	default:
		return nil, nil
	}
	var read struct {
		LoadID string `json:"load_id"`
	}
	if raw, err := json.Marshal(output); err != nil || json.Unmarshal(raw, &read) != nil || read.LoadID == "" {
		return visible, nil
	}
	connectors, err := db.ListConnectors(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	kinds := map[string]connector.Audienced{}
	for _, c := range connectors {
		if kind, ok := connector.AudiencedKind(c.Kind); ok {
			kinds[c.ID] = kind
		}
	}
	if len(kinds) == 0 {
		return nil, nil
	}
	holdings, err := db.FoundryLoadHoldings(ctx, workspaceID, read.LoadID)
	if err != nil {
		return nil, err
	}
	reach := make(map[string]access.SourceReach, len(kinds))
	for id := range kinds {
		var keys []string
		for _, item := range visible {
			if item.ConnectorID == id {
				keys = append(keys, item.Item)
			}
		}
		reach[id] = access.SharedReach(keys)
	}
	grants := access.NewGrants(targets).WithSharing(access.NewSharing(reach, func(connectorID, stream, externalID string) (string, bool) {
		kind, ok := kinds[connectorID]
		if !ok {
			return "", false
		}
		return kind.ItemOfDocument(stream, externalID)
	}))
	return grants.Part(holdings).Items, nil
}

// recordDrew records on turn's thread the items a restricted member's or a
// bot's call drew on (drew), read between held and holds, what they read
// before and after it (either may have given the call what it read): once
// its result is theirs, the thread is theirs only
// while each is still shared with them. One shared with them no longer, after
// the call, fails the turn for good before its result reaches the model or the
// person; the thread, holding it, is no longer theirs. A read or a write that
// fails fails the attempt, to retry.
func recordDrew(ctx context.Context, db Store, turn *model.FoundryAgentTurn, name string, input json.RawMessage, output any,
	held, holds threadView) error {
	retry := func(err error) error {
		return &Failure{Message: "Your access to this workspace couldn't be read; Helix will retry", Err: err}
	}
	items, err := drew(ctx, db, turn.WorkspaceID, name, input, output, slices.Concat(held.targets, holds.targets),
		access.SortItems(slices.Concat(held.items, holds.items)))
	if err != nil {
		return retry(err)
	}
	if len(items) == 0 {
		return nil
	}
	if err := db.RecordFoundryAgentThreadGrants(ctx, turn.WorkspaceID, turn.ThreadID,
		&model.FoundryAgentThreadGrants{Targets: holds.targets, Items: access.SortItems(slices.Clone(items))}); err != nil {
		return retry(err)
	}
	if slices.ContainsFunc(items, func(item access.ItemRef) bool { return !slices.Contains(holds.items, item) }) {
		return &Failure{Message: "This conversation holds what your access no longer lets you read: start a new one", Permanent: true}
	}
	return nil
}
