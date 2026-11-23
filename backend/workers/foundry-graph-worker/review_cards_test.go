package foundrygraphworker

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// reviewMem is the index's store: one load's owed documents, and what each
// store and back-off did to them. Its graph lookups are memStore's.
type reviewMem struct {
	*memStore
	mu sync.Mutex
	// owed are the load's owed documents, and attempts their failed reads.
	owed     []string
	attempts map[string]int
	// stored is each document's last store: its cards, or nil once gone.
	stored map[string]*model.FoundryReviewIndexed
	// leaseLost makes every store find its lease lost.
	leaseLost              bool
	dropped, loadBackOffs  int
	requested, advanced    int
	recounted, cleared     int
	claimDecisionDocuments []string
}

const reviewLoad = "load-1"

func newReviewMem(s *memStore, docs ...string) *reviewMem {
	return &reviewMem{memStore: s, owed: docs, attempts: map[string]int{}, stored: map[string]*model.FoundryReviewIndexed{}}
}

func (m *reviewMem) RequestFoundryReviewPasses(context.Context) (int, error) {
	m.requested++
	return 0, nil
}
func (m *reviewMem) AdvanceFoundryReviewPasses(context.Context) (int, error) {
	m.advanced++
	return 0, nil
}
func (m *reviewMem) RecountFoundryReviewLoads(context.Context, int) (int, error) {
	m.recounted++
	return 0, nil
}
func (m *reviewMem) ClearFoundryReviewLoads(context.Context, int) (int, error) {
	m.cleared++
	return 0, nil
}
func (m *reviewMem) DropFoundryReviewLoad(context.Context, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropped++
	m.owed = nil
	return nil
}

// ClaimFoundryReviewDocuments claims every owed document not set aside, 20 at
// most, once: a claim is held until stored or backed off.
func (m *reviewMem) ClaimFoundryReviewDocuments(context.Context, string, time.Duration) ([]model.FoundryReviewClaim, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := model.FoundryReviewClaim{LoadID: reviewLoad, OntologyID: "ont-1", WorkspaceID: "ws-1", VersionID: "ver-3"}
	for _, d := range m.owed {
		if m.attempts[d] < model.FoundryReviewQuarantine && len(c.Owed) < 20 {
			c.Owed = append(c.Owed, model.FoundryReviewOwed{DocID: d, OwedAt: time.Now()})
		}
	}
	m.owed = slices.DeleteFunc(m.owed, func(d string) bool {
		return slices.ContainsFunc(c.Owed, func(o model.FoundryReviewOwed) bool { return o.DocID == d })
	})
	if len(c.Owed) == 0 {
		return nil, nil
	}
	return []model.FoundryReviewClaim{c}, nil
}

func (m *reviewMem) StoreFoundryReviewDocuments(_ context.Context, _ string, c model.FoundryReviewClaim, indexed []model.FoundryReviewIndexed) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.leaseLost {
		return false, nil
	}
	if len(c.Owed) != len(indexed) {
		panic("a store of other documents than its claim's")
	}
	for i := range indexed {
		m.stored[indexed[i].DocID] = &indexed[i]
	}
	return true, nil
}

func (m *reviewMem) BackOffFoundryReviewDocument(_ context.Context, _, _, doc string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.attempts[doc]++
	m.owed = append(m.owed, doc)
	return m.attempts[doc] >= model.FoundryReviewQuarantine, nil
}

func (m *reviewMem) BackOffFoundryReviewLoad(context.Context, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadBackOffs++
	m.owed = nil
	return nil
}

func (m *reviewMem) ListFoundryClaimDecisions(_ context.Context, _ string, docs []string) ([]*model.FoundryReviewDecision, error) {
	m.claimDecisionDocuments = append(m.claimDecisionDocuments, docs...)
	return nil, nil
}

// reviewGraph answers the indexer's reads: each document asked for that
// it holds, with one unresolved name each; a read naming a bad document
// fails, and status, when set, answers every read.
type reviewGraph struct {
	mu      sync.Mutex
	holds   map[string]bool
	bad     map[string]bool
	status  int
	reads   [][]string
	tenants []string
}

func (g *reviewGraph) Query(_ context.Context, _, tenant string, body []byte, _ http.Header) (helixgateway.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var docs []string
	for _, part := range strings.Split(string(body), `{"property":"doc_id"},"right":{"constant":{"string":"`)[1:] {
		docs = append(docs, part[:strings.Index(part, `"`)])
	}
	g.reads = append(g.reads, docs)
	g.tenants = append(g.tenants, tenant)
	if g.status != 0 {
		return helixgateway.Response{StatusCode: g.status, Body: []byte(`{"error":"rate_limit_unavailable"}`)}, nil
	}
	answer := map[string][]map[string]any{"documents": {}, "evidence": {}, "reviews": {}}
	for i, d := range docs {
		if g.bad[d] {
			return helixgateway.Response{StatusCode: 500, Body: []byte(`boom`)}, nil
		}
		if !g.holds[d] {
			continue
		}
		answer["documents"] = append(answer["documents"], map[string]any{"doc_id": d})
		answer["evidence"] = append(answer["evidence"], map[string]any{"$id": 100 + i, "doc_id": d, "confidence": 0.95})
		answer["reviews"] = append(answer["reviews"], map[string]any{"$id": i, "review_key": "r-" + d, "reason": "unresolved", "type": "Customer",
			"doc_id": d, "norm_name": "globex", "section": 0, "start": 1, "end": 5})
	}
	b, _ := json.Marshal(answer)
	return helixgateway.Response{StatusCode: 200, Body: b}, nil
}

func newReviewIndexer(t *testing.T, docs ...string) (*indexer, *reviewMem, *reviewGraph) {
	t.Helper()
	h := newHarness(t)
	mem := newReviewMem(h.store, docs...)
	g := &reviewGraph{holds: map[string]bool{}, bad: map[string]bool{}}
	for _, d := range docs {
		g.holds[d] = true
	}
	x := newIndexer(mem, h.store, g, "w1")
	x.readGap = 0
	return x, mem, g
}

func TestIndexerStoresOwedDocuments(t *testing.T) {
	x, mem, g := newReviewIndexer(t, "doc-a", "doc-b", "doc-gone")
	g.holds["doc-gone"] = false
	x.tick(context.Background())

	if len(g.reads) != 1 || len(g.reads[0]) != 3 || g.tenants[0] != "tenant-1" {
		t.Fatalf("reads = %v of %v, want one of the claim's three documents", g.reads, g.tenants)
	}
	for _, d := range []string{"doc-a", "doc-b"} {
		s := mem.stored[d]
		if s == nil || !s.Present || len(s.Cards) != 1 || s.Cards[0].ReviewKey != "r-"+d || s.Quotes[0] != 1 {
			t.Fatalf("%s stored %+v", d, s)
		}
	}
	if s := mem.stored["doc-gone"]; s == nil || s.Present || len(s.Cards) != 0 {
		t.Fatalf("a document gone from the graph stored %+v", s)
	}
	if mem.requested != 1 || mem.advanced != 1 || mem.recounted != 1 || mem.cleared != 1 {
		t.Fatalf("housekeeping ran request %d, advance %d, recount %d, clear %d", mem.requested, mem.advanced, mem.recounted, mem.cleared)
	}
	// Within the minute, a tick advances and recounts but asks nothing.
	x.tick(context.Background())
	if mem.requested != 1 || mem.cleared != 1 || mem.advanced != 2 || mem.recounted != 2 {
		t.Fatalf("a second tick ran request %d, advance %d, recount %d, clear %d", mem.requested, mem.advanced, mem.recounted, mem.cleared)
	}
}

// A read that fails splits down to one document: the rest are stored, and
// only that one backs off, until it is set aside.
func TestIndexerSplitsAFailingRead(t *testing.T) {
	x, mem, g := newReviewIndexer(t, "doc-a", "doc-b", "doc-c", "doc-d")
	g.bad["doc-c"] = true
	for range model.FoundryReviewQuarantine + 2 {
		x.tick(context.Background())
	}
	for _, d := range []string{"doc-a", "doc-b", "doc-d"} {
		if mem.stored[d] == nil {
			t.Fatalf("%s wasn't stored beside a failing document", d)
		}
	}
	if mem.stored["doc-c"] != nil || mem.attempts["doc-c"] != model.FoundryReviewQuarantine {
		t.Fatalf("the failing document stored %v after %d attempts", mem.stored["doc-c"], mem.attempts["doc-c"])
	}
	if mem.loadBackOffs != 0 {
		t.Fatal("one document's failure backed the load off")
	}
	// The first read of all four, then halves, then quarters: a, b and d
	// are read once each in a read that worked.
	if got := len(g.reads); got < 4 {
		t.Fatalf("reads %v", g.reads)
	}
}

// A database still starting, rate limited or paused backs the load off,
// one whose ontology or database is gone drops the load's index, and a
// store whose lease was lost writes nothing.
func TestIndexerBacksOffAndDrops(t *testing.T) {
	x, mem, g := newReviewIndexer(t, "doc-a")
	g.status = http.StatusServiceUnavailable
	x.tick(context.Background())
	if mem.loadBackOffs != 1 || len(mem.stored) != 0 || mem.attempts["doc-a"] != 0 {
		t.Fatalf("a starting database: %d back-offs, %d stored, %d document attempts", mem.loadBackOffs, len(mem.stored), mem.attempts["doc-a"])
	}

	x, mem, _ = newReviewIndexer(t, "doc-a")
	mem.gate = "queries are paused"
	x.tick(context.Background())
	if mem.loadBackOffs != 1 || len(mem.stored) != 0 {
		t.Fatalf("a paused database: %d back-offs, %d stored", mem.loadBackOffs, len(mem.stored))
	}

	x, mem, g = newReviewIndexer(t, "doc-a")
	mem.ontology = nil
	x.tick(context.Background())
	if mem.dropped != 1 || len(g.reads) != 0 {
		t.Fatalf("a deleted ontology: %d drops, %d reads", mem.dropped, len(g.reads))
	}

	x, mem, _ = newReviewIndexer(t, "doc-a")
	mem.leaseLost = true
	x.tick(context.Background())
	if len(mem.stored) != 0 {
		t.Fatal("a store past its lease wrote")
	}
}

// Reads of one database keep readGap apart.
func TestIndexerPacesReads(t *testing.T) {
	x, _, _ := newReviewIndexer(t)
	x.readGap = 30 * time.Millisecond
	start := time.Now()
	for range 3 {
		if !x.pace(context.Background(), "tenant-1") {
			t.Fatal("pace ended early")
		}
	}
	if took := time.Since(start); took < 60*time.Millisecond {
		t.Fatalf("three reads took %v, want at least two gaps", took)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if x.pace(ctx, "tenant-1") {
		t.Fatal("a pace whose context ended went on")
	}
}

// An attempt that sent an Extract, or an apply that changed rows, owes the
// load's cards a full pass; an apply that changed nothing doesn't.
func TestReindexReviews(t *testing.T) {
	for name, c := range map[string]struct {
		a    attempt
		want bool
	}{
		"an extract":                    {attempt{sentExtract: true}, true},
		"an apply that changed rows":    {attempt{applied: []ontologyagent.AppliedStream{{Changed: 1}}}, true},
		"an apply that changed nothing": {attempt{applied: []ontologyagent.AppliedStream{{}}}, false},
		"nothing sent":                  {attempt{}, false},
	} {
		if got := c.a.reindexReviews(); got != c.want {
			t.Errorf("%s: reindexReviews = %t", name, got)
		}
	}
}
