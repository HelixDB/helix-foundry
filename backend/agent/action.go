package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// RunAction runs an action that is executing, as its person now, through a
// session that confirms as the caller: validated and authorized again, its
// sealed confirmation opened (keys) and consumed once. It says how it ended:
// succeeded with its result, failed, or unknown when its answer was lost
// after the confirmation was spent, never to run again. Both a person's click
// (the API) and an automation's approved step (actionSink) run actions so.
func RunAction(ctx context.Context, sessions integration.SessionHost, keys *secretbox.Keyring, a *model.FoundryAgentAction) model.FoundryAgentActionEnd {
	failed := func(why string) model.FoundryAgentActionEnd {
		return model.FoundryAgentActionEnd{Status: model.FoundryAgentActionFailed, Error: why}
	}
	token, err := keys.Open(a.TokenSealed, a.KeyID, a.ID)
	if err != nil {
		return failed("the action's confirmation can't be opened; ask Founder to prepare it again")
	}
	var payload map[string]any
	decoder := json.NewDecoder(bytes.NewReader(a.Payload))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return failed("the action's payload can't be read")
	}
	session, err := sessions.Session(ctx, ports.Actor{Source: ports.SourceUser, UserID: a.UserID, WorkspaceID: a.WorkspaceID},
		integration.ConfirmByCaller{})
	if err != nil {
		return failed("your access to this workspace couldn't be read; nothing ran, so you can ask again")
	}
	switch done := session.Service.ExecuteForPerson(session.Ctx, session.Identity, integration.ExecuteFoundryInput{
		Operation: a.Operation, Target: a.Target, Payload: payload, ConfirmationID: a.ConfirmationID, ConfirmationToken: string(token),
	}).(type) {
	case integration.ExecutedOK:
		result, err := json.Marshal(done.Result)
		if err != nil {
			return model.FoundryAgentActionEnd{Status: model.FoundryAgentActionUnknown, Error: "it ran, and its result can't be shown"}
		}
		return model.FoundryAgentActionEnd{Status: model.FoundryAgentActionSucceeded, Result: result}
	case integration.ExecutedUnknown:
		return model.FoundryAgentActionEnd{Status: model.FoundryAgentActionUnknown,
			Error: "it may or may not have run: Foundry lost its answer (" + ToolErrorText(done.Err) + ")"}
	case integration.ExecutedFailed:
		return failed(ToolErrorText(done.Err))
	default:
		return failed("it ended in a way Foundry doesn't know")
	}
}

// ToolErrorText is a tool's error for the person: its message, else its code.
func ToolErrorText(err error) string {
	code, message, ok := integration.ToolErrorDetails(err)
	switch {
	case ok && message != "":
		return message
	case ok:
		return strings.ReplaceAll(code, "_", " ")
	default:
		return "internal error"
	}
}
