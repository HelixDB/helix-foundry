import type { Page } from "@playwright/test";
import { expect, mock, signIn, test } from "./support/mock";

// Suggestions: what audits of the current load found the ontology lacks,
// opened from the ontology's pages, each with its kind, support, examples
// and what it proposes; dismissing one takes it off the list. Where
// foundry.heal.enabled is off there are none, and no page to open.

const O = "/ontology/ont_acme_contracts";
const suggestions = (page: Page) =>
    page.getByRole("list", { name: "Suggestions" }),
  // Its cards, not the examples listed in them.
  cards = (page: Page) => suggestions(page).locator(":scope > li"),
  dismissOf = (card: ReturnType<typeof cards>) =>
    card.getByRole("button", { name: "Dismiss", exact: true });
// A dismissal's Undo window (index.tsx UNDO_MS), and then some.
const UNDO = 6_000;

// Makes version 3 current and loads it; again, loads the current version
// once more. It returns once the load is made: the page may then go
// anywhere, as the load goes on in the mock.
async function load(page: Page, again = false) {
  if (!again) {
    await page.goto(`${O}/versions/ver_seed_3`);
    await page.getByRole("button", { name: "Make current" }).click();
    await page
      .getByRole("dialog", { name: "Make version 3 current?" })
      .getByRole("button", { name: "Make current" })
      .click();
    await expect(page.getByText("Version 3 is now current.")).toBeVisible();
  }
  await page.goto(`${O}#loads`);
  await page
    .getByRole("main")
    .getByRole("button", { name: "Load data" })
    .first()
    .click();
  const made = page.waitForResponse(
    (r) =>
      r.request().method() === "POST" &&
      new URL(r.url()).pathname.endsWith("/loads"),
  );
  await page
    .getByRole("dialog", { name: "Load version 3 into contracts?" })
    .getByRole("button", { name: "Load" })
    .click();
  expect((await made).ok()).toBe(true);
}

// Opens the Suggestions page again until check passes: a load just made
// becomes current, and is audited, a moment after it is made.
async function openUntil(page: Page, check: () => Promise<void>) {
  await expect(async () => {
    await page.goto(`${O}/suggestions`);
    await check();
  }).toPass({ timeout: 45_000 });
}

// Signs in and loads, the page's clock installed first: a test moves it on
// past a dismissal's Undo window, or to the page's next poll, at once.
async function loaded(page: Page) {
  await page.clock.install();
  await signIn(page);
  await load(page);
  await openUntil(page, () =>
    expect(cards(page)).toHaveCount(3, { timeout: 2000 }),
  );
}

test("lists the current load's suggestions and dismisses one", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await page.clock.install();
  await signIn(page);
  await page.goto(`${O}/suggestions`);
  await expect(page.getByText("No suggestions")).toBeVisible();

  await load(page);
  await openUntil(page, () =>
    expect(cards(page)).toHaveCount(3, { timeout: 2000 }),
  );
  await page.goto(O);
  await page
    .getByRole("list", { name: "Pages" })
    .getByRole("link", { name: "Suggestions" })
    .click();
  await expect(page).toHaveURL(new RegExp(`${O}/suggestions$`));
  await expect(cards(page)).toHaveCount(3);
  const first = cards(page).first();
  await expect(first).toContainText(
    "Documents state “supplier of”, which no link covers",
  );
  // Its set by name, as the workspace's sets list it.
  await expect(first).toContainText("In document set “General”");
  await expect(first).toContainText("12 reviews in 5 documents");
  await expect(first).toContainText(
    "Suggested: Let documents of “General” state “supplies”",
  );
  await expect(first.getByRole("list", { name: "Examples" })).toContainText(
    "Globex supplies Acme with steel under this agreement.",
  );
  const showAll = first.getByRole("button", { name: "Show all 4 examples" });
  await expect(showAll).toHaveAttribute("aria-expanded", "false");
  await showAll.click();
  await expect(
    first.getByRole("list", { name: "Examples" }).getByRole("listitem"),
  ).toHaveCount(4);
  await expect(
    first.getByRole("button", { name: "Show fewer" }),
  ).toHaveAttribute("aria-expanded", "true");
  await expect(page.getByText(/^Last audited/)).toBeVisible();
  await expect(page.getByText(/Rows weren’t checked/)).toHaveCount(0);

  // Each Dismiss is named Dismiss, described by its card's title; one
  // takes its card away at once and hands focus to the next.
  const dismiss = first.getByRole("button", { name: "Dismiss", exact: true });
  await expect(dismiss).not.toHaveAttribute("aria-label");
  await expect(dismiss).toHaveAccessibleDescription(
    "Documents state “supplier of”, which no link covers",
  );
  await dismiss.click();
  await expect(cards(page)).toHaveCount(2);
  await expect(suggestions(page)).not.toContainText("supplier of");
  await expect(suggestions(page)).toContainText(
    "Rows of “customers” didn't load",
  );
  await expect(
    cards(page).first().getByRole("button", { name: "Dismiss", exact: true }),
  ).toBeFocused();
  // Sent once its toast ends: a reload no longer lists it.
  await expect(page.getByText("Suggestion dismissed.")).toBeVisible();
  await page.clock.fastForward(UNDO);
  await expect(page.getByText("Suggestion dismissed.")).toBeHidden();
  await page.reload();
  await expect(cards(page)).toHaveCount(2);
});

test("takes a dismissal back with Undo, before it is sent", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await loaded(page);
  await dismissOf(cards(page).first()).click();
  await expect(cards(page)).toHaveCount(2);
  await page
    .getByRole("status")
    .filter({ hasText: "Suggestion dismissed." })
    .getByRole("button", { name: "Undo" })
    .click();
  await expect(cards(page)).toHaveCount(3);
  await expect(cards(page).first()).toContainText("supplier of");
  await expect(dismissOf(cards(page).first())).toBeFocused();
  // Nothing was sent: past the toast's time, a reload still lists it.
  await page.clock.fastForward(UNDO);
  await page.reload();
  await expect(cards(page)).toHaveCount(3);
});

test("keeps every dismissal not sent yet hidden as the list refreshes", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await loaded(page);
  // The second dismissal sends the first, whose refreshed list still holds
  // the second: it stays hidden, then is sent in its turn.
  await dismissOf(cards(page).first()).click();
  await expect(cards(page)).toHaveCount(2);
  await dismissOf(cards(page).first()).click();
  await expect(cards(page)).toHaveCount(1);
  await expect(suggestions(page)).toContainText("Rows of “customers”");
  await page.waitForTimeout(1000);
  await expect(cards(page)).toHaveCount(1);
  await page.clock.fastForward(UNDO);
  await expect(page.getByText("Suggestion dismissed.")).toBeHidden();
  await page.reload();
  await expect(cards(page)).toHaveCount(1);
});

test("puts a card back, focused, when its dismissal fails", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await loaded(page);
  await mock.configure({ healDismiss: "fails" });
  await dismissOf(cards(page).first()).click();
  await expect(cards(page)).toHaveCount(2);
  await page.clock.fastForward(UNDO);
  await expect(
    page.getByText(/^Couldn’t dismiss the suggestion/),
  ).toBeVisible();
  await expect(cards(page)).toHaveCount(3);
  await expect(dismissOf(cards(page).first())).toBeFocused();
});

test("says the rows weren't checked when the audit didn't measure them", async ({
  page,
}) => {
  await mock.configure({ healRowsMeasured: false, loadStepMs: 200 });
  await loaded(page);
  await expect(
    page.getByText(/^Last audited .*\. Rows weren’t checked on this load\.$/),
  ).toBeVisible();
});

test("says suggestions aren't on where audits don't run", async ({ page }) => {
  await mock.configure({ healEnabled: false, loadStepMs: 200 });
  await signIn(page);
  await load(page);
  await page.goto(O);
  await expect(
    page.getByRole("list", { name: "Pages" }).getByRole("link", {
      name: "Overview",
    }),
  ).toBeVisible();
  await expect(
    page.getByRole("list", { name: "Pages" }).getByRole("link", {
      name: "Suggestions",
    }),
  ).toHaveCount(0);
  await page.goto(`${O}/suggestions`);
  await expect(page.getByText("Suggestions aren’t turned on")).toBeVisible();
  await expect(cards(page)).toHaveCount(0);
  await expect(page.getByText("No suggestions")).toHaveCount(0);
});

for (const [healRun, said] of [
  ["running", /^Auditing the current load now/],
  ["failed", /^The last audit .*failed/],
  ["incomplete", /^The last audit .*didn’t finish/],
] as const)
  test(`says the current load isn't audited whole while its audit is ${healRun}`, async ({
    page,
  }) => {
    await mock.configure({ healRun, loadStepMs: 200 });
    await signIn(page);
    await load(page);
    await openUntil(page, () =>
      expect(page.getByText(said)).toBeVisible({ timeout: 2000 }),
    );
    await expect(
      page.getByText("The current load hasn’t been fully audited yet."),
    ).toBeVisible();
    // No suggestion is shown, so none is said to be from an earlier audit.
    await expect(page.getByText(/from the audit before it/)).toHaveCount(0);
  });

test("shows the suggestions once the current load's audit completes, unasked", async ({
  page,
}) => {
  await mock.configure({ healRun: "running", loadStepMs: 200 });
  await page.clock.install();
  await signIn(page);
  await load(page);
  await openUntil(page, () =>
    expect(page.getByText(/^Auditing the current load now/)).toBeVisible({
      timeout: 2000,
    }),
  );
  await expect(cards(page)).toHaveCount(0);
  // The audit completes: the page's next poll shows it, with no reload.
  await mock.configure({ healRun: null });
  await page.clock.fastForward(16_000);
  await expect(cards(page)).toHaveCount(3);
  await expect(page.getByText(/^Last audited/)).toBeVisible();
});

test("marks another load's suggestions until the current load is audited", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await loaded(page);
  await expect(page.getByText("Another load")).toHaveCount(0);

  await mock.configure({ healRun: "running" });
  await load(page, true);
  await openUntil(page, () =>
    expect(
      cards(page).first().getByText("Another load", { exact: true }),
    ).toBeVisible({ timeout: 2000 }),
  );
  await expect(cards(page)).toHaveCount(3);
  await expect(
    page
      .getByText(/^The last audit .*was of another load/)
      .or(page.getByText(/^Auditing the current load now/)),
  ).toBeVisible();
});

test("drops a suggestion an audit superseded as it was dismissed", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await loaded(page);
  await mock.configure({ healDismiss: "superseded" });
  await dismissOf(cards(page).first()).click();
  await page.clock.fastForward(UNDO);
  // Its dismissal fails once sent, and the refreshed list no longer holds
  // it: no stale card to click again.
  await expect(
    page.getByText(/^Couldn’t dismiss the suggestion: .*superseded/),
  ).toBeVisible();
  await expect(cards(page)).toHaveCount(2);
  await expect(suggestions(page)).not.toContainText("supplier of");
});
