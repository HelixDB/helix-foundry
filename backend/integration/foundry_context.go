package integration

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// maxExampleQuestions is how many example questions get_context returns: as many as Foundry's
// Analyst suggests for a new thread (askSuggestions on the website).
const maxExampleQuestions = 4

// GetFoundryContextOutput is what an agent needs before it asks or reads an ontology: the schema of
// the version its answers come from, and questions that version was drafted to answer.
type GetFoundryContextOutput struct {
	ContentTrust string                `json:"content_trust"`
	Ontology     FoundryOntologyOutput `json:"ontology"`
	// Version is the current load's version, else the current version; absent while the ontology
	// has neither, or when the caller's grants hold no load.
	Version *FoundryVersionOutput `json:"version,omitempty" jsonschema:"The version described: the current load's, else the current version while nothing is loaded. Absent while the ontology has no version, or when the caller's source grants cover no load."`
	// OntologyTOML is an agent bundle's schema as the ontology agent wrote it.
	OntologyTOML string `json:"ontology_toml,omitempty" jsonschema:"Untrusted. An agent bundle's schema, its ontology.toml: [types.<Type>] with description, props (name = type), key, names and identifiers; [links.<name>] with from, to, description, cardinality and required; [rules.<name>] and [constraints.<name>] with description, severity and datalog. Types and links are the ones ask and read_graph name."`
	// Definition is a Definition v1 version's schema, which has no ontology.toml.
	Definition any `json:"definition,omitempty" jsonschema:"Untrusted. A Definition v1 version's schema: its entities, relationships, rules and constraints."`
	// ExampleQuestions are the questions the version was drafted to answer, as the Analyst
	// suggests them.
	ExampleQuestions []string `json:"example_questions" jsonschema:"Untrusted. Up to 4 questions the version was drafted to answer, the ones Foundry's Analyst suggests: good first questions for helix_foundry_ask. Empty for a Definition v1 version, a draft without them, and a caller whose source grants cover only part of the load."`
}

func (s *Service) GetFoundryContext(
	ctx context.Context, identity *Identity, call *Call, input OntologyInput,
) (GetFoundryContextOutput, int, error) {
	call.ResourceKind, call.ResourceID = "ontology", input.OntologyID
	ws, id := input.WorkspaceID, input.OntologyID
	if !ValidIDs([]string{ws, id}) {
		return GetFoundryContextOutput{}, 0, errCodeInvalidArguments
	}
	req := &foundryv1.GetFoundryOntologyRequest{WorkspaceId: ws, OntologyId: id}
	// The calls a grantee may make too: the load its grants cover, and that load's version.
	if err := s.AuthorizeFoundry(ctx, identity, req, QueryRead,
		"GetFoundryOntology", "GetFoundryLoad", "GetOntologyVersion"); err != nil {
		return GetFoundryContextOutput{}, 0, err
	}
	ontology, err := s.Foundry.GetFoundryOntology(ctx, req)
	if err != nil {
		return GetFoundryContextOutput{}, 0, FoundryError(err)
	}
	out := GetFoundryContextOutput{
		ContentTrust: contentTrustUntrusted, Ontology: s.FoundryOntology(identity, ws, ontology.GetOntology()), ExampleQuestions: []string{},
	}
	// Ask and read_graph answer from the current load, so its version is the one to describe;
	// with nothing loaded, the current version is what a load would hold. A grantee reads only
	// the version of the load its grants cover, so one with no load is told nothing more.
	versionID := ontology.GetOntology().GetCurrentVersionId()
	if loadID := ontology.GetOntology().GetCurrentLoad().GetId(); loadID != "" {
		load, err := s.Foundry.GetFoundryLoad(ctx, &foundryv1.GetFoundryLoadRequest{WorkspaceId: ws, OntologyId: id, Id: loadID})
		if err != nil {
			return GetFoundryContextOutput{}, 0, FoundryError(err)
		}
		versionID = load.GetLoad().GetVersionId()
	} else if identity.IsGrantee != nil && identity.IsGrantee(ctx) {
		return out, 1, nil
	}
	if versionID == "" {
		return out, 1, nil
	}
	response, err := s.Foundry.GetOntologyVersion(ctx, &foundryv1.GetOntologyVersionRequest{WorkspaceId: ws, OntologyId: id, Id: versionID})
	if err != nil {
		return GetFoundryContextOutput{}, 0, FoundryError(err)
	}
	version := s.FoundryVersion(ws, id, response.GetVersion(), false)
	out.Version = &version
	// FoundryVersion leaves the draft out of what it returns; the example questions are only there.
	definition := response.GetVersion().GetDefinition().AsMap()
	size := 0
	if version.Format == "agent_bundle" {
		out.OntologyTOML, _ = definition["ontology_toml"].(string)
		out.ExampleQuestions = exampleQuestions(definition)
		size = len(out.OntologyTOML)
	} else if len(definition) > 0 {
		raw, _ := json.Marshal(definition)
		out.Definition, size = definition, len(raw)
	}
	if size > maxFoundryDefinitionBytes {
		message := fmt.Sprintf("the schema is %d KiB, past the %d KiB this tool returns; open the version in Foundry", size>>10, maxFoundryDefinitionBytes>>10)
		if version.URL != "" {
			message += ": " + version.URL
		}
		return GetFoundryContextOutput{}, 0, detailedToolError{code: errCodeFailedPrecondition, message: message}
	}
	return out, 1, nil
}

// exampleQuestions are an agent bundle's competency questions as the website's askSuggestions
// picks them: the draft's cq.<id> items in their ids' natural order, each trimmed question once,
// the first maxExampleQuestions. They are review data in no file, so only the draft holds them,
// each as a TOML snippet ([cq.CQ1] question = "…"); one that doesn't read is skipped.
func exampleQuestions(definition map[string]any) []string {
	view, _ := definition["draft"].(map[string]any)
	draft, _ := view["draft"].(map[string]any)
	items, _ := draft["items"].(map[string]any)
	type competency struct{ id, question string }
	var found []competency
	for key, item := range items {
		id, ok := strings.CutPrefix(key, "cq.")
		fields, _ := item.(map[string]any)
		snippet, _ := fields["toml"].(string)
		var doc struct {
			CQ map[string]struct {
				Question string `toml:"question"`
			} `toml:"cq"`
		}
		if !ok || snippet == "" {
			continue
		}
		if _, err := toml.Decode(snippet, &doc); err != nil {
			continue
		}
		found = append(found, competency{id: id, question: strings.TrimSpace(doc.CQ[id].Question)})
	}
	slices.SortFunc(found, func(a, b competency) int { return naturalCompare(a.id, b.id) })
	questions := []string{}
	for _, c := range found {
		if c.question == "" || slices.Contains(questions, c.question) {
			continue
		}
		if questions = append(questions, c.question); len(questions) == maxExampleQuestions {
			break
		}
	}
	return questions
}

// idChunks splits an id into runs of digits and runs of anything else.
var idChunks = regexp.MustCompile(`\d+|\D+`)

// naturalCompare orders ids as the website's localeCompare with numeric collation does the
// agent's: runs of digits by their value, so CQ2 comes before CQ10.
func naturalCompare(a, b string) int {
	x, y := idChunks.FindAllString(a, -1), idChunks.FindAllString(b, -1)
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] == y[i] {
			continue
		}
		nx, errX := strconv.ParseUint(x[i], 10, 64)
		ny, errY := strconv.ParseUint(y[i], 10, 64)
		if errX == nil && errY == nil && nx != ny {
			return cmp.Compare(nx, ny)
		}
		return strings.Compare(x[i], y[i])
	}
	return cmp.Compare(len(x), len(y))
}
