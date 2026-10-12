import { expect, test } from "@playwright/test";
import { api, openApp } from "./support/workspace";
import { datasetTable } from "../../packages/shared/src/index.js";

test("author, review, publish and refresh an accepted SQL pipeline through native output", async ({
  page,
}, testInfo) => {
  await openApp(page);
  await page.emulateMedia({ reducedMotion: "reduce" });
  const ws = await api(page, "/workspaces", {
    name: "Pipeline acceptance " + Date.now(),
  });
  const base = `/workspaces/${ws.id}`;
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    ws.id,
  );
  const ds = await api(page, base + "/ingest", {
    name: "Pipeline acceptance rows",
    rows: [
      { category: "A", profit: 10 },
      { category: "B", profit: -5 },
    ],
  });
  await page.goto("/data");
  await page.getByRole("link", { name: "Pipelines", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Pipelines", exact: true }),
  ).toBeVisible();
  await page
    .getByRole("textbox", { name: "Pipeline name", exact: true })
    .fill("Scaled profit");
  await page
    .getByRole("checkbox", { name: /Pipeline acceptance rows/ })
    .check();
  const sql = (factor: number) =>
    `SELECT category,profit*${factor} profit FROM ${datasetTable(ds.id)} ORDER BY category`;
  await page.getByRole("textbox", { name: "SQL", exact: true }).fill(sql(2));
  await page
    .getByRole("button", { name: "Create review", exact: true })
    .click();
  await expect(page).toHaveURL(/\/proposals\?selected=/);
  await page.getByText(/^Details · \d+ checks passed$/).click();
  await page.getByText("Definitions and evidence", { exact: true }).click();
  await expect(
    page.getByRole("cell", { name: "20", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "-10", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  let accepted: any;
  await expect
    .poll(async () => {
      accepted = (await api(page, base + "/resources/pipeline")).items.find(
        (p: any) => p.name === "Scaled profit",
      );
      return !!accepted;
    })
    .toBe(true);
  const first = await api(
    page,
    base + `/resources/dataset/${accepted.data.outputDatasetId}`,
  );
  await page.goto("/data/pipelines/" + accepted.id);
  await expect(
    page.getByRole("textbox", { name: "SQL", exact: true }),
  ).toHaveValue(sql(2));
  await api(page, base + "/ingest", {
    name: ds.name,
    datasetId: ds.id,
    rows: [
      { category: "A", profit: 40 },
      { category: "B", profit: -20 },
    ],
  });
  await page.getByRole("textbox", { name: "SQL", exact: true }).fill(sql(999));
  await expect(
    page.getByRole("region", { name: "Accepted pipeline output" }),
  ).toContainText("Run uses the accepted definition");
  await page
    .getByRole("button", { name: "Run accepted pipeline", exact: true })
    .click();
  await expect(page.getByRole("status")).toContainText("succeeded");
  const refreshed = await api(
    page,
    base + `/resources/dataset/${accepted.data.outputDatasetId}`,
  );
  expect(refreshed.data.activeVersion).not.toBe(first.data.activeVersion);
  await expect(
    page.getByRole("textbox", { name: "SQL", exact: true }),
  ).toHaveValue(sql(999));
  expect(
    (await api(page, base + `/resources/pipeline/${accepted.id}`)).data.sql,
  ).toBe(sql(2));
  await expect(
    page.getByRole("region", { name: "Accepted pipeline output" }),
  ).toContainText(refreshed.data.activeVersion);
  await page.screenshot({
    path: testInfo.outputPath("pipeline-workspace-desktop.png"),
    fullPage: true,
  });
  await page
    .getByRole("link", { name: "Inspect output dataset", exact: true })
    .click();
  await expect(
    page.getByRole("cell", { name: "80", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "-40", exact: true }),
  ).toBeVisible();
  await page.goto("/data/pipelines/" + accepted.id);
  await page.getByRole("textbox", { name: "SQL", exact: true }).fill(sql(3));
  await page
    .getByRole("button", { name: "Create review", exact: true })
    .click();
  const stillAccepted = (await api(page, base + "/resources/pipeline"))
    .items[0];
  expect(stillAccepted.id).toBe(accepted.id);
  expect(stillAccepted.data.sql).toBe(sql(2));
  await page.getByText(/^Details · \d+ checks passed$/).click();
  await page.getByText("Definitions and evidence", { exact: true }).click();
  await expect(
    page.getByRole("cell", { name: "120", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Publish", exact: true }).click();
  let next: any;
  await expect
    .poll(async () => {
      next = (await api(page, base + "/resources/pipeline")).items[0];
      return next.id;
    })
    .not.toBe(accepted.id);
  await page.goto("/data/pipelines/" + next.id);
  await expect(
    page.getByRole("textbox", { name: "SQL", exact: true }),
  ).toHaveValue(sql(3));
  await page.setViewportSize({ width: 390, height: 844 });
  await page.emulateMedia({ reducedMotion: "reduce" });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await expect(
    page.getByRole("link", { name: "Inspect output dataset", exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: testInfo.outputPath("pipeline-workspace-mobile.png"),
    fullPage: true,
  });
});
