package enrichment

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryplan "github.com/helixdb/helix-foundry/backend/plan"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// clock is a test's time, moved by hand.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// fakeJob is a load job as its step sees it: who leases it, and whether
// its step's end woke it.
type fakeJob struct {
	owner string
	woken int
}

type fakeCharge struct {
	model.FoundryEnrichmentCharge
	loadID, workspaceID string
	at                  time.Time
}

// stepDB is the provider's foundry_load_enrichments and companions in
// memory, with the same fences: writes of a step's job need the job's
// lease, the worker's need the step's.
type stepDB struct {
	mu       sync.Mutex
	clock    *clock
	order    []string
	steps    map[string]*model.FoundryLoadEnrichment
	jobs     map[string]*fakeJob
	consents map[string]*model.FoundryEnrichmentConsent
	charges  []fakeCharge
	halts    map[string]*model.FoundryEnrichmentProvider
	// gone are the ontologies deleted, or being deleted, as the re-check
	// reads them. As in MySQL, which never deletes a load or a version, their
	// loads and versions stay readable; noLoad breaks that, as a row naming
	// a load that isn't there would.
	gone     map[string]bool
	noLoad   bool
	mappings string
	// failCharge fails the next charge: before it lands, or after it landed
	// (its answer lost); failCheck fails every re-check, failRenew every
	// renewal of a lease (MySQL unreachable, not the lease lost).
	failCharge, loseCharge, failCheck, failRenew bool
	// afterCharge runs after each charge lands, with the batch charged.
	afterCharge func(batch int)
	// renewing runs as a renewal starts, before it lands; released after a
	// release lands.
	renewing, released func()
	// behind is how far the database's clock is behind the attempt's.
	behind time.Duration
}

func newStepDB(c *clock, mappings string) *stepDB {
	return &stepDB{
		clock: c, steps: map[string]*model.FoundryLoadEnrichment{}, jobs: map[string]*fakeJob{},
		consents: map[string]*model.FoundryEnrichmentConsent{}, halts: map[string]*model.FoundryEnrichmentProvider{},
		gone: map[string]bool{}, mappings: mappings,
	}
}

func clone(e *model.FoundryLoadEnrichment) *model.FoundryLoadEnrichment {
	c := *e
	return &c
}

func ptr[T any](v T) *T { return &v }

// samples is how many observations the histogram name holds, in the
// default registry.
func samples(t *testing.T, name string) uint64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == name {
			return f.GetMetric()[0].GetHistogram().GetSampleCount()
		}
	}
	return 0
}

// owned is load's step when jobWorker holds its job's lease.
func (d *stepDB) owned(loadID, jobWorker string) (*model.FoundryLoadEnrichment, error) {
	e, ok := d.steps[loadID]
	if !ok || d.jobs[e.JobID].owner != jobWorker {
		return nil, store.ErrNotFound
	}
	return e, nil
}

// leased is load's running step when workerID holds its lease.
func (d *stepDB) leased(loadID, workerID string) (*model.FoundryLoadEnrichment, error) {
	e, ok := d.steps[loadID]
	if !ok || e.Status != model.FoundryEnrichmentRunning || e.LeaseOwner == nil || *e.LeaseOwner != workerID {
		return nil, store.ErrNotFound
	}
	return e, nil
}

func (d *stepDB) end(e *model.FoundryLoadEnrichment, end model.FoundryEnrichmentEnd) {
	e.Status, e.StoppedBy, e.EndedAt = end.Status(), end.StoppedBy(), ptr(d.clock.now())
}

func (d *stepDB) StartFoundryLoadEnrichment(_ context.Context, jobWorker string, job *model.FoundryGraphJob) (*model.FoundryLoadEnrichment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	j, ok := d.jobs[job.ID]
	if !ok {
		j = &fakeJob{owner: jobWorker}
		d.jobs[job.ID] = j
	}
	if j.owner != jobWorker {
		return nil, store.ErrNotFound
	}
	if _, ok := d.steps[job.LoadID]; !ok {
		d.steps[job.LoadID] = &model.FoundryLoadEnrichment{
			LoadID: job.LoadID, WorkspaceID: job.WorkspaceID, OntologyID: job.OntologyID, JobID: job.ID,
			Status: model.FoundryEnrichmentPreparing, CreatedAt: d.clock.now(),
		}
		d.order = append(d.order, job.LoadID)
	}
	return clone(d.steps[job.LoadID]), nil
}

func (d *stepDB) GetFoundryLoadEnrichment(_ context.Context, loadID string) (*model.FoundryLoadEnrichment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.steps[loadID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return clone(e), nil
}

func (d *stepDB) GetFoundryEnrichmentConsent(_ context.Context, workspaceID string) (*model.FoundryEnrichmentConsent, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	c, ok := d.consents[workspaceID]
	if !ok {
		return nil, store.ErrNotFound
	}
	return ptr(*c), nil
}

// newest is the ontology's newest step that keep accepts.
func (d *stepDB) newest(ontologyID string, keep func(*model.FoundryLoadEnrichment) bool) (*model.FoundryLoadEnrichment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, loadID := range slices.Backward(d.order) {
		if e := d.steps[loadID]; e.OntologyID == ontologyID && e.PurgedAt == nil && keep(e) {
			return clone(e), nil
		}
	}
	return nil, store.ErrNotFound
}

func (d *stepDB) GetLatestFoundryEnrichmentResults(_ context.Context, ontologyID string) (*model.FoundryLoadEnrichment, error) {
	return d.newest(ontologyID, func(e *model.FoundryLoadEnrichment) bool { return e.ResultsKey != nil })
}

func (d *stepDB) GetLatestFoundryEnrichmentApplied(_ context.Context, ontologyID string) (*model.FoundryLoadEnrichment, error) {
	return d.newest(ontologyID, func(e *model.FoundryLoadEnrichment) bool {
		own := func(key *string) bool { return key != nil && e.Applied != nil && *key == e.Applied.Key }
		return own(e.CarriedKey) || own(e.ProfilesKey)
	})
}

func (d *stepDB) GetFoundryEnrichmentLedger(_ context.Context, workspaceID string, since time.Time) (model.FoundryEnrichmentLedger, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ledger(workspaceID, "", since), nil
}

func (d *stepDB) ledger(workspaceID, exceptLoad string, since time.Time) model.FoundryEnrichmentLedger {
	var l model.FoundryEnrichmentLedger
	for _, c := range d.charges {
		if c.at.Before(since) {
			continue
		}
		total := c.Credits + c.UncertainCredits
		l.GlobalCharged += total
		if c.workspaceID == workspaceID {
			l.WorkspaceCharged += total
		}
	}
	for _, e := range d.steps {
		if e.Status != model.FoundryEnrichmentRunning || e.LoadID == exceptLoad {
			continue
		}
		l.GlobalReserved += e.ReservedCredits
		if e.WorkspaceID == workspaceID {
			l.WorkspaceReserved += e.ReservedCredits
		}
	}
	return l
}

func (d *stepDB) GetFoundryEnrichmentProvider(_ context.Context, name string) (*model.FoundryEnrichmentProvider, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.halts[name]; ok {
		return ptr(*p), nil
	}
	return &model.FoundryEnrichmentProvider{Provider: name}, nil
}

func (d *stepDB) HaltFoundryEnrichmentProvider(_ context.Context, name string, until time.Time, code string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.halts[name]; !ok || p.HaltedUntil.Before(until) {
		d.halts[name] = &model.FoundryEnrichmentProvider{Provider: name, HaltedUntil: &until, HaltCode: &code}
	}
	return nil
}

func (d *stepDB) QueueFoundryLoadEnrichment(_ context.Context, loadID, jobWorker string, q model.FoundryEnrichmentQueue) (*model.FoundryLoadEnrichment, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, err := d.owned(loadID, jobWorker)
	if err != nil {
		return nil, err
	}
	if e.Status != model.FoundryEnrichmentPreparing {
		return nil, fmt.Errorf("step %s is %s", loadID, e.Status)
	}
	e.PlanKey, e.CarriedKey, e.Counts, e.ClaimBy = &q.PlanKey, &q.CarriedKey, q.Counts, &q.ClaimBy
	switch {
	case e.CancelRequested:
		d.end(e, model.EndFoundryEnrichment(model.FoundryEnrichmentCancelled))
	case e.StopRequested != nil:
		d.end(e, model.PartialFoundryEnrichment(model.FoundryEnrichmentStopStopped))
	default:
		e.Status = model.FoundryEnrichmentQueued
	}
	return clone(e), nil
}

func (d *stepDB) EndPreparedFoundryLoadEnrichment(_ context.Context, loadID, jobWorker string, p model.FoundryEnrichmentPrepared) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, err := d.owned(loadID, jobWorker)
	if err != nil {
		return err
	}
	if e.Status != model.FoundryEnrichmentPreparing {
		return fmt.Errorf("step %s is %s", loadID, e.Status)
	}
	end, applied := p.End, p.Applied
	if e.CancelRequested {
		end, applied = model.EndFoundryEnrichment(model.FoundryEnrichmentCancelled), model.FoundryEnrichmentNone
	}
	key := func(k string) *string {
		if k == "" {
			return nil
		}
		return &k
	}
	e.PlanKey, e.CarriedKey, e.ResultsKey, e.Counts, e.Applied = key(p.PlanKey), key(p.CarriedKey), key(p.ResultsKey), p.Counts, &applied
	d.end(e, end)
	return nil
}

func (d *stepDB) ApplyFoundryLoadEnrichment(_ context.Context, loadID, jobWorker string, applied model.FoundryEnrichmentApplied) (model.FoundryEnrichmentApplied, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, err := d.owned(loadID, jobWorker)
	if err != nil {
		return model.FoundryEnrichmentApplied{}, err
	}
	if e.PurgedAt != nil {
		applied = model.FoundryEnrichmentNone
	}
	if e.Applied == nil {
		e.Applied = &applied
	}
	return *e.Applied, nil
}

func (d *stepDB) EndBusyFoundryLoadEnrichment(_ context.Context, loadID, jobWorker string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, err := d.owned(loadID, jobWorker)
	if err != nil {
		return err
	}
	if e.Status == model.FoundryEnrichmentQueued && !e.ClaimBy.After(d.clock.now().Add(-d.behind)) {
		d.end(e, model.PartialFoundryEnrichment(model.FoundryEnrichmentStopBusy))
	}
	return nil
}

func (d *stepDB) ClaimFoundryLoadEnrichment(_ context.Context, workerID, _ string, lease, loadWait, grace time.Duration, maxRunning int) (*model.FoundryLoadEnrichment, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock.now()
	running, slots := map[string]bool{}, 0
	for _, loadID := range d.order {
		e := d.steps[loadID]
		if e.Status != model.FoundryEnrichmentRunning {
			continue
		}
		if e.LeaseUntil.Before(now) {
			e.LeaseOwner, e.LeaseUntil = &workerID, ptr(now.Add(lease))
			return clone(e), false, nil
		}
		if e.Deadline.Add(grace).After(now) {
			running[e.WorkspaceID] = true
			slots++
		}
	}
	if slots >= maxRunning {
		return nil, false, nil
	}
	var due *model.FoundryLoadEnrichment
	for _, loadID := range d.order {
		e := d.steps[loadID]
		if e.Status == model.FoundryEnrichmentQueued && e.ClaimBy.After(now) && !e.CancelRequested && e.StopRequested == nil &&
			!running[e.WorkspaceID] && (due == nil || e.ClaimBy.Before(*due.ClaimBy)) {
			due = e
		}
	}
	if due == nil {
		return nil, false, nil
	}
	due.Status, due.LeaseOwner, due.LeaseUntil = model.FoundryEnrichmentRunning, &workerID, ptr(now.Add(lease))
	due.ClaimedAt, due.Deadline = ptr(now), ptr(now.Add(loadWait))
	return clone(due), true, nil
}

func (d *stepDB) RenewFoundryLoadEnrichmentLease(_ context.Context, loadID, workerID, name string, lease time.Duration) (model.FoundryEnrichmentSignals, error) {
	if d.renewing != nil {
		d.renewing()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failRenew {
		return model.FoundryEnrichmentSignals{}, errBoom
	}
	e, err := d.leased(loadID, workerID)
	if err != nil {
		return model.FoundryEnrichmentSignals{}, err
	}
	e.LeaseUntil = ptr(d.clock.now().Add(lease))
	return model.FoundryEnrichmentSignals{Stop: e.StopRequested != nil, Cancel: e.CancelRequested, Halted: d.halts[name].Halted(d.clock.now())}, nil
}

func (d *stepDB) ReleaseFoundryLoadEnrichment(_ context.Context, loadID, workerID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, err := d.leased(loadID, workerID)
	if err != nil {
		return err
	}
	e.LeaseUntil = ptr(d.clock.now().Add(-time.Microsecond))
	if d.released != nil {
		d.released()
	}
	return nil
}

func (d *stepDB) CheckFoundryLoadEnrichment(_ context.Context, loadID, name string) (model.FoundryEnrichmentChecks, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.steps[loadID]
	if !ok {
		return model.FoundryEnrichmentChecks{}, store.ErrNotFound
	}
	if d.failCheck {
		return model.FoundryEnrichmentChecks{}, errBoom
	}
	c := model.FoundryEnrichmentChecks{Gone: d.gone[e.OntologyID]}
	c.Stop, c.Cancel, c.Halted = e.StopRequested != nil, e.CancelRequested, d.halts[name].Halted(d.clock.now())
	if consent, ok := d.consents[e.WorkspaceID]; ok {
		c.Consent = ptr(*consent)
	}
	return c, nil
}

func (d *stepDB) GetFoundryLoad(_ context.Context, ontologyID, id string) (*model.FoundryLoad, error) {
	if d.noLoad {
		return nil, store.ErrNotFound
	}
	return &model.FoundryLoad{ID: id, OntologyID: ontologyID, VersionID: "version-" + id}, nil
}

func (d *stepDB) GetOntologyVersion(_ context.Context, ontologyID, id string) (*model.OntologyVersion, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	def, err := json.Marshal(ontologyagent.Definition{Format: "agent_bundle", MappingsTOML: d.mappings})
	return &model.OntologyVersion{ID: id, OntologyID: ontologyID, Format: model.OntologyFormatAgentBundle, Definition: def}, err
}

func (d *stepDB) ReserveFoundryEnrichmentCredits(_ context.Context, loadID, workerID, _ string, want int64, caps model.FoundryEnrichmentCaps) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, err := d.leased(loadID, workerID)
	if err != nil {
		return 0, err
	}
	if e.InflightCredits > 0 {
		return 0, fmt.Errorf("step %s has a batch in flight", loadID)
	}
	e.ReservedCredits = max(min(want, d.ledger(e.WorkspaceID, loadID, caps.Since).Headroom(caps)), 0)
	return e.ReservedCredits, nil
}

func (d *stepDB) BeginFoundryEnrichmentBatch(_ context.Context, loadID, workerID, name string, batch, want int,
	caps model.FoundryEnrichmentCaps) (model.FoundryEnrichmentBegun, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, err := d.leased(loadID, workerID)
	if err != nil {
		return model.FoundryEnrichmentBegun{}, err
	}
	if e.InflightCredits > 0 || e.BatchCursor != batch {
		return model.FoundryEnrichmentBegun{}, fmt.Errorf("step %s is at batch %d, not ready for %d", loadID, e.BatchCursor, batch)
	}
	begun := model.FoundryEnrichmentBegun{FoundryEnrichmentSignals: model.FoundryEnrichmentSignals{
		Stop: e.StopRequested != nil, Cancel: e.CancelRequested, Halted: d.halts[name].Halted(d.clock.now()),
	}}
	if begun.Stop || begun.Cancel || begun.Halted {
		return begun, nil
	}
	credits := min(e.ReservedCredits, d.ledger(e.WorkspaceID, loadID, caps.Since).Headroom(caps))
	begun.N = int(max(min(int64(want), credits/model.FoundryEnrichmentCredits), 0))
	e.InflightCredits = int64(begun.N) * model.FoundryEnrichmentCredits
	return begun, nil
}

func (d *stepDB) ChargeFoundryEnrichmentBatch(ctx context.Context, loadID, workerID string, charge model.FoundryEnrichmentCharge, counts model.FoundryEnrichmentCounts) error {
	err := d.charge(loadID, workerID, charge, counts)
	if err == nil && d.afterCharge != nil {
		d.afterCharge(charge.Batch)
	}
	return err
}

func (d *stepDB) charge(loadID, workerID string, charge model.FoundryEnrichmentCharge, counts model.FoundryEnrichmentCounts) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failCharge {
		d.failCharge = false
		return errBoom
	}
	e, err := d.leased(loadID, workerID)
	if err != nil {
		return err
	}
	total := charge.Credits + charge.UncertainCredits
	if charge.Batch != e.BatchCursor || total > e.InflightCredits {
		return fmt.Errorf("batch %d of %s charges %d with %d in flight at %d", charge.Batch, loadID, total, e.InflightCredits, e.BatchCursor)
	}
	d.charges = append(d.charges, fakeCharge{FoundryEnrichmentCharge: charge, loadID: loadID, workspaceID: e.WorkspaceID, at: d.clock.now()})
	e.BatchCursor, e.InflightCredits, e.ReservedCredits, e.Counts = charge.Batch+1, 0, min(max(e.ReservedCredits-total, 0), charge.Remaining), counts
	if d.loseCharge {
		d.loseCharge = false
		return errBoom
	}
	return nil
}

func (d *stepDB) EndFoundryLoadEnrichment(_ context.Context, loadID, workerID string, end model.FoundryEnrichmentEnd,
	published *model.FoundryEnrichmentPublished, counts model.FoundryEnrichmentCounts) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, err := d.leased(loadID, workerID)
	if err != nil {
		return err
	}
	if e.InflightCredits > 0 {
		return fmt.Errorf("step %s has a batch in flight", loadID)
	}
	if published != nil {
		e.ProfilesKey, e.ResultsKey, e.PublishedAt = &published.ProfilesKey, &published.ResultsKey, ptr(d.clock.now())
	}
	e.Counts, e.LeaseOwner, e.LeaseUntil, e.ReservedCredits = counts, nil, nil, 0
	d.end(e, end)
	d.jobs[e.JobID].woken++
	return nil
}

// stop is a user's Stop, as StopFoundryLoadEnrichment does it.
func (d *stepDB) stop(loadID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e := d.steps[loadID]
	switch e.Status {
	case model.FoundryEnrichmentQueued:
		e.StopRequested = ptr("user-ada")
		d.end(e, model.PartialFoundryEnrichment(model.FoundryEnrichmentStopStopped))
		d.jobs[e.JobID].woken++
	case model.FoundryEnrichmentPreparing, model.FoundryEnrichmentRunning:
		e.StopRequested = ptr("user-ada")
	}
}

// withdraw is consent turned off, or the ontology deleted (gone), as the
// provider cancels and purges its steps.
func (d *stepDB) withdraw(workspaceID, ontologyID string, gone bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if gone {
		d.gone[ontologyID] = true
	} else {
		d.consents[workspaceID] = &model.FoundryEnrichmentConsent{WorkspaceID: workspaceID, Enabled: false}
	}
	for _, e := range d.steps {
		if e.OntologyID != ontologyID {
			continue
		}
		switch e.Status {
		case model.FoundryEnrichmentQueued:
			d.end(e, model.EndFoundryEnrichment(model.FoundryEnrichmentCancelled))
			d.jobs[e.JobID].woken++
		case model.FoundryEnrichmentPreparing, model.FoundryEnrichmentRunning:
			e.CancelRequested = true
		}
		e.PurgedAt = ptr(d.clock.now())
	}
}

func (d *stepDB) charged() (credits, uncertain int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.charges {
		credits, uncertain = credits+c.Credits, uncertain+c.UncertainCredits
	}
	return credits, uncertain
}

func (d *stepDB) woken(jobID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.jobs[jobID].woken
}

// devServer is an httptest context.dev. It answers as the fake provider does
// (Fake), unless answer has an answer for the request; a "timeout" clue
// hangs until the client gives up.
type devServer struct {
	mu       sync.Mutex
	requests []string
	answer   func(email, slug string) (int, string, bool)
	url      string
}

func newDevServer(t *testing.T) *devServer {
	t.Helper()
	dev := &devServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var in struct {
			Email      string   `json:"email"`
			SocialURLs []string `json:"social_urls"`
		}
		_ = json.Unmarshal(raw, &in)
		slug := ""
		if len(in.SocialURLs) > 0 {
			slug, _ = CanonicalLinkedIn(in.SocialURLs[0])
		}
		dev.mu.Lock()
		dev.requests = append(dev.requests, in.Email+"|"+slug)
		answer := dev.answer
		dev.mu.Unlock()
		if answer != nil {
			if status, body, ok := answer(in.Email, slug); ok {
				w.Header().Set("X-Request-Id", "req-"+in.Email)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
				return
			}
		}
		resp, err := Fake{}.RoundTrip(httptest.NewRequest(http.MethodPost, "/v1/people/enrich", bytes.NewReader(raw)))
		if err != nil {
			<-r.Context().Done()
			return
		}
		w.Header().Set("X-Request-Id", resp.Header.Get("X-Request-Id"))
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(server.Close)
	dev.url = server.URL
	return dev
}

func (d *devServer) sent() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.requests)
}

func (d *devServer) setAnswer(answer func(email, slug string) (int, string, bool)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.answer = answer
}

// proPlans is a workspace's plan: on Pro unless off; err is Autumn
// failing.
type proPlans struct {
	mu  sync.Mutex
	off bool
	err error
}

func (p *proPlans) Status(context.Context, string) (foundryplan.Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	free := foundryplan.Status{Plan: &foundryplan.Plan{Code: "free"}}
	if p.off {
		return free, nil
	}
	if p.err != nil {
		return free, p.err
	}
	return foundryplan.Status{Plan: &foundryplan.Plan{Code: "pro", AutumnPlanID: "foundry-pro", Rank: 1}}, nil
}

func (p *proPlans) set(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.off = !on
}

// itemTOML enriches Person by email and LinkedIn URL, with work history.
const itemTOML = `
[enrichments.people]
type = "Person"
clues = { email = "email", linkedin_url = "linkedin_url" }
props = { current_title = "current_title", location = "location", country = "location_country", profile_url = "linkedin_url" }
work_history = { link = "worked_at", match = "domain", props = { title = "title", start_date = "start_date" } }
`

var stepStart = time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)

// stepFixture is one ontology's loads, their steps, the enrichment worker
// and an httptest context.dev.
type stepFixture struct {
	t     *testing.T
	clock *clock
	db    *stepDB
	store *memStore
	dev   *devServer
	pro   *proPlans
	cfg   config.EnrichmentConfig
	item  *ontologyagent.Enrichment
	loads int
}

func newStepFixture(t *testing.T) *stepFixture {
	t.Helper()
	c := &clock{at: stepStart}
	item, err := ontologyagent.ParseEnrichments(itemTOML)
	if err != nil {
		t.Fatal(err)
	}
	f := &stepFixture{
		t: t, clock: c, db: newStepDB(c, itemTOML), store: newMemStore(), dev: newDevServer(t), pro: &proPlans{}, item: item,
		cfg: config.EnrichmentConfig{
			Enabled: true, Provider: config.EnrichmentContextDev, MinScore: 90, Concurrency: 48, MaxRunning: 3,
			ClaimWait: 5 * time.Minute, LoadWait: time.Hour, Grace: 2 * time.Minute, MonthlyCreditsMax: 100_000,
			GlobalMonthlyCredits: 300_000, RawRetentionDays: 90, NotFoundDays: 90, Lease: 2 * time.Minute, MaxPeople: 1000,
		},
	}
	f.db.consents["ws"] = &model.FoundryEnrichmentConsent{WorkspaceID: "ws", Enabled: true, MonthlyCreditCap: 50_000}
	return f
}

func (f *stepFixture) files() *Files { return &Files{Store: f.store, Dir: f.t.TempDir()} }

func (f *stepFixture) loadSide() *Load {
	return &Load{DB: f.db, Files: f.files(), Cfg: f.cfg, Pro: f.pro, Now: f.clock.now}
}

func (f *stepFixture) worker(id string) *Worker {
	client := NewClient(f.cfg) // context.dev's: its connections stay open between calls
	client.HTTP.Timeout, client.BaseURL, client.APIKey, client.Now = 300*time.Millisecond, f.dev.url+"/v1", "key", f.clock.now
	client.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	client.Jitter = func(time.Duration) time.Duration { return 0 }
	return &Worker{DB: f.db, Files: f.files(), Pro: f.pro, Cfg: f.cfg, WorkerID: id, Now: f.clock.now, Client: client}
}

// newLoad is the ontology's next load job, leased by the graph worker.
func (f *stepFixture) newLoad() *model.FoundryGraphJob {
	f.loads++
	id := fmt.Sprintf("load-%d", f.loads)
	return &model.FoundryGraphJob{
		ID: "job-" + id, LoadID: id, WorkspaceID: "ws", OntologyID: "ont", Kind: model.FoundryGraphJobLoad,
		Phase: model.FoundryGraphJobProvisioning,
	}
}

// step runs the graph worker's side of job's step on people.
func (f *stepFixture) step(job *model.FoundryGraphJob, people ReadPeople) Next {
	f.t.Helper()
	next, err := f.loadSide().Step(context.Background(), job, "graph", f.item, people)
	if err != nil {
		f.t.Fatalf("step %s: %v", job.LoadID, err)
	}
	return next
}

// work claims the next step as worker id and runs it; false when none was due.
func (f *stepFixture) work(id string) bool {
	f.t.Helper()
	w := f.worker(id)
	leased := f.clock.now()
	step, err := w.Claim(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	if step == nil {
		return false
	}
	if err := w.Run(context.Background(), step, leased); err != nil {
		f.t.Logf("run %s: %v", step.LoadID, err)
	}
	return true
}

// row is a load's step as stored.
func (f *stepFixture) row(loadID string) *model.FoundryLoadEnrichment {
	f.t.Helper()
	row, err := f.db.GetFoundryLoadEnrichment(context.Background(), loadID)
	if err != nil {
		f.t.Fatal(err)
	}
	return row
}

// profiles reads an applied file's slim rows.
func (f *stepFixture) profiles(applied *model.FoundryEnrichmentApplied) []map[string]any {
	f.t.Helper()
	if applied == nil {
		f.t.Fatal("no file applied")
	}
	if applied.Key == "" {
		return nil
	}
	rows, err := ReadAll[map[string]any](context.Background(), f.store, applied.Key)
	if err != nil {
		f.t.Fatal(err)
	}
	return rows
}

// results reads a step's published results.
func (f *stepFixture) results(loadID string) []Result {
	f.t.Helper()
	row := f.row(loadID)
	if row.ResultsKey == nil {
		f.t.Fatalf("step %s published no results", loadID)
	}
	results, err := ReadAll[Result](context.Background(), f.store, *row.ResultsKey)
	if err != nil {
		f.t.Fatal(err)
	}
	return results
}

// person is someone in the tables the fake provider finds as named: the
// email's local part is their first and last name.
func person(entity, first, last, domain string) Clue {
	return Clue{Entity: entity, Name: first + " " + last, Email: strings.ToLower(first+"."+last) + "@" + domain}
}

// people reads clues as Clues sends them, none shared.
func people(clues ...Clue) ReadPeople { return sharing(0, clues...) }

// sharing reads clues as Clues sends them, shared of them having lost a
// clue to another.
func sharing(shared int64, clues ...Clue) ReadPeople {
	return func(_ context.Context, add func(Clue) error) (int64, error) {
		for _, c := range clues {
			if err := add(c); err != nil {
				return 0, err
			}
		}
		return shared, nil
	}
}

// outcomes are results' outcomes and reasons, sorted.
func outcomes(results []Result) []string {
	var out []string
	for _, r := range results {
		out = append(out, strings.TrimSuffix(string(r.Outcome)+":"+r.Reason, ":"))
	}
	slices.Sort(out)
	return out
}
