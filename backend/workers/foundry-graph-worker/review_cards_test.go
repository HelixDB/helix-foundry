package foundrygraphworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	parked, loadBackOffs   int
	requested, advanced    int
	recounted, cleared     int
	claimDecisionDocuments []string
	// next is each database's next read slot, shared by every indexer of
	// this store.
	next map[string]time.Time
}

const reviewLoad = "load-1"

func newReviewMem(s *memStore, docs ...string) *reviewMem {
	return &reviewMem{memStore: s, owed: docs, attempts: map[string]int{}, stored: map[string]*model.FoundryReviewIndexed{}, next: map[string]time.Time{}}
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
func (m *reviewMem) ParkFoundryReviewLoad(context.Context, string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.parked++
	m.owed = nil
	return nil
}

// ReserveFoundryReviewRead takes the database's next slot, as the store's
// row does.
func (m *reviewMem) ReserveFoundryReviewRead(_ context.Context, tenant string, gap time.Duration) (time.Duration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	slot := m.next[tenant]
	if slot.Before(now) {
		slot = now
	}
	m.next[tenant] = slot.Add(gap)
	return slot.Sub(now), nil
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
// fails with its status (500 the database's failure, 400 the document's),
// and status, when set, answers every read.
type reviewGraph struct {
	mu      sync.Mutex
	holds   map[string]bool
	bad     map[string]int
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
		if code := g.bad[d]; code != 0 {
			return helixgateway.Response{StatusCode: code, Body: []byte(`boom`)}, nil
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
	g := &reviewGraph{holds: map[string]bool{}, bad: map[string]int{}}
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
// only that one backs off, until it is set aside, when the failure is its
// own (here a 400).
func TestIndexerSplitsAFailingRead(t *testing.T) {
	x, mem, g := newReviewIndexer(t, "doc-a", "doc-b", "doc-c", "doc-d")
	g.bad["doc-c"] = http.StatusBadRequest
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

// The database failing a document's read (a 5xx) charges the document only
// beside one that read fine: alone, it backs the load off instead.
func TestIndexerChargesTheDatabaseOnlyBesideAGoodRead(t *testing.T) {
	x, mem, g := newReviewIndexer(t, "doc-a", "doc-b", "doc-c")
	g.bad["doc-b"] = http.StatusInternalServerError
	// The tick reads all three (b charged beside a and c), then b again
	// alone: its 500 is the database's as much as its own, so the load
	// backs off and b isn't charged again.
	x.tick(context.Background())
	if mem.stored["doc-a"] == nil || mem.stored["doc-c"] == nil || mem.stored["doc-b"] != nil {
		t.Fatalf("stored %v", mem.stored)
	}
	if mem.attempts["doc-b"] != 1 || mem.loadBackOffs != 1 {
		t.Fatalf("%d attempts, %d load back-offs", mem.attempts["doc-b"], mem.loadBackOffs)
	}
}

// A database that fails every read (an outage: transport errors, 401, 403,
// 5xx) backs the load off and charges no document, so an outage never sets
// a load's documents aside, and it costs a read or two a level of the
// split, not one a document.
func TestIndexerOutageChargesNoDocument(t *testing.T) {
	for _, code := range []int{http.StatusBadGateway, http.StatusInternalServerError, http.StatusUnauthorized, http.StatusForbidden} {
		docs := make([]string, 20)
		for i := range docs {
			docs[i] = fmt.Sprintf("doc-%02d", i)
		}
		x, mem, g := newReviewIndexer(t, docs...)
		g.status = code
		x.tick(context.Background())
		if len(mem.attempts) != 0 || mem.loadBackOffs != 1 || len(mem.stored) != 0 {
			t.Fatalf("%d: attempts %v, %d load back-offs, %d stored", code, mem.attempts, mem.loadBackOffs, len(mem.stored))
		}
		if len(g.reads) > 10 {
			t.Fatalf("%d: an outage cost %d reads of 20 documents", code, len(g.reads))
		}
	}
	// A transport error too.
	x, mem, _ := newReviewIndexer(t, "doc-a", "doc-b")
	x.gateway = failingGateway{}
	x.tick(context.Background())
	if len(mem.attempts) != 0 || mem.loadBackOffs != 1 {
		t.Fatalf("transport: attempts %v, %d load back-offs", mem.attempts, mem.loadBackOffs)
	}
}

// failingGateway fails every read before it reaches the database.
type failingGateway struct{}

func (failingGateway) Query(context.Context, string, string, []byte, http.Header) (helixgateway.Response, error) {
	return helixgateway.Response{}, errors.New("connection refused")
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
	if mem.parked != 1 || len(g.reads) != 0 {
		t.Fatalf("a deleted ontology: %d parked, %d reads", mem.parked, len(g.reads))
	}

	x, mem, _ = newReviewIndexer(t, "doc-a")
	mem.leaseLost = true
	x.tick(context.Background())
	if len(mem.stored) != 0 {
		t.Fatal("a store past its lease wrote")
	}
}

// Reads of one database keep readGap apart, across every indexer (replica)
// of one store.
func TestIndexerPacesReads(t *testing.T) {
	x, mem, g := newReviewIndexer(t)
	x.readGap = 30 * time.Millisecond
	other := newIndexer(mem, nil, g, "w2")
	other.readGap = x.readGap
	start := time.Now()
	for _, by := range []*indexer{x, other, x} {
		if !by.pace(context.Background(), "tenant-1") {
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
