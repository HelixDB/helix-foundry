import { expect, test } from "@playwright/test";
import { api, openApp } from "./support/workspace";

test("owners edit persistent column meanings and units with conflict protection", async ({
  page,
}, info) => {
  await openApp(page);
  const workspace = await api(page, "/workspaces", {
    name: "Dictionary browser acceptance",
  });
  const base = `/workspaces/${workspace.id}`;
  const dataset = await api(page, base + "/ingest", {
    name: "Dictionary accounts",
    rows: [
      { id: 1, amount: 150 },
      { id: 2, amount: 250 },
    ],
  });
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    workspace.id,
  );
  await page.goto(`/data/datasets/${dataset.id}`);
  const openDetails = () =>
    page.locator("details.dataset-details > summary").click();
  await openDetails();
  const dictionary = page.getByRole("region", { name: "Column dictionary" });
  await dictionary
    .getByRole("button", { name: "Edit column dictionary", exact: true })
    .click();
  const dialog = page.getByRole("dialog", {
    name: "Edit column dictionary",
    exact: true,
  });
  await dialog
    .getByRole("textbox", { name: "Meaning for amount", exact: true })
    .fill("Revenue booked before fees");
  await dialog
    .getByRole("textbox", { name: "Unit for amount", exact: true })
    .fill("cents");
  await dialog
    .getByRole("button", { name: "Save dictionary", exact: true })
    .click();
  await expect(dialog).toHaveCount(0);
  await expect(
    dictionary.getByText("Revenue booked before fees", { exact: false }),
  ).toBeVisible();
  const saved = await api(page, base + `/datasets/${dataset.id}/dictionary`);
  expect(saved.definitions).toEqual([
    { column: "amount", meaning: "Revenue booked before fees", unit: "cents" },
  ]);
  expect(
    (await api(page, base + `/resources/dataset/${dataset.id}`)).data
      .activeVersion,
  ).toBe(dataset.data.activeVersion);
  await page.reload();
  await openDetails();
  await expect(
    dictionary.getByText("Revenue booked before fees", { exact: false }),
  ).toBeVisible();
  await dictionary
    .getByRole("button", { name: "Edit column dictionary", exact: true })
    .click();
  await dialog
    .getByRole("textbox", { name: "Meaning for amount", exact: true })
    .fill("Unsaved owner refinement");
  // An actual concurrent refresh invalidates the captured dataset revision.
  const changed = await api(page, base + "/ingest", {
    name: dataset.name,
    datasetId: dataset.id,
    rows: [{ id: 3, amount: 750 }],
  });
  const rejected = page.waitForResponse(
    (response) =>
      response.request().method() === "PUT" &&
      response.url().endsWith(`/datasets/${dataset.id}/dictionary`),
  );
  await dialog
    .getByRole("button", { name: "Save dictionary", exact: true })
    .click();
  expect((await rejected).status()).toBe(409);
  await expect(
    page.getByRole("alert").filter({
      hasText: "Dataset changed; reload the dictionary before saving.",
    }),
  ).toBeVisible();
  await expect(
    dialog.getByRole("textbox", { name: "Meaning for amount", exact: true }),
  ).toHaveValue("Unsaved owner refinement");
  expect(
    (await api(page, base + `/datasets/${dataset.id}/dictionary`)).definitions,
  ).toEqual(saved.definitions);
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await dictionary
    .getByRole("button", { name: "Edit column dictionary", exact: true })
    .click();
  await dialog
    .getByRole("textbox", { name: "Meaning for amount", exact: true })
    .fill("Owner reviewed refreshed revenue");
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(
    dialog.getByRole("button", { name: "Save dictionary", exact: true }),
  ).toBeVisible();
  expect(
    await dialog.evaluate(
      (element) => element.scrollWidth <= element.clientWidth,
    ),
  ).toBe(true);
  await page.screenshot({
    path: info.outputPath("column-dictionary-mobile.png"),
    fullPage: true,
  });
  await dialog
    .getByRole("button", { name: "Save dictionary", exact: true })
    .click();
  await expect(dialog).toHaveCount(0);
  expect(
    (await api(page, base + `/datasets/${dataset.id}/dictionary`)).definitions,
  ).toEqual([
    {
      column: "amount",
      meaning: "Owner reviewed refreshed revenue",
      unit: "cents",
    },
  ]);
  expect(
    (await api(page, base + `/resources/dataset/${dataset.id}`)).data
      .activeVersion,
  ).toBe(changed.data.activeVersion);
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
});

test("an actual failed background dictionary GET preserves a dirty editing session", async ({
  page,
}, info) => {
  await openApp(page);
  const ws = await api(page, "/workspaces", {
    name: "Independent dictionary reconnect",
  });
  const base = `/workspaces/${ws.id}`;
  const ds = await api(page, base + "/ingest", {
    name: "Reconnect rows",
    rows: [{ id: 1, amount: 150 }],
  });
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    ws.id,
  );
  await page.goto(`/data/datasets/${ds.id}`);
  await page.locator("details.dataset-details > summary").click();
  await page
    .getByRole("button", { name: "Edit column dictionary", exact: true })
    .click();
  const dialog = page.getByRole("dialog", {
    name: "Edit column dictionary",
    exact: true,
  });
  const meaning = dialog.getByRole("textbox", {
    name: "Meaning for amount",
    exact: true,
  });
  await meaning.fill("Unsaved dictionary survives reconnect");
  const path = `/api/v1${base}/datasets/${ds.id}/dictionary`;
  let attempts = 0;
  await page.route("**" + path, async (route) => {
    if (route.request().method() !== "GET") return route.continue();
    attempts++;
    await route.fulfill({
      status: 503,
      contentType: "application/json",
      body: JSON.stringify({ error: "Owned dictionary GET outage" }),
    });
  });
  await page.waitForTimeout(5100);
  await page.context().setOffline(true);
  await expect.poll(() => page.evaluate(() => navigator.onLine)).toBe(false);
  await page.context().setOffline(false);
  await expect.poll(() => page.evaluate(() => navigator.onLine)).toBe(true);
  await expect(
    page
      .locator('[role="alert"]')
      .filter({ hasText: "Owned dictionary GET outage" }),
  ).toBeVisible({ timeout: 15000 });
  expect(attempts).toBe(2);
  await info.attach("real-dictionary-get-retry-exhaustion", {
    body: JSON.stringify({
      attempts,
      dictionaryPath: path,
      errorVisible: true,
    }),
    contentType: "application/json",
  });
  await page.screenshot({
    path: info.outputPath("dictionary-outage-observed-state.png"),
    fullPage: true,
  });
  await expect(dialog).toBeVisible();
  await expect(meaning).toHaveValue("Unsaved dictionary survives reconnect");
  await page.screenshot({
    path: info.outputPath("dictionary-outage-retained-draft.png"),
    fullPage: true,
  });
  await page.unroute("**" + path);
  await dialog
    .getByRole("button", { name: "Save dictionary", exact: true })
    .click();
  await expect(dialog).toHaveCount(0);
  expect(
    (await api(page, base + `/datasets/${ds.id}/dictionary`)).definitions,
  ).toEqual([
    {
      column: "amount",
      meaning: "Unsaved dictionary survives reconnect",
      unit: "",
    },
  ]);
});
