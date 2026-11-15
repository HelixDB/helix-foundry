// What the person's Claude Code is signed in to, as Foundry's owner sees it
// in Settings › AI models: the helper asks its CLI as it starts, every
// CHECK_MS after, and takes a turn that failed signed out at its word.

import {
  query,
  type AccountInfo,
  type SDKUserMessage,
} from "@anthropic-ai/claude-agent-sdk";

import { cliEnv, type Query } from "./claude.ts";
import type { Account } from "./foundry.ts";

/** How often a running helper asks its CLI again. */
export const CHECK_MS = 10 * 60_000;
/** How long the CLI has to say who it is. */
export const ASK_MS = 30_000;
/** A subscription's name as Foundry keeps it: printable, at most 32 bytes. */
const MAX_SUBSCRIPTION = 32;

/** Whom Claude Code is signed in as, as it says when it starts. */
export async function signedIn(
  claude: string,
  env: NodeJS.ProcessEnv,
  run: Query = query,
  ms = ASK_MS,
): Promise<AccountInfo> {
  // A prompt that never comes: the CLI starts, signs in and says who it is.
  const never = new Promise<never>(() => {});
  async function* prompt(): AsyncGenerator<SDKUserMessage> {
    yield await never;
  }
  const abort = new AbortController();
  const q = run({
    prompt: prompt(),
    options: {
      pathToClaudeCodeExecutable: claude,
      tools: [],
      settingSources: [],
      strictMcpConfig: true,
      mcpServers: {},
      persistSession: false,
      abortController: abort,
      env: cliEnv(env),
      stderr: () => {},
    },
  });
  let timer: NodeJS.Timeout | undefined;
  try {
    const init = await Promise.race([
      q.initializationResult(),
      new Promise<never>((_, reject) => {
        timer = setTimeout(
          () => reject(new Error(`Claude Code didn't start in ${ms / 1000} s`)),
          ms,
        );
      }),
    ]);
    return init.account ?? {};
  } finally {
    clearTimeout(timer);
    abort.abort();
    q.close();
  }
}

/**
 * The CLI's account as Foundry is told it: on a subscription (its plan),
 * signed in some other way (an API account, whose turns don't bill the
 * subscription), or signed out.
 *
 * @example
 * claudeAccount({ subscriptionType: "max", email: "ada@example.com" });
 * // { runtime: "claude_code", state: "signed_in", subscription: "max" }
 * claudeAccount({}); // { runtime: "claude_code", state: "signed_out" }
 */
export function claudeAccount(info: AccountInfo): Account {
  const plan = (info.subscriptionType ?? "")
    .replace(/[\x00-\x1f\x7f]/g, "")
    .slice(0, MAX_SUBSCRIPTION);
  if (plan)
    return { runtime: "claude_code", state: "signed_in", subscription: plan };
  const source = (s?: string) => !!s && s !== "none";
  return info.email || source(info.tokenSource) || source(info.apiKeySource)
    ? { runtime: "claude_code", state: "signed_in" }
    : { runtime: "claude_code", state: "signed_out" };
}

/** Claude Code's account: unknown when its CLI didn't start. */
export async function checkAccount(
  claude: string,
  env: NodeJS.ProcessEnv,
  run?: Query,
): Promise<Account> {
  try {
    return claudeAccount(await signedIn(claude, env, run));
  } catch {
    return { runtime: "claude_code", state: "unknown" };
  }
}
