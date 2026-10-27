package sendtoken_test

import (
	"context"
	"fmt"
	"time"

	"github.com/helixdb/helix-foundry/backend/sendtoken"
)

// A door admits a request's token, sends as its sender, and tells the admission when the
// backend refuses the send with PERMISSION_DENIED: the token was revoked since it was found live.
func Example() {
	const token = "hx_ev_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	live := map[string]sendtoken.Sender{
		sendtoken.Hash(token): {TokenID: "token-1", WorkspaceID: "workspace-1", ConnectorID: "source-1"},
	}
	// The HTTP door caches a token found live for 30 s, as the backend checks it on every send.
	admission := sendtoken.New(func(_ context.Context, hash string) (sendtoken.Sender, bool, error) {
		sender, ok := live[hash]
		return sender, ok, nil
	}, 30*time.Second)
	ctx, now := context.Background(), time.Now()

	sent := admission.Admit(ctx, "203.0.113.9", token, now)
	fmt.Println(sent.Verdict == sendtoken.Admitted, sent.ConnectorID)

	// The token is revoked. The cache still admits it, and the backend refuses the send.
	delete(live, sendtoken.Hash(token))
	fmt.Println(admission.Admit(ctx, "203.0.113.9", token, now.Add(time.Second)).Verdict == sendtoken.Admitted)
	admission.Revoked("203.0.113.9", token, now.Add(time.Second))

	// Its next request is looked up again, and refused.
	fmt.Println(admission.Admit(ctx, "203.0.113.9", token, now.Add(2*time.Second)).Verdict == sendtoken.Refused)

	// A request without a token spends a failure, as a guess does.
	fmt.Println(admission.Unauthenticated("203.0.113.9", now).Verdict == sendtoken.Refused)

	// A door that makes room for a request first prescreens it: Prescreen refuses unlooked what
	// Admit would, and leaves the rest to Admit once the door has room for them.
	screened, decided := admission.Prescreen("203.0.113.9", "hx_ev_guess", now)
	fmt.Println(decided, screened.Verdict == sendtoken.Refused)
	_, decided = admission.Prescreen("203.0.113.9", token, now)
	fmt.Println(decided)
	// Output:
	// true source-1
	// true
	// true
	// true
	// true true
	// false
}
