package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
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

// ConfirmByAdmin is a Slack install's bot's (bot mode): prepare checks
// nothing as the bot and confirms nothing; it hands the action to Sink as a
// proposal, which an owner or admin reviews and runs as themselves
// (RunSlackApproved), or not. Execute refuses, and complete_document_upload
// isn't offered.
type ConfirmByAdmin struct{ Sink ProposalSink }

func (ConfirmByCaller) confirmMode() {}
func (ConfirmByPerson) confirmMode() {}
func (ConfirmByAdmin) confirmMode()  {}

// PreparedAction is an action prepare checked and confirmed, with its
// one-time confirmation: Sink keeps the token from the agent.
type PreparedAction struct {
	Operation, Target string
	// Payload is canonical: the same action has the same bytes and PayloadHash.
	Payload      []byte
	PayloadHash  string
	Confirmation Confirmation
}

// Recorded is what became of an action the agent prepared: a closed set.
type Recorded interface{ recorded() }

// AwaitingPerson is a prepared action waiting for its person.
type AwaitingPerson struct {
	ActionID  string
	ExpiresAt time.Time
}

// RanApproved is an action that ran at once, as its person: a workflow's
// owner approved it for the step whose turn prepared it, or (Auto) the
// person's own message asked for it and Founder's auto-run rule lets it run
// without a card (a new dashboard, which they can undo). Status is how it
// ended (succeeded, failed, or unknown: it lost its answer after its
// confirmation was spent), Result its untrusted result, Error why it didn't
// succeed.
type RanApproved struct {
	ActionID string
	Status   string
	Result   json.RawMessage
	Error    string
	Auto     bool
}

func (AwaitingPerson) recorded() {}
func (RanApproved) recorded()    {}

// ActionSink keeps what the agent prepares for its person.
type ActionSink interface {
	// Allows reports whether the agent may prepare operation at all, before
	// anything is checked or a confirmation spent.
	Allows(operation string) bool
	// Record keeps the action and its confirmation for its person, or runs it
	// at once when they approved it beforehand. Recording the same payload
	// again in a turn returns what became of the action already kept.
	Record(ctx context.Context, action PreparedAction) (Recorded, error)
}

// ProposedAction is an action a bot proposed: the operation, a target the bot
// reads, and its payload as the model wrote it (keys sorted), unchecked and
// unconfirmed: the owner or admin who runs it checks it as themselves.
// PayloadHash binds the payload a click on the proposal names.
type ProposedAction struct {
	Operation, Target string
	Payload           []byte
	PayloadHash       string
}

// AwaitingAdmin is a proposal waiting for an owner or admin until ExpiresAt.
type AwaitingAdmin struct {
	ActionID  string
	ExpiresAt time.Time
}

// ProposalSink keeps what a bot proposes, for an owner or admin.
type ProposalSink interface {
	// Allows reports whether the bot may propose operation at all, before
	// anything is read.
	Allows(operation string) bool
	// Propose keeps the proposal. Proposing the same payload again in a turn
	// returns the proposal already kept while it waits.
	Propose(ctx context.Context, action ProposedAction) (AwaitingAdmin, error)
}

// ProposedForAdminOutput is what a bot's model learns of an action it
// proposed: only that an owner or admin decides.
type ProposedForAdminOutput struct {
	ActionID  string `json:"action_id"`
	Status    string `json:"status" jsonschema:"awaiting_admin: an owner or admin reviews the action in Slack and runs it as themselves, or not."`
	ExpiresAt string `json:"expires_at"`
	Next      string `json:"next"`
}

// awaitingAdminNext tells a bot's model what happens to its proposal.
const awaitingAdminNext = "An owner or admin reviews this in Slack and runs it as themselves, or not; you're told the result. " +
	"Say in a sentence what it will do and stop."

// maxProposalBytes bounds a proposal's payload.
const maxProposalBytes = 32 << 10

// PreparedForPersonOutput is what the agent learns of an action it prepared:
// no token, only that its person decides, or how it ran when they approved
// it beforehand.
type PreparedForPersonOutput struct {
	ActionID string `json:"action_id"`
	Status   string `json:"status" jsonschema:"awaiting_person: a card shows the person the action, which runs only if they click Run; or ran: it ran at once, because its person approved it beforehand (a workflow's step) or asked for it (a new dashboard they asked for), and next says which."`
	// ExpiresAt is an awaiting action's.
	ExpiresAt string `json:"expires_at,omitempty"`
	// Outcome, Result and Error are a ran action's.
	Outcome string `json:"outcome,omitempty" jsonschema:"succeeded, failed, or unknown (it may or may not have taken effect: read its state before saying which, and never prepare it again until you know)."`
	// See FoundryVersionOutput.Definition: the description keeps the schema an object.
	Result any    `json:"result,omitempty" jsonschema:"Untrusted: what the action made or changed."`
	Error  string `json:"error,omitempty"`
	Next   string `json:"next"`
}

// awaitingPersonNext tells the agent what happens to its action.
const awaitingPersonNext = "The person sees this action as a card and runs it, or not, with a click; you can't run it. " +
	"Say in a sentence what it will do and stop: when they run it, you are told its result and go on from there."

// ranApprovedNext tells the agent what happened to its action.
const ranApprovedNext = "Its person approved this action beforehand, so it ran. Go on from its outcome."

// ranAutoNext tells the agent what happened to an action that ran at once because its person asked for it.
const ranAutoNext = "Foundry made this new dashboard at once, because the person asked for one; it isn't pinned or published, " +
	"and they can undo it from the conversation. Give them its link (/dashboards/<id>) in a sentence."

// ranAutoUnsureNext tells the agent an action that ran at once because its person asked for it didn't succeed, or may not have.
const ranAutoUnsureNext = "Foundry ran this at once, because the person asked for it, and it didn't succeed or may not have. Go on from its outcome."

// preparedForPerson is what the agent reads of r.
func preparedForPerson(r Recorded) PreparedForPersonOutput {
	switch r := r.(type) {
	case RanApproved:
		out := PreparedForPersonOutput{ActionID: r.ActionID, Status: "ran", Outcome: r.Status, Error: r.Error, Next: ranApprovedNext}
		switch {
		case r.Auto && r.Status == "succeeded":
			out.Next = ranAutoNext
		case r.Auto:
			out.Next = ranAutoUnsureNext
		}
		if len(r.Result) > 0 {
			out.Result = UntrustedJSON(string(r.Result))
		}
		return out
	case AwaitingPerson:
		return PreparedForPersonOutput{ActionID: r.ActionID, Status: "awaiting_person", ExpiresAt: r.ExpiresAt.UTC().Format(time.RFC3339Nano),
			Next: awaitingPersonNext}
	}
	panic(fmt.Sprintf("integration: no output of %T", r))
}

// FounderAPI is Founder's own RPCs, which Helix Foundry's Slack bot calls as
// the person a session is of: it starts their turns, reads what a turn
// showed, and runs, dismisses or prepares again the actions they click,
// stops their turns, and approves the workflow a bot's proposal they ran
// made, all through Foundry's API, so every check the site's
// calls pass (membership, the turn's thread being theirs, the hour's turns,
// the workspace's budget, a restricted member's reach) holds in Slack too.
// Every call runs with its Session's Ctx.
//
// A host serves it from the same API as the site: hosted Helix with a
// FoundryServiceClient on its backend connection that forwards the session's
// user (ports.SourceUser), a standalone install with its own gRPC client
// carrying the site token, which acts as the install's owner.
type FounderAPI interface {
	SendFoundryAgentMessage(context.Context, *foundryv1.SendFoundryAgentMessageRequest, ...grpc.CallOption) (*foundryv1.FoundryAgentTurnResponse, error)
	ListFoundryAgentTurnEvents(context.Context, *foundryv1.ListFoundryAgentTurnEventsRequest, ...grpc.CallOption) (
		*foundryv1.ListFoundryAgentTurnEventsResponse, error)
	ExecuteFoundryAgentAction(context.Context, *foundryv1.ExecuteFoundryAgentActionRequest, ...grpc.CallOption) (*foundryv1.FoundryAgentActionResponse, error)
	DismissFoundryAgentAction(context.Context, *foundryv1.DismissFoundryAgentActionRequest, ...grpc.CallOption) (*foundryv1.FoundryAgentActionResponse, error)
	RefreshFoundryAgentAction(context.Context, *foundryv1.RefreshFoundryAgentActionRequest, ...grpc.CallOption) (*foundryv1.FoundryAgentActionResponse, error)
	CancelFoundryAgentTurn(context.Context, *foundryv1.CancelFoundryAgentTurnRequest, ...grpc.CallOption) (*foundryv1.FoundryAgentTurnResponse, error)
	// ApproveFoundryAutomation approves, as the owner or admin who ran a
	// bot's create_workflow or update_workflow, the revision their run
	// wrote: the steps they reviewed (agent.Proposals).
	ApproveFoundryAutomation(context.Context, *foundryv1.ApproveFoundryAutomationRequest, ...grpc.CallOption) (*foundryv1.FoundryAutomationResponse, error)
}

// The backend's own client serves FounderAPI, on either host.
var _ FounderAPI = foundryv1.FoundryServiceClient(nil)

// Session is the tools one agent turn or one click acts with, as one user:
// Service and Identity add Foundry's tools to a server, HostTools the host's
// own (helix_list_projects and helix_list_databases), and every call runs
// with Ctx, which carries what the host's calls and confirmations need.
// Founder is Founder's own API as the same user, for a caller that talks to
// Founder on their behalf (the Slack bot); nil where the host offers none,
// which such a caller refuses.
type Session struct {
	Ctx       context.Context
	Service   *Service
	Identity  *Identity
	HostTools func(*mcp.Server)
	Founder   FounderAPI
}

// SessionHost opens a Session for actor, a user, confirming as confirm. It is
// called on every attempt of an agent's turn and on every click, so what the
// tools may do is what the user may do now.
//
// A host implements it as its MCP builds a user's tools, with three
// differences: the Service is ForAgent's copy (audience AgentAudience, no
// sign-in tools, no use case tools); the Identity's Confirm is confirm; and
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
// belong to it, so a turn reads and acts in its own workspace only. Its
// Founder (FounderAPI) calls Foundry's API as that same user, with Ctx.
//
// A restricted member (source grants) talks to Founder too, read-only, which
// holds only while the host:
//  1. forwards a member calling Founder's thread routes as a user, never as
//     ports.SourceGrantee: Foundry's access check narrows a restricted member
//     itself, and Founder refuses a caller sent on as a grantee;
//  2. opens the Session for any member of the workspace, its Service's
//     Foundry calling as that user, so the access check narrows every tool
//     call and GetMyFoundryAccess answers Grantee for a restricted member,
//     which the worker reads as the turn's Reach (agent.Reach);
//  3. builds the Identity as the member's MCP identity: IsGrantee and
//     AsGrantee set, and FoundryGrants while their grants give them
//     something, so the read tools register for a member without query read.
//
// A Slack install's bot (bot mode) opens a grantee session: actor is
// ports.SourceGrantee with its ServiceCredentialID and no UserID, and the
// Identity is that key's MCP identity, which Foundry reads as the bot's
// scope says (the whole workspace, as access.Reader, or its source grants
// alone), FoundryGrants set while either gives it something; it confirms
// ConfirmByAdmin only. The host refuses a key that is
// revoked, expired, of another workspace, or holds database grants (which
// would read past Foundry's grants), with PermissionDenied.
type SessionHost interface {
	Session(ctx context.Context, actor ports.Actor, confirm ConfirmMode) (Session, error)
}

// ForAgent is s as the Assistant's tools use it: its confirmations' audience
// is AgentAudience, and it offers neither connecting by signing in nor
// use cases, which record an outside agent's runs.
func (s *Service) ForAgent() *Service {
	c := *s
	c.ResourceURI, c.SignIn, c.Workflows = AgentAudience, false, nil
	return &c
}

// prepareForPerson checks an action, prepares its confirmation and hands it
// to sink, returning the agent what became of it and no token.
func (s *Service) prepareForPerson(ctx context.Context, identity *Identity, call *Call, input FoundryOperationInput, sink ActionSink) (
	Recorded, error) {
	call.ResourceKind, call.ResourceID = TargetKind(input.Target), input.Target
	if !sink.Allows(input.Operation) {
		return nil, detailedToolError{code: errCodeInvalidArguments,
			message: "Founder can't prepare " + input.Operation + " here: tell the person how to do it in Foundry instead"}
	}
	operation, err := s.ValidateFoundryOperation(ctx, identity, input)
	if err != nil {
		return nil, err
	}
	payload := operation.CanonicalPayload()
	payloadHash := confirmationPayloadHash(foundryOperationPrefix+input.Operation, input.Target, payload)
	prepared, err := s.Confirmations.Prepare(ctx, ConfirmationBinding{
		Operation: foundryOperationPrefix + input.Operation, Target: input.Target, Audience: s.ResourceURI, PayloadHash: payloadHash,
	})
	if err != nil {
		return nil, FoundryError(err)
	}
	return sink.Record(ctx, PreparedAction{
		Operation: input.Operation, Target: input.Target, Payload: payload, PayloadHash: payloadHash, Confirmation: prepared,
	})
}

// PrepareAgain prepares an action its person's confirmation ran out on, as
// that person and with no model, and hands it to sink as prepare does.
func (s *Service) PrepareAgain(ctx context.Context, identity *Identity, input FoundryOperationInput, sink ActionSink) (Recorded, error) {
	var out Recorded
	var raw error
	_, _, _ = s.Run(ctx, toolPrepareFoundry, func(ctx context.Context, call *Call) (any, int, error) {
		out, raw = s.prepareForPerson(ctx, identity, call, input, sink)
		if raw != nil {
			return nil, 0, raw
		}
		return preparedForPerson(out).ActionID, 1, nil
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

// toolRunApproved is how the audit names an action a workflow's action step
// runs.
const toolRunApproved = "helix_foundry_workflow_action"

// toolRunAnswers is how the audit names the answers a person sent a
// generation from the Assistant's dock.
const toolRunAnswers = "helix_foundry_ask_person"

// RunApproved runs an action its person approved beforehand, a
// workflow's action step, as identity (that person, now, confirming as the
// caller): validated and authorized again, a confirmation prepared and
// consumed at once, and the backend called once, under Run's audit. It ends
// as ExecuteForPerson does; unknown is never run again.
func (s *Service) RunApproved(ctx context.Context, identity *Identity, input FoundryOperationInput) Executed {
	return s.runNow(ctx, identity, toolRunApproved, input)
}

// RunAnswers sends a generation the answers its person gave its questions
// in the Assistant's dock, an answer_generation_questions, as RunApproved
// runs an action: their click on Send answers is their consent.
func (s *Service) RunAnswers(ctx context.Context, identity *Identity, input FoundryOperationInput) Executed {
	return s.runNow(ctx, identity, toolRunAnswers, input)
}

// toolRunSlackApproved is how the audit names a Slack bot's proposal an
// owner or admin ran.
const toolRunSlackApproved = "helix_foundry_slack_action"

// RunSlackApproved runs a Slack bot's proposal as identity, the owner or
// admin who reviewed and ran it (confirming as the caller), as RunApproved
// runs a workflow's step: validated and authorized as them now, a
// confirmation prepared and consumed at once, the backend called once,
// under Run's audit. Their click on Run is their consent; nothing was
// checked or confirmed as the bot.
func (s *Service) RunSlackApproved(ctx context.Context, identity *Identity, input FoundryOperationInput) Executed {
	return s.runNow(ctx, identity, toolRunSlackApproved, input)
}

// proposeForAdmin hands a bot's action to sink as a proposal, refusing an
// operation sink doesn't allow, a target the bot can't read (only
// workspace:<id>, or an ontology, dashboard or workflow a read as the bot
// finds, which source grants narrow), and a payload over maxProposalBytes.
// It checks nothing else as the bot and prepares no confirmation: the owner
// or admin who runs it checks it as themselves (RunSlackApproved).
func (s *Service) proposeForAdmin(ctx context.Context, identity *Identity, call *Call, input FoundryOperationInput, sink ProposalSink) (
	AwaitingAdmin, error) {
	call.ResourceKind, call.ResourceID = TargetKind(input.Target), input.Target
	if !sink.Allows(input.Operation) {
		return AwaitingAdmin{}, detailedToolError{code: errCodeInvalidArguments,
			message: "the bot can't propose " + input.Operation + ": tell them how to do it in Foundry instead"}
	}
	payload, err := json.Marshal(input.Payload)
	switch {
	case input.Payload == nil || err != nil:
		return AwaitingAdmin{}, detailedToolError{code: errCodeInvalidArguments, message: "operation, target and a payload object are required"}
	case len(payload) > maxProposalBytes:
		return AwaitingAdmin{}, detailedToolError{code: errCodeInvalidArguments, message: fmt.Sprintf("the payload is over %d bytes", maxProposalBytes)}
	}
	unseen := detailedToolError{code: errCodeFailedPrecondition,
		message: "the bot can't see " + input.Target + ": it proposes only on the workspace (workspace:<id>) or what it reads, such as the ontologies helix_foundry_list_ontologies lists"}
	kind, _, _ := strings.Cut(input.Target, ":")
	if kind == "workspace" {
		if _, ok := cutTarget(input.Target, "workspace"); !ok {
			return AwaitingAdmin{}, unseen
		}
	} else {
		workspaceID, id, ok := CutFoundryTarget(input.Target, kind)
		if !ok {
			return AwaitingAdmin{}, unseen
		}
		var read error
		switch kind {
		case "ontology":
			_, _, read = s.GetFoundryOntology(ctx, identity, &Call{}, OntologyInput{WorkspaceID: workspaceID, OntologyID: id})
		case "dashboard":
			_, _, read = s.GetFoundryDashboard(ctx, identity, &Call{}, FoundryDashboardInput{WorkspaceID: workspaceID, DashboardID: id})
		case "workflow":
			_, _, read = s.GetFoundryAutomation(ctx, identity, &Call{}, FoundryAutomationInput{WorkspaceID: workspaceID, AutomationID: id})
		default:
			// A database or a connector: a grantee reads neither.
			read = errCodeNotFound
		}
		if read != nil {
			return AwaitingAdmin{}, unseen
		}
	}
	return sink.Propose(ctx, ProposedAction{Operation: input.Operation, Target: input.Target, Payload: payload,
		PayloadHash: confirmationPayloadHash(foundryOperationPrefix+input.Operation, input.Target, payload)})
}

// runNow is RunApproved and RunAnswers, under tool's name in the audit.
func (s *Service) runNow(ctx context.Context, identity *Identity, tool string, input FoundryOperationInput) Executed {
	var out FoundryOperationOutput
	var consumed bool
	var raw error
	_, _, _ = s.Run(ctx, tool, func(ctx context.Context, call *Call) (any, int, error) {
		prepared, _, err := s.PrepareFoundryOperation(ctx, identity, call, input)
		if err != nil {
			raw = err
			return nil, 0, err
		}
		var count int
		out, count, consumed, raw = s.executeFoundry(ctx, identity, call, ExecuteFoundryInput{
			Operation: input.Operation, Target: input.Target, Payload: input.Payload,
			ConfirmationID: prepared.ConfirmationID, ConfirmationToken: prepared.ConfirmationToken,
		})
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
