package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// An action's display is what its card names, resolved by Foundry from the
// workspace when the action is prepared, never the model's words: one shape
// per operation, which the site reads by the action's operation. A name
// that can't be read (gone since, or never there) is left out, its id kept.

// Ref is one thing of the workspace: its id and its name.
type Ref struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// ConnectorRef is a connector, with its kind (postgres, stripe, ...), and
// whether it reads the sample company's made-up database.
type ConnectorRef struct {
	Ref
	Kind   string `json:"kind,omitempty"`
	Sample bool   `json:"sample,omitempty"`
}

// VersionRef is an ontology version by its number, as the site shows it (v3).
type VersionRef struct {
	ID     string `json:"id"`
	Number int    `json:"number,omitempty"`
}

// LoadRef is a load: the version it wrote, its status and when it started.
type LoadRef struct {
	ID        string     `json:"id"`
	Version   VersionRef `json:"version"`
	Status    string     `json:"status,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

// CreateOntologyDisplay is create_ontology's: an ontology on an existing
// database (Database), or on a new one made first (NewDatabase); exactly one.
type CreateOntologyDisplay struct {
	Name        string              `json:"name"`
	Database    *Ref                `json:"database,omitempty"`
	NewDatabase *NewDatabaseDisplay `json:"new_database,omitempty"`
}

// NewDatabaseDisplay is a database create_ontology makes: in an existing
// project (ProjectID: Foundry holds no project names) or in a new one
// (NewProject, its name); exactly one.
type NewDatabaseDisplay struct {
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	ProjectID  string `json:"project_id,omitempty"`
	NewProject string `json:"new_project,omitempty"`
}

// ConnectorScope is which connectors a generation reads: a closed set.
type ConnectorScope string

const (
	// ConnectorsEvery reads every connector the workspace has (Connectors
	// lists them as they are now), as an empty connector_ids does.
	ConnectorsEvery ConnectorScope = "every"
	// ConnectorsNone reads none: documents alone (no_connectors).
	ConnectorsNone ConnectorScope = "none"
	// ConnectorsChosen reads the ones Connectors names.
	ConnectorsChosen ConnectorScope = "chosen"
)

// GenerateDisplay is generate_ontology's: the draft's goal and what it reads.
type GenerateDisplay struct {
	Ontology       Ref                  `json:"ontology"`
	Goal           string               `json:"goal,omitempty"`
	ConnectorsRead ConnectorScope       `json:"connectors_read"`
	Connectors     []ConnectorRef       `json:"connectors"`
	DocumentSets   []Ref                `json:"document_sets"`
	EventSources   []EventSourceDisplay `json:"event_sources,omitempty"`
	// Sample is "acme" for the sample company's ready-made draft.
	Sample string `json:"sample,omitempty"`
}

// EventSourceDisplay is an API source a generation reads, and how.
type EventSourceDisplay struct {
	Connector ConnectorRef `json:"connector"`
	Mode      string       `json:"mode"`
}

// SetCurrentVersionDisplay is set_current_version's: the version made
// current in place of Current, nil when none is.
type SetCurrentVersionDisplay struct {
	Ontology Ref         `json:"ontology"`
	Version  VersionRef  `json:"version"`
	Current  *VersionRef `json:"current,omitempty"`
}

// CreateLoadDisplay is create_load's: the version loaded.
type CreateLoadDisplay struct {
	Ontology Ref        `json:"ontology"`
	Version  VersionRef `json:"version"`
}

// SetCurrentLoadDisplay is set_current_load's: the load made current in
// place of Current, nil when none is.
type SetCurrentLoadDisplay struct {
	Ontology Ref      `json:"ontology"`
	Load     LoadRef  `json:"load"`
	Current  *LoadRef `json:"current,omitempty"`
}

// ReviseDisplay is revise_version's: the version revised and its edits, by
// item; a dry run only previews them.
type ReviseDisplay struct {
	Ontology Ref           `json:"ontology"`
	Version  VersionRef    `json:"version"`
	Edits    []EditDisplay `json:"edits"`
	DryRun   bool          `json:"dry_run"`
}

// EditDisplay is one edit: kind put (its TOML is in the payload) or remove.
type EditDisplay struct {
	Kind   string `json:"kind"`
	ItemID string `json:"item_id"`
}

// SyncDisplay is sync_connector's.
type SyncDisplay struct {
	Connector ConnectorRef `json:"connector"`
}

// OntologyDisplay is add_sample_data's: the ontology it adds the sample for.
type OntologyDisplay struct {
	Ontology Ref `json:"ontology"`
}

// CreateSetDisplay is create_document_set's.
type CreateSetDisplay struct {
	Ontology    Ref    `json:"ontology"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// UpdateSetDisplay is update_document_set's: the set as it is named now,
// and what changes; a field left nil stays as it is, an empty description
// clears it.
type UpdateSetDisplay struct {
	Ontology    Ref     `json:"ontology"`
	Set         Ref     `json:"set"`
	Name        *string `json:"name,omitempty"`
	Description *string `json:"description,omitempty"`
}

// MoveDisplay is move_document's.
type MoveDisplay struct {
	Ontology Ref `json:"ontology"`
	Document Ref `json:"document"`
	From     Ref `json:"from"`
	To       Ref `json:"to"`
}

// DecisionKind is what a settle_review decides: a closed set.
type DecisionKind string

const (
	// DecisionLink links the review's name to EntityKey.
	DecisionLink DecisionKind = "link"
	// DecisionNotInData says the name is no one in the data.
	DecisionNotInData DecisionKind = "not_in_data"
	// DecisionSetValue sets the property to Value.
	DecisionSetValue DecisionKind = "set_value"
	// DecisionRemoveValue removes the property.
	DecisionRemoveValue DecisionKind = "remove_value"
	// DecisionApprove writes the statement the review holds back.
	DecisionApprove DecisionKind = "approve"
	// DecisionReject says the statement is wrong: nothing is written.
	DecisionReject DecisionKind = "reject"
)

// SettleDisplay is settle_review's: the review, as the current load held it
// when the action was prepared, and the decision.
type SettleDisplay struct {
	Ontology Ref                             `json:"ontology"`
	LoadID   string                          `json:"load_id"`
	Review   integration.FoundryReviewOutput `json:"review"`
	// Document is the document the review was read from.
	Document *Ref            `json:"document,omitempty"`
	Decision DecisionDisplay `json:"decision"`
}

// DecisionDisplay is a decision: EntityKey for a link, with whether it is
// one of the review's candidates; Value, in settle_review's shape
// ({text|integer|number|boolean: value}), for a value set.
type DecisionDisplay struct {
	Kind      DecisionKind   `json:"kind"`
	EntityKey string         `json:"entity_key,omitempty"`
	Candidate bool           `json:"candidate,omitempty"`
	Value     map[string]any `json:"value,omitempty"`
}

// ReviewReader reads one review of an ontology's current load as one
// person, and the load it is of; nil when the load doesn't hold it.
type ReviewReader func(ctx context.Context, ontologyID, reviewKey string) (*integration.FoundryReviewOutput, string, error)

// Reads is what drawing a card reads of Foundry's store.
type Reads interface {
	GetFoundryOntology(ctx context.Context, workspaceID, id string) (*model.FoundryOntology, error)
	ListFoundryOntologies(ctx context.Context, workspaceID, pageToken string, limit int) ([]*model.FoundryOntology, string, error)
	GetOntologyVersion(ctx context.Context, ontologyID, id string) (*model.OntologyVersion, error)
	GetFoundryLoad(ctx context.Context, ontologyID, id string) (*model.FoundryLoad, error)
	GetConnector(ctx context.Context, workspaceID, id string) (*model.Connector, error)
	ListConnectors(ctx context.Context, workspaceID string) ([]*model.Connector, error)
	GetFoundryDocumentSet(ctx context.Context, workspaceID, id string) (*model.FoundryDocumentSet, error)
	GetFoundryDocument(ctx context.Context, workspaceID, id string) (*model.FoundryDocument, error)
}

// Cards draws the cards of actions prepared in one workspace for one
// person: what an action acts on, which the workspace must hold, and its
// display, checked as that person reads it now. A turn draws a card when
// its model prepares an action; a Refresh draws it again, as its clicker,
// with the confirmation it renews.
type Cards struct {
	DB          Reads
	WorkspaceID string
	// Databases says whose a database is, for create_ontology on one; nil
	// on a host that can't, which refuses such an action.
	Databases ports.Databases
	// Reviews reads a review as the person, for settle_review; nil refuses it.
	Reviews ReviewReader
	// Sample is the host's sample company database; nil on a host without.
	Sample *integration.SampleDatabase
}

// SessionCards are the cards of a person's session: its reviews read as
// them, and its host's sample database.
func SessionCards(db Reads, databases ports.Databases, session integration.Session, workspaceID string) Cards {
	return Cards{DB: db, WorkspaceID: workspaceID, Databases: databases, Sample: session.Service.SampleDatabase,
		Reviews: func(ctx context.Context, ontologyID, reviewKey string) (*integration.FoundryReviewOutput, string, error) {
			listed, _, err := session.Service.ListFoundryReviews(ctx, session.Identity, &integration.Call{}, integration.ListFoundryReviewsInput{
				OntologyInput: integration.OntologyInput{WorkspaceID: workspaceID, OntologyID: ontologyID}, ReviewKeys: []string{reviewKey}})
			if err != nil {
				return nil, "", err
			}
			for _, r := range listed.Reviews {
				if r.ReviewKey == reviewKey {
					return &r, listed.LoadID, nil
				}
			}
			return nil, listed.LoadID, nil
		}}
}

// Card is an action's card as drawn: its display, and what it acts on.
type Card struct {
	Display json.RawMessage
	target  target
}

// Draw checks a acts in the workspace on something it holds, and draws its
// card. Its refusals are the action's words for whoever prepared it.
func (c Cards) Draw(ctx context.Context, a integration.PreparedAction) (Card, error) {
	t, err := c.target(ctx, a)
	if err != nil {
		return Card{}, err
	}
	display, err := c.describe(ctx, a, t)
	if err != nil {
		return Card{}, err
	}
	raw, err := json.Marshal(display)
	return Card{Display: raw, target: t}, err
}

// describe is a's display, in the target t names. settle_review is refused
// when its decision isn't one the review takes, and generate_ontology when
// reading every connector would read the sample's, before its person sees it.
func (c Cards) describe(ctx context.Context, a integration.PreparedAction, t target) (any, error) {
	ws := c.WorkspaceID
	ontology := Ref{ID: t.id, Name: t.name}
	// An ontology target is one the workspace holds (target): its versions and
	// loads are read under it.
	version := func(id string) VersionRef {
		out := VersionRef{ID: id}
		if v, err := c.DB.GetOntologyVersion(ctx, t.id, id); err == nil {
			out.Number = v.Version
		}
		return out
	}
	set := func(id string) Ref {
		out := Ref{ID: id}
		if set, err := c.DB.GetFoundryDocumentSet(ctx, ws, id); err == nil {
			out.Name = set.DisplayName()
		}
		return out
	}
	connector := func(co *model.Connector) ConnectorRef {
		return ConnectorRef{Ref: Ref{ID: co.ID, Name: co.Name}, Kind: co.Kind, Sample: c.Sample != nil && c.Sample.Holds(co.Kind, co.Config)}
	}
	connectorOf := func(id string) ConnectorRef {
		if co, err := c.DB.GetConnector(ctx, ws, id); err == nil {
			return connector(co)
		}
		return ConnectorRef{Ref: Ref{ID: id}}
	}
	load := func(id string) LoadRef {
		out := LoadRef{ID: id}
		if l, err := c.DB.GetFoundryLoad(ctx, t.id, id); err == nil {
			out.Version, out.Status, out.CreatedAt = VersionRef{ID: l.VersionID, Number: l.Version}, string(l.Status), &l.CreatedAt
		}
		return out
	}
	decode := func(into any) error {
		if err := json.Unmarshal(a.Payload, into); err != nil {
			return fmt.Errorf("read the %s payload: %w", a.Operation, err)
		}
		return nil
	}
	switch a.Operation {
	case "create_ontology":
		var p integration.CreateOntologyPayload
		if err := decode(&p); err != nil {
			return nil, err
		}
		out := CreateOntologyDisplay{Name: p.Name}
		if d := p.Database; d != nil {
			out.NewDatabase = &NewDatabaseDisplay{Name: d.Name, Slug: d.Slug, ProjectID: d.ProjectID, NewProject: d.ProjectName}
		} else {
			out.Database = &Ref{ID: t.id, Name: t.name}
		}
		return out, nil
	case "generate_ontology":
		var p integration.GenerateOntologyPayload
		if err := decode(&p); err != nil {
			return nil, err
		}
		out := GenerateDisplay{Ontology: ontology, Goal: p.Goal, Sample: p.Sample, Connectors: []ConnectorRef{}, DocumentSets: []Ref{}}
		switch {
		case p.NoConnectors:
			out.ConnectorsRead = ConnectorsNone
		case len(p.ConnectorIDs) > 0:
			out.ConnectorsRead = ConnectorsChosen
			for _, id := range p.ConnectorIDs {
				out.Connectors = append(out.Connectors, connectorOf(id))
			}
		default:
			out.ConnectorsRead = ConnectorsEvery
			all, err := c.DB.ListConnectors(ctx, ws)
			if err != nil {
				return nil, err
			}
			// API sources are read only when named, as event_sources.
			for _, co := range all {
				if co.Kind != model.ConnectorKindEvents {
					out.Connectors = append(out.Connectors, connector(co))
				}
			}
			// The sample's made-up data is only for the sample's own draft, as the site
			// leaves it out of every other: it is read only when named.
			if i := slices.IndexFunc(out.Connectors, func(r ConnectorRef) bool { return r.Sample }); i >= 0 {
				return nil, status.Error(codes.FailedPrecondition, "an empty connector_ids would read every connector, the made-up sample company's ("+
					out.Connectors[i].ID+") too: name the connector_ids to read, or set no_connectors")
			}
		}
		for _, id := range p.DocumentSetIDs {
			out.DocumentSets = append(out.DocumentSets, set(id))
		}
		for _, pick := range p.EventSources {
			out.EventSources = append(out.EventSources, EventSourceDisplay{Connector: connectorOf(pick.ConnectorID), Mode: pick.Mode})
		}
		return out, nil
	case "set_current_version":
		var p integration.SetCurrentVersionPayload
		if err := decode(&p); err != nil {
			return nil, err
		}
		out := SetCurrentVersionDisplay{Ontology: ontology, Version: version(p.VersionID)}
		if p.ExpectedCurrentVersionID != "" {
			current := version(p.ExpectedCurrentVersionID)
			out.Current = &current
		}
		return out, nil
	case "create_load":
		var p integration.CreateLoadPayload
		if err := decode(&p); err != nil {
			return nil, err
		}
		return CreateLoadDisplay{Ontology: ontology, Version: version(p.VersionID)}, nil
	case "set_current_load":
		var p integration.SetCurrentLoadPayload
		if err := decode(&p); err != nil {
			return nil, err
		}
		out := SetCurrentLoadDisplay{Ontology: ontology, Load: load(p.LoadID)}
		if t.ontology != nil && t.ontology.CurrentLoadID != nil {
			current := load(*t.ontology.CurrentLoadID)
			out.Current = &current
		}
		return out, nil
	case "revise_version":
		var p integration.ReviseVersionPayload
		if err := decode(&p); err != nil {
			return nil, err
		}
		out := ReviseDisplay{Ontology: ontology, Version: version(p.VersionID), DryRun: p.DryRun, Edits: make([]EditDisplay, 0, len(p.Edits))}
		for _, e := range p.Edits {
			if e.Put != nil {
				out.Edits = append(out.Edits, EditDisplay{Kind: "put", ItemID: e.Put.ItemID})
			} else if e.Remove != nil {
				out.Edits = append(out.Edits, EditDisplay{Kind: "remove", ItemID: e.Remove.ItemID})
			}
		}
		return out, nil
	case "sync_connector":
		return SyncDisplay{Connector: ConnectorRef{Ref: Ref{ID: t.id, Name: t.name}, Kind: t.connectorKind}}, nil
	case "add_sample_data":
		return OntologyDisplay{Ontology: ontology}, nil
	case "create_document_set":
		var p integration.CreateDocumentSetPayload
		if err := decode(&p); err != nil {
			return nil, err
		}
		return CreateSetDisplay{Ontology: ontology, Name: p.Name, Description: p.Description}, nil
	case "update_document_set":
		var p integration.UpdateDocumentSetPayload
		if err := decode(&p); err != nil {
			return nil, err
		}
		return UpdateSetDisplay{Ontology: ontology, Set: set(p.SetID), Name: p.Name, Description: p.Description}, nil
	case "move_document":
		var p integration.MoveDocumentPayload
		if err := decode(&p); err != nil {
			return nil, err
		}
		out := MoveDisplay{Ontology: ontology, Document: Ref{ID: p.DocumentID}, To: set(p.SetID)}
		if d, err := c.DB.GetFoundryDocument(ctx, ws, p.DocumentID); err == nil {
			out.Document.Name, out.From = d.Name, set(d.SetID)
		}
		return out, nil
	case "settle_review":
		var p integration.SettleReviewPayload
		if err := decode(&p); err != nil {
			return nil, err
		}
		return c.describeSettle(ctx, ontology, p)
	}
	return nil, fmt.Errorf("no card for %s", a.Operation)
}

// describeSettle reads the review settle_review decides as the person, and
// refuses a decision it doesn't take: the review must be in the current
// load, settled by that kind of decision, and, for an approval or a reject,
// read as the statement the decision names.
func (c Cards) describeSettle(ctx context.Context, ontology Ref, p integration.SettleReviewPayload) (any, error) {
	refuse := func(why string) error { return status.Error(codes.FailedPrecondition, why) }
	if c.Reviews == nil {
		return nil, status.Error(codes.Unavailable, "the Assistant can't read reviews here")
	}
	review, loadID, err := c.Reviews(ctx, ontology.ID, p.ReviewID)
	if err != nil {
		return nil, err
	}
	if review == nil {
		return nil, refuse("the current load holds no review " + p.ReviewID + ": list them again with helix_foundry_list_reviews")
	}
	out := SettleDisplay{Ontology: ontology, LoadID: loadID, Review: *review}
	if review.DocID != "" {
		doc := Ref{ID: review.DocID}
		if d, err := c.DB.GetFoundryDocument(ctx, c.WorkspaceID, review.DocID); err == nil {
			doc.Name = d.Name
		}
		out.Document = &doc
	}
	d := p.Decision
	// The validated payload holds exactly one decision.
	var takes, hash string
	switch {
	case d.Resolve != nil && d.Resolve.NotInData:
		takes, out.Decision = "resolve", DecisionDisplay{Kind: DecisionNotInData}
	case d.Resolve != nil:
		takes, out.Decision = "resolve", DecisionDisplay{Kind: DecisionLink, EntityKey: d.Resolve.EntityKey,
			Candidate: slices.Contains(review.Candidates, d.Resolve.EntityKey)}
	case d.Prop != nil && d.Prop.Null:
		takes, out.Decision = "prop", DecisionDisplay{Kind: DecisionRemoveValue}
	case d.Prop != nil:
		value := map[string]any{}
		switch v := d.Prop; {
		case v.Text != nil:
			value["text"] = *v.Text
		case v.Integer != nil:
			value["integer"] = *v.Integer
		case v.Number != nil:
			value["number"] = *v.Number
		case v.Boolean != nil:
			value["boolean"] = *v.Boolean
		}
		takes, out.Decision = "prop", DecisionDisplay{Kind: DecisionSetValue, Value: value}
	case d.Release != nil:
		takes, hash, out.Decision = "release", d.Release.StatementHash, DecisionDisplay{Kind: DecisionApprove}
	case d.Reject != nil:
		takes, hash, out.Decision = "release", d.Reject.StatementHash, DecisionDisplay{Kind: DecisionReject}
	}
	switch {
	case review.Settle == "stale":
		return nil, refuse("this review lacks what settling it reads, as a load from before decisions wrote it: a new load writes it again")
	case review.Settle == "none":
		return nil, refuse("no decision settles this review")
	case review.Settle != takes:
		return nil, refuse("this review takes a " + review.Settle + " decision, as helix_foundry_list_reviews says")
	case hash != "" && hash != review.StatementHash:
		return nil, refuse("the statement reads differently now: list the review again and decide on what it says")
	}
	return out, nil
}
