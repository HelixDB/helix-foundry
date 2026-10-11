import { test, expect } from "@playwright/test";
import { api, openApp } from "./support/workspace";
import { ports } from "./support/ports";

test("owners pause and resume automatic refresh from the connection card", async ({
  page,
}) => {
  await openApp(page);
  const workspace = await api(page, "/workspaces", {
    name: "Refresh controls " + Date.now(),
  });
  const base = `/workspaces/${workspace.id}`;
  const source = await api(page, base + "/sources", {
    name: "Refresh browser fixture",
    kind: "rest",
    config: {
      url: `http://127.0.0.1:${ports.api}/fixture/orders`,
      pagination: "offset",
      pageSize: 2,
    },
  });
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    workspace.id,
  );
  await page.goto("/data");
  const card = page
    .locator(".cn-card")
    .filter({
      has: page.getByRole("heading", { name: "Refresh browser fixture" }),
    });
  for (const width of [1440, 1024, 390]) {
    await page.setViewportSize({ width, height: 1040 });
    await expect(
      card.getByRole("button", {
        name: "Pause automatic refresh",
        exact: true,
      }),
    ).toBeVisible();
    const footer = card.locator("footer > span");
    await expect(footer).toBeVisible();
    const size = await footer.boundingBox();
    expect(size!.width).toBeGreaterThan(200);
    expect(size!.height).toBeLessThan(55);
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth),
    ).toBeLessThanOrEqual(width);
  }
  await page.setViewportSize({ width: 1440, height: 1040 });
  await page
    .getByRole("button", { name: "Pause automatic refresh", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Resume automatic refresh", exact: true }),
  ).toBeVisible();
  expect(
    (await api(page, base + "/resources/source/" + source.id)).data.paused,
  ).toBe(true);
  await page.reload();
  await expect(
    page.getByRole("button", { name: "Resume automatic refresh", exact: true }),
  ).toBeVisible();
  await page.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect
    .poll(
      async () =>
        (await api(page, base + "/resources/job")).items.some(
          (job: any) =>
            job.data.sourceId === source.id &&
            !job.data.background &&
            job.data.status === "succeeded",
        ),
      { timeout: 20000 },
    )
    .toBe(true);
  expect(
    (await api(page, base + "/resources/source/" + source.id)).data.paused,
  ).toBe(true);
  await page
    .getByRole("button", { name: "Resume automatic refresh", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Pause automatic refresh", exact: true }),
  ).toBeVisible();
  expect(
    (await api(page, base + "/resources/source/" + source.id)).data.paused,
  ).toBe(false);
});
