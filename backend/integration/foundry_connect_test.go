package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// formHost links forms only: it names its website but offers no signing in, as a local build
// and a hosted stamp with pipes.enabled off do. It offers Stripe, PostgreSQL without Pro, and
// with events an API source.
func formHost(events bool) (*Service, *signInBackend) {
	b := newSignInBackend()
	b.kinds = []*foundryv1.ConnectorKind{
		{Kind: "stripe", DisplayName: "Stripe", Available: true},
		{Kind: "postgres", DisplayName: "PostgreSQL", RequiresPro: true},
		{Kind: "events", DisplayName: "API source", Available: true},
	}
	return &Service{Foundry: b, FoundryURL: "https://foundry.example", Events: events}, b
}

func TestConnectLinksForms(t *testing.T) {
	t.Run("links a kind's form in the workspace", func(t *testing.T) {
		s, b := formHost(true)
		got, errText := call(t, connect(t, s, manager()), toolFoundryConnect, map[string]any{"workspace_id": "workspace-1", "kind": "stripe"})
		require.Empty(t, errText)
		assert.Equal(t, map[string]any{
			"url": "https://foundry.example/data?add=stripe&workspace=workspace-1", "kind": "stripe", "display_name": "Stripe", "available": true,
			"next": "Give the user this link to open in a browser signed in to Foundry: it opens the Stripe form, where they enter the credentials and save. " +
				"Then helix_foundry_list_connectors lists the new connector.",
		}, got)
		assert.Zero(t, b.count("StartConnectorSignIn"))
	})
	// A kind the plan doesn't include still links its form, which shows owners how to upgrade.
	t.Run("says how to upgrade for a kind the plan lacks", func(t *testing.T) {
		s, _ := formHost(true)
		got, errText := call(t, connect(t, s, manager()), toolFoundryConnect, map[string]any{"workspace_id": "workspace-1", "kind": "postgres"})
		require.Empty(t, errText)
		assert.Equal(t, false, got["available"])
		assert.True(t, strings.HasPrefix(got["next"].(string), "The workspace's plan doesn't include PostgreSQL; the link shows owners and admins how to upgrade to Foundry Pro first."), got["next"])
	})
	// The API source form asks for no credential, and an operation makes one without it.
	t.Run("links the API source form", func(t *testing.T) {
		s, _ := formHost(true)
		got, errText := call(t, connect(t, s, manager()), toolFoundryConnect, map[string]any{"workspace_id": "workspace-1", "kind": "events"})
		require.Empty(t, errText)
		assert.Equal(t, "https://foundry.example/data?add=events&workspace=workspace-1", got["url"])
		assert.Contains(t, got["next"], "it takes no credential, and the create_event_source operation makes one without the browser")
	})
	// A host without API sources neither names nor links one, though its catalog lists it.
	t.Run("has no API source on a host without them", func(t *testing.T) {
		s, _ := formHost(false)
		_, errText := call(t, connect(t, s, manager()), toolFoundryConnect, map[string]any{"workspace_id": "workspace-1", "kind": "events"})
		assert.Equal(t, `invalid_arguments: Foundry has no "events" connector; it offers stripe, postgres`, errText)
		raw, err := json.Marshal(listedTools(t, s, manager())[toolFoundryConnect])
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "events")
		assert.NotContains(t, string(raw), "API source")
	})
	t.Run("names the kinds there are for one there isn't", func(t *testing.T) {
		s, _ := formHost(true)
		_, errText := call(t, connect(t, s, manager()), toolFoundryConnect, map[string]any{"workspace_id": "workspace-1", "kind": "oracle"})
		assert.Equal(t, `invalid_arguments: Foundry has no "oracle" connector; it offers stripe, postgres, events`, errText)
	})
	// A caller who can't sign in sends the Workspace and the kind alone: the schema refuses the rest.
	t.Run("takes no sign-in's fields from a caller who can't sign in", func(t *testing.T) {
		s, _ := formHost(true)
		for _, field := range signInFields {
			_, errText := call(t, connect(t, s, manager()), toolFoundryConnect, map[string]any{"workspace_id": "workspace-1", "kind": "stripe", field: "x"})
			assert.Contains(t, errText, `unexpected additional properties ["`+field+`"]`, field)
		}
	})
	t.Run("refuses bad arguments before any call", func(t *testing.T) {
		s, b := formHost(true)
		for _, input := range []FoundryConnectInput{{WorkspaceID: "workspace-1"}, {WorkspaceID: "a b", Kind: "stripe"}} {
			_, _, err := s.FoundryConnect(context.Background(), manager(), &Call{}, input)
			assert.Equal(t, errCodeInvalidArguments, err)
		}
		assert.Empty(t, b.calls)
	})
	// Only a user who manages the workspace's connectors gets a link, to their own workspace.
	t.Run("refuses another workspace and a member before any call", func(t *testing.T) {
		s, b := formHost(true)
		var authorized []string
		identity := manager()
		authorize := identity.Authorize
		identity.Authorize = func(ctx context.Context, req any, grant Grant, methods ...string) error {
			authorized = methods
			return authorize(ctx, req, grant, methods...)
		}
		_, _, err := s.FoundryConnect(context.Background(), identity, &Call{}, FoundryConnectInput{WorkspaceID: "workspace-2", Kind: "stripe"})
		assert.Equal(t, errCodeNotFound, err)
		assert.Equal(t, []string{"ListConnectorKinds", "CreateConnector"}, authorized)
		_, _, err = s.FoundryConnect(context.Background(), member(), &Call{}, FoundryConnectInput{WorkspaceID: "workspace-1", Kind: "stripe"})
		assert.Equal(t, errCodeNotFound, err)
		assert.Empty(t, b.calls)
	})
	t.Run("passes the catalog's failure through", func(t *testing.T) {
		s, b := formHost(true)
		b.kindsErr = status.Error(codes.Unavailable, "backend down")
		_, _, err := s.FoundryConnect(context.Background(), manager(), &Call{}, FoundryConnectInput{WorkspaceID: "workspace-1", Kind: "stripe"})
		assert.Equal(t, codes.Unavailable, status.Code(err))
	})
}
