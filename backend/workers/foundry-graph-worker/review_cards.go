package foundrygraphworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/graphproxy"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/pkg/helixgateway"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/reviewcards"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The review cards indexer: beside the jobs, every PollInterval, it keeps
// the index of what each live load asks a person to decide
// (store/foundry_review_cards.go) in step with the loads' graphs. Each tick
// it asks for and advances full passes, recounts loads whose counts are
// stale and clears loads no longer live, then claims owed documents (a
// lease, SKIP LOCKED, so replicas split them) until none is due, at most
// reviewLoadsInFlight loads (databases) at a time. It reads a load's
// claimed documents in one gateway read as the control plane, builds their
// cards (reviewcards.Build) and stores them while its lease holds.
//
// A database takes one read a second (the site's tenant limiter), and the
// boost covers job attempts only: the indexer reads each database at most
// once every reviewReadGap, half of that, so people keep the rest. The pace
// is shared by every replica (store.ReserveFoundryReviewRead), so more
// replicas read more databases at once, never one database more often.
//
// A read the gateway turned away (rate limited, a database still starting,
// paused) backs the whole load off. Any other failed read is split in half,
// down to one document, but a document is charged with it (backs off, and
// is set aside after model.FoundryReviewQuarantine failures) only where the
// failure is its own: its answer was too large or unreadable, or the
// database failed it (transport, 401, 403, 5xx) while another document of
// the claim read fine. A database that fails every read backs the load off
// and charges no document, after a read or two a level of the split. A load
// whose ontology or database is gone is parked for an hour.

const (
	// reviewLoadsInFlight is how many loads' claims a tick reads at once.
	reviewLoadsInFlight = 4
	// reviewLease is how long a claim of owed documents is held.
	reviewLease = 2 * time.Minute
	// reviewReadGap is the least time between two reads of one database.
	reviewReadGap = 2 * time.Second
	// reviewHousekeeping is how often the indexer asks for passes of loads
	// that lack an index, or whose index is old, and clears loads no longer
	// live: each reads every live ontology.
	reviewHousekeeping = time.Minute
	// reviewRecounts is how many stale counts a tick recounts at most.
	reviewRecounts = 50
	// reviewClears is how many dead loads a housekeeping clears at most.
	reviewClears = 20
)

var reviewIndexDocuments = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_review_index_documents_total",
	Help: "Documents the review cards indexer handled, by outcome: indexed, removed (gone from the graph), invalid (reviews dropped " +
		"for a field no column holds), failed (its read failed on its own; it backs off), quarantined (set aside after repeated " +
		"failures) or lease_lost (another worker may store a newer read).",
}, []string{"outcome"})

var reviewIndexOwedAge = promauto.NewHistogram(prometheus.HistogramOpts{
	Name:    "helix_foundry_review_index_owed_age_seconds",
	Help:    "How long a document's review cards were owed before they were stored.",
	Buckets: []float64{1, 5, 15, 60, 300, 900, 3600, 4 * 3600, 24 * 3600},
})

var reviewIndexFullPasses = promauto.NewCounter(prometheus.CounterOpts{
	Name: "helix_foundry_review_index_full_passes_total",
	Help: "Full passes of a load's review cards started: a load never indexed, of an older builder, or asked for again.",
})

// reviewStore is what the indexer reads and writes of Foundry's store.
type reviewStore interface {
	graphproxy.Store
	GetOntologyVersion(ctx context.Context, ontologyID, id string) (*model.OntologyVersion, error)
	RequestFoundryReviewPasses(ctx context.Context) (int, error)
	AdvanceFoundryReviewPasses(ctx context.Context) (int, error)
	RecountFoundryReviewLoads(ctx context.Context, limit int) (int, error)
	ClearFoundryReviewLoads(ctx context.Context, limit int) (int, error)
	ParkFoundryReviewLoad(ctx context.Context, loadID string) error
	ReserveFoundryReviewRead(ctx context.Context, tenantID string, gap time.Duration) (time.Duration, error)
	ClaimFoundryReviewDocuments(ctx context.Context, workerID string, lease time.Duration) ([]model.FoundryReviewClaim, error)
	StoreFoundryReviewDocuments(ctx context.Context, workerID string, claim model.FoundryReviewClaim, indexed []model.FoundryReviewIndexed) (bool, error)
	BackOffFoundryReviewDocument(ctx context.Context, workerID, loadID, docID string) (bool, error)
	BackOffFoundryReviewLoad(ctx context.Context, loadID string) error
	ListFoundryClaimDecisions(ctx context.Context, ontologyID string, docIDs []string) ([]*model.FoundryReviewDecision, error)
}

// indexer keeps the review cards index; one per worker.
type indexer struct {
	db       reviewStore
	dbs      ports.Databases
	gateway  graphproxy.Querier
	workerID string
	now      func() time.Time
	// readGap is the least time between two reads of one database.
	readGap time.Duration
	// housekept is when the last housekeeping ran.
	housekept time.Time
}

func newIndexer(db reviewStore, dbs ports.Databases, gateway graphproxy.Querier, workerID string) *indexer {
	return &indexer{db: db, dbs: dbs, gateway: gateway, workerID: workerID, now: time.Now, readGap: reviewReadGap}
}

// tick runs one round: housekeeping, then claims until none is due.
func (x *indexer) tick(ctx context.Context) {
	if err := x.housekeep(ctx); err != nil {
		log.From(ctx).Warn("review cards: housekeeping failed", "err", err)
	}
	for ctx.Err() == nil {
		claims, err := x.db.ClaimFoundryReviewDocuments(ctx, x.workerID, reviewLease)
		if err != nil {
			log.From(ctx).Warn("review cards: claim failed", "err", err)
			return
		}
		if len(claims) == 0 {
			return
		}
		var wg sync.WaitGroup
		for _, c := range claims[:min(len(claims), reviewLoadsInFlight)] {
			wg.Add(1)
			go func() {
				defer wg.Done()
				x.index(ctx, c)
			}()
		}
		wg.Wait()
	}
}

// housekeep advances passes and recounts stale counts every tick, and, every
// reviewHousekeeping, asks for the passes owed and clears dead loads.
func (x *indexer) housekeep(ctx context.Context) error {
	if x.now().Sub(x.housekept) >= reviewHousekeeping {
		if _, err := x.db.RequestFoundryReviewPasses(ctx); err != nil {
			return fmt.Errorf("request passes: %w", err)
		}
		if _, err := x.db.ClearFoundryReviewLoads(ctx, reviewClears); err != nil {
			return fmt.Errorf("clear loads: %w", err)
		}
		x.housekept = x.now()
	}
	started, err := x.db.AdvanceFoundryReviewPasses(ctx)
	if err != nil {
		return fmt.Errorf("advance passes: %w", err)
	}
	reviewIndexFullPasses.Add(float64(started))
	if _, err := x.db.RecountFoundryReviewLoads(ctx, reviewRecounts); err != nil {
		return fmt.Errorf("recount loads: %w", err)
	}
	return nil
}

// index reads, builds and stores one claim's documents.
func (x *indexer) index(ctx context.Context, c model.FoundryReviewClaim) {
	logger := log.From(ctx).With("load", c.LoadID, "ontology", c.OntologyID)
	binding, err := graphproxy.BindRead(ctx, x.db, x.dbs, c.WorkspaceID, c.OntologyID)
	switch status.Code(err) {
	case codes.OK:
	case codes.NotFound:
		logger.Info("review cards: the load's ontology or database is gone; parking its index for an hour", "err", err)
		if err := x.db.ParkFoundryReviewLoad(ctx, c.LoadID); err != nil {
			logger.Warn("review cards: park failed", "err", err)
		}
		return
	default:
		logger.Info("review cards: the load's database can't be read now; backing off", "err", err)
		x.backOffLoad(ctx, c.LoadID)
		return
	}
	single, err := x.singleLinks(ctx, c)
	if err != nil {
		logger.Warn("review cards: could not read the load's version; backing off", "err", err)
		x.backOffLoad(ctx, c.LoadID)
		return
	}
	saved, err := x.db.ListFoundryClaimDecisions(ctx, c.OntologyID, c.DocIDs())
	if err != nil {
		logger.Warn("review cards: could not read the documents' decisions", "err", err)
		return
	}
	decisions := map[string][]reviewcards.ClaimDecision{}
	for _, d := range saved {
		if d.DocID == nil || d.ClaimSpan == nil || d.ClaimKey == nil || d.StatementHash == nil {
			continue
		}
		decisions[*d.DocID] = append(decisions[*d.DocID], reviewcards.ClaimDecision{Kind: string(d.Kind), Section: d.ClaimSpan.Section,
			Start: d.ClaimSpan.Start, End: d.ClaimSpan.End, ClaimKey: *d.ClaimKey, StatementHash: *d.StatementHash})
	}
	r := x.read(ctx, binding, c, c.DocIDs(), false)
	if r.stopped {
		return
	}
	// The database's failures count against a document only beside a
	// document that read fine; with none, the database failed, not them.
	theirs := len(r.read) > 0 && !r.down
	backOff := r.refused || r.down
	for doc, f := range r.failed {
		if f.database && !theirs {
			backOff = true
			continue
		}
		x.backOffDocument(ctx, c.LoadID, doc, f.err)
	}
	if backOff {
		// After the store: a load's back-off ends every claim of it, this
		// one's lease too.
		logger.Info("review cards: the database failed the claim's reads; backing the load off")
		defer x.backOffLoad(ctx, c.LoadID)
	}
	read := r.read
	if len(read) == 0 {
		return
	}
	var stored model.FoundryReviewClaim
	stored.LoadID, stored.OntologyID, stored.WorkspaceID, stored.VersionID = c.LoadID, c.OntologyID, c.WorkspaceID, c.VersionID
	var indexed []model.FoundryReviewIndexed
	outcomes := map[string]int{}
	for _, o := range c.Owed {
		r, ok := read[o.DocID]
		if !ok {
			continue
		}
		stored.Owed = append(stored.Owed, o)
		doc := model.FoundryReviewIndexed{DocID: o.DocID, Present: r.Present[o.DocID]}
		if !doc.Present {
			outcomes["removed"]++
		} else {
			built := reviewcards.Build(c.LoadID, o.DocID, r.Reviews[o.DocID], decisions[o.DocID], single)
			doc.Cards, doc.Quotes = built.Cards, *r.Quotes[o.DocID]
			outcomes["indexed"]++
			outcomes["invalid"] += built.Invalid
		}
		indexed = append(indexed, doc)
	}
	ok, err := x.db.StoreFoundryReviewDocuments(ctx, x.workerID, stored, indexed)
	switch {
	case err != nil:
		logger.Warn("review cards: store failed; the lease lapses and another claim reads them again", "err", err)
		return
	case !ok:
		reviewIndexDocuments.WithLabelValues("lease_lost").Add(float64(len(stored.Owed)))
		logger.Info("review cards: the claim's lease was lost before its store", "documents", len(stored.Owed))
		return
	}
	for outcome, n := range outcomes {
		reviewIndexDocuments.WithLabelValues(outcome).Add(float64(n))
	}
	for _, o := range stored.Owed {
		reviewIndexOwedAge.Observe(x.now().Sub(o.OwedAt).Seconds())
	}
}

// readFailure is why a document's read failed alone.
type readFailure struct {
	err error
	// database: the database or the way to it failed (transport, 401, 403,
	// 5xx), which is the document's only beside one that read fine.
	database bool
}

// readResult is what reading some of a claim's documents found.
type readResult struct {
	// read is what each document read fine found.
	read map[string]reviewcards.Read
	// failed are the documents whose read failed alone, and why.
	failed map[string]readFailure
	// refused: the gateway turned a read away; the rest went unread.
	refused bool
	// down: a read of several documents failed at the database after none
	// before it read fine, so they went unread and uncharged.
	down bool
	// stopped: ctx ended, or the pace couldn't be read; nothing is charged.
	stopped bool
}

func (r *readResult) merge(o readResult) {
	for d, v := range o.read {
		r.read[d] = v
	}
	for d, f := range o.failed {
		r.failed[d] = f
	}
	r.refused, r.down, r.stopped = r.refused || o.refused, r.down || o.down, r.stopped || o.stopped
}

// read reads docIDs of the claim in one gateway read, splitting a read that
// fails in half down to one document. After a half that read nothing and
// failed only at the database, the other half is read whole (probe) and,
// failing at the database too, isn't split: the database is down, and
// splitting it would cost a read a document.
func (x *indexer) read(ctx context.Context, binding graphproxy.Binding, c model.FoundryReviewClaim, docIDs []string, probe bool) readResult {
	out := readResult{read: map[string]reviewcards.Read{}, failed: map[string]readFailure{}}
	body, err := reviewcards.ReadQuery(c.LoadID, docIDs)
	if err != nil {
		for _, d := range docIDs {
			out.failed[d] = readFailure{err: err}
		}
		return out
	}
	if !x.pace(ctx, binding.TenantID) {
		out.stopped = true
		return out
	}
	resp, err := x.gateway.Query(ctx, binding.Endpoint, binding.TenantID, body, nil)
	if err == nil && refusal(resp) {
		log.From(ctx).Info("review cards: the database turned a read away; backing off", "load", c.LoadID, "status", resp.StatusCode)
		out.refused = true
		return out
	}
	// The database's failure, not the documents': it may fail any read.
	database := err != nil && !errors.Is(err, helixgateway.ErrResponseTooLarge) ||
		err == nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 500)
	if err == nil && resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("the database answered %d", resp.StatusCode)
	}
	var read reviewcards.Read
	if err == nil {
		read, err = reviewcards.ParseRead(resp.Body, docIDs)
	}
	switch {
	case err == nil:
		for _, d := range docIDs {
			out.read[d] = read
		}
		return out
	case ctx.Err() != nil:
		out.stopped = true
		return out
	case len(docIDs) == 1:
		out.failed[docIDs[0]] = readFailure{err: err, database: database}
		return out
	case probe && database:
		out.down = true
		return out
	}
	half := len(docIDs) / 2
	first := x.read(ctx, binding, c, docIDs[:half], false)
	out.merge(first)
	if out.refused || out.stopped || out.down {
		return out
	}
	unread := len(first.read) == 0
	for _, f := range first.failed {
		unread = unread && f.database
	}
	out.merge(x.read(ctx, binding, c, docIDs[half:], unread))
	return out
}

// backOffDocument puts off a document whose read failed on its own, and
// sets it aside after model.FoundryReviewQuarantine failures.
func (x *indexer) backOffDocument(ctx context.Context, loadID, docID string, err error) {
	quarantined, berr := x.db.BackOffFoundryReviewDocument(ctx, x.workerID, loadID, docID)
	switch {
	case berr != nil:
		log.From(ctx).Warn("review cards: back off a document failed", "load", loadID, "document", docID, "err", berr)
	case quarantined:
		reviewIndexDocuments.WithLabelValues("quarantined").Inc()
		log.From(ctx).Error("review cards: a document's reviews can't be read; it is set aside until it is owed again",
			"load", loadID, "document", docID, "err", err)
	default:
		reviewIndexDocuments.WithLabelValues("failed").Inc()
		log.From(ctx).Warn("review cards: a document's reviews could not be read; it backs off", "load", loadID, "document", docID, "err", err)
	}
}

// refusal reports whether the gateway turned a read away before running
// it, or refused the database's queries: rate limited (429), not taken on
// yet or no limiter (503), or paused for billing (402).
func refusal(resp helixgateway.Response) bool {
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusPaymentRequired:
		return true
	}
	return false
}

// pace takes the database's next read slot, shared by every replica, and
// waits for it. It reports false when ctx ended first, or the slot couldn't
// be taken.
func (x *indexer) pace(ctx context.Context, tenant string) bool {
	wait, err := x.db.ReserveFoundryReviewRead(ctx, tenant, x.readGap)
	if err != nil {
		if ctx.Err() == nil {
			log.From(ctx).Warn("review cards: could not take a read slot", "tenant", tenant, "err", err)
		}
		return false
	}
	if wait <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// singleLinks reads the claim's load's version: which of its links hold one
// value, which decides whether a held conflict can be approved.
func (x *indexer) singleLinks(ctx context.Context, c model.FoundryReviewClaim) (func(string) bool, error) {
	version, err := x.db.GetOntologyVersion(ctx, c.OntologyID, c.VersionID)
	if err != nil {
		return nil, err
	}
	var def ontologyagent.Definition
	if err := json.Unmarshal(version.Definition, &def); err != nil {
		return nil, fmt.Errorf("read the version's definition: %w", err)
	}
	one := map[string]bool{}
	return func(link string) bool {
		single, ok := one[link]
		if !ok {
			v, err := model.FoundryOneValuedLink(def.OntologyTOML, link)
			single = err == nil && v
			one[link] = single
		}
		return single
	}, nil
}

func (x *indexer) backOffLoad(ctx context.Context, loadID string) {
	if err := x.db.BackOffFoundryReviewLoad(ctx, loadID); err != nil && !errors.Is(err, context.Canceled) {
		log.From(ctx).Warn("review cards: back off a load failed", "load", loadID, "err", err)
	}
}
