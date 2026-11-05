# Connecting sources by signing in

A user, or the agent onboarding them, connects GitHub or Linear by signing in at the provider instead of pasting a key.

- The credential lives in WorkOS Pipes, owned by the workspace's WorkOS organization.
- Foundry stores which Pipes account a connector uses, and asks Pipes for a fresh token each time it reads the provider.
- No credential passes through an agent, the chat or Foundry's database.
- Hosted only: a local build has no WorkOS and keeps keys.

The proof of concept on branch `poc/workos-pipes` (`backend/cmd/pipes-poc`, and its README's results) ran this flow against WorkOS staging and GitHub. This plan builds it into Foundry's backend module, hosted Helix (the WFE and the MCP) and the site. It was reviewed for edge cases against all three codebases. What the proof of concept never tried is marked "verify on dev".

**Scope.**

- In: every kind Pipes can sign in to.
  - GitHub, through a read-only GitHub App.
  - Linear, Slack, Notion, PostHog, Cal.com, Stripe and Google Workspace, through OAuth apps.
  - Accounts are owned by the organization, and Pipes is used for OAuth only.
  - Each kind is offered only once hosted lists it in `pipes.kinds` and its Pipes integration exists ("One-time setup"). The code for all of them ships together and stays dormant until then. Stripe (a Stripe Connect platform) and Google (app verification) need a decision before they are switched on.
- Out:
  - kinds Pipes has no OAuth for: Granola, WorkOS, PostgreSQL, MySQL and S3, whose IAM role already stores no key;
  - API sources;
  - accounts owned by one user;
  - moving existing key connectors over;
  - the local build;
  - email (see "When an account breaks").
- Keys stay in Foundry.

**Before production:** Pipes' price.

**Status: final, 2 November 2026.** It is built as "Implementation" lays out, in five pull requests across helix-foundry and helix-hyperscale. "Not in v1" lists what was left out on purpose.

## The flow

1. The agent asks "Connect GitHub?" and the user says yes. The agent may also ask which repositories to include.
2. The agent calls `helix_foundry_start_sign_in`, which answers with a link to Foundry: `/data?add=github&sign_in=<id>&workspace=<ws>`.
3. The agent opens the link in the user's browser, or gives it to the user. Foundry shows GitHub's form, bound to that sign-in, with the workspace, name and settings, and one button: **Connect with GitHub**.
4. GitHub asks the user to authorize Helix Foundry, and the user clicks Authorize. GitHub returns to Pipes, which returns to Foundry's `/connect/callback`.
5. The callback finishes the sign-in. The backend:
   - saves the Pipes account and checks it is this workspace's, made by this user;
   - creates the connector;
   - schedules its first sync.
6. The agent's `helix_foundry_get_sign_in` wait sees `connected`, with the connector's id, and the agent follows the first sync as it does today.

In the Data area, step 3 is the user choosing GitHub in Add data; steps 4 to 6 are the same.

```mermaid
sequenceDiagram
    participant A as Agent (MCP)
    participant F as Foundry API
    participant S as Site (browser)
    participant P as WorkOS Pipes
    participant G as GitHub
    A->>F: StartConnectorSignIn {kind, name, config}
    F-->>A: sign-in (pending); MCP returns its link
    S->>F: AuthorizeConnectorSignIn {return_to}
    F->>P: POST /data-integrations/github/authorize (organization, PKCE challenge)
    F-->>S: provider URL
    S->>G: Authorize Helix Foundry
    G->>P: code
    P->>S: /connect/callback?connect_code=…
    S->>F: FinishConnectorSignIn {connect_code}
    F->>P: POST /data-integrations/finish (verifier)
    F->>P: POST /data-integrations/github/credentials
    F->>G: Prepare, identity, Discover
    F-->>S: connected (connector id); first sync scheduled
    A->>F: GetConnectorSignIn (wait)
```

## Decisions

**Accounts belong to the organization.**

- Every Pipes call uses `connection_owner: organization` with the workspace's `workos_organization_id`.
- Pipes requires a `user_id` who is an active member of the organization:
  - Foundry asks as the member Pipes says made the account, stored as the connector's `account_user_id`;
  - scheduled syncs, which act as no one today, ask as that member too.
- A Helix member without a WorkOS membership of the organization can't sign in, and is told why.
- When the account's member leaves the workspace, Pipes ends the account (the POC showed this live). The connector stops syncing and keeps its data until an owner or admin signs it in again (see "When an account breaks").

**The credential stays in Pipes.**

- Foundry never stores the token, its hint or its expiry.
- It asks for one before each use: a sync, a settings re-test, or a weekly check.
- It keeps the token in memory only, as `connector.Source.Secret`, with `Source.SignIn` set.
- Pipes refreshes expiring tokens. A GitHub App's user token lasts 8 hours; Linear's lasts 24 hours.

**One sign-in makes one connector.**

- Finishing a sign-in creates the connector from the kind, name and settings chosen before the user signed in (for GitHub, the repositories and review history).
- A sign-in naming a `connector_id` signs that connector in again, and creates nothing.

**A sign-in is its starter's, and ends once.**

- Only the signed-in user who started a sign-in can authorize and finish it, so a forwarded link fails for anyone else.
- Authorize may be called again, for a fresh provider URL, until the sign-in connects. This covers retrying after a decline or a failure, which keeps the agent's wait on the same sign-in.
- One PKCE verifier serves every Authorize of a sign-in.
- Finish moves the sign-in from `AUTHORIZING` to `FINISHING` exactly once (compare-and-set). Any repeat returns the sign-in as it stands, so a refreshed callback, a double effect or a retry can't spend a second code or undo a connection.

**The callback is the site's.**

- Pipes requires `return_to` to match a registered redirect URI exactly, with no query string. So the site keeps the sign-in's id in `sessionStorage` (the tab), and the callback reads it there.
- The verifier is made and kept, sealed, by the backend.
- The connect code is single-use and expires 90 seconds after the redirect, according to Pipes' reference (verify on dev).

**Foundry's record of a break decides.**

- Once Foundry marks an account broken, nothing syncs it: not the schedule, not Sync now, not a sync that started before.
- Only signing in again clears it.
- Each sign-in bumps the connector's `account_generation`, and a sync breaks an account only if it holds the generation it asked with. So a late failure can't break an account that was just repaired.

**Only the account's member widens what it reads.**

- A signed-in connector reads with one member's provider access. GitHub's reach is everything that member can see, where Helix Foundry is installed.
- The GitHub App is shared by every Foundry customer. If another company installed it on its organization, that company's repositories are within reach of any of its members who belong to this workspace too.
- So adding repositories, or clearing the list (which means "all"), is allowed only for `account_user_id`. Other owners and admins may narrow the list, rename, change the schedule, or sign the connector in again as themselves.
- The connector's page says whose access it reads with.

**A port, not a dependency.**

- The module defines `ports.ProviderAccounts`, and hosted Helix implements it on Pipes' REST API. hyperscale's `workos-go` v4.46.1 has no Pipes support.
- A local build leaves the port nil, so no kind offers sign-in there.
- If Pipes is ever replaced, only this implementation changes.

**Starting a sign-in needs no confirmation.**

- MCP writes go through `helix_prepare_foundry_operation`/`helix_execute_foundry_operation`, whose confirmation is "not human approval" (`plans/FOUNDRY_MCP.md:215-219`, hyperscale).
- A sign-in writes nothing that matters until the user clicks Authorize. The form shows the workspace, name and settings, and never submits on load. So `helix_foundry_start_sign_in` is a plain tool, annotated as a write.

**A GitHub App, not an OAuth app.**

- It reads metadata, issues and pull requests, nothing else. An OAuth app's `repo` scope reads and writes every repository.
- The cost: it reads a private repository only where it is installed, so an organization's admin installs Helix Foundry, on top of the user's Authorize. Foundry links to the install page.

**GitHub reads where the app is installed.** With no repositories named, a signed-in GitHub connector reads the repositories of the app's installations the user can reach. It doesn't read every repository the user can see, which would include public ones they only contribute to.

**Keys stay.** GitHub and Linear keep "Use a token instead". Existing key connectors are untouched, and every other kind keeps `helix_foundry_connector_link`.

## Backend module (`helix-foundry/backend`)

### The port (`ports/ports.go`)

```go
// ProviderAccounts keeps the accounts users connect by signing in at a
// provider, owned by their workspace (hosted: WorkOS Pipes). Nil when no
// kind connects that way, as in a local build.
type ProviderAccounts interface {
	// Kinds are the connector kinds userID can connect to the workspace by
	// signing in: none when they can't (hosted: no WorkOS organization, no
	// WorkOS membership of it, or Pipes switched off).
	Kinds(ctx context.Context, workspaceID, userID string) ([]string, error)
	// Authorize is the provider's URL for the user to authorize a. With
	// a.AccountID set, an account the broker no longer has is an
	// *AccountBroken{account_missing}: Foundry authorizes again for a new
	// account, and only once that succeeds records the old one as missing
	// (a broker may answer so for a switched-off integration too).
	Authorize(ctx context.Context, a AccountAuthorization) (string, error)
	// Finish saves the account the user authorized, having checked it is the
	// workspace's and was made by ref.UserID; otherwise it deletes it and
	// fails (ErrAccountNotThisWorkspace), returning the refused account with
	// the error so Foundry purges it too. ref.AccountID is set when the
	// sign-in reauthorized the connector's own account: Finish never deletes
	// that one. A lapsed, reused or mismatched code is ErrAccountCodeRejected.
	Finish(ctx context.Context, ref AccountRef, code, verifier string) (ProviderAccount, error)
	// Token is the account's current credential, asked for as ref.UserID.
	Token(ctx context.Context, ref AccountRef) (Credential, error)
	// Delete removes the account; an account already gone is no error.
	Delete(ctx context.Context, ref AccountRef) error
}

type AccountAuthorization struct {
	WorkspaceID, UserID, Kind, Label string
	ReturnTo, Challenge              string // S256
	// AccountID, when set, reauthorizes that account. Foundry sets it only
	// when UserID is the member the account acts as: an account can't move
	// to another member, so anyone else signing in again makes a new one.
	AccountID string
}

type AccountRef struct{ WorkspaceID, UserID, Kind, AccountID string }

// ProviderAccount is a saved account and the Helix user it acts as.
type ProviderAccount struct{ ID, UserID string }

type Credential struct {
	Value     string
	ExpiresAt *time.Time
}

// AccountBroken is Token's error when the account needs signing in again.
type AccountBroken struct {
	Reason string // member_left, access_revoked, expired, account_missing
}

// ErrAccountsUnavailable is any other failure to reach the accounts:
// switched off, unreachable, rate-limited. It is retried, never a break.
var ErrAccountsUnavailable = errors.New("signing in to providers is unavailable")

// ErrAccountCodeRejected: Finish's code lapsed, was used, or its verifier
// doesn't match. ErrAccountNotThisWorkspace: the account isn't the
// workspace's, or wasn't made by the user finishing.
var ErrAccountCodeRejected, ErrAccountNotThisWorkspace error
```

- **Wiring:** the port goes in `api.Deps` (`api/deps.go:22`) as `Accounts`, and in the sync worker's `Deps` (`workers/foundry-sync-worker/task.go:38-49`) as `optional:"true"`, passed to `syncer.Syncer`.
- **Store methods hosts call:** list `BreakConnectorAccounts` in the `ports.go` header, beside the others.
- **Errors:** every port error is typed. Pipes' response bodies go to logs only, never into a gRPC status, the site or an agent.

### Proto (`pb/proto/helix/foundry/v1`)

New RPCs in `FoundryService` (`foundry.proto`), all under `/v1/workspaces/{workspace_id}/foundry`:

| RPC                        | HTTP                                        | Request → response                                                                                                                                    |
| -------------------------- | ------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| `StartConnectorSignIn`     | `post …/sign-ins` (body `*`)                | `kind`, `name`, `config` (settings: a credential field set is `INVALID_ARGUMENT`), `connector_id` (sign in again), `client_token` → `ConnectorSignIn` |
| `GetConnectorSignIn`       | `get …/sign-ins/{id}`                       | → `ConnectorSignIn`                                                                                                                                   |
| `AuthorizeConnectorSignIn` | `post …/sign-ins/{id}:authorize` (body `*`) | `return_to` → `url`                                                                                                                                   |
| `FinishConnectorSignIn`    | `post …/sign-ins/{id}:finish` (body `*`)    | `connect_code`, or `error` (a callback without a code) → `ConnectorSignIn`                                                                            |

**New and changed messages** (`types.proto`):

- **`ConnectorSignIn`** has these fields:
  - `id`, `kind`, `name`;
  - `status`: `CONNECTOR_SIGN_IN_STATUS_{PENDING,AUTHORIZING,FINISHING,CONNECTED,DECLINED,FAILED,EXPIRED}`;
  - `failure`, a fixed code that the site and agents word themselves: `declined`, `provider_error`, `code_expired`, `not_this_workspace`, `token_rejected`, `settings_invalid`;
  - `connector_id`;
  - `discovery_problem`, Foundry's own words when the connector was made but can't read what was named, such as a repository the app can't see;
  - `identity_changed`, set when signing in again as a different provider account;
  - `mine`, true when the caller started it;
  - `created_at`, `expires_at`.

  No provider text is returned.

- **`Connector`** gains `ConnectorAccount account = 13`, with these fields:
  - `label`, the provider identity (a GitHub login; a Linear user and workspace);
  - `state`: `CONNECTOR_ACCOUNT_STATE_{CONNECTED,NEEDS_SIGN_IN}`;
  - `broken`: `CONNECTOR_ACCOUNT_BROKEN_{MEMBER_LEFT,ACCESS_REVOKED,EXPIRED,ACCOUNT_MISSING}`, and `broken_at`;
  - `user_id` and `connected_by`, the member's display name, or "a former member" once they have left;
  - `connected_at`.

  A connector with an account has an empty `secret_hint`.

- **`ConnectorKind`** gains:
  - `bool sign_in = 7`;
  - `GitHubConnectorSetup github = 8`, in the existing `oneof setup` beside `s3`, holding `install_url`.
- **`ListConnectorsRequest`** gains `needs_sign_in`. When it is set, the response holds only connectors whose account needs signing in, without tables, syncs or plan reads. This is the cheap read for the banner, on the same route, so the allowlist needs no new entry.

Regenerate with `buf generate && buf generate --template buf.internal.gen.yaml`, then run `pnpm contract:sync` on the site.

### Store

**Migration** `<version>_foundry_connector_sign_ins.sql`. Its version must be later than Foundry's newest (`20261112000000`) and hosted's (`20261030000000`); check both again at merge. It does three things:

- **adds to `connectors`:**
  - `account_id VARCHAR(64) NULL`;
  - `account_user_id CHAR(26) NULL`;
  - `account_label VARCHAR(255) NULL`;
  - `account_generation INT NOT NULL DEFAULT 0`;
  - `account_broken VARCHAR(32) NULL` (a reason; NULL means connected), and `account_broken_at DATETIME(6) NULL`;
  - `account_checked_at DATETIME(6) NULL`, when Pipes last vended for it.

  A connector with an account seals an empty secret, as secretless kinds do (`api/connectors.go:111`).

- **creates `foundry_connector_sign_ins`:**
  - `id CHAR(26)` PK, `workspace_id`, `kind`, `name`, `config JSON`;
  - `connector_id CHAR(26) NULL`, `status VARCHAR(16)`, `failure VARCHAR(32) NULL`;
  - `discovery_problem VARCHAR(512) NULL`, `identity_changed BOOL NOT NULL DEFAULT 0`;
  - `verifier_ciphertext VARBINARY(256) NULL`, `verifier_key_id VARCHAR(32) NULL`;
  - `account_id VARCHAR(64) NULL`, written as soon as Pipes saves the account;
  - `created_by_user_id CHAR(26) NOT NULL`, `client_token VARCHAR(64) NULL`;
  - `created_at`, `updated_at`, `expires_at`;
  - indexes: unique (`workspace_id`, `created_by_user_id`, `client_token`), and (`workspace_id`, `created_at`).
- **creates `foundry_connector_account_purges`:** Pipes accounts to delete, with:
  - `workspace_id`, `kind`, `account_id`;
  - `attempts`, `last_error`, `created_at`.

  Rows are written in the same transaction as whatever lets an account go:
  - a connector's delete;
  - `DeleteWorkspaceConnectors`;
  - a sign-in that ends without using its account.

**Queries** go in a new `store/connector_sign_ins.go` (hand-written, like `store/connectors.go`), with the model in `store/model`. Every query is keyed by (`workspace_id`, `id`). `connectorColumns` and `scanConnector` (`store/connectors.go:23`, `:377`) gain the account columns.

**Scheduling:**

- `QueueDueConnectorSyncs` (`store/connectors.go:249`) skips connectors with `account_broken` set.
- `FinishConnectorSync`'s schedule write (`store/connector_syncs.go:164-168`) gains `AND c.account_broken IS NULL`, so a sync that ends after a break can't un-pause it.

**`BreakConnectorAccounts(ctx, workspaceID, userID, reason)`**, for every connector of the workspace whose `account_user_id` is `userID` and isn't already broken:

- sets `account_broken` and `account_broken_at`;
- sets `next_sync_at = NULL`;
- cancels its queued syncs.

**`breakAccount(ctx, connectorID, generation, reason)`** does the same for one connector, only while `account_generation` still equals `generation`.

**Cleanup:** the sync worker's schedule loop (`task.go:501`):

- deletes a sign-in row a day after it ends, once its `account_id` is used or purged;
- works through the purges with backoff, deleting each one that `Accounts.Delete` confirms;
- does the weekly check (see Sync).

### API (`api/sign_ins.go`; changes in `api/connectors.go`)

**Every sign-in RPC** needs an actor with a `UserID`. `AuthorizeConnectorSignIn` and `FinishConnectorSignIn` also need `SourceUser`.

**Start**

- Refuses with `FAILED_PRECONDITION`, reason `SIGN_IN_UNAVAILABLE`, when either holds:
  - `Accounts` is nil;
  - the kind isn't in `Accounts.Kinds(ws, actor)`.
- Then `requireKind` (`api/plan.go:144`).
- Checks the settings as `Prepare` does with `Source.SignIn` set and no secret (GitHub: repository names, at most 100).
- With `connector_id`:
  - the connector must be of the same kind and have an account;
  - a live sign-in for it by the same user is returned;
  - one by another user is `FAILED_PRECONDITION` `SIGN_IN_IN_PROGRESS`, naming them.
- **Caps:** at most 5 live sign-ins per user per workspace, and 20 starts an hour per user. Past either: `RESOURCE_EXHAUSTED`.
- Writes a `PENDING` row that expires in 15 minutes. A `client_token` seen before returns the same sign-in.

**Authorize**

- **Refusals:**
  - not the actor's sign-in: `FAILED_PRECONDITION` `SIGN_IN_NOT_YOURS`;
  - `CONNECTED`, `FINISHING` or `EXPIRED`: `SIGN_IN_ENDED`;
  - `return_to` not in `foundry.connectors.sign_in_return_to`.
- **Retry:** `PENDING`, `AUTHORIZING`, `DECLINED` and `FAILED` may authorize, so retrying keeps the same id.
- The first Authorize makes the verifier and seals it with the connector keyring (context `sign-in:<id>`). Later ones reuse it.
- **Calls `Accounts.Authorize`:**
  - intent `add` for a new connector;
  - `reauthorize` with the account only when the caller is the account's `account_user_id` and it isn't broken `member_left` or `account_missing`. Pipes can't move an account to another member, and Finish refuses an account another user made, so a reauthorize by anyone else would cost the connector its account;
  - `add` for everyone else signing in again. Once it connects, the old account goes to the purges.
  - A reauthorize answered `AccountBroken{account_missing}` is retried once with `add`. Only when that succeeds is `account_missing` recorded at the connector's generation; if it fails, its error is the answer and nothing is recorded, since Pipes answers a switched-off integration the same way.
- Sets `AUTHORIZING`, moves `expires_at` to 15 minutes from now, and answers with the URL.

**Finish**

- The actor's sign-in only.
- **Status:**
  - `FINISHING`, `CONNECTED`, `DECLINED`, `FAILED` and `EXPIRED` return the sign-in as it is, without calling Pipes;
  - `AUTHORIZING` is claimed with a compare-and-set to `FINISHING`.

  Only the claimant may end it.

- **Without a code:** `error=access_denied` is `DECLINED`; anything else is `FAILED` `provider_error`.
- **Otherwise** it runs under `context.WithoutCancel` with its own 60-second timeout for the provider calls, so a closed tab or a 504 at `/papi` doesn't stop it halfway. Each store write that records how it ended, the compensating delete and the final read get a fresh 10-second context of their own, so a slow provider can't leave the outcome unrecorded. A sign-in still `FINISHING` after 5 minutes (a crashed finish) reads `FAILED` `unavailable`, may authorize again, and its account is purged.
  1. `Accounts.Finish`, with `ref.AccountID` set when it reauthorized. Write `account_id` on the row at once; if that write fails, delete the account unless it is the one the connector reads with. A refused account that Finish returns with its error goes to the purges in the transaction that ends the sign-in, unless a live connector reads with it.
  2. `Accounts.Token`.
  3. `Prepare` with `Source.SignIn`. A refusal is `FAILED` `settings_invalid`.
  4. The provider identity (`connector.Identifier`, below), for `account_label`. When signing in again to a different identity, set `identity_changed`.
  5. `Discover`. Its error is kept as `discovery_problem` and doesn't stop the sign-in; the connector page and the agent say what to do.
  6. One transaction:
     - refuses a workspace that is being deleted (`store.Workspaces.Deleting`);
     - creates the connector with `NextSyncAt = now`, as `CreateConnector` does (`api/connectors.go:103-108`).

     Or, when signing in again, it locks the connector row and refuses if it was deleted. Then it updates:
     - `account_id`, `account_user_id` (Pipes' maker), `account_label`;
     - `account_generation + 1`;
     - clears `account_broken`;
     - `next_sync_at = now`, as a full sync when the identity changed.

     Finally it marks the sign-in `CONNECTED`, with `connector_id`.

- **When any step after `Accounts.Finish` fails,** the account goes to the purges, unless it is a connector's existing account being signed in again, and the sign-in ends `FAILED`.

**Get**

- Reads the row for any member with `workspace.connectors.manage`, with `mine`.
- A sign-in past `expires_at` reads `EXPIRED`, unless it is `FINISHING` or `CONNECTED`.

**Changes to existing RPCs:**

- **`ListConnectorKinds`** (`:30`): `sign_in` from `Accounts.Kinds(ws, actor)`; GitHub's `install_url` from `foundry.connectors.github_app_install_url`.
- **`ListConnectors`:** the `needs_sign_in` filter.
- **`connectorToProto`** (`:561`): `account`.
- **`UpdateConnector`** (`:161`), on a connector with an account:
  - a credential field is `FAILED_PRECONDITION` ("signs in with GitHub: sign it in again instead");
  - adding repositories, or clearing them, by anyone but `account_user_id` is `FAILED_PRECONDITION` `ACCOUNT_OWNER_ONLY`;
  - a re-test asks Pipes for the token. It still seals `""` and sets an empty hint: the vended token must never reach `:215-216`.
- **`SyncConnector`** (`api/connector_syncs.go:30`): a broken account is `FAILED_PRECONDITION` `ACCOUNT_NEEDS_SIGN_IN`.
- **`DeleteConnector`** (`:274`): purges its account in the delete's transaction.
- **`credential(ctx, c)`:** one helper that opens the sealed secret or asks `Accounts.Token`, setting `Source.SignIn`. Used by `UpdateConnector` and `Syncer.Run` (`syncer/syncer.go:206`); GitHub and Linear never reach `releaseOld` or `Syncer.Release`.

### Sync

**Getting the credential.** `Syncer.Run` gets it through `credential(ctx, c)`:

- **A connector with an account and no port,** or `ErrAccountsUnavailable`: a refusal (`RefusedError`), retried with backoff. Never a permanent failure, which would pause the connector for good.
- **`AccountBroken`:** `breakAccount` with the generation it asked with, then a permanent failure (`syncer.classify`, `syncer.go:1036`). Its error reads: "GitHub needs signing in again: the member who connected it left the workspace. Its data is frozen as of its last sync. An owner or admin signs it in again from its page."
- **A broken account** is refused before asking Pipes, with the same words.

**When the provider rejects a vended token.**

- The connectors return a typed `connector.AuthError` for a 401, or Linear's `AUTHENTICATION_ERROR`, in place of today's "check it and enter it again" refusal.
- For a connector with an account, the syncer asks Pipes again once and retries. Pipes refreshes an expired token, which covers a token that ran out mid-sync; `Credential.ExpiresAt` is logged.
- A second rejection is `AccountBroken{access_revoked}`.

**A sync that outlives a sign-in again.** A sync records the `account_generation` it read with. Its cursor write and `FinishConnectorSync`'s schedule writes apply only while that generation is current. Otherwise the cursor is dropped, `schedule_paused` is left alone, and `next_sync_at` is set to now. A sync of the old account can't then undo the new sign-in's full read, or pause a connector that was just signed in again. A key connector stays at generation 0.

**Weekly check.**

- The schedule loop asks Pipes for every connector with an account not vended in 7 days, paused or manual-only ones included.
- A check that gets no token defers that account by an hour (`DeferConnectorAccountCheck`), and the loop moves on. The tick stops early only after 3 unavailable answers in a row, so one unreachable account can't hold back every other.
- This keeps GitHub's refresh token (6 months) from lapsing, and catches accounts that broke outside Helix: a membership removed in WorkOS directly, a grant revoked at the provider.

The token is never logged.

### Connectors

- **`Source.SignIn`.**
  - GitHub and Linear skip their key-format checks (`github.go:85-88`, `linear.go:87-89`) for a vended token.
  - Linear sends it as `Bearer` whatever it looks like. Linear's OAuth tokens may carry no `lin_oauth_` prefix, so the POC branch's prefix rule (`linear.go:17`, `:178-183`) gives way to the flag.
  - Their refusals say "sign it in again", not "enter it again".
- **`connector.Identifier`**, an optional interface, `Identity(ctx, src) (string, error)`:
  - GitHub: `/user`'s login;
  - Linear: `viewer`'s name and its organization's name.
- **GitHub, signed in, with no repositories named.** It reads the repositories of the app's installations the user can reach (`/user/installations`, then each installation's repositories). It doesn't read `/user/repos`, which includes public repositories the user only contributes to. With no installation, discovery says to install the app.
- **GitHub, a named repository the app can't see.** The message names the install link, and says installing the app and then **Sync now** resumes it. An organization uninstalling or suspending the app reads the same way: it isn't a break, because signing in again wouldn't help.

#### Every kind that signs in

Each kind keeps its connector code and tables. A vended token replaces the key: `Source.SignIn` skips the key-format check and sends the token as the kind already sends its key. The kind's settings stay in its form, without the credential field. Every row's token shape, expiry and identity call is "verify on dev".

| Kind      | Pipes slug                                     | Signed in, it reads                                                                                                                                                                                                        | Identity (`connector.Identifier`)                | Widens reach (`ACCOUNT_OWNER_ONLY`)   |
| --------- | ---------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------ | ------------------------------------- |
| `github`  | `github`                                       | the app's installations the user can reach                                                                                                                                                                                 | `/user`'s login                                  | adding repositories, or clearing them |
| `linear`  | `linear`                                       | the workspace the user authorized                                                                                                                                                                                          | `viewer`'s name and organization                 | nothing                               |
| `slack`   | `slack` (the bot provider, never `slack-user`) | the channels the bot is in, as today. A vended user token (`xoxp-`) is refused `settings_invalid`, as a pasted one is                                                                                                      | `auth.test`'s team and bot user                  | nothing: `history_days` only          |
| `notion`  | `notion`                                       | the pages the user shared at Notion's consent screen. A new sign-in chooses them again                                                                                                                                     | `/v1/users/me`'s bot owner: the workspace's name | nothing: no settings                  |
| `posthog` | `posthog`                                      | PostHog US Cloud only: Pipes' `posthog` provider has no region setting (verify on dev). EU Cloud and self-hosted keep keys: refused `settings_invalid`                                                                     | `/api/users/@me`'s email and organization        | changing `project_id`                 |
| `calcom`  | `cal-dot-com`                                  | the user's Cal.com account, through `/v2`                                                                                                                                                                                  | `/v2/me`'s username                              | nothing: no settings                  |
| `stripe`  | `stripe`                                       | the connected account, with a `read_only` grant. `mode` comes from the vended key's prefix, as from a pasted one                                                                                                           | `/v1/account`'s display name                     | nothing                               |
| `google`  | `google`                                       | as the signed-in user: their Drive and Calendar, and the directory only for an admin (otherwise that table is unavailable, a discovery problem, not a failure). It skips the service-account exchange and has no `subject` | Drive's `about.user.emailAddress`                | nothing: `event_days` only            |

## Hosted Helix (`helix-hyperscale/backend`)

**Pipes client: `internal/workos/pipes.go`**, beside the custom HTTP calls in `internal/workos/client.go`. It ports the POC's `pipes.go`. Its calls:

- `POST /data-integrations/{slug}/authorize`;
- `POST /data-integrations/finish`;
- `POST /data-integrations/{slug}/credentials`;
- `GET /organizations/{org}/connected_accounts/{slug}?connected_account_id=…`, for `state` and `state_reason`, which a vend doesn't return;
- `DELETE /organizations/{org}/connected_accounts/{slug}?supports_multiple_connections=true&connected_account_id=…`.

It has a key of its own, `pipes.api_key`: the Foundry application's key, so the key that can vend every organization's tokens can be narrowed and rotated alone. Its errors are typed, and response bodies go to logs only. A 429 or 5xx is `ErrAccountsUnavailable`.

`internal/workos/pipestest` is the POC's `fake.go`, which answers as WorkOS did live.

**The port: `internal/foundryhost/accounts.go`, `ProviderAccounts{DB, Pipes, Slugs}`.**

- **`Kinds`:** the configured kinds when all three hold:
  - `pipes.enabled` is on;
  - the workspace has a `workos_organization_id` (not staff workspaces or unclaimed agent sandboxes);
  - the user's `workspace_members` row has a `WorkosMembershipID`.
- **`Authorize`** maps the workspace to its organization, and the user to `users.workos_user_id`. It sends:
  - `connection_owner: organization`;
  - `connection_intent: add`, or `reauthorize` with `connected_account_id`;
  - `account_display_name`: the connector's name, a label for WorkOS's dashboard only;
  - `code_challenge`.
- **`Finish`** calls Pipes' finish, then checks three things:
  - `organization_id` is the workspace's;
  - `established_by_user_id` is the actor's WorkOS user;
  - the state is `connected`.

  On any mismatch it deletes the account (never `ref.AccountID`) and returns it with `ErrAccountNotThisWorkspace`. A 400 is `ErrAccountCodeRejected`. Otherwise it returns `ref.UserID`, the user `established_by_user_id` was just checked against.

- **`Authorize`** requires Pipes' `completion` to be exactly `finish_required`, so Foundry's checks and the PKCE binding can't be skipped. A 404 on a reauthorize is `AccountBroken{account_missing}`.

- **`Token`** vends with `supports_multiple_connections: true`, as `ref.UserID`. Answers map as follows:
  - **active:** the value and `expires_at`;
  - **inactive:** read the account's `state_reason`:
    - `establishing_member_membership_ended` is `member_left`;
    - a failed refresh is `expired`;
    - any other is `access_revoked`;
  - **`not_installed`** (Pipes has no such account): `account_missing`;
  - **a 403** carrying exactly "User does not have access to the specified organization": `member_left`. Any other 403 (a narrowed key, say) is unavailable and counted as an unknown answer, so it can't break every connector;
  - **an `error` value it doesn't know:** `ErrAccountsUnavailable`, with an alert.
- **`Delete`:** a 404 is done.

**Member removal.**

- `DeleteWorkspaceMember` (`server/service/backend/members.go:184-235`) calls `tx.Foundry().BreakConnectorAccounts(ctx, workspaceID, userID, "member_left")` inside its transaction, as `workspace.go:539` calls `CancelWorkspaceFoundryWork`.
- The hydrator's delete of stale memberships (`internal/auth/hydrator.go:380-389`) does the same.

**Workspace deletion.** `deleteConnectors` (`server/tasks/provision-worker/handlers/workspace_delete.go:110`) deletes every connector's Pipes account through the port before `DeleteWorkspaceConnectors`, retrying like the other steps. The WorkOS organization isn't deleted today, so without this a deleted workspace's grants would stay vendable.

**Config:**

- `pipes.enabled`, which gates `Kinds` only, so connectors that already have accounts keep syncing;
- `pipes.api_key`;
- `pipes.kinds`: Foundry kind to Pipes slug (`github: github`, `linear: linear`, `slack: slack`, `notion: notion`, `posthog: posthog`, `calcom: cal-dot-com`, `stripe: stripe`, `google: google`). Each stamp lists only the kinds whose integration exists; dev and prod start with `github` and `linear`;
- Foundry's `foundry.connectors.sign_in_return_to` and `foundry.connectors.github_app_install_url`, per stamp.

**Wiring:**

- `foundryapi.Deps{…, Accounts}` in `server/service/backend/service.go:229`.
- A `provideFoundryAccounts` in `server/deps.go` for the sync worker.
- Bump the module in `go.mod`. hosted pins `20261028042522-ff6459497611`, so the bump also brings in everything Foundry merged since, including the notifications migration: deploy it as one unit.

**WFE authorization:** `internal/auth/authz/registry.go:105-109` gives the four RPCs `PermissionWorkspaceConnectorsManage` with `FromWorkspaceID`, like `CreateConnector`.

### MCP (helix-foundry `backend/integration`)

The MCP's Foundry tools moved into the module's `backend/integration` package (helix-foundry #212 and #236) while #220 was in review; hosted's `mcpinsights` registers them from there. The sign-in tools are in `integration/foundry_sign_ins.go`.

| Tool                          | Does                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     | RPCs                                                       |
| ----------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------- |
| `helix_foundry_start_sign_in` | `{workspace_id, kind, name?, repos?, project_id?, connector_id?, client_token?}` → `{sign_in_id, url, expires_at, next}`. `repos` is GitHub's and `project_id` PostHog's (US Cloud only); every other setting takes the kind's default, which the user changes on the connector's page. `url` is `foundryURL + /data?add=<kind>&sign_in=<id>&workspace=<ws>`. `next`: open it in the user's browser (macOS `open`, Linux `xdg-open`), or give it to them, then call `helix_foundry_get_sign_in` with `wait`. It refuses `repos` for any kind but GitHub and `project_id` for any kind but PostHog, and, with `connector_id` (signing a connector in again), any name or setting.                         | `ListConnectorKinds`, `StartConnectorSignIn`               |
| `helix_foundry_get_sign_in`   | `{workspace_id, sign_in_id, wait}`. Waits up to 25 s (`waitWhile` in `integration/foundry.go`) while the sign-in is pending, authorizing or finishing. Returns `sign_in` (with `discovery_problem` and `identity_changed`), GitHub's `install_url` on every answer, and `next`. Connected: the connector, its `url` and `last_sync`; `next` says to follow the sync, and names the install link when there is a discovery problem. Declined or failed: `next` says to ask whether to try again; the same link works until it expires, except after `settings_invalid`, which needs a new sign-in. Expired: no `url`; `next` says to start again (with `connector_id` when signing a connector in again). | `GetConnectorSignIn`, `GetConnector`, `ListConnectorKinds` |

**Registration and access:**

- Registered for users with `workspace.connectors.manage`, and only when the host offers sign-in (`integration.Service.SignIn`, which hosted sets from `pipes.enabled`; a local install leaves it off) and names the website (`FoundryURL`, hosted's `mcp.foundry_url`): `signInTools`. Service keys, agent registrations and send tokens get neither tool.
- Checked with `AuthorizeFoundry(…, UserOnly, …)`. Keys get neither tool.
- `start_sign_in` is annotated as a write.
- Outputs carry `failure` codes and Foundry's own words, never provider text: query strings and labels are data an attacker can write.

**Changes to existing tools:**

- `helix_foundry_list_connector_kinds` returns `sign_in`.
- `helix_foundry_list_connectors` returns each connector's `account` (its `state`, `needs_sign_in` when broken, and `broken`), never the provider's account label. The MCP has no `needs_sign_in` filter; that is the site's.
- `sync_connector` on a broken account returns the MCP's code `account_needs_sign_in` with the backend's message. The backend's `SIGN_IN_UNAVAILABLE` and `SIGN_IN_IN_PROGRESS` reach the agent as `failed_precondition` with the backend's message, and the prompt sends the agent to the connector link, never to asking for a token.
- `helix_foundry_connector_link`'s `next`, for a kind with `sign_in`, points at `helix_foundry_start_sign_in`.

**Docs:**

- hyperscale's `plans/FOUNDRY_MCP.md`: its tools table, and the rule at `:189-190`.
- hyperscale's `backend/docs/identity/credentials.md` (`:189`, `:210`).

## Site (`helix-foundry`)

**Contract and plumbing.**

- `pnpm contract:sync`.
- **Types** in `src/shared/wfe.ts`, under "Data":
  - `ConnectorSignIn`, its status and failure;
  - `ConnectorAccount`;
  - `ConnectorKind.signIn` and `.github.installUrl`;
  - `ListConnectors`' `needsSignIn`.
- **Allowlist:** four entries in the Data section of `src/server/papi-allowlist.ts`, with a comment that connector sign-ins aren't the sign-in the file's rules forbid.
- **`areas/data/data-api.ts`:** `startSignIn`, `getSignIn`, `authorizeSignIn` and `finishSignIn`.

**The form (`areas/data/connection-form.tsx`).** For a kind with `signIn`:

- The credential field becomes a **Connect with GitHub** (or Linear) button, with "Use a personal access token instead" opening today's field.
- GitHub's Repositories and Review history stay above the button, and are sent as `config.github` without `token`.
- **The button** is disabled after one click. It does three things:
  1. `startSignIn`, unless the link named a sign-in;
  2. `authorizeSignIn` with the callback `new URL("/connect/callback", <origin of NEXT_PUBLIC_WORKOS_REDIRECT_URI>)`, so a preview deployment never sends an unregistered URI;
  3. writes `foundry.connect.v1` to `sessionStorage` (`{workspaceId, signInId, returnPath}`), then `location.assign(url)`, for an `https:` URL only.

**The agent's link.**

- `ConnectData` (`connect-data.tsx`) takes the link's `sign_in` and opens that kind's form bound to it. The form says "Your agent asked to connect GitHub to <workspace>", and shows the name and repositories from the sign-in.
- When `mine` is false, it says the link was made for someone else, before any click.
- **Every host of `ConnectData` passes `sign_in` on with `add`:**
  - `ToOnboarding` (`main.tsx:83`);
  - the Data overview (`overview.tsx:66`, `:327`);
  - the wizard (`new-ontology.tsx:187-196`), where `sign_in` sets `own` and is dropped from the URL like `add`.

**The callback, `/connect/callback`.**

- **Placement:** a signed-in route outside the onboarding redirect (`main.tsx`).
- **Before any API call:**
  - it moves `connect_code` (or `error`) into the `foundry.connect.v1` entry, with the time;
  - `history.replaceState` makes the URL `/connect/callback`, so the code never reaches history, or `/login?return_to` if the session ran out;
  - a ref guards against the double effect of `<StrictMode>`.
- **Finishing:**
  - it calls `finishSignIn` with the stored `workspaceId`, not the workspace last chosen in another tab;
  - after a transport error it polls `getSignIn` rather than starting again.
- **When connected,** it goes to the stored `returnPath`, run through `parseReturnPath` and refusing `/connect/`, with `&workspace=<stored>`. Each origin returns its own way:
  - **the wizard:** `/ontology/new?step=sources&connected=<connectorId>`. `connected` sets `own` and is added to the chosen connectors as `ConnectorPicker` does (`generate-parts.tsx:486`);
  - **Generate:** its picks are encoded in the path (`?goal=&connectors=&sets=`), with `connected` added;
  - **Data:** the connector's page.
- **Failures** are worded from `failure`. **Try again** authorizes the same sign-in, so the agent's wait follows it.
- **Edge cases:**
  - no `sessionStorage` entry (another tab or browser): the page says to open the link in the tab that started it;
  - a code more than 90 seconds old (a sign-in detour): the page says the sign-in took too long, with **Try again**.

**The connector's page (`connection.tsx`).**

- **Details** show "Reads with Ada Lovelace's GitHub access (ada-l)" in place of Credential (`:90`).
- **When `NEEDS_SIGN_IN`,** it shows the callout from "When an account breaks".
- **For GitHub,** it shows `discovery_problem` and the install guidance with `installUrl`.
- **Edit:**
  - has no credential field;
  - adding repositories is disabled for anyone but the account's member, with the reason.
- **After signing in again,** `identity_changed` is said: "Now reads with ada-other's access".

**Analytics.**

- `connector_sign_in_started` and `connector_sign_in_finished` join `AnalyticsEvents`, with properties `{kind, outcome}`. `connector_created` fires on success, as the form does today.
- `trackBeforeLeaving` sends the fixed path `/data`, since the button also runs on pages whose paths name ids (`analytics.ts:111-113`).

**Mock and docs.**

- **Mock:**
  - `mock/routes/sign-ins.ts` holds sign-ins in memory, cleared with `onReset`, with the backend's rules.
  - `/mock/connect` is the provider's consent page, with Authorize and Cancel, served like `/mock/checkout` (`handleBillingPage`, `server.ts:137`).
  - GitHub and Linear have `signIn: true`, except in local mode.
  - `/__mock/config` takes `{"signIn": false}` and `{"accountBroken": {"<connectorId>": "member_left"}}`.
- **Guides:** `connection-guides.tsx`'s GitHub and Linear guides describe signing in and the install step, with the token as the alternative.
- **`AGENTS.md`:** an entry in "Things to know", and the redirect URI next to the login callback's.

**Local build:** its backend has no accounts port, so no kind has `signIn` and nothing here shows.

## When an account breaks

**Reasons**, each with its own words:

- `member_left`: the member it acts as left the workspace.
- `access_revoked`: the provider rejected it twice, or Pipes says access was revoked.
- `expired`: Pipes couldn't refresh it, for example after six months unused.
- `account_missing`: Pipes no longer has it.

Pipes can't move an account to another member, so someone signs it in again.

**Frozen, not lost.**

- Syncing stops: the schedule, Sync now, and the MCP's `sync_connector` all refuse it.
- Its tables keep what the last sync read, and loads and Ask keep reading them.
- Nothing is deleted.
- Signing in again resumes with one sync.

**Found:**

- **at once**, when a member is removed through Helix, or when the hydrator finds a membership gone, inside the same transaction;
- **at the next sync or weekly check**, when the membership was removed in WorkOS directly, or the provider revokes access.

**What people see.**

- **Owners and admins**:
  - **A dialog** the first time they open Foundry after a break (once per break, per browser: `localStorage` `foundry.account-broken.seen.v1`):
    - "GitHub stopped syncing. Ada Lovelace, who connected it, left the workspace. Its data is frozen as of 2 Nov, 14:05."
    - Its buttons: **Sign in again** and **Later**.
  - **A banner** at the top of every page until it is fixed: the same words, with **Sign in again**. **Later** hides it for the session.
- **Members** see the banner without the button: "Ask an owner or admin to sign it in again."
- **Several broken connectors** share one dialog and one banner, which link to Data.
- **The connector itself:**
  - its page has a callout with the reason, when its data froze, and **Sign in again** for owners and admins;
  - its row in the Data explorer and the overview's Status column read "Needs signing in again";
  - the generate pickers and the Load dialog say its data is frozen as of its last sync.
- **The shell** reads `ListConnectors` with `needs_sign_in` (one cheap query, with a long `staleTime`), not the full connector list.

**Agents:**

- `helix_foundry_list_connectors` shows `account`.
- The onboarding prompt and skill tell the agent to say so, and to offer `helix_foundry_start_sign_in` with `connector_id`.

**Email: not in v1.** hosted runs no `foundry-notify-worker`, and its mailer only logs (`server/deps_billing.go:114-118`). When Foundry's email runs in hosted, `connector_needs_sign_in` needs these changes:

- a break id per break;
- one row per owner and admin: the unique key `(kind, subject_id)` becomes `(kind, subject_id, user_id)`;
- a `Discover…` step in the worker with `People` in its deps, and `People.Managers(ctx, workspaceID)` as a new port method;
- skipping connectors fixed or deleted before it sends;
- one email per workspace and break, not one per connector.

## Prompts and the skill

**`onboarding-prompt.ts`, "Connect a source".** For a kind whose `sign_in` is true:

1. Call `helix_foundry_start_sign_in`.
2. Open its `url` in my browser (`open` on macOS, `xdg-open` on Linux), or give it to me.
3. Call `helix_foundry_get_sign_in` with `wait` until it ends.
4. Then, by outcome:
   - connected: follow the first sync as now;
   - declined, failed or expired: ask whether to try again (the same link works until it expires);
   - for GitHub, if `discovery_problem` names a repository the app can't see: give me the install link.

**Other rules:**

- A connector that needs signing in again: say so, and offer the link.
- Other kinds keep `helix_foundry_connector_link`.
- The credentials rule ("Never ask me for credentials…") stays as it is.
- Test: `tests/unit/ontology-onboarding-prompt.test.ts`.

**The `helix-foundry-onboarding` skill** in HelixDB/skills gets the same change.

## One-time setup, per WorkOS environment

The dev stamp uses WorkOS staging; production uses WorkOS production.

1. **GitHub App** "Helix Foundry" (dev: "Helix Foundry (dev)"), owned by HelixDB's GitHub organization:
   - public, so any account can install it;
   - Metadata, Issues and Pull requests: read;
   - no webhook;
   - expiring user tokens on;
   - "Request user authorization during installation" off;
   - callback: the Pipes integration's `redirect_uri`;
   - setup URL: `https://<site>/data`.

   `githubapp.go`'s manifest is the template.

2. **Linear OAuth app**, in Helix's Linear workspace: public, scope `read`, callback the integration's `redirect_uri`.
   - **Each other kind**, when it is switched on, needs an OAuth app at its provider, owned by Helix, with its callback set to the integration's `redirect_uri`, and the read scopes its connector documents:
     - **Slack:** a distributable app with the bot scopes the connector's guide lists.
     - **Notion:** a public integration with read content.
     - **PostHog:** an OAuth app with read scopes.
     - **Cal.com:** an OAuth client, which Cal.com approves.
     - **Stripe:** a Connect platform with `read_only`.
     - **Google:** a verified OAuth client with `drive.readonly`, `calendar.readonly` and `admin.directory.user.readonly`. Drive's scope is restricted and needs Google's security assessment.
3. **Pipes integrations**, one per kind switched on (`github`, `linear`, then the others' slugs):
   - organization-owned;
   - custom credentials, set in the dashboard or with `PUT /data-integrations/{slug}/organization`.

   An OAuth integration needs credentials to be created, and is "not found" while disabled. These slugs then belong to Foundry: providers are environment-wide, so any other Helix product would share Foundry's app.

4. **Redirect URIs** on the Foundry application:
   - production: `https://foundry.helix-db.com/connect/callback`;
   - staging: `https://dev.foundry.helix-db.com/connect/callback` and `https://local.foundry.helix-db.com:3000/connect/callback`.
5. **Hosted config:**
   - `pipes.enabled`, `pipes.api_key`, `pipes.kinds`;
   - `foundry.connectors.sign_in_return_to`;
   - `foundry.connectors.github_app_install_url`.
6. **The POC's `round1.sh` and `round2.sh`** create and then delete `/data-integrations/github/organization`. Before staging has its integration, change them to refuse an environment that already has the slug, or run them only in a sandbox.

## States and errors

| What happens                                                 | Where                            | What the user or agent sees                                           |
| ------------------------------------------------------------ | -------------------------------- | --------------------------------------------------------------------- |
| No WorkOS organization, or the user has no WorkOS membership | kinds, Start                     | `sign_in` false for them; Start: `SIGN_IN_UNAVAILABLE`, saying why    |
| Someone else opens the link                                  | the form (`mine`), Authorize     | "This link was made for someone else: ask your agent for a new one"   |
| Another member is already signing the connector in again     | Start                            | `SIGN_IN_IN_PROGRESS`, naming them                                    |
| Too many sign-ins                                            | Start                            | `RESOURCE_EXHAUSTED`                                                  |
| 15 minutes pass after the last Authorize                     | Authorize, Finish, Get           | `EXPIRED`: "This sign-in ran out: start again"                        |
| The user declines                                            | callback, Finish                 | `DECLINED`; **Try again** on the same sign-in                         |
| The provider or Pipes returns an error                       | callback, Finish                 | `FAILED` `provider_error`; **Try again**                              |
| The connect code lapses (slow return, a sign-in detour)      | Finish                           | `FAILED` `code_expired`; **Try again**                                |
| The account isn't the workspace's or the user's              | Finish                           | `FAILED` `not_this_workspace`; the account is deleted                 |
| The callback is refreshed, double-run, or retried            | Finish                           | The sign-in as it stands; no second code is spent                     |
| A crash or timeout after Pipes saved the account             | Finish, then cleanup             | `account_id` is on the row; the purge deletes it                      |
| The workspace is deleted mid-sign-in                         | Finish                           | Refused; the account is purged                                        |
| A named repository isn't visible                             | Finish, first sync               | The connector exists; "install Helix Foundry on <org>, then Sync now" |
| An organization uninstalls or suspends the app               | sync                             | The same install message; not a break                                 |
| The member is removed through Helix                          | `DeleteWorkspaceMember`, at once | Frozen; dialog and banner; **Sign in again**                          |
| The membership is removed in WorkOS, or access is revoked    | next sync or weekly check        | The same, from then                                                   |
| A refresh token lapses, or Pipes loses the account           | next sync or weekly check        | The same, as `expired` or `account_missing`                           |
| Pipes is off, unreachable or rate-limited                    | each token                       | Retried; never a break                                                |
| A token runs out mid-sync                                    | the provider's 401               | One fresh token, then retried                                         |
| Someone other than the account's member widens the settings  | `UpdateConnector`                | `ACCOUNT_OWNER_ONLY`, naming the member                               |

## Security

- **Tokens:**
  - asked for per use, in the backend's memory;
  - never stored, hinted, logged, returned, or given to an agent;
  - `UpdateConnector` seals `""` for a connector with an account.
- **The sign-in id is not a capability:**
  - authorizing and finishing need the signed-in user who started it (`SourceUser`), with `workspace.connectors.manage`;
  - the verifier never leaves the backend;
  - Finish checks the account's organization and maker with Pipes, and deletes a mismatch;
  - the connect code works once.
- **The callback:**
  - removes the code from the URL before anything else;
  - follows only a stored, parsed in-app path;
  - assigns only `https:` URLs;
  - an attacker's code in a victim's browser fails PKCE, and finds no `sessionStorage` entry.
- **`return_to`** comes from a config list, and WorkOS also requires it to be registered.
- **Least privilege:**
  - the GitHub App reads metadata, issues and pull requests; Linear's scope is `read`;
  - only the account's member widens what a connector reads;
  - `pipes.api_key` is separate from the dashboard's key.
- **Abuse:**
  - caps on live sign-ins and starts per user;
  - WorkOS 429s are retried, not surfaced.
- **Provider text** (the query's error, labels) never reaches an agent: failures are fixed codes.
- **Letting go:** a deleted connector or workspace, and a failed sign-in, purge their Pipes accounts with retries.
- **Revoking at the provider is later work.** When it comes, revoke the token (GitHub `DELETE /applications/{client_id}/token`; Linear `POST /oauth/revoke`), never GitHub's grant, which is per user and app and would break that person's other connectors.
- **The rule stands:** credentials never pass through an agent.

## Observability

- **Metrics:**
  - sign-ins by kind and outcome;
  - token requests by outcome and reason;
  - breaks by reason;
  - the purge backlog and its oldest row;
  - Pipes latency and status.
- **Alerts:**
  - a Pipes `error` value we don't know;
  - a purge older than a day;
  - a rate of `ErrAccountsUnavailable`.
- **Logs:**
  - each Start, Authorize, Finish, break and purge, with workspace, user, kind and ids;
  - never a token, code or verifier.

## Implementation

Five pull requests, then a setup track, verification on dev, and production. Each is safe to merge alone: with no port, `pipes.enabled` off, or no `signIn`, nothing changes for anyone. The work was built as the five parts below (PR 1 to 5), then gathered by repository.

| Pull request          | Repository       | Branch (from `dev`/`main`)   | Holds                     | Needs                                              | Safe because                                                                                                           |
| --------------------- | ---------------- | ---------------------------- | ------------------------- | -------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------- |
| helix-foundry #220    | helix-foundry    | `sign-in-connectors-backend` | parts 1, 3 and 5's prompt | none                                               | Without a port, `sign_in` is false; every new screen is gated on `signIn`; the prompt falls back to the connector form |
| MCP tools             | helix-foundry    | `sign-in-mcp-tools`          | part 4's tools            | #220 merged                                        | `integration.Service.SignIn` is false until hosted sets it, so no tool registers and no description names them         |
| helix-hyperscale #584 | helix-hyperscale | `foundry-sign-in-connectors` | part 2                    | #220 merged, for the module bump                   | `pipes.enabled` stays off until setup is done                                                                          |
| Hosted switch         | helix-hyperscale | to come                      | part 4's switch           | #584 and the MCP tools merged, for the module bump | the MCP's tools register only where sign-in is offered                                                                 |
| Skills                | HelixDB/skills   | `foundry-sign-in-connectors` | part 5's skills           | the hosted PR deployed to dev                      | —                                                                                                                      |

**About the POC branch:**

- `poc/workos-pipes` stays as it is.
- PR 1 carries this plan; its Linear change gives way to `Source.SignIn`.
- PR 2 ports the POC's `pipes.go` and `fake.go`.
- `backend/cmd/pipes-poc` isn't merged.

### PR 1: backend module (helix-foundry)

- [ ] Proto:
  - the four RPCs, `ConnectorSignIn`, `ConnectorAccount`;
  - `ConnectorKind.sign_in` and `github` setup;
  - `ListConnectorsRequest.needs_sign_in`;
  - `pb/gen` regenerated with both templates.
- [ ] Migration and store:
  - the `connectors` columns, `foundry_connector_sign_ins` and `foundry_connector_account_purges`;
  - `store/connector_sign_ins.go`;
  - `BreakConnectorAccounts` and `breakAccount`;
  - the guards in `QueueDueConnectorSyncs` and `FinishConnectorSync`.
- [ ] Port: `ports.ProviderAccounts` and its types, `api.Deps.Accounts`, the sync worker's optional dependency, and a fake for tests.
- [ ] API:
  - `api/sign_ins.go` (Start, Authorize, Finish, Get);
  - the changes to `ListConnectorKinds`, `ListConnectors`, `connectorToProto`, `UpdateConnector`, `SyncConnector` and `DeleteConnector`;
  - `credential(ctx, c)`.
- [ ] Syncer and worker:
  - a token per sync;
  - `AccountBroken`;
  - no port as a refusal;
  - `AuthError` with one fresh token;
  - the weekly check;
  - the purge sweep;
  - sign-in cleanup.
- [ ] Connectors:
  - `Source.SignIn`, `connector.Identifier` and `connector.AuthError`;
  - GitHub's installation reads and install messages;
  - Linear's `Bearer`;
  - sign-in copy.
- [ ] Config: `foundry.connectors.sign_in_return_to` and `foundry.connectors.github_app_install_url`.

**Done when:**

- `gofmt -l .` prints nothing; `go vet`, `golangci-lint` and `go test ./...` (test databases up) pass;
- the cases in Testing › Backend module pass;
- with `Accounts` nil, every existing test passes unchanged and `sign_in` is false everywhere.

### PR 2: hosted (helix-hyperscale)

- [ ] `internal/workos/pipes.go` and `internal/workos/pipestest`.
- [ ] `internal/foundryhost/accounts.go`.
- [ ] Config:
  - `pipes.enabled`, `pipes.api_key` and `pipes.kinds`;
  - Foundry's two keys per stamp;
  - chart values and the secret for `pipes.api_key`.
- [ ] Wiring: `foundryapi.Deps.Accounts`, and `provideFoundryAccounts` for the sync worker.
- [ ] Registry entries for the four RPCs.
- [ ] `DeleteWorkspaceMember` and the hydrator call `BreakConnectorAccounts` inside their transactions.
- [ ] Workspace deletion deletes each connector's Pipes account.
- [ ] The module bump. Check the migrations it brings run in order with hosted's, and deploy them as one unit.

**Done when:**

- hosted's checks pass;
- the cases in Testing › Hosted pass;
- it is deployed to dev with the `helix-dev-deploy` skill, `pipes.enabled` on once setup is done.

### PR 3: site (helix-foundry)

- [ ] Contract and plumbing:
  - `pnpm contract:sync`;
  - types in `src/shared/wfe.ts`;
  - the four allowlist entries and their comment;
  - `data-api.ts`.
- [ ] The form:
  - **Connect with GitHub** / **Connect with Linear**;
  - "Use a personal access token instead";
  - settings above the button;
  - `mine`;
  - one click.
- [ ] `sign_in` passed on by every host of `ConnectData`: `ToOnboarding`, the Data overview, the wizard.
- [ ] `/connect/callback`:
  - storage and URL clean-up;
  - the `<StrictMode>` guard;
  - Finish, and polling after a transport error;
  - the return paths to the wizard, Generate and Data;
  - failures and **Try again**.
- [ ] The connector's page:
  - whose access it reads with;
  - `discovery_problem` and install guidance;
  - Edit's rules;
  - a changed identity.
- [ ] Broken accounts:
  - the shell's `needs_sign_in` query;
  - the dialog and banner;
  - the explorer's and overview's status;
  - the notes in the generate pickers and the Load dialog.
- [ ] Analytics events.
- [ ] Mock: sign-ins, `/mock/connect`, the config switches, and `signIn` false in local mode.
- [ ] Guides and `AGENTS.md`.

**Done when:**

- `pnpm typecheck`, `pnpm lint`, `pnpm test` and `pnpm e2e` pass, including `tests/e2e/sign-in.spec.ts` and the local spec;
- it may merge before PR 2.

### PR 4: MCP (helix-foundry `backend/integration`; hosted's switch)

The tools are the module's; hosted sets `integration.Service.SignIn` from `pipes.enabled` where `mcpinsights` builds the service, with a module bump.

- [ ] `helix_foundry_start_sign_in` and `helix_foundry_get_sign_in`:
  - registered only where sign-in is offered;
  - annotated as a write;
  - failure codes only, never provider text.
- [ ] Changes to existing tools:
  - `account` in `helix_foundry_list_connectors`;
  - `sign_in` in `helix_foundry_list_connector_kinds`;
  - the connector link's `next`.
- [ ] `plans/FOUNDRY_MCP.md` and `backend/docs/identity/credentials.md`.

**Done when:**

- the hermetic MCP tests pass;
- Claude Code against `mcp.dev.helix-db.com` connects GitHub from start to finish.

### PR 5: prompts and skill

- [ ] `onboarding-prompt.ts`: "Connect a source" and its rules, with `tests/unit/ontology-onboarding-prompt.test.ts`.
- [ ] The `helix-foundry-onboarding` skill in HelixDB/skills.

**Done when:** an agent given only the prompt connects GitHub on dev and follows its first sync.

### Setup in WorkOS staging (before verification)

This needs people with access to the HelixDB GitHub organization, Helix's Linear workspace and the WorkOS staging dashboard.

- [ ] Guard the POC scripts: refuse an environment that already has the slug.
- [ ] GitHub App "Helix Foundry (dev)", as in "One-time setup".
- [ ] Linear OAuth app (dev).
- [ ] Pipes integrations `github` and `linear` with the apps' credentials. Set each app's callback to its integration's `redirect_uri`.
- [ ] Redirect URIs on the Foundry application.
- [ ] The dev stamp's config and `pipes.api_key` secret.

### Verification on dev (before production)

- [ ] Every case in Testing › On dev, and the negative cases at real Pipes. Record the results in this plan.
- [ ] Fix whatever they contradict, and answer the open questions they settle.

### Production

- [ ] Pipes' price settled.
- [ ] The production GitHub App and Linear app, integrations, redirect URIs, config and secret.
- [ ] Enable it on the production stamp, then watch the Observability metrics for a week.

## Testing

**Backend module:**

- store tests with the test databases up, including:
  - the scheduling guards;
  - generation-checked breaks;
  - purges;
- API tests with a fake `ProviderAccounts`:
  - start, authorize and finish;
  - authorizing again after a decline or failure;
  - expiry moving with Authorize;
  - someone else's sign-in;
  - a second Finish, during and after the first (one Pipes call);
  - a code-less callback;
  - a failure after Pipes saved the account (it is purged);
  - a deleted workspace;
  - signing in again, by the same and another member;
  - `SIGN_IN_IN_PROGRESS`;
  - the caps;
  - a `client_token` replay;
  - `UpdateConnector` sealing `""` and refusing to widen;
- syncer tests:
  - a token asked for on every sync;
  - no port means retried, not paused;
  - `AccountBroken` freezes;
  - a late failure on an older generation doesn't break;
  - a success after a break doesn't un-pause;
  - two provider rejections break it, while one fresh token recovers;
  - the weekly check;
- connector tests:
  - Linear sends `Bearer` for any vended token;
  - GitHub reads installation repositories;
- the module's checks: `gofmt`, `go vet`, `golangci-lint` and `go test ./...`.

**Hosted:**

- the Pipes client against `pipestest`;
- `ProviderAccounts`:
  - `Kinds` without an organization or WorkOS membership;
  - Finish's checks;
  - every `Token` mapping;
  - `reauthorize`;
- member removal and the hydrator breaking accounts;
- workspace deletion purging accounts;
- the registry;
- hermetic MCP tool tests, waits included.

**Site:**

- unit tests:
  - model helpers;
  - the callback's storage, URL clean-up and return paths;
  - the allowlist test;
  - the prompt test;
- `tests/e2e/sign-in.spec.ts`:
  - connect GitHub from Data through `/mock/connect`;
  - Cancel, then Try again on the same sign-in;
  - the agent's link during onboarding, back to the wizard with the connector chosen;
  - someone else's link;
  - a refreshed callback;
  - a broken connector: an admin gets the dialog once and then the banner, a member gets the banner without a button, and signing in again clears both;
  - a member sees no Connect button;
  - the local build offers none (`tests/e2e/local`);
- all four checks.

**On dev,** with Claude Code against `mcp.dev.helix-db.com`. An agent onboards a new workspace and:

- connects GitHub with the dev app, then syncs;
- connects Linear, recording its token's shape, expiry and refresh;
- declines once, recording what the callback gets;
- refreshes the callback page;
- removes the member who connected, then checks the banner appears at once;
- has another member sign it in again, recording whether Pipes' `established_by` changes;
- installs the app, then includes a private repository.

**Negative cases at real Pipes:**

- a wrong verifier;
- a reused code;
- another organization's code;
- a code finished after 90 seconds;
- a Helix member without a WorkOS membership authorizing.

Also: one GitHub user signing into 11 connectors, to see whether GitHub's 10-token cap revokes the oldest. Record the results here.

## Not in v1

- **Email** to owners and admins when an account breaks; the design is in "When an account breaks".
- **A warning before removing a member who connected accounts,** in the Helix dashboard's remove-member dialog: "Ada connected GitHub and Linear. Removing her stops them until someone signs them in again."
- **Offering the link through the MCP's URL-mode elicitation.** go-sdk v1.7.0 is used without elicitation today, and the server is stateless.
- **Revoking the token at the provider** when a connector is deleted.
- **Sign-in for workspaces without a WorkOS organization** (staff workspaces, unclaimed sandboxes).
- **Moving existing key connectors** over to sign-in.
- **Other kinds:**
  - PostHog, Stripe and Cal.com have OAuth in Pipes;
  - Granola is API-key only;
  - WorkOS as a source has no OAuth.
- **The local build.**

## Open questions

These are answered during verification on dev.

- **Pipes' price:** settle before production.
- **What a decline returns:** the callback takes any answer without a code.
- **How long an authorization URL lasts:** Authorize gives a fresh one.
- **Whether the 90-second code lifetime holds.**
- **Which application's redirect list Pipes checks** for `return_to`. The reference says any of the environment's applications; the Foundry application has its own key.
- **Linear:**
  - its token's format;
  - Pipes refreshing its 24-hour tokens;
  - whether a Linear workspace that requires approving OAuth apps lands on the callback with an error, or not at all.
- **What `reauthorize` does to `established_by`.** No longer a risk: only the account's own member reauthorizes, and anyone else adds a new account. Worth recording on dev all the same.
- **GitHub's per-user cap on tokens** for one app.
