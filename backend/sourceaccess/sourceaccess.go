// Package sourceaccess builds what a grantee's source grants give it once the
// sources' own permissions narrow them (plans/SOURCE_ACLS.md): the grants
// the store holds for it and its teams, and, for each connector whose kind
// reports who its source shares each item with (connector.Audienced), the
// items its source shares with the grantee, matched by the grantee's
// verified email. Package api and the ask worker build every grantee's
// grants here, so what one reads is the same everywhere.
package sourceaccess

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Store is what grants and sharing are read from.
type Store interface {
	ListFoundryAccessGrants(ctx context.Context, workspaceID string, principals []access.Principal) ([]model.FoundryAccessGrant, error)
	ListConnectors(ctx context.Context, workspaceID string) ([]*model.Connector, error)
	SourceSharingFor(ctx context.Context, workspaceID string, reader *connector.Email) ([]model.FoundrySourceSharing, error)
}

// Reads builds grantees' grants for one call, reading each reader's email
// and what the sources share with it once, so every check the call makes
// sees one moment of them. Make one per call with New.
type Reads struct {
	db Store
	// people tells a user's verified email; nil tells none, and every user's
	// sources' permissions are then unknown: they read none of their items.
	people ports.People
	now    time.Time

	mu     sync.Mutex
	emails map[[2]string]connector.Email
	// sharing is by workspace and reader: "agent" for no one, else
	// "person " and the email.
	sharing map[[2]string]access.Sharing
	// connectors are each workspace's live connectors.
	connectors map[string][]*model.Connector
}

// New reads through db and people as of now.
func New(db Store, people ports.People, now time.Time) *Reads {
	return &Reads{db: db, people: people, now: now, emails: map[[2]string]connector.Email{}, sharing: map[[2]string]access.Sharing{},
		connectors: map[string][]*model.Connector{}}
}

// Grants are what g holds: its own grants and its teams', narrowed by what
// each audienced source shares with g's principal.
func (r *Reads) Grants(ctx context.Context, g access.Grantee) (access.Grants, error) {
	held, err := r.db.ListFoundryAccessGrants(ctx, g.WorkspaceID, g.Principals())
	if err != nil {
		return access.Grants{}, fmt.Errorf("read grants: %w", err)
	}
	targets := make([]access.Target, len(held))
	for i, gr := range held {
		targets[i] = gr.Target
	}
	return r.Of(ctx, g.WorkspaceID, g.Principal, targets)
}

// Of is targets, given in workspaceID, as reader reads them: narrowed by
// what each audienced source shares with it, and a workspace grant to the
// workspace's live connectors. A user reads as their verified
// email: one the source has no account for, or no email at all, reads only
// the items that list them (none), and with no way to tell emails (people
// nil) the sources' items are unknown. A service credential (an agent's key)
// or a team, whose members each read as themselves, is no one in any
// source, and reads only what one shares with everyone.
func (r *Reads) Of(ctx context.Context, workspaceID string, reader access.Principal, targets []access.Target) (access.Grants, error) {
	grants := access.NewGrants(targets)
	if grants.Empty() {
		return grants, nil
	}
	var sharing access.Sharing
	var err error
	switch {
	case reader.Kind != access.PrincipalUser:
		sharing, err = r.sharingFor(ctx, workspaceID, nil)
	case r.people == nil:
		sharing, err = r.unknown(ctx, workspaceID)
	default:
		var email connector.Email
		if email, err = r.Email(ctx, workspaceID, reader.ID); err == nil {
			sharing, err = r.sharingFor(ctx, workspaceID, &email)
		}
	}
	if err != nil {
		return access.Grants{}, err
	}
	return grants.WithSharing(sharing), nil
}

// Email is userID's verified email in workspaceID, in the form sources'
// people are matched by; the zero Email when they have none, or it is no
// address, or nothing tells emails (people nil).
func (r *Reads) Email(ctx context.Context, workspaceID, userID string) (connector.Email, error) {
	if r.people == nil {
		return connector.Email{}, nil
	}
	key := [2]string{workspaceID, userID}
	r.mu.Lock()
	email, ok := r.emails[key]
	r.mu.Unlock()
	if ok {
		return email, nil
	}
	m, err := r.people.Member(ctx, workspaceID, userID)
	if err != nil {
		return connector.Email{}, fmt.Errorf("read the member's email: %w", err)
	}
	email, _ = connector.ParseEmail(m.Email) // none matches no one
	r.mu.Lock()
	r.emails[key] = email
	r.mu.Unlock()
	return email, nil
}

// liveConnectors are workspaceID's live connectors, read once per call.
func (r *Reads) liveConnectors(ctx context.Context, workspaceID string) ([]*model.Connector, error) {
	r.mu.Lock()
	connectors, ok := r.connectors[workspaceID]
	r.mu.Unlock()
	if ok {
		return connectors, nil
	}
	connectors, err := r.db.ListConnectors(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("read connectors: %w", err)
	}
	r.mu.Lock()
	r.connectors[workspaceID] = connectors
	r.mu.Unlock()
	return connectors, nil
}

// Audienced are those of connectors whose kinds report their sources'
// permissions (connector.AudiencedKind), by id.
//
//	sourceaccess.Audienced([]*model.Connector{{ID: "c1", Kind: "slack"}, {ID: "c2", Kind: "stripe"}}) // c1 alone
func Audienced(connectors []*model.Connector) map[string]connector.Audienced {
	kinds := map[string]connector.Audienced{}
	for _, c := range connectors {
		if kind, ok := connector.AudiencedKind(c.Kind); ok {
			kinds[c.ID] = kind
		}
	}
	return kinds
}

// Told is targets as a grantee reads them in a workspace whose live
// connectors are connectors, who the API told visible are the items the
// audienced ones share with it now
// (GetMyFoundryAccess's source_items, access.Grants.VisibleItems): what it
// reads of a load's documents and rows (access.Grants.Part.Items) is what the
// grants Of built that told it read. Founder's worker reads what a grantee's
// call read with it. The API tells a source's directory (access.RowsDirectory)
// as its item access.DirectoryItem, so Told reads it whole as the API did, and
// it is one of the items of a part that read it.
//
//	sourceaccess.Told(connectors, targets, []access.ItemRef{{ConnectorID: "c1", Item: "C1"}}) // reads c1's #general (C1) alone
func Told(connectors []*model.Connector, targets []access.Target, visible []access.ItemRef) access.Grants {
	kinds := Audienced(connectors)
	reach := make(map[string]access.SourceReach, len(kinds))
	for id := range kinds {
		var keys []string
		for _, item := range visible {
			if item.ConnectorID == id {
				keys = append(keys, item.Item)
			}
		}
		reach[id] = access.SharedReach(access.Shared{Items: keys})
	}
	return access.NewGrants(targets).WithSharing(newSharing(connectors, reach))
}

// newSharing is reach, by audienced connector of connectors, each told its
// documents' and rows' items by its kind, in a workspace whose live
// connectors are connectors.
func newSharing(connectors []*model.Connector, reach map[string]access.SourceReach) access.Sharing {
	live := make([]string, len(connectors))
	for i, c := range connectors {
		live[i] = c.ID
	}
	kinds := Audienced(connectors)
	told := make(map[string]access.SourceKind, len(kinds))
	for id, kind := range kinds {
		told[id] = kind
	}
	return access.NewSharing(live, reach, told)
}

// unknown is sharing in which every audienced connector of workspaceID
// reads as items unknown: its documents and tables go to full readers only.
func (r *Reads) unknown(ctx context.Context, workspaceID string) (access.Sharing, error) {
	connectors, err := r.liveConnectors(ctx, workspaceID)
	if err != nil {
		return access.Sharing{}, err
	}
	reach := map[string]access.SourceReach{}
	for id := range Audienced(connectors) {
		reach[id] = access.SourceReach{}
	}
	return newSharing(connectors, reach), nil
}

// sharingFor is what the sources of workspaceID's audienced connectors share
// with reader (nil: no one, an agent's key): a connector whose audiences
// were never stored, or are no longer fresh (access.AudiencesFresh), reads
// as items unknown.
func (r *Reads) sharingFor(ctx context.Context, workspaceID string, reader *connector.Email) (access.Sharing, error) {
	key := [2]string{workspaceID, "agent"}
	if reader != nil {
		key[1] = "person " + reader.String()
	}
	r.mu.Lock()
	sharing, ok := r.sharing[key]
	r.mu.Unlock()
	if ok {
		return sharing, nil
	}
	connectors, err := r.liveConnectors(ctx, workspaceID)
	if err != nil {
		return access.Sharing{}, err
	}
	reach := map[string]access.SourceReach{}
	for id := range Audienced(connectors) {
		reach[id] = access.SourceReach{}
	}
	if len(reach) > 0 {
		stored, err := r.db.SourceSharingFor(ctx, workspaceID, reader)
		if err != nil {
			return access.Sharing{}, fmt.Errorf("read what sources share: %w", err)
		}
		for _, s := range stored {
			if _, ok := reach[s.ConnectorID]; ok {
				reach[s.ConnectorID] = access.ReachOf(s.ReadAt, s.ProblemAt, access.Shared{Items: s.Visible, Member: s.Member}, r.now)
			}
		}
	}
	sharing = newSharing(connectors, reach)
	r.mu.Lock()
	r.sharing[key] = sharing
	r.mu.Unlock()
	return sharing, nil
}
