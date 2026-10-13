import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";
import { spawn, type ChildProcess } from "node:child_process";
import { once } from "node:events";
import { mkdir, readdir, rm, writeFile } from "node:fs/promises";
import { resolve } from "node:path";
import { randomUUID } from "node:crypto";
import { createApp } from "../apps/api/src/app.js";
import { config } from "../apps/api/src/config.js";
import { MemoryStore } from "../apps/api/src/store.js";
import { startWorker } from "../apps/api/src/jobs.js";
import { datasetTable } from "../packages/shared/src/index.js";
import { FoundryClient } from "../packages/sdk/src/index.js";
const saved = { executor: config.executor, internalKey: config.internalKey };
const root = resolve(config.dataDir),
  key = "synthetic-cancel-executor-key-not-production";
let child: ChildProcess;
let output = "";
async function ownJobDirectories() {
  return (await readdir(resolve(root, "scratch")).catch(() => []))
    .filter((name) => name.startsWith("job-"))
    .sort();
}

beforeAll(async () => {
  config.internalKey = key;
  child = spawn(
    process.execPath,
    ["--import", "tsx", "services/executor/src/server.ts"],
    {
      env: {
        ...process.env,
        DATA_DIR: root,
        EXECUTOR_PORT: "0",
        INTERNAL_KEY: key,
        NODE_ENV: "test",
        EXECUTOR_QUERY_TIMEOUT: "10",
        EXECUTOR_JOB_TIMEOUT: "10",
      },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  child.stdout!.on("data", (data) => (output += data));
  child.stderr!.on("data", (data) => (output += data));
  const started = Date.now();
  while (!output.match(/Server listening at (http:\/\/127\.0\.0\.1:\d+)/)) {
    if (child.exitCode !== null)
      throw new Error("Native executor exited before listening: " + output);
    if (Date.now() - started > 10000)
      throw new Error(
        "Native executor did not listen within 10 seconds: " + output,
      );
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  config.executor = output.match(
    /Server listening at (http:\/\/127\.0\.0\.1:\d+)/,
  )![1];
});
afterAll(async () => {
  Object.assign(config, saved);
  if (child && child.exitCode === null) {
    const ended = once(child, "exit");
    child.kill("SIGTERM");
    await ended;
  }
});
const owned: {
  app: Awaited<ReturnType<typeof createApp>>;
  scope: string;
  stop?: () => Promise<void>;
}[] = [];
afterEach(async () => {
  for (const f of owned.splice(0)) {
    await f.stop?.();
    await f.app.close();
    for (const directory of ["incoming", "datasets", "results"])
      await rm(resolve(root, directory, f.scope), {
        recursive: true,
        force: true,
      });
  }
});
async function fixture() {
  const store = new MemoryStore(),
    app = await createApp(store),
    origin = await app.listen({ host: "127.0.0.1", port: 0 });
  const scope = (await app.inject("/api/v1/me")).json().workspaces[0].id,
    base = `/api/v1/workspaces/${scope}`;
  const f = {
    app,
    scope,
    stop: undefined as (() => Promise<void>) | undefined,
  };
  owned.push(f);
  const ingest = async (work: number, datasetId?: string) => {
    const r = await app.inject({
      method: "POST",
      url: base + "/ingest",
      payload: { name: "Pipeline work", rows: [{ id: 1, work }], datasetId },
    });
    expect(r.statusCode, r.body).toBe(200);
    return r.json();
  };
  const raw = await ingest(2);
  const draft = await app.inject({
    method: "POST",
    url: base + "/pipelines/draft",
    payload: {
      name: "Cancelable native totals",
      inputs: [raw.id],
      sql: `SELECT id, sum(x) AS total FROM "${datasetTable(raw.id)}", range(work) AS series(x) GROUP BY id`,
    },
  });
  expect(draft.statusCode, draft.body).toBe(200);
  const proposal = draft.json();
  expect(
    proposal.data.validation.valid,
    JSON.stringify(proposal.data.validation),
  ).toBe(true);
  const publish = await app.inject({
    method: "POST",
    url: base + `/proposals/${proposal.id}/publish`,
    payload: { hash: proposal.data.hash },
  });
  expect(publish.statusCode, publish.body).toBe(200);
  const pipeline = (await store.list(scope, "pipeline"))[0],
    dataset = (await store.get(scope, pipeline.data.outputDatasetId))!;
  const token = (
    await app.inject({
      method: "POST",
      url: base + "/tokens",
      payload: { name: "Pipeline SDK", readOnly: false },
    })
  ).json();
  const sdk = new FoundryClient({
    baseUrl: origin,
    workspaceId: scope,
    token: token.token,
  });
  const start = () => (f.stop = startWorker(store));
  const cancel = (id: string, headers: any = {}) =>
    app.inject({
      method: "POST",
      url: base + `/pipeline-jobs/${id}/cancel`,
      payload: {},
      headers,
    });
  return {
    store,
    app,
    scope,
    base,
    raw,
    pipeline,
    dataset,
    sdk,
    ingest,
    start,
    cancel,
  };
}
const files = async (path: string) =>
  (await readdir(path).catch(() => [])).sort();
describe("public pipeline cancellation through the real disposable executor", () => {
  it("retains healthy accepted pipeline execution and publication", async () => {
    const f = await fixture(),
      job = await f.sdk.runPipeline(f.pipeline.id);
    expect((await f.app.inject(f.base + "/onboarding")).json().complete).toBe(
      false,
    );
    f.start();
    await expect
      .poll(async () => (await f.store.get(f.scope, job.id))?.data.status, {
        timeout: 10000,
      })
      .toBe("succeeded");
    const ds = (await f.store.get(f.scope, f.dataset.id))!;
    expect(ds.data.activeVersion).not.toBe(f.dataset.data.activeVersion);
    const preview = await f.sdk.previewDataset(ds.id);
    expect(preview.rows).toEqual([{ id: "1", total: 1 }]);
  });
  it("cancels queued SDK jobs idempotently without running or replacing their snapshot", async () => {
    const f = await fixture(),
      before = await files(resolve(root, "datasets", f.scope)),
      job = await f.sdk.runPipeline(f.pipeline.id);
    expect((await f.sdk.cancelPipelineJob(job.id)).data.status).toBe(
      "canceled",
    );
    expect((await f.sdk.cancelPipelineJob(job.id)).data.status).toBe(
      "canceled",
    );
    f.start();
    await new Promise((r) => setTimeout(r, 300));
    expect((await f.store.get(f.scope, job.id))?.data.attempts).toBeUndefined();
    expect((await f.store.get(f.scope, f.dataset.id))?.data.activeVersion).toBe(
      f.dataset.data.activeVersion,
    );
    expect(await files(resolve(root, "datasets", f.scope))).toEqual(before);
  });
  it("stops a running native child, retains its snapshot and completes unrelated work", async () => {
    const f = await fixture();
    await f.ingest(1000000000, f.raw.id);
    const job = await f.sdk.runPipeline(f.pipeline.id),
      before = await files(resolve(root, "datasets", f.scope));
    f.start();
    await expect
      .poll(async () => (await f.store.get(f.scope, job.id))?.data.status, {
        timeout: 10000,
      })
      .toBe("running");
    await expect
      .poll(
        async () =>
          (await files(resolve(root, "scratch"))).some((name) =>
            name.startsWith("job-"),
          ),
        { timeout: 10000 },
      )
      .toBe(true);
    await expect.poll(async () => (await ownJobDirectories()).length).toBe(1);
    const [runningJob] = await ownJobDirectories();
    const other = await fetch(config.executor + "/execute", {
      method: "POST",
      headers: { "content-type": "application/json", "x-internal-key": key },
      body: JSON.stringify({
        mode: "query",
        scope: f.scope,
        sql: "SELECT 42 answer",
      }),
    });
    expect(other.status).toBe(200);
    expect((await other.json()).rows).toEqual([{ answer: 42 }]);
    expect((await f.store.get(f.scope, job.id))?.data.status).toBe("running");
    expect(await ownJobDirectories()).toContain(runningJob);
    expect((await f.cancel(job.id)).statusCode).toBe(200);
    await expect
      .poll(async () => await ownJobDirectories())
      .not.toContain(runningJob);
    await expect
      .poll(async () => await files(resolve(root, "scratch")), {
        timeout: 10000,
      })
      .toEqual([]);
    expect((await f.store.get(f.scope, job.id))?.data.status).toBe("canceled");
    expect((await f.store.get(f.scope, f.dataset.id))?.data.activeVersion).toBe(
      f.dataset.data.activeVersion,
    );
    expect(await files(resolve(root, "datasets", f.scope))).toEqual(before);
    await f.ingest(2, f.raw.id);
    const next = await f.sdk.runPipeline(f.pipeline.id);
    await expect
      .poll(async () => (await f.store.get(f.scope, next.id))?.data.status, {
        timeout: 10000,
      })
      .toBe("succeeded");
  });
  it("rechecks cancellation when it wins an optimistic output promotion", async () => {
    const f = await fixture(),
      job = await f.sdk.runPipeline(f.pipeline.id),
      before = await files(resolve(root, "datasets", f.scope));
    const commit = f.store.commit.bind(f.store);
    let intervened = false;
    f.store.commit = async (scope, version, changes) => {
      if (
        !intervened &&
        scope === f.scope &&
        changes.some(
          (change: any) =>
            change.put?.kind === "version" &&
            change.put.data.datasetId === f.dataset.id,
        )
      ) {
        intervened = true;
        expect((await f.cancel(job.id)).statusCode).toBe(200);
        return false;
      }
      return commit(scope, version, changes);
    };
    f.start();
    await expect
      .poll(async () => (await f.store.get(f.scope, job.id))?.data.status, {
        timeout: 10000,
      })
      .toBe("canceled");
    await expect
      .poll(async () => await files(resolve(root, "datasets", f.scope)), {
        timeout: 10000,
      })
      .toEqual(before);
    expect(intervened).toBe(true);
    expect((await f.store.get(f.scope, f.dataset.id))?.data.activeVersion).toBe(
      f.dataset.data.activeVersion,
    );
  });
  it("rejects finished, foreign and readonly cancellation without changing those jobs", async () => {
    const f = await fixture(),
      job = await f.sdk.runPipeline(f.pipeline.id);
    const readOnly = (
      await f.app.inject({
        method: "POST",
        url: f.base + "/tokens",
        payload: { name: "Readonly" },
      })
    ).json();
    expect(
      (await f.cancel(job.id, { authorization: "Bearer " + readOnly.token }))
        .statusCode,
    ).toBe(403);
    const other = (
      await f.app.inject({
        method: "POST",
        url: "/api/v1/workspaces",
        payload: { name: "Other workspace" },
      })
    ).json();
    expect(
      (
        await f.app.inject({
          method: "POST",
          url: `/api/v1/workspaces/${other.id}/pipeline-jobs/${job.id}/cancel`,
          payload: {},
        })
      ).statusCode,
    ).toBe(404);
    expect((await f.cancel(f.raw.id)).statusCode).toBe(404);
    f.start();
    await expect
      .poll(async () => (await f.store.get(f.scope, job.id))?.data.status, {
        timeout: 10000,
      })
      .toBe("succeeded");
    expect((await f.cancel(job.id)).statusCode).toBe(409);
    expect((await f.store.get(f.scope, job.id))?.data.status).toBe("succeeded");
  });
  it("removes only a disconnected queued request and preserves other executor work and scratch", async () => {
    const f = await fixture(),
      controllers = [new AbortController(), new AbortController()],
      marker = "unrelated-sentinel",
      version = randomUUID();
    const scratch = resolve(root, "scratch");
    await mkdir(scratch, { recursive: true });
    await writeFile(resolve(scratch, marker), "unrelated");
    const post = (body: unknown, signal?: AbortSignal, suffix = "") =>
      fetch(config.executor + "/execute" + suffix, {
        method: "POST",
        headers: { "content-type": "application/json", "x-internal-key": key },
        body: JSON.stringify(body),
        signal,
      });
    const slow = controllers.map((controller) =>
      post(
        {
          mode: "query",
          scope: f.scope,
          sql: "SELECT sum(n % 100) total FROM range(1000000000) AS r(n)",
        },
        controller.signal,
      ).catch((error) => error),
    );
    let canceled: Promise<unknown> | undefined;
    const queued = new AbortController();
    try {
      await expect
        .poll(
          async () =>
            (await files(scratch)).filter((name) => name.startsWith("job-"))
              .length,
          { timeout: 10000 },
        )
        .toBe(2);
      await expect.poll(async () => (await ownJobDirectories()).length).toBe(2);
      const requestId = randomUUID();
      canceled = post(
        {
          mode: "query",
          scope: f.scope,
          sql: "SELECT 99 result",
          persist: true,
          version,
        },
        queued.signal,
        "?queued=" + requestId,
      ).catch((error) => error);
      await expect
        .poll(() => output.includes("queued=" + requestId), { timeout: 5000 })
        .toBe(true);
      expect(
        (await files(scratch)).filter((name) => name.startsWith("job-")).length,
      ).toBe(2);
      queued.abort();
      await canceled;
      const healthy = post({
        mode: "query",
        scope: f.scope,
        sql: "SELECT 7 answer",
      });
      controllers[0].abort();
      const response = await healthy;
      expect(response.status).toBe(200);
      expect((await response.json()).rows).toEqual([{ answer: 7 }]);
      expect(
        (await files(scratch)).filter((name) => name.startsWith("job-")).length,
      ).toBe(1);
      expect(await files(resolve(root, "datasets", f.scope))).not.toContain(
        version + ".parquet",
      );
      expect(await files(scratch)).toContain(marker);
    } finally {
      queued.abort();
      controllers.forEach((controller) => controller.abort());
      await Promise.allSettled(slow);
      await canceled;
      await expect
        .poll(
          async () =>
            (await files(scratch)).filter((name) => name.startsWith("job-"))
              .length,
          { timeout: 10000 },
        )
        .toBe(0);
      await expect.poll(async () => await ownJobDirectories()).toEqual([]);
      await rm(resolve(scratch, marker), { force: true });
    }
  });
});
