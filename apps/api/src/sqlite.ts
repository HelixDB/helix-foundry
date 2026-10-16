import { createHash, randomUUID } from "node:crypto";
import { createWriteStream } from "node:fs";
import { mkdir, open, rename, rm, stat } from "node:fs/promises";
import { platform } from "node:os";
import { resolve } from "node:path";
import { pipeline } from "node:stream/promises";
import { DatabaseSync, constants } from "node:sqlite";
import type { Readable } from "node:stream";
import { config } from "./config.js";
import { AppError, assert } from "./security.js";
import type { ConnectorConfig } from "./connectors.js";

// A SQLite database arrives as an uploaded file rather than a server. It is
// staged once per workspace under its content hash, and each selected table
// becomes a source that reads it, as a database connection's tables do. The
// file's declared primary and foreign keys give the suggested ontology the
// same declared links a PostgreSQL or MySQL connection gives it.

// Bytes 0-15 of every SQLite 3 database. Encrypted files (SQLCipher) do not
// start with it.
const MAGIC = Buffer.from("SQLite format 3\0", "latin1");
const FILE = /^[0-9a-f]{64}\.sqlite$/;

export const sqliteDir = (scope: string) =>
  resolve(config.dataDir, "sqlite", scope);

// The staged file a source names. The name is a content hash, so a config
// cannot point outside this workspace's folder or at another workspace's file.
export function sqlitePath(scope: string, file: unknown) {
  assert(scope, "SQLite files belong to a workspace");
  assert(
    typeof file === "string" && FILE.test(file),
    "Upload the SQLite file again.",
  );
  return resolve(sqliteDir(scope), file);
}

const quote = (s: string) => '"' + s.replaceAll('"', '""') + '"';
// data_version is read by the full-text index while quick_check verifies it.
const allowedPragmas = new Set([
  "table_list",
  "table_xinfo",
  "foreign_key_list",
  "quick_check",
  "data_version",
]);

const allowedFunctions = new Set(["count", "max", "typeof"]);

// The file comes from the browser, so it is opened read-only and defensively,
// without extensions or functions from its schema, and statements may only
// read: no ATTACH, writes or other pragmas, whatever a table definition holds.
function openDatabase(path: string) {
  // Node.js 24.10 added the authorizer and 24.12 the defensive flag; earlier
  // releases would open the file without them.
  const api = DatabaseSync.prototype as Partial<DatabaseSync>;
  assert(
    typeof api.setAuthorizer === "function" &&
      typeof api.enableDefensive === "function",
    "Reading SQLite files needs Node.js 24.12 or later. Update Node.js, or use the Docker install.",
  );
  let db: DatabaseSync | undefined;
  try {
    db = new DatabaseSync(path, {
      readOnly: true,
      allowExtension: false,
      readBigInts: true,
      timeout: 0,
    });
    db.enableDefensive(true);
    db.exec("PRAGMA trusted_schema = OFF");
  } catch {
    if (db?.isOpen) db.close();
    throw new AppError(
      400,
      "This file could not be opened as a SQLite database. Encrypted databases are not supported.",
    );
  }
  db.setAuthorizer((action, first, second) =>
    action === constants.SQLITE_SELECT ||
    action === constants.SQLITE_READ ||
    (action === constants.SQLITE_PRAGMA && allowedPragmas.has(first ?? "")) ||
    (action === constants.SQLITE_FUNCTION && allowedFunctions.has(second ?? ""))
      ? constants.SQLITE_OK
      : constants.SQLITE_DENY,
  );
  return db;
}

export async function openSqlite(scope: string, c: ConnectorConfig) {
  const path = sqlitePath(scope, c.file);
  try {
    await stat(path);
  } catch {
    throw new AppError(400, "Upload the SQLite file again.");
  }
  return openDatabase(path);
}

// SQLite integers read as BigInt so none loses precision. Those in the safe
// range become numbers; larger ones stay strings, as the MySQL connector keeps
// big numbers. Blobs become base64 text. Numbers in a text column are written
// as text, so each column has one type in every row.
function plain(row: Record<string, unknown>, text = new Set<string>()) {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(row))
    out[k] =
      (typeof v === "bigint" || typeof v === "number") && text.has(k)
        ? String(v)
        : typeof v === "bigint"
          ? v >= BigInt(Number.MIN_SAFE_INTEGER) &&
            v <= BigInt(Number.MAX_SAFE_INTEGER)
            ? Number(v)
            : v.toString()
          : v instanceof Uint8Array
            ? Buffer.from(v).toString("base64")
            : v;
  return out;
}

// SQLite lets one column hold numbers in some rows and text in others, and an
// integer may exceed JavaScript's safe range. Either way the column is
// imported as text: one pass over the table finds them, in groups of columns
// that stay within SQLite's limit on result columns.
function textColumns(db: DatabaseSync, table: string, names: string[]) {
  const text = new Set<string>(),
    safe = String(Number.MAX_SAFE_INTEGER);
  for (let i = 0; i < names.length; i += 300) {
    const part = names.slice(i, i + 300);
    const found = db
      .prepare(
        "SELECT " +
          part
            .map((name, j) => {
              const c = quote(name);
              return `max(typeof(${c}) IN ('text', 'blob')) AS t${j}, max(typeof(${c}) IN ('integer', 'real')) AS n${j}, max(typeof(${c}) = 'integer' AND (${c} > ${safe} OR ${c} < -${safe})) AS b${j}`;
            })
            .join(", ") +
          " FROM " +
          quote(table),
      )
      .get() as Record<string, bigint | null>;
    part.forEach((name, j) => {
      if ((found[`t${j}`] && found[`n${j}`]) || found[`b${j}`]) text.add(name);
    });
  }
  return text;
}

// Ordinary tables of the main database. Views, virtual tables, their shadow
// tables and SQLite's own tables are left out.
function tableNames(db: DatabaseSync) {
  return (db.prepare("PRAGMA table_list").all() as any[])
    .filter(
      (t) =>
        t.schema === "main" &&
        t.type === "table" &&
        !String(t.name).toLowerCase().startsWith("sqlite_"),
    )
    .map((t) => String(t.name))
    .sort();
}

// Columns as SELECT * returns them, generated columns included.
const columnsOf = (db: DatabaseSync, table: string) =>
  (
    db.prepare(`PRAGMA table_xinfo(${quote(table)})`).all() as {
      name: string;
      type: string;
      notnull: bigint;
      dflt_value: string | null;
      pk: bigint;
      hidden: bigint;
    }[]
  ).filter((col) => Number(col.hidden) !== 1);

// A declared type in the names the executor maps for an empty table; SQLite
// accepts any declared type, and its affinity rules read INT and REAL in it.
function columnType(declared: string) {
  const t = declared.toUpperCase();
  return /INT/.test(t)
    ? "integer"
    : /REAL|FLOA|DOUB/.test(t)
      ? "double"
      : "varchar";
}

export async function discoverSqlite(scope: string, c: ConnectorConfig) {
  const db = await openSqlite(scope, c);
  try {
    const names = tableNames(db);
    const keys: Record<string, unknown>[] = [],
      foreignKeys: Record<string, unknown>[] = [],
      columns: Record<string, unknown>[] = [];
    const primary = new Map<string, string[]>();
    for (const table of names) {
      const own = columnsOf(db, table);
      primary.set(
        table.toLowerCase(),
        own
          .filter((col) => Number(col.pk) > 0)
          .sort((a, b) => Number(a.pk) - Number(b.pk))
          .map((col) => col.name),
      );
      for (const col of own) {
        columns.push({
          tableName: table,
          name: col.name,
          type: col.type || "",
          nullable: Number(col.notnull) ? "NO" : "YES",
          defaultValue: col.dflt_value,
        });
        if (Number(col.pk) > 0)
          keys.push({
            tableName: table,
            columnName: col.name,
            constraintName: "PRIMARY",
          });
      }
    }
    for (const table of names)
      for (const fk of db
        .prepare(`PRAGMA foreign_key_list(${quote(table)})`)
        .all() as any[]) {
        // A reference without columns names the parent's primary key.
        const to =
          fk.to ??
          primary.get(String(fk.table).toLowerCase())?.[Number(fk.seq)];
        if (to)
          foreignKeys.push({
            tableName: table,
            columnName: fk.from,
            referencedTable: fk.table,
            referencedColumn: to,
          });
      }
    const tables = names.map((name) => ({
      name,
      estimatedRows: Number(
        (db.prepare(`SELECT count(*) AS n FROM ${quote(name)}`).get() as any).n,
      ),
    }));
    let sample: Record<string, unknown>[] = [];
    if (c.table) {
      assert(names.includes(c.table), "Choose a table from the SQLite file");
      sample = (
        db.prepare(`SELECT * FROM ${quote(c.table)} LIMIT 10`).all() as any[]
      ).map((row) => plain(row));
    }
    // A preview describes its own table.
    return {
      tables,
      keys,
      foreignKeys,
      columns: c.table
        ? columns.filter((col) => col.tableName === c.table)
        : columns,
      sample,
    };
  } finally {
    db.close();
  }
}

export async function importSqlite(
  scope: string,
  c: ConnectorConfig,
  write: (row: Record<string, unknown>) => Promise<void>,
) {
  assert(c.table, "Select a table");
  const db = await openSqlite(scope, c);
  try {
    assert(
      tableNames(db).includes(c.table),
      "This table is no longer in the SQLite file",
    );
    const columns = columnsOf(db, c.table);
    const schema = columns.map((col) => ({
      name: col.name,
      type: columnType(col.type || ""),
    }));
    const text = textColumns(
      db,
      c.table,
      columns.map((col) => col.name),
    );
    for (const row of db
      .prepare(`SELECT * FROM ${quote(c.table)}`)
      .iterate() as Iterable<Record<string, unknown>>)
      await write(plain(row, text));
    return schema;
  } finally {
    db.close();
  }
}

// Stages an uploaded file: written beside its final name, checked, then moved
// into place under its content hash, so a repeated upload of the same file
// reuses it. A database last written in WAL mode is switched to rollback
// journal mode in its header, since only the main file is uploaded and a
// read-only connection cannot create the WAL index.
export async function stageSqlite(
  scope: string,
  file: Readable & { truncated?: boolean },
) {
  const dir = sqliteDir(scope);
  await mkdir(dir, { recursive: true });
  const temporary = resolve(dir, randomUUID() + ".staging");
  const hash = createHash("sha256");
  file.on("data", (chunk: Buffer) => hash.update(chunk));
  try {
    await pipeline(file, createWriteStream(temporary, { flags: "wx" }));
    assert(!file.truncated, "File exceeds the 256 MB upload limit");
    const handle = await open(temporary, "r+");
    try {
      const header = Buffer.alloc(100);
      const { bytesRead } = await handle.read(header, 0, 100, 0);
      assert(
        bytesRead === 100 && header.subarray(0, 16).equals(MAGIC),
        "Choose a SQLite database file (.db, .sqlite, .sqlite3 or .db3). Encrypted databases are not supported.",
      );
      if (header[18] === 2 || header[19] === 2)
        await handle.write(Buffer.from([1, 1]), 0, 2, 18);
      await handle.sync();
    } finally {
      await handle.close();
    }
    const db = openDatabase(temporary);
    try {
      const check = db.prepare("PRAGMA quick_check(1)").get() as any;
      assert(
        Object.values(check ?? {})[0] === "ok",
        "This SQLite file is damaged. Copy it again with the application that writes it closed, then upload the copy.",
      );
      tableNames(db);
    } catch (error) {
      if (error instanceof AppError) throw error;
      throw new AppError(
        400,
        "This SQLite file is damaged. Copy it again with the application that writes it closed, then upload the copy.",
      );
    } finally {
      db.close();
    }
    const name = hash.digest("hex") + ".sqlite";
    await rename(temporary, resolve(dir, name));
    // Sources that name the file are saved by later requests. Windows cannot
    // flush a directory handle; the file's contents are synced above.
    if (platform() !== "win32") {
      const directory = await open(dir, "r");
      try {
        await directory.sync();
      } finally {
        await directory.close();
      }
    }
    return { file: name, bytes: (await stat(resolve(dir, name))).size };
  } finally {
    await rm(temporary, { force: true });
  }
}
