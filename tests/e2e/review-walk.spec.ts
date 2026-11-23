import type { Page, Request, Route } from "@playwright/test";
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
// A card's own settle (the FYI lane, spot checks), and the walk's: through
// its outbox, a batch of the choices made while one was in flight.
const isSettle = (r: Request) =>
  r.method() === "POST" && /\/reviews\/[^/]+:settle$/.test(r.url());
const isBatch = (r: Request) =>
  r.method() === "POST" && /\/reviews:batchSettle$/.test(r.url());
type BatchBody = {
  loadId: string;
  expectOpen: boolean;
  items: { reviewId: string; decision: unknown }[];
};
/** The choices a walk's request sends, each expecting its card open. */
const choices = (r: Request) => {
  const b = r.postDataJSON() as BatchBody;
  expect(b.expectOpen).toBe(true);
  expect(b.loadId).toMatch(/^load_/);
  return b.items.map((it) => ({ decision: it.decision }));
};
/** The one choice a walk's request sends. */
const choice = (r: Request) => {
  const [only, ...rest] = choices(r);
  expect(rest).toEqual([]);
  return only;
};
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
  // Another page's dock says the load left cards to decide, as many as the
  // review cards index counts, and opens them.
  await page.goto("/data");
  const dock = page.getByRole("region", { name: "Jobs in progress" });
  await expect(dock).toContainText("Data loaded", { timeout: 30_000 });
  await dock.getByRole("link", { name: "Review 4", exact: true }).click();
  await expect(page).toHaveURL(
    new RegExp(`${O}/review(\\?card=[0-9a-f]{64})?$`),
  );
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
  let sent = page.waitForRequest(isBatch);
  await page.keyboard.press("Enter");
  expect(choice(await sent)).toEqual(RELEASE);
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
  sent = page.waitForRequest(isBatch);
  await page.keyboard.press("x");
  expect(choice(await sent)).toEqual({
    decision: {
      reject: { statementHash: expect.stringMatching(/^[0-9a-f]{64}$/) },
    },
  });
  // A name the load linked by its legal form: a choice applies from the
  // next load.
  await expect(heading(page)).toContainText("names “Umbrella Holdings");
  await expect(card(page)).toContainText("Applies from the next load.");
  sent = page.waitForRequest(isBatch);
  await page.keyboard.press("n");
  expect(choice(await sent)).toEqual({
    decision: { resolve: { notInData: true } },
  });
  // The same name in the other document: Founder suggests the choice just
  // made, as a repeat of it.
  await expect(heading(page)).toContainText("names “Umbrella Holdings");
  await expect(card(page)).toContainText(
    "Founder suggests Not a Customer we haveRuleYour earlier choice ×1",
  );
  sent = page.waitForRequest(isBatch);
  await page.keyboard.press("Enter");
  expect(choice(await sent)).toEqual({
    decision: { resolve: { notInData: true } },
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
  // A workflow to be emailed when cards pile up again, on this ontology.
  await expect(
    done.getByRole("link", { name: "Email me when reviews wait" }),
  ).toHaveAttribute(
    "href",
    "/workflows/new?template=reviews-digest&ontology=ont_acme_contracts",
  );
  await done.getByRole("button", { name: "Go through 1 skipped" }).click();
  await expect(heading(page)).toContainText(first);
  // Every count is the index's: the card skipped, unless the finish drew it
  // as a spot check, which is then no card to decide.
  await page.goto(`${O}/review`);
  await expect(lane(page, "Decide")).toHaveText(/^Decide\s*[01]$/);
  const n = Number(
    /(\d+)$/.exec((await lane(page, "Decide").textContent())!)![1],
  );
  await page
    .getByRole("list", { name: "Pages" })
    .getByRole("link", { name: "Overview" })
    .click();
  await expect(page).toHaveURL(new RegExp(`${O}$`));
  if (n) {
    await expect(
      page.getByText("1 card to decide", { exact: true }),
    ).toBeVisible();
    await page.getByRole("link", { name: "Start reviewing" }).click();
    await expect(page).toHaveURL(
      new RegExp(`${O}/review(\\?card=[0-9a-f]{64})?$`),
    );
  } else
    await expect(
      page.getByRole("link", { name: "Start reviewing" }),
    ).toHaveCount(0);
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
  const sent = page.waitForRequest(isBatch);
  await page.keyboard.press("Enter");
  expect(choice(await sent)).toEqual({
    decision: { resolve: { entityKey: expect.any(String) } },
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
  // With nothing to decide, the panel's Review badge still leads with Facts'
  // count, so the audit's suggestions never read as cards.
  const review = page
    .getByRole("list", { name: "Pages" })
    .getByRole("link", { name: /^Review/ });
  await expect(async () => {
    await page.goto(`${O}/connections`);
    await expect(review.locator(".hf-nav-badge")).toHaveText("0 · 4", {
      timeout: 3000,
    });
  }).toPass({ timeout: 45_000 });
  await expect(review).toHaveAccessibleName("Review: 4 ontology suggestions");
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
  const sent = page.waitForRequest(isBatch);
  await page.keyboard.press("Enter");
  expect(choice(await sent)).toEqual({
    decision: { resolve: { entityKey: expect.any(String) } },
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

test("a link the load can't apply in place waits for the next load, and the finish offers its other documents again without moving the walk", async ({
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
  // The reads that follow (the settle's job ends, the cards and decisions
  // are read again) never move the walk back to a card on its own: the
  // finish stays, offering the two.
  await page.waitForTimeout(5000);
  await expect(done).toBeVisible();
  await expect(card(page)).toHaveCount(0);
  // Nor does coming back to Facts, which takes the cards read again.
  await page.getByRole("tab", { name: /^Ontology/ }).click();
  await expect(done).toHaveCount(0);
  await page.getByRole("tab", { name: /^Facts/ }).click();
  await expect(done).toBeVisible();
  await page.waitForTimeout(500);
  await expect(done).toBeVisible();
  await expect(card(page)).toHaveCount(0);
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

test("a walk that starts with a name's other documents covered keeps its finish when the link's job opens them again", async ({
  page,
}) => {
  // The mock runs a name's link without a job whose end reads the cards
  // again, so the walk reads them as the person comes back to the tab, once
  // they are stale (30 s).
  test.setTimeout(150_000);
  await mock.configure({ loadStepMs: 200, nameInPlace: "not_replayed" });
  await signIn(page);
  await uploadNaming(page, "Umbrella Holdings", 3);
  await makeContractsCurrent(page);
  await loadVersion3(page);
  // The link's job waits a long step, so a new walk reads the name's two
  // other documents covered, never in the walk's order.
  await mock.configure({ loadStepMs: 8000 });
  await page.goto(`${O}/review`);
  await expect(heading(page)).toContainText("names “Umbrella Holdings");
  const sent = page.waitForRequest(isBatch);
  await page.keyboard.press("Enter");
  await sent;
  await expect(
    toast(page, "Linked, with 2 other documents asking about this name."),
  ).toBeVisible();
  await page.goto("/data");
  await page.goto(`${O}/review`);
  await expect(lane(page, "Covered")).toHaveText(/^Covered\s*2$/);
  await expect(
    page.getByRole("heading", { level: 2, name: "Nothing to decide" }),
  ).toBeVisible();
  await expect(card(page)).toHaveCount(0);
  // The job ends refusing it in place: the next read of the cards lists
  // the two to decide, and the finish offers them without moving the walk
  // to one.
  const done = page.getByRole("region", { name: "2 cards still to decide" });
  await expect(async () => {
    await page.evaluate(() =>
      window.dispatchEvent(new Event("visibilitychange")),
    );
    await expect(lane(page, "Covered")).toHaveText(/^Covered\s*0$/, {
      timeout: 2000,
    });
  }).toPass({ timeout: 75_000, intervals: [3000] });
  await expect(done).toBeVisible();
  await expect(card(page)).toHaveCount(0);
  await page.waitForTimeout(3000);
  await expect(done).toBeVisible();
  await expect(card(page)).toHaveCount(0);
  await done
    .getByRole("button", { name: "Go to the first still to decide" })
    .click();
  await expect(heading(page)).toContainText("names “Umbrella Holdings");
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
    if (isBatch(r)) settles.push(r);
    if (isRemove(r)) removes.push(r);
  });
  await dialog.getByRole("button", { name: "Approve 3" }).click();
  await expect(dialog.getByRole("status")).toHaveText("Saved 3 choices.");
  // One request for the batch, three choices in it.
  expect(settles).toHaveLength(1);
  expect(settles.flatMap(choices)).toEqual([RELEASE, RELEASE, RELEASE]);
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
  const sent = page.waitForResponse((r) => isBatch(r.request()));
  await page.keyboard.press("1");
  expect((await (await sent).json()).results[0].refused).toMatchObject({
    reason: "FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_STALE",
  });
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
  const again = page.waitForResponse((r) => isBatch(r.request()));
  await page.keyboard.press("1");
  expect((await (await again).json()).results[0].decision).toBeTruthy();
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
  const saved = page.waitForResponse((r) => isBatch(r.request()));
  await page.keyboard.press("Enter");
  expect((await (await saved).json()).results[0].decision).toBeTruthy();
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
    if (isBatch(r)) settles.push(r);
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
  await expect(
    done.getByRole("link", { name: "Email me when reviews wait" }),
  ).toHaveCount(0);
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

test("F asks Founder: the dock opens composed, and nothing is sent; keys typed there, in a dialog or on a button are theirs", async ({
  page,
}) => {
  await walk(page, [file("variant.md", HEDGED)]);
  const { first } = await claims(page);
  // The card shown is in the URL, for Founder's page context.
  await expect(page).toHaveURL(/[?&]card=[0-9a-f]{64}/);
  const posts: string[] = [];
  page.on("request", (r) => {
    if (r.method() === "POST") posts.push(r.url());
  });
  await page.keyboard.press("f");
  const composer = page
    .getByRole("complementary", { name: "Founder" })
    .getByRole("textbox", { name: "Message" });
  await expect(composer).toBeVisible();
  // No rule suggests a choice here (nothing measures the score yet): only
  // the ask; the page context names the card.
  await expect(composer).toHaveValue("Help me decide this review.");
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
  expect(posts.filter((u) => /:batchSettle$/.test(u))).toHaveLength(1);
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

/** The ontology's saved releases, as the walk's person reads them. */
async function releases(page: Page) {
  const r = await page.request.get(
    "/papi/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts/review-decisions",
  );
  return (
    (await r.json()).decisions as {
      id: string;
      kind: string;
      decidedByUserId: string;
    }[]
  ).filter((d) => d.kind === "release");
}

test("quick choices share requests: one in flight, the next carrying the rest", async ({
  page,
}) => {
  await walk(
    page,
    [file("variant.md", HEDGED), file("second.md", SECOND)],
    MEASURED(40),
  );
  await claims(page);
  // Each request takes a while, so the choices made meanwhile wait for it.
  const batches: Request[] = [];
  await page.route("**/papi/**/reviews:batchSettle", async (route: Route) => {
    batches.push(route.request());
    await new Promise((r) => setTimeout(r, 600));
    await route.continue();
  });
  for (let i = 0; i < 4; i += 1) await page.keyboard.press("Enter");
  await expect(progress(page)).toHaveText(
    /^4 of \d+ · 4 accepted · 0 changed · 0 skipped$/,
  );
  await expect(async () => {
    expect(batches.flatMap(choices)).toHaveLength(4);
  }).toPass({ timeout: 10_000 });
  expect(batches.length).toBeLessThanOrEqual(2);
  await expect(async () =>
    expect(await releases(page)).toHaveLength(4),
  ).toPass();
});

test("a retried choice after a dropped answer keeps one decision, the person's", async ({
  page,
}) => {
  await walk(page, [file("variant.md", HEDGED)], MEASURED(40));
  const { first, other } = await claims(page);
  // The first request reaches the backend, which saves the choice, but its
  // answer is lost on the way back.
  let dropped = false;
  await page.route("**/papi/**/reviews:batchSettle", async (route: Route) => {
    if (dropped) return route.continue();
    dropped = true;
    await route.fetch();
    await route.abort("failed");
  });
  await page.keyboard.press("Enter");
  await expect(heading(page)).toContainText(other);
  await page.keyboard.press("s");
  await expect(heading(page)).toContainText(first);
  await expect(card(page).getByRole("alert")).toContainText("Couldn’t save it");
  // Choosing again settles it again: the person's own decision, never a
  // second one, and the walk shows it saved.
  const again = page.waitForResponse((r) => isBatch(r.request()));
  await page.keyboard.press("Enter");
  const result = (await (await again).json()).results[0];
  expect(result.decision.decidedByUserId).toBe("usr_ada");
  await expect(progress(page)).toHaveText(
    "2 of 4 · 1 accepted · 0 changed · 1 skipped",
  );
  const saved = await releases(page);
  expect(saved).toHaveLength(1);
  expect(saved[0]!.id).toBe(result.decision.id);
});

test("two admins on one card: the second is told who decided it, and their Undo removes nothing, not even their own choice before it", async ({
  page,
  browser,
}) => {
  await walk(page, [file("variant.md", HEDGED)], {
    ...MEASURED(40),
    alanRole: "WORKSPACE_ROLE_ADMIN",
  });
  const { first, other } = await claims(page);
  // Alan opens the walk too: everyone walks one order, so his first card
  // is Ada's.
  const alanContext = await browser.newContext();
  const alan = await alanContext.newPage();
  await signIn(alan, "alan@example.com");
  await alan.goto(`${O}/review`);
  await expect(heading(alan)).toContainText(first);
  // Alan approves the first card; Ada skips it and approves the next.
  const his = alan.waitForResponse((r) => isBatch(r.request()));
  await alan.keyboard.press("Enter");
  const mine = (await (await his).json()).results[0].decision;
  expect(mine.decidedByUserId).toBe("usr_alan");
  await expect(heading(alan)).toContainText(other);
  await page.keyboard.press("s");
  await expect(heading(page)).toContainText(other);
  const sent = page.waitForResponse((r) => isBatch(r.request()));
  await page.keyboard.press("Enter");
  const ada = (await (await sent).json()).results[0].decision;
  expect(ada.decidedByUserId).toBe("usr_ada");
  // Alan's approval of it is refused, naming her; he presses Z while it is
  // in flight. His choice is hers, so Undo has nothing to take back: his
  // approval of the first card stays, and nothing is removed.
  const removes: Request[] = [];
  alan.on("request", (r) => {
    if (isRemove(r)) removes.push(r);
  });
  await alan.route("**/papi/**/reviews:batchSettle", async (route: Route) => {
    await new Promise((r) => setTimeout(r, 600));
    await route.continue();
  });
  const refused = alan.waitForResponse((r) => isBatch(r.request()));
  await alan.keyboard.press("Enter");
  await expect(toast(alan, "Approved")).toBeVisible();
  await alan.keyboard.press("z");
  expect((await (await refused).json()).results[0].refused).toMatchObject({
    reason: "FOUNDRY_REVIEW_SETTLE_REFUSAL_REASON_DECIDED",
    decided: { id: ada.id },
  });
  await expect(
    toast(alan, "Already decided by Ada Lovelace: their decision stands."),
  ).toBeVisible();
  await alan.keyboard.press("ArrowLeft");
  await expect(heading(alan)).toContainText(other);
  await expect(card(alan).locator(".rv-outcome")).toHaveText(
    "Decided by Ada Lovelace. Their choice stands; yours wasn’t saved.",
  );
  await expect(progress(alan)).toContainText("1 decided elsewhere");
  // A second Z takes back his own approval, the choice before it.
  await alan.keyboard.press("z");
  await expect(heading(alan)).toContainText(first);
  await expect.poll(() => removes.length).toBe(1);
  expect(removes[0]!.url()).toContain(mine.id);
  // The decisions read again since, Ada's card still reads hers.
  await alan
    .getByRole("navigation", { name: "Next cards" })
    .getByRole("button", { name: new RegExp(`^${other}`) })
    .click();
  await expect(card(alan).locator(".rv-outcome")).toContainText(
    "Decided by Ada Lovelace.",
  );
  await alanContext.close();
  const saved = await releases(page);
  expect(saved.map((d) => [d.id, d.decidedByUserId])).toEqual([
    [ada.id, "usr_ada"],
  ]);
});

test("F asks Founder about the card: the message carries it, and the reply recommends one choice", async ({
  page,
}) => {
  await walk(page, [file("variant.md", HEDGED)], {
    ...MEASURED(40),
    agentStepMs: 100,
  });
  await claims(page);
  const cardId = new URL(page.url()).searchParams.get("card");
  expect(cardId).toMatch(/^[0-9a-f]{64}$/);
  await page.keyboard.press("f");
  const dock = page.getByRole("complementary", { name: "Founder" });
  const composer = dock.getByRole("textbox", { name: "Message" });
  // The rule's suggestion, in words, then the ask: composed, not sent.
  await expect(composer).toHaveValue(
    "The rule suggests approving it. Help me decide this review.",
  );
  await expect(dock).toContainText("this review");
  const turn = page.waitForRequest(
    (r) => r.method() === "POST" && r.url().endsWith("/foundry/agent/turns"),
  );
  await dock.getByRole("button", { name: "Send", exact: true }).click();
  expect((await turn).postDataJSON().pageContext).toMatchObject({
    ontologyId: "ont_acme_contracts",
    reviewCardId: cardId,
  });
  await expect(
    dock.getByText(/^I recommend approving it\./).first(),
  ).toBeVisible({ timeout: 15_000 });
  // Founder prepares nothing unasked.
  await expect(dock.getByRole("region", { name: /^Action:/ })).toHaveCount(0);
});

test("a settle Founder runs from the dock is the walk's choice: it moves on, and Undo takes it back", async ({
  page,
}) => {
  await mock.configure({ agentStepMs: 100, loadStepMs: 200 });
  await signIn(page);
  await makeContractsCurrent(page);
  await uploadInData(page, [
    file(
      "variant.md",
      "# Variant contract\n\nGlobexx Corporation signed. The Agreement runs for 24 months.\n",
    ),
  ]);
  await loadVersion3(page);
  await page.goto(`${O}/review`);
  await expect(heading(page)).toBeVisible();
  await expect(progress(page)).toHaveText(/^0 of \d+ · 0 accepted · 0 changed/);
  await page.keyboard.press("ControlOrMeta+j");
  const dock = page.getByRole("complementary", { name: "Founder" });
  await dock
    .getByRole("textbox", { name: "Message" })
    .fill("Settle the review about Globexx in Customer contracts");
  await dock.getByRole("button", { name: "Send", exact: true }).click();
  const action = dock.getByRole("region", {
    name: "Action: Link “globexx corporation” to Globex Corporation",
  });
  await action.getByRole("button", { name: "Link", exact: true }).click();
  await expect(action.getByText("Ran", { exact: true })).toBeVisible({
    timeout: 15_000,
  });
  // The walk took it as the person's own choice.
  await expect(progress(page)).toHaveText(
    /^\d+ of \d+ · 0 accepted · 1 changed/,
  );
  await expect(toast(page, "Founder’s choice saved.")).toBeVisible();
  const removed = page.waitForRequest(isRemove);
  await toast(page, "Founder’s choice saved.")
    .getByRole("button", { name: /^Undo/ })
    .click();
  await removed;
  // A name's link the load applied in place: taken back, kept by this load.
  await expect(
    toast(page, "Choice removed: it stops applying from the next load."),
  ).toBeVisible();
  await expect(progress(page)).toHaveText(
    /· 0 accepted · 0 changed · 0 skipped · 1 taken back$/,
  );
});

test("a reload on ?card= lands on that card", async ({ page }) => {
  await walk(page, [file("variant.md", HEDGED)]);
  const { other } = await claims(page);
  await page.keyboard.press("s");
  await expect(heading(page)).toContainText(other);
  const shown = await heading(page).textContent();
  await expect(page).toHaveURL(/[?&]card=[0-9a-f]{64}/);
  await page.reload();
  await expect(heading(page)).toHaveText(shown!);
});

/**
 * Loads `forms` order forms naming “Globex Initech Holdings”, which two
 * stored customers share a word with, so the rules can't pick: Founder's
 * model may. Opens the walk on the first such card.
 */
async function tiedName(
  page: Page,
  config: Parameters<typeof mock.configure>[0] = {},
  forms = 1,
) {
  await mock.configure({ loadStepMs: 200, ...config });
  await signIn(page);
  await uploadNaming(page, "Globex Initech Holdings", forms);
  await makeContractsCurrent(page);
  await loadVersion3(page);
  await page.goto(`${O}/review`);
  await expect(heading(page)).toBeVisible();
  for (let i = 0; i < 12; i++) {
    if ((await heading(page).textContent())?.includes("names “Globex Initech"))
      break;
    await page.keyboard.press("s");
  }
  await expect(heading(page)).toContainText("names “Globex Initech");
}

test("Founder's model suggests a choice where the rules can't: its source, its quote, Enter takes it, and G batches nothing", async ({
  page,
}) => {
  // Two forms: two cards with the same advice, which a rule's suggestion
  // would batch.
  await tiedName(page, {}, 2);
  const box = card(page).getByRole("region", { name: "Founder’s suggestion" });
  await expect(box).toContainText("Founder suggests");
  await expect(box).toContainText("Founder’s model");
  await expect(box).toContainText("Not measured");
  const why = box.getByRole("list", { name: "Why" });
  await expect(why).toContainText(
    /^Founder’s model: .+ shares the most words with the name the document uses\. Section \d+ of naming-\d\.md$/,
  );
  await expect(
    why.getByRole("link", { name: /^Section \d+ of naming-\d\.md$/ }),
  ).toHaveAttribute("href", /\/documents\/[^?#]+\?tab=review#s\d+-\d+-\d+$/);
  const first = await why.textContent();
  // Never batched: no Accept all like this, and G opens nothing, on either
  // card.
  for (const shown of ["first", "second"]) {
    await expect(box, shown).toContainText("Founder’s model");
    await expect(
      page.getByRole("button", { name: /Accept all like this/ }),
    ).toHaveCount(0);
    await page.keyboard.press("g");
    await expect(page.getByRole("dialog")).toHaveCount(0);
    if (shown === "second") break;
    for (let i = 0; i < 12; i++) {
      const at = page.url();
      await page.keyboard.press("s");
      await expect(page).not.toHaveURL(at);
      if (
        (await heading(page).textContent())?.includes("names “Globex Initech")
      )
        break;
    }
    await expect(heading(page)).toContainText("names “Globex Initech");
    await expect(why).not.toHaveText(first!);
  }
  const sent = page.waitForRequest(isBatch);
  await page.keyboard.press("Enter");
  expect(choice(await sent)).toEqual({
    decision: { resolve: { entityKey: expect.any(String) } },
  });
});

test("the model's rationale shows as plain text, whatever it holds", async ({
  page,
}) => {
  await tiedName(page, {
    reviewModelAdviceRationale:
      "See <b>https://evil.test</b>\u202e now,\n<a href='x'>click</a>",
  });
  const why = card(page).getByRole("list", { name: "Why" });
  await expect(why).toContainText(
    "Founder’s model: See <b>https://evil.test</b> now, <a href='x'>click</a>",
  );
  await expect(why.locator("b")).toHaveCount(0);
  await expect(why.getByRole("link")).toHaveText([
    /^Section \d+ of naming-1\.md$/,
  ]);
});

test("with the model's advice off, a name between candidates needs you", async ({
  page,
}) => {
  await tiedName(page, { reviewModelAdvice: false });
  await expect(card(page)).toContainText(
    "Needs you: more than one record fits this name.",
  );
  await expect(card(page)).not.toContainText("Founder’s model");
});
