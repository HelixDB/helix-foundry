package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/integration"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// ProposalStore is what reviewing and running a bot's proposals read and
// record of Foundry's store.
type ProposalStore interface {
	Reads
	GetFoundryAgentAction(ctx context.Context, workspaceID, id string) (*model.FoundryAgentAction, error)
	ClaimFoundryAgentProposal(ctx context.Context, workspaceID, id, payloadHash, approverID string) (*model.FoundryAgentAction, error)
	FinishFoundryAgentAction(ctx context.Context, id string, end model.FoundryAgentActionEnd, continuation *model.FoundryAgentTurn,
		firstEvent json.RawMessage) error
}

// Proposals reviews and runs a Slack install's bot's proposals (Proposed) as
// the owner or admin who approves them, in two clicks: Review draws the
// card and checks the action as them, never as the bot; Run draws and
// checks it again, claims it once for them, and runs it as them
// (integration.RunSlackApproved). The bot's thread then learns only whether
// it ran (FinishFoundryAgentAction); the approver, everything.
//
// Its caller (the Slack bot) checks, before each call, that approverID is
// the Helix user the clicking Slack user linked, and an owner or admin of
// the workspace now: Proposals trusts it.
type Proposals struct {
	DB       ProposalStore
	Sessions integration.SessionHost
	// Databases says whose a database is, for a create_ontology card on one;
	// nil refuses those.
	Databases ports.Databases
	// MaxMicros caps the turn the bot goes on from a run in: the
	// Assistant's cap on a turn (config.AssistantConfig.MaxMicros).
	MaxMicros int64
}

// ProposalReview is a proposal as its approver reviews it.
type ProposalReview struct {
	Action *model.FoundryAgentAction
	// Display is its card, drawn as the approver now (Cards.Draw).
	Display json.RawMessage
	// SpecHash is the SHA-256, in hex, of the workflow steps the card
	// shows: a create_workflow's, or an update_workflow's that carries
	// steps; empty for every other. Run takes it back, so a run approves
	// the steps reviewed.
	SpecHash string
}

// ProposalRun is how an approver's run of a proposal ended.
type ProposalRun struct {
	// Action is the proposal as the bot's thread keeps it: ended, decided
	// by the approver, its result only {operation, status, decided_by}.
	Action *model.FoundryAgentAction
	// End is how it ended in full: for the approver alone.
	End model.FoundryAgentActionEnd
	// Continuation is the turn the bot goes on from the run in; nil when
	// another turn of its thread went live first.
	Continuation *model.FoundryAgentTurn
}

// Review draws the proposal's card as approverID and checks the action as
// them now (ValidateFoundryOperation), changing nothing. A proposal that
// isn't the workspace's bot's is foundrystore.ErrNotFound; one no longer
// waiting is a model.FoundryAgentRefusal (FoundryAgentExpired, else
// FoundryAgentNotPrepared); what the approver may not do, or the card's
// checks refuse, is FAILED_PRECONDITION in the tool's words.
func (p Proposals) Review(ctx context.Context, workspaceID, actionID, approverID string) (ProposalReview, error) {
	reviewed, _, _, err := p.review(ctx, workspaceID, actionID, approverID)
	return reviewed, err
}

// Run runs the proposal as approverID, who reviewed it, once: Review's
// checks again, the workflow steps shown the ones reviewed (specHash, else
// FoundryAgentChanged), then ClaimFoundryAgentProposal for the payload
// reviewed (payloadHash: its refusals are returned, and a second approver
// is refused, the first having run it), and RunSlackApproved as them. A
// create_workflow or update_workflow that saved steps is then approved by
// them, at the revision their run wrote (the steps they reviewed). Its end
// is recorded, whatever happens to the caller, with the turn the bot goes on
// in, which reads only whether it ran.
func (p Proposals) Run(ctx context.Context, workspaceID, actionID, payloadHash, specHash, approverID string) (ProposalRun, error) {
	reviewed, session, input, err := p.review(ctx, workspaceID, actionID, approverID)
	if err != nil {
		return ProposalRun{}, err
	}
	if reviewed.SpecHash != specHash {
		return ProposalRun{}, model.FoundryAgentRefusal{Reason: model.FoundryAgentChanged, Detail: "It changed since you reviewed it; Review again"}
	}
	a, err := p.DB.ClaimFoundryAgentProposal(ctx, workspaceID, actionID, payloadHash, approverID)
	if err != nil {
		return ProposalRun{}, err
	}
	// From here its end is recorded, whatever happens to the caller, and
	// its run ends before a send would sweep it up as unknown.
	ctx = context.WithoutCancel(ctx)
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(session.Ctx), model.FoundryAgentExecutingFor-approvedRunMargin)
	end := actionEnd(session.Service.RunSlackApproved(runCtx, session.Identity, input))
	if end.Status == model.FoundryAgentActionSucceeded && reviewed.SpecHash != "" {
		var ran struct {
			Result struct {
				ID       string `json:"id"`
				Revision int64  `json:"revision"`
			} `json:"result"`
		}
		switch {
		case json.Unmarshal(end.Result, &ran) != nil || ran.Result.ID == "" || ran.Result.Revision < 1:
			end.Error = "it was saved, but its approval can't be read: approve it on its page in Foundry"
		case session.Founder == nil:
			end.Error = "it was saved, but not approved here: approve it on its page in Foundry"
		default:
			// The revision the run wrote holds the steps reviewed: an edit
			// since makes another revision, which the approval refuses.
			if _, err := session.Founder.ApproveFoundryAutomation(runCtx, &foundryv1.ApproveFoundryAutomationRequest{
				WorkspaceId: workspaceID, Id: ran.Result.ID, ExpectedRevision: ran.Result.Revision}); err != nil {
				end.Error = "it was saved, but not approved (" + status.Convert(err).Message() + "): approve it on its page in Foundry"
			}
		}
	}
	cancel()
	// What the bot's thread reads, as the store keeps it.
	told := ActionResult{ActionID: a.ID, Operation: a.Operation, Status: string(end.Status)}
	// Strings only: it can't fail.
	told.Result, _ = json.Marshal(map[string]string{"operation": a.Operation, "status": string(end.Status), "decided_by": approverID})
	switch end.Status {
	case model.FoundryAgentActionFailed:
		told.Error = model.FoundryAgentProposalFailed
	case model.FoundryAgentActionUnknown:
		told.Error = model.FoundryAgentProposalUnknown
	}
	first, err := MarshalEvent(told)
	if err != nil {
		return ProposalRun{}, err
	}
	// The store sets its principal and surface, the action's turn's, once
	// it is queued.
	continuation := &model.FoundryAgentTurn{ID: id.New(), WorkspaceID: workspaceID, ThreadID: a.ThreadID, Kind: model.FoundryAgentTurnActionResult,
		Input: a.ID, MaxUSDMicros: p.MaxMicros}
	if err := p.DB.FinishFoundryAgentAction(ctx, a.ID, end, continuation, first); err != nil {
		return ProposalRun{}, err
	}
	done, err := p.DB.GetFoundryAgentAction(ctx, workspaceID, a.ID)
	if err != nil {
		return ProposalRun{}, err
	}
	out := ProposalRun{Action: done, End: end}
	if continuation.Principal != nil {
		out.Continuation = continuation
	}
	return out, nil
}

// review is Review's: the proposal, the approver's session it was checked
// in, and its action as that session runs it.
func (p Proposals) review(ctx context.Context, workspaceID, actionID, approverID string) (
	ProposalReview, integration.Session, integration.FoundryOperationInput, error) {
	var none integration.FoundryOperationInput
	a, err := p.DB.GetFoundryAgentAction(ctx, workspaceID, actionID)
	if err != nil {
		return ProposalReview{}, integration.Session{}, none, err
	}
	if _, bot := a.Principal.(model.AgentBot); !bot {
		return ProposalReview{}, integration.Session{}, none, foundrystore.ErrNotFound
	}
	switch shown := a.Shown(time.Now()); shown {
	case model.FoundryAgentActionPrepared:
	case model.FoundryAgentActionExpired:
		return ProposalReview{}, integration.Session{}, none, model.FoundryAgentRefusal{Reason: model.FoundryAgentExpired,
			Detail: "the proposal expired: ask the bot again"}
	default:
		return ProposalReview{}, integration.Session{}, none, model.FoundryAgentRefusal{Reason: model.FoundryAgentNotPrepared,
			Detail: "the proposal is " + string(shown)}
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(a.Payload))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return ProposalReview{}, integration.Session{}, none, status.Error(codes.FailedPrecondition, "the proposal's payload can't be read")
	}
	session, err := p.Sessions.Session(ctx, ports.Actor{Source: ports.SourceUser, UserID: approverID, WorkspaceID: workspaceID},
		integration.ConfirmByCaller{})
	if err != nil {
		return ProposalReview{}, integration.Session{}, none, err
	}
	input := integration.FoundryOperationInput{Operation: a.Operation, Target: a.Target, Payload: payload}
	operation, err := session.Service.ValidateFoundryOperation(session.Ctx, session.Identity, input)
	if err != nil {
		return ProposalReview{}, integration.Session{}, none, status.Error(codes.FailedPrecondition, ToolErrorText(integration.ToolError(err)))
	}
	canonical := operation.CanonicalPayload()
	card, err := SessionCards(p.DB, p.Databases, session, workspaceID).Draw(session.Ctx, integration.PreparedAction{
		Operation: a.Operation, Target: a.Target, Payload: canonical, PayloadHash: a.PayloadHash})
	if err != nil {
		// Its refusals are the action's words, for the approver.
		return ProposalReview{}, integration.Session{}, none, err
	}
	out := ProposalReview{Action: a, Display: card.Display}
	if a.Operation == "create_workflow" || a.Operation == "update_workflow" {
		var steps struct {
			Spec json.RawMessage `json:"spec"`
		}
		// The canonical payload is its operation's payload as JSON: it reads.
		_ = json.Unmarshal(canonical, &steps)
		if len(steps.Spec) > 0 && string(steps.Spec) != "null" {
			sum := sha256.Sum256(steps.Spec)
			out.SpecHash = hex.EncodeToString(sum[:])
		}
	}
	return out, session, input, nil
}
