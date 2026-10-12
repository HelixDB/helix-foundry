import { test, expect } from "@playwright/test";
import { api, openApp } from "./support/workspace";

test("inspect historical and pending rows without changing the current dataset snapshot", async ({
  page,
}) => {
  await openApp(page);
  const workspace = await api(page, "/workspaces", {
    name: "History acceptance " + Date.now(),
  });
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    workspace.id,
  );
  const base = `/workspaces/${workspace.id}`;
  const first = await api(page, base + "/ingest", {
    name: "History acceptance",
    rows: [
      { id: 1, note: "original historical row" },
      { id: 2, note: "second historical row" },
    ],
  });
  const other = await api(page, base + "/ingest", {
    name: "Second navigation dataset",
    rows: [{ id: 9, note: "independent current dataset row" }],
  });
  const refreshed = await api(page, base + "/ingest", {
    name: first.name,
    datasetId: first.id,
    rows: [{ id: 3, note: "accepted current row" }],
  });
  const staged = await api(page, base + "/ingest", {
    name: first.name,
    datasetId: first.id,
    rows: [{ id: 4, note: "pending schema row", proposed: "unaccepted field" }],
  });
  expect(staged.data.activeVersion).toBe(refreshed.data.activeVersion);
  expect(staged.data.pendingVersion).toBeTruthy();
  await page.goto(`/data/datasets/${first.id}`);
  const selector = page.getByRole("combobox", {
    name: "Snapshot",
    exact: true,
  });
  await expect(selector).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "accepted current row", exact: true }),
  ).toBeVisible();
  await selector.selectOption(first.data.activeVersion);
  await expect(
    page.getByRole("cell", { name: "original historical row", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("Historical snapshot", { exact: false }),
  ).toBeVisible();
  const version = await api(
    page,
    base + `/resources/version/${first.data.activeVersion}`,
  );
  await expect(page.locator("time")).toHaveAttribute(
    "datetime",
    version.createdAt,
  );
  const shown = await page.evaluate(
    (at) => new Date(at).toLocaleString(),
    version.createdAt,
  );
  await expect(
    selector.locator(`option[value="${first.data.activeVersion}"]`),
  ).toHaveText(`Historical · ${shown}`);
  await expect(page.getByText("Sample · 1–2", { exact: true })).toBeVisible();
  await selector.selectOption(staged.data.pendingVersion);
  await expect(
    page.getByRole("cell", { name: "pending schema row", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("columnheader", { name: "proposed", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByText("Snapshot awaiting review", { exact: false }),
  ).toBeVisible();
  await page.getByText("Details", { exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Schema and profile", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("cell", { name: "proposed", exact: true }),
  ).toHaveCount(2);
  await expect(
    page.getByRole("link", { name: "Export current Parquet", exact: true }),
  ).toHaveAttribute(
    "href",
    `/api/v1/workspaces/${workspace.id}/datasets/${first.id}/export`,
  );
  await expect(
    page.getByText("Exports and queries use the current snapshot.", {
      exact: false,
    }),
  ).toBeVisible();
  const unchanged = await api(page, base + `/resources/dataset/${first.id}`);
  expect(unchanged.data.activeVersion).toBe(refreshed.data.activeVersion);
  expect(unchanged.data.pendingVersion).toBe(staged.data.pendingVersion);
  await selector.selectOption(refreshed.data.activeVersion);
  await expect(
    page.getByRole("cell", { name: "accepted current row", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("link", { name: "Export Parquet", exact: true }),
  ).toHaveAttribute(
    "href",
    `/api/v1/workspaces/${workspace.id}/datasets/${first.id}/export`,
  );
  await selector.selectOption(first.data.activeVersion);
  await expect(
    page.getByRole("cell", { name: "original historical row", exact: true }),
  ).toBeVisible();
  await page
    .getByRole("navigation", { name: "Main navigation" })
    .getByRole("link", { name: "Data", exact: true })
    .click();
  await page.locator(`a[href="/data/datasets/${other.id}"]`).click();
  await expect(
    page.getByRole("cell", {
      name: "independent current dataset row",
      exact: true,
    }),
  ).toBeVisible();
  await expect(
    page.getByRole("combobox", { name: "Snapshot", exact: true }),
  ).toHaveValue(other.data.activeVersion);
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page
    .getByRole("navigation", { name: "Main navigation" })
    .getByRole("link", { name: "Data", exact: true })
    .click();
  await page.locator(`a[href="/data/datasets/${first.id}"]`).click();
  await expect(
    page.getByRole("cell", { name: "accepted current row", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("combobox", { name: "Snapshot", exact: true }),
  ).toHaveValue(refreshed.data.activeVersion);
});
