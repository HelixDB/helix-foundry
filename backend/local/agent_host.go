package local

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/local/directory"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The helpers' side of the integration listener: the routes a helper on the
// owner's machine calls (agent-host/...), and the Foundry tools its CLI calls
// with a turn's token (/mcp with an hxr_ bearer). Neither takes the site
// token, and the site token's routes take neither of theirs: an hxh_ token
// claims and reports on turns only, and an hxr_ token calls the Assistant's
// tools for its one turn only. Both pass the listener's host and
// cross-origin checks first (IntegrationHandler).
//
// The routes, JSON in and out, each with the helper's hxh_ bearer but pair:
//
//	POST /agent-host/pair                {code, name, runtimes, version, platform} → {host_id, token}
//	POST /agent-host/hello               {runtimes, version, accounts} → {host_id, name}
//	POST /agent-host/claim               {runtimes, version, accounts} → {turn} within 25 s, or 204
//	POST /agent-host/turns/{id}/renew    {} → {lease_expires_at}
//	POST /agent-host/turns/{id}/text     {step, text, final} → 204
//	POST /agent-host/turns/{id}/finish   {end, reason, failure, detail, steps, session_id} → 204
//
// accounts says what each runtime's CLI is signed in to, as the helper last
// found it ([{runtime, state, subscription}], agent.HostAccount): the owner
// sees it in Settings › AI models.
//
// Refusals are {"error": {code, message}}: unauthenticated (401: the token
// is unknown or its helper revoked: pair again), pairing_invalid (404),
// turn_lost (410: the claim lost its lease, to a cancel, a deleted thread or
// its lapse; stop), wall_time (410: Foundry ended the turn at its wall
// time; stop), invalid_arguments (400).

const (
	// claimWait is the longest a claim waits for a turn; claimPoll how often
	// it looks while it waits.
	claimWait = 25 * time.Second
	claimPoll = time.Second
	// maxHostRequestBytes bounds a helper's request: a step's text is at most
	// 64 KiB, which JSON may escape to six times its size.
	maxHostRequestBytes = 512 << 10
)

// agentHostHandler serves the helpers.
type agentHostHandler struct {
	dir    *directory.Store
	turns  *agent.Hosts
	logger *slog.Logger
	mux    *http.ServeMux
}

func newAgentHostHandler(dir *directory.Store, turns *agent.Hosts, logger *slog.Logger) *agentHostHandler {
	h := &agentHostHandler{dir: dir, turns: turns, logger: logger, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /agent-host/pair", h.pair)
	h.mux.HandleFunc("POST /agent-host/hello", h.withHost(h.hello))
	h.mux.HandleFunc("POST /agent-host/claim", h.withHost(h.claim))
	h.mux.HandleFunc("POST /agent-host/turns/{id}/renew", h.withClaim(h.renew))
	h.mux.HandleFunc("POST /agent-host/turns/{id}/text", h.withClaim(h.text))
	h.mux.HandleFunc("POST /agent-host/turns/{id}/finish", h.withClaim(h.finish))
	return h
}

func (h *agentHostHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxHostRequestBytes)
	h.mux.ServeHTTP(w, r)
}

// hostError is a refusal of the helpers' routes: a closed set the helper
// acts on by its code.
type hostError struct {
	status  int
	code    string
	message string
}

var (
	errHostUnauthenticated = hostError{http.StatusUnauthorized, "unauthenticated", "the helper's token is unknown or revoked: pair it again"}
	errPairingInvalid      = hostError{http.StatusNotFound, "pairing_invalid", "the pairing code is unknown, used or expired: make another in Foundry"}
	errTurnLost            = hostError{http.StatusGone, "turn_lost", "the turn is no longer this helper's: stop it"}
	errWallTime            = hostError{http.StatusGone, "wall_time", "the turn ran out of time, and Foundry ended it: stop it"}
)

func invalid(message string) hostError {
	return hostError{http.StatusBadRequest, "invalid_arguments", message}
}

func (h *agentHostHandler) refuse(w http.ResponseWriter, e hostError) {
	writeHost(w, e.status, map[string]any{"error": map[string]string{"code": e.code, "message": e.message, "request_id": newRequestID(w)}})
}

// failed answers an error that isn't a refusal: one the store or a turn
// returned. A lost lease is turn_lost; anything else is logged, and the
// helper told only that it failed.
func (h *agentHostHandler) failed(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, agent.ErrLost):
		h.refuse(w, errTurnLost)
	case errors.Is(err, agent.ErrWallTime):
		h.refuse(w, errWallTime)
	default:
		h.logger.Error("agent host: request failed", "path", r.URL.Path, "err", err)
		h.refuse(w, hostError{http.StatusInternalServerError, "internal", "Foundry couldn't do it; try again"})
	}
}

func writeHost(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// decode reads a request's JSON strictly: an unknown field is refused.
func decode(r *http.Request, into any) error {
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	return d.Decode(into)
}

// bearerToken is the request's bearer token, when it has prefix.
func bearerToken(r *http.Request, prefix string) (string, bool) {
	const scheme = "bearer "
	value := r.Header.Get("Authorization")
	if len(value) < len(scheme) || !strings.EqualFold(value[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(value[len(scheme):])
	return token, strings.HasPrefix(token, prefix)
}

// hostRuntimes are the runtimes a helper says it offers, each a helper's.
func hostRuntimes(names []string) ([]model.FoundryAgentRuntime, bool) {
	out := make([]model.FoundryAgentRuntime, 0, len(names))
	for _, n := range names {
		r := model.FoundryAgentRuntime(n)
		if !r.Subscription() {
			return nil, false
		}
		out = append(out, r)
	}
	return out, true
}

type pairRequest struct {
	Code     string   `json:"code"`
	Name     string   `json:"name"`
	Runtimes []string `json:"runtimes"`
	Version  string   `json:"version"`
	Platform string   `json:"platform"`
}

// pair spends a pairing code on a new helper, and gives it its token, once.
func (h *agentHostHandler) pair(w http.ResponseWriter, r *http.Request) {
	var req pairRequest
	if err := decode(r, &req); err != nil {
		h.refuse(w, invalid("the body is {code, name, runtimes, version, platform}"))
		return
	}
	runtimes, ok := hostRuntimes(req.Runtimes)
	if !ok {
		h.refuse(w, invalid("runtimes are claude_code and codex"))
		return
	}
	host, token, err := h.dir.PairAgentHost(r.Context(), req.Code, agent.Host{Name: req.Name, Version: req.Version, Platform: req.Platform, Runtimes: runtimes})
	switch {
	case errors.Is(err, store.ErrNotFound):
		h.refuse(w, errPairingInvalid)
		return
	case errors.Is(err, directory.ErrInvalidAgentHost):
		h.refuse(w, invalid(err.Error()))
		return
	case err != nil:
		h.failed(w, r, err)
		return
	}
	h.logger.Info("agent host paired", "host", host.ID, "name", host.Name, "runtimes", host.Runtimes)
	writeHost(w, http.StatusOK, map[string]string{"host_id": host.ID, "token": token})
}

// withHost runs next for the live helper whose hxh_ bearer this is.
func (h *agentHostHandler) withHost(next func(http.ResponseWriter, *http.Request, agent.Host)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r, directory.HostTokenPrefix)
		if !ok {
			h.refuse(w, errHostUnauthenticated)
			return
		}
		host, err := h.dir.AgentHostByToken(r.Context(), token)
		switch {
		case errors.Is(err, store.ErrNotFound):
			h.refuse(w, errHostUnauthenticated)
		case err != nil:
			h.failed(w, r, err)
		default:
			next(w, r, host)
		}
	}
}

// withClaim runs next for the live claim the helper holds of the path's turn.
func (h *agentHostHandler) withClaim(next func(http.ResponseWriter, *http.Request, directory.AgentTurnToken)) http.HandlerFunc {
	return h.withHost(func(w http.ResponseWriter, r *http.Request, host agent.Host) {
		claim, err := h.dir.AgentTurnClaim(r.Context(), host.ID, r.PathValue("id"))
		switch {
		case errors.Is(err, store.ErrNotFound):
			h.refuse(w, errTurnLost)
			return
		case err != nil:
			h.failed(w, r, err)
			return
		}
		if err := h.dir.TouchAgentHost(r.Context(), host.ID); err != nil {
			h.failed(w, r, err)
			return
		}
		next(w, r, claim)
	})
}

type claimRequest struct {
	Runtimes []string            `json:"runtimes"`
	Version  string              `json:"version"`
	Accounts []agent.HostAccount `json:"accounts"`
}

// seen records what a live helper says it offers now, and returns it; false
// once it was refused.
func (h *agentHostHandler) seen(w http.ResponseWriter, r *http.Request, host agent.Host) ([]model.FoundryAgentRuntime, bool) {
	var req claimRequest
	if err := decode(r, &req); err != nil {
		h.refuse(w, invalid("the body is {runtimes, version, accounts}"))
		return nil, false
	}
	runtimes, ok := hostRuntimes(req.Runtimes)
	if !ok || len(runtimes) == 0 {
		h.refuse(w, invalid("runtimes are claude_code and codex, at least one"))
		return nil, false
	}
	switch err := h.dir.SeeAgentHost(r.Context(), host.ID, runtimes, req.Version, req.Accounts); {
	case errors.Is(err, directory.ErrInvalidAgentHost):
		h.refuse(w, invalid(err.Error()))
		return nil, false
	case err != nil:
		h.failed(w, r, err)
		return nil, false
	}
	return runtimes, true
}

// hello is a helper starting, or its doctor: it is seen, and told who it is.
func (h *agentHostHandler) hello(w http.ResponseWriter, r *http.Request, host agent.Host) {
	if _, ok := h.seen(w, r, host); ok {
		writeHost(w, http.StatusOK, map[string]string{"host_id": host.ID, "name": host.Name})
	}
}

// claimedTurn is a claimed turn as its helper is told it.
type claimedTurn struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspace_id"`
	ThreadID    string    `json:"thread_id"`
	Runtime     string    `json:"runtime"`
	Attempt     int       `json:"attempt"`
	Lease       time.Time `json:"lease_expires_at"`
	Deadline    time.Time `json:"deadline"`
	MaxSteps    int       `json:"max_steps"`
	// MCPToken is the bearer its CLI calls Foundry's tools at /mcp with.
	MCPToken string `json:"mcp_token"`
	System   string `json:"system"`
	Prompt   string `json:"prompt"`
	Resume   string `json:"resume,omitempty"`
	History  string `json:"history,omitempty"`
}

// claim waits up to claimWait for a due turn of a runtime the helper offers,
// leases it to a claim of its own, and mints the claim's hxr_ token.
func (h *agentHostHandler) claim(w http.ResponseWriter, r *http.Request, host agent.Host) {
	runtimes, ok := h.seen(w, r, host)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), claimWait)
	defer cancel()
	ticker := time.NewTicker(claimPoll)
	defer ticker.Stop()
	for {
		for _, runtime := range runtimes {
			workerID := "agent-host:" + host.ID + ":" + randomHex(4)
			turn, err := h.turns.Claim(ctx, runtime, workerID)
			if err != nil {
				if ctx.Err() != nil {
					break
				}
				h.failed(w, r, err)
				return
			}
			if turn == nil {
				continue
			}
			h.claimed(w, r, host, runtime, workerID, turn)
			return
		}
		select {
		case <-ctx.Done():
			// Seen again as it leaves: it calls again at once.
			_ = h.dir.TouchAgentHost(context.WithoutCancel(r.Context()), host.ID)
			w.WriteHeader(http.StatusNoContent)
			return
		case <-ticker.C:
		}
	}
}

// claimed mints the claim's token and tells the helper its turn. A turn
// whose token can't be minted is left to its lease, which a claim takes
// again.
func (h *agentHostHandler) claimed(w http.ResponseWriter, r *http.Request, host agent.Host, runtime model.FoundryAgentRuntime, workerID string, turn *agent.HostTurn) {
	ctx := context.WithoutCancel(r.Context())
	token, err := h.dir.CreateAgentTurnToken(ctx, directory.AgentTurnToken{
		HostID: host.ID, TurnID: turn.Turn.ID, WorkspaceID: turn.Turn.WorkspaceID, LeaseOwner: workerID, ExpiresAt: turn.Lease, Deadline: turn.Deadline,
	})
	if err != nil {
		h.failed(w, r, err)
		return
	}
	h.logger.Info("agent host claimed a turn", "host", host.ID, "turn", turn.Turn.ID, "runtime", runtime, "attempt", turn.Turn.Attempts)
	writeHost(w, http.StatusOK, map[string]claimedTurn{"turn": {
		ID: turn.Turn.ID, WorkspaceID: turn.Turn.WorkspaceID, ThreadID: turn.Turn.ThreadID, Runtime: string(runtime), Attempt: turn.Turn.Attempts,
		Lease: turn.Lease, Deadline: turn.Deadline, MaxSteps: turn.MaxSteps, MCPToken: token,
		System: turn.System, Prompt: turn.Prompt, Resume: turn.Resume, History: turn.History,
	}})
}

// renew extends the claim's lease and its token's life; past the turn's
// wall time, Foundry ends the turn instead (wall_time).
func (h *agentHostHandler) renew(w http.ResponseWriter, r *http.Request, claim directory.AgentTurnToken) {
	if err := decode(r, &struct{}{}); err != nil {
		h.refuse(w, invalid("the body is {}"))
		return
	}
	lease, err := h.turns.Renew(r.Context(), claim.TurnID, claim.LeaseOwner, claim.Deadline)
	if err == nil {
		err = h.dir.ExtendAgentTurnTokens(r.Context(), claim.TurnID, claim.LeaseOwner, lease)
	}
	if err != nil {
		if over(err) {
			h.endTokens(r, claim)
		}
		h.failed(w, r, err)
		return
	}
	writeHost(w, http.StatusOK, map[string]time.Time{"lease_expires_at": lease})
}

type textRequest struct {
	Step  int    `json:"step"`
	Text  string `json:"text"`
	Final bool   `json:"final"`
}

// text shows the person what the helper's model writes.
func (h *agentHostHandler) text(w http.ResponseWriter, r *http.Request, claim directory.AgentTurnToken) {
	var req textRequest
	if err := decode(r, &req); err != nil {
		h.refuse(w, invalid("the body is {step, text, final}"))
		return
	}
	var err error
	if time.Now().Before(claim.Deadline) {
		err = h.turns.Text(r.Context(), claim.TurnID, claim.LeaseOwner, agent.HostText{Step: req.Step, Text: req.Text, Final: req.Final})
	} else {
		// Past its wall time, Foundry ends the turn (agent.ErrWallTime).
		_, err = h.turns.Renew(r.Context(), claim.TurnID, claim.LeaseOwner, claim.Deadline)
	}
	if refusal, ok := invalidArgument(err); ok {
		h.refuse(w, invalid(refusal))
		return
	}
	if err != nil {
		if over(err) {
			h.endTokens(r, claim)
		}
		h.failed(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type finishRequest struct {
	// End is answered, stopped or failed.
	End string `json:"end"`
	// Reason is a stop's: max_steps, max_tokens, refusal or context.
	Reason string `json:"reason,omitempty"`
	// Failure is a failure's: signed_out, not_installed, rate_limited or
	// cli_error, with the CLI's words as Detail.
	Failure   string `json:"failure,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Steps     int    `json:"steps"`
	SessionID string `json:"session_id,omitempty"`
}

// hostEnd is how a helper says its turn ended, as package agent takes it.
func (f finishRequest) hostEnd() (agent.HostEnd, bool) {
	switch {
	case f.End == "answered" && f.Reason == "" && f.Failure == "" && f.Detail == "":
		return agent.HostAnswered{}, true
	case f.End == "stopped" && f.Failure == "" && f.Detail == "":
		return agent.HostStopped{Reason: agent.StopReason(f.Reason)}, true
	case f.End == "failed" && f.Reason == "":
		return agent.HostFailed{Kind: agent.HostFailure(f.Failure), Detail: f.Detail}, true
	}
	return nil, false
}

// finish ends the turn as the helper says it ended; its tokens stop working.
func (h *agentHostHandler) finish(w http.ResponseWriter, r *http.Request, claim directory.AgentTurnToken) {
	var req finishRequest
	if err := decode(r, &req); err != nil {
		h.refuse(w, invalid("the body is {end, reason, failure, detail, steps, session_id}"))
		return
	}
	end, ok := req.hostEnd()
	if !ok {
		h.refuse(w, invalid("end is answered; stopped with a reason; or failed with a failure and its detail"))
		return
	}
	turn, err := h.turns.DB.GetFoundryAgentTurn(r.Context(), claim.WorkspaceID, claim.TurnID)
	if err != nil {
		h.failed(w, r, err)
		return
	}
	err = h.turns.Finish(r.Context(), claim.TurnID, claim.LeaseOwner, turn.Runtime, agent.HostOutcome{End: end, Steps: req.Steps, Session: req.SessionID})
	if refusal, ok := invalidArgument(err); ok {
		h.refuse(w, invalid(refusal))
		return
	}
	if err == nil || over(err) {
		// The turn ended, or is no longer this claim's.
		h.endTokens(r, claim)
	}
	if err != nil {
		h.failed(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// over reports an error that ends a claim's turn for its helper: its lease
// was lost, or Foundry ended the turn at its wall time.
func over(err error) bool { return errors.Is(err, agent.ErrLost) || errors.Is(err, agent.ErrWallTime) }

// endTokens deletes the tokens of a claim whose turn ended or was lost: its
// CLI calls nothing more.
func (h *agentHostHandler) endTokens(r *http.Request, claim directory.AgentTurnToken) {
	if err := h.dir.EndAgentTurnTokens(context.WithoutCancel(r.Context()), claim.TurnID); err != nil {
		h.logger.Warn("agent host: end a turn's tokens", "turn", claim.TurnID, "err", err)
	}
}

// invalidArgument is a turn's refusal of what the helper sent, in its words.
func invalidArgument(err error) (string, bool) {
	if status.Code(err) != codes.InvalidArgument {
		return "", false
	}
	return status.Convert(err).Message(), true
}

// serveMCP serves the Assistant's tools to the CLI of the turn whose hxr_
// token this is, as that turn's person, in its workspace alone, while its
// claim holds the lease and the turn has time left: each call counted against the turn's, and shown to
// the person (agent.Hosts.Server). Any other path, or a token that stopped
// working, is refused.
func (h *agentHostHandler) serveMCP(w http.ResponseWriter, r *http.Request, token string) {
	if r.URL.Path != "/mcp" {
		refuse(w, "a turn's token calls the Assistant's tools only")
		return
	}
	claim, err := h.dir.AgentTurnToken(r.Context(), token)
	if errors.Is(err, store.ErrNotFound) {
		writeHost(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "unauthenticated", "message": "the turn's token stopped working"}})
		return
	}
	if err != nil {
		h.failed(w, r, err)
		return
	}
	turn, err := h.turns.DB.GetFoundryAgentTurn(r.Context(), claim.WorkspaceID, claim.TurnID)
	if err != nil || turn.Status != model.FoundryAgentTurnRunning || turn.LeaseOwner == nil || *turn.LeaseOwner != claim.LeaseOwner ||
		!time.Now().Before(claim.Deadline) {
		writeHost(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "unauthenticated", "message": "the turn's token stopped working"}})
		return
	}
	spend := func(ctx context.Context, ask bool) error {
		switch err := h.dir.SpendAgentTurnCall(ctx, token, ask, agent.HostCallsPerTurn, agent.HostAsksPerTurn); {
		case errors.Is(err, directory.ErrTurnCallsSpent):
			return agent.ErrHostCallsSpent
		case errors.Is(err, directory.ErrTurnAsksSpent):
			return agent.ErrHostAsksSpent
		case errors.Is(err, store.ErrNotFound):
			return agent.ErrLost
		default:
			return err
		}
	}
	server, err := h.turns.Server(r.Context(), turn, claim.LeaseOwner, spend)
	if err != nil {
		h.failed(w, r, err)
		return
	}
	mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: maxMCPRequestBytes, PropagateRequestCancellation: true,
			Logger: h.logger}).ServeHTTP(w, r)
}

// randomHex is n random bytes in hex.
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
