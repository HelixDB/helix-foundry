import { test, expect, type Page } from "@playwright/test";
import { api, openApp } from "./support/workspace";

// A dataset of 160 rows, open on its first page of 50.
async function fixture(page: Page, label: string) {
  await openApp(page);
  const ws = await api(page, "/workspaces", { name: label + " " + Date.now() });
  const base = `/workspaces/${ws.id}`;
  const ds = await api(page, base + "/ingest", {
    name: label,
    rows: Array.from({ length: 160 }, (_, i) => ({
      id: i,
      category: "Original " + i,
    })),
  });
  await page.evaluate(
    (id: string) => localStorage.setItem("foundry.workspace", id),
    ws.id,
  );
  await page.goto(`/data/datasets/${ds.id}`);
  await expect(page.getByText("Sample · 1–50", { exact: true })).toBeVisible();
  return { base, ds };
}

// The page refetches the dataset when the browser comes back online, once
// the query is older than its 5-second staleTime.
async function reconnect(page: Page, base: string, dsId: string) {
  const response = page.waitForResponse(
    (r) =>
      r.request().method() === "GET" &&
      new URL(r.url()).pathname ===
        `/api/v1${base}/resources/dataset/${dsId}` &&
      r.status() === 200,
  );
  await page.waitForTimeout(5100);
  await page.context().setOffline(true);
  await expect.poll(() => page.evaluate(() => navigator.onLine)).toBe(false);
  await page.context().setOffline(false);
  await expect.poll(() => page.evaluate(() => navigator.onLine)).toBe(true);
  return (await (await response).json()).data.activeVersion;
}

const previewOf = (base: string, dsId: string, version: string) =>
  `/api/v1${base}/datasets/${dsId}/versions/${version}/preview`;

test("a new current snapshot starts on its first page", async ({ page }) => {
  const { base, ds } = await fixture(page, "Current page refresh");
  await page.getByRole("button", { name: "Next", exact: true }).click();
  await expect(
    page.getByText("Sample · 51–100", { exact: true }),
  ).toBeVisible();
  const changed = await api(page, base + "/ingest", {
    name: ds.name,
    datasetId: ds.id,
    rows: [{ id: 777, category: "Current only row" }],
  });
  expect(changed.data.activeVersion).not.toBe(ds.data.activeVersion);
  const preview = page.waitForResponse(
    (r) =>
      r.request().method() === "POST" &&
      new URL(r.url()).pathname ===
        previewOf(base, ds.id, changed.data.activeVersion),
  );
  expect(await reconnect(page, base, ds.id)).toBe(changed.data.activeVersion);
  expect((await preview).request().postDataJSON()).toEqual({
    offset: 0,
    limit: 50,
  });
  await expect(
    page.getByRole("cell", { name: "Current only row", exact: true }),
  ).toBeVisible();
  await expect(page.getByText("Sample · 1–1", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Previous", exact: true }),
  ).toBeDisabled();
});

test("a refetch of the same snapshot keeps the page", async ({ page }) => {
  const { base, ds } = await fixture(page, "Same version metadata");
  await page.getByRole("button", { name: "Next", exact: true }).click();
  await expect(
    page.getByText("Sample · 51–100", { exact: true }),
  ).toBeVisible();
  await page.getByText("Details", { exact: true }).click();
  await page
    .getByRole("textbox", { name: "Dataset description", exact: true })
    .fill("Edited metadata");
  const metadata = page.waitForResponse(
    (r) =>
      r.request().method() === "GET" &&
      new URL(r.url()).pathname ===
        `/api/v1${base}/resources/dataset/${ds.id}` &&
      r.status() === 200,
  );
  const preview = page.waitForResponse(
    (r) =>
      r.request().method() === "POST" &&
      new URL(r.url()).pathname ===
        previewOf(base, ds.id, ds.data.activeVersion) &&
      r.request().postDataJSON()?.offset === 50 &&
      r.status() === 200,
  );
  await page
    .getByRole("button", { name: "Save description", exact: true })
    .click();
  expect((await (await metadata).json()).data.activeVersion).toBe(
    ds.data.activeVersion,
  );
  expect((await (await preview).json()).rows).toHaveLength(50);
  await expect(
    page.getByText("Sample · 51–100", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "Original 50", exact: true }),
  ).toBeVisible();
});

test("a chosen historical snapshot keeps its page; choosing resets it", async ({
  page,
}) => {
  const { base, ds } = await fixture(page, "Historical page retention");
  const second = await api(page, base + "/ingest", {
    name: ds.name,
    datasetId: ds.id,
    rows: Array.from({ length: 160 }, (_, i) => ({
      id: i,
      category: "Second " + i,
    })),
  });
  expect(await reconnect(page, base, ds.id)).toBe(second.data.activeVersion);
  const selector = page.getByRole("combobox", {
    name: "Snapshot",
    exact: true,
  });
  await selector.selectOption(ds.data.activeVersion);
  await expect(
    page.getByRole("cell", { name: "Original 0", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Next", exact: true }).click();
  await expect(
    page.getByText("Sample · 51–100", { exact: true }),
  ).toBeVisible();
  const third = await api(page, base + "/ingest", {
    name: ds.name,
    datasetId: ds.id,
    rows: [{ id: 888, category: "Newest current row" }],
  });
  expect(await reconnect(page, base, ds.id)).toBe(third.data.activeVersion);
  await expect(selector).toHaveValue(ds.data.activeVersion);
  await expect(
    page.getByText("Sample · 51–100", { exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "Original 50", exact: true }),
  ).toBeVisible();
  await selector.selectOption(third.data.activeVersion);
  await expect(
    page.getByRole("cell", { name: "Newest current row", exact: true }),
  ).toBeVisible();
  await expect(page.getByText("Sample · 1–1", { exact: true })).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Previous", exact: true }),
  ).toBeDisabled();
});
