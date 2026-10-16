import { test, expect } from "@playwright/test";
import { artifact } from "./support/ports";
import { api, openApp } from "./support/workspace";

test("adds a dataset imported after setup to the published ontology", async ({
  page,
}) => {
  await openApp(page);
  const workspace = await api(page, "/workspaces", { name: "Ontology update" });
  const base = `/workspaces/${workspace.id}`;
  await api(page, base + "/ingest", {
    name: "customers",
    rows: [1, 2, 3].map((i) => ({
      customer_id: `c${i}`,
      name: `Customer ${i}`,
    })),
  });
  const suggested = await api(page, base + "/onboarding/ontology", {});
  await api(page, base + "/onboarding/ontology/confirm", {
    hash: suggested.ontology.hash,
    excluded: [],
    names: {},
  });
  await expect
    .poll(async () => (await api(page, base + "/onboarding")).complete, {
      timeout: 60000,
      intervals: [1000],
    })
    .toBe(true);
  // Imported from the Data tab once setup is complete.
  await api(page, base + "/ingest", {
    name: "orders",
    rows: [1, 2, 3, 4].map((i) => ({
      order_id: `o${i}`,
      customer_id: `c${(i % 3) + 1}`,
      total: i * 10,
    })),
  });
  await page.evaluate(
    (id) => localStorage.setItem("foundry.workspace", id),
    workspace.id,
  );
  await page.goto("/ontology");

  const notice = page.getByRole("status").filter({
    hasText: "1 new dataset isn’t in your ontology yet.",
  });
  await expect(notice).toBeVisible();
  await notice.getByRole("button", { name: "Update ontology" }).click();
  await expect(
    page.getByRole("heading", { name: "Here’s how your data fits together" }),
  ).toBeVisible({ timeout: 20000 });
  // Back leaves the published ontology as it was.
  await page.getByRole("button", { name: "Back", exact: true }).click();
  await expect(notice).toBeVisible();
  await notice.getByRole("button", { name: "Update ontology" }).click();
  const confirm = page.getByRole("button", {
    name: "Looks right — update ontology",
  });
  await expect(confirm).toBeVisible({ timeout: 20000 });
  await page.screenshot({
    path: artifact("ontology-update-review.png"),
    fullPage: true,
  });
  await confirm.click();

  // The page refreshes once the update is published.
  await expect(notice).toHaveCount(0, { timeout: 60000 });
  await expect(
    page.getByRole("region", { name: "Ontology update" }),
  ).toHaveCount(0, { timeout: 60000 });
  const published = async () => {
    const state = await api(page, base + "/onboarding");
    const types = await api(page, base + "/resources/objectType");
    return {
      status: state.buildRun?.data.status,
      types: types.items.map((t: any) => t.name).sort(),
    };
  };
  await expect.poll(published, { timeout: 60000 }).toMatchObject({
    status: "succeeded",
  });
  const { types } = await published();
  expect(types.length).toBe(2);
  for (const name of types)
    await expect(
      page.getByRole("heading", { name, level: 3, exact: true }),
    ).toBeVisible();
  await page.screenshot({
    path: artifact("ontology-updated.png"),
    fullPage: true,
  });
});
