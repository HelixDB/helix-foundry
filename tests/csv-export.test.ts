import {
  afterEach,
  beforeAll,
  afterAll,
  describe,
  expect,
  it,
  vi,
} from "vitest";
import { randomUUID } from "node:crypto";
import { mkdir, writeFile, readFile, readdir, rm } from "node:fs/promises";
import { resolve } from "node:path";
import { parse } from "csv-parse/sync";
import { execute as nativeExecute } from "../services/executor/src/engine.js";
import { createServer } from "node:http";
import { once } from "node:events";
import { createApp } from "../apps/api/src/app.js";
import { MemoryStore } from "../apps/api/src/store.js";
import { ingestFile } from "../apps/api/src/datasets.js";
import { config } from "../apps/api/src/config.js";
import { datasetCsv } from "../apps/api/src/results.js";
import { datasetInputs } from "../apps/api/src/datasets.js";
import { datasetTable } from "../packages/shared/src/index.js";

// Real loopback transport into native DuckDB, with no provider or database
// services. The worker and API use the actual execute/postJson code paths.
const originalExecutor = config.executor;
const executorCalls: any[] = [];
const executorServer = createServer(async (req, res) => {
  try {
    const parts: Buffer[] = [];
    for await (const part of req) parts.push(part);
    const job = JSON.parse(Buffer.concat(parts).toString());
    executorCalls.push(job);
    const result = await nativeExecute(job);
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify(result));
  } catch (e) {
    res.statusCode = 422;
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify({ error: (e as Error).message }));
  }
});
beforeAll(async () => {
  executorServer.listen(0, "127.0.0.1");
  await once(executorServer, "listening");
  config.executor = `http://127.0.0.1:${(executorServer.address() as any).port}`;
});
afterAll(async () => {
  config.executor = originalExecutor;
  executorServer.closeAllConnections();
  await new Promise<void>((r) => executorServer.close(() => r()));
});

const installations: {
  app: Awaited<ReturnType<typeof createApp>>;
  scope: string;
}[] = [];
afterEach(async () => {
  for (const { app, scope } of installations.splice(0)) {
    await app.close();
    for (const kind of ["incoming", "datasets", "results"])
      await rm(resolve(config.dataDir, kind, scope), {
        recursive: true,
        force: true,
      });
  }
  executorCalls.length = 0;
  vi.unstubAllEnvs();
});
async function fixture() {
  const store = new MemoryStore(),
    app = await createApp(store);
  const origin = await app.listen({ host: "127.0.0.1", port: 0 });
  const scope = (await app.inject("/api/v1/me")).json().workspaces[0].id;
  installations.push({ app, scope });
  const dir = resolve(config.dataDir, "incoming", scope);
  await mkdir(dir, { recursive: true });
  const file = randomUUID() + ".jsonl";
  await writeFile(
    resolve(dir, file),
    Array.from(
      { length: 1201 },
      (_, i) =>
        `{"id":${9007199254740993n + BigInt(i)},"note":${JSON.stringify(i % 2 ? 'comma, quote " and\nline' : "")},"missing":${i % 2 ? '"present"' : "null"},"nested":{"flag":true,"labels":["a","b"]}}`,
    ).join("\n"),
  );
  const dataset = await ingestFile(store, scope, "Download fixture", file);
  executorCalls.length = 0;
  return {
    store,
    app,
    scope,
    origin,
    dataset,
    url: `${origin}/api/v1/workspaces/${scope}/datasets/${dataset.id}/export`,
  };
}
describe("whole-snapshot CSV download", () => {
  it("removes the native materialized file when a download closes before its first read", async () => {
    const { store, scope, dataset } = await fixture();
    const inputs = await datasetInputs(store, scope, [dataset.id]);
    const stream = await datasetCsv(
      { scope, inputs, sql: `SELECT * FROM "${datasetTable(dataset.id)}"` },
      dataset.data.profile.columns.map((c: any) => c.name),
    );
    expect(
      await readdir(resolve(config.dataDir, "results", scope)),
    ).toHaveLength(1);
    stream.destroy();
    await expect
      .poll(async () => readdir(resolve(config.dataDir, "results", scope)))
      .toEqual([]);
  });

  it("exports every native DuckDB row with exact integers, quotes, newlines, nulls and nested JSON", async () => {
    const { scope, dataset, url } = await fixture();
    const response = await fetch(url + "?format=csv");
    const body = await response.text();
    expect(response.status).toBe(200);
    expect(response.headers.get("content-type")).toContain("text/csv");
    expect(response.headers.get("content-disposition")).toContain(
      'filename="dataset.csv"',
    );
    expect(response.headers.get("x-foundry-version")).toBe(
      dataset.data.activeVersion,
    );
    const rows = parse(body, { columns: true }) as Record<string, string>[];
    expect(rows).toHaveLength(1201);
    expect(rows[0]).toEqual({
      id: "9007199254740993",
      note: "",
      missing: "",
      nested: '{"flag":true,"labels":["a","b"]}',
    });
    expect(rows[1].note).toBe('comma, quote " and\nline');
    expect(rows.at(-1)!.id).toBe("9007199254742193");
    // Quoted empty string and unquoted null remain distinguishable in the file.
    expect(body).toContain('"9007199254740993","",,');
    expect(executorCalls).toHaveLength(1);
    expect(executorCalls[0]).toMatchObject({
      mode: "materialize",
      inputs: [{ version: dataset.data.activeVersion }],
    });
    expect(await readdir(resolve(config.dataDir, "results", scope))).toEqual(
      [],
    );
  });
  it("retains byte-identical Parquet downloads by default", async () => {
    const { scope, url, dataset } = await fixture();
    const parquet = await fetch(url);
    expect(parquet.headers.get("content-disposition")).toContain(
      "dataset.parquet",
    );
    expect(Buffer.from(await parquet.arrayBuffer())).toEqual(
      await readFile(
        resolve(
          config.dataDir,
          "datasets",
          scope,
          dataset.data.activeVersion + ".parquet",
        ),
      ),
    );
  });
  it("includes typed headers for an empty snapshot", async () => {
    const { store, scope, origin } = await fixture();
    await writeFile(
      resolve(config.dataDir, "incoming", scope, "empty.csv"),
      "",
    );
    const empty = await ingestFile(
      store,
      scope,
      "Empty",
      "empty.csv",
      undefined,
      undefined,
      undefined,
      [
        { name: "Column, name", type: "string" },
        { name: "Count", type: "bigint" },
      ],
    );
    const response = await fetch(
      `${origin}/api/v1/workspaces/${scope}/datasets/${empty.id}/export?format=csv`,
    );
    const body = await response.text();
    expect(response.headers.get("content-type")).toContain("text/csv");
    expect(body).toBe('"Column, name","Count"\r\n');
    expect(await readdir(resolve(config.dataDir, "results", scope))).toEqual(
      [],
    );
  });
  it("uses normal API errors before streaming and refuses foreign workspace datasets", async () => {
    const { app, scope, url, dataset } = await fixture();
    const other = (
      await app.inject({
        method: "POST",
        url: "/api/v1/workspaces",
        payload: { name: "Foreign download" },
      })
    ).json().id;
    expect(
      (
        await app.inject(
          `/api/v1/workspaces/${other}/datasets/${dataset.id}/export?format=csv`,
        )
      ).statusCode,
    ).toBe(404);
    vi.stubEnv("EXECUTOR_RESULT_LIMIT", "1B");
    const failed = await fetch(url + "?format=csv");
    const failure = await failed.text();
    expect(failed.status).toBe(422);
    expect(failed.headers.get("content-type")).toContain("application/json");
    expect(JSON.parse(failure)).toMatchObject({
      error: expect.stringContaining(
        "Result exceeds EXECUTOR_RESULT_LIMIT (1B)",
      ),
    });
    expect((await fetch(url + "?format=unknown")).status).toBe(400);
    expect(
      await readdir(resolve(config.dataDir, "results", scope)).catch(() => []),
    ).toEqual([]);
  });
});
