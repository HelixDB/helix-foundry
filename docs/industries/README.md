# Industry playbooks

The first runtime playbook is [online retail](ecommerce.md). This is the smaller
replacement for PR #74: schema-based retail detection, persisted background
data checks, and ordering of Home's supported metrics.

A playbook is Markdown with a JSON configuration block. `industry-playbook.ts` validates
its roles, primitive metrics, check kinds and referenced roles before use.
The prose states the meaning and limits of the same rules reviewers see in code.
Only online retail is supported in this PR; a loader or drafting engine for
other industries is not implied by the file format.

`industry-checks.ts` uses the existing durable worker. A cache key includes the
playbook content and role mapping, active publication generation, accepted
ontology hash, relevant dataset versions and privacy policy. Results are saved
as one `industryPlaybook` resource per workspace. GET only reads metadata and
saved results and coalesces a background job when needed; it never runs a
check. An editor may request a refresh. Automatic refreshes within a generation
start at most once per 15 minutes, including across worker restarts. A new
generation can scan immediately. After the worker's bounded retries, failures
retain earlier results and wait for manual retry for the same inputs.
Superseded results cannot overwrite a newer
generation or dataset version.

Workspace API routes:

- `GET /api/v1/workspaces/:id/industry` returns the reading, saved checks,
  suggestions, original check time, stale flag and job state.
- `POST /api/v1/workspaces/:id/industry/check` requests a refresh, or advances
  an already queued refresh. It requires editor access. Read-only tokens may
  read the saved results but cannot request a manual scan.

Home's existing compiler validates every proposed metric. The playbook changes
neither the compiler's supported operations nor the model prompts. The four
retail primitives precede other working picks; generic workspaces preserve their
existing signatures and ordering. The playbook's content identity invalidates
stored retail picks when the reading changes.

Deferred features are listed at the end of the e-commerce playbook. Issue #71
remains open until those later parts have been reviewed and implemented.
