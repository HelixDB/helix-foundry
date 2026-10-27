package enrichment

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// enriched runs a load whose step looks everyone up and publishes, and
// returns its job: the earlier load the next one carries from.
func (f *stepFixture) enriched(found ReadPeople) *model.FoundryGraphJob {
	f.t.Helper()
	job := f.newLoad()
	if next := f.step(job, found); next.Apply != nil {
		f.t.Fatalf("load %s applied %+v without lookups", job.LoadID, next.Apply)
	}
	if !f.work("w1") {
		f.t.Fatal("no step to run")
	}
	next := f.step(job, found)
	if next.Apply == nil {
		f.t.Fatalf("load %s still waits until %s", job.LoadID, next.Until)
	}
	return job
}

func TestLoadStep(t *testing.T) {
	ada := person("c_17", "Ada", "Lovelace", "acme.io")
	grace := person("c_18", "Grace", "Hopper", "navy.mil")

	// Without the owner's consent the agent never sees the source and a load
	// applies no profiles: nothing is planned and no one is looked up.
	t.Run("applies no profiles without consent", func(t *testing.T) {
		f := newStepFixture(t)
		delete(f.db.consents, "ws")
		off := testutil.ToFloat64(steps.WithLabelValues("off", ""))
		job := f.newLoad()
		next := f.step(job, people(ada))
		if got := testutil.ToFloat64(steps.WithLabelValues("off", "")); got != off+1 {
			t.Fatalf("off ends counted %v, want %v", got, off+1)
		}
		if next.Apply == nil || *next.Apply != model.FoundryEnrichmentNone {
			t.Fatalf("next = %+v, want the zero-byte file", next)
		}
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentOff || row.PlanKey != nil {
			t.Fatalf("step = %+v, want off with no plan", row)
		}
	})

	// The first load parks while the worker looks people up; the second
	// finds everyone carried, spends nothing and doesn't park.
	t.Run("a load with no one to look up doesn't park", func(t *testing.T) {
		f := newStepFixture(t)
		parkedBefore := samples(t, "helix_foundry_enrichment_parked_seconds")
		first := f.enriched(people(ada, grace))
		if got := samples(t, "helix_foundry_enrichment_parked_seconds"); got != parkedBefore+1 {
			t.Fatalf("parked ages observed %d, want %d: load 1 parked", got, parkedBefore+1)
		}
		applied := *f.row(first.LoadID).Applied
		if rows := f.profiles(&applied); len(rows) != 2 {
			t.Fatalf("load 1 applied %d profiles, want 2", len(rows))
		}
		requests := len(f.dev.sent())
		credits, _ := f.db.charged()

		job := f.newLoad()
		next := f.step(job, people(ada, grace))
		if next.Apply == nil {
			t.Fatalf("load 2 parked until %s", next.Until)
		}
		row := f.row(job.LoadID)
		if row.Status != model.FoundryEnrichmentDone || row.Counts.Carried != 2 || row.Counts.ToLookUp != 0 || row.Counts.Profiles != 2 {
			t.Fatalf("load 2's step = %s %+v", row.Status, row.Counts)
		}
		if again, _ := f.db.charged(); len(f.dev.sent()) != requests || again != credits {
			t.Fatalf("load 2 made %d calls and charged %d", len(f.dev.sent())-requests, again-credits)
		}
		rows := f.profiles(next.Apply)
		if len(rows) != 2 || rows[0]["clue_name"] == nil || rows[0]["current_title"] != "VP Engineering" {
			t.Fatalf("load 2 applied %v", rows)
		}
		if len(f.results(job.LoadID)) != 2 {
			t.Fatal("load 2's results don't carry everyone forward")
		}
		if got := samples(t, "helix_foundry_enrichment_parked_seconds"); got != parkedBefore+1 {
			t.Fatalf("parked ages observed %d, want %d: load 2 never parked", got, parkedBefore+1)
		}
	})

	// Lookups that aren't allowed end the step at once with the carried
	// profiles: the flag is carry-only, Pro is needed for new lookups, and
	// a cap or a provider halt leave nothing to spend.
	for name, block := range map[string]struct {
		why   model.FoundryEnrichmentStop
		apply func(*stepFixture)
	}{
		"the flag off":    {model.FoundryEnrichmentStopFlag, func(f *stepFixture) { f.cfg.Enabled = false }},
		"off Pro":         {model.FoundryEnrichmentStopPro, func(f *stepFixture) { f.pro.set(false) }},
		"no credits left": {model.FoundryEnrichmentStopCap, func(f *stepFixture) { f.db.consents["ws"].MonthlyCreditCap = 19 }},
		"the provider halted": {model.FoundryEnrichmentStopProviderCredits, func(f *stepFixture) {
			_ = f.db.HaltFoundryEnrichmentProvider(context.Background(), "contextdev", f.clock.now().Add(time.Minute), "402")
		}},
		"Helix's limit reached": {model.FoundryEnrichmentStopCap, func(f *stepFixture) { f.cfg.GlobalMonthlyCredits = 10 }},
	} {
		t.Run("applies the carried profiles with "+name, func(t *testing.T) {
			f := newStepFixture(t)
			f.enriched(people(ada))
			block.apply(f)
			requests := len(f.dev.sent())
			job := f.newLoad()
			next := f.step(job, people(ada, grace))
			row := f.row(job.LoadID)
			if row.Status != model.FoundryEnrichmentPartial || *row.StoppedBy != block.why {
				t.Fatalf("step = %s %v, want partial(%s)", row.Status, row.StoppedBy, block.why)
			}
			if rows := f.profiles(next.Apply); len(rows) != 1 || len(f.dev.sent()) != requests {
				t.Fatalf("applied %d profiles after %d calls, want Ada's carried and none", len(rows), len(f.dev.sent())-requests)
			}
			if results := f.results(job.LoadID); len(results) != 1 {
				t.Fatalf("results = %v, want Ada's carried only: Grace is looked up next load", outcomes(results))
			}
		})
	}

	// A running step's reservation is what it may spend, not what it spent:
	// a load of the workspace's other ontology queues behind it, rather than
	// ending partial(cap) with nothing charged, and looks its people up with
	// what that step left once it ends.
	t.Run("a running step's reservation queues another load rather than capping it", func(t *testing.T) {
		f := newStepFixture(t)
		f.db.consents["ws"].MonthlyCreditCap = 100
		var misses []Clue
		for i := range 5 {
			misses = append(misses, Clue{Entity: fmt.Sprintf("m_%d", i), Name: "Pat Bad", Email: fmt.Sprintf("pat.bad%d@acme.io", i)})
		}
		first := f.newLoad()
		f.step(first, people(misses...))
		stopping, sigterm := context.WithCancel(context.Background())
		sigterm() // handed on before its first batch, holding what it reserved
		w := f.worker("w1")
		step, _ := w.Claim(context.Background())
		if err := w.Run(stopping, step, f.clock.now()); err != nil {
			t.Fatal(err)
		}
		if row := f.row(first.LoadID); row.Status != model.FoundryEnrichmentRunning || row.ReservedCredits != 100 {
			t.Fatalf("the first step is %s, reserving %d", row.Status, row.ReservedCredits)
		}
		second := f.newLoad()
		second.OntologyID = "ont-2"
		if next := f.step(second, people(ada)); next.Apply != nil {
			t.Fatalf("the second load ended %s at once", *f.row(second.LoadID).StoppedBy)
		}
		if row := f.row(second.LoadID); row.Status != model.FoundryEnrichmentQueued {
			t.Fatalf("the second step is %s, want queued", row.Status)
		}
		f.work("w2")
		f.ended(first.LoadID, model.FoundryEnrichmentDone, "")
		f.work("w3")
		f.ended(second.LoadID, model.FoundryEnrichmentDone, "")
		if credits, _ := f.db.charged(); credits != 20 || len(f.dev.sent()) != 6 {
			t.Fatalf("charged %d after %d calls, want Ada's", credits, len(f.dev.sent()))
		}
	})

	// Enrichment never fails a load: a step that can't be planned applies
	// the ontology's newest profiles, which the name guard binds only to the
	// people they were found for.
	t.Run("a step that fails to prepare applies the newest profiles", func(t *testing.T) {
		f := newStepFixture(t)
		first := f.enriched(people(ada))
		job := f.newLoad()
		next, err := f.loadSide().Step(context.Background(), job, "graph", f.item, func(context.Context, func(Clue) error) (int64, error) {
			return 0, errors.New("the sidecar hung up")
		})
		if err != nil {
			t.Fatal(err)
		}
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentFailed || *next.Apply != *f.row(first.LoadID).Applied {
			t.Fatalf("step = %s applying %+v", row.Status, next.Apply)
		}
	})

	// More people than a step may hold fail only the step: Clues stops at
	// the one past max_people, and the load goes on with the newest
	// profiles.
	t.Run("more people than max_people fail only the step", func(t *testing.T) {
		f := newStepFixture(t)
		first := f.enriched(people(ada))
		f.cfg.MaxPeople = 1
		job := f.newLoad()
		var sent int
		next := f.step(job, func(ctx context.Context, add func(Clue) error) (int64, error) {
			for _, c := range []Clue{ada, grace, person("c_19", "Alan", "Turing", "acme.io")} {
				if err := add(c); err != nil {
					return 0, err
				}
				sent++
			}
			return 0, nil
		})
		row := f.row(job.LoadID)
		if row.Status != model.FoundryEnrichmentFailed || *next.Apply != *f.row(first.LoadID).Applied || sent != 1 || row.PlanKey != nil {
			t.Fatalf("step = %s applying %+v after %d people", row.Status, next.Apply, sent)
		}
	})

	// Autumn can't say whether the workspace is on Pro: the step fails
	// closed and the load goes on with the newest profiles.
	t.Run("a plan that can't be read fails the step closed", func(t *testing.T) {
		f := newStepFixture(t)
		first := f.enriched(people(ada))
		f.pro.err = errors.New("autumn is down")
		job := f.newLoad()
		next := f.step(job, people(ada, grace))
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentFailed || *next.Apply != *f.row(first.LoadID).Applied {
			t.Fatalf("step = %s applying %+v", row.Status, next.Apply)
		}
	})

	// The previous results were swept: there is nothing to carry, and
	// everyone is looked up again.
	t.Run("swept results carry nothing", func(t *testing.T) {
		f := newStepFixture(t)
		first := f.enriched(people(ada))
		_ = f.store.Delete(context.Background(), *f.row(first.LoadID).ResultsKey)
		job := f.newLoad()
		f.step(job, people(ada))
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentQueued || row.Counts.Carried != 0 || row.Counts.ToLookUp != 1 {
			t.Fatalf("step = %s %+v", row.Status, row.Counts)
		}
	})

	// Invalid mappings or an unavailable source fail or retry the load as
	// its Ingest would; the step waits, preparing, for the next attempt.
	t.Run("a load's own failure passes through and the retry prepares again", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.newLoad()
		unavailable := errors.New("source_unavailable")
		_, err := f.loadSide().Step(context.Background(), job, "graph", f.item, func(context.Context, func(Clue) error) (int64, error) {
			return 0, &LoadFailure{Err: unavailable}
		})
		if err != unavailable || f.row(job.LoadID).Status != model.FoundryEnrichmentPreparing {
			t.Fatalf("err = %v, step %s", err, f.row(job.LoadID).Status)
		}
		if next := f.step(job, people(ada)); next.Apply != nil || f.row(job.LoadID).Status != model.FoundryEnrichmentQueued {
			t.Fatalf("the retry = %+v", next)
		}
	})

	// A lost lease writes nothing: another attempt owns the job.
	t.Run("a stale attempt writes nothing", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.newLoad()
		f.step(job, people(ada))
		if _, err := f.loadSide().Step(context.Background(), job, "stale", f.item, nil); err == nil {
			t.Fatal("a worker without the job's lease read its step")
		}
	})

	// A parked job waits at most claim_wait for a worker; then the step ends
	// partial(busy) and the carried profiles apply. No worker ended it, so
	// the load counts its end, which an alert on busy ends watches.
	t.Run("no worker gives busy", func(t *testing.T) {
		f := newStepFixture(t)
		f.enriched(people(ada))
		busy := testutil.ToFloat64(steps.WithLabelValues("partial", "busy"))
		waited := samples(t, "helix_foundry_enrichment_claim_wait_seconds")
		parkedBefore := samples(t, "helix_foundry_enrichment_parked_seconds")
		job := f.newLoad()
		next := f.step(job, people(ada, grace))
		if next.Apply != nil || !next.Until.Equal(f.clock.now().Add(5*time.Minute)) {
			t.Fatalf("next = %+v, want a park until claim_by", next)
		}
		f.clock.advance(5 * time.Minute)
		next = f.step(job, people(ada, grace))
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentPartial || *row.StoppedBy != model.FoundryEnrichmentStopBusy {
			t.Fatalf("step = %s %v", row.Status, row.StoppedBy)
		}
		if rows := f.profiles(next.Apply); len(rows) != 1 {
			t.Fatalf("applied %d profiles, want Ada's carried", len(rows))
		}
		if f.work("w1") {
			t.Fatal("a busy step was claimed")
		}
		f.step(job, people())
		if got := testutil.ToFloat64(steps.WithLabelValues("partial", "busy")); got != busy+1 {
			t.Fatalf("busy ends counted %v, want %v", got, busy+1)
		}
		if got := samples(t, "helix_foundry_enrichment_claim_wait_seconds"); got != waited+1 {
			t.Fatalf("claim waits observed %d, want %d: the busy step waited out claim_wait", got, waited+1)
		}
		if got := samples(t, "helix_foundry_enrichment_parked_seconds"); got != parkedBefore+1 {
			t.Fatalf("parked ages observed %d, want %d", got, parkedBefore+1)
		}
	})

	// An attempt that died between queuing its step and parking its job is
	// claimed again while the step is still queued: it parks until claim_by,
	// not every few seconds asking for a busy end that can't come before
	// claim_by, each time a whole attempt.
	t.Run("a queued step before its claim_by parks until then", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.newLoad()
		first := f.step(job, people(ada))
		f.clock.advance(time.Minute)
		next := f.step(job, people())
		row := f.row(job.LoadID)
		if next.Apply != nil || !next.Until.Equal(*row.ClaimBy) || !next.Until.Equal(first.Until) || row.Status != model.FoundryEnrichmentQueued {
			t.Fatalf("next = %+v after %+v, step %s until %s", next, first, row.Status, row.ClaimBy)
		}
	})

	// claim_by is written from the attempt's clock, but the step ends busy
	// by the database's. While the attempt's clock runs ahead, the step stays
	// queued: the job parks a few seconds and looks again, rather than
	// ending it busy, unchanged, again and again at once.
	t.Run("a claim_by passed only by the attempt's clock parks briefly", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.newLoad()
		f.step(job, people(ada))
		f.clock.advance(5 * time.Minute)
		f.db.behind = time.Second
		next := f.step(job, people())
		if next.Apply != nil || !next.Until.Equal(f.clock.now().Add(busyRecheck)) || f.row(job.LoadID).Status != model.FoundryEnrichmentQueued {
			t.Fatalf("next = %+v, step %s", next, f.row(job.LoadID).Status)
		}
		f.db.behind = 0
		if next := f.step(job, people()); next.Apply == nil || *f.row(job.LoadID).StoppedBy != model.FoundryEnrichmentStopBusy {
			t.Fatalf("once the database's clock is past claim_by: %+v", next)
		}
	})

	// A Stop that comes while the step is prepared ends it before it is
	// queued: the load never parks, so no parked age is observed, and its
	// end is counted here, as no worker will.
	t.Run("a Stop while preparing counts its end, not a park", func(t *testing.T) {
		f := newStepFixture(t)
		f.enriched(people(ada))
		stopped := testutil.ToFloat64(steps.WithLabelValues("partial", "stopped"))
		parkedBefore := samples(t, "helix_foundry_enrichment_parked_seconds")
		job := f.newLoad()
		next, err := f.loadSide().Step(context.Background(), job, "graph", f.item, func(ctx context.Context, add func(Clue) error) (int64, error) {
			f.db.stop(job.LoadID)
			return people(ada, grace)(ctx, add)
		})
		if err != nil || next.Apply == nil || len(f.profiles(next.Apply)) != 1 {
			t.Fatalf("next = (%+v, %v), want Ada's carried profile", next, err)
		}
		if got := testutil.ToFloat64(steps.WithLabelValues("partial", "stopped")); got != stopped+1 {
			t.Fatalf("stopped ends counted %v, want %v", got, stopped+1)
		}
		if got := samples(t, "helix_foundry_enrichment_parked_seconds"); got != parkedBefore {
			t.Fatalf("parked ages observed %d, want %d: the load never parked", got, parkedBefore)
		}
	})

	// A worker that dies mid-step holds the load at most load_wait and the
	// grace: then the carried profiles apply. Its step, finished late by
	// another worker, still publishes, and the next load carries what it
	// found.
	t.Run("a dead worker: carried applies and the late publish is carried next load", func(t *testing.T) {
		f := newStepFixture(t)
		f.enriched(people(ada))
		var many []Clue
		for i := range 60 {
			many = append(many, person(fmt.Sprintf("p_%02d", i), "Pat", fmt.Sprintf("Num%s", strings.Repeat("x", i+1)), "acme.io"))
		}
		job := f.newLoad()
		f.step(job, people(append(many, ada)...))
		stopping, die := context.WithCancel(context.Background())
		f.db.afterCharge = func(int) { die() }
		w := f.worker("w1")
		step, _ := w.Claim(context.Background())
		if err := w.Run(stopping, step, f.clock.now()); err != nil {
			t.Fatal(err)
		}
		f.db.afterCharge = nil
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentRunning || row.BatchCursor != 1 {
			t.Fatalf("after the first batch: %s at %d", row.Status, row.BatchCursor)
		}
		if next := f.step(job, people()); next.Apply != nil || !next.Until.Equal(step.Deadline.Add(2*time.Minute)) {
			t.Fatalf("next = %+v, want a park until the deadline and grace", next)
		}

		f.clock.advance(time.Hour + 2*time.Minute)
		next := f.step(job, people())
		if rows := f.profiles(next.Apply); len(rows) != 1 {
			t.Fatalf("applied %d profiles, want Ada's carried", len(rows))
		}
		requests := len(f.dev.sent())
		if !f.work("w2") {
			t.Fatal("the dead worker's step wasn't claimed again")
		}
		row := f.row(job.LoadID)
		if row.Status != model.FoundryEnrichmentPartial || *row.StoppedBy != model.FoundryEnrichmentStopTime || row.Counts.Found != 50 {
			t.Fatalf("the late step = %s %v %+v", row.Status, row.StoppedBy, row.Counts)
		}
		if len(f.dev.sent()) != requests {
			t.Fatal("the late worker called again")
		}
		if again := f.step(job, people()); *again.Apply != *next.Apply {
			t.Fatal("the load's choice changed after the late publish")
		}

		later := f.newLoad()
		requests = len(f.dev.sent())
		f.step(later, people(append(many, ada)...))
		if row := f.row(later.LoadID); row.Counts.Carried != 51 || row.Counts.ToLookUp != 10 {
			t.Fatalf("the next load carried %d and looks up %d", row.Counts.Carried, row.Counts.ToLookUp)
		}
		if len(f.dev.sent()) != requests {
			t.Fatal("planning called the provider")
		}
	})

	// Stop ends a queued step at once: its job wakes to the carried
	// profiles.
	t.Run("stopping while queued applies the carried", func(t *testing.T) {
		f := newStepFixture(t)
		f.enriched(people(ada))
		job := f.newLoad()
		f.step(job, people(ada, grace))
		f.db.stop(job.LoadID)
		if f.db.woken(job.ID) != 1 {
			t.Fatal("the stop didn't wake the job")
		}
		next := f.step(job, people())
		if row := f.row(job.LoadID); *row.StoppedBy != model.FoundryEnrichmentStopStopped || len(f.profiles(next.Apply)) != 1 {
			t.Fatalf("step = %v applying %+v", row.StoppedBy, next.Apply)
		}
	})

	// Deleting the ontology, or withdrawing consent, while a job is parked
	// cancels its step, and the job wakes to apply no profiles.
	for name, gone := range map[string]bool{"the ontology deleted": true, "consent withdrawn": false} {
		t.Run(name+" while parked applies none", func(t *testing.T) {
			f := newStepFixture(t)
			f.enriched(people(ada))
			job := f.newLoad()
			f.step(job, people(ada, grace))
			f.db.withdraw("ws", "ont", gone)
			next := f.step(job, people())
			if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentCancelled || *next.Apply != model.FoundryEnrichmentNone {
				t.Fatalf("step = %s applying %+v", row.Status, next.Apply)
			}
		})
	}

	// Consent withdrawn after the step published, before the load applied
	// it: the purged profiles never apply.
	t.Run("a purged step applies none", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.newLoad()
		f.step(job, people(ada))
		f.work("w1")
		f.db.withdraw("ws", "ont", false)
		if next := f.step(job, people()); *next.Apply != model.FoundryEnrichmentNone {
			t.Fatalf("applied %+v after consent was withdrawn", next.Apply)
		}
	})

	// The planner's filters run before anything is sent: no full name, a
	// LinkedIn URL that names no profile, free mail and role mailboxes. Clues
	// sends every person, so those it left nameless or without a clue are
	// skipped and counted here too.
	t.Run("plans only usable people", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.newLoad()
		f.step(job, sharing(1, []Clue{
			ada,
			{Entity: "c_2", Name: "Cher", Email: "cher@acme.io"},
			{Entity: "c_3", Name: "Max Mustermann", Email: "max@gmail.com"},
			{Entity: "c_4", Name: "Sam Jones", Email: "sales@acme.io"},
			{Entity: "c_5", Name: "Lin Wu", LinkedInURL: "https://www.linkedin.com/sales/lead/123"},
			{Entity: "c_6", Name: "Ola Berg", Email: "ola@gmail.com", LinkedInURL: "linkedin.com/in/ola-berg"},
			{Entity: "c_7", Name: "Kim Park", LinkedInURL: "linkedin.com/in/shared"},
			{Entity: "c_8", Name: "Nia Obi"},
			{Entity: "c_9"},
		}...))
		counts := f.row(job.LoadID).Counts
		want := model.FoundryEnrichmentCounts{People: 9, NoName: 2, NoClue: 4, Shared: 1, Skipped: 6, ToLookUp: 3}
		if counts != want {
			t.Fatalf("counts = %+v, want %+v", counts, want)
		}
		plan, err := ReadAll[Entry](context.Background(), f.store, *f.row(job.LoadID).PlanKey)
		if err != nil {
			t.Fatal(err)
		}
		var calls []string
		for _, e := range plan {
			if e.Action == ActionCall {
				calls = append(calls, e.Entity)
			}
		}
		if !slices.Equal(calls, []string{"c_17", "c_6", "c_7"}) {
			t.Fatalf("calls = %v", calls)
		}
	})
}
