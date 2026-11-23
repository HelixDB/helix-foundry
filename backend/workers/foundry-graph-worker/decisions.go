package foundrygraphworker

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
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

// decisions are the load's decisions for docs: every prop decision, the
// aliases docs write, and the resolve and release decisions of docs. One the
// loaded version no longer takes (model.FoundryReviewDecision.Extractable:
// its type or prop is gone, or a prop decision's key or value no longer
// fits) is left out, before the caps. A resolve whose chosen entity the
// type's changed key no longer reads goes: the engine counts it stale and
// reviews its name again, where left out the name would be matched another
// way. Past a cap (maxKindDecisions of one kind, maxExtractDecisions in all)
// the Extract is too large to send, rather than drop a choice: sendSplit
// sends its documents in smaller Extracts.
//
// An alias (contract 4.22.0) is a resolve of the load's list that chose an
// entity, of any document, which links the name in every document where the
// names rung would ask a person. From contract 4.23.0 one acts only on a
// document whose text writes its name, so an Extract sends those its
// documents write (a.aliases, found as each was staged: documentAliases),
// and the run is what every alias of the load would give. One per type, name
// and entity, the earliest decided, the smallest id among those decided at
// once, so every Extract sends the same alias, with its id and time, in id
// order; a name chosen as two entities gives both, and the engine links
// neither. One whose type is gone, or whose entity the type's key no longer
// reads (model.FoundryReviewDecision.Fits), is left out: the engine would
// only count it stale. No alias is stored. The list is what the load holds
// now: the choices the ontology held when the load was created, and the
// approvals and value decisions settled against it or carried to it since
// (store.SettleFoundryReviews), and the withdraws
// of approvals taken back that wait (store.DeleteFoundryReviewDecision); a
// name settled after the load started waits for the next load. Each goes with its id and
// when it was settled (foundry_load_decisions.decided_at), so the load tells
// when a value was decided; one without that time fails the job rather than
// go as decided in year 1.
func (r *runner) decisions(ctx context.Context, job *model.FoundryGraphJob, ontologyTOML string, docs []ontologyagent.ExtractDocument,
	a *attempt) ([]ontologyagent.ExtractDecision, error) {
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
	// The earliest decided of each type, name and entity the documents write.
	earliest := map[[3]string]*model.FoundryReviewDecision{}
	for _, doc := range docs {
		for _, d := range a.aliases[doc.DocID] {
			k := [3]string{d.Type, *d.NormName, *d.EntityKey}
			if e := earliest[k]; e == nil || d.DecidedAt.Before(e.DecidedAt) || d.DecidedAt.Equal(e.DecidedAt) && d.ID < e.ID {
				earliest[k] = d
			}
		}
	}
	aliases := slices.SortedFunc(maps.Values(earliest), func(x, y *model.FoundryReviewDecision) int { return strings.Compare(x.ID, y.ID) })
	for _, d := range aliases {
		if !d.Fits(types) {
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
	}
	if stale > 0 {
		log.From(ctx).Info("graph job: left out review decisions the version no longer takes", "job", job.ID, "count", stale)
	}
	if len(aliases) > 0 {
		log.From(ctx).Info("graph job: sends aliases its documents write", "job", job.ID, "load", job.LoadID, "count", len(aliases))
	}
	return out, nil
}
