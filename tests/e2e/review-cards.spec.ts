import type { Page } from "@playwright/test";
import {
  CONTRACT,
  expect,
  file,
  makeContractsCurrent,
  mock,
  signIn,
  startContractsLoad,
  test,
  uploadAndLoadContract,
  uploadInData,
} from "./support/mock";

// The review cards index (reviews/cards-api.ts, mock/routes/review-cards.ts):
// the walk reads its cards a page at a time; every count (the Review tab,
// the Ontologies panel, the job dock, the sidebar's nudge) is the index's
// summary, less the walk's own choices; spot checks are drawn once, the
// same for everyone; and while the index is still counting, the walk is
// never done.

const O = "/ontology/ont_acme_contracts";
// Held claims, a value in conflict and a name in no row, beside the
// contract's facts (review-queue.spec.ts).
const HEDGED =
  "# Hedged deal\n\nUmbrella Holdings was copied. Globex Corporation and Initech may have signed. Globex Corporation supplies Initech. Initech competes with Globex Corporation.\n\n## Billing\n\nGlobex Corporation is billed in GBP.\n";

const card = (page: Page) => page.getByRole("article");
const rail = (page: Page) =>
  page.getByRole("navigation", { name: "Next cards" }).getByRole("listitem");
const nudge = (page: Page) =>
  page.getByRole("status", { name: "Cards to review" });
const reviewLink = (page: Page) =>
  page.getByRole("list", { name: "Pages" }).getByRole("link", {
    name: /^Review/,
  });

async function loaded(page: Page, config = {}) {
  await mock.configure({ loadStepMs: 200, documentUnmatched: true, ...config });
  await signIn(page);
  await uploadAndLoadContract(page, [file("variant.md", HEDGED)]);
}

test("reads the walk's cards a page at a time, the next before the walk reaches the end", async ({
  page,
}) => {
  await loaded(page, { reviewCardsPage: 2 });
  const next = page.waitForRequest(
    (r) => r.method() === "GET" && /\/review-cards\?.*pageToken=/.test(r.url()),
  );
  await page.goto(`${O}/review`);
  await expect(card(page)).toBeVisible();
  await next;
  // Every page read: more than one page's cards, each once.
  await expect.poll(async () => rail(page).count()).toBeGreaterThan(2);
  const lines = await rail(page).allTextContents();
  expect(new Set(lines).size).toBe(lines.length);
  // The tab counts what the index counts.
  const count = Number(
    await page
      .getByRole("tab", { name: /^Facts/ })
      .locator(".hf-tab-count")
      .textContent(),
  );
  await expect(
    page
      .getByRole("radiogroup", { name: "Lane" })
      .getByRole("radio", { name: /^Decide/ }),
  ).toHaveText(new RegExp(`^Decide\\s*${count}$`));
});

test("draws the spot checks once, the same for two people", async ({
  page,
  browser,
}) => {
  await loaded(page);
  const drawn = (p: Page) =>
    p.waitForResponse(
      (r) =>
        r.request().method() === "POST" &&
        r.url().endsWith("/review-spot-checks:draw"),
    );
  const first = drawn(page);
  await page.goto(`${O}/review?kind=value`);
  const mine = (await (await first).json()).draws;
  expect(mine.length).toBeGreaterThan(0);
  await expect(
    page.getByRole("list", { name: "Spot checks" }).getByRole("listitem"),
  ).toHaveCount(mine.length);

  const other = await (await browser.newContext()).newPage();
  await signIn(other, "alan@example.com");
  const second = drawn(other);
  await other.goto(`${O}/review?kind=value`);
  expect((await (await second).json()).draws).toEqual(mine);
  await other.context().close();
});

test("counts the cards to decide where no one has opened Review: the panel, the dock and the nudge, one fewer after a choice", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 1500, documentUnmatched: true });
  await signIn(page);
  await uploadInData(page, [
    file("contract.md", CONTRACT),
    file("variant.md", HEDGED),
  ]);
  await makeContractsCurrent(page);
  await startContractsLoad(page);
  await expect(page).toHaveURL(/\/loads\/load_/);
  await page.goto("/data");
  const dock = page.getByRole("region", { name: "Jobs in progress" });
  await expect(dock).toContainText("Data loaded", { timeout: 30_000 });
  const review = dock.getByRole("link", { name: /^Review \d+$/ });
  const n = Number((await review.textContent())!.replace("Review ", ""));
  expect(n).toBeGreaterThan(1);
  // The nudge names it, with as many.
  await expect(nudge(page)).toContainText(`${n} to review`);
  await expect(nudge(page)).toContainText("Customer contracts");
  // The panel counts it, Review unopened (on a page that doesn't show the
  // load, so the dock keeps following it).
  await page.goto(`${O}/connections`);
  await expect(reviewLink(page)).toHaveAccessibleName(
    new RegExp(`^Review: ${n} cards to decide`),
  );
  // A choice in the walk takes one off every count.
  await reviewLink(page).click();
  await expect(card(page)).toBeVisible();
  // The walk counts it already.
  await expect(
    nudge(page).filter({ hasText: "Customer contracts" }),
  ).toHaveCount(0);
  await card(page).getByRole("button", { name: "Reject" }).first().click();
  await expect(page.locator(".rv-toast")).toContainText("Rejected.");
  await expect(reviewLink(page)).toHaveAccessibleName(
    new RegExp(`^Review: ${n - 1} cards? to decide`),
  );
  await expect(dock.getByRole("link", { name: /^Review \d+$/ })).toHaveText(
    `Review ${n - 1}`,
  );
});

test("the nudge opens the walk, and Later hides it for that load", async ({
  page,
}) => {
  await loaded(page);
  await page.goto("/data");
  await expect(nudge(page)).toContainText(/\d+ to review/);
  const name = (await nudge(page).locator(".on-job-card-name").textContent())!;
  await nudge(page).getByRole("link", { name: "Start" }).click();
  await expect(page).toHaveURL(/\/ontology\/[^/]+\/review$/);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Review");
  await page.goto("/data");
  await nudge(page).getByRole("button", { name: "Later" }).click();
  await expect(nudge(page).filter({ hasText: name })).toHaveCount(0);
  await page.reload();
  await expect(page.getByRole("main")).toBeVisible();
  await expect(nudge(page).filter({ hasText: name })).toHaveCount(0);
});

test("members get no nudge", async ({ page }) => {
  await mock.configure({ loadStepMs: 200, documentUnmatched: true });
  await signIn(page);
  await uploadAndLoadContract(page, [file("variant.md", HEDGED)]);
  await page.context().clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto("/data");
  await expect(page.getByRole("main")).toBeVisible();
  await expect(nudge(page)).toHaveCount(0);
});

test("while the index is still counting, the walk says so and is never done", async ({
  page,
}) => {
  await loaded(page, { reviewIndexing: true });
  // No spot check is drawn from an index still being built.
  const draws: string[] = [];
  page.on("request", (r) => {
    if (r.method() === "POST" && r.url().endsWith("/review-spot-checks:draw"))
      draws.push(r.url());
  });
  await page.goto(`${O}/review`);
  await expect(
    page.getByRole("note").filter({ hasText: "Counting…" }),
  ).toBeVisible();
  await page.goto(`${O}/review?kind=value`);
  await expect(
    page.getByRole("heading", { level: 2, name: "More cards may come" }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", { level: 2, name: "Nothing to decide" }),
  ).toHaveCount(0);
  await expect(
    page.getByText("Spot checks are drawn once the load’s cards are counted."),
  ).toBeVisible();
  expect(draws).toEqual([]);
});
