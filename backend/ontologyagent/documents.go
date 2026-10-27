package ontologyagent

import (
	"context"
	"fmt"
	"slices"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// documents.go is a generation's table files as sources: each uploaded CSV,
// TSV, JSON, JSONL or Parquet document whose read succeeded is a table of
// its own, whose source id is its document id, read from where its read
// stored it.

// DocumentTable is a table file a generation recorded that can be read.
type DocumentTable struct {
	Document *model.FoundryDocument
	// SetName is the name of the set it is in now.
	SetName string
}

// DisplayName is what the user calls it: "Set / file".
func (d DocumentTable) DisplayName() string { return TableDisplayName(d.SetName, d.Document.Name) }

// Format is how its read stored its rows: a JSON document as it is, and
// every other table file as JSON Lines.
func (d DocumentTable) Format() FileFormat {
	if d.Document.MediaType == model.FoundryMediaJSON {
		return FileJSON
	}
	return FileJSONL
}

// DocumentTableStore reads a generation's table files.
type DocumentTableStore interface {
	ListFoundryDocumentsByID(ctx context.Context, workspaceID string, ids []string) (map[string]*model.FoundryDocument, error)
	ListFoundryDocumentSets(ctx context.Context, workspaceID string) ([]*model.FoundryDocumentSet, error)
}

// GoneDocumentTable is a table file a generation recorded that can't be
// read any more: deleted, or moved out of the sets read.
type GoneDocumentTable struct {
	ID string
	// Name is its name, or its id when no row is left.
	Name string
}

// ResolveDocumentTables returns, of the table files a generation recorded,
// those that can still be read, in the order recorded: uploaded, their read
// succeeded, and in one of sets now. Gone are the others.
func ResolveDocumentTables(ctx context.Context, db DocumentTableStore, workspaceID string, recorded []model.OntologyGenerationDocument,
	sets []string) (present []DocumentTable, gone []GoneDocumentTable, err error) {
	if len(recorded) == 0 {
		return nil, nil, nil
	}
	ids := make([]string, len(recorded))
	for i, r := range recorded {
		ids[i] = r.ID
	}
	docs, err := db.ListFoundryDocumentsByID(ctx, workspaceID, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("read the generation's table files: %w", err)
	}
	all, err := db.ListFoundryDocumentSets(ctx, workspaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("read the workspace's document sets: %w", err)
	}
	names := make(map[string]string, len(all))
	for _, s := range all {
		names[s.ID] = s.Name
	}
	for _, r := range recorded {
		d := docs[r.ID]
		if d == nil {
			gone = append(gone, GoneDocumentTable{ID: r.ID, Name: r.ID})
			continue
		}
		if d.Status != model.FoundryDocumentUploaded || d.Read == nil || d.Read.Status != model.FoundryFileReadSucceeded ||
			d.Read.ArtifactKey == nil || d.MediaType.ReadKind() != model.FoundryReadTable || !slices.Contains(sets, d.SetID) {
			gone = append(gone, GoneDocumentTable{ID: r.ID, Name: d.Name})
			continue
		}
		present = append(present, DocumentTable{Document: d, SetName: names[d.SetID]})
	}
	return present, gone, nil
}
