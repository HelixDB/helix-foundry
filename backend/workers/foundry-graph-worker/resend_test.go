package foundrygraphworker

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// resendJob sends load-0 the documents it owes a resend.
func resendJob() *model.FoundryGraphJob {
	job := loadJob()
	job.ID, job.LoadID, job.Kind, job.Phase = "job-4", "load-0", model.FoundryGraphJobResend, model.FoundryGraphJobDocuments
	return job
}

// settledAt is when the fixtures' approvals were settled.
var settledAt = time.Date(2026, 10, 17, 8, 2, 3, 0, time.UTC)

const (
	hashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	claim = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// approval is a release decision of doc, as a load's list holds it.
func approval(id, doc, hash string) *model.FoundryReviewDecision {
	claimKey := claim
	return &model.FoundryReviewDecision{
		ID: id, OntologyID: "ont-1", Kind: model.FoundryReviewRelease, DocID: &doc, ClaimKey: &claimKey,
		ClaimSpan: &model.FoundryClaimSpan{Section: 0, Start: 2, End: 9}, StatementHash: &hash, DecidedByUserID: "user",
		DecidedAt: settledAt,
	}
}

// resent is a held document's event of a resend: nothing committed, and what
// became of each release sent with it.
func resent(doc string, released int, releases string) string {
	return `{"event":"document","doc_id":"` + doc + `","outcome":{"kind":"already_ingested"},"text_bytes":120,"sections":2,` +
		`"counts":{"entities":0,"mentions":0,"facts":0,"reviews":0,"unresolved":0,"rejected":0,"attributes":0,"attribute_reviews":0,` +
		`"prop_conflicts":0,"created":0,"identifiers_unverified":0,"attributes_over_entries":0,"released":` + strconv.Itoa(released) +
		`,"release_refused":0,"release_missed":0,"resolved":{"user":0,"adjudicated":0,"identifier":0,"name":0,"alias":0,"legal_form":0,"created":0}},` +
		noUsage + `,"releases":` + releases + `}`
}

// owes makes load-0 owe each of docs a resend for an approval, as the
// harness's documents.
func owes(h *harness, docs ...string) {
	byID := map[string]*model.FoundryDocument{}
	for _, d := range h.store.pending {
		byID[d.ID] = d
	}
	// A resend sends what its load holds, never what it waits for.
	h.store.pending = nil
	for i, id := range docs {
		h.store.resends = append(h.store.resends, &model.FoundryLoadResend{
			LoadID: "load-0", DocumentID: id, Reason: model.FoundryResendApproval, Requests: i + 1, Document: byID[id],
		})
	}
}

func TestResend(t *testing.T) {
	// A person approved a claim of doc-a and one of doc-b. One Extract sends
	// both documents again, each with its saved extraction and its
	// decisions, with nothing to spend; what the engine says of each
	// decision is recorded with its document, and each request ends.
	t.Run("sends the owed documents once, at no cost, and records each decision", func(t *testing.T) {
		h := newHarness(t)
		owes(h, "doc-a", "doc-b")
		h.bucket.put(document.ArtifactPrefix("ws-1", "doc-a")+"key_a.json.gz", []byte(`{"calls":[]}`))
		h.store.decisions = []*model.FoundryReviewDecision{approval("1", "doc-a", hashA), approval("2", "doc-b", hashB), approval("3", "doc-c", hashA)}
		h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
			return extractEvents(stream, []string{
				extracted[0],
				resent("doc-a", 1, `[{"decision_id":"1","outcome":{"kind":"written"}}]`),
				resent("doc-b", 0, `[{"decision_id":"2","outcome":{"kind":"refused","reason":"unresolved"}}]`),
				`{"event":"extract_finished"}`,
			}, `{"status":"done","totals":`+totals+`}`)
		}
		outcome := h.run(t, resendJob())
		if outcome.Status != model.FoundryGraphJobSucceeded || outcome.Error != "" {
			t.Fatalf("%+v", outcome)
		}
		ingests, calls := h.agent.calls()
		if len(ingests) != 0 || len(calls) != 1 {
			t.Fatalf("%d ingests and %d extracts, want one Extract", len(ingests), len(calls))
		}
		req := decodeExtract(t, calls[0])
		if req.Load != "load-0" || req.Limits.MaxUSD != 0 || len(req.Remove) != 0 {
			t.Fatalf("sent load %s, max_usd %v, remove %v", req.Load, req.Limits.MaxUSD, req.Remove)
		}
		if len(req.Documents) != 2 || req.Documents[0].DocID != "doc-a" || req.Documents[1].DocID != "doc-b" {
			t.Fatalf("sent %+v", req.Documents)
		}
		if len(req.Documents[0].Artifacts) != 1 || len(req.Documents[1].Artifacts) != 0 {
			t.Fatalf("artifacts %v and %v", req.Documents[0].Artifacts, req.Documents[1].Artifacts)
		}
		// The decisions of the documents sent, and no other document's.
		if len(req.Decisions) != 2 || req.Decisions[0].DecisionID != "1" || req.Decisions[1].DecisionID != "2" {
			t.Fatalf("decisions %+v", req.Decisions)
		}
		a, b := h.store.extractions["doc-a"], h.store.extractions["doc-b"]
		if a == nil || b == nil || !h.store.kept["doc-a"] || !h.store.kept["doc-b"] {
			t.Fatalf("recorded %+v and %+v, kept %v", a, b, h.store.kept)
		}
		// A held document's event counts only what it released: a fact more.
		if a.Counts.Facts != 1 || a.Counts.Reviews != 0 || a.USDMicros != 0 {
			t.Fatalf("doc-a counts %+v", a.Counts)
		}
		wantA := []model.FoundryReleaseOutcome{{DecisionID: "1", StatementHash: hashA, Kind: model.FoundryReleaseOutcomeWritten}}
		wantB := []model.FoundryReleaseOutcome{{DecisionID: "2", StatementHash: hashB, Kind: model.FoundryReleaseOutcomeRefused, Reason: "unresolved"}}
		if !reflect.DeepEqual(a.Releases, wantA) || !reflect.DeepEqual(b.Releases, wantB) {
			t.Fatalf("releases %+v and %+v", a.Releases, b.Releases)
		}
		want := []model.FoundryResendEnd{
			{DocumentID: "doc-a", Reason: model.FoundryResendApproval, Requests: 1},
			{DocumentID: "doc-b", Reason: model.FoundryResendApproval, Requests: 2},
		}
		if !reflect.DeepEqual(h.store.ended, want) {
			t.Fatalf("ended %+v", h.store.ended)
		}
		if last := h.store.lastUpdate(); last.Phase != model.FoundryGraphJobFinishing || last.Progress.DocumentsDone != 2 || last.Progress.DocumentsTotal != 2 {
			t.Fatalf("last update %+v", last)
		}
	})

	// A document whose saved extraction is gone would need a model call the
	// run has no money for: the engine sends no event for it and ends
	// budget_exhausted. Its approvals are refused, saying so, and the job
	// still succeeds: nothing waits for a job that can't apply it.
	t.Run("a document that doesn't replay refuses its approvals", func(t *testing.T) {
		h := newHarness(t)
		owes(h, "doc-a", "doc-b")
		h.store.decisions = []*model.FoundryReviewDecision{approval("1", "doc-a", hashA), approval("2", "doc-b", hashB)}
		h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
			return extractEvents(stream, []string{
				extracted[0], resent("doc-a", 1, `[{"decision_id":"1","outcome":{"kind":"written"}}]`), `{"event":"extract_finished"}`,
			}, `{"status":"budget_exhausted","totals":`+totals+`}`)
		}
		outcome := h.run(t, resendJob())
		if outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		want := []model.FoundryResendEnd{
			{DocumentID: "doc-a", Reason: model.FoundryResendApproval, Requests: 1},
			{DocumentID: "doc-b", Reason: model.FoundryResendApproval, Requests: 2, Refused: model.FoundryReleaseNotReplayed},
		}
		if !reflect.DeepEqual(h.store.ended, want) {
			t.Fatalf("ended %+v", h.store.ended)
		}
		if h.store.extractions["doc-b"] != nil {
			t.Fatalf("doc-b recorded %+v: its row says what its commit wrote", h.store.extractions["doc-b"])
		}
	})

	// A document deleted since, one whose text is gone, or one the load holds
	// with no extraction to replay is never sent: its approvals are refused,
	// each for its reason, and with nothing left to send no Extract runs.
	t.Run("a document the load can't send refuses its approvals", func(t *testing.T) {
		h := newHarness(t)
		owes(h, "doc-a", "doc-b", "doc-c")
		h.store.resends[0].Document = nil
		h.store.resends[2].Document, h.store.resends[2].Unsendable = nil, model.FoundryReleaseDocumentUnread
		h.bucket.mu.Lock()
		delete(h.bucket.objects, document.Key("ws-1", "doc-b"))
		h.bucket.mu.Unlock()
		outcome := h.run(t, resendJob())
		if outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		if _, calls := h.agent.calls(); len(calls) != 0 {
			t.Fatalf("%d extracts, want none", len(calls))
		}
		want := []model.FoundryResendEnd{
			{DocumentID: "doc-a", Reason: model.FoundryResendApproval, Requests: 1, Refused: model.FoundryReleaseDocumentGone},
			{DocumentID: "doc-c", Reason: model.FoundryResendApproval, Requests: 3, Refused: model.FoundryReleaseDocumentUnread},
			{DocumentID: "doc-b", Reason: model.FoundryResendApproval, Requests: 2, Refused: model.FoundryReleaseDocumentGone},
		}
		if !reflect.DeepEqual(h.store.ended, want) {
			t.Fatalf("ended %+v", h.store.ended)
		}
	})

	// A load that owes nothing, as when another job already sent what this
	// one was queued for, ends at once.
	t.Run("nothing owed sends nothing", func(t *testing.T) {
		h := newHarness(t)
		owes(h)
		if outcome := h.run(t, resendJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		if _, calls := h.agent.calls(); len(calls) != 0 || len(h.store.ended) != 0 {
			t.Fatalf("%d extracts, ended %+v", len(calls), h.store.ended)
		}
	})

	// The engine failing a document it holds changes nothing of what the load
	// records of it: the row its commit wrote stays, and its approvals are
	// refused once the run has ended.
	t.Run("a document the engine fails keeps its row", func(t *testing.T) {
		h := newHarness(t)
		owes(h, "doc-a")
		h.store.decisions = []*model.FoundryReviewDecision{approval("1", "doc-a", hashA)}
		h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
			return extractEvents(stream, []string{
				extracted[0], documentEvent("doc-a", `{"kind":"failed","code":"store","retryable":false}`, noUsage), `{"event":"extract_finished"}`,
			}, `{"status":"done","totals":`+totals+`}`)
		}
		outcome := h.run(t, resendJob())
		if outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		if h.store.extractions["doc-a"] != nil {
			t.Fatalf("recorded %+v over the row its commit wrote", h.store.extractions["doc-a"])
		}
		want := []model.FoundryResendEnd{{DocumentID: "doc-a", Reason: model.FoundryResendApproval, Requests: 1, Refused: model.FoundryReleaseDocumentFailed}}
		if !reflect.DeepEqual(h.store.ended, want) {
			t.Fatalf("ended %+v", h.store.ended)
		}
	})

	// A run that stops (the store failed, the worker was stopped) says
	// nothing of the documents it didn't reach: they stay owed, and the retry
	// sends them again, which writes a release once.
	t.Run("a run that stops ends only what it heard of, and is retried", func(t *testing.T) {
		h := newHarness(t)
		owes(h, "doc-a", "doc-b")
		h.store.decisions = []*model.FoundryReviewDecision{approval("1", "doc-a", hashA), approval("2", "doc-b", hashB)}
		h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
			return extractEvents(stream, []string{
				extracted[0], resent("doc-a", 1, `[{"decision_id":"1","outcome":{"kind":"written"}}]`),
				documentEvent("doc-b", `{"kind":"failed","code":"store","retryable":true}`, noUsage), `{"event":"extract_finished"}`,
			}, `{"status":"store_failed","error":"the database refused a write"}`)
		}
		outcome := h.run(t, resendJob())
		if outcome.RetryAt == nil {
			t.Fatalf("outcome %+v, want a retry", outcome)
		}
		want := []model.FoundryResendEnd{{DocumentID: "doc-a", Reason: model.FoundryResendApproval, Requests: 1}}
		if !reflect.DeepEqual(h.store.ended, want) {
			t.Fatalf("ended %+v", h.store.ended)
		}
		if h.store.extractions["doc-b"] != nil {
			t.Fatalf("doc-b recorded %+v", h.store.extractions["doc-b"])
		}

		// The retry: doc-a's request has ended, so only doc-b is owed. Its
		// release was written before the stop, and the event that said so
		// lost: sent again, the decision finds no review, which is recorded.
		h.store.resends = h.store.resends[1:]
		h.store.ended = nil
		h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
			return extractEvents(stream, []string{
				extracted[0], resent("doc-b", 0, `[{"decision_id":"2","outcome":{"kind":"not_held"}}]`), `{"event":"extract_finished"}`,
			}, `{"status":"done","totals":`+totals+`}`)
		}
		job := resendJob()
		job.Attempts = 2
		if outcome := h.run(t, job); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		_, calls := h.agent.calls()
		if req := decodeExtract(t, calls[len(calls)-1]); len(req.Documents) != 1 || req.Documents[0].DocID != "doc-b" {
			t.Fatalf("the retry sent %+v", req.Documents)
		}
		wantB := []model.FoundryReleaseOutcome{{DecisionID: "2", StatementHash: hashB, Kind: model.FoundryReleaseOutcomeNotHeld}}
		if got := h.store.extractions["doc-b"]; got == nil || !reflect.DeepEqual(got.Releases, wantB) {
			t.Fatalf("doc-b recorded %+v", got)
		}
		if want := []model.FoundryResendEnd{{DocumentID: "doc-b", Reason: model.FoundryResendApproval, Requests: 2}}; !reflect.DeepEqual(h.store.ended, want) {
			t.Fatalf("ended %+v", h.store.ended)
		}
	})

	// The engine ends a document and says nothing of a decision sent with it,
	// which the contract has it do: the decision is refused as unanswered
	// rather than left waiting, and what it did say of another stands.
	t.Run("a decision the engine says nothing of is refused, not left waiting", func(t *testing.T) {
		h := newHarness(t)
		owes(h, "doc-a")
		second := approval("2", "doc-a", hashB)
		second.ClaimSpan = &model.FoundryClaimSpan{Section: 1, Start: 0, End: 4}
		h.store.decisions = []*model.FoundryReviewDecision{approval("1", "doc-a", hashA), second, approval("3", "doc-b", hashA)}
		h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
			return extractEvents(stream, []string{
				extracted[0], resent("doc-a", 1, `[{"decision_id":"1","outcome":{"kind":"written"}},{"decision_id":"3","outcome":{"kind":"written"}}]`),
				`{"event":"extract_finished"}`,
			}, `{"status":"done","totals":`+totals+`}`)
		}
		if outcome := h.run(t, resendJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		want := []model.FoundryReleaseOutcome{
			{DecisionID: "1", StatementHash: hashA, Kind: model.FoundryReleaseOutcomeWritten},
			{DecisionID: "2", StatementHash: hashB, Kind: model.FoundryReleaseOutcomeRefused, Reason: model.FoundryReleaseUnanswered},
		}
		if got := h.store.extractions["doc-a"]; got == nil || !reflect.DeepEqual(got.Releases, want) {
			t.Fatalf("recorded %+v", got)
		}
	})

	// Another worker took the job: nothing more is recorded, and the attempt
	// is retried, which the claim that took it makes moot.
	t.Run("a lost lease ends the attempt", func(t *testing.T) {
		h := newHarness(t)
		owes(h, "doc-a")
		h.store.endErr = foundrystore.ErrNotFound
		h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
			return extractEvents(stream, []string{extracted[0], resent("doc-a", 0, `[]`), `{"event":"extract_finished"}`},
				`{"status":"done","totals":`+totals+`}`)
		}
		outcome := h.run(t, resendJob())
		if outcome.RetryAt == nil || !strings.Contains(outcome.Error, "lease was lost") {
			t.Fatalf("%+v", outcome)
		}
	})

	// A database that fails to end a request leaves it owed: the attempt is
	// retried rather than end a job whose documents still wait.
	t.Run("a failed end is retried", func(t *testing.T) {
		h := newHarness(t)
		owes(h, "doc-a")
		h.store.resends[0].Document = nil
		h.store.endErr = errors.New("database is down")
		if outcome := h.run(t, resendJob()); outcome.RetryAt == nil {
			t.Fatalf("%+v", outcome)
		}
	})
}

// massApproved gives each of docs n approvals in load-0's list, each of a
// claim of its own, with ids in the list's order.
func massApproved(h *harness, n int, docs ...string) {
	for i, doc := range docs {
		for j := range n {
			d := approval(fmt.Sprintf("%02d-%05d", i, j), doc, hashA)
			d.ClaimSpan = &model.FoundryClaimSpan{Section: uint32(j), Start: 2, End: 9}
			h.store.decisions = append(h.store.decisions, d)
		}
	}
}

// writesAll answers a resend's Extract as an engine that writes every release
// it was sent: an event per document, saying so of each of its decisions.
func writesAll(t *testing.T) func(string, agentv1.Agent_ExtractServer) error {
	return func(raw string, stream agentv1.Agent_ExtractServer) error {
		req := decodeExtract(t, raw)
		events := []string{extracted[0]}
		for _, doc := range req.Documents {
			var releases []string
			for _, d := range req.Decisions {
				if d.DocID == doc.DocID {
					releases = append(releases, `{"decision_id":"`+d.DecisionID+`","outcome":{"kind":"written"}}`)
				}
			}
			events = append(events, resent(doc.DocID, len(releases), "["+strings.Join(releases, ",")+"]"))
		}
		return extractEvents(stream, append(events, `{"event":"extract_finished"}`), `{"status":"done","totals":`+totals+`}`)
	}
}

// A mass of approvals, as the first carry after a deploy or a large Approve
// all brings, is past what one Extract takes in decisions. The resend sends
// its documents in as few Extracts as take them, in the load's order, rather
// than fail and leave every approval unconfirmed.
func TestResendSplitsWhatOneExtractCannotTake(t *testing.T) {
	t.Run("more decisions than one Extract takes go in two", func(t *testing.T) {
		h := newHarness(t)
		owes(h, "doc-a", "doc-b", "doc-c")
		massApproved(h, 3400, "doc-a", "doc-b", "doc-c")
		h.agent.extract = writesAll(t)
		outcome := h.run(t, resendJob())
		if outcome.Status != model.FoundryGraphJobSucceeded || outcome.Error != "" {
			t.Fatalf("%+v", outcome)
		}
		_, calls := h.agent.calls()
		if len(calls) != 2 {
			t.Fatalf("%d extracts, want two", len(calls))
		}
		first, second := decodeExtract(t, calls[0]), decodeExtract(t, calls[1])
		if first.RequestID != "job-4" || second.RequestID != "job-4:2" {
			t.Fatalf("request ids %q and %q", first.RequestID, second.RequestID)
		}
		if got := [][]string{docIDs(first), docIDs(second)}; !reflect.DeepEqual(got, [][]string{{"doc-a"}, {"doc-b", "doc-c"}}) {
			t.Fatalf("sent %v", got)
		}
		// Each Extract carries the decisions of its own documents only.
		if len(first.Decisions) != 3400 || len(second.Decisions) != 6800 {
			t.Fatalf("%d and %d decisions", len(first.Decisions), len(second.Decisions))
		}
		if first.Limits.MaxUSD != 0 || second.Limits.MaxUSD != 0 {
			t.Fatalf("max_usd %v and %v", first.Limits.MaxUSD, second.Limits.MaxUSD)
		}
		want := []model.FoundryResendEnd{
			{DocumentID: "doc-a", Reason: model.FoundryResendApproval, Requests: 1},
			{DocumentID: "doc-b", Reason: model.FoundryResendApproval, Requests: 2},
			{DocumentID: "doc-c", Reason: model.FoundryResendApproval, Requests: 3},
		}
		if !reflect.DeepEqual(h.store.ended, want) {
			t.Fatalf("ended %+v", h.store.ended)
		}
		for _, doc := range []string{"doc-a", "doc-b", "doc-c"} {
			if got := h.store.extractions[doc]; got == nil || len(got.Releases) != 3400 {
				t.Fatalf("%s recorded %+v", doc, got)
			}
		}
		// An Extract too large to send counts no document.
		if last := h.store.lastUpdate(); last.Progress.DocumentsDone != 3 || last.Progress.DocumentsTotal != 3 {
			t.Fatalf("last update %+v", last)
		}
		for _, call := range []string{"resend", "resend:2"} {
			if gjson(t, string(outcome.Report), call) == "" {
				t.Fatalf("the report lacks %s: %s", call, outcome.Report)
			}
		}
	})

	t.Run("a request past the agent's message limit goes in halves", func(t *testing.T) {
		h := newHarness(t)
		owes(h, "doc-a", "doc-b", "doc-c", "doc-d")
		massApproved(h, 40, "doc-a", "doc-b", "doc-c", "doc-d")
		// What all four documents' Extract weighs, less a byte: two halves fit.
		h.agent.extract = writesAll(t)
		if outcome := h.run(t, resendJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		_, whole := h.agent.calls()
		if len(whole) != 1 {
			t.Fatalf("%d extracts, want one", len(whole))
		}

		h = newHarness(t)
		owes(h, "doc-a", "doc-b", "doc-c", "doc-d")
		massApproved(h, 40, "doc-a", "doc-b", "doc-c", "doc-d")
		h.agent.extract = writesAll(t)
		h.task.runner.maxRequestBytes = len(whole[0]) - 1
		if outcome := h.run(t, resendJob()); outcome.Status != model.FoundryGraphJobSucceeded {
			t.Fatalf("%+v", outcome)
		}
		_, calls := h.agent.calls()
		if len(calls) != 2 {
			t.Fatalf("%d extracts, want two", len(calls))
		}
		if got := [][]string{docIDs(decodeExtract(t, calls[0])), docIDs(decodeExtract(t, calls[1]))}; !reflect.DeepEqual(got, [][]string{{"doc-a", "doc-b"}, {"doc-c", "doc-d"}}) {
			t.Fatalf("sent %v", got)
		}
		if len(h.store.ended) != 4 {
			t.Fatalf("ended %+v", h.store.ended)
		}
	})

	// One document whose own decisions are past the cap can't be split, and
	// is not sent: its approvals are refused as too large, and the documents
	// before and after it in the load's order go as they would without it.
	t.Run("a single document too large is refused and the rest is sent", func(t *testing.T) {
		h := newHarness(t)
		owes(h, "doc-a", "doc-b", "doc-c")
		massApproved(h, 10, "doc-a")
		massApproved(h, maxExtractDecisions+1, "doc-b")
		for _, d := range h.store.decisions[10:] {
			d.ID = "1" + d.ID[1:]
		}
		massApproved(h, 10, "doc-c")
		for _, d := range h.store.decisions[10+maxExtractDecisions+1:] {
			d.ID = "2" + d.ID[1:]
		}
		h.agent.extract = writesAll(t)
		outcome := h.run(t, resendJob())
		if outcome.Status != model.FoundryGraphJobSucceeded || outcome.Error != "" {
			t.Fatalf("%+v", outcome)
		}
		_, calls := h.agent.calls()
		if len(calls) != 2 {
			t.Fatalf("%d extracts, want two", len(calls))
		}
		if got := [][]string{docIDs(decodeExtract(t, calls[0])), docIDs(decodeExtract(t, calls[1]))}; !reflect.DeepEqual(got, [][]string{{"doc-a"}, {"doc-c"}}) {
			t.Fatalf("sent %v", got)
		}
		want := []model.FoundryResendEnd{
			{DocumentID: "doc-a", Reason: model.FoundryResendApproval, Requests: 1},
			{DocumentID: "doc-b", Reason: model.FoundryResendApproval, Requests: 2, Refused: model.FoundryReleaseTooLarge},
			{DocumentID: "doc-c", Reason: model.FoundryResendApproval, Requests: 3},
		}
		if !reflect.DeepEqual(h.store.ended, want) {
			t.Fatalf("ended %+v", h.store.ended)
		}
		if h.store.extractions["doc-b"] != nil {
			t.Fatalf("doc-b recorded %+v, though it was never sent", h.store.extractions["doc-b"])
		}
		for _, doc := range []string{"doc-a", "doc-c"} {
			if got := h.store.extractions[doc]; got == nil || len(got.Releases) != 10 {
				t.Fatalf("%s recorded %+v", doc, got)
			}
		}
	})
}

// What the engine says of a release decision is recorded whatever job sent
// its document: a load's commit says written, refused or missed of the
// decisions the load copied. One the worker didn't send is not recorded.
func TestExtractRecordsEachRelease(t *testing.T) {
	h := newHarness(t)
	h.store.decisions = []*model.FoundryReviewDecision{approval("1", "doc-a", hashA), approval("2", "doc-a", hashB)}
	h.store.decisions[1].ClaimSpan = &model.FoundryClaimSpan{Section: 1, Start: 0, End: 4}
	h.agent.extract = func(_ string, stream agentv1.Agent_ExtractServer) error {
		a := strings.TrimSuffix(documentEvent("doc-a", `{"kind":"ingested"}`, noUsage), `"releases":[]}`) + `"releases":[{"decision_id":"1","outcome":{"kind":"missed"}},` +
			`{"decision_id":"2","outcome":{"kind":"refused","reason":"a_reason_of_a_later_engine"}},{"decision_id":"9","outcome":{"kind":"written"}}]}`
		events := []string{extracted[0], a}
		for _, doc := range []string{"doc-b", "doc-c", "doc-d"} {
			events = append(events, documentEvent(doc, `{"kind":"out_of_scope"}`, noUsage))
		}
		return extractEvents(stream, append(events, `{"event":"extract_finished"}`), `{"status":"done","totals":`+totals+`}`)
	}
	if outcome := h.run(t, extractJob()); outcome.Status != model.FoundryGraphJobSucceeded {
		t.Fatalf("%+v", outcome)
	}
	want := []model.FoundryReleaseOutcome{
		{DecisionID: "1", StatementHash: hashA, Kind: model.FoundryReleaseOutcomeMissed},
		{DecisionID: "2", StatementHash: hashB, Kind: model.FoundryReleaseOutcomeRefused, Reason: "a_reason_of_a_later_engine"},
	}
	if got := h.store.extractions["doc-a"]; got == nil || !reflect.DeepEqual(got.Releases, want) {
		t.Fatalf("recorded %+v", got)
	}
	if len(h.store.ended) != 0 {
		t.Fatalf("an extract ended resends: %+v", h.store.ended)
	}
}

// A document event whose release names no decision, or refuses without a
// reason, is one this contract doesn't write: the job fails rather than guess.
func TestDocumentEventReleases(t *testing.T) {
	for name, releases := range map[string]string{
		"no decision":       `[{"decision_id":"","outcome":{"kind":"written"}}]`,
		"no outcome":        `[{"decision_id":"1","outcome":{}}]`,
		"refusal no reason": `[{"decision_id":"1","outcome":{"kind":"refused"}}]`,
		"reason no refusal": `[{"decision_id":"1","outcome":{"kind":"written","reason":"unresolved"}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ontologyagent.DecodeDocumentEvent([]byte(resent("doc-a", 0, releases))); err == nil {
				t.Fatal("decoded")
			}
		})
	}
	e, err := ontologyagent.DecodeDocumentEvent([]byte(resent("doc-a", 1, `[{"decision_id":"1","outcome":{"kind":"written"}}]`)))
	if err != nil || len(e.Releases) != 1 || e.Releases[0].Outcome.Kind != model.FoundryReleaseOutcomeWritten || e.Counts.Released != 1 {
		t.Fatalf("%+v, %v", e, err)
	}
}
