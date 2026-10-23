import { expect, it } from "vitest";
import { mkdtemp, readdir, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import { randomUUID } from "node:crypto";
import { submit } from "../services/executor/src/jobs.js";

const files = async (path: string) =>
  (await readdir(path).catch(() => [])).sort();
let observedScratch: string;
async function ownedJobDirectories() {
  return (await files(observedScratch)).filter((name) =>
    name.startsWith("job-"),
  );
}

it("removes an admitted native executor waiter while preserving the other child", async () => {
  const root = await mkdtemp(resolve(tmpdir(), "helix-cancel-waiter-"));
  const previous = {
    query: process.env.EXECUTOR_QUERY_TIMEOUT,
    job: process.env.EXECUTOR_JOB_TIMEOUT,
  };
  process.env.EXECUTOR_QUERY_TIMEOUT = "10";
  process.env.EXECUTOR_JOB_TIMEOUT = "10";
  const controllers = [new AbortController(), new AbortController()];
  const queued = new AbortController();
  const scratch = resolve(root, "scratch"),
    version = randomUUID();
  observedScratch = scratch;
  const pending: Promise<unknown>[] = [];
  try {
    // Both calls enter the complete production admission path before yielding.
    for (const controller of controllers)
      pending.push(
        submit(
          {
            mode: "query",
            scope: "waiter",
            sql: "SELECT sum(n % 100) total FROM range(1000000000) AS r(n)",
          },
          root,
          controller.signal,
        ),
      );
    await expect
      .poll(async () => (await ownedJobDirectories()).length, {
        timeout: 10000,
      })
      .toBe(2);
    await expect
      .poll(
        async () =>
          (await files(scratch)).filter((n) => n.startsWith("job-")).length,
        { timeout: 10000 },
      )
      .toBe(2);
    await writeFile(resolve(scratch, "unrelated-sentinel"), "unrelated");
    const occupyingJobs = await ownedJobDirectories();
    // submit synchronously calls turn when active===2. Its returned promise
    // therefore proves waiter admission, unlike an HTTP incoming-request log.
    const canceled = submit(
      {
        mode: "query",
        scope: "waiter",
        sql: "SELECT 99 result",
        persist: true,
        version,
      },
      root,
      queued.signal,
    );
    pending.push(canceled);
    queued.abort();
    expect(await canceled).toEqual({
      status: 499,
      body: { error: "Executor request canceled" },
    });
    expect(await ownedJobDirectories()).toEqual(occupyingJobs);
    expect(await files(resolve(root, "datasets", "waiter"))).not.toContain(
      version + ".parquet",
    );
    const healthy = submit(
      { mode: "query", scope: "waiter", sql: "SELECT 7 answer" },
      root,
    );
    pending.push(healthy);
    controllers[0].abort();
    const response = await healthy;
    expect(response.status).toBe(200);
    expect((response.body as any).rows).toEqual([{ answer: 7 }]);
    expect((await ownedJobDirectories()).length).toBe(1);
    expect(await files(scratch)).toContain("unrelated-sentinel");
  } finally {
    queued.abort();
    controllers.forEach((controller) => controller.abort());
    await Promise.allSettled(pending);
    await expect
      .poll(async () => await ownedJobDirectories(), { timeout: 10000 })
      .toEqual([]);
    await expect
      .poll(
        async () =>
          (await files(scratch)).filter((n) => n.startsWith("job-")).length,
        { timeout: 10000 },
      )
      .toBe(0);
    if (previous.query === undefined) delete process.env.EXECUTOR_QUERY_TIMEOUT;
    else process.env.EXECUTOR_QUERY_TIMEOUT = previous.query;
    if (previous.job === undefined) delete process.env.EXECUTOR_JOB_TIMEOUT;
    else process.env.EXECUTOR_JOB_TIMEOUT = previous.job;
    await rm(root, { recursive: true, force: true });
  }
});
