package foundrygraphworker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// applyJob brings load-0, the current load, up to its tables in place.
func applyJob() *model.FoundryGraphJob {
	job := loadJob()
	job.ID, job.LoadID, job.Kind, job.Origin = "job-4", "load-0", model.FoundryGraphJobApply, model.FoundryLoadByUser
	return job
}

// ingestApplied ends an Ingest done, the diff having changed one row and added another.
func ingestApplied(req string, stream agentv1.Agent_IngestServer) error {
	return ingestEvents(stream, []string{
		`{"event":"ingest_started","contract_version":"4.2.0"}`,
		`{"event":"chunk","report":{"mapping":"customers","pass":"create","rows":2,"outcome":"loaded"},"diff":{"added":1,"changed":1,"removed":0,"unchanged":0}}`,
		`{"event":"ingest_finished"}`,
	}, `{"status":"done","report":{"mappings":[]},"applied":[{"stream":"customers","snapshot":"s1","change_seq":7,"added":1,"changed":1,"removed":0}]}`)
}

func TestApply(t *testing.T) {
	// An apply sends every table the version reads, at its newest snapshot,
	// and asks the agent to write only what changed into the current load.
	t.Run("diffs every table into the current load", func(t *testing.T) {
		h := newHarness(t)
		h.store.pending = nil
		h.agent.ingest = ingestApplied
		doneBefore := testutil.ToFloat64(applies.WithLabelValues("done"))
		addedBefore := testutil.ToFloat64(applyRows.WithLabelValues("added"))
		outcome := h.run(t, applyJob())
		if outcome.Status != model.FoundryGraphJobSucceeded || !outcome.IngestDone || outcome.Error != "" {
			t.Fatalf("outcome = %+v", outcome)
		}
		ingests, extracts := h.agent.calls()
		if len(ingests) != 1 || len(extracts) != 0 {
			t.Fatalf("calls = %d ingests, %d extracts: an apply with no documents waiting extracts nothing", len(ingests), len(extracts))
		}
		// The sources are read as sent: a Source is written, never read.
		var req struct {
			ontologyagent.IngestRequest
			Sources []json.RawMessage `json:"sources"`
		}
		if err := json.Unmarshal([]byte(ingests[0]), &req); err != nil {
			t.Fatal(err)
		}
		if req.Reload != ontologyagent.ReloadDiff || req.Load != "load-0" || req.RequestID != "job-4" || len(req.Sources) != 1 {
			t.Fatalf("ingest request = %s", ingests[0])
		}
		if pins := h.store.pins["job-4"]; len(pins) != 1 || pins[0] != firstPin {
			t.Fatalf("pins = %+v: the finish records them as the load's position", pins)
		}
		if got := testutil.ToFloat64(applies.WithLabelValues("done")) - doneBefore; got != 1 {
			t.Fatalf("applies done = %v", got)
		}
		if got := testutil.ToFloat64(applyRows.WithLabelValues("added")) - addedBefore; got != 1 {
			t.Fatalf("rows added = %v", got)
		}
	})

	// An apply's report counts what it changed; the load's count of open
	// violations describes the load, so an apply leaves it alone.
	t.Run("leaves the load's violation count alone", func(t *testing.T) {
		h := newHarness(t)
		h.store.pending = nil
		h.agent.ingest = ingestApplied
		h.run(t, applyJob())
		for _, u := range h.store.updates {
			if u.IngestViolations != nil {
				t.Fatalf("update %+v sets the load's violations", u)
			}
		}
	})

	// What the agent can't apply in place is rebuilt: the apply ends failed
	// before its tables are in, which the finish turns into a full refresh.
	t.Run("hands what the agent can't diff to a full load", func(t *testing.T) {
		h := newHarness(t)
		h.agent.ingest = func(_ string, stream agentv1.Agent_IngestServer) error {
			return ingestEvents(stream, nil, `{"status":"diff_unsupported","reason":"load_predates_diff","streams":["customers"]}`)
		}
		before := testutil.ToFloat64(applies.WithLabelValues("unsupported"))
		outcome := h.run(t, applyJob())
		if outcome.Status != model.FoundryGraphJobFailed || outcome.IngestDone || outcome.RetryAt != nil || outcome.Next.RefreshID == "" ||
			!strings.Contains(outcome.Error, "load_predates_diff") {
			t.Fatalf("outcome = %+v", outcome)
		}
		if _, extracts := h.agent.calls(); len(extracts) != 0 {
			t.Fatal("an apply that wrote no tables extracted documents")
		}
		if got := testutil.ToFloat64(applies.WithLabelValues("unsupported")) - before; got != 1 {
			t.Fatalf("applies unsupported = %v", got)
		}
	})

	// In place, an apply would bind the profiles the load was built with;
	// until People enrichment is recomputed per apply, its versions rebuild.
	t.Run("never applies a version that enriches people", func(t *testing.T) {
		h := newHarness(t)
		enrichingVersion(t, h)
		outcome := h.run(t, applyJob())
		if outcome.Status != model.FoundryGraphJobFailed || outcome.IngestDone {
			t.Fatalf("outcome = %+v", outcome)
		}
		if ingests, _ := h.agent.calls(); len(ingests) != 0 || len(h.agent.cluesCalls) != 0 {
			t.Fatalf("the agent was called: %d ingests, %d clues", len(ingests), len(h.agent.cluesCalls))
		}
	})
}
