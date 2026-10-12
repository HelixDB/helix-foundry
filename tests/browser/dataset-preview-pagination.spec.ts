import { test, expect } from "@playwright/test";
import { api, openApp } from "./support/workspace";

async function fixture(page: any, label: string) {
  await openApp(page);
  const ws = await api(page, "/workspaces", { name: label + " " + Date.now() });
  const base = `/workspaces/${ws.id}`;
  const ds = await api(page, base + "/ingest", {
    name: label,
    rows: Array.from({ length: 160 }, (_, i) => ({ id: i, category: "Original " + i })),
  });
  await page.evaluate((id: string) => localStorage.setItem("foundry.workspace", id), ws.id);
  await page.goto(`/data/datasets/${ds.id}`);
  await expect(page.getByText("Sample · 1–50", { exact: true })).toBeVisible();
  return { base, ds };
}

async function reconnectDataset(page: any, base: string, ds: any, version: string) {
  const response = page.waitForResponse((r: any) =>
    r.request().method() === "GET" &&
    new URL(r.url()).pathname === `/api/v1${base}/resources/dataset/${ds.id}` &&
    r.status() === 200);
  // The actual installed query runtime listens for online events; a synthetic
  // window focus is not a supported reconnect oracle.
  await page.waitForTimeout(5100);
  await page.context().setOffline(true);
  await expect.poll(() => page.evaluate(() => navigator.onLine)).toBe(false);
  await page.context().setOffline(false);
  await expect.poll(() => page.evaluate(() => navigator.onLine)).toBe(true);
  expect((await (await response).json()).data.activeVersion).toBe(version);
}

test("current snapshot refresh resets an unavailable page", async ({ page }, info) => {
  const { base, ds } = await fixture(page, "Current page refresh");
  await page.getByRole("button", { name: "Next", exact: true }).click();
  await expect(page.getByText("Sample · 51–100", { exact: true })).toBeVisible();
  const changed = await api(page, base + "/ingest", {
    name: ds.name, datasetId: ds.id,
    rows: [{ id: 777, category: "Current only row" }],
  });
  expect(changed.data.activeVersion).not.toBe(ds.data.activeVersion);
  const previews: any[] = [];
  const actualPreview = page.waitForResponse(r =>
    r.request().method() === "POST" &&
    new URL(r.url()).pathname === `/api/v1${base}/datasets/${ds.id}/versions/${changed.data.activeVersion}/preview`);
  page.on("response", async (r) => {
    if (r.request().method() === "POST" &&
        new URL(r.url()).pathname === `/api/v1${base}/datasets/${ds.id}/versions/${changed.data.activeVersion}/preview`) {
      previews.push({ request: r.request().postDataJSON(), status: r.status(), body: await r.json() });
    }
  });
  await reconnectDataset(page, base, ds, changed.data.activeVersion);
  try {
    const response = await actualPreview;
    const body = await response.json();
    expect(response.request().postDataJSON()).toEqual({ offset: 0, limit: 50 });
    expect(response.status()).toBe(200);
    expect(body.snapshot.id).toBe(changed.data.activeVersion);
    // DuckDB BIGINT values retain their exact decimal text at the JSON boundary.
    expect(body.rows).toEqual([{ id: "777", category: "Current only row" }]);
    await expect(page.getByRole("cell", { name: "Current only row", exact: true })).toBeVisible();
    await expect(page.getByText("Sample · 1–1", { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "Previous", exact: true })).toBeDisabled();
    await expect(page.getByRole("button", { name: "Next", exact: true })).toBeDisabled();
  } finally {
    await info.attach("actual-new-version-preview", { body: JSON.stringify(previews, null, 2), contentType: "application/json" });
    await page.screenshot({ path: info.outputPath("current-snapshot-page.png"), fullPage: true });
  }
});

test("ordinary same-version metadata changes retain the current page", async ({ page }) => {
  const { base, ds } = await fixture(page, "Same version metadata");
  await page.getByRole("button", { name: "Next", exact: true }).click();
  await expect(page.getByText("Sample · 51–100", { exact: true })).toBeVisible();
  await page.getByText("Details", { exact: true }).click();
  await page.getByRole("textbox", { name: "Dataset description", exact: true }).fill("Edited metadata");
  const metadata = page.waitForResponse(r => r.request().method() === "GET" &&
    new URL(r.url()).pathname === `/api/v1${base}/resources/dataset/${ds.id}` && r.status() === 200);
  const preview = page.waitForResponse(r => r.request().method() === "POST" &&
    new URL(r.url()).pathname === `/api/v1${base}/datasets/${ds.id}/versions/${ds.data.activeVersion}/preview` &&
    r.request().postDataJSON()?.offset === 50 && r.status() === 200);
  await page.getByRole("button", { name: "Save description", exact: true }).click();
  expect((await (await metadata).json()).data.activeVersion).toBe(ds.data.activeVersion);
  expect((await (await preview).json()).rows).toHaveLength(50);
  await expect(page.getByText("Sample · 51–100", { exact: true })).toBeVisible();
  await expect(page.getByRole("cell", { name: "Original 50", exact: true })).toBeVisible();
});

test("historical page remains pinned when current changes and selection still resets pages", async ({ page }) => {
  const { base, ds } = await fixture(page, "Historical page retention");
  const second = await api(page, base + "/ingest", {
    name: ds.name, datasetId: ds.id,
    rows: Array.from({ length: 160 }, (_, i) => ({ id: i, category: "Second " + i })),
  });
  await reconnectDataset(page, base, ds, second.data.activeVersion);
  const selector = page.getByRole("combobox", { name: "Snapshot", exact: true });
  await selector.selectOption(ds.data.activeVersion);
  await expect(page.getByRole("cell", { name: "Original 0", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Next", exact: true }).click();
  await expect(page.getByText("Sample · 51–100", { exact: true })).toBeVisible();
  const third = await api(page, base + "/ingest", {
    name: ds.name, datasetId: ds.id,
    rows: [{ id: 888, category: "Newest current row" }],
  });
  await reconnectDataset(page, base, ds, third.data.activeVersion);
  await expect(selector).toHaveValue(ds.data.activeVersion);
  await expect(page.getByText("Sample · 51–100", { exact: true })).toBeVisible();
  await expect(page.getByRole("cell", { name: "Original 50", exact: true })).toBeVisible();
  await selector.selectOption(third.data.activeVersion);
  await expect(page.getByRole("cell", { name: "Newest current row", exact: true })).toBeVisible();
  await expect(page.getByText("Sample · 1–1", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "Previous", exact: true })).toBeDisabled();
});
