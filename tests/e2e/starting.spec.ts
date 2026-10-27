import type { Page } from "@playwright/test";
import {
  createdOntology,
  expect,
  finishOntology,
  mock,
  signIn,
  startOntology,
  test,
} from "./support/mock";

// A new database the gateway hasn't taken on yet answers reads with 503
// rate_limit_unavailable for its first minutes. Within 15 minutes of its
// ontology's creation the pages say it is starting and read it again by
// themselves until it answers; an older ontology's database answering so
// keeps the error card.

/**
 * Creates "Billing" on the empty sandbox database, as new as the ontology,
 * with a draft of the Stripe account as its current version 1; the
 * overview's path.
 */
async function newBillingOntology(page: Page): Promise<string> {
  await signIn(page);
  await startOntology(page, "Billing");
  await page.getByRole("radio", { name: /sandbox/ }).check();
  await page.getByRole("button", { name: "Continue" }).click();
  await page
    .getByRole("list", { name: "Connectors" })
    .getByRole("checkbox", { name: /Shop/ })
    .uncheck();
  await page.getByRole("button", { name: "Continue" }).click();
  await page.getByRole("button", { name: "Skip" }).click();
  await page.getByRole("button", { name: "Create and generate" }).click();
  const overview = await createdOntology(page);
  await page
    .getByRole("link", { name: "Review version 1" })
    .click({ timeout: 15_000 });
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 1 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 1 is now current.")).toBeVisible();
  return overview;
}

/** Loads version 1 from the Loads page and waits for the end; the load's id. */
async function loadVersion1(page: Page, overview: string): Promise<string> {
  await page.goto(`${overview}#loads`);
  await page
    .getByRole("main")
    .getByRole("button", { name: "Load data" })
    .first()
    .click();
  await page
    .getByRole("dialog", { name: "Load version 1 into sandbox?" })
    .getByRole("button", { name: "Load" })
    .click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 20_000 });
  return new URL(page.url()).pathname.split("/").pop()!;
}

test("says a new database is starting, and shows what it holds once it answers", async ({
  page,
}) => {
  // The database refuses its first two reads.
  await mock.configure({ startingQueries: { orders: 2 } });
  await signIn(page);
  await startOntology(page, "Orders");
  await page.getByRole("radio", { name: "orders in Production" }).check();
  await finishOntology(page);
  await createdOntology(page);
  await page
    .getByRole("navigation", { name: "Main navigation" })
    .getByRole("link", { name: "Explorer" })
    .click();
  // Nothing is loaded, so nothing is read; the database's own data is
  // Other data.
  await page
    .getByRole("main")
    .getByRole("link", { name: "Finish setting up" })
    .waitFor();
  await page.goto(`${new URL(page.url()).pathname}?load=other`);

  const starting = page
    .getByRole("status")
    .filter({ hasText: "Your new database is starting" });
  await expect(starting).toBeVisible();
  await expect(starting).toContainText("reads it again by itself");
  // Retry reads now: refused again, the card stays, not an error.
  await starting.getByRole("button", { name: "Retry" }).click();
  await expect
    .poll(async () => (await mock.settings()).startingQueries?.orders)
    .toBe(0);
  await expect(starting).toBeVisible();
  await expect(page.getByText(/refusing queries/)).toHaveCount(0);
  await expect(page.locator("#main").getByRole("alert")).toHaveCount(0);

  // It reads again by itself, 5 s later, and shows what the database holds.
  await expect(
    page.getByRole("application", {
      name: "Graph of orders: 7 nodes, 4 edges",
    }),
  ).toBeVisible({ timeout: 15_000 });
  await expect(starting).toHaveCount(0);
});

test("keeps the error card for an older ontology's database", async ({
  page,
}) => {
  // Customer contracts was created long ago: its database refusing a read
  // is the platform failure it is, with a Retry and no reading by itself.
  await mock.configure({ startingQueries: { contracts: 1 } });
  await signIn(page);
  await page.goto("/explore/ont_acme_contracts/records?load=other");
  await expect(
    page.getByText("The database's gateway is refusing queries"),
  ).toBeVisible();
  await expect(page.getByText("Your new database is starting")).toHaveCount(0);
  await page.waitForTimeout(6_000);
  await expect(
    page.getByText("The database's gateway is refusing queries"),
  ).toBeVisible();
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(page.getByText("Nothing loaded yet")).toBeVisible();
});

test("says a load waits for a new database to start, then reads its records once it answers", async ({
  page,
}) => {
  await mock.configure({ ontologyStepMs: 300, loadStepMs: 1500 });
  const overview = await newBillingOntology(page);

  // Its load waits for the database, saying so, then loads.
  await page.goto(overview);
  await page
    .getByRole("status")
    .filter({ hasText: "Version 1 isn’t loaded yet" })
    .getByRole("button", { name: "Load data" })
    .click();
  await page
    .getByRole("dialog", { name: "Load version 1 into sandbox?" })
    .getByRole("button", { name: "Load" })
    .click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  const loadPage = page.url();
  // The database refuses the load's first write: the load waits three steps
  // (4.5 s) before it tries again. Its next request from here, as the
  // overview's list of loads has read the new load's counts already.
  await mock.configure({ startingQueries: { sandbox: 1 } });
  await expect(
    page
      .getByRole("status")
      .filter({ hasText: "The new database is still starting" }),
  ).toBeVisible({ timeout: 10_000 });
  await expect(page.getByText(/refused a write/)).toHaveCount(0);
  // So do the overview and the Loads page's Jobs column, while it waits.
  const pages = page.getByRole("complementary", { name: "Ontologies" });
  await pages.getByRole("link", { name: "Overview" }).click();
  await expect(
    page
      .getByRole("status")
      .filter({ hasText: "A load is waiting for the new database" }),
  ).toContainText("The load tries again by itself once it takes writes.");
  await expect(
    page
      .getByRole("grid", { name: "Loads" })
      .getByRole("row")
      .filter({
        has: page.locator(`a[href$="${new URL(loadPage).pathname}"]`),
      }),
  ).toContainText("Tables and documents: database starting");
  await expect(page.getByText(/refused a write/)).toHaveCount(0);
  await page.goto(loadPage);
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 30_000 });
  await expect(
    page.getByText("The new database is still starting"),
  ).toHaveCount(0);

  // Records of a database refusing its first reads say it is starting.
  await mock.configure({ startingQueries: { sandbox: 3 } });
  await page.goto(`${overview.replace("/ontology/", "/explore/")}/records`);
  const starting = page
    .getByRole("status")
    .filter({ hasText: "Your new database is starting" });
  await expect(starting).toBeVisible();
  // Retry reads now, not after the 5 s wait: refused again, still starting.
  await starting.getByRole("button", { name: "Retry" }).click();
  await expect
    .poll(async () => (await mock.settings()).startingQueries?.sandbox, {
      timeout: 3_000,
    })
    .toBe(1);
  await expect(starting).toBeVisible();

  // From here the card stays up through a refused retry, never giving way
  // to a skeleton, until the records' grid replaces it.
  await page.evaluate(() => {
    const w = window as unknown as { gaps: number };
    w.gaps = 0;
    const shown = () =>
      !!document.querySelector('[role="grid"]') ||
      [...document.querySelectorAll('[role="status"]')].some((e) =>
        e.textContent?.includes("Your new database is starting"),
      );
    new MutationObserver(() => {
      if (!shown()) w.gaps++;
    }).observe(document.body, {
      childList: true,
      subtree: true,
      characterData: true,
    });
  });
  await expect
    .poll(async () => (await mock.settings()).startingQueries?.sandbox, {
      timeout: 10_000,
    })
    .toBe(0);
  await expect(starting).toBeVisible();
  await expect(
    page.getByRole("grid", { name: "Customer records" }),
  ).toBeVisible({ timeout: 15_000 });
  await expect(starting).toHaveCount(0);
  expect(
    await page.evaluate(() => (window as unknown as { gaps: number }).gaps),
  ).toBe(0);
});

test("says the Loads page's counts and a compare wait for a new database, then shows them", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await mock.configure({ ontologyStepMs: 300, loadStepMs: 200 });
  const overview = await newBillingOntology(page);
  // Two loads of version 1: the second is current.
  const first = await loadVersion1(page, overview);
  const second = await loadVersion1(page, overview);
  const STARTING =
    "The new database is still starting; this reads it again by itself.";
  const waiting = page.getByRole("status").filter({ hasText: STARTING });

  // The Loads page's counts: each load's is refused once, says the database
  // is starting, and is read again by itself.
  await mock.configure({ startingQueries: { sandbox: 2 } });
  await page.goto(`${overview}#loads`);
  const grid = page.getByRole("grid", { name: "Loads" });
  const column = (name: string) =>
    grid.getByRole("columnheader", { name }).getAttribute("aria-colindex");
  const [nodesAt, factsAt] = [await column("Nodes"), await column("Facts")];
  const cell = (id: string, at: string | null) =>
    grid
      .getByRole("row")
      .filter({ has: page.locator(`a[href$="/loads/${id}"]`) })
      .locator(`[role="gridcell"][aria-colindex="${at}"]`);
  for (const id of [first, second])
    for (const at of [nodesAt, factsAt])
      await expect(cell(id, at).getByTitle(STARTING)).toHaveText("…");
  await expect
    .poll(async () => (await mock.settings()).startingQueries?.sandbox)
    .toBe(0);
  for (const id of [first, second]) {
    await expect(cell(id, nodesAt)).toHaveText(/^[1-9][\d,]*$/, {
      timeout: 15_000,
    });
    await expect(cell(id, factsAt)).toHaveText(/^\d[\d,]*$/);
  }
  await expect(grid.getByTitle(STARTING)).toHaveCount(0);

  // A compare's diff says so in place of its counts, and reads again by
  // itself until the database answers.
  await mock.configure({ startingQueries: { sandbox: 8 } });
  await page.goto(
    `${overview.replace("/ontology/", "/explore/")}?load=${first}&compare=${second}`,
  );
  await expect(waiting).toBeVisible();
  await expect
    .poll(async () => (await mock.settings()).startingQueries?.sandbox, {
      timeout: 45_000,
    })
    .toBe(0);
  const customers = page.getByTestId("diff-row-Customer");
  await expect(customers).toBeVisible({ timeout: 30_000 });
  await expect(waiting).toHaveCount(0);
  await expect(page.locator("#main").getByRole("alert")).toHaveCount(0);
  // Both loads hold the same customers: no change.
  const [a, b, change] = await customers
    .getByRole("cell")
    .evaluateAll((cells) => cells.slice(1).map((c) => c.textContent));
  expect(a).toMatch(/^[1-9][\d,]*$/);
  expect(b).toBe(a);
  expect(change).toBe("0");

  // Opening a type reads its keys in both loads: starting, then the diff.
  // The canvases read their first views first: once the database has had no
  // read for 2 s, the next two are the keys'.
  await expect(
    page.getByRole("application", { name: /^Graph of sandbox: \d+ nodes/ }),
  ).toHaveCount(2, { timeout: 30_000 });
  const reads = async () =>
    (await mock.stats()).requests.filter((r) => r.includes(":query-read"))
      .length;
  let seen = -1;
  await expect
    .poll(
      async () => {
        const now = await reads();
        const still = now === seen;
        seen = now;
        return still;
      },
      { intervals: [2_000], timeout: 30_000 },
    )
    .toBe(true);
  await mock.configure({ startingQueries: { sandbox: 2 } });
  await customers.getByRole("button", { name: /Customer/ }).click();
  await expect(waiting).toBeVisible();
  const keys = page.getByTestId("diff-keys-Customer");
  await expect(keys).toContainText("Added in v1 · 0", { timeout: 20_000 });
  await expect(keys).toContainText("Removed since v1 · 0");
  await expect(waiting).toHaveCount(0);
});
