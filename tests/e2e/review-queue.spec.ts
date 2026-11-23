import { createHash } from "node:crypto";
import type { Page, Request } from "@playwright/test";
import {
  expect,
  file,
  mock,
  signIn,
  test,
  uploadAndLoadContract,
} from "./support/mock";

// The review queue (/ontology/:id/review, the Review page's Facts tab), as
// the walkthrough takes it: the decisions writing the most facts first,
// each card's evidence before its score, values a table keeps for
// information, and about one card in ten drawn at random from a score band,
// which waits for the finish as a spot check, each decision saying how its
// card was drawn (review-walk.spec.ts walks the cards).

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
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Review");
}
const card = (page: Page) => page.getByRole("article");
const rail = (page: Page) =>
  page.getByRole("navigation", { name: "Next cards" }).getByRole("listitem");
/** The finish, at once: no value is left to decide, so the walk is empty. */
async function finish(page: Page) {
  await page.goto(`${O}/review?kind=value`);
  await expect(
    page.getByRole("heading", { level: 2, name: "Nothing to decide" }),
  ).toBeVisible();
}
const spot = (page: Page) => page.getByRole("list", { name: "Spot checks" });
const body = (r: Request) => r.postDataJSON() as Record<string, unknown>;

test("walks what writes facts first, evidence before the score, values a table keeps to FYI, and random draws at the finish", async ({
  page,
}) => {
  await queued(page);
  // It's one of the ontology's pages.
  await expect(page.locator('.hf-subnav-link[aria-current="page"]')).toHaveText(
    /^Review/,
  );
  // A held claim: its quote and document, then what it writes, then the
  // verifier's score.
  await expect(card(page)).toContainText(
    /may have signed[\s\S]*variant\.md · section \d+[\s\S]*Verifier score 0\.72 \(0\.7 to 0\.8\)/,
  );
  await expect(card(page)).not.toContainText("Random check");
  // Held claims (a fact each, at once) before names.
  const lines = await rail(page).allTextContents();
  const claims = lines.flatMap((l, i) =>
    l.includes("Held claim · ") ? [i] : [],
  );
  const names = lines.flatMap((l, i) => (l.includes("Name · ") ? [i] : []));
  expect(claims.length).toBe(2);
  expect(names.length).toBeGreaterThan(0);
  expect(Math.max(...claims)).toBeLessThan(Math.min(...names));
  // The name in no row writes the claim it holds back.
  await rail(page)
    .filter({ hasText: "Umbrella Holdings was copied" })
    .getByRole("button")
    .click();
  await expect(card(page)).toContainText(
    "Settling it writes 1 fact the load holds back for it.",
  );
  // The value documents state against the table is for information.
  await page
    .getByRole("radiogroup", { name: "Lane" })
    .getByRole("radio", { name: /^FYI/ })
    .click();
  await expect(
    page
      .getByRole("list", { name: "For information" })
      .getByRole("listitem")
      .filter({ hasText: "A value the table keeps" }),
  ).toHaveCount(1);
  // What isn't in the queue, and why.
  await expect(
    page.getByText(/Not in the queue: .*no decision settles here/),
  ).toBeVisible();
  // The random draws wait for the finish, where they can be skipped.
  await finish(page);
  await expect(spot(page).getByText("Random check").first()).toBeVisible();
  const section = page.getByRole("region", { name: "Spot checks" });
  await section.getByRole("button", { name: "Skip" }).click();
  await expect(spot(page)).toHaveCount(0);
  await section.getByRole("button", { name: "Show" }).click();
  await expect(spot(page)).toBeVisible();
});

test("a decision from the walk says it was ranked, and read again its card has left the queue", async ({
  page,
}) => {
  await queued(page);
  await expect(card(page)).toContainText("may have signed");
  const sent = page.waitForRequest(
    (r) => r.method() === "POST" && /\/reviews\/[^/]+:settle$/.test(r.url()),
  );
  await card(page).getByRole("button", { name: "Reject" }).click();
  expect(body(await sent)).toEqual({
    decision: {
      reject: { statementHash: expect.stringMatching(/^[0-9a-f]{64}$/) },
    },
    drawn: "ranked",
  });
  const before = await rail(page).count();
  await page.reload();
  await expect(card(page)).toBeVisible();
  await expect.poll(async () => rail(page).count()).toBe(before - 1);
  await expect(page.getByText(/Not in the queue: 1 decided/)).toBeVisible();
});

test("labels an accepted fact drawn at random, which changes nothing and counts in the random checks", async ({
  page,
}) => {
  await queued(page);
  await finish(page);
  const fact = spot(page)
    .getByRole("listitem")
    .filter({ hasText: "An accepted fact" })
    .first();
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
  const held = card(page);
  await expect(held).toContainText("may have signed");
  const saved = page.waitForResponse(
    (r) => r.request().method() === "POST" && /:settle$/.test(r.url()),
  );
  await held.getByRole("button", { name: "Reject" }).click();
  const decision = (await (await saved).json()).decision;
  await expect(page.locator(".rv-toast")).toContainText("Rejected.");
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
  await finish(page);
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
