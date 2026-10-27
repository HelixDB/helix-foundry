package api

import (
	"context"
	"slices"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
)

// arrive is what a server behind ActorInterceptor sees of a call to method
// made on ctx: the Actor its handler gets, or the interceptor's refusal.
func arrive(ctx context.Context, method string) (ports.Actor, bool, error) {
	md, _ := metadata.FromOutgoingContext(ctx)
	var got ports.Actor
	var ok bool
	_, err := ActorInterceptor()(metadata.NewIncomingContext(context.Background(), md), nil, &grpc.UnaryServerInfo{FullMethod: method},
		func(ctx context.Context, _ any) (any, error) {
			got, ok = ports.ActorFrom(ctx)
			return nil, nil
		})
	return got, ok, err
}

// An Actor attached by a front end reaches the handler as it was sent:
// source, user or send token, workspace, and both query scopes.
func TestActorRoundTrip(t *testing.T) {
	for name, a := range map[string]ports.Actor{
		"a user":       {Source: ports.SourceUser, UserID: "user-1", WorkspaceID: "ws-1", QueryRead: ports.AllProjects(), QueryWrite: ports.SelectedProjects([]string{"p-1", "p-2"})},
		"an MCP agent": {Source: ports.SourceMCP, WorkspaceID: "ws-1", QueryRead: ports.SelectedProjects([]string{"p-1"})},
		"a send token": {Source: ports.SourceEventSource, EventSourceTokenID: "tok-1", WorkspaceID: "ws-1"},
	} {
		got, ok, err := arrive(AttachActor(context.Background(), a), foundryv1.FoundryService_GetFoundryAI_FullMethodName)
		if err != nil || !ok {
			t.Fatalf("%s: %v", name, err)
		}
		gotRead, gotWrite := got.Scope("ws-1")
		wantRead, wantWrite := a.Scope("ws-1")
		if got.Source != a.Source || got.UserID != a.UserID || got.EventSourceTokenID != a.EventSourceTokenID || got.WorkspaceID != a.WorkspaceID ||
			gotRead.All() != wantRead.All() || !slices.Equal(gotRead.Projects(), wantRead.Projects()) ||
			gotWrite.All() != wantWrite.All() || !slices.Equal(gotWrite.Projects(), wantWrite.Projects()) {
			t.Fatalf("%s: sent %+v, got %+v", name, a, got)
		}
	}
}

// AttachActor replaces the outgoing metadata ctx had: nothing the caller
// set, a forged Actor least of all, goes with the call.
func TestAttachActorDropsOtherMetadata(t *testing.T) {
	forged, err := proto.Marshal(&foundryv1.Actor{Source: foundryv1.Source_SOURCE_USER, UserId: "someone-else"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := metadata.AppendToOutgoingContext(context.Background(), actorKey, string(forged), "authorization", "Bearer x")
	ctx = AttachActor(ctx, ports.Actor{Source: ports.SourceUser, UserID: "user-1"})
	md, _ := metadata.FromOutgoingContext(ctx)
	if len(md) != 1 || len(md.Get(actorKey)) != 1 {
		t.Fatalf("outgoing metadata = %v", md)
	}
	if got, _, err := arrive(ctx, foundryv1.FoundryService_GetFoundryAI_FullMethodName); err != nil || got.UserID != "user-1" {
		t.Fatalf("arrived as %+v, %v", got, err)
	}
}

// A call to Foundry's services needs exactly one well-formed Actor of a known
// source; a call to another service passes untouched, with no Actor.
func TestActorInterceptorRefusals(t *testing.T) {
	foundryMethods := []string{foundryv1.FoundryService_ListConnectors_FullMethodName, foundryv1.FoundryEventsService_SendFoundryEvents_FullMethodName}
	valid, err := proto.Marshal(&foundryv1.Actor{Source: foundryv1.Source_SOURCE_USER, UserId: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := proto.Marshal(&foundryv1.Actor{Source: foundryv1.Source(42), UserId: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	for name, md := range map[string]metadata.MD{
		"no actor":        {},
		"two actors":      metadata.Pairs(actorKey, string(valid), actorKey, string(valid)),
		"a malformed one": metadata.Pairs(actorKey, "\xff\xff"),
		"no source":       metadata.Pairs(actorKey, ""),
		"an unknown one":  metadata.Pairs(actorKey, string(unknown)),
	} {
		for _, method := range foundryMethods {
			ctx := metadata.NewOutgoingContext(context.Background(), md)
			if _, _, err := arrive(ctx, method); status.Code(err) != codes.Unauthenticated {
				t.Fatalf("%s to %s: %v", name, method, err)
			}
		}
	}
	if _, ok, err := arrive(context.Background(), "/helix.backend.v1.BackendService/GetWorkspace"); err != nil || ok {
		t.Fatalf("another service's call: actor %v, %v", ok, err)
	}
}

// A scope that is malformed on the wire grants nothing: all with projects
// named, or a mode this build doesn't know.
func TestActorScopeFromProto(t *testing.T) {
	for name, s := range map[string]*foundryv1.ProjectScope{
		"unset":             nil,
		"none":              {},
		"all with projects": {Mode: foundryv1.ProjectScope_MODE_ALL, ProjectIds: []string{"p-1"}},
		"selected, empty":   {Mode: foundryv1.ProjectScope_MODE_SELECTED},
		"an unknown mode":   {Mode: foundryv1.ProjectScope_Mode(9), ProjectIds: []string{"p-1"}},
	} {
		if got := scopeFromProto(s); got.All() || got.Projects() != nil || got.Allows("p-1") {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	if got := scopeFromProto(&foundryv1.ProjectScope{Mode: foundryv1.ProjectScope_MODE_SELECTED, ProjectIds: []string{"p-1"}}); !got.Allows("p-1") || got.Allows("p-2") {
		t.Fatalf("selected: %+v", got)
	}
}
