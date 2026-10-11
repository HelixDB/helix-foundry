import { test, expect } from "@playwright/test";
import { api, openApp } from "./support/workspace";
import { datasetTable } from "../../packages/shared/src/index.js";

test("owner defines persistent Home metrics, revisits and refreshes them without AI", async ({
  page,
}, testInfo) => {
  await openApp(page);
  await page.emulateMedia({ reducedMotion: "reduce" });
  const workspace = await api(page, "/workspaces", {
    name: "Pinned metrics acceptance",
  });
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    workspace.id,
  );
  const base = `/workspaces/${workspace.id}`;
  await api(page, base + "/onboarding/provider", {
    provider: "local",
    model: "qwen3:4b",
  });
  const dataset = await api(page, base + "/ingest", {
    name: "Revenue accounts",
    rows: [
      { id: 1, amount: 150 },
      { id: 2, amount: 250 },
    ],
  });
  const proposal = (
    await api(page, base + "/definitions/import", {
      version: 1,
      proposals: [
        {
          summary: "Revenue accounts",
          assumptions: [],
          catalog: [],
          pipelines: [],
          ontology: [],
          relationships: [],
          actions: [],
          dashboards: [
            {
              name: "Accounts",
              widgets: [
                {
                  title: "Accounts",
                  type: "metric",
                  inputs: [dataset.id],
                  sql: `SELECT count(*) accounts FROM ${datasetTable(dataset.id)}`,
                },
              ],
            },
          ],
        },
      ],
    })
  ).items[0];
  expect(proposal.data.validation.valid).toBe(true);
  await api(page, base + `/proposals/${proposal.id}/publish`, {
    hash: proposal.data.hash,
  });
  await page.goto("/");
  const section = page.getByRole("region", {
    name: "Pinned metrics",
    exact: true,
  });
  await expect(section).toBeVisible();
  await section
    .getByRole("button", { name: "Pin metric", exact: true })
    .click();
  const dialog = page.getByRole("dialog", {
    name: "Pin a metric",
    exact: true,
  });
  await dialog.getByLabel("Title", { exact: true }).fill("Owner revenue");
  await dialog.getByRole("combobox", { name: "Measure", exact: true }).selectOption("sum");
  await dialog
    .getByRole("combobox", { name: "Numeric column", exact: true })
    .selectOption("amount");
  await dialog.getByRole("combobox", { name: "Format", exact: true }).selectOption("currency");
  await dialog.getByRole("combobox", { name: "Units", exact: true }).selectOption("cents");
  await dialog
    .getByRole("button", { name: "Save metric", exact: true })
    .click();
  await expect(dialog).toHaveCount(0);
  await expect(section.getByText("$4", { exact: true })).toBeVisible();
  const saved = await api(page, base + "/home/pinned-metrics");
  expect(saved.items[0].spec).toMatchObject({
    title: "Owner revenue",
    measure: "sum",
    column: "amount",
    scale: "cents",
  });
  expect(saved.items[0].version).toBe(dataset.data.activeVersion);
  await page.reload();
  await expect(section.getByText("$4", { exact: true })).toBeVisible();
  await api(page, base + "/home/metrics/regenerate", {});
  await page.reload();
  await expect(
    section.getByText("Owner revenue", { exact: true }),
  ).toBeVisible();
  const changed = await api(page, base + "/ingest", {
    name: dataset.name,
    datasetId: dataset.id,
    rows: [{ id: 3, amount: 750 }],
  });
  await section
    .getByRole("button", { name: "Refresh pinned metrics", exact: true })
    .click();
  await expect(section.getByText("$7.50", { exact: true })).toBeVisible();
  await section.getByText("Definition and evidence", { exact: true }).click();
  await expect(
    section.getByText(
      `Revenue accounts · Snapshot ${changed.data.activeVersion}`,
      { exact: true },
    ),
  ).toBeVisible();
  await expect(section.locator("pre")).toContainText(
    'sum(TRY_CAST("amount" AS DOUBLE)) / 100.0',
  );
  await section
    .getByRole("button", { name: "Edit Owner revenue", exact: true })
    .click();
  const edit = page.getByRole("dialog", {
    name: "Edit pinned metric",
    exact: true,
  });
  await edit.getByLabel("Title", { exact: true }).fill("Owner average");
  await edit.getByRole("combobox", { name: "Measure", exact: true }).selectOption("avg");
  await expect(edit.getByRole("combobox", { name: "Numeric column", exact: true })).toHaveValue("amount");
  await edit.getByRole("button", { name: "Save metric", exact: true }).click();
  await expect(edit).toHaveCount(0);
  await expect(
    section.getByText("Owner average", { exact: true }),
  ).toBeVisible();
  await expect(section.getByText("$7.50", { exact: true })).toBeVisible();
  await page.screenshot({ path: testInfo.outputPath("pinned-metrics-desktop.png"), fullPage: true });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(
    section.getByRole("button", { name: "Edit Owner average", exact: true }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.screenshot({ path: testInfo.outputPath("pinned-metrics-mobile.png"), fullPage: true });
  await section
    .getByRole("button", { name: "Remove Owner average", exact: true })
    .click();
  await page
    .getByRole("dialog", { name: "Remove pinned metric?", exact: true })
    .getByRole("button", { name: "Remove metric", exact: true })
    .click();
  await expect(section.getByText("Owner average", { exact: true })).toHaveCount(
    0,
  );
  expect((await api(page, base + "/home/pinned-metrics")).items).toEqual([]);
  expect(
    (await api(page, base + `/resources/dataset/${dataset.id}`)).data
      .activeVersion,
  ).toBe(changed.data.activeVersion);
});
