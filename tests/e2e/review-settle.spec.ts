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

const TERMS = [
  {
    name: "original.md",
    text: "# Shared contract\n\nGlobex Corporation signed. The Agreement runs for 24 months.\n",
  },
  {
    name: "amendment.md",
    text: "# Shared contract\n\nGlobex Corporation signed. The Agreement runs for 36 months.\n",
  },
];
/** Opens the contract two documents give different terms, on its Needs review tab. */
async function conflictingTerms(page: Page) {
  await setup(page, TERMS);
  const at = entity("Contract", "n:shared contract");
  await page.goto(at);
  await expect(page.locator(".ex-props")).toContainText("No value");
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  return at;
}
/** Chooses a term on the open property review, and sends it. */
async function chooseTerm(page: Page, months: string, field = "Keep value") {
  await choose(page.getByLabel(field), months);
  await page.getByRole("button", { name: "Settle review" }).click();
}

test("picks a property conflict value, and the load holds it at once", async ({
  page,
}) => {
  const at = await conflictingTerms(page);
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
  // From here the page is never loaded again: what changes, changes in place.
  let loads = 0;
  page.on("load", () => (loads += 1));
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await expect(reviews(page)).toContainText(
    "It’s sent to this load at once, and later loads read it too. A document dated after your decision still wins.",
  );
  const sent = page.waitForRequest(
    (r) => r.method() === "POST" && /:settle$/.test(r.url()),
  );
  await chooseTerm(page, "36");
  expect((await sent).postDataJSON()).toEqual({
    decision: { prop: { integer: "36" } },
  });
  // One job sends the load its decisions; the card says what the load knows.
  const status = reviews(page).getByRole("status");
  await expect(status).toContainText(
    "Decided: 36. This load was sent the decision, and holds that value unless a document dated after the decision states another.",
  );
  await expect(page.getByText("Reload to apply")).toHaveCount(0);
  const decisions = page.getByRole("list", { name: "Saved decisions" });
  await expect(decisions).toContainText("term_months: 36 · sent to this load");
  await page.getByRole("tab", { name: /^Overview/ }).click();
  await expect(page.locator(".ex-props")).toContainText(/term_months\s*36/);
  await expect(page.locator(".ex-props")).not.toContainText("No value");

  // Chosen again, the load holds the value chosen last. The value it holds
  // is the decision's, so the card offers another, and names no table.
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await expect(reviews(page)).not.toContainText("The table’s value");
  await chooseTerm(page, "24", "Choose another value");
  await expect(status).toContainText("Decided: 24.");
  await page.getByRole("tab", { name: /^Overview/ }).click();
  await expect(page.locator(".ex-props")).toContainText(/term_months\s*24/);

  // Removed, the load takes it back: the property holds nothing again.
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await decisions.getByRole("button", { name: "Remove decision" }).click();
  await expect(
    page.getByText(
      "Decision removed. This load is taking the value back now; later loads won’t read it.",
    ),
  ).toBeVisible();
  await expect(decisions).toHaveCount(0);
  await page.getByRole("tab", { name: /^Overview/ }).click();
  await expect(page.locator(".ex-props")).toContainText("No value");
  expect(loads).toBe(0);

  // A later load reads a saved value as it is made, and asks nothing.
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await chooseTerm(page, "36");
  await expect(status).toContainText("Decided: 36.");
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

test("says what a chosen value waits behind while another job is running", async ({
  page,
}) => {
  await conflictingTerms(page);
  // Another job holds the database, so the value's own job has yet to run.
  await mock.configure({ loadStepMs: 60_000 });
  let kind = "extract";
  await page.route(
    /\/foundry\/ontologies\/ont_acme_contracts(\?|$)/,
    async (route) => {
      if (route.request().method() !== "GET") return route.continue();
      const response = await route.fetch();
      const body = await response.json();
      await route.fulfill({
        response,
        json: {
          ...body,
          ontology: {
            ...body.ontology,
            activeJob: {
              kind,
              loadId: body.ontology.currentLoad?.id,
              phase: "documents",
            },
          },
        },
      });
    },
  );
  await page.reload();
  await chooseTerm(page, "36");
  const status = reviews(page).getByRole("status");
  await expect(status).toContainText(
    "Decision saved. Another job is running, so this waits: it’s applied to this load when that ends.",
  );
  await expect(
    page.getByRole("list", { name: "Saved decisions" }),
  ).toContainText("term_months: 36 · being sent to this load");
  // Behind a new load, it goes to whichever load is current when that ends.
  kind = "load";
  await page.reload();
  await expect(status).toContainText(
    "Decision saved. A load is running, so this waits: it’s applied when that ends, to the load that is current then.",
  );
  // The load holds nothing of it yet.
  await page.getByRole("tab", { name: /^Overview/ }).click();
  await expect(page.locator(".ex-props")).toContainText("No value");
});

test("says a chosen value's job failed, and sends it when it is chosen again", async ({
  page,
}) => {
  await conflictingTerms(page);
  await mock.configure({ loadStepMs: 200, loadOutcome: "fail" });
  await chooseTerm(page, "36");
  const status = reviews(page).getByRole("status");
  await expect(status).toContainText(
    "Decided: 36, but the job that sends it to this load failed. Choose it again to send it.",
  );
  await expect(
    page.getByRole("list", { name: "Saved decisions" }),
  ).toContainText("term_months: 36 · not confirmed as sent to this load");
  await page.getByRole("tab", { name: /^Overview/ }).click();
  await expect(page.locator(".ex-props")).toContainText("No value");
  await mock.configure({ loadStepMs: 200, loadOutcome: "succeed" });
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await chooseTerm(page, "36");
  await expect(status).toContainText("Decided: 36. This load was sent");
  await page.getByRole("tab", { name: /^Overview/ }).click();
  await expect(page.locator(".ex-props")).toContainText(/term_months\s*36/);
});

test("approves a held claim, and the load writes it as a fact at once", async ({
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
  await expect(held).toContainText("written to this load at once");
  await page.getByRole("tab", { name: /^Facts/ }).click();
  await expect(page.getByRole("list", { name: "Facts" })).toHaveCount(0);
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  // The job that writes it takes a moment: the card says it is on its way.
  await mock.configure({ loadStepMs: 1500 });
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
    "Approved. Writing it to this load…",
  );
  await expect(
    held.getByRole("button", { name: "Approve", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("list", { name: "Saved decisions" }),
  ).toContainText("Approved, being written: “Globex Corporation may have");
  // The load writes it: its card goes and its fact shows, with no new load
  // and the page never loaded again.
  await expect(page.getByText("Nothing needs review.")).toBeVisible();
  await expect(
    page.getByRole("list", { name: "Saved decisions" }),
  ).toContainText("Approved: “Globex Corporation may have signed.”");
  await page.getByRole("tab", { name: /^Facts/ }).click();
  await expect(page.getByRole("list", { name: "Facts" })).toContainText(
    "signed_with",
  );
  // A later load writes it too, and has no review of it.
  await mock.configure({ loadStepMs: 200 });
  const next = await loadVersion3(page);
  expect(next).not.toBe(old);
  await page.goto(`${doc}?tab=review`);
  await expect(page.getByText("Nothing needs review.")).toBeVisible();
  await expect(
    page.getByRole("list", { name: "Saved decisions" }),
  ).toContainText("Approved: “Globex Corporation may have signed.”");
});

test("asks again when an approval was of what the claim said before", async ({
  page,
}) => {
  // The load keeps this one held, so its card stays to be read again.
  await mock.configure({ documentHeld: "refused" });
  const { held } = await hedged(page);
  await approve(held);
  await expect(held.getByRole("status")).toContainText(
    "this load kept it held",
  );
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
  await expect(
    held.getByRole("button", { name: "Approve", exact: true }),
  ).toBeVisible();
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
/** Approves a held claim on its card: the load applies it a moment later. */
const approve = async (card: ReturnType<Page["locator"]>) => {
  await card.getByRole("button", { name: "Approve", exact: true }).click();
};

test("says why the load kept an approved claim held", async ({ page }) => {
  await mock.configure({ documentHeld: "refused" });
  const { doc, held } = await hedged(page);
  await approve(held);
  // The load's own reason, on the card, with no new load.
  await expect(held.getByRole("status")).toContainText(
    "Approved, and this load kept it held. It conflicts with what the graph holds for this link",
  );
  await expect(
    held.getByRole("button", { name: "Approve", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("list", { name: "Saved decisions" }),
  ).toContainText("Approved, not written: “");
  // A later load reads the approval as it commits the document, and says the same.
  await loadVersion3(page);
  await page.goto(`${doc}?tab=review`);
  await expect(held.getByRole("status")).toContainText(
    "this load kept it held. It conflicts with what the graph holds",
  );
});

test("rejects a claim the load kept held, in place of its approval", async ({
  page,
}) => {
  await mock.configure({ documentHeld: "refused" });
  const { held } = await hedged(page);
  // While it is being written it can't be rejected: its job may have sent it.
  await approve(held);
  await expect(held.getByRole("status")).toContainText("kept it held");
  const saved = page
    .getByRole("list", { name: "Saved decisions" })
    .getByRole("listitem");
  await expect(
    saved.filter({ hasText: /^Approved, not written: “/ }),
  ).toHaveCount(1);
  // The load refused it; the person says it is wrong after all.
  await held.getByRole("button", { name: "Reject", exact: true }).click();
  await expect(held).toHaveCount(0);
  await expect(page.getByText("Nothing needs review.")).toBeVisible();
  await expect(saved).toHaveCount(1);
  await expect(saved).toContainText(/^Rejected: “/);
  // It stays out when the page is loaded again.
  await page.reload();
  await expect(saved).toContainText(/^Rejected: “/);
  await expect(held).toHaveCount(0);
});

test("says what may have kept a claim held when its load never said", async ({
  page,
}) => {
  await mock.configure({ documentHeld: "refused" });
  const { held } = await hedged(page);
  await approve(held);
  await expect(held.getByRole("status")).toContainText("kept it held");
  // A load made before loads said what became of an approval: it read the
  // decision, and reported nothing.
  await page.route(/\/review-decisions(\?|$)/, async (route) => {
    const response = await route.fetch();
    const body = (await response.json()) as ListReviewDecisionsResponse;
    await route.fulfill({
      response,
      json: {
        ...body,
        decisions: body.decisions?.map((d) => ({
          ...d,
          applied: { status: "" },
        })),
      },
    });
  });
  await page.reload();
  await expect(held.getByRole("status")).toContainText(
    "This load read the approval, and the claim stays held: a name in it isn’t in your data",
  );
});

test("offers to write an approval the current load hasn't read", async ({
  page,
}) => {
  await mock.configure({ documentHeld: "refused" });
  const { held } = await hedged(page);
  await approve(held);
  await expect(held.getByRole("status")).toContainText("kept it held");
  // Saved while another load was current: this load's decisions lack it.
  const decisions = /\/review-decisions(\?|$)/;
  await page.route(decisions, async (route) => {
    const response = await route.fetch();
    const body = (await response.json()) as ListReviewDecisionsResponse;
    await route.fulfill({
      response,
      json: {
        ...body,
        decisions: body.decisions?.map(({ applied: _, ...d }) => d),
      },
    });
  });
  await page.reload();
  await expect(held.getByRole("status")).toContainText(
    "An approval of this is saved, and this load hasn’t read it. Approve it to write it now.",
  );
  await page.unroute(decisions);
  await approve(held);
  await expect(held.getByRole("status")).toContainText("kept it held");
});

test("says an approval's job failed before saying what became of it, and finishes it", async ({
  page,
}) => {
  await mock.configure({ documentHeld: "refused" });
  const { held } = await hedged(page);
  await approve(held);
  await expect(held.getByRole("status")).toContainText("kept it held");
  // The job that applies it failed for good before the load said anything:
  // the claim may or may not be written, and no job is coming.
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
          applied: { status: "unconfirmed", reason: "job_failed" },
        })),
      },
    });
  });
  await page.reload();
  await expect(held.getByRole("status")).toContainText(
    "Approved, but the job that writes it failed before it said whether this was written. Approve it again to finish.",
  );
  // Nothing says the load kept it held: no one knows that.
  await expect(held.getByRole("status")).not.toContainText("kept it held");
  await page.unroute(decisions);
  await approve(held);
  await expect(held.getByRole("status")).toContainText("kept it held");
});

test("says what an approval waits behind while another job is running", async ({
  page,
}) => {
  const { held } = await hedged(page);
  // Another job holds the database, so the approval's own job has yet to run.
  await mock.configure({ loadStepMs: 60_000 });
  let kind = "extract";
  await page.route(
    /\/foundry\/ontologies\/ont_acme_contracts(\?|$)/,
    async (route) => {
      if (route.request().method() !== "GET") return route.continue();
      const response = await route.fetch();
      const body = await response.json();
      await route.fulfill({
        response,
        json: {
          ...body,
          ontology: {
            ...body.ontology,
            activeJob: {
              kind,
              loadId: body.ontology.currentLoad?.id,
              phase: "documents",
            },
          },
        },
      });
    },
  );
  await page.reload();
  await expect(held).toContainText(
    "It’s written to this load at once. Later loads read the approval too",
  );
  await approve(held);
  await expect(held.getByRole("status")).toContainText(
    "Approved. Another job is running, so this waits: it’s written to this load when that ends.",
  );
  // Behind a new load, it goes to whichever load is current when that ends.
  kind = "load";
  await page.reload();
  await expect(held.getByRole("status")).toContainText(
    "Approved. A load is running, so this waits: it’s written when that ends, to the load that is current then.",
  );
});

test("names the name a legal form linked when it holds a one-valued claim", async ({
  page,
}) => {
  await oneValued(page);
  await mock.configure({ documentHeld: "legal_form" });
  const { held } = await hedged(page);
  await expect(held).toContainText("Settle that name too");
  await approve(held);
  await expect(held.getByRole("status")).toContainText(
    "this load kept it held. “Globex Corporation” is linked only by its legal form",
  );
  await expect(held.getByRole("status")).toContainText(
    "Settle that name, and the next load writes this.",
  );
  await expect(held.getByRole("status")).not.toContainText(
    "isn’t in your data",
  );
  await expect(reviews(page)).toContainText("Linked by its legal form");
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
  const { held } = await hedged(page);
  await approve(held);
  await expect(page.getByText("Nothing needs review.")).toBeVisible();
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
  // The load writes the one approved; the other's card is as it was.
  await expect(held).toHaveCount(1);
  await expect(held).not.toContainText("earlier approval");
  await expect(
    held.getByRole("button", { name: "Approve", exact: true }),
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
  await mock.configure({ documentLinked: "adjudicated" });
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
  await mock.configure({ documentLinked: "legal_form" });
  await setup(page);
  const doc = await openDocument(page);
  await page.getByRole("tab", { name: /^Entities/ }).click();
  await expect(page.getByText("By its legal form")).toBeVisible();
  await page.getByRole("link", { name: "Review link" }).click();
  await expect(
    page.getByRole("tab", { name: /^Needs review/ }),
  ).toHaveAttribute("aria-selected", "true");
  await expect(reviews(page)).toContainText("Linked by its legal form");
  // The row is one the load linked, not a suggestion.
  await expect(reviews(page)).toContainText("Linked to:");
  await expect(reviews(page)).not.toContainText("Suggestions only:");
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

// Contract 4.13.0: a name the load linked by part of a row's name, or by an
// identifier the text uses as a name. Each has a review a person keeps or
// overturns, and holds an approval at its end until then.
for (const [linked, by, title] of [
  ["part_name", "By part of its name", "Linked by part of a name"],
  [
    "own_name",
    "By an identifier used as its name",
    "Linked by an identifier used as its name",
  ],
] as const)
  test(`links a mention by ${linked} to its review and undoes the link on reload`, async ({
    page,
  }) => {
    await mock.configure({ documentLinked: linked });
    await setup(page);
    const doc = await openDocument(page);
    await page.getByRole("tab", { name: /^Entities/ }).click();
    await expect(page.getByText(by)).toBeVisible();
    await page.getByRole("link", { name: "Review link" }).click();
    await expect(
      page.getByRole("tab", { name: /^Needs review/ }),
    ).toHaveAttribute("aria-selected", "true");
    // What the load did, why a person is asked, and no model's confidence.
    await expect(reviews(page)).toContainText(title);
    await expect(reviews(page)).toContainText(
      "can be wrong, so it waits for you",
    );
    await expect(reviews(page)).toContainText("Linked to:");
    await expect(reviews(page)).not.toContainText("% confident");
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

test("keeps a link by part of a name, and the next load holds it as a person's", async ({
  page,
}) => {
  await mock.configure({ documentLinked: "part_name" });
  await setup(page);
  const doc = await openDocument(page);
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await expect(reviews(page)).toContainText("Linked by part of a name");
  await choose(page.getByLabel("Link this name to"), "Globex Corporation");
  await page.getByRole("button", { name: "Settle review" }).click();
  await expect(
    page.getByText("Decision saved. Reload to apply."),
  ).toBeVisible();
  await expect(
    page.getByRole("list", { name: "Saved decisions" }),
  ).toContainText("globexx corporation → cus_GLOBEX01");
  await loadVersion3(page);
  await page.goto(`${doc}?tab=entities`);
  await expect(page.getByText("Linked by a person")).toBeVisible();
  await expect(page.getByText("By part of its name")).toHaveCount(0);
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await expect(page.getByText("Nothing needs review.")).toBeVisible();
});

// The engine holds an approval at such a name until a person settles it,
// which only a later load reads: the card says so before and after, and
// leads to the name's own review, at the name.
for (const [linked, how, title] of [
  [
    "part_name",
    "linked only by part of a row’s name",
    "Linked by part of a name",
  ],
  [
    "own_name",
    "linked only by a code the document calls it by",
    "Linked by an identifier used as its name",
  ],
] as const)
  test(`sends an approval held for a name linked by ${linked} to that name's review`, async ({
    page,
  }) => {
    // On a link that holds many: unlike a legal form's, this holds any
    // claim. The name isn't where its sentence starts, so the claim's place
    // and the name's are told apart.
    await mock.configure({ documentHeld: linked });
    const { held } = await hedged(
      page,
      "Reportedly, Globex Corporation may have signed.",
    );
    // Named as the document writes it.
    await expect(held).toContainText(
      `“Globex Corporation” is ${how}. Settle that name too, or this stays held.`,
    );
    // No approval writes it to this load, and the card promises none.
    await expect(held).toContainText(
      "This load keeps it held: the first load after that name is settled writes it.",
    );
    await expect(held).not.toContainText("written to this load at once");
    await approve(held);
    await expect(held.getByRole("status")).toContainText(
      `Approved, and this load kept it held. “Globex Corporation” is ${how}, and an approval doesn’t stand on such a link. Settle that name, and the next load writes this.`,
    );
    await expect(held.getByRole("status")).not.toContainText("holds one value");
    // Saved, it offers no approval this load would refuse again.
    await expect(
      held.getByRole("button", { name: "Approve", exact: true }),
    ).toHaveCount(0);
    // The card leads to the name's own review, at the name: not at the
    // claim's sentence, where the held claim's review is.
    const toName = held
      .getByRole("status")
      .getByRole("link", { name: "Review that name" });
    const href = (await toName.getAttribute("href")) ?? "";
    expect(href).toMatch(/tab=review.*#s\d+-\d+$/);
    const anchor = href.slice(href.lastIndexOf("#") + 1);
    await toName.click();
    await expect(page).toHaveURL(new RegExp(`#${anchor}$`));
    await expect(
      page.getByLabel("Document text").locator(`[id="${anchor}"]`),
    ).toHaveText("Globex Corporation");
    const name = reviews(page).getByRole("list", { name: title });
    await expect(name).toContainText("Linked to:");
    await choose(name.getByLabel("Link this name to"), "Globex Corporation");
    await name.getByRole("button", { name: "Settle review" }).click();
    await expect(name).toContainText("Decision saved. Reload to apply.");
  });

test("promises no write in this load, and offers no approval it would refuse, for a claim a name holds", async ({
  page,
}) => {
  await mock.configure({ documentHeld: "part_name" });
  const { held } = await hedged(
    page,
    "Reportedly, Globex Corporation may have signed.",
  );
  await approve(held);
  await expect(held.getByRole("status")).toContainText("kept it held");
  // What the load made of the saved approval, as the decisions say it.
  const decisions = /\/review-decisions(\?|$)/;
  const reads = async (applied?: { status: string; reason?: string }) => {
    await page.unroute(decisions);
    await page.route(decisions, async (route) => {
      const response = await route.fetch();
      const body = (await response.json()) as ListReviewDecisionsResponse;
      await route.fulfill({
        response,
        json: {
          ...body,
          decisions: body.decisions?.map(({ applied: _, ...d }) =>
            applied ? { ...d, applied } : d,
          ),
        },
      });
    });
    await page.reload();
  };
  // Its job yet to run: the card doesn't say it is being written.
  await reads({ status: "pending" });
  await expect(held.getByRole("status")).toContainText(
    "Approved. This load keeps it held: the first load after that name is settled writes it.",
  );
  await expect(held.getByRole("status")).not.toContainText(
    "Writing it to this load",
  );
  // Saved while another load was current, its job failed before saying,
  // or it found no claim: approving again here would only be refused.
  for (const applied of [
    undefined,
    { status: "unconfirmed", reason: "job_failed" },
    { status: "missed" },
  ]) {
    await reads(applied);
    await expect(held.getByRole("status")).toContainText(
      "An approval of this is saved, and the claim stays held. “Globex Corporation” is linked only by part of a row’s name, and an approval doesn’t stand on such a link. Settle that name, and the next load writes this.",
    );
    await expect(
      held.getByRole("button", { name: "Approve", exact: true }),
    ).toHaveCount(0);
    await expect(
      held.getByRole("status").getByRole("link", { name: "Review that name" }),
    ).toBeVisible();
  }
  await page.unroute(decisions);
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
    "A chosen value replaces the table’s in this load and every later one, even after the table changes.",
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
  // The load is sent the choice at once: the table's value gives way, and
  // the card no longer calls the value it holds the table's.
  await expect(review.getByRole("status")).toContainText(
    "Decided: GBP. This load was sent the decision",
  );
  await expect(review).not.toContainText("The table’s value");
  await expect(review.getByLabel("Choose another value")).toBeVisible();
  const decisions = page.getByRole("list", { name: "Saved decisions" });
  await expect(decisions).toContainText("currency: GBP · sent to this load");
  await page.getByRole("tab", { name: /^Overview/ }).click();
  await expect(page.locator(".ex-props")).toContainText(/currency\s*GBP/);
  // Removed, the table's value is the load's again, with no reload.
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await decisions.getByRole("button", { name: "Remove decision" }).click();
  await expect(
    page.getByText("This load is taking the value back"),
  ).toBeVisible();
  await page.getByRole("tab", { name: /^Overview/ }).click();
  await expect(page.locator(".ex-props")).toContainText(/currency\s*usd/);
  // Chosen again, a later load reads it as it is made.
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await choose(replace, "GBP");
  await review.getByRole("button", { name: "Settle review" }).click();
  await expect(review.getByRole("status")).toContainText("Decided: GBP.");
  await loadVersion3(page);
  await page.goto(globex);
  await expect(page.locator(".ex-props")).toContainText("GBP");
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await expect(
    page.getByRole("list", { name: "Saved decisions" }),
  ).toContainText("currency: GBP");
});
