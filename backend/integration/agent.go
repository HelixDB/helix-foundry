package integration

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/helixdb/helix-foundry/backend/ports"
)

// AgentAudience is the audience of the confirmations Foundry's own agent (the
// Assistant) prepares. MCP clients' confirmations carry their host's
// ResourceURI, so neither can spend the other's.
const AgentAudience = "foundry-agent"

// ConfirmMode is who confirms an action prepare checked: a closed set.
type ConfirmMode interface{ confirmMode() }

// ConfirmByCaller is the MCP's: prepare returns the confirmation to the
// caller, which executes it. A nil Identity.Confirm is this mode.
type ConfirmByCaller struct{}

// ConfirmByPerson is the Assistant's: prepare hands the confirmation to Sink,
// for the person the agent works for to run with a click, and returns the
// agent no token; Execute refuses, and complete_document_upload isn't offered.
type ConfirmByPerson struct{ Sink ActionSink }

func (ConfirmByCaller) confirmMode() {}
func (ConfirmByPerson) confirmMode() {}

// PreparedAction is an action prepare checked and confirmed, with its
// one-time confirmation: Sink keeps the token from the agent.
type PreparedAction struct {
	Operation, Target string
	// Payload is canonical: the same action has the same bytes and PayloadHash.
	Payload      []byte
	PayloadHash  string
	Confirmation Confirmation
}

// AwaitingPerson is a prepared action waiting for its person.
type AwaitingPerson struct {
	ActionID  string
	ExpiresAt time.Time
}

// ActionSink keeps what the agent prepares for its person.
type ActionSink interface {
	// Allows reports whether the agent may prepare operation at all, before
	// anything is checked or a confirmation spent.
	Allows(operation string) bool
	// Record keeps the action and its confirmation for its person. Recording
	// the same payload again in a turn returns the action already kept.
	Record(ctx context.Context, action PreparedAction) (AwaitingPerson, error)
}

// AwaitingPersonOutput is what the agent learns of an action it prepared: no
// token, only that its person decides.
type AwaitingPersonOutput struct {
	ActionID  string `json:"action_id"`
	Status    string `json:"status" jsonschema:"awaiting_person: a card shows the person the action, which runs only if they click Run."`
	ExpiresAt string `json:"expires_at"`
	Next      string `json:"next"`
}

// awaitingPersonNext tells the agent what happens to its action.
const awaitingPersonNext = "The person sees this action as a card and runs it, or not, with a click; you can't run it. " +
	"Say in a sentence what it will do and stop: when they run it, you are told its result and go on from there."

// Session is the tools one agent turn or one click acts with, as one user:
// Service and Identity add Foundry's tools to a server, HostTools the host's
// own (helix_list_projects and helix_list_databases), and every call runs
// with Ctx, which carries what the host's calls and confirmations need.
type Session struct {
	Ctx       context.Context
	Service   *Service
	Identity  *Identity
	HostTools func(*mcp.Server)
}

// SessionHost opens a Session for actor, a user, confirming as confirm. It is
// called on every attempt of an agent's turn and on every click, so what the
// tools may do is what the user may do now.
//
// A host implements it as its MCP builds a user's tools, with three
// differences: the Service is ForAgent's copy (audience AgentAudience, no
// sign-in tools, no workflow tools); the Identity's Confirm is confirm; and
// the Identity is a User, never an Agent, whose callbacks (HasPermission,
// Authorize, Allows, WithScope, OnlyWorkspace) decide as they would for that
// user calling the MCP. Hosted Helix resolves actor.UserID to the user's
// sign-in identity, hydrates their memberships (failing closed: an error, not
// fewer tools), and sets Ctx up as its MCP's request context: the principal
// its Confirmations bind (the worker that prepares and the API that consumes
// must bind the same principal for the same user), and its audit source
// "foundry-agent". It refuses an actor that isn't a user of actor.WorkspaceID,
// and its Identity is bound to that workspace: Authorize refuses (not found)
// every request naming another, and Allows allows none, though the user may
// belong to it, so a turn reads and acts in its own workspace only.
type SessionHost interface {
	Session(ctx context.Context, actor ports.Actor, confirm ConfirmMode) (Session, error)
}

// ForAgent is s as the Assistant's tools use it: its confirmations' audience
// is AgentAudience, and it offers neither connecting by signing in nor
// workflows, which record an outside agent's runs.
func (s *Service) ForAgent() *Service {
	c := *s
	c.ResourceURI, c.SignIn, c.Workflows = AgentAudience, false, nil
	return &c
}

// prepareForPerson checks an action, prepares its confirmation and hands it
// to sink, returning the agent the action's id and no token.
func (s *Service) prepareForPerson(ctx context.Context, identity *Identity, call *Call, input FoundryOperationInput, sink ActionSink) (
	AwaitingPerson, error) {
	call.ResourceKind, call.ResourceID = TargetKind(input.Target), input.Target
	if !sink.Allows(input.Operation) {
		return AwaitingPerson{}, detailedToolError{code: errCodeInvalidArguments,
			message: "the Assistant can't prepare " + input.Operation + " here: tell the person how to do it in Foundry instead"}
	}
	operation, err := s.ValidateFoundryOperation(ctx, identity, input)
	if err != nil {
		return AwaitingPerson{}, err
	}
	payload := operation.CanonicalPayload()
	payloadHash := confirmationPayloadHash(foundryOperationPrefix+input.Operation, input.Target, payload)
	prepared, err := s.Confirmations.Prepare(ctx, ConfirmationBinding{
		Operation: foundryOperationPrefix + input.Operation, Target: input.Target, Audience: s.ResourceURI, PayloadHash: payloadHash,
	})
	if err != nil {
		return AwaitingPerson{}, FoundryError(err)
	}
	return sink.Record(ctx, PreparedAction{
		Operation: input.Operation, Target: input.Target, Payload: payload, PayloadHash: payloadHash, Confirmation: prepared,
	})
}

// PrepareAgain prepares an action its person's confirmation ran out on, as
// that person and with no model, and hands it to sink as prepare does.
func (s *Service) PrepareAgain(ctx context.Context, identity *Identity, input FoundryOperationInput, sink ActionSink) (AwaitingPerson, error) {
	var out AwaitingPerson
	var raw error
	_, _, _ = s.Run(ctx, toolPrepareFoundry, func(ctx context.Context, call *Call) (any, int, error) {
		out, raw = s.prepareForPerson(ctx, identity, call, input, sink)
		if raw != nil {
			return nil, 0, raw
		}
		return out.ActionID, 1, nil
	})
	return out, raw
}

// Executed is how running a confirmed action ended: a closed set.
type Executed interface{ executed() }

// ExecutedOK ran, and Result is the operation's untrusted result.
type ExecutedOK struct{ Result FoundryOperationOutput }

// ExecutedFailed made no change Foundry knows of: the action was refused
// before its confirmation was spent, or the backend refused it after.
type ExecutedFailed struct{ Err error }

// ExecutedUnknown spent its confirmation, then lost the backend's answer: it
// may or may not have taken effect, and must never run again.
type ExecutedUnknown struct{ Err error }

func (ExecutedOK) executed()      {}
func (ExecutedFailed) executed()  {}
func (ExecutedUnknown) executed() {}

// ExecuteForPerson runs the action its person clicked, with the confirmation
// prepare made: validated and authorized again as identity (the person, now),
// its confirmation consumed once, and the backend called once, under Run's
// audit as the execute tool. identity confirms as the caller: it holds the
// token.
func (s *Service) ExecuteForPerson(ctx context.Context, identity *Identity, input ExecuteFoundryInput) Executed {
	var out FoundryOperationOutput
	var consumed bool
	var raw error
	_, _, _ = s.Run(ctx, toolExecuteFoundry, func(ctx context.Context, call *Call) (any, int, error) {
		var count int
		out, count, consumed, raw = s.executeFoundry(ctx, identity, call, input)
		if raw != nil {
			return nil, 0, raw
		}
		return out, count, nil
	})
	switch {
	case raw == nil:
		return ExecutedOK{Result: out}
	case consumed && unsureOfOutcome(raw):
		return ExecutedUnknown{Err: ToolError(raw)}
	default:
		return ExecutedFailed{Err: ToolError(raw)}
	}
}

// unsureOfOutcome reports a failure that doesn't say the call made no
// change: no answer, a timeout, or an internal error the backend may return
// after its write committed.
func unsureOfOutcome(err error) bool {
	switch stableErrorCode(err) {
	case errCodeBackendUnavailable, errCodeInternal, errCodeRequestTimeout:
		return true
	}
	return false
}
