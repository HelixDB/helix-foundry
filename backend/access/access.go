// Package access says what a caller admitted through Foundry's source grants
// may do. Owners and admins grant a principal (a user, a team, or a service
// credential: an agent's key) the workspace's sources: a connector, one of
// its tables, a document set, or People enrichment. A grantee may read an
// ontology's current load only while every source that load holds is
// granted to it, and may change nothing.
//
// Everyone else is the host's caller, as before grants existed: the front
// end that authenticated them authorized the call, and Foundry trusts its
// Actor. A workspace that grants nothing behaves as it did without them. A
// member given grants, or on a team given any, is restricted to them, and
// taking them away never lifts that (see Service.SetFoundryAccessGrants).
//
// The package holds the rules: who a call is for (Caller), and what each of
// Foundry's methods lets a grantee do (Rules). Package api enforces them on
// every call, and the store answers which loads a grantee covers.
package access

import (
	"context"
	"fmt"
	"slices"
)

// PrincipalKind is the kind of principal a grant names.
type PrincipalKind uint8

const (
	// PrincipalUser is a workspace member.
	PrincipalUser PrincipalKind = iota + 1
	// PrincipalServiceCredential is a workspace service credential: an
	// agent's key.
	PrincipalServiceCredential
	// PrincipalTeam is a workspace team; its members hold its grants.
	PrincipalTeam
)

var principalKindNames = map[PrincipalKind]string{
	PrincipalUser:              "user",
	PrincipalServiceCredential: "service_credential",
	PrincipalTeam:              "team",
}

// String is the kind as the store keeps it.
func (k PrincipalKind) String() string { return principalKindNames[k] }

// ParsePrincipalKind reads a kind as the store keeps it.
//
//	k, err := access.ParsePrincipalKind("team") // access.PrincipalTeam, nil
func ParsePrincipalKind(s string) (PrincipalKind, error) {
	for k, name := range principalKindNames {
		if name == s {
			return k, nil
		}
	}
	return 0, fmt.Errorf("access: unknown principal kind %q", s)
}

// Principal is who a grant is for.
type Principal struct {
	Kind PrincipalKind
	ID   string
}

// Caller is who a call is for: Host or Grantee.
type Caller interface{ caller() }

// Host is a caller the front end authorized: Foundry serves it as it did
// before grants, trusting its Actor.
type Host struct{}

// Grantee is a caller admitted only through source grants in one workspace.
type Grantee struct {
	WorkspaceID string
	// Principal is the user or service credential calling.
	Principal Principal
	// Teams are the workspace teams the user is a member of now; none for a
	// service credential.
	Teams []string
}

func (Host) caller()    {}
func (Grantee) caller() {}

// Principals are those whose grants the grantee holds: itself, then its
// teams.
func (g Grantee) Principals() []Principal {
	out := []Principal{g.Principal}
	for _, team := range g.Teams {
		out = append(out, Principal{Kind: PrincipalTeam, ID: team})
	}
	return slices.Clip(out)
}

type callerKey struct{}

// WithCaller returns ctx carrying c.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom is the Caller ctx carries: Host when it carries none, as a call
// that never passed the access check (a host calling the service directly)
// is the host's.
func CallerFrom(ctx context.Context) Caller {
	if c, ok := ctx.Value(callerKey{}).(Caller); ok {
		return c
	}
	return Host{}
}

// GranteeFrom is the Grantee ctx carries, if its caller is one.
func GranteeFrom(ctx context.Context) (Grantee, bool) {
	g, ok := CallerFrom(ctx).(Grantee)
	return g, ok
}
