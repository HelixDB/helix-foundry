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
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// actionSink keeps what a turn's model prepares, for the turn's person: the
// confirmation's token sealed with the connectors' keyring, under the
// action's own id, never shown to the model, and the card Foundry draws of
// it (describe).
type actionSink struct {
	db         Store
	keys       *secretbox.Keyring
	turn       *model.FoundryAgentTurn
	workerID   string
	maxPending int
	// databases says whose a database is, for create_ontology on one; nil
	// on a host that can't, which refuses such an action.
	databases ports.Databases
	// reviews reads a review as the turn's person, for settle_review's
	// card; set once the person's session opens.
	reviews reviewReader
}

var _ integration.ActionSink = (*actionSink)(nil)

func (s *actionSink) Allows(operation string) bool { return Operations[operation] }

// Record checks the action acts in the turn's workspace, names what it acts
// on, and stores it with its card in one write. A payload the turn already
// prepared is the action already stored, unless its person already decided
// it. Its refusals reach the model as their words.
func (s *actionSink) Record(ctx context.Context, a integration.PreparedAction) (integration.AwaitingPerson, error) {
	if s.keys == nil {
		return integration.AwaitingPerson{}, status.Error(codes.Unavailable, "the Assistant can't keep actions on this host")
	}
	t, err := s.target(ctx, a)
	if err != nil {
		return integration.AwaitingPerson{}, err
	}
	display, err := s.describe(ctx, a, t)
	if err != nil {
		return integration.AwaitingPerson{}, err
	}
	rawDisplay, err := json.Marshal(display)
	if err != nil {
		return integration.AwaitingPerson{}, err
	}
	named, err := s.namedByPerson(ctx, t)
	if err != nil {
		return integration.AwaitingPerson{}, err
	}
	actionID := id.New()
	sealed, keyID, err := s.keys.Seal([]byte(a.Confirmation.Token), actionID)
	if err != nil {
		return integration.AwaitingPerson{}, fmt.Errorf("seal the action's confirmation: %w", err)
	}
	stored, err := s.db.RecordFoundryAgentAction(ctx, s.workerID, &model.FoundryAgentAction{
		ID: actionID, TurnID: s.turn.ID, ThreadID: s.turn.ThreadID, WorkspaceID: s.turn.WorkspaceID, UserID: s.turn.UserID,
		Operation: a.Operation, Target: a.Target, Payload: a.Payload, PayloadHash: a.PayloadHash, Display: rawDisplay,
		TargetNamedByUser: named, ConfirmationID: a.Confirmation.ID, TokenSealed: sealed, KeyID: keyID,
		Status: model.FoundryAgentActionPrepared, ExpiresAt: a.Confirmation.ExpiresAt,
	}, s.maxPending, func(stored *model.FoundryAgentAction) (json.RawMessage, error) {
		return MarshalEvent(ActionCard{
			ActionID: stored.ID, Operation: stored.Operation, Target: stored.Target, Payload: stored.Payload, PayloadHash: stored.PayloadHash,
			Display: stored.Display, TargetNamedByUser: stored.TargetNamedByUser, ExpiresAt: stored.ExpiresAt,
		})
	})
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

// target is what an action acts on, read from the turn's workspace: its id
// and its name, and the names of the workspace's other things of its kind,
// which a message naming one of them may hold its name within.
type target struct {
	id, name string
	others   []string
	// ontology is an ontology target's; connectorKind a connector's kind.
	ontology      *model.FoundryOntology
	connectorKind string
}

// target reads what a acts on, refusing one of another workspace: a turn
// acts in its own. A thing the workspace doesn't hold keeps its id alone;
// the action is checked again when it runs.
func (s *actionSink) target(ctx context.Context, a integration.PreparedAction) (target, error) {
	ws := s.turn.WorkspaceID
	elsewhere := status.Error(codes.InvalidArgument, "the target is of another workspace: the Assistant acts in this one only")
	kind, rest, _ := strings.Cut(a.Target, ":")
	inWorkspace, thingID, _ := strings.Cut(rest, "/")
	ontologyNames := func(except string) ([]string, error) {
		all, _, err := s.db.ListFoundryOntologies(ctx, ws, "", 100)
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
		// A new ontology on a new database: what a person names is its name.
		var p integration.CreateOntologyPayload
		if err := json.Unmarshal(a.Payload, &p); err != nil {
			return target{}, err
		}
		others, err := ontologyNames("")
		return target{name: p.Name, others: others}, err
	case "tenant":
		if s.databases == nil {
			return target{}, status.Error(codes.FailedPrecondition, "the Assistant can't tell whose a database is here: prepare create_ontology on a new database instead")
		}
		d, err := s.databases.Get(ctx, rest)
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
		all, err := s.db.ListConnectors(ctx, ws)
		if err != nil {
			return target{}, err
		}
		t := target{id: thingID}
		for _, c := range all {
			if c.ID == thingID {
				t.name, t.connectorKind = c.Name, c.Kind
			} else {
				t.others = append(t.others, c.Name)
			}
		}
		return t, nil
	case "ontology":
		if inWorkspace != ws {
			return target{}, elsewhere
		}
		t := target{id: thingID}
		switch o, err := s.db.GetFoundryOntology(ctx, ws, thingID); {
		case err == nil && o.DeletedAt == nil:
			t.name, t.ontology = o.Name, o
		case err != nil && !errors.Is(err, foundrystore.ErrNotFound):
			return target{}, err
		}
		others, err := ontologyNames(thingID)
		t.others = others
		return t, err
	}
	return target{}, status.Error(codes.InvalidArgument, "the Assistant acts on no target of kind "+kind)
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
