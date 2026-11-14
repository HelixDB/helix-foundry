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

	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// actionSink keeps what a turn's model prepares, for the turn's person: the
// confirmation's token sealed with the connectors' keyring, under the
// action's own id, never shown to the model, and the card Foundry draws of
// it (cards).
type actionSink struct {
	db         Store
	keys       *secretbox.Keyring
	turn       *model.FoundryAgentTurn
	workerID   string
	maxPending int
	// cards draws as the turn's person, set once their session opens.
	cards Cards
}

var _ integration.ActionSink = (*actionSink)(nil)

func (s *actionSink) Allows(operation string) bool { return Operations[operation] }

// Record checks the action acts in the turn's workspace, names what it acts
// on, and stores it with its card in one write. A payload the turn already
// prepared is the action already stored, unless its person already decided
// it, and is never drawn again: a replayed attempt finds the card the first
// drew, whatever changed since. Its refusals reach the model as their words.
func (s *actionSink) Record(ctx context.Context, a integration.PreparedAction) (integration.AwaitingPerson, error) {
	if s.keys == nil {
		return integration.AwaitingPerson{}, status.Error(codes.Unavailable, "the Assistant can't keep actions on this host")
	}
	stored, err := s.db.GetFoundryAgentActionByPayload(ctx, s.turn.ID, a.PayloadHash)
	if errors.Is(err, foundrystore.ErrNotFound) {
		stored, err = s.record(ctx, a)
	}
	var refusal model.FoundryAgentRefusal
	switch {
	case errors.As(err, &refusal):
		return integration.AwaitingPerson{}, status.Error(codes.FailedPrecondition, refusal.Detail)
	case errors.Is(err, foundrystore.ErrNotFound):
		return integration.AwaitingPerson{}, ErrLost
	case err != nil:
		return integration.AwaitingPerson{}, err
	case stored.Status != model.FoundryAgentActionPrepared:
		// A replayed attempt prepared it again after its person decided.
		return integration.AwaitingPerson{}, status.Error(codes.FailedPrecondition,
			"the person already decided this action: it is "+string(stored.Shown(time.Now()))+". Don't prepare it again unless they ask")
	}
	return integration.AwaitingPerson{ActionID: stored.ID, ExpiresAt: stored.ExpiresAt}, nil
}

// record draws a's card and stores it, with its card's event. ErrNotFound
// means the turn's lease was lost.
func (s *actionSink) record(ctx context.Context, a integration.PreparedAction) (*model.FoundryAgentAction, error) {
	card, err := s.cards.Draw(ctx, a)
	if err != nil {
		return nil, err
	}
	named, err := s.namedByPerson(ctx, card.target)
	if err != nil {
		return nil, err
	}
	actionID := id.New()
	sealed, keyID, err := s.keys.Seal([]byte(a.Confirmation.Token), actionID)
	if err != nil {
		return nil, fmt.Errorf("seal the action's confirmation: %w", err)
	}
	return s.db.RecordFoundryAgentAction(ctx, s.workerID, &model.FoundryAgentAction{
		ID: actionID, TurnID: s.turn.ID, ThreadID: s.turn.ThreadID, WorkspaceID: s.turn.WorkspaceID, UserID: s.turn.UserID,
		Operation: a.Operation, Target: a.Target, Payload: a.Payload, PayloadHash: a.PayloadHash, Display: card.Display,
		TargetNamedByUser: named, ConfirmationID: a.Confirmation.ID, TokenSealed: sealed, KeyID: keyID,
		Status: model.FoundryAgentActionPrepared, ExpiresAt: a.Confirmation.ExpiresAt,
	}, s.maxPending, func(stored *model.FoundryAgentAction) (json.RawMessage, error) {
		return MarshalEvent(ActionCard{
			ActionID: stored.ID, Operation: stored.Operation, Target: stored.Target, Payload: stored.Payload, PayloadHash: stored.PayloadHash,
			Display: stored.Display, TargetNamedByUser: stored.TargetNamedByUser, ExpiresAt: stored.ExpiresAt,
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
	// dashboard a dashboard's.
	ontology      *model.FoundryOntology
	connectorKind string
	dashboard     *model.FoundryDashboard
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
	elsewhere := status.Error(codes.InvalidArgument, "the target is of another workspace: the Assistant acts in this one only")
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
			return target{}, status.Error(codes.FailedPrecondition, "the Assistant can't tell whose a database is here: prepare create_ontology on a new database instead")
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
	}
	return target{}, status.Error(codes.InvalidArgument, "the Assistant acts on no target of kind "+kind)
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

// runtimeTarget is what set_assistant_runtime and revoke_assistant_helper
// act on, read as the person: the runtime chosen, named as people name it,
// or the helper revoked, which must be paired.
func (c Cards) runtimeTarget(ctx context.Context, a integration.PreparedAction) (target, error) {
	if c.Runtime == nil {
		return target{}, status.Error(codes.FailedPrecondition, "the Assistant runs on a subscription only on a local install")
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
