package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/sourceaccess"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// toolReadGraph reads a load of an ontology.
	toolReadGraph = "helix_foundry_read_graph"
	// toolListQuestions lists an ontology's questions, with their text.
	toolListQuestions = "helix_foundry_list_questions"
)

// heldLoad is the load a graph read is about to read, and what it holds
// then (holdingBefore).
type heldLoad struct {
	id       string
	holdings access.Holdings
}

// holdingBefore is what the load a restricted member's or a bot's graph
// read of input is about to read holds, before it runs: the input's load,
// else the ontology's current one. A document the read drew on may leave
// the load while it runs (a sync trims an old day of a channel), so drew
// reads what it held on either side. The zero heldLoad for any other call,
// or for a load that isn't one to read (not found, not ready, deleted),
// which the read itself refuses or reads as it is after. A read that fails
// fails the attempt, to retry.
func holdingBefore(ctx context.Context, db Store, workspaceID, name string, input json.RawMessage) (heldLoad, error) {
	if name != toolReadGraph {
		return heldLoad{}, nil
	}
	var in struct {
		OntologyID string `json:"ontology_id"`
		LoadID     string `json:"load_id"`
	}
	if json.Unmarshal(input, &in) != nil {
		return heldLoad{}, nil
	}
	if in.LoadID == "" && in.OntologyID != "" {
		o, err := db.GetFoundryOntology(ctx, workspaceID, in.OntologyID)
		switch {
		case errors.Is(err, foundrystore.ErrNotFound):
			return heldLoad{}, nil
		case err != nil:
			return heldLoad{}, accessRetry(err)
		case o.CurrentLoadID != nil:
			in.LoadID = *o.CurrentLoadID
		}
	}
	if in.LoadID == "" {
		return heldLoad{}, nil
	}
	holdings, err := db.FoundryLoadHoldings(ctx, workspaceID, in.LoadID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return heldLoad{}, nil
	case err != nil:
		return heldLoad{}, accessRetry(err)
	}
	return heldLoad{id: in.LoadID, holdings: holdings}, nil
}

// drew are the items of audienced sources (Slack channels) that a restricted
// member's or a bot's call of name, with input, which returned output, drew
// on, read under targets (their grants) with visible shared with them, as the
// API told them on either side of the call:
//   - a graph read's: the items of the documents, table files and table rows
//     of its load that they read, and the directory (access.DirectoryItem) of
//     each source whose directory they read (sourceaccess.Told, as the API
//     narrows the read, told the directory with the items), as the load held
//     them before it ran (before, when it is that load) and after. A result
//     names the documents of its quotes alone, not those its rows, entities,
//     counts and derived facts came from, so a read draws on every one it
//     could read; one whose load can't be told, on every item visible;
//   - an ask's or a get_question's: its question's (SeenItems), which its
//     answer stays the reader's with; a list_questions', every listed one's,
//     whose text may say what its answer read;
//   - none for any other call: no other tool a grantee has returns what a
//     document says.
func drew(ctx context.Context, db Store, workspaceID, name string, input json.RawMessage, output any,
	targets []access.Target, visible []access.ItemRef, before heldLoad) ([]access.ItemRef, error) {
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
	case toolListQuestions:
		var in struct {
			OntologyID string `json:"ontology_id"`
		}
		var out struct {
			Questions []struct {
				ID string `json:"id"`
			} `json:"questions"`
		}
		if raw, err := json.Marshal(output); err != nil || json.Unmarshal(raw, &out) != nil || json.Unmarshal(input, &in) != nil {
			return visible, nil
		}
		// One read a question listed (50 a page at most): a page read again
		// might not be the page the call returned.
		var items []access.ItemRef
		for _, listed := range out.Questions {
			question, err := db.GetFoundryQuestion(ctx, in.OntologyID, listed.ID)
			if err != nil {
				return nil, err
			}
			items = append(items, question.SeenItems...)
		}
		return items, nil
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
	if len(sourceaccess.Audienced(connectors)) == 0 {
		return nil, nil
	}
	holdings, err := db.FoundryLoadHoldings(ctx, workspaceID, read.LoadID)
	if err != nil {
		return nil, err
	}
	grants := sourceaccess.Told(connectors, targets, visible)
	items := grants.Part(holdings).Items
	if before.id == read.LoadID {
		items = slices.Concat(items, grants.Part(before.holdings).Items)
	}
	return items, nil
}

// accessRetry fails an attempt whose read or record of the reader's access
// failed, to retry.
func accessRetry(err error) error {
	return &Failure{Message: "Your access to this workspace couldn't be read; Helix will retry", Err: err}
}

// recordDrew records on turn's thread the items a restricted member's or a
// bot's call drew on (drew), read between held and holds, what they read
// before and after it (either may have given the call what it read), with
// before what its load held before it ran: once its result is theirs, the
// thread is theirs only while each is still shared with them. One shared
// with them no longer, after the call, fails the turn for good before its
// result reaches the model or the person; the thread, holding it, is no
// longer theirs. A read or a write that fails fails the attempt, to retry.
func recordDrew(ctx context.Context, db Store, turn *model.FoundryAgentTurn, name string, input json.RawMessage, output any,
	held, holds threadView, before heldLoad) error {
	items, err := drew(ctx, db, turn.WorkspaceID, name, input, output, slices.Concat(held.targets, holds.targets),
		access.SortItems(slices.Concat(held.items, holds.items)), before)
	if err != nil {
		return accessRetry(err)
	}
	if len(items) == 0 {
		return nil
	}
	if err := db.RecordFoundryAgentThreadGrants(ctx, turn.WorkspaceID, turn.ThreadID,
		&model.FoundryAgentThreadGrants{Targets: holds.targets, Items: access.SortItems(slices.Clone(items))}); err != nil {
		return accessRetry(err)
	}
	if slices.ContainsFunc(items, func(item access.ItemRef) bool { return !slices.Contains(holds.items, item) }) {
		return &Failure{Message: "This conversation holds what your access no longer lets you read: start a new one", Permanent: true}
	}
	return nil
}
