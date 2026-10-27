package ontologyagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"

	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// ReviseDefinition is the immutable draft the agent rebuilt. JSON fields carry
// serialized JSON, not a second, backend-owned interpretation of DraftView.
type ReviseDefinition struct {
	OntologyTOML  string `json:"ontology_toml"`
	MappingsTOML  string `json:"mappings_toml"`
	DraftJSON     string `json:"draft_json"`
	QuestionsJSON string `json:"questions_json"`
}

// ReviseEval is the current load a revision is measured on, read through a
// read-only proxy (schema/revise_request.json Eval). Load is its id, the id
// of the job that created it (printable ASCII, no space); OntologyVersion is
// the version its marker records, and OntologyTOML and MappingsTOML that
// version's ontology.toml and mappings.toml as stored, never the draft's: the
// engine reads the load's People enrichment item from MappingsTOML, and a
// constraint over what only its profiles filled is not measured on the load.
// Either file not parsing refuses the revision (Evaluation). The engine
// measures on the load only if the revised draft keeps the load's types,
// links and props and the marker records OntologyVersion; otherwise it
// reports no_comparable_load and caps changed constraints at warn.
// Evaluation never writes to this target.
type ReviseEval struct {
	Database        string      `json:"database"`
	Load            string      `json:"load"`
	OntologyVersion string      `json:"ontology_version"`
	OntologyTOML    string      `json:"ontology_toml"`
	MappingsTOML    string      `json:"mappings_toml"`
	Helix           HelixTarget `json:"helix"`
}

// ReviseRequest is Revise's unary request. The agent owns edit
// policy (including identifiers-only type edits), compilation and evaluation.
type ReviseRequest struct {
	Definition ReviseDefinition    `json:"definition"`
	Edits      model.OntologyEdits `json:"edits"`
	Eval       *ReviseEval         `json:"eval,omitempty"`
}

// ReviseStatus is how a revision ended: revised, with a complete definition
// and the whole draft's rule counts, or refused, with neither.
type ReviseStatus string

const (
	ReviseRevised ReviseStatus = "revised"
	ReviseRefused ReviseStatus = "refused"
)

// ReviseFindingCode is a stable agent refusal or nonfatal coverage/lint code.
// Lint and no_comparable_load may come with a revised result; budget comes
// alone, in a refusal with no cards: measuring on the load needed more rule
// instances or wall time than the engine allows, and no card comes from part
// of a measurement.
type ReviseFindingCode string

const (
	ReviseEditNotAllowed    ReviseFindingCode = "EditNotAllowed"
	ReviseInvalidDefinition ReviseFindingCode = "InvalidDefinition"
	ReviseInvalidEdit       ReviseFindingCode = "InvalidEdit"
	ReviseValidation        ReviseFindingCode = "Validation"
	ReviseEvaluation        ReviseFindingCode = "Evaluation"
	ReviseLint              ReviseFindingCode = "Lint"
	ReviseNoComparableLoad  ReviseFindingCode = "no_comparable_load"
	ReviseBudget            ReviseFindingCode = "budget"
)

// ReviseFinding names an edited item, or nil for a whole-definition finding.
type ReviseFinding struct {
	Code    ReviseFindingCode `json:"code"`
	ItemID  *string           `json:"item_id"`
	Message string            `json:"message"`
}

// UnmarshalJSON requires the schema's fields, including an explicit nullable item_id.
func (f *ReviseFinding) UnmarshalJSON(raw []byte) error {
	var fields struct {
		Code    ReviseFindingCode `json:"code"`
		ItemID  json.RawMessage   `json:"item_id"`
		Message *string           `json:"message"`
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	switch fields.Code {
	case ReviseEditNotAllowed, ReviseInvalidDefinition, ReviseInvalidEdit, ReviseValidation, ReviseEvaluation, ReviseLint, ReviseNoComparableLoad,
		ReviseBudget:
	default:
		return fmt.Errorf("unknown revise finding code %q", fields.Code)
	}
	if fields.Message == nil || fields.ItemID == nil {
		return fmt.Errorf("revise finding requires message and item_id")
	}
	var id *string
	if err := json.Unmarshal(fields.ItemID, &id); err != nil {
		return err
	}
	*f = ReviseFinding{Code: fields.Code, ItemID: id, Message: *fields.Message}
	return nil
}

// ReviseVerdict is the harness outcome for one evaluated item.
type ReviseVerdict string

const (
	ReviseRule        ReviseVerdict = "rule"
	ReviseWarn        ReviseVerdict = "warn"
	ReviseCardRefused ReviseVerdict = "refused"
)

// ReviseCard retains the agent's EvidenceCard JSON inside its per-item envelope.
// Its card's unmeasured says what its counts leave out: rows (a mapping
// missing from the dry run's sample), documents (preview coverage), both
// ("rows, documents"), load (no comparable load) or entities (some over the
// read budget). Any caps a constraint at warn; a rule that missing rows can
// fill is refused.
type ReviseCard struct {
	Item    string          `json:"item"`
	Verdict ReviseVerdict   `json:"verdict"`
	Card    json.RawMessage `json:"card"`
}

// ReviseResult preserves engine evidence. A refused result
// carries no definition; a revised result must carry a complete definition.
type ReviseResult struct {
	Status     ReviseStatus      `json:"status"`
	Definition *ReviseDefinition `json:"definition,omitempty"`
	Findings   []ReviseFinding   `json:"findings"`
	Cards      []ReviseCard      `json:"cards"`
	Rules      *RuleCounts       `json:"rules,omitempty"`
}

// DecodeReviseResult reads a Revise result (schema/revise_result.json),
// refusing one the schema's union doesn't allow: exactly a revised result
// carries a complete definition (its four fields, with draft_json an object
// and questions_json an array) and rule counts with every rule and
// constraint evaluated; a refused one carries neither key, not even as null.
// Findings and cards are always arrays; every finding has its code, its
// nullable item_id and its message; every card its item, a known verdict and
// an evidence object; a budget finding is a refusal's only finding, with no
// cards. Unknown fields and trailing JSON are refused. Card evidence is kept
// as the engine wrote it.
func DecodeReviseResult(raw []byte) (ReviseResult, error) {
	var result ReviseResult
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	if decoder.More() {
		return result, fmt.Errorf("revise result has trailing JSON")
	}
	if result.Findings == nil || result.Cards == nil {
		return result, fmt.Errorf("revise result requires findings and cards arrays")
	}
	budget := slices.ContainsFunc(result.Findings, func(f ReviseFinding) bool { return f.Code == ReviseBudget })
	if budget && (result.Status != ReviseRefused || len(result.Findings) != 1 || len(result.Cards) != 0) {
		return result, fmt.Errorf("a budget finding refuses the revision alone, with no cards")
	}
	for _, card := range result.Cards {
		if card.Verdict != "rule" && card.Verdict != "warn" && card.Verdict != "refused" {
			return result, fmt.Errorf("unknown revise verdict %q", card.Verdict)
		}
		var evidence map[string]json.RawMessage
		if card.Item == "" || json.Unmarshal(card.Card, &evidence) != nil || evidence == nil {
			return result, fmt.Errorf("revise card requires item and evidence")
		}
	}
	// Refused variants omit these keys, including explicit null values.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return result, err
	}
	switch result.Status {
	case ReviseRefused:
		if fields["definition"] != nil || fields["rules"] != nil {
			return result, fmt.Errorf("refused revision carries a definition or rules")
		}
	case ReviseRevised:
		if result.Rules == nil || result.Rules.Evaluated != result.Rules.Count {
			return result, fmt.Errorf("revised result requires valid rule counts")
		}
		var counts struct {
			Count     *uint32 `json:"count"`
			Evaluated *uint32 `json:"evaluated"`
		}
		if err := json.Unmarshal(fields["rules"], &counts); err != nil || counts.Count == nil || counts.Evaluated == nil {
			return result, fmt.Errorf("revised result requires count and evaluated")
		}
		var definition map[string]json.RawMessage
		if err := json.Unmarshal(fields["definition"], &definition); err != nil {
			return result, err
		}
		for _, field := range []string{"ontology_toml", "mappings_toml", "draft_json", "questions_json"} {
			if len(definition[field]) == 0 || bytes.Equal(definition[field], []byte("null")) {
				return result, fmt.Errorf("revised definition requires %s", field)
			}
		}
		d := result.Definition
		var draft map[string]json.RawMessage
		var questions []json.RawMessage
		if d != nil {
			if json.Unmarshal([]byte(d.DraftJSON), &draft) != nil || json.Unmarshal([]byte(d.QuestionsJSON), &questions) != nil {
				return result, fmt.Errorf("revised definition has invalid draft or questions JSON")
			}
		}
		if d == nil || d.OntologyTOML == "" || draft == nil || questions == nil {
			return result, fmt.Errorf("revised result has no complete definition")
		}
	default:
		return result, fmt.Errorf("unknown revise status %q", result.Status)
	}
	return result, nil
}

// Revise calls the agent's unary Revise RPC. The worker's context bounds the whole run.
func (c *Client) Revise(ctx context.Context, request []byte) (json.RawMessage, error) {
	out, err := c.agent.Revise(ctx, &agentv1.ReviseRequest{RequestJson: string(request)})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out.GetResultJson()), nil
}
