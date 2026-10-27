package local

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	directoryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/directory/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
)

const token = "site-token-0123456789abcdefghijklmnop"

// request stands in for a FoundryService request: its workspace, and the
// database or ontology it names.
type request struct{ workspace, tenant, ontology string }

func (r request) GetWorkspaceId() string { return r.workspace }
func (r request) GetTenantId() string    { return r.tenant }
func (r request) GetOntologyId() string  { return r.ontology }

func newAuth(t *testing.T, dir *fakeDirectory) *Authenticator {
	t.Helper()
	gone := time.Now()
	a, err := NewAuthenticator(token, dir, fakeOntologies{
		{"ws", "o"}:    {ID: "o", WorkspaceID: "ws"},
		{"ws", "dead"}: {ID: "dead", WorkspaceID: "ws", DeletedAt: &gone},
		{"gone", "o"}:  {ID: "o", WorkspaceID: "gone"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func bearer(value string) context.Context {
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", value))
}

// call runs method through the interceptor, and returns the actor its
// handler ran as.
func call(a *Authenticator, ctx context.Context, method string, req any) (ports.Actor, error) {
	var actor ports.Actor
	_, err := a.Unary()(ctx, req, &grpc.UnaryServerInfo{FullMethod: method}, func(ctx context.Context, _ any) (any, error) {
		got, ok := ports.ActorFrom(ctx)
		if !ok {
			return nil, errors.New("no actor")
		}
		actor = got
		return nil, nil
	})
	return actor, err
}

func reason(err error) string {
	for _, d := range status.Convert(err).Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.GetReason() + "@" + info.GetDomain()
		}
	}
	return ""
}

func TestNewAuthenticatorRefusesAShortToken(t *testing.T) {
	if _, err := NewAuthenticator(strings.Repeat("x", MinSiteToken-1), &fakeDirectory{}, fakeOntologies{}, slog.Default()); err == nil {
		t.Fatal("accepted a 31-byte token")
	}
}

func TestAuthenticatorToken(t *testing.T) {
	a := newAuth(t, testDirectory())
	method := foundry("ListConnectors")

	// The health service needs no token.
	if _, err := a.Unary()(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"},
		func(context.Context, any) (any, error) { return nil, nil }); err != nil {
		t.Fatalf("health: %v", err)
	}

	for name, ctx := range map[string]context.Context{
		"no metadata":     context.Background(),
		"no bearer":       bearer(""),
		"another scheme":  bearer("Basic " + token),
		"the wrong token": bearer("Bearer " + token + "x"),
		"a prefix of it":  bearer("Bearer " + token[:MinSiteToken]),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := call(a, ctx, method, request{workspace: "ws"})
			if status.Code(err) != codes.Unauthenticated || reason(err) != PreDispatchAuthenticationReason+"@auth.helix-db.com" {
				t.Fatalf("err = %v (%s)", err, reason(err))
			}
		})
	}

	// The scheme is any case, as the hosted front end reads it.
	actor, err := call(a, bearer("bearer "+token), method, request{workspace: "ws"})
	if err != nil {
		t.Fatal(err)
	}
	want := ports.Actor{Source: ports.SourceUser, UserID: "owner", WorkspaceID: "ws", QueryRead: ports.AllProjects(), QueryWrite: ports.AllProjects()}
	if actor.Source != want.Source || actor.UserID != want.UserID || actor.WorkspaceID != want.WorkspaceID ||
		!actor.QueryRead.All() || !actor.QueryWrite.All() {
		t.Fatalf("actor = %+v", actor)
	}
	if read, write := actor.Scope("ws"); !read.All() || !write.All() {
		t.Fatalf("scope = %v %v", read, write)
	}
}

func TestAuthenticatorOwner(t *testing.T) {
	ctx := bearer("Bearer " + token)
	// Without the owner's row the token names nobody.
	dir := testDirectory()
	dir.owner = nil
	_, err := call(newAuth(t, dir), ctx, foundry("ListConnectors"), request{workspace: "ws"})
	if status.Code(err) != codes.Unauthenticated || !strings.Contains(err.Error(), "identity no longer exists") {
		t.Fatalf("no owner: %v", err)
	}
	dir = testDirectory()
	dir.err = errors.New("db down")
	_, err = call(newAuth(t, dir), ctx, foundry("ListConnectors"), request{workspace: "ws"})
	if status.Code(err) != codes.Internal {
		t.Fatalf("db down: %v", err)
	}
}

func TestAuthenticatorRefusesWhatTheInstallDoesNotServe(t *testing.T) {
	a := newAuth(t, testDirectory())
	for _, method := range []string{
		foundry("CreateFoundryProCheckout"),
		foundry("UpdateFoundryEnrichment"),
		foundry("CreateFoundryEventSourceToken"),
		"/helix.foundry.v1.FoundryEventsService/SendFoundryEvents",
		"/helix.wfe.v1.WfeService/ListConnectors",
	} {
		_, err := call(a, bearer("Bearer "+token), method, request{workspace: "ws"})
		if status.Code(err) != codes.FailedPrecondition || reason(err) != UnsupportedReason+"@foundry.helix-db.com" ||
			!strings.Contains(err.Error(), method+" is not available in local mode") {
			t.Fatalf("%s: %v", method, err)
		}
		// Without the token it is not signed in, which it is told first.
		if _, err := call(a, context.Background(), method, request{}); status.Code(err) != codes.Unauthenticated {
			t.Fatalf("%s without a token: %v", method, err)
		}
	}
}

func TestAuthenticatorChecksTheRequest(t *testing.T) {
	a := newAuth(t, testDirectory())
	ctx := bearer("Bearer " + token)
	for name, tc := range map[string]struct {
		method string
		req    any
		code   codes.Code
	}{
		"a workspace":                {foundry("ListConnectors"), request{workspace: "ws"}, codes.OK},
		"a workspace being deleted":  {foundry("ListConnectors"), request{workspace: "deleting"}, codes.OK},
		"no workspace":               {foundry("ListConnectors"), request{}, codes.InvalidArgument},
		"an unknown workspace":       {foundry("ListConnectors"), request{workspace: "nope"}, codes.PermissionDenied},
		"a deleted workspace":        {foundry("ListConnectors"), request{workspace: "gone"}, codes.PermissionDenied},
		"a request with none":        {foundry("ListConnectors"), &directoryv1.WhoamiRequest{}, codes.Internal},
		"a directory list":           {directoryv1.DirectoryService_ListProjects_FullMethodName, &directoryv1.ListProjectsRequest{WorkspaceId: "ws"}, codes.OK},
		"a directory list elsewhere": {directoryv1.DirectoryService_ListProjects_FullMethodName, &directoryv1.ListProjectsRequest{WorkspaceId: "nope"}, codes.PermissionDenied},
		"the handler's to check":     {directoryv1.DirectoryService_GetWorkspace_FullMethodName, &directoryv1.GetWorkspaceRequest{Id: "nope"}, codes.OK},

		"a database of the workspace": {foundry("CreateFoundryOntology"), request{workspace: "ws", tenant: "t"}, codes.OK},
		"no database":                 {foundry("CreateFoundryOntology"), request{workspace: "ws"}, codes.InvalidArgument},
		"an unknown database":         {foundry("CreateFoundryOntology"), request{workspace: "ws", tenant: "nope"}, codes.NotFound},
		"a deleted database":          {foundry("CreateFoundryOntology"), request{workspace: "ws", tenant: "deleted"}, codes.NotFound},
		"a retired database":          {foundry("CreateFoundryOntology"), request{workspace: "ws", tenant: "retired"}, codes.NotFound},
		"another workspace's":         {foundry("CreateFoundryOntology"), request{workspace: "deleting", tenant: "t"}, codes.NotFound},
		"a request with no database":  {foundry("CreateFoundryOntology"), &directoryv1.ListProjectsRequest{WorkspaceId: "ws"}, codes.Internal},

		"an ontology of the workspace":  {foundry("ReadFoundryGraph"), request{workspace: "ws", ontology: "o"}, codes.OK},
		"no ontology":                   {foundry("ReadFoundryGraph"), request{workspace: "ws"}, codes.InvalidArgument},
		"an unknown ontology":           {foundry("ReadFoundryGraph"), request{workspace: "ws", ontology: "nope"}, codes.NotFound},
		"a deleted ontology":            {foundry("ReadFoundryGraph"), request{workspace: "ws", ontology: "dead"}, codes.NotFound},
		"a deleted workspace's":         {foundry("ReadFoundryGraph"), request{workspace: "gone", ontology: "o"}, codes.PermissionDenied},
		"a request with no ontology":    {foundry("ReadFoundryGraph"), &directoryv1.ListProjectsRequest{WorkspaceId: "ws"}, codes.Internal},
		"no workspace for the ontology": {foundry("ReadFoundryGraph"), request{ontology: "o"}, codes.InvalidArgument},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := call(a, ctx, tc.method, tc.req); status.Code(err) != tc.code {
				t.Fatalf("err = %v, want %s", err, tc.code)
			}
		})
	}
}

// A stream is gated as a call with no request: the health service's Watch
// passes, and anything else needs the token.
func TestAuthenticatorStream(t *testing.T) {
	a := newAuth(t, testDirectory())
	run := func(ctx context.Context, method string) error {
		return a.Stream()(nil, &fakeStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: method},
			func(any, grpc.ServerStream) error { return nil })
	}
	if err := run(context.Background(), "/grpc.health.v1.Health/Watch"); err != nil {
		t.Fatalf("watch: %v", err)
	}
	if err := run(context.Background(), foundry("ListConnectors")); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no token: %v", err)
	}
	if err := run(bearer("Bearer "+token), foundry("ListConnectors")); status.Code(err) != codes.Internal {
		t.Fatalf("no request: %v", err)
	}
}

type fakeStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *fakeStream) Context() context.Context { return s.ctx }

// Every method the install serves is a directory or FoundryService one,
// checked as its request can be.
func TestMethods(t *testing.T) {
	for method, c := range methods {
		directory := strings.HasPrefix(method, "/helix.foundry.directory.v1.DirectoryService/")
		if !directory && !strings.HasPrefix(method, "/helix.foundry.v1.FoundryService/") {
			t.Errorf("%s is neither the directory's nor Foundry's", method)
		}
		if !directory && c == checkHandler {
			t.Errorf("%s checks nothing", method)
		}
	}
}
