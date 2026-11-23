import type { Page, Request } from "@playwright/test";
import {
  CONTRACT,
  expect,
  file,
  loadVersion3,
  makeContractsCurrent,
  mock,
  signIn,
  startContractsLoad,
  test,
  uploadAndLoadContract,
  uploadInData,
  uploadNaming,
} from "./support/mock";

// The review walkthrough (the Review page's Facts tab, reviews/walkthrough.tsx):
// the current load's cards one at a time, each with Founder's suggestion
// where a rule makes one, settled by a key or a button and saved at once
// (Z takes it back), Accept all like this only where the suggestion is
// measured, a refusal put back next, the walk rebuilt when a new load
// becomes current, the FYI and Covered lanes, and members reading only.

const O = "/ontology/ont_acme_contracts";
// Two held claims (signed_with, scored 0.72) and Umbrella Holdings, a name
// no row holds, beside the contract's facts.
const HEDGED =
  "# Hedged deal\n\nUmbrella Holdings was copied. Globex Corporation and Initech may have signed.\n";
// Two more held claims of the same link and score, in a contract of its own.
const SECOND =
  "# Second deal\n\nGlobex Corporation and Initech may have signed.\n";
// Random checks that found every claim scored 0.7 to 0.8 right: 40 lets
// Founder suggest approving them, 80 also measures them enough to accept
// them all at once (a lower bound of 0.95).
const MEASURED = (right: number) => ({
  seedReviewLabels: { band: "0.7" as const, right, wrong: 0 },
});

const card = (page: Page) => page.getByRole("article");
const choiceButtons = (page: Page) =>
  card(page).getByRole("list", { name: "Choices" }).getByRole("button");
const heading = (page: Page) => card(page).getByRole("heading", { level: 2 });
const progress = (page: Page) => page.locator(".rv-progress");
const lane = (page: Page, name: "Decide" | "FYI" | "Covered") =>
  page
    .getByRole("radiogroup", { name: "Lane" })
    .getByRole("radio", { name: new RegExp(`^${name}`) });
const toast = (page: Page, text: string) =>
  page.locator(".rv-toast").filter({ hasText: text });
const isSettle = (r: Request) =>
  r.method() === "POST" && /\/reviews\/[^/]+:settle$/.test(r.url());
const isRemove = (r: Request) =>
  r.method() === "DELETE" && /\/review-decisions\/[^/]+$/.test(r.url());
const body = (r: Request) => r.postDataJSON() as Record<string, unknown>;
/**
 * The held claim shown first and the other, by the customer each names: the
 * queue ranks them alike, so either may come first.
 */
async function claims(page: Page) {
  await expect(heading(page)).toContainText("signed_with");
  const first = (await heading(page).textContent())!.includes("Initech")
    ? "Initech LLC"
    : "Globex Corporation";
  return {
    first,
    other: first === "Initech LLC" ? "Globex Corporation" : "Initech LLC",
  };
}
const RELEASE = {
  decision: {
    release: { statementHash: expect.stringMatching(/^[0-9a-f]{64}$/) },
  },
  drawn: "ranked",
};

/** Loads the contract and `more`, then opens the walkthrough. */
async function walk(
  page: Page,
  more: ReturnType<typeof file>[],
  config: Parameters<typeof mock.configure>[0] = {},
) {
  await mock.configure({ loadStepMs: 200, ...config });
  await signIn(page);
  await uploadAndLoadContract(page, more);
  await page.goto(`${O}/review`);
  await expect(heading(page)).toBeVisible();
}

test("walks from the dock's Review N: Enter accepts, Z takes it back, S skips, a digit chooses, then the finish offers Load again", async ({
  page,
}) => {
  await mock.configure({
    loadStepMs: 1500,
    documentLinked: "legal_form",
    ...MEASURED(40),
  });
  await signIn(page);
  await uploadInData(page, [
    file("contract.md", CONTRACT),
    file("variant.md", HEDGED),
  ]);
  await makeContractsCurrent(page);
  await startContractsLoad(page);
  await expect(page).toHaveURL(/\/loads\/load_/);
  // Another page's dock says the load left reviews, and opens them; it
  // names no count until the Review page has counted the cards to decide.
  await page.goto("/data");
  const dock = page.getByRole("region", { name: "Jobs in progress" });
  await expect(dock).toContainText("Data loaded", { timeout: 30_000 });
  await dock.getByRole("link", { name: "Review", exact: true }).click();
  await expect(page).toHaveURL(new RegExp(`${O}/review$`));
  const { first, other } = await claims(page);
  await expect(heading(page)).toHaveText(
    `Is this right: Hedged deal → signed_with → ${first}?`,
  );
  await expect(progress(page)).toHaveText(
    "0 of 4 · 0 accepted · 0 changed · 0 skipped",
  );
  await expect(card(page)).toContainText(
    "Founder suggests ApproveRuleMeasured · right 40 of 40",
  );
  // The suggestion is the primary choice, shown once, and Enter takes it;
  // the other choice follows with its key.
  await expect(choiceButtons(page)).toHaveText([
    /^Approve\s*$/,
    /^X\s*Reject$/,
  ]);
  await expect(choiceButtons(page).first()).toHaveAttribute(
    "aria-keyshortcuts",
    "Enter",
  );
  await expect(choiceButtons(page).nth(1)).toHaveAttribute(
    "aria-keyshortcuts",
    "X",
  );
  // Measured, but not enough to accept them all at once.
  await expect(
    page.getByRole("button", { name: /Accept all like this/ }),
  ).toHaveCount(0);

  // Enter accepts the suggestion, saved at once, and moves on.
  let sent = page.waitForRequest(isSettle);
  await page.keyboard.press("Enter");
  expect(body(await sent)).toEqual(RELEASE);
  await expect(heading(page)).toContainText(other);
  await expect(heading(page)).toBeFocused();
  await expect(progress(page)).toHaveText(
    "1 of 4 · 1 accepted · 0 changed · 0 skipped",
  );
  await expect(toast(page, "Approved.")).toBeVisible();

  // Z takes it back and shows it again.
  const removed = page.waitForRequest(isRemove);
  await page.keyboard.press("z");
  await removed;
  await expect(heading(page)).toContainText(first);
  await expect(
    toast(page, "Approval taken back: the claim is held for review again."),
  ).toBeVisible();
  await expect(progress(page)).toHaveText(
    "0 of 4 · 0 accepted · 0 changed · 0 skipped",
  );

  // S skips it.
  await page.keyboard.press("s");
  await expect(heading(page)).toContainText(other);
  await expect(progress(page)).toHaveText(
    "1 of 4 · 0 accepted · 0 changed · 1 skipped",
  );

  // A choice's key saves it at once; no digit is shown, so none chooses.
  await page.keyboard.press("1");
  await expect(heading(page)).toContainText(other);
  sent = page.waitForRequest(isSettle);
  await page.keyboard.press("x");
  expect(body(await sent)).toEqual({
    decision: {
      reject: { statementHash: expect.stringMatching(/^[0-9a-f]{64}$/) },
    },
    drawn: "ranked",
  });
  // A name the load linked by its legal form: a choice applies from the
  // next load.
  await expect(heading(page)).toContainText("names “Umbrella Holdings");
  await expect(card(page)).toContainText("Applies from the next load.");
  sent = page.waitForRequest(isSettle);
  await page.keyboard.press("n");
  expect(body(await sent)).toEqual({
    decision: { resolve: { notInData: true } },
    drawn: "ranked",
  });
  // The same name in the other document: Founder suggests the choice just
  // made, as a repeat of it.
  await expect(heading(page)).toContainText("names “Umbrella Holdings");
  await expect(card(page)).toContainText(
    "Founder suggests Not a Customer we haveRuleYour earlier choice ×1",
  );
  sent = page.waitForRequest(isSettle);
  await page.keyboard.press("Enter");
  expect(body(await sent)).toEqual({
    decision: { resolve: { notInData: true } },
    drawn: "ranked",
  });

  // The finish: what applies now, what waits for the next load, and the
  // card skipped.
  const done = page.getByRole("region", { name: /skipped$/ });
  await expect(done.getByRole("heading", { level: 2 })).toHaveText(
    "Through to the end, 1 skipped",
  );
  await expect(progress(page)).toHaveText(
    "4 of 4 · 1 accepted · 2 changed · 1 skipped",
  );
  await expect(done).toContainText("From the next load2 choices");
  await expect(done.getByRole("link", { name: "Load again" })).toHaveAttribute(
    "href",
    `${O}?new-load=1`,
  );
  await done.getByRole("button", { name: "Go through 1 skipped" }).click();
  await expect(heading(page)).toContainText(first);
  // The overview says what the load's documents left (a fresh page has
  // counted no cards), and starts the walk too; back from the walk, it says
  // the walk's count of cards still to decide: the one skipped.
  await page.goto(O);
  await expect(page.getByText(/^\d+ reviews waiting$/)).toBeVisible();
  await page.getByRole("link", { name: "Start reviewing" }).click();
  await expect(page).toHaveURL(new RegExp(`${O}/review$`));
  const facts = page
    .getByRole("tab", { name: /^Facts/ })
    .locator(".hf-tab-count");
  await expect(facts).toHaveText("1");
  await expect(lane(page, "Decide")).toHaveText(/^Decide\s*1$/);
  await page
    .getByRole("list", { name: "Pages" })
    .getByRole("link", { name: "Overview" })
    .click();
  await expect(page).toHaveURL(new RegExp(`${O}$`));
  await expect(
    page.getByText("1 card to decide", { exact: true }),
  ).toBeVisible();
});

/** Three documents name Umbrella Holdings; the walk opens on the first. */
async function naming(
  page: Page,
  nameInPlace: "written" | "not_replayed" | "unconfirmed",
) {
  await mock.configure({ loadStepMs: 200, nameInPlace });
  await signIn(page);
  await uploadNaming(page, "Umbrella Holdings", 3);
  await makeContractsCurrent(page);
  await loadVersion3(page);
  await page.goto(`${O}/review`);
  await expect(heading(page)).toContainText("names “Umbrella Holdings");
  await expect(progress(page)).toHaveText(
    "0 of 3 · 0 accepted · 0 changed · 0 skipped",
  );
  await expect(card(page)).toContainText(
    "Applies now to 3 documents asking about this name.",
  );
  // The only record the load found is the suggestion: the primary choice.
  await expect(choiceButtons(page)).toHaveText([
    /^Link to .+\S\s*$/,
    /^N\s*Not a Customer we have$/,
  ]);
  // What that choice does, under the choices and as its button's description.
  const none =
    "“Umbrella Holdings” isn’t a Customer in your data: it stays unlinked in this document and no Customer is made. Facts and held claims that name it here are dropped.";
  await expect(card(page)).toContainText(none);
  await expect(
    card(page).getByRole("button", { name: "Not a Customer we have" }),
  ).toHaveAccessibleDescription(none);
  await expect(lane(page, "Covered")).toHaveText(/^Covered\s*0$/);
  const sent = page.waitForRequest(isSettle);
  await page.keyboard.press("Enter");
  expect(body(await sent)).toEqual({
    decision: { resolve: { entityKey: expect.any(String) } },
    drawn: "ranked",
  });
  await expect(
    toast(page, "Linked, with 2 other documents asking about this name."),
  ).toBeVisible();
}

test("a link to a name settles every document asking about it, which go to Covered", async ({
  page,
}) => {
  await naming(page, "written");
  await expect(lane(page, "Covered")).toHaveText(/^Covered\s*2$/);
  await expect(progress(page)).toHaveText(
    "3 of 3 · 1 accepted · 0 changed · 0 skipped · 2 covered",
  );
  await expect(lane(page, "Decide")).toHaveText(/^Decide\s*0$/);
  await expect(
    page.getByRole("heading", { level: 2, name: "All caught up" }),
  ).toBeVisible();
  await expect(
    page.getByText("1 choice, and 2 names they settled", { exact: false }),
  ).toBeVisible();
  await lane(page, "Covered").click();
  await expect(page).toHaveURL(/[?&]lane=covered/);
  const covered = page.getByRole("list", { name: "Covered" });
  await expect(covered.getByRole("listitem")).toHaveCount(2);
  await expect(covered.getByRole("listitem").first()).toContainText(
    /settled by your link in naming-\d\.md/,
  );
  // Each covered card says so too, in the same words.
  await lane(page, "Decide").click();
  await page
    .getByRole("region", { name: "All caught up" })
    .getByRole("button", { name: "Back" })
    .click();
  await expect(card(page).locator(".rv-outcome")).toHaveText(
    /^Settled by your link in naming-\d\.md: the load links this name in every document asking about it\.$/,
  );
  // The load linked the name in place: a fresh walk has none of the three
  // to decide, and covers none, once its job has run.
  await expect(async () => {
    await page.goto(`${O}/review`);
    await expect(lane(page, "Covered")).toHaveText(/^Covered\s*0$/, {
      timeout: 3000,
    });
    await expect(
      page.getByRole("heading", { level: 2, name: "Nothing to decide" }),
    ).toBeVisible({ timeout: 3000 });
  }).toPass({ timeout: 20_000 });
});

test("a link whose job failed before saying offers its choices again, and is still to decide", async ({
  page,
}) => {
  await naming(page, "unconfirmed");
  // Its job failed: the link and the two it covered are all to decide.
  await expect(lane(page, "Covered")).toHaveText(/^Covered\s*0$/, {
    timeout: 15_000,
  });
  await expect(lane(page, "Decide")).toHaveText(/^Decide\s*3$/);
  await expect(progress(page)).toHaveText(
    "0 of 3 · 0 accepted · 0 changed · 0 skipped",
  );
  const done = page.getByRole("region", { name: "3 cards still to decide" });
  await expect(done).not.toContainText("Applied now");
  await done
    .getByRole("button", { name: "Go to the first still to decide" })
    .click();
  await expect(card(page).locator(".rv-outcome")).toHaveText(
    /^Saved: .+, but the job that applies it failed before it said whether it did\. Choose it again to finish\.$/,
  );
  await expect(choiceButtons(page)).toHaveCount(2);
  const sent = page.waitForRequest(isSettle);
  await page.keyboard.press("Enter");
  expect(body(await sent)).toEqual({
    decision: { resolve: { entityKey: expect.any(String) } },
    drawn: "ranked",
  });
});

test("undoing a link the load applied in place shows it taken back, not to decide again", async ({
  page,
}) => {
  await naming(page, "written");
  await page.keyboard.press("z");
  await expect(
    toast(page, "Choice removed: it stops applying from the next load."),
  ).toBeVisible();
  // The load keeps the link, so the card and the two it covered aren't
  // asked again here: settling them would find no review once its job ran.
  await expect(heading(page)).toContainText("names “Umbrella Holdings");
  await expect(card(page).locator(".rv-outcome")).toHaveText(
    "Link taken back. This load keeps it; the next load asks about this name again.",
  );
  await expect(choiceButtons(page)).toHaveCount(0);
  await expect(lane(page, "Decide")).toHaveText(/^Decide\s*0$/);
  await expect(progress(page)).toHaveText(
    "3 of 3 · 0 accepted · 0 changed · 0 skipped · 3 taken back",
  );
  await expect(progress(page)).not.toContainText("elsewhere");
});

test("a link the load can't apply in place waits for the next load, and its other documents are to decide again", async ({
  page,
}) => {
  await naming(page, "not_replayed");
  // The load refuses it in place (read before names could be settled so):
  // the two others it would have settled are open again.
  await expect(lane(page, "Covered")).toHaveText(/^Covered\s*0$/, {
    timeout: 15_000,
  });
  await expect(progress(page)).toHaveText(
    "1 of 3 · 1 accepted · 0 changed · 0 skipped",
  );
  const done = page.getByRole("region", { name: "2 cards still to decide" });
  // Nothing applied now: no group for it.
  await expect(done).not.toContainText("Applied now");
  await expect(done).toContainText(
    "From the next load1 choice the current load doesn’t take in place",
  );
  await expect(done.getByRole("link", { name: "Load again" })).toBeVisible();
  await done
    .getByRole("button", { name: "Go to the first still to decide" })
    .click();
  await expect(heading(page)).toContainText("names “Umbrella Holdings");
  await expect(card(page)).toContainText(
    /Founder suggests .+RuleYour earlier choice ×1/,
  );
  // The link itself says why it waits.
  await page.keyboard.press("ArrowLeft");
  await expect(card(page).locator(".rv-outcome")).toContainText(
    "This load was built before names could be applied at once; the next load applies it.",
  );
});

test("accepts all like this from a ticked list: untick one, see it go, then take back the batch", async ({
  page,
}) => {
  await walk(
    page,
    [file("variant.md", HEDGED), file("second.md", SECOND)],
    MEASURED(80),
  );
  await expect(card(page)).toContainText("Measured · right 80 of 80");
  await expect(
    page.getByRole("button", { name: "Accept all like this (4)" }),
  ).toBeVisible();
  await page.keyboard.press("g");
  const dialog = page.getByRole("dialog", { name: "Accept all like this" });
  await expect(dialog).toContainText(
    "Founder suggests Approve for all 4: random checks of claims scored 0.7 to 0.8 were right 80 of 80 times. Untick any you want to read yourself.",
  );
  // It opens on its confirm.
  await expect(dialog.getByRole("button", { name: "Approve 4" })).toBeFocused();
  const ticks = dialog.getByRole("checkbox", { name: "Include this card" });
  await expect(ticks).toHaveCount(4);
  for (const t of await ticks.all()) await expect(t).toBeChecked();
  await ticks.nth(3).uncheck();
  const settles: Request[] = [],
    removes: Request[] = [];
  page.on("request", (r) => {
    if (isSettle(r)) settles.push(r);
    if (isRemove(r)) removes.push(r);
  });
  await dialog.getByRole("button", { name: "Approve 3" }).click();
  await expect(dialog.getByRole("status")).toHaveText("Saved 3 choices.");
  expect(settles.map(body)).toEqual([RELEASE, RELEASE, RELEASE]);
  await expect(progress(page)).toHaveText(
    "3 of 6 · 3 accepted · 0 changed · 0 skipped",
  );
  await dialog.getByRole("button", { name: "Undo these 3" }).click();
  await expect(dialog.getByRole("status")).toHaveText(
    "The batch is taken back: its cards are open again.",
  );
  expect(removes).toHaveLength(3);
  await dialog.getByRole("button", { name: "Done" }).click();
  await expect(dialog).toHaveCount(0);
  await expect(progress(page)).toHaveText(
    "0 of 6 · 0 accepted · 0 changed · 0 skipped",
  );
});

test("puts a refused choice back next, saying why, to choose again", async ({
  page,
}) => {
  await walk(page, [file("variant.md", HEDGED)], { settleRefusals: 1 });
  const { first, other } = await claims(page);
  // No suggestion (nothing measures the score yet): no primary, the
  // choices numbered as they show.
  await expect(choiceButtons(page)).toHaveText([
    /^1\s*Approve$/,
    /^X\s*Reject$/,
  ]);
  const sent = page.waitForResponse((r) => isSettle(r.request()));
  await page.keyboard.press("1");
  expect((await sent).status()).toBe(400);
  // The card shown is the next; the refused one follows it.
  await expect(heading(page)).toContainText(other);
  const rail = page.getByRole("navigation", { name: "Next cards" });
  await expect(rail.getByRole("listitem").nth(1)).toContainText(first);
  await expect(progress(page)).toHaveText(
    "0 of 4 · 0 accepted · 0 changed · 0 skipped",
  );
  await page.keyboard.press("s");
  await expect(heading(page)).toContainText(first);
  await expect(card(page).getByRole("alert")).toHaveText(
    "Couldn’t save it: this claim reads differently now; reload the review and decide on what it says Choose again.",
  );
  const again = page.waitForResponse((r) => isSettle(r.request()));
  await page.keyboard.press("1");
  expect((await again).status()).toBe(200);
  await expect(progress(page)).toHaveText(
    "2 of 4 · 0 accepted · 1 changed · 1 skipped",
  );
});

test("a new load made current mid-walk says so and rebuilds the walk", async ({
  page,
  context,
}) => {
  await walk(page, [file("variant.md", HEDGED)], MEASURED(40));
  const { other } = await claims(page);
  const saved = page.waitForResponse((r) => isSettle(r.request()));
  await page.keyboard.press("Enter");
  expect((await saved).status()).toBe(200);
  await expect(progress(page)).toHaveText(
    "1 of 4 · 1 accepted · 0 changed · 0 skipped",
  );
  // Someone loads again in another tab.
  const tab = await context.newPage();
  await loadVersion3(tab);
  await tab.close();
  // Nothing on the page polls the loads once none is busy: a visibility
  // change (the person coming back to the tab) reads them again.
  const banner = page.locator(".rv-banner");
  await expect(async () => {
    await page.evaluate(() =>
      window.dispatchEvent(new Event("visibilitychange")),
    );
    await expect(banner).toContainText(
      "A new load is current. Choices saved before it started were read by it, and the walk now follows it.",
      { timeout: 2000 },
    );
  }).toPass({ timeout: 20_000 });
  // The new load wrote the approval: its claim isn't asked about again.
  await expect(progress(page)).toHaveText(
    "0 of 3 · 0 accepted · 0 changed · 0 skipped",
  );
  await expect(heading(page)).toContainText(other);
  await banner.getByRole("button", { name: "Dismiss" }).click();
  await expect(banner).toHaveCount(0);
});

test("members read the cards, with Next and the reading keys, but no choices", async ({
  page,
}) => {
  await walk(page, [file("variant.md", HEDGED)], MEASURED(40));
  await page.context().clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto(`${O}/review`);
  const { first, other } = await claims(page);
  // Said once, above the walk, not again on the card.
  await expect(page.getByText("Owners and admins settle reviews.")).toHaveCount(
    1,
  );
  await expect(card(page)).not.toContainText("Owners and admins settle");
  for (const name of [/^Accept/, /Approve/, /Reject/, /^Undo/])
    await expect(card(page).getByRole("button", { name })).toHaveCount(0);
  const settles: Request[] = [];
  page.on("request", (r) => {
    if (isSettle(r)) settles.push(r);
  });
  await page.keyboard.press("Enter");
  await page.keyboard.press("1");
  await page.keyboard.press("x");
  await expect(heading(page)).toContainText(first);
  // S and → go to the next card, ← back, marking nothing skipped.
  await page.keyboard.press("s");
  await expect(heading(page)).toContainText(other);
  await page.keyboard.press("ArrowLeft");
  await expect(heading(page)).toContainText(first);
  await card(page).getByRole("button", { name: /^Next/ }).click();
  await expect(heading(page)).toContainText(other);
  // Next on to the last card, then past it to the finish and its spot
  // checks.
  for (let i = 0; i < 3; i += 1)
    await card(page).getByRole("button", { name: /^Next/ }).click();
  await expect(
    page.getByRole("heading", { level: 3, name: "Spot checks" }),
  ).toBeVisible();
  await expect(card(page)).toHaveCount(0);
  expect(settles).toHaveLength(0);
  await expect(progress(page)).toHaveText(
    "0 of 4 · 0 accepted · 0 changed · 0 skipped",
  );
  // The finish words the cards as waiting for someone who can decide them.
  const done = page.getByRole("region", {
    name: "4 cards wait for an owner or admin",
  });
  await expect(done).not.toContainText("still to decide");
  await done.getByRole("button", { name: "Go back to the first" }).click();
  await expect(heading(page)).toContainText(first);
});

test("FYI holds a name the model linked, with Undo link", async ({ page }) => {
  await walk(page, [file("variant.md", HEDGED)], {
    documentLinked: "adjudicated",
  });
  await expect(lane(page, "FYI")).toHaveText(/^FYI\s*[1-9]/);
  await lane(page, "FYI").click();
  const fyi = page.getByRole("list", { name: "For information" });
  const linked = fyi
    .getByRole("listitem")
    .filter({ hasText: "A name the model linked" })
    .filter({ hasText: "Umbrella Holdings" })
    .first();
  await expect(linked).toBeVisible();
  const sent = page.waitForRequest(isSettle);
  await linked.getByRole("button", { name: "Undo link", exact: true }).click();
  expect(body(await sent)).toEqual({
    decision: { resolve: { notInData: true } },
    drawn: "ranked",
  });
  await expect(
    linked.getByText("Decision saved. The next load applies it."),
  ).toBeVisible();
});

test("F asks Founder: the dock opens with the card in its composer, and nothing is sent; keys typed there, in a dialog or on a button are theirs", async ({
  page,
}) => {
  await walk(page, [file("variant.md", HEDGED)]);
  const { first } = await claims(page);
  const posts: string[] = [];
  page.on("request", (r) => {
    if (r.method() === "POST") posts.push(r.url());
  });
  await page.keyboard.press("f");
  const composer = page
    .getByRole("complementary", { name: "Founder" })
    .getByRole("textbox", { name: "Message" });
  await expect(composer).toBeVisible();
  await expect(composer).toHaveValue(
    new RegExp(
      `^Help me decide review [0-9a-f]+ in “variant\\.md”\\. Is this claim right: Hedged deal \\(Contract\\) → signed_with → ${first} \\(Customer\\)\\?.*don’t settle it\\.$`,
    ),
  );
  expect(posts.filter((u) => !u.includes("/review-labels"))).toEqual([]);

  // Typing in the composer settles, skips and undoes nothing.
  const removes: string[] = [];
  page.on("request", (r) => {
    if (isRemove(r)) removes.push(r.url());
  });
  await composer.focus();
  await composer.press("End");
  await composer.pressSequentially(" 1 2 n x z s g");
  await composer.press("ArrowRight");
  await composer.press("ArrowLeft");
  await expect(composer).toHaveValue(/ 1 2 n x z s g$/);
  await expect(heading(page)).toContainText(first);
  await expect(progress(page)).toHaveText(
    /^0 of \d+ · 0 accepted · 0 changed · 0 skipped$/,
  );

  // Nor do keys pressed while a dialog is open.
  await heading(page).focus();
  await page.keyboard.press("?");
  const help = page.getByRole("dialog", { name: "Keyboard shortcuts" });
  await expect(help).toBeVisible();
  for (const key of ["1", "x", "s", "ArrowRight", "Enter"])
    await page.keyboard.press(key);
  await page.keyboard.press("Escape");
  await expect(help).toHaveCount(0);
  await expect(heading(page)).toContainText(first);
  // The card's ? button opens them too.
  await card(page).getByRole("button", { name: "Keyboard shortcuts" }).click();
  await expect(help).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(help).toHaveCount(0);

  // Enter on a focused button is that button's: Skip skips, nothing is saved.
  await card(page).getByRole("button", { name: /^Skip/ }).focus();
  await page.keyboard.press("Enter");
  await expect(heading(page)).not.toContainText(first);
  await expect(progress(page)).toHaveText(
    /^1 of \d+ · 0 accepted · 0 changed · 1 skipped$/,
  );
  // Holding a key down decides one card, not one per repeat.
  await heading(page).focus();
  await page.keyboard.down("x");
  await page.keyboard.down("x");
  await page.keyboard.down("x");
  await page.keyboard.up("x");
  await expect(progress(page)).toHaveText(
    /^2 of \d+ · 0 accepted · 1 changed · 1 skipped$/,
  );
  expect(posts.filter((u) => /:settle$/.test(u))).toHaveLength(1);
  expect(removes).toEqual([]);
});

test("the rail tells cards apart in two lines, and the filters drop below their own fields", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await walk(page, [file("variant.md", HEDGED), file("second.md", SECOND)]);
  const rail = page
    .getByRole("navigation", { name: "Next cards" })
    .getByRole("listitem");
  // A claim by the customer it names, then its kind and document.
  const lines = await rail.allTextContents();
  expect(
    lines.filter((l) =>
      /^(Globex Corporation|Initech LLC)Held claim · (variant|second)\.md$/.test(
        l,
      ),
    ),
  ).toHaveLength(4);
  expect(new Set(lines).size).toBe(lines.length);
  expect(lines.some((l) => /^“.+”Name · \S+\.md$/.test(l))).toBe(true);

  // Each filter is a field of its own: its list opens under it, on the
  // option chosen.
  const kind = page.getByRole("combobox", { name: "Kind" });
  await expect(kind).toHaveText("Any");
  await kind.click();
  const list = page.getByRole("listbox", { name: "Kind" });
  await expect(list.getByRole("option", { name: "Any" })).toBeFocused();
  const [field, popover] = await Promise.all([
    kind.boundingBox(),
    list.boundingBox(),
  ]);
  expect(popover!.y).toBeGreaterThanOrEqual(field!.y + field!.height);
  await list.getByRole("option", { name: "Held claims" }).click();
  await expect(page).toHaveURL(/[?&]kind=claim/);
  await expect(kind).toHaveText("Held claims");
  await expect(progress(page)).toHaveText(
    "0 of 4 · 0 accepted · 0 changed · 0 skipped",
  );
  await kind.click();
  await expect(list.getByRole("option", { name: "Held claims" })).toBeFocused();
  await page.keyboard.press("Escape");
  const doc = page.getByRole("combobox", { name: "Document" });
  await doc.click();
  await page
    .getByRole("listbox", { name: "Document" })
    .getByRole("option", { name: "second.md" })
    .click();
  await expect(progress(page)).toHaveText(
    "0 of 2 · 0 accepted · 0 changed · 0 skipped",
  );
  await page.getByRole("button", { name: "Clear filters" }).click();
  await expect(page).not.toHaveURL(/[?&](kind|doc)=/);
  await expect(kind).toHaveText("Any");
});
