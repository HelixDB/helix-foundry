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
// action's own id, never shown to the model.
type actionSink struct {
	db         Store
	keys       *secretbox.Keyring
	turn       *model.FoundryAgentTurn
	workerID   string
	maxPending int
}

var _ integration.ActionSink = (*actionSink)(nil)

func (s *actionSink) Allows(operation string) bool { return Operations[operation] }

// Record seals the token, names what the action acts on, and stores the
// action with its card in one write. A payload the turn already prepared is
// the action already stored, unless its person already decided it. A target
// of another workspace is refused: a turn acts in its own. Its refusals
// reach the model as their words.
func (s *actionSink) Record(ctx context.Context, a integration.PreparedAction) (integration.AwaitingPerson, error) {
	if s.keys == nil {
		return integration.AwaitingPerson{}, status.Error(codes.Unavailable, "the Assistant can't keep actions on this host")
	}
	if _, rest, _ := strings.Cut(a.Target, ":"); strings.SplitN(rest, "/", 2)[0] != s.turn.WorkspaceID {
		return integration.AwaitingPerson{}, status.Error(codes.InvalidArgument, "the target is of another workspace: the Assistant acts in this one only")
	}
	actionID := id.New()
	sealed, keyID, err := s.keys.Seal([]byte(a.Confirmation.Token), actionID)
	if err != nil {
		return integration.AwaitingPerson{}, fmt.Errorf("seal the action's confirmation: %w", err)
	}
	display := describe(ctx, s.db, s.turn.WorkspaceID, a)
	rawDisplay, err := json.Marshal(display)
	if err != nil {
		return integration.AwaitingPerson{}, err
	}
	named, err := s.namedByPerson(ctx, a.Target, display)
	if err != nil {
		return integration.AwaitingPerson{}, err
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

// Display is what an action's card names, resolved by Foundry from the
// workspace: never the model's words.
type Display struct {
	OntologyID   string `json:"ontology_id,omitempty"`
	OntologyName string `json:"ontology_name,omitempty"`
	VersionID    string `json:"version_id,omitempty"`
	// Version is the version's number, as the site shows it (v3).
	Version int `json:"version,omitempty"`
}

// describe names what a prepared action acts on: its ontology and the
// version its payload names. What can't be read is left unnamed.
func describe(ctx context.Context, db Store, workspaceID string, a integration.PreparedAction) Display {
	var out Display
	_, ontologyID, ok := integration.CutFoundryTarget(a.Target, "ontology")
	if !ok {
		return out
	}
	o, err := db.GetFoundryOntology(ctx, workspaceID, ontologyID)
	if err != nil || o.DeletedAt != nil {
		return out
	}
	out.OntologyID, out.OntologyName = o.ID, o.Name
	var payload struct {
		VersionID string `json:"version_id"`
	}
	if json.Unmarshal(a.Payload, &payload) == nil && payload.VersionID != "" {
		out.VersionID = payload.VersionID
		if v, err := db.GetOntologyVersion(ctx, o.ID, payload.VersionID); err == nil {
			out.Version = v.Version
		}
	}
	return out
}

// minNameRunes is the shortest ontology name a message names by its words:
// a shorter one is in too many sentences by chance.
const minNameRunes = 3

// namedByPerson reports whether the person named the action's target
// themselves, in a message they sent or a page they were on in this thread:
// its id, or its ontology's whole name, in words of its own, when no other
// ontology's name holding it (Sales, in Sales graph) is there too. A card
// whose target only the model chose says so.
func (s *actionSink) namedByPerson(ctx context.Context, target string, display Display) (bool, error) {
	_, targetID, ok := strings.Cut(target, "/")
	if !ok {
		return false, nil
	}
	turns, err := s.db.ListFoundryAgentTurns(ctx, s.turn.ThreadID, 100)
	if err != nil {
		return false, err
	}
	var name *regexp.Regexp
	var longer []*regexp.Regexp
	if n := strings.TrimSpace(display.OntologyName); utf8.RuneCountInString(n) >= minNameRunes {
		name = wholeWords(n)
		others, _, err := s.db.ListFoundryOntologies(ctx, s.turn.WorkspaceID, "", 100)
		if err != nil {
			return false, err
		}
		for _, o := range others {
			if o.ID != display.OntologyID && len(o.Name) > len(n) && name.MatchString(o.Name) {
				longer = append(longer, wholeWords(o.Name))
			}
		}
	}
	for _, t := range turns {
		if t.Kind != model.FoundryAgentTurnMessage {
			continue
		}
		if strings.Contains(t.Input+" "+string(t.PageContext), targetID) {
			return true, nil
		}
		if name != nil && name.MatchString(t.Input) &&
			!slices.ContainsFunc(longer, func(l *regexp.Regexp) bool { return l.MatchString(t.Input) }) {
			return true, nil
		}
	}
	return false, nil
}

// wholeWords matches phrase in a text, any case, as words of its own.
func wholeWords(phrase string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(?:^|[^\pL\pN_])` + regexp.QuoteMeta(strings.TrimSpace(phrase)) + `(?:$|[^\pL\pN_])`)
}
