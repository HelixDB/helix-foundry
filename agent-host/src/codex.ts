// One turn on the Codex the person signed in to, as t3code drives it: the
// person's own `codex app-server`, spoken to over stdio JSON-RPC, signed in
// as Codex always is (their CODEX_HOME's login, which the helper never
// reads, copies or refreshes), so their ChatGPT plan pays.
//
// Codex runs with the person's own Codex configuration (their MCP servers,
// plugins, apps and managed layers): its app-server has no way to leave it
// out, and the person chose it so (Settings › AI models says so). What the
// helper sets, over that, for each turn's app-server (one per turn, ended
// with it):
//   - its shell and exec tools, image viewer, web search, image generation,
//     browser and computer use, goals, memories, hooks, tool suggestions,
//     sleep and sub-agents off (LOCKED: configuration overrides, none of
//     them secret);
//   - a read-only sandbox, every action asking first (approvalPolicy
//     untrusted) and asking this client, not a reviewer of the person's
//     (approvalsReviewer user); the helper declines every command, file
//     change, permission and other server's tool call Codex asks it about
//     (answer): Codex's apply_patch, which no configuration removes, asks
//     and is declined;
//   - Foundry's MCP server (CODEX_SERVER), with the turn's hxr_ token by
//     environment variable (TOKEN_ENV, never an argument), its tools
//     approved (default_tools_approval_mode approve: Foundry prepares its
//     changes as cards the person runs);
//   - Foundry's system prompt in place of Codex's, an empty working
//     directory of the thread's, and the thread's Codex thread resumed.
// A managed configuration (managed_config.toml, MDM) outranks all of that,
// so before the model is called the helper reads back what Codex will run
// with (config/read, and the thread's settings as Codex answers them), and
// runs nothing unless every one holds (unheld); nor on a Codex older than
// MIN_CODEX, whatever Foundry was last told.

import { execFile, spawn, type ChildProcess } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import { homedir, tmpdir } from "node:os";
import { join } from "node:path";
import { promisify } from "node:util";

import type { Sessions } from "./config.ts";
import type { Account, End, Turn } from "./foundry.ts";
import { promptText, type Cli, type Ran, type Reporter } from "./runtime.ts";

/** The oldest Codex the helper runs: the one its isolation was tested on (test/real-codex.test.ts). Foundry holds the same floor (agent.MinCodexVersion). */
export const MIN_CODEX = "0.169.0";

/** Foundry's MCP server as Codex names it: a name no one's own server is likely to have, since Codex merges a server of the same name with the person's. Foundry's system prompt names it (agent.CodexMCPServer). */
export const CODEX_SERVER = "foundry_assistant_turn";

/** The environment variable the turn's token reaches Codex's MCP client in. */
export const TOKEN_ENV = "HELIX_FOUNDRY_TURN_TOKEN";

/** How long Codex has to start, answer a request, and start Foundry's tools. */
const START_MS = 60_000;

/** How much of Codex's stderr an app-server keeps: its last words. */
const STDERR_KEPT = 4096;

/**
 * What the helper turns off in every app-server of its, each a key and the
 * value it sets: overrides of the person's configuration, none secret. Sub-
 * agents are off as Codex 0.169.0 says to turn them off (agents.enabled,
 * which multi_agent_v2 would override, and multi_agent's V1 fallback).
 */
export const LOCKED: readonly (readonly [string, boolean | string])[] = [
  ["features.shell_tool", false],
  ["features.unified_exec", false],
  ["features.view_image", false],
  ["web_search", "disabled"],
  ["features.image_generation", false],
  ["features.browser_use", false],
  ["features.computer_use", false],
  ["features.in_app_browser", false],
  ["features.goals", false],
  ["features.memories", false],
  ["features.hooks", false],
  ["features.tool_suggest", false],
  ["features.sleep_tool", false],
  ["agents.enabled", false],
  ["features.multi_agent", false],
  ["features.multi_agent_v2", false],
];

/** The configuration every app-server of the helper's runs with: LOCKED, then Foundry's MCP server. */
export function codexConfig(mcpURL: string): string[] {
  const server = `{url=${JSON.stringify(mcpURL)}, bearer_token_env_var="${TOKEN_ENV}", default_tools_approval_mode="approve", startup_timeout_sec=30, tool_timeout_sec=300}`;
  return [
    ...LOCKED.map(([k, v]) => `${k}=${JSON.stringify(v)}`),
    `mcp_servers.${CODEX_SERVER}=${server}`,
  ];
}

/** Every thread's settings: read-only, every action asking this client. */
const THREAD = {
  approvalPolicy: "untrusted",
  sandbox: "read-only",
  approvalsReviewer: "user",
} as const;

/** A turn's thread's settings, started or resumed: THREAD, in cwd, on Foundry's system prompt. */
const threadSettings = (turn: Turn, cwd: string) => ({
  cwd,
  ...THREAD,
  baseInstructions: turn.system,
});

/**
 * What of the helper's settings Codex doesn't hold: the LOCKED keys its
 * effective configuration (config/read's config) has otherwise, and those
 * of a thread (thread/start's or thread/resume's answer) that aren't
 * threadSettings'. A managed configuration (managed_config.toml, MDM)
 * outranks the helper's overrides and a thread's settings alike, so the
 * helper reads back what Codex will run with and runs no turn unless it is
 * all as set. Pure: read without trust.
 *
 * @example
 * unheld({ features: { shell_tool: true } }); // every LOCKED key: shell_tool on, the rest unset
 * unheld(undefined, { approvalPolicy: "never", sandbox: { type: "readOnly" }, approvalsReviewer: "user" }); // ["approval_policy"]
 */
export function unheld(config?: unknown, thread?: unknown): string[] {
  const at = (o: unknown, path: string) =>
    path
      .split(".")
      .reduce<unknown>(
        (x, k) =>
          x && typeof x === "object" ? (x as Record<string, unknown>)[k] : undefined,
        o,
      );
  const keys =
    config === undefined
      ? []
      : LOCKED.filter(([k, v]) => at(config, k) !== v).map(([k]) => k);
  if (thread === undefined) return keys;
  return [
    ...keys,
    ...(at(thread, "approvalPolicy") === "untrusted" ? [] : ["approval_policy"]),
    ...(at(thread, "sandbox.type") === "readOnly" ? [] : ["sandbox_mode"]),
    ...(at(thread, "approvalsReviewer") === "user" ? [] : ["approvals_reviewer"]),
  ];
}

/** Why a turn doesn't run when Codex holds not all the helper sets (unheld), in the person's words. */
export function unheldDetail(keys: string[]): string {
  return `your Codex configuration turns back on what Foundry's Assistant runs without (${keys.join(", ")}), most likely a managed configuration (managed_config.toml or MDM): Foundry ran nothing on it`;
}

/** Environment the app-server never gets: a key would bill an API account, not the ChatGPT plan. */
const DROPPED_ENV = ["OPENAI_API_KEY", "CODEX_API_KEY", TOKEN_ENV];

/** The app-server's environment: the helper's (HOME and CODEX_HOME as they are), less any API key, with the turn's token when it has one. */
export function codexEnv(
  env: NodeJS.ProcessEnv,
  token?: string,
): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(env))
    if (v !== undefined && !DROPPED_ENV.includes(k)) out[k] = v;
  return token ? { ...out, [TOKEN_ENV]: token } : out;
}

/**
 * How the helper answers what Codex asks its client: no to every command,
 * file change, permission and question, and to any MCP server's tool call
 * but Foundry's (which runs unasked; this accepts one should a managed
 * configuration make Codex ask). Pure: what it answers is all it does.
 */
export function answer(
  method: string,
  params: unknown,
): { result: unknown } | { error: { code: number; message: string } } {
  const no = "Foundry's Assistant doesn't run commands or change files.";
  switch (method) {
    case "item/commandExecution/requestApproval":
    case "item/fileChange/requestApproval":
      return { result: { decision: "decline" } };
    case "item/permissions/requestApproval":
      return { result: { permissions: {}, scope: "turn" } };
    case "execCommandApproval":
    case "applyPatchApproval":
      return { result: { decision: { denied: { rejection: no } } } };
    case "mcpServer/elicitation/request": {
      const p = (params ?? {}) as {
        serverName?: unknown;
        _meta?: { codex_approval_kind?: unknown } | null;
      };
      const foundrys =
        p.serverName === CODEX_SERVER &&
        p._meta?.codex_approval_kind === "mcp_tool_call";
      return {
        result: foundrys
          ? { action: "accept", content: {}, _meta: null }
          : { action: "decline", content: null, _meta: null },
      };
    }
  }
  return { error: { code: -32601, message: `${no} (${method})` } };
}

/** A failed JSON-RPC request, with Codex's words. */
export class CodexError extends Error {
  readonly code: number;
  constructor(code: number, message: string) {
    super(message);
    this.code = code;
  }
}

type Message = {
  id?: number | string;
  method?: string;
  params?: any;
  result?: any;
  error?: { code: number; message: string };
};

/**
 * One `codex app-server` over stdio: requests and their answers, the
 * notifications it sends (to `on`), and the requests it makes of its
 * client, each answered by answer().
 */
export class AppServer {
  readonly child: ChildProcess;
  private next = 0;
  private waiting = new Map<
    number,
    { resolve: (v: any) => void; reject: (e: Error) => void }
  >();
  private listeners: ((m: Message) => void)[] = [];
  /** Why it ended, once it did. */
  exited?: Error;
  /** What Codex asked and was told, for the helper's tests. */
  asked: { method: string; answered: unknown }[] = [];
  /** The end of what Codex wrote to stderr: its words when it fails to start. */
  stderr = "";

  constructor(
    codex: string,
    args: string[],
    cwd: string,
    env: Record<string, string>,
  ) {
    this.child = spawn(codex, ["app-server", ...args], {
      cwd,
      env,
      stdio: ["pipe", "pipe", "pipe"],
    });
    this.child.stderr!.setEncoding("utf8");
    this.child.stderr!.on("data", (d: string) => {
      this.stderr = (this.stderr + d).slice(-STDERR_KEPT);
    });
    let buf = "";
    this.child.stdout!.setEncoding("utf8");
    this.child.stdout!.on("data", (d: string) => {
      buf += d;
      for (let k = buf.indexOf("\n"); k >= 0; k = buf.indexOf("\n")) {
        const line = buf.slice(0, k).trim();
        buf = buf.slice(k + 1);
        if (!line) continue;
        let m: Message;
        try {
          m = JSON.parse(line) as Message;
        } catch {
          continue;
        }
        this.received(m);
      }
    });
    const end = (e: Error) => {
      this.exited ??= e;
      for (const w of this.waiting.values()) w.reject(e);
      this.waiting.clear();
      for (const l of this.listeners) l({ method: "helper/exited" });
    };
    this.child.on("error", (e) => end(e));
    this.child.on("exit", (code, signal) =>
      end(new Error(`Codex exited (${signal ?? code})`)),
    );
    this.child.stdin!.on("error", () => {});
  }

  private received(m: Message) {
    if (m.method && m.id !== undefined) {
      const reply = answer(m.method, m.params);
      this.asked.push({
        method: m.method,
        answered: "result" in reply ? reply.result : reply.error,
      });
      this.send({ id: m.id, ...reply });
      return;
    }
    if (m.id !== undefined) {
      const w = this.waiting.get(Number(m.id));
      this.waiting.delete(Number(m.id));
      if (m.error) w?.reject(new CodexError(m.error.code, m.error.message));
      else w?.resolve(m.result);
      return;
    }
    for (const l of this.listeners) l(m);
  }

  private send(m: Message) {
    if (!this.exited) this.child.stdin!.write(JSON.stringify(m) + "\n");
  }

  /** Listens to what Codex notifies; the returned function stops. */
  on(listener: (m: Message) => void): () => void {
    this.listeners.push(listener);
    return () =>
      (this.listeners = this.listeners.filter((l) => l !== listener));
  }

  request<T = any>(method: string, params: unknown, ms = START_MS): Promise<T> {
    if (this.exited) return Promise.reject(this.exited);
    const id = ++this.next;
    return new Promise<T>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.waiting.delete(id);
        reject(new Error(`Codex didn't answer ${method} in ${ms / 1000} s`));
      }, ms);
      this.waiting.set(id, {
        resolve: (v) => (clearTimeout(timer), resolve(v)),
        reject: (e) => (clearTimeout(timer), reject(e)),
      });
      this.send({ id, method, params });
    });
  }

  notify(method: string, params?: unknown) {
    this.send({ method, ...(params === undefined ? {} : { params }) });
  }

  /** Initializes the connection: Codex's version, from its user agent. */
  async initialize(): Promise<string | undefined> {
    const init = await this.request<{ userAgent?: string }>("initialize", {
      clientInfo: {
        name: "helix_foundry_agent_host",
        title: "Helix Foundry",
        version: "1",
      },
      capabilities: { experimentalApi: false, requestAttestation: false },
    });
    this.notify("initialized");
    return /\/(\d+\.\d+\.\d+[\w.+-]*)/.exec(init?.userAgent ?? "")?.[1];
  }

  /** Ends it: asked to, then made to. */
  close(): Promise<void> {
    if (this.child.exitCode !== null || this.child.signalCode !== null)
      return Promise.resolve();
    return new Promise((resolve) => {
      const kill = setTimeout(() => this.child.kill("SIGKILL"), 3000);
      this.child.once("exit", () => (clearTimeout(kill), resolve()));
      this.child.stdin!.end();
      this.child.kill("SIGTERM");
    });
  }
}

/**
 * Whether version is at least min, both x.y.z; a prerelease of min is
 * older than min.
 *
 * @example
 * atLeast("0.170.1", "0.169.0"); // true
 * atLeast("0.169.0-alpha.2", "0.169.0"); // false
 */
export function atLeast(version: string, min: string): boolean {
  const parse = (v: string) => {
    const m = /^(\d+)\.(\d+)\.(\d+)(-.+)?$/.exec(v.trim());
    return m ? { n: [+m[1]!, +m[2]!, +m[3]!], pre: !!m[4] } : undefined;
  };
  const a = parse(version),
    b = parse(min);
  if (!a || !b) return false;
  for (let i = 0; i < 3; i++) if (a.n[i] !== b.n[i]) return a.n[i]! > b.n[i]!;
  return !a.pre || b.pre;
}

/** A plan's name as Foundry keeps it: printable ASCII, at most 32 bytes. */
const plan = (s: string) =>
  [...s]
    .filter((c) => c >= " " && c <= "~")
    .join("")
    .slice(0, 32);

/** A version as Foundry keeps it: printable ASCII, at most 64 bytes. */
const label = (s?: string) =>
  s
    ? [...s]
        .filter((c) => c >= " " && c <= "~")
        .join("")
        .slice(0, 64)
    : undefined;

/** What account/read answers (v2 GetAccountResponse), read without trust. */
export type CodexAccountRead = {
  account?: { type?: string; planType?: string } | null;
  requiresOpenaiAuth?: boolean;
};

/**
 * Codex's account as Foundry is told it: on a ChatGPT plan (its plan type),
 * signed in some other way (an API key or a cloud's, whose turns don't bill
 * the plan), or signed out; with its version.
 *
 * @example
 * codexAccount({ account: { type: "chatgpt", planType: "plus" } }, "0.169.0");
 * // { runtime: "codex", state: "signed_in", subscription: "plus", version: "0.169.0" }
 * codexAccount({ account: null, requiresOpenaiAuth: true }); // { runtime: "codex", state: "signed_out" }
 */
export function codexAccount(
  read: CodexAccountRead,
  version?: string,
): Account {
  const v = label(version);
  const withVersion = <T extends Account>(a: T): T =>
    v ? { ...a, version: v } : a;
  const a = read.account;
  if (!a)
    return withVersion(
      read.requiresOpenaiAuth === false
        ? { runtime: "codex", state: "signed_in" }
        : { runtime: "codex", state: "signed_out" },
    );
  const p = a.type === "chatgpt" ? plan(a.planType ?? "") : "";
  return withVersion(
    p && p !== "unknown"
      ? { runtime: "codex", state: "signed_in", subscription: p }
      : { runtime: "codex", state: "signed_in" },
  );
}

const exec = promisify(execFile);

/** Codex's version, as `codex --version` says it ("codex-cli 0.169.0"). */
export async function codexVersion(
  codex: string,
  env: NodeJS.ProcessEnv,
): Promise<string | undefined> {
  const { stdout } = await exec(codex, ["--version"], {
    env: codexEnv(env),
    timeout: 20_000,
  });
  return /(\d+\.\d+\.\d+[\w.+-]*)/.exec(stdout)?.[1];
}

/** Starts an app-server with nothing of a turn's, in the person's home: to read its account or its threads. */
async function plainServer(
  codex: string,
  env: NodeJS.ProcessEnv,
): Promise<{ server: AppServer; version?: string }> {
  const server = new AppServer(codex, [], env.HOME || homedir(), codexEnv(env));
  try {
    return { server, version: await server.initialize() };
  } catch (e) {
    await server.close();
    throw e;
  }
}

/**
 * What of the helper's settings this Codex doesn't hold (unheld), as a turn
 * would find it: its effective configuration under LOCKED, and an ephemeral
 * thread's settings, in an empty directory. For doctor; a turn checks its own.
 */
export async function checkLocked(
  codex: string,
  env: NodeJS.ProcessEnv,
): Promise<string[]> {
  const dir = await mkdtemp(join(tmpdir(), "foundry-codex-"));
  const server = new AppServer(
    codex,
    LOCKED.flatMap(([k, v]) => ["-c", `${k}=${JSON.stringify(v)}`]),
    dir,
    codexEnv(env),
  );
  try {
    await server.initialize();
    const read = await server.request<{ config?: unknown }>("config/read", {
      cwd: dir,
      includeLayers: false,
    });
    const thread = await server.request("thread/start", {
      cwd: dir,
      ...THREAD,
      ephemeral: true,
    });
    return unheld(read?.config ?? {}, thread);
  } finally {
    await server.close();
    await rm(dir, { recursive: true, force: true });
  }
}

/** Codex's account as it says it, and its version: unknown when it didn't start. */
export async function checkCodex(
  codex: string,
  env: NodeJS.ProcessEnv,
): Promise<Account> {
  let version: string | undefined;
  try {
    version = await codexVersion(codex, env);
  } catch {
    return { runtime: "codex", state: "unknown" };
  }
  try {
    const { server, version: said } = await plainServer(codex, env);
    try {
      return codexAccount(
        await server.request<CodexAccountRead>("account/read", {
          refreshToken: false,
        }),
        said ?? version,
      );
    } finally {
      await server.close();
    }
  } catch {
    return label(version)
      ? { runtime: "codex", state: "unknown", version: label(version) }
      : { runtime: "codex", state: "unknown" };
  }
}

/** Every source a Codex thread can have (0.169.0's ThreadSourceKind): thread/list lists interactive ones alone unless told. */
export const THREAD_SOURCES = [
  "cli",
  "vscode",
  "exec",
  "appServer",
  "subAgent",
  "subAgentReview",
  "subAgentCompact",
  "subAgentThreadSpawn",
  "subAgentOther",
  "unknown",
];

/** Codex's threads on this computer, as its app-server lists and deletes them: those started in a thread's directory. */
export function codexSessions(codex: string, env: NodeJS.ProcessEnv): Sessions {
  return {
    async list(dir) {
      const { server } = await plainServer(codex, env);
      try {
        const ids: string[] = [];
        // Archived or not, and of every source: a sub-agent's thread holds
        // its parent's history, Foundry's tool answers included.
        for (const archived of [false, true]) {
          let cursor: string | null | undefined;
          do {
            const page = await server.request<{
              data?: { id: string }[];
              nextCursor?: string | null;
            }>("thread/list", {
              cwd: dir,
              cursor,
              limit: 100,
              archived,
              sourceKinds: THREAD_SOURCES,
            });
            ids.push(...(page.data ?? []).map((t) => t.id));
            cursor = page.nextCursor;
          } while (cursor);
        }
        return [...new Set(ids)];
      } finally {
        await server.close();
      }
    },
    async delete(id) {
      const { server } = await plainServer(codex, env);
      try {
        await server.request("thread/delete", { threadId: id });
      } finally {
        await server.close();
      }
    },
  };
}

/** Codex's failure, as Foundry takes it, from its turn's error. */
export function codexFailure(error: {
  message?: string;
  codexErrorInfo?: unknown;
}): End {
  const info =
    typeof error.codexErrorInfo === "string"
      ? error.codexErrorInfo
      : error.codexErrorInfo && typeof error.codexErrorInfo === "object"
        ? (Object.keys(error.codexErrorInfo)[0] ?? "")
        : "";
  const words = error.message ?? "";
  switch (info) {
    case "usageLimitExceeded":
    case "rateLimitExceeded":
      return { end: "failed", failure: "rate_limited" };
    case "unauthorized":
      return { end: "failed", failure: "signed_out" };
    case "contextWindowExceeded":
      return { end: "stopped", reason: "context" };
  }
  if (
    /(codex login|not logged in|sign in|log in|unauthorized|401)/i.test(words)
  )
    return { end: "failed", failure: "signed_out" };
  if (/(usage limit|rate limit|429)/i.test(words))
    return { end: "failed", failure: "rate_limited" };
  return {
    end: "failed",
    failure: "cli_error",
    detail: (words || info || "Codex failed").slice(0, 1000),
  };
}

/** Items that are a tool's: the model is called again after one. */
const TOOL_ITEMS = new Set([
  "mcpToolCall",
  "dynamicToolCall",
  "commandExecution",
  "fileChange",
  "collabAgentToolCall",
  "webSearch",
  "imageView",
  "imageGeneration",
]);

/**
 * Runs a turn on Codex: on the thread's Codex thread, or a new one told the
 * thread in words should Codex have lost it. Steps are its model calls, as
 * its items show them: one starts at the turn and after each tool's item.
 */
export async function runCodex(
  turn: Turn,
  opts: {
    codex: string;
    cwd: string;
    mcpURL: string;
    abort: AbortController;
    env?: NodeJS.ProcessEnv;
  },
  report: Reporter,
): Promise<Ran> {
  const env = opts.env ?? process.env;
  const server = new AppServer(
    opts.codex,
    codexConfig(opts.mcpURL).flatMap((c) => ["-c", c]),
    opts.cwd,
    codexEnv(env, turn.mcp_token),
  );
  // Its model calls so far, and the Codex thread it runs on.
  const at = { steps: 0, threadId: "" };
  const ran = (end: End): Ran => ({
    end,
    steps: at.steps,
    sessionId: at.threadId,
  });
  // Foundry's MCP server's start, by thread: Codex starts it for each thread.
  const started = new Map<string, string>();
  let onStart: () => void = () => {};
  const stopListening = server.on((m) => {
    if (
      m.method === "mcpServer/startupStatus/updated" &&
      m.params?.name === CODEX_SERVER
    ) {
      started.set(m.params.threadId, m.params.status);
      onStart();
    }
  });
  const abort = () => void server.close();
  opts.abort.signal.addEventListener("abort", abort, { once: true });
  // Codex's own words, never the turn's token, should it have written it.
  const said = (message: string) => {
    const tail = (
      turn.mcp_token
        ? server.stderr.split(turn.mcp_token).join("hxr_…")
        : server.stderr
    )
      .trim()
      .slice(-800);
    return tail ? `${message}: ${tail}` : message;
  };
  try {
    let version: string | undefined;
    try {
      version = await server.initialize();
    } catch (e) {
      const err = e as NodeJS.ErrnoException;
      if (err.code === "ENOENT" || /ENOENT/.test(err.message))
        return ran({ end: "failed", failure: "not_installed" });
      // Its last words, not just its exit code: a broken config.toml says why.
      await new Promise((r) => setTimeout(r, 100));
      return ran(codexFailure({ message: said(err.message) }));
    }
    // Foundry checks the version it was last told; this one is the version that runs.
    if (!atLeast(version ?? "", MIN_CODEX))
      return ran({
        end: "failed",
        failure: "cli_error",
        detail: `Codex ${version ?? "of an unknown version"} is older than ${MIN_CODEX}, the oldest Foundry runs: update it`,
      });
    try {
      const read = await server.request<{ config?: unknown }>("config/read", {
        cwd: opts.cwd,
        includeLayers: false,
      });
      const off = unheld(read?.config ?? {});
      if (off.length)
        return ran({
          end: "failed",
          failure: "cli_error",
          detail: unheldDetail(off),
        });
    } catch (e) {
      return ran(codexFailure({ message: said((e as Error).message) }));
    }
    let resumed = false;
    let thread: unknown;
    if (turn.resume) {
      try {
        thread = await server.request<{ thread: { id: string } }>(
          "thread/resume",
          { threadId: turn.resume, ...threadSettings(turn, opts.cwd) },
        );
        at.threadId = (thread as { thread: { id: string } }).thread.id;
        resumed = true;
      } catch {
        // Lost (deleted, or another computer's): start afresh, told the thread.
      }
    }
    if (!at.threadId) {
      try {
        thread = await server.request<{ thread: { id: string } }>(
          "thread/start",
          threadSettings(turn, opts.cwd),
        );
        at.threadId = (thread as { thread: { id: string } }).thread.id;
      } catch (e) {
        return ran(codexFailure({ message: said((e as Error).message) }));
      }
    }
    const off = unheld(undefined, thread);
    if (off.length)
      return ran({ end: "failed", failure: "cli_error", detail: unheldDetail(off) });
    // The model's first call never runs without Foundry's tools.
    const status = await new Promise<string>((resolve) => {
      const timer = setTimeout(() => resolve("timeout"), START_MS);
      onStart = () => {
        const s = started.get(at.threadId);
        if (s === "ready" || s === "failed" || server.exited) {
          clearTimeout(timer);
          resolve(s ?? "exited");
        }
      };
      onStart();
    });
    if (status !== "ready")
      return ran({
        end: "failed",
        failure: "cli_error",
        detail: "Codex couldn't reach Foundry's tools",
      });
    return ran(
      await turnOn(server, turn, at, resumed, report, opts.abort.signal),
    );
  } finally {
    opts.abort.signal.removeEventListener("abort", abort);
    stopListening();
    await server.close();
  }
}

/** The turn itself, on a thread Codex has open with Foundry's tools: how it ended, counting its steps in at. */
async function turnOn(
  server: AppServer,
  turn: Turn,
  at: { steps: number; threadId: string },
  resumed: boolean,
  report: Reporter,
  aborted: AbortSignal,
): Promise<End> {
  const threadId = at.threadId;
  let turnId = "";
  let overSteps = false;
  // Whether the next item starts a model call: at the turn's start, and
  // after a tool's item.
  let fresh = true;
  // Each final text, in order: awaited before the turn ends.
  const saying: Promise<void>[] = [];
  const ended = new Promise<{ status: string; error?: any } | undefined>(
    (resolve) => {
      const stop = server.on((m) => {
        if (m.method === "helper/exited") return (stop(), resolve(undefined));
        const p = m.params ?? {};
        // A sub-agent's thread is the model's own: only the turn's thread shows.
        if (p.threadId !== threadId) return;
        switch (m.method) {
          case "item/started": {
            if (p.item?.type === "userMessage" || !fresh) break;
            fresh = false;
            if (at.steps >= turn.max_steps) {
              overSteps = true;
              void server
                .request("turn/interrupt", {
                  threadId,
                  turnId: p.turnId ?? turnId,
                })
                .catch(() => {});
              break;
            }
            at.steps++;
            break;
          }
          case "item/agentMessage/delta":
            if (!overSteps && typeof p.delta === "string" && p.delta)
              report.delta(Math.max(1, at.steps), p.delta);
            break;
          case "item/completed": {
            const item = p.item ?? {};
            const text = typeof item.text === "string" ? item.text : "";
            if (item.type === "agentMessage" && !overSteps && text.trim()) {
              const said = report.said(Math.max(1, at.steps), text);
              said.catch(() => {});
              saying.push(said);
            }
            if (TOOL_ITEMS.has(item.type)) fresh = true;
            break;
          }
          case "turn/completed":
            stop();
            resolve(p.turn ?? { status: "failed" });
            break;
        }
      });
    },
  );
  try {
    const r = await server.request<{ turn: { id: string } }>("turn/start", {
      threadId,
      input: [
        { type: "text", text: promptText(turn, resumed), text_elements: [] },
      ],
    });
    turnId = r.turn.id;
  } catch (e) {
    return codexFailure({ message: (e as Error).message });
  }
  const done = await ended;
  await Promise.all(saying);
  if (overSteps) return { end: "stopped", reason: "max_steps" };
  if (!done)
    return {
      end: "failed",
      failure: "cli_error",
      detail: aborted.aborted ? "stopped" : "Codex stopped without an answer",
    };
  switch (done.status) {
    case "completed":
      return { end: "answered" };
    case "interrupted":
      return {
        end: "failed",
        failure: "cli_error",
        detail: "Codex was interrupted",
      };
  }
  return codexFailure(done.error ?? {});
}

/** Codex at codex, as the loop drives it: sessions are a test's when given. */
export function codexCli(
  codex: string,
  env: NodeJS.ProcessEnv,
  opts: { sessions?: Sessions } = {},
): Cli {
  return {
    runtime: "codex",
    account: () => checkCodex(codex, env),
    run: (turn, ctx, report) => runCodex(turn, { codex, env, ...ctx }, report),
    sessions: opts.sessions ?? codexSessions(codex, env),
  };
}
