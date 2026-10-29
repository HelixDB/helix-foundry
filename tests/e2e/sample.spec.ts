import {
  expect,
  explorer,
  mock,
  signInToNewWorkspace,
  test,
} from "./support/mock";

// A new workspace's onboarding offers the sample company. This build is the
// dev stamp's, which has Acme Corp's database (next.config.ts), so the sample
// opens its own page: it connects the database over TLS, uploads the
// documents, shows the goal and the ontology made from its ready-made draft
// on a new Helix database, loads it, asks its first question and answers
// it, then offers its other questions and the user's own data. A reload
// carries on with what it made, and nothing is made or asked twice.

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
    page.getByRole("heading", {
      level: 1,
      name: "Acme Corp, a sample company",
    }),
  ).toBeVisible();
  const steps = page.getByRole("list", { name: "Steps" });
  await expect(steps).toContainText("6 tables, 1,121 rows", {
    timeout: 30_000,
  });
  await expect(steps).toContainText("Foundry models Acme Corp as 9 types", {
    timeout: 30_000,
  });
  await page.reload();
  // Once loaded, the tour asks the draft's first question itself and shows
  // its answer; the others are a click away, then the user's own data.
  await expect(steps.getByRole("region", { name: "Answer" })).toBeVisible({
    timeout: 60_000,
  });
  await expect(
    page.getByRole("link", { name: "Connect your own data" }),
  ).toHaveAttribute("href", "/ontology/new");
  const others = steps.getByRole("group", { name: "Suggested questions" });
  await expect(others.getByRole("button").first()).toBeEnabled();
  // A reload shows the same answer: nothing is asked twice.
  await page.reload();
  await expect(steps.getByRole("region", { name: "Answer" })).toHaveCount(1, {
    timeout: 30_000,
  });
  const next = (await others.getByRole("button").first().textContent())!;
  await others.getByRole("button", { name: next }).click();
  await expect(page).toHaveURL(/\/ask\/[^?]+\?thread=q_/);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(next);

  // One Acme Corp database in Data, and one set of its documents.
  await page.goto("/data");
  await expect(explorer(page).getByText("Acme Corp sample")).toBeVisible();
  await expect(
    explorer(page).getByText("Acme Corp", { exact: true }),
  ).toHaveCount(1);
});
