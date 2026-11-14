package foundrygraphworker

import (
	"context"
	"fmt"
	"time"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// maxKindDecisions is the most decisions of one kind an Extract takes, and
// maxExtractDecisions the most in all.
const (
	maxKindDecisions    = model.FoundryExtractKindDecisionsMax
	maxExtractDecisions = model.FoundryExtractAllDecisionsMax
)

// decisions are the load's decisions for docs: every prop decision, every
// alias, and the resolve and release decisions of docs. One the loaded
// version no longer takes (model.FoundryReviewDecision.Extractable: its type
// or prop is gone, or a prop decision's key or value no longer fits) is left
// out, before the caps. A resolve whose chosen entity the type's changed key
// no longer reads goes: the engine counts it stale and reviews its name
// again, where left out the name would be matched another way. Past a cap
// (maxKindDecisions of one kind, maxExtractDecisions in all) the Extract
// fails, too large to send, rather than drop a choice.
//
// An alias (contract 4.22.0) is a resolve of the load's list that chose an
// entity, of any document, sent with every Extract so the name links in
// every document where the names rung would ask a person: one per type, name
// and entity, the earliest decided (store.ListFoundryLoadAliases), with its
// id and time. One whose type is gone, or whose entity the type's key no
// longer reads (model.FoundryReviewDecision.Fits), is left out: the engine
// would only count it stale. No alias is stored. The list is what the load holds
// now: the choices the ontology held when the load was created, and the
// approvals and value decisions settled against it or carried to it since
// (store.SettleFoundryReleases, store.SettleFoundryValue), and the withdraws
// of approvals taken back that wait (store.DeleteFoundryReviewDecision); a
// name settled after the load started waits for the next load. Each goes with its id and
// when it was settled (foundry_load_decisions.decided_at), so the load tells
// when a value was decided; one without that time fails the job rather than
// go as decided in year 1.
func (r *runner) decisions(ctx context.Context, job *model.FoundryGraphJob, ontologyTOML string, docs []ontologyagent.ExtractDocument) ([]ontologyagent.ExtractDecision, error) {
	types, err := model.FoundryDecisionTypes(ontologyTOML)
	if err != nil {
		return nil, permanent("the version has invalid ontology TOML", err)
	}
	ids := make([]string, 0, len(docs))
	sent := map[string]bool{}
	for _, d := range docs {
		ids = append(ids, d.DocID)
		sent[d.DocID] = true
	}
	var out []ontologyagent.ExtractDecision
	// kinds counts the decisions listed so far of each kind.
	kinds := map[model.FoundryReviewDecisionKind]int{}
	add := func(d ontologyagent.ExtractDecision) error {
		out = append(out, d)
		if kinds[d.Kind]++; kinds[d.Kind] > maxKindDecisions {
			return tooLarge(fmt.Sprintf("this extract needs more than %d review decisions of one kind; reduce its documents or decisions", maxKindDecisions), nil)
		}
		if len(out) > maxExtractDecisions {
			return tooLarge(fmt.Sprintf("this extract needs more than %d review decisions; reduce its documents or decisions", maxExtractDecisions), nil)
		}
		return nil
	}
	// claims are the releases and withdraws listed so far, by id.
	claims := map[string]bool{}
	after, stale := "", 0
	for {
		page, err := r.db.ListFoundryLoadDecisions(ctx, job.LoadID, after, ids, 1000)
		if err != nil {
			return nil, retry("could not read review decisions; Helix will retry", err)
		}
		for _, d := range page {
			if !d.Extractable(types) {
				stale++
				continue
			}
			// decided_at is NOT NULL: a zero time is a broken row, not a time to send.
			if d.DecidedAt.IsZero() {
				return nil, permanent(fmt.Sprintf("review decision %s has no decision time", d.ID), nil)
			}
			decision := ontologyagent.ExtractDecision{Kind: d.Kind, Type: d.Type, EntityKey: d.EntityKey, DecisionID: d.ID, DecidedAt: d.DecidedAt.UTC()}
			switch d.Kind {
			case model.FoundryReviewResolve:
				if !sent[*d.DocID] {
					continue
				}
				decision.DocID, decision.NormName = *d.DocID, *d.NormName
			case model.FoundryReviewProp:
				decision.Prop, decision.Value = *d.Prop, d.Value
			case model.FoundryReviewReject:
				// No load's list holds one: it is a person's word to the
				// site, and the engine knows no such decision.
				continue
			case model.FoundryReviewRelease, model.FoundryReviewWithdraw:
				// A withdraw is the release as it was sent (contract 4.16.0),
				// time included: the engine takes back what that decision
				// wrote. One id is one decision, and the engine refuses a
				// request naming one as both; the list holds one row an id.
				if !sent[*d.DocID] {
					continue
				}
				if claims[d.ID] {
					return nil, permanent(fmt.Sprintf("review decision %s is listed twice", d.ID), nil)
				}
				claims[d.ID] = true
				decision.DocID, decision.ClaimKey, decision.StatementHash = *d.DocID, *d.ClaimKey, *d.StatementHash
				decision.Section, decision.Start, decision.End = d.ClaimSpan.Section, d.ClaimSpan.Start, d.ClaimSpan.End
				// From contract 4.14.0 the last decided of a link's approvals
				// is its decision, on a link that holds one value, and an
				// undated quote's decision takes effect from when it was
				// decided. The engine keeps a written
				// approval's time to the millisecond and ranks the one it
				// writes by the time sent, so sent to the millisecond two
				// approvals rank alike whichever is written first.
				decision.DecidedAt = decision.DecidedAt.Truncate(time.Millisecond)
			}
			if err := add(decision); err != nil {
				return nil, err
			}
		}
		if len(page) < 1000 {
			break
		}
		next := page[len(page)-1].ID
		if next <= after {
			return nil, permanent("review decision paging did not advance", nil)
		}
		after = next
	}
	aliases := 0
	for after = ""; ; {
		page, err := r.db.ListFoundryLoadAliases(ctx, job.LoadID, after, 1000)
		if err != nil {
			return nil, retry("could not read review decisions; Helix will retry", err)
		}
		for _, d := range page {
			if d.Kind != model.FoundryReviewResolve || d.EntityKey == nil || !d.Fits(types) {
				stale++
				continue
			}
			if d.DecidedAt.IsZero() {
				return nil, permanent(fmt.Sprintf("review decision %s has no decision time", d.ID), nil)
			}
			// An alias may take its resolve's id: the engine holds ids apart
			// only among releases and withdraws.
			if err := add(ontologyagent.ExtractDecision{Kind: ontologyagent.AliasDecision, Type: d.Type, NormName: *d.NormName, EntityKey: d.EntityKey,
				DecisionID: d.ID, DecidedAt: d.DecidedAt.UTC()}); err != nil {
				return nil, err
			}
			aliases++
		}
		if len(page) < 1000 {
			break
		}
		next := page[len(page)-1].ID
		if next <= after {
			return nil, permanent("alias paging did not advance", nil)
		}
		after = next
	}
	if stale > 0 {
		log.From(ctx).Info("graph job: left out review decisions the version no longer takes", "job", job.ID, "count", stale)
	}
	if aliases > 0 {
		log.From(ctx).Info("graph job: sends aliases of settled names", "job", job.ID, "load", job.LoadID, "count", aliases)
	}
	return out, nil
}
