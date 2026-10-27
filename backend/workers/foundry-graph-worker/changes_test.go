package foundrygraphworker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// changeCase is a customers table the load holds at batch 3, pinned at batch 5.
func changeCase() (tableChange, []*model.ConnectorChangeBatch, *ontologyagent.MappedTable) {
	key := func(s string) *string { return &s }
	return tableChange{
			pin:      model.FoundryLoadSource{ConnectorID: "conn-1", Table: "customers", S3Key: "s5", ChangeSeq: 5},
			position: &model.FoundryLoadSource{ConnectorID: "conn-1", Table: "customers", S3Key: "s3", ChangeSeq: 3},
			table:    &model.ConnectorTable{TableName: "customers", PrimaryKey: json.RawMessage(`["id"]`), RowCount: 1000},
			kind:     "postgres",
		}, []*model.ConnectorChangeBatch{
			{Seq: 4, Kind: model.ConnectorChangesBatch, S3Key: key("b4"), Updates: 2},
			{Seq: 5, Kind: model.ConnectorChangesBatch, S3Key: key("b5"), Deletes: 1},
		}, &ontologyagent.MappedTable{RowKeys: [][]string{{"id"}}}
}

func TestFitsChangeFile(t *testing.T) {
	// A table whose every change since the load's position is in the log goes as those changes.
	t.Run("takes a table the log covers", func(t *testing.T) {
		c, batches, mapped := changeCase()
		if !fitsChangeFile(c, batches, mapped, map[string]bool{}) {
			t.Fatal("refused a table the log covers")
		}
	})

	// Each rule guards a case where the changes alone would build another
	// graph than the snapshot does; any of them sends the snapshot.
	for _, tc := range []struct {
		name  string
		spoil func(*tableChange, *[]*model.ConnectorChangeBatch, *ontologyagent.MappedTable, map[string]bool)
	}{
		{"no position kept for the load", func(c *tableChange, _ *[]*model.ConnectorChangeBatch, _ *ontologyagent.MappedTable, _ map[string]bool) {
			c.position = nil
		}},
		{"a batch swept or missing", func(_ *tableChange, b *[]*model.ConnectorChangeBatch, _ *ontologyagent.MappedTable, _ map[string]bool) {
			*b = (*b)[1:]
		}},
		{"a gap in the numbering", func(_ *tableChange, b *[]*model.ConnectorChangeBatch, _ *ontologyagent.MappedTable, _ map[string]bool) {
			(*b)[1].Seq = 6
		}},
		{"a table replaced whole", func(_ *tableChange, b *[]*model.ConnectorChangeBatch, _ *ontologyagent.MappedTable, _ map[string]bool) {
			(*b)[0].Kind = model.ConnectorReplaceBatch
		}},
		{"numbering restarted below the position", func(c *tableChange, _ *[]*model.ConnectorChangeBatch, _ *ontologyagent.MappedTable, _ map[string]bool) {
			c.position.ChangeSeq = 9
		}},
		{"a windowed table", func(c *tableChange, _ *[]*model.ConnectorChangeBatch, _ *ontologyagent.MappedTable, _ map[string]bool) {
			c.kind, c.pin.Table = "posthog", "events"
		}},
		{"half the table changed", func(c *tableChange, _ *[]*model.ConnectorChangeBatch, _ *ontologyagent.MappedTable, _ map[string]bool) {
			c.table.RowCount = 6
		}},
		{"a row key other than the primary key", func(_ *tableChange, _ *[]*model.ConnectorChangeBatch, m *ontologyagent.MappedTable, _ map[string]bool) {
			m.RowKeys = [][]string{{"email"}}
		}},
		{"a mapping nested under it", func(_ *tableChange, _ *[]*model.ConnectorChangeBatch, m *ontologyagent.MappedTable, _ map[string]bool) {
			m.Nested = true
		}},
		{"another mapping looks it up", func(_ *tableChange, _ *[]*model.ConnectorChangeBatch, m *ontologyagent.MappedTable, _ map[string]bool) {
			m.LookedUp = true
		}},
		{"it looks up a table that changed", func(_ *tableChange, _ *[]*model.ConnectorChangeBatch, m *ontologyagent.MappedTable, changed map[string]bool) {
			m.LooksUp, changed["src_planes"] = []string{"src_planes"}, true
		}},
	} {
		t.Run("sends the snapshot for "+tc.name, func(t *testing.T) {
			c, batches, mapped := changeCase()
			changed := map[string]bool{}
			tc.spoil(&c, &batches, mapped, changed)
			if fitsChangeFile(c, batches, mapped, changed) {
				t.Fatal("took a table its changes can't stand in for")
			}
		})
	}
}

func TestWithoutChanges(t *testing.T) {
	// A position the agent doesn't hold sends that table whole and leaves the rest as changes.
	t.Run("sends the snapshot of each stream at another position", func(t *testing.T) {
		sources := []ontologyagent.Source{
			{ID: "src_a", Changes: &ontologyagent.ChangeFile{Path: "a", FromSeq: 1, ToSeq: 2}},
			{ID: "src_b", Changes: &ontologyagent.ChangeFile{Path: "b", FromSeq: 1, ToSeq: 2}},
		}
		got, err := withoutChanges(sources, []ontologyagent.StoredPosition{{Stream: "src_a\x1fcustomers"}})
		if err != nil || got[0].Changes != nil || got[1].Changes == nil || sources[0].Changes == nil {
			t.Fatalf("withoutChanges = (%+v, %v)", got, err)
		}
	})
	// A stream Helix sent no change file for can't be at the wrong position.
	t.Run("refuses a stream it sent whole", func(t *testing.T) {
		if _, err := withoutChanges([]ontologyagent.Source{{ID: "src_a"}}, []ontologyagent.StoredPosition{{Stream: "src_a\x1fx"}}); err == nil {
			t.Fatal("accepted a mismatch for a snapshot")
		}
	})
}

func (s *memStore) ListFoundryLoadSources(_ context.Context, loadIDs []string) (map[string][]model.FoundryLoadSource, error) {
	out := map[string][]model.FoundryLoadSource{}
	for _, id := range loadIDs {
		if p, ok := s.positions[id]; ok {
			out[id] = p
		}
	}
	return out, nil
}

func (s *memStore) ListConnectorChangeBatchesAfter(_ context.Context, _, connectorID, table string, afterSeq, toSeq int64) (
	[]*model.ConnectorChangeBatch, error) {
	var out []*model.ConnectorChangeBatch
	for _, b := range s.batches {
		if b.ConnectorID == connectorID && b.TableName == table && b.Seq > afterSeq && b.Seq <= toSeq {
			out = append(out, b)
		}
	}
	return out, nil
}

// changedCustomers makes the harness's customers table one the current load
// holds at batch 5 and that two change batches lead from there to the pin
// (firstPin, batch 7), with a version whose mapping keys it by its id.
func changedCustomers(t *testing.T, h *harness) string {
	t.Helper()
	source := ontologyagent.TableSourceID("conn-1", "customers")
	def, _ := json.Marshal(ontologyagent.Definition{
		Format: "agent_bundle", Status: ontologyagent.StatusDone, OntologyTOML: "version = \"draft\"\n",
		MappingsTOML: "ontology = \"draft\"\n[mappings.customers]\nsource = \"" + source + "\"\nrow_key = [\"id\"]\n",
		Draft:        json.RawMessage(`{}`), Questions: json.RawMessage(`[]`),
	})
	h.store.version.Definition = def
	h.store.tables[0].PrimaryKey, h.store.tables[0].RowCount = json.RawMessage(`["id"]`), 100
	h.store.positions = map[string][]model.FoundryLoadSource{"load-0": {{
		ConnectorID: "conn-1", Table: "customers", S3Key: "ws/ws-1/connectors/conn-1/customers/s0.jsonl.gz", ChangeSeq: 5,
	}}}
	for seq, line := range map[int64]string{
		6: `{"op":"upsert","key":{"id":"cus_1"},"row":{"id":"cus_1","name":"Acme"}}`,
		7: `{"op":"delete","key":{"id":"cus_2"}}`,
	} {
		key := fmt.Sprintf("ws/ws-1/connectors/conn-1/customers/changes/b%d.jsonl.gz", seq)
		h.bucket.put(key, gz(t, line+"\n"))
		h.store.batches = append(h.store.batches, &model.ConnectorChangeBatch{
			ConnectorID: "conn-1", TableName: "customers", Seq: seq, Kind: model.ConnectorChangesBatch, S3Key: &key, Updates: 1,
		})
	}
	slices.SortFunc(h.store.batches, func(a, b *model.ConnectorChangeBatch) int { return int(a.Seq - b.Seq) })
	return source
}

// sentAccess is the access of the source an Ingest request sent as id.
func sentAccess(t *testing.T, request, id string) map[string]json.RawMessage {
	t.Helper()
	var req struct {
		Sources []struct {
			ID     string                     `json:"id"`
			Access map[string]json.RawMessage `json:"access"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(request), &req); err != nil {
		t.Fatal(err)
	}
	for _, s := range req.Sources {
		if s.ID == id {
			return s.Access
		}
	}
	t.Fatalf("the request sent no source %s", id)
	return nil
}

func TestApplyChangeFiles(t *testing.T) {
	// A table the log covers since the load's position goes as those changes,
	// merged into one file that is there while Ingest runs and gone after.
	t.Run("sends a changed table as its change batches", func(t *testing.T) {
		h := newHarness(t)
		h.store.pending = nil
		source := changedCustomers(t, h)
		var staged string
		h.agent.ingest = func(req string, stream agentv1.Agent_IngestServer) error {
			var changes ontologyagent.ChangeFile
			if err := json.Unmarshal(sentAccess(t, req, source)["changes"], &changes); err == nil {
				body, _ := os.ReadFile(filepath.Join(h.workDir, changes.Path))
				staged = string(body)
			}
			return ingestApplied(req, stream)
		}
		outcome := h.run(t, applyJob())
		if outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("outcome = %+v", outcome)
		}
		ingests, _ := h.agent.calls()
		var changes ontologyagent.ChangeFile
		if err := json.Unmarshal(sentAccess(t, ingests[0], source)["changes"], &changes); err != nil {
			t.Fatalf("the table went as %s, not its changes", ingests[0])
		}
		if changes.FromSeq != 5 || changes.ToSeq != 7 || changes.Path != "jobs/job-4/changes/"+source+".jsonl" {
			t.Fatalf("changes = %+v", changes)
		}
		want := `{"op":"upsert","key":{"id":"cus_1"},"row":{"id":"cus_1","name":"Acme"}}` + "\n" + `{"op":"delete","key":{"id":"cus_2"}}` + "\n"
		if staged != want {
			t.Fatalf("staged change file =\n%s", staged)
		}
		if _, err := os.Stat(filepath.Join(h.workDir, changes.Path)); !os.IsNotExist(err) {
			t.Fatalf("the change file outlived the attempt: %v", err)
		}
	})

	// The agent holds the table elsewhere than the file starts from: the same
	// attempt sends its snapshot instead, under a request of its own.
	t.Run("sends the snapshot of a table at another position", func(t *testing.T) {
		h := newHarness(t)
		h.store.pending = nil
		source := changedCustomers(t, h)
		h.agent.ingest = func(req string, stream agentv1.Agent_IngestServer) error {
			if _, ok := sentAccess(t, req, source)["changes"]; ok {
				return ingestEvents(stream, nil, `{"status":"position_mismatch","streams":[{"stream":"`+source+`\u001fcustomers","stored_change_seq":4}]}`)
			}
			return ingestApplied(req, stream)
		}
		outcome := h.run(t, applyJob())
		if outcome.Status != model.FoundryGraphJobSucceeded || !outcome.IngestDone {
			t.Fatalf("outcome = %+v", outcome)
		}
		ingests, _ := h.agent.calls()
		if len(ingests) != 2 {
			t.Fatalf("%d ingests, want the change file then the snapshot", len(ingests))
		}
		if _, ok := sentAccess(t, ingests[1], source)["file"]; !ok || !strings.Contains(ingests[1], `"request_id":"job-4:snapshots"`) {
			t.Fatalf("second request = %s", ingests[1])
		}
	})

	// A load from before positions were kept holds none: its tables go whole.
	t.Run("sends snapshots for a load with no positions", func(t *testing.T) {
		h := newHarness(t)
		h.store.pending = nil
		source := changedCustomers(t, h)
		h.store.positions = nil
		h.agent.ingest = ingestApplied
		h.run(t, applyJob())
		ingests, _ := h.agent.calls()
		if _, ok := sentAccess(t, ingests[0], source)["file"]; !ok {
			t.Fatalf("request = %s", ingests[0])
		}
	})
}
