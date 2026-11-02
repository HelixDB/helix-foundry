package syncer

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

var (
	accountTokens = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_connector_account_tokens_total",
		Help: "Tokens asked of the provider broker for signed-in connectors' accounts, by outcome (vended, broken, unavailable, " +
			"failed) and, for broken, its reason.",
	}, []string{"outcome", "reason"})
	accountBreaks = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "helix_foundry_connector_account_breaks_total",
		Help: "Signed-in connectors' accounts a sync or the weekly check found broken, by reason.",
	}, []string{"reason"})
)

// CountAccountBreak counts an account a sync or the weekly check found
// broken, by reason.
func CountAccountBreak(reason string) { accountBreaks.WithLabelValues(reason).Inc() }

// accountChecks records the tokens the broker vends.
type accountChecks interface {
	MarkConnectorAccountChecked(ctx context.Context, workspaceID, connectorID string) error
}

// Credentials opens a connector's credential: the key sealed in its row, or,
// for a connector with an account (connected by signing in), a token its
// host's broker vends, asked for at each use and held in memory only, never
// stored, logged or returned. Every sync, and the API's re-test of new
// settings, opens one here.
type Credentials struct {
	Keys *secretbox.Keyring
	// Accounts is the broker; nil where no connector signs in.
	Accounts ports.ProviderAccounts
	// Checked records each token the broker vends; nil records none.
	Checked accountChecks
}

// Source is the connector's stored settings with its credential, Source.SignIn
// set for a vended token. Its errors are Token's, for a connector with an
// account.
func (c Credentials) Source(ctx context.Context, conn *model.Connector) (connector.Source, error) {
	src := connector.Source{Settings: conn.Config, WorkspaceID: conn.WorkspaceID, ConnectorID: conn.ID}
	if conn.Account == nil {
		secret, err := c.Keys.Open(conn.SecretCiphertext, conn.SecretKeyID, connector.SecretContext(conn.ID))
		if err != nil {
			return src, fmt.Errorf("open connector %s credential: %w", conn.ID, err)
		}
		src.Secret = string(secret)
		return src, nil
	}
	credential, err := c.Token(ctx, conn)
	if err != nil {
		return src, err
	}
	src.Secret, src.SignIn = credential.Value, true
	return src, nil
}

// Token asks the broker for the token of a connector's account, as the
// member it acts as. Foundry's record of a break decides first: a broken
// account is a *ports.AccountBroken with its reason, and the broker isn't
// asked. Without a broker the error is ports.ErrAccountsUnavailable, which,
// like the broker's own, is retried and never breaks the account. An empty
// token is one no provider takes: a *connector.AuthError, as though the
// provider had rejected it, so no kind ever reads with one.
func (c Credentials) Token(ctx context.Context, conn *model.Connector) (ports.Credential, error) {
	a := conn.Account
	switch {
	case a == nil:
		return ports.Credential{}, fmt.Errorf("connector %s has no account", conn.ID)
	case a.Broken != nil:
		return ports.Credential{}, &ports.AccountBroken{Reason: *a.Broken}
	case c.Accounts == nil:
		accountTokens.WithLabelValues("unavailable", "").Inc()
		return ports.Credential{}, fmt.Errorf("connector %s signs in, and nothing here vends tokens: %w", conn.ID, ports.ErrAccountsUnavailable)
	}
	credential, err := c.Accounts.Token(ctx, ports.AccountRef{WorkspaceID: conn.WorkspaceID, UserID: a.UserID, Kind: conn.Kind, AccountID: a.ID})
	var broken *ports.AccountBroken
	switch {
	case errors.As(err, &broken):
		accountTokens.WithLabelValues("broken", broken.Reason).Inc()
		return ports.Credential{}, err
	case errors.Is(err, ports.ErrAccountsUnavailable):
		accountTokens.WithLabelValues("unavailable", "").Inc()
		return ports.Credential{}, err
	case err != nil:
		accountTokens.WithLabelValues("failed", "").Inc()
		return ports.Credential{}, err
	case strings.TrimSpace(credential.Value) == "":
		accountTokens.WithLabelValues("empty", "").Inc()
		return ports.Credential{}, &connector.AuthError{Provider: conn.Kind}
	}
	accountTokens.WithLabelValues("vended", "").Inc()
	if c.Checked != nil {
		if err := c.Checked.MarkConnectorAccountChecked(ctx, conn.WorkspaceID, conn.ID); err != nil {
			log.From(ctx).Warn("record a vended token failed", "connector", conn.ID, "err", err)
		}
	}
	return credential, nil
}

// AccountBrokenMessage is what a signed-in connector of the kind display
// names says once its account broke for reason (a ports.Account* reason):
//
//	GitHub needs signing in again: the member who connected it left the
//	workspace. Its data is frozen as of its last sync. An owner or admin
//	signs it in again from its page.
func AccountBrokenMessage(display, reason string) string {
	why := "its account needs attention"
	switch reason {
	case ports.AccountMemberLeft:
		why = "the member who connected it left the workspace"
	case ports.AccountAccessRevoked:
		why = display + " no longer accepts the access it signed in with"
	case ports.AccountExpired:
		why = "its access lapsed and couldn't be renewed"
	case ports.AccountAccountMissing:
		why = "the account it signed in with is gone"
	}
	return display + " needs signing in again: " + why +
		". Its data is frozen as of its last sync. An owner or admin signs it in again from its page."
}
