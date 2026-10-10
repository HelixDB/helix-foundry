import mysql from "mysql2";
import pg from "pg";
import {
  S3Client,
  ListObjectsV2Command,
  GetObjectCommand,
} from "@aws-sdk/client-s3";
import { createWriteStream } from "node:fs";
import { mkdir } from "node:fs/promises";
import { pipeline } from "node:stream/promises";
import { once } from "node:events";
import { resolve } from "node:path";
import { randomUUID } from "node:crypto";
import { config } from "./config.js";
import { assert } from "./security.js";
import { postgresPoolConfig } from "./postgres-config.js";
import { discoverStripe, importStripe } from "./stripe.js";
import { discoverWorkos, importWorkos } from "./workos.js";
import { discoverPosthog, importPosthog } from "./posthog.js";
export type ConnectorConfig = Record<string, any>;
function mysqlPool(c: ConnectorConfig) {
  return mysql.createPool({
    host: c.host,
    port: Number(c.port || 3306),
    user: c.user,
    password: c.password,
    database: c.database,
    connectionLimit: 2,
    connectTimeout: 10000,
    supportBigNumbers: true,
    bigNumberStrings: true,
    decimalNumbers: false,
    dateStrings: true,
    ssl: c.tls
      ? { rejectUnauthorized: true, ...(c.ca ? { ca: c.ca } : {}) }
      : undefined,
  });
}
function pgPool(c: ConnectorConfig) {
  return new pg.Pool(postgresPoolConfig(c));
}
const identifier = (s: string, kind: string) =>
  kind === "mysql"
    ? "`" + s.replaceAll("`", "``") + "`"
    : '"' + s.replaceAll('"', '""') + '"';
const s3 = (c: ConnectorConfig) =>
  new S3Client({
    region: c.region || "us-east-1",
    endpoint: c.endpoint || undefined,
    forcePathStyle: !!c.endpoint,
    credentials: c.accessKeyId
      ? { accessKeyId: c.accessKeyId, secretAccessKey: c.secretAccessKey }
      : undefined,
  });
export function checkedURL(value: string) {
  const u = new URL(value);
  assert(["http:", "https:"].includes(u.protocol), "Use an HTTP(S) URL");
  assert(
    !u.username && !u.password,
    "Use credential fields instead of URL credentials",
  );
  assert(
    ![
      "169.254.169.254",
      "metadata.google.internal",
      "100.100.100.200",
    ].includes(u.hostname),
    "Metadata endpoints are not data sources",
  );
  return u;
}
function headers(c: ConnectorConfig) {
  return c.token
    ? {
        [c.header || "Authorization"]: c.header
          ? String(c.token)
          : "Bearer " + c.token,
      }
    : {};
}
export async function discover(kind: string, c: ConnectorConfig) {
  if (kind === "mysql") {
    const pool = mysqlPool(c);
    try {
      const [tables] = await pool
        .promise()
        .query(
          "SELECT TABLE_NAME name, TABLE_ROWS estimatedRows FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = ?",
          [c.database, "BASE TABLE"],
        );
      const [keys] = await pool
        .promise()
        .query(
          "SELECT TABLE_NAME tableName, COLUMN_NAME columnName, CONSTRAINT_NAME constraintName, REFERENCED_TABLE_NAME referencedTable, REFERENCED_COLUMN_NAME referencedColumn FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA = ?",
          [c.database],
        );
      const [databases] = await pool
        .promise()
        .query("SELECT SCHEMA_NAME name FROM information_schema.SCHEMATA");
      const [columns] = await pool
        .promise()
        .query(
          "SELECT TABLE_NAME tableName, COLUMN_NAME name, COLUMN_TYPE type, IS_NULLABLE nullable, COLUMN_DEFAULT defaultValue FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = ? ORDER BY TABLE_NAME, ORDINAL_POSITION",
          [c.database],
        );
      const [sample] = c.table
        ? await pool
            .promise()
            .query("SELECT * FROM " + identifier(c.table, kind) + " LIMIT 10")
        : [[]];
      return { tables, keys, databases, columns, sample };
    } finally {
      await pool.promise().end();
    }
  }
  if (kind === "postgres") {
    const pool = pgPool(c);
    try {
      const tables = await pool.query(
        "SELECT table_schema schema, table_name name FROM information_schema.tables WHERE table_schema NOT IN ('pg_catalog','information_schema') AND table_type='BASE TABLE'",
      );
      const keys = await pool.query(
        "SELECT tc.table_name, kcu.column_name, tc.constraint_type FROM information_schema.table_constraints tc JOIN information_schema.key_column_usage kcu ON tc.constraint_name=kcu.constraint_name AND tc.table_schema=kcu.table_schema WHERE tc.constraint_type IN ('PRIMARY KEY','FOREIGN KEY')",
      );
      const columns = await pool.query(
        "SELECT table_schema AS schema,table_name AS table_name,column_name AS name,data_type AS type,is_nullable AS nullable FROM information_schema.columns WHERE table_schema NOT IN ('pg_catalog','information_schema') ORDER BY table_schema,table_name,ordinal_position",
      );
      const foreignKeys = await pool.query(
        "SELECT fk.table_schema, fk.table_name, fk.column_name, pk.table_schema AS referenced_schema, pk.table_name AS referenced_table, pk.column_name AS referenced_column FROM information_schema.referential_constraints rc JOIN information_schema.key_column_usage fk ON fk.constraint_name=rc.constraint_name AND fk.constraint_schema=rc.constraint_schema JOIN information_schema.key_column_usage pk ON pk.constraint_name=rc.unique_constraint_name AND pk.constraint_schema=rc.unique_constraint_schema AND pk.ordinal_position=fk.position_in_unique_constraint",
      );
      const sample = c.table
        ? (
            await pool.query(
              "SELECT * FROM " +
                identifier(c.schema || "public", kind) +
                "." +
                identifier(c.table, kind) +
                " LIMIT 10",
            )
          ).rows
        : [];
      return {
        tables: tables.rows,
        keys: keys.rows,
        columns: columns.rows,
        foreignKeys: foreignKeys.rows,
        sample,
      };
    } finally {
      await pool.end();
    }
  }
  if (kind === "s3") {
    const client = s3(c);
    try {
      const tables: { name: string; size?: number }[] = [];
      let cursor: string | undefined;
      do {
        const r = await client.send(
          new ListObjectsV2Command({
            Bucket: c.bucket,
            Prefix: c.prefix || "",
            MaxKeys: 1000,
            ContinuationToken: cursor,
          }),
        );
        for (const item of r.Contents || [])
          if (item.Key && /\.(csv|json|jsonl|parquet)$/i.test(item.Key))
            tables.push({ name: item.Key, size: item.Size });
        cursor = r.IsTruncated ? r.NextContinuationToken : undefined;
        assert(
          !r.IsTruncated || cursor,
          "Bucket listing was incomplete. Try a narrower prefix.",
        );
        assert(
          tables.length <= 10000,
          "More than 10,000 files found. Use a narrower prefix to select files.",
        );
      } while (cursor);
      return { tables };
    } finally {
      client.destroy();
    }
  }
  if (kind === "stripe") return discoverStripe(c);
  if (kind === "workos") return discoverWorkos(c);
  if (kind === "posthog") return discoverPosthog(c);
  if (kind === "rest") {
    const r = await fetch(checkedURL(c.url), {
      headers: headers(c),
      redirect: "error",
      signal: AbortSignal.timeout(15000),
    });
    assert(r.ok, `Source returned HTTP ${r.status}`);
    const data = await r.json();
    const rows = c.itemsPath ? atPath(data, c.itemsPath) : data;
    assert(Array.isArray(rows), "REST items must be an array; set itemsPath");
    return {
      tables: [{ name: "API response", estimatedRows: rows.length }],
      sample: rows.slice(0, 10),
    };
  }
  return { tables: [] };
}
function atPath(value: any, path: string) {
  return path.split(".").reduce((v, k) => v?.[k], value);
}
export async function importSource(
  scope: string,
  kind: string,
  c: ConnectorConfig,
  progress?: (progress: {
    message: string;
    records?: number;
    bytes?: number;
  }) => Promise<unknown>,
): Promise<{
  file: string;
  records: number;
  schema?: { name: string; type: string }[];
}> {
  await mkdir(resolve(config.dataDir, "incoming", scope), { recursive: true });
  const ext =
    kind === "s3"
      ? String(c.key).split(".").at(-1)?.toLowerCase() || "json"
      : "jsonl";
  assert(
    ["csv", "json", "jsonl", "parquet"].includes(ext),
    "Choose CSV, JSON, JSONL or Parquet",
  );
  const file = randomUUID() + "." + ext,
    path = resolve(config.dataDir, "incoming", scope, file),
    out = createWriteStream(path);
  let count = 0;
  let schema: { name: string; type: string }[] | undefined;
  let lastProgress = 0;
  await progress?.({ message: "Reading complete snapshot", records: 0 });
  const write = async (row: any) => {
    if (Date.now() - lastProgress > 1000) {
      lastProgress = Date.now();
      await progress?.({
        message: "Reading complete snapshot",
        records: count,
      });
    }
    if (++count > 2_000_000)
      throw new Error("Import limit is two million records per snapshot");
    if (
      !out.write(
        JSON.stringify(row, (_, v) =>
          typeof v === "bigint" ? v.toString() : v,
        ) + "\n",
      )
    )
      await once(out, "drain");
  };
  try {
    if (kind === "mysql") {
      assert(c.table, "Select a table");
      const pool = mysqlPool(c);
      try {
        const connection = await pool.promise().getConnection();
        try {
          const [columns] = await connection.query(
            "SELECT COLUMN_NAME name,DATA_TYPE type FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? ORDER BY ORDINAL_POSITION",
            [c.database, c.table],
          );
          schema = columns as { name: string; type: string }[];
          await connection.query(
            "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ",
          );
          await connection.query("START TRANSACTION READ ONLY");
          const stream = (
            connection.connection as unknown as mysql.PoolConnection
          )
            .query("SELECT * FROM " + identifier(c.table, kind))
            .stream({ highWaterMark: 100 });
          const raw = connection.connection as any;
          const failed = (error: Error) => stream.destroy(error);
          const ended = () =>
            stream.destroy(
              new Error(
                "Source connection ended before the snapshot completed",
              ),
            );
          raw.on("error", failed);
          raw.on("end", ended);
          const timer = setTimeout(
            () =>
              stream.destroy(new Error("Snapshot exceeded import time limit")),
            Number(c.timeoutMs || 120000),
          );
          try {
            for await (const row of stream) await write(row);
          } finally {
            clearTimeout(timer);
            raw.off("error", failed);
            raw.off("end", ended);
          }
          await connection.commit();
        } finally {
          connection.release();
        }
      } finally {
        await pool.promise().end();
      }
    } else if (kind === "postgres") {
      assert(c.table, "Select a table");
      const pool = pgPool(c),
        client = await pool.connect();
      try {
        schema = (
          await client.query(
            "SELECT column_name name,data_type type FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2 ORDER BY ordinal_position",
            [c.schema || "public", c.table],
          )
        ).rows;
        await client.query("BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY");
        await client.query(
          "DECLARE foundry_cursor NO SCROLL CURSOR FOR SELECT * FROM " +
            identifier(c.schema || "public", kind) +
            "." +
            identifier(c.table, kind),
        );
        while (true) {
          const r = await client.query("FETCH 1000 FROM foundry_cursor");
          if (!r.rows.length) break;
          for (const row of r.rows) await write(row);
        }
        await client.query("COMMIT");
      } finally {
        client.release();
        await pool.end();
      }
    } else if (kind === "rest") {
      let url = checkedURL(c.url),
        page = 0,
        seen = new Set<string>();
      if (c.pagination === "offset") {
        url.searchParams.set(c.offsetParam || "offset", "0");
        url.searchParams.set(
          c.limitParam || "limit",
          String(c.pageSize || 100),
        );
      }
      while (page++ < 1000) {
        assert(!seen.has(url.href), "Pagination repeated a URL");
        seen.add(url.href);
        const response = await fetch(url, {
          headers: headers(c),
          redirect: "error",
          signal: AbortSignal.timeout(30000),
        });
        assert(response.ok, `Source returned HTTP ${response.status}`);
        const data = (await response.json()) as any,
          rows = c.itemsPath ? atPath(data, c.itemsPath) : data;
        assert(Array.isArray(rows), "REST items must be an array");
        for (const row of rows) await write(row);
        if (c.pagination === "next" && atPath(data, c.nextPath || "next")) {
          const next = checkedURL(
            new URL(atPath(data, c.nextPath || "next"), url).href,
          );
          assert(
            next.origin === url.origin,
            "Pagination must stay on the configured origin",
          );
          url = next;
        } else if (
          c.pagination === "cursor" &&
          atPath(data, c.nextPath || "next_cursor")
        ) {
          url = new URL(url);
          url.searchParams.set(
            c.cursorParam || "cursor",
            String(atPath(data, c.nextPath || "next_cursor")),
          );
        } else if (
          c.pagination === "offset" &&
          rows.length >= Number(c.pageSize || 100)
        ) {
          url = new URL(url);
          url.searchParams.set(
            c.offsetParam || "offset",
            String(page * Number(c.pageSize || 100)),
          );
          url.searchParams.set(
            c.limitParam || "limit",
            String(c.pageSize || 100),
          );
        } else break;
        if (page === 1000) throw new Error("Pagination exceeded 1000 pages");
      }
    } else if (kind === "stripe") {
      schema = await importStripe(c, write);
    } else if (kind === "workos") {
      schema = await importWorkos(c, write);
    } else if (kind === "posthog") {
      schema = await importPosthog(c, write);
    } else if (kind === "s3") {
      assert(c.key, "Select an object key");
      const client = s3(c);
      try {
        const result = await client.send(
          new GetObjectCommand({ Bucket: c.bucket, Key: c.key }),
        );
        assert(result.Body, "Empty S3 object");
        for await (const chunk of result.Body as any) {
          if (!out.write(chunk)) await once(out, "drain");
          if (Date.now() - lastProgress > 1000) {
            lastProgress = Date.now();
            await progress?.({
              message: "Downloading complete file",
              bytes: out.bytesWritten,
            });
          }
        }
        out.end();
        await once(out, "finish");
        await progress?.({
          message: "Profiling complete snapshot",
          bytes: out.bytesWritten,
        });
        return { file, records: count };
      } finally {
        client.destroy();
      }
    } else throw new Error("Upload files through the upload endpoint");
    out.end();
    await once(out, "finish");
    await progress?.({
      message: "Profiling complete snapshot",
      records: count,
    });
    return { file, records: count, schema: count === 0 ? schema : undefined };
  } catch (e) {
    out.destroy();
    throw e;
  }
}
