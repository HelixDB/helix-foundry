// Package portstest has in-memory stand-ins for Foundry's ports, for the
// tests of what uses them.
package portstest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/helixdb/helix-foundry/backend/ports"
)

// Accounts is a ports.ProviderAccounts that keeps its accounts in memory and
// answers as the hosted broker (WorkOS Pipes) does: Authorize hands out a
// code bound to its PKCE challenge, which Finish takes once, with the
// verifier that matches; Token vends each account's current token, a new one
// after Rotate. Its zero value offers no kind; it is safe for concurrent use.
type Accounts struct {
	// Offered are the kinds Kinds returns for every user NoSignIn leaves out.
	Offered  []string
	NoSignIn map[string]bool
	// Unavailable fails every call with ports.ErrAccountsUnavailable.
	Unavailable bool
	// Foreign has Finish find each account it saves another workspace's: it
	// deletes it, unless it is ref.AccountID, and fails with
	// ports.ErrAccountNotThisWorkspace, returning the account it refused.
	Foreign bool
	// KeepRefused makes Finish's own delete of an account it refuses fail:
	// the broker keeps it.
	KeepRefused bool
	// TokenPrefix starts every token vended, as a provider's own tokens
	// start (xoxb- for a Slack bot's).
	TokenPrefix string
	// Disabled is the integration switched off, as Pipes answers it:
	// reauthorizing any account reads as account_missing, and authorizing a
	// new one is unavailable.
	Disabled bool
	// OnFinish runs inside each Finish, after the code is spent and before it
	// returns, as a slow broker would let other calls in.
	OnFinish func()

	mu             sync.Mutex
	failures       map[string]error
	empties        map[string]int
	authorizations []ports.AccountAuthorization
	finishes       []ports.AccountRef
	codes          map[string]*code
	accounts       map[string]*account
	calls          map[string]int
	next           int
}

type code struct {
	challenge string
	auth      ports.AccountAuthorization
	used      bool
}

type account struct {
	ref    ports.AccountRef
	token  int
	broken string
}

var _ ports.ProviderAccounts = (*Accounts)(nil)

func (a *Accounts) call(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.calls == nil {
		a.calls, a.codes, a.accounts = map[string]int{}, map[string]*code{}, map[string]*account{}
	}
	a.calls[name]++
	if a.Unavailable {
		return fmt.Errorf("portstest: %s: %w", name, ports.ErrAccountsUnavailable)
	}
	return nil
}

func (a *Accounts) Kinds(_ context.Context, _, userID string) ([]string, error) {
	if err := a.call("Kinds"); err != nil {
		return nil, err
	}
	if a.NoSignIn[userID] {
		return nil, nil
	}
	return slices.Clone(a.Offered), nil
}

// Authorize answers with the provider's page, which carries the code its
// callback gets (Code). Reauthorizing an account it doesn't have (deleted,
// say) is an AccountBroken for account_missing, as the hosted broker
// answers.
func (a *Accounts) Authorize(_ context.Context, auth ports.AccountAuthorization) (string, error) {
	if err := a.call("Authorize"); err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.accounts[auth.AccountID]; auth.AccountID != "" && (!ok || a.Disabled) {
		return "", &ports.AccountBroken{Reason: ports.AccountAccountMissing}
	}
	if a.Disabled {
		return "", fmt.Errorf("portstest: the integration is off: %w", ports.ErrAccountsUnavailable)
	}
	a.next++
	c := fmt.Sprintf("code-%d", a.next)
	a.codes[c] = &code{challenge: auth.Challenge, auth: auth}
	a.authorizations = append(a.authorizations, auth)
	return "https://provider.test/authorize?code=" + c, nil
}

func (a *Accounts) Finish(_ context.Context, ref ports.AccountRef, codeValue, verifier string) (ports.ProviderAccount, error) {
	if err := a.call("Finish"); err != nil {
		return ports.ProviderAccount{}, err
	}
	saved, err := a.finish(ref, codeValue, verifier)
	if err == nil && a.OnFinish != nil {
		a.OnFinish()
	}
	return saved, err
}

// finish spends the code as the broker does. Reauthorizing an account
// keeps it, and the member who made it: one reauthorized by anyone else
// isn't theirs to finish. An account that doesn't check out is deleted,
// unless it is ref.AccountID.
func (a *Accounts) finish(ref ports.AccountRef, codeValue, verifier string) (ports.ProviderAccount, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.finishes = append(a.finishes, ref)
	c, ok := a.codes[codeValue]
	sum := sha256.Sum256([]byte(verifier))
	if !ok || c.used || base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge || c.auth.WorkspaceID != ref.WorkspaceID {
		return ports.ProviderAccount{}, ports.ErrAccountCodeRejected
	}
	c.used = true
	maker := ref.UserID
	id := c.auth.AccountID
	if existing, ok := a.accounts[id]; ok && id != "" {
		existing.broken, maker = "", existing.ref.UserID
	} else {
		a.next++
		id = fmt.Sprintf("acct-%d", a.next)
		a.accounts[id] = &account{ref: ports.AccountRef{WorkspaceID: ref.WorkspaceID, UserID: maker, Kind: c.auth.Kind, AccountID: id}}
	}
	if a.Foreign || maker != ref.UserID {
		if id != ref.AccountID && !a.KeepRefused {
			delete(a.accounts, id)
		}
		return ports.ProviderAccount{ID: id, UserID: maker}, ports.ErrAccountNotThisWorkspace
	}
	a.accounts[id].token++
	return ports.ProviderAccount{ID: id, UserID: maker}, nil
}

// Token vends the account's current token, expiring in an hour.
func (a *Accounts) Token(_ context.Context, ref ports.AccountRef) (ports.Credential, error) {
	if err := a.call("Token"); err != nil {
		return ports.Credential{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	acct, ok := a.accounts[ref.AccountID]
	switch {
	case a.failures[ref.AccountID] != nil:
		return ports.Credential{}, a.failures[ref.AccountID]
	case a.empties[ref.AccountID] > 0:
		a.empties[ref.AccountID]--
		return ports.Credential{Value: " "}, nil
	case !ok:
		return ports.Credential{}, &ports.AccountBroken{Reason: ports.AccountAccountMissing}
	case acct.broken != "":
		return ports.Credential{}, &ports.AccountBroken{Reason: acct.broken}
	}
	expires := time.Now().Add(time.Hour)
	return ports.Credential{Value: a.tokenOf(ref.AccountID, acct.token), ExpiresAt: &expires}, nil
}

// Delete forgets the account; one already gone is no error.
func (a *Accounts) Delete(_ context.Context, ref ports.AccountRef) error {
	if err := a.call("Delete"); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.accounts, ref.AccountID)
	return nil
}

func (a *Accounts) tokenOf(accountID string, n int) string {
	return fmt.Sprintf("%stoken-%s-%d", a.TokenPrefix, accountID, n)
}

// Code is the code the n-th Authorize (from 0) gave its callback.
func (a *Accounts) Code(n int) string { return fmt.Sprintf("code-%d", a.codeNumber(n)) }

func (a *Accounts) codeNumber(n int) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	seen := 0
	for i := 1; i <= a.next; i++ {
		if _, ok := a.codes[fmt.Sprintf("code-%d", i)]; ok {
			if seen == n {
				return i
			}
			seen++
		}
	}
	return 0
}

// Finishes are the refs each Finish was given, in order.
func (a *Accounts) Finishes() []ports.AccountRef {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.finishes)
}

// Authorizations are what each Authorize was asked, in order.
func (a *Accounts) Authorizations() []ports.AccountAuthorization {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.authorizations)
}

// Calls is how many times the method name was called.
func (a *Accounts) Calls(name string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[name]
}

// Add stores an account, as a finished sign-in would, and returns its first
// token.
func (a *Accounts) Add(ref ports.AccountRef) string {
	_ = a.call("Add")
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accounts[ref.AccountID] = &account{ref: ref, token: 1}
	return a.tokenOf(ref.AccountID, 1)
}

// Has reports whether the broker holds the account.
func (a *Accounts) Has(accountID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.accounts[accountID]
	return ok
}

// CurrentToken is the token Token vends for the account now.
func (a *Accounts) CurrentToken(accountID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tokenOf(accountID, a.accounts[accountID].token)
}

// Rotate gives the account a new token, as the broker's refresh does.
func (a *Accounts) Rotate(accountID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accounts[accountID].token++
}

// Empty makes the account's next n tokens empty, as a broker that vends
// nothing usable does.
func (a *Accounts) Empty(accountID string, n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.empties == nil {
		a.empties = map[string]int{}
	}
	a.empties[accountID] = n
}

// Fail makes Token fail with err for the account, as a broker that can't
// reach it does; nil ends that.
func (a *Accounts) Fail(accountID string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failures == nil {
		a.failures = map[string]error{}
	}
	a.failures[accountID] = err
}

// Break makes Token refuse the account for reason, a ports.Account* reason.
func (a *Accounts) Break(accountID, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accounts[accountID].broken = reason
}
