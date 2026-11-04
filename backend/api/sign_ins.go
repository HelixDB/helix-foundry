package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/helixdb/helix-foundry/backend/connector"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"
	"github.com/helixdb/helix-foundry/backend/syncer"
)

// Connecting a connector by signing in at its provider (plans/
// SIGN_IN_CONNECTORS.md). A user, or their agent, starts a sign-in with the
// connector's kind, name and settings; that user authorizes it in their
// browser, where the provider sends them back to the site's callback, which
// finishes it. The account lives with the host's broker (ports.
// ProviderAccounts; hosted, WorkOS Pipes), owned by the workspace, and the
// connector asks it for a token at each use: no credential passes through
// an agent, the site or Foundry's database. Where there is no broker (a
// local build), no kind signs in.

// signInFinishTimeout bounds a finish's calls to the broker and the
// provider, which run past a closed tab or a front end's timeout; a test
// shortens it.
var signInFinishTimeout = 60 * time.Second

const (
	// signInTTL is how long a sign-in lasts from its start, and from each
	// authorization.
	signInTTL = 15 * time.Minute
	// signInRecordTimeout bounds each write that records how a finish went,
	// and the read that answers with it: each has its own, so a finish that
	// spent its time on the provider still records its end.
	signInRecordTimeout = 10 * time.Second
	// signInStale is when a finish that never ended (its process died)
	// reads failed and may authorize again: well past signInFinishTimeout.
	signInStale = 5 * time.Minute
	// maxLiveSignIns caps a user's sign-ins in a workspace that haven't
	// ended; maxSignInsPerHour the sign-ins a user starts in an hour.
	maxLiveSignIns    = 5
	maxSignInsPerHour = 20
	// The longest discovery problem and account label kept.
	maxDiscoveryProblemChars = 512
	maxAccountLabelChars     = 255
)

var connectorSignIns = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "helix_foundry_connector_sign_ins_total",
	Help: "Connector sign-ins by kind and outcome: started, authorized, connected, declined, or the failure that ended a finish.",
}, []string{"kind", "outcome"})

// StartConnectorSignIn starts connecting a connector by signing in, for the
// caller (a user, or their agent) to authorize in their browser: a pending
// sign-in, lasting signInTTL, with the connector's kind, name (the kind's
// name when none is given) and settings, which are checked as Prepare checks
// them and may hold no credential. With connector_id it signs that
// connector in again, keeping its name and settings. Refused with
// SIGN_IN_UNAVAILABLE where the caller can't sign in to the kind, with
// SIGN_IN_IN_PROGRESS while another member signs the connector in, and
// RESOURCE_EXHAUSTED past the caps. A client token seen before returns the
// sign-in it started.
func (s *Service) StartConnectorSignIn(ctx context.Context, req *foundryv1.StartConnectorSignInRequest) (*foundryv1.ConnectorSignInResponse, error) {
	caller := actor(ctx)
	if caller.UserID == "" {
		return nil, status.Error(codes.PermissionDenied, "only a signed-in user, or their agent, connects a source by signing in")
	}
	workspaceID := req.GetWorkspaceId()
	catalog, _, err := s.connectors(workspaceID)
	if err != nil {
		return nil, err
	}
	kind, err := catalog.Kind(req.GetKind())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "connector kind %q is not available", req.GetKind())
	}
	if err := s.requireSignIn(ctx, workspaceID, caller.UserID, kind); err != nil {
		return nil, err
	}
	if err := s.requireKind(ctx, workspaceID, kind); err != nil {
		return nil, err
	}
	si := &foundrymodel.ConnectorSignIn{ID: id.New(), WorkspaceID: workspaceID, Kind: kind.Name(), CreatedByUserID: caller.UserID}
	if token := req.GetClientToken(); token != "" {
		if len(token) > maxClientTokenLength {
			return nil, status.Errorf(codes.InvalidArgument, "client_token must be at most %d characters", maxClientTokenLength)
		}
		si.ClientToken = &token
	}
	if connectorID := req.GetConnectorId(); connectorID != "" {
		if req.GetName() != "" || req.GetConfig().GetKind() != nil {
			return nil, status.Error(codes.InvalidArgument, "signing a connector in again keeps its name and settings; change them on its page")
		}
		c, err := s.loadConnector(ctx, workspaceID, connectorID)
		if err != nil {
			return nil, err
		}
		if c.Kind != kind.Name() || c.Account == nil {
			return nil, status.Errorf(codes.FailedPrecondition, "%s doesn't sign in with %s", c.Name, kind.DisplayName())
		}
		si.ConnectorID, si.Name, si.Config = &c.ID, c.Name, c.Config
	} else {
		name := req.GetName()
		if strings.TrimSpace(name) == "" {
			name = kind.DisplayName()
		}
		if si.Name, err = connectorName(name); err != nil {
			return nil, err
		}
		if si.Config, err = signInSettings(ctx, catalog, kind, req.GetConfig()); err != nil {
			return nil, err
		}
	}
	stored, err := s.deps.DB.StartConnectorSignIn(ctx, foundrymodel.ConnectorSignInStart{SignIn: si, TTL: signInTTL,
		MaxLive: maxLiveSignIns, MaxPerHour: maxSignInsPerHour})
	var busy *foundrystore.SignInInProgressError
	switch {
	case errors.As(err, &busy):
		return nil, foundryPrecondition(workspaceID, "SIGN_IN_IN_PROGRESS",
			fmt.Sprintf("%s is signing this connector in already; try again once they finish", s.memberName(ctx, busy.UserID)))
	case errors.Is(err, foundrystore.ErrLimitExceeded):
		return nil, status.Errorf(codes.ResourceExhausted, "too many sign-ins: at most %d at once in a workspace, and %d an hour",
			maxLiveSignIns, maxSignInsPerHour)
	case errors.Is(err, foundrystore.ErrNotFound):
		return nil, status.Error(codes.NotFound, "the workspace or connector was not found")
	case err != nil:
		return nil, status.Errorf(codes.Internal, "start sign-in: %v", err)
	}
	if stored.ID == si.ID {
		connectorSignIns.WithLabelValues(kind.Name(), "started").Inc()
		log.From(ctx).Info("connector sign-in started", "workspace", workspaceID, "user", caller.UserID, "kind", kind.Name(),
			"sign_in", stored.ID, "connector", req.GetConnectorId())
	}
	return signInResponse(stored, caller.UserID)
}

// GetConnectorSignIn reads a sign-in of the workspace, whoever started it:
// mine says whether the caller did. Past its expiry it reads expired, unless
// it is finishing or connected.
func (s *Service) GetConnectorSignIn(ctx context.Context, req *foundryv1.GetConnectorSignInRequest) (*foundryv1.ConnectorSignInResponse, error) {
	caller := actor(ctx)
	if caller.UserID == "" {
		return nil, status.Error(codes.PermissionDenied, "only a signed-in user, or their agent, reads a sign-in")
	}
	si, err := s.loadSignIn(ctx, req.GetWorkspaceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	return signInResponse(si, caller.UserID)
}

// AuthorizeConnectorSignIn answers with the provider's page for the user who
// started the sign-in to authorize it, returning to return_to, one of
// foundry.connectors.sign_in_return_to. It may be called again, for a new
// page, until the sign-in connects: after a decline or a failure too, so a
// retry keeps the agent's wait on the same sign-in. The first call makes the
// PKCE verifier and seals it (signInVerifier); every later one shares it.
// Signing a connector in again reauthorizes its account when the caller is
// the member it acts as (reauthorizes); anyone else makes a new account,
// which replaces it. A reauthorization the broker answers account_missing
// is tried once more as a new account; only once that works is the
// connector's account recorded as missing, since the broker answers so for
// an integration that is switched off too. The sign-in then lasts signInTTL
// more.
func (s *Service) AuthorizeConnectorSignIn(ctx context.Context, req *foundryv1.AuthorizeConnectorSignInRequest) (
	*foundryv1.AuthorizeConnectorSignInResponse, error) {
	catalog, keyring, err := s.connectors(req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	si, caller, err := s.ownSignIn(ctx, req.GetWorkspaceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	kind, err := catalog.Kind(si.Kind)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "connector kind %q is no longer supported", si.Kind)
	}
	now := time.Now()
	switch current, _ := si.StatusAt(now, now.Add(-signInStale)); current {
	case foundrymodel.ConnectorSignInConnected, foundrymodel.ConnectorSignInFinishing, foundrymodel.ConnectorSignInExpired:
		return nil, signInEnded(si.WorkspaceID, current)
	}
	if !slices.Contains(s.connectorsConfig().SignInReturnTo, req.GetReturnTo()) {
		return nil, status.Error(codes.InvalidArgument, "return_to is not one of this deployment's sign-in callbacks")
	}
	if s.deps.Accounts == nil {
		return nil, signInUnavailable(si.WorkspaceID, kind)
	}
	auth := ports.AccountAuthorization{WorkspaceID: si.WorkspaceID, UserID: caller.UserID, Kind: si.Kind, Label: si.Name,
		ReturnTo: req.GetReturnTo()}
	var current *foundrymodel.Connector
	if si.ConnectorID != nil {
		current, err = s.loadConnector(ctx, si.WorkspaceID, *si.ConnectorID)
		if status.Code(err) == codes.NotFound {
			return nil, foundryPrecondition(si.WorkspaceID, "SIGN_IN_ENDED", "the connector this sign-in was for was deleted")
		}
		if err != nil {
			return nil, err
		}
		auth.AccountID = reauthorizes(current, caller.UserID)
	}
	verifier, err := s.signInVerifier(ctx, keyring, si)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign-in %s verifier: %v", si.ID, err)
	}
	auth.Challenge = pkceChallenge(verifier)
	url, err := s.deps.Accounts.Authorize(ctx, auth)
	var broken *ports.AccountBroken
	if auth.AccountID != "" && errors.As(err, &broken) && broken.Reason == ports.AccountAccountMissing {
		// The broker has no such account, or its integration is off: it
		// can't say which. A new account tells: if the broker makes one, the
		// old is gone, and Foundry's record says so, at the generation read;
		// if not, nothing is recorded and the retry's error is the answer.
		lost := auth.AccountID
		auth.AccountID = ""
		if url, err = s.deps.Accounts.Authorize(ctx, auth); err == nil {
			if _, berr := s.deps.DB.BreakConnectorAccount(ctx, si.WorkspaceID, current.ID, current.Account.Generation,
				ports.AccountAccountMissing); berr != nil {
				log.From(ctx).Warn("record a lost connector account failed", "connector", current.ID, "err", berr)
			}
			log.From(ctx).Info("a connector's account is gone from the broker; authorizing a new one", "workspace", si.WorkspaceID,
				"connector", current.ID, "account", lost, "sign_in", si.ID)
		}
	}
	if err != nil {
		log.From(ctx).Warn("authorize a connector sign-in failed", "workspace", si.WorkspaceID, "sign_in", si.ID, "kind", si.Kind, "err", err)
		return nil, accountsUnavailable(err)
	}
	err = s.deps.DB.AuthorizeConnectorSignIn(ctx, si.WorkspaceID, si.ID, signInTTL, signInStale)
	if errors.Is(err, foundrystore.ErrNotFound) {
		// It connected, expired or began finishing since it was read.
		return nil, signInEnded(si.WorkspaceID, foundrymodel.ConnectorSignInFinishing)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "authorize sign-in: %v", err)
	}
	connectorSignIns.WithLabelValues(si.Kind, "authorized").Inc()
	log.From(ctx).Info("connector sign-in authorizing", "workspace", si.WorkspaceID, "user", caller.UserID, "kind", si.Kind,
		"sign_in", si.ID, "reauthorize", auth.AccountID != "")
	return &foundryv1.AuthorizeConnectorSignInResponse{Url: url}, nil
}

// FinishConnectorSignIn finishes a sign-in with what the provider gave its
// callback: connect_code, or, without one, its error (access_denied is a
// decline). Only the user who started it finishes it, and only once: an
// authorizing sign-in is claimed for finishing, and every other call, a
// refreshed or doubled callback or a retry included, returns it as it
// stands, so no second code is spent and a connection is never undone. The
// finish runs past the caller's going away, within signInFinishTimeout
// (finishSignIn), and the sign-in is returned as it ended.
func (s *Service) FinishConnectorSignIn(ctx context.Context, req *foundryv1.FinishConnectorSignInRequest) (*foundryv1.ConnectorSignInResponse, error) {
	catalog, keyring, err := s.connectors(req.GetWorkspaceId())
	if err != nil {
		return nil, err
	}
	si, caller, err := s.ownSignIn(ctx, req.GetWorkspaceId(), req.GetId())
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if current, _ := si.StatusAt(now, now.Add(-signInStale)); current != foundrymodel.ConnectorSignInAuthorizing {
		return signInResponse(si, caller.UserID)
	}
	err = s.deps.DB.ClaimConnectorSignIn(ctx, si.WorkspaceID, si.ID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		// Another finish claimed it, or it expired.
	case err != nil:
		return nil, status.Errorf(codes.Internal, "claim sign-in: %v", err)
	default:
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), signInFinishTimeout)
		defer cancel()
		if err := s.finishSignIn(finishCtx, catalog, keyring, si, req.GetConnectCode(), req.GetError()); err != nil {
			return nil, err
		}
	}
	readCtx, cancel := recordContext(ctx)
	defer cancel()
	ended, err := s.loadSignIn(readCtx, si.WorkspaceID, si.ID)
	if err != nil {
		return nil, err
	}
	return signInResponse(ended, caller.UserID)
}

// finishSignIn ends a sign-in this call claimed. Without a code it is
// declined (access_denied) or failed (provider_error). Otherwise:
//  1. the broker saves the account, having checked it is the workspace's
//     and the starter's, and the sign-in records it at once;
//  2. the broker vends its token;
//  3. Prepare checks the settings with it;
//  4. the provider says who signed in (connector.Identifier): the account's
//     label, for people, and its stable id; signing in again as another
//     id, or where either id is unknown, has the next sync read in full;
//  5. Discover's failure is kept, in Foundry's words, as the sign-in's
//     discovery problem, and doesn't stop it;
//  6. one transaction creates the connector, due to sync now, or signs the
//     existing one in again, and connects the sign-in
//     (ConnectConnectorSignIn).
//
// A failure ends it failed with a fixed code, never the provider's text, and
// an account it saved goes to the purge, unless the connector reads with it
// already (FailConnectorSignIn): an account the connector signed in again
// reads with is never let go. Its error is the store's, when it can't record
// the end: the sign-in then reads failed once stale. ctx bounds the calls to
// the broker and the provider; each write that records the outcome has a
// time of its own (recordContext).
func (s *Service) finishSignIn(ctx context.Context, catalog *connector.Catalog, keyring *secretbox.Keyring,
	si *foundrymodel.ConnectorSignIn, code, callbackError string) error {
	ws := si.WorkspaceID
	if code == "" {
		if callbackError == "access_denied" {
			return s.endSignIn(ctx, si, foundrymodel.ConnectorSignInEnd{Status: foundrymodel.ConnectorSignInDeclined}, nil)
		}
		return s.endSignIn(ctx, si, foundrymodel.ConnectorSignInEnd{Status: foundrymodel.ConnectorSignInFailed,
			Failure: foundrymodel.SignInProviderError}, fmt.Errorf("the callback brought no code (error %q)", callbackError))
	}
	fail := func(failure foundrymodel.ConnectorSignInFailure, err error) error {
		return s.endSignIn(ctx, si, foundrymodel.ConnectorSignInEnd{Status: foundrymodel.ConnectorSignInFailed, Failure: failure}, err)
	}
	kind, err := catalog.Kind(si.Kind)
	if err != nil {
		return fail(foundrymodel.SignInSettingsInvalid, err)
	}
	if s.deps.Accounts == nil || si.VerifierCiphertext == nil || si.VerifierKeyID == nil {
		return fail(foundrymodel.SignInUnavailable, errors.New("no broker, or no verifier"))
	}
	verifier, err := keyring.Open(si.VerifierCiphertext, *si.VerifierKeyID, signInContext(si.ID))
	if err != nil {
		return fail(foundrymodel.SignInUnavailable, fmt.Errorf("open the verifier: %w", err))
	}
	// The connector signed in again comes first: the account it reads with
	// is the broker's to keep (Finish's ref.AccountID when reauthorized), and
	// never deleted here.
	var current *foundrymodel.Connector
	settings := si.Config
	ref := ports.AccountRef{WorkspaceID: ws, UserID: si.CreatedByUserID, Kind: si.Kind}
	if si.ConnectorID != nil {
		if current, err = s.deps.DB.GetConnector(ctx, ws, *si.ConnectorID); err != nil {
			return fail(foundrymodel.SignInUnavailable, fmt.Errorf("read the connector signed in again: %w", err))
		}
		settings, ref.AccountID = current.Config, reauthorizes(current, si.CreatedByUserID)
	}
	account, err := s.deps.Accounts.Finish(ctx, ref, code, string(verifier))
	if err != nil {
		end := foundrymodel.ConnectorSignInEnd{Status: foundrymodel.ConnectorSignInFailed, Failure: foundrymodel.SignInProviderError}
		switch {
		case errors.Is(err, ports.ErrAccountCodeRejected):
			end.Failure = foundrymodel.SignInCodeExpired
		case errors.Is(err, ports.ErrAccountNotThisWorkspace):
			end.Failure = foundrymodel.SignInNotThisWorkspace
		case errors.Is(err, ports.ErrAccountsUnavailable):
			end.Failure = foundrymodel.SignInUnavailable
		}
		// An account the broker refused is purged too, in case its own
		// delete failed; never the one the sign-in reauthorized.
		if account.ID != "" && account.ID != ref.AccountID {
			end.Refused = &foundrymodel.ConnectorAccount{ID: account.ID, UserID: account.UserID}
		}
		return s.endSignIn(ctx, si, end, err)
	}
	if err := s.recordAccount(ctx, si, current, account); err != nil {
		return err
	}
	credential, err := s.deps.Accounts.Token(ctx, ports.AccountRef{WorkspaceID: ws, UserID: account.UserID, Kind: si.Kind, AccountID: account.ID})
	var broken *ports.AccountBroken
	switch {
	case errors.As(err, &broken):
		return fail(foundrymodel.SignInTokenRejected, err)
	case errors.Is(err, ports.ErrAccountsUnavailable):
		return fail(foundrymodel.SignInUnavailable, err)
	case err != nil:
		return fail(foundrymodel.SignInProviderError, err)
	case strings.TrimSpace(credential.Value) == "":
		return fail(foundrymodel.SignInTokenRejected, errors.New("the broker vended an empty token"))
	}
	src := connector.Source{Settings: settings, Secret: credential.Value, WorkspaceID: ws, SignIn: true}
	if src.Settings, err = kind.Prepare(src); err != nil {
		return fail(foundrymodel.SignInSettingsInvalid, err)
	}
	var label, identity string
	if identifier, ok := kind.(connector.Identifier); ok {
		name, id, err := identifier.Identity(ctx, src)
		var rejected *connector.AuthError
		var refusedSettings *connector.InvalidError
		switch {
		case errors.As(err, &rejected):
			return fail(foundrymodel.SignInTokenRejected, err)
		case errors.As(err, &refusedSettings):
			return fail(foundrymodel.SignInSettingsInvalid, err)
		case err != nil:
			return fail(foundrymodel.SignInProviderError, err)
		}
		label, identity = accountLabel(name), clipChars(id, maxAccountLabelChars)
	}
	// Labels repeat (two Slack teams of one name), so the id decides. An id
	// unknown on either side counts as a change: a full read costs one
	// sync, and a wrong guess would keep rows the new identity can't see.
	identityChanged := current != nil && current.Account != nil &&
		(current.Account.Identity == "" || identity == "" || current.Account.Identity != identity)
	var problem string
	discovery, err := kind.Discover(ctx, src)
	var rejected *connector.AuthError
	switch {
	case errors.As(err, &rejected):
		return fail(foundrymodel.SignInTokenRejected, err)
	case err != nil:
		problem = discoveryProblem(kind, err)
		log.From(ctx).Info("a connector sign-in's discovery failed", "workspace", ws, "sign_in", si.ID, "err", err)
	case discovery.Settings != nil:
		src.Settings = discovery.Settings
	}
	connect := foundrymodel.ConnectorSignInConnect{WorkspaceID: ws, SignInID: si.ID, IdentityChanged: identityChanged,
		DiscoveryProblem: problem}
	signedIn := foundrymodel.ConnectorAccount{ID: account.ID, UserID: account.UserID, Label: label, Identity: identity}
	if current != nil {
		connect.ConnectorID, connect.Account = current.ID, signedIn
	} else {
		// A signed-in connector holds no credential: it seals nothing, as a
		// kind that needs none does.
		c := &foundrymodel.Connector{ID: id.New(), WorkspaceID: ws, Kind: si.Kind, Name: si.Name, Config: src.Settings,
			CreatedByUserID: &si.CreatedByUserID, Account: &signedIn}
		now := time.Now()
		c.NextSyncAt = &now
		if c.SecretCiphertext, c.SecretKeyID, err = keyring.Seal(nil, connector.SecretContext(c.ID)); err != nil {
			return fail(foundrymodel.SignInUnavailable, fmt.Errorf("seal the connector's empty credential: %w", err))
		}
		connect.New = c
	}
	recordCtx, cancel := recordContext(ctx)
	defer cancel()
	err = s.deps.DB.ConnectConnectorSignIn(recordCtx, connect)
	switch {
	case errors.Is(err, foundrystore.ErrWorkspaceDeleting):
		return fail(foundrymodel.SignInUnavailable, err)
	case errors.Is(err, foundrystore.ErrNotFound):
		return fail(foundrymodel.SignInUnavailable, fmt.Errorf("the connector was deleted, or the sign-in ended: %w", err))
	case err != nil:
		return fail(foundrymodel.SignInUnavailable, err)
	}
	connectorSignIns.WithLabelValues(si.Kind, "connected").Inc()
	log.From(ctx).Info("connector sign-in connected", "workspace", ws, "user", si.CreatedByUserID, "kind", si.Kind, "sign_in", si.ID,
		"account", account.ID, "account_user", account.UserID, "again", current != nil, "identity_changed", identityChanged)
	return nil
}

// endSignIn ends a claimed sign-in declined or failed, err (logged only)
// saying why.
func (s *Service) endSignIn(ctx context.Context, si *foundrymodel.ConnectorSignIn, end foundrymodel.ConnectorSignInEnd, err error) error {
	outcome := string(end.Status)
	if end.Failure != "" {
		outcome = string(end.Failure)
	}
	connectorSignIns.WithLabelValues(si.Kind, outcome).Inc()
	log.From(ctx).Warn("connector sign-in ended", "workspace", si.WorkspaceID, "user", si.CreatedByUserID, "kind", si.Kind,
		"sign_in", si.ID, "outcome", outcome, "err", err)
	recordCtx, cancel := recordContext(ctx)
	defer cancel()
	if err := s.deps.DB.FailConnectorSignIn(recordCtx, si.WorkspaceID, si.ID, end); err != nil {
		return status.Errorf(codes.Internal, "end sign-in: %v", err)
	}
	return nil
}

// recordAccount records the account the broker saved on the sign-in, so
// that one the finish never uses is purged. When it can't, nothing would
// purge it: it is deleted now, unless it is the account current, the
// connector signed in again, reads with, which reauthorizing gave back.
func (s *Service) recordAccount(ctx context.Context, si *foundrymodel.ConnectorSignIn, current *foundrymodel.Connector,
	account ports.ProviderAccount) error {
	recordCtx, cancel := recordContext(ctx)
	defer cancel()
	err := s.deps.DB.SetConnectorSignInAccount(recordCtx, si.WorkspaceID, si.ID, account.ID)
	if err == nil {
		return nil
	}
	if current == nil || current.Account == nil || current.Account.ID != account.ID {
		ref := ports.AccountRef{WorkspaceID: si.WorkspaceID, UserID: account.UserID, Kind: si.Kind, AccountID: account.ID}
		if derr := s.deps.Accounts.Delete(recordCtx, ref); derr != nil {
			log.From(ctx).Error("delete an unrecorded connector account failed", "workspace", si.WorkspaceID, "sign_in", si.ID,
				"account", account.ID, "err", derr)
		}
	}
	return status.Errorf(codes.Internal, "record the sign-in's account: %v", err)
}

// recordContext is a context of its own, for a write that records how a
// finish went: the finish's may have run out, and the caller may be gone.
func recordContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), signInRecordTimeout)
}

// reauthorizes is the account of c a sign-in by userID reauthorizes: its own
// account, when userID is the member it acts as and the broker still has
// it. An account can't move to another member, so anyone else, like a
// member signing in after the account's member left, makes a new account
// (""), which replaces it.
func reauthorizes(c *foundrymodel.Connector, userID string) string {
	a := c.Account
	if a == nil || a.UserID != userID {
		return ""
	}
	if a.Broken != nil && (*a.Broken == ports.AccountMemberLeft || *a.Broken == ports.AccountAccountMissing) {
		return ""
	}
	return a.ID
}

// ownSignIn reads the caller's own sign-in, for authorizing or finishing,
// which only the user who started it does, in their browser: an agent's
// call, or another user's, is refused (SIGN_IN_NOT_YOURS for a forwarded
// link).
func (s *Service) ownSignIn(ctx context.Context, workspaceID, id string) (*foundrymodel.ConnectorSignIn, ports.Actor, error) {
	caller := actor(ctx)
	if caller.UserID == "" || caller.Source != ports.SourceUser {
		return nil, caller, status.Error(codes.PermissionDenied, "only the person who started a sign-in authorizes and finishes it, in their browser")
	}
	si, err := s.loadSignIn(ctx, workspaceID, id)
	if err != nil {
		return nil, caller, err
	}
	if si.CreatedByUserID != caller.UserID {
		return nil, caller, foundryPrecondition(workspaceID, "SIGN_IN_NOT_YOURS",
			"This link was made for someone else: ask your agent for a new one")
	}
	return si, caller, nil
}

func (s *Service) loadSignIn(ctx context.Context, workspaceID, id string) (*foundrymodel.ConnectorSignIn, error) {
	if workspaceID == "" || id == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id and id are required")
	}
	si, err := s.deps.DB.GetConnectorSignIn(ctx, workspaceID, id)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "sign-in not found")
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get sign-in: %v", err)
	}
	return si, nil
}

// requireSignIn refuses a kind userID can't connect to the workspace by
// signing in (SIGN_IN_UNAVAILABLE): there is no broker (a local build), or
// it doesn't offer the kind to them (hosted: no WorkOS organization, no
// WorkOS membership of it, or signing in switched off).
func (s *Service) requireSignIn(ctx context.Context, workspaceID, userID string, kind connector.Kind) error {
	if s.deps.Accounts == nil {
		return signInUnavailable(workspaceID, kind)
	}
	kinds, err := s.deps.Accounts.Kinds(ctx, workspaceID, userID)
	if err != nil {
		log.From(ctx).Warn("read the kinds a user signs in to failed", "workspace", workspaceID, "user", userID, "err", err)
		return accountsUnavailable(err)
	}
	if !slices.Contains(kinds, kind.Name()) {
		return foundryPrecondition(workspaceID, "SIGN_IN_UNAVAILABLE", fmt.Sprintf(
			"Connecting %s by signing in isn't available to you in this workspace: it needs your membership of the workspace's WorkOS organization. "+
				"Use a token instead", kind.DisplayName()))
	}
	return nil
}

// signInKinds are the kinds userID can connect to the workspace by signing
// in; none when there is no broker or it can't be read, so listing kinds
// never fails on it.
func (s *Service) signInKinds(ctx context.Context, workspaceID, userID string) []string {
	if s.deps.Accounts == nil || userID == "" {
		return nil
	}
	kinds, err := s.deps.Accounts.Kinds(ctx, workspaceID, userID)
	if err != nil {
		log.From(ctx).Warn("read the kinds a user signs in to failed", "workspace", workspaceID, "user", userID, "err", err)
		return nil
	}
	return kinds
}

// signInUnavailable refuses signing in where no broker is (SIGN_IN_UNAVAILABLE).
func signInUnavailable(workspaceID string, kind connector.Kind) error {
	return foundryPrecondition(workspaceID, "SIGN_IN_UNAVAILABLE",
		fmt.Sprintf("Connecting %s by signing in isn't offered here; use a token instead", kind.DisplayName()))
}

// signInEnded refuses to authorize a sign-in that has ended (SIGN_IN_ENDED).
func signInEnded(workspaceID string, as foundrymodel.ConnectorSignInStatus) error {
	msg := "This sign-in is finishing; wait for it to end"
	switch as {
	case foundrymodel.ConnectorSignInConnected:
		msg = "This sign-in is connected already"
	case foundrymodel.ConnectorSignInExpired:
		msg = "This sign-in ran out: start again"
	}
	return foundryPrecondition(workspaceID, "SIGN_IN_ENDED", msg)
}

// accountsUnavailable is the broker's failure as callers see it: Foundry's
// words, never the broker's.
func accountsUnavailable(err error) error {
	if errors.Is(err, ports.ErrAccountsUnavailable) {
		return status.Error(codes.Unavailable, "signing in to providers is unavailable right now; try again")
	}
	return status.Error(codes.Unavailable, "the provider's sign-in could not be reached; try again")
}

// signInSettings reads the settings a sign-in starts with from cfg, of kind,
// nil being the kind's defaults. A credential in it is refused: signing in
// is the credential. Prepare checks the rest as it would with a token.
func signInSettings(ctx context.Context, catalog *connector.Catalog, kind connector.Kind, cfg *foundryv1.ConnectorConfig) (json.RawMessage, error) {
	if cfg.GetKind() == nil {
		empty, err := configToProto(kind.Name(), json.RawMessage(`{}`))
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "%s takes no settings to sign in with", kind.DisplayName())
		}
		cfg = empty
	}
	configKind, src, _, err := sourceFromProto(catalog, cfg)
	if err != nil {
		return nil, err
	}
	if configKind.Name() != kind.Name() {
		return nil, status.Errorf(codes.InvalidArgument, "config is for %s, not %s", configKind.DisplayName(), kind.DisplayName())
	}
	if src.Secret != "" {
		return nil, status.Errorf(codes.InvalidArgument, "a sign-in takes no credential: signing in at %s is the credential", kind.DisplayName())
	}
	src.SignIn = true
	settings, err := kind.Prepare(src)
	if err != nil {
		return nil, connectorError(ctx, err)
	}
	return settings, nil
}

// signInVerifier is the sign-in's PKCE verifier: made and sealed by its
// first authorization (under the connector keyring, bound to the sign-in),
// and shared by every later one. Two first authorizations at once keep the
// one stored first.
func (s *Service) signInVerifier(ctx context.Context, keyring *secretbox.Keyring, si *foundrymodel.ConnectorSignIn) (string, error) {
	for range 2 {
		if si.VerifierCiphertext != nil && si.VerifierKeyID != nil {
			verifier, err := keyring.Open(si.VerifierCiphertext, *si.VerifierKeyID, signInContext(si.ID))
			return string(verifier), err
		}
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		verifier := base64.RawURLEncoding.EncodeToString(raw)
		sealed, keyID, err := keyring.Seal([]byte(verifier), signInContext(si.ID))
		if err != nil {
			return "", err
		}
		err = s.deps.DB.SetConnectorSignInVerifier(ctx, si.WorkspaceID, si.ID, sealed, keyID)
		if err == nil {
			return verifier, nil
		}
		if !errors.Is(err, foundrystore.ErrNotFound) {
			return "", err
		}
		if si, err = s.deps.DB.GetConnectorSignIn(ctx, si.WorkspaceID, si.ID); err != nil {
			return "", err
		}
	}
	return "", errors.New("no verifier was stored")
}

// signInContext binds a sealed verifier to its sign-in.
func signInContext(id string) string { return "sign-in:" + id }

// pkceChallenge is a verifier's S256 challenge.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// accountLabel is the provider's name for who signed in, as shown: control
// characters dropped, at most maxAccountLabelChars.
func accountLabel(identity string) string {
	label := strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, identity))
	return clipChars(label, maxAccountLabelChars)
}

// discoveryProblem is what a connector a sign-in made can't read, in
// Foundry's words: a kind's message (a kind keeps the provider's text out of
// a signed-in source's messages), else a general one. The detail is logged
// only.
func discoveryProblem(kind connector.Kind, err error) string {
	var invalidErr *connector.InvalidError
	var refusedErr *connector.RefusedError
	switch {
	case errors.As(err, &invalidErr):
		return clipChars(invalidErr.Message, maxDiscoveryProblemChars)
	case errors.As(err, &refusedErr):
		return clipChars(refusedErr.Message, maxDiscoveryProblemChars)
	}
	return "Foundry couldn't read " + kind.DisplayName() + " yet; Sync now tries again"
}

func clipChars(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// memberName is what a refusal calls a member: their name, else "another
// member".
func (s *Service) memberName(ctx context.Context, userID string) string {
	if s.deps.People != nil {
		if name, err := s.deps.People.DisplayName(ctx, userID); err == nil && name != "" {
			return name
		}
	}
	return "Another member"
}

var signInStatusToProto = map[foundrymodel.ConnectorSignInStatus]foundryv1.ConnectorSignInStatus{
	foundrymodel.ConnectorSignInPending:     foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_PENDING,
	foundrymodel.ConnectorSignInAuthorizing: foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_AUTHORIZING,
	foundrymodel.ConnectorSignInFinishing:   foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_FINISHING,
	foundrymodel.ConnectorSignInConnected:   foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_CONNECTED,
	foundrymodel.ConnectorSignInDeclined:    foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_DECLINED,
	foundrymodel.ConnectorSignInFailed:      foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_FAILED,
	foundrymodel.ConnectorSignInExpired:     foundryv1.ConnectorSignInStatus_CONNECTOR_SIGN_IN_STATUS_EXPIRED,
}

var signInFailureToProto = map[foundrymodel.ConnectorSignInFailure]foundryv1.ConnectorSignInFailure{
	foundrymodel.SignInProviderError:    foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_PROVIDER_ERROR,
	foundrymodel.SignInCodeExpired:      foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_CODE_EXPIRED,
	foundrymodel.SignInNotThisWorkspace: foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_NOT_THIS_WORKSPACE,
	foundrymodel.SignInTokenRejected:    foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_TOKEN_REJECTED,
	foundrymodel.SignInSettingsInvalid:  foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_SETTINGS_INVALID,
	foundrymodel.SignInUnavailable:      foundryv1.ConnectorSignInFailure_CONNECTOR_SIGN_IN_FAILURE_UNAVAILABLE,
}

// signInResponse is the sign-in as it reads now to callerID: its settings,
// never a credential, and no provider text.
func signInResponse(si *foundrymodel.ConnectorSignIn, callerID string) (*foundryv1.ConnectorSignInResponse, error) {
	now := time.Now()
	current, failure := si.StatusAt(now, now.Add(-signInStale))
	cfg, err := configToProto(si.Kind, si.Config)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read sign-in %s settings: %v", si.ID, err)
	}
	out := &foundryv1.ConnectorSignIn{
		Id: si.ID, Kind: si.Kind, Name: si.Name, Status: signInStatusToProto[current], Failure: signInFailureToProto[failure],
		IdentityChanged: si.IdentityChanged, Mine: si.CreatedByUserID == callerID, Config: cfg,
		CreatedAt: timestamppb.New(si.CreatedAt), ExpiresAt: timestamppb.New(si.ExpiresAt),
	}
	if si.ConnectorID != nil {
		out.ConnectorId = *si.ConnectorID
	}
	if si.DiscoveryProblem != nil {
		out.DiscoveryProblem = *si.DiscoveryProblem
	}
	return &foundryv1.ConnectorSignInResponse{SignIn: out}, nil
}

// accountToProto is a signed-in connector's account, with the name of the
// member it reads as: "a former member" once they left.
func (s *Service) accountToProto(ctx context.Context, c *foundrymodel.Connector) *foundryv1.ConnectorAccount {
	a := c.Account
	if a == nil {
		return nil
	}
	out := &foundryv1.ConnectorAccount{Label: a.Label, State: foundryv1.ConnectorAccountState_CONNECTOR_ACCOUNT_STATE_CONNECTED,
		UserId: a.UserID, ConnectedBy: s.connectedBy(ctx, a)}
	if a.ConnectedAt != nil {
		out.ConnectedAt = timestamppb.New(*a.ConnectedAt)
	}
	if a.Broken != nil {
		out.State, out.Broken = foundryv1.ConnectorAccountState_CONNECTOR_ACCOUNT_STATE_NEEDS_SIGN_IN, accountBrokenToProto[*a.Broken]
		if a.BrokenAt != nil {
			out.BrokenAt = timestamppb.New(*a.BrokenAt)
		}
	}
	return out
}

var accountBrokenToProto = map[string]foundryv1.ConnectorAccountBroken{
	ports.AccountMemberLeft:     foundryv1.ConnectorAccountBroken_CONNECTOR_ACCOUNT_BROKEN_MEMBER_LEFT,
	ports.AccountAccessRevoked:  foundryv1.ConnectorAccountBroken_CONNECTOR_ACCOUNT_BROKEN_ACCESS_REVOKED,
	ports.AccountExpired:        foundryv1.ConnectorAccountBroken_CONNECTOR_ACCOUNT_BROKEN_EXPIRED,
	ports.AccountAccountMissing: foundryv1.ConnectorAccountBroken_CONNECTOR_ACCOUNT_BROKEN_ACCOUNT_MISSING,
}

// connectedBy names the member an account reads as.
func (s *Service) connectedBy(ctx context.Context, a *foundrymodel.ConnectorAccount) string {
	const former = "a former member"
	if a.Broken != nil && *a.Broken == ports.AccountMemberLeft {
		return former
	}
	if s.deps.People == nil {
		return ""
	}
	name, err := s.deps.People.DisplayName(ctx, a.UserID)
	switch {
	case errors.Is(err, foundrystore.ErrNotFound):
		return former
	case err != nil:
		log.From(ctx).Warn("read the member an account reads as failed", "user", a.UserID, "err", err)
		return ""
	}
	return name
}

// credentials opens connectors' credentials as the syncer does: the sealed
// key, or a signed-in connector's token, asked of the broker.
func (s *Service) credentials() syncer.Credentials {
	return syncer.Credentials{Keys: s.deps.ConnectorKeys, Accounts: s.deps.Accounts, Checked: s.deps.DB}
}

// accountError is a signed-in connector's credential failing, as a caller
// sees it: an account that needs signing in again, broken or vending a
// token no provider takes (ACCOUNT_NEEDS_SIGN_IN), or a broker that can't
// be reached.
func accountError(c *foundrymodel.Connector, kind connector.Kind, err error) error {
	var broken *ports.AccountBroken
	var rejected *connector.AuthError
	switch {
	case errors.As(err, &broken):
		return foundryPrecondition(c.WorkspaceID, "ACCOUNT_NEEDS_SIGN_IN", syncer.AccountBrokenMessage(kind.DisplayName(), broken.Reason))
	case errors.As(err, &rejected):
		return foundryPrecondition(c.WorkspaceID, "ACCOUNT_NEEDS_SIGN_IN",
			fmt.Sprintf("%s rejected the access this connector signed in with: sign it in again", kind.DisplayName()))
	}
	return accountsUnavailable(err)
}
