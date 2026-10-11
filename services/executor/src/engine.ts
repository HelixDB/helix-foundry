import {
  DuckDBInstance,
  type DuckDBConnection,
  type Json,
} from "@duckdb/node-api";
import { createHash, randomUUID } from "node:crypto";
import { createWriteStream } from "node:fs";
import { copyFile, mkdir, mkdtemp, rename, rm, stat } from "node:fs/promises";
import { pipeline } from "node:stream/promises";
import { resolve, dirname } from "node:path";
import type { DatasetProfile } from "../../../packages/shared/src/index.js";
import { resultFile, scratchRoot } from "./paths.js";
export const quote = (v: string) => '"' + v.replaceAll('"', '""') + '"';
const lit = (v: string) => "'" + v.replaceAll("'", "''") + "'";
const safePart = (s: string) => /^[a-zA-Z0-9_-]{1,100}$/.test(s);
export type Execution = {
  mode: "ingest" | "query" | "export" | "materialize";
  scope: string;
  version?: string;
  file?: string;
  format?: string;
  schema?: { name: string; type: string }[];
  inputs?: {
    table: string;
    version: string;
    aliases?: string[];
    filters?: { column: string; value: string | number | boolean | null }[];
  }[];
  sql?: string;
  persist?: boolean;
  offset?: number;
  limit?: number;
  // Query only: also profile how each column's values run in the result's
  // row order (ColumnProfile.order), over every row, not only the sample.
  order?: boolean;
};
export type Executed = {
  profile: DatasetProfile;
  version?: string;
  rows: Record<string, Json>[];
  // Ingest only: profile.fingerprint in the untagged format older executors
  // stored.
  legacyFingerprint?: string;
};
export type Materialized = { version: string; count: number };
const units: Record<string, number> = {
  b: 1,
  k: 1e3,
  kb: 1e3,
  m: 1e6,
  mb: 1e6,
  g: 1e9,
  gb: 1e9,
  t: 1e12,
  tb: 1e12,
  kib: 2 ** 10,
  mib: 2 ** 20,
  gib: 2 ** 30,
  tib: 2 ** 40,
};
// A size written as DuckDB's settings take it: 2GB is 10^9 bytes, 2GiB 2^30.
// NaN when it is not one.
export function bytes(size: string) {
  const [, n, unit = "b"] =
    /^\s*(\d+(?:\.\d+)?)\s*([a-z]+)?\s*$/i.exec(size) || [];
  return Number(n) * (units[unit.toLowerCase()] ?? NaN);
}
// The executor passes a scratch directory it removes however the job ends;
// without one, the job makes its own and removes it when it finishes.
export async function execute(
  job: Execution & { mode: "materialize" },
  root?: string,
  scratch?: string,
): Promise<Materialized>;
export async function execute(
  job: Execution,
  root?: string,
  scratch?: string,
): Promise<Executed>;
export async function execute(
  job: Execution,
  root = resolve(process.env.DATA_DIR || ".data"),
  scratch?: string,
): Promise<Executed | Materialized> {
  if (
    !safePart(job.scope) ||
    (job.version && !safePart(job.version)) ||
    (job.mode === "materialize" && !job.version)
  )
    throw new Error("Invalid storage identity");
  const out = resolve(root, "datasets", job.scope, `${job.version}.parquet`);
  await mkdir(dirname(out), { recursive: true });
  if (!scratch) await mkdir(scratchRoot(root), { recursive: true });
  const temp =
    scratch || (await mkdtemp(resolve(scratchRoot(root), "executor-")));
  let db: DuckDBInstance | undefined;
  let c: DuckDBConnection | undefined;
  try {
    db = await DuckDBInstance.create(":memory:", {
      memory_limit: process.env.DUCKDB_MEMORY || "512MB",
      threads: "2",
      temp_directory: temp,
      allow_unsigned_extensions: "false",
      autoinstall_known_extensions: "false",
      autoload_known_extensions: "false",
    });
    c = await db.connect();
    // DuckDB reports max_temp_directory_size when it is given here but does
    // not enforce it; once SET, it stops a job whose spill files would pass
    // it. Unset, DuckDB's own limit applies: 90% of the disk's free space.
    const spill = process.env.DUCKDB_SPILL_LIMIT?.trim();
    if (spill)
      await c
        .run(`SET max_temp_directory_size = ${lit(spill)}`)
        .catch((e: Error) => {
          throw new Error(`Invalid DUCKDB_SPILL_LIMIT: ${spill}. ${e.message}`);
        });
    if (job.mode === "ingest") {
      if (!job.file || !/^[-\w]+\.(csv|json|jsonl|parquet)$/.test(job.file))
        throw new Error("Invalid input file");
      const file = resolve(root, "incoming", job.scope, job.file);
      const info = await stat(file);
      const fn = job.file.endsWith(".csv")
        ? "read_csv_auto"
        : job.file.endsWith(".parquet")
          ? "read_parquet"
          : "read_json_auto";
      if (info.size === 0 && job.schema?.length) {
        const typeMap: Record<string, string> = {
          int: "BIGINT",
          integer: "BIGINT",
          bigint: "VARCHAR",
          smallint: "INTEGER",
          tinyint: "INTEGER",
          float: "DOUBLE",
          double: "DOUBLE",
          boolean: "BOOLEAN",
          date: "DATE",
        };
        await c.run(
          `CREATE TABLE result (${job.schema.map((col) => quote(col.name) + " " + (typeMap[col.type.toLowerCase()] || "VARCHAR")).join(",")})`,
        );
      } else
        await c.run(`CREATE TABLE result AS SELECT * FROM ${fn}(${lit(file)})`);
    } else {
      for (const input of job.inputs || []) {
        if (!safePart(input.table) || !safePart(input.version))
          throw new Error("Invalid input");
        await c.run(
          `CREATE TABLE ${quote(input.table)} AS SELECT * FROM read_parquet(${lit(resolve(root, "datasets", job.scope, input.version + ".parquet"))})`,
        );
        for (const filter of input.filters || []) {
          const value =
            filter.value === null
              ? "NULL"
              : typeof filter.value === "string"
                ? lit(filter.value)
                : String(filter.value);
          await c.run(
            `DELETE FROM ${quote(input.table)} WHERE NOT (${quote(filter.column)} IS NOT DISTINCT FROM ${value})`,
          );
        }
      }
      const aliasCounts = new Map<string, number>();
      for (const input of job.inputs || [])
        for (const alias of input.aliases || [])
          aliasCounts.set(
            alias.toLowerCase(),
            (aliasCounts.get(alias.toLowerCase()) || 0) + 1,
          );
      const taken = new Set(
        (job.inputs || []).map((i) => i.table.toLowerCase()),
      );
      taken.add("result");
      for (const input of job.inputs || [])
        for (const alias of input.aliases || []) {
          if (
            aliasCounts.get(alias.toLowerCase()) === 1 &&
            !taken.has(alias.toLowerCase())
          ) {
            await c.run(
              `CREATE VIEW ${quote(alias)} AS SELECT * FROM ${quote(input.table)}`,
            );
            taken.add(alias.toLowerCase());
          }
        }
      const sql = job.sql?.trim().replace(/;\s*$/, "");
      if (!sql || !/^\s*(select|with)\b/i.test(sql))
        throw new Error("Only a single SELECT query is allowed");
      const extracted = await c.extractStatements(sql);
      if (extracted.count !== 1)
        throw new Error("Only a single SELECT query is allowed");
      await c.run(`SET allowed_paths = [${lit(out)}]`);
      await c.run("SET enable_external_access = false");
      await c.run("SET lock_configuration = true");
      if (job.mode === "materialize") {
        // One pass and no profile. Rows convert exactly as a query's rows do.
        // The file is written in scratch and moved into place once complete.
        // Its size is capped, since no other limit applies to it: a join
        // that multiplies rows writes gigabytes long before the time limit.
        const limit = process.env.EXECUTOR_RESULT_LIMIT || "2GB",
          max = bytes(limit);
        if (!(max >= 0))
          throw new Error("Invalid EXECUTOR_RESULT_LIMIT: " + limit);
        const result = await c.stream(
          `SELECT * FROM (${sql}) AS foundry_result`,
        );
        const partial = resolve(temp, "result.jsonl"),
          file = resultFile(root, job.scope, job.version!);
        let count = 0,
          size = 0;
        await pipeline(async function* () {
          for await (const rows of result.yieldRowObjectJson()) {
            const lines = rows
              .map((row) => JSON.stringify(row) + "\n")
              .join("");
            size += Buffer.byteLength(lines);
            if (size > max)
              throw new Error(
                `Result exceeds EXECUTOR_RESULT_LIMIT (${limit})`,
              );
            count += rows.length;
            yield lines;
          }
        }, createWriteStream(partial));
        await mkdir(dirname(file), { recursive: true });
        // Scratch may be mounted from another file system. A copy is complete
        // before the worker learns of the file, which is all it relies on.
        await rename(partial, file).catch((e) =>
          e.code === "EXDEV" ? copyFile(partial, file) : Promise.reject(e),
        );
        return { version: job.version!, count };
      }
      await c.run(
        `CREATE TABLE result AS SELECT * FROM (${sql}) AS foundry_result`,
      );
    }
    if (job.mode === "export") {
      const path = out.replace(/\.parquet$/, ".csv");
      throw new Error("Use dataset Parquet export or paginated CSV export");
    }
    if (job.mode === "ingest" || job.persist)
      await c.run(`COPY result TO ${lit(out)} (FORMAT PARQUET)`);
    const rows = Number(
      (
        await c.runAndReadAll("SELECT count(*) n FROM result")
      ).getRowObjectsJson()[0].n,
    );
    const schema = (
      await c.runAndReadAll("DESCRIBE result")
    ).getRowObjectsJson();
    const sample = (
      await c.runAndReadAll(
        `SELECT * FROM result LIMIT ${Math.min(job.limit || 50, 1000)} OFFSET ${Math.max(job.offset || 0, 0)}`,
      )
    ).getRowObjectsJson();
    const columns = [];
    // How each column's values run in the result's row order, which rowid
    // follows. A column named rowid hides it, so that column is renamed
    // while the order is read.
    const orders = new Map<string, Record<string, Json>>();
    if (job.mode === "query" && job.order) {
      const names = schema.map((col) => String(col.column_name));
      const hiding = names.find((n) => n.toLowerCase() === "rowid"),
        aside = "foundry_rowid_" + randomUUID().replaceAll("-", "");
      if (hiding)
        await c.run(
          `ALTER TABLE result RENAME COLUMN ${quote(hiding)} TO ${quote(aside)}`,
        );
      try {
        for (const name of names) {
          const q = quote(name === hiding ? aside : name);
          // A column whose values cannot be compared (a map) has no order.
          const o = await c
            .runAndReadAll(
              `SELECT count(*) FILTER (WHERE v > p) AS ups, count(*) FILTER (WHERE v < p) AS downs, count(*) FILTER (WHERE v = f) AS firsts FROM (SELECT ${q} AS v, lag(${q}) OVER (ORDER BY rowid) AS p, first_value(${q}) OVER (ORDER BY rowid) AS f FROM result WHERE ${q} IS NOT NULL)`,
            )
            .then((r) => r.getRowObjectsJson()[0])
            .catch(() => null);
          if (o) orders.set(name, o);
        }
      } finally {
        if (hiding)
          await c.run(
            `ALTER TABLE result RENAME COLUMN ${quote(aside)} TO ${quote(hiding)}`,
          );
      }
    }
    for (const col of schema) {
      const name = String(col.column_name),
        q = quote(name);
      const p = (
        await c.runAndReadAll(
          `SELECT count(*)-count(${q}) AS null_count, count(DISTINCT ${q}) distincts FROM result`,
        )
      ).getRowObjectsJson()[0];
      const o = orders.get(name);
      columns.push({
        name,
        type: String(col.column_type),
        nulls: Number(p.null_count),
        distinct: Number(p.distincts),
        examples: sample.slice(0, 3).map((r) => r[name]),
        sensitive:
          /email|phone|ssn|password|secret|token|address|birth|credit/i.test(
            name,
          ),
        primaryKey:
          rows > 0 &&
          Number(p.null_count) === 0 &&
          Number(p.distincts) === rows,
        ...(o
          ? {
              order: {
                up: Number(o.ups),
                down: Number(o.downs),
                first: Number(o.firsts),
              },
            }
          : {}),
      });
    }
    const fp = (
      await c.runAndReadAll(
        "SELECT bit_xor(hash(result)) h, sum(hash(result)::HUGEINT) hash_sum FROM result",
      )
    ).getRowObjectsJson()[0];
    const duplicateRows = Number(
      (
        await c.runAndReadAll(
          "SELECT (SELECT count(*) FROM result) - count(*) AS duplicate_rows FROM (SELECT DISTINCT * FROM result)",
        )
      ).getRowObjectsJson()[0].duplicate_rows,
    );
    const digest = (v: unknown) =>
      createHash("sha256").update(JSON.stringify(v)).digest("hex");
    const profile: DatasetProfile = {
      rows,
      duplicateRows,
      columns,
      sample,
      // Tagged since it counts duplicate rows; untagged ones are older.
      fingerprint: "v2:" + digest([schema, rows, fp]),
    };
    return {
      profile,
      version: job.version,
      rows: sample,
      // Ingest also gets the untagged fingerprint of the same data, so a
      // stored one can be compared like for like after an upgrade.
      ...(job.mode === "ingest"
        ? { legacyFingerprint: digest([schema, rows, { h: fp.h }]) }
        : {}),
    };
  } finally {
    c?.closeSync();
    db?.closeSync();
    if (!scratch) await rm(temp, { recursive: true, force: true });
  }
}
