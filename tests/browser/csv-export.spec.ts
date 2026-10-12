import { test, expect } from "@playwright/test";
import { readFile } from "node:fs/promises";
import { parse } from "csv-parse/sync";
import { api, openApp } from "./support/workspace";
test("dataset details download every row as CSV through the real executor", async ({
  page,
}) => {
  await openApp(page);
  const workspace = await api(page, "/workspaces", {
    name: "CSV download " + Date.now(),
  });
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    workspace.id,
  );
  const dataset = await api(page, `/workspaces/${workspace.id}/ingest`, {
    name: "CSV acceptance",
    rows: Array.from({ length: 75 }, (_, i) => ({
      id: i,
      note: i === 74 ? 'last, "quoted"\nrow' : "row " + i,
    })),
  });
  await page.goto(`/data/datasets/${dataset.id}`);
  await page.getByText("Details", { exact: true }).click();
  const exported = page.waitForEvent("download");
  await page.getByRole("link", { name: "Export CSV", exact: true }).click();
  const download = await exported;
  expect(download.suggestedFilename()).toBe("dataset.csv");
  const path = await download.path();
  expect(path).toBeTruthy();
  const rows = parse(await readFile(path!, "utf8"), { columns: true });
  expect(rows).toHaveLength(75);
  expect(rows.at(-1)).toEqual({ id: "74", note: 'last, "quoted"\nrow' });
  await expect(
    page.getByRole("link", { name: "Export Parquet", exact: true }),
  ).toBeVisible();
});
