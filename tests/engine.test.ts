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
  rename,
  rm,
  chmod,
  stat,
} from "node:fs/promises";
import { DuckDBInstance } from "@duckdb/node-api";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { bytes, execute } from "../services/executor/src/engine.js";
import {
  runJob,
  staleScratch,
  submit,
  sweepScratch,
} from "../services/executor/src/jobs.js";
import {
  checkTimeLimits,
  duration,
  timeLimit,
} from "../services/executor/src/limits.js";
import { resultFile, scratchRoot } from "../services/executor/src/paths.js";
vi.mock("node:fs/promises", async (original) => {
  const fs = await original<typeof import("node:fs/promises")>();
  return { ...fs, rename: vi.fn(fs.rename) };
});
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
    // Only ingest compares against a fingerprint stored before the upgrade.
    expect(r.legacyFingerprint).toMatch(/^[0-9a-f]{64}$/);
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
    expect(r).not.toHaveProperty("legacyFingerprint");
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
  it("copies a materialized result when scratch is on another file system", async () => {
    vi.mocked(rename)
      .mockClear()
      .mockRejectedValueOnce(
        Object.assign(new Error("EXDEV: cross-device link not permitted"), {
          code: "EXDEV",
        }),
      );
    expect(
      await execute(
        {
          mode: "materialize",
          scope: "a",
          version: "copied",
          inputs: [],
          sql: "SELECT i AS id FROM range(3000) t(i)",
        },
        dir,
      ),
    ).toEqual({ version: "copied", count: 3000 });
    const lines = (await readFile(resultFile(dir, "a", "copied"), "utf8"))
      .trimEnd()
      .split("\n");
    expect(lines).toHaveLength(3000);
    expect(JSON.parse(lines[2999])).toEqual({ id: "2999" });
    expect(rename).toHaveBeenCalledOnce();
    expect(await readdir(scratchRoot(dir))).toEqual([]);
  });
  it("reads sizes as DuckDB settings do", () => {
    expect(bytes("2GB")).toBe(2e9);
    expect(bytes("512MiB")).toBe(512 * 2 ** 20);
    expect(bytes(" 1.5 gb ")).toBe(1.5e9);
    expect(bytes("4096")).toBe(4096);
    for (const size of ["", "2 gigs", "-1GB", "GB"])
      expect(bytes(size)).toBeNaN();
  });
  it("stops a materialized result at EXECUTOR_RESULT_LIMIT", async () => {
    // Shaped like a relationship whose key matches many rows on both sides:
    // 100,000 pairs from 1,000 rows each.
    const join = JSON.stringify({
      mode: "materialize",
      scope: "a",
      version: "exploding",
      inputs: [],
      sql: "SELECT a.i AS from_key, b.i AS to_key FROM range(1000) a(i) JOIN range(1000) b(i) ON a.i % 10 = b.i % 10",
    });
    vi.stubEnv("EXECUTOR_RESULT_LIMIT", "100KB");
    const r = await runJob(join, dir);
    expect(r.code).toBe(1);
    expect(r.err).toBe("Result exceeds EXECUTOR_RESULT_LIMIT (100KB)");
    await expect(stat(resultFile(dir, "a", "exploding"))).rejects.toThrow();
    expect(await readdir(scratchRoot(dir))).toEqual([]);
    vi.stubEnv("EXECUTOR_RESULT_LIMIT", "2 gigs");
    await expect(
      execute({ ...JSON.parse(join), version: "invalid" }, dir),
    ).rejects.toThrow("Invalid EXECUTOR_RESULT_LIMIT: 2 gigs");
    vi.stubEnv("EXECUTOR_RESULT_LIMIT", "10MB");
    expect(JSON.parse((await runJob(join, dir)).out)).toEqual({
      version: "exploding",
      count: 100000,
    });
    await rm(resultFile(dir, "a", "exploding"));
  });
  it("profiles a snapshot larger than memory using writable, isolated scratch storage", async () => {
    vi.stubEnv("DUCKDB_MEMORY", "128MB");
    // DuckDB's own limit: 90% of the free disk space.
    vi.stubEnv("DUCKDB_SPILL_LIMIT", "");
    const result = await execute({ ...large, version: "large" }, dir);
    expect(result.profile.rows).toBe(1000000);
    expect(result.profile.duplicateRows).toBe(0);
    expect(
      result.profile.columns.find((c) => c.name === "id")?.primaryKey,
    ).toBe(true);
    expect(await readdir(scratchRoot(dir))).toEqual([]);
  });
  it("stops a job whose spill files would pass DUCKDB_SPILL_LIMIT", async () => {
    // The import above, which spills to succeed, through the executor.
    vi.stubEnv("DUCKDB_MEMORY", "128MB");
    vi.stubEnv("DUCKDB_SPILL_LIMIT", "1MB");
    const r = await runJob(
      JSON.stringify({ ...large, version: "capped" }),
      dir,
    );
    expect(r.code).toBe(1);
    expect(r.err).toContain(
      "This limit was set by the 'max_temp_directory_size' setting",
    );
    await expect(
      stat(join(dir, "datasets", "a", "capped.parquet")),
    ).rejects.toThrow();
    expect(await readdir(scratchRoot(dir))).toEqual([]);
    // DuckDB reads the size; one it cannot read fails the job.
    vi.stubEnv("DUCKDB_SPILL_LIMIT", "1 gigs");
    await expect(
      execute({ ...large, version: "invalid" }, dir),
    ).rejects.toThrow("Invalid DUCKDB_SPILL_LIMIT: 1 gigs. ");
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
    for (const stale of ["executor-stale", "job-stale"]) {
      await mkdir(join(scratchRoot(dir), stale), { recursive: true });
      await writeFile(
        join(scratchRoot(dir), stale, "duckdb_temp_storage.tmp"),
        "x",
      );
    }
    // Not the executor's, as on a mounted file system.
    await mkdir(join(scratchRoot(dir), "lost+found"));
    await sweepScratch(dir);
    expect(await readdir(scratchRoot(dir))).toEqual(["lost+found"]);
    // It may be a mount point, which cannot be removed.
    expect((await stat(scratchRoot(dir))).ino).toBe(before.ino);
    await rm(join(scratchRoot(dir), "lost+found"), { recursive: true });
  });
  it("removes only scratch that was there before the executor listened", async () => {
    await mkdir(join(scratchRoot(dir), "job-stale"));
    const sweep = await staleScratch(dir);
    // A job that starts once the executor listens.
    const running = runJob(slow, dir, { timeout: 3000 });
    let fresh: string[] = [];
    for (let i = 0; i < 100 && !fresh.length; i++) {
      fresh = (await readdir(scratchRoot(dir))).filter(
        (n) => n !== "job-stale",
      );
      if (!fresh.length) await new Promise((r) => setTimeout(r, 10));
    }
    expect(fresh).toHaveLength(1);
    await sweep();
    expect(await readdir(scratchRoot(dir))).toEqual(fresh);
    expect((await running).code).toBeNull();
    expect(await readdir(scratchRoot(dir))).toEqual([]);
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
describe("executor time limits", () => {
  const query = JSON.parse(slow),
    materialize = (version: string) => ({
      mode: "materialize",
      scope: "a",
      version,
      inputs: [],
      sql: "SELECT i AS id FROM range(3) t(i)",
    });
  it("gives queries two minutes and whole-table jobs 15, unless set", () => {
    expect(timeLimit({ mode: "query" })).toEqual({
      ms: 120000,
      setting: "EXECUTOR_QUERY_TIMEOUT",
    });
    // Imports, pipelines and publication read or write whole tables.
    for (const job of [
      { mode: "ingest" },
      { mode: "query", persist: true },
      { mode: "materialize" },
    ] as const)
      expect(timeLimit(job)).toEqual({
        ms: 900000,
        setting: "EXECUTOR_JOB_TIMEOUT",
      });
    vi.stubEnv("EXECUTOR_QUERY_TIMEOUT", "30");
    vi.stubEnv("EXECUTOR_JOB_TIMEOUT", " 3600 ");
    expect(timeLimit({ mode: "query" }).ms).toBe(30000);
    expect(timeLimit({ mode: "materialize" }).ms).toBe(3600000);
    // Compose passes an unset setting as empty.
    vi.stubEnv("EXECUTOR_JOB_TIMEOUT", "");
    expect(timeLimit({ mode: "ingest" }).ms).toBe(900000);
    // Whole or decimal seconds.
    vi.stubEnv("EXECUTOR_JOB_TIMEOUT", "1.5");
    expect(timeLimit({ mode: "ingest" }).ms).toBe(1500);
    vi.stubEnv("EXECUTOR_JOB_TIMEOUT", "86400");
    expect(timeLimit({ mode: "ingest" }).ms).toBe(86400000);
    for (const value of [
      "0",
      "0.5",
      "-5",
      "15m",
      "2m",
      "1e3",
      "0x10",
      "86401",
      "100000",
    ]) {
      vi.stubEnv("EXECUTOR_JOB_TIMEOUT", value);
      expect(() => timeLimit({ mode: "ingest" })).toThrow(
        `Invalid EXECUTOR_JOB_TIMEOUT: ${value} (whole or decimal seconds, from 1 to 86400)`,
      );
    }
    expect(duration(900000)).toBe("15 minutes");
    expect(duration(60000)).toBe("1 minute");
    expect(duration(90000)).toBe("90 seconds");
  });
  it("checks both settings at startup, naming an invalid one", () => {
    expect(() => checkTimeLimits()).not.toThrow();
    vi.stubEnv("EXECUTOR_QUERY_TIMEOUT", "2m");
    expect(() => checkTimeLimits()).toThrow(
      "Invalid EXECUTOR_QUERY_TIMEOUT: 2m (whole or decimal seconds, from 1 to 86400)",
    );
    vi.stubEnv("EXECUTOR_QUERY_TIMEOUT", "");
    vi.stubEnv("EXECUTOR_JOB_TIMEOUT", "100000");
    expect(() => checkTimeLimits()).toThrow(
      "Invalid EXECUTOR_JOB_TIMEOUT: 100000 (whole or decimal seconds, from 1 to 86400)",
    );
  });
  it("stops a job at its time limit and says which", async () => {
    vi.stubEnv("EXECUTOR_QUERY_TIMEOUT", "1");
    expect(await submit(query, dir)).toEqual({
      status: 422,
      body: {
        error:
          "Stopped at the executor's time limit (1 second, EXECUTOR_QUERY_TIMEOUT)",
      },
    });
    expect(await readdir(scratchRoot(dir))).toEqual([]);
    vi.stubEnv("EXECUTOR_JOB_TIMEOUT", "soon");
    expect(await submit(materialize("invalid"), dir)).toEqual({
      status: 500,
      body: {
        error:
          "Invalid EXECUTOR_JOB_TIMEOUT: soon (whole or decimal seconds, from 1 to 86400)",
      },
    });
  });
  it("counts time queued behind other jobs, and never runs a job whose time ran out", async () => {
    // Two jobs take both turns until their limit.
    vi.stubEnv("EXECUTOR_QUERY_TIMEOUT", "2");
    const busy = [submit(query, dir), submit(query, dir)];
    vi.stubEnv("EXECUTOR_JOB_TIMEOUT", "1");
    expect(await submit(materialize("expired"), dir)).toEqual({
      status: 503,
      body: {
        error:
          "The executor was busy with other jobs for this job's whole time limit (1 second, EXECUTOR_JOB_TIMEOUT). Try again later.",
      },
    });
    await expect(stat(resultFile(dir, "a", "expired"))).rejects.toThrow();
    // One with time to spare runs when a turn comes free.
    vi.stubEnv("EXECUTOR_JOB_TIMEOUT", "30");
    expect(await submit(materialize("waited"), dir)).toEqual({
      status: 200,
      body: { version: "waited", count: 3 },
    });
    await rm(resultFile(dir, "a", "waited"));
    for (const r of await Promise.all(busy)) expect(r.status).toBe(422);
    expect(await readdir(scratchRoot(dir))).toEqual([]);
  });
});
