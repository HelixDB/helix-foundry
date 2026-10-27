package foundryread

import (
	"context"
	"encoding/json"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryreadv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundryread/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
)

// resolver is where a workspace's model calls go; *ai.Resolver resolves it.
type resolver interface {
	ResolveServed(ctx context.Context, workspaceID string, providers []string) (ai.Resolved, error)
}

// Title writes a generation's title from its goal, within the caller's
// deadline (the backend's ontologygen.TitleTimeout). It is not counted
// against the workspace's reads.
//  1. Check the request, take one of the workspace's MaxTitlesPerWorkspace
//     slots or refuse, and check the sidecar speaks the vendored contract.
//  2. Resolve the workspace's AI setting, of a provider the sidecar calls.
//  3. Call the sidecar's Title on the setting's title model, its key in the
//     call's metadata, and relay the text the model ended its turn with.
//
// A setting that can't run and a model that failed are FAILED_PRECONDITION,
// whose message says which, never the key; the backend then titles the
// generation with its goal's first sentence.
func (r *reader) Title(ctx context.Context, req *foundryreadv1.TitleRequest) (*foundryreadv1.TitleResponse, error) {
	if req.GetWorkspaceId() == "" || req.GetGenerationId() == "" || req.GetGoal() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id, generation_id and goal are required")
	}
	release, ok := r.titles.acquire(req.GetWorkspaceId())
	if !ok {
		return nil, status.Errorf(codes.ResourceExhausted, "the workspace already writes %d titles; try again when one ends", r.titles.max)
	}
	defer release()
	infoCtx, cancelInfo := context.WithTimeout(ctx, agentReadyWait)
	info, err := r.agent.Info(infoCtx)
	cancelInfo()
	if err != nil {
		return nil, status.Error(codes.Unavailable, "the title writer is unavailable; try again")
	}
	if info.GetContractVersion() != ontologyagent.ContractVersion {
		return nil, status.Errorf(codes.FailedPrecondition, "the title writer speaks contract %s, not %s", info.GetContractVersion(), ontologyagent.ContractVersion)
	}
	resolved, err := r.ai.ResolveServed(ctx, req.GetWorkspaceId(), info.GetProviders())
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, ai.ResolveFailure(err).Logged(ctx, err).Message)
	}
	body, err := json.Marshal(ontologyagent.TitleRequest{RequestID: req.GetGenerationId(), Goal: req.GetGoal(), Models: resolved.Title()})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode title: %v", err)
	}
	raw, err := r.agent.Title(ctx, body, resolved.Key)
	if err != nil {
		return nil, status.Errorf(status.Code(err), "the title writer failed: %s", status.Convert(err).Message())
	}
	result, err := ontologyagent.DecodeTitleResult(raw)
	if err != nil {
		log.From(ctx).Error("foundry-read: unreadable title from the ontology agent", "err", err)
		return nil, status.Error(codes.Internal, "the title writer returned a result Helix can't read")
	}
	if result.Failure != "" {
		ai.TitleFailure(resolved, result.Failure).Logged(ctx, fmt.Errorf("the title's model failed: %s", result.Failure))
		return nil, status.Errorf(codes.FailedPrecondition, "the title model failed: %s", result.Failure)
	}
	return &foundryreadv1.TitleResponse{Text: result.Text}, nil
}
