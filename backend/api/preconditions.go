package api

import (
	"crypto/sha256"
	"encoding/hex"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// billingDomain names the refusals a workspace's plan decides, which the
// site routes by reason (FOUNDRY_PRO_REQUIRED: Settings › Billing).
const billingDomain = "billing.helix-db.com"

// billingPrecondition is a refusal the workspace's plan decides, named by reason.
func billingPrecondition(workspaceID, reason, msg string) error {
	st := status.New(codes.FailedPrecondition, msg)
	detail, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason:   reason,
		Domain:   billingDomain,
		Metadata: map[string]string{"workspace_id": workspaceID},
	})
	if err != nil {
		return st.Err()
	}
	return detail.Err()
}

// hashToken is a token's SHA-256 in lowercase hex: what is stored and looked up.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
