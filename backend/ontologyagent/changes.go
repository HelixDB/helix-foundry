package ontologyagent

import (
	"fmt"
	"slices"

	"github.com/BurntSushi/toml"
)

// ChangeFile is a table's changes from one position to another
// (SourceAccess changes): JSON Lines of {"op":"upsert","key","row"} and
// {"op":"delete","key"}, one line per key, as snapshot.MergeBatches writes
// them. The load must hold the table at change batch FromSeq; afterwards it
// holds ToSeq.
type ChangeFile struct {
	Path    string `json:"path"`
	FromSeq int64  `json:"from_seq"`
	ToSeq   int64  `json:"to_seq"`
}

// MappedTable is how a version's mappings read one source.
type MappedTable struct {
	// RowKeys are the row_key of each mapping that reads it.
	RowKeys [][]string
	// Nested is a mapping that reads it under a parent mapping's records.
	Nested bool
	// LookedUp is a lookup, from any mapping, into one of its mappings.
	LookedUp bool
	// LooksUp are the sources its mappings look up into, once each.
	LooksUp []string
}

// MappedTables reads which sources a mappings.toml's mappings read, by source
// id, and how they key and look up each other. A nested mapping counts
// toward its parent's source.
func MappedTables(mappingsTOML string) (map[string]*MappedTable, error) {
	var file struct {
		Mappings map[string]struct {
			Source string   `toml:"source"`
			Parent string   `toml:"parent"`
			RowKey []string `toml:"row_key"`
			Bind   map[string]struct {
				Lookup *struct {
					Mapping string `toml:"mapping"`
				} `toml:"lookup"`
			} `toml:"bind"`
		} `toml:"mappings"`
	}
	if _, err := toml.Decode(mappingsTOML, &file); err != nil {
		return nil, fmt.Errorf("read mappings: %w", err)
	}
	// source follows parents to the mapping that names a source; a cycle or
	// an unknown parent reads none, which the agent refuses anyway.
	source := func(name string) string {
		for range len(file.Mappings) + 1 {
			m, ok := file.Mappings[name]
			if !ok || m.Source != "" {
				return m.Source
			}
			name = m.Parent
		}
		return ""
	}
	out := map[string]*MappedTable{}
	table := func(id string) *MappedTable {
		if out[id] == nil {
			out[id] = &MappedTable{}
		}
		return out[id]
	}
	for name, m := range file.Mappings {
		id := source(name)
		if id == "" {
			continue
		}
		t := table(id)
		t.RowKeys = append(t.RowKeys, m.RowKey)
		if m.Parent != "" {
			t.Nested = true
		}
		for _, b := range m.Bind {
			if b.Lookup == nil {
				continue
			}
			target := source(b.Lookup.Mapping)
			if target == "" {
				continue
			}
			table(target).LookedUp = true
			if !slices.Contains(t.LooksUp, target) {
				t.LooksUp = append(t.LooksUp, target)
			}
		}
	}
	return out, nil
}
