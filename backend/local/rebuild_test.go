package local

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/local/directory"
	directoryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/directory/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// rebuildFixture is a migrated install with one workspace: what an upgrade
// to the typed-DateTime release starts from.
type rebuildFixture struct {
	db  *sql.DB
	st  store.Store
	ws  string
	log *bytes.Buffer
}

func newRebuildFixture(t *testing.T) rebuildFixture {
	t.Helper()
	db := freshDB(t)
	ctx := context.Background()
	if err := Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	dir := directory.NewStore(db)
	if err := dir.SeedOwner(ctx); err != nil {
		t.Fatal(err)
	}
	owner, err := dir.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w, err := directory.NewService(dir, "http://helix", http.DefaultClient).CreateWorkspace(
		ports.WithActor(ctx, ports.Actor{Source: ports.SourceUser, UserID: owner.ID}),
		&directoryv1.CreateWorkspaceRequest{Slug: "acme", Region: directory.Region})
	if err != nil {
		t.Fatal(err)
	}
	return rebuildFixture{db: db, st: store.NewMySQL(db, Workspaces{}), ws: w.GetId(), log: &bytes.Buffer{}}
}

// tagID is a 26-character id from tag.
func tagID(tag string) string { return (tag + strings.Repeat("0", 26))[:26] }

// ontology makes ontology FO<tag> with a ready current load, FJ<tag>,
// created before the install first started this release when earlier.
func (f rebuildFixture) ontology(t *testing.T, tag string, earlier bool) string {
	t.Helper()
	ctx := context.Background()
	o := &model.FoundryOntology{ID: tagID("FO" + tag), WorkspaceID: f.ws, Name: "Ontology " + tag, ProjectID: tagID("FP" + tag),
		TenantID: tagID("FT" + tag), ClientToken: "ontology-" + tag}
	if err := f.st.CreateFoundryOntology(ctx, o); err != nil {
		t.Fatal(err)
	}
	v := &model.OntologyVersion{ID: tagID("FV" + tag), WorkspaceID: f.ws, OntologyID: o.ID, Source: model.OntologySourceGenerated,
		Format: model.OntologyFormatAgentBundle, Definition: json.RawMessage(`{}`), DocumentSetIDs: &[]string{}}
	if err := f.st.CreateOntologyVersion(ctx, v); err != nil {
		t.Fatal(err)
	}
	f.load(t, o.ID, v.ID, "FJ"+tag)
	f.end(t, tagID("FJ"+tag), model.FoundryGraphJobSucceeded)
	if earlier {
		if _, err := f.db.Exec(`UPDATE foundry_loads SET created_at = DATE_SUB(created_at, INTERVAL 1 DAY) WHERE id = ?`, tagID("FJ"+tag)); err != nil {
			t.Fatal(err)
		}
	}
	return o.ID
}

// load queues a load a person started of version, with the job's id from tag.
func (f rebuildFixture) load(t *testing.T, ontologyID, versionID, tag string) {
	t.Helper()
	token := "load-" + tag
	if err := f.st.CreateFoundryLoad(context.Background(), &model.FoundryGraphJob{ID: tagID(tag), WorkspaceID: f.ws, OntologyID: ontologyID,
		VersionID: versionID, LoadID: tagID(tag), Kind: model.FoundryGraphJobLoad, Phase: model.FoundryGraphJobProvisioning, ClientToken: &token}); err != nil {
		t.Fatal(err)
	}
}

// end claims the due job, which must be jobID, and ends it with status.
func (f rebuildFixture) end(t *testing.T, jobID string, status model.FoundryGraphJobStatus) {
	t.Helper()
	ctx := context.Background()
	job, err := f.st.ClaimFoundryGraphJob(ctx, "w", time.Minute)
	if err != nil || job == nil || job.ID != jobID {
		t.Fatalf("claim %s: %v, %v", jobID, job, err)
	}
	if _, err := f.st.FinishFoundryGraphJob(ctx, job.ID, "w", model.FoundryGraphJobOutcome{Status: status, IngestDone: true,
		Next: model.FoundryGraphNext{ExtractID: tagID("NX" + jobID[2:]), DeleteID: tagID("ND" + jobID[2:]), ResendID: tagID("NR" + jobID[2:]), RetainLoads: 5}}); err != nil {
		t.Fatal(err)
	}
}

// rebuild runs RebuildOwedLoads as run does, and checks whether it reports
// a rebuild still owed.
func (f rebuildFixture) rebuild(t *testing.T, owed bool) {
	t.Helper()
	got, err := RebuildOwedLoads(context.Background(), f.db, f.st, f.logger())
	if err != nil {
		t.Fatal(err)
	}
	if got != owed {
		t.Fatalf("owed = %t, want %t: %s", got, owed, f.log)
	}
}

func (f rebuildFixture) logger() *slog.Logger { return slog.New(slog.NewTextHandler(f.log, nil)) }

// drain claims and ends every due job: a load with its ontology's status
// in loads, anything else (a delete of a load a rebuild replaced)
// succeeded.
func (f rebuildFixture) drain(t *testing.T, loads map[string]model.FoundryGraphJobStatus) {
	t.Helper()
	ctx := context.Background()
	for {
		job, err := f.st.ClaimFoundryGraphJob(ctx, "w", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if job == nil {
			return
		}
		status := model.FoundryGraphJobSucceeded
		if job.Kind == model.FoundryGraphJobLoad {
			status = loads[job.OntologyID]
		}
		if _, err := f.st.FinishFoundryGraphJob(ctx, job.ID, "w", model.FoundryGraphJobOutcome{Status: status, IngestDone: true,
			Next: model.FoundryGraphNext{ExtractID: id.New(), DeleteID: id.New(), ResendID: id.New(), RetainLoads: 5}}); err != nil {
			t.Fatal(err)
		}
	}
}

// done reports whether the typed-DateTime rebuild is marked done.
func (f rebuildFixture) done(t *testing.T) bool {
	t.Helper()
	var done bool
	if err := f.db.QueryRow(`SELECT done_at IS NOT NULL FROM foundry_local_rebuilds WHERE name = 'typed_datetime'`).Scan(&done); err != nil {
		t.Fatal(err)
	}
	return done
}

// live is the ontology's live job's id and its load's origin, or "".
func (f rebuildFixture) live(t *testing.T, ontologyID string) (string, model.FoundryLoadOrigin) {
	t.Helper()
	job, err := f.st.GetActiveFoundryGraphJob(context.Background(), ontologyID)
	if err != nil {
		return "", ""
	}
	l, err := f.st.GetFoundryLoad(context.Background(), ontologyID, job.LoadID)
	if err != nil {
		t.Fatal(err)
	}
	return job.ID, l.Origin
}

// A fresh install owes the rebuild too and finds nothing to reload: it is
// done at its first start.
func TestRebuildOwedLoadsOnAFreshInstall(t *testing.T) {
	f := newRebuildFixture(t)
	if f.done(t) {
		t.Fatal("the migration owes the rebuild")
	}
	f.rebuild(t, false)
	if !f.done(t) {
		t.Fatalf("not done: %s", f.log)
	}
	// Done, it is never run again.
	o := f.ontology(t, "A", true)
	f.rebuild(t, false)
	if job, _ := f.live(t, o); job != "" {
		t.Fatalf("a done rebuild queued %s", job)
	}
}

// An upgrade reloads every current load an earlier release wrote, once
// each: an ontology a live job holds back is reloaded once it ends, one
// whose reload failed is reloaded again, and the rebuild is done only once
// every one is reloaded.
func TestRebuildOwedLoadsOnAnUpgrade(t *testing.T) {
	f := newRebuildFixture(t)
	old := f.ontology(t, "A", true)
	busy := f.ontology(t, "B", true)
	current := f.ontology(t, "C", false)
	f.load(t, busy, tagID("FVB"), "UB")

	f.rebuild(t, true)
	if job, origin := f.live(t, old); job == "" || origin != model.FoundryLoadByRefresh {
		t.Fatalf("old: %q %q", job, origin)
	}
	if job, origin := f.live(t, busy); job != tagID("UB") || origin != model.FoundryLoadByUser {
		t.Fatalf("busy: %q %q", job, origin)
	}
	if job, _ := f.live(t, current); job != "" {
		t.Fatalf("a load of this release is reloaded: %s", job)
	}
	if f.done(t) {
		t.Fatal("done while an ontology is held back")
	}
	if !strings.Contains(f.log.String(), busy) {
		t.Fatalf("the held back ontology isn't logged: %s", f.log)
	}
	rebuilt, _ := f.live(t, old)

	// The user's load ended; the old rebuild still runs.
	f.end(t, tagID("UB"), model.FoundryGraphJobFailed)
	f.rebuild(t, true)
	if job, origin := f.live(t, busy); job == "" || job == tagID("UB") || origin != model.FoundryLoadByRefresh {
		t.Fatalf("busy, next run: %q %q", job, origin)
	}
	if job, _ := f.live(t, old); job != rebuilt {
		t.Fatalf("old was queued twice: %s, then %s", rebuilt, job)
	}
	if f.done(t) {
		t.Fatal("done while reloads are under way")
	}

	// The old one's reload failed: it is queued again, and the rebuild is
	// done once both reloads replaced their loads.
	f.drain(t, map[string]model.FoundryGraphJobStatus{old: model.FoundryGraphJobFailed, busy: model.FoundryGraphJobSucceeded})
	f.rebuild(t, true)
	again, _ := f.live(t, old)
	if again == "" || again == rebuilt {
		t.Fatalf("a failed reload isn't queued again: %q", again)
	}
	if job, _ := f.live(t, busy); job != "" {
		t.Fatalf("a replaced load is reloaded again: %s", job)
	}
	f.drain(t, map[string]model.FoundryGraphJobStatus{old: model.FoundryGraphJobSucceeded})
	f.rebuild(t, false)
	if !f.done(t) {
		t.Fatalf("not done once every load is reloaded: %s", f.log)
	}
}

// An ontology whose reloads keep failing is given up on, and logged: the
// rebuild is done.
func TestRebuildOwedLoadsGivesUp(t *testing.T) {
	f := newRebuildFixture(t)
	o := f.ontology(t, "A", true)
	for range store.MaxFoundryRebuilds {
		f.rebuild(t, true)
		f.drain(t, map[string]model.FoundryGraphJobStatus{o: model.FoundryGraphJobFailed})
	}
	f.rebuild(t, false)
	if !f.done(t) || !strings.Contains(f.log.String(), "keep failing") {
		t.Fatalf("not given up: %s", f.log)
	}
}

// Rebuilds runs the rebuild until nothing is owed: a load a live job held
// back at start is reloaded once that job ends, without a restart.
func TestRebuildsRunsUntilNothingIsOwed(t *testing.T) {
	f := newRebuildFixture(t)
	o := f.ontology(t, "A", true)
	f.load(t, o, tagID("FVA"), "UA")
	f.rebuild(t, true)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ended := make(chan error, 1)
	go func() {
		ended <- Rebuilds{DB: f.db, Store: f.st, Logger: f.logger(), Every: 20 * time.Millisecond}.Start(ctx)
	}()
	f.end(t, tagID("UA"), model.FoundryGraphJobFailed)
	var rebuild string
	for rebuild == "" || rebuild == tagID("UA") {
		select {
		case <-ctx.Done():
			t.Fatal("no reload queued")
		case <-time.After(20 * time.Millisecond):
		}
		rebuild, _ = f.live(t, o)
	}
	f.drain(t, map[string]model.FoundryGraphJobStatus{o: model.FoundryGraphJobSucceeded})
	if err := <-ended; err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !f.done(t) {
		t.Fatalf("not done: %s", f.log)
	}
}
