package api

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	foundryagent "github.com/helixdb/helix-foundry/backend/agent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// Where a local install's Assistant runs its threads (foundry.agent's
// helpers): Foundry's own worker, or a helper on the owner's machine that
// drives the Claude Code or Codex CLI they signed in to. Codex runs with the
// owner's own Codex setup, so they confirm that once before choosing it. A
// host without helpers (Deps.AgentHosts nil: hosted Helix) refuses every
// route here, and runs every thread on its worker.

// agentHostIDPattern is a helper's id.
var agentHostIDPattern = regexp.MustCompile(`^[0-9A-Z]{26}$`)

// agentHosts is the install's helpers, for the owner: FAILED_PRECONDITION
// (FOUNDRY_AGENT_HOSTS_LOCAL_ONLY) where the host has none, and
// PERMISSION_DENIED for anyone who may not choose the workspace's model.
func (s *Service) agentHosts(ctx context.Context, workspaceID string) (foundryagent.HostRegistry, error) {
	if _, err := s.agentUser(ctx, workspaceID); err != nil {
		return nil, err
	}
	switch {
	case s.deps.AgentHosts == nil:
		return nil, foundryPrecondition(workspaceID, "FOUNDRY_AGENT_HOSTS_LOCAL_ONLY",
			"the Assistant runs on a subscription only on a local install")
	case !s.mayManageAI(ctx, workspaceID):
		return nil, status.Error(codes.PermissionDenied, "only the workspace's owner chooses where the Assistant runs")
	}
	return s.deps.AgentHosts, nil
}

func (s *Service) GetFoundryAgentRuntime(ctx context.Context, req *foundryv1.GetFoundryAgentRuntimeRequest) (*foundryv1.FoundryAgentRuntimeResponse, error) {
	hosts, err := s.agentHosts(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	return s.agentRuntimeResponse(ctx, hosts, req.GetWorkspaceId())
}

// UpdateFoundryAgentRuntime sets where the workspace's new threads run.
func (s *Service) UpdateFoundryAgentRuntime(ctx context.Context, req *foundryv1.UpdateFoundryAgentRuntimeRequest) (*foundryv1.FoundryAgentRuntimeResponse, error) {
	hosts, err := s.agentHosts(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	runtime := foundrymodel.FoundryAgentRuntime(req.GetRuntime())
	if !runtime.Valid() {
		return nil, status.Error(codes.InvalidArgument, "runtime is api, claude_code or codex")
	}
	_, err = hosts.SetAgentRuntime(ctx, req.GetWorkspaceId(), runtime, req.GetConfirmCodex())
	switch {
	case errors.Is(err, foundryagent.ErrCodexUnconfirmed):
		return nil, foundryPrecondition(req.GetWorkspaceId(), "FOUNDRY_AGENT_CODEX_UNCONFIRMED",
			"Codex runs with your own Codex setup, its MCP servers and plugins included, on your ChatGPT plan: choose it in Settings › AI models, where you confirm that once")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "set the runtime: %v", err)
	}
	return s.agentRuntimeResponse(ctx, hosts, req.GetWorkspaceId())
}

func (s *Service) agentRuntimeResponse(ctx context.Context, hosts foundryagent.HostRegistry, workspaceID string) (*foundryv1.FoundryAgentRuntimeResponse, error) {
	setting, err := hosts.AgentRuntime(ctx, workspaceID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read the runtime: %v", err)
	}
	listed, err := hosts.ListAgentHosts(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list the helpers: %v", err)
	}
	now := time.Now()
	out := &foundryv1.FoundryAgentRuntime{Runtime: string(setting.Runtime), Online: []string{}, CodexMinVersion: foundryagent.MinCodexVersion}
	if setting.CodexConfirmedAt != nil {
		out.CodexConfirmedAt = timestamppb.New(*setting.CodexConfirmedAt)
	}
	for _, h := range listed {
		out.Hosts = append(out.Hosts, agentHostToProto(h, now))
		if !h.Online(now) {
			continue
		}
		for _, r := range h.Runtimes {
			if !slices.Contains(out.Online, string(r)) {
				out.Online = append(out.Online, string(r))
			}
		}
	}
	slices.Sort(out.Online)
	return &foundryv1.FoundryAgentRuntimeResponse{Runtime: out}, nil
}

// CreateFoundryAgentHostPairing makes a one-time code that pairs a helper,
// and the command that pairs one with it.
func (s *Service) CreateFoundryAgentHostPairing(ctx context.Context, req *foundryv1.CreateFoundryAgentHostPairingRequest) (*foundryv1.FoundryAgentHostPairingResponse, error) {
	hosts, err := s.agentHosts(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	code, expires, err := hosts.CreateAgentPairing(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "make a pairing code: %v", err)
	}
	return &foundryv1.FoundryAgentHostPairingResponse{Code: code, ExpiresAt: timestamppb.New(expires),
		Command: foundryagent.PairCommand(code, s.deps.AgentHostURL)}, nil
}

// RevokeFoundryAgentHost unpairs a helper.
func (s *Service) RevokeFoundryAgentHost(ctx context.Context, req *foundryv1.RevokeFoundryAgentHostRequest) (*foundryv1.FoundryAgentHostResponse, error) {
	hosts, err := s.agentHosts(ctx, req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	if !agentHostIDPattern.MatchString(req.GetId()) {
		return nil, status.Error(codes.InvalidArgument, "id is a helper's")
	}
	h, err := hosts.RevokeAgentHost(ctx, req.GetId())
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "helper not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "revoke the helper: %v", err)
	}
	return &foundryv1.FoundryAgentHostResponse{Host: agentHostToProto(h, time.Now())}, nil
}

// agentThreadRuntime is the runtime a message runs on: its thread's, or for
// a new thread the workspace's, refusing a helper's while none that offers
// it is online (FOUNDRY_AGENT_HOST_OFFLINE).
func (s *Service) agentThreadRuntime(ctx context.Context, workspaceID, userID, threadID string) (foundrymodel.FoundryAgentRuntime, error) {
	runtime := foundrymodel.FoundryAgentRuntimeAPI
	switch {
	case threadID != "":
		t, err := s.agentThread(ctx, workspaceID, userID, threadID)
		if err != nil {
			return "", err
		}
		runtime = t.Runtime
	case s.deps.AgentHosts != nil:
		r, err := s.deps.AgentHosts.AgentRuntime(ctx, workspaceID)
		if err != nil {
			return "", status.Errorf(codes.Internal, "read the runtime: %v", err)
		}
		runtime = r.Runtime
	}
	if !runtime.Subscription() {
		return runtime, nil
	}
	if s.deps.AgentHosts == nil {
		// A helper's thread on a host without helpers: none can run it.
		return "", foundryPrecondition(workspaceID, "FOUNDRY_AGENT_HOST_OFFLINE", "no helper runs this thread here")
	}
	online, err := s.deps.AgentHosts.AgentRuntimeOnline(ctx, runtime)
	switch {
	case err != nil:
		return "", status.Errorf(codes.Internal, "look for a helper: %v", err)
	case !online:
		return "", foundryPrecondition(workspaceID, "FOUNDRY_AGENT_HOST_OFFLINE", s.offlineWhy(ctx, runtime))
	}
	return runtime, nil
}

// offlineWhy says why no helper runs runtime: one online whose Codex is
// outdated, or didn't say its version, says so; any other none ran.
func (s *Service) offlineWhy(ctx context.Context, runtime foundrymodel.FoundryAgentRuntime) string {
	if hosts, err := s.deps.AgentHosts.ListAgentHosts(ctx); err == nil {
		now := time.Now()
		for _, h := range hosts {
			i := slices.IndexFunc(h.Accounts, func(a foundryagent.HostAccount) bool { return a.Runtime == runtime })
			switch {
			case !h.Online(now) || i < 0 || runtime != foundrymodel.FoundryAgentRuntimeCodex:
			case h.Accounts[i].Outdated():
				return "the Codex on " + h.Name + " is older than " + foundryagent.MinCodexVersion +
					", the oldest Foundry runs the Assistant on: update it there, then send again in a minute"
			case h.Accounts[i].Version == "":
				return "the Codex on " + h.Name + " didn't start: run codex --version there to see why, then send again in a minute"
			}
		}
	}
	return "no helper running " + string(runtime) + " has been seen in the last 30 seconds: start it on your computer, then send again"
}

func agentHostToProto(h foundryagent.Host, now time.Time) *foundryv1.FoundryAgentHost {
	out := &foundryv1.FoundryAgentHost{Id: h.ID, Name: h.Name, Version: h.Version, Platform: h.Platform, Runtimes: []string{},
		CreatedAt: timestamppb.New(h.CreatedAt), Online: h.Online(now)}
	if h.LastSeenAt != nil {
		out.LastSeenAt = timestamppb.New(*h.LastSeenAt)
	}
	for _, r := range h.Runtimes {
		out.Runtimes = append(out.Runtimes, string(r))
	}
	for _, a := range h.Accounts {
		out.Accounts = append(out.Accounts, &foundryv1.FoundryAgentHostAccount{Runtime: string(a.Runtime), State: string(a.State),
			Subscription: a.Subscription, Version: a.Version, Outdated: a.Outdated()})
	}
	if h.RevokedAt != nil {
		out.RevokedAt = timestamppb.New(*h.RevokedAt)
	}
	return out
}
