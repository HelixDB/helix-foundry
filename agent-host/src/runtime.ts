// What a helper's runtime is to the loop (run.ts): a CLI on this computer
// that runs a turn, says what it is signed in to, and keeps sessions the
// helper deletes once their thread is gone. Claude Code's is claude.ts's,
// Codex's codex.ts's.

import type { Sessions } from "./config.ts";
import type { Account, End, Runtime, Turn } from "./foundry.ts";

/**
 * What a turn reports as it runs: the model's text, by step (its model
 * calls, from 1). A step's text is said once it is final: when the step
 * goes on to a tool call (so it shows before the call, as the model wrote
 * it), when its call ends, or when the next starts. Text after a tool call
 * in the same step is said again, after it.
 */
export type Reporter = {
  /** Text the model is writing in step, as it comes. */
  delta(step: number, text: string): void;
  /** The step's text so far, final: it replaces the step's deltas. */
  said(step: number, text: string): Promise<void>;
};

/** How a turn ran: how it ended, its model calls, and its session. */
export type Ran = { end: End; steps: number; sessionId: string };

/** A runtime's CLI, as the loop drives it. */
export type Cli = {
  runtime: Runtime;
  /** What it is signed in to: unknown when it didn't start. */
  account(): Promise<Account>;
  /** Runs a turn in cwd, its tools Foundry's at mcpURL, until abort. */
  run(
    turn: Turn,
    ctx: { cwd: string; mcpURL: string; abort: AbortController },
    report: Reporter,
  ): Promise<Ran>;
  /** Its sessions under a thread's directory. */
  sessions: Sessions;
};

/** What the person said, as the CLI's first message: with the thread in words when it starts afresh. */
export function promptText(turn: Turn, resumed: boolean): string {
  return !resumed && turn.history
    ? `${turn.history}\n\n---\n\n${turn.prompt}`
    : turn.prompt;
}
