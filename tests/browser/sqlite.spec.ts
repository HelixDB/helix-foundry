import { test, expect, type Page } from "@playwright/test";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { artifact } from "./support/ports";
import { api, openApp, sampleWorkspace } from "./support/workspace";

async function sqliteFile(name: string, sql: string) {
  const dir = await mkdtemp(resolve(tmpdir(), "foundry-sqlite-spec-"));
  try {
    const db = new DatabaseSync(resolve(dir, name));
    db.exec(sql);
    db.close();
    return {
      name,
      mimeType: "application/vnd.sqlite3",
      buffer: await readFile(resolve(dir, name)),
    };
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}

// A small claims database: claims reference beneficiaries, and claim lines
// reference claims, through declared foreign keys.
// Later statements change a copy, as the application would.
const claimsDatabase = (later = "") =>
  sqliteFile(
    "claims.db",
    `
      CREATE TABLE beneficiaries (mbi TEXT PRIMARY KEY, name TEXT);
      CREATE TABLE claims (claim_id INTEGER PRIMARY KEY, mbi TEXT REFERENCES beneficiaries, paid REAL);
      CREATE TABLE claim_lines (line_id INTEGER PRIMARY KEY, claim_id INTEGER REFERENCES claims, amount REAL);
      CREATE VIEW paid_claims AS SELECT * FROM claims WHERE paid > 0;
      INSERT INTO beneficiaries VALUES ('1EG4TE5MK73', 'Zoë Tanaka'), ('2FH5UF6NL84', 'Sam Okafor');
      INSERT INTO claims VALUES (1, '1EG4TE5MK73', 120.5), (2, '2FH5UF6NL84', 0), (3, '1EG4TE5MK73', 75.25);
      INSERT INTO claim_lines (claim_id, amount) VALUES (1, 100.5), (1, 20), (2, 0), (3, 75.25);
      ${later}
    `,
  );

// A second database whose fills reference prescribers.
const pharmacyDatabase = () =>
  sqliteFile(
    "pharmacy.sqlite",
    `
      CREATE TABLE prescribers (npi TEXT PRIMARY KEY, name TEXT);
      CREATE TABLE fills (fill_id INTEGER PRIMARY KEY, npi TEXT REFERENCES prescribers, cost REAL);
      INSERT INTO prescribers VALUES ('1234567893', 'Dr. Ada Mensah');
      INSERT INTO fills (npi, cost) VALUES ('1234567893', 18.4), ('1234567893', 6.15);
    `,
  );

// Opens the SQLite card on the Data step of a new workspace.
async function openSqlite(page: Page, name: string) {
  await openApp(page);
  const workspace = await api(page, "/workspaces", { name });
  const base = `/workspaces/${workspace.id}`;
  await api(page, base + "/onboarding/provider", {
    provider: "local",
    model: "qwen3:4b",
  });
  await page.request.patch("/api/v1" + base + "/onboarding", {
    data: { step: "data" },
  });
  await expect
    .poll(async () => (await api(page, base + "/onboarding")).providerReady, {
      timeout: 30000,
    })
    .toBe(true);
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    workspace.id,
  );
  await page.goto("/onboarding");
  await page.getByRole("button", { name: "SQLite", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "SQLite", exact: true });
  await expect(dialog).toBeVisible();
  return dialog;
}

test("reads a SQLite file's tables and imports the selected ones in setup", async ({
  page,
}) => {
  const dialog = await openSqlite(page, "SQLite claims");
  const read = dialog.getByRole("button", { name: "Read file", exact: true });
  await expect(read).toBeDisabled();
  await dialog
    .getByLabel("SQLite database files")
    .setInputFiles(await claimsDatabase());
  await read.click();

  // Tables only: the view is not offered.
  for (const table of ["beneficiaries", "claim_lines", "claims"])
    await expect(dialog.getByLabel(table, { exact: true })).toBeVisible();
  await expect(dialog.getByText("paid_claims")).toHaveCount(0);
  await dialog.getByLabel("claims", { exact: true }).check();
  // Declared foreign keys point out the tables that link to the selection.
  await expect(dialog.getByText("Related to your selection")).toHaveCount(2);
  await dialog
    .getByRole("button", { name: "Preview", exact: true })
    .nth(2)
    .click();
  await expect(dialog.getByText("Sample and schema")).toBeVisible();
  await expect(dialog.getByRole("cell", { name: "120.5" })).toBeVisible();
  await page.screenshot({
    path: artifact("sqlite-tables.png"),
    fullPage: true,
  });
  await dialog.getByRole("button", { name: "Select all", exact: true }).click();
  await dialog
    .getByRole("button", { name: "Import 3 selected", exact: true })
    .click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "SQLite", exact: true }),
  ).toHaveAccessibleDescription(/1 added/);

  await page.getByRole("button", { name: "SQLite", exact: true }).click();
  await expect(
    page.getByText(
      "Imported from the uploaded file. Replace the file to refresh it.",
    ),
  ).toBeVisible();
  for (const [name, rows] of [
    ["beneficiaries", 2],
    ["claims", 3],
    ["claim_lines", 4],
  ])
    await expect(
      page.getByText(`${name} · ${rows} records`, { exact: true }),
    ).toBeVisible({ timeout: 30000 });
  await page.screenshot({
    path: artifact("sqlite-imported.png"),
    fullPage: true,
  });

  // A copy without one of the connection's tables changes nothing.
  const replaceFile = async (file: Awaited<ReturnType<typeof sqliteFile>>) => {
    const chooser = page.waitForEvent("filechooser");
    await page
      .getByRole("button", { name: "Replace file", exact: true })
      .click();
    await (await chooser).setFiles(file);
  };
  await replaceFile(await claimsDatabase("DROP TABLE claim_lines;"));
  await expect(page.getByRole("alert")).toContainText(
    "claims.db has no table named claim_lines. Choose a copy of the same database.",
  );
  await expect(
    page.getByText("claim_lines · 4 records", { exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: artifact("sqlite-replace-refused.png"),
    fullPage: true,
  });

  // Replacing the file updates the connection's datasets in place.
  await replaceFile(
    await claimsDatabase(
      "INSERT INTO beneficiaries VALUES ('3GJ6VG7PM95', 'Ana Ruiz');",
    ),
  );
  await expect(
    page.getByText("beneficiaries · 3 records", { exact: true }),
  ).toBeVisible({ timeout: 30000 });
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect(page.locator(".connection-entry")).toHaveCount(1);
  for (const name of ["beneficiaries", "claims", "claim_lines"])
    await expect(
      page.getByText(new RegExp(`^${name} · \\d+ records$`)),
    ).toHaveCount(1);
  await page.screenshot({
    path: artifact("sqlite-replaced.png"),
    fullPage: true,
  });
});

test("reads several SQLite files at once and imports each as its own connection", async ({
  page,
}) => {
  const dialog = await openSqlite(page, "SQLite claims and pharmacy");
  await dialog.getByLabel("SQLite database files").setInputFiles([
    await claimsDatabase(),
    await pharmacyDatabase(),
    {
      name: "notes.db",
      mimeType: "application/octet-stream",
      buffer: Buffer.from("These are notes, not a database."),
    },
  ]);
  await dialog.getByRole("button", { name: "Read files", exact: true }).click();

  const claims = dialog.getByRole("group", { name: "claims.db", exact: true }),
    pharmacy = dialog.getByRole("group", {
      name: "pharmacy.sqlite",
      exact: true,
    }),
    notes = dialog.getByRole("group", { name: "notes.db", exact: true });
  for (const table of ["beneficiaries", "claim_lines", "claims"])
    await expect(claims.getByLabel(table, { exact: true })).toBeVisible();
  for (const table of ["fills", "prescribers"])
    await expect(pharmacy.getByLabel(table, { exact: true })).toBeVisible();
  // A file that is not SQLite shows its error and does not stop the others.
  await expect(notes.getByRole("alert")).toContainText(
    "Choose a SQLite database file",
  );
  await expect(notes.getByRole("checkbox")).toHaveCount(0);

  // Related tables and previews stay within their own file.
  await pharmacy.getByLabel("fills", { exact: true }).check();
  await expect(pharmacy.getByText("Related to your selection")).toHaveCount(1);
  await expect(claims.getByText("Related to your selection")).toHaveCount(0);
  await pharmacy
    .getByRole("button", { name: "Preview", exact: true })
    .first()
    .click();
  await expect(dialog.getByRole("cell", { name: "18.4" })).toBeVisible();

  // Select all and Clear act on one file.
  await claims.getByRole("button", { name: "Select all", exact: true }).click();
  await expect(
    pharmacy.getByRole("checkbox", { name: /^prescribers/ }),
  ).not.toBeChecked();
  await pharmacy.getByRole("button", { name: "Clear", exact: true }).click();
  await expect(claims.getByLabel("claims", { exact: true })).toBeChecked();
  await pharmacy
    .getByRole("button", { name: "Select all", exact: true })
    .click();
  await page.screenshot({
    path: artifact("sqlite-several-files.png"),
    fullPage: true,
  });
  await dialog
    .getByRole("button", { name: "Import 5 selected", exact: true })
    .click();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "SQLite", exact: true }),
  ).toHaveAccessibleDescription(/2 added/);

  await page.getByRole("button", { name: "SQLite", exact: true }).click();
  for (const connection of ["claims", "pharmacy"])
    await expect(
      page.locator(".connection-entry-heading strong", {
        hasText: new RegExp(`^${connection}$`),
      }),
    ).toBeVisible();
  for (const [name, rows] of [
    ["beneficiaries", 2],
    ["claims", 3],
    ["claim_lines", 4],
    ["prescribers", 1],
    ["fills", 2],
  ])
    await expect(
      page.getByText(`${name} · ${rows} records`, { exact: true }),
    ).toBeVisible({ timeout: 30000 });
});

test("replaces a SQLite connection's file from the Data page", async ({
  page,
}) => {
  await openApp(page);
  const { base } = await sampleWorkspace(page, "SQLite replace on Data");
  const original = await claimsDatabase();
  const staged = await page.request.post(`/api/v1${base}/sources/sqlite`, {
    multipart: { file: original },
  });
  expect(staged.ok(), await staged.text()).toBeTruthy();
  await api(page, base + "/onboarding/sources", {
    name: "claims",
    kind: "sqlite",
    config: { file: (await staged.json()).file },
    selections: [
      { table: "beneficiaries" },
      { table: "claims" },
      { table: "claim_lines" },
    ],
    operationId: "sqlite-replace-on-data",
  });
  await page.goto("/data");
  const card = page.locator(".cn-card", {
    has: page.getByRole("heading", { name: "claims", exact: true }),
  });
  const rows = (table: string) =>
    card.getByRole("link", { name: new RegExp(`^${table}`) });
  await expect(
    card.getByText("Uploaded file · replace the file to refresh"),
  ).toBeVisible();
  // Re-reading an unchanged file is not offered as a refresh.
  await expect(card.getByRole("button", { name: "Refresh" })).toHaveCount(0);
  await expect(async () => {
    await page.reload();
    await expect(rows("beneficiaries")).toContainText("2", { timeout: 1000 });
  }).toPass({ timeout: 30000 });

  const chooser = page.waitForEvent("filechooser");
  await card.getByRole("button", { name: "Replace file", exact: true }).click();
  await (
    await chooser
  ).setFiles(
    await claimsDatabase(
      "INSERT INTO beneficiaries VALUES ('3GJ6VG7PM95', 'Ana Ruiz');",
    ),
  );
  await expect(page.locator(".hf-toast")).toContainText(
    "File replaced. Importing its tables…",
  );
  await expect(async () => {
    await page.reload();
    await expect(rows("beneficiaries")).toContainText("3", { timeout: 1000 });
  }).toPass({ timeout: 30000 });
  // Still one connection with each table once.
  await expect(card).toHaveCount(1);
  for (const table of ["beneficiaries", "claims", "claim_lines"])
    await expect(rows(table)).toHaveCount(1);
  await page.screenshot({
    path: artifact("sqlite-replaced-data-page.png"),
    fullPage: true,
  });
});
