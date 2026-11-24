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
	// audienced are each workspace's connectors whose kinds report their
	// sources' permissions, by id.
	audienced map[string]map[string]connector.Audienced
}

// New reads through db and people as of now.
func New(db Store, people ports.People, now time.Time) *Reads {
	return &Reads{db: db, people: people, now: now, emails: map[[2]string]connector.Email{}, sharing: map[[2]string]access.Sharing{},
		audienced: map[string]map[string]connector.Audienced{}}
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
// what each audienced source shares with it. A user reads as their verified
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

// kinds are workspaceID's connectors whose kinds report their sources'
// permissions, by id, read once per call.
func (r *Reads) kinds(ctx context.Context, workspaceID string) (map[string]connector.Audienced, error) {
	r.mu.Lock()
	kinds, ok := r.audienced[workspaceID]
	r.mu.Unlock()
	if ok {
		return kinds, nil
	}
	connectors, err := r.db.ListConnectors(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("read connectors: %w", err)
	}
	kinds = Audienced(connectors)
	r.mu.Lock()
	r.audienced[workspaceID] = kinds
	r.mu.Unlock()
	return kinds, nil
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

// Told is targets as a grantee reads them who the API told visible are the
// items the audienced connectors of kinds share with it now
// (GetMyFoundryAccess's source_items, access.Grants.VisibleItems): what it
// reads of a load (access.Grants.Part) is what the grants Of built that told
// it read. Founder's worker reads what a grantee's call read with it.
//
//	sourceaccess.Told(kinds, targets, []access.ItemRef{{ConnectorID: "c1", Item: "C1"}}) // reads c1's #general (C1) alone
func Told(kinds map[string]connector.Audienced, targets []access.Target, visible []access.ItemRef) access.Grants {
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
	return access.NewGrants(targets).WithSharing(newSharing(kinds, reach))
}

// newSharing is reach, by audienced connector of kinds, each told its
// documents' items by its kind.
func newSharing(kinds map[string]connector.Audienced, reach map[string]access.SourceReach) access.Sharing {
	return access.NewSharing(reach, func(connectorID, stream, externalID string) (string, bool) {
		kind, ok := kinds[connectorID]
		if !ok {
			return "", false
		}
		return kind.ItemOfDocument(stream, externalID)
	})
}

// unknown is sharing in which every audienced connector of workspaceID
// reads as items unknown: its documents go to full readers only.
func (r *Reads) unknown(ctx context.Context, workspaceID string) (access.Sharing, error) {
	kinds, err := r.kinds(ctx, workspaceID)
	if err != nil {
		return access.Sharing{}, err
	}
	reach := make(map[string]access.SourceReach, len(kinds))
	for id := range kinds {
		reach[id] = access.SourceReach{}
	}
	return newSharing(kinds, reach), nil
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
	kinds, err := r.kinds(ctx, workspaceID)
	if err != nil {
		return access.Sharing{}, err
	}
	reach := make(map[string]access.SourceReach, len(kinds))
	for id := range kinds {
		reach[id] = access.SourceReach{}
	}
	if len(kinds) > 0 {
		stored, err := r.db.SourceSharingFor(ctx, workspaceID, reader)
		if err != nil {
			return access.Sharing{}, fmt.Errorf("read what sources share: %w", err)
		}
		for _, s := range stored {
			if _, ok := reach[s.ConnectorID]; ok {
				reach[s.ConnectorID] = access.ReachOf(s.ReadAt, s.ProblemAt, s.Visible, r.now)
			}
		}
	}
	sharing = newSharing(kinds, reach)
	r.mu.Lock()
	r.sharing[key] = sharing
	r.mu.Unlock()
	return sharing, nil
}
