// The helper's loop: claim a turn, run it on Claude Code while renewing its
// lease, show the person what the model writes, and say how it ended.
// Foundry decides everything else: which turn, its caps, the tools and the
// cards. A turn whose lease Foundry took back (a cancel, a deleted thread,
// its wall time) is stopped at once and reported no further.

import { CHECK_MS, checkAccount } from "./account.ts";
import { runClaude, type Query, type Ran } from "./claude.ts";
import { threadDir } from "./config.ts";
import {
  Foundry,
  FoundryError,
  type Account,
  type End,
  type Runtime,
  type Turn,
} from "./foundry.ts";

/** How often the lease is renewed: well inside its minute. */
export const RENEW_MS = 15_000;
/** How often the text the model is writing is shown. */
export const DELTA_MS = 250;
/** How long after a failed claim the helper tries again. */
const RETRY_MS = 5_000;

export type RunOptions = {
  foundry: Foundry;
  /** The `claude` executable. */
  claude: string;
  version: string;
  /** Turns run at once: one claim each. */
  concurrency: number;
  env?: NodeJS.ProcessEnv;
  query?: Query;
  log?: (line: string) => void;
  signal?: AbortSignal;
  renewMs?: number;
  deltaMs?: number;
  /** How often the CLI is asked what it is signed in to (CHECK_MS). */
  checkMs?: number;
  /** Told how each turn it ran ended. */
  ended?: (end: End) => void;
};

const RUNTIMES: Runtime[] = ["claude_code"];

/**
 * Runs turns until signal aborts. A token Foundry no longer knows (the
 * helper was revoked) stops the helper with that error. Every call says
 * what the CLI is signed in to: asked as it starts and every checkMs, and
 * signed out once a turn failed so.
 */
export async function serve(opts: RunOptions): Promise<void> {
  const log = opts.log ?? (() => {});
  const ask = () => checkAccount(opts.claude, opts.env ?? process.env, opts.query);
  const accounts: Account[] = [await ask()];
  const me = await opts.foundry.hello(RUNTIMES, opts.version, accounts);
  log(
    `Paired as ${me.name}: running Foundry's Assistant on Claude Code, ${opts.concurrency} turn${opts.concurrency === 1 ? "" : "s"} at a time.`,
  );
  if (accounts[0]!.state === "signed_out")
    log("Claude Code isn't signed in: run claude, then /login.");
  const checking = setInterval(
    () => void ask().then((a) => (accounts[0] = a)),
    opts.checkMs ?? CHECK_MS,
  );
  const ran: RunOptions = {
    ...opts,
    ended(end) {
      if (end.end === "failed" && end.failure === "signed_out")
        accounts[0] = { runtime: "claude_code", state: "signed_out" };
      // A turn that ran says the person signed in since: ask again.
      else if (end.end === "answered" && accounts[0]!.state !== "signed_in")
        void ask().then((a) => (accounts[0] = a));
      opts.ended?.(end);
    },
  };
  try {
    await Promise.all(
      Array.from({ length: opts.concurrency }, () => slot(ran, accounts, log)),
    );
  } finally {
    clearInterval(checking);
  }
}

async function slot(
  opts: RunOptions,
  accounts: Account[],
  log: (line: string) => void,
): Promise<void> {
  while (!opts.signal?.aborted) {
    let turn: Turn | undefined;
    try {
      turn = await opts.foundry.claim(
        RUNTIMES,
        opts.version,
        accounts,
        opts.signal,
      );
    } catch (e) {
      if (opts.signal?.aborted) return;
      if (e instanceof FoundryError && e.code === "unauthenticated") throw e;
      log(`Claiming a turn failed: ${(e as Error).message}; trying again.`);
      await sleep(RETRY_MS, opts.signal);
      continue;
    }
    if (!turn) continue;
    log(`Running turn ${turn.id} (attempt ${turn.attempt}).`);
    try {
      const outcome = await runTurn(turn, opts);
      log(`Turn ${turn.id}: ${outcome}.`);
    } catch (e) {
      log(`Turn ${turn.id} failed to report: ${(e as Error).message}.`);
    }
  }
}

/** Runs one claimed turn to its end, and says how it ended. */
export async function runTurn(turn: Turn, opts: RunOptions): Promise<string> {
  const abort = new AbortController();
  // Why Foundry took the turn back, once it did.
  let taken: string | undefined;
  const stop = (why: string) => {
    taken ??= why;
    abort.abort(why);
  };
  opts.signal?.addEventListener("abort", () => stop("the helper stopped"), {
    once: true,
  });
  const renewing = setInterval(() => {
    opts.foundry.renew(turn.id).catch((e) => {
      if (
        e instanceof FoundryError &&
        (e.code === "turn_lost" ||
          e.code === "wall_time" ||
          e.code === "unauthenticated")
      )
        stop(e.code);
    });
  }, opts.renewMs ?? RENEW_MS);
  const deadline = setTimeout(
    () => stop("wall_time"),
    Math.max(0, Date.parse(turn.deadline) - Date.now()),
  );
  // Live text, a step at a time, sent every deltaMs. Every post of text
  // goes in order, so a step's final text lands after its deltas.
  let pending = { step: 0, text: "" };
  let posts = Promise.resolve();
  // Live text is for show: a delta that can't be posted is dropped. A
  // final text's failure is its caller's.
  const post = (step: number, text: string, final: boolean) => {
    const sent = posts.then(() =>
      opts.foundry.text(turn.id, step, text, final),
    );
    posts = sent.catch(() => {});
    return final ? sent : posts;
  };
  const flush = () => {
    if (!pending.text) return posts;
    const { step, text } = pending;
    pending = { step, text: "" };
    return post(step, text, false);
  };
  const flushing = setInterval(() => void flush(), opts.deltaMs ?? DELTA_MS);
  let ran: Ran;
  try {
    ran = await runClaude(
      turn,
      {
        claude: opts.claude,
        cwd: await threadDir(turn.thread_id, opts.env),
        mcpURL: opts.foundry.mcpURL,
        abort,
        env: opts.env,
        query: opts.query,
      },
      {
        delta(step, text) {
          if (step !== pending.step) void flush();
          pending = {
            step,
            text: (step === pending.step ? pending.text : "") + text,
          };
        },
        async said(step, text) {
          pending = { step: 0, text: "" };
          await post(step, text, true);
        },
      },
    );
  } finally {
    clearInterval(renewing);
    clearInterval(flushing);
    clearTimeout(deadline);
  }
  if (taken) return `stopped: ${taken}`;
  await opts.foundry.finish(turn.id, ran.end, ran.steps, ran.sessionId);
  opts.ended?.(ran.end);
  return ran.end.end === "failed" ? `failed: ${ran.end.failure}` : ran.end.end;
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    signal?.addEventListener("abort", () => (clearTimeout(t), resolve()), {
      once: true,
    });
  });
}
