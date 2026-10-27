package api

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
)

// actorKey is the gRPC metadata key a front end sends the caller's Actor
// in, as a marshalled helix.foundry.v1.Actor.
const actorKey = "helix-foundry-actor-bin"

// foundryMethods prefixes the full method names of Foundry's services.
const foundryMethods = "/helix.foundry.v1."

// AttachActor returns ctx with a as its only outgoing metadata: a call made
// with it reaches Foundry's API as a, and carries nothing else of ctx's
// metadata.
func AttachActor(ctx context.Context, a ports.Actor) context.Context {
	raw, err := proto.Marshal(actorToProto(a))
	if err != nil {
		panic(err) // an Actor of strings and enums always marshals
	}
	return metadata.NewOutgoingContext(ctx, metadata.Pairs(actorKey, string(raw)))
}

// ActorInterceptor puts the Actor a call to Foundry's services carries in
// its metadata (AttachActor) on the call's context, and refuses such a call
// without one (UNAUTHENTICATED). Calls to other services pass untouched.
// Install it only on a server no one but trusted front ends reaches.
func ActorInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !strings.HasPrefix(info.FullMethod, foundryMethods) {
			return handler(ctx, req)
		}
		a, err := actorFromMetadata(ctx)
		if err != nil {
			return nil, err
		}
		return handler(ports.WithActor(ctx, a), req)
	}
}

func actorFromMetadata(ctx context.Context) (ports.Actor, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(actorKey)
	if len(values) != 1 {
		return ports.Actor{}, status.Error(codes.Unauthenticated, "a Foundry call needs exactly one actor")
	}
	var pa foundryv1.Actor
	if err := proto.Unmarshal([]byte(values[0]), &pa); err != nil {
		return ports.Actor{}, status.Error(codes.Unauthenticated, "the call's actor is malformed")
	}
	a, ok := actorFromProto(&pa)
	if !ok {
		return ports.Actor{}, status.Error(codes.Unauthenticated, "the call's actor names no caller")
	}
	return a, nil
}

var actorSourceToProto = map[ports.Source]foundryv1.Source{
	ports.SourceUser:        foundryv1.Source_SOURCE_USER,
	ports.SourceMCP:         foundryv1.Source_SOURCE_MCP,
	ports.SourceEventSource: foundryv1.Source_SOURCE_EVENT_SOURCE,
}

var actorSourceFromProto = map[foundryv1.Source]ports.Source{
	foundryv1.Source_SOURCE_USER:         ports.SourceUser,
	foundryv1.Source_SOURCE_MCP:          ports.SourceMCP,
	foundryv1.Source_SOURCE_EVENT_SOURCE: ports.SourceEventSource,
}

func actorToProto(a ports.Actor) *foundryv1.Actor {
	return &foundryv1.Actor{
		Source: actorSourceToProto[a.Source], UserId: a.UserID, EventSourceTokenId: a.EventSourceTokenID, WorkspaceId: a.WorkspaceID,
		QueryRead: scopeToProto(a.QueryRead), QueryWrite: scopeToProto(a.QueryWrite),
	}
}

// actorFromProto reads an Actor; false for one of no known source.
func actorFromProto(pa *foundryv1.Actor) (ports.Actor, bool) {
	source, ok := actorSourceFromProto[pa.GetSource()]
	if !ok {
		return ports.Actor{}, false
	}
	return ports.Actor{
		Source: source, UserID: pa.GetUserId(), EventSourceTokenID: pa.GetEventSourceTokenId(), WorkspaceID: pa.GetWorkspaceId(),
		QueryRead: scopeFromProto(pa.GetQueryRead()), QueryWrite: scopeFromProto(pa.GetQueryWrite()),
	}, true
}

func scopeToProto(s ports.ProjectScope) *foundryv1.ProjectScope {
	switch {
	case s.All():
		return &foundryv1.ProjectScope{Mode: foundryv1.ProjectScope_MODE_ALL}
	case len(s.Projects()) > 0:
		return &foundryv1.ProjectScope{Mode: foundryv1.ProjectScope_MODE_SELECTED, ProjectIds: s.Projects()}
	default:
		return &foundryv1.ProjectScope{}
	}
}

// scopeFromProto reads a scope; one that is malformed (all with projects,
// or a mode it doesn't know) grants none.
func scopeFromProto(s *foundryv1.ProjectScope) ports.ProjectScope {
	switch s.GetMode() {
	case foundryv1.ProjectScope_MODE_ALL:
		if len(s.GetProjectIds()) == 0 {
			return ports.AllProjects()
		}
	case foundryv1.ProjectScope_MODE_SELECTED:
		return ports.SelectedProjects(s.GetProjectIds())
	}
	return ports.NoProjects()
}

// actor is the call's Actor: the zero Actor, of no user and no projects,
// where the call carries none.
func actor(ctx context.Context) ports.Actor {
	a, _ := ports.ActorFrom(ctx)
	return a
}
