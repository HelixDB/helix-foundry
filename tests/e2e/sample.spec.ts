import type { Page } from "@playwright/test";
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
// carries on with what it made, and nothing is made or asked twice. Once the
// sample exists a banner says so, and links back to that page.

/** A new workspace, on the plan the mock sells databases with, on the sample's page. */
async function openSample(page: Page) {
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
  await page
    .getByRole("button", { name: "See it on a sample company (1 min)" })
    .click();
  await expect(page).toHaveURL(/\/ontology\/sample$/);
  await expect(
    page.getByRole("heading", {
      level: 1,
      name: "Acme Corp, a sample company",
    }),
  ).toBeVisible();
}

test("walks a new workspace through the sample company, from its database to Ask", async ({
  page,
}) => {
  await openSample(page);
  const steps = page.getByRole("list", { name: "Steps" });
  await expect(steps).toContainText("6 tables, 1,121 rows", {
    timeout: 30_000,
  });
  await expect(steps).toContainText("Foundry models Acme Corp as 9 types", {
    timeout: 30_000,
  });
  await page.reload();
  // Once loaded, the tour asks the draft's first question itself and shows
  // how it ended, as Ask does (the mock can't answer this one); the others
  // are a click away, then the user's own data.
  const asked = steps.getByRole("article", { name: /^Question: / });
  await expect(asked).toBeVisible({ timeout: 60_000 });
  await expect(
    steps.getByRole("group", { name: "Suggested questions" }),
  ).toBeVisible({ timeout: 30_000 });
  await expect(
    page.getByRole("link", { name: "Connect your own data" }),
  ).toHaveAttribute("href", "/ontology/new");
  const others = steps.getByRole("group", { name: "Suggested questions" });
  await expect(others.getByRole("button").first()).toBeEnabled();
  // A reload shows the same question: nothing is asked twice.
  const first = (await asked.getAttribute("aria-label"))!;
  await page.reload();
  await expect(asked).toHaveCount(1, { timeout: 30_000 });
  await expect(asked).toHaveAttribute("aria-label", first);
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

test("says it is sample data, and its link back to the tour shows every step done and makes nothing", async ({
  page,
}) => {
  test.slow();
  await openSample(page);
  const banner = page.getByRole("region", { name: "Sample data" }),
    back = banner.getByRole("link", { name: "Back to getting started" }),
    steps = page.getByRole("list", { name: "Steps" }),
    others = steps.getByRole("group", { name: "Suggested questions" });
  // Nothing to say until the sample exists.
  await expect(banner).toHaveCount(0);
  await expect(others).toBeVisible({ timeout: 60_000 });
  // On the tour itself it has nowhere to go back to.
  await expect(banner).toHaveText(
    "Sample data: you’re exploring Acme Corp, a made-up company.",
  );
  await expect(back).toHaveCount(0);
  // Nor is Getting into production's strip here.
  const production = page.getByRole("region", {
    name: "Getting into production",
  });
  await expect(production).toHaveCount(0);

  // From here on, nothing is written: reads over POST aside, no call at all.
  const writes: string[] = [];
  page.on("request", (request) => {
    const path = new URL(request.url()).pathname;
    if (
      request.method() !== "GET" &&
      path.startsWith("/papi/") &&
      !/:(query-)?read$/.test(path)
    )
      writes.push(`${request.method()} ${path}`);
  });
  const asked = steps.getByRole("article", { name: /^Question: / });
  const everyStepDone = async () => {
    await expect(others).toBeVisible({ timeout: 30_000 });
    await expect(steps.getByText("(done)")).toHaveCount(5);
    await expect(steps.locator('[aria-current="step"]')).toContainText(
      "Now connect your own data",
    );
    await expect(steps).toContainText("6 tables, 1,121 rows");
    await expect(steps).toContainText("Foundry models Acme Corp as 9 types");
    await expect(asked).toHaveCount(1);
  };

  // The load's page, where the tour's link leaves the reader, has the way back.
  await steps.getByRole("link", { name: "See the load" }).click();
  await expect(page).toHaveURL(/\/ontology\/[^/]+\/loads\/[^/]+$/);
  await back.click();
  await expect(page).toHaveURL(/\/ontology\/sample$/);
  await everyStepDone();
  await expect(back).toHaveCount(0);

  // Opened afresh, with nothing in the cache.
  await page.reload();
  await everyStepDone();

  // The only ontology: the banner is on every page, and there is one of
  // everything.
  await page.goto("/data");
  await expect(explorer(page).getByText("Acme Corp sample")).toBeVisible();
  await expect(
    explorer(page).getByText("Acme Corp", { exact: true }),
  ).toHaveCount(1);
  // The sample isn't the workspace's own ontology: creating one comes first,
  // above the page and, on the Analyst, in the card.
  await expect(
    production.getByRole("link", { name: "Create an ontology" }),
  ).toHaveAttribute("href", "/ontology/new");
  await expect(
    production.getByRole("button", { name: "Copy MCP prompt" }),
  ).toHaveCount(0);
  await page.goto("/ask");
  await expect(page).toHaveURL(/\/ask\/[^/]+$/);
  await expect(page.locator(".hf-strip")).toHaveCount(0);
  await expect(production).toContainText("0 of 2");
  // The prompt waits for an ontology of the workspace's own.
  await expect(
    production.getByRole("button", { name: "Copy prompt" }),
  ).toBeDisabled();
  await page.goto("/data");
  await back.click();
  await expect(page).toHaveURL(/\/ontology\/sample$/);
  await everyStepDone();
  expect(writes).toEqual([]);

  await page.goto("/ontology");
  await expect(
    page
      .getByRole("complementary", { name: "Ontologies" })
      .getByRole("link", { name: /Acme Corp customers/ }),
  ).toHaveCount(1);
});
