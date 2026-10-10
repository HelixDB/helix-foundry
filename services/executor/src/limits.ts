import type { Execution } from "./engine.js";
// How long the executor may take to answer a job, from when it receives the
// job until it replies, so time queued behind other jobs counts too. The API
// reads the same settings and waits a little longer, so the executor always
// answers first.
//
// A query answers someone who is waiting (a dashboard, a question, a check
// while building), so it keeps EXECUTOR_QUERY_TIMEOUT, two minutes by default.
// Imports, pipelines and publication read or write whole tables, so they get
// EXECUTOR_JOB_TIMEOUT, 15 minutes by default. Both are in seconds.
const settings = {
  query: ["EXECUTOR_QUERY_TIMEOUT", 120],
  job: ["EXECUTOR_JOB_TIMEOUT", 15 * 60],
} as const;
export type TimeLimit = { ms: number; setting: string };
export function timeLimit(
  job: Partial<Pick<Execution, "mode" | "persist">>,
): TimeLimit {
  const [setting, fallback] =
    settings[job.mode === "query" && !job.persist ? "query" : "job"];
  const value = process.env[setting]?.trim();
  const seconds = value ? Number(value) : fallback;
  // Timers fire at once past 2^31 - 1 ms, so a day is the most allowed.
  if (!(seconds > 0 && seconds <= 86400))
    throw new Error(`Invalid ${setting}: ${value} (seconds, from 1 to 86400)`);
  return { ms: seconds * 1000, setting };
}
// "15 minutes", "90 seconds".
export function duration(ms: number) {
  const unit = ms % 60000 ? ["second", 1000] : ["minute", 60000];
  const n = ms / (unit[1] as number);
  return `${n} ${unit[0]}${n === 1 ? "" : "s"}`;
}
