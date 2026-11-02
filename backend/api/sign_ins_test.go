package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/ports/portstest"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
)

// signInStore keeps sign-ins in memory beside connectorStore's connectors,
// with the MySQL store's guards, and the account purges they queue.
type signInStore struct {
	*connectorStore
	mu       sync.Mutex
	signIns  map[string]*foundrymodel.ConnectorSignIn
	purged   []string
	deleting bool
	// failSetAccount fails recording a finish's account.
	failSetAccount bool
}

func (s *signInStore) held(accountID string) bool {
	for _, c := range s.rows {
		if c.Account != nil && c.Account.ID == accountID {
			return true
		}
	}
	return false
}

func (s *signInStore) purge(accountID string) {
	if !s.held(accountID) {
		s.purged = append(s.purged, accountID)
	}
}

func (s *signInStore) StartConnectorSignIn(_ context.Context, start foundrymodel.ConnectorSignInStart) (*foundrymodel.ConnectorSignIn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	si, now := start.SignIn, time.Now()
	live := func(o *foundrymodel.ConnectorSignIn) bool {
		return o.Status != foundrymodel.ConnectorSignInConnected && o.ExpiresAt.After(now)
	}
	for _, o := range s.signIns {
		if si.ClientToken != nil && o.ClientToken != nil && *o.ClientToken == *si.ClientToken &&
			o.WorkspaceID == si.WorkspaceID && o.CreatedByUserID == si.CreatedByUserID {
			copied := *o
			return &copied, nil
		}
	}
	if si.ConnectorID != nil {
		if c, ok := s.rows[*si.ConnectorID]; !ok || c.WorkspaceID != si.WorkspaceID {
			return nil, foundrystore.ErrNotFound
		}
		for _, o := range s.signIns {
			if o.ConnectorID != nil && *o.ConnectorID == *si.ConnectorID && live(o) {
				if o.CreatedByUserID != si.CreatedByUserID {
					return nil, &foundrystore.SignInInProgressError{UserID: o.CreatedByUserID}
				}
				copied := *o
				return &copied, nil
			}
		}
	}
	liveCount, hour := 0, 0
	for _, o := range s.signIns {
		if o.CreatedByUserID != si.CreatedByUserID {
			continue
		}
		if o.WorkspaceID == si.WorkspaceID && live(o) {
			liveCount++
		}
		if o.CreatedAt.After(now.Add(-time.Hour)) {
			hour++
		}
	}
	if liveCount >= start.MaxLive || hour >= start.MaxPerHour {
		return nil, foundrystore.ErrLimitExceeded
	}
	stored := *si
	stored.Status, stored.CreatedAt, stored.UpdatedAt, stored.ExpiresAt = foundrymodel.ConnectorSignInPending, now, now, now.Add(start.TTL)
	s.signIns[si.ID] = &stored
	copied := stored
	return &copied, nil
}

func (s *signInStore) GetConnectorSignIn(ctx context.Context, workspaceID, id string) (*foundrymodel.ConnectorSignIn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	si, ok := s.signIns[id]
	if !ok || si.WorkspaceID != workspaceID {
		return nil, foundrystore.ErrNotFound
	}
	copied := *si
	return &copied, nil
}

func (s *signInStore) SetConnectorSignInVerifier(_ context.Context, _, id string, ciphertext []byte, keyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	si := s.signIns[id]
	if si.VerifierCiphertext != nil {
		return foundrystore.ErrNotFound
	}
	si.VerifierCiphertext, si.VerifierKeyID = ciphertext, &keyID
	return nil
}

func (s *signInStore) AuthorizeConnectorSignIn(_ context.Context, _, id string, ttl, stale time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	si, now := s.signIns[id], time.Now()
	switch {
	case !si.ExpiresAt.After(now):
		return foundrystore.ErrNotFound
	case si.Status == foundrymodel.ConnectorSignInFinishing && si.UpdatedAt.Before(now.Add(-stale)):
	case !slices.Contains([]foundrymodel.ConnectorSignInStatus{foundrymodel.ConnectorSignInPending, foundrymodel.ConnectorSignInAuthorizing,
		foundrymodel.ConnectorSignInDeclined, foundrymodel.ConnectorSignInFailed}, si.Status):
		return foundrystore.ErrNotFound
	}
	if si.AccountID != nil {
		s.purge(*si.AccountID)
	}
	si.Status, si.Failure, si.AccountID, si.ExpiresAt, si.UpdatedAt = foundrymodel.ConnectorSignInAuthorizing, nil, nil, now.Add(ttl), now
	return nil
}

func (s *signInStore) ClaimConnectorSignIn(_ context.Context, _, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	si := s.signIns[id]
	if si.Status != foundrymodel.ConnectorSignInAuthorizing || !si.ExpiresAt.After(time.Now()) {
		return foundrystore.ErrNotFound
	}
	si.Status, si.UpdatedAt = foundrymodel.ConnectorSignInFinishing, time.Now()
	return nil
}

func (s *signInStore) SetConnectorSignInAccount(ctx context.Context, _, id, accountID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.failSetAccount {
		return errors.New("connection reset")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	si := s.signIns[id]
	if si.Status != foundrymodel.ConnectorSignInFinishing {
		return foundrystore.ErrNotFound
	}
	si.AccountID = &accountID
	return nil
}

func (s *signInStore) FailConnectorSignIn(ctx context.Context, _, id string, end foundrymodel.ConnectorSignInEnd) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	si := s.signIns[id]
	if si.Status != foundrymodel.ConnectorSignInFinishing {
		return foundrystore.ErrNotFound
	}
	if si.AccountID != nil {
		s.purge(*si.AccountID)
	}
	si.Status, si.AccountID = end.Status, nil
	if end.Failure != "" {
		failure := end.Failure
		si.Failure = &failure
	}
	return nil
}

func (s *signInStore) ConnectConnectorSignIn(ctx context.Context, c foundrymodel.ConnectorSignInConnect) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	si := s.signIns[c.SignInID]
	switch {
	case s.deleting:
		return foundrystore.ErrWorkspaceDeleting
	case si.Status != foundrymodel.ConnectorSignInFinishing:
		return foundrystore.ErrNotFound
	}
	connectorID := c.ConnectorID
	if c.New != nil {
		account := *c.New.Account
		account.Generation = 1
		c.New.Account = &account
		if err := s.CreateConnector(ctx, c.New); err != nil {
			return err
		}
		connectorID = c.New.ID
	} else {
		existing, ok := s.rows[c.ConnectorID]
		if !ok {
			return foundrystore.ErrNotFound
		}
		old := existing.Account.ID
		now := time.Now()
		existing.Account = &foundrymodel.ConnectorAccount{ID: c.Account.ID, UserID: c.Account.UserID, Label: c.Account.Label,
			Generation: existing.Account.Generation + 1, ConnectedAt: &now}
		existing.NextSyncAt = &now
		if old != c.Account.ID {
			s.purge(old)
		}
	}
	si.Status, si.ConnectorID, si.IdentityChanged = foundrymodel.ConnectorSignInConnected, &connectorID, c.IdentityChanged
	if c.DiscoveryProblem != "" {
		si.DiscoveryProblem = &c.DiscoveryProblem
	}
	return nil
}

func (s *signInStore) MarkConnectorAccountChecked(context.Context, string, string) error { return nil }

func (s *signInStore) ListConnectors(_ context.Context, workspaceID string) ([]*foundrymodel.Connector, error) {
	var out []*foundrymodel.Connector
	for _, c := range s.rows {
		if c.WorkspaceID == workspaceID {
			copied := *c
			out = append(out, &copied)
		}
	}
	slices.SortFunc(out, func(a, b *foundrymodel.Connector) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// providers is GitHub and Linear for signed-in tokens: who signs in, the
// repositories they read, and the tokens they reject.
type providers struct {
	mu       sync.Mutex
	who      string
	repos    []string
	rejected map[string]bool
	// hang holds every request until its caller gives up.
	hang bool
}

func newProviders(t *testing.T) (*providers, string) {
	t.Helper()
	p := &providers{who: "Ada Lovelace", rejected: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		hang := p.hang
		p.mu.Unlock()
		if hang {
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer token-") || p.rejected[strings.TrimPrefix(auth, "Bearer ")] {
			if r.URL.Path == "/graphql" {
				_, _ = w.Write([]byte(`{"errors":[{"message":"Authentication required","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		who := p.who
		switch path := r.URL.Path; {
		case path == "/graphql":
			fmt.Fprintf(w, `{"data":{"viewer":{"id":"u1","name":%q,"organization":{"name":"Acme"}}}}`, who)
		case path == "/user":
			fmt.Fprintf(w, `{"login":%q}`, strings.ToLower(strings.Fields(who)[0]))
		case strings.HasPrefix(path, "/repos/"):
			name := strings.ToLower(strings.Join(strings.Split(strings.TrimPrefix(path, "/repos/"), "/")[:2], "/"))
			switch i := slices.Index(p.repos, name); {
			case i < 0:
				w.WriteHeader(http.StatusNotFound)
			case strings.Count(path, "/") == 3:
				fmt.Fprintf(w, `{"id":%d,"full_name":%q}`, i+1, name)
			default:
				_, _ = w.Write([]byte(`[]`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return p, server.URL
}

// people names user-1 Ada Lovelace and user-2 Grace Hopper.
type people struct{}

func (people) MayManageAI(context.Context, string, string) bool { return false }
func (people) DisplayName(_ context.Context, userID string) (string, error) {
	switch userID {
	case "user-1":
		return "Ada Lovelace", nil
	case "user-2":
		return "Grace Hopper", nil
	}
	return "", foundrystore.ErrNotFound
}

const callback = "https://foundry.test/connect/callback"

type signInFixture struct {
	s         *Service
	store     *signInStore
	broker    *portstest.Accounts
	providers *providers
	keyring   *secretbox.Keyring
}

func newSignInFixture(t *testing.T) signInFixture {
	t.Helper()
	keyring, err := secretbox.NewKeyring("k1", map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	p, url := newProviders(t)
	endpoints := connector.DefaultEndpoints()
	endpoints.GitHub, endpoints.Linear = url, url
	cfg := &config.Config{}
	cfg.Foundry.Connectors.SignInReturnTo = []string{callback}
	cfg.Foundry.Connectors.GitHubAppInstallURL = "https://github.com/apps/helix-foundry/installations/new"
	store := &signInStore{connectorStore: &connectorStore{rows: map[string]*foundrymodel.Connector{}},
		signIns: map[string]*foundrymodel.ConnectorSignIn{}}
	broker := &portstest.Accounts{Offered: []string{"github", "linear"}}
	return signInFixture{
		s: &Service{deps: &runtimeDeps{Cfg: cfg, DB: store, ConnectorKeys: keyring, Accounts: broker, People: people{},
			ConnectorCatalog: connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{}, connector.EventsOptions{})}},
		store: store, broker: broker, providers: p, keyring: keyring,
	}
}

func as(userID string) context.Context {
	return ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceUser, UserID: userID})
}

func (f signInFixture) start(t *testing.T, ctx context.Context, req *foundryv1.StartConnectorSignInRequest) *foundryv1.ConnectorSignIn {
	t.Helper()
	req.WorkspaceId = "ws-1"
	resp, err := f.s.StartConnectorSignIn(ctx, req)
	if err != nil {
		t.Fatalf("StartConnectorSignIn = %v", err)
	}
	return resp.GetSignIn()
}

func (f signInFixture) authorize(t *testing.T, ctx context.Context, id string) string {
	t.Helper()
	resp, err := f.s.AuthorizeConnectorSignIn(ctx, &foundryv1.AuthorizeConnectorSignInRequest{WorkspaceId: "ws-1", Id: id, ReturnTo: callback})
	if err != nil {
		t.Fatalf("AuthorizeConnectorSignIn = %v", err)
	}
	return resp.GetUrl()
}

func (f signInFixture) finish(t *testing.T, ctx context.Context, id, code, callbackError string) *foundryv1.ConnectorSignIn {
	t.Helper()
	resp, err := f.s.FinishConnectorSignIn(ctx, &foundryv1.FinishConnectorSignInRequest{WorkspaceId: "ws-1", Id: id, ConnectCode: code,
		Error: callbackError})
	if err != nil {
		t.Fatalf("FinishConnectorSignIn = %v", err)
	}
	return resp.GetSignIn()
}

// connect signs a new connector of kind in as userID, start to finish.
func (f signInFixture) connect(t *testing.T, userID string, req *foundryv1.StartConnectorSignInRequest) *foundryv1.ConnectorSignIn {
	t.Helper()
	ctx := as(userID)
	si := f.start(t, ctx, req)
	f.authorize(t, ctx, si.GetId())
	done := f.finish(t, ctx, si.GetId(), f.broker.Code(len(f.broker.Authorizations())-1), "")
	if done.GetStatus() != foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_CONNECTED {
		t.Fatalf("sign-in = %v, want connected", done)
	}
	return done
}

func reasonOf(err error) string { return foundryReason(err) }

func linearSignIn() *foundryv1.StartConnectorSignInRequest {
	return &foundryv1.StartConnectorSignInRequest{Kind: "linear", Name: "Acme Linear"}
}

func githubSignIn(repos ...string) *foundryv1.StartConnectorSignInRequest {
	return &foundryv1.StartConnectorSignInRequest{Kind: "github", Config: &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Github{
		Github: &foundryv1.GitHubConnectorConfig{Repos: repos}}}}
}

func TestConnectorSignIns(t *testing.T) {
	statusOf := func(si *foundryv1.ConnectorSignIn) foundryv1.ConnectorSignInStatus { return si.GetStatus() }

	t.Run("starts, authorizes and finishes a sign-in into a connector", func(t *testing.T) {
		f := newSignInFixture(t)
		ctx := as("user-1")
		si := f.start(t, ctx, linearSignIn())
		if statusOf(si) != foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_PENDING || !si.GetMine() || si.GetName() != "Acme Linear" ||
			si.GetConfig().GetLinear() == nil {
			t.Fatalf("started = %v", si)
		}
		url := f.authorize(t, ctx, si.GetId())
		if !strings.HasPrefix(url, "https://provider.test/authorize") {
			t.Fatalf("url = %s", url)
		}
		auth := f.broker.Authorizations()[0]
		if auth.ReturnTo != callback || auth.Label != "Acme Linear" || auth.UserID != "user-1" || auth.AccountID != "" || auth.Challenge == "" {
			t.Fatalf("authorization = %+v", auth)
		}
		done := f.finish(t, ctx, si.GetId(), f.broker.Code(0), "")
		if statusOf(done) != foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_CONNECTED || done.GetConnectorId() == "" ||
			done.GetDiscoveryProblem() != "" {
			t.Fatalf("finished = %v", done)
		}

		c := f.store.rows[done.GetConnectorId()]
		if c.Account == nil || c.Account.UserID != "user-1" || c.Account.Label != "Ada Lovelace in Acme" || c.NextSyncAt == nil ||
			c.SecretHint != "" || c.Name != "Acme Linear" {
			t.Fatalf("connector = %+v, account %+v", c, c.Account)
		}
		if secret, err := f.keyring.Open(c.SecretCiphertext, c.SecretKeyID, connector.SecretContext(c.ID)); err != nil || len(secret) != 0 {
			t.Fatalf("sealed = %q, %v; want nothing", secret, err)
		}
		got, err := f.s.GetConnector(ctx, &foundryv1.GetConnectorRequest{WorkspaceId: "ws-1", Id: c.ID})
		if err != nil {
			t.Fatal(err)
		}
		account := got.GetConnector().GetAccount()
		if account.GetState() != foundryv1.ConnectorAccountState_CONNECTOR_ACCOUNT_STATE_CONNECTED || account.GetConnectedBy() != "Ada Lovelace" ||
			account.GetLabel() != "Ada Lovelace in Acme" || account.GetUserId() != "user-1" || got.GetConnector().GetSecretHint() != "" {
			t.Fatalf("account = %v", account)
		}
	})

	// A decline, or a failure, keeps the sign-in: authorizing again retries
	// it, with the same verifier, and the agent's wait follows it.
	t.Run("authorizes again after a decline or a failure", func(t *testing.T) {
		f := newSignInFixture(t)
		ctx := as("user-1")
		si := f.start(t, ctx, linearSignIn())
		f.authorize(t, ctx, si.GetId())
		if got := f.finish(t, ctx, si.GetId(), "", "access_denied"); statusOf(got) != foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_DECLINED {
			t.Fatalf("declined = %v", got)
		}
		f.authorize(t, ctx, si.GetId())
		got := f.finish(t, ctx, si.GetId(), "code-nobody-gave", "")
		if statusOf(got) != foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_FAILED ||
			got.GetFailure() != foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_CODE_EXPIRED {
			t.Fatalf("failed = %v", got)
		}
		f.authorize(t, ctx, si.GetId())
		auths := f.broker.Authorizations()
		if len(auths) != 3 || auths[0].Challenge != auths[2].Challenge {
			t.Fatalf("authorizations = %+v, want one verifier for all", auths)
		}
		if got := f.finish(t, ctx, si.GetId(), f.broker.Code(2), ""); statusOf(got) != foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_CONNECTED {
			t.Fatalf("finished = %v", got)
		}
		if _, err := f.s.AuthorizeConnectorSignIn(ctx, &foundryv1.AuthorizeConnectorSignInRequest{WorkspaceId: "ws-1", Id: si.GetId(),
			ReturnTo: callback}); reasonOf(err) != "SIGN_IN_ENDED" {
			t.Fatalf("Authorize after connecting = %v, want SIGN_IN_ENDED", err)
		}
	})

	// A callback without a code ends the sign-in without asking the broker.
	t.Run("fails a callback without a code", func(t *testing.T) {
		f := newSignInFixture(t)
		ctx := as("user-1")
		si := f.start(t, ctx, linearSignIn())
		f.authorize(t, ctx, si.GetId())
		got := f.finish(t, ctx, si.GetId(), "", "server_error")
		if got.GetFailure() != foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_PROVIDER_ERROR || f.broker.Calls("Finish") != 0 {
			t.Fatalf("finished = %v, broker finishes %d", got, f.broker.Calls("Finish"))
		}
	})

	t.Run("expires, and authorizing moves the expiry", func(t *testing.T) {
		f := newSignInFixture(t)
		ctx := as("user-1")
		si := f.start(t, ctx, linearSignIn())
		f.store.signIns[si.GetId()].ExpiresAt = time.Now().Add(time.Minute)
		f.authorize(t, ctx, si.GetId())
		if expires := f.store.signIns[si.GetId()].ExpiresAt; time.Until(expires) < 14*time.Minute {
			t.Fatalf("expires in %s, want 15 minutes", time.Until(expires))
		}
		f.store.signIns[si.GetId()].ExpiresAt = time.Now().Add(-time.Second)
		got, err := f.s.GetConnectorSignIn(ctx, &foundryv1.GetConnectorSignInRequest{WorkspaceId: "ws-1", Id: si.GetId()})
		if err != nil || got.GetSignIn().GetStatus() != foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_EXPIRED {
			t.Fatalf("Get = %v, %v; want expired", got, err)
		}
		if _, err := f.s.AuthorizeConnectorSignIn(ctx, &foundryv1.AuthorizeConnectorSignInRequest{WorkspaceId: "ws-1", Id: si.GetId(),
			ReturnTo: callback}); reasonOf(err) != "SIGN_IN_ENDED" {
			t.Fatalf("Authorize = %v, want SIGN_IN_ENDED", err)
		}
		if got := f.finish(t, ctx, si.GetId(), f.broker.Code(0), ""); statusOf(got) != foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_EXPIRED ||
			f.broker.Calls("Finish") != 0 {
			t.Fatalf("Finish = %v", got)
		}
	})

	// The sign-in id is no capability: only its starter, in their browser,
	// authorizes and finishes it.
	t.Run("refuses someone else's sign-in", func(t *testing.T) {
		f := newSignInFixture(t)
		si := f.start(t, as("user-1"), linearSignIn())
		f.authorize(t, as("user-1"), si.GetId())
		if _, err := f.s.AuthorizeConnectorSignIn(as("user-2"), &foundryv1.AuthorizeConnectorSignInRequest{WorkspaceId: "ws-1", Id: si.GetId(),
			ReturnTo: callback}); reasonOf(err) != "SIGN_IN_NOT_YOURS" {
			t.Fatalf("Authorize by another = %v", err)
		}
		if _, err := f.s.FinishConnectorSignIn(as("user-2"), &foundryv1.FinishConnectorSignInRequest{WorkspaceId: "ws-1", Id: si.GetId(),
			ConnectCode: f.broker.Code(0)}); reasonOf(err) != "SIGN_IN_NOT_YOURS" {
			t.Fatalf("Finish by another = %v", err)
		}
		agent := ports.WithActor(context.Background(), ports.Actor{Source: ports.SourceMCP, UserID: "user-1"})
		if _, err := f.s.AuthorizeConnectorSignIn(agent, &foundryv1.AuthorizeConnectorSignInRequest{WorkspaceId: "ws-1", Id: si.GetId(),
			ReturnTo: callback}); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("Authorize by an agent = %v", err)
		}
		got, err := f.s.GetConnectorSignIn(as("user-2"), &foundryv1.GetConnectorSignInRequest{WorkspaceId: "ws-1", Id: si.GetId()})
		if err != nil || got.GetSignIn().GetMine() {
			t.Fatalf("Get by another = %v, %v; want it, not theirs", got, err)
		}
		if _, err := f.s.AuthorizeConnectorSignIn(as("user-1"), &foundryv1.AuthorizeConnectorSignInRequest{WorkspaceId: "ws-1", Id: si.GetId(),
			ReturnTo: "https://evil.test/callback"}); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("Authorize to another callback = %v", err)
		}
	})

	// A refreshed callback, a double effect or a retry spends no second code.
	t.Run("finishes once, whatever comes during and after", func(t *testing.T) {
		f := newSignInFixture(t)
		ctx := as("user-1")
		si := f.start(t, ctx, linearSignIn())
		f.authorize(t, ctx, si.GetId())
		var during *foundryv1.ConnectorSignIn
		f.broker.OnFinish = func() { during = f.finish(t, ctx, si.GetId(), f.broker.Code(0), "") }
		done := f.finish(t, ctx, si.GetId(), f.broker.Code(0), "")
		after := f.finish(t, ctx, si.GetId(), f.broker.Code(0), "")
		if statusOf(during) != foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_FINISHING ||
			statusOf(done) != foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_CONNECTED ||
			statusOf(after) != foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_CONNECTED {
			t.Fatalf("during = %v, done = %v, after = %v", during, done, after)
		}
		if f.broker.Calls("Finish") != 1 || len(f.store.rows) != 1 {
			t.Fatalf("broker finishes = %d, connectors = %d", f.broker.Calls("Finish"), len(f.store.rows))
		}
	})

	// Whatever fails after the broker saved the account lets it go.
	t.Run("purges the account of a sign-in that fails after the broker saved it", func(t *testing.T) {
		f := newSignInFixture(t)
		ctx := as("user-1")
		si := f.start(t, ctx, linearSignIn())
		f.authorize(t, ctx, si.GetId())
		f.providers.rejected["token-acct-2-1"] = true
		got := f.finish(t, ctx, si.GetId(), f.broker.Code(0), "")
		if got.GetFailure() != foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_TOKEN_REJECTED ||
			!slices.Equal(f.store.purged, []string{"acct-2"}) || len(f.store.rows) != 0 {
			t.Fatalf("finished = %v, purged %v, connectors %d", got, f.store.purged, len(f.store.rows))
		}
	})

	t.Run("refuses a workspace being deleted, and purges the account", func(t *testing.T) {
		f := newSignInFixture(t)
		ctx := as("user-1")
		si := f.start(t, ctx, linearSignIn())
		f.authorize(t, ctx, si.GetId())
		f.store.deleting = true
		got := f.finish(t, ctx, si.GetId(), f.broker.Code(0), "")
		if got.GetFailure() != foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_UNAVAILABLE || len(f.store.purged) != 1 {
			t.Fatalf("finished = %v, purged %v", got, f.store.purged)
		}
	})

	t.Run("fails an account that isn't the workspace's", func(t *testing.T) {
		f := newSignInFixture(t)
		ctx := as("user-1")
		si := f.start(t, ctx, linearSignIn())
		f.authorize(t, ctx, si.GetId())
		f.broker.Foreign = true
		if got := f.finish(t, ctx, si.GetId(), f.broker.Code(0), ""); got.GetFailure() !=
			foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_NOT_THIS_WORKSPACE {
			t.Fatalf("finished = %v", got)
		}
	})

	// Signing in again keeps the connector: a generation more, no longer
	// broken, reading with whoever signed it in.
	// Only the account's own member reauthorizes it, and Finish is told so,
	// so the broker never deletes it; anyone else makes a new account, which
	// replaces it, and the old one is purged.
	t.Run("signs a connector in again, by the same member and another", func(t *testing.T) {
		f := newSignInFixture(t)
		first := f.connect(t, "user-1", linearSignIn())
		c := f.store.rows[first.GetConnectorId()]
		broken := ports.AccountAccessRevoked
		c.Account.Broken = &broken
		account := c.Account.ID

		again := &foundryv1.StartConnectorSignInRequest{Kind: "linear", ConnectorId: c.ID}
		si := f.start(t, as("user-1"), again)
		if si.GetName() != "Acme Linear" || si.GetConnectorId() != c.ID {
			t.Fatalf("started = %v", si)
		}
		f.authorize(t, as("user-1"), si.GetId())
		if auth := f.broker.Authorizations()[1]; auth.AccountID != account {
			t.Fatalf("authorization = %+v, want a reauthorization of %s", auth, account)
		}
		done := f.finish(t, as("user-1"), si.GetId(), f.broker.Code(1), "")
		c = f.store.rows[c.ID]
		if done.GetIdentityChanged() || c.Account.Generation != 2 || c.Account.Broken != nil || c.Account.ID != account ||
			len(f.store.purged) != 0 {
			t.Fatalf("done = %v, account = %+v, purged %v", done, c.Account, f.store.purged)
		}
		if ref := f.broker.Finishes()[1]; ref.AccountID != account || ref.UserID != "user-1" {
			t.Fatalf("finish ref = %+v, want the reauthorized account named", ref)
		}

		f.providers.who = "Grace Hopper"
		si = f.start(t, as("user-2"), again)
		f.authorize(t, as("user-2"), si.GetId())
		if auth := f.broker.Authorizations()[2]; auth.AccountID != "" {
			t.Fatalf("authorization = %+v: another member's account can't be reauthorized", auth)
		}
		done = f.finish(t, as("user-2"), si.GetId(), f.broker.Code(2), "")
		c = f.store.rows[c.ID]
		if !done.GetIdentityChanged() || c.Account.UserID != "user-2" || c.Account.Label != "Grace Hopper in Acme" || c.Account.Generation != 3 ||
			c.Account.ID == account || !slices.Equal(f.store.purged, []string{account}) || f.broker.Finishes()[2].AccountID != "" {
			t.Fatalf("done = %v, account = %+v, purged %v", done, c.Account, f.store.purged)
		}
	})

	// A member whose account's member left makes a new account too, even the
	// one who left.
	t.Run("makes a new account where its member left", func(t *testing.T) {
		f := newSignInFixture(t)
		first := f.connect(t, "user-1", linearSignIn())
		c := f.store.rows[first.GetConnectorId()]
		broken := ports.AccountMemberLeft
		c.Account.Broken = &broken
		si := f.start(t, as("user-1"), &foundryv1.StartConnectorSignInRequest{Kind: "linear", ConnectorId: c.ID})
		f.authorize(t, as("user-1"), si.GetId())
		if auth := f.broker.Authorizations()[1]; auth.AccountID != "" {
			t.Fatalf("authorization = %+v, want a new account", auth)
		}
	})

	// The broker saved an account the sign-in can't record: it goes at
	// once, unless it is the account the connector reads with, which
	// reauthorizing gave back.
	t.Run("lets an unrecorded account go, never the connector's own", func(t *testing.T) {
		f := newSignInFixture(t)
		first := f.connect(t, "user-1", linearSignIn())
		account := f.store.rows[first.GetConnectorId()].Account.ID
		f.store.failSetAccount = true
		ctx := as("user-1")
		si := f.start(t, ctx, &foundryv1.StartConnectorSignInRequest{Kind: "linear", ConnectorId: first.GetConnectorId()})
		f.authorize(t, ctx, si.GetId())
		_, err := f.s.FinishConnectorSignIn(ctx, &foundryv1.FinishConnectorSignInRequest{WorkspaceId: "ws-1", Id: si.GetId(),
			ConnectCode: f.broker.Code(1)})
		if status.Code(err) != codes.Internal || !f.broker.Has(account) {
			t.Fatalf("Finish = %v; broker still has the connector's account: %v", err, f.broker.Has(account))
		}

		fresh := f.start(t, ctx, linearSignIn())
		f.authorize(t, ctx, fresh.GetId())
		if _, err := f.s.FinishConnectorSignIn(ctx, &foundryv1.FinishConnectorSignInRequest{WorkspaceId: "ws-1", Id: fresh.GetId(),
			ConnectCode: f.broker.Code(2)}); status.Code(err) != codes.Internal {
			t.Fatalf("Finish = %v", err)
		}
		if made := f.broker.Finishes()[2]; made.AccountID != "" || f.broker.Calls("Delete") != 1 {
			t.Fatalf("finish ref = %+v, deletes %d: the new account goes", made, f.broker.Calls("Delete"))
		}
	})

	// A finish that spends its time on the provider still records its end,
	// and answers with it.
	t.Run("records the end of a finish that ran out of time", func(t *testing.T) {
		saved := signInFinishTimeout
		signInFinishTimeout = 200 * time.Millisecond
		t.Cleanup(func() { signInFinishTimeout = saved })
		f := newSignInFixture(t)
		ctx := as("user-1")
		si := f.start(t, ctx, linearSignIn())
		f.authorize(t, ctx, si.GetId())
		f.providers.hang = true
		got := f.finish(t, ctx, si.GetId(), f.broker.Code(0), "")
		if statusOf(got) != foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_FAILED || len(f.store.purged) != 1 {
			t.Fatalf("finished = %v, purged %v", got, f.store.purged)
		}
	})

	t.Run("refuses a second member while one signs the connector in", func(t *testing.T) {
		f := newSignInFixture(t)
		first := f.connect(t, "user-1", linearSignIn())
		again := &foundryv1.StartConnectorSignInRequest{Kind: "linear", ConnectorId: first.GetConnectorId()}
		f.start(t, as("user-1"), again)
		_, err := f.s.StartConnectorSignIn(as("user-2"), &foundryv1.StartConnectorSignInRequest{WorkspaceId: "ws-1", Kind: "linear",
			ConnectorId: first.GetConnectorId()})
		if reasonOf(err) != "SIGN_IN_IN_PROGRESS" || !strings.Contains(status.Convert(err).Message(), "Ada Lovelace") {
			t.Fatalf("Start = %v, want SIGN_IN_IN_PROGRESS naming Ada", err)
		}
	})

	t.Run("returns the sign-in a client token started, within the caps", func(t *testing.T) {
		f := newSignInFixture(t)
		ctx := as("user-1")
		req := linearSignIn()
		req.ClientToken = "retry-1"
		first := f.start(t, ctx, req)
		replay := f.start(t, ctx, &foundryv1.StartConnectorSignInRequest{Kind: "linear", Name: "Acme Linear", ClientToken: "retry-1"})
		if replay.GetId() != first.GetId() {
			t.Fatalf("replay = %s, want %s", replay.GetId(), first.GetId())
		}
		for range maxLiveSignIns - 1 {
			f.start(t, ctx, linearSignIn())
		}
		_, err := f.s.StartConnectorSignIn(ctx, &foundryv1.StartConnectorSignInRequest{WorkspaceId: "ws-1", Kind: "linear"})
		if status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("Start past the cap = %v", err)
		}
	})

	t.Run("refuses a credential, and kinds the caller can't sign in to", func(t *testing.T) {
		f := newSignInFixture(t)
		_, err := f.s.StartConnectorSignIn(as("user-1"), &foundryv1.StartConnectorSignInRequest{WorkspaceId: "ws-1", Kind: "linear",
			Config: &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Linear{Linear: &foundryv1.LinearConnectorConfig{ApiKey: "lin_api_x"}}}})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("Start with a key = %v", err)
		}
		_, err = f.s.StartConnectorSignIn(as("user-1"), &foundryv1.StartConnectorSignInRequest{WorkspaceId: "ws-1", Kind: "stripe"})
		if reasonOf(err) != "SIGN_IN_UNAVAILABLE" {
			t.Fatalf("Start of stripe = %v", err)
		}
		f.broker.NoSignIn = map[string]bool{"user-3": true}
		if _, err := f.s.StartConnectorSignIn(as("user-3"), &foundryv1.StartConnectorSignInRequest{WorkspaceId: "ws-1", Kind: "linear"}); reasonOf(err) != "SIGN_IN_UNAVAILABLE" {
			t.Fatalf("Start without a WorkOS membership = %v", err)
		}
		f.s.deps.Accounts = nil
		if _, err := f.s.StartConnectorSignIn(as("user-1"), &foundryv1.StartConnectorSignInRequest{WorkspaceId: "ws-1", Kind: "linear"}); reasonOf(err) != "SIGN_IN_UNAVAILABLE" {
			t.Fatalf("Start without a broker = %v", err)
		}
	})

	t.Run("lists the kinds the caller signs in to, and GitHub's install link", func(t *testing.T) {
		f := newSignInFixture(t)
		signIn := func() map[string]bool {
			resp, err := f.s.ListConnectorKinds(as("user-1"), &foundryv1.ListConnectorKindsRequest{WorkspaceId: "ws-1"})
			if err != nil {
				t.Fatal(err)
			}
			out := map[string]bool{}
			for _, k := range resp.GetKinds() {
				out[k.GetKind()] = k.GetSignIn()
				if k.GetKind() == "github" && f.s.deps.Accounts != nil && !strings.Contains(k.GetGithub().GetInstallUrl(), "helix-foundry") {
					t.Fatalf("github = %v", k)
				}
			}
			return out
		}
		if got := signIn(); !got["github"] || !got["linear"] || got["stripe"] {
			t.Fatalf("sign_in = %v", got)
		}
		f.s.deps.Accounts = nil
		for kind, on := range signIn() {
			if on {
				t.Fatalf("%s signs in without a broker", kind)
			}
		}
	})

	// A broken account stops syncing until someone signs it in again; the
	// banner's list holds only such connectors.
	t.Run("refuses to sync a broken account, and lists it for signing in again", func(t *testing.T) {
		f := newSignInFixture(t)
		done := f.connect(t, "user-1", linearSignIn())
		f.connect(t, "user-1", linearSignIn())
		c := f.store.rows[done.GetConnectorId()]
		broken := ports.AccountMemberLeft
		c.Account.Broken = &broken
		_, err := f.s.SyncConnector(as("user-2"), &foundryv1.SyncConnectorRequest{WorkspaceId: "ws-1", Id: c.ID})
		if reasonOf(err) != "ACCOUNT_NEEDS_SIGN_IN" || !strings.Contains(status.Convert(err).Message(), "the member who connected it left") {
			t.Fatalf("SyncConnector = %v", err)
		}
		resp, err := f.s.ListConnectors(as("user-2"), &foundryv1.ListConnectorsRequest{WorkspaceId: "ws-1", NeedsSignIn: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.GetConnectors()) != 1 || resp.GetConnectors()[0].GetId() != c.ID {
			t.Fatalf("needs sign-in = %v", resp.GetConnectors())
		}
		account := resp.GetConnectors()[0].GetAccount()
		if account.GetState() != foundryv1.ConnectorAccountState_CONNECTOR_ACCOUNT_STATE_NEEDS_SIGN_IN ||
			account.GetBroken() != foundryv1.ConnectorAccountBroken_CONNECTOR_ACCOUNT_BROKEN_MEMBER_LEFT || account.GetConnectedBy() != "a former member" {
			t.Fatalf("account = %v", account)
		}
	})
}

// A signed-in connector's settings are tested with a vended token and still
// seal nothing; only the member whose access it reads with widens it.
func TestSignedInConnectorUpdates(t *testing.T) {
	f := newSignInFixture(t)
	f.providers.repos = []string{"acme/api", "acme/web", "acme/ops"}
	done := f.connect(t, "user-1", githubSignIn("acme/api", "acme/web"))
	id := done.GetConnectorId()
	update := func(userID string, repos ...string) error {
		_, err := f.s.UpdateConnector(as(userID), &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: id,
			Config: &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Github{Github: &foundryv1.GitHubConnectorConfig{Repos: repos}}}})
		f.store.syncs = nil // the sync new settings queue ran
		return err
	}
	for name, repos := range map[string][]string{"adds a repository": {"acme/api", "acme/ops"}, "clears the list": nil} {
		if err := update("user-2", repos...); reasonOf(err) != "ACCOUNT_OWNER_ONLY" || !strings.Contains(status.Convert(err).Message(), "Ada Lovelace") {
			t.Fatalf("another member %s: %v, want ACCOUNT_OWNER_ONLY naming Ada", name, err)
		}
	}
	if err := update("user-2", "ACME/API"); err != nil {
		t.Fatalf("another member narrows: %v", err)
	}
	if err := update("user-1", "acme/api", "acme/ops"); err != nil {
		t.Fatalf("the member widens: %v", err)
	}
	c := f.store.rows[id]
	if secret, err := f.keyring.Open(c.SecretCiphertext, c.SecretKeyID, connector.SecretContext(id)); err != nil || len(secret) != 0 || c.SecretHint != "" {
		t.Fatalf("sealed %q (%v), hint %q: the token must never be stored", secret, err, c.SecretHint)
	}
	_, err := f.s.UpdateConnector(as("user-1"), &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: id,
		Config: &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Github{Github: &foundryv1.GitHubConnectorConfig{Token: "ghp_abcdefghijklmnopqrstuvwxyz"}}}})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "sign it in again") {
		t.Fatalf("a key on a signed-in connector = %v", err)
	}

	f.broker.Break(c.Account.ID, ports.AccountExpired)
	if err := update("user-1", "acme/api"); reasonOf(err) != "ACCOUNT_NEEDS_SIGN_IN" {
		t.Fatalf("an expired account's re-test = %v", err)
	}
	f.broker.Unavailable = true
	if err := update("user-1", "acme/api"); status.Code(err) != codes.Unavailable {
		t.Fatalf("an unreachable broker's re-test = %v", err)
	}
}

// A sign-in's settings are checked as Prepare checks them, with no token.
func TestSignInSettings(t *testing.T) {
	catalog := connector.NewCatalog(connector.DefaultEndpoints(), connector.Guard{}, connector.S3Options{}, connector.EventsOptions{})
	github, err := catalog.Kind("github")
	if err != nil {
		t.Fatal(err)
	}
	got, err := signInSettings(context.Background(), catalog, github, nil)
	if err != nil || !json.Valid(got) || string(got) != `{"review_days":90}` {
		t.Fatalf("defaults = %s, %v", got, err)
	}
	if _, err := signInSettings(context.Background(), catalog, github, githubSignIn("not-a-repo").GetConfig()); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a malformed repository = %v", err)
	}
	if _, err := signInSettings(context.Background(), catalog, github, linearSignIn().GetConfig()); err != nil {
		t.Fatalf("no config is the kind's defaults: %v", err)
	}
	if _, err := signInSettings(context.Background(), catalog, github, &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_Linear{
		Linear: &foundryv1.LinearConnectorConfig{}}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("another kind's config = %v", err)
	}
	if widensReach("github", json.RawMessage(`{"repos":["a/b"]}`), json.RawMessage(`{"repos":["A/B"]}`)) ||
		!widensReach("github", json.RawMessage(`{"repos":["a/b"]}`), json.RawMessage(`{}`)) ||
		widensReach("github", json.RawMessage(`{}`), json.RawMessage(`{"repos":["a/b"]}`)) ||
		widensReach("linear", json.RawMessage(`{}`), json.RawMessage(`{}`)) {
		t.Fatal("widensReach")
	}
}
