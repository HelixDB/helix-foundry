import type { Page } from "@playwright/test";
import {
  expect,
  makeContractsCurrent,
  mock,
  signIn,
  test,
} from "./support/mock";

// Suggestions, the Review page's Ontology tab: what audits of the current
// load found the ontology lacks, each with its kind, support, examples and
// what it proposes; dismissing one takes it off the list. Where
// foundry.heal.enabled is off there are none, and no tab to open.

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
  if (!again) await makeContractsCurrent(page);
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

// Opens the Ontology tab again until check passes: a load just made
// becomes current, and is audited, a moment after it is made.
async function openUntil(page: Page, check: () => Promise<void>) {
  await expect(async () => {
    await page.goto(`${O}/review?tab=ontology`);
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
    expect(cards(page)).toHaveCount(4, { timeout: 2000 }),
  );
}

test("lists the current load's suggestions and dismisses one", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await page.clock.install();
  await signIn(page);
  await page.goto(`${O}/review?tab=ontology`);
  await expect(page.getByText("No suggestions")).toBeVisible();

  await load(page);
  await openUntil(page, () =>
    expect(cards(page)).toHaveCount(4, { timeout: 2000 }),
  );
  await page.goto(O);
  await page
    .getByRole("list", { name: "Pages" })
    .getByRole("link", { name: /^Review/ })
    .click();
  await page.getByRole("tab", { name: /^Ontology/ }).click();
  await expect(page).toHaveURL(new RegExp(`${O}/review\\?tab=ontology$`));
  await expect(cards(page)).toHaveCount(4);
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
  // 4.8.0 gathered two forms of the relationship into one finding: a
  // person's reading of the quotes is the only check they belong together.
  await expect(first).toContainText(
    "Reported as “supplier_of”, “supplier_of_packaging”: check the quotes say the same thing.",
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
  await expect(cards(page)).toHaveCount(3);
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
  await expect(cards(page)).toHaveCount(3);
});

test("drafts a link a suggestion says documents state, on Generate", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await loaded(page);
  const none = cards(page).filter({
      hasText: "Documents state “subsidiary of”",
    }),
    route = cards(page).first();
  // A relationship no link covers that proposes nothing: a manager can
  // still add a link for it, and is told what that gets them.
  await expect(none).toContainText("Relationship no link covers");
  await expect(none).toContainText("In document set “General”");
  await expect(none).toContainText("9 reviews in 4 documents");
  await expect(none).not.toContainText("Suggested:");
  await expect(
    none.getByText("Adding this link lets future loads store these 9 claims.", {
      exact: true,
    }),
  ).toBeVisible();
  // Only a relationship no link covers has one.
  for (const other of ["“globex corp”", "Rows of “customers”"]) {
    const card = cards(page).filter({ hasText: other });
    await expect(card).toHaveCount(1);
    await expect(card.getByRole("link", { name: "Add this link" })).toHaveCount(
      0,
    );
    await expect(card).not.toContainText("Adding this link");
  }
  // Named Add this link alone, described by its card's title: an accessible
  // name may reach analytics. Beside it, the agent drafts the link.
  const add = none.getByRole("link", { name: "Add this link", exact: true });
  await expect(add).not.toHaveAttribute("aria-label");
  await expect(add).toHaveAccessibleDescription(
    "Documents state “subsidiary of”, which no link covers",
  );
  const draft = none.getByRole("link", {
    name: "Draft with the agent",
    exact: true,
  });
  await expect(draft).toHaveAccessibleDescription(
    "Documents state “subsidiary of”, which no link covers",
  );
  await draft.click();
  await expect(page).toHaveURL(new RegExp(`${O}/generate\\?goal=`));
  await expect(page.getByLabel("Goal")).toHaveValue(
    [
      "Add a link for “subsidiary of”: documents of “General” state it.",
      "For example:",
      "“Globex Freight, a subsidiary of Globex, handles delivery.”",
      "“Initech Labs is a wholly owned subsidiary of Initech.”",
      "“Acme Freight signs as a subsidiary of Acme.”",
    ].join("\n"),
  );

  // One that proposes something keeps its proposal, which fills the goal.
  await page.goBack();
  await expect(route).toContainText(
    "Suggested: Let documents of “General” state “supplies”",
  );
  await expect(route).toContainText(
    "Adding this link lets future loads store these 12 claims.",
  );
  await route.getByRole("link", { name: "Add this link", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`${O}/generate\\?goal=`));
  await expect(page.getByLabel("Goal")).toHaveValue(
    /^Let documents of “General” state “supplies”: documents state “supplier of”, also reported as “supplier of packaging”\.\nFor example:\n“Globex supplies Acme with steel under this agreement\.”\n/,
  );
});

// Add this link in place: the current version's Edit tab saves a version
// with the link and the route reading its set, and making that version
// current with the finding moves the current load to it, which the card
// then says, with no new load.
test("adds a link in place, and the current load moves to it", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await loaded(page);
  const none = () =>
    cards(page).filter({ hasText: "Documents state “subsidiary of”" });
  await none()
    .getByRole("link", { name: "Add this link", exact: true })
    .click();
  await expect(page).toHaveURL(
    /\/versions\/[^?]+\?tab=edit&fill=d{64}&link=subsidiary_of&set=set_acme_general&describe=/,
  );
  const form = page.getByRole("region", { name: "Add this link" });
  await expect(form.getByRole("heading")).toHaveText(
    "Add the link “subsidiary_of”",
  );
  await expect(form.getByLabel("Description")).toHaveValue(
    "Subsidiary of, as documents state it: “Globex Freight, a subsidiary of Globex, handles delivery.”",
  );
  // Its ends are the person's to choose: none chosen is refused.
  await form.getByRole("button", { name: "Save as a new version" }).click();
  await expect(form.getByRole("alert")).toHaveText(
    "Choose the types the link joins.",
  );
  for (const end of ["From", "To"]) {
    await form.getByLabel(end).click();
    await page.getByRole("option", { name: "Customer", exact: true }).click();
  }
  await form.getByRole("button", { name: "Save as a new version" }).click();
  await expect(
    page.getByText(/^Version \d+ saved with the link\.$/),
  ).toBeVisible({
    timeout: 15_000,
  });
  await page
    .getByRole("button", { name: "Make current and update the load" })
    .click();
  await expect(page).toHaveURL(new RegExp(`${O}/review\\?tab=ontology$`));
  await expect(none().getByRole("status")).toHaveText(
    /^Updating the current load to version \d+…$/,
  );
  await page.clock.runFor(2_500);
  await expect(none().getByRole("status")).toHaveText(
    /^The current load is on version \d+ now: documents read from now on state this link\./,
  );
  // Done in place: no Add this link, no new load to start.
  await expect(none().getByRole("link", { name: "Add this link" })).toHaveCount(
    0,
  );
  await expect(
    none().getByRole("button", { name: "Start a new load" }),
  ).toHaveCount(0);
});

// Once the load moved, self-healing fills the documents behind the finding
// with the link: the card counts them as they go, then what the fill did.
test("shows the fill of the documents behind the finding", async ({ page }) => {
  await mock.configure({ loadStepMs: 200, healFill: true });
  await loaded(page);
  const none = () =>
    cards(page).filter({ hasText: "Documents state “subsidiary of”" });
  await none()
    .getByRole("link", { name: "Add this link", exact: true })
    .click();
  const form = page.getByRole("region", { name: "Add this link" });
  for (const end of ["From", "To"]) {
    await form.getByLabel(end).click();
    await page.getByRole("option", { name: "Customer", exact: true }).click();
  }
  await form.getByRole("button", { name: "Save as a new version" }).click();
  await expect(
    page.getByText(/^Version \d+ saved with the link\.$/),
  ).toBeVisible({ timeout: 15_000 });
  await page
    .getByRole("button", { name: "Make current and update the load" })
    .click();
  await expect(none().getByRole("status")).toHaveText(
    /^Updating the current load to version \d+…$/,
  );
  await page.clock.runFor(2_500);
  await expect(none().getByRole("status")).toHaveText(
    /^The current load is on version \d+ now/,
  );
  await page.reload();
  await expect(none().getByRole("status")).toHaveText(
    /^Filling the current load \(version \d+\) with this link: 1 of 2 documents, \$0\.01 of \$5\.00\.$/,
  );
  await page.clock.runFor(2_500);
  await expect(none().getByRole("status")).toHaveText(
    /^5 reviews resolved, 4 facts added in the current load \(version \d+\)\.$/,
  );
  await expect(none().getByRole("link", { name: "Add this link" })).toHaveCount(
    0,
  );
});

// A version that changes more than a link needs a new load, which the card
// says, offering one.
test("says when the version needs a new load", async ({ page }) => {
  await mock.configure({ loadStepMs: 200, healMove: "needs_load" });
  await loaded(page);
  const none = () =>
    cards(page).filter({ hasText: "Documents state “subsidiary of”" });
  await none()
    .getByRole("link", { name: "Add this link", exact: true })
    .click();
  const form = page.getByRole("region", { name: "Add this link" });
  for (const end of ["From", "To"]) {
    await form.getByLabel(end).click();
    await page.getByRole("option", { name: "Customer", exact: true }).click();
  }
  await form.getByRole("button", { name: "Save as a new version" }).click();
  await page
    .getByRole("button", { name: "Make current and update the load" })
    .click({ timeout: 15_000 });
  await expect(none().getByRole("status")).toHaveText(
    /^Needs a new load: version \d+ changes more than a link: it adds types\.Deal\.$/,
  );
  await expect(
    none().getByRole("button", { name: "Start a new load" }),
  ).toBeVisible();
});

test("offers Add this link to managers only", async ({ page }) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await load(page);
  await openUntil(page, () =>
    expect(cards(page)).toHaveCount(4, { timeout: 2000 }),
  );
  // A member reads the suggestions, and can't act on them.
  await page.context().clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto(`${O}/suggestions`);
  await expect(cards(page)).toHaveCount(4);
  await expect(
    suggestions(page).getByRole("link", { name: "Add this link" }),
  ).toHaveCount(0);
  await expect(suggestions(page)).not.toContainText("Adding this link");
  await expect(
    suggestions(page).getByRole("button", { name: "Dismiss" }),
  ).toHaveCount(0);
});

test("takes a dismissal back with Undo, before it is sent", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await loaded(page);
  await dismissOf(cards(page).first()).click();
  await expect(cards(page)).toHaveCount(3);
  await page
    .getByRole("status")
    .filter({ hasText: "Suggestion dismissed." })
    .getByRole("button", { name: "Undo" })
    .click();
  await expect(cards(page)).toHaveCount(4);
  await expect(cards(page).first()).toContainText("supplier of");
  await expect(dismissOf(cards(page).first())).toBeFocused();
  // Nothing was sent: past the toast's time, a reload still lists it.
  await page.clock.fastForward(UNDO);
  await page.reload();
  await expect(cards(page)).toHaveCount(4);
});

test("keeps every dismissal not sent yet hidden as the list refreshes", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await loaded(page);
  // The second dismissal sends the first, whose refreshed list still holds
  // the second: it stays hidden, then is sent in its turn.
  await dismissOf(cards(page).first()).click();
  await expect(cards(page)).toHaveCount(3);
  await dismissOf(cards(page).first()).click();
  await expect(cards(page)).toHaveCount(2);
  await expect(suggestions(page)).toContainText("Rows of “customers”");
  await page.waitForTimeout(1000);
  await expect(cards(page)).toHaveCount(2);
  await page.clock.fastForward(UNDO);
  await expect(page.getByText("Suggestion dismissed.")).toBeHidden();
  await page.reload();
  await expect(cards(page)).toHaveCount(2);
});

test("puts a card back, focused, when its dismissal fails", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await loaded(page);
  await mock.configure({ healDismiss: "fails" });
  await dismissOf(cards(page).first()).click();
  await expect(cards(page)).toHaveCount(3);
  await page.clock.fastForward(UNDO);
  await expect(
    page.getByText(/^Couldn’t dismiss the suggestion/),
  ).toBeVisible();
  await expect(cards(page)).toHaveCount(4);
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

test("has no Ontology tab where audits don't run", async ({ page }) => {
  await mock.configure({ healEnabled: false, loadStepMs: 200 });
  await signIn(page);
  await load(page);
  await page.goto(O);
  const review = page
    .getByRole("list", { name: "Pages" })
    .getByRole("link", { name: /^Review/ });
  await expect(review).toBeVisible();
  // An old link to Suggestions opens Review on Facts, its only tab.
  await page.goto(`${O}/suggestions`);
  await expect(page).toHaveURL(new RegExp(`${O}/review\\?tab=ontology$`));
  // Its count follows its name.
  await expect(page.getByRole("tab")).toHaveText([/^Facts/]);
  await expect(page.getByRole("tab", { name: "Facts" })).toHaveAttribute(
    "aria-selected",
    "true",
  );
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
  await expect(cards(page)).toHaveCount(4);
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
  await expect(cards(page)).toHaveCount(4);
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
  await expect(cards(page)).toHaveCount(3);
  await expect(suggestions(page)).not.toContainText("supplier of");
});
