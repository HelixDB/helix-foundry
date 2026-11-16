package api

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// automationWorkflow adds the workflow Foundry made of ont-1 for automation
// auto-1, owned by the test person, with one open run of its run arun-1.
func automationWorkflow(store *workflowStore) (*foundrymodel.FoundryWorkflow, *foundrymodel.FoundryWorkflowRun) {
	w := &foundrymodel.FoundryWorkflow{ID: "wf-auto", WorkspaceID: "ws-1", OntologyID: "ont-1", Name: "Weekly digest", ClientToken: "automation:auto-1",
		CreatedByUserID: person.UserID, CreatedAt: time.Now(), Automation: &foundrymodel.FoundryWorkflowAutomation{ID: "auto-1", Name: "Weekly digest"}}
	input, runID := "The automation \"Weekly digest\" ran.", "arun-1"
	r := &foundrymodel.FoundryWorkflowRun{ID: "wfr-auto", WorkflowID: w.ID, WorkspaceID: "ws-1", OntologyID: "ont-1", ClientToken: runID,
		StartedBy: personPrincipal(), StartedAt: time.Now(), Input: &input, AutomationRunID: &runID, AutomationKey: "0123456789abcdef0123456789abcdef"}
	store.workflows, store.runs = append(store.workflows, w), append(store.runs, r)
	return w, r
}

func personPrincipal() access.Principal {
	return access.Principal{Kind: access.PrincipalUser, ID: person.UserID}
}

func TestAutomationWorkflowsAreFoundrys(t *testing.T) {
	s, store := workflowService(t)
	w, r := automationWorkflow(store)
	ctx := asPerson()

	got := must(s.GetFoundryWorkflow(ctx, &foundryv1.GetFoundryWorkflowRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: w.ID})).GetWorkflow()
	require.Equal(t, "auto-1", got.GetAutomation().GetAutomationId())
	require.Equal(t, "Weekly digest", got.GetAutomation().GetName())
	require.False(t, got.GetAutomation().GetDeleted())
	run := must(s.GetFoundryWorkflowRun(ctx, &foundryv1.GetFoundryWorkflowRunRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: w.ID,
		Id: r.ID})).GetRun()
	require.Equal(t, "arun-1", run.GetAutomationRunId())
	require.Equal(t, person.UserID, run.GetStartedByUserId())

	_, err := s.StartFoundryWorkflowRun(ctx, &foundryv1.StartFoundryWorkflowRunRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: w.ID})
	wantCode(t, err, codes.FailedPrecondition)
	require.Equal(t, reasonWorkflowOfAutomation, reasonOf(err), "only Foundry starts its runs")
	_, err = s.FinishFoundryWorkflowRun(ctx, &foundryv1.FinishFoundryWorkflowRunRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: w.ID,
		Id: r.ID, Output: "Done."})
	wantCode(t, err, codes.FailedPrecondition)
	require.Equal(t, reasonWorkflowOfAutomation, reasonOf(err), "only Foundry finishes them")
	_, err = s.DeleteFoundryWorkflow(ctx, &foundryv1.DeleteFoundryWorkflowRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: w.ID})
	wantCode(t, err, codes.FailedPrecondition)
	require.Equal(t, reasonWorkflowOfAutomation, reasonOf(err))
	require.Contains(t, status.Convert(err).Message(), "once the automation is deleted")

	// Once its automation is deleted, it is history, deleted like any.
	w.Automation.Deleted = true
	got = must(s.GetFoundryWorkflow(ctx, &foundryv1.GetFoundryWorkflowRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: w.ID})).GetWorkflow()
	require.True(t, got.GetAutomation().GetDeleted())
	_, err = s.DeleteFoundryWorkflow(ctx, &foundryv1.DeleteFoundryWorkflowRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: w.ID})
	require.NoError(t, err)
}

// Only the automation's own calls record under its runs: they name the run
// with its key, which no answer shows. Its id alone, which its pages show,
// records nothing, whoever passes it, the owner's own agent included.
func TestAutomationWorkflowRunsRecordOnlyTheirOwnCalls(t *testing.T) {
	s, store := workflowService(t)
	_, r := automationWorkflow(store)
	for name, ctx := range map[string]context.Context{
		"its owner":      asPerson(),
		"another person": ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceUser, UserID: "user-2"}),
		"an agent's key": ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceMCP, ServiceCredentialID: "cred-1"}),
	} {
		for _, ref := range []string{r.ID, r.ID + "_", r.ID + "_0123456789abcdef0123456789abcdee", r.ID + "_" + strings.ToUpper(r.AutomationKey)} {
			_, err := s.trackedRun(ctx, "ws-1", "ont-1", ref)
			wantCode(t, err, codes.NotFound)
		}
		require.NotEmpty(t, name)
	}
	got, err := s.trackedRun(asPerson(), "ws-1", "ont-1", r.Recording())
	require.NoError(t, err)
	require.Equal(t, r.ID, got.ID)
	_, err = s.trackedRun(asPerson(), "ws-2", "", r.Recording())
	wantCode(t, err, codes.NotFound)

	// A person's run named with a key is no run.
	mine := startRun(t, s, "Mine")
	_, err = s.trackedRun(asPerson(), "ws-1", "ont-1", mine.GetId()+"_"+r.AutomationKey)
	wantCode(t, err, codes.NotFound)

	// Tracking never fails the automation's calls: a run that holds all it
	// may, or finished, records nothing more and the call goes on.
	r.Counts.Accesses = foundrymodel.FoundryWorkflowRunMaxAccesses
	got, err = s.trackedRun(asPerson(), "ws-1", "ont-1", r.Recording())
	require.NoError(t, err)
	require.Nil(t, got)
	r.Counts.Accesses = 0
	r.Finished = &foundrymodel.FoundryWorkflowRunEnd{At: time.Now(), Output: "Done."}
	got, err = s.trackedRun(asPerson(), "ws-1", "ont-1", r.Recording())
	require.NoError(t, err)
	require.Nil(t, got)
}

// A grantee sees no automation, so neither the workflows Foundry made for
// automations nor their runs: they speak for the owner's access.
func TestGranteesSeeNoAutomationsWorkflows(t *testing.T) {
	s, store := workflowService(t)
	w, r := automationWorkflow(store)
	mine := startRun(t, s, "Mine")
	grantee := context.WithValue(access.WithCaller(asPerson(), access.Grantee{WorkspaceID: "ws-1", Principal: agentKey}), granteeLoadKey{},
		foundrymodel.FoundryLoadInputs{LoadID: "load-1"})

	listed := must(s.ListFoundryWorkflows(grantee, &foundryv1.ListFoundryWorkflowsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})).GetWorkflows()
	require.Len(t, listed, 1)
	require.Equal(t, mine.GetWorkflowId(), listed[0].GetId())
	require.Len(t, must(s.ListFoundryWorkflows(asPerson(), &foundryv1.ListFoundryWorkflowsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"})).GetWorkflows(), 2)

	_, err := s.GetFoundryWorkflow(grantee, &foundryv1.GetFoundryWorkflowRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", Id: w.ID})
	wantCode(t, err, codes.NotFound)
	_, err = s.ListFoundryWorkflowRuns(grantee, &foundryv1.ListFoundryWorkflowRunsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: w.ID})
	wantCode(t, err, codes.NotFound)
	_, err = s.GetFoundryWorkflowRun(grantee, &foundryv1.GetFoundryWorkflowRunRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: w.ID, Id: r.ID})
	wantCode(t, err, codes.NotFound)
	_, err = s.ListFoundryWorkflowRunAccesses(grantee, &foundryv1.ListFoundryWorkflowRunAccessesRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
		WorkflowId: w.ID, Id: r.ID})
	wantCode(t, err, codes.NotFound)
	_, err = s.GetFoundryWorkflowInsights(grantee, &foundryv1.GetFoundryWorkflowInsightsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1", WorkflowId: w.ID})
	wantCode(t, err, codes.NotFound)
	insights := must(s.GetFoundryWorkflowInsights(grantee, &foundryv1.GetFoundryWorkflowInsightsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}))
	require.Equal(t, int32(1), insights.GetTotals().GetRuns(), "the person's run, not the automation's")
	for _, a := range insights.GetAgents() {
		require.Nil(t, a.GetAutomation())
	}
	require.Equal(t, int32(2), must(s.GetFoundryWorkflowInsights(asPerson(), &foundryv1.GetFoundryWorkflowInsightsRequest{WorkspaceId: "ws-1",
		OntologyId: "ont-1"})).GetTotals().GetRuns())
}

func TestWorkflowInsightsCountAutomationsApart(t *testing.T) {
	var runs []foundrymodel.FoundryWorkflowInsightRun
	for i, by := range []string{"own", "auto", "auto"} {
		r := insightRun(by+string(rune('0'+i)), insightFrom.Add(time.Duration(i)*time.Hour))
		if by == "auto" {
			r.WorkflowID = "wf-auto"
			r.Automation = &foundrymodel.FoundryWorkflowAutomation{ID: "auto-1", Name: "Weekly digest"}
		}
		runs = append(runs, r)
	}
	agents := workflowInsights(runs, nil, insightFrom, 1, true).GetAgents()
	require.Len(t, agents, 2, "Ada's own runs, and her automation's")
	require.Equal(t, "auto-1", agents[0].GetAutomation().GetAutomationId())
	require.Equal(t, "Weekly digest", agents[0].GetAutomation().GetName())
	require.Equal(t, "wf-auto", agents[0].GetWorkflowId(), "its workflow filters to it")
	require.Equal(t, int32(2), agents[0].GetTotals().GetRuns())
	require.Equal(t, "user-ada", agents[0].GetPrincipal().GetId(), "its owner started them")
	require.Nil(t, agents[1].GetAutomation())
	require.Equal(t, int32(1), agents[1].GetTotals().GetRuns())

	// A starter the request names counts what they started themselves.
	s, store := workflowService(t)
	automationWorkflow(store)
	ctx := asPerson()
	all := must(s.GetFoundryWorkflowInsights(ctx, &foundryv1.GetFoundryWorkflowInsightsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1"}))
	require.Equal(t, int32(1), all.GetTotals().GetRuns())
	require.Equal(t, "auto-1", all.GetAgents()[0].GetAutomation().GetAutomationId())
	mine := must(s.GetFoundryWorkflowInsights(ctx, &foundryv1.GetFoundryWorkflowInsightsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
		StartedBy: &foundryv1.FoundryPrincipal{Kind: foundryv1.FoundryPrincipal_KIND_USER, Id: person.UserID}}))
	require.Zero(t, mine.GetTotals().GetRuns())
	byWorkflow := must(s.GetFoundryWorkflowInsights(ctx, &foundryv1.GetFoundryWorkflowInsightsRequest{WorkspaceId: "ws-1", OntologyId: "ont-1",
		WorkflowId: "wf-auto"}))
	require.Equal(t, int32(1), byWorkflow.GetTotals().GetRuns())
}

const loadAutomation = `{"v":1,"nodes":[` +
	`{"id":"start","kind":"trigger","config":{"kind":"generation_finished","ontology_id":"ont-1","outcome":"any"},"position":{"x":0,"y":0}},` +
	`{"id":"load","kind":"action","config":{"operation":"create_load","ontology_id":"ont-1"},"position":{"x":200,"y":0}}],` +
	`"edges":[{"from":"start","to":"load"}]}`

const syncAutomation = `{"v":1,"nodes":[` +
	`{"id":"start","kind":"trigger","config":{"kind":"generation_finished","ontology_id":"ont-1","outcome":"any"},"position":{"x":0,"y":0}},` +
	`{"id":"sync","kind":"action","config":{"operation":"sync_connector","connector_id":"con-1"},"position":{"x":200,"y":0}}],` +
	`"edges":[{"from":"start","to":"sync"}]}`

// Approving steps that name an ontology makes its workflow, so the page
// shows it before a run: an action's or an allowed action's, never what
// the trigger listens to.
func TestApproveAutomationTracksTheOntologiesItsStepsName(t *testing.T) {
	for name, tc := range map[string]struct {
		spec string
		want []string
	}{
		"an action step's load":     {loadAutomation, []string{"ont-1"}},
		"an Assistant step's allow": {weeklyAutomation, []string{"ont-1"}},
		"only the trigger's":        {syncAutomation, nil},
	} {
		t.Run(name, func(t *testing.T) {
			s, store := automationService(t)
			a := createAutomation(t, s, site("user-ada"), tc.spec)
			_, err := s.ApproveFoundryAutomation(site("user-ada"), &foundryv1.ApproveFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId(),
				ExpectedRevision: 1})
			require.NoError(t, err)
			require.Equal(t, tc.want, store.tracked)
		})
	}
	t.Run("an ontology full of workflows doesn't stop the approval", func(t *testing.T) {
		s, store := automationService(t)
		a := createAutomation(t, s, site("user-ada"), loadAutomation)
		store.automations[a.GetId()].Spec = []byte(strings.ReplaceAll(loadAutomation, "ont-1", "ont-full"))
		store.ontologiesKnown = []string{"ont-full"}
		_, err := s.ApproveFoundryAutomation(site("user-ada"), &foundryv1.ApproveFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId(),
			ExpectedRevision: 1})
		require.NoError(t, err)
		require.Empty(t, store.tracked)
	})
}

func TestAutomationsSayHowMuchTheirOntologiesHelped(t *testing.T) {
	s, store := automationService(t)
	a := createAutomation(t, s, site("user-ada"), weeklyAutomation)
	last := time.Now().Add(-time.Hour)
	w := &foundrymodel.FoundryWorkflow{ID: "wf-1", OntologyID: "ont-1", Runs: 4, FinishedRuns: 3, LastRunAt: &last}
	store.ontologies = map[string][]foundrymodel.FoundryAutomationOntology{a.GetId(): {
		{OntologyID: "ont-1", OntologyName: "Sales", Tracking: foundrymodel.FoundryAutomationTracked{Workflow: w,
			Verdicts: map[string]int64{"unused": 1, "essential": 2}}},
		{OntologyID: "ont-2", OntologyName: "Support", Tracking: foundrymodel.FoundryAutomationUntracked{Reason: foundrymodel.FoundryAutomationWorkflowsFull,
			At: last}},
	}}
	gaps := `{"gaps":[{"kind":"missing_data","what":"no renewal dates"},{"what":"a"},{"what":"b"},{"what":"c"},{"what":"d"},{"what":"e"},{"what":7}]}`
	store.latest = map[string]*foundrymodel.FoundryWorkflowAssessment{"wf-1": {Assessment: []byte(gaps)}}

	got := must(s.GetFoundryAutomation(site("user-alan"), &foundryv1.GetFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId()})).GetAutomation()
	require.Len(t, got.GetOntologies(), 2, "every member sees them")
	sales := got.GetOntologies()[0]
	require.Equal(t, "wf-1", sales.GetWorkflowId())
	require.Equal(t, int64(4), sales.GetRuns())
	require.Equal(t, int64(3), sales.GetFinishedRuns())
	require.NotNil(t, sales.GetLastRunAt())
	var verdicts []string
	for _, v := range sales.GetVerdicts() {
		verdicts = append(verdicts, v.GetVerdict())
	}
	require.Equal(t, []string{"essential", "unused"}, verdicts, "in the verdicts' order")
	require.Equal(t, []string{"no renewal dates", "a", "b", "c", "d"}, sales.GetGaps(), "the newest's, at most 5")
	require.Empty(t, sales.GetUntracked())
	support := got.GetOntologies()[1]
	require.Equal(t, "workflows_full", support.GetUntracked())
	require.Empty(t, support.GetWorkflowId())
	require.NotNil(t, support.GetUntrackedAt())

	list := must(s.ListFoundryAutomations(site("user-ada"), &foundryv1.ListFoundryAutomationsRequest{WorkspaceId: "ws-1"}))
	require.Len(t, list.GetAutomations()[0].GetOntologies(), 2)
	require.Empty(t, list.GetAutomations()[0].GetOntologies()[0].GetGaps(), "a list leaves the gaps out")

	// A run says how much each ontology it used helped it.
	runResp := must(s.RunFoundryAutomation(site("user-ada"), &foundryv1.RunFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId(), ClientToken: "r"}))
	runID, wrun := runResp.GetRun().GetId(), "wrun-1"
	finished := &foundrymodel.FoundryWorkflowRunEnd{At: time.Now(), Output: "Done."}
	store.runOntologies = []foundrymodel.FoundryAutomationRunOntology{
		{OntologyID: "ont-1", OntologyName: "Sales", Run: &foundrymodel.FoundryWorkflowRun{ID: wrun, WorkflowID: "wf-1", AutomationRunID: &runID, Finished: finished},
			Assessment: &foundrymodel.FoundryWorkflowAssessment{Status: foundrymodel.FoundryWorkflowAssessmentDone, Verdict: "essential"}},
		{OntologyID: "ont-3", OntologyName: "Tickets", Run: &foundrymodel.FoundryWorkflowRun{ID: "wrun-2", WorkflowID: "wf-3", AutomationRunID: &runID}},
	}
	run := must(s.GetFoundryAutomationRun(site("user-alan"), &foundryv1.GetFoundryAutomationRunRequest{WorkspaceId: "ws-1", AutomationId: a.GetId(),
		Id: runID})).GetRun()
	require.Len(t, run.GetOntologies(), 2)
	require.Equal(t, &foundryv1.FoundryAutomationRunOntology{OntologyId: "ont-1", OntologyName: "Sales", WorkflowId: "wf-1", WorkflowRunId: wrun,
		Status: "finished", AssessmentStatus: "done", Verdict: "essential"}, run.GetOntologies()[0])
	require.Equal(t, "open", run.GetOntologies()[1].GetStatus())
	require.Empty(t, run.GetOntologies()[1].GetAssessmentStatus())
	runs := must(s.ListFoundryAutomationRuns(site("user-ada"), &foundryv1.ListFoundryAutomationRunsRequest{WorkspaceId: "ws-1", AutomationId: a.GetId()}))
	require.True(t, slices.ContainsFunc(runs.GetRuns(), func(r *foundryv1.FoundryAutomationRun) bool { return len(r.GetOntologies()) == 0 }),
		"a list of runs leaves them out")
}
