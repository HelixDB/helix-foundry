package api

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/helixdb/helix-foundry/backend/access"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

var (
	insightFrom = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ada         = access.Principal{Kind: access.PrincipalUser, ID: "user-ada"}
	supportKey  = access.Principal{Kind: access.PrincipalServiceCredential, ID: "cred-support"}
)

// insightRun is a finished run of wf-a by Ada at `at`, which called the
// ontology once.
func insightRun(id string, at time.Time) foundrymodel.FoundryWorkflowInsightRun {
	return foundrymodel.FoundryWorkflowInsightRun{ID: id, WorkflowID: "wf-a", StartedBy: ada, StartedAt: at,
		Status: foundrymodel.FoundryWorkflowRunFinished, Counts: foundrymodel.FoundryWorkflowRunCounts{Accesses: 1, Reads: 1}}
}

// assessedAs gives r a done assessment with verdict, saying body.
func assessedAs(r foundrymodel.FoundryWorkflowInsightRun, verdict string, body any) foundrymodel.FoundryWorkflowInsightRun {
	raw, _ := json.Marshal(body)
	r.Assessment = &foundrymodel.FoundryWorkflowInsightAssessment{Status: foundrymodel.FoundryWorkflowAssessmentDone, Verdict: verdict,
		Assessment: raw}
	return r
}

// gapsSaid is an assessment body naming gaps, each kind then text.
func gapsSaid(kindText ...string) map[string]any {
	var out []map[string]string
	for i := 0; i+1 < len(kindText); i += 2 {
		out = append(out, map[string]string{"kind": kindText[i], "what": kindText[i+1]})
	}
	return map[string]any{"gaps": out}
}

// A window is `days` whole UTC days from its first instant; a run at that
// instant is in it, one an instant before is the previous window's, and one
// at the end is in neither.
func TestWorkflowInsightsWindow(t *testing.T) {
	runs := []foundrymodel.FoundryWorkflowInsightRun{
		insightRun("first", insightFrom),
		insightRun("last", insightFrom.Add(3*24*time.Hour-time.Millisecond)),
		insightRun("after", insightFrom.Add(3*24*time.Hour)),
	}
	previous := []foundrymodel.FoundryWorkflowInsightRun{
		insightRun("before", insightFrom.Add(-time.Millisecond)),
		insightRun("long ago", insightFrom.AddDate(0, 0, -4)),
	}
	got := workflowInsights(runs, previous, insightFrom, 3, true)
	if got.GetFrom() != "2026-11-01" || got.GetDays() != 3 || got.GetTotals().GetRuns() != 2 || got.GetPrevious().GetRuns() != 1 {
		t.Fatalf("window = %s %d, %d runs, %d before", got.GetFrom(), got.GetDays(), got.GetTotals().GetRuns(), got.GetPrevious().GetRuns())
	}
	var dates []string
	var perDay []int32
	for _, d := range got.GetSeries() {
		dates = append(dates, d.GetDate())
		perDay = append(perDay, d.GetTotals().GetRuns())
	}
	if !slices.Equal(dates, []string{"2026-11-01", "2026-11-02", "2026-11-03"}) || !slices.Equal(perDay, []int32{1, 0, 1}) {
		t.Fatalf("series = %v %v", dates, perDay)
	}
	if got.GetTruncated() {
		t.Fatal("truncated")
	}
}

// Totals count runs as they stand, and claims, verdicts and gap kinds of
// done assessments only; the funnel's steps narrow.
func TestWorkflowInsightsTotalsAndFunnel(t *testing.T) {
	at := insightFrom.Add(time.Hour)
	a := assessedAs(insightRun("a", at), "essential", map[string]any{
		"claims":      []map[string]string{{"support": "supported"}, {"support": "supported"}, {"support": "unsupported"}},
		"gaps":        []map[string]string{{"kind": "missing_property", "what": "No health score."}},
		"suggestions": []string{"Add a health score"},
	})
	a.Counts = foundrymodel.FoundryWorkflowRunCounts{Accesses: 2, Reads: 1, Questions: 1}
	a.Assessment.USDMicros = 100
	b := assessedAs(insightRun("b", at), "unused", map[string]any{
		"claims": []map[string]string{{"support": "contradicted"}},
		// A gap without its kind is other's; one without text isn't one.
		"gaps": []map[string]any{{"what": "Calls not read"}, {"kind": "missing_link", "what": "  "}},
	})
	b.Counts = foundrymodel.FoundryWorkflowRunCounts{}
	b.Assessment.USDMicros = 50
	c := insightRun("c", at)
	c.Counts = foundrymodel.FoundryWorkflowRunCounts{Accesses: 1, Sends: 1}
	c.Assessment = &foundrymodel.FoundryWorkflowInsightAssessment{Status: foundrymodel.FoundryWorkflowAssessmentQueued}
	d := insightRun("d", at)
	d.Status = foundrymodel.FoundryWorkflowRunOpen
	d.Counts = foundrymodel.FoundryWorkflowRunCounts{Accesses: 3, Uploads: 3}
	e := insightRun("e", at)
	e.Counts = foundrymodel.FoundryWorkflowRunCounts{Accesses: 1}
	e.Assessment = &foundrymodel.FoundryWorkflowInsightAssessment{Status: foundrymodel.FoundryWorkflowAssessmentDone, Verdict: "helpful",
		Assessment: json.RawMessage(`not json`), USDMicros: 25}
	f := insightRun("f", at)
	f.Counts = foundrymodel.FoundryWorkflowRunCounts{Accesses: 1}
	f.Assessment = &foundrymodel.FoundryWorkflowInsightAssessment{Status: foundrymodel.FoundryWorkflowAssessmentFailed, USDMicros: 10}

	got := workflowInsights([]foundrymodel.FoundryWorkflowInsightRun{a, b, c, d, e, f}, nil, insightFrom, 7, true)
	tot := got.GetTotals()
	if tot.GetRuns() != 6 || tot.GetOpen() != 1 || tot.GetFinished() != 5 || tot.GetAssessed() != 3 || tot.GetHelped() != 2 || tot.GetUsed() != 5 {
		t.Fatalf("runs %d, open %d, finished %d, assessed %d, helped %d, used %d", tot.GetRuns(), tot.GetOpen(), tot.GetFinished(),
			tot.GetAssessed(), tot.GetHelped(), tot.GetUsed())
	}
	if tot.GetClaimsSupported() != 2 || tot.GetClaimsUnsupported() != 1 || tot.GetClaimsContradicted() != 1 {
		t.Fatalf("claims %d/%d/%d", tot.GetClaimsSupported(), tot.GetClaimsUnsupported(), tot.GetClaimsContradicted())
	}
	if tot.GetReads() != 1 || tot.GetQuestions() != 1 || tot.GetSends() != 1 || tot.GetUploads() != 3 || tot.GetAssessmentUsdMicros() != 185 {
		t.Fatalf("calls %d/%d/%d/%d, spend %d", tot.GetReads(), tot.GetQuestions(), tot.GetSends(), tot.GetUploads(), tot.GetAssessmentUsdMicros())
	}
	var verdicts, kinds []string
	for _, v := range tot.GetVerdicts() {
		verdicts = append(verdicts, fmt.Sprintf("%s:%d", v.GetVerdict(), v.GetRuns()))
	}
	for _, k := range tot.GetGapKinds() {
		kinds = append(kinds, fmt.Sprintf("%s:%d", k.GetKind(), k.GetRuns()))
	}
	if !slices.Equal(verdicts, []string{"essential:1", "helpful:1", "unused:1"}) || !slices.Equal(kinds, []string{"missing_property:1", "other:1"}) {
		t.Fatalf("verdicts %v, gap kinds %v", verdicts, kinds)
	}
	fu := got.GetFunnel()
	steps := []int32{fu.GetStarted(), fu.GetFinished(), fu.GetAssessed(), fu.GetUsed(), fu.GetHelped()}
	if !slices.Equal(steps, []int32{6, 5, 3, 2, 2}) {
		t.Fatalf("funnel = %v", steps)
	}
}

// Runs naming one gap, case, spacing and end punctuation aside, count once
// each under the newest run's words; ties go to the newest, then the text.
func TestWorkflowInsightsRecurring(t *testing.T) {
	newest := assessedAs(insightRun("new", insightFrom.Add(3*time.Hour)), "helpful", gapsSaid("missing_property", "No health score on Customer"))
	newest.WorkflowID = "wf-b"
	mid := assessedAs(insightRun("mid", insightFrom.Add(2*time.Hour)), "marginal",
		gapsSaid("missing_data", "  no HEALTH   score on customer.  ", "wrong_data", "Contract terms disagree"))
	old := assessedAs(insightRun("old", insightFrom.Add(time.Hour)), "marginal",
		gapsSaid("missing_property", "No health score on customer!", "missing_property", "No health score on customer", "other", "A"))
	got := workflowInsights([]foundrymodel.FoundryWorkflowInsightRun{old, newest, mid}, nil, insightFrom, 1, true).GetGaps()
	if len(got) != 3 {
		t.Fatalf("gaps = %v", got)
	}
	g := got[0]
	if g.GetText() != "No health score on Customer" || g.GetKind() != "missing_property" || g.GetRuns() != 3 || g.GetLastRunId() != "new" ||
		g.GetLastWorkflowId() != "wf-b" || !g.GetLastAt().AsTime().Equal(newest.StartedAt) || !slices.Equal(g.GetWorkflowIds(), []string{"wf-a", "wf-b"}) {
		t.Fatalf("top gap = %v", g)
	}
	if got[1].GetText() != "Contract terms disagree" || got[2].GetText() != "A" {
		t.Fatalf("one run each, newest first: %v, %v", got[1], got[2])
	}

	// Suggestions group the same way, kind aside; at most 10 recur.
	var many []string
	for i := range 12 {
		many = append(many, fmt.Sprintf("Suggestion %02d", i))
	}
	one := assessedAs(insightRun("one", insightFrom), "helpful", map[string]any{"suggestions": many})
	two := assessedAs(insightRun("two", insightFrom.Add(time.Minute)), "helpful", map[string]any{"suggestions": []string{"suggestion 05."}})
	sug := workflowInsights([]foundrymodel.FoundryWorkflowInsightRun{one, two}, nil, insightFrom, 1, true).GetSuggestions()
	if len(sug) != 10 || sug[0].GetText() != "suggestion 05." || sug[0].GetRuns() != 2 || sug[0].GetKind() != "" || sug[1].GetText() != "Suggestion 00" {
		t.Fatalf("suggestions = %v", sug)
	}
}

// Segments: who started runs, most runs first and at most 10, each with its
// totals, runs by day, newest run and top gap; workflows only when asked.
func TestWorkflowInsightsSegments(t *testing.T) {
	var runs []foundrymodel.FoundryWorkflowInsightRun
	add := func(id string, by access.Principal, workflow string, day int) {
		r := assessedAs(insightRun(id, insightFrom.AddDate(0, 0, day).Add(time.Hour)), "helpful", gapsSaid("missing_link", "No link from "+id))
		r.StartedBy, r.WorkflowID = by, workflow
		runs = append(runs, r)
	}
	add("a1", ada, "wf-a", 0)
	add("a2", ada, "wf-a", 1)
	add("a3", ada, "wf-b", 1)
	add("k1", supportKey, "wf-a", 0)
	add("k2", supportKey, "wf-b", 1)
	add("b1", access.Principal{Kind: access.PrincipalUser, ID: "user-bob"}, "wf-a", 0)
	got := workflowInsights(runs, nil, insightFrom, 2, true)
	agents := got.GetAgents()
	if len(agents) != 3 || agents[0].GetPrincipal().GetId() != "user-ada" || agents[1].GetPrincipal().GetKind() != foundryv1.FoundryPrincipal_KIND_SERVICE_CREDENTIAL ||
		agents[2].GetPrincipal().GetId() != "user-bob" {
		t.Fatalf("agents = %v", agents)
	}
	if a := agents[0]; a.GetTotals().GetRuns() != 3 || !slices.Equal(a.GetDailyRuns(), []int32{1, 2}) || a.GetTopGap() == "" ||
		!a.GetLastRunAt().AsTime().Equal(insightFrom.AddDate(0, 0, 1).Add(time.Hour)) || a.GetWorkflowId() != "" {
		t.Fatalf("ada = %v", a)
	}
	workflows := got.GetWorkflows()
	if len(workflows) != 2 || workflows[0].GetWorkflowId() != "wf-a" || workflows[0].GetTotals().GetRuns() != 4 || workflows[0].GetPrincipal() != nil {
		t.Fatalf("workflows = %v", workflows)
	}
	if one := workflowInsights(runs, nil, insightFrom, 2, false); len(one.GetWorkflows()) != 0 {
		t.Fatalf("one workflow's insights list workflows: %v", one.GetWorkflows())
	}

	var crowd []foundrymodel.FoundryWorkflowInsightRun
	for i := range 12 {
		r := insightRun(fmt.Sprintf("r%d", i), insightFrom)
		r.StartedBy = access.Principal{Kind: access.PrincipalUser, ID: fmt.Sprintf("user-%d", i)}
		crowd = append(crowd, r)
	}
	if n := len(workflowInsights(crowd, nil, insightFrom, 1, true).GetAgents()); n != insightAgents {
		t.Fatalf("%d agents", n)
	}
}

// Past insightRuns, the newest are read and the answer says so.
func TestWorkflowInsightsTruncated(t *testing.T) {
	runs := make([]foundrymodel.FoundryWorkflowInsightRun, insightRuns)
	for i := range runs {
		runs[i] = insightRun(fmt.Sprintf("r%05d", i), insightFrom.Add(time.Duration(i)*time.Second))
	}
	if got := workflowInsights(runs, nil, insightFrom, 1, true); got.GetTruncated() || got.GetTotals().GetRuns() != insightRuns {
		t.Fatalf("at the cap: truncated %v, %d runs", got.GetTruncated(), got.GetTotals().GetRuns())
	}
	runs = append(runs, insightRun("oldest", insightFrom))
	got := workflowInsights(runs, nil, insightFrom, 1, true)
	if !got.GetTruncated() || got.GetTotals().GetRuns() != insightRuns {
		t.Fatalf("past the cap: truncated %v, %d runs", got.GetTruncated(), got.GetTotals().GetRuns())
	}
}

func TestGetFoundryWorkflowInsights(t *testing.T) {
	s, store := workflowService(t)
	run := startRun(t, s, "q")
	ctx := asPerson()
	req := func(workflowID string, days int32, by *foundryv1.FoundryPrincipal) *foundryv1.GetFoundryWorkflowInsightsRequest {
		return &foundryv1.GetFoundryWorkflowInsightsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: workflowID, Days: days, StartedBy: by}
	}

	all := must(s.GetFoundryWorkflowInsights(ctx, req("", 0, nil)))
	if all.GetDays() != insightDays || all.GetTotals().GetRuns() != 1 || len(all.GetWorkflows()) != 1 ||
		all.GetAgents()[0].GetPrincipal().GetId() != person.UserID || len(all.GetSeries()) != insightDays {
		t.Fatalf("insights = %v", all)
	}
	if today := time.Now().UTC().Format(time.DateOnly); all.GetSeries()[insightDays-1].GetDate() != today {
		t.Fatalf("the window ends %s, not today %s", all.GetSeries()[insightDays-1].GetDate(), today)
	}
	one := must(s.GetFoundryWorkflowInsights(ctx, req(run.GetWorkflowId(), 7, nil)))
	if one.GetDays() != 7 || one.GetTotals().GetRuns() != 1 || len(one.GetWorkflows()) != 0 {
		t.Fatalf("one workflow's = %v", one)
	}
	key := &foundryv1.FoundryPrincipal{Kind: foundryv1.FoundryPrincipal_KIND_SERVICE_CREDENTIAL, Id: "cred-x"}
	if theirs := must(s.GetFoundryWorkflowInsights(ctx, req("", 7, key))); theirs.GetTotals().GetRuns() != 0 {
		t.Fatalf("another's runs = %v", theirs.GetTotals())
	}
	for name, tc := range map[string]struct {
		req  *foundryv1.GetFoundryWorkflowInsightsRequest
		want codes.Code
	}{
		"days past 90":     {req("", 91, nil), codes.InvalidArgument},
		"days below 0":     {req("", -1, nil), codes.InvalidArgument},
		"a team":           {req("", 7, &foundryv1.FoundryPrincipal{Kind: foundryv1.FoundryPrincipal_KIND_TEAM, Id: "t-1"}), codes.InvalidArgument},
		"no one's id":      {req("", 7, &foundryv1.FoundryPrincipal{Kind: foundryv1.FoundryPrincipal_KIND_USER}), codes.InvalidArgument},
		"another workflow": {req("nope", 7, nil), codes.NotFound},
		"another ontology": {&foundryv1.GetFoundryWorkflowInsightsRequest{WorkspaceId: "ws-1", OntologyId: "ont-9"}, codes.NotFound},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := s.GetFoundryWorkflowInsights(ctx, tc.req)
			wantCode(t, err, tc.want)
		})
	}

	// A run started before the window is the window before's.
	store.runs[0].StartedAt = time.Now().AddDate(0, 0, -35)
	moved := must(s.GetFoundryWorkflowInsights(ctx, req("", 30, nil)))
	if moved.GetTotals().GetRuns() != 0 || moved.GetPrevious().GetRuns() != 1 {
		t.Fatalf("moved = %v / %v", moved.GetTotals(), moved.GetPrevious())
	}
}

// A grantee holding part of the ontology reads only its own runs, whoever
// it names; one holding all of it reads every run, or one starter's.
func TestGranteeWorkflowInsights(t *testing.T) {
	s, acc := accessService(t)
	store := newWorkflowStore(acc)
	s.deps.DB = store
	conn, as := served(t, s)
	client := foundryv1.NewFoundryServiceClient(conn)
	owners := startRun(t, s, "the owner's")
	for _, who := range []ports.Actor{agent, member} {
		if _, err := client.StartFoundryWorkflowRun(as(who), &foundryv1.StartFoundryWorkflowRunRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: owners.GetWorkflowId(), Input: "mine",
		}); err != nil {
			t.Fatal(err)
		}
	}
	runs := func(who ports.Actor, by *foundryv1.FoundryPrincipal) int32 {
		t.Helper()
		got, err := client.GetFoundryWorkflowInsights(as(who), &foundryv1.GetFoundryWorkflowInsightsRequest{
			WorkspaceId: "ws-1", OntologyId: "ont-1", StartedBy: by,
		})
		if err != nil {
			t.Fatal(err)
		}
		return got.GetTotals().GetRuns()
	}
	owner := &foundryv1.FoundryPrincipal{Kind: foundryv1.FoundryPrincipal_KIND_USER, Id: person.UserID}
	if n := runs(member, nil); n != 1 {
		t.Fatalf("user-9 reads %d runs, want its own", n)
	}
	if n := runs(member, owner); n != 1 {
		t.Fatalf("user-9 naming the owner reads %d runs, want its own", n)
	}
	if n := runs(agent, nil); n != 3 {
		t.Fatalf("the agent's key reads %d runs, want every run", n)
	}
	if n := runs(agent, owner); n != 1 {
		t.Fatalf("the agent's key naming the owner reads %d runs", n)
	}
	acc.workflowsOff = []access.Principal{agentKey}
	_, err := client.GetFoundryWorkflowInsights(as(agent), &foundryv1.GetFoundryWorkflowInsightsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})
	wantCode(t, err, codes.PermissionDenied)
}
