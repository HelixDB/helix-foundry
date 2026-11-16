package integration

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// memConfirmations prepares and consumes confirmations as a host's must: bound
// to the operation, target, audience and payload, spent once.
type memConfirmations struct {
	mu       sync.Mutex
	bindings map[string]ConfirmationBinding
	prepared int
}

func (c *memConfirmations) Prepare(_ context.Context, b ConfirmationBinding) (Confirmation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prepared++
	if c.bindings == nil {
		c.bindings = map[string]ConfirmationBinding{}
	}
	id := "conf-" + string(rune('a'+c.prepared))
	b.ID, b.Token = id, "hlxc_"+id
	c.bindings[id] = b
	return Confirmation{ID: id, Token: b.Token, ExpiresAt: time.Now().Add(5 * time.Minute)}, nil
}

func (c *memConfirmations) Consume(_ context.Context, b ConfirmationBinding) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bindings[b.ID] != b {
		return status.Error(codes.FailedPrecondition, "confirmation does not match")
	}
	delete(c.bindings, b.ID)
	return nil
}

// memSink keeps what the agent prepares, allowing create_load alone.
type memSink struct {
	recorded []PreparedAction
}

func (s *memSink) Allows(operation string) bool { return operation == "create_load" }

func (s *memSink) Record(_ context.Context, a PreparedAction) (Recorded, error) {
	s.recorded = append(s.recorded, a)
	return AwaitingPerson{ActionID: "act-1", ExpiresAt: a.Confirmation.ExpiresAt}, nil
}

// loadActions makes loads, answering with err when set.
type loadActions struct {
	FoundryActionAPI
	err   error
	calls int
}

func (a *loadActions) CreateFoundryLoad(_ context.Context, r *foundryv1.CreateFoundryLoadRequest, _ ...grpc.CallOption) (*foundryv1.FoundryLoadResponse, error) {
	a.calls++
	if a.err != nil {
		return nil, a.err
	}
	return &foundryv1.FoundryLoadResponse{Load: &foundryv1.FoundryLoad{Id: "load-1", VersionId: r.GetVersionId()}}, nil
}

func personIdentity(sink ActionSink) *Identity {
	identity := reader()
	identity.Confirm = ConfirmByPerson{Sink: sink}
	return identity
}

var loadInput = FoundryOperationInput{Operation: "create_load", Target: "ontology:ws1/ont1", Payload: map[string]any{"version_id": "ver1"}}

func TestAPersonsAgentPreparesAndNeverExecutes(t *testing.T) {
	confirmations, sink := &memConfirmations{}, &memSink{}
	s := (&Service{Confirmations: confirmations, Actions: &loadActions{}}).ForAgent()
	session := connect(t, s, personIdentity(sink))
	tools, err := session.ListTools(context.Background(), nil)
	require.NoError(t, err)
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	assert.Contains(t, names, toolPrepareFoundry)
	assert.NotContains(t, names, toolExecuteFoundry)
	assert.NotContains(t, names, toolFoundryCompleteUpload)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: toolPrepareFoundry, Arguments: map[string]any{
		"operation": loadInput.Operation, "target": loadInput.Target, "payload": loadInput.Payload,
	}})
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	raw, err := json.Marshal(result)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "hlxc_", "the agent never sees the token")
	assert.NotContains(t, string(raw), "confirmation")
	assert.Contains(t, string(raw), `"awaiting_person"`)
	require.Len(t, sink.recorded, 1)
	kept := sink.recorded[0]
	assert.Equal(t, "hlxc_conf-b", kept.Confirmation.Token)
	assert.JSONEq(t, `{"version_id":"ver1"}`, string(kept.Payload))
	assert.Equal(t, AgentAudience, confirmations.bindings[kept.Confirmation.ID].Audience)

	// An operation the sink doesn't allow is refused before a confirmation is spent.
	refused, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: toolPrepareFoundry, Arguments: map[string]any{
		"operation": "sync_connector", "target": "connector:ws1/c1", "payload": map[string]any{},
	}})
	require.NoError(t, err)
	assert.True(t, refused.IsError)
	assert.Equal(t, 1, confirmations.prepared)

	// Execute refuses a person's agent outright, whatever it holds.
	_, _, err = s.ExecuteFoundryOperation(context.Background(), personIdentity(sink), &Call{}, ExecuteFoundryInput{
		Operation: loadInput.Operation, Target: loadInput.Target, Payload: loadInput.Payload, ConfirmationID: kept.Confirmation.ID,
		ConfirmationToken: kept.Confirmation.Token,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only the person runs")
}

func TestExecuteForPersonSaysHowItEnded(t *testing.T) {
	prepare := func(t *testing.T, s *Service) ExecuteFoundryInput {
		t.Helper()
		sink := &memSink{}
		_, err := s.prepareForPerson(context.Background(), personIdentity(sink), &Call{}, loadInput, sink)
		require.NoError(t, err)
		kept := sink.recorded[0]
		return ExecuteFoundryInput{Operation: kept.Operation, Target: kept.Target, Payload: loadInput.Payload,
			ConfirmationID: kept.Confirmation.ID, ConfirmationToken: kept.Confirmation.Token}
	}
	t.Run("ok, once", func(t *testing.T) {
		actions := &loadActions{}
		s := (&Service{Confirmations: &memConfirmations{}, Actions: actions}).ForAgent()
		input := prepare(t, s)
		ok, isOK := s.ExecuteForPerson(context.Background(), reader(), input).(ExecutedOK)
		require.True(t, isOK)
		assert.Equal(t, "create_load", ok.Result.Operation)
		// The same confirmation again is spent: nothing runs.
		again, failed := s.ExecuteForPerson(context.Background(), reader(), input).(ExecutedFailed)
		require.True(t, failed)
		assert.Error(t, again.Err)
		assert.Equal(t, 1, actions.calls)
	})
	t.Run("another payload is refused before the backend", func(t *testing.T) {
		actions := &loadActions{}
		s := (&Service{Confirmations: &memConfirmations{}, Actions: actions}).ForAgent()
		input := prepare(t, s)
		input.Payload = map[string]any{"version_id": "ver2"}
		_, failed := s.ExecuteForPerson(context.Background(), reader(), input).(ExecutedFailed)
		assert.True(t, failed)
		assert.Zero(t, actions.calls)
	})
	t.Run("a refusal after the confirmation fails", func(t *testing.T) {
		s := (&Service{Confirmations: &memConfirmations{}, Actions: &loadActions{err: status.Error(codes.FailedPrecondition, "over the budget")}}).ForAgent()
		failed, ok := s.ExecuteForPerson(context.Background(), reader(), prepare(t, s)).(ExecutedFailed)
		require.True(t, ok)
		assert.Contains(t, failed.Err.Error(), "over the budget")
	})
	t.Run("a lost answer is unknown", func(t *testing.T) {
		for _, err := range []error{status.Error(codes.Unavailable, "gone"), status.Error(codes.Internal, "boom"), context.DeadlineExceeded} {
			s := (&Service{Confirmations: &memConfirmations{}, Actions: &loadActions{err: err}}).ForAgent()
			_, unknown := s.ExecuteForPerson(context.Background(), reader(), prepare(t, s)).(ExecutedUnknown)
			assert.True(t, unknown, "%v", err)
		}
	})
	t.Run("the clicker is authorized again", func(t *testing.T) {
		actions := &loadActions{}
		s := (&Service{Confirmations: &memConfirmations{}, Actions: actions}).ForAgent()
		input := prepare(t, s)
		denied := reader()
		denied.Authorize = func(context.Context, any, Grant, ...string) error { return errors.New("not theirs") }
		_, failed := s.ExecuteForPerson(context.Background(), denied, input).(ExecutedFailed)
		assert.True(t, failed)
		assert.Zero(t, actions.calls)
	})
}

func TestPrepareAgainHandsTheSinkANewConfirmation(t *testing.T) {
	confirmations, sink := &memConfirmations{}, &memSink{}
	s := (&Service{Confirmations: confirmations}).ForAgent()
	got, err := s.PrepareAgain(context.Background(), reader(), loadInput, sink)
	require.NoError(t, err)
	assert.Equal(t, "act-1", got.(AwaitingPerson).ActionID)
	require.Len(t, sink.recorded, 1)
	_, err = s.PrepareAgain(context.Background(), reader(), FoundryOperationInput{Operation: "create_load", Target: "ontology:ws1/ont1", Payload: map[string]any{}}, sink)
	assert.Error(t, err, "a payload prepare refuses is refused again")
}

func TestForAgentIsACopy(t *testing.T) {
	s := &Service{ResourceURI: "http://localhost:8081/mcp", SignIn: true}
	agent := s.ForAgent()
	assert.Equal(t, AgentAudience, agent.ResourceURI)
	assert.False(t, agent.SignIn)
	assert.Nil(t, agent.Workflows)
	assert.Equal(t, "http://localhost:8081/mcp", s.ResourceURI, "the host's own service is untouched")
}

func TestPreparedForPersonSaysHowAnApprovedActionRan(t *testing.T) {
	ran := preparedForPerson(RanApproved{ActionID: "act-2", Status: "succeeded", Result: []byte(`{"load_id":"l1"}`)})
	assert.Equal(t, "ran", ran.Status)
	assert.Equal(t, "succeeded", ran.Outcome)
	assert.Equal(t, ranApprovedNext, ran.Next)
	assert.NotNil(t, ran.Result)
	assert.Empty(t, ran.ExpiresAt)
	failed := preparedForPerson(RanApproved{ActionID: "act-3", Status: "failed", Error: "refused"})
	assert.Equal(t, "refused", failed.Error)
	assert.Nil(t, failed.Result)
	waiting := preparedForPerson(AwaitingPerson{ActionID: "act-1", ExpiresAt: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)})
	assert.Equal(t, "awaiting_person", waiting.Status)
	assert.Equal(t, "2026-11-14T09:00:00Z", waiting.ExpiresAt)
	assert.Empty(t, waiting.Outcome)
}

// An action Founder ran at once because its person asked for it is told
// apart from one approved beforehand: the agent gives its link, and says the
// person can undo it.
func TestPreparedForPersonSaysWhenAnActionRanBecauseItWasAskedFor(t *testing.T) {
	made := preparedForPerson(RanApproved{ActionID: "act-4", Status: "succeeded", Result: []byte(`{"result":{"id":"dash1"}}`), Auto: true})
	assert.Equal(t, "ran", made.Status)
	assert.Equal(t, "succeeded", made.Outcome)
	assert.Equal(t, ranAutoNext, made.Next)
	assert.Contains(t, made.Next, "because the person asked for one")
	assert.Contains(t, made.Next, "it isn't pinned or published")
	assert.Contains(t, made.Next, "undo")
	assert.Contains(t, made.Next, "/dashboards/<id>")
	assert.NotNil(t, made.Result)
	for _, outcome := range []string{"failed", "unknown"} {
		unsure := preparedForPerson(RanApproved{ActionID: "act-5", Status: outcome, Error: "refused", Auto: true})
		assert.Equal(t, ranAutoUnsureNext, unsure.Next, outcome)
		assert.NotContains(t, unsure.Next, "made this new dashboard", "it doesn't say it made what it may not have")
	}
	assert.Equal(t, ranApprovedNext, preparedForPerson(RanApproved{ActionID: "act-6", Status: "succeeded"}).Next, "a workflow's stays as it was")
}

func TestRunApprovedPreparesAndRunsOnce(t *testing.T) {
	actions := &loadActions{}
	s := (&Service{Confirmations: &memConfirmations{}, Actions: actions}).ForAgent()
	done := s.RunApproved(context.Background(), reader(), loadInput)
	ok, isOK := done.(ExecutedOK)
	require.True(t, isOK, "%#v", done)
	assert.Equal(t, "create_load", ok.Result.Operation)
	assert.Equal(t, 1, actions.calls)

	refused := s.RunApproved(context.Background(), reader(), FoundryOperationInput{Operation: "create_load", Target: "ontology:ws1/ont1", Payload: map[string]any{}})
	assert.IsType(t, ExecutedFailed{}, refused, "a payload prepare refuses runs nothing")
	assert.Equal(t, 1, actions.calls)

	assert.IsType(t, ExecutedFailed{}, s.RunApproved(context.Background(), personIdentity(&memSink{}), loadInput),
		"an identity whose person confirms never runs it")

	actions.err = status.Error(codes.Unavailable, "the backend went away")
	assert.IsType(t, ExecutedUnknown{}, s.RunApproved(context.Background(), reader(), loadInput), "its answer lost after the confirmation was spent")
	actions.err = status.Error(codes.FailedPrecondition, "the version can't be loaded")
	assert.IsType(t, ExecutedFailed{}, s.RunApproved(context.Background(), reader(), loadInput))
}
