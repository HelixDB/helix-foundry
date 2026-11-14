package api

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// loadDocumentSetsTTL is how long a busy load's document counts are kept.
// The load's page polls every second or two while a job runs, an MCP wait
// about as often, and counting reads three rows a document.
const loadDocumentSetsTTL = 5 * time.Second

// loadDocumentSets keeps what busy loads made of their documents for
// loadDocumentSetsTTL, by load id. Only a load with a live job is kept: once
// its jobs end it is counted afresh, so the counts a caller reads last are
// the final ones. It holds only the loads counted within the last
// loadDocumentSetsTTL. The zero value is ready to use.
type loadDocumentSets struct {
	// now is time.Now when nil.
	now  func() time.Time
	mu   sync.Mutex
	kept map[string]keptLoadDocumentSets
}

type keptLoadDocumentSets struct {
	at   time.Time
	sets []*foundryv1.FoundryLoadDocumentSet
}

// get returns the load's kept counts, if it has any that are still fresh.
func (c *loadDocumentSets) get(loadID string) ([]*foundryv1.FoundryLoadDocumentSet, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept, ok := c.kept[loadID]
	if !ok || c.clock().Sub(kept.at) >= loadDocumentSetsTTL {
		return nil, false
	}
	return kept.sets, true
}

// put keeps the load's counts, and drops every load's that are stale.
func (c *loadDocumentSets) put(loadID string, sets []*foundryv1.FoundryLoadDocumentSet) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	if c.kept == nil {
		c.kept = map[string]keptLoadDocumentSets{}
	}
	maps.DeleteFunc(c.kept, func(_ string, kept keptLoadDocumentSets) bool { return now.Sub(kept.at) >= loadDocumentSetsTTL })
	c.kept[loadID] = keptLoadDocumentSets{at: now, sets: sets}
}

func (c *loadDocumentSets) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}

// foundryLoadDocumentSets is what the load made of its documents, by set
// (ListFoundryLoadDocumentSets), each saying whether an extraction route of
// the load's version reads it: the count of what no route read is what a
// caller can't see in the jobs' progress, which counts a stored document as
// done.
//   - While a job of the load is live, the counts are kept for
//     loadDocumentSetsTTL (loadDocumentSets).
//   - A version whose mappings can't be read, or whose routes can't, still
//     has its documents counted: a set is then routed exactly when the load
//     read a document of it. That is logged, not refused.
//   - A load with no documents reads no mappings.
func (s *Service) foundryLoadDocumentSets(ctx context.Context, ontologyID string, load *foundryv1.FoundryLoad) ([]*foundryv1.FoundryLoadDocumentSet, error) {
	busy := slices.ContainsFunc(load.GetJobs(), func(job *foundryv1.FoundryGraphJob) bool {
		return foundrymodel.FoundryGraphJobStatus(job.GetStatus()).Live()
	})
	if busy {
		if kept, ok := s.loadDocuments.get(load.GetId()); ok {
			return kept, nil
		}
	}
	sets, err := s.deps.DB.ListFoundryLoadDocumentSets(ctx, load.GetId())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "count load documents: %v", err)
	}
	var out []*foundryv1.FoundryLoadDocumentSet
	if len(sets) > 0 {
		var routes ontologyagent.DocumentRoutes
		mappings, err := s.deps.DB.GetOntologyVersionMappings(ctx, ontologyID, load.GetVersionId())
		if err == nil {
			routes, err = ontologyagent.ParseDocumentRoutes(mappings)
		}
		if err != nil {
			log.From(ctx).Warn("read document routes failed", "ontology", ontologyID, "version", load.GetVersionId(), "err", err)
		}
		out = make([]*foundryv1.FoundryLoadDocumentSet, 0, len(sets))
		for _, set := range sets {
			out = append(out, &foundryv1.FoundryLoadDocumentSet{
				SetId: set.SetID, Routed: routes.Routes(set.SetID) || err != nil && set.Read > 0,
				Documents: int32(set.Documents), Read: int32(set.Read),
				Duplicate: int32(set.Duplicate), Failed: int32(set.Failed), NotRead: int32(set.NotRead), Waiting: int32(set.Waiting),
				Facts: set.Facts, Attributes: set.Attributes, Reviews: set.Reviews, Unresolved: set.Unresolved,
			})
		}
	}
	if busy {
		s.loadDocuments.put(load.GetId(), out)
	}
	return out, nil
}
