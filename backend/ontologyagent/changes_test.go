package ontologyagent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestMappedTables(t *testing.T) {
	// A change file can stand in for a table only when nothing else reads it
	// through a lookup, so each source says who looks it up and whom it does.
	t.Run("reads keys and lookups by source", func(t *testing.T) {
		got, err := MappedTables(`ontology = "aviation-v1"
[mappings.planes]
source = "src_planes"
row_key = ["id"]
[mappings.planes.bind.plane]
type = "Plane"
[mappings.flights]
source = "src_flights"
row_key = ["id"]
[mappings.flights.bind.plane]
type = "Plane"
lookup = { mapping = "planes", on = { id = "plane_id" } }
[mappings.maintenance]
source = "src_maintenance"
row_key = ["/record_id"]
[mappings.maintenance_parts]
parent = "maintenance"
row_key = ["/part_no"]
`)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]*MappedTable{
			"src_planes":      {RowKeys: [][]string{{"id"}}, LookedUp: true},
			"src_flights":     {RowKeys: [][]string{{"id"}}, LooksUp: []string{"src_planes"}},
			"src_maintenance": {Nested: true},
		}
		if m := got["src_maintenance"]; m == nil || len(m.RowKeys) != 2 {
			t.Fatalf("maintenance = %+v: its nested mapping counts toward its source", m)
		}
		got["src_maintenance"].RowKeys = nil
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("MappedTables = %+v", got)
		}
	})
}

func TestChangeFileSource(t *testing.T) {
	// A changes source is sent instead of the snapshot's file, with the batch range it covers.
	t.Run("sends changes in place of the file", func(t *testing.T) {
		raw, err := json.Marshal(Source{ID: "src_a", Path: "graph/a.jsonl", Fingerprint: "f",
			Changes: &ChangeFile{Path: "jobs/j/changes/src_a.jsonl", FromSeq: 3, ToSeq: 5}})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"access":{"changes":{"path":"jobs/j/changes/src_a.jsonl","from_seq":3,"to_seq":5}}`) {
			t.Fatalf("source = %s", raw)
		}
	})
}
