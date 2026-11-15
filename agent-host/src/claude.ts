// One turn on the Claude Code the person signed in to, as t3code drives it
// (ClaudeAdapterV2): the Agent SDK's query() runs their own `claude`
// executable (pathToClaudeCodeExecutable), which signs in as it always does
// (the macOS Keychain, ~/.claude/.credentials.json or
// CLAUDE_CODE_OAUTH_TOKEN), so its subscription pays and no key is asked
// for. The helper keeps HOME (and CLAUDE_CONFIG_DIR, when set) as they are,
// so the Keychain stays reachable.
//
// What the CLI may do is Foundry's tools alone:
//   - no built-in tool (tools: []), so no shell, file or web tool exists;
//   - one MCP server, Foundry's, with the turn's token, registered over the
//     CLI's control channel (setMcpServers), never in its argv or
//     environment, and none of the person's (strictMcpConfig, and no
//     claude.ai connectors);
//   - none of the person's settings, CLAUDE.md, hooks or plugins
//     (settingSources: [], and hooks off);
//   - allowedTools mcp__foundry__* in dontAsk mode, and canUseTool denying
//     anything else, should the CLI ever ask;
//   - Foundry's system prompt in place of Claude Code's;
//   - an empty working directory of the thread's, at most max_steps model
//     calls, and the thread's session resumed.

import {
  query,
  type Options,
  type SDKMessage,
  type SDKUserMessage,
} from "@anthropic-ai/claude-agent-sdk";

import type { End, Turn } from "./foundry.ts";

/** What a turn reports as it runs: the model's text, by step. */
export type Reporter = {
  /** Text the model is writing in step, as it comes. */
  delta(step: number, text: string): void;
  /** The step's text once its call ended: it replaces the step's deltas. */
  said(step: number, text: string): Promise<void>;
};

/** How a turn ran: how it ended, its model calls, and its session. */
export type Ran = { end: End; steps: number; sessionId: string };

/** The tools a turn's CLI may call: Foundry's, by its MCP server's name. */
export const FOUNDRY_TOOLS = "mcp__foundry__";

/** Environment the CLI never gets: a key would bill an API account, not the subscription. */
const DROPPED_ENV = ["ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"];

/** The CLI's environment: the helper's, less any API key, with the person's other MCP sources and IDE off. */
export function cliEnv(env: NodeJS.ProcessEnv): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(env))
    if (v !== undefined && !DROPPED_ENV.includes(k)) out[k] = v;
  return {
    ...out,
    ENABLE_CLAUDEAI_MCP_SERVERS: "false",
    CLAUDE_CODE_AUTO_CONNECT_IDE: "0",
    CLAUDE_CODE_IDE_SKIP_AUTO_INSTALL: "1",
    DISABLE_AUTOUPDATER: "1",
  };
}

/** The query's options for a turn: everything but its MCP server, which goes over the control channel. */
export function claudeOptions(
  turn: Turn,
  opts: {
    claude: string;
    cwd: string;
    resume: boolean;
    abort: AbortController;
    env: NodeJS.ProcessEnv;
  },
): Options {
  return {
    pathToClaudeCodeExecutable: opts.claude,
    cwd: opts.cwd,
    tools: [],
    mcpServers: {},
    strictMcpConfig: true,
    settingSources: [],
    settings: { disableAllHooks: true },
    allowedTools: [`${FOUNDRY_TOOLS}*`],
    permissionMode: "dontAsk",
    canUseTool: async (name, input) =>
      name.startsWith(FOUNDRY_TOOLS)
        ? { behavior: "allow", updatedInput: input }
        : { behavior: "deny", message: "Only Foundry's tools run here." },
    systemPrompt: turn.system,
    maxTurns: turn.max_steps,
    includePartialMessages: true,
    persistSession: true,
    ...(opts.resume && turn.resume ? { resume: turn.resume } : {}),
    abortController: opts.abort,
    env: cliEnv(opts.env),
    stderr: () => {},
  };
}

/** What the person said, as the CLI's first message: with the thread in words when it starts afresh. */
export function promptText(turn: Turn, resumed: boolean): string {
  return !resumed && turn.history
    ? `${turn.history}\n\n---\n\n${turn.prompt}`
    : turn.prompt;
}

/** A CLI's failure in its own words, as Foundry words it. */
export function failureOf(text: string): End {
  const t = text.toLowerCase();
  if (
    /(\/login|not logged in|log in|invalid api key|oauth token|authentication|unauthorized|credit balance)/.test(
      t,
    )
  )
    return { end: "failed", failure: "signed_out" };
  if (/(usage limit|rate limit|rate_limit|429|limit reached)/.test(t))
    return { end: "failed", failure: "rate_limited" };
  return { end: "failed", failure: "cli_error", detail: text.slice(0, 1000) };
}

/** A resume of a session the CLI no longer holds. */
const LOST_SESSION = /no conversation found/i;

/** The query function: the SDK's, or a test's. */
export type Query = typeof query;

/**
 * Runs a turn on Claude Code: once on the thread's session, and once
 * afresh, told the thread in words, should the CLI have lost it.
 */
export async function runClaude(
  turn: Turn,
  opts: {
    claude: string;
    cwd: string;
    mcpURL: string;
    abort: AbortController;
    env?: NodeJS.ProcessEnv;
    query?: Query;
  },
  report: Reporter,
): Promise<Ran> {
  const resumed = !!turn.resume;
  const first = await attempt(turn, { ...opts, resume: resumed }, report);
  if (!resumed || !first.lostSession) return first.ran;
  return (await attempt(turn, { ...opts, resume: false }, report)).ran;
}

async function attempt(
  turn: Turn,
  opts: {
    claude: string;
    cwd: string;
    mcpURL: string;
    abort: AbortController;
    env?: NodeJS.ProcessEnv;
    query?: Query;
    resume: boolean;
  },
  report: Reporter,
): Promise<{ ran: Ran; lostSession: boolean }> {
  const run = opts.query ?? query;
  let release: () => void = () => {};
  const registered = new Promise<void>((resolve) => (release = resolve));
  // The message waits for Foundry's MCP server: the model's first call
  // never runs without it.
  async function* prompt(): AsyncGenerator<SDKUserMessage> {
    await registered;
    yield {
      type: "user",
      message: { role: "user", content: promptText(turn, opts.resume) },
      parent_tool_use_id: null,
      session_id: "",
    };
  }
  let steps = 0;
  let sessionId = turn.resume && opts.resume ? turn.resume : "";
  let step = { id: "", text: "" };
  // The step's final text, once the next starts or the turn ends.
  const close = async () => {
    if (step.id && step.text.trim()) await report.said(steps, step.text);
    step = { id: "", text: "" };
  };
  try {
    const q = run({
      prompt: prompt(),
      options: claudeOptions(turn, { ...opts, env: opts.env ?? process.env }),
    });
    const set = await q.setMcpServers({
      foundry: {
        type: "http",
        url: opts.mcpURL,
        headers: { Authorization: `Bearer ${turn.mcp_token}` },
      },
    });
    if (set.errors && Object.keys(set.errors).length > 0) {
      q.close();
      return {
        ran: {
          end: {
            end: "failed",
            failure: "cli_error",
            detail: "Claude Code couldn't reach Foundry's tools",
          },
          steps,
          sessionId,
        },
        lostSession: false,
      };
    }
    release();
    for await (const m of q as AsyncIterable<SDKMessage>) {
      if ("session_id" in m && typeof m.session_id === "string" && m.session_id)
        sessionId = m.session_id;
      switch (m.type) {
        case "stream_event": {
          const e = m.event as {
            type?: string;
            delta?: { type?: string; text?: string };
          };
          if (
            e.type === "content_block_delta" &&
            e.delta?.type === "text_delta" &&
            e.delta.text
          )
            report.delta(Math.max(steps, 1), e.delta.text);
          break;
        }
        case "assistant": {
          const message = m.message as {
            id?: string;
            content?: { type: string; text?: string }[];
          };
          const id = message.id ?? "";
          if (id !== step.id) {
            await close();
            steps++;
            step = { id: id || `step-${steps}`, text: "" };
          }
          for (const block of message.content ?? [])
            if (block.type === "text" && block.text) step.text += block.text;
          break;
        }
        case "result": {
          await close();
          const r = m as {
            subtype: string;
            is_error?: boolean;
            result?: string;
            errors?: string[];
          };
          if (r.subtype === "success" && !r.is_error)
            return {
              ran: { end: { end: "answered" }, steps, sessionId },
              lostSession: false,
            };
          if (r.subtype === "error_max_turns")
            return {
              ran: {
                end: { end: "stopped", reason: "max_steps" },
                steps,
                sessionId,
              },
              lostSession: false,
            };
          const words =
            [r.result ?? "", ...(r.errors ?? [])].join(" ").trim() || r.subtype;
          if (opts.resume && LOST_SESSION.test(words))
            return {
              ran: { end: failureOf(words), steps, sessionId: "" },
              lostSession: true,
            };
          return {
            ran: { end: failureOf(words), steps, sessionId },
            lostSession: false,
          };
        }
      }
    }
    await close();
    return {
      ran: {
        end: {
          end: "failed",
          failure: "cli_error",
          detail: "Claude Code stopped without an answer",
        },
        steps,
        sessionId,
      },
      lostSession: false,
    };
  } catch (e) {
    release();
    const err = e as NodeJS.ErrnoException;
    const words = err.message ?? String(e);
    if (opts.resume && LOST_SESSION.test(words))
      return {
        ran: { end: failureOf(words), steps, sessionId: "" },
        lostSession: true,
      };
    if (
      err.code === "ENOENT" ||
      /ENOENT|executable not found|no such file/i.test(words)
    )
      return {
        ran: {
          end: { end: "failed", failure: "not_installed" },
          steps,
          sessionId,
        },
        lostSession: false,
      };
    return {
      ran: { end: failureOf(words), steps, sessionId },
      lostSession: false,
    };
  }
}
