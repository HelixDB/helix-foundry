package foundryhealworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// fills acts on the fills due (store.DueFoundryHealFills): a load moved in
// place by a suggestion's "Add this link" has the documents behind the
// suggestion's reviews collected (collect) and owed a fill of the link; one
// whose fill paused, or waits with no job live, is resumed. One at a time,
// the one with the least support first, as a load fills one suggestion at a
// time. A collect that fails is tried again at the next tick: it owes
// documents again, which costs nothing.
func (r *runner) fills(ctx context.Context) {
	due, err := r.db.DueFoundryHealFills(ctx, dueBatch)
	if err != nil {
		log.From(ctx).Error("find fills due failed", "err", err)
		return
	}
	for _, d := range due {
		if ctx.Err() != nil {
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
		owed, left, err := r.collect(ctx, d)
		switch {
		case errors.Is(err, errFillChanged):
			fillRuns.WithLabelValues("changed").Inc()
			continue
		case err != nil:
			fillRuns.WithLabelValues("failed").Inc()
			logger.Warn("collect fill failed", "err", err)
			continue
		}
		switch err := r.db.StartFoundryFill(ctx, d.Fill, d.Due.WorkspaceID, owed, left, &model.FoundryGraphJob{ID: r.newID()}); {
		case errors.Is(err, store.ErrConflict):
			fillRuns.WithLabelValues("changed").Inc()
		case err != nil:
			fillRuns.WithLabelValues("failed").Inc()
			logger.Error("start fill failed", "err", err)
		default:
			fillRuns.WithLabelValues("started").Inc()
			logger.Info("fill started", "owed", owed, "left", left)
		}
	}
}

// errFillChanged is a fill, or its load, that changed while it was
// collected: nothing more is done of it.
var errFillChanged = errors.New("the fill or its load changed")

// collect pages Audit over d's load with documents_for the suggestion's
// relationship (its set and spellings, from the finding as stored), and owes
// the load a fill of each document it lists (store.OweFoundryFillDocuments),
// page by page, so no list of them is held: owed is how many, left those
// past model.FoundryFillMaxDocuments. The pages are the load's documents
// with reviews, as an audit's; a page the agent's wall cuts is read again at
// half the size.
func (r *runner) collect(ctx context.Context, d model.FoundryFillDue) (owed, left int64, err error) {
	var finding struct {
		Signal struct {
			Kind      string   `json:"kind"`
			Set       string   `json:"set"`
			Spellings []string `json:"spellings"`
		} `json:"signal"`
	}
	if err := json.Unmarshal(d.Finding, &finding); err != nil || finding.Signal.Kind != "unmatched" || len(finding.Signal.Spellings) == 0 {
		return 0, 0, fmt.Errorf("the fill's suggestion names no relationship to fill: %v", err)
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
			switch ok, err := r.db.OweFoundryFillDocuments(ctx, d.Fill, d.Due.WorkspaceID, take); {
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
