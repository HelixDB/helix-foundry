# Custom AI provider

Add a Custom choice to the existing AI setup and workspace settings form. Users
provide an OpenAI-compatible API base URL, model name, protocol (Chat Completions
or Responses), and an optional API key. Models must support structured JSON
output. Existing Local, Claude and OpenAI behavior stays intact.

## Implementation

1. Extend the shared provider contract and validate Custom HTTP(S) base URLs.
   Reject credentials, query strings and fragments in these URLs.
2. Send Custom requests to the selected base URL and protocol, with bearer
   authentication when configured, structured output and existing call timeouts.
3. Add Custom controls using the current choice cards and form styles. Store
   credentials through the existing encrypted provider workflow. Reuse a saved
   key only for the same provider endpoint; never carry it to a changed URL.
4. Update activity labels and usage documentation.
5. Verify both protocols, encrypted persistence, endpoint changes, validation
   failures and existing provider behavior. Build the Docker image, run isolated
   tests, restart this task's app services and verify the deployed form and health.

## Review

- A distinct Custom provider keeps OpenAI-only voice sessions and Local-only
  model downloads from receiving gateway credentials.
- Custom endpoints may be remote; they retain hosted-provider data controls.
- Tests use dummy keys and isolated storage, never the user's workspace or token.
- DP MaaS example: base URL `https://dev.agify.app/infra/maas/v1`, model `best`.
  The user enters the API key in the browser.

## Validation completed, 2026-10-11

- Docker production build passed, including shared/SDK builds and TypeScript.
- 61 tests passed across Custom providers, existing providers, onboarding, voice,
  SDK, workspace and activity contracts in an isolated container.
- Deployed at `http://localhost:3001`; health and container dependency checks pass.
- Browser verified Custom selection, URL/model entry and Responses selection.
  Screenshot: `.data/custom-ai-settings.jpg` (ignored by Git).
- DP MaaS fields are prepared in the open form. Its API key and authenticated
  gateway test remain for the user to complete through the browser.
