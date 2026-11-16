package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/access"
	"github.com/helixdb/helix-foundry/backend/config"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// automationStore keeps automations in memory, over a workspace with one
// connector, ontology, set and dashboard. It records what the API asked of
// it; the store's own rules are the store's tests'.
type automationStore struct {
	foundrystore.Unimplemented
	automations map[string]*foundrymodel.FoundryAutomation
	runs        map[string]*foundrymodel.FoundryAutomationRun
	nodes       []*foundrymodel.FoundryAutomationRunNode
	// approvedNext is the first time the last approval or resume was given.
	approvedNext *time.Time
	refuse       error
	nowRun       *foundrymodel.FoundryAutomationRun
	triggerNode  string
	lastUpdate   foundrymodel.FoundryAutomationUpdate
	// stats is what FoundryAutomationRunStats answers, and statsSince what it was asked.
	stats      *foundrymodel.FoundryAutomationRunStats
	statsSince time.Time
	// tracked are the ontologies approvals made workflows of; ontologies,
	// runOntologies and latest what the reads of them answer.
	tracked         []string
	ontologiesKnown []string
	ontologies      map[string][]foundrymodel.FoundryAutomationOntology
	runOntologies   []foundrymodel.FoundryAutomationRunOntology
	latest          map[string]*foundrymodel.FoundryWorkflowAssessment
}

func (s *automationStore) EnsureFoundryAutomationWorkflow(_ context.Context, ws, automationID, ontologyID string) (*foundrymodel.FoundryWorkflow, error) {
	if ontologyID == "ont-full" {
		return nil, foundrystore.ErrLimitExceeded
	}
	s.tracked = append(s.tracked, ontologyID)
	return &foundrymodel.FoundryWorkflow{ID: "wf-" + ontologyID, WorkspaceID: ws, OntologyID: ontologyID,
		Automation: &foundrymodel.FoundryWorkflowAutomation{ID: automationID}}, nil
}

func (s *automationStore) ListFoundryAutomationOntologies(_ context.Context, _ string, ids []string) (map[string][]foundrymodel.FoundryAutomationOntology, error) {
	out := map[string][]foundrymodel.FoundryAutomationOntology{}
	for _, id := range ids {
		out[id] = s.ontologies[id]
	}
	return out, nil
}

func (s *automationStore) ListFoundryAutomationRunOntologies(context.Context, string) ([]foundrymodel.FoundryAutomationRunOntology, error) {
	return s.runOntologies, nil
}

func (s *automationStore) LatestFoundryWorkflowAssessments(_ context.Context, ids []string) (map[string]*foundrymodel.FoundryWorkflowAssessment, error) {
	out := map[string]*foundrymodel.FoundryWorkflowAssessment{}
	for _, id := range ids {
		if a := s.latest[id]; a != nil {
			out[id] = a
		}
	}
	return out, nil
}

func newAutomationStore() *automationStore {
	return &automationStore{automations: map[string]*foundrymodel.FoundryAutomation{}, runs: map[string]*foundrymodel.FoundryAutomationRun{}}
}

func (s *automationStore) GetConnector(_ context.Context, ws, id string) (*foundrymodel.Connector, error) {
	if ws != "ws-1" || id != "con-1" {
		return nil, foundrystore.ErrNotFound
	}
	return &foundrymodel.Connector{ID: id, WorkspaceID: ws}, nil
}

func (s *automationStore) GetFoundryOntology(_ context.Context, ws, id string) (*foundrymodel.FoundryOntology, error) {
	switch {
	case ws == "ws-1" && id == "ont-1":
		return &foundrymodel.FoundryOntology{ID: id, WorkspaceID: ws}, nil
	case ws == "ws-1" && id == "ont-gone":
		now := time.Now()
		return &foundrymodel.FoundryOntology{ID: id, WorkspaceID: ws, DeletedAt: &now}, nil
	case ws == "ws-1" && slices.Contains(s.ontologiesKnown, id):
		return &foundrymodel.FoundryOntology{ID: id, WorkspaceID: ws}, nil
	}
	return nil, foundrystore.ErrNotFound
}

// GetFoundryDocumentSet knows set-1, a person's, and set-notes, which con-1's syncs write.
func (s *automationStore) GetFoundryDocumentSet(_ context.Context, ws, id string) (*foundrymodel.FoundryDocumentSet, error) {
	switch {
	case ws == "ws-1" && id == "set-1":
		return &foundrymodel.FoundryDocumentSet{ID: id, WorkspaceID: ws}, nil
	case ws == "ws-1" && id == "set-notes":
		return &foundrymodel.FoundryDocumentSet{ID: id, WorkspaceID: ws, Owner: &foundrymodel.FoundryDocumentSetOwner{ConnectorID: "con-1", Stream: "notes"}}, nil
	}
	return nil, foundrystore.ErrNotFound
}

func (s *automationStore) GetFoundryDashboard(_ context.Context, ws, id string) (*foundrymodel.FoundryDashboard, error) {
	if ws != "ws-1" || id != "dash-1" {
		return nil, foundrystore.ErrNotFound
	}
	return &foundrymodel.FoundryDashboard{ID: id, WorkspaceID: ws}, nil
}

func (s *automationStore) CreateFoundryAutomation(_ context.Context, a *foundrymodel.FoundryAutomation, max int) error {
	if len(s.automations) >= max {
		return foundrymodel.FoundryAutomationRefusal{Reason: foundrymodel.FoundryAutomationLimit, Detail: "too many"}
	}
	stored := *a
	stored.Status, stored.Revision, stored.SpecRevision = foundrymodel.FoundryAutomationNeedsApproval, 1, 1
	s.automations[a.ID] = &stored
	return nil
}

func (s *automationStore) GetFoundryAutomation(_ context.Context, ws, id string) (*foundrymodel.FoundryAutomation, error) {
	a, ok := s.automations[id]
	if !ok || a.WorkspaceID != ws {
		return nil, foundrystore.ErrNotFound
	}
	copied := *a
	return &copied, nil
}

func (s *automationStore) GetFoundryAutomationByClientToken(_ context.Context, ws, owner, token string) (*foundrymodel.FoundryAutomation, error) {
	for _, a := range s.automations {
		if a.WorkspaceID == ws && a.OwnerUserID == owner && a.ClientToken != nil && *a.ClientToken == token {
			copied := *a
			return &copied, nil
		}
	}
	return nil, foundrystore.ErrNotFound
}

func (s *automationStore) ListFoundryAutomations(_ context.Context, ws string) ([]*foundrymodel.FoundryAutomation, error) {
	var out []*foundrymodel.FoundryAutomation
	for _, a := range s.automations {
		if a.WorkspaceID == ws {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *automationStore) FoundryAutomationRunStats(_ context.Context, ws string, since time.Time) (*foundrymodel.FoundryAutomationRunStats, error) {
	s.statsSince = since
	if s.stats == nil {
		return &foundrymodel.FoundryAutomationRunStats{ByAutomation: map[string]foundrymodel.FoundryAutomationRunCounts{}}, nil
	}
	return s.stats, nil
}

func (s *automationStore) UpdateFoundryAutomation(_ context.Context, ws, id, owner string, expected int64, u foundrymodel.FoundryAutomationUpdate) (
	*foundrymodel.FoundryAutomation, error) {
	if s.refuse != nil {
		return nil, s.refuse
	}
	a := s.automations[id]
	s.lastUpdate = u
	if u.Spec != nil {
		a.Spec, a.TriggerKind, a.TriggerTarget, a.Status = u.Spec.JSON, u.Spec.TriggerKind, u.Spec.TriggerTarget, foundrymodel.FoundryAutomationNeedsApproval
	}
	if u.Name != nil {
		a.Name = *u.Name
	}
	if u.Description != nil {
		a.Description = *u.Description
	}
	a.Revision++
	return s.GetFoundryAutomation(context.Background(), ws, id)
}

func (s *automationStore) ApproveFoundryAutomation(_ context.Context, ws, id, owner string, expected int64, next *time.Time) (*foundrymodel.FoundryAutomation, error) {
	if s.refuse != nil {
		return nil, s.refuse
	}
	a := s.automations[id]
	a.Status, s.approvedNext, a.NextFireAt = foundrymodel.FoundryAutomationActive, next, next
	return s.GetFoundryAutomation(context.Background(), ws, id)
}

func (s *automationStore) PauseFoundryAutomation(_ context.Context, ws, id string, by foundrymodel.FoundryAutomationActor) (*foundrymodel.FoundryAutomation, error) {
	if s.refuse != nil {
		return nil, s.refuse
	}
	a, ok := s.automations[id]
	if !ok {
		return nil, foundrystore.ErrNotFound
	}
	reason := foundrymodel.FoundryAutomationPausedByPerson
	a.Status, a.PausedReason, a.PausedByUserID = foundrymodel.FoundryAutomationPaused, &reason, &by.UserID
	return s.GetFoundryAutomation(context.Background(), ws, id)
}

func (s *automationStore) ResumeFoundryAutomation(_ context.Context, ws, id, owner string, next *time.Time) (*foundrymodel.FoundryAutomation, error) {
	a := s.automations[id]
	a.Status, a.PausedReason, s.approvedNext = foundrymodel.FoundryAutomationActive, nil, next
	return s.GetFoundryAutomation(context.Background(), ws, id)
}

func (s *automationStore) DeleteFoundryAutomation(_ context.Context, ws, id string, by foundrymodel.FoundryAutomationActor) error {
	if s.refuse != nil {
		return s.refuse
	}
	delete(s.automations, id)
	return nil
}

func (s *automationStore) RunFoundryAutomationNow(_ context.Context, run *foundrymodel.FoundryAutomationRun, triggerNodeID string, _ json.RawMessage,
	limits foundrymodel.FoundryAutomationLimits) (*foundrymodel.FoundryAutomationRun, error) {
	if s.refuse != nil {
		return nil, s.refuse
	}
	stored := *run
	stored.Status, stored.CreatedAt = foundrymodel.FoundryAutomationRunQueued, time.Now()
	s.nowRun, s.triggerNode, s.runs[run.ID] = &stored, triggerNodeID, &stored
	return &stored, nil
}

func (s *automationStore) ListFoundryAutomationRuns(_ context.Context, automationID, pageToken string, limit int) ([]*foundrymodel.FoundryAutomationRun, string, error) {
	if pageToken == "bad" {
		return nil, "", foundrystore.ErrInvalidPageToken
	}
	var out []*foundrymodel.FoundryAutomationRun
	for _, r := range s.runs {
		if r.AutomationID == automationID {
			out = append(out, r)
		}
	}
	return out, "", nil
}

func (s *automationStore) GetFoundryAutomationRun(_ context.Context, ws, id string) (*foundrymodel.FoundryAutomationRun, error) {
	r, ok := s.runs[id]
	if !ok || r.WorkspaceID != ws {
		return nil, foundrystore.ErrNotFound
	}
	return r, nil
}

func (s *automationStore) ListFoundryAutomationRunNodes(context.Context, string) ([]*foundrymodel.FoundryAutomationRunNode, error) {
	return s.nodes, nil
}

func (s *automationStore) CancelFoundryAutomationRun(_ context.Context, ws, id string, by foundrymodel.FoundryAutomationActor) (*foundrymodel.FoundryAutomationRun, error) {
	if s.refuse != nil {
		return nil, s.refuse
	}
	r := s.runs[id]
	r.Status = foundrymodel.FoundryAutomationRunCancelled
	return r, nil
}

const weeklyAutomation = `{"v":1,"nodes":[` +
	`{"id":"start","kind":"trigger","config":{"kind":"schedule","every":"week","weekday":"monday","at":"09:00","time_zone":"Europe/London"},"position":{"x":0,"y":0}},` +
	`{"id":"sync","kind":"action","config":{"operation":"sync_connector","connector_id":"con-1"},"position":{"x":200,"y":0}},` +
	`{"id":"ask","kind":"assistant","config":{"instruction":"Summarise the overdue invoices","allow":[{"operation":"create_load","ontology_id":"ont-1"}]},"position":{"x":400,"y":0}},` +
	`{"id":"mail","kind":"notify","config":{"message":"This week"},"position":{"x":600,"y":0}}],` +
	`"edges":[{"from":"start","to":"sync"},{"from":"sync","to":"ask"},{"from":"ask","to":"mail"}]}`

func automationService(t *testing.T) (*Service, *automationStore) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Foundry.Automations.Enabled = true
	store := newAutomationStore()
	return &Service{deps: &runtimeDeps{Cfg: &cfg, DB: store,
		People: dashboardPeople{"user-ada": ports.RoleMember, "user-admin": ports.RoleAdmin, "user-alan": ports.RoleMember}}}, store
}

func site(userID string) context.Context {
	return asActor(ports.Actor{Source: ports.SourceUser, UserID: userID, WorkspaceID: "ws-1"})
}

func throughMCP(userID string) context.Context {
	return asActor(ports.Actor{Source: ports.SourceMCP, UserID: userID, WorkspaceID: "ws-1"})
}

func createAutomation(t *testing.T, s *Service, ctx context.Context, spec string) *foundryv1.FoundryAutomation {
	t.Helper()
	resp, err := s.CreateFoundryAutomation(ctx, &foundryv1.CreateFoundryAutomationRequest{WorkspaceId: "ws-1", Name: " Weekly ", SpecJson: spec,
		ClientToken: fmt.Sprintf("token-%d", time.Now().UnixNano())})
	require.NoError(t, err)
	return resp.GetAutomation()
}

func TestAutomationsAreOffAndPeoples(t *testing.T) {
	s, _ := automationService(t)
	s.deps.Cfg.Foundry.Automations.Enabled = false
	_, err := s.ListFoundryAutomations(site("user-ada"), &foundryv1.ListFoundryAutomationsRequest{WorkspaceId: "ws-1"})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Equal(t, "FOUNDRY_AUTOMATIONS_DISABLED", reasonOf(err))
	require.Equal(t, "workflows aren't available here", status.Convert(err).Message(), "people read them as workflows")
	s.deps.Cfg.Foundry.Automations.Enabled = true

	for name, ctx := range map[string]context.Context{
		"a key":    asActor(ports.Actor{Source: ports.SourceMCP, ServiceCredentialID: "key-1", WorkspaceID: "ws-1"}),
		"no one":   context.Background(),
		"an event": asActor(ports.Actor{Source: ports.SourceEventSource, WorkspaceID: "ws-1"}),
		"a grantee": access.WithCaller(asActor(ports.Actor{Source: ports.SourceGrantee, UserID: "user-ada", WorkspaceID: "ws-1"}),
			access.Grantee{WorkspaceID: "ws-1", Principal: access.Principal{Kind: access.PrincipalUser, ID: "user-ada"}}),
		"a no-userID": asActor(ports.Actor{Source: ports.SourceUser, WorkspaceID: "ws-1"}),
	} {
		_, err := s.ListFoundryAutomations(ctx, &foundryv1.ListFoundryAutomationsRequest{WorkspaceId: "ws-1"})
		require.Equal(t, codes.PermissionDenied, status.Code(err), name)
		require.Equal(t, "workflows are people's: sign in to Foundry", status.Convert(err).Message(), name)
	}
	_, err = s.ListFoundryAutomations(site("user-ada"), &foundryv1.ListFoundryAutomationsRequest{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestCreateAutomationChecksItsSpec(t *testing.T) {
	s, store := automationService(t)
	a := createAutomation(t, s, throughMCP("user-ada"), weeklyAutomation)
	require.Equal(t, "needs_approval", a.GetStatus(), "an automation never runs before its owner approves it")
	require.Equal(t, "Weekly", a.GetName())
	require.Equal(t, "schedule", a.GetTriggerKind())
	require.True(t, a.GetCanEdit())
	require.False(t, a.GetCanApprove(), "through the MCP, it can't be approved")
	require.False(t, a.GetCanRun())
	require.Len(t, store.automations, 1)

	again, err := s.CreateFoundryAutomation(site("user-ada"), &foundryv1.CreateFoundryAutomationRequest{WorkspaceId: "ws-1", Name: "x",
		SpecJson: weeklyAutomation, ClientToken: *store.automations[a.GetId()].ClientToken})
	require.NoError(t, err)
	require.Equal(t, a.GetId(), again.GetAutomation().GetId(), "the same client token returns it")

	for _, c := range []struct{ name, spec, want string }{
		{"not a spec", `{"v":2}`, "spec_json:"},
		{"another workspace's connector", strings.Replace(weeklyAutomation, "con-1", "con-9", 1), "the connector con-9"},
		{"a deleted ontology", strings.Replace(weeklyAutomation, `"ontology_id":"ont-1"`, `"ontology_id":"ont-gone"`, 1), "the ontology ont-gone"},
		{"another set", strings.Replace(strings.Replace(weeklyAutomation,
			`{"kind":"schedule","every":"week","weekday":"monday","at":"09:00","time_zone":"Europe/London"}`, `{"kind":"document_arrived","set_id":"set-9"}`, 1),
			"", "", 1), "the document set set-9"},
		{"another dashboard", strings.Replace(weeklyAutomation, `{"operation":"sync_connector","connector_id":"con-1"}`,
			`{"operation":"publish_dashboard","dashboard_id":"dash-9"}`, 1), "the dashboard dash-9"},
	} {
		_, err := s.CreateFoundryAutomation(site("user-ada"), &foundryv1.CreateFoundryAutomationRequest{WorkspaceId: "ws-1", Name: "x", SpecJson: c.spec,
			ClientToken: "t-" + c.name})
		require.Equal(t, codes.InvalidArgument, status.Code(err), c.name)
		require.Contains(t, status.Convert(err).Message(), c.want, c.name)
	}
	for _, req := range []*foundryv1.CreateFoundryAutomationRequest{
		{WorkspaceId: "ws-1", Name: "x", SpecJson: weeklyAutomation},
		{WorkspaceId: "ws-1", Name: "x", SpecJson: weeklyAutomation, ClientToken: strings.Repeat("t", 65)},
		{WorkspaceId: "ws-1", Name: " ", SpecJson: weeklyAutomation, ClientToken: "t"},
		{WorkspaceId: "ws-1", Name: "x", Description: strings.Repeat("d", 1001), SpecJson: weeklyAutomation, ClientToken: "t"},
	} {
		_, err := s.CreateFoundryAutomation(site("user-ada"), req)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
	// A sync of the connector whose syncs write the set its trigger waits on
	// would set off another run each time, as an action step or an Assistant
	// step's; a set a person made can't be.
	onArrival := func(set string) string {
		return strings.Replace(weeklyAutomation, `{"kind":"schedule","every":"week","weekday":"monday","at":"09:00","time_zone":"Europe/London"}`,
			`{"kind":"document_arrived","set_id":"`+set+`"}`, 1)
	}
	assistantSyncs := strings.Replace(strings.Replace(onArrival("set-notes"), `{"operation":"sync_connector","connector_id":"con-1"}`,
		`{"operation":"create_load","ontology_id":"ont-1"}`, 1), `"allow":[{"operation":"create_load","ontology_id":"ont-1"}]`,
		`"allow":[{"operation":"sync_connector","connector_id":"con-1"}]`, 1)
	for name, c := range map[string]struct{ spec, node string }{
		"an action step":    {onArrival("set-notes"), "sync"},
		"an Assistant step": {assistantSyncs, "ask"},
	} {
		_, err := s.CreateFoundryAutomation(site("user-ada"), &foundryv1.CreateFoundryAutomationRequest{WorkspaceId: "ws-1", Name: "x", SpecJson: c.spec,
			ClientToken: "loop-" + name})
		require.Equal(t, codes.InvalidArgument, status.Code(err), name)
		require.Equal(t, "spec_json: node "+c.node+" syncs the connector whose documents its trigger waits on, so each run would start another: start it on a schedule instead",
			status.Convert(err).Message(), name)
	}
	createAutomation(t, s, site("user-ada"), onArrival("set-1"))
	s.deps.Cfg.Foundry.Automations.MaxPerWorkspace = 1
	_, err = s.CreateFoundryAutomation(site("user-ada"), &foundryv1.CreateFoundryAutomationRequest{WorkspaceId: "ws-1", Name: "x",
		SpecJson: weeklyAutomation, ClientToken: "fresh"})
	require.Equal(t, "FOUNDRY_AUTOMATION_LIMIT", reasonOf(err))
}

func TestApproveAutomationIsTheOwnersClickInTheSite(t *testing.T) {
	s, store := automationService(t)
	a := createAutomation(t, s, site("user-ada"), weeklyAutomation)
	require.True(t, a.GetCanApprove())
	_, err := s.ApproveFoundryAutomation(throughMCP("user-ada"), &foundryv1.ApproveFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId(), ExpectedRevision: 1})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "an agent never approves")
	_, err = s.ApproveFoundryAutomation(site("user-ada"), &foundryv1.ApproveFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId()})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	approved, err := s.ApproveFoundryAutomation(site("user-ada"), &foundryv1.ApproveFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId(), ExpectedRevision: 1})
	require.NoError(t, err)
	require.Equal(t, "active", approved.GetAutomation().GetStatus())
	require.NotNil(t, store.approvedNext, "a schedule fires first at its next time")
	require.Equal(t, time.Monday, store.approvedNext.In(mustZone(t, "Europe/London")).Weekday())
	require.True(t, store.approvedNext.After(time.Now()))
	require.NotNil(t, approved.GetAutomation().GetNextRunAt())
	require.True(t, approved.GetAutomation().GetCanPause())
	require.True(t, approved.GetAutomation().GetCanRun())

	store.refuse = foundrymodel.FoundryAutomationRefusal{Reason: foundrymodel.FoundryAutomationChanged, Detail: "changed"}
	_, err = s.ApproveFoundryAutomation(site("user-ada"), &foundryv1.ApproveFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId(), ExpectedRevision: 1})
	require.Equal(t, "FOUNDRY_AUTOMATION_CHANGED", reasonOf(err))
	store.refuse = foundrymodel.FoundryAutomationRefusal{Reason: foundrymodel.FoundryAutomationNotOwner, Detail: "not yours"}
	_, err = s.ApproveFoundryAutomation(site("user-alan"), &foundryv1.ApproveFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId(), ExpectedRevision: 1})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	store.refuse = nil

	// What it names is checked again: gone since it was saved, it isn't approved.
	stored := store.automations[a.GetId()]
	stored.Spec = []byte(strings.Replace(weeklyAutomation, "con-1", "con-9", 1))
	_, err = s.ApproveFoundryAutomation(site("user-ada"), &foundryv1.ApproveFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId(), ExpectedRevision: 1})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = s.ApproveFoundryAutomation(site("user-ada"), &foundryv1.ApproveFoundryAutomationRequest{WorkspaceId: "ws-1", Id: "missing", ExpectedRevision: 1})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

func TestUpdatePauseResumeDeleteAutomation(t *testing.T) {
	s, store := automationService(t)
	a := createAutomation(t, s, site("user-ada"), weeklyAutomation)
	name := "Monday sync"
	resp, err := s.UpdateFoundryAutomation(site("user-ada"), &foundryv1.UpdateFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId(), ExpectedRevision: 1, Name: &name})
	require.NoError(t, err)
	require.Equal(t, "Monday sync", resp.GetAutomation().GetName())
	for _, req := range []*foundryv1.UpdateFoundryAutomationRequest{
		{WorkspaceId: "ws-1", Id: a.GetId(), ExpectedRevision: 2},
		{WorkspaceId: "ws-1", Id: a.GetId()},
		{WorkspaceId: "ws-1", Id: a.GetId(), ExpectedRevision: 2, SpecJson: &[]string{"{}"}[0]},
		{WorkspaceId: "ws-1", Id: a.GetId(), ExpectedRevision: 2, Name: &[]string{""}[0]},
	} {
		_, err := s.UpdateFoundryAutomation(site("user-ada"), req)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
	spec := strings.Replace(weeklyAutomation, `"at":"09:00"`, `"at":"10:00"`, 1)
	resp, err = s.UpdateFoundryAutomation(site("user-ada"), &foundryv1.UpdateFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId(), ExpectedRevision: 2,
		SpecJson: &spec})
	require.NoError(t, err)
	require.Contains(t, resp.GetAutomation().GetSpecJson(), `"10:00"`)
	require.False(t, store.lastUpdate.Spec.LayoutOnly, "a new time is a new spec")
	moved := strings.Replace(spec, `"position":{"x":200,"y":0}`, `"position":{"x":260,"y":80}`, 1)
	_, err = s.UpdateFoundryAutomation(site("user-ada"), &foundryv1.UpdateFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId(), ExpectedRevision: 3,
		SpecJson: &moved})
	require.NoError(t, err)
	require.True(t, store.lastUpdate.Spec.LayoutOnly, "moving a node keeps its approval")
	_, err = s.UpdateFoundryAutomation(site("user-ada"), &foundryv1.UpdateFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId(), ExpectedRevision: 3,
		SpecJson: &moved})
	require.NoError(t, err)
	require.False(t, store.lastUpdate.Spec.LayoutOnly, "compared with a revision it no longer has, it is a new spec")
	store.refuse = foundrymodel.FoundryAutomationRefusal{Reason: foundrymodel.FoundryAutomationNotOwner, Detail: "only its owner changes a workflow"}
	_, err = s.UpdateFoundryAutomation(site("user-alan"), &foundryv1.UpdateFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId(), ExpectedRevision: 5,
		Name: &name})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	store.refuse = foundrymodel.FoundryAutomationRefusal{Reason: foundrymodel.FoundryAutomationNotActive, Detail: "waits"}
	_, err = s.PauseFoundryAutomation(site("user-ada"), &foundryv1.PauseFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId()})
	require.Equal(t, "FOUNDRY_AUTOMATION_NOT_ACTIVE", reasonOf(err))
	store.refuse = nil
	paused, err := s.PauseFoundryAutomation(site("user-admin"), &foundryv1.PauseFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId()})
	require.NoError(t, err)
	require.Equal(t, "person", paused.GetAutomation().GetPausedReason())
	require.Equal(t, "user-admin", paused.GetAutomation().GetPausedByUserId())
	require.False(t, paused.GetAutomation().GetCanResume(), "only its owner resumes it")
	require.True(t, paused.GetAutomation().GetCanDelete(), "an admin deletes anyone's")
	_, err = s.PauseFoundryAutomation(site("user-ada"), &foundryv1.PauseFoundryAutomationRequest{WorkspaceId: "ws-1", Id: "missing"})
	require.Equal(t, codes.NotFound, status.Code(err))

	resumed, err := s.ResumeFoundryAutomation(site("user-ada"), &foundryv1.ResumeFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId()})
	require.NoError(t, err)
	require.Equal(t, "active", resumed.GetAutomation().GetStatus())
	require.NotNil(t, store.approvedNext)

	store.refuse = foundrymodel.FoundryAutomationRefusal{Reason: foundrymodel.FoundryAutomationNotAllowed, Detail: "not yours"}
	_, err = s.DeleteFoundryAutomation(site("user-alan"), &foundryv1.DeleteFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId()})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	store.refuse = nil
	_, err = s.DeleteFoundryAutomation(site("user-ada"), &foundryv1.DeleteFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId()})
	require.NoError(t, err)
	_, err = s.GetFoundryAutomation(site("user-ada"), &foundryv1.GetFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId()})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestRunAutomationNowAndItsRuns(t *testing.T) {
	s, store := automationService(t)
	a := createAutomation(t, s, site("user-ada"), weeklyAutomation)
	_, err := s.RunFoundryAutomation(site("user-ada"), &foundryv1.RunFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId()})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "a run by hand has its client token")
	resp, err := s.RunFoundryAutomation(site("user-ada"), &foundryv1.RunFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId(), ClientToken: "click-1"})
	require.NoError(t, err)
	run := resp.GetRun()
	require.Equal(t, "queued", run.GetStatus())
	require.True(t, run.GetCanCancel())
	require.Equal(t, "manual:click-1", store.nowRun.TriggerKey)
	require.Equal(t, "start", store.triggerNode, "the trigger's step starts it")
	var event foundrymodel.FoundryAutomationEvent
	require.NoError(t, json.Unmarshal(store.nowRun.TriggerEvent, &event))
	require.Equal(t, "manual", event.Kind)
	require.Equal(t, "user-ada", event.ByUserID)

	for refusal, want := range map[foundrymodel.FoundryAutomationRefusalReason]codes.Code{
		foundrymodel.FoundryAutomationBusy: codes.FailedPrecondition, foundrymodel.FoundryAutomationUnapproved: codes.FailedPrecondition,
		foundrymodel.FoundryAutomationRunsPerDay: codes.ResourceExhausted, foundrymodel.FoundryAutomationNotOwner: codes.PermissionDenied,
	} {
		store.refuse = foundrymodel.FoundryAutomationRefusal{Reason: refusal, Detail: "no"}
		_, err := s.RunFoundryAutomation(site("user-ada"), &foundryv1.RunFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId(), ClientToken: "click-2"})
		require.Equal(t, want, status.Code(err), refusal)
	}
	store.refuse = errors.New("the database went away")
	_, err = s.RunFoundryAutomation(site("user-ada"), &foundryv1.RunFoundryAutomationRequest{WorkspaceId: "ws-1", Id: a.GetId(), ClientToken: "click-3"})
	require.Equal(t, codes.Internal, status.Code(err))
	store.refuse = nil

	runs, err := s.ListFoundryAutomationRuns(site("user-alan"), &foundryv1.ListFoundryAutomationRunsRequest{WorkspaceId: "ws-1", AutomationId: a.GetId()})
	require.NoError(t, err)
	require.Len(t, runs.GetRuns(), 1)
	require.False(t, runs.GetRuns()[0].GetCanCancel(), "another member doesn't cancel it")
	_, err = s.ListFoundryAutomationRuns(site("user-ada"), &foundryv1.ListFoundryAutomationRunsRequest{WorkspaceId: "ws-1", AutomationId: a.GetId(), PageToken: "bad"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = s.ListFoundryAutomationRuns(site("user-ada"), &foundryv1.ListFoundryAutomationRunsRequest{WorkspaceId: "ws-1", AutomationId: "missing"})
	require.Equal(t, codes.NotFound, status.Code(err))

	finished := time.Now()
	store.nodes = []*foundrymodel.FoundryAutomationRunNode{
		{RunID: run.GetId(), NodeID: "start", Status: foundrymodel.FoundryAutomationNodeSucceeded, Output: json.RawMessage(`{"summary":"Run by hand."}`), FinishedAt: &finished},
		{RunID: run.GetId(), NodeID: "sync", Status: foundrymodel.FoundryAutomationNodeExecuting},
	}
	got, err := s.GetFoundryAutomationRun(site("user-ada"), &foundryv1.GetFoundryAutomationRunRequest{WorkspaceId: "ws-1", AutomationId: a.GetId(), Id: run.GetId()})
	require.NoError(t, err)
	require.Len(t, got.GetRun().GetNodes(), 2)
	require.Equal(t, "running", got.GetRun().GetNodes()[1].GetStatus(), "an action in flight reads running")
	require.Equal(t, `{"summary":"Run by hand."}`, got.GetRun().GetNodes()[0].GetOutputJson())
	thread, turn, why := "thread-1", "turn-1", "Its ontology is gone."
	store.runs[run.GetId()].ThreadID, store.runs[run.GetId()].Error = &thread, &why
	store.nodes[1].TurnID, store.nodes[1].Error = &turn, &why
	owner, err := s.GetFoundryAutomationRun(site("user-ada"), &foundryv1.GetFoundryAutomationRunRequest{WorkspaceId: "ws-1", AutomationId: a.GetId(), Id: run.GetId()})
	require.NoError(t, err)
	require.Equal(t, "thread-1", owner.GetRun().GetThreadId())
	require.Equal(t, "turn-1", owner.GetRun().GetNodes()[1].GetTurnId())
	admin, err := s.GetFoundryAutomationRun(site("user-admin"), &foundryv1.GetFoundryAutomationRunRequest{WorkspaceId: "ws-1", AutomationId: a.GetId(), Id: run.GetId()})
	require.NoError(t, err)
	require.Equal(t, why, admin.GetRun().GetError(), "an admin sees why it failed")
	require.Empty(t, admin.GetRun().GetThreadId(), "the thread is its owner's")
	member, err := s.GetFoundryAutomationRun(site("user-alan"), &foundryv1.GetFoundryAutomationRunRequest{WorkspaceId: "ws-1", AutomationId: a.GetId(), Id: run.GetId()})
	require.NoError(t, err)
	require.Equal(t, "running", member.GetRun().GetNodes()[1].GetStatus(), "another member sees how far it went")
	for _, n := range member.GetRun().GetNodes() {
		require.Empty(t, n.GetOutputJson(), "and not what its owner's steps read")
		require.Empty(t, n.GetError())
		require.Empty(t, n.GetTurnId())
	}
	require.Empty(t, member.GetRun().GetError())
	require.Empty(t, member.GetRun().GetThreadId())
	require.False(t, member.GetRun().GetCanCancel())
	_, err = s.GetFoundryAutomationRun(site("user-ada"), &foundryv1.GetFoundryAutomationRunRequest{WorkspaceId: "ws-1", AutomationId: a.GetId(), Id: "missing"})
	require.Equal(t, codes.NotFound, status.Code(err))
	store.runs["other"] = &foundrymodel.FoundryAutomationRun{ID: "other", AutomationID: "elsewhere", WorkspaceID: "ws-1"}
	_, err = s.GetFoundryAutomationRun(site("user-ada"), &foundryv1.GetFoundryAutomationRunRequest{WorkspaceId: "ws-1", AutomationId: a.GetId(), Id: "other"})
	require.Equal(t, codes.NotFound, status.Code(err), "a run of another automation is not found")

	store.refuse = foundrymodel.FoundryAutomationRefusal{Reason: foundrymodel.FoundryAutomationRunEnded, Detail: "ended"}
	_, err = s.CancelFoundryAutomationRun(site("user-ada"), &foundryv1.CancelFoundryAutomationRunRequest{WorkspaceId: "ws-1", AutomationId: a.GetId(), Id: run.GetId()})
	require.Equal(t, "FOUNDRY_AUTOMATION_RUN_ENDED", reasonOf(err))
	store.refuse = nil
	cancelled, err := s.CancelFoundryAutomationRun(site("user-ada"), &foundryv1.CancelFoundryAutomationRunRequest{WorkspaceId: "ws-1", AutomationId: a.GetId(), Id: run.GetId()})
	require.NoError(t, err)
	require.Equal(t, "cancelled", cancelled.GetRun().GetStatus())

	list, err := s.ListFoundryAutomations(site("user-ada"), &foundryv1.ListFoundryAutomationsRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	require.True(t, list.GetCanCreate())
	require.True(t, list.GetEmail())
	require.Empty(t, list.GetAutomations()[0].GetSpecJson(), "a list has no specs")
	s.deps.Cfg.Host.Local = true
	list, err = s.ListFoundryAutomations(site("user-ada"), &foundryv1.ListFoundryAutomationsRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	require.False(t, list.GetEmail(), "an install sends no email")
}

// A list counts each automation's runs of the days asked, by the days of
// the time zone asked, today the last; without stats_days it counts none.
func TestListAutomationsCountsTheirRuns(t *testing.T) {
	s, store := automationService(t)
	a := createAutomation(t, s, site("user-ada"), weeklyAutomation)
	failed := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	store.automations[a.GetId()].LastFailedAt = &failed

	list, err := s.ListFoundryAutomations(site("user-ada"), &foundryv1.ListFoundryAutomationsRequest{WorkspaceId: "ws-1"})
	require.NoError(t, err)
	require.Nil(t, list.GetSummary(), "no days, no counts")
	require.Nil(t, list.GetAutomations()[0].GetStats())
	require.Equal(t, failed, list.GetAutomations()[0].GetLastFailedAt().AsTime())

	for name, req := range map[string]*foundryv1.ListFoundryAutomationsRequest{
		"too many days": {WorkspaceId: "ws-1", StatsDays: 31},
		"negative":      {WorkspaceId: "ws-1", StatsDays: -1},
		"no zone":       {WorkspaceId: "ws-1", StatsDays: 7, TimeZone: "Mars/Olympus"},
		"local":         {WorkspaceId: "ws-1", StatsDays: 7, TimeZone: "Local"},
		"zone, no days": {WorkspaceId: "ws-1", TimeZone: "UTC"},
	} {
		_, err := s.ListFoundryAutomations(site("user-ada"), req)
		require.Equal(t, codes.InvalidArgument, status.Code(err), name)
	}

	store.stats = &foundrymodel.FoundryAutomationRunStats{ByAutomation: map[string]foundrymodel.FoundryAutomationRunCounts{
		a.GetId(): {Runs: 4, Succeeded: 2, Failed: 1, USDMicros: 1500, Waiting: 1},
		"gone":    {Runs: 1, Succeeded: 1},
	}}
	// Its owner, and owners and admins, count the actions its runs left
	// waiting; another member counts the runs only. The totals are the
	// listed automations': a run of one gone counts nowhere.
	for user, waiting := range map[string]int32{"user-ada": 1, "user-admin": 1, "user-alan": 0} {
		list, err = s.ListFoundryAutomations(site(user), &foundryv1.ListFoundryAutomationsRequest{WorkspaceId: "ws-1", StatsDays: 7})
		require.NoError(t, err)
		require.Equal(t, &foundryv1.FoundryAutomationRunCounts{Runs: 4, Succeeded: 2, Failed: 1, UsdMicros: 1500, Waiting: waiting},
			list.GetAutomations()[0].GetStats(), user)
		require.Equal(t, &foundryv1.FoundryAutomationRunCounts{Runs: 4, Succeeded: 2, Failed: 1, UsdMicros: 1500, Waiting: waiting},
			list.GetSummary().GetTotals(), user)
	}
	summary := list.GetSummary()
	require.Equal(t, int32(7), summary.GetDays())
	require.Len(t, summary.GetByDay(), 7)
	require.Equal(t, time.Now().UTC().Format(time.DateOnly), summary.GetByDay()[6].GetDate(), "today is the last day")
	require.Equal(t, statsFrom(time.Now(), time.UTC, 7), store.statsSince)
}

// The days begin at the time zone's midnights: a run counts on the day it
// started there, however the zone's offset falls on the quarter hour.
func TestRunSummaryCountsByTheZonesDays(t *testing.T) {
	kathmandu, err := time.LoadLocation("Asia/Kathmandu") // UTC+05:45
	require.NoError(t, err)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, kathmandu)
	from := statsFrom(now, kathmandu, 3)
	require.Equal(t, time.Date(2026, 11, 13, 0, 0, 0, 0, kathmandu), from)
	slot := func(at time.Time, status foundrymodel.FoundryAutomationRunStatus, n int) foundrymodel.FoundryAutomationRunSlot {
		return foundrymodel.FoundryAutomationRunSlot{At: at.UTC(), Status: status, Runs: n}
	}
	summary := runSummary([]foundrymodel.FoundryAutomationRunSlot{
		slot(time.Date(2026, 11, 13, 0, 0, 0, 0, kathmandu), foundrymodel.FoundryAutomationRunSucceeded, 2),
		slot(time.Date(2026, 11, 13, 23, 45, 0, 0, kathmandu), foundrymodel.FoundryAutomationRunFailed, 1),
		slot(time.Date(2026, 11, 14, 0, 0, 0, 0, kathmandu), foundrymodel.FoundryAutomationRunSkipped, 3),
		slot(time.Date(2026, 11, 15, 11, 30, 0, 0, kathmandu), foundrymodel.FoundryAutomationRunRunning, 1),
	}, foundrymodel.FoundryAutomationRunCounts{Runs: 7}, from, kathmandu, 3)
	require.Equal(t, []*foundryv1.FoundryAutomationRunDay{
		{Date: "2026-11-13", Succeeded: 2, Failed: 1},
		{Date: "2026-11-14", Other: 3},
		{Date: "2026-11-15", Other: 1},
	}, summary.GetByDay())
	require.Equal(t, &foundryv1.FoundryAutomationRunCounts{Runs: 7}, summary.GetTotals(), "the totals are as given")

	// A change of daylight saving keeps each day whole: London's day after
	// the clocks went back is 25 hours long.
	london, err := time.LoadLocation("Europe/London")
	require.NoError(t, err)
	from = statsFrom(time.Date(2026, 10, 26, 9, 0, 0, 0, london), london, 2)
	summary = runSummary([]foundrymodel.FoundryAutomationRunSlot{
		slot(time.Date(2026, 10, 25, 23, 45, 0, 0, london), foundrymodel.FoundryAutomationRunSucceeded, 1),
	}, foundrymodel.FoundryAutomationRunCounts{Runs: 1}, from, london, 2)
	require.Equal(t, "2026-10-25", summary.GetByDay()[0].GetDate())
	require.Equal(t, int32(1), summary.GetByDay()[0].GetSucceeded())
}

// The person's Run on an Assistant's card approves the automation it saved:
// exactly the steps the card showed, and nothing when it showed none.
func TestRunningAnAutomationCardApprovesIt(t *testing.T) {
	s, store := automationService(t)
	a := createAutomation(t, s, site("user-ada"), weeklyAutomation)
	succeeded := func(result string) foundrymodel.FoundryAgentActionEnd {
		return foundrymodel.FoundryAgentActionEnd{Status: foundrymodel.FoundryAgentActionSucceeded, Result: json.RawMessage(result)}
	}
	ran := succeeded(fmt.Sprintf(`{"result":{"id":%q,"revision":1}}`, a.GetId()))
	shown := json.RawMessage(`{"name":"Weekly","spec":` + weeklyAutomation + `}`)

	// A rename's card showed no steps: the automation still waits for its owner.
	end := s.approveOnClick(context.Background(), "ws-1", "user-ada", json.RawMessage(`{"expected_revision":1,"name":"Renamed"}`), ran)
	require.Empty(t, end.Error)
	require.Equal(t, foundrymodel.FoundryAutomationNeedsApproval, store.automations[a.GetId()].Status, "a card without steps approves none")
	require.Nil(t, store.approvedNext)

	// A card whose steps aren't the automation's approves nothing either.
	other := strings.Replace(weeklyAutomation, "Summarise the overdue invoices", "Something else", 1)
	end = s.approveOnClick(context.Background(), "ws-1", "user-ada", json.RawMessage(`{"spec":`+other+`}`), ran)
	require.Equal(t, foundrymodel.FoundryAgentActionSucceeded, end.Status, "the save stands")
	require.Contains(t, end.Error, "aren't the ones the card showed")
	require.Equal(t, foundrymodel.FoundryAutomationNeedsApproval, store.automations[a.GetId()].Status)
	end = s.approveOnClick(context.Background(), "ws-1", "user-ada", shown, succeeded(fmt.Sprintf(`{"result":{"id":%q,"revision":2}}`, a.GetId())))
	require.Contains(t, end.Error, "aren't the ones the card showed", "nor one at another revision than the save's")
	for _, unreadable := range []string{`[]`, `{"spec":{"v":9}}`} {
		end = s.approveOnClick(context.Background(), "ws-1", "user-ada", json.RawMessage(unreadable), ran)
		require.Contains(t, end.Error, "can't be read")
	}

	moved := strings.Replace(weeklyAutomation, `"x":200`, `"x":260`, 1)
	end = s.approveOnClick(context.Background(), "ws-1", "user-ada", json.RawMessage(`{"spec":`+moved+`}`), ran)
	require.Empty(t, end.Error, "the steps the card showed, wherever it drew them")
	require.Equal(t, foundrymodel.FoundryAutomationActive, store.automations[a.GetId()].Status)
	require.NotNil(t, store.approvedNext)

	end = s.approveOnClick(context.Background(), "ws-1", "user-ada", shown, succeeded(`{"result":{}}`))
	require.Equal(t, foundrymodel.FoundryAgentActionSucceeded, end.Status, "the save stands")
	require.Contains(t, end.Error, "approve it on its page")
	store.refuse = foundrymodel.FoundryAutomationRefusal{Reason: foundrymodel.FoundryAutomationChanged, Detail: "it changed"}
	end = s.approveOnClick(context.Background(), "ws-1", "user-ada", shown, ran)
	require.Contains(t, end.Error, "not approved (it changed)")
}
