package ontologyagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/helixdb/helix-foundry/backend/store/model"
)

// MaxAuditDocuments is the most documents one Audit reads: a page.
const MaxAuditDocuments = 1000

// MaxAuditExamples is the most examples a finding's backing keeps, summed
// over pages as on one (the engine's Backing::MAX_EXAMPLES).
const MaxAuditExamples = 20

// AuditRequest audits one page of a load's documents
// (schema/audit_request.json): 1 to MaxAuditDocuments ids of documents with
// reviews, or none with Rows. It writes nothing and calls no model.
type AuditRequest struct {
	// RequestID is the idempotency key; a retry simply reads again.
	RequestID       string      `json:"request_id"`
	Database        string      `json:"database"`
	Load            string      `json:"load"`
	OntologyTOML    string      `json:"ontology_toml"`
	OntologyVersion string      `json:"ontology_version"`
	MappingsTOML    string      `json:"mappings_toml"`
	Helix           HelixTarget `json:"helix"`
	DocIDs          []string    `json:"doc_ids"`
	// Rows is the load's ingest report's row errors and its sources'
	// drift, sent with one call of a run, so each row finding is counted
	// once; nil for none, and the row kinds go unmeasured.
	Rows *AuditRows `json:"rows,omitempty"`
}

// AuditRows is what a run sends of a load's rows (RowsReported).
type AuditRows struct {
	RowErrors   []AuditMappingErrors `json:"row_errors"`
	SourceDrift []AuditMappingDrift  `json:"source_drift"`
}

// AuditMappingErrors is one mapping's row errors, as the ingest report gave
// them: how many, and some of their texts, no more than Count.
type AuditMappingErrors struct {
	Mapping  string   `json:"mapping"`
	Count    uint64   `json:"count"`
	Examples []string `json:"examples"`
}

// AuditMappingDrift is the fields one mapping's source gained and lost
// since its version was written.
type AuditMappingDrift struct {
	Mapping string   `json:"mapping"`
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
}

// IngestRowErrors are the row errors of a load job's report (each call's
// result by name, the ingest's under "ingest"), by mapping and pass, as
// Audit's Rows takes them; nil when the report doesn't say, as for a load
// whose report was cut to its status or an engine that kept none, so the
// row kinds go unmeasured. The ingest report's shape is the ingest crate's
// (ingest::Report), not the contract's, so it is read leniently. Foundry
// doesn't yet see its sources' fields, so SourceDrift is always empty.
func IngestRowErrors(jobReport json.RawMessage) *AuditRows {
	type counted struct {
		Count    *uint64  `json:"count"`
		Examples []string `json:"examples"`
	}
	type pass struct {
		RowErrors *counted `json:"row_errors"`
	}
	var r struct {
		Ingest struct {
			Status string `json:"status"`
			Report struct {
				Mappings *[]struct {
					Mapping string `json:"mapping"`
					Create  pass   `json:"create"`
					Resolve pass   `json:"resolve"`
				} `json:"mappings"`
			} `json:"report"`
		} `json:"ingest"`
	}
	if json.Unmarshal(jobReport, &r) != nil || r.Ingest.Status != "done" || r.Ingest.Report.Mappings == nil {
		return nil
	}
	rows := &AuditRows{RowErrors: []AuditMappingErrors{}, SourceDrift: []AuditMappingDrift{}}
	for _, m := range *r.Ingest.Report.Mappings {
		for _, p := range []pass{m.Create, m.Resolve} {
			if p.RowErrors == nil || p.RowErrors.Count == nil {
				return nil
			}
			if *p.RowErrors.Count == 0 {
				continue
			}
			examples := p.RowErrors.Examples
			if uint64(len(examples)) > *p.RowErrors.Count {
				examples = examples[:*p.RowErrors.Count]
			}
			rows.RowErrors = append(rows.RowErrors, AuditMappingErrors{Mapping: m.Mapping, Count: *p.RowErrors.Count, Examples: append([]string{}, examples...)})
		}
	}
	return rows
}

// AuditOutcome is how an Audit ended.
type AuditOutcome string

const (
	// AuditDone audited the page: its findings, and whether every kind was
	// read whole.
	AuditDone AuditOutcome = "done"
	// AuditLoadUnknown is a load the graph doesn't hold, or is deleting.
	AuditLoadUnknown AuditOutcome = "load_unknown"
	// AuditLoadMismatch is a load of another version.
	AuditLoadMismatch AuditOutcome = "load_mismatch"
)

// AuditResult is one page's audit (schema/audit_result.json), read
// leniently: a later contract adds fields (contract 4.8.0's spellings,
// Route.gathered and Draft.origin) and kinds, which are kept as sent.
type AuditResult struct {
	Outcome AuditOutcome
	// Findings are the page's, for done.
	Findings []AuditFinding
	// Cut, for done, lists the kinds the agent's 60 s wall cut short: the
	// page must be audited again before the load's counts hold. None when
	// every kind was read whole.
	Cut []string
	// RowsMeasured, for done, is whether the request's rows were audited.
	RowsMeasured bool
	// StoredVersion, for load_mismatch, is the version the load holds.
	StoredVersion string
}

// AuditFinding is one finding of a page, kept as the agent wrote it.
type AuditFinding struct {
	Fingerprint string
	// Kind is its signal's kind: unmatched, unresolved, clash, row_errors,
	// source_drift, or one a later contract adds.
	Kind string
	// raw is the finding's fields, signal its signal's, as sent.
	raw, signal map[string]json.RawMessage
	// backing is the signal's reviews, nil for one backed by none (rows).
	backing *auditBacking
}

// auditBacking is a signal's backing (Backing): reviews, their documents
// and up to MaxAuditExamples examples, as sent.
type auditBacking struct {
	Count    uint64            `json:"count"`
	Sources  uint64            `json:"sources"`
	Examples []json.RawMessage `json:"examples"`
}

// fingerprintPattern is a fingerprint as the engine writes it: a SHA-256,
// lowercase hex.
var fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// maxKindBytes is the longest kind foundry_heal_findings stores.
const maxKindBytes = 32

// DecodeAuditResult reads an Audit's result, refusing an outcome it
// doesn't know and a finding without a fingerprint or kind.
func DecodeAuditResult(data []byte) (AuditResult, error) {
	var raw struct {
		Outcome  AuditOutcome                  `json:"outcome"`
		Findings *[]map[string]json.RawMessage `json:"findings"`
		Mined    *struct {
			Mined string   `json:"mined"`
			Kinds []string `json:"kinds"`
		} `json:"mined"`
		Rows          string  `json:"rows"`
		StoredVersion *string `json:"stored_version"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return AuditResult{}, fmt.Errorf("read audit result: %w", err)
	}
	r := AuditResult{Outcome: raw.Outcome}
	switch raw.Outcome {
	case AuditLoadUnknown:
		return r, nil
	case AuditLoadMismatch:
		if raw.StoredVersion == nil {
			return AuditResult{}, errors.New("audit result load_mismatch lacks its stored_version")
		}
		r.StoredVersion = *raw.StoredVersion
		return r, nil
	case AuditDone:
	default:
		return AuditResult{}, fmt.Errorf("audit result has outcome %q", raw.Outcome)
	}
	if raw.Findings == nil || raw.Mined == nil {
		return AuditResult{}, errors.New("audit result done lacks its findings or mined")
	}
	switch raw.Mined.Mined {
	case "complete":
	case "cut":
		if len(raw.Mined.Kinds) == 0 {
			return AuditResult{}, errors.New("audit result cut names no kinds")
		}
		r.Cut = raw.Mined.Kinds
	default:
		return AuditResult{}, fmt.Errorf("audit result has mined %q", raw.Mined.Mined)
	}
	// An unknown rows is read as unmeasured: nothing is superseded on it.
	r.RowsMeasured = raw.Rows == "measured"
	for i, f := range *raw.Findings {
		finding, err := decodeAuditFinding(f)
		if err != nil {
			return AuditResult{}, fmt.Errorf("audit finding %d: %w", i, err)
		}
		r.Findings = append(r.Findings, finding)
	}
	return r, nil
}

func decodeAuditFinding(raw map[string]json.RawMessage) (AuditFinding, error) {
	f := AuditFinding{raw: raw}
	if err := json.Unmarshal(raw["fingerprint"], &f.Fingerprint); err != nil || !fingerprintPattern.MatchString(f.Fingerprint) {
		return AuditFinding{}, fmt.Errorf("fingerprint %s is not a SHA-256 in lowercase hex", raw["fingerprint"])
	}
	if err := json.Unmarshal(raw["signal"], &f.signal); err != nil || f.signal == nil {
		return AuditFinding{}, errors.New("no signal")
	}
	if err := json.Unmarshal(f.signal["kind"], &f.Kind); err != nil || f.Kind == "" || len(f.Kind) > maxKindBytes {
		return AuditFinding{}, fmt.Errorf("signal kind %s is not a kind", f.signal["kind"])
	}
	if backing, ok := f.signal["backing"]; ok {
		f.backing = &auditBacking{}
		if err := json.Unmarshal(backing, f.backing); err != nil || f.backing.Count == 0 || f.backing.Sources == 0 {
			return AuditFinding{}, fmt.Errorf("backing %s backs nothing", backing)
		}
	}
	return f, nil
}

// AuditSums sums a load's findings over its pages, by fingerprint, in the
// order they were first found. A finding backed by reviews sums its
// reviews and documents (pages partition the documents, so both are
// exact), keeps the first MaxAuditExamples examples and every spelling
// once it is whole (below); one backed by rows comes once a run and is
// kept as found. The first proposal kept is kept: it follows from the
// signal.
//
// Its memory follows the findings that matter, not the load: a finding
// backed by fewer than model.FoundryHealMinSources documents so far is
// held thin, its kind and counts alone, and becomes whole from the page
// that backs it enough, its examples and spellings those of that page and
// the pages after. Seen leaves out a finding still thin: it is no pattern,
// and is neither stored nor shown.
type AuditSums struct {
	order []string
	by    map[string]*AuditFinding
}

// thin reports whether a sum holds a finding backed by too few documents
// to keep whole yet: its kind and counts alone.
func (f *AuditFinding) thin() bool { return f.raw == nil }

// thinned is f held thin: its kind, and its backing's counts with no
// example.
func (f *AuditFinding) thinned() *AuditFinding {
	return &AuditFinding{Fingerprint: f.Fingerprint, Kind: f.Kind, backing: &auditBacking{Count: f.backing.Count, Sources: f.backing.Sources}}
}

// whole is f kept whole: its fields as sent, its signal and backing its
// own to sum into.
func (f *AuditFinding) whole() *AuditFinding {
	out := *f
	out.signal = cloneFields(f.signal)
	if f.backing != nil {
		backing := *f.backing
		backing.Examples = slices.Clone(f.backing.Examples)
		out.backing = &backing
	}
	return &out
}

// Add sums a page's findings in. A fingerprint found again with another
// kind, or backed on one page and not another, is refused: it is no
// finding the engine writes.
func (s *AuditSums) Add(findings []AuditFinding) error {
	if s.by == nil {
		s.by = map[string]*AuditFinding{}
	}
	for _, f := range findings {
		sum, ok := s.by[f.Fingerprint]
		if !ok {
			if f.backing != nil && f.backing.Sources < model.FoundryHealMinSources {
				s.by[f.Fingerprint] = f.thinned()
			} else {
				s.by[f.Fingerprint] = f.whole()
			}
			s.order = append(s.order, f.Fingerprint)
			continue
		}
		if sum.Kind != f.Kind || (sum.backing == nil) != (f.backing == nil) {
			return fmt.Errorf("finding %s is %s on one page and %s on another", f.Fingerprint, sum.Kind, f.Kind)
		}
		if f.backing == nil {
			continue
		}
		if sum.thin() {
			if sum.backing.Sources+f.backing.Sources < model.FoundryHealMinSources {
				sum.backing.Count += f.backing.Count
				sum.backing.Sources += f.backing.Sources
				continue
			}
			// Backed enough: f stands for it from now on, its examples and
			// spellings the first, with the thin sum's counts.
			whole := f.whole()
			whole.backing.Count += sum.backing.Count
			whole.backing.Sources += sum.backing.Sources
			s.by[f.Fingerprint] = whole
			continue
		}
		sum.backing.Count += f.backing.Count
		sum.backing.Sources += f.backing.Sources
		for _, e := range f.backing.Examples {
			if len(sum.backing.Examples) >= MaxAuditExamples {
				break
			}
			if !slices.ContainsFunc(sum.backing.Examples, func(have json.RawMessage) bool { return string(have) == string(e) }) {
				sum.backing.Examples = append(sum.backing.Examples, e)
			}
		}
		if spellings, ok := f.signal["spellings"]; ok {
			sum.signal["spellings"] = unionSpellings(sum.signal["spellings"], spellings)
		}
	}
	return nil
}

// Len is how many findings the sums hold, thin ones too.
func (s *AuditSums) Len() int { return len(s.order) }

// Seen are the sums as foundry_heal_findings stores them: each finding
// backed by model.FoundryHealMinSources documents or by rows, its JSON
// with its backing summed, its support and its sources. A finding still
// thin is left out.
func (s *AuditSums) Seen() ([]model.FoundryHealSeen, error) {
	var out []model.FoundryHealSeen
	for _, fp := range s.order {
		f := s.by[fp]
		if f.thin() {
			continue
		}
		seen := model.FoundryHealSeen{Fingerprint: fp, Kind: f.Kind, Support: f.rowSupport()}
		signal := f.signal
		if f.backing != nil {
			backing, err := json.Marshal(f.backing)
			if err != nil {
				return nil, err
			}
			signal = cloneFields(f.signal)
			signal["backing"] = backing
			seen.Support, seen.Sources = int64(f.backing.Count), int64(f.backing.Sources)
		}
		encodedSignal, err := json.Marshal(signal)
		if err != nil {
			return nil, err
		}
		finding := cloneFields(f.raw)
		finding["signal"] = encodedSignal
		if seen.Finding, err = json.Marshal(finding); err != nil {
			return nil, err
		}
		out = append(out, seen)
	}
	return out, nil
}

// rowSupport is a finding backed by rows' support: its row errors' count,
// the fields its source gained and lost, or 1 for a kind it doesn't know.
func (f *AuditFinding) rowSupport() int64 {
	var errs struct {
		Count int64 `json:"count"`
	}
	var drift struct {
		Added   []json.RawMessage `json:"added"`
		Removed []json.RawMessage `json:"removed"`
	}
	// Best-effort: a kind it doesn't know has neither, and counts 1.
	if json.Unmarshal(f.signal["errors"], &errs) == nil && errs.Count > 0 {
		return errs.Count
	}
	if json.Unmarshal(f.signal["drift"], &drift) == nil && len(drift.Added)+len(drift.Removed) > 0 {
		return int64(len(drift.Added) + len(drift.Removed))
	}
	return 1
}

// unionSpellings is both lists of spellings together, sorted, each once; a
// list that doesn't read as one leaves the other.
func unionSpellings(a, b json.RawMessage) json.RawMessage {
	var x, y []string
	if json.Unmarshal(a, &x) != nil {
		return b
	}
	if json.Unmarshal(b, &y) != nil {
		return a
	}
	all := slices.Concat(x, y)
	slices.Sort(all)
	encoded, _ := json.Marshal(slices.Compact(all))
	return encoded
}

func cloneFields(m map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}
