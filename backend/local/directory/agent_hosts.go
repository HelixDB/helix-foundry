package directory

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// The helpers paired with the install (migrations/00003_agent_hosts.sql):
// processes on the owner's machine that run the Assistant's turns on the
// Claude Code or Codex CLI the owner signed in to. Each of their secrets is
// random, shown once, and kept only as its SHA-256:
//
//   - hxp_, a pairing code the owner makes in Foundry: it pairs one helper,
//     once, within PairingTTL;
//   - hxh_, a helper's token: it claims turns and reports on them, nothing
//     else, until the helper is revoked;
//   - hxr_, a claimed turn's token: its CLI calls Foundry's tools with it, in
//     the turn's workspace, while the claim's lease lives, a bounded number
//     of times.

const (
	// PairingTTL is how long a pairing code pairs.
	PairingTTL = 10 * time.Minute
	// maxHostName bounds a helper's name, version and platform, in bytes.
	maxHostName  = 128
	maxHostLabel = 64
)

// The secrets' prefixes.
const (
	PairingPrefix   = "hxp_"
	HostTokenPrefix = "hxh_"
	TurnTokenPrefix = "hxr_"
)

// ErrTurnCallsSpent and ErrTurnAsksSpent refuse a turn token's call past its
// turn's calls, or its Analyst questions.
var (
	ErrTurnCallsSpent = errors.New("the turn made all the tool calls it may")
	ErrTurnAsksSpent  = errors.New("the turn asked all the questions it may")
)

// ErrInvalidAgentHost is a helper's name, version, platform or runtimes
// that the table can't hold.
var ErrInvalidAgentHost = errors.New("invalid helper")

// AgentTurnToken is an hxr_ token as stored: its helper, its turn and the
// claim it was minted for, when it stops working (ExpiresAt, with the
// claim's lease) and when the turn's time runs out (Deadline), and what it
// has spent.
type AgentTurnToken struct {
	HostID, TurnID, WorkspaceID, LeaseOwner string
	ExpiresAt, Deadline                     time.Time
	Calls, Asks                             int
}

// newSecret is prefix and 32 random bytes, unpadded base64url.
func newSecret(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// tokenHash is how a secret is kept: its SHA-256, in hex.
func tokenHash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// hostRuntimes is a SET column's runtimes; unknown members are dropped.
func hostRuntimes(set string) []model.FoundryAgentRuntime {
	var out []model.FoundryAgentRuntime
	for _, r := range strings.Split(set, ",") {
		if runtime := model.FoundryAgentRuntime(r); runtime.Subscription() {
			out = append(out, runtime)
		}
	}
	return out
}

// runtimeSet is runtimes as a SET column takes them: helpers' runtimes only,
// each once, in order.
func runtimeSet(runtimes []model.FoundryAgentRuntime) (string, error) {
	var names []string
	for _, r := range runtimes {
		if !r.Subscription() {
			return "", fmt.Errorf("%w: a helper offers claude_code or codex, not %q", ErrInvalidAgentHost, r)
		}
		if !slices.Contains(names, string(r)) {
			names = append(names, string(r))
		}
	}
	slices.Sort(names)
	return strings.Join(names, ","), nil
}

// validLabel is a helper's name, version or platform: printable, bounded.
func validLabel(s string, maxBytes int) bool {
	return len(s) <= maxBytes && !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

const agentHostColumns = `id, name, runtimes, accounts, version, platform, created_at, last_seen_at, revoked_at`

func scanAgentHost(row scanner) (agent.Host, error) {
	var h agent.Host
	var runtimes string
	var accounts []byte
	var revoked sql.NullTime
	if err := row.Scan(&h.ID, &h.Name, &runtimes, &accounts, &h.Version, &h.Platform, &h.CreatedAt, &h.LastSeenAt, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return agent.Host{}, store.ErrNotFound
		}
		return agent.Host{}, err
	}
	h.Runtimes = hostRuntimes(runtimes)
	// Written by SeeAgentHost alone, checked; NULL until the helper's first
	// call after pairing.
	if len(accounts) > 0 {
		if err := json.Unmarshal(accounts, &h.Accounts); err != nil {
			return agent.Host{}, fmt.Errorf("helper %s's accounts: %w", h.ID, err)
		}
	}
	if revoked.Valid {
		h.RevokedAt = &revoked.Time
	}
	return h, nil
}

// CreateAgentPairing makes a pairing code, valid for PairingTTL, and forgets
// codes that expired a day ago.
func (s *Store) CreateAgentPairing(ctx context.Context) (string, time.Time, error) {
	code, err := newSecret(PairingPrefix)
	if err != nil {
		return "", time.Time{}, err
	}
	now := time.Now().UTC()
	expires := now.Add(PairingTTL)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_local_agent_pairings (code_hash, expires_at) VALUES (?, ?)`,
		tokenHash(code), expires); err != nil {
		return "", time.Time{}, err
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM foundry_local_agent_pairings WHERE expires_at < ?`, now.Add(-24*time.Hour))
	return code, expires, nil
}

// PairAgentHost spends a pairing code on a new helper and returns it with its
// hxh_ token. A code that is unknown, used or expired is ErrNotFound, and
// pairs nothing.
func (s *Store) PairAgentHost(ctx context.Context, code string, h agent.Host) (agent.Host, string, error) {
	runtimes, err := runtimeSet(h.Runtimes)
	switch {
	case err != nil:
		return agent.Host{}, "", err
	case strings.TrimSpace(h.Name) == "" || !validLabel(h.Name, maxHostName) || !validLabel(h.Version, maxHostLabel) || !validLabel(h.Platform, maxHostLabel):
		return agent.Host{}, "", fmt.Errorf("%w: its name is 1 to %d bytes, its version and platform at most %d, printable", ErrInvalidAgentHost, maxHostName, maxHostLabel)
	case !strings.HasPrefix(code, PairingPrefix):
		return agent.Host{}, "", store.ErrNotFound
	}
	token, err := newSecret(HostTokenPrefix)
	if err != nil {
		return agent.Host{}, "", err
	}
	h.ID = id.New()
	err = inTx(ctx, s.db, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		res, err := tx.ExecContext(ctx, `UPDATE foundry_local_agent_pairings SET used_at=? WHERE code_hash=? AND used_at IS NULL AND expires_at > ?`,
			now, tokenHash(code), now)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return cmp.Or(err, store.ErrNotFound)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO foundry_local_agent_hosts (id, name, token_hash, runtimes, version, platform, created_at, last_seen_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, h.ID, strings.TrimSpace(h.Name), tokenHash(token), runtimes, h.Version, h.Platform, now, now)
		return err
	})
	if err != nil {
		return agent.Host{}, "", err
	}
	paired, err := s.AgentHost(ctx, h.ID)
	return paired, token, err
}

// AgentHost returns a helper, revoked or not.
func (s *Store) AgentHost(ctx context.Context, id string) (agent.Host, error) {
	return scanAgentHost(s.db.QueryRowContext(ctx, `SELECT `+agentHostColumns+` FROM foundry_local_agent_hosts WHERE id=?`, id))
}

// AgentHostByToken is the live helper whose hxh_ token this is: ErrNotFound
// for any other secret, or a revoked helper's.
func (s *Store) AgentHostByToken(ctx context.Context, token string) (agent.Host, error) {
	if !strings.HasPrefix(token, HostTokenPrefix) {
		return agent.Host{}, store.ErrNotFound
	}
	return scanAgentHost(s.db.QueryRowContext(ctx, `SELECT `+agentHostColumns+`
FROM foundry_local_agent_hosts WHERE token_hash=? AND revoked_at IS NULL`, tokenHash(token)))
}

// SeeAgentHost records a live helper's call, what it offers now and what
// their CLIs are signed in to (agent.ValidHostAccounts), which replace what
// its last call said.
func (s *Store) SeeAgentHost(ctx context.Context, id string, runtimes []model.FoundryAgentRuntime, version string, accounts []agent.HostAccount) error {
	set, err := runtimeSet(runtimes)
	if err != nil {
		return err
	}
	if !validLabel(version, maxHostLabel) {
		return fmt.Errorf("%w: its version is at most %d printable bytes", ErrInvalidAgentHost, maxHostLabel)
	}
	if err := agent.ValidHostAccounts(accounts, runtimes); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidAgentHost, err)
	}
	if accounts == nil {
		accounts = []agent.HostAccount{}
	}
	// A string, not bytes: MySQL takes no JSON from a binary string.
	encoded, err := json.Marshal(accounts)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE foundry_local_agent_hosts SET last_seen_at=?, runtimes=?, version=?, accounts=? WHERE id=? AND revoked_at IS NULL`,
		time.Now().UTC(), set, version, string(encoded), id)
	return err
}

// TouchAgentHost records a live helper's call that says nothing of what it
// offers: a turn's report.
func (s *Store) TouchAgentHost(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_local_agent_hosts SET last_seen_at=? WHERE id=? AND revoked_at IS NULL`, time.Now().UTC(), id)
	return err
}

// ListAgentHosts returns the helpers not revoked, newest first.
func (s *Store) ListAgentHosts(ctx context.Context) ([]agent.Host, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+agentHostColumns+`
FROM foundry_local_agent_hosts WHERE revoked_at IS NULL ORDER BY created_at DESC, id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agent.Host
	for rows.Next() {
		h, err := scanAgentHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// RevokeAgentHost unpairs a helper and deletes its turns' tokens, so neither
// its hxh_ nor any hxr_ it holds works again. Revoking it again returns it.
func (s *Store) RevokeAgentHost(ctx context.Context, id string) (agent.Host, error) {
	err := inTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE foundry_local_agent_hosts SET revoked_at=? WHERE id=? AND revoked_at IS NULL`,
			time.Now().UTC(), id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM foundry_local_agent_turn_tokens WHERE host_id=?`, id)
		return err
	})
	if err != nil {
		return agent.Host{}, err
	}
	return s.AgentHost(ctx, id)
}

// AgentRuntimeOnline reports whether a live helper offering runtime called
// within HostOnlineFor.
func (s *Store) AgentRuntimeOnline(ctx context.Context, runtime model.FoundryAgentRuntime) (bool, error) {
	if !runtime.Subscription() {
		return false, fmt.Errorf("no helper runs %q", runtime)
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM foundry_local_agent_hosts
WHERE revoked_at IS NULL AND last_seen_at > ? AND FIND_IN_SET(?, runtimes) > 0`, time.Now().UTC().Add(-agent.HostOnlineFor), string(runtime)).Scan(&n)
	return n > 0, err
}

// AgentRuntime is where the workspace's new threads run: api unless the
// owner chose a helper's.
func (s *Store) AgentRuntime(ctx context.Context, workspaceID string) (model.FoundryAgentRuntime, error) {
	var runtime string
	err := s.db.QueryRowContext(ctx, `SELECT runtime FROM foundry_local_agent_settings WHERE workspace_id=?`, workspaceID).Scan(&runtime)
	if errors.Is(err, sql.ErrNoRows) {
		return model.FoundryAgentRuntimeAPI, nil
	}
	return model.FoundryAgentRuntime(runtime), err
}

var _ agent.HostRegistry = (*Store)(nil)

// SetAgentRuntime sets where the workspace's new threads run.
func (s *Store) SetAgentRuntime(ctx context.Context, workspaceID string, runtime model.FoundryAgentRuntime) error {
	if !runtime.Valid() {
		return fmt.Errorf("no runtime %q", runtime)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO foundry_local_agent_settings (workspace_id, runtime) VALUES (?, ?)
ON DUPLICATE KEY UPDATE runtime=VALUES(runtime)`, workspaceID, string(runtime))
	return err
}

// CreateAgentTurnToken mints the hxr_ token of a turn a live helper claimed,
// under its claim's lease owner, working until expires.
func (s *Store) CreateAgentTurnToken(ctx context.Context, t AgentTurnToken) (string, error) {
	if t.HostID == "" || t.TurnID == "" || t.WorkspaceID == "" || t.LeaseOwner == "" || t.ExpiresAt.IsZero() || t.Deadline.IsZero() {
		return "", fmt.Errorf("a turn token names its helper, turn, workspace, claim, expiry and deadline")
	}
	token, err := newSecret(TurnTokenPrefix)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO foundry_local_agent_turn_tokens
  (token_hash, host_id, turn_id, workspace_id, lease_owner, expires_at, deadline) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		tokenHash(token), t.HostID, t.TurnID, t.WorkspaceID, t.LeaseOwner, t.ExpiresAt.UTC(), t.Deadline.UTC()); err != nil {
		return "", err
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM foundry_local_agent_turn_tokens WHERE expires_at < ?`, now.Add(-time.Hour))
	return token, nil
}

const agentTurnTokenColumns = `t.host_id, t.turn_id, t.workspace_id, t.lease_owner, t.expires_at, t.deadline, t.calls, t.asks`

func scanAgentTurnToken(row scanner) (AgentTurnToken, error) {
	var t AgentTurnToken
	err := row.Scan(&t.HostID, &t.TurnID, &t.WorkspaceID, &t.LeaseOwner, &t.ExpiresAt, &t.Deadline, &t.Calls, &t.Asks)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentTurnToken{}, store.ErrNotFound
	}
	return t, err
}

// AgentTurnToken is the live hxr_ token this is: ErrNotFound for any other
// secret, one expired, or a revoked helper's.
func (s *Store) AgentTurnToken(ctx context.Context, token string) (AgentTurnToken, error) {
	if !strings.HasPrefix(token, TurnTokenPrefix) {
		return AgentTurnToken{}, store.ErrNotFound
	}
	return scanAgentTurnToken(s.db.QueryRowContext(ctx, `SELECT `+agentTurnTokenColumns+`
FROM foundry_local_agent_turn_tokens t JOIN foundry_local_agent_hosts h ON h.id = t.host_id AND h.revoked_at IS NULL
WHERE t.token_hash=? AND t.expires_at > ?`, tokenHash(token), time.Now().UTC()))
}

// AgentTurnClaim is the live claim of turnID that the helper hostID holds:
// its newest token's row. ErrNotFound when it holds none.
func (s *Store) AgentTurnClaim(ctx context.Context, hostID, turnID string) (AgentTurnToken, error) {
	return scanAgentTurnToken(s.db.QueryRowContext(ctx, `SELECT `+agentTurnTokenColumns+`
FROM foundry_local_agent_turn_tokens t JOIN foundry_local_agent_hosts h ON h.id = t.host_id AND h.revoked_at IS NULL
WHERE t.host_id=? AND t.turn_id=? AND t.expires_at > ? ORDER BY t.expires_at DESC LIMIT 1`, hostID, turnID, time.Now().UTC()))
}

// ExtendAgentTurnTokens moves the expiry of a claim's tokens with its lease.
func (s *Store) ExtendAgentTurnTokens(ctx context.Context, turnID, leaseOwner string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE foundry_local_agent_turn_tokens SET expires_at=? WHERE turn_id=? AND lease_owner=? AND expires_at > ?`,
		expires.UTC(), turnID, leaseOwner, time.Now().UTC())
	return err
}

// SpendAgentTurnCall counts one call of a live hxr_ token, and an Analyst
// question when ask: past maxCalls it is ErrTurnCallsSpent, past maxAsks
// ErrTurnAsksSpent, and nothing is counted. A token that stopped working is
// ErrNotFound.
func (s *Store) SpendAgentTurnCall(ctx context.Context, token string, ask bool, maxCalls, maxAsks int) error {
	asks := 0
	if ask {
		asks = 1
	}
	return inTx(ctx, s.db, func(tx *sql.Tx) error {
		var calls, asked int
		err := tx.QueryRowContext(ctx, `SELECT t.calls, t.asks
FROM foundry_local_agent_turn_tokens t JOIN foundry_local_agent_hosts h ON h.id = t.host_id AND h.revoked_at IS NULL
WHERE t.token_hash=? AND t.expires_at > ? FOR UPDATE`, tokenHash(token), time.Now().UTC()).Scan(&calls, &asked)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return store.ErrNotFound
		case err != nil:
			return err
		case calls >= maxCalls:
			return ErrTurnCallsSpent
		case asked+asks > maxAsks:
			return ErrTurnAsksSpent
		}
		_, err = tx.ExecContext(ctx, `UPDATE foundry_local_agent_turn_tokens SET calls=calls+1, asks=asks+? WHERE token_hash=?`, asks, tokenHash(token))
		return err
	})
}

// EndAgentTurnTokens deletes a turn's tokens: its CLI calls nothing more.
func (s *Store) EndAgentTurnTokens(ctx context.Context, turnID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM foundry_local_agent_turn_tokens WHERE turn_id=?`, turnID)
	return err
}
