import { DuckDBInstance } from "@duckdb/node-api";
import { createHash } from "node:crypto";
import { mkdir, stat } from "node:fs/promises";
import { resolve, dirname } from "node:path";
import type { DatasetProfile } from "../../../packages/shared/src/index.js";
export const quote = (v: string) => '"' + v.replaceAll('"', '""') + '"';
const lit = (v: string) => "'" + v.replaceAll("'", "''") + "'";
const safePart = (s: string) => /^[a-zA-Z0-9_-]{1,100}$/.test(s);
export type Execution = {
  mode: "ingest" | "query" | "export";
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
};
export async function execute(
  job: Execution,
  root = resolve(process.env.DATA_DIR || ".data"),
) {
  if (!safePart(job.scope) || (job.version && !safePart(job.version)))
    throw new Error("Invalid storage identity");
  const out = resolve(root, "datasets", job.scope, `${job.version}.parquet`);
  await mkdir(dirname(out), { recursive: true });
  const db = await DuckDBInstance.create(":memory:", {
    memory_limit: process.env.DUCKDB_MEMORY || "512MB",
    threads: "2",
    allow_unsigned_extensions: "false",
    autoinstall_known_extensions: "false",
    autoload_known_extensions: "false",
  });
  const c = await db.connect();
  try {
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
    for (const col of schema) {
      const name = String(col.column_name),
        q = quote(name);
      const p = (
        await c.runAndReadAll(
          `SELECT count(*)-count(${q}) AS null_count, count(DISTINCT ${q}) distincts FROM result`,
        )
      ).getRowObjectsJson()[0];
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
    const profile: DatasetProfile = {
      rows,
      duplicateRows,
      columns,
      sample,
      fingerprint: createHash("sha256")
        .update(JSON.stringify([schema, rows, fp]))
        .digest("hex"),
    };
    return { profile, version: job.version, rows: sample };
  } finally {
    c.closeSync();
    db.closeSync();
  }
}
