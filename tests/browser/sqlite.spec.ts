import { test, expect } from "@playwright/test";
import { mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { resolve } from "node:path";
import { DatabaseSync } from "node:sqlite";
import { artifact } from "./support/ports";
import { api, openApp } from "./support/workspace";

// A small claims database: claims reference beneficiaries, and claim lines
// reference claims, through declared foreign keys.
async function claimsDatabase() {
  const dir = await mkdtemp(resolve(tmpdir(), "foundry-sqlite-spec-"));
  try {
    const db = new DatabaseSync(resolve(dir, "claims.db"));
    db.exec(`
      CREATE TABLE beneficiaries (mbi TEXT PRIMARY KEY, name TEXT);
      CREATE TABLE claims (claim_id INTEGER PRIMARY KEY, mbi TEXT REFERENCES beneficiaries, paid REAL);
      CREATE TABLE claim_lines (line_id INTEGER PRIMARY KEY, claim_id INTEGER REFERENCES claims, amount REAL);
      CREATE VIEW paid_claims AS SELECT * FROM claims WHERE paid > 0;
      INSERT INTO beneficiaries VALUES ('1EG4TE5MK73', 'Zoë Tanaka'), ('2FH5UF6NL84', 'Sam Okafor');
      INSERT INTO claims VALUES (1, '1EG4TE5MK73', 120.5), (2, '2FH5UF6NL84', 0), (3, '1EG4TE5MK73', 75.25);
      INSERT INTO claim_lines (claim_id, amount) VALUES (1, 100.5), (1, 20), (2, 0), (3, 75.25);
    `);
    db.close();
    return await readFile(resolve(dir, "claims.db"));
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}

test("reads a SQLite file's tables and imports the selected ones in setup", async ({
  page,
}) => {
  await openApp(page);
  const workspace = await api(page, "/workspaces", { name: "SQLite claims" });
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
  const read = dialog.getByRole("button", { name: "Read file", exact: true });
  await expect(read).toBeDisabled();
  await dialog.getByLabel("SQLite database file").setInputFiles({
    name: "claims.db",
    mimeType: "application/vnd.sqlite3",
    buffer: await claimsDatabase(),
  });
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
      "Imported from the uploaded file. Upload a new copy to refresh it.",
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
});
