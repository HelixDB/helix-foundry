// What stands between this computer and running Foundry's Assistant on its
// Claude Code or Codex: each check in a line, and what to do about one that
// fails.

import { execFile } from "node:child_process";
import { stat } from "node:fs/promises";
import { promisify } from "node:util";

import type { AccountInfo } from "@anthropic-ai/claude-agent-sdk";

import { claudeAccount, signedIn } from "./account.ts";
import { cliEnv, type Query } from "./claude.ts";
import { MIN_CODEX, atLeast, checkCodex, checkLocked } from "./codex.ts";
import { configPath, loadPaired } from "./config.ts";
import { findCli } from "./find.ts";
import { Foundry, type Account } from "./foundry.ts";
import { VERSION } from "./version.ts";

export type Check = { ok: boolean; what: string; fix?: string };

const run = promisify(execFile);

/**
 * Every check, in order; a later one may depend on an earlier. The CLIs are
 * asked first, so Foundry's hello says what they are signed in to.
 */
export async function doctor(
  opts: {
    claude?: string;
    codex?: string;
    env?: NodeJS.ProcessEnv;
    query?: Query;
  } = {},
): Promise<Check[]> {
  const env = opts.env ?? process.env;
  const checks: Check[] = [];
  const major = Number(process.versions.node.split(".")[0]);
  checks.push({
    ok: major >= 24,
    what: `Node ${process.versions.node}`,
    fix: "Install Node 24 or later.",
  });
  const claude = await findCli("claude", opts.claude, env);
  const codex = await findCli("codex", opts.codex, env);
  // A CLI is checked once found, or once named: one named but not found is a problem.
  const clis = [
    ...(claude || opts.claude
      ? [{ found: !!claude, ...(await claudeChecks(claude, env, opts.query)) }]
      : []),
    ...(codex || opts.codex
      ? [{ found: !!codex, ...(await codexChecks(codex, env)) }]
      : []),
  ];
  if (clis.length === 0)
    checks.push({
      ok: false,
      what: "Claude Code or Codex installed",
      fix: "Install Claude Code (https://claude.com/claude-code) or Codex (npm i -g @openai/codex), or name one with --claude <path> or --codex <path>.",
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
      const found = clis.filter((c) => c.found);
      const me = await new Foundry(paired.foundry, paired.token).hello(
        found.length ? found.map((c) => c.account.runtime) : ["claude_code"],
        VERSION,
        found.map((c) => c.account),
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
  return [...checks, ...clis.flatMap((c) => c.checks)];
}

/** Claude Code's checks, and its account as Foundry is told it. */
async function claudeChecks(
  claude: string | undefined,
  env: NodeJS.ProcessEnv,
  query?: Query,
): Promise<{ checks: Check[]; account: Account }> {
  const unknown: Account = { runtime: "claude_code", state: "unknown" };
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

/** Codex's checks, and its account as Foundry is told it. */
async function codexChecks(
  codex: string | undefined,
  env: NodeJS.ProcessEnv,
): Promise<{ checks: Check[]; account: Account }> {
  if (!codex)
    return {
      account: { runtime: "codex", state: "unknown" },
      checks: [
        {
          ok: false,
          what: "Codex installed",
          fix: "Install Codex (npm i -g @openai/codex), or name it with --codex <path>.",
        },
      ],
    };
  const account = await checkCodex(codex, env);
  const checks: Check[] = [];
  if (!account.version)
    return {
      account,
      checks: [
        {
          ok: false,
          what: `Codex at ${codex}`,
          fix: "It didn't run: run codex --version there to see why.",
        },
      ],
    };
  checks.push(
    atLeast(account.version, MIN_CODEX)
      ? { ok: true, what: `Codex ${account.version} (${codex})` }
      : {
          ok: false,
          what: `Codex ${account.version} is older than ${MIN_CODEX}, the oldest Foundry runs (${codex})`,
          fix: "Update it: npm i -g @openai/codex@latest, or as you installed it.",
        },
  );
  if (atLeast(account.version, MIN_CODEX))
    checks.push(
      await checkLocked(codex, env).then(
        (off): Check =>
          off.length === 0
            ? {
                ok: true,
                what: "Codex runs as Foundry sets it: no shell, read-only, every action asking",
              }
            : {
                ok: false,
                what: `Codex runs as Foundry sets it: your configuration turns back on ${off.join(", ")}`,
                fix: "Most likely a managed configuration (/etc/codex/managed_config.toml, or MDM on macOS) sets it, and it outranks Foundry's: Foundry runs no turn on this Codex while it does. Ask whoever manages this computer, or use Claude Code.",
              },
        (e: Error): Check => ({
          ok: false,
          what: "Codex runs as Foundry sets it",
          fix: `Its app-server didn't say: ${e.message}`,
        }),
      ),
    );
  checks.push(
    account.state === "signed_in" && account.subscription
      ? {
          ok: true,
          what: `Signed in to Codex on a ChatGPT ${account.subscription} plan`,
        }
      : account.state === "unknown"
        ? {
            ok: false,
            what: "Codex starts",
            fix: "Its app-server didn't answer: run codex there to see why.",
          }
        : {
            ok: false,
            what:
              account.state === "signed_in"
                ? "Signed in to Codex, but not with a ChatGPT plan"
                : "Signed in to Codex with a ChatGPT plan",
            fix: "Run codex login, and sign in with ChatGPT.",
          },
  );
  return { checks, account };
}
