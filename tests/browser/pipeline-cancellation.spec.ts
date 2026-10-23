import { test, expect } from "@playwright/test";
import { api, openApp } from "./support/workspace";
import { artifact, dataDir } from "./support/ports";
import { resolve } from "node:path";
import { readdir } from "node:fs/promises";

async function ownJobDirectories() {
  return (await readdir(resolve(dataDir, "scratch")).catch(() => []))
    .filter((name) => name.startsWith("job-"))
    .sort();
}

test("cancel a running pipeline from Data before Home is available and retain the published snapshot", async ({
  page,
}) => {
  await openApp(page);
  const workspace = await api(page, "/workspaces", {
      name: "Pipeline cancellation " + Date.now(),
    }),
    base = `/workspaces/${workspace.id}`;
  const raw = await api(page, base + "/ingest", {
    name: "Cancel work",
    rows: [{ id: 1, work: 2 }],
  });
  const proposal = await api(page, base + "/pipelines/draft", {
    name: "Cancelable browser totals",
    inputs: [raw.id],
    sql: `SELECT id, sum(x % 100) AS total FROM "${raw.data.table}", range(work) AS series(x) GROUP BY id`,
  });
  expect(proposal.data.validation.valid).toBe(true);
  await api(page, base + `/proposals/${proposal.id}/publish`, {
    hash: proposal.data.hash,
  });
  const pipeline = (await api(page, base + "/resources/pipeline")).items[0],
    before = await api(
      page,
      base + `/resources/dataset/${pipeline.data.outputDatasetId}`,
    );
  expect((await api(page, base + "/onboarding")).complete).toBe(false);
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    workspace.id,
  );
  await page.goto("/");
  await expect(page).toHaveURL(/\/onboarding$/);
  await page.goto("/data");
  await expect(page).toHaveURL(/\/data$/);
  await api(page, base + "/ingest", {
    name: "Cancel work",
    datasetId: raw.id,
    rows: [{ id: 1, work: 1000000000000 }],
  });
  const job = await api(page, base + `/pipelines/${pipeline.id}/run`, {});
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    workspace.id,
  );
  await page.goto("/data");
  await expect
    .poll(
      async () =>
        (await api(page, base + `/resources/job/${job.id}`)).data.status,
      { timeout: 20000 },
    )
    .toBe("running");
  await expect.poll(async () => (await ownJobDirectories()).length).toBe(1);
  const [runningJob] = await ownJobDirectories();
  const panel = page.getByRole("region", { name: "Pipeline jobs" }),
    cancel = panel.getByRole("button", {
      name: "Cancel Run Cancelable browser totals",
      exact: true,
    });
  await expect(panel.getByText(/running ·/)).toBeVisible();
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 1040 });
    await expect(cancel).toBeVisible();
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth),
    ).toBeLessThanOrEqual(width);
    await page.screenshot({
      path: artifact(`pipeline-cancel-${width}.png`),
      fullPage: true,
    });
  }
  expect((await api(page, base + `/resources/job/${job.id}`)).data.status).toBe(
    "running",
  );
  expect(await ownJobDirectories()).toContain(runningJob);
  await cancel.click();
  await expect
    .poll(
      async () =>
        (await api(page, base + `/resources/job/${job.id}`)).data.status,
    )
    .toBe("canceled");
  await expect
    .poll(async () => await ownJobDirectories())
    .not.toContain(runningJob);
  await expect
    .poll(async () => await readdir(resolve(dataDir, "scratch")))
    .toEqual([]);
  await expect(panel.getByText(/canceled ·/)).toBeVisible();
  await expect(cancel).toHaveCount(0);
  expect(
    (await api(page, base + `/resources/dataset/${before.id}`)).data
      .activeVersion,
  ).toBe(before.data.activeVersion);
  const preview = await api(page, base + `/datasets/${before.id}/preview`, {});
  expect(preview.rows).toEqual([{ id: "1", total: 1 }]);
  await page.reload();
  await expect(panel.getByText(/canceled ·/)).toBeVisible();
  await api(page, base + "/ingest", {
    name: "Cancel work",
    datasetId: raw.id,
    rows: [{ id: 1, work: 2 }],
  });
  const next = await api(page, base + `/pipelines/${pipeline.id}/run`, {});
  await expect
    .poll(
      async () =>
        (await api(page, base + `/resources/job/${next.id}`)).data.status,
      { timeout: 20000 },
    )
    .toBe("succeeded");
  expect(
    (await api(page, base + `/resources/dataset/${before.id}`)).data
      .activeVersion,
  ).not.toBe(before.data.activeVersion);
});
