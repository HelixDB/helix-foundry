package foundryhealworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// fills acts on the fills due (store.DueFoundryHealFills): a load moved in
// place by a suggestion's "Add this link" has the documents behind the
// suggestion's reviews collected (collect) and owed a fill of the link; one
// whose fill paused, or waits with no job live, is resumed, as is a move
// whose job failed, once it backed off. One at a time, the one with the
// least support first, as a load fills one suggestion at a time, until the
// tick has spent its poll interval on them: the next tick lists them again,
// and the audits due run between. A collect is claimed first
// (store.ClaimFoundryFillCollect), for maxRunTime and collectLeaseSlack, so
// no other worker collects the fill, or another of its load, meanwhile. A
// collect that fails, or outlasts maxRunTime, backs off and is tried again
// (store.FailFoundryFillCollect), until it has failed too often and the
// suggestion needs a load: it owes documents again, which costs nothing.
func (r *runner) fills(ctx context.Context) {
	started := time.Now()
	due, err := r.db.DueFoundryHealFills(ctx, dueBatch)
	if err != nil {
		log.From(ctx).Error("find fills due failed", "err", err)
		return
	}
	for i, d := range due {
		if ctx.Err() != nil || i > 0 && time.Since(started) > r.cfg.PollInterval {
			return
		}
		logger := log.From(ctx).With("fill", d.Fill.ID, "ontology", d.Fill.OntologyID, "load", d.Fill.LoadID)
		if d.Resume {
			queued, err := r.db.ResumeFoundryFill(ctx, d.Fill, d.Due.WorkspaceID, &model.FoundryGraphJob{ID: r.newID()})
			if err != nil {
				logger.Error("resume fill failed", "err", err)
			} else if queued {
				fillRuns.WithLabelValues("resumed").Inc()
				logger.Info("fill resumed")
			}
			continue
		}
		switch claimed, err := r.db.ClaimFoundryFillCollect(ctx, d.Fill, r.maxRun+collectLeaseSlack); {
		case err != nil:
			fillRuns.WithLabelValues("failed").Inc()
			logger.Error("claim fill collect failed", "err", err)
			continue
		case !claimed:
			fillRuns.WithLabelValues("changed").Inc()
			continue
		}
		collectCtx, cancel := context.WithTimeout(ctx, r.maxRun)
		owed, left, err := r.collect(collectCtx, d)
		cancel()
		switch {
		case errors.Is(err, errFillChanged):
			fillRuns.WithLabelValues("changed").Inc()
			continue
		case err != nil && ctx.Err() != nil:
			return
		case err != nil:
			fillRuns.WithLabelValues("failed").Inc()
			logger.Warn("collect fill failed", "err", err)
			if err := r.db.FailFoundryFillCollect(ctx, d.Fill, err.Error()); err != nil {
				logger.Error("record failed collect", "err", err)
			}
			continue
		}
		switch err := r.db.StartFoundryFill(ctx, d.Fill, d.Due.WorkspaceID, owed, left, &model.FoundryGraphJob{ID: r.newID()}); {
		case errors.Is(err, store.ErrConflict):
			// What it owed isn't what the load owes, or the fill changed:
			// counted as a failed collect, which ends what it owed and backs
			// off (nothing, if the fill changed).
			fillRuns.WithLabelValues("changed").Inc()
			logger.Warn("start fill refused", "err", err)
			if err := r.db.FailFoundryFillCollect(ctx, d.Fill, err.Error()); err != nil {
				logger.Error("record failed collect", "err", err)
			}
		case err != nil:
			fillRuns.WithLabelValues("failed").Inc()
			logger.Error("start fill failed", "err", err)
		default:
			fillRuns.WithLabelValues("started").Inc()
			logger.Info("fill started", "owed", owed, "left", left)
		}
	}
}

// errFillChanged is a fill, its collect's lease, or its load, that changed
// while it was collected: nothing more is done of it.
var errFillChanged = errors.New("the fill or its load changed")

// collectLeaseSlack is how long a collect's lease outlasts its time limit
// (maxRunTime), so it never runs out while the collect still writes.
const collectLeaseSlack = 5 * time.Minute

// collect pages Audit over d's load with documents_for the suggestion's
// relationship (its set and spellings, from the finding as stored), and owes
// the load a fill of each document it lists (store.OweFoundryFillDocuments),
// page by page, so no list of them is held: owed is how many, left those
// past model.FoundryFillMaxDocuments. The pages are the load's documents
// with reviews, as an audit's; a page the agent's wall cuts is read again at
// half the size. A move that didn't add the suggestion's link (its finding's
// canonical) owes none: there is nothing of it to fill.
func (r *runner) collect(ctx context.Context, d model.FoundryFillDue) (owed, left int64, err error) {
	var finding struct {
		Signal struct {
			Kind      string   `json:"kind"`
			Canonical string   `json:"canonical"`
			Set       string   `json:"set"`
			Spellings []string `json:"spellings"`
		} `json:"signal"`
	}
	if err := json.Unmarshal(d.Finding, &finding); err != nil || finding.Signal.Kind != "unmatched" || len(finding.Signal.Spellings) == 0 {
		return 0, 0, fmt.Errorf("the fill's suggestion names no relationship to fill: %v", err)
	}
	// A fresh collect ends what one that stopped partway left owed.
	switch ok, err := r.db.OweFoundryFillDocuments(ctx, d.Fill, d.Due.WorkspaceID, nil, true); {
	case err != nil:
		return 0, 0, err
	case !ok:
		return 0, 0, errFillChanged
	}
	if !slices.Contains(d.Fill.LinksAdded, finding.Signal.Canonical) {
		return 0, 0, nil
	}
	info, err := r.agent.Info(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("the ontology agent is unavailable: %w", err)
	}
	if info.GetContractVersion() != ontologyagent.ContractVersion {
		return 0, 0, fmt.Errorf("the ontology agent speaks contract %s, not %s", info.GetContractVersion(), ontologyagent.ContractVersion)
	}
	load := d.Due.Load
	version, err := r.db.GetOntologyVersion(ctx, d.Fill.OntologyID, load.VersionID)
	if err != nil {
		return 0, 0, fmt.Errorf("read the load's version: %w", err)
	}
	var def ontologyagent.Definition
	if err := json.Unmarshal(version.Definition, &def); err != nil || version.Format != model.OntologyFormatAgentBundle {
		return 0, 0, fmt.Errorf("the load's version is not an ontology agent draft: %v", err)
	}
	t, err := r.open(ctx, d.Due)
	if err != nil {
		return 0, 0, fmt.Errorf("reach the ontology's database: %w", err)
	}
	defer t.close()
	request := ontologyagent.AuditRequest{
		Database: t.database, Load: load.ID, OntologyTOML: def.OntologyTOML, OntologyVersion: load.OntologyVersion(),
		MappingsTOML: def.MappingsTOML, Helix: t.helix,
		DocumentsFor: []ontologyagent.AuditDocumentsFor{{Set: finding.Signal.Set, Spellings: finding.Signal.Spellings}},
	}
	for after, size, page := "", r.pageSize, 0; ; page++ {
		ids, err := r.db.ListFoundryHealDocuments(ctx, load.ID, after, size)
		if err != nil {
			return 0, 0, err
		}
		if len(ids) == 0 {
			return owed, left, nil
		}
		request.RequestID, request.DocIDs = "fill:"+d.Fill.ID+":"+strconv.Itoa(page), ids
		result, err := r.page(ctx, request)
		if err != nil {
			return 0, 0, err
		}
		if len(result.Cut) > 0 {
			if len(ids) <= 1 {
				return 0, 0, errors.New("a page of one document was cut short")
			}
			size = max(1, len(ids)/2)
			continue
		}
		if len(result.DocumentsOf) != 1 {
			return 0, 0, fmt.Errorf("the audit listed %d relationships' documents, not 1", len(result.DocumentsOf))
		}
		docs := result.DocumentsOf[0]
		take := docs[:min(int64(len(docs)), max(0, model.FoundryFillMaxDocuments-owed))]
		if len(take) > 0 {
			switch ok, err := r.db.OweFoundryFillDocuments(ctx, d.Fill, d.Due.WorkspaceID, take, false); {
			case err != nil:
				return 0, 0, err
			case !ok:
				return 0, 0, errFillChanged
			}
		}
		owed, left = owed+int64(len(take)), left+int64(len(docs)-len(take))
		if len(ids) < size {
			return owed, left, nil
		}
		after, size = ids[len(ids)-1], min(r.pageSize, 2*size)
	}
}
