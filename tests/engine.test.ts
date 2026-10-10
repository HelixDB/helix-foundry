import {
  describe,
  it,
  expect,
  beforeAll,
  afterAll,
  afterEach,
  vi,
} from "vitest";
import {
  mkdtemp,
  writeFile,
  mkdir,
  readdir,
  rm,
  chmod,
} from "node:fs/promises";
import { DuckDBInstance } from "@duckdb/node-api";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execute } from "../services/executor/src/engine.js";
import { runJob, sweepScratch } from "../services/executor/src/jobs.js";
import { scratchRoot } from "../services/executor/src/paths.js";
let dir: string;
beforeAll(async () => {
  dir = await mkdtemp(join(tmpdir(), "foundry-engine-"));
  await mkdir(join(dir, "incoming", "a"), { recursive: true });
  await writeFile(
    join(dir, "incoming", "a", "input.jsonl"),
    [
      { id: "a", amount: 12, region: "EU", email: "a@example.com" },
      { id: "b", amount: 23, region: "US", email: null },
    ]
      .map((r) => JSON.stringify(r))
      .join("\n"),
  );
  // Profiling this snapshot with 128 MB of memory has to spill to disk.
  const fixture = await DuckDBInstance.create(":memory:");
  const connection = await fixture.connect();
  try {
    await connection.run(
      `COPY (SELECT i id, md5(i::VARCHAR) || repeat('x', 128) payload FROM range(1000000) t(i)) TO '${join(dir, "incoming", "a", "large.parquet").replaceAll("'", "''")}' (FORMAT PARQUET)`,
    );
  } finally {
    connection.closeSync();
    fixture.closeSync();
  }
});
afterEach(() => {
  vi.unstubAllEnvs();
});
afterAll(() => rm(dir, { recursive: true, force: true }));
const large = { mode: "ingest", scope: "a", file: "large.parquet" } as const;
describe("isolated dataset execution", () => {
  it("profiles an immutable snapshot and preserves nulls and key evidence", async () => {
    const r = await execute(
      { mode: "ingest", scope: "a", version: "one", file: "input.jsonl" },
      dir,
    );
    expect(r.profile.rows).toBe(2);
    expect(r.profile.columns.find((c) => c.name === "id")?.primaryKey).toBe(
      true,
    );
    expect(r.profile.columns.find((c) => c.name === "email")).toMatchObject({
      nulls: 1,
      sensitive: true,
    });
  });
  it("executes and persists a verified aggregate", async () => {
    const r = await execute(
      {
        mode: "query",
        scope: "a",
        version: "aggregate",
        inputs: [{ table: "input", version: "one" }],
        sql: "SELECT region, sum(amount) revenue FROM input GROUP BY region",
        persist: true,
      },
      dir,
    );
    expect(r.rows).toEqual(
      expect.arrayContaining([
        { region: "EU", revenue: "12" },
        { region: "US", revenue: "23" },
      ]),
    );
  });
  it("blocks external files, network reads, multiple statements, and undeclared tables", async () => {
    for (const sql of [
      "SELECT * FROM read_csv('/etc/passwd')",
      "SELECT * FROM read_csv('https://example.com/data.csv')",
      "SELECT 1; SELECT 2",
      "SELECT * FROM secret_table",
    ])
      await expect(
        execute(
          { mode: "query", scope: "a", version: "bad", inputs: [], sql },
          dir,
        ),
      ).rejects.toThrow();
  });
  it("rejects storage traversal", async () => {
    await expect(
      execute(
        { mode: "ingest", scope: "../a", version: "bad", file: "input.jsonl" },
        dir,
      ),
    ).rejects.toThrow("Invalid storage");
  });
  it("fingerprints rows as a multiset, independent of their order", async () => {
    let n = 0;
    const fingerprint = async (...rows: string[]) => {
      const version = "multiset" + n++;
      await writeFile(
        join(dir, "incoming", "a", version + ".jsonl"),
        rows.join("\n"),
      );
      return (
        await execute(
          { mode: "ingest", scope: "a", version, file: version + ".jsonl" },
          dir,
        )
      ).profile.fingerprint;
    };
    const a = '{"id":"a"}',
      b = '{"id":"b"}';
    expect(await fingerprint(a, a)).not.toBe(await fingerprint(b, b));
    expect(await fingerprint(a, b, a)).toBe(await fingerprint(a, a, b));
  });
  it("profiles a snapshot larger than memory using writable, isolated scratch storage", async () => {
    vi.stubEnv("DUCKDB_MEMORY", "128MB");
    const result = await execute({ ...large, version: "large" }, dir);
    expect(result.profile.rows).toBe(1000000);
    expect(result.profile.duplicateRows).toBe(0);
    expect(
      result.profile.columns.find((c) => c.name === "id")?.primaryKey,
    ).toBe(true);
    expect(await readdir(scratchRoot(dir))).toEqual([]);
  });
  it("spills from a read-only working directory, as in the container", async () => {
    const cwd = await mkdtemp(join(tmpdir(), "foundry-readonly-"));
    await chmod(cwd, 0o555);
    vi.stubEnv("DUCKDB_MEMORY", "128MB");
    try {
      const r = await runJob(
        JSON.stringify({ ...large, version: "readonly" }),
        dir,
        { cwd },
      );
      expect(r.err).toBe("");
      expect(r.code).toBe(0);
      expect(JSON.parse(r.out).profile.rows).toBe(1000000);
      expect(await readdir(cwd)).toEqual([]);
      expect(await readdir(scratchRoot(dir))).toEqual([]);
    } finally {
      await chmod(cwd, 0o755);
      await rm(cwd, { recursive: true, force: true });
    }
  });
  it("removes the scratch directory of a job killed at its time limit", async () => {
    const running = runJob(
      JSON.stringify({
        mode: "query",
        scope: "a",
        version: "slow",
        inputs: [],
        sql: "SELECT sum(i) FROM range(1000000000000) t(i)",
      }),
      dir,
      { timeout: 2000 },
    );
    let jobs: string[] = [];
    for (let i = 0; i < 100 && !jobs.length; i++) {
      jobs = await readdir(scratchRoot(dir));
      if (!jobs.length) await new Promise((r) => setTimeout(r, 10));
    }
    expect(jobs).toHaveLength(1);
    // Stands in for spill files the killed job never got to remove.
    await writeFile(join(scratchRoot(dir), jobs[0], "spill.tmp"), "x");
    const r = await running;
    expect(r.code).toBeNull();
    expect(await readdir(scratchRoot(dir))).toEqual([]);
  });
  it("clears scratch left by jobs that ended with the executor", async () => {
    await mkdir(join(scratchRoot(dir), "executor-stale"), { recursive: true });
    await writeFile(
      join(scratchRoot(dir), "executor-stale", "duckdb_temp_storage.tmp"),
      "x",
    );
    await sweepScratch(dir);
    expect(await readdir(scratchRoot(dir))).toEqual([]);
  });
});
