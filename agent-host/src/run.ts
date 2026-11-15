// The helper's loop: claim a turn of a runtime it runs, run it on that
// runtime's CLI (Claude Code, Codex) while renewing its lease, show the person what the model writes, and say how it ended.
// Foundry decides everything else: which turn, its caps, the tools and the
// cards. A turn whose lease Foundry took back (a cancel, a deleted thread,
// its wall time) is stopped at once and reported no further. A turn that
// reaches its wall time here is stopped, and Foundry told (one last
// renew), so it ends the turn then rather than once its lease lapses.
//
// What each CLI keeps of each thread (its sessions, with what Foundry's
// tools answered) is deleted once Foundry says the thread is gone: asked
// as the helper starts and every SWEEP_MS, and of every thread once the
// helper is revoked.

import { CHECK_MS } from "./account.ts";
import {
  forgetThread,
  listThreads,
  threadDir,
  type Sessions,
} from "./config.ts";
import {
  Foundry,
  FoundryError,
  GONE_BATCH,
  type Account,
  type End,
  type Turn,
} from "./foundry.ts";
import type { Cli, Ran } from "./runtime.ts";

/** How often the lease is renewed: well inside its minute. */
export const RENEW_MS = 15_000;
/** How often the text the model is writing is shown. */
export const DELTA_MS = 250;
/** How long after a failed claim the helper tries again. */
const RETRY_MS = 5_000;
/** How often the helper asks which threads it keeps sessions of are gone. */
export const SWEEP_MS = 60 * 60_000;

export type RunOptions = {
  foundry: Foundry;
  /** The CLIs it runs, one per runtime: at least one. */
  clis: Cli[];
  version: string;
  /** Turns run at once: one claim each. */
  concurrency: number;
  env?: NodeJS.ProcessEnv;
  log?: (line: string) => void;
  signal?: AbortSignal;
  renewMs?: number;
  deltaMs?: number;
  /** How often the CLIs are asked what they are signed in to (CHECK_MS). */
  checkMs?: number;
  /** Told how each turn it ran ended, and on which CLI. */
  ended?: (end: End, cli: Cli) => void;
  /** How often gone threads are looked for (SWEEP_MS). */
  sweepMs?: number;
};

/**
 * Runs turns until signal aborts. A token Foundry no longer knows (the
 * helper was revoked) stops the helper with that error, once it deleted
 * every thread's session. Every call says what the CLI is signed in to:
 * asked as it starts and every checkMs, and signed out once a turn failed
 * so.
 */
export async function serve(opts: RunOptions): Promise<void> {
  const log = opts.log ?? (() => {});
  const sessions = opts.clis.map((c) => c.sessions);
  try {
    await serving(opts, log);
  } catch (e) {
    if (e instanceof FoundryError && e.code === "unauthenticated") {
      const threads = await listThreads(opts.env);
      for (const t of threads) await forgetThread(t, sessions, opts.env);
      log(
        `Foundry no longer knows this helper (revoked): deleted what ${names(opts.clis)} kept of ${threads.length} thread${threads.length === 1 ? "" : "s"}. Pair it again to run more.`,
      );
    }
    throw e;
  }
}

/** The CLIs' names, as the log says them. */
const names = (clis: Cli[]) =>
  clis
    .map((c) => (c.runtime === "codex" ? "Codex" : "Claude Code"))
    .join(" and ");

/**
 * Deletes what the CLIs keep of the threads Foundry says are gone; a sweep
 * that fails is logged, and tried again at the next.
 */
export async function sweep(
  foundry: Foundry,
  sessions: Sessions[],
  env: NodeJS.ProcessEnv | undefined,
  log: (line: string) => void,
): Promise<void> {
  try {
    const threads = await listThreads(env);
    for (let i = 0; i < threads.length; i += GONE_BATCH) {
      const batch = threads.slice(i, i + GONE_BATCH);
      const gone = new Set(await foundry.gone(batch));
      for (const t of batch.filter((t) => gone.has(t.threadId)))
        await forgetThread(t, sessions, env);
    }
  } catch (e) {
    if (e instanceof FoundryError && e.code === "unauthenticated") throw e;
    log(
      `Looking for deleted threads failed: ${(e as Error).message}; trying again later.`,
    );
  }
}

async function serving(
  opts: RunOptions,
  log: (line: string) => void,
): Promise<void> {
  const sessions = opts.clis.map((c) => c.sessions);
  // Each CLI's account, by its index in opts.clis.
  const accounts: Account[] = await Promise.all(
    opts.clis.map((c) => c.account()),
  );
  const ask = (i: number) =>
    void opts.clis[i]!.account().then((a) => (accounts[i] = a));
  const runtimes = opts.clis.map((c) => c.runtime);
  const me = await opts.foundry.hello(runtimes, opts.version, accounts);
  log(
    `Paired as ${me.name}: running Foundry's Assistant on ${names(opts.clis)}, ${opts.concurrency} turn${opts.concurrency === 1 ? "" : "s"} at a time.`,
  );
  for (const a of accounts)
    if (a.state === "signed_out")
      log(
        a.runtime === "codex"
          ? "Codex isn't signed in: run codex login."
          : "Claude Code isn't signed in: run claude, then /login.",
      );
  const checking = setInterval(
    () => opts.clis.forEach((_, i) => ask(i)),
    opts.checkMs ?? CHECK_MS,
  );
  await sweep(opts.foundry, sessions, opts.env, log);
  const sweeping = setInterval(
    () => void sweep(opts.foundry, sessions, opts.env, log).catch(() => {}),
    opts.sweepMs ?? SWEEP_MS,
  );
  const ran: RunOptions = {
    ...opts,
    ended(end, cli) {
      const i = opts.clis.indexOf(cli);
      if (end.end === "failed" && end.failure === "signed_out")
        accounts[i] = {
          runtime: cli.runtime,
          state: "signed_out",
          ...(accounts[i]?.version && { version: accounts[i]!.version }),
        };
      // A turn that ran says the person signed in since: ask again.
      else if (end.end === "answered" && accounts[i]!.state !== "signed_in")
        ask(i);
      opts.ended?.(end, cli);
    },
  };
  try {
    await Promise.all(
      Array.from({ length: opts.concurrency }, () => slot(ran, accounts, log)),
    );
  } finally {
    clearInterval(checking);
    clearInterval(sweeping);
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
        opts.clis.map((c) => c.runtime),
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
    opts.foundry.renew(turn.claim_id).catch((e) => {
      if (
        e instanceof FoundryError &&
        (e.code === "turn_lost" ||
          e.code === "wall_time" ||
          e.code === "unauthenticated")
      )
        stop(e.code);
    });
  }, opts.renewMs ?? RENEW_MS);
  // Its wall time here: stopped, and Foundry told after (timedOut).
  let timedOut = false;
  const deadline = setTimeout(
    () => {
      timedOut = true;
      stop("wall_time");
    },
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
      opts.foundry.text(turn.claim_id, step, text, final),
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
  const cli = opts.clis.find((c) => c.runtime === turn.runtime);
  let ran: Ran;
  try {
    ran = cli
      ? await cli.run(
          turn,
          {
            cwd: await threadDir(
              { workspaceId: turn.workspace_id, threadId: turn.thread_id },
              opts.env,
            ),
            mcpURL: opts.foundry.mcpURL,
            abort,
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
        )
      : // Foundry leased a runtime this helper doesn't run: say so.
        {
          end: { end: "failed", failure: "not_installed" },
          steps: 0,
          sessionId: "",
        };
  } finally {
    clearInterval(renewing);
    clearInterval(flushing);
    clearTimeout(deadline);
  }
  if (timedOut) {
    // Past its wall time, a renew has Foundry end the turn (wall_time): the
    // person sees it stopped now, not once its lease lapses.
    await opts.foundry.renew(turn.claim_id).catch(() => {});
  }
  if (taken) return `stopped: ${taken}`;
  await opts.foundry.finish(turn.claim_id, ran.end, ran.steps, ran.sessionId);
  if (cli) opts.ended?.(ran.end, cli);
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
