import { afterAll, beforeAll, expect, it, vi } from "vitest";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import { Readable } from "node:stream";
import { randomUUID } from "node:crypto";
import { DatabaseSync } from "node:sqlite";
import { execute as localExecute } from "../services/executor/src/engine.js";
vi.mock("../apps/api/src/executor.js", () => ({
  execute: (job: any) => localExecute(job),
}));
import { discover, importSource } from "../apps/api/src/connectors.js";
import { openSqlite, sqliteDir, stageSqlite } from "../apps/api/src/sqlite.js";
import { config } from "../apps/api/src/config.js";
import { MemoryStore } from "../apps/api/src/store.js";
import { createApp } from "../apps/api/src/app.js";
import { startWorker } from "../apps/api/src/jobs.js";
import { onboardingState } from "../apps/api/src/onboarding-state.js";
import { suggestOntology } from "../apps/api/src/discovery.js";
import { scheduledRefresh } from "../apps/api/src/source-sync.js";

let dir: string, fixture: Buffer;
const scopes: string[] = [];

// Claims data as an application would keep it: a beneficiary table, claims
// that reference it by its primary key without naming the column, claim lines,
// and claim codes with a composite key. A view, a full-text index and its
// shadow tables are not data to import. The file was last written in WAL mode.
beforeAll(async () => {
  dir = await mkdtemp(resolve(tmpdir(), "foundry-sqlite-"));
  const path = resolve(dir, "claims.db");
  const db = new DatabaseSync(path);
  db.exec(`
    PRAGMA journal_mode = WAL;
    CREATE TABLE beneficiaries (mbi TEXT PRIMARY KEY, name TEXT, born TEXT);
    CREATE TABLE claims (
      claim_id INTEGER PRIMARY KEY,
      mbi TEXT REFERENCES beneficiaries,
      paid REAL,
      control_number INTEGER,
      attachment BLOB,
      service_date,
      paid_cents INTEGER GENERATED ALWAYS AS (CAST(paid * 100 AS INTEGER)) VIRTUAL
    );
    CREATE TABLE claim_lines (
      line_id INTEGER PRIMARY KEY,
      claim_id INTEGER REFERENCES claims(claim_id),
      line INTEGER,
      amount NUMERIC
    );
    CREATE TABLE claim_codes (
      claim_id INTEGER REFERENCES claims,
      code TEXT,
      PRIMARY KEY (claim_id, code)
    );
    CREATE TABLE "empty ""quoted"" table" (id INTEGER, label VARCHAR(10), score DOUBLE);
    CREATE VIEW paid_claims AS SELECT * FROM claims WHERE paid > 0;
    CREATE VIRTUAL TABLE notes USING fts5(body);
    INSERT INTO notes VALUES ('follow up');
    INSERT INTO beneficiaries VALUES ('1EG4TE5MK73', 'Zoë Tanaka', '1948-02-11'), ('2FH5UF6NL84', '東京 Patient', '1951-07-30');
  `);
  const claim = db.prepare(
    "INSERT INTO claims (claim_id, mbi, paid, control_number, attachment, service_date) VALUES (?, ?, ?, ?, ?, ?)",
  );
  // Dates as an application might have written them, as text and as a number.
  claim.run(
    1,
    "1EG4TE5MK73",
    120.5,
    9007199254740993n,
    new Uint8Array([1, 2, 3]),
    "2026-01-05",
  );
  claim.run(2, "2FH5UF6NL84", 0, 42, null, 20260210);
  claim.run(3, "1EG4TE5MK73", 75.25, null, null, null);
  const line = db.prepare(
    "INSERT INTO claim_lines (claim_id, line, amount) VALUES (?, ?, ?)",
  );
  for (const [id, n, amount] of [
    [1, 1, 100.5],
    [1, 2, 20],
    [2, 1, 0],
    [3, 1, 75.25],
  ])
    line.run(id, n, amount);
  db.exec("INSERT INTO claim_codes VALUES (1, 'J0'), (1, 'E11')");
  db.close();
  fixture = await readFile(path);
  // The header still says WAL after the connection closes.
  expect(fixture[18]).toBe(2);
});

afterAll(async () => {
  await rm(dir, { recursive: true, force: true });
  for (const scope of scopes)
    for (const part of ["sqlite", "incoming", "datasets"])
      await rm(resolve(config.dataDir, part, scope), {
        recursive: true,
        force: true,
      });
});

const newScope = () => {
  const scope = "sqlite-" + randomUUID();
  scopes.push(scope);
  return scope;
};

it("lists tables, declared keys and samples, and imports every row of a staged file", async () => {
  const scope = newScope();
  const staged = await stageSqlite(scope, Readable.from([fixture]));
  expect(staged.file).toMatch(/^[0-9a-f]{64}\.sqlite$/);
  // The same file stages under the same name.
  expect(await stageSqlite(scope, Readable.from([fixture]))).toEqual(staged);
  const copy = await readFile(resolve(sqliteDir(scope), staged.file));
  expect([copy[18], copy[19]]).toEqual([1, 1]);

  const found: any = await discover("sqlite", { file: staged.file }, scope);
  expect(found.tables).toEqual([
    { name: "beneficiaries", estimatedRows: 2 },
    { name: "claim_codes", estimatedRows: 2 },
    { name: "claim_lines", estimatedRows: 4 },
    { name: "claims", estimatedRows: 3 },
    { name: 'empty "quoted" table', estimatedRows: 0 },
  ]);
  expect(found.foreignKeys).toEqual(
    expect.arrayContaining([
      {
        tableName: "claims",
        columnName: "mbi",
        referencedTable: "beneficiaries",
        referencedColumn: "mbi",
      },
      {
        tableName: "claim_lines",
        columnName: "claim_id",
        referencedTable: "claims",
        referencedColumn: "claim_id",
      },
      {
        tableName: "claim_codes",
        columnName: "claim_id",
        referencedTable: "claims",
        referencedColumn: "claim_id",
      },
    ]),
  );
  expect(found.foreignKeys).toHaveLength(3);
  expect(
    found.keys.map((k: any) => `${k.tableName}.${k.columnName}`).sort(),
  ).toEqual([
    "beneficiaries.mbi",
    "claim_codes.claim_id",
    "claim_codes.code",
    "claim_lines.line_id",
    "claims.claim_id",
  ]);
  expect(found.sample).toEqual([]);

  const preview: any = await discover(
    "sqlite",
    { file: staged.file, table: "beneficiaries" },
    scope,
  );
  expect(preview.sample).toEqual([
    { mbi: "1EG4TE5MK73", name: "Zoë Tanaka", born: "1948-02-11" },
    { mbi: "2FH5UF6NL84", name: "東京 Patient", born: "1951-07-30" },
  ]);

  const claimsPreview: any = await discover(
    "sqlite",
    { file: staged.file, table: "claims" },
    scope,
  );
  expect(claimsPreview.sample.map((r: any) => r.claim_id)).toEqual([1, 2, 3]);
  expect(claimsPreview.sample[0].control_number).toBe("9007199254740993");
  expect(claimsPreview.columns.map((c: any) => c.name)).toEqual([
    "claim_id",
    "mbi",
    "paid",
    "control_number",
    "attachment",
    "service_date",
    "paid_cents",
  ]);

  const imported = await importSource(scope, "sqlite", {
    file: staged.file,
    table: "claims",
  });
  expect(imported.records).toBe(3);
  const rows = (
    await readFile(
      resolve(config.dataDir, "incoming", scope, imported.file),
      "utf8",
    )
  )
    .trim()
    .split("\n")
    .map((l) => JSON.parse(l));
  // A column with an integer beyond the safe range, or with numbers and text,
  // is text in every row. Generated columns are read like the others.
  expect(rows).toEqual([
    {
      claim_id: 1,
      mbi: "1EG4TE5MK73",
      paid: 120.5,
      control_number: "9007199254740993",
      attachment: "AQID",
      service_date: "2026-01-05",
      paid_cents: 12050,
    },
    {
      claim_id: 2,
      mbi: "2FH5UF6NL84",
      paid: 0,
      control_number: "42",
      attachment: null,
      service_date: "20260210",
      paid_cents: 0,
    },
    {
      claim_id: 3,
      mbi: "1EG4TE5MK73",
      paid: 75.25,
      control_number: null,
      attachment: null,
      service_date: null,
      paid_cents: 7525,
    },
  ]);

  // An empty table still has its columns, in types the executor maps.
  const empty = await importSource(scope, "sqlite", {
    file: staged.file,
    table: 'empty "quoted" table',
  });
  expect(empty.records).toBe(0);
  expect(empty.schema).toEqual([
    { name: "id", type: "integer" },
    { name: "label", type: "varchar" },
    { name: "score", type: "double" },
  ]);
});

it("refuses other files, damaged files, unknown tables and names outside the workspace", async () => {
  const scope = newScope();
  await expect(
    stageSqlite(scope, Readable.from([Buffer.from("id,name\n1,Ada\n")])),
  ).rejects.toThrow("Choose a SQLite database file");
  await expect(
    stageSqlite(scope, Readable.from([fixture.subarray(0, 100)])),
  ).rejects.toThrow("damaged");
  const damaged = Buffer.from(fixture);
  damaged.fill(0xff, 4096, 8192);
  await expect(stageSqlite(scope, Readable.from([damaged]))).rejects.toThrow(
    "damaged",
  );

  const { file } = await stageSqlite(scope, Readable.from([fixture]));
  await expect(
    discover("sqlite", { file, table: "paid_claims" }, scope),
  ).rejects.toThrow("Choose a table from the SQLite file");
  await expect(
    importSource(scope, "sqlite", { file, table: "notes_data" }),
  ).rejects.toThrow("no longer in the SQLite file");
  for (const name of ["../" + file, "/etc/passwd", "x.sqlite", undefined])
    await expect(discover("sqlite", { file: name }, scope)).rejects.toThrow(
      "Upload the SQLite file again",
    );
  // Another workspace cannot read this workspace's file.
  await expect(discover("sqlite", { file }, newScope())).rejects.toThrow(
    "Upload the SQLite file again",
  );
  await expect(discover("sqlite", { file })).rejects.toThrow(
    "belong to a workspace",
  );
  // A connection may only read, whatever SQL reaches it.
  const db = await openSqlite(scope, { file });
  try {
    for (const sql of [
      "ATTACH DATABASE ':memory:' AS other",
      "SELECT load_extension('/tmp/x')",
      "PRAGMA writable_schema = ON",
      "DELETE FROM claims",
      "SELECT readfile('/etc/passwd')",
    ])
      expect(() => db.prepare(sql), sql).toThrow();
    expect(db.prepare("SELECT count(*) AS n FROM claims").get()).toEqual({
      n: 3n,
    });
  } finally {
    db.close();
  }
});

it("uploads a SQLite file, imports the selected tables and suggests their declared links", async () => {
  const store = new MemoryStore(),
    app = await createApp(store);
  let stop = () => {};
  try {
    const me = await app.inject({ method: "GET", url: "/api/v1/me" });
    const scope = me.json().workspaces[0].id;
    scopes.push(scope);
    const upload = (name: string, body: Buffer) => {
      const boundary = "sqlite-boundary";
      return app.inject({
        method: "POST",
        url: `/api/v1/workspaces/${scope}/sources/sqlite`,
        headers: {
          "content-type": `multipart/form-data; boundary=${boundary}`,
        },
        payload: Buffer.concat([
          Buffer.from(
            `--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="${name}"\r\nContent-Type: application/octet-stream\r\n\r\n`,
          ),
          body,
          Buffer.from(`\r\n--${boundary}--\r\n`),
        ]),
      });
    };
    const post = (path: string, payload: unknown) =>
      app.inject({
        method: "POST",
        url: `/api/v1/workspaces/${scope}` + path,
        payload: payload as any,
      });

    const wrongType = await upload("claims.csv", fixture);
    expect(wrongType.statusCode).toBe(400);
    expect(wrongType.json().error).toContain("Choose a SQLite database file");
    const notSqlite = await upload("claims.db", Buffer.from("not a database"));
    expect(notSqlite.statusCode).toBe(400);

    const staged = await upload("claims.db", fixture);
    expect(staged.statusCode, staged.body).toBe(200);
    const { file } = staged.json();

    const tested = await post("/sources/test", {
      name: "claims",
      kind: "sqlite",
      config: { file },
    });
    expect(tested.statusCode, tested.body).toBe(200);
    expect(tested.json().tables.map((t: any) => t.name)).toContain("claims");

    const outside = await post("/onboarding/sources", {
      name: "claims",
      kind: "sqlite",
      config: { file },
      selections: [{ table: "paid_claims" }],
      operationId: "sqlite-view-selection",
    });
    expect(outside.statusCode).toBe(400);

    const created = await post("/onboarding/sources", {
      name: "claims",
      kind: "sqlite",
      config: { file },
      selections: [
        { table: "beneficiaries" },
        { table: "claims" },
        { table: "claim_lines" },
      ],
      operationId: "sqlite-connection",
    });
    expect(created.statusCode, created.body).toBe(200);
    expect(created.json()).toHaveLength(3);
    // The workspace never sees where the file is kept or its settings.
    expect(JSON.stringify(created.json())).not.toContain(file);

    stop = startWorker(store);
    await expect
      .poll(async () => (await onboardingState(store, scope)).inputsReady, {
        timeout: 30000,
        interval: 250,
      })
      .toBe(true);
    const state = await onboardingState(store, scope);
    expect(
      Object.fromEntries(
        state.datasets.map((d) => [d.name, d.data.profile.rows]),
      ),
    ).toEqual({ beneficiaries: 2, claims: 3, claim_lines: 4 });
    const claims = state.datasets.find((d) => d.name === "claims")!;
    expect(
      Object.fromEntries(
        claims.data.profile.columns.map((c: any) => [c.name, c.type]),
      ),
    ).toMatchObject({
      claim_id: "BIGINT",
      paid: "DOUBLE",
      control_number: "VARCHAR",
      service_date: "VARCHAR",
      attachment: "VARCHAR",
    });
    for (const source of state.sources) {
      expect(source.data.nextRun).toBeNull();
      expect(scheduledRefresh(source)).toBe(false);
    }

    const suggestion = await suggestOntology(store, scope, {
      datasets: state.datasets,
      sources: state.sources,
      signature: state.signature,
    });
    const declared = suggestion.links
      .filter((l) => l.evidence === "declared")
      .map((l) => `${l.fromColumn} ${l.toColumn}`)
      .sort();
    expect(declared).toEqual(["claim_id claim_id", "mbi mbi"]);

    // Replacing the file re-imports each table into the dataset it has, so
    // nothing is duplicated. Given only the new file, a source keeps its table.
    const copy = async (name: string, sql: string) => {
      const path = resolve(dir, name);
      await writeFile(path, fixture);
      const db = new DatabaseSync(path);
      db.exec(sql);
      db.close();
      return readFile(path);
    };
    const put = (id: string, payload: unknown) =>
      app.inject({
        method: "PUT",
        url: `/api/v1/workspaces/${scope}/onboarding/sources/${id}`,
        payload: payload as any,
      });
    const replaced = await upload(
      "claims.db",
      await copy(
        "claims-new.db",
        "INSERT INTO beneficiaries VALUES ('3GJ6VG7PM95', 'Ana Ruiz', '1960-03-02')",
      ),
    );
    expect(replaced.statusCode, replaced.body).toBe(200);
    expect(replaced.json().file).not.toBe(file);
    for (const source of state.sources) {
      const r = await put(source.id, {
        name: source.name,
        kind: "sqlite",
        config: { file: replaced.json().file },
      });
      expect(r.statusCode, r.body).toBe(200);
    }
    await expect
      .poll(
        async () =>
          (await onboardingState(store, scope)).datasets.find(
            (d) => d.name === "beneficiaries",
          )?.data.profile.rows,
        { timeout: 30000, interval: 250 },
      )
      .toBe(3);
    const after = await onboardingState(store, scope);
    expect(after.sources.map((s) => s.id).sort()).toEqual(
      state.sources.map((s) => s.id).sort(),
    );
    expect(after.datasets.map((d) => d.id).sort()).toEqual(
      state.datasets.map((d) => d.id).sort(),
    );

    // A copy without the source's table is refused and changes nothing.
    const partial = await upload(
      "claims.db",
      await copy("claims-partial.db", "DROP TABLE claim_lines"),
    );
    expect(partial.statusCode, partial.body).toBe(200);
    const lines = after.sources.find((s) => s.name === "claim_lines")!;
    const refused = await put(lines.id, {
      name: lines.name,
      kind: "sqlite",
      config: { file: partial.json().file },
    });
    expect(refused.statusCode).toBe(400);
    expect(refused.json().error).toBe("Choose a table from the SQLite file");
    expect(
      (await onboardingState(store, scope)).datasets.find(
        (d) => d.name === "claim_lines",
      )?.data.profile.rows,
    ).toBe(4);
  } finally {
    stop();
    await app.close();
  }
});
