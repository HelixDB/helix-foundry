import type { Page } from "@playwright/test";
import type { ListReviewDecisionsResponse } from "../../src/shared/wfe";
import {
  APP_ORIGIN,
  choose,
  csrfToken,
  expect,
  file,
  loadVersion3,
  makeContractsCurrent,
  mock,
  signIn,
  test,
  uploadInData,
} from "./support/mock";

const O = "/ontology/ont_acme_contracts",
  X = "/explore/ont_acme_contracts";
const VARIANT =
  "# Variant contract\n\nGlobexx Corporation signed. The Agreement runs for 24 months.\n";
const entity = (type: string, identity: string) =>
  `${X}/entities/${Buffer.from(`${type}\0${identity}`).toString("base64url")}`;
async function setup(
  page: Page,
  files = [{ name: "variant.md", text: VARIANT }],
) {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await makeContractsCurrent(page);
  for (const f of files) await uploadInData(page, [file(f.name, f.text)]);
  return loadVersion3(page);
}
async function openDocument(page: Page) {
  await page.goto(`${O}/documents`);
  await page
    .getByRole("grid", { name: "Documents" })
    .getByRole("link", { name: "variant.md", exact: true })
    .click();
  return new URL(page.url()).pathname;
}
const reviews = (page: Page) =>
  page.getByRole("list", { name: "Needs review" });
/** Settles the open document's review as Not in data, from its Needs review tab. */
async function settleNotInData(page: Page) {
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await choose(page.getByLabel("Link this name to"), "Not in data");
  await page.getByRole("button", { name: "Settle review" }).click();
  await expect(
    page.getByText("Decision saved. Reload to apply."),
  ).toBeVisible();
}

test("settles a suggested candidate, reloads linked, and removes the saved decision", async ({
  page,
}) => {
  const old = await setup(page);
  const doc = await openDocument(page);
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await expect(reviews(page)).toContainText("Suggestions only:");
  // Candidates read by name, each linked to its page, never by stored key.
  await expect(reviews(page)).not.toContainText("cus_GLOBEX01");
  await expect(
    reviews(page).getByRole("link", { name: "Globex Corporation" }),
  ).toHaveAttribute("href", new RegExp(entity("Customer", "k:cus_GLOBEX01")));
  await choose(page.getByLabel("Link this name to"), "Globex Corporation");
  // Failed writes stay visible and can be retried; nothing is marked saved.
  await page.route("**/papi/**/reviews/*:settle", (route) =>
    route.fulfill({
      status: 503,
      contentType: "application/json",
      body: JSON.stringify({ code: 14, message: "Settlement unavailable" }),
    }),
  );
  await page.getByRole("button", { name: "Settle review" }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "Settlement unavailable" }),
  ).toContainText("Settlement unavailable");
  await expect(page.getByText("Decision saved. Reload to apply.")).toHaveCount(
    0,
  );
  await page.unroute("**/papi/**/reviews/*:settle");
  const sent = page.waitForRequest(
    (r) => r.method() === "POST" && /\/reviews\/[^/]+:settle$/.test(r.url()),
  );
  await page.getByRole("button", { name: "Settle review" }).click();
  expect((await sent).postDataJSON()).toEqual({
    decision: { resolve: { entityKey: "cus_GLOBEX01" } },
  });
  await expect(
    page.getByText("Decision saved. Reload to apply."),
  ).toBeVisible();
  await expect(reviews(page)).toContainText(
    "Names something that isn’t in your data",
  );
  await loadVersion3(page);
  await page.goto(doc);
  await page.getByRole("tab", { name: /^Entities/ }).click();
  await expect(page.getByText("Linked by a person")).toBeVisible();
  await expect(
    page.getByRole("link", { name: "Globex Corporation", exact: true }),
  ).toBeVisible();
  await page.getByRole("tab", { name: /^Facts/ }).click();
  await expect(page.getByRole("list", { name: "Facts" })).toContainText(
    "signed_with",
  );
  // Historic reviews are readable but cannot settle a different load, and
  // never say to reload: the current load already read the decision.
  await page.goto(`${doc}?load=${old}&tab=review`);
  await expect(
    page.getByText("Open the current load to settle this review."),
  ).toBeVisible();
  await expect(
    page.getByText("A decision is saved for this review."),
  ).toBeVisible();
  await expect(page.getByText("Decision saved. Reload to apply.")).toHaveCount(
    0,
  );
  await expect(page.getByRole("button", { name: "Settle review" })).toHaveCount(
    0,
  );
  await page.goto(`${doc}?tab=review`);
  await page.getByRole("button", { name: "Remove decision" }).click();
  await expect(
    page.getByText("Decision removed. Reload to apply."),
  ).toBeVisible();
  await loadVersion3(page);
  await page.goto(`${doc}?tab=review`);
  await expect(page.getByLabel("Link this name to")).toBeVisible();
});

test("settles not-in-data without creating an entity or link", async ({
  page,
}) => {
  await setup(page);
  const doc = await openDocument(page);
  await settleNotInData(page);
  await loadVersion3(page);
  await page.goto(`${doc}?tab=review`);
  await expect(page.getByText("Nothing needs review.")).toBeVisible();
  await expect(
    page.getByRole("list", { name: "Saved decisions" }),
  ).toContainText("Not in data");
  await page.getByRole("tab", { name: /^Entities/ }).click();
  await expect(
    page.getByRole("region", { name: "Customer", exact: true }),
  ).toHaveCount(0);
});

test("picks a property conflict value and keeps its quotes after reload", async ({
  page,
}) => {
  await setup(page, [
    {
      name: "original.md",
      text: "# Shared contract\n\nGlobex Corporation signed. The Agreement runs for 24 months.\n",
    },
    {
      name: "amendment.md",
      text: "# Shared contract\n\nGlobex Corporation signed. The Agreement runs for 36 months.\n",
    },
  ]);
  const at = entity("Contract", "n:shared contract");
  await page.goto(at);
  await expect(page.locator(".ex-props")).toContainText("No value");
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await expect(
    page.getByRole("list", { name: "Conflicting values" }),
  ).toContainText("24 months");
  await expect(
    page.getByRole("list", { name: "Conflicting values" }),
  ).toContainText("36 months");
  await page
    .getByRole("list", { name: "Conflicting values" })
    .getByRole("link")
    .filter({ hasText: "24 months" })
    .click();
  await expect(
    page.getByLabel("Document text").locator("mark.is-span"),
  ).toContainText("24 months");
  await page.goto(at);
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await choose(page.getByLabel("Keep value"), "36");
  const sent = page.waitForRequest(
    (r) => r.method() === "POST" && /:settle$/.test(r.url()),
  );
  await page.getByRole("button", { name: "Settle review" }).click();
  expect((await sent).postDataJSON()).toEqual({
    decision: { prop: { integer: "36" } },
  });
  await expect(
    page.getByText("Decision saved. Reload to apply."),
  ).toBeVisible();
  await page.getByRole("tab", { name: /^Overview/ }).click();
  await expect(page.locator(".ex-props")).toContainText("No value");
  await loadVersion3(page);
  await page.goto(at);
  await expect(page.locator(".ex-props")).toContainText("36");
  await expect(page.locator(".ex-props")).toContainText("24");
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await expect(page.getByText("Nothing needs review")).toBeVisible();
  await expect(
    page.getByRole("list", { name: "Saved decisions" }),
  ).toContainText("term_months: 36");
});

test("approves a held claim, and the next load writes it as a fact", async ({
  page,
}) => {
  const old = await setup(page, [
    {
      name: "variant.md",
      text: "# Hedged deal\n\nGlobex Corporation may have signed.\n",
    },
  ]);
  const doc = await openDocument(page);
  await page.goto(`${doc}?tab=review`);
  const held = reviews(page).getByRole("listitem").filter({
    hasText: "The model wasn’t sure",
  });
  await expect(held).toContainText("Globex Corporation may have signed");
  // The card says what gets written, ends in order, before anyone approves.
  await expect(held).toContainText(
    "Approving writes: Hedged deal (Contract) → signed_with → Globex Corporation (Customer)",
  );
  await expect(held).toContainText("from this sentence alone");
  const settled = page.waitForRequest(
    (r) => r.method() === "POST" && /\/reviews\/[^/]+:settle$/.test(r.url()),
  );
  await held.getByRole("button", { name: "Approve", exact: true }).click();
  expect((await settled).postDataJSON()).toEqual({
    decision: {
      release: { statementHash: expect.stringMatching(/^[0-9a-f]{64}$/) },
    },
  });
  await expect(held.getByRole("status")).toContainText(
    "Decision saved. Reload to apply.",
  );
  await expect(
    held.getByRole("button", { name: "Approve", exact: true }),
  ).toHaveCount(0);
  // An approval of what the claim said before doesn't stand: the card asks again.
  const decisions = /\/review-decisions(\?|$)/;
  await page.route(decisions, async (route) => {
    const response = await route.fetch();
    const body = (await response.json()) as ListReviewDecisionsResponse;
    await route.fulfill({
      response,
      json: {
        ...body,
        decisions: body.decisions?.map((d) => ({
          ...d,
          statementHash: "0".repeat(64),
        })),
      },
    });
  });
  await page.reload();
  await expect(held.getByRole("status")).toContainText(
    "reads differently from what was approved",
  );
  await page.unroute(decisions);
  await held.getByRole("button", { name: "Approve", exact: true }).click();
  await expect(held.getByRole("status")).toContainText(
    "Decision saved. Reload to apply.",
  );
  // The next load writes it, and has no review of it.
  const next = await loadVersion3(page);
  expect(next).not.toBe(old);
  await page.goto(`${doc}?tab=review`);
  await expect(page.getByText("Nothing needs review.")).toBeVisible();
  await expect(
    page.getByRole("list", { name: "Saved decisions" }),
  ).toContainText("Approved: “Globex Corporation may have signed.”");
});

/** Serves the contracts ontology with signed_with holding one value. */
async function oneValued(page: Page) {
  await page.route(/\/versions\/[^/?]+(\?|$)/, async (route) => {
    const response = await route.fetch();
    const text = (await response.text()).replace(
      /(\[links\.signed_with\].*?cardinality = \\")many/g,
      "$1one",
    );
    await route.fulfill({ response, body: text });
  });
}

/** A document with one hedged claim, opened on its reviews, and its card. */
async function hedged(
  page: Page,
  text = "Globex Corporation may have signed.",
) {
  await setup(page, [
    { name: "variant.md", text: `# Hedged deal\n\n${text}\n` },
  ]);
  const doc = await openDocument(page);
  await page.goto(`${doc}?tab=review`);
  const held = reviews(page)
    .getByRole("listitem")
    .filter({
      hasText: /The model wasn’t sure|couldn’t confirm/,
    });
  return { doc, held };
}
const approve = async (card: ReturnType<Page["locator"]>) => {
  await card.getByRole("button", { name: "Approve", exact: true }).click();
  await expect(card.getByRole("status")).toContainText("Decision saved");
};

test("says why a load kept an approved claim held", async ({ page }) => {
  await mock.configure({ documentHeld: "refused" });
  const { doc, held } = await hedged(page);
  await approve(held);
  await loadVersion3(page);
  await page.goto(`${doc}?tab=review`);
  await expect(held.getByRole("status")).toContainText(
    "This load read the approval, and the claim stays held",
  );
  await expect(
    page.getByRole("list", { name: "Saved decisions" }),
  ).toContainText("Approved, not written: “");
});

test("names the name a legal form linked when it holds a one-valued claim", async ({
  page,
}) => {
  await oneValued(page);
  await mock.configure({ documentHeld: "legal_form" });
  const { doc, held } = await hedged(page);
  await expect(held).toContainText("Settle that name too");
  await approve(held);
  await loadVersion3(page);
  await page.goto(`${doc}?tab=review`);
  await expect(held.getByRole("status")).toContainText(
    "“globex corporation” is linked only by its legal form",
  );
  await expect(held.getByRole("status")).not.toContainText(
    "isn’t in your data",
  );
  await expect(reviews(page)).toContainText("Linked by its legal form");
});

test("doesn't blame a legal form for a many-valued claim that stays held", async ({
  page,
}) => {
  await mock.configure({ documentHeld: "legal_form" });
  const { doc, held } = await hedged(page);
  await approve(held);
  await loadVersion3(page);
  await page.goto(`${doc}?tab=review`);
  await expect(held.getByRole("status")).toContainText(
    "This load read the approval, and the claim stays held",
  );
  await expect(held.getByRole("status")).not.toContainText(
    "“globex corporation”",
  );
});

test("sends a review with no statement to a new load, or to the current one", async ({
  page,
}) => {
  await mock.configure({ documentHeld: "no_statement" });
  const old = await setup(page, [
    {
      name: "variant.md",
      text: "# Hedged deal\n\nGlobex Corporation may have signed.\n",
    },
  ]);
  const doc = await openDocument(page);
  await page.goto(`${doc}?tab=review`);
  const stale = reviews(page).getByRole("listitem").filter({
    hasText: "The model wasn’t sure",
  });
  await expect(stale).toContainText("Load again to settle this review.");
  await loadVersion3(page);
  await page.goto(`${doc}?load=${old}&tab=review`);
  await expect(stale).toContainText(
    "Open the current load to settle this review.",
  );
  await expect(stale).not.toContainText("Load again");
});

test("offers no approval of a sentence read two ways", async ({ page }) => {
  await mock.configure({ documentHeld: "differs" });
  const { held } = await hedged(page);
  await expect(held).toContainText("read more than one way");
  await expect(
    held.getByRole("button", { name: "Approve", exact: true }),
  ).toHaveCount(0);
});

test("says an end isn't in the graph yet", async ({ page }) => {
  await mock.configure({ documentHeld: "one_end" });
  const { held } = await hedged(page);
  await expect(held).toContainText("An end isn’t in the graph yet");
});

test("shows no percentage on a claim no verifier judged", async ({ page }) => {
  await mock.configure({ documentHeld: "unverified" });
  const { doc, held } = await hedged(page);
  await approve(held);
  await loadVersion3(page);
  await page.goto(doc);
  await page.getByRole("tab", { name: /^Facts/ }).click();
  await expect(page.getByText("signed_with").first()).toBeVisible();
  await expect(page.getByText(/\b0% confident/)).toHaveCount(0);
});

test("doesn't tell one claim of a sentence about another's approval", async ({
  page,
}) => {
  const { doc } = await hedged(
    page,
    "Globex Corporation and Initech may have signed.",
  );
  // Its group's rows: the reason is said once, in the group's head.
  const held = reviews(page)
    .getByRole("list", { name: "The model wasn’t sure" })
    .locator(":scope > li");
  await expect(held).toHaveCount(2);
  await approve(held.first());
  await expect(held.last()).not.toContainText("earlier approval");
  await expect(
    held.last().getByRole("button", { name: "Approve", exact: true }),
  ).toBeVisible();
  await loadVersion3(page);
  await page.goto(`${doc}?tab=review`);
  await expect(held).toHaveCount(1);
  await expect(held).not.toContainText("earlier approval");
  await expect(held).not.toContainText("reads differently");
});

test("promises no edge on a link that holds one value", async ({ page }) => {
  await oneValued(page);
  const { held } = await hedged(page);
  await expect(held).toContainText("Approving states:");
  await expect(held).not.toContainText("Approving writes:");
  await expect(held).toContainText("This link holds one value");
});

test("hides Approve from a member", async ({ page }) => {
  const { doc, held } = await hedged(page);
  await expect(
    held.getByRole("button", { name: "Approve", exact: true }),
  ).toBeVisible();
  await page.context().clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto(`${doc}?tab=review`);
  await expect(held).toContainText("Approving writes:");
  await expect(
    held.getByRole("button", { name: "Approve", exact: true }),
  ).toHaveCount(0);
});

test("links an adjudicated mention to its review and undoes the link on reload", async ({
  page,
}) => {
  await mock.configure({ documentAdjudicated: true });
  await setup(page);
  const doc = await openDocument(page);
  await page.getByRole("tab", { name: /^Entities/ }).click();
  await expect(page.getByText("Matched by the model")).toBeVisible();
  await page.getByRole("link", { name: "Review link" }).click();
  await expect(
    page.getByRole("tab", { name: /^Needs review/ }),
  ).toHaveAttribute("aria-selected", "true");
  await expect(reviews(page)).toContainText("Matched by the model");
  await page.getByRole("button", { name: "Undo link", exact: true }).click();
  await expect(
    page.getByText("Decision saved. Reload to apply."),
  ).toBeVisible();
  await loadVersion3(page);
  await page.goto(`${doc}?tab=entities`);
  await expect(
    page.getByRole("region", { name: "Customer", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await expect(page.getByText("Nothing needs review.")).toBeVisible();
});

test("links a mention by legal form to its review and undoes the link on reload", async ({
  page,
}) => {
  await mock.configure({ documentLegalForm: true });
  await setup(page);
  const doc = await openDocument(page);
  await page.getByRole("tab", { name: /^Entities/ }).click();
  await expect(page.getByText("By its legal form")).toBeVisible();
  await page.getByRole("link", { name: "Review link" }).click();
  await expect(
    page.getByRole("tab", { name: /^Needs review/ }),
  ).toHaveAttribute("aria-selected", "true");
  await expect(reviews(page)).toContainText("Linked by its legal form");
  await expect(reviews(page)).toContainText("Suggestions only:");
  await expect(reviews(page)).toContainText("for the name “");
  await expect(reviews(page)).not.toContainText("couldn’t confirm");
  await page.getByRole("button", { name: "Undo link", exact: true }).click();
  await expect(
    page.getByText("Decision saved. Reload to apply."),
  ).toBeVisible();
  await loadVersion3(page);
  await page.goto(`${doc}?tab=entities`);
  await expect(
    page.getByRole("region", { name: "Customer", exact: true }),
  ).toHaveCount(0);
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await expect(page.getByText("Nothing needs review.")).toBeVisible();
});

test("members can read candidate reviews but cannot settle or remove decisions", async ({
  page,
}) => {
  await setup(page);
  const doc = await openDocument(page);
  await settleNotInData(page);
  await page.context().clearCookies();
  await signIn(page, "alan@example.com");
  await page.goto(`${doc}?tab=review`);
  await expect(reviews(page)).toContainText("Suggestions only:");
  await expect(page.getByRole("button", { name: "Settle review" })).toHaveCount(
    0,
  );
  await expect(
    page.getByRole("button", { name: "Remove decision" }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("list", { name: "Saved decisions" }),
  ).toContainText("Not in data");
});

test("reconciles a decision removed elsewhere without keeping the saved status", async ({
  page,
}) => {
  await setup(page);
  await openDocument(page);
  await settleNotInData(page);
  const decisions = page.getByRole("list", { name: "Saved decisions" });
  await expect(decisions).toContainText("Not in data");
  // Other failures leave the saved row available to retry.
  const pattern = "**/papi/**/review-decisions/*";
  await page.route(pattern, (route) =>
    route.fulfill({
      status: 503,
      contentType: "application/json",
      body: JSON.stringify({ code: 14, message: "Removal unavailable" }),
    }),
  );
  await page.getByRole("button", { name: "Remove decision" }).click();
  await expect(
    page.getByRole("alert").filter({ hasText: "Removal unavailable" }),
  ).toBeVisible();
  await expect(decisions).toBeVisible();
  await page.unroute(pattern);
  const base =
    "/papi/v1/workspaces/ws_acme/foundry/ontologies/ont_acme_contracts/review-decisions";
  const saved = await (await page.request.get(base)).json();
  const removed = await page.request.delete(
    `${base}/${saved.decisions[0].id}`,
    {
      headers: { origin: APP_ORIGIN, "x-helix-csrf": await csrfToken(page) },
    },
  );
  expect(removed.status()).toBe(200);
  const response = page.waitForResponse(
    (r) =>
      r.request().method() === "DELETE" &&
      r.url().includes("/review-decisions/") &&
      r.status() === 404,
  );
  await page.getByRole("button", { name: "Remove decision" }).click();
  expect((await response).status()).toBe(404);
  await expect(decisions).toHaveCount(0);
  await expect(page.getByText("Decision saved. Reload to apply.")).toHaveCount(
    0,
  );
  await expect(
    page.getByText("Decision removed. Reload to apply."),
  ).toBeVisible();
  await expect(page.locator("p[role=alert]")).toHaveCount(0);
});

test("settles while saved decisions are still being read", async ({ page }) => {
  await setup(page);
  // The list pages through every decision; settling never waits for it.
  let release = () => {};
  const held = new Promise<void>((r) => (release = r));
  await page.route("**/papi/**/review-decisions*", async (route) => {
    if (route.request().method() === "GET") await held;
    await route.continue();
  });
  await openDocument(page);
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await choose(page.getByLabel("Link this name to"), "Not in data");
  const sent = page.waitForResponse(
    (r) => r.request().method() === "POST" && /:settle$/.test(r.url()),
  );
  await page.getByRole("button", { name: "Settle review" }).click();
  expect((await sent).status()).toBe(200);
  release();
  await expect(
    page.getByText("Decision saved. Reload to apply."),
  ).toBeVisible();
});

test("says a decision this load read left the review open, without asking to reload", async ({
  page,
}) => {
  await setup(page);
  const doc = await openDocument(page);
  // A decision saved before the current load started, linking the name to
  // an entity that isn't in it: the engine leaves the name unresolved.
  await page.route("**/papi/**/review-decisions*", (route) =>
    route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({
        decisions: [
          {
            id: "decision_old",
            kind: "resolve",
            type: "Customer",
            docId: doc.split("/").at(-1),
            normName: "globexx corporation",
            entityKey: "cus_GONE",
            decidedAt: "2000-01-01T00:00:00Z",
          },
        ],
      }),
    }),
  );
  await page.goto(`${doc}?tab=review`);
  await expect(reviews(page)).toContainText(
    "This load read the saved decision, and the review is still open",
  );
  await expect(page.getByText("Decision saved. Reload to apply.")).toHaveCount(
    0,
  );
  await expect(page.getByLabel("Link this name to")).toBeEnabled();
});

test("keeps a table's value over a document's, and replaces it only by choice", async ({
  page,
}) => {
  await setup(page, [
    {
      name: "billing.md",
      text: "# Billing terms\n\nGlobex Corporation is billed in GBP. Globex Corporation signed.\n",
    },
  ]);
  const globex = entity("Customer", "k:cus_GLOBEX01");
  await page.goto(globex);
  await expect(page.locator(".ex-props")).toContainText("usd");
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  const review = reviews(page)
    .getByRole("listitem")
    .filter({ hasText: "Disagrees on a value" });
  await expect(review).toContainText("The table’s value, usd, is kept.");
  await expect(
    review.getByRole("list", { name: "Conflicting values" }),
  ).toContainText("GBP");
  // The only choice offered replaces the table's value, and says so.
  const replace = review.getByLabel("Replace the table’s value with");
  await expect(replace).toHaveText("Choose…");
  await replace.click();
  await expect(page.getByRole("listbox").getByRole("option")).toHaveText([
    "GBP",
  ]);
  await page.keyboard.press("Escape");
  await expect(review).toContainText(
    "A chosen value replaces the table’s in every later load, even after the table changes.",
  );
  await expect(review.getByLabel("Keep value")).toHaveCount(0);
  // The document's view shows the same review the same way.
  await page.goto(`${O}/documents`);
  await page
    .getByRole("grid", { name: "Documents" })
    .getByRole("link", { name: "billing.md", exact: true })
    .click();
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await expect(reviews(page)).toContainText("The table’s value, usd, is kept.");
  await page.goto(globex);
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await choose(replace, "GBP");
  const sent = page.waitForRequest(
    (r) => r.method() === "POST" && /:settle$/.test(r.url()),
  );
  await review.getByRole("button", { name: "Settle review" }).click();
  expect((await sent).postDataJSON()).toEqual({
    decision: { prop: { text: "GBP" } },
  });
  await expect(
    review.getByText("Decision saved. Reload to apply."),
  ).toBeVisible();
  await loadVersion3(page);
  await page.goto(globex);
  await expect(page.locator(".ex-props")).toContainText("GBP");
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await expect(
    page.getByRole("list", { name: "Saved decisions" }),
  ).toContainText("currency: GBP");
});
