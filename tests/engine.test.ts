import { describe, it, expect, beforeAll, afterAll } from "vitest";
import { mkdtemp, writeFile, mkdir, readdir, rm } from "node:fs/promises";
import { DuckDBInstance } from "@duckdb/node-api";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { execute } from "../services/executor/src/engine.js";
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
});
afterAll(() => rm(dir, { recursive: true, force: true }));
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
  it("profiles a snapshot larger than memory using writable, isolated scratch storage", async () => {
    const path = join(dir, "incoming", "a", "large.parquet");
    const fixture = await DuckDBInstance.create(":memory:");
    const connection = await fixture.connect();
    try {
      await connection.run(
        `COPY (SELECT i id, md5(i::VARCHAR) || repeat('x', 128) payload FROM range(1000000) t(i)) TO '${path.replaceAll("'", "''")}' (FORMAT PARQUET)`,
      );
    } finally {
      connection.closeSync();
      fixture.closeSync();
    }
    const before = process.env.DUCKDB_MEMORY;
    process.env.DUCKDB_MEMORY = "128MB";
    try {
      const result = await execute(
        { mode: "ingest", scope: "a", version: "large", file: "large.parquet" },
        dir,
      );
      expect(result.profile.rows).toBe(1000000);
      expect(result.profile.duplicateRows).toBe(0);
      expect(
        result.profile.columns.find((c) => c.name === "id")?.primaryKey,
      ).toBe(true);
      expect(await readdir(join(dir, "scratch"))).toEqual([]);
    } finally {
      if (before === undefined) delete process.env.DUCKDB_MEMORY;
      else process.env.DUCKDB_MEMORY = before;
    }
  });
});
