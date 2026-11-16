package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/automation"
	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// actionSink keeps what a turn's model prepares, for the turn's person: the
// confirmation's token sealed with the connectors' keyring, under the
// action's own id, never shown to the model, and the card Foundry draws of
// it (cards). An automation's turn runs at once, as its owner, what fits an
// action they approved for its step (approved, through sessions), and a
// person's turn what autoRun lets it run because their message asked for it;
// anything else waits for them as a card.
type actionSink struct {
	db         Store
	keys       *secretbox.Keyring
	turn       *model.FoundryAgentTurn
	workerID   string
	maxPending int
	// cards draws as the turn's person, set once their session opens.
	cards Cards
	// approved is what an automation's step may run unasked; sessions run it,
	// and what autoRun runs.
	approved []automation.Action
	sessions integration.SessionHost
}

var _ integration.ActionSink = (*actionSink)(nil)

// autoRun is what Founder runs without a card: a new dashboard (always unpinned and unpublished:
// CreateFoundryDashboardRequest has neither field) the person's own message asked for. The dock offers Undo.
// Everything else (pinning, publishing, loads, deletes, …) stays a card the person clicks.
var autoRun = map[string]func(turn *model.FoundryAgentTurn) bool{
	"create_dashboard": askedForDashboard,
}

// dashboardWord is the word dashboard or dashboards, whole, in any case.
var dashboardWord = regexp.MustCompile(`(?i)(?:^|[^\pL\pN_])dashboards?(?:$|[^\pL\pN_])`)

// askedForDashboard reports whether turn is a message the person wrote that
// asks for a dashboard: it holds the word dashboard or dashboards, whole, in
// any case. An automation's turn, an action's result and answers to
// questions never ask.
func askedForDashboard(turn *model.FoundryAgentTurn) bool {
	return turn.Kind == model.FoundryAgentTurnMessage && dashboardWord.MatchString(turn.Input)
}

// autoRuns reports whether a runs at once under autoRun: an operation it
// lists, acting in the turn's own workspace, in a turn its rule lets.
func (s *actionSink) autoRuns(a integration.PreparedAction) bool {
	asked, ok := autoRun[a.Operation]
	return ok && a.Target == "workspace:"+s.turn.WorkspaceID && asked(s.turn)
}

// Allows is Founder's operations, which hold every one an automation's
// owner may approve for a step (automation.Operations).
func (s *actionSink) Allows(operation string) bool { return Operations[operation] }

// Record checks the action acts in the turn's workspace, names what it acts
// on, and stores it with its card in one write. A payload the turn already
// prepared is the action already stored, unless its person already decided
// it, and is never drawn again: a replayed attempt finds the card the first
// drew, whatever changed since. Its refusals reach the model as their words.
//
// In an automation's turn, an action that fits one its owner approved for
// the step (automation.Fits: the same operation and target, its payload in
// bounds; a publish, of the revision already at the dashboard's link) is
// stored as executing, decided by them, and runs at once as them
// (RunAction): authorized again, its confirmation consumed once. A replay
// finds how it ran, and never runs it again.
//
// In a person's turn, an action autoRun lets (a new dashboard their message
// asked for) runs the same way, as them, and its card and result say so
// (auto): the dock offers Undo.
func (s *actionSink) Record(ctx context.Context, a integration.PreparedAction) (integration.Recorded, error) {
	if s.keys == nil {
		return nil, status.Error(codes.Unavailable, "Founder can't keep actions on this host")
	}
	approved := slices.ContainsFunc(s.approved, func(x automation.Action) bool {
		return automation.Fits(x, s.turn.WorkspaceID, a.Operation, a.Target, a.Payload)
	})
	if approved && a.Operation == (automation.PublishDashboard{}).Operation() {
		// It refreshes what a person published: only the revision already at
		// the link. An edit made since, which no one who may publish has seen,
		// waits for the owner as a card; the publish names the revision, so
		// one made after this check is refused.
		var payload struct {
			ExpectedRevision int64 `json:"expected_revision"`
		}
		_, dashboardID, _ := strings.Cut(a.Target, "/")
		d, err := s.db.GetFoundryDashboard(ctx, s.turn.WorkspaceID, dashboardID)
		approved = err == nil && json.Unmarshal(a.Payload, &payload) == nil && d.Share != nil && d.Share.SpecRevision == payload.ExpectedRevision &&
			d.Revision == payload.ExpectedRevision
	}
	auto := !approved && s.autoRuns(a)
	approved = approved || auto
	// actionID is the action this attempt stores; empty when one is stored already.
	var actionID string
	stored, err := s.db.GetFoundryAgentActionByPayload(ctx, s.turn.ID, a.PayloadHash)
	if errors.Is(err, foundrystore.ErrNotFound) {
		actionID = id.New()
		stored, err = s.record(ctx, a, actionID, approved, auto)
	}
	var refusal model.FoundryAgentRefusal
	switch {
	case errors.As(err, &refusal):
		return nil, status.Error(codes.FailedPrecondition, refusal.Detail)
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, ErrLost
	case err != nil:
		return nil, err
	case stored.Status == model.FoundryAgentActionPrepared:
		return integration.AwaitingPerson{ActionID: stored.ID, ExpiresAt: stored.ExpiresAt}, nil
	case !approved:
	case stored.ID == actionID && stored.Status == model.FoundryAgentActionExecuting:
		return s.runApproved(ctx, stored, auto)
	case stored.Status == model.FoundryAgentActionExecuting:
		// A replayed attempt finds the one that died running it.
		return integration.RanApproved{ActionID: stored.ID, Status: string(model.FoundryAgentActionUnknown),
			Error: "it was running when Founder stopped, so it may or may not have taken effect", Auto: auto}, nil
	case approvedEnd(stored.Status):
		ran := integration.RanApproved{ActionID: stored.ID, Status: string(stored.Status), Result: stored.Result, Auto: auto}
		if stored.Error != nil {
			ran.Error = *stored.Error
		}
		return ran, nil
	}
	// A replayed attempt prepared it again after its person decided.
	return nil, status.Error(codes.FailedPrecondition,
		"the person already decided this action: it is "+string(stored.Shown(time.Now()))+". Don't prepare it again unless they ask")
}

// approvedEnd reports how an approved action that ran may have ended.
func approvedEnd(s model.FoundryAgentActionStatus) bool {
	return s == model.FoundryAgentActionSucceeded || s == model.FoundryAgentActionFailed || s == model.FoundryAgentActionUnknown
}

// approvedRunMargin is how long before model.FoundryAgentExecutingFor an
// approved action's run is stopped, leaving it time to record how it ended.
const approvedRunMargin = 15 * time.Second

// runApproved runs the approved action stored as executing, records how it
// ended, and shows the person: whatever happens to the turn meanwhile, its
// end is recorded, so it never runs again. auto is one autoRun ran.
func (s *actionSink) runApproved(ctx context.Context, a *model.FoundryAgentAction, auto bool) (integration.Recorded, error) {
	ctx = context.WithoutCancel(ctx)
	runCtx, cancel := context.WithTimeout(ctx, model.FoundryAgentExecutingFor-approvedRunMargin)
	end := RunAction(runCtx, s.sessions, s.keys, a)
	cancel()
	if err := s.db.FinishFoundryAgentAction(ctx, a.ID, end, nil, nil); err != nil {
		return nil, fmt.Errorf("record the approved action's end: %w", err)
	}
	event, err := MarshalEvent(ActionResult{ActionID: a.ID, Operation: a.Operation, Status: string(end.Status), Result: end.Result, Error: end.Error,
		Auto: auto})
	if err != nil {
		return nil, err
	}
	if err := s.db.AppendFoundryAgentEvents(ctx, s.turn.ID, s.workerID, []json.RawMessage{event}); errors.Is(err, foundrystore.ErrNotFound) {
		return nil, ErrLost
	} else if err != nil {
		return nil, err
	}
	return integration.RanApproved{ActionID: a.ID, Status: string(end.Status), Result: end.Result, Error: end.Error, Auto: auto}, nil
}

// record draws a's card and stores it as actionID, with its card's event:
// executing, decided by the turn's person, when approved (auto when autoRun
// approved it). ErrNotFound means the turn's lease was lost.
func (s *actionSink) record(ctx context.Context, a integration.PreparedAction, actionID string, approved, auto bool) (*model.FoundryAgentAction, error) {
	card, err := s.cards.Draw(ctx, a)
	if err != nil {
		return nil, err
	}
	named, err := s.namedByPerson(ctx, card.target)
	if err != nil {
		return nil, err
	}
	sealed, keyID, err := s.keys.Seal([]byte(a.Confirmation.Token), actionID)
	if err != nil {
		return nil, fmt.Errorf("seal the action's confirmation: %w", err)
	}
	action := &model.FoundryAgentAction{
		ID: actionID, TurnID: s.turn.ID, ThreadID: s.turn.ThreadID, WorkspaceID: s.turn.WorkspaceID, UserID: s.turn.UserID,
		Operation: a.Operation, Target: a.Target, Payload: a.Payload, PayloadHash: a.PayloadHash, Display: card.Display,
		TargetNamedByUser: named, ConfirmationID: a.Confirmation.ID, TokenSealed: sealed, KeyID: keyID,
		Status: model.FoundryAgentActionPrepared, ExpiresAt: a.Confirmation.ExpiresAt,
	}
	if approved {
		action.Status, action.DecidedBy = model.FoundryAgentActionExecuting, &s.turn.UserID
	}
	return s.db.RecordFoundryAgentAction(ctx, s.workerID, action, s.maxPending, func(stored *model.FoundryAgentAction) (json.RawMessage, error) {
		return MarshalEvent(ActionCard{
			ActionID: stored.ID, Operation: stored.Operation, Target: stored.Target, Payload: stored.Payload, PayloadHash: stored.PayloadHash,
			Display: stored.Display, TargetNamedByUser: stored.TargetNamedByUser, ExpiresAt: stored.ExpiresAt, Auto: auto,
		})
	})
}

// target is what an action acts on, read from the turn's workspace: its id
// and its name, and the names of the workspace's other things of its kind,
// which a message naming one of them may hold its name within.
type target struct {
	id, name string
	others   []string
	// ontology is an ontology target's; connectorKind a connector's kind;
	// dashboard a dashboard's; automation an automation's.
	ontology      *model.FoundryOntology
	connectorKind string
	dashboard     *model.FoundryDashboard
	automation    *model.FoundryAutomation
	// runtime is where the Assistant runs, for set_assistant_runtime (whose
	// id is the runtime chosen) and revoke_assistant_helper (whose host is
	// the helper revoked).
	runtime *integration.GetAssistantRuntimeOutput
	host    integration.AssistantHostOutput
}

// target reads what a acts on, refusing one of another workspace (a card
// acts in its own) and an ontology the workspace doesn't hold, whose
// versions and loads a card would otherwise read. A connector the workspace
// doesn't hold keeps its id alone: its card reads nothing under it, and the
// action is checked again when it runs.
func (c Cards) target(ctx context.Context, a integration.PreparedAction) (target, error) {
	ws := c.WorkspaceID
	elsewhere := status.Error(codes.InvalidArgument, "the target is of another workspace: Founder acts in this one only")
	kind, rest, _ := strings.Cut(a.Target, ":")
	inWorkspace, thingID, _ := strings.Cut(rest, "/")
	ontologyNames := func(except string) ([]string, error) {
		all, _, err := c.DB.ListFoundryOntologies(ctx, ws, "", 100)
		var names []string
		for _, o := range all {
			if o.ID != except {
				names = append(names, o.Name)
			}
		}
		return names, err
	}
	switch kind {
	case "workspace":
		if rest != ws {
			return target{}, elsewhere
		}
		switch a.Operation {
		case "set_assistant_runtime", "revoke_assistant_helper":
			return c.runtimeTarget(ctx, a)
		case "create_dashboard":
			// A new dashboard: what a person names is its name.
			var p integration.CreateDashboardPayload
			if err := json.Unmarshal(a.Payload, &p); err != nil {
				return target{}, err
			}
			others, err := c.dashboardNames(ctx, "")
			return target{name: p.Name, others: others}, err
		case "create_automation":
			// A new automation: what a person names is its name.
			var p integration.CreateAutomationPayload
			if err := json.Unmarshal(a.Payload, &p); err != nil {
				return target{}, err
			}
			others, err := c.automationNames(ctx, "")
			return target{name: p.Name, others: others}, err
		}
		// A new ontology on a new database: what a person names is its name.
		var p integration.CreateOntologyPayload
		if err := json.Unmarshal(a.Payload, &p); err != nil {
			return target{}, err
		}
		others, err := ontologyNames("")
		return target{name: p.Name, others: others}, err
	case "tenant":
		if c.Databases == nil {
			return target{}, status.Error(codes.FailedPrecondition, "Founder can't tell whose a database is here: prepare create_ontology on a new database instead")
		}
		d, err := c.Databases.Get(ctx, rest)
		switch {
		case errors.Is(err, foundrystore.ErrNotFound) || err == nil && d.WorkspaceID != ws:
			return target{}, elsewhere
		case err != nil:
			return target{}, err
		}
		return target{id: rest, name: d.Name}, nil
	case "connector":
		if inWorkspace != ws {
			return target{}, elsewhere
		}
		all, err := c.DB.ListConnectors(ctx, ws)
		if err != nil {
			return target{}, err
		}
		t := target{id: thingID}
		for _, co := range all {
			if co.ID == thingID {
				t.name, t.connectorKind = co.Name, co.Kind
			} else {
				t.others = append(t.others, co.Name)
			}
		}
		return t, nil
	case "ontology":
		if inWorkspace != ws {
			return target{}, elsewhere
		}
		o, err := c.DB.GetFoundryOntology(ctx, ws, thingID)
		switch {
		case errors.Is(err, foundrystore.ErrNotFound) || err == nil && o.DeletedAt != nil:
			return target{}, status.Error(codes.FailedPrecondition, "this workspace holds no ontology "+thingID+": read its ontologies again")
		case err != nil:
			return target{}, err
		}
		others, err := ontologyNames(thingID)
		return target{id: thingID, name: o.Name, ontology: o, others: others}, err
	case "dashboard":
		if inWorkspace != ws {
			return target{}, elsewhere
		}
		d, err := c.DB.GetFoundryDashboard(ctx, ws, thingID)
		switch {
		case errors.Is(err, foundrystore.ErrNotFound):
			return target{}, status.Error(codes.FailedPrecondition, "this workspace holds no dashboard "+thingID+": read its dashboards again")
		case err != nil:
			return target{}, err
		}
		others, err := c.dashboardNames(ctx, thingID)
		return target{id: thingID, name: d.Name, dashboard: d, others: others}, err
	case "automation":
		if inWorkspace != ws {
			return target{}, elsewhere
		}
		au, err := c.DB.GetFoundryAutomation(ctx, ws, thingID)
		switch {
		case errors.Is(err, foundrystore.ErrNotFound):
			return target{}, status.Error(codes.FailedPrecondition, "this workspace holds no automation "+thingID+": read its automations again")
		case err != nil:
			return target{}, err
		}
		others, err := c.automationNames(ctx, thingID)
		return target{id: thingID, name: au.Name, automation: au, others: others}, err
	}
	return target{}, status.Error(codes.InvalidArgument, "Founder acts on no target of kind "+kind)
}

// dashboardNames are the names of the workspace's dashboards but except.
func (c Cards) dashboardNames(ctx context.Context, except string) ([]string, error) {
	all, err := c.DB.ListFoundryDashboards(ctx, c.WorkspaceID, false)
	var names []string
	for _, d := range all {
		if d.ID != except {
			names = append(names, d.Name)
		}
	}
	return names, err
}

// automationNames are the names of the workspace's automations but except.
func (c Cards) automationNames(ctx context.Context, except string) ([]string, error) {
	all, err := c.DB.ListFoundryAutomations(ctx, c.WorkspaceID)
	var names []string
	for _, au := range all {
		if au.ID != except {
			names = append(names, au.Name)
		}
	}
	return names, err
}

// runtimeTarget is what set_assistant_runtime and revoke_assistant_helper
// act on, read as the person: the runtime chosen, named as people name it,
// or the helper revoked, which must be paired.
func (c Cards) runtimeTarget(ctx context.Context, a integration.PreparedAction) (target, error) {
	if c.Runtime == nil {
		return target{}, status.Error(codes.FailedPrecondition, "Founder runs on a subscription only on a local install")
	}
	runtime, err := c.Runtime(ctx)
	if err != nil {
		return target{}, err
	}
	if a.Operation == "set_assistant_runtime" {
		var p integration.SetAssistantRuntimePayload
		if err := json.Unmarshal(a.Payload, &p); err != nil {
			return target{}, err
		}
		var others []string
		for r, label := range runtimeLabels {
			if r != p.Runtime {
				others = append(others, label)
			}
		}
		return target{id: p.Runtime, name: runtimeLabels[p.Runtime], others: others, runtime: runtime}, nil
	}
	var p integration.RevokeAssistantHelperPayload
	if err := json.Unmarshal(a.Payload, &p); err != nil {
		return target{}, err
	}
	t := target{id: p.HostID, runtime: runtime}
	found := false
	for _, h := range runtime.Hosts {
		if h.ID == p.HostID {
			t.name, t.host, found = h.Name, h, true
		} else {
			t.others = append(t.others, h.Name)
		}
	}
	if !found {
		return target{}, status.Error(codes.FailedPrecondition, "no helper "+p.HostID+" is paired: read the helpers again with helix_foundry_get_assistant_runtime")
	}
	return t, nil
}

// minNameRunes is the shortest name a message names by its words: a
// shorter one is in too many sentences by chance.
const minNameRunes = 3

// namedByPerson reports whether the person named the action's target
// themselves, in a message they sent or a page they were on in this thread:
// its id, or its whole name, in words of its own, when no other name of its
// kind holding it (Sales, in Sales graph) is there too. A card whose target
// only the model chose says so.
func (s *actionSink) namedByPerson(ctx context.Context, t target) (bool, error) {
	turns, err := s.db.ListFoundryAgentTurns(ctx, s.turn.ThreadID, 100)
	if err != nil {
		return false, err
	}
	var name *regexp.Regexp
	var longer []*regexp.Regexp
	if n := strings.TrimSpace(t.name); utf8.RuneCountInString(n) >= minNameRunes {
		name = wholeWords(n)
		for _, other := range t.others {
			if len(other) > len(n) && name.MatchString(other) {
				longer = append(longer, wholeWords(other))
			}
		}
	}
	for _, turn := range turns {
		if turn.Kind != model.FoundryAgentTurnMessage {
			continue
		}
		if t.id != "" && strings.Contains(turn.Input+" "+string(turn.PageContext), t.id) {
			return true, nil
		}
		if name != nil && name.MatchString(turn.Input) &&
			!slices.ContainsFunc(longer, func(l *regexp.Regexp) bool { return l.MatchString(turn.Input) }) {
			return true, nil
		}
	}
	return false, nil
}

// wholeWords matches phrase in a text, any case, as words of its own.
func wholeWords(phrase string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:^|[^\pL\pN_])` + regexp.QuoteMeta(strings.TrimSpace(phrase)) + `(?:$|[^\pL\pN_])`)
}
