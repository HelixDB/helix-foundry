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
	SourceSharingFor(ctx context.Context, workspaceID, email string) ([]model.FoundrySourceSharing, error)
}

// Reads builds grantees' grants for one call, reading each reader's email
// and what the sources share with it once, so every check the call makes
// sees one moment of them. Make one per call with New.
type Reads struct {
	db Store
	// people tells a user's verified email; nil tells none, and every user
	// is then no one in any source.
	people ports.People
	now    time.Time

	mu      sync.Mutex
	emails  map[[2]string]string
	sharing map[[2]string]access.Sharing
}

// New reads through db and people as of now.
func New(db Store, people ports.People, now time.Time) *Reads {
	return &Reads{db: db, people: people, now: now, emails: map[[2]string]string{}, sharing: map[[2]string]access.Sharing{}}
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
// email; a service credential (an agent's key) or a team, whose members each
// read as themselves, is no one in any source, and reads only what one
// shares with everyone.
func (r *Reads) Of(ctx context.Context, workspaceID string, reader access.Principal, targets []access.Target) (access.Grants, error) {
	grants := access.NewGrants(targets)
	if grants.Empty() {
		return grants, nil
	}
	email, err := r.email(ctx, workspaceID, reader)
	if err != nil {
		return access.Grants{}, err
	}
	sharing, err := r.sharingFor(ctx, workspaceID, email)
	if err != nil {
		return access.Grants{}, err
	}
	return grants.WithSharing(sharing), nil
}

// email is reader's verified email in workspaceID, empty for none.
func (r *Reads) email(ctx context.Context, workspaceID string, reader access.Principal) (string, error) {
	if reader.Kind != access.PrincipalUser || r.people == nil {
		return "", nil
	}
	key := [2]string{workspaceID, reader.ID}
	r.mu.Lock()
	email, ok := r.emails[key]
	r.mu.Unlock()
	if ok {
		return email, nil
	}
	m, err := r.people.Member(ctx, workspaceID, reader.ID)
	if err != nil {
		return "", fmt.Errorf("read the member's email: %w", err)
	}
	r.mu.Lock()
	r.emails[key] = m.Email
	r.mu.Unlock()
	return m.Email, nil
}

// sharingFor is what the sources of workspaceID's audienced connectors share
// with email: a connector whose audiences were never stored, or were read
// longer ago than access.MaxAudienceAge, reads as items unknown.
func (r *Reads) sharingFor(ctx context.Context, workspaceID, email string) (access.Sharing, error) {
	key := [2]string{workspaceID, email}
	r.mu.Lock()
	sharing, ok := r.sharing[key]
	r.mu.Unlock()
	if ok {
		return sharing, nil
	}
	connectors, err := r.db.ListConnectors(ctx, workspaceID)
	if err != nil {
		return access.Sharing{}, fmt.Errorf("read connectors: %w", err)
	}
	kinds := map[string]connector.Audienced{}
	reach := map[string]access.SourceReach{}
	for _, c := range connectors {
		if kind, ok := connector.AudiencedKind(c.Kind); ok {
			kinds[c.ID], reach[c.ID] = kind, access.SourceReach{}
		}
	}
	if len(kinds) > 0 {
		stored, err := r.db.SourceSharingFor(ctx, workspaceID, email)
		if err != nil {
			return access.Sharing{}, fmt.Errorf("read what sources share: %w", err)
		}
		for _, s := range stored {
			if _, ok := reach[s.ConnectorID]; ok {
				reach[s.ConnectorID] = access.ReachOf(s.ReadAt, s.Visible, s.Items, r.now)
			}
		}
	}
	sharing = access.NewSharing(reach, func(connectorID, stream, externalID string) (string, bool) {
		kind, ok := kinds[connectorID]
		if !ok {
			return "", false
		}
		return kind.ItemOfDocument(stream, externalID)
	})
	r.mu.Lock()
	r.sharing[key] = sharing
	r.mu.Unlock()
	return sharing, nil
}
