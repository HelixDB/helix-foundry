package foundrygraphworker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

func (f *fakeAgent) Compare(_ context.Context, req *agentv1.CompareRequest) (*agentv1.CompareResponse, error) {
	f.mu.Lock()
	f.compares = append(f.compares, req.GetRequestJson())
	answer := f.compare
	f.mu.Unlock()
	if err := f.refused("compare_request.json", req.GetRequestJson()); err != nil {
		return nil, err
	}
	if answer == nil {
		return nil, status.Error(codes.Unimplemented, "no compare")
	}
	return &agentv1.CompareResponse{ResultJson: answer(req.GetRequestJson())}, nil
}

// rebuildJob is a refresh load, job-1, of the current load load-0.
func rebuildJob(h *harness, appliedAt *time.Time) *model.FoundryGraphJob {
	refreshes := "load-0"
	h.store.loads = map[string]*model.FoundryLoad{
		"job-1":  {ID: "job-1", OntologyID: "ont-1", Origin: model.FoundryLoadByRefresh, RefreshesLoadID: &refreshes},
		"load-0": {ID: "load-0", OntologyID: "ont-1", AppliedAt: appliedAt},
	}
	job := loadJob()
	job.Origin, job.RefreshesLoadID = model.FoundryLoadByRefresh, &refreshes
	return job
}

// compared is a Compare result as a 4.9.0 agent writes it: a Plane entity
// label and a held claims label, equal, then these kinds as given.
func compared(t *testing.T, kinds ...string) string {
	t.Helper()
	result := `{"equal":` + fmt.Sprint(len(kinds) == 0) + `,"kinds":[` +
		`{"kind":"entity","label":"Plane","a_count":2,"b_count":2,"equal":true,"a_hash":"h","b_hash":"h","differing":[]},` +
		`{"kind":"held","label":"held","a_count":1,"b_count":1,"equal":true,"a_hash":"k","b_hash":"k","differing":[]}`
	for _, k := range kinds {
		result += "," + k
	}
	result += "]}"
	if err := contracttest.Validate(t, contracttest.Schema(t, "compare_result.json"), []byte(result)); err != nil {
		t.Fatalf("not the contract's: %v\n%s", err, result)
	}
	return result
}

const (
	factDiffers = `{"kind":"fact","label":"operated_by","a_count":3,"b_count":2,"equal":false,"a_hash":"x","b_hash":"y","differing":["F1"]}`
	heldDiffers = `{"kind":"held","label":"held","a_count":1,"b_count":2,"equal":false,"a_hash":"k","b_hash":"m","differing":["C2"]}`
)

func TestRebuildDrift(t *testing.T) {
	// A rebuild of a load applies kept up to date is what that load should
	// hold; each label that differs is drift, counted and named in the log.
	t.Run("compares a rebuild with the applied load it replaces", func(t *testing.T) {
		h := newHarness(t)
		applied := time.Now()
		job := rebuildJob(h, &applied)
		answer := compared(t, factDiffers)
		h.agent.compare = func(string) string { return answer }
		drifted, facts := testutil.ToFloat64(rebuilds.WithLabelValues("drifted")), testutil.ToFloat64(applyDrift.WithLabelValues("fact"))
		outcome := h.run(t, job)
		if outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		if len(h.agent.compares) != 1 || !strings.Contains(h.agent.compares[0], `"load_a":"load-0"`) ||
			!strings.Contains(h.agent.compares[0], `"load_b":"job-1"`) {
			t.Fatalf("compares = %v", h.agent.compares)
		}
		if got := testutil.ToFloat64(rebuilds.WithLabelValues("drifted")) - drifted; got != 1 {
			t.Fatalf("drifted rebuilds = %v", got)
		}
		if got := testutil.ToFloat64(applyDrift.WithLabelValues("fact")) - facts; got != 1 {
			t.Fatalf("drifted facts = %v", got)
		}
	})

	// Held claims write nothing to the graph, and a load stored before 4.9.0
	// keys them otherwise: a rebuild that differs only there is held, not
	// drifted, and one that differs in the graph too is drifted.
	for name, c := range map[string]struct {
		kinds   []string
		outcome string
	}{
		"only held claims":          {[]string{heldDiffers}, "held"},
		"held claims, then facts":   {[]string{heldDiffers, factDiffers}, "drifted"},
		"facts, then held claims":   {[]string{factDiffers, heldDiffers}, "drifted"},
		"nothing (equal, by kinds)": {nil, "equal"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			applied := time.Now()
			job := rebuildJob(h, &applied)
			answer := compared(t, c.kinds...)
			h.agent.compare = func(string) string { return answer }
			before := map[string]float64{}
			for _, o := range []string{"equal", "drifted", "held", "failed"} {
				before[o] = testutil.ToFloat64(rebuilds.WithLabelValues(o))
			}
			held := testutil.ToFloat64(applyDrift.WithLabelValues("held"))
			if outcome := h.run(t, job); outcome.Status != model.FoundryGraphJobSucceeded {
				t.Fatalf("outcome = %+v", outcome)
			}
			for o, was := range before {
				want := 0.0
				if o == c.outcome {
					want = 1
				}
				if got := testutil.ToFloat64(rebuilds.WithLabelValues(o)) - was; got != want {
					t.Fatalf("%s rebuilds = %v, want %v", o, got, want)
				}
			}
			wantHeld := 0.0
			if len(c.kinds) > 0 {
				wantHeld = 1
			}
			if got := testutil.ToFloat64(applyDrift.WithLabelValues("held")) - held; got != wantHeld {
				t.Fatalf("differing held labels = %v, want %v", got, wantHeld)
			}
		})
	}

	// A load no apply touched is a fresh load already: nothing to compare.
	t.Run("leaves a rebuild of an unapplied load alone", func(t *testing.T) {
		h := newHarness(t)
		h.run(t, rebuildJob(h, nil))
		if len(h.agent.compares) != 0 {
			t.Fatalf("compares = %v", h.agent.compares)
		}
	})

	// The comparison only measures: an agent that can't compare fails nothing.
	t.Run("finishes the rebuild when the comparison fails", func(t *testing.T) {
		h := newHarness(t)
		applied := time.Now()
		failed := testutil.ToFloat64(rebuilds.WithLabelValues("failed"))
		outcome := h.run(t, rebuildJob(h, &applied))
		if outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		if got := testutil.ToFloat64(rebuilds.WithLabelValues("failed")) - failed; got != 1 {
			t.Fatalf("failed rebuilds = %v", got)
		}
	})
}
