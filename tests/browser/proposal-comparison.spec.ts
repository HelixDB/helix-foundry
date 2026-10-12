import { expect, test } from "@playwright/test";
import { api, openApp } from "./support/workspace";
import { datasetTable } from "../../packages/shared/src/index.js";

test("review exact definition changes and refreshed-input staleness without publishing", async ({
  page,
}, testInfo) => {
  await openApp(page);
  await page.emulateMedia({ reducedMotion: "reduce" });
  const ws = await api(page, "/workspaces", {
    name: "Comparison acceptance " + Date.now(),
  });
  const base = `/workspaces/${ws.id}`;
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    ws.id,
  );
  const ds = await api(page, base + "/ingest", {
    name: "Comparison rows",
    rows: [
      { category: "A", profit: 10 },
      { category: "B", profit: -5 },
    ],
  });
  const table = datasetTable(ds.id);
  const original = {
    summary: "Original definitions",
    assumptions: [],
    catalog: [],
    pipelines: [
      {
        name: "Totals",
        description: "Net profit",
        inputs: [ds.id],
        sql: `SELECT category,profit FROM ${table}`,
        nodes: [],
        checks: [],
      },
    ],
    ontology: [
      {
        name: "Obsolete",
        datasetId: ds.id,
        primaryKey: "category",
        properties: ["category", "profit"],
      },
    ],
    relationships: [],
    actions: [],
    dashboards: [
      {
        name: "Profit",
        widgets: [
          {
            title: "Profit",
            type: "metric",
            inputs: [ds.id],
            sql: `SELECT sum(profit) total FROM ${table}`,
          },
        ],
      },
    ],
  };
  const save = async (bundle: unknown) => {
    const p = (
      await api(page, base + "/definitions/import", {
        version: 1,
        proposals: [bundle],
      })
    ).items[0];
    expect(
      p.data.validation.valid,
      JSON.stringify(p.data.validation.checks),
    ).toBe(true);
    return p;
  };
  const accepted = await save(original);
  await api(page, base + `/proposals/${accepted.id}/publish`, {
    hash: accepted.data.hash,
  });
  const generation = (await api(page, base + "/overview")).workspace.data
    .activeGeneration;
  const nextSql = `SELECT category,profit*2 profit FROM ${table}`;
  const pending = await save({
    ...original,
    summary: "Revised definitions",
    pipelines: [{ ...original.pipelines[0], sql: nextSql }],
    ontology: [],
    dashboards: [
      ...original.dashboards,
      {
        name: "Extra",
        widgets: [
          {
            title: "Rows",
            type: "metric",
            inputs: [ds.id],
            sql: `SELECT count(*) total FROM ${table}`,
          },
        ],
      },
    ],
  });
  await page.goto(`/proposals?selected=${pending.id}`);
  const changes = page.getByRole("region", {
    name: "Definition changes",
    exact: true,
  });
  await expect(changes).toBeVisible();
  await expect(changes).toContainText("1 added · 1 changed · 1 removed");
  await changes
    .getByText("Changed pipelines · Totals", { exact: true })
    .click();
  const pipeline = changes.locator("details").filter({
    has: page.getByText("Changed pipelines · Totals", { exact: true }),
  });
  await expect(pipeline).toContainText(original.pipelines[0].sql);
  await expect(pipeline).toContainText(nextSql);
  await changes
    .getByText("Removed ontology · Obsolete", { exact: true })
    .click();
  await expect(changes).toContainText('"name": "Obsolete"');
  expect(
    (await api(page, base + "/overview")).workspace.data.activeGeneration,
  ).toBe(generation);
  expect(
    (await api(page, base + `/resources/proposal/${pending.id}`)).data.status,
  ).not.toBe("published");
  await page.screenshot({
    path: testInfo.outputPath("proposal-comparison-desktop.png"),
    fullPage: true,
  });
  const refreshed = await api(page, base + "/ingest", {
    name: ds.name,
    datasetId: ds.id,
    rows: [
      { category: "A", profit: 20 },
      { category: "B", profit: -10 },
    ],
  });
  expect(refreshed.data.activeVersion).not.toBe(ds.data.activeVersion);
  await page.reload();
  await expect(changes.getByRole("alert")).toContainText(
    "Inputs or published definitions changed",
  );
  await page.setViewportSize({ width: 390, height: 844 });
  await page.emulateMedia({ reducedMotion: "reduce" });
  await changes
    .getByText("Changed pipelines · Totals", { exact: true })
    .click();
  await expect(changes).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  expect(
    (await api(page, base + "/overview")).workspace.data.activeGeneration,
  ).toBe(generation);
  await page.screenshot({
    path: testInfo.outputPath("proposal-comparison-mobile.png"),
    fullPage: true,
  });
});
