// Package ports names what Foundry needs from the host that runs it: who is
// calling, the databases ontologies load into, the plans workspaces hold,
// the people in a workspace, and the rate-limit boosts a graph job may take.
//
// A host implements each port. A hosted deployment backs them with its own
// identity, billing and database gateway; a standalone install backs them
// with its one owner and its one local HelixDB. A port documented as
// optional may be nil, and Foundry then behaves as the port's comment says.
//
// The ports Foundry's store already defines are not repeated here:
// store.Workspaces (the host's workspaces, read and locked inside the
// store's statements) and the store methods a host calls inside its own
// transactions (CancelWorkspaceFoundryWork, DeleteWorkspaceConnectors).
// Object storage is an aws.Config the host supplies; metrics register on
// Prometheus's default registerer.
package ports

import (
	"context"
	"slices"

	"github.com/helixdb/helix-foundry/backend/plan"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// Source is the kind of caller an Actor is.
type Source int

const (
	// SourceUnspecified is no caller; Foundry refuses it.
	SourceUnspecified Source = iota
	// SourceUser is a person signed in to the Foundry site (or an engineer
	// acting for one).
	SourceUser
	// SourceMCP is an agent calling through the MCP server, for a user.
	SourceMCP
	// SourceEventSource is an event source's sender, holding its send token.
	SourceEventSource
)

// Actor is who calls Foundry's API, as the front end that authenticated the
// caller resolved it. Foundry trusts it as given: the front end has already
// authorized the call. It never comes from a request body.
type Actor struct {
	Source Source
	// UserID is the user's id; empty for an event source.
	UserID string
	// EventSourceTokenID is the send token's id; set only for SourceEventSource.
	EventSourceTokenID string
	// WorkspaceID is the workspace QueryRead and QueryWrite are of: the
	// request's. A scope of another workspace grants nothing.
	WorkspaceID string
	// QueryRead and QueryWrite are the projects of WorkspaceID whose
	// databases the caller may read and write with queries.
	QueryRead, QueryWrite ProjectScope
}

// Scope returns the caller's read and write scopes for workspaceID: none
// unless they are of it.
func (a Actor) Scope(workspaceID string) (read, write ProjectScope) {
	if workspaceID == "" || a.WorkspaceID != workspaceID {
		return NoProjects(), NoProjects()
	}
	return a.QueryRead, a.QueryWrite
}

type actorKey struct{}

// WithActor returns ctx carrying a.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

// ActorFrom returns the Actor ctx carries, if any.
func ActorFrom(ctx context.Context) (Actor, bool) {
	a, ok := ctx.Value(actorKey{}).(Actor)
	return a, ok
}

type scopeKind uint8

const (
	scopeNone scopeKind = iota
	scopeAll
	scopeSelected
)

// ProjectScope is none, every or some of a workspace's projects. The zero
// value is none.
type ProjectScope struct {
	kind scopeKind
	ids  []string
}

// NoProjects grants no project.
func NoProjects() ProjectScope { return ProjectScope{} }

// AllProjects grants every project of the workspace.
func AllProjects() ProjectScope { return ProjectScope{kind: scopeAll} }

// SelectedProjects grants the projects ids names; none when ids is empty.
func SelectedProjects(ids []string) ProjectScope {
	if len(ids) == 0 {
		return ProjectScope{}
	}
	return ProjectScope{kind: scopeSelected, ids: slices.Clone(ids)}
}

// Allows reports whether the scope includes projectID.
func (s ProjectScope) Allows(projectID string) bool {
	switch s.kind {
	case scopeAll:
		return true
	case scopeSelected:
		return slices.Contains(s.ids, projectID)
	default:
		return false
	}
}

// All reports whether the scope grants every project.
func (s ProjectScope) All() bool { return s.kind == scopeAll }

// Projects returns the projects a selected scope names; nil for none or all.
func (s ProjectScope) Projects() []string { return slices.Clone(s.ids) }

// Database is a HelixDB database an ontology loads into, as its host knows it.
type Database struct {
	// TenantID is the database's id, which ontologies store as tenant_id.
	TenantID    string
	WorkspaceID string
	ProjectID   string
	Name        string
	// Deleted is set once the database's deletion was recorded.
	Deleted bool
	// Retired is set once its lifecycle reads deleted; it may precede Deleted.
	Retired bool
}

// Databases resolves the databases ontologies are bound to.
type Databases interface {
	// Get returns a database as it is now, deleted or not;
	// store.ErrNotFound when there is none.
	Get(ctx context.Context, tenantID string) (Database, error)
	// Live returns a database a new ontology may bind to. Its errors are gRPC
	// statuses: NotFound when there is none or it is deleted,
	// FailedPrecondition when it can take no new work (its project is
	// deleted, or it is paused).
	Live(ctx context.Context, tenantID string) (Database, error)
	// Endpoint returns the URL Foundry sends a live database's queries to
	// (path appended). Its errors are gRPC statuses: NotFound for a database
	// that is gone, FailedPrecondition while its queries are paused or its
	// host is not serving, Unavailable when it can't be resolved safely.
	Endpoint(ctx context.Context, tenantID, path string) (string, error)
}

// GraphReader runs read queries for Foundry's API on a workspace's database.
type GraphReader interface {
	// ReadQuery runs query, a HelixDB read request, on the database as the
	// Actor in ctx, with the host's own checks on the caller, the database
	// and the answer's size. It returns the database's HTTP status and body;
	// an error is a gRPC status.
	ReadQuery(ctx context.Context, tenantID string, query []byte) (status int, body []byte, err error)
}

// People answers what Foundry's API shows and decides about a workspace's
// members.
type People interface {
	// MayManageAI reports whether userID may change workspaceID's model
	// provider, and so may see its keys' hints. Any doubt is false.
	MayManageAI(ctx context.Context, workspaceID, userID string) bool
	// DisplayName is the name Foundry shows for a user: their display name,
	// else their email; store.ErrNotFound when there is no such user.
	DisplayName(ctx context.Context, userID string) (string, error)
}

// PlanReader reads the Foundry plan a workspace holds now. Optional: where
// it is nil no plan is sold, and every workspace is on the catalog's first
// plan (plan.Catalog.Free).
type PlanReader interface {
	Status(ctx context.Context, workspaceID string) (plan.Status, error)
}

// Plans sells Foundry's plans. Optional, as PlanReader; where it is nil the
// API refuses checkout, cancel and resume.
type Plans interface {
	PlanReader
	// RequireKind returns a *plan.PlanRequiredError unless the workspace's
	// plan includes the connector kind. It fails closed, unless the free
	// plan includes the kind.
	RequireKind(ctx context.Context, workspaceID, kind string) error
	// Checkout buys the plan with code; clientToken keys the charge, so a
	// retried click is not a second purchase. Errors: plan.ErrPlanNotSold,
	// plan.ErrNoCustomer, plan.ErrPromotionCode, plan.ErrDeclined and
	// plan.ErrUnverified.
	Checkout(ctx context.Context, workspaceID, code, promotionCode, clientToken string) (plan.CheckoutResult, error)
	// Cancel ends the held paid plan at its period's end; plan.ErrNotPaid
	// when none is held.
	Cancel(ctx context.Context, workspaceID string) (plan.Status, error)
	// Resume keeps a held paid plan set to end; plan.ErrNotPaid when none is held.
	Resume(ctx context.Context, workspaceID string) (plan.Status, error)
}

// BoostChange is the host's gateway configuration revision that carries a
// boost, on the cluster serving the database.
type BoostChange struct {
	ClusterID string
	Revision  uint64
}

// Boosts lifts a database's query rate limit while one of Foundry's attempts
// holds it (package boost). Optional: required only when
// foundry.graph.load_rate_limit is set.
type Boosts interface {
	// AcquireLoadBoost writes the attempt's lease on its database's boost, in
	// one transaction with the attempt's own row (store.LockBoostHolder).
	// store.ErrConflict means an operator's override holds the database;
	// store.ErrNotFound that the attempt no longer holds its work or its
	// database.
	AcquireLoadBoost(ctx context.Context, workerID string, boost model.FoundryLoadBoost) (BoostChange, error)
	// ReleaseLoadBoost removes the attempt's lease, leaving other holders';
	// store.ErrNotFound when it holds none.
	ReleaseLoadBoost(ctx context.Context, boost model.FoundryLoadBoost) error
	// SyncedRevision is the revision the cluster's gateways have applied.
	SyncedRevision(ctx context.Context, clusterID string) (uint64, error)
	// SweepLoadBoosts removes up to limit lapsed leases, and says how many.
	SweepLoadBoosts(ctx context.Context, limit int) (int, error)
}
