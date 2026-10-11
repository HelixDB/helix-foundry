import { expect, test } from "@playwright/test";
import { api, openApp } from "./support/workspace";

test("finds and investigates records outside the graph sample and current neighbourhood", async ({
  page,
}, info) => {
  await openApp(page);
  const ws = await api(page, "/workspaces", {
    name: "Global graph acceptance",
  });
  const base = `/workspaces/${ws.id}`;
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    ws.id,
  );
  await api(page, base + "/onboarding/provider", {
    provider: "local",
    model: "qwen3:4b",
  });
  const ds = await api(page, base + "/ingest", {
    name: "Graph people",
    rows: Array.from({ length: 1605 }, (_, i) => ({
      id: `p${i}`,
      name: `Needle person ${String(i).padStart(4, "0")}`,
      region: i % 2 ? "West" : "East",
    })),
  });
  const proposal = (
    await api(page, base + "/definitions/import", {
      version: 1,
      proposals: [
        {
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
          dashboards: [
            {
              name: "Graph acceptance",
              widgets: [
                {
                  title: "People",
                  type: "metric",
                  inputs: [ds.id],
                  sql: `SELECT count(*) total FROM ${ds.data.table}`,
                },
              ],
            },
          ],
          actions: [],
        },
      ],
    })
  ).items[0];
  expect(proposal.data.validation.valid).toBe(true);
  await api(page, base + `/proposals/${proposal.id}/publish`, {
    hash: proposal.data.hash,
  });
  expect((await api(page, base + "/onboarding")).complete).toBe(true);
  const sample = await api(page, base + "/graph");
  expect(sample.total).toBe(1605);
  expect(sample.nodes).toHaveLength(1500);
  const ids = new Set(sample.nodes.map((n: any) => n.id));
  const records: any[] = [];
  for (let offset = 0; offset < 1605; offset += 500) {
    const result = await api(
      page,
      base + `/resources/object?limit=500&offset=${offset}`,
    );
    expect(result.total).toBe(1605);
    records.push(...result.items);
  }
  const outside = records.filter((r) => !ids.has(r.data.logicalId));
  expect(outside.length).toBe(105);
  const [first, second] = outside;
  // The existing public full-record API already finds the omitted record.
  // The regression is its missing integration into graph navigation.
  const positive = await api(
    page,
    base +
      `/resources/object?${new URLSearchParams({ search: first.data.label, limit: "8" })}`,
  );
  expect(positive.items.map((r: any) => r.id)).toEqual([first.id]);
  await page.goto("/ontology/explore");
  await expect(
    page.getByRole("img", {
      name: "Graph of 1500 records and 0 links. Use the Table view for the same records as a list.",
    }),
  ).toBeVisible();
  const search = page.getByLabel("Find a record");
  await search.fill(first.data.label);
  const option = page.getByRole("option", {
    name: `${first.data.label} Person`,
  });
  await expect(option).toBeVisible();
  await search.press("Enter");
  const selected = page.getByRole("complementary", { name: first.data.label });
  await expect(selected).toBeVisible();
  await expect(
    page.getByRole("img", { name: /^Graph of 1 records and 0 links/ }),
  ).toBeVisible();
  await expect(search).toHaveValue("");
  await selected.getByRole("button", { name: "Open record" }).click();
  const detail = page.getByRole("dialog", { name: first.name, exact: true });
  await expect(detail).toBeVisible();
  await expect(
    detail.getByRole("definition").filter({ hasText: first.data.effective.region }),
  ).toBeVisible();
  await detail.getByRole("button", { name: "Close", exact: true }).click();
  // Searching from this one-record neighbourhood still reaches the whole workspace.
  await page.setViewportSize({ width: 390, height: 844 });
  await search.fill(second.data.label);
  await expect(
    page.getByRole("option", { name: `${second.data.label} Person` }),
  ).toBeVisible();
  await search.press("ArrowDown");
  await search.press("Enter");
  await expect(
    page.getByRole("complementary", { name: second.data.label }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await search.fill("No such synthetic graph record");
  await expect(
    page.getByRole("status").filter({ hasText: "No matching records" }),
  ).toBeVisible();
  await expect(page.getByRole("option")).toHaveCount(0);
  await search.press("Escape");
  await expect(search).toHaveValue("");
  await page.screenshot({
    path: info.outputPath("global-graph-mobile.png"),
    fullPage: true,
  });
  await page.getByRole("button", { name: "Show all records" }).click();
  await expect(
    page.getByRole("img", { name: /^Graph of 1500 records/ }),
  ).toBeVisible();
  // Immediate Enter remains usable while the remote search is pending.
  const local = sample.nodes[0];
  await search.fill(local.label);
  await search.press("Enter");
  await expect(
    page.getByRole("complementary", { name: local.label }),
  ).toBeVisible();
  await expect(
    page.getByRole("img", { name: /^Graph of 1500 records/ }),
  ).toBeVisible();
  // The new UI does not mutate imported rows or their immutable version.
  const unchanged = await api(page, base + `/resources/dataset/${ds.id}`);
  expect(unchanged.data.activeVersion).toBe(ds.data.activeVersion);
  expect(unchanged.data.profile.rows).toBe(1605);
});
