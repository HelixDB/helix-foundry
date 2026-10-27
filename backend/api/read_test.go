package api

import (
	"context"
	"encoding/json"
	"testing"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundryreadv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundryread/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeFoundryRead stands for foundry-read: it keeps each request and answers
// result, or err.
type fakeFoundryRead struct {
	// FoundryReadClient's Title panics: reads never title.
	foundryreadv1.FoundryReadClient
	requests []*foundryreadv1.ReadRequest
	result   string
	err      error
}

func (f *fakeFoundryRead) Read(_ context.Context, in *foundryreadv1.ReadRequest, _ ...grpc.CallOption) (*foundryreadv1.ReadResponse, error) {
	f.requests = append(f.requests, in)
	if f.err != nil {
		return nil, f.err
	}
	return &foundryreadv1.ReadResponse{ResultJson: f.result}, nil
}

func TestReadFoundryGraph(t *testing.T) {
	ctx := context.Background()
	const reads = `[{"entity":{"type":"Customer","id":"c-1"}}]`
	setup := func(t *testing.T, loadStatus foundrymodel.FoundryLoadStatus) (*Service, *foundryStore, *fakeFoundryRead) {
		t.Helper()
		s, store, _ := foundryService(t)
		def, _ := json.Marshal(map[string]any{"format": "agent_bundle", "status": "done", "ontology_toml": "version = \"v3\"\n", "mappings_toml": ""})
		store.versions = append(store.versions, &foundrymodel.OntologyVersion{
			ID: "v-3", WorkspaceID: "ws-1", OntologyID: "ont-1", Version: 3, Format: foundrymodel.OntologyFormatAgentBundle, Definition: def,
		})
		store.loads = append(store.loads,
			&foundrymodel.FoundryLoad{ID: "load-1", WorkspaceID: "ws-1", OntologyID: "ont-1", VersionID: "v-3", Version: 3, Status: loadStatus},
			&foundrymodel.FoundryLoad{ID: "load-2", WorkspaceID: "ws-2", OntologyID: "ont-2", VersionID: "v-9", Version: 9, Status: foundrymodel.FoundryLoadReady})
		reader := &fakeFoundryRead{result: `{"as_of":"2026-10-19","results":[{"ok":{}}]}`}
		s.deps.FoundryRead = reader
		return s, store, reader
	}
	read := func(s *Service, workspace, ontology, load, reads string) (*foundryv1.ReadFoundryGraphResponse, error) {
		return s.ReadFoundryGraph(ctx, &foundryv1.ReadFoundryGraphRequest{WorkspaceId: workspace, OntologyId: ontology, LoadId: load, ReadsJson: reads})
	}

	// A load of the ontology is read with its own version's rules, in any state it holds data in.
	t.Run("reads a load with its version", func(t *testing.T) {
		for _, loadStatus := range []foundrymodel.FoundryLoadStatus{foundrymodel.FoundryLoadReady, foundrymodel.FoundryLoadPartial, foundrymodel.FoundryLoadFailed, foundrymodel.FoundryLoadWriting} {
			s, _, reader := setup(t, loadStatus)
			resp, err := read(s, "ws-1", "ont-1", "load-1", reads)
			if err != nil {
				t.Fatalf("%s: %v", loadStatus, err)
			}
			if resp.GetResultJson() != reader.result {
				t.Fatalf("%s: result = %s", loadStatus, resp.GetResultJson())
			}
			got := reader.requests[0]
			if got.GetWorkspaceId() != "ws-1" || got.GetOntologyId() != "ont-1" || got.GetLoadId() != "load-1" ||
				got.GetOntologyVersion() != "foundry-v3" || got.GetOntologyToml() != "version = \"v3\"\n" || got.GetReadsJson() != reads {
				t.Fatalf("%s: foundry-read got %+v", loadStatus, got)
			}
		}
	})

	// Only the ontology's own loads, in its own workspace, are read; a deleted one is gone.
	t.Run("reads only the ontology's loads", func(t *testing.T) {
		s, _, reader := setup(t, foundrymodel.FoundryLoadDeleted)
		for name, ids := range map[string][3]string{
			"another ontology's load":   {"ws-1", "ont-1", "load-2"},
			"through another workspace": {"ws-2", "ont-1", "load-1"},
			"a missing load":            {"ws-1", "ont-1", "load-x"},
			"a deleted load":            {"ws-1", "ont-1", "load-1"},
			"a missing ontology's load": {"ws-1", "ont-x", "load-1"},
		} {
			if _, err := read(s, ids[0], ids[1], ids[2], reads); status.Code(err) != codes.NotFound {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if len(reader.requests) != 0 {
			t.Fatalf("foundry-read was called %d times", len(reader.requests))
		}
	})

	// A load being deleted no longer serves reads: its marker says so.
	t.Run("refuses a load being deleted", func(t *testing.T) {
		for _, loadStatus := range []foundrymodel.FoundryLoadStatus{foundrymodel.FoundryLoadDeleting, foundrymodel.FoundryLoadDeleteFailed} {
			s, _, reader := setup(t, loadStatus)
			if _, err := read(s, "ws-1", "ont-1", "load-1", reads); status.Code(err) != codes.FailedPrecondition || len(reader.requests) != 0 {
				t.Fatalf("%s: %v after %d calls", loadStatus, err, len(reader.requests))
			}
		}
	})

	t.Run("refuses bad requests", func(t *testing.T) {
		s, _, reader := setup(t, foundrymodel.FoundryLoadReady)
		for name, req := range map[string][4]string{
			"no load":        {"ws-1", "ont-1", "", reads},
			"no reads":       {"ws-1", "ont-1", "load-1", ""},
			"an empty list":  {"ws-1", "ont-1", "load-1", `[]`},
			"an unknown one": {"ws-1", "ont-1", "load-1", `[{"drop":{}}]`},
			"no ontology":    {"ws-1", "", "load-1", reads},
		} {
			if _, err := read(s, req[0], req[1], req[2], req[3]); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if len(reader.requests) != 0 {
			t.Fatalf("foundry-read was called %d times", len(reader.requests))
		}
	})

	// foundry-read's refusals, its per-workspace limit among them, reach the caller as they are.
	t.Run("relays foundry-read's statuses", func(t *testing.T) {
		s, _, reader := setup(t, foundrymodel.FoundryLoadReady)
		reader.err = status.Error(codes.ResourceExhausted, "the workspace already runs 4 graph reads")
		if _, err := read(s, "ws-1", "ont-1", "load-1", reads); status.Code(err) != codes.ResourceExhausted ||
			status.Convert(err).Message() != "the workspace already runs 4 graph reads" {
			t.Fatalf("over the limit: %v", err)
		}
	})

	t.Run("is unavailable without foundry-read", func(t *testing.T) {
		s, _, _ := setup(t, foundrymodel.FoundryLoadReady)
		s.deps.FoundryRead = nil
		if _, err := read(s, "ws-1", "ont-1", "load-1", reads); status.Code(err) != codes.Unavailable {
			t.Fatalf("without foundry-read: %v", err)
		}
	})
}
