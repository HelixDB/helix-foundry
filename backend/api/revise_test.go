package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

// A revision reads what its parent version reads (its scope), not only what
// the parent's generation picked; the worker adds the sets its revised
// routes name.
func TestReviseOntologyVersion(t *testing.T) {
	for _, dry := range []bool{false, true} {
		t.Run(map[bool]string{false: "create", true: "preview"}[dry], func(t *testing.T) {
			s, db := generationService(t, 20)
			base := &foundrymodel.OntologyGeneration{ID: "base-gen", OntologyID: "ont-1", Status: foundrymodel.OntologyGenerationSucceeded, Input: foundrymodel.OntologyGenerationInput{Tables: []foundrymodel.OntologyGenerationTable{{ConnectorID: "conn-1", Table: "customers"}}, DocumentSetIDs: []string{"set"},
				Documents: []foundrymodel.OntologyGenerationDocument{{ID: "doc-csv", SetID: "set"}}}}
			db.generations = append(db.generations, base)
			raw, _ := json.Marshal(ontologyagent.Definition{Status: ontologyagent.StatusDone, Draft: json.RawMessage(`{"draft":{"items":{}}}`)})
			// The parent reads more than its generation picked: a revision of it added a set its routes name.
			scope := []string{"set", "set-routed"}
			db.versions = append(db.versions, &foundrymodel.OntologyVersion{ID: "parent", OntologyID: "ont-1", Format: foundrymodel.OntologyFormatAgentBundle, GenerationID: &base.ID, Definition: raw,
				DocumentSetIDs: &scope})
			edits := []*foundryv1.OntologyEdit{{Edit: &foundryv1.OntologyEdit_Put{Put: &foundryv1.OntologyPut{ItemId: "extraction.contracts", Toml: "[extractions.contracts]"}}}}
			req := &foundryv1.ReviseOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", VersionId: "parent", Edits: edits, DryRun: dry}
			response, err := s.ReviseOntologyVersion(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			gen := response.Generation
			if gen.Revision.GetParentVersionId() != "parent" || gen.Revision.GetDryRun() != dry || gen.ResultVersionId != "" || gen.BaseVersionId != "" ||
				gen.Tables[0].Table != "customers" || strings.Join(gen.DocumentSetIds, ",") != "set,set-routed" ||
				len(gen.Documents) != 1 || gen.Documents[0].GetId() != "doc-csv" {
				t.Fatalf("bad generation: %v", gen)
			}
			queued := db.generations[len(db.generations)-1]
			if len(db.versions) != 1 || len(db.generations) != 2 || queued.Input.BaseVersionID != "" ||
				queued.Input.Revise.Edits[0] != (foundrymodel.OntologyPut{ItemID: "extraction.contracts", TOML: "[extractions.contracts]"}) {
				t.Fatal("request path must only queue the edits it was given")
			}
			if _, err = s.ReviseOntologyVersion(context.Background(), req); status.Code(err) != codes.AlreadyExists {
				t.Fatal(err)
			}
		})
	}
}

func put(item, toml string) *foundryv1.OntologyEdit {
	return &foundryv1.OntologyEdit{Edit: &foundryv1.OntologyEdit_Put{Put: &foundryv1.OntologyPut{ItemId: item, Toml: toml}}}
}

func remove(item string) *foundryv1.OntologyEdit {
	return &foundryv1.OntologyEdit{Edit: &foundryv1.OntologyEdit_Remove{Remove: &foundryv1.OntologyRemove{ItemId: item}}}
}

func TestReviseOntologyVersionValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*foundryv1.ReviseOntologyVersionRequest, *generationStore)
		code   codes.Code
	}{
		{"many edits", func(r *foundryv1.ReviseOntologyVersionRequest, _ *generationStore) {
			r.Edits = make([]*foundryv1.OntologyEdit, 101)
		}, codes.InvalidArgument},
		{"empty item", func(r *foundryv1.ReviseOntologyVersionRequest, _ *generationStore) { r.Edits[0] = remove("") }, codes.InvalidArgument},
		{"blank put", func(r *foundryv1.ReviseOntologyVersionRequest, _ *generationStore) { r.Edits[0] = put(" ", "") }, codes.InvalidArgument},
		{"long item", func(r *foundryv1.ReviseOntologyVersionRequest, _ *generationStore) {
			r.Edits[0] = remove(strings.Repeat("x", 257))
		}, codes.InvalidArgument},
		{"no edits", func(r *foundryv1.ReviseOntologyVersionRequest, _ *generationStore) { r.Edits = nil }, codes.InvalidArgument},
		{"neither put nor remove", func(r *foundryv1.ReviseOntologyVersionRequest, _ *generationStore) {
			r.Edits[0] = &foundryv1.OntologyEdit{}
		}, codes.InvalidArgument},
		{"nil edit", func(r *foundryv1.ReviseOntologyVersionRequest, _ *generationStore) { r.Edits[0] = nil }, codes.InvalidArgument},
		{"duplicate", func(r *foundryv1.ReviseOntologyVersionRequest, _ *generationStore) {
			r.Edits = append(r.Edits, put("rule.test", "[rules.test]"))
		}, codes.InvalidArgument},
		{"huge", func(r *foundryv1.ReviseOntologyVersionRequest, _ *generationStore) {
			r.Edits = []*foundryv1.OntologyEdit{put("rule.a", strings.Repeat("x", 1<<19)), put("rule.b", strings.Repeat("x", 1<<19+1))}
		}, codes.InvalidArgument},
		{"other workspace", func(r *foundryv1.ReviseOntologyVersionRequest, _ *generationStore) { r.WorkspaceId = "ws-2" }, codes.NotFound},
		{"other ontology", func(r *foundryv1.ReviseOntologyVersionRequest, _ *generationStore) { r.OntologyId = "ont-2" }, codes.NotFound},
		{"missing parent", func(r *foundryv1.ReviseOntologyVersionRequest, _ *generationStore) { r.VersionId = "missing" }, codes.NotFound},
		{"old format", func(_ *foundryv1.ReviseOntologyVersionRequest, s *generationStore) {
			s.versions[0].Format = foundrymodel.OntologyFormatFoundryV1
		}, codes.FailedPrecondition},
		{"unfinished", func(_ *foundryv1.ReviseOntologyVersionRequest, s *generationStore) {
			s.versions[0].Definition = json.RawMessage(`{"status":"needs_input"}`)
		}, codes.FailedPrecondition},
		{"no generation", func(_ *foundryv1.ReviseOntologyVersionRequest, s *generationStore) { s.versions[0].GenerationID = nil }, codes.FailedPrecondition},
		{"missing generation", func(_ *foundryv1.ReviseOntologyVersionRequest, s *generationStore) { s.generations = nil }, codes.FailedPrecondition},
		{"ontology deleted before it is queued", func(_ *foundryv1.ReviseOntologyVersionRequest, s *generationStore) {
			s.enqueueErr = foundrystore.ErrNotFound
		}, codes.NotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, db := generationService(t, 20)
			id := "base"
			db.versions = append(db.versions, &foundrymodel.OntologyVersion{ID: "parent", OntologyID: "ont-1", Format: foundrymodel.OntologyFormatAgentBundle, GenerationID: &id, Definition: json.RawMessage(`{"status":"done","draft":{}}`)})
			db.generations = append(db.generations, &foundrymodel.OntologyGeneration{ID: id, OntologyID: "ont-1", Status: foundrymodel.OntologyGenerationSucceeded})
			req := &foundryv1.ReviseOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", VersionId: "parent", Edits: []*foundryv1.OntologyEdit{remove("rule.test")}}
			tc.change(req, db)
			before := len(db.generations)
			_, err := s.ReviseOntologyVersion(context.Background(), req)
			if status.Code(err) != tc.code || len(db.generations) != before || len(db.versions) != 1 {
				t.Fatalf("%v, generations %d", err, len(db.generations))
			}
		})
	}
}

// What Revise measured on the load (its result's cards, counts and findings,
// and the cards it leaves in a revised version's draft) is shown only to a
// caller who may query the ontology's database, as reading the load needs.
// Anyone else is told it was withheld.
func TestOntologyLoadEvidence(t *testing.T) {
	scopes := map[string]struct {
		workspace string
		scope     ports.ProjectScope
		evidence  bool
	}{
		"every project":       {"ws-1", ports.AllProjects(), true},
		"its project":         {"ws-1", ports.SelectedProjects([]string{"proj-1"}), true},
		"another":             {"ws-1", ports.SelectedProjects([]string{"proj-2"}), false},
		"none":                {"ws-1", ports.NoProjects(), false},
		"another workspace's": {"ws-2", ports.AllProjects(), false},
	}
	card := `{"rev":1,"instances":40,"counterexamples":[{"fact":"Order o-17 has no customer","proof":[],"rows":[]}]}`
	result := `{"status":"revised","findings":[],"cards":[{"item":"rule.a","verdict":"rule","card":` + card + `}],"rules":{"count":1,"evaluated":1}}`
	draft := func(parent string) json.RawMessage {
		return json.RawMessage(`{"format":"agent_bundle","status":"done","parent_version_id":"` + parent + `","draft":{"ontology_toml":"","mappings_toml":"","draft":{"items":{
"rule.a":{"toml":"[rules.a]","verdict":"rule","card":` + card + `},"type.Order":{"toml":"[types.Order]"}}}}}`)
	}
	for name, tc := range scopes {
		t.Run(name, func(t *testing.T) {
			ctx := ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceUser, UserID: "user-1", WorkspaceID: tc.workspace, QueryRead: tc.scope})
			s, db := generationService(t, 20)
			revise := &foundrymodel.OntologyRevision{ParentVersionID: "parent"}
			db.generations = append(db.generations,
				&foundrymodel.OntologyGeneration{ID: "revised", OntologyID: "ont-1", Status: foundrymodel.OntologyGenerationSucceeded, Input: foundrymodel.OntologyGenerationInput{Revise: revise}, ReviseResult: json.RawMessage(result)},
				&foundrymodel.OntologyGeneration{ID: "queued", OntologyID: "ont-1", Status: foundrymodel.OntologyGenerationQueued, Input: foundrymodel.OntologyGenerationInput{Revise: revise}})
			gen, err := s.GetOntologyGeneration(ctx, &foundryv1.GetOntologyGenerationRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "revised"})
			if err != nil {
				t.Fatal(err)
			}
			rev := gen.GetGeneration().GetRevision()
			if tc.evidence && (rev.GetResultJson() != result || rev.GetResultWithheld()) || !tc.evidence && (rev.GetResultJson() != "" || !rev.GetResultWithheld()) {
				t.Fatalf("revision = %v", rev)
			}
			queued, err := s.GetOntologyGeneration(ctx, &foundryv1.GetOntologyGenerationRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "queued"})
			if err != nil || queued.GetGeneration().GetRevision().GetResult() != nil {
				t.Fatalf("a revision with no result yet has neither: %v %v", queued, err)
			}

			db.versions = append(db.versions,
				&foundrymodel.OntologyVersion{ID: "revised", OntologyID: "ont-1", Format: foundrymodel.OntologyFormatAgentBundle, Source: foundrymodel.OntologySourceManual, Definition: draft("parent")},
				&foundrymodel.OntologyVersion{ID: "generated", OntologyID: "ont-1", Format: foundrymodel.OntologyFormatAgentBundle, Source: foundrymodel.OntologySourceGenerated, Definition: draft("")})
			items := func(v *foundryv1.OntologyVersion) map[string]*structpb.Value {
				return v.GetDefinition().GetFields()["draft"].GetStructValue().GetFields()["draft"].GetStructValue().GetFields()["items"].GetStructValue().GetFields()
			}
			reads := map[string]func() (*foundryv1.OntologyVersionResponse, error){
				"get": func() (*foundryv1.OntologyVersionResponse, error) {
					return s.GetOntologyVersion(ctx, &foundryv1.GetOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "revised"})
				},
				"set current": func() (*foundryv1.OntologyVersionResponse, error) {
					return s.SetCurrentOntologyVersion(ctx, &foundryv1.SetCurrentOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", VersionId: "revised"})
				},
				"get current": func() (*foundryv1.OntologyVersionResponse, error) {
					return s.GetCurrentOntologyVersion(ctx, &foundryv1.GetCurrentOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
				},
			}
			for _, read := range []string{"get", "set current", "get current"} {
				got, err := reads[read]()
				if err != nil {
					t.Fatalf("%s: %v", read, err)
				}
				v := items(got.GetVersion())
				if v["rule.a"].GetStructValue().GetFields()["verdict"].GetStringValue() != "rule" || v["type.Order"] == nil {
					t.Fatalf("%s: the draft keeps its items and verdicts: %v", read, v)
				}
				if kept := v["rule.a"].GetStructValue().GetFields()["card"] != nil; kept != tc.evidence || got.GetVersion().GetEvidenceWithheld() == tc.evidence {
					t.Fatalf("%s: card kept %v, withheld %v", read, kept, got.GetVersion().GetEvidenceWithheld())
				}
			}
			generated, err := s.GetOntologyVersion(ctx, &foundryv1.GetOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: "generated"})
			if err != nil || items(generated.GetVersion())["rule.a"].GetStructValue().GetFields()["card"] == nil || generated.GetVersion().GetEvidenceWithheld() {
				t.Fatalf("a generated draft's cards measure synced rows, not the load: %v %v", generated, err)
			}
		})
	}
}
