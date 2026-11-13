package api

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/access"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// What runs of an ontology's workflows came to over a window of whole UTC
// days (GetFoundryWorkflowInsights): the site's Workflows pages lead with it.
// Runs count by the day they started; a done assessment's claims, gaps and
// suggestions count; a gap or suggestion recurs when runs name the same text,
// case, spacing and end punctuation aside. The mock serves the same
// (mock/routes/workflow-insights.ts).
const (
	// insightRuns is the most runs of a window insights read: the newest.
	insightRuns = 5000
	// insightIssues is the most recurring gaps, and suggestions, answered.
	insightIssues = 10
	// insightAgents is the most of those who started runs answered.
	insightAgents = 10
	// insightDays is a window's length when the request names none, and
	// insightMaxDays the longest.
	insightDays    = 30
	insightMaxDays = 90
)

// insightGapKinds are the engine's gap kinds (assess_result.json GapKind),
// in the order insights list them.
var insightGapKinds = []string{"missing_type", "missing_link", "missing_property", "missing_data", "wrong_data", "other"}

// GetFoundryWorkflowInsights says what the runs of a live ontology's
// workflows (of one, when the request names it) that started in the last
// `days` UTC days came to, and the window before. A grantee who sees only
// its own runs reads only those, whoever the request names.
func (s *Service) GetFoundryWorkflowInsights(ctx context.Context, req *foundryv1.GetFoundryWorkflowInsightsRequest) (*foundryv1.FoundryWorkflowInsights, error) {
	days := int(req.GetDays())
	switch {
	case days < 0 || days > insightMaxDays:
		return nil, status.Errorf(codes.InvalidArgument, "days is 1 to %d", insightMaxDays)
	case days == 0:
		days = insightDays
	}
	var startedBy *access.Principal
	if p := req.GetStartedBy(); p.GetKind() != foundryv1.FoundryPrincipal_KIND_UNSPECIFIED || p.GetId() != "" {
		principal, err := principalFromProto(p)
		if err != nil || principal.Kind == access.PrincipalTeam {
			return nil, status.Error(codes.InvalidArgument, "started_by names a user or a service credential, and its id")
		}
		startedBy = &principal
	}
	if by, own := ownRunsOnly(ctx); own {
		startedBy = &by
	}
	o, err := s.liveFoundryOntology(ctx, req.GetWorkspaceId(), req.GetOntologyId())
	if err != nil {
		return nil, err
	}
	if req.GetWorkflowId() != "" {
		if _, err := s.foundryWorkflow(ctx, o.ID, req.GetWorkflowId()); err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	from := time.Date(now.Year(), now.Month(), now.Day()+1-days, 0, 0, 0, 0, time.UTC)
	runs, err := s.deps.DB.ListFoundryWorkflowInsightRuns(ctx, o.ID, req.GetWorkflowId(), startedBy, from, from.AddDate(0, 0, days), insightRuns+1)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list workflow runs: %v", err)
	}
	previous, err := s.deps.DB.ListFoundryWorkflowInsightRuns(ctx, o.ID, req.GetWorkflowId(), startedBy, from.AddDate(0, 0, -days), from, insightRuns)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list workflow runs: %v", err)
	}
	return workflowInsights(runs, previous, from, days, req.GetWorkflowId() == ""), nil
}

// insightAssessed is what a done assessment says: its claims' support, its
// gaps (kind and text) and its suggestions, read without trust.
type insightAssessed struct {
	claims      []string
	gaps        []insightIssue
	suggestions []insightIssue
}

// insightIssue is a gap (with its kind) or a suggestion a run named.
type insightIssue struct{ kind, text string }

// assessedOf reads r's assessment once it is done; nothing for any other,
// or one that isn't the engine's JSON.
func assessedOf(r *foundrymodel.FoundryWorkflowInsightRun) insightAssessed {
	var out insightAssessed
	a := r.Assessment
	if a == nil || a.Status != foundrymodel.FoundryWorkflowAssessmentDone || len(a.Assessment) == 0 {
		return out
	}
	var body struct {
		Claims []struct {
			Support any `json:"support"`
		} `json:"claims"`
		Gaps []struct {
			Kind any `json:"kind"`
			What any `json:"what"`
		} `json:"gaps"`
		Suggestions []any `json:"suggestions"`
	}
	if json.Unmarshal(a.Assessment, &body) != nil {
		return out
	}
	for _, c := range body.Claims {
		if support, ok := c.Support.(string); ok {
			out.claims = append(out.claims, support)
		}
	}
	for _, g := range body.Gaps {
		what, ok := g.What.(string)
		if !ok || strings.TrimSpace(what) == "" {
			continue
		}
		kind, _ := g.Kind.(string)
		if kind == "" {
			kind = "other"
		}
		out.gaps = append(out.gaps, insightIssue{kind: kind, text: strings.TrimSpace(what)})
	}
	for _, v := range body.Suggestions {
		if text, ok := v.(string); ok && strings.TrimSpace(text) != "" {
			out.suggestions = append(out.suggestions, insightIssue{text: strings.TrimSpace(text)})
		}
	}
	return out
}

// normalizeIssue is a gap's or suggestion's text as runs are matched by it.
//
//	normalizeIssue("  Add a  Health score.") // "add a health score"
func normalizeIssue(text string) string {
	t := strings.Join(strings.Fields(strings.ToLower(text)), " ")
	return strings.TrimSpace(strings.TrimRight(t, ".!?;:,"))
}

// insightHelped is whether a verdict says the ontology helped.
func insightHelped(verdict string) bool { return verdict == "essential" || verdict == "helpful" }

// insightDone is r's assessment when it is done with a verdict.
func insightDone(r *foundrymodel.FoundryWorkflowInsightRun) *foundrymodel.FoundryWorkflowInsightAssessment {
	if a := r.Assessment; a != nil && a.Status == foundrymodel.FoundryWorkflowAssessmentDone {
		return a
	}
	return nil
}

// insightTotals is what runs come to.
func insightTotals(runs []*foundrymodel.FoundryWorkflowInsightRun) *foundryv1.FoundryWorkflowInsightTotals {
	t := &foundryv1.FoundryWorkflowInsightTotals{Runs: int32(len(runs))}
	verdicts, kinds := map[string]int64{}, map[string]int32{}
	for _, r := range runs {
		if r.Status == foundrymodel.FoundryWorkflowRunOpen {
			t.Open++
		} else {
			t.Finished++
		}
		c := r.Counts
		if c.Accesses > 0 {
			t.Used++
		}
		t.Reads += c.Reads
		t.Questions += c.Questions
		t.Sends += c.Sends
		t.Uploads += c.Uploads
		if r.Assessment != nil {
			t.AssessmentUsdMicros += r.Assessment.USDMicros
		}
		a := insightDone(r)
		if a == nil || a.Verdict == "" {
			continue
		}
		t.Assessed++
		if insightHelped(a.Verdict) {
			t.Helped++
		}
		verdicts[a.Verdict]++
		said := assessedOf(r)
		for _, support := range said.claims {
			switch support {
			case "supported":
				t.ClaimsSupported++
			case "unsupported":
				t.ClaimsUnsupported++
			case "contradicted":
				t.ClaimsContradicted++
			}
		}
		named := map[string]bool{}
		for _, g := range said.gaps {
			if !named[g.kind] {
				named[g.kind] = true
				kinds[g.kind]++
			}
		}
	}
	for _, v := range foundrymodel.FoundryWorkflowVerdicts {
		if n := verdicts[v]; n > 0 {
			t.Verdicts = append(t.Verdicts, &foundryv1.FoundryWorkflowVerdictCount{Verdict: v, Runs: n})
		}
	}
	for _, k := range insightGapKinds {
		if n := kinds[k]; n > 0 {
			t.GapKinds = append(t.GapKinds, &foundryv1.FoundryWorkflowGapCount{Kind: k, Runs: n})
		}
	}
	return t
}

// insightRecurring is the issues runs (newest first) named most: their gaps
// when gaps, else their suggestions. A group's text and kind are its newest
// run's, and a run counts once in each group it names.
func insightRecurring(runs []*foundrymodel.FoundryWorkflowInsightRun, gaps bool) []*foundryv1.FoundryWorkflowInsightIssue {
	type group struct {
		issue     insightIssue
		runs      map[string]bool
		last      *foundrymodel.FoundryWorkflowInsightRun
		workflows map[string]bool
	}
	groups := map[string]*group{}
	var order []*group
	for _, r := range runs {
		said := assessedOf(r)
		issues := said.suggestions
		if gaps {
			issues = said.gaps
		}
		for _, issue := range issues {
			key := normalizeIssue(issue.text)
			if key == "" {
				continue
			}
			g, ok := groups[key]
			if !ok {
				g = &group{issue: issue, runs: map[string]bool{}, last: r, workflows: map[string]bool{}}
				groups[key] = g
				order = append(order, g)
			}
			g.runs[r.ID] = true
			g.workflows[r.WorkflowID] = true
		}
	}
	slices.SortStableFunc(order, func(a, b *group) int {
		return cmp.Or(cmp.Compare(len(b.runs), len(a.runs)), b.last.StartedAt.Compare(a.last.StartedAt),
			strings.Compare(a.issue.text, b.issue.text))
	})
	var out []*foundryv1.FoundryWorkflowInsightIssue
	for _, g := range order[:min(len(order), insightIssues)] {
		out = append(out, &foundryv1.FoundryWorkflowInsightIssue{
			Kind: g.issue.kind, Text: g.issue.text, Runs: int32(len(g.runs)), LastRunId: g.last.ID, LastWorkflowId: g.last.WorkflowID,
			LastAt: timestamppb.New(g.last.StartedAt), WorkflowIds: slices.Sorted(maps.Keys(g.workflows)),
		})
	}
	return out
}

// inWindow is the runs that started in the `days` UTC days from `from`,
// newest first.
func inWindow(runs []foundrymodel.FoundryWorkflowInsightRun, from time.Time, days int) []*foundrymodel.FoundryWorkflowInsightRun {
	to := from.AddDate(0, 0, days)
	var out []*foundrymodel.FoundryWorkflowInsightRun
	for i := range runs {
		if at := runs[i].StartedAt; !at.Before(from) && at.Before(to) {
			out = append(out, &runs[i])
		}
	}
	slices.SortStableFunc(out, func(a, b *foundrymodel.FoundryWorkflowInsightRun) int {
		return cmp.Or(b.StartedAt.Compare(a.StartedAt), strings.Compare(b.ID, a.ID))
	})
	return out
}

// workflowInsights is what runs came to over the `days` UTC days from
// `from` (the newest insightRuns of them, truncated when there are more),
// and previous over the days before; byWorkflow adds each workflow's.
func workflowInsights(runs, previous []foundrymodel.FoundryWorkflowInsightRun, from time.Time, days int,
	byWorkflow bool) *foundryv1.FoundryWorkflowInsights {
	window := inWindow(runs, from, days)
	truncated := len(window) > insightRuns
	window = window[:min(len(window), insightRuns)]
	dates := make([]string, days)
	for i := range dates {
		dates[i] = from.AddDate(0, 0, i).Format(time.DateOnly)
	}
	byDay := func(rs []*foundrymodel.FoundryWorkflowInsightRun) [][]*foundrymodel.FoundryWorkflowInsightRun {
		out := make([][]*foundrymodel.FoundryWorkflowInsightRun, days)
		for _, r := range rs {
			if i := slices.Index(dates, r.StartedAt.UTC().Format(time.DateOnly)); i >= 0 {
				out[i] = append(out[i], r)
			}
		}
		return out
	}
	segment := func(rs []*foundrymodel.FoundryWorkflowInsightRun) *foundryv1.FoundryWorkflowInsightSegment {
		seg := &foundryv1.FoundryWorkflowInsightSegment{Totals: insightTotals(rs), LastRunAt: timestamppb.New(rs[0].StartedAt)}
		for _, d := range byDay(rs) {
			seg.DailyRuns = append(seg.DailyRuns, int32(len(d)))
		}
		if gaps := insightRecurring(rs, true); len(gaps) > 0 {
			seg.TopGap = gaps[0].GetText()
		}
		return seg
	}
	// Runs grouped by key, in the order each group's newest run comes.
	groups := func(key func(*foundrymodel.FoundryWorkflowInsightRun) string) [][]*foundrymodel.FoundryWorkflowInsightRun {
		index := map[string]int{}
		var out [][]*foundrymodel.FoundryWorkflowInsightRun
		for _, r := range window {
			i, ok := index[key(r)]
			if !ok {
				i = len(out)
				index[key(r)] = i
				out = append(out, nil)
			}
			out[i] = append(out[i], r)
		}
		slices.SortStableFunc(out, func(a, b []*foundrymodel.FoundryWorkflowInsightRun) int { return cmp.Compare(len(b), len(a)) })
		return out
	}

	out := &foundryv1.FoundryWorkflowInsights{From: dates[0], Days: int32(days), Totals: insightTotals(window), Truncated: truncated}
	out.Previous = insightTotals(inWindow(previous, from.AddDate(0, 0, -days), days))
	out.Gaps, out.Suggestions = insightRecurring(window, true), insightRecurring(window, false)
	for i, rs := range byDay(window) {
		out.Series = append(out.Series, &foundryv1.FoundryWorkflowInsightDay{Date: dates[i], Totals: insightTotals(rs)})
	}
	f := &foundryv1.FoundryWorkflowInsightFunnel{Started: int32(len(window))}
	for _, r := range window {
		if r.Status != foundrymodel.FoundryWorkflowRunFinished {
			continue
		}
		f.Finished++
		a := insightDone(r)
		if a == nil {
			continue
		}
		f.Assessed++
		if r.Counts.Accesses == 0 {
			continue
		}
		f.Used++
		if insightHelped(a.Verdict) {
			f.Helped++
		}
	}
	out.Funnel = f
	agents := groups(func(r *foundrymodel.FoundryWorkflowInsightRun) string {
		return r.StartedBy.Kind.String() + ":" + r.StartedBy.ID
	})
	for _, rs := range agents[:min(len(agents), insightAgents)] {
		seg := segment(rs)
		seg.Principal = principalToProto(rs[0].StartedBy)
		out.Agents = append(out.Agents, seg)
	}
	if byWorkflow {
		for _, rs := range groups(func(r *foundrymodel.FoundryWorkflowInsightRun) string { return r.WorkflowID }) {
			seg := segment(rs)
			seg.WorkflowId = rs[0].WorkflowID
			out.Workflows = append(out.Workflows, seg)
		}
	}
	return out
}
