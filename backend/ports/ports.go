// Package ports names what Foundry needs from the host that runs it: who is
// calling, the databases ontologies load into, the plans workspaces hold,
// the people in a workspace, the rate-limit boosts a graph job may take, the
// email it sends people, and the provider accounts connectors sign in with.
//
// A host implements each port. A hosted deployment backs them with its own
// identity, billing and database gateway; a standalone install backs them
// with its one owner and its one local HelixDB. A port documented as
// optional may be nil, and Foundry then behaves as the port's comment says.
//
// The ports Foundry's store already defines are not repeated here:
// store.Workspaces (the host's workspaces, read and locked inside the
// store's statements) and the store methods a host calls inside its own
// transactions (CancelWorkspaceFoundryWork, DeleteWorkspaceConnectors,
// BreakConnectorAccounts when a member leaves, ListConnectorAccounts before
// a workspace's connectors are deleted).
// Object storage is an aws.Config the host supplies; metrics register on
// Prometheus's default registerer.
package ports

import (
	"context"
	"errors"
	"slices"
	"time"

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
	// SourceGrantee is a user or service credential the front end's own
	// permissions did not admit, so Foundry decides from its source grants
	// (package access). It names exactly one of UserID and
	// ServiceCredentialID, a WorkspaceID, and no query scope.
	SourceGrantee
)

// Actor is who calls Foundry's API, as the front end that authenticated the
// caller resolved it. Foundry trusts it as given: the front end has already
// authorized the call. It never comes from a request body.
type Actor struct {
	Source Source
	// UserID is the user's id; empty for an event source and for a service
	// credential.
	UserID string
	// EventSourceTokenID is the send token's id; set only for SourceEventSource.
	EventSourceTokenID string
	// ServiceCredentialID is the workspace service credential (an agent's
	// key) the caller authenticated with; set only for SourceMCP and
	// SourceGrantee, and then UserID is empty.
	ServiceCredentialID string
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
	// Member is userID's membership of workspaceID, RoleNone when they are
	// not a member.
	Member(ctx context.Context, workspaceID, userID string) (Member, error)
}

// Role is a member's role in a workspace.
type Role uint8

const (
	// RoleNone is no member.
	RoleNone Role = iota
	RoleMember
	RoleAdmin
	RoleOwner
)

// Member is what Foundry's source grants need of a workspace member.
type Member struct {
	Role Role
	// RawAccess is that the member may write a project, or query a
	// project's databases, outside Foundry: grants can't restrict what they
	// read.
	RawAccess bool
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

// Recipient is where Foundry emails a user.
type Recipient struct {
	// Email is the user's address; never empty.
	Email string
	// FirstName is what a greeting calls them; empty when the host knows no
	// name, and the email then greets no one by name.
	FirstName string
}

// TopicFoundryTips is the subscription topic of Foundry's emails a user
// didn't ask for, such as the follow-up a day after the sample.
const TopicFoundryTips = "foundry-tips"

// Email is one message Foundry sends: its subject and the same body as
// plain text and as HTML.
type Email struct {
	Subject string
	Text    string
	HTML    string
	// Topic is a subscription topic (TopicFoundryTips) for an email the
	// recipient didn't ask for: the host adds a way to unsubscribe from it,
	// and doesn't deliver it to someone who has. Empty for an email about
	// the recipient's own work, which always goes.
	Topic string
}

// Mail sends Foundry's emails. Optional: where it is nil Foundry sends
// none, and the foundry-notify-worker doesn't run.
type Mail interface {
	// Recipient is where to email userID; store.ErrNotFound when the user
	// is gone or has no address.
	Recipient(ctx context.Context, userID string) (Recipient, error)
	// Send sends e to `to`. An error may be retried; the host has not
	// delivered the email.
	Send(ctx context.Context, to Recipient, e Email) error
}

// ProviderAccounts keeps the accounts users connect by signing in at a
// provider, owned by their workspace (hosted: WorkOS Pipes). Optional: where
// it is nil no kind connects by signing in, as in a local build.
type ProviderAccounts interface {
	// Kinds are the connector kinds userID can connect to the workspace by
	// signing in: none when they can't (hosted: no WorkOS organization, no
	// WorkOS membership of it, or Pipes switched off).
	Kinds(ctx context.Context, workspaceID, userID string) ([]string, error)
	// Authorize is the provider's page for the user to authorize a. With
	// a.AccountID set (a reauthorization), an account the broker no longer
	// has is an *AccountBroken{Reason: AccountAccountMissing}. Foundry then
	// authorizes again without AccountID, for a new account, and only once
	// that succeeds records the connector's account as missing: a broker
	// that can't tell a lost account from a switched-off integration may
	// answer so for either.
	Authorize(ctx context.Context, a AccountAuthorization) (string, error)
	// Finish saves the account the user authorized, having checked it is
	// the workspace's and was made by ref.UserID; otherwise it deletes it
	// and fails (ErrAccountNotThisWorkspace). ref.AccountID is set when the
	// sign-in reauthorized that account, the connector's own
	// (AccountAuthorization.AccountID): Finish never deletes it, and on a
	// mismatch returns the error and leaves it alone. When Finish refuses an
	// account the broker gave it, it returns that account (its ID and
	// UserID) with the error, whether or not its own delete of it
	// succeeded: Foundry purges it too, unless it is ref.AccountID.
	Finish(ctx context.Context, ref AccountRef, code, verifier string) (ProviderAccount, error)
	// Token is the account's current credential, asked for as ref.UserID.
	Token(ctx context.Context, ref AccountRef) (Credential, error)
	// Delete removes the account; one already gone is no error.
	Delete(ctx context.Context, ref AccountRef) error
}

// AccountAuthorization is what a user authorizes: a new account of Kind for
// the workspace, or, with AccountID, that account signed in again.
type AccountAuthorization struct {
	WorkspaceID, UserID, Kind string
	// Label names the account in the broker's own records.
	Label string
	// ReturnTo is the site's callback, Challenge the S256 PKCE challenge of
	// the verifier Finish is given.
	ReturnTo, Challenge string
	// AccountID, when set, reauthorizes that account. Foundry sets it only
	// when UserID is the member the account acts as, since an account
	// can't move to another member: anyone else signing a connector in
	// again makes a new one.
	AccountID string
}

// AccountRef names a workspace's account of a kind, used as UserID.
type AccountRef struct {
	WorkspaceID, UserID, Kind, AccountID string
}

// ProviderAccount is a saved account and the user it acts as: the one the
// broker says made it.
type ProviderAccount struct {
	ID, UserID string
}

// Credential is an account's token, held in memory for one use.
type Credential struct {
	Value     string
	ExpiresAt *time.Time
}

// The reasons an account needs signing in again.
const (
	AccountMemberLeft     = "member_left"
	AccountAccessRevoked  = "access_revoked"
	AccountExpired        = "expired"
	AccountAccountMissing = "account_missing"
)

// AccountBroken is Token's error when the account needs signing in again.
type AccountBroken struct {
	Reason string // one of the Account* reasons
}

func (e *AccountBroken) Error() string { return "the account needs signing in again: " + e.Reason }

// ErrAccountsUnavailable is every other failure to reach the accounts:
// switched off, unreachable, rate limited. It is retried, never a break.
var ErrAccountsUnavailable = errors.New("provider accounts are unavailable")

// ErrAccountCodeRejected is Finish's error when the broker refuses the
// callback's code: it lapsed, was used already, or its verifier doesn't
// match. Authorizing again gives the user a new one.
var ErrAccountCodeRejected = errors.New("the sign-in's code was refused")

// ErrAccountNotThisWorkspace is Finish's error when the account the code
// made isn't the workspace's, or wasn't made by the user finishing; Finish
// has deleted it.
var ErrAccountNotThisWorkspace = errors.New("the account isn't this workspace's")
