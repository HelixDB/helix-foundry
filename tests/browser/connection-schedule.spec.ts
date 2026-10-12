import { test, expect } from "@playwright/test";
import { createServer } from "node:http";
import { once } from "node:events";
import { api, openApp } from "./support/workspace";
import { artifact } from "./support/ports";
const server = createServer((req, res) => {
  res.setHeader("content-type", "application/json");
  res.end(
    JSON.stringify([
      { id: 1, amount: 10 },
      { id: 2, amount: 20 },
    ]),
  );
});
let origin: string;
test.beforeAll(async () => {
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  origin = `http://127.0.0.1:${(server.address() as any).port}`;
});
test.afterAll(async () => {
  server.closeAllConnections();
  await new Promise<void>((r) => server.close(() => r()));
});
test("owners edit preset and custom cadence without losing the connection or manual refresh", async ({
  page,
}) => {
  await openApp(page);
  const workspace = await api(page, "/workspaces", {
    name: "Cadence browser " + Date.now(),
  });
  const base = `/workspaces/${workspace.id}`;
  const source = await api(page, base + "/sources", {
    name: "Cadence acceptance",
    kind: "rest",
    config: { url: origin + "/rows" },
  });
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    workspace.id,
  );
  await page.goto(`/data/connections/${source.id}`);
  const schedule = page.getByRole("region", {
    name: "Snapshot refresh schedule",
  });
  await schedule
    .getByRole("combobox", { name: "Refresh cadence", exact: true })
    .selectOption("*/15 * * * *");
  await schedule
    .getByRole("button", { name: "Save schedule", exact: true })
    .click();
  await expect
    .poll(
      async () =>
        (await api(page, base + `/resources/source/${source.id}`)).data
          .schedule,
    )
    .toBe("*/15 * * * *");
  await page.reload();
  await expect(
    schedule.getByRole("combobox", { name: "Refresh cadence", exact: true }),
  ).toHaveValue("*/15 * * * *");
  for (const width of [1440, 390]) {
    await page.setViewportSize({ width, height: 900 });
    await expect(
      schedule.getByRole("combobox", { name: "Refresh cadence", exact: true }),
    ).toBeVisible();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    await page.screenshot({
      path: artifact(`cadence-${width}.png`),
      fullPage: true,
    });
  }
  await schedule
    .getByRole("combobox", { name: "Refresh cadence", exact: true })
    .selectOption("custom");
  await schedule
    .getByLabel("UTC cron schedule", { exact: true })
    .fill("not a cron");
  await schedule
    .getByRole("button", { name: "Save schedule", exact: true })
    .click();
  await expect(page.getByRole("alert")).toContainText("valid refresh schedule");
  expect(
    (await api(page, base + `/resources/source/${source.id}`)).data.schedule,
  ).toBe("*/15 * * * *");
  await schedule
    .getByLabel("UTC cron schedule", { exact: true })
    .fill("0 9 * * 1-5");
  await schedule
    .getByRole("button", { name: "Save schedule", exact: true })
    .click();
  await expect
    .poll(
      async () =>
        (await api(page, base + `/resources/source/${source.id}`)).data
          .schedule,
    )
    .toBe("0 9 * * 1-5");
  await page.reload();
  await expect(
    schedule.getByRole("combobox", { name: "Refresh cadence", exact: true }),
  ).toHaveValue("custom");
  await expect(
    schedule.getByLabel("UTC cron schedule", { exact: true }),
  ).toHaveValue("0 9 * * 1-5");
  const sync = await api(page, base + `/sources/${source.id}/sync`, {});
  await expect
    .poll(
      async () =>
        (await api(page, base + `/resources/job/${sync.id}`)).data.status,
    )
    .toBe("succeeded");
  const datasets = await api(page, base + "/resources/dataset");
  expect(
    datasets.items.find((d: any) => d.data.sourceId === source.id).data.profile
      .rows,
  ).toBe(2);
  await schedule
    .getByRole("combobox", { name: "Refresh cadence", exact: true })
    .selectOption("");
  await schedule
    .getByRole("button", { name: "Save schedule", exact: true })
    .click();
  await expect
    .poll(
      async () =>
        (await api(page, base + `/resources/source/${source.id}`)).data
          .schedule,
    )
    .toBeNull();
});
