import { spawn, type ChildProcess } from "node:child_process";
import { mkdir, mkdtemp, readdir, rm } from "node:fs/promises";
import { createRequire } from "node:module";
import { resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import type { Execution } from "./engine.js";
import { duration, timeLimit, type TimeLimit } from "./limits.js";
import { scratchRoot } from "./paths.js";
const runner = fileURLToPath(new URL("./runner.ts", import.meta.url));
// Resolved here, so the child does not depend on its working directory.
const tsx = pathToFileURL(createRequire(import.meta.url).resolve("tsx")).href;
// What the executor makes in scratch: a directory per job, and one per run of
// the engine in-process (tests and the benchmark). Nothing else there is
// touched.
const owned = /^(job|executor)-/;
// Jobs that ended with the executor, as in an out-of-memory kill, leave their
// spill files behind. Listed before the executor listens, when none of its
// jobs can be running, they are all stale; removing them once it listens
// spares the jobs of another executor on the same data, whose port this one
// then fails to take. Scratch itself stays: it may be a mount point. Failures
// are logged and never stop the executor from starting.
export async function staleScratch(root: string) {
  const dir = scratchRoot(root);
  let names: string[] = [];
  try {
    await mkdir(dir, { recursive: true });
    names = (await readdir(dir)).filter((name) => owned.test(name));
  } catch (e) {
    console.error(
      "Could not clear executor scratch:",
      e instanceof Error ? e.message : e,
    );
  }
  return async () => {
    for (const name of names)
      await rm(resolve(dir, name), { recursive: true, force: true }).catch(
        (e) => console.error("Could not remove executor scratch:", e.message),
      );
  };
}
export const sweepScratch = async (root: string) =>
  (await staleScratch(root))();
let stopping = false;
const children = new Set<ChildProcess>(),
  jobs = new Set<Promise<unknown>>();
// Kills every running job and refuses new ones, then waits until each job's
// scratch is removed. The executor calls it when the container stops.
export async function stopJobs() {
  stopping = true;
  for (const child of children) child.kill("SIGKILL");
  await Promise.allSettled(jobs);
}
// Two jobs run at a time and up to 50 more wait for a turn.
let active = 0;
const waiters: ((ok: boolean) => void)[] = [];
// Resolves true when it is the caller's turn, or false after `wait` ms.
function turn(wait: number) {
  return new Promise<boolean>((resolve) => {
    const waiter = (ok: boolean) => {
      clearTimeout(timer);
      resolve(ok);
    };
    const timer = setTimeout(() => {
      const i = waiters.indexOf(waiter);
      if (i >= 0) waiters.splice(i, 1);
      resolve(false);
    }, wait);
    waiters.push(waiter);
  });
}
// Answers one request to /execute within the job's time limit, counted from
// now: a job still waiting for its turn when its time is up never runs.
export async function submit(
  job: unknown,
  root: string,
): Promise<{ status: number; body: unknown }> {
  let limit: TimeLimit;
  try {
    limit = timeLimit((job || {}) as Execution);
  } catch (e) {
    return { status: 500, body: { error: (e as Error).message } };
  }
  const deadline = Date.now() + limit.ms;
  if (active >= 2) {
    if (waiters.length >= 50)
      return { status: 429, body: { error: "Executor queue is full" } };
    if (!(await turn(limit.ms)))
      return {
        status: 503,
        body: {
          error: `The executor was busy with other jobs for this job's whole time limit (${duration(limit.ms)}, ${limit.setting}). Try again later.`,
        },
      };
  }
  active++;
  try {
    const r = await runJob(JSON.stringify(job), root, {
      timeout: deadline - Date.now(),
    });
    if (r.failed) return { status: 500, body: { error: r.failed } };
    if (r.code === 0)
      try {
        return { status: 200, body: JSON.parse(r.out) };
      } catch {
        return { status: 500, body: { error: "Invalid executor response" } };
      }
    return {
      status: 422,
      body: {
        error: r.timedOut
          ? `Stopped at the executor's time limit (${duration(limit.ms)}, ${limit.setting})`
          : r.err.slice(0, 1500) || "Query exceeded execution limits",
      },
    };
  } finally {
    active--;
    waiters.shift()?.(true);
  }
}
// Runs one job in a disposable child process. The job's scratch directory
// belongs to this process, which removes it once the child has exited, however
// it ended: a kill for the time or output limit skips the child's cleanup.
export function runJob(
  body: string,
  root: string,
  options: { timeout?: number; maxOutput?: number; cwd?: string } = {},
) {
  const job = run(body, root, options);
  jobs.add(job);
  const done = () => jobs.delete(job);
  job.then(done, done);
  return job;
}
async function run(
  body: string,
  root: string,
  {
    timeout = timeLimit(JSON.parse(body)).ms,
    maxOutput = 10_000_000,
    cwd,
  }: { timeout?: number; maxOutput?: number; cwd?: string },
) {
  await mkdir(scratchRoot(root), { recursive: true });
  const scratch = await mkdtemp(resolve(scratchRoot(root), "job-"));
  try {
    return await new Promise<{
      code: number | null;
      out: string;
      err: string;
      failed?: string;
      timedOut?: boolean;
    }>((done) => {
      if (stopping)
        return done({
          code: null,
          out: "",
          err: "",
          failed: "Executor is stopping",
        });
      const child = spawn(process.execPath, ["--import", tsx, runner], {
        cwd,
        // Unset settings stay unset, so the engine's defaults apply.
        env: {
          PATH: process.env.PATH,
          DATA_DIR: root,
          DUCKDB_MEMORY: process.env.DUCKDB_MEMORY || "",
          DUCKDB_SPILL_LIMIT: process.env.DUCKDB_SPILL_LIMIT || "",
          EXECUTOR_RESULT_LIMIT: process.env.EXECUTOR_RESULT_LIMIT || "",
          EXECUTOR_SCRATCH: scratch,
        },
        stdio: ["pipe", "pipe", "pipe"],
      });
      children.add(child);
      let out = "",
        err = "",
        timedOut = false;
      const timer = setTimeout(() => {
        timedOut = true;
        child.kill("SIGKILL");
      }, timeout);
      child.stdout.on("data", (x) => {
        out += x;
        if (out.length > maxOutput) child.kill("SIGKILL");
      });
      child.stderr.on("data", (x) => (err += x));
      // A child that exits before reading its input closes the pipe.
      child.stdin.on("error", () => {});
      child.on("error", (e) => {
        if (child.pid !== undefined) return;
        children.delete(child);
        clearTimeout(timer);
        done({ code: null, out, err, failed: e.message });
      });
      child.on("close", (code) => {
        children.delete(child);
        clearTimeout(timer);
        done({ code, out, err, timedOut });
      });
      child.stdin.end(body);
    });
  } finally {
    // Left for the next start's sweep if it cannot go; the job's answer stands.
    await rm(scratch, { recursive: true, force: true }).catch((e) =>
      console.error("Could not remove executor scratch:", e.message),
    );
  }
}
