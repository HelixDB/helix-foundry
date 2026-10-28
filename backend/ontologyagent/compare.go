package ontologyagent

import (
	"encoding/json"
	"fmt"
)

// CompareRequest compares two loads of one version, kind by kind
// (schema/compare_request.json): what a rebuild checks an applied load
// against. It writes nothing.
type CompareRequest struct {
	RequestID       string      `json:"request_id"`
	Database        string      `json:"database"`
	LoadA           string      `json:"load_a"`
	LoadB           string      `json:"load_b"`
	OntologyTOML    string      `json:"ontology_toml"`
	OntologyVersion string      `json:"ontology_version"`
	Helix           HelixTarget `json:"helix"`
	// MaxKeys is how many differing keys to list per label; zero is the agent's 20.
	MaxKeys uint64 `json:"max_keys,omitempty"`
}

// CompareResult is what two loads hold alike (schema/compare_result.json).
type CompareResult struct {
	Equal bool           `json:"equal"`
	Kinds []KindCompared `json:"kinds"`
}

// KindCompared is one label of one kind in both loads: entity by type, fact
// by link, evidence of rows (row) and documents (quote), and open violation
// records; Differing lists the first keys held by one load only or with
// another digest.
type KindCompared struct {
	Kind      string   `json:"kind"`
	Label     string   `json:"label"`
	ACount    uint64   `json:"a_count"`
	BCount    uint64   `json:"b_count"`
	Equal     bool     `json:"equal"`
	AHash     string   `json:"a_hash"`
	BHash     string   `json:"b_hash"`
	Differing []string `json:"differing"`
}

// DecodeCompareResult reads a Compare's result, refusing one with no kinds.
func DecodeCompareResult(data []byte) (CompareResult, error) {
	var r CompareResult
	if err := json.Unmarshal(data, &r); err != nil {
		return CompareResult{}, fmt.Errorf("read compare result: %w", err)
	}
	if r.Kinds == nil {
		return CompareResult{}, fmt.Errorf("compare result %s lacks its kinds", data)
	}
	return r, nil
}
