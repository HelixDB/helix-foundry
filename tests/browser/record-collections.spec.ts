import { expect, test } from "@playwright/test";
import { api, openApp } from "./support/workspace";

test("curates and returns to durable collections through the actual published workspace", async ({
  page,
}, info) => {
  await openApp(page);
  const ws = await api(page, "/workspaces", {
      name: "Record collection acceptance",
    }),
    base = `/workspaces/${ws.id}`;
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    ws.id,
  );
  await api(page, base + "/onboarding/provider", {
    provider: "local",
    model: "qwen3:4b",
  });
  const ds = await api(page, base + "/ingest", {
    name: "Collection people",
    rows: [
      { id: 1, name: "Ada acceptance", region: "West" },
      { id: 2, name: "Lin acceptance", region: "East" },
      { id: 3, name: "Sam acceptance", region: "South" },
    ],
  });
  const bundle = {
    summary: "People",
    assumptions: [],
    catalog: [],
    pipelines: [],
    ontology: [
      {
        name: "Person",
        datasetId: ds.id,
        primaryKey: "id",
        properties: ["id", "name", "region"],
      },
    ],
    relationships: [],
    dashboards: [{ name: "Collection acceptance", widgets: [{ title: "People", type: "metric", inputs: [ds.id], sql: `SELECT count(*) total FROM ${ds.data.table}` }] }],
    actions: [],
  };
  const publish = async () => {
    const p = (
      await api(page, base + "/definitions/import", {
        version: 1,
        proposals: [bundle],
      })
    ).items[0];
    expect(p.data.validation.valid).toBe(true);
    return api(page, base + `/proposals/${p.id}/publish`, {
      hash: p.data.hash,
    });
  };
  await publish();
  expect((await api(page, base + "/onboarding")).complete).toBe(true);
  await page.goto("/ontology/explore");
  await page.getByRole("link", { name: "Collections", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Record collections", exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "New collection", exact: true })
    .click();
  let dialog = page.getByRole("dialog", {
    name: "New collection",
    exact: true,
  });
  await dialog.getByLabel("Collection name").fill("Follow up accounts");
  await dialog.getByRole("button", { name: "Save collection" }).click();
  await expect(dialog).not.toBeVisible();
  await page.getByRole("button", { name: "Add record", exact: true }).click();
  dialog = page.getByRole("dialog", {
    name: "Add record to Follow up accounts",
  });
  await dialog.getByLabel("Find collection record").fill("Ada acceptance");
  await expect(
    dialog.getByText("Ada acceptance", { exact: true }),
  ).toBeVisible();
  await dialog.getByRole("button", { name: "Add", exact: true }).click();
  await expect(dialog).not.toBeVisible();
  let list = await api(page, base + "/record-collections");
  expect(list.items).toHaveLength(1);
  const collectionId = list.items[0].id;
  expect(list.items[0].logicalIds).toHaveLength(1);
  const member = page.getByRole("region", { name: "Collection records" });
  await expect(
    member.getByRole("link", { name: "Ada acceptance", exact: true }),
  ).toBeVisible();
  await member
    .getByRole("link", { name: "Ada acceptance", exact: true })
    .click();
  dialog = page.getByRole("dialog", { name: "Person 1", exact: true });
  await expect(dialog).toBeVisible();
  await expect(dialog.getByRole("definition").filter({ hasText: "West" })).toBeVisible();
  await dialog.getByRole("button", { name: "Close", exact: true }).click();
  await page.getByRole("link", { name: "Collections", exact: true }).click();
  await page
    .getByRole("region", { name: "Collections", exact: true })
    .getByRole("button", { name: /Follow up accounts/ })
    .click();
  await page.getByRole("button", { name: "Rename", exact: true }).click();
  dialog = page.getByRole("dialog", { name: "Rename collection" });
  await dialog.getByLabel("Collection name").fill("Priority accounts");
  await dialog.getByRole("button", { name: "Save collection" }).click();
  await expect(dialog).not.toBeVisible();
  await publish();
  await page.reload();
  await page
    .getByRole("region", { name: "Collections", exact: true })
    .getByRole("button", { name: /Priority accounts/ })
    .click();
  await expect(
    member.getByRole("link", { name: "Ada acceptance", exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: info.outputPath("collections-desktop.png"),
    fullPage: true,
  });
  // A native concurrent edit must be refused while a stale add dialog preserves its draft.
  await page.getByRole("button", { name: "Add record", exact: true }).click();
  dialog = page.getByRole("dialog", {
    name: "Add record to Priority accounts",
  });
  await dialog.getByLabel("Find collection record").fill("Lin acceptance");
  await expect(
    dialog.getByText("Lin acceptance", { exact: true }),
  ).toBeVisible();
  const current = await api(page, base + `/record-collections/${collectionId}`);
  const changed = await page.request.put(
    "/api/v1" + base + `/record-collections/${collectionId}`,
    {
      data: { revision: current.revision, name: current.name, logicalIds: [] },
    },
  );
  expect(changed.ok()).toBe(true);
  const refused = page.waitForResponse(
    (r) =>
      r.request().method() === "PUT" &&
      r.url().endsWith(`/record-collections/${collectionId}`),
  );
  await dialog.getByRole("button", { name: "Add", exact: true }).click();
  expect((await refused).status()).toBe(409);
  await expect(dialog).toBeVisible();
  await expect(dialog.getByLabel("Find collection record")).toHaveValue(
    "Lin acceptance",
  );
  await expect(
    page.getByRole("alert").filter({ hasText: "Collection changed" }),
  ).toBeVisible();
  await dialog.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.getByRole("button", { name: "Add record", exact: true }).click();
  dialog = page.getByRole("dialog", {
    name: "Add record to Priority accounts",
  });
  await dialog.getByLabel("Find collection record").fill("Lin acceptance");
  await expect(
    dialog.getByText("Lin acceptance", { exact: true }),
  ).toBeVisible();
  await dialog.getByRole("button", { name: "Add", exact: true }).click();
  await expect(dialog).not.toBeVisible();
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(
    member.getByRole("link", { name: "Lin acceptance", exact: true }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.screenshot({
    path: info.outputPath("collections-mobile.png"),
    fullPage: true,
  });
  await member
    .getByRole("button", { name: "Remove Lin acceptance", exact: true })
    .click();
  await expect(
    member.getByText("No records in this collection.", { exact: false }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Delete collection", exact: true })
    .click();
  dialog = page.getByRole("dialog", { name: "Delete collection", exact: true });
  await dialog
    .getByRole("button", { name: "Delete collection", exact: true })
    .click();
  await expect(dialog).not.toBeVisible();
  await expect(
    page.getByText("No collections yet.", { exact: true }),
  ).toBeVisible();
  expect((await api(page, base + "/record-collections")).items).toEqual([]);
  expect(
    (await api(page, base + `/resources/dataset/${ds.id}`)).data.activeVersion,
  ).toBe(ds.data.activeVersion);
});
