package foundrygraphworker

import (
	"slices"
	"strconv"
	"testing"
	"time"

	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// filling makes load-0's fill apply, owing each of docs a fill of mentors
// (its finding's link, beside another link its move added), as the
// harness's documents; one not among them is owed as gone.
func filling(h *harness, docs ...string) {
	byID := map[string]*model.FoundryDocument{}
	for _, d := range h.store.pending {
		byID[d.ID] = d
	}
	h.store.pending = nil
	h.store.fill = &model.FoundryHealFill{ID: "fill-1", OntologyID: "ont-1", LoadID: "load-0", State: model.FoundryFillApplying,
		LinksAdded: []string{"advises", "mentors"}, CapUSD: model.FoundryFillCapUSD, ToVersionID: "ver-3", ToVersion: 3,
		Link: "mentors", Spellings: []string{"mentors", "mentor_of"}}
	for _, id := range docs {
		r := &model.FoundryLoadResend{LoadID: "load-0", DocumentID: id, Reason: model.FoundryResendLink, Requests: 1, Document: byID[id]}
		if r.Document == nil {
			r.Unsendable = model.FoundryReleaseDocumentGone
		}
		h.store.links = append(h.store.links, r)
	}
}

// filledEvent is the engine's word on a document's fill.
func filledEvent(doc, outcome string, facts, held, taken int) string {
	return `{"event":"filled","doc_id":"` + doc + `","links":["mentors"],"outcome":` + outcome + `,"facts":` + strconv.Itoa(facts) +
		`,"held":` + strconv.Itoa(held) + `,"reviews_taken":` + strconv.Itoa(taken) + `}`
}

// held is a held document's event of a fill: nothing committed.
func held(doc string) string {
	return documentEvent(doc, `{"kind":"already_ingested"}`, usage)
}

func TestFillSendsEachOwedDocumentWithTheLinkAlone(t *testing.T) {
	// Each owed document goes with its saved work and the links to fill,
	// under the fill's budget; each word ends its request and counts once;
	// the last ends the fill.
	t.Run("fills each document and ends the fill", func(t *testing.T) {
		h := newHarness(t)
		filling(h, "doc-a", "doc-b")
		h.agent.extract = func(raw string, stream agentv1.Agent_ExtractServer) error {
			return extractEvents(stream, []string{
				filledEvent("doc-a", `{"kind":"written"}`, 2, 1, 1), held("doc-a"),
				filledEvent("doc-b", `{"kind":"nothing"}`, 0, 0, 0), held("doc-b"),
				`{"event":"extract_finished"}`}, `{"status":"done","totals":`+totals+`}`)
		}
		outcome := h.run(t, resendJob())
		if outcome.Status != model.FoundryGraphJobSucceeded || outcome.Error != "" {
			t.Fatalf("%+v", outcome)
		}
		_, calls := h.agent.calls()
		if len(calls) != 1 {
			t.Fatalf("%d extracts, want 1", len(calls))
		}
		req := decodeExtract(t, calls[0])
		if len(req.Documents) != 2 {
			t.Fatalf("sent %d documents", len(req.Documents))
		}
		// The finding's link alone, of the two the move added, with its
		// spellings.
		for _, d := range req.Documents {
			if !slices.Equal(d.Fill, []string{"mentors"}) || !slices.Equal(d.FillSpellings, []string{"mentors", "mentor_of"}) ||
				d.Artifacts == nil {
				t.Fatalf("document %s sent fill %v spellings %v artifacts %v", d.DocID, d.Fill, d.FillSpellings, d.Artifacts)
			}
		}
		if req.Limits.MaxUSD <= 0 || req.Limits.MaxUSD > model.FoundryFillCapUSD {
			t.Fatalf("max_usd %v, want the fill's cap at most", req.Limits.MaxUSD)
		}
		if len(h.store.fillEnded) != 2 || h.store.fillEnded[0].Outcome != "written" || h.store.fillEnded[0].Facts != 2 ||
			h.store.fillEnded[0].ReviewsTaken != 1 || h.store.fillEnded[0].USDMicros != 1500 || h.store.fillEnded[1].Outcome != "nothing" {
			t.Fatalf("recorded %+v", h.store.fillEnded)
		}
		if h.store.fill.State != model.FoundryFillFilled {
			t.Fatalf("fill %s", h.store.fill.State)
		}
		if len(h.store.extractions) != 0 {
			t.Fatalf("a fill's documents are recorded as the fill's alone: %v", h.store.extractions)
		}
	})
	// A document the load no longer holds ends refused without a call; one
	// the engine said nothing of the fill of ends refused unanswered; one it
	// failed, refused document_failed.
	t.Run("ends what can't be filled, refused", func(t *testing.T) {
		h := newHarness(t)
		filling(h, "doc-a", "doc-b", "doc-gone", "doc-c")
		h.agent.extract = func(raw string, stream agentv1.Agent_ExtractServer) error {
			req := decodeExtract(t, raw)
			if len(req.Documents) != 3 {
				t.Errorf("sent %d documents: the gone one goes nowhere", len(req.Documents))
			}
			return extractEvents(stream, []string{
				held("doc-a"),
				documentEvent("doc-b", `{"kind":"failed","code":"store","retryable":false}`, noUsage),
				filledEvent("doc-c", `{"kind":"refused","reason":"not_replayed"}`, 0, 0, 0), held("doc-c"),
				`{"event":"extract_finished"}`}, `{"status":"done","totals":`+totals+`}`)
		}
		outcome := h.run(t, resendJob())
		if outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		got := map[string]string{}
		for _, d := range h.store.fillEnded {
			got[d.DocumentID] = d.Outcome + "/" + d.Reason
		}
		want := map[string]string{"doc-gone": "refused/document_gone", "doc-a": "refused/unanswered", "doc-b": "refused/document_failed",
			"doc-c": "refused/not_replayed"}
		if len(got) != len(want) {
			t.Fatalf("recorded %v", got)
		}
		for doc, w := range want {
			if got[doc] != w {
				t.Fatalf("%s recorded %q, want %q (all %v)", doc, got[doc], w, got)
			}
		}
	})
	// A fill past its cap stops, its documents no longer owed; past the
	// ontology's day it pauses a day, still owed. Neither asks the engine.
	for name, c := range map[string]struct {
		spent    float64
		daySpent int64
		stopped  string
		paused   bool
	}{
		"cap reached":   {spent: model.FoundryFillCapUSD, stopped: model.FoundryFillCapReached},
		"a day's spend": {daySpent: int64(model.FoundryFillDailyCapUSD * 1e6), paused: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			filling(h, "doc-a")
			h.store.fill.SpentUSD, h.store.fillSpend = c.spent, c.daySpent
			outcome := h.run(t, resendJob())
			if outcome.Status != model.FoundryGraphJobSucceeded {
				t.Fatalf("%+v", outcome)
			}
			if _, calls := h.agent.calls(); len(calls) != 0 {
				t.Fatalf("%d extracts, want none", len(calls))
			}
			if h.store.fillStop != c.stopped {
				t.Fatalf("stopped %q, want %q", h.store.fillStop, c.stopped)
			}
			if (h.store.fillPause != nil) != c.paused || c.paused && h.store.fillPause.Before(time.Now().Add(23*time.Hour)) {
				t.Fatalf("paused %v", h.store.fillPause)
			}
		})
	}
	// A run that spends its budget: bound by the fill's own cap, the fill
	// stops, what it didn't reach no longer owed; bound by the day's, it
	// pauses, still owed. No job follows one that could afford nothing.
	for name, c := range map[string]struct {
		daySpent int64
		stopped  string
		owed     int
	}{
		"bound by the fill's cap": {stopped: model.FoundryFillCapReached},
		"bound by the day's":      {daySpent: int64((model.FoundryFillDailyCapUSD - 1) * 1e6), owed: 1},
	} {
		t.Run("a budget spent mid-run, "+name, func(t *testing.T) {
			h := newHarness(t)
			filling(h, "doc-a", "doc-b")
			h.store.fillSpend = c.daySpent
			h.agent.extract = func(raw string, stream agentv1.Agent_ExtractServer) error {
				return extractEvents(stream, []string{filledEvent("doc-a", `{"kind":"written"}`, 1, 0, 1), held("doc-a"), `{"event":"extract_finished"}`},
					`{"status":"budget_exhausted","totals":`+totals+`}`)
			}
			outcome := h.run(t, resendJob())
			if outcome.Status != model.FoundryGraphJobSucceeded {
				t.Fatalf("%+v", outcome)
			}
			if len(h.store.fillEnded) != 1 || len(h.store.links) != c.owed || h.store.fillStop != c.stopped {
				t.Fatalf("ended %+v, owed %+v, stopped %q", h.store.fillEnded, h.store.links, h.store.fillStop)
			}
			if (h.store.fillPause != nil) != (c.owed == 1) {
				t.Fatalf("paused %v", h.store.fillPause)
			}
		})
	}
	// The workspace's own provider refusing its key fails the job, the
	// document still owed, for the fill to back off.
	t.Run("a model that refuses the workspace's key", func(t *testing.T) {
		h := newHarness(t)
		filling(h, "doc-a")
		h.agent.extract = func(raw string, stream agentv1.Agent_ExtractServer) error {
			return extractEvents(stream, []string{
				documentEvent("doc-a", `{"kind":"failed","code":"model","model_failure":"auth","retryable":true}`, noUsage),
				`{"event":"extract_finished"}`}, `{"status":"done","totals":`+totals+`}`)
		}
		outcome := h.run(t, resendJob())
		if outcome.Status == model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		if len(h.store.fillEnded) != 0 || len(h.store.links) != 1 {
			t.Fatalf("ended %+v, owed %+v", h.store.fillEnded, h.store.links)
		}
	})
	// A model that fails a document after spending: what it spent is the
	// fill's and the job's, though the document stays owed.
	t.Run("a model failure's spend is recorded", func(t *testing.T) {
		h := newHarness(t)
		filling(h, "doc-a")
		h.agent.extract = func(raw string, stream agentv1.Agent_ExtractServer) error {
			return extractEvents(stream, []string{
				documentEvent("doc-a", `{"kind":"failed","code":"model","model_failure":"overloaded","retryable":true}`, usage),
				`{"event":"extract_finished"}`}, `{"status":"done","totals":`+totals+`}`)
		}
		outcome := h.run(t, resendJob())
		if outcome.Status == model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		if len(h.store.fillSpent) != 1 || h.store.fillSpent[0].USDMicros != 1500 || h.store.fill.SpentUSD == 0 {
			t.Fatalf("spent %+v, fill %v", h.store.fillSpent, h.store.fill.SpentUSD)
		}
		if len(h.store.fillEnded) != 0 || len(h.store.links) != 1 {
			t.Fatalf("ended %+v, owed %+v", h.store.fillEnded, h.store.links)
		}
	})
	// A finding that no longer names the link stops the fill, asking nothing.
	t.Run("a finding gone", func(t *testing.T) {
		h := newHarness(t)
		filling(h, "doc-a")
		h.store.fill.Link, h.store.fill.Spellings = "", nil
		outcome := h.run(t, resendJob())
		if outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		if _, calls := h.agent.calls(); len(calls) != 0 || h.store.fillStop != model.FoundryFillFindingGone {
			t.Fatalf("%d extracts, stopped %q", len(calls), h.store.fillStop)
		}
	})
	// Off, the binary is the rollback floor: it sends no fill.
	t.Run("add in place off", func(t *testing.T) {
		h := newHarness(t)
		filling(h, "doc-a")
		h.task.runner.cfg.Foundry.Heal.AddInPlace = false
		outcome := h.run(t, resendJob())
		if outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		if _, calls := h.agent.calls(); len(calls) != 0 || len(h.store.links) != 1 {
			t.Fatalf("%d extracts, owed %v", len(calls), h.store.links)
		}
	})
	// No fill applies: nothing is sent for one.
	t.Run("no fill", func(t *testing.T) {
		h := newHarness(t)
		filling(h, "doc-a")
		h.store.fill.State = model.FoundryFillMoved
		outcome := h.run(t, resendJob())
		if outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		if _, calls := h.agent.calls(); len(calls) != 0 {
			t.Fatalf("%d extracts", len(calls))
		}
	})
}
