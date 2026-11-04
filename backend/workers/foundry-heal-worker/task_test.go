package foundryhealworker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/pressly/goose/v3"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	"github.com/helixdb/helix-foundry/backend/ontologyagent/contracttest"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/migrations"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The worker's tests run on the test MySQL server (HELIX_TEST_ROOT_DSN), in
// a schema of their own, with a fake agent; a run's store is Foundry's.

const testSchema = "foundry_heal_worker_test"

var testDB *sql.DB

func TestMain(m *testing.M) {
	rootDSN := os.Getenv("HELIX_TEST_ROOT_DSN")
	if rootDSN == "" {
		rootDSN = "root:helix@tcp(127.0.0.1:3307)/"
	}
	if err := openTestSchema(rootDSN); err != nil {
		fmt.Fprintf(os.Stderr, "foundry-heal-worker tests: %v. Run `docker compose -f compose.test.yaml up -d --wait` first.\n", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = testDB.Close()
	os.Exit(code)
}

func openTestSchema(rootDSN string) error {
	root, err := sql.Open("mysql", rootDSN)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, stmt := range []string{"DROP DATABASE IF EXISTS " + testSchema, "CREATE DATABASE " + testSchema} {
		if _, err := root.Exec(stmt); err != nil {
			return fmt.Errorf("%s: %w", stmt, err)
		}
	}
	mc, err := gomysql.ParseDSN(rootDSN + testSchema)
	if err != nil {
		return err
	}
	mc.ParseTime, mc.InterpolateParams, mc.ClientFoundRows = true, true, true
	if testDB, err = sql.Open("mysql", mc.FormatDSN()); err != nil {
		return err
	}
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("mysql"); err != nil {
		return err
	}
	goose.SetLogger(goose.NopLogger())
	if err := goose.Up(testDB, "."); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	_, err = testDB.Exec(`CREATE TABLE workspaces (id CHAR(26) NOT NULL PRIMARY KEY, status VARCHAR(16) NOT NULL DEFAULT 'active', deleted_at DATETIME(6) NULL)`)
	return err
}

// workspaces is the tests' host table of workspaces.
type workspaces struct{}

func (workspaces) Table() string { return "workspaces" }

func (workspaces) Lock(ctx context.Context, q store.Executor, id string) error {
	var locked string
	if err := q.QueryRowContext(ctx, `SELECT id FROM workspaces WHERE id=? AND deleted_at IS NULL FOR UPDATE`, id).Scan(&locked); errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	} else if err != nil {
		return err
	}
	return nil
}

func (workspaces) Deleting(ctx context.Context, q store.Executor, id string) (bool, error) {
	var deleting bool
	err := q.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL OR status IN ('deleting', 'deleted') FROM workspaces WHERE id=?`, id).Scan(&deleting)
	if errors.Is(err, sql.ErrNoRows) {
		return false, store.ErrNotFound
	}
	return deleting, err
}

// id26 pads tag to a 26-character id.
func id26(tag string) string { return tag + strings.Repeat("0", 26-len(tag)) }

// fp is a 64-hex fingerprint named by tag.
func fp(tag string) string { return tag + strings.Repeat("0", 64-len(tag)) }

// world is one ontology in a fresh database.
type world struct {
	t  *testing.T
	db store.Store
	// workspace and ontology are the ontology's ids.
	workspace, ontology string
	// versions is how many versions it has; prefix starts their ids, and
	// their loads', apart from another ontology's in the same database.
	versions int
	prefix   string
}

func exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := testDB.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

func newWorld(t *testing.T) *world {
	t.Helper()
	rows, err := testDB.Query(`SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name <> 'goose_db_version'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	_ = rows.Close()
	exec(t, "SET FOREIGN_KEY_CHECKS=0")
	for _, table := range tables {
		exec(t, "TRUNCATE TABLE "+table)
	}
	exec(t, "SET FOREIGN_KEY_CHECKS=1")
	w := &world{t: t, db: store.NewMySQL(testDB, workspaces{}), workspace: id26("WS"), ontology: id26("ONT")}
	exec(t, `INSERT INTO workspaces (id) VALUES (?)`, w.workspace)
	exec(t, `INSERT INTO foundry_ontologies (id, workspace_id, name, project_id, tenant_id, client_token, live_key) VALUES (?, ?, 'O', ?, ?, 'o', 1)`,
		w.ontology, w.workspace, id26("PRJ"), id26("TEN"))
	return w
}

// another is a second ontology of w's workspace, in w's database, its ids
// starting tag.
func (w *world) another(tag string) *world {
	o := &world{t: w.t, db: w.db, workspace: w.workspace, ontology: id26(tag + "ONT"), prefix: tag}
	exec(w.t, `INSERT INTO foundry_ontologies (id, workspace_id, name, project_id, tenant_id, client_token, live_key) VALUES (?, ?, 'O', ?, ?, ?, 1)`,
		o.ontology, o.workspace, id26(tag+"PRJ"), id26(tag+"TEN"), tag)
	return o
}

// load writes a new version and a ready load of it, current, whose load
// job's report has these row errors of mapping people (none: a report
// with no row errors), and reviewed documents docs[i] with reviews[i]
// reviews. It returns the load's id.
func (w *world) load(docs int, rowErrors ...string) string {
	w.versions++
	n := w.versions
	version, load := id26(fmt.Sprintf("%sV%d", w.prefix, n)), id26(fmt.Sprintf("%sL%d", w.prefix, n))
	def, _ := json.Marshal(map[string]string{"format": "agent_bundle", "ontology_toml": "version = \"v1\"\n", "mappings_toml": "version = \"v1\"\n"})
	exec(w.t, `INSERT INTO foundry_ontology_versions (id, workspace_id, ontology_id, version, format, definition, source) VALUES (?, ?, ?, ?, 'agent_bundle', ?, 'generated')`,
		version, w.workspace, w.ontology, n, string(def))
	exec(w.t, `INSERT INTO foundry_loads (id, workspace_id, ontology_id, version_id, status, finished_at) VALUES (?, ?, ?, ?, 'ready', NOW(6))`,
		load, w.workspace, w.ontology, version)
	examples, _ := json.Marshal(rowErrors)
	report := fmt.Sprintf(`{"ingest":{"status":"done","report":{"mappings":[{"mapping":"people","create":{"row_errors":{"count":%d,"examples":%s}},
"resolve":{"row_errors":{"count":0,"examples":[]}}}],"created":{"sweep":"swept"}}}}`, len(rowErrors), examples)
	exec(w.t, `INSERT INTO foundry_load_jobs (id, workspace_id, ontology_id, version_id, load_id, kind, status, phase, report, finished_at)
VALUES (?, ?, ?, ?, ?, 'load', 'succeeded', 'finishing', ?, NOW(6))`, load, w.workspace, w.ontology, version, load, report)
	for i := range docs {
		exec(w.t, `INSERT INTO foundry_load_extractions (load_id, document_id, ontology_id, workspace_id, job_id, status, reviews) VALUES (?, ?, ?, ?, ?, 'extracted', 1)`,
			load, id26(fmt.Sprintf("D%03d", i)), w.ontology, w.workspace, load)
	}
	// One document with no reviews, never audited.
	exec(w.t, `INSERT INTO foundry_load_extractions (load_id, document_id, ontology_id, workspace_id, job_id, status, reviews) VALUES (?, ?, ?, ?, ?, 'extracted', 0)`,
		load, id26("DZZZ"), w.ontology, w.workspace, load)
	exec(w.t, `UPDATE foundry_ontologies SET current_version_id=?, current_load_id=? WHERE id=?`, version, load, w.ontology)
	return load
}

// finding reads the ontology's row of fingerprint tag; nil for none.
func (w *world) finding(tag string) *model.FoundryHealFinding {
	w.t.Helper()
	f, err := w.db.GetFoundryHealFinding(context.Background(), w.ontology, fp(tag))
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		w.t.Fatal(err)
	}
	return f
}

// count is how many rows a query counts.
func count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := testDB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// fakeAgent answers each Audit with what respond makes of its request.
type fakeAgent struct {
	t        *testing.T
	contract string
	respond  func(req ontologyagent.AuditRequest) string
	mu       sync.Mutex
	requests []ontologyagent.AuditRequest
	// inFlight and most count the audits running at once.
	inFlight, most atomic.Int32
	// gate, when set, holds each audit until it is closed.
	gate chan struct{}
}

func (a *fakeAgent) Info(context.Context) (*agentv1.GetInfoResponse, error) {
	contract := a.contract
	if contract == "" {
		contract = ontologyagent.ContractVersion
	}
	return &agentv1.GetInfoResponse{ContractVersion: contract}, nil
}

func (a *fakeAgent) Audit(ctx context.Context, body []byte) (json.RawMessage, error) {
	if err := contracttest.Validate(a.t, contracttest.Schema(a.t, "audit_request.json"), body); err != nil {
		a.t.Errorf("an AuditRequest the contract refuses: %v\n%s", err, body)
	}
	var req ontologyagent.AuditRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	if n := a.inFlight.Add(1); n > a.most.Load() {
		a.most.Store(n)
	}
	defer a.inFlight.Add(-1)
	if a.gate != nil {
		select {
		case <-a.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()
	return json.RawMessage(a.respond(req)), nil
}

// sent is the requests the agent took, in order.
func (a *fakeAgent) sent() []ontologyagent.AuditRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.requests)
}

// backed answers a page as findings of these tags, each backed by one
// review of every document of the page, and the rows' errors, if sent.
func backed(tags ...string) func(ontologyagent.AuditRequest) string {
	return func(req ontologyagent.AuditRequest) string {
		var findings []string
		for _, tag := range tags {
			if len(req.DocIDs) == 0 {
				continue
			}
			var examples []string
			for _, doc := range req.DocIDs {
				examples = append(examples, fmt.Sprintf(`{"doc_id":%q,"quote":"Acme supplies Helio"}`, doc))
			}
			findings = append(findings, fmt.Sprintf(`{"fingerprint":%q,"signal":{"kind":"unmatched","canonical":%q,"set":"set_1",
"backing":{"count":%d,"sources":%d,"examples":[%s]}},"proposal":null}`, fp(tag), tag, len(req.DocIDs), len(req.DocIDs), strings.Join(examples[:min(20, len(examples))], ",")))
		}
		rows := "unmeasured"
		if req.Rows != nil {
			rows = "measured"
			for _, e := range req.Rows.RowErrors {
				encoded, _ := json.Marshal(e.Examples)
				findings = append(findings, fmt.Sprintf(`{"fingerprint":%q,"signal":{"kind":"row_errors","mapping":%q,"errors":{"count":%d,"examples":%s}},"proposal":null}`,
					fp("ee"), e.Mapping, e.Count, encoded))
			}
		}
		return fmt.Sprintf(`{"outcome":"done","findings":[%s],"mined":{"mined":"complete"},"rows":%q,"documents":%d,"unread":0,"new_link_support":{"docs":7,"quotes":7}}`,
			strings.Join(findings, ","), rows, len(req.DocIDs))
	}
}

// runIDs numbers the runs every runner starts, so no two share an id.
var runIDs atomic.Int64

var healConfig = config.HealConfig{Enabled: true, PollInterval: time.Second, Every: 24 * time.Hour, RetryAfter: time.Hour, Lease: time.Minute}

// runnerOf is a worker on the world's store and agent, paging pageSize
// documents at a time.
func (w *world) runnerOf(agent *fakeAgent, pageSize int) *runner {
	return &runner{db: w.db, agent: agent, cfg: healConfig, pageSize: pageSize, maxRun: time.Hour,
		newID: func() string { return id26(fmt.Sprintf("R%d", runIDs.Add(1))) },
		open: func(context.Context, model.FoundryHealDue) (target, error) {
			return target{database: "tenant", close: func() {}, helix: ontologyagent.HelixTarget{URL: "http://127.0.0.1:1", Token: "t", MaxRequestBytes: 1 << 20, TimeoutSecs: 30}}, nil
		}}
}

// age moves the ontology's runs back, so the next tick finds the load due.
func (w *world) age(by time.Duration) {
	exec(w.t, `UPDATE foundry_heal_runs SET created_at = created_at - INTERVAL ? MICROSECOND, finished_at = finished_at - INTERVAL ? MICROSECOND
WHERE ontology_id=?`, by.Microseconds(), by.Microseconds(), w.ontology)
}

// latestRun is the ontology's newest run.
func (w *world) latestRun() *model.FoundryHealRun {
	w.t.Helper()
	run, err := w.db.GetLatestFoundryHealRun(context.Background(), w.ontology)
	if err != nil {
		w.t.Fatal(err)
	}
	return run
}

// Three runs of a load store each finding once, its support the load's,
// never summed across runs; the rows go with the first call alone, and a
// load whose every page was read stores its findings.
func TestHealRunsStoreNoDuplicates(t *testing.T) {
	w := newWorld(t)
	load := w.load(5, "row 4 has no name", "row 9 has no name")
	agent := &fakeAgent{t: t, respond: backed("aa", "bb")}
	r := w.runnerOf(agent, 2)
	ctx := context.Background()
	for i := range 3 {
		r.tick(ctx)
		if run := w.latestRun(); run.Status != model.FoundryHealRunComplete || run.Pages != 3 {
			t.Fatalf("run %d = %+v", i, run)
		}
		w.age(25 * time.Hour)
	}
	if n := count(t, `SELECT COUNT(*) FROM foundry_heal_findings WHERE ontology_id=?`, w.ontology); n != 3 {
		t.Fatalf("%d rows, want aa, bb and the rows' finding", n)
	}
	if n := count(t, `SELECT COUNT(*) FROM foundry_heal_runs WHERE ontology_id=?`, w.ontology); n != 3 {
		t.Fatalf("%d runs, want 3", n)
	}
	aa := w.finding("aa")
	if aa.Support != 5 || aa.Sources != 5 || aa.LoadID != load || aa.Status != model.FoundryHealOpen {
		t.Fatalf("aa = %+v", aa)
	}
	if rows := w.finding("ee"); rows == nil || rows.Support != 2 || rows.Sources != 0 {
		t.Fatalf("the rows' finding = %+v", rows)
	}
	sent := agent.sent()
	if len(sent) != 9 {
		t.Fatalf("%d calls, want 3 pages a run", len(sent))
	}
	for i, req := range sent {
		if (req.Rows != nil) != (i%3 == 0) {
			t.Fatalf("call %d: rows %v; they go with a run's first call alone", i, req.Rows)
		}
		if len(req.DocIDs) < 1 || len(req.DocIDs) > 2 || slices.Contains(req.DocIDs, id26("DZZZ")) {
			t.Fatalf("call %d pages %v", i, req.DocIDs)
		}
		if req.Load != load || req.OntologyVersion != "foundry-v1" || req.Database != "tenant" || !strings.HasPrefix(req.RequestID, "R") {
			t.Fatalf("call %d = %+v", i, req)
		}
	}
	shown, err := w.db.ListFoundryHealFindings(ctx, w.ontology)
	if err != nil || len(shown) != 3 {
		t.Fatalf("shown = %v, %v", shown, err)
	}
}

// A person's dismissal holds across a new version's load until a run finds
// twice the support it had.
func TestHealDismissalAcrossVersions(t *testing.T) {
	w := newWorld(t)
	w.load(4)
	agent := &fakeAgent{t: t, respond: backed("aa")}
	r := w.runnerOf(agent, 1000)
	ctx := context.Background()
	r.tick(ctx)
	if err := w.db.DismissFoundryHealFinding(ctx, w.ontology, fp("aa"), id26("USER")); err != nil {
		t.Fatal(err)
	}
	dismissed := w.finding("aa")

	load2 := w.load(7)
	r.tick(ctx)
	held := w.finding("aa")
	if held.Status != model.FoundryHealDismissed || held.Support != 7 || held.LoadID != load2 || held.Dismissal.Support != 4 {
		t.Fatalf("after a version bump at 7 of 8: %+v", held)
	}
	if string(held.Finding) != string(dismissed.Finding) {
		t.Fatal("the dismissed finding changed")
	}
	w.load(8)
	r.tick(ctx)
	if reopened := w.finding("aa"); reopened.Status != model.FoundryHealOpen || reopened.Dismissal != nil || reopened.Support != 8 {
		t.Fatalf("at twice its support: %+v", reopened)
	}
}

// cutResult is a page the agent's wall cut short.
const cutResult = `{"outcome":"done","findings":[],"mined":{"mined":"cut","kinds":["unmatched","unresolved","clash"]},"rows":"unmeasured","documents":1,
"unread":0,"new_link_support":{"docs":7,"quotes":7}}`

// A page the agent's wall cuts short is audited again within the run, from
// the same document at half the size, and the pages grow back after.
func TestHealCutAuditedAgain(t *testing.T) {
	w := newWorld(t)
	w.load(5)
	agent := &fakeAgent{t: t}
	agent.respond = func(req ontologyagent.AuditRequest) string {
		if len(req.DocIDs) > 1 && slices.Contains(req.DocIDs, id26("D002")) {
			return cutResult
		}
		return backed("aa")(req)
	}
	w.runnerOf(agent, 2).tick(context.Background())
	run := w.latestRun()
	if run.Status != model.FoundryHealRunComplete || run.Pages != 4 || run.Cuts != 1 {
		t.Fatalf("run = %+v", run)
	}
	var pages [][]string
	for _, req := range agent.sent() {
		pages = append(pages, req.DocIDs)
	}
	want := [][]string{{id26("D000"), id26("D001")}, {id26("D002"), id26("D003")}, {id26("D002")}, {id26("D003"), id26("D004")}}
	if fmt.Sprint(pages) != fmt.Sprint(want) {
		t.Fatalf("pages %v, want %v", pages, want)
	}
	if aa := w.finding("aa"); aa == nil || aa.Support != 5 || aa.Sources != 5 {
		t.Fatalf("aa = %+v", aa)
	}
}

// Even a page of one document cut short ends the run incomplete, storing
// nothing; the next run, once retry_after has passed, pages the load at
// full size again.
func TestHealCutMarksRunIncomplete(t *testing.T) {
	w := newWorld(t)
	w.load(4)
	var cut atomic.Bool
	cut.Store(true)
	agent := &fakeAgent{t: t}
	agent.respond = func(req ontologyagent.AuditRequest) string {
		if cut.Load() && slices.Contains(req.DocIDs, id26("D002")) {
			return cutResult
		}
		return backed("aa")(req)
	}
	r := w.runnerOf(agent, 2)
	ctx := context.Background()
	r.tick(ctx)
	run := w.latestRun()
	if run.Status != model.FoundryHealRunIncomplete || run.Pages != 3 || run.Cuts != 2 || run.FinishedAt == nil {
		t.Fatalf("cut run = %+v", run)
	}
	if n := count(t, `SELECT COUNT(*) FROM foundry_heal_findings`); n != 0 {
		t.Fatalf("an incomplete run stored %d findings", n)
	}
	r.tick(ctx)
	if w.latestRun().ID != run.ID {
		t.Fatal("audited again before retry_after")
	}
	cut.Store(false)
	w.age(2 * time.Hour)
	r.tick(ctx)
	again := w.latestRun()
	if again.Status != model.FoundryHealRunComplete || again.Pages != 2 || again.Cuts != 0 {
		t.Fatalf("the next run = %+v, want 2 pages of 2", again)
	}
	if aa := w.finding("aa"); aa == nil || aa.Support != 4 {
		t.Fatalf("aa = %+v", aa)
	}
}

// A run past its time ends incomplete at its next page, storing nothing,
// so one load holds the worker no longer.
func TestHealRunOutOfTime(t *testing.T) {
	w := newWorld(t)
	w.load(4)
	r := w.runnerOf(&fakeAgent{t: t, respond: backed("aa")}, 1)
	r.maxRun = time.Nanosecond
	r.tick(context.Background())
	if run := w.latestRun(); run.Status != model.FoundryHealRunIncomplete || run.Pages != 1 || run.Cuts != 0 {
		t.Fatalf("run = %+v", run)
	}
	if n := count(t, `SELECT COUNT(*) FROM foundry_heal_findings`); n != 0 {
		t.Fatalf("a run out of time stored %d findings", n)
	}
}

// A graph job that starts on the ontology while its load is audited (an
// apply here) leaves the run incomplete, storing nothing: its pages may
// have changed under it. The run stops at the page it learns it, not at
// its end.
func TestHealGraphJobMeanwhile(t *testing.T) {
	w := newWorld(t)
	load := w.load(3)
	gate := make(chan struct{})
	agent := &fakeAgent{t: t, respond: backed("aa"), gate: gate}
	done := make(chan struct{})
	go func() { w.runnerOf(agent, 1).tick(context.Background()); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for agent.inFlight.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	exec(t, `INSERT INTO foundry_load_jobs (id, workspace_id, ontology_id, version_id, load_id, kind, status, phase, report, finished_at)
VALUES (?, ?, ?, ?, ?, 'apply', 'succeeded', 'finishing', '{}', NOW(6))`, id26("APPLY"), w.workspace, w.ontology, id26("V1"), load)
	close(gate)
	<-done
	if run := w.latestRun(); run.Status != model.FoundryHealRunIncomplete || run.FinishedAt == nil || run.Pages != 1 {
		t.Fatalf("run = %+v, want it stopped after its first page of 3", run)
	}
	if n := count(t, `SELECT COUNT(*) FROM foundry_heal_findings`); n != 0 {
		t.Fatalf("a run its load changed under stored %d findings", n)
	}
}

// A tick stops once its list of loads due is older than the poll interval:
// the next tick lists them again, so a load just made current waits for
// one run at most.
func TestHealTickRelists(t *testing.T) {
	w := newWorld(t)
	w.load(2)
	other := w.another("B")
	other.load(2)
	r := w.runnerOf(&fakeAgent{t: t, respond: backed("aa")}, 1000)
	r.cfg.PollInterval = time.Nanosecond
	ctx := context.Background()
	r.tick(ctx)
	if n := count(t, `SELECT COUNT(*) FROM foundry_heal_runs`); n != 1 {
		t.Fatalf("%d runs from a list older than the poll interval, want 1", n)
	}
	r.tick(ctx)
	if n := count(t, `SELECT COUNT(DISTINCT ontology_id) FROM foundry_heal_runs WHERE status='complete'`); n != 2 {
		t.Fatalf("%d ontologies audited after the next tick, want both", n)
	}
}

// failingPublish is a store whose publishes fail partway, as a database
// that went away.
type failingPublish struct{ store.Store }

func (failingPublish) PublishFoundryHealRun(context.Context, model.FoundryHealPublish) error {
	return errors.New("the database went away")
}

// A run whose findings couldn't be stored ends failed, so it waits
// retry_after, not just its lease.
func TestHealPublishFails(t *testing.T) {
	w := newWorld(t)
	w.load(2)
	r := w.runnerOf(&fakeAgent{t: t, respond: backed("aa")}, 1000)
	r.db = failingPublish{w.db}
	ctx := context.Background()
	r.tick(ctx)
	run := w.latestRun()
	if run.Status != model.FoundryHealRunFailed || run.FinishedAt == nil {
		t.Fatalf("run = %+v", run)
	}
	r.tick(ctx)
	if w.latestRun().ID != run.ID {
		t.Fatal("audited again before retry_after")
	}
}

// Two workers ticking at once never audit one ontology twice: one holds
// its run, the other leaves it.
func TestHealSingleFlight(t *testing.T) {
	w := newWorld(t)
	w.load(3)
	gate := make(chan struct{})
	agent := &fakeAgent{t: t, respond: backed("aa"), gate: gate}
	first, second := w.runnerOf(agent, 1000), w.runnerOf(agent, 1000)
	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Go(func() { first.tick(ctx) })
	// The first holds the run, waiting in Audit: the second finds the load
	// not due, and starting its run anyway is refused.
	deadline := time.Now().Add(10 * time.Second)
	for agent.inFlight.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if agent.inFlight.Load() != 1 {
		t.Fatal("the first run never audited")
	}
	second.tick(ctx)
	second.run(ctx, model.FoundryHealDue{WorkspaceID: w.workspace, OntologyID: w.ontology, Load: &model.FoundryLoad{ID: id26("L1"), VersionID: id26("V1")}})
	close(gate)
	wg.Wait()
	if most := agent.most.Load(); most != 1 {
		t.Fatalf("%d audits of one ontology at once", most)
	}
	if n := count(t, `SELECT COUNT(*) FROM foundry_heal_runs WHERE ontology_id=?`, w.ontology); n != 1 {
		t.Fatalf("%d runs, want the first's alone", n)
	}
	if run := w.latestRun(); run.Status != model.FoundryHealRunComplete {
		t.Fatalf("run = %+v", run)
	}
}

// A run that can't audit ends failed, and stores nothing: an agent of
// another contract, a load the database doesn't hold, a version that isn't
// the agent's, or findings it can't read.
func TestHealRunFails(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		agent  *fakeAgent
		mutate func(w *world)
	}{
		"another contract": {agent: &fakeAgent{contract: "4.2.0", respond: backed("aa")}},
		"an unknown load":  {agent: &fakeAgent{respond: func(ontologyagent.AuditRequest) string { return `{"outcome":"load_unknown"}` }}},
		"another version": {agent: &fakeAgent{respond: func(ontologyagent.AuditRequest) string {
			return `{"outcome":"load_mismatch","stored_version":"foundry-v9"}`
		}}},
		"an unreadable result": {agent: &fakeAgent{respond: func(ontologyagent.AuditRequest) string { return `{"outcome":"done"}` }}},
		"a version that isn't the agent's": {agent: &fakeAgent{respond: backed("aa")}, mutate: func(w *world) {
			exec(w.t, `UPDATE foundry_ontology_versions SET definition='[]'`)
		}},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.load(2)
			if c.mutate != nil {
				c.mutate(w)
			}
			c.agent.t = t
			w.runnerOf(c.agent, 1000).tick(ctx)
			if run := w.latestRun(); run.Status != model.FoundryHealRunFailed || run.FinishedAt == nil {
				t.Fatalf("run = %+v", run)
			}
			if n := count(t, `SELECT COUNT(*) FROM foundry_heal_findings`); n != 0 {
				t.Fatalf("a failed run stored %d findings", n)
			}
		})
	}
}

// A load whose report keeps no row errors sends no rows: its row findings
// stay as they were, while a review finding no longer found is superseded.
func TestHealRowsUnmeasured(t *testing.T) {
	w := newWorld(t)
	w.load(2, "row 4 has no name")
	r := w.runnerOf(&fakeAgent{t: t, respond: backed("aa")}, 1000)
	ctx := context.Background()
	r.tick(ctx)
	if run := w.latestRun(); !run.RowsMeasured {
		t.Fatalf("a run of a load whose report has rows = %+v", run)
	}
	exec(t, `UPDATE foundry_load_jobs SET report='{"ingest":{"status":"done"}}'`)
	r.agent = &fakeAgent{t: t, respond: backed()}
	w.age(25 * time.Hour)
	r.tick(ctx)
	if aa := w.finding("aa"); aa.Status != model.FoundryHealSuperseded {
		t.Fatalf("aa = %+v", aa)
	}
	if rows := w.finding("ee"); rows.Status != model.FoundryHealOpen {
		t.Fatalf("an unmeasured row finding = %+v", rows)
	}
	if run := w.latestRun(); run.Status != model.FoundryHealRunComplete || run.RowsMeasured {
		t.Fatalf("a run of a load whose report keeps no rows = %+v", run)
	}
}

// A run whose lease is lost, as when it lapsed and another worker took the
// ontology, stops and stores nothing.
func TestHealLostLease(t *testing.T) {
	w := newWorld(t)
	w.load(2)
	agent := &fakeAgent{t: t, respond: backed("aa")}
	r := w.runnerOf(agent, 1000)
	r.cfg.Lease = 30 * time.Millisecond
	gate := make(chan struct{})
	agent.gate = gate
	done := make(chan struct{})
	go func() { r.tick(context.Background()); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for agent.inFlight.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Another worker fails the run as lapsed and takes the ontology.
	exec(t, `UPDATE foundry_heal_runs SET status='failed', active_key=NULL, lease_until=NULL, finished_at=NOW(6)`)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		close(gate)
		t.Fatal("the run kept going without its lease")
	}
	if n := count(t, `SELECT COUNT(*) FROM foundry_heal_findings`); n != 0 {
		t.Fatalf("a lost run stored %d findings", n)
	}
}

// noDatabases is a host's databases no test reads.
type noDatabases struct{ ports.Databases }

// With foundry.heal.enabled off, New is a task that does nothing until
// Stop, and needs no store: a host may list it where heal is off.
func TestNewOff(t *testing.T) {
	cfg := config.Defaults()
	task, err := New(Deps{Cfg: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() { started <- task.Start(context.Background()) }()
	select {
	case err := <-started:
		t.Fatalf("an idle task returned before Stop: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := task.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-started:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an idle task kept going after Stop")
	}
}

// On, New is built on a store and databases.
func TestNew(t *testing.T) {
	cfg := config.Defaults()
	cfg.Foundry.Heal.Enabled = true
	if _, err := New(Deps{Cfg: &cfg}); err == nil {
		t.Fatal("built with no store")
	}
	if _, err := New(Deps{Cfg: &cfg, DB: store.Unimplemented{}}); err == nil {
		t.Fatal("built with no databases")
	}
	deps := Deps{Cfg: &cfg, DB: store.Unimplemented{}, Databases: noDatabases{}}
	cfg.Foundry.Heal.Lease = time.Second
	if _, err := New(deps); err == nil {
		t.Fatal("built with a lease it renews too often")
	}
	cfg.Foundry.Heal.Lease = time.Minute
	task, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	if task.Name() != Name {
		t.Fatalf("name %q", task.Name())
	}
	if err := task.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}
