package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/ontologygen"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundryreadv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundryread/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/sample"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
)

// generationStore adds the generation queue and document sets to the ontology store.
type generationStore struct {
	*ontologyStore
	generations []*foundrymodel.OntologyGeneration
	// sets are the live document sets, oldest first.
	sets []*foundrymodel.FoundryDocumentSet
	// activeErr, when set, is what GetActiveOntologyGeneration returns.
	activeErr error
	// setsErr, when set, is what ListFoundryDocumentSets returns.
	setsErr error
	// perHour is the workspace cap the last enqueue was given.
	perHour int
	// enqueueErr, when set, is what EnqueueOntologyGeneration returns.
	enqueueErr error
	// titleErr, when set, is what SetOntologyGenerationTitle returns.
	titleErr error
	// files are the workspace's table files whose read succeeded, and text
	// the documents each set holds that are read as text.
	files []*foundrymodel.FoundryDocument
	text  map[string]int
}

func (s *generationStore) ListReadFoundryTableDocuments(_ context.Context, workspaceID string, setIDs []string) ([]*foundrymodel.FoundryDocument, error) {
	var out []*foundrymodel.FoundryDocument
	for _, f := range s.files {
		if f.WorkspaceID == workspaceID && slices.Contains(setIDs, f.SetID) {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *generationStore) CountReadableFoundryTextDocuments(_ context.Context, _ string, setIDs []string) (int, error) {
	n := 0
	for _, id := range setIDs {
		n += s.text[id]
	}
	return n, nil
}

func (s *generationStore) ListFoundryDocumentSets(_ context.Context, workspaceID string) ([]*foundrymodel.FoundryDocumentSet, error) {
	if s.setsErr != nil {
		return nil, s.setsErr
	}
	var out []*foundrymodel.FoundryDocumentSet
	for _, set := range s.sets {
		if set.WorkspaceID == workspaceID {
			out = append(out, set)
		}
	}
	return out, nil
}

func (s *generationStore) live(ontologyID string) *foundrymodel.OntologyGeneration {
	for _, g := range s.generations {
		if g.OntologyID == ontologyID && (g.Status == foundrymodel.OntologyGenerationQueued || g.Status == foundrymodel.OntologyGenerationRunning) {
			return g
		}
	}
	return nil
}

func (s *generationStore) GetActiveOntologyGeneration(_ context.Context, ontologyID string) (*foundrymodel.OntologyGeneration, error) {
	if s.activeErr != nil {
		return nil, s.activeErr
	}
	if g := s.live(ontologyID); g != nil {
		return g, nil
	}
	return nil, foundrystore.ErrNotFound
}

func (s *generationStore) EnqueueOntologyGeneration(_ context.Context, gen *foundrymodel.OntologyGeneration, perHour int) error {
	s.perHour = perHour
	if s.enqueueErr != nil {
		return s.enqueueErr
	}
	if s.live(gen.OntologyID) != nil {
		return foundrystore.ErrConflict
	}
	copied := *gen
	copied.Status, copied.CreatedAt, copied.NextAttemptAt = foundrymodel.OntologyGenerationQueued, time.Now(), time.Now()
	s.generations = append(s.generations, &copied)
	return nil
}

func (s *generationStore) SetOntologyGenerationTitle(_ context.Context, id, title string) error {
	if s.titleErr != nil {
		return s.titleErr
	}
	for _, g := range s.generations {
		if g.ID == id {
			g.Title = &title
			return nil
		}
	}
	return foundrystore.ErrNotFound
}

func (s *generationStore) GetOntologyGeneration(_ context.Context, ontologyID, id string) (*foundrymodel.OntologyGeneration, error) {
	for _, g := range s.generations {
		if g.ID == id && g.OntologyID == ontologyID {
			return g, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *generationStore) ListOntologyGenerations(_ context.Context, ontologyID string, limit int) ([]*foundrymodel.OntologyGeneration, error) {
	var out []*foundrymodel.OntologyGeneration
	for i := len(s.generations) - 1; i >= 0 && len(out) < limit; i-- {
		if s.generations[i].OntologyID == ontologyID {
			out = append(out, s.generations[i])
		}
	}
	return out, nil
}

// generationService has one Stripe connector with a synced customers table
// and allows maxTables tables per generation.
func generationService(t *testing.T, maxTables int) (*Service, *generationStore) {
	t.Helper()
	s, ontologies := ontologyService(t)
	store := &generationStore{ontologyStore: ontologies}
	cfg := &config.Config{}
	cfg.Foundry.Ontology.MaxTables = maxTables
	cfg.Foundry.Ontology.PerHour = 10
	s.deps.DB, s.deps.Cfg = store, cfg
	s.deps.GenerationTitles = ontologygen.NewTitler(nil, nil)
	return s, store
}

// titleWriter is foundry-read's Title, answering text, or err; calls counts
// the requests, last is the latest.
type titleWriter struct {
	foundryreadv1.FoundryReadClient
	text  string
	err   error
	calls atomic.Int32
	last  atomic.Pointer[foundryreadv1.TitleRequest]
}

func (w *titleWriter) Title(_ context.Context, in *foundryreadv1.TitleRequest, _ ...grpc.CallOption) (*foundryreadv1.TitleResponse, error) {
	w.calls.Add(1)
	w.last.Store(in)
	if w.err != nil {
		return nil, w.err
	}
	return &foundryreadv1.TitleResponse{Text: w.text}, nil
}

// titleModel titles through a foundry-read that answers text, or err.
func titleModel(text string, err error) (*ontologygen.Titler, *titleWriter) {
	w := &titleWriter{text: text, err: err}
	return ontologygen.NewTitler(w, slog.New(slog.DiscardHandler)), w
}

func TestFoundryOntologyGenerationRPCs(t *testing.T) {
	ctx := context.Background()
	// generate drafts as person, signed in to the site.
	generate := func(s *Service, req *foundryv1.GenerateOntologyRequest) (*foundryv1.OntologyGeneration, error) {
		resp, err := s.GenerateOntology(ports.WithActor(ctx, person), req)
		return resp.GetGeneration(), err
	}

	// The generation records what it covers, its ontology, and what it builds on.
	t.Run("queues a generation over every synced table", func(t *testing.T) {
		s, _ := generationService(t, 50)
		base, err := s.CreateOntologyVersion(ctx, &foundryv1.CreateOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Definition: definition(t, "customers")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetCurrentOntologyVersion(ctx, &foundryv1.SetCurrentOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", VersionId: base.GetVersion().GetId()}); err != nil {
			t.Fatal(err)
		}
		gen, err := generate(s, &foundryv1.GenerateOntologyRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", Goal: "customer billing health",
		})
		if err != nil {
			t.Fatal(err)
		}
		if gen.GetStatus() != foundryv1.OntologyGenerationStatus_ONTOLOGY_GENERATION_STATUS_QUEUED || gen.GetGoal() != "customer billing health" ||
			len(gen.GetTables()) != 1 || gen.GetTables()[0].GetTable() != "customers" ||
			gen.GetBaseVersionId() != base.GetVersion().GetId() || gen.GetRequestedByUserId() != "user-1" || gen.GetOntologyId() != "ont-1" {
			t.Fatalf("generation = %+v", gen)
		}
		got, err := s.GetOntologyGeneration(ctx, &foundryv1.GetOntologyGenerationRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: gen.GetId()})
		if err != nil || got.GetGeneration().GetId() != gen.GetId() {
			t.Fatalf("GetOntologyGeneration = (%+v, %v)", got, err)
		}
	})
	// The draft reads the sets the user picks and no other: picking none is
	// how a draft reads no document, not a short way of picking every one.
	t.Run("covers no document set unless chosen", func(t *testing.T) {
		s, store := generationService(t, 50)
		store.sets = []*foundrymodel.FoundryDocumentSet{
			{ID: "set-general", WorkspaceID: "ws-1", Name: "General"},
			{ID: "set-contracts", WorkspaceID: "ws-1", Name: "Contracts"},
		}
		gen, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Goal: "contracts with our customers"})
		if err != nil {
			t.Fatal(err)
		}
		if got := store.generations[0].Input.DocumentSetIDs; len(got) != 0 || len(gen.GetDocumentSetIds()) != 0 {
			t.Fatalf("document sets = %v, shown %v, want none", got, gen.GetDocumentSetIds())
		}
	})
	// The user picks among the workspace's sets; one it doesn't have is refused.
	t.Run("covers the chosen document sets of the workspace", func(t *testing.T) {
		s, store := generationService(t, 50)
		store.sets = []*foundrymodel.FoundryDocumentSet{
			{ID: "set-general", WorkspaceID: "ws-1", Name: "General"},
			{ID: "set-contracts", WorkspaceID: "ws-1", Name: "Contracts"},
			{ID: "set-theirs", WorkspaceID: "ws-2", Name: "General"},
		}
		for _, chosen := range [][]string{{"set-contracts", "set-gone"}, {"set-theirs"}} {
			_, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", DocumentSetIds: chosen})
			if status.Code(err) != codes.NotFound || len(store.generations) != 0 {
				t.Fatalf("sets %v: GenerateOntology error = %v, want NotFound and nothing queued", chosen, err)
			}
		}
		gen, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
			DocumentSetIds: []string{"set-contracts", "set-general"}})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"set-general", "set-contracts"}
		if got := store.generations[0].Input.DocumentSetIDs; !slices.Equal(got, want) || !slices.Equal(gen.GetDocumentSetIds(), want) {
			t.Fatalf("document sets = %v, shown %v, want %v", got, gen.GetDocumentSetIds(), want)
		}
	})
	// A workspace may hold more sets than one draft reads, from its
	// ontologies' sets before release 1; a generation picks at most 20.
	t.Run("reads at most maxDocumentSets sets", func(t *testing.T) {
		s, store := generationService(t, 50)
		var all []string
		for i := range maxDocumentSets + 1 {
			id := fmt.Sprintf("set-%02d", i)
			store.sets = append(store.sets, &foundrymodel.FoundryDocumentSet{ID: id, WorkspaceID: "ws-1", Name: id})
			all = append(all, id)
		}
		_, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", DocumentSetIds: all})
		if status.Code(err) != codes.InvalidArgument || len(store.generations) != 0 {
			t.Fatalf("%d sets: %v", len(all), err)
		}
		if _, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
			DocumentSetIds: append(all[:maxDocumentSets:maxDocumentSets], all[0])}); err != nil {
			t.Fatalf("%d sets, one twice: %v", maxDocumentSets, err)
		}
	})
	t.Run("fails when the sets can't be read", func(t *testing.T) {
		s, store := generationService(t, 50)
		store.setsErr = errors.New("connection reset")
		if _, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}); status.Code(err) != codes.Internal {
			t.Fatalf("GenerateOntology error = %v, want Internal", err)
		}
	})
	// Lists show the title; the goal stays whole for the detail.
	t.Run("titles the generation from its goal", func(t *testing.T) {
		s, store := generationService(t, 50)
		titles, calls := titleModel("Customer billing health", nil)
		s.deps.GenerationTitles = titles
		goal := "Show which customers pay late and why. Include refunds."
		gen, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Goal: goal})
		if err != nil {
			t.Fatal(err)
		}
		if gen.GetTitle() != "Customer billing health" || gen.GetGoal() != goal || *store.generations[0].Title != "Customer billing health" || calls.calls.Load() != 1 {
			t.Fatalf("generation = %+v after %d title requests", gen, calls.calls.Load())
		}
		// foundry-read titles it on the workspace's own AI setting.
		if req := calls.last.Load(); req.GetWorkspaceId() != "ws-1" || req.GetGenerationId() != store.generations[0].ID || req.GetGoal() != goal {
			t.Fatalf("title request = %v", req)
		}
		list, err := s.ListOntologyGenerations(ctx, &foundryv1.ListOntologyGenerationsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || list.GetGenerations()[0].GetTitle() != "Customer billing health" {
			t.Fatalf("ListOntologyGenerations = (%+v, %v)", list, err)
		}
	})
	// A failed title never fails or blocks the generation.
	t.Run("titles with the goal's first sentence when the model fails", func(t *testing.T) {
		s, store := generationService(t, 50)
		titles, calls := titleModel("", status.Error(codes.FailedPrecondition, "the title model failed: unsupported"))
		s.deps.GenerationTitles = titles
		gen, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Goal: "Show which customers pay late and why. Include refunds."})
		if err != nil {
			t.Fatal(err)
		}
		if gen.GetTitle() != "Show which customers pay late and why" || *store.generations[0].Title != gen.GetTitle() || calls.calls.Load() != 1 {
			t.Fatalf("title = %q after %d title requests", gen.GetTitle(), calls.calls.Load())
		}
	})
	// The generation is already queued, so a title that can't be stored is left off.
	t.Run("keeps the generation when its title can't be stored", func(t *testing.T) {
		s, store := generationService(t, 50)
		store.titleErr = errors.New("connection reset")
		gen, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Goal: "Map suppliers to parts"})
		if err != nil || gen.GetTitle() != "" || gen.GetGoal() != "Map suppliers to parts" || len(store.generations) != 1 {
			t.Fatalf("GenerateOntology = (%+v, %v)", gen, err)
		}
	})
	t.Run("titles with the goal's first sentence without foundry-read", func(t *testing.T) {
		s, _ := generationService(t, 50)
		gen, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Goal: "Map suppliers to parts\nand their lead times"})
		if err != nil || gen.GetTitle() != "Map suppliers to parts" {
			t.Fatalf("GenerateOntology = (%+v, %v)", gen, err)
		}
	})
	t.Run("has no title without a goal", func(t *testing.T) {
		s, store := generationService(t, 50)
		titles, calls := titleModel("", nil)
		s.deps.GenerationTitles = titles
		gen, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || gen.GetTitle() != "" || store.generations[0].Title != nil || calls.calls.Load() != 0 {
			t.Fatalf("GenerateOntology = (%+v, %v) after %d title requests", gen, err, calls.calls.Load())
		}
	})
	// A double submit is refused before it waits on, and pays for, a title.
	t.Run("refuses a second live generation of an ontology", func(t *testing.T) {
		s, store := generationService(t, 50)
		titles, calls := titleModel("Customer billing health", nil)
		s.deps.GenerationTitles = titles
		req := &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Goal: "customer billing health"}
		if _, err := generate(s, req); err != nil {
			t.Fatal(err)
		}
		if _, err := generate(s, req); status.Code(err) != codes.AlreadyExists || calls.calls.Load() != 1 || len(store.generations) != 1 {
			t.Fatalf("second GenerateOntology error = %v after %d title requests, want AlreadyExists after 1", err, calls.calls.Load())
		}
		store.ontologies["ont-3"] = &foundrymodel.FoundryOntology{ID: "ont-3", WorkspaceID: "ws-1", Name: "Support", TenantID: "tenant-3"}
		if _, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-3"}); err != nil {
			t.Fatalf("another ontology of the workspace drafts on its own: %v", err)
		}
	})
	// Two submits can both find no live generation; the insert still refuses one.
	t.Run("refuses a live generation that races in", func(t *testing.T) {
		s, store := generationService(t, 50)
		if _, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}); err != nil {
			t.Fatal(err)
		}
		store.activeErr = foundrystore.ErrNotFound
		if _, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}); status.Code(err) != codes.AlreadyExists {
			t.Fatalf("racing GenerateOntology error = %v, want AlreadyExists", err)
		}
	})
	// The provider counts the workspace's hour under its lock; the refusal
	// names the cap, and a refused request never pays for a title.
	t.Run("refuses a generation past the workspace's hourly cap", func(t *testing.T) {
		s, store := generationService(t, 50)
		titles, calls := titleModel("", nil)
		s.deps.GenerationTitles = titles
		store.enqueueErr = foundrystore.ErrLimitExceeded
		_, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Goal: "customer billing health"})
		if status.Code(err) != codes.ResourceExhausted || !strings.Contains(status.Convert(err).Message(), "10 ontology generations an hour") {
			t.Fatalf("GenerateOntology error = %v, want ResourceExhausted naming the cap", err)
		}
		if calls.calls.Load() != 0 {
			t.Fatalf("a refused generation made %d title requests", calls.calls.Load())
		}
		if store.perHour != 10 {
			t.Fatalf("enqueue was given a cap of %d, want foundry.ontology.per_hour 10", store.perHour)
		}
	})
	// The ontology is read live first, then its document sets and tables; a
	// delete of it, or of its workspace, in between leaves nothing to queue a
	// generation of.
	t.Run("does not find an ontology or workspace that is gone", func(t *testing.T) {
		s, store := generationService(t, 50)
		store.enqueueErr = foundrystore.ErrNotFound
		_, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if status.Code(err) != codes.NotFound || status.Convert(err).Message() != "ontology not found" || len(store.generations) != 0 {
			t.Fatalf("GenerateOntology error = %v, want NotFound and nothing queued", err)
		}
	})
	t.Run("fails when the live generation cannot be read", func(t *testing.T) {
		s, store := generationService(t, 50)
		store.activeErr = errors.New("connection reset")
		if _, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}); status.Code(err) != codes.Internal || len(store.generations) != 0 {
			t.Fatalf("GenerateOntology error = %v, want Internal", err)
		}
	})
	// A generation needs something to read, of either kind: with no synced
	// table, no table file read and no text in the sets chosen, there is
	// nothing, and the refusal says what would do.
	t.Run("needs a table, a table file or text", func(t *testing.T) {
		s, store := generationService(t, 50)
		store.tables = nil
		store.sets = []*foundrymodel.FoundryDocumentSet{{ID: "set-empty", WorkspaceID: "ws-1", Name: "Empty"}}
		_, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", DocumentSetIds: []string{"set-empty"}})
		if status.Code(err) != codes.FailedPrecondition || !strings.HasPrefix(status.Convert(err).Message(), "connect your data or upload documents first") {
			t.Fatalf("GenerateOntology error = %v, want FailedPrecondition", err)
		}
	})
	// Someone may have only documents: a set's table files are tables of
	// their own, recorded with the set they were in, and need no connector.
	t.Run("drafts from table files alone", func(t *testing.T) {
		s, store := generationService(t, 50)
		store.tables = nil
		store.sets = []*foundrymodel.FoundryDocumentSet{{ID: "set-files", WorkspaceID: "ws-1", Name: "Exports"}}
		store.files = []*foundrymodel.FoundryDocument{{ID: "doc-csv", WorkspaceID: "ws-1", SetID: "set-files"}, {ID: "doc-other", WorkspaceID: "ws-1", SetID: "set-x"}}
		gen, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", DocumentSetIds: []string{"set-files"}})
		if err != nil {
			t.Fatal(err)
		}
		want := []foundrymodel.OntologyGenerationDocument{{ID: "doc-csv", SetID: "set-files"}}
		if got := store.generations[0].Input.Documents; !slices.Equal(got, want) || len(gen.GetTables()) != 0 ||
			len(gen.GetDocuments()) != 1 || gen.GetDocuments()[0].GetId() != "doc-csv" || gen.GetDocuments()[0].GetSetId() != "set-files" {
			t.Fatalf("documents = %v, shown %v", got, gen.GetDocuments())
		}
	})
	// The sample company drafts from its database's connector, each of its
	// tables synced, and its documents' set, titled as its draft is, with no
	// model called.
	acme := func(store *generationStore, except string) {
		store.sets = []*foundrymodel.FoundryDocumentSet{{ID: "set-k", WorkspaceID: "ws-1", Name: "Acme Corp sample"}, {ID: "set-x", WorkspaceID: "ws-1", Name: "Other"}}
		store.rows["conn-k"] = &foundrymodel.Connector{ID: "conn-k", WorkspaceID: "ws-1", Kind: "postgres", Name: "Acme Corp", Config: []byte(`{}`)}
		for _, name := range sample.Tables {
			if name != except {
				store.tables = append(store.tables, &foundrymodel.ConnectorTable{WorkspaceID: "ws-1", ConnectorID: "conn-k", TableName: name, SyncID: "sync-k"})
			}
		}
	}
	sampleRequest := func() *foundryv1.GenerateOntologyRequest {
		return &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Goal: "Acme Corp's customers",
			ConnectorIds: []string{"conn-k"}, DocumentSetIds: []string{"set-k"}, Sample: sample.Acme}
	}
	t.Run("queues the sample from its database's tables, titled with no model", func(t *testing.T) {
		s, store := generationService(t, 50)
		titles, calls := titleModel("A model's title", nil)
		s.deps.GenerationTitles = titles
		acme(store, "")
		gen, err := generate(s, sampleRequest())
		if err != nil {
			t.Fatal(err)
		}
		if gen.GetSample() != sample.Acme || store.generations[0].Input.Sample != sample.Acme || len(gen.GetTables()) != len(sample.Tables) ||
			gen.GetTitle() != sample.Title || calls.calls.Load() != 0 {
			t.Fatalf("generation = %+v after %d title requests", gen, calls.calls.Load())
		}
	})
	t.Run("refuses another sample, or the sample beside other sources", func(t *testing.T) {
		s, store := generationService(t, 50)
		acme(store, "")
		other := sampleRequest()
		other.Sample = "globex"
		every := sampleRequest()
		every.ConnectorIds = nil
		sets := sampleRequest()
		sets.DocumentSetIds = []string{"set-k", "set-x"}
		for _, req := range []*foundryv1.GenerateOntologyRequest{other, every, sets} {
			if _, err := generate(s, req); status.Code(err) != codes.InvalidArgument {
				t.Errorf("GenerateOntology(%v) = %v, want InvalidArgument", req, err)
			}
		}
		if len(store.generations) != 0 {
			t.Fatalf("queued %d generations", len(store.generations))
		}
	})
	t.Run("waits for each of the sample's tables to be synced", func(t *testing.T) {
		s, store := generationService(t, 50)
		acme(store, "invoices")
		_, err := generate(s, sampleRequest())
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "invoices") || len(store.generations) != 0 {
			t.Fatalf("GenerateOntology = %v, %d queued", err, len(store.generations))
		}
	})
	// A set holding text is enough on its own: the draft routes it to
	// extraction with no table at all.
	t.Run("drafts from text alone", func(t *testing.T) {
		s, store := generationService(t, 50)
		store.tables = nil
		store.sets = []*foundrymodel.FoundryDocumentSet{{ID: "set-contracts", WorkspaceID: "ws-1", Name: "Contracts"}}
		store.text = map[string]int{"set-contracts": 2}
		if _, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", DocumentSetIds: []string{"set-contracts"}}); err != nil {
			t.Fatal(err)
		}
		if in := store.generations[0].Input; len(in.Tables) != 0 || len(in.Documents) != 0 || !slices.Equal(in.DocumentSetIDs, []string{"set-contracts"}) {
			t.Fatalf("input = %+v", in)
		}
	})
	// A workspace with connectors may still draft from documents alone; no
	// connector ids must come with it, which would contradict it.
	t.Run("reads no connector when asked", func(t *testing.T) {
		s, store := generationService(t, 50)
		store.sets = []*foundrymodel.FoundryDocumentSet{{ID: "set-contracts", WorkspaceID: "ws-1", Name: "Contracts"}}
		store.text = map[string]int{"set-contracts": 1}
		req := &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", DocumentSetIds: []string{"set-contracts"}, NoConnectors: true}
		if _, err := generate(s, req); err != nil {
			t.Fatal(err)
		}
		if in := store.generations[0].Input; len(in.Tables) != 0 {
			t.Fatalf("read %v with no connectors", in.Tables)
		}
		req.ConnectorIds = []string{"conn-1"}
		if _, err := generate(s, req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("connector ids with no_connectors: %v", err)
		}
		s, _ = generationService(t, 50)
		req.ConnectorIds, req.DocumentSetIds = nil, nil
		if _, err := generate(s, req); status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("no connector and no set: %v", err)
		}
	})
	// Synced tables and table files count together toward the deployment's cap,
	// and the refusal says which to choose fewer of.
	t.Run("caps the tables and table files one generation covers", func(t *testing.T) {
		s, store := generationService(t, 1)
		store.tables = append(store.tables, &foundrymodel.ConnectorTable{ConnectorID: "conn-1", WorkspaceID: "ws-1", TableName: "invoices", Columns: []byte(`[]`)})
		if _, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("GenerateOntology error = %v, want InvalidArgument", err)
		}
		s, store = generationService(t, 1)
		store.sets = []*foundrymodel.FoundryDocumentSet{{ID: "set-files", WorkspaceID: "ws-1", Name: "Exports"}}
		store.files = []*foundrymodel.FoundryDocument{{ID: "doc-csv", WorkspaceID: "ws-1", SetID: "set-files"}}
		_, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", DocumentSetIds: []string{"set-files"}})
		if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() !=
			"these connectors and sets have 2 tables (1 synced, 1 files) and one generation covers at most 1; choose fewer connectors or sets" {
			t.Fatalf("a table and a file past a cap of 1: %v", err)
		}
	})
	t.Run("rejects a connector of another workspace", func(t *testing.T) {
		s, _ := generationService(t, 50)
		if _, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-2", OntologyId: "ont-1"}); status.Code(err) != codes.NotFound {
			t.Fatalf("another workspace's ontology: GenerateOntology error = %v, want NotFound", err)
		}
		_, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-2", OntologyId: "ont-2", ConnectorIds: []string{"conn-1"}})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("GenerateOntology error = %v, want NotFound", err)
		}
	})
	t.Run("does not find another workspace's generation", func(t *testing.T) {
		s, _ := generationService(t, 50)
		gen, err := generate(s, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.GetOntologyGeneration(ctx, &foundryv1.GetOntologyGenerationRequest{WorkspaceId: "ws-2", OntologyId: "ont-2", Id: gen.GetId()})
		if status.Code(err) != codes.NotFound {
			t.Fatalf("GetOntologyGeneration error = %v, want NotFound", err)
		}
		list, err := s.ListOntologyGenerations(ctx, &foundryv1.ListOntologyGenerationsRequest{WorkspaceId: "ws-2", OntologyId: "ont-2"})
		if err != nil || len(list.GetGenerations()) != 0 {
			t.Fatalf("ListOntologyGenerations(ws-2) = (%+v, %v)", list, err)
		}
	})
}

// load_when_ready asks for an ontology's first draft to be made current and
// loaded once stored, which writes its database: only a caller who may write
// it asks, only while the ontology has no current version, and never for the
// sample.
func TestGenerateOntologyLoadWhenReady(t *testing.T) {
	ctx := context.Background()
	// writer may write the databases of ws-1's project proj-1, ont-1's.
	writer := ports.Actor{Source: ports.SourceUser, UserID: "user-1", WorkspaceID: "ws-1",
		QueryRead: ports.AllProjects(), QueryWrite: ports.SelectedProjects([]string{"proj-1"})}
	generate := func(s *Service, as ports.Actor, req *foundryv1.GenerateOntologyRequest) (*foundryv1.OntologyGeneration, error) {
		resp, err := s.GenerateOntology(ports.WithActor(ctx, as), req)
		return resp.GetGeneration(), err
	}
	first := func() *foundryv1.GenerateOntologyRequest {
		return &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Goal: "customer billing health", LoadWhenReady: true}
	}

	t.Run("records and echoes it for a first draft", func(t *testing.T) {
		s, store := generationService(t, 50)
		gen, err := generate(s, writer, first())
		if err != nil {
			t.Fatal(err)
		}
		if !gen.GetLoadWhenReady() || !store.generations[0].Input.LoadWhenReady || gen.GetBaseVersionId() != "" {
			t.Fatalf("generation = %+v, input = %+v", gen, store.generations[0].Input)
		}
		got, err := s.GetOntologyGeneration(ctx, &foundryv1.GetOntologyGenerationRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: gen.GetId()})
		if err != nil || !got.GetGeneration().GetLoadWhenReady() {
			t.Fatalf("GetOntologyGeneration = (%+v, %v)", got, err)
		}
		list, err := s.ListOntologyGenerations(ctx, &foundryv1.ListOntologyGenerationsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
		if err != nil || !list.GetGenerations()[0].GetLoadWhenReady() {
			t.Fatalf("ListOntologyGenerations = (%+v, %v)", list, err)
		}
	})
	// Unset, nothing changes, and it is stored as absent.
	t.Run("leaves a draft without it for review", func(t *testing.T) {
		s, store := generationService(t, 50)
		req := first()
		req.LoadWhenReady = false
		gen, err := generate(s, person, req)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(store.generations[0].Input)
		if err != nil {
			t.Fatal(err)
		}
		if gen.GetLoadWhenReady() || strings.Contains(string(raw), "load_when_ready") {
			t.Fatalf("generation = %+v, input = %s", gen, raw)
		}
	})
	// The front end authorizes generating on managing ontologies alone, so
	// the database's write is checked here: a caller with no write scope, one
	// of another project, or one of another workspace's, is refused.
	t.Run("needs a caller who may write the ontology's database", func(t *testing.T) {
		s, store := generationService(t, 50)
		otherProject := writer
		otherProject.QueryWrite = ports.SelectedProjects([]string{"proj-2"})
		otherWorkspace := writer
		otherWorkspace.WorkspaceID, otherWorkspace.QueryWrite = "ws-2", ports.AllProjects()
		for name, as := range map[string]ports.Actor{"no scope": person, "another project": otherProject, "another workspace": otherWorkspace} {
			_, err := generate(s, as, first())
			if status.Code(err) != codes.PermissionDenied || !strings.Contains(status.Convert(err).Message(), "load_when_ready") {
				t.Errorf("%s: GenerateOntology = %v, want PermissionDenied", name, err)
			}
		}
		if len(store.generations) != 0 {
			t.Fatalf("queued %d generations", len(store.generations))
		}
		every := writer
		every.QueryWrite = ports.AllProjects()
		if _, err := generate(s, every, first()); err != nil {
			t.Fatalf("a caller who may write every project: %v", err)
		}
	})
	// A current version means the first draft was had; the user loads later
	// drafts themselves.
	t.Run("refuses an ontology that has a current version", func(t *testing.T) {
		s, store := generationService(t, 50)
		base, err := s.CreateOntologyVersion(ctx, &foundryv1.CreateOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Definition: definition(t, "customers")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetCurrentOntologyVersion(ctx, &foundryv1.SetCurrentOntologyVersionRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", VersionId: base.GetVersion().GetId()}); err != nil {
			t.Fatal(err)
		}
		_, err = generate(s, writer, first())
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "current version") || len(store.generations) != 0 {
			t.Fatalf("GenerateOntology = %v, %d queued", err, len(store.generations))
		}
		req := first()
		req.LoadWhenReady = false
		if _, err := generate(s, writer, req); err != nil {
			t.Fatalf("without load_when_ready: %v", err)
		}
	})
	// The sample's draft is the user's to load; asking for both is a
	// mistake, refused before the caller's scope is looked at.
	t.Run("refuses it with the sample", func(t *testing.T) {
		s, store := generationService(t, 50)
		store.sets = []*foundrymodel.FoundryDocumentSet{{ID: "set-k", WorkspaceID: "ws-1", Name: "Acme Corp sample"}}
		store.rows["conn-k"] = &foundrymodel.Connector{ID: "conn-k", WorkspaceID: "ws-1", Kind: "postgres", Name: "Acme Corp", Config: []byte(`{}`)}
		for _, name := range sample.Tables {
			store.tables = append(store.tables, &foundrymodel.ConnectorTable{WorkspaceID: "ws-1", ConnectorID: "conn-k", TableName: name, SyncID: "sync-k"})
		}
		req := &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", ConnectorIds: []string{"conn-k"},
			DocumentSetIds: []string{"set-k"}, Sample: sample.Acme, LoadWhenReady: true}
		for _, as := range []ports.Actor{writer, person} {
			if _, err := generate(s, as, req); status.Code(err) != codes.InvalidArgument || len(store.generations) != 0 {
				t.Fatalf("GenerateOntology = %v, %d queued", err, len(store.generations))
			}
		}
		req.LoadWhenReady = false
		if _, err := generate(s, person, req); err != nil {
			t.Fatalf("the sample alone: %v", err)
		}
	})
}

// An event source is read only when a generation picks it, with how its
// ontology reads it: structured, its events table among the tables, or
// extraction, its own document set among the sets.
func TestGenerateOntologyEventSources(t *testing.T) {
	ctx := context.Background()
	structured := foundryv1.FoundryEventSourceMode_FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED
	extraction := foundryv1.FoundryEventSourceMode_FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION
	// sourced is generationService's workspace with a person's set, General,
	// and five event sources:
	//   - ev-1 synced with rows, its text field note, its set 3 documents;
	//   - ev-2 synced empty, its text field note, its set none;
	//   - ev-3 never synced, with no text field;
	//   - ev-4 synced with rows, its text field reply, its set 2 documents;
	//   - ev-5 with a text field no sync has used yet, so no set.
	sourced := func(t *testing.T, maxTables int) (*Service, *generationStore) {
		t.Helper()
		s, store := generationService(t, maxTables)
		for _, c := range []struct{ id, name, config string }{
			{"ev-1", "Orders feed", `{"description":"Orders","text_field":"note"}`},
			{"ev-2", "Empty feed", `{"description":"Orders","text_field":"note"}`},
			{"ev-3", "New feed", `{"description":"Orders"}`},
			{"ev-4", "Replies feed", `{"description":"Replies","text_field":"reply"}`},
			{"ev-5", "Fresh feed", `{"text_field":"note"}`},
		} {
			store.rows[c.id] = &foundrymodel.Connector{ID: c.id, WorkspaceID: "ws-1", Kind: "events", Name: c.name, Config: []byte(c.config)}
		}
		store.tables = append(store.tables,
			&foundrymodel.ConnectorTable{ConnectorID: "ev-1", WorkspaceID: "ws-1", TableName: "events", RowCount: 3, SyncID: "sync-ev-1", Columns: []byte(`[]`)},
			&foundrymodel.ConnectorTable{ConnectorID: "ev-2", WorkspaceID: "ws-1", TableName: "events", SyncID: "sync-ev-2", Columns: []byte(`[]`)},
			&foundrymodel.ConnectorTable{ConnectorID: "ev-4", WorkspaceID: "ws-1", TableName: "events", RowCount: 2, SyncID: "sync-ev-4", Columns: []byte(`[]`)})
		pieces := func(source string, documents int) *foundrymodel.FoundryDocumentSet {
			return &foundrymodel.FoundryDocumentSet{ID: "set-" + source, WorkspaceID: "ws-1", Name: "Pieces", Documents: documents,
				Owner: &foundrymodel.FoundryDocumentSetOwner{ConnectorID: source, ConnectorKind: foundrymodel.ConnectorKindEvents, ConnectorName: source,
					Stream: connector.EventsStream}}
		}
		store.sets = []*foundrymodel.FoundryDocumentSet{{ID: "set-general", WorkspaceID: "ws-1", Name: "General"}, pieces("ev-1", 3), pieces("ev-2", 0),
			pieces("ev-4", 2)}
		store.text = map[string]int{"set-ev-1": 3, "set-ev-4": 2}
		return s, store
	}
	tablesOf := func(gen *foundrymodel.OntologyGeneration) []string {
		var out []string
		for _, tb := range gen.Input.Tables {
			out = append(out, tb.ConnectorID+"/"+tb.Table+"@"+tb.SyncID)
		}
		slices.Sort(out)
		return out
	}

	// The stored input holds the source's table as any synced table, pinned
	// to the sync that made it, beside the other connectors' tables.
	t.Run("drafts from a structured source's table beside the connectors'", func(t *testing.T) {
		s, store := sourced(t, 50)
		_, err := s.GenerateOntology(ctx, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
			EventSources: []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-1", Mode: structured}}})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := tablesOf(store.generations[0]), []string{"conn-1/customers@", "ev-1/events@sync-ev-1"}; !slices.Equal(got, want) {
			t.Fatalf("tables = %v, want %v", got, want)
		}
	})
	// "Every connector" is the connectors whose tables mean the same to every
	// ontology; an event source needs a mode, so it is never among them.
	t.Run("leaves event sources out of every connector", func(t *testing.T) {
		s, store := sourced(t, 50)
		if _, err := s.GenerateOntology(ctx, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}); err != nil {
			t.Fatal(err)
		}
		if got := tablesOf(store.generations[0]); !slices.Equal(got, []string{"conn-1/customers@"}) {
			t.Fatalf("tables = %v, want the Stripe table alone", got)
		}
	})
	// no_connectors governs connector_ids only, and a source's table is
	// something to read: a generation may draft from event sources alone.
	t.Run("drafts from event sources alone", func(t *testing.T) {
		s, store := sourced(t, 50)
		_, err := s.GenerateOntology(ctx, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", NoConnectors: true,
			EventSources: []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-1", Mode: structured}}})
		if err != nil {
			t.Fatal(err)
		}
		if got := tablesOf(store.generations[0]); !slices.Equal(got, []string{"ev-1/events@sync-ev-1"}) {
			t.Fatalf("tables = %v, want the source's table alone", got)
		}
	})
	// A structured source's table is one of the tables a generation covers.
	t.Run("counts a structured source toward the tables cap", func(t *testing.T) {
		s, store := sourced(t, 1)
		_, err := s.GenerateOntology(ctx, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
			EventSources: []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-1", Mode: structured}}})
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(status.Convert(err).Message(), "2 tables") || len(store.generations) != 0 {
			t.Fatalf("GenerateOntology = %v, want the cap of 1 refused", err)
		}
	})
	// Each pick must name a live event source of the workspace once, with a
	// mode this deployment reads, and a structured one needs rows to map: a sync
	// over no batches publishes an empty table. Nothing is queued otherwise.
	for _, tc := range []struct {
		name         string
		connectorIDs []string
		picks        []*foundryv1.FoundryEventSourcePick
		code         codes.Code
	}{
		{"refuses an event source among the connectors", []string{"ev-1"}, nil, codes.InvalidArgument},
		{"refuses an unknown source", nil, []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-9", Mode: structured}}, codes.NotFound},
		{"refuses a connector of another kind", nil, []*foundryv1.FoundryEventSourcePick{{ConnectorId: "conn-1", Mode: structured}}, codes.InvalidArgument},
		{"refuses a source picked twice", nil, []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-1", Mode: structured}, {ConnectorId: "ev-1", Mode: structured}},
			codes.InvalidArgument},
		{"refuses a pick without a mode", nil, []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-1"}}, codes.InvalidArgument},
		{"refuses a mode this deployment does not know", nil, []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-1", Mode: 9}}, codes.InvalidArgument},
		{"refuses a source picked twice in both modes", nil,
			[]*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-1", Mode: structured}, {ConnectorId: "ev-1", Mode: extraction}}, codes.InvalidArgument},
		{"refuses a source whose table has no rows", nil, []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-2", Mode: structured}}, codes.FailedPrecondition},
		{"refuses a source never synced", nil, []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-3", Mode: structured}}, codes.FailedPrecondition},
		{"refuses extracting a source with no text field", nil, []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-3", Mode: extraction}},
			codes.FailedPrecondition},
		{"refuses extracting a source with no set yet", nil, []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-5", Mode: extraction}},
			codes.FailedPrecondition},
		{"refuses extracting a source whose set holds no document", nil, []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-2", Mode: extraction}},
			codes.FailedPrecondition},
		{"refuses extracting another workspace's source", nil, []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-theirs", Mode: extraction}},
			codes.NotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, store := sourced(t, 50)
			store.rows["ev-theirs"] = &foundrymodel.Connector{ID: "ev-theirs", WorkspaceID: "ws-2", Kind: "events", Name: "Theirs",
				Config: []byte(`{"text_field":"note"}`)}
			store.sets = append(store.sets, &foundrymodel.FoundryDocumentSet{ID: "set-theirs", WorkspaceID: "ws-2", Name: "Pieces", Documents: 4,
				Owner: &foundrymodel.FoundryDocumentSetOwner{ConnectorID: "ev-theirs", ConnectorKind: foundrymodel.ConnectorKindEvents, Stream: connector.EventsStream}})
			_, err := s.GenerateOntology(ctx, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
				ConnectorIds: tc.connectorIDs, EventSources: tc.picks})
			if status.Code(err) != tc.code || len(store.generations) != 0 {
				t.Fatalf("GenerateOntology = %v with %d generations, want %v and none", err, len(store.generations), tc.code)
			}
		})
	}
	// Extraction reads the source's own set; drafted from document_set_ids
	// instead, it would read the pieces with no mode, so it is refused.
	t.Run("refuses a source's set among the chosen sets, naming the source", func(t *testing.T) {
		s, store := sourced(t, 50)
		for _, picks := range [][]*foundryv1.FoundryEventSourcePick{nil, {{ConnectorId: "ev-1", Mode: extraction}}} {
			_, err := s.GenerateOntology(ctx, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
				DocumentSetIds: []string{"set-general", "set-ev-1"}, EventSources: picks})
			want := "document set set-ev-1 holds the API source ev-1's pieces: choose that source in event_sources with mode extraction"
			if status.Code(err) != codes.InvalidArgument || status.Convert(err).Message() != want || len(store.generations) != 0 {
				t.Fatalf("picks %v: GenerateOntology = %v, want %q", picks, err, want)
			}
		}
	})
	// Each refusal names the source as people know it and says what to do.
	for _, tc := range []struct{ name, source, want string }{
		{"says how to choose a text field", "ev-3", `the API source "New feed" has no text field: choose the field that holds each piece's text ` +
			`on its page in Foundry, then wait for its next sync`},
		{"says how to fill a source's set", "ev-5",
			`the API source "Fresh feed" has no pieces as documents yet: send some, and wait for its next sync (about a minute)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := sourced(t, 50)
			_, err := s.GenerateOntology(ctx, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
				EventSources: []*foundryv1.FoundryEventSourcePick{{ConnectorId: tc.source, Mode: extraction}}})
			if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != tc.want {
				t.Fatalf("GenerateOntology = %v, want %q", err, tc.want)
			}
		})
	}
	// The source's set is read as any chosen set, in the workspace's order,
	// once; its table is not read.
	t.Run("drafts by extraction from the source's own set beside the chosen sets", func(t *testing.T) {
		s, store := sourced(t, 50)
		resp, err := s.GenerateOntology(ctx, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
			DocumentSetIds: []string{"set-general"}, EventSources: []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-1", Mode: extraction}}})
		if err != nil {
			t.Fatal(err)
		}
		gen := store.generations[0]
		if want := []string{"set-general", "set-ev-1"}; !slices.Equal(gen.Input.DocumentSetIDs, want) ||
			!slices.Equal(resp.GetGeneration().GetDocumentSetIds(), want) {
			t.Fatalf("sets = %v, shown %v, want %v", gen.Input.DocumentSetIDs, resp.GetGeneration().GetDocumentSetIds(), want)
		}
		if got := tablesOf(gen); !slices.Equal(got, []string{"conn-1/customers@"}) {
			t.Fatalf("tables = %v, want the Stripe table alone", got)
		}
	})
	// no_connectors governs connector_ids only: a source's pieces, read as
	// text, are enough to draft from.
	t.Run("drafts from an extracted source alone", func(t *testing.T) {
		s, store := sourced(t, 50)
		if _, err := s.GenerateOntology(ctx, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", NoConnectors: true,
			EventSources: []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-1", Mode: extraction}}}); err != nil {
			t.Fatal(err)
		}
		if gen := store.generations[0]; len(gen.Input.Tables) != 0 || !slices.Equal(gen.Input.DocumentSetIDs, []string{"set-ev-1"}) {
			t.Fatalf("input = %+v, want the source's set alone", gen.Input)
		}
	})
	// Readers tell an extracted source by its set's connector kind, so the
	// stored input is the same as any other: tables, then sets.
	t.Run("stores one source structured and another extracted, in the input's usual shape", func(t *testing.T) {
		s, store := sourced(t, 50)
		if _, err := s.GenerateOntology(ctx, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", NoConnectors: true,
			EventSources: []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-1", Mode: structured}, {ConnectorId: "ev-4", Mode: extraction}}}); err != nil {
			t.Fatal(err)
		}
		stored, err := json.Marshal(store.generations[0].Input)
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"tables":[{"connector_id":"ev-1","table":"events","sync_id":"sync-ev-1"}],"document_set_ids":["set-ev-4"]}`; string(stored) != want {
			t.Fatalf("stored input = %s, want %s", stored, want)
		}
	})
	// A source's set is one of the sets a generation reads.
	t.Run("counts an extracted source's set toward maxDocumentSets", func(t *testing.T) {
		s, store := sourced(t, 50)
		var chosen []string
		for i := range maxDocumentSets - 1 {
			id := fmt.Sprintf("set-%02d", i)
			store.sets = append(store.sets, &foundrymodel.FoundryDocumentSet{ID: id, WorkspaceID: "ws-1", Name: id})
			chosen = append(chosen, id)
		}
		picks := []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-1", Mode: extraction}, {ConnectorId: "ev-4", Mode: extraction}}
		_, err := s.GenerateOntology(ctx, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", DocumentSetIds: chosen,
			EventSources: picks})
		if status.Code(err) != codes.InvalidArgument || len(store.generations) != 0 {
			t.Fatalf("%d sets and two sources = %v, want the cap refused", len(chosen), err)
		}
		if _, err := s.GenerateOntology(ctx, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", DocumentSetIds: chosen,
			EventSources: picks[:1]}); err != nil {
			t.Fatalf("%d sets and one source: %v", len(chosen), err)
		}
		if got := store.generations[0].Input.DocumentSetIDs; len(got) != maxDocumentSets || !slices.Contains(got, "set-ev-1") {
			t.Fatalf("sets = %v", got)
		}
	})
	// The refusal names the source as people know it and says what to do.
	t.Run("says how to fill a source with no pieces", func(t *testing.T) {
		s, _ := sourced(t, 50)
		_, err := s.GenerateOntology(ctx, &foundryv1.GenerateOntologyRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
			EventSources: []*foundryv1.FoundryEventSourcePick{{ConnectorId: "ev-3", Mode: structured}}})
		if want := `the API source "New feed" has no pieces in its table yet: send some, and wait for its first sync (about a minute)`; status.Convert(err).Message() != want {
			t.Fatalf("GenerateOntology = %v, want %q", err, want)
		}
	})
}

// Clients read the title as proto JSON `title`; a generation queued before
// titles, or without a goal, has none.
func TestOntologyGenerationTitleJSON(t *testing.T) {
	title := "Retail order fulfilment and inventory"
	for _, tc := range []struct {
		title *string
		want  any
	}{{&title, title}, {nil, nil}} {
		encoded, err := protojson.Marshal(ontologyGenerationToProto(&foundrymodel.OntologyGeneration{ID: "gen-1", Title: tc.title}, false))
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["title"] != tc.want {
			t.Fatalf("title %v encodes as %s", tc.title, encoded)
		}
	}
}

// Each document says how ontologies read it and what reading it has come
// to, which the website shows without asking for anything else.
func TestFoundryFileReadProto(t *testing.T) {
	rows, pages := int64(42), 3
	message := "storage is unavailable"
	key := "k"
	for _, tc := range []struct {
		name string
		doc  foundrymodel.FoundryDocument
		want string
	}{
		{"text, ready as it is", foundrymodel.FoundryDocument{MediaType: foundrymodel.FoundryMediaMDX, Status: foundrymodel.FoundryDocumentUploaded},
			`{"kind":"FOUNDRY_READ_KIND_TEXT","state":"FOUNDRY_FILE_READ_STATE_READY"}`},
		{"a table file read", foundrymodel.FoundryDocument{MediaType: foundrymodel.FoundryMediaCSV, Status: foundrymodel.FoundryDocumentUploaded,
			Read: &foundrymodel.FoundryFileRead{Mode: foundrymodel.FoundryFileReadStandard, Status: foundrymodel.FoundryFileReadSucceeded, RowCount: &rows, ArtifactKey: &key}},
			`{"kind":"FOUNDRY_READ_KIND_TABLE","state":"FOUNDRY_FILE_READ_STATE_READY","rows":"42","mode":"FOUNDRY_FILE_READ_MODE_STANDARD"}`},
		{"a table file retrying", foundrymodel.FoundryDocument{MediaType: foundrymodel.FoundryMediaParquet, Status: foundrymodel.FoundryDocumentUploaded,
			Read: &foundrymodel.FoundryFileRead{Mode: foundrymodel.FoundryFileReadStandard, Status: foundrymodel.FoundryFileReadQueued, LastError: &message}},
			`{"kind":"FOUNDRY_READ_KIND_TABLE","state":"FOUNDRY_FILE_READ_STATE_READING","mode":"FOUNDRY_FILE_READ_MODE_STANDARD","retrying":true}`},
		{"a table file that can't be read", foundrymodel.FoundryDocument{MediaType: foundrymodel.FoundryMediaJSON, Status: foundrymodel.FoundryDocumentUploaded,
			Read: &foundrymodel.FoundryFileRead{Mode: foundrymodel.FoundryFileReadStandard, Status: foundrymodel.FoundryFileReadFailed,
				Error: &foundrymodel.FoundryFileReadError{Code: foundrymodel.FoundryFileReadUnsupported, Message: "it holds no list of records"}}},
			`{"kind":"FOUNDRY_READ_KIND_TABLE","state":"FOUNDRY_FILE_READ_STATE_FAILED","mode":"FOUNDRY_FILE_READ_MODE_STANDARD",` +
				`"error":{"code":"unsupported","message":"it holds no list of records"}}`},
		{"a PDF being read", foundrymodel.FoundryDocument{MediaType: foundrymodel.FoundryMediaPDF, Status: foundrymodel.FoundryDocumentUploaded,
			Read: &foundrymodel.FoundryFileRead{Mode: foundrymodel.FoundryFileReadStandard, Status: foundrymodel.FoundryFileReadRunning}},
			`{"kind":"FOUNDRY_READ_KIND_PDF","state":"FOUNDRY_FILE_READ_STATE_READING","mode":"FOUNDRY_FILE_READ_MODE_STANDARD"}`},
		{"a PDF read to its text", foundrymodel.FoundryDocument{MediaType: foundrymodel.FoundryMediaPDF, Status: foundrymodel.FoundryDocumentUploaded,
			Read: &foundrymodel.FoundryFileRead{Mode: foundrymodel.FoundryFileReadStandard, Status: foundrymodel.FoundryFileReadSucceeded, PageCount: &pages, ArtifactKey: &key}},
			`{"kind":"FOUNDRY_READ_KIND_PDF","state":"FOUNDRY_FILE_READ_STATE_READY","pages":3,"mode":"FOUNDRY_FILE_READ_MODE_STANDARD"}`},
		{"a scanned PDF", foundrymodel.FoundryDocument{MediaType: foundrymodel.FoundryMediaPDF, Status: foundrymodel.FoundryDocumentUploaded,
			Read: &foundrymodel.FoundryFileRead{Mode: foundrymodel.FoundryFileReadStandard, Status: foundrymodel.FoundryFileReadFailed,
				Error: &foundrymodel.FoundryFileReadError{Code: foundrymodel.FoundryFileReadUnsupported, Message: "Scanned PDF: no text to read"}}},
			`{"kind":"FOUNDRY_READ_KIND_PDF","state":"FOUNDRY_FILE_READ_STATE_FAILED","mode":"FOUNDRY_FILE_READ_MODE_STANDARD",` +
				`"error":{"code":"unsupported","message":"Scanned PDF: no text to read"}}`},
		{"a file uploaded before reads were queued", foundrymodel.FoundryDocument{MediaType: foundrymodel.FoundryMediaTSV, Status: foundrymodel.FoundryDocumentUploaded},
			`{"kind":"FOUNDRY_READ_KIND_TABLE","state":"FOUNDRY_FILE_READ_STATE_READING"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := protojson.Marshal(foundryFileReadProto(&tc.doc))
			if err != nil {
				t.Fatal(err)
			}
			var gotJSON, wantJSON any
			_ = json.Unmarshal(got, &gotJSON)
			_ = json.Unmarshal([]byte(tc.want), &wantJSON)
			if fmt.Sprint(gotJSON) != fmt.Sprint(wantJSON) {
				t.Fatalf("read = %s\nwant %s", got, tc.want)
			}
		})
	}
	// A document waiting for its bytes, or deleted, has no read to show.
	for _, status := range []foundrymodel.FoundryDocumentStatus{foundrymodel.FoundryDocumentPendingUpload, foundrymodel.FoundryDocumentDeleted} {
		if got := foundryFileReadProto(&foundrymodel.FoundryDocument{MediaType: foundrymodel.FoundryMediaCSV, Status: status}); got != nil {
			t.Errorf("%s: read = %v", status, got)
		}
	}
}
