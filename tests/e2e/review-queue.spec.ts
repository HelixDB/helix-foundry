import { createHash } from "node:crypto";
import type { Page, Request } from "@playwright/test";
import {
  choose,
  expect,
  file,
  mock,
  signIn,
  test,
  uploadAndLoadContract,
} from "./support/mock";

// The review queue (/ontology/:id/review): one list of the current load's
// open reviews, the decisions writing the most facts first, about one card
// in ten drawn at random from a score band, each card's evidence before
// its score, settled by the paths a document's Needs review uses, each
// decision saying how its card was drawn. No Approve all.

const O = "/ontology/ont_acme_contracts";
// Two held claims, a value in conflict, a name in no row that blocks a
// claim, and relationships the ontology lacks; the contract adds accepted
// facts.
const HEDGED =
  "# Hedged deal\n\nUmbrella Holdings was copied. Globex Corporation and Initech may have signed. Globex Corporation supplies Initech. Initech competes with Globex Corporation.\n\n## Billing\n\nGlobex Corporation is billed in GBP.\n";

async function queued(page: Page) {
  await mock.configure({ loadStepMs: 200, documentUnmatched: true });
  await signIn(page);
  await uploadAndLoadContract(page, [file("variant.md", HEDGED)]);
  await page.goto(`${O}/review`);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText(
    "Review queue",
  );
}
const queue = (page: Page) => page.getByRole("list", { name: "Review queue" });
const cards = (page: Page) => queue(page).locator(":scope > li");
const card = (page: Page, text: string | RegExp) =>
  cards(page).filter({ hasText: text });
const settles = (page: Page) =>
  page.waitForRequest(
    (r) => r.method() === "POST" && /\/reviews\/[^/]+:settle$/.test(r.url()),
  );
const body = (r: Request) => r.postDataJSON() as Record<string, unknown>;

test("ranks one queue for the ontology: what writes facts first, evidence before the score, a random check, no Approve all", async ({
  page,
}) => {
  await queued(page);
  await expect(cards(page).first()).toBeVisible();
  // It's one of the ontology's pages.
  await expect(page.locator('.hf-subnav-link[aria-current="page"]')).toHaveText(
    "Review queue",
  );
  await expect(page.getByRole("button", { name: /Approve all/ })).toHaveCount(
    0,
  );
  // Each held claim: its quote and document, then what it writes, then the
  // verifier's score.
  const held = card(page, "may have signed");
  await expect(held.first()).toContainText(
    /may have signed[\s\S]*variant\.md · section \d+[\s\S]*Verifier score 0\.72 \(0\.7 to 0\.8\)/,
  );
  // The name in no row writes the claim it holds back, from the next load.
  const umbrella = card(page, "A name in a document").filter({
    hasText: "Umbrella Holdings",
  });
  await expect(umbrella.first()).toContainText(
    "Settling it writes 1 fact the load holds back for it.",
  );
  await expect(umbrella.first()).toContainText(
    "A name’s decision applies from the next load",
  );
  // Held claims (a fact each, at once) before names, before values.
  const titles = await cards(page).locator("h3").allTextContents();
  const first = (title: string) => titles.indexOf(title);
  expect(first("A held claim")).toBeGreaterThanOrEqual(0);
  expect(first("A held claim")).toBeLessThan(first("A name in a document"));
  expect(first("A name in a document")).toBeLessThan(
    first("A value documents state differently"),
  );
  // About one card in ten is drawn at random, said on the card.
  const total = await cards(page).count();
  const random = await card(page, "Random check").count();
  expect(random).toBeGreaterThanOrEqual(1);
  expect(random).toBeLessThanOrEqual(Math.ceil(total / 9));
  // What isn't in the queue, and why.
  await expect(
    page.getByText(/Not in the queue: .*no decision settles here/),
  ).toBeVisible();
});

test("settles from the queue by the paths a document's page uses, each decision saying how its card was drawn", async ({
  page,
}) => {
  await queued(page);
  const held = card(page, "may have signed").filter({
    hasNotText: "Random check",
  });
  await expect(held.first()).toBeVisible();
  // Approve: a release bound to what was read, ranked.
  let sent = settles(page);
  await held
    .first()
    .getByRole("button", { name: "Approve", exact: true })
    .click();
  expect(body(await sent)).toEqual({
    decision: {
      release: { statementHash: expect.stringMatching(/^[0-9a-f]{64}$/) },
    },
    drawn: "ranked",
  });
  // The card stays, saying what became of it.
  await expect(held.first()).toContainText(/Approved/);
  // Reject another.
  const other = card(page, "may have signed").filter({
    hasNot: page.getByText(/Approved/),
  });
  if (await other.count()) {
    const drawn = (await other.first().getByText("Random check").count())
      ? "random"
      : "ranked";
    sent = settles(page);
    await other.first().getByRole("button", { name: "Reject" }).click();
    expect(body(await sent)).toEqual({
      decision: {
        reject: { statementHash: expect.stringMatching(/^[0-9a-f]{64}$/) },
      },
      drawn,
    });
  }
  // A name: not in the data, from the next load. Each document's mention is
  // a decision of its own.
  const umbrellas = card(page, "A name in a document").filter({
    hasText: "Umbrella Holdings",
  });
  const names = await umbrellas.count();
  const umbrella = umbrellas.first();
  await choose(umbrella.getByLabel("Link this name to"), "Not in data");
  sent = settles(page);
  await umbrella.getByRole("button", { name: "Settle review" }).click();
  expect(body(await sent)).toEqual({
    decision: { resolve: { notInData: true } },
    drawn: "ranked",
  });
  await expect(umbrella).toContainText("Decision saved. Reload to apply.");
  // A value.
  const value = card(page, "A value documents state differently").first();
  await choose(
    value.getByLabel(/Keep value|Replace the table’s value with/),
    "GBP",
  );
  sent = settles(page);
  await value.getByRole("button", { name: "Settle review" }).click();
  expect(body(await sent)).toMatchObject({
    decision: { prop: { text: "GBP" } },
    drawn: "ranked",
  });
  // Read again, the decided cards have left the queue.
  const before = await cards(page).count();
  await page.reload();
  await expect(cards(page).first()).toBeVisible();
  await expect.poll(async () => cards(page).count()).toBeLessThan(before);
  await expect(umbrellas).toHaveCount(names - 1);
  await expect(page.getByText(/Not in the queue: \d+ decided/)).toBeVisible();
});

test("labels an accepted fact drawn at random, which changes nothing and counts in the random checks", async ({
  page,
}) => {
  await queued(page);
  const fact = card(page, "An accepted fact").first();
  await expect(fact).toContainText("The graph holds it as a fact:");
  await expect(fact).toContainText(/Verifier score 0\.9\d/);
  const sent = page.waitForRequest(
    (r) => r.method() === "POST" && r.url().endsWith("/review-labels"),
  );
  await fact.getByRole("button", { name: "Right" }).click();
  expect(body(await sent)).toEqual({
    fact: {
      docId: expect.any(String),
      section: expect.any(Number),
      start: expect.any(Number),
      end: expect.any(Number),
      factKey: expect.any(String),
    },
    verdict: "right",
    drawn: "random",
  });
  await expect(fact).toContainText("You said it’s right.");
  const checks = page.getByRole("table");
  await expect(
    checks.getByRole("row", { name: /0\.9 and above/ }),
  ).toContainText(/0\.9 and above\s*1\s*1/);
});

test("members read the queue but settle nothing", async ({ page }) => {
  await queued(page);
  await page.context().clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto(`${O}/review`);
  await expect(cards(page).first()).toBeVisible();
  await expect(
    page.getByText("Owners and admins settle reviews."),
  ).toBeVisible();
  for (const name of ["Approve", "Reject", "Settle review", "Right", "Wrong"])
    await expect(
      queue(page).getByRole("button", { name, exact: true }),
    ).toHaveCount(0);
});

// An id the queue offers a second person (reviews/queue.ts rechecks: one in
// twenty, by a hash of the id).
function sampledId() {
  for (let i = 0; ; i++) {
    const id = `decision_e2e${i}`;
    const h = createHash("sha256").update(`recheck\u001f${id}`).digest();
    if (h.readUInt32BE(0) / 2 ** 32 < 0.05) return id;
  }
}

test("offers a second person a re-check of another's queue decision, with what the claim states, which changes nothing", async ({
  page,
}) => {
  await queued(page);
  // Ada rejects a held claim from the queue; the decisions read names it
  // as another person's, drawn for a re-check.
  const held = card(page, "may have signed")
    .filter({ hasNotText: "Random check" })
    .first();
  const saved = page.waitForResponse(
    (r) => r.request().method() === "POST" && /:settle$/.test(r.url()),
  );
  await held.getByRole("button", { name: "Reject" }).click();
  const decision = (await (await saved).json()).decision;
  await expect(held).toContainText("Rejected: nothing is written");
  await expect(held.getByRole("button", { name: "Approve" })).toHaveCount(0);
  const id = sampledId();
  await page.route(/\/review-decisions(\?.*)?$/, async (route) => {
    const response = await route.fetch();
    const json = await response.json();
    json.decisions = (json.decisions ?? []).map((d: { id: string }) =>
      d.id === decision.id
        ? { ...d, id, decidedByUserId: "usr_alan", drawn: "ranked" }
        : d,
    );
    await route.fulfill({ response, json });
  });
  await page.route(/\/review-labels$/, async (route) => {
    if (route.request().method() !== "POST") return route.continue();
    await route.fulfill({
      json: {
        label: {
          id: "label_1",
          drawn: "recheck",
          docId: decision.docId,
          verdict: "wrong",
          against: "wrong",
          decisionId: id,
        },
      },
    });
  });
  await page.reload();
  const rechecks = page.getByRole("list", { name: "Re-checks" });
  await expect(rechecks).toContainText("may have signed");
  await expect(rechecks).toContainText(
    /It states .+ → signed_with → .+\. Someone rejected it\. Is it right\?/,
  );
  const sent = page.waitForRequest(
    (r) => r.method() === "POST" && r.url().endsWith("/review-labels"),
  );
  await rechecks.getByRole("button", { name: "Wrong" }).click();
  expect(body(await sent)).toEqual({
    decisionId: id,
    verdict: "wrong",
    drawn: "recheck",
  });
  await expect(rechecks).toContainText("You said it’s wrong, as they did.");
});
