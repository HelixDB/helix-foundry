import { readFileSync } from "node:fs";
import type { Page } from "@playwright/test";
import { expect, explorer, mock, signIn, test } from "./support/mock";

// A load's rules, evaluated when they are read: the contracts draft says
// customers named in one contract co-signed it (a symmetric relation),
// flags customers invoiced and under contract, and checks that invoices
// bill customers under contract. An entity's page shows what they derive
// and why; the load's Rule breaks page counts and pages what the
// constraint found, and says what its checks leave out. A break an Ask
// answer cites opens there, at its record, and its entity's page explains it.

const O = "/ontology/ont_acme_contracts",
  X = "/explore/ont_acme_contracts";
const CONTRACT = readFileSync(
  new URL("../../mock/fixtures/contract.md", import.meta.url),
);
const customer = (key: string) =>
  `${X}/entities/${Buffer.from(`Customer\0k:${key}`).toString("base64url")}`;

// Uploads the seeded contract, makes version 3 current and loads it, ending
// on the load's page.
async function load(page: Page) {
  await page.goto("/data");
  await explorer(page).getByLabel("Choose documents").setInputFiles({
    name: "contract.md",
    mimeType: "text/markdown",
    buffer: CONTRACT,
  });
  await expect(
    page
      .getByRole("list", { name: "Uploads" })
      .getByRole("listitem", { name: "contract.md" }),
  ).toContainText("Uploaded");
  await page.goto(`${O}/versions/ver_seed_3`);
  await page.getByRole("button", { name: "Make current" }).click();
  await page
    .getByRole("dialog", { name: "Make version 3 current?" })
    .getByRole("button", { name: "Make current" })
    .click();
  await expect(page.getByText("Version 3 is now current.")).toBeVisible();
  await page.goto(O);
  await page
    .getByRole("status")
    .filter({ hasText: "Version 3 isn’t loaded yet" })
    .getByRole("button", { name: "Load data" })
    .click();
  await page
    .getByRole("dialog", { name: "Load version 3 into contracts?" })
    .getByRole("button", { name: "Load" })
    .click();
  await expect(page).toHaveURL(/\/loads\/load_/);
  await expect(
    page.getByRole("status").filter({ hasText: "Loaded" }).first(),
  ).toBeVisible({ timeout: 20_000 });
}

test("shows what the rules derive about an entity, and why", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await load(page);
  await page.goto(customer("cus_GLOBEX01"));
  await page.getByRole("tab", { name: /^Derived/ }).click();
  const flags = page.getByRole("list", { name: "Flags" });
  await expect(flags).toContainText("invoiced");
  await expect(flags).toContainText("under contract");
  // Globex co-signed the contract with Initech, both ways round.
  const cosigned = page.getByRole("region", { name: "co_signed to" });
  await expect(
    cosigned.getByRole("link", { name: "Initech LLC" }),
  ).toBeVisible();
  await expect(cosigned).toContainText("Derived");
  await expect(
    page
      .getByRole("region", { name: "co_signed from" })
      .getByRole("link", { name: "Initech LLC" }),
  ).toBeVisible();
  // Why: the rule, down to the contract's two stated facts.
  await cosigned.getByRole("button", { name: "Why" }).click();
  const why = cosigned.getByRole("list", { name: "Why it holds" });
  await expect(why).toContainText("by the rule “co_signers”");
  await expect(why.getByText("stated in the data")).toHaveCount(2);
  await expect(
    why.getByRole("link", { name: "Master services agreement" }).first(),
  ).toBeVisible();
  // A flag's why.
  await flags.getByRole("button", { name: "Why" }).first().click();
  await expect(
    flags.getByRole("list", { name: "Why it holds" }).first(),
  ).toContainText("by the rule");
  // The self-link a rule would make is shown, never stored.
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  await expect(page.getByRole("list", { name: "Needs review" })).toContainText(
    "Breaks a rule: co_signed.self_loop",
  );
  // A pred too costly to compute live says so; the rest still shows.
  await mock.configure({ readErrors: { "derived:invoiced": "budget" } });
  await page.reload();
  await page.getByRole("tab", { name: /^Derived/ }).click();
  await expect(page.getByRole("list", { name: "Not computed" })).toContainText(
    "invoiced: too deep to compute live.",
  );
  await expect(page.getByRole("list", { name: "Flags" })).not.toContainText(
    "invoiced",
  );
  await expect(page.getByRole("list", { name: "Flags" })).toContainText(
    "under contract",
  );
});

test("lists a load's rule breaks with counts, what isn't checked, and why", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200 });
  await signIn(page);
  await load(page);
  await page.getByRole("link", { name: "Rule breaks" }).click();
  await expect(
    page.getByRole("heading", { name: "Rule breaks", level: 1 }),
  ).toBeVisible();
  // Counts per constraint and severity: every invoice but Globex's two
  // and Initech's one bills a customer no contract names.
  const row = page
    .getByRole("grid", { name: "Constraints" })
    .getByRole("row")
    .filter({ hasText: "invoice_under_contract" });
  await expect(row).toContainText("Warns");
  await expect(row).toContainText("62");
  // What the checks leave out, said rather than counted: the engine
  // doesn't record it in the graph.
  await expect(
    page.getByText(/It records no rule linking an entity to itself/),
  ).toBeVisible();
  // A page of 50 breaks, then the rest.
  const breaks = page.getByRole("list", { name: "Rule breaks" });
  await expect(breaks.getByRole("listitem")).toHaveCount(50);
  await expect(page.getByText("1–50 of 62 breaks")).toBeVisible();
  await page.getByRole("button", { name: "Next" }).click();
  await expect(breaks.getByRole("listitem")).toHaveCount(12);
  await expect(page.getByText("51–62 of 62 breaks")).toBeVisible();
  await expect(page.getByRole("button", { name: "Next" })).toBeDisabled();
  // Why: the constraint, its invoice's stated customer and the flag that
  // doesn't hold.
  const first = breaks.getByRole("listitem").first();
  await first.getByRole("button", { name: "Why" }).click();
  const why = first.getByRole("list", { name: "Why it holds" });
  await expect(why).toContainText("by the constraint “invoice_under_contract”");
  await expect(why).toContainText("stated in the data");
  await expect(why).toContainText("nothing states it");
  await expect(why).toContainText("under contract");
  // Back a page, and on to the invoice's own page.
  await page.getByRole("button", { name: "Previous" }).click();
  await expect(page.getByText("1–50 of 62 breaks")).toBeVisible();
  await breaks.getByRole("listitem").first().getByRole("link").first().click();
  await expect(page).toHaveURL(/\/entities\//);
  await expect(
    page.getByText("Invoice", { exact: true }).first(),
  ).toBeVisible();
});

test("follows a stored rule break from Ask to its record and its entity's Needs review", async ({
  page,
}) => {
  await mock.configure({ loadStepMs: 200, askStepMs: 100 });
  await signIn(page);
  await load(page);
  // An answer cites the load's first stored break of invoice_under_contract.
  await page.getByRole("link", { name: "Analyst", exact: true }).click();
  const form = page.getByRole("form", { name: "Ask a question" });
  await form
    .getByRole("textbox", { name: /^Question/ })
    .fill("Which invoices break a rule?");
  await form.getByRole("button", { name: "Ask", exact: true }).click();
  const answer = page
    .getByRole("article", { name: "Question: Which invoices break a rule?" })
    .getByRole("region", { name: "Answer" });
  await answer
    .getByRole("link", {
      name: /^Source \d+: A break of invoice_under_contract$/,
    })
    .click({ timeout: 10_000 });
  // It opens the load's Rule breaks at that record, the constraint chosen
  // and the record marked.
  await expect(page).toHaveURL(
    /\/loads\/load_\w+\/rule-breaks\?constraint=invoice_under_contract&key=[0-9a-f]{64}$/,
  );
  const constraint = page
    .getByRole("grid", { name: "Constraints" })
    .getByRole("row")
    .filter({ hasText: "invoice_under_contract" });
  await expect(constraint).toContainText("Warns");
  await expect(
    constraint.getByRole("link", { name: "invoice_under_contract" }),
  ).toHaveAttribute("aria-current", "true");
  const cited = page
    .getByRole("list", { name: "Rule breaks" })
    .locator('li[aria-current="true"]');
  await expect(cited).toHaveCount(1);
  await expect(cited.getByText("Invoice", { exact: true })).toBeVisible();
  // The record names one invoice; its page lists the break, stored.
  const invoice = cited.getByRole("link");
  await expect(invoice).toHaveCount(1);
  const name = (await invoice.textContent())!;
  await invoice.click();
  await expect(page.getByRole("heading", { level: 1, name })).toBeVisible();
  await page.getByRole("tab", { name: /^Needs review/ }).click();
  const review = page
    .getByRole("list", { name: "Needs review" })
    .getByRole("listitem")
    .filter({ hasText: "Breaks a rule: invoice_under_contract" });
  await expect(review).toHaveCount(1);
  await expect(review).toContainText(
    "A rule the ontology checks doesn’t hold for it.",
  );
  await expect(review).not.toContainText("keeps no record");
  // Why: the constraint, broken by the record's own invoice, down to its
  // stated customer, whom no contract names.
  await review.getByRole("button", { name: "Why" }).click();
  const why = review.getByRole("list", { name: "Why it holds" });
  const root = why.locator(".ex-proof-step").first();
  await expect(root).toContainText(`breaks invoice under contract: ${name}`);
  await expect(root).toContainText(
    "by the constraint “invoice_under_contract”",
  );
  await expect(root.getByRole("link", { name })).toBeVisible();
  await expect(why).toContainText("billed to");
  await expect(why).toContainText("stated in the data");
  await expect(why).toContainText("no under contract:");
  await expect(why).toContainText("nothing states it");
});
