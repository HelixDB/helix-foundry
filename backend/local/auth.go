package local

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"log/slog"
	"strings"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/local/directory"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// MinSiteToken is the shortest site token an install accepts.
const MinSiteToken = 32

// Ontologies is what the front end reads of Foundry's store: the ontology
// a call names. store.ReadStore is it.
type Ontologies interface {
	GetFoundryOntology(ctx context.Context, workspaceID, id string) (*model.FoundryOntology, error)
}

// Authenticator is a standalone install's front end, as a gRPC interceptor
// (endpoint.ServerInterceptors). Its only caller is the Foundry site, which
// sends the site token as its bearer for the install's owner. For each call
// it:
//
//  1. lets the health service's methods through, as they are;
//  2. refuses a call without the site token: Unauthenticated, before any
//     handler runs;
//  3. refuses a method the install does not serve (methods): FailedPrecondition,
//     reason LOCAL_MODE_UNSUPPORTED;
//  4. checks what the method's request names, as a hosted front end's
//     authorization does: a workspace the owner has (PermissionDenied
//     otherwise), and a database or ontology of it (NotFound otherwise);
//  5. runs the handler as the owner: a ports.Actor of SourceUser, with every
//     project of the request's workspace in its query scopes.
type Authenticator struct {
	digest     [sha256.Size]byte
	directory  Directory
	ontologies Ontologies
	logger     *slog.Logger
}

// NewAuthenticator accepts exactly token, at least MinSiteToken bytes, as
// the owner of directory's workspaces.
func NewAuthenticator(token string, directory Directory, ontologies Ontologies, logger *slog.Logger) (*Authenticator, error) {
	if len(token) < MinSiteToken {
		return nil, errors.New("local: the site token must be at least 32 bytes")
	}
	return &Authenticator{digest: sha256.Sum256([]byte(token)), directory: directory, ontologies: ontologies, logger: logger}, nil
}

func (a *Authenticator) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := a.gate(ctx, info.FullMethod, req)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// Stream gates a stream as a unary call with no request: the install serves
// no streaming method but the health service's Watch.
func (a *Authenticator) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := a.gate(ss.Context(), info.FullMethod, nil)
		if err != nil {
			return err
		}
		return handler(srv, &stream{ServerStream: ss, ctx: ctx})
	}
}

type stream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *stream) Context() context.Context { return s.ctx }

func (a *Authenticator) gate(ctx context.Context, method string, req any) (context.Context, error) {
	if healthMethods[method] {
		return ctx, nil
	}
	if !a.verify(ctx) {
		a.logger.Warn("local: the call has no site token", "method", method)
		return nil, rejected("invalid token")
	}
	owner, err := a.directory.Owner(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil, rejected("identity no longer exists")
	}
	if err != nil {
		a.logger.Error("local: read the owner", "method", method, "err", err)
		return nil, status.Error(codes.Internal, "auth hydrate failed")
	}
	check, served := methods[method]
	if !served {
		return nil, unsupported(method)
	}
	workspaceID, err := a.authorize(ctx, check, req)
	if err != nil {
		return nil, err
	}
	return ports.WithActor(ctx, ports.Actor{
		Source: ports.SourceUser, UserID: owner.ID, WorkspaceID: workspaceID,
		QueryRead: ports.AllProjects(), QueryWrite: ports.AllProjects(),
	}), nil
}

// verify compares the bearer's digest with the token's in constant time, so
// a wrong token reveals neither the right one's length nor a shared prefix.
func (a *Authenticator) verify(ctx context.Context) bool {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	const prefix = "bearer "
	if len(values) == 0 || len(values[0]) < len(prefix) || !strings.EqualFold(values[0][:len(prefix)], prefix) {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(values[0][len(prefix):])))
	return subtle.ConstantTimeCompare(got[:], a.digest[:]) == 1
}

// authorize checks what req names for check, and returns its workspace.
func (a *Authenticator) authorize(ctx context.Context, check check, req any) (string, error) {
	if check == checkHandler {
		return "", nil
	}
	r, ok := req.(interface{ GetWorkspaceId() string })
	if !ok {
		return "", status.Errorf(codes.Internal, "authz: %T has no workspace", req)
	}
	ws := r.GetWorkspaceId()
	switch check {
	case checkWorkspace:
		if ws == "" {
			return "", status.Error(codes.InvalidArgument, "workspace_id is required")
		}
	case checkTenant:
		t, ok := req.(interface{ GetTenantId() string })
		if !ok {
			return "", status.Errorf(codes.Internal, "authz: %T has no database", req)
		}
		if ws == "" || t.GetTenantId() == "" {
			return "", status.Error(codes.InvalidArgument, "workspace_id and tenant_id are required")
		}
		tenant, err := a.directory.Tenant(ctx, t.GetTenantId())
		if errors.Is(err, store.ErrNotFound) || err == nil && (tenant.Deleted() || tenant.WorkspaceID != ws) {
			return "", status.Error(codes.NotFound, "resource not found")
		}
		if err != nil {
			return "", status.Errorf(codes.Internal, "authz: get tenant: %v", err)
		}
	case checkOntology:
		o, ok := req.(interface{ GetOntologyId() string })
		if !ok {
			return "", status.Errorf(codes.Internal, "authz: %T has no ontology", req)
		}
		if ws == "" || o.GetOntologyId() == "" {
			return "", status.Error(codes.InvalidArgument, "workspace_id and ontology_id are required")
		}
		ontology, err := a.ontologies.GetFoundryOntology(ctx, ws, o.GetOntologyId())
		if errors.Is(err, store.ErrNotFound) || err == nil && ontology.DeletedAt != nil {
			return "", status.Error(codes.NotFound, "resource not found")
		}
		if err != nil {
			return "", status.Errorf(codes.Internal, "authz: get ontology: %v", err)
		}
	}
	w, err := a.directory.Workspace(ctx, ws)
	if errors.Is(err, store.ErrNotFound) || err == nil && (w.DeletedAt != nil || w.Status == directory.StatusDeleted) {
		return "", status.Error(codes.PermissionDenied, "workspace access required")
	}
	if err != nil {
		return "", status.Errorf(codes.Internal, "authz: get workspace: %v", err)
	}
	return ws, nil
}

// PreDispatchAuthenticationReason tags an Unauthenticated call refused
// before its handler ran, so a client can tell a rejected token from a
// handler's error.
const PreDispatchAuthenticationReason = "SESSION_REJECTED_PRE_DISPATCH"

func rejected(message string) error {
	return withReason(status.New(codes.Unauthenticated, message), PreDispatchAuthenticationReason, "auth.helix-db.com")
}

// UnsupportedReason is the reason of a call to a method a standalone
// install does not serve.
const UnsupportedReason = "LOCAL_MODE_UNSUPPORTED"

func unsupported(method string) error {
	return withReason(status.Newf(codes.FailedPrecondition, "%s is not available in local mode", method), UnsupportedReason, "foundry.helix-db.com")
}

func withReason(st *status.Status, reason, domain string) error {
	detailed, err := st.WithDetails(&errdetails.ErrorInfo{Reason: reason, Domain: domain})
	if err != nil {
		return st.Err()
	}
	return detailed.Err()
}
