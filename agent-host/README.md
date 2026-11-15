# @helixdb/foundry-agent-host

Runs a local Helix Foundry install's Assistant on the Claude Code or Codex you're signed in to on this computer: your Claude or ChatGPT subscription pays for its model, not the install's AI provider. Foundry runs in Docker, which can't reach your Keychain or your CLIs' logins, so this helper runs beside it, on the computer itself.

## Use

With Node 24, Claude Code (`claude`, then `/login`) or Codex 0.169.0 or later (`codex login`, with ChatGPT) installed and signed in, or both, and Foundry running here:

```sh
npx -y @helixdb/foundry-agent-host@0.1.0 pair hxp_…     # the code Foundry's Settings › AI models shows
npx -y @helixdb/foundry-agent-host@0.1.0 install        # keep it running (launchd on macOS, systemd on Linux)
npx -y @helixdb/foundry-agent-host@0.1.0 doctor         # what stands in the way, if anything
```

`run` runs it in a terminal instead of `install`; `--claude <path>` and `--codex <path>` name your `claude` and `codex` when they aren't on `PATH` (each command takes them; `install` keeps them, and your `CODEX_HOME` when you set one). The helper runs each CLI it finds, and pairs offering those. `pair --foundry <url>` names Foundry when it isn't at `http://127.0.0.1:8081` (the pair command Foundry shows names it when `FOUNDRY_MCP_PORT` moved it). Pairing only pairs: Foundry counts the helper as running once `run` or `install` starts it.

## What it can do

Nothing but run the Assistant's turns, with Foundry deciding everything else.

- **Its token.** A pairing code is good once, for ten minutes. It is spent for the helper's `hxh_` token, kept in `~/.config/helix-foundry/agent-host.json` (mode 600); Foundry keeps only its SHA-256. That token only claims turns and reports on them (`/agent-host/…`): it reaches neither Foundry's API nor its MCP.
- **A turn's token.** Each turn Foundry leases the helper comes with an `hxr_` token for that turn alone: Foundry's MCP takes it for the Assistant's tools only, in the turn's workspace, as you, at most 25 calls, until the lease or the turn ends. Revoking the helper in Foundry stops both at once. It never goes in a CLI's arguments or the helper's log.
- **Claude Code.** It drives your own `claude` through the Claude Agent SDK, as t3code does, signed in as Claude Code always is (the Keychain, `~/.claude/.credentials.json` or `CLAUDE_CODE_OAUTH_TOKEN`; `HOME` and `CLAUDE_CONFIG_DIR` are left as they are). Each turn runs with no built-in tool (`tools: []`: no shell, files or web), Foundry's MCP server alone (sent over Claude Code's control channel, never its arguments or environment; `strictMcpConfig`, no claude.ai connectors), none of your settings, CLAUDE.md, hooks or plugins (`settingSources: []`), `allowedTools: ["mcp__foundry__*"]` in `dontAsk` mode with a `canUseTool` that denies anything else, Foundry's system prompt in place of Claude Code's, and an empty directory of the thread's own (`~/.local/state/helix-foundry/agent-host/threads/<workspace>/<thread>`), under which Claude Code keeps the session the thread's next turn resumes (in `~/.claude/projects/`). `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` are taken out of its environment, so it uses your subscription, not an API account.
- **Codex.** It drives your own `codex app-server`, as t3code does: one per turn, over stdio, signed in as Codex always is (your `CODEX_HOME`'s login, which the helper never reads, copies or refreshes), with your own Codex setup (below).
- **What Foundry is told of your sign-in.** Whether each CLI is signed in, and the plan of its subscription (`pro`, `max`, … for Claude; `plus`, `pro`, `team`, … for ChatGPT), and Codex's version: never your email or a token. The helper asks as it starts and every 10 minutes, and Settings › AI models shows it beside the helper.
- **What stays on this computer.** A thread's CLI session holds what Foundry's tools answered in it (graph reads, document text, reviews): Claude Code's under `~/.claude/projects/`, Codex's in its `CODEX_HOME` (its `codex resume` lists them). The helper asks Foundry which of its threads were deleted as it starts and every hour, and deletes those sessions and directories (Codex's through its app-server's `thread/delete`); revoked, it deletes every one.
- **What you see.** Foundry, not the helper, records each tool call and draws each card; the helper reports only the text the model writes and how the turn ended. Anything that changes your workspace is a card you run, or not, with a click.

A turn costs the install nothing, and counts toward the Assistant's hourly messages, steps and wall time (Codex's steps are its model calls, as its items show them). If the helper stops mid-turn, Foundry runs the turn again, within the wall time left from its first start, unless it had shown you something of it, in which case it says it was interrupted. A turn no helper takes within 3 minutes, while none is running, fails and says so.

Whether your plan's terms let you use your subscription this way is yours to check.

## Codex

Codex runs with **your own Codex setup**: its app-server merges your `~/.codex/config.toml` (your MCP servers, plugins, apps, profiles), managed and MDM configuration and what a ChatGPT workspace pushes into every thread, and has no option to leave them out. That is your choice to make, and Settings › AI models asks you once: choosing "My ChatGPT subscription (Codex)" shows what it runs with, and Foundry won't run new threads on Codex until you confirm (`FOUNDRY_AGENT_CODEX_UNCONFIRMED`; the Assistant's cards can't confirm it for you).

The Assistant reads untrusted data (documents, synced rows) through Foundry's tools, and a document could carry instructions meant to mislead it. So, over your setup, each turn's app-server runs with:

- **No shell.** `features.shell_tool` and `features.unified_exec` off: Codex offers no command tool at all, so nothing runs unasked, "known safe" or not (`untrusted` would auto-run those). The image viewer (`features.view_image`), web search (`web_search = "disabled"`), image generation, browser and computer use, goals, memories, hooks, tool suggestions and sleep are off too. These go as `-c` overrides, none secret.
- **Read-only, every action asking this helper.** `thread/start` (and `thread/resume`) set `sandbox: read-only`, `approvalPolicy: untrusted` and `approvalsReviewer: user`, over yours (an auto reviewer of yours would approve on its own). The helper, as the app-server's client, declines every approval Codex asks for: command executions, file changes (Codex's `apply_patch`, which no configuration removes, asks and is declined, inside the thread's directory or out of it), permission escalations, and any MCP server's tool call that asks (your servers' writing tools, by their annotations). It answers no question for you (`request_user_input`).
- **Foundry's tools, unasked.** Foundry's MCP server is added as `foundry_assistant_turn` (a name unlikely to be one of yours: Codex merges a server of the same name), with `bearer_token_env_var = "HELIX_FOUNDRY_TURN_TOKEN"`: the turn's token is in the app-server's environment only, never an argument, and its stdio MCP servers get Codex's default environment, not it. `default_tools_approval_mode = "approve"`: Foundry's tools never ask (should a managed configuration make one ask, the helper accepts Foundry's alone). They are whatever Foundry's MCP server lists; the helper names none. Anything that changes your workspace is still a card you run, or not, with a click.
- **Foundry's system prompt** in place of Codex's (`baseInstructions`), telling the model to use Foundry's tools alone; an empty directory of the thread's own as its working directory; and the thread's Codex thread resumed.

What still runs, and is yours: a tool of your MCP servers, plugins or apps that doesn't ask for approval (one marked read-only, say) can be called in the Assistant's turns, and Codex can start sub-agents (no configuration turns them off), which run in the same app-server under the same overrides and ask the same helper, so their commands and file changes are declined too; the app-server, and any sub-agent, ends with the turn. Your OpenAI API key in the environment (`OPENAI_API_KEY`, `CODEX_API_KEY`) isn't passed on, so turns bill your ChatGPT plan; a Codex signed in with an API key says so in Settings.

**Version.** The helper and Foundry run Codex 0.169.0 or later (`MIN_CODEX`, `agent.MinCodexVersion`), the version the isolation above was tested on: its configuration keys and approvals change between versions. A helper with an older Codex still pairs and says its version; Foundry runs no turn on it (Settings and the Assistant say to update it), and `doctor` says so too.

`test/real-codex.test.ts` checks all of this against the real `codex app-server` (`REAL_CODEX`; CI runs the 0.169.0 pinned as a dev dependency), signed in with `codex login --with-api-key` and a fake key in a throwaway `CODEX_HOME` whose own `config.toml` points Codex at a stand-in Responses API (`openai_base_url`), sets `approval_policy = "never"`, a full-access sandbox, an auto reviewer and the shell on, allows `touch` by an exec rule, and adds MCP servers of the person's (one named `foundry`, one with a writing and a reading tool). It proves, in code mode (the default models' `exec` tool) and with direct tools (`gpt-5.5`): no shell or exec tool is offered; Foundry's tools are called with the turn's token, unasked, and answered; `apply_patch` and the person's writing tool are declined and nothing is written; a sub-agent gets the same tools and its changes are declined; the token is in no argument and nothing under `CODEX_HOME`; the next turn resumes the Codex thread; and the helper deletes it. A ChatGPT plan's `account/read` is the fake app-server's (`test/fake-codex.mjs`): no ChatGPT login exists on CI.

## Develop

```sh
pnpm install
pnpm typecheck
pnpm test        # against test/fake-claude.mjs, test/fake-codex.mjs and a fake Foundry: no login needed
pnpm build       # dist/, which the package ships
```

`backend/cmd/foundry`'s `TestFrontWithTheHelper` runs the built helper against Foundry's real front end, with `FOUNDRY_HELPER_E2E` set to this directory.

`test/real-codex.test.ts` (above) and `test/real-claude.test.ts` check the isolation against the real CLIs. `real-claude` runs against a real Claude Code, `REAL_CLAUDE` (CI uses the one the pinned Agent SDK bundles), talking to a stand-in for Anthropic's API with your own MCP server, hooks and permissions planted in its `HOME`: the model is offered Foundry's tools alone (and Claude Code's inert `DeferredToolPlaceholder`), and Bash and Read are refused. Run them when you bump the SDK or Codex.
