import { spawn, type ChildProcess } from "node:child_process";
import { mkdir, mkdtemp, readdir, rm } from "node:fs/promises";
import { createRequire } from "node:module";
import { resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { scratchRoot } from "./paths.js";
const runner = fileURLToPath(new URL("./runner.ts", import.meta.url));
// Resolved here, so the child does not depend on its working directory.
const tsx = pathToFileURL(createRequire(import.meta.url).resolve("tsx")).href;
// Jobs that ended with the executor, as in an out-of-memory kill, leave their
// spill files behind. No job runs yet, so all of them are stale. Only the
// contents go: scratch itself may be a mount point. A failure is logged and
// never stops the executor from starting.
export async function sweepScratch(root: string) {
  const dir = scratchRoot(root);
  try {
    await mkdir(dir, { recursive: true });
    for (const name of await readdir(dir))
      await rm(resolve(dir, name), { recursive: true, force: true }).catch(
        (e) => console.error("Could not remove executor scratch:", e.message),
      );
  } catch (e) {
    console.error(
      "Could not clear executor scratch:",
      e instanceof Error ? e.message : e,
    );
  }
}
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
    timeout = 120000,
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
        env: {
          PATH: process.env.PATH,
          DATA_DIR: root,
          DUCKDB_MEMORY: process.env.DUCKDB_MEMORY || "512MB",
          DUCKDB_SPILL_LIMIT: process.env.DUCKDB_SPILL_LIMIT || "2GB",
          EXECUTOR_SCRATCH: scratch,
        },
        stdio: ["pipe", "pipe", "pipe"],
      });
      children.add(child);
      let out = "",
        err = "";
      const timer = setTimeout(() => child.kill("SIGKILL"), timeout);
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
        done({ code, out, err });
      });
      child.stdin.end(body);
    });
  } finally {
    await rm(scratch, { recursive: true, force: true });
  }
}
