// What stands between this computer and running Foundry's Assistant on its
// Claude Code: each check in a line, and what to do about one that fails.

import { execFile } from "node:child_process";
import { stat } from "node:fs/promises";
import { promisify } from "node:util";

import { query, type SDKUserMessage } from "@anthropic-ai/claude-agent-sdk";

import { cliEnv, type Query } from "./claude.ts";
import { configPath, loadPaired } from "./config.ts";
import { findClaude } from "./find.ts";
import { Foundry } from "./foundry.ts";
import { VERSION } from "./version.ts";

export type Check = { ok: boolean; what: string; fix?: string };

const run = promisify(execFile);

/** Whom Claude Code is signed in as: its subscription's kind, if any. */
export async function signedIn(
  claude: string,
  env: NodeJS.ProcessEnv,
  run: Query = query,
): Promise<{ subscription?: string; email?: string }> {
  // A prompt that never comes: the CLI starts, signs in and says who it is.
  const never = new Promise<never>(() => {});
  async function* prompt(): AsyncGenerator<SDKUserMessage> {
    yield await never;
  }
  const q = run({
    prompt: prompt(),
    options: {
      pathToClaudeCodeExecutable: claude,
      tools: [],
      settingSources: [],
      strictMcpConfig: true,
      mcpServers: {},
      persistSession: false,
      env: cliEnv(env),
      stderr: () => {},
    },
  });
  try {
    const init = (await q.initializationResult()) as {
      account?: { subscriptionType?: string; email?: string };
    };
    return {
      subscription: init.account?.subscriptionType,
      email: init.account?.email,
    };
  } finally {
    q.close();
  }
}

/** Every check, in order; a later one may depend on an earlier. */
export async function doctor(
  opts: { claude?: string; env?: NodeJS.ProcessEnv; query?: Query } = {},
): Promise<Check[]> {
  const env = opts.env ?? process.env;
  const checks: Check[] = [];
  const major = Number(process.versions.node.split(".")[0]);
  checks.push({
    ok: major >= 24,
    what: `Node ${process.versions.node}`,
    fix: "Install Node 24 or later.",
  });

  const path = configPath(env);
  const paired = await loadPaired(path).catch((e: Error) => e);
  if (paired instanceof Error || !paired) {
    checks.push({
      ok: false,
      what: `Paired with Foundry (${path})`,
      fix:
        paired instanceof Error
          ? paired.message
          : "In Foundry, open Settings › AI models, choose your Claude subscription and run the pair command it shows.",
    });
  } else {
    const mode = (await stat(path)).mode & 0o777;
    checks.push({
      ok: (mode & 0o077) === 0,
      what: `Its token is readable by you alone (${path})`,
      fix: `chmod 600 ${path}`,
    });
    try {
      const me = await new Foundry(paired.foundry, paired.token).hello(
        ["claude_code"],
        VERSION,
      );
      checks.push({
        ok: true,
        what: `Foundry at ${paired.foundry} knows this computer as ${me.name}`,
      });
    } catch (e) {
      checks.push({
        ok: false,
        what: `Foundry at ${paired.foundry}`,
        fix: `${(e as Error).message}. Is Foundry running, with its port 8081 published on this computer?`,
      });
    }
  }

  const claude = await findClaude(opts.claude, env);
  if (!claude) {
    checks.push({
      ok: false,
      what: "Claude Code installed",
      fix: "Install Claude Code (https://claude.com/claude-code), or name it with --claude <path>.",
    });
    return checks;
  }
  try {
    const { stdout } = await run(claude, ["--version"], {
      env: cliEnv(env),
      timeout: 20_000,
    });
    checks.push({ ok: true, what: `Claude Code ${stdout.trim()} (${claude})` });
  } catch (e) {
    checks.push({
      ok: false,
      what: `Claude Code at ${claude}`,
      fix: `It didn't run: ${(e as Error).message}`,
    });
    return checks;
  }
  try {
    const who = await signedIn(claude, env, opts.query);
    checks.push({
      ok: !!who.subscription,
      what: who.subscription
        ? `Signed in to a Claude ${who.subscription} subscription${who.email ? ` as ${who.email}` : ""}`
        : "Signed in to a Claude subscription",
      fix: "Run claude, then /login, and sign in with your Claude account.",
    });
  } catch (e) {
    checks.push({
      ok: false,
      what: "Claude Code starts",
      fix: (e as Error).message,
    });
  }
  return checks;
}
