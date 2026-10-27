package enrichment

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// crowd is n people the fake provider finds, each at acme.io.
func crowd(n int) []Clue {
	var out []Clue
	for i := range n {
		out = append(out, person(fmt.Sprintf("p_%03d", i), "Pat", "N"+strings.Repeat("o", i+1), "acme.io"))
	}
	return out
}

// queued prepares a load of found and returns its job, parked.
func (f *stepFixture) queued(found ReadPeople) *model.FoundryGraphJob {
	f.t.Helper()
	job := f.newLoad()
	if next := f.step(job, found); next.Apply != nil {
		f.t.Fatalf("load %s applied %+v without lookups", job.LoadID, next.Apply)
	}
	return job
}

// handedOn runs the first claim of a queued step until SIGTERM comes with
// its first batch's lookups going out: the requests already sent answer,
// and the batch is handed on in flight with the rest unsent. It returns how
// many were sent.
func (f *stepFixture) handedOn() int {
	f.t.Helper()
	stopping, sigterm := context.WithCancel(context.Background())
	f.dev.setAnswer(func(string, string) (int, string, bool) {
		sigterm()
		time.Sleep(100 * time.Millisecond) // within the client's timeout
		return 0, "", false
	})
	w := f.worker("w1")
	step, err := w.Claim(context.Background())
	if err != nil || step == nil {
		f.t.Fatalf("claimed %v: %v", step, err)
	}
	if err := w.Run(stopping, step, f.clock.now()); err != nil {
		f.t.Fatal(err)
	}
	f.dev.setAnswer(nil)
	sent := len(f.dev.sent())
	if row := f.row(step.LoadID); row.InflightCredits == 0 || sent == 0 || sent >= BatchSize {
		f.t.Fatalf("handed on with %d in flight after %d requests", row.InflightCredits, sent)
	}
	return sent
}

// ended checks a step's end.
func (f *stepFixture) ended(loadID string, status model.FoundryEnrichmentStatus, why model.FoundryEnrichmentStop) *model.FoundryLoadEnrichment {
	f.t.Helper()
	row := f.row(loadID)
	if row.Status != status || (why == "") != (row.StoppedBy == nil) || (row.StoppedBy != nil && *row.StoppedBy != why) {
		f.t.Fatalf("step %s = %s %v, want %s %s", loadID, row.Status, row.StoppedBy, status, why)
	}
	return row
}

func TestWorkerRun(t *testing.T) {
	// The gate keeps only matches that agree with the person: a wrong
	// person, a low score and a profile two people share are held (and
	// charged), a miss is free, and only Ada's profile applies, slim.
	t.Run("gates matches and publishes the accepted, slim", func(t *testing.T) {
		f := newStepFixture(t)
		done := testutil.ToFloat64(steps.WithLabelValues("done", ""))
		waited := samples(t, "helix_foundry_enrichment_claim_wait_seconds")
		job := f.queued(people(
			person("c_1", "Ada", "Lovelace", "acme.io"),
			Clue{Entity: "c_2", Name: "Carol Jones", Email: "carol.jones.other@acme.io"},
			Clue{Entity: "c_3", Name: "Dave King", Email: "dave.king.low@acme.io"},
			Clue{Entity: "c_4", Name: "Erin West", Email: "erin.west.bad@acme.io"},
			Clue{Entity: "c_5", Name: "Frank Lee", Email: "frank.lee.dup@acme.io"},
			Clue{Entity: "c_6", Name: "Gina Ray", Email: "gina.ray.dup@acme.io"},
		))
		f.work("w1")
		row := f.ended(job.LoadID, model.FoundryEnrichmentDone, "")
		want := []string{"accepted", "held:duplicate_profile", "held:duplicate_profile", "held:low_score", "held:name_mismatch", "not_found"}
		if got := outcomes(f.results(job.LoadID)); !slices.Equal(got, want) {
			t.Fatalf("outcomes = %v, want %v", got, want)
		}
		if credits, uncertain := f.db.charged(); credits != 100 || uncertain != 0 || row.Counts.Credits != 100 {
			t.Fatalf("charged %d (+%d uncertain), counted %d; want 5 matches at 20", credits, uncertain, row.Counts.Credits)
		}
		if c := row.Counts; c.LookedUp != 6 || c.Found != 1 || c.Held != 4 || c.NotFound != 1 || c.Profiles != 1 {
			t.Fatalf("counts = %+v", c)
		}
		if f.db.woken(job.ID) != 1 {
			t.Fatal("the step's end didn't wake its load")
		}
		if got := testutil.ToFloat64(steps.WithLabelValues("done", "")); got != done+1 {
			t.Fatalf("done ends counted %v, want %v", got, done+1)
		}
		if got := samples(t, "helix_foundry_enrichment_claim_wait_seconds"); got != waited+1 {
			t.Fatalf("claim waits observed %d, want %d", got, waited+1)
		}
		rows := f.profiles(f.step(job, people()).Apply)
		if len(rows) != 1 {
			t.Fatalf("applied %d profiles, want Ada's", len(rows))
		}
		var fields []string
		for k := range rows[0] {
			fields = append(fields, k)
		}
		slices.Sort(fields)
		wantFields := []string{"clue_email", "clue_name", "current_title", "experience", "linkedin_url", "location", "location_country", "profile_id"}
		if !slices.Equal(fields, wantFields) {
			t.Fatalf("a published profile holds %v, want %v", fields, wantFields)
		}
		if rows[0]["profile_id"] != "li:ada-lovelace" || rows[0]["clue_name"] != "Ada Lovelace" {
			t.Fatalf("profile = %v", rows[0])
		}
	})

	// A refused key or credits halts lookups everywhere for 15 minutes and
	// ends the step partial: it still publishes, so the load goes on.
	t.Run("a 402 halts lookups and the load still applies", func(t *testing.T) {
		f := newStepFixture(t)
		f.dev.setAnswer(func(string, string) (int, string, bool) { return 402, `{"error_code":"INSUFFICIENT_CREDITS"}`, true })
		job := f.queued(people(crowd(60)...))
		halted := testutil.ToFloat64(steps.WithLabelValues("partial", "provider_credits"))
		f.work("w1")
		row := f.ended(job.LoadID, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopProviderCredits)
		if got := testutil.ToFloat64(steps.WithLabelValues("partial", "provider_credits")); got != halted+1 {
			t.Fatalf("provider_credits ends counted %v, want %v", got, halted+1)
		}
		halt, _ := f.db.GetFoundryEnrichmentProvider(context.Background(), "contextdev")
		if !halt.Halted(f.clock.now().Add(14*time.Minute)) || halt.Halted(f.clock.now().Add(16*time.Minute)) || *halt.HaltCode != "402" {
			t.Fatalf("halt = %+v", halt)
		}
		if credits, _ := f.db.charged(); credits != 0 || row.ProfilesKey == nil || len(f.dev.sent()) > BatchSize {
			t.Fatalf("charged %d after %d calls; published %v", credits, len(f.dev.sent()), row.ProfilesKey)
		}
		if next := f.step(job, people()); next.Apply == nil {
			t.Fatal("the load still waits")
		}
	})

	// A 429 is a refusal before any work: it is waited out and sent again,
	// and never charged.
	t.Run("a 429 storm is never charged", func(t *testing.T) {
		f := newStepFixture(t)
		var refused atomic.Int32
		f.dev.setAnswer(func(string, string) (int, string, bool) { return 429, `{}`, refused.Add(1) <= 20 })
		job := f.queued(people(crowd(10)...))
		f.work("w1")
		row := f.ended(job.LoadID, model.FoundryEnrichmentDone, "")
		if credits, uncertain := f.db.charged(); credits != 200 || uncertain != 0 || row.Counts.Found != 10 {
			t.Fatalf("charged %d (+%d uncertain) for %d found", credits, uncertain, row.Counts.Found)
		}
		if len(f.dev.sent()) != 30 {
			t.Fatalf("%d requests, want 20 refused and 10 answered", len(f.dev.sent()))
		}
	})

	// context.dev refuses a lookup it can't make (a 400 for too few clues, a
	// 422 for a disposable address) before doing any work: the person is
	// skipped, free, and looked up again next load, never carried as not
	// found for 90 days.
	t.Run("a refused lookup is skipped, free, and looked up next load", func(t *testing.T) {
		f := newStepFixture(t)
		ada := person("c_1", "Ada", "Lovelace", "acme.io")
		few := person("c_2", "Carol", "Jones", "acme.io")
		disposable := person("c_3", "Dave", "King", "acme.io")
		f.dev.setAnswer(func(email, _ string) (int, string, bool) {
			switch email {
			case few.Email:
				return 400, `{"error_code":"INSUFFICIENT_CLUES"}`, true
			case disposable.Email:
				return 422, `{"error_code":"DISPOSABLE_EMAIL_DETECTED"}`, true
			}
			return 0, "", false
		})
		job := f.queued(people(ada, few, disposable))
		f.work("w1")
		row := f.ended(job.LoadID, model.FoundryEnrichmentDone, "")
		if got, want := outcomes(f.results(job.LoadID)), []string{"accepted", "skipped:refused", "skipped:refused"}; !slices.Equal(got, want) {
			t.Fatalf("outcomes = %v, want %v", got, want)
		}
		if credits, uncertain := f.db.charged(); credits != 20 || uncertain != 0 || row.Counts.Credits != 20 || row.Counts.NotFound != 0 {
			t.Fatalf("charged %d (+%d uncertain): %+v", credits, uncertain, row.Counts)
		}
		f.dev.setAnswer(nil)
		next := f.newLoad()
		f.step(next, people(ada, few, disposable))
		if counts := f.row(next.LoadID).Counts; counts.ToLookUp != 2 || counts.Carried != 1 {
			t.Fatalf("the next load carries %d and looks up %d", counts.Carried, counts.ToLookUp)
		}
	})

	// context.dev saying its answer is unfinished: a match held for what it
	// left out is charged and looked up next load; one accepted is applied
	// and carried like any other.
	t.Run("a partial answer held is looked up next load, one accepted is carried", func(t *testing.T) {
		f := newStepFixture(t)
		ada := person("c_1", "Ada", "Lovelace", "acme.io")
		unfinished := Clue{Entity: "c_2", Name: "Carol Jones", Email: "carol.jones.partial@acme.io"}
		bySlug := Clue{Entity: "c_3", Name: "Dave King", LinkedInURL: "https://www.linkedin.com/in/dave-king-partial"}
		job := f.queued(people(ada, unfinished, bySlug))
		f.work("w1")
		row := f.ended(job.LoadID, model.FoundryEnrichmentDone, "")
		var partial []string
		for _, r := range f.results(job.LoadID) {
			if r.Partial {
				partial = append(partial, strings.TrimSuffix(string(r.Outcome)+":"+r.Reason, ":"))
			}
		}
		slices.Sort(partial)
		if want := []string{"accepted", "held:domain_unknown"}; !slices.Equal(partial, want) {
			t.Fatalf("partial results = %v, want %v", partial, want)
		}
		if credits, _ := f.db.charged(); credits != 60 || row.Counts.Found != 2 || row.Counts.Held != 1 || row.Counts.Profiles != 2 {
			t.Fatalf("charged %d: %+v", credits, row.Counts)
		}
		next := f.newLoad()
		f.step(next, people(ada, unfinished, bySlug))
		if counts := f.row(next.LoadID).Counts; counts.ToLookUp != 1 || counts.Carried != 2 {
			t.Fatalf("the next load carries %d and looks up %d", counts.Carried, counts.ToLookUp)
		}
	})

	// A call that gets no answer may have been charged: it is booked
	// uncertain, never sent again in this load, and looked up again next
	// load.
	t.Run("a timeout is not resent", func(t *testing.T) {
		f := newStepFixture(t)
		slow := Clue{Entity: "c_9", Name: "Tim Out", Email: "tim.out.timeout@acme.io"}
		job := f.queued(people(slow))
		f.work("w1")
		row := f.ended(job.LoadID, model.FoundryEnrichmentDone, "")
		if _, uncertain := f.db.charged(); uncertain != 20 || row.Counts.Uncertain != 1 || len(f.dev.sent()) != 1 {
			t.Fatalf("uncertain %d, counted %d, after %d requests", uncertain, row.Counts.Uncertain, len(f.dev.sent()))
		}
		next := f.newLoad()
		f.step(next, people(slow))
		if row := f.row(next.LoadID); row.Counts.ToLookUp != 1 || row.Counts.Carried != 0 {
			t.Fatalf("the next load = %+v", row.Counts)
		}
	})

	// Ten failed calls in a row mean context.dev is down: the step stops
	// partial(provider) after its batch, and the rest wait for the next
	// load. Each failed call is sent once more first.
	t.Run("a 5xx streak ends partial(provider)", func(t *testing.T) {
		f := newStepFixture(t)
		f.dev.setAnswer(func(string, string) (int, string, bool) { return 503, `{}`, true })
		job := f.queued(people(crowd(60)...))
		failing := testutil.ToFloat64(steps.WithLabelValues("partial", "provider"))
		f.work("w1")
		f.ended(job.LoadID, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopProvider)
		if got := testutil.ToFloat64(steps.WithLabelValues("partial", "provider")); got != failing+1 {
			t.Fatalf("provider ends counted %v, want %v", got, failing+1)
		}
		results := f.results(job.LoadID)
		if len(results) == 0 || len(results) > BatchSize || outcomes(results)[0] != "skipped:provider_error" {
			t.Fatalf("results = %v", outcomes(results))
		}
		if credits, _ := f.db.charged(); credits != 0 || len(f.dev.sent()) > 2*BatchSize {
			t.Fatalf("charged %d after %d requests for %d people", credits, len(f.dev.sent()), len(results))
		}
	})

	// Credits are reserved up front and every batch is shrunk to what is
	// left: the cap stops the step partial(cap), and the people left wait.
	t.Run("the cap shrinks a batch and ends partial(cap)", func(t *testing.T) {
		f := newStepFixture(t)
		f.db.consents["ws"].MonthlyCreditCap = 100
		job := f.queued(people(crowd(12)...))
		f.work("w1")
		row := f.ended(job.LoadID, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopCap)
		if credits, _ := f.db.charged(); credits != 100 || len(f.dev.sent()) != 5 || row.Counts.Found != 5 {
			t.Fatalf("charged %d after %d calls", credits, len(f.dev.sent()))
		}
		if ledger, _ := f.db.GetFoundryEnrichmentLedger(context.Background(), "ws", monthStart(f.clock.now())); ledger.WorkspaceReserved != 0 {
			t.Fatalf("the ended step still reserves %d", ledger.WorkspaceReserved)
		}
	})

	// A batch's free answers (not found, refused, failed) leave reserved
	// what they didn't cost: each charge cuts the step's reservation to what
	// its calls left may cost, so other steps can reserve what it can no
	// longer spend.
	t.Run("a step's reservation shrinks to what its calls left may cost", func(t *testing.T) {
		f := newStepFixture(t)
		var misses []Clue
		for i := range BatchSize + 10 {
			misses = append(misses, Clue{Entity: fmt.Sprintf("m_%03d", i), Name: "Pat Bad", Email: fmt.Sprintf("pat.bad%03d@acme.io", i)})
		}
		job := f.queued(people(misses...))
		var reserved []int64
		f.db.afterCharge = func(int) { reserved = append(reserved, f.row(job.LoadID).ReservedCredits) }
		f.work("w1")
		f.ended(job.LoadID, model.FoundryEnrichmentDone, "")
		if want := []int64{10 * 20, 0}; !slices.Equal(reserved, want) {
			t.Fatalf("reserved %v after each batch, want %v", reserved, want)
		}
	})

	// The deadline stops a step partial(time) at its next batch; it
	// publishes what it found.
	t.Run("the deadline ends partial(time)", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(60)...))
		f.db.afterCharge = func(int) { f.clock.advance(time.Hour) }
		f.work("w1")
		row := f.ended(job.LoadID, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopTime)
		if row.Counts.Found != BatchSize || row.Counts.Profiles != BatchSize || len(f.dev.sent()) != BatchSize {
			t.Fatalf("counts = %+v after %d calls", row.Counts, len(f.dev.sent()))
		}
	})

	// A lease no renewal confirmed for a while may lapse: no call starts
	// that could outlive it (a call's timeout before the lease ends), and
	// the step is handed on.
	t.Run("an unconfirmed lease stops calls and hands the step on", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(60)...))
		f.db.afterCharge = func(int) { f.clock.advance(f.cfg.Lease - f.worker("").Client.HTTP.Timeout) }
		f.work("w1")
		f.db.afterCharge = nil
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentRunning || row.BatchCursor != 1 || !row.LeaseUntil.Before(f.clock.now()) {
			t.Fatalf("step = %s at %d", row.Status, row.BatchCursor)
		}
		f.work("w2")
		f.ended(job.LoadID, model.FoundryEnrichmentDone, "")
		if len(f.dev.sent()) != 60 {
			t.Fatalf("%d requests for 60 people", len(f.dev.sent()))
		}
	})

	// SIGTERM: no new call starts, the batch in flight settles and the step
	// is handed to the next worker at once, which goes on from the cursor.
	t.Run("SIGTERM settles the batch and hands the step on", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(120)...))
		stopping, sigterm := context.WithCancel(context.Background())
		f.db.afterCharge = func(int) { sigterm() }
		w := f.worker("w1")
		step, _ := w.Claim(context.Background())
		if err := w.Run(stopping, step, f.clock.now()); err != nil {
			t.Fatal(err)
		}
		f.db.afterCharge = nil
		row := f.row(job.LoadID)
		if row.Status != model.FoundryEnrichmentRunning || row.BatchCursor != 1 || row.InflightCredits != 0 || !row.LeaseUntil.Before(f.clock.now()) {
			t.Fatalf("after SIGTERM: %s at %d, %d in flight", row.Status, row.BatchCursor, row.InflightCredits)
		}
		f.work("w2")
		f.ended(job.LoadID, model.FoundryEnrichmentDone, "")
		if sent := f.dev.sent(); len(sent) != 120 || len(slices.Compact(slices.Sorted(slices.Values(sent)))) != 120 {
			t.Fatalf("%d requests for 120 people", len(sent))
		}
	})

	// SIGTERM mid-batch: no call starts, but the requests already sent run
	// to their answers. Cut short, each would be a call context.dev may have
	// charged, booked uncertain and lost to this load; and enough of them
	// would end the step partial(provider) instead of handing it on. The
	// batch is handed on in flight with its calls so far, and the next claim
	// sends the lookups not sent: everyone is looked up once, and the step
	// isn't done with people skipped.
	t.Run("SIGTERM lets the calls already sent answer", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(120)...))
		stopping, sigterm := context.WithCancel(context.Background())
		f.dev.setAnswer(func(string, string) (int, string, bool) {
			sigterm()
			time.Sleep(100 * time.Millisecond) // within the client's timeout
			return 0, "", false
		})
		w := f.worker("w1")
		step, _ := w.Claim(context.Background())
		if err := w.Run(stopping, step, f.clock.now()); err != nil {
			t.Fatal(err)
		}
		f.dev.setAnswer(nil)
		sent := len(f.dev.sent())
		row := f.row(job.LoadID)
		if row.Status != model.FoundryEnrichmentRunning || row.BatchCursor != 0 || row.InflightCredits != BatchSize*20 ||
			!row.LeaseUntil.Before(f.clock.now()) {
			t.Fatalf("after SIGTERM: %s at %d, %d in flight", row.Status, row.BatchCursor, row.InflightCredits)
		}
		if credits, uncertain := f.db.charged(); sent == 0 || sent >= BatchSize || credits != 0 || uncertain != 0 {
			t.Fatalf("charged %d (+%d uncertain) with %d sent", credits, uncertain, sent)
		}
		f.work("w2")
		row = f.ended(job.LoadID, model.FoundryEnrichmentDone, "")
		requests := f.dev.sent()
		if len(requests) != 120 || len(slices.Compact(slices.Sorted(slices.Values(requests)))) != 120 {
			t.Fatalf("%d requests for 120 people, %d sent before SIGTERM", len(requests), sent)
		}
		if credits, uncertain := f.db.charged(); credits != 120*20 || uncertain != 0 || row.Counts.LookedUp != 120 || row.Counts.Found != 120 {
			t.Fatalf("charged %d (+%d uncertain): %+v", credits, uncertain, row.Counts)
		}
	})

	// A crash while a claim sends what the batch it took over left unsent
	// loses those lookups' answers: they are booked uncertain, as calls that
	// may have been charged, and never sent a third time.
	t.Run("a crash sending what a batch left unsent books it uncertain", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(BatchSize)...))
		sent := f.handedOn()
		puts := 0
		f.store.failPut = func(key string) bool { // the second write of calls/0: after the lookups were sent
			if strings.Contains(key, "/calls/") {
				puts++
			}
			return puts == 2
		}
		f.work("w2")
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentRunning || row.BatchCursor != 0 || len(f.dev.sent()) != BatchSize {
			t.Fatalf("after the crash: %s at %d, %d requests", row.Status, row.BatchCursor, len(f.dev.sent()))
		}
		f.clock.advance(f.cfg.Lease + time.Second) // the failed claim's lease lapses
		f.work("w3")
		row := f.ended(job.LoadID, model.FoundryEnrichmentDone, "")
		if requests := f.dev.sent(); len(requests) != BatchSize || len(slices.Compact(slices.Sorted(slices.Values(requests)))) != BatchSize {
			t.Fatalf("%d requests for %d people", len(requests), BatchSize)
		}
		lost := int64(BatchSize - sent)
		if credits, uncertain := f.db.charged(); credits != int64(sent)*20 || uncertain != lost*20 || row.Counts.Uncertain != lost {
			t.Fatalf("charged %d (+%d uncertain) with %d answered before the crash: %+v", credits, uncertain, sent, row.Counts)
		}
	})

	// A batch handed on mid-way whose step ends before the next claim sends
	// the rest (a user's Stop meanwhile) settles as it stands: the answers
	// it has are charged and published, the lookups not sent wait for the
	// next load, and nothing more is sent.
	t.Run("a batch handed on, then stopped, settles as it stands", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(BatchSize)...))
		sent := f.handedOn()
		f.db.stop(job.LoadID)
		f.work("w2")
		row := f.ended(job.LoadID, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopStopped)
		if credits, uncertain := f.db.charged(); len(f.dev.sent()) != sent || credits != int64(sent)*20 || uncertain != 0 ||
			row.Counts.LookedUp != int64(sent) || row.Counts.Profiles != int64(sent) || row.InflightCredits != 0 {
			t.Fatalf("%d requests after %d, charged %d (+%d uncertain): %+v", len(f.dev.sent()), sent, credits, uncertain, row.Counts)
		}
	})

	// A batch handed on mid-way whose step then fails past the late window
	// settles as it stands before the step ends failed: what was answered is
	// charged exactly, not booked uncertain.
	t.Run("a batch handed on, then failing past the late window, settles as it stands", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(BatchSize)...))
		sent := f.handedOn()
		f.clock.advance(time.Hour + 2*time.Minute + lateFor)
		f.db.failCheck = true
		f.work("w2")
		row := f.ended(job.LoadID, model.FoundryEnrichmentFailed, "")
		if credits, uncertain := f.db.charged(); len(f.dev.sent()) != sent || credits != int64(sent)*20 || uncertain != 0 || row.InflightCredits != 0 {
			t.Fatalf("%d requests after %d, charged %d (+%d uncertain)", len(f.dev.sent()), sent, credits, uncertain)
		}
	})

	// The claim that takes a batch handed on mid-way over begins no batch,
	// and its lease's renewal has read nothing yet: its re-check is all that
	// keeps the lookups left unsent from going out once consent was
	// withdrawn or the ontology deleted. Each reason the re-check reads
	// cancels the step (the first two as the provider records them, the
	// rest alone): the batch settles as it stands, its answers charged
	// exactly, nothing more is sent and nothing is published.
	for name, cancel := range map[string]func(f *stepFixture, loadID string){
		"consent withdrawn":    func(f *stepFixture, _ string) { f.db.withdraw("ws", "ont", false) },
		"the ontology deleted": func(f *stepFixture, _ string) { f.db.withdraw("ws", "ont", true) },
		"a cancel":             func(f *stepFixture, loadID string) { f.db.steps[loadID].CancelRequested = true },
		"the ontology gone":    func(f *stepFixture, _ string) { f.db.gone["ont"] = true },
		"consent off":          func(f *stepFixture, _ string) { f.db.consents["ws"].Enabled = false },
		"no consent":           func(f *stepFixture, _ string) { delete(f.db.consents, "ws") },
	} {
		t.Run("a batch handed on, then "+name+", sends nothing more and ends cancelled", func(t *testing.T) {
			f := newStepFixture(t)
			job := f.queued(people(crowd(BatchSize)...))
			sent := f.handedOn()
			cancel(f, job.LoadID) // nothing runs between claims
			f.work("w2")
			row := f.ended(job.LoadID, model.FoundryEnrichmentCancelled, "")
			if credits, uncertain := f.db.charged(); len(f.dev.sent()) != sent || credits != int64(sent)*20 || uncertain != 0 ||
				row.InflightCredits != 0 || row.Counts.LookedUp != int64(sent) {
				t.Fatalf("%d requests after %d, charged %d (+%d uncertain): %+v", len(f.dev.sent()), sent, credits, uncertain, row.Counts)
			}
			if row.ProfilesKey != nil || row.ResultsKey != nil || f.db.woken(job.ID) != 1 {
				t.Fatalf("published %v %v, woke the load %d times", row.ProfilesKey, row.ResultsKey, f.db.woken(job.ID))
			}
		})
	}

	// A lease no renewal confirmed in the middle of a batch holds its
	// lookups not sent back: the batch is handed on in flight, and the next
	// claim sends them, so no one is skipped and the step isn't done short.
	t.Run("an unconfirmed lease mid-batch hands the rest of the batch on", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(BatchSize)...))
		var once sync.Once
		f.dev.setAnswer(func(string, string) (int, string, bool) {
			once.Do(func() { f.clock.advance(f.cfg.Lease - f.worker("").Client.HTTP.Timeout) })
			return 0, "", false
		})
		w := f.worker("w1")
		w.Client.Limiter = NewLimiter(1) // one call at a time: the fence holds back every other
		step, _ := w.Claim(context.Background())
		if err := w.Run(context.Background(), step, f.clock.now()); err == nil {
			t.Fatal("the unconfirmed lease went unreported")
		}
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentRunning || row.BatchCursor != 0 || len(f.dev.sent()) != 1 ||
			!row.LeaseUntil.Before(f.clock.now()) {
			t.Fatalf("after the fence: %s at %d after %d requests", row.Status, row.BatchCursor, len(f.dev.sent()))
		}
		f.work("w2")
		row := f.ended(job.LoadID, model.FoundryEnrichmentDone, "")
		if len(f.dev.sent()) != BatchSize || row.Counts.LookedUp != BatchSize || row.Counts.Found != BatchSize {
			t.Fatalf("%d requests: %+v", len(f.dev.sent()), row.Counts)
		}
	})

	// The deadline passing in the middle of the plan's last batch leaves
	// people not looked up: the step ends partial(time), the rest next load,
	// not done.
	t.Run("the deadline in the last batch ends partial(time)", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(10)...))
		var once sync.Once
		f.dev.setAnswer(func(string, string) (int, string, bool) {
			once.Do(func() { f.clock.advance(time.Hour) })
			return 0, "", false
		})
		w := f.worker("w1")
		w.Client.Limiter = NewLimiter(1)
		step, _ := w.Claim(context.Background())
		if err := w.Run(context.Background(), step, f.clock.now()); err != nil {
			t.Fatal(err)
		}
		row := f.ended(job.LoadID, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopTime)
		if row.Counts.LookedUp != 1 || row.Counts.ToLookUp != 10 || len(f.dev.sent()) != 1 {
			t.Fatalf("%d requests: %+v", len(f.dev.sent()), row.Counts)
		}
	})

	// A worker that lost its lease writes nothing more: the next owner
	// settles the batch from what was written, and nobody is called twice.
	t.Run("a stale lease is fenced", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(60)...))
		f.db.afterCharge = func(int) {
			f.db.mu.Lock()
			f.db.steps[job.LoadID].LeaseOwner = ptr("w2")
			f.db.mu.Unlock()
		}
		f.work("w1")
		f.db.afterCharge = nil
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentRunning || row.BatchCursor != 1 {
			t.Fatalf("the stale worker moved the step to %s at %d", row.Status, row.BatchCursor)
		}
		f.clock.advance(3 * time.Minute)
		f.work("w2")
		f.ended(job.LoadID, model.FoundryEnrichmentDone, "")
		if len(f.dev.sent()) != 60 {
			t.Fatalf("%d requests for 60 people", len(f.dev.sent()))
		}
	})

	// One process runs several steps under one host:pid. A claim of a step
	// whose lease lapsed while this process still held it (a run stuck on a
	// dead connection, say) is fenced against that run as against another
	// process: the stuck run writes and releases nothing, so the claim that
	// took the step over keeps it, and no one is looked up twice.
	t.Run("a step this process claims again fences the run still holding it", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(60)...))
		w := f.worker("w1")
		stuck, _ := w.Claim(context.Background())
		leased := f.clock.now()
		f.clock.advance(3 * time.Minute)
		taken, _ := w.Claim(context.Background())
		if taken == nil || taken.LoadID != job.LoadID || *taken.LeaseOwner == *stuck.LeaseOwner {
			t.Fatalf("claimed again as %v, first as %s", taken, *stuck.LeaseOwner)
		}
		if err := f.db.ReleaseFoundryLoadEnrichment(context.Background(), job.LoadID, *stuck.LeaseOwner); err == nil {
			t.Fatal("the stuck run released the claim that took its step over")
		}
		if err := w.Run(context.Background(), stuck, leased); err != nil {
			t.Fatal(err)
		}
		if row := f.row(job.LoadID); len(f.dev.sent()) != 0 || row.Status != model.FoundryEnrichmentRunning || *row.LeaseOwner != *taken.LeaseOwner {
			t.Fatalf("the stuck run made %d calls and left the step %s, leased to %s", len(f.dev.sent()), row.Status, *row.LeaseOwner)
		}
		if err := w.Run(context.Background(), taken, f.clock.now()); err != nil {
			t.Fatal(err)
		}
		f.ended(job.LoadID, model.FoundryEnrichmentDone, "")
		if credits, uncertain := f.db.charged(); len(f.dev.sent()) != 60 || credits != 60*20 || uncertain != 0 {
			t.Fatalf("%d requests for 60 people, charged %d (+%d uncertain)", len(f.dev.sent()), credits, uncertain)
		}
	})

	// A crash around each write of a batch is settled by the next worker
	// without a second call: from results/<b>, from calls/<b> judged again,
	// or, with neither, by booking what was in flight as uncertain.
	for _, crash := range []struct {
		name      string
		arm       func(f *stepFixture)
		uncertain int64
	}{
		{"before calls/<b> is written", func(f *stepFixture) {
			f.store.failPut = func(key string) bool { return strings.Contains(key, "/calls/") }
		}, 3 * 20},
		{"before results/<b> is written", func(f *stepFixture) {
			f.store.failPut = func(key string) bool { return strings.Contains(key, "/results/") }
		}, 0},
		{"before the charge lands", func(f *stepFixture) { f.db.failCharge = true }, 0},
		{"after the charge landed, its answer lost", func(f *stepFixture) { f.db.loseCharge = true }, 0},
		{"before publishing", func(f *stepFixture) {
			f.store.failPut = func(key string) bool { return strings.HasSuffix(key, "/profiles.jsonl.gz") }
		}, 0},
	} {
		t.Run("a crash "+crash.name+" makes no second call", func(t *testing.T) {
			f := newStepFixture(t)
			job := f.queued(people(append(crowd(2), Clue{Entity: "c_x", Name: "Erin West", Email: "erin.west.bad@acme.io"})...))
			crash.arm(f)
			f.work("w1")
			if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentRunning {
				t.Fatalf("the crashed step is %s", row.Status)
			}
			f.clock.advance(f.cfg.Lease + time.Second) // the failed claim's lease lapses
			f.work("w2")
			row := f.ended(job.LoadID, model.FoundryEnrichmentDone, "")
			if len(f.dev.sent()) != 3 {
				t.Fatalf("%d requests for 3 people", len(f.dev.sent()))
			}
			credits, uncertain := f.db.charged()
			if uncertain != crash.uncertain || (crash.uncertain == 0 && credits != 40) || row.Counts.Credits != credits+uncertain {
				t.Fatalf("charged %d (+%d uncertain), counted %d", credits, uncertain, row.Counts.Credits)
			}
			if crash.uncertain == 0 && row.Counts.Profiles != 2 {
				t.Fatalf("published %d profiles, want 2", row.Counts.Profiles)
			}
		})
	}

	// A step whose last batch ended it early (the cap shrank it, or the
	// provider kept failing) and whose publish then failed is finished by
	// the next claim the same way: not done, with people left unlooked-up.
	t.Run("a failed publish keeps a partial(cap) end", func(t *testing.T) {
		f := newStepFixture(t)
		f.db.consents["ws"].MonthlyCreditCap = 100
		job := f.queued(people(crowd(12)...))
		f.store.failPut = func(key string) bool { return strings.HasSuffix(key, "/profiles.jsonl.gz") }
		f.work("w1")
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentRunning || row.BatchCursor != 1 {
			t.Fatalf("after the failed publish: %s at %d", row.Status, row.BatchCursor)
		}
		f.clock.advance(f.cfg.Lease + time.Second) // the failed claim's lease lapses
		f.work("w2")
		row := f.ended(job.LoadID, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopCap)
		if row.Counts.LookedUp != 5 || row.Counts.Profiles != 5 || len(f.dev.sent()) != 5 {
			t.Fatalf("counts = %+v after %d calls", row.Counts, len(f.dev.sent()))
		}
	})
	// The streak that stops a step may span batches, counted as calls come
	// back: the batch it ends in says so in its calls (the lookups it held
	// back), so the next claim, finishing a step whose publish failed, ends
	// it partial(provider) too, with people left for the next load.
	t.Run("a failed publish keeps a partial(provider) end whose streak spans batches", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(60)...))
		f.dev.setAnswer(func(email, _ string) (int, string, bool) {
			local, _, _ := strings.Cut(email, "@")
			switch i := strings.Count(local, "o") - 1; {
			case i >= 45 && i < 50: // batch 0's last answers: five failures
				time.Sleep(50 * time.Millisecond)
				return 503, `{}`, true
			case i >= 50 && i < 55: // five more, in batch 1: ten in a row
				return 503, `{}`, true
			case i >= 55: // held up until the streak stops them
				time.Sleep(time.Millisecond)
				return 429, `{}`, true
			}
			return 0, "", false
		})
		var failed atomic.Bool
		f.store.failPut = func(key string) bool { return strings.HasSuffix(key, "/profiles.jsonl.gz") && !failed.Swap(true) }
		f.work("w1")
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentRunning || row.BatchCursor != 2 {
			t.Fatalf("after the failed publish: %s at %d", row.Status, row.BatchCursor)
		}
		requests := len(f.dev.sent())
		f.clock.advance(f.cfg.Lease + time.Second) // the failed claim's lease lapses
		f.work("w2")
		row := f.ended(job.LoadID, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopProvider)
		if row.Counts.LookedUp != 55 || len(f.dev.sent()) != requests {
			t.Fatalf("%d requests after the first claim's %d: %+v", len(f.dev.sent()), requests, row.Counts)
		}
	})
	// An end chosen before the plan's last batch survives a failed publish
	// too: the next claim reads it from the batch that ended the step and
	// calls the failing provider no more.
	t.Run("a failed publish of an end before the last batch makes no more calls", func(t *testing.T) {
		f := newStepFixture(t)
		f.dev.setAnswer(func(string, string) (int, string, bool) { return 503, `{}`, true })
		job := f.queued(people(crowd(3 * BatchSize)...))
		f.store.failPut = func(key string) bool { return strings.HasSuffix(key, "/profiles.jsonl.gz") }
		f.work("w1")
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentRunning || row.BatchCursor != 1 {
			t.Fatalf("after the failed publish: %s at %d", row.Status, row.BatchCursor)
		}
		requests := len(f.dev.sent())
		f.clock.advance(f.cfg.Lease + time.Second) // the failed claim's lease lapses
		f.work("w2")
		f.ended(job.LoadID, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopProvider)
		if len(f.dev.sent()) != requests {
			t.Fatalf("the next claim called %d more times", len(f.dev.sent())-requests)
		}
	})
	t.Run("a failed publish keeps a partial(provider) end", func(t *testing.T) {
		f := newStepFixture(t)
		f.dev.setAnswer(func(string, string) (int, string, bool) { return 503, `{}`, true })
		job := f.queued(people(crowd(30)...))
		f.store.failPut = func(key string) bool { return strings.HasSuffix(key, "/profiles.jsonl.gz") }
		f.work("w1")
		requests := len(f.dev.sent())
		f.clock.advance(f.cfg.Lease + time.Second) // the failed claim's lease lapses
		f.work("w2")
		f.ended(job.LoadID, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopProvider)
		if len(f.dev.sent()) != requests {
			t.Fatal("the next claim called again")
		}
	})

	// Consent withdrawn mid-step cancels it before its next batch, whenever
	// the lease's renewal would carry it: the batch in flight settles,
	// nothing is published, and the load applies no profiles.
	t.Run("consent withdrawn mid-step cancels without publishing", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(120)...))
		f.db.afterCharge = func(int) { f.db.withdraw("ws", "ont", false) }
		cancelled := testutil.ToFloat64(steps.WithLabelValues("cancelled", ""))
		f.work("w1")
		row := f.ended(job.LoadID, model.FoundryEnrichmentCancelled, "")
		if got := testutil.ToFloat64(steps.WithLabelValues("cancelled", "")); got != cancelled+1 {
			t.Fatalf("cancelled ends counted %v, want %v", got, cancelled+1)
		}
		if row.ProfilesKey != nil || row.ResultsKey != nil || len(f.dev.sent()) != BatchSize {
			t.Fatalf("published %v %v after %d calls", row.ProfilesKey, row.ResultsKey, len(f.dev.sent()))
		}
		if credits, _ := f.db.charged(); credits != BatchSize*20 {
			t.Fatalf("charged %d, want the settled batch's", credits)
		}
		if next := f.step(job, people()); *next.Apply != model.FoundryEnrichmentNone {
			t.Fatalf("applied %+v", next.Apply)
		}
	})

	// The flag, Pro and the ontology are re-checked every ten batches; off
	// Pro, the step stops partial(pro) and publishes what it found.
	t.Run("off Pro mid-step ends partial(pro) at the next re-check", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(12 * BatchSize)...))
		f.db.afterCharge = func(b int) {
			if b == 0 {
				f.pro.set(false)
			}
		}
		f.work("w1")
		row := f.ended(job.LoadID, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopPro)
		if row.Counts.Found != recheckEvery*BatchSize || row.Counts.Profiles != recheckEvery*BatchSize {
			t.Fatalf("counts = %+v", row.Counts)
		}
	})

	// A user's Stop of a running step ends it before its next batch,
	// whenever the lease's renewal would carry it: the batch settles and
	// what it found is published.
	t.Run("stopping while running publishes what was found", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(120)...))
		f.db.afterCharge = func(int) { f.db.stop(job.LoadID) }
		f.work("w1")
		row := f.ended(job.LoadID, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopStopped)
		if row.Counts.Profiles != BatchSize || len(f.dev.sent()) != BatchSize {
			t.Fatalf("published %d profiles after %d calls", row.Counts.Profiles, len(f.dev.sent()))
		}
	})

	// A halt is the provider's, whoever recorded it: an operator's during
	// an incident, or another step's after a 402, stops a step already
	// running at its next batch.
	t.Run("a halt recorded meanwhile ends partial(provider_credits)", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(120)...))
		f.db.afterCharge = func(int) {
			_ = f.db.HaltFoundryEnrichmentProvider(context.Background(), "contextdev", f.clock.now().Add(time.Hour), "operator")
		}
		f.work("w1")
		row := f.ended(job.LoadID, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopProviderCredits)
		if row.Counts.Profiles != BatchSize || len(f.dev.sent()) != BatchSize {
			t.Fatalf("published %d profiles after %d calls", row.Counts.Profiles, len(f.dev.sent()))
		}
	})

	// What ends a step reaches a batch still resending through a 429 storm
	// within one renewal of the lease, which reads it: an operator's halt
	// during an incident, consent withdrawn, a user's Stop. Left to the next
	// batch, the storm would hold it off until the deadline, an hour of
	// people's clues going out after consent was withdrawn.
	for name, tc := range map[string]struct {
		end       func(f *stepFixture, loadID string)
		status    model.FoundryEnrichmentStatus
		why       model.FoundryEnrichmentStop
		publishes bool
	}{
		"a halt recorded": {func(f *stepFixture, _ string) {
			_ = f.db.HaltFoundryEnrichmentProvider(context.Background(), "contextdev", f.clock.now().Add(time.Hour), "operator")
		}, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopProviderCredits, true},
		"consent withdrawn": {func(f *stepFixture, _ string) { f.db.withdraw("ws", "ont", false) },
			model.FoundryEnrichmentCancelled, "", false},
		"a Stop": {func(f *stepFixture, loadID string) { f.db.stop(loadID) }, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopStopped, true},
	} {
		t.Run(name+" during a 429 storm stops the batch's resends", func(t *testing.T) {
			f := newStepFixture(t)
			f.cfg.Lease = 600 * time.Millisecond // renewed every 200ms
			job := f.queued(people(crowd(10)...))
			var refused atomic.Int32
			f.dev.setAnswer(func(string, string) (int, string, bool) {
				if refused.Add(1) == 20 {
					tc.end(f, job.LoadID)
				}
				return 429, `{}`, true
			})
			w := f.worker("w1")
			w.Client.Sleep = func(ctx context.Context, _ time.Duration) error { return sleep(ctx, 5*time.Millisecond) }
			step, _ := w.Claim(context.Background())
			done := make(chan error, 1)
			go func() { done <- w.Run(context.Background(), step, f.clock.now()) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the batch kept resending past its end")
			}
			row := f.ended(job.LoadID, tc.status, tc.why)
			if credits, uncertain := f.db.charged(); credits != 0 || uncertain != 0 || row.Counts.LookedUp != 0 || (row.ProfilesKey != nil) != tc.publishes {
				t.Fatalf("charged %d (+%d uncertain), published %v: %+v", credits, uncertain, row.ProfilesKey, row.Counts)
			}
		})
	}

	// Loads are never deleted, so a step whose load can't be found is a
	// failure, tried again a lease later, never a lost lease: that would
	// leave the step to lapse and be claimed again, silently, for ever.
	t.Run("a step whose load is missing fails, and isn't taken for a lost lease", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(3)...))
		f.db.noLoad = true
		w := f.worker("w1")
		step, _ := w.Claim(context.Background())
		if err := w.Run(context.Background(), step, f.clock.now()); err == nil || !strings.Contains(err.Error(), "has no load") {
			t.Fatalf("run = %v, want the missing load reported", err)
		}
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentRunning || len(f.dev.sent()) != 0 {
			t.Fatalf("step %s after %d calls", row.Status, len(f.dev.sent()))
		}
	})

	// A step whose claim finds its ontology deleted makes no call: it ends
	// cancelled and publishes nothing.
	t.Run("a deleted ontology's step ends cancelled", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(3)...))
		f.db.mu.Lock()
		f.db.gone["ont"] = true
		f.db.mu.Unlock()
		f.work("w1")
		if row := f.ended(job.LoadID, model.FoundryEnrichmentCancelled, ""); row.ProfilesKey != nil || len(f.dev.sent()) != 0 {
			t.Fatalf("published %v after %d calls", row.ProfilesKey, len(f.dev.sent()))
		}
	})

	// A failure while the load waits (Autumn, S3 or MySQL failing) leaves
	// the lease to lapse: the step is tried again a lease later, not at the
	// next poll, where a failure that lasts would have the worker read the
	// version and the plan again, and ask Autumn, every few seconds.
	t.Run("a failure is tried again once the lease lapses, not at once", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(3)...))
		f.pro.err = errors.New("autumn is down")
		f.work("w1")
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentRunning || !row.LeaseUntil.After(f.clock.now()) {
			t.Fatalf("after the failure: %s, leased until %s", row.Status, row.LeaseUntil)
		}
		if f.work("w2") {
			t.Fatal("the failed step was claimed again at once")
		}
		f.pro.err = nil
		f.clock.advance(f.cfg.Lease + time.Second)
		if !f.work("w2") {
			t.Fatal("the failed step was not claimed again once its lease lapsed")
		}
		if row := f.ended(job.LoadID, model.FoundryEnrichmentDone, ""); row.Counts.Found != 3 {
			t.Fatalf("counts = %+v", row.Counts)
		}
	})

	// claim_wait is how long a step waited for its first claim: once per
	// step. A reclaim before any batch began (Autumn failing, a stopping
	// worker's hand-on) keeps claimed_at, and records no other wait.
	t.Run("records the claim wait once, at the step's first claim", func(t *testing.T) {
		f := newStepFixture(t)
		waited := samples(t, "helix_foundry_enrichment_claim_wait_seconds")
		job := f.queued(people(crowd(3)...))
		f.pro.err = errors.New("autumn is down")
		f.work("w1")
		if got := samples(t, "helix_foundry_enrichment_claim_wait_seconds"); got != waited+1 {
			t.Fatalf("claim waits observed %d after the first claim, want %d", got, waited+1)
		}
		for range 2 {
			f.clock.advance(f.cfg.Lease + time.Second) // the failed claim's lease lapses
			if !f.work("w2") {
				t.Fatal("the failed step was not claimed again once its lease lapsed")
			}
		}
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentRunning || row.BatchCursor != 0 || row.InflightCredits != 0 {
			t.Fatalf("after the reclaims: %s, cursor %d, %d in flight", row.Status, row.BatchCursor, row.InflightCredits)
		}
		f.pro.err = nil
		f.clock.advance(f.cfg.Lease + time.Second)
		f.work("w3")
		f.ended(job.LoadID, model.FoundryEnrichmentDone, "")
		if got := samples(t, "helix_foundry_enrichment_claim_wait_seconds"); got != waited+1 {
			t.Fatalf("claim waits observed %d after three reclaims, want %d", got, waited+1)
		}
	})

	// Past its deadline and the grace the load applies the carried
	// profiles; the step's late publish keeps what it paid for in the next
	// load's carry-forward. A failure then (S3 throttling, a database blip)
	// is tried again once the lease lapses, a lease apart, instead of
	// throwing the step's results away.
	t.Run("a failure past the deadline and the grace is tried again, and the late publish lands", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(3)...))
		var answered atomic.Int32
		f.dev.setAnswer(func(string, string) (int, string, bool) {
			if answered.Add(1) == 3 { // every call is sent: the batch runs whole, then the clock is past the grace
				f.clock.advance(time.Hour + 2*time.Minute)
			}
			return 0, "", false
		})
		f.store.failPut = func(key string) bool { return strings.HasSuffix(key, "/profiles.jsonl.gz") }
		var released atomic.Int32
		f.db.released = func() { released.Add(1) }
		f.work("w1")
		row := f.row(job.LoadID)
		if row.Status != model.FoundryEnrichmentRunning || row.ProfilesKey != nil || row.BatchCursor != 1 || released.Load() != 0 {
			t.Fatalf("after the failed late publish: %s at %d, published %v, released %d times", row.Status, row.BatchCursor, row.ProfilesKey,
				released.Load())
		}
		f.work("w2")
		row = f.ended(job.LoadID, model.FoundryEnrichmentDone, "")
		if row.ProfilesKey == nil || row.Counts.Found != 3 || row.Counts.Profiles != 3 || len(f.dev.sent()) != 3 {
			t.Fatalf("published %v: %+v after %d calls", row.ProfilesKey, row.Counts, len(f.dev.sent()))
		}
		next := f.newLoad()
		f.step(next, people(crowd(3)...))
		if counts := f.row(next.LoadID).Counts; counts.Carried != 3 || counts.ToLookUp != 0 {
			t.Fatalf("the next load carried %d and looks up %d", counts.Carried, counts.ToLookUp)
		}
	})

	// A step past its deadline makes no call, so Pro is moot: Autumn failing
	// doesn't hold its late publish up.
	t.Run("Autumn failing past the deadline doesn't hold the late publish up", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(3)...))
		w := f.worker("w1")
		leased := f.clock.now()
		step, _ := w.Claim(context.Background())
		f.clock.advance(time.Hour + 2*time.Minute)
		f.pro.err = errors.New("autumn is down")
		if err := w.Run(context.Background(), step, leased); err != nil {
			t.Fatal(err)
		}
		if row := f.ended(job.LoadID, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopTime); row.ProfilesKey == nil {
			t.Fatalf("published %v", row.ProfilesKey)
		}
	})

	// A step past its deadline makes no call, so it frees what it reserved
	// at its first claim: its late publish may be tried again for up to
	// lateFor, and a reservation held all that while would shrink, or stop,
	// every other step's lookups under the caps.
	t.Run("a step past its deadline frees its reservation while it publishes late", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(3)...))
		stopping, sigterm := context.WithCancel(context.Background())
		sigterm() // handed on before its first batch, holding its reservation
		w := f.worker("w1")
		step, _ := w.Claim(context.Background())
		if err := w.Run(stopping, step, f.clock.now()); err != nil {
			t.Fatal(err)
		}
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentRunning || row.ReservedCredits != 3*20 {
			t.Fatalf("handed on: %s, %d reserved", row.Status, row.ReservedCredits)
		}
		f.clock.advance(time.Hour + 2*time.Minute)
		var failed atomic.Bool
		f.store.failPut = func(key string) bool { return strings.HasSuffix(key, "/profiles.jsonl.gz") && !failed.Swap(true) }
		f.work("w2")
		if row := f.row(job.LoadID); row.Status != model.FoundryEnrichmentRunning || row.ReservedCredits != 0 {
			t.Fatalf("after the failed late publish: %s, %d reserved", row.Status, row.ReservedCredits)
		}
		f.clock.advance(f.cfg.Lease + time.Second) // a late step is tried again once its lease lapses
		f.work("w3")
		if row := f.ended(job.LoadID, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopTime); row.ProfilesKey == nil || len(f.dev.sent()) != 0 {
			t.Fatalf("published %v after %d calls", row.ProfilesKey, len(f.dev.sent()))
		}
	})

	// A step still failing once the late window has passed too ends failed:
	// what it wrote of its publication is deleted, and it wakes its load.
	t.Run("a step failing past the late window ends failed", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(3)...))
		f.store.failPut = func(key string) bool { return strings.HasSuffix(key, "/profiles.jsonl.gz") }
		w := f.worker("w1")
		leased := f.clock.now()
		step, _ := w.Claim(context.Background())
		f.clock.advance(time.Hour + 2*time.Minute + lateFor)
		failed := testutil.ToFloat64(steps.WithLabelValues("failed", ""))
		if err := w.Run(context.Background(), step, leased); err == nil {
			t.Fatal("the failure went unreported")
		}
		row := f.ended(job.LoadID, model.FoundryEnrichmentFailed, "")
		if got := testutil.ToFloat64(steps.WithLabelValues("failed", "")); got != failed+1 {
			t.Fatalf("failed ends counted %v, want %v", got, failed+1)
		}
		layout := LoadLayout("ws", "ont", job.LoadID)
		if row.ProfilesKey != nil || row.ResultsKey != nil || f.store.has(layout.Results()) || f.store.has(layout.Profiles()) {
			t.Fatalf("a failed step left its publication: %v %v", row.ProfilesKey, row.ResultsKey)
		}
		if f.db.woken(job.ID) != 1 {
			t.Fatal("the failed step didn't wake its load")
		}
	})

	// A step whose charge keeps failing past the late window settles its
	// batch from what it wrote before ending failed: the ledger is exact and
	// no batch stays in flight.
	t.Run("a step failing past the late window settles its batch first", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(3)...))
		var answered atomic.Int32
		f.dev.setAnswer(func(string, string) (int, string, bool) {
			if answered.Add(1) == 3 {
				f.clock.advance(time.Hour + 2*time.Minute + lateFor)
			}
			return 0, "", false
		})
		f.db.failCharge = true
		f.work("w1")
		row := f.ended(job.LoadID, model.FoundryEnrichmentFailed, "")
		if credits, uncertain := f.db.charged(); credits != 60 || uncertain != 0 || row.InflightCredits != 0 || row.Counts.Credits != 60 {
			t.Fatalf("charged %d (+%d uncertain) with %d in flight", credits, uncertain, row.InflightCredits)
		}
	})

	// Helix's operators turned the flag off after the step was queued: it
	// makes no call and publishes the carried profiles.
	t.Run("the flag off ends partial(flag)", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(3)...))
		f.cfg.Enabled = false
		f.work("w1")
		if row := f.ended(job.LoadID, model.FoundryEnrichmentPartial, model.FoundryEnrichmentStopFlag); row.ProfilesKey == nil || len(f.dev.sent()) != 0 {
			t.Fatalf("published %v after %d calls", row.ProfilesKey, len(f.dev.sent()))
		}
	})

	// Claims honour the running limits: one step per workspace at once.
	t.Run("claims one step per workspace", func(t *testing.T) {
		f := newStepFixture(t)
		f.queued(people(crowd(1)...))
		f.queued(people(crowd(1)...))
		w := f.worker("w1")
		first, _ := w.Claim(context.Background())
		second, _ := w.Claim(context.Background())
		if first == nil || second != nil {
			t.Fatalf("claimed %v then %v", first, second)
		}
	})
}

func TestLease(t *testing.T) {
	// Renewals carry a user's Stop and a cancel to the running step, and
	// move the fence on; a lease another worker took cancels the claim.
	t.Run("renews, hears signals and cancels on loss", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(1)...))
		f.cfg.Lease = 30 * time.Millisecond
		w := f.worker("w1")
		step, _ := w.Claim(context.Background())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		held := w.hold(ctx, cancel, step.LoadID, *step.LeaseOwner, f.clock.now())
		defer held.release()
		claimed := held.fence()
		f.db.stop(job.LoadID)
		f.clock.advance(time.Second)
		deadline := time.Now().Add(5 * time.Second)
		for !held.signals().Stop && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if !held.signals().Stop || !held.fence().After(claimed) {
			t.Fatalf("signals %+v, fence %s", held.signals(), held.fence())
		}
		f.db.mu.Lock()
		f.db.steps[job.LoadID].LeaseOwner = ptr("w2")
		f.db.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("a lost lease didn't cancel the claim")
		}
	})

	// A renewal that fails for any reason but the lease being taken (MySQL
	// out of reach, say) confirms nothing: the fence stays where the last
	// renewal that landed put it, so calls stop a call's timeout before the
	// lease could lapse, while the claim goes on. A fence moved on a failure
	// would let a worker cut off from MySQL go on calling while another
	// claims the lapsed step and sends the same batch again.
	t.Run("a failed renewal leaves the fence and the signals where they were", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(1)...))
		f.cfg.Lease = 30 * time.Millisecond
		w := f.worker("w1")
		step, _ := w.Claim(context.Background())
		var renewals atomic.Int32
		f.db.renewing = func() { renewals.Add(1) }
		f.db.failRenew = true
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		held := w.hold(ctx, cancel, step.LoadID, *step.LeaseOwner, f.clock.now())
		defer held.release()
		claimed := held.fence()
		f.db.stop(job.LoadID)
		f.clock.advance(time.Second)
		for deadline := time.Now().Add(5 * time.Second); renewals.Load() < 3 && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		if n := renewals.Load(); n < 3 || !held.fence().Equal(claimed) || held.signals().Stop || ctx.Err() != nil {
			t.Fatalf("after %d failed renewals: fence %s (claimed %s), signals %+v, claim ended: %v", n, held.fence(), claimed, held.signals(),
				ctx.Err())
		}
		f.db.mu.Lock()
		f.db.failRenew = false
		f.db.mu.Unlock()
		for deadline := time.Now().Add(5 * time.Second); !held.signals().Stop && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}
		if !held.signals().Stop || !held.fence().After(claimed) {
			t.Fatalf("once a renewal landed: fence %s (claimed %s), signals %+v", held.fence(), claimed, held.signals())
		}
	})

	// Handing a step on stops renewing first: a renewal in flight that
	// landed after the release would put the lease back for its whole
	// length, and the next claim would wait it out.
	t.Run("hands on only once no renewal is in flight", func(t *testing.T) {
		f := newStepFixture(t)
		job := f.queued(people(crowd(1)...))
		f.cfg.Lease = 30 * time.Millisecond
		w := f.worker("w1")
		step, _ := w.Claim(context.Background())
		renewing, released := make(chan struct{}), make(chan struct{})
		var once sync.Once
		f.db.renewing = func() {
			once.Do(func() { close(renewing) })
			select { // a slow renewal, landing after any release made meanwhile
			case <-released:
			case <-time.After(200 * time.Millisecond):
			}
		}
		f.db.released = func() { close(released) }
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		held := w.hold(ctx, cancel, step.LoadID, *step.LeaseOwner, f.clock.now())
		<-renewing
		if err := held.handOn(ctx, f.db, step.LoadID, *step.LeaseOwner); err != nil {
			t.Fatal(err)
		}
		if row := f.row(job.LoadID); !row.LeaseUntil.Before(f.clock.now()) {
			t.Fatalf("lease until %s after the hand-on at %s", row.LeaseUntil, f.clock.now())
		}
	})
}

func TestPlanCalls(t *testing.T) {
	// A step holds one batch of its plan at a time, whatever the plan's
	// size: batches come from a local copy in the plan's order of calls,
	// skips and carries left out, and a batch asked for again (a reclaim,
	// an abandon) is the same batch. The copy, people's clues, has no name
	// on disk: it goes with the process however that ends.
	t.Run("reads the plan's calls a batch at a time", func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		files := &Files{Store: newMemStore(), Dir: dir}
		var plan []Entry
		for i := range 2*BatchSize + 7 {
			plan = append(plan, Entry{Clue: Clue{Entity: fmt.Sprintf("c_%03d", i)}, Action: ActionCall},
				Entry{Clue: Clue{Entity: fmt.Sprintf("s_%03d", i)}, Action: ActionSkip})
		}
		if _, err := Put(ctx, files, "plan", plan); err != nil {
			t.Fatal(err)
		}
		p, err := files.planCalls(ctx, "plan")
		if err != nil {
			t.Fatal(err)
		}
		if left, _ := os.ReadDir(dir); len(left) != 0 { // a worker that dies leaves no copy
			t.Fatalf("the plan's copy is in %s: %v", dir, left)
		}
		if p.total != 2*BatchSize+7 || p.batchLen(2) != 7 || p.batchLen(3) != 0 {
			t.Fatalf("total %d, batch 2 of %d", p.total, p.batchLen(2))
		}
		entities := func(b int) []string {
			t.Helper()
			batch, err := p.batch(b)
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, e := range batch {
				out = append(out, e.Entity)
			}
			return out
		}
		if got := entities(1); len(got) != BatchSize || got[0] != "c_050" || got[BatchSize-1] != "c_099" {
			t.Fatalf("batch 1 = %v", got)
		}
		if got := entities(1); len(got) != BatchSize || got[0] != "c_050" {
			t.Fatalf("batch 1 again = %v", got)
		}
		if got := entities(0); len(got) != BatchSize || got[0] != "c_000" {
			t.Fatalf("batch 0 after batch 1 = %v", got)
		}
		if got := entities(2); !slices.Equal(got, []string{"c_100", "c_101", "c_102", "c_103", "c_104", "c_105", "c_106"}) {
			t.Fatalf("batch 2 = %v", got)
		}
		if got := entities(3); len(got) != 0 {
			t.Fatalf("batch 3 = %v", got)
		}
		p.close()
		if left, _ := os.ReadDir(dir); len(left) != 0 {
			t.Fatalf("the plan's copy is left: %v", left)
		}
	})
}
