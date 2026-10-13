import { test, expect } from "@playwright/test";
import { api, openApp } from "./support/workspace";

test("owners add persistent sample masking while detected sensitivity remains protected", async ({
  page,
}, testInfo) => {
  await openApp(page);
  const workspace = await api(page, "/workspaces", {
    name: "Privacy browser " + Date.now(),
  });
  const base = `/workspaces/${workspace.id}`;
  const rows = Array.from({ length: 6 }, (_, i) => ({
    id: i,
    region: i % 2 ? "PRIVATE-SOUTH" : "PRIVATE-NORTH",
    amount: 10 + i,
    email: `private-${i}@example.com`,
  }));
  const dataset = await api(page, base + "/ingest", {
    name: "Privacy acceptance",
    rows,
  });
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    workspace.id,
  );
  await page.goto(`/data/datasets/${dataset.id}`);
  await page.getByText("Details", { exact: true }).click();
  const policy = page.getByRole("region", { name: "Hosted AI privacy" });
  for (const width of [1440, 1024, 390]) {
    await page.setViewportSize({ width, height: 1040 });
    await expect(
      policy.getByLabel("Mask region", { exact: true }),
    ).toBeVisible();
    await expect(
      policy.getByRole("button", { name: "Save privacy", exact: true }),
    ).toBeVisible();
    expect(
      await policy
        .locator("label")
        .first()
        .evaluate((label) => getComputedStyle(label).flexDirection),
    ).toBe("row");
    const box = await policy
      .getByLabel("Mask region", { exact: true })
      .boundingBox();
    const text = await policy
      .getByLabel("Mask region", { exact: true })
      .locator("..")
      .locator("span")
      .boundingBox();
    expect(Math.abs(box!.y - text!.y)).toBeLessThan(8);
    const bounds = await policy.boundingBox();
    expect(bounds!.width).toBeGreaterThan(250);
    expect(bounds!.x + bounds!.width).toBeLessThanOrEqual(width);
    expect(
      await page.evaluate(() => document.documentElement.scrollWidth),
    ).toBeLessThanOrEqual(width);
    await page.screenshot({
      path: testInfo.outputPath(`privacy-${width}.png`),
      fullPage: true,
    });
  }
  await page.setViewportSize({ width: 1440, height: 1040 });
  await expect(policy.getByLabel("Mask email", { exact: true })).toBeChecked();
  await expect(policy.getByLabel("Mask email", { exact: true })).toBeDisabled();
  await policy.getByLabel("Mask region", { exact: true }).check();
  await policy
    .getByRole("button", { name: "Save privacy", exact: true })
    .click();
  await expect
    .poll(
      async () =>
        (await api(page, base + `/resources/dataset/${dataset.id}`)).data
          .privacy.additionalColumns,
    )
    .toEqual(["region"]);
  await page.reload();
  await page.getByText("Details", { exact: true }).click();
  await expect(policy.getByLabel("Mask region", { exact: true })).toBeChecked();
  await api(page, base + "/ingest", {
    name: "Privacy acceptance",
    datasetId: dataset.id,
    rows: rows.map((r) => ({ ...r, amount: r.amount + 1 })),
  });
  await page.reload();
  await page.getByText("Details", { exact: true }).click();
  await expect(policy.getByLabel("Mask region", { exact: true })).toBeChecked();
  await policy.getByLabel("Mask region", { exact: true }).uncheck();
  await policy
    .getByRole("button", { name: "Save privacy", exact: true })
    .click();
  await expect
    .poll(
      async () =>
        (await api(page, base + `/resources/dataset/${dataset.id}`)).data
          .privacy.additionalColumns,
    )
    .toEqual([]);
  await expect(policy.getByLabel("Mask email", { exact: true })).toBeChecked();
  await expect(policy.getByLabel("Mask email", { exact: true })).toBeDisabled();
});
