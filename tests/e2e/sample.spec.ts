import {
  expect,
  explorer,
  mock,
  signInToNewWorkspace,
  test,
} from "./support/mock";

// A new workspace's onboarding offers the sample company. This build is the
// dev stamp's, which has Kestrel's database (next.config.ts), so the sample
// opens its own page: it connects the database over TLS, uploads the
// documents, shows the goal and the ontology made from its ready-made draft
// on a new Helix database, loads it, and offers its questions. A reload
// carries on with what it made, and nothing is made twice.

test("walks a new workspace through the sample company, from its database to Ask", async ({
  page,
}) => {
  await mock.configure({
    ontologyStepMs: 300,
    loadStepMs: 200,
    checkoutCardOnFile: true,
  });
  await signInToNewWorkspace(page);
  // The mock still sells databases with Pro; both stamps offer them on every
  // plan, Free included.
  await page.goto("/settings/billing");
  await page.getByRole("button", { name: "Upgrade", exact: true }).click();
  await page
    .getByRole("dialog", { name: "Upgrade Foundry" })
    .getByRole("button", { name: "Continue to payment" })
    .click();
  await expect(
    page.getByRole("heading", { name: /Foundry Pro/ }),
  ).toContainText("Active", { timeout: 15_000 });
  await page.goto("/");
  await page.getByRole("button", { name: "Use the sample" }).click();
  await expect(page).toHaveURL(/\/ontology\/sample$/);
  await expect(
    page.getByRole("heading", { level: 1, name: "Kestrel, a sample company" }),
  ).toBeVisible();
  const steps = page.getByRole("list", { name: "Steps" });
  await expect(steps).toContainText("6 tables, 1,121 rows", {
    timeout: 30_000,
  });
  await expect(steps).toContainText("Foundry models Kestrel as 9 types", {
    timeout: 30_000,
  });
  await page.reload();
  const ask = page.getByRole("link", { name: "Ask a question" });
  await expect(ask).toBeVisible({ timeout: 60_000 });
  await ask.click();
  await expect(
    page.getByRole("group", { name: "Suggested questions" }),
  ).toBeVisible();

  // One Kestrel database in Data, and one set of its documents.
  await page.goto("/data");
  await expect(explorer(page).getByText("Kestrel sample")).toBeVisible();
  await expect(
    explorer(page).getByText("Kestrel", { exact: true }),
  ).toHaveCount(1);
});
