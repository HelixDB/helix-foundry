package directory

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/pkg/id"
	"github.com/helixdb/helix-foundry/backend/ports"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Confirmations persists local MCP confirmations, separately from hosted migrations.
type Confirmations struct{ Store *Store }

// errConfirmationMismatch is a confirmation that isn't the one this call needs: FailedPrecondition,
// as hosted answers, so the agent keeps its message and prepares again. PermissionDenied or
// NotFound would reach it as a bare not_found, which reads as a wrong ontology or workspace id.
var errConfirmationMismatch = status.Error(codes.FailedPrecondition, "confirmation does not match; prepare it again with the same operation, target and payload")

func confirmationOwner(ctx context.Context) (string, error) {
	a, ok := ports.ActorFrom(ctx)
	if !ok || a.UserID == "" {
		return "", status.Error(codes.Unauthenticated, "owner authentication required")
	}
	return a.UserID, nil
}

func (c Confirmations) Prepare(ctx context.Context, b integration.ConfirmationBinding) (integration.Confirmation, error) {
	owner, err := confirmationOwner(ctx)
	if err != nil {
		return integration.Confirmation{}, err
	}
	if b.Operation == "" || len(b.Operation) > 128 || b.Target == "" || len(b.Target) > 255 || b.Audience == "" || len(b.Audience) > 255 || len(b.PayloadHash) != 64 {
		return integration.Confirmation{}, status.Error(codes.InvalidArgument, "invalid confirmation binding")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return integration.Confirmation{}, err
	}
	out := integration.Confirmation{ID: id.New(), Token: "hlxc_" + hex.EncodeToString(secret), ExpiresAt: time.Now().UTC().Add(5 * time.Minute)}
	hash := sha256.Sum256([]byte(out.Token))
	_, err = c.Store.db.ExecContext(ctx, `INSERT INTO foundry_local_mcp_confirmations
 (id, token_hash, owner_id, operation, target, audience, payload_hash, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		out.ID, hex.EncodeToString(hash[:]), owner, b.Operation, b.Target, b.Audience, b.PayloadHash, out.ExpiresAt)
	if err != nil {
		return integration.Confirmation{}, err
	}
	// Bound retention independently of process restarts; no live confirmation is purged.
	_, _ = c.Store.db.ExecContext(ctx, `DELETE FROM foundry_local_mcp_confirmations WHERE expires_at < ?`, time.Now().UTC().Add(-24*time.Hour))
	return out, nil
}

func (c Confirmations) Consume(ctx context.Context, b integration.ConfirmationBinding) error {
	owner, err := confirmationOwner(ctx)
	if err != nil {
		return err
	}
	tx, err := c.Store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var hash, boundOwner, operation, target, audience, payload string
	var expires time.Time
	var consumed sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT token_hash, owner_id, operation, target, audience, payload_hash, expires_at, consumed_at
 FROM foundry_local_mcp_confirmations WHERE id = ? FOR UPDATE`, b.ID).Scan(&hash, &boundOwner, &operation, &target, &audience, &payload, &expires, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return errConfirmationMismatch
	}
	if err != nil {
		return err
	}
	got := sha256.Sum256([]byte(b.Token))
	if subtle.ConstantTimeCompare([]byte(hash), []byte(hex.EncodeToString(got[:]))) != 1 || boundOwner != owner || operation != b.Operation || target != b.Target || audience != b.Audience || payload != b.PayloadHash {
		return errConfirmationMismatch
	}
	if consumed.Valid {
		return status.Error(codes.AlreadyExists, "confirmation is already consumed; read the operation's state before trying again")
	}
	if !time.Now().UTC().Before(expires) {
		return status.Error(codes.FailedPrecondition, "confirmation is expired")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE foundry_local_mcp_confirmations SET consumed_at = ? WHERE id = ?`, time.Now().UTC(), b.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// OntologyWorkspace is used only after owner authentication. The normal API
// authorization still checks the resolved workspace and ontology on each call.
func (s *Store) OntologyWorkspace(ctx context.Context, ontologyID string) (string, error) {
	var workspaceID string
	err := s.db.QueryRowContext(ctx, `SELECT workspace_id FROM foundry_ontologies WHERE id = ? AND deleted_at IS NULL`, ontologyID).Scan(&workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", status.Error(codes.NotFound, "ontology not found")
	}
	return workspaceID, err
}
