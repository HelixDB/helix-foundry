package foundryhealworker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
)

// movedFor records load moved for finding tag, as "Add this link" leaves
// it, with the finding stored: a fill due to collect.
func (w *world) movedFor(load, tag string) {
	w.t.Helper()
	var version string
	if err := testDB.QueryRow(`SELECT version_id FROM foundry_loads WHERE id=?`, load).Scan(&version); err != nil {
		w.t.Fatal(err)
	}
	exec(w.t, `INSERT INTO foundry_heal_fills (id, ontology_id, load_id, fingerprint, from_version_id, to_version_id, state, links_added)
VALUES (?, ?, ?, ?, ?, ?, 'moved', '["mentors"]')`, id26("HF"+tag), w.ontology, load, fp(tag), version, version)
	exec(w.t, `INSERT INTO foundry_heal_findings (ontology_id, fingerprint, kind, status, base_version_id, load_id, finding, support, sources)
VALUES (?, ?, 'unmatched', 'open', ?, ?, ?, 3, 3)`, w.ontology, fp(tag), version, load,
		`{"signal":{"kind":"unmatched","canonical":"mentors","spellings":["mentors","mentor_of"],"set":"set_1"}}`)
}

// listing answers each page with its documents of even number as the
// finding's: what documents_for lists.
func listing(req ontologyagent.AuditRequest) string {
	var docs []string
	for i, d := range req.DocIDs {
		if i%2 == 0 {
			docs = append(docs, fmt.Sprintf("%q", d))
		}
	}
	of := ""
	if len(req.DocumentsFor) > 0 {
		of = fmt.Sprintf(`,"documents_of":[{"set":%q,"doc_ids":[%s]}]`, req.DocumentsFor[0].Set, strings.Join(docs, ","))
	}
	return fmt.Sprintf(`{"outcome":"done","findings":[],"mined":{"mined":"complete"},"rows":"unmeasured","documents":%d,"unread":0,`+
		`"new_link_support":{"docs":7,"quotes":7}%s}`, len(req.DocIDs), of)
}

// A moved load's fill: the worker pages the load's reviewed documents
// asking for the finding's, owes each a fill, and starts the fill with a
// resend queued; it publishes nothing and is collected once.
func TestHealFillCollectsTheFindingsDocuments(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	load := w.load(5)
	w.movedFor(load, "ff")
	agent := &fakeAgent{t: t, respond: listing}
	r := w.runnerOf(agent, 2)
	r.fills(ctx)
	sent := agent.sent()
	if len(sent) != 3 {
		t.Fatalf("%d pages, want 3 of 2 documents", len(sent))
	}
	for _, req := range sent {
		if len(req.DocumentsFor) != 1 || req.DocumentsFor[0].Set != "set_1" || !slices.Equal(req.DocumentsFor[0].Spellings, []string{"mentors", "mentor_of"}) {
			t.Fatalf("documents_for %+v", req.DocumentsFor)
		}
		if req.Rows != nil {
			t.Fatal("a collect sends no rows")
		}
	}
	if n := count(t, `SELECT COUNT(*) FROM foundry_load_resends WHERE load_id=? AND reason='link' AND status='owed'`, load); n != 3 {
		t.Fatalf("%d documents owed, want 3", n)
	}
	var state string
	var owed, left int
	if err := testDB.QueryRow(`SELECT state, docs_owed, docs_left FROM foundry_heal_fills WHERE load_id=?`, load).Scan(&state, &owed, &left); err != nil {
		t.Fatal(err)
	}
	if state != "applying" || owed != 3 || left != 0 {
		t.Fatalf("fill %s owed %d left %d", state, owed, left)
	}
	if n := count(t, `SELECT COUNT(*) FROM foundry_load_jobs WHERE load_id=? AND kind='resend' AND status='queued'`, load); n != 1 {
		t.Fatalf("%d resends queued, want 1", n)
	}
	if n := count(t, `SELECT COUNT(*) FROM foundry_heal_runs WHERE ontology_id=?`, w.ontology); n != 0 {
		t.Fatalf("a collect runs no audit: %d runs", n)
	}
	// Collected once: a live job, and the fill applies.
	r.fills(ctx)
	if len(agent.sent()) != 3 {
		t.Fatal("collected twice")
	}
}

// A fill whose documents wait with no job live (the job that sent them
// failed, and its pause ended) is resumed with a resend; one paused isn't.
func TestHealFillResumed(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	load := w.load(2)
	w.movedFor(load, "fr")
	exec(t, `UPDATE foundry_heal_fills SET state='applying', active_key=load_id, docs_owed=1, paused_until=NOW(6) + INTERVAL 1 HOUR WHERE load_id=?`, load)
	exec(t, `INSERT INTO foundry_load_resends (load_id, document_id, reason) VALUES (?, ?, 'link')`, load, id26("D000"))
	agent := &fakeAgent{t: t, respond: listing}
	r := w.runnerOf(agent, 10)
	r.fills(ctx)
	if n := count(t, `SELECT COUNT(*) FROM foundry_load_jobs WHERE load_id=? AND kind='resend'`, load); n != 0 {
		t.Fatalf("a paused fill was resumed: %d jobs", n)
	}
	exec(t, `UPDATE foundry_heal_fills SET paused_until=NOW(6) - INTERVAL 1 SECOND WHERE load_id=?`, load)
	r.fills(ctx)
	if n := count(t, `SELECT COUNT(*) FROM foundry_load_jobs WHERE load_id=? AND kind='resend' AND status='queued'`, load); n != 1 {
		t.Fatalf("%d resends queued, want 1", n)
	}
	if n := count(t, `SELECT COUNT(*) FROM foundry_heal_fills WHERE load_id=? AND paused_until IS NULL`, load); n != 1 {
		t.Fatal("the pause stays")
	}
	if len(agent.sent()) != 0 {
		t.Fatal("a resume audits nothing")
	}
}

// A collect whose suggestion names no relationship, or whose agent speaks
// another contract, owes nothing and starts nothing.
func TestHealFillCollectRefused(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	load := w.load(2)
	w.movedFor(load, "fx")
	exec(t, `UPDATE foundry_heal_findings SET finding='{"signal":{"kind":"clash"}}' WHERE ontology_id=?`, w.ontology)
	r := w.runnerOf(&fakeAgent{t: t, respond: listing}, 10)
	r.fills(ctx)
	other := w.another("O2")
	otherLoad := other.load(2)
	other.movedFor(otherLoad, "fy")
	r = other.runnerOf(&fakeAgent{t: t, respond: listing, contract: "4.31.0"}, 10)
	r.fills(ctx)
	for _, l := range []string{load, otherLoad} {
		if n := count(t, `SELECT COUNT(*) FROM foundry_heal_fills WHERE load_id=? AND state='moved'`, l); n != 1 {
			t.Fatalf("load %s's fill changed", l)
		}
		if n := count(t, `SELECT COUNT(*) FROM foundry_load_resends WHERE load_id=?`, l); n != 0 {
			t.Fatalf("load %s owes %d", l, n)
		}
		// Counted and backed off: not tried again at the next tick.
		if n := count(t, `SELECT COUNT(*) FROM foundry_heal_fills WHERE load_id=? AND failures=1 AND paused_until > NOW(6)`, l); n != 1 {
			t.Fatalf("load %s's failed collect wasn't counted", l)
		}
	}
}

// A move that didn't add the suggestion's link owes nothing: its fill ends
// at once, taking no review, which reads as needing a load. A collect ends
// what one that stopped partway left owed.
func TestHealFillOfALinkTheMoveDidntAdd(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	load := w.load(2)
	w.movedFor(load, "fl")
	exec(t, `UPDATE foundry_heal_fills SET links_added='["advises"]' WHERE load_id=?`, load)
	exec(t, `INSERT INTO foundry_load_resends (load_id, document_id, reason) VALUES (?, ?, 'link')`, load, id26("D001"))
	agent := &fakeAgent{t: t, respond: listing}
	w.runnerOf(agent, 10).fills(ctx)
	if len(agent.sent()) != 0 {
		t.Fatal("nothing to list")
	}
	if n := count(t, `SELECT COUNT(*) FROM foundry_heal_fills WHERE load_id=? AND state='filled' AND reviews_taken=0`, load); n != 1 {
		t.Fatal("the fill didn't end")
	}
	if n := count(t, `SELECT COUNT(*) FROM foundry_load_resends WHERE load_id=? AND status='owed'`, load); n != 0 {
		t.Fatalf("%d rows still owed", n)
	}
}

// A fill another worker collects, its lease held, is left alone: nothing
// listed, owed or counted against it.
func TestHealFillCollectLeasedElsewhere(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	load := w.load(4)
	w.movedFor(load, "fz")
	exec(t, `UPDATE foundry_heal_fills SET paused_until=NOW(6) + INTERVAL 1 HOUR WHERE load_id=?`, load)
	agent := &fakeAgent{t: t, respond: listing}
	w.runnerOf(agent, 2).fills(ctx)
	if len(agent.sent()) != 0 {
		t.Fatal("collected under another worker's lease")
	}
	if n := count(t, `SELECT COUNT(*) FROM foundry_heal_fills WHERE load_id=? AND state='moved' AND failures=0`, load); n != 1 {
		t.Fatal("the fill changed")
	}
}

// A tick spends its poll interval on fills at most, one at least: past it,
// the fills left wait for the next tick, and the audits due run between.
func TestHealFillsBoundedByTheTick(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	a := w.load(2)
	w.movedFor(a, "fa")
	other := w.another("O3")
	b := other.load(2)
	other.movedFor(b, "fb")
	agent := &fakeAgent{t: t, respond: listing}
	r := w.runnerOf(agent, 10)
	r.cfg.PollInterval = time.Nanosecond
	r.fills(ctx)
	started := `SELECT COUNT(*) FROM foundry_heal_fills WHERE load_id IN (?, ?) AND state='applying'`
	if n := count(t, started, a, b); n != 1 {
		t.Fatalf("%d fills started in one tick, want 1", n)
	}
	r.fills(ctx)
	if n := count(t, started, a, b); n != 2 {
		t.Fatalf("%d fills started after the next tick, want 2", n)
	}
}

// windowed is an agent that answers pages pages a window, then holds the
// next until the collect's time runs out: a load too large for one window.
type windowed struct {
	*fakeAgent
	pages  int32
	served atomic.Int32
}

func (a *windowed) Audit(ctx context.Context, body []byte) (json.RawMessage, error) {
	if a.served.Add(1)%(a.pages+1) == 0 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return a.fakeAgent.Audit(ctx, body)
}

// cursor reads the load's fill's saved collect: its last document and
// what it owed; "" for none.
func cursor(t *testing.T, load string) (after string, owed int) {
	t.Helper()
	var at sql.NullString
	if err := testDB.QueryRow(`SELECT collect_after, collect_owed FROM foundry_heal_fills WHERE load_id=?`, load).Scan(&at, &owed); err != nil {
		t.Fatal(err)
	}
	return at.String, owed
}

// A collect that runs out of time past pages it saved gives its lease up,
// counting no failure, and the next tick resumes after its last page, never
// listing a document twice, window after window, until it starts the fill
// owing every document once, its cursor cleared.
func TestHealFillCollectResumedAcrossWindows(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	load := w.load(5)
	w.movedFor(load, "fw")
	agent := &windowed{fakeAgent: &fakeAgent{t: t, respond: listing}, pages: 1}
	r := w.runnerOf(nil, 2)
	r.agent, r.maxRun = agent, 300*time.Millisecond
	for window := 1; window <= 2; window++ {
		r.fills(ctx)
		after, owed := cursor(t, load)
		if after != id26(fmt.Sprintf("D%03d", 2*window-1)) || owed != window {
			t.Fatalf("window %d: cursor after %q owing %d", window, after, owed)
		}
		if n := count(t, `SELECT COUNT(*) FROM foundry_heal_fills WHERE load_id=? AND state='moved' AND failures=0 AND paused_until IS NULL`, load); n != 1 {
			t.Fatalf("window %d: the fill counted a failure or kept its lease", window)
		}
		if n := count(t, `SELECT COUNT(*) FROM foundry_load_resends WHERE load_id=? AND reason='link' AND status='owed'`, load); n != window {
			t.Fatalf("window %d: %d documents owed", window, n)
		}
	}
	r.fills(ctx)
	var listed []string
	for _, req := range agent.sent() {
		listed = append(listed, req.DocIDs...)
	}
	want := []string{id26("D000"), id26("D001"), id26("D002"), id26("D003"), id26("D004")}
	if !slices.Equal(listed, want) {
		t.Fatalf("listed %v, want each reviewed document once, in order", listed)
	}
	var state string
	var owed int
	if err := testDB.QueryRow(`SELECT state, docs_owed FROM foundry_heal_fills WHERE load_id=?`, load).Scan(&state, &owed); err != nil {
		t.Fatal(err)
	}
	if state != "applying" || owed != 3 {
		t.Fatalf("fill %s owing %d, want applying owing 3", state, owed)
	}
	if after, _ := cursor(t, load); after != "" {
		t.Fatalf("a started fill keeps its cursor %q", after)
	}
}

// A cursor saved at a version the load has moved on from (a later move in
// place) is stale: the next collect starts over, ending what the first
// owed, and owes each document once.
func TestHealFillCollectStartsOverAfterANewerMove(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	load := w.load(5)
	w.movedFor(load, "fv")
	agent := &windowed{fakeAgent: &fakeAgent{t: t, respond: listing}, pages: 1}
	r := w.runnerOf(nil, 2)
	r.agent, r.maxRun = agent, 300*time.Millisecond
	r.fills(ctx)
	if after, owed := cursor(t, load); after == "" || owed != 1 {
		t.Fatalf("cursor after %q owing %d", after, owed)
	}
	// Another suggestion's move: the load records a newer version, along a
	// chain of moves that each only add.
	var from string
	if err := testDB.QueryRow(`SELECT version_id FROM foundry_loads WHERE id=?`, load).Scan(&from); err != nil {
		t.Fatal(err)
	}
	newer := id26("FVNEWER")
	exec(t, `INSERT INTO foundry_ontology_versions (id, workspace_id, ontology_id, version, format, definition, source)
SELECT ?, workspace_id, ontology_id, version + 10, format, definition, source FROM foundry_ontology_versions WHERE id=?`, newer, from)
	exec(t, `INSERT INTO foundry_heal_fills (id, ontology_id, load_id, fingerprint, from_version_id, to_version_id, state, links_added)
VALUES (?, ?, ?, ?, ?, ?, 'filled', '["advises"]')`, id26("HFfv2"), w.ontology, load, fp("fv2"), from, newer)
	exec(t, `UPDATE foundry_loads SET version_id=? WHERE id=?`, newer, load)
	agent.pages = 10
	r.fills(ctx)
	sent := agent.sent()
	if first := sent[1].DocIDs[0]; first != id26("D000") {
		t.Fatalf("the collect resumed at %s at a version its cursor didn't list", first)
	}
	var state string
	var owed int
	if err := testDB.QueryRow(`SELECT state, docs_owed FROM foundry_heal_fills WHERE load_id=? AND fingerprint=?`, load, fp("fv")).Scan(&state, &owed); err != nil {
		t.Fatal(err)
	}
	if state != "applying" || owed != 3 {
		t.Fatalf("fill %s owing %d, want applying owing 3", state, owed)
	}
	if n := count(t, `SELECT COUNT(*) FROM foundry_load_resends WHERE load_id=? AND reason='link' AND status='owed'`, load); n != 3 {
		t.Fatalf("%d documents owed, want 3", n)
	}
}
