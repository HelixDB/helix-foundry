import { expect, test } from "@playwright/test";
import { api, openApp } from "./support/workspace";
import { datasetTable } from "../../packages/shared/src/index.js";

test("save, reopen, edit and rerun native SQL against refreshed snapshots", async ({
  page,
}, testInfo) => {
  await openApp(page);
  await page.emulateMedia({ reducedMotion: "reduce" });
  const ws = await api(page, "/workspaces", {
    name: "SQL acceptance " + Date.now(),
  });
  const base = `/workspaces/${ws.id}`;
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    ws.id,
  );
  const ds = await api(page, base + "/ingest", {
    name: "SQL acceptance rows",
    rows: [
      { category: "A", profit: 10 },
      { category: "B", profit: -5 },
    ],
  });
  await page.goto("/data");
  await page.getByRole("link", { name: "SQL queries", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "SQL queries", exact: true }),
  ).toBeVisible();
  await page
    .getByRole("textbox", { name: "Query name", exact: true })
    .fill("Net profit");
  await page.getByRole("checkbox", { name: /SQL acceptance rows/ }).check();
  const sql = `SELECT sum(profit) total FROM ${datasetTable(ds.id)}`;
  await page.getByRole("textbox", { name: "SQL", exact: true }).fill(sql);
  await page.getByRole("button", { name: "Run query", exact: true }).click();
  await expect(
    page.getByRole("cell", { name: "5", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Save query", exact: true }).click();
  await expect(page).toHaveURL(/\/data\/queries\/.+/);
  const queryId = page.url().split("/").at(-1)!;
  await page.reload();
  await expect(
    page.getByRole("textbox", { name: "SQL", exact: true }),
  ).toHaveValue(sql);
  await expect(
    page.getByRole("textbox", { name: "Query name", exact: true }),
  ).toHaveValue("Net profit");
  const refreshed = await api(page, base + "/ingest", {
    name: ds.name,
    datasetId: ds.id,
    rows: [
      { category: "A", profit: 40 },
      { category: "B", profit: -20 },
    ],
  });
  await page.getByRole("button", { name: "Run query", exact: true }).click();
  await expect(
    page.getByRole("cell", { name: "20", exact: true }),
  ).toBeVisible();
  await page.getByText("Snapshot sources", { exact: true }).click();
  await expect(
    page.getByText(refreshed.data.activeVersion, { exact: true }),
  ).toBeVisible();
  const edited = `SELECT category,profit FROM ${datasetTable(ds.id)} ORDER BY category`;
  await page.getByRole("textbox", { name: "SQL", exact: true }).fill(edited);
  await page
    .getByRole("textbox", { name: "Query name", exact: true })
    .fill("Current profit rows");
  await page.getByRole("button", { name: "Save query", exact: true }).click();
  await expect
    .poll(
      async () =>
        (await api(page, base + `/resources/query/${queryId}`)).data.sql,
    )
    .toBe(edited);
  await page.getByRole("button", { name: "Run query", exact: true }).click();
  await expect(
    page.getByRole("cell", { name: "40", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "-20", exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("saved-query-workspace-desktop.png"),
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.emulateMedia({ reducedMotion: "reduce" });
  await expect(
    page.getByRole("textbox", { name: "SQL", exact: true }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.getByText("Snapshot sources", { exact: true }).click();
  await expect(
    page.getByText(refreshed.data.activeVersion, { exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("saved-query-workspace-mobile.png"),
    fullPage: true,
  });
  await page
    .getByRole("textbox", { name: "SQL", exact: true })
    .fill(`DELETE FROM ${datasetTable(ds.id)}`);
  await page.getByRole("button", { name: "Run query", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText("SELECT");
  expect((await api(page, base + `/resources/query/${queryId}`)).data.sql).toBe(
    edited,
  );
  page.once("dialog", (dialog) => dialog.accept());
  await page.getByRole("button", { name: "Delete query", exact: true }).click();
  await expect(page).toHaveURL(/\/data\/queries$/);
  await expect(
    page.getByRole("link", { name: "Current profit rows", exact: true }),
  ).toHaveCount(0);
  const removed = await page.request.get(
    "/api/v1" + base + `/resources/query/${queryId}`,
  );
  expect(removed.status()).toBe(404);
});

test("background refetch preserves a stale editor's revision and repeated own saves advance it", async ({
  page,
}, testInfo) => {
  await openApp(page);
  await page.emulateMedia({ reducedMotion: "reduce" });
  const ws = await api(page, "/workspaces", {
    name: "Concurrent SQL " + Date.now(),
  });
  const base = `/workspaces/${ws.id}`;
  const ds = await api(page, base + "/ingest", {
    name: "Concurrent rows",
    rows: [{ profit: 7 }],
  });
  const initialSql = `SELECT profit FROM ${datasetTable(ds.id)}`;
  const externalSql = `SELECT profit*10 profit FROM ${datasetTable(ds.id)}`;
  const saved = await api(page, base + "/queries", {
    name: "Concurrent query",
    inputs: [ds.id],
    sql: initialSql,
  });
  const resourcePath = "/api/v1" + base + `/resources/query/${saved.id}`;
  const updatePath = "/api/v1" + base + `/queries/${saved.id}`;
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    ws.id,
  );
  await page.goto(`/data/queries/${saved.id}`);
  const sql = page.getByRole("textbox", { name: "SQL", exact: true });
  const name = page.getByRole("textbox", { name: "Query name", exact: true });
  await expect(sql).toHaveValue(initialSql);
  await name.fill("Local draft");
  const update = await page.request.put(updatePath, {
    data: {
      name: "Other editor",
      inputs: [ds.id],
      sql: externalSql,
      revision: saved.revision,
    },
  });
  expect(update.status()).toBe(200);
  const external = await update.json();
  expect(external.revision).toBe(saved.revision + 1);
  await page.context().setOffline(true);
  await expect.poll(() => page.evaluate(() => navigator.onLine)).toBe(false);
  await expect
    .poll(async () => Date.now() - Date.parse(external.updatedAt), {
      timeout: 10000,
    })
    .toBeGreaterThan(5100);
  const refetched = page.waitForResponse(
    (r) =>
      new URL(r.url()).pathname === resourcePath &&
      r.request().method() === "GET",
  );
  await page.context().setOffline(false);
  await expect.poll(() => page.evaluate(() => navigator.onLine)).toBe(true);
  expect((await (await refetched).json()).revision).toBe(external.revision);
  await expect(sql).toHaveValue(initialSql);
  await expect(name).toHaveValue("Local draft");
  const run = page.waitForResponse(
    (r) =>
      new URL(r.url()).pathname === "/api/v1" + base + "/query" &&
      r.request().method() === "POST",
  );
  await page.getByRole("button", { name: "Run query", exact: true }).click();
  expect((await run).status()).toBe(200);
  await expect(
    page.getByRole("cell", { name: "7", exact: true }),
  ).toBeVisible();
  const stale = page.waitForResponse(
    (r) =>
      new URL(r.url()).pathname === updatePath &&
      r.request().method() === "PUT",
  );
  await page.getByRole("button", { name: "Save query", exact: true }).click();
  const staleResponse = await stale;
  const persisted = await api(page, base + `/resources/query/${saved.id}`);
  await testInfo.attach("concurrent-save-receipt", {
    contentType: "application/json",
    body: Buffer.from(
      JSON.stringify(
        {
          initial: saved,
          external,
          submitted: staleResponse.request().postDataJSON(),
          status: staleResponse.status(),
          persisted,
        },
        null,
        2,
      ),
    ),
  });
  expect(staleResponse.status()).toBe(409);
  await expect(page.getByRole("alert")).toContainText("Reload before saving");
  expect(persisted.name).toBe("Other editor");
  expect(persisted.data.sql).toBe(externalSql);
  expect(persisted.revision).toBe(external.revision);
  await page.reload();
  await expect(sql).toHaveValue(externalSql);
  for (const nextName of ["Own save one", "Own save two"]) {
    const before = await api(page, base + `/resources/query/${saved.id}`);
    await name.fill(nextName);
    const success = page.waitForResponse(
      (r) =>
        new URL(r.url()).pathname === updatePath &&
        r.request().method() === "PUT",
    );
    await page.getByRole("button", { name: "Save query", exact: true }).click();
    const response = await success;
    expect(response.status()).toBe(200);
    expect(response.request().postDataJSON().revision).toBe(before.revision);
    await expect
      .poll(
        async () =>
          (await api(page, base + `/resources/query/${saved.id}`)).name,
      )
      .toBe(nextName);
  }
});
