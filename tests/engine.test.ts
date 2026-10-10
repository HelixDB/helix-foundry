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
  readFile,
  rm,
  chmod,
  stat,
} from "node:fs/promises";
import { DuckDBInstance } from "@duckdb/node-api";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execute } from "../services/executor/src/engine.js";
import { runJob, sweepScratch } from "../services/executor/src/jobs.js";
import { resultFile, scratchRoot } from "../services/executor/src/paths.js";
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
// Runs until it is killed.
const slow = JSON.stringify({
  mode: "query",
  scope: "a",
  version: "slow",
  inputs: [],
  sql: "SELECT sum(i) FROM range(1000000000000) t(i)",
});
// Waits for the one running job's scratch directory, and puts a file in it
// that stands in for spill files a killed job never gets to remove.
async function spilling() {
  let jobs: string[] = [];
  for (let i = 0; i < 100 && !jobs.length; i++) {
    jobs = await readdir(scratchRoot(dir));
    if (!jobs.length) await new Promise((r) => setTimeout(r, 10));
  }
  expect(jobs).toHaveLength(1);
  await writeFile(join(scratchRoot(dir), jobs[0], "spill.tmp"), "x");
}
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
    for (const mode of ["query", "materialize"] as const)
      for (const sql of [
        "SELECT * FROM read_csv('/etc/passwd')",
        "SELECT * FROM read_csv('https://example.com/data.csv')",
        "SELECT 1; SELECT 2",
        "SELECT * FROM secret_table",
      ])
        await expect(
          execute({ mode, scope: "a", version: "bad", inputs: [], sql }, dir),
        ).rejects.toThrow();
    await expect(readdir(join(dir, "results"))).rejects.toThrow();
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
  it("materializes every row once as JSON lines, converted like query rows", async () => {
    await writeFile(
      join(dir, "incoming", "a", "wide.jsonl"),
      Array.from(
        { length: 3000 },
        (_, i) =>
          `{"id":${9007199254740993n + BigInt(i)},"amount":${i / 4},"day":"2026-10-09","note":${i % 2 ? '"line\\nbreak"' : "null"}}`,
      ).join("\n"),
    );
    await execute(
      { mode: "ingest", scope: "a", version: "wide", file: "wide.jsonl" },
      dir,
    );
    const job = {
      scope: "a",
      inputs: [{ table: "wide", version: "wide" }],
      sql: "SELECT * FROM wide",
    };
    expect(
      await execute({ ...job, mode: "materialize", version: "result" }, dir),
    ).toEqual({ version: "result", count: 3000 });
    const lines = (await readFile(resultFile(dir, "a", "result"), "utf8"))
      .trimEnd()
      .split("\n")
      .map((line) => JSON.parse(line));
    const pages = [];
    for (let offset = 0; offset < 3000; offset += 1000)
      pages.push(
        ...(
          await execute(
            { ...job, mode: "query", version: "page", offset, limit: 1000 },
            dir,
          )
        ).rows,
      );
    expect(lines).toEqual(JSON.parse(JSON.stringify(pages)));
    // BIGINTs past 2^53 stay exact.
    expect(lines.at(-1)).toMatchObject({
      id: "9007199254743992",
      note: "line\nbreak",
    });
    expect(await readdir(join(dir, "datasets", "a"))).not.toContain(
      "result.parquet",
    );
    expect(await readdir(scratchRoot(dir))).toEqual([]);
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
      expect(r.code, r.err).toBe(0);
      expect(JSON.parse(r.out).profile.rows).toBe(1000000);
      expect(await readdir(cwd)).toEqual([]);
      expect(await readdir(scratchRoot(dir))).toEqual([]);
    } finally {
      await chmod(cwd, 0o755);
      await rm(cwd, { recursive: true, force: true });
    }
  });
  it("removes the scratch directory of a job killed at its time limit", async () => {
    const running = runJob(slow, dir, { timeout: 2000 });
    await spilling();
    const r = await running;
    expect(r.code).toBeNull();
    expect(await readdir(scratchRoot(dir))).toEqual([]);
  });
  it("ends running jobs and removes their scratch when the executor stops", async () => {
    // A fresh copy of the job runner, since a stopped one refuses jobs.
    vi.resetModules();
    const jobs = await import("../services/executor/src/jobs.js");
    const running = jobs.runJob(slow, dir);
    await spilling();
    await jobs.stopJobs();
    expect((await running).code).toBeNull();
    expect(await readdir(scratchRoot(dir))).toEqual([]);
    expect(await jobs.runJob(slow, dir)).toMatchObject({
      failed: "Executor is stopping",
    });
    expect(await readdir(scratchRoot(dir))).toEqual([]);
  });
  it("empties scratch left by jobs that ended with the executor, and keeps scratch itself", async () => {
    const before = await stat(scratchRoot(dir));
    await mkdir(join(scratchRoot(dir), "executor-stale"), { recursive: true });
    await writeFile(
      join(scratchRoot(dir), "executor-stale", "duckdb_temp_storage.tmp"),
      "x",
    );
    await sweepScratch(dir);
    expect(await readdir(scratchRoot(dir))).toEqual([]);
    // It may be a mount point, which cannot be removed.
    expect((await stat(scratchRoot(dir))).ino).toBe(before.ino);
  });
  it("starts even when stale scratch cannot be removed", async () => {
    const log = vi.spyOn(console, "error").mockImplementation(() => {});
    await mkdir(join(scratchRoot(dir), "job-stale"));
    await chmod(scratchRoot(dir), 0o555);
    try {
      await expect(sweepScratch(dir)).resolves.toBeUndefined();
      expect(log).toHaveBeenCalledWith(
        "Could not remove executor scratch:",
        expect.stringContaining("EACCES"),
      );
    } finally {
      await chmod(scratchRoot(dir), 0o755);
      log.mockRestore();
    }
    await sweepScratch(dir);
    expect(await readdir(scratchRoot(dir))).toEqual([]);
  });
});
