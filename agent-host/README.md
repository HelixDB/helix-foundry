# @helixdb/foundry-agent-host

Runs a local Helix Foundry install's Assistant on the Claude Code you're signed in to on this computer: your Claude subscription pays for its model, not the install's AI provider. Foundry runs in Docker, which can't reach your Keychain or Claude Code's login, so this helper runs beside it, on the computer itself.

## Use

With Node 24, Claude Code installed and signed in (`claude`, then `/login`), and Foundry running here:

```sh
npx -y @helixdb/foundry-agent-host@0.1.0 pair hxp_…     # the code Foundry's Settings › AI models shows
npx -y @helixdb/foundry-agent-host@0.1.0 install        # keep it running (launchd on macOS, systemd on Linux)
npx -y @helixdb/foundry-agent-host@0.1.0 doctor         # what stands in the way, if anything
```

`run` runs it in a terminal instead of `install`; `--claude <path>` names your `claude` when it isn't on `PATH`; `pair --foundry <url>` names Foundry when it isn't at `http://127.0.0.1:8081` (the pair command Foundry shows names it when `FOUNDRY_MCP_PORT` moved it). Pairing only pairs: Foundry counts the helper as running once `run` or `install` starts it.

## What it can do

Nothing but run the Assistant's turns, with Foundry deciding everything else.

- **Its token.** A pairing code is good once, for ten minutes. It is spent for the helper's `hxh_` token, kept in `~/.config/helix-foundry/agent-host.json` (mode 600); Foundry keeps only its SHA-256. That token only claims turns and reports on them (`/agent-host/…`): it reaches neither Foundry's API nor its MCP.
- **A turn's token.** Each turn Foundry leases the helper comes with an `hxr_` token for that turn alone: Foundry's MCP takes it for the Assistant's tools only, in the turn's workspace, as you, at most 25 calls, until the lease or the turn ends. Revoking the helper in Foundry stops both at once.
- **Claude Code.** It drives your own `claude` through the Claude Agent SDK, as t3code does, signed in as Claude Code always is (the Keychain, `~/.claude/.credentials.json` or `CLAUDE_CODE_OAUTH_TOKEN`; `HOME` and `CLAUDE_CONFIG_DIR` are left as they are). Each turn runs with no built-in tool (`tools: []`: no shell, files or web), Foundry's MCP server alone (sent over Claude Code's control channel, never its arguments or environment; `strictMcpConfig`, no claude.ai connectors), none of your settings, CLAUDE.md, hooks or plugins (`settingSources: []`), `allowedTools: ["mcp__foundry__*"]` in `dontAsk` mode with a `canUseTool` that denies anything else, Foundry's system prompt in place of Claude Code's, and an empty directory of the thread's own (`~/.local/state/helix-foundry/agent-host/threads/<workspace>/<thread>`), under which Claude Code keeps the session the thread's next turn resumes (in `~/.claude/projects/`). `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` are taken out of its environment, so it uses your subscription, not an API account.
- **What Foundry is told of your sign-in.** Whether Claude Code is signed in, and the plan of its subscription (`pro`, `max`, …), as it says when it starts: never your email or a token. The helper asks as it starts and every 10 minutes, and Settings › AI models shows it beside the helper.
- **What stays on this computer.** A thread's Claude Code session holds what Foundry's tools answered in it (graph reads, document text, reviews). The helper asks Foundry which of its threads were deleted as it starts and every hour, and deletes those sessions and directories; revoked, it deletes every one.
- **What you see.** Foundry, not the helper, records each tool call and draws each card; the helper reports only the text the model writes and how the turn ended. Anything that changes your workspace is a card you run, or not, with a click.

A turn costs the install nothing, and counts toward the Assistant's hourly messages, steps and wall time. If the helper stops mid-turn, Foundry runs the turn again, within the wall time left from its first start, unless it had shown you something of it, in which case it says it was interrupted. A turn no helper takes within 3 minutes, while none is running, fails and says so.

Whether your plan's terms let you use your subscription this way is yours to check.

## Codex

Codex isn't offered yet: on the pinned `codex app-server` (0.169.0), no configuration gives Codex Foundry's tools alone.

- `codex app-server` merges your own Codex configuration into every thread: your `~/.codex/config.toml` (its MCP servers, profiles, hooks and plugins), managed and MDM configuration, and the configuration a ChatGPT workspace pushes. `codex exec` has `--ignore-user-config`; the app-server has no such option, and `-c` overrides merge into those layers rather than replace them, so your MCP servers can only be turned off one by one, by a name the helper would have to find in layers it can't all read.
- Its file-editing tool, `apply_patch`, is offered whatever the configuration, by the model's catalog entry. With its shell tools and image viewer turned off and the experimental `thread/start.environments: []`, a probe of 0.169.0 against a stand-in Responses endpoint saw `request_user_input`, `apply_patch` and the goal tools offered to the model (and `exec_command`, `write_stdin`, `view_image`, the MCP resource tools, `tool_search` and `web_search` by default); `apply_patch` then fails for want of an environment, which is experimental behaviour, not a guarantee.
- Giving Codex a home of its own would leave your configuration out, but not your login: Codex saves `auth.json` by writing a new file and renaming it over the old, and ChatGPT refresh tokens rotate, so a copy or a link of it would sign out the Codex you use yourself. Sharing tokens instead (`account/login/start` with `chatgptAuthTokens`) is experimental, and would have the helper refresh your ChatGPT tokens itself.

Codex can join once its app-server takes a configuration of the helper's alone (an option like `exec`'s `--ignore-user-config`) and can leave out its built-in tools. Foundry's side is ready for it (the runtime `codex`); it refuses the runtime until then (`FOUNDRY_AGENT_RUNTIME_UNAVAILABLE`).

## Develop

```sh
pnpm install
pnpm typecheck
pnpm test        # against test/fake-claude.mjs and a fake Foundry: no login needed
pnpm build       # dist/, which the package ships
```

`backend/cmd/foundry`'s `TestFrontWithTheHelper` runs the built helper against Foundry's real front end, with `FOUNDRY_HELPER_E2E` set to this directory.

`test/real-claude.test.ts` checks the isolation against a real Claude Code, `REAL_CLAUDE` (CI uses the one the pinned Agent SDK bundles), talking to a stand-in for Anthropic's API with your own MCP server, hooks and permissions planted in its `HOME`: the model is offered Foundry's tools alone (and Claude Code's inert `DeferredToolPlaceholder`), and Bash and Read are refused. Run it when you bump the SDK.
