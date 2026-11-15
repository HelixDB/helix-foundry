// What stands between this computer and running Foundry's Assistant on its
// Claude Code: each check in a line, and what to do about one that fails.

import { execFile } from "node:child_process";
import { stat } from "node:fs/promises";
import { promisify } from "node:util";

import type { AccountInfo } from "@anthropic-ai/claude-agent-sdk";

import { claudeAccount, signedIn } from "./account.ts";
import { cliEnv, type Query } from "./claude.ts";
import { configPath, loadPaired } from "./config.ts";
import { findClaude } from "./find.ts";
import { Foundry, type Account } from "./foundry.ts";
import { VERSION } from "./version.ts";

export type Check = { ok: boolean; what: string; fix?: string };

const run = promisify(execFile);

/**
 * Every check, in order; a later one may depend on an earlier. Claude Code
 * is asked first, so Foundry's hello says what it is signed in to.
 */
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
  const cli = await claudeChecks(opts.claude, env, opts.query);

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
        [cli.account],
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
  return [...checks, ...cli.checks];
}

/** Claude Code's checks, and its account as Foundry is told it. */
async function claudeChecks(
  path: string | undefined,
  env: NodeJS.ProcessEnv,
  query?: Query,
): Promise<{ checks: Check[]; account: Account }> {
  const unknown: Account = { runtime: "claude_code", state: "unknown" };
  const claude = await findClaude(path, env);
  if (!claude)
    return {
      account: unknown,
      checks: [
        {
          ok: false,
          what: "Claude Code installed",
          fix: "Install Claude Code (https://claude.com/claude-code), or name it with --claude <path>.",
        },
      ],
    };
  const checks: Check[] = [];
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
    return { checks, account: unknown };
  }
  let who: AccountInfo;
  try {
    who = await signedIn(claude, env, query);
  } catch (e) {
    checks.push({
      ok: false,
      what: "Claude Code starts",
      fix: (e as Error).message,
    });
    return { checks, account: unknown };
  }
  const account = claudeAccount(who);
  checks.push(
    account.state === "signed_in" && account.subscription
      ? {
          ok: true,
          what: `Signed in to a Claude ${account.subscription} subscription${who.email ? ` as ${who.email}` : ""}`,
        }
      : {
          ok: false,
          what:
            account.state === "signed_in"
              ? "Signed in to Claude Code, but not on a Claude subscription"
              : "Signed in to a Claude subscription",
          fix: "Run claude, then /login, and sign in with your Claude account.",
        },
  );
  return { checks, account };
}
