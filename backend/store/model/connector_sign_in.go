package model

import (
	"encoding/json"
	"time"
)

// ConnectorSignInStatus is where a connector's sign-in is. A sign-in past its
// expiry reads expired unless it is finishing or connected; that status is
// never stored.
type ConnectorSignInStatus string

const (
	// ConnectorSignInPending is started, its link not yet followed.
	ConnectorSignInPending ConnectorSignInStatus = "pending"
	// ConnectorSignInAuthorizing is sent to the provider to authorize.
	ConnectorSignInAuthorizing ConnectorSignInStatus = "authorizing"
	// ConnectorSignInFinishing is claimed by the one finish that ends it.
	ConnectorSignInFinishing ConnectorSignInStatus = "finishing"
	ConnectorSignInConnected ConnectorSignInStatus = "connected"
	// ConnectorSignInDeclined and ConnectorSignInFailed may authorize again
	// until the sign-in expires.
	ConnectorSignInDeclined ConnectorSignInStatus = "declined"
	ConnectorSignInFailed   ConnectorSignInStatus = "failed"
	// ConnectorSignInExpired is how a sign-in past its expiry reads.
	ConnectorSignInExpired ConnectorSignInStatus = "expired"
)

// ConnectorSignInFailure is why a sign-in failed, a fixed code the site and
// agents word themselves; never the provider's text.
type ConnectorSignInFailure string

const (
	// SignInProviderError is the provider or the broker answering with an
	// error, or a callback that came back without a code.
	SignInProviderError ConnectorSignInFailure = "provider_error"
	// SignInCodeExpired is a callback's code that had lapsed, was used, or
	// isn't this sign-in's.
	SignInCodeExpired ConnectorSignInFailure = "code_expired"
	// SignInNotThisWorkspace is an account that isn't the workspace's, or
	// wasn't made by the user finishing; the broker deleted it.
	SignInNotThisWorkspace ConnectorSignInFailure = "not_this_workspace"
	// SignInTokenRejected is the provider refusing the token it issued.
	SignInTokenRejected ConnectorSignInFailure = "token_rejected"
	// SignInSettingsInvalid is the connector's settings refused.
	SignInSettingsInvalid ConnectorSignInFailure = "settings_invalid"
	// SignInUnavailable is the broker, or Foundry, unable to finish it now:
	// the broker unreachable, the workspace being deleted, a finish that
	// never ended.
	SignInUnavailable ConnectorSignInFailure = "unavailable"
)

// ConnectorSignIn connects a connector by signing in at its provider. A user,
// or their agent, starts it; only that user authorizes and finishes it, in
// their browser. Finishing creates the connector from Kind, Name and Config,
// or, with ConnectorID, signs that connector in again.
type ConnectorSignIn struct {
	ID          string
	WorkspaceID string
	Kind        string
	Name        string
	// Config is the connector's prepared settings; never a credential.
	Config json.RawMessage
	// ConnectorID is the connector it signs in again, or the one it made
	// once connected; nil while a new connector's sign-in isn't.
	ConnectorID *string
	Status      ConnectorSignInStatus
	// Failure is set with ConnectorSignInFailed.
	Failure *ConnectorSignInFailure
	// DiscoveryProblem is what the connector it made can't read, in
	// Foundry's words; the connector was made all the same.
	DiscoveryProblem *string
	// IdentityChanged is set when signing in again changed who the
	// provider says signed in.
	IdentityChanged bool
	// VerifierCiphertext is the PKCE verifier every authorization of the
	// sign-in shares, sealed under VerifierKeyID; nil until the first.
	VerifierCiphertext []byte
	VerifierKeyID      *string
	// AccountID is the broker's account, written as soon as the broker
	// saves it.
	AccountID       *string
	CreatedByUserID string
	ClientToken     *string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ExpiresAt       time.Time
}

// StatusAt is how the sign-in reads at now: as stored, except that one
// finishing since before staleBefore never ended (its finish runs for a
// bounded time) and reads failed, and one past its expiry that isn't
// finishing or connected reads expired. failure is set with failed.
func (s *ConnectorSignIn) StatusAt(now, staleBefore time.Time) (ConnectorSignInStatus, ConnectorSignInFailure) {
	status, failure := s.Status, ConnectorSignInFailure("")
	if s.Failure != nil {
		failure = *s.Failure
	}
	if status == ConnectorSignInFinishing && s.UpdatedAt.Before(staleBefore) {
		status, failure = ConnectorSignInFailed, SignInUnavailable
	}
	switch status {
	case ConnectorSignInFinishing, ConnectorSignInConnected:
	default:
		if !now.Before(s.ExpiresAt) {
			return ConnectorSignInExpired, ""
		}
	}
	return status, failure
}

// ConnectorSignInStart is a sign-in to start, how long it lasts unless
// authorized, and the caps it starts under.
type ConnectorSignInStart struct {
	SignIn *ConnectorSignIn
	TTL    time.Duration
	// MaxLive caps the user's sign-ins in the workspace that haven't ended;
	// MaxPerHour the sign-ins the user starts in an hour, in any workspace.
	MaxLive, MaxPerHour int
}

// ConnectorSignInEnd ends a finishing sign-in declined or failed.
type ConnectorSignInEnd struct {
	Status  ConnectorSignInStatus
	Failure ConnectorSignInFailure
	// Refused is an account the broker gave the finish and refused (ID and
	// UserID), which the end purges too, in case the broker's own delete
	// of it failed.
	Refused *ConnectorAccount
}

// ConnectorSignInConnect ends a finishing sign-in connected: it creates New,
// or signs ConnectorID in again with Account.
type ConnectorSignInConnect struct {
	WorkspaceID string
	SignInID    string
	// New is the connector the sign-in makes, with its Account; nil when it
	// signs ConnectorID in again.
	New *Connector
	// ConnectorID is the connector signed in again, and Account the account
	// it reads with now (ID, UserID, Label and Identity).
	ConnectorID string
	Account     ConnectorAccount
	// IdentityChanged has the connector's next sync read in full: what it
	// reads may differ under the new identity.
	IdentityChanged  bool
	DiscoveryProblem string
}

// ConnectorAccountPurge is a broker account to delete, as UserID.
type ConnectorAccountPurge struct {
	ID          string
	WorkspaceID string
	Kind        string
	AccountID   string
	UserID      string
	Attempts    int
	CreatedAt   time.Time
}
