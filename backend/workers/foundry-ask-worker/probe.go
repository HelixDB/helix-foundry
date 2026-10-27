package foundryaskworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// probe.go tests the AI settings owners save (foundry_ai_configs) before
// any model call runs on them: a probe of every role's model through the
// sidecar's Probe, one at a time per replica, on a goroutine of its own
// beside the questions, so a probe never waits behind one.

// probeTail is how long a Probe call may outlast the probe's wall-time cap,
// for the agent to end its checks and answer.
const probeTail = 15 * time.Second

// probeQueue is where saved settings wait for their probe.
type probeQueue interface {
	ClaimFoundryAIProbe(ctx context.Context, owner string, lease time.Duration, maxAttempts int) (*model.FoundryAIConfig, error)
	ReleaseFoundryAIProbe(ctx context.Context, configID, owner string, maxAttempts int) error
	ReturnFoundryAIProbe(ctx context.Context, configID, owner string) error
	FinishFoundryAIProbe(ctx context.Context, configID, owner string, outcome model.FoundryAIProbeOutcome) (model.FoundryAIProbeState, error)
}

// prober probes saved settings, as owner.
type prober struct {
	queue      probeQueue
	agent      agent
	keys       *secretbox.Keyring
	deployment *ai.Deployment
	cfg        config.AIProbe
	owner      string
}

// errCantRun is a probe that couldn't run; another attempt may.
var errCantRun = errors.New("the probe could not run")

// tick probes the oldest setting waiting for its probe, if one is.
//  1. Claim it, under the probe's lease.
//  2. Open its key: the zero key for a kind that takes none.
//  3. Check the sidecar speaks the vendored contract and calls the setting's
//     kind, then probe every role's model, the key in the call's metadata.
//  4. Record what it found: passed, or the first failed check's failure and
//     detail, with every check's report. A passed probe makes the setting
//     active unless a newer save or setting came first; one whose report
//     its runs couldn't send (an Ollama digest that isn't one) couldn't run.
//
// A probe that couldn't run (no keyring, a key that doesn't open, a sidecar
// unavailable or of another contract) gives its claim back, the attempt
// counted: after foundry.ai.probe.max_attempts the setting fails
// unavailable, and the one before it stays. One the worker cut short as it
// stopped is given back uncounted, so rollouts use up no attempts.
func (p *prober) tick(ctx context.Context) {
	c, err := p.queue.ClaimFoundryAIProbe(ctx, p.owner, p.cfg.Lease, p.cfg.MaxAttempts)
	if err != nil {
		log.From(ctx).Error("claim AI settings probe failed", "err", err)
		return
	}
	if c == nil {
		return
	}
	logger := log.From(ctx).With("config", c.ID, "workspace", c.WorkspaceID, "kind", c.Kind, "attempt", c.ProbeAttempts)
	ctx = log.Into(ctx, logger)
	// The lease runs from the claim: the probe stops in time to record its
	// outcome within it, however long the sidecar took to answer.
	probeCtx, cancelProbe := context.WithTimeout(ctx, p.cfg.Lease-finishTimeout)
	outcome, err := p.probe(probeCtx, c)
	cancelProbe()
	// What's left is recorded even as the worker stops.
	endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	if err != nil && ctx.Err() != nil {
		logger.Warn("AI settings probe cut short as the worker stops; it is given back uncounted", "err", err)
		if err := p.queue.ReturnFoundryAIProbe(endCtx, c.ID, p.owner); err != nil && !errors.Is(err, foundrystore.ErrNotFound) {
			logger.Error("return AI settings probe failed; its lease lapses and it runs again", "err", err)
		}
		probes.WithLabelValues("returned").Inc()
		return
	}
	if err != nil {
		logger.Error("AI settings probe could not run; it is given back", "err", err)
		if err := p.queue.ReleaseFoundryAIProbe(endCtx, c.ID, p.owner, p.cfg.MaxAttempts); err != nil && !errors.Is(err, foundrystore.ErrNotFound) {
			logger.Error("release AI settings probe failed", "err", err)
		}
		probes.WithLabelValues("released").Inc()
		return
	}
	state, err := p.queue.FinishFoundryAIProbe(endCtx, c.ID, p.owner, outcome)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		probes.WithLabelValues("lost").Inc()
		logger.Warn("AI settings probe ended after its lease; another worker owns it")
	case err != nil:
		logger.Error("finish AI settings probe failed; its lease lapses and it runs again", "err", err)
	default:
		probes.WithLabelValues(string(state)).Inc()
		failure := ""
		if outcome.Failure != nil {
			failure = outcome.Failure.Failure + "/" + outcome.Failure.Detail
		}
		logger.Info("AI settings probed", "state", state, "failure", failure)
	}
}

// probe runs c's probe: its outcome, or an error wrapping errCantRun.
func (p *prober) probe(ctx context.Context, c *model.FoundryAIConfig) (model.FoundryAIProbeOutcome, error) {
	cantRun := func(why string, err error) (model.FoundryAIProbeOutcome, error) {
		return model.FoundryAIProbeOutcome{}, fmt.Errorf("%w: %s: %w", errCantRun, why, err)
	}
	settings, err := ai.DecodeSettings(c.Settings)
	if err != nil {
		return cantRun("read the settings", err)
	}
	var key ontologyagent.ModelKey
	switch {
	case c.Secret != nil && p.keys == nil:
		return cantRun("open the key", ai.ErrNoKeyring)
	case c.Secret != nil:
		if key, err = ai.OpenKey(p.keys, c); err != nil {
			return cantRun("open the key", ai.ErrKeyUnreadable)
		}
	}
	infoCtx, cancelInfo := context.WithTimeout(ctx, config.AskReadyWait)
	info, err := p.agent.Info(infoCtx)
	cancelInfo()
	switch {
	case err != nil:
		return cantRun("reach the ontology agent", err)
	case info.GetContractVersion() != ontologyagent.ContractVersion:
		return cantRun("check the ontology agent", fmt.Errorf("it speaks contract %s, not %s", info.GetContractVersion(), ontologyagent.ContractVersion))
	case !slices.Contains(info.GetProviders(), c.Kind):
		return cantRun("check the ontology agent", fmt.Errorf("%w: %s", ai.ErrNotServed, c.Kind))
	}
	request := p.deployment.ProbeRequest(c.ID, settings, p.cfg.Timeout)
	body, err := json.Marshal(request)
	if err != nil {
		return cantRun("write the probe", err)
	}
	callCtx, cancelCall := context.WithTimeout(ctx, p.cfg.Timeout+probeTail)
	defer cancelCall()
	raw, err := p.agent.Probe(callCtx, body, key)
	if err != nil {
		return cantRun("probe", err)
	}
	roles := make([]ontologyagent.ProbeRole, len(request.Checks))
	for i, check := range request.Checks {
		roles[i] = check.Role
	}
	result, err := ontologyagent.DecodeProbeResult(raw, roles)
	if err != nil {
		return cantRun("read the probe's result", err)
	}
	found := ai.ReportOf(result)
	f := result.Failed()
	if err := found.Usable(); f == nil && err != nil {
		return cantRun("read the probe's result", err)
	}
	report, err := json.Marshal(found)
	if err != nil {
		return cantRun("write the probe's report", err)
	}
	outcome := model.FoundryAIProbeOutcome{Report: report}
	if f != nil {
		outcome.Failure = &model.FoundryAIProbeFailure{Failure: string(f.Failure), Detail: string(f.Detail)}
	}
	return outcome, nil
}
