package foundryread

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/graphproxy"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryreadv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundryread/v1"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
)

const (
	// MaxReadsPerWorkspace is how many reads one workspace runs at once on a
	// replica; past it a read is RESOURCE_EXHAUSTED, not queued.
	MaxReadsPerWorkspace = 4
	// MaxTitlesPerWorkspace is how many titles one workspace writes at once on
	// a replica, apart from its reads: each spends its AI setting's key. Past
	// it a title is RESOURCE_EXHAUSTED, and the backend titles with the goal.
	MaxTitlesPerWorkspace = 2
	// MaxResultBytes is the largest result relayed. The plan bounds the agent's
	// at 4 MiB, but the backend's and the website front end's gRPC clients keep
	// gRPC's 4 MiB default, envelope included.
	MaxResultBytes = 3 << 20
	// helixTimeoutSecs bounds one Helix request of a read.
	helixTimeoutSecs = 10
	// admissionWait is how long the proxy sends a read the gateway turned
	// away (rate limited) again: within the budget's wall time.
	admissionWait = 2 * time.Second
	// readTail is how long a call may outlast the budget's wall time, for the
	// agent's round in flight and its answer.
	readTail = 10 * time.Second
	// agentReadyWait bounds the contract check of a sidecar still starting.
	agentReadyWait = 5 * time.Second
)

// agent is the sidecar; *ontologyagent.Client calls it.
type agent interface {
	Info(ctx context.Context) (*agentv1.GetInfoResponse, error)
	Read(ctx context.Context, request []byte) (json.RawMessage, error)
	Title(ctx context.Context, request []byte, key ontologyagent.ModelKey) (json.RawMessage, error)
}

// reader serves FoundryRead.
type reader struct {
	db graphproxy.Store
	// dbs are the host's databases, which a load's is.
	dbs   ports.Databases
	cfg   *config.Config
	agent agent
	// ai resolves the model and key each title runs on.
	ai    resolver
	proxy *graphproxy.ReadProxy
	limit *workspaceLimit
	// titles holds each workspace's titles in flight, apart from its reads.
	titles *workspaceLimit
	now    func() time.Time
}

// Read reads one load.
//  1. Check the request and parse its reads.
//  2. Take one of the workspace's MaxReadsPerWorkspace slots, or refuse.
//  3. Check the sidecar speaks the vendored contract.
//  4. Bind the ontology's live database and register the binding on the
//     read-only proxy for the call's life: the budget's wall time plus
//     readTail.
//  5. Call the sidecar's Read with the default budget, as of today (UTC).
//  6. Relay its result, at most MaxResultBytes, once it answers each read
//     in order, with an answer of its kind or a failure.
func (r *reader) Read(ctx context.Context, req *foundryreadv1.ReadRequest) (*foundryreadv1.ReadResponse, error) {
	if req.GetWorkspaceId() == "" || req.GetOntologyId() == "" || req.GetLoadId() == "" || req.GetOntologyVersion() == "" || req.GetOntologyToml() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id, ontology_id, load_id, ontology_version and ontology_toml are required")
	}
	reads, err := ontologyagent.ParseReads(req.GetReadsJson())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	release, ok := r.limit.acquire(req.GetWorkspaceId())
	if !ok {
		return nil, status.Errorf(codes.ResourceExhausted, "the workspace already runs %d graph reads; try again when one ends", r.limit.max)
	}
	defer release()
	infoCtx, cancelInfo := context.WithTimeout(ctx, agentReadyWait)
	info, err := r.agent.Info(infoCtx)
	cancelInfo()
	if err != nil {
		log.From(ctx).Warn("foundry-read: the ontology agent is unavailable", "err", err)
		return nil, status.Error(codes.Unavailable, "the graph reader is unavailable; try again")
	}
	if info.GetContractVersion() != ontologyagent.ContractVersion {
		return nil, status.Errorf(codes.FailedPrecondition, "the graph reader speaks contract %s, not %s", info.GetContractVersion(), ontologyagent.ContractVersion)
	}
	binding, err := graphproxy.BindRead(ctx, r.db, r.dbs, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err // gRPC statuses: NotFound, FailedPrecondition, Unavailable
	}
	budget := ontologyagent.DefaultReadBudget
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(budget.WallMS)*time.Millisecond+readTail)
	defer cancel() // ends the token and any of the call's reads still in flight
	// The call ends well within graphproxy.RecheckEvery, so its binding is not checked again.
	token, err := r.proxy.Register(callCtx, binding, nil)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "register read: %v", err)
	}
	body, err := json.Marshal(ontologyagent.ReadRequest{
		RequestID: id.New(), Database: binding.TenantID, Load: req.GetLoadId(), OntologyTOML: req.GetOntologyToml(),
		OntologyVersion: req.GetOntologyVersion(),
		Helix: ontologyagent.HelixTarget{
			URL: r.proxy.URL(), Token: token, MaxRequestBytes: graphproxy.MaxRequestBytes, TimeoutSecs: helixTimeoutSecs,
		},
		AsOf: r.now().UTC().Format(time.DateOnly), Budget: budget, Reads: reads,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode read: %v", err)
	}
	result, err := r.agent.Read(callCtx, body)
	if err != nil {
		return nil, agentError(ctx, err)
	}
	if len(result) > MaxResultBytes {
		return nil, status.Errorf(codes.ResourceExhausted, "the reads' result is %d KiB, past the %d MiB a read returns; ask for fewer reads at once",
			len(result)>>10, MaxResultBytes>>20)
	}
	if err := ontologyagent.CheckReadResult(result, reads); err != nil {
		log.From(ctx).Error("foundry-read: unreadable result from the ontology agent", "err", err)
		return nil, status.Error(codes.Internal, "the graph reader returned a result Helix can't read")
	}
	return &foundryreadv1.ReadResponse{ResultJson: string(result)}, nil
}

// agentError is the caller's status for a failed Read call. The agent's own
// refusals keep their code and message: bad reads (INVALID_ARGUMENT), a load
// the database doesn't hold for the version (FAILED_PRECONDITION), a full
// sidecar (RESOURCE_EXHAUSTED) and one shutting down (UNAVAILABLE).
func agentError(ctx context.Context, err error) error {
	switch code := status.Code(err); code {
	case codes.InvalidArgument, codes.FailedPrecondition, codes.ResourceExhausted, codes.Unavailable, codes.Canceled:
		return status.Error(code, status.Convert(err).Message())
	case codes.DeadlineExceeded:
		return status.Error(codes.DeadlineExceeded, "the reads took too long; ask for fewer at once")
	default:
		log.From(ctx).Error("foundry-read: the ontology agent's read failed", "err", err)
		return status.Error(codes.Unavailable, "the graph reader failed; try again")
	}
}

// workspaceLimit admits at most max reads at once per workspace.
type workspaceLimit struct {
	max     int
	mu      sync.Mutex
	running map[string]int
}

func newWorkspaceLimit(max int) *workspaceLimit {
	if max < 1 {
		panic(fmt.Sprintf("foundry-read: a workspace limit of %d admits nothing", max))
	}
	return &workspaceLimit{max: max, running: map[string]int{}}
}

// acquire takes one of workspaceID's slots and returns its release; ok is
// false, and nothing taken, while all of them are.
func (l *workspaceLimit) acquire(workspaceID string) (release func(), ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running[workspaceID] >= l.max {
		return nil, false
	}
	l.running[workspaceID]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.running[workspaceID]--; l.running[workspaceID] == 0 {
				delete(l.running, workspaceID)
			}
		})
	}, true
}
