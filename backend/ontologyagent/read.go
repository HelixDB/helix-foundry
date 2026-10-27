package ontologyagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// The Read contract (schema/read_request.json, schema/read_result.json):
// reads of one load, with the version's rules applied as they are read,
// within a budget. The agent checks each read's own fields; this checks the
// request's shape and that the result answers every read, in order.

const (
	// MaxReads is the most reads one request carries.
	MaxReads = 20
	// MaxReadsBytes bounds the reads of one request as the caller sent them.
	MaxReadsBytes = 64 << 10
)

// ReadKinds are the reads a request may carry (ReadItem), each answered by
// an answer of the same kind.
var ReadKinds = []string{"entity", "pattern", "explain", "violations", "evidence", "query"}

// ReadRequest reads one load (schema/read_request.json).
type ReadRequest struct {
	// RequestID names the call in the agent's logs; each call sends a new one.
	RequestID string `json:"request_id"`
	// Database names the database to the agent: the tenant id.
	Database string `json:"database"`
	// Load is the load read. Ingest must have created it with OntologyVersion,
	// and it must not be being deleted, or the call is FAILED_PRECONDITION.
	Load            string `json:"load"`
	OntologyTOML    string `json:"ontology_toml"`
	OntologyVersion string `json:"ontology_version"`
	// Helix is the read-only proxy: it forwards reads only.
	Helix HelixTarget `json:"helix"`
	// AsOf is the date rules compare `today` with, YYYY-MM-DD.
	AsOf   string     `json:"as_of"`
	Budget ReadBudget `json:"budget"`
	Reads  Reads      `json:"reads"`
}

// ReadBudget is what one read call may spend (ReadLimits); past any bound a
// read ends in a budget, hub or too_broad error, never a partial answer.
type ReadBudget struct {
	// Rounds are rounds of reads from the graph, per read.
	Rounds uint64 `json:"rounds"`
	// Facts are stated facts read, per read.
	Facts uint64 `json:"facts"`
	// PerList is the most facts one adjacency list may hold: a longer one (a
	// hub) fails what needs it.
	PerList uint64 `json:"per_list"`
	// Instances are rule instances fired or weighed, over the whole call.
	Instances uint64 `json:"instances"`
	// ScanEntities is the most matches a query read may read at any stage
	// before it is too_broad.
	ScanEntities uint64 `json:"scan_entities"`
	// WallMS is the whole call's time, enforced inside evaluation.
	WallMS uint64 `json:"wall_ms"`
}

// DefaultReadBudget is the budget every Foundry read call sends: the agent's
// own defaults.
var DefaultReadBudget = ReadBudget{Rounds: 64, Facts: 200_000, PerList: 10_000, Instances: 2_000_000, ScanEntities: 20_000, WallMS: 5_000}

// Reads are 1 to MaxReads reads, each a JSON object with exactly one key, a
// kind of ReadKinds, whose value is an object. Only ParseReads makes one.
type Reads struct {
	items []json.RawMessage
	// kinds are the items' kinds, in order.
	kinds []string
}

// ParseReads checks a caller's reads: a JSON array of at most MaxReadsBytes.
// Each read is written again from its one key, so the agent reads exactly
// what was checked.
func ParseReads(raw string) (Reads, error) {
	if len(raw) > MaxReadsBytes {
		return Reads{}, fmt.Errorf("reads are at most %d bytes", MaxReadsBytes)
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return Reads{}, errors.New("reads must be a JSON array of objects")
	}
	if len(items) == 0 || len(items) > MaxReads {
		return Reads{}, fmt.Errorf("a request has 1 to %d reads, not %d", MaxReads, len(items))
	}
	out := Reads{items: make([]json.RawMessage, 0, len(items)), kinds: make([]string, 0, len(items))}
	for i, item := range items {
		if len(item) != 1 {
			return Reads{}, fmt.Errorf("read %d must name exactly one of %v", i, ReadKinds)
		}
		for kind, value := range item {
			if !slices.Contains(ReadKinds, kind) {
				return Reads{}, fmt.Errorf("read %d is %q; it must be one of %v", i, kind, ReadKinds)
			}
			if len(value) == 0 || value[0] != '{' {
				return Reads{}, fmt.Errorf("read %d's %s must be an object", i, kind)
			}
			out.kinds = append(out.kinds, kind)
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return Reads{}, fmt.Errorf("read %d: %w", i, err)
		}
		out.items = append(out.items, encoded)
	}
	return out, nil
}

// Len is how many reads there are; the result has one result for each.
func (r Reads) Len() int { return len(r.items) }

// MarshalJSON writes the reads as the array the agent reads.
func (r Reads) MarshalJSON() ([]byte, error) { return json.Marshal(r.items) }

// ReadErrorCode is why one read, or part of an entity read, has no answer.
type ReadErrorCode string

const (
	// ReadBudgetExceeded went past the request's budget: "too deep to compute live".
	ReadBudgetExceeded ReadErrorCode = "budget"
	// ReadHub needs an adjacency list longer than the budget's per_list.
	ReadHub ReadErrorCode = "hub"
	// ReadTooBroad is a query that would read more than scan_entities matches.
	ReadTooBroad ReadErrorCode = "too_broad"
	// ReadUnknownEntity names an entity the load doesn't hold.
	ReadUnknownEntity ReadErrorCode = "unknown_entity"
	// ReadInvalid doesn't fit the ontology: an unknown type, predicate or
	// constraint, a wrong number of arguments, a limit out of range.
	ReadInvalid ReadErrorCode = "invalid"
)

// ReadFailure is why a read has no answer (read_result.json Failure), one
// variant per code, each with its own fields: Limit for budget, hub and
// too_broad; Count too for too_broad; an optional Pointer for invalid.
type ReadFailure struct {
	Code    ReadErrorCode
	Message string
	// Limit is the bound the read went over.
	Limit uint64
	// Count is how many matches a too_broad query would read.
	Count uint64
	// Pointer is, for an invalid query, a JSON pointer to what is wrong in it.
	Pointer *string
}

// readFailureCodes are the codes a whole read may fail with, and
// partFailureCodes those part of an entity read may (PartFailure).
var (
	readFailureCodes = []ReadErrorCode{ReadBudgetExceeded, ReadHub, ReadTooBroad, ReadUnknownEntity, ReadInvalid}
	partFailureCodes = []ReadErrorCode{ReadBudgetExceeded, ReadHub, ReadInvalid}
)

// decodeReadFailure reads a failure whose code is one of codes, refusing one
// without the fields its code needs.
func decodeReadFailure(data json.RawMessage, codes []ReadErrorCode) (ReadFailure, error) {
	var raw struct {
		Code    ReadErrorCode `json:"code"`
		Message *string       `json:"message"`
		Limit   *uint64       `json:"limit"`
		Count   *uint64       `json:"count"`
		Pointer *string       `json:"pointer"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return ReadFailure{}, fmt.Errorf("read failure: %w", err)
	}
	if !slices.Contains(codes, raw.Code) {
		return ReadFailure{}, fmt.Errorf("unknown error code %q", raw.Code)
	}
	if raw.Message == nil {
		return ReadFailure{}, fmt.Errorf("%s error has no message", raw.Code)
	}
	f := ReadFailure{Code: raw.Code, Message: *raw.Message}
	switch raw.Code {
	case ReadBudgetExceeded, ReadHub:
		if raw.Limit == nil {
			return ReadFailure{}, fmt.Errorf("%s error has no limit", raw.Code)
		}
		f.Limit = *raw.Limit
	case ReadTooBroad:
		if raw.Limit == nil || raw.Count == nil {
			return ReadFailure{}, fmt.Errorf("too_broad error lacks its limit or count")
		}
		f.Limit, f.Count = *raw.Limit, *raw.Count
	case ReadInvalid:
		f.Pointer = raw.Pointer
	}
	return f, nil
}

// CheckReadResult checks a Read's result (schema/read_result.json) answers
// reads: as_of and stats, and one result for each read, in order, either
// its answer, of the read's kind, or its failure, whole. An entity answer's
// failed parts must be whole too. The result goes back to the caller as the
// agent wrote it; fields this contract doesn't name are left alone.
func CheckReadResult(data []byte, reads Reads) error {
	var result struct {
		AsOf    *string `json:"as_of"`
		Results []struct {
			OK    json.RawMessage `json:"ok"`
			Error json.RawMessage `json:"error"`
		} `json:"results"`
		Stats *struct {
			Instances *uint64 `json:"instances"`
			WallMS    *uint64 `json:"wall_ms"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("read read result: %w", err)
	}
	switch {
	case result.AsOf == nil:
		return errors.New("read result has no as_of")
	case result.Stats == nil || result.Stats.Instances == nil || result.Stats.WallMS == nil:
		return errors.New("read result has no stats")
	case len(result.Results) != reads.Len():
		return fmt.Errorf("read result has %d results for %d reads", len(result.Results), reads.Len())
	}
	for i, r := range result.Results {
		if isNull(r.OK) == isNull(r.Error) {
			return fmt.Errorf("read result %d must be exactly one of ok and error", i)
		}
		if !isNull(r.Error) {
			if _, err := decodeReadFailure(r.Error, readFailureCodes); err != nil {
				return fmt.Errorf("read result %d: %w", i, err)
			}
			continue
		}
		var answer struct {
			Kind   string            `json:"kind"`
			Errors []json.RawMessage `json:"errors"`
		}
		if err := json.Unmarshal(r.OK, &answer); err != nil {
			return fmt.Errorf("read result %d: %w", i, err)
		}
		if answer.Kind != reads.kinds[i] {
			return fmt.Errorf("read result %d is a %q answer to a %s read", i, answer.Kind, reads.kinds[i])
		}
		for j, part := range answer.Errors {
			var pred struct {
				Pred string `json:"pred"`
			}
			if err := json.Unmarshal(part, &pred); err != nil || pred.Pred == "" {
				return fmt.Errorf("read result %d: error %d names no predicate", i, j)
			}
			if _, err := decodeReadFailure(part, partFailureCodes); err != nil {
				return fmt.Errorf("read result %d: %s: %w", i, pred.Pred, err)
			}
		}
	}
	return nil
}
